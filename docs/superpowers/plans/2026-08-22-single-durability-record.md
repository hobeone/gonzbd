# Single Durability Record — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make one post-`fsync` record the sole description of a downloaded article, so Class A and Class B cannot disagree.

**Architecture:** `WrittenArticle` gains the decoded CRC and travels decoder → `WriteRequest` → `FileWriter` → `Drain`. The barrier writes the record inside the transaction that already commits the extent, at **both** commit sites, under last-write-wins. The decode-time append is deleted, the two reconciliation guards are deleted, and the queue's `done` bit is derived from the records.

**Tech Stack:** Go 1.26.6, SQLite (`modernc.org/sqlite`), `log/slog`.

**Spec:** `docs/superpowers/specs/2026-08-22-single-durability-record-design.md`

> **Revision note.** This plan was rewritten after a validation round produced four blocking findings against its first draft. The design in the spec survived unchanged — its premises P1, P2 and P3 were confirmed against the code — but the task list did not. What changed is recorded under *Corrections applied* at the end, because the first draft's mistakes are the ones most likely to be made again.

## Global Constraints

- **No backwards compatibility.** Persisted state an earlier build wrote may be assumed to satisfy the invariants this change introduces. No drain period, no dual-read path, no migration. The one carve-out is a security invariant, and spec §3.1 argues it does not apply.
- **No schema change, and no new goose migration.** Last-write-wins is `INSERT OR REPLACE` on the existing `article_facts` shape. Re-encoding `job_files.articles_done` is a blob-format change the rule above waives. Do not edit `001_initial.sql`; a falsified claim there is superseded by a later migration's comment block, never corrected in place.
- **Every commit builds and passes.** Go will not let an interface change land apart from its consumers, so several tasks are larger than they look. That is forced, not chosen.
- **Red-green is observed.** Each failing test is run against unpatched code with `-count=1` and its message recorded in the commit body. A cached `ok` is not an observation.
- **Never `git stash`.** Restore from your own copy.
- Gates before every commit: `go fix ./...`, `goimports -w .`, `go vet ./...`, `go test -race ./...`, `golangci-lint run ./...`, `./scripts/run_tests.sh`, `go run ./scripts/check_dup_comments`, `go run ./scripts/check_review_banner`.
- **`go test -tags=crash -timeout=20m ./test/crash/` is a gate for this change specifically.** `run_tests.sh:140-148` deliberately excludes it, so it is invisible to every gate above — and `test/crash/harness.go:983` carries an *independent reimplementation* of the `articles_done` format that `t.Fatalf`s on a length mismatch (`:997`), with the suite's central invariant built on `f.Done[i] && !f.Failed[i]` implying bytes on disk (`crash_test.go:78,112`). This is the most durability-relevant suite in the repository and this is a durability change. Run it at Task 6 and again at the end.

## Escalation this plan carries

Task 6 makes `internal/queue` read `article_facts`, a table `internal/durability` owns. That is a **cross-package interface change** under the Decision Protocol. It is in scope by explicit decision, together with Task 7's reconciliation of the second `done` source. Whoever implements Task 6 must not widen that read beyond what Task 6 states.

---

## Task 1: Carry the CRC to `Drain`

**Files:** `internal/durability/synctarget.go`, `internal/assembler/assembler.go`, `internal/assembler/filewriter.go`, `internal/assembler/writecache.go`, `internal/app/pipeline.go`. Test: `internal/assembler/filewriter_test.go`.

**Interfaces produced:** `WrittenArticle.CRC32 uint32`, `WriteRequest.CRC32 uint32`.

**Established, not assumed:** `bufferedArticle` (`writecache.go:98`) and `runPart` (`:123`) already carry offset, id and length. Add `crc32` to both and to `noteWritten`. **No new map is needed** — the first draft listed this as an open question; it is answered.

- [ ] **Step 1: Read `filewriter.go:602`'s `Accept` signature before writing the test.** It is `Accept(id articleID, off int64, data []byte) error` — *not* a `WriteRequest`, and it returns an error. A test written against the wrong signature fails to compile, which demonstrates nothing. (An earlier draft of this very step, written to warn about the signature, dropped the return value from it.)
- [ ] **Step 2: Write the failing test** — a written article's CRC survives to `Drain`. Assert on `Drain()`'s returned `[]durability.WrittenArticle` that `CRC32` matches what `Accept` was given.
- [ ] **Step 3: Run it; confirm it fails for the right reason.** `go test -count=1 ./internal/assembler/ -run TestDrain_ReportsTheDecodedCRC`. The failure must name `CRC32`, not `Accept`. Record the message.
- [ ] **Step 4: Thread the field** through `WriteRequest`, `Accept`, `bufferedArticle`, `runPart`, `noteWritten`, `Drain`, and `pipeline.go`'s request construction.
- [ ] **Step 5: Run the test; expect PASS.** Then `go test -race ./internal/assembler/ ./internal/durability/ ./internal/app/`.
- [ ] **Step 6: Commit** — `feat(durability): carry the decoded CRC through to the drain report`.

