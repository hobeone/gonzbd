# `localnntp` — Local Usenet Stress, Fault Injection & Crash Verification

`localnntp` is an integrated Usenet simulation, semantic fault injection, and crash verification tool designed for local stress testing and crash consistency verification of `gonzbd`.

## Capabilities

- **Synthetic Release Generation**: Creates multi-volume RAR5 archives with configurable PAR2 recovery sets, sidecar files (NFO, samples), split segments, and formatted NZB XML.
- **Fast Line-Rate Server**: Serves NNTP `ARTICLE`, `BODY`, `STAT`, `CAPABILITIES`, and `AUTHINFO` commands using an immutable pre-encoded `articles.pack` fast path exceeding 1 GB/s.
- **Semantic Fault Engine**: Injects article drops, wire corruption, payload corruption (invalid content with valid yEnc CRC to trigger DirectUnpack / quickcheck repair), mid-body disconnects, and stalls.
- **Dynamic HTTP Control API**: Provides live endpoints for updating bandwidth rate limits, adding/clearing fault rules, and recording point-in-time article snapshots.
- **Crash & Completion Verifier**: Verifies `gonzbd` SQLite state (`written_articles`, `job_files`, `dispatch_jobs`, `history`) against partial and complete files on disk, ensuring zero re-fetches after crash recovery.

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
- `--dir`: Release directory containing `manifest.json` (required).
- `--listen`: Address to bind the NNTP server (default: `"127.0.0.1:11190"`).
- `--control`: Address to bind the HTTP control API (default: `"127.0.0.1:11191"`).
- `--rate-mbps`: Bandwidth limit in MB/s (`0` = unlimited).
- `--seed`: RNG seed for fault engine (default: `1`).
- `--fault`: Repeatable fault specification string.

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
  --snapshot "crash1"
```
