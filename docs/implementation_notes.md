# Implementation Notes

Cross-session knowledge for agents working on the Go rewrite of SABnzbd.
Read this after `CLAUDE.md` and before picking up a plan step.

These notes capture **conventions, decisions, and gotchas that are not
obvious from the code itself**. For the step-by-step plan see
`docs/golang_implementation.md`; for behavioral truth see
`docs/sabnzbd_spec.md`.

---

## 1. Workflow

### 1.1 Per-step commits

Each step in `golang_implementation.md` becomes exactly one git commit.
The commit message starts with `Step X.Y:` and the body describes what
landed. Composition steps (when multiple subsystems are wired together)
use a descriptive prefix instead, e.g. `Phase 7 integration: ...`.

### 1.2 Quality gates (must all pass before commit)

```
go vet ./...
go test -race -count=1 ./...
golangci-lint run ./...
```

- Run from the module root (`/home/hobe/software/sabnzbd-go/`).
- A lint finding is fixed at the source, not silenced. If a
  suppression is genuinely required, it is narrow and documented:
  `//nolint:rulename // short reason`.
- `errcheck.check-blank=true` is on. Every `_ = f()` that returns an
  error needs a `//nolint:errcheck // <reason>` comment.

### 1.3 Model-per-step rule

Plan steps are tagged `[opus]`, `[sonnet]`, or `[haiku]`. The step is
implemented by that model. The orchestrator (usually Opus) dispatches a
subagent when the tag doesn't match its own model.

When dispatching, announce the model first:

> **Model**: Sonnet 4.5 (per the `[sonnet]` tag).

Commit trailers identify who did the work:

```
Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>
Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>
Co-Authored-By: Claude Haiku 4.5 <noreply@anthropic.com>
```

### 1.4 User authorization cadence

The user drives the cadence by typing `proceed`. Agents do **not**
start the next step on their own — the user reviews the previous
commit, approves, then authorizes. Treat anything else the user says
(a question, a correction, a followup) as feedback on the current
step, not as authorization to start a new one.

### 1.5 Subagent dispatch brief

When dispatching a subagent, the prompt must be self-contained. It
includes:

- The working directory (the subagent's shell may default to the
  Python repo — always `cd sabnzbd-go` first).
- The task, a suggested package shape, and any conventions that
  matter.
- The explicit quality gates the subagent must pass before committing.
- The commit message template with the correct model trailer.
- A "report back" section: commit SHA, files + LOC, quality-gate
  output, diverged decisions, and anything the next step should know.

Subagents are told to **stop and report** rather than force a bad
solution. Forcing produces broken code that looks finished.

---

## 2. Architecture patterns

### 2.1 Adapters live in `cmd/sabnzbd`, not in library packages

The Phase 7 composition revealed a useful pattern: each library
package (`dirscanner`, `urlgrabber`, `rss`, `scheduler`, `notifier`,
`bpsmeter`) defines a small `Handler` interface for the events it
produces, but it **does not** know about `queue.Queue`, `nzb.Parse`,
or any sibling package. The glue code that bridges a producer to a
consumer lives in `cmd/sabnzbd/adapters.go`.

- `ingestHandler` satisfies both `dirscanner.Handler` and
  `urlgrabber.Handler` (identical shape: `HandleNZB(ctx, filename,
  []byte) error`), so a single object feeds both the watched-directory
  scanner and the URL grabber into the queue.
- `rssToURLHandler` bridges `rss.Handler` (takes an `rss.Item`) to a
  `*urlgrabber.Grabber` so fresh RSS items trigger URL fetches.

**Why this layout**: library packages stay dependency-free and
individually testable. Composition is a separate concern, visible in
one file. When a new source appears (e.g. a plugin system) the author
adds a new adapter in `cmd/sabnzbd`, not a new cross-package
dependency.

### 2.2 Matching interface shapes across packages enables substitution

`dirscanner.Handler` and `urlgrabber.Handler` deliberately share the
same signature. If they had diverged (`HandleWatchedNZB`,
`HandleFetchedNZB`), the composition step would have needed two
separate adapters doing the same work. When designing a new package
that consumes bytes or events, check whether an existing package's
Handler shape fits — reuse the shape even if you re-declare the type.

