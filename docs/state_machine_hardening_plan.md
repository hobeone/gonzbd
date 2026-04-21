# State-Machine Hardening Plan

Plan to audit, test, and fix the NZB state-transition interactions between
`queue`, `downloader`, `assembler`, `app` (pipeline + completion watcher),
`postproc`, and `history`. Read `docs/nzb_processing_lifecycle.md` first for
the current intended flow; this document lists what is wrong with that flow
today and how to fix it.

## Scope and goals

1. Produce a deterministic test harness that can freeze the pipeline at any
   named state transition and assert invariants.
2. Fix the known corner cases uncovered during the audit. Each fix lands as
   its own commit and turns one red scenario test green.
3. Leave behind an integration-level fault-injection suite and an up-to-date
   state diagram so future changes to state-mutating code paths are easy to
   review.

Red tests at head-of-branch are acceptable during the landing sequence (per
the project lead): Layer 1 commits the scenarios as `EXPECTED FAIL`, and
each Layer 2 fix turns the corresponding scenario green.

## Summary of audited state flow

```
Add → Queue[Queued]
   └─ downloader.dispatchPass → connWorker.handleRequest
        ├─ success   → queue.MarkArticleDone → emit ArticleResult
        ├─ 430       → emit ArticleResult (err = ErrNoArticle)
        └─ net err   → unmarkTried → emit ArticleResult
   pipeline.handleResult
        ├─ ErrNoServersLeft → queue.MarkArticleFailed → assembler.WriteArticle{FatalErr}
        ├─ decode err       → drop silently
        └─ ok               → assembler.WriteArticle{Data}
   assembler → OnFileComplete (internalFileComplete, blocking)
   app.watchCompletions → queue.MarkFileComplete → IsComplete? → sendToPostProcessor
   downloader.dispatchPass health gate → OnJobHopeless → sendToPostProcessor
   postproc.run → stages → OnJobDone → history.Add + SaveJob + queue.Remove
   app.Start rescan: any IsComplete job → sendToPostProcessor
   RetryHistoryJob → reset failed articles → queue.Add → history.Delete
```

`SetPostProcStarted` is the single CAS that guarantees a job is handed to
the post-processor exactly once, regardless of which path fires first
(normal completion, hopeless-gate, startup rescan, or retry).

## Corner cases to fix

Numbering here matches the original audit notes; the fix-step IDs (B.x) in
the sequence below reference these.

1. **Crash recovery can strand jobs with `PostProc=true`.** The flag is
   persisted but `queue.Remove` is not transactional with `history.Add`. On
   restart, the rescan funnels through `SetPostProcStarted`, which returns
   `(false, nil)` and silently skips — the job lives in the queue forever.
   `app.go:377-382` + `queue.go:192-205`. *Fixed by B.1.*
2. **Deadlock window in `tryDispatch`.** When all servers are exhausted
   `emitResult` is called while holding both `tryMu` and the queue
   `RLock` (from `ForEachUnfinishedArticle`); the consumer of
   `completions` needs the queue write lock, so a full 256-slot
   completions buffer deadlocks the dispatcher.
   `dispatch.go:104-150` + `pipeline.go:82`. *Fixed by B.2.*
3. **Files with all-failed articles may never `Complete`.** Needs
   verification that the assembler fires `OnFileComplete` for a file whose
   last parts arrive via `WriteRequest{FatalErr}`. If not, `IsComplete()`
   stays false and the hopeless-gate is the only escape — and it no longer
   fires once every article has been visited. *Verified and, if needed,
   fixed as part of B.6.*
4. **Queue state is persisted only on `Shutdown`.** All per-article and
   per-file progress is in-memory. A crash mid-download discards work that
   is on disk. *Fixed by B.4.*
5. **"Done" is set before the bytes are durable (bug).** `handleRequest`
   calls `MarkArticleDone` before the assembler writes. Crash between the
   two leaves the queue claiming progress that does not exist on disk.
   "Done" must mean "on disk". *Fixed by B.6.*
6. **`RetryHistoryJob` does not reset `DownloadStarted` or `ServerStats`.**
   Post-proc duration math (`app.go:188-192`) produces misleading history
   entries on retry. *Fixed by B.3.*
