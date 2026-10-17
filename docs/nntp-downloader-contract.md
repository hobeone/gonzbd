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
| **Dispatcher** | Main loop (`run`) | Queue scanning, server selection, work fan-out via `workCh` sends | `Dispatcher.List()` / `Dispatcher.Job()` under dispatcher mutex; `optsMu` RLock for pass options. Never blocks on I/O. |
| **Tracker** | `dispatchTracker` | In-flight deduplication, per-article try-bitmaps (`serverMask`), per-article, per-server connection-fault counts | Internal `tracker.Mutex`. Lock-held periods bounded to O(1) bitmap ops, except the reap's scans (`Instances`, `DropInstances`), which are O(entries). |
| **Server state** | `Server` | Per-server penalty tracking, bad/good connection counters, optional-server auto-deactivation | `Server.mu` (RWMutex) for penalty/deactivation; atomic counters for bad/good. |
| **Worker / Connection** | `connWorker` & `managedConn` | Pipelined NNTP network I/O (`nntp.Conn`), yEnc decoding, rate shaping, TLS | `managedConn.mu` for dial-coalescing only; `nntp.Conn` internal locks for pipelining. |

**The Tracker's identity is `(job instance, artIdx)`, not the Message-ID and not
the job ID.** Each of its three maps (try-lists, in-flight counts, connection-fault counts) is keyed on that pair, built by `keyFor` in
`tracker.go`. The job is part of the key rather than decoration: two resident
jobs can legitimately hold the same Message-ID — the same NZB added twice, or a
reposted par2 volume — and keyed on the Message-ID alone they shared one
try-list entry and one in-flight count, so one job's article could reach
`ErrNoServersLeft` without ever having been fetched for it. The instance rather
than the ID because a retry registers a new `*job.Job` under the ID while a
fetch for the removed instance can still be running: keyed on the ID, that
fetch's completion decremented the retry's in-flight count and unmarked or
cleared the retry's try-list (#665). Every completion updates the tracker
through `clearInFlight`, `unmarkTried` or `clearTried`, which build the key from
the request's own instance. `artIdx` rather than the Message-ID because the
manifest already assigns it, it is unique within a job by construction, and it
cannot be absent; nothing in this tier needs the Message-ID's content, only an
identity.

The key holds the instance through a `weak.Pointer`, so an entry an instance
leaves behind does not keep its job and manifest in memory. The key also
carries the job ID, which `CancelJob` uses to reap the tracker: it drops the
entries under the cancelled ID, and those of every instance that is not the
one `Dispatcher.Job` returns for its own ID. The second clause is what removes
an instance's leftovers when it leaves the dispatcher without an abort, as a
job settled at `Fetching` and then removed does. `CancelJob` is called from
sched's `Abort`, which fires only for an instance holding a lease itself, and
a removed instance has been parked, so the instance `Abort` names is the one
registered under its ID (`TestCancel_AnInstanceHoldingNoLeaseIsNotAborted`).
`DropInstances` is the tracker's one deleter of whole instances' entries, and
the reap is its one caller: `git grep -n 'DropInstances(' -- '*.go' ':!*_test.go'`
finds 3 lines: the declaration, that call, and the line in `tracker.go` quoting
the pattern back at itself.

## State machines

### `nntp.Conn` lifecycle (state.go)

Each `nntp.Conn` follows a strict linear state machine. Transitions are enforced
by `canTransition`; illegal moves return `ErrInvalidState`.

```
Disconnected ──► Connected ──► Authenticated ──► Ready ──► Closed
     │              │                                │
     │              ├──► Ready (no-auth servers)      │
     │              │                                │
     └──► Closed    └──► Closed                      └──► Connected (480 re-auth)
```

`Closed` is terminal. Once closed, discard the `*Conn` and `Dial` a fresh one.

### `managedConn` lifecycle (dispatch.go)

`managedConn` is a nullable pointer wrapper, not a state machine. It guards lazy
connection setup so concurrent `handleRequest` goroutines on the same worker
slot coalesce their dial attempts:

