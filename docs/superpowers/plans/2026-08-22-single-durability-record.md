# Single Durability Record — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make one post-`fsync` record the sole description of a downloaded article, and the sole record of whether that article resolved at all.

**Architecture:** `WrittenArticle` gains the decoded CRC and travels decoder → `WriteRequest` → `FileWriter` → `Drain`. The barrier writes the record inside the transaction that already commits the extent, at both commit sites, under last-write-wins. The decode-time append goes, both reconciliation guards go, and the per-article `articles_done` blob goes — a failed article becomes a tombstone row instead.

**Tech Stack:** Go 1.26.6, SQLite (`modernc.org/sqlite`), `log/slog`.

**Spec:** `docs/superpowers/specs/2026-08-22-single-durability-record-design.md`

> **Revision note.** Four validation rounds produced twenty blocking findings against earlier drafts; the design survived all four, the task list did not. A subsequent end-state review then found the change had been scoped too *narrowly* — correctness review converges on the plan as written and has no mechanism for noticing the target was set too low. Two further moves were added. All of it is recorded under *Corrections applied*, because the mistakes are the reusable part.

## Execution order

**1 → 2 → 3 → 4 → 5 → 6 → 7 → 8 → 9**, in file order.

Stated once, here, and nowhere else. An earlier draft expressed the ordering constraint in two places that disagreed — the same "one constraint, N places" shape that caused the worst finding of every review round, applied to the fix for it. Tasks are now numbered in the order they run.

## Global Constraints

- **No backwards compatibility.** Persisted state an earlier build wrote may be assumed to satisfy the invariants this change introduces. No drain period, no dual-read path. The one carve-out is a security invariant, and spec §3.1 argues it does not apply.
- **One schema change, in Task 8.** Task 8 adds a `failed` discriminator to the records table and needs **one new goose migration**. Everything before Task 8 is schema-neutral. Never edit `001_initial.sql`; a falsified claim there is superseded by the new migration's comment block.
- **Every commit builds and passes.** Go will not let an interface change land apart from its consumers, so several tasks are larger than they look. That is forced, not chosen.
- **Red-green is observed.** Each failing test is run against unpatched code with `-count=1` and its message recorded in the commit body. A cached `ok` is not an observation.
- **Never `git stash`.** Restore from your own copy.
- Gates before every commit: `go fix ./...`, `goimports -w .`, `go vet ./...`, `go test -race ./...`, `golangci-lint run ./...`, `./scripts/run_tests.sh`, `go run ./scripts/check_dup_comments`, `go run ./scripts/check_review_banner`.
- **`go test -tags=crash -timeout=20m ./test/crash/` is a gate for this change specifically.** `run_tests.sh:140-148` deliberately excludes it, so it is invisible to every gate above — and `test/crash/harness.go:983` carries an *independent reimplementation* of the `articles_done` format that `t.Fatalf`s on a length mismatch (`:997`), with the suite's central invariant built on `f.Done[i] && !f.Failed[i]` (`crash_test.go:78,112`). Task 8 deletes that format outright, so this suite must be run at Tasks 5, 8 and 9, and again at the end.

## Escalations this plan carries

Three, each already ruled on, listed so no reviewer has to infer them:

1. **A second writer to an append-only store** — `INSERT OR IGNORE` becomes last-write-wins (Task 4). Spec §3.1 argues R1's threat model does not survive the move.
2. **A cross-package interface change** — `internal/queue` reads the records table, which `internal/durability` owns (Task 8).
3. **A persistence-format change** — Task 8's migration, and the deletion of `articles_done` from both `job_files` and `history_job_files`.

---

## File Structure

| File | Responsibility after this change |
|---|---|
| `internal/durability/synctarget.go` | `WrittenArticle` gains `CRC32`. Still the pre-`fsync` type; the barrier is the only converter. |
| `internal/durability/fact.go` | `DurableArticle` — the persisted record, with a `Failed` discriminator after Task 8. |
| `internal/durability/store_sqlite.go` (new, absorbing `factlog_sqlite.go` + `extentstore_sqlite.go`) | One store committing records and extents in one transaction. |
| `internal/durability/barrier.go` | The union walk; both commit sites; both guards gone by Task 7. |
| `internal/durability/resume.go` | Deletes what it cannot verify, rather than leaving it. |
| `internal/assembler/` | `WriteRequest.CRC32`; the writer retains it until `Drain`. |
| `internal/app/pipeline.go` | `appendArticleFacts` and its call site deleted. |
| `internal/queue/` | `articles_done` gone; resolution derived from the records. |
| `internal/app/app.go`, `internal/history/repository.go` | The retry keeps its records, gated on manifest alignment. |

