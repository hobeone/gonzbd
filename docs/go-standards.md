# Go Coding Standards, Testing Standards, and Lessons Learned

Read this before creating, editing, or refactoring any `.go` file. It is the
project-specific complement to `AGENTS.md`'s always-loaded core — everything
here is scoped to Go source and doesn't need to be in context for a
docs-only or UI-only change.

## Go Coding Standards

### Idioms (Required)

- **Accept interfaces, return structs**. Define interfaces at the consumer side, not the producer side.
- **Small interfaces**. Single-method interfaces are good. Compose with embedding when needed.
- **Context propagation**. Every blocking operation accepts `context.Context` as its first parameter.
- **Error wrapping**. Use `fmt.Errorf("operation failed: %w", err)` to preserve error chains. Never use `%v` on errors.
- **Structured logging**. Use `log/slog`. Pass `*slog.Logger` via constructor; do not use a package-level global logger. **All loggers must be component-scoped** using `.With("component", "name")` to support log filtering.
- **Goroutine lifecycle**. Every goroutine has a clearly defined exit condition tied to a context, channel close, or explicit signal. No "fire and forget" goroutines.
- **Standard library first**. Prefer `slices`, `maps`, `errors.Is/As`, and `min`/`max` builtins over custom helpers or reflection.

### Anti-Patterns (Forbidden)

- **No `panic` for control flow.** Panic is for unrecoverable programmer errors only.
- **No silent error swallowing.** `_ = doSomething()` requires a comment explaining why the error is intentionally ignored.
- **No `time.Sleep` in tests** for synchronization. Use channels, `sync.WaitGroup`, or `chan struct{}` signals.
- **No `init()` functions** for non-trivial setup. Use explicit `New*` constructors called from `main`.
- **No global mutable state.** Configuration, loggers, and dependencies are passed explicitly.
- **No `interface{}` / `any`** in new code unless absolutely required (e.g., generic JSON handling). Prefer concrete types or generics. When a dynamic type is necessary, prefer `any` over `interface{}`.

### Database Migrations

All schema changes MUST be implemented as a new `goose` migration file in
`internal/history/migrations/`. **Never modify existing migration files.**

### Concurrency Architecture (Decided)

The architecture establishes specific concurrency patterns. Follow them:

- **Queue → Downloader signaling**: channel-based (`chan struct{}`, cap=1, non-blocking send). NOT `sync.Cond`. Rationale in `docs/ARCHITECTURE.md` § Coordination Architecture.
- **Queue internal locking**: `sync.RWMutex`. The hot path (`GetArticles`) takes RLock; mutations take full Lock.
- **Per-NzbObject locking**: `sync.Mutex` per object.
- **Article cache**: `sync.RWMutex` + `atomic.Int64` for memory tracking.
- **Downloader main loop**: `select{}` over multiple channels.

If a new component needs coordination, document the choice (mutex vs channel vs other) in a comment near its declaration.

### Persistence (Decided)

- **Queue state**: in-memory with event-triggered JSON+gzip persistence per NzbObject. NOT SQLite.
- **History**: SQLite via `modernc.org/sqlite` (pure Go, no CGO).
- **Config**: YAML via `gopkg.in/yaml.v3`.
- **Atomic writes**: all file persistence uses temp file + fsync + rename.

Rationale is documented in `docs/ARCHITECTURE.md`. Do not deviate without escalating.

## Library Selection

Prefer existing, well-maintained Go libraries over custom implementations. Before writing utility code, search for an existing solution.

When evaluating a new library:
- Check last commit date (active in last 12 months)
- Check open issues for concerning bugs
- Check that it has tests and reasonable test coverage
- Verify license compatibility (GPL-2.0+ for SABnzbd compatibility)
- Escalate the addition for user approval

## Testing Standards

- **Table-driven tests** with subtests (`t.Run`) for each case.
- **`-race` flag** required for tests involving goroutines or shared state.
- **Test files alongside source**: `foo.go` ↔ `foo_test.go`.
- **Test helpers** in `testhelper_test.go` or a `testdata/` package.
- **Integration tests** under `test/integration/` with `//go:build integration` tag.
- **Mocks/fakes** preferred over interface mocking frameworks. Hand-rolled fakes are clearer than `gomock`-generated ones for small interfaces.
- **Coverage target**: 80%+ for `internal/` packages. Don't chase coverage for trivial code paths.