- **`conn == nil`**: No connection. `Get()` dials under `mu`, stores the result,
  and returns it. All concurrent `Get()` callers for the same slot block on `mu`
  until the dial resolves.
- **`conn != nil`**: Connection live. If `conn.Closed()` reports true (e.g. idle
  teardown by remote server or earlier disconnect), `Get()` discards `conn`,
  clears worker connectivity tracking, and redials lazily under `mu`. Otherwise,
  `Get()` returns `conn` immediately.
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
   (and thus maximum TCP connections) equals `S.Connections()`, which
   `config.ServerConfig.Checked` requires to be at least 1. Workers
   are created once in `Start` and not resized.

2. **Sequential in-flight invariant**: For any article identity
   `(job instance, artIdx)`, exactly one request can be active across all
   server pools at any moment (`InFlight(instance, artIdx) ≤ 1`). `tryDispatch`
   checks `InFlightLocked(key) > 0` before sending. Fallback to
   secondary/backup servers is strictly sequential — only after the current
   request resolves and `clearInFlight` runs.

   The identity is the pair, not the `MessageID`. Two resident jobs holding the
   same Message-ID have independent in-flight budgets and may be fetched
   concurrently, which is correct: they are different articles that happen to
   share a name.

   A retry and the removed instance it replaced also have independent budgets.
   A fetch for the removed instance that is already running when the retry
   dispatches the same article runs alongside the retry's; invariant 7 drops
   its result. `fetchArticle` drops a queued request for a removed instance
   before any network I/O, so the overlap is bounded by the requests that had
   already passed that check.

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

5. **Emitted-is-transient durability contract**: `Job.MarkArticleEmitted` is not
   persisted to disk. If the process crashes, the `Emitted` flag is lost on
   restart and the article is re-dispatched unless the restart can prove its
   bytes are on disk.

   This is no longer a standalone rule. It is a consequence of S3 — *absence of
   evidence is absence* — in `docs/durability-contract.md`: an article a restart
   cannot prove is on disk is Outstanding, and `Emitted` is by construction not
   evidence about disk, so it is not persisted at all.

   In a running process an article is `Done` once its `pwrite` returned, and the
   recorder buffers a `written_articles` row for it, flushed every 5 s. Neither
   is trusted across a restart: the first hydration reads every row of an
   incomplete file back from the device and keeps only those whose bytes match
   their CRC.

   What a crash costs, by what the restart can prove:

   | State at the crash | What the restart does |
   |---|---|
   | still in memory ahead of the write, or never received | no row. Outstanding, re-dispatched. This contract. |
   | written, but its row not yet flushed | no row. Outstanding, re-dispatched, and its bytes are written again. |
   | row flushed, and the bytes read back with its CRC | Done; not re-dispatched. |
   | row flushed, but the bytes are gone, short or different (lost from the page cache, truncated, overwritten, the file deleted) | the row is deleted; Outstanding, re-dispatched. |
   | the file was recorded `complete=1` | not read: `complete=1` is written only after the file's fsync. |
   | a storage layer that acknowledged an fsync it did not honour (a lying disk, an NFS server that acks early), for a `complete=1` file | not re-dispatched, and nothing in the download path repairs it. par2 covers it. |

   The last row is the only case where a restart's `Done` can be wrong, and it
   is outside what this program can observe.

   See `docs/durability-contract.md` for the assembler and recorder side of
   this.

6. **A job handed to post-processing is not dispatched**: `buildDispatchPlan`
   and `hasDownloadableJobs` skip a job `Options.HandedOff` reports, whatever
   its row says. The row alone is not enough: a job the app hands over from
   `Fetching` keeps `IntentRun` at `Fetching` until the finalizer cancels it,
   and its file handles are already closed. The app wires the option to its
   post-processing admission record; `docs/post-processing-contract.md` has
   the rest.

   `buildDispatchPlan` consults `HandedOff` once per job, before the article
   loop, so a hand-off landing mid-loop still queues the rest of the job's
   articles into `workCh` — `enqueuePostProc`'s admission is not serialised
   with `ForEachUnfinishedArticle`. `fetchArticle`'s per-job check (see
   invariant 7) consults `HandedOff` again, on the request's own instance,
   before any network I/O — the same gate that drops a paused, cancelled, or
   superseded-instance request — so a queued article for an admitted job is
   dropped there even when the loop above already queued it.

