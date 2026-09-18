# Architecture Design: Unified `durability.Store` for Per-Job Row Lifecycle

**Status:** Synthesized Specification (incorporating Design A + Design B Cross-Critique)  
**Author:** Designer B  
**Date:** 2026-09-18  
**Scope:** issue 560 (lifecycle of per-job persisted rows), issue 561 (checkpoint prune race), and PR 557

---

## 1. The design in one paragraph

Consolidate the persistence, queries, atomic deletion, and orphan reclamation of all three active download tables (`durable_runs`, `failed_articles`, `job_files`) into `internal/durability` under a unified `durability.Store`, evicting all ten ad-hoc raw SQL statements from `internal/app` and `internal/history`. `durability.Barrier` becomes the **compiler-enforced** exclusive writer of `durable_runs` content (§6) by unexporting `Store.commit`, while typed methods using primitive records (`SeedJobFiles`, `SaveProgressBatch`, `LoadJobFiles`) manage `job_files` and `failed_articles` without creating an import cycle with `internal/job`. Deletion across all three tables is unified into a single state-derived operation, `Store.Reclaim(ctx, id, moreIDs...)`, which deletes `job_files` and `failed_articles` whenever a job is absent from `dispatch_jobs`, and deletes `durable_runs` whenever the job is absent from both `dispatch_jobs` and `history(status='Failed')`. Prerequisite P1 persists `dispatch_jobs` synchronously inside `Dispatcher.Add` **before** `d.kick()` makes the job launchable, closing E2 and E5 without leaking phantom background downloads on persist failure. Issue 561 is eliminated by combining the SQL `WHERE EXISTS (SELECT 1 FROM job_files ...)` liveness guard with `Checkpointer.Prune` synchronizing against `inFlight` flushes. Exported `durability.PerJobTables` provides the machine-readable enumeration mandated by Standing Design Rule 4, and `history_job_files` gains `FOREIGN KEY (job_id) REFERENCES history(nzo_id) ON DELETE CASCADE` written atomically inside `history.Add`.

---

## 2. Evidence base

Facts established at `f83c366d` relied on by this design:

