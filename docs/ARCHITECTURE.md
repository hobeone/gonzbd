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
    - `config/`: YAML configuration schema, loading, validation, and atomic saves (marshal under RLock, then write lock-free).
    - `constants/`: Shared constants (priorities, statuses) used across packages.
    - `decoder/`: High-performance yEnc and UU decoding with LUT-based scanning and fused subtract-42 output.
    - `deobfuscate/`: Renames obfuscated filenames using NZB subject hints, PAR2 filenames, and extension detection.
    - `dirscanner/`: Watches a folder for new NZB files.
    - `downloader/`: The NNTP engine, handling server pools, connection management, and article dispatch with O(1) pending-article tracking.
    - `fsutil/`: File system utilities: path sanitization, atomic writes (temp+fsync+rename), symlink-safe containment checks, and cross-device move.
    - `history/`: Persistence layer for completed jobs using SQLite and `goose` migrations.
    - `nntp/`: Low-level NNTP protocol implementation with message-ID validation and bounded response reading.
    - `notifier/`: Dispatcher for user notifications (email, Apprise, scripts).
    - `nzb/`: NZB (XML) parsing and model definitions with input size limits.
    - `par2/`: PAR2 parity verification and repair tool wrapper with structured status parsing.
    - `postproc/`: Post-processing pipeline: repair, unpack, deobfuscate, script, finalize.
    - `queue/`: The active download queue and job state management with lazy article index and transient field recomputation.
    - `rarheader/`: RAR archive header parsing with filename sanitization.
    - `rss/`: RSS/Atom feed processing, filtering, and dedup with bounded fetch sizes.
    - `scheduler/`: Cron-like task scheduling.
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
5.  **Pipeline Bridge**: As articles are downloaded, they are sent through a `pipeline` goroutine (in `internal/app/pipeline.go`).
6.  **Decoding**: The `pipeline` decodes raw NNTP bodies (usually yEnc) using the `decoder`.
7.  **Assembly**: Decoded parts are handed to the `assembler`, which writes them to their exact byte offset in the target file using `pwrite`. This allows for out-of-order assembly as segments arrive.
8.  **Post-Processing**: Once all segments of a job are assembled, the job is handed to the `postproc` package, which runs a configurable chain of stages: repair (PAR2), unpack (RAR/7z/join), deobfuscate, user script, and finalize (move to complete directory). Sorting/renaming is intentionally not implemented — it is handled by external tools (Sonarr, Radarr, etc.).

### Concurrency Model

Unlike the original Python implementation's single-threaded selector loop, GoNZBD leverages Go's native concurrency:

- **Goroutine per Connection**: Each NNTP connection runs in its own goroutine, allowing for massive parallelism across servers.
- **Channels for Signaling**: Channels are used to stream `ArticleResult`s from the downloader to the pipeline and assembler.
- **Shared State Locking**: Hot-path state (the queue, job metadata) is protected by `sync.RWMutex`.

---

## Subsystem Deep Dives

### Queue & Job Management (`internal/queue`)

- **State Ownership**: The `Queue` owns the ordered list of active `Job`s and a map for fast ID-based lookup. All mutations are protected by a `sync.RWMutex`.
- **Downloader Signaling**: The `Queue` provides a `Notify()` channel (cap-1) that wakes up the `downloader` whenever new work is added or a job is resumed.
- **Batched Updates**: To minimize lock contention on high-speed connections, the `Queue` supports batched updates for article completions (`MarkArticlesDone`, `MarkArticlesFailed`).
- **O(1) Article Lookup**: The lazy `artIdx` map (built on first access via `articleByID`) provides O(1) article lookups by message-ID, avoiding linear scans. Transient fields like `Pending`, `PendingArticles`, `FileIdx`, and `Emitted` are `json:"-"` and recomputed on load via `recomputePending()`.
- **Persistence**: Active job state is persisted as gzipped JSON files in `admin/queue/jobs`.

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
| 1 | `repair` | `par2` | PAR2 verification and repair |
| 2 | `unpack` | `unpack` | RAR, 7z extraction and split file joining |
| 3 | `deobfuscate` | `deobfuscate` | Rename obfuscated files using NZB hints and PAR2 filenames |
| 4 | `script` | `postproc` | Run user-supplied post-processing script (see `docs/post-processing-scripts.md`) |
| 5 | `finalize` | `postproc` | Move job from incomplete to complete directory |

> **Note:** Sorting/renaming (TV, movie, date templates) is intentionally not implemented.
> This functionality is handled by external tools such as Sonarr, Radarr, and similar media managers.

A stage returning an error aborts the pipeline; subsequent stages are recorded as "Skipped" in the `StageLog`. The processor supports pause/resume and ensures idempotent start/stop via `sync.Once` guards.

### Persistence (`internal/history`, `internal/config`)

- **SQLite History**: Completed jobs are stored in `history.db`. The schema is maintained via `goose` migrations and is designed to be byte-for-byte compatible with the original Python implementation's history database.
- **YAML Configuration**: The application uses a YAML configuration (`gonzbd.yaml`). The `config` package handles loading, validation, and atomic saves (marshal under RLock, release lock, write to temp file, fsync, and rename). Environment variable expansion is supported within the YAML.

---

## Startup Sequence

When running in daemon mode (`--serve`), the application follows this sequence in `cmd/gonzbd/main.go`:

1.  **Configuration**: Loads `gonzbd.yaml` and resolves directory paths.
2.  **Logging**: Initializes structured logging (`log/slog`) with optional component-level filtering.
3.  **Locking**: Acquires a filesystem lock to ensure only one instance runs per admin directory.
4.  **Persistence**: Opens the SQLite history database and runs any pending migrations.
5.  **Application Core**: Constructs the `app.Application` orchestrator, which initializes the internal `queue`, `downloader`, `assembler`, and `postProcessor`.
6.  **Subsystem Start**: Invokes `application.Start()`, which boots the background goroutines for the pipeline, downloader, and post-processor.
7.  **API & Web**: Constructs the `api.Server` and `web.Handler`, binding them to a single HTTP listener.
8.  **Wait**: Blocks until a termination signal (SIGINT/SIGTERM) is received, then performs a graceful shutdown (stop producers → drain consumers → cancel context → wait → cleanup).

---

## API & Web Integration

The GoNZBD binary serves both the functional API and the modern web UI from a single port:

- **HTTP API**: Located at `/api`, it implements the SABnzbd legacy mode-dispatch system. Each `mode` (e.g., `queue`, `history`, `config`) is mapped to a handler with a specific `AccessLevel` (Open, Protected, Admin).
- **Error Logging**: All non-200 API responses are automatically logged. Status codes 500 and above are logged as errors, while other non-200 codes (4xx) are logged as warnings, including the explanation of what went wrong.
- **WebSockets**: Located at `/api/ws`, it provides real-time state updates to the UI using a broadcaster pattern.
- **Web UI**: The Svelte 5 SPA is embedded in the binary using `go:embed` (see `ui/embed.go`). The `internal/web` package handles serving these static assets and ensures SPA routing (fallback to `index.html`).

### Svelte 5 Development Caveats

When contributing to the UI, keep the following hard-won lessons in mind:
1. **Reactivity**: Do not use module-level $state in .svelte.ts stores for data that drives conditional rendering. Keep $state inside components for reliable re-renders during async operations.
2. **Dialogs**: bits-ui Dialog components may not fire onOpenChange when their state is bound from a parent. Use $effect to watch the open prop instead.
3. **Data Flow**: Child components should use onupdate callbacks rather than importing store functions directly to maintain explicit data flow.