### Coverage Exemptions

The `scripts/check_coverage` tool enforces an 80% per-function threshold on
changed code. Some functions are **trivially correct by inspection** and testing
them adds no confidence — e.g., no-op interface stubs, single-field getters, or
type-assertion wrappers. **Do not insert dead code** (like `_ = struct{}{}`)
to make the coverage tool instrument empty method bodies. That is coverage
gaming, not testing.

Instead, mark the function with a `//nocover:` comment on the `func` line
explaining *why* testing provides no value:

```go
func (d dummyEmitter) Broadcast(_ Event) {} //nocover: no-op interface stub
```

The coverage checker skips any function whose declaration line contains
`//nocover:`. The comment MUST include a reason after the colon. Functions
eligible for exemption:

- **No-op interface stubs** (empty method bodies satisfying an interface).
- **Trivial getters/setters** with no logic, branching, or side effects.
- **Compile-time interface checks** (`var _ Foo = (*Bar)(nil)`).

Functions NOT eligible — these must be tested:

- Anything with branching (`if`, `switch`, `for`).
- Anything with error handling or error wrapping.
- Anything that mutates shared state.

### Red-Green Discipline (write the failing test first)

**Every bug fix and every regression test MUST be proven to fail on the
unpatched code before the fix lands.** A test that already passes against the
buggy code does not test the fix — it is a change-detector that will silently
let the bug return. This has happened here repeatedly: tests named for a fix
that still passed with the fix reverted (a `"1.0 TiB"` case that never reached
the panicking branch; a `download.nzb` fallback case that never exercised the
fixed `/` path). A passing test is not evidence until you have seen it fail for
the right reason.

The required order for any fix:

1. **Write the test first**, encoding the *correct* expected behavior (not the
   current output — assert what the code *should* do, with an independent oracle
   where possible).
2. **Run it against the unfixed code and watch it FAIL.** For a pre-existing
   bug, write the test before touching the code. For a regression guard added
   alongside a fix, stash or revert the fix (e.g. the one-line change) and
   confirm the test goes red. Read the failure message — it must fail because of
   the bug, not a typo or wrong setup.
3. **Apply the fix**, confirm the test now passes, and confirm the rest of the
   suite stays green.

**The cheap pre-commit check for any `fix:` + `test:` pair:** mentally (or
actually) revert the fix and confirm the new test fails. If it still passes, the
test is exercising the wrong branch or input — fix the *test*, not just the
code. The fix and its test belong in the same change so this is verifiable.

**For de-flaking concurrency/timing tests**, the analogous proof is
`go test -race -count=N` (N ≥ 50, ideally also under `GOMAXPROCS=1`): a single
green run does not prove a flaky test is fixed, because a flaky test passes most
of the time by definition. Replace synchronization `time.Sleep` calls with a
deterministic signal (channel, `sync.WaitGroup`, or a poll-until-condition
helper); leave only genuine timing windows (mock latency, negative-observation
windows) and document each as intentional.

## Go Backend Lessons Learned

These rules are distilled from real bugs found across dozens of audit and hardening commits. **Every rule below was learned from a production-quality bug.** They must be followed for all new Go code.

### 1. Concurrency & Locking

- **Never hold a mutex during disk I/O or network calls.** Snapshot data under the lock (e.g., JSON-marshal), release the lock, then perform I/O. Holding `RLock` during `writeGzJSON` blocked the entire download pipeline for seconds. Pattern: `mu.RLock() → marshal → mu.RUnlock() → writeToDisk(marshaledBytes)`.

- **Always use `defer mu.Unlock()`.** Manual unlock-before-return in multiple branches has caused deadlocks and double-close panics. The only exception is snapshot-then-release (above), where unlock is intentional mid-function. In that case, add a `// --- No lock held below this line ---` comment.

- **Never `delete()` from a map while holding `RLock`.** `RLock` permits concurrent readers; mutation requires a full `Lock`. This caused a `concurrent map write` panic in the WebSocket broadcaster.

- **Every `select` on a channel or semaphore must also watch the relevant context/shutdown channel.** Goroutines blocked on semaphore acquisition without watching `c.ctx.Done()` blocked forever when the connection died. Pattern:
  ```go
  select {
  case sem <- struct{}{}:
  case <-ctx.Done():
      return ctx.Err()
  case <-shutdownCh:
      return ErrShutdown
  }
  ```

