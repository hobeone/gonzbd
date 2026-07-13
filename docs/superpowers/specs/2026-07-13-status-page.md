# Design: Extended Status Page

Date: 2026-07-13
Status: Approved

## Problem

The UI's current status-related surfaces are scattered across three
components: `AboutDialog.svelte` (version/paths/binary versions, fetched
once via `mode=about`), `ServerStatusPanel.svelte` (per-server NNTP health,
pushed live via WebSocket, backed by `mode=server_stats`), and
`WarningsBanner.svelte` (`mode=warnings`). There is no single place a user
can go to get a full picture of "is my install healthy" the way NZBGet's
Status tab provides — and no equivalent of NZBGet's active diagnostics
(connection test, disk speed test) at all.

## Goal

Add a dedicated `/status` page to the Svelte UI, modeled on NZBGet's Status
tab, covering:

1. **General Info** — version/commit/build date, uptime, config file path,
   Go version, hostname, local/public IP, resolved par2/unrar/7z binary
   paths (version strings for unrar/7z, which the existing probe already
   captures via `unpack.UnrarInfo.VersionStr`/`unpack.SevenzInfo.Version`;
   par2 shows path only — `par2.Caps` tracks capability flags, not a
   version string, and adding one is out of scope here), and a
   GitHub-release version check (replaces NZBGet's "Updates" section —
   gonzbd has no auto-updater, so this is informational only: "up to date"
   vs. "vX.Y.Z available").
2. **News Servers** — per-server passive status (reusing the existing
   WebSocket-pushed data already shown in `ServerStatusPanel`) plus a new
   **"Test Connection"** action per server.
3. **System Info** — OS/arch, article-cache memory usage, download
   directory free space (with the configured `min_free_space` threshold
   for context), plus a **"Test Disk Speed"** action for the download
   directory.

## Non-goals

- No general "Internet Speed Test" against a public server.
- No per-server bandwidth speed test (only connection test).
- No "Updates" release-channel/changelog/install mechanism — gonzbd has no
  auto-updater; the GitHub check is read-only information.
- No changes to any SABnzbd-compatible mode's response shape
  (`about`, `fullstatus`, `queue`, etc.) — see Architecture below.

## Architecture: where this lives in the API

Three options were considered:

- **(A) Extend an existing SABnzbd-compatible mode** (e.g. cram new fields
  into `fullstatus`/`about`). Rejected: those endpoints are polled by
  third-party tools (Sonarr/Radarr) on a schedule per the SABnzbd-compat
  contract. Bloating them — especially with anything that triggers actual
  I/O like a disk-speed test — means paying for expensive checks on every
  routine poll.
- **(B) New dedicated mode(s) inside the existing `/api?mode=` dispatch
  table.** Reuses existing auth levels (`LevelProtected`), the JSON
  response envelope (`respondOK`/`respondJSON`), and routing
  (`internal/api/router.go`'s `registerModes`). There's already precedent:
  `mode=server_stats` and `mode=warnings` are gonzbd-only extensions
  coexisting with SABnzbd-compat modes. New modes are opt-in, only called
  when the status page is open.
- **(C) A wholly separate URL prefix/router outside the mode dispatch.**
  Rejected: reinvents auth/logging/envelope machinery that already exists,
  introduces a second routing convention for no benefit over (B).

**Decision: (B).** The page (`ui/src/routes/status/+page.svelte`) is new;
the backend stays inside the existing, consistent mode-dispatch pattern.

## New API surface (all additive)

- **`mode=status_overview`** (new top-level mode, `LevelProtected`):
  aggregate snapshot for General Info + System Info. Fetched once on page
  load and on a manual refresh button — no polling, no WebSocket push.
  Response shape:
  ```json
  {
    "status": true,
    "general": {
      "version": "v1.2.0", "commit": "...", "build_date": "...",
      "go_version": "go1.26.4", "uptime_seconds": 12345,
      "hostname": "...", "local_ip": "...", "public_ip": "...",
      "config_path": "...", "download_dir": "...", "complete_dir": "...",
      "admin_dir": "...", "script_dir": "...", "log_dir": "...",
      "par2": {"path": "..."},
      "unrar": {"path": "...", "version": "..."},
      "sevenzip": {"path": "...", "version": "..."},
      "update_check": {"status": "up_to_date|update_available|unknown", "latest_version": "v1.3.0"}
    },
    "system": {
      "os": "linux", "arch": "amd64",
      "article_cache_bytes": 12345678,
      "download_dir_free_bytes": 987654321,
      "min_free_space_bytes": 1073741824
    }
  }
  ```
- **`mode=status&name=test_connection&value=<server_name>`**: new `case`
  added to the *existing* sub-action dispatcher in
  `internal/api/status.go`'s `modeStatus` (same pattern as
  `unblock_server`). Dials the named server via `internal/nntp.Dial` with a
  short timeout, closes immediately, and reports:
  ```json
  {"status": true, "result": {"ok": true, "latency_ms": 143}}
  ```
  or on failure:
  ```json
  {"status": true, "result": {"ok": false, "error": "...", "likely_connection_limit": true}}
  ```
  `likely_connection_limit` is `true` when the dial error satisfies
  `errors.Is(err, nntp.ErrServerUnavailable)` (NNTP 502/503) — the response
  most providers return when an account's configured connection limit is
  already in use. The UI surfaces this as a distinct, softer message
  ("this commonly happens when your existing downloads are using all
  configured connections, not necessarily a config problem") rather than a
  bare failure. The server's current active/max connection counts (already
  tracked and pushed via the existing `server_stats` WebSocket data) are
  shown inline next to the "Test Connection" button *before* the user
  clicks it, so a maxed-out server is visible up front, not only after a
  failed test.
