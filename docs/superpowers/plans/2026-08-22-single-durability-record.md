# Durable Runs — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** One record, written only after the `fsync` that makes its bytes durable, describing a contiguous **run** of articles rather than a single one.

**Architecture:** The decoder's CRC travels to `Drain`. The barrier groups drained articles into runs that abut in both offset and article index, combines their CRCs, merges them with adjacent stored rows, and commits them in the transaction that already commits durability. `article_facts` and `file_extents` both go, along with the entire contiguity apparatus that existed to re-derive at read time what a run records at write time.

**Tech Stack:** Go 1.26.6, SQLite (`modernc.org/sqlite`), `log/slog`.

**Spec:** `docs/superpowers/specs/2026-08-22-single-durability-record-design.md`

> **Second plan.** The first was written against a per-article record and hardened over four review rounds and twenty blocking findings. Those rounds asked whether it was *correct*; none asked whether the record needed to be per-article at all. It did not. Most of what those rounds fixed — the five-consumer union, its dedup and sort requirements, `durableAt`'s index-parallel closure, the two reconciliation guards and their sequencing — does not exist in this shape. What survives from them is recorded under *Carried forward*, because those findings were about the code, not about the plan.

## Execution order

**1 → 2 → 3 → 4 → 5 → 6 → 7 → 8 → 9**, in file order.

Stated once, here, and nowhere else. An earlier plan expressed one ordering constraint in two places that disagreed.

## Global Constraints

- **No backwards compatibility.** Persisted state an earlier build wrote may be assumed to satisfy the new invariants. No drain period, no dual-read path. The one carve-out is a security invariant, and spec §3.1 argues it does not apply.
- **One migration, in Task 2.** It drops `article_facts` and `file_extents` and creates `durable_runs` and `failed_articles`. Never edit `001_initial.sql`; its two false claims (`:150-152`, `:329` — that `articles_done` is "the authoritative record" and that "nothing a retry needs is lost") are superseded in the new migration's comment block, which is the only place that correction can live.
- **Every commit builds and passes.** Go will not let an interface change land apart from its consumers.
- **Red-green is observed.** Each failing test runs against unpatched code with `-count=1`, and its message goes in the commit body. A cached `ok` is not an observation.
- **Never `git stash`.**
- Gates before every commit: `go fix ./...`, `goimports -w .`, `go vet ./...`, `go test -race ./...`, `golangci-lint run ./...`, `./scripts/run_tests.sh`, `go run ./scripts/check_dup_comments`, `go run ./scripts/check_review_banner`.
- **`go test -tags=crash -timeout=20m ./test/crash/` is a gate for this change.** `run_tests.sh:140-148` deliberately excludes it, so it is invisible to every gate above — and `test/crash/harness.go:983` independently reimplements the `articles_done` format, `t.Fatalf`ing on a length mismatch (`:997`), with the suite's central invariant built on `Done[i] && !Failed[i]` (`crash_test.go:78,112`). Task 8 deletes that format. Run the suite at Tasks 6, 8 and 9.

## Escalations this plan carries

1. **A second writer to a formerly append-only store** — merging is read-modify-write (Task 2). Ruled on.
2. **A cross-package interface change** — `internal/queue` reads `durable_runs`, which `internal/durability` owns (Task 8). Ruled on.
3. **A persistence-format change** — Task 2's migration drops two tables and creates two.
4. **S4 is inverted** (spec §3.4). The record becomes authoritative and startup does no reads. This is a departure from `docs/durability-contract.md` as written, not an elaboration of it.

---

## Task 1: Carry the CRC to `Drain`

**Files:** `internal/durability/synctarget.go`, `internal/assembler/assembler.go`, `filewriter.go`, `writecache.go`, `internal/app/pipeline.go`. Test: `internal/assembler/filewriter_test.go`.

**Established:** `bufferedArticle` (`writecache.go:98`) and `runPart` (`:123`) already carry offset, id and length. Add `crc32` to both and to `noteWritten`. No new map.

