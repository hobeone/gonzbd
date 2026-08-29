# `internal/dispatch` — The Dispatcher and the Composed View — Design (RFC)

**Half B2.3.** Amends `docs/superpowers/specs/2026-08-26-lifecycle-intents-design.md`
and `docs/superpowers/specs/2026-08-28-sched-exported-surface-design.md` (RFC
#450, decisions D-B1 … D-B6). Depends on B2.1, which landed as `803d30a8`.

This RFC decides what drives `internal/sched`, who owns the job registry and
manifest residency, and how a queue listing is composed. It does **not** decide
persistence (B2.2) or the swap that retires `internal/queue` (B2.4).

---

## Problem

`internal/sched` is a decision core with nine exported doors and no caller. It
can answer every question about a single job and cannot ask itself any of them,
because D-B5 gives it no registry. Three specific things follow.

### 1. The condition that promotes jobs has nine call sites and no owner

Today's promotion is event-driven. `grep -c 'q\.PromoteNext(' internal/queue/queue.go`
returns **9** — nine places that must each remember that something they just
did might have made a job eligible.

Every one is individually defensible: a job was added, capacity changed, a job
finished. None of them is a mistake. But the set is exactly the shape Standing
Design Rule 2 names:

> **When a check and an owner would both work, take the owner.** A check must
> be called at every site that could violate the invariant, and the failure
> mode of forgetting one is silence.

A tenth site that forgets the call produces a job that is eligible, unblocked,
and never starts. Nothing logs, nothing errors, and no test that does not
exercise that precise path can see it. The count grew because promotion was
framed as *an event to respond to* rather than *a condition to maintain* — and
that reframing is invisible at each individual call site, which is why review
never caught it accumulating.

### 2. Three of B2.1's contracts name a dispatcher that does not exist

`internal/sched/doc.go` and the B2.1 spec defer three obligations, each phrased
as an assumption about a loop:

- **The pause sweep.** D-B3 gives `Pause` no sweep, because `sched` holds no
  jobs — "the sweep is a documented contract on the dispatcher".
- **Cancel's settle.** `finishCancel`'s interrupt arm calls `Abort` and returns,
  leaving the job to settle "on the tick after it yields" (`cancel.go`).
- **The `Park`-then-cancel window.** `doc.go` records a job cancelled while
  running and then `Park`ed rather than `Settle`d, which renders `StatusQueued`
  "until the next `Advance` routes it through `finishCancel` — transient and
  self-healing, but real for the tick it lasts".

All three say *tick*. None of them can be satisfied by a caller that advances
jobs only in response to events, because under that model "the next `Advance`"
has no upper bound.

### 3. `job.RenderView` can be filled one job at a time, and a listing needs many

D-B4 exports `Render(j)`, which takes `Queue.mu` once per job. A queue listing
renders every job, so N jobs means N lock acquisitions and a torn list: a job
may settle between row 3 and row 300. For a single job's fields, B2.1 rejected
exactly this — "two lock acquisitions and two snapshots, reintroducing the tear
`job.Snapshot` exists to remove" — but it left the list case to its consumer.

Per-row consistency is defensible for display. It is not defensible for
aggregates: a total remaining-bytes figure summed across a torn list is a sum
of figures from different instants, and no row in the table shows the
inconsistency.

---

## The organising rule

One sentence decides most of what follows:

> **The tick is the owner of liveness. Everything else is an optimisation over
> it, and must remain deletable without changing what the system computes.**

This is what separates the design from the one it replaces. A kick that reduces
latency is safe precisely because removing it costs latency and nothing else. A
kick that is *required* for a job to start is the tenth `PromoteNext` call site
wearing a different name.

---

## Decision 1 (D-B7) — a ticker owns liveness; the kick is an optimisation

A single goroutine on a timer walks the registry and calls `Advance` on each
job. A non-blocking send on a buffered channel of size 1 wakes it early.

**Why a ticker rather than events.** §1 above is the argument: nine call sites
maintaining a condition is a check where an owner belongs, and the ticker is
the owner. It cannot be forgotten, because there is nowhere else to write.

**Why the kick is not a regression to events.** Both routes call the same
`Advance`, and the ticker alone is sufficient for correctness. The kick is a
latency optimisation over a mechanism that already works without it. The test
that keeps it honest is deletion: remove the kick and the system must still be
correct, only slower. If that ever stops holding, the kick has become a second
owner and must go.

**Why a size-1 buffered channel.** A burst of job-adds collapses into one
wakeup, and ticks cannot overlap. Overlapping ticks would need locking between
them; a single goroutine needs none. This is also what lets the design drop
`internal/queue`'s `promoting` map, whose only content is "another goroutine is
already mid-promotion on this job" — a fact that has nothing to say once there
is no other goroutine.

**Why user actions do not need the tick.** `Cancel`, `Pause`, `Resume` and
`Retry` are synchronous `sched` doors that act immediately. Only promotion and
transition need driving, and those are operations where a tick of latency is
invisible against a multi-minute download.

**What the tick discharges for free.** All three of §2's deferred contracts,
without code written for them: the pause sweep is the next tick's `Advance`
seeing `gatedBy → GlobalPause` and parking every non-running job (branch 2
leaves running holders alone by design); cancel's settle is the next tick after
the worker yields; and the `Park`-then-cancel window acquires an upper bound of
one tick interval where the event-driven model gave it none.

---

## Decision 2 (D-B8) — manifest residency is derived from pool membership

> `manifestResident(j) ⟺ q.holds(j)`

A job needs its manifest exactly when it is doing work, and "doing work"
already has an owner: it holds a pool-A lease or a pool-B slot. The dispatcher
hydrates when `Advance` grants and evicts when the pools are returned.

**This is a Rule 2 correction, not a new bound.** `internal/queue` bounds
residency by `maxActive` while the pools bound concurrency separately — two
independent limits on one underlying question, which is the "derived value that
is also stored" smell. `docs/queue-lifecycle.md` already pins
`phase ∈ {Active, Processing} ⟺ manifest resident` as a property test. This
design keeps that invariant and changes only who computes it: residency stops
being a set the dispatcher maintains and becomes a function of the pools.

**Only the manifest is evictable.** `docs/queue-lifecycle.md`'s three tiers are
unchanged by this RFC: header fields and `JobProgress` are resident for every
job from `Add` until it leaves the queue; the `Manifest` is the sole evictable
tier. D-B8 governs that tier and no other. The measured cost is what makes the
split affordable — 1,356,394 B per 20k-article manifest against 7,512 B for the
same job's three always-resident bitsets, a ratio of 181×, both figures produced
by `TestTerminalJobRetention_Measured`.

**The memory bound follows from the pool capacities.** At most
`leaseCap + slotCap` distinct jobs hold anything — the sum rather than the
maximum, since a job at `Assessing` holds both while one at `Fetching` holds
only a lease. No separate residency knob is needed, and `MaxActiveJobs` becomes
redundant: concurrency is `leaseCap` and `slotCap`.

**Retiring `MaxActiveJobs` is deliberately not decided here.** Removing or
renaming a config field is an escalation-class change requiring
`docs/config-contract.md`, and B2.3 is imported by nothing, so nothing forces
the question now. `dispatch.New` takes the capacities as parameters, exactly as
`sched.New` does. The config mapping belongs to B2.4, where the repoint
actually happens, rather than riding along inside a dispatcher PR.

**Residency is consistent at tick boundaries, not instantaneously.** `grantFor`
runs inside `Advance` under `Queue.mu`, so the dispatcher learns a job acquired
resources only after `Advance` returns, and manifest I/O must happen unlocked.
A job can therefore hold a lease with no manifest for the length of one disk
read. Nothing consumes that window: a worker is handed the job only after
hydration, and a failed read settles the job `Failed` in the same tick,
returning both pools. This is `PromoteNext`'s existing reserve → unlocked I/O →
re-verify shape, minus the `promoting` map.

State the invariant as *at tick boundaries*. The stronger phrasing is false and
would be read as licence to assume a manifest is present wherever a lease is.

---

## Decision 3 (D-B9) — the dispatcher never holds its own lock across a `sched` call

A tick takes `dispatch.mu`, copies the job list, releases it, and only then
calls `Advance` per job.

The constraint is already written into `sched`'s `Workers` interface:

> `Abort` MUST NOT block, and MUST NOT acquire any lock that a caller could
> hold across a call into `Queue`. `cancel.go` calls `Abort` from inside
> `Queue.mu`'s span.

B2.3 does not implement `Workers` (see "What B2.3 does not get"), so the ABBA
deadlock is not reachable in this PR. That is precisely why the discipline must
be established here: it is what makes B2.4's `Abort` able to take `dispatch.mu`
safely. Violating it now breaks a PR that has not been written yet, with
nothing failing in between — the failure mode `check_lock_io` explicitly cannot
catch, since it sees one level of call-graph descent and cannot see through an
interface method implemented in another package.

Lock order overall: `dispatch.mu` → (released) → `Queue.mu` → `Job.mu`.

---

## Decision 4 (D-B10) — `RenderAll`, one computer behind two doors

`sched` gains `RenderAll(js []*job.Job) []job.RenderView`, taking `Queue.mu`
**once**. Both `Render` and `RenderAll` delegate to the existing unexported
`renderLocked`.

**Why not a loop over `Render`.** §3 above: aggregates over a torn list sum
figures from different instants. Holding `Queue.mu` across a listing blocks
`Advance` for its duration, but a `Snapshot` is a value copy under `Job.mu`, so
a thousand-job queue costs on the order of a millisecond.

**Why this does not create a second owner.** Rule 2 requires one function that
computes the view, not one door that exposes it. `renderLocked` remains the
sole computer; the two doors differ only in how many jobs they lock around. A
second *computation* of a `RenderView` — the thing B2.1 refused when it
rejected exported `Running`/`WaitReason` predicates — is still refused.

**This supersedes D-B4's placement clause.** D-B4 reads "`RenderAll` lands with
its consumer in B2.4", which contradicts D-B6 and the B2.1 roadmap table, both
of which place the composed view in B2.3. `RenderAll`'s consumer *is* the
composed view, so it arrives here. D-B4's substantive decisions — one read
door returning `job.RenderView`, computed in a single lock span, no exported
predicates, no pool telemetry — are unchanged.

**The composed row does no I/O and cannot fail.** Status inputs come from
`sched`; name, category and priority from the header tier; byte and article
figures from `JobProgress`. All three are in memory for every job at every
residency. `docs/queue-lifecycle.md`: "Remaining bytes is derived, not stored…
from `FileProgress` alone, so it holds at any residency and needs no
adjustment."

This is worth stating because the opposite is easy to conclude and wrong. Most
jobs in a listing are manifest-non-resident under D-B8, and a
manifest-non-resident job does read `job_files` — but on the **startup** path,
not the listing one. `Store.ArticleCountsByJob` is called from `Load`
(`internal/queue/persistence.go`), once, to size `JobProgress` without
decompressing every manifest. Nothing on the render path touches a store.

---

## Decision 5 (D-B11) — the store interface is startup-read plus state-write

`dispatch` defines the interface it needs and B2.2 implements it, per the
consumer-defines-interface idiom. Two obligations, and no others:

- **Read once at startup** — the job list with its four axes (`State`, `Next`,
  `Outcome`, `Intent`), plus the per-file metadata that sizes `JobProgress`.
- **Write on change** — persist the four axes when they move.

`crossed` is deliberately absent, as B2.1 already recorded: it is derived from
`State` via `func (a *Attempt) crossed() bool { return IsProduction(a.state) }`,
and persisting it would create a second source of truth that could disagree
with `State` after a restore.

B2.3 ships an in-memory implementation for tests. This leaves the dispatcher
unable to persist until B2.2 — the same "imported by nothing" state B2.1
shipped in, and acceptable for the same reason: nothing uses it yet.

---

## Decision 6 (D-B12) — the dispatcher evicts a cancelled never-run job

After `Advance`, the dispatcher removes any job with
`State == StateUnset && Intent == IntentCancel` from the registry and the store.

This closes a defect, not a gap. B2.1 traced it: `finishCancel` returns `nil`
for a never-run job because `Outcome` lives on the `Attempt` and there is none,
so the job survives; `gatedBy` ignores `IntentCancel`, `waitReason` returns
`NoLease`, `NoLease.IsPause()` is false, and `job.ToSABnzbd` therefore returns
`StatusQueued`. **A job the user deleted renders as queued, forever.**

Ordering is safe in both directions. `Advance` routes `IntentCancel` before
every other branch, and `finishCancel` no-ops for `StateUnset`, so eviction
cannot race a settle. It frees no pools, because
`TestRequirements_StateUnsetRequiresNothing` pins that `StateUnset` requires
neither.

It also closes the hole this left in `gatedBy`'s own stated justification —
"advance handles it first, so no cancel value reaches the render path" — which
B2.1 correctly narrowed to "true for every job that has run, false for one that
has not".

---

## Decision 7 (D-B13) — `dispatch` owns the `sched.Queue`; callers talk only to `dispatch`

The dispatcher constructs and holds the `*sched.Queue`. It is not passed one,
and nothing else holds a reference to it.

**This is forced by D-B12, not a matter of taste.** Cancelling a never-run job
must latch the intent *and* evict the job, and only the dispatcher has a
registry to evict from. If the API could reach `sched.Cancel` directly, that
path would latch the intent and skip the eviction — reintroducing the
renders-as-queued-forever defect through a second door, with the first door
still correct. One owner of the cancel sequence means one place it can be
wrong.

The surface, therefore:

| Door | Purpose |
|---|---|
| `New(...) *Dispatcher` | Constructs the `sched.Queue` with the given capacities, clock, `Workers` and store. |
| `Start(ctx) error` / `Stop() error` | The ticker goroutine's lifecycle. `Stop` is idempotent and does not settle anything. |
| `Add(j *job.Job) error` | Registers a job and kicks the tick. |
| `List() []Row` | The composed view — one `RenderAll` call plus the header and `JobProgress` tiers. |
| `Cancel(id) error` | `sched.Cancel`, then D-B12's eviction. |
| `Retry(id) error`, `Pause()`, `Resume()`, `Paused() bool` | Delegate to `sched` unchanged. |

`Row` is a `dispatch` type composing `job.RenderView` with the header and
progress fields a listing needs. It is not a `job` type: `internal/job` cannot
name the registry that supplies half of it.

**Startup.** `Start` reads the store once (D-B11), reconstructs each job's four
axes and sizes its `JobProgress`, and registers everything before the first
tick. Jobs are restored with no lease and no slot regardless of the state they
were persisted at — the pools are process-local and there is nothing to
reclaim — so the first tick re-acquires resources through the ordinary
`grantFor` path. Standing Design Rule 1 applies directly: a job persisted mid-
`Repairing` is simply a job at `Repairing` that holds nothing, which `Advance`
already handles as scenario §5.12's restored post-boundary job does.

---

## Answers to B2.1's carried-forward questions

**§8 q3 — does the dispatcher need `next`? No.** The dispatcher makes two
decisions that might plausibly want it, and neither does. *"Should this job
have a worker?"* is `running(id, s) = IsOpen && Next == StateUnset && holds` —
a function of `Next`, but one `sched` already computes and exposes as
`RenderView.Running`. *"Which kind of worker?"* keys on `State`, because a job
with `Next` set has finished its work and is waiting to move; `Advance` moves
it and `Next` clears, and only then does it need a worker for its new current
state. `Next` stays internal to the state machine plus a rendering passthrough.

**§8 q4 — does pausing change queue position? No.** Position is
`sort_key`/priority, an axis independent of `Intent` and the global flag. §8.1.1
already makes reorder total and unconditionally recorded, so a paused job keeps
its place and competes for a lease there on resume. This is a confirmation, and
needs no code.

**§4.7's reorder table — folded in here.** B2.1 recorded that the table has no
row for a settled attempt and that its last row ("`Extracting`, `Finalizing`,
`Finished` → never, the job will not fetch again") is wrong for one, since
`Retry` reopens a settled attempt at `Fetching`. The replacement rows, split at
the one-way boundary of §4.1:

| Settled attempt at | Reorder takes effect | Why |
|---|---|---|
| `StateUnset`, `Fetching`, `Assessing`, `Repairing` (pre-boundary) | recorded immediately; applies at the reopened attempt's next lease issuance | `Retry` may still reopen it |
| `Extracting`, `Finalizing` (post-boundary) | recorded, but can never take effect | `BeginAttempt` refuses with `ErrBoundaryConsumed` when the job's most recent attempt crossed, so `Retry` can never reach it |

Neither case holds a lease or a slot, so nothing else can be affected. Both are
consistent with reorder remaining total and unconditionally recorded (§8.1.1).

---

## What B2.3 does not get

- **A `Workers` implementation.** `Abort` is called only on the interrupt arm
  — `running && cancelInterrupts(state)`, i.e. pre-boundary. Mapping the
  lifecycle onto today's subsystems, `Fetching` is `internal/downloader`, which
  has **no** per-job cancellation surface at all (only global `Pause`, `Stop`,
  `DisconnectAll`); `Assessing` and `Repairing` are `internal/postproc`'s
  `RepairStage`, covered by the existing `PostProcessor.Cancel(jobID)`. So a
  real `Workers` requires adding per-job cancellation to the downloader — a
  concurrency-sensitive change to a subsystem B2 is not otherwise touching,
  with no consumer until B2.4. `Workers` stays injected, as `sched.New` already
  takes it.

  Note the shape of that gap: the one per-job cancellation the codebase already
  has sits in the half where `Abort` is never called, because post-boundary
  cancel is a gate rather than an interrupt.

- **Persistence.** B2.2 implements D-B11's interface.
- **Any production wiring.** B2.4 repoints the five non-test files that import
  `queue.Queue` and deletes `internal/queue`.
- **A config decision.** See D-B8 on `MaxActiveJobs`.

---

## Testing

The dispatcher's decisions are all functions of the registry, the pools and the
clock, so they are testable with a fake `Workers`, a fake store and an injected
clock — the same shape `internal/sched` already uses.

What must be pinned by observed mutation, each with the mutation named:

| Property | Mutation that must break it |
|---|---|
| The ticker alone is sufficient — the kick is deletable | Delete the kick; every promotion test must still pass, only later |
| A tick promotes in queue order | Reverse the walk order; the last-lease-winner test must fail |
| Manifest residency follows the pools | Drop the evict-on-release call; a resident-set-size assertion must fail |
| Hydration failure settles `Failed` and returns both pools | Neuter the error branch; the pool-accounting oracle must fail |
| `dispatch.mu` is not held across a `sched` call | An enumeration test over the tick's lock spans, in the shape of `TestQueueMuLockers_MatchTheEnumerationStatedInProse` |
| A cancelled never-run job is evicted | Delete the D-B12 branch; a render test must show `StatusQueued` |
| `RenderAll` takes one lock, not N | Two goroutines mutating different jobs during a listing, under `-race` |

The last row needs the two-different-jobs shape specifically: B2.1 established
that a single-goroutine test exercises those lines constantly and constrains
the lock not at all, and that a synchronising call between an unlocked write
and a read creates a happens-before edge that hides the race.

---

## Recommendation

| ID | Decision |
|---|---|
| **D-B7** | A ticker owns liveness; a size-1 buffered kick is a deletable latency optimisation. Discharges the pause sweep, cancel's settle, and the `Park`-then-cancel window. |
| **D-B8** | `manifestResident(j) ⟺ q.holds(j)`. Only the manifest tier is evictable; header and `JobProgress` stay resident. Consistent at tick boundaries, not instantaneously. `MaxActiveJobs` becomes redundant but is not retired here. |
| **D-B9** | `dispatch.mu` is never held across a call into `sched`. Lock order `dispatch.mu` → `Queue.mu` → `Job.mu`. |
| **D-B10** | Add `RenderAll(js)` taking `Queue.mu` once; both doors delegate to `renderLocked`. Supersedes D-B4's "lands in B2.4" placement clause only. |
| **D-B11** | `dispatch` defines a store interface of startup-read plus state-write; B2.2 implements it. `crossed` stays derived. |
| **D-B12** | The dispatcher evicts `StateUnset && IntentCancel` from the registry and the store, closing the renders-as-queued-forever defect. |
| **D-B13** | `dispatch` owns the `sched.Queue`; callers reach `sched` only through `dispatch`. Forced by D-B12: a direct `sched.Cancel` would skip the eviction. |

Answered from B2.1's carried-forward list: §8 q3 (no), §8 q4 (no), §4.7's
reorder rows (folded in above). §4.7's amendment is a spec edit with no code
behind it.
