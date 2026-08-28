# Lifecycle Intents — Design

**Status:** proposed, revision 7. Revisions 1, 2 and 3 were each reviewed
externally; §10 records what changed and why. Revision 2 had a data-losing
flaw and revision 3 a lease-leaking one; both are described there.

**Amends** `docs/superpowers/specs/2026-08-25-job-lifecycle-design.md`:

| Section | Disposition |
|---|---|
| §8.2 (`Waiting` unifies pause and slot-waiting) | superseded — §3.2 |
| §10 (every persisted job restores to `Waiting`) | superseded — §3.8 |
| §3.1 / D1 (`State()` for a never-run job) | superseded — §3.2 |
| §8.1.1 (reorder table keys on `Waiting{Next: Fetching}`) | superseded — §4.7 |
| §6 (`BeginFetch(l *Lease)`) | amended — the lease is granted by the Queue, not required to open an attempt; §3.5 |
| §8.3, §8.4, §12 | narrowed |

Everything else stands. §6's argument is not merely preserved but load-bearing
throughout.

**Resolves:** issue #441, whose filed premise this document rejects — see §2.3.

**Scope:** `internal/job`, and an amendment to the swap plan's `Queue`.

---

## 1. The problem

`internal/job` landed in #439 with `Waiting` as one of seven states, carrying a
payload (`Next`, `Reason`) meaningful for that state alone. Two things it
cannot express have since surfaced.

### 1.1 A pause request has nowhere to live

Prior spec §8.3 makes pause a **gate, not an interrupt**, and then requires the
UI show that:

> The cost is honest: pausing mid-repair on a large set does nothing for
> minutes. The fix is to *show* that ("finishing repair, then pausing"), not to
> make repair interruptible.

The model cannot represent that sentence. A pause is a `WaitReason`, and
`Attempt.setReason` refuses unless the attempt is already `Waiting`:

```go
if a.state != Waiting {
    return fmt.Errorf("%w: cannot set a wait reason on an attempt in state %s", ErrNotWaiting, a.state)
}
```

Pausing a `Repairing` job returns `ErrNotWaiting`. The request has no home until
the job has *already* stopped. The daemon ships per-job pause today
(`queueSetPaused` in `internal/api`), so this is a gap against existing
behaviour.

A gate has two parts — the flag and the check. The design modelled only the
moment the check fires.

### 1.2 `WaitReason` mixes two ownerships in one field

| Reason | Who can know it |
|---|---|
| `UserPaused` | the job — nothing else can |
| `NoLease` | whether this job holds a pool-A lease |
| `NoComputeSlot` | whether this job holds a pool-B slot |
| `GlobalPause` | the Queue: already owns this, persisted via `Store.SetPaused` |

Three of the four are derived, yet all four are stored on the `Attempt` and
independently assignable. Standing Design Rule 2 names this: *"a derived value
that is also persisted… creates a second source of truth, and the stored copy
is the one that drifts."*

The drift is reachable. A job parked at `NoComputeSlot` that is then globally
paused has its reason overwritten, and nothing remembers what it was waiting
for.

---

## 2. The pattern behind #439's defects

| # | Escape |
|---|---|
| 1 | `transition(Waiting)` stranded an attempt — movable afterwards only by `finish` |
| 2 | `Extracting → Waiting → Fetching` — two legal edges composing into the one the graph forbids |
| 3 | `hold(Finalizing) → hold(Fetching) → transition(Fetching)` — a second hold re-declared the destination |
| 4 | `BeginAttempt` let a crossed job open a fresh attempt in Correctness |
| 5 | `finish` guarded on `IsProduction(a.state)`, but `hold` had already overwritten `a.state` with `Waiting` |

### 2.1 `state` is a lossy summary

`legalEdges` is a relation over `(from, to)` pairs — a Markov model. None of the
machine's invariants are expressible that way:

| Invariant | Shape |
|---|---|
| Never return from Production to Correctness | reachability |
| Resume where the decision was made | memory |
| `Unrecoverable` means never crossed | history |
| A crossed job cannot be retried | history, spanning attempts |

Each was patched with a latch remembering what `state` forgot. Every defect
lived at a latch boundary.

### 2.2 Three of five live on `Waiting`

Defects 1, 2 and 3 are all `Waiting`: the only state with a mandatory payload,
and the only state two doors write.

### 2.3 What #441 proposed, and why it was wrong

#441 argued pause and admission are two situations wearing one name, and that
pause should be an orthogonal axis because *"pause has no destination."*

That premise is false. Under §8.3's gate model a paused job has finished its
state and is held at a boundary with a decided next step — structurally
identical to waiting for a compute slot. §8.2's unification is correct.

What §8.2 did not notice is that `Waiting` was carrying **two** facts, not one:
*where the job will go next*, and *whether the current state's work is
finished*. The first is often derivable. **The second never is** — it is a fact
about the world, not about the graph. Revision 2 of this document assumed both
were derivable and lost data as a result (§10).

---

## 3. The design

### 3.1 `Intent` is a fourth orthogonal axis

Prior spec §3 establishes that position (`State`), execution (`Activity`) and
verdict (`Outcome`) are orthogonal. `Intent` is a fourth: **what a person has
asked of this job**, independent of where it is or what it is doing.

```go
type Intent uint8

const (
    IntentRun    Intent = iota // default
    IntentPause
    IntentCancel               // latches
)

func (j *Job) Intent() Intent
func (j *Job) SetIntent(i Intent) error
```

`Intent` lives on the **`Job`**: a paused job that is retried stays paused,
because the pause was a statement about the job, not about one run of it.

`IntentCancel` latches — `SetIntent` refuses to leave it, so "cancel then
unpause" cannot revive a cancelled job. `IntentRun ↔ IntentPause` is freely
reversible.

`SetIntent` is legal in every state, including once the current attempt is
settled. Revision 3 refused it there, reasoning by analogy with `setReason` —
a wait *reason* is meaningless when nothing is being waited for. An *intent* is
not: a settled job may be retried, and the intent it carries governs what
happens when it is. Refusing left a real trap — pause a job, let it fail, and
it could be neither unpaused nor usefully retried, because the retry produced
an attempt gated by an intent that could no longer be cleared.

Only the `IntentCancel` latch restricts transitions.

**Retry does not clear the latch, and that is deliberate.** A cancelled job
whose attempt is settled still carries `IntentCancel`, so `q.Retry(j)` would
open an attempt that `advance` cancels on its next tick. That reads like a trap
and is not one: cancel renders as `Deleted` (§4.4), and prior spec D8 makes a
full redo **a re-added NZB starting a new `Job`**, not a new attempt on a
cancelled one. Clearing the latch on retry would let a job the user deleted come
back through a path that never re-asked them.

Stated here because §3.1's stated rationale — *"cancel then unpause"* — does not
cover retry, and a reader could reasonably infer the latch was only about pause.
It is not: it is about cancel being final for this `Job`.

### 3.2 `Waiting` is removed; `StateUnset` becomes the zero

A job that is not running is **still at the state it last occupied**.

`State` becomes:

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

> **Superseded during implementation (change 03). This note scopes the whole
> document, not just this block.**
>
> `Finished` is no longer a `State`, and the `crossed` field no longer exists.
> Both are gone for one reason: `finish` used to write `a.state = Finished`,
> which **erased the position the attempt settled at**, so `IsProduction(state)`
> stopped answering after settling and a shadow latch had to remember what the
> state had forgotten. `finish` no longer touches `a.state`, so:
>
> - **Settledness is an `Outcome` fact.** `isOpen()` is `outcome ==
>   OutcomePending` and always was; assigning the verdict is what closes the
>   attempt. A settled attempt keeps its position — `Finalizing` + `OutcomeOK`,
>   not `Finished` + `OutcomeOK`, which is strictly more information and is what
>   a history view wants: *where did this attempt end?*
> - **`crossed` is derived**: `IsProduction(a.state)`. Sound only because no
>   edge runs from Production back to Correctness, so
>   `TestBoundaryIsUnreachableByAnyPath` now asserts the derivation against
>   replayed history at every reachable configuration rather than leaving it
>   assumed.
> - **`ErrFinishRequired` is deleted.** `transition` cannot be asked to reach
>   settledness because no `State` names it: a runtime error became a compile
>   error.
> - **A settled attempt can be in a Correctness state.** `OutcomeFailed` from
>   `Fetching` settles at `Fetching`. Anything below that treats "settled" and
>   "in a Correctness state" as mutually exclusive describes the old model.
>
> **Every NORMATIVE occurrence — the tables and rules Half B will build code
> from — has been corrected in place and says so.** An external review was
> right that this note alone is not enough for them: a blanket "read the rest
> as history" does not stop someone writing a lease gate or a SQLite schema
> keyed on a state row two hundred lines below. The corrected sites are §3.4's
> resource table and the two `holds`/`needsLease` arguments around it, §3.6's
> door table, and §3.8's persistence shape.
>
> **Read every REMAINING occurrence of `Finished`-as-a-State or
> `crossed`-as-a-field below as the superseded model.** They are left in place because the arguments
> around them — why the latch was needed, what the two escapes were, how
> rendering keys — remain the record of how the design got here, and rewriting
> them would destroy that record while pretending the reasoning was always this
> shape. Where a statement below would lead a reader to *act* wrongly rather
> than merely read history, it is corrected in place and says so.

`StateUnset` exists because removing `Waiting` would otherwise make `Fetching`
the zero value, so a zero `StateView` would read as an active download —
Rule 2's "a type with a valid zero used as a key" smell, which the old ordering
avoided by accident.

`AllStates()` returns the six real states. `Job.State()` returns
`StateView{State: StateUnset}` for a job with no attempt, **superseding prior
spec §3.1 and D1**, which specify `StateView{State: Waiting, Next: Fetching,
Reason: NoLease}`.

`TestAllStates_Exhaustive` parses `state.go` by AST
(`stateConstantsFromSource`), rejects any declared constant missing from
`AllStates()`, and asserts `len(AllStates()) == len(declared)`. `StateUnset` is
declared and deliberately unlisted, so both halves fail as written.

