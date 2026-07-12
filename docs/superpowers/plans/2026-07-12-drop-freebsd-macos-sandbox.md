# Drop FreeBSD/macOS Process Sandboxing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remove the untestable FreeBSD (`jail`) and macOS (`sandbox-exec`) process-sandbox backends, and make `strict_sandbox: true` fail immediately (at startup and at live config reload) on any non-Linux platform instead of silently failing per-extraction.

**Architecture:** `internal/cmdutil` keeps a single real sandbox backend (`bwrap` on Linux); all other platforms fall through to the existing "unsupported" stub. Two call sites gate `strict_sandbox=true` on non-Linux: `buildStages()` (process startup, inside `app.New()`) and `UnpackStage.SetStrictSandbox` (live config reload via the API), both returning an error instead of silently degrading. The error propagates up through `Application.SetStrictSandbox` → `ReloadPostProcOptions` → the API's `set_config` handler, which already has a convention (`ReloadDownloader`'s error path) for reporting a reload failure as a `"warning"` in an otherwise-successful save response.

**Tech Stack:** Go 1.26, standard library only (`runtime.GOOS`, `errors`, `fmt`).

## Global Constraints

- Every `.go` file touched must be run through `goimports -w <file>` and `go build ./...` immediately after editing (AGENTS.md).
- `go test -race ./...` and `golangci-lint run ./...` must pass before any commit that isn't purely docs.
- One logical change per commit; commit messages follow Conventional Commits 1.0.0 with `Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>`.
- Never modify existing `internal/history/migrations/` files — not applicable here (no schema change).
- No new external dependencies — not applicable here (stdlib only).
- Red-Green discipline: every new test must be shown failing against the pre-fix code before the fix is applied, per AGENTS.md.

---

### Task 1: Remove FreeBSD/macOS sandbox backends, widen the stub

**Files:**
- Delete: `internal/cmdutil/sandbox_freebsd.go`
- Delete: `internal/cmdutil/sandbox_darwin.go`
- Modify: `internal/cmdutil/sandbox_stub.go`
- Modify: `internal/cmdutil/sandbox_test.go:33-52` (`TestBuildSandboxedCommand_StrictFailure`)

**Interfaces:**
- Consumes: nothing new.
- Produces: on every GOOS except `linux`, `wrapSandbox(targetDir, name, args string/[]string)` now returns `("", nil, ErrSandboxUnsupported)` (previously darwin/freebsd had their own success paths). This is consumed by `BuildSandboxedCommand` in `internal/cmdutil/sandbox.go`, which already handles `ErrSandboxUnsupported` via the existing `Strict`/non-`Strict` branch — no changes needed there.

- [ ] **Step 1: Confirm current behavior with a failing-first check**

Run the existing test to see today's platform-dependent branching (this documents the pre-change baseline; it should currently PASS since it's written to match current behavior):

```bash
go test ./internal/cmdutil/... -run TestBuildSandboxedCommand_StrictFailure -v
```

Expected: PASS (this confirms the test we're about to change currently encodes the *old* three-platform behavior, which we're intentionally changing).

- [ ] **Step 2: Update `TestBuildSandboxedCommand_StrictFailure` to expect the new (post-change) behavior**

Edit `internal/cmdutil/sandbox_test.go`, replacing the body of `TestBuildSandboxedCommand_StrictFailure`:

```go
func TestBuildSandboxedCommand_StrictFailure(t *testing.T) {
	orig := lookPath
	defer func() { lookPath = orig }()
	lookPath = func(file string) (string, error) {
		return "", errors.New("not found")
	}

	ctx := context.Background()
	cfg := SandboxConfig{Enabled: true, Strict: true, TargetDir: "/tmp"}
	_, err := BuildSandboxedCommand(ctx, CmdConfig{}, cfg, "echo", "hello")
	if runtime.GOOS == "linux" {
		if !errors.Is(err, ErrSandboxUnavailable) {
			t.Fatalf("expected ErrSandboxUnavailable, got %v", err)
		}
	} else {
		if !errors.Is(err, ErrSandboxUnsupported) {
			t.Fatalf("expected ErrSandboxUnsupported, got %v", err)
		}
	}
}
```

(Only the condition on line `if runtime.GOOS == "linux" {` changes — it previously also included `|| runtime.GOOS == "darwin" || runtime.GOOS == "freebsd"`.)

- [ ] **Step 3: Run the test to verify it still passes on this (Linux) machine**

```bash
go test ./internal/cmdutil/... -run TestBuildSandboxedCommand_StrictFailure -v
```

Expected: PASS (this dev machine is Linux, so the `linux` branch is exercised; the `else` branch is exercised on the darwin/freebsd CI/build targets, which is fine to not test locally — Step 5 below adds a GOOS-independent test for the deleted files).

- [ ] **Step 4: Delete the FreeBSD and macOS backend files**

```bash
rm internal/cmdutil/sandbox_freebsd.go internal/cmdutil/sandbox_darwin.go
```