7. **`ReloadDownloader` can drop emitted-but-unprocessed results.** Between
   old-downloader `Stop()` and `pipeline.setCompletions(newCh)`, the
   pipeline nils `p.completions` on the closed-channel signal. Under
   "Done = downloaded" this corrupted queue state; under "Done = on disk"
   (B.6) the dropped results are harmless — the articles stay `Done=false`
   and the new downloader re-dispatches them. *Verified and possibly
   collapsed to a test-only commit by B.5.*
8. **External `fileComplete` / `jobComplete` channels drop on full.**
   Documented and intentional; not in scope for this plan. Noted here so
   reviewers do not confuse it with issue 7.

## Layer 1 — Test harness

Each step below is one commit.

### A.1 — `queue.Snapshot()` helper
- Returns a deep-copied, read-only view of all jobs including per-article
  `Done`/`Failed`, `PostProc`, `RemainingBytes`, `Files[].Complete`.
- Pure addition; no production caller.
- **Files:** `internal/queue/snapshot.go`, `internal/queue/snapshot_test.go`.
- **Model:** Haiku. **Effort:** low.

### A.2 — Fake NNTP server for integration tests
- Grep `internal/nntp/` first; skip if a usable fake already exists.
- Otherwise a `net.Listener`-based fake that answers `ARTICLE <msgid>` from
  a scripted map and supports: 430-not-found, connection-drop mid-body,
  deliberate stall. Expose `ScriptedServer.InjectFailure(msgID, failure)`.
- **Files:** `internal/nntp/fakeserver.go` (new) + its test.
- **Model:** Sonnet. **Effort:** medium.

### A.3 — State-machine test orchestrator
- Helper in `internal/app/statemachine_test.go` that builds a real
  `Application` with the fake NNTP server and exposes `WaitUntil(predicate)`
  plus `FreezeAt(transition)`.
- Named transitions: `ArticleDispatched`, `ArticleDone`, `FileComplete`,
  `JobComplete`, `PostProcStarted`, `PostProcDone`, `HistoryWritten`.
- Implement via instrumentation hooks threaded through existing callbacks
  (`OnJobHopeless`, `OnFileComplete`, `OnJobDone`); add at most one
  test-only hook on `Application` gated behind a `_test.go` file.
- **Files:** `internal/app/statemachine_test.go`.
- **Model:** Opus. **Effort:** medium. Design-heavy — shape determines how
  readable Layer 2's regression tests will be.

### A.4 — Scenario tests (expected to fail)
Six tests, one commit each, explicitly marked `// EXPECTED FAIL until B.x`:

1. `TestRecovery_PostProcTrueOnRestart` — shutdown after
   `SetPostProcStarted` but before `queue.Remove`; verify restart completes
   the job. *(Fails until B.1.)*
2. `TestDispatch_NoDeadlockWhenCompletionsFull` — fill completions buffer,
   exhaust servers for one article; verify dispatcher makes progress.
   *(Fails until B.2.)*
3. `TestRetry_ResetsDownloadStats` — retry a historic job; verify
   `DownloadStarted` and `ServerStats` are zeroed. *(Fails until B.3.)*
4. `TestCheckpoint_SurvivesCrashMidDownload` — force-kill between article
   completions; verify ≤ 30s of progress lost. *(Fails until B.4.)*
5. `TestReload_NoArticleLossInFlight` — call `ReloadDownloader` with N
   articles in flight; verify all N eventually land on disk.
   *(May flip green with B.6; otherwise B.5.)*
6. `TestDurability_DoneMeansOnDisk` — crash between `MarkArticleDone` and
   assembler write; verify next run re-downloads that article.
   *(Fails until B.6.)*

- **Files:** six files under `internal/app/` with `_test.go` suffix.
- **Model:** Sonnet. **Effort:** medium per test; one session after A.3 lands.

## Layer 2 — Fixes

Each step below is one commit. Land order is specified in the sequence
table at the bottom.

### B.1 — Consolidate the completion funnel + fix restart stranding
- Extract `app.maybeFinalize(job)` as the *only* site that calls
  `sendToPostProcessor`. Callers: `watchCompletions`, `Start` rescan,
  `RetryHistoryJob`, `OnJobHopeless`.
