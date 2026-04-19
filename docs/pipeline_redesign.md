# SABnzbd-Go Pipeline Redesign: A Cleaner, Idiomatic Approach

## Background & Motivation
An analysis of the legacy C++ implementation in NZBGet (as seen in `DOWNLOAD_PIPELINE.md` and the `daemon/**` source code) reveals several architectural complexities:
- **Monolithic Downloader:** `ArticleDownloader::Run` is a massive method that intertwines server rotation, connection retries, retention limits, yEnc decoding, and direct disk writes.
- **Fragile Renaming Logic:** Renaming (Direct Rename, Par Rename, Rar Rename) is fractured across multiple controllers. Attempting to rename files mid-download by sniffing yEnc headers leads to race conditions and immense complexity when dealing with heavily obfuscated release names (a very common occurrence with messy Usenet data).
- **Brittle Post-Processing:** Tools like `unrar` are executed by controllers inheriting from a generic `Thread`, relying on fragile character-by-character parsing of `stdout` (e.g., `UnpackController::ReadLine`) to infer progress and error states.

## Scope & Impact
The goal of this redesign is to simplify the download, processing, and cleanup pipeline for `sabnzbd-go`, making it cleaner and more resilient to messy data (such as heavily obfuscated Usenet releases) than both the original C++ implementation in NZBGet and the current Go implementation.

## Comparison of Approaches

### 1. Legacy NZBGet (C++)
- **Architecture:** Heavily multi-threaded with deep class hierarchies (`ArticleDownloader` extending `Thread`). 
- **Coupling:** `ArticleDownloader::Run` is a massive method that intertwines server rotation, connection retries, yEnc decoding, and direct disk writes.
- **Naming & Post-Processing:** Attempts to rename files mid-download by sniffing yEnc headers ("Direct Rename"). Post-processing relies on fragile character-by-character stdout parsing of tools like `unrar` to track progress.
- **Complexity:** High. Prone to race conditions and relies heavily on mutexes for state coordination.

### 2. Current SABnzbd-Go Implementation
- **Architecture:** Uses Go idioms (goroutines and channels). A Downloader dispatches articles to connection goroutines, which push results to a `Completions` channel for decoding and assembly.
- **Assembly:** Uses `os.WriteAt` for out-of-order assembly directly into named target files in an `incomplete` directory.
- **Post-Processing:** A sequential pipeline (`Repair -> Unpack -> Deobfuscate -> Sort`). Includes "Direct Unpack" (extracting while downloading).
- **Critique:** While structurally better than C++, it still retains legacy complexities. Writing directly to named target files requires upfront filename guessing (often incorrect due to obfuscation) and necessitates a complex "Deobfuscate" phase. "Direct Unpack" adds extreme streaming complexity for minimal real-world performance gains on modern SSDs.

### 3. Proposed Solution: Pipeline with Deferred Naming
This approach prioritizes data integrity and simplicity over premature optimization.

- **Pure Data Pipeline:** The download process is modeled as a strict, unidirectional Go channel pipeline: `Fetcher (NNTP) -> Decoder (yEnc/UU) -> Assembler (Disk)`.
- **Deferred Naming Strategy:** 
  - We entirely discard mid-download file renaming and upfront filename guessing.
  - Files are assembled into a temporary directory using robust, unique identifiers (e.g., `incomplete/job-id/file-id.tmp`).
  - The true, validated filenames are derived *only* during the `PAR2` verification phase. `PAR2` files contain the ultimate source of truth for filenames. If no `PAR2` files exist, we fall back to the NZB metadata.
  - **Why?** This eliminates race conditions, removes the need for a dedicated "Deobfuscate" stage, and makes the pipeline completely resilient to noisy Usenet data.
- **Simplified Post-Processing:**
  - Remove "Direct Unpack". It adds immense complexity to the assembler and unpacker for marginal gains on fast modern storage.
  - Execute post-processing tools (`unrar`, `7z`, `par2`) using standard Go `os/exec` with combined output capture, checking exit codes for success rather than fragile stdout parsing.
- **Graceful Failure & Transparency:**
  - **Early Health Gate:** Dynamically track collection health (Total Bytes vs. Failed Bytes). If `FailedBytes > Par2RecoveryBytes`, halt the pipeline immediately, set the job status to `Failed: Insufficient Repair Data`, and wait for user intervention.

## Implementation Plan

1. **Refactor Assembler:** Modify `internal/assembler` to write decoded chunks to temporary files named by `file-id.tmp` rather than attempting to construct final filenames.
2. **Remove Mid-Flight Features:** Strip out "Direct Unpack" and "Direct Rename" logic from the pipeline to drastically reduce streaming complexity.
3. **Elevate PAR2 as Source of Truth:** Modify the post-processing pipeline so that the `PAR2` check phase explicitly handles the renaming of `*.tmp` files to their proper names *before* the `Unpack` phase begins.
4. **Modernize Post-Processing Exec:** Refactor the `unrar` and `7z` wrapper code to rely on robust exit code evaluation rather than character-by-character stdout stream parsing.
5. **Implement Early Health Gate:** Add dynamic health tracking to the Downloader orchestrator to pause/fail jobs immediately when repair becomes mathematically impossible.
6. **Fix Early Health Gate Deadlock:** Modify the `UnfinishedArticle` struct in `internal/queue/queue.go` to include `FailedBytes` and `Par2Bytes`, and update `dispatchPass` in `internal/downloader/dispatch.go` to evaluate the health gate without requesting a nested lock, collecting hopeless jobs to pause them after the read lock is released.
7. **Implement FinalizeStage:** Add a post-processing stage to move the completed job from the temporary `[JobID]` directory in `DownloadDir` to a user-friendly `[JobName]` directory in `CompleteDir`, optionally respecting category subdirectories.