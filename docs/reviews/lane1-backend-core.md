# Lane 1 — Backend Core Review

Scope: `internal/queue/`, `internal/downloader/`, `internal/assembler/`, `internal/decoder/`, `internal/nntp/`
Repo state at review: `main` @ `9be19e24`, working tree clean.

## Subsystem map

**`internal/nntp`** owns one TCP/TLS socket per `Conn`. `Dial` does a synchronous
greeting/AUTHINFO/CAPABILITIES handshake on the caller's goroutine, then spawns one
`runReader` goroutine that consumes responses in order and matches them against a
FIFO of `pendingCmd` entries (`pipeline.go`). Callers (`Fetch`/`Stat`) acquire a
pipelining semaphore sized to `PipeliningRequests`, `submit` under `sendLock` (so
wire order == FIFO order), and block on a per-command `done` channel. Cancellation
marks the entry `orphaned` so the reader still drains the response and keeps the
socket in protocol sync. `io.go` holds the bounded line/body readers
(2 KiB line cap, 10 MiB body cap, 768 KiB pre-sized body buffer). Teardown is
`closeOnce`-guarded and shared between `Close()` and `finishReader()`.

**`internal/downloader`** is the scheduler. One `run` goroutine selects over
`ctx.Done`, `queue.Notify()`, `dispatchReady`, and a 5 s ticker; each wake runs
`checkExpiredPenalties` + `dispatchPass`. `dispatchPass` snapshots server configs and
mutable options, then splits into a pure `buildDispatchPlan` (runs entirely inside
`queue.ForEachUnfinishedArticle`'s RLock, no channel blocking, no write locks) and
`applyDispatchPlan` (all side effects, lock-free). Per-article eligibility lives in
`tryDispatch`, backed by `dispatchTracker` (one mutex over a `tryList
map[string]serverMask` + `inFlight map[string]int`). Each configured connection gets
a `connWorker` goroutine that owns a lazily-dialed `managedConn` and fans each request
out to a bounded (`pipelineDepth*2`) sub-goroutine running `handleRequest` →
`fetchArticle` (network + penalty) → `processFetchedArticle` (decode + emit).
Results land on the buffered `completions` channel consumed by `internal/app`'s pipeline.

**`internal/decoder`** is a pure function library: `DecodeArticle` parses the
`=ybegin`/`=ypart`/`=yend` frame, decodes with a 256-byte `specialLUT` scan plus a
fused unrolled `sub42Span`, verifies CRC32, and returns a `sync.Pool`-backed buffer.
Buffer ownership is explicit: whoever holds the returned `Data` must `PutBuffer` it,
and that ownership is handed from the downloader → `ArticleResult` → app pipeline →
`assembler.WriteRequest` → assembler write path.

**`internal/assembler`** has one worker goroutine owning every open FD. Requests
arrive on a 2048-deep channel; `dispatchRequest` handles two control messages (cancel
`FileIdx==-1`, close-handles `FileIdx==-2`, both synchronously acked via `ackCh`) and
delegates the rest to `processRequest`. Writes go through an optional `writeCache`
that coalesces contiguous runs from a per-file cursor into single `WriteAt` calls,
force-flushing the largest file under >90 % memory pressure. Per-article Done/Failed
acks are batched into `pending*` maps and flushed to the queue on file completion, on
a 250 ms ticker, or at Stop. File completion truncates to `maxWritten`, fsyncs, closes,
tombstones the key, flushes, computes the combined file CRC32, then fires `OnFileComplete`.

**`internal/queue`** is the shared state. A single `sync.RWMutex` guards an ordered
`[]*Job` plus a `byID` map. Each `Job` splits into an immutable `Manifest` (flat
parallel arrays + prefix-sum file offsets + lazy messageID index) and a mutable
`JobProgress` (flat `done`/`failed`/`emitted` bool arrays, per-file `FileProgress`,
job-level counters). `ActiveSet` bounds how many jobs keep manifest+progress resident;
`PromoteNext` is the promotion loop (unlocked manifest read sandwiched between two
locked phases, with a `promoting` set as the reservation token). Persistence is SQLite
(`SQLiteStore`) for job rows plus gzip-JSON manifests on disk. The handoffs are:
queue → downloader via `Notify()` + `ForEachUnfinishedArticle`; downloader → assembler
via `completions` + app pipeline; assembler → queue via the batched
`MarkArticles*ByIdx` / `SetFileWriteCursor` callbacks.

---

## Findings

### 1. Nil-pointer panic: `ClearArticleEmitted` on a job the queue just evicted
`internal/queue/queue.go:1122-1141`, reached from `internal/downloader/dispatch.go:553`
**CONFIRMED** (read end-to-end; not executed) — lens 1, 2

`Queue.Pause(id)` sets `StatusPaused` then calls `evictJobLocked`, which does
`job.manifest = nil; job.progress = nil` whenever `q.store != nil || q.stateDir != ""`
(`queue.go:616-625`) — i.e. always in production. `fetchArticle` reacts to exactly that
status:

```go
if status, err := d.queue.GetJobStatus(req.jobID); err == nil && status == constants.StatusPaused {
    d.unmarkTried(req.jobID, req.messageID, serverIdx)
    _ = d.queue.ClearArticleEmitted(req.jobID, req.messageID)   // dispatch.go:553
```

`ClearArticleEmitted` then does `job.manifest.articleIndexByID(messageID)` with **no nil
guard**. `articleIndexByID` has a pointer receiver and dereferences `m.messageIDIndex`
→ nil-pointer dereference → process panic. This is not a rare interleaving: articles
sitting in `workCh` when the user pauses a downloading job are the *designed* trigger
for this branch. The same window applies to the global-pause branch at `dispatch.go:562`.

Note the sibling `ClearArticleEmittedByIdx` (`queue.go:1144-1157`) *does* guard
(`job.progress == nil || job.manifest == nil`), and the app pipeline uses the ByIdx
form — so the exposure is specifically the two remaining by-messageID call sites in
the downloader.

Direction: add the same nil guard the `ByIdx` variants already have, and add a
regression test that pauses a job with an in-flight `articleRequest`.
Effort: **S**.

### 2. Same nil-deref shape in `MarkArticleEmitted`, `CountUnfinishedArticles`, `CheckEarlyAbort`
`internal/queue/queue.go:1087-1100`, `queue.go:187-211`, `queue.go:1216-1224` → `job.go:207-209`
**CONFIRMED** (read) — lens 1, 2

Three more methods dereference `job.manifest` / `job.progress` unguarded while their
ByIdx / sibling variants guard:

- `MarkArticleEmitted` — `job.manifest.articleIndexByID(...)` then `job.progress.markEmitted(...)`; called from `dispatch.go:679` on the terminal decode-error path.
- `CountUnfinishedArticles` — `job.manifest.NumFiles()` at `queue.go:194`; called from `internal/app/pipeline.go:401` when the assembler needs `TotalParts`.
- `CheckEarlyAbort` → `Job.IsEarlyAbort()` → `j.progress.isEarlyAbort()`; `isEarlyAbort` (`progress.go:469`) has **no** nil-receiver guard even though almost every other `*JobProgress` accessor in that file does. Called from `internal/app/pipeline.go:256`.

The underlying issue is a *convention* one: `JobProgress`'s exported accessors are
uniformly nil-safe, but the queue-level methods and one unexported method are not, and
which is which is unpredictable. Direction: make nil-eviction a first-class,
uniformly-handled case (return `ErrNotFound`/no-op), and add a package test that
exercises every exported `Queue` method against an evicted job.
Effort: **S** for the guards, **M** for the systematic test.

### 3. Assembler acks Done for bytes that are only in the write cache
`internal/assembler/assembler.go:940-964`, `writecache.go:95-118`, `assembler.go:683-733`
**CONFIRMED** (read) — lens 2

`handleSuccessArticle` calls `a.recordPendingDone(...)` whenever
`writeArticleOrBuffer` returns true — but that function returns true as soon as
`wc.buffer(...)` accepts the article into RAM (`assembler.go:1161-1172`). `flush()`
then hands the article to `MarkArticlesDoneByIdx`, and `Save` persists `done[i]=true`
to SQLite. Nothing has reached the filesystem.

This contradicts the invariant the code itself documents in two places:
`dispatch.go:688-695` ("the assembler calls MarkArticleDone after pwrite + fsync so
that Done => bytes on disk") and the go-standards B.6 durability rule. On an unclean
shutdown, those articles are `Done` on restart and are never re-dispatched, while the
file has a hole; `WriteCursor` only advances on a *flushed* contiguous run, so it does
not cover them either. (Even without the cache, the 250 ms ticker flush acks after
`WriteAt` but before `Sync`, so Done means "in page cache" rather than "on stable
storage" — a weaker but arguably acceptable form. The cache path is the strict
violation because the bytes are not even in the kernel.)

Direction: only record pending-Done for a request once its bytes have left the cache
(record it on flush of the run/drain, keyed by the articles the run consumed), or
document the weakened invariant explicitly and rely on par2. Effort: **M**.

### 4. `writeCachedArticles` silently discards `WriteAt` errors
`internal/assembler/assembler.go:1222-1241` (callers: `flushPressure` 1246, `drainCacheForFile` 1270, `flushWriteCache` 1280)
**CONFIRMED** (read) — lens 1, 2

The direct-write path (`writeArticleOrBuffer`, `assembler.go:1176-1187`) and the
coalesced-run path (`flushRun`, 1196-1212) both return `false` on `WriteAt` error, and
`handleSuccessArticle` converts that into `crcValid = false` + a pending-Failed ack.
`writeCachedArticles` — the third write path, used for every pressure flush, every
pre-close drain, and the whole shutdown drain — logs the error and moves on. It has no
return value at all. The article was already acked as Done (see finding 3), so a
failed drain write produces a file with a hole that the queue believes is complete and
that quickcheck will report as `Mismatched` (corrupted) rather than `NoCRC`
(download had failures) — the exact distinction `assembler.go:941-950` goes out of its
way to get right on the other two paths.

Direction: give `writeCachedArticles` a per-article result and route failures through
the same `crcValid=false` + `recordPendingFailed` path. Effort: **M**.

### 5. `flushRun` failure marks only the current article failed, not the whole run
`internal/assembler/assembler.go:1161-1172`
**CONFIRMED** (read) — lens 2

A coalesced run can contain dozens of articles, all of which were already recorded as
pending-Done when they were buffered. If `flushRun` fails, `writeArticleOrBuffer`
returns false and only `req` (the article that happened to trigger the flush) is
demoted to Failed. Every other article in that run stays Done despite its bytes never
reaching disk. Same root cause as findings 3 and 4: the ack is decoupled from the write.
Direction: fold into the finding-3 fix. Effort: **M** (shared with 3/4).

### 6. `checkExpiredPenalties` takes a write lock on every server on every dispatch wake
`internal/downloader/downloader.go:726-737`, `server.go:126-131`
**CONFIRMED** code shape / **INFERRED** magnitude — lens 2, 3 (hot-path discipline)

```go
for _, srv := range d.servers {
    if srv.Active(now) { srv.ClearDeactivation() }   // takes s.mu.Lock() unconditionally
}
```

`ClearDeactivation` acquires `Server.mu` for *write* and zeroes `penaltyExpiry` even
when there was no penalty and no deactivation to clear. `run()` calls this on every
`dispatchReady` signal, and `handleRequest` fires `signalDispatch` on *every* article
completion — ~330/s at the documented 2 Gbps target. That is ~330 × N write-lock
acquisitions per second on the same mutex `Server.Active()` reads under RLock from the
per-article `selectServerForArticle` path. This is precisely the shape §7 of
go-standards warns about (`srv.Cfg()` per-article was measured at 0.69 s), and it was
not caught because the `serverCfgs` snapshot fix addressed the config copy, not this.

Direction: make it a no-op when nothing needs clearing — check
`srv.PenaltyExpiry().IsZero()` (RLock) first, or fold "expired penalty" handling into
`Active()` with a CAS. Effort: **S**.

### 7. Dead DNS-resolution subsystem (`Resolve`, `ResolvedAddrs`, `SetResolvedAddrs`)
`internal/downloader/resolver.go:1-35`, `server.go:48-56, 133-158`
**CONFIRMED** — lens 3

`downloader.Resolve` has no non-test caller. `Server.resolvedAddrs`/`resolvedAt` and
their accessors are written only by `Resolve` and read only by `server_test.go:354`.
`nntp.Dial` builds `net.JoinHostPort(host, port)` and hands it to `net.Dialer`, which
does its own resolution — the cache is never consulted. The doc comments cite
"Spec §3.7" and promise "fastest address selected / cached per-server for the session",
which the code does not do. Two mutex-guarded fields, four methods, and a file exist
to support a feature that isn't wired up. Direction: delete it (or file an issue and
mark the file as unimplemented). Effort: **S**.

### 8. `Server.ApplyPenalty` doc comment describes a parameter the function does not have
`internal/downloader/server.go:88-101`
**CONFIRMED** — lens 1

> "The now argument is passed explicitly (rather than calling time.Now internally) so
> callers and tests can control the clock."

The signature is `ApplyPenalty(d time.Duration)` and line 90 is `now := time.Now()`.
The comment is a leftover from a refactor that reversed the decision. Trivially
misleading to anyone writing a test against it. Effort: **S**.

### 9. The assembler carries two parallel ack APIs; the message-ID one is dead in production
`internal/assembler/assembler.go:162-183, 304-311, 683-733, 791-817`; wiring at `internal/app/app.go:321-333`
**CONFIRMED** — lens 2, 3

`Options` exposes `MarkArticlesDone`, `MarkArticlesFailed`, `MarkArticlesDoneByIdx`,
`MarkArticlesFailedByIdx`; the struct carries four `pending*` maps; `flush()` and both
`recordPending*` helpers branch `if …ByIdx != nil { } else if …`. `app.go` wires **all
four**, so the ByIdx branch always wins and the message-ID branch is unreachable outside
tests and `WithMarkArticlesDoneHook`. The cost is real: two of the four `clear()` calls
in `flush()` are always no-ops, the empty-check at `assembler.go:684-686` has to test
four maps, and every future change to ack semantics has to be made twice. The
message-ID variants on the queue side (`MarkArticlesDone`/`MarkArticlesFailed`,
`queue.go:1246`/`1337`) then exist mainly to serve this dead branch — and they are one
of the two unguarded-nil families in finding 2.

Direction: pick ByIdx as the single API; keep the test hook by making it an
`ArticleAcker` interface or by having the hook wrap the ByIdx form. Effort: **M**.

### 10. `ActiveSet` has its own `sync.RWMutex` but is only ever touched under `q.mu`
`internal/queue/active_set.go:9-90`; call sites `queue.go:114, 503, 591, 609, 620, 856`
**CONFIRMED** (grep: `ActiveSet()` has no caller outside `queue.go`) — lens 2, 3

Every `activeSet.Len/MaxActive/IsResident/Add/Evict` call happens with `q.mu` already
held, so the inner lock protects nothing and adds a nested lock-ordering constraint
(`q.mu` → `activeSet.mu`) that a future contributor could invert. `SetMaxActive` is the
one exception, reached via `Queue.SetMaxActiveJobs` without `q.mu`, which is also the
only reason the lock can't just be deleted today. The exported `Queue.ActiveSet()`
accessor has no callers at all. Direction: either drop the inner mutex and route
`SetMaxActive` through `q.mu`, or document why the second lock exists. Effort: **S**.

### 11. `PromoteNext`'s reservation dance is a hand-rolled lock-release-reacquire loop
`internal/queue/queue.go:499-602`
**CONFIRMED** — lens 2

The loop is: lock → capacity check against `activeSet.Len() + len(q.promoting)` →
pick candidate → mark `promoting[id]=true` → **unlock** → read manifest from disk →
**relock** → `delete(promoting, id)` → re-verify existence *and* status → attach →
`RestoreJobProgress` (under the lock, with a `//lockio:` marker admitting it) →
`store.Update` (also under the lock) → unlock → repeat. It is correct as written and
the comments are honest about the compromises, but it is the most intricate control
flow in the package: three lock phases, a shadow reservation map that duplicates what
`ActiveSet` half-tracks, a `cachedManifest` copy taken under the lock to skip the disk
read, and two documented-as-follow-up I/O-under-lock exceptions. It is also called from
seven places (`Add`, `Remove`, `Pause`, `Resume`, `Retry`, `SetStatus`, `SetStatusIf`,
`ResumeAll`, `SetMaxActiveJobs`), each unconditionally, so a status change on any job
walks the whole queue looking for candidates.

I found no bug here. Flagging it as the highest-complexity concentration in the lane
and the place where a future change is most likely to introduce one. Direction: consider
a dedicated single promoter goroutine fed by a cap-1 signal (the same pattern already
used for `Notify`/`dispatchReady`), which would eliminate `promoting` entirely and let
the manifest read happen with no lock held at all. Effort: **L**.

### 12. `Queue.Add` writes the manifest to disk only when `store == nil`
`internal/queue/queue.go:324-330`
**INFERRED** — lens 2

```go
if q.store == nil && q.stateDir != "" && job.manifest != nil {
    … writeGzJSON(manifestPath, job.manifest)
}
```

but `PromoteNext` (`queue.go:526-533`) and `hydrateSnapshot` (`snapshot.go:38`) read
`<stateDir>/manifests/<id>.json.gz` unconditionally. I did not fully trace
`SQLiteStore.Add` to confirm it writes the manifest file in the store!=nil case — if it
does, this is fine and only the asymmetry is confusing; if it doesn't, promotion of a
job added in this process would fall through to `prepareClaimFailureLocked` and fail
the job as "corrupt manifest". The `cachedManifest` shortcut in `PromoteNext` masks it
for jobs still holding a resident manifest, which would make any real bug here
restart-only. Worth a definitive check by the synthesis pass or a follow-up.
Effort: **S** to verify.

### 13. `errors` are silently swallowed on the promotion / status-transition store writes
`internal/queue/queue.go:364, 596, 733, 826, 854, 929, 949, 968`
**CONFIRMED** — lens 1

Eight `_ = q.store.Update(...)` / `_ = q.store.ShiftSortKey(...)` /
`_ = q.store.RestoreJobProgress(...)` sites. go-standards forbids bare `_ = f()`
without a comment explaining why the error is ignored; these carry `//lockio:` markers
explaining the *lock* decision but say nothing about the discarded error. In practice a
failing `store.Update` here means the SQLite view of a status transition silently
diverges from RAM until the next `Save`/`Prune`, which is the same class of
"observable divergence" that `finishClaimFailure` (`queue.go:674-702`) explicitly
decided to *log*. Direction: at minimum log at Warn, matching `finishClaimFailure`'s
own stated rationale. Effort: **S**.

### 14. `serverMask.count()` hand-rolls popcount instead of `math/bits.OnesCount64`
`internal/downloader/mask.go:76-92`
**CONFIRMED** — lens 1

go-standards: "Standard library first". `bits.OnesCount64` compiles to a single
`POPCNT` instruction on amd64/arm64; the Kernighan loop is O(set bits) branches. The
comment even advertises the O(k) property as a feature. Same for `isEmpty`, which
could be `m.fast[0] == 0 && !slices.ContainsFunc(...)` but is fine as-is.
Effort: **S**.

### 15. `sub42Span`'s grow path allocates and zeroes a throwaway slice
`internal/decoder/decoder.go:309-312`
**CONFIRMED** — lens 1, 2 (hot path)

```go
if cap(dst)-base < n {
    dst = append(dst[:base], make([]byte, n)...)[:base]
}
```

This allocates `n` zeroed bytes purely to force `append` to grow, costing a `memclr`
plus a `memmove` of the zeros that are then immediately overwritten. `slices.Grow(dst,
n)` does the same growth with neither. This is in the function the standards document
specifically calls out as tuned for L1 efficiency. In practice `decodeBody` pre-sizes
`out` to `min(len(encoded), maxDecodeSize)`, so the branch should be cold — but it is
reachable whenever a caller passes a too-small `scratch`, and it is the kind of thing
a profiler wouldn't attribute clearly. Effort: **S**.

### 16. `DecodeArticleBuf`'s `scratch` parameter has no caller and forces an `unsafe` import
`internal/decoder/decoder.go:159-230` (the `unsafe.SliceData` compare at 204-207)
**CONFIRMED** (grep: no `DecodeArticleBuf` caller outside the package) — lens 3

The only production entry point is `DecodeArticle(body)`, which passes `scratch = nil`.
The scratch path exists to let a caller supply a reusable buffer, and to make its
ownership rules work the function has to compare backing-array pointers with
`unsafe.SliceData` to decide whether it may return the buffer to the pool. That is the
package's only use of `unsafe`, sustained entirely for an unused parameter. Direction:
delete the parameter and the `unsafe` import, or wire it up if there is a measured win.
Effort: **S**.

### 17. `GetBuffer` discards an undersized pooled buffer instead of returning it
`internal/decoder/decoder.go:126-142`
**CONFIRMED** — lens 2

When the pooled slice's capacity is smaller than the request, `GetBuffer` allocates a
fresh one and lets the pooled slice become garbage rather than putting it back. Since
`PutBuffer` accepts anything up to 10 MiB, the pool can accumulate a mix of sizes and a
run of large requests will steadily evict small buffers. Low impact given the pool's
`New` makes 768 KiB slices, but it is a silent pool-pollution asymmetry. Effort: **S**.

### 18. `handleFatalArticle`'s early-return paths skip `PutBuffer(req.Data)`
`internal/assembler/assembler.go:770-779, 891-917`
**CONFIRMED** — lens 1

```go
if req.FatalErr != nil {
    if !a.handleFatalArticle(f, req) { return }   // ← Data never returned to the pool
    if req.Data != nil { decoder.PutBuffer(req.Data) }
```

Both `false` returns (duplicate failure; already-counted-as-success) leak the buffer
back to the GC instead of the pool. `FatalErr` requests normally carry `Data == nil`,
so this is a missed-reuse rather than a correctness bug — but it is inconsistent with
every other exit in the file, which is careful to `PutBuffer`. Effort: **S**.

### 19. `writeCache.buildContiguousRun` returns a slice aliasing the shared `scratchBuf`
`internal/assembler/writecache.go:165-179`
**CONFIRMED** (safe today) — lens 2

`flushRun.data` is `wc.scratchBuf`, reused across calls. It is safe because the only
consumer (`a.flushRun`) writes it synchronously on the same goroutine before the next
`buildContiguousRun`, but nothing in `flushRun`'s type or doc says "do not retain this".
`scratchBuf` also grows to the largest run ever seen and is never shrunk. Direction: a
one-line doc comment on `flushRun.data` stating the aliasing contract. Effort: **S**.

### 20. `flushPressure` / `flushWriteCache` drop drained articles when `open[key]` is missing
`internal/assembler/assembler.go:1246-1266, 1280-1296`
**INFERRED** — lens 2

Both branches log `"pressure flush for unknown file"` / `"cached articles for unknown
file on shutdown"` and `continue` — the drained `[]bufferedArticle` is discarded
without `decoder.PutBuffer` and without any failure ack, even though `drainFile` has
already removed the entries from the cache and decremented `wc.used`. I could not
construct a reachable interleaving (`initCursor` creates the entry when the file is
opened, `finalizeFile` and the cancel path both drain/forget before deleting from
`open`), so this reads as defence-in-depth that would nonetheless lose data + leak
buffers if it ever fired. Direction: `PutBuffer` in those branches at minimum.
Effort: **S**.

### 21. `Assembler.Stop` can lose a request that wins the race against the drain loop
`internal/assembler/assembler.go:410-434, 581-602`
**INFERRED** — lens 2

`WriteArticle` selects over `a.reqs <- req` and `<-a.stopCh`; Go picks uniformly when
both are ready. If the send wins after the worker's drain loop has already taken its
`default:` branch, the request lands in the buffer and is never processed — yet
`WriteArticle` returns `nil` (success). `wg.Wait()` doesn't help because both
goroutines complete. In-flight articles are documented as discarded at Stop, so the
practical impact is a Done-ack that never arrives (the article is simply re-downloaded
next run), which is the safe direction. Noting it because the same "closed channel is
permanently ready, so a plain select is biased" hazard already produced issue #182 in
`selectWork` (`dispatch.go:435-451`), and the fix pattern there (non-blocking
pre-check) is not applied here. Effort: **S**.

### 22. `d.hasActiveConnections()` scans a map under RLock on every dispatch pass
`internal/downloader/downloader.go:603-612`, called from `dispatch.go:163`
**CONFIRMED** code shape / **INFERRED** magnitude — lens 2

`applyDispatchPlan`'s idle-disconnect check runs `plan.dispatched == 0 &&
!d.queue.HasDownloadableJobs() && d.hasActiveConnections()` on every pass.
`HasDownloadableJobs` takes `q.mu.RLock` and walks all jobs; `hasActiveConnections`
takes `connActivityMu.RLock` and walks every connection entry. Short-circuiting saves
this in the busy case (`plan.dispatched > 0`), so the cost is confined to idle passes —
but the 5 s ticker plus every trailing `signalDispatch` from a completing worker hits
it. A cheap `atomic.Int32` connected-connection counter maintained by
`setConnConnected` would make it O(1). Effort: **S**.

### 23. `idleTimeoutReader` reuses the *dial* timeout as the *idle read* deadline
`internal/nntp/conn.go:255-283, 617-630`
**CONFIRMED** — lens 1

`dopts.dialer.Timeout` is derived from `cfg.Timeout` (default 60 s) and is used for
three different things: the TCP connect deadline, the handshake deadline, and the
per-Read idle deadline. `Conn.readTimeout` is stored but the comment says "kept for
reference; actual idle enforced by idleTimeoutReader" — a field that exists only to be
unused. These are legitimately different knobs (a connect timeout wants to be short; an
idle read timeout under a speed limiter wants to be generous). Direction: either give
the idle deadline its own config field or drop the vestigial `readTimeout` field and
say so in one place. Effort: **S**.

### 24. `Conn.closeErr` is written under `closeOnce` and read without synchronization
`internal/nntp/conn.go:73-76, 592-615`; `pipeline.go:188-201, 228-235`
**INFERRED** (believed safe) — lens 2

`finishReader` writes `c.closeErr` then `c.closed.Store(true)` then `c.cancel()`.
`closeError()` reads `c.closeErr` with no lock, from `submit` (after
`c.closed.Load()`) and from `Fetch`/`Stat` (after observing `c.ctx.Done()`). Both
readers are ordered behind a synchronizing edge (a Go atomic, or the context's internal
mutex+channel close), so I believe there is no race — the ordering comment at
`pipeline.go:190-191` shows this was reasoned about deliberately. Flagging it only
because the safety depends on an argument that lives in a comment rather than in the
types; `atomic.Pointer[error]` would make it self-evident and cost nothing on a cold
path. Effort: **S**.

### 25. `queue.MarkArticleDone` / `MarkArticleFailed` / `ExistsByName` have no production callers
`internal/queue/queue.go:1071-1073, 1230-1233, 275-284`
**CONFIRMED** (grep) — lens 3

Singular wrappers kept alive by tests only; `Store.ExistsByName` (`store.go:34`) is the
one actually used. `writeCache.bytesFor` (`writecache.go:260`) is likewise test-only.
Small, but they widen the surface that findings 2 and 9 have to be applied across.
Effort: **S**.

### 26. `run()`'s three select arms are identical
`internal/downloader/downloader.go:757-771`
**CONFIRMED** — lens 1

All three non-cancel cases are `d.checkExpiredPenalties(); d.dispatchPass(ctx)`. A
single `wake := ...; select { case <-ctx.Done(): return; case <-d.queue.Notify():
case <-d.dispatchReady: case <-ticker.C: }` followed by the two calls would remove the
triplication and make it impossible for the three paths to drift. Pure readability.
Effort: **S**.

---

## Positive / load-bearing

**The `buildDispatchPlan` / `applyDispatchPlan` split (`dispatch.go:44-215`) is the
single best piece of design in the lane.** It makes the "never block on a channel while
holding the queue RLock" rule structurally enforced rather than remembered: the phase
that holds the lock returns a plain value, and the phase that emits results, transitions
statuses, and calls `DisconnectAll` provably runs lock-free. The doc comment at 236-248
correctly explains *why* (`emitResult` on a full completions channel would deadlock
against the consumer's need for the queue write lock). Do not merge these back together
and do not add channel operations to `buildDispatchPlan`.

**The `Manifest` / `JobProgress` split (`manifest.go`, `progress.go`).** Immutable
share-by-reference structure + deep-copied mutable state is exactly right for a system
that snapshots for the UI on a timer while the download hot path mutates. The flat
parallel arrays with a prefix-sum `fileArticleOffsets` make `fileIndexForArticle` a
binary search instead of a cached back-pointer that could drift — and the comment at
`manifest.go:109-113` explicitly records that decision and why. `recompute()` as the
self-healing ground-truth rebuild after every bulk mutation is the right escape hatch.
Preserve all of this.

**Transient-state exclusion from persistence.** `emitted`, `pendingArticles`,
`articlesResolved`, `articlesFailed`, `earlyAborted` are all deliberately absent from
`jobProgressJSON` (`progress.go:389-407`) with a comment stating these are *correctness*
exclusions, not wire-compat ones. Persisting `emitted` would silently skip
re-downloading articles whose bytes never hit disk. This is subtle and correct.

**`selectWork`'s non-blocking pre-check (`dispatch.go:426-451`).** The comment names
the failure mode (a closed broadcast channel stays select-ready forever, so uniform
random selection occasionally picks a stale disconnect over freshly-arrived work), cites
issue #182, and is honest that the fix narrows rather than closes the window. Exactly
the standard the rest of the codebase should be held to.

**`managedConn`'s documented lock-over-I/O exception (`dispatch.go:746-798`).** The
`//lockio:` markers are backed by a 15-line rationale explaining that `mu` doubles as
the dial-coalescing lock and that this is `sync.Once`-shaped rather than the
anti-pattern the rule targets. Correct exception, correctly documented, correctly
suppressed.

**The assembler's single-worker + synchronous-ack control-message protocol.**
`CancelJob` / `CloseJobHandles` block on `ackCh` until the worker has actually closed
the FDs, and `dispatchRequest` closes the ack even on the shutdown-drain path so the
caller can never hang. This is what makes NFS silly-rename safe and it is the pattern
go-standards §5 codifies. Similarly the `completed` tombstone set rejecting late
duplicates. Do not make these asynchronous.

**`nntp`'s orphaned-command handling.** Cancelling a `Fetch` sets `orphaned` and returns,
but the reader still reads and discards the response and releases the semaphore slot, so
the connection stays in protocol sync and is reusable. The `finishReader` ordering
(`closeErr` before the `closed` flag) is the fix for a real past bug and is called out
in go-standards. Both should stay.

**The bounded-input discipline against hostile servers** — `maxResponseLineLen`,
`maxBodySize`, `maxDecodeSize`, `maxPartOffset`, and the assembler's `offsetInRange`
with its slack divisor — is layered (parse-time *and* write-time) and each layer's doc
comment names the attack. This is the legitimate kind of untrusted-input defense for
this deployment and should not be trimmed as "over-engineering for a single-user app".

---

## Open questions for synthesis

1. **Does `SQLiteStore.Add` write `<stateDir>/manifests/<id>.json.gz`?** Finding 12
   hinges on it. `Queue.Add` only writes the manifest when `store == nil`, yet
   `PromoteNext` and `hydrateSnapshot` read that path unconditionally. Whoever covers
   `internal/queue/sqlite_store.go` in depth should settle this.

2. **Does `internal/app/pipeline.go` guard against an evicted (nil-manifest) job before
   calling `CheckEarlyAbort` (line 256) and `CountUnfinishedArticles` (line 401)?**
   Finding 2's severity depends on it. From the downloader side there is no guard.

3. **Who calls `Assembler.Stop` relative to `Downloader.Stop`, and does the documented
   shutdown order (stop downloader → stop assembler → cancel ctx → wait → flush queue)
   actually hold in `internal/app/app.go`?** Findings 3 and 21 are only benign if the
   assembler drains after the downloader has stopped producing.

4. **Is the queue checkpoint (`Save`) ever invoked while the assembler has un-flushed
   cached bytes?** If yes, finding 3 is a live data-integrity issue rather than a
   crash-only one. The checkpoint ticker lives in `internal/app`.

5. **`WithMarkArticlesDoneHook`** (`app.go:353-356`) is the only consumer of the
   message-ID ack path. If finding 9's consolidation is accepted, that hook needs a
   replacement design — an app-lane call.

6. **Is `queue.CheckEarlyAbort`'s "returns true exactly once per job" contract relied on
   by app-side abort logic?** `isEarlyAbort` mutates `earlyAborted` under the queue write
   lock, but `earlyAborted` is not persisted — so a restart mid-job re-arms the
   heuristic. Whether that matters is an app-lane question.

---

## git status proof

```
$ git status --short
```
(no output — working tree clean)
