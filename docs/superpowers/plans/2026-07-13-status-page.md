# Extended Status Page Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a dedicated `/status` page to the Svelte UI, modeled on NZBGet's Status tab (General Info, News Servers with connection test, System Info with disk free-space + write-speed test), backed by additive gonzbd-only API modes that never touch any SABnzbd-compatible endpoint.

**Architecture:** New backend capabilities (article-cache usage, retained binary-probe results, disk free space, disk write-speed test) are added to `internal/assembler`/`internal/app` and exposed through `Application`/`ApplicationReloader`, matching the existing "`internal/api` only talks to `internal/app`" layering. Three new gonzbd-only API modes are added: `mode=status_overview` (aggregate snapshot, fetched once per page load/refresh) and two new `case`s in the *existing* `modeStatus` sub-action dispatcher (`test_connection`, `test_disk_speed`) plus one more (`check_update`, deliberately separate so a slow/unreachable GitHub never blocks the rest of the page). The UI is a new SvelteKit route (`ui/src/routes/status/+page.svelte`) that composes these plus the *existing* WebSocket-pushed `server_stats` store — no polling, no new WebSocket message types.

**Tech Stack:** Go 1.26, Svelte 5 (runes) + SvelteKit (adapter-static, SPA mode) + TypeScript + Tailwind CSS 4 + bits-ui.

## Global Constraints

