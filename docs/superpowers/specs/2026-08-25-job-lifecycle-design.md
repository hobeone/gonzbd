# Job Lifecycle — Design

**Status:** Direction settled in discussion, and nine decisions are recorded
(§14) — the six questions this document originally opened, plus three that
arose from settling them. No question it raised is still open. It is the
argument the implementation plans are written against. Nothing here has been
built.

**Scope:** Replaces the job state model in `internal/queue` and the ownership
boundaries between `app`, `queue`, `downloader`, `durability` and `postproc`.

> **This is not a migration.** Per Standing Design Rule 1, no part of this
> design owes anything to state an earlier build wrote or to parity with any
> other implementation. Where the current code is cited below it is cited as
> *evidence about the problem*, never as a constraint on the answer.

---

## 1. The problem

The current model has 17 `constants.Status` values, a 15-key transition table
with 66 directed edges, and a separate 5-value `JobPhase` derived from it. The
value count is not the problem. Three specific defects are, and each is
structural — the model cannot not have them.

### 1.1 One string carries three orthogonal facts

`Status` conflates lifecycle position, current sub-activity, and terminal
outcome:

| Axis | Values tangled into `Status` today |
|---|---|
| Position | `Queued`, `Downloading`, `Paused`, `Completed` |
| Sub-activity | `Verifying`, `Repairing`, `Extracting`, `Moving`, `Running`, `QuickCheck` |
| Outcome | `Completed` vs `Failed` |

`Verifying` is a position (inside post-processing) *and* an activity (par2
verify is running). That conflation is most of why the edge table needs 66
entries: the post-processing block is not modelling transitions, it is
enumerating which activity might come next. Every processing state has an edge
to every later processing state because the model has no way to say "still
post-processing, doing something else now".

The consequence that matters is testability. Six states each making their own
routing decision gives a test surface that is the *product* of those decisions
— which is why the current pipeline needs a self-gating matrix of flag
combinations (`ParError`, `UnpackError`, `QuickCheck`, PP level) rather than a
set of cases.

### 1.2 The verification decision has two implementations

`Application.par2NeedsRecovery` decides, at download completion, whether a job's
deferred par2 recovery volumes must be fetched. Its doc comment states the
duplication outright:

> *"It mirrors the post-processing QuickCheck stage: it parses the par2 index
> files already on disk in dir and…"*

The same question — *are these bytes intact?* — is answered once in
`internal/app` and again as the `quickcheck` stage in `internal/postproc`. Two
enforcement points for one invariant is precisely what Standing Design Rule 2
forbids.

The related loop is worse. `postproc`'s repair stage sets `NeedRequeue` and
`RequeueBlocksNeeded` when par2 reports insufficient blocks, but
`postproc.go:512` records that this "is recorded for informational purposes
(history/UI) but no longer aborts the pipeline". So *"we need more par2, go get
it"* is a live path in one place, a dead flag in the other, and a reader cannot
tell which from the type system.

### 1.3 Residency is a property three functions must agree about

`docs/queue-lifecycle.md` states the intended design and records that it was
never built:

> Four phases replace the 14 statuses, with residency as a function of phase —
> Active/Processing if and only if the manifest is resident — rather than a
> parallel `inflated` axis. That structural choice is what removes the
> silent-nil defect class.

The memory optimization shipped; the structural choice did not.
`JobPhase.IsResident()` declares the invariant and nothing enforces it, while
residency is decided independently in `Queue.Add`, `evictJobLocked` and
`PromoteNext`. The document attributes eight issues and four pull requests to
this, and notes that the existing property test passed through all of them
because it walks one job down the happy path.

**A property that three functions must agree about is not an invariant.** The
fix is not a fourth check.

---

## 2. The design in one page

