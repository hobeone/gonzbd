# Lifecycle Intents — Design

**Status:** proposed. Supersedes §8.2 of
`docs/superpowers/specs/2026-08-25-job-lifecycle-design.md`, and narrows §8.3,
§8.4 and §12 of the same document. Everything else in that spec stands.

**Resolves:** issue #441, whose filed premise this document rejects — see §2.3.

**Scope:** `internal/job` and the `Queue` that the next plan builds. No other
package changes.

---

## 1. The problem

The job lifecycle machine landed in #439 with `Waiting` as one of seven states,
carrying a payload (`Next`, `Reason`) meaningful for that state alone. Two
things it cannot express have since surfaced, and they are not independent.

### 1.1 A pause request has nowhere to live

`docs/superpowers/specs/2026-08-25-job-lifecycle-design.md` §8.3 states that
pause is a **gate, not an interrupt**: work in flight runs to the end of its
state, and the job then stops. It goes on to require the UI show this:

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

So pausing a `Repairing` job returns `ErrNotWaiting`. There is no window
between the request and the boundary that consumes it, because the request has
no home until the job has *already* stopped. The daemon ships per-job pause
today (`queueSetPaused` in `internal/api`), so this is a gap against existing
behaviour, not a hypothetical.

A gate has two parts — the flag, and the check. The design modelled only the
moment the check fires.

### 1.2 `WaitReason` mixes two ownerships in one field

| Reason | Who can know it |
|---|---|
| `UserPaused` | the job — nothing else can |
| `NoLease` | the Queue: pool-A availability |
| `NoComputeSlot` | the Queue: pool-B availability |
| `GlobalPause` | the Queue: already owns this, persisted via `Store.SetPaused` |

Three of the four are **derived** from what the Queue knows, yet all four are
stored on the `Attempt` and independently assignable. `AGENTS.md` Standing
Design Rule 2 names this shape directly: *"a derived value that is also
persisted… creates a second source of truth, and the stored copy is the one
that drifts."*

The drift is reachable. A job parked at `NoComputeSlot` that is then globally
paused has its reason overwritten, and nothing remembers what it was waiting
for. The reverse — a global pause arriving and being unable to record itself
at all — is the gap the core plan named as known-open.

---

## 2. The pattern behind #439's defects

Five functional defects were found on #439. Classifying them is what produced
this design, and the classification matters because it also rules out the fix
originally proposed.

| # | Escape |
|---|---|
| 1 | `transition(Waiting)` stranded an attempt — movable afterwards only by `finish` |
| 2 | `Extracting → Waiting → Fetching` — two legal edges composing into the one the graph forbids |
| 3 | `hold(Finalizing) → hold(Fetching) → transition(Fetching)` — a second hold re-declared the destination |
| 4 | `BeginAttempt` let a crossed job open a fresh attempt in Correctness |
| 5 | `finish` guarded on `IsProduction(a.state)`, but `hold` had already overwritten `a.state` with `Waiting` |

### 2.1 `state` is a lossy summary

`legalEdges` is a relation over `(from, to)` pairs — a Markov model, where the
next legal move depends only on where the job is now. None of the machine's
invariants are expressible that way:

| Invariant | Shape |
|---|---|
| Never return from Production to Correctness | reachability |
| Resume where the pause decided | memory |
| `Unrecoverable` means never crossed | history |
| A crossed job cannot be retried | history, spanning attempts |

Each was therefore patched with a latch remembering what `state` forgot —
`next`, `assessed`, `crossed`, `Job.pending`. Every defect above lived at a
latch boundary.

### 2.2 Three of five live on `Waiting`

Defects 1, 2 and 3 are all `Waiting`. That is not coincidence: it is the only
state with a mandatory payload, and it is the only state two doors write.

### 2.3 What #441 proposed, and why it was wrong

Issue #441 argued that pause and admission are two different situations wearing
one name, and that pause should become an orthogonal axis because *"pause has
no destination."*

That premise is false. Under §8.3's gate model a paused job has **finished its
state** and is held at a boundary with a decided next step — structurally
identical to waiting for a compute slot. §8.2's unification is correct, and
#441 argued against it without engaging it.

