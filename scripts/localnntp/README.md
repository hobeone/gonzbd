# `localnntp` — Local Usenet Stress, Fault Injection & Crash Verification

`localnntp` is an integrated Usenet simulation, semantic fault injection, and crash verification tool designed for local stress testing and crash consistency verification of `gonzbd`.

## Capabilities

- **Synthetic Release Generation**: Creates multi-volume RAR5 archives with configurable PAR2 recovery sets, sidecar files (NFO, samples), split segments, and formatted NZB XML.
- **Fast Line-Rate Server**: Serves NNTP `ARTICLE`, `BODY`, `STAT`, `CAPABILITIES`, and `AUTHINFO` commands using an immutable pre-encoded `articles.pack` fast path exceeding 1 GB/s.
- **Semantic Fault Engine**: Injects article drops, wire corruption, payload corruption (invalid content with valid yEnc CRC to trigger DirectUnpack / quickcheck repair), mid-body disconnects, and stalls.
- **Multi-Release Serving**: `serve --dir` is repeatable, so several releases share one server and one Message-ID index.
- **Connection-Level Faults**: Connection caps (`502`), dead servers, `400` greetings, auth failures and abrupt kills of open connections, switchable at runtime.
- **Dynamic HTTP Control API**: Provides live endpoints for updating bandwidth rate limits, adding/clearing fault rules, setting connection faults, and recording point-in-time article snapshots.
- **State Waiter (`wait`)**: Blocks until a SQLite scalar, a log line or a file path satisfies a condition, replacing shell polling loops.
- **Crash & Completion Verifier**: Verifies `gonzbd` SQLite state (`written_articles`, `job_files`, `dispatch_jobs`, `history`) against partial and complete files on disk (including a file-scoped post-processing fallback reported via `fallback_rows` when a `complete=1` file was renamed or backed up to `.1` during mid-post-processing crashes), ensuring zero re-fetches after crash recovery.

---

## Usage

### 1. Generating a Release (`gen`)

```bash
go run ./scripts/localnntp gen \
  --dir /tmp/fake-usenet/movie \
  --name "StressMovie" \
  --size "500M" \
  --volumes 10 \
  --part-size 524288 \
  --par2-redundancy 10 \
  --seed 1
```

Flags:
- `--dir`: Output directory for generated files (required).
- `--name`: Name of the release (default: `"release"`).
- `--size`: Payload size (e.g. `100M`, `1G`, `500MB`).
- `--volumes`: Number of split RAR volumes (default: `10`).
- `--part-size`: Size of NNTP article segments in bytes (default: `524288`).
- `--par2-redundancy`: PAR2 redundancy percentage (default: `10`).
- `--par2-volumes`: Number of PAR2 volume files (default: `4`).
- `--seed`: RNG seed for deterministic generation (default: `1`).
- `--compress`: Compress payload with RAR (default: `false`).
- `--obfuscate`: Obfuscate volume filenames in the NZB (default: `false`).
- `--sidecars`: Include sample media and NFO sidecars (default: `true`).

Message-IDs are derived from the namespace `<name>/<seed>` (the `--name` and `--seed` values) plus the file name and part number. Releases generated with a different `--name` or `--seed` therefore never share a Message-ID, even though each contains a `sample.mkv`, which is what lets `serve` index several of them together.

---

### 2. Running the Server (`serve`)

```bash
go run ./scripts/localnntp serve \
  --dir /tmp/fake-usenet/movie \
  --listen "127.0.0.1:11190" \
  --control "127.0.0.1:11191" \
  --rate-mbps 50 \
  --fault "drop:part01.rar:parts=1,2" \
  --fault "corrupt-payload:part02.rar:bytes=256"
```

Flags:
- `--dir`: Release directory containing `manifest.json` (required, **repeatable**). Each directory keeps its own `articles.pack` and `files/`; all releases share one Message-ID index, and startup fails if a Message-ID appears in two releases. Fault rules, stats and snapshots span every release. Startup also fails for a release without a `files/` directory, and for an `articles.pack` that cannot be opened, is not a regular file, or is shorter than the manifest's wire offsets need; only a missing `articles.pack` is tolerated, and that release is then served from `files/` without the fast path.
- `--listen`: Address to bind the NNTP server (default: `"127.0.0.1:11190"`).
- `--control`: Address to bind the HTTP control API (default: `"127.0.0.1:11191"`).
- `--rate-mbps`: Bandwidth limit in MB/s (`0` = unlimited).
- `--seed`: RNG seed for fault engine (default: `1`).
- `--fault`: Repeatable fault specification string.
- `--max-connections`: Cap on simultaneous client connections; a connection over the cap gets `502 too many connections` and is closed (`0` = unlimited; a negative value is an error).
- `--refuse`: Accept then close immediately with no greeting (a dead server).
- `--reject-greeting`: Greet with `400 service temporarily unavailable` and close.
- `--auth-fail`: `AUTHINFO PASS` answers `481` whatever the credentials, and `BODY`/`ARTICLE`/`STAT` answer `480` on every connection while it is set, including connections that authenticated before it was set.

The four connection flags are the initial state of the runtime-adjustable connection faults below.

#### Connection faults at runtime

`POST /conn` on the control address changes them live. Omitted fields keep their current value; unknown fields are rejected with `400`.

```bash
curl -s -X POST http://127.0.0.1:11191/conn -d '{"max_connections": 2, "auth_fail": false}'
curl -s -X POST http://127.0.0.1:11191/conn -d '{"kill_active": true}'   # one-shot: closes every open client connection
```