---

## Task 2: One store, one transaction

**Files:** create `internal/durability/store_sqlite.go`; delete `factlog_sqlite.go` and `extentstore_sqlite.go`; modify `fact.go` (the `FactLog` interface), **`extent.go:145-157`** (where `ExtentStore` declares `Commit`), `resume.go` (`writeBack`), `internal/app/durability.go` (`deleteJobDurability`, wiring), `internal/app/app.go` (**both** `NewSQLiteFactLog` *and* `NewSQLiteExtentStore` wiring), `internal/queue/sqlite_store.go` (`pruneDurabilityRows` `:1294-1315`), plus every fake and bench: `factlog_sqlite_test.go`, `factlog_bench_test.go`, `resume_writeback_test.go`, `mutation_gaps_test.go`.

**Interfaces produced:** `Commit(ctx, jobID string, recs []DurableArticle, exts []FileExtent) error`.

**This task is large because the compiler makes it large.** Changing `Commit`'s signature breaks `Resumer.writeBack` and every fake in one step; they cannot be split into a working intermediate.

- [ ] **Step 1: Define `DurableArticle` here, as a new type.** Do **not** mint it as `ArticleFact` and rename later — the first draft did that and left Tasks 2 and 3 unable to compile. Keep `ArticleFact` alive alongside it until Task 4 deletes its last producer.
- [ ] **Step 2: Write the failing rollback test — and make it actually test rollback.** A `VerifiedTo: -1` extent is rejected by the pre-`BeginTx` validity check (`extentstore_sqlite.go:34-38`), so no statement ever runs and the test cannot distinguish pre-validation from rollback. Force a failure *inside* the transaction instead: a second extent whose `Exec` fails, or a DB closed mid-transaction. Assert `Commit` errors **and** the first record is absent.
- [ ] **Step 3: Run it; confirm it fails.** Record the message.
- [ ] **Step 4: Implement.** Validate every extent **before** `BeginTx`, as the current `Commit` does. Keep `INSERT OR IGNORE` for records **in this task** — the switch to last-write-wins belongs to Task 4, for the reason in that task's note.
- [ ] **Step 5: Guard the empty-extent early return.** The current `Commit` returns nil on `len(exts) == 0`. The merged one must **not** drop records when a cycle produces records but no extents. Add a test for exactly that shape.
- [ ] **Step 6: Update `deleteJobDurability` and `pruneDurabilityRows`'s prose only.** Both carry commentary about Class A and Class B as separate records. **There is still one table each** — `article_facts` and `file_extents` both survive, and `pruneDurabilityRows` still needs two `DELETE`s. What changes is one *store* and one *writer*, not the schema; the Global Constraints say no schema change and the prose must not contradict them.
- [ ] **Step 7: Run `go test -race ./...`; expect PASS.**
- [ ] **Step 8: Commit** — `refactor(durability): commit records and extents in one transaction`.

---

## Task 3: The barrier writes the record — at both commit sites

**Files:** `internal/durability/barrier.go`. Test: `internal/durability/barrier_test.go`.

**This is where the first draft was most wrong.** It said "phase 4 passes both to the merged `Commit`", covering one of two commit sites and one of four walks.

There are **two** drains — `barrier.go:162` (`Run`) and `:599` (`FinalizeFile`) — and **two** commits — `:272` and `:769`. `FinalizeFile` loads `facts` at `:629` and then runs `durableExtent` (`:642`), `recordedExtent` (`:671`), the `unrecorded > 0` guard (`:701`) and `overlapAnywhere`, all *before* its commit at `:769`. If the just-drained articles are durable but absent from `facts`, `unrecorded > 0` fires on **every finalize**, `bound = 0`, and no file is ever truncated — which per spec §7 also costs the QuickCheck exact-size match on every download.