7. **A result belongs to the instance it was dispatched for**: a retry
   registers a new `*job.Job` under the same ID, and a fetch can outlive its
   instance's failure, finalization and that retry. So the ID alone does not
   say which instance a result is for. `UnfinishedArticle.Job`, the private
   `articleRequest.job` and `ArticleResult.Job` carry the instance from the
   dispatch pass to the pipeline, and `ArticleResult.JobID()` is derived from
   it.

   The downloader's own job mutations for a request — `markEmitted` after a
   fetch or an exhausted try-list, the `Job.ClearArticleEmitted` calls when a
   request is drained or its result dropped, and the per-job intent and
   hand-off reads in `fetchArticle` — act on that instance, never on a by-ID
   lookup. So do its tracker updates for a request, whose key is built from
   the request's instance (see "The Tracker's identity" above). Before any
   network I/O, `fetchArticle` also drops a request whose
   instance is not the one `Dispatcher.Job` returns for its ID, since the
   pipeline would discard its result.

   The app pipeline's `handleResult` drops a result whose `Job` is not the
   instance `Dispatcher.Job` returns for its ID, before any failure marking,
   early-abort accounting, stats, file registration or assembler write, and
   the handlers after it act on `res.Job`.

## `nntp.Conn` pipelining contract

Each `nntp.Conn` supports pipelined NNTP commands, bounded by a semaphore
(`Conn.sem`) of capacity `ServerConfig.PipeliningRequests`, which
`config.ServerConfig.Checked` requires to be at least 1.

- **Command submission**: `Fetch` (and the test/e2e-only `Stat` peer method)
  acquires a semaphore slot, appends a `pendingCmd` to a FIFO queue, and writes
  the NNTP command to the wire under `sendLock`. The FIFO order matches the
  wire order exactly.
- **Response consumption**: A single `runReader` goroutine pops the FIFO head,
  reads the response (including dot-stuffed body for BODY commands), fills the
  result, and closes the `pendingCmd.done` channel. It then releases the
  semaphore slot. When `ServerConfig.Timeout > 0` (`itr == nil` otherwise, in
  which case no per-read deadline is armed), `idleTimeoutReader` arms a
  per-read socket deadline (`ServerConfig.Timeout`) on every `Read` during the
  handshake (`gateOnInFlight == false`) and, after `endHandshake`, while at
  least one command is in flight (`itr.beginCmd` through `itr.endCmd`). When
  the last in-flight command completes, the read deadline is disarmed
  (`SetReadDeadline(time.Time{})`) so an idle connection parked in `runReader`
  between commands does not time out on silence. Consequently, a peer that
  vanishes silently (without sending FIN or RST) while no command is in flight
  is not detected by `idleTimeoutReader` until either OS TCP keepalive fires or
  the next command is submitted and its read deadline expires (whereas a clean
  server close that sends FIN, EOF, or an unsolicited 400/205 timeout is observed
  immediately by the parked `runReader`, which flags the connection as closed
  under `ErrIdleClosed`).