- [ ] **Step 5: Widen `sandbox_stub.go`'s build tag**

Read current content first:

```bash
cat internal/cmdutil/sandbox_stub.go
```

Replace the build tag line so the stub covers every non-Linux GOOS:

```go
//go:build !linux

package cmdutil

import "fmt"

func wrapSandbox(targetDir, name string, args []string) (bin string, wrappedArgs []string, err error) {
	return "", nil, fmt.Errorf("%w", ErrSandboxUnsupported)
}
```

(Only the build tag changed: `//go:build !linux && !darwin && !freebsd` → `//go:build !linux`.)

- [ ] **Step 6: Verify the package still builds for darwin and freebsd via cross-compilation**

```bash
GOOS=darwin GOARCH=amd64 go build ./internal/cmdutil/...
GOOS=freebsd GOARCH=amd64 go build ./internal/cmdutil/...
GOOS=linux GOARCH=amd64 go build ./internal/cmdutil/...
```

Expected: all three succeed with no output (a `//go:build` mismatch or duplicate `wrapSandbox` definition would fail one of these builds — this is the only way to catch that locally since the dev machine is Linux).

- [ ] **Step 7: Run quality gates**

```bash
goimports -w internal/cmdutil/sandbox_stub.go internal/cmdutil/sandbox_test.go
go vet ./internal/cmdutil/...
go test -race ./internal/cmdutil/...
golangci-lint run ./internal/cmdutil/...
```

Expected: all pass, no new lint findings.

- [ ] **Step 8: Commit**

```bash
git add internal/cmdutil/sandbox_freebsd.go internal/cmdutil/sandbox_darwin.go internal/cmdutil/sandbox_stub.go internal/cmdutil/sandbox_test.go
git commit -m "$(cat <<'EOF'
refactor(cmdutil): drop untestable FreeBSD/macOS sandbox backends

The jail (FreeBSD) and sandbox-exec (macOS) wrappers were never
exercised in CI or on real hardware available to this project. Both
platforms now uniformly hit the existing "unsupported" stub, matching
every other non-Linux GOOS.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: Fail fast at process startup when strict_sandbox is unsupported

**Files:**
- Modify: `internal/app/stages.go:1-15` (imports), `:107` (var block), `:145-149` (WithRead block + new check)
- Modify: `internal/app/stages_test.go:328-421` (`Config` struct, `convertConfig`)
- Test: `internal/app/stages_test.go` (new test)

**Interfaces:**
- Consumes: `internal/config.PostProcConfig.StrictSandbox bool` (existing field, `internal/config/postproc.go:117`).
- Produces: a package-level `var goos = runtime.GOOS` in `package app` (internal/app/stages.go), overridable by tests in the same package (`internal/app/stages_test.go`, `internal/app/reloader_test.go`, `internal/app/app_test.go` if it were internal — it's `package app_test` and does not need this). `buildStages(cfg *config.Config, version string, log *slog.Logger, probe binaryProbe) (builtStages, error)` now returns a non-nil error when `strict_sandbox=true` and `goos != "linux"`.

- [ ] **Step 1: Write the failing test first**

Add to `internal/app/stages_test.go`, in the "Error paths" section (after `TestBuildStages_BadExtraUnrarParams`, before the "Enablement via config flags" section):

```go
func TestBuildStages_StrictSandboxRejectedOnUnsupportedPlatform(t *testing.T) {
	origGOOS := goos
	defer func() { goos = origGOOS }()

	goos = "darwin"
	cfg := Config{
		DownloadDir:   t.TempDir(),
		CompleteDir:   t.TempDir(),
		StrictSandbox: true,
	}
	_, err := testBuildStages(cfg, discardLog(), emptyProbe())
	if err == nil {
		t.Fatal("expected error for strict_sandbox=true on darwin, got nil")
	}
	if !strings.Contains(err.Error(), "darwin") {
		t.Errorf("error = %q, want it to mention the platform (darwin)", err.Error())
	}
}

func TestBuildStages_StrictSandboxAllowedOnLinux(t *testing.T) {
	origGOOS := goos
	defer func() { goos = origGOOS }()

	goos = "linux"
	cfg := Config{
		DownloadDir:   t.TempDir(),
		CompleteDir:   t.TempDir(),
		StrictSandbox: true,
	}
	if _, err := testBuildStages(cfg, discardLog(), emptyProbe()); err != nil {
		t.Fatalf("buildStages: unexpected error on linux: %v", err)
	}
}

func TestBuildStages_NonStrictSandboxAllowedOnUnsupportedPlatform(t *testing.T) {
	origGOOS := goos
	defer func() { goos = origGOOS }()

	goos = "darwin"
	cfg := Config{
		DownloadDir:   t.TempDir(),
		CompleteDir:   t.TempDir(),
		StrictSandbox: false,
	}
	if _, err := testBuildStages(cfg, discardLog(), emptyProbe()); err != nil {
		t.Fatalf("buildStages: unexpected error for strict_sandbox=false on darwin: %v", err)
	}
}
```

Add `"strings"` to `internal/app/stages_test.go`'s import block if not already present (check first — it currently imports `log/slog`, `testing`, and three internal packages; `strings` is not there).

Add `StrictSandbox bool` to the `Config` struct (`internal/app/stages_test.go:328-370`), immediately after the `FlatUnpack bool` field:

```go
	FlatUnpack           bool
	StrictSandbox        bool