- **Don't expose mutable data to concurrent readers before it is fully initialized.** Calling `addHistory(job)` before `processJob(job)` exposed partially-initialized `StageLog` fields to API handlers reading the same struct.

- **Atomic flag ordering matters.** In `finishReader`, `closeErr` must be set *before* the `closed` atomic flag is flipped, otherwise concurrent readers see `closed=true` but read a nil error.

- **Use `sync.Once` or `CompareAndSwap` for idempotent stop/close.** Multiple stop paths (shutdown, error, cancel) can race. Using `closeOnce.Do(func(){...})` prevents double-close panics on channels and connections.

- **Guard `Start()`/`Stop()` state checks with a mutex, not bare reads.** `CancelJob` must check `started`/`stopped` under `mu.Lock` and track `inFlight` to prevent sending on a closed channel during `Stop()`.

- **Set state atomically with its observable effect.** `setBusyWithJob(true, ...)` must happen inside `popWithPause()`, not after return, to eliminate the window where `Empty()` returns true while a job is being processed.

### 2. File I/O & Persistence

- **All disk writes must be atomic: temp file → fsync → rename.** `os.WriteFile` truncates before writing; concurrent readers see partial/corrupt data. Use `os.CreateTemp` → write → `Sync()` → `Close()` → `os.Rename`. This pattern was missing in cache, queue, and dirscanner state — all required the same fix.

- **Use `os.CreateTemp` for unique temp files, never a hardcoded `.tmp` suffix.** Concurrent writes to `path + ".tmp"` corrupt state files. Dirscanner state had this bug.

- **Close the source file before `os.Remove` in cross-device move.** `defer in.Close()` runs after `os.Remove(src)`, which fails on some platforms because the file handle is still open.

- **On resume, count unfinished articles, not total articles.** `len(Articles)` includes already-downloaded parts that won't be re-dispatched, causing the assembler to hang waiting for parts that will never arrive.

- **Never delete an archive on partial extraction failure.** If only some files fail to extract from a ZIP/RAR, preserve the archive for retry or manual recovery.

- **Check directory containment before recursive delete.** `SortStage` deleted `FinalDir` when it was inside `origDir`. Always verify `!strings.HasPrefix(targetDir, sourceDir)` before removing a directory tree.

- **Path length limits are per-component (NAME_MAX = 255 bytes), not per-path.** This is Linux-only software; do not import Windows MAX_PATH heuristics. When sanitizing folder + filename pairs, make the folder name a function of the job alone — never derive folder truncation from the filename, or files in the same job will scatter across multiple directories.

### 3. Shutdown & Lifecycle Ordering

- **Shutdown order: stop producers → drain consumers → cancel context → wait → cleanup.** The correct order is: (1) Stop downloader (no new articles), (2) Stop assembler (drains in-flight writes, delivers completions), (3) Cancel context (watchCompletions exits), (4) Wait for goroutines, (5) Stop post-processor, flush cache, save queue. Getting this wrong drops file completion events.

- **Fallback goroutines spawned for channel delivery must watch `ctx.Done()`.** A `go func() { ch <- val }()` goroutine leaks forever if the receiver has exited. Always add a `case <-ctx.Done()` branch.

- **Don't penalize servers on `context.Canceled`.** Pause and shutdown cancel contexts, which is not a server error. Check `ctx.Err()` before calling `RecordBadConnection` or `ApplyPenalty`.

- **Clean up orphaned resources on startup.** Crash-orphaned temp files, stale lock files, and incomplete downloads accumulate across restarts. `Prune()` must clean these up.

### 4. HTTP API & Security

- **Extract `mode` and `apikey` from query params first, form body second.** For routing (`mode=`) and authentication (`apikey=`/`nzbkey=`), always check `r.URL.Query()` first. For POST requests, fall back to the form body using `formValue()` (which respects `MaxBytesReader`). This supports third-party apps (Sonarr, Radarr, NZB360) that send parameters as form fields. Never use `r.FormValue()` directly in routing/auth — it triggers implicit `ParseMultipartForm` with Go's default 32MiB limit.

