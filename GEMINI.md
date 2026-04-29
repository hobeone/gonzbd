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
- **Test (Integration):** `go test -tags=integration ./test/integration/...`
- **Lint:** `go vet ./...` and `golangci-lint run ./...`

## Development Mandates

### 1. Authoritative Documentation (Order of Precedence)
1.  **`GEMINI.md`** (This file) - Foundational mandates and project overview. Read this first for every session.
2.  **`CLAUDE.md`** - Strict development protocols, quality gates, and the mandatory "Decision Needed" escalation format.
3.  **`docs/ARCHITECTURE.md`** - Technical overview, architecture patterns, and subsystem deep dives. **Read this for architectural context.**
4.  **`docs/sabnzbd_spec.md`** - The source of truth for functional behavior. Defines protocols (NNTP), data formats (NZB, persistence), and API endpoint schemas. **Refer here for behavioral truth.**
5.  **`../sabnzbd/`** - The original Python implementation (external to this repo). Use for intent clarification, but do not transliterate.

### 2. Coding Standards
- **Idioms:** "Accept interfaces, return structs." Define interfaces at the consumer side.
- **Context:** Every blocking operation **must** accept `context.Context` as the first parameter.
- **Logging:** Pass `*slog.Logger` via constructors. **Never** use a package-level global logger. All loggers MUST be component-scoped using `.With("component", "name")` to support log filtering.
- **Errors:** Wrap errors with `fmt.Errorf("...: %w", err)`. Never use `%v` for errors.
- **Concurrency:** Prefer channels for signaling (e.g., `chan struct{}`) over `sync.Cond`. Use `sync.RWMutex` for hot-path memory state.
- **No hacks:** No `init()` functions for setup, no `panic` for control flow, and no `time.Sleep` in tests for synchronization.
- **Database Migrations:** All schema changes MUST be implemented as a new `goose` migration file in `internal/history/migrations/`. Never modify existing migration files.

### 3. Workflow & Quality Gates
- **Per-Step Commits:** Implement one step from `docs/golang_implementation.md` at a time.
- **Verification:** Before every commit, you **must** pass:
    ```bash
    ./scripts/run_tests.sh
    go vet ./...
    golangci-lint run ./...
    ```
- **Ambiguity Protocol:** If the spec or plan is unclear, investigate the Python source (`../sabnzbd/`), form an opinion, and present it to the user using the "Decision Needed" format defined in `CLAUDE.md`.

## Go Coding Guidelines
You are working in a Go codebase. Whenever you create, edit, or refactor a `.go` file, you MUST immediately use your shell tools to execute the following command to format the code and resolve imports:
`goimports -w <filename>`

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

- **All disk writes must be atomic: temp file → fsync → rename.** `os.WriteFile` truncates before writing; concurrent readers see partial/corrupt data. Use `os.CreateTemp` → write → `Sync()` → `Close()` → `os.Rename`. This pattern was missing in cache, queue, RSS dedup, and dirscanner state — all required the same fix.

- **Use `os.CreateTemp` for unique temp files, never a hardcoded `.tmp` suffix.** Concurrent writes to `path + ".tmp"` corrupt state files. RSS dedup and dirscanner state both had this bug.

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