**Relaxing either half is not the fix** — that would let a genuinely forgotten
state slip through. Instead the test asserts the exception by name:

> `declared` must equal `AllStates()` ∪ `{StateUnset}`, exactly.

A second sentinel, or a real state omitted from `AllStates()`, still fails. The
sentinel is named in the assertion rather than subtracted by a count, so the
test says *which* constant is exempt and why, and adding another requires
someone to write it down here.

### 3.3 `next` marks completion, and carries the verdict

> **`next` is set ⟺ the current state's work has ENDED and the job continues
> to another work state.** Its value is where it continues to. When work ends
> and the job does *not* continue, `finish` settles the attempt instead, and
> `next` stays unset.

*Ended*, not *succeeded*. A `Fetching` that exhausts every server has ended —
every article was attempted — and `Assessing` is entitled to decide what the
result means. Failure is not a separate contract.

This is the fact `Waiting` was carrying, and it is the correction that defines
this revision. Revision 2 confined `next` to `Assessing` on the grounds that
every other state has one spine successor, so the *value* is derivable. The
value is; **the presence is not.** "Has this download finished?" is a fact about
the world, and no amount of graph inspection answers it.

Who writes it: **the worker that completes a state's work**, as the last act of
that work.

| State completes | Writes |
|---|---|
| `Fetching` | `SetNext(Assessing)` |
| `Assessing` | `SetNext(verdict)` — `Fetching`, `Repairing` or `Extracting` |
| `Repairing` | `SetNext(Assessing)` |
| `Extracting` | `SetNext(Finalizing)` |
| `Finalizing` | `Finish(OutcomeOK)` — see below |

**`Finalizing` is not an exception to that rule — it is an instance of its
second clause.** Its work ends and the job continues nowhere, so it settles via
`Finish` and never sets `next`. Revisions 3 to 5 described it as an exception,
which made it look like a special case needing special handling in `advance`,
`finishCancel` and `waitReason`; it is not, and stating the rule in two clauses
removes the appearance. An earlier draft added a separate `workComplete` flag to make the marker uniform;
it does not help, because `advance` would then need a special case for
"`Finalizing` + complete → `Finish(OK)`" — `next` cannot carry an `Outcome`. The
special case only moves. Keeping `Finish` atomic means the window between "work
done" and "recorded done" does not exist. A crash before that single call means
the work was never recorded, and restart re-runs `Finalizing` — the same
guarantee every other state has, and `docs/durability-contract.md`'s concern,
not this document's.

**When a state's work fails**, the rule above applies unchanged: work has
ended, and what follows depends only on whether the job continues.

| Failure in | Path |
|---|---|
| `Fetching`, `Repairing` | continues → `SetNext(Assessing)`. §5 makes `Assessing` the only thing entitled to decide what a failure means |
| `Assessing` | does not continue → `Finish(OutcomeFailed)` or `Finish(OutcomeUnrecoverable)` |
| `Extracting`, `Finalizing` | does not continue → `Finish(OutcomeFailed)`. Crossing is irreversible, so there is nowhere to go back to |

