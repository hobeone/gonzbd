# GEMINI.md - Project Context & Instructions

This document provides foundational context and instructional mandates for the GoNZBD project. It must be read and followed by any AI agent working on this codebase.

## Project Overview
GoNZBD is a high-performance Go reimplementation of [SABnzbd](https://sabnzbd.org), the automated Usenet binary newsreader. It targets fresh installations and is not a drop-in replacement for the Python version.

- **Status:** Core backend download pipeline and legacy mode-dispatch API (`/api?mode=...`) are functional. The Glitter web UI port (Phase 12) is the current active focus.
- **Main Technologies:**
    - **Language:** Go 1.25+
    - **Configuration:** YAML (`gopkg.in/yaml.v3`)
    - **Persistence:** SQLite (`modernc.org/sqlite`, pure Go) for history; JSON+gzip for queue state.
    - **Logging:** Structured logging via `log/slog`.
    - **Concurrency:** Idiomatic goroutines + channels; `sync.RWMutex` for shared state.

## Architecture
- `cmd/gonzbd/`: Entry point, flag parsing, and application orchestration.
- `internal/`: Core packages (API, app, downloader, queue, nzb, assembler, decoder, etc.).
- `docs/`: Critical design documents (`ARCHITECTURE.md`, `sabnzbd_spec.md`).
- `test/`: Integration tests, fixtures, and a mock NNTP server.

## Building and Running
- **Build:** `go build ./cmd/gonzbd`
- **Run (Daemon):** `./gonzbd --config ~/.config/gonzbd/gonzbd.yaml --serve`
- **One-shot Download:** `./gonzbd --config <path> --nzb <path>`
- **Test (Unit):** `go test ./...`
- **Test (Race):** `go test -race ./...` (Required for CI/commits)
- **Test (Integration):** `go test -v -tags=integration ./test/integration/...` (requires par2, rar, unrar, 7z)
- **Test (UI/Playwright):** `go test -v -tags=uitest ./test/uitest/...` (requires pre-built UI + Playwright Chromium)
- **Test (E2E):** `go test -tags=e2e -timeout=10m ./test/e2e/` (requires live Usenet server)
- **Test (Config contract):** `go test ./internal/config/ -run 'TestUI|TestAllFlat'`
- **Lint:** `go vet ./...` and `golangci-lint run ./...`

> **See `docs/TESTING.md` for the full testing guide** — build tags, required
> tools, per-file descriptions, and a decision guide for which suites to run
> based on the area of code being changed.


## Development Mandates

### 1. Authoritative Documentation (Order of Precedence)
1.  **`GEMINI.md`** (This file) - Foundational mandates and project overview. Read this first for every session.
2.  **`CLAUDE.md`** - Strict development protocols, quality gates, and the mandatory "Decision Needed" escalation format.
3.  **`docs/ARCHITECTURE.md`** - Technical overview, architecture patterns, and subsystem deep dives. **Read this for architectural context.**
4.  **`docs/TESTING.md`** - Comprehensive testing guide. Covers all test suites (unit, integration, E2E, contract), build tags, required tools, and when to run each. **Read this before running or modifying tests.**
5.  **`docs/sabnzbd_spec.md`** - The source of truth for functional behavior. Defines protocols (NNTP), data formats (NZB, persistence), and API endpoint schemas. **Refer here for behavioral truth.**
6.  **`../sabnzbd/`** - The original Python implementation (external to this repo). Use for intent clarification, but do not transliterate.

### 2. Coding Standards
- **Idioms:** "Accept interfaces, return structs." Define interfaces at the consumer side.
- **Context:** Every blocking operation **must** accept `context.Context` as the first parameter.
- **Logging:** Pass `*slog.Logger` via constructors. **Never** use a package-level global logger. All loggers MUST be component-scoped using `.With("component", "name")` to support log filtering.
- **Errors:** Wrap errors with `fmt.Errorf("...: %w", err)`. Never use `%v` for errors.
- **Concurrency:** Prefer channels for signaling (e.g., `chan struct{}`) over `sync.Cond`. Use `sync.RWMutex` for hot-path memory state.
- **No hacks:** No `init()` functions for setup, no `panic` for control flow, and no `time.Sleep` in tests for synchronization.
- **Database Migrations:** All schema changes MUST be implemented as a new `goose` migration file in `internal/history/migrations/`. Never modify existing migration files.
- **Config Documentation:** The root `gonzbd.yaml` contains inline comments above every directive documenting its purpose, valid values, and important considerations. When adding, renaming, or removing config fields in `internal/config/`, you MUST update the corresponding comments in `gonzbd.yaml` and `test/fixtures/gonzbd.yaml` to stay in sync. Also update `docs/sabnzbd_spec.md` §9.x tables.
- **Config ↔ UI Contract Test:** `internal/config/ui_contract_test.go` contains `TestUIKeywordsAreValidConfigTags`, which is the canonical list of every `keyword=` prop used in Svelte config components. **This file must be kept in sync with both the Go config structs and the Svelte UI.** Specifically:
  - When you **add a new `keyword=` prop** to any `ConfigInput`, `ConfigSwitch`, or `ConfigTextarea` in `ui/src/lib/components/config/`, add a matching entry to `uiKeywords` in `ui_contract_test.go`.
  - When you **remove or rename a Svelte keyword**, remove or update the corresponding entry.
  - When you **rename or remove a Go config field** (changing its `json:` tag), the test `TestAllFlatConfigTagsAreSettable` will catch the breakage automatically — but you must also update any matching Svelte `keyword=` props.
  - Run `go test ./internal/config/ -run 'TestUI|TestAllFlat'` to verify after any config or UI change.

### 3. Workflow & Quality Gates
- **Per-Step Commits:** Implement one step from `docs/golang_implementation.md` at a time.
- **Verification:** Before every commit, you **must** pass:
    ```bash
    go fix ./...
    ./scripts/run_tests.sh
    go vet ./...
    golangci-lint run ./...
    ```
- **Ambiguity Protocol:** If the spec or plan is unclear, investigate the Python source (`../sabnzbd/`), form an opinion, and present it to the user using the "Decision Needed" format defined in `CLAUDE.md`.

## Go Coding Guidelines
You are working in a Go codebase. Whenever you create, edit, or refactor a `.go` file, you MUST immediately use your shell tools to execute the following commands:
1. Format the code and resolve imports: `goimports -w <filename>`
2. Apply Go toolchain modernizations: `go fix ./...` — this automatically adopts new language features (e.g., `min`/`max` builtins, `slices.Contains`, `wg.Go()`) and keeps the codebase current with the Go version declared in `go.mod`.

## Key File Locations
- **API Handlers:** `internal/api/`
- **Download Engine:** `internal/downloader/`
- **Queue Logic:** `internal/queue/`
- **Web UI (Svelte SPA):** `ui/` — Svelte 5 + TypeScript + Vite, embedded via `//go:embed all:dist` in `ui/embed.go`
- **SPA Handler:** `internal/web/` — serves embedded dist with SPA catch-all fallback to index.html
- **Configuration Schema:** `internal/config/`

## Svelte 5 UI Gotchas

These are hard-won lessons that **must** be followed when editing the Svelte SPA in `ui/`:

1. **Do not use module-level $state in .svelte.ts stores for data that drives conditional rendering.** Mutations inside async functions in external store modules do not reliably trigger re-renders in consuming components. Instead, declare $state inside the component and use .then() chains for fetches. See SettingsDialog.svelte for the working pattern.

2. **bits-ui Dialog onOpenChange does not fire when bind:open is set by the parent.** Use a $effect watching the $open prop to trigger side effects (like data loading) when a dialog opens.

3. **Child components (ConfigInput, ConfigSwitch) receive onupdate callbacks** instead of importing store functions directly. This keeps data flow explicit and avoids the store reactivity issue.

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

### 7. Code Complexity & Hotspot Refactoring

These rules are established from real hotspots targeted by repowise. They ensure cyclomatic complexity remains low, allowing standard linter checks and manual reviews to succeed easily.

- **Simplify Multi-Strategy Fallbacks**: When a method implements multiple fallback, validation, or conditional path strategies (like CSRF `isCrossOrigin` or complex auth logic), extract each strategy into its own focused helper (e.g. `isRefererCrossOrigin`). This drops the parent method's cyclomatic complexity (CCN) and enables targeted, isolated testing.

- **Consolidate Subsystem Boilerplates**: Avoid duplicating decoder setups, channel progress monitoring goroutines, and panic recovery setups across adjacent methods (like `GoVerify` and `GoRepair`). Consolidate these into unified helper methods (e.g. `newDecoderForDir`, `monitorProgress`). This ensures setup bug-fixes propagate globally.

- **Isolate Parsing & Normalization**: Keep primary decoding handlers (like config `decode`) concise. Extract error-type partitioning loops (like parsing `yaml.TypeError`) and struct normalizations (like assigning defaults or converting nil slices) into dedicated helpers.

- **Measure the result; preserve behavior exactly**: After a complexity-reduction extraction, run `gocyclo`/`gocognit` on the function and use the *measured* number — never an estimate — in any commit claim. Confirm the extraction is behavior-preserving: when hoisting shared statements out of sibling branches (e.g. `ParError = true`), verify every branch set them; when converting fall-through into return values, re-run `golangci-lint` (it may now flag `S1008`/`ifElseChain` that the original control flow hid).

### 6. Performance & Hot-Path Discipline

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

## Standard Go Development Mandates

### Tooling Setup

```bash
# Install goimports if not present
go install golang.org/x/tools/cmd/goimports@latest

# Install golangci-lint if not present (see https://golangci-lint.run/welcome/install/)
```

### Per-File Workflow (after every .go file edit)

```bash
goimports -w <file>   # format + resolve imports
go fix ./...          # adopt new language features automatically
go build ./...        # verify it compiles
```

### Quality Gate (before every commit)

```bash
goimports -w .
go fix ./...
go vet ./...
go test -race ./...
golangci-lint run ./...
```

All five must pass. Do not commit with failing tests, vet errors, or lint warnings.

### Coding Standards (Canonical Mandates)

- **Idioms:** "Accept interfaces, return structs." Define interfaces at the consumer side.
- **Context:** Every blocking or cancellable operation **must** accept `context.Context` as the first parameter.
- **Errors:** Wrap with `fmt.Errorf("component: ...: %w", err)`. Never use `%v` for errors that will be inspected.
- **No hacks:** No `init()` for setup. No `panic` for control flow. No `time.Sleep` in tests — use channels or `sync.WaitGroup`.
- **Standard library first:** Prefer `slices`, `maps`, `errors.Is/As`, `min`/`max` builtins over custom helpers or reflection.

### Concurrency & Locking (Canonical Mandates)

- **Never hold a mutex during I/O.** Snapshot under the lock, release, then do I/O.
- **Always `defer mu.Unlock()`.** Only exception: intentional snapshot-then-release, marked with `// --- no lock held below this line ---`.
- **Every `select` must watch `ctx.Done()`.** Goroutines blocked without a context escape route leak forever.
- **Use `sync.Once` or `CompareAndSwap` for idempotent shutdown.** Prevents double-close panics.

### Commit Convention

All commits must follow [Conventional Commits 1.0.0](https://www.conventionalcommits.org/):

```
<type>[optional scope]: <description>
```

| Type | When to use |
|------|-------------|
| `feat` | New user-visible capability |
| `fix` | Bug patch |
| `perf` | Performance improvement with benchmark evidence |
| `refactor` | Code restructuring, no behavior change |
| `test` | Adding or improving tests |
| `docs` | Documentation only |
| `chore` | Build, CI, dependency updates |

Append `!` or add `BREAKING CHANGE:` footer for any public API or wire-format change.

**Commit hygiene (mandatory — learned from real mistakes):**

- **The subject MUST match the diff.** Before committing, run `git diff --cached --stat` and confirm the scope and changed files match the message. A commit subjected `refactor(api): …` that actually edits `internal/assembler/` is a defect: it hides the real change from `git log <file>` and `git bisect`.
- **One logical change per commit.** When several edits pile up in the working tree, never `git add -A` and let commits be sliced by timing. Stage per logical unit with `git add <paths>` and verify each commit holds only that unit. Two unrelated extractions are two commits.
- **Quantitative claims MUST be measured.** Do not write "drops CCN from 24 to <5" unless you ran `gocyclo`/`gocognit` on the result. Extraction lowers the parent's complexity by construction, but the magnitude is not guessable — a real case landed at 12, not the claimed <5. State the measured number or drop the claim.
- **Re-run `golangci-lint` on the actual final diff.** Control-flow refactors (fall-through `return` → boolean returns) can introduce *new* findings (`S1008`, `ifElseChain`) absent from the original. The gate must run against the code you are about to commit, not an assumption about it.
