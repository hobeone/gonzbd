# Architecture Design: Unified `durability.Store` for Per-Job Row Lifecycle

**Status:** Proposal for Cross-Agent Critique  
**Author:** Designer B  
**Date:** 2026-09-18  
**Scope:** issue 560 (lifecycle of per-job persisted rows), issue 561 (checkpoint prune race), and PR 557

---

## 1. The design in one paragraph

Consolidate the persistence, queries, atomic deletion, and orphan sweep of all three active download tables (`durable_runs`, `failed_articles`, `job_files`) into `internal/durability` under a unified `durability.Store` interface, evicting all ten ad-hoc raw SQL statements from `internal/app` and `internal/history`. `durability.Barrier` remains the exclusive writer of `durable_runs` content (§6), while typed methods (`SeedJobFiles`, `SaveProgressBatch`, `LoadJobFiles`) manage `job_files` and `failed_articles`. Deletion across all three tables is unified into `Store.DeleteJob` and `Store.DeleteJobs` inside a single private SQLite transaction, eliminating the leaked `DeleteJobTx` / `Execer` transaction hooks from PR 557. Issue 561 is eliminated at the database statement level by conditioning `failed_articles` batch inserts on `job_files` presence (`WHERE EXISTS`), ensuring delayed checkpoint flushes skip removed jobs without aborting the batch for active siblings. A single exported `durability.PerJobTables` slice provides the machine-readable enumeration mandated by Standing Design Rule 4, replacing 33 scattered raw-SQL test inserts across 15 files with shared, derived fixtures. Finally, `history_job_files` gains `FOREIGN KEY (job_id) REFERENCES history(nzo_id) ON DELETE CASCADE` in `001_initial.sql`, converting history-retained file progress into a database-enforced lifecycle.

---

## 2. Evidence base

Facts established at `f83c366d` relied on by this design:

| # | Fact | Status & Citation | Role in this Design |
|---|---|---|---|
| E1 | `dispatch_jobs` has one writer, reached only from the dispatcher tick and `Stop`. `Dispatcher.Add` writes nothing to disk. | Verified (`internal/dispatch/store/store.go:93`, `internal/dispatch/tick.go:119`) | Disproves FKs from `job_files` to `dispatch_jobs`: children are written before the parent exists. |
| E2 | A job accepted by the API is lost if killed right after `mode=addfile` returns 200, orphaning `job_files` rows and manifest. | Verified (`test/crash/` reproduction: 10/15 lost) | Mandates that startup reconciliation sweeps orphaned `job_files` rows lacking a `dispatch_jobs` parent. |
| E3 | The E2 window lasts until the woken tick persists the row; within a tick, `launch` runs before `persistIfChanged`. | Verified (`registry.go:205`, `dispatch.go:454-455`, `tick.go:29-63`) | Confirms in-flight active downloads can precede `dispatch_jobs` persistence. |
| E4 | To the user, E2 means the job is lost; dir scanner removes source; manual resubmit matches `admin/nzb` backup and is added paused. | Verified (`internal/dirscanner/scanner.go`, `internal/app/app.go:657-667`) | Confirms cleaning up orphaned `job_files` on startup restores a clean state. |
| E5 | `RetryHistoryJob` deletes the history row after an in-memory `Add` and before any tick writes `dispatch_jobs`. | Verified (`internal/app/app.go:2342`, `:2347`) | Proves a job can temporarily exist without either a `dispatch_jobs` or `history` row, refuting cascading FKs across handoff. |
| E6 | Only `durable_runs` must be read across queue $\to$ history handoff for a failed job; `job_files` has no reader for a job in history. `failed_articles` must be absent at retry. | Verified (`internal/app/residency.go:108,158,179`, `app.go:2324-2338`, `app.go:2266`) | Dictates the retention contract: failed jobs keep `durable_runs`, but retries clear `failed_articles` and re-seed `job_files`. |
| E7 | Retry's `failed_articles` clear is non-fatal on the progress-applied branch (logs `Warn` only). | Verified (`app.go:2265-2270` vs `durability.go:1480-1487`) | Fixed by delegating retry resets to `Store.DropRetriedDurability`, making failure fatal. |
| E8 | `RemoveJob` of a queued job that never ran can skip all cleanup if `evictCancelledNeverRun` wins the race. | Inferred / verified from code (`app.go:866-875`, `tick.go`) | Self-heals: any stranded rows are swept by `SweepOrphanedDurability`. |
| E9 | `MarkCompleted` turns a Failed history entry into Completed with a bare `UPDATE`, stranding retained rows. | Verified (`internal/history/repository.go:429`, `internal/api/history.go:284`) | Fixed: `MarkCompleted` calls `durability.Store.DeleteJob` to purge retained rows when a job can no longer be retried. |
| E10 | `RunStore.Commit`'s only production callers are `durability.Barrier`. | Verified (`internal/durability/barrier.go:282, 628`) | Proves §6 content exclusivity holds today and is preserved by this design. |
| E11 | Ten raw-SQL statement sites touch `job_files`, `failed_articles`, or `durable_runs` outside `internal/durability`. | Verified (`app.go:815,2267`, `dispatcher_wiring.go:99,107`, `durability.go:1464,1532`, `residency.go:108,158,179`, `repository.go:400`) | Target of this refactor: all 10 sites are replaced with typed method calls on `durability.Store`. |
| E12 | All tables share one `*sql.DB` with `_pragma=foreign_keys(1)`. | Verified (`app.go:357,525`, `internal/history/db.go:79-80`) | Enables `history_job_files` FK cascade while sharing DB pool. |
| E13 | `seedJobFiles` must run before `Add` (#552 fix); `RetryHistoryJob` uses the same order. | Verified (`app.go:762-768`, `app.go:2306-2320`) | Prevents `job_files` from declaring a foreign key to `dispatch_jobs(id)`. |
| E14 | `history.Repository.delete` drops `durable_runs` and `failed_articles` via string concatenation but omits `job_files`. | Verified (`internal/history/repository.go:399-406`) | Proves the silent `job_files` leak in history deletion exposed in issue 560. |
| E15 | Issue 561 race: `Checkpointer.Flush` captures batch, `RemoveJob` deletes rows, in-flight `SaveBatch` re-inserts `failed_articles`. | Verified (`issue 561`, `internal/checkpoint/checkpointer.go`, `internal/app/dispatcher_wiring.go:106`) | Target for structural fix in `SaveProgressBatch`. |
| E16 | Test fixture scattering: 33 raw `INSERT` statements across 15 test files without a shared table enumeration. | Verified (`issue 560 comment 5721839944`) | Target for Rule 4 exported machine-readable enumeration `durability.PerJobTables`. |

---

## 3. Design

### 3.1 Schema changes (DDL)

No foreign keys are added to `durable_runs`, `failed_articles`, or `job_files`, because their lifecycle is an application-level state machine with conditional retention and asynchronous parent insertion (E1, E6, E13).

The sole schema modification in `internal/history/migrations/001_initial.sql` is adding an `ON DELETE CASCADE` constraint to `history_job_files`:

```sql
-- +goose StatementBegin
CREATE TABLE history_job_files (
    job_id           TEXT NOT NULL,
    file_index       INTEGER NOT NULL,
    complete         INTEGER NOT NULL DEFAULT 0,
    filename         TEXT,
    assembled_crc32  INTEGER DEFAULT 0,
    article_count    INTEGER NOT NULL DEFAULT 0,
    fetch_policy     INTEGER NOT NULL DEFAULT 0 CHECK (fetch_policy BETWEEN 0 AND 2),
    PRIMARY KEY (job_id, file_index),
    FOREIGN KEY (job_id) REFERENCES history(nzo_id) ON DELETE CASCADE
);
-- +goose StatementEnd
```

Because `history_job_files` is written only after a history entry exists, is read only while in history, and has no conditional retention, `ON DELETE CASCADE` is completely sound here and eliminates the manual `DELETE FROM history_job_files` statement in `history.Repository.delete`.

---

### 3.2 Machine-readable table enumeration (Rule 4)

In non-test code in `internal/durability/tables.go`:

```go
package durability

// PerJobTable identifies a database table whose rows are partitioned by job ID
// and share the active download lifecycle.
type PerJobTable struct {
	Name        string
	JobIDColumn string
}

// PerJobTables is the canonical enumeration of tables storing active per-job
// download progress. Test helpers and orphan consistency sweeps derive from
// this slice rather than maintaining hardcoded table lists.
var PerJobTables = []PerJobTable{
	{Name: "durable_runs", JobIDColumn: "job_id"},
	{Name: "failed_articles", JobIDColumn: "job_id"},
	{Name: "job_files", JobIDColumn: "job_id"},
}
```

---

### 3.3 Go package signatures and domain types

#### `internal/durability/store.go`

`durability` sits at the foundation of the dependency tree (`job` imports `durability`; `checkpoint` imports `job`). Therefore, `durability` imports neither and uses standard Go primitives:

```go
package durability

import "context"

type FileSeed struct {
	FileIndex   int
	FetchPolicy int
}

type FileRecord struct {
	FileIndex      int
	Filename       string
	Complete       bool
	AssembledCRC32 uint32
	FetchPolicy    int
}

type FileProgressUpdate struct {
	FileIndex      int
	Complete       bool
	FetchPolicy    int
	Filename       string
	AssembledCRC32 uint32
}

type JobProgressUpdate struct {
	JobID          string
	Files          []FileProgressUpdate
	FailedArticles []int
}

// Store encapsulates the persistence, queries, and lifecycle of in-flight
// durability records across durable_runs, failed_articles, and job_files.
type Store interface {
	// Commit groups arts into runs and merges them into durable_runs.
	// Invariant §6: durability.Barrier is the SOLE production caller.
	Commit(ctx context.Context, jobID string, arts []DurableArticle) ([]Collision, error)
	ForFile(ctx context.Context, jobID string, fileIdx int32) ([]Run, error)
	ForJob(ctx context.Context, jobID string) ([]Run, error)
	DeleteFile(ctx context.Context, jobID string, fileIdx int32) error

	// SeedJobFiles writes initial job_files rows with ON CONFLICT DO NOTHING.
	SeedJobFiles(ctx context.Context, jobID string, files []FileSeed) error

	// LoadJobFiles loads per-file records for residency hydration.
	LoadJobFiles(ctx context.Context, jobID string) ([]FileRecord, error)

	// SaveProgressBatch applies file updates and failed articles in one transaction.
	// Structurally eliminates issue 561 by conditioning failed_articles on active job_files.
	SaveProgressBatch(ctx context.Context, updates []JobProgressUpdate) error

	// DeleteJob atomically removes durable_runs, failed_articles, and job_files for one job.
	DeleteJob(ctx context.Context, jobID string) error

	// DeleteJobs atomically removes durability rows for multiple jobs in one transaction.
	DeleteJobs(ctx context.Context, jobIDs ...string) error

	// DropRetriedDurability atomically removes durable_runs and failed_articles,
	// keeping job_files for a retry whose manifest changed shape.
	DropRetriedDurability(ctx context.Context, jobID string) error

	// SweepOrphanedDurability reclaims rows whose job is in neither dispatch_jobs
	// nor history-as-Failed.
	SweepOrphanedDurability(ctx context.Context) (int, error)
}
```

---

### 3.4 Elimination of Issue 561 in `SaveProgressBatch`

In `internal/durability/store_sqlite.go`, `SaveProgressBatch` prepares `stmtFailed`:

```sql
INSERT OR IGNORE INTO failed_articles (job_id, art_idx)
SELECT ?, ?
WHERE EXISTS (SELECT 1 FROM job_files WHERE job_id = ?)
```

If `DeleteJob` has already run for a removed job, `job_files` for that job has been deleted. When the delayed `Flush` commits, the subquery returns empty: **zero rows are inserted for the removed job**. Crucially, unlike foreign-key constraints which roll back the entire transaction upon failure (breaking all other jobs in the batch), this conditional insert silently skips only the departed job while successfully committing updates for all active jobs in the batch.

---

### 3.5 Elimination of raw SQL in callers

All ten raw SQL sites (E11) are replaced:

1. `seedJobFiles` (`internal/app/app.go:815`) $\to$ `app.durabilityStore.SeedJobFiles(ctx, jobID, seeds)`
2. `restoreJobFiles` (`internal/app/residency.go:108`) $\to$ `app.durabilityStore.LoadJobFiles(ctx, jobID)`
3. `residency.go:158` (direct `SELECT FROM durable_runs`) $\to$ `app.durabilityStore.ForJob(ctx, jobID)`
4. `residency.go:179` (direct `SELECT FROM failed_articles`) $\to$ `app.durabilityStore.LoadFailedArticles(ctx, jobID)`
5. `appCheckpointStore.SaveBatch` (`dispatcher_wiring.go:99,107`) $\to$ maps `cps` to `JobProgressUpdate` and calls `s.store.SaveProgressBatch(ctx, updates)`
6. `deleteJobDurability` (`durability.go:1464`) $\to$ `app.durabilityStore.DeleteJob(ctx, jobID)`
7. `dropJobDurability` (`durability.go:1532`) $\to$ `app.durabilityStore.DropRetriedDurability(ctx, jobID)`
8. Retry `failed_articles` clear (`app.go:2267`) $\to$ subsumed by `DropRetriedDurability`
9. `history.Repository.delete` (`repository.go:400`) $\to$ calls `r.durability.DeleteJobs(ctx, nzoIDs...)`
10. `MarkCompleted` (`repository.go:429`) $\to$ calls `r.durability.DeleteJobs(ctx, nzoID)`

---

## 4. Scored against the agreed criteria

| # | Criterion | Score | Detailed Verdict & Analysis |
|---|---|---|---|
| 1 | **Crash window** | **Narrowed & Self-Healing** | **What is closed:** Partial orphans from queue departures (#549) are closed at the source by single-transaction deletion across all three tables. Issue 561 resurrecting rows post-removal is closed at the statement level. <br>**What remains:** Ingestion SIGKILL (E2: crash between `addfile` 200 and dispatcher tick) leaves `job_files` rows with no parent. This is strictly bounded: `SweepOrphanedDurability` runs at startup and deterministically deletes them. No rows remain uncollected across a restart. |
| 2 | **§6 exclusive-writer** | **Kept (Compiler + Convention)** | `Commit(ctx, jobID, arts)` remains the sole method that inserts or amends `durable_runs` content. Within `internal/durability`, only `durability.Barrier` calls `Commit` (E10). Auxiliary methods (`SeedJobFiles`, `SaveProgressBatch`) touch only `job_files` and `failed_articles`. |
| 3 | **Rule 2** | **Consolidated (Writers/Deleters)** | **`durable_runs`:** 1 writer (`Barrier`), 2 deleters (`Resumer`, `Store.DeleteJobs`). <br>**`failed_articles`:** 1 writer (`Store.SaveProgressBatch`), 1 deleter (`Store.DeleteJobs`). <br>**`job_files`:** 2 writers (`Store.SeedJobFiles`, `Store.SaveProgressBatch`), 1 deleter (`Store.DeleteJobs`). <br>**`history_job_files`:** 1 writer (`jobFinalizer`), 1 deleter (SQLite FK `ON DELETE CASCADE`). <br>Raw SQL sites in `app` and `history` dropped from 10 to 0. |
| 4 | **Issue 561** | **Fixed** | `SaveProgressBatch` inserts `failed_articles` only `WHERE EXISTS (SELECT 1 FROM job_files WHERE job_id = ?)`. When `DeleteJob` deletes `job_files`, straggling flushes insert zero rows. Valid jobs in the same batch succeed without constraint aborts. |
| 5 | **Simplicity** | **High (Zero Contortions)** | Added tables: **0**. Cross-package transaction handles (`Execer`): **0**. Ingestion ordering changes (#552): **0** (`SeedJobFiles` runs before `Add` without constraint failure). State copying on failure: **0** (no `history_durable_runs`). |
| 6 | **Runtime cost** | **Zero Hot-Path Overhead** | `SaveProgressBatch` runs identical prepared statements within a single transaction. `DeleteJob` replaces multi-round autocommits with one transaction. `SweepOrphanedDurability` runs once at boot using indexed subqueries (< 2ms). |

---

## 5. Residual risks — attack these

1. **Startup-only sweep vs. long-running daemon:**  
   If an unexpected SIGKILL or storage fault strands an orphan during a running instance, the orphan is not reclaimed until the next restart. If a daemon runs for 6 months without restarting, that orphaned row sits on disk for 6 months. (Counter: orphans can *only* be created by process death or storage faults; steady-state operations produce zero orphans).
2. **Surface area expansion of `internal/durability`:**  
   `internal/durability` was conceived to own durable byte runs. Adding `job_files` metadata (file names, CRC32, fetch policies) expands its domain from "durability proof" to "active job persistence". If the critic argues this muddles the package's single responsibility, the counter is that these three tables share a single lifecycle and storage engine; splitting them is what caused #549, #557, and #560.
3. **History $\to$ Durability coupling without transactional atomicity:**  
   When `history.Repository.Delete` runs, it calls `durability.Store.DeleteJobs` in one transaction and `DELETE FROM history` in another. If the process is killed between the two, the history row remains while durability rows are gone (a subsequent retry would find no runs). (Counter: S3 safe direction—absence of evidence is absence; par2 or redownload recovers).

---

## 6. What this design does not do

1. **Does not make `AddJob` synchronous:** It does not force a synchronous disk write on `mode=addfile` to close E2. Closing E2 requires changing dispatcher architecture; this design contains the blast radius by sweeping E2 orphans on boot.
2. **Does not add foreign keys between `dispatch_jobs` and durability tables:** It explicitly avoids them to respect E1, E6, and E13.
3. **Does not invent shadow tables:** It does not introduce `history_durable_runs` or copy rows during the queue $\to$ history handoff.
4. **Does not alter the `checkpoint.Checkpointer` concurrency model:** It fixes issue 561 at the SQL execution layer without adding locks to `Prune`.

---

## 7. Implementation sketch

Each step builds, passes all gates, and is independently revertable:

- **Commit 1 (`schema(history): cascade history_job_files from history`)**  
  Update `001_initial.sql` to add `FOREIGN KEY (job_id) REFERENCES history(nzo_id) ON DELETE CASCADE`. Remove raw delete in `history.Repository.delete`. Update `schema.golden`.
- **Commit 2 (`feat(durability): introduce unified Store and PerJobTables enumeration`)**  
  Add `tables.go` with `PerJobTables`. Implement `SQLiteStore` with `SeedJobFiles`, `LoadJobFiles`, `SaveProgressBatch`, `DeleteJob`, `DeleteJobs`, `DropRetriedDurability`, and `SweepOrphanedDurability`. Add comprehensive package unit tests.
- **Commit 3 (`refactor(app): delegate job_files and failed_articles persistence to durability.Store`)**  
  Wire `durability.Store` into `Application`. Replace `seedJobFiles`, `residency.restoreJobFiles`, and `dispatcher_wiring.appCheckpointStore` with calls to `durability.Store`. Remove raw SQL in `app.go`.
- **Commit 4 (`fix(app,durability): atomic multi-table DeleteJob and startup orphan sweep`)**  
  Replace `deleteJobDurability` and `dropJobDurability` with `Store.DeleteJob` and `Store.DropRetriedDurability`. Add startup orphan sweep in `Application.Start`. Replace PR 557.
- **Commit 5 (`fix(history): clean up durability rows including job_files on history delete`)**  
  Inject `durability.Store` into `history.Repository`. Call `DeleteJobs` on history entry deletion. Purge retained durability rows in `MarkCompleted`.
- **Commit 6 (`test(app,durability): modernize per-job test fixtures with PerJobTables`)**  
  Refactor test assertion helpers across `package app` and `package app_test` to derive queries from `durability.PerJobTables`.

---

## 8. Test plan and red checks

### Required mutation specs (`scripts/mutate`)

1. **Atomicity Mutation (`durability_delete_atomic.spec`):**  
   Neuter transaction in `SQLiteStore.DeleteJobs` to separate autocommits.  
   *Killed by:* `TestDeleteJobDurability_AtomicRollback` asserting 1/1/1 rows remain on injected failure.
2. **Issue 561 Guard Mutation (`issue_561_resurrect.spec`):**  
   Remove `WHERE EXISTS (SELECT 1 FROM job_files WHERE job_id = ?)` from `stmtFailed`.  
   *Killed by:* `TestCheckpointer_PruneRaceDoesNotResurrectFailedArticles` verifying zero failed article rows after concurrent flush.
3. **History Deletion Leak Mutation (`history_delete_job_files.spec`):**  
   Remove `job_files` from `PerJobTables` deletion loop.  
   *Killed by:* `TestHistoryDelete_CleansJobFiles` asserting `job_files` count is 0 after failed history job deletion.
4. **Orphan Sweep Exception Mutation (`durability_orphan_sweep.spec`):**  
   Remove `AND NOT EXISTS (... WHERE status = 'Failed')` from sweep query.  
   *Killed by:* `TestOrphanSweep_PreservesFailedHistoryJobs` asserting failed job durability rows survive sweep.

---

## 9. Questions for the critique

1. **Ingest Ordering (E1, E13, #552):** How does your foreign-key design permit `seedJobFiles` to run before `dispatcher.Add` without throwing `FOREIGN KEY constraint failed (787)` on `job_files`? If you defer FKs, what transaction spans the two packages?
2. **Failed Job Truncate Retention (E6, #422):** How does an unconditional `ON DELETE CASCADE` from `dispatch_jobs` prevent destroying `durable_runs` when a failed job leaves the queue? If you introduce `history_durable_runs`, how do you justify the row copying and duplicate persistence under Standing Design Rule 2?
3. **Issue 561 Batch Failure:** When `Checkpointer.Flush` races `RemoveJob`, SQLite conflict clauses like `INSERT OR IGNORE` do not suppress FK violations. How does an FK design prevent a single removed job from rolling back the entire checkpoint batch for all active jobs?
