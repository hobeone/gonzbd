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
returns 14 lines — is `Advance`, `Cancel`, `Park`, `Handoff`, `Retry`, `Settle`
(write or gate), `Pause`, `Resume`, `Paused`, `SetCaps`, `LeaseCap`, `SlotCap`
(pool management), and `Render`, `RenderAll` (the two rendering doors). Of
those 14, 8 reach a `*job.Job` method call — `internal/job/job.go`'s comment
enumerates them (Cancel, Park, Handoff, Retry, Advance, Settle, Render,
RenderAll) and
`TestQueueDoorsReachingJob_MatchTheEnumerationStatedInProse`
(`internal/sched/lock_enumeration_test.go`) checks it; the remaining six
(`Pause`, `Resume`, `Paused`, `SetCaps`, `LeaseCap`, `SlotCap`) touch only the
Queue's own fields.

Every decision is a function of a `job.Snapshot` — a value taken once under
`Job.mu` — so no decision can acquire a resource as a side effect of being
asked. Resource acquisition happens in exactly one place, `grantFor`;
resource return happens only through `reclaim` (pool A, the sole reclaimer)
and `releaseFor` (pool B, the sole releaser), both of which `Settle`, `Park`,
`Handoff` and `Cancel` route through.

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
  `Finished`/`Yielded`/`AdvanceFrom`/`YieldedFrom` exit doors
  (`internal/dispatch/worker.go`).
- **Persistence bookkeeping** (`written`, the `Store` interface) and the
  removal-in-progress marker (`removing`, `occupiers` et al.) that makes
  teardown safe under concurrent callers.

`internal/dispatch` is **constructed** from exactly one production call site —
`dispatch.New` appears once outside tests, at `internal/app/app.go:375`
(`git grep -n 'dispatch\.New(' -- '*.go' ':!*_test.go'`). The package itself is
imported far more widely: 12 non-test files, across `cmd/gonzbd`, `internal/api`,
`internal/app`, `internal/downloader` and `internal/dispatch/store`. It is the
single constructor that matters here, not a single importer — a `Dispatcher`
nobody else can build is what makes its lifecycle statements hold.

## The tick: a ticker owns liveness, the kick is an optimisation

A single goroutine (`Dispatcher.run`) walks the registry on a `time.Ticker`
and calls `sched.Queue.Advance` on each job. `Dispatcher.kick` performs a
non-blocking send on a size-1 buffered channel (`wake`) to wake the loop
early; `Add`, `Cancel`, `Pause`, `Resume`, `SetCaps`, `Finished`,
`YieldedFor`, and `handoff` for `AdvanceFrom` and `YieldedFrom` all call it.

**Why a channel and not a `sync.Cond`.** The two are interchangeable for
"wake a waiter", and `sync.Cond` is the closer fit for the literal signalling
pattern — which is why the question keeps coming back. The reason it loses is
structural rather than stylistic: `run` does not wait for one condition, it
`select`s over three sources — `d.stop`, `d.wake`, and the ticker — and
**`sync.Cond` cannot participate in a `select`**. A `Cond` waiter blocks in
`Wait` and can be woken by nothing else, so shutdown and the periodic tick
would each need their own mechanism layered on top.

Two properties fall out of the channel that would have to be rebuilt by hand
around a `Cond`:

- **A burst collapses.** The buffer is size 1 and the send is
  `select`/`default`, so ten `Add`s in a row produce one wakeup, and a full
  buffer means a wakeup is already pending. `Cond.Signal` on a non-waiting
  goroutine is simply lost, which is the opposite failure.
- **Shutdown can be given priority.** Because all three are channels, `run`
  can drain `d.stop` in a non-blocking pass *before* the blocking `select`,
  which is what stops a ready `d.stop` and a primed `d.wake` from being decided
  by `select`'s uniform random choice. That fix has no expression in `Cond`.

`Dispatcher.Notify` hands out the receive end of a second size-1 channel for
external observers, on the same terms.

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

