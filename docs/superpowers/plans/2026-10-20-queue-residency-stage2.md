# Queue Residency Stage 2 — Progress from Restore — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** every registered job has a `FileTable` and a `JobProgress` before the first tick, so the queue shows files, an estimated percentage and a "progress from the record" badge for every job after a restart, and hydration verifies a restored job once and then marks it verified.

**Architecture:** one SQL pass over `job_files` LEFT JOIN a `COUNT(*)` of `written_articles` runs in `reconcileBeforeFirstTick`; it attaches a table and a seeded progress record (`Verified=false`, `Recorded=n`, `Complete`, `Filename`, `Fetch` from the row) through the existing `AttachFiles` door, which gains a seed parameter. No per-article bit is seeded (durability §3 stays verbatim). `Hydrate` requires a table, keys its articles-only branch on "every file Verified", and `InstallFileVerification` is the sole writer of `Verified = true`. `dispatch_jobs.recovery_bytes`, `SetRecoveryBytes` and the header-bytes fallback go.

**Tech Stack:** Go, SQLite (`modernc.org/sqlite`), Svelte 5 + TypeScript, `scripts/mutate` specs.

**Spec:** `docs/superpowers/specs/2026-10-20-queue-residency-design.md` §4.3–4.9, §5, §6, §7 item 2. Decision log: `docs/superpowers/specs/2026-10-20-queue-residency-decisions.md` (D13 binds the hydration branch; D16 the retry rows).

## Global Constraints