- [ ] **Step 1: Read `filewriter.go:602`'s `Accept` signature first.** It is `Accept(id articleID, off int64, data []byte) error` — not a `WriteRequest`, and it returns an error. A test against the wrong signature fails to compile, which demonstrates nothing.
- [ ] **Step 2: Write the failing test** — a written article's CRC reaches `Drain`.
- [ ] **Step 3: Run it; confirm it fails naming `CRC32`, not `Accept`.** Record the message.
- [ ] **Step 4: Thread the field** through `WriteRequest`, `Accept`, `bufferedArticle`, `runPart`, `noteWritten`, `Drain`, and `pipeline.go`'s request construction.
- [ ] **Step 5: `go test -race ./internal/assembler/ ./internal/durability/ ./internal/app/`. Commit** — `feat(durability): carry the decoded CRC through to the drain report`.

---

## Task 2: The run store

**Files:** new migration in `internal/history/migrations/`; create `internal/durability/runs.go` and `runstore_sqlite.go`; delete `factlog_sqlite.go`, `extentstore_sqlite.go`, `fact.go`, `extent.go`; update `internal/app/durability.go` (`deleteJobDurability`), `internal/app/app.go` (both store constructors), `internal/queue/sqlite_store.go` (`pruneDurabilityRows` `:1294-1315`), and every fake and bench.

**Schema:**

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

Two tables, and that is not a Rule 2 violation: they describe different facts with different lifetimes — bytes that reached disk, and articles that never will.

- [ ] **Step 1: Write the migration**, dropping `article_facts` and `file_extents`. Its comment block must supersede `001_initial.sql:150-152` and `:329`.
- [ ] **Step 2: Write the merge tests before the merge.** At minimum: two runs that abut in both offset and index merge into one with the combined CRC; two that abut in offset but **not** index do not merge; two that abut in index but not offset do not merge; an overlapping run does not merge and does not extend; merging is order-independent (insert A then B, and B then A, produce the same row).
- [ ] **Step 3: Run them; confirm they fail.** Record the messages.
- [ ] **Step 4: Implement `Commit(ctx, jobID, runs []Run) error`** — one transaction: load the file's existing rows, insert the new ones, merge adjacent pairs repeatedly, write the result. Merge condition is **both** `a.offset+a.length == b.offset` **and** `a.last_art_idx+1 == b.first_art_idx`. CRC combines as `crc32util.Combine(a.crc32, b.crc32, b.length)`.
- [ ] **Step 5: Pin associativity against the old path.** For a file of N articles, the merged row's CRC must equal `crc32.ChecksumIEEE` of the concatenated bytes. This is the one property the whole design rests on; test it with real bytes, not with combined values compared to each other.
- [ ] **Step 6: Update `deleteJobDurability` and `pruneDurabilityRows`** — two tables still, different two.
- [ ] **Step 7: `go test -race ./...`. Commit** — `feat(durability): add the durable-run store`.

---

## Task 3: The barrier builds and commits runs

**Files:** `internal/durability/barrier.go`; delete `prefix.go` (`verifiedPrefix`, `prefixWalk`, `overlapAnywhere`), and `barrier.go`'s `durableAt`, `durableExtent`, `recordedExtent`, `buildExtent`, and **both** `FinalizeFile` guards. Test: `internal/durability/barrier_test.go`.

There are **two** drains — `:162` (`Run`) and `:599` (`FinalizeFile`) — and **two** commits — `:272` and `:769`. Both take runs. Wiring one leaves every file finalized through the other unrecorded.

- [ ] **Step 1: Read `Run`'s signature.** `Run(ctx, jobID string, t SyncTarget)`.
- [ ] **Step 2: Write two failing tests.** (a) a cycle writes runs matching exactly the drained set, only after the commit; (b) **a finalize still truncates** — a file whose tail articles drain in the finalize itself reaches the correct bound. (b) is the more important.
- [ ] **Step 3: Run both; confirm both fail.** Record both messages.
- [ ] **Step 4: Implement run-building.** Sort the drained set by offset and **dedup on `ArtIdx`**, keeping the drained copy. The sets overlap by construction: `Confirm` runs only after `AckDurable` (`:287`, `:776`), so an ack failure leaves the report unconfirmed while the commit has landed, and the next `Drain` re-delivers (R12). Group into runs where offset and index both abut; combine each run's CRC. Both requirements are now local to this function.
- [ ] **Step 5: Commit runs inside both existing commits**, in the same transaction, before the ack. Phase 4's constraint stands: nothing may be inserted between the commit and the ack.
- [ ] **Step 6: Replace the truncate bound with `max(offset+length)` over the file's rows.** Delete both guards — with one record there is nothing to reconcile.
- [ ] **Step 7: Delete `prefix.go` and the fact-walking helpers.** Confirm with `git grep` that nothing references them.
- [ ] **Step 8: Mutation-check the dedup and the sort separately.** Neuter each; a test must fail for each. They protect different failures.
- [ ] **Step 9: Commit** — `feat(durability): record durable runs from the barrier`.