```
        ┌───────────────────┐
        │      Waiting      │  holds nothing; knows where it is going
        └─┬───────────────┬─┘
          │               │
          ▼               ▼
   ╔══════════════════════════════════════════════════════╗
   ║  CORRECTNESS — reversible, idempotent                ║
   ║                                                      ║
   ║   Fetching ─────────► Assessing ◄────► Repairing     ║
   ║      ▲                 │      │                      ║
   ║      └─────────────────┘      └──► Finished(Unrec.)  ║
   ║          NeedsMore            │                      ║
   ╚═══════════════════════════════╪══════════════════════╝
                                   │
                                   ▼   ◄══ THE IRREVERSIBLE BOUNDARY
   ╔══════════════════════════════════════════════════════╗
   ║  PRODUCTION — forward only, destructive              ║
   ║                                                      ║
   ║   Extracting ─────────► Finalizing ─────► Finished   ║
   ╚══════════════════════════════════════════════════════╝
```

Seven states on one axis, two orthogonal axes beside it, one branching node,
one irreversible edge.

Resource holding, which cuts across the boundary rather than along it:

| State | Lease (pool A) | Compute slot (pool B) |
|---|---|---|
| `Waiting` | — | — |
| `Fetching` | held | — |
| `Assessing` | held | held |
| `Repairing` | held | held |
| `Extracting` | — | held |
| `Finalizing` | — | held |
| `Finished` | — | — |

The lease is surrendered on crossing the boundary, not on leaving `Fetching`
(§6, §8.1).

```go
type State uint8
const (
    Waiting State = iota  // holds nothing; knows where it is going
    Fetching              // holds a Lease
    Assessing             // holds a Lease + a compute slot   ← the only decider
    Repairing             // holds a Lease + a compute slot
    Extracting            // holds a compute slot
    Finalizing            // holds a compute slot
    Finished              // terminal
)

type Activity uint8   // what is running right now; NOT a state
type Outcome  uint8   // write-once, set only on entering Finished
```

**`Queued` is not among them, and that is the decision rather than an
oversight** (D1). A newly added job is `Waiting{Next: Fetching, Reason:
NoLease}` — the same situation as any other job blocked on capacity. Nothing
distinguishes a never-started job at the level of the machine; "has this ever
run" is `len(attempts) == 0` (§3.1), and deletion is unconditional and
idempotent rather than conditional on the answer.

---

## 3. Three axes, not one enum

**`State`** is the machine. It answers *where in its current attempt is this
job, and what may happen next*. Seven values, listed above. The field lives on
the attempt rather than the job (§3.1).

**`Activity`** is what is executing right now: `Par2Verify`, `CRCCheck`,
`Unpack`, `VolumeRecovery`, `Deobfuscate`, `Move`, `Script`, and so on. It is a
field the running component writes, never a transition. It exists for the UI,
the API and the log — nothing branches on it.

**`Outcome`** is the verdict, and it is **write-once**, assigned only on the
edge into `Finished`: `OK`, `Failed`, `Unrecoverable`, `Cancelled`.

Write-once matters more than it looks. Today `Failed → Queued` is a legal edge,
so *"did this job fail?"* is a question whose answer can change, and
`Queue.Retry` has to reconstruct the distinction between "failed, retry me" and
"done, keep me" from `failed[]` bits. With a write-once outcome, a retry is
unambiguously a **new attempt**, not a mutation of an old verdict.

**Consequence for the transition table.** With position separated from
activity, the edge set collapses from 66 to the handful drawn in §2, and every
remaining edge is genuinely reachable. There are no fan-out blocks because
"still post-processing, doing something else" is an `Activity` write rather
than a transition.

### 3.1 Attempts: the machine lives on the attempt, not the Job

A write-once `Outcome` and a retryable job are in tension only if the job has
one outcome. It does not — it has a list of attempts, each with its own (D2).

```go
type Attempt struct {
    State    State
    Activity Activity
    Outcome  Outcome   // write-once, on entering Finished
    Started  time.Time
    Ended    time.Time
}

type Job struct {
    // identity and progress
    attempts []Attempt   // the machine; current attempt is the last
}
```

> **An `Attempt` opens when a lease is first issued and no attempt is open. It
> closes when the job reaches `Finished`.**

Pause and resume inside an attempt do not end it: the lease is surrendered and
later re-taken, and the attempt persists across that. A retry from `Finished`
opens a new one.

`Job.State()` is therefore derived, not stored:

```go
func (j *Job) State() StateView {
    if len(j.attempts) == 0 {
        return StateView{State: Waiting, Next: Fetching, Reason: NoLease}
    }
    return j.attempts[len(j.attempts)-1].StateView()
}
```

