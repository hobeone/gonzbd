# Lifecycle Intents — Design

**Status:** proposed, revision 3. Revisions 1 and 2 were each reviewed
externally; §10 records what changed and why. Revision 2 was found to have a
data-losing flaw, described in §10.

**Amends** `docs/superpowers/specs/2026-08-25-job-lifecycle-design.md`:

| Section | Disposition |
|---|---|
| §8.2 (`Waiting` unifies pause and slot-waiting) | superseded — §3.2 |
| §10 (every persisted job restores to `Waiting`) | superseded — §3.8 |
| §3.1 / D1 (`State()` for a never-run job) | superseded — §3.2 |
| §8.1.1 (reorder table keys on `Waiting{Next: Fetching}`) | superseded — §4.7 |
| §6 (`BeginFetch(l *Lease)`) | amended — folded into `BeginAttempt`, §3.5 |
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

`SetIntent` is legal while an attempt is open or none has run, and refused once
the current attempt is settled: a settled job's next move is a decision for the
user, not an intent to record.

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
    Finished
)
```

`StateUnset` exists because removing `Waiting` would otherwise make `Fetching`
the zero value, so a zero `StateView` would read as an active download —
Rule 2's "a type with a valid zero used as a key" smell, which the old ordering
avoided by accident.

`AllStates()` returns the six real states. `Job.State()` returns
`StateView{State: StateUnset}` for a job with no attempt, **superseding prior
spec §3.1 and D1**, which specify `StateView{State: Waiting, Next: Fetching,
Reason: NoLease}`.

`TestAllStates_Exhaustive` parses `state.go` by AST (`stateConstantsFromSource`)
and asserts `len(AllStates()) == len(declared)`. It must be updated to expect
`StateUnset` as declared-but-unlisted, deliberately — not by relaxing the
check.

### 3.3 `next` marks completion, and carries the verdict

> **`next` is set ⟺ the current state's work is finished.**
> **Its value is where the job goes next.**

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

**`Finalizing` is the one exception, and deliberately so.** Its completion and
its settlement are the same act, so it calls `Finish` rather than `SetNext`. An
earlier draft added a separate `workComplete` flag to make the marker uniform;
it does not help, because `advance` would then need a special case for
"`Finalizing` + complete → `Finish(OK)`" — `next` cannot carry an `Outcome`. The
special case only moves. Keeping `Finish` atomic means the window between "work
done" and "recorded done" does not exist. A crash before that single call means
the work was never recorded, and restart re-runs `Finalizing` — the same
guarantee every other state has, and `docs/durability-contract.md`'s concern,
not this document's.

Three rules on `SetNext`:

1. **Guarded by `legalEdges`.** A value the current state could not reach
   directly cannot be recorded either.
2. **Write-once per visit.** `SetNext` refuses when `next` is already set to a
   *different* value, and is a no-op for the same one. Without this, a verdict
   of `Repairing` could be overwritten with `Extracting` and the job would cross
   the boundary skipping repair — defect 3's mechanism surviving into a new
   door.
3. **Cleared by the move.** `transition` and `Cross` each clear `next` as part
   of taking it. Nothing else clears it, so an attempt that never re-enters
   `Assessing` cannot carry a stale verdict.

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
| `Finished`, `StateUnset` | nothing |

```
holds(j)   ≡ j holds everything its CURRENT state requires
running(j) ≡ the attempt is open && holds(j) && next is unset
```

All three conjuncts are load-bearing. A job holding a slot whose work is
finished is not running, it is waiting to move — that is the `next` clause. And
the open-attempt clause is not redundant: `Finished` and `StateUnset` require
nothing, so `holds` is *vacuously true* for them and `next` is unset, and
without it a settled job would report as running. An earlier draft of this
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
    if q.running(j) { return 0, false }
    if r, gated := q.gatedBy(j); gated { return r, true }
    if !j.HoldsLease() { return job.NoLease, true }
    return job.NoComputeSlot, true
}

// grantFor acquires what s requires and j does not already hold. Acquisition
// happens ONLY here.
func (q *Queue) grantFor(j *job.Job, s job.State) bool
```

`IntentCancel` is absent from `gatedBy`: `advance` handles it first, so no
cancel value reaches the render path.

### 3.5 Four doors

Revision 2 had three. Tracing the crossing (§5, scenario 3) showed it needs its
own, for the same reason `finish` has one.

