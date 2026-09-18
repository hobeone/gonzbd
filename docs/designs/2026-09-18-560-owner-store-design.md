# Issue 560, design A: one owner and one reclaim rule for per-job rows

Tree: `f83c366d` (main). Line numbers are against that commit unless marked.
Status: design A of a two-design cross-critique, 2026-09-18. Nothing here is implemented.

## 1. The design in one paragraph

`internal/durability` becomes the single owner of a job's per-job rows —
`durable_runs`, `job_files`, `failed_articles` — and every other package stops
executing SQL against them. Deletion has **one** lifecycle path, `Reclaim`,
driven by **one** rule stated once in SQL: *a job's rows are reclaimed when
nothing can reach the job; a job reachable only as a FAILED history entry keeps
its `durable_runs` and nothing else.* Every departure calls `Reclaim` after
changing the state it depends on; startup calls it for everything. Because the
rule is re-derived from the database's current truth rather than from a
decision a call site remembered, a crash or a missed call can delay a reclaim
but can never make one wrong. One prerequisite makes the rule's inputs exact:
the dispatcher writes a job's queue row **before `Add` returns**.
`history_job_files` stays with `internal/history`, written and deleted in the
same transactions as its entry. No lifecycle is enforced by the schema.

## 2. Evidence base

Every item was re-derived from source. The ones marked *adversarial* also went
through an independent, fresh-context attempt to refute them; the Status column
gives the outcome.

