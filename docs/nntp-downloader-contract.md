# NNTP Downloader & Connection Pool Contract

This document is the contract for `internal/downloader` and `internal/nntp`: what
state NNTP connections and worker pools are guaranteed to maintain, how article
requests are scheduled and retried, which operations are fallible, and how
failures degrade across server pools.

`docs/ARCHITECTURE.md` describes the downloader's high-level topology. This
describes its structural obligations and invariants.

**This states the contract in the present tense, including parts not yet built.**
That is deliberate — it is the target the code is held to, not a report on the
code as it stands. The Status section below records exactly what has landed.
Where the two disagree, the code is wrong and the gap is a bug, not a
documentation error.

## Why this exists

The NNTP downloader orchestrates parallel fetching across multiple server groups
(primary, optional/backup, priority tiers). Multi-connection pooling and fallback
mechanics introduced subtle concurrency bugs and performance degradation:

- **Disconnect-on-idle race condition (#182)**: Go's uniform-random `select`
  case selection allowed idle worker goroutines to pick a closed `disconnectCh`
  over a newly arrived article request on `workCh`, improperly destroying
  connections that were about to perform work.
- **Lock-holding during I/O**: Mutexes held across network reads or socket
  operations blocked unrelated workers and status reporting.
- **Redundant bandwidth spending**: Without the sequential in-flight invariant,
  parallel workers could fetch the same article concurrently on multiple paid
  Usenet providers.
- **Ambiguous failure semantics**: Network drops, temporary timeouts, NNTP
  430/423 (Article Not Found) errors, and CRC mismatches were misclassified,
  causing either premature job failures or infinite retry loops.

## The four tiers

The downloader pipeline operates across four isolated tiers of ownership:

| Tier | Owned By | Responsibility | Lock / Synchronization |
|---|---|---|---|
| **Dispatcher** | Main loop (`run`) | Queue scanning, server selection, work fan-out via `workCh` sends | Queue `RLock`; `optsMu` RLock for pass options. Never blocks on I/O. |
| **Tracker** | `dispatchTracker` | In-flight deduplication, per-article try-bitmaps (`serverMask`) | Internal `tracker.Mutex`. Lock-held periods bounded to O(1) bitmap ops. |
| **Server state** | `Server` | Per-server penalty tracking, bad/good connection counters, optional-server auto-deactivation | `Server.mu` (RWMutex) for penalty/deactivation; atomic counters for bad/good. |
| **Worker / Connection** | `connWorker` & `managedConn` | Pipelined NNTP network I/O (`nntp.Conn`), yEnc decoding, rate shaping, TLS | `managedConn.mu` for dial-coalescing only; `nntp.Conn` internal locks for pipelining. |

## State machines

### `nntp.Conn` lifecycle (state.go)

Each `nntp.Conn` follows a strict linear state machine. Transitions are enforced
by `canTransition`; illegal moves return `ErrInvalidState`.

```
Disconnected ──► Connected ──► Authenticated ──► Ready ──► Closed
                    │                                │
                    ├──► Ready (no-auth servers)      │
                    │                                │
                    └──► Closed                      └──► Connected (480 re-auth)
```

`Closed` is terminal. Once closed, discard the `*Conn` and `Dial` a fresh one.

### `managedConn` lifecycle (dispatch.go)

`managedConn` is a nullable pointer wrapper, not a state machine. It guards lazy
connection setup so concurrent `handleRequest` goroutines on the same worker
slot coalesce their dial attempts:

- **`conn == nil`**: No connection. `Get()` dials under `mu`, stores the result,
  and returns it. All concurrent `Get()` callers for the same slot block on `mu`
  until the dial resolves.
- **`conn != nil`**: Connection live. `Get()` returns it immediately.
- **`Close()` / `DropIfMatches()`**: Sets `conn = nil` under `mu`. The next
  `Get()` re-dials lazily.

`managedConn.mu` is held across the `nntp.Dial` call itself — an intentional
exception to the "never hold a mutex during I/O" rule (see the `managedConn` doc
comment in dispatch.go). This is closer to `sync.Once`'s pattern than to the
anti-pattern the rule targets: releasing mid-dial would let two goroutines dial
concurrently or let `Close()` orphan a connection.

### Server penalty lifecycle (server.go)

Penalty state lives on `Server`, not on connections or the tracker:

```
Active ──► Penalized (penaltyExpiry set) ──► Active (expiry passed, ClearDeactivation)
  │                                              ▲
  └──► Deactivated (optional server, bad ratio)──┘
```

Required servers (`cfg.Required == true`) are never auto-deactivated regardless
of their failure ratio.

## Mandatory invariants

1. **Strict connection cap**: For any server S, the total worker goroutines
   (and thus maximum TCP connections) equals `max(S.Connections(), 1)`. Workers
   are created once in `Start` and not resized.

2. **Sequential in-flight invariant**: For any article `MessageID`, exactly one
   request can be active across all server pools at any moment
   (`InFlight(msgID) ≤ 1`). `tryDispatch` checks `InFlightLocked(key) > 0`
   before sending. Fallback to secondary/backup servers is strictly sequential —
   only after the current request resolves and `clearInFlight` runs.

3. **Non-blocking dispatch loop**: The main loop (`run`) must never perform
   blocking socket I/O, wait on unbuffered channels, or take write locks across
   queue scans. `buildDispatchPlan` runs under queue `RLock`; all side effects
   (status transitions, result emissions, hopeless-job callbacks) are applied
   via `applyDispatchPlan` after the lock is released.

4. **Idempotent disconnect-on-idle**: `DisconnectAll()` signals idle connections
   to close by closing the `disconnectPtr` channel. Workers that are mid-fetch
   finish their current article before closing. `ensureDisconnectChan()`
   replaces the closed channel with a fresh open one when the next connection
   dials.

   A closed channel stays permanently ready, so a worker must select on it
   **only while it has, or may imminently acquire, a connection** — that is
   what `disconnectChanFor(mc, inFlight)` enforces. It returns `nil` (which
   blocks forever in `select`) only when **both** halves of the guard say the
   worker is fully idle: `!mc.isOpen() && !inFlight`.

   Both halves are load-bearing. `connWorker` passes `inFlight` as
   `len(sem) > 1` — `sem` holds the loop's own slot plus one per running
   `handleRequest` goroutine, and those goroutines dial through `mc.Get`. A
   worker whose `managedConn` is merely closed *at check time* therefore
   keeps the real channel while a request is still in flight: dropping to
   `nil` there would leave it holding a connection opened behind its back,
   unwakeable by a later `DisconnectAll()` and leaked until the next unit of
   work arrived.

   `mc.isOpen()` is a lock-free atomic read of `managedConn.open`, never a
   `mu` acquisition — `mu` is held across `nntp.Dial`, and the parent loop
   shares the `managedConn` with the goroutine that is dialling, so taking it
   would stall the dispatch select for the whole dial. A stale read is safe
   both ways: stale-open selects on the real channel and loops once more,
   stale-closed is covered by `inFlight`.

   Snapshotting alone is not sufficient protection: `ensureDisconnectChan()`
   runs only on dial, and an idle daemon never dials, so a worker that kept
   selecting on the stale closed channel would take the `workDisconnect`
   branch on every iteration and busy-loop forever. That was a real bug —
   after the first idle-disconnect following a completed download, every
   `connWorker` spun at full CPU, silently (the branch logs only at `Debug`,
   and `downloader` is commonly configured at `info`).

5. **Emitted-is-transient durability contract**: `MarkArticleEmitted` is not
   persisted to disk. If the process crashes before the assembler marks the
   article `Done`, the `Emitted` flag is lost on restart and the article is
   re-dispatched.

   What `Done` means is narrower than "bytes on stable storage", and the
   boundary moved in #355. The assembler acks an article once its bytes have
   reached `WriteAt` — never while they are only in its write cache, which is
   what makes the re-dispatch above reliable. It does not wait for an `fsync`:
   the assembler syncs once per file completion, not once per article, so an
   ack published by the periodic flush describes bytes that are in the page
   cache and may not be on the platter. Only a file's final acks, flushed by
   `finalizeFile` after its `Sync`, are fsync-backed.

   The two crash classes therefore differ, and only one is the download path's
   to repair:

   | Lost | Acked? | Recovery |
   |------|--------|----------|
   | bytes that never reached `WriteAt` — still buffered, or never received | no | `Emitted` is lost, the next run re-dispatches the article. This contract. |
   | bytes that reached `WriteAt` but not the platter | yes | not re-dispatched, and nothing in the download path repairs it. par2 covers it. |

   See `assembler-storage-contract.md` §3 for the assembler side of this.

## `nntp.Conn` pipelining contract

Each `nntp.Conn` supports pipelined NNTP commands, bounded by a semaphore
(`Conn.sem`) of capacity `max(1, ServerConfig.PipeliningRequests)`.

- **Command submission**: `Fetch`/`Stat` acquire a semaphore slot, append a
  `pendingCmd` to a FIFO queue, and write the NNTP command to the wire under
  `sendLock`. The FIFO order matches the wire order exactly.
- **Response consumption**: A single `runReader` goroutine pops the FIFO head,
  reads the response (including dot-stuffed body for BODY commands), fills the
  result, and closes the `pendingCmd.done` channel. It then releases the
  semaphore slot.
- **Context cancellation**: If a caller's `ctx` is cancelled while waiting for
  `pc.done`, the `pendingCmd` is marked `orphaned`. The reader goroutine still
  reads and discards the response to keep the connection in protocol sync. The
  semaphore slot is released either way.
- **Connection death**: `finishReader` sets `closed=true`, closes the socket,
  cancels the Conn's internal context, and wakes all pending callers with the
  fatal error via `wakeOrphans`.

### Per-connection goroutine limit

Each `connWorker` bounds outstanding `handleRequest` goroutines via a local
semaphore of capacity `pipelineDepth × 2`. Up to `pipelineDepth` requests can
be on the wire (bounded by `nntp.Conn.sem`), while another `pipelineDepth` can
be decoding yEnc in parallel. This prevents a fast `connWorker` from eagerly
draining the entire `workCh` and spawning unbounded goroutines.

## Try-list & failure escalation matrix

Article retries are governed by a per-article bitmask (`serverMask`). The outcome
of an article fetch determines try-list retention, penalty escalation, and
downstream action:

| Error Category / Response | Try-List Retention | Server Penalty | Downloader Action |
|---|---|---|---|
| **Success (NNTP 222)** | Cleared (`clearTried`) | None | `MarkArticleEmitted`, emits `ArticleResult` with data. |
| **NNTP 430 / 423 (Not Found)** | Retained (`mask.set(idx)`) | None — server healthy, `RecordGoodConnection` | Logs debug, falls through to next eligible server. |
| **CRC Mismatch** | Retained (`mask.set(idx)`) | None — `RecordGoodConnection` | Increments `ErrClassCRCMismatch`, tries alternate server. |
| **Connection / Socket Error** | Unmarked (`unmarkTried`) | Yes — `PenaltyFor(err)` | Drops socket via `DropIfMatches`, returns article to pool. Only the first goroutine to observe the failure records `RecordBadConnection` and applies penalty. |
| **Penalized Server Dial** | Unmarked (`unmarkTried`) | Existing penalty retained | Silently returns article to dispatch pool. No result emitted, no telemetry counted. |
| **Context Cancelled (pause/shutdown)** | Unmarked (`unmarkTried`) | None — not a server fault | `ClearArticleEmitted`, article re-dispatched on resume. |
| **Decode Error (non-CRC)** | Cleared (`clearTried`) | None | `MarkArticleEmitted` (terminal), emits failed `ArticleResult`. Includes DMCA/takedown (`ErrArticleRemoved`). |
| **Max Tries Exceeded (`maxArtTries`)** | — | None | Emits `ErrNoServersLeft`. |
| **All Eligible Servers Exhausted** | — | None | Emits `ErrNoServersLeft` after queue lock release (via `applyDispatchPlan`). |

### DMCA / takedown handling

`ErrArticleRemoved` is detected by `isDMCA()` after both yEnc and UU decoding
fail — the article body is scanned for keywords (`dmca`, `removed`, `cancel`,
`blocked`). It flows through the generic non-CRC decode error path in
`processFetchedArticle`: `MarkArticleEmitted` + `clearTried` + emit. There is no
special short-circuit; it is terminal because the emitted `ArticleResult` carries
the error and the dispatcher will never re-pick an emitted article.

## Penalty classification

`PenaltyFor(err)` maps NNTP errors to server hold-out durations. The mapping is
part of this contract — changing a duration changes server failover behavior:

| Error | Penalty | Duration |
|---|---|---|
| `ErrAuthRejected` (481/482) | `PenaltyPerm` | 10 min |
| `ErrServerUnavailable` (502/503) | `Penalty502` | 5 min |
| `ErrClosed` (unexpected disconnect) | `PenaltyUnknown` | 3 min |
| `ErrAuthRequired` (480 mid-session) | `PenaltyShort` | 1 min |
| `ErrInvalidState` (programming error) | `PenaltyShort` | 1 min |
| `ErrTransient` (bare 4xx) | `PenaltyVeryShort` | 6 sec |
| `ErrNoArticle` (430/423) | — | 0 (no penalty) |
| Anything else | `PenaltyUnknown` | 3 min |

When `opts.NoPenalties` is set, `clampPenalty` caps all penalties to
`PenaltyShort` (1 min) regardless of the error class.

### Optional server auto-deactivation

`shouldDeactivateOptional` fires inside `ApplyPenalty` when a non-required
optional server's bad-connection ratio exceeds
`constants.OptionalDeactivationThreshold` (0.3). Once deactivated, the server
remains out of the pool until its penalty expires and `ClearDeactivation` is
called by `checkExpiredPenalties`.

`RecordGoodConnection` resets the bad-connection counter to zero atomically.
A single successful fetch after N failures forgives all bad history. This means
a server that intermittently succeeds will never trigger auto-deactivation.

## Memory & allocation budget

| Allocation Target | Scope | Bound / Strategy |
|---|---|---|
| **Article Request (`articleRequest`)** | Per-dispatch item | Allocated only after confirming `InFlight == 0`. ~68 bytes struct overhead + string backing (~100–200 bytes total with typical message-IDs). |
| **Completions Channel (`completions`)** | Downloader-wide | Bounded buffer (`opts.CompletionsBuffer`, default 256). Backpressures dispatcher when assembler is slow. |
| **Per-Server Work Queue (`workCh`)** | Per-server pool | Capacity = `2 × Connections × PipeliningRequests` (minimum 1). |
| **Per-Connection Goroutines** | Per-worker slot | Bounded to `pipelineDepth × 2` via local semaphore in `connWorker`. |
| **Decoding Buffers** | Worker payload | Allocated via `decoder.GetBuffer`, returned to `sync.Pool` on failure or after downstream consumption. |
| **Try-List (`serverMask`)** | Per-article | `uint64` bitmask for ≤64 servers (zero allocation); dynamic `[]uint64` slice only for >64 servers. |

## Failing versus degrading

- **Single server outage**: When a server drops connections or returns 5xx
  errors, `PenaltyFor` determines the hold-out duration (6s to 10min depending
  on error class). Optional servers may be auto-deactivated if their bad-ratio
  exceeds 0.3. Dispatch degrades gracefully to remaining active servers and
  backup groups.

- **All servers penalized**: The 5-second ticker in `run` ensures the dispatcher
  wakes periodically to discover expired penalties even when no workers are
  active (no `dispatchReady` signals arriving). `checkExpiredPenalties` calls
  `ClearDeactivation` on any server whose penalty has passed.

- **Global pause / Job pause**: Pausing cancels the `pauseCtx`, which is
  snapshotted by workers in `fetchArticle` as the context for `mc.Get()` and
  `c.Fetch()`. In-flight network reads abort immediately. Workers drain pending
  `workCh` items, call `unmarkTried` and `ClearArticleEmitted` so articles can
  be cleanly re-dispatched on resume. `DisconnectAll()` is called to free idle
  sockets.

- **Completions buffer full**: If the assembler cannot keep up, the
  `completions` channel fills. Workers block on `emitResult`'s channel send
  (watching `ctx.Done()` for shutdown). The dispatcher is not directly blocked,
  but workers stop calling `signalDispatch` until they unblock, so dispatch
  passes produce no new work. No data is dropped or corrupted.

## Compiler & architectural enforcement

1. **No direct socket access in dispatcher**: `dispatchPass` has no reference to
   `nntp.Conn` — it communicates exclusively via channel sends to `workCh`.

2. **Two-phase dispatch**: `buildDispatchPlan` runs read-only under queue `RLock`
   and populates a `dispatchPlan`. `applyDispatchPlan` runs after the lock is
   released and executes all side effects: exhausted-article emission, status
   transitions, hopeless-job callbacks, idle disconnect. This separation
   prevents deadlocks between queue locks and the completions channel.

3. **Double-select disconnect guard (`selectWork`)**: `selectWork` uses a
   non-blocking pre-check on `workCh` and `ctx.Done()` before falling into the
   3-way select that includes `disconnectCh`. This narrows the window where Go's
   uniform-random case selection could pick a stale closed `disconnectCh` over
   ready work (#182).

## Status

Landed:
- `managedConn` dial-coalescing with documented `lockio` exception.
- `selectWork` pre-check for disconnect-on-idle race protection (#182).
- Two-phase dispatch separation (`buildDispatchPlan` / `applyDispatchPlan`).
- Stack-local `dispatchOpts` snapshotting (eliminated per-article `optsMu`
  contention).
- Full penalty classification matrix with `NoPenalties` clamp.
- Optional server auto-deactivation via `shouldDeactivateOptional`.
- Per-connection goroutine bounding via local semaphore.
- Emitted-is-transient durability contract matching assembler handoff.