---

## Task 4: Delete the decode-time append

**Files:** `internal/app/pipeline.go`, `internal/app/durability.go` (`appendArticleFacts` `:1164-1170`). Test: an internal test in package `app`.

- [ ] **Step 1: Establish the test seam.** `newScenarioHarness` is in package `app_test` and cannot reach the DB, so the #421 pin must be an internal test in package `app`, or the harness must gain an accessor. Decide and note which.
- [ ] **Step 2: Establish whether the mock NNTP server can emit a chosen `=ypart begin=`.** `nntptest` does not expose this. If not, drive `handleSuccessResult` directly.
- [ ] **Step 3: Write the failing test** — an article rejected by `offsetOutOfRange` leaves no record. Offset `1_099_511_627_777` decodes cleanly (`decoder.go:65` caps at `1 << 48`) and is rejected by the `ExpectedSize + ExpectedSize/8` bound.
- [ ] **Step 4: Run it; confirm it fails** with the bogus offset recorded. **The load-bearing red check.** Record the message verbatim.
- [ ] **Step 5: Delete `appendArticleFacts`, its call site, and the pipeline's store field.**
- [ ] **Step 6: `go test -race ./...`. Commit** — `fix(durability): stop recording an article the assembler has not accepted`. Closes #421 and #389.

---

## Task 5: Refuse overlapping articles

**Files:** `internal/assembler/assembler.go` (beside `offsetOutOfRange`). Test: `internal/assembler/assembler_test.go`.

Spec §3.3. An article that overlaps an already-written range is refused the way an out-of-range offset is: counted toward `TotalParts`, recorded failed, bytes charged to `failedBytes`, left for par2. This is what keeps `Σ length == stat size` exact.

- [ ] **Step 1: Write the failing test** — an article whose range overlaps a written one is refused, and the file's byte accounting still balances.
- [ ] **Step 2: Run it; confirm it fails** with the overlap written. Record the message.
- [ ] **Step 3: Implement.** Reuse the existing per-file written-range knowledge rather than adding a second source; if none is usable, say so and stop rather than adding one.
- [ ] **Step 4: `go test -race ./internal/assembler/`. Commit** — `fix(assembler): refuse an article that overlaps a written range`.

---

## Task 6: Resume from the record, gated on one `stat`

**Files:** `internal/durability/resume.go` — delete `recompute`, `verifyRegions`, `writeBack`; `internal/app/app.go` (the startup sweep). Test: `internal/durability/resume_test.go`, plus the crash suite.

Spec §3.4. The resume set is the complement of the runs' index ranges, minus `failed_articles`. The only check is `stat(path).size >= max(offset+length)`; a missing or short file discards that file's rows.

- [ ] **Step 1: Write two failing tests.** (a) a job resumes to exactly the articles no run covers; (b) **a deleted partial file discards its rows** rather than reporting most articles complete. (b) is the floor the whole "trust the log" decision rests on.
- [ ] **Step 2: Run both; confirm both fail.** Record both messages.
- [ ] **Step 3: Implement, then delete the verification path.** `recompute`, `verifyRegions` and `writeBack` all go.
- [ ] **Step 4: Log the discard at `Warn`.** Throwing away durability state must never be silent, even when correct.
- [ ] **Step 5: Run the crash suite.** This task changes what a restart does; that suite exists to check exactly this.
- [ ] **Step 6: Commit** — `refactor(durability): resume from the record, gated on a size check`.

---

## Task 7: The retry path — #422

**Files:** `internal/app/app.go` (`RetryHistoryJob`), `internal/history/repository.go` (`Delete`), `internal/queue/sqlite_store.go` (`RestoreRetryProgress` `:1030`), `internal/app/job_finalizer.go`. Test: `internal/app/retry_durability_internal_test.go`.