The `Running` check that decides is the one made **after** `claimLaunched`
succeeds; a failed re-check clears the claim and launches nothing. An exit
report that has fully landed between a check and the claim has already parked
the job and cleared a claim that did not yet exist, so a claim taken on the
strength of the earlier check has no report left to clear it: the job holds
its resources with no worker, and no tick launches it again until a removal
or `Stop` clears the claim. For a job that is not running, the check before
the claim only saves a tick from claiming and releasing it; for a running job
whose intent is no longer `IntentRun`, it is also where the grant is returned
(below). `TestLaunch_ReportBeforeClaimLeavesNoStrandedClaim` pins the re-check.

The re-check is sound on two branches. Every exit report changes what
`Render` returns before it calls `clearLaunched` (`Settle` closes the
attempt, `Park` drops the lease or slot, `Handoff` records `Next` and parks),
so a report that landed before the re-check reads as not `Running`. And
`Advance` and `launch` both run only from `tick`, which never overlaps itself,
so nothing re-grants the job between that report and the re-check.

**A grant with no worker is given back.** A job that reads as `Running` holds
its lease or slot, and `sched` cannot tell a job that was granted from one
that is working, so no later `Advance` parks it. A cancel, a per-job pause of
a job not at `Fetching` (`PauseJob` parks one at `Fetching` itself), or a
removal that lands between the grant and the start of the worker leaves the
job holding with no worker to report for it. Two points return that grant:
- `launch`'s first check, for a running job whose intent is no longer
  `IntentRun` and which holds no launch claim. A cancel or pause landing after
  that check is caught by it on the next tick.
- `removeFor`, after `waitLaunched` and `waitLive` and before deregistering.
  A removed job is visited by no later tick, and the removal can also land
  where the tick never reaches `launch`, as when `reconcileResidency` fails.

A held claim means a worker owns the grant, and it is left alone. Once parked,
a cancelled job settles `Cancelled` on the next tick's `finishCancel`, as any
non-running job does after the boundary. A queue-wide pause leaves every
job's intent at `IntentRun`, so it declines no launch.
`TestLaunch_DeclinedLaunchReturnsTheGrant` and
`TestLaunch_DeclinedLaunchLeavesALiveWorkersGrant` pin it.

**A report of finished work is one call, scoped to the state it reports
from.** `Dispatcher.AdvanceFrom(j, from, next)` records `Next`, parks the job
and clears its claim inside one `sched.Queue.Handoff` span, and does nothing
(`ErrStaleReport`) unless the job is still open at `from` with no `Next`. The
app's two kinds of report use it. Download complete (`Fetching → Assessing`)
is made by `Application.reportDownloadComplete` (`internal/app/app.go`), which
`completeFinalizedFile` calls when the last file completes and
`appRunner.runFetch` (`internal/app/runner.go`) calls for a job already
complete when its Fetching worker launches; if both are made, the second is
stale and changes nothing. It makes no report for a job admitted to
post-processing (`docs/post-processing-contract.md`). `runAssess`'s verdict is the other kind
(`internal/app/runner.go`). As two calls, `SetNext` then `Yielded` by ID, a
tick could land between them: it moved the job to `next` and launched that
state's worker, and the late `Yielded` parked that worker's resources and
cleared its claim, so the next tick launched the same state again.
`TestAdvanceFrom_LateReportLeavesTheNextStatesWorkerAlone` and
`TestAdvanceFrom_UnlaunchedReportLaunchesTheNextStateOnce` pin it.