- [ ] **Step 1: Write two failing tests.** (a) A barrier cycle writes records matching exactly the drained set, and only after the commit. (b) **A finalize still truncates** — a file whose tail articles are drained in the finalize itself must reach the same bound it reaches today. Test (b) is the B1 pin and is the more important of the two.
- [ ] **Step 2: Read `Run`'s signature before writing the tests.** It is `Run(ctx, jobID string, t SyncTarget)`.
- [ ] **Step 3: Run both; confirm both fail.** Record both messages.
- [ ] **Step 4: Implement the union — and it is not a concatenation.** Three properties, each of which fails silently if missed:

  **(a) Dedup, keyed on `ArtIdx`, drained wins.** Stored and drained sets overlap by construction: `Confirm` runs only *after* `AckDurable` (`barrier.go:287`, `:776`), so an ack failure leaves the drain report unconfirmed while the commit that now carries the records has landed. The next `Drain` re-delivers those articles (R12 makes delivery at-least-once), and `ForFile` returns them too. A duplicate reaching `verifiedPrefix` satisfies `fact.Offset < prefix && consumed > 0` exactly, which sets `hasOverlap` — so it **raises a false overlap anomaly to the user** and freezes `VerifiedTo`, leaving `consumedAll == false` and the file with no whole-file CRC. `overlapAnywhere` returns the same false pair, and `recordedExtent`'s `missing` counter double-counts.

  **(b) Sort by offset after merging.** `verifiedPrefix`'s doc says *"FactLog.ForFile returns facts ordered by Offset, so this needs no sort of its own."* The drained side is **write-ordered**, not offset-ordered — `noteWritten` appends in write order (`filewriter.go:294-305`) and `Drain` returns `reported ++ written`. Merging without re-sorting stalls the prefix at the first out-of-order element, on every barrier, silently, in the direction that costs the CRC.

  **(c) The union is per-file, not per-cycle.** `Run` loads `facts` inside phase 3's per-file loop (`barrier.go:241`). And the records committed at `:272` must come from `drained` **restricted to `built`** — phase 3 drops closed files with `delete(drained, idx)` at `:254`, and a record for a dropped file must not be committed. `FinalizeFile`'s `written` (`:599`) needs no analogue — it handles one file, and every path that could drop it returns before the commit at `:769`.

  **(d) `durableAt` is the fifth consumer, and it is index-parallel.** `durableAt(facts, idx, ext.Durable, t)` (`barrier.go:555`) returns a closure that indexes `facts[i]`, and it is what `verifiedPrefix` and `overlapAnywhere` are *given* (`:389`, `:765`). Passing the union to `verifiedPrefix` while `durableAt` still closes over the stored slice makes the predicate read a different slice than the walk — out of range when the union is longer, wrong article when it is not. **Both** call sites take it.

  Then pass the union to *all five* consumers — `verifiedPrefix`, `durableAt`, `durableExtent`, `recordedExtent`, `overlapAnywhere` — and widen **both** commit calls. Delete the comment at `barrier.go:383-387` asserting the barrier does not write facts; replace it with why it now does.

  Two things the dedup is **not** needed for, so that the justification does not drift: `durableExtent` is a `max` and is order- and duplicate-insensitive, and `recordedExtent`'s `missing++` counter cannot double-count a drained duplicate, because `buildExtent` set that article's durable bit at `:377` so it lands in `covered`. The dedup stands on `verifiedPrefix` and `overlapAnywhere` alone. `buildExtent` does not double-count `BytesDurable` either — the `if !ext.Durable.Get(ord)` guard at `:376` is exactly that protection — but it does append a duplicate `ArtIdx` to `acked` at `:380`, so confirm `AckDurable` is idempotent over a repeated index and say so in one line.
- [ ] **Step 5: Run both; expect PASS.** Then `go test -race ./internal/durability/`.
- [ ] **Step 6: Mutation-check the union at each of the four consumers separately.** Neuter one at a time and confirm a test fails each time. A single test covering one consumer says nothing about the other three — this is exactly the "one half being pinned says nothing about the other" case.
- [ ] **Step 7: Commit** — `feat(durability): write the article record inside the barrier's commit`.

---

## Task 4: Delete the decode-time append, and switch to last-write-wins

