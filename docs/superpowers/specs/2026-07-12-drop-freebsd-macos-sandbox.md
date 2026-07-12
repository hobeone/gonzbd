# Design: Drop FreeBSD/macOS process sandboxing, fail fast on strict_sandbox

Date: 2026-07-12
Status: Approved

## Problem

`internal/cmdutil` wraps external unpacker subprocesses (`unrar`, `7z`) in an
OS-level sandbox: `bwrap` on Linux, `sandbox-exec` on macOS, `jail` on
FreeBSD. Only the Linux (`bwrap`) backend is exercised in CI or on real
hardware available to this project. The macOS and FreeBSD backends are
unverified and untestable here, so they should not be presented as a
supported enforcement mechanism.

Today, if `strict_sandbox: true` is set on an unsupported platform and the
wrapper binary is present, `wrapSandbox` "succeeds" (it only confirms the
binary is on `PATH`) but the resulting invocation is unverified. If the
binary is missing, `strict_sandbox: true` aborts the *extraction*, not
startup — the misconfiguration only surfaces the first time a job needs
unpacking, well into runtime.

## Goal

- Remove the FreeBSD (`jail`) and macOS (`sandbox-exec`) backends entirely.
- Make `strict_sandbox: true` on any non-Linux platform a hard, immediate
  error — at process startup and at live config reload — rather than a
  per-invocation failure.
- Leave `strict_sandbox: false` (the default outside Docker) unchanged on
  every platform: unpacking still runs unsandboxed there, exactly as today.

## Non-goals

- No new sandboxing backend for FreeBSD/macOS.
- No change to Linux (`bwrap`) behavior.
- No change to the always-on post-extraction path-containment check
  (`stage_unpack.go`), which is independent of `strict_sandbox` and already
  covers the "extracted file escapes the job directory" case.

## Design

### 1. `internal/cmdutil` — delete platform backends

- Delete `sandbox_freebsd.go` (`jail` wrapper) and `sandbox_darwin.go`
  (`sandbox-exec` wrapper).
- Widen `sandbox_stub.go`'s build tag from
  `//go:build !linux && !darwin && !freebsd` to `//go:build !linux`.
  FreeBSD, macOS, and every other non-Linux GOOS now uniformly return
  `ErrSandboxUnsupported` from `wrapSandbox`. `sandbox_linux.go` (`bwrap`) is
  untouched.
- Update `sandbox_test.go`'s `TestBuildSandboxedCommand_StrictFailure`: only
  `GOOS == "linux"` expects `ErrSandboxUnavailable`; every other GOOS
  (including darwin/freebsd now) expects `ErrSandboxUnsupported`.

### 2. Fail fast at process startup

`internal/app/stages.go`, `buildStages()`: immediately after `strictSandbox`
is read out of config (existing `cfg.WithRead` block), add:

```go
if strictSandbox && goos != "linux" {
    return builtStages{}, fmt.Errorf("postproc.strict_sandbox is not supported on %s (only linux); disable strict_sandbox or run on linux", goos)
}
```

Introduce a package-level `var goos = runtime.GOOS` in `internal/app`
(mirroring the existing `lookPath` override pattern in `internal/cmdutil`) so
tests can force both branches regardless of the OS actually running the test
suite.

`buildStages()` runs once inside `app.New()`, so this error surfaces through
`New()`'s existing error-return path at startup — no separate check needed
in `cmd/gonzbd`.

### 3. Fail fast on live config reload

Live settings changes (via the web UI / API) bypass `buildStages()` and flip
`UnpackStage.BaseOpts.Sandbox.Strict` directly through a setter chain. That
chain must reject the same way:

- `internal/postproc/stage_unpack.go`: change
  `SetStrictSandbox(v bool)` → `SetStrictSandbox(v bool) error`. When
  `v && goos != "linux"`, return the same "not supported on GOOS" error and
  leave `BaseOpts.Sandbox.Strict` unchanged (reject, don't partially apply).
  Introduce the same overridable `var goos = runtime.GOOS` pattern in
  `internal/postproc`.
- `internal/app/reloader.go`: `Application.SetStrictSandbox(v bool) error`
  propagates the `UnpackStage` error.
- `internal/app/reloader.go`: `ReloadPostProcOptions` changes from `void` to
  returning `error` — the `SetStrictSandbox` error, if any. (Every other
  `Set*` call in that function is currently infallible.)
- `internal/api/server.go` `AppInterface` and `internal/api/nopapp.go`:
  update the `ReloadPostProcOptions` signature to match.
- `internal/api/config.go`: handle the returned error from
  `s.app.ReloadPostProcOptions(...)` the same way the adjacent
  `ReloadDownloader` error is already handled a few lines above — config has
  already been written to disk via `s.config.Set` + `s.config.Save`, so
  respond `200 OK` with a `"warning"` field describing the rejected reload,
  rather than turning the whole config-save request into a failure.
- Update test call sites for the new signatures: `internal/app/app_test.go`,
  `internal/app/reloader_test.go`, `internal/postproc/stage_unpack_test.go`,
  `internal/api/config_test.go`, `internal/api/nopapp_test.go`.

### 4. Docs sync

- `README.md` "OS sandboxing in Docker" section: remove the `sandbox-exec`
  (macOS) mention; state sandboxing enforcement is Linux-only (`bwrap`) and
  that `strict_sandbox: true` is rejected at startup (and at config reload)
  on every other platform.
- `docs/ARCHITECTURE.md` "OS-level sandboxing" section: same edit — drop
  `sandbox-exec`/macOS from the description of layer 1, note the new
  fail-fast startup/reload behavior.
- `internal/config/postproc.go`: update the `StrictSandbox` field doc
  comment to note Linux-only enforcement.
- `docs/sabnzbd_spec.md` §9.x config table entry for `strict_sandbox`: same
  note.
- No change needed to `test/fixtures/gonzbd.yaml`'s existing
  `strict_sandbox: true` — confirm the test/CI suite only runs on Linux
  runners so this fixture stays valid; call this out explicitly as a plan
  step rather than assuming it.

### 5. Testing

- `internal/cmdutil`: updated `TestBuildSandboxedCommand_StrictFailure`
  (see §1).
- `internal/app`: table-driven test around `buildStages`/`New()` covering:
  - `strict_sandbox=true`, `goos=linux` → succeeds.
  - `strict_sandbox=true`, `goos=darwin` (and freebsd) → startup error
    naming the platform.
  - `strict_sandbox=false`, `goos=darwin` → succeeds, unchanged fallback
    behavior.
- `internal/postproc`: extend `TestUnpackStage_SetStrictSandbox` to cover the
  new error return and the `goos` override.
- `internal/app`: extend `TestReloadPostProcOptions`/reloader tests for the
  now-propagated error.
- `internal/api`: extend `config_test.go` for the `"warning"`-response path
  when a live reload rejects `strict_sandbox`.

## Interface change note

`AppInterface.ReloadPostProcOptions` (in `internal/api/server.go`) changes
signature from `(pp, scriptDir)` to `(pp, scriptDir) error`. Per AGENTS.md,
changes to public interfaces between packages should be escalated — flagged
here: this is a mechanical, in-repo-only change (no external consumers of
this interface), following the exact existing `ReloadDownloader`
error-handling convention already present in `internal/api/config.go`.

## Open items carried into implementation

None — all decisions above were confirmed during brainstorming.