| # | Fact | Evidence | Status |
|---|---|---|---|
| E1 | `dispatch_jobs` has one writer, reached only from the tick and from `Stop`. `Add` writes nothing. | `store.go:93` (sole INSERT); `tick.go:119` (sole `Save`); callers `tick.go:58,62`, `dispatch.go:601` (Stop); `registry.go` `register` only mutates maps and calls `kick()` | adversarial: confirmed |
| E2 | **An accepted job is lost by a hard kill right after `mode=addfile` returns 200.** | Crash-test repro: kill immediately after the 200 → job absent after restart in **10/15** runs; positive control (kill 1.5s later) present **5/5**. Every loss orphaned 1 `job_files` row and the manifest. Startup rebuilds the queue from `dispatch_jobs` only; no `ReadDir` of manifests or `admin/nzb` in production. | reproduced |
| E3 | The window is "until the woken tick persists", not "up to 1s". The tick launches before it persists, and walks earlier jobs first. | `registry.go:205` (`kick`), `dispatch.go:454-455`, `tick.go:29-63` | adversarial: corrects an earlier "~1s" estimate |
| E4 | For a user, E2 means *lost*, not *re-added*: the dir scanner deletes the source after `HandleNZB`; a manual resubmit matches the surviving `admin/nzb` backup and is added **paused as a duplicate**. | `internal/dirscanner/scanner.go` (`os.Remove` after success); `app.go:657-667` | adversarial |
| E5 | `RetryHistoryJob` deletes the history row **after** an in-memory `Add` and **before** any tick writes `dispatch_jobs` — a span where the job has neither row. A crash there loses it and strands its manifest, `durable_runs` and `job_files`. | `app.go:2342` (`Add`), `:2347` (`DeleteKeepingDurability`) | adversarial: refutes "queue OR failed-history" as a current invariant |
| E6 | Only `durable_runs` must be **read** across the queue→history handoff. `job_files` has no reader for a job in history; the retry flush rewrites every column for every file. `failed_articles` must be **absent** at retry, not kept. | readers: `residency.go:108,158,179` (live jobs only), `app.go:2240` (`runs.ForJob`, the retry); flush `app.go:2324-2338` over `dispatcher_wiring.go:99,114-123`; retry clears `failed_articles` at `app.go:2266` | adversarial: partial — see E7 |
| E7 | The retry's `failed_articles` clear is **non-fatal** (`Warn` only) on the progress-applied branch, so stale failed marks can reach the requeued job's hydration — the #422 sibling. It contradicts `dropJobDurability`'s own rule that a failed cleanup aborts a retry. | `app.go:2265-2270` vs `durability.go:1480-1487`; applied via `residency.go:177-203` | adversarial: new |
| E8 | **`RemoveJob` of a queued, never-run job can skip all cleanup.** `Cancel` kicks the tick; if `evictCancelledNeverRun` wins, it deletes the row itself, `Remove` returns `ErrNotFound`, and `RemoveJob` returns early — no manifest unlink, no `CancelJob`, no durability delete. | `app.go:866-875` (early `return rmErr`); `tick.go` `evictCancelledNeverRun` calls `d.store.Delete`; `Cancel` callers: `app.go:866`, `job_finalizer.go:114` (a job that ran) | verified; race inferred, not reproduced |
| E9 | `MarkCompleted` flips a Failed entry to Completed with a bare `UPDATE`, stranding the retained rows under an entry no retry can use. | `repository.go:429`; called from `internal/api/history.go:284` directly on the history store | verified |
| E10 | `RunStore.Commit`'s only production callers are the barrier. | `barrier.go:282`, `:628` | adversarial: confirmed |
| E11 | Nine raw SQL statements against the three tables live in `internal/app`; `history.Repository.delete` deletes two of them by string concatenation; `residency.go:158` reads `durable_runs` bypassing `RunStore`. | `app.go:815,2267`; `dispatcher_wiring.go:99,107`; `durability.go:1464,1532`; `residency.go:108,158,179`; `repository.go:400` | verified |
| E12 | All these tables share one `*sql.DB`, opened with `_txlock=immediate`. | `app.go:357,525`; `internal/history/db.go:79-80` | verified |
| E13 | `seedJobFiles` must precede `Add` (#552): registering first lets a failed seed report "failed to add" for a job already downloading. | comment at `app.go:762-768` | verified |

Corrections to #560's own text, for whoever edits it: `MoveToHistory` is not a
function (comments only); Candidate 1's stated blocker ("must absorb the
barrier's commit, weakening §6") is false by E10; #557's description says "one
transaction per orphan" but its code runs one for the whole sweep.

## 3. Design

### 3.1 Prerequisite P1 — the queue row exists before `Add` returns

`Dispatcher.Add` gains a context and persists synchronously, through the same
function the tick uses — and **the tick does not see a job until its queue row
has been written**:

```go
func (d *Dispatcher) Add(ctx context.Context, j *job.Job, h Header) error {
	// ... stopped/restoring checks, SetAdded, unchanged ...
	if err := d.register(j, h, seqNext); err != nil {
		return err
	}
	// The first row, written here rather than by a tick, so that a caller
	// acknowledging the job has a durable reason to.
	if err := d.persistIfChanged(ctx, j); err != nil {
		// Nothing but the registry holds this job: the tick has not seen it
		// (snapshotOrder), so there is no lease, no residency and no worker.
		// Unwind through the removal gatekeeper, the only path to deregister.
		if rm, ok := d.beginRemoval(j.ID()); ok {
			rm.end()
		}
		return fmt.Errorf("dispatch: Add: persist %s: %w", j.ID(), err)
	}
	d.kick() // register's kick may have been spent on a tick that skipped j
	return nil
}

// snapshotOrder returns the registered jobs that have a written queue row.
// It is the tick's and Stop's only view of the registry, so an unwritten job
// is never advanced, hydrated, launched or persisted by either.
func (d *Dispatcher) snapshotOrder() []*job.Job {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]*job.Job, 0, len(d.order))
	for _, id := range d.order {
		if _, ok := d.written[id]; !ok { // read under the held d.mu
			continue
		}
		out = append(out, d.byID[id].j)
	}
	return out
}
```

- **Why the gate sits at the top of the tick, not at launch.** `register`
  kicks the tick (`internal/dispatch/registry.go:205`), and the tick acts on a
  job well before it launches it (`internal/dispatch/tick.go:30-62`). The
  first tick runs `Advance`, whose never-run branch calls `BeginAttempt`, and
  then `persistIfChanged`. The next runs `Advance`'s resume branch, which takes
  a lease through `grantFor`, and `reconcileResidency`, which hydrates once the
  job holds one. A synchronous persist can span several ticks, since
  `busy_timeout(5000)` lets a contended write wait five seconds
  (`internal/history/db.go:80`). So a gate only at `claimLaunched` still lets
  a failed `Add` leak a scheduler lease and a hydrated manifest. Worse, a tick
  that persists the job after `Add`'s write has failed but before `Add` unwinds
  leaves a row that the unwind does not delete. The job then reappears at the
  next start, after the caller was told the add failed.
  Filtering in `snapshotOrder` keeps the tick away from the job entirely, and
  makes `Add` the only writer of a job's first row.
- **The unwind goes through `beginRemoval` → `removal.end`.** `removal.end`
  is `deregister`'s only caller, which is the #513 gatekeeper
  (`internal/dispatch/registry.go:404-412`). A bare `deregister` in `Add` would
  be the first caller to bypass it.
- **`Add` kicks after the persist.** `register`'s kick can be consumed by a
  tick that runs while `Add` is still writing, which then skips the job. Without
  a second kick, the job waits for the next tick of the one-second ticker.
- **One enforcement point.** Only the tick launches (`d.launch` is called only
  at `internal/dispatch/tick.go:61`), so the gate in `snapshotOrder` makes a
  second check in `claimLaunched` redundant, and there is none.
- Nothing changes on the normal path for existing jobs: those restored at
  startup are marked written as they load (`internal/dispatch/dispatch.go:716`),
  before the tick starts.
- (Revision after cross-critique, twice: design B found both that a failed
  persist could deregister a launched job, and that a launch-only gate leaves
  the lease, residency and removal-gatekeeper problems above. The orphaned-row
  case follows from the same gate.)
- `dispatch_jobs` keeps exactly one writer function; nothing new acquires
  `storeMu` or `d.mu` differently (`register` releases `d.mu` before returning,
  and `persistIfChanged` already runs under `storeMu`).
- Closes E2 and E5 outright: retry's history delete now always follows a
  durable queue row.
- `AddJob` and `RetryHistoryJob` pass a detached, bounded context
  (`context.WithoutCancel` + timeout), because the request context must not be
  able to abandon a persist the client will be told succeeded.
- This is justified independently of #560 by E2, and every candidate needs it:
  an FK, a sweep and an `EXISTS` guard all misbehave inside the span it closes.
  It could land first, alone, with the crash test as its regression pin.

### 3.2 The owner — `durability.Store`

`SQLiteRunStore` grows into `durability.Store`, a concrete type owning three
tables. Its methods are lifecycle events, not CRUD.

**The API takes plain types only.** `internal/job` imports
`internal/durability` (`go list -f '{{join .Imports "\n"}}' ./internal/job`),
so `durability` cannot name `job.FetchPolicy` or `job.Checkpoint`, and cannot
implement `checkpoint.Store`, whose method takes `[]job.Checkpoint`.
`appCheckpointStore` therefore stays in `internal/app` as a thin adapter:
it maps each `job.Checkpoint` to a `durability.JobProgress` and calls
`SaveProgress`. (Revision after cross-critique: the first version of this
section gave `durability.Store` `job`-typed signatures, which is an import
cycle. Design B's plain-type API was correct.)

```go
type Store struct{ db *sql.DB }

type FileProgress struct {
	FileIndex      int
	Complete       bool
	FetchPolicy    uint8 // job.FetchPolicy's underlying type
	Filename       string
	AssembledCRC32 uint32
}

type JobProgress struct {
	JobID          string
	Files          []FileProgress
	FailedArticles []int
}

// PerJobTables is the machine-readable list of the tables this package owns,
// for test fixtures and for a completeness test over Reclaim (8.2).
// Adopted from design B.
var PerJobTables = []string{"durable_runs", "failed_articles", "job_files"}

// Admit seeds one job_files row per file, fetch[i] being file i's policy.
// Moves seedJobFiles's SQL: same single transaction, same ON CONFLICT DO NOTHING.
func (s *Store) Admit(ctx context.Context, jobID string, fetch []uint8) error

// SaveProgress moves appCheckpointStore.SaveBatch's SQL, adding the
// liveness guard (3.5). One transaction for the whole batch.
func (s *Store) SaveProgress(ctx context.Context, batch []JobProgress) error

// Reads that today are raw SQL in residency.go:108,158,179.
func (s *Store) FileRows(ctx context.Context, jobID string) ([]FileRow, error)
func (s *Store) FailedArticles(ctx context.Context, jobID string) ([]int, error)
func (s *Store) ForJob(ctx context.Context, jobID string) ([]Run, error)
func (s *Store) ForFile(ctx context.Context, jobID string, fileIdx int32) ([]Run, error)

// DiscardRuns invalidates a job's durable record — the retry's
// "!progressApplied" case (app.go:2261). Content management, not lifecycle.
func (s *Store) DiscardRuns(ctx context.Context, jobID string) error

// Reclaim applies the reclaim rule to the named jobs. At least one id is
// required by the signature, so a runtime caller cannot reach the
// every-job form by passing nothing. One transaction. See 3.3.
func (s *Store) Reclaim(ctx context.Context, id string, more ...string) error

// SweepOrphans applies the same rule to every job. Startup only, before the
// API and dir scanner start (main.go:223 vs :257): a job between Admit and Add
// has job_files but no queue row yet, and the rule would reclaim it.
// (Revision after cross-critique: design B found the race.)
func (s *Store) SweepOrphans(ctx context.Context) error

// commit is unexported: only package durability can write run content.
func (s *Store) commit(ctx context.Context, jobID string, arts []DurableArticle) ([]Collision, error)
```

**§6 becomes compiler-enforced.** Today the exclusive-writer property holds by
convention: `Application` holds the same `RunStore` the barrier does and could
call `Commit` (`app.go:164,525`). With `commit` unexported, only code in
package `durability` can write run content, and the barrier is that code.
`Resumer`'s per-file discard (`resume.go:148`) and `commit`'s own
read-modify-write delete stay in-package as content operations, which is what
§6 already says they are.

### 3.3 The reclaim rule — stated once

```sql
-- One transaction. :filter is "AND job_id = ?" per id for Reclaim, and empty
-- for SweepOrphans. The two share these statements, so the rule has one text.
DELETE FROM job_files
 WHERE NOT EXISTS (SELECT 1 FROM dispatch_jobs d WHERE d.id = job_files.job_id) :filter;

DELETE FROM failed_articles
 WHERE NOT EXISTS (SELECT 1 FROM dispatch_jobs d WHERE d.id = failed_articles.job_id) :filter;

DELETE FROM durable_runs
 WHERE NOT EXISTS (SELECT 1 FROM dispatch_jobs d WHERE d.id = durable_runs.job_id)
   AND NOT EXISTS (SELECT 1 FROM history h
                    WHERE h.nzo_id = durable_runs.job_id AND h.status = 'Failed') :filter;
```

What this buys:

- **The retention rule exists in one place.** Today it is decided at
  `job_finalizer.go` (`shouldDeleteDurability`, plus a history re-read on
  persist error), `dropJobAlreadyInHistory`, `history.Repository.delete`'s
  `dropDurability` branch, and `DeleteKeepingDurability`'s existence. All four
  go.
- **It is the smallest correct retention (E6):** a failed job keeps
  `durable_runs` only. `job_files` and `failed_articles` are reclaimed at
  departure, which also removes E7's hazard at the source — there are no stale
  failed marks left for a retry to fail to clear.
- **Call sites stop needing to know what happened.** `Reclaim(id)` is
  idempotent and consults the truth, so it is safe to call after a departure
  that succeeded, failed, or was done by someone else. That is exactly E8's
  case: `RemoveJob` can call it on `ErrNotFound`.
- **Doubt is not a category any more.** #559 had to distinguish "not in
  history" (knowledge) from "lookup failed" (doubt) in Go. Inside one SQL
  transaction a failed read aborts the transaction and deletes nothing.
- **`NOT EXISTS` is NULL-safe by construction.** #557 needed a dedicated test
  for the `NOT IN` + NULL trap; this form cannot express it.

### 3.4 Call sites — the enumeration

Every event that can make a job unreachable, and what calls `Reclaim` after it.
Manifest unlink rides along in two app helpers: `app.reclaim(ctx, id, more...)`
runs `store.Reclaim` and then unlinks the manifest of each named id with no
queue row; `app.sweepOrphans(ctx)` runs `store.SweepOrphans` and then unlinks
every manifest in the manifests directory with no queue row. A manifest's
lifetime is exactly the queue row's: finalize unlinks it on removal, and retry
writes a fresh one.

| Event | Site today | Change |
|---|---|---|
| Remove from queue | `RemoveJob` `app.go:871` | `app.reclaim(id)` after `Remove`, **including on `ErrNotFound`** (E8) |
| Tick evicts a cancelled never-run job | `tick.go` `evictCancelledNeverRun` | covered by the row above; its only trigger is `RemoveJob`'s `Cancel` |
| Finalize (done or failed) | `persistAndCommit` `job_finalizer.go:218-229` | `app.reclaim(id)` unconditionally; `shouldDeleteDurability` and the history re-read go |
| Startup reconciliation | `dropJobAlreadyInHistory` `durability.go:1386,1419` | `app.reclaim(id)`; its FAILED rule goes |
| Delete history entries | `app.go:1046` | `app.reclaim` over the deleted ids, after the delete |
| Retry | `app.go:2347` | `history.Delete` (plain); `Reclaim` after it is a no-op by P1, called anyway |
| Mark completed | `internal/api/history.go:284` → repo | route through an app method that calls `app.reclaim(id)` (E9) |
| `AddJob` / retry fails after `Admit` | `app.go:779`, `:2342` | `app.reclaim(id)` on the error path |
| Anything missed, or a crash between an event and its reclaim | — | `app.sweepOrphans()` at startup, before the API and dir scanner start (`main.go:223` vs `:257`) — and never at runtime (3.2) |

A missed call site costs disk until the next start. It cannot cost data,
because the rule never deletes anything reachable.

### 3.5 #561 — a liveness guard owned by the store

`SaveProgress` inserts a failed article only while the job is live **to the
store**, using its own table as the marker:

```sql
INSERT OR IGNORE INTO failed_articles (job_id, art_idx)
SELECT ?1, ?2 WHERE EXISTS (SELECT 1 FROM job_files WHERE job_id = ?1)
```

- `_txlock=immediate` serialises the flush against `Reclaim`: a flush that
  commits first has its rows reclaimed; one that commits after inserts nothing.
- Keyed on `job_files` rather than `dispatch_jobs` on purpose. The adversarial
  pass showed a `dispatch_jobs` guard drops legitimate writes for a new job
  between `launch` and its first persist, and would undo #329's fix if extended
  to `job_files`. `job_files` is seeded by `Admit` before `Add` (E13) and
  before retry's flush, so the guard is true for every live job and false only
  after `Reclaim`.
- Cost: one indexed probe (`UNIQUE(job_id, file_index)` prefix), only when a
  job has failed articles; hoistable to once per job per batch.

**Paired with `Checkpointer.Prune` waiting for an in-flight flush.** The guard
cannot tell two uses of one job ID apart: a batch captured before a failure
that commits after the retry's `Admit` passes it and writes stale failed marks.
So `Prune(id)` also waits on `flushMu` when `id` is in `inFlight`, and a
departure's `Reclaim` always follows its `Prune` (`RemoveJob`,
`persistAndCommit`). No batch holding a departed job can then commit after the
reclaim.

- This **changes `Prune`'s contract**. Its doc says today that "a write batch
  already handed to the store still completes"
  (`internal/checkpoint/checkpointer.go:64-66`), and #561 is that sentence's
  consequence. The doc changes with the code.
- The two mechanisms cover **different windows**, which is why neither is a
  second enforcement point for the other. `Prune`'s wait covers a batch already
  in flight at departure. The guard covers a `Mark` arriving *after* `Prune`,
  which `Prune` cannot see: `Mark` has 8 production call sites, including
  download workers in `internal/app/pipeline.go`, and nothing yet shows a
  straggler cannot reach it.
- The wait blocks only when the race is live: `inFlight` holds `id` only during
  a flush whose batch contains it.

(Revision after cross-critique: design B proposed the `Prune` wait.)

### 3.6 `history_job_files` — the entry's child

- **No foreign key.** Ruled out on 2026-09-18 along with every other
  schema-enforced lifecycle: a cascade splits the lifecycle between the Go code
  and the schema, so no one place states it. `internal/history` owns both the
  entry and its retained file progress, so it deletes them together — as
  `history.Repository.delete` already does, in the same transaction as the
  history row (`internal/history/repository.go:363`).
- Written inside `history.Add`'s transaction (the entry carries its file
  progress) instead of as separate `_, _ =` statements after it
  (`job_finalizer.go`, discarded errors today). An entry and its retained
  progress then land or fail together.