The narrower truth is that the thing §8.2 unifies — *blocked-ness* — is
**derivable**, so it needs no state at all. That is a different conclusion from
#441's, reached by rejecting its argument rather than accepting it.

---

## 3. The design

Four changes, which stand or fall together.

### 3.1 `Intent` is a fourth orthogonal axis

§3 of the prior spec establishes that position (`State`), execution
(`Activity`) and verdict (`Outcome`) are orthogonal, and that collapsing them
into one enum is the original defect. `Intent` is a fourth on the same footing:
**what a person has asked of this job**, independent of where the job is or
what it is doing.

```go
// Intent is settable at almost any point and consumed at the next boundary.
// It is the half of "pause is a gate" (prior spec §8.3) that the state
// machine could not express: the window between the request and the boundary
// that acts on it.
type Intent uint8

const (
    IntentRun    Intent = iota // default
    IntentPause
    IntentCancel              // latches — see below
)

func (j *Job) Intent() Intent
func (j *Job) SetIntent(i Intent) error
```

`Intent` lives on the **`Job`**, not the `Attempt`: a paused job that is retried
stays paused, because the pause was a statement about the job, not about one
run of it.

`IntentCancel` latches. `SetIntent` returns an error rather than leaving it, so
"cancel then unpause" cannot revive a cancelled job. `IntentRun ↔ IntentPause`
is freely reversible.

`SetIntent` is legal in any state where an attempt is open or none has run. It
is refused once the current attempt is settled, for the reason
`Attempt.setReason` already gives: a settled job's next move is a decision for
the user, not an intent to record.

### 3.2 `Waiting` is removed from `State`

A blocked job is **still at the state it last occupied**, with `Activity:
ActNone`.

| Situation | Before | After |
|---|---|---|
| Assessed, wants `Repairing`, no compute slot | `Waiting{Next: Repairing}` | `Assessing`, `next = Repairing` |
| Paused mid-fetch | `Waiting{Next: Fetching}` | `Fetching`, no pending move |
| Never run | `Waiting{Next: Fetching}` + `Job.pending` | no attempt open |

This is the prior spec's own §3 rule applied to a case it had not been applied
to: **position and execution are different axes.** "At `Fetching`, not
currently fetching" is exactly what `Activity: ActNone` already means
everywhere else in the machine.

`State` becomes six members: `Fetching`, `Assessing`, `Repairing`,
`Extracting`, `Finalizing`, `Finished`.

### 3.3 `next` is retained, and narrows to one meaning

> **`next` is the memory of the single decider's output across a block.**

§5 makes `Assessing` the only state that branches. Its verdict — `NeedsMore →
Fetching`, `NeedsRepair → Repairing`, `OK → Extracting` — is not derivable from
anything else, and must survive if the job cannot act on it immediately.

The Queue must not hold it: that would be per-job state in the Queue, a second
source of truth for a decision the job's own machine made.

`next` is therefore **the decided-but-not-yet-taken move**, and is unset in
every other case. A paused `Fetching` job has no `next` — it has made no
decision, it is simply not running.

Setting `next` is guarded by the same edge check that guards taking it, so a
value the current state could never reach directly cannot be parked either.

### 3.4 Blocked-ness and its reason are both derived

Neither is stored. The Queue answers both, from one function:

```go
// blockedBy reports why this job may not take its next move, if it may not.
// PURE: it acquires nothing. Acquisition happens only in advance, below.
// This is the single place intent, global pause and both capacity pools
// meet; the precedence is total and exhaustively testable without a live
// queue holding capacity.
func (q *Queue) blockedBy(j *job.Job, next job.State) (job.WaitReason, bool) {
    switch j.Intent() {
    case job.IntentCancel: return job.Cancelling, true
    case job.IntentPause:  return job.UserPaused, true
    }
    if q.paused                        { return job.GlobalPause,   true }
    if needsLease(next) && q.poolA.full() { return job.NoLease,       true }
    if needsSlot(next)  && q.poolB.full() { return job.NoComputeSlot, true }
    return 0, false
}
```