- Every `.go` file touched must be run through `goimports -w <file>` and `go build ./...` immediately after editing (AGENTS.md).
- `go test -race ./...` and `golangci-lint run ./...` must pass before any commit that isn't purely docs.
- One logical change per commit; Conventional Commits 1.0.0 with `Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>`.
- No new external Go dependencies: the version comparison is hand-rolled (see Task 7) specifically to avoid promoting `golang.org/x/mod` from a transitive to a direct dependency for a need this narrow — this was a deliberate choice made during planning, not an oversight; do not "fix" it by adding the import.
- No changes to any SABnzbd-compatible mode's response shape (`about`, `fullstatus`, `queue`, `server_stats`, `warnings`, etc.).
- `internal/api` must not import `internal/assembler` directly for the new disk-space/cache-usage/write-speed capabilities — go through `Application`. (It already imports `internal/nntp` directly for `configTestServer`, `internal/api/config.go:418` — that precedent is why Task 5's connection test also imports `internal/nntp` directly rather than routing through `Application`; there's nothing in `Application`'s state that test needs.)
- Follow TDD: red before green, for every backend task with a testable unit.

---

### Task 1: Assembler — thread-safe article-cache usage counter

**Files:**
- Modify: `internal/assembler/assembler.go:231-243` (`Assembler` struct), `:517-553` (`dispatchRequest`)
- Test: `internal/assembler/assembler_test.go` (or wherever existing `Assembler` tests live — check for a `assembler_test.go` file; if none, create one following the package's existing test file naming)

**Interfaces:**
- Consumes: nothing new.
- Produces: `func (a *Assembler) CacheUsageBytes() int64` — thread-safe read of the write-coalescing cache's current buffered byte count. Consumed by Task 3.

**Context:** `internal/assembler/writecache.go`'s `writeCache` struct tracks `used int64` (current buffered bytes) as a **plain, non-atomic field**, explicitly documented as safe only because `writeCache` "runs exclusively on the assembler's single worker goroutine, so it requires no locking." Reading `wc.used` from any other goroutine (e.g. an HTTP handler) would be a data race. This task adds an `atomic.Int64` mirror on `Assembler` itself, updated from the one place already proven safe (the worker goroutine), so external goroutines can read it via `.Load()`.

- [ ] **Step 1: Write the failing test first**

Add this test to `internal/assembler/assembler_test.go`, modeled directly on the existing `TestTelemetryDiskWriteCountersCachedDrain` (same file, ~line 621) which already exercises the exact "articles stay buffered until file completion" path via the real `Assembler`/worker goroutine — reusing its helpers (`makeOpts`, `registerFile`, `startAssembler`) rather than inventing new test scaffolding:

```go
func TestAssembler_CacheUsageBytes_TracksBufferedBytes(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	registerFile(t, dir, files, "job1", 0, 3) // 3-part file

	opts := makeOpts(dir, files)
	opts.WriteCacheBytes = 1 << 20 // 1 MiB — caching enabled
	a := startAssembler(t, opts)

	if got := a.CacheUsageBytes(); got != 0 {
		t.Fatalf("CacheUsageBytes() before any writes = %d, want 0", got)
	}

	// Write 2 of the file's 3 parts. Both stay buffered (well under the
	// 512KB coalescing threshold) since the file isn't complete yet, so
	// CacheUsageBytes must eventually reflect the buffered bytes before
	// the 3rd (completing) write drains them to disk.
	//
	// WriteArticle only enqueues onto a channel (a.reqs <- req) and
	// returns immediately — it does not wait for the worker goroutine to
	// actually process the request. Asserting CacheUsageBytes() right
	// after the loop would race against that goroutine, so poll with a
	// bounded deadline instead of sleeping a fixed duration (no
	// time.Sleep-for-synchronization per project convention).
	for i := range 2 {
		req := WriteRequest{JobID: "job1", FileIdx: 0, Offset: int64(i * 4), Data: []byte("XXXX")}
		if err := a.WriteArticle(t.Context(), req); err != nil {
			t.Fatalf("WriteArticle: %v", err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := a.CacheUsageBytes(); got == 8 { // 2 articles * 4 bytes
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("CacheUsageBytes() did not reach 8 within 2s (got %d)", a.CacheUsageBytes())
		}
		time.Sleep(time.Millisecond)
	}

	// Complete the file (3rd part) — this drains all buffered articles.
	req := WriteRequest{JobID: "job1", FileIdx: 0, Offset: 8, Data: []byte("XXXX")}
	if err := a.WriteArticle(t.Context(), req); err != nil {
		t.Fatalf("WriteArticle: %v", err)
	}
	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := a.CacheUsageBytes(); got != 0 {
		t.Errorf("CacheUsageBytes() after drain = %d, want 0", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./internal/assembler/... -run TestAssembler_CacheUsageBytes_TracksBufferedBytes -v
```

Expected: compile error `a.CacheUsageBytes undefined` — confirms the method doesn't exist yet.

- [ ] **Step 3: Add the atomic field and getter**

In `internal/assembler/assembler.go`, add to the `Assembler` struct (near the existing `minFreeBytes atomic.Int64` field around line 239):

```go
	// cacheUsedBytes mirrors writeCache.used so it can be read safely from
	// goroutines other than the worker goroutine (writeCache itself is
	// documented as single-goroutine, no-lock). Updated after every
	// dispatchRequest call (via defer, covering processRequest and
	// wc.forget on job cancel) and again after the shutdown drain
	// (flushWriteCache in worker()), so it stays accurate through the
	// only two places writeCache.used can change.
	cacheUsedBytes atomic.Int64
```

Add the getter, placed near other public accessor methods (e.g. alongside wherever `FreeBytes` or similar read-only accessors live in this file, or right after the struct definition):

```go
// CacheUsageBytes returns the current number of bytes buffered in the
// write-coalescing cache. Safe to call from any goroutine.
func (a *Assembler) CacheUsageBytes() int64 {
	return a.cacheUsedBytes.Load()
}
```

- [ ] **Step 4: Update dispatchRequest and the shutdown drain to maintain the mirror**

In `internal/assembler/assembler.go`, modify `dispatchRequest` (around line 517) to add a `defer` as its first statement:

```go
func (a *Assembler) dispatchRequest(
	req WriteRequest,
	open map[fileKey]*openFile,
	completed map[fileKey]struct{},
	cancelledJobs map[string]struct{},
	wc *writeCache,
) int {
	defer a.cacheUsedBytes.Store(wc.used)

	if req.JobID == "" && req.FileIdx == -1 {
```

(Everything else in the function body is unchanged — the `defer` fires on every return path: the cancel-message path at `return 0` (line ~545), the already-cancelled-job path at `return 0` (line ~549), and the normal path at `return 1` (line ~552).)

`dispatchRequest` covers every mutation of `wc.used` reached through it (`processRequest`, `wc.forget`), but there is a second, separate mutation site: the shutdown-drain path in `worker()` calls `a.flushWriteCache(wc, open)` directly (not through `dispatchRequest`) when the request channel is fully drained after `stopCh` closes — around line 497:

```go
			default:
				// Drain all cached articles to disk before shutdown.
				a.flushWriteCache(wc, open)
```

Add the mirror update immediately after that call:

```go
			default:
				// Drain all cached articles to disk before shutdown.
				a.flushWriteCache(wc, open)
				a.cacheUsedBytes.Store(wc.used)
```

With both sites updated, the mirror is accurate through every place `wc.used` can change — not just the common case.

- [ ] **Step 5: Run the test to verify it passes**

```bash
go test ./internal/assembler/... -run TestAssembler_CacheUsageBytes_TracksBufferedBytes -v
```

Expected: PASS.

- [ ] **Step 6: Run the full assembler package test suite with the race detector**

```bash
go test -race ./internal/assembler/...
```

Expected: PASS — this specifically validates the race-safety claim (a concurrent goroutine calling `CacheUsageBytes()` while the worker goroutine mutates `wc.used` must not race, since only the atomic mirror is read cross-goroutine).

- [ ] **Step 7: Quality gates**

```bash
goimports -w internal/assembler/assembler.go
go vet ./internal/assembler/...
golangci-lint run ./internal/assembler/...
```

Expected: all pass.

- [ ] **Step 8: Commit**

```bash
git add internal/assembler/assembler.go internal/assembler/*_test.go
git commit -m "$(cat <<'EOF'
feat(assembler): expose thread-safe write-cache usage counter

writeCache.used is a plain int64, safe only because writeCache runs
exclusively on the assembler's worker goroutine. Add an atomic.Int64
mirror on Assembler, updated after every dispatchRequest call and
after the shutdown drain (the two places writeCache.used can change),
so other goroutines (the new status page's backend) can read current
cache usage without racing.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: Assembler — disk write-speed-test helper

**Files:**
- Modify: `internal/assembler/diskspace.go` (add alongside existing `FreeBytes`)
- Test: `internal/assembler/diskspace_test.go` (create if it doesn't exist, or extend if it does — check first: `ls internal/assembler/diskspace_test.go`)

**Interfaces:**
- Consumes: nothing new (stdlib `os`, `time`, `context` only).
- Produces: `func WriteSpeedMBPerSec(ctx context.Context, dir string, sizeBytes int64) (float64, error)`. Consumed by Task 3.

- [ ] **Step 1: Write the failing test first**

```bash
ls internal/assembler/diskspace_test.go 2>&1
```

Create (or add to) `internal/assembler/diskspace_test.go`:

```go
package assembler

import (
	"context"
	"testing"
)

func TestWriteSpeedMBPerSec_WritesAndCleansUp(t *testing.T) {
	dir := t.TempDir()

	mbPerSec, err := WriteSpeedMBPerSec(context.Background(), dir, 4*1024*1024) // 4 MiB for a fast test
	if err != nil {
		t.Fatalf("WriteSpeedMBPerSec: %v", err)
	}
	if mbPerSec <= 0 {
		t.Errorf("mbPerSec = %f, want > 0", mbPerSec)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected temp file to be cleaned up, found %d entries in %s: %v", len(entries), dir, entries)
	}
}

func TestWriteSpeedMBPerSec_NonexistentDir(t *testing.T) {
	_, err := WriteSpeedMBPerSec(context.Background(), "/nonexistent/path/that/does/not/exist", 1024)
	if err == nil {
		t.Fatal("expected error for nonexistent directory, got nil")
	}
}

func TestWriteSpeedMBPerSec_ContextCanceled(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := WriteSpeedMBPerSec(ctx, dir, 4*1024*1024)
	if err == nil {
		t.Fatal("expected error for already-cancelled context, got nil")
	}
}
```

(Add `"os"` to the test file's imports for the cleanup check.)

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/assembler/... -run TestWriteSpeedMBPerSec -v
```

Expected: compile error `undefined: WriteSpeedMBPerSec`.

- [ ] **Step 3: Implement `WriteSpeedMBPerSec`**

Add to `internal/assembler/diskspace.go`:

```go
// WriteSpeedMBPerSec writes a sizeBytes temp file into dir, times the
// write plus fsync, deletes the file, and returns the throughput in
// MB/s (decimal megabytes: bytes / 1e6 / seconds). Used by the status
// page's on-demand disk-speed-test action; ctx bounds the whole
// operation so a broken or extremely slow disk can't hang the caller
// indefinitely.
func WriteSpeedMBPerSec(ctx context.Context, dir string, sizeBytes int64) (float64, error) {
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("assembler: write speed test: %w", err)
	}

	f, err := os.CreateTemp(dir, "gonzbd-diskspeedtest-*")
	if err != nil {
		return 0, fmt.Errorf("assembler: create temp file in %s: %w", dir, err)
	}
	path := f.Name()
	defer func() {
		_ = f.Close()         //nolint:errcheck // best-effort cleanup
		_ = os.Remove(path)   //nolint:errcheck // best-effort cleanup
	}()

	buf := make([]byte, 1024*1024) // 1 MiB write chunks
	start := time.Now()
	var written int64
	for written < sizeBytes {
		if err := ctx.Err(); err != nil {
			return 0, fmt.Errorf("assembler: write speed test: %w", err)
		}
		n := int64(len(buf))
		if remaining := sizeBytes - written; remaining < n {
			n = remaining
		}
		if _, err := f.Write(buf[:n]); err != nil {
			return 0, fmt.Errorf("assembler: write speed test: write %s: %w", path, err)
		}
		written += n
	}
	if err := f.Sync(); err != nil {
		return 0, fmt.Errorf("assembler: write speed test: fsync %s: %w", path, err)
	}
	elapsed := time.Since(start)
	if elapsed <= 0 {
		return 0, fmt.Errorf("assembler: write speed test: elapsed time non-positive")
	}

	mb := float64(written) / 1_000_000
	return mb / elapsed.Seconds(), nil
}
```

Update the file's imports (currently `"fmt"` and `"syscall"` per the existing `FreeBytes` function) to add `"context"`, `"os"`, and `"time"`.

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/assembler/... -run TestWriteSpeedMBPerSec -v
```

Expected: all three PASS.

- [ ] **Step 5: Run the full package suite and quality gates**

```bash
go test -race ./internal/assembler/...
goimports -w internal/assembler/diskspace.go internal/assembler/diskspace_test.go
go vet ./internal/assembler/...
golangci-lint run ./internal/assembler/...
```

Expected: all pass.

- [ ] **Step 6: Commit**

```bash
git add internal/assembler/diskspace.go internal/assembler/diskspace_test.go
git commit -m "$(cat <<'EOF'
feat(assembler): add disk write-speed-test helper

WriteSpeedMBPerSec writes and fsyncs a fixed-size temp file, times it,
and cleans up. Bounded by ctx so a broken disk can't hang the caller.
Backs the status page's on-demand "Test Disk Speed" action.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: Application — retain binary-probe results, add status-page getters

**Files:**
- Modify: `internal/app/app.go` (`Application` struct, `New()`)
- Modify: `internal/app/reloader.go` or a new `internal/app/statusinfo.go` (new getters — use a new file since these are read-only accessors unrelated to hot-reload, keeping `reloader.go` focused on its existing responsibility)
- Modify: `internal/api/server.go:63-89` (`ApplicationReloader` interface)
- Modify: `internal/api/nopapp.go` (stub implementations)
- Test: `internal/app/statusinfo_test.go` (new), `internal/api/nopapp_test.go` (verify compile)

**Interfaces:**
- Consumes: `binaryProbe` (existing, `internal/app/stages.go:20-24`), `assembler.FreeBytes` (existing), `assembler.WriteSpeedMBPerSec`/`Assembler.CacheUsageBytes` (Tasks 1-2).
- Produces:
  ```go
  type BinaryVersions struct {
      Par2Path, Par2Version       string
      UnrarPath, UnrarVersion     string
      SevenzPath, SevenzVersion   string
  }
  func (app *Application) BinaryVersionsInfo() BinaryVersions
  func (app *Application) ArticleCacheBytes() int64
  func (app *Application) DownloadDirFreeBytes() (int64, error)
  func (app *Application) TestDownloadDirWriteSpeedMBPerSec(ctx context.Context) (float64, error)
  ```
  All four consumed by Task 4 (`status_overview`) and Task 6 (`test_disk_speed`) via the `ApplicationReloader` interface.

**Context:** `binaryProbe` (containing `Par2Caps`, `UnrarInfo`, `SevenzInfo`, each with version fields — `par2.Caps.Version`, `unpack.UnrarInfo.VersionStr`, `unpack.SevenzInfo.Version`) is currently computed once in `New()` (`internal/app/app.go:247`, `probe := probeBinaries(app.ctx, cfg, log)`) and passed straight into `buildStages` — never retained. This task stores it on `Application` for later retrieval. Binary **paths** are resolved independently via `exec.LookPath` (same as `internal/api/about.go`'s `resolveBinary` helper) — no new work needed for paths, only versions need the retained probe.

- [ ] **Step 1: Write the failing test first**

Create `internal/app/statusinfo_test.go`:

```go
package app

import (
	"context"
	"testing"
)

func TestApplication_ArticleCacheBytes_ReturnsZeroInitially(t *testing.T) {
	cfg := testConfig(t.TempDir(), t.TempDir(), t.TempDir())
	app, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer app.Shutdown()

	if got := app.ArticleCacheBytes(); got != 0 {
		t.Errorf("ArticleCacheBytes() = %d, want 0 on a fresh app", got)
	}
}

func TestApplication_DownloadDirFreeBytes_ReturnsPositiveForRealDir(t *testing.T) {
	dlDir := t.TempDir()
	cfg := testConfig(dlDir, t.TempDir(), t.TempDir())
	app, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer app.Shutdown()

	free, err := app.DownloadDirFreeBytes()
	if err != nil {
		t.Fatalf("DownloadDirFreeBytes: %v", err)
	}
	if free <= 0 {
		t.Errorf("DownloadDirFreeBytes() = %d, want > 0 for a real temp dir", free)
	}
}

func TestApplication_TestDownloadDirWriteSpeedMBPerSec_ReturnsPositive(t *testing.T) {
	dlDir := t.TempDir()
	cfg := testConfig(dlDir, t.TempDir(), t.TempDir())
	app, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer app.Shutdown()

	mbPerSec, err := app.TestDownloadDirWriteSpeedMBPerSec(context.Background())
	if err != nil {
		t.Fatalf("TestDownloadDirWriteSpeedMBPerSec: %v", err)
	}
	if mbPerSec <= 0 {
		t.Errorf("mbPerSec = %f, want > 0", mbPerSec)
	}
}

func TestApplication_BinaryVersionsInfo_StableAcrossCalls(t *testing.T) {
	cfg := testConfig(t.TempDir(), t.TempDir(), t.TempDir())
	app, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer app.Shutdown()

	// Whatever par2/unrar/7z are (or aren't) installed on the test
	// machine, BinaryVersionsInfo() must return the same retained
	// struct every call — proving it reads stored state from New()'s
	// probe rather than re-probing (which would be slow and wasteful)
	// or returning something nondeterministic.
	first := app.BinaryVersionsInfo()
	second := app.BinaryVersionsInfo()
	if first != second {
		t.Errorf("BinaryVersionsInfo() not stable across calls: %+v vs %+v", first, second)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/app/... -run 'TestApplication_ArticleCacheBytes|TestApplication_DownloadDirFreeBytes|TestApplication_TestDownloadDirWriteSpeedMBPerSec|TestApplication_BinaryVersionsInfo' -v
```

Expected: compile errors — none of these four methods exist yet.

- [ ] **Step 3: Add retained probe fields and the four getters**

In `internal/app/app.go`, add to the `Application` struct (near other post-construction-immutable fields, e.g. near `version string`):

```go
	// binaryVersions is populated once in New() from the startup probe
	// and never mutated afterward — safe to read from any goroutine
	// without synchronization (same pattern as the immutable version field).
	binaryVersions BinaryVersions
```

Add the exported type and getter to the new file `internal/app/statusinfo.go`:

```go
package app

import (
	"context"

	"github.com/hobeone/gonzbd/internal/config"
)

// BinaryVersions holds the resolved version strings for external
// post-processing tools, captured once at startup. Paths are not
// included here — callers resolve those independently via exec.LookPath
// (see internal/api/about.go's resolveBinary), since path resolution
// doesn't require the startup probe.
type BinaryVersions struct {
	Par2Version   string
	UnrarVersion  string
	SevenzVersion string
}

// BinaryVersionsInfo returns the external tool version strings captured
// by the startup probe. Safe to call from any goroutine.
func (app *Application) BinaryVersionsInfo() BinaryVersions {
	return app.binaryVersions
}

// ArticleCacheBytes returns the current number of bytes buffered in the
// post-processing pipeline's write-coalescing cache. Safe to call from
// any goroutine.
func (app *Application) ArticleCacheBytes() int64 {
	if app.assembler == nil {
		return 0
	}
	return app.assembler.CacheUsageBytes()
}

// DownloadDirFreeBytes returns the free bytes available on the
// filesystem containing the configured download directory.
func (app *Application) DownloadDirFreeBytes() (int64, error) {
	var dlDir string
	app.config.WithRead(func(c *config.Config) {
		dlDir = c.General.DownloadDir
	})
	return assembler.FreeBytes(dlDir)
}

// TestDownloadDirWriteSpeedMBPerSec runs a bounded disk write-speed test
// against the configured download directory. Backs the status page's
// on-demand "Test Disk Speed" action.
func (app *Application) TestDownloadDirWriteSpeedMBPerSec(ctx context.Context) (float64, error) {
	var dlDir string
	app.config.WithRead(func(c *config.Config) {
		dlDir = c.General.DownloadDir
	})
	const testSizeBytes = 64 * 1024 * 1024 // 64 MiB
	return assembler.WriteSpeedMBPerSec(ctx, dlDir, testSizeBytes)
}
```

Add `"github.com/hobeone/gonzbd/internal/assembler"` to this new file's imports (needed for `assembler.FreeBytes`/`assembler.WriteSpeedMBPerSec`).

In `internal/app/app.go`, right after the existing `probe := probeBinaries(app.ctx, cfg, log)` call (line ~247), add:

```go
	app.binaryVersions = BinaryVersions{
		Par2Version:   probe.Par2Caps.Version,
		UnrarVersion:  probe.UnrarInfo.VersionStr,
		SevenzVersion: probe.SevenzInfo.Version,
	}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/app/... -run 'TestApplication_ArticleCacheBytes|TestApplication_DownloadDirFreeBytes|TestApplication_TestDownloadDirWriteSpeedMBPerSec|TestApplication_BinaryVersionsInfo' -v
```

Expected: all PASS.

- [ ] **Step 5: Add the four methods to `ApplicationReloader` and `NopApp`**

In `internal/api/server.go`, add to the `ApplicationReloader` interface (`:63-89`), e.g. after `DirectUnpackStatus`:

```go
	// BinaryVersionsInfo returns resolved external-tool version strings
	// captured at startup, for the status page.
	BinaryVersionsInfo() app.BinaryVersions
	// ArticleCacheBytes returns current write-cache usage, for the status page.
	ArticleCacheBytes() int64
	// DownloadDirFreeBytes returns free disk space on the download directory.
	DownloadDirFreeBytes() (int64, error)
	// TestDownloadDirWriteSpeedMBPerSec runs an on-demand disk write-speed test.
	TestDownloadDirWriteSpeedMBPerSec(ctx context.Context) (float64, error)
```

Add the import `"github.com/hobeone/gonzbd/internal/app"` to `server.go` (check it isn't already imported under a different alias — it currently is not, per the file's existing import list).

In `internal/api/nopapp.go`, add stub implementations:

```go
// BinaryVersionsInfo is a stub.
func (n NopApp) BinaryVersionsInfo() app.BinaryVersions { return app.BinaryVersions{} }

// ArticleCacheBytes is a stub.
func (n NopApp) ArticleCacheBytes() int64 { return 0 }

// DownloadDirFreeBytes is a stub.
func (n NopApp) DownloadDirFreeBytes() (int64, error) { return 0, nil }

// TestDownloadDirWriteSpeedMBPerSec is a stub.
func (n NopApp) TestDownloadDirWriteSpeedMBPerSec(context.Context) (float64, error) { return 0, nil }
```

Add `"github.com/hobeone/gonzbd/internal/app"` to `nopapp.go`'s imports.

- [ ] **Step 6: Verify no other `ApplicationReloader` implementer breaks**

```bash
go build ./...
```

Expected: succeeds. (Per research during planning, only `nopapp.go` implements this interface directly in production code; `internal/api/config_test.go`'s `setConfigSpyApp` embeds `NopApp`, so it inherits the new stub methods automatically with no changes needed. If this build step surfaces any other implementer, add the same stub pattern there.)

- [ ] **Step 7: Run the full app and api package suites**

```bash
go test -race ./internal/app/... ./internal/api/...
```

Expected: PASS.

- [ ] **Step 8: Quality gates**

```bash
goimports -w internal/app/app.go internal/app/statusinfo.go internal/app/statusinfo_test.go internal/api/server.go internal/api/nopapp.go
go vet ./internal/app/... ./internal/api/...
golangci-lint run ./internal/app/... ./internal/api/...
```

Expected: all pass.

- [ ] **Step 9: Commit**

```bash
git add internal/app/app.go internal/app/statusinfo.go internal/app/statusinfo_test.go internal/api/server.go internal/api/nopapp.go
git commit -m "$(cat <<'EOF'
feat(app): expose binary versions, cache usage, and disk stats for status page

Retain the startup binary probe's version strings (previously
discarded after buildStages), and add getters for article-cache usage,
download-dir free space, and an on-demand disk write-speed test.
Wired through ApplicationReloader so internal/api keeps talking only
to internal/app, not directly to internal/assembler.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: API — `mode=status_overview` handler

**Files:**
- Create: `internal/api/statusoverview.go`
- Modify: `internal/api/server.go` (add `startTime time.Time` field, set in `New()`)
- Modify: `internal/api/router.go:100-103` (register the new mode)
- Test: `internal/api/statusoverview_test.go`

**Interfaces:**
- Consumes: `ApplicationReloader.BinaryVersionsInfo()`, `.ArticleCacheBytes()`, `.DownloadDirFreeBytes()` (Task 3); `s.config`, `s.version`/`s.commit`/`s.date` (existing `Server` fields); `resolveBinary`, `localIPv4` (existing, `internal/api/about.go`).
- Produces: `mode=status_overview` HTTP response per the spec's documented shape (below). Consumed by Task 8 (UI).

- [ ] **Step 1: Write the failing test first**

Create `internal/api/statusoverview_test.go`:

```go
package api

import (
	"net/http"
	"testing"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
)

type statusOverviewSpyApp struct {
	NopApp
	binaryVersions   app.BinaryVersions
	articleCache     int64
	downloadDirFree  int64
	downloadDirErr   error
}

func (a *statusOverviewSpyApp) BinaryVersionsInfo() app.BinaryVersions { return a.binaryVersions }
func (a *statusOverviewSpyApp) ArticleCacheBytes() int64               { return a.articleCache }
func (a *statusOverviewSpyApp) DownloadDirFreeBytes() (int64, error) {
	return a.downloadDirFree, a.downloadDirErr
}

func TestModeStatusOverview_ReturnsGeneralAndSystemSections(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	cfg.With(func(c *config.Config) {
		c.General.APIKey = testAPIKey
		c.General.DownloadDir = "/tmp/downloads"
		c.Downloads.MinFreeSpace = 1024 * 1024 * 1024
	})
	spy := &statusOverviewSpyApp{
		binaryVersions:  app.BinaryVersions{Par2Version: "1.0", UnrarVersion: "6.24", SevenzVersion: "23.01"},
		articleCache:    12345,
		downloadDirFree: 987654321,
	}
	s := New(Options{Version: "v1.2.0", Commit: "abc123", Config: cfg, App: spy})

	rr := apiGet(t, s.Handler(), "/api?mode=status_overview&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	m := decodeJSON(t, rr)

	general, ok := m["general"].(map[string]any)
	if !ok {
		t.Fatalf("expected general section, got %v", m)
	}
	if general["version"] != "v1.2.0" {
		t.Errorf("general.version = %v; want v1.2.0", general["version"])
	}
	unrar, ok := general["unrar"].(map[string]any)
	if !ok || unrar["version"] != "6.24" {
		t.Errorf("general.unrar.version = %v; want 6.24", general["unrar"])
	}

	system, ok := m["system"].(map[string]any)
	if !ok {
		t.Fatalf("expected system section, got %v", m)
	}
	if system["article_cache_bytes"].(float64) != 12345 {
		t.Errorf("system.article_cache_bytes = %v; want 12345", system["article_cache_bytes"])
	}
	if system["download_dir_free_bytes"].(float64) != 987654321 {
		t.Errorf("system.download_dir_free_bytes = %v; want 987654321", system["download_dir_free_bytes"])
	}
	if system["min_free_space_bytes"].(float64) != 1024*1024*1024 {
		t.Errorf("system.min_free_space_bytes = %v; want %d", system["min_free_space_bytes"], 1024*1024*1024)
	}
}

func TestModeStatusOverview_RequiresAuth(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	cfg.With(func(c *config.Config) { c.General.APIKey = testAPIKey })
	s := New(Options{Config: cfg, App: &statusOverviewSpyApp{}})

	rr := apiGet(t, s.Handler(), "/api?mode=status_overview")
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401 without apikey", rr.Code)
	}
}
```

(Verify `apiGet`, `decodeJSON`, and `testAPIKey` are the correct existing test helper names in this package by checking `internal/api/config_test.go` or `internal/api/about_test.go` — reuse whatever helpers already exist rather than reinventing them.)

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./internal/api/... -run TestModeStatusOverview -v
```

Expected: compile error or "unknown mode: status_overview" (400) — the mode doesn't exist yet.

- [ ] **Step 3: Add `startTime` to `Server` and set it in `New()`**

In `internal/api/server.go`, add to the `Server` struct (near `version string`):

```go
	startTime time.Time
```

Add `"time"` to `server.go`'s imports. In `New()`, add (right after `s := &Server{...}` construction, or as a field in that literal):

```go
		startTime: time.Now(),
```

- [ ] **Step 4: Implement `modeStatusOverview`**

Create `internal/api/statusoverview.go`:

```go
package api

import (
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/unpack"
)

// modeStatusOverview returns the aggregate General Info + System Info
// snapshot for the status page. Deliberately excludes the GitHub update
// check (see modeStatusCheckUpdate) so a slow/unreachable network call
// never delays this handler, and excludes per-server data (already
// available, live, via mode=server_stats).
func (s *Server) modeStatusOverview(w http.ResponseWriter, _ *http.Request) {
	hostname, _ := os.Hostname()

	var (
		downloadDir, completeDir, adminDir, scriptDir, logDir string
		par2Cmd, unrarCmd, sevenzCmd                           string
		minFreeSpace                                          int64
	)
	if s.config != nil {
		s.config.WithRead(func(cfg *config.Config) {
			downloadDir = cfg.General.DownloadDir
			completeDir = cfg.General.CompleteDir
			adminDir = cfg.General.AdminDir
			scriptDir = cfg.General.ScriptDir
			logDir = cfg.General.LogDir
			par2Cmd = cfg.PostProc.Par2Command
			unrarCmd = cfg.PostProc.UnrarCommand
			sevenzCmd = cfg.PostProc.SevenzCommand
			minFreeSpace = int64(cfg.Downloads.MinFreeSpace)
		})
	}

	var articleCacheBytes int64
	var downloadDirFreeBytes int64
	var binVersions struct{ Par2, Unrar, Sevenz string }
	if s.app != nil {
		articleCacheBytes = s.app.ArticleCacheBytes()
		if free, err := s.app.DownloadDirFreeBytes(); err == nil {
			downloadDirFreeBytes = free
		} else {
			s.log.Warn("status_overview: download dir free bytes", "error", err)
		}
		bv := s.app.BinaryVersionsInfo()
		binVersions.Par2, binVersions.Unrar, binVersions.Sevenz = bv.Par2Version, bv.UnrarVersion, bv.SevenzVersion
	}

	general := map[string]any{
		"version":      s.version,
		"commit":       s.commit,
		"build_date":   s.date,
		"go_version":   runtime.Version(),
		"uptime_seconds": int64(time.Since(s.startTime).Seconds()),
		"hostname":     hostname,
		"local_ip":     localIPv4(),
		"config_path":  s.configPath,
		"download_dir": downloadDir,
		"complete_dir": completeDir,
		"admin_dir":    adminDir,
		"script_dir":   scriptDir,
		"log_dir":      logDir,
		"par2":         map[string]any{"path": resolveBinary(par2Cmd, "par2"), "version": binVersions.Par2},
		"unrar":        map[string]any{"path": resolveBinary(unrarCmd, "unrar"), "version": binVersions.Unrar},
		"sevenzip":     map[string]any{"path": resolveBinary(sevenzCmd, unpack.SevenZipBinaries...), "version": binVersions.Sevenz},
	}

	system := map[string]any{
		"os":                      runtime.GOOS,
		"arch":                    runtime.GOARCH,
		"article_cache_bytes":     articleCacheBytes,
		"download_dir_free_bytes": downloadDirFreeBytes,
		"min_free_space_bytes":    minFreeSpace,
	}

	respondJSON(w, http.StatusOK, map[string]any{
		"status":  true,
		"general": general,
		"system":  system,
	})
}
```

(Public IP is deliberately excluded from this handler — the spec was amended during planning to reflect this decision, see its General Info section: `about.go`'s public-IP lookup takes up to ~3s via two concurrent external HTTP calls, and carrying that same cost into every status-page load/refresh would reintroduce exactly the kind of avoidable external-network latency the separate `check_update` endpoint (Task 7) exists to avoid. This is settled scope, not a gap to fill in — do not add it back into this handler.)

- [ ] **Step 5: Register the mode**

In `internal/api/router.go`, add to `registerModes()`'s "Status modes" group (`:99-103`):

```go
		"status_overview": {handler: s.modeStatusOverview, level: LevelProtected},
```

- [ ] **Step 6: Run the tests to verify they pass**

```bash
go test ./internal/api/... -run TestModeStatusOverview -v
```

Expected: both PASS.

- [ ] **Step 7: Run the full api package suite and quality gates**

```bash
go test -race ./internal/api/...
goimports -w internal/api/statusoverview.go internal/api/statusoverview_test.go internal/api/server.go internal/api/router.go
go vet ./internal/api/...
golangci-lint run ./internal/api/...
```

Expected: all pass.

- [ ] **Step 8: Commit**

```bash
git add internal/api/statusoverview.go internal/api/statusoverview_test.go internal/api/server.go internal/api/router.go
git commit -m "$(cat <<'EOF'
feat(api): add mode=status_overview for the status page

New gonzbd-only mode aggregating General Info (version, paths, binary
versions, uptime) and System Info (OS/arch, article cache usage, disk
free space) in one call. Deliberately excludes per-server data
(already live via mode=server_stats) and the GitHub update check
(its own mode, see follow-up commit) to keep this handler's latency
independent of any external network call.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 5: API — `mode=status&name=test_connection` sub-action

**Files:**
- Create: `internal/api/statustests.go`
- Modify: `internal/api/status.go:48-56` (`modeStatus`'s switch)
- Test: `internal/api/statustests_test.go`

**Interfaces:**
- Consumes: `s.config` (existing), `internal/nntp.Dial`, `internal/nntp.ErrServerUnavailable` (existing).
- Produces: HTTP response for `mode=status&name=test_connection&value=<server_name>`. Consumed by Task 9 (UI).

**Context:** `internal/api/config.go:375-435`'s `configTestServer` already dials an ad-hoc `config.ServerConfig` built from raw form fields (used to test *unsaved* candidate credentials before saving a new server). This task follows the identical `nntp.Dial` + timeout + result-envelope pattern, but looks up an **existing, already-configured** server by name instead of accepting raw host/port/credentials, and additionally classifies 502/503 responses as "likely connection limit" per the spec.

- [ ] **Step 1: Write the failing test first**

Create `internal/api/statustests_test.go`:

```go
package api

import (
	"net/http"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
)

func TestModeStatus_TestConnection_UnknownServer(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	cfg.With(func(c *config.Config) { c.General.APIKey = testAPIKey })
	s := New(Options{Config: cfg})

	rr := apiGet(t, s.Handler(), "/api?mode=status&name=test_connection&value=nonexistent&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	m := decodeJSON(t, rr)
	result, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected result object, got %v", m)
	}
	if result["ok"] != false {
		t.Errorf("result.ok = %v; want false for unknown server", result["ok"])
	}
}

func TestModeStatus_TestConnection_MissingValue(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	cfg.With(func(c *config.Config) { c.General.APIKey = testAPIKey })
	s := New(Options{Config: cfg})

	rr := apiGet(t, s.Handler(), "/api?mode=status&name=test_connection&apikey="+testAPIKey)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 when value (server name) is missing", rr.Code)
	}
}

func TestModeStatus_TestConnection_UnreachableHost(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	cfg.With(func(c *config.Config) {
		c.General.APIKey = testAPIKey
		c.Servers = []config.ServerConfig{{
			Name: "s1", Host: "127.0.0.1", Port: 1, // port 1 should refuse/timeout quickly
			Connections: 1, Timeout: 2,
		}}
	})
	s := New(Options{Config: cfg})

	rr := apiGet(t, s.Handler(), "/api?mode=status&name=test_connection&value=s1&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	m := decodeJSON(t, rr)
	result := m["result"].(map[string]any)
	if result["ok"] != false {
		t.Errorf("result.ok = %v; want false for unreachable host", result["ok"])
	}
	if result["error"] == nil || result["error"] == "" {
		t.Error("expected a non-empty error message")
	}
}

// TestModeStatus_TestConnection_ServerUnavailableSetsLikelyConnectionLimit
// proves the likely_connection_limit classification, which none of the
// other tests exercise (they hit "connection refused"/"not found", never
// a 502/503 NNTP response). Stands up a minimal raw TCP listener that
// writes a 502 greeting — the exact response most providers send when
// an account's connection limit is already in use — instead of using
// internal/nntp/nntptest's Scripted server, which always sends a
// successful 200/201 greeting (it's built for article-fetch scenarios,
// not greeting-rejection ones).
func TestModeStatus_TestConnection_ServerUnavailableSetsLikelyConnectionLimit(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed by test cleanup; nothing to serve
		}
		defer func() { _ = conn.Close() }()
		_, _ = conn.Write([]byte("502 Too many connections from your account\r\n"))
	}()

	host, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("Atoi(%q): %v", portStr, err)
	}

	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	cfg.With(func(c *config.Config) {
		c.General.APIKey = testAPIKey
		c.Servers = []config.ServerConfig{{
			Name: "s1", Host: host, Port: port, Connections: 1, Timeout: 5,
		}}
	})
	s := New(Options{Config: cfg})

	rr := apiGet(t, s.Handler(), "/api?mode=status&name=test_connection&value=s1&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	m := decodeJSON(t, rr)
	result := m["result"].(map[string]any)
	if result["ok"] != false {
		t.Errorf("result.ok = %v; want false", result["ok"])
	}
	if result["likely_connection_limit"] != true {
		t.Errorf("result.likely_connection_limit = %v; want true for a 502 greeting", result["likely_connection_limit"])
	}
}
```

(Add `"net"` and `"strconv"` to this test file's imports.)

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/api/... -run TestModeStatus_TestConnection -v
```

Expected: `unknown status action: test_connection` (400 via the existing `default:` case in `modeStatus`'s switch) — confirms the sub-action doesn't exist yet.

- [ ] **Step 3: Implement the sub-action**

Create `internal/api/statustests.go`:

```go
package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/nntp"
)

// testConnectionTimeout bounds the on-demand connection test — independent
// of the server's configured Timeout, since a broken/maxed-out server
// shouldn't make the test hang for the user's full configured timeout
// (which can be 60s+).
const testConnectionTimeout = 10 * time.Second

// statusTestConnection dials an existing, already-configured server by
// name (value= parameter) and reports success/failure. A 502/503
// response is flagged as likely_connection_limit: true, since that's
// the response most providers return when an account's connection
// limit is already in use by active downloads — a common, often
// benign cause distinct from a real configuration problem.
func (s *Server) statusTestConnection(w http.ResponseWriter, r *http.Request) {
	name := formString(r, "value")
	if name == "" {
		s.respondError(w, http.StatusBadRequest, "missing value parameter (server name)")
		return
	}

	var target *config.ServerConfig
	if s.config != nil {
		s.config.WithRead(func(cfg *config.Config) {
			for i := range cfg.Servers {
				if cfg.Servers[i].Name == name {
					sc := cfg.Servers[i]
					target = &sc
					return
				}
			}
		})
	}
	if target == nil {
		respondOK(w, "result", map[string]any{"ok": false, "error": "server not found: " + name})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), testConnectionTimeout)
	defer cancel()

	start := time.Now()
	conn, err := nntp.Dial(ctx, *target, nntp.WithLogger(s.log))
	if err != nil {
		s.log.Warn("test_connection failed", "server", name, "error", err)
		respondOK(w, "result", map[string]any{
			"ok":                      false,
			"error":                   err.Error(),
			"likely_connection_limit": errors.Is(err, nntp.ErrServerUnavailable),
		})
		return
	}
	latency := time.Since(start)
	_ = conn.Close() //nolint:errcheck // test connection; close error is irrelevant

	s.log.Info("test_connection passed", "server", name, "latency_ms", latency.Milliseconds())
	respondOK(w, "result", map[string]any{
		"ok":         true,
		"latency_ms": latency.Milliseconds(),
	})
}
```

In `internal/api/status.go`, add a new `case` to `modeStatus`'s switch (`:48-56`):

```go
	switch action {
	case "unblock_server":
		s.statusUnblockServer(w, r)
	case "test_connection":
		s.statusTestConnection(w, r)
	// Stubbed sub-actions: not yet implemented.
	case "delete_orphan", "delete_all_orphan", "add_orphan", "add_all_orphan":
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/api/... -run TestModeStatus_TestConnection -v
```

Expected: all four PASS. (The unreachable-host test connecting to `127.0.0.1:1` should fail fast — typically "connection refused" — well within the 10s bound; if it's flaky in CI due to environment-specific port behavior, that's a signal to adjust the test's target, not the production timeout. The new 502-greeting test's raw listener should also respond well within the timeout since it's local.)

- [ ] **Step 5: Run the full api package suite and quality gates**

```bash
go test -race ./internal/api/...
goimports -w internal/api/statustests.go internal/api/statustests_test.go internal/api/status.go
go vet ./internal/api/...
golangci-lint run ./internal/api/...
```

Expected: all pass.

- [ ] **Step 6: Commit**

```bash
git add internal/api/statustests.go internal/api/statustests_test.go internal/api/status.go
git commit -m "$(cat <<'EOF'
feat(api): add mode=status&name=test_connection sub-action

Dials an existing configured server by name (following the same
nntp.Dial pattern already used by configTestServer for unsaved
candidate servers) and reports success/latency or failure. A 502/503
response is flagged likely_connection_limit: true, since that's what
most providers return when an account's connection limit is already
in use by active downloads.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 6: API — `mode=status&name=test_disk_speed` sub-action

**Files:**
- Modify: `internal/api/statustests.go` (add handler)
- Modify: `internal/api/status.go` (add `case`)
- Test: `internal/api/statustests_test.go` (extend)

**Interfaces:**
- Consumes: `ApplicationReloader.TestDownloadDirWriteSpeedMBPerSec(ctx)` (Task 3).
- Produces: HTTP response for `mode=status&name=test_disk_speed`. Consumed by Task 10 (UI).

- [ ] **Step 1: Write the failing test first**

Add to `internal/api/statustests_test.go`:

```go
type diskSpeedSpyApp struct {
	NopApp
	mbPerSec float64
	err      error
}

func (a *diskSpeedSpyApp) TestDownloadDirWriteSpeedMBPerSec(ctx context.Context) (float64, error) {
	return a.mbPerSec, a.err
}

func TestModeStatus_TestDiskSpeed_Success(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	cfg.With(func(c *config.Config) { c.General.APIKey = testAPIKey })
	s := New(Options{Config: cfg, App: &diskSpeedSpyApp{mbPerSec: 210.4}})

	rr := apiGet(t, s.Handler(), "/api?mode=status&name=test_disk_speed&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	m := decodeJSON(t, rr)
	result := m["result"].(map[string]any)
	if result["mb_per_sec"] != 210.4 {
		t.Errorf("result.mb_per_sec = %v; want 210.4", result["mb_per_sec"])
	}
}

func TestModeStatus_TestDiskSpeed_Error(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	cfg.With(func(c *config.Config) { c.General.APIKey = testAPIKey })
	s := New(Options{Config: cfg, App: &diskSpeedSpyApp{err: errors.New("permission denied")}})

	rr := apiGet(t, s.Handler(), "/api?mode=status&name=test_disk_speed&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	m := decodeJSON(t, rr)
	result := m["result"].(map[string]any)
	if result["ok"] != false {
		t.Errorf("result.ok = %v; want false on write error", result["ok"])
	}
}
```

(Add `"context"` and `"errors"` to this test file's imports if not already present from Task 5.)

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/api/... -run TestModeStatus_TestDiskSpeed -v
```

Expected: `unknown status action: test_disk_speed`.

- [ ] **Step 3: Implement the handler**

Add to `internal/api/statustests.go`:

```go
// testDiskSpeedTimeout bounds the on-demand disk-speed test so a
// genuinely broken disk can't hang the request indefinitely.
const testDiskSpeedTimeout = 10 * time.Second

// statusTestDiskSpeed runs a bounded write-speed test against the
// configured download directory.
func (s *Server) statusTestDiskSpeed(w http.ResponseWriter, r *http.Request) {
	if s.app == nil {
		s.respondError(w, http.StatusInternalServerError, "app not wired")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), testDiskSpeedTimeout)
	defer cancel()

	mbPerSec, err := s.app.TestDownloadDirWriteSpeedMBPerSec(ctx)
	if err != nil {
		s.log.Warn("test_disk_speed failed", "error", err)
		respondOK(w, "result", map[string]any{"ok": false, "error": err.Error()})
		return
	}
	respondOK(w, "result", map[string]any{"ok": true, "mb_per_sec": mbPerSec})
}
```

In `internal/api/status.go`'s switch, add:

```go
	case "test_disk_speed":
		s.statusTestDiskSpeed(w, r)
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/api/... -run TestModeStatus_TestDiskSpeed -v
```

Expected: both PASS.

- [ ] **Step 5: Run the full api package suite and quality gates**

```bash
go test -race ./internal/api/...
goimports -w internal/api/statustests.go internal/api/statustests_test.go internal/api/status.go
go vet ./internal/api/...
golangci-lint run ./internal/api/...
```

Expected: all pass.

- [ ] **Step 6: Commit**

```bash
git add internal/api/statustests.go internal/api/statustests_test.go internal/api/status.go
git commit -m "$(cat <<'EOF'
feat(api): add mode=status&name=test_disk_speed sub-action

Bounded on-demand disk write-speed test against the download
directory, via Application.TestDownloadDirWriteSpeedMBPerSec.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 7: API — `mode=status&name=check_update` sub-action

**Files:**
- Create: `internal/api/versioncheck.go`
- Modify: `internal/api/status.go` (add `case`)
- Test: `internal/api/versioncheck_test.go`

**Interfaces:**
- Consumes: nothing new (stdlib `net/http`, `strconv`, `strings` only — no new dependency, see Global Constraints on why `golang.org/x/mod/semver` is deliberately not used).
- Produces: HTTP response for `mode=status&name=check_update`. Consumed by Task 8 (UI, fetched independently/in parallel with `status_overview`).

**Context:** gonzbd's version tags are simple `vMAJOR.MINOR.PATCH` (confirmed: `v1.0.1` .. `v1.2.0`, no prerelease/build-metadata suffixes in this repo's tag history), so a small hand-rolled comparison is sufficient and avoids promoting `golang.org/x/mod` from a transitive to a direct dependency for a need this narrow.

- [ ] **Step 1: Write the failing test first**

Create `internal/api/versioncheck_test.go`:

```go
package api

import "testing"

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		current, latest string
		want             int // -1 current<latest, 0 equal, 1 current>latest
	}{
		{"v1.2.0", "v1.2.0", 0},
		{"v1.2.0", "v1.3.0", -1},
		{"v1.2.0", "v1.1.9", 1},
		{"v1.2.0", "v2.0.0", -1},
		{"v1.9.0", "v1.10.0", -1}, // numeric, not lexicographic, comparison
		{"v1.2", "v1.2.0", 0},     // missing patch defaults to 0
		{"v1.2.1-2b7f9150", "v1.2.1", 0}, // strips build metadata
		{"v1.2.0-rc1", "v1.2.0", 0},      // strips prerelease suffixes
	}
	for _, tc := range tests {
		t.Run(tc.current+"_vs_"+tc.latest, func(t *testing.T) {
			got := compareVersions(tc.current, tc.latest)
			if got != tc.want {
				t.Errorf("compareVersions(%q, %q) = %d, want %d", tc.current, tc.latest, got, tc.want)
			}
		})
	}
}

func TestCompareVersions_InvalidInput(t *testing.T) {
	// Malformed versions must not panic; exact return value for garbage
	// input is not asserted beyond "does not panic."
	_ = compareVersions("not-a-version", "v1.2.0")
	_ = compareVersions("v1.2.0", "also-not-a-version")
}

func TestModeStatus_CheckUpdate_DevBuildSkipsNetworkCall(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	cfg.With(func(c *config.Config) { c.General.APIKey = testAPIKey })
	s := New(Options{Version: "dev", Config: cfg})

	rr := apiGet(t, s.Handler(), "/api?mode=status&name=check_update&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	m := decodeJSON(t, rr)
	result := m["result"].(map[string]any)
	if result["status"] != "unknown" {
		t.Errorf("result.status = %v; want unknown for a dev build", result["status"])
	}
}

// TestModeStatus_CheckUpdate_StatusMapping exercises statusCheckUpdate's
// full status-mapping logic (up_to_date / update_available / unknown on
// error) end-to-end via HTTP, using an httptest.Server in place of the
// real GitHub API by overriding the githubLatestReleaseURL var. Without
// this, only compareVersions itself (a pure function) would be tested,
// leaving the 3-line mapping in statusCheckUpdate uncovered.
func TestModeStatus_CheckUpdate_StatusMapping(t *testing.T) {
	tests := []struct {
		name           string
		runningVersion string
		latestTag      string
		serverStatus   int
		wantStatus     string
		wantLatest     string
	}{
		{"up to date", "v1.2.0", "v1.2.0", http.StatusOK, "up_to_date", "v1.2.0"},
		{"update available", "v1.2.0", "v1.3.0", http.StatusOK, "update_available", "v1.3.0"},
		{"github error status", "v1.2.0", "", http.StatusInternalServerError, "unknown", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.serverStatus)
				if tc.serverStatus == http.StatusOK {
					_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": tc.latestTag})
				}
			}))
			defer github.Close()

			origURL := githubLatestReleaseURL
			githubLatestReleaseURL = github.URL
			defer func() { githubLatestReleaseURL = origURL }()

			cfg, err := config.Default()
			if err != nil {
				t.Fatalf("Default(): %v", err)
			}
			cfg.With(func(c *config.Config) { c.General.APIKey = testAPIKey })
			s := New(Options{Version: tc.runningVersion, Config: cfg})

			rr := apiGet(t, s.Handler(), "/api?mode=status&name=check_update&apikey="+testAPIKey)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
			}
			m := decodeJSON(t, rr)
			result := m["result"].(map[string]any)
			if result["status"] != tc.wantStatus {
				t.Errorf("result.status = %v; want %v", result["status"], tc.wantStatus)
			}
			if tc.wantLatest != "" && result["latest_version"] != tc.wantLatest {
				t.Errorf("result.latest_version = %v; want %v", result["latest_version"], tc.wantLatest)
			}
		})
	}
}
```

(Add `"net/http"`, `"net/http/httptest"`, `"encoding/json"`, and `"github.com/hobeone/gonzbd/internal/config"` to this test file's imports. Note this test cannot use `t.Parallel()` at the outer level since it mutates the shared package-level `githubLatestReleaseURL` var — the subtests run sequentially, which is fine since each restores the var via `defer` before the next begins.)

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/api/... -run 'TestCompareVersions|TestModeStatus_CheckUpdate' -v
```

