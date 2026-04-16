# NZB Processing Lifecycle

This document provides a code-level overview of how `sabnzbd-go` handles an NZB from ingestion to final unpacking. It is intended for developers and contributors to understand the flow of data and the coordination between subsystems.

## 1. Ingestion & Parsing
**Primary Files:** `internal/nzb/parser.go`, `internal/nzb/model.go`, `cmd/sabnzbd/adapters.go`

The lifecycle begins when an NZB is received via the API, a watched folder, or an RSS feed.
- **Parsing:** `nzb.Parse()` converts the XML into a structured `NzbObject`. This model contains the metadata (passwords, categories) and a hierarchy of `NzbFile` objects, which in turn contain `Article` (segment) definitions.
- **Ingestion:** In `cmd/sabnzbd/adapters.go`, the `ingestHandler` receives the raw bytes, parses them, and hands the resulting `NzbObject` to the `Queue`.

## 2. Queueing & Signaling
**Primary Files:** `internal/queue/queue.go`, `internal/queue/job.go`

- **The Queue:** The `Queue` is an in-memory, priority-sorted list of `Job` objects (which wrap the `NzbObject`).
- **Signaling:** When a job is added or modified, the queue sends a non-blocking signal on its `notify` channel (`chan struct{}`).
- **Key Takeaway:** The `Queue` is the source of truth for the downloader. It is in-memory for speed but persists its state to `admin/queue/` using JSON+gzip on every significant event (job added, file completed).

## 3. Downloading & NNTP Engine
**Primary Files:** `internal/downloader/downloader.go`, `internal/downloader/dispatch.go`, `internal/nntp/conn.go`

- **The Downloader:** The `Downloader` goroutine runs a `select` loop, waiting for signals on the `notify` channel.
- **Dispatch:** When signaled, `downloader.dispatchArticles()` iterates through the queue and assigns available articles to idle NNTP connections.
- **NNTP Connections:** Each connection (`internal/nntp/conn.go`) runs its own goroutine. It fetches articles using the `BODY` or `ARTICLE` command and pushes the raw response (including headers) to the `Completions()` channel.
- **Key Takeaway:** Backpressure is handled by connection availability. If all connections are busy, the downloader stops consuming from the queue.

## 4. Decoding & Pipeline Orchestration
**Primary Files:** `internal/app/pipeline.go`, `internal/decoder/decoder.go`

- **The Pipeline:** The `pipeline.run()` loop in `internal/app/pipeline.go` is the "glue" of the download process. It listens for results from the `downloader.Completions()` channel.
- **Decoding:** Raw article bodies are passed to `decoder.Decode()`, which handles yEnc or UU decoding and verifies the CRC32.
- **Key Takeaway:** The pipeline is where the transition from "network segment" to "file chunk" happens. Errors in decoding are reported back to the queue for retry on a different server.

## 5. Caching & Assembly
**Primary Files:** `internal/cache/cache.go`, `internal/assembler/assembler.go`

- **Article Cache:** Decoded data is temporarily held in the `Cache`. If memory usage exceeds the limit (`cfg.Downloads.ArticleCacheSize`), articles spill to disk as temporary files in the admin directory.
- **Assembler:** The `Assembler` receives decoded chunks and writes them to the target file in the `Downloads/incomplete` directory. It uses `os.WriteAt` (pwrite) to perform **out-of-order assembly**, meaning articles are written to their correct offsets as soon as they arrive, regardless of their segment number.
- **Key Takeaway:** A file is marked "complete" only when every expected byte has been successfully written to disk. The `Assembler` signals the `pipeline` when a full `NzbFile` is done.

## 6. Post-Processing & Unpacking
**Primary Files:** `internal/postproc/postproc.go`, `internal/par2/par2.go`, `internal/unpack/unrar.go`, `internal/sorting/sorter.go`

Once an `NzbObject` (the whole job) is fully downloaded and assembled, it moves to the `PostProcessor`.
- **Stages:** The job passes through a sequential pipeline:
    1. **Repair:** `par2` is invoked to verify and, if necessary, repair the files.
    2. **Unpack:** `unrar` or `7z` is called to extract the archives.
    3. **Deobfuscate:** Files are renamed if the Usenet poster used randomized names.
    4. **Sort:** Files are moved to their final category-based directory based on naming templates.
- **Direct Unpack:** As an optimization (`internal/unpack/direct.go`), extraction can begin while the download is still in progress if the volumes arrive in order.

## Developer & Contributor Key Takeaways

1.  **Concurrency Model:** Goroutines are everywhere. Use `go test -race` religiously. Coordination is almost exclusively via channels.
2.  **Context Propagation:** Every blocking or long-running function must respect the `context.Context` passed to it. Shutdown is handled by canceling the root context.
3.  **Atomic Persistence:** Any state written to disk (queue, config, bpsmeter) uses a "write-temp-then-rename" pattern to prevent corruption during crashes or power failures.
4.  **Decoupled Adapters:** Subsystems (like `dirscanner` or `rss`) do not talk to the `Queue` directly. They use `cmd/sabnzbd/adapters.go` to bridge the gap, keeping the core packages library-like and easy to test.
5.  **Logging:** Use `log/slog`. Injected loggers are preferred over the global `slog.Default()`. Include relevant metadata (e.g., `job_id`, `filename`) in the structured fields.
