# Job Lifecycle Contract

This is the contract for a job: what it is made of, what state it is
guaranteed to have, who may write each piece, which operations may fail and
which must not, and what a restart is allowed to change.

The lifecycle is implemented across four packages, and the split is the
contract's first fact:

| Package | Owns |
|---|---|
| `internal/job` | the `Job`, its four axes, `Attempt`, `Lease`, `Manifest`, `JobProgress`. No I/O, no import of any other package here except `internal/constants`. |
| `internal/sched` | the scheduling decisions — the two pools, grant and release, `Advance`, `Cancel`, `Settle`, `Retry`, `Park`, and the rendering doors. No registry, no store, no workers. |
| `internal/dispatch` | the registry in queue order, manifest residency, the tick loop, worker launch and exit, and the queue-state rows. |
| `internal/checkpoint` | batched writes of the progress record. |

`docs/ARCHITECTURE.md` describes the architecture. This describes its
obligations. `internal/job/doc.go`, `internal/sched/doc.go` and
`internal/dispatch/doc.go` are the same contract stated at the code; where a
sentence here and one there disagree, the code's own doc is nearer the truth
and the gap is a bug in this file.

---

## 1. What a job is

A `Job` is an identity (`ID`, `Name`), a `Policy`, a list of `Attempt`s, an
`Intent`, and — when resident — a `Manifest` and a `JobProgress`.

`job.New(id, name, Policy)` is the **only** constructor
(`git grep -n 'func New(' internal/job/` returns one line, `job.go:245`).
A restore does not get a second one: `dispatch.reconstruct` rebuilds a
persisted job by calling `New` and then replaying it forward through the
exported doors along `replayPath` (`internal/dispatch/dispatch.go`). A second
constructor was escalated under the Decision Protocol and declined — Standing
Design Rule 2 names it as the first smell, and `newManifest` /
`Manifest.UnmarshalJSON` are the worked example that had already diverged over
`totalBytes` before anyone noticed.

### Policy, not a PP level

`Policy` is what a job is *permitted* to do, resolved once at ingestion from
SABnzbd's PP level plus the job's category:

```go
type Policy struct {
	Verify bool // the Assessor may reach a real verdict
	Repair bool // Repairing may be entered
	Unpack bool // archives may be extracted inside Extracting
	Delete bool // archives and sidecars may be removed after extraction
}
```

PP 0–3 is a cumulative integer mask inherited from upstream, and it is the same
*kind* of thing as upstream's status strings: external vocabulary, translated
at the boundary and never stored internally. **The integer does not exist past
`App`.** `PolicyFromPP` saturates at both ends without an explicit clamp,
because each field's `>=` comparison already does.

**Every state runs at every policy.** At `Verify: false` the Assessor returns
`Complete` without doing work and the job crosses the boundary immediately.
Gating *states* on the level instead would mean skipping `Assessing` at PP=0 —
which removes the only state that decides and leaves nothing to authorize the
crossing, forcing a second decider back into the design. The machine's shape is
therefore policy-independent, which is what keeps it exhaustively testable and
what makes per-category overrides stop being a special case.

The zero value is download-only, so a job built without an explicit policy does
the least destructive thing.

---

## 2. Four orthogonal axes

`State`, `Activity`, `Outcome` and `Intent` are four separate facts. Keeping
them apart is what collapses the transition table: "still producing, doing
something else now" is an `Activity` write rather than a state change, and
"stop at the next gate" is an `Intent` write rather than a state one.

> **No axis reads another.** Nothing branches on `Activity`; `Intent` is not
> consulted while rendering a running job's status; `Outcome` is the only axis
> that says an attempt is settled.

### `State` — position

```go
const (
	StateUnset State = iota // not a state; the zero value, never a legal destination
	Fetching
	Assessing
	Repairing
	Extracting
	Finalizing
)
```

`StateUnset` exists so a zero `StateView` cannot read as an active download —
Rule 2's "a type with a valid zero used as a key" smell. **`StateUnset` means
one thing only: the job has no attempt at all.** It is never what a settled
attempt looks like.

There is no `Queued` and no `Waiting`. A newly added job has no attempt;
"has this ever run" is `Job.HasRun()`, exact where any predicate over byte
counters would conflate *did not start* with *started and got nowhere*.
There is no `Finished` either — see `Outcome` below.

`AllStates()` returns the five real states. `TestAllStates_Exhaustive` parses
`state.go` by AST and asserts `declared == AllStates() ∪ {StateUnset}`,
exactly — the sentinel is named in the assertion rather than subtracted by a
count, so a second sentinel, or a real state omitted from `AllStates()`, still
fails.

### `Activity` — what is executing

Ten values in `activity.go`: `ActNone`, `ActCRCCheck`, `ActPar2Verify`,
`ActPar2Repair`, `ActVolumeRecovery`, `ActUnpack`, `ActDeobfuscate`,
`ActCleanup`, `ActMove`, `ActScript`. A field the running component writes,
never a transition. It exists for the UI, the API and the log.

There is deliberately no activity for fetching. `{Fetching, holds}` and
`{Fetching, does not hold}` are already distinguishable from what the job
holds, so a downloading activity would be a second encoding of the same fact.

### `Outcome` — the verdict, write-once

`OutcomePending` (the zero, meaning not settled), `OutcomeOK`, `OutcomeFailed`,
`OutcomeUnrecoverable`, `OutcomeCancelled`.

**Settledness is an `Outcome` fact, not a `State` one.** `finish` assigns the
verdict and does not touch `state`, so a settled attempt keeps the position it
settled at — `Finalizing` + `OutcomeOK`, which is strictly more information
than a `Finished` state would carry, and is what a history view wants: *where
did this attempt end?* A settled attempt can therefore be sitting in a
Correctness state, and anything that treats "settled" and "in a Correctness
state" as mutually exclusive is wrong.

`finish` is the sole writer, pinned by
`TestOutcomeWrites_MatchTheEnumerationStatedInProse`
(`internal/job/outcome_writer_enumeration_test.go`). Note what that test names:
the unexported `Attempt.finish`, not the exported door `Job.Finish` — the door
takes the lock and yields the lease, the method assigns the field, and the
enumeration asserts against the assignment.

**Which verdict a position admits is `admissibleAt`'s** (`admissibility.go`),
the sole owner of that question and `finish`'s only guard:

| Outcome | Admissible at |
|---|---|
| `OutcomeOK` | `Finalizing` alone |
| `OutcomeUnrecoverable` | the Correctness zone only — unrecoverable means the job never crossed |
| `OutcomeFailed`, `OutcomeCancelled` | every state |

It replaced two hand-written guards inline in `finish`, one of which was wrong
through two review rounds while a correct rule sat in a comment beside it.

### `Intent` — what a person asked for

```go
const (
	IntentRun    Intent = iota
	IntentPause
	IntentCancel // latches
)
```

`Intent` lives on the **`Job`**, not the attempt: a paused job that is retried
stays paused, because the pause was a statement about the job, not about one
run of it. `SetIntent` is its sole writer
(`TestIntentWrites_MatchTheEnumerationStatedInProse`).

`SetIntent` is legal in **every** state, including once the current attempt is
settled. An intent is not a wait reason: a settled job may be retried, and the
intent it carries governs what happens when it is. Refusing it there leaves a
real trap — pause a job, let it fail, and it can be neither unpaused nor
usefully retried.

`IntentCancel` latches; `SetIntent` refuses to leave it, so "cancel then
unpause" cannot revive a cancelled job. `IntentRun ↔ IntentPause` is freely
reversible.

> **Retry does not clear the latch, and that is deliberate.** A cancelled job
> whose attempt is settled still carries `IntentCancel`, so `Retry` would open
> an attempt that `Advance` cancels on its next tick. Cancel renders as
> `Deleted`, and a full redo is a re-added NZB starting a new `Job`, not a new
> attempt on a cancelled one. Clearing the latch on retry would let a job the
> user deleted come back through a path that never re-asked them.

---

## 3. Attempts

The machine lives on the current `Attempt`, not on the `Job`. A `Job` holds a
list of them, each with its own write-once `Outcome`, so a retry **appends a
verdict instead of revising one**. This is the ledger move: do not mutate the
balance, append an entry.

An attempt carries `State`, `Activity`, `Outcome`, `next`, start and end
stamps, and `Assessed` — a flag that latches the first time the attempt reaches
`Assessing` and stays latched for the rest of it. `Assessed` is what tells a
first-pass download from a re-entry fetching recovery volumes; both are
`Fetching`, and nothing else on the struct distinguishes them.

An attempt **opens** on the first `BeginAttempt` while none is open, at
`Fetching`, holding nothing. It **closes** when `finish` assigns its verdict.
Pause and resume — which surrender and later re-take a lease — do not end it.
`BeginAttempt` is the sole writer of `attempts`.

Three things follow:

- **Retry costs nothing structurally.** Job identity is stable, so the
  durability record, the manifest path and the partial file on disk are all
  still keyed correctly.
- **`Outcome` stays genuinely write-once.** A verdict is never revised, only
  superseded by the next attempt's.
- **"Never started" is exact.** `HasRun()` is `len(attempts) != 0`.

**Retry has exactly one meaning.** A retry resumes this job, re-fetching only
what previously failed. There is **no** full re-fetch — no `retry --all` flag,
no second code path, and no question at any call site about which kind of retry
is in progress. A user who wants every byte re-downloaded adds the NZB again,
which is genuinely a different operation and behaves like one: a new job ID, a
fresh working directory, no inherited durability record, and duplicate
detection against the original, which is correct.