The zero-attempt arm is a constant, not a special case: a job that has never
run needs no attempt record because nothing has happened to it.

Three things follow, and each retires a question that would otherwise need its
own answer:

- **Retry costs nothing structurally.** Job identity is stable, so the
  durability record, the manifest path and the partial file on disk are all
  still keyed correctly. Today a failed job deliberately *keeps* those rows so
  a retry re-fetches only what failed; here that is a consequence rather than
  an exception.
- **`Outcome` stays genuinely write-once.** A verdict is never revised, only
  superseded by the next attempt's. This is the ledger move: do not mutate the
  balance, append an entry.
- **"Never started" is exact.** `len(attempts) == 0`, rather than a predicate
  over progress. No figure derived from bytes or durable runs can distinguish
  *did not start* from *started and got nowhere*, and those differ.

**The list is unbounded** (D7). Attempts are small and the growth case is
narrow — a job an automation tool retries on a schedule. Not worth a retention
policy before there is evidence one is needed; the implementation carries a
comment at the field recording the case and the two obvious remedies (cap the
list, or sweep with history retention), so the next person to hit it does not
have to rediscover the shape.

### 3.1.1 Retry has exactly one meaning

> **A retry resumes this job, re-fetching only what previously failed. There is
> no full re-fetch.** A user who wants every byte re-downloaded adds the NZB
> again (D8).

This is worth stating as a rule rather than leaving as a default, because it
removes a mode. There is no `retry --all` flag, no second code path, and no
question at any call site about which kind of retry is in progress. It also
makes the durability retention on a failed job unconditionally correct: retry
always wants those records, so there is no case in which keeping them is
wasted.

Re-adding is genuinely a different operation and behaves like one — a new job
ID, a fresh working directory via `UniqueName`, no inherited durability record.
It will trip duplicate detection against the original, which is correct: the
user is told, and proceeds if they meant it.

### 3.2 Policy replaces the PP integer

SABnzbd's PP levels 0–3 are a cumulative integer mask, and they are the same
*kind* of thing as its status strings: external vocabulary that should be
translated at the boundary and never stored internally (D4).

```go
// Resolved once at ingestion from PP plus the job's category.
// The integer does not exist past App.
type Policy struct {
    Verify bool   // run a real verdict rather than a trivial Complete
    Repair bool
    Unpack bool
    Delete bool
}
```

**Every state runs at every policy.** At `Verify: false` the `Assessor` returns
`Complete` without doing work, and the job crosses the boundary immediately.
This matters structurally: gating *states* on PP would mean skipping
`Assessing` at PP=0, which removes the only state that decides and leaves
nothing to authorize the crossing. A second decider would have to be
reintroduced — the exact thing this design exists to avoid.

The machine's shape is therefore policy-independent, which is what keeps it
exhaustively testable, and per-category overrides stop being a special case.

---

## 4. The reversibility boundary

This is the central invariant of the design. Everything else follows from it.

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
bandwidth. Unpacking twice, or deleting archives you still need, is
destructive.

This one line does four separate jobs in the design:

1. It defines **pause granularity** (§8).
2. It defines **cancel semantics** (§8).
3. It defines the **lease lifetime** (§6).
4. It defines which failures are **recoverable** — everything before the
   boundary is restartable from the same files.

---

## 5. `Assessing` is the only decider

Every other state does work and returns. `Assessing` is the sole branching node
in the machine, and its verdict is total:

| Verdict | Next state |
|---|---|
| `Complete` | `Extracting` — cross the boundary |
| `Repairable` | `Repairing`, then back to `Assessing` to re-verify |
| `NeedsMore(blocks)` | `Fetching` — acquire recovery volumes |
| `Unrecoverable` | `Finished(Unrecoverable)` |

Two properties follow.

**Repair never fails for insufficiency.** par2 verify computes block
sufficiency *before* any repair runs, so the decision to enter `Repairing` is
made with complete information. "We needed more blocks" is a verdict, never a
repair failure. `NeedRequeue` ceases to exist as a concept — it becomes an
ordinary edge.