```

Add the corresponding line to `convertConfig` (`internal/app/stages_test.go:372-416`), immediately after `o.PostProc.ScriptCanFail = c.ScriptCanFail`:

```go
		o.PostProc.ScriptCanFail = c.ScriptCanFail
		o.PostProc.StrictSandbox = c.StrictSandbox
```

- [ ] **Step 2: Run the new tests to verify they fail for the right reason**

```bash
go test ./internal/app/... -run TestBuildStages_StrictSandbox -v
```

Expected: `TestBuildStages_StrictSandboxRejectedOnUnsupportedPlatform` FAILS with `expected error for strict_sandbox=true on darwin, got nil` (the guard doesn't exist yet). The other two either fail to compile (no `goos` var yet) or fail — either way, this confirms the guard and the `goos` var are both missing before we add them. Also expect a compile error mentioning `undefined: goos` — that's the correct failure signal for a not-yet-written variable; note it and proceed to Step 3.

- [ ] **Step 3: Add the `goos` var and the startup guard**

Edit `internal/app/stages.go`. Add `"runtime"` to the import block (alphabetical order among stdlib imports — after `"net"` and before `"strconv"`):

```go
import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"runtime"
	"strconv"

	"github.com/hobeone/gonzbd/internal/cmdutil"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/par2"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/unpack"
)
```

Add the overridable var right after the `binaryProbe` struct declaration (after the closing `}` that currently ends around line 24), before `probeBinaries`:

```go
// goos is runtime.GOOS by default; tests override it to exercise
// platform-specific validation (e.g. strict_sandbox rejection) without
// needing to cross-compile or run on multiple real operating systems.
var goos = runtime.GOOS
```

In `buildStages`, immediately after the `cfg.WithRead(func(c *config.Config) { ... })` block closes (the line reading `})` right after `listenAddr = net.JoinHostPort(...)`, i.e. right before the `// Quick-check stage:` comment), insert:

```go
	if strictSandbox && goos != "linux" {
		return builtStages{}, fmt.Errorf("postproc.strict_sandbox is not supported on %s (only linux); disable strict_sandbox or run on linux", goos)
	}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/app/... -run TestBuildStages_StrictSandbox -v
```

Expected: all three PASS.

- [ ] **Step 5: Run the full app package test suite**

```bash
go test -race ./internal/app/...
```