- [ ] **Step 1: Write the failing test** — a retry keeps its durability rows. Without them the truncate bounds to the re-fetched articles alone and the partial is destroyed silently.
- [ ] **Step 2: Run it; confirm it fails** with zero rows. Record the message.
- [ ] **Step 3: Fix the deletion.** Give `Delete` a variant omitting the durability rows. **Move the delete above `queue.Add` at `app.go:1849`**, so rows the re-enqueued job writes before the delete at `:1858` are not caught by it.
- [ ] **Step 4: Fix the two false comments.** `app.go:1841-1842` claims the facts survive, seventeen lines above the `Delete` that removes them; `:1852-1857` enumerates what goes and what stays and omits them. **Both are false at `ae865fec`.** Rewrite each; inherit neither framing.
- [ ] **Step 5: Gate the retention on manifest alignment, and make the gate act.** `retainedMatchesManifest` (`sqlite_store.go:1118`) is the right predicate — it compares `NumFiles`, index order and per-file article count, and `art_idx` derives from cumulative `FileRange`, so a shape match implies identical numbering. But on mismatch `RestoreRetryProgress` only declines the overlay and returns `false` (`:1041-1046`); **it deletes nothing.** Naming the check narrows nothing. `applied` is returned at `app.go:1831`, above the `queue.Add`: hoist it out of its `if store :=` block and branch — **`applied == false` takes the full `Delete`; only `applied == true` preserves the rows.**

  Two consequences to write down rather than discover: `applied == false` also covers `len(retained) == 0` and `job.manifest == nil`; and the shape check inherits the existing "same NZB bytes re-parse deterministically" assumption, so it does not close a segment reordering *within* a file. That exposure is pre-existing and not widened.
- [ ] **Step 6: Reconcile the two ownership comments** — `job_finalizer.go:104-110` says a failed job's rows are kept for the retry; `repository.go:348-357` says the history entry owns them.
- [ ] **Step 7: `go test -race ./internal/app/ ./internal/history/ ./internal/queue/`. Commit** — `fix(app): stop the retry deleting the durability rows it needs`. Closes #422.

---

## Task 8: The queue derives resolution from the record

**Files:** `internal/queue/sqlite_store.go` (`encodeArticlesDone` `:116`, `decodeArticleFlags` `:157`, `decodeArticlesDone` `:192`, `RestoreJobProgress`'s `qFiles` `:503`, `ArticleCountsByJob` `:655`, `MoveToHistory`'s `qRetain` `:933-934`), `internal/queue/persistence.go` (`newJobProgressSized` `:130`, its loop `:150-183`), `internal/queue/progress.go` (`resetForReload` `:770-779`), `internal/queue/workset.go` (`SeedFromExtents`, `ReplaceFromResume`), `test/crash/harness.go`, `test/crash/crash_test.go`.

```
done(art)       ==  some run's [first_art_idx, last_art_idx] covers it
failed(art)     ==  a failed_articles row exists
unfinished(art) ==  neither
```

`articles_done` disappears from `job_files` **and** `history_job_files`, with its shared-column length check, its second copy, and the crash harness's independent reimplementation.

**The consumer is `persistence.go`, not `sqlite_store.go`.** `ArticleCountsByJob` only decodes; `newJobProgressSized` applies the bits, and its loop reads `if i >= len(f.Done) || !f.Done[i] { continue }` with `f.Failed[i]` applied **only inside** that branch. Any change leaving `Done` empty makes every article of every non-resident job read as Pending.