- **Response identity**: FIFO position alone does not establish that a response
  answers the command it was popped against. `pendingCmd.msgID` carries the
  Message-ID the command asked for — `newArticleCmd` emits it and the wire bytes
  from one argument, so the two cannot disagree — and `runReader` compares it
  against the one echoed on the **success** line of every kind that echoes one
  (`BODY` 222, `ARTICLE` 220, `HEAD` 221, `STAT` 223). The comparison is
  octet-exact, because RFC 3977 §3.6 makes Message-IDs case-sensitive, and the
  ID is located by its angle brackets rather than by field position.

  `cmdKind.successResponse` is the sole owner of that code table:
  `runReader` publishes its verdict as `cmdResult.success`, and `Fetch` (and
  `Stat`) branch on that rather than re-comparing 222/223 themselves.

  This covers only responses that carry an identity. An error response
  (`430`/`423`) names no article, so it still pairs by FIFO alone; a desync
  landing on a run of those goes unnoticed until the next success line.

  A disagreement, or a success line carrying no Message-ID at all, is proof the
  queue is desynced — which makes the *next* response mis-paired too — so it
  kills the connection through `finishReader` rather than failing one article.
  Failing one article would catch the first casualty and silently mis-attribute
  every article after it: each would be a well-formed, CRC-valid body written
  into the wrong file, and no later layer can detect that. Both cases wrap
  `nntp.ErrDesynced`, which `classifyConnError` counts under its own
  `nntp_desynced` label rather than folding into `other_connection_error` — a
  desync is a server bug or a MITM, not a flaky link, and it is the one
  connection failure here that warrants a human looking at it. `PenaltyFor`
  gives it `PenaltyUnknown`: the same figure the catch-all would, stated
  explicitly so it is a decision rather than a fall-through. Nothing yet
  justifies a harsher one — a single bad response should not retire a server —
  and the new label is where the evidence to revisit it will come from.
- **Context cancellation**: If a caller's `ctx` is cancelled while waiting for
  `pc.done`, the `pendingCmd` is marked `orphaned`. The reader goroutine still
  reads and discards the response to keep the connection in protocol sync. The
  semaphore slot is released either way.
- **Connection death**: `finishReader` sets `closed=true`, closes the socket,
  cancels the Conn's internal context, and wakes all pending callers with the
  fatal error via `wakeOrphans`. When `runReader` terminates before popping any
  command for the response and the pending FIFO is empty, the cause is classified
  as an idle disconnect (`ErrIdleClosed`), enabling callers to discard the socket
  without applying server penalties.

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
| **Success (NNTP 222)** | Cleared (`clearTried`) | None | `Job.MarkArticleEmitted`, emits `ArticleResult` with data. |
| **NNTP 430 / 423 (Not Found)** | Retained (`mask.set(idx)`) | None — server healthy, `RecordGoodConnection` | Emits retryable `ArticleResult` (`ErrNoArticle`), leaves un-emitted so next pass tries fallback server. Pipeline filters retryable error. |
| **CRC Mismatch** | Retained (`mask.set(idx)`) | None — `RecordGoodConnection` | Increments `ErrClassCRCMismatch`, emits retryable `ArticleResult` (`ErrCRCMismatch`), leaves un-emitted so dispatcher tries alternate server. Pipeline filters retryable error. |
| **Connection fault** (`isConnFault`: timeout, reset, EOF or broken pipe with a command outstanding) | Unmarked (`UnmarkTriedAfterFault`), except on the article's `maxConnFaultsPerArticle`-th (3) fault on that server, which leaves the server marked | Only for a run — `PenaltyTimeout` once `RecordConnFault` reports one | Drops socket via `DropIfMatches`, returns article to pool; the same server may retry it, up to that bound. Only the first goroutine to observe the failure records it. |
| **Other Connection-Level Error** (any other: an NNTP error response, a desync, a protocol error, `ErrClosed`) | Unmarked (`unmarkTried`) | Yes — `PenaltyFor(err)` | Drops socket via `DropIfMatches`, returns article to pool. Only the first goroutine to observe the failure records `RecordBadConnection` and applies penalty. |
| **Penalized Server Dial** | Unmarked (`unmarkTried`) | Existing penalty retained | Silently returns article to dispatch pool. No result emitted, no telemetry counted. |
| **Context Cancelled (pause/shutdown)** | Unmarked (`unmarkTried`) | None — not a server fault | `Job.ClearArticleEmitted`, article re-dispatched on resume. |
| **Decode Error (non-CRC)** | Cleared (`clearTried`) | None | `Job.MarkArticleEmitted` (terminal), emits failed `ArticleResult`. Includes DMCA/takedown (`ErrArticleRemoved`). |
| **Max Tries Exceeded (`maxArtTries`)** | — | None | Emits `ErrNoServersLeft`. |
| **All Eligible Servers Exhausted** | — | None | Emits `ErrNoServersLeft` after queue lock release (via `applyDispatchPlan`). |