Expected: PASS — this also confirms no other `buildStages` caller/test broke (e.g. `TestBuildStages_StageOrder`, which doesn't set `StrictSandbox`, so it defaults to `false` and is unaffected).

- [ ] **Step 6: Quality gates**

```bash
goimports -w internal/app/stages.go internal/app/stages_test.go
go vet ./internal/app/...
golangci-lint run ./internal/app/...
```

Expected: all pass.

- [ ] **Step 7: Commit**

```bash
git add internal/app/stages.go internal/app/stages_test.go
git commit -m "$(cat <<'EOF'
feat(app): reject strict_sandbox at startup on unsupported platforms

buildStages() now fails fast when postproc.strict_sandbox=true and the
platform isn't linux (the only OS with a real sandbox backend since
the FreeBSD/macOS wrappers were removed), instead of deferring the
failure to the first extraction attempt.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: Fail fast on live config reload (UnpackStage.SetStrictSandbox)

**Files:**
- Modify: `internal/postproc/stage_unpack.go:1-20` (imports), `:195-200` (`SetStrictSandbox`)
- Modify: `internal/postproc/stage_unpack_test.go:1357-1379` (`TestUnpackStage_SetStrictSandbox`)

**Interfaces:**
- Consumes: nothing new.
- Produces: `func (u *UnpackStage) SetStrictSandbox(v bool) error` (was `func (u *UnpackStage) SetStrictSandbox(v bool)`, no return). A package-level `var goos = runtime.GOOS` in `package postproc` (same pattern as Task 2, separate package so it needs its own var). Consumed by Task 4's `Application.SetStrictSandbox`.

- [ ] **Step 1: Write the failing test first**

Edit `internal/postproc/stage_unpack_test.go`, replacing `TestUnpackStage_SetStrictSandbox`:

```go
func TestUnpackStage_SetStrictSandbox(t *testing.T) {
	s := NewUnpackStageWith(unpack.Options{
		Sandbox: cmdutil.SandboxConfig{
			Enabled: true,
			Strict:  true,
		},
	}, false)

	if err := s.SetStrictSandbox(false); err != nil {
		t.Fatalf("SetStrictSandbox(false): unexpected error: %v", err)
	}
	s.mu.RLock()
	strict := s.BaseOpts.Sandbox.Strict
	s.mu.RUnlock()
	if strict {
		t.Error("expected SetStrictSandbox(false) to update BaseOpts.Sandbox.Strict")
	}

	if err := s.SetStrictSandbox(true); err != nil {
		t.Fatalf("SetStrictSandbox(true): unexpected error: %v", err)
	}
	job := &Job{DownloadDir: "/tmp/test-sandbox-target", Queue: &queue.Job{}}
	opts := s.prepareOptions(t.Context(), slog.Default(), job, s.BaseOpts, "")
	if opts.Sandbox.TargetDir != "/tmp/test-sandbox-target" {
		t.Errorf("expected prepareOptions to set TargetDir to %q, got %q", job.DownloadDir, opts.Sandbox.TargetDir)
	}
}

func TestUnpackStage_SetStrictSandbox_RejectedOnUnsupportedPlatform(t *testing.T) {
	origGOOS := goos
	defer func() { goos = origGOOS }()
	goos = "darwin"

	s := NewUnpackStageWith(unpack.Options{
		Sandbox: cmdutil.SandboxConfig{Enabled: true, Strict: false},
	}, false)

	err := s.SetStrictSandbox(true)
	if err == nil {
		t.Fatal("expected error enabling strict_sandbox on darwin, got nil")
	}
	if !strings.Contains(err.Error(), "darwin") {
		t.Errorf("error = %q, want it to mention the platform (darwin)", err.Error())
	}

	s.mu.RLock()
	strict := s.BaseOpts.Sandbox.Strict
	s.mu.RUnlock()
	if strict {
		t.Error("expected rejected SetStrictSandbox(true) to leave BaseOpts.Sandbox.Strict unchanged (false)")
	}
}
```

Check `internal/postproc/stage_unpack_test.go`'s import block for `"strings"`; add it if absent (alphabetical order in the stdlib group).

- [ ] **Step 2: Run the new test to verify it fails**

```bash
go test ./internal/postproc/... -run TestUnpackStage_SetStrictSandbox -v
```

Expected: compile error `undefined: goos` (the var doesn't exist in `package postproc` yet), or once stubbed, `TestUnpackStage_SetStrictSandbox_RejectedOnUnsupportedPlatform` fails with "expected error ..., got nil". Either failure mode confirms the guard is missing.

- [ ] **Step 3: Add the `goos` var and update `SetStrictSandbox`**

Edit `internal/postproc/stage_unpack.go`. Add `"runtime"` to the import block (after `"path/filepath"`, before `"slices"`):

```go
import (
	"cmp"
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/hobeone/gonzbd/internal/cmdutil"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/unpack"
)
```

Add the overridable var near the top of the file, after the import block and before the `UnpackStage` struct declaration:

```go
// goos is runtime.GOOS by default; tests override it to exercise
// platform-specific validation without needing to run on multiple
// real operating systems.
var goos = runtime.GOOS
```

Replace `SetStrictSandbox`:

```go
// SetStrictSandbox updates strict sandbox setting at runtime. Thread-safe.
// Returns an error, leaving the setting unchanged, if v is true and the
// current platform has no working sandbox backend (only linux does).
func (u *UnpackStage) SetStrictSandbox(v bool) error {
	if v && goos != "linux" {
		return fmt.Errorf("postproc.strict_sandbox is not supported on %s (only linux); disable strict_sandbox or run on linux", goos)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.BaseOpts.Sandbox.Strict = v
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/postproc/... -run TestUnpackStage_SetStrictSandbox -v
```

Expected: both tests PASS.

- [ ] **Step 5: Run the full postproc package test suite**

```bash
go test -race ./internal/postproc/...
```

Expected: PASS.

- [ ] **Step 6: Quality gates**

```bash
goimports -w internal/postproc/stage_unpack.go internal/postproc/stage_unpack_test.go
go vet ./internal/postproc/...
golangci-lint run ./internal/postproc/...
```

Expected: all pass.

- [ ] **Step 7: Commit**

```bash
git add internal/postproc/stage_unpack.go internal/postproc/stage_unpack_test.go
git commit -m "$(cat <<'EOF'
feat(postproc): reject live strict_sandbox toggle on unsupported platforms

UnpackStage.SetStrictSandbox now returns an error (leaving the setting
unchanged) when enabling strict_sandbox on a platform without a
working sandbox backend, closing the gap where a live UI/API toggle
could silently accept a setting that would only fail at the next
extraction.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: Propagate the error through Application.SetStrictSandbox and ReloadPostProcOptions

**Files:**
- Modify: `internal/app/reloader.go:23-59` (`ReloadPostProcOptions`), `:397-405` (`SetStrictSandbox`)
- Modify: `internal/app/reloader_test.go:111-156` (`TestApplication_ReloadOptions` step 6/7 block — verify no compile break; add explicit assertion), `:180-204` (`TestApplication_ReloadPostProcOptions_NoDeadlockUnderReadLock` — verify no compile break)
- Modify: `internal/app/app_test.go:1013`, `:1053`, `:1107` (call sites — verify no compile break)

**Interfaces:**
- Consumes: `(u *UnpackStage) SetStrictSandbox(v bool) error` from Task 3.
- Produces: `func (app *Application) SetStrictSandbox(v bool) error` (was no return). `func (app *Application) ReloadPostProcOptions(pp config.PostProcConfig, scriptDir string) error` (was no return). Consumed by Task 5 (`internal/api`).

- [ ] **Step 1: Write the failing test first**

Add to `internal/app/reloader_test.go`, after `TestApplication_ReloadPostProcOptions_NoDeadlockUnderReadLock` (end of file):

```go
func TestApplication_SetStrictSandbox_RejectedOnUnsupportedPlatform(t *testing.T) {
	origGOOS := goos
	defer func() { goos = origGOOS }()
	goos = "darwin"

	cfg := testConfig(t.TempDir(), t.TempDir(), t.TempDir())
	app, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer app.Shutdown()

	if err := app.SetStrictSandbox(true); err == nil {
		t.Fatal("expected error enabling strict_sandbox on darwin, got nil")
	}
}

func TestApplication_ReloadPostProcOptions_PropagatesStrictSandboxError(t *testing.T) {
	origGOOS := goos
	defer func() { goos = origGOOS }()
	goos = "darwin"

	cfg := testConfig(t.TempDir(), t.TempDir(), t.TempDir())
	app, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer app.Shutdown()

	pp := config.PostProcConfig{StrictSandbox: true}
	if err := app.ReloadPostProcOptions(pp, ""); err == nil {
		t.Fatal("expected ReloadPostProcOptions to propagate strict_sandbox rejection, got nil")
	}
}
```

(`New(cfg, nil)` and `testConfig(...)` are already used identically a few lines above in the same file, in `TestApplication_ReloadPostProcOptions_NoDeadlockUnderReadLock`, and `config` is already imported in `reloader_test.go` — verify with `head -15 internal/app/reloader_test.go` before writing, and add the import if it's actually missing.)

- [ ] **Step 2: Run the new tests to verify they fail**

```bash
go test ./internal/app/... -run 'TestApplication_SetStrictSandbox_RejectedOnUnsupportedPlatform|TestApplication_ReloadPostProcOptions_PropagatesStrictSandboxError' -v
```

Expected: compile errors, since `SetStrictSandbox`/`ReloadPostProcOptions` don't return anything yet (`app.SetStrictSandbox(true)` used in an `if err := ...; err == nil` form won't compile against a `void` function). This compile failure is the correct "red" signal for this step.

- [ ] **Step 3: Update `SetStrictSandbox` and `ReloadPostProcOptions` to return errors**

Edit `internal/app/reloader.go`. Replace `SetStrictSandbox`:

```go
// SetStrictSandbox updates strict sandboxing option at runtime. Thread-safe.
// Returns an error, leaving the setting unchanged, if v is true and the
// current platform has no working sandbox backend (only linux does).
func (app *Application) SetStrictSandbox(v bool) error {
	if app.unpackStage != nil {
		if err := app.unpackStage.SetStrictSandbox(v); err != nil {
			return err
		}
	}
	app.config.With(func(c *config.Config) {
		c.PostProc.StrictSandbox = v
	})
	return nil
}
```

(Note the reordering versus the original: validate via `unpackStage.SetStrictSandbox` *before* writing the new value into `app.config`, so a rejected toggle doesn't leave the persisted config and the running stage disagreeing. If `app.unpackStage` is nil — not expected in production, but some tests construct `Application` without stages — skip validation and apply directly, matching the original's unconditional `c.PostProc.StrictSandbox = v` when there's no stage to guard.)

Change the last line of `ReloadPostProcOptions`'s signature and body. The function currently ends with:

```go
	app.SetIgnoreUnrarDates(pp.IgnoreUnrarDates)
	app.SetStrictSandbox(pp.StrictSandbox)
}
```

Replace the signature and that final call:

```go
func (app *Application) ReloadPostProcOptions(pp config.PostProcConfig, scriptDir string) error {
```

```go
	app.SetIgnoreUnrarDates(pp.IgnoreUnrarDates)
	return app.SetStrictSandbox(pp.StrictSandbox)
}
```

(Every other line inside `ReloadPostProcOptions` is unchanged — they're all infallible `Set*` calls with no return value, per the existing code shown in the design.)

- [ ] **Step 4: Fix compile breaks at existing call sites**

These calls currently ignore the (previously nonexistent) return value as a bare statement, which still compiles fine in Go even after adding a return value (`app.SetStrictSandbox(false)` and `application.ReloadPostProcOptions(cfg.PostProc, cfg.General.ScriptDir)` remain valid statements). Confirm this by building:

```bash
go build ./...
```

Expected: succeeds. If it does NOT succeed, the failure will point at specific call sites — none are expected here, since Go permits discarding a single return value in a plain expression statement.

- [ ] **Step 5: Run the new tests to verify they pass**

```bash
go test ./internal/app/... -run 'TestApplication_SetStrictSandbox_RejectedOnUnsupportedPlatform|TestApplication_ReloadPostProcOptions_PropagatesStrictSandboxError' -v
```

Expected: both PASS.

- [ ] **Step 6: Run the full app package test suite**

```bash
go test -race ./internal/app/...
```

Expected: PASS, including the pre-existing `TestApplication_ReloadOptions` (steps 6/7 in `reloader_test.go`) and `TestReloadPostProcOptions`/the `SetStrictSandbox(false)` call in `app_test.go` — none of these set `strict_sandbox: true` on a non-linux `goos`, so their behavior is unaffected.

- [ ] **Step 7: Quality gates**

```bash
goimports -w internal/app/reloader.go internal/app/reloader_test.go
go vet ./internal/app/...
golangci-lint run ./internal/app/...
```

Expected: all pass.

- [ ] **Step 8: Commit**

```bash
git add internal/app/reloader.go internal/app/reloader_test.go
git commit -m "$(cat <<'EOF'
feat(app): propagate strict_sandbox rejection through the reload chain

Application.SetStrictSandbox and ReloadPostProcOptions now return an
error, forwarding UnpackStage's platform rejection instead of
swallowing it. Config is only mutated after the stage accepts the new
value, so a rejected toggle can't leave persisted config and the
running stage out of sync.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 5: Surface the reload error through the API as a response warning

**Files:**
- Modify: `internal/api/server.go:77-81` (`ApplicationReloader.ReloadPostProcOptions`)
- Modify: `internal/api/nopapp.go:54-55` (`NopApp.ReloadPostProcOptions`)
- Modify: `internal/api/config.go:205-214` (`postproc` reload branch)
- Modify: `internal/api/config_test.go:514-518` (`setConfigSpyApp.ReloadPostProcOptions`)
- Test: `internal/api/config_test.go` (new test)

**Interfaces:**
- Consumes: `Application.ReloadPostProcOptions(pp config.PostProcConfig, scriptDir string) error` from Task 4.
- Produces: `ApplicationReloader.ReloadPostProcOptions(pp config.PostProcConfig, scriptDir string) error` (interface method signature change — the escalated public-interface change noted in the spec). HTTP behavior: `mode=set_config&section=postproc` with a rejected `strict_sandbox=true` on an unsupported platform still returns `200 OK` (config was already persisted to disk) with a `"warning"` field describing the rejection, mirroring the existing `ReloadDownloader` error-handling block a few lines above in the same handler.

- [ ] **Step 1: Write the failing test first**

Read `internal/api/config_test.go` around lines 490-660 first to confirm exact helper names (`setConfigSpyApp`, `apiGet`, `decodeJSON`, `testAPIKey`) — they are already used identically by the neighboring `reload_servers_error` test at lines 625-656, shown in the design discovery. Add to `setConfigSpyApp` (after the existing `ReloadPostProcOptions` stub, `internal/api/config_test.go:514-518`) a way to inject an error, and a new subtest.

Add a field to `setConfigSpyApp` (in its struct literal, alongside `reloadDownloaderErr`):

```go
	reloadDownloaderErr error
	reloadPostProcErr   error
```

Replace the `ReloadPostProcOptions` stub:

```go
func (a *setConfigSpyApp) ReloadPostProcOptions(_ config.PostProcConfig, _ string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reloadedPostProc++
	return a.reloadPostProcErr
}
```

Add a new subtest, placed after the existing `reload_servers_error` subtest (`internal/api/config_test.go`, after line 656's closing `})`):

```go
	t.Run("reload_postproc_error", func(t *testing.T) {
		t.Parallel()
		cfg, err := config.Default()
		if err != nil {
			t.Fatalf("Default(): %v", err)
		}
		spy := &setConfigSpyApp{reloadPostProcErr: errors.New("postproc.strict_sandbox is not supported on darwin (only linux)")}
		s := New(Options{
			Version: "1.0.0-test",
			Config:  cfg,
			App:     spy,
		})
		cfg.With(func(c *config.Config) {
			c.General.APIKey = testAPIKey
		})

		rr := apiGet(t, s.Handler(), "/api?mode=set_config&section=postproc&keyword=strict_sandbox&value=true&apikey="+testAPIKey)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
		}
		m := decodeJSON(t, rr)
		warning, _ := m["warning"].(string)
		if !strings.Contains(warning, "strict_sandbox") {
			t.Errorf("warning = %q; want to contain 'strict_sandbox'", warning)
		}
		spy.mu.Lock()
		count := spy.reloadedPostProc
		spy.mu.Unlock()
		if count != 1 {
			t.Errorf("reloadedPostProc = %d; want 1", count)
		}
	})
```

(This test uses the same request shape and helper functions as `reload_servers_error` immediately above it in the same `t.Run` table — verify the exact enclosing `func Test...(t *testing.T) {` and indentation by reading the surrounding 150 lines before inserting, since this must land inside the same parent test as its neighbors, not as a new top-level function.)

- [ ] **Step 2: Run the new test to verify it fails**

```bash
go test ./internal/api/... -run TestSetConfig -v
```

(Use whatever the actual enclosing test function name is, discovered in Step 1 — likely something like `TestSetConfig` or `TestModeSetConfig`; find it with `grep -n "^func Test.*[Ss]et[Cc]onfig" internal/api/config_test.go` before running.)

Expected: compile error, since `ReloadPostProcOptions` doesn't return `error` on the interface/stub yet — this is the correct red signal.

- [ ] **Step 3: Update the interface, NopApp stub, and config.go handler**

Edit `internal/api/server.go`. Replace the `ReloadPostProcOptions` line in the `ApplicationReloader` interface (with its existing doc comment kept, since it's still accurate):

```go
	// ReloadPostProcOptions applies all hot-applicable postproc settings.
	// Callers must pass a value snapshot taken without holding config.Config's
	// lock (see internal/app/reloader.go) — never call from inside
	// config.WithRead/With, as that would deadlock. Returns an error if a
	// setting was rejected (e.g. strict_sandbox on an unsupported platform);
	// other settings in pp are still applied even when this returns an error.
	ReloadPostProcOptions(pp config.PostProcConfig, scriptDir string) error
```

Edit `internal/api/nopapp.go`. Replace:

```go
// ReloadPostProcOptions is a stub.
func (n NopApp) ReloadPostProcOptions(config.PostProcConfig, string) error { return nil }
```

Edit `internal/api/config.go`. Replace the `postproc` case body (currently just `s.app.ReloadPostProcOptions(pp, scriptDir)`) to mirror the adjacent `ReloadDownloader` error handling pattern used for the `servers` section a few lines above in the same function:

```go
		case "postproc":
			var pp config.PostProcConfig
			var scriptDir string
			s.config.WithRead(func(cfg *config.Config) {
				pp = cfg.PostProc
				scriptDir = cfg.General.ScriptDir
			})
			if err := s.app.ReloadPostProcOptions(pp, scriptDir); err != nil {
				s.log.Error("reload postproc options", "error", err)
				respondJSON(w, http.StatusOK, map[string]any{
					"status":  true,
					"value":   value,
					"warning": "config saved but postproc reload failed: " + err.Error(),
				})
				return
			}
```

- [ ] **Step 4: Run the new test to verify it passes**

```bash
go test ./internal/api/... -run TestSetConfig -v
```

Expected: PASS.

- [ ] **Step 5: Run the full api package test suite**

```bash
go test -race ./internal/api/...
```

Expected: PASS.

- [ ] **Step 6: Run the full repository build and test suite**

```bash
go build ./...
go test -race ./...
```

Expected: PASS — this confirms no other `ApplicationReloader` implementer (UI tests, integration test mocks) was missed. If another implementer surfaces here (e.g. in `test/uitest` or `test/integration`), update its `ReloadPostProcOptions` signature the same way (`func (...) ReloadPostProcOptions(config.PostProcConfig, string) error { return nil }` for a no-op mock).

- [ ] **Step 7: Quality gates**

```bash
goimports -w internal/api/server.go internal/api/nopapp.go internal/api/config.go internal/api/config_test.go
go vet ./internal/api/...
golangci-lint run ./internal/api/...
```

Expected: all pass.

- [ ] **Step 8: Commit**

```bash
git add internal/api/server.go internal/api/nopapp.go internal/api/config.go internal/api/config_test.go
git commit -m "$(cat <<'EOF'
feat(api): surface strict_sandbox reload rejection as a warning

mode=set_config for the postproc section now reports a rejected
strict_sandbox toggle (e.g. enabling it on macOS/FreeBSD) as a
"warning" in an otherwise-successful save response, mirroring the
existing ReloadDownloader error-handling convention. ApplicationReloader
gains an error return on ReloadPostProcOptions to carry this through.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 6: Documentation sync

**Files:**
- Modify: `README.md:275-294` (OS sandboxing in Docker section)
- Modify: `docs/ARCHITECTURE.md:138-166` (External subprocess containment section)
- Modify: `internal/config/postproc.go:114-117` (`StrictSandbox` field doc comment)

**Interfaces:** None (docs only).

- [ ] **Step 1: Update `README.md`**

In `README.md`, replace the first paragraph of the "OS sandboxing in Docker" section (currently lines 277-280):

```markdown
On native (non-Docker) installs, external `unrar`/`7z` subprocesses run inside
an OS-level sandbox (`bwrap` on Linux — the only supported platform for this)
that restricts filesystem access to the job's own directory, and
`strict_sandbox: true` (the default) aborts extraction if that sandbox can't
be established. `strict_sandbox: true` is rejected at startup (and at live
config reload) on any platform other than Linux, since there is no working
sandbox backend to enforce it there.
```

- [ ] **Step 2: Update `docs/ARCHITECTURE.md`**

In `docs/ARCHITECTURE.md`, replace point 1 of the numbered list (currently lines 143-148):

```markdown
1. **OS-level sandboxing** (`internal/cmdutil.BuildSandboxedCommand`): wraps
   the subprocess with `bwrap`, restricting filesystem writes to the job's
   directory at the kernel level. Linux is the only platform with a working
   backend — `strict_sandbox: true` on any other platform is rejected at
   startup (`internal/app.buildStages`) and at live config reload
   (`UnpackStage.SetStrictSandbox`), rather than deferring the failure to the
   first extraction attempt. On Linux, `strict_sandbox: true` makes
   `BuildSandboxedCommand` return `ErrSandboxUnavailable` (aborting
   extraction) if `bwrap` can't be found; `false` falls back to running the
   subprocess unwrapped.
```

- [ ] **Step 3: Update `internal/config/postproc.go`'s field doc comment**

Replace the `StrictSandbox` field comment (lines 114-117):

```go
	// StrictSandbox determines if external unpacker execution must abort
	// immediately when OS-level sandboxing (bwrap on Linux — the only
	// supported platform) cannot be established. Setting this to true on
	// any other platform is rejected at startup and at live config reload,
	// since there is no working sandbox backend there. Defaults to true.
	StrictSandbox bool `yaml:"strict_sandbox" json:"strict_sandbox"`
```

- [ ] **Step 4: Verify build still passes after the doc-comment edit**

```bash
go build ./...
```

Expected: succeeds (doc comment only, no behavior change).

- [ ] **Step 5: Confirm `docs/sabnzbd_spec.md` needs no change**

```bash
grep -n "strict_sandbox" docs/sabnzbd_spec.md
```

Expected: no output — this field isn't documented there today, so there is nothing to update. (Confirmed during design discovery; this step just re-verifies it before committing so the plan doesn't silently skip a real gap.)

- [ ] **Step 6: Confirm `test/fixtures/gonzbd.yaml` stays valid**

```bash
grep -n "strict_sandbox" test/fixtures/gonzbd.yaml
go test ./internal/config/... ./internal/app/... 2>&1 | tail -20
```

Expected: `test/fixtures/gonzbd.yaml:60: strict_sandbox: true` (unchanged), and both test packages pass — this fixture is only loaded by tests that run on Linux CI, where `strict_sandbox: true` remains valid.

- [ ] **Step 7: Commit**

```bash
git add README.md docs/ARCHITECTURE.md internal/config/postproc.go
git commit -m "$(cat <<'EOF'
docs(sandbox): document Linux-only sandbox enforcement

Update README, ARCHITECTURE.md, and the strict_sandbox config field
comment to reflect that sandbox-exec (macOS) and jail (FreeBSD)
backends were removed, and that strict_sandbox=true now fails fast on
any non-Linux platform instead of degrading silently.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 7: Full-repo verification pass

**Files:** None modified — verification only.

**Interfaces:** None.

- [ ] **Step 1: Run the complete quality gate sequence from AGENTS.md**

```bash
go fix ./...
goimports -w .
go vet ./...
go test -race ./...
./scripts/run_tests.sh
golangci-lint run ./...
```

Expected: all pass, no new issues. If `go fix ./...` or `goimports -w .` modify any file beyond what this plan already touched, inspect the diff (`git diff --stat`) before deciding whether to include it in a follow-up commit — don't silently fold unrelated modernization changes into this feature's commits.

- [ ] **Step 2: Run mutation testing on the diff**

```bash
gremlins unleash --timeout-coefficient 100 --diff origin/main
```

Expected: no LIVED mutants in the changed lines. If any mutant lives (e.g. the `goos != "linux"` string comparison, or the error-message `%s` formatting), add a targeted test asserting the specific behavior the mutant broke — per `docs/mutation-testing-playbook.md`.

- [ ] **Step 3: Cross-compile check for darwin/freebsd one more time, whole repo**

```bash
GOOS=darwin GOARCH=amd64 go build ./...
GOOS=freebsd GOARCH=amd64 go build ./...
```

Expected: both succeed — confirms no other package in the repo (e.g. `cmd/gonzbd`) has an unguarded reference to the deleted `sandbox_freebsd.go`/`sandbox_darwin.go` symbols.

- [ ] **Step 4: Manually verify the startup rejection end-to-end (documentation of intended behavior, not an automated step)**

This step can't run on the Linux dev machine (the guard only fires when `goos != "linux"`), so it's a code-reading confirmation instead of an executed command: re-read `internal/app/stages.go`'s new guard and confirm it is reached before any subprocess is spawned — `buildStages` returns before constructing the `unpack.Options`/`UnpackStage`, so no unrar/7z process is ever launched with a false sense of sandboxing on an unsupported platform.

- [ ] **Step 5: No commit for this task** — it's verification only. If Step 1 or Step 2 surface a real fix, that fix gets its own properly-scoped commit, not folded in here.