Expected: compile error — `compareVersions` doesn't exist, and `check_update` is an unknown status action.

- [ ] **Step 3: Implement `compareVersions` and the handler**

Create `internal/api/versioncheck.go`:

```go
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// checkUpdateTimeout bounds the GitHub API call so an unreachable or
// slow GitHub never blocks the caller for long. This handler is
// deliberately separate from mode=status_overview and fetched
// independently by the UI so it can't add latency to the rest of the
// status page.
const checkUpdateTimeout = 3 * time.Second

// githubLatestReleaseURL is a var, not a const, specifically so tests can
// point it at a local httptest.Server instead of the real GitHub API —
// the same test-seam pattern already used elsewhere in this codebase
// (e.g. internal/cmdutil's `var lookPath = exec.LookPath`). Without this,
// statusCheckUpdate's status-mapping logic (up_to_date/update_available)
// would be untestable without hitting the real network.
var githubLatestReleaseURL = "https://api.github.com/repos/hobeone/gonzbd/releases/latest"

// statusCheckUpdate compares the running build's version against the
// latest GitHub release. Reports status "unknown" (never an HTTP error)
// on any failure: dev build, network error, timeout, or non-2xx
// response — this is informational only, never load-bearing.
func (s *Server) statusCheckUpdate(w http.ResponseWriter, r *http.Request) {
	if s.version == "" || s.version == "dev" {
		respondOK(w, "result", map[string]any{"status": "unknown"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), checkUpdateTimeout)
	defer cancel()

	latest, err := fetchLatestGithubRelease(ctx, "gonzbd/"+s.version)
	if err != nil {
		s.log.Debug("check_update: github fetch failed", "error", err)
		respondOK(w, "result", map[string]any{"status": "unknown"})
		return
	}

	cmp := compareVersions(s.version, latest)
	status := "up_to_date"
	if cmp < 0 {
		status = "update_available"
	}
	respondOK(w, "result", map[string]any{"status": status, "latest_version": latest})
}

// fetchLatestGithubRelease queries the GitHub releases API for this
// repo's latest release tag. No caching: this only fires when a human
// opens/refreshes the status page, so GitHub's unauthenticated rate
// limit (60 req/hr/IP) is not a practical concern.
func fetchLatestGithubRelease(ctx context.Context, userAgent string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubLatestReleaseURL, http.NoBody)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort cleanup

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github releases API: status %d", resp.StatusCode)
	}

	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	return body.TagName, nil
}

// compareVersions compares two "vMAJOR.MINOR.PATCH"-style version
// strings numerically (not lexicographically — "v1.9.0" < "v1.10.0").
// Returns -1 if a < b, 0 if equal, 1 if a > b. Malformed components
// parse as 0, so garbage input degrades gracefully rather than
// panicking (this is informational-only display logic, not a security
// or correctness boundary).
func compareVersions(a, b string) int {
	pa := parseVersionParts(a)
	pb := parseVersionParts(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func parseVersionParts(v string) [3]int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if idx := strings.IndexAny(v, "-+"); idx != -1 {
		v = v[:idx]
	}
	parts := strings.SplitN(v, ".", 3)
	var out [3]int
	for i := 0; i < len(parts) && i < 3; i++ {
		n, _ := strconv.Atoi(parts[i]) //nolint:errcheck // malformed input defaults to 0, see doc comment
		out[i] = n
	}
	return out
}
```