---

## Task 1: Carry the CRC to `Drain`

**Files:** `internal/durability/synctarget.go`, `internal/assembler/assembler.go`, `internal/assembler/filewriter.go`, `internal/assembler/writecache.go`, `internal/app/pipeline.go`. Test: `internal/assembler/filewriter_test.go`.

**Interfaces produced:** `WrittenArticle.CRC32 uint32`, `WriteRequest.CRC32 uint32`.

**Established, not assumed:** `bufferedArticle` (`writecache.go:98`) and `runPart` (`:123`) already carry offset, id and length. Add `crc32` to both and to `noteWritten`. No new map.

- [ ] **Step 1: Read `filewriter.go:602`'s `Accept` signature before writing the test.** It is `Accept(id articleID, off int64, data []byte) error` — not a `WriteRequest`, and it returns an error. (An earlier draft of this very step, written to warn about the signature, dropped the return value from it.)
- [ ] **Step 2: Write the failing test** — a written article's CRC survives to `Drain`.
- [ ] **Step 3: Run it; confirm it fails for the right reason.** The failure must name `CRC32`, not `Accept`. Record the message.
- [ ] **Step 4: Thread the field** through `WriteRequest`, `Accept`, `bufferedArticle`, `runPart`, `noteWritten`, `Drain`, and `pipeline.go`'s request construction.
- [ ] **Step 5: Run the test; expect PASS.** Then `go test -race ./internal/assembler/ ./internal/durability/ ./internal/app/`.
- [ ] **Step 6: Commit** — `feat(durability): carry the decoded CRC through to the drain report`.

---

## Task 2: One store, one transaction

**Files:** create `internal/durability/store_sqlite.go`; delete `factlog_sqlite.go` and `extentstore_sqlite.go`; modify `fact.go`, `extent.go:145-157` (where `ExtentStore` declares `Commit`), `resume.go` (`writeBack`), `internal/app/durability.go`, `internal/app/app.go` (**both** `NewSQLiteFactLog` and `NewSQLiteExtentStore` wiring), `internal/queue/sqlite_store.go` (`pruneDurabilityRows` `:1294-1315`), plus every fake and bench.

**Interfaces produced:** `Commit(ctx, jobID string, recs []DurableArticle, exts []FileExtent) error`.

- [ ] **Step 1: Define `DurableArticle` here, as a new type.** Do not mint it as `ArticleFact` and rename later. Keep `ArticleFact` alive alongside it until Task 4 deletes its last producer.
- [ ] **Step 2: Write a rollback test that actually tests rollback.** A `VerifiedTo: -1` extent is rejected *before* `BeginTx` (`extentstore_sqlite.go:34-38`), so it cannot distinguish pre-validation from rollback. Force a failure inside the transaction instead.
- [ ] **Step 3: Run it; confirm it fails.** Record the message.
- [ ] **Step 4: Implement.** Validate every extent before `BeginTx`. Keep `INSERT OR IGNORE` in this task — the switch belongs to Task 4.
- [ ] **Step 5: Guard the empty-extent early return.** The current `Commit` returns nil on `len(exts) == 0`; the merged one must not drop records when a cycle produces records but no extents. Test that shape.
- [ ] **Step 6: Update `deleteJobDurability` and `pruneDurabilityRows` prose only.** Both tables still exist and `pruneDurabilityRows` still needs two `DELETE`s. One *store* and one *writer* replace two, not one table.
- [ ] **Step 7: `go test -race ./...`. Commit** — `refactor(durability): commit records and extents in one transaction`.

---

## Task 3: The barrier writes the record — at both commit sites

**Files:** `internal/durability/barrier.go`. Test: `internal/durability/barrier_test.go`.

