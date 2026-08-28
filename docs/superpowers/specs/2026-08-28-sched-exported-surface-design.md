# `internal/sched` Exported Surface — Design (RFC)

> **Source of record:** [issue #450](https://github.com/hobeone/gonzbd/issues/450).
> This file is the committed copy so the Half B2.1 plan has a spec that travels
> with it. The issue carries the review thread; where they differ, the issue's
> comments are later. Amends
> [`2026-08-26-lifecycle-intents-design.md`](2026-08-26-lifecycle-intents-design.md),
> which remains the parent spec — section references (§3.x, §4.x, D-Ixx) are
> to that document unless stated otherwise.

---

## Problem

`internal/sched` landed in #448 as a decision core that nothing imports. Half B2
gives it a caller — a dispatcher, persistence, and the swap that retires
`internal/queue`. Before that plan is written, its exported surface has to be
decided, because today the package's ownership rules stop at the package
boundary and hand each invariant to a caller that cannot honour it.

### 1. Three exported doors yield a lease. Zero exported doors take one back.

`grep -n 'func (j \*Job) \(Finish\|Cross\|Surrender\)' internal/job/job.go`
returns three lines — 307, 323, 471 — and each returns a `*job.Lease` the
caller now owns. On the other side, `grep -n '^func (q \*Queue) [A-Z]'
internal/sched/*.go | grep -v _test.go` returns three: `Cancel`, `Retry`,
`Advance`. None of them accepts a lease.

`q.reclaim` is the sole reclaimer (§3.9, D-I13) and it is unexported. So a B2
dispatcher whose worker fails a download has exactly two options, and both are
defects:

- call `j.Finish(OutcomeFailed, now)` and drop the returned lease — pool A
  loses a slot permanently and silently, which is the precise defect revision 3
  shipped on five paths (§3.9's table);
- do not settle at all, and let the job sit at `Fetching` with an open attempt
  forever, since `Advance` never settles a job on its own.

This is not a missing convenience method. It is the one asymmetry §3.9 exists to
forbid, reintroduced at the package boundary.

### 2. `Advance` cannot park a job that is holding mid-state. Proved, not argued.

The claim is specifically about a job whose current state's work has **not**
finished (`Next == StateUnset`). `Advance`'s branch 2 tests `q.holds` before
`q.gatedBy`:

```go
if s.State.Next == job.StateUnset {
    if q.holds(j.ID(), s) {
        return nil // already working; never take resources from a live worker
    }
    if _, gated := q.gatedBy(s); gated {
        return q.parkLocked(j)
    }
```

Branch 3 (`advance.go:212`) has no such guard and *does* park a gated job whose
work has finished. The gap is branch 2's alone, and that order is correct and
must stay: the Queue cannot distinguish "holding and working" from "holding and
yielded", and stripping a live worker's lease is the worse failure.

The consequence is that **pausing a mid-state job releases nothing**. A
throwaway probe against `0549d7cd` — two `Advance` ticks to open an attempt and
grant a lease at `Fetching`, then `SetIntent(IntentPause)`, then a third tick:

```
after pause+Advance: HoldsLease=true outstanding=1
```

The lease survives the pause indefinitely. `parkLocked` is what releases it, and
it is unexported. Spec §5.1 and §5.2 already show the dispatcher calling `park`
directly on a worker's yield; `internal/sched/doc.go` records this as a B2
obligation. The probe is what turns "the traces show it" into "nothing else
can do it".

### 3. `q.paused` has no setter, so `job.GlobalPause` is unreachable.

`gatedBy` reads `q.paused`; `grep -n 'paused' internal/sched/*.go | grep -v
_test.go` finds five lines — the field declaration at `queue.go:49`, that one
read at `queue.go:167`, and three comment mentions. Nothing writes it. The
global-pause wait reason exists in `internal/job` and cannot be produced by any
code path in production.

### 4. `job.RenderView` exists, is documented as the seam, and nothing can fill it.

`internal/job/render.go:14` defines `RenderView` — `StateView` plus `Running`,
`Reason` and `Intent` — with a doc comment that says outright: *"Nothing in this
package can answer that ... so this type is the seam. Half A constructs these
directly in tests ... Half B fills them for real."*

Half B has not filled it. `waitReason`, `running`, `holds` and `gatedBy` are all
package-private, so no caller outside `sched` can construct a `RenderView`, and
§4.4 makes it the input to `ToSABnzbd`. B1 framed the gap as missing **write**
doors; it is equally a missing **read** door, and the two must be decided
together or B2.1 ships a package the renderer still cannot use.

---

## The organising rule

Three of the five decisions below turn on one distinction, so it is worth
stating before them rather than repeating it three times:

> **No door may take a resource away from a live worker unless the caller *is*
> that worker.**

Every existing door that can release something carries a check sufficient to
establish the job is not mid-work, and no two of them use the same check:

| Door | Initiated by | Check |
|---|---|---|
| `Cancel` → `finishCancel` | user | `q.running` (`cancel.go:49`) |
| `Retry` | user | `errNotSettled` — and `IsSettled` implies `!running`, since `running` requires `IsOpen` |
| `Advance` branch 2 | tick | `q.holds` (`advance.go:203`) |
| `Pause` (proposed) | user | none needed — it releases nothing |

The doors B2 needs are the other case: the dispatcher calls them *because* its
worker has returned. That fact cannot be checked inside `sched` at all.
`running()` is `s.IsOpen() && Next == StateUnset && holds()`, and for a worker
that has just finished normally at `Fetching` every conjunct is still **true** —
so a `!running` guard on `Settle` would reject every ordinary terminal return,
which is the only call `Settle` exists to serve. The precondition can only be
asserted, so it belongs in a stated contract, of the same kind
`Workers.Abort`'s comment already carries for non-blocking.

The failure mode this rules out is dressing an unverifiable precondition as a
verified one, which reads as a guarantee at the call site and is not. An earlier
draft of this RFC did exactly that; see Decision 2.

## Decision 1 — `Settle`

**Proposal:** `func (q *Queue) Settle(j *job.Job, o job.Outcome) error`, called
by the dispatcher when its worker has returned terminally.

It takes `q.mu`, calls `j.Finish(o, q.now())`, releases the slot through
`releaseFor(j, job.StateUnset)`, and reclaims the yielded lease — in that order,
with `Finish`'s error short-circuiting before either release.

That description is already in the tree. It is the tail of `finishCancel`
(`cancel.go:58-80`), comments and all. So the proposal is not "add a method" but
**extract the settle path into one owner and give it a door**:

```go
func (q *Queue) settleLocked(j *job.Job, o job.Outcome) error  // the current finishCancel tail
func (q *Queue) Settle(j *job.Job, o job.Outcome) error        // lock + settleLocked
```

`finishCancel` then calls `settleLocked(j, job.OutcomeCancelled)`. A second,
independently maintained copy of a four-step ordering whose steps were each
fixed once during B1 review is the second-writer smell Standing Design Rule 2
names, and the ordering constraints are not obvious: the slot is freed *before*
the reclaim so an identity-audit failure cannot turn one error into a permanent
pool-B leak, and *after* `Finish`'s error check so a refused settle cannot
strand a running job resourceless.

**`Settle` must refuse `OutcomeCancelled`** — with a sentinel, not a comment.
Cancel is final for a `Job` (D-I14), and it is final because `Cancel` latches
`SetIntent(IntentCancel)` before settling. A dispatcher reaching `Settle` with
`OutcomeCancelled` skips the latch, leaving a job that renders as `Deleted`
(§4.4) but still carries `IntentRun` — so `q.Retry` sees an ordinary settled
attempt and reopens it. That is precisely the resurrection D-I14's note says
must not have a path: "clearing the latch on retry would let a job the user
deleted come back through a path that never re-asked them." A door that never
sets the latch is that path by another route.

**`Settle` honours the cancel latch by overriding the outcome.** This closes a
sequence the first draft missed, and it is not hypothetical: `Cancel` on a
*running* pre-boundary job calls `q.work.Abort` and returns without settling
(`cancel.go:49-56`), leaving the settle to whoever handles the worker's return.
That worker returns with an I/O or context error, and a dispatcher that reports
what it saw calls `Settle(j, OutcomeFailed)`. The job then settles as `Failed`
and renders as `StatusFailed`, when the user deleted it and §4.4 says it must
render as `Deleted`.

So `settleLocked` reads the snapshot it already holds and, when the latch is set
**and the cancel was an interrupt**, settles `OutcomeCancelled` regardless of the
outcome its caller passed. The latch is the authority on how a cancelled job
ends, and one function honouring it beats every dispatcher call site remembering
to check `Intent` first — a second enforcement point for one invariant, which the
Decision Protocol requires escalating rather than adding.

**The interrupt qualifier is load-bearing, and an earlier revision of this RFC
omitted it — a critical defect caught in review.** §8.4 makes cancel an
*interrupt* before the boundary and a *gate* after it, and D-I11 spells out what
the gate means: *"Cancelling a running `Finalizing` job lets it complete as
`OutcomeOK` … the files have moved and the script has run, so `Cancelled` would
be false."* An unconditional override falsifies exactly that: a `Finalizing`
worker that moves the files, runs the user script and returns `OutcomeOK` would
have been recorded as `Cancelled` and rendered `StatusDeleted`, with the disk
saying otherwise.

The rule that closes it follows §8.4's own split rather than enumerating a case.
Cancel-as-interrupt means the job stopped *because of* the cancel, so its error
is our artifact and the outcome is `Cancelled`. Cancel-as-gate means the job
stopped for its own reasons and the outcome must record what actually happened —
with `Intent` surviving on the settled job for the UI to read, which is what
D-I11 says it is for.

**That split already exists in the tree, unnamed, and giving it a name is half
this decision.** `finishCancel` selects its arm on `job.IsProduction`
(`cancel.go:52`); the override needs the complement of the same test. Writing
both inline would put one invariant in two files with nothing linking them —
the second-enforcement-point smell the Decision Protocol requires escalating
rather than adding. They would be correct today only because they happen to
agree.

```go
// cancelInterrupts reports whether Cancel stops this job's work rather than
// gating it (§8.4). It is why the outcome override exists: an interrupted
// worker's error is OUR artifact and settles Cancelled, while a gated
// worker's error is its own and must be recorded as what it was.
func cancelInterrupts(s job.State) bool { return !job.IsProduction(s) }
```

`finishCancel` takes the gate arm when `!cancelInterrupts(s.State.State)`;
`settleLocked` overrides the outcome when
`s.Intent == IntentCancel && cancelInterrupts(s.State.State)`. One predicate,
two readers.

This is not hypothetical tidiness. §8.4's line is drawn at `IsProduction`, but
D-I11's *argument* for it is `Finalizing`-shaped — *"the files have moved and
the script has run"* — and it transfers poorly to `Extracting`, which writes
into the job's own working area that a delete removes anyway. `IsProduction`
itself exists to answer a different question entirely: whether a job can return
to `Fetching` (§4.1's one-way boundary). Cancel borrows it.

Investigating what it would cost to make `Extracting` interruptible found the
mechanism already built — `GoUnRAR` is in-process `rarengine` taking a `ctx`
checked at `go_unrar.go:123`, `contextCopy` checks every 256 KiB, external 7z
and user scripts go through `exec.CommandContext`, and
`PostProcessor.Cancel(jobID)` (`postproc.go:168`) already cancels an in-flight
job's derived context by ID, which is `Workers.Abort`'s exact shape. The barrier
is a spec revision and a partial-extraction cleanup contract, not plumbing. So
the line is plausibly worth moving one day, and `cancelInterrupts` is what makes
that a one-line change in one place instead of a two-file invariant somebody has
to remember.

This is deliberately broader than a `Finalizing && OutcomeOK` carve-out, and the
difference is a real design call rather than a detail. Under `!IsProduction`, a
cancelled `Extracting` job whose unrar fails settles `Failed`, not `Cancelled`.
Recording `Cancelled` there would be false in D-I11's own sense — archives were
partially written — and it would erase a genuine extraction failure behind a
`Deleted` badge.

**The existing test does not catch this, which is the worrying part.**
`TestScenario_5_13_CancellingARunningFinalizingJob`
(`scenario_test.go:846`) settles by calling `j.Finish(job.OutcomeOK, …)`
directly, because no `Settle` door exists yet. It asserts
`StatusCompleted` and *"recording Cancelled here would be false: the files
moved"* — and it would keep passing while a dispatcher routed through an
unconditional `Settle` produced the opposite. B2.1 must therefore re-point this
scenario at `Settle`, or the pin covers a path production no longer takes.

Note this is what makes the `OutcomeCancelled` refusal coherent rather than
arbitrary: the latch is the *only* thing that authorises `Cancelled`. A caller
may not pass it, and a caller does not need to. The refusal is a pure check on
an argument, so it returns before `q.mu` is taken and before any snapshot is
read.

**`Settle` does NOT guard on `running`,** and that is deliberate rather than an
oversight — see the organising rule and its mechanical form: a worker finishing
normally at `Fetching` still satisfies all three conjuncts of `running`, so the
guard would reject exactly the call the door exists for.

**Alternatives considered.**

- *Workers report an outcome; `Advance` settles.* Needs a pending-verdict field
  on the `Job` that something other than `Finish` writes — a second writer of
  the outcome, against D-I3 and Rule 2. Rejected.
- *Export `reclaim`, let the dispatcher call `j.Finish` then `q.Reclaim(l)`.*
  This is the two-call coordination §3.5 rejected for `Cross`, in the same words:
  forgetting the second call leaks a pool-A slot permanently and silently.
  Rejected.

## Decision 2 — export `park` as `Park`, unconditional

**Proposal:** `func (q *Queue) Park(j *job.Job) error` — the existing `park`,
exported unchanged, with a rewritten doc comment. `parkLocked` stays unexported.

**No gate check, and this reverses the draft this RFC started as.** That draft
proposed refusing a non-gated job with `errNotGated`, by analogy to `Retry`'s
`errNotSettled`. The analogy is wrong. `errNotSettled` guards a *user-initiated*
door against a live worker; `gatedBy` cannot do that job, because it reads
`Intent` and `q.paused` and consults nothing about worker liveness. A gated job
whose worker is still mid-article passes the check and gets stripped anyway,
while legitimate non-gate returns — teardown, shutdown, a worker whose
connection died — are refused. It protects against nothing and forbids
something real.

**Unconditional is safe, and this was probed rather than argued.** A throwaway
test against `0549d7cd` ran `park` on all four shapes an unconditional door can
be handed — a never-run job, a job already parked, a settled job, and a job
mid-crossing with `next` set. All four returned no error, released nothing
twice, and left `next` intact. The reason is structural: `slotPool.release` is a
map delete, `Surrender` returns `nil` when nothing is held, and `reclaim`
no-ops on `nil` — the last of which §3.9 introduced for the paused-then-crossing
case, and which incidentally makes this door total.

**The name stays `park`, and that was not free.** An intermediate draft proposed
renaming it `Yield`, on the grounds that a door's name should state the caller's
precondition ("my worker has returned") rather than the Queue's effect. Rejected
on two counts:

- **`yield` is already a term of art in this subsystem, for the opposite
  direction.** `grep -in 'yield'` over the spec returns 19 lines, and the
  dominant sense is the lease: §3.5's door table has a column headed *"Yields
  the lease"*, D-I13 reads *"every door that can end the need for a lease yields
  it"*, and Half A's tests are named `TestJob_CrossYieldsTheLeaseAtomically`. So
  `Cross`, `Finish` and `Surrender` are the doors that *yield*, and a
  `Queue.Yield` would be the door that *receives* what they yield. §3.6 already
  strains under the double meaning, using both senses in one paragraph and
  needing the corrective clause *"that yield is not a completion and must not be
  reported as one"*.
- **Naming a method after its caller's precondition is not a Go convention.** A
  method names what it does to its receiver; the precondition belongs in the doc
  comment, which is where this RFC puts it anyway. `Park` also pairs correctly
  against `Settle` — parked is set down and resumable, settled is ended — which
  a rename would lose.

The rename would also have cost ~121 touch points: 19 spec occurrences, 22
non-test lines in `internal/sched` (including the door enumerations at
`queue.go:41` and `pool.go:51`), 80 test lines, 8 test identifiers, and one
**live citation** — `advance.go:48` embeds `` `grep -n 'q\.parkLocked(' advance.go` ``
with a stated count of three, which `check_citations` verifies on every run.

**What must change is the doc comment, not the name.** It currently opens
*"park releases what a gated job must not keep holding"*, which embeds the gate
assumption this decision removes. Under an unconditional door the gate is one
reason among several, so the precondition replaces it — stated explicitly, as
`Workers.Abort`'s comment already does for its own unenforceable constraint:

> The caller's worker for j has returned and will not touch the job's lease,
> slot, manifest or barrier again. This CANNOT be checked here — running()
> stays true for a worker that has yielded and not yet been parked, which is
> exactly why this door exists — so it is the caller's to guarantee.

## Decision 3 — `Pause`, `Resume`, `Paused`

**Proposal:** `Pause()`, `Resume()`, and `Paused() bool`, each taking `q.mu`.
Two verbs rather than `SetPaused(bool)`, so no call site reads as a blind
boolean; the getter because `/api?mode=queue` must render the global-pause
state and `q.paused` is otherwise unreadable from outside.

**`Pause` sets the flag and nothing else, and this is the interesting part.**
The `Queue` holds no jobs: its fields are `mu`, `paused`, `leases`, `slots`,
`clock`, `work` — two pools keyed by `job.LeaseID` and job ID, with no registry
and no way to enumerate what is resident. So `Pause` structurally cannot sweep
running workers; per Decision 2's probe, the flag alone releases nothing for any
mid-state job until its holder returns and the dispatcher yields it.

That is the right split rather than a gap to fill — see Decision 5 — but it is a
**contract on B2's dispatcher** that must be written down: after `Pause`, the
dispatcher awaits its workers and calls `Park` per job.

**It awaits them; it never aborts them.** §8.3 is explicit that *gating never
interrupts work*, and `Workers.Abort` is Cancel's alone. A `Fetching` worker
checks the gate at an article boundary and reports yielded (§3.6's `yielded`
contract); a worker in any other state runs its stage to the end and sets
`next`, after which `Advance`'s branch 3 gates and parks it without help. An
earlier revision of this RFC wrote "aborts or awaits", which would have licensed
exactly the interruption §8.3 forbids.

**No notification channel is needed**, and the spec already answers this for the
mirror case: §3.6's *"Resume needs no notification — `SetIntent(IntentRun)`
writes a flag; the scheduling loop calls `advance` on its ordinary cadence and
picks it up."* Pause is symmetric. The API handler calls `q.Pause()`, the
dispatcher's next tick observes it through `gatedBy`, and no `sync.Cond` or
channel enters the Queue. What B2.3 must decide is only how a `Fetching` worker
learns of it mid-job — `q.Paused()` at an article boundary is the obvious
answer, and it is why Decision 3 exports the getter.

If the contract is left implicit, global pause silently becomes "no new work
starts" while every in-flight download runs to completion holding pool A.

## Decision 4 — one read door, returning `job.RenderView`

**Proposal:** `func (q *Queue) Render(j *job.Job) job.RenderView` — one method,
one `q.mu` span, one `j.Snapshot()`, filling all four fields. `waitReason`,
`running`, `holds` and `gatedBy` stay unexported.

The first draft proposed exporting `WaitReason` and `Running` as separate
predicates. That is wrong twice over, and the type the repo already has says so.

**`waitReason`'s `(0, false)` is three-ways ambiguous.** Reading `queue.go:174`,
it returns "no reason" when the attempt is settled, when the job is running, and
when work has finished and the job already holds what `Next` requires. A caller
handed only that cannot fill `RenderView.Running`, so an exported `WaitReason`
without an exported `Running` ships a package the renderer still cannot use —
which is exactly what Problem §4 says must not happen.

**Two exported predicates are two lock acquisitions and a tear.** A renderer
calling `q.Running(j)` and then `q.WaitReason(j)` takes `q.mu` twice and
`j.Snapshot()` twice, and a transition landing between them yields a view that
was never true at any instant. That is the precise tear `job.Snapshot` was built
to remove (`job.go:539-545` argues it for `IsOpen`), reintroduced one layer up.

One door closes both. `RenderView`'s own doc comment already designates it as
the seam and says Half B fills it; filling it in one call under one lock is what
that sentence asks for.

**`RenderAll(jobs []*job.Job) []job.RenderView` is accepted, but lands in B2.4
rather than B2.1.** `/api?mode=queue` renders every resident job, so a
per-job `Render` takes `q.mu` once per job. The reason is not the one first
offered in review, though: cross-job tearing is bounded to
`MaxActiveJobs` — default **4** (`internal/config/defaults.go:79`) — because
every other resident job is at `StateUnset` and has nothing to tear. The real
cost is N acquire/release cycles on the scheduler's hot lock.

That trade runs both ways — one span is cheaper in total but holds `q.mu`
longer, delaying a concurrent `Advance` — and which side wins depends on whether
the renderer walks all resident jobs or a page. B2.4 repoints the API and is
where that is known, so it takes the door with its consumer in hand. `sched`
supplying no job list of its own (D-B5) is unaffected: the caller passes the
slice.

**Pool utilisation** (`LeasesOutstanding`, `SlotsOutstanding`) is deliberately
**not** proposed. Nothing in §4.4 consumes it, and `outstanding()` currently has
no caller outside this package's own tests. If B2.3 wants telemetry it can be
added there, with a named consumer.

## Decision 5 — what `sched` does *not* get

**Proposal:** `internal/sched` acquires no job registry, no residency, no store,
and keeps `internal/job` as its only dependency.

Stated because Decisions 1–4 each create pressure the other way — `Pause` would
like to sweep, `Settle` would like to persist, `Render` would like a list to
render — and because the alternative is how the package this replaces reached its
current size. `docs/queue-lifecycle.md` already assigns residency to the
`ActiveSet`; the dispatcher owns it in B2.

---

## What this means for B2's shape

B2 cannot be one plan, but it is smaller than the raw figure suggests.
`internal/queue` is 27,204 lines across 99 files — of which **7,831 lines in 15
files are non-test** (`queue.go` 2,211; `sqlite_store.go` 1,605; `job.go` 1,024;
`progress.go` 940), and `*Queue` carries 71 methods. The other 84 files, and
roughly 19,000 lines, are tests.

The production coupling is thinner still. `grep -lc 'queue\.Queue'` across every
file importing the package finds **five** that are not `_test.go` —
`internal/api/server.go`, `internal/api/apitest/nopapp.go`, `internal/app/app.go`,
`internal/app/pipeline.go`, `internal/downloader/downloader.go` — against 21
test files. **The swap's risk is concentrated in test rewriting and persistence,
not in production call sites.**

Proposed decomposition, for discussion:

| PR | Contains | Depends on |
|---|---|---|
| **B2.1** | `Settle` + `settleLocked` extraction, `Park`, `Pause`/`Resume`/`Paused`, `Render`. Still imported by nothing. | this RFC |
| **B2.2** | Persistence of `State`, `Next`, `Outcome`, `Intent`, `crossed` — a new `goose` migration | B2.1 |
| **B2.3** | The dispatcher and the composed view together: `Workers` implementation, residency, worker yield → `Park`, tick → `Advance`, §4.4's `ToSABnzbd` inputs | B2.1, B2.2 |
| **B2.4** | The swap: repoint the five production files, rewrite tests, delete `internal/queue` | all |

The view and the dispatcher land together because the dispatcher is what
populates the view, and splitting them would mean defining view types in one PR
that nothing fills until the next.

## Carried forward — open, not decided here

1. **§8 question 3 — does the dispatcher need `next`?** Recorded in the spec as
   "to be checked when Half B is written". B2.3's question, not this one.
2. **§8 question 4 — does pausing change queue position?** §4.7 says a paused
   never-started job takes effect at the next lease issuance, but never says
   whether pausing itself reorders. Probably not; B2.3 confirms.
3. **§4.7's reorder table has no row for a settled attempt.** Its rows assume
   unsettled positions, and its last row — "`Extracting`, `Finalizing`,
   `Finished` → never, the job will not fetch again" — is now wrong for a
   *settled* attempt at any position, since `Retry` reopens one at `Fetching`.
   **Proposed row, from review:** *reordering a settled job is recorded
   immediately and takes effect only if `Retry` reopens it, at that new
   position's next lease issuance.* It holds neither a lease nor a slot, so
   nothing else can be affected. This is consistent with reorder remaining total
   and unconditionally recorded (§8.1.1); folding it into §4.7 needs a spec
   edit rather than a code change.
4. **`q.discard`, and a rendering hole it leaves open.** `finishCancel` returns
   `nil` for a never-run job and names `discard` as B2's, with the store. Review
   traced what that costs today, and it is worse than a missing feature: a
   cancelled never-run job **renders as `StatusQueued` forever**. `gatedBy`
   deliberately ignores `IntentCancel`, `waitReason` returns `NoLease` for
   `StateUnset`, `NoLease.IsPause()` is false (`wait.go:59`), and
   `sabnzbd.go:53-63` therefore returns `StatusQueued`. The user deletes a
   queued job and it stays, looking active.

   Note this also puts a hole in `gatedBy`'s own justification — *"IntentCancel
   is absent deliberately: advance handles it first, so no cancel value reaches
   the render path"* (`queue.go:161`). For a never-run job `finishCancel` does
   nothing, so the job survives and a cancel value reaches the render path
   after all. **B2.3's dispatcher must evict `StateUnset && IntentCancel` from
   residency and the store**, and that is a contract this RFC records rather
   than a defect `sched` can fix alone: with no registry (D-B5), `Cancel` has
   nothing to remove the job from.
5. **The failed-`Transition` slot release in `Advance` is unpinned.** Reaching
   it needs a concurrent settle; recorded on #448 rather than dropped. `Settle`
   makes it reachable from a test for the first time — a settled attempt refuses
   `Transition` — so B2.1 should pin it rather than carry it further.

## Recommendation

| | Recommendation |
|---|---|
| **D-B1** | Add `Settle(j, o)`; extract `settleLocked` as the sole settle path; `finishCancel` calls it. It refuses `OutcomeCancelled` from a caller, and *produces* it when the latch is set **and the state is pre-boundary** — post-boundary, §8.4's gate means the worker's own outcome stands (D-I11). The split is named once as `cancelInterrupts` and read by both `finishCancel` and `settleLocked`. No `running` guard. |
| **D-B2** | Export `park` as `Park(j)`, unconditional, with its unverifiable precondition replacing the gate framing in its doc comment. No gate check, no rename. |
| **D-B3** | Add `Pause()`/`Resume()`/`Paused()`; they set and read the flag only, with the sweep as a documented dispatcher contract. |
| **D-B4** | Export one read door, `Render(j) job.RenderView`, computed in a single lock span. No exported predicates, no pool telemetry. `RenderAll` lands with its consumer in B2.4. |
| **D-B5** | `sched` gains no registry, store, or dependency beyond `internal/job`. |
| **D-B6** | B2 lands as four PRs, B2.1 first; the dispatcher and the composed view land together. |