Precedence is **cancel > pause > global pause > lease > compute slot**, and it
lives in this function alone rather than being re-derived by each caller.

`Cancelling` is a fifth `WaitReason`, added by this design. It never renders —
`advance` routes it to `finish` rather than to a parked state — but it keeps
`blockedBy` total over the precedence table rather than needing a separate
out-of-band signal for the one arm that does not park.

`advance` is the only way a job moves between work states:

```go
func (q *Queue) advance(j *job.Job, next job.State) error {
    if r, blocked := q.blockedBy(j, next); blocked {
        if r == job.Cancelling {
            return j.Finish(job.OutcomeCancelled, q.now())
        }
        return j.SetNext(next)   // record the decision; do not move
    }
    if err := q.acquireFor(next); err != nil {
        return err
    }
    return j.Transition(next)
}
```

**`blockedBy` must stay side-effect-free.** It is called on the render path, and
a `tryAcquire` leaking into it would hand capacity to a UI refresh. Acquisition
belongs to `advance` alone.

---

## 4. Consequences

### 4.1 `legalEdges` narrows to the work spine

Removing `Waiting` removes its six outgoing and five incoming edges. The
`→ Finished` edges then have no consumer: `git grep -n 'CanTransition('
internal/ --include='*.go' | grep -v _test.go` returns two live call sites,
both in `attempt.go` — `transition` (line 197) and `hold` (line 266); every
other hit is a comment. `hold` is deleted by this design, and `transition`
refuses `Finished` outright before it ever consults the map. Cancellation is a
property of `finish` and the boundary rules, not an edge.

`legalEdges` therefore becomes the six-edge work spine:

```
Fetching   → Assessing
Assessing  → Fetching, Repairing, Extracting
Repairing  → Assessing
Extracting → Finalizing
```

Down from 22. The partition rule that `TestEdgeCountsMatchTheStatedPartition`
enforces collapses with it: there is one bucket now, so that test is replaced
by a direct assertion on the map's contents.

### 4.2 What deletes

| Deleted | Why |
|---|---|
| `Waiting` (State) | blocked-ness is derived |
| `Attempt.reason` | reason is derived |
| `Job.pending` | a never-run paused job is `IntentPause` with no attempt |
| `hold`, `Job.Hold` | nothing to park into |
| `SetWaitReason`, `setReason`, `ErrNotWaiting` | no stored reason to update |
| `ErrHoldRequired` | `transition` has no `Waiting` to refuse |
| `hold`'s re-park refusal | `hold` is gone |

`IsCorrectness` has no non-test caller today — `git grep -n 'IsCorrectness'
internal/job/*.go` returns the definition and comments only. It is **retained**
regardless: `TestBoundaryIsUnreachableByAnyPath` uses it as its oracle
precisely because no door decides with it, and that independence is the
property making the test meaningful (§5.3).

**`transition`'s `to == a.next` check does NOT delete.** Its justification
changes. Today it exists to stop `Waiting` from fanning out (defect 2); with
`Waiting` gone that job is done by `legalEdges` alone. But it acquires a second
and now sole purpose: **enforcing that `Assessing` is the only decider.** Once a
verdict has set `next`, `transition` must accept nothing else, or a caller could
take a different legal edge — from `Assessing`, `legalEdges` permits `Fetching`,
`Repairing` and `Extracting`, so a caller ignoring a `NeedsRepair` verdict and
calling `Transition(Extracting)` would pass a bare edge check. The rule is
therefore:

> When `next` is set, `transition` accepts only `next`. When `next` is unset,
> `transition` accepts any edge `legalEdges` permits from the current state.

### 4.3 What this does to the five defects

Defects 1, 2 and 3 become **unrepresentable** rather than guarded:

- **1** — `transition(Waiting)` cannot be written; there is no such state.
- **2** — there is no `Waiting` node to launder through. From `Extracting`,
  `legalEdges` permits only `Finalizing`. The composition escape has no path.