There are **two** drains — `:162` (`Run`) and `:599` (`FinalizeFile`) — and **two** commits — `:272` and `:769`. `FinalizeFile` loads `facts` at `:629` then runs `durableExtent` (`:640`), `recordedExtent` (`:671`) and the `unrecorded > 0` guard (`:701`) *before* committing at `:769`. Wire the record write into one site only and `unrecorded > 0` fires on every finalize, `bound = 0`, and no file is ever truncated.

- [ ] **Step 1: Read `Run`'s signature.** `Run(ctx, jobID string, t SyncTarget)`.
- [ ] **Step 2: Write two failing tests.** (a) A cycle writes records matching exactly the drained set, only after the commit. (b) **A finalize still truncates** — a file whose tail articles drain in the finalize itself reaches the same bound it reaches today. (b) is the more important.
- [ ] **Step 3: Run both; confirm both fail.** Record both messages.
- [ ] **Step 4: Implement the union — it is not a concatenation.** Four properties, each failing silently if missed:

  **(a) Dedup, keyed on `ArtIdx`, drained wins.** The sets overlap by construction: `Confirm` runs only after `AckDurable` (`:287`, `:776`), so an ack failure leaves the report unconfirmed while the commit carrying the records has landed. The next `Drain` re-delivers (R12), and `ForFile` returns them too. A duplicate satisfies `verifiedPrefix`'s `fact.Offset < prefix && consumed > 0` exactly — **raising a false overlap anomaly to the user** and freezing `VerifiedTo`, leaving `consumedAll == false` and no whole-file CRC.

  **(b) Sort by offset after merging.** `verifiedPrefix` documents that `ForFile` returns offset-ordered rows so it needs no sort. The drained side is *write*-ordered (`noteWritten` appends in write order, `Drain` returns `reported ++ written`). Merging without re-sorting stalls the prefix at the first out-of-order element, on every barrier.

  **(c) Per-file, and restricted to `built`.** `Run` loads `facts` inside phase 3's per-file loop (`:241`), and phase 3 drops closed files at `:254`. `FinalizeFile`'s `written` needs no analogue — one file, and every dropping path returns before `:769`.

  **(d) `durableAt` is the fifth consumer, and it is index-parallel.** `durableAt` (`:555`) returns a closure indexing `facts[i]`, and is what `verifiedPrefix` and `overlapAnywhere` are *given*. Build the union at `:241`/`:629` and pass it through `buildExtent`'s `facts` parameter — that parameter is the carrier, and `:389` is reached by both callers. Wiring it at `:389` alone leaves `FinalizeFile`'s own `durableExtent(facts,…)` at `:640` and `recordedExtent(facts,…)` at `:671` on the raw slice.

  Then pass the union to all five consumers and widen **both** commit calls. Delete the comment at `:383-387` asserting the barrier does not write facts.

  Two things the dedup is *not* for, so the justification does not drift: `durableExtent` is a `max` and is order- and duplicate-insensitive; `recordedExtent`'s `missing++` cannot double-count a drained duplicate, because `buildExtent` set that article's bit at `:377` so it lands in `covered`. `buildExtent` does not double-count `BytesDurable` either (`:376`), but it does append a duplicate `ArtIdx` to `acked` at `:380` — confirm `AckDurable` is idempotent over a repeated index.

- [ ] **Step 5: Run both; expect PASS.**
- [ ] **Step 6: Mutation-check the union at each of the five consumers separately.** Neuter one at a time; a test must fail each time. One test covering one consumer says nothing about the other four.
- [ ] **Step 7: Commit** — `feat(durability): write the article record inside the barrier's commit`.

---

## Task 4: Delete the decode-time append, and switch to last-write-wins

**Files:** `internal/app/pipeline.go`, `internal/app/durability.go` (`appendArticleFacts` `:1164-1170`), `internal/durability/store_sqlite.go`, `internal/durability/fact.go` — **plus every consumer of the type name**: `prefix.go`, `barrier.go`, `resume.go`, `postanomaly.go:62`, `extent.go`, and roughly twenty `_test.go` files across four packages. Deleting a type deletes it from every signature that names it.

**The two halves must land together.** Between them the decode-time append would be live *and* last-write-wins, so a redelivery's pre-write, unvalidated offset would overwrite a post-`fsync` record — `offsetOutOfRange` only rejects it afterwards. Splitting them leaves two commits where #421 is strictly worse than at `ae865fec`.