**Verification method is an implementation detail.** The cheap CRC path and the
full par2 verify are two ways for one `Assessor` to reach one verdict. There is
no `QuickCheck` state and no "bypass"; there is one component that answers *are
these bytes right?* and is free to answer cheaply when it can. This is the
single implementation that §1.2's duplication is missing.

**An `Unrecoverable` job never crosses the boundary** (D3). Its files stay in
the working directory and the job is `Finished(Unrecoverable)`.

The reason is not that partial output is worthless — for a post of independent
files it is genuinely useful. The reason is what crossing *costs*. Crossing is
irreversible: archives are deleted, files are moved, and the inputs a later
attempt would need are consumed. **Not crossing keeps the job retryable**, and
a missing article may well be available next month, or from a server the user
adds next week. Preserving that is worth more than salvaging the intact subset,
and it is the whole point of having a boundary.

Delivering the intact files only when no archive set is implicated is the
sophisticated alternative. It was rejected: it requires classifying files into
archive sets *before* extraction, which is a new inference with its own failure
modes, in service of a case that is uncommon on binary Usenet.

**Testability.** Every path through a job is `Fetching → Assessing → {one of
four}`. The test surface is the verdict function, not the graph.

---

## 6. The Lease

Three things have exactly the same lifetime — from a job beginning to fetch
until it crosses the irreversible boundary:

| | lifetime |
|---|---|
| pool-A capacity (reserved across the whole correctness loop) | Fetching → crossing |
| the resident `Manifest` | needs to be resident for exactly that span |
| the `StorageBarrier` | writes downloaded articles; useless after crossing |

Three things with one lifetime are one object.

```go
// Issued by Queue. Held by Job for the whole correctness loop.
// Surrendered on crossing into Extracting.
// There is no other way to obtain a Manifest or a StorageBarrier.
type Lease struct {
    manifest *Manifest
    barrier  *StorageBarrier
}

func (j *Job) BeginFetch(l *Lease) error  // cannot be called without one
func (j *Job) Surrender() *Lease          // called on crossing
```

**This is what actually retires §1.3.** Residency stops being a property three
functions must agree about and becomes an object you either hold or do not.
There is no code path that produces a nil manifest, because there is no path to
a manifest except through a lease you were handed. The compiler sees it.

The `Manifest`/`JobProgress` split also stops needing a rule. `JobProgress` is
job state, always present, owned by the `Job`. `Manifest` is leased. That is
not something to remember; it is which struct the field lives in.

**A lease is unpersistable by construction** — in-process capacity, an
in-memory manifest, a live barrier. §10 depends on this.

---

## 7. Ownership map

Every piece of state has exactly one owner and exactly one mutation path.
Everything else reads.

| Component | Owns | Never does |
|---|---|---|
| **App** | NZB ingestion and validation; process construction; reading persisted rows into `Job`s | resume or recovery logic (§10) |
| **Queue** | the priority-ordered job list; lease issuance; compute-slot issuance; the dispatcher | mutate job state except through `Job` methods |
| **Job** | all of its own state, behind its own `sync.RWMutex`; answers `IsDone()`; holds its `Lease` | call any `Queue` method (§7.1) |
| **Lease** | the grant of manifest + barrier + pool-A capacity | outlive the crossing |
| **StorageBarrier** | persisting article bytes to durable storage; write caching; reconciling its own record against the disk at construction | mark an article done without an fsync |
| **Assessor** (`internal/par2`) | the verdict — the single answer to *are these bytes right?* | mutate the job, or accept a `queue` type (§7.3) |
| **Dispatcher** | server pools, connection lending, per-article retry and penalty policy | hold per-job state of its own (§9) |
| **Checkpointer** | writing job state to SQLite; sole DB writer for job rows | read anything but snapshots |
| **PostProcessor** | the production stage sequence; writes `Activity` | decide anything the `Assessor` decides |

### 7.1 The lock-ordering rule

`Job` holds a real lock, so lock ordering is a proof obligation. One rule
discharges it:

> **A `Job` method never calls a `Queue` method.**

Queue is strictly the caller, Job strictly the callee, and the order is always
`Queue.mu → Job.mu`. Anything that walks the queue takes `q.mu`, snapshots, and
releases before touching job locks.

### 7.2 Snapshots are the only read path