- **3** — there is no `hold`, so no second `hold` to re-declare a destination.
  `SetNext` is guarded by the same edge check, so `Extracting` cannot park
  `next = Fetching` in the first place.

Defect **5** is repaired at its root: a pause no longer overwrites `state`, so a
post-boundary paused job still reads `Extracting`. `finish`'s guard on
`a.crossed` remains correct and is now belt-and-braces rather than load-bearing,
because `IsProduction(a.state)` would also be true. **`crossed` is not added to
`StateView`** — an earlier draft of this design required it for `Cancel`, and
that requirement disappears with `Waiting`.

Defect **4** is untouched. `ErrBoundaryConsumed` and the `crossed` latch stay
exactly as they are: `finish` erases `state`, so a *settled* attempt's crossing
is still only recoverable from the latch, and the invariant still spans
`[]Attempt` rather than one attempt.

**This design does not retire the boundary machinery.** `crossed`,
`ErrBoundaryConsumed`, `ErrUnrecoverableAfterBoundary` and
`TestBoundaryIsUnreachableByAnyPath` all remain load-bearing.

### 4.4 `ToSABnzbd`

`ToSABnzbd` currently reads `v.State == Waiting` and `v.Reason.IsPause()`.
Neither survives. It takes the Queue-composed view instead:

| Composed view | Status |
|---|---|
| blocked, reason `IsPause()` | `Paused` |
| blocked, reason not `IsPause()` | `Queued` |
| not blocked, `Fetching`, `Assessed` | `Fetching` |
| not blocked, `Fetching`, not `Assessed` | `Downloading` |
| not blocked, `Assessing`, `ActCRCCheck` | `QuickCheck` |
| not blocked, `Assessing`, otherwise | `Verifying` |
| not blocked, `Repairing` | `Repairing` |
| not blocked, `Extracting` | `Extracting` |
| not blocked, `Finalizing`, `ActScript` | `Running` |
| not blocked, `Finalizing`, otherwise | `Moving` |
| `Finished` | per `finishedStatus(Outcome)`, unchanged |

A job with `IntentPause` that has not yet reached a boundary renders as its
current state, not as `Paused` — it is still repairing. Surfacing "finishing
repair, then pausing" is a UI concern reading `Intent` alongside the status,
and is what §1.1 requires.

The four never-produced statuses (`Idle`, `Grabbing`, `Propagating`,
`Checking`) stay never-produced.

### 4.5 Ownership after this change

| State | Owner | Written by |
|---|---|---|
| `Intent` | `Job` | `SetIntent` |
| `state` | `Attempt` | `transition`, `finish` |
| `next` | `Attempt` | `SetNext`, cleared by `transition` |
| `crossed` | `Attempt` | `transition` |
| `outcome` | `Attempt` | `finish` |
| blocked-ness | derived | nobody |
| wait reason | derived | nobody |

### 4.6 Sequencing: this design lands in two halves

**There is no `Queue` type in this repository today.** `git grep -n
'type[ ]Queue[ ]struct' internal/queue` finds the *existing* queue, which the
prior spec's swap plan replaces; `blockedBy` and `advance` belong to the
replacement, which does not exist yet. So this design cannot land as one
change.

| Half | Contains | Lands |
|---|---|---|
| **A — `internal/job`** | remove `Waiting`; add `Intent`; narrow `next`; delete `hold`/`SetWaitReason`/`Job.pending`/`Attempt.reason`; narrow `legalEdges`; rewrite the affected tests | its own plan, next |
| **B — `Queue`** | `blockedBy`, `advance`, the composed view, `ToSABnzbd`'s new inputs | folded into the existing swap plan, as an amendment to its item 3 |

Half A is safe to land alone because **nothing imports `internal/job`** —
`git grep -ln 'gonzbd/internal/job' -- '*.go' | grep -v '^internal/job/'`
returns nothing, run against this commit rather than taken from the core plan's
prose. The package is deliberately ahead of its consumers, so removing a state
and a field breaks no caller. That is a fact about the tree *now*, and it stops
being true the moment Half B wires the Queue up: Half A must land first.

