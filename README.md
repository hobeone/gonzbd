# GoNZBD

A high-performance Usenet binary newsreader written in Go, heavily inspired
by [SABnzbd](https://sabnzbd.org). GoNZBD targets fresh installations — it
is not a drop-in replacement for an existing Python SABnzbd install.

## Features

- **Full download pipeline** — NZB parsing, NNTP article fetching with
  multi-server failover, yEnc decoding, and file assembly.
- **Post-processing** — par2 verify/repair, pure-Go RAR extraction (no
  external binary required), 7z/zip extraction, DirectUnpack (streaming
  RAR extraction during download), file deobfuscation, sorting rules, and
  user scripts.
- **Web UI** — Svelte 5 SPA (TypeScript, Tailwind CSS, shadcn-svelte)
  embedded in the binary. Queue, History, Warnings tabs with real-time
  WebSocket push (no polling), speed display, bandwidth limiting, settings
  editor, three-way theme toggle (light/dark/system), and a connection-loss
  overlay with auto-reconnect.
- **Legacy API** — Full `/api?mode=...` dispatch compatible with tools like
  Sonarr, Radarr, and NZB360.
- **RSS feeds** — Configurable feed polling with regex filters.
- **Watched folders** — Directory scanner for automatic NZB ingestion.
- **HTTPS** — Optional TLS listener with auto-generated self-signed certs.
- **Single binary** — The UI is embedded via `//go:embed`; no external
  assets or runtime dependencies beyond optional `par2` and `7z`.
- **Pure Go** — No CGO dependencies. RAR decoding via `nwaples/rardecode`.
  SQLite via `modernc.org/sqlite`.

## Requirements

- Go 1.25 or later (see `go.mod`).
- Node.js 18+ (build-time only, for the Svelte UI).
- Optional at runtime:
  - `par2` — parity verify and repair.
  - `7z`, `7zz`, `7zzs`, or `7za` — 7-Zip for non-RAR archive extraction
    (zip, 7z, etc.). GoNZBD probes these names in order; override with
    `GONZBD_SEVENZIP_BIN` or `postproc.sevenz_command` in config.
  - `unrar` — only needed if you set `postproc.use_go_rar: false`.
    GoNZBD defaults to pure-Go RAR extraction (`use_go_rar: true`) which
    requires no external binary. Disable the Go extractor to fall back to
    the `unrar` command-line tool.

  If a required binary is not on `PATH` and no override is configured, the
  corresponding post-processing step is skipped with a logged warning. The
  core download pipeline does not require any of these.

- For the quality gates (optional for end users, required for contributors):
  [`golangci-lint`](https://golangci-lint.run/) v2.0+.

## Build

```bash
cd ui && npm install && npm run build && cd ..
go build ./cmd/gonzbd
```

The first command builds the Svelte SPA into `ui/dist/`; the Go build
embeds it into the binary. Node.js is only needed at build time.

Versioned build:

```bash
go build -ldflags "-X main.Version=$(git describe --tags --always --dirty)" ./cmd/gonzbd
```

## Docker

Docker is the recommended way to run GoNZBD. The image is based on
Alpine Linux and includes all post-processing dependencies (`par2`,
`7z`). RAR extraction uses the built-in pure-Go decoder by default;
no `unrar` binary is needed unless you explicitly set
`postproc.use_go_rar: false` in your config.

### Quick start with Docker Compose

```bash
# 1. Clone the repo (or just grab the docker-compose.yml)
git clone https://github.com/hobeone/gonzbd.git
cd gonzbd

# 2. Create the host directories
mkdir -p config downloads/incomplete downloads/complete admin

# 3. Start the container
docker compose up -d

# 4. Open the web UI
open http://localhost:4289
```

On first run, GoNZBD creates a default `config/gonzbd.yaml` with
randomly generated API keys. Edit it to add your news servers, then
restart:

```bash
docker compose restart gonzbd
```

### User / group identifiers

The container supports `PUID` and `PGID` environment variables to
control which user/group ID the process runs as. This is important
when sharing download volumes with other containers (Plex, Sonarr,
Radarr) or the host filesystem — files will be owned by the specified
UID/GID.

Find your user's IDs with:

```bash
id $USER
# uid=1000(you) gid=1000(you) ...
```

Then set them in your compose file:

```yaml
environment:
  - PUID=1000
  - PGID=1000
```

If not specified, both default to `1000`.

### Ports

Port mapping has two levels:

1. **`GONZBD_PORT`** (environment variable, default `4289`) — the port
   gonzbd listens on *inside* the container. The entrypoint passes
   `--listen 0.0.0.0:$GONZBD_PORT` automatically.
2. **`ports:`** in docker-compose (or `-p` in `docker run`) — maps the
   container port to a host port.

Most users only need to change the compose `ports:` mapping:

```yaml
ports:
  # Map host port 6789 → container port 4289
  - "6789:4289"
```

To change the internal port too (e.g., if another process inside the
container uses 4289):

```yaml
environment:
  - GONZBD_PORT=9090
ports:
  - "4289:9090"
```

For HTTPS, set `https_port` in `gonzbd.yaml` and add a second port
mapping:

```yaml
ports:
  - "4289:4289"      # HTTP
  - "8443:8443"      # HTTPS (must match https_port in gonzbd.yaml)
```

### Docker Compose reference

The included [`docker-compose.yml`](docker-compose.yml) provides a
fully documented starting point:

```yaml
services:
  gonzbd:
    image: gonzbd:latest
    build:
      context: .
      dockerfile: Dockerfile
      args:
        VERSION: "1.0.0"
    container_name: gonzbd
    restart: unless-stopped

    ports:
      - "4289:4289"
      # - "8443:8443"  # HTTPS

    volumes:
      - ./config:/config
      - ./downloads/incomplete:/data/downloads
      - ./downloads/complete:/data/complete
      - ./admin:/data/admin
      # - ./watch:/data/watch      # watched folder
      # - ./scripts:/data/scripts  # post-processing scripts

    environment:
      - PUID=1000
      - PGID=1000
      - TZ=America/Los_Angeles

    healthcheck:
      test: wget --spider -q http://localhost:$${GONZBD_PORT:-4289}/api?mode=version&output=json
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 10s
```

### Configuration for Docker

The auto-generated config uses relative paths and localhost binding.
For Docker, edit `config/gonzbd.yaml` with absolute container paths:

```yaml
general:
  host: 0.0.0.0             # bind to all interfaces (required in Docker)
  port: 4289
  api_key: <auto-generated>  # keep the generated key or replace it
  nzb_key: <auto-generated>
  download_dir: /data/downloads
  complete_dir: /data/complete
  admin_dir: /data/admin
  # Optional watched folder (uncomment the volume mount too):
  # dirscan_dir: /data/watch
  # dirscan_speed: 5

servers:
  - name: primary
    host: news.your-provider.com
    port: 563
    username: your_username
    password: your_password
    connections: 8
    ssl: true
    ssl_verify: 2           # 0=none, 1=cert, 2=hostname
    priority: 0
    pipelining_requests: 2  # commands in-flight per connection (1-10)
    enable: true
  # Optional backup server:
  # - name: backup
  #   host: news.backup-provider.com
  #   port: 563
  #   username: backup_user
  #   password: backup_pass
  #   connections: 4
  #   ssl: true
  #   priority: 1
  #   optional: true
  #   enable: true

downloads:
  bandwidth_max: 0          # 0 = unlimited; or e.g. "50M" for 50 MB/s
  bandwidth_perc: 100
  write_cache_size: 500M
  max_art_tries: 3

postproc:
  enable_unrar: true
  enable_7zip: true
  enable_par_cleanup: true  # delete .par2 files after successful repair
  use_go_rar: true          # pure-Go RAR extraction — no unrar binary required
  direct_unpack: false      # stream-extract RAR volumes while downloading
  direct_unpack_threads: 3  # max concurrent DirectUnpack workers

categories:
  - name: Default
    pp: 3                   # 0=none, 1=repair, 2=unpack, 3=both
    dir: ""
  - name: tv
    pp: 3
    dir: TV                 # creates /data/complete/TV/<job>
  - name: movies
    pp: 3
    dir: Movies
```

> **Note**: The entrypoint automatically passes
> `--listen 0.0.0.0:$GONZBD_PORT` (default 4289), so the `host` field
> in the config is overridden inside Docker.

### Volumes

| Container Path | Purpose | Size | Backup? |
|---|---|---|---|
| `/config` | Config file (`gonzbd.yaml`) | Tiny | Yes |
| `/data/downloads` | Active/incomplete downloads | Large, fast I/O | No |
| `/data/complete` | Finished downloads | Large | No |
| `/data/admin` | Queue state, history DB, locks | Small | **Yes** |
| `/data/watch` | NZB watched folder (optional) | Tiny | No |
| `/data/scripts` | Post-processing scripts (optional) | Tiny | Yes |

### Building the image

```bash
# Standard build
docker build -t gonzbd:latest .

# With version tag
docker build --build-arg VERSION=1.0.0 -t gonzbd:1.0.0 .

# Or via docker compose
docker compose build
```

### Running without Compose

```bash
docker run -d \
  --name gonzbd \
  -p 4289:4289 \
  -v ./config:/config \
  -v ./downloads/incomplete:/data/downloads \
  -v ./downloads/complete:/data/complete \
  -v ./admin:/data/admin \
  -e PUID=1000 \
  -e PGID=1000 \
  -e TZ=America/Los_Angeles \
  --restart unless-stopped \
  gonzbd:latest
```

## Quickstart — run the daemon

1. **Build the binary** (see above) so `./gonzbd` sits in the repo root.

2. **Create a config directory and copy the sample config**:

   ```bash
   mkdir -p ~/.config/gonzbd
   cp test/fixtures/gonzbd.yaml ~/.config/gonzbd/gonzbd.yaml
   ```

3. **Edit `~/.config/gonzbd/gonzbd.yaml`**. At minimum, replace the
   example upstream news server block under `servers:` with your provider's
   real `host`, `port`, `username`, and `password`. The sample config has
   two servers (`primary` and `backup`) — delete the backup entry if you
   only have one account.

   Other fields worth reviewing:

   - `general.host` / `general.port` — the listen address (`127.0.0.1:4289`
     by default).
   - `general.api_key` — pre-populated with a placeholder key. **Replace it
     with a fresh random key** before exposing the daemon beyond localhost:

     ```bash
     head -c 8 /dev/urandom | xxd -p
     ```

     Paste the output into the `api_key:` field. (The same format is
     accepted for `nzb_key:`.)

   - `general.https_port` — set to a port (e.g., `8443`) to enable
     HTTPS alongside the HTTP listener. See [HTTPS](#https) below.

4. **Start the daemon**:

   ```bash
   ./gonzbd --config ~/.config/gonzbd/gonzbd.yaml --serve
   ```

   The daemon automatically creates `download_dir`, `complete_dir`, and
   `admin_dir` on startup if they don't exist. Add `-v` for debug-level
   logging. The server logs `http listener starting addr=127.0.0.1:4289 ...`
   when it's ready.

5. **Open the UI**. Navigate to `http://127.0.0.1:4289/` in a browser.
   The API key is set automatically via a cookie — no manual entry needed.
   The UI shows Queue, History, and Warnings tabs with real-time updates.

   If you prefer API-only access:

   ```bash
   curl 'http://127.0.0.1:4289/api?mode=version'
   curl 'http://127.0.0.1:4289/api?mode=fullstatus&apikey=YOUR_KEY&output=json'
   ```

6. **Add an NZB** either by dropping it into the `dirscan_dir` watched
   folder or POSTing it to the API:

   ```bash
   curl -F 'name=@/path/to/file.nzb' \
        'http://127.0.0.1:4289/api?mode=addfile&apikey=YOUR_KEY&output=json'
   ```

   Watch progress with:

   ```bash
   curl 'http://127.0.0.1:4289/api?mode=queue&apikey=YOUR_KEY&output=json'
   ```

Shut down with Ctrl-C (SIGINT); the daemon persists queue state and
history on exit.

## One-shot download (non-UI)

For smoke-testing or scripted use, the daemon can download a single NZB and
exit without starting the HTTP server:

```bash
./gonzbd --config ~/.config/gonzbd/gonzbd.yaml --nzb /path/to/file.nzb
```

## Test

```bash
go test ./...                                     # unit tests
go test -race ./...                               # with race detector
go test -run TestFoo ./internal/nzb/              # single test
go test -bench=. ./internal/decoder/              # benchmarks for one package
go test -tags=integration ./test/integration/...  # integration tests (API)
./scripts/run_tests.sh                            # full suite (Go + UI + E2E)
```

The full test script runs Go unit tests, Vitest component tests for the
Svelte UI, and Playwright-based end-to-end browser tests.

## Lint and static analysis

```bash
go vet ./...
golangci-lint run ./...
```

These checks must pass before each commit. See
[`CLAUDE.md`](CLAUDE.md) for the full quality-gate policy.

## Repository layout

```
cmd/gonzbd/         Main binary entry point
internal/           Internal packages (api, app, downloader, queue, ...)
ui/                 Svelte 5 SPA (TypeScript, Vite, Tailwind CSS)
docs/               Architecture docs and SABnzbd spec reference
scripts/            Build and test helper scripts
test/e2e/           End-to-end download pipeline tests
test/fixtures/      Sample config, NZB fixtures
test/integration/   Integration tests (API, //go:build integration)
test/mocknntp/      Configurable NNTP server for integration tests
test/uitest/        Playwright browser tests for the web UI
```

No `Makefile` is provided; standard `go` tooling is the supported build
interface.

## HTTPS

To enable HTTPS, set three fields in your config file:

```yaml
general:
  https_port: 8443
  https_cert: /path/to/server.crt
  https_key:  /path/to/server.key
```

- `https_port` — The TLS listen port. `0` (default) disables HTTPS.
- `https_cert` / `https_key` — Paths to PEM-encoded certificate and
  private key files.

**Auto-generated self-signed certificate**: If `https_port` is set but
the cert/key files don't exist on disk, the daemon automatically
generates a self-signed Ed25519 certificate and writes it to the
configured paths. This is convenient for local development but browsers
will show a security warning. For production use, provide a real
certificate (e.g., from Let's Encrypt).

Both the HTTP and HTTPS listeners serve the same API and web UI. They
share the same authentication (API key, HTTP Basic Auth) and are shut
down gracefully on SIGINT/SIGTERM.

## Unimplemented Config Fields

The following config fields exist in the schema (and are accepted by the
YAML parser) but are **not yet wired** to operational logic. They are
preserved for forward compatibility with planned features.

### GeneralConfig

| Field | Notes |
|-------|-------|
| `language` | The `internal/i18n` package exists but the UI doesn't use it for locale selection yet. |

### RSSFeedConfig / RSSFilterConfig

| Field | Notes |
|-------|-------|
| `RSSFeedConfig.cat` | Feed-level default category. Not copied to `rss.Feed`. |
| `RSSFeedConfig.pp` | Feed-level default PP level. Not copied. |
| `RSSFeedConfig.script` | Feed-level default script. Not copied. |
| `RSSFeedConfig.priority` | Feed-level default priority. Not copied. |
| `RSSFilterConfig.body` | Body regex matching. Only title regex is compiled. |
| `RSSFilterConfig.cat` | Per-filter category override. Not copied to `rss.Filter`. |
| `RSSFilterConfig.pp` | Per-filter PP override. Not copied. |
| `RSSFilterConfig.script` | Per-filter script override. Not copied. |
| `RSSFilterConfig.priority` | Per-filter priority override. Not copied. |
| `RSSFilterConfig.size_from` | Min size filter. `rss.Feed` has `MinBytes` but never populated from config. |
| `RSSFilterConfig.size_to` | Max size filter. Same. |
| `RSSFilterConfig.age` | Max age filter. Same. |

### SorterConfig

| Field | Notes |
|-------|-------|
| `multipart_label` | Multi-part label expansion. Not read in `sorterRulesFromConfig()`. |

## Acknowledgements

GoNZBD is heavily inspired by [SABnzbd](https://sabnzbd.org), the
original Python-based automated Usenet binary newsreader. The design,
API schema, and behavioral spec were studied from the SABnzbd source
code and documentation. GoNZBD is not affiliated with or endorsed by
the SABnzbd project.

## License

GPL-2.0 or later, matching upstream SABnzbd.