Add `"fmt"` to this file's imports (used in `fetchLatestGithubRelease`).

In `internal/api/status.go`'s switch, add:

```go
	case "check_update":
		s.statusCheckUpdate(w, r)
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/api/... -run 'TestCompareVersions|TestModeStatus_CheckUpdate' -v
```

Expected: all PASS.

- [ ] **Step 5: Run the full api package suite and quality gates**

```bash
go test -race ./internal/api/...
goimports -w internal/api/versioncheck.go internal/api/versioncheck_test.go internal/api/status.go
go vet ./internal/api/...
golangci-lint run ./internal/api/...
```

Expected: all pass.

- [ ] **Step 6: Verify `go.mod` gained no new dependency**

```bash
git diff go.mod go.sum
```

Expected: no output — this task must not add `golang.org/x/mod` or any other new module.

- [ ] **Step 7: Commit**

```bash
git add internal/api/versioncheck.go internal/api/versioncheck_test.go internal/api/status.go
git commit -m "$(cat <<'EOF'
feat(api): add mode=status&name=check_update sub-action

One-off, bounded-timeout GitHub release check, deliberately separate
from mode=status_overview so a slow/unreachable GitHub never blocks
the rest of the status page. Hand-rolled numeric version comparison
(gonzbd's tags are plain vMAJOR.MINOR.PATCH) instead of pulling in
golang.org/x/mod/semver as a new direct dependency for this narrow
need. Degrades to status: "unknown" on any failure -- never an error
response.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 8: UI — new `/status` route: General Info + System Info

**Files:**
- Create: `ui/src/routes/status/+page.svelte`
- Modify: `ui/src/lib/api.ts` (add helper functions, following existing conventions)

**Interfaces:**
- Consumes: `mode=status_overview` (Task 4), `mode=status&name=check_update` (Task 7), `fetchJSON`/`apiUrl` (existing, `ui/src/lib/api.ts`).
- Produces: the page shell that Tasks 9-10 extend with the News Servers and disk-speed-test sections.

**Context:** Follows `AboutDialog.svelte`'s exact `$state`/`$effect`/fetch pattern (local component state, no new store file — this section doesn't need live/shared state). `ui/src/routes/+layout.svelte` does **not** provide a shared nav shell (confirmed during planning: `+page.svelte` renders its own `Navbar` etc.) — this new route must do the same. `ui/src/routes/+layout.ts` sets `ssr = false; prerender = false;` for the whole app; no new layout file is needed since that setting is inherited from the root layout automatically for any new route.

- [ ] **Step 1: Add fetch helpers to `ui/src/lib/api.ts`**

Add near the other exported fetch functions:

```ts
export interface StatusOverviewGeneral {
	version: string;
	commit: string;
	build_date: string;
	go_version: string;
	uptime_seconds: number;
	hostname: string;
	local_ip: string;
	config_path: string;
	download_dir: string;
	complete_dir: string;
	admin_dir: string;
	script_dir: string;
	log_dir: string;
	par2: { path: string; version: string };
	unrar: { path: string; version: string };
	sevenzip: { path: string; version: string };
}