- `history.Repository.delete` keeps its `history_job_files` delete and loses
  every durability clause: no `durable_runs`/`failed_articles` delete
  (`Reclaim`), no `dropDurability` flag, no `DeleteKeepingDurability`.
  `internal/history` stops knowing the durability tables exist.

### 3.7 What is deleted

`deleteJobDurability`, `dropJobDurability`, `appCheckpointStore`'s SQL (the
type remains as the `job`→`durability` mapping adapter, 3.2), `seedJobFiles`
(moved), the three raw reads in `residency.go`, retry's raw
`failed_articles` delete (`app.go:2267`), `shouldDeleteDurability` and its
history re-read, `DeleteKeepingDurability`, `history.delete`'s concatenated
deletes, and — relative to #557 — `Execer`, `RunStore.DeleteJobTx` and the
separate sweep: the startup sweep is `SweepOrphans`, which shares `Reclaim`'s
statements.

## 4. Scored against the agreed criteria

| Criterion | Result |
|---|---|
| 1. Crash window | **Narrowed and made safe, not closed.** No crash can cause a wrong deletion (the rule is re-derived). A crash between a state change and its reclaim leaves rows until the next start. P1 separately closes the two windows that lost *jobs* (E2, E5). An FK cascade would close the row window at the database; this design does not, and trades that for having no ordering constraints at the call sites. |
| 2. §6 | **Strengthened**: compiler-enforced at package granularity (`commit` unexported). |
| 3. Rule 2 | Three tables: writers and readers in one package; one lifecycle deleter (`Reclaim`); content invalidation (`commit`'s merge, `Resumer`, `DiscardRuns`) in-package. `history_job_files`: one writer (`history.Add`), one deleter (`history.Repository.delete`, in the entry's transaction). Raw SQL statement sites on these tables outside `internal/durability` (production, excluding `test/`): 10 → 0 — nine in `internal/app` (E11) plus `repository.go:400`. |
| 4. #561 | **Fixed** by 3.5; residual below. |
| 5. Contortions | (a) The guard uses `job_files` presence as the liveness marker — correct, but implicit; an explicit marker would be a table added only for this. (b) The rule reads two tables another package owns (`dispatch_jobs`, `history.status`): reading is permitted by Rule 2, but it couples the rule to their schema. (c) Manifest unlink is outside the transaction (a file); idempotent and caught at startup. None exists solely to satisfy another criterion except arguably (a). |
| 6. Runtime | `Add`: one synchronous row write the tick would have done milliseconds later. Departure: one transaction, three indexed anti-join deletes. `SaveBatch`: one indexed probe per job with failed articles. Startup: three anti-join deletes plus a manifest-directory scan. |