### DMCA / takedown handling

`ErrArticleRemoved` is detected by `isDMCA()` after both yEnc and UU decoding
fail — the article body is scanned for keywords (`dmca`, `removed`, `cancel`,
`blocked`). It flows through the generic non-CRC decode error path in
`processFetchedArticle`: `Job.MarkArticleEmitted` + `clearTried` + emit. There is no
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
| `ErrDesynced` (response identity mismatch) | `PenaltyUnknown` | 3 min |
| `ErrAuthRequired` (480 mid-session) | `PenaltyShort` | 1 min |
| `ErrInvalidState` (programming error) | `PenaltyShort` | 1 min |
| `ErrTransient` (bare 4xx) | `PenaltyVeryShort` | 6 sec |
| `ErrNoArticle` (430/423) | — | 0 (no penalty) |
| `ErrIdleClosed` (idle disconnect/unsolicited timeout) | — | 0 (no penalty) |
| A timeout (dial or handshake) | `PenaltyUnknown` | 3 min |
| Anything else | `PenaltyUnknown` | 3 min |

`fetchArticle`'s fetch-failure path asks `PenaltyFor` only about errors that
are not connection faults. A connection fault — `isConnFault`: a timeout, a
reset, an EOF or a broken pipe while a command was outstanding — is a fault of
that one connection: `chargeFetchFailure` passes it to
`Server.RecordConnFault`. That counts it as a bad connection and applies no
penalty until more bad connections than the server has connections have been
recorded since its last successful fetch, the latest being a connection fault;
then it applies `PenaltyTimeout` (10 min). So one stalled article costs one
connection and a retry, and losing every connection once from a clean count (a
provider restart, a NAT flush) costs a redial each. A server that keeps losing
connections is penalized on the first fault after every connection has been
lost once, rather than being redialled without bound (#986). SABnzbd counts
timed-out and errored connections too, but takes a server out only if it is
optional, at a bad-connection ratio above 0.3; GoNZBD takes out any server, so
its work moves to the next one. A dial failure, timeouts included, still costs `PenaltyFor(err)` at
once.

The server-wide count cannot catch one article that faults every time on a
server whose other fetches succeed, because each success resets it. So the
tracker also counts each article's connection faults, per server:
`UnmarkTriedAfterFault` leaves the server marked in the article's try-list on
the article's `maxConnFaultsPerArticle`-th (3) fault there, and the article moves to the next
server, or to `ErrNoServersLeft` when none is left, without the server it left
being penalized. SABnzbd's `max_art_tries` (default 3) bounds the same case.
Every request on a connection that dies counts a fault, including pipelined
requests that were only queued behind the one being read.

`ErrDesynced` draws the same duration as the catch-all, and has its own row
anyway: the figure is chosen rather than inherited, so a later change to the
catch-all does not silently redecide it. It is deliberately not harsher —
a desync may be a one-off, and `PenaltyPerm` would retire a server on a single
bad response. `classifyConnError` counts it under `nntp_desynced`, which is
where the evidence to revisit this would come from.

When `opts.MaxPenalty` is set or `opts.NoPenalties` is true, `clampPenalty` caps
all penalties (to `opts.MaxPenalty`, or `PenaltyShort` (1 min) by default when
`NoPenalties` is true) regardless of the error class.

*(Note: `constants.PenaltyShare` and `constants.PenaltyTooMany` are defined in
`internal/constants` as protocol constants from the upstream spec, but are reserved
and not currently mapped by `PenaltyFor`. `constants.PenaltyTimeout` is not mapped
by `PenaltyFor` either; `chargeFetchFailure` applies it, as above.)*

### Optional server auto-deactivation

`shouldDeactivateOptional` fires inside `ApplyPenalty` when an optional
server's bad-connection ratio exceeds
`constants.OptionalDeactivationThreshold` (0.3). Once deactivated, the server
remains out of the pool until its penalty expires and `ClearDeactivation` is
called by `checkExpiredPenalties`. A required server is never optional:
`ServerConfig.validate` refuses both flags together.

`RecordGoodConnection` resets the bad-connection counter to zero atomically.
A single successful fetch after N failures forgives all bad history. So a server
whose failures are interleaved with successes keeps resetting the count, and
triggers neither auto-deactivation nor the connection-fault penalty above,
which counts on the same counter.
`RecordConnFault` resets the counter when it reports a run, before the caller's
`ApplyPenalty`, so that penalty does not also deactivate an optional server
unless another fault lands between the two; the penalty alone takes it out of
the pool for the same time, since `Active` reads a deactivated server and a
penalized one alike.

## Memory & allocation budget

| Allocation Target | Scope | Bound / Strategy |
|---|---|---|
| **Article Request (`articleRequest`)** | Per-dispatch item | Allocated only after confirming `InFlight == 0`. ~68 bytes struct overhead + string backing (~100–200 bytes total with typical message-IDs). |
| **Completions Channel (`completions`)** | Downloader-wide | Bounded buffer (`opts.CompletionsBuffer`, default 256). Backpressures dispatcher when assembler is slow. |
| **Per-Server Work Queue (`workCh`)** | Per-server pool | Capacity = `2 × Connections × PipeliningRequests`; both factors are at least 1 for a checked server. |
| **Per-Connection Goroutines** | Per-worker slot | Bounded to `pipelineDepth × 2` via local semaphore in `connWorker`. |
| **Decoding Buffers** | Worker payload | Allocated via `decoder.GetBuffer`, returned to `sync.Pool` on failure or after downstream consumption. |
| **Try-List (`serverMask`)** | Per-article | `uint64` bitmask for ≤64 servers (zero allocation); dynamic `[]uint64` slice only for >64 servers. |

## Failing versus degrading

- **Single server outage**: When a server returns 5xx errors or refuses
  dials, `PenaltyFor` determines the hold-out duration (6s to 10min depending
  on error class). When it keeps dropping or stalling connections mid-fetch,
  `RecordConnFault` applies `PenaltyTimeout` once its bad connections since
  the last success outnumber its connections. Optional servers may be auto-deactivated if their bad-ratio
  exceeds 0.3. Dispatch degrades gracefully to remaining active servers and
  backup groups.

- **All servers penalized**: The 5-second ticker in `run` ensures the dispatcher
  wakes periodically to discover expired penalties even when no workers are
  active (no `dispatchReady` signals arriving). `checkExpiredPenalties` calls
  `ClearDeactivation` on any server whose penalty has passed.

- **Global pause / Job pause**: Before initiating network I/O in `fetchArticle`,
  workers check both the per-job intent of the request's own instance
  (anything but `IntentRun` drops it, so a paused or cancelled job's requests
  are drained) and global pause (`d.paused.Load() || dispatcher.Paused()`). Pausing also cancels
  `pauseCtx`, aborting in-flight socket reads immediately. Buffered requests
  in `workCh` are drained without I/O, calling `unmarkTried` and
  `Job.ClearArticleEmitted` so articles re-dispatch cleanly on resume.
  `DisconnectAll()` is called to close idle sockets.

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

### Landed
- `managedConn` dial-coalescing with documented `lockio` exception.
- `selectWork` pre-check for disconnect-on-idle race protection (#182).
- Two-phase dispatch separation (`buildDispatchPlan` / `applyDispatchPlan`).
- Stack-local `dispatchOpts` snapshotting (eliminated per-article `optsMu`
  contention).
- Full penalty classification matrix with `NoPenalties` clamp.
- Optional server auto-deactivation via `shouldDeactivateOptional`.
- Per-connection goroutine bounding via local semaphore.
- Emitted-is-transient durability contract, now derived from the durability design's S3 rather than standing alone.
- The Tracker keyed on the job instance, so a removed instance's late
  completion leaves a retry's in-flight count and try-list alone (#665).
- `CancelJob` reaps the entries of every instance no longer registered under
  its ID, not only those under the cancelled ID (#679).
- A connection fault mid-fetch costs its connection, not its server, until a
  run of them outnumbers the server's connections (#986).

### Open Gaps
- None recorded.