### 2.3 Inject clocks as functions, not interfaces

Every time-dependent subsystem (scheduler, bpsmeter) takes a
`func() time.Time` injected at construction. In production that's
`time.Now`; in tests it's `func() time.Time { return fixedT }`. A
single-method `Clock` interface is the Java-brained alternative —
four extra types for no additional flexibility. Interfaces are
appropriate when multiple methods are needed (`Now` + `NewTicker`).

### 2.4 Split long-running loops from single ticks

`Scanner.Run(ctx, interval)` loops forever until `ctx` cancels.
`Scanner.ScanOnce(ctx)` does one pass. This pattern appears in
`rss.Scanner`, `dirscanner.Scanner`, and `scheduler.Scheduler`.
Reason: `Run` is hard to test (sleep, mock clock, channel
orchestration). `ScanOnce`/`Tick` exposes the interesting behavior
synchronously, so every filter/dedup/dispatch test is deterministic.
`Run` only needs one test that proves it respects `ctx` and fires on
the interval.

### 2.5 Channel-based coordination for queue↔downloader

Follow the pattern already in `internal/queue` and `internal/downloader`:
a `chan struct{}` with cap=1 and non-blocking send is the signaling
primitive. Do **not** introduce `sync.Cond` or shared-memory
polling. Rationale documented in `docs/golang_implementation.md`
§ Coordination Architecture.

### 2.6 Persistence format choices (frozen)

- **Queue state**: in-memory with event-triggered JSON+gzip per NzbObject
- **History**: SQLite via `modernc.org/sqlite` (pure-Go, no CGO)
- **Config**: YAML via `gopkg.in/yaml.v3`
- **Dedup stores** (RSS, dirscanner): JSON maps
- **Bandwidth state** (bpsmeter): JSON, atomic write via tmp+rename
- **All file writes**: tmp + fsync + rename (atomic)

Do not deviate from these without escalating.

---

## 3. Conventions

### 3.1 Constant naming: suffix style

`NormalPriority`, `HighPriority`, `DailyPeriod`, `DownloadStarted`.
**Not** `PriorityNormal`, `PeriodDaily`, `EventDownloadStarted`.
Enum-like groups may keep a common suffix if that's the natural
reading (`RepairStage`, `UnpackStage`, `ScriptStage`).

### 3.2 Linter suppressions

Always narrow and justified:

```go
_ = f.Close() //nolint:errcheck // read-only file cleanup
os.Open(path) //nolint:gosec // G304: caller validates path is absolute
r.FormValue("q") //nolint:gosec // G120: body already limited by MaxBytesReader
```

Never silence a whole file or a whole package with a blanket
directive. If lint complains about something that's genuinely
structural, the code is probably wrong — fix it.

### 3.3 Comments

Default to none. Add a comment only when the **why** is
non-obvious: a hidden invariant, a subtle workaround, a surprising
constraint. Do not narrate what the code does; do not reference
current tickets or recent PRs ("added for issue #123" rots fast).

Exception: godoc comments on exported symbols are required (revive
enforces this). Keep them short — one or two sentences.

### 3.4 Error wrapping

```go
return fmt.Errorf("load config %s: %w", path, err)
```

Always use `%w`, always include a sensible prefix. Never use `%v` on
an error.

### 3.5 Context

First parameter on every blocking operation. Even if the current
implementation doesn't honor cancellation, accept the ctx so callers
can.

### 3.6 Logging

`log/slog` with structured fields:

```go
slog.Info("ingested nzb", "filename", filename, "bytes", job.TotalBytes)
```

Never use printf-style formatting. The logger is injected; never use
a package-level global.

### 3.7 No emojis in code or commits

User preference. Emojis only appear when the user explicitly asks.

---

## 4. Testing patterns

### 4.1 Table-driven with subtests

```go
tests := []struct{ name, input string; want int }{...}
for _, tt := range tests {
    t.Run(tt.name, func(t *testing.T) { ... })
}
```