## 5. Residual risks — attack these

1. **Reused job ID across a retry — narrowed, not closed.** `Prune`'s wait
   (3.5) closes the in-flight batch. What remains is a straggling `Mark` of the
   *old* job object that lands after the retry's `Admit`: the guard passes, and
   stale failed marks are written. An attempt generation on `job_files` would
   close it at the cost of a column; whether a straggler can reach `Mark` after
   departure at all is unestablished.
2. **The row window stays open until restart.** A daemon up for months keeps
   whatever a crash stranded between an event and its reclaim. The same bound
   as #557, reached with far fewer mechanisms. **A periodic sweep is not
   available as a fix:** an unfiltered pass would reclaim any job between
   `Admit` and `Add`, which is why `SweepOrphans` is startup-only (3.2).
   (An earlier version of this item called a periodic sweep "cheap"; design B
   showed it is unsafe.)
3. **Power loss.** `synchronous(NORMAL)` with WAL can lose recent commits on
   power loss (not on process crash), so P1's guarantee is "survives a crash",
   not "survives a power cut". Same for every candidate.
4. **`Add` now does I/O.** Its callers (API handler, ingest adapter, retry)
   must tolerate a persist failure as a failure to add. `persistIfChanged` runs
   under `storeMu`; `check_lock_io` should be re-run, since it does not descend
   into un-suffixed callees.