- [ ] **Step 1: Establish the test seam.** `newScenarioHarness` is in package `app_test` and cannot reach the DB, so the #421 pin must be an **internal** test in package `app`, or the harness must gain an accessor. Decide and note which.
- [ ] **Step 2: Establish whether the mock NNTP server can emit a chosen `=ypart begin=`.** `nntptest` does not expose this. If it cannot, drive `handleSuccessResult` directly with a decode result carrying the bogus offset.
- [ ] **Step 3: Write the failing test** — an article rejected by `offsetOutOfRange` leaves no record. Offset `1_099_511_627_777` decodes cleanly (`decoder.go:65` caps at `1 << 48`) and is rejected by the `ExpectedSize + ExpectedSize/8` bound.
- [ ] **Step 4: Run it; confirm it fails** with the bogus offset recorded. **The load-bearing red check of the change.** Record the message verbatim.
- [ ] **Step 5: Delete `appendArticleFacts`, its call site, the pipeline's `FactLog` field, `ArticleFact`, and `Append`.** In the same commit, switch the record insert to `INSERT OR REPLACE`.
- [ ] **Step 6: `go test -race ./...`. Commit** — `fix(durability): stop recording an article the assembler has not accepted`. Closes #421 and #389.

---

## Task 5: The resumer deletes what it cannot verify

**Files:** `internal/durability/resume.go` (`verifyRegions` `:340-380`), `internal/durability/store_sqlite.go` (a scoped delete). Test: `internal/durability/resume_test.go`, plus the crash suite.

Today `verifyRegions` `continue`s past a region that is out of range or whose CRC mismatches, **leaving the record in place**. That is the sole remaining producer of *recorded but not durable*, and it is why `missing > 0` outlives the rest of this change.

**Why deleting is correct, not merely convenient.** An unverifiable record describes bytes that are not there — the region falls outside the file, or the bytes differ from what the record claims. Preserving a truncate bound over them protects garbage. Where the region *was* overwritten by an overlapping neighbour, that neighbour's own record covers the bytes, so the bound does not drop.

**The cost, stated plainly: this makes the startup sweep destructive.** `Resumer` currently never deletes a record; it only writes extents back. After this task a verification bug deletes real records. The blast radius is bounded — a deleted record returns its article to Outstanding, the file cannot finalize until it resolves, and the cost is a re-fetch rather than data — but the risk profile of the startup path genuinely changes, and that is the price of Task 7.

- [ ] **Step 1: Write the failing test** — a record whose region fails CRC verification is gone from the store after a resume, and its article is Outstanding.
- [ ] **Step 2: Run it; confirm it fails** with the record still present. Record the message.
- [ ] **Step 3: Implement.** Collect the unverifiable records during the walk and delete them in **one** scoped statement per file, not per article. Log the count at `Warn` — silent deletion of durability state is not acceptable even when correct.
- [ ] **Step 4: Write the test that bounds the blast radius** — a record whose region verifies is **never** deleted, including when a sibling in the same file is.
- [ ] **Step 5: Run the crash suite.** `go test -tags=crash -timeout=20m ./test/crash/`. This task changes what a restart does to the durability store, which is exactly what that suite exists to check.
- [ ] **Step 6: Commit** — `fix(durability): delete records the resume sweep cannot verify`.

---

## Task 6: The retry path — #422, the alignment gate, and the second `done` source

**Files:** `internal/app/app.go` (`RetryHistoryJob`), `internal/history/repository.go` (`Delete`), `internal/queue/sqlite_store.go` (`RestoreRetryProgress` `:1030`), `internal/app/job_finalizer.go`. Test: `internal/app/retry_durability_internal_test.go`.

**These are one task by decision.** #422 and the second `done` source are separate defects, but both live in this path and Task 8 makes the second reachable.