- In `Start` rescan: for each job where `job.PostProc == true`, enqueue
  directly into `postProcessor.Process(...)` bypassing
  `SetPostProcStarted` (the flag is already set). Gate on whether
  the post-proc queue already contains that ID (add
  `postproc.Has(jobID) bool`).
- Document why the CAS bypass is safe: at the point of the rescan the
  downloader and assembler are not started, so the startup goroutine is
  the exclusive owner.
- **Files:** `internal/app/app.go`, `internal/postproc/postproc.go`,
  `internal/postproc/queue.go`.
- **Model:** Opus. **Effort:** medium.

### B.2 — Move `emitResult` out of held locks
- In `tryDispatch`, when `!anyEligible`, return a signal instead of
  emitting inline.
- The caller (`dispatchPass`) collects these into a local slice after
  `ForEachUnfinishedArticle` returns, then emits them with no locks held.
- **Files:** `internal/downloader/dispatch.go`.
- **Model:** Sonnet. **Effort:** low. Mechanical.

### B.3 — `RetryHistoryJob` resets full download state
- Zero `DownloadStarted` and `ServerStats`; everything else is already
  correct. Two-line change.
- **Files:** `internal/app/app.go` (`RetryHistoryJob`).
- **Model:** Haiku. **Effort:** low.

### B.6 — Article durability (bug fix)
- Thread `MessageID` through `assembler.WriteRequest`.
- On successful `pwrite + fsync` in the assembler worker, call
  `queue.MarkArticleDone(jobID, messageID)`.
- On `WriteRequest{FatalErr: ...}`, call `queue.MarkArticleFailed(...)`
  from the assembler (moved from `pipeline.handleResult`).
- Remove `queue.MarkArticleDone` from
  `downloader/dispatch.go:handleRequest` (~line 254).
- Verify the assembler does fsync before firing the completion callback;
  if not, that fix belongs in this commit.
- The downloader's existing `inFlight` map continues to provide
  in-process dedup during a single run — no new mechanism needed.
- Verify issue 3 at the same time: a file whose parts all arrive as
  `WriteRequest{FatalErr}` must still fire `OnFileComplete`.
- **Files:** `internal/assembler/assembler.go`,
  `internal/downloader/dispatch.go`, `internal/app/pipeline.go`,
  test updates.
- **Model:** Opus. **Effort:** medium-high. The ordering constraint
  ("Done must fire from the same goroutine that made the bytes durable")
  is easy to get subtly wrong.

### B.7 — Batch the Done/Failed writes from the assembler
- Add `queue.MarkArticlesDone(jobID string, messageIDs []string) error`
  and `queue.MarkArticlesFailed(jobID string, messageIDs []string)
  (firstTimeIDs []string, err error)`. Each takes the queue write lock
  exactly once.
- In the assembler, maintain a per-job pending set of completed
  `MessageID`s. Flush triggers:
  1. A file's `OnFileComplete` fires — *flush before the callback*, so
     `watchCompletions` never sees `IsComplete()` true on a file whose
     articles are not yet all marked Done.
  2. A timer, default 250ms, configurable via
     `assembler.Options.DoneFlushInterval` — keeps UI progress live for
     long-running files.
  3. `Stop()` — final drain before the worker exits.
- `MarkArticlesFailed` returns the subset that flipped for the first time
  so the "first-time failure" logic currently in `pipeline.handleResult`
  (for emitting `ErrNoServersLeft`) still works after the move.
- Benchmark in `internal/assembler/assembler_bench_test.go`: measure
  articles/sec under a fake fast-path resolver before and after.
  Target: ≥ 2× on a pathological workload (many tiny articles) and no
  regression on realistic sizes.
- **Files:** `internal/queue/queue.go` (two new methods + tests),
  `internal/assembler/assembler.go`, `internal/assembler/assembler_bench_test.go`.
- **Model:** Opus. **Effort:** medium.

### B.4 — Periodic queue checkpoint
- Add a `time.Ticker` in `Application.Start` (default 30s, configurable)
  that calls `queue.Save(...)`. Coalesce with the existing `Shutdown`
  save. Guard against running after `Shutdown` via the existing
  `stopped` atomic.
- Add an `atomic.Bool` "dirty" flag on the queue set by
  `MarkArticleDone`/`MarkArticleFailed`/`MarkFileComplete`/
  `MarkArticlesDone`/`MarkArticlesFailed` and cleared by `Save`; the
  ticker no-ops on clean queues.
