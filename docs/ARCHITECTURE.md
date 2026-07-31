# GoNZBD Architecture & Design

This document provides a detailed overview of the architecture, design, and implementation of GoNZBD, a high-performance Go reimplementation of [SABnzbd](https://sabnzbd.org).

## Project Overview

GoNZBD is designed as a long-running daemon that automates the Usenet download lifecycle: ingestion (NZB files), downloading (NNTP), decoding (yEnc), assembly, and post-processing. It emphasizes high performance, modern Go idioms, and a self-contained binary (including the web UI).

---

## File and Directory Structure

The project follows a standard Go project layout:

- `cmd/gonzbd/`: The application entry point. Handles CLI flags, configuration loading, and service orchestration.
- `internal/`: Core application logic, restricted from external import.
    - `api/`: Implementation of the legacy SABnzbd HTTP API (`/api?mode=...`) and modern WebSocket events.
    - `app/`: The central orchestrator (`Application`) and the download pipeline bridge.
    - `assembler/`: Logic for writing decoded article parts to disk using `pwrite`, with a write cache for coalescing contiguous runs.
    - `bpsmeter/`: Bandwidth statistics and speed limiting.
    - `cmdutil/`: Helpers for building and validating external command invocations (nice/ionice wrapping, extra-param parsing).
    - `config/`: YAML configuration schema, loading, validation, and atomic saves (marshal under RLock, then write lock-free).
    - `constants/`: Shared constants (priorities, statuses) used across packages.
    - `crc32util/`: CRC-32 utilities used by the quick-check stage.
    - `decoder/`: High-performance yEnc and UU decoding with LUT-based scanning and fused subtract-42 output.
    - `deobfuscate/`: Renames obfuscated filenames using NZB subject hints, PAR2 filenames, and extension detection.
    - `directunpack/`: In-flight RAR extraction that runs in parallel with downloading.
    - `dirscanner/`: Watches a folder for new NZB files.
    - `downloader/`: The NNTP engine, handling server pools, connection management, and article dispatch with O(1) pending-article tracking.
    - `fsutil/`: File system utilities: path sanitization, atomic writes (temp+fsync+rename), symlink-safe containment checks, and cross-device move.
    - `history/`: Persistence layer for completed jobs using SQLite and `goose` migrations.
    - `humanfmt/`: Human-readable formatting helpers (sizes, durations) shared across packages.
    - `nntp/`: Low-level NNTP protocol implementation with message-ID validation and bounded response reading.
    - `notifier/`: Dispatcher for user notifications (email, Apprise, scripts).
    - `nzb/`: NZB (XML) parsing and model definitions with input size limits.
    - `par2/`: PAR2 parity verification and repair tool wrapper with structured status parsing.
    - `postproc/`: Post-processing pipeline: quickcheck, repair, unpack, deobfuscate, finalize, script, and supporting stages.
    - `queue/`: The active download queue and job state management with lazy article index and transient field recomputation.
    - `rarheader/`: RAR archive header parsing with filename sanitization.
    - `telemetry/`: Runtime metrics collection and export.
    - `types/`: Shared type definitions used across packages.
    - `unpack/`: Archive extraction wrappers for RAR, 7z, and split file joining.
    - `urlgrabber/`: Fetches NZB files from URLs with SSRF protection (private IP blocking, redirect validation).
    - `web/`: Glue code for serving the embedded web UI and integrating with the API.
- `ui/`: Svelte 5 + TypeScript + Vite frontend.
- `scripts/`: Build, test, and development helper scripts (`run_tests.sh`, `run_fuzz.sh`, etc.).
- `docs/`: Technical specifications, architectural documentation, and user guides.
- `test/`: Integration tests, E2E tests, UI (Playwright) tests, and a mock NNTP server.

---

## Architecture & Data Flow

### The Download Pipeline

Data flows through the system in a multi-stage pipeline designed for maximum concurrency and disk I/O efficiency:

1.  **Ingestion**: NZB files are ingested via the watched folder (`dirscanner`), URL fetching (`urlgrabber`), or direct API upload.
2.  **Parsing**: The `nzb` package parses the XML into a `Job` which is added to the `queue`.
3.  **Downloader**: The `downloader` picks up jobs from the `queue`. It manages a pool of `nntp` connections across multiple servers.
4.  **Fetching**: Each connection goroutine fetches articles (segments) from Usenet servers.
5.  **Decoding**: The connection goroutine decodes raw NNTP bodies (usually yEnc or UU-encoded) using the `decoder` package concurrently to ensure maximum overlap.
6.  **Pipeline Bridge**: As decoded article parts are emitted, they are routed through a `pipeline` goroutine (in `internal/app/pipeline.go`) which fans them out.
7.  **Assembly**: The `pipeline` hands decoded parts to the `assembler`, which writes them to their exact byte offset in the target file using `pwrite`. This allows for out-of-order assembly as segments arrive.
8.  **On-Demand PAR2 Gate** (optional, default on): when a job's non-deferred files are all assembled, if PAR2 recovery volumes were held back (see *On-Demand PAR2* below), the downloaded data is CRC-verified against the PAR2 index *before* post-processing. Clean ⇒ the job finalizes and the recovery volumes are never downloaded; damaged ⇒ the volumes are un-deferred and fetched via the normal download path, then completion fires again and proceeds to post-processing.
9.  **Post-Processing**: Once all segments of a job are assembled, the job is handed to the `postproc` package, which runs a configurable chain of stages: repair (PAR2), unpack (RAR/7z/join), deobfuscate, user script, and finalize (move to complete directory). Sorting/renaming is intentionally not implemented — it is handled by external tools (Sonarr, Radarr, etc.).

### Concurrency Model

Unlike the original Python implementation's single-threaded selector loop, GoNZBD leverages Go's native concurrency:

- **Goroutine per Connection**: Each NNTP connection runs a persistent worker goroutine (`connWorker`) that manages the socket state and handles pipelined fetches and concurrent decodes via sub-goroutines (bounded by configuration settings), allowing for massive parallelism across servers.
- **Channels for Signaling**: Channels are used to stream `ArticleResult`s from the downloader to the pipeline and assembler.
- **Shared State Locking**: Hot-path state (the queue, job metadata) is protected by `sync.RWMutex`.

---

## Subsystem Deep Dives

### Queue & Job Management (`internal/queue`)

- **State Ownership**: The `Queue` owns the ordered list of active `Job`s and a map for fast ID-based lookup. All mutations are protected by a `sync.RWMutex`.
- **Downloader Signaling**: The `Queue` provides a `Notify()` channel (cap-1) that wakes up the `downloader` whenever new work is added or a job is resumed.
- **Batched Updates**: To minimize lock contention on high-speed connections, the `Queue` supports batched updates for article completions (`MarkArticlesDone`, `MarkArticlesFailed`).
- **Manifest/Progress split**: a `Job` holds an immutable `Manifest` (parsed-once article/file structure — subjects, byte counts, the flat global article arrays) and a mutable `JobProgress` (per-article done/failed/emitted state, per-file assembly bookkeeping, job-level counters). `Manifest` is safe to share by reference across every `Snapshot`/`SnapshotJob` clone since nothing mutates it after construction; `JobProgress` is deep-copied per clone. External packages reach both only through `Job.Manifest()`/`Job.Progress()` accessor methods, and neither type exposes a mutating exported method. Those accessors take a per-`Job` `residencyMu` rather than reading the fields directly: `Get`/`List` hand out `*Job` pointers that alias queue storage, and lazy eviction and promotion reassign both fields under `q.mu`, so an unsynchronized read would race them. `residencyMu` covers *which* `Manifest`/`JobProgress` a job points to and nothing else — `Manifest` contents are immutable, but `JobProgress` fields are mutated in place under `q.mu` and remain unsynchronized for readers outside the package.
- **O(1) Article Lookup**: `Manifest`'s lazy `messageIDIndex` (built on first access via `articleIndexByID`) provides O(1) article lookups by message-ID, avoiding linear scans. Derived counters on `JobProgress` (`Pending`, `PendingArticles`) are excluded from JSON and recomputed on load via `recompute()`; the transient `Emitted` bit is likewise excluded so a restart re-dispatches articles whose bytes hadn't reached stable storage before the crash.
- **ActiveSet & Promotion Loop**: `ActiveSet` manages in-memory resident `(Manifest, JobProgress)` pairs exclusively for active/processing jobs. Pending jobs are stored in SQLite (`history.db`) with zero resident manifests in RAM. The promotion loop (`Pending -> Active`) is single-caller bounded by `MaxActiveJobs` (default 4).
- **Persistence**: Active queue state and job metadata are persisted in SQLite, in the `jobs`, `job_files` and `queue_meta` tables of `<AdminDir>/history.db` — the same database file history uses, not a separate one. `<AdminDir>/queue/` holds only the immutable job manifests, at `<AdminDir>/queue/manifests/<id>.json.gz`.

### On-Demand PAR2 (`internal/queue`, `internal/app`, `internal/par2`)

To save bandwidth, PAR2 **recovery volumes** (`*.volNNN+MM.par2`) are downloaded only when repair is actually needed. Controlled by `downloads.on_demand_par2` (default **on**). Design (full detail in `docs/on-demand-par2-plan.md`):

- **Classification & deferral**: at add-time `NewJob` flags recovery volumes (`Manifest.FileIsPar2Recovery`, via `par2.IsRecoveryVolume`) and marks them `Deferred` on `JobProgress`. The PAR2 **index** file (no `volNNN+MM` suffix) is never deferred — it carries the per-file checksums used to verify. Both are persisted.
- **Skipped during download**: deferred files have `Pending == 0`, are skipped by `ForEachUnfinishedArticle`, and do not block `IsComplete()` — so a job is "downloaded" once its non-deferred files finish.
- **Decision = existing CRC oracle**: at download-complete (`handleFileComplete`), `par2NeedsRecovery` runs the same `par2.VerifyCRCs` check as the QuickCheck stage against the on-disk index. Repair is needed iff `Mismatched + NoCRC + Unverified > 0`; a missing/unusable index falls back to fetching all volumes.
- **Re-activation is download→download, not postproc→download**: on damage, `UndeferRecoveryVolumes(jobID, fileIdxs)` clears `Deferred`, recomputes counters, sets `Par2Recovered` (guards re-firing), and wakes the dispatcher. The job becomes incomplete again and the *normal* download path fetches the volumes — no back-edge from post-processing.
- **Early un-defer**: a permanent data-article failure during download releases the volumes immediately (`MarkArticlesFailed`), shrinking the window in which the volumes themselves could age off the server.
- **Phasing**: Phase 1 fetches *all* recovery volumes on damage (the `fileIdxs` selection arg is the seam for Phase 2's block-exact subset selection).

### NNTP & Downloader (`internal/nntp`, `internal/downloader`)

- **Connection Management**: The `nntp` package implements the raw NNTP protocol. A `nntp.Conn` represents a single socket. The `downloader` manages pools of these connections per server.
- **Message-ID Validation**: All message-IDs are validated before use to prevent NNTP command injection (CR, LF, NUL, `>` rejected).
- **Bounded Reading**: Response lines are capped at 2KB and article bodies at 10MB to prevent OOM from malicious servers.
- **Pipelining**: The system supports NNTP pipelining (multiple in-flight requests per socket) to maximize throughput over high-latency connections.
- **Error Classification**: NNTP status codes are mapped to Go sentinel errors (`ErrNoArticle`, `ErrAuthRejected`, etc.), allowing for robust retry and penalty logic.
- **Dispatch Optimization**: The dispatch loop uses cached server configs per pass and 2-case selects to minimize overhead at ~330 articles/second throughput.

### Decoder & Assembler (`internal/decoder`, `internal/assembler`)

- **High-Performance Decoding**: The `decoder` provides yEnc and UU decoding. The yEnc implementation uses a 256-byte lookup table (`specialLUT`) via `indexSpecial` to find CR/LF/`=` bytes in O(1) per byte, and a fused `sub42Span` function that combines the subtract-42 transform with the output copy in a single pass for L1 cache efficiency. Both are capped at 10MB to reject oversized payloads.
- **Out-of-Order Assembly**: The `assembler` uses a single worker goroutine and `pwrite` (via `WriteAt` in Go) to write articles directly to their target offsets. This avoids the need for a sequential assembly step and handles articles arriving in any order.
- **Write Cache**: A write cache coalesces contiguous articles into single `WriteAt` calls, reducing syscall overhead. Under memory pressure (>90% of limit), the cache force-flushes the largest pending file.
- **Batching**: Successful writes are batched and periodically flushed to the queue to minimize locking overhead on high-speed connections.

### Post-Processing (`internal/postproc`)

Post-processing runs a chain of `Stage` implementations in order for each completed job:

| Order | Stage | Package | Description |
|-------|-------|---------|-------------|
| 1 | `quickcheck` | `postproc` | CRC-verify assembled files against PAR2 metadata; relocate flat files into expected subdirectories |
| 2 | `repair` | `par2` | PAR2 verification and repair (skipped when quickcheck passes) |
| 3 | `unpack` | `unpack` | RAR, 7z extraction and split file joining |
| 4 | `sample_cleanup` | `postproc` | Delete sample video files (when enabled) |
| 5 | `par2names` | `postproc` | Recover original filenames from PAR2 metadata |
| 6 | `par2_cleanup` | `postproc` | Delete `.par2` files after repair/rename (when enabled) |
| 7 | `deobfuscate` | `deobfuscate` | Rename obfuscated files using NZB hints and PAR2 filenames |
| 8 | `extension_cleanup` | `postproc` | Delete files matching the user's cleanup extension list |
| 9 | `finalize` | `postproc` | Move job from incomplete to complete directory |
| 10 | `cleanup` | `postproc` | Remove `__ADMIN__` sidecar directory from the job folder |
| 11 | `script` | `postproc` | Run user-supplied post-processing script (see `docs/post-processing-scripts.md`) |

> **Note:** Sorting/renaming (TV, movie, date templates) is intentionally not implemented.
> This functionality is handled by external tools such as Sonarr, Radarr, and similar media managers.

Stage errors are recorded in the `StageLog` but do **not** abort the pipeline — subsequent stages still run. Each stage self-gates based on job flags (`ParError`, `UnpackError`, `FailMsg`) to decide whether to skip when a prior stage has failed. The only reason to abort remaining stages is context cancellation, either daemon shutdown or a single job being cancelled mid-processing (`Cancel`). The processor has no pause/resume control of its own — downloads can be paused via `pause`/`resume`, which transitively stalls the post-processing queue since no new jobs finish downloading.

#### External subprocess containment (`internal/cmdutil`, `internal/unpack`)

External `unrar`/`7z` subprocesses get two independent layers of containment,
enforced differently depending on how they run:

1. **OS-level sandboxing** (`internal/cmdutil.BuildSandboxedCommand`): wraps
   the subprocess with `bwrap`, restricting filesystem writes to the job's
   directory at the kernel level. Linux is the only platform with a working
   backend — the Linux-only constraint on `strict_sandbox: true` is enforced
   centrally by `internal/config.PostProcConfig.validate()`, which runs on
   every config load and on every `Config.Set()` call (see
   `internal/config/set.go`). This rejects an unsupported combination before
   it is ever persisted or applied, rather than deferring the failure to the
   first extraction attempt. On Linux, `strict_sandbox: true` makes
   `BuildSandboxedCommand` return `ErrSandboxUnavailable` (aborting
   extraction) if `bwrap` can't be found; `false` falls back to running the
   subprocess unwrapped.
2. **Post-extraction path containment** (`stage_unpack.go`, always on,
   independent of `strict_sandbox`): after extraction, every produced path is
   checked against the job's output directory; anything outside it is deleted
   (only paths that lie inside `outDir` are ever removed) and the job is
   flagged with a containment-violation error.

**In the official Docker image, layer 1 is effectively disabled by default**
(`bwrap` isn't installed, and `Default()` seeds `strict_sandbox: false` for
brand-new container configs — see `internal/config/defaults.go`'s
`runningInDockerImage`). This isn't an oversight: `bwrap` needs to create an
unprivileged user+mount namespace, which a normal (non-`--privileged`)
container's default seccomp/AppArmor profile blocks. Installing `bwrap` there
doesn't restore sandboxing — it just makes `bwrap` itself fail at exec time
(after `wrapSandbox` has already succeeded, since it only checks that the
binary exists in `PATH`, not that it can actually create a namespace), which
silently breaks every extraction regardless of `strict_sandbox`. Docker's own
container boundary plus layer 2 (path containment, unaffected by any of this)
are the containment model actually in effect for the shipped image.

### Persistence (`internal/history`, `internal/config`)

- **SQLite History**: Completed jobs are stored in `history.db`. The schema is maintained via `goose` migrations and is designed to be byte-for-byte compatible with the original Python implementation's history database.
- **YAML Configuration**: The application uses a YAML configuration (`gonzbd.yaml`). The `config` package handles loading, validation, and atomic saves (marshal under RLock, release lock, write to temp file, fsync, and rename). Environment variable expansion (`$VAR`, `${VAR}`) and `~` home-directory expansion are supported in **path-typed fields only** (e.g., `download_dir`, `admin_dir`, `script_dir`); non-path values (passwords, API keys) are intentionally left unexpanded to avoid corrupting values that contain `$`.

---

## Startup Sequence

When running in daemon mode (`--serve`), the application follows this sequence in `cmd/gonzbd/main.go`:

1.  **Configuration**: Loads `gonzbd.yaml` and resolves directory paths.
2.  **Directories**: Creates download, complete, admin, and (optionally) watch directories on disk.
3.  **Logging**: Initializes structured logging (`log/slog`) with optional component-level filtering.
4.  **Locking**: Acquires a filesystem lock to ensure only one instance runs per admin directory.
5.  **Persistence**: Opens the SQLite history database (`history.db`) and runs any pending `goose` migrations.
6.  **Application Core**: Constructs the `app.Application` orchestrator, which initializes the internal `queue`, `downloader`, `assembler`, and `postProcessor`.
7.  **Subsystem Start**: Invokes `application.Start()`, which boots the background goroutines for the pipeline, downloader, and post-processor.
8.  **Ancillary services**: Starts the bandwidth meter (`bpsmeter`), notifier, and directory scanner (`dirscanner`).
9.  **API & Web**: Constructs the `api.Server` and `web.Handler`, binding them to a single HTTP listener (and optionally a separate HTTPS listener).
10. **Wait**: Blocks until a termination signal (SIGINT/SIGTERM) is received, then performs a graceful shutdown (stop producers → drain consumers → cancel context → wait → cleanup).

---

## API & Web Integration

The GoNZBD binary serves both the functional API and the modern web UI from a single port:

- **HTTP API**: Located at `/api`, it implements the SABnzbd legacy mode-dispatch system. Each `mode` (e.g., `queue`, `history`, `config`) is mapped to a handler with a specific `AccessLevel` (Open, Protected, Admin).
- **Error Logging**: All non-200 API responses are automatically logged. Status codes 500 and above are logged as errors, while other non-200 codes (4xx) are logged as warnings, including the explanation of what went wrong.
- **WebSockets**: Located at `/api/ws`, it provides real-time state updates to the UI using a broadcaster pattern.
- **Web UI**: The Svelte 5 SPA is embedded in the binary using `go:embed` (see `ui/embed.go`). The `internal/web` package handles serving these static assets and ensures SPA routing (fallback to `index.html`).
- **Authentication**: Security is enforced at `/api` and `/api/ws` using either the API key (passed via `?apikey=` parameter or `X-Api-Key` header) or the NZB key (for upload-only modes). For Web UI browser-based requests, the backend sets a secure `HttpOnly` session cookie (`gonzbd_apikey`) automatically upon API verification during navigation. Local Basic Authentication (username/password configurations) and localhost bypasses are deprecated and removed from the core application, deferring ingress auth to front-end reverse proxies.

### Svelte 5 Development Caveats

When contributing to the UI, keep the following hard-won lessons in mind:
1. **Reactivity**: Do not use module-level $state in .svelte.ts stores for data that drives conditional rendering. Keep $state inside components for reliable re-renders during async operations.
2. **Dialogs**: all dialogs use `ui/src/lib/components/ui/Modal.svelte`, a wrapper over the native `<dialog>` element, controlled by the parent via `bind:open`. Run open-time logic with a $effect watching the open prop; there is no open-change callback. See `docs/svelte-gotchas.md`.
3. **Data Flow**: Child components should use onupdate callbacks rather than importing store functions directly to maintain explicit data flow.