- [ ] **Step 1: Write the failing test** — a retry keeps its durability records. Without them `durableExtent` bounds the truncate to the re-fetched articles alone and the partial is destroyed silently.
- [ ] **Step 2: Run it; confirm it fails** with zero records. Record the message.
- [ ] **Step 3: Fix the deletion.** Give `Delete` a variant omitting the durability rows and call it from `RetryHistoryJob`. **Move the delete above `queue.Add` at `app.go:1849`**, so records the re-enqueued job appends before the delete at `:1858` are not caught by it.
- [ ] **Step 4: Fix the two false comments inside `RetryHistoryJob`.** `app.go:1841-1842` claims "its Class A facts survive with it (`Append` is `INSERT OR IGNORE` …)" seventeen lines above the `Delete` that removes them. `app.go:1852-1857` justifies `Delete` by enumerating what goes and what stays and omits the durability rows entirely. **Both are false at `ae865fec`.** Rewrite each; do not inherit either framing.
- [ ] **Step 5: Gate the retention on manifest alignment — the gate must act.** `retainedMatchesManifest` (`sqlite_store.go:1118`) is the right predicate: it compares `NumFiles`, index order and per-file article count, and because `art_idx` derives from cumulative `FileRange`, a shape match implies identical numbering. But on mismatch `RestoreRetryProgress` only declines the bitmap overlay and returns `false` (`:1041-1046`) — **it deletes nothing.** Naming the check narrows nothing.

  The seam exists: `applied` is returned at `app.go:1831`, above the `queue.Add` at `:1849`. Hoist it out of its `if store := …` block and branch the deletion — **`applied == false` takes the full `Delete` and the durability rows go; only `applied == true` takes Step 3's preserving variant.**

  Two consequences to write down rather than discover: `applied == false` also covers `len(retained) == 0` and `job.manifest == nil`, so those discard the rows too; and the shape check inherits the existing "same NZB bytes re-parse deterministically" assumption, so it does not close a segment reordering *within* a file. That exposure is pre-existing for the bitmap and is not widened.

- [ ] **Step 6: Reconcile the second `done` source against the CURRENT format.** `history_job_files.articles_done` feeds `RestoreRetryProgress` → `decodeArticlesDone` (`:1063`). Task 8 deletes that column; this task must not leave it authoritative in the meantime.
- [ ] **Step 7: Reconcile the two ownership comments** — `job_finalizer.go:104-110` says a failed job's rows are kept for the retry; `repository.go:348-357` says the history entry owns them. One must say what owns them now.
- [ ] **Step 8: `go test -race ./internal/app/ ./internal/history/ ./internal/queue/`. Commit** — `fix(app): stop the retry deleting the durability rows it needs`. Closes #422.

---

## Task 7: Delete both reconciliation guards

**Files:** `internal/durability/barrier.go` — the `missing > 0` and `unrecorded > 0` blocks, and `recordedExtent` entirely. Test: `internal/durability/barrier_test.go`.

**Both guards die, but only because Tasks 5 and 6 landed first.** An earlier draft kept `missing > 0` on the grounds that the resumer leaves unverifiable records behind. Task 5 stops it doing that; Task 6's alignment gate stops a renumbered retry producing the same state. With both, every producer of *recorded but not durable* is closed:

| Producer of `missing > 0` | Closed by |
|---|---|
| Resumer leaves a CRC-failed record | Task 5 |
| A renumbered retry keeps misaligned records | Task 6 Step 5 |
| Offset collision / displaced article | Task 4 — a rejected article never gets a record |
| Retry of an interrupted finalize | Task 3 — record and bit now commit together |

- [ ] **Step 1: Prove each row of that table before deleting anything.** If any producer survives, **stop** — keep exactly that guard and correct spec §4 rather than the code. Paths already checked and closed: `pruneDurabilityRows` (`sqlite_store.go:1294`) and `history.Repository.Delete` (`repository.go:347-364`) drop both tables together, so records cannot vanish under surviving extents; `Resumer.recompute` derives the bitmap *from* the records, so `writeBack` cannot commit a bit with no record; `SeedFromExtents` writes the queue's bits, not the durability bitmap.
- [ ] **Step 2: Delete both blocks and `recordedExtent`.** `durableExtent` becomes the only bound.
- [ ] **Step 3: `go test -race ./internal/durability/`.** Any test that fails was pinning a guard; re-read each to decide whether it pinned the guard or the property behind it.
- [ ] **Step 4: Commit** — `refactor(durability): delete the guards a single record makes unreachable`.

---

## Task 8: Records own resolution — tombstones replace `articles_done`

