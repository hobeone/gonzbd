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

**This is the whole change.** Every deletion below removes a type from every signature that names it, so none of it can land separately.

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
| `Commit(ctx, jobID, runs []Run) error` | the barrier, both commit sites |
| `ForFile(ctx, jobID, fileIdx) ([]Run, error)` | the resume gate, `recordAssembledCRC` |
| `ForJob(ctx, jobID) ([]Run, error)` | `stall.go`'s seed — **a whole-job read the earlier API sketch omitted** |
| `DeleteJob(ctx, jobID) error` | `deleteJobDurability`, `pruneDurabilityRows` |

### Steps

- [ ] **Step 1: Write the merge tests before the merge.** Two runs abutting in both offset and index merge with the combined CRC; two abutting in offset but not index do not; two abutting in index but not offset do not; **a new run whose index range a stored row already covers is dropped, not inserted**; merging is order-independent.
- [ ] **Step 2: Pin associativity against real bytes.** For a file of N articles, the merged row's CRC must equal `crc32.ChecksumIEEE` of the concatenation. Everything rests on this. **`Combine(a.crc32, b.crc32, b.length)` requires `b.length` to be b's whole run length, not one article's** — strict left-to-right merging only. That is the one arithmetic trap; pin it.
- [ ] **Step 3: Run both; confirm they fail.** Record the messages.
- [ ] **Step 4: Implement `Commit` — and make it idempotent against STORED rows, not just the drained set.** `Confirm` runs only after `AckDurable` (`barrier.go:287`, `:776`), so an ack failure leaves the report unconfirmed while the commit has landed, and the next `Drain` re-delivers (R12). Deduping the drained set against itself does **not** cover this: a re-delivered article whose stored row has since merged *forward* does not collide on the primary key and is inserted as a duplicate — stored `[off=0,len=1000]` covering articles 0–9, plus a fresh `[off=500,len=100]` for article 5, gives `Σ length` 1100 against a 1000-byte file and **a permanent false overlap finding on a healthy file.** Drop any new run whose `[first_art_idx, last_art_idx]` a stored row already covers, before merging.
- [ ] **Step 5: Range-scope `Commit`'s read.** Only rows bracketing the new runs' offset span can merge, and the primary key supports that query. The barrier fires at 64 MiB and an article is ~700 KB, so at most ~90 arrive per cycle; a full-file read is fine at one row and wasteful at twenty thousand.
- [ ] **Step 6: Build runs in the barrier, at BOTH commit sites.** Two drains — `:162` (`Run`) and `:599` (`FinalizeFile`) — and two commits — `:272` and `:769`. Sort the drained set by offset, dedup on `ArtIdx`, group where offset and index both abut, combine each run's CRC, commit inside the existing transaction before the ack. Index-abutment is **sufficient and conservative, not necessary**: byte-abutting runs whose article indices disagree are refused a merge that would still be CRC-correct, which costs rows and never correctness.
- [ ] **Step 7: Replace the truncate bound with `max(offset+length)`, and delete both guards.** Note that deleting `buildExtent` also deletes its `artCount <= 0` guard (`:341-347`) and its `FileLocalOrdinal` A2/R28 failure (`:362-369`). Both are intended; say so where they went.
- [ ] **Step 8: Rewire `recordAssembledCRC` (`app/durability.go:957-980`).** It is the *only* production path carrying the whole-file CRC to par2 — `app.extents.LoadFile` → `HasPrefixCRC` guard → `queue.SetFileCRC32` → `par2.VerifyCRCs` — and no earlier draft named it. Reimplement as: **publish `crc32` only when one row has `offset == 0` and `length == the file's size`.** That preserves the R23 semantic its current comment states — *"a file with a permanently failed article has a prefix that stops at the hole, and recording that as the file's CRC would report a mismatch against par2 for a file that is merely incomplete"* — because a holed file keeps more than one row. **Pin it red-green;** this is the claim spec §3.5 rests on.
- [ ] **Step 9: Rewire `stall.go`'s seed.** `seedFromCommittedExtents` (`:453-467`) calls `app.extents.Load` and `queue.SeedFromExtents`, both deleted. Its doc says the seed is what stops a job that finalized a file during a stall from re-fetching it, so the behaviour must survive: `ForJob` plus whatever replaces `SeedFromExtents`.
- [ ] **Step 10: Reconcile `failed_articles`' THREE reversal sites.** Writing is one site (`AckPermanentFailure`, `workset.go:94`). Reversal is not: `resetForReload` (`progress.go:767`) **and** `Job.ResetForRetry` (`job.go:812`), the latter reached from both `Queue.Retry` (`queue.go:658`) and `RetryHistoryJob`. Today these stay consistent because `articles_done` is re-serialised wholesale on the next store update; a separate table has no wholesale rewrite, so stale rows survive a retry and **the next restart marks the retry's re-fetched articles failed.** Batch the delete per job, never per article.
- [ ] **Step 11: Trim `ResumeResult`.** `Resume`'s **return type changes** — `VerifiedTo`, `PrefixCRC`, `HasPrefixCRC`, `BytesDurable` and the `Durable Bitmap` all go (`resume.go:23-56`). An earlier draft claimed the signature was unchanged, in two places; only the parameter list is. `fileResumer` (`resume_startup.go:27`) and `resumeJobFiles`'s construction (`:325-330`) both change, as does its `recomputed` counter.
- [ ] **Step 12: Reduce `Resumer` to the `stat` gate.** `recompute`, `verifyRegions` and `writeBack` go; `Resume` compares `stat(path).size >= max(offset+length)`, discards the file's rows on a shortfall, and returns the resume set. **The sweep runs inside `Start` before dispatch** (`resume_startup.go:52-56`), which is what stops the assembler re-creating and pre-allocating a deleted partial so the gate passes over zeros — a property of the sweep's placement, not of the gate. Say so where the gate lives. Log every discard at `Warn`.
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
- [ ] **Step 17: Check `p.done`/`p.emitted` sizing** — `progress.go:605` panics on a mismatch and `Done`'s length was the source. Name the new one.
- [ ] **Step 18: Run everything, including the crash suite. Commit** — `refactor(durability): replace two per-article records with durable runs`. Closes #421 and #389.

---

## Task 4: Check the recorded lengths against the file at completion

**Files:** `internal/durability/barrier.go` (`FinalizeFile`). Test: `internal/durability/barrier_test.go`.

| | Meaning |
|---|---|
| `Σ length > stat size` | **definite overlap** — articles wrote over each other |
| `Σ length == stat size` | complete and cleanly tiled |
| `Σ length < stat size` | articles missing or failed — ordinary, not a finding |

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
- [ ] **Step 2: Rewrite the Class A/B table, §4, and the resume section.** R1 and R2 are deleted; **S4 is inverted**, not amended — say so, because a reader who knows the old contract will assume a recomputation still wins.
- [ ] **Step 3: Grep every falsified literal from the repository root.**

```bash
git grep -n 'INSERT OR IGNORE'; git grep -n 'R1\b'; git grep -n 'R2\b'
git grep -n 'asserts nothing about presence'; git grep -n 'Class A'; git grep -n 'Class B'
git grep -n 'ArticleFact'; git grep -n 'FileExtent'; git grep -n 'verifiedPrefix'
git grep -n 'PrefixCRC'; git grep -n 'recordedExtent'; git grep -n 'articles_done'
git grep -n 'articles_done' -- test/
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
