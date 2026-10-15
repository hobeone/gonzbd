---
name: localnntp
description: Use when running local Usenet stress tests, multi-GiB RAR5+PAR2 downloads, fault injection, or SIGKILL crash and restore verification against gonzbd without touching real Usenet
---

# `localnntp`: Local Usenet Stress, Fault Injection & Crash Verification Harness

## 1. Overview & Architecture

`localnntp` is a self-contained Usenet simulation and verification harness for `gonzbd`. It allows agents and engineers to run multi-gigabyte end-to-end downloads, semantic NNTP fault injection, bandwidth shaping, and hard `SIGKILL` crash/restore verifications entirely on local disk without connecting to external Usenet servers or writing mock fixtures by hand.

The tool consists of three integrated subsystems:

1. **Synthetic Release Generator (`gen`)**: Produces deterministic multi-volume RAR5 archives with configurable PAR2 parity files, sidecars (`.nfo`, sample media), segment splitting, yEnc formatting, NZB XML index generation, and pre-computed `articles.pack` binary bundles.
2. **High-Performance NNTP Server (`serve`)**: Serves RFC 3977 / 2980 NNTP commands (`ARTICLE`, `BODY`, `STAT`, `CAPABILITIES`, `AUTHINFO`) with line-rate throughput via the immutable pre-encoded `articles.pack` fast path (1+ GB/s). Houses a dynamic HTTP control API for live fault injection, rate limiting, and snapshot tracking.
3. **Crash & Completion Verifier (`verify`)**: Verifies `gonzbd` database invariants against disk state. For `crash` mode, it validates that every SQLite `written_articles` row matches partial disk bytes and CRC-32 before taking an article snapshot. For `complete` mode, it verifies extracted payloads against SHA-256 digests, ensures temporary directories/artifacts are purged, and asserts zero redundant article re-fetches after daemon restart.

---

## 2. Quick Reference

### CLI Subcommands

| Subcommand | Description | Key Flags |
|---|---|---|
| `gen` | Generates synthetic RAR5+PAR2 releases and NZB XML | `--dir`, `--size`, `--volumes`, `--par2-redundancy`, `--seed` |
| `serve` | Runs disk-backed NNTP server & HTTP control API | `--dir`, `--listen`, `--control`, `--rate-mbps`, `--fault` |
| `verify` | Validates SQLite state against disk & server stats | `--mode` (`crash`\|`complete`), `--manifest`, `--admin-dir`, `--download-dir`, `--complete-dir`, `--server`, `--snapshot-as`, `--snapshot`, `--expect-archive-cleanup` |

### HTTP Control API Endpoints

All endpoints are hosted on the address configured by `--control` (default: `127.0.0.1:11191`):

| Method & Route | Purpose | Payload / Query Parameters |
|---|---|---|
| `GET /stats` | Returns aggregate counters and served article map | Response: `{ "served_clean_total": ..., "served_clean": { "<id>": count } }` |
| `POST /faults` | Installs a dynamic fault injection rule | JSON body: `{"spec": "<spec>"}` or structured rule object |
| `DELETE /faults` | Clears all active fault rules | None |
| `POST /rate` | Dynamically updates bandwidth throttle | JSON body: `{"rate_mbps": 50.0}` (`0` = unlimited) |
| `POST /snapshot` | Takes an immutable snapshot of served clean articles | Query: `?name=<name>`, JSON body: `{"name": "<name>", "recorded_ids": [...]}` |
| `GET /snapshot` | Retrieves an existing snapshot | Query: `?name=<name>` |

---

## 3. Fault Injection Reference

Fault rules match against requested message IDs, target filenames, part numbers, or file roles (`rar_volume`, `par2_index`, `par2_volume`, `sidecar`).

| Fault Action | Behavior & Impact | gonzbd Pipeline Response |
|---|---|---|
| `drop` | Server returns NNTP error `430 No such article` | Downloader charges article failure and promotes `.vol*.par2` recovery volumes inline inside `Job.MarkArticleFailed` |
| `corrupt-payload` | Valid yEnc CRC32 header, but corrupt internal payload bytes | Passes yEnc decoder; `.vol*.par2` remains deferred (`FetchIfNeeded`) until initial download finishes and `Assessing` (`maybeReleaseRecoveryVolumes`) detects the file-level CRC mismatch and triggers a second recovery download wave |
| `corrupt-wire` | Invalid yEnc body or bad CRC32 line | Rejected immediately by `internal/decoder` (`ErrCRCMismatch`); downloader tries alternate servers or marks failed |
| `drop-mid-body` | Server sends half the body then abruptly closes TCP connection | Downloader catches mid-stream EOF / connection reset, drops socket, and applies a server penalty (clear immediately in single-server tests via `GET /api?mode=status&name=unblock_server&value=localnntp`) |
| `stall` | Server sends `222` status line then sleeps for `timeout` before closing | Triggers client socket read timeout (`idleTimeoutReader`) and server penalty |