That also makes durability retention on a failed job unconditionally right:
retry always wants those records, so there is no case in which keeping them is
wasted.

**The attempt list is unbounded.** Attempts are small and the growth case is
narrow — a job an automation tool retries on a schedule. The field carries a
comment recording the case and the two remedies (cap the list, or sweep with
history retention) rather than a policy nothing has needed yet.

---

## 4. The work spine

`legalEdges` is exactly six edges, pinned by `TestLegalEdgesIsTheWorkSpine`:

```
Fetching   → Assessing
Assessing  → Fetching, Repairing, Extracting
Repairing  → Assessing
Extracting → Finalizing
```

`CanTransition` has no `from == to` early return: no door requests a
self-transition, and permitting one would be a legal no-op that clears
`Activity` and nothing else.

Cancellation is **not** an edge. Settling is `finish`, which never consults
`legalEdges` at all.

### `next` marks completion and carries the verdict

> **`next` is set ⟺ the current state's work has ENDED and the job continues
> to another work state.** Its value is where it continues to. When work ends
> and the job does *not* continue, `Finish` settles the attempt instead and
> `next` stays unset.

*Ended*, not *succeeded*. A `Fetching` that exhausts every server has ended —
every article was attempted — and `Assessing` is entitled to decide what the
result means. **Failure is not a separate contract.**

This is the fact the removed `Waiting` state used to carry, and it is not
derivable. The *value* of `next` usually is: every state but `Assessing` has
one spine successor. The *presence* never is — "has this download finished?" is
a fact about the world, and no amount of graph inspection answers it. A model
that derived both jumped a partially-downloaded job straight into verification
on resume.

Who writes it: **the worker that completes a state's work**, as the last act of
that work.

| State completes | Writes |
|---|---|
| `Fetching` | `SetNext(Assessing)` |
| `Assessing` | `SetNext(verdict)` — `Fetching`, `Repairing` or `Extracting` |
| `Repairing` | `SetNext(Assessing)` |
| `Extracting` | `SetNext(Finalizing)` |
| `Finalizing` | `Finish(OutcomeOK)` |

**`Finalizing` is not an exception — it is an instance of the rule's second
clause.** Its work ends and the job continues nowhere, so it settles. Stating
the rule in two clauses is what stops it needing a carve-out in `Advance`,
`finishCancel` and `waitReason`. A separate `workComplete` flag was considered
and rejected: `next` cannot carry an `Outcome`, so `Advance` would then need a
special case for "`Finalizing` + complete → `Finish(OK)`" and the special case
would only move. Keeping `Finish` atomic means the window between "work done"
and "recorded done" does not exist; a crash before that single call means the
work was never recorded and restart re-runs `Finalizing`.

When a state's work **fails**, the rule applies unchanged:

| Failure in | Path |
|---|---|
| `Fetching`, `Repairing` | continues → `SetNext(Assessing)`; only `Assessing` is entitled to decide what a failure means |
| `Assessing` | does not continue → `Finish(OutcomeFailed)` or `Finish(OutcomeUnrecoverable)` |
| `Extracting`, `Finalizing` | does not continue → `Finish(OutcomeFailed)`; crossing is irreversible, so there is nowhere to go back to |