| # | Fact | Status & Citation | Role in this Design |
|---|---|---|---|
| E1 | `dispatch_jobs` has one writer, reached only from the dispatcher tick and `Stop`. `Dispatcher.Add` writes nothing to disk. | Verified (`internal/dispatch/store/store.go:93`, `internal/dispatch/tick.go:119`) | Fixed by P1: `Dispatcher.Add` calls `persistIfChanged` synchronously *before* `d.kick()`. |
| E2 | A job accepted by the API is lost if killed right after `mode=addfile` returns 200, orphaning `job_files` rows and manifest. | Verified (`test/crash/` reproduction: 10/15 lost) | Closed by P1; any crash between `SeedJobFiles` and `Add` is reclaimed at startup by `SweepOrphans`. |
| E3 | The E2 window lasts until the woken tick persists the row; within a tick, `launch` runs before `persistIfChanged`. | Verified (`registry.go:205`, `dispatch.go:454-455`, `tick.go:29-63`) | Proves `register` must NOT call `d.kick()` before `persistIfChanged` succeeds in P1. |
| E4 | To the user, E2 means the job is lost; dir scanner removes source; manual resubmit matches `admin/nzb` backup and is added paused. | Verified (`internal/dirscanner/scanner.go`, `internal/app/app.go:657-667`) | Closed by P1 + startup `SweepOrphans`. |
| E5 | `RetryHistoryJob` deletes the history row after an in-memory `Add` and before any tick writes `dispatch_jobs`. | Verified (`internal/app/app.go:2342`, `:2347`) | Closed by P1: `dispatch_jobs` row is durable before `Add` returns, preceding `history.Delete`. |
| E6 | Only `durable_runs` must be read across queue $\to$ history handoff for a failed job; `job_files` has no reader for a job in history. `failed_articles` must be absent at retry. | Verified (`internal/app/residency.go:108,158,179`, `app.go:2324-2338`, `app.go:2266`) | Enables `Reclaim` to delete `job_files` and `failed_articles` immediately when a job fails, keeping *only* `durable_runs`. |
| E7 | Retry's `failed_articles` clear is non-fatal on the progress-applied branch (logs `Warn` only). | Verified (`app.go:2265-2270` vs `durability.go:1480-1487`) | Closed at the source: `Reclaim` already deleted `failed_articles` when the job failed. |
| E8 | `RemoveJob` of a queued job that never ran can skip all cleanup if `evictCancelledNeverRun` wins the race. | Verified (`app.go:866-875`, `tick.go`) | Fixed: `RemoveJob` calls `app.reclaim(ctx, id)` even when `Remove` returns `ErrNotFound`. |
| E9 | `MarkCompleted` turns a Failed history entry into Completed with a bare `UPDATE`, stranding retained rows. | Verified (`internal/history/repository.go:429`, `internal/api/history.go:284`) | Fixed: `MarkCompleted` invokes `Reclaim(ctx, nzoID)`, purging `durable_runs`. |
| E10 | `RunStore.Commit`'s only production callers are `durability.Barrier`. | Verified (`internal/durability/barrier.go:282, 628`) | Upgraded to a compile-time guarantee by unexporting `Store.commit`. |
| E11 | Ten raw-SQL statement sites touch `job_files`, `failed_articles`, or `durable_runs` outside `internal/durability`. | Verified (`app.go:815,2267`, `dispatcher_wiring.go:99,107`, `durability.go:1464,1532`, `residency.go:108,158,179`, `repository.go:400`) | All 10 sites are evicted and replaced with calls on `durability.Store`. |
| E12 | All tables share one `*sql.DB` with `_pragma=foreign_keys(1)` and `_txlock=immediate`. | Verified (`app.go:357,525`, `internal/history/db.go:79-80`) | Serialises `SaveProgressBatch` against `Reclaim` and enforces `history_job_files` cascade. |
| E13 | `seedJobFiles` must run before `Add` (issue 552 fix); `RetryHistoryJob` uses the same order. | Verified (`app.go:762-768`, `app.go:2306-2320`) | Requires runtime `Reclaim` to take explicit job IDs so it never races an in-flight `SeedJobFiles`. |
| E14 | `internal/job/content.go:8` imports `internal/durability` for `DurableProof`. | Verified (`internal/job/content.go:8, 196`) | Forbids `internal/durability` from importing `internal/job` (prevents Go compile cycle). |
| E15 | `d.register` calls `d.kick()` at `registry.go:205`; `deregister` requires callers to drain launched workers first (`registry.go:264-265`). | Verified (`internal/dispatch/registry.go:191-207, 261-268`) | Requires P1 to run `persistIfChanged` *before* `d.kick()` and launch eligibility. |
| E16 | 33 raw `INSERT` statements across 15 test files lack a shared table enumeration across `package app` and `package app_test`. | Verified (`issue 560 comments 5721839944, 5721847476`) | Resolved by exporting `durability.PerJobTables` in non-test code. |

---

## 3. Design

### 3.1 Prerequisite P1 (Race-Free Synchronous `Dispatcher.Add`)

To close E2 and E5 without violating `deregister`'s worker/lease drain contract (`registry.go:264-265`), leaking a scheduler lease in `d.q.Advance` (`tick.go:31`), or bypassing the `#513` single-caller gatekeeper on `deregister` (`registry.go:404-412`):
- `register` inserts the job without calling `d.kick()`,
- `snapshotOrder()` skips any job where `_, ok := d.written[id]` is `false` (so an concurrent tick cannot call `d.q.Advance`, `reconcileResidency`, or `d.launch` before the initial `Save` completes), and
- `Add` calls `d.kick()` only after `persistIfChanged` marks `d.written[id]`, unwinding via `beginRemoval` $\to$ `rm.end()` on error:

```go
func (d *Dispatcher) Add(ctx context.Context, j *job.Job, h Header) error {
	seqNext := d.seq.Add(1)
	// 1. Insert into registry without kicking; snapshotOrder skips unwritten jobs.
	if err := d.registerUnwritten(j, h, seqNext); err != nil {
		return err
	}
	// 2. Persist synchronously under storeMu before waking the tick.
	if err := d.persistIfChanged(ctx, j); err != nil {
		// Safe: snapshotOrder skipped j while unwritten, so d.q.Advance,
		// reconcileResidency, and claimLaunched never touched j.
		// Unwind via beginRemoval -> rm.end() so removal.end stays the
		// sole caller of d.deregister (registry.go:404-412).
		if rm, ok := d.beginRemoval(j.ID()); ok {
			rm.end()
		}
		return fmt.Errorf("dispatch: Add: persist %s: %w", j.ID(), err)
	}
	// 3. Row is now in d.written[id]; wake the tick to advance and launch.
	d.kick()
	return nil
}
```