Every consumer that is not mutating — the API, metrics, the checkpointer, the
UI, the dispatcher — takes an immutable `JobSnapshot` and never holds a job
lock.

This bounds the cost of per-job locking honestly: **cross-job aggregates are
snapshot-based and slightly stale.** Job A is read at T₀ and job B at T₁. For
reporting that is correct and already effectively true. For decisions it is
not, so lease issuance reads fresh under `q.mu`.

`Manifest` is immutable after parse and is shared by reference into snapshots.
`JobProgress` is deep-copied. That is what a snapshot *is*, rather than a rule
callers must remember.

### 7.3 The Assessor lives in `internal/par2`, and takes only values

`par2` already owns both verification methods and already expresses them over
value types rather than queue types — `VerifyCRCs(files []AssembledFile, sets
[]Set, …) CRCVerifyResult`, `QuickCheck(dir string, sets []Set, …)`, and
`GoVerify`. Neither `par2` nor `queue` imports the other today. The `Assessor`
is not a new component with new dependencies; it is a function unifying three
things that already live there into one verdict, and it belongs with them
(D5).

The guardrail that keeps this true:

> **The Assessor's inputs are value types — `[]AssembledFile`, `[]Set`, a
> `Policy`, and a speculation-evidence value. Never `*queue.Manifest`.**

That is the line between `par2` remaining a format-and-verification library and
acquiring a queue dependency by accident. Everything the Assessor needs about
the job — which files are recovery volumes, their expected sizes and CRCs, what
DirectUnpack managed to extract — is expressible as data, and passing it as
data is what keeps the verdict independently testable with no queue at all.

---

## 8. Scheduling, pause, and cancel

### 8.1 Two pools, one ordering

- **Pool A — acquisition leases.** Bounds how many jobs may be working toward
  correct bytes. Held across the entire correctness loop, *including* while
  assessing and repairing.
- **Pool B — compute slots.** Bounds concurrent CPU/disk work: `Assessing`,
  `Repairing`, `Extracting`, `Finalizing`. **One pool, not split by resource
  class** (D6) — "max concurrent post-processing" is the knob users already
  understand, and splitting it doubles the tuning surface for a benefit nobody
  has measured.

  **The known cost, named and not solved:** a user script in `Finalizing` is
  arbitrary code of arbitrary duration, and it holds a compute slot while
  consuming none of what the pool exists to bound. If that turns out to block
  real work in practice, the fix is to run the script outside the pool — a
  small change, and better made against evidence than against speculation.

Pool A is reserved rather than released-and-reacquired. A job re-entering
`Fetching` from `Assessing` never waits, so the correctness loop is provably
non-starving. The cost is real and accepted: acquisition capacity sits idle
while a leased job assesses or repairs.

The Queue keeps **one priority-ordered list with two consumers**:

```
Queue owns the priority-ordered job list
   ├── lease issuance:  top N by that order get leases
   └── dispatch:        serve leased jobs in that same order
```

Priority therefore has exactly one meaning in the system. A second fairness
policy governing bandwidth would have to re-derive priority in order not to
contradict the lease order; there is no second policy.

### 8.1.1 Reorder is defined over fetch ordering, and is total

Both consumers of the list above are fetch concerns, so reorder is defined
against that and nothing else (D9):

> **Reorder sets the job's position in the priority-ordered list. The change is
> always recorded, and takes effect whenever the job next competes for fetch
> capacity — which for some states is immediately, for others later, and for
> some never.**

| Job's state when reordered | When the new position takes effect |
|---|---|
| `Waiting{Next: Fetching}` | at the next lease issuance |
| `Fetching` | immediately — it changes dispatch precedence |
| `Assessing`, `Repairing` | on re-entering `Fetching`, if the verdict is `NeedsMore` |
| `Extracting`, `Finalizing`, `Finished` | never — the job will not fetch again |

The alternatives were to reject the call or to silently no-op for
mid-lifecycle jobs. Both are worse for the same reason: they make reorder a
*partial* operation whose success depends on state the caller would have to
inspect first, which is a race by construction. Recording unconditionally makes
it total — every call succeeds, there is no error arm to document, and the API
needs no state check.