`Dispatcher.YieldedFrom(j, from)` is the same door with no verdict: it parks
and clears the claim only while the job is open at `from`.
`Dispatcher.yieldPaused` uses it with `Fetching`. `PauseJob` (`Application.Stall`'s
included) reaches it through `pauseJob`, and `BlockUnwanted`, when it pauses, sets
the pause intent itself under `d.mu` and then calls `yieldPaused`: the downloader stops serving a paused job but
reports nothing, so without it a paused `Fetching` job kept its lease. A
storage fault can reach a job that has moved to `Assessing` — the assembler
routes a fault on its own goroutine, so one raised before the move can land
after it — and a by-ID `Yielded` there took the live assess worker's slot and
claim, so a resume launched a second one. A pause
therefore latches at any state and releases only a `Fetching` worker; any other
worker finishes and reports, and the pause gates the move.
`TestStall_LeavesALiveAssessingWorkerAlone` and
`TestPauseJob_FetchingJobFreesItsLease` pin it.

On worker exit, the runner (or an external caller) must call exactly one of:

- **`Dispatcher.Finished(id, outcome)` / `FinishedJob(j, outcome)`** — the
  worker finished the state's work, terminally. It rejects
  `job.OutcomeCancelled` before touching the Queue (only the cancel latch may
  produce that outcome — `sched.Settle` refuses it too, via
  `ErrCancelReserved`), then calls `sched.Queue.Settle`. `FinishedJob` does
  nothing and returns `ErrNotFound` unless `j` is the instance registered
  under its ID, so a worker holding a removed instance cannot settle a later
  one; `appRunner.runAssess` reports through it.
- **`Dispatcher.AdvanceFrom(j, from, next)`** — the worker finished the
  work of `from` and the job continues to `next`. It calls
  `sched.Queue.Handoff`. A `next` that `SetNext` refuses settles the job
  `OutcomeFailed` rather than parking it, which would relaunch `from` to
  report the same verdict again; no caller reaches that today.
- **`Dispatcher.YieldedFrom(j, from)`** — the worker for `from` stopped
  without finishing, and the job may since have moved on. It calls
  `sched.Queue.Handoff` with no verdict.
