# Durable Runs — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** One record, written only after the `fsync` that makes its bytes durable, describing a contiguous **run** of articles rather than a single one.

**Spec:** `docs/superpowers/specs/2026-08-22-single-durability-record-design.md`

> **Third plan.** The first was per-article and was hardened over four review rounds. The second changed the record's shape to runs but kept a nine-task decomposition. A review of *that* found the decomposition was fiction: Go's type system will not let the core land in pieces, because deleting `ArticleFact` and `FileExtent` deletes them from every signature that names them, across nine files in five packages. Five of those tasks are **one commit**. What follows says so instead of pretending otherwise. Everything found is under *Corrections applied*.

## Two pull requests

**PR 1 — Task 1 only.** #422, the retry fix. Genuinely independent of everything below, `severity:high` live data loss, and it was previously sequenced seventh behind a refactor. It lands first, alone, against the *current* schema.

**PR 2 — Tasks 2 to 5.** The durability change.

## Execution order

**1 → 2 → 3 → 4 → 5**, in file order. Stated once, here, and nowhere else.

**Task 3 is a single large commit and cannot be split.** Deleting `ArticleFact`/`FileExtent` removes them from `barrier.go`, `prefix.go`, `postanomaly.go`, `resume.go`, `resume_startup.go`, `workset.go`, `stall.go`, `history/repository.go` and `durability.go` at once. Landing the store first leaves those unbuildable; landing the barrier first leaves the old readers reading tables nothing writes. The only alternative is a temporary dual-write — adding the exact path this design exists to delete, plus a window where two records can disagree — and it was rejected for that reason.

## Global Constraints

- **No backwards compatibility.** Persisted state an earlier build wrote may be assumed to satisfy the new invariants. The one carve-out is a security invariant; spec §3.1 argues it does not apply.
- **One migration, in Task 3.** `002_durable_runs.sql` drops `article_facts` and `file_extents`, creates `durable_runs` and `failed_articles`. Migrations run at open, before jobs load, and Global Constraint 1 waives older state — **so no in-flight-download handling is needed, and the plan says so rather than leaving it silent.** Its `Down` needs a sibling to `001_initial.sql:350-351`. Never edit `001_initial.sql`; its two false claims (`:150-152`, `:329`) are superseded in the new migration's comment block, which is the only place that correction can live.
- **Every commit builds and passes.** Task 3 is large *because* of this, not in spite of it.
- **Red-green is observed.** Each failing test runs against unpatched code with `-count=1`; its message goes in the commit body. A cached `ok` is not an observation.
- **Never `git stash`.**
- Gates before every commit: `go fix ./...`, `goimports -w .`, `go vet ./...`, `go test -race ./...`, `golangci-lint run ./...`, `./scripts/run_tests.sh`, `go run ./scripts/check_dup_comments`, `go run ./scripts/check_review_banner`.
- **`go test -tags=crash -timeout=20m ./test/crash/` is a gate for this change.** `run_tests.sh:140-148` excludes it, so it is invisible to every gate above — and `test/crash/harness.go:802-888` queries `article_facts` and `file_extents` **directly**, while `:983` independently reimplements the `articles_done` format. Task 3 deletes all three. Run the suite at Tasks 3, 4 and 5.

## Escalations

1. **A second writer to a formerly append-only store** — merging is read-modify-write.
2. **Cross-package coupling in both directions.** `internal/durability` owns and writes `durable_runs`; `internal/queue` **reads** it. `internal/queue` owns and writes `failed_articles`; `internal/durability` never touches it. Both tables are created by one migration, so state the ownership explicitly rather than leaving a Rule 2 reader to infer it.
3. **A persistence-format change** — two tables dropped, two created.
4. **S4 is inverted** (spec §3.4). The record becomes authoritative and startup does no reads. A departure from `docs/durability-contract.md`, not an elaboration.
5. **S7 is narrowed to size, and `ModTimeNs` is deleted** (spec §3.4). A numbered contract rule loses half its stamp, so it is escalated beside the S4 inversion rather than carried as a cleanup step. The argument is that the mismatch *response* changes: today it costs one read (`recompute`), after this change it costs the whole file, and an mtime moves without any byte moving.
6. **Two public cross-package signatures change: `Assembler.SyncTargetFor` and `SyncTarget.Stat`.** `Stat` drops its `modTimeNs` return under escalation 5, which also lets `FileWriter.Stat` drop its second value — verified to have exactly one consumer, `assembler/synctarget.go:543`. `SyncTargetFor` is the larger one: Step 13b deletes `ArticleCount` and `FileLocalOrdinal`. Verified against source: those are the *only* two methods on `assembler.ArticleMap` (`synctarget.go:150-159`), so the interface goes to zero methods and the type dies with them — taking its sole implementation, `app.manifestArticleMap` (`app/durability.go:20-60`), and reducing `SyncTargetFor(jobID string, am ArticleMap)` (`synctarget.go:185`) to `SyncTargetFor(jobID string)`. One production caller (`app/durability.go:480`). Listed separately from #3 because AGENTS.md escalates a public interface change between packages on its own terms, and Step 13b reads as a deletion list where a reader skims past it.

---

# PR 1

## Task 1: The retry keeps the durability rows it needs

**Files:** `internal/app/app.go` (`RetryHistoryJob`), `internal/history/repository.go` (`Delete`), `internal/queue/sqlite_store.go` (`RestoreRetryProgress` `:1030`), `internal/app/job_finalizer.go`. Test: `internal/app/retry_durability_internal_test.go`.

Closes #422. Lands against the **current** schema — `article_facts` and `file_extents` — because it precedes everything below.