A failed `Fetching` therefore looks exactly like a successful one to the
machine. This is Standing Design Rule 3 ("a bad article costs only its own
bytes") applied one level up.

Three rules on `SetNext`:

1. **Guarded by `legalEdges`.** A value the current state could not reach
   directly cannot be recorded either.
2. **Write-once per visit.** It refuses when `next` is already set to a
   *different* value (`ErrNextAlreadySet`) and is a no-op for the same one.
   Without this, a verdict of `Repairing` could be overwritten with
   `Extracting` and the job would cross the boundary skipping repair.
3. **Cleared by the move.** `Transition`, `Cross` and `Finish` each clear
   `next` as part of taking it. So `next` has **four** writers —
   `TestNextWrites_MatchTheEnumerationStatedInProse` asserts exactly that four,
   and `TestAttempt_FinishClearsNext` pins the clearing itself. A settled
   attempt that kept a destination would report a move it will never take.

`Transition` keeps its `to == next` check. Its purpose is enforcing that once a
state's work has decided where to go, nothing else may choose.

**The `Assessing → Repairing → Assessing` loop is bounded by the Assessor, not
by this machine.** Nothing in `internal/job` counts repair attempts, and
deliberately so: how many repairs are worth attempting is a `Policy` question
that `internal/par2`'s verdict owns. Named here because an unbounded loop is
the obvious failure and it should be clear whose it is.

---

## 5. The reversibility boundary

This is the central invariant. Everything else follows from it.

|  | **Correctness** | **Production** |
|---|---|---|
| States | `Fetching`, `Assessing`, `Repairing` | `Extracting`, `Finalizing` |
| Goal | have the correct bytes | turn bytes into final files |
| Consumes | network, then CPU + our own working dir | CPU + disk **outside** the working dir |
| Side effects | none observable outside the job | deletes archives, moves files, runs user scripts |
| Idempotent? | yes — refetching costs bandwidth, nothing else | **no** |
| Can go back? | yes, freely | **no** |

> **A job crosses from Correctness to Production exactly once, and never
> returns.**

The asymmetry that makes this safe to model is that acquisition is idempotent
and production is not. Re-downloading an article you already have wastes
bandwidth; unpacking twice, or deleting archives you still need, is
destructive.

That one line defines four other things: pause granularity, cancel semantics,
the lease's lifetime, and which failures are recoverable — everything before
the boundary is restartable from the same files.

**The rule holds across attempts, not merely within one.** A fresh attempt
opens in `Fetching`, so without that scope the boundary would be re-crossable
by opening a new attempt after a crossed one settled. Crossing consumes the
inputs a later attempt would need, so once any attempt has crossed, no further
attempt on that job is a legal way to retry it. The path to a full redo is a
re-added NZB, which starts a new `Job`.

### Two tests, and why the second exists

`TestBoundaryIsOneWay` (`transition_test.go`) enumerates `AllStates()` and
fails if any single **edge** runs Production→Correctness. That is a property of
`legalEdges`.

The invariant above is a property of **reachability**, and two individually
legal edges can compose into the transit the edge map forbids directly — which
is exactly how both escapes found on this surface got through, with
`TestBoundaryIsOneWay` green throughout.
`TestBoundaryIsUnreachableByAnyPath` (`reachability_test.go`) is what pins the
invariant: it replays action sequences through the exported doors and asserts
the property at every reachable configuration.

**Cite the reachability test when you mean the invariant; cite
`TestBoundaryIsOneWay` only when you mean the edge map.**

Its oracle must share no code with the doors it judges. `Transition` refuses
`IsCorrectness(from) && IsProduction(to)` and `Advance` selects the crossing
with the same predicate, so judging those doors with it would let a wrong
predicate agree with itself. The oracle enumerates the Correctness states as a
literal set, guarded by an assertion that `AllStates()` has not grown a member
the literal does not classify.

There is no `crossed` field. It is derived — `IsProduction(a.state)` — sound
only because no edge runs from Production back to Correctness, which is exactly
what the reachability test now asserts rather than assumes. It is not a
persisted column either: storing it would be a second source of truth beside
the `State` it is computed from.

### `Cross` is the only door

There is exactly one Correctness→Production edge, `Assessing → Extracting`, so
`Cross` owns one edge rather than a state class.

```go
func (j *Job) Cross(to State) (*Lease, error)
```

It sets `state`, clears `next` and yields the lease **inside one locked
callback**. Doing it as two calls — transition, then surrender — reproduces a
defect that already shipped: forgetting the second leaks a pool-A slot
permanently and silently, and doing them in the other order leaves a job in
`Assessing` holding no lease that `Assessing` requires. Two coordinated
mutations in different objects, one forgettable, failing without an error, is a
check where an owner is owed.

`Transition` refuses that edge outright (`ErrCrossRequired`) precisely so
`Cross` is the only way to take it. `Cross` validates exactly what `Transition`
would have — the attempt must be at `Assessing`, and `to` must equal `next` —
without which it would be a hole in the single-decider property.

**`Cross` makes the leak one call site; it does not make it
unrepresentable.** The caller still receives a `*Lease` and can still drop it
— by panicking, by an early return, or by forgetting. What `Cross` removes is
the *coordination*. A callback form would close it and is rejected: it would
take the Queue's mutex while the Job's is held, inverting the lock order.

---

## 6. `Assessing` is the only decider

Every other state does work and returns. Within the Correctness zone,
`Assessing` is the only state with more than one work successor:

| Verdict | Next |
|---|---|
| `Complete` | `Extracting` — cross the boundary |
| `Repairable` | `Repairing`, then back to `Assessing` to re-verify |
| needs more recovery volumes | `Fetching` |
| `Unrecoverable` | settle `Finish(OutcomeUnrecoverable)` |

`TestOnlyAssessingBranchesWithinCorrectness` pins this for the Correctness zone
specifically. It does not examine Production, where the same property happens
to hold today but is unenforced.

**Verification method is an implementation detail.** The cheap CRC path and the
full par2 verify are two ways for one assessment to reach one verdict; there is
no `QuickCheck` state and no "bypass". `par2.Assess` is the one shared
computation both consumers read.

Two consumers remain, and that is deliberate rather than a duplication:
`app.par2Verdict` decides at download completion whether to fetch deferred
recovery volumes, and `internal/postproc`'s `quickcheck` stage decides whether
repair is needed. Once both read one `par2.Assess` result they are asking
genuinely different questions, and neither destroys state the other needs.
Unifying them may still be worth doing; it is not owed.

The `quickcheck` stage is **retained permanently**, for two responsibilities
nothing else has:

- **Subdirectory relocation.** It is the only caller of `par2.ApplyRenames`,
  and pairs it with its own rename bookkeeping so a relocated file does not
  leave its old path recorded as owned — where the ownership guards in the
  cleanup stages would then skip it as unowned. Without this pass ahead of
  repair, a job whose par2 set names nested paths fails verification and
  extraction.
- **The external-binary bypass.** A clean QuickCheck is what lets the repair
  stage skip spawning par2 entirely. Delete the stage and every clean download
  pays a full external par2 verify.

Neither is a verification decision. The download path deliberately does the
opposite — it performs no I/O and applies no renames — so there is no home for
these on that side even in principle.

**An `Unrecoverable` job never crosses the boundary.** Its files stay in the
working directory. The reason is not that partial output is worthless — for a
post of independent files it is genuinely useful. The reason is what crossing
*costs*: archives are deleted, files are moved, and the inputs a later attempt
would need are consumed. **Not crossing keeps the job retryable**, and a
missing article may well be available next month, or from a server the user
adds next week.

Delivering the intact files only when no archive set is implicated is the
sophisticated alternative, and it is rejected: it requires classifying files
into archive sets *before* extraction, which is a new inference with its own
failure modes, in service of a case that is uncommon on binary Usenet.

**Block-exact recovery-volume promotion is a gap, not a property.** The
`Assessing → Fetching` edge is built and legal, but nothing computes an exact
block shortfall to drive it; `UndeferRecoveryVolumes` accepts a
block-covering subset of file indices and no producer of one exists. Do not
price in "repair never fails for insufficiency" — insufficient blocks set
`ParError`, which suppresses unpack, and the count goes in the repair stage's
log line. See `docs/post-processing-contract.md` § *Open Gaps*.

---

## 7. The Lease

A `Lease` is the admission token for the correctness loop: pool-A capacity, and
nothing else.

```go
type LeaseID uint64
const LeaseUnset LeaseID = 0   // the invalid zero, in StateUnset's spirit
type Lease struct{ id LeaseID }
```

Identity cannot come from the pointer: Go gives distinct zero-size allocations
the same address, so a pointer-keyed pool held one entry for two jobs.
`LeaseID` is what a pool uses to tell two outstanding leases apart, to verify
that a lease handed back is one it issued, and to name one in a log line.
`TestLease_HasDistinctIdentity` pins the size directly.

| State | Requires |
|---|---|
| `Fetching` | lease |
| `Assessing`, `Repairing` | lease + compute slot |
| `Extracting`, `Finalizing` | compute slot (the lease went at the crossing) |
| `StateUnset` | nothing |
| any state, once settled | nothing |

That last row is normative and is the one most easily got wrong: a settled
attempt keeps the position it settled at, so a gate written as
`needsLease(state)` alone will request a lease for a settled job.
**Settledness must be checked first.** `needsLease`/`needsSlot`
(`internal/sched/requirements.go`) answer about a **position**, never about an
attempt, and say so.

**The lease does not carry the manifest or the barrier.** An earlier design
argued that pool-A capacity, the resident `Manifest` and the `StorageBarrier`
share one lifetime and are therefore one object. They do not, and `lease.go`
records the refutation at the type:

- The **barrier is process-level**. One is built in `app.New` and holds
  cross-job state; reconciling per-lease would destroy durable records for jobs
  in post-processing.
- The **manifest is keyed on holding what a position requires**, not on holding
  a lease. `grantFor` runs under the Queue's mutex and hydration does disk I/O,
  so there is no manifest to install at grant time; and post-processing reads
  the manifest at `Extracting`/`Finalizing`, where `needsLease` is false — so a
  lease-gated manifest would be unreadable exactly where the stage that
  verifies CRCs needs it.

Do not reintroduce either field.

### One releaser, one reclaimer

```go
func (j *Job) surrenderLocked() *Lease                          // SOLE releaser; caller holds j.mu
func (j *Job) Surrender() *Lease                                // takes j.mu, then surrenderLocked
func (j *Job) Cross(to State) (*Lease, error)                   // calls surrenderLocked
func (j *Job) Finish(o Outcome, now time.Time) (*Lease, error)  // calls surrenderLocked
func (q *Queue) reclaim(l *job.Lease) error                     // SOLE reclaimer; no-ops on nil
```

> **Every door that can end the job's need for a lease yields it, and the Queue
> reclaims through one call that no-ops on nil.**

`Cross` and `Finish` must call `surrenderLocked`, not the exported
`Surrender`. Both run their bodies inside `withOpenAttempt`'s callback, which
holds `j.mu` across the attempt mutation, and Go's mutexes are not reentrant —
either one calling `Surrender()` would take `j.mu` a second time and deadlock
the job permanently, with no error and no timeout. What matters is that `j.mu`
is held when the door's body runs, not which frame took it. `Surrender` stays
outside that set structurally: the helper refuses a job with no open attempt,
which is precisely the caller `Surrender` exists to serve.

Five attempt-mutating doors share one open-attempt check —
`Transition`, `SetNext`, `SetActivity`, `Cross`, `Finish` — through
`withOpenAttempt`, with `withOpenAttemptLease` as an adapter for the two that
must return a `*Lease`. Dropping the `isOpen` test from that single helper
fails all five subtests of `TestJob_FinishedJobHasNoOpenAttempt`.

Every exit that must yield:

| Exit | Yields via |
|---|---|
| Cross into Production | `Cross` |
| Settle after a pre-boundary failure | `Finish` |
| Settle `Unrecoverable` from `Assessing` | `Finish` |
| Cancel | `Finish` |
| Pause / park | `Surrender` |
| Cross a job that was paused before crossing | `Cross` → `nil` |

That last row is why `Surrender` returns `nil` rather than asserting, and why
`reclaim` no-ops on `nil`. A job may legitimately reach the crossing holding no
lease: it was paused at `Assessing{next: Extracting}`, surrendered, and
resumed. It does not need one to cross, because crossing is where it would have
given the lease up anyway. Each call site testing for nil itself is another
chance to forget; one function that accepts nil is none.

`reclaim` carries an identity audit that fails on a lease this pool did not
issue or has already taken back, which is why it returns an error rather than
nothing.

---

## 8. Scheduling

### Two pools

- **Pool A — acquisition leases.** Bounds how many jobs may be working toward
  correct bytes. Held across the entire correctness loop, *including* while
  assessing and repairing.
- **Pool B — compute slots.** Bounds concurrent CPU/disk work: `Assessing`,
  `Repairing`, `Extracting`, `Finalizing`. `Fetching` takes none — it is
  network-bound and its concurrency is pool A's business.

Pool B is **one pool, not split by resource class**. "Max concurrent
post-processing" is the knob users already understand, and splitting it doubles
the tuning surface for a benefit nobody has measured. The known cost is named
and not solved: a user script in `Finalizing` is arbitrary code of arbitrary
duration and holds a compute slot while consuming none of what the pool exists
to bound. If that blocks real work in practice, the fix is to run the script
outside the pool — a small change, better made against evidence.

Pool A is **reserved rather than released-and-reacquired**. A job re-entering
`Fetching` from `Assessing` never waits, so the correctness loop is provably
non-starving. The cost is real and accepted: acquisition capacity sits idle
while a leased job assesses or repairs.

Capacities are `SetCaps` / `LeaseCap` / `SlotCap` on `sched.Queue`.

### Acquisition and release each have exactly one owner

- **`grantFor` is the only acquisition.** One production call site for
  `leases.issue`, one for `slots.acquire`, both inside its body. It grants the
  lease before the slot and does **not** roll back a lease it took when the
  slot is unavailable: the job keeps the lease and waits. Rolling back would
  make a job that is one slot short give up capacity it will need again on the
  next tick.
- **`reclaim` is the only lease return** (§7).
- **`releaseFor(j, s)` is the only slot release.** It frees what `s` does not
  require, so `releaseFor(j, StateUnset)` frees everything without naming pool
  B at the call.

The asymmetry is the argument for the second owner: a review found **three**
separate slot leaks — the `Assessing → Fetching` demotion, park, and settlement
— where a per-site release would have had to be remembered at each and was not.
Acquisition had an owner from the start and leaked nowhere; release had none
and leaked everywhere.

Slots differ from leases in that nothing travels with them: the job does not
carry one, so there is no `Surrender` to mirror, and release is idempotent.

### Running-ness and grantability are different questions

| Question | About | Asked by |
|---|---|---|
| Is this job running? | the **current** state | rendering |
| May it take its next move? | the **next** state | scheduling |

```
holds(j)   ≡ j holds everything its CURRENT position requires
running(j) ≡ the attempt is open && next is unset && holds(j)
```

All three conjuncts are load-bearing. A job holding a slot whose work is
finished is not running, it is waiting to move — that is the `next` clause. And
the open-attempt clause is not redundant: a settled attempt keeps its position,
so `holds` may be genuinely true for it, and only that clause excludes it.

`holds` says **false** for `StateUnset` explicitly rather than deriving it.
`needsLease` and `needsSlot` are both false there, so the two resource guards
are vacuous and the function would otherwise fall through to `true` — reporting
that a job owning nothing held everything its position required. Two of three
callers were immune; the third is the rendered `Holds` field, and the
dispatcher hydrates on `Holds && !resident`, so a paused never-started job had
its manifest read into memory while holding nothing.

Every scheduling predicate takes a `job.Snapshot` — `State`, `Intent`,
`HoldsLease` and `HasRun` read under one `RLock` — rather than a `*Job`. That
is what makes the render path's purity **structural** rather than asserted: a
predicate handed a value has no `*Job` to acquire anything from. Composite
questions read one instant instead of a composite of several a door could land
between.

### Gates and wait reasons

```go
func (q *Queue) gatedBy(s job.Snapshot) (job.WaitReason, bool)   // PURE
func (q *Queue) waitReason(id string, s job.Snapshot) (job.WaitReason, bool) // PURE
```

`gatedBy` reports an intent or queue-wide gate and **consults no resources** —
they are a grant question, not a gate question. Precedence is user pause >
global pause > lease > compute slot, with cancel handled before the gate.
`IntentCancel` is absent from `gatedBy` because `Advance` routes it first.

`waitReason` returns early — *waiting for nothing* — for a settled attempt and
for a running job, then reports the gate, then `NoLease` for `StateUnset`
(waiting to start). Only after that does it consult resources, and **which
state's requirements it tests depends on whether work has ended**: a job whose
work has ended waits on what its *next* state needs. Testing the current state
unconditionally reports `NoComputeSlot` for `Assessing{next: Fetching}`, which
holds its lease and needs only that lease to continue.

`needsLease(want)` is load-bearing rather than decorative: `Extracting` and
`Finalizing` legitimately hold no lease, so an unconditional `!HoldsLease`
reports `NoLease` for a post-boundary job that is in fact waiting for a slot.

### `Advance`

`Advance` is the scheduling loop's entry point for one job. It **takes no
target** — the target is `next`, written by the worker that finished the state.

```
cancel first        → finishCancel
1. StateUnset       → gated? nothing : BeginAttempt
   settled          → releaseFor(StateUnset); stop
2. next unset       → holds? stop : gated? park : grantFor(current)
3. next set         → gated? park
                      crossing? Cross, reclaim, grantFor(next) [result ignored]
                      else moveTo(current, next)
```

A blocked path never records a verdict — `State`, `next` and `Outcome` are
untouched — so a lost acquisition race costs a tick, never a decision. It is
*not* true that a blocked path writes no job state at all: branch 3's
non-crossing tail can block after `grantFor` has already written the lease, and
that write is deliberate.

**Branch 1's settled arm must release.** A settled attempt keeps the position
it settled at but needs none of that position's resources; without the release
a job that settles at `Assessing`, `Repairing`, `Extracting` or `Finalizing`
holds pool-B capacity forever, because every other release is on a path a
settled job never takes again.

**Branch 2 checks `holds` before the gate, and that order is deliberate.**
`holds` means a worker owns the job's resources and is using them; stripping a
live worker is the worse failure. Gating never interrupts work, so a gated job
that still holds keeps holding until its worker yields.

**A settled attempt is never reopened here.** Retry is an explicit user action.
`Queue.Retry` refuses with `ErrNotSettled` rather than releasing anything when
the attempt is open or the job never ran, and releases the settled attempt's
compute slot *before* opening the new one — the new attempt starts at
`Fetching`, which needs no slot, so nothing on that path would ever release it.
`Retry` takes no lease: `BeginAttempt` needs none, so a retry cannot fail for
want of capacity.

**Crossing before acquiring the slot is deliberate**, and branch 3 ignores
`grantFor`'s result there: the decision was already recorded in `next`,
crossing only *adds* pool-A capacity, and a job that crosses and then fails to
get a slot is simply not running until it does. Branch 2 grants it on a later
tick. It cannot go back, and does not need to.

`moveTo` is branch 3's non-crossing tail. On a refused `Transition` the job is
still at `from`, so its release is keyed to `from`, not `to` — otherwise a slot
`grantFor` just acquired for a destination the job never reached stays held
forever. On a successful one, `releaseFor(to)` frees what the new position does
not need. `Assessing → Fetching` is the only demotion in the work spine and the
live case: without it the job downloads for hours while occupying pool B.

### `Park` is a separate door, and `Advance` cannot do its job

> **A worker that stops on a gate without ending its work reports `yielded`,
> and the dispatcher calls `Park`.**

`Advance`'s branch 2 returns early for a job that still holds, so a gated job
holding a lease mid-state is never parked by any number of ticks. Only this
door releases it, and the alternative is a paused `Fetching` job holding a
pool-A lease forever — a deadlock, not an inefficiency. With pool A at 3 and
three paused jobs, no job could ever fetch again.

`Park` is **unconditional** and takes no view on why the worker stopped. A gate
is the common reason but not the only one: teardown, shutdown and a dead
connection all end a worker without ending the work.

Its precondition is the caller's to guarantee and cannot be checked here: the
worker has returned and will not touch the job's lease, slot, manifest or
barrier again. `running()` stays **true** for a worker that has yielded and not
yet been parked — which is precisely why this door exists.

```go
func (q *Queue) parkLocked(j *job.Job) error {
	q.releaseFor(j, job.StateUnset)
	return q.reclaim(j.Surrender())
}
```

Both halves are needed. Releasing only the lease leaves a parked job silently
holding whatever compute slot its position held.

### Ordering

The registry keeps one job list, and both consumers — which jobs are advanced,
and in what order the downloader serves them — read it. Priority therefore has
exactly one meaning in the system, and there is no second fairness policy that
would have to re-derive it.

**That order is insertion sequence, not priority.** `entry.seq` is an insertion
sequence persisted as `Persisted.SortKey`; `restore` rebuilds order from
`SortKey` alone, with an `(SortKey, ID)` tiebreak matching the store's
`ORDER BY sort_key ASC, id ASC` so the sort is deterministic when two keys
collide. `TestSortKey_ReproducesQueueOrderAcrossRemoval` pins that removal does
not reorder survivors.

`Header.Priority` is written at ingest, persisted, mutated by
`Dispatcher.SetPriority` and rendered by the API, and **orders nothing** —
`internal/sched` never reads it. The one behavioural use of priority anywhere
is `constants.PausedPriority` at ingest, which maps to `SetIntent(IntentPause)`.
Closing that gap is open work (§14).

Reorder, when it is built, is defined over fetch ordering and is **total**: the
change is always recorded and takes effect whenever the job next competes for
fetch capacity — immediately for a running `Fetching` job, at the next lease
issuance for one holding nothing, on re-entering `Fetching` for a job holding a
lease at `Assessing` or `Repairing`, and never for a job past the boundary.
"Never" is not a failure: a job past the boundary has no remaining fetch
ordering to take a position in, and recording a position that will not be
consulted costs one integer. Rejecting the call or silently no-opping are both
worse, because they make reorder *partial* — its success would depend on state
the caller had to inspect first, which is a race by construction.

A reorder must **renumber**, and that is why it is not a free extension:
`restore` reads order from `SortKey` alone, so a move that did not rewrite keys
would survive in memory and vanish at the next restart. It needs an atomic
whole-queue resequence in the store.

---

## 9. Pause and cancel

### Pause is a gate, not an interrupt

Pause closes the gate at state transitions. Work in flight runs to the end of
its state and the job then stops. Granularity differs only in how often the
gate is checked: **per-article in `Fetching`, per-state everywhere else**.

This removes a whole failure class: **partially-applied work**. If pause could
interrupt an unpack, resume would have to answer "what state is the extraction
directory in?", which is unanswerable in general because external tools do not
checkpoint. Gating at boundaries means every state is entered and left
atomically, so resume is always "start the next state", never "resume the
middle of one".

The cost is honest: pausing mid-repair on a large set does nothing for minutes.
The fix is to **show** that — a running job with `IntentPause` renders as its
current state, and the UI reads `Intent` alongside the status to say "finishing
repair, then pausing" — not to make repair interruptible.

`Fetching` is the one state that gates per-article, so its worker stops without
its work having ended and `next` stays unset. That yield is not a completion
and must not be reported as one; it goes through `Park`.

**Resume needs no notification.** `SetIntent(IntentRun)` writes a flag; the
tick loop picks it up on its ordinary cadence. A `Job` cannot call a Queue and
does not need to.

### Cancel is an interrupt before the boundary, a gate after

| State | Cancel |
|---|---|
| `StateUnset` | the job is removed from the queue; there is no attempt to settle |
| `Fetching`, `Assessing`, `Repairing` | abort the worker; settle on the tick after it yields |
| `Extracting`, `Finalizing` | finish the current state, then settle |

Before the boundary everything is restartable from the same files and nothing
external was touched, so cancel means *stop now*. After it there is no clean
stop for half-moved files or a half-run user script.

**The gate is `IsProduction && running`, not `IsProduction && work-not-done`.**
`Finalizing` never sets `next`, so the latter is permanently true there and
fails three ways: a `Finalizing` job could never be cancelled at all; a
post-boundary job restored from a restart gated forever, because `Advance`
handles cancel before granting so the work that would have set `next` never
ran; and a `Finalizing` job waiting for a slot gated forever though no work was
in flight to protect. `running` asks the question the design actually poses —
*is work in flight?*

**Neither arm settles while a worker is live.** "Immediately" describes when
the worker is *told to stop*, not when its resources are taken; seizing a lease
from under a downloader mid-article is a use-after-free in all but name.

**A cancel releases the compute slot too.** `Assessing` and `Repairing` hold
one alongside the lease, and the settled arm of `finishCancel` must release as
well — `Advance` routes `IntentCancel` to `finishCancel` before it ever reaches
its own settled-branch release, so no later tick can recover what that arm
fails to free.

**A never-run job cannot be settled**, because `Outcome` lives on the `Attempt`
and there is none — `Finish` returns `ErrNoOpenAttempt`. Cancelling a queued
job **removes it from the queue**, which is what upstream does and what a user
means. `internal/sched` cannot do that itself (it holds no registry and no
store), so `finishCancel` returns nil for that case and
`Dispatcher.evictCancelledNeverRun` removes `StateUnset && IntentCancel` from
the registry and the store immediately after `Advance`, on every tick.

That bounds `gatedBy`'s stated reason for ignoring `IntentCancel` — true for
every job that has run, false for one that has not, and false for one cancelled
while running and then parked rather than settled, which sits open at
`Fetching` with no lease and `IntentCancel` and renders as `Queued` until the
next `Advance` routes it through `finishCancel`. Transient and self-healing,
but real for the tick it lasts.

**Cancelling a running `Finalizing` job lets it complete as `OutcomeOK`.** The
cancel arrived after the last gate; the files have moved and the script has
run, so recording `Cancelled` would be false. `Intent` survives on the settled
job for the UI to say the request came too late.

---

## 10. Rendering, and the translation to SABnzbd

Internal states are internal. `RenderView` is the seam: an attempt's own
`StateView` plus four facts a consumer reads together — `Running`, `Reason`,
`Holds` and `Intent`. The first three are the Queue's, because they depend on
pool-B slots and a queue-wide pause flag; `Intent` is `internal/job`'s own,
copied through because a consumer reads it alongside the others.

`Holds` is carried rather than reconstructed: `Running` is `holds && open &&
next unset`, so a false `Running` says nothing about which conjunct failed.

`Reason`'s zero value, `NoLease`, is **ambiguous by construction**.
`waitReason` returns "not waiting" for a settled attempt, a running job, and a
job whose work has ended while already holding what the next state requires,
and the renderer discards that boolean. **Consult `Running` first; do not read
`Reason == NoLease` as "waiting for a lease" on its own.**

`ToSABnzbd(RenderView) constants.Status` is the one place `constants.Status`
may appear, and it is **write-only** — the translation never reads a status
back into the machine.

| View | Status |
|---|---|
| settled (any position) | `Completed` / `Failed` / `Deleted`, per the outcome |
| not running, reason `IsPause()` | `Paused` |
| not running, otherwise (incl. `StateUnset`) | `Queued` |
| running `Fetching`, `Assessed` | `Fetching` |
| running `Fetching`, not `Assessed` | `Downloading` |
| running `Assessing`, `ActCRCCheck` | `QuickCheck` |
| running `Assessing`, otherwise | `Verifying` |
| running `Repairing` / `Extracting` | `Repairing` / `Extracting` |
| running `Finalizing`, `ActScript` | `Running` |
| running `Finalizing`, otherwise | `Moving` |

**The `Paused` row keys on the wait reason, not on `Intent`.** Under a
queue-wide pause every job still carries `IntentRun`, so keying on
`IntentPause` alone renders the whole queue `Queued` — a live API regression,
pinned by `TestToSABnzbd_GlobalPauseRendersAsPaused`. `WaitReason.IsPause()`
covers `UserPaused` and `GlobalPause` both.

**Intent is deliberately not consulted for a running job.** A job with a pause
requested is still repairing.

`TestToSABnzbd_IsTotal` walks the product space of every axis to prove no
combination yields an empty string — an unhandled combination shows up as a
blank status in somebody's Sonarr rather than as a crash here. The product
space must include the `WaitReason` axis, not just `Intent`, or it passes while
`GlobalPause` renders `Queued`.

Four upstream statuses — `Idle`, `Grabbing`, `Propagating`, `Checking` — are
never produced, pinned by `TestToSABnzbd_NeverEmitsUnproducedStatuses`. (The
weaker `TestToSABnzbd_EmitsOnlyDeclaredStatuses` cannot make that claim: it
checks membership in `constants.AllStatuses()`, which declares all four.)
`Fetching` finally means what upstream documents it to mean — downloading extra
par2 files for repair — which is exactly the `Assessing → Fetching` re-entry,
told apart by the attempt's latched `Assessed`.

`Row.Status()` on `dispatch.Row` is a second door onto that one place, for
sites that **render** a status — the API, history, log lines. It is **not** for
sites that branch on one; those read `State`/`Outcome`/`Intent`. Applying the
accessor to them blanket-wise would preserve the legacy vocabulary permanently
at every one.

The receiver is `Row` rather than `Job` because `ToSABnzbd` needs facts
`internal/job` cannot produce. The dependency runs one way — `internal/sched`
imports `internal/job` and not the reverse — so a `Status()` on `Job` would
need a back-pointer that inverts it into a cycle. `Row` already carries the
inputs.

---

## 11. Residency: the three tiers

Every mutating `JobProgress` operation takes a `*Manifest`. Not one read does.
The tiering is that boundary made explicit.

| Tier | Needs | May fail? |
|---|---|---|
| **Header** — remove, reorder, priority, ID/name lookups | neither | **Never** |
| **Progress** — all reporting, counters, completion and abort checks | `JobProgress` | **Never**, once it exists |
| **Manifest** — dispatch, article indexing, byte accounting | `Manifest` (evictable) | Yes, and must say so |

Writes need article byte counts and the file↔article mapping. Reads do not.

**Residency is not derived from position.** Either you hold a manifest or you
do not, and `Job.Manifest() (*Manifest, error)` makes every dependence on one a
compile error until it is resolved — the compiler enumerates the work where a
hand audit does not. A hand search over this surface returned a different
subset three separate times before it was compiler-enforced.

**`Job.Progress()` stays infallible.** Progress is not evictable, so a fallible
accessor would force error handling at every reporting site for a condition
that cannot occur, and would bury the manifest sites where absence is real.
Only the evictable tier is fallible.

The rule that falls out: **gate on residency if and only if the method needs
the manifest.** Adding a residency check to a progress-tier method is the same
defect wearing caution's clothes — it refuses work the method is always able to
do. This was not hypothetical: `SetPar2ReleaseReason` once demanded a manifest
it never reads, so the reason a job's par2 volumes were released was silently
discarded for precisely the non-resident jobs the on-demand par2 path acts on.

The gate on `*Job` is `j.manifest == nil` returning `job.ErrNotResident`,
pinned by `TestManifestAccessIsGated`
(`internal/job/manifest_gate_test.go`). `ErrNotResident` is deliberately
distinct from a hydration failure: "evicted" is routine and "unreadable on
disk" is data loss.

`errors.Is(err, job.ErrNotResident)` is something production code branches on:
a job removed or evicted mid-flight is ordinary and logs at Debug, while
anything else keeps its Warn.

### Always resident, and the one window where progress is not

**Always resident once it exists:** header fields, `JobProgress`, and the five
manifest-derived scalars (`TotalBytes`, `NumFiles`, `NumArticles`,
`RecoveryBytes`, `RecoveryFiles`). These are computed once at ingest and never
change, so they live in the always-resident tier rather than behind a fallible
handle. `Evict` clears only the manifest, and `AttachContent`/`RestoreContent`
are the only writers of the progress pointer.

The consequence is the point of the whole design: **every reporting path is
infallible.** Only mutation paths take the fallible handle.

**But `JobProgress` does not exist from ingest for a job restored at
startup.** `dispatch.restore` rebuilds each job with `job.New` and no content:
manifest loading is deferred to `Residency.Hydrate` by architectural discipline
(enforced by `TestDispatchNamesNoManifestType`), so the restore loop does not
access manifests and does not have the file and article counts sizing needs.
The record arrives later, at first hydration.
`grep -n 'j\.progress == nil\|j\.progress != nil' internal/job/*.go | grep -v
_test.go` finds 45 lines, so "no caller checks for their absence" describes an
intent rather than the code.

That window is why `Job` carries a small set of `restored*` fields for
progress-tier state recovered from the queue-state row before the record exists
— the download stamps, the par2 release reason, and the par2-recovered flag.
The Job-level accessors read them while `progress` is nil; `AttachContent`
seeds the fresh record from them **through the ordinary restore doors** and
zeroes them, so the two are never both authoritative and the stamp filter is
not reimplemented. Without that seeding those writes were simply dropped, and
the next persist wrote the zeroes back over the stored row.

`AttachContent` refuses a second attach rather than silently replacing a live
record: it builds a *fresh* `JobProgress`, so a re-attach would discard every
done/failed bit and both stamps with no error — and since the first attach
zeroed the Job-level copies, there would be nothing left to seed the
replacement from either. `RestoreContent` is the door for a job that has run
before, and it verifies `describesSameJobAs` rather than trusting the caller.

### Residency is bounded by what a job holds

> **manifest resident ⟺ the job holds everything its current position
> requires.**

`Dispatcher.reconcileResidency` is the sole enforcement point, driven by one
`Render` call and its `Holds` field — not `HoldsLease()`, which under-reports a
job at `Extracting` that holds a compute slot and no lease.

Memory is therefore bounded by the two pool capacities, not by queue depth:
only holding jobs carry manifests, so a global pause evicts at most pool-A-many.

**The invariant is stated at tick boundaries, not instantaneously.** `grantFor`
runs under the Queue's mutex and hydration does disk I/O that must not run
under any lock, so a job holds without a manifest for the length of one read.
Nothing consumes that window: worker launch runs after reconciliation, and a
read that failed on the manifest itself settles the job here.

The two hydration failure modes are not the same fact and must not be treated
alike:

- **An unreadable manifest is a fact about the job.** It can never run, and it
  is holding resources it can never use, so it is settled `Failed` — which
  returns both pools. Leaving it would strand them, because no later tick
  reaches a different branch for it.
- **A cancelled context is a fact about the process.** `Outcome` is write-once,
  so settling there would mark a healthy job `Failed` permanently and the user
  would find it failed after a restart when it was merely interrupted. The
  context error is checked **first** and matters most: the I/O a real hydration
  does mostly does not wrap a context error at all — `os.Open` returns
  `*os.PathError`, gzip and json return `io.ErrUnexpectedEOF` — so on a
  sentinel test alone every one of them settled the job `Failed` during a
  shutdown. The sentinel tests are kept beside it, because a hydration given
  its own deadline can report `DeadlineExceeded` while the outer context is
  still live.

Two further properties follow from the manifest being immutable after parse:

- **A handle cannot be invalidated by eviction.** A handle holding a pointer
  the registry has since replaced still describes the job correctly. No lock,
  no re-check, no stale read.
- **Snapshots do not hydrate from disk for reporting**, so there is no window
  between cloning a job and reading a file that may have been unlinked
  meanwhile. A concurrently removed job yields a stale-but-correct read instead
  of an error, and a queue listing can no longer fail because a job completed
  during it.

`Manifest` is shared by reference into snapshots; `JobProgress` is deep-copied.
That is what a snapshot *is*, rather than a rule callers must remember.

---

## 12. Persistence and restart

### Nothing persists a lease

> **Every persisted job restores to the position it was persisted at, holding
> nothing.**

`State`, `next`, `Outcome`, `Assessed` and `Intent` persist — all are decisions
or history, none is a resource. `crossed` is **not** a column: it is derived
from `State`, and storing it would be a second source of truth that could
disagree after a restore.

`Assessed` is easy to omit and its loss is silent. Drop it across a restart and
a job resuming a par2 fetch reports itself as starting its download over —
wrong to every API client, and wrong in a way no error surfaces.

Persistence stores the **raw attempt fields**, not a derived view. A settled
attempt restores with the position it settled at plus its `Outcome`, and
`Advance` declines it at the settled check exactly as it would have before the
restart.

**Restoring `next` is what makes this correct rather than destructive.** A job
persisted mid-`Extracting` restores with `next` unset, so branch 2 acquires a
slot and extraction **runs**. A job persisted after extraction finished
restores with `next = Finalizing`, so branch 3 moves it on **without
re-extracting**. The completion marker survives the crash and says which
happened.

**Restart is not a special code path.** It is the ordinary scheduler starting
from a cold pool. That is forced rather than remembered: the thing you would
need in order to be in any other state cannot be deserialized.

**Pause takes the same path.** A paused job holds nothing, exactly like a
restarted one; resume re-acquires and the manifest is re-read from
`admin/queue/manifests/<id>.json.gz`. Pause/resume and crash/restart are one
code path, and that is a property of the design rather than a coincidence: both
are "this job holds nothing and its work is unfinished".

### Who writes what

- **`internal/dispatch/store`** is the only reader or writer of the
  `dispatch_jobs` table: identity, header, queue order, `Policy`, and the four
  axes. It lives beside `internal/dispatch` rather than inside it so that
  package stays free of a SQL driver.
- **`internal/checkpoint`** batches the progress record — per-file completion,
  fetch policy, filename, CRC, failed and downloaded bytes, and failed-article
  rows. `Mark` records that a job moved and never writes; the ticker and
  `Flush` are the only things that write. It takes a `job.Checkpoint`, a
  **value** taken under the Job's own lock, which is what lets it batch without
  holding anything and keeps `Job` doing no I/O.
- **`Policy` is persisted resolved**, not as the PP integer it came from —
  persisting the integer would carry external vocabulary back inside the
  internal layer.

Restore is **all-or-nothing**. Registering as it goes would leave every row
before a failing one in the registry, and since `Start` clears its started flag
on error, a legitimate retry would re-load the same rows and be refused with
"already registered" — so the dispatcher could never start again.

### Article resolution is derived, not stored

`job_files` carries no per-article blob. Hydration reads the job's
`durable_runs` rows and its `failed_articles` rows and produces the done/failed
pair from them: `done` means "covered by a run", `failed` means "has a
`failed_articles` row", and `failed` implies `done`. Storing a third copy
beside the two records that already held the answer is exactly the second
authority Rule 2 forbids, and the column that held it has been dropped.

Both records index articles **globally**, so replaying them needs the file
boundaries. Those come from `Manifest.fileArticleOffsets`, the prefix sum
`newManifest` builds over the manifest's own file list
(`internal/job/manifest.go:104`). No per-file width is stored: `job_files`
carries download results only, and `appResidency.restoreResolution` runs inside
`Hydrate` after `readManifest` has attached the manifest, so the boundaries are
always already in hand at the one moment they are needed.

This is why there is no cheaper non-resident replay. A job that has not been
hydrated has no `JobProgress` at all, so it reports its header's full byte count
as remaining (`internal/dispatch/registry.go:473` in `List`, and again at 504 in
`Row` — the fallback is implemented independently in both) — a half-downloaded job shows
as untouched after a restart until it is promoted. Closing that would mean
constructing progress without a manifest, which nothing currently does; it is a
missing constructor, not a missing column.

`emitted` is deliberately **not** restored: it is transient per-process state
about what a downloader has in flight, and nothing that survived a restart is.

### The startup sweep

`Application.resumeAllJobs` runs once, synchronously, inside `Start` — after
the queue is loaded and **before** the downloader dispatches. It stats each
downloading job's files, has `durability.Resumer` **delete** the runs of any
file shorter than they claim, and installs what survives through
`Job.ReplaceFromRuns` — which clears a bit no surviving run covers as well as
setting the ones that are covered. The restored state is what the runs said
before the stat; the sweep's finding supersedes it.

The ordering is load-bearing twice over, and only the first half is about
re-fetching. A seed that lands after dispatch has begun still marks the right
articles done, but the request is already on the wire. And the gate itself
depends on it: `Resumer` compares a file's size against what its runs claim, so
if the assembler had already re-created and pre-allocated a deleted partial,
that comparison would run against a file of zeros and pass. Nothing inside
`Resumer` can notice, so moving the sweep later breaks the guarantee silently.

**It is bounded to jobs at `Fetching`**, and that word is the guard rather than
a description of it. In any other position something other than the assembler
owns the job's files: par2 repairs in place, unpack reads, and a move relocates
the file out of the download directory. Those bytes are correct and they are
not the bytes the runs recorded, so re-deriving over them would clear real
progress. Worse, a moved file's path no longer exists, `Resume` reports
`Restart`, and `Resumer` **deletes the file's runs** — erasing the record that
those bytes were ever made durable, and the erasure survives the restart the
seed exists to survive.

Running only at startup is nonetheless complete: a job admitted later has no
runs to seed from, and a job's runs cannot *gain* content while it is not
running, because only a barrier puts content into a row and a barrier runs only
for a job with open files. Other paths delete rows, and a delete cannot make a
row claim more than it already did.

The sweep writes **nothing** to the durability record. Its one mutation is
discarding the runs of a file that is missing or shorter than claimed. A
non-resident job in the swept position is hydrated for the duration and evicted
again, so this costs no residency — and it matters, because a paused job is the
case that needs the sweep most and is never resident. See
`docs/durability-contract.md` § *Restart* for the sweep's bounds.

---

## 13. Size and byte accounting

### Two immutable figures, not one filtered total

A job advertises `total_bytes` and `recovery_bytes`, with `recovery_files`
alongside. Both are computed once from the manifest and never recomputed;
neither changes for the life of the job.

The split key is **recovery volume, not par2 file**. The par2 index is always
downloaded and therefore counts as content. Conflating the two overstates
recovery capacity.

A *filtered* total would have to answer "does this include volumes we will not
fetch", and the answer depends on when it is read. Worse, the fetch policy
lives on `FileProgress` while the byte counts live on the `Manifest`, so a
filtered total is a property of the pair — and the manifest is evictable while
progress is not, which is how earlier framings arrived at a figure that meant
different things depending on residency. Two immutable figures have no such
coupling.

`content_bytes` is deliberately **not** stored: it has no consumer and it is
exactly `TotalBytes - RecoveryBytes` from two scalars every job already carries.

### Remaining and expected are derived, not maintained

```
remaining = Σ over files where !Complete && Fetch == FetchAlways
            of max(0, Bytes - BytesDownloaded - FailedBytes)

expected  = Σ over files where Fetch == FetchAlways of Bytes
```

Both derive on the same walk and the same predicate, so they cannot drift, and
they hold at any residency because they read `FileProgress` alone.
`FileProgress.Bytes` exists precisely to make that derivation independent of the
evictable manifest.

`FailedBytes` is subtracted per file because a failed article was never counted
as downloaded but also stopped being "remaining" in any useful sense. Each
file's contribution is clamped at zero.

**Remaining and the size it is paired with must share an exclusion set.** Every
consumer that turns remaining into a percentage or a downloaded total pairs it
with a size, and `downloaded = expected - failed - remaining` closes only if
both exclude the same files. `Job.TotalBytes()` stays the immutable
whole-manifest total for logging and post-processing display; the consumers
that combine a size with remaining or with failed bytes read `ExpectedBytes()`.
Getting this wrong is visible rather than silent — a freshly added
on-demand-par2 job reported non-zero progress, and history over-reported
downloaded bytes for a job finalized before its volumes were discarded.

Deferring, un-deferring or discarding a volume needs **no fixup**; the next read
reflects it. The cost is an O(files) walk on reporting reads in place of an O(1)
field read. Files number in the hundreds; articles, untouched by this, number in
the tens of thousands.

A job's advertised expectation therefore **moves** as par2 decisions are made:
it excludes recovery until damage forces an un-defer, and includes it after.
That was decided deliberately.

### Units, and the two per-file caches

`FileProgress.Bytes` and `FileProgress.BytesDownloaded` are both in the
**encoded** NZB `bytes` unit, because remaining subtracts them and only figures
in one unit can be subtracted. That is **not** the unit durability works in: a
durable run's length is the decoded payload an fsync proved, summed over the
same articles, and runs a few percent lower. The two are not interchangeable.
Routing the downloaded figure back through the durability record made every
non-resident job overstate its remaining bytes by the encoding overhead.

Both figures live **only in memory**. `job_files` used to persist them, as
`bytes_downloaded` and `failed_bytes`, defended as caches with a single writer
rather than second authorities. Nothing ever read either column back, and the
argument that `failed_bytes` was "the one per-file byte figure the durability
record cannot supply" does not hold: `failed_articles` gives the set of failed
article indices and the manifest gives each one's size via `m.ArticleBytes(i)`,
which is exactly the sum `JobProgress.markFailed` performs. Both columns are
gone.

They joined `write_cursor` and `max_written`, removed earlier for being
maintained in parallel with facts held elsewhere. The difference is that those
two had a live reader and these did not.

That reasoning is recorded in commit `eb540a64`'s message, not in the schema:
the same change that dropped the columns rewrote `001_initial.sql`'s `job_files`
comment block to describe the surviving schema rather than its history, so the
migration no longer mentions `bytes_downloaded` or `failed_bytes` at all.

### Memory

Per article, against the field types:

| | per article | 20k-article job |
|---|---|---|
| `Manifest` — `articleIDs []string`, `articleBytes`, `articleNumber` | ~86 B | ~1.6 MB |
| `JobProgress` — `done`/`failed`/`emitted` as bitsets | 0.375 B | ~7.5 KB |

The manifest figure is dominated by `articleIDs`: a 16-byte string header plus
a realistic ~52-character Message-ID, with two `[]int` at 8 bytes each beside
it. The progress figure is three bits.

`done`/`failed`/`emitted` are **bitsets, not `[]bool`** — three `[]bool` spend
three bytes to hold three bits, and at 20k articles that is 60 KB against 7.5.

Progress is therefore roughly 0.4% of what eviction reclaims. Keeping it
resident for hundreds of jobs costs a few hundred KB; keeping manifests
resident for the same queue costs hundreds of MB. **That asymmetry is the whole
justification for evicting one and not the other.**

### Terminal compaction: measured and declined

A job that has settled is not dispatching, so its per-article arrays look like
dead weight, and dropping them on settlement and rehydrating on retry looks
free. It is not worth doing.

The manifest is already evicted on settlement — a settled attempt holds
nothing, so residency reconciliation drops it on the next tick. What remains to
reclaim is the three bitsets: **7.5 KB per parked 20k-article job**, roughly
140 such failures per megabyte, against a manifest two orders of magnitude
larger that is already gone.

The cheap version is also unavailable. `ArticleDone(i)` returns `false` for an
out-of-range index, so simply dropping the bitsets would make a compacted job
report every article as not-done — silently, and plausibly. That is exactly the
silent-nil class this contract exists to remove, so compaction would require a
summary type with no per-article accessors threaded through every package that
reads progress. A cross-package API break for 7.5 KB a job is not a trade worth
making.

The rehydration half already exists and is exercised: `Retry` reopens the
attempt, the next tick hydrates, and resolution is re-derived from
`durable_runs` and `failed_articles`.

---

## 14. Failing versus degrading

The fallible surface is manifest-bearing mutation, and nothing else.

**Recovery paths never DEPEND on hydration.** Removing a job does not require
reading what is being deleted, and enumerating IDs does not require loading
manifests. A damaged job must remain removable, and one damaged manifest must
never prevent the daemon from starting.

"Depend on", not "never touch". A removal path may read a header field and get
a manifest read along with it; what matters is that an unreadable manifest is
recorded rather than raised, so the operation still answers and the job is
still removed.

**A hydration failure has exactly one meaning:** a job that needs its manifest
in order to run cannot load it. Fail *that* job. Do not fail the operation that
happened to observe it, and do not fail unrelated jobs. (See §11 for why a
cancelled context is not this case.)

**Do not distinguish concurrent removal from corruption.** That distinction was
only ever needed because reporting paths hydrated from disk. In the manifest
tier a vanished job makes the operation moot.

### A job's file set is fixed at ingest

The manifest blob and the `job_files` rows are both written once, from the same
file list, and neither changes shape again for the life of the job.
`file_index` is therefore **stable for the job's entire life**: nothing after
ingest removes a file, so nothing after ingest renumbers one.

`DiscardDeferredPar2` is the only *verdict* that changes a job's download
intent after ingest, and it does not touch the file set to do it. It is not
the only thing that moves `fetch_policy` there: `Job.undeferRecovery` reverses
a hold back to `FetchAlways` when damage appears, and residency hydration
re-applies whatever `job_files` holds through `Job.RestoreFetchPolicy` on every
eviction and re-hydration (#329). None of the three touches the file set. A
recovery
volume proven unnecessary keeps its `job_files` row exactly where it was; only
its `fetch_policy` column moves, from `FetchIfNeeded` to `FetchNever`. That
write is residency-independent: the checkpoint batch writes each file's row
from the always-resident progress record, so a discard on a job with no
manifest still persists.

`FetchPolicy` is a three-valued enum rather than a `Deferred` bool so that
"held pending a verdict" and "proven unnecessary" cannot both be true, and so
every read site has to say which it means. It is read through two predicates
that mean different things — `!= FetchAlways` for dispatch, completion and byte
accounting, and `== FetchIfNeeded` for the deferred-par2 queries — and the
failure mode of adding a fourth value and missing one site is silence: a policy
matching neither predicate is excluded from every aggregate and invisible to
the un-defer path, so its file is never fetched and never blocks completion.
`TestAllFetchPolicies_Exhaustive` parses the const block itself rather than
trusting the hand-written list.

Because the file set cannot change, a whole tier of machinery has no reachable
caller and does not exist: a manifest-and-rows rewrite, the staleness
generation counters that tracked whether persisted rows still matched a
changing file set, and the reconciliation retry for a rewrite a checkpoint
failed to land. All of it existed because dropping a file renumbered every
`file_index` after it.

### The size guard, and what it does not cover

`describesSameJobAs` compares article and file counts and is `RestoreContent`'s
guard. Its purpose is unchanged: a `Manifest`/`JobProgress` size mismatch would
panic inside `recompute` on a background goroutine with no `recover`, so it is
reported instead of tolerated.

Its cause has changed. No write path this process performs can produce a torn
pair: the manifest blob is written once, before the ingest transaction opens,
so a crash between them leaves an orphan manifest and no job row rather than a
disagreeing pair. A mismatch that reaches the guard now means a truncated or
damaged manifest blob.

**It is not what detects `job_files` rows altered out of band.** The restore
path fills the progress record by `file_index` under a bounds check and never
resizes it, so rows deleted or renumbered outside this process still satisfy
the size check and silently attach one file's metadata and outcomes to another
file's slot — no error, no log. What a manifest/`job_files` disagreement should
do is open (§17).

---

## 15. Scenarios

These thirteen traces are the design's validation and its regression suite:
each corresponds to a defect found in an earlier revision of the machine.
Columns are `state`, `next`, `intent`, what the job holds, and the rendered
status. Each has a matching `TestScenario_*` in
`internal/sched/scenario_test.go`, and
`TestEveryScenarioHasATest` fails if this section grows a scenario nobody
pinned.

### Scenario 5.1 — Pause mid-download, then resume

```
Fetching   —           Run    lease    running   → "Downloading"
  user pauses                                      intent=Pause
  downloader yields between articles; the dispatcher calls Park
Fetching   —           Pause  —        not run.  → "Paused"
  user resumes                                     intent=Run
  Advance branch 2: next unset, holds nothing → grantFor(Fetching)
Fetching   —           Run    lease    running   → "Downloading"
```

Never touches `Assessing`. A model that derived `next` jumped a partially
downloaded job straight into verification here.

### Scenario 5.2 — Pause at a boundary

```
Fetching   —           Run    lease              → "Downloading"
  download completes → SetNext(Assessing)
Fetching   Assessing   Run    lease    workDone  → ready
  user pauses; Advance branch 3 gated            → "Paused"
  user resumes; grantFor(Assessing); Transition clears next
Assessing  —           Run    lease+slot         → "Verifying"
```

### Scenario 5.3 — Repair loop and the crossing

```
Assessing  —           Run    lease+slot         → "Verifying"
  verdict Repairable → SetNext(Repairing)
Assessing  Repairing   Run    lease+slot         → ready
  grantFor(Repairing); Transition clears next
Repairing  —           Run    lease+slot         → "Repairing"
  repair done → SetNext(Assessing)
  ... → Assessing → verdict Complete → SetNext(Extracting)
Assessing  Extracting  Run    lease+slot         → ready
  Advance branch 3: IsCorrectness && IsProduction
  Cross(Extracting) → state, next cleared, lease yielded — ONE call
  reclaim(lease); grantFor(Extracting) — no-op, the slot was already held
Extracting —           Run    slot               → "Extracting"
```

**The slot is held continuously from the first `Verifying` line to the last** —
neither the repair verdict nor the repair's own completion releases it.
`needsSlot` makes `Assessing`, `Repairing`, `Extracting` and `Finalizing` all
slot-needing, and `releaseFor` frees a slot only for a destination that does
not need one. `Assessing → Repairing` is a same-zone move, not a demotion; the
one demotion in the work spine is `Assessing → Fetching`.

### Scenario 5.4 — Restart, both variants

```
persisted: Extracting  —            holds nothing
  branch 2 → grantFor(Extracting) → extraction RUNS

persisted: Extracting  Finalizing   holds nothing
  branch 3 → grantFor(Finalizing); Transition → does NOT re-extract
```

The completion marker survives the crash and distinguishes the two.

### Scenario 5.5 — Cancel, post-boundary

```
Extracting  —            Run     slot
  Cancel → intent=Cancel; IsProduction && running → gate, return nil
  unpacker finishes → SetNext(Finalizing)
Extracting  Finalizing   Cancel   slot            running == false
  Advance → finishCancel: not running → Finish(Cancelled), releaseFor(StateUnset)
                                                            → "Deleted"
```

`running` goes false once the unpacker finishes because `next` is set — that
alone is sufficient, and it is the only thing that changes at that line. **The
slot is not released there**: `Finalizing` is slot-needing exactly like
`Extracting`, so `releaseFor` keeps the slot across this same-zone move. It is
freed one step later inside `finishCancel`, by a separate
`releaseFor(StateUnset)` that is not part of `Finish` — which yields only the
lease.

### Scenario 5.6 — Contention at a boundary

```
Fetching   Assessing   Run    lease    pool B full
  branch 3: grantFor fails → no move; lease retained  → "Queued"
```

### Scenario 5.7 — Pause during `Finalizing`

```
Finalizing  —   Run    slot     → "Moving"
  user pauses: pause gates per-state, so the state runs to completion
  Finish(OutcomeOK)
Finalizing  —   Run    slot            — Finish yields only the lease (there is
                                          none here); the slot is not released yet
  Advance → settled branch: releaseFor(StateUnset)   → releases the slot
Finalizing  —   Run    —               → "Completed"
```

There is no such thing as a paused `Finalizing` job that must resume: pause can
only hold a job *before* `Finalizing` starts.

### Scenario 5.8 — A pre-boundary failure returns the lease

```
Fetching    —   Run   lease            → "Downloading"
  every server exhausted for some articles; fetch STOPS, not completes
  SetNext(Assessing)                     — only Assessing may decide
Fetching    Assessing  Run  lease
  ... Assessing verdict Unrecoverable
  l, _ := Finish(OutcomeUnrecoverable); reclaim(l)   ← lease returned
Assessing   —   Run   slot             — Assessing's slot is not released yet
  Advance → settled branch: releaseFor(StateUnset)   → releases the slot
Assessing   —   Run   —                → "Failed"
```

### Scenario 5.9 — Retry when pool A is exhausted

```
Fetching  —  Run  —   Outcome=Failed          → "Failed"
  user retries → Retry(j) → BeginAttempt(now)          (no lease needed)
Fetching  —  Run  —   holds nothing           → "Queued"
  ... capacity frees → branch 2 → grantFor(Fetching)
Fetching  —  Run  lease                        → "Downloading"
```

A `BeginAttempt` that demanded a lease dropped this retry permanently: the
lease could not be taken and nothing recorded that a retry was wanted.

### Scenario 5.10 — Paused, then failed, then retried

```
Fetching  —  Pause  —          → "Paused"
  ... the attempt settles Failed
Fetching  —  Pause  —          → "Failed"
  user unpauses: SetIntent(IntentRun)   — legal on a settled attempt
  user retries  → Retry(j) → BeginAttempt; branch 2 grants
Fetching  —  Run    lease      → "Downloading"
```

This is why `SetIntent` is legal on a settled attempt: refusing it there left a
job that could be neither unpaused nor usefully retried.

### Scenario 5.11 — Cancelling a never-run job

```
StateUnset  —  Run  —          → "Queued"
  Cancel → SetIntent(IntentCancel); finishCancel sees StateUnset → returns nil
  the tick's evictCancelledNeverRun removes it from the registry and the store
```

No attempt exists, so there is nothing to carry `OutcomeCancelled`.

### Scenario 5.12 — Cancelling a post-boundary job restored from a restart

```
persisted: Extracting  —  holds nothing
  Cancel → IsProduction, but running is FALSE (holds no slot)
        → Finish(OutcomeCancelled); reclaim(nil)          → "Deleted"
```

A gate keyed on "work not done" deadlocked here: `Advance` handles cancel
before granting, so the extraction that would have set `next` never ran.

### Scenario 5.13 — Cancelling a running `Finalizing` job

```
Finalizing  —  Run     slot       → "Moving"
  Cancel → IsProduction && running → gate
  the move and the user script complete; the worker calls Finish(OutcomeOK)
Finalizing  —  Cancel  slot            — the slot is not released yet
  Advance → Intent still Cancel → finishCancel's settled arm: releaseFor(StateUnset)
Finalizing  —  Cancel  —          → "Completed", Intent still IntentCancel
```

The cancel arrived after the last gate, and there is no gate after
`Finalizing`. Recording `Cancelled` would be false — the files moved and the
script ran — so the outcome is honest and the surviving `IntentCancel` is what
the UI reads to say the request came too late.

The trailing `Advance` matters here for a reason none of the other traces
share: `Intent` is still `IntentCancel` after the worker's own `Finish`, so
every later `Advance` keeps routing to `finishCancel` rather than the settled
branch — which is why `finishCancel`'s settled arm must release, or this exact
job's slot is stranded permanently.

---

## 16. Deliberately not built

Named here so they are decisions rather than omissions.

- **Anti-starvation floor on dispatch.** Strict priority means a low-priority
  job at 99% can sit indefinitely behind a high-priority job that keeps getting
  work. That is what priority *means*. Boosting jobs near completion introduces
  a second ordering and undoes the single-ordering property.
- **Interruptible production.** The cost of a slow pause is accepted in
  exchange for never representing partially-applied external work.
- **Per-job dispatchers.** The composition benefit is worth less than the
  single-ordering property.
- **A generic "gate" abstraction.** `Intent` has two non-default values and one
  consumer.
- **A separate `workComplete` flag.** It moves the `Finalizing` special case
  rather than removing it.
- **`Intent` on the `Attempt`.** A paused job that is retried stays paused.
- **Per-intent timestamps.** A history question, owned by the Checkpointer.
- **A speculative extraction area with promote/discard.** DirectUnpack extracts
  in place, and the bet that makes that safe is sound: a permanently failed
  article marks the whole set corrupt before extraction is admitted, and the
  repair skip fires only where there is no par2 to repair with. What is not
  claimed is that in-place extraction is free of consequence — a set that *is*
  marked corrupt leaves partial output in the download directory before
  re-extraction, and whether that collides or is harmlessly overwritten turns
  on the overwrite setting and has not been traced.
- **A migration path.** Standing Design Rule 1.
- **Crash consistency for a `Finalizing` that completed but was not settled.**
  Pre-existing, and owned by `docs/durability-contract.md`.

---

## 17. Open questions

1. **Priority orders nothing.** `Header.Priority` is stored, persisted,
   settable and rendered, and `internal/sched` never reads it; order is FIFO by
   insertion sequence. Closing this means building the priority-ordered list
   *and* the reorder resequence together — `restore` reads order from
   `SortKey` alone, so a reorder that did not renumber would vanish at the next
   restart.
2. **Does pausing a job change its queue position?** Reorder says a paused
   never-started job takes effect at the next lease issuance, but nothing says
   whether pausing itself moves it. Probably not.
3. **What should a manifest/`job_files` disagreement do at boot?** The size
   guard runs on the hydration path and not on the load path, and it would not
   catch rows renumbered out of band in either case (§14).
4. **Nothing pins the memory figures in §13.** They are derived from the field
   types and are re-derivable by hand, but no test asserts bytes per article,
   and the terminal-compaction decision rests on arithmetic rather than on a
   measurement a change would break.
5. **Nothing pins residency parity for the byte figures.** Remaining, expected
   and failed bytes are supposed to be identical for the same job resident and
   non-resident — that is the property the derived-remaining design exists to
   guarantee — and no test compares the two.