- **Always apply `http.MaxBytesReader` in middleware, not in individual handlers.** Create the `statusWriter` before `MaxBytesReader` so 413 responses are logged correctly. Use `maxUploadBytes` for `multipart/form-data`, `maxFormBytes` for everything else.

- **CSRF protection requires *both* `Origin` and `Sec-Fetch-Site` checks.** Cross-origin GET requests (via `<img>` or `<form method=GET>`) don't send an `Origin` header. Modern browsers send `Sec-Fetch-Site` instead. Block requests with `Sec-Fetch-Site: cross-site` or `cross-origin`.

- **Cookie-based auth on local-network services needs Referer/Origin validation.** Even `localhost` APIs are vulnerable to CSRF if the browser sends cookies automatically.

- **Cap all query `limit` parameters.** `limit=0` or `limit=999999999` loads unbounded data into memory. Enforce `defaultLimit` and `maxLimit` constants on all list/search endpoints.

- **Never use `os.ExpandEnv` on raw config file bytes.** It leaks host environment variables into config values. Expand only explicitly marked fields.

### 5. Resource Management

- **Track and close file descriptors for cancelled jobs.** The assembler holds open file handles per job. When a job is cancelled, `CancelJob` must close all associated FDs via a control message to the worker goroutine, or FDs leak indefinitely.

- **Use tombstone sets to reject late/duplicate messages.** After a file is completed and closed, late duplicate articles can re-open it, leaking FDs. Maintain a `completedFiles` set to reject them.

- **Add idle read deadlines on long-lived network sockets.** NNTP connections without read deadlines hang silently when the remote end disappears. Use `SetReadDeadline` and reset on each successful read.

- **SQLite per-connection pragmas belong in the DSN, not in post-connect hooks.** `journal_mode=WAL` and `busy_timeout` set via `_pragma=` in the DSN ensure every connection (including pool-created ones) has them from the start.

- **Batch large deletions to avoid unbounded transactions.** Deleting thousands of history records in a single `DELETE ... WHERE id IN (...)` can lock the database. Use chunked deletes with a reasonable batch size.

### 6. Code Complexity & Hotspot Refactoring

These rules are established from real hotspots targeted by repowise. They ensure cyclomatic complexity remains low, allowing standard linter checks and manual reviews to succeed easily.

- **Simplify Multi-Strategy Fallbacks**: When a method implements multiple fallback, validation, or conditional path strategies (like CSRF `isCrossOrigin` or complex auth logic), extract each strategy into its own focused helper (e.g. `isRefererCrossOrigin`). This drops the parent method's cyclomatic complexity (CCN) and enables targeted, isolated testing.

- **Consolidate Subsystem Boilerplates**: Avoid duplicating decoder setups, channel progress monitoring goroutines, and panic recovery setups across adjacent methods (like `GoVerify` and `GoRepair`). Consolidate these into unified helper methods (e.g. `newDecoderForDir`, `monitorProgress`). This ensures setup bug-fixes propagate globally.

- **Isolate Parsing & Normalization**: Keep primary decoding handlers (like config `decode`) concise. Extract error-type partitioning loops (like parsing `yaml.TypeError`) and struct normalizations (like assigning defaults or converting nil slices) into dedicated helpers.

- **Measure the result; preserve behavior exactly**: After a complexity-reduction extraction, run `gocyclo`/`gocognit` on the function and use the *measured* number — never an estimate — in any commit claim. Confirm the extraction is behavior-preserving: when hoisting shared statements out of sibling branches (e.g. `ParError = true`), verify every branch set them; when converting fall-through into return values, re-run `golangci-lint` (it may now flag `S1008`/`ifElseChain` that the original control flow hid).

### 7. Performance & Hot-Path Discipline

These rules were learned from production pprof profiling at 2 Gbps. The download pipeline processes ~330 articles/second; any per-article overhead multiplies fast.

#### Dispatch Loop (`internal/downloader/dispatch.go`)

- **Never iterate all articles to find pending work.** `ForEachUnfinishedArticle` uses `Pending` counters on `JobFile` and `PendingArticles` on `Job` to skip completed files/jobs in O(1). Any new code that walks articles must respect these counters — do not introduce new linear scans over the article slice.