| Door | Produces | Sole writer of |
|---|---|---|
| `BeginAttempt(l *Lease, now)` | an open attempt at `Fetching` | `attempts` |
| `transition(to)` | ordinary spine moves | — |
| `Cross(to) (*Lease, error)` | the one boundary edge | `crossed` |
| `finish(o, now)` | `Finished` | `outcome` |

**`BeginAttempt` now takes the lease.** An attempt opens at `Fetching`,
`Fetching` requires a lease, so opening one without a lease should not compile.
This folds prior spec §6's `BeginFetch` in, turning its prose rule *"cannot be
called without one"* into a signature. `ErrBoundaryConsumed` still guards it.

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
func (j *Job) Cross(to State) (*Lease, error)
```

`Cross` does not release the lease itself — it calls `Surrender`, which stays
the **sole releaser**. Pause releases through the same call (§3.8). Two
independent paths nulling one handle would be two writers of the same field, and
the whole point of `Cross` is to remove a coordination, not add one.

`transition` correspondingly refuses `IsCorrectness(from) && IsProduction(to)`.
The design's central invariant gets a door proportionate to it.

### 3.6 `advance`

```go
func (q *Queue) advance(j *job.Job) error {
    if j.Intent() == job.IntentCancel {
        return q.finishCancel(j)                     // §3.7
    }
    v := j.State()

    // 1. Never run: start, if permitted. A SETTLED attempt is never reopened
    //    here — retry is an explicit user action, not something that resumes
    //    on its own.
    if v.State == job.StateUnset {
        if _, gated := q.gatedBy(j); gated { return nil }
        l, ok := q.takeLease(j)
        if !ok { return nil }
        return j.BeginAttempt(l, q.now())
    }
    if v.Outcome.IsSettled() { return nil }

    // 2. Current state's work is unfinished: make it runnable. This is the
    //    resume path AND the restart path — they are the same path.
    if v.Next == job.StateUnset {
        if q.holds(j) { return nil }                 // already working
        if _, gated := q.gatedBy(j); gated { return nil }
        q.grantFor(j, v.State)
        return nil
    }

    // 3. Work is finished: move.
    if _, gated := q.gatedBy(j); gated { return nil }
    if job.IsCorrectness(v.State) && job.IsProduction(v.Next) {
        l, err := j.Cross(v.Next)
        if err != nil { return err }
        q.poolA.put(l)
        q.grantFor(j, v.Next)
        return nil
    }
    if !q.grantFor(j, v.Next) { return nil }
    return j.Transition(v.Next)
}
```

`advance` writes no job state on any blocked path, so a lost acquisition race
costs a tick, never a verdict. **It takes no target** — the target is `next`,
written by the worker that finished the state.

Crossing before acquiring the slot is deliberate: the decision was already made
and recorded in `next`, crossing only *adds* pool-A capacity, and a job that
crosses and then fails to get a slot is simply not running until it does. It
cannot go back, and does not need to.

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
    if job.IsProduction(v.State) && v.Next == job.StateUnset {
        return nil    // gate: work still in flight, let it reach the boundary
    }
    return j.Finish(job.OutcomeCancelled, q.now())
}
```

The guard is `IsProduction && !workDone`, not `IsProduction` alone. Revision 2
used the latter, and both call sites then hit it forever: a post-boundary cancel
never completed, and a normally-completing `Finalizing` would record
`OutcomeOK` on a cancelled job (§10).

### 3.8 Restart, and why it is the pause path

This **supersedes prior spec §10's** *"every persisted job restores to
`Waiting`. There is no other legal option."* `Waiting` no longer exists.

The argument behind it is unchanged and is what the new rule preserves:
**nothing persists a lease.**

> **Every persisted job restores to the state it was persisted at, holding
> nothing.** `state`, `next`, `crossed` and `Intent` persist — all are
> decisions, none is a resource.

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

---

## 4. Consequences

### 4.1 `legalEdges` narrows to the work spine

Removing `Waiting` removes its six outgoing and five incoming edges. The
`→ Finished` edges then have no consumer: `git grep -n 'CanTransition('
internal/ --include='*.go' | grep -v _test.go` returns two live call sites, both
in `attempt.go` — `transition` (197) and `hold` (266); every other hit is a
comment. `hold` is deleted here, and `transition` refuses `Finished` outright
before consulting the map. `finish` never consults it at all — verified by
`sed -n '/func (a \*Attempt) finish/,/^}/p' internal/job/attempt.go | grep
CanTransition`, which returns nothing.

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
| `Finished` | per `finishedStatus(Outcome)`, unchanged |
| running | its state's status, per the rows below |
| not running, `IntentPause` | `Paused` |
| not running, otherwise (incl. `StateUnset`) | `Queued` |