JSON fields: `max_connections` (int), `refuse` (bool), `reject_greeting` (bool), `auth_fail` (bool), `kill_active` (bool, an action rather than state). A negative `max_connections` is rejected with `400`. The response is `{"conn": {...}, "killed": N}`. Killed connections leave the open set before the response is sent, so a reconnect straight after is not counted against `max_connections`.

`GET /stats` reports the current state as `conn` (`max_connections`, `refuse`, `reject_greeting`, `auth_fail`), plus `active_connections`, `peak_connections` and `connections_rejected_total` (refused, rejected-greeting and over-cap connections).

---

### 3. Verifying Recovery (`verify`)

#### Crash Mode

Checks that every article in `written_articles` has matching bytes and CRC-32 on disk, and records an article snapshot via the HTTP control API:

```bash
go run ./scripts/localnntp verify \
  --mode crash \
  --manifest /tmp/fake-usenet/movie/manifest.json \
  --admin-dir /tmp/gonzbd-test/admin \
  --download-dir /tmp/gonzbd-test/incomplete \
  --server "http://127.0.0.1:11191" \
  --snapshot-as "crash1"
```

#### Complete Mode

Validates that expected outputs match their target SHA-256 hashes, verifies cleanup hygiene, and ensures no previously recorded articles were re-fetched:

```bash
go run ./scripts/localnntp verify \
  --mode complete \
  --manifest /tmp/fake-usenet/movie/manifest.json \
  --admin-dir /tmp/gonzbd-test/admin \
  --download-dir /tmp/gonzbd-test/incomplete \
  --complete-dir /tmp/gonzbd-test/complete \
  --server "http://127.0.0.1:11191" \
  --snapshot "crash1" \
  --expect-archive-cleanup
```

Flags:
- `--mode`: Verification mode (`crash` or `complete`, required).
- `--manifest`: Path to `manifest.json` (required).
- `--admin-dir`: Path to `gonzbd` admin directory (required).
- `--download-dir`: Path to `gonzbd` incomplete download directory (required).
- `--complete-dir`: Path to `gonzbd` complete directory (required for `complete` mode).
- `--job-id`: Explicit job ID when multiple jobs exist in SQLite (optional).
- `--server`: HTTP control URL of `localnntp serve` (required when `--snapshot-as` or `--snapshot` is set).
- `--snapshot-as`: Snapshot name to record on the server in `crash` mode.
- `--snapshot`: Snapshot name to compare against in `complete` mode.
- `--expect-archive-cleanup`: Expect `.rar` and `.par2` files to be cleaned up from `complete_dir` (default: `true`).
- `--servers-for-fetch-check`: Repeatable control URL (`complete` mode only; the same URL twice is an error, since it would count one server twice). Fetches `/stats` from each, sums `served_clean` per Message-ID, and adds `served_clean_sum`, `distinct_ids_served` and `multi_served_ids` (formatted `<id> xN`) to the report. The report carries those fields only when this flag is given.
- `--expect-single-fetch`: With `--servers-for-fetch-check`, fail if any Message-ID was served clean more than once across all those servers, if nothing was served clean at all, or if a non-par2 segment of the manifest was never served clean (par2 volumes may be served without being needed, and are not required). Because every non-par2 segment must be served clean, do not combine it with `drop` or `corrupt-*` faults on those segments. Independent of `--snapshot`.

---

### 4. Waiting for State (`wait`)

Polls one condition and exits `0` when it holds (printing the matching value or path on stdout), `1` on timeout (printing the last observed value on stderr), `2` on a usage error or a SQL statement that can never succeed. Exactly one condition family per call; a flag that belongs to another family is a usage error.

```bash
# SQLite scalar, opened read-only; a missing db, a table not created yet, a busy or locked
# database and a query with no row yet are "not yet"; a syntax error or unknown column exits 2
go run ./scripts/localnntp wait --timeout 10m --db /tmp/gonzbd/admin/history.db \
  --sql "SELECT count(*) FROM written_articles" --ge 50
go run ./scripts/localnntp wait --db /tmp/gonzbd/admin/history.db --sql "SELECT status FROM history LIMIT 1" --eq Completed

# A log line matching a regex (read incrementally; the file may not exist yet)
go run ./scripts/localnntp wait --log /tmp/gonzbd/gonzbd.log --regex 'repair needed, fetching recovery volumes'

# A path appearing
go run ./scripts/localnntp wait --glob '/tmp/gonzbd/complete/*/*.mkv'
go run ./scripts/localnntp wait --glob-root /tmp/gonzbd/incomplete --glob-name '*.par2engine-*.tmp'
```

Flags:
- `--timeout` (default `10m`), `--interval` (default `200ms`).
- `--db` with `--sql` (a query returning one scalar) and exactly one of `--ge N` / `--gt N` (a finite number; `NaN` and `Inf` are rejected) or `--eq VALUE` (string compare). A SQL `NULL` never satisfies any of them, including `--eq ""`; it is reported as `(NULL)`.
- `--log` with `--regex`, and optionally `--from-end`: ignore lines already in the file when `wait` starts, so a previous run's output cannot satisfy the wait. A log that does not exist yet is read from its first byte.
- `--glob` (a `filepath.Glob` pattern; `**` is not supported) or `--glob-root` with `--glob-name` (a `filepath.Match` pattern against the base name, searched recursively).