"Never" is not a failure. A job past the boundary has no remaining fetch
ordering to take a position in, and recording a position that will not be
consulted costs one integer.

### 8.2 `Waiting` unifies pause and slot-waiting

Waiting for a lease, waiting for a compute slot, and being paused are the same
situation: the job is at a known boundary, holds nothing, decides nothing, and
is blocked on permission. Only the reason differs.

```go
type WaitState struct {
    Next   State       // already decided; Waiting never decides
    Reason WaitReason  // NoLease | NoComputeSlot | UserPaused | GlobalPause
}
```

`Waiting` does not add a branching node — the next state was already chosen, by
`Assessing` or by the natural forward edge — so §5's single-decider property
survives. The UI gains honesty: "waiting to repair" and "waiting for a download
slot" and "paused" are one shape rendered three ways, instead of `Paused`
meaning four things depending on what the job was doing.

### 8.3 Pause is a gate, not an interrupt

Pause closes the gate at state transitions. Work in flight runs to the end of
its state and the job then enters `Waiting`. Granularity differs only in how
often the gate is checked: per-article in `Fetching`, per-state elsewhere.

This removes a whole failure class: **partially-applied work**. If pause could
interrupt an unpack, resume would have to answer "what state is the extraction
directory in?", which is unanswerable in general because external tools do not
checkpoint. Gating at boundaries means every state is entered and left
atomically, so resume is always "start the next state", never "resume the
middle of one".

The cost is honest: pausing mid-repair on a large set does nothing for minutes.
The fix is to *show* that ("finishing repair, then pausing"), not to make
repair interruptible.

### 8.4 Cancel is an interrupt before the boundary, a gate after

| State | Cancel behaviour |
|---|---|
| `Waiting`, `Fetching` | abort immediately |
| `Assessing` | abort immediately |
| `Repairing` | kill the repair, abort immediately |
| `Extracting`, `Finalizing` | finish the current state, then stop |

Before the boundary, everything is restartable from the same files and nothing
external was touched, so cancel means *stop now*. After it, there is no clean
stop for half-moved files or a half-run user script, so cancel degrades to a
gate.

**In-flight articles for a cancelled job must be dropped, not written.**
`Job.AddArticle` on a cancelled job returns `ErrNotAccepting`. The `Job` owns
that decision because it is the `Job`'s state that says "cancelled".

---

## 9. Dispatch

One dispatcher, owned by the `Queue`, serving leased jobs in priority order.

```go
// Everything the dispatcher needs, and nothing more.
type LeasedJobs interface {
    InPriorityOrder() []*Job  // snapshot; caller holds no lock
}
```

The `Job` still owns what is outstanding — `job.NextArticle()` is the only way
to learn it and `job.AddArticle()` the only way to resolve it. The dispatcher
is a **worker, not state**, and workers may be shared as long as they mutate
only through the owner's methods.

**The line to hold:** the dispatcher reads snapshots and holds no per-job state
of its own. Any work-conserving scheduler must see all candidates — that is
inherent to scheduling, not a coupling smell. The discipline is that the thing
which sees everything only *reads*.

Three properties fall out:

- **Work-conserving without policy.** A job holds the dispatcher only while it
  has a *dispatchable* article. In-flight, already-emitted and permanently
  failed articles are not dispatchable, so the loop falls through to the next
  job automatically. "Serve the top job until it cannot use the capacity" is
  emergent, not written.
- **Higher throughput than round-robin.** Consecutive articles from one job
  share a newsgroup and have related Message-IDs, so a connection that stays on
  one job pipelines deeper and hits server-side caching. Interleaving across
  jobs destroys that.
- **Global concerns have one home.** Speed limiting, idle-disconnect, server
  penalties and the all-servers-exhausted verdict are inherently cross-job.

The dispatcher lives in its own package, not inside `internal/queue`, and
depends only on the read-only interface above.

---

## 10. Persistence and restart

**Nothing persists a lease.** A lease is in-process capacity, an in-memory
manifest and a live barrier — none of it survives a restart.

> **Therefore every persisted job restores to `Waiting`. There is no other
> legal option.**

`Waiting{Next: Fetching}` for one that was fetching, `Waiting{Next:
Extracting}` for one past the boundary. The Queue then issues leases up to the
pool limits in priority order, exactly as at any other moment.