Ordering matters: a never-run job with `IntentPause` must match the `Paused` row,
not a `StateUnset` row. Revision 2 listed `StateUnset → Queued` first and
rendered such a job `Queued`, failing `TestJob_PausedRendersAsStatusPaused`
(`job_test.go:415`).

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
| `next` | `Attempt` | `SetNext`; cleared by `transition` and `Cross` |
| `crossed` | `Attempt` | `Cross` — sole writer |
| `outcome` | `Attempt` | `finish` — sole writer |
| `attempts` | `Job` | `BeginAttempt` — sole writer |
| the `Lease` | `Job` | `BeginAttempt` acquires; `Surrender` releases — sole releaser, called by `Cross` and by pause |
| running-ness, wait reason | derived | nobody |

### 4.6 Resource lifetimes

| | Acquired | Released |
|---|---|---|
| Lease (pool A) | `BeginAttempt` | `Surrender` — via `Cross` at the crossing, directly on pause or settle |
| Compute slot (pool B) | `grantFor`, per state | when that state's work completes |

Pool A is reserved across the correctness loop and **not** released between
`Fetching`, `Assessing` and `Repairing`, per §8.1. Pool B is per-state.

### 4.7 Reorder

Prior spec §8.1.1's table keys its first row on `Waiting{Next: Fetching}`, which
no longer exists. It is superseded by:

| Job's state when reordered | When the new position takes effect |
|---|---|
| `StateUnset`, or `Fetching` with work unfinished and holding nothing | at the next lease issuance |
| `Fetching`, running | immediately — it changes dispatch precedence |
| `Assessing`, `Repairing` | on re-entering `Fetching`, if the verdict is `NeedsMore` |
| `Extracting`, `Finalizing`, `Finished` | never — the job will not fetch again |

Reorder remains total and unconditionally recorded, for the reason §8.1.1 gives.

### 4.8 Sequencing

There is no replacement `Queue` yet — `git grep -n 'type[ ]Queue[ ]struct'
internal/queue` finds `queue.go:74`, the *existing* queue the swap plan
replaces. So this lands in two halves.

| Half | Contains | Lands |
|---|---|---|
| **A — `internal/job`** | remove `Waiting`; add `StateUnset`; add `Intent`; `next` as completion marker with its three rules; `Cross`; `BeginAttempt(l, now)`; delete the §4.2 list; narrow `legalEdges`; rewrite affected tests | its own plan, next |
| **B — `Queue`** | `gatedBy`, `waitReason`, `grantFor`, `advance`, `Cancel`, the composed view, `ToSABnzbd`'s inputs, the pools | amendment to the swap plan's item 3 |

Half A depends on `Lease` existing as a **type**, which `Cross` and
`BeginAttempt` both name. That is smaller than the swap plan's item 1 (which
also moves `Manifest` and wires the barrier), but it is not nothing: Half A's
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
  verdict NeedsRepair → SetNext(Repairing); slot released
Assessing  Repairing   Run    lease              → ready
  grantFor(Repairing); Transition clears next
Repairing  —           Run    lease+slot         → "Repairing"
  repair done → SetNext(Assessing); slot released
  ... → Assessing → verdict OK → SetNext(Extracting)
Assessing  Extracting  Run    lease              → ready
  advance branch 3: IsCorrectness && IsProduction
  Cross(Extracting) → state, crossed, next cleared, lease yielded — ONE call
  poolA.put(lease); grantFor(Extracting)
Extracting —           Run    slot               → "Extracting"
```

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
  Cancel → intent=Cancel; IsProduction && !workDone → gate, return nil
  unpacker finishes → SetNext(Finalizing); slot released
Extracting  Finalizing   Cancel                    workDone
  advance → finishCancel: workDone → Finish(Cancelled)     → "Deleted"
```

Revision 2 deadlocked here and would then have recorded `OutcomeOK`.

### 5.6 Contention at a boundary

```
Fetching   Assessing   Run    lease    pool B full
  branch 3: grantFor fails → no move; lease retained (§8.1)  → "Queued"
```

