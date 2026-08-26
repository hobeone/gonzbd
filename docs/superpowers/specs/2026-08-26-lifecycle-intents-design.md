# Lifecycle Intents — Design

**Status:** proposed, revision 2. Revision 1 was reviewed externally and had
eleven findings against it; §9 records what changed and why.

**Amends** `docs/superpowers/specs/2026-08-25-job-lifecycle-design.md`:
supersedes §8.2 and §10, narrows §8.3, §8.4 and §12. Everything else in that
spec stands — §6 in particular is not merely preserved but load-bearing here.

**Resolves:** issue #441, whose filed premise this document rejects — see §2.3.

**Scope:** `internal/job`, and an amendment to the swap plan's `Queue`.

---

## 1. The problem

`internal/job` landed in #439 with `Waiting` as one of seven states, carrying a
payload (`Next`, `Reason`) meaningful for that state alone. Two things it
cannot express have since surfaced, and they are not independent.

### 1.1 A pause request has nowhere to live

The prior spec §8.3 states that pause is a **gate, not an interrupt**: work in
flight runs to the end of its state, and the job then stops. It then requires
the UI show exactly that:

> The cost is honest: pausing mid-repair on a large set does nothing for
> minutes. The fix is to *show* that ("finishing repair, then pausing"), not to
> make repair interruptible.

The model cannot represent that sentence. A pause is recorded as a
`WaitReason`, and `Attempt.setReason` refuses unless the attempt is already
`Waiting`:

```go
if a.state != Waiting {
    return fmt.Errorf("%w: cannot set a wait reason on an attempt in state %s", ErrNotWaiting, a.state)
}
```

Pausing a `Repairing` job returns `ErrNotWaiting`. There is no window between
the request and the boundary that consumes it, because the request has no home
until the job has *already* stopped. The daemon ships per-job pause today
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

Three of the four are **derived**, yet all four are stored on the `Attempt` and
independently assignable. Standing Design Rule 2 names this shape: *"a derived
value that is also persisted… creates a second source of truth, and the stored
copy is the one that drifts."*

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

Each was patched with a latch remembering what `state` forgot — `next`,
`assessed`, `crossed`, `Job.pending`. Every defect lived at a latch boundary.

### 2.2 Three of five live on `Waiting`

Defects 1, 2 and 3 are all `Waiting`: the only state with a mandatory payload,
and the only state two doors write.

### 2.3 What #441 proposed, and why it was wrong

Issue #441 argued pause and admission are two situations wearing one name, and
that pause should become an orthogonal axis because *"pause has no
destination."*

That premise is false. Under §8.3's gate model a paused job has **finished its
state** and is held at a boundary with a decided next step — structurally
identical to waiting for a compute slot. §8.2's unification is correct, and
#441 argued against it without engaging it.

The narrower truth is that the thing §8.2 unifies — *blocked-ness* — is
**derivable**, so it needs no state. That is a different conclusion from
#441's, reached by rejecting its argument rather than accepting it.

---

## 3. The design

### 3.1 `Intent` is a fourth orthogonal axis

Prior spec §3 establishes that position (`State`), execution (`Activity`) and
verdict (`Outcome`) are orthogonal. `Intent` is a fourth on the same footing:
**what a person has asked of this job**, independent of where it is or what it
is doing.

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
the current attempt is settled, for the reason `setReason` already gives: a
settled job's next move is a decision for the user, not an intent to record.

### 3.2 `Waiting` is removed from `State`

A blocked job is **still at the state it last occupied**.

| Situation | Before | After |
|---|---|---|
| Assessed, wants `Repairing`, no compute slot | `Waiting{Next: Repairing}` | `Assessing`, `next = Repairing`, holds no slot |
| Paused mid-fetch | `Waiting{Next: Fetching}` | `Fetching`, holds no lease |
| Never run | `Waiting{Next: Fetching}` + `Job.pending` | no attempt; `StateUnset` |