- Lands after B.6/B.7 so the checkpoint reflects genuine on-disk state.
- **Files:** `internal/queue/queue.go`, `internal/app/app.go`.
- **Model:** Sonnet. **Effort:** low-medium.

### B.5 — `ReloadDownloader` — verify, possibly no code
- Under "Done = on disk" (B.6), dropped emitted-but-unprocessed results
  are harmless: articles stay `Done=false` and the new downloader
  re-dispatches them.
- Budget one investigation session using A.3's orchestrator. If the
  `TestReload_NoArticleLossInFlight` scenario passes after B.6 without
  code changes, this commit is just the test flip from `EXPECTED FAIL`
  to green.
- **Files:** likely none beyond the test.
- **Model:** Opus. **Effort:** low (investigation-heavy).

## Layer 3 — Continuous invariants

### C.1 — Fault-injection integration test
- Real NZBs under `test/integration/statemachine_test.go` with
  `//go:build integration`, driven by A.2's fake server and a chaos
  harness (`InjectFailure` in random patterns).
- Assert end-state invariants:
  - Every added job eventually reaches history.
  - No job stays in the queue with `PostProc=true` for more than 60s.
  - `ServerStats` sums match the injected byte counts.
- **Model:** Sonnet. **Effort:** high — mostly scaffolding.

### C.2 — State diagram in `docs/implementation_notes.md`
- Mermaid diagram of all legal state transitions.
- PR template gate: updating this diagram is required when touching
  state-mutating queue methods.
- **Model:** Haiku. **Effort:** low.

## Commit sequence

| Order | Step | What lands | Model  | Effort       | Turns green |
|-------|------|------------|--------|--------------|-------------|
| 1     | A.1  | `queue.Snapshot()` helper                   | Haiku  | low          | —           |
| 2     | A.2  | Fake NNTP server (skip if exists)           | Sonnet | medium       | —           |
| 3     | A.3  | Test orchestrator                           | Opus   | medium       | —           |
| 4     | A.4  | Six red scenario tests                      | Sonnet | medium       | — (red)     |
| 5     | B.2  | `emitResult` deadlock fix                   | Sonnet | low          | A.4 #2      |
| 6     | B.3  | `RetryHistoryJob` reset                     | Haiku  | low          | A.4 #3      |
| 7     | B.1  | Completion funnel + restart recovery        | Opus   | medium       | A.4 #1      |
| 8     | B.6  | Article durability (bug fix)                | Opus   | medium-high  | A.4 #6 (and likely #5) |
| 9     | B.7  | Batch Done/Failed writes                    | Opus   | medium       | —           |
| 10    | B.4  | Periodic queue checkpoint                   | Sonnet | low-medium   | A.4 #4      |
| 11    | B.5  | Reload downloader (verify)                  | Opus   | low          | A.4 #5 (if not already) |
| 12    | C.1  | Fault-injection integration suite           | Sonnet | high         | —           |
| 13    | C.2  | State diagram                               | Haiku  | low          | —           |

Rationale for order:
- Test harness before fixes so each fix lands with a regression test.
- B.2 and B.3 first among fixes: cheapest, most obviously correct,
  reduce noise before the opus-effort commits.
- B.1 before B.6 because B.1 tightens the completion funnel that B.6
  then feeds from the assembler goroutine.
- B.6 before B.7 so the slow-but-correct durability fix ships first and
  the batched version has a known-good baseline to benchmark against.
- B.4 after B.6/B.7 so checkpoints capture truthful on-disk state.
- B.5 last among fixes: likely a no-op once B.6 lands.
- C.1/C.2 as cleanup; not blocking any bug fix.

## Open decisions

- **Default checkpoint interval (B.4):** 30s proposed. Change if
  sustained write I/O is a concern for large queues.
- **Default `DoneFlushInterval` (B.7):** 250ms proposed. Shorter keeps
  UI responsive; longer amortizes lock cost further. Tunable via
  `assembler.Options`.
- **Whether `queue.Snapshot()` graduates out of test-only usage** once
  the API server wants a consistent-read view of queue state. Not in
  scope for this plan but worth considering when touching the API layer.
