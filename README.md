# sabnzbd-go

A Go reimplementation of [SABnzbd](https://sabnzbd.org), the automated Usenet
binary newsreader. Fresh-install target — not a drop-in replacement for an
existing Python SABnzbd install (see
[`docs/golang_implementation.md`](docs/golang_implementation.md) *Design
Policy: Compatibility Scope*).

## Status

Core implementation complete for the backend and the Glitter web UI. The
daemon downloads and assembles NZBs end-to-end, runs post-processing (par2
verify + unrar/7z unpack + sorters + user scripts), and exposes the full
legacy mode-dispatch API (`/api?mode=...`) that the UI talks to.

**The Glitter UI is now served at `/`** with all five partials wired:
queue, history, messages, overlays, and menu. Opening
`http://127.0.0.1:8080/` in a browser shows the full Knockout-powered
interface with Queue, History, and Warnings tabs.

A few upstream features render as inactive UI elements (sysinfo display,
OS-power options, post-processing pause toggle). These are deliberate
deferrals — see [`docs/implementation_notes.md`](docs/implementation_notes.md)
§6 for the full list. For browser-based manual verification steps, see
[`docs/ui_smoke_checklist.md`](docs/ui_smoke_checklist.md).

See [`docs/golang_implementation.md`](docs/golang_implementation.md) for
the full phase/step breakdown.

## Requirements

- Go 1.25 or later (see `go.mod`).
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
go build ./cmd/sabnzbd
```

Versioned build:

```bash
go build -ldflags "-X main.Version=$(git describe --tags --always --dirty)" ./cmd/sabnzbd
```

## Quickstart — run the daemon

These steps get you a running daemon you can use via the Glitter web UI,
`curl`, a watched folder, or the `--nzb` one-shot flag.

1. **Build the binary** (see above) so `./sabnzbd` sits in the repo root.

2. **Create a config directory and copy the sample config**:

   ```bash
   mkdir -p ~/.config/sabnzbd-go
   cp test/fixtures/sabnzbd.yaml ~/.config/sabnzbd-go/sabnzbd.yaml
   ```

3. **Edit `~/.config/sabnzbd-go/sabnzbd.yaml`**. At minimum, replace the
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

4. **Create the directories the config references**. By default the sample
   expects the following tree relative to the working directory you start
   the daemon from:

   ```bash
   mkdir -p Downloads/incomplete Downloads/complete Downloads/watch \
            scripts logs admin
   ```

   Or edit `general.download_dir`, `general.complete_dir`,
   `general.dirscan_dir`, `general.log_dir`, and `general.admin_dir` to
   absolute paths you prefer.

5. **Start the daemon**:

   ```bash
   ./sabnzbd --config ~/.config/sabnzbd-go/sabnzbd.yaml --serve
   ```

   Add `-v` for debug-level logging. The server logs `http listener
   starting addr=127.0.0.1:8080 ...` when it's ready.

6. **Open the UI**. Navigate to `http://127.0.0.1:8080/` in a browser.
   The Glitter UI loads with Queue, History, and Warnings tabs. For a
   full manual verification walkthrough see
   [`docs/ui_smoke_checklist.md`](docs/ui_smoke_checklist.md).

   If you prefer API-only access, the existing `curl` examples still work:

   ```bash
   curl 'http://127.0.0.1:8080/api?mode=version'
   curl 'http://127.0.0.1:8080/api?mode=fullstatus&apikey=YOUR_KEY&output=json'
   ```

7. **Add an NZB** either by dropping it into the `dirscan_dir` watched
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
./sabnzbd --config ~/.config/sabnzbd-go/sabnzbd.yaml --nzb /path/to/file.nzb
```

## Test

```bash
go test ./...                                   # unit tests
go test -race ./...                             # with race detector
go test -run TestFoo ./internal/nzb/            # single test
go test -bench=. ./internal/decoder/            # benchmarks for one package
go test -tags=integration ./test/integration/...  # integration tests
```

## Lint and static analysis

```bash
go vet ./...
golangci-lint run ./...
```

These checks must pass before each commit. See
[`CLAUDE.md`](CLAUDE.md) for the full quality-gate policy.

## Repository layout

```
cmd/sabnzbd/        Main binary entry point
internal/           Internal packages (api, app, downloader, queue, ...)
test/mocknntp/      Configurable NNTP server for integration tests
test/integration/   Integration tests gated by //go:build integration
test/fixtures/      Sample config, NZB fixtures, etc.
docs/               Spec, implementation plan, cross-session notes
```

No `Makefile` is provided; standard `go` tooling is the supported build
interface.

## License

GPL-2.0 or later, matching upstream SABnzbd.