- [ ] **Step 1: Write the failing test** — a retry keeps its durability rows. Without them the truncate bounds to the re-fetched articles alone and the partial is destroyed silently.
- [ ] **Step 2: Run it; confirm it fails** with zero rows. Record the message.
- [ ] **Step 3: Fix the deletion.** Give `Delete` a variant omitting the durability rows. **Move the delete above `queue.Add` at `app.go:1849`**, so rows the re-enqueued job writes before the delete at `:1858` are not caught by it.
- [ ] **Step 4: Fix the two false comments.** `app.go:1841-1842` claims the facts survive, seventeen lines above the `Delete` that removes them; `:1852-1857` enumerates what goes and what stays and omits them. **Both are false at `ae865fec`.** Rewrite each; inherit neither framing.
- [ ] **Step 5: Gate the retention on manifest alignment, and make the gate act.** `retainedMatchesManifest` (`sqlite_store.go:1118`) is the right predicate — it compares `NumFiles`, index order and per-file article count, and `art_idx` derives from cumulative `FileRange`, so a shape match implies identical numbering. But on mismatch `RestoreRetryProgress` only declines the overlay and returns `false` (`:1041-1046`); **it deletes nothing.** `applied` is returned at `app.go:1831`: hoist it out of its `if store :=` block and branch — **`applied == false` takes the full `Delete`; only `applied == true` preserves the rows.**

  Write down rather than discover: `applied == false` also covers `len(retained) == 0` and `job.manifest == nil`; and the shape check inherits the existing "same NZB bytes re-parse deterministically" assumption, so it does not close a segment reordering *within* a file. That exposure is pre-existing and not widened.
- [ ] **Step 6: Reconcile the two ownership comments** — `job_finalizer.go:104-110` says a failed job's rows are kept for the retry; `repository.go:348-357` says the history entry owns them.
- [ ] **Step 7: `go test -race ./internal/app/ ./internal/history/ ./internal/queue/`. Commit** — `fix(app): stop the retry deleting the durability rows it needs`. Closes #422.

---

# PR 2

## Task 2: Carry the CRC to `Drain`

**Files:** `internal/durability/synctarget.go`, `internal/assembler/assembler.go`, `filewriter.go`, `writecache.go`, `internal/app/pipeline.go`. Test: `internal/assembler/filewriter_test.go`.

Additive and independently landable — the field is unused until Task 3.

**Established:** `bufferedArticle` (`writecache.go:98`) and `runPart` (`:123`) already carry offset, id and length. Add `crc32` to both and to `noteWritten`. No new map.

- [ ] **Step 1: Read `filewriter.go:602`'s `Accept` signature first.** It is `Accept(id articleID, off int64, data []byte) error` — not a `WriteRequest`, and it returns an error.
- [ ] **Step 2: Write the failing test** — a written article's CRC reaches `Drain`.
- [ ] **Step 3: Run it; confirm it fails naming `CRC32`, not `Accept`.** Record the message.
- [ ] **Step 4: Thread the field** through `WriteRequest`, `Accept`, `bufferedArticle`, `runPart`, `noteWritten`, `Drain`, and `pipeline.go`'s request construction.
- [ ] **Step 5: `go test -race ./internal/assembler/ ./internal/durability/ ./internal/app/`. Commit** — `feat(durability): carry the decoded CRC through to the drain report`.

---

## Task 3: Replace both records with durable runs — one commit

**Two commits, not one.** The earlier claim that the whole task was atomic is true of the *deletions* and false of what precedes them:

- **3a — purely additive.** Steps 1–5: a `002_durable_runs.sql` that only **creates** `durable_runs` and `failed_articles`, the store, and the merge and associativity tests. Nothing reads the new tables and nothing is deleted, so the tree builds and every existing test passes unchanged. This is what makes Steps 1–3's red-green possible at all: a merge test cannot be observed failing inside a commit that has already removed the types its neighbours compile against.
- **3b — the flip.** Steps 6–18, plus a `003` that **drops** `article_facts` and `file_extents`. This half genuinely is atomic: deleting `ArticleFact` and `FileExtent` removes them from every signature that names them, across nine files in five packages.

Splitting the migration in two is the point — a single `002` that both creates and drops cannot be half-applied, and would force 3a to carry the deletion.

### Test triage — do this before writing 3b

`internal/durability`'s tests total **5,834 lines** across 20 files, and 3b deletes the types most of them name. They do not fail; they stop **compiling**, and a package that does not build reports no coverage at all. A green suite afterwards proves nothing about what was dropped.

Every file and test name below was verified present in the tree. **Four verdicts, not three** — two files carry both obsolete and surviving tests, and a whole-file verdict is exactly what hides the survivors.

**DELETE — the mechanism is gone**

| File | |
|---|---|
| `extent_test.go` (121 lines) | `Bitmap`, bit-width padding masks, `FileExtent` |
| `factlog_sqlite_test.go`, `factlog_bench_test.go` | `article_facts` CRUD, R1 immutability, `INSERT OR IGNORE` |
| `prefix_test.go`, `verifiedprefix_bench_test.go` | `verifiedPrefix`, `prefixWalk`, `VerifiedTo` contiguity walks |
| `resume_writeback_test.go` | `recompute()` and the write-back — see Step 12 |
| `barrier_test.go`, six cases | `TestBarrier_VerifiedToAdvancesOnlyOverAGaplessPrefix`, `…PriorPaddingBitsDoNotInflateTheDurableCount`, `…PriorBitmapWidensToTheArticleCount`, `…UnplaceableArticleIsAnError`, `…FactLogReadFailureAbortsTheBarrier`, `…RecomputesThePrefixCRCRatherThanCarryingIt` |

**PORT — the property survives under a new mechanism.** This is the pile that matters. Anything not explicitly ported is a property silently dropped.