**Restart is not a special code path.** It is the ordinary scheduler starting
from a cold pool. This is forced rather than remembered: the thing you would
need in order to be in any other state cannot be deserialized.

### 10.1 The barrier reconciles itself

Something must reconcile *what the durability record claims* against *what is
actually on disk* after a crash. That belongs to the component whose stated
purpose is owning durable storage.

**The `StorageBarrier` reconciles at construction** — it reads its own record,
stats the files it claims, drops any claim longer than reality, and only then
is handed over inside a lease. That happens on **every** lease issuance, of
which the first after a restart is merely one instance.

Two consequences: `App` never has resume logic, and "crash recovery" stops
being a distinct concern. A crash is just a restart where the record disagrees
with the disk, and resolving that disagreement is the barrier's normal
constructor. Exceptional paths that run rarely are exactly the ones that rot.

### 10.2 The Checkpointer is the sole DB writer

`Job` does no I/O. It exposes `Snapshot()`; one `Checkpointer` reads snapshots
and batches writes to SQLite.

This keeps the single-writer property, preserves batching, leaves `Job`
trivially testable with no store dependency, and upholds §7.1 — `Job` never
calls out. The cost is that `JobSnapshot` is a second shape of job state that
must stay honest.

Article done/failed state remains **derived, not stored**, reconstructed from
the durability record and the failed-article record. Storing it would create a
second authority, and the stored copy is the one that drifts.

---

## 11. DirectUnpack: speculation with discard

DirectUnpack is a production activity running during acquisition — which
appears to violate §4's one-way boundary. It does not, because **the boundary
governs *committing* output, not *computing* it.**

Extraction may run during `Fetching`, writing to a **speculative area**.
`Assessing`'s verdict then either:

- **promotes** it — verdict `Complete`, the extraction is already done, so
  `Extracting` is a no-op; or
- **discards** it — verdict `Repairable` or `NeedsMore`, so the speculative
  output is thrown away, the job repairs, and extraction runs properly
  afterwards.

Nothing speculative reaches final output without passing through the hub, so
the invariant holds exactly. DirectUnpack failing degrades to "no speculation
happened", which needs no modelling. Speculative execution with a discard path
is the standard shape for doing work before you are entitled to trust it.

---

## 12. Translation to SABnzbd

Our states are internal. The legacy `/api?mode=queue` contract is satisfied by
a total function at the API boundary:

```go
func ToSABnzbd(s State, a Activity, o Outcome, w WaitReason) constants.Status
```

| Ours | SABnzbd |
|---|---|
| `Waiting{NoLease}`, never started | `Queued` |
| `Waiting{NoComputeSlot}` | `Queued` |
| `Waiting{UserPaused \| GlobalPause}` | `Paused` |
| `Fetching`, first pass | `Downloading` |
| `Fetching`, re-entered for recovery volumes | `Fetching` |
| `Assessing`, cheap method | `QuickCheck` |
| `Assessing`, full par2 | `Verifying` |
| `Repairing` | `Repairing` |
| `Extracting` | `Extracting` |
| `Finalizing`, activity `Move` | `Moving` |
| `Finalizing`, activity `Script` | `Running` |
| `Finished(OK)` | `Completed` |
| `Finished(Failed \| Unrecoverable)` | `Failed` |
| `Finished(Cancelled)` | `Deleted` |

The four SABnzbd statuses that no current code path assigns — `Grabbing`,
`Fetching`, `Propagating`, `Checking` — become **output values the shim may
emit**, never states we store or transition through. `Fetching` in particular
finally means what upstream documents it to mean: *downloading extra par2 files
for repair*, which is exactly our `Assessing → Fetching` edge.

---

## 13. Deliberately not built

Named here so they are decisions rather than omissions.

- **Anti-starvation floor on dispatch.** Strict priority means a low-priority
  job at 99% can sit indefinitely behind a high-priority job that keeps getting
  work. That is what priority *means*. The obvious fix — boosting jobs
  re-entering `Fetching` because they are near completion — introduces a second
  ordering and undoes §8.1's single-ordering property. Not built.
- **Interruptible production.** See §8.3. The cost of a slow pause is accepted
  in exchange for never representing partially-applied external work.