**Files:** new migration in `internal/history/migrations/`; `internal/durability/fact.go`, `store_sqlite.go`; `internal/queue/sqlite_store.go` (`encodeArticlesDone` `:116`, `decodeArticleFlags` `:157`, `decodeArticlesDone` `:192`, `RestoreJobProgress`'s `qFiles` `:503`, `ArticleCountsByJob` `:655`, `MoveToHistory`'s `qRetain` `:933-934`), `internal/queue/persistence.go` (`newJobProgressSized` `:130`, its loop `:150-183`), `internal/queue/progress.go` (`resetForReload` `:770-779`), `test/crash/harness.go` (`decodeArticlesDone` `:983`), `test/crash/crash_test.go`.

**This is the schema change, and the one new goose migration.** A permanently-failed article gets a **tombstone row** — same `(job_id, art_idx, file_idx)` key, with a `failed` discriminator and no meaningful offset, length or CRC. Then:

```
done(art)       ==  a non-tombstone record exists
failed(art)     ==  a tombstone record exists
unfinished(art) ==  no record of either kind exists
```

and `articles_done` disappears from `job_files` **and** `history_job_files`, along with its shared-column length check, its second copy, and its independent reimplementation in the crash harness.

**Do not use a magic `Length = -1` as the discriminator.** A sentinel in a field that has a valid domain is exactly the "type with a valid zero used as a key" smell; add the column.

**The consumer is `persistence.go`, not `sqlite_store.go`.** `ArticleCountsByJob` only decodes; `newJobProgressSized` applies the bits, and its loop reads `if i >= len(f.Done) || !f.Done[i] { continue }` with `f.Failed[i]` applied **only inside** that branch. Any change that leaves `Done` empty makes every article of every non-resident job read as Pending.

- [ ] **Step 1: Solve the boot cost before writing code.** `ArticleCountsByJob` derives Pending for **every non-resident job at startup**, without a manifest (`:648-651`). A naive derivation is one records scan per queued job. Establish a cheaper shape — a single grouped query across all queued jobs — and record it. **If none is cheap enough, stop and re-scope.**
- [ ] **Step 2: Write the migration.** Add the discriminator. Its comment block must supersede `001_initial.sql:150-152` and `:329`, which claim `articles_done` is "the authoritative record" and that "nothing a retry needs is lost" — both falsified. This is the only place those corrections can live.
- [ ] **Step 3: Write the failing test** — reload derives resolution from records alone, without stranding failed articles. Seed a 3-article job: article 0 with a record, article 1 with a tombstone, article 2 with neither. Assert `CountUnfinishedArticles() == 1`.
- [ ] **Step 4: Run it; confirm it fails.** Record the message.
- [ ] **Step 5: Implement.** Write tombstones where `AckPermanentFailure` resolves an article. Rebuild both bitmaps at load. Delete `encodeArticlesDone`, `decodeArticlesDone`, `decodeArticleFlags`, the column from both tables, and the crash harness's copy.
- [ ] **Step 6: Handle `resetForReload`'s new cost.** It clears `failed` and adjusts `failedBytes` for every failed article, at every process start and every reload (`progress.go:770-779`) — currently pure in-memory. Under tombstones it must also delete those rows. **Batch it: one scoped delete per job, never per article.** `failedBytes` still comes from `m.ArticleBytes(i)`, and `resetForReload` already takes the manifest, so the tombstone need not carry bytes.
- [ ] **Step 7: Mutation-check the tombstone clause.** Neuter the "a tombstone counts as resolved" branch; confirm Step 3's test fails. If it passes, the file-completion regression is live and untested.
- [ ] **Step 8: Check `p.done`/`p.emitted` sizing** — `progress.go:605` panics on a mismatch and `Done`'s length was the source. Name the new one.
- [ ] **Step 9: Check `manifest_gate_test.go`'s allow-list.** Keyed on method names, and it fails on **stale** entries as well as missing ones.
- [ ] **Step 10: Run the crash suite.** It loses a whole helper here; its invariant must be re-expressed against the records.
- [ ] **Step 11: `go test -race ./...`. Commit** — `refactor(queue): let the durability records own article resolution`.

---

## Task 9: Contract and comment sweep