- **`mode=status&name=test_disk_speed`**: new `case` in the same
  dispatcher. Writes a fixed-size (64 MiB) temp file to the configured
  download directory via `os.CreateTemp`, times the write + `fsync`,
  deletes it, and reports MB/s:
  ```json
  {"status": true, "result": {"mb_per_sec": 210.4}}
  ```
  Bounded by a context timeout (e.g. 10s) so a genuinely broken disk can't
  hang the request indefinitely.

No changes to `mode=server_stats`, `mode=about`, `mode=warnings`, or any
SABnzbd-compat mode.

## New backend capabilities

- **Disk free space**: `internal/assembler.FreeBytes(dir) (int64, error)`
  already exists (used today to enforce `min_free_space`) — reused as-is.
  Exposed through a new `Application` method (e.g.
  `DownloadDirFreeBytes() (int64, error)`) so `internal/api` keeps talking
  only to `internal/app`, consistent with its existing dependency pattern,
  rather than importing `internal/assembler` directly.
- **Disk write-speed test**: new small helper (co-located with the new
  handler or in `internal/assembler` alongside `FreeBytes`) — write, time,
  fsync, delete a temp file in the download directory.
- **NNTP connection test**: reuses `internal/nntp.Dial` directly — no new
  connection logic. A short-lived, ad-hoc connection outside the
  downloader's pool; distinct from the pool's `Connections` limit, but
  subject to the *provider's* account-level connection cap, hence the
  502/503 handling above.
- **Article cache memory usage**: already tracked via an `atomic.Int64` in
  `internal/assembler` per existing architecture — needs a getter exposed
  up through `Application`.
- **GitHub release check**: a small background component with its own
  goroutine tied to the application's shutdown context (per the project's
  goroutine-lifecycle convention), periodically calling
  `GET https://api.github.com/repos/hobeone/gonzbd/releases/latest`,
  caching the result, and comparing against the build's `Version` (a
  `vX.Y.Z` string; `"dev"` builds report `status: "unknown"` and skip
  comparison). Uses `golang.org/x/mod/semver` (already present transitively
  in the module graph, not a new external dependency) for the comparison.
  Degrades to `status: "unknown"` if GitHub is unreachable — never blocks
  the status page or returns an error for this reason alone.

## UI

New `ui/src/routes/status/+page.svelte` — confirmed a new route under
`ui/src/routes/` "just works" with the existing `@sveltejs/adapter-static`
+ `fallback: 'index.html'` setup and the Go side's `go:embed` + SPA
fallback serving; no Go routing changes needed beyond the new API modes.

- General Info + System Info: fetched from `mode=status_overview` on
  mount, with a manual refresh button. No WebSocket subscription for this
  section.
- News Servers: reuses the existing `server_stats`-backed store (already
  live via WebSocket, same data `ServerStatusPanel` uses) so this section
  needs no new data-fetching path — just a new "Test Connection" button per
  server row, calling the new sub-action on click and showing the result
  inline (with the pre-emptive active/max-connections hint always visible,
  not just on test).
- System Info's disk speed test: a single "Test Disk Speed" button for the
  download directory, result shown inline near the free-space figure.

## Testing

- Table-driven unit tests for the disk-speed-test helper and the semver
  comparison logic (up-to-date / update-available / dev-build /
  unreachable-network cases).
- Handler tests for `status_overview`, `test_connection`, and
  `test_disk_speed`, including degradation paths: GitHub unreachable, disk
  write permission error, unknown server name, and a 502/503 dial failure
  correctly setting `likely_connection_limit: true`.
- No new UI e2e requirement beyond extending existing `test/uitest`
  coverage patterns (used for dialogs) to the new route.

## Open items carried into implementation

None — all decisions above were confirmed during brainstorming.