### 4.2 `t.TempDir()` for all filesystem tests

Auto-cleanup, parallelism-safe. Never create your own temp dirs.

### 4.3 `httptest.NewServer` for HTTP-dependent tests

Covers RSS parser, URL grabber, Apprise notifier. No real network,
no mocks — a real HTTP server in-process.

### 4.4 Deterministic clocks

If a component takes a clock function, tests pass a fixed-time closure.
Never `time.Sleep` to cross a boundary.

### 4.5 Black-box vs white-box

Default: black-box (`package foo_test`). Promote a helper to
exported when it has a real non-test consumer (example:
`notifier.FormatMessage`). Don't export solely for tests.

### 4.6 Subprocess `WaitDelay`

`exec.CommandContext` does not kill grandchild processes when `ctx`
cancels. Set `cmd.WaitDelay = 2*time.Second` so the runtime
force-kills the process group after ctx timeout + waitdelay. Any new
code that shells out to a user-supplied binary must follow this
pattern (`notifier.ScriptNotifier`, `postproc.ScriptStage`).

### 4.7 Rate limiter tests and the burst trap

`rate.Limiter` pre-accumulates a burst. A naive test of "rate=1000,
ask for 2000, expect ~2s" fails because the initial burst absorbs
small asks. Drain the burst first (one `WaitN` equal to the burst
size) then time the second `WaitN`. See `internal/bpsmeter/meter_test.go`
for the pattern.

---

## 5. Gotchas

### 5.1 gopls workspace-cwd phantom diagnostics

The shell default cwd is `/home/hobe/software/sabnzbd/` (the Python
repo). gopls resolves imports relative to its cwd, so every file in
`sabnzbd-go/internal/...` gets a "not in your workspace" warning plus
a cascade of `BrokenImport` and `UndeclaredName` errors — **all
phantoms**. Ignore them.

Verify directly from the module root:

```
cd /home/hobe/software/sabnzbd-go
go vet ./internal/<pkg>/...
go build ./internal/<pkg>/...
go test -race -count=1 ./internal/<pkg>/...
```

gopls also caches intermediate drafts. After a subagent edits a file,
the IDE may report "unused import" or wrong arg counts for a version
no longer on disk. Trust `go vet`/`go build`, not the IDE overlay.

### 5.2 Shell cwd resets between Bash calls

The tool harness resets shell cwd to `/home/hobe/software/sabnzbd/`
after each Bash call. Use absolute paths or `cd X && ...` in every
call.

### 5.3 Python spec is not always right

The `docs/sabnzbd_spec.md` was patched during Phase 5.6 after the
subagent discovered §8.4 (external script contract) diverged from the
actual Python `newsunpack.py::external_processing` behavior in 5
places (argv count, env var names, etc.). When the spec disagrees
with the Python code, read the Python and patch the spec.

### 5.4 Live smoke-test after composition

Unit tests verify each package's happy path. Composition bugs
(nil derefs on optional fields, goroutine lifetime races, config
mismatches) surface only when packages cooperate. After any wiring
step:

1. Build the binary.
2. Write a small config exercising the new wiring.
3. Start the daemon; watch the log for expected startup lines.
4. Hit the relevant API endpoints with `curl`.
5. Send SIGINT; verify clean shutdown.

This caught the `bpsmeter.Capture(nil)` nil-deref that 23/23 unit
tests missed, and would catch the same class of bug again.

---

## 6. Known deferrals

These are load-bearing gaps noted but deliberately not fixed yet.
Anyone touching the surrounding code should be aware:

| Area | Deferral | Where it bites |
|---|---|---|
| `LocalhostBypass` in API auth | Hard-coded `true` in `serveMode` | Must become config-driven before any production-like use |
| HTTPS / TLS | Config fields exist; no listener implements them | Planned as Phase 8 |
| Notifier sinks | Dispatcher wired but no email/apprise/script config | Needs new fields in `config.GeneralConfig` (or new sub-structs) |
| Quota config | `bpsmeter.Quota` exists but no config surface | Add `downloads.quota_period` / `downloads.quota_bytes` when wiring |
| Speedlimit dispatch | Scheduler logs but does not throttle | Downloader must take a `*bpsmeter.Limiter`; every `conn.Read` gates on `limiter.Wait` |
| Restart / shutdown HTTP modes | Return 501 | Thread a `shutdown func()` into `api.Options` |
| History DB writes | Opened but never written | No pipeline stage emits history entries yet |
| Glitter web UI | Phase 12 in progress; templates being ported to `html/template`. Steps 12.1-12.6 done. | Some upstream feature flags do not yet have backing `RenderContext` fields — when porters hit one, the gated upstream content is omitted (not faked-as-true). See deferrals just below. |
| Glitter `$pp_pause_event` flag | Step 12.6 omitted the gated "Resume post-processing" menu entry | When pp-pause runtime state lands, add `RenderContext.PpPauseEvent`, restore the `{{if .PpPauseEvent}}...{{end}}` block referencing upstream `include_menu.tmpl` |
| Glitter `$power_options` flag | Step 12.6 omitted shutdown/standby/hibernate `<option>` entries from the menu | When OS-power shims land, add `RenderContext.PowerOptions []string` and restore the gated `<option>` block |
| Glitter sysinfo (`$platform`, `$cpumodel`, `$cpusimd`) | Step 12.7 ports `include_overlays.tmpl` with these as empty strings; the Options→Status modal shows blank "Platform / CPU / SIMD" rows instead of crashing | When a sysinfo package lands, add `RenderContext.Platform`, `RenderContext.CpuModel`, `RenderContext.CpuSimd` and populate from `runtime.GOOS`/CPU detection |
| RSS scan interval | Hard-coded 15 min in `cmd/sabnzbd/main.go::startRSSScanner` | Promote to `RSSFeedConfig.Interval` when someone needs per-feed tuning |
| `addurl` sync vs async | Blocks until fetch completes | Python's version returns immediately; revisit if clients complain |

---

## 7. Package map

Quick index — where things live.

```
cmd/sabnzbd/             daemon entry, one-shot NZB runner, composition glue
internal/api/            HTTP /api dispatcher + per-mode handlers
internal/app/            lifecycle: wires Queue + Downloader + Assembler + Postproc
internal/assembler/      writes decoded articles to disk in NZB order
internal/bpsmeter/       rolling BPS, quota, speed limiter
internal/cache/          in-memory article cache with disk spill
internal/config/         YAML config types + validation + loader
internal/constants/      typed enums (Priority, Status, PostProc stages)
internal/decoder/        yEnc/UU decode via sabctools
internal/deobfuscate/    filename de-obfuscation
internal/dirscanner/     watched-directory scanner + decompress
internal/downloader/     NNTP event loop + server dispatch
internal/history/        SQLite repository
internal/nntp/           NNTP protocol (AUTHINFO, BODY, pipelining)
internal/notifier/       dispatcher + email/apprise/script sinks
internal/nzb/            NZB XML parser + typed in-memory model
internal/par2/            par2 repair driver
internal/postproc/       stage pipeline + wrappers (par2 → unpack → deobf → sort → script)
internal/queue/          priority-ordered queue + job state
internal/rss/            feed parser, filter, dedup, scanner
internal/scheduler/      cron parser + action registry + oneshots
internal/sorting/        filename template engine
internal/unpack/         unrar + 7z drivers
internal/urlgrabber/     HTTP fetch + decompress + handler dispatch
internal/web/            embedded Glitter static assets + placeholder index
```

---

## 8. Reference priorities

When the answer isn't in the code, consult in this order:

1. `docs/golang_implementation.md` — what to build next.
2. `docs/sabnzbd_spec.md` — how it must behave. Treat spec updates
   as authoritative once committed.
3. `../sabnzbd/sabnzbd/*.py` — clarification of intent, edge cases,
   and gotchas. **Not** for line-by-line transliteration. Python's
   threading model, persistence, and class hierarchies do not map
   1:1 to the Go design.

If all three disagree, surface the conflict to the user before
picking. The spec is authoritative, but the spec has been wrong
before.