export interface StatusOverviewSystem {
	os: string;
	arch: string;
	article_cache_bytes: number;
	download_dir_free_bytes: number;
	min_free_space_bytes: number;
}

export interface StatusOverviewResponse {
	status: boolean;
	general: StatusOverviewGeneral;
	system: StatusOverviewSystem;
}

export interface CheckUpdateResult {
	status: 'up_to_date' | 'update_available' | 'unknown';
	latest_version?: string;
}

export async function fetchStatusOverview(): Promise<StatusOverviewResponse> {
	return fetchJSON<StatusOverviewResponse>(apiUrl('status_overview'));
}

export async function fetchCheckUpdate(): Promise<{ status: boolean; result: CheckUpdateResult }> {
	return fetchJSON(apiUrl('status', { name: 'check_update' }));
}
```

(`apiUrl` is currently unexported in `api.ts` per earlier research — check whether it needs exporting, or whether these two new functions should live inside `api.ts` itself where `apiUrl` is already in scope, which is simpler and avoids changing its visibility. Prefer keeping `apiUrl` unexported and defining `fetchStatusOverview`/`fetchCheckUpdate` in the same file.)

- [ ] **Step 2: Create the page with General Info + System Info sections**

Create `ui/src/routes/status/+page.svelte`:

```svelte
<script lang="ts">
	import { onMount } from 'svelte';
	import Navbar from '$lib/components/Navbar.svelte';
	import {
		fetchStatusOverview,
		fetchCheckUpdate,
		type StatusOverviewResponse,
		type CheckUpdateResult
	} from '$lib/api';

	let overview = $state<StatusOverviewResponse | null>(null);
	let overviewError = $state('');
	let overviewLoading = $state(false);

	let updateCheck = $state<CheckUpdateResult | null>(null);
	let updateCheckLoading = $state(false);

	function loadOverview() {
		overviewLoading = true;
		overviewError = '';
		fetchStatusOverview()
			.then((res) => {
				overview = res;
			})
			.catch((e) => {
				overviewError = e instanceof Error ? e.message : 'Failed to load status';
			})
			.finally(() => {
				overviewLoading = false;
			});
	}

	function loadUpdateCheck() {
		updateCheckLoading = true;
		fetchCheckUpdate()
			.then((res) => {
				updateCheck = res.result;
			})
			.catch(() => {
				updateCheck = { status: 'unknown' };
			})
			.finally(() => {
				updateCheckLoading = false;
			});
	}

	function refresh() {
		loadOverview();
		loadUpdateCheck();
	}

	onMount(refresh);

	function formatUptime(seconds: number): string {
		const days = Math.floor(seconds / 86400);
		const hours = Math.floor((seconds % 86400) / 3600);
		const mins = Math.floor((seconds % 3600) / 60);
		return `${days}d ${hours}h ${mins}m`;
	}

	function formatBytes(bytes: number): string {
		if (bytes <= 0) return '0 B';
		const units = ['B', 'KB', 'MB', 'GB', 'TB'];
		const i = Math.floor(Math.log(bytes) / Math.log(1024));
		return `${(bytes / Math.pow(1024, i)).toFixed(1)} ${units[i]}`;
	}
