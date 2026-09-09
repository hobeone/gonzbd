# The dispatch/sched contract

`internal/sched` is a decision core over a single job: which jobs may run,
what they are waiting for, and what moves them. `internal/dispatch` drives it:
it owns the job registry in queue order, manifest residency, the tick loop,
and worker lifecycle. `internal/dispatch` imports `internal/sched` and
`internal/job`; `internal/sched` imports only `internal/job`. The dependency
points one way — `internal/sched` has no registry, no store, and no knowledge
that a dispatcher exists (verified: `internal/sched`'s own import block names
neither `internal/dispatch` nor `internal/job`'s callers).

`internal/queue` does not exist in this codebase. It was the predecessor this
design replaced; both packages below are its permanent home. There is no
migration and no "old jobs behave differently" branch — a persisted row from
before either package existed is simply a row this design has never seen.

## What sched owns

A `sched.Queue` holds two admission pools and a pause flag: `mu`, `paused`,
`leases *leasePool`, `slots *slotPool`, `clock`, `work Workers`
(`internal/sched/queue.go`). It holds no jobs, no registry, and no way to
enumerate what is resident. Its exported surface —
`git grep -n '^func (q \*Queue) [A-Z]' internal/sched/*.go | grep -v _test.go`
returns 13 lines — is `Advance`, `Cancel`, `Park`, `Retry`, `Settle` (write or
gate), `Pause`, `Resume`, `Paused`, `SetCaps`, `LeaseCap`, `SlotCap` (pool
management), and `Render`, `RenderAll` (the two rendering doors). Of those
13, 7 reach a `*job.Job` method call — `internal/job/job.go`'s comment
enumerates them (Cancel, Park, Retry, Advance, Settle, Render, RenderAll) and
`TestQueueDoorsReachingJob_MatchTheEnumerationStatedInProse`
(`internal/sched/lock_enumeration_test.go`) checks it; the remaining six
(`Pause`, `Resume`, `Paused`, `SetCaps`, `LeaseCap`, `SlotCap`) touch only the
Queue's own fields.

Every decision is a function of a `job.Snapshot` — a value taken once under
`Job.mu` — so no decision can acquire a resource as a side effect of being
asked. Resource acquisition happens in exactly one place, `grantFor`;
resource return happens only through `reclaim` (pool A, the sole reclaimer)
and `releaseFor` (pool B, the sole releaser), both of which `Settle`, `Park`
and `Cancel` route through.

## What dispatch owns

`internal/dispatch.Dispatcher` owns the `*sched.Queue` outright: it
constructs it in `New` and nothing else holds a reference (`sched.New` is
called from exactly one production site, `dispatch.New` —
`git grep -n 'sched.New(' -- '*.go' | grep -v _test.go` returns one line).
This is forced by the eviction described below, not a matter of taste: only
the dispatcher has a registry to evict a job from, so if a caller could reach
`sched.Cancel` directly, that path would latch the cancel intent and skip the
eviction, reintroducing a job that renders as queued forever through a
second door.

The Dispatcher additionally owns:

- **The job registry in queue order** (`byID`, `order`, `nextSeq`) —
  `internal/dispatch/registry.go`.
- **Manifest residency bookkeeping** (`resident`) and the `Residency`
  interface (`Hydrate`/`Evict`) that does the actual I/O.
- **The tick loop** (`run`/`tick` in `internal/dispatch/tick.go`).
- **Worker lifecycle**: `launched`, the `Runner` interface, and the
  `Finished`/`Yielded` exit doors (`internal/dispatch/worker.go`).
- **Persistence bookkeeping** (`written`, the `Store` interface) and the
  removal-in-progress marker (`removing`, `occupiers` et al.) that makes
  teardown safe under concurrent callers.

`internal/dispatch` is imported by exactly one production file today —
`internal/app/app.go:375` calls `dispatch.New`, and `internal/app/reloader.go`
calls `dispatch.(*Dispatcher).SetCaps`.

## The tick: a ticker owns liveness, the kick is an optimisation

A single goroutine (`Dispatcher.run`) walks the registry on a `time.Ticker`
and calls `sched.Queue.Advance` on each job. `Dispatcher.kick` performs a
non-blocking send on a size-1 buffered channel (`wake`) to wake the loop
early; `Add`, `Cancel`, `Retry`, `Pause`, `Resume`, `SetCaps`, `Finished` and
`YieldedFor` all call it.

The ticker alone is sufficient for correctness — deleting the kick would only
add latency, not change what the system computes, because both routes call
the same `run` iteration, which calls the same `tick`. A single goroutine
means ticks cannot overlap and need no locking between them, which is also
why the registry carries no `promoting`-style marker: there is no second
goroutine for such a marker to guard against.

**The tick alone cannot discharge every liveness contract.** It supplies
*when* to ask, not the one fact `sched.Queue` structurally cannot know:
whether a worker is still working. `Advance`'s branch 2
(`internal/sched/advance.go:210-221`) tests `q.holds` before `q.gatedBy` and
returns early when a job still holds its resources, because the Queue cannot
distinguish "holding and working" from "holding and yielded", and stripping
a live worker's lease is the worse failure. That is why the dispatcher must
also own the worker exit path (below) — without it, `q.work.Abort` on a
cancelled job returns but surrenders nothing, `HoldsLease` stays true, and
every subsequent tick re-aborts it while the job never settles.

## Worker lifecycle: the dispatcher launches every worker and observes every exit

`Dispatcher.launch` (`internal/dispatch/worker.go`) starts a worker when
`sched.Queue.Render` reports the job `Running` with `Intent == job.IntentRun`,
via `claimLaunched` (a map insert guarding against a second launch) and the
injected `Runner.Run`. It re-reads the snapshot at launch time rather than
trusting the tick's own snapshot: between `Advance` granting resources and
`launch` running, the manifest read happens unlocked and a concurrent
`Cancel` can have latched `IntentCancel` in that window. Launching anyway is
not a correctness failure — the next tick's `Advance` routes the cancel
through `finishCancel` and aborts it — but it starts work the user has
already cancelled.

On worker exit, the runner (or an external caller) must call exactly one of:

- **`Dispatcher.Finished(id, outcome)`** — the worker finished the state's
  work, terminally. It rejects `job.OutcomeCancelled` before touching the
  Queue (only the cancel latch may produce that outcome — `sched.Settle`
  refuses it too, via `ErrCancelReserved`), then calls `sched.Queue.Settle`.
- **`Dispatcher.Yielded(id)` / `YieldedFor(id, expected)`** — the worker
  stopped without finishing: a pause yield at an article boundary, an abort,
  a shutdown, a dead connection. It calls `sched.Queue.Park`.

`Park` is unconditional and total for every shape it can be handed — a
never-run job, an already-parked job, a settled job, a job mid-crossing —
because `slotPool.release` is a map delete, `Surrender` returns nil when
nothing is held, and `reclaim` no-ops on nil. The dispatcher therefore never
has to decide *whether* a yielding worker still holds something; it calls
`Park` and totality makes that correct at every shape.

In both `Finished` and `Yielded`, the launch claim (`clearLaunched`) is
cleared **after** the `sched` call and **before** `kick`: clearing first
would let a concurrent tick's `launch` observe the job still `Running` (the
Queue state hasn't moved yet) with the claim already free, and start a
second worker on resources the first has not yet released; clearing via
`defer` would let `kick` wake the tick before the claim clears, letting the
woken tick consume the wake without launching and leave the job unworked
until the next timer tick.

## Manifest residency is derived from pool membership

`manifestResident(j) ⟺ q.holds(j)`. `Dispatcher.reconcileResidency`
(`internal/dispatch/tick.go`) calls `sched.Queue.Render` once and reads
`v.Holds` — the field `renderLocked` computes from `q.holds(id, s)`, i.e.
"has every resource the job's current position requires" — and hydrates
(`Residency.Hydrate`) when a job holds but is not yet resident, or evicts
(`Residency.Evict`) when a job is resident but no longer holds.

Only the manifest tier is evictable: nothing drops a `JobProgress` once it
exists, and header fields never leave. That is weaker than "resident for a
job's whole time in the registry", and deliberately so — a job restored at
startup has no `JobProgress` until first hydration, because `dispatch.restore`
rebuilds it with `job.New` and no content and this package has no database
access to size one. See `docs/job-lifecycle.md` for that window and the
`restored*` fields covering it.

What the dispatcher changes is only who computes manifest residency: it stops
being a set the dispatcher maintains independently and becomes a function of
the pools.

**The invariant holds at tick boundaries, not instantaneously.** `grantFor`
runs inside `Advance` under `Queue.mu`, so the dispatcher learns a job
acquired resources only after `Advance` returns, and `Hydrate` (disk I/O)
must run unlocked. A job can therefore hold a lease with no manifest for the
length of one disk read: nothing consumes that window, because a worker is
only launched after hydration succeeds, and a failed read settles the job
`Failed` in the same tick, returning both pools via `sched.Queue.Settle`.

A context cancellation during `Hydrate` is treated differently from every
other read failure: `reconcileResidency` checks `ctx.Err()` and the
`context.Canceled`/`context.DeadlineExceeded` sentinels first, and on a
match returns the error **without** settling. `Outcome` is write-once, so
settling a job `Failed` because the process happened to be shutting down
while its manifest was mid-read would be permanent and wrong — the user
would find a healthy job marked failed after a restart. Every other read
failure (a short read, a corrupt gzip stream, a missing file) is a fact
about the job — it can never run — so it settles `Failed` and frees both
pools.

`MaxActiveJobs` (`internal/config`) is not redundant: `internal/app/app.go`
passes it directly as the dispatcher's `leaseCap`, and
`internal/app/reloader.go` calls `Dispatcher.SetCaps` to resize it live on a
config reload. (One of the source specs for this document predicted it
would become redundant once pool capacities were the only knob; the code
that shipped kept it as the config-facing name for that knob instead.)

## Lock discipline across the dispatch → sched boundary

**The dispatcher never holds its own lock (`d.mu`) across a call into
`sched`.** `Dispatcher.tick` copies the registry under `d.mu` via
`snapshotOrder`, releases the lock, and only then calls `sched.Queue.Advance`
per job (`internal/dispatch/tick.go`). Every other call into `d.q` —
`Cancel`, `Retry`, `Pause`, `Resume`, `SetCaps`, `Park` in `Stop`'s sweep,
`Render`/`RenderAll` in `List`/`Row`/`reconcileResidency`/`launch`, `Settle`
in `Finished`/`reconcileResidency`, `Park` in `YieldedFor` — is likewise made
outside any `d.mu` span (verified: `grep -n 'd\.q\.' internal/dispatch/*.go
| grep -v _test.go` shows none of these calls nested inside a
`d.mu.Lock()`/`Unlock()` pair).

This discipline exists because `sched.Workers.Abort` runs **inside**
`Queue.mu`'s span — `sched.Queue.Cancel`'s interrupt arm calls
`q.work.Abort(j)` while holding `q.mu` — and `Abort`'s own contract
(`internal/sched/queue.go`) forbids it from acquiring any lock a caller
could hold across a call into `Queue`. If the dispatcher held `d.mu` across
`Advance` and a real `Workers.Abort` implementation also needed `d.mu`, a
concurrent `Cancel` calling `Abort` from inside `Queue.mu` would deadlock
ABBA against the tick's `Advance` call holding `d.mu` and waiting on
`Queue.mu`. Lock order overall: `dispatch.mu` → (released) → `Queue.mu` →
`Job.mu` — never held simultaneously in that first arrow.

`sched.Queue`'s own lock order is stated in `internal/sched/queue.go`:
`Queue.mu` is taken before any call into a `*job.Job` method (`Job.mu`
inside `Snapshot` or a mutator), never the reverse — enforced structurally
by `internal/job` importing nothing from `internal/sched`.

## The read doors: Render and RenderAll

`sched.Queue.Render(j)` composes a `job.RenderView` for one job under a
single `q.mu` acquisition and a single `j.Snapshot()`. `RenderAll(js)`
composes the same view for every job in `js` under **one** `q.mu`
acquisition, in the same order. Both delegate to the unexported
`renderLocked`, which is the sole computer of a `RenderView` — there is one
function that computes the view and two doors that differ only in how many
jobs they lock around.

The two-door split exists because a loop over `Render` would take `q.mu`
once per job, and a transition landing between two of those acquisitions
would yield a listing that was true at no single instant (job 3 rendered
`Downloading`, job 300 rendered `Queued`, when nothing ever held both at
once). `Dispatcher.List` (`internal/dispatch/registry.go`) calls
`RenderAll` exactly once per listing for this reason.

`Dispatcher.Row(id)` — the single-job lookup used where a caller needs one
job's status without paying for a full listing walk — calls the per-job
`Render` instead, deliberately: using `List` for a single lookup would trade
one manifest-free `RenderAll` call for an O(n) walk over the whole registry.

`RenderView` (`internal/job/render.go`) carries `StateView` plus `Running`,
`Reason`, `Intent` and `Holds` — the fields "nothing in `internal/job` can
answer", because they depend on pool-B slots and the queue-wide pause flag
that live in `sched.Queue`. `job.ToSABnzbd` consumes a `RenderView` to
render the legacy SABnzbd status vocabulary; `dispatch.Row.Status()`
(`internal/dispatch/status.go`) is the only non-test call site of
`constants.Status` inside `internal/dispatch` — a write-only rendering door,
not a second translation.

## The store interface: startup-read plus state-write

`internal/dispatch.Store` (`internal/dispatch/ports.go`) has exactly two
obligations:

- **`Load(ctx) ([]Persisted, error)`** — read the whole queue once, at
  `Dispatcher.Start`.
- **`Save(ctx, Persisted) error`** and **`Delete(ctx, id) error`** — write a
  job's four axes (`State`, `Intent`, plus header/policy/progress fields)
  when they move, and delete a row when the job is removed or evicted.

`internal/dispatch/store` implements this against SQLite; `internal/dispatch`
itself stays free of a SQL driver. `Persisted` deliberately omits a
`crossed` field: it is derivable from `State` via
`Attempt.crossed() = IsProduction(a.state)`, and persisting it would create
a second source of truth that could disagree with `State` after a restore.
`Persisted.Policy` is stored resolved (a `job.Policy`), not as the upstream
SABnzbd `PP` integer it derives from, because `PP` "does not exist past App"
and persisting it would carry external vocabulary back inside the internal
layer.

`Dispatcher.restore` rebuilds every job by replaying it forward through
`job.Job`'s own doors (`reconstruct`, `internal/dispatch/dispatch.go`) —
`job.New` followed by `BeginAttempt` and a canonical hop sequence
(`replayPath`) to the persisted `State` — rather than through a second
constructor. `internal/job` exports exactly one constructor
(`git grep -n 'func New(' internal/job/` returns one), and replaying instead
of adding a `job.Restore(...)` gets the state machine's own validation for
free: an illegal position, an inadmissible `Outcome`, or an illegal `Next`
is refused by the door itself rather than trusted into memory. Every
restored job comes back holding no lease and no slot regardless of the
position it was persisted at — the pools are process-local, so there is
nothing from a previous process to reclaim — and the first tick re-acquires
resources through the ordinary `grantFor` path, the same path a paused job
resumes through.

## Cancelling a never-run job: the dispatcher evicts what sched cannot discard

`sched.Queue.finishCancel` returns `nil` for a job at `job.StateUnset`,
because `Outcome` lives on the `Attempt` and a never-run job has none — there
is nothing for `Finish` to settle. Left there, such a job survives in the
registry: `gatedBy` ignores `IntentCancel` by design (`Advance` handles it
first, so a cancel value is not supposed to reach the render path), so
`waitReason` returns `NoLease`, `NoLease.IsPause()` is false, and
`job.ToSABnzbd` renders `StatusQueued`. A job the user deleted would render
as queued, forever.

`Dispatcher.tick` closes this: after `sched.Queue.Advance`,
`evictCancelledNeverRun` (`internal/dispatch/tick.go`) removes any job with
`State == StateUnset && Intent == IntentCancel` from the registry and the
store. `Advance` routes `IntentCancel` to `finishCancel` before every other
branch, so eviction cannot race a settle, and it frees no pools —
`TestRequirements_StateUnsetRequiresNothing`
(`internal/sched/requirements_test.go`) pins that `StateUnset` requires
neither a lease nor a slot. `sched` cannot close this gap itself: it has no
registry to remove a job from (see "What sched owns" above), which is why
the eviction lives in `internal/dispatch` rather than in `sched`.

## Cancel: interrupt before the boundary, gate after it

`sched.Queue.Cancel` latches `job.IntentCancel` and calls `finishCancel`,
which behaves differently depending on whether the job's current state is
pre- or post-*production boundary* (`job.IsProduction`): `cancelInterrupts(s)
= !job.IsProduction(s)` (`internal/sched/cancel.go`) names the split, and it
is read by both `finishCancel` (choosing which arm to take) and
`settleLocked` (deciding whether a cancelled job's recorded outcome is
"our artifact" or "the worker's own"), so the two do not risk disagreeing
about where the line falls.

- **Pre-boundary and running** (`Fetching`, `Assessing`, `Repairing`):
  `finishCancel` calls `q.work.Abort(j)` and returns without settling — the
  job settles later, when the worker's exit reaches `Settle`/`Park`.
  `settleLocked` overrides whatever outcome that later call passes to
  `job.OutcomeCancelled`, because the worker's error in this case is the
  cancellation's own artifact.
- **Post-boundary** (`Extracting`, `Finalizing`) or **not running**:
  `finishCancel` settles directly via `settleLocked(j,
  job.OutcomeCancelled, s)`, but the override in `settleLocked` does not
  fire for a post-boundary job — its own outcome, if it later reports one
  through some other path, stands. This is what lets a running `Finalizing`
  job that completes normally after being cancelled settle `OutcomeOK`
  rather than falsely `Cancelled`: the files have already moved and the
  script has already run, so recording `Cancelled` would misdescribe what
  is on disk.

`sched.Queue.Settle` refuses a caller-supplied `job.OutcomeCancelled`
(`ErrCancelReserved`) before taking any lock — only the cancel latch may
produce that outcome, because `Cancel` latching the intent is what makes a
later `Retry` re-cancel a reopened attempt (`Advance`'s
`s.Intent == IntentCancel` route). A worker calling `Settle` with
`Cancelled` directly would skip that latch and open a resurrection path.

## What sched does not get, and why

`internal/sched` acquires no job registry, no residency, and no store, and
depends on nothing beyond `internal/job`. This is deliberate: `Pause` would
like to sweep running workers, `Settle` would like to persist what it
settles, and `Render` would like a list of jobs to render, and giving in to
any of those pressures is how the package this design replaced grew large.
`sched.Queue.Pause` sets the flag and nothing else — the Queue structurally
cannot enumerate what is resident to sweep, and even if it could, gating
must never interrupt work (`Workers.Abort` is `Cancel`'s alone). The
contract this places on the dispatcher: after `Pause`, a caller awaits its
workers' yields and calls `Park` per job as they arrive: a `Fetching`
worker checks `Paused()` at an article boundary and yields; a worker in any
other state runs its stage to completion, sets `Next`, and `Advance`'s
branch 3 gates and parks it unaided on the next tick (Finalizing has no
outbound edges and never sets `Next`, so a Finalizing worker always settles
directly rather than being parked).

## What neither package gets

- **No production wiring for a second `Dispatcher`/`Queue` per process.**
  `internal/app/app.go` constructs exactly one `Dispatcher`
  (`grep -rln 'dispatch\.New(' --include='*.go' internal/ cmd/ | grep -v
  _test.go` returns one file), and `sched`'s lease-identity audit
  (`internal/sched/pool.go`) is scoped to one `leasePool`'s own issuance —
  two `Queue`s sharing one `*job.Job` is not a configuration anything
  constructs today.
- **No pool-utilisation telemetry** (`LeasesOutstanding`, `SlotsOutstanding`)
  is exported from `sched`; nothing consumes it.
- **No per-job cancellation surface in `internal/downloader`.** Cancel's
  interrupt arm calls `Workers.Abort`, whose production implementation is
  `appWorkers` (`internal/app/dispatcher_wiring.go`); `internal/postproc`
  already exposes `PostProcessor.Cancel(jobID)` for `Assessing`/`Repairing`,
  but a `Fetching` worker has no equivalent per-job stop today (only global
  pause/stop/disconnect on the downloader).

## Open question

The two source specs disagreed with each other, and with the shipped code,
about whether `MaxActiveJobs` would become a redundant config field once
pool capacities were the only concurrency knob. The code kept it as the
live-resizable knob feeding `leaseCap` (see "Manifest residency" above), so
that question is settled by what shipped rather than left open — recorded
here only because a reader of the superseded specs would otherwise expect
the field to have been retired.
