# GoNZBD

A high-performance Usenet binary newsreader written in Go, heavily inspired
by [SABnzbd](https://sabnzbd.org). GoNZBD targets fresh installations — it
is not a drop-in replacement for an existing Python SABnzbd install.

## Features

- **Full download pipeline** — NZB parsing, NNTP article fetching with
  multi-server failover, yEnc decoding, and file assembly.
- **Post-processing** — par2 verify/repair, RAR/7z extraction, file
  deobfuscation, sorting rules, and user scripts.
- **Web UI** — Svelte 5 SPA (TypeScript, Tailwind CSS, shadcn-svelte)
  embedded in the binary. Queue, History, Warnings tabs with real-time
  WebSocket updates, speed display, bandwidth limiting, and settings editor.
- **Legacy API** — Full `/api?mode=...` dispatch compatible with tools like
  Sonarr, Radarr, and NZB360.
- **RSS feeds** — Configurable feed polling with regex filters.
- **Watched folders** — Directory scanner for automatic NZB ingestion.
- **HTTPS** — Optional TLS listener with auto-generated self-signed certs.
- **Single binary** — The UI is embedded via `//go:embed`; no external
  assets or runtime dependencies beyond optional `par2`/`unrar`/`7z`.
- **Pure Go** — No CGO dependencies. SQLite via `modernc.org/sqlite`.

## Requirements

- Go 1.25 or later (see `go.mod`).
- Node.js 18+ (build-time only, for the Svelte UI).
- Optional at runtime:
  - `par2` — parity verify and repair.
  - `unrar` — archive extraction.
  - `7z` or `7zz` — archive extraction (alternative to unrar).

  If these binaries are not on `PATH`, the corresponding post-processing
  steps are skipped with a logged warning. The core download pipeline
  does not require them.

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

   - `general.host` / `general.port` — the listen address (`127.0.0.1:8080`
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
   logging. The server logs `http listener starting addr=127.0.0.1:8080 ...`
   when it's ready.

5. **Open the UI**. Navigate to `http://127.0.0.1:8080/` in a browser.
   Enter your API key (from `gonzbd.yaml`) when prompted. The UI shows
   Queue, History, and Warnings tabs with real-time updates.

   If you prefer API-only access:

   ```bash
   curl 'http://127.0.0.1:8080/api?mode=version'
   curl 'http://127.0.0.1:8080/api?mode=fullstatus&apikey=YOUR_KEY&output=json'
   ```

6. **Add an NZB** either by dropping it into the `dirscan_dir` watched
   folder or POSTing it to the API:

   ```bash
   curl -F 'name=@/path/to/file.nzb' \
        'http://127.0.0.1:8080/api?mode=addfile&apikey=YOUR_KEY&output=json'
   ```

   Watch progress with:

   ```bash
   curl 'http://127.0.0.1:8080/api?mode=queue&apikey=YOUR_KEY&output=json'
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

### DownloadConfig

| Field | Notes |
|-------|-------|
| `min_free_space_cleanup` | Post-processing cleanup target. Depends on `min_free_space` guard (which _is_ wired). |

### CategoryConfig

| Field | Notes |
|-------|-------|
| `pp` | Per-category post-processing level. Not resolved during job ingestion. |
| `script` | Per-category post-processing script. Not resolved during job ingestion. |
| `priority` | Per-category download priority. Not resolved during job ingestion. |

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
