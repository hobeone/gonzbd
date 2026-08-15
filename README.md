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
- **Watched folders** — Directory scanner for automatic NZB ingestion.
- **HTTPS** — Optional TLS listener with auto-generated self-signed certs.
- **Single binary** — The UI is embedded via `//go:embed`; no external
  assets or runtime dependencies beyond optional `par2` and `7z`.
- **Pure Go** — No CGO dependencies. RAR5 decoding via `hobeone/rarengine`.
  SQLite via `modernc.org/sqlite`.

## Requirements

- Go 1.26 or later (see `go.mod`).
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
cd ui && bun install && bun run build && cd ..
go build ./cmd/gonzbd
```

The first command builds the Svelte SPA into `ui/dist/`; the Go build
embeds it into the binary. Bun is only needed at build time.

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

The included [`docker-compose-example.yml`](docker-compose-example.yml) provides a
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
    stop_grace_period: 75s

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

### Shutdown Grace Period

GoNZBD gracefully drains active NNTP connections and flushes queue state to disk during shutdown.
- **Docker Compose:** Handled automatically via `stop_grace_period: 75s` in `docker-compose-example.yml`.
- **`docker run` / `docker create`:** Pass `--stop-timeout 75` when creating containers, or use `-t 75` when stopping/restarting (`docker stop -t 75`).
- **Systemd:** Set `TimeoutStopSec=75` in your systemd service unit file (under `[Service]`).

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
  write_cache_size: 64M       # write-coalescing budget; 0 disables
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

### OS sandboxing in Docker

On native (non-Docker) installs, external `unrar`/`7z` subprocesses run inside
an OS-level sandbox (`bwrap` on Linux — the only supported platform for this)
that restricts filesystem access to the job's own directory, and
`strict_sandbox: true` (the default) aborts extraction if that sandbox can't
be established. `strict_sandbox: true` is rejected at startup (and at live
config reload) on any platform other than Linux, since there is no working
sandbox backend to enforce it there. **This means an existing non-Docker
FreeBSD/macOS install that has never touched this setting is already at the
default of `true`, and will fail to start after upgrading until you set
`strict_sandbox: false` in its config.**

**The Docker image does not install `bwrap`, and a brand-new container config
defaults `strict_sandbox` to `false`.** `bwrap` needs to create an
unprivileged user+mount namespace, which a normal (non-`--privileged`)
container's default seccomp/AppArmor profile blocks — installing it would not
make sandboxing work, it would just make every extraction fail immediately.
Docker's own container boundary plus gonzbd's own post-extraction path
containment check (which runs regardless of `strict_sandbox` and rejects any
extracted file that lands outside the job directory — see `docs/ARCHITECTURE.md`)
provide the practical protection here instead.

`strict_sandbox` is a normal `gonzbd.yaml` field — set it to `true` at any
time (existing config or new) if you've built a custom image with
`bubblewrap` installed and granted the container `--privileged` (or an
equivalent capability set) so `bwrap` can actually run. The `false` default
described above only applies to the value written the first time a container
generates a brand-new config; it does not change any config you already have.

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

Standard Docker builds are fully supported. To build the image locally:

```bash
docker build -t gonzbd:latest .
```

Via Docker Compose:

```bash
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

These checks must pass before pushing your changes to the repository.

## Pure-Go Extraction & Repair (Zero-Dependency Mode)

GoNZBD supports **zero-dependency execution** by utilizing native, pure-Go libraries for all core post-processing operations, eliminating the need to install or shell out to external system binaries:

* **Pure-Go RAR Extraction** (`use_go_rar`): Powered by `hobeone/rarengine` (RAR5). Enabled by default; falls back to external `unrar` for RAR3/4 or unsupported features.
* **Pure-Go 7-Zip Extraction** (`use_go_7z`): Powered by `bodgit/sevenzip`. Enabled by default; no `7z` binary required.
* **Pure-Go PAR2 Verification & Repair** (`use_go_par2`): Powered by our high-performance native `github.com/hobeone/par2engine` module. Enabled by default; no `par2cmdline` binary required.

All pure-Go extractors and repair engines can be toggled in the Web UI (Settings -> Post-Processing) or via `gonzbd.yaml`. If any native engine encounters errors, it can automatically fall back to executing local system binaries (`unrar`, `7z`, `par2`) if they are available.

## Upgrading Dependencies

### Go Dependencies
Upgrade dependencies using standard Go toolchain commands:

```bash
# Upgrade all direct and indirect dependencies to their latest minor/patch versions
go get -u ./...

# Clean up unused/redundant dependencies in go.mod and go.sum
go mod tidy
```

To upgrade a Go package to a newer **major version** (which typically introduces breaking changes and uses a new import path like `/v2` or `/v3`):
1. Update the package import paths in your Go source files (e.g., changing `github.com/hobeone/par2engine` to `github.com/hobeone/par2engine/v2`).
2. Run `go get` with the new import path to fetch the major version, then tidy:
   ```bash
   go get github.com/hobeone/par2engine/v2@latest
   go mod tidy
   ```


### Frontend Packages
To manage and upgrade frontend Svelte dependencies in the `ui/` subdirectory:
```bash
cd ui

# Upgrade all packages to their latest versions, ignoring semver constraints (may introduce breaking changes)
bun x npm-check-updates -u && bun install

# Upgrade a specific package to its latest version (including major/breaking changes)
bun add <package-name>@latest

# Update all packages according to semver constraints in package.json (safe minor/patch updates)
bun update

# Audit vulnerability warnings
bun pm audit
```

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

## Security & Reverse Proxy Deployments

By default, GoNZBD trusts loopback connections (`127.0.0.1` and `::1`)
and automatically issues an ephemeral session cookie to them, granting
full administrative access to the web UI without entering a key. Any
other client address is untrusted by default and must authenticate
with the API key or NZB key.

**The risk:** if you run a reverse proxy (Nginx, Apache, Caddy, Traefik,
etc.) on the *same host* as GoNZBD, every proxied request arrives at
GoNZBD from `127.0.0.1` — indistinguishable from a genuinely local
connection. If the proxy itself doesn't authenticate the client, anyone
who can reach the proxy gets an unauthenticated admin session.

**How to configure this safely:**

- **Proxy on the same host, no proxy-level auth:** leave
  `general.verify_xff_header` at its default (`false`). GoNZBD has no
  way to distinguish proxied traffic from genuinely local traffic in
  this setup, so the proxy itself is responsible for restricting who
  can reach it (e.g. bind it to a private interface, put it behind a
  VPN, or add HTTP auth at the proxy).
- **Proxy on a different host or container (e.g. a Docker bridge
  network):** first make sure the proxy itself authenticates every
  client (e.g. HTTP auth, a VPN, or an access-control layer) — adding
  its IP or CIDR to `general.local_ranges` extends GoNZBD's
  loopback-level trust to that proxy's traffic, it does not add any
  authentication of its own. Once the proxy authenticates its clients,
  add its IP or CIDR to `general.local_ranges` so the UI works for it
  without an API key — see the `LocalRanges` field documentation in
  `internal/config/general.go` for the exact matching rules. Loopback
  remains trusted either way.
- **`general.verify_xff_header`:** when `true`, GoNZBD additionally
  requires that *every* hop listed in a trusted request's
  `X-Forwarded-For` header also be a trusted address (loopback or
  `local_ranges`) — not just the direct connection. This guards against
  a malicious client spoofing an `X-Forwarded-For` header to impersonate
  a trusted proxy chain; it is not a substitute for the proxy
  authenticating its own clients. Leave it `false` (the default) unless
  you understand this distinction and your proxy chain's every hop is
  itself trusted.

In short: GoNZBD's loopback/`local_ranges` trust model secures the
connection *to* GoNZBD, not the connection *to* your reverse proxy —
that authentication is always the proxy's own responsibility.

## Unimplemented Config Fields

The following config fields exist in the schema (and are accepted by the
YAML parser) but are **not yet wired** to operational logic. They are
preserved for forward compatibility with planned features.

### GeneralConfig

| Field | Notes |
|-------|-------|
| `language` | The `internal/i18n` package exists but the UI doesn't use it for locale selection yet. |

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

GoNZBD is free software licensed under the **GNU General Public License,
version 2 or (at your option) any later version** — the same terms as
upstream [SABnzbd](https://sabnzbd.org).

```
SPDX-License-Identifier: GPL-2.0-or-later
```

The full text of the GPL v2 is in the [`LICENSE`](LICENSE) file. This program
is distributed in the hope that it will be useful, but WITHOUT ANY WARRANTY;
without even the implied warranty of MERCHANTABILITY or FITNESS FOR A
PARTICULAR PURPOSE. See the GNU General Public License for more details.