- **`Dispatcher.Yielded(id)` / `YieldedFor(id, expected)`** — the worker
  stopped without finishing: a pause yield at an article boundary, an abort,
  a shutdown, a dead connection. It calls `sched.Queue.Park`, whatever state
  the job is at, so it suits only a caller for whom the job cannot have left
  the worker's state: one holding that worker's launch claim, or one that
  latched cancel or a global pause first.

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
until the next timer tick. `AdvanceFrom` and `YieldedFrom` clear it inside
`Handoff`'s `Queue.mu` span instead, and only for the instance they report
on. That placement is defence in depth. While the claim taken for `from` is
held, `claimLaunched` already refuses `next`'s worker until the clear. The
span matters only when no such claim is held at the clear, either because
none was taken (after a stall's resume, or at startup) or because another
by-ID clearer released it in between. A tick could then launch `next`, and a
clear made after the span would drop that worker's claim. Inside the span
the job cannot leave `from`. No test pins the placement, because no seam
can interleave a tick there.

## Manifest residency is derived from pool membership and pause

A job is hydrated when it holds and is not resident, and evicted when it is
resident, does not hold, and is not paused. `Dispatcher.reconcileResidency`
(`internal/dispatch/tick.go`) calls `sched.Queue.Render` once and reads
`v.Holds` — the field `renderLocked` computes from `q.holds(id, s)`, i.e.
"has every resource the job's current position requires" — and `v.Intent`,
and hydrates (`Residency.Hydrate`) or evicts (`Residency.Evict`) accordingly.

A paused job keeps a manifest it already has: `PauseJob` returns a `Fetching`
job's lease while fetches it dispatched are in flight, and those land on the
manifest, and a resume launches without re-reading it. A pause adds no
hydration of its own: the hydrate arm requires `Holds` and an intent other than
`IntentPause`, so a paused job restored at startup is hydrated here only once a
resume lets it take a lease, and a paused job that still holds a compute slot
is not re-hydrated every tick after a residency fault parked it.
`TestPauseJob_KeepsThePausedJobResident`,
`TestResumeJob_ContinuesWithoutRehydrating`,
`TestRestore_PausedJobIsNotHydratedUntilResumed` and
`TestReconcileResidency_DoesNotRehydrateAPausedSlotHolder` pin the four.

Two loads happen outside this rule, both through `Dispatcher.LoadProgress`,
which hydrates a registered job with no progress and records the load so a
later tick evicts it once it holds nothing and is not paused:
`Application.hydratePausedJobs`, before the first tick, for every job restored
paused at `Fetching`, so its verified progress is reported — each such job
stays resident until it is resumed or removed, since the tick's eviction arm
skips `IntentPause` (`docs/durability-contract.md`, Accepted limitation 1);
and `SetName`, so a rename sees whether the job's download has begun. A hydration that cannot verify the job's files
returns an error wrapping `ErrResidencyFault`, which no caller settles: the
job has been parked (`docs/durability-contract.md` §3).

Only the manifest tier is evictable: nothing drops a `JobProgress` once it
exists, and header fields never leave. That is weaker than "resident for a
job's whole time in the registry", and deliberately so — a job restored at
startup has no `JobProgress` until first hydration, because `dispatch.restore`
rebuilds it with `job.New` and no content and this package has no database
access to size one. See `docs/job-lifecycle.md` for that window and the
`restored*` fields covering it.

What the dispatcher changes is only who computes manifest residency: it stops
being a set the dispatcher maintains independently and becomes a function of
the pools and the job's intent.

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

**`markResident` gates on registration alone, never on `admitsLocked`.** A
`Remove` can begin while `Hydrate` runs, and the load that `Hydrate` completes
is real whatever the removal goes on to do, so an outstanding removal must not
suppress the record of it. A removal that succeeds takes the manifest with its
own `Evict` and the record with `deregister`. One that aborts leaves the job
registered, recorded resident and holding nothing, and the next tick's
eviction arm reclaims the manifest. Suppressing the record left that arm, which
evicts only a job `isResident` reports, blind to it, and the manifest stayed in
memory until a restart. The record admits no work: `occupyFor`, `claimLaunched` and
`persistIfChanged` still consult `admitsLocked`, and a resident job is one the
hydrate arm skips.
`TestResidency_AbortedRemoveDuringHydrateIsEvictedByTheNextTick` and
`TestResidency_SuccessfulRemoveDuringHydrateLeavesNothingResident` pin both
outcomes.

`MaxActiveJobs` (`internal/config`) is not redundant: `internal/app/app.go`
passes it directly as the dispatcher's `leaseCap`, and
`internal/app/reloader.go` calls `Dispatcher.SetCaps` to resize it live on a
config reload. (One of the source specs for this document predicted it
would become redundant once pool capacities were the only knob; the code
that shipped kept it as the config-facing name for that knob instead.)

## Lock discipline across the dispatch → sched boundary

**The dispatcher never holds its own lock (`d.mu`) across a call into
`sched`.** `Dispatcher.tick` copies the written registry entries (`d.written`)
under `d.mu` via `snapshotOrder`, releases the lock, and only then calls
`sched.Queue.Advance` per job (`internal/dispatch/tick.go`). Every other call into `d.q` —
`Cancel`, `Pause`, `Resume`, `SetCaps`, `Park` in `Stop`'s sweep,
`Render`/`RenderAll` in `List`/`rowFor` (for `Row` and
`RowJob`)/`reconcileResidency`/`launch`, `Settle` in `finishedFor` (for
`Finished` and `FinishedJob`)/`reconcileResidency`, `Park` in `YieldedFor` and in `parkGrant`
(for `launch` and `removeFor`), `Handoff` in
`handoff` (for `AdvanceFrom` and `YieldedFrom`) — is likewise made
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

The one nesting in the other direction is `Queue.mu` → `dispatch.mu`:
`handoff` (for `AdvanceFrom` and `YieldedFrom`) passes `Handoff` a callback that clears the launch claim, and
`Handoff` runs it inside its `Queue.mu` span. It is held to `Abort`'s rule,
and meets it for the same reason: `d.mu` is never held across a call into
`sched`, so no holder of `d.mu` can be waiting on `Queue.mu`.

`sched.Queue`'s own lock order is stated in `internal/sched/queue.go`:
`Queue.mu` is taken before any call into a `*job.Job` method (`Job.mu`
inside `Snapshot` or a mutator), never the reverse — enforced structurally
by `internal/job` importing nothing from `internal/sched`.

Above the dispatcher, `internal/app` keeps a per-job transition lock
(`jobTransitions`, `internal/app/transition.go`) that admits one retry,
finalization, queue removal or history change of a job at a time, except that
a finalizer proceeds without it after a bounded wait, or at once when the
application is stopping. Even then a retry of the job is refused for as long
as the finalizer commits: it records the job's ID for its whole run, and
`RetryHistoryJob` refuses a recorded ID when it claims the lock, and again
once it has found no instance registered under the ID and before it changes
anything. So a retry is refused while a finalizer of its ID commits at either
of those points, and a refused retry leaves the state the finalizer is filing
untouched (`TestPersistAndCommit_RefusesARetryWhileItCommits`,
`TestRetryHistoryJob_AFinalizerStartingAfterTheClaimKeepsItsState`). A
finalizer that begins after the second of those checks is a finalizer of an
instance a `RemoveJob` marked removed: `RemoveJob` keeps that mark on its
instance even when its `dispatcher.Remove` fails, and the finalizer returns
`errFinalizedJobRemoved` before any step that acts on the ID, so the retry
needs no later check of its own to stay clear of it. The `jobTransitions` doc
carries the argument
(`TestRetryHistoryJob_AFinalizerBetweenItsChecksLeavesItsState`,
`TestRetryHistoryJob_AFinalizerAfterItsLastCheckLeavesItsState`).
A holder may
wait inside the dispatcher: `Dispatcher.Remove` waits on the job's launch
claim. So nothing may wait for that lock while holding something the
dispatcher waits on. The finalizer is the site that runs holding one —
post-processing's launch claim — so it takes the lock after its own
`YieldedJob`, which clears that claim; every other site takes it before its
dispatcher calls. `jobFinalizer.cancelled`, which releases the claim of a
job `PostProcessor.CancelJob` took, also runs holding it but takes no
transition lock at all.
`TestJobTransitions_LockSites` pins the set of functions that take the lock.

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

`Dispatcher.Row(id)` and its instance-bound form `RowJob(j)` — the
single-job lookups used where a caller needs one job's status without paying
for a full listing walk — call the per-job `Render` instead, deliberately: using `List` for a single lookup would trade
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
  `Dispatcher.Start` (or `Dispatcher.StartWith`, which runs one caller-supplied
  step after the restore and before the ticker launches).
- **`Save(ctx, Persisted) error`** and **`Delete(ctx, id) error`** — write a
  job's row synchronously in `Dispatcher.Add` before `Add` returns (while
  `snapshotOrder` withholds the unwritten entry from `tick` via `d.written`) and
  whenever its four axes (`State`, `Intent`, plus header/policy/progress fields)
  move, and delete a row when the job is removed or evicted.