---

### 3.2 Schema changes (DDL) & `history.Add` Atomicity

In `internal/history/migrations/001_initial.sql`, `history_job_files` becomes a cascading child of `history(nzo_id)`:

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

`history.Repository.Add` writes `history_job_files` inside the same transaction as the `history` entry, replacing the separate `_, _ =` statements in `job_finalizer.go`. `internal/history` removes all `durable_runs` and `failed_articles` SQL and drops `DeleteKeepingDurability`.

---

### 3.3 Machine-readable table enumeration (Rule 4)

In non-test code in `internal/durability/tables.go`:

```go
package durability

type PerJobTable struct {
	Name        string
	JobIDColumn string
}

var PerJobTables = []PerJobTable{
	{Name: "durable_runs", JobIDColumn: "job_id"},
	{Name: "failed_articles", JobIDColumn: "job_id"},
	{Name: "job_files", JobIDColumn: "job_id"},
}
```

---

### 3.4 Package-Safe `durability.Store` Signatures & Unexported `commit`

To preserve the dependency DAG (`checkpoint` $\to$ `job` $\to$ `durability`, E14), `internal/durability` uses primitive records and unexports `commit` so §6 is enforced by the Go compiler:

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

type Store struct{ db *sql.DB }

// commit is unexported: only durability.Barrier (inside package durability)
// can insert or amend durable_runs content (§6 compiler-enforced).
func (s *Store) commit(ctx context.Context, jobID string, arts []DurableArticle) ([]Collision, error)

// Content queries and file-level invalidation (Resumer & retry manifest mismatch).
func (s *Store) ForFile(ctx context.Context, jobID string, fileIdx int32) ([]Run, error)
func (s *Store) ForJob(ctx context.Context, jobID string) ([]Run, error)
func (s *Store) DeleteFile(ctx context.Context, jobID string, fileIdx int32) error
func (s *Store) DiscardRuns(ctx context.Context, jobID string) error

// Active job file metadata and checkpoint persistence.
func (s *Store) SeedJobFiles(ctx context.Context, jobID string, files []FileSeed) error
func (s *Store) LoadJobFiles(ctx context.Context, jobID string) ([]FileRecord, error)
func (s *Store) LoadFailedArticles(ctx context.Context, jobID string) ([]int, error)
func (s *Store) SaveProgressBatch(ctx context.Context, updates []JobProgressUpdate) error

// Reclaim applies the state-derived retention rule to one or more explicit job IDs.
// Requires at least one ID so runtime calls cannot race an in-flight SeedJobFiles.
func (s *Store) Reclaim(ctx context.Context, id string, moreIDs ...string) error

// SweepOrphans applies the retention rule across all tables once at startup,
// after resumeAllJobs and before the API or dirscanner starts.
func (s *Store) SweepOrphans(ctx context.Context) (int, error)
```

---

### 3.5 The State-Derived Reclaim Rule & Manifest Cleanup

Inside `Store.Reclaim(ctx, id, moreIDs...)` (scoped to `job_id IN (...)`) and `Store.SweepOrphans(ctx)` (unfiltered at startup), one SQLite transaction runs:

```sql
DELETE FROM job_files
 WHERE NOT EXISTS (SELECT 1 FROM dispatch_jobs d WHERE d.id = job_files.job_id)
   AND job_id IN (?);

DELETE FROM failed_articles
 WHERE NOT EXISTS (SELECT 1 FROM dispatch_jobs d WHERE d.id = failed_articles.job_id)
   AND job_id IN (?);

DELETE FROM durable_runs
 WHERE NOT EXISTS (SELECT 1 FROM dispatch_jobs d WHERE d.id = durable_runs.job_id)
   AND NOT EXISTS (SELECT 1 FROM history h
                    WHERE h.nzo_id = durable_runs.job_id AND h.status = 'Failed')
   AND job_id IN (?);