This applies the prior spec's own §3 rule to a case it had not been applied to:
position and execution are different axes.

**`State` gains an invalid zero.** Removing `Waiting` would otherwise make
`Fetching` the zero value, so a zero `StateView` would read as an active
download — Rule 2's "a type with a valid zero used as a key" smell, which the
old ordering avoided by accident. Therefore:

```go
const (
    StateUnset State = iota // not a state; the zero value, never legal as a destination
    Fetching
    Assessing
    Repairing
    Extracting
    Finalizing
    Finished
)
```

`AllStates()` returns the six real states and excludes `StateUnset`. Every door
refuses it as a destination. `Job.State()` returns `StateView{State:
StateUnset}` for a job with no attempt, which `HasRun()` already distinguishes
and which `ToSABnzbd` maps explicitly (§4.4).

### 3.3 `next` exists for exactly one state

> **`next` is the single decider's verdict, and nothing else.**

This is not a stipulation — it falls out of the graph. After removing
`Waiting`, the spine successors are:

| State | Spine successors | Needs `next`? |
|---|---|---|
| `Fetching` | `Assessing` | no — derivable |
| `Assessing` | `Fetching`, `Repairing`, `Extracting` | **yes** |
| `Repairing` | `Assessing` | no — derivable |
| `Extracting` | `Finalizing` | no — derivable |
| `Finalizing` | — | no |

Every state but `Assessing` has at most one spine successor, so where a blocked
job "wants to go" is computable from `legalEdges`. `Assessing` is the only
state that branches — which is §5's single-decider property, arrived at from
the graph rather than asserted.

So `next` is written **only by the assessor recording a verdict**, and is
cleared by `transition` when the move is taken:

```go
func (j *Job) SetNext(n State) error   // Assessing only; write-once per visit
```

Three rules, each closing a specific hole:

1. **`SetNext` is legal only from `Assessing`.** Elsewhere there is nothing to
   decide.
2. **Write-once per visit.** `SetNext` refuses when `next` is already set to a
   different value, and is a no-op when set to the same one. Without this, a
   verdict of `Repairing` could be overwritten with `Extracting` and the job
   would cross the boundary skipping repair — defect 3's mechanism surviving
   into a new door. Re-entering `Assessing` clears `next` and permits a fresh
   verdict.
3. **`SetNext` is guarded by `legalEdges`.** A value the current state could not
   reach directly cannot be parked either.

`transition` correspondingly keeps its `to == next` check, whose justification
changes: with `Waiting` gone it no longer guards the boundary, and its sole
remaining purpose is enforcing that once `Assessing` has decided, nothing else
may choose. When `next` is unset, `transition` accepts any edge `legalEdges`
permits.

### 3.4 Running-ness is a property of what the job holds

Prior spec §6 already answers "is this job running", and revision 1 failed to
use it:

```go
func (j *Job) BeginFetch(l *Lease) error  // cannot be called without one
func (j *Job) Surrender() *Lease          // called on crossing
```

**The Job holds the Lease object.** Running-ness is therefore not a pool
question but a holding question:

| State | Requires |
|---|---|
| `Fetching` | lease |
| `Assessing`, `Repairing` | lease + compute slot |
| `Extracting`, `Finalizing` | compute slot (the lease was surrendered at crossing) |
| `Finished`, `StateUnset` | nothing |

> **`running(j)` ≡ the job holds everything its current state requires.**

This is §6's own argument — *"residency stops being a property three functions
must agree about and becomes an object you either hold or do not"* — applied to
admission. Three consequences follow without further machinery:

- An actively-downloading job holds a lease; a parked one does not. `{Fetching,
  holds}` and `{Fetching, does not}` are distinguishable, so no `ActDownload`
  activity is needed. There is none: `grep -n 'Act[A-Z][a-zA-Z]*'
  internal/job/activity.go` returns ten activities, none for fetching.