The one thing Half A must not do is leave `ToSABnzbd` asserting a mapping it
can no longer compute. Two options, to be settled by Half A's plan rather than
here: either `ToSABnzbd` takes a view type whose blocked-ness fields Half B
later fills, or it is removed in Half A and reintroduced in Half B. The second
is cleaner by YAGNI — a shim with no caller and no way to compute half its
inputs is not a shim — but it discards working, tested code, so the choice
belongs with the plan that has to write it.

---

---

## 5. Testing

The enumeration tests from #439 are the model, and each needs a counterpart
here.

1. **`blockedBy` precedence is total.** Walk `Intent × globalPause × poolAFull ×
   poolBFull × next ∈ AllStates()` and assert the reason matches the stated
   precedence at every point. This is the whole reason `blockedBy` is pure.
2. **`blockedBy` acquires nothing.** Call it across the product space with both
   pools at capacity and assert pool occupancy is unchanged. A `tryAcquire`
   leaking into the render path is the one failure mode that would not show up
   as a wrong answer.
3. **`TestBoundaryIsUnreachableByAnyPath` is rewritten, not deleted.** Its
   action set changes (`Hold` out, `SetIntent`/`SetNext` in) and its
   configuration key gains `Intent`. It must stay mutation-verified: reverting
   `BeginAttempt`'s `crossed` refusal must still turn it red.
4. **`next` is only ever a legal successor.** Assert over `AllStates() ×
   AllStates()` that `SetNext` accepts exactly the pairs in `legalEdges`.
5. **Writer enumerations carry forward.** The existing `outcome` and `attempts`
   writer tests stay as-is. Add one for `Intent`, asserting `SetIntent` is the
   sole writer.
6. **`ToSABnzbd` product-space tests** lose the `Reason` axis from the job view
   and gain it on the composed view; totality, declared-status and
   never-produced assertions are otherwise unchanged.

---

## 6. Deliberately not built

- **A generic "gate" abstraction.** `Intent` has two non-default values and one
  consumer. A mechanism parameterised over future gate kinds would be an
  abstraction with one user.
- **Per-intent timestamps.** When a job was paused is a history question, and
  history is the Checkpointer's surface, not the machine's.
- **Intent on the `Attempt`.** Rejected in §3.1: a paused job that is retried
  should stay paused.
- **Interruptible post-boundary work.** §8.3's argument stands unchanged —
  gating at boundaries is what makes resume "start the next state" rather than
  "resume the middle of one".

---

## 7. Open questions

1. **Does `SetNext` need its own error, or reuse `ErrIllegalTransition`?**
   Leaning reuse: it is the same edge check rejecting the same pair, and a
   second sentinel for one predicate is a second enforcement point.
2. **Where does `needsLease`/`needsSlot` live?** They are a property of a
   `State` (pool A spans `Fetching` → crossing; pool B covers `Assessing`,
   `Repairing`, `Extracting`, `Finalizing`), so `internal/job` is the natural
   home — but they are consumed only by the Queue. Deferred to the plan.
3. **Does the dispatcher need `next` too?** §9 is unread against this change.
   To be checked when the swap plan is written, not assumed here.

---

## 8. Decisions

| | Decision |
|---|---|
| **D-I1** | `Intent` is a fourth orthogonal axis, on the `Job`, with `IntentCancel` latching. |
| **D-I2** | `Waiting` is removed from `State`. A blocked job stays at its current state with `Activity: ActNone`. |
| **D-I3** | `next` is retained, meaning only "the decided-but-not-yet-taken move" — the single decider's output across a block. |
| **D-I4** | Blocked-ness and wait reason are derived by the Queue, never stored. |
| **D-I5** | `blockedBy` is pure; acquisition happens only in `advance`. |
| **D-I6** | Precedence is cancel > pause > global pause > lease > compute slot, in one function. |
| **D-I7** | `legalEdges` narrows to the six-edge work spine; cancellation is not an edge. |
| **D-I8** | The boundary machinery (`crossed`, `ErrBoundaryConsumed`, the reachability test) is unchanged and remains load-bearing. |