A failed `Fetching` therefore looks exactly like a successful one to the machine
— articles were attempted, some are missing, and whether that is recoverable is
par2's question, not the state machine's. This is Standing Design Rule 3 ("a bad
article costs only its own bytes") applied one level up.

**The `Assessing → Repairing → Assessing` loop is bounded by the Assessor, not
by this machine.** Nothing in `internal/job` counts repair attempts, and
deliberately so: how many repairs are worth attempting is a `Policy` question
that `internal/par2`'s verdict owns (prior spec D5). Named here because an
unbounded loop is the obvious failure and it should be clear whose it is.

Three rules on `SetNext`:

1. **Guarded by `legalEdges`.** A value the current state could not reach
   directly cannot be recorded either.
2. **Write-once per visit.** `SetNext` refuses when `next` is already set to a
   *different* value, and is a no-op for the same one. Without this, a verdict
   of `Repairing` could be overwritten with `Extracting` and the job would cross
   the boundary skipping repair — defect 3's mechanism surviving into a new
   door.
3. **Cleared by the move.** `transition` and `Cross` each clear `next` as part
   of taking it, so an attempt that never re-enters `Assessing` cannot carry a
   stale verdict.

   > **Corrected during implementation (Half A, commit `779f95e6`).** This rule
   > originally read "Nothing else clears it", and that is wrong: `finish`
   > clears `next` too. A settled attempt that kept a destination would report
   > a move it will never take, and `Finished` has no outgoing edge for it to
   > name. So `next` has **four** writers, not three — `setNext` sets it;
   > `transition`, `Cross` and `finish` clear it — and
   > `TestNextWrites_MatchTheEnumerationStatedInProse` asserts exactly that
   > four. `TestAttempt_FinishClearsNext` pins the clearing itself. §6.8's
   > wording ("`SetNext`/`transition`/`Cross` the only writers of `next`")
   > inherits the same error and is superseded by this note.

`transition` keeps its `to == next` check. Its justification changes: with
`Waiting` gone it no longer guards the boundary, and its sole remaining purpose
is enforcing that once a state's work has decided where to go, nothing else may
choose.

### 3.4 Running-ness and grantability are different questions

Revision 2 conflated these into one predicate and deadlocked (§10). They are
asked about different states:

| Question | About | Asked by |
|---|---|---|
| **Is this job running?** | the **current** state | rendering |
| **May it take its next move?** | the **next** state | scheduling |

Prior spec §6 answers the first, and revisions 1 and 2 failed to use it:

```go
func (j *Job) BeginFetch(l *Lease) error  // cannot be called without one
func (j *Job) Surrender() *Lease          // called on crossing
```

**The Job holds the Lease object.** So:

| State | Requires |
|---|---|
| `Fetching` | lease |
| `Assessing`, `Repairing` | lease + compute slot |
| `Extracting`, `Finalizing` | compute slot (the lease was surrendered at crossing) |
| `StateUnset` | nothing |
| *any state, once settled* | nothing |

> **Corrected by change 03 — this row is normative and the scoping note above
> is not enough for it.** There is no `Finished` state. A settled attempt keeps
> the position it settled at, so it can be sitting in `Fetching` or `Assessing`
> and require **nothing**. Any gate written as `needsLease(state)` alone will
> therefore request a lease for a settled job. **Settledness must be checked
> first** — `!a.isOpen()`, i.e. `Outcome.IsSettled()` — and only then the
> position consulted.

```
holds(j)   ≡ j holds everything its CURRENT state requires
running(j) ≡ the attempt is open && holds(j) && next is unset
```

All three conjuncts are load-bearing. A job holding a slot whose work is
finished is not running, it is waiting to move — that is the `next` clause. And
the open-attempt clause is not redundant — and change 03 made it MORE
load-bearing, not less. It was written when a settled attempt sat in a
`Finished` state that required nothing, so `holds` was vacuously true for it.
A settled attempt now keeps its position, so one settled at `Fetching`
requires a lease and `holds` is **not** vacuous: it may be genuinely false, or
genuinely true if the lease was never reclaimed. Either way the attempt is not
open, and only the open-attempt clause says so. Without it a settled job would
report as running. An earlier draft of this
section omitted it and was saved only by §4.4's table ordering, which is exactly
the kind of accidental correctness this document exists to remove.

Three consequences follow without further machinery:

- An actively-downloading job holds a lease; a paused one does not. `{Fetching,
  holds}` and `{Fetching, does not}` are distinguishable, so no `ActDownload`
  activity is needed — there is none (`grep -n 'Act[A-Z][a-zA-Z]*'
  internal/job/activity.go` returns ten activities, none for fetching).
- A job re-entering `Fetching` from `Assessing` still holds its lease, so it is
  running by construction. §8.1's non-starvation guarantee stops being a rule
  and becomes a consequence of never having let go.
- A job with capacity free but low priority correctly holds no lease, so
  `NoLease` is the honest reason.

The gate — intent and queue-wide pause — is separate from resources, and is
the only part shared between the two questions:

```go
// gatedBy reports an intent or queue-wide gate. Resources are NOT consulted:
// they are a grant question, not a gate question. PURE.
func (q *Queue) gatedBy(j *job.Job) (job.WaitReason, bool) {
    if j.Intent() == job.IntentPause { return job.UserPaused, true }
    if q.paused                      { return job.GlobalPause, true }
    return 0, false
}

// waitReason explains why j is not running — about its CURRENT state. PURE,
// so it is safe on the render path.
func (q *Queue) waitReason(j *job.Job) (job.WaitReason, bool) {
    v := j.State()
    if v.Outcome.IsSettled() { return 0, false }   // terminal: not waiting for anything
    if q.running(j)          { return 0, false }
    if r, gated := q.gatedBy(j); gated { return r, true }
    if v.State == job.StateUnset { return job.NoLease, true }   // waiting to start
    want := v.State
    if v.Next != job.StateUnset { want = v.Next }   // work ended; it waits on the NEXT state
    if needsLease(want) && !j.HoldsLease() { return job.NoLease, true }
    return job.NoComputeSlot, true
}

// grantFor acquires what s requires and j does not already hold. Acquisition
// happens ONLY here.
func (q *Queue) grantFor(j *job.Job, s job.State) bool
```

The two early returns before the resource checks are not decoration.
`running(j)` is false for a settled attempt and for a never-run job alike, but
after change 03 for two DIFFERENT reasons: a never-run job requires nothing, so
`holds` is vacuously true and only the open-attempt clause excludes it, while a
settled attempt keeps its position and may require a lease it does or does not
hold. `needsLease` is false for `StateUnset`, and is whatever the settled
position says — **it must not be consulted for a settled attempt at all.**
Without these returns
both fall through to `NoComputeSlot`: a terminal job reported as waiting for a
compute slot, and a never-run job reported as waiting for one when it is
waiting for a lease.

**Which state's requirements to test depends on whether work has ended.** A job
that is mid-work and not running lacks something its *current* state needs. A
job whose work has ended is waiting on what its *next* state needs — and those
differ. `Assessing{next: Fetching}` after a `NeedsMore` verdict holds its lease
and needs only that lease to continue, so testing `Assessing`'s requirements
would report `NoComputeSlot` for a job that is not waiting for a slot at all and
should be granted at once. An earlier revision tested the current state
unconditionally and had exactly that bug.

`needsLease(s)` is load-bearing rather than decorative. `Extracting` and
`Finalizing` legitimately hold no lease — it was surrendered at the crossing —
so an unconditional `!j.HoldsLease()` reports `NoLease` for a post-boundary job
that is in fact waiting for a compute slot. Revision 3 had exactly that bug.

`IntentCancel` is absent from `gatedBy`: `advance` handles it first, so no
cancel value reaches the render path.

### 3.5 Four doors

Revision 2 had three. Tracing the crossing (§5, scenario 3) showed it needs its
own, for the same reason `finish` has one.

| Door | Produces | Sole writer of | Yields the lease |
|---|---|---|---|
| `BeginAttempt(now)` | an open attempt at `Fetching`, holding nothing | `attempts` | — |
| `transition(to)` | ordinary spine moves | — | — |
| `Cross(to) (*Lease, error)` | the one boundary edge | `state` (with `transition`) | yes |
| `finish(o, now) (*Lease, error)` | a settled attempt — `state` **unchanged** | `outcome` | yes |

**`BeginAttempt` does NOT take a lease.** Revision 3 changed it to
`BeginAttempt(l *Lease, now)`, reasoning that `Fetching` requires a lease so
opening an attempt without one should not compile. That is wrong on this
design's own terms: `Fetching` **holding nothing** is a legal, representable
state — it is exactly what a paused fetch and a restarted fetch look like
(§3.8). Requiring a lease to *reach* `Fetching` contradicts the model that makes
those two states expressible.

Prior spec §6's rule survives where it actually bites: **the downloader** cannot
obtain a `Manifest` or `StorageBarrier` without a lease. You cannot *fetch*
without one; you can be *at* `Fetching` without one, and not be fetching.

Reverting this fixes three problems revision 3 created: a retry when pool A is
full has nowhere to strand (the attempt simply opens and waits for a grant), a
lease passed to a `BeginAttempt` that returns `ErrBoundaryConsumed` cannot be
orphaned, and neither can one passed to a `BeginAttempt` that no-ops because an
attempt is already open. `ErrBoundaryConsumed` still guards it.

**`Cross` is new.** Count the Correctness→Production edges in the six-edge
spine (§4.1): `Assessing → Extracting`, and no other. Doing it as two calls
reproduces defect 5's exact shape —

```go
j.Transition(Extracting)   // sets crossed
l := j.Surrender()         // returns the lease
q.poolA.put(l)
```

— where forgetting the second leaks a pool-A slot permanently and **silently**,
and doing them in the other order leaves a job in `Assessing` holding no lease
that `Assessing` requires. Two coordinated mutations in different objects, one
forgettable, failing without an error. That is a check where an owner is owed:

```go
// Cross is the sole door across the irreversible boundary. It sets state,
// latches crossed, clears next and yields the lease in one call. There is no
// way to do one without the others.
//
// It validates exactly what transition would have: the attempt must be in
// Assessing, and to must equal a.next. Without both, Cross would be a hole
// in the single-decider property that transition's own to == next check
// exists to protect - a caller could cross from anywhere, to anywhere in
// Production, ignoring the verdict.
func (j *Job) Cross(to State) (*Lease, error)
```

`Cross` does not null the handle itself — it calls `surrenderLocked`, which
stays the **sole releaser**. Pause and `finish` release through the same call,
and the exported `Surrender` is a lock-taking wrapper around it for a holder
with no open attempt (§3.9).
Two independent paths nulling one handle would be two writers of the same field,
and the whole point of `Cross` is to remove a coordination, not add one.

**`Cross` makes the leak a single call site, not unrepresentable.** Revision 3
claimed the stronger thing and was wrong. The caller still receives a `*Lease`
and can still drop it — by panicking, by an early return, or by forgetting. What
`Cross` removes is the *coordination*: it is no longer possible to change state
without also being handed the lease. That is worth having and it is a guard, not
a proof. A callback form (`j.Cross(to, q.reclaim)`) would close it, and is
rejected: it would take `Queue.mu` while `Job.mu` is held, inverting prior spec
§7.1's lock order.

`transition` correspondingly refuses `IsCorrectness(from) && IsProduction(to)`.
The design's central invariant gets a door proportionate to it.

### 3.6 `advance`

```go
// advance runs under q.mu, per prior spec §7.1's lock order (Queue.mu before
// Job.mu). It is never called concurrently for the same job; without that,
// two calls could each read next and race to take it, and the loser's
// Transition would fail on the to == next check.
func (q *Queue) advance(j *job.Job) error {
    if j.Intent() == job.IntentCancel {
        return q.finishCancel(j)                     // §3.7
    }
    v := j.State()

    // 1. Never run: start it, if permitted. No lease is needed or taken here;
    //    branch 2 grants it, exactly as for a paused or restarted job.
    if v.State == job.StateUnset {
        if _, gated := q.gatedBy(j); gated { return nil }
        return j.BeginAttempt(q.now())
    }
    // A SETTLED attempt is never reopened here. Retry is an explicit user
    // action — q.Retry(j) calls BeginAttempt directly — not something a
    // scheduling tick decides. Because BeginAttempt needs no lease (D-I12),
    // a retry cannot fail for want of capacity: it opens the attempt and
    // branch 2 grants when capacity frees.
    if v.Outcome.IsSettled() { return nil }

    // 2. Current state's work is unfinished: make it runnable. This is the
    //    resume path AND the restart path — they are the same path.
    if v.Next == job.StateUnset {
        if q.holds(j) { return nil }                 // already working
        if _, gated := q.gatedBy(j); gated { q.park(j); return nil }
        q.grantFor(j, v.State)
        return nil
    }

    // 3. Work is finished: move.
    if _, gated := q.gatedBy(j); gated { q.park(j); return nil }
    if job.IsCorrectness(v.State) && job.IsProduction(v.Next) {
        l, err := j.Cross(v.Next)
        if err != nil { return err }
        q.reclaim(l)                                 // no-ops on nil; see §3.9
        q.grantFor(j, v.Next)                        // may fail; branch 2 retries
        return nil
    }
    if !q.grantFor(j, v.Next) { return nil }
    return j.Transition(v.Next)
}
```

`advance` writes no job state on any blocked path, so a lost acquisition race
costs a tick, never a verdict. **It takes no target** — the target is `next`,
written by the worker that finished the state.

```go
// park releases what a gated job must not keep holding. Both gated paths go
// through it: §3.8's deadlock is not hypothetical, and a `return nil` that
// merely declines to move leaves a paused job holding a pool-A lease forever.
func (q *Queue) park(j *job.Job) {
    q.reclaim(j.Surrender())   // no-ops when nothing is held
}
```

> **Corrected during implementation (Half B1, D3 — the lease audit was strong
> enough, but the slot had no audit at all).** This pseudocode releases only
> the lease. Reviewed against `Job.Surrender`, which is §3.9's own definition
> of the lease's SOLE releaser and yields nothing else, that leaves `park`
> silently keeping any compute slot the parked position held — the exact
> pool-B leak D3 named. The built `park` (`internal/sched/advance.go`) is:
>
> ```go
> func (q *Queue) park(j *job.Job) error {
>     q.releaseFor(j, job.StateUnset)
>     return q.reclaim(j.Surrender())
> }
> ```
>
> `releaseFor(StateUnset)` is unconditional, because `needsSlot(StateUnset)`
> is always false — see the table correction below.

**Branch 2 checks `q.holds(j)` before the gate, and that order is deliberate.**
`holds` means a worker owns the job's resources and is using them — the
`Manifest` and `StorageBarrier` come with the lease (§6). Surrendering while a
downloader is mid-article would pull them out from under it. Gating never
interrupts work (§8.3), so a gated job that still holds keeps holding until its
worker yields.

**The yield is what hands the resources back, and it needs an owner.** For every
state but `Fetching`, §8.3 gates per-state: the worker runs to the end and sets
`next` or settles — after which branch 3 gates and parks, and it is `park`
that releases what the job holds (§3.9, and §5.3/§5.5's corrected traces),
never the worker itself. `park` releases for `StateUnset`, the one destination
that needs nothing, so this case is unconditional; it is not the general rule
for every move, only for a gated one. `Fetching` is the one state that gates
*per-article*, so its worker stops without its work having ended and `next`
stays unset. That yield is not a completion and must not be reported as one:

> **A worker that stops on a gate without ending its work reports `yielded`, and
> the dispatcher calls `q.park(j)`.** `advance` cannot do it — branch 2 correctly
> declines to touch a job a worker still holds.

Half B owns the dispatcher side. Named here because the alternative is a paused
`Fetching` job holding a pool-A lease forever, which §3.8 says is a deadlock,
and because prose in §3.8 and scenario 5.1 described this handoff without ever
giving it a caller.

Only the lease needs a separate table entry (§3.9's), and that follows from
§8.3 rather than being a separate rule: gating never interrupts work, so a job
holding a compute slot is by definition mid-state and not yet gated. Once a
job IS gated, `park` releases everything it holds in one call, because
`park`'s target is `StateUnset`, and `StateUnset` needs nothing — not because
the worker released the slot itself while finishing its state's work (§3.6's
correction above; the worker only sets `next` or settles). A job gated at
`Extracting{next: Finalizing}` therefore holds neither — the lease went at the
crossing, and the slot goes at `park`.

Crossing before acquiring the slot is deliberate, and branch 3 deliberately
ignores `grantFor`'s result there: the decision was already made and recorded in
`next`, crossing only *adds* pool-A capacity, and a job that crosses and then
fails to get a slot is simply not running until it does. Branch 2 grants it on a
later tick. It cannot go back, and does not need to.

**Resume needs no notification.** `SetIntent(IntentRun)` writes a flag; the
scheduling loop calls `advance` on its ordinary cadence and picks it up. `Job`
cannot call `Queue` and does not need to.

### 3.7 Cancel

§8.4 makes cancel an interrupt before the boundary and a gate after it:

```go
func (q *Queue) Cancel(j *job.Job) error {
    if err := j.SetIntent(job.IntentCancel); err != nil { return err }
    return q.finishCancel(j)
}

func (q *Queue) finishCancel(j *job.Job) error {
    v := j.State()
    if v.State == job.StateUnset {
        return q.discard(j)          // never ran: no attempt to settle; drop it
    }
    if v.Outcome.IsSettled() {
        return nil                   // already closed, by cancel or otherwise
    }
    if q.running(j) {
        // A worker owns this job's resources and is using them. §8.4's
        // interrupt/gate split decides what to DO about that, but neither
        // arm may seize a lease or slot out from under a live worker.
        if job.IsProduction(v.State) {
            return nil               // gate: let it reach the boundary
        }
        q.abortWorker(j)             // interrupt: stop it now
        return nil                   // settled on the tick after it yields
    }
    l, err := j.Finish(job.OutcomeCancelled, q.now())
    q.reclaim(l)
    q.releaseSlot(j)                 // Assessing/Repairing hold one too
    return err
}
```

**The gate is `IsProduction && running`, not `IsProduction && !workDone`.**
Revision 3 used the latter and it failed three ways, all because `Finalizing`
never sets `next` (§3.3) so `!workDone` is permanently true there:

- a `Finalizing` job could never be cancelled at all;
- a post-boundary job **restored from a restart**, holding nothing, gated
  forever — `advance` handles cancel before granting, so the work that would
  have set `next` never ran;
- a `Finalizing` job waiting for a compute slot gated forever, though no work
  was in flight to protect.

`running(j)` asks the question §8.4 actually poses — *is work in flight?* —
rather than a proxy that one state cannot express.

**Neither arm settles while a worker is live.** §8.4 says a pre-boundary cancel
aborts *immediately*, and an earlier revision read that as "settle and reclaim
now". It cannot: the `Manifest` and `StorageBarrier` come with the lease (§6),
so reclaiming one from under a downloader mid-article is a use-after-free in all
but name. "Immediately" describes when the worker is **told to stop**, not when
its resources are taken. The interrupt arm aborts the worker and settles on the
following tick, once `running(j)` has gone false — the same handoff §3.6
specifies for a gated yield.

**A cancel must also release the compute slot.** `Assessing` and `Repairing`
hold one alongside the lease (§3.4), and an earlier revision reclaimed only the
lease, leaking pool-B capacity on every cancel from those two states.

**A never-run job cannot be settled**, because `Outcome` lives on the `Attempt`
and there is none: `Job.Finish` returns `ErrNoOpenAttempt`
(`internal/job/job.go`), from the same `withOpenAttempt` check every other
mutating door uses — reached via the `withOpenAttemptLease` adapter, which lets
a door return the lease it yields. Cancelling a queued job therefore
**removes it from the queue** rather than settling it, which is what upstream
does and what a user means. `discard` is the Queue's, and is named here only so
the case is not silently unhandled.

**Cancelling a running `Finalizing` job lets it complete as `OutcomeOK`** — see
§9, D-I11.

### 3.8 Restart, and why it is the pause path

This **supersedes prior spec §10's** *"every persisted job restores to
`Waiting`. There is no other legal option."* `Waiting` no longer exists.

The argument behind it is unchanged and is what the new rule preserves:
**nothing persists a lease.**

> **Every persisted job restores to the state it was persisted at, holding
> nothing.** `state`, `next`, `crossed`, `outcome`, `assessed` and `Intent`
> persist — all are decisions or history, none is a resource.

`assessed` is easy to omit and its loss is silent. It latches when an attempt
first reaches `Assessing`, and `ToSABnzbd` reads it to tell a re-entry fetching
recovery volumes (`Fetching`) from a first-pass download (`Downloading`). Drop
it across a restart and a job resuming a par2 fetch reports itself as starting
its download over — wrong to every API client, and wrong in a way no error
surfaces.

Persistence stores the **raw `Attempt` fields**, not a derived view. So a
settled attempt persists and restores with the position it settled at plus its
`Outcome`, and `advance` declines it at the `v.Outcome.IsSettled()` check
exactly as it would have before the restart. **`StateUnset` means one thing
only: the job has no attempt at all.** It is never what a settled attempt
restores to.

> **Corrected by change 03 — normative, and a schema will be built from it.**
> This section previously said a settled attempt persists as `Finished`, and
> that a crossed-then-settled attempt's crossing was recoverable *only* from
> the `crossed` latch. Neither holds. `finish` no longer touches `state`, there
> is no `Finished` value to store, and `crossed` is not a column: it is
> `IsProduction(state)`, derived on read.
>
> For Half B this means the persisted shape is **`state` + `outcome`, with no
> crossing flag**. A schema carrying a `crossed` boolean would be storing a
> value recomputable from a column beside it — the redundancy this change
> existed to remove — and the stored copy is the one that would drift.
>
> §4.3's "`finish` erases `state`" is likewise superseded: it erases nothing.

Restoring `next` is what makes this correct rather than destructive. A job
persisted mid-`Extracting` restores with `next` unset, so `advance` branch 2
acquires a slot and extraction **runs**. A job persisted after extraction
finished restores with `next = Finalizing`, so branch 3 moves it on **without
re-extracting**. The completion marker survives the crash and says which
happened.

Pause takes the same path. Surrendering the lease evicts the `Manifest` and
`StorageBarrier` with it — §6 is explicit that there is no other way to hold
either — so a paused job holds nothing, exactly like a restarted one. Resume
re-acquires, re-reads `manifests/<id>.json.gz`, and the barrier reconciles
against the disk (§10.1). **Pause/resume and crash/restart are one code path**,
and that is a property of the design rather than a coincidence: both are "this
job holds nothing and its work is unfinished".

Memory is bounded by pool A, not by queue depth: only leased jobs hold
manifests, so a global pause evicts at most pool-A-many.

**Not releasing the lease on pause is a deadlock, not an optimisation.** With
pool A = 3 and three paused jobs, no job could ever fetch again.

### 3.9 The lease has one releaser and one reclaimer

Revision 3 specified two lease lifetimes in a two-row table and leaked on five
separate paths (§10). The rule that closes them:

> **Every door that can end the job's need for a lease yields it, and the Queue
> reclaims through one call that no-ops on nil.**

```go
func (j *Job) surrenderLocked() *Lease                         // nil if none held — SOLE releaser; caller holds j.mu
func (j *Job) Surrender() *Lease                               // takes j.mu, then surrenderLocked
func (j *Job) Cross(to State) (*Lease, error)                  // holds j.mu; calls surrenderLocked
func (j *Job) Finish(o Outcome, now time.Time) (*Lease, error) // holds j.mu; calls surrenderLocked

func (q *Queue) reclaim(l *Lease)                              // no-op on nil — SOLE reclaimer
```

**`Cross` and `Finish` must call `surrenderLocked`, not `Surrender`.** `Job.mu`
is a `sync.RWMutex` and Go's mutexes are not reentrant. Both doors run their
bodies inside `withOpenAttempt`'s callback, which holds `j.mu` across the
attempt mutation (`internal/job/job.go`) — so either one calling the exported
`Surrender()` would take `j.mu` a second time and **deadlock the job
permanently**, with no error and no timeout.

> **Corrected twice during implementation (Half A); this is the current
> statement.** The paragraph first derived the hazard from `Job.Finish`
> routing through `withOpenAttempt`, which at the time it did not — both doors
> locked inline, because the helper's callback returns only `error` while they
> must return a `*Lease`. That inline duplication was then removed: a
> `withOpenAttemptLease` adapter over the same helper changes the shape of the
> callback's result without taking a lock or repeating a check, so **all five
> attempt-mutating doors now share one open-attempt check** — `Transition`,
> `SetNext`, `SetActivity`, `Cross`, `Finish`. Dropping `!a.isOpen()` from that
> single helper fails all five subtests of
> `TestJob_FinishedJobHasNoOpenAttempt`, which is the property made
> observable rather than asserted.
>
> The deadlock conclusion is unchanged throughout: what matters is that `j.mu`
> is held when the door's body runs, not which frame took it. The exported
> `Surrender` remains outside this set and structurally cannot join it — the
> helper refuses a job with no open attempt, which is precisely the caller
> `Surrender` exists to serve. It takes `j.mu` itself and calls
> `surrenderLocked`.

The single-releaser property is unaffected: `surrenderLocked` is still the only
code that clears `j.lease`, and `Surrender` is a thin lock-taking wrapper over
it for the pause path, which is not already holding `j.mu`.

Every exit, and why each one needs to be there:

| Exit | Yields | Revision 3's behaviour |
|---|---|---|
| Cross into Production | `Cross` | correct |
| Settle after a pre-boundary failure | `Finish` | **leaked** — `Finish` returned no lease and `advance` skips settled attempts, so every failed download lost a pool-A slot until restart |
| Settle with `Unrecoverable` from `Assessing` | `Finish` | **leaked**, same cause |
| Cancel | `Finish` | **leaked**, same cause |
| Pause | `Surrender` | correct |
| Cross a job that was paused before crossing | `Cross` → `Surrender` → `nil` | **`q.poolA.put(nil)`** — the pause had already surrendered it |

That last row is why `Surrender` returns `nil` rather than asserting, and why
`reclaim` no-ops on `nil`. A job may legitimately reach the crossing holding no
lease: it was paused at `Assessing{next: Extracting}`, surrendered, and resumed.
It does not need one to cross, because crossing is where it would have given the
lease up anyway.

**`reclaim` no-ops on nil rather than the callers checking.** Every exit in the
table above is a call site, and each one testing for nil itself is another
chance to forget; one function that accepts nil is none. The count is
deliberately not stated here — it is a property of Half B's code, which does not
exist yet, and a number written now would be stale before it was true.

---

---

## 4. Consequences

### 4.1 `legalEdges` narrows to the work spine

Removing `Waiting` removes its six outgoing and five incoming edges. The
`→ Finished` edges then have no consumer: `git grep -n 'CanTransition(' --
'internal/*.go' | grep -v _test.go` returns two live call sites, both
in `attempt.go` — `transition` (197) and `hold` (266); every other hit is a
comment. `hold` is deleted here, and `transition` refuses `Finished` outright
before consulting the map. `finish` never consults it at all — verified by
`sed -n '/func (a \*Attempt) finish/,/^}/p' internal/job/attempt.go | grep
CanTransition`, which returns nothing.

Both commands above were re-run against this commit before being written here.
An earlier revision cited the first as `git grep -n 'CanTransition(' internal/
--include='*.go'`, which **does not run** — `git grep` takes pathspecs, not
`--include`, and errors with *"option '--include=*.go' must come before
non-option arguments"*. The `grep -rn` form was what had actually been executed,
and transcribing it as `git grep` produced a citation that fails for anyone who
tries it. Standing Design Rule 4's point exactly: a citation exists so the
reader need not go and look, which makes a broken one worse than none.

```
Fetching   → Assessing
Assessing  → Fetching, Repairing, Extracting
Repairing  → Assessing
Extracting → Finalizing
```

Six edges, down from 22. Exactly one is a Correctness→Production edge —
`Assessing → Extracting` — which is why `Cross` owns one edge rather than a
state class.

**`CanTransition`'s `from == to` early return is removed.** It partly existed to
keep `hold`'s `Finalizing` case reachable, and `hold` is deleted; no door
requests a self-transition. Leaving it would permit a legal no-op that clears
`Activity` and nothing else.

### 4.2 What deletes

| Deleted | Why |
|---|---|
| `Waiting` (State) | running-ness is derived from what the job holds |
| `Attempt.reason` | reason is derived |
| `Job.pending` | a never-run job has no attempt; a never-run paused job is `IntentPause` |
| `hold`, `Job.Hold` | nothing to park into |
| `SetWaitReason`, `setReason`, `ErrNotWaiting` | no stored reason to update |
| `ErrHoldRequired` | `transition` has no `Waiting` to refuse |
| `CanTransition`'s `from == to` | no caller requests a self-transition |

`IsCorrectness` is **retained** despite having no non-test caller today
(`git grep -n 'IsCorrectness' internal/job/*.go` returns the definition and
comments only). It gains one in `advance`'s branch 3, and
`TestBoundaryIsUnreachableByAnyPath` uses it as an oracle.

### 4.3 What this does to the five defects

- **1** — `transition(Waiting)` cannot be written. Unrepresentable.
- **2** — no `Waiting` node to launder through; from `Extracting`, `legalEdges`
  permits only `Finalizing`. Unrepresentable.
- **3** — **guarded, not unrepresentable.** Deleting `hold` removes one door for
  re-declaring a destination; `SetNext` is a new one, closed by §3.3's
  write-once-per-visit rule. That is a guard, and it needs a mutation-verified
  test.
- **5** — repaired at its root, twice over. A pause no longer overwrites
  `state`, and `Cross` makes it impossible to enter Production without
  surrendering the lease in the same call.
- **4** — untouched. `crossed`, `ErrBoundaryConsumed` and
  `TestBoundaryIsUnreachableByAnyPath` all remain load-bearing; `finish` still
  erases `state`, so a settled attempt's crossing is recoverable only from the
  latch.

**This design does not retire the boundary machinery.**

### 4.4 `ToSABnzbd`

It currently reads `v.State == Waiting` and `v.Reason.IsPause()`; neither
survives. It takes the Queue-composed view, keyed on **running-ness and intent
first**, because a running job's intent must not change its status:

| Composed view | Status |
|---|---|
| *settled* (`Outcome.IsSettled()`, any position) | per `finishedStatus(Outcome)`, unchanged — **corrected by change 03**: keyed on the Outcome axis, not on a `Finished` state |
| running | its state's status, per the rows below |
| not running, `waitReason` satisfies `IsPause()` | `Paused` |
| not running, otherwise (incl. `StateUnset`) | `Queued` |

**The `Paused` row keys on the wait reason, not on `Intent`.** An earlier draft
keyed it on `IntentPause` alone, which silently dropped queue-wide pause: under
a global pause each job still carries `IntentRun`, so every one of them would
have matched the `Queued` row. `waitReason` returns `UserPaused` or
`GlobalPause` from `gatedBy`, and `WaitReason.IsPause()` already covers both —
so routing through it costs nothing and cannot omit one. This is a live API
contract, not a hypothetical: `TestToSABnzbd_GlobalPauseRendersAsPaused`
(`internal/job/sabnzbd_test.go:190`) asserts `StatusPaused` for exactly that
case. It replaced `TestJob_PausedRendersAsStatusPaused`, which Half A deleted
along with the state that made a separate job-level pause test meaningful; the
old citation named a test that no longer exists and a line past the end of
`job_test.go`.

Ordering matters too: a never-run job that is paused must match the `Paused`
row, not the `StateUnset` catch-all. Revision 2 listed `StateUnset → Queued`
first and rendered such a job `Queued`.

Rows for a running job are unchanged from today: `Fetching` + `Assessed` →
`Fetching`, else `Downloading`; `Assessing` + `ActCRCCheck` → `QuickCheck`, else
`Verifying`; `Repairing` → `Repairing`; `Extracting` → `Extracting`;
`Finalizing` + `ActScript` → `Running`, else `Moving`.

**A running job with `IntentPause` renders as its current state** — it is still
repairing. That is §1.1's requirement. Surfacing "finishing repair, then
pausing" is the UI reading `Intent` alongside the status.

The four never-produced statuses stay never-produced.

### 4.5 Ownership

| State | Owner | Written by |
|---|---|---|
| `Intent` | `Job` | `SetIntent` |
| `state` | `Attempt` | `transition`, `Cross`, `finish` |
| `next` | `Attempt` | `SetNext`; cleared by `transition`, `Cross` and `finish` |
| `crossed` | `Attempt` | `Cross` — sole writer |
| `outcome` | `Attempt` | `finish` — sole writer |
| `attempts` | `Job` | `BeginAttempt` — sole writer |
| the `Lease` | `Job` | `Grant` acquires; `surrenderLocked` releases — sole releaser, called by `Cross`, `Finish`, pause, and the exported `Surrender` wrapper |
| running-ness, wait reason | derived | nobody |

### 4.6 Resource lifetimes

| | Acquired | Released |
|---|---|---|
| Lease (pool A) | `grantFor`, once per attempt | `surrenderLocked` — via `Cross`, via `Finish`, via the exported `Surrender`, or directly on pause. See §3.9 |
| Compute slot (pool B) | `grantFor`, per state | by `releaseFor`, when the job moves to a position `needsSlot` does not name — **not** simply when the current state's work completes: `Assessing → Repairing` and `Extracting → Finalizing` both keep the slot, because the destination needs one too (§5.3, §5.5) |

Pool A is reserved across the correctness loop and **not** released between
`Fetching`, `Assessing` and `Repairing`, per §8.1. Pool B is per-state.

### 4.7 Reorder

Prior spec §8.1.1's table keys its first row on `Waiting{Next: Fetching}`, which
no longer exists. It is superseded by:

| Job's state when reordered | When the new position takes effect |
|---|---|
| `StateUnset`, or `Fetching` with work unfinished and holding nothing | at the next lease issuance |
| `Fetching`, running | immediately — it changes dispatch precedence |
| `Assessing`, `Repairing`, holding a lease | on re-entering `Fetching`, if the verdict is `NeedsMore` |
| `Assessing`, `Repairing`, holding nothing (restored, or paused) | at the next lease issuance — it must reacquire before it can do anything |
| `Extracting`, `Finalizing`, `Finished` | never — the job will not fetch again |

Reorder remains total and unconditionally recorded, for the reason §8.1.1 gives.

### 4.8 Sequencing

There is no replacement `Queue` yet — `git grep -n 'type[ ]Queue[ ]struct'
internal/queue` finds `queue.go:74`, the *existing* queue the swap plan
replaces. So this lands in two halves.

| Half | Contains | Lands |
|---|---|---|
| **A — `internal/job`** | remove `Waiting`; add `StateUnset`; add `Intent`; `next` as completion marker with its three rules; `Cross`; `Finish` and `Surrender` yielding the lease; delete the §4.2 list; narrow `legalEdges`; rewrite affected tests | its own plan, next |
| **B — `Queue`** | `gatedBy`, `waitReason`, `grantFor`, `advance`, `Cancel`, the composed view, `ToSABnzbd`'s inputs, the pools | amendment to the swap plan's item 3 |

Half A depends on `Lease` existing as a **type**, which `Cross`, `Finish` and
`Surrender` name in their signatures (`BeginAttempt` does not — D-I12). That is
smaller than the swap plan's item 1, which also moves `Manifest` and wires the
barrier, but it is not nothing: Half A's
plan must either define `Lease` or take an opaque handle. **Decide that in Half
A's plan**, and prefer defining the type — §6's argument is that the lease,
manifest and barrier are one object, and an opaque placeholder would be a second
representation of the same thing.

Half A still lands a package that cannot answer "is this job running", because
nothing grants leases yet. That is accepted: nothing imports `internal/job` —
`git grep -ln 'gonzbd/internal/job' -- '*.go' | grep -v '^internal/job/'`
returns nothing, run against this commit — so the gap is unobservable, and Rule
1 says land the end state rather than defend an intermediate. **Half A must land
before Half B.**

`ToSABnzbd` cannot compute its new inputs in Half A. Half A's plan decides
whether it takes a view type B later fills, or is removed and reintroduced in B.

---

## 5. Scenarios

These traces are the design's validation, and each corresponds to a failure
found in an earlier revision. Columns are `state`, `next`, `intent`, what the
job holds, and the rendered status.

### 5.1 Pause mid-download, then resume

```
Fetching   —           Run    lease    running   → "Downloading"
  user pauses                                      intent=Pause
  downloader yields between articles; Queue surrenders the lease
Fetching   —           Pause  —        not run.  → "Paused"
  user resumes                                     intent=Run
  advance branch 2: next unset, holds nothing → grantFor(Fetching)
Fetching   —           Run    lease    running   → "Downloading"
```

Never touches `Assessing`. Revision 2 jumped a partially-downloaded job straight
into verification here.

### 5.2 Pause at a boundary

```
Fetching   —           Run    lease              → "Downloading"
  download completes → SetNext(Assessing)
Fetching   Assessing   Run    lease    workDone  → ready
  user pauses; advance branch 3 gated            → "Paused"
  user resumes; grantFor(Assessing); Transition clears next
Assessing  —           Run    lease+slot         → "Verifying"
```

### 5.3 Repair loop and the crossing

```
Assessing  —           Run    lease+slot         → "Verifying"
  verdict NeedsRepair → SetNext(Repairing)
Assessing  Repairing   Run    lease+slot         → ready
  grantFor(Repairing); Transition clears next
Repairing  —           Run    lease+slot         → "Repairing"
  repair done → SetNext(Assessing)
  ... → Assessing → verdict OK → SetNext(Extracting)
Assessing  Extracting  Run    lease+slot         → ready
  advance branch 3: IsCorrectness && IsProduction
  Cross(Extracting) → state, crossed, next cleared, lease yielded — ONE call
  q.reclaim(lease); grantFor(Extracting) — no-op, the slot was already held
Extracting —           Run    slot               → "Extracting"
```

**The slot is held continuously from the first `Verifying` line to the last —
neither the repair verdict nor the repair's own completion releases it.** An
earlier revision of this trace said "slot released" at both points; it is
wrong, and two independent reviews confirmed the code is right and the trace
was not. The mechanism: `needsSlot` (`internal/sched/requirements.go`) makes
`Assessing`, `Repairing`, `Extracting` and `Finalizing` all slot-needing, and
`releaseFor` (`internal/sched/queue.go`) frees a slot only for a destination
that does **not** need one. `Assessing → Repairing` is a same-zone move, not a
demotion — the one demotion in the work spine is `Assessing → Fetching`
(§3.6) — so `releaseFor(Repairing)` no-ops, and the same holds in reverse for
`Repairing → Assessing`.

### 5.4 Restart, both variants

```
persisted: Extracting  —            crossed=true   holds nothing
  branch 2 → grantFor(Extracting) → extraction RUNS

persisted: Extracting  Finalizing   crossed=true   holds nothing
  branch 3 → grantFor(Finalizing); Transition → does NOT re-extract
```

The completion marker survives the crash and distinguishes the two.

### 5.5 Cancel, post-boundary

```
Extracting  —            Run     slot
  Cancel → intent=Cancel; IsProduction && running(j) → gate, return nil
  unpacker finishes → SetNext(Finalizing)
Extracting  Finalizing   Cancel   slot            running(j) == false
  advance → finishCancel: not running → Finish(Cancelled), releaseFor(StateUnset)
                                                             → "Deleted"
```

`running(j)` goes false once the unpacker finishes because `next` is set —
that alone is sufficient, and it is the ONLY thing that changes at that line.
**The slot is not released there.** An earlier revision of this trace said it
was, and had the job "holding nothing" one step early; two independent
reviews confirmed the code is right and the trace was not. `needsSlot`
(`internal/sched/requirements.go`) makes `Finalizing` slot-needing exactly
like `Extracting`, so `releaseFor` (`internal/sched/queue.go`) keeps the slot
across this same-zone move precisely as it does for `Assessing → Repairing`
in §5.3. The slot is freed one step later, inside `finishCancel` — right
after its `Finish(Cancelled)` call, by a separate `releaseFor(j, StateUnset)`
statement that is not part of `Finish` itself (`Finish` yields only the
lease, via `surrenderLocked`). `StateUnset` is the first destination in this
trace that needs nothing.

The gate opens because work is no longer in flight, which is the question §8.4
poses. Revision 2 deadlocked here and would then have recorded `OutcomeOK`; an
earlier draft of this trace still described the gate as `!workDone`, which
`Finalizing` cannot express.

### 5.6 Contention at a boundary

```
Fetching   Assessing   Run    lease    pool B full
  branch 3: grantFor fails → no move; lease retained (§8.1)  → "Queued"
```

### 5.7 Pause during `Finalizing`

```
Finalizing  —   Run    slot     → "Moving"
  user pauses: §8.3 gates per-state, so the state runs to completion
  Finish(OutcomeOK)
Finalizing  —   Run    slot            — Finish yields only the lease (there is
                                          none here); the slot Finalizing held
                                          is not released yet
  advance → settled branch: releaseFor(StateUnset)   → releases the slot
Finalizing  —   Run    —               → "Completed"
```

There is no such thing as a paused `Finalizing` job that must resume: pause can
only hold a job *before* `Finalizing` starts. Revision 2 had this stuck forever.
The trailing `advance` step is the same sweep §5.5 needed: `Finish` yields the
lease only, via `surrenderLocked`, and the compute slot survives until the
settled branch's `releaseFor` runs on a later tick — see §5.5's own note for
the mechanism.

### 5.8 A pre-boundary failure returns the lease

```
Fetching    —   Run   lease            → "Downloading"
  every server exhausted for some articles; fetch STOPS, not completes
  SetNext(Assessing)                     — §5 says only Assessing may decide
Fetching    Assessing  Run  lease
  ... Assessing verdict Unrecoverable
  l, _ := Finish(OutcomeUnrecoverable); q.reclaim(l)   ← lease returned
Assessing   —   Run   slot             — Assessing's slot is not released yet
  advance → settled branch: releaseFor(StateUnset)   → releases the slot
Assessing   —   Run   —                → "Failed"
```

Revision 3 leaked the lease here on every failed download. The slot is a
separate story, on the same schedule as §5.5 and §5.7: `Finish` returns only
the lease, so the compute slot Assessing held survives until the settled
branch's `releaseFor` runs on the next `advance` tick.

### 5.9 Retry when pool A is exhausted

```
Fetching  —  Run  —   Outcome=Failed          → "Failed"
  user retries → q.Retry(j) → BeginAttempt(now)          (no lease needed)
Fetching  —  Run  —   holds nothing           → "Queued"
  ... capacity frees → branch 2 → grantFor(Fetching)
Fetching  —  Run  lease                        → "Downloading"
```

Revision 3 dropped this retry permanently: `BeginAttempt` demanded a lease that
could not be taken, and nothing could record that a retry was wanted.

### 5.10 Paused, then failed, then retried

```
Fetching  —  Pause  —          → "Paused"
  ... a prior attempt settles Failed
Fetching  —  Pause  —          → "Failed"
  user unpauses: SetIntent(IntentRun)   — legal on a settled attempt (§3.1)
  user retries  → q.Retry(j) → BeginAttempt; branch 2 grants
Fetching  —  Run    lease      → "Downloading"
```

Revision 3 refused `SetIntent` on a settled attempt, so this job could be
neither unpaused nor usefully retried.

### 5.11 Cancelling a never-run job

```
StateUnset  —  Run  —          → "Queued"
  Cancel → SetIntent(IntentCancel); finishCancel sees StateUnset → q.discard(j)
```

No attempt exists, so there is nothing to carry `OutcomeCancelled`. Revision 3
called `Finish` here and got `ErrNoOpenAttempt`.

### 5.12 Cancelling a post-boundary job restored from a restart

```
persisted: Extracting  —  crossed=true   holds nothing
  Cancel → IsProduction, but running(j) is FALSE (holds no slot)
        → Finish(OutcomeCancelled); reclaim(nil)          → "Deleted"
```

Revision 3 gated on `!workDone` and deadlocked: `advance` handles cancel before
granting, so the extraction that would have set `next` never ran.

### 5.13 Cancelling a running `Finalizing` job

```
Finalizing  —  Run     slot       → "Moving"
  Cancel → IsProduction && running → gate
  the move and the user script complete; worker calls Finish(OutcomeOK)
Finalizing  —  Cancel  slot            — Finalizing's slot is not released yet
  advance → Intent still Cancel → finishCancel's settled arm: releaseFor(StateUnset)
Finalizing  —  Cancel  —          → "Completed", Intent still IntentCancel
```

The cancel arrived after the last gate. §8.4 degrades post-boundary cancel to a
gate, and there is no gate after `Finalizing`. Recording `Cancelled` would be
false — the files moved and the script ran — so the outcome is honest and the
surviving `IntentCancel` is what the UI reads to say the request came too late
(D-I11).

The trailing `advance` step matters here for a reason none of the other traces
share: `Intent` is still `IntentCancel` after the worker's own `Finish`, so
every later `advance` call keeps routing to `finishCancel` rather than
`advance`'s own settled branch — and, before this branch's Critical 1 fix,
`finishCancel`'s settled arm returned without releasing anything, which
stranded this exact job's slot permanently. `finishCancel`'s settled arm now
releases it, on the same schedule the other post-`Finish` traces show.

---

## 6. Testing

1. **`waitReason` and `gatedBy` are pure.** Call both across the product space
   with both pools exhausted and assert occupancy is unchanged. Acquisition
   leaking into the render path is the failure mode that would not show up as a
   wrong answer.
2. **Gate precedence is total.** Walk `Intent × globalPause × holdsLease ×
   holdsSlot × State` and assert `waitReason` matches §3.4 at every point.
3. **`advance`'s three branches**, one test each, and one asserting a settled
   attempt is never reopened.
4. **Each scenario in §5 is a test.** They are the regression suite for four
   revisions of defects. 5.1, 5.4, 5.5 and 5.7 pin revision 2's; 5.8 through
   5.13 pin revision 3's.
4b. **No path settles or crosses without reclaiming the lease.** Walk every
   exit in §3.9's table and assert pool-A occupancy returns to its starting
   value. This is the cluster that revision 3 leaked on five separate paths, and
   the only test that would have caught all five at once.
5. **`SetNext`'s three rules**, one test each. Write-once-per-visit is defect
   3's pin and must be mutation-verified.
6. **`Cross` is atomic.** Assert it is impossible to reach a Production state
   with a lease still held, by enumeration over the doors — and that
   `transition` refuses the `Assessing → Extracting` edge. Note what this does
   NOT prove: a caller may still drop the `*Lease` `Cross` returns (§3.5), and
   no test in this package can see that.
7. **`TestBoundaryIsUnreachableByAnyPath` is rewritten, and needs a NEW
   ORACLE.** Action set changes (`Hold` out; `SetIntent`, `SetNext`, `Cross`
   in); config key gains `Intent` and `next`; must stay mutation-verified —
   reverting `BeginAttempt`'s `crossed` refusal must still turn it red.

   The oracle change is not cosmetic. That test currently judges with
   `IsCorrectness`, and its own doc comment says why that was legitimate and
   states the condition under which it stops being so:

   > *"if a door ever starts branching on `IsCorrectness`, this test needs a
   > different oracle."*

   **This design creates that condition.** `transition` refuses
   `IsCorrectness(from) && IsProduction(to)` (§3.5) and `advance` branch 3
   selects the crossing with it (§3.6). Judging those doors with the predicate
   they decide by means a wrong predicate would agree with itself — the exact
   failure the comment was written to prevent.

   The replacement must share no code with the doors: enumerate the Correctness
   states as a **literal set** in the test (`Fetching`, `Assessing`,
   `Repairing`), with a guard asserting `AllStates()` has not grown members the
   literal does not classify. A literal cannot drift silently the way a shared
   predicate can, and the guard is what makes adding a state fail loudly here
   rather than quietly widening the oracle.
8. **Writer enumerations carry forward and extend.** `outcome` and `attempts`
   tests stay. Add: `SetIntent` sole writer of `Intent`; `Cross` sole writer of
   `crossed`; `SetNext`/`transition`/`Cross` the only writers of `next`.
9. **The zero value is loud.** `StateView{}.State == StateUnset`, and
   `ToSABnzbd` maps it to `Queued`, not to a download.
10. **`ToSABnzbd` product-space tests** gain running-ness, `Intent` **and
    `WaitReason`** axes. The `WaitReason` axis is not optional: §4.4's `Paused`
    row keys on `IsPause()` precisely so queue-wide pause is covered, and a
    product space walking only `Intent` would pass while `GlobalPause` rendered
    `Queued` — the regression that reached revision 5. Pin both a never-run
    paused job and a globally-paused running-eligible one.

---

## 7. Deliberately not built

- **A generic "gate" abstraction.** `Intent` has two non-default values and one
  consumer.
- **A separate `workComplete` flag.** Rejected in §3.3 — it moves the
  `Finalizing` special case rather than removing it.
- **Per-intent timestamps.** A history question, owned by the Checkpointer.
- **`Intent` on the `Attempt`.** Rejected in §3.1.
- **Interruptible post-boundary work.** §8.3's argument stands unchanged.
- **Crash consistency for a `Finalizing` that completed but was not settled.**
  Pre-existing and owned by `docs/durability-contract.md`.

---

## 8. Open questions

1. **~~Can a `Finished` job be told retryable from outside?~~ Dissolved.** The
   question assumed `finish` erases `state`, which change 03 corrected (§3.2's
   scoping note): `finish` no longer touches `state`, there is no `Finished`
   value, and a settled attempt keeps the position it settled at. A caller
   asking whether a settled job already crossed reads
   `IsProduction(StateView.State)` directly — `StateView.State` already
   answers it, and there is no `Crossed` field to add to it. Recorded as
   dissolved rather than deleted: the question was real under the model that
   predated change 03, and a reader tracing why it is gone should find the
   reason here rather than a silent deletion.
2. **~~Where do the per-state resource requirements live?~~ Answered:
   `internal/job` was the candidate this question weighed against the actual
   choice, and the actual choice is `internal/sched/requirements.go`.** The
   Half B1 plan's own decision D1 — that `internal/sched`, not `internal/job`
   or `internal/queue`, is the right home for the Queue's decision core —
   settles the package question this open question was really asking; that
   package's `needsLease`/`needsSlot` restate §3.4's table in code, with the
   Queue as their only consumer. A copy living in `internal/job` with no
   in-package caller would be a second place to maintain one fact (Standing
   Design Rule 2) — the reasoning `internal/sched/requirements.go`'s own doc
   comment states beside the table itself.
3. **Does the dispatcher need `next`?** Prior spec §9 is unread against this
   change. To be checked when Half B is written, not assumed here.
4. **Does a paused job keep its queue position?** Reorder (§4.7) says a paused
   never-started job takes effect at the next lease issuance, but the design
   does not say whether pausing itself changes position. Probably not; confirm
   in Half B.

---

## 9. Decisions

| | Decision |
|---|---|
| **D-I1** | `Intent` is a fourth orthogonal axis, on the `Job`, with `IntentCancel` latching. |
| **D-I2** | `Waiting` is removed; `StateUnset` becomes an invalid zero. |
| **D-I3** | `next` is set ⟺ the current state's work has *ended* and the job continues to another work state; its value is where. Work that ends without continuing settles via `Finish`. Failure is not a separate contract, and `Finalizing` is not an exception. |
| **D-I4** | Running-ness is about the CURRENT state and derived from what the job holds; grantability is about the NEXT state. They are different predicates. |
| **D-I5** | `gatedBy` and `waitReason` are pure; acquisition happens only in `grantFor`; `advance` writes no job state on any blocked path. |
| **D-I6** | Gate precedence is pause > global pause > lease > compute slot; cancel is handled before the gate. |
| **D-I7** | `legalEdges` narrows to the six-edge work spine; `from == to` is removed; cancellation is not an edge. |
| **D-I8** | `Cross` is a fourth door owning the one boundary edge, sole writer of `crossed`, and the only way to surrender the lease at the crossing. |
| **D-I9** | Every persisted job restores to its persisted state holding nothing; `state`, `next`, `crossed`, `outcome`, `assessed` and `Intent` persist. Pause and restart are one path. |
| **D-I10** | Half A lands before Half B. |
| **D-I11** | Cancelling a *running* `Finalizing` job lets it complete as `OutcomeOK`. §8.4 degrades post-boundary cancel to a gate and there is no gate after `Finalizing`; the files have moved and the script has run, so `Cancelled` would be false. `Intent` survives on the finished job for the UI to read. |
| **D-I12** | `BeginAttempt` does **not** take a lease. `Fetching` holding nothing is a legal state, so requiring one to reach it contradicts the model. §6's rule binds the downloader, which cannot obtain a `Manifest` without one. |
| **D-I13** | Every door that can end the need for a lease yields it; `Surrender` is the sole releaser and `q.reclaim` the sole reclaimer, no-opping on nil. |
| **D-I14** | `SetIntent` is legal in every state, including on a settled attempt. Only the `IntentCancel` latch restricts transitions.

**Retry does not clear the latch, and that is deliberate.** A cancelled job
whose attempt is settled still carries `IntentCancel`, so `q.Retry(j)` would
open an attempt that `advance` cancels on its next tick. That reads like a trap
and is not one: cancel renders as `Deleted` (§4.4), and prior spec D8 makes a
full redo **a re-added NZB starting a new `Job`**, not a new attempt on a
cancelled one. Clearing the latch on retry would let a job the user deleted come
back through a path that never re-asked them.

Stated here because §3.1's stated rationale — *"cancel then unpause"* — does not
cover retry, and a reader could reasonably infer the latch was only about pause.
It is not: it is about cancel being final for this `Job`. |

---

## 10. Revision history

**Revision 6 → 7.** A second `deep-pr-review` pass against `b6217c51` returned
thirteen findings. Eleven accepted, one accepted in part, one rejected.

The two that mattered most were both invisible to a reader of the document
alone, and both were verified against source before being accepted:

| Finding | Evidence |
|---|---|
| **`Cross` and `Finish` calling the exported `Surrender` self-deadlocks.** Each takes `j.mu.Lock()` in its own body and holds it across the attempt mutation, and Go mutexes are not reentrant — the job would hang permanently, with no error and no timeout | `internal/job/job.go`; §3.9 now specifies `surrenderLocked`, and see the correction there on where the lock is taken |
| **§4.1's own verification command does not run.** It was written as `git grep -n 'CanTransition(' internal/ --include='*.go'`, which errors: *"option '--include=*.go' must come before non-option arguments"*. The `grep -rn` form had been executed and transcribed as `git grep` | re-run against this commit; §4.1 corrected and the failure recorded |

That second one is Rule 4's failure in its purest form. A citation exists so the
reader need not go and look; one that breaks when they do is worse than none,
and no gate in this repository can catch it — the command lives in prose.

Also accepted:

| Finding | Fix |
|---|---|
| A pre-boundary cancel settled and reclaimed the lease **while a worker was mid-article**, taking the `Manifest` out from under it | §3.7: neither arm settles while `running(j)`; the interrupt arm aborts the worker and settles on the following tick |
| Cancel released the lease but never the **compute slot**, leaking pool-B capacity on every cancel from `Assessing` or `Repairing` | §3.7 calls `releaseSlot` |
| `waitReason` tested the **current** state's requirements even when work had ended, reporting `NoComputeSlot` for `Assessing{next: Fetching}`, which needs only the lease it already holds | §3.4 tests `next`'s requirements once work has ended |
| `Cross` bypassed the `to == next` check `transition` enforces, leaving a hole in the single-decider property | §3.5: `Cross` validates `Assessing` and `to == a.next` |
| `assessed` was absent from the persisted set; losing it makes a resumed par2 fetch report `Downloading` instead of `Fetching` | §3.8 and D-I9 |
| D-I9 omitted `outcome`, which §3.8 had gained in revision 6 | corrected — another sweep miss of the same class |
| Scenario 5.3 called `poolA.put` directly, bypassing the sole reclaimer | uses `q.reclaim` |
| Test 10's product space walked `Intent` but not `WaitReason`, so it would have passed while `GlobalPause` rendered `Queued` | §6.10 adds the axis, with the reason |
| `park` returned an error that is always nil | returns nothing |

**Accepted in part:** that the `IntentCancel` latch "bricks retrying cancelled
jobs". The documentation gap is real and §3.1 now closes it. The proposed
behaviour change — clearing the latch on retry — is **rejected**: cancel renders
as `Deleted`, and prior spec D8 makes a full redo a re-added NZB starting a new
`Job`. Clearing it would let a deleted job return through a path that never
re-asked the user.

**Rejected:** that `advance` branch 1 eagerly opens an attempt on a queued job
and prevents `q.discard`. `advance` tests `IntentCancel` before branch 1, so
branch 1 cannot run on a cancelled job at all; a queued cancelled job reaches
`finishCancel`, matches `StateUnset`, and is discarded exactly as §3.7 and
scenario 5.11 specify.

**Revision 5 → 6.** CodeRabbit reviewed the pre-revision-5 commit and returned
six findings; two were already fixed by revision 5 and four were new. Three of
the four are contradictions this document introduced about itself.

| Finding | Fix |
|---|---|
| **§3.3's prose said `SetNext` is not called on failure; its own table then calls `SetNext(Assessing)` for a failed `Fetching`** | the rule is restated in two clauses — see below |
| §3.2 required `StateUnset` to be declared-but-unlisted *and* said not to relax `TestAllStates_Exhaustive`, which asserts equal lengths. Both cannot hold | §3.2 asserts the exception by name: `declared` = `AllStates()` ∪ `{StateUnset}`, exactly |
| The pause handoff that releases a lease was described in §3.8 and scenario 5.1 but had **no caller**; branch 2 short-circuits on `holds` before the gate | §3.6 specifies the `yielded` contract and gives the dispatcher the `park` call |
| §3.8 restores the persisted `state`, but §4.3 says `finish` "erases" it — leaving restart, rendering and retry able to disagree | §3.8 states persistence stores raw `Attempt` fields; `StateUnset` means *no attempt*, never a settled one |

The failure-contract finding is the one worth dwelling on. Revision 4 added the
failure paths and wrote a sentence flatly contradicting the table directly below
it, and three subsequent reviews did not catch it. The resolution is not a
patch: the rule was stated as a biconditional over *finishing*, and the accurate
statement is over **ending** —

> `next` is set ⟺ work has ended AND the job continues to another work state.

— which also dissolves the `Finalizing` "exception" that revisions 3 through 5
carried. `Finalizing` is not special; its work ends and it continues nowhere, so
it settles. Stating the rule in two clauses is what stopped it needing a
carve-out in `advance`, `finishCancel` and `waitReason`.

**Revision 4 → 5.** A fourth review — the first run through a review *skill*
(`deep-pr-review`) rather than a hand-written prompt — returned six findings,
all confirmed. Two were sweep failures from revision 4's own edits, which is the
failure mode `AGENTS.md` names directly: *"sweep against the diff the commit
will land as, not the diff that motivated the edit."*

| Finding | Fix |
|---|---|
| `waitReason` fell through to `NoComputeSlot` for both settled attempts and never-run jobs, since `needsLease` is false for `Finished` and `StateUnset` alike | §3.4 returns early for a settled attempt and reports `NoLease` for `StateUnset` |
| **`ToSABnzbd` ignored queue-wide pause entirely.** Under a global pause every job still carries `IntentRun`, so a table keyed on `IntentPause` rendered them all `Queued` | §4.4's `Paused` row keys on `waitReason` satisfying `IsPause()`, which covers both |
| **Neither gated path in `advance` surrendered the lease**, so a paused job held pool-A capacity forever — the deadlock §3.8 warns about, present in the same document's own pseudocode | §3.6 routes both through `park` |
| §5.5's trace still described the cancel gate as `!workDone` | rewritten to `running(j)` |
| §4.8 and §10 still carried `BeginAttempt(l, now)` after D-I12 reverted it | corrected, and §10 now records the revert beside the change |
| **The reachability test's oracle is no longer independent** | §6.7 specifies a literal-set replacement |

That last one is the sharpest finding any of the four rounds produced, and it
was caught by a comment rather than by a reviewer's insight.
`reachability_test.go` chose `IsCorrectness` as its oracle because no door
branched on it, and stated the condition under which that stops being true:
*"if a door ever starts branching on `IsCorrectness`, this test needs a
different oracle."* This design creates that condition — `transition` refuses
`IsCorrectness(from) && IsProduction(to)` and `advance` selects the crossing
with it. A comment written to survive its own obsolescence did exactly that.

**Revision 3 → 4.** A third adversarial review returned twelve findings. Unlike
the previous rounds they were concentrated rather than spread: the state machine
produced nothing new, and **five of the twelve were one problem** — nobody
returned the lease.

| Leak path | Cause |
|---|---|
| Pre-boundary failure → `Finish(Failed)` | `Finish` yielded no lease and `advance` skips settled attempts |
| `Assessing → Finish(Unrecoverable)` | same |
| Cancel → `Finish(Cancelled)` | same |
| `BeginAttempt` returning `ErrBoundaryConsumed`, or no-opping | the lease passed in was orphaned |
| Cross after a pause that already surrendered | `q.poolA.put(nil)` |

§3.9 is the answer, and D-I12 removed the fourth by removing the parameter.

Four findings were not about leases:

| Finding | Fix |
|---|---|
| The `Finalizing` exemption leaked into `finishCancel`: `Finalizing` never sets `next`, so an `IsProduction && !workDone` gate was permanently true there. A `Finalizing` job could never be cancelled, and a running one's `Finish(OutcomeOK)` silently discarded the cancel | §3.7 gates on `IsProduction && running` — the question §8.4 actually asks |
| Cancelling a never-run job called `Finish` and got `ErrNoOpenAttempt` | §3.7 discards it; there is no attempt to carry an `Outcome` |
| Cancelling a restored post-boundary job gated forever, since `advance` handles cancel before granting the slot whose work would set `next` | same `running` fix |
| `waitReason` reported `NoLease` for `Extracting`/`Finalizing` jobs waiting on a slot, which hold no lease by design | §3.4 guards on `needsLease(state)` |

Two claims were withdrawn rather than defended. **`Cross` does not make lease
leaks unrepresentable** — it makes them one call site; the caller can still drop
the returned `*Lease`, and a callback form that would close it is rejected on
lock-order grounds (§3.5). That is the third revision running in which
"unrepresentable" was claimed where "guarded" was earned. And **revision 3's
`BeginAttempt(l *Lease, now)` was wrong** (D-I12) — it contradicted the model
that makes a paused or restarted fetch expressible, and reverting it closed
three findings at once.

Independently of the review, §3.3 gained the failure paths it had never
specified — it described who writes `next` when a state *completes* and said
nothing about when one *fails* — and named the `Assessing → Repairing` loop as
the Assessor's to bound, not this machine's. §3.6 states its concurrency
contract.

**Revision 2 → 3.** A second adversarial review found revision 2 had a
data-losing flaw with three symptoms, all one root cause: removing `Waiting`
destroyed the distinction between *"at X, X's work is done"* and *"at X, still
doing X"*, and revision 2's `next` — confined to `Assessing` — could not carry
it.

| Symptom | Consequence |
|---|---|
| Unpausing a mid-download job | `nextMove(Fetching)` returned `Assessing`; a partially-downloaded job entered verification |
| Pausing a `Finalizing` job | `nextMove` returned nothing; the job could never reacquire a slot |
| Restarting mid-`Extracting` | restored to `Extracting`, moved to `Finalizing`; extraction never ran |

Revision 2's §3.3 derivation — "only `Assessing` needs `next`, because every
other state has one spine successor" — is where it went wrong. Only `Assessing`
needs the *value*. Every state needs the *presence*. §3.3 now says so.

Revision 1's review had raised the same seam (finding: "§3.3 and §3.4 disagreed
on when `next` is set"); revision 2 resolved that contradiction in the wrong
direction.

Also fixed in revision 3:

| Finding | Fix |
|---|---|
| `blockedBy` checked before `acquireFor`, so an ungranted job was "blocked" and never acquired — the normal path deadlocked | §3.4 splits running-ness from grantability; §3.6's `advance` acquires in branches 1–3 |
| `finishCancel`'s `IsProduction`-only guard hit at both call sites forever; a completing `Finalizing` would record `OutcomeOK` on a cancelled job | §3.7's guard is `IsProduction && !workDone` |
| Render table matched `StateUnset` before `IntentPause`, so a never-run paused job rendered `Queued` | §4.4 keys on running-ness and intent first |
| Prior spec §3.1, D1 and §8.1.1 were broken without being declared superseded | declared in the header; §4.7 supersedes the reorder table |
| `next`-clearing stated twice, redundantly and inconsistently | §3.3 rule 3 — but see the correction there: `finish` clears it as well, so the writers are `setNext`, `transition`, `Cross` and `finish` |
| `StateUnset` breaks `TestAllStates_Exhaustive`'s AST check | named in §3.2 with the required fix |

**Rejected.** The claim that removing the `→ Finished` edges breaks completion,
on the grounds that an `Unrecoverable` verdict needs `SetNext(Finished)`. It
does not: `Finish` is a separate door and the assessor enacts that verdict
through it. `finish` never consults `legalEdges` (§4.1). The secondary point —
that existing tests assert those edges — is true but expected; §6 scopes them
for rewrite.

**From tracing rather than review.** §5's scenarios were built to game the fix
out, and produced four changes no review had asked for: `Cross` as a fourth door
(§3.5), `BeginAttempt` taking the lease (§3.5 — **subsequently reverted in
revision 4, D-I12**), the pause/restart path equivalence (§3.8), and the
rejection of a separate `workComplete` flag (§3.3).

**Revision 1 → 2** is recorded in this file's git history.
