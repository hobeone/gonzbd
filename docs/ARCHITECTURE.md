# SABnzbd-Go Architecture & Design

This document provides a detailed overview of the architecture, design, and implementation of `sabnzbd-go`, a high-performance Go reimplementation of [SABnzbd](https://sabnzbd.org).

## Project Overview

`sabnzbd-go` is designed as a long-running daemon that automates the Usenet download lifecycle: ingestion (NZB files), downloading (NNTP), decoding (yEnc), assembly, and post-processing. It emphasizes high performance, modern Go idioms, and a self-contained binary (including the web UI).

---

## File and Directory Structure

The project follows a standard Go project layout:

- `cmd/sabnzbd/`: The application entry point. Handles CLI flags, configuration loading, and service orchestration.
- `internal/`: Core application logic, restricted from external import.
    - `api/`: Implementation of the legacy SABnzbd HTTP API (`/api?mode=...`) and modern WebSocket events.
    - `app/`: The central orchestrator (`Application`) and the download pipeline bridge.
    - `assembler/`: Logic for writing decoded article parts to disk using `pwrite`.
    - `bpsmeter/`: Bandwidth statistics and speed limiting.
    - `config/`: YAML configuration schema, loading, and validation.
    - `decoder/`: High-performance yEnc and UU decoding.
    - `dirscanner/`: Watches a folder for new NZB files.
    - `downloader/`: The NNTP engine, handling server pools, connection management, and article fetching.
    - `history/`: Persistence layer for completed jobs using SQLite and `goose` migrations.
    - `i18n/`: Internationalization support (catalog-based).
    - `nntp/`: Low-level NNTP protocol implementation.
    - `notifier/`: Dispatcher for user notifications (email, scripts, etc.).
    - `nzb/`: NZB (XML) parsing and model definitions.
    - `postproc/`: Post-processing pipeline (repair, unpack, finalize).
    - `queue/`: The active download queue and job state management.
    - `rss/`: RSS feed processing and filtering.
    - `scheduler/`: Cron-like task scheduling.
    - `urlgrabber/`: Fetches NZB files from URLs.
    - `web/`: Glue code for serving the embedded web UI and integrating with the API.
- `ui/`: Svelte 5 + TypeScript + Vite frontend.
- `docs/`: Technical specifications and architectural documentation.
- `test/`: Integration, E2E, and mock NNTP server for testing.

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
8.  **Post-Processing**: Once all segments of a job are assembled, the job is handed to the `postproc` package for repair (PAR2), unpacking (RAR/7z), and finalization.

### Concurrency Model

Unlike the original Python implementation's single-threaded selector loop, `sabnzbd-go` leverages Go's native concurrency:

- **Goroutine per Connection**: Each NNTP connection runs in its own goroutine, allowing for massive parallelism across servers.
- **Channels for Signaling**: Channels are used to stream `ArticleResult`s from the downloader to the pipeline and assembler.
- **Shared State Locking**: Hot-path state (the queue, job metadata) is protected by `sync.RWMutex`.

---

## Subsystem Deep Dives

### Queue & Job Management (`internal/queue`)

- **State Ownership**: The `Queue` owns the ordered list of active `Job`s and a map for fast ID-based lookup. All mutations are protected by a `sync.RWMutex`.
- **Downloader Signaling**: The `Queue` provides a `Notify()` channel (cap-1) that wakes up the `downloader` whenever new work is added or a job is resumed.
- **Batched Updates**: To minimize lock contention on high-speed connections, the `Queue` supports batched updates for article completions (`MarkArticlesDone`, `MarkArticlesFailed`).
- **Persistence**: Active job state is persisted as gzipped JSON files in `admin/queue/jobs`.

### NNTP & Downloader (`internal/nntp`, `internal/downloader`)

- **Connection Management**: The `nntp` package implements the raw NNTP protocol. A `nntp.Conn` represents a single socket. The `downloader` manages pools of these connections per server.
- **Pipelining**: The system supports NNTP pipelining (multiple in-flight requests per socket) to maximize throughput over high-latency connections.
- **Error Classification**: NNTP status codes are mapped to Go sentinel errors (`ErrNoArticle`, `ErrAuthRejected`, etc.), allowing for robust retry and penalty logic.

### Decoder & Assembler (`internal/decoder`, `internal/assembler`)

- **High-Performance Decoding**: The `decoder` provides yEnc and UU decoding. The yEnc implementation is optimized for speed using `bytes.IndexByte` and correctly handles escape characters that shift across line endings.
- **Out-of-Order Assembly**: The `assembler` uses a single worker goroutine and `pwrite` (via `WriteAt` in Go) to write articles directly to their target offsets. This avoids the need for a sequential assembly step and handles articles arriving in any order.
- **Batching**: Successful writes are batched and periodically flushed to the queue to minimize locking overhead on high-speed connections.

### Persistence (`internal/history`, `internal/config`)

- **SQLite History**: Completed jobs are stored in `history.db`. The schema is maintained via `goose` migrations and is designed to be byte-for-byte compatible with the original Python implementation's history database.
- **YAML Configuration**: The application uses a YAML configuration (`sabnzbd.yaml`). The `config` package handles loading, validation, and atomic saves (write to temp file and rename). Environment variable expansion is supported within the YAML.

---

## Startup Sequence

When running in daemon mode (`--serve`), the application follows this sequence in `cmd/sabnzbd/main.go`:

1.  **Configuration**: Loads `sabnzbd.yaml` and resolves directory paths.
2.  **Logging**: Initializes structured logging (`log/slog`) with optional component-level filtering.
3.  **Locking**: Acquires a filesystem lock to ensure only one instance runs per admin directory.
4.  **Persistence**: Opens the SQLite history database and runs any pending migrations.
5.  **Application Core**: Constructs the `app.Application` orchestrator, which initializes the internal `queue`, `downloader`, `assembler`, and `postProcessor`.
6.  **Subsystem Start**: Invokes `application.Start()`, which boots the background goroutines for the pipeline, downloader, and post-processor.
7.  **API & Web**: Constructs the `api.Server` and `web.Handler`, binding them to a single HTTP listener.
8.  **Wait**: Blocks until a termination signal (SIGINT/SIGTERM) is received, then performs a graceful shutdown.

---

## API & Web Integration

The `sabnzbd-go` binary serves both the functional API and the modern web UI from a single port:

- **HTTP API**: Located at `/api`, it implements the SABnzbd legacy mode-dispatch system. Each `mode` (e.g., `queue`, `history`, `config`) is mapped to a handler with a specific `AccessLevel` (Open, Protected, Admin).
- **Error Logging**: All non-200 API responses are automatically logged. Status codes 500 and above are logged as errors, while other non-200 codes (4xx) are logged as warnings, including the explanation of what went wrong.
- **WebSockets**: Located at `/api/ws`, it provides real-time state updates to the UI using a broadcaster pattern.
- **Web UI**: The Svelte 5 SPA is embedded in the binary using `go:embed` (see `ui/embed.go`). The `internal/web` package handles serving these static assets and ensures SPA routing (fallback to `index.html`).

### Svelte 5 Development Caveats

When contributing to the UI, keep the following hard-won lessons in mind:
1. **Reactivity**: Do not use module-level `$state` in `.svelte.ts` stores for data that drives conditional rendering. Keep `$state` inside components for reliable re-renders during async operations.
2. **Dialogs**: `bits-ui` Dialog components may not fire `onOpenChange` when their state is bound from a parent. Use `$effect` to watch the `open` prop instead.
3. **Data Flow**: Child components should use `onupdate` callbacks rather than importing store functions directly to maintain explicit data flow.
