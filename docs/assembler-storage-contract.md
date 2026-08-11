# Assembler & Streaming Storage Contract

This document is the contract for `internal/assembler` and
`internal/directunpack`: how article segments are stitched into complete files,
disk write caching and pre-allocation, CRC combination, streaming unpack, and
disk space safety guarantees.

`docs/ARCHITECTURE.md` describes the assembler's position in the download
pipeline. This document defines its operational invariants and structural
guarantees.

**This states the contract in the present tense, including parts not yet built.**
That is deliberate — it is the target the code is held to, not a report on the
code as it stands. The Status section below records exactly what has landed.
Where the two disagree, the code is wrong and the gap is a bug, not a
documentation error.

## Why this exists

The assembler converts non-sequential yEnc decoded article chunks arriving from
the downloader into structured target files on stable storage. At 1Gbps+
download speeds, naive I/O creates several classes of failure:

- **Syscall overhead**: Thousands of 128KB `pwrite` calls per second overwhelm
  the page cache. Write coalescing batches contiguous articles into single
  `WriteAt` calls, reducing syscall count by 8–16×.
- **Queue lock contention**: Marking each article `Done` individually acquires
  the queue's write lock once per article — thousands of times per second.
  Batching into periodic flushes (default 250ms) collapses this to ~4 lock
  acquisitions/sec.
- **Wedged network mounts**: Unbounded `statfs` calls on stuck NFS/SMB mounts
  freeze the single assembler worker. Bounded probes (`diskCheckTimeout = 5s`)
  with the `DiskProbe` at-most-one-in-flight pattern prevent this.
- **Pre-allocation / truncation mismatch**: NZB declares *encoded* sizes (~2%
  larger than decoded). Pre-allocation at that size, then writing decoded data,
  leaves trailing zero bytes that par2 reports as damage. The assembler
  truncates to `maxWritten` at file completion to fix this.