**Files:** `internal/app/pipeline.go`, `internal/app/durability.go` (`appendArticleFacts` at `:1164-1170`), `internal/durability/store_sqlite.go`, `internal/durability/fact.go` (delete `ArticleFact` and `Append`) — **plus every consumer of the type name**, which the first draft omitted: `prefix.go` (`verifiedPrefix`, `overlapAnywhere`, `prefixWalk.overlapVictim`/`overlapArrival`), `barrier.go` (`buildExtent`, `durableAt`, `durableExtent`, `recordedExtent`), `resume.go`, `postanomaly.go:62` (`overlapAnomaly`), `extent.go`, and roughly twenty `_test.go` files across four packages. Deleting a type deletes it from every signature that names it; there is no partial landing. Test: an internal test in package `app`.

**The two halves must land together.** Between them, the decode-time append would still be live *and* last-write-wins, so a redelivery's pre-write, unvalidated offset would overwrite a record the barrier wrote post-`fsync` — `offsetOutOfRange` only rejects it afterwards. `INSERT OR IGNORE` protects that today; `INSERT OR REPLACE` would not. Splitting these leaves two commits where #421 is strictly worse than at `ae865fec`.

- [ ] **Step 1: Establish the test seam first.** The #421 pin needs to observe the record store after a rejected article. `newScenarioHarness` is in package `app_test` and cannot reach `app.factLog` or the DB, so the pin must be an **internal** test in package `app`, or the harness must gain an accessor. Decide and note which — the first draft asserted a helper set that does not exist. Establishing the seam is a step, not an assumption.
- [ ] **Step 2: Establish that the mock NNTP server can emit a chosen `=ypart begin=`.** `nntptest` does not expose this today. If it cannot, the pin must drive `handleSuccessResult` directly with a decode result carrying the bogus offset.
- [ ] **Step 3: Write the failing test** — an article rejected by `offsetOutOfRange` leaves no record. Use offset `1_099_511_627_777` against a small file, which decodes cleanly (`decoder.go` caps `=ypart begin=` at `1 << 48`) and is rejected by `assembler.go`'s `ExpectedSize + ExpectedSize/8` bound.
- [ ] **Step 4: Run it; confirm it fails** with the bogus offset recorded. **This is the load-bearing red check of the change.** Record the message verbatim in the commit body.
- [ ] **Step 5: Delete `appendArticleFacts`, its call site, the pipeline's `FactLog` field, `ArticleFact`, and `Append`.** In the same commit, switch the record insert to `INSERT OR REPLACE`.
- [ ] **Step 6: Run it; expect PASS.** Then `go test -race ./...`.
- [ ] **Step 7: Commit** — `fix(durability): stop recording an article the assembler has not accepted`. Closes #421 and #389.

---

## Task 5: Delete the one reconciliation guard that dies

