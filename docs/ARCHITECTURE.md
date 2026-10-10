# GoNZBD Architecture & Design

This document provides a detailed overview of the architecture, design, and implementation of GoNZBD, a high-performance automated Usenet binary downloader written in Go. Its one compatibility obligation is the SABnzbd-compatible HTTP API that third-party tools call (`docs/sabnzbd_spec.md` §10); behind that API the design is its own, with [SABnzbd](https://sabnzbd.org) and [NZBGet](https://nzbget.com) as prior art.

## Project Overview

GoNZBD is designed as a long-running daemon that automates the Usenet download lifecycle: ingestion (NZB files), downloading (NNTP), decoding (yEnc), assembly, and post-processing. It emphasizes high performance, modern Go idioms, and a self-contained binary (including the web UI). GoNZBD is POSIX-only (Linux, macOS, FreeBSD); Windows is not a supported target.

---

## File and Directory Structure

The project follows a standard Go project layout:

- `cmd/gonzbd/`: The application entry point. Handles CLI flags, configuration loading, and service orchestration.
- `internal/`: Core application logic, restricted from external import.
    - `api/`: Implementation of the legacy SABnzbd HTTP API (`/api?mode=...`) and modern WebSocket events.
    - `app/`: The central orchestrator (`Application`) and the download pipeline bridge.
    - `assembler/`: Logic for writing decoded article parts to disk using `pwrite`, one synchronous write per accepted article. It reports which bytes reached `WriteAt`; it does not decide that an article is done.
    - `bpsmeter/`: Bandwidth statistics and speed limiting.
    - `cmdutil/`: Helpers for building and validating external command invocations (nice/ionice wrapping, extra-param parsing).
    - `config/`: YAML configuration schema, loading, validation, and atomic saves (marshal under RLock, then write lock-free).
    - `constants/`: Shared constants (priorities, statuses) used across packages.
    - `crc32util/`: CRC-32 utilities used by the quick-check stage.
    - `decoder/`: High-performance yEnc and UU decoding with LUT-based scanning and fused subtract-42 output.
    - `deobfuscate/`: Renames obfuscated filenames using NZB subject hints, PAR2 filenames, and extension detection.
    - `directunpack/`: In-flight RAR extraction that runs in parallel with downloading.
    - `dirscanner/`: Watches a folder for new NZB files.
    - `durability/`: The SQL of the loose article record — `written_articles` and `job_files` — and the reclaim rule that removes a job's rows. The recorder that writes it and the verifier that reads it back on restart live in `internal/app`. See [`docs/durability-contract.md`](durability-contract.md).
    - `downloader/`: The NNTP engine, handling server pools, connection management, and article dispatch with O(1) pending-article tracking.
    - `fsutil/`: File system utilities: path sanitization, atomic writes (temp+fsync+rename), symlink-safe containment checks, and cross-device move.
    - `history/`: Persistence layer for completed jobs using SQLite and `goose` migrations.
    - `humanfmt/`: Human-readable formatting helpers (sizes, durations) shared across packages.
    - `nntp/`: Low-level NNTP protocol implementation with bounded response reading. It does not validate message-IDs — see the Message-ID Validation note below.
    - `notifier/`: Dispatcher for user notifications (email, Apprise, scripts).
    - `nzb/`: NZB (XML) parsing and model definitions with input size limits.
    - `par2/`: PAR2 parity verification and repair tool wrapper with structured status parsing.
    - `postproc/`: Post-processing pipeline: quickcheck, repair, unpack, deobfuscate, finalize, script, and supporting stages.
    - `dispatch/`: The active job registry, execution engine, and scheduling orchestrator.
    - `job/`: Active job models, immutable manifests, thread-safe progress bookkeeping, and attempt lifecycle states.
    - `sched/`: Pure scheduling decisions (leases, compute slots, state machine transitions) with zero I/O.
    - `rarheader/`: RAR archive header parsing with filename sanitization.
    - `storagefault/`: Classification of storage errors into retryable (stall the job) and permanent (fail the job), carrying the operation and path for a reason a user can act on.
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
7.  **Assembly**: The `pipeline` hands decoded parts to the `assembler`, which writes them to their exact byte offset in the target file using `pwrite`. This allows for out-of-order assembly as segments arrive. The article's decoded CRC32 travels with it.
8.  **The article record**: once an article's `pwrite` returns nil, the assembler reports it (`OnArticleWritten`) and the recorder buffers one `written_articles` row — offset, length, CRC — and marks the article Done in memory. The recorder flushes every 5 s and synchronously at shutdown, at the hand-over to post-processing and when a job is filed in history. A row is not a durability claim: a completed file is fsynced and trimmed on its writing handle before `job_files.complete` is set, a failed fsync untrusts the file (its rows and Done bits go), and after a restart a row becomes a Done bit again only once its bytes are read back from the device and match its CRC. A storage fault stalls or fails the *job* and never marks an article failed. See [`docs/durability-contract.md`](durability-contract.md).
9.  **On-Demand PAR2 Gate** (optional, default on): when every file the job is actually fetching is assembled, if PAR2 recovery volumes were held back (see *On-Demand PAR2* below), the downloaded data is CRC-verified against the PAR2 index *before* post-processing. Clean ⇒ the job finalizes and the held volumes are marked as never fetched; damaged ⇒ the volumes are released and fetched via the normal download path, then completion fires again and proceeds to post-processing.
10. **Post-Processing**: Once all segments of a job are assembled, the job is handed to the `postproc` package, which runs a configurable chain of stages: repair (PAR2), unpack (RAR/7z/join), deobfuscate, user script, and finalize (move to complete directory). Sorting/renaming is intentionally not implemented — it is handled by external tools (Sonarr, Radarr, etc.).

### Concurrency Model

Unlike the original Python implementation's single-threaded selector loop, GoNZBD leverages Go's native concurrency:

- **Goroutine per Connection**: Each NNTP connection runs a persistent worker goroutine (`connWorker`) that manages the socket state and handles pipelined fetches and concurrent decodes via sub-goroutines (bounded by configuration settings), allowing for massive parallelism across servers.
- **Channels for Signaling**: Channels are used to stream `ArticleResult`s from the downloader to the pipeline and assembler.
- **Shared State Locking**: Hot-path state (the queue, job metadata) is protected by `sync.RWMutex`.

---

## Subsystem Deep Dives

### Job Management & Scheduling (`internal/job`, `internal/sched`, `internal/dispatch`)

- **State Ownership**: The `Dispatcher` (`internal/dispatch`) owns the ordered registry of `Job` records (`internal/job`) and executes scheduling transitions via `internal/sched`.
- **Pure Decision Core**: `internal/sched` decides pool allocations (Pool A leases, Pool B compute slots) and state transitions with zero I/O.
- **Manifest/Progress Separation**: A `Job` holds an immutable `Manifest` (parsed-once article/file structure — subjects, byte counts, flat article arrays) and a mutable `JobProgress` (per-article done/failed/emitted state, per-file assembly bookkeeping, job-level counters).
- **Compiler-Enforced Residency**: Manifest residency is bounded by what a job holds: a manifest is hydrated when the job holds what its position requires, and a paused job keeps one it already has. Startup also hydrates every job restored paused at `Fetching`, which then stays resident until it is resumed or removed (see [`docs/job-lifecycle.md`](job-lifecycle.md) § *Residency is bounded by what a job holds*). A non-resident job holds no manifest in RAM, and `Job.Manifest()` answers it with `job.ErrNotResident`.
- **Single Progress Writer**: the recorder (`internal/app/record.go`) is the only thing that *updates* `job_files` in SQLite. It buffers one row per written article and one state per dirty file, and writes them, and the verdicts a verification or an untrust commits, through `Store.ApplyRecord` (`internal/durability/written.go`), whose production callers are the recorder's `flush` and `apply`. `git grep -n 'UPDATE job_files' -- '*.go' ':!*_test.go'` returns 2 lines, both in `internal/durability/written.go` and reached only through `ApplyRecord`. Ingestion and retry seed the initial `job_files` rows through `Store.Admit`. Their rows are deleted by one reclaim rule, `Store.Reclaim` after each departure and `Store.SweepOrphans` at startup, which takes a job's rows once nothing reaches it and keeps a FAILED history entry's `job_files` and `written_articles` for a retry (`git grep -nE 'durable\.(Reclaim|SweepOrphans)\(' -- '*.go' ':!*_test.go'` returns 2 lines, in `internal/app`'s `reclaim` and `sweepOrphans` helpers). What the recorder owns is progress, not row lifetime — `docs/durability-contract.md` § 6 states the lifecycle rule.
- **Article Addressing**: Articles are addressed by global index into the manifest's flat arrays, never by Message-ID. A written article (`Job.MarkArticleWritten`), a verified row (`Job.InstallVerified`) and a permanent failure (`Job.MarkArticleFailed`) all name the article by that index.

### On-Demand PAR2 (`internal/job`, `internal/app`, `internal/par2`)

To save bandwidth, PAR2 **recovery volumes** (`*.volNNN+MM.par2`) are downloaded only when repair is actually needed. Controlled by `downloads.on_demand_par2` (default **on**). This section is the design record:

> **The saving is reachable only for a file whose rows tile it exactly.** The verdict below reads `FileProgress.AssembledCRC32`, which `Job.SettleFileCRC` derives from the file's `written_articles` rows when the file completes: the per-article CRCs are combined, with no read of the file, only when every article of the file has one row, none failed, the first starts at offset 0, and each starts where the previous one ends. A file with a hole, a permanently failed article, or rows that do not chain supplies no CRC, reports `NoCRC` and fetches every volume. That direction is safe: it costs bandwidth on a file whose bytes are in doubt and never ships an unrepaired one. See [`docs/durability-contract.md`](durability-contract.md) § "The whole-file CRC" and [`docs/post-processing-contract.md`](post-processing-contract.md) § *Verification bypass guarantees*.

- **Fetch policy, not a boolean**: every file carries a `FetchPolicy` on `JobProgress`, persisted as `job_files.fetch_policy`. `FetchAlways` is the default; `FetchIfNeeded` is a recovery volume awaiting the verdict; `FetchNever` is one the verdict ruled unnecessary. A job's file set never changes after `NewJob` — a volume that is ruled out is *marked*, not removed, so every `file_index` stays valid for the job's whole life. See [`docs/job-lifecycle.md`](job-lifecycle.md).
- **Classification**: at add-time `NewJob` flags recovery volumes (`Manifest.FileIsPar2Recovery`, via `par2.IsRecoveryVolume`) and sets them to `FetchIfNeeded`. Classification reads the NZB subject, before anything is downloaded, so it recognizes the conventional `.volNNN+MM.par2` naming rather than inspecting packets. A par2 file whose name does not match stays `FetchAlways` and is therefore always fetched — which is how the **index** file, carrying the per-file checksums the verdict needs, is kept. The converse does not hold: the PAR2 specification recommends that name but does not require it, so a plainly-named `.par2` may carry recovery data the classification does not recognize. `RepairState` accounts for that by withholding a beyond-repair verdict from a job that has par2 files but no recognized volumes.
- **Skipped during download**: files that are not `FetchAlways` have `Pending == 0`, are skipped by `ForEachUnfinishedArticle`, and do not block `IsComplete()` — so a job is "downloaded" once its `FetchAlways` files finish.
- **Decision = identify, then verify, three-valued**: at download-complete (`handleFileComplete`), `par2.Assess` reads the on-disk index once and answers three things together — which par2 entry each delivered file *is* (by `Hash16k` content, since an obfuscated name says nothing), whether that file is intact (its recorded CRC against par2's), and which relocations would follow. `par2Verdict` then reads that answer, in two steps rather than one, and reports one of `outcomeClean`, `outcomeRepair`, or `outcomeUnknown` — not a bool, because "verified clean" and "nothing could be identified" are different claims and must not collapse into the same false. **Accounting first:** if some entries were identified and others were not, repair is possible and `outcomeRepair` is reported; if *nothing* was identified, `outcomeUnknown` is reported (the Layout B case below, which is also what an obfuscated post damaged inside its first 16 KB looks like — the two are indistinguishable from this value). **Then verification:** for a fully accounted set, `outcomeRepair` is reported iff `Mismatched + NoCRC + Unverified > 0`, else `outcomeClean`. A missing or unusable index skips both and falls back to `outcomeRepair`. A file the durability record cannot supply a whole-file CRC for reports `NoCRC` and forces `outcomeRepair` — see the note above. The QuickCheck stage calls `par2.Assess` separately (`internal/postproc/stage_quickcheck.go:158`) to read the same directory against the same index, so they build assessments over the same content and agree about file identities; they are separate calls answering different questions (download path: *can* these volumes be discarded? QuickCheck: *should* we relocate these files, and does par2 describe anything delivered?), and their verdicts are not required to agree. The download path reads "nothing was identified" through `par2.Identification.NothingIdentified`. QuickCheck instead judges each par2 set on the entries it did not identify: when unpack will run, none of them is named as an archive, and each is a member of a delivered RAR or 7z archive, the set is deferred — `repair` skips it and `extracted_repair` verifies it against the extracted files after unpack — and a job whose every set is deferred records `Unidentified` (see `docs/post-processing-contract.md` § Core Pipeline Invariants).
- **The download path decides; it never renames**: `Assess` reports the moves that would put each file at its par2 path, and this path discards them. Relocation belongs to post-processing's `quickcheck` stage, ahead of the `repair` stage that needs the files there. Renaming here was tried and removed (#494): it existed only so that name-based verification would have corrected names to match against, and once identification became content-based it bought the verdict nothing — while costing a real defect, since `JobProgress.Filename` cannot hold a path, so a file relocated into a subdirectory could not be recorded truthfully and a restart's verification, finding nothing at the recorded path, would re-download it in full.
- **Where relocation does happen, identification precedes it, and the type enforces that**: `Assess` reports renames without applying them, and `par2.ApplyRenames` takes the whole `Assessment`, so a verdict is always computed from pre-rename state. The reverse order was #492 and #494 — verification matched par2 entries by *name*, renaming invalidated exactly those names, and the ambiguous "nothing matched" result was then read as proof the set protected other files and used to **discard** the volumes on a healthy download. Content identification is what makes that signature meaningful: it distinguishes cases where something delivered matches a par2 entry (repair may help) from cases where nothing does. A Layout B post produces "nothing matched" because par2 protects the extracted contents, not the delivered archives; an obfuscated single-file post damaged inside its first 16 KB also produces "nothing matched" because the damage defeats all identification passes. The two are indistinguishable from the assessment value alone, which is why `outcomeUnknown` now holds the volumes rather than discarding them.
- **Re-activation is download→download, not postproc→download**: on damage, `UndeferRecoveryVolumes(jobID, fileIdxs)` promotes those files from `FetchIfNeeded` to `FetchAlways`, recomputes counters, sets `Par2Recovered` (guards re-firing), and wakes the dispatcher. The job becomes incomplete again and the *normal* download path fetches the volumes — no back-edge from post-processing. Volumes still held when post-processing fails par2 are fetched by a new job instance instead: the finalizer files the failure and retries the job with them released (see "Unidentifiable verdict is held" below).
- **Clean verdict**: `DiscardDeferredPar2` moves every still-`FetchIfNeeded` volume to `FetchNever`, and only `outcomeClean` reaches it. No *verdict* promotes it back within a run — `Job.undeferRecovery` skips anything that is not `FetchIfNeeded`. A restart is the exception, and it is not a verdict: the first hydration after it restores the policy from `job_files` (`installVerification`), so the job resumes with whatever the row holds. That is safe only while every writer of the policy marks the job's files dirty for the recorder, which is why both verdicts call `Application.markFetchPolicyDirty` — a flush writes only files already marked, so an unmarked verdict would be undone by the next restart rather than merely delayed. A retry does not inherit the previous attempt's policy at all — it rebuilds the job from scratch through `BuildIngestJob`, whose own classification re-derives the policy from current config: `FetchIfNeeded` for a recovery volume while `downloads.on_demand_par2` is enabled, and `FetchAlways` once it is disabled. So a retry re-derives the verdict by re-verifying instead of re-downloading volumes the oracle already ruled out, and honours a setting the user has changed since the failed attempt (`TestRetryHistoryJob_ConfigurationIsHonoured`, #329).
- **Unidentifiable verdict is held, not discarded**: `outcomeUnknown` calls `SetPar2ReleaseReason` but neither `DiscardDeferredPar2` nor `UndeferRecoveryVolumes` — the volumes stay `FetchIfNeeded` ("held" in the UI, `internal/api/queue.go`'s `fileState`) rather than `FetchNever` ("skipped"), because a verdict that could not identify anything has not earned the "skipped" label, and a healthy post never fetches them. Holding is also what lets a damaged one be repaired (#651): when its post-processing fails par2 with volumes still held (`heldVolumesMightRepair`), `jobFinalizer.finalize` files the Failed entry and then retries the job through `RetryHistoryJob`'s body with every held volume released before the retry is registered (`retryWithHeldVolumes`); it sends the failure notification only when that retry could not start. The retry releases everything it holds, so its own par2 failure is final. A retry the user starts is rebuilt by `BuildIngestJob` and holds the volumes again, so it gets one automatic retry of its own.
- **Early un-defer**: a permanent data-article failure during download releases the volumes immediately (`Job.MarkArticleFailed`), shrinking the window in which the volumes themselves could age off the server — but only when the job's `Policy.Repair` is true, and only while the job's manifest is resident. A failure that arrives after eviction records only its bits, and leaves the release to a later failure or the Assessing-time verdict (see `docs/job-lifecycle.md` § "Residency: the three tiers"). `Repairing` never runs for a PP=0 (download-only) job, so `MarkArticleFailed` leaves its volumes held rather than releasing them for a repair that will never happen; the Assessing-time verdict (`maybeReleaseRecoveryVolumes`) checks the same field before its own release, for the same reason. See [`docs/post-processing-contract.md`](post-processing-contract.md) § *On-Demand Par2: Fetch Policy and Verdict*.
- **Phasing**: Phase 1 fetches *all* recovery volumes on damage (the `fileIdxs` selection arg is the seam for Phase 2's block-exact subset selection).

### NNTP & Downloader (`internal/nntp`, `internal/downloader`)

- **Connection Management**: The `nntp` package implements the raw NNTP protocol. A `nntp.Conn` represents a single socket. The `downloader` manages pools of these connections per server.
- **Message-ID Validation**: `nntp` does *not* validate message-IDs; it interpolates them into the command line as given. Validity is decided once, further out, at NZB parse time — `internal/nzb` refuses any ID that is empty, longer than 495 octets, or carrying SP, HT, CR, LF, NUL or an interior `<`/`>`, which is what prevents command injection. `job.Manifest.UnmarshalJSON` re-applies the same predicate to IDs read back from disk, since a manifest written before that rule existed could still carry one. See `docs/article-validation-contract.md`.
- **Bounded Reading**: Response lines are capped at 2KB and article bodies at 10MB to prevent OOM from malicious servers.
- **Pipelining**: The system supports NNTP pipelining (multiple in-flight requests per socket) to maximize throughput over high-latency connections. Responses are paired with requests by FIFO position, but that pairing is not trusted: each in-flight command remembers the Message-ID it asked for, and the reader compares it against the one the server echoes on the success line. A disagreement kills the connection rather than failing one article, because it proves the queue is desynced and every response after it is mis-paired too — see `docs/nntp-downloader-contract.md` § `nntp.Conn` pipelining contract.
- **Error Classification**: NNTP status codes are mapped to Go sentinel errors (`ErrNoArticle`, `ErrAuthRejected`, etc.), allowing for robust retry and penalty logic.
- **Dispatch Optimization**: The dispatch loop uses cached server configs per pass and 2-case selects to minimize overhead at ~330 articles/second throughput.

### Decoder & Assembler (`internal/decoder`, `internal/assembler`)

- **High-Performance Decoding**: The `decoder` provides yEnc and UU decoding. The yEnc implementation uses a 256-byte lookup table (`specialLUT`) via `indexSpecial` to find CR/LF/`=` bytes in O(1) per byte, and a fused `sub42Span` function that combines the subtract-42 transform with the output copy in a single pass for L1 cache efficiency. Both are capped at 10MB to reject oversized payloads.
- **Out-of-Order Assembly**: The `assembler` uses a single worker goroutine and `pwrite` (via `WriteAt` in Go) to write articles directly to their target offsets. This avoids the need for a sequential assembly step and handles articles arriving in any order.
- **Synchronous writes**: The assembler writes each accepted article with its own `WriteAt` and buffers nothing, so an article's bytes are in the file before the next article is accepted.
- **No ack path**: the assembler cannot mark an article done or record a whole-file CRC. It reports that an article's bytes reached `WriteAt` (`OnArticleWritten`), and the recorder in `internal/app` marks it Done. The assembler's one trim of a target file is the completion finish, `FileWriter.finish`: fsync, truncate to the end of the last byte range the writer owns (`ownedRanges.maxEnd`), fsync again, on the writing handle and before `OnFileComplete`. After a restart, articles are resolved by the verifier's readback of their rows instead. See [`docs/durability-contract.md`](durability-contract.md).

### Post-Processing (`internal/postproc`)

Post-processing runs a chain of `Stage` implementations in order for each completed job:

| Order | Stage | Package | Description |
|-------|-------|---------|-------------|
| 1 | `quickcheck` | `postproc` | CRC-verify assembled files against PAR2 metadata; relocate flat files into expected subdirectories |
| 2 | `repair` | `par2` | PAR2 verification and repair (skipped when quickcheck passes; skips par2 sets quickcheck deferred until after unpack) |
| 3 | `rar_volume_recovery` | `postproc` | Reconstruct a missing RAR volume from PAR2 data before extraction |
| 4 | `unpack` | `unpack` | RAR, 7z extraction and split file joining |
| 5 | `extracted_repair` | `par2` | PAR2 verification and repair of the deferred sets, against the files unpack extracted |
| 6 | `sample_cleanup` | `postproc` | Delete sample video files (when enabled) |
| 7 | `par2names` | `postproc` | Recover original filenames from PAR2 metadata |
| 8 | `par2_cleanup` | `postproc` | Delete `.par2` files after repair/rename (when enabled) |
| 9 | `deobfuscate` | `deobfuscate` | Rename obfuscated files using NZB hints and PAR2 filenames |
| 10 | `unwanted_cleanup` | `postproc` | Delete every file under the job's download directory whose extension the unwanted-extension rules exclude, unless the user approved the job. Backstop for `app.peekArchiveForUnwanted`, which blocks a job early from RAR5 volume and par2 headers while it downloads (RAR3 content is caught here) |
| 11 | `extension_cleanup` | `postproc` | Delete files matching the user's cleanup extension list |
| 12 | `finalize` | `postproc` | Move job from incomplete to complete directory |
| 13 | `script` | `postproc` | Run user-supplied post-processing script (see `docs/post-processing-scripts.md`) |

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

- **SQLite Persistence**: Both active queue state (`dispatch_jobs`, `job_files`, `written_articles`) and completed jobs (`history`) are stored in `history.db`; a FAILED history entry keeps its `job_files` and `written_articles` rows for a retry. The schema is a single canonical `goose` migration (`001_initial.sql`) that descends from the original Python implementation's history table and has diverged from it. It is deliberately not interchangeable: `history.Open` refuses any database this build's migrations did not write.
- **YAML Configuration**: The application uses a YAML configuration (`gonzbd.yaml`). The `config` package handles loading, validation, and atomic saves (marshal under RLock, release lock, write to temp file, fsync, and rename). Environment variable expansion (`$VAR`, `${VAR}`) and `~` home-directory expansion are supported in **path-typed fields only** (e.g., `download_dir`, `admin_dir`, `script_dir`); non-path values (passwords, API keys) are intentionally left unexpanded to avoid corrupting values that contain `$`.

---

## Startup Sequence

When running in daemon mode (`--serve`), the application follows this sequence in `cmd/gonzbd/main.go`:

1.  **Configuration**: Loads `gonzbd.yaml` and resolves directory paths.
2.  **Directories**: Creates download, complete, admin, and (optionally) watch directories on disk.
3.  **Logging**: Initializes structured logging (`log/slog`) with optional component-level filtering.
4.  **Locking**: Acquires a filesystem lock to ensure only one instance runs per admin directory.
5.  **Persistence**: Opens the SQLite history database (`history.db`) and runs any pending `goose` migrations.
6.  **Application Core**: Constructs the `app.Application` orchestrator, which initializes the internal `dispatcher`, `downloader`, `assembler`, `postProcessor`, and bandwidth meter (`bpsmeter`, restoring persisted lifetime totals from `bpsmeter.json`).
7.  **Subsystem Start**: Invokes `application.Start()`. Inside `Dispatcher.StartWith`, after the queue is restored and before the first tick, `reconcileBeforeFirstTick` drops any queued job already filed in history and hydrates the jobs restored paused at `Fetching`, so their progress is reported. Then it boots the assembler, downloader, post-processor and the background goroutines, and finally reclaims the record rows and manifests no job reaches (`sweepOrphans`). Each job's written articles are verified at its **first hydration** by reading every row of an incomplete file back from the device and checking its CRC. For a job restored paused at `Fetching` that hydration is the startup reconciliation's, synchronously inside `Application.Start`, so the API does not listen until it ends ([`docs/durability-contract.md`](durability-contract.md), Accepted limitation 1); any other job is verified by whichever hydrates it first: a tick, before it can launch the job, a rename (`Dispatcher.SetName`), or the startup filing of an owed unwanted failure (`fileOwedUnwantedFailures`). An article whose bytes are gone or do not match comes back Outstanding; a file that cannot be read for a reason about the device parks the job rather than discarding its progress. See [`docs/durability-contract.md`](durability-contract.md) §3.
8.  **Ancillary services**: Starts the notifier and directory scanner (`dirscanner`).
9.  **API & Web**: Constructs the `api.Server` and `web.Handler`, binding them to a single HTTP listener (and optionally a separate HTTPS listener).
10. **Wait**: Blocks until a termination signal (SIGINT/SIGTERM) is received, then performs a graceful shutdown: stop the downloader, stop the assembler (which fsyncs and closes every open file), cancel the context, wait, stop post-processing and the dispatcher, and **flush the recorder** last, so the rows of everything the assembler wrote reach SQLite and are not refetched on the next start. See `Application.Shutdown` and [`docs/durability-contract.md`](durability-contract.md) § "Clean shutdown".

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