### Format Specification Syntax

Specs use `<action>:<selector>:<params>` format:
- `drop:part01.rar:parts=1,2:max=1` — Drops each named segment (parts 1 and 2) of `part01.rar` at most once per part (`parts=` and `msgid=` rules enforce `max` per targeted article).
- `corrupt-payload:part03.rar:bytes=100:max=2` — Flips 100 bytes inside the payload of up to 2 segments of `part03.rar` in total (whole-file / role rules without `parts=` or `msgid=` enforce `max` across the rule).
- `corrupt-wire:role=par2_volume:rate=0.05` — Corrupts yEnc framing for 5% of PAR2 volume articles.
- `stall:timeout=10s:msgid=<art123@localnntp.test>` — Delays serving article `art123` by 10 seconds.

---

## 4. End-to-End Agent Playbook: SIGKILL Crash & Restore Verification

This playbook demonstrates how an agent tests `gonzbd` crash consistency under active download workloads without relying on external networks.

### Step 1: Generate a Test Release

```bash
mkdir -p /tmp/fake-usenet/movie
go run ./scripts/localnntp gen \
  --dir /tmp/fake-usenet/movie \
  --name "StressMovie" \
  --size "500M" \
  --volumes 10 \
  --part-size 524288 \
  --par2-redundancy 15 \
  --par2-volumes 4 \
  --seed 42
```

This writes:
- `/tmp/fake-usenet/movie/files/` — The split `.partNN.rar` and `.par2` files.
- `/tmp/fake-usenet/movie/manifest.json` — Release structural manifest and SHA-256 expected outputs.
- `/tmp/fake-usenet/movie/StressMovie.nzb` — Standard NZB XML pointing to synthetic Message-IDs.
- `/tmp/fake-usenet/movie/articles.pack` — Pre-encoded yEnc articles pack file.

### Step 2: Start the Local NNTP Server

```bash
go run ./scripts/localnntp serve \
  --dir /tmp/fake-usenet/movie \
  --listen "127.0.0.1:11190" \
  --control "127.0.0.1:11191" \
  --rate-mbps 100 &
SERVER_PID=$!
```

### Step 3: Configure and Start `gonzbd`

Create a clean temporary environment configuration in `/tmp/gonzbd-crash/gonzbd.yaml`:

```yaml
general:
  host: 127.0.0.1
  port: 18089
  api_key: "0123456789abcdef"
  nzb_key: "fedcba9876543210"
  complete_dir: /tmp/gonzbd-crash/complete
  download_dir: /tmp/gonzbd-crash/incomplete
  admin_dir: /tmp/gonzbd-crash/admin
  log_dir: /tmp/gonzbd-crash
downloads:
  on_demand_par2: true
  no_penalties: true
  max_art_tries: 4
postproc:
  direct_unpack: true
  use_go_par2: true
servers:
  - name: localnntp
    host: 127.0.0.1
    port: 11190
    connections: 8
    enable: true
    timeout: 30
    pipelining_requests: 2
    priority: 0
    retention: 0
```

Start the daemon and submit the NZB:

```bash
./gonzbd --config /tmp/gonzbd-crash/gonzbd.yaml --serve &
GONZBD_PID=$!
sleep 1

# Enqueue the job via SABnzbd compatibility API
curl -sf "http://127.0.0.1:18089/api?mode=addfile&output=json&apikey=0123456789abcdef" \
  -F "nzbfile=@/tmp/fake-usenet/movie/StressMovie.nzb"
```

### Step 4: Deliver `SIGKILL` During Active Download

Monitor `/tmp/gonzbd-crash/admin/history.db` until partial article records appear, then kill the daemon immediately:

```bash
while true; do
  COUNT=$(sqlite3 /tmp/gonzbd-crash/admin/history.db "SELECT count(*) FROM written_articles;" 2>/dev/null || echo 0)
  if [ "$COUNT" -gt 50 ]; then
    kill -9 $GONZBD_PID
    break
  fi
  sleep 0.1
done
```

### Step 5: Run Crash State Verification

Verify that every `written_articles` row in SQLite matches actual bytes on disk, and capture an immutable snapshot on the server:

```bash
go run ./scripts/localnntp verify \
  --mode crash \
  --manifest /tmp/fake-usenet/movie/manifest.json \
  --admin-dir /tmp/gonzbd-crash/admin \
  --download-dir /tmp/gonzbd-crash/incomplete \
  --server "http://127.0.0.1:11191" \
  --snapshot-as "crash1"
```

If SQLite points to bytes or CRC-32 checksums that do not exist on disk, `localnntp verify` exits non-zero and outputs the failing offset and CRC mismatch.

### Step 6: Restart `gonzbd` and Await Completion

Restart the daemon:

```bash
./gonzbd --config /tmp/gonzbd-crash/gonzbd.yaml --serve &
GONZBD_PID=$!
```

Wait until the job reaches `Completed` in `/tmp/gonzbd-crash/admin/history.db`:

```bash
while true; do
  STATUS=$(sqlite3 /tmp/gonzbd-crash/admin/history.db "SELECT status FROM history LIMIT 1;" 2>/dev/null || echo "")
  if [ "$STATUS" = "Completed" ]; then
    break
  fi
  sleep 0.5
done
```

### Step 7: Run Completion & No-Refetch Verification

Verify extracted outputs, disk hygiene, and ensure that articles recorded prior to `SIGKILL` were not re-fetched:

```bash
go run ./scripts/localnntp verify \
  --mode complete \
  --manifest /tmp/fake-usenet/movie/manifest.json \
  --admin-dir /tmp/gonzbd-crash/admin \
  --download-dir /tmp/gonzbd-crash/incomplete \
  --complete-dir /tmp/gonzbd-crash/complete \
  --server "http://127.0.0.1:11191" \
  --snapshot "crash1"
```

---

## 5. Targeted Crash Scenarios

When validating recovery, test across these specific lifecycle checkpoints:

1. **Killing during `Downloading` + `DirectUnpack`**:
   - Injects `POST /rate` to throttle to 10-80 MB/s.
   - Kills during active extraction. Verifies partial unrar temp state does not corrupt resumed download.
2. **Killing during `Assessing` / `on_demand_par2` recovery promotion**:
   - Note that permanent `430` failures (`drop`) promote `.vol*.par2` recovery volumes inline inside `Job.MarkArticleFailed`, whereas `corrupt-payload` (valid yEnc CRC with corrupted payload bytes) keeps `.vol*.par2` deferred (`FetchIfNeeded`) until initial download finishes and `Assessing` (`maybeReleaseRecoveryVolumes`) verifies assembled CRCs against the `.par2` index.
   - Inject `corrupt-payload:part04.rar:parts=2:bytes=64:max=1` and wait for `"on-demand par2: repair needed, fetching recovery volumes"` in `gonzbd.log` to deterministically kill during the second recovery download wave.
3. **Killing during `Repairing` / `Extracting`**:
   - For mid-repair `SIGKILL`: wait for `<target>.par2engine-*.tmp` in the incomplete job directory (written by `par2engine` during out-of-place repair) and `kill -9`. On restart, `sweepTempFiles` (`internal/postproc/postproc.go`) removes names matched by `fsutil.IsTempFile` (`internal/fsutil/rootedcreate.go`), including `.gonzbd-tmp-<16 lowercase hex>` and `<target>.par2engine-<16 uppercase base32>.tmp`, before `recover_par2_names`.
   - For mid-extraction `SIGKILL`: wait for `.gonzbd-tmp-unpack-*/.gonzbd-tmp-*` in the complete job directory (while `go_unrar`'s `writeEntrySafely` streams an entry before renaming it to the final member name) and `kill -9`. On restart, `sweepTempFiles` removes `.gonzbd-tmp-unpack-*` and re-extracts cleanly.
4. **Killing right after final directory move**:
   - Kills when job is inserted into `history` table.
   - Verifies job is atomically absent from `dispatch_jobs`.

---

## 6. Common Mistakes & Red Flags

| Red Flag | Root Cause & Correction |
|---|---|
| Re-fetched article error during `verify --mode complete` | Resumed job re-downloaded articles already committed to SQLite before crash. Check `readRecord` hydration in `internal/app`. |
| CRC32 mismatch during `verify --mode crash` | Article row was committed to `written_articles` before decoded bytes were flushed/synced to disk. Violates durability contract. |
| Incomplete directory leftover violation | The job completed successfully, but old `.part` files or empty folders remain in `download_dir`. Check job finalizer cleanup. |
| Parity or RAR file found in complete directory | Archive or recovery files were moved into `complete_dir` alongside extracted media. Check cleanup flags in config. |