</script>

<svelte:head><title>Status - GoNZBD</title></svelte:head>

<Navbar />

<div class="mx-auto max-w-4xl p-6">
	<h1 class="mb-4 text-2xl font-semibold text-m3-on-surface">Status</h1>

	<button
		class="mb-4 rounded-full bg-m3-primary px-4 py-2 text-sm text-m3-on-primary"
		onclick={refresh}
		disabled={overviewLoading}
	>
		{overviewLoading ? 'Refreshing...' : 'Refresh'}
	</button>

	{#if overviewError}
		<p class="text-red-500">{overviewError}</p>
	{:else if overview}
		<section class="mb-6 rounded-3xl border border-m3-outline/20 bg-m3-surface p-6 shadow-sm transition-all duration-300 hover:shadow-md hover:border-m3-primary/30">
			<h2 class="mb-4 text-lg font-medium text-m3-on-surface">General Info</h2>
			<dl class="grid grid-cols-[180px_1fr] gap-x-4 gap-y-3 text-sm">
				<dt class="text-m3-on-surface/60">Version</dt>
				<dd class="font-mono text-m3-on-surface">{overview.general.version} ({overview.general.commit})</dd>
				<dt class="text-m3-on-surface/60">Uptime</dt>
				<dd class="text-m3-on-surface">{formatUptime(overview.general.uptime_seconds)}</dd>
				<dt class="text-m3-on-surface/60">Go version</dt>
				<dd class="font-mono text-m3-on-surface">{overview.general.go_version}</dd>
				<dt class="text-m3-on-surface/60">Hostname</dt>
				<dd class="text-m3-on-surface">{overview.general.hostname}</dd>
				<dt class="text-m3-on-surface/60">Local IP</dt>
				<dd class="font-mono text-m3-on-surface">{overview.general.local_ip}</dd>
				<dt class="text-m3-on-surface/60">Config path</dt>
				<dd class="font-mono text-m3-on-surface">{overview.general.config_path}</dd>
				<dt class="text-m3-on-surface/60">par2</dt>
				<dd class="font-mono text-m3-on-surface">{overview.general.par2.path || 'not found'} {overview.general.par2.version}</dd>
				<dt class="text-m3-on-surface/60">unrar</dt>
				<dd class="font-mono text-m3-on-surface">{overview.general.unrar.path || 'not found'} {overview.general.unrar.version}</dd>
				<dt class="text-m3-on-surface/60">7-Zip</dt>
				<dd class="font-mono text-m3-on-surface">{overview.general.sevenzip.path || 'not found'} {overview.general.sevenzip.version}</dd>
				<dt class="text-m3-on-surface/60">Update</dt>
				<dd>
					{#if updateCheckLoading}
						<span class="inline-flex items-center gap-1.5 text-xs text-m3-on-surface/60">
							<svg class="animate-spin h-3.5 w-3.5 text-m3-primary" xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24"><circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"></circle><path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"></path></svg>
							checking…
						</span>
					{:else if updateCheck?.status === 'update_available'}
						<span class="inline-flex items-center gap-1.5 text-xs font-semibold text-amber-600 bg-amber-50 px-2 py-0.5 rounded-full border border-amber-200">
							<span class="relative flex h-2 w-2">
								<span class="animate-ping absolute inline-flex h-full w-full rounded-full bg-amber-400 opacity-75"></span>
								<span class="relative inline-flex rounded-full h-2 w-2 bg-amber-500"></span>
							</span>
							update available: {updateCheck.latest_version}
						</span>
					{:else if updateCheck?.status === 'up_to_date'}
						<span class="inline-flex items-center gap-1.5 text-xs text-green-600 bg-green-50 px-2 py-0.5 rounded-full border border-green-200">
							<span class="h-2 w-2 rounded-full bg-green-500"></span>
							up to date
						</span>
					{:else}
						<span class="text-m3-on-surface/60">unknown</span>
					{/if}
				</dd>
			</dl>
		</section>

		<section class="mb-6 rounded-3xl border border-m3-outline/20 bg-m3-surface p-6 shadow-sm transition-all duration-300 hover:shadow-md hover:border-m3-primary/30">
			<h2 class="mb-4 text-lg font-medium text-m3-on-surface">System Info</h2>
			<dl class="grid grid-cols-[180px_1fr] gap-x-4 gap-y-3 text-sm">
				<dt class="text-m3-on-surface/60">OS / Arch</dt>
				<dd class="text-m3-on-surface">{overview.system.os} / {overview.system.arch}</dd>
				<dt class="text-m3-on-surface/60">Article cache usage</dt>
				<dd class="text-m3-on-surface">{formatBytes(overview.system.article_cache_bytes)}</dd>
				<dt class="text-m3-on-surface/60">Download dir free space</dt>
				<dd class="text-m3-on-surface">
					{formatBytes(overview.system.download_dir_free_bytes)}
					<span class="text-m3-on-surface/50"
						>(min: {formatBytes(overview.system.min_free_space_bytes)})</span
					>
				</dd>
			</dl>
		</section>
	{/if}
</div>
```

(`Navbar`'s props — `paused` and `onpausetoggle`, per `ui/src/lib/components/Navbar.svelte` — are both optional with defaults, so `<Navbar />` with no props, as written above, is valid; no queue-pause wiring is needed on this page. Copy the exact Tailwind `m3-*` token names from `AboutDialog.svelte` rather than inventing new ones — the markup above already uses only tokens confirmed present there (`bg-m3-surface`, `text-m3-on-surface`, `border-m3-outline/20`).)

- [ ] **Step 3: Manually verify in a browser**

```bash
cd ui && npm run dev
```

Navigate to `http://localhost:5173/status` (or whatever port Vite reports) and confirm the General Info and System Info sections render with real data from a running backend, the Refresh button works, and the update-check row shows "checking…" then resolves independently of the rest of the page (verify this specifically by throttling network in devtools or temporarily pointing `githubLatestReleaseURL` at an unreachable host to confirm the rest of the page still renders immediately).

- [ ] **Step 4: Build the UI and confirm it embeds cleanly**

```bash
cd ui && npm run build
cd .. && go build ./...
```

Expected: both succeed, confirming the new route is included in the SvelteKit static build and the Go binary still embeds/serves it.

- [ ] **Step 5: Commit**

```bash
git add ui/src/routes/status/+page.svelte ui/src/lib/api.ts
git commit -m "$(cat <<'EOF'
feat(ui): add /status page with General Info and System Info sections

New SvelteKit route consuming mode=status_overview (fetched on mount
and via a manual refresh button) and mode=status&name=check_update
(fetched independently and in parallel, so a slow/unreachable GitHub
never delays the rest of the page).

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 9: UI — News Servers section with Test Connection

**Files:**
- Modify: `ui/src/routes/status/+page.svelte`
- Modify: `ui/src/lib/api.ts` (add `testConnection` helper)

**Interfaces:**
- Consumes: `getServerStats()`, `startTelemetry()`/`stopTelemetry()` (existing, `$lib/stores/telemetry.svelte.ts`), `mode=status&name=test_connection` (Task 5), `postAction` (existing, `$lib/api.ts`).

**Context — important correction to Task 8's framing:** `getServerStats()` reads a module-singleton store that only receives data while a WebSocket subscription is active. That subscription is reference-counted (`ui/src/lib/stores/websocket.svelte.ts:77-88`: `subscribe()` opens the socket on the first handler, closes it on the last) and is only started by calling `startTelemetry()` (`ui/src/lib/stores/telemetry.svelte.ts:73-74`). Today, the only caller of `startTelemetry()` is `queue.svelte.ts`'s `start()` (`queue.svelte.ts:51-53`), which is invoked exclusively by the main dashboard's `onMount` (`ui/src/routes/+page.svelte`). The root `+layout.svelte` starts nothing but the theme. **This means a standalone `/status` route that never calls `startTelemetry()` itself will render an empty News Servers section on direct load/refresh/bookmark** (the primary way a user reaches this page) — there is no other code path that starts the subscription for it. Task 8's "Context" note describing the server-stats store as "already live via WebSocket" is only true *while the main dashboard page is also mounted*; it does not hold for this route in isolation. This task must start and stop the subscription itself, scoped to this page's own lifecycle — importing `queue.svelte.ts`'s wrapper is unnecessary and would incorrectly also start queue polling that this page doesn't use, so import `startTelemetry`/`stopTelemetry` directly from `telemetry.svelte.ts` instead.

- [ ] **Step 1: Add the test-connection helper to `ui/src/lib/api.ts`**

```ts
export interface TestConnectionResult {
	ok: boolean;
	latency_ms?: number;
	error?: string;
	likely_connection_limit?: boolean;
}

export async function testServerConnection(name: string): Promise<{ status: boolean; result: TestConnectionResult }> {
	return fetchJSON(apiUrl('status', { name: 'test_connection', value: name }));
}
```

- [ ] **Step 2: Add the News Servers section to the page**

Extend `ui/src/routes/status/+page.svelte`'s `<script>` block. Add `onDestroy` to the existing `import { onMount } from 'svelte';` line from Task 8 (making it `import { onMount, onDestroy } from 'svelte';`), then add:

```ts
	import { getServerStats } from '$lib/stores/queue.svelte';
	import { startTelemetry, stopTelemetry } from '$lib/stores/telemetry.svelte';
	import { testServerConnection, type TestConnectionResult } from '$lib/api';

	let servers = $derived(getServerStats());
	let testingServer = $state<string | null>(null);
	let connectionResults = $state<Record<string, TestConnectionResult>>({});

	// This page is the only route that renders server data outside the
	// main dashboard, so it must start its own WebSocket subscription
	// (reference-counted — see Context above) rather than assuming one is
	// already running. Symmetric stop on unmount so a repeat visit to
	// /status doesn't leak a duplicate handler.
	onMount(() => {
		startTelemetry();
	});
	onDestroy(() => {
		stopTelemetry();
	});

	async function runConnectionTest(name: string) {
		testingServer = name;
		try {
			const res = await testServerConnection(name);
			connectionResults = { ...connectionResults, [name]: res.result };
		} catch (e) {
			connectionResults = {
				...connectionResults,
				[name]: { ok: false, error: e instanceof Error ? e.message : 'Test failed' }
			};
		} finally {
			testingServer = null;
		}
	}
```

Add the section markup, after the System Info section (verify exact `ServerSnapshot` field names against `ui/src/lib/types.ts:190-207` before writing — `active_conns`/`max_connections` per earlier research):

```svelte
	<section class="mb-6 rounded-3xl border border-m3-outline/20 bg-m3-surface p-6 shadow-sm transition-all duration-300 hover:shadow-md hover:border-m3-primary/30">
		<h2 class="mb-4 text-lg font-medium text-m3-on-surface">News Servers</h2>
		{#each servers as server (server.name)}
			<div class="mb-3 rounded-2xl border border-m3-outline/10 p-4 transition-all hover:bg-m3-surface-variant/5">
				<div class="flex items-center justify-between">
					<span class="font-medium text-m3-on-surface">{server.name} ({server.host}:{server.port})</span>
					<span class="text-sm text-m3-on-surface/60">
						{server.active_conns}/{server.max_connections} connections in use
					</span>
				</div>
				<div class="mt-3 flex items-center gap-3">
					<button
						class="inline-flex items-center gap-1.5 rounded-full bg-m3-secondary px-3 py-1 text-xs text-m3-on-secondary disabled:opacity-50"
						onclick={() => runConnectionTest(server.name)}
						disabled={testingServer === server.name}
					>
						{#if testingServer === server.name}
							<svg class="animate-spin h-3 w-3 text-m3-on-secondary" xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24"><circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"></circle><path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"></path></svg>
							Testing...
						{:else}
							Test Connection
						{/if}
					</button>
					{#if connectionResults[server.name]}
						{@const result = connectionResults[server.name]}
						{#if result.ok}
							<span class="inline-flex items-center gap-1.5 text-xs text-green-600 bg-green-50 px-2.5 py-0.5 rounded-full border border-green-200">
								<span class="relative flex h-1.5 w-1.5">
									<span class="animate-ping absolute inline-flex h-full w-full rounded-full bg-green-400 opacity-75"></span>
									<span class="relative inline-flex rounded-full h-1.5 w-1.5 bg-green-500"></span>
								</span>
								Connected ({result.latency_ms}ms)
							</span>
						{:else if result.likely_connection_limit}
							<span class="inline-flex items-center gap-1.5 text-xs text-amber-600 bg-amber-50 px-2.5 py-0.5 rounded-full border border-amber-200">
								<span class="h-1.5 w-1.5 rounded-full bg-amber-500"></span>
								Connection limit reached ({result.error})
							</span>
						{:else}
							<span class="inline-flex items-center gap-1.5 text-xs text-red-600 bg-red-50 px-2.5 py-0.5 rounded-full border border-red-200">
								<span class="relative flex h-1.5 w-1.5">
									<span class="animate-ping absolute inline-flex h-full w-full rounded-full bg-red-400 opacity-75"></span>
									<span class="relative inline-flex rounded-full h-1.5 w-1.5 bg-red-500"></span>
								</span>
								Failed: {result.error}
							</span>
						{/if}
					{/if}
				</div>
			</div>
		{/each}
	</section>
```

- [ ] **Step 3: Manually verify in a browser**

**Load `/status` by typing the URL directly (or a hard refresh) — not by clicking a nav link from the dashboard** — and confirm the News Servers section actually populates. This specifically exercises the `startTelemetry()` wiring from Step 2: navigating from the dashboard would appear to work even if that wiring were missing, because the dashboard's own subscription is often still tearing down/overlapping. A direct load is the case that was broken before this task's fix. Then, with a real (or test) NNTP server configured, click "Test Connection" on a server row and confirm the result renders inline. If possible, verify the 502/503 "likely_connection_limit" messaging by temporarily configuring a server with all connections already saturated by an active download.

- [ ] **Step 4: Build and confirm**

```bash
cd ui && npm run build
cd .. && go build ./...
```

Expected: both succeed.

- [ ] **Step 5: Commit**

```bash
git add ui/src/routes/status/+page.svelte ui/src/lib/api.ts
git commit -m "$(cat <<'EOF'
feat(ui): add News Servers section with Test Connection to /status

Reuses the existing WebSocket-pushed server-stats store rather than
building a new data-fetching path for server data, starting and
stopping the subscription on this page's own mount/unmount since it's
the only route besides the dashboard that renders it. Adds a
per-server on-demand connection test, surfacing a softer message when
the failure looks like an exhausted connection limit rather than a
real config problem.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 10: UI — Test Disk Speed button

**Files:**
- Modify: `ui/src/routes/status/+page.svelte`
- Modify: `ui/src/lib/api.ts` (add `testDiskSpeed` helper)

**Interfaces:**
- Consumes: `mode=status&name=test_disk_speed` (Task 6).

- [ ] **Step 1: Add the helper to `ui/src/lib/api.ts`**

```ts
export interface TestDiskSpeedResult {
	ok: boolean;
	mb_per_sec?: number;
	error?: string;
}

export async function testDiskSpeed(): Promise<{ status: boolean; result: TestDiskSpeedResult }> {
	return fetchJSON(apiUrl('status', { name: 'test_disk_speed' }));
}
```

- [ ] **Step 2: Add the button to the System Info section**

Extend `ui/src/routes/status/+page.svelte`'s `<script>` block:

```ts
	import { testDiskSpeed, type TestDiskSpeedResult } from '$lib/api';

	let diskSpeedTesting = $state(false);
	let diskSpeedResult = $state<TestDiskSpeedResult | null>(null);

	async function runDiskSpeedTest() {
		diskSpeedTesting = true;
		try {
			const res = await testDiskSpeed();
			diskSpeedResult = res.result;
		} catch (e) {
			diskSpeedResult = { ok: false, error: e instanceof Error ? e.message : 'Test failed' };
		} finally {
			diskSpeedTesting = false;
		}
	}
```

Add markup inside the System Info section, after the free-space `<dd>`:

```svelte
				<dt class="text-m3-on-surface/60">Disk speed</dt>
				<dd class="flex items-center gap-2">
					<button
						class="inline-flex items-center gap-1.5 rounded-full bg-m3-secondary px-3 py-1 text-xs text-m3-on-secondary disabled:opacity-50"
						onclick={runDiskSpeedTest}
						disabled={diskSpeedTesting}
					>
						{#if diskSpeedTesting}
							<svg class="animate-spin h-3 w-3 text-m3-on-secondary" xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24"><circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"></circle><path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"></path></svg>
							Testing...
						{:else}
							Test Disk Speed
						{/if}
					</button>
					{#if diskSpeedResult}
						{#if diskSpeedResult.ok}
							<span class="inline-flex items-center gap-1 text-xs font-semibold text-green-600 bg-green-50 px-2 py-0.5 rounded-full border border-green-200">
								{diskSpeedResult.mb_per_sec?.toFixed(1)} MB/s
							</span>
						{:else}
							<span class="inline-flex items-center gap-1 text-xs text-red-600 bg-red-50 px-2 py-0.5 rounded-full border border-red-200">
								Failed: {diskSpeedResult.error}
							</span>
						{/if}
					{/if}
				</dd>
```

- [ ] **Step 3: Manually verify in a browser**

Load `/status`, click "Test Disk Speed", confirm a plausible MB/s figure appears.

- [ ] **Step 4: Build and confirm**

```bash
cd ui && npm run build
cd .. && go build ./...
```

Expected: both succeed.

- [ ] **Step 5: Commit**

```bash
git add ui/src/routes/status/+page.svelte ui/src/lib/api.ts
git commit -m "$(cat <<'EOF'
feat(ui): add Test Disk Speed button to /status System Info section

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 11: UI end-to-end test

**Files:**
- Modify: `test/uitest/uitest_test.go` (or create a new file in the same package if it's grown too large — check current line count first)

**Interfaces:**
- Consumes: existing `test/uitest` helpers (`newTestEnv`, `env.newPage`, `screenshotOnFailure`, `env.navigate`).

- [ ] **Step 1: Check file size before adding**

```bash
wc -l test/uitest/uitest_test.go
```

If it's already large (500+ lines per earlier research), create `test/uitest/status_test.go` instead, in the same package, following the same helper conventions.

- [ ] **Step 2: Write the test**

```go
func TestStatusPageLoadsAndShowsGeneralInfo(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	page := env.newPage(t)
	screenshotOnFailure(t, page)
	env.navigate(t, page, "/status")

	heading := page.GetByRole("heading", playwright.PageGetByRoleOptions{Name: playwright.String("Status")})
	if err := heading.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Fatalf("Status page heading not visible: %v", err)
	}

	generalInfo := page.GetByText("General Info")
	if err := generalInfo.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Errorf("General Info section not visible: %v", err)
	}

	systemInfo := page.GetByText("System Info")
	if err := systemInfo.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Errorf("System Info section not visible: %v", err)
	}
}
```

(Verify `playwright.PageGetByRoleOptions`/`GetByRole` is the actual pattern used elsewhere in this test package before committing to it — the representative test found during planning used `page.GetByText(...)` for dialog content, which may be simpler and more consistent to reuse for the heading check too, e.g. `page.GetByText("Status", playwright.PageGetByTextOptions{Exact: playwright.Bool(true)})` if `GetByRole` isn't already an established pattern in this file.)

- [ ] **Step 3: Run the new test**

```bash
go test -tags=uitest ./test/uitest/... -run TestStatusPageLoadsAndShowsGeneralInfo -v
```

Expected: PASS (requires the pre-built UI + Playwright Chromium per `docs/TESTING.md`'s uitest prerequisites — confirm these are available before running; if not, this step must be run in an environment that has them).

- [ ] **Step 4: Commit**

```bash
git add test/uitest/status_test.go  # or uitest_test.go if added there
git commit -m "$(cat <<'EOF'
test(uitest): add coverage for the new /status page

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 12: Documentation sync

**Files:**
- Modify: `docs/sabnzbd_spec.md` (if it enumerates supported API modes — check first)
- Modify: `docs/ARCHITECTURE.md` (if it enumerates UI routes/pages)
- Modify: `README.md` (if it lists UI features/pages)

- [ ] **Step 1: Check whether any doc enumerates API modes or UI pages**

```bash
grep -n "mode=server_stats\|mode=warnings\|mode=about" docs/sabnzbd_spec.md docs/ARCHITECTURE.md README.md
```

- [ ] **Step 2: If found, add entries for the four new modes**

Add `status_overview`, `status&name=test_connection`, `status&name=test_disk_speed`, `status&name=check_update` to whichever doc(s) Step 1 surfaced, following the exact formatting of the neighboring existing entries (e.g. `server_stats`'s entry) — do not invent a new format.

- [ ] **Step 3: If nothing enumerates these, note that explicitly rather than skipping silently**

If Step 1 finds no doc that would need updating, state this in the task's commit message rather than silently doing nothing, so it's clear the check was performed.

- [ ] **Step 4: Commit (only if changes were made)**

```bash
git add docs/sabnzbd_spec.md docs/ARCHITECTURE.md README.md  # whichever were actually touched
git commit -m "$(cat <<'EOF'
docs(status): document new status-page API modes

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 13: Full-repo verification pass

**Files:** None modified — verification only.

- [ ] **Step 1: Full quality gate sequence**

```bash
go fix ./...
goimports -w .
go vet ./...
go test -race ./...
./scripts/run_tests.sh
golangci-lint run ./...
```

Expected: all pass, no new issues.

- [ ] **Step 2: Mutation testing on the diff**

```bash
gremlins unleash --timeout-coefficient 100 --diff origin/main
```

Expected: no LIVED mutants in the changed lines. Pay particular attention to `compareVersions` (Task 7) and the `errors.Is(err, nntp.ErrServerUnavailable)` branch (Task 5) — both are exactly the kind of boundary/branch logic mutation testing is designed to catch. If any mutant lives, add a targeted test per `docs/mutation-testing-playbook.md`.

- [ ] **Step 3: Confirm `go.mod`/`go.sum` are unchanged from this entire plan**

```bash
git diff origin/main -- go.mod go.sum
```

Expected: no output — the whole feature was built with zero new external dependencies (Task 7's deliberate hand-rolled version comparison).

- [ ] **Step 4: No commit for this task** — verification only. If Step 1 or Step 2 surface a real fix, that fix gets its own properly-scoped commit.