```

- **Orphaned Manifest Unlinking (E2, E8):** `app.reclaim(ctx, id, moreIDs...)` calls `durabilityStore.Reclaim(ctx, id, moreIDs...)` and unlinks `admin/queue/manifests/<id>.json.gz` for each `id` absent from `dispatch.GetJob(id)`. At startup, after `resumeAllJobs` and `durabilityStore.SweepOrphans(ctx)`, `app` scans `admin/queue/manifests/` and unlinks any `<id>.json.gz` not present in the dispatcher, collecting manifests from any crash between `manifestStore.Save` (`app.go:753`) and `Dispatcher.Add`.
- **History Deletion Ordering:** In `DeleteHistory`, `history.Delete` (`DELETE FROM history`) executes **before** `app.reclaim(ctx, ids...)`. If a crash occurs between the two calls, the history row is already gone, so startup `SweepOrphans` reclaims the remaining `durable_runs` rather than stranding a retryable `Failed` history entry whose `durable_runs` were prematurely deleted.

---

### 3.6 Complete Closure of Issue 561 (Liveness Guard + `Prune` In-Flight Sync)

1. **SQL Liveness Guard in `SaveProgressBatch`:**
   ```sql
   INSERT OR IGNORE INTO failed_articles (job_id, art_idx)
   SELECT ?1, ?2 WHERE EXISTS (SELECT 1 FROM job_files WHERE job_id = ?1)
   ```
   Prevents a late flush from resurrecting `failed_articles` after `Reclaim` deleted `job_files`, while allowing sibling active jobs in the same batch transaction to commit cleanly.
2. **Closing Residual Risk 1 (Retry Re-Seed Race):**  
   In `Checkpointer.Prune(id)`, if `id` is currently present in `c.inFlight`, `Prune` waits on `c.flushMu` before returning. Because `c.inFlight` holds `id` only for the duration of an active SQLite `SaveBatch` transaction containing `id`, `Prune` blocks only when the race is actively occurring. Once `Prune(id)` returns, no stale batch from the previous attempt is in flight when a retry calls `SeedJobFiles`.

---

## 4. Scored against the agreed criteria

| # | Criterion | Score | Detailed Verdict & Analysis |
|---|---|---|---|
| 1 | **Crash window** | **Closed for Jobs; Self-Healing for Rows** | **Jobs (E2, E5):** Closed outright by P1 (`Dispatcher.Add` persists synchronously before `d.kick()`).<br>**Rows (#549):** Partial departures closed by single-transaction `Reclaim`. Any row stranded by a hard `SIGKILL` between departure and `Reclaim` cannot cause wrong behaviour (rule is state-derived) and is swept deterministically at startup by `SweepOrphans`. |
| 2 | **§6 exclusive-writer** | **Strengthened (Compiler-Enforced)** | `Store.commit` is unexported inside `package durability`. No package outside `internal/durability` can compile a call that writes `durable_runs` content; within the package, `durability.Barrier` is the sole caller (E10). |
| 3 | **Rule 2** | **Single Owner Package & One Reclaim Rule** | `internal/durability` is the sole owner of `durable_runs`, `failed_articles`, and `job_files`. Lifecycle deletion has one rule (`Reclaim`/`SweepOrphans`). `history_job_files` is written once in `history.Add` and deleted by SQLite `ON DELETE CASCADE`. Raw SQL sites outside `internal/durability` drop from 10 to 0. Exported `PerJobTables` unifies all test fixtures. |
| 4 | **Issue 561** | **Fixed (Zero Residual Race)** | Guarded by `WHERE EXISTS (SELECT 1 FROM job_files ...)` in `SaveProgressBatch` AND `Checkpointer.Prune` waiting on `flushMu` when `id` is in `c.inFlight`, eliminating both post-removal resurrection and cross-retry corruption. |
| 5 | **Simplicity** | **High (Zero Contortions)** | Added tables: **0**. Leaked `Execer` interfaces: **0**. Go import cycles: **0**. Split signature (`Reclaim(ctx, id, ...)` vs. startup `SweepOrphans(ctx)`) prevents runtime sweeps from racing `SeedJobFiles`. |
| 6 | **Runtime cost** | **Minimal** | `Add`: one synchronous row write the tick would perform milliseconds later. `SaveProgressBatch`: one indexed prefix probe per job with failed articles. Departure: one transaction with three indexed `NOT EXISTS` deletes. |

---

## 5. Residual risks — attack these

1. **Power loss under `synchronous=NORMAL`:**  
   SQLite WAL with `synchronous(NORMAL)` survives process crashes (`SIGKILL`) but can roll back the lastfew milliseconds of commits on physical power loss. Identical across all candidates.
2. **Schema coupling in `Reclaim`:**  
   `Reclaim` reads `dispatch_jobs.id` and `history(nzo_id, status)`. Reading across tables in the same SQLite database is permitted by Rule 2 (no mutations), and is pinned by table-driven integration tests against the real schema.

---

## 6. What this design does not do

1. **Does not add foreign keys between `dispatch_jobs` and active durability tables:** Avoids E13 ordering hazards and `history_durable_runs` reparenting copies.
2. **Does not import `internal/job` into `internal/durability`:** Preserves the clean acyclic package graph.

---

## 7. Implementation sketch

1. **`fix(dispatch): persist a job's queue row in Add before waking the tick`** — Implement race-free P1 (`registerUnwritten` $\to$ `persistIfChanged` $\to$ `kick`). Pin with crash test.
2. **`refactor(durability): consolidate job_files and failed_articles into Store and export PerJobTables`** — Add `durability.Store` with primitive types, unexport `commit`, export `PerJobTables`, move `seedJobFiles`, `SaveBatch`, and residency reads.
3. **`fix(app,durability): replace four deletion rules with state-derived Reclaim and startup SweepOrphans`** — Wire `Reclaim(ctx, id, ...)` across `RemoveJob` (including `ErrNotFound` for E8), `jobFinalizer`, `MarkCompleted` (E9), and `DeleteHistory`; run `SweepOrphans` at startup.
4. **`fix(checkpoint,durability): guard failed_articles on job_files liveness and sync Prune with inFlight`** — Close issue 561 and cross-retry race.
5. **`refactor(history): cascade history_job_files from history and write inside history.Add`** — Edit `001_initial.sql` and `schema.golden`; remove durability SQL and `DeleteKeepingDurability` from `internal/history`.
6. **`test(app,durability): modernize per-job test fixtures with PerJobTables`** — Replace scattered raw-SQL test counts with helpers derived from `durability.PerJobTables`.

