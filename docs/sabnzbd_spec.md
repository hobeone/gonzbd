# SABnzbd Go Reimplementation Specification

**Source**: SABnzbd v5.x (Python 3.9+)  
**Purpose**: Automated Usenet binary newsreader — downloads NZB files, verifies, repairs, and extracts archives with zero human interaction.

---

## Table of Contents

1. [System Overview](#1-system-overview)
2. [Architecture](#2-architecture)
3. [NNTP Protocol Engine](#3-nntp-protocol-engine)
4. [Queue Manager](#4-queue-manager)
5. [Article Cache](#5-article-cache)
6. [Decoder](#6-decoder)
7. [Assembler](#7-assembler)
8. [Post-Processing Pipeline](#8-post-processing-pipeline)
9. [Configuration System](#9-configuration-system)
10. [HTTP API](#10-http-api)
11. [History Database](#11-history-database)
12. [RSS Feed Processor](#12-rss-feed-processor)
13. [Directory Scanner](#13-directory-scanner)
14. [Scheduler](#14-scheduler)
15. [Notifications](#15-notifications)
16. [Bandwidth Metering and Quotas](#16-bandwidth-metering-and-quotas)
17. [Data Formats and Types](#17-data-formats-and-types)
18. [Security](#18-security)
19. [Suggested Agents for Spec Refinement](#19-suggested-agents-for-spec-refinement)

---

## 1. System Overview

SABnzbd automates the complete lifecycle of Usenet binary downloads:

1. Accept NZB files (via web UI, API, RSS feeds, or watched folder)
2. Download article segments from one or more NNTP servers
3. Decode yEnc/UU-encoded segments
4. Assemble decoded segments into files
5. Verify and repair with par2
6. Extract archives (RAR, 7z, ZIP)
7. Sort/rename output files by detected content type (TV, movie, etc.)
8. Run user-provided post-processing scripts
9. Move final output to a configured completion directory

The application is long-running, daemon-capable, and provides a browser-based UI and a JSON/XML API for third-party integration.

---

## 2. Architecture

### Concurrency Model

The system is built around independent concurrent subsystems that communicate through shared, lock-protected state:

```
[NZB Sources] ──▶ [URL Grabber] ──▶ [NZB Parser] ──▶ [NzbQueue]
                                                           │
                                            ┌──────────────┘
                                            ▼
                                      [Downloader]
                                      (selector loop)
                                            │
                                   ┌────────┴────────┐
                                   ▼                 ▼
                             [NewsWrapper]     [NewsWrapper]
                             (NNTP conn 1)     (NNTP conn N)
                                   │
                                   ▼
                             [Decoder] ──▶ [ArticleCache]
                                                 │
                                          ┌──────┘
                                          ▼
                                     [Assembler]
                                          │
                                          ▼
                                   [PostProcessor]
                                          │
                          ┌──────┬────────┼────────┬──────┐
                          ▼      ▼        ▼        ▼      ▼
                        [par2] [unrar] [7zip] [script] [sort]
```

### Global Locks

Two primary locks coordinate the concurrent subsystems:

| Lock | Type | Purpose |
|------|------|---------|
| `NZBQUEUE_LOCK` | RLock | All queue mutations; also serves as the base for `DOWNLOADER_CV` |
| `DOWNLOADER_CV` | Condition (on NZBQUEUE_LOCK) | Wakes the downloader when queue state changes |
| `DOWNLOADER_LOCK` | RLock | Downloader internal state (server list, speed limit) |
| `CONFIG_LOCK` | RLock | Config reads/writes |

Queue-modifying operations must: acquire `NZBQUEUE_LOCK`, mutate state, call `DOWNLOADER_CV.notify_all()`.

### Startup Sequence

1. Parse command-line flags (`-f configfile`, `-d` daemon, `--server`, `--port`, etc.)
2. Load configuration INI file
3. Initialize logging
4. Acquire `zc.lockfile` lock on config dir (prevent double-start)
5. Initialize database (`history1.db`)
6. Load queue from disk (`queue10.sab`)
7. Start BPS meter thread
8. Start scheduler
9. Start article cache thread
10. Start assembler thread
11. Start post-processor thread
12. Start dir scanner
13. Start RSS scanner
14. Start downloader (connects to servers)
15. Start CherryPy HTTP server
16. (Optional) Launch browser
17. Enter main event loop

---

## 3. NNTP Protocol Engine

### 3.1 Protocol Commands

Only a subset of RFC 3977 is used:

| Command | Expected Response | Purpose |
|---------|------------------|---------|
| `AUTHINFO USER <user>` | 381 (need pass), 281 (ok), 482 (rejected) | Auth step 1 |
| `AUTHINFO PASS <pass>` | 281 (ok), 481/482 (failed) | Auth step 2 |
| `ARTICLE <message-id>` | 220 (ok), 430 (missing) | Fetch full article |
| `BODY <message-id>` | 222 (ok), 423/430 (missing), 500 (unsupported) | Fetch body only |
| `STAT <message-id>` | 223 (exists), 423/430 (missing), 500 (unsupported) | Check existence (pre-check) |
| `HEAD <message-id>` | 221 (ok), 423/430 (missing) | Fallback when STAT unsupported |

**Capability probing**: BODY and STAT support are assumed true on first connection. If a server returns 500 for BODY, the `have_body` flag is cleared and ARTICLE is used instead. Similarly for STAT → HEAD fallback. These flags are persisted per-server in memory for the lifetime of the process.

### 3.2 Connection State Machine

Each `NewsWrapper` (one per NNTP connection) progresses through these states:

```
DISCONNECTED
    │ TCP connect
    ▼
CONNECTED (received 200/201 greeting)
    │ send AUTHINFO USER
    ▼
USER_SENT
    │ receive 381
    ▼
USER_OK
    │ send AUTHINFO PASS
    ▼
PASS_SENT
    │ receive 281
    ▼
READY  ──── articles can be dispatched ────▶ READING/WRITING
    │ error / timeout
    ▼
DISCONNECTED (schedule reconnect with penalty)
```

**Response codes and transitions**:
- `200`/`201` greeting → proceed to auth
- `480` at any time → re-authenticate (set `force_login`)
- `400` → treat as disconnect, apply `PENALTY_SHARE` if "maximum connections" in message
- `502`/`503` → disconnect, apply `PENALTY_502`
- `481`/`482` → auth failed, apply `PENALTY_PERM`, log error

### 3.3 SSL/TLS

SSL verification levels (`ssl_verify` config, 0–3):

| Level | Behavior |
|-------|----------|
| 0 | `CERT_NONE`, no hostname check |
| 1 | Partial chain allowed, no hostname check |
| 2 | Partial chain allowed, hostname check enabled |
| 3 | `X509_STRICT`, full chain, hostname check |

- Minimum TLS version: **TLSv1.2** by default (older allowed via `allow_old_ssl_tls` config)
- Custom cipher string: applied via `SSLContext.set_ciphers()`. When set, TLSv1.3 is disabled (forced to TLSv1.2 max) because Go's `tls.Config.CipherSuites` only applies to TLS 1.0–1.2.
- `ssl_info` (protocol + cipher name) cached per connection for display in status.

### 3.4 Non-Blocking I/O and Selector Loop

The `Downloader` thread runs a single `selectors.DefaultSelector` loop across all NNTP sockets. This is equivalent to Go's `net.Conn` with goroutines per connection, but the Python design uses a single-thread multiplexing model.

**Go recommendation**: One goroutine per `NewsWrapper` (connection), communicating with the queue via channels. This is idiomatic Go and equivalent in result.

**Socket registration**: Each socket is registered for both `EVENT_READ` and `EVENT_WRITE`. Write interest is needed so the selector wakes up when the kernel send buffer drains, allowing queued pipeline commands to be sent without polling.

**Buffer sizes**:
- Receive buffer: **256 KB** (`NNTP_BUFFER_SIZE`)
- Max receive buffer: **10 MB** (`NTTP_MAX_BUFFER_SIZE`) — dynamically grown for large responses
- No explicit send buffer; `sendall()` is used (blocks until sent)

### 3.5 Pipelining

- Default in-flight requests per connection: **2** (`DEF_PIPELINING_REQUESTS`)
- Per-server configurable via `pipelining_requests` setting
- Tracked via a semaphore (bounded); acquire before sending, release on response
- Commands are queued; the selector's write-ready event drives flushing queued commands

### 3.6 Server Penalty and Backoff

Penalty constants (in minutes):

| Constant | Minutes | Trigger |
|----------|---------|---------|
| `PENALTY_UNKNOWN` | 3 | Unrecognized error |
| `PENALTY_VERYSHORT` | 0.1 | 400 error, unknown cause |
| `PENALTY_SHORT` | 1 | Used when `no_penalties` config is true |
| `PENALTY_502` | 5 | 502/503 response |
| `PENALTY_TIMEOUT` | 10 | Repeated timeouts |
| `PENALTY_SHARE` | 10 | 400 "maximum connections" (account sharing) |
| `PENALTY_TOOMANY` | 10 | Too many connections |
| `PENALTY_PERM` | 10 | Bad credentials |

**Optional server deactivation**: When `bad_connections / thread_count > 0.3`, optional servers are deactivated for the penalty period. Required servers are never deactivated.

**Resume planning**: A scheduler event is posted for `now + penalty_minutes` to re-enable the server.

### 3.7 Address Resolution

- `get_addrinfo.py` resolves server hostnames asynchronously (asyncio-based)
- Fastest address selected (first successful probe)
- Result cached per server for the session
- On reconnect after timeout, DNS is re-resolved

### 3.8 Article Request Flow

For each article:
1. Queue calls `get_article(server)` → returns `Article` or `None`
2. `Article.get_article(server, all_servers)` checks:
   - Is server already in try-list? → skip
   - Is there a higher-priority server not yet tried? → delay (don't assign yet, wait for better server)
   - Otherwise assign this server
3. `NewsWrapper.write()` selects command: STAT (if pre-check mode), else BODY or ARTICLE based on `have_body`
4. Response received → `on_response()` dispatches to decode or retry logic

---

## 4. Queue Manager

### 4.1 Data Model

Three-level hierarchy:

```
NzbObject (download job)
  ├─ metadata: name, category, priority, status, password, script
  ├─ stats: bytes_total, bytes_downloaded, bytes_missing, bytes_par2
  ├─ flags: repair, unpack, delete (post-processing)
  └─ NzbFile[] (files within the job)
        ├─ filename, type, subject
        ├─ state: completed, direct_unpack
        └─ Article[] (NNTP segments)
              ├─ article_id (message-id)
              ├─ bytes (segment size)
              ├─ tries, fetcher, fetcher_priority
              └─ TryList (set of already-tried servers)
```

### 4.2 Priority Levels

```
REPAIR_PRIORITY = 3    # Internal: par2 repair jobs
FORCE_PRIORITY  = 2    # Force: bypass pause, download immediately
HIGH_PRIORITY   = 1
NORMAL_PRIORITY = 0
LOW_PRIORITY    = -1
PAUSED_PRIORITY = -2   # Job is paused
STOP_PRIORITY   = -4   # Internal: stop signal
DEFAULT_PRIORITY = -100 # Inherit category default
```

### 4.3 Job Statuses

```
Queued, Downloading, Paused, Fetching, Propagating,
Checking, Repairing, Verifying, Extracting, Moving,
Running, QuickCheck, Completed, Failed, Deleted, Idle
```

**Propagating**: Job is waiting for propagation delay (article not yet available on all servers).

### 4.4 Queue Persistence

- **Format**: Python pickle + gzip (incompatible with Go; must redesign)
- **Filename**: `queue10.sab` in admin directory
- **Postproc queue**: `postproc2.sab`
- **Repair modes** (on startup):
  - Mode 0: Use existing queue as-is
  - Mode 1: Use existing queue, re-add missing work-in-progress folders
  - Mode 2: Discard queue, reconstruct from `incomplete/` directory scan

**Go recommendation**: Serialize queue to JSON or Protocol Buffers. On-disk format must support atomic write (write to temp file, rename).

### 4.5 Duplicate Detection

- **Key**: Derived from NZB name (normalized: lowercase, stripped of release group tags, year, quality markers)
- **Duplicate status types**:
  - `DUPLICATE`: Same key found in queue/history
  - `DUPLICATE_ALTERNATIVE`: Found but different quality/release group
  - `SMART_DUPLICATE`: Detected via guessit title+season+episode matching
  - `SMART_DUPLICATE_ALTERNATIVE`: Smart match with differences
  - `DUPLICATE_IGNORED`: Configured to ignore duplicates

### 4.6 Article Assignment to Servers

Servers are sorted by `(priority ASC, name ASC)`. Priority 0 is highest (lowest number = highest priority).

`get_articles(server)` in the queue:
1. Iterate NzbObjects in priority order
2. For each object, iterate NzbFiles
3. For each file, iterate Articles
4. For each article: call `article.get_article(server, all_servers)`
5. Return batch of assignable articles

An article is assignable to `server` if:
- `server` not in `article.try_list`
- No higher-priority active server exists that hasn't tried this article

### 4.7 Server Configuration

```
Server:
  name        string   # unique ID
  host        string
  port        int      # Default: 119 (plain), 563 (SSL)
  username    string
  password    string
  connections int      # Number of simultaneous NNTP connections
  priority    int      # 0 = highest priority
  ssl         bool
  ssl_verify  int      # 0-3 (see §3.3)
  ssl_ciphers string   # OpenSSL cipher string, empty = defaults
  required    bool     # Never deactivate
  optional    bool     # Can deactivate temporarily
  retention   int      # Days; 0 = unlimited
  timeout     int      # Seconds; default 60
  pipelining  int      # In-flight requests; default 2
  enabled     bool
```

---

## 5. Article Cache

### 5.1 Purpose

Buffers decoded article data in memory between the decoder and the assembler, with transparent disk spill when memory is exhausted.

### 5.2 Limits

| Parameter | Default | Config Key |
|-----------|---------|-----------|
| Memory limit | 500 MB | `article_cache_size` |
| Max configurable | 1 GB | `DEF_ARTICLE_CACHE_MAX` |
| Flush trigger | 90% full | `ARTICLE_CACHE_NON_CONTIGUOUS_FLUSH_PERCENTAGE` |
| Flush interval | 0.5 sec | Assembler poll rate |

### 5.3 Operations

- `reserve_space(size)` → bool: Check if `size` bytes fit in remaining memory
- `save_article(article, data)`: Store in memory dict; if full, write to `{admin_path}/{article.article_id}`
- `load_article(article)` → bytes: Read from memory or disk
- `flush_cache()`: Signal assembler to consume and clear cache

### 5.4 Behavior

- Articles are stored by `article.article_id` (message-id) as key
- Disk-spilled articles: stored in the job's admin folder, deleted after assembly
- Non-contiguous flush: When cache is 90% full, force-flush to assembler even if not all articles in a file are ready — assembler handles out-of-order writes

---

## 6. Decoder

### 6.1 yEnc Decoding

yEnc is the dominant encoding on Usenet binary groups. Format:

```
=ybegin part=N total=T line=128 size=S name=filename.ext
=ypart begin=B end=E
<encoded binary data>
=yend size=S part=N pcrc32=XXXXXXXX
```

**Decoding steps**:
1. Locate `=ybegin`, `=ypart`, `=yend` markers
2. For each data byte: `decoded = (byte - 42) mod 256`; handle escape sequences (`=` prefix: `decoded = (byte - 64 - 42) mod 256`)
3. Verify CRC32 of decoded chunk against `pcrc32` value in `=yend`
4. Store `(begin_offset, end_offset, decoded_bytes)` per article

**UU decoding** (fallback, rare): Standard Unix-to-Unix encoding; begin/end markers are `begin <mode> <filename>` and `end`.

### 6.2 Performance

SABnzbd uses `sabctools` (C++ extension) for yEnc decode. A Go reimplementation should use an optimized yEnc library or implement the tight decode loop in Go (the algorithm is simple; performance comes from tight loops and avoiding allocations).

### 6.3 CRC Verification

- Per-article: CRC32 of decoded data verified against `pcrc32` in `=yend`
- Per-file: CRC32 accumulated across all articles verified against `=ybegin` `crc32` (if present)
- On mismatch: mark article as bad, trigger retry on another server

### 6.4 DMCA / Bad Data Detection

If an article returns data but CRC fails and all servers have been tried, the article is marked `bad`. A configurable number of bad articles (`max_art_tries`) will cause the job to be marked as incomplete.

---

## 7. Assembler

### 7.1 Responsibility

Write decoded article data to the target file in the correct position. Each article carries `(file_offset, length)` metadata from the yEnc header.

### 7.2 Write Strategy

- **Direct write**: If article metadata includes valid offset/size (yEnc), seek to `file_offset` and write. Allows out-of-order assembly.
- **Sequential**: Fallback when offset information is unavailable.
- File pre-allocated or grown as articles arrive.

### 7.3 Disk Space Checking

Before each write, verify that free disk space exceeds `min_free_space` (default 1 GB). If insufficient:
- Pause the downloader
- Notify user
- Resume when space is freed

### 7.4 File Completion

When all articles for an `NzbFile` are assembled:
1. Update NzbFile status to complete
2. If `DirectUnpacker` is active: signal it
3. When all NzbFiles in an NzbObject are complete: move job to PostProcessor queue

### 7.5 Write Performance

- Write interval: **5 seconds** (`ASSEMBLER_WRITE_INTERVAL`) — batches small writes
- Assembler queue: max **12 items** (`DEF_MAX_ASSEMBLER_QUEUE`) in-flight before backpressure

---

## 8. Post-Processing Pipeline

### 8.1 Stages

Post-processing runs sequentially through named stages:

| Stage | Index | Description |
|-------|-------|-------------|
| RSS | 0 | Mark RSS entry as processed |
| Source | 1 | Record source URL/metadata |
| Download | 2 | Record download statistics |
| Servers | 3 | Record which servers contributed |
| Repair | 4 | Run par2 verification/repair |
| Filejoin | 5 | Join split files (.001/.002/...) |
| Unpack | 6 | Extract archives |
| Deobfuscate | 7 | Rename obfuscated files |
| Script | 8 | Run user post-processing script |

The stage log is stored in the history database for display.

### 8.2 Post-Processing Flags (PP bits)

Controlled per-job and per-category:

| Bit | Flag | Meaning |
|-----|------|---------|
| 0 | repair | Run par2 repair |
| 1 | unpack | Extract archives |
| 2 | delete | Delete NZB and par2 files after success |

PP value is a bitmask: 0=none, 1=repair only, 2=unpack only, 3=repair+unpack, 7=repair+unpack+delete.

### 8.3 External Tool Invocations

#### par2

```
par2 r <par2file> [<additional_par2_files>]
```

- Tool path: config `par2_command` (default: `par2` on PATH, or bundled binary)
- `par2_turbo`: Use `par2cmdline-turbo` for multi-threaded repair (faster on multi-core)
- Detection: scan assembled files for `.par2` extension
- Recovery: Run in job's work directory
- On success: source files verified or repaired
- On failure: mark job as incomplete

#### UnRAR

```
unrar e -y -p<password> -idp <rarfile> <outputdir>
```

- Tool path: config `unrar_command`
- Password: from NZB metadata or job password field (max 127 chars, UTF-16 encoded for UnRAR)
- Multi-volume: UnRAR handles `.part01.rar`, `.part02.rar`, etc. automatically
- Direct unpack: can extract while downloading (see §8.5)

#### 7-Zip

```
7zz e -y -p<password> <archive> -o<outputdir>
```

- Tool path: config `sevenz_command`
- Formats supported: 7z, zip, and many others

#### File Join

For split files (`.001`, `.002`, ...): concatenate in order to produce the original file.

### 8.4 Post-Processing Script Interface

User scripts receive job metadata via positional arguments **and** environment
variables. The canonical reference is the Python implementation in
`sabnzbd/newsunpack.py::external_processing` + `create_env`; the spec below
reflects what Python actually emits. Script authors rely on the env-var
forms — argv is unreliable (arg 4 is a hardcoded empty string) and incomplete.

1. **Command-line arguments** (positional, nine elements including argv[0]):
   ```
   script <complete_dir> <nzb_filename> <final_name> "" <category> <group>
          <pp_status> <failure_url>
   ```
   Notes:
   - `complete_dir` is the job-specific output directory (e.g. `/complete/MyJob`), not the root complete dir
   - `nzb_filename` is the original `.nzb` file name
   - `final_name` is the post-deobfuscation job name
   - Arg 4 is always the empty string (historical placeholder)
   - `pp_status` is an integer: 0 = all stages succeeded, non-zero otherwise
   - `failure_url` is the nzo's failure URL (empty if no failure)

2. **Environment variables**, grouped by source:

   **From NzbObject fields (`ENV_NZO_FIELDS`)**:
   ```
   SAB_BYTES, SAB_BYTES_DOWNLOADED, SAB_BYTES_TRIED, SAB_CAT,
   SAB_CORRECT_PASSWORD, SAB_DUPLICATE, SAB_DUPLICATE_KEY, SAB_ENCRYPTED,
   SAB_FAIL_MSG, SAB_FILENAME, SAB_FINAL_NAME, SAB_GROUP, SAB_NZO_ID,
   SAB_OVERSIZED, SAB_PASSWORD, SAB_PP, SAB_PRIORITY, SAB_REPAIR,
   SAB_SCRIPT, SAB_STATUS, SAB_UNPACK, SAB_UNWANTED_EXT, SAB_URL
   ```

   **From post-proc extras**:
   ```
   SAB_FAILURE_URL, SAB_COMPLETE_DIR, SAB_PP_STATUS, SAB_DOWNLOAD_TIME,
   SAB_AVG_BPS, SAB_AGE, SAB_ORIG_NZB_GZ
   ```
   When the script name ends in `.py`, `SAB_PYTHONUNBUFFERED=1` is also set.

   **Always included (by `create_env`)**:
   ```
   SAB_PROGRAM_DIR, SAB_API_KEY, SAB_API_URL, SAB_PAR2_COMMAND,
   SAB_RAR_COMMAND, SAB_7ZIP_COMMAND, SAB_VERSION
   ```

   The envelope key is `SAB_API_KEY` (with underscore), not `SAB_APIKEY`.
   `SAB_FINAL_PROCESSING_DIR` is NOT emitted by Python; use `SAB_COMPLETE_DIR`.

3. **Return code**: Non-zero indicates failure (recorded in history).
4. **stdout+stderr**: Combined stream captured and stored as `script_log` in history (gzip-compressed BLOB).

### 8.5 Direct Unpack

Allows extraction to begin while the job is still downloading:
- `DirectUnpacker` thread starts when first RAR volume is assembled
- Spawns `unrar` subprocess in streaming mode
- Subsequent RAR volumes fed to the process as they complete
- Requires all volumes to succeed; falls back to standard unpack on failure

**Enable**: `cfg.direct_unpack = true`

### 8.6 Deobfuscation

Many Usenet posts use randomized filenames. After extraction, SABnzbd attempts to rename files to meaningful names:
1. Use NZB subject line to derive filename hints
2. Apply `guessit` to parse TV/movie metadata from job name
3. Match extracted files to expected filenames using difflib similarity
4. Rename if confident match found

### 8.7 Sorting / Renaming

Sorters are configured as rules (category + content-type pairs) with rename templates:
- Content types: TV, Movie, Date, Unknown
- Detection: `guessit` library parses job name
- Template tokens: `%G.I<title>`, `%s<season>`, `%e<episode>`, `%y<year>`, etc.
- Output: files moved from work directory to final directory using template path

**Per-category default**: Categories have a default sorter; jobs inherit the category's sorter if none explicitly set.

### 8.8 Quick Check (Par2 Pre-Check)

Before downloading all data, SABnzbd can send STAT requests for par2 files to determine if any articles are missing. If article availability is >99%, skip par2 download entirely (saves bandwidth).

Enable: `cfg.enable_par_cleanup = true`

---

## 9. Configuration System

### 9.1 Format

INI file (`sabnzbd.ini`) managed by `configobj` library. Go equivalent: `gopkg.in/ini.v1` or a custom INI parser.

Key design: Configuration parameters are typed objects with validators. Config reads return the value or default; writes validate before storing. INI file is rewritten atomically on save.

### 9.2 General Settings

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `host` | string | `127.0.0.1` | HTTP bind address |
| `port` | int | `8080` | HTTP port |
| `https_port` | int | `0` | HTTPS port (0=disabled) |
| `https_cert` | path | | TLS certificate path |
| `https_key` | path | | TLS key path |
| `api_key` | string | (generated) | 16-char hex API key |
| `nzb_key` | string | (generated) | 16-char hex NZB upload key |
| `username` | string | | Web UI username |
| `password` | string | | Web UI password (hashed) |
| `download_dir` | path | `~/Downloads/incomplete` | Work-in-progress directory |
| `complete_dir` | path | `~/Downloads/complete` | Final output directory |
| `dirscan_dir` | path | | Watched folder path |
| `dirscan_speed` | int | `5` | Watched folder scan interval (seconds) |
| `script_dir` | path | | Directory containing user scripts |
| `email_dir` | path | | Email template directory |
| `log_dir` | path | | Log file directory |
| `log_level` | string | `info` | Minimum log level (debug, info, warn, error) |
| `log_allow` | list | | Only log messages from these components |
| `log_deny` | list | | Suppress log messages from these components |
| `admin_dir` | path | | Admin/state file directory |
| `language` | string | `en` | UI language |

### 9.3 Download Settings

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `bandwidth_max` | string | `` | Max bandwidth (e.g., `10M`, `1G`, `0`=unlimited) |
| `bandwidth_perc` | int | `100` | Percentage of max to use |
| `min_free_space` | int | `1024` | Min free disk space in MB before pause |
| `min_free_space_cleanup` | int | `2048` | Free space needed for post-proc cleanup |
| `article_cache_size` | int | `500` | Article cache size in MB |
| `max_art_tries` | int | `3` | Max tries per article before marking bad |
| `max_art_opt` | int | `1` | Max tries on optional servers |
| `top_only` | bool | false | Only use top-priority server |
| `no_penalties` | bool | false | Use minimal penalty times |
| `pre_check` | bool | false | Pre-check article availability via STAT |
| `propagation_delay` | int | `0` | Minutes to wait before downloading |

### 9.4 Post-Processing Settings

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enable_unrar` | bool | true | Enable RAR extraction |
| `enable_7zip` | bool | true | Enable 7z extraction |
| `direct_unpack` | bool | false | Extract while downloading |
| `enable_par_cleanup` | bool | true | Delete par2 after repair |
| `par2_command` | string | `par2` | par2 binary path |
| `unrar_command` | string | (auto-detected) | UnRAR binary path |
| `sevenz_command` | string | (auto-detected) | 7-Zip binary path |
| `par2_turbo` | bool | false | Use par2cmdline-turbo |
| `ignore_unrar_dates` | bool | false | Ignore timestamps in archives |
| `overwrite_files` | bool | false | Overwrite existing extracted files |
| `flat_unpack` | bool | false | Extract all to single folder |

### 9.5 Category Configuration

Categories are named groups with per-category defaults:

```
[categories]
[[categoryname]]
  pp = 7          # Post-processing flags (bitmask)
  script = ""     # Script name or "None"
  priority = 0    # Default priority
  dir = ""        # Subdirectory within complete_dir
  newzbin = ""    # (legacy)
  order = 0       # Display order
```

**Special categories**: `Default` (fallback), `*` (catch-all).

### 9.6 Server Configuration

Each server is a subsection of `[servers]`:

```
[servers]
[[servername]]
  host = news.example.com
  port = 563
  username = user
  password = pass
  connections = 8
  ssl = 1
  ssl_verify = 2
  ssl_ciphers = ""
  priority = 0
  required = 0
  optional = 0
  retention = 0
  timeout = 60
  pipelining_requests = 2
  enable = 1
```

### 9.7 Sorter Configuration

```
[sorters]
[[sortername]]
  name = "TV sorter"
  order = 0
  min_size = 0         # Bytes; 0 = no minimum
  multipart_label = "" 
  sort_string = ""     # Rename template
  sort_cats = ["tv"]   # Categories this applies to
  sort_type = [1]      # 1=TV, 2=Movie, 4=Date (bitmask)
  is_active = 1
```

### 9.8 Schedule Configuration

```
[schedules]
[[schedulename]]
  enabled = 1
  arguments = ""       # Action arguments
  minute = "*"         # 0-59 or *
  hour = "*"           # 0-23 or *
  dayofweek = "*"      # 1-7 (1=Mon) or *
  action = "speedlimit" # Action name
```

### 9.9 RSS Configuration

```
[rss]
[[feedname]]
  uri = "https://..."
  cat = ""
  pp = ""
  script = ""
  enable = 1
  priority = 0
  
  [[[filtername]]]
    enabled = 1
    title = ""          # Regex match on title
    body = ""           # Regex match on body
    cat = ""
    pp = ""
    script = ""
    priority = 0
    type = "require"    # require, must_not_match, ignore
    size_from = ""      # e.g., "100M"
    size_to = ""
    age = 0             # Max age in days
```

---

## 10. HTTP API

### 10.1 Authentication

- **API key** (`api_key`): Required for most operations. Pass as `?apikey=<key>` or POST field.
- **NZB key** (`nzb_key`): Alternative key for NZB upload only (addfile, addurl).
- **Session cookie**: Web UI uses session-based auth after username/password login.
- **Localhost bypass**: Requests from `127.0.0.1` can be configured to bypass auth (`local_ranges`).

### 10.2 Endpoint

All API calls are `GET` or `POST` to `/api`:
```
GET /api?mode=<mode>&apikey=<key>[&output=json|xml][&params...]
```

Default output format: JSON. Specify `output=xml` for XML.

### 10.3 Response Envelope

Success:
```json
{"status": true, ...mode-specific fields...}
```

Error:
```json
{"status": false, "error": "description"}
```

### 10.4 API Modes Reference

#### Queue / Job Management

| Mode | Parameters | Response | Description |
|------|-----------|----------|-------------|
| `queue` | `start`, `limit`, `search`, `nzo_ids` | Queue object | Get queue; no sub-action |
| `queue` + `name=delete` | `value=nzo_id[,...]` | status | Delete job(s) |
| `queue` + `name=delete_nzf` | `value=nzo_id`, `value2=nzf_id` | status | Delete file from job |
| `queue` + `name=rename` | `value=nzo_id`, `value2=name`, `value3=password` | status | Rename job |
| `queue` + `name=pause` | `value=nzo_id` | status | Pause specific job |
| `queue` + `name=resume` | `value=nzo_id` | status | Resume specific job |
| `queue` + `name=priority` | `value=nzo_id`, `value2=priority` | priority int | Change priority |
| `queue` + `name=sort` | `value=column`, `dir=asc/desc` | status | Sort queue |
| `queue` + `name=change_complete_action` | `value=action` | status | Post-queue action |
| `queue` + `name=purge` | | status | Empty queue |
| `addfile` | multipart: `nzbfile`, `cat`, `script`, `priority`, `pp`, `nzbname` | nzo_ids | Upload NZB |
| `addlocalfile` | `name=filepath`, `cat`, `script`, `priority`, `pp`, `nzbname` | nzo_ids | Add local NZB |
| `addurl` | `name=url`, `cat`, `script`, `priority`, `pp`, `nzbname` | nzo_ids | Add NZB URL |
| `switch` | `value=nzo_id`, `value2=position` | result, priority | Move job in queue |
| `change_cat` | `value=nzo_id`, `value2=category` | status | Change category |
| `change_script` | `value=nzo_id`, `value2=script` | status | Change script |
| `change_opts` | `value=nzo_id`, `value2=pp_flags` | status | Change PP flags |
| `get_files` | `value=nzo_id` | files list | Get files in job |
| `move_nzf_bulk` | `value=nzo_id`, `nzf_ids` | status | Move files between jobs |
| `retry` | `value=nzo_id`, `password` | status | Retry failed job |
| `retry_all` | | status | Retry all failed |
| `cancel_pp` | `value=nzo_id` | status | Cancel post-processing |

#### Downloader Control

| Mode | Parameters | Response | Description |
|------|-----------|----------|-------------|
| `pause` | | status | Pause downloader |
| `resume` | | status | Resume downloader |
| `pause_pp` | | status | Pause post-processor |
| `resume_pp` | | status | Resume post-processor |
| `disconnect` | | status | Force disconnect all NNTP |
| `speedlimit` | `value=bytes_or_perc` | status | Set speed limit |

#### Status and Information

| Mode | Parameters | Response | Description |
|------|-----------|----------|-------------|
| `fullstatus` | `skip_dashboard` | Full status JSON | All queue, server, stats |
| `version` | | version string | SABnzbd version |
| `auth` | | auth type | Validate API key |
| `warnings` | | warnings list | Recent warnings |
| `showlog` | | streaming text | Tail the log |
| `get_cats` | | categories list | All category names |
| `get_scripts` | | scripts list | All scripts in script_dir |
| `server_stats` | | per-server stats | Download counts per server |
| `gc_stats` | | memory stats | Garbage collector info |
| `status` + `name=unblock_server` | `value=server_id` | status | Re-enable blocked server |
| `status` + `name=delete_orphan` | `value=path` | status | Delete orphaned files |
| `status` + `name=add_orphan` | `value=path` | status | Re-queue orphaned job |

#### History

| Mode | Parameters | Response | Description |
|------|-----------|----------|-------------|
| `history` | `start`, `limit`, `search`, `category`, `nzo_ids`, `failed_only` | history list | Get history entries |
| `history` + `name=delete` | `value=nzo_id[,...]\|failed` | status | Delete history entries |
| `history` + `name=mark_as_completed` | `value=nzo_id` | status | Mark failed as completed |

#### Configuration

| Mode | Parameters | Response | Description |
|------|-----------|----------|-------------|
| `get_config` | `section`, `keyword` | config value | Get one setting |
| `set_config` | `section`, `keyword`, `value` | config value | Set one setting |
| `set_config_default` | `keyword` | status | Reset to default |
| `config` + `name=test_server` | server params | connection result | Test NNTP server |
| `config` + `name=speedlimit` | `value` | status | Speed limit |
| `config` + `name=set_apikey` | | new_apikey | Regenerate API key |
| `config` + `name=set_nzbkey` | | new_nzbkey | Regenerate NZB key |
| `config` + `name=regenerate_certs` | | status | Create HTTPS certs |
| `config` + `name=create_backup` | | backup path | Backup config |
| `config` + `name=purge_log_files` | | status | Delete log files |

#### Miscellaneous

| Mode | Parameters | Response | Description |
|------|-----------|----------|-------------|
| `watched_now` | | status | Trigger watched folder scan |
| `rss_now` | | status | Trigger RSS scan |
| `reset_quota` | | status | Reset bandwidth quota |
| `restart` | | status | Restart SABnzbd |
| `restart_repair` | | status | Restart with queue repair |
| `shutdown` | | status | Shut down |
| `browse` | `name=path` | file list | Browse filesystem |
| `eval_sort` | `job`, `sort_string`, `multipart_label` | result path | Test sorter |
| `test_email` | | status | Send test email |
| `test_notif` | `service` | status | Test notification service |

### 10.5 Queue Response Schema

```json
{
  "queue": {
    "version": 1,
    "paused": false,
    "pause_int": "0",
    "paused_all": false,
    "diskspace1": "50.00",
    "diskspace2": "50.00",
    "diskspace1_norm": "50.0 G",
    "diskspace2_norm": "50.0 G",
    "diskspacetotal1": "500.00",
    "diskspacetotal2": "500.00",
    "loadavg": "1.2",
    "speedlimit": "0",
    "speedlimit_abs": "",
    "have_warnings": "0",
    "finishaction": null,
    "quota": "0",
    "have_quota": false,
    "left_quota": "0",
    "cache_art": "0",
    "cache_size": "0 B",
    "cache_max": "500 M",
    "kbpersec": "5000.0",
    "speed": "5.0 MB/s",
    "mbleft": "1234.5",
    "mb": "5678.9",
    "sizeleft": "1.2 GB",
    "size": "5.5 GB",
    "noofslots_total": 5,
    "noofslots": 5,
    "start": 0,
    "limit": 20,
    "finish": 120,
    "slots": [
      {
        "status": "Downloading",
        "index": 0,
        "password": "",
        "avg_age": "2h",
        "script": "None",
        "has_rating": false,
        "mb": "1234.5",
        "mbleft": "678.9",
        "mbmissing": "0.0",
        "size": "1.2 GB",
        "sizeleft": "678.9 MB",
        "filename": "My.Show.S01E01",
        "priority": "Normal",
        "cat": "tv",
        "eta": "0:05:00",
        "timeleft": "0:05:00",
        "percentage": "45",
        "nzo_id": "SABnzbd_nzo_abc123",
        "unpackopts": "7",
        "labels": []
      }
    ]
  }
}
```

---

## 11. History Database

### 11.1 Backend

SQLite. File: `history1.db` in admin directory.

### 11.2 Schema

```sql
CREATE TABLE history (
    id              INTEGER PRIMARY KEY,
    completed       INTEGER,          -- Unix timestamp of completion
    name            TEXT,             -- Display name
    nzb_name        TEXT,             -- Original NZB filename
    category        TEXT,
    pp              TEXT,             -- PP flags string ("7", "3", etc.)
    script          TEXT,
    report          TEXT,             -- Internal status/report string
    url             TEXT,             -- Source URL
    status          TEXT,             -- Final status (Completed, Failed, etc.)
    nzo_id          TEXT UNIQUE,
    storage         TEXT,             -- Filename/archive name for display
    path            TEXT,             -- Final directory path
    script_log      BLOB,             -- Compressed script stdout (gzip)
    script_line     TEXT,             -- Last non-empty script output line
    download_time   INTEGER,          -- Seconds to download
    postproc_time   INTEGER,          -- Seconds for post-processing
    stage_log       TEXT,             -- JSON: {stage_name: [log_lines]}
    downloaded      INTEGER,          -- Bytes actually downloaded
    completeness    INTEGER,          -- 0-100 percentage
    fail_message    TEXT,
    url_info        TEXT,             -- Additional source info
    bytes           INTEGER,          -- Total NZB size in bytes
    meta            TEXT,             -- JSON metadata dict
    series          TEXT,             -- (deprecated; keep for migration)
    md5sum          TEXT,             -- MD5 of first 16 KB of first file
    password        TEXT,
    duplicate_key   TEXT,
    archive         INTEGER DEFAULT 0, -- 0=active, 1=archived
    time_added      INTEGER           -- Unix timestamp when NZB added
);

CREATE UNIQUE INDEX idx_history_nzo_id ON history(nzo_id);
CREATE INDEX idx_history_archive_completed ON history(archive, completed DESC);
```

### 11.3 History Entry Lifecycle

1. Created in PostProcessor on job start
2. Updated at each stage (stage_log accumulated)
3. Finalized with `status`, `completed` timestamp, `path` on success or failure
4. `script_log` compressed with gzip and stored as BLOB

### 11.4 Pruning

- History auto-purge: configurable `history_retention` (days; 0 = keep forever)
- Failed jobs: separately configurable `history_failed_retention`
- VACUUM run on startup to reclaim space

---

## 12. RSS Feed Processor

### 12.1 Feed Parsing

Uses `feedparser` library. Processes these fields per entry:
- **Link**: Prefer enclosures with `type=application/x-nzb`; fall back to standard link
- **Title**: Raw title string
- **Size**: Extracted via regex from `<description>` or newznab attributes
- **Date**: `newznab:usenetdate_parsed` > `published_parsed`
- **Category**: `cattext` attribute > `category` field > newznab category tag

### 12.2 Filter Rules (per feed)

Each feed has ordered filter rules. Rules are evaluated top-to-bottom; first match wins:

| Rule type | Behavior |
|-----------|----------|
| `require` | Accept if title matches regex |
| `must_not_match` | Accept only if title does NOT match regex |
| `ignore` | Reject if title matches regex |
| (no match) | Apply feed default settings |

Additional filter criteria: `size_from`, `size_to` (file size bounds), `age` (max days old).

### 12.3 Deduplication

- Each entry has a unique key based on normalized title + feed URI
- Processed keys stored in `rss_data.sab` (pickle; rewrite as JSON)
- Entries with duplicate keys are skipped
- `rss_ent` count per feed tracked; entries older than limit cleaned

### 12.4 Scan Interval

- Default: **60 minutes** (`cfg.rss_rate`)
- Triggered by scheduler or API (`rss_now` mode)
- Per-feed: enable/disable independently

---

## 13. Directory Scanner

### 13.1 Purpose

Watches a configured directory for NZB files and automatically adds them to the queue.

### 13.2 Accepted Formats

| Extension | Handling |
|-----------|---------|
| `.nzb` | Direct add |
| `.nzb.gz` | Decompress, then add |
| `.nzb.bz2` | Decompress, then add |
| `.zip` | Extract NZB from archive, then add |

### 13.3 Deduplication State

Per file, tracked in `watched_data2.sab`:
- `inode`: File inode number
- `size`: File size in bytes
- `mtime`: Modification timestamp
- `ctime`: Status change timestamp

A file is "stable" (ready to process) when its size, mtime, and ctime haven't changed between two consecutive scans.

### 13.4 Category/Priority Inference

If the watched folder has subdirectories matching category names (case-insensitive), files placed in those subdirectories inherit that category's settings.

### 13.5 Scan Interval

Default: **5 seconds** (`dirscan_speed`). Uses asyncio for non-blocking directory traversal.

---

## 14. Scheduler

### 14.1 Supported Actions

| Action | Arguments | Description |
|--------|-----------|-------------|
| `resume` | | Resume downloader |
| `pause` | | Pause downloader |
| `pause_all` | | Pause all (download + post-proc) |
| `shutdown` | | Shut down the program |
| `restart` | | Restart the program |
| `pause_post` | | Pause post-processor |
| `resume_post` | | Resume post-processor |
| `speedlimit` | `<value>` | Set speed limit (bytes or %) |
| `enable_server` | `<server_id>` | Re-enable a disabled server |
| `disable_server` | `<server_id>` | Disable a server |
| `scan_folder` | | Trigger watched folder scan |
| `rss_scan` | | Trigger RSS scan |
| `create_backup` | | Backup config to zip |
| `remove_failed` | | Delete failed history entries |
| `remove_completed` | | Delete completed history entries |
| `enable_quota` | | Enable bandwidth quota |
| `disable_quota` | | Disable bandwidth quota |

### 14.2 Schedule Format

Each schedule entry has:
- `enabled`: bool
- `minute`: `0-59` or `*`
- `hour`: `0-23` or `*`
- `dayofweek`: `1-7` (Monday=1) or `*`
- `action`: action name (see table above)
- `arguments`: string (action-specific)

Schedules run via a cron-like mechanism. Evaluation is minute-granularity.

### 14.3 Server Resume Planning

After a server penalty, the scheduler posts a single-fire event at `now + penalty_minutes` to call `plan_server(server_id, 0)` — clearing the penalty and re-enabling the server.

---

## 15. Notifications

### 15.1 Notification Events

| Event key | Trigger |
|-----------|---------|
| `startup` | Program start or shutdown |
| `pause_resume` | Downloader paused or resumed |
| `download` | NZB added to queue |
| `pp` | Post-processing started for a job |
| `complete` | Job completed successfully |
| `failed` | Job failed post-processing |
| `warning` | Warning logged |
| `error` | Error logged |
| `disk_full` | Disk space too low |
| `quota` | Bandwidth quota reached |
| `queue_done` | Queue empty |
| `new_login` | New web UI login |
| `other` | Miscellaneous |

Each notification service can be configured to receive any subset of event types.

### 15.2 Notification Services

| Service | Platform | Config Section |
|---------|----------|---------------|
| Windows Toast | Windows | `ntpr_wintray` |
| D-Bus (notify2) | Linux | `ntpr_dbus` |
| Apprise | All | `ntpr_apprise` (URL-based config) |
| Pushover | All | `ntpr_pushover` |
| Prowl | iOS | `ntpr_prowl` |
| Pushbullet | All | `ntpr_pushbullet` |
| Custom script | All | `ntpr_script` |
| Email | All | `email_*` settings |

**Apprise**: Single URL encodes service + credentials (e.g., `discord://token/channel_id`). Supports 100+ services. Go equivalent: use an Apprise HTTP gateway or individual service clients.

### 15.3 Email Notifications

- **SMTP**: plain or STARTTLS (`cfg.email_server`, port 25 or 587)
- **SMTP_SSL**: TLS from connect (port 465)
- **Auth**: Username + password (optional)
- **Templates**: Per-notification-type (uses Cheetah; rewrite as Go templates)
- **Events**: Completion, failure, warnings, disk full, quota

---

## 16. Bandwidth Metering and Quotas

### 16.1 Real-Time Speed

- Current BPS computed as rolling average of recent N-second window
- Displayed in queue status as `kbpersec` and formatted `speed`

### 16.2 Speed Limiting

- `bandwidth_max`: Absolute ceiling in bytes/sec (or `"10M"` notation: k/m/g suffixes)
- `bandwidth_perc`: Percentage of `bandwidth_max` to actually use (1-100)
- Effective limit: `bandwidth_max * bandwidth_perc / 100`
- Applied by throttling the rate at which articles are dispatched to connections
- Scheduler can change `speedlimit` at scheduled times

### 16.3 Bandwidth Quota

| Setting | Description |
|---------|-------------|
| `quota_size` | Total quota (bytes, with k/m/g suffix) |
| `quota_period` | Period: `d` (day), `w` (week), `m` (month) |
| `quota_day` | Day of week/month quota resets (0=Monday for week; 1-28 for month) |
| `quota_resume_time` | Time of day quota resets (HH:MM) |

When quota is reached:
1. Downloader paused
2. Notification sent (`quota` event)
3. Auto-resume at quota reset time via scheduler

### 16.4 Statistics Tracking

Per-server cumulative download stats stored in `bpsmeter.sab`:
- `bytes_today`, `bytes_this_week`, `bytes_this_month`, `bytes_total`
- Broken down per server by server ID

---

## 17. Data Formats and Types

### 17.1 NZB XML Format

NZB files conform to the NZB specification (DTD at `http://www.newzbin.com/DTD/nzb/`):

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE nzb PUBLIC "-//newzBin//DTD NZB 1.1//EN"
  "http://www.newzbin.com/DTD/nzb/nzb-1.1.dtd">
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <head>
    <meta type="title">My Show S01E01</meta>
    <meta type="password">secret</meta>
    <meta type="category">TV</meta>
    <meta type="tag">HDTV</meta>
  </head>
  <file poster="poster@example.com" date="1234567890"
        subject="My.Show.S01E01.HDTV.x264 [1/25] yEnc (1/50)">
    <groups>
      <group>alt.binaries.hdtv</group>
    </groups>
    <segments>
      <segment bytes="760000" number="1">
        unique-article-id@news.example.com
      </segment>
      ...
    </segments>
  </file>
  ...
</nzb>
```

Key parsing:
- `<meta type="password">`: Job password for RAR extraction
- `<meta type="category">`: Override category
- `<file subject>`: Parse part number `[X/Y]` and segment info
- `<segment number>` + `bytes`: Segment ordering and size
- `<group>`: Newsgroup(s) to fetch from

### 17.2 Internal ID Format

Job IDs: `SABnzbd_nzo_<8_alphanumeric_chars>` (e.g., `SABnzbd_nzo_a1b2c3d4`)
File IDs: `SABnzbd_nzf_<8_alphanumeric_chars>`

### 17.3 Size Notation

Human-readable sizes use binary prefixes (IEC): B, KB (1024), MB, GB, TB.
API inputs accept: `"500M"`, `"2G"`, `"1024K"` (case-insensitive).

### 17.4 Admin Files (State Persistence)

| File | Contents | Format |
|------|----------|--------|
| `queue10.sab` | Download queue | Python pickle + gzip → replace with JSON |
| `postproc2.sab` | Post-processing queue | Python pickle + gzip → replace with JSON |
| `rss_data.sab` | RSS processed entries | Python pickle + gzip → replace with JSON |
| `watched_data2.sab` | Dir scanner state | Python pickle + gzip → replace with JSON |
| `bpsmeter.sab` | Bandwidth statistics | Python pickle + gzip → replace with JSON |
| `history1.db` | Completed job history | SQLite → keep as-is |
| `sabnzbd.ini` | Configuration | INI (configobj) → keep format or migrate to TOML |

---

## 18. Security

### 18.1 API Authentication

- **API key**: 16-character random alphanumeric string, generated on first run
- **NZB key**: Separate key allowing NZB uploads without full API access
- Keys stored in `sabnzbd.ini` in plaintext (protect file permissions)
- Regeneration: POST to `api?mode=config&name=set_apikey`

### 18.2 Web UI Authentication

- **Username + password**: Stored as bcrypt hash (or plaintext legacy)
- **Session cookie**: Set after successful login; HTTP-only
- **HTTPS**: Self-signed cert generated via `certgen.py`; `cryptography` library used

### 18.3 NNTP Password Handling

- Stored in config as plaintext (protected by file permissions)
- NOT transmitted in log output (masked as `****`)

### 18.4 Access Control

- **Local ranges**: Requests from `127.0.0.1` and RFC 1918 ranges can be granted UI access without credentials (configurable)
- **X-Forwarded-For**: Considered when `cfg.verify_host = true` (reverse proxy support)
- **Config lock**: Prevent configuration changes without a separate PIN

### 18.5 Certificate Generation

Self-signed TLS certificates generated via `cryptography` library:
- 4096-bit RSA key
- `CN=sabnzbd`
- `subjectAltName=IP:127.0.0.1,DNS:localhost`
- Valid for 5 years
- Stored in admin directory

---

## 19. Suggested Agents for Spec Refinement

The following specialized agents or skills would produce a more accurate and complete specification for Go reimplementation:

### 19.1 Protocol Verification Agent

**Purpose**: Cross-reference the NNTP implementation against RFC 3977 (NNTP), RFC 4643 (AUTHINFO), and RFC 5536 (Article Format) to identify:
- Any RFC-compliant behaviors that the spec above missed
- Non-standard extensions (XOVER, HDR, etc.) that some servers support
- Edge cases in pipelining that the Go implementation must handle

**Skill to use**: `general-purpose` agent with web search + code grep  
**Query**: "Compare sabnzbd/newswrapper.py NNTP handling against RFC 3977 section by section"

### 19.2 Test Behavior Extractor Agent

**Purpose**: Read all tests in `tests/` to extract behavioral requirements that aren't obvious from the production code. Tests encode edge cases, known bugs, and regression scenarios.

**Files to focus on**: `test_newswrapper.py`, `test_nzbqueue.py`, `test_decoder.py`, `test_assembler.py`, `test_postproc.py`, `test_sorting.py`, `test_api_and_interface.py`

**Skill to use**: `feature-dev:code-explorer` agent  
**Output**: List of "the code must behave X in situation Y" requirements per subsystem

### 19.3 yEnc Specification Agent

**Purpose**: Produce a complete yEnc format spec including all edge cases (escape sequences, multi-part, CRC handling, malformed data tolerance). The current spec covers the happy path only.

**Skill to use**: `general-purpose` with `WebSearch` for the yEnc specification document + code analysis of `sabnzbd/decoder.py`

### 19.4 API Surface Completeness Agent

**Purpose**: Systematically enumerate every API parameter, response field, and edge case by:
1. Reading `sabnzbd/api.py` completely (it's large)
2. Reading the Glitter UI JavaScript (`interfaces/Glitter/static/`) to see how the UI calls the API — the UI reveals response fields the API spec doesn't document

**Skill to use**: `feature-dev:code-explorer`  
**Output**: Complete API reference with all request parameters and response field schemas

### 19.5 Integration Test Scenario Agent

**Purpose**: The functional tests (`test_functional_*.py`) exercise end-to-end scenarios including downloads, post-processing, sorting, and API calls. Extract all test scenarios as requirements — these represent the "must work" acceptance criteria for the reimplementation.

**Skill to use**: `pr-review-toolkit:pr-test-analyzer`  
**Framing**: "Treat this as if the functional tests are the acceptance tests for a Go reimplementation. Document every scenario as a requirement."

### 19.6 Performance Requirements Agent

**Purpose**: The Python code has several explicit performance choices (256 KB buffers, 2 pipelined requests, 500 MB cache, 12-item assembler queue). A Go reimplementation may differ, but the agent should:
- Extract all explicit tuning constants with their rationale
- Profile which subsystems are throughput-critical vs. latency-sensitive
- Recommend Go concurrency patterns for each

**Skill to use**: `python-development:python-performance-optimization` + `golang-performance`

### 19.7 sabctools Interface Agent

**Purpose**: `sabctools` is a C++ extension that SABnzbd relies on for yEnc decoding, CRC32, and SSL buffer handling. The Go reimplementation must match its behavior exactly. This agent should:
- Locate the `sabctools` source (likely at `github.com/sabnzbd/sabctools`)
- Document every function's inputs, outputs, and error behavior
- Flag any behavior that's non-obvious from Python (e.g., how it handles partial yEnc lines, malformed CRCs)

**Skill to use**: `general-purpose` with GitHub access + `WebFetch`

### 19.8 Sorting / guessit Behavior Agent

**Purpose**: `guessit` is a Python library that parses media filenames into structured metadata (title, season, episode, year, quality, etc.). The sorting subsystem depends heavily on it. The Go reimplementation needs either:
- A Go equivalent library (e.g., `github.com/nicholasgasior/gsfmt` or custom)
- A subprocess call to guessit
- A reimplementation of the relevant parsing rules

**Skill to use**: `general-purpose` with web search for Go media filename parsers + `guessit` documentation

### 19.9 Migration Compatibility Agent

**Purpose**: Real users will migrate from SABnzbd (Python) to the Go reimplementation. This agent should:
- Document exactly what needs to be migrated: queue format, history DB, config INI, RSS state, bpsmeter stats
- Propose migration strategies (converter tool, compatible formats)
- Identify what can be kept as-is (SQLite history) vs. must be converted (pickle files)

**Skill to use**: `python-development:python-design-patterns` + `golang-design-patterns`

---

*Spec generated from SABnzbd source at commit `6ff8aa7ce` (v5.0.0RC3 branch `develop`). All line-number references are approximate; verify against current source.*