### 5.7 Pause during `Finalizing`

```
Finalizing  —   Run    slot     → "Moving"
  user pauses: §8.3 gates per-state, so the state runs to completion
  Finish(OutcomeOK)                                      → "Completed"
```

There is no such thing as a paused `Finalizing` job that must resume: pause can
only hold a job *before* `Finalizing` starts. Revision 2 had this stuck forever.

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
4. **Each scenario in §5 is a test.** They are the regression suite for three
   revisions of defects; 5.1, 5.4, 5.5 and 5.7 each pin a data-losing or
   deadlocking bug from revision 2.
5. **`SetNext`'s three rules**, one test each. Write-once-per-visit is defect
   3's pin and must be mutation-verified.
6. **`Cross` is atomic.** Assert it is impossible to reach a Production state
   with a lease still held, by enumeration over the doors — and that
   `transition` refuses the `Assessing → Extracting` edge.
7. **`TestBoundaryIsUnreachableByAnyPath` is rewritten, not deleted.** Action
   set changes (`Hold` out; `SetIntent`, `SetNext`, `Cross` in); config key
   gains `Intent` and `next`. Must stay mutation-verified — reverting
   `BeginAttempt`'s `crossed` refusal must still turn it red.
8. **Writer enumerations carry forward and extend.** `outcome` and `attempts`
   tests stay. Add: `SetIntent` sole writer of `Intent`; `Cross` sole writer of
   `crossed`; `SetNext`/`transition`/`Cross` the only writers of `next`.
9. **The zero value is loud.** `StateView{}.State == StateUnset`, and
   `ToSABnzbd` maps it to `Queued`, not to a download.
10. **`ToSABnzbd` product-space tests** gain running-ness and `Intent` axes,
    with a case pinning that a never-run paused job renders `Paused`.

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

1. **Can a `Finished` job be told retryable from outside?** `finish` erases
   `state`, so a `Finished(Failed)` job's crossing is only in the `crossed`
   latch, which is not on `StateView`. Rendering moves to the Queue in Half B;
   decide there whether `Crossed` joins the composed view. **Not** a reason to
   put it on `StateView` in Half A.
2. **Where do the per-state resource requirements live?** §3.4's table is a
   property of a `State`, so `internal/job` is natural, but only the Queue
   consumes it. Half B.
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
| **D-I3** | `next` is set ⟺ the current state's work is finished; its value is the next move. Written by the worker that completes the state; `Finalizing` excepted, settling via `Finish`. |
| **D-I4** | Running-ness is about the CURRENT state and derived from what the job holds; grantability is about the NEXT state. They are different predicates. |
| **D-I5** | `gatedBy` and `waitReason` are pure; acquisition happens only in `grantFor`; `advance` writes no job state on any blocked path. |
| **D-I6** | Gate precedence is pause > global pause > lease > compute slot; cancel is handled before the gate. |
| **D-I7** | `legalEdges` narrows to the six-edge work spine; `from == to` is removed; cancellation is not an edge. |
| **D-I8** | `Cross` is a fourth door owning the one boundary edge, sole writer of `crossed`, and the only way to surrender the lease at the crossing. |
| **D-I9** | Every persisted job restores to its persisted state holding nothing; `state`, `next`, `crossed` and `Intent` persist. Pause and restart are one path. |
| **D-I10** | Half A lands before Half B. |

---

## 10. Revision history

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
| `next`-clearing stated twice, redundantly and inconsistently | §3.3 rule 3: `transition` and `Cross` clear it, nothing else |
| `StateUnset` breaks `TestAllStates_Exhaustive`'s AST check | named in §3.2 with the required fix |

**Rejected.** The claim that removing the `→ Finished` edges breaks completion,
on the grounds that an `Unrecoverable` verdict needs `SetNext(Finished)`. It
does not: `Finish` is a separate door and the assessor enacts that verdict
through it. `finish` never consults `legalEdges` (§4.1). The secondary point —
that existing tests assert those edges — is true but expected; §6 scopes them
for rewrite.

**From tracing rather than review.** §5's scenarios were built to game the fix
out, and produced four changes no review had asked for: `Cross` as a fourth door
(§3.5), `BeginAttempt` taking the lease (§3.5), the pause/restart path
equivalence (§3.8), and the rejection of a separate `workComplete` flag (§3.3).

**Revision 1 → 2** is recorded in this file's git history.