- **A truncate target that outlives one run**: with no persisted seed to
  restore, `maxWritten` describes only the current run, but the file on disk is
  the product of every run. A resumed
  file's `TotalParts` counts only the articles still outstanding, so it
  completes as soon as those arrive — and truncating to this run's extent then
  discards whatever an earlier run wrote above them. The mark is persisted and
  re-seeded on open so the target describes the file rather than the session
  (#342).

## The storage & assembly tiers

| Tier | Component | Primary Responsibility | Synchronization |
|---|---|---|---|
| **Ingest** | `WriteArticle` / `CancelJob` / `CloseJobHandles` | Enqueue `WriteRequest` items into bounded channel (`reqs`, cap 2048). Control messages for cancel/close-handles. | Channel send with `select` on `stopCh` and `ctx.Done()`. `wg.Add(1)` tracks every in-flight sender so `Stop()` can drain cleanly. |
| **Worker** | `worker` goroutine | Owns all file handles. Dispatches requests, manages open-file map, drives write cache, executes periodic flush, checks disk space. | Single goroutine — zero lock contention on file handles, write cache, or pending batches. |
| **Write cache** | `writeCache` (writecache.go) | Buffers decoded articles in memory, coalesces contiguous runs into single `WriteAt` calls, tracks per-file write cursor for resume. | Owned exclusively by the worker goroutine. No locks. |
| **Batch flush** | `flush()` on worker | Accumulates `pendingDone` / `pendingFailed` / `pendingExtent` maps and flushes to queue callbacks in groups. | Worker-owned maps. Flushed on: file completion, ticker (250ms), and `Stop()`. |
| **DirectUnpack** | `internal/directunpack` | Streams RAR extraction (via `rarengine`) as volumes complete during download. Reads whole-file volumes after assembly, not partial article data. | Mutex (`mu`) guards volume tracking and kill state. Blocking `volumeReady` channel coordinates volume availability. |

## File assembly lifecycle

Every target file progresses through these stages on the single worker
goroutine. The lifecycle is managed by the `open` map (in-progress), the
`completed` tombstone set (finished), and the `cancelledJobs` set (aborted):

```
First WriteRequest  ──►  openTargetFile()  ──►  Assembling (WriteAt / cache)
for this (job, file)     │                              │
                         ├── MkdirAll + OpenFile        ├── writeArticleOrBuffer
                         ├── preallocateFile            ├── recordArticleCRC
                         └── initCursor (write cache)   ├── handleFatalArticle (failed articles)
                                                        └── partsWritten++
                                                                │
                                               partsWritten >= TotalParts
                                                                │
                                                                ▼
                                                   finalizeFile()
                                                   │
                                                   ├── drainCacheForFile
                                                   ├── Truncate(maxWritten) — trim only
                                                   ├── Sync() (fsync)
                                                   ├── Close()
                                                   ├── flush() (Done/Failed batches)
                                                   ├── compute file CRC32
                                                   └── OnFileComplete callback
```

## Mandatory invariants

1. **Single-worker ownership**: File handles (`os.File`), the `open` map,
   `completed` tombstone set, `cancelledJobs` set, write cache, and all
   `pending*` batch maps are owned exclusively by the `worker` goroutine. No
   external goroutine may read or mutate them. `WriteArticle` communicates
   solely via channel send.

2. **Durability order**: `flush()` is called inside `finalizeFile` *before* the
   `OnFileComplete` callback fires. This guarantees `MarkArticlesDone` /
   `MarkArticlesDoneByIdx` reaches the queue before any downstream code
   (e.g. `watchCompletions`) can observe `IsComplete() == true` and trigger
   job-completion logic. The ordering is:
   `pwrite → Truncate → Sync → Close → flush(Done/Failed) → OnFileComplete`.

   **Important nuance**: `Sync()` (fsync) is called once per *file completion*,
   not once per article. Individual article `WriteAt` calls are not individually
   fsynced — they rely on the OS page cache until the file completes. This means
   a crash mid-download loses unfsynced articles; the `Emitted`-is-transient
   invariant (see `nntp-downloader-contract.md` §5) handles this by re-fetching
   them on restart.

3. **Batch flush cadence**: `pendingDone` / `pendingFailed` / `pendingExtent`
   maps are flushed to queue callbacks on three triggers:
   - **File completion** (`finalizeFile` → `flush()`).
   - **Periodic ticker** (default 250ms, `defaultDoneFlushInterval`).
   - **Shutdown** (final `flush()` after drain loop in `worker()`).

   `flush()` prefers O(1) index-based callbacks (`MarkArticlesDoneByIdx` /
   `MarkArticlesFailedByIdx`) when set, falling back to string-based
   `MarkArticlesDone` / `MarkArticlesFailed` only when the `ByIdx` callbacks
   are nil. The `ByIdx` path is the production path; the string-based path
   exists for tests and backward compatibility.

   Errors from either callback tier are logged and swallowed — the queue
   mutation is best-effort once bytes are on disk, since partsWritten tracking
   is local to the assembler.

4. **Disk-space pre-flight timeouts**: `checkDiskSpace` runs every 16
   `WriteRequest` items (`diskCheckInterval`). Two distinct timeouts bound
   disk probe latency:
   - **Caller timeout** (`diskCheckTimeout = 5s`): Each per-directory
     `FreeBytes` call uses `context.WithTimeout(5s)` to bound how long the
     worker blocks waiting for a result.
   - **Cache TTL** (`DefaultDiskProbeTTL = 5s`): `DiskProbe` caches a
     completed `statfs` result for this duration before launching a new probe.
     These values happen to match today but serve independent purposes.

   The `DiskProbe` ensures at most one outstanding `statfs` goroutine per
   directory — repeated calls for a stuck mount return the cached result or the
   timeout error, preventing goroutine accumulation. Stale entries are evicted
   after 10 minutes (`diskProbeEvictAfter`).

5. **CRC32 incremental combination**: Per-article CRC32 values (from yEnc
   headers) are accumulated with their byte offsets in `crcParts`. At file
   completion, parts are sorted by offset and combined via
   `crc32util.Combine(crc_a, crc_b, len_b)` to produce a whole-file CRC32
   without re-reading the file from disk. If any article had CRC=0 (UU-encoded,
   failed, or write error), `crcValid` is set false and the file CRC is reported
   as 0.

   `Combine` is positional: it reconstructs `CRC(A||B)` from the two CRCs and
   `B`'s length alone, with no offsets, so the fold equals the file's CRC only
   when the parts tile `[0, maxWritten)` exactly. `combineWholeFileCRC` enforces
   that and reports 0 when they do not — the resume case, where an earlier run's
   articles are never re-dispatched and so contribute no part. Skipping bytes is
   not harmless even when they are zero, since appending `n` bytes multiplies
   the CRC state by `M^n` whatever their value.

6. **Offset bounds checking**: `offsetInRange` rejects `WriteRequest` items
   whose offset is negative, whose offset+length overflows int64, or whose write
   extends past `ExpectedSize + ExpectedSize/8` (12.5% slack). This prevents a
   hostile NNTP server from inflating the file's apparent size via a crafted
   yEnc `=ypart begin=` header. Rejected writes return false, triggering
   `recordPendingFailed` + `PutBuffer`.

## Pre-allocation contract

File pre-allocation reduces per-write filesystem metadata overhead and
fragmentation. The implementation is platform-specific:

| Platform | Mechanism | Failure behavior |
|---|---|---|
| **Linux** | `fallocate(2)` — reserves contiguous extents without zeroing | Falls back to `ftruncate` (sparse file) on `ENOTSUP`/`EOPNOTSUPP` (NFS, tmpfs, older FUSE) |
| **Non-Linux** (macOS, etc.) | `ftruncate` — creates a sparse file on supporting filesystems (APFS, HFS+, ext4, xfs, btrfs) | May allocate real blocks on non-sparse filesystems; acceptable since the file will be filled |

Pre-allocation uses `ExpectedSize` from the NZB, which is the *encoded* size
(~2% larger than decoded). At file completion, `finalizeFile` calls
`Truncate(maxWritten)` to trim to the actual decoded size. Without this
truncation, trailing zero bytes cause par2 to report the file as damaged despite
100% download health.

`maxWritten` is the file's decoded high-water mark, and it must survive a
restart. It is seeded on open from `max(InitialMaxWritten, InitialWriteCursor)`
and raised only where bytes reach `WriteAt` — never when an article is merely
accepted into the write cache, since an article can be acked Done while still
resident in memory, and a mark persisted ahead of the bytes would make a later
truncate *extend* a short file with zeros that nothing will ever fill.

| Seed | Meaning | Why it is not the whole answer |
|------|---------|--------------------------------|
| `InitialMaxWritten` | highest byte position written | the answer when present — zero for a file whose earlier run predates the column |
| `InitialWriteCursor` | *contiguous* write frontier | normally lags the high-water mark, since articles arriving out of order leave it behind — but it is not bounded by the file either, see below |

**The truncate never grows a file.** Both seeds come from state persisted by an
earlier run, describing a file this process has not measured, so neither is a
guarantee. `drainFile` advances the cursor past everything it hands back — gaps
included, and before any write is attempted — so a `WriteAt` that
`writeCachedArticles` then logs as failed leaves the cursor above the bytes
actually on disk. The directory may also have been removed between runs, or
pre-allocation may have failed and been logged rather than fatal.

`finalizeFile` therefore stats the handle and skips the truncate when the
target exceeds the file's real size. Growing it would append exactly the
trailing zeros the truncate exists to remove, and a job with no par2 has no
repair stage to notice. Trimming is the only direction ever intended.

The two figures are persisted together through `SetFileExtents` on the worker's
batched flush, so the queue's write lock is taken once per file rather than
twice. Each write path stages only the figure it knows — a coalesced run
advances the cursor, while a drained or uncached write raises the mark and
leaves the cursor untouched — so the store must keep the larger of the stored
and reported value for each rather than overwriting. Taking a report literally
would erase the cursor on every uncached flush.

The mark is staged on both the cached and uncached paths. With
`WriteCacheBytes` at 0 the direct-write path is the only one that runs, and
staging solely from the cache branch left the resume hint silently unpersisted
in that configuration.

Neither the completion path nor `CloseJobHandles` drops the pending entry
before flushing. Both drain the write cache first, and that drain raises the
mark, so dropping the entry first would discard the increment the drain just
earned. `CloseJobHandles` is the path that makes this matter: it runs when a
job enters post-processing, so the files still open are the incomplete ones a
retry resumes. The flush clears the whole map afterwards, so nothing
accumulates.

`SupportsSparse()` in `sparse.go` probes whether the target filesystem supports
sparse files by creating a temporary file, truncating it to 1 MiB, and checking
`st_blocks * 512 < apparent_size`. This is an **informational probe** used at
startup for logging — it does not gate pre-allocation behavior. The assembler
always attempts `fallocate`/`ftruncate` regardless of the probe result.

## Write coalescing cache

When `Options.WriteCacheBytes > 0`, the write cache buffers decoded articles in
memory and coalesces contiguous runs into larger `WriteAt` calls:

- **Buffering**: Each article is stored in `fileBuf.articles[offset]`, keyed by
  byte offset. Total memory is tracked in `writeCache.used`.
- **Contiguous flush**: After each `buffer()`, `flushContiguous()` scans from
  the file's `writeCursor` for a contiguous run ≥ 512KB
  (`contiguousRunSize`). If found, the run is coalesced into `scratchBuf`
  (reusable, avoids heap allocation) and written as a single `WriteAt`. The
  cursor advances past the flushed range.
- **Pressure relief**: When `used > 90% of limit` (`pressure()`), the file
  with the most buffered data is force-flushed (`forceFlushLargest`) regardless
  of contiguity. Articles are written individually, sorted by offset.
- **A drain advances the cursor and keeps the file's entry.** `drainFile()`
  moves `writeCursor` past every article it returns, and clears the entry
  rather than deleting it, so the cursor survives into the next round of
  buffering. This is what keeps contiguous coalescing alive across a pressure
  flush: the drain has written those bytes, so the frontier really has moved,
  and an entry deleted here would be recreated at cursor 0 — an offset whose
  article was just written and will never be re-buffered, stranding the scan
  for the rest of the file. The cursor moves *past* a gap rather than up to
  it, because a drain writes what is buffered above the gap too. An article
  arriving later below the advanced cursor is still buffered and still written
  by the next drain; it just does not join a coalesced run. See #311.
- **File completion drain**: `drainCacheForFile()` flushes all remaining cached
  articles before `Truncate` + `Sync` + `Close`, then `forget()`s the entry —
  the file is closing, so there is no cursor left to preserve.
- **Shutdown drain**: `flushWriteCache()` drains all files on `Stop()` and
  drops their entries for the same reason.
- **Resume cursor**: `initCursor(key, InitialWriteCursor)` seeds the write
  cursor from the persisted resume point so coalescing doesn't stall waiting
  for offset-0 articles that were already written before a restart.
  `pendingExtent` carries that cursor *and* the file's decoded high-water
  mark, and is flushed alongside `pendingDone` on the same cadence. The two
  travel together so the flush takes the queue's write lock once per file
  rather than twice; see `SetFileExtents`.

When `WriteCacheBytes == 0` (default), caching is disabled. Each article is
written directly via `WriteAt`. Decoder buffers are returned to `sync.Pool`
(`decoder.PutBuffer`) immediately after the write.

## Duplicate and late-article handling

The assembler handles duplicates at multiple levels:

- **Per-file `seenDone` / `seenFailed` maps**: Deduplicate by MessageID within
  the worker. A duplicate success re-records `pendingDone` (in case a prior
  flush dropped it) but does NOT increment `partsWritten`. A duplicate failure
  similarly re-records `pendingFailed` without incrementing.
- **Cross-state dedup**: If a MessageID was previously counted as a success and
  now arrives as a failure (or vice versa), the write/ack is recorded but
  `partsWritten` is not incremented again.
- **Late articles (completed tombstone)**: Articles arriving for a file already
  in the `completed` set are handled by `handleLateDuplicate`: the Done/Failed
  ack is recorded (so the queue sees it) and the data is returned to the pool.
  No disk write occurs.

## Control messages

The assembler supports two control message types, distinguished by sentinel
field values on `WriteRequest`:

| Control | Encoding | Worker behavior |
|---|---|---|
| **CancelJob** | `JobID=""`, `FileIdx=-1`, `MessageID=jobID` | Closes and *deletes* all open files for the job. Adds job to `cancelledJobs` tombstone. Discards all cached articles via `wc.forget()`. Closes `ackCh` to unblock the caller. |
| **CloseJobHandles** | `JobID=""`, `FileIdx=-2`, `MessageID=jobID` | Drains write cache for each file, `Sync()`s and `Close()`s handles *without deleting*. Adds files to `completed` tombstone. Flushes pending Done/Failed. Closes `ackCh`. Used when a job enters post-processing or par2 repair. |

Both control messages are synchronous from the caller's perspective — the caller
blocks on `ackCh` until the worker has fully processed the message.

## DirectUnpack streaming contract

DirectUnpack is a *volume-level* streaming extractor, not an article-level one.
It reads fully assembled RAR volume files from disk. It does NOT read partial
articles or sparse file regions. The coordination model:

1. **Volume completion signal**: The assembler's `OnFileComplete` callback
   calls `DirectUnpacker.Add(filename, path)` when a RAR volume file has been
   fully written, fsynced, and closed. The volume is complete and stable on
   disk at this point.

2. **Volume waiting**: `waitForVolume()` blocks on the `volumeReady` channel
   until the requested volume number appears in `completedVols`. If the set is
   marked corrupt (`corruptSets`), it returns an error immediately.

3. **Sequential volume feeding**: `startVolumeFeed()` spawns a goroutine that
   opens completed volumes in order (vol 1, 2, 3, ...) and sends their
   `*os.File` handles to `rarengine.StreamDecompressor` via a channel. The
   decompressor reads each volume sequentially.

4. **Corrupt volume handling**: `MarkCorrupt(setname, reason)` is called by the
   queue when a volume was assembled from a download with missing/failed articles.
   Once marked, the set can never be reported as successfully extracted:
   - `waitForVolume` checks `corruptSets` on each wake and returns an error.
   - `extractSet` checks `corruptSets` after extraction completes (backstop for
     volumes that arrived before the corruption was detected).

5. **Non-RAR handling**: `extractSet` calls `rarheader.Version()` on the first
   volume's magic bytes. Any error — including I/O errors, not just format
   mismatches — results in `errNotRAR` and the set is recorded as `SkippedSet`
   (not failed). The normal unpack stage's external `unrar` handles these.

6. **Format support**: DirectUnpack uses `rarengine` (pure Go RAR3/RAR5
   decompressor). Other archive formats, legacy RAR2, and non-RAR files
   identified by filename are handled by the post-processing unpack stage.

7. **Abort/kill**: `Abort()` sets `killed=true`, records failures for current
   and queued sets, clears success results, and signals the reader goroutine
   via `volumeReady`. If `run()` was never started, `Abort()` closes the `done`
   channel directly.

8. **Path traversal safety**: `extractEntries` opens an `os.Root` anchored at
   `extractDir` and writes all entries through it. This prevents archive
   entries with `..` components, absolute paths, or symlinked path components
   from escaping the extraction directory.

## Memory & allocation budget

| Component | Bound / Strategy |
|---|---|
| **Write channel (`reqs`)** | Capacity = 2048 requests (`defaultQueueSize`). Under 128KB articles: ~256MB worst-case buffered data. Backpressures downloader when disk I/O is slow. |
| **Write cache** | Bounded by `Options.WriteCacheBytes`. Pressure relief at 90% occupancy. Zero when caching is disabled (default). |
| **Contiguous flush threshold** | 512KB (`contiguousRunSize`). Runs below this stay buffered. |
| **Coalescing scratch buffer** | Single reusable `[]byte` (`scratchBuf`) per `writeCache` instance. Grows to the largest flush and is reused across flushes. |
| **Pending Done/Failed batches** | Unbounded maps, flushed every 250ms or on file completion. In practice bounded by download speed × flush interval. |
| **Decoder buffer returns** | Every `req.Data` is returned to `decoder.PutBuffer` (sync.Pool) after write or on error/discard. Failure to return leaks pool entries. |
| **Disk probe cache** | One `probeState` per unique directory. Stale entries evicted after 10 minutes. At most one outstanding `statfs` goroutine per directory. |

## Failure & degradation rules

- **Disk full (`OnLowDisk`)**: When free space drops below `MinFreeBytes`, the
  `OnLowDisk` callback is invoked. The assembler does NOT pause itself — the
  callback is responsible for pausing the job or the download. The assembler
  continues processing requests in the channel.

- **Write error (`pwrite` failure)**: The article is recorded as failed
  (`recordPendingFailed`), its data is returned to the pool, `crcValid` is set
  false, and `partsWritten` is still incremented. The file completes with
  damaged-file semantics (CRC=0), routing to par2 repair.

- **FileInfo resolution failure**: If `opts.FileInfo()` returns an error, the
  article's data is returned to the pool and the request is silently discarded.
  The file is never opened and never appears in `open`.

- **Directory creation failure (`MkdirAll`)**: If `openTargetFile` cannot create
  the parent directory, the article's data is returned to the pool and the
  request is silently discarded. Same semantics as FileInfo failure.

- **File open failure (`OpenFile`)**: If `openTargetFile` cannot create or open
  the target file (e.g. permission denied, stale NFS handle), the article's data
  is returned to the pool and the request is silently discarded. Same semantics
  as FileInfo failure.

- **CancelJob**: Closes and deletes open files. Adds job to `cancelledJobs`
  tombstone so subsequent articles for that job are silently discarded (data
  returned to pool). The `ackCh` synchronization ensures the caller can safely
  delete the job directory immediately after `CancelJob` returns — no open
  handles remain.

- **Shutdown (`Stop`)**: Closes `stopCh`, waits for all in-flight `WriteArticle`
  and `CancelJob` goroutines to finish (`wg.Wait`). The worker drains remaining
  channel items, flushes the write cache, performs a final `flush()`, and closes
  all open files. Partial files (partsWritten < TotalParts) are closed without
  firing `OnFileComplete`.

## Status

### Landed
- Single-worker goroutine with bounded write queue (2048).
- Batched Done/Failed flushes with 250ms ticker and file-completion flush,
  preferring O(1) index-based `ByIdx` callbacks over string-based variants.
- OS-specific pre-allocation (`fallocate` on Linux, `ftruncate` elsewhere) with
  decoded-size truncation at completion.
- Write coalescing cache with contiguous flush (512KB threshold), 90% pressure
  relief, and resume cursor persistence.
- Per-article CRC32 accumulation and offset-ordered `crc32util.Combine` at file
  completion.
- Offset bounds checking against NZB-declared size with 12.5% slack.
- Timeout-bounded disk probe with at-most-one-in-flight per directory, cache TTL,
  and stale entry eviction.
- CancelJob and CloseJobHandles control messages with synchronous ack.
- DirectUnpack volume-level streaming via `rarengine` with corrupt-volume gating,
  non-RAR skip semantics, and `os.OpenRoot`-based path traversal safety.
- Per-file duplicate dedup via `seenDone`/`seenFailed` maps and cross-state
  dedup.

### Open Gaps

- **In-flight coalescing still stalls on a permanently failed article.** A gap
  the download will never fill leaves `buildContiguousRun` stranded at the
  cursor until the next drain re-anchors it. #311 fixed the pressure-drain
  route to the same symptom; this route remains. Its cost is memory residency
  rather than syscalls, and two designs targeting it directly were measured
  and found worse than leaving it alone — one had an unbounded stranding
  bound, the other regressed hole-free files. Measure the residency cost
  before attempting it again.
- **A resumed file still has no whole-file CRC, only an honest absence of one.**
  A resumed run accumulates parts for the articles it received, and `crcValid`
  stays true because nothing failed, so `finalizeFile` used to report the
  subrange CRC as trustworthy and QuickCheck read it as a mismatch on a file
  that was correct on disk after #342. `combineWholeFileCRC` now checks the
  parts tile `[0, maxWritten)` and reports 0 — read as NoCRC, "unavailable" —
  when they do not, so the needless repair no longer comes with a false
  corruption claim (#349). Producing a *correct* CRC across a restart needs a
  gapless prefix CRC persisted per file, which the write cursor cannot carry
  because `drainFile` advances it past holes by design. Tracked as #353, and it
  shares the persistence question with the high-water mark above.
- **The syscall-reduction figure above is unmeasured.** "Reducing syscall
  count by 8–16×" states a ratio no benchmark in this repository produces. A
  sweep of `WriteAt` chunk sizes over the same payload found wall-clock flat
  on btrfs and *worse* for large chunks on tmpfs, because coalescing trades N
  syscalls for one syscall plus a second memcpy of the same bytes. The
  mechanism is sound where a write is expensive per call — NFS/SMB, where each
  `pwrite` is a round trip — but the local-filesystem claim should be measured
  or qualified rather than repeated.