- [ ] **Step 1: Solve the boot cost before writing code.** `ArticleCountsByJob` derives Pending for **every non-resident job at startup**, without a manifest (`:648-651`). Runs make this much cheaper than per-article rows — a file's coverage is a handful of ranges — but establish the query shape and record it. **If none is cheap enough, stop and re-scope.**
- [ ] **Step 2: Write the failing test** — reload derives resolution from runs alone without stranding failed articles. Seed a job with a run covering articles 0–1, a `failed_articles` row for 2, and nothing for 3. Assert `CountUnfinishedArticles() == 1`.
- [ ] **Step 3: Run it; confirm it fails.** Record the message.
- [ ] **Step 4: Implement.** Write `failed_articles` where `AckPermanentFailure` resolves an article. Rebuild both bitmaps at load. Delete `encodeArticlesDone`, `decodeArticlesDone`, `decodeArticleFlags`, the column from both tables, `SeedFromExtents`, `ReplaceFromResume`, and the crash harness's copy.
- [ ] **Step 5: Handle `resetForReload`'s new cost.** It clears `failed` and adjusts `failedBytes` for every failed article at every process start and reload (`progress.go:770-779`) — currently pure memory. It must now delete `failed_articles` rows. **Batch it: one scoped delete per job, never per article.** `failedBytes` still comes from `m.ArticleBytes(i)`, and the function already takes the manifest.
- [ ] **Step 6: Mutation-check the failed clause.** Neuter "a `failed_articles` row counts as resolved"; confirm Step 2's test fails.
- [ ] **Step 7: Check `p.done`/`p.emitted` sizing** — `progress.go:605` panics on a mismatch and `Done`'s length was the source. Name the new one.
- [ ] **Step 8: Check `manifest_gate_test.go`'s allow-list** — keyed on method names, and it fails on **stale** entries as well as missing ones.
- [ ] **Step 9: Run the crash suite.** It loses a whole helper; its invariant must be re-expressed against the runs.
- [ ] **Step 10: `go test -race ./...`. Commit** — `refactor(queue): derive article resolution from the durable runs`.

---

## Task 9: Contract and comment sweep

**Files:** `docs/durability-contract.md`, `docs/ARCHITECTURE.md`, `docs/queue-lifecycle.md`, `docs/article-validation-contract.md`, `internal/durability/proof.go`, `internal/queue/workset.go`.

- [ ] **Step 1: Read `docs/durability-contract.md` and `docs/ARCHITECTURE.md` in full.** Not grep — the claims that survive a grep are the ones restated in prose sharing no token with the code.
- [ ] **Step 2: Rewrite the Class A/B table, §4, and the resume section.** R1 and R2 are deleted; **S4 is inverted**, not amended — say so explicitly, because a reader who knows the old contract will otherwise assume a recomputation still wins.
- [ ] **Step 3: Grep every falsified literal from the repository root.**

```bash
git grep -n 'INSERT OR IGNORE'; git grep -n 'R1\b'; git grep -n 'R2\b'
git grep -n 'asserts nothing about presence'; git grep -n 'Class A'; git grep -n 'Class B'
git grep -n 'ArticleFact'; git grep -n 'FileExtent'; git grep -n 'verifiedPrefix'
git grep -n 'PrefixCRC'; git grep -n 'recordedExtent'; git grep -n 'articles_done'
git grep -n 'articles_done' -- test/
```

- [ ] **Step 4: Fix the two claims already false in the tree.** `progress.go:790-794` says the done bit "is set only once the bytes have reached WriteAt (#355)", contradicting `statusinfo.go:200-207` **today**. And any comment describing QuickCheck as a par2 bypass — it is filename relocation, it computes its own CRC from disk, and the verification-without-I/O mechanism is `par2.VerifyCRCs`.
- [ ] **Step 5: Re-read `proof.go` and `workset.go`.** Their docs assert the barrier is the only `done` authority — still true, different mechanism.
- [ ] **Step 6: Run `pr-review-toolkit:comment-analyzer`** over the cumulative branch diff, once, on the last round.
- [ ] **Step 7: Run both whole-repo gates and the crash suite. Commit** — `docs(durability): restate the contract for durable runs`.

---

## Inconclusive / Deferred items

1. **How often do runs actually merge?** The run carries an index *range*, so merging requires offset-contiguity and index-contiguity together.

   *Two of the three links are settled without a probe.* `art_idx` order **is** NZB segment-`number` order — `normalizeFileStruct` (`parser.go:502-519`) sorts part numbers and builds `Articles` in that order, and `FileRange` hands out contiguous indices from it. And yEnc part order **is** offset order, by the format: part *k+1* begins where part *k* ended.

   The one open link is whether an NZB's segment `number` equals the yEnc part number in the body. Nothing in an NZB establishes that — **an NZB carries segment numbers and encoded byte counts; the offset lives in the article body** — so it cannot be probed from NZBs at all. An earlier draft of this item said to check offsets "across a sample of real NZBs", which is not a thing that can be done. Our own fixtures are circular: they are generated by our encoder, which numbers parts to match segments by construction.

   *Probe:* instrument the decoder, which already parses `=ypart begin=`. When article `art_idx N` decodes, compare its offset against `N-1`'s `offset+length` and count disagreements. Ten lines, answers the question against real traffic, ships independently of this change.

   *Branches:* (a) they agree normally → runs collapse and a complete file is one row; (b) they often disagree → more rows, and those files never yield a whole-file CRC. **(b) is not a stop and does not block any task.** The truncate bound (`max(offset+length)`) and the integrity check (`Σ length == stat size`) do not reference `art_idx`, so both are unaffected; the only cost is one par2 read for the affected files. This is a question about how often an optimisation fires, not about whether the design holds.