- **Per-job dispatchers.** Rejected in §9; the composition benefit is worth
  less than the single-ordering property.
- **A migration path.** Standing Design Rule 1.

---

## 14. Decisions

D1–D6 are the questions this document originally opened. D7 and D8 arose from
settling them and are recorded here rather than in a follow-up, so the whole
decision set reads in one place. Each carries its reason, because the reason is
what a later reader needs in order to reopen one honestly.

| | Question | Decision | Stated at |
|---|---|---|---|
| **D1** | Does `Queued` exist separately from `Waiting`? | **No.** A new job is `Waiting{Next: Fetching, Reason: NoLease}`. Nothing distinguishes it at the level of the machine, delete is unconditional, and freshness is `len(attempts) == 0`. | §2, §3.1 |
| **D2** | Retry semantics | **Attempts are a list.** Same `Job`, same ID, a new `Attempt` per run, each with its own write-once `Outcome`. | §3.1 |
| **D3** | Partial success on `Unrecoverable` | **Never cross the boundary.** Files stay in the working directory; the job stays retryable, which is worth more than salvaging an intact subset. | §5 |
| **D4** | PP levels 0–3 | **Resolve to a `Policy` at ingestion.** The integer does not exist past `App`; every state runs at every policy. | §3.2 |
| **D5** | Where the `Assessor` lives | **`internal/par2`,** which already owns both verification methods over value types. Guardrail: value inputs only. | §7.3 |
| **D6** | Compute-slot granularity | **One pool.** The long-running-script cost is named and deliberately unsolved. | §8.1 |
| **D7** | `Attempt` retention | **Unbounded.** The growth case is narrow and the remedies are cheap; the field carries a comment rather than a policy. | §3.1 |
| **D8** | Full re-fetch | **Not a retry mode.** Retry re-fetches only what failed; re-adding the NZB is how a user asks for everything. | §3.1.1 |
| **D9** | Reorder on a mid-lifecycle job | **Always recorded, effect deferred.** Reorder is defined over fetch ordering and is total — no error arm, no state check. | §8.1.1 |

### 14.1 Status of the question set

**No question this document raised is still open.** That is a statement about
this document, not a claim that the design is complete: writing the phase plans
will raise questions of its own, and those belong in the plans rather than
here.

Three of the nine came from settling the others — D7 and D8 from D2's attempt
model, D9 from D1's collapse of `Queued` into `Waiting`. That is the expected
shape. A decision that generates no follow-on questions usually means the
consequences were not chased.

---

## 15. Implementation decomposition

Seven phases. Each must leave the tree green. Phases 1–3 are behaviour-
preserving refactors; 4–7 change behaviour.

| # | Phase | Delivers | Depends on |
|---|---|---|---|
| 1 | **State surface** | `State`/`Activity`/`Outcome`, **`Policy` (D4)**, the transition function, `ToSABnzbd` shim, contract test against the existing API | — |
| 2 | **Job owns its lock** | job state mutation moves inside `Job`; **the machine moves onto `Attempt` (D2)**; `Queue` becomes an ordered index; §7.1 rule enforced | 1 |
| 3 | **Lease** | `Manifest` + `StorageBarrier` reachable only through a `Lease`; retires §1.3 | 2 |
| 4 | **Assessor** | one verdict implementation; `Fetching ⇄ Assessing` loop; `NeedRequeue` and the `quickcheck` stage both deleted | 3 |
| 5 | **Pools and `Waiting`** | two pools, reservation, unified `Waiting`, pause-as-gate, cancel semantics | 4 |
| 6 | **Dispatch inversion** | `job.NextArticle()` / `job.AddArticle()`; Queue-owned dispatcher over `LeasedJobs` | 5 |
| 7 | **Speculation** | DirectUnpack writes to a speculative area; promote/discard at the verdict | 4, 6 |

Phase 3 is the one that pays for the whole exercise on its own. Phase 4 is the
one that removes a shipped duplication.

**Each phase gets its own implementation plan and its own PR.** This document
is too large to be a single plan, and the phases have real dependencies — a
plan written for phase 5 before phase 4 lands would be written against a
verdict function that does not exist yet.