**Files:** `docs/durability-contract.md`, `docs/ARCHITECTURE.md`, `docs/queue-lifecycle.md`, `docs/article-validation-contract.md`, `internal/durability/proof.go`, `internal/queue/workset.go`, plus what the greps find.

- [ ] **Step 1: Read `docs/durability-contract.md` and `docs/ARCHITECTURE.md` in full.** Not grep — the claims that survive a grep are the ones restated in prose sharing no token with the code.
- [ ] **Step 2: Rewrite the Class A/B table, §4, and the resume section** against the spec's invariant table. R1 and R2 are deleted, not amended.
- [ ] **Step 3: Grep every falsified literal from the repository root.**

```bash
git grep -n 'INSERT OR IGNORE'
git grep -n 'R1\b'; git grep -n 'R2\b'
git grep -n 'asserts nothing about presence'
git grep -n 'ArticleFact'; git grep -n 'appendArticleFacts'
git grep -n 'recordedExtent'; git grep -n 'unrecorded'
git grep -n 'articles_done'; git grep -n 'at decode'
```

- [ ] **Step 4: Fix the stale claim already in the tree.** `progress.go:790-794` says the done bit "is set only once the bytes have reached WriteAt (#355)", contradicting `statusinfo.go:200-207` **today** and doubly wrong after Task 8.
- [ ] **Step 5: Re-read `proof.go` and `workset.go`.** Their docs assert the barrier is the only `done` authority — still true, different mechanism.
- [ ] **Step 6: Run `pr-review-toolkit:comment-analyzer`** over the cumulative branch diff, once, on the last round.
- [ ] **Step 7: Run both whole-repo gates and the crash suite. Commit** — `docs(durability): restate the contract for a single post-fsync record`.

---

## Inconclusive / Deferred items

1. **Coalesced-run partial writes.** `flushRun` fails every article in a run on any write error, and `WriteAt` can return `io.ErrShortWrite` having written leading bytes — bytes on disk with no record.
   *Narrowed:* `writeOne`/`flushRun` discard `n` and fail every part on any error, so the case arises only through the injectable `w.writeAt` seam.
   *Probe:* inject a short write inside `flushRun`; observe whether the leading article's bytes are fully written. **Run this before Task 3.**
   *Branches:* (a) those bytes sit above every good record, so a `max` bound never drops below good content → accept under Standing Design Rule 3; (b) the bound can drop below a good article's bytes → **stop**, the truncate argument is refuted and the design fails.

2. **The boot-time derivation shape** (Task 8 Step 1).
   *Probe:* benchmark `ArticleCountsByJob` against a queue of jobs at 20k articles each.
   *Branches:* (a) a single grouped query is within noise → proceed; (b) it is not → **re-plan trigger**, Task 8 leaves this change.

3. **Whether deleting unverifiable records loses a bound anyone needed** (Task 5).
   *Probe:* construct a file where an unverifiable record holds the top offset and its article then permanently fails; confirm the resulting truncate destroys only bytes the record misdescribed.
   *Branches:* (a) only misdescribed bytes are lost → proceed; (b) a good sibling's bytes are lost → **stop**, Task 5 is wrong and Task 7 reverts to deleting `unrecorded` alone.

### Answered from the code, and no longer open

- **Where the CRC lives between `Accept` and `Drain`** — `bufferedArticle` and `runPart` already carry offset, id and length. Add a field; no new map.
- **`retryFinalize` against an evicted job** — it returns before `finalizeCompletedFile` when the handle is absent or `syncTargetFor(jobID) == nil` (`stall.go:504,514,528`), so an evicted job never reaches the record write.
- **Other pre-barrier readers of the records** — `ForFile` has exactly three callers: `barrier.go:241`, `:629`, `resume.go:281`.
- **`SeedFromExtents`/`ReplaceFromResume`** — they install `done` from a CRC-verified extent bitmap, which a raw record scan does not reproduce. After Task 5 the records are themselves CRC-verified at resume, so the distinction collapses; Task 8 should delete them or say why not.

---

## Corrections applied

The plan was wrong in twenty ways across four review rounds, and then wrong once more about its own scope. Recorded because the mistakes are the reusable part.

### Round one