2. **Merge cost per barrier.** Read-modify-write over a file's rows on every checkpoint.
   *Probe:* benchmark `Commit` against a file at 20k articles across 1, 10 and 10,000 rows.
   *Branches:* (a) small → proceed; (b) large at high row counts → the merge needs an index or a bound on rows per commit.

3. **Coalesced-run partial writes.** `flushRun` fails every article in a run on any write error, and `WriteAt` can return `io.ErrShortWrite` having written leading bytes — bytes on disk with no record.
   *Narrowed:* both callers discard `n` and fail every part, so it arises only through the injectable `w.writeAt` seam.
   *Probe:* inject a short write inside `flushRun`; observe whether the leading article's bytes are fully written. **Run before Task 3.**
   *Branches:* (a) those bytes sit above every recorded run and a `max` bound never drops below good content → accept under Standing Design Rule 3; (b) the bound can drop below good bytes → **stop**.

4. **Can `Resumer` be deleted outright,** or does the `stat` gate need a home that survives it? Task 6 assumes the former.

5. **Where the assembler's written-range knowledge lives** (Task 5). Refusing an overlap needs to know what has been written. If no existing per-file structure carries it, adding one is a second source of truth about the same fact and Task 5 should stop rather than introduce it.

### Answered from the code

- **`bufferedArticle`/`runPart` already carry offset, id and length** — Task 1 adds a field, not a map.
- **`retryFinalize` never reaches the record write** — it returns before `finalizeCompletedFile` when the handle is absent or `syncTargetFor(jobID) == nil` (`stall.go:504,514,528`).
- **`crc32util.Combine` is zlib's `crc32_combine`** and is associative, which is what makes runs possible at all.
- **`par2.QuickCheck` computes its own CRC from disk** (`tryMatchCRC32File` takes a path), so it does not consume the assembled CRC. Only `par2.VerifyCRCs` does.

---

## Carried forward

Findings from four review rounds against the previous, per-article plan. Most of what those rounds fixed does not exist in this shape — the five-consumer union, `durableAt`'s index-parallel closure, the guard sequencing. These are the ones that were about the **code** rather than the plan, and they still apply:

1. **Two drains and two commits.** `Run` at `:162`/`:272`, `FinalizeFile` at `:599`/`:769`. Wiring one leaves the other unrecorded — and `FinalizeFile`'s drain carries a file's *tail* articles, so its records are the ones the truncate bound most depends on.
2. **The drained and stored sets overlap.** `Confirm` runs only after `AckDurable`, so an ack failure leaves the report unconfirmed while the commit has landed, and the next `Drain` re-delivers.
3. **The drained set is write-ordered**, not offset-ordered — `noteWritten` appends in write order and `Drain` returns `reported ++ written`.
4. **`articles_done` is the real `done` store**, not `jobProgressJSON`, which has had no production caller since #298. Its consumer is `newJobProgressSized` in `persistence.go`, which applies `Failed[i]` only inside the `Done[i]` branch.
5. **The crash suite reimplements that format** and no standard gate runs it.
6. **`RetryHistoryJob` contains two comments that are false today**, nine lines apart.
7. **A rejected article counts toward `TotalParts`** (`assembler.go:1418-1430`), so a complete file does not imply every article is durable.
8. **`ExpectedSize` is the NZB's encoded byte count**, ~2% above decoded, so the truncate fires on every completed file.
9. **`Accept` is `(id, off, data) error`**; **`Run` takes a `SyncTarget`**; the #421 pin cannot live in package `app_test`.