| File | Property to re-assert |
|---|---|
| `extentstore_sqlite_test.go` → `durable_runs_sqlite_test.go` | merging when offset **and** art_idx abut; **article-granularity dedup against stored rows before grouping** (Step 4); order-independence and strict left-to-right CRC associativity; transaction rollback; `DeleteJob` |
| `finalize_test.go` | truncate bound is `max(offset+length)` over stored runs; no truncate when `bound == 0` |
| `finalize_crc_test.go` | the CRC predicate — **exactly one row, `offset == 0`** — pinned red-green on *both* the holed file (R23) and the overlapped file (#387), per Step 8 |
| `finalize_overlap_test.go` | `PostAnomaly` raised when `Σ length > stat size`, and **not** when `Σ length < stat size` |
| `resume_test.go`, `resume_missingfile_test.go` | the size-only gate: `size >= max(offset+length)` adopts; missing or short deletes the file's runs and returns Outstanding |
| `test/crash/harness.go` | its direct SQLite reads move from `article_facts`/`file_extents`/`articles_done` to `durable_runs`/`failed_articles`. Mandatory gate on Tasks 3, 4 and 5 |
| `internal/queue/prune_durability_test.go`, `manifest_gate_test.go` | prune and schema assertions retarget the new tables |

**KEEP — untouched by the mechanism change**

`closerace_test.go`, `closerace_phases_test.go` (file-close race, `ErrFileNotOpen`), `proof_test.go` (the unexported `DurableProof` payload against `Queue.AckDurable`), `raise_test.go` (fault classification, `ErrTargetUnavailable`), and from `barrier_test.go`: `TestBarrier_SyncPrecedesCommitAndAck`, `…SyncFailureAcksNothing`, `…CommitFailureAcksNothing`, `…AckFailurePropagates`, `…ConfirmsOnlyAfterTheCommitAndAck`, `…MultipleFilesSyncAllBeforeAnyClaim`, `…PermanentFaultFailsRatherThanStalls`, `TestRouteFault`.

**SPLIT — the file survives, some of its tests do not.** Both were missing from a first triage that assigned verdicts per file; between them they hold **640 lines**, and one is the #413 suite.

`mutation_gaps_test.go` (270 lines)

| Test | |
|---|---|
| `TestBitmap_IndexingAcrossAWordBoundary` | delete |
| `TestResume_FastPathWidensAStoredBitmapNarrowerByAWholeWord` | delete |
| `TestBarrier_PriorExtentWidensAStoredBitmapNarrowerByAWholeWord` | delete |
| `TestResume_FactAtExactlyTheArticleCountFailsLoudly` | delete — the A2/R28 loud failure goes with `FileLocalOrdinal` (Step 7, Step 13b) |
| `TestResume_VerifiesAFactEndingExactlyAtEndOfFile` | **port** — the same boundary against the size gate, `size == max(offset+length)` exactly |
| `TestFinalizeFile_FactsButNoneDurableDoesNotTruncate` | **port** — this is Task 4's `bound == 0` branch |

`overlap_report_test.go` (370 lines) — **the #413 overlap-reporting suite.** Task 4 reimplements overlap detection on `Σ length`, so these are the properties that must survive a mechanism swap, not tests of a mechanism being removed.

| Test | |
|---|---|
| `TestAdmit_LatchesPerJobAndFile` | **keep** — the latch lives in `Barrier.reported`, unchanged |
| `TestRun_RaisesEachOverlapOnce` | **keep** — same |
| `TestForgetJob_LetsARetryWarnAgain` | **keep** — same, and PR #422 touches its subject |
| `TestFinalizeFile_ReportsOverlappingDurableArticles` | **port** to the `Σ length` mechanism |
| `TestRun_ReportsAnOverlapWhenNothingWasAcked` | **port** |
| `TestFinalizeFile_DoesNotReportAHoleAsAnOverlap` | **port** — `Σ length < size` must stay silent |
| `TestFinalizeFile_ReportsAnOverlapAboveAPermanentHole` | **port, and expect it to fail** — see Task 4's cancellation note. Decide there whether it becomes a documented gap |
| `TestOverlapFrom_ReportsTheIntersectionNotTheVictimsEnd` | delete — `overlapFrom` takes the deleted `prefixWalk`, and `Σ length` yields a total excess, not an intersection |
| `TestOverlapReason_NamesBothArticlesAndTheFile` | delete — a merged row cannot name two articles. **`PostAnomaly.Reason`'s wording changes**, which Step 17b's `postanomaly.go` row must cover |

`finalize_factgap_test.go` is a third split, and the one a per-file verdict got wrong. `TestFinalizeFile_DoesNotTrimBelowADurableArticleWithNoFact` **deletes** — Class A/Class B divergence is unrepresentable in a single record. But `TestFinalizeFile_PostTruncateStatFaultNamesTheFile` **ports**: it pins the R27 path that names the file on a post-truncate stat fault, that stat survives (Task 4 Step 3 reasons about it at `barrier.go:710`, `:726`), and deleting the file would drop a live storage-fault property along with the obsolete one.

### Schema (`002_durable_runs.sql`)

```sql
CREATE TABLE durable_runs (
  job_id        TEXT    NOT NULL,
  file_idx      INTEGER NOT NULL,
  first_art_idx INTEGER NOT NULL,
  last_art_idx  INTEGER NOT NULL,
  offset        INTEGER NOT NULL,
  length        INTEGER NOT NULL,
  crc32         INTEGER NOT NULL,
  PRIMARY KEY (job_id, file_idx, offset)
);
CREATE TABLE failed_articles (
  job_id  TEXT    NOT NULL,
  art_idx INTEGER NOT NULL,
  PRIMARY KEY (job_id, art_idx)
);
```

`failed_articles` is a table rather than a column on `job_files` because its reversal is per-article and per-job (`ResetForRetry`, `resetForReload`), which a packed bitmap column can only express by rewriting the whole blob — the coupling that made the old `articles_done` encoding fragile. It is **owned and written solely by `internal/queue`.**

### Store API

| Method | Used by |
|---|---|
| `Commit(ctx, jobID, arts []DurableArticle) error` | the barrier, both commit sites |
| `ForFile(ctx, jobID, fileIdx) ([]Run, error)` | the resume gate, `recordAssembledCRC` |
| `ForJob(ctx, jobID) ([]Run, error)` | `stall.go`'s seed — **a whole-job read the earlier API sketch omitted** |
| `DeleteJob(ctx, jobID) error` | `deleteJobDurability`, `pruneDurabilityRows` |

**`Commit` takes articles, not runs, and the store groups them.** An earlier
draft had the barrier build runs and hand them over. That put a derived value's
construction at two call sites, and — worse — made the correct dedup
unreachable; see spec §6 and Step 4 below. Run construction has one owner, and
it is the store.

**`offset` and `length` need no quoting** — checked, do not "fix" it. A review
round flagged them as SQLite keywords that would fail at apply time. Probed
against `modernc.org/sqlite`: `CREATE TABLE`, `INSERT`, `SELECT … WHERE offset
>= 0` and `SELECT max(offset+length)` all execute unquoted without error.
SQLite admits most keywords as identifiers, and `length` is a function name
rather than a reserved word at all. Recorded because the claim is plausible
enough to be re-raised.

### Steps

- [ ] **Step 1: Write the merge tests before the merge.** Two runs abutting in both offset and index merge with the combined CRC; two abutting in offset but not index do not; two abutting in index but not offset do not; **a new run whose index range a stored row already covers is dropped, not inserted**; merging is order-independent.
- [ ] **Step 2: Pin associativity against real bytes.** For a file of N articles, the merged row's CRC must equal `crc32.ChecksumIEEE` of the concatenation. Everything rests on this. **`Combine(a.crc32, b.crc32, b.length)` requires `b.length` to be b's whole run length, not one article's** — strict left-to-right merging only. That is the one arithmetic trap; pin it.
- [ ] **Step 3: Run both; confirm they fail.** Record the messages.
- [ ] **Step 4: Implement `Commit` — and dedup at ARTICLE granularity against STORED rows, before grouping.** `Confirm` runs only after `AckDurable` (`barrier.go:287`, `:776`), so an ack failure leaves the report unconfirmed while the commit has landed, and the next `Drain` re-delivers (R12). Two things follow, and the second is the one an earlier draft got wrong.

  **Deduping the drained set against itself does not cover it.** A re-delivered article whose stored row has since merged *forward* does not collide on the primary key: stored `[off=0,len=1000]` covering articles 0–9, plus a fresh `[off=500,len=100]` for article 5, gives `Σ length` 1100 against a 1000-byte file — **a permanent false overlap finding on a healthy file.**

  **Deduping whole RUNS against stored rows does not cover it either.** That was the previous fix and it leaks as soon as a re-delivery is adjacent to genuinely new work. Articles 5–9 re-delivered alongside new 10–12 group into one run `[5,12]`; no stored row covers all of it, so no whole-run test drops it, and it is inserted beside the stored row. Same false overlap, one level deeper.

  So the order is **subtract, then sort, then group, then merge**: drop every incoming article whose `art_idx` a stored row already covers, and build runs from what is left. This is why `Commit` takes articles rather than runs — the barrier cannot perform this subtraction without reaching into the store's knowledge from two call sites.

  Pin the adjacent case specifically. The wholly-covered case passes under both rules and proves nothing about the one that failed.
- [ ] **Step 5: Range-scope `Commit`'s read.** Only rows bracketing the new runs' offset span can merge, and the primary key supports that query. The barrier fires at 64 MiB and an article is ~700 KB, so at most ~90 arrive per cycle; a full-file read is fine at one row and wasteful at twenty thousand.
- [ ] **Step 6: Hand the drained articles to `Commit`, at BOTH commit sites.** Two drains — `:162` (`Run`) and `:599` (`FinalizeFile`) — and two commits — `:272` and `:769`. The barrier passes the drained set through and does **not** group it; grouping is Step 4's, inside the same transaction, before the ack. Index-abutment is **sufficient and conservative, not necessary**: byte-abutting runs whose article indices disagree are refused a merge that would still be CRC-correct, which costs rows and never correctness.
- [ ] **Step 7: Replace the truncate bound with `max(offset+length)`, and delete both guards.** Note that deleting `buildExtent` also deletes its `artCount <= 0` guard (`:341-347`) and its `FileLocalOrdinal` A2/R28 failure (`:362-369`). Both are intended; say so where they went.
- [ ] **Step 8: Rewire `recordAssembledCRC` (`app/durability.go:957-980`).** It is the *only* production path carrying the whole-file CRC to par2 — `app.extents.LoadFile` → `HasPrefixCRC` guard → `queue.SetFileCRC32` → `par2.VerifyCRCs` — and no earlier draft named it. Reimplement as: **publish `crc32` only when the file has EXACTLY ONE row, and that row has `offset == 0`.**

  **Not "some row spans the file".** That was this step's previous wording and it republishes #387. §3.3 writes an overlapping article rather than refusing it, and gives it its own row — so articles tiling `[0,1000)` into one merged row, plus a displaced article at `[450,550)` in a second row, still satisfy the span form: a row starts at 0 and its length equals the maximum. The CRC would be published, combined from the *original* articles, over a file holding foreign bytes at 450–550. par2 matches a manifest that does not describe the disk and the recovery volumes are never fetched. `prefixWalk.consumedAll` (`prefix.go:120-122`) is the guard that catches this today, and its doc at `:95-102` names #387 by number — Step 13 deletes it, so the replacement predicate is what has to carry the guarantee.

  The row-count form also preserves the R23 semantic the current comment states — *"a file with a permanently failed article has a prefix that stops at the hole, and recording that as the file's CRC would report a mismatch against par2 for a file that is merely incomplete"* — because a holed file keeps more than one row.

  **Pin both cases red-green:** the holed file (the R23 case, which the span form also passes) *and* the overlapped file (the #387 case, which only the row-count form catches). A pin on the first alone is what let the span form look correct.
- [ ] **Step 9: Rewire `stall.go`'s seed.** `seedFromCommittedExtents` (`:453-467`) calls `app.extents.Load` and `queue.SeedFromExtents`, both deleted. Its doc says the seed is what stops a job that finalized a file during a stall from re-fetching it, so the behaviour must survive: `ForJob` plus whatever replaces `SeedFromExtents`.
- [ ] **Step 10: Reconcile `failed_articles`' THREE reversal sites.** Writing is one site (`AckPermanentFailure`, `workset.go:94`). Reversal is not: `resetForReload` (`progress.go:767`) **and** `Job.ResetForRetry` (`job.go:812`), the latter reached from both `Queue.Retry` (`queue.go:658`) and `RetryHistoryJob`. Today these stay consistent because `articles_done` is re-serialised wholesale on the next store update; a separate table has no wholesale rewrite, so stale rows survive a retry and **the next restart marks the retry's re-fetched articles failed.** Batch the delete per job, never per article.

  **Scope the batch to exactly the jobs the caller resets.** `ClearAllEmitted` (`queue.go:1513-1520`) loops every article of every job holding both a manifest and progress — that is, **resident jobs only** — and calls `resetForReload` on each, so a job-scoped `DELETE … WHERE job_id = ?` per resident job is exactly equivalent to the per-article clearing it replaces. A delete that swept every job instead would resurrect the permanently-failed articles of every *non-resident* job, and the symptom is a silent re-download storm rather than an error. The equivalence is what makes the batch safe; the scoping is what makes the equivalence true.
- [ ] **Step 11: Trim `ResumeResult`.** `Resume`'s **return type changes** — `VerifiedTo`, `PrefixCRC`, `HasPrefixCRC`, `BytesDurable`, `ModTimeNs` (escalation 5) and the `Durable Bitmap` all go (`resume.go:23-56`). `Size` stays — it is the one half of the S7 stamp §3.4 keeps. An earlier draft claimed the signature was unchanged, in two places; only the parameter list is. `fileResumer` (`resume_startup.go:27`) and `resumeJobFiles`'s construction (`:325-330`) both change, as does its `recomputed` counter.
- [ ] **Step 12: Reduce `Resumer` to the `stat` gate — which retires the SECOND WRITER to the durability record.** This is the change's strongest Rule 2 result and neither document previously said it. `writeBack` calls `r.exts.Commit` (`resume.go:274`) and its own doc describes itself as committing "a resume's own answer as the file's Class B record", so `Resumer` is a genuine second writer today. After this step the barrier's commit is the sole writer, and the resume only ever **deletes** — and only when the file on disk contradicts the record. Say so where the gate lives; it is the justification for trusting the record at all. `recompute`, `verifyRegions` and `writeBack` go; `Resume` compares `stat(path).size >= max(offset+length)`, discards the file's rows on a shortfall, and returns the resume set. **The sweep runs inside `Start` before dispatch** (`resume_startup.go:52-56`), which is what stops the assembler re-creating and pre-allocating a deleted partial so the gate passes over zeros — a property of the sweep's placement, not of the gate. Say so where the gate lives. Log every discard at `Warn`.
- [ ] **Step 13: Delete `prefix.go` and its THREE dependents' uses** — `barrier.go`, `resume.go:309` (inside `recompute`, Step 12), and `postanomaly.go:48`, whose `overlapFrom(w prefixWalk, …)` takes the deleted type. **`PostAnomaly` itself stays** — it is the barrier's public return type, reaching the user through `app.reportPostAnomalies` → `handlePostAnomaly`. Only its producer changes.

- [ ] **Step 13a: Delete `extent.go` outright — all 157 lines, both `FileExtent` and `Bitmap`.** An earlier draft said "most of". It is all of it: every `Bitmap` user is on this task's delete list — `priorExtent` (`barrier.go:527,535`), `durableAt` (`:555`), `durableExtent` (`:797`), `recordedExtent` (`:827`), `ResumeResult.Durable` and `verifyRegions` (`resume.go:26,107,293,341`), `extentstore_sqlite.go` (`:109,142`), and `fileDurableBitmap`/`BitmapFromBytes` in `queue/workset.go:400-421`. Nothing outside those holds one.

- [ ] **Step 13b: Delete `SyncTarget.FileLocalOrdinal` and `SyncTarget.ArticleCount`, and everything behind them.** A run carries `first_art_idx`/`last_art_idx` directly, so no global-index-to-file-bit conversion exists to perform. Their own docs name the reason they existed — *"the barrier needs it to size the durable bitmap"*, *"maps a global article index to this file's bit position"* — and this task deletes the bitmap.

  Every caller is inside code already being deleted: `buildExtent` (`barrier.go:341,362`), `durableAt` (`:557`), `durableExtent` (`:800`), `recordedExtent` (`:836`). Nothing else calls either.

  The slice spans three packages, and `Truncator` embeds `SyncTarget` so both interfaces shrink together:

  | | |
  |---|---|
  | `internal/durability/synctarget.go:163,169` | the interface declaration |
  | `internal/assembler/synctarget.go:154,159` | the second declaration |
  | `internal/assembler/synctarget.go:401,408` | `jobSyncTarget`'s implementations |
  | `internal/app/durability.go:37-60` | `manifestArticleMap`, which exists only to back them |

  Small in lines, but it is a **concept** leaving an interface, so it is listed as its own step rather than folded into a deletion list where a reader would skim past it.

  **The write cache stays** — it was surveyed as a candidate and deliberately kept.
- [ ] **Step 14: Derive article resolution in the queue.** `done` = covered by a run; `failed` = a `failed_articles` row; `unfinished` = neither. Delete `encodeArticlesDone`, `decodeArticlesDone`, `decodeArticleFlags`, the column from `job_files` and `history_job_files`, `SeedFromExtents`, `ReplaceFromResume`, and the crash harness's copy. **The consumer is `newJobProgressSized` (`persistence.go:130`, loop `:150-183`), not `sqlite_store.go`** — it applies `f.Failed[i]` only inside the `f.Done[i]` branch, so anything leaving `Done` empty makes every article of every non-resident job read as Pending.
- [ ] **Step 15: Solve the boot cost.** `ArticleCountsByJob` derives Pending for every non-resident job at startup without a manifest (`sqlite_store.go:648-651`). Runs make this far cheaper than per-article rows, but establish the query shape. **If none is cheap enough, stop and re-scope.**
- [ ] **Step 16: Check the gates that fail on stale entries.** `manifest_gate_test.go`'s allow-list is keyed on method names and fails on stale entries as well as missing ones. `internal/history/testdata/schema.golden` and `schema_guard_test.go` both move with the migration.
- [ ] **Step 17: Nothing to do — `p.done` sizing does not depend on `articles_done`.** Kept as a step so it is not re-raised. The earlier wording said `Done`'s length was the sizing source; it is not. `newJobProgressSized` sizes every bitset from `Σ FileMeta.ArticleCount` (`persistence.go:130-137`), and the panic at `progress.go:605` compares `p.done.Len()` against `m.NumArticles()` — manifest against manifest. Step 14 removes what *fills* the bitsets, never what sizes them. **Do still confirm the `Done`-gated read at `:150-183`**, which is a real hazard and belongs to Step 14: it applies `f.Failed[i]` only inside the `f.Done[i]` branch, so anything leaving `Done` empty makes every article of every non-resident job read as Pending.
- [ ] **Step 17b: Sweep the comments THIS commit falsifies, before committing it.** Task 5 sweeps the `docs/` prose. These are in-package comments this task's own diff makes wrong, and AGENTS.md is explicit that a sweep runs against the diff the commit will land as — deferring them to Task 5 ships the drift in the commit that caused it. Each was read and confirmed present:

  | Location | What it asserts that stops being true |
  |---|---|
  | `fact.go:1-18` | the **package doc**, which defines Class A and Class B as the package's organising idea. Both classes are gone. This is the largest single rewrite in the task and the easiest to miss, because nothing in the diff sits next to it. |
  | `barrier.go:14-31` | names `SeedFromExtents` and `ReplaceFromResume` as the seeding doors that bypass the proof, and `durability.FileExtent` as the exported type they take. All three are deleted by Step 14 and Step 13a. |
  | `barrier.go:112-119` | `ForgetJob`'s rationale, keyed on `article_facts` and `INSERT OR IGNORE`. See the note below — this one is already wrong today. |
  | `barrier.go:39-59`, `:225-238`, `:574-597`, `:383-387` | the checkpoint/finalize narration, in terms of Class A walks and Class B extents. |
  | `postanomaly.go:8-24` | *"found while walking a file's Class A facts"* — there is no walk after Step 13. |
  | `synctarget.go:138-142` | `Stat`'s doc names the **S7 validity stamp** as the `(size, mtime)` pair. **Decided (escalation 5): S7 narrows to size, `modTimeNs` is deleted from the signature.** Rewrite the doc to name the surviving half and why — not to delete the sentence, which would leave a reader who knows S7 unable to tell a narrowing from an oversight. |

- [ ] **Step 18: Run everything, including the crash suite. Commit** — `refactor(durability): replace two per-article records with durable runs`. Closes #421 and #389.

---

## Task 4: Check the recorded lengths against the file at completion

**Files:** `internal/durability/barrier.go` (`FinalizeFile`). Test: `internal/durability/barrier_test.go`.

| | Meaning |
|---|---|
| `Σ length > stat size` | **definite overlap** — articles wrote over each other |
| `Σ length == stat size` | **no evidence of overlap** — not proof of a clean tiling, because a hole and an equal-sized overlap cancel here (see below) |
| `Σ length < stat size` | articles missing or failed — ordinary, not a finding |

**A hole and an overlap of the same size cancel, and this check cannot see either.** `Σ length` counts recorded bytes; the file's size is `max(offset+length)`. An overlap of N bytes adds N to the sum, a hole of N bytes takes N off the span — so a file carrying both lands on `Σ length == stat size` and is reported *"complete and cleanly tiled"* while holding both defects. The row above says so and is wrong in exactly that case.

This is a **detection gap the old mechanism did not have**: the prefix walk compared adjacent extents structurally, so it saw the overlap regardless of what else was wrong with the file. `TestFinalizeFile_ReportsAnOverlapAboveAPermanentHole` is the existing pin, and it will not survive a straight port — see the triage matrix.

Two things bound it, and they are why this is a documented gap rather than a blocker:

- **It cannot reach #387.** A hole means a gap between rows, so the file has more than one row and Step 8's predicate withholds the whole-file CRC on the row count alone. The dangerous outcome — publishing a CRC over bytes that are not there — is closed by a different guard, and closed structurally.
- **par2 repairs both.** The file is already incomplete, so recovery volumes are fetched regardless of whether the overlap was also named.

What is lost is a *warning*, on a file the user is already being told is incomplete. Record the gap in the contract, and change the table's middle row from "cleanly tiled" to what it can actually support.

**Nothing is refused at accept time.** `acceptedAt` is `map[int64]offsetOwner` with `offsetOwner{id, written}`, keyed on start offset with no length — it answers #383's exact-collision question, not range intersection. Building an index that could would duplicate a fact this check already covers.

- [ ] **Step 1: Write the failing test** — an overlapped file is flagged, and a file with a permanently failed article is **not** (`Σ length < size` legitimately).
- [ ] **Step 2: Run it; confirm it fails.** Record the message.
- [ ] **Step 3: Implement, and name the `bound == 0` branch.** The post-truncate stat lives inside `if bound > 0` (`barrier.go:710`, `:726`), and `buildExtent`'s pre-truncate stat (`:348`) is gone by Task 3. Say what happens when there is no truncate and therefore no stat.
- [ ] **Step 4: Surface it as a `PostAnomaly`, not a log line.** That is the existing path to the user for "something structurally wrong with what the servers served", and #413 landed that visibility deliberately. A `Warn` beside it would silently downgrade overlap reporting.
- [ ] **Step 5: `go test -race ./internal/durability/`. Commit** — `feat(durability): flag a file whose recorded lengths exceed its size`.

---

## Task 5: Contract and comment sweep

**Files:** `docs/durability-contract.md`, `docs/ARCHITECTURE.md`, `docs/queue-lifecycle.md`, `docs/article-validation-contract.md`, `internal/durability/proof.go`, `internal/queue/workset.go`.

- [ ] **Step 1: Read `docs/durability-contract.md` and `docs/ARCHITECTURE.md` in full.** Not grep — the claims that survive a grep are the ones restated in prose sharing no token with the code.
- [ ] **Step 2: Rewrite the Class A/B table, §4, and the resume section.** R1 and R2 are deleted; **S4 is inverted**, not amended — say so, because a reader who knows the old contract will assume a recomputation still wins. **S7 is narrowed to size**, and must be stated as a narrowing with its reason, not silently reworded: a reader who knows the `(size, mtime)` stamp cannot otherwise tell a decision from an omission. Record Task 4's `Σ length` cancellation gap here too — it is a bound on what the overlap check detects, which belongs in the contract rather than only in the plan.
- [ ] **Step 3: Grep every falsified literal from the repository root.**

```bash
git grep -n 'INSERT OR IGNORE'; git grep -n 'R1\b'; git grep -n 'R2\b'
git grep -n 'asserts nothing about presence'; git grep -n 'Class A'; git grep -n 'Class B'
git grep -n 'ArticleFact'; git grep -n 'FileExtent'; git grep -n 'verifiedPrefix'
git grep -n 'PrefixCRC'; git grep -n 'recordedExtent'; git grep -n 'articles_done'
git grep -n 'articles_done' -- test/
git grep -n 'ModTimeNs'; git grep -n 'modTimeNs'; git grep -n 'S7'
git grep -n 'writeBack'; git grep -n 'cleanly tiled'
```

  Named consumers the earlier sweep list missed: `internal/api/queue.go:184,204,215`, `internal/app/statusinfo.go:233`, `internal/assembler/assembler.go:163,1695`, `internal/history/testdata/schema.golden`.

- [ ] **Step 4: Fix the two claims already false in the tree.** `progress.go:790-794` says the done bit "is set only once the bytes have reached WriteAt (#355)", contradicting `statusinfo.go:200-207` **today**. And any comment describing QuickCheck as a par2 bypass — it is filename relocation, it computes its own CRC from disk (`tryMatchCRC32File` takes a path), and the verification-without-I/O mechanism is `par2.VerifyCRCs`.
- [ ] **Step 5: Re-read `proof.go` and `workset.go`.** Their docs assert the barrier is the only `done` authority — still true, different mechanism.
- [ ] **Step 6: Run `pr-review-toolkit:comment-analyzer`** over the cumulative branch diff, once, on the last round.
- [ ] **Step 7: Run both whole-repo gates and the crash suite. Commit** — `docs(durability): restate the contract for durable runs`.

---

## Inconclusive / Deferred items

1. **How often do runs actually merge?** Two of three links are settled from source: `art_idx` order **is** NZB segment-`number` order (`normalizeFileStruct`, `parser.go:502-519`), and yEnc part order **is** offset order by the format. The open link is whether an NZB's segment `number` equals the yEnc part number in the body.

   *Confirmed unanswerable in-repo:* the only two `=ypart` emitters are `test/mocknntp/articles.go` and `test/integration/testhelpers_test.go`, both our own generators. There are no captured real article bodies.

   *Probe:* instrument the decoder, which already parses `=ypart begin=`. Compare article `N`'s offset against `N-1`'s `offset+length` and count disagreements. Ships independently.

   **The only item left open, and it gates nothing.** A disagreement costs a whole-file CRC for that file — neither the truncate bound nor the integrity check references `art_idx`.

2. **Boot-cost query shape** (Task 3 Step 15) — a re-plan trigger if no cheap shape exists.

### Resolved

- **Coalesced-run partial writes — branch (a).** `flushRun` discards the byte count, so a short write leaves real bytes on disk that no record describes; the probe wrote 50 of 200 and read them back. What bounds it is `fail` routing the run's articles to **Outstanding**, not resolving them (A1). The file cannot reach `TotalParts`, so no truncate runs while unrecorded partial bytes exist. Pinned by `TestFileWriter_ShortWriteLeavesNoClaimOverPartialBytes`, mutation-checked.
- **Merge cost** — a query-shape choice, now Task 3 Step 5.
- **`Resumer`** — reduced, not deleted; its return type changes.
- **Range knowledge at accept time** — not needed; the question dissolved.

---

## Corrections applied

### Rounds one to four (against the per-article plan)

Twenty findings. Most concerned machinery this shape does not have — the five-consumer union, `durableAt`'s index-parallel closure, the guard sequencing. These were about the **code** and still apply: two drains and two commits; the drained and stored sets overlap; the drained set is write-ordered; `articles_done` is the real `done` store and `persistence.go` its consumer; the crash suite reimplements that format and no standard gate runs it; `RetryHistoryJob` holds two comments that are false today; a rejected article counts toward `TotalParts`; `ExpectedSize` is the encoded byte count; `Accept` is `(id, off, data) error`.

### Round five (first against the run plan)

1. **The decomposition was fiction.** Five tasks are one commit, because deleting a type deletes it from every signature across nine files in five packages. The plan now says so and splits #422 out instead of pretending the rest can stage.
2. **`Commit` was idempotent against the drained set, not against stored rows.** A re-delivery whose stored row merged forward inserts a duplicate and raises a permanent false overlap on a healthy file.
3. **`recordAssembledCRC` was unwired** — the only production path carrying the whole-file CRC to par2, named by the spec as its justification, named by no task.
4. **`stall.go` was unwired** — a live consumer of both `app.extents.Load` and `SeedFromExtents`, and the store API had no whole-job read.
5. **`failed_articles` has three reversal sites and one was named.** `ResetForRetry` reaches it from two callers, and without a wholesale rewrite the stale rows survive to the next restart.
6. **`prefix.go` had three dependents named as one** — `resume.go:309` and `postanomaly.go:48` were missing, and `PostAnomaly` is a public return type that stays.
7. **Task 4 would have downgraded overlap reporting to a log line**, beside an anomaly path that already reaches the user.
8. **`ResumeResult`'s dead fields** were listed as deleted in the spec and by no task.
9. **"`Resume`'s signature is unchanged" was false in two places** — the return type changes.
10. Also: `resetForReload` is at `:767`; the sweep is `resume_startup.go`, not `app.go`; `schema.golden` and `schema_guard_test.go` move with the migration; Task 4's stat sits inside `if bound > 0`; `Combine`'s second length argument is the whole run's; index-abutment is conservative, not necessary; deleting `buildExtent` also deletes two guards worth naming.

### Deletion survey

After the round-five rewrite, the surrounding code was surveyed for machinery this design makes purposeless, rather than only for what the plan already named.

11. **`extent.go` goes entirely, not partly.** Every `Bitmap` holder is on the delete list, so `Bitmap` and `FileExtent` both lose their last consumer — 157 lines, where the plan had said "most of".
12. **`SyncTarget` loses two methods and the concept behind them.** `FileLocalOrdinal` and `ArticleCount` exist to size and index a per-file durable bitmap. Runs carry their own article-index range, so the conversion has nowhere to happen. The slice reaches three packages and both interface declarations.
13. **The write cache was surveyed and deliberately kept.** `writeCache` is ~448 non-test lines plus ~680 of tests and a `write_cache_size` config key, it coalesces contiguous articles into fewer `WriteAt` calls, and the no-cache path already exists and is exercised (`withCacheBytes(0)`). It is not needed for run formation — runs are grouped from the drained set's offsets, not from how the bytes reached disk — so it was a genuine candidate. **Decision: keep it.** Recorded here so a later reader does not re-litigate it as an oversight.

Measured totals for the change: roughly **1,850 non-test lines deleted against ~360 written**, before tests.

### Round six — Task 3 validation

14. **The whole-file CRC predicate republished #387.** Spec §3.5 and Step 8 gated the CRC on *"some row has `offset == 0` and `length == the file's size"*, while §3.3 stated the different — and correct — rule that an overlapped file *"never collapses to one [row], and so never publishes a whole-file CRC"*. The two are not equivalent, because §3.3 **writes** an overlapping article and gives it its own row: a file tiled `[0,1000)` into one merged row plus a displaced article at `[450,550)` satisfies the span form, and the CRC would be published over foreign bytes. `prefixWalk.consumedAll` is the guard that catches this today and this change deletes it. Both now read **exactly one row, starting at offset 0**, and Step 8 pins the overlapped case as well as the holed one.
15. **Correction #2 leaked one level deeper.** Dropping whole *runs* a stored row covers does not help when a re-delivery is adjacent to new work: articles 5–9 re-delivered beside new 10–12 group into `[5,12]`, which no stored row covers, and insert beside the stored row for the same permanent false overlap. Dedup moved to **article granularity, before grouping** — which is only reachable if the store, not the barrier, builds the runs. `Commit` now takes articles, and run construction has one owner instead of two call sites.
16. **No step swept the comments Task 3 falsifies.** Task 5 sweeps `docs/`; these are in-package, and AGENTS.md requires the sweep to run against the diff the commit lands as. Six locations confirmed present, including `fact.go:1-18`, which is the **package doc** defining Class A and Class B as the package's organising idea. Now Step 17b.
17. **S7 was losing its mtime half silently.** `synctarget.go:138-142` names the `(size, mtime)` pair as the validity stamp a resume checks; §3.4 keeps only the size comparison. Neither document said so. Step 17b forces the decision rather than letting the stamp erode.
18. **Task 3 is splittable after all, in the one place it matters.** Steps 1–5 are purely additive — a `002` that only *creates*, with the `003` that drops old tables held for the flip. Without that split, Steps 1–3's red-green is unobservable, because the merge tests would sit in a commit that has already deleted the types their neighbours compile against.
19. **`SyncTargetFor`'s signature change was buried in a deletion list.** `ArticleMap`'s only two methods are the ones Step 13b deletes, so the interface dies and the public cross-package signature changes. Now escalation 5.
20. **Step 17's premise was false.** `p.done` is sized from `Σ FileMeta.ArticleCount`, and the panic compares against `m.NumArticles()` — manifest against manifest, untouched by deleting `articles_done`. The real hazard is the `Done`-gated `Failed` read, which already belonged to Step 14.
21. **The SQLite-keyword finding was itself wrong.** A round-six finding said `offset`/`length` must be quoted or the migration fails. Probed against `modernc.org/sqlite`: all four statement shapes execute unquoted. Recorded in the Store API section so it is not re-raised.
22. **Test strategy was the plan's biggest silence.** ~4,000 test lines in `internal/durability` name types 3b deletes, so they stop compiling rather than failing — and a package that does not build reports no coverage. The delete/port/keep triage is now stated where 3b begins. (Measured in round seven: **5,834 lines** across 20 files.)

### Round seven — external review

A different model reviewed both documents against two stated principles: prefer re-downloading to defensive machinery, and one owner per piece of state. Five architectural findings plus a test triage matrix. Every file and test name it cited was verified present in the tree before anything was applied.

23. **S7's mtime half is deleted, not merely narrowed** — round six's one open question, now decided (escalation 5, spec §3.4). What settles it is the *response* to a mismatch, not the stamp. Today a mismatch falls through to `recompute()` and costs one read; with `recompute` deleted the only response left is discard-and-refetch, so the identical stamp costs the whole file. And an mtime moves without a byte moving — a restore, a copy, a touch — where a size shortfall cannot. `ModTimeNs` goes from `SyncTarget.Stat`, `ResumeResult`, and the barrier and resumer plumbing; `FileWriter.Stat`'s second return has exactly one consumer (`assembler/synctarget.go:543`) and goes with it.
24. **The change retires a second writer to the durability record, and neither document claimed it.** `Resumer.writeBack` calls `exts.Commit` (`resume.go:274`), and its own doc describes committing "a resume's own answer as the file's Class B record". So the record has two writers today; after Step 12 the barrier is the only one and the resume can only delete. Under Rule 2 that is the strongest result in the whole change.
25. **The `failed_articles` batch delete needs its scope stated, not just its granularity.** `ClearAllEmitted` loops every article of every **resident** job, which is what makes a per-job batch equivalent to the per-article clearing it replaces. A delete swept across all jobs would resurrect every non-resident job's permanently-failed articles, and the symptom is a re-download storm rather than an error.
26. **The test matrix needed a fourth verdict.** One verdict per file hides survivors inside condemned files. `finalize_factgap_test.go` was marked "delete entirely" while holding `TestFinalizeFile_PostTruncateStatFaultNamesTheFile`, an R27 storage-fault pin on a path Task 4 keeps. `mutation_gaps_test.go` and `overlap_report_test.go` — 640 lines, the latter the entire #413 suite — were in no category at all. All three are now SPLIT, test by test.
27. **`Σ length` cannot see a hole and an equal-sized overlap.** Found while triaging `TestFinalizeFile_ReportsAnOverlapAboveAPermanentHole` for the port: the overlap adds N to the sum, the hole takes N off the span, and both cancel to `Σ length == stat size`. The old prefix walk compared extents structurally and saw it. Both copies of the table row now read "no evidence of overlap" rather than "cleanly tiled", with the gap documented and bounded — a holed file has more than one row, so §3.5 withholds the CRC on the row count regardless, and par2 repairs both defects anyway. What is lost is a warning on a file already reported incomplete.

Three of the five architectural findings restated the plan back — strict merge (Step 6), completion-only overlap handling (§3.3), batched reversal (Step 10). That is the expected shape when a reviewer argues from principles rather than a diff, and it is cheap to triage. The matrix is where the round did work the plan had not.