- **Maintain pending counters on every state mutation.** When changing `art.Done`, `art.Emitted`, or `art.Failed`, you **must** update `job.Files[art.FileIdx].Pending` and `job.PendingArticles`. The pattern: decrement when an article leaves the pending state (Emitted, Done, or Failed for the first time); increment when it returns (ClearArticleEmitted). If a bulk operation makes incremental tracking fragile, call `job.recomputePending()` instead. See `MarkArticleEmitted`, `MarkArticlesDone`, `ClearAllEmitted` for canonical examples.

- **Cache per-server data once per dispatch pass, not per article.** `srv.Cfg()` returns a by-value struct copy. Calling it per-article per-server cost 0.69s in production profiles. The `serverCfgs []config.ServerConfig` slice in `dispatchPass` caches these. Any new per-server state queries (e.g., `Active()`, penalty checks) should follow the same pattern: snapshot once, pass the slice to `tryDispatch`.

- **Use 2-case selects (send/default), not 3-case (send/default/ctx.Done).** `runtime.selectgo` is significantly cheaper with 2 cases. Check `ctx.Err()` once before the server loop instead.

- **Defer heap allocations past early-exit checks.** `articleRequest` is allocated only after confirming the article is not already in-flight. Moving the alloc before the `inFlight` check wasted 1.9s/10s on objects immediately discarded.

#### Decoder (`internal/decoder/decoder.go`)

- **Use the LUT for `indexSpecial`, not `bytes.IndexAny`.** The 256-byte lookup table `specialLUT` identifies CR, LF, and `=` bytes in O(1) per byte. `bytes.IndexAny` performed O(N×M) string scanning and was the #1 decoder bottleneck. Do not replace the LUT with standard library functions.

- **`sub42Span` fuses copy + subtract into one pass.** The yEnc subtract-42 operation and the output append are combined into a single unrolled scalar loop for L1 cache efficiency. Do not split this back into `copy` + loop, and do not add bounds checks inside the inner loop (the capacity pre-check at the top ensures safety).

- **The LUT must be a compile-time constant array, not built in `init()`.** `init()` functions are forbidden by project convention, and the LUT values are known at compile time.

#### NNTP I/O (`internal/nntp/io.go`)

- **Pre-size `readDotStuffedBody`'s buffer to 768 KB.** Without this, `bytes.Buffer` grows incrementally, causing `memclrNoHeapPointers` (4.1%) and `memmove` (2.6%) to dominate the profile. The 768 KB value matches a typical yEnc article (~750 KB payload).

#### Queue (`internal/queue/`)

- **Use `job.articleByID()` for O(1) lookups, never linear scans.** The `artIdx` map is built lazily on first access. All queue mutation methods (`MarkArticlesDone`, `MarkArticleFailed`, `MarkArticleEmitted`, etc.) must use this, not nested `for fi / for ai` loops.

- **`JobArticle.FileIdx` is a back-pointer set by `recomputePending` / `buildArtIndex`.** It allows mutation methods to update per-file `Pending` without scanning for the parent file. This field is `json:"-"` (not persisted) — it must be recomputed on load.

- **All transient fields (`Pending`, `PendingArticles`, `FileIdx`, `artIdx`, `Emitted`) are `json:"-"`.** They are recomputed by `recomputePending()` on load and `ClearAllEmitted`. If you add new transient state, follow this pattern and ensure it is initialized in both `Add` and `Load`.

- **`ClearAllEmitted` is the self-healing reset.** It calls `recomputePending()` to rebuild all counters from ground truth. If you suspect counter drift during development, calling `recomputePending()` on a job will correct it. The `pending_test.go` `verifyPending` helper validates counters against ground truth.

#### General Performance Rules

- **Profile before optimizing.** Use `go tool pprof` with production workloads. Synthetic benchmarks miss real bottlenecks (e.g., `selectgo` overhead only appears under multi-server dispatch contention).

- **String map keys for message-IDs are expensive.** NNTP message-IDs are long strings (40-80 bytes); `aeshashbody` for these keys costs 1.15s/10s at 2 Gbps. Avoid adding new `map[string]` lookups in the per-article hot path. If you must, consider integer keys or pre-hashed values.

- **`sync.Pool` is usually not worth it in this codebase.** The `articleRequest` allocation (0.3s at steady-state) is small enough that pool overhead (Put/Get synchronization) would offset the savings. Only pool objects that are large and allocated at >10K/sec.
