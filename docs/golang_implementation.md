# SABnzbd Go Reimplementation Plan

**Reference spec**: `sabnzbd_spec.md`  
**Go version**: 1.22+  
**Module path**: `github.com/hobeone/sabnzbd-go`

## Model Legend

Each step is annotated with the recommended Claude model:

| Tag | Model | Use For |
|-----|-------|---------|
| `[opus]` | Claude Opus 4.6 | Architecture decisions, interface design, concurrency patterns, complex state machines |
| `[sonnet]` | Claude Sonnet 4.6 | Feature implementation, API handlers, integration, tests with moderate complexity |
| `[haiku]` | Claude Haiku 4.5 | Mechanical/rote tasks: boilerplate, additional test cases, config field expansion, repetitive handlers following established patterns |

**Rule of thumb**: If the step establishes a pattern, use Opus. If it follows an established pattern, use Sonnet or Haiku. If it's purely additive (more of the same), use Haiku.

---

## Build and Test Commands

No Makefile. Use standard Go tooling directly:

```bash
go build ./cmd/sabnzbd               # Build
go test -race ./...                   # Test with race detector
go test -run TestFoo ./internal/nzb/  # Single test
go vet ./...                          # Vet
golangci-lint run ./...               # Lint
go test -bench=. ./internal/decoder/  # Benchmarks
go test -tags=integration ./test/...  # Integration tests
```

---

## Existing Libraries to Use

Prefer proven libraries over custom implementations:

| Need | Library | Notes |
|------|---------|-------|
| NZB parsing | [`github.com/GJRTimmer/nzb`](https://github.com/GJRTimmer/nzb) | Parses NZB XML, fixes escaped article IDs, JSON save/load. Evaluate; fork or wrap if it lacks fields we need (password, meta tags). Also consider [`github.com/chrisfarms/nzb`](https://github.com/chrisfarms/nzb) as simpler alternative. |
| yEnc decoding | [`github.com/GJRTimmer/yenc`](https://github.com/GJRTimmer/yenc) | Multi-core yEnc decoder with CRC32. Also [`github.com/chrisfarms/yenc`](https://github.com/chrisfarms/yenc) as lighter option. Benchmark both. |
| NNTP client | [`github.com/Tensai75/nntp`](https://pkg.go.dev/github.com/tensai75/nntp) | RFC 3977 compliant. Pair with [`github.com/Tensai75/nntpPool`](https://pkg.go.dev/github.com/Tensai75/nntpPool) for connection pooling. Will need to add pipelining support — evaluate if wrapping is sufficient or if a fork is needed. |
| Human-readable sizes | [`github.com/dustin/go-humanize`](https://github.com/dustin/go-humanize) | Parse "10M"→bytes, format bytes→"10 MB". Well maintained, widely used. |
| RSS/Atom parsing | [`github.com/mmcdole/gofeed`](https://github.com/mmcdole/gofeed) | Universal feed parser (RSS, Atom, JSON Feed). |
| SQLite | [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite) | Pure Go, no CGO. Cross-compiles cleanly. |
| Rate limiting | [`golang.org/x/time/rate`](https://pkg.go.dev/golang.org/x/time/rate) | Token bucket for bandwidth limiting. |
| Password hashing | [`golang.org/x/crypto/bcrypt`](https://pkg.go.dev/golang.org/x/crypto/bcrypt) | Web UI password hashing. |
| YAML config | [`gopkg.in/yaml.v3`](https://pkg.go.dev/gopkg.in/yaml.v3) | Config file parsing (see config decision below). |
| Media filename parsing | Evaluate [`github.com/middelink/go-parse-torrent-name`](https://github.com/middelink/go-parse-torrent-name) or similar. Fallback: regex-based parser for TV/Movie/Date patterns. |

---

## Design Policy: Compatibility Scope

Not all "compatibility with Python SABnzbd" carries equal weight. This
section makes the scope explicit so future steps don't inherit implicit
constraints that aren't actually required.

### Required compatibility

| Area | Why required |
|------|--------------|
| **Glitter web UI** ↔ HTTP API | The Glitter UI is served as-is from this codebase. Its requests must match the Python API's mode dispatch, parameter names, and response shapes. |
| **External script contract** | Post-processing scripts users already wrote depend on the env vars and argv described in spec §8.4. Break this and real workflows break. |

### Not required (free to modernize)

| Area | Rationale |
|------|-----------|
| **TLS certificate algorithm** | Self-signed certs are generated per-install and never shared. Using Ed25519 instead of Python's RSA-4096 costs nothing. |
| **API key format** | Auto-generated per-install. The plan originally specified 16-char hex for Python parity, but a longer modern token (e.g. 32-byte base64url) would be equally valid — the Python-compat framing was accidental. Leaving the format alone for now, but treat it as a free choice if we ever touch it. |
| **Config file format** | We already diverged: YAML instead of Python's INI. Fresh installs only — no migration path. |
| **History DB schema** | Fresh installs only. Schema is Go-native; not read-compatible with Python's `history1.db`. |
| **Persistence layout for queue state** | Python's pickle files are not read. The plan chose JSON+gzip per-NzbObject for its own reasons (§ Coordination Architecture). |
| **Internal code structure, goroutine model, concurrency primitives** | A full rewrite — Python threads do not map 1:1 to Go goroutines. |

### Rule of thumb

When a design decision is framed as "match Python because..." ask whether
the decision has any **cross-install effect**:

- **Yes** (data files, API shape, user scripts, user config semantics): compatibility matters; match Python.
- **No** (crypto primitives, internal types, concurrency, log format): free choice; pick the best option.

If in doubt, the default is "not required" — constraints should be
justified, not assumed.

---

## Configuration Format Decision: YAML

**Choice**: YAML over JSON.

**Rationale**:
- **Comments**: YAML supports comments — essential for user-edited config files. JSON does not. This is the decisive factor for a config file that users edit by hand.
- **Readability**: Server definitions, category lists, and nested RSS filter rules are much more readable in YAML's indentation-based format than in JSON's brace-heavy syntax.
- **Go support**: `gopkg.in/yaml.v3` is mature, handles struct tags, and supports marshaling with comments preserved via custom marshaler.
- **Precedent**: Docker Compose, Kubernetes, GitHub Actions, and most modern Go tools use YAML for configuration.

**JSON is still used for**: queue persistence, RSS state, BPS meter state, and API responses. These are machine-read/written and benefit from Go's built-in `encoding/json`.

**Sample config structure** (`sabnzbd.yaml`):
```yaml
# SABnzbd-Go Configuration
general:
  host: "127.0.0.1"
  port: 8080
  api_key: "a1b2c3d4e5f6g7h8"
  language: "en"

directories:
  download_dir: "~/Downloads/incomplete"
  complete_dir: "~/Downloads/complete"
  dirscan_dir: ""       # Empty = disabled
  script_dir: ""

servers:
  - name: "My Primary"
    host: news.example.com
    port: 563
    username: user
    password: pass
    connections: 8
    ssl: true
    ssl_verify: 2       # 0=none, 1=loose, 2=normal, 3=strict
    priority: 0          # 0 = highest
    required: true
    retention: 3000      # Days; 0 = unlimited

categories:
  - name: "tv"
    pp: 7                # Bitmask: repair+unpack+delete
    script: ""
    priority: 0
    dir: "TV"

# ... etc
```

---

## Project Structure

```
sabnzbd-go/
├── cmd/
│   └── sabnzbd/
│       └── main.go              # Entry point, flag parsing, startup orchestration
├── internal/
│   ├── config/                  # YAML config system
│   ├── constants/               # Priority levels, statuses, limits
│   ├── nzb/                     # NZB data model (wraps or extends library parser)
│   ├── queue/                   # Queue manager, persistence, duplicate detection
│   ├── nntp/                    # NNTP client extensions (pipelining, state machine)
│   ├── downloader/              # Downloader orchestrator, server management, article dispatch
│   ├── decoder/                 # Decoder orchestration (wraps yEnc library + CRC checks)
│   ├── cache/                   # Article cache (memory + disk spill)
│   ├── assembler/               # File assembler, disk space checks
│   ├── postproc/                # Post-processing pipeline orchestrator
│   ├── par2/                    # par2 tool invocation
│   ├── unpack/                  # unrar/7zip tool invocation, direct unpack
│   ├── sorting/                 # Media detection, rename templates
│   ├── deobfuscate/             # Filename deobfuscation
│   ├── api/                     # HTTP API handlers (/api?mode=...)
│   ├── web/                     # Static file serving, template rendering
│   ├── history/                 # SQLite history database
│   ├── rss/                     # RSS feed processor
│   ├── dirscanner/              # Watched folder scanner
│   ├── scheduler/               # Cron-like scheduler
│   ├── notifier/                # Notification dispatch (Apprise, email, etc.)
│   ├── bpsmeter/                # Bandwidth metering, speed limiting, quotas
│   ├── urlgrabber/              # Remote NZB fetcher
│   └── app/                     # Application lifecycle, startup, shutdown
├── web/                         # Static assets (Glitter UI)
│   ├── static/
│   └── templates/
├── test/                        # Integration test fixtures, mock NNTP server
├── go.mod
├── go.sum
├── .golangci.yml
└── README.md
```

---

## Phase 0: Project Scaffold and Shared Foundations

These produce no user-visible functionality but establish the patterns everything else builds on.

### Step 0.1 — Module Init and Tooling Config `[haiku]`

Create `go.mod`, `.golangci.yml`, and `cmd/sabnzbd/main.go` stub.

```
Deliverables:
  - go.mod (github.com/hobeone/sabnzbd-go, go 1.22)
  - .golangci.yml with standard rules
  - cmd/sabnzbd/main.go that prints version and exits
  - README.md with build/test commands (no Makefile)
  - `go build ./cmd/sabnzbd && go vet ./...` passes
```

### Step 0.2 — Constants Package `[haiku]`

Translate all constants from spec §4.2, §4.3, §3.6, §7.5 into `internal/constants/`.

```
Deliverables:
  - internal/constants/priority.go — Priority iota constants
  - internal/constants/status.go — Job status string constants
  - internal/constants/penalty.go — Penalty duration constants
  - internal/constants/limits.go — Buffer sizes, cache limits, queue limits
  - internal/constants/constants_test.go — Verify values match spec
```

### Step 0.3 — Configuration System `[opus]`

YAML-based config. Every other package depends on this.

```
Deliverables:
  - internal/config/config.go — Top-level Config struct with YAML tags
  - internal/config/general.go — GeneralConfig (host, port, api_key, etc.)
  - internal/config/servers.go — []ServerConfig
  - internal/config/categories.go — []CategoryConfig
  - internal/config/downloads.go — DownloadConfig (bandwidth, cache size, retry limits)
  - internal/config/postproc.go — PostProcConfig (par2/unrar/7zip paths, flags)
  - internal/config/sorters.go — []SorterConfig
  - internal/config/schedules.go — []ScheduleConfig
  - internal/config/rss.go — []RSSFeedConfig with nested []FilterConfig
  - internal/config/loader.go — Load(path) / Save(path) with atomic write
  - internal/config/defaults.go — Default values for all fields
  - internal/config/config_test.go — Round-trip: marshal → unmarshal → compare
  - test/fixtures/sabnzbd.yaml — Sample config covering all sections

Design decisions (Opus should make these):
  - Thread-safe read/write (sync.RWMutex on the top-level Config)
  - Atomic save (write temp file, rename)
  - Validation on load (required fields, range checks, path existence)
  - Change notification: channel or callback — decide based on subscriber count
  - Use gopkg.in/yaml.v3 for marshal/unmarshal
```

---

## Coordination Architecture: In-Memory + Event-Triggered Persistence

### The Decision: In-Memory Queue with Channel-Based Signaling

After analyzing the Python codebase's actual persistence model (not just the spec), **SQLite-backed workflow was rejected** in favor of an in-memory queue with event-triggered file persistence and channel-based coordination. Here is the analysis.

### What Python Actually Does (and why it works)

The Python SABnzbd persistence model is more nuanced than "periodic pickle snapshots":

1. **Queue index** (`queue10.sab`): Stores only NZO IDs — a tiny file listing active jobs.
2. **Per-NzbObject save**: Each job is pickled individually to its own admin folder via `save_to_disk()`.
3. **Event-triggered saves, not time-based**: Saves fire on **file completion**, throttled by an adaptive interval:
   `save_timeout = max(120s, min(6.0 * job_bytes / 1GB, 300s))` — large jobs save every 2-5 min.
4. **Article state is never persisted individually** — it's embedded in the NzbObject pickle. On crash, in-flight articles get re-downloaded (a tiny fraction of total work).
5. **`register_article()` is explicitly unlocked** for performance — it only modifies individual NZOs.

### Why Not SQLite for the Queue

| Concern | SQLite Queue | In-Memory Queue |
|---------|-------------|-----------------|
| **Per-article writes** | Catastrophic: 5000 articles/sec × DB write = bottleneck. Even with WAL mode + batching, contention with the single-writer model kills throughput. | Zero cost: pointer update in memory. |
| **Article dispatch latency** | DB query per dispatch cycle (index scan over articles WHERE status=queued AND server NOT IN tried). | Direct slice iteration — the Python code already does this unlocked. |
| **Crash recovery granularity** | Per-article: perfect recovery, zero re-download. | Per-file: lose in-flight articles at crash time. For a 50GB job with 500K articles, losing 100 in-flight articles means re-downloading ~10MB — negligible. |
| **Complexity** | Schema design, migrations, query optimization, connection pooling for a hot path. | Simple Go structs + `sync.RWMutex`. |
| **Query flexibility** | Excellent for ad-hoc queries. But the queue is always iterated fully (priority order) — there are no ad-hoc queries in the hot path. | API queries (search, filter) are infrequent and can scan the in-memory list. |

**Verdict**: SQLite's per-write cost is unacceptable for the article dispatch hot path. The crash recovery improvement (per-article vs per-file) is not worth the throughput regression. Python's approach already proved this design works for 15+ years.

SQLite remains the right choice for **history** (write-once, query-heavy, needs search/pagination) — just not for the active download queue.

### Channel-Based Signaling (Not sync.Cond)

The Python code uses `DOWNLOADER_CV` (a `threading.Condition` on `NZBQUEUE_LOCK`) to wake the downloader. The original plan proposed `sync.Cond` as the Go equivalent. **This is replaced with channels**, for three reasons:

1. **`select{}` composability**: Channels work with `select{}`, so the downloader can wait on `{queue notification, context cancellation, speed limit change, shutdown signal}` in one statement. `sync.Cond.Wait()` cannot be combined with anything.
2. **No lock-hold requirement**: `sync.Cond.Wait()` requires holding the mutex, creating a lock ordering constraint that propagates through the codebase. Channels decouple sender and receiver.
3. **Idiomatic Go**: The Go team has informally discouraged `sync.Cond` for new code; channels cover all its use cases with less footgun potential.

### Coordination Pattern

```go
// Queue notifies downloader via non-blocking channel send
type NzbQueue struct {
    mu      sync.RWMutex
    jobs    []*NzbObject        // sorted by priority
    table   map[string]*NzbObject
    notify  chan struct{}        // cap=1, non-blocking signal
    history HistoryQuerier       // interface for duplicate checks
}

func (q *NzbQueue) Add(nzo *NzbObject) {
    q.mu.Lock()
    q.jobs = insertSorted(q.jobs, nzo)
    q.table[nzo.ID] = nzo
    q.mu.Unlock()
    // Wake the downloader — non-blocking, coalescing
    select {
    case q.notify <- struct{}{}:
    default: // already pending notification, skip
    }
}

// Downloader consumes notifications
func (d *Downloader) run(ctx context.Context) {
    for {
        select {
        case <-d.queue.notify:
            d.dispatchArticles()
        case result := <-d.completions:
            d.handleCompletion(result)
        case <-d.speedChanged:
            d.updateRateLimit()
        case <-ctx.Done():
            d.drainAndShutdown()
            return
        }
    }
}
```

The `notify` channel has capacity 1 and uses non-blocking send — this **coalesces** rapid queue mutations (e.g., adding 100 NZBs via RSS) into a single wake-up, which is more efficient than `sync.Cond.Broadcast()` which wakes all waiters per call.

### Persistence Strategy

```
Trigger                    What gets saved               Where
────────────────────────── ──────────────────────────── ─────────────────────
Job added/removed          Queue index (list of IDs)     {admin}/queue.json.gz
File completed (throttled) NzbObject JSON                {job_admin}/{nzo_id}.json
Job paused/resumed         NzbObject JSON                {job_admin}/{nzo_id}.json
Job completed              Move to history DB            history.db (SQLite)
Shutdown                   All of the above              All locations
```

**Save throttle** (matching Python's adaptive interval):
```go
func (nzo *NzbObject) saveCooldown() time.Duration {
    // Scale save interval with job size: 120s-300s
    secs := 6.0 * float64(nzo.BytesTotal) / (1024*1024*1024)
    return time.Duration(max(120, min(secs, 300))) * time.Second
}
```

**Restart recovery**:
1. Load `queue.json.gz` → list of NZO IDs
2. For each ID: load `{job_admin}/{nzo_id}.json` → reconstruct NzbObject
3. For each NzbObject: articles already assembled (file exists on disk with correct size) are marked complete; remaining articles are re-queued
4. In-flight articles at crash time (~100 out of 500K) are re-downloaded — negligible bandwidth cost

**Atomic write** for all persistence: write to `{path}.tmp`, `fsync`, `rename` over original.

---

## Phase 1: NZB Data Model and Persistence

### Step 1.1 — NZB Parsing and Data Model `[sonnet]`

Evaluate existing Go NZB libraries and wrap or extend the best fit.

```
Deliverables:
  - Evaluate these libraries against our needs:
    * github.com/GJRTimmer/nzb — Has JSON save/load, article ID fixup
    * github.com/chrisfarms/nzb — Simpler, fewer features
    * github.com/andrewstuart/go-nzb — Minimal
    Check each for: <meta> tag support (password, category), segment ordering,
    gzip/bzip2 wrapper handling, last commit date, test coverage.

  - internal/nzb/model.go — Our domain model structs (NzbObject, NzbFile, Article)
    that may embed or adapt the library types. Must include all fields from spec §4.1:
    NzbObject: ID, Name, Category, Priority, Status, Password, Script,
               BytesTotal, BytesDownloaded, BytesMissing, BytesPar2,
               Repair, Unpack, Delete, Files []NzbFile, TimeAdded
    NzbFile:   ID, Filename, Subject, Type, Completed, Articles []Article
    Article:   ID (message-id), Bytes, Number, Tries, TryList (map[string]bool)

  - internal/nzb/parser.go — ParseNZB(io.Reader) wrapping the chosen library,
    handling gzip/bzip2 decompression, populating our model structs
  - internal/nzb/parser_test.go — Parse sample NZBs, verify segment counts, metadata
  - test/fixtures/*.nzb — Sample NZB files

If no library meets our needs (especially <meta> tag handling), write a thin
parser using encoding/xml — the NZB format is simple XML.
```

### Step 1.2 — Queue Manager Core `[opus]`

The queue is the central coordinating data structure. Uses in-memory state with channel-based signaling (see Coordination Architecture section above).

```
Deliverables:
  - internal/queue/queue.go — NzbQueue: Add, Remove, Pause, Resume, Reorder, GetArticles
    * notify channel (cap=1) for downloader wake-up
    * sync.RWMutex for concurrent access (RLock for reads in GetArticles hot path)
  - internal/queue/persistence.go — JSON+gzip persistence:
    * SaveIndex() — write queue.json.gz (just NZO IDs, tiny)
    * SaveJob(nzo) — write individual NzbObject JSON to admin dir
    * LoadQueue() — reconstruct from index + per-job files
    * All writes via atomic temp+fsync+rename
  - internal/queue/recovery.go — Restart recovery:
    * Load index, load each NzbObject
    * Scan assembled files on disk to determine which articles are already done
    * Re-queue remaining articles
  - internal/queue/duplicate.go — Duplicate detection (normalize key, check queue + history)
  - internal/queue/throttle.go — Adaptive save throttle: saveCooldown() based on job size
  - internal/queue/queue_test.go — Concurrent access tests with -race, recovery tests

Design decisions (Opus):
  - Channel-based notification (NOT sync.Cond) — composable with select{}
  - GetArticles() takes RLock only — no write lock on the hot dispatch path
  - Article-level state lives in memory only; file-level completion persisted per-NzbObject
  - Priority ordering via sorted slice insertion (queue is small, always iterated fully)
  - Save triggers: job add/remove, file completion (throttled), pause/resume, shutdown
```

### Step 1.3 — History Database `[sonnet]`

SQLite via `modernc.org/sqlite` (pure Go, no CGO).

```
Deliverables:
  - internal/history/db.go — Open, migrate schema, VACUUM on startup
  - internal/history/schema.go — CREATE TABLE matching spec §11.2 exactly
  - internal/history/repository.go — Add, Get, Search, Delete, MarkCompleted, Prune
  - internal/history/repository_test.go — CRUD tests with in-memory SQLite

Schema matches Python version for potential migration from existing installations.
```

---

## Phase 2: NNTP Protocol Engine

This phase ends with the ability to connect to a real NNTP server, authenticate, and fetch a single article.

### Step 2.1 — NNTP Connection and State Machine `[opus]`

Evaluate `github.com/Tensai75/nntp` + `github.com/Tensai75/nntpPool` as the base. The Tensai75 library provides RFC 3977 basics and connection pooling but likely lacks pipelining support and the specific penalty/capability-probing logic SABnzbd needs.

```
Deliverables:
  - Evaluate Tensai75/nntp for:
    * AUTH support (AUTHINFO USER/PASS)
    * BODY, ARTICLE, STAT, HEAD commands
    * TLS with configurable verification
    * Timeout handling
    * Extensibility for pipelining

  - internal/nntp/conn.go — NNTPConn wrapping or extending the library:
    Dial, authenticate, send commands, read responses
  - internal/nntp/state.go — Connection state enum and transition logic (spec §3.2)
  - internal/nntp/tls.go — TLS config builder for ssl_verify levels 0-3
  - internal/nntp/pipeline.go — Pipelining: buffered command channel (size = pipelining_requests)
  - internal/nntp/capabilities.go — have_body, have_stat probing (500 → fallback)
  - internal/nntp/conn_test.go — Test against mock NNTP server (net.Pipe based)

Design decisions (Opus):
  - One goroutine per NNTPConn — idiomatic Go, replaces Python's selector loop
  - Commands sent via channel, responses returned via channel
  - State transitions enforced: cannot send BODY before READY
  - Pipelining: semaphore (chan struct{}) with cap = pipelining_requests
  - 256 KB read buffer (bufio.NewReaderSize)
  - Timeout: context.WithTimeout per command, server-level default 60s

If the Tensai75 library is too rigid to extend, fork it or write a minimal
NNTP client — the protocol surface we use is small (6 commands).
```

### Step 2.2 — Server Manager and Penalty Logic `[sonnet]`

Manages multiple servers with priorities, penalties, and connection pools.

```
Deliverables:
  - internal/downloader/server.go — Server struct, connection pool, penalty tracking
  - internal/downloader/penalty.go — Penalty constants, backoff logic, optional server deactivation
  - internal/downloader/resolver.go — Async DNS resolution (net.Resolver with goroutine)
  - internal/downloader/server_test.go — Penalty escalation, optional server deactivation at 30% bad

Server.BadConnections / Server.Connections > 0.3 → deactivate optional server.
Required servers: never deactivated, just penalized.
```

### Step 2.3 — Downloader Orchestrator `[opus]`

Coordinates all servers and connections. Dispatches articles from queue to connections.

```
Deliverables:
  - internal/downloader/downloader.go — Downloader: Start, Stop, Pause, Resume, SetSpeedLimit
  - internal/downloader/dispatch.go — Article dispatch loop: queue.GetArticles → assign to connections
  - internal/downloader/downloader_test.go — Integration test: mock NNTP server, download articles

Design decisions (Opus):
  - Main loop: select{} on queue.notify channel, completions channel, speedChanged channel, ctx.Done()
    (see Coordination Architecture section — NOT sync.Cond)
  - Per-server: pool of NNTPConn goroutines, each pulling from a per-server article channel
  - Completion flow: connection goroutine sends result on shared completions channel →
    downloader calls queue.RegisterArticle() → triggers persistence if file completed
  - Backpressure: if all connections busy, don't consume from queue.notify (let it coalesce)
  - Speed limiting: token bucket (golang.org/x/time/rate), updated via speedChanged channel
  - Graceful shutdown: cancel context → each connection goroutine drains current article →
    unfinished articles returned to queue → final queue save
```

---

## Phase 3: Decode and Assemble

Turn raw NNTP article data into files on disk.

### Step 3.1 — yEnc Decoder Integration `[sonnet]`

Wrap an existing yEnc library rather than reimplementing.

```
Deliverables:
  - Evaluate and benchmark:
    * github.com/GJRTimmer/yenc — Multi-core, CRC32 built-in
    * github.com/chrisfarms/yenc — Simpler, single-part and multi-part
    * github.com/ovrlord-app/go-yenc-decoder — Enhanced error handling
    Pick based on: decode throughput (target >500 MB/s), CRC verification,
    multi-part support, error handling for malformed data.

  - internal/decoder/decoder.go — DecodeArticle() wrapping the chosen library,
    returning (data []byte, offset int64, length int64, crc uint32, err error)
  - internal/decoder/uu.go — UU decode fallback (rare, can use encoding or small implementation)
  - internal/decoder/decoder_test.go — Decode real yEnc samples, verify CRC, benchmark
  - internal/decoder/bench_test.go — Benchmark: verify >500 MB/s throughput

If no library meets performance needs, the yEnc algorithm is simple enough
to implement as a tight loop — but start with a library.
```

### Step 3.2 — Article Cache `[sonnet]`

Memory-bounded cache with disk spill.

```
Deliverables:
  - internal/cache/cache.go — ArticleCache: Save, Load, ReserveSpace, Flush
  - internal/cache/cache_test.go — Test memory limit enforcement, disk spill, load-after-spill

Design:
  - sync.RWMutex-protected map[string][]byte for in-memory articles
  - When memory usage > limit: write to disk ({admin_dir}/{article_id})
  - Flush trigger at 90% capacity (non-contiguous flush)
  - Track current memory usage with atomic int64
```

### Step 3.3 — File Assembler `[sonnet]`

Writes decoded articles to target files using seek-based out-of-order assembly.

```
Deliverables:
  - internal/assembler/assembler.go — Assembler: Start, Stop, WriteArticle
  - internal/assembler/diskspace.go — Free space check, pause downloader if below threshold
  - internal/assembler/assembler_test.go — Assemble out-of-order articles into correct file

Design:
  - Worker goroutine consuming from buffered channel (cap 12 = DEF_MAX_ASSEMBLER_QUEUE)
  - Per-file: os.File with WriteAt (no seek needed — pwrite is atomic per call)
  - Batch writes: flush every 5s (ASSEMBLER_WRITE_INTERVAL) or on channel drain
  - On NzbFile complete: notify queue, signal DirectUnpacker if active
  - On NzbObject complete: push to post-processor channel
```

---

## Phase 4: Integration Milestone — End-to-End Download

Wire Phase 1-3 together for a working download pipeline. This is the first testable end-to-end milestone.

### Step 4.1 — Pipeline Integration `[opus]`

Connect: Queue → Downloader → Decoder → Cache → Assembler → file on disk.

```
Deliverables:
  - cmd/sabnzbd/main.go — Wire all components, accept NZB file path as CLI arg
  - internal/app/app.go — Application struct, Start/Shutdown lifecycle
  - Integration test: parse NZB → download from mock NNTP → decode → assemble → verify file

This is the first "it actually works" moment. Test with a mock NNTP server
that serves canned yEnc articles for a known test file.

Build tag: //go:build integration
```

---

## Phase 5: Post-Processing Pipeline

### Step 5.1 — Post-Processor Orchestrator `[sonnet]`

Stage-based pipeline (spec §8.1).

```
Deliverables:
  - internal/postproc/postproc.go — PostProcessor: Start, Stop, Process(NzbObject)
  - internal/postproc/stages.go — Stage interface, stage registry, stage log accumulator
  - internal/postproc/queue.go — Fast queue (direct unpack) + slow queue, max 3 fast per cycle
  - internal/postproc/postproc_test.go — Mock stages, verify ordering and log capture
```

### Step 5.2 — Par2 Repair Stage `[sonnet]`

Shell out to `par2` binary.

```
Deliverables:
  - internal/par2/par2.go — FindPar2Files, Verify, Repair (exec.CommandContext)
  - internal/par2/par2_test.go — Integration test (skip if par2 not installed)

Invocation: par2 r <par2file> — capture stdout/stderr, parse exit code.
```

### Step 5.3 — Unpack Stage (UnRAR + 7zip) `[sonnet]`

```
Deliverables:
  - internal/unpack/unrar.go — Extract RAR (exec: unrar e -y -p<pw> -idp <rar> <outdir>)
  - internal/unpack/sevenzip.go — Extract 7z (exec: 7zz e -y -p<pw> <archive> -o<outdir>)
  - internal/unpack/filejoin.go — Join split files (.001, .002, ...)
  - internal/unpack/detect.go — Scan directory for archives, determine type
  - internal/unpack/unpack_test.go — Test with sample archives
```

### Step 5.4 — Direct Unpack `[opus]`

Extract while downloading — requires careful coordination with assembler.

```
Deliverables:
  - internal/unpack/direct.go — DirectUnpacker goroutine: watches for completed RAR volumes
  - internal/unpack/direct_test.go — Simulate volume-by-volume arrival, verify extraction

Design (Opus):
  - Goroutine per NzbObject (when direct_unpack enabled and RAR detected)
  - Wait on channel for "volume N assembled" signals from assembler
  - Spawn unrar subprocess, feed volumes in order
  - On failure: mark for standard unpack fallback
```

### Step 5.5 — Deobfuscation and Sorting `[sonnet]`

```
Deliverables:
  - internal/deobfuscate/deobfuscate.go — Rename obfuscated files using NZB subject hints
  - internal/sorting/mediainfo.go — Media type detection (TV/Movie/Date)
    Evaluate existing Go libraries for media filename parsing:
    * github.com/middelink/go-parse-torrent-name
    * regex-based fallback for common patterns (S01E02, 2024, etc.)
  - internal/sorting/template.go — Parse and apply sort_string templates (%s, %e, %y, etc.)
  - internal/sorting/sorter.go — Apply matching sorter rule, move files to final dir
  - internal/sorting/sorter_test.go — Template expansion, media detection tests
```

### Step 5.6 — Post-Processing Script Runner `[sonnet]`

```
Deliverables:
  - internal/postproc/script.go — RunScript: build argv + env vars (spec §8.4), exec, capture log
  - internal/postproc/script_test.go — Test env var population, return code handling

Must set BOTH positional args AND SAB_* environment variables for backward compat
with existing user scripts from Python SABnzbd installations.
```

---

## Phase 6: HTTP API and Web Interface

### Step 6.1 — API Server Skeleton `[opus]`

Establish the router, auth middleware, and response format.

```
Deliverables:
  - internal/api/server.go — HTTP server setup (net/http stdlib)
  - internal/api/middleware.go — API key auth, session cookie auth, localhost bypass
  - internal/api/response.go — JSON/XML response helpers, error envelope
  - internal/api/router.go — Route /api to mode dispatcher
  - internal/api/server_test.go — Auth middleware tests, response format tests

Design (Opus):
  - Single /api endpoint, mode= parameter dispatches to handler functions
  - Use stdlib net/http (single endpoint doesn't need a framework)
  - Middleware chain: logging → auth → handler
  - JSON via encoding/json
```

### Step 6.2 — Queue and History API Handlers `[sonnet]`

The most-used API endpoints.

```
Deliverables:
  - internal/api/queue.go — mode=queue (all sub-actions), addfile, addurl, addlocalfile
  - internal/api/history.go — mode=history (get, delete, mark_as_completed)
  - internal/api/queue_test.go — Test all queue sub-actions
  - internal/api/history_test.go — Test history retrieval with pagination
```

### Step 6.3 — Status and Config API Handlers `[haiku]`

Follow the exact pattern established in 6.2.

```
Deliverables:
  - internal/api/status.go — mode=fullstatus, status, version, auth, warnings, server_stats
  - internal/api/config.go — mode=get_config, set_config, test_server, backup
  - internal/api/control.go — mode=pause, resume, shutdown, restart, speedlimit, disconnect
  - internal/api/misc.go — mode=get_cats, get_scripts, browse, eval_sort, watched_now, rss_now
  - Tests for each
```

### Step 6.4 — Web UI Static Serving `[haiku]`

Serve the Glitter UI (copied from Python repo as static files).

```
Deliverables:
  - internal/web/server.go — Serve static files from web/ directory
  - internal/web/auth.go — Login page, session management
  - Copy interfaces/Glitter/ from Python repo into web/static/

The Glitter UI is pure HTML/JS/CSS that talks to the /api endpoint.
It should work unmodified once the API is compatible.
```

---

## Phase 7: Supporting Subsystems

These are independent of each other and can be implemented in parallel.

### Step 7.1 — RSS Feed Processor `[sonnet]`

```
Deliverables:
  - internal/rss/parser.go — Parse feeds using github.com/mmcdole/gofeed
  - internal/rss/filter.go — Apply filter rules (require, must_not_match, ignore, size, age)
  - internal/rss/dedup.go — Deduplication state (JSON persistence)
  - internal/rss/scanner.go — Periodic scan goroutine, per-feed enable/disable
  - internal/rss/scanner_test.go — Filter matching, dedup tests
```

### Step 7.2 — Directory Scanner `[haiku]`

```
Deliverables:
  - internal/dirscanner/scanner.go — Scan dir, detect stable files, auto-add NZBs
  - internal/dirscanner/decompress.go — Handle .nzb.gz, .nzb.bz2, .zip containing NZBs
  - internal/dirscanner/state.go — Track inode/size/mtime/ctime per file (JSON persistence)
  - internal/dirscanner/scanner_test.go — Stability detection (file changes between scans)
```

### Step 7.3 — Scheduler `[sonnet]`

```
Deliverables:
  - internal/scheduler/scheduler.go — Cron-like scheduler: parse schedule format, tick every minute
  - internal/scheduler/actions.go — Action registry: map action names to handler functions
  - internal/scheduler/onetime.go — One-shot events (server resume after penalty)
  - internal/scheduler/scheduler_test.go — Schedule matching, one-shot fire-and-forget
```

### Step 7.4 — Notification System `[sonnet]`

```
Deliverables:
  - internal/notifier/notifier.go — Notifier interface, event type enum, dispatch
  - internal/notifier/email.go — SMTP/SMTP_SSL (net/smtp + crypto/tls)
  - internal/notifier/apprise.go — Apprise URL-based notification (HTTP POST)
  - internal/notifier/script.go — Custom notification script runner
  - internal/notifier/notifier_test.go — Event routing tests
```

### Step 7.5 — Bandwidth Meter and Quota `[sonnet]`

```
Deliverables:
  - internal/bpsmeter/meter.go — Real-time BPS (rolling window), per-server stats
  - internal/bpsmeter/quota.go — Daily/weekly/monthly quota, pause on exceed
  - internal/bpsmeter/persistence.go — JSON persistence
  - internal/bpsmeter/speedlimit.go — Wrap golang.org/x/time/rate for bandwidth throttling
  - internal/bpsmeter/meter_test.go — Quota rollover, speed limit accuracy
```

### Step 7.6 — URL Grabber `[haiku]`

```
Deliverables:
  - internal/urlgrabber/grabber.go — Fetch URL (net/http), detect content type, decompress, add to queue
  - internal/urlgrabber/grabber_test.go — Test with net/http/httptest
```

---

## Phase 8: Security and TLS

### Step 8.1 — Certificate Generation `[haiku]`

Self-signed TLS certs for HTTPS UI.

```
Deliverables:
  - internal/app/certgen.go — Generate Ed25519 key + self-signed cert
    (CN=sabnzbd, SAN=127.0.0.1+::1+localhost, 5-year validity)
    Use crypto/x509 + crypto/ed25519 from stdlib.
  - internal/app/certgen_test.go — Generate and parse cert, verify fields
```

Ed25519 is chosen over RSA-4096: 32-byte public key vs 512 bytes, faster
sign/verify, equivalent-or-better security, and supported by all TLS 1.3
clients. Python SABnzbd's RSA-4096 default is not a compatibility
constraint here — self-signed certs are per-install artifacts, so the
algorithm choice is free. See § Design Policy: Compatibility Scope.

### Step 8.2 — API Key Management `[haiku]`

```
Deliverables:
  - internal/api/apikey.go — Generate, validate, regenerate API keys (crypto/rand, 16-char hex)
  - internal/api/apikey_test.go — Generation uniqueness, validation
```

---

## Phase 9: Application Lifecycle

### Step 9.1 — Startup Orchestration `[opus]`

Wire all subsystems with proper startup ordering and graceful shutdown.

```
Deliverables:
  - internal/app/app.go — Application: New, Start, Shutdown, signal handling
  - internal/app/lockfile.go — Single-instance lock (flock on config dir)
  - cmd/sabnzbd/main.go — Flag parsing (-f, -d, --server, --port, etc.), call app.Start

Startup order (spec §2):
  1. Parse flags → 2. Load config → 3. Init logging → 4. Acquire lock →
  5. Init DB → 6. Load queue → 7. Start BPS meter → 8. Start scheduler →
  9. Start cache → 10. Start assembler → 11. Start post-processor →
  12. Start dir scanner → 13. Start RSS → 14. Start downloader →
  15. Start HTTP server → 16. Optional browser launch

Shutdown: reverse order, context cancellation, drain goroutines, save state.
```

### Step 9.2 — Logging `[haiku]`

```
Deliverables:
  - internal/app/logging.go — Structured logging (log/slog with file + stderr output)
  - Configurable log level, log file path from config
```

---

## Phase 10: Migration Compatibility — **SKIPPED (out of scope)**

Per user direction (2026-04-16), migration from an existing Python SABnzbd install
is not a goal of this project. The Go reimplementation targets fresh installs only.
This reverses the earlier "Required compatibility" row for the history DB schema
in the *Design Policy: Compatibility Scope* section above — history is now
Go-native only.

Steps 10.1 (History DB Migration) and 10.2 (Config Migration Tool) are intentionally
left unimplemented. If migration is ever needed, it can be added as a standalone
tool without affecting the main binary.

---

## Phase 11: Testing and Hardening

### Step 11.1 — Mock NNTP Server `[sonnet]`

For integration testing without a real Usenet server.

```
Deliverables:
  - test/mocknntp/server.go — TCP server implementing NNTP protocol subset
  - test/mocknntp/articles.go — Canned yEnc-encoded articles for known test files
  - Configurable: auth required, BODY/STAT support toggle, random failures, timeouts
```

### Step 11.2 — End-to-End Integration Tests `[sonnet]`

```
Deliverables:
  - test/integration/download_test.go — NZB → download → decode → assemble → verify checksum
  - test/integration/postproc_test.go — Assemble → par2 verify → unrar → sort → verify output
  - test/integration/api_test.go — Start server, exercise all API modes, verify responses
  - test/integration/rss_test.go — Mock RSS feed → filter → auto-add to queue

Build tag: //go:build integration
```

### Step 11.3 — Benchmark Suite `[sonnet]`

```
Deliverables:
  - internal/decoder/bench_test.go — yEnc decode throughput
  - internal/cache/bench_test.go — Cache save/load under contention
  - internal/queue/bench_test.go — GetArticles with 1000 jobs, 100 servers
  - internal/nntp/bench_test.go — Response parsing throughput
```

---

## Phase 12: Glitter UI Port

Port upstream SABnzbd's Cheetah templates to Go `html/template` so the
daemon actually serves the Glitter web UI (today it only serves a
placeholder landing page — see `internal/web/static/index.html`).

Context (measured from `../sabnzbd/interfaces/Glitter/templates/`):

| Template | Lines | Cheetah directives | Notes |
|---|---:|---:|---|
| `main.tmpl` | 164 | 21 (`#if`, `#include`, `#set`) | Shell + server-side glue |
| `include_menu.tmpl` | 118 | 11 (all `#if`) | Feature-flag toggles |
| `include_overlays.tmpl` | 839 | 2 (`#set`, `#from`) | Modals; otherwise pure HTML + Knockout |
| `include_queue.tmpl` | 230 | 0 | Pure HTML + Knockout + `$T()` |
| `include_history.tmpl` | 175 | 0 | Pure HTML + Knockout + `$T()` |
| `include_messages.tmpl` | 47 | 0 | Pure HTML + Knockout + `$T()` |

Data surface: 22 top-level context variables (`apikey`, `version`,
`active_lang`, `rtl`, `color_scheme`, `webdir`, `new_release`,
`new_rel_url`, feature flags `have_logout`/`have_quota`/
`have_rss_defined`/`have_watched_dir`, etc.). Translation: 236 unique
`$T('key')` lookups across all templates.

**Strategy**: build the context struct and renderer first, then port
zero-directive templates to prove the renderer, then the two
directive-heavy templates, then wire i18n. Ship English-only
translations in v1; real multi-language is a separate follow-up.

**Validation at each step** (all steps):
- Quality gates: `go vet ./... && go test -race -count=1 ./... && golangci-lint run ./...`
- Step-specific: rendered-HTML assertion via `httptest` against known
  markers (div IDs, `data-bind` attributes, injected JS globals). Failed
  step = failed commit; move to next step only after green.

### Step 12.1 — Vendor `staticcfg/` icons `[haiku]`

Glitter's `main.tmpl` references `./staticcfg/ico/favicon.ico` and
seven `apple-touch-icon-*.png` / android / safari mask-icon assets.
Upstream stores them under `interfaces/Config/templates/staticcfg/`
(shared across skins), not under Glitter itself.

```
Deliverables:
  - internal/web/static/staticcfg/ico/ — copy favicon.ico, apple-touch-icon-*.png,
    android-192x192.png, safari-pinned-tab.svg from upstream
  - Update internal/web/server.go routing so /staticcfg/ maps to the
    embedded staticcfg subtree

Validation:
  - curl -I http://127.0.0.1:8080/staticcfg/ico/favicon.ico → 200 OK
  - httptest asserts Content-Type: image/x-icon for favicon, image/png for apple-touch-icons
```

### Step 12.2 — Render-context struct and template pipeline `[sonnet]`

Build the foundation every template step depends on: the Go struct
holding all context variables, the `html/template` setup with required
FuncMap entries, and a handler that renders `main.html.tmpl` from an
`embed.FS`. Keep the body of `main.html.tmpl` trivial for this step
(just `<h1>{{.Version}}</h1>`) — the real port is Step 12.4.

```
Deliverables:
  - internal/web/render.go — RenderContext struct (api_key, version,
    active_lang, rtl, color_scheme, webdir, new_release, new_rel_url,
    bytespersec_list, have_* flags). Builder func takes *config.Config
    + *queue.Queue + runtime state, returns fully populated context.
  - internal/web/render.go — template.FuncMap with placeholders: T, staticURL
  - internal/web/server.go — new renderIndex handler replacing the
    placeholder; parses main.html.tmpl from embed.FS once at startup
  - internal/web/templates/main.html.tmpl — minimal stub that
    references {{.Version}} and {{.APIKey}} to prove data wiring
  - internal/web/render_test.go — table-driven RenderContext builder tests;
    httptest asserts rendered HTML contains the expected Version + APIKey

Validation:
  - GET / returns 200 with text/html
  - Body contains the configured version string and api_key value
  - Go quality gates pass
```

### Step 12.3 — Translation function stub `[haiku]`

Ship `T(key)` returning the key itself (English fallback). This lets
`$T(...)` porting happen mechanically in Steps 12.4-12.8 without
blocking on a real i18n catalog. Real translations are a follow-up.

```
Deliverables:
  - internal/i18n/catalog.go — type Catalog map[string]string; Lookup(key) string
    returning the key verbatim when no entry exists
  - internal/i18n/catalog.go — embed upstream sabnzbd English strings
    if trivially available (po file or JSON); otherwise ship an empty
    map and rely on fallback
  - internal/web/render.go — wire T into the FuncMap so templates can
    call {{T "post-Paused"}}
  - internal/i18n/catalog_test.go — Lookup returns key on miss, value on hit

Validation:
  - Template expression {{T "menu-queue"}} renders the English text
    (or the key itself if catalog is empty) — not an error
```

### Step 12.4 — Port `include_messages.tmpl` `[haiku]`

Smallest template (47 lines), zero Cheetah directives. Mechanical
substitution of `$T('key')` → `{{T "key"}}`. Serves as the reference
conversion pattern for the other zero-directive templates.

```
Deliverables:
  - internal/web/templates/include_messages.html.tmpl
  - internal/web/templates/messages_test.go — render assertion:
    output contains #messages root div and expected Knockout
    data-bind attributes preserved verbatim

Validation:
  - Rendered output contains every data-bind= attribute from the upstream source
  - No $T( or $ tokens remain in rendered output
```

### Step 12.5 — Port `include_queue.tmpl` + `include_history.tmpl` `[haiku]`

Same mechanical pattern as 12.4 scaled up (230 + 175 lines, zero
directives each). Two templates in one step because the pattern is
now proven and both are pure `$T()` substitution.

```
Deliverables:
  - internal/web/templates/include_queue.html.tmpl
  - internal/web/templates/include_history.html.tmpl
  - Tests asserting #queue-tab / #history-tab roots and Knockout
    bindings survive the port intact

Validation:
  - grep '\$T\|<!--#' on rendered output returns nothing
  - The same data-bind attribute set as upstream is present
```

### Step 12.6 — Port `include_menu.tmpl` `[sonnet]`

First directive-heavy template: 11 `#if` blocks gating menu entries
on feature flags (`have_logout`, `have_quota`, `have_rss_defined`,
`have_watched_dir`, etc.). Translation is `#if $flag` → `{{if .Flag}}`.

```
Deliverables:
  - internal/web/templates/include_menu.html.tmpl
  - Table-driven render test: each feature flag on/off produces
    the expected menu entry present/absent

Validation:
  - When have_rss_defined=true, #rss-menu-item is in output; when false, it is absent
  - Same for the other 4 feature flags
```

### Step 12.7 — Port `include_overlays.tmpl` `[sonnet]`

839 lines but only 2 directives (one `#set` for a helper value, one
`#from sabnzbd` import that we can skip entirely since we only need
the rendered HTML). Size drives the model choice, not complexity.

```
Deliverables:
  - internal/web/templates/include_overlays.html.tmpl
  - Render test: at least one representative modal (e.g. #modal_options,
    #modal_add_nzb, #modal_shutdown) renders with expected structure
  - Visual sanity test: rendered page includes the expected count of
    <div class="modal"> elements (~15-20 upstream)

Validation:
  - All data-bind attributes from upstream preserved
  - All $T() calls replaced; no $ or <!--# tokens remain
  - Modal count matches upstream source
```

### Step 12.8 — Port `main.tmpl` `[sonnet]`

The shell template. Most complex: 21 directives including `#include`
(replace with `{{template "name" .}}`), `#include raw` for inline JS
bundles (replace with `<script src="/static/...">` tags — serving JS
as separate requests is modern best practice and simpler than
embed-then-inline), and the one `#set $active_lang=...` (do the
normalization in the Go builder, not the template).

Special handling:
- `<!--#if $rtl#-->dir="rtl"<!--#end if#-->` → `{{if .RTL}}dir="rtl"{{end}}`
- Inline JS `var apiKey = "$apikey";` → `var apiKey = {{.APIKey | js}};`
  (use the `js` template func to ensure proper quoting and escaping)
- `glitterTranslate.X = "$T('Y')"` → `glitterTranslate.X = {{T "Y" | js}};`
- 10 `#include raw $webdir + "/..."` for JS bundles → drop from
  template; add matching `<script src="/static/glitter/...">` tags
  pointing at the already-embedded assets

```
Deliverables:
  - internal/web/templates/main.html.tmpl — the full shell wiring
    the four include_*.html.tmpl children
  - internal/web/render.go — update to ParseFS the full template set
    (template.ParseFS with all include_*.html.tmpl names) so {{template}}
    resolves across files
  - Integration test: GET / renders full page; scrape for:
    * <script> block containing apiKey = "configured-value"
    * <script src="/static/glitter/javascripts/glitter.js"> tag
    * All 4 include blocks present (queue, history, messages, overlays)
    * <html lang="..."> matches configured language

Validation:
  - go test ./internal/web/ -run TestRenderIndex passes
  - Visual: user runs ./sabnzbd --serve, opens /, browser dev tools
    show the Knockout view model bound (ko.observable elements update
    when /api?mode=queue is polled)
  - No console errors in browser
```

### Step 12.9 — Browser smoke test and README restoration `[sonnet]`

Manual validation with a real browser, plus restore the README's
UI-opening Quickstart step now that it's truthful.

```
Deliverables:
  - docs/ui_smoke_checklist.md — step-by-step manual verification:
    page loads, queue/history/warnings tabs switch, shutdown modal
    opens, api key prompt works, speed graph renders, refresh works
  - README.md — add back the "open http://127.0.0.1:8080/ and use the UI"
    flow; remove the "API-only for now" caveat from Status section

Validation:
  - All items in docs/ui_smoke_checklist.md pass by user inspection
  - curl http://127.0.0.1:8080/ | grep 'ko.applyBindings' returns a match
  - Go quality gates still green
```

### Model selection rationale

| Step | Model | Why |
|---|---|---|
| 12.1 | haiku | File copy + 1-line routing change |
| 12.2 | sonnet | Data-surface design; every later step depends on the struct shape |
| 12.3 | haiku | Map wrapper with trivial fallback |
| 12.4 | haiku | Mechanical `$T()` → `{{T}}` substitution; pattern reference |
| 12.5 | haiku | Same pattern as 12.4 at larger size |
| 12.6 | sonnet | 11 `#if` translations need care around Knockout-adjacent markup |
| 12.7 | sonnet | Size (839 lines) requires judgement on what to test |
| 12.8 | sonnet | Shell template with nontrivial JS-escaping and `{{template}}` wiring |
| 12.9 | sonnet | Writes the manual-verification doc and re-verifies README claims |

Opus is intentionally not used in Phase 12: the design work (data
surface, render pipeline) fits in one Sonnet step once the inventory
is in hand. Escalate to Opus only if Step 12.2 surfaces an
architectural ambiguity the plan didn't anticipate.

### Dependencies

```
12.1 ─┐
      ├─► 12.2 ─► 12.3 ─► 12.4 ─► 12.5 ─► 12.6 ─► 12.7 ─► 12.8 ─► 12.9
      │                                                          │
      └─────────── 12.1 also serves 12.8's <link rel=icon> path ─┘
```

Each step leaves the daemon in a working state. Stopping at Step 12.5
already yields three functional panels (queue, history, messages)
rendered through the new pipeline, with the old placeholder still
serving when the template falls back. Stop at any step; the
repository compiles and passes tests.

---

## Dependency Summary

```
Required:
  modernc.org/sqlite              # Pure-Go SQLite
  gopkg.in/yaml.v3                # YAML config
  github.com/mmcdole/gofeed       # RSS/Atom feed parsing
  github.com/dustin/go-humanize   # Human-readable size parsing/formatting
  golang.org/x/time               # Rate limiting (token bucket)
  golang.org/x/crypto             # bcrypt for password hashing

Evaluate during implementation (pick one per category):
  NZB:  github.com/GJRTimmer/nzb OR github.com/chrisfarms/nzb
  yEnc: github.com/GJRTimmer/yenc OR github.com/chrisfarms/yenc
  NNTP: github.com/Tensai75/nntp + github.com/Tensai75/nntpPool (or fork)
```

---

## Implementation Order and Dependencies

```
Phase 0 ──────────────────────────────────────── (no deps)
  ↓
Phase 1 ──────────────────────────────────────── (depends on Phase 0)
  ↓
Phase 2 ──────────────────────────────────────── (depends on Phase 0, 1)
  ↓
Phase 3 ──────────────────────────────────────── (depends on Phase 0, 1)
  ↓
Phase 4 ──────────────────────────────────────── (depends on Phase 1, 2, 3)
  ↓
Phase 5 ─────┐                                   (depends on Phase 1, 3)
Phase 6 ─────┤ (can run in parallel)              (depends on Phase 1, 4)
Phase 7 ─────┤                                    (depends on Phase 0, 1)
Phase 8 ─────┘                                    (depends on Phase 0)
  ↓
Phase 9 ──────────────────────────────────────── (depends on all above)
  ↓
Phase 10 ─────────────────────────────────────── (depends on Phase 0, 1)
  ↓
Phase 11 ─────────────────────────────────────── (depends on all above)
```

Phases 5, 6, 7, and 8 are independent and can be assigned to parallel agents.

---

## Critical Design Decisions (Opus Required)

These cross-cutting decisions affect multiple packages and should be made in Phase 0/1:

1. **Error strategy**: Typed errors per package? Sentinel errors? Always wrap with `fmt.Errorf("...: %w", err)`?
2. **Context propagation**: All blocking operations accept `context.Context`.
3. **Concurrency primitives by component** (DECIDED):
   - **Queue ↔ Downloader**: Channel-based notification (`chan struct{}`, cap=1, non-blocking send). NOT sync.Cond. Rationale: composable with `select{}`, no lock-hold requirement, natural coalescing of rapid mutations.
   - **Queue internal**: `sync.RWMutex`. GetArticles takes RLock (hot path). Mutations take full Lock + notify.
   - **NzbObject internal**: `sync.Mutex` per object (matches Python's per-NZO lock).
   - **Article cache**: `sync.RWMutex` + atomic int64 for memory tracking.
   - **Downloader**: `select{}` loop over multiple channels (queue notify, completions, speed changes, shutdown).
4. **Persistence strategy** (DECIDED): In-memory queue with event-triggered file persistence. NOT SQLite for queue. See "Coordination Architecture" section for full rationale and implementation details.
5. **Graceful shutdown ordering**: Cancel root context → downloader drains connections → assembler flushes → queue saves all NzbObjects → post-processor completes current job → history DB closes → config saves.
6. **Config change notification**: Channel-based (fan-out) or callback registration? Decide based on subscriber patterns.
7. **Logging**: `log/slog` structured logging from day one — all packages use `slog.Logger` passed via constructor.