1. **Both commit sites, and all four walks.** The first draft wired the record write into `Run`'s phase 4 only. `FinalizeFile` has its own drain and its own commit, and three more walks between them — as written, `unrecorded > 0` would have fired on every finalize and no file would ever have been truncated.
2. **Task 6 targeted dead code.** `jobProgressJSON` has no production caller. The real store is a shared `[done][failed]` blob in `job_files.articles_done`.
3. **Last-write-wins moved into Task 4.** Landing it earlier left two commits where a redelivery's unvalidated offset could overwrite a post-`fsync` record.
4. **`DurableArticle` is defined in Task 2, not renamed later.** The first draft used the type two tasks before introducing it.
5. **Test signatures established before tests are written.** `Accept` takes `(id, off, data)`; `Run` takes a `SyncTarget`; the #421 pin cannot live in package `app_test`.
6. **A task deletes the guards.** The spec claimed they become unreachable and no task acted on it.
7. **`BytesDownloaded` was already derived**, so the spec's claim that it "stays persisted" was wrong on the facts.

### Round two

Every finding sat immediately adjacent to a fix round one had just applied.

8. **The union is not a concatenation** — it needs dedup, a re-sort, and per-file scoping. Missing the dedup raises a false overlap anomaly *to the user*; missing the sort costs the CRC on every barrier, silently.
9. **`missing > 0` does not die** — corrected in spec and plan. (Subsequently reopened; see *End-state review*.)
10. **Task 6's consumer is `persistence.go`**, not `sqlite_store.go`. Round one found the right column and still named the wrong function.
11. **Deleting a type deletes it from every signature** — `ArticleFact` appears across five source files and about twenty test files.
12. **Task 2's rollback test pinned nothing** — extents are validated before `BeginTx`.
13. **Task ordering** — one commit existed in which every retry re-downloaded from scratch.
14. **"One table now" was false.** Both tables survive.

### Round three

15. **The crash suite reimplements the blob format, and no gate ran it.** `run_tests.sh` deliberately excludes `-tags=crash`, so the change would have broken the repository's most durability-relevant suite while every listed gate stayed green.
16. **`durableAt` is the fifth union consumer.** Rounds one and two both converged on "four walks".
17. **Task 5's proof is falsified by the retry task** unless the manifest-alignment gate lands first.

Also six line numbers and one signature — including, in the very step warning that `Accept`'s signature must be read before writing a test against it, a copy of that signature with its return value dropped.

### Round four

18. **The plan stated two contradictory execution orders**, having expressed one constraint in two places — the same shape as the worst finding of every prior round, this time in the fix for it. There is now one *Execution order* section and nothing else states an order.
19. **Naming the alignment check narrowed nothing.** On a shape mismatch `RestoreRetryProgress` declines the overlay and *deletes nothing*, so misaligned records would have outlived a renumbering. The gate now acts.
20. **`buildExtent`'s `facts` parameter is the carrier** — wiring the union at `:389` alone leaves `FinalizeFile`'s own two walks on the raw slice.

### End-state review

Four rounds asked whether the plan was *correct*. None asked whether it was *enough* — correctness review converges on the plan as written and cannot notice that the target was set too low. Tracing the end-state flow found the change merged the two *writers* and left the second record's *payload* duplicating the first's.

21. **The resumer is the last producer of "recorded but not durable".** Deleting what it cannot verify closes it, which retires `missing > 0`, `recordedExtent`, and the argument that `SeedFromExtents` must exist — one behaviour change collapsing three justifications. Correction 9 is superseded: both guards die after all. **Cost: the startup sweep becomes destructive**, which it has never been.
22. **`articles_done` need not survive at all.** With a tombstone row for a failed article, the records own resolution outright and the blob goes — with its length check, its `history_job_files` copy, and its independent reimplementation in the crash harness. **Cost: one goose migration, and `resetForReload` gains a batched database write on a path that was pure memory.**

### Still on the table, deliberately out of scope

**`file_extents` is now mostly derived.** A record exists if and only if the article is durable, which makes `Durable`, `BytesDurable`, `VerifiedTo` and `PrefixCRC` caches of a walk the barrier already performs; only `Size` and `ModTimeNs` remain sources of truth. Reducing the table to a per-file stat stamp is the largest remaining deletion and needs its consumers traced and its cost measured first. It is not in this plan.