- A job re-entering `Fetching` from `Assessing` still holds its lease, so it is
  running by construction. §8.1's non-starvation guarantee stops being a rule
  and becomes a consequence of never having let go.
- A job with capacity free but low priority correctly does not hold a lease, so
  `NoLease` is the honest reason.

### 3.5 The Queue composes, and nothing is stored

```go
// nextMove reports where this job would go if unblocked: the recorded verdict
// if one is pending, otherwise the sole spine successor of the current state.
// Returns false when there is no forward move (Finalizing, Finished, StateUnset).
func nextMove(v job.StateView) (job.State, bool)

// blockedBy reports why this job is not running, if it is not.
// PURE: acquires nothing, so it is safe on the render path.
func (q *Queue) blockedBy(j *job.Job, next job.State) (job.WaitReason, bool) {
    if j.Intent() == job.IntentPause { return job.UserPaused, true }
    if q.paused                      { return job.GlobalPause, true }
    if !j.HoldsLeaseFor(next)        { return job.NoLease, true }
    if !j.HoldsSlotFor(next)         { return job.NoComputeSlot, true }
    return 0, false
}
```

Precedence is **pause > global pause > lease > compute slot**. `IntentCancel`
is deliberately absent: `advance` handles it before `blockedBy` is consulted,
so no cancel value leaks onto the render path (revision 1 had one, and it
rendered as `Queued`).

```go
func (q *Queue) advance(j *job.Job) error {
    if j.Intent() == job.IntentCancel {
        return q.finishCancel(j)      // see §3.6
    }
    next, ok := nextMove(j.State())
    if !ok { return nil }
    if _, blocked := q.blockedBy(j, next); blocked { return nil }
    if !q.acquireFor(j, next) { return nil }   // lost the race; retried next tick
    return j.Transition(next)
}
```

`advance` writes no job state on the blocked or lost-race paths, because there
is nothing to record: the verdict already lives in `next`, written by the
assessor. Revision 1's `advance` called `SetNext` on every block and could drop
a verdict when acquisition lost a race; that failure mode does not exist here.

**`advance` is the only way a job moves between work states, and it takes no
target** — the target is computed, not passed. **`Finish` is not routed through
it:** `Finished` is not a work state, and closing an attempt needs an
`Outcome`, which `advance` does not carry. Whoever completes `Finalizing` calls
`j.Finish(OutcomeOK, now)` directly.

**Resume needs no notification.** `SetIntent(IntentRun)` writes a flag; the
Queue's scheduling loop calls `advance` for eligible jobs on its ordinary
cadence and picks the change up. `Job` cannot call `Queue` and does not need
to.

### 3.6 Cancel

§8.4 makes cancel an interrupt before the boundary and a gate after it, so it
is the one intent not purely an acquisition decision:

```go
func (q *Queue) Cancel(j *job.Job) error {
    if err := j.SetIntent(job.IntentCancel); err != nil { return err }
    return q.finishCancel(j)
}

func (q *Queue) finishCancel(j *job.Job) error {
    if job.IsProduction(j.State().State) {
        return nil    // gate: work runs to the boundary; advance closes it there
    }
    return j.Finish(job.OutcomeCancelled, q.now())   // interrupt: stop now
}
```

`IsProduction(j.State().State)` is sound here because a paused post-boundary job
now reads `Extracting`, not `Waiting` — the erasure that made defect 5 possible
is gone.

### 3.7 Restart

This **supersedes prior spec §10's** statement that *"every persisted job
restores to `Waiting`. There is no other legal option."* `Waiting` no longer
exists, so that sentence has no referent.

The argument behind it is unchanged and is what the new rule preserves:
**nothing persists a lease.** So:

> **Every persisted job restores to the state it was persisted at, holding
> nothing.**