- Durability §3 verbatim: a Done bit after restart exists only because a device read matched its row's CRC or the file was `complete=1`. The restore seed sets **no** article bit. Pinned by a test that asserts `done.Count() == 0` after a seed, and a mutation that seeds bits.
- Standing Rule 2: `AttachFiles` stays the only writer of the `fileTable` and `progress` pointers (`TestFilesPointerWriters_…`, `TestProgressPointerWriters_…`); `InstallFileVerification` is the only writer of `Verified = true` (new enumeration test). No second constructor anywhere.
- Standing Rule 4: every "only/sole/every" in a comment cites its grep; every new pin has a `scripts/mutate` spec with every mutation KILLED.
- Standing Rule 1: `recovery_bytes` is dropped from `001_initial.sql` and `schema.golden` in place; no migration.
- Every commit builds and passes `go test ./...`.
- UI: no arithmetic in Svelte; lookup tables, not conditions (`QueueRow.svelte`'s own rule).
- Worktree: `.claude/worktrees/queue-residency-stage2`, branch `worktree-queue-residency-stage2`, based on `refactor/queue-residency-stage1`. Push refspec `HEAD:feat/queue-residency-stage2`. PR base `refactor/queue-residency-stage1` until #1031 merges.

## Rulings taken at planning (recorded as D17–D19 in Task 0)

- **D17 — `AttachFiles(ft *FileTable, seed []FileSeed)`.** `seed == nil` (ingest, `AttachContent`, the stage-1 hydration path) yields every file `Verified=true, Recorded=0`; a non-nil seed must have `len(seed) == ft.NumFiles()` and is the restore path. One door, one writer; `AttachContent(m)` forwards `nil`. The design wrote this signature; stage 1 landed the one-argument form to keep each commit building.
- **D18 — `Hydrate` requires an attached table.** After the restore pass every registered job has one (when there is no store there is no restored job), so the stage-1 `fileTableFromRows` branch inside `Hydrate` is unreachable and is replaced by an error. `fileTableFromRows` moves to the restore pass. The articles-only branch keys on `j.AllVerified()` (D13).
- **D19 — the restore read is its own row type.** `durability.SeedRow{FileRow; Recorded int}` from one query over every job; `FileRow`/`FileRows` are unchanged so a field that is only sometimes meaningful does not appear on the shared type.

## Review Focus

1. A restored `complete=1` file is `Complete=true` but `!Verified` with no bits; `IsComplete()` is true before hydration. Every reader of `IsComplete` outside `internal/job` runs on a hydrated job (worker or fault path) — Task 3 enumerates and records them in the PR body; Task 6 fixes the comment at `app.go:1785`.
2. A restored job with zero `written_articles` rows and `complete=0` everywhere must show 0% and still be verified on first hydration (row-less files get an empty install). Task 3 test.
3. A job whose `job_files` rows are missing or gapped at restore is settled Failed with an operational error through the existing door the tick uses for an unreadable manifest, never with a second path. Task 3.
4. A paused-never-run job after restart shows its files and 0% and is never hydrated by the tick (residency rule unchanged); `verifyPausedJobs` (deleted in stage 3) must still work this stage. Task 3 keeps `paused_verify_test.go` green by retargeting "no progress" assertions to "not verified".
5. The UI badge must come from `progress_verified`, never inferred from `files`. Task 5.

---

### Task 0: Record D17–D19

**Files:** Modify `docs/superpowers/specs/2026-10-20-queue-residency-decisions.md` — append D17, D18, D19 in the existing `### D<n> — title` / **Call** / **Why** / **Cost if wrong** format, text from "Rulings taken at planning" above.

- [ ] Step 1: append; `go run ./scripts/check_doc_citations`; commit `docs: record stage-2 decisions D17–D19`. Batched with Task 1's dispatch.

### Task 1: `FileProgress.Verified`/`Recorded`, the seed door, the estimate

**Files:**
- Modify: `internal/job/progress.go` — `FileProgress` gains `Verified bool` and `Recorded int` (doc text from design §4.3 verbatim); `type FileSeed struct { Recorded int; Complete bool; Filename string; Fetch FetchPolicy }`; `newJobProgress(ft *FileTable, seed []FileSeed) *JobProgress` applies the seed (nil → `Verified=true`); `func (p *JobProgress) fileBytesDownloaded(fi int) int64` returns `Bytes×Recorded/ArticleCount` (ArticleCount = `fileStarts[fi+1]-fileStarts[fi]`; 0 articles → 0) when `!Verified`, else `BytesDownloaded`; `sizeFigures` and `FileBytesDownloaded` read through it; `func (p *JobProgress) AllVerified() bool`; `func (p *JobProgress) FileVerified(fi int) bool`.
- Modify: `internal/job/content.go` — `AttachFiles(ft *FileTable, seed []FileSeed) error` (refuses `seed != nil && len(seed) != ft.NumFiles()`); `AttachContent(m)` passes `nil`; `Job.AllVerified()` wrapper.
- Modify: `internal/job/verification.go` — `InstallFileVerification` sets `files[fi].Verified = true; Recorded = 0` as its last step.
- Modify: `internal/job/writer_enumeration_test.go` — new `TestVerifiedTrueWriters_MatchTheEnumerationStatedInProse` scanning assignments of `.Verified = true` (expect `[InstallFileVerification]`).
- Tests: `internal/job/progress_test.go` or new `internal/job/seed_test.go`; spec `internal/job/testdata/restore_seed.spec`.
- Every caller of `AttachFiles(` gains `, nil` (`git grep -n 'AttachFiles(' -- '*.go'`; paste the count in the commit body).

**Interfaces:**
- Produces: `job.FileSeed`, `(*Job).AttachFiles(ft, seed) error`, `(*Job).AllVerified() bool`, `(*JobProgress).FileVerified(fi) bool`, `(*JobProgress).fileBytesDownloaded(fi) int64` (unexported), `(*JobProgress).AllVerified() bool`.

- [ ] Step 1: tests first:
  ```go
  func TestAttachFiles_SeedSetsNoDoneBit(t *testing.T)            // seed Recorded=n for every file → progress.done.Count()==0, pendingArticles==total, every Verified==false
  func TestAttachFiles_NilSeedIsVerified(t *testing.T)            // AllVerified()==true, Recorded==0
  func TestAttachFiles_RefusesASeedOfTheWrongLength(t *testing.T)
  func TestFileBytesDownloaded_EstimateIsBoundedAndInBytesUnits(t *testing.T) // Recorded 0,k,n of n → 0, Bytes*k/n, Bytes; Verified → BytesDownloaded
  func TestInstallFileVerification_MarksVerifiedAndZeroesRecorded(t *testing.T)
  func TestRemainingBytes_UsesTheEstimateWhileUnverified(t *testing.T)
  ```
- [ ] Step 2: run → FAIL to compile.
- [ ] Step 3: implement.
- [ ] Step 4: `go build ./... && go test -count=1 -race ./internal/job/...` PASS.
- [ ] Step 5: `restore_seed.spec`: (a) seed sets a Done bit for the first Recorded articles → `TestAttachFiles_SeedSetsNoDoneBit` dies; (b) `fileBytesDownloaded` ignores `Verified` (always estimate) → the estimate test dies; (c) `InstallFileVerification` does not set `Verified` → its test dies; (d) nil seed yields `Verified=false` → `TestAttachFiles_NilSeedIsVerified` dies. All KILLED.
- [ ] Step 6: commit `feat(job): seed restored progress with a per-file recorded count and a Verified flag`.

### Task 2: One query for the restore pass

**Files:**
- Modify: `internal/durability/progress.go` — `type SeedRow struct { FileRow; Recorded int }`; `func (s *Store) SeedRows(ctx) (map[string][]SeedRow, error)`:
  ```sql
  SELECT f.job_id, f.file_index, f.subject, f.bytes, f.article_count, f.filename, f.complete, f.fetch_policy, COALESCE(w.n, 0)
  FROM job_files f LEFT JOIN (SELECT job_id, file_idx, COUNT(*) AS n FROM written_articles GROUP BY job_id, file_idx) w
    ON w.job_id = f.job_id AND w.file_idx = f.file_index
  ORDER BY f.job_id, f.file_index
  ```
  A scan error on any row is an error for the whole call (the pass needs a consistent snapshot; a torn read is a storage fault, not a per-job gap).
- Modify: `internal/history/migrations/001_initial.sql` :78-87 comment enumerating statements that touch `job_files` — re-run its grep, update the count.
- Modify: `internal/app/durability.go` :560-566 `durabilityStore` interface gains `SeedRows`; every test double implementing it (`git grep -n 'FileRows(ctx' -- 'internal/app/*_test.go'`).
- Tests: `internal/durability/progress_test.go` — two jobs, one file with 3 rows, one with none (COALESCE → 0), one job absent from `written_articles` entirely; spec `internal/durability/testdata/seed_rows.spec` (COALESCE removed / LEFT JOIN made INNER → row-less file vanishes → test dies).

- [ ] Steps: red → implement → `go test -count=1 -race ./internal/durability/... ./internal/app/...` → spec KILLED → commit `feat(durability): read every job's file structure and recorded counts in one pass`.

### Task 3: The restore pass; hydration keyed on Verified

**Files:**
- Create: `internal/app/restore_progress.go` — `func (app *Application) restoreProgress(ctx) error`: if `app.durable == nil` return nil; `rows := app.durable.SeedRows(ctx)` (error → return it; Start fails, as the design's "no record store" case); for each `app.dispatcher.List()` row: `j := app.dispatcher.Job(id)`; skip if `j.Files() != nil` (defensive? NO — Rule 1: at this point no job has a table; if one does, return an error naming it, it is a programming error); `ft, err := fileTableFromRows(...)` (moved here from `residency.go`, taking `[]durability.FileRow`); on error or on no rows: settle the job Failed with an operational error through the SAME door the tick uses for an unreadable manifest (find it: `git grep -n 'OperationalError\|settleFailed\|FailOperational' -- internal/dispatch internal/app`; cite it in the comment); else `seed[i] = FileSeed{Recorded, Complete, Filename, Fetch}` and `j.AttachFiles(ft, seed)`.
- Modify: `internal/app/startup_reconcile.go` :28-36 — call `restoreProgress` after the history-drop loop and before `fileOwedUnwantedFailures`.
- Modify: `internal/app/residency.go` — `Hydrate`: `ft := j.Files(); if ft == nil { return fmt.Errorf("residency: hydrate %s: no file table attached; the restore pass did not run", id) }`; branch `if j.AllVerified() { return j.AttachArticles(m) }`; else `files, rows := readRecord` → `verifyAndAttach`, which no longer calls `AttachFiles` (the table is attached) — only `AttachArticles(m)` then `installVerification` for every file (already the case, :336). Delete `fileTableFromRows` from this file (moved). Re-anchor: `hydrate_file_table.spec`, `residency_restore.spec`, `residency_hydrate_branches.spec`, `loose_record_waits.spec`, `install_verification_owner.spec` (keep `Settle: setComplete[fi],` and `Failed: res.Failed[fi],` verbatim).
- Modify: `internal/app/hydrate_file_table_test.go` — `restartedUnhydrated` becomes `restartedUnverified`: after `a2 := env.newApp(t)` call `a2.restoreProgress(ctx)` and assert `Files() != nil && HasProgress() && !AllVerified() && !Resident()`; `TestHydrate_WithoutAStoreNeedsAnAttachedFileTable` → asserts the new error text; `TestFileTableFromRows_*` follow the function.
- Modify: `internal/app/paused_verify_test.go` :328, :380-383; `residency_linux_test.go` :155; `loose_record_helpers_internal_test.go` :114 — "nothing attached after a fault" becomes "not Verified and not Resident after a fault" where the job was restored; where the fixture is a bare `job.New` with no table, the test now expects `Hydrate` to fail with the no-table error (D18), so give it a table via the restore pass or `AttachFiles(ft, seed)` and keep the fault assertion on `!Resident() && !AllVerified()`.
- Tests (new, `internal/app/restore_progress_test.go`):
  ```go
  func TestRestoreProgress_EveryRegisteredJobHasFilesAndProgress(t *testing.T) // the design §6 row-1 probe: restart over {paused-never-run, paused-Fetching, queued-partial, paused-Assessing, paused-Extracting} with lease cap 1; each has NumFiles()>0, HasProgress(), !AllVerified(), !Resident(); percentage from the API is Bytes*Recorded/ArticleCount-derived for the partial one
  func TestRestoreProgress_SeedsNoDoneBit(t *testing.T)                     // after restart, ArticleDone(i)==false for every recorded article until Hydrate
  func TestRestoreProgress_SettlesAJobWithoutStructureFailed(t *testing.T)   // delete the job_files rows → after Start the job is Failed with an operational error naming the gap; others unaffected
  func TestHydrate_FirstHydrationVerifiesEveryFile(t *testing.T)            // restored job, one file complete=1, one row-less, one partial: after Hydrate AllVerified(), Done bits == verified rows, failed set/settled CRC installed for the complete file
  func TestHydrate_VerifiedJobRehydratesWithoutVerifying(t *testing.T)      // hydrate, evict, hydrate again: verifyJobFiles not called the second time (count via a test hook already used by residency tests, or by a mutation that makes the second verify fail)
  func TestHydrate_RefusesAJobWithoutAFileTable(t *testing.T)
  ```
- Spec `internal/app/testdata/restore_progress.spec`: (a) seed `Verified: true` → `TestHydrate_FirstHydrationVerifiesEveryFile` dies (the verification never runs); (b) restore pass skips `AttachFiles` → `EveryRegisteredJobHasFilesAndProgress` dies; (c) the Failed settlement on missing rows replaced by `continue` → `SettlesAJobWithoutStructureFailed` dies; (d) `Hydrate`'s branch keyed on `Files() != nil` instead of `AllVerified()` → `FirstHydrationVerifiesEveryFile` dies. All KILLED.
- PR-body enumeration (do it here, paste into the report): readers of `IsComplete()` / `HasProgress()` / `PendingArticles()` outside `internal/job`, each with the reason it runs on a hydrated job or is display-only (design §4.5 list; re-derive with `git grep`).

- [ ] Steps: red → implement → `go build ./... && go vet ./... && go test -count=1 -race ./internal/app/... ./internal/dispatch/... && go run ./scripts/mutate --check-all` and every re-anchored spec → commit `feat(app): attach every registered job's file table and recorded progress before the first tick`.

### Task 4: Delete `recovery_bytes` and the header-bytes fallback

**Files:**
- `internal/history/migrations/001_initial.sql` :218-233 (column + comment), `internal/history/testdata/schema.golden` (regenerate: `go test ./internal/history -run TestMigrations_GoldenSchema -update`).
- `internal/dispatch/ports.go:79` `Persisted.RecoveryBytes`; `internal/dispatch/store/store.go` :43, :102, :123, :145, :156 and `store_test.go:111`; `internal/dispatch/store/testdata/unwanted.spec` re-anchor.
- `internal/dispatch/dispatch.go:1041` `SetRecoveryBytes` call; `internal/dispatch/tick.go:124` persist line (keep the `last == p` equality).
- `internal/job/content.go` :527-539 `SetRecoveryBytes` deleted; `writer_enumeration_test.go` :310-318 → `{"AttachFiles"}`; `internal/job/testdata/recovery_bytes_owner.spec` rewritten to a mutation that still has a kill (e.g. `AttachFiles` not setting `recoveryBytes` → the API/queue test on `recovery_bytes` or a job test dies); `content_test.go:797`, `dispatch/tick_test.go:814,893` need another dirtier (`SetName`).
- `internal/dispatch/registry.go` :669-679 and :717-721 — `RemainingBytes: j.RemainingBytes()` unconditionally (every registered job has progress after `StartWith`; `List` during `restore` before the hook? check `List` callers inside `StartWith`'s window: `dropJobAlreadyInHistory` loop runs BEFORE `restoreProgress` — it reads only `row.ID`; verify and say so). Doc comments :640-648, :700-702, :1195-1200 and `startup_reconcile.go:48-52` updated.
- `internal/api/queue.go` :470-475 — drop the header fallback; `ProgressFigures()` always.
- Tests: `internal/api/queue_nonresident_test.go` :36-75 (evicted slot `recovery_bytes` now from the table — still asserts the value), dispatch tests of `List` on a progress-less job become tests on a seeded job.

- [ ] Steps: build-driven; `go test -count=1 -race ./internal/... ` ; `go run ./scripts/mutate --check-all`; commit `refactor: derive recovery bytes and remaining bytes from the resident progress record`.

### Task 5: API fields and UI badge

**Files:**
- `internal/api/queue.go` — `queueSlot.ProgressVerified bool `json:"progress_verified"`` set from `j.AllVerified()` at :474-476 (a slot with no job → true? No: every slot has a job; if `j == nil` the slot is being removed — use `true` and say why); `queueFile.Verified bool `json:"verified"`` from `p.FileVerified(fi)` in `buildQueueFiles`.
- `test/integration/contract_test.go` :84-88, :146 — add the two keys.
- `ui/src/lib/types.ts` — `progress_verified: boolean` on `QueueSlot`, `verified: boolean` on `QueueFile`.
- `ui/src/lib/components/QueueRow.svelte` — progress cell (:507-516): when `!slot.progress_verified` the bar gets an `unverified` class (outlined/hatched via CSS, no arithmetic) and a `title`/hint text *"Progress from the download record; it is checked against the files when the job next runs."*; drawer rows (:800-825): an unfilled dot before the name when `!f.verified`; `applyFilesUpdate` (:222-224) copies `verified`; add a `PROGRESS_LABELS`-style lookup for the hint.
- `ui/src/lib/components/QueueRow.test.ts` — `baseSlot` gains `progress_verified: true`; tests: unverified slot renders the hatched bar + hint; verified does not; drawer dot per file; `applyFilesUpdate` updates `verified`.
- Read `docs/svelte-gotchas.md` before touching the component; run the UI unit tests (`bun test` or whatever `scripts/run_tests.sh` invokes — read it) and rebuild `ui/dist` so `go:embed` and the Playwright suite see the change.

- [ ] Steps: red (Go api test + Svelte test) → implement → `go test ./internal/api/... && go test -tags=integration ./test/integration/...` + UI tests → commit `feat(api,ui): show whether a job's progress has been verified against its files`.

### Task 6: Sweep

**Files:** `docs/job-lifecycle.md` §11–§14 (every registered job has a table and progress from restore; the "one window" section; Verified/Recorded; the estimate formula; memory table gains the two fields' cost), `docs/durability-contract.md` §3 (reaffirmed, add the recorded-count paragraph: a recorded count beside the bits, bits never seeded) and the `verifyPausedJobs` limitation text (stays until stage 3 — update, do not delete), `docs/ARCHITECTURE.md` "Compiler-Enforced Residency", `internal/app/app.go:1785-1788` comment, `startup_reconcile.go` :48-52, `registry.go` comments, `docs/sabnzbd_spec.md` only if it documents our slot JSON, decision log D9 marked closed by this stage. `git grep -n -E 'header-only|header bytes|RecoveryBytes|SetRecoveryBytes|RestoreContent|not yet hydrated|until hydrated' -- '*.go' '*.md' ':!*_test.go'` — every hit edited or justified. Gates: the six whole-repo checks.

- [ ] Commit `docs: describe restored progress, the Verified flag and the recorded-count estimate`.

### Task 7: Gates

Same block as stage 1's Task 8 (`go fix`, `goimports -l`, vet + tagged vet, `./scripts/run_tests.sh`, `golangci-lint`, integration + par2, the six whole-repo gates). Fix as separate commits; report status lines.