---

## 8. Test plan and red checks

1. **P1 Crash & Error-Path Tests:**  
   - `TestSIGKILL_AddThenImmediateKill` in `test/crash`: assert job survives immediate `SIGKILL` after HTTP 200.  
   - `TestAdd_PersistFailureDoesNotLaunchWorkers`: inject store failure in `Add`, assert zero workers launched and `deregister` leaves clean state.
2. **Reclaim State Matrix (`scripts/mutate`):**  
   Table-driven test covering all 5 states (`in queue`, `in queue + failed in history`, `failed in history only`, `completed in history`, `in neither`). Every mutation (dropping `dispatch_jobs` clause, dropping `history` clause, flipping `'Failed'` to `'Completed'`, relaxing `NOT EXISTS` to `NOT IN` with NULL key) must be **KILLED**.
3. **Issue 561 & Cross-Retry Interleaving (`scripts/mutate`):**  
   - Capture batch, `Reclaim`, commit batch $\to$ 0 `failed_articles`. Mutation removing `WHERE EXISTS` must be **KILLED**.  
   - Capture batch, fail job, retry job (`SeedJobFiles`), verify `Prune` synchronized with `inFlight` so stale batch does not pollute retry.
4. **Compiler §6 Pin:**  
   Verify `Store.commit` is unexported and no exported method on `durability.Store` inserts into `durable_runs`.

---

## 9. Questions for the critique

1. Does gating `claimLaunched` / `d.kick()` until after `persistIfChanged` in `Dispatcher.Add` completely close Finding 2 (the worker/lease leak on persist failure), or is there any other path that can observe the unwritten registry entry?
2. With `Reclaim(ctx, id, moreIDs...)` requiring at least one explicit ID at runtime and `SweepOrphans(ctx)` restricted to startup, is there any remaining interleaving where `SeedJobFiles` before `Dispatcher.Add` can lose rows?