5. **Schema coupling.** If `history.status` values or `dispatch_jobs.id`
   change, the rule silently changes meaning. Pin it with a test that
   exercises each branch against the real schema (8.2).
6. **Deleting a failed entry while retrying it.** A user who deletes a failed
   history entry during its own retry — after the retry's `Admit`, before its
   `Add` persists — makes the delete's `Reclaim(id)` see a job with neither a
   queue row nor a history row, and it reclaims the retry's fresh `job_files`
   and the `durable_runs` the retry is about to read. Two concurrent user
   actions on one entry; accepted rather than designed around, but a per-job
   lock around retry and history delete would close it.

## 6. What this design does not do

- It does not make the database enforce the lifecycle — **decided on
  2026-09-18**: foreign keys are ruled out because they split the lifecycle
  logic between Go and the schema. For the record, the comparison that stood
  against them E-for-E: FKs close
  the row window but need P1 anyway (the spike's single root cause was E1), a
  reparenting copy for `durable_runs` (two copies: out at failure, back at
  retry), and a copy-before-cascade ordering at every site that removes a queue
  row for a possibly-failed job.
- It does not own the manifest file or the `admin/nzb` backup. The manifest is
  reclaimed by the same helper; the backup's lifetime belongs to the history
  entry and is out of scope.

## 7. Implementation sketch

Each step builds, passes the gates, and is independently revertable.

1. **`fix(dispatch): persist a job's queue row before Add returns`** — P1,
   `Add(ctx, …)`, the written-row filter in `snapshotOrder`, the unwind through
   `beginRemoval`/`removal.end`, the post-persist kick, and callers passing a
   detached bounded context. Regression pins: the crash test and the
   unwritten-job test (8.1).
2. **`refactor(durability): make one store own job_files and failed_articles`**
   — move the SQL of `seedJobFiles` and `SaveBatch`, the residency reads and
   `DeleteJob` into `durability.Store` behind the plain-type API of 3.2, leaving
   `appCheckpointStore` as the mapping adapter; unexport `commit`. Behaviour-neutral; the existing suite
   is the check.
3. **`fix(app): reclaim a job's rows by one rule instead of four`** — `Reclaim`
   and `SweepOrphans` plus their app helpers, every call site in 3.4, startup
   `SweepOrphans`; delete the
   list in 3.7. Behaviour change: a failed job's `job_files` and
   `failed_articles` go at departure (E6/E7).
4. **`fix(checkpoint,durability): stop a late flush resurrecting a removed job's rows`**
   — the 3.5 guard, and `Prune` waiting for an in-flight flush that holds the
   job, with `Prune`'s doc changed to match. Closes #561.
5. **`refactor(history): write retained file progress in its entry's transaction`**
   — `history.Add` writes `history_job_files` in its own transaction;
   `history.Repository.delete` keeps deleting them with the entry and sheds all
   durability clauses. No schema change.

## 8. Test plan and red checks

### 8.1 Crash test (P1)
`test/crash`: kill immediately after the `mode=addfile` 200, restart, assert
the job is present. Observed red today in 10/15; the positive control (kill
after 1.5s) is 5/5 green. Source in Appendix A. Run with
`-count` ≥ 10, since it is a race.

Unwritten job: a `dispatch.Store` wrapper whose `Save` blocks until released,
then fails. While it blocks, drive several ticks (the pattern `tick_test.go`
already uses). Once it fails, assert that `Add` returns the error, that the job
never began an attempt, holds no lease and was never hydrated, that no worker
launched and no row was saved, and that the job is absent from `List()`.
Mutation: remove the `d.written` filter from `snapshotOrder` → `BeginAttempt`,
a lease or a save is observed. A second mutation drops `Add`'s post-persist
`kick`; a success-path test that consumes the wake during the blocked `Save`
must then see the launch wait for the ticker.

### 8.2 Reclaim rule — one table-driven test, every branch
A job in each state, against the real schema, through both `SweepOrphans` and
`Reclaim(id)` per job — the two share their statements, and the test proves it:

| State | job_files | failed_articles | durable_runs |
|---|---|---|---|
| in queue | kept | kept | kept |
| in queue **and** failed in history (retry overlap) | kept | kept | kept |
| failed in history only | gone | gone | **kept** |
| completed in history | gone | gone | gone |
| in neither | gone | gone | gone |

The table's columns are generated from `PerJobTables`, not written out, so a
fourth owned table fails this test until `Reclaim` covers it.

Mutations (`scripts/mutate` spec), each must be KILLED:
- drop the history clause from the `durable_runs` delete → "failed in history
  only" loses runs;
- drop the `dispatch_jobs` clause from any delete → "in queue" loses rows;
- `status = 'Failed'` → `'Completed'` → both history rows flip;
- `NOT EXISTS` → `NOT IN` with a NULL `dispatch_jobs.id` seeded → nothing
  reclaimed (the #557 trap, pinned by the same fixture).

### 8.3 Call sites
- `RemoveJob` where the tick evicts first (E8): drive `Cancel`, let the tick
  evict, then `RemoveJob` → rows and manifest gone. Mutation: skip reclaim on
  `ErrNotFound` → survives.
- `MarkCompleted` on a failed entry → `durable_runs` gone.
- Finalize FAILED → only `durable_runs` remain; finalize done → nothing.
- History delete of a failed entry → `durable_runs` gone (via `Reclaim`),
  `history_job_files` gone in the entry's own transaction.

### 8.4 #561
Deterministic interleaving through a store wrapper (the `dispatch.Store`
injection pattern from #559): capture a batch, `Reclaim`, then commit the
batch → zero `failed_articles`. Mutation: remove the `EXISTS` → rows
resurrected.

`Prune` wait: hold a flush mid-`SaveBatch` whose batch contains the job, call
`Prune` from another goroutine, and assert it has not returned until the flush
commits. Mutation: drop the wait → `Prune` returns first.

### 8.5 §6
A compile-time check is the pin: code outside `internal/durability` calling
`commit` does not build. Keep one `go vet`-level test asserting `Store` has no
exported method that writes `durable_runs` content.

## 9. Questions for the critique

1. ~~Is re-deriving the rule on every departure, instead of cascading, the
   simpler system — or only the smaller diff?~~ Settled 2026-09-18: no
   cascades (3.6, section 6).
2. Is `job_files` presence an acceptable liveness marker, or is the implicit
   marker a contortion that an explicit one should replace?
3. Should the store own the manifest file too?
4. Does any departure path exist that 3.4 misses? (Rule 4: enumerate from
   source, `git grep` for `store.Delete`, `dispatcher.Remove`, history
   deletes and status updates.)

## Appendix A — crash-test reproduction of E2

<!-- doccite:ok TestSIGKILL_AddThenImmediateKill — proposed test, source below; it lands with step 1 of the sketch -->
<!-- doccite:ok TestSIGKILL_AddThenImmediateKill_PositiveControl — proposed test, source below; not in the tree yet -->
<!-- doccite:ok TestSIGKILL_AddThenImmediateKill_LosesTheJob — proposed test, source below; not in the tree yet -->

```go
// Reproduction of F2 (accepted job lost by SIGKILL before the dispatcher's
// woken tick persists its dispatch_jobs row). Throwaway spike; keep as the
// starting point for the regression test of the real fix.
//
// Observed 2026-09-17 against f83c366d:
//   positive control (kill 1.5s after add): job present, 5/5
//   red case (kill immediately after the addfile HTTP 200): job ABSENT 10/15
//   every loss left 1 job_files row + the gzipped manifest orphaned.
// Path: HTTP mode=addfile -> modeAddFile -> enqueueNZBData -> Application.AddJob.
// Run: go test -tags=crash -count=5 -timeout=20m -run TestSIGKILL_AddThenImmediateKill ./test/crash/ -v

//go:build crash && linux

package crash

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func lostAddFixture() harnessOpts {
	return harnessOpts{
		CheckpointBytes:    1 << 20,
		CheckpointInterval: time.Hour,
		WriteCacheBytes:    1 << 20,
		Connections:        1,
		BodyDelay:          50 * time.Millisecond,
		Files:              []fileSpec{{Name: "payload.bin", Size: 4 << 20, PartSize: 128 << 10}},
	}
}

func manifestPathFor(adminDir, jobID string) string {
	return filepath.Join(adminDir, "queue", "manifests", jobID+".json.gz")
}

func jobFilesRowCount(t *testing.T, h *harness, jobID string) int {
	t.Helper()
	db := h.openDB()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM job_files WHERE job_id = ?`, jobID).Scan(&n); err != nil {
		t.Fatalf("count job_files for %s: %v", jobID, err)
	}
	return n
}

func TestSIGKILL_AddThenImmediateKill_PositiveControl(t *testing.T) {
	h := newHarness(t, lostAddFixture())
	jobID := h.AddJob()
	time.Sleep(1500 * time.Millisecond)
	h.Kill()
	h.Restart()
	if _, ok := h.Slot(jobID); !ok {
		t.Fatalf("job %s is missing from the queue after a kill that waited 1.5s past the add; "+
			"the positive control itself failed, so the red case proves nothing", jobID)
	}
}

func TestSIGKILL_AddThenImmediateKill_LosesTheJob(t *testing.T) {
	h := newHarness(t, lostAddFixture())
	jobID := h.AddJob()
	h.Kill()

	rowsBefore := jobFilesRowCount(t, h, jobID)
	manifestPath := manifestPathFor(h.AdminDir, jobID)
	_, manifestErrBefore := os.Stat(manifestPath)

	h.Restart()

	slot, ok := h.Slot(jobID)
	if !ok {
		t.Errorf("job %s is ABSENT from the queue after an immediate SIGKILL following AddJob", jobID)
	} else {
		t.Logf("job %s survived the kill and restart with status %q", jobID, slot.Status)
	}
	t.Logf("job_files rows for %s at kill time: %d", jobID, rowsBefore)
	if !ok && rowsBefore > 0 {
		t.Logf("ORPHAN: %d job_files row(s) remain with no queue entry", rowsBefore)
	}
	if !ok && manifestErrBefore == nil {
		t.Logf("ORPHAN: manifest %s remains with no queue entry", manifestPath)
	}
}
```