**Files:** `internal/durability/barrier.go` (the `unrecorded > 0` block and `recordedExtent`'s third return value). Test: `internal/durability/barrier_test.go`.

**Only `unrecorded > 0` dies. `missing > 0` survives and must be kept** — see spec §4. A durable bit without a record stops being expressible once both commit sites write in one transaction; a *record without a durable bit* does not, because `Resumer.verifyRegions` (`resume.go:355-372`) sets the bit only on a CRC match and `continue`s past a mismatch, leaving the record. That article can reach finalize non-durable, and dropping the bound to `durableExtent` there destroys bytes above it — the #342/#350 class through the recovery path.

An earlier draft of this task said both guards go. Deleting `missing > 0` would have re-created, through the resume path, the exact silent data loss this whole change exists to remove.

> **Depends on Task 7 Step 7.** The unreachability proof below is valid when performed and is *falsified by Task 7* unless Task 7 adds the manifest-alignment check to the retained records. A retry that re-derives a shifted numbering maps a surviving extent's durable ordinal onto an `art_idx` whose record `ForFile` no longer returns — which is exactly `unrecorded > 0`, arriving after this guard has been deleted. **Do not run Task 5 until Task 7 Step 7 has landed.**

- [ ] **Step 1: Prove `unrecorded > 0` is unreachable before deleting it.** Construct the state it fires on and show it cannot occur once both commit sites carry records. The paths to close: `pruneDurabilityRows` (`sqlite_store.go:1294`) and `history.Repository.Delete` (`repository.go:347-364`) each drop both tables together, so records cannot vanish under surviving extents; `Resumer.recompute`/`verifyRegions` derive the bitmap *from* the records, so `writeBack` cannot commit a durable bit with no record; `SeedFromExtents` writes the queue's bits, not the durability bitmap. If any path *can* still fire, **stop** and correct the spec instead of the code.
- [ ] **Step 2: Write a test pinning that `missing > 0` still fires** on the resumer path — a record whose region fails CRC verification, then permanently fails on re-fetch, must still trim to the recorded bound rather than the durable one. This is the guard that stays; nothing currently pins *why* it stays.
- [ ] **Step 3: Delete the `unrecorded > 0` block and `recordedExtent`'s `unrecorded` return.** Keep `recordedExtent` itself and the `missing > 0` branch.
- [ ] **Step 4: Run `go test -race ./internal/durability/`.** Any test that fails was pinning the deleted guard; re-read each to decide whether it pinned the guard or the property behind it.
- [ ] **Step 5: Commit** — `refactor(durability): delete the guard a single record makes unreachable`.

---

## Task 6: Derive `done` from the records

> **Run Task 7 before this task.** Task 6 strips the retry's only source of "already succeeded" (`decodeArticlesDone` at `sqlite_store.go:1063`); if Task 7's record retention has not landed, there is one commit in which every retry re-downloads from scratch. Swapping them removes the window at no cost.

**Files:** `internal/queue/sqlite_store.go` (`encodeArticlesDone` `:116`, `decodeArticleFlags` `:157`, `decodeArticlesDone` `:192`, `RestoreJobProgress`'s `qFiles` `:503`, `ArticleCountsByJob` `:655`, `MoveToHistory`'s `qRetain` `:933-934`), **`internal/queue/persistence.go`** (`newJobProgressSized` `:130`, its bit-applying loop `:150-183`), `internal/queue/progress.go` (`resetForReload` `:770-779`), **`test/crash/harness.go`** (`decodeArticlesDone` `:983`) and **`test/crash/crash_test.go`** (`:78`, `:112`). Test: `internal/queue/sqlite_store_test.go`, `internal/queue/persistence_test.go`, and the crash suite.

**Two writers of this state the first two drafts missed.** `resetForReload` (`progress.go:770-779`) clears `done` *and* `failed` for failed articles, and `queue.go:660` / `workset.go:356` persist that reset specifically so `RestoreJobProgress` cannot re-read a stale row — both carry `//lockio` comments saying so. After this task the persisted column can no longer clear `done`, because the record decides it. For an ordinary failed article (no record) nothing changes; for the failed-and-recorded case spec §3.1 says is reachable, it diverges. State the position in code.

**The first draft targeted dead code.** `jobProgressJSON` has had **no production caller since #298** (`progress.go:837`). The real store is `job_files.articles_done`: a hex bitmap packing `[done bits][failed bits]` into **one** column with a hard length check (`:167`, `:211`). Dropping `done` is a re-encoding of a shared blob, not a field deletion.

**And the consumer is `persistence.go`, not `sqlite_store.go`.** `ArticleCountsByJob` only *decodes*; `newJobProgressSized` is what applies the bits. Its loop reads:

```go
if i >= len(f.Done) || !f.Done[i] {
    continue
}
p.done.Set(base + i)
// ... Pending--, pendingArticles--, articlesResolved++
if i < len(f.Failed) && f.Failed[i] {
```

`f.Failed[i]` is applied **only inside** the `f.Done[i]` branch. Encode the column as failed-only and this loop `continue`s over every article, so **no failed bit is ever set and every article of every non-resident job reads as Pending.** This loop, not the decoder, is where `unfinished == !hasRecord && !failed` has to be written, and its 30-line doc comment asserting the bits come from `job_files.articles_done` must be rewritten with it.

`MoveToHistory`'s `qRetain` copies the blob verbatim into `history_job_files`, so this re-encoding silently changes the format Task 7 Step 5 reconciles. The two tasks must agree on it.

**The derivation is `unfinished(art) == !hasRecord(art) && !failed(art)`**, not `done == hasRecord`. `markFailed` sets `done` *and* `failed` so a permanently-failed article is excluded from `CountUnfinishedArticles` and the file still finalizes. Getting this backwards strands every failed article and no file ever completes.

- [ ] **Step 1: Solve the boot cost before writing code.** `ArticleCountsByJob` derives Pending for **every non-resident job at startup**, from the blob, without a manifest (`:648-651`). A naive derivation is one `article_facts` scan per queued job at boot. Establish a cheaper shape — a grouped `COUNT` in one query, or keeping the blob as a cache validated against the records — and record which. **If no shape is cheap enough, stop and re-scope**; this is a re-plan trigger, not an ambiguity to rule on.
- [ ] **Step 2: Write the failing test** — reload derives `done` from records without stranding failed articles. Seed a 3-article job, mark article 1 failed with no record, write a record for article 0, save, reload, assert `CountUnfinishedArticles() == 1`.
- [ ] **Step 3: Run it; confirm it fails.** Record the message.
- [ ] **Step 4: Implement.** Re-encode `articles_done` to carry `failed` alone. Rebuild `done` at load as `hasRecord || failed`. Update `decodeArticleFlags` and `ArticleCountsByJob` to the shape chosen in Step 1.
- [ ] **Step 5: Mutation-check the `|| failed` clause.** Neuter it; confirm Step 2's test fails. If it passes, the clause is untested and the file-completion regression is live.
- [ ] **Step 6: Check `p.done` and `p.emitted` sizing.** `UnmarshalJSON` sized `emitted` from `len(pj.Done)` (`progress.go:845`) and `progress.go:605` panics on a mismatch. Name the new length source.
- [ ] **Step 7: Check `manifest_gate_test.go`'s allow-list.** It is keyed on method names and fails on **stale** entries; a new method reading `job.manifest` must be added, and a method that stops using it must be removed.
- [ ] **Step 8: Run `go test -race ./...`. Commit** — `refactor(queue): derive the done bitmap from the durability records`.

---

## Task 7: The retry path — #422 and the second `done` source

**Files:** `internal/app/app.go` (`RetryHistoryJob`), `internal/history/repository.go` (`Delete`), `internal/queue/sqlite_store.go` (`RestoreRetryProgress` `:1030`), `internal/app/job_finalizer.go` (the retention comment). Test: `internal/app/retry_durability_internal_test.go`.

**These are one task by decision.** #422 and the second `done` source are separate defects, but both live in `RetryHistoryJob`'s path and Task 6 makes the second one reachable, so they stop being separable commits.

- [ ] **Step 1: Write the failing test** — a retry keeps its durability records. Seed a failed job with records, call `RetryHistoryJob`, assert the records survive. Without them `durableExtent` bounds the truncate to the re-fetched articles alone and the partial is destroyed silently.
- [ ] **Step 2: Run it; confirm it fails** with zero records. Record the message.
- [ ] **Step 3: Fix the deletion.** Give `Delete` a variant omitting the durability rows and call it from `RetryHistoryJob`. **Move the delete above `queue.Add` at `app.go:1849`**, so records the re-enqueued job appends before the delete at `:1858` are not caught by it.
- [ ] **Step 4: Fix the two false comments inside `RetryHistoryJob`.** `app.go:1841-1842` claims "its Class A facts survive with it (`Append` is `INSERT OR IGNORE` …)" seventeen lines above the `Delete` that removes them. `app.go:1852-1857` justifies `Delete` by enumerating what goes and what stays and omits the durability rows entirely. **Both are false at `ae865fec`**, before this branch changes anything. Rewrite each; do not inherit either framing.
- [ ] **Step 5: Reconcile the second `done` source — against the CURRENT `[done][failed]` format.** `history_job_files.articles_done` feeds `RestoreRetryProgress` → `decodeArticlesDone` (`:1063`), a non-record source of `done` that Task 6 otherwise leaves live. Make the records the single source, or state in the code why two derivations are one owner. **This task runs before Task 6**, so implement against the format as it exists today; Task 6 then re-encodes both this column and `job_files.articles_done` together, and its own step list must name this site.
- [ ] **Step 6: Reconcile the two ownership comments** — `job_finalizer.go:104-110` says a failed job's rows are kept for the retry; `repository.go:348-357` says the history entry owns them. One must say what owns them now.
- [ ] **Step 7: Add the manifest-alignment check to the retained records. This is required, not a position to write down.** `RestoreRetryProgress` guards its bitmap overlay with `retainedMatchesManifest` (`sqlite_store.go:1022`, `:1118`) because that bitmap is *positionally* indexed. After Step 3 the records survive a retry with no equivalent guard, and a retry re-parses the NZB backup, so the numbering is re-derived rather than carried.

  An earlier draft left this open. It cannot be left open, because **Task 5 depends on it.** If the numbering shifts, a surviving extent's durable ordinal maps to an `art_idx` whose record `ForFile` no longer returns — which is `durable.Count() - len(covered) > 0`, the `unrecorded > 0` state Task 5 deletes the guard for. Leaving the alignment unguarded falsifies Task 5's unreachability proof *after* Task 5 has already run.
- [ ] **Step 8: Run `go test -race ./internal/app/ ./internal/history/ ./internal/queue/`. Commit** — `fix(app): stop the retry deleting the durability rows it needs`. Closes #422.

---

## Task 8: Contract and comment sweep

**Files:** `docs/durability-contract.md`, `docs/ARCHITECTURE.md`, `docs/queue-lifecycle.md`, `docs/article-validation-contract.md`, `internal/durability/proof.go`, `internal/queue/workset.go`, plus whatever the greps find.

- [ ] **Step 1: Read `docs/durability-contract.md` and `docs/ARCHITECTURE.md` in full.** Not grep. The claims that survive a grep are the ones restated in prose sharing no token with the code.
- [ ] **Step 2: Rewrite the Class A/B table, §4 (the truncate bound), and the resume section** against the spec's invariant table. R1 and R2 are deleted, not amended.
- [ ] **Step 3: Grep every falsified literal from the repository root.**

```bash
git grep -n 'INSERT OR IGNORE'
git grep -n 'R1\b'; git grep -n 'R2\b'
git grep -n 'asserts nothing about presence'
git grep -n 'ArticleFact'; git grep -n 'appendArticleFacts'
git grep -n 'recordedExtent'; git grep -n 'unrecorded'
git grep -n 'articles_done'; git grep -n 'at decode'
git grep -n 'articles_done' -- test/     # the crash harness reimplements the format
```

- [ ] **Step 4: Fix the stale claim already in the tree.** `progress.go:790-794` says the done bit "is set only once the bytes have reached WriteAt (#355)", contradicting `statusinfo.go:200-207` **today** and doubly wrong after Task 6. Do not inherit its framing.
- [ ] **Step 5: Re-read `proof.go` and `workset.go`.** Their docs assert the barrier is the only `done` authority. That stays true, but the mechanism changes.
- [ ] **Step 6: Check `001_initial.sql:150-152` and `:329`** — they claim `articles_done` is "the authoritative record" and that "nothing a retry needs is lost". Both are falsified. The file is applied and must not be edited; supersede in a later migration's comment block, or record that no later migration exists yet and the correction is owed.
- [ ] **Step 7: Run `pr-review-toolkit:comment-analyzer` over the cumulative branch diff**, once, on the last round.
- [ ] **Step 8: Run both whole-repo gates. Commit** — `docs(durability): restate the contract for a single post-fsync record`.

---

## Inconclusive / Deferred items

1. **Coalesced-run partial writes.** `flushRun` fails every article in a run on any write error, and `WriteAt` can return `io.ErrShortWrite` having written leading bytes — bytes on disk with no record.
   *Narrowed:* `writeOne`/`flushRun` discard `n` and fail every part on any error, so the leading-bytes case arises only through the injectable `w.writeAt` seam.
   *Probe:* inject a short write inside `flushRun`; observe whether the leading article's bytes are fully written.
   *Branches:* (a) those bytes sit above every good record, so a `max` bound never drops below good content → accept under Standing Design Rule 3; (b) the bound can drop below a good article's bytes → **stop**, the truncate argument is refuted.

2. **The boot-time derivation shape** (Task 6 Step 1).
   *Probe:* benchmark `ArticleCountsByJob` against a seeded queue of jobs at 20k articles each, before and after.
   *Branches:* (a) a grouped `COUNT` is within noise → proceed; (b) it is not → keep the blob as a cache validated against the records; (c) neither is acceptable → **re-plan trigger**, Task 6 leaves this change.

3. **Whether `SeedFromExtents` and `ReplaceFromResume` remain necessary.**
   *Established:* they install `done` from a **CRC-verified** extent bitmap (`resume.go:373`), which a raw record scan does not reproduce, so they are **not** redundant.
   *Open:* whether the plan can state, in code, why two derivations of `done` are one owner rather than two. If it cannot, Task 6's premise weakens and Task 7 Step 5 grows.

### Answered from the code, and no longer open

- **Where the CRC lives between `Accept` and `Drain`** — `bufferedArticle` and `runPart` already carry offset, id and length. Add a field; no new map.
- **`retryFinalize` against an evicted job** — it returns before `finalizeCompletedFile` when the handle is absent or `syncTargetFor(jobID) == nil` (`stall.go:504,514,528`), so an evicted job never reaches the record write.
- **Other pre-barrier readers of the records** — `ForFile` has exactly three callers: `barrier.go:241`, `barrier.go:629`, `resume.go:281`. No fourth reader loses decode-time records.

## Corrections applied after validation

1. **Both commit sites, and all four walks.** The first draft wired the record write into `Run`'s phase 4 only, and named only `verifiedPrefix` as needing the union. `FinalizeFile` has its own drain and its own commit, and three more walks between them. As first written, `unrecorded > 0` would have fired on every finalize and no file would ever have been truncated.
2. **Task 6 targeted dead code.** `jobProgressJSON` has no production caller. The real store is a shared `[done][failed]` blob in `job_files.articles_done`.
3. **Last-write-wins moved into Task 4.** Landing it in Task 2 left two commits where a redelivery's unvalidated offset could overwrite a post-`fsync` record.
4. **`DurableArticle` is defined in Task 2, not renamed in Task 5.** The first draft used the type two tasks before introducing it, so neither task could compile.
5. **Test signatures are established before tests are written.** `Accept` takes `(id, off, data)`, not a `WriteRequest`; `Run` takes a `SyncTarget`; the #421 pin cannot live in package `app_test`.
6. **A task deletes the guards.** The spec claimed they become unreachable and no task acted on it.
7. **`BytesDownloaded` was already derived**, so the spec's claim that it "stays persisted" was wrong on the facts.

### Round two

Round one's corrections were themselves incomplete. Every finding below sat immediately adjacent to a fix round one had just applied — which is the predicted failure mode and the reason a corrected plan is re-reviewed in fresh context rather than re-read by its author.

8. **The union is not a concatenation.** Round one established that four walks need it and stopped there. It also needs dedup keyed on `ArtIdx` (stored and drained sets overlap whenever an ack fails after a commit), a re-sort (the drained side is write-ordered, the stored side offset-ordered), and per-file scoping restricted to `built`. Missing the dedup raises a **false overlap anomaly to the user** and costs the whole-file CRC; missing the sort costs the CRC on every barrier, silently.
9. **`missing > 0` does not die.** The spec claimed both guards become unreachable and round one added a task to delete both. Only `unrecorded` dies. Deleting `missing` would re-create, through the resume path, the silent data loss this change exists to remove. Corrected in the spec as well as here.
10. **Task 6's consumer is `persistence.go`, not `sqlite_store.go`.** Round one found the right *column* and still named the wrong *function*. `newJobProgressSized` applies `f.Failed[i]` only inside the `f.Done[i]` branch, so a failed-only encoding would leave every article of every non-resident job reading as Pending.
11. **Deleting a type deletes it from every signature.** Round one moved `DurableArticle`'s definition into Task 2 but left Task 4's file list naming four files, when `ArticleFact` appears in signatures across five source files and about twenty test files.
12. **Task 2's rollback test pinned nothing** — extents are validated before `BeginTx`, so the invalid extent never reached a transaction to roll back.
13. **Task 7 must run before Task 6**, or one commit exists in which every retry re-downloads from scratch.
14. **"One table now" was false.** Both tables survive; one *store* and one *writer* replace two.

### Round three

Three blocking, and the same shape a third time — a seam identified correctly and wired into fewer places than need it.

15. **The crash-consistency suite reimplements the blob format, and no gate ran it.** `test/crash/harness.go:983` decodes `articles_done` independently and `t.Fatalf`s on a length mismatch (`:997`); `crash_test.go:78,112` builds the suite's central invariant on `Done[i] && !Failed[i]`. `run_tests.sh:140-148` deliberately excludes `-tags=crash`, so Task 6 would have broken the repository's most durability-relevant suite while every gate in this plan stayed green. Added to the gate list, to Task 6's files, and to Task 8's greps.
16. **`durableAt` is the fifth union consumer.** Rounds one and two converged on "four walks". `durableAt` (`barrier.go:555`) returns a closure that indexes `facts[i]` and is what `verifiedPrefix` and `overlapAnywhere` are *given* — so passing the union to the walk while the predicate closes over the stored slice reads two different slices.
17. **Task 5's proof is valid when performed and falsified by Task 7.** Under the corrected 1-2-3-4-5-7-6-8 order, Task 7 makes both tables survive a retry while leaving the records' manifest alignment unguarded — re-creating `unrecorded > 0` after Task 5 has deleted the guard for it. Task 7 Step 7 is now mandatory rather than a position to record, and Task 5 declares the dependency.

Also corrected: six line numbers and one signature — including, in the very step warning that `Accept`'s signature must be read before writing a test against it, a copy of that signature with its return value dropped.