`running` is therefore false for every job immediately after a restart, without
any special-casing: it holds no lease and no slot, so `blockedBy` reports
`NoLease` or `NoComputeSlot` and the Queue re-grants in priority order.
`Intent` and `next` are persisted — both are decisions, not resources. Restart
remains the ordinary scheduler starting from a cold pool, and remains forced
rather than remembered: the thing you would need in order to be running cannot
be deserialized.

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
CanTransition`, which returns nothing — so completion is unaffected by their
removal.

```
Fetching   → Assessing
Assessing  → Fetching, Repairing, Extracting
Repairing  → Assessing
Extracting → Finalizing
```

Six edges, down from 22.

**`CanTransition`'s `from == to` early return is removed.** It exists partly to
keep `hold`'s `Finalizing` case reachable, and `hold` is deleted; `nextMove`
never returns the current state, so no door requests a self-transition. Leaving
it would permit a legal no-op that clears `Activity` and nothing else.

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

`IsCorrectness` is **retained** despite having no non-test caller (`git grep -n
'IsCorrectness' internal/job/*.go` returns the definition and comments only):
`TestBoundaryIsUnreachableByAnyPath` uses it as its oracle precisely because no
door decides with it, and that independence is what makes the test meaningful.

`transition`'s `to == next` check is **retained**, with a new justification —
see §3.3.

### 4.3 What this does to the five defects

- **1** — `transition(Waiting)` cannot be written. Unrepresentable.
- **2** — no `Waiting` node to launder through; from `Extracting`, `legalEdges`
  permits only `Finalizing`. Unrepresentable.
- **3** — **guarded, not unrepresentable.** Revision 1 claimed otherwise and was
  wrong: deleting `hold` removes one door for re-declaring a destination, but
  `SetNext` is a new one. §3.3's write-once-per-visit rule is what closes it,
  and it is a guard.
- **5** — repaired at its root: a pause no longer overwrites `state`, so
  `IsProduction(a.state)` is correct for a held post-boundary job. `finish`'s
  guard on `a.crossed` stays, because `finish` still erases `state` and defect 4
  depends on reading the latch after settlement.
- **4** — untouched. `crossed`, `ErrBoundaryConsumed` and
  `TestBoundaryIsUnreachableByAnyPath` all remain load-bearing.

**This design does not retire the boundary machinery.**

### 4.4 `ToSABnzbd`

It currently reads `v.State == Waiting` and `v.Reason.IsPause()`; neither
survives. It takes the Queue-composed view, keyed on **running-ness and
intent** rather than on a wait reason:

| Composed view | Status |
|---|---|
| `StateUnset` (never run) | `Queued` |
| `Finished` | per `finishedStatus(Outcome)`, unchanged |
| running (any intent) | its state's status, per the rows below |
| not running, `IntentPause` | `Paused` |
| not running, otherwise | `Queued` |

State rows for a running job are unchanged from today: `Fetching` + `Assessed`
→ `Fetching`, else `Downloading`; `Assessing` + `ActCRCCheck` → `QuickCheck`,
else `Verifying`; `Repairing` → `Repairing`; `Extracting` → `Extracting`;
`Finalizing` + `ActScript` → `Running`, else `Moving`.

**A running job with `IntentPause` renders as its current state, not as
`Paused`** — it is still repairing. This is §1.1's requirement, and it is why
the table keys on running-ness first. Revision 1's table keyed on the wait
reason and rendered such a job `Paused`, contradicting its own following
paragraph.

Surfacing "finishing repair, then pausing" is the UI reading `Intent` alongside
the status.

The four never-produced statuses (`Idle`, `Grabbing`, `Propagating`,
`Checking`) stay never-produced.

### 4.5 Ownership

| State | Owner | Written by |
|---|---|---|
| `Intent` | `Job` | `SetIntent` |
| `state` | `Attempt` | `transition`, `finish` |
| `next` | `Attempt` | `SetNext`, cleared by `transition` |
| `crossed` | `Attempt` | `transition` |
| `outcome` | `Attempt` | `finish` |
| the `Lease` | `Job` | `BeginFetch`, `Surrender` |
| running-ness | derived | nobody |
| wait reason | derived | nobody |

### 4.6 Sequencing

There is no replacement `Queue` yet — `git grep -n 'type[ ]Queue[ ]struct'
internal/queue` finds `queue.go:74`, the *existing* queue the swap plan
replaces. So this lands in two halves.

| Half | Contains | Lands |
|---|---|---|
| **A — `internal/job`** | remove `Waiting`; add `StateUnset`; add `Intent`; narrow `next` and add its three rules; delete the §4.2 list; narrow `legalEdges`; rewrite the affected tests | its own plan, next |
| **B — `Queue`** | `nextMove`, `blockedBy`, `advance`, `Cancel`, the composed view, `ToSABnzbd`'s new inputs, `HoldsLeaseFor`/`HoldsSlotFor` | amendment to the swap plan's item 3 |

**Half A lands a package that cannot express "not running."** That marker was
`Waiting`, and its replacement is the `Lease`, which is the swap plan's item 1.
The `Lease` cannot be pulled forward piecemeal: §6's argument is that the
lease, manifest and barrier are one object *because they share a lifetime*, and
a bare admission token would re-create the problem §6 solves.

This is accepted rather than worked around. Nothing imports `internal/job` —
`git grep -ln 'gonzbd/internal/job' -- '*.go' | grep -v '^internal/job/'`
returns nothing, run against this commit — so the gap is unobservable, and Rule
1 says land the end state rather than defend an intermediate. **Half A must land
before Half B**, since that grep stops returning nothing the moment B wires the
Queue up.

`ToSABnzbd` cannot compute its new inputs in Half A. Half A's plan decides
whether it takes a view type B later fills, or is removed and reintroduced in
B; YAGNI favours the latter, but it discards working tested code, so the call
belongs with the plan that writes it.

---

## 5. Testing

1. **`blockedBy` precedence is total.** Walk `Intent × globalPause × holdsLease
   × holdsSlot × next ∈ AllStates()` and assert the reason matches §3.5's
   precedence at every point.
2. **`blockedBy` acquires nothing.** Call it across the product space with both
   pools exhausted and assert occupancy is unchanged. A `tryAcquire` leaking
   into the render path is the failure mode that would not show up as a wrong
   answer.
3. **`nextMove` agrees with `legalEdges`.** For every state with one spine
   successor, assert `nextMove` returns it; for `Assessing`, assert it returns
   the recorded verdict, and nothing when none is recorded.
4. **`SetNext`'s three rules**, one test each: refused outside `Assessing`;
   refused when re-declaring a different value; refused for a non-edge. The
   second is defect 3's pin and must be mutation-verified.
5. **`TestBoundaryIsUnreachableByAnyPath` is rewritten, not deleted.** Action
   set changes (`Hold` out, `SetIntent`/`SetNext` in); config key gains
   `Intent`. It must stay mutation-verified — reverting `BeginAttempt`'s
   `crossed` refusal must still turn it red.
6. **Writer enumerations carry forward.** The `outcome` and `attempts` writer
   tests stay. Add one asserting `SetIntent` is the sole writer of `Intent`, and
   one asserting `SetNext` and `transition` are the only writers of `next`.
7. **The zero value is loud.** Assert `StateView{}.State == StateUnset` and that
   `ToSABnzbd` maps it to `Queued`, not to a download.
8. **`ToSABnzbd` product-space tests** gain running-ness and `Intent` axes;
   totality, declared-status and never-produced assertions otherwise unchanged.

---

## 6. Deliberately not built

- **A generic "gate" abstraction.** `Intent` has two non-default values and one
  consumer.
- **Per-intent timestamps.** When a job was paused is a history question, owned
  by the Checkpointer.
- **`Intent` on the `Attempt`.** Rejected in §3.1.
- **Interruptible post-boundary work.** §8.3's argument stands unchanged.

---

## 7. Open questions

1. **Can a `Finished` job be told retryable from outside?** `finish` erases
   `state`, so a `Finished(Failed)` job's crossing is only in the `crossed`
   latch, which is not on `StateView`. `BeginAttempt` returning
   `ErrBoundaryConsumed` is the authoritative answer but a poor basis for a UI
   affordance. Rendering moves to the Queue in Half B; decide there whether
   `Crossed` joins the composed view. **Not** a reason to put it on `StateView`
   in Half A.
2. **Where do `needsLease`/`needsSlot` live?** They are properties of a `State`,
   so `internal/job` is natural, but only the Queue consumes them. Deferred to
   Half B.
3. **Does the dispatcher need `next`?** §9 of the prior spec is unread against
   this change. To be checked when Half B is written, not assumed here.

---

## 8. Decisions

| | Decision |
|---|---|
| **D-I1** | `Intent` is a fourth orthogonal axis, on the `Job`, with `IntentCancel` latching. |
| **D-I2** | `Waiting` is removed from `State`; `StateUnset` becomes an invalid zero. |
| **D-I3** | `next` exists only for `Assessing`, written only by the assessor, write-once per visit, cleared by `transition`. |
| **D-I4** | Running-ness is derived from what the job holds (§6's `Lease`), never stored. |
| **D-I5** | `blockedBy` is pure; acquisition happens only in `advance`; neither writes job state when blocked. |
| **D-I6** | Precedence is pause > global pause > lease > compute slot; cancel is handled before `blockedBy`. |
| **D-I7** | `legalEdges` narrows to the six-edge work spine; `from == to` is removed; cancellation is not an edge. |
| **D-I8** | The boundary machinery is unchanged and remains load-bearing. |
| **D-I9** | Every persisted job restores to its persisted state holding nothing; `Intent` and `next` persist. |
| **D-I10** | Half A lands before Half B, accepting a window where "not running" is inexpressible. |

---

## 9. Revision history

**Revision 2** answers an external adversarial review of revision 1. Nine of its
eleven findings were accepted, one rejected, one partially accepted.

| Finding | Disposition |
|---|---|
| `blockedBy` blind to priority; active and parked `Fetching` indistinguishable; re-entry blocked despite holding a lease | **Accepted.** One error, three symptoms. §3.4 rewritten around what the job holds, using §6's `Lease`. |
| `advance` could drop a verdict on a lost acquisition race | **Accepted.** §3.5's `advance` writes no job state when blocked; the verdict lives in `next`, written by the assessor. |
| Contradicts prior spec §10's "every persisted job restores to `Waiting`" | **Accepted.** §3.7 supersedes it explicitly and preserves its argument. |
| Defect 3 is representable via a second `SetNext` | **Accepted.** §3.3's write-once-per-visit rule; §4.3 corrected to say "guarded, not unrepresentable". |
| §3.3 and §3.4 disagreed on when `next` is set | **Accepted.** §3.3 was right; `next` is needed only by `Assessing`, which §3.3's table now derives. |
| Removing `Waiting` makes `Fetching` the zero value | **Accepted.** `StateUnset` added in §3.2. |
| `Cancelling` leaked to the render path as `Queued`; the §4.4 table contradicted its own prose | **Accepted.** `Cancelling` removed from `WaitReason`; §4.4 keys on running-ness first. |
| Resume trigger unspecified | **Accepted.** §3.5: the scheduling loop picks it up; no notification path needed. |
| A `Finished` job's retryability is not visible externally | **Partially accepted.** A real gap, but not a false claim — logged as open question 1 rather than changing `StateView`. |
| Removing the `→ Finished` edges breaks normal completion | **Rejected.** `finish` never consults `legalEdges` — verified in §4.1. The finding assumed completion routes through `advance`; it does not. It did expose that revision 1 never said how completion is invoked, now stated in §3.5. |