`d.written` is the whole of `snapshotOrder`'s gate, and there is deliberately no
second flag beside it: presence of a row is one fact, owned by `markWritten`,
which `persistIfChanged` calls inside the `storeMu` span that wrote the row.

**`markWritten` gates on registration alone, never on `admitsLocked`.** An
outstanding removal must not suppress the record, because the row is already on
disk by then and every `store.Delete` takes `storeMu` first. Suppressing it left
`d.written` silent about a real row, so a removal that then *aborted* stranded
the job — registered, persisted, and invisible to `snapshotOrder` forever.
Refusing a Save while a removal is outstanding is `persistIfChanged`'s job, and
is the single enforcement point for it.

**`d.removing` is a refcount, so it also gates admission.** `deregister` is
total — it clears `d.byID` even when it only decremented a marker another
removal still holds — so between an `Add` unwinding and the removal that
preempted it reaching its own `end()`, `d.byID` reports the ID free while
`d.removing` does not. `register` therefore refuses an ID with an outstanding
removal, returning `errPreemptedByRemoval`. Without that, a job admitted in
that span is reachable by a removal it has nothing to do with: that removal's
`Cancel` latches `IntentCancel` onto it, and its `end()` deregisters it.

`internal/dispatch/store` implements this against SQLite; `internal/dispatch`
itself stays free of a SQL driver. `Persisted` deliberately omits a
`crossed` field: it is derivable from `State` via
`Attempt.crossed() = IsProduction(a.state)`, and persisting it would create
a second source of truth that could disagree with `State` after a restore.
`Persisted.Policy` is stored resolved (a `job.Policy`), not as the upstream
SABnzbd `PP` integer it derives from, because `PP` "does not exist past App"
and persisting it would carry external vocabulary back inside the internal
layer.

