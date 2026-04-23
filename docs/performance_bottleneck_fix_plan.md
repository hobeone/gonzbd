# SABnzbd-Go Performance Bottleneck Fix Plan

## Objective
Resolve the performance bottleneck in the download -> decode -> disk pipeline to maximize throughput (targeting 1Gbps+). The current implementation suffers from sequential CPU-bound decoding, synchronous disk I/O on every article, and inadequate buffering.

## Background & Motivation
Investigation reveals `sabnzbd-go` hits only ~10% of a 1Gbps connection because:
1. **Sequential Decoding**: The pipeline decodes articles sequentially in a single goroutine (`pipeline.run`), failing to utilize multi-core CPUs.
2. **Aggressive Fsyncing**: The assembler calls `f.handle.Sync()` after *every* article write, overwhelming the disk with small synchronous flushes.
3. **Inadequate Buffering**: The assembler queue size defaults to `12`, which immediately fills up during disk I/O spikes, causing rapid backpressure that stalls the downloader.

## Proposed Solution & Implementation Steps

This plan takes a test-first approach, ensuring each step is verifiable and that dead/redundant code is aggressively removed.

### Step 1: Eliminate Per-Article `fsync` in Assembler
**Goal**: Defer to the OS page cache for contiguous disk I/O scheduling.
* **Test-First**: Add/update benchmarks in `internal/assembler/assembler_bench_test.go` to measure write throughput without per-article fsyncs. Update unit tests to verify `fsync` is only called on file completion or via a periodic background flusher (if batched durability is required).
* **Implementation**: Remove `f.handle.Sync()` from the per-article write path in `internal/assembler/assembler.go`. Implement `Sync` on file close or via a timed batch flush.
* **Dead Code Removal**: Remove any tight-loop tracking or redundant error handling specifically tied to the per-article fsync.
* **Model**: `[gemini-pro]` (Handles durability constraints and I/O architecture).
* **Skills**: `golang-performance`, `golang-testing`.

### Step 2: Increase Assembler Buffering
**Goal**: Absorb momentary disk I/O spikes without stalling the network threads.
* **Test-First**: Create a benchmark demonstrating that a larger channel buffer prevents upstream blocking during simulated I/O latency.
* **Implementation**: Increase `DEF_MAX_ASSEMBLER_QUEUE` or the assembler's channel capacity from `12` to a significantly larger value (e.g., `2048` or `4096`) in `internal/assembler/assembler.go`.
* **Dead Code Removal**: N/A (Constant update).
* **Model**: `[gemini-flash]` (Simple configuration/constant change).
* **Skills**: `golang-performance`.

### Step 3: Parallelize Decoding (Move to Downloader/Fetcher)
**Goal**: Distribute the CPU-bound yEnc decoding process across all active NNTP connection goroutines.
* **Test-First**: Update `downloader_test.go` to assert that the `Completions` channel receives *decoded* data (with offsets) rather than raw NNTP bodies. Add a benchmark to prove that parallel decoding scales linearly with CPU cores.
* **Implementation**: 
  - Move the `decoder.DecodeArticle` invocation out of `pipeline.run` (`internal/app/pipeline.go`).
  - Integrate decoding directly into the NNTP connection worker (`internal/downloader/dispatch.go` or `internal/nntp/conn.go`) as soon as the article is fetched.
  - Modify `downloader.ArticleResult` to carry decoded `Data` and `Offset` instead of raw `Body`.
* **Dead Code Removal**: Strip all decoding logic, including the UU fallback, out of `pipeline.go`. Remove unused fields (like `Body`) from the `ArticleResult` struct.
* **Model**: `[gemini-pro]` (Major concurrency architecture shift across package boundaries).
* **Skills**: `golang-concurrency`, `golang-performance`, `golang-testing`.

### Step 4: End-to-End Integration & Benchmark
**Goal**: Verify the entire pipeline operates flawlessly at high speeds without deadlocks or data corruption.
* **Test-First**: Enhance the end-to-end integration test (`test/integration/download_test.go`) to simulate a high-speed 1Gbps+ download using the mock NNTP server.
* **Implementation**: Wire the updated components together. Ensure that the early health gate (FatalErr handling) remains intact despite the shifted decoding responsibilities.
* **Dead Code Removal**: Clean up any remaining legacy pipeline artifacts or obsolete mock implementations.
* **Model**: `[gemini-pro]` (Integration and test validation).
* **Skills**: `golang-benchmark`, `golang-testing`.

## Verification & Testing
Before committing each step, the standard quality gates must pass:
```bash
./scripts/run_tests.sh
go vet ./...
golangci-lint run ./...
```
Additionally, `go test -bench=. ./...` should be used to empirically validate the throughput gains.

## Alternatives Considered
* **Implementing a massive `ArticleCache` (Python approach)**: While effective, it adds significant memory management complexity and garbage collection overhead in Go. Increasing channel buffers and relying on Go's efficient concurrency primitives combined with the OS page cache is more idiomatic and simpler.
* **Worker Pool for Decoding**: Instead of decoding in the fetcher, we could spin up a pool of decoder goroutines. However, decoding in the fetcher thread naturally pairs the CPU work with the network I/O, providing implicit backpressure and avoiding an extra channel hop.

## Migration & Rollback
Since this is a greenfield Go reimplementation targeting fresh installs, no data migration is required. Rollback can be achieved via standard `git revert` if the new concurrency model introduces unexpected race conditions.