`Dispatcher.restore` skips any stored row equal to `d.lastWritten(p.ID)` that
was already written by a pre-`Start` `Dispatcher.Add` on the same instance, and
rebuilds every other job by replaying it forward through
`job.Job`'s own doors (`reconstruct`, `internal/dispatch/dispatch.go`) —
`job.New` followed by `BeginAttempt` and a canonical hop sequence
(`replayPath`) to the persisted `State` — rather than through a second
constructor. `internal/job` exports exactly one constructor
(`git grep -n 'func New(' internal/job/` returns one), and replaying instead
of adding a `job.Restore(...)` gets the state machine's own validation for
free: an illegal position, an inadmissible `Outcome`, or an illegal `Next`
is refused by the door itself rather than trusted into memory. A row that
fails `rows.Scan` in `Store.Load`, or fails `reconstruct` or `register` in
`Dispatcher.restore`, is logged at `Error` with its job ID and skipped — left
untouched in `dispatch_jobs` and omitted from `d.written` so `persistIfChanged`
neither rewrites nor deletes it — while the remaining rows still restore
(Standing Design Rule 3); a query or cursor iteration failure in `Store.Load`
(or an error returned by `beforeFirstTick` in `StartWith`) still fails
`Start`/`StartWith`. Every
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
- **Post-boundary and running** (`Extracting`, `Finalizing`):
  `finishCancel` gates without interrupting (`!cancelInterrupts(s.State.State)`)
  and returns `nil` without calling `settleLocked` — the active worker owns the
  job's resources and runs to completion. When that worker later finishes and
  reports its outcome, the override in `settleLocked` (`s.Intent ==
  job.IntentCancel && cancelInterrupts`) does not fire for a post-boundary
  state, so the worker's own outcome stands. This is what lets a running
  `Finalizing` job that completes normally after being cancelled settle
  `OutcomeOK` rather than falsely `Cancelled`: the files have already moved and
  the script has already run, so recording `Cancelled` would misdescribe what
  is on disk.
- **Not running** (pre- or post-boundary — e.g. waiting on a lease or compute
  slot, or restored from a restart): `finishCancel` settles directly via
  `settleLocked(j, job.OutcomeCancelled, s)`. Passing `OutcomeCancelled`
  explicitly is what makes a non-running post-boundary job record
  `OutcomeCancelled`, since `settleLocked`'s `cancelInterrupts` override does
  not fire for post-boundary states.

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
other state runs its stage to completion and sets `Next`, and `Advance`'s
branch 3 gates it rather than moving it on the next tick. A worker that
reports through `AdvanceFrom` is parked by that report; one that sets `Next`
any other way is parked by branch 3 (Finalizing has no
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
  already exposes `PostProcessor.CancelJob(j)` for jobs in post-processing
  (`Repairing`/`Extracting`/`Finalizing`),
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
