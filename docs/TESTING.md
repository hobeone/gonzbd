# GoNZBD Testing Guide

This document describes all test suites in the GoNZBD project, their purpose,
required tools, and when to run each one. **Agents must read this file before
running or modifying tests.**

## Quick Reference

| Suite | Command | Build Tag | External Tools | Duration |
|-------|---------|-----------|---------------|----------|
| Unit tests | `go test ./...` | none | none | ~30s |
| Unit + race | `go test -race ./...` | none | none | ~100s |
| Integration | `go test -v -tags=integration ./test/integration/... ./internal/par2/...` | `integration` | par2, rar, unrar, 7z | ~35s |
| UI (Playwright) | `go test -v -tags=uitest ./test/uitest/...` | `uitest` | Chromium (Playwright), pre-built UI | ~30s |
| E2E | `go test -timeout=10m ./test/e2e/` | none (runtime env-var gates) | live Usenet server | ~5min |
| Crash consistency | `go test -tags=crash -timeout=20m ./test/crash/` | `crash` | Linux; builds `./cmd/gonzbd` itself | ~10s |
| Config contract | `go test ./internal/config/ -run 'TestUI\|TestAllFlat'` | none | none | <1s |
| Tagged-file compile check | `go vet -tags=integration,uitest,crash ./...` | all three | none (host GOOS only; `crash` files are `crash && linux`, so they are skipped off Linux) | 0.2s warm, 6.9s cold |

### Convenience Script

`scripts/run_tests.sh` runs the full suite with a single command: unit →
integration → crash-consistency → UI vitest → `bun run build` → uitest
sequentially. Use it as the canonical pre-commit gate (matches CLAUDE.md
quality gates).

## 1. Unit Tests (`go test ./...`)

**When to run:** Before every commit. Required to pass with `-race`.

Standard Go unit tests across all packages. No external dependencies.
Includes app-level scenario tests (`internal/app/scenario_*.go` — nine files;
see section 6) which use an in-process mock NNTP server to test
download → assembly → post-processing flows without shelling out to real tools.

```bash
# Standard run (required before every commit)
go test -race -count=1 ./...

# Single package
go test -race ./internal/queue/

# Verbose with specific test
go test -v -race -run TestSetCategory ./internal/queue/
```

## 2. Integration Tests (`-tags=integration`)

**When to run:** After changes to:
- Post-processing pipeline (`internal/postproc/`)
- Archive scanning/extraction (`internal/unpack/`)
- Par2 verify/repair (`internal/par2/`)
- NZB download pipeline (`internal/downloader/`, `internal/assembler/`)
- API endpoint behavior (`internal/api/`)
- File naming or directory structure (`internal/nzb/`, `internal/fsutil/`)

**Location:** `test/integration/`

**Build tag:** `integration` — these tests are excluded from `go test ./...`
because they require external tool binaries.

### Required Tools

Tests that need specific tools call `requireTool(t, "toolname")` and skip
automatically if the tool is not in `PATH`. The tools are:

| Tool | Used by |
|------|---------|
| `rar` | Creating RAR fixtures (archiving) |
| `unrar` | RAR extraction pipeline tests |
| `par2` | Par2 verify/repair pipeline tests |
| `7z` | 7-zip extraction pipeline tests |

Install on Debian/Ubuntu:
```bash
sudo apt install unrar rar par2 p7zip-full
```

### Test Files

| File | Purpose |
|------|---------|
| `testhelpers_test.go` | Core helpers: TestFile, BuildNZB, RegisterArticles, mock NNTP wiring |
| `helpers_ext_test.go` | Fixture creators (createRarFixture, createPar2Fixture, create7zFixture), app builders, path verification |
| `download_test.go` | Basic download: single-file, multi-file, multi-part, file size verification |
| `naming_test.go` | File/directory naming: NZB extension stripping, PRiVATE subjects, quoted filenames, final move |
| `pipeline_test.go` | Full pipelines: par2+unrar, par2 repair+unrar, 7z, rar-via-7z, missing tool, plain file, PRiVATE, **subdirectory unrar** |
| `postproc_test.go` | Post-processing stages: par2 verify, par2 repair, unrar extraction |
| `config_pipeline_test.go` | Config-driven pipeline: flat unpack, nice wrapping |
| `contract_test.go` | API response shape contracts: queue, history, status, config round-trip |
| `api_test.go` | API auth/mode tests: open modes, protected modes, admin modes, addfile/addurl/addlocalfile |
| `statemachine_test.go` | Download state machine chaos testing |
| `duplicate_test.go` | Duplicate detection |
| `oneshot_test.go` | One-shot CLI download mode |
| `truncation_test.go` | Truncated article handling |
| `replay_test.go` | Historical NZB replay (requires fixtures) |
| `directunpack_integration_test.go` | Pure-Go RAR extraction via rarengine: multi-volume streaming and volume-boundary handling |
| `pause_integration_test.go` | Tests pause and resume behaviors of the active queue and downloading connection goroutines |
| `queue_updated_broadcast_test.go` | Verifies that queue updates trigger active real-time WebSocket broadcasts |

### Running

```bash
# Full suite (verbose recommended for pipeline tests)
go test -v -tags=integration ./test/integration/... ./internal/par2/...

# Single test
go test -v -tags=integration -run TestPipeline_SubdirectoryUnrar ./test/integration/...

# With race detector (slower but catches concurrency bugs)
go test -v -race -tags=integration ./test/integration/... ./internal/par2/...
```

### Adding New Integration Tests

1. Use `requireTool(t, "toolname")` for any external binary dependency
2. Use `NewTestAppSeparateDirs(t, addr, AppTestOpts{...})` for pipeline tests
3. Use `createRarFixture` / `create7zFixture` / `createPar2Fixture` for archive fixtures
4. Use `fixtureToTestFiles` for flat fixtures, `fixtureToTestFilesRecursive` for subdirectory structures
5. Use `waitForPostProcWithTimeout(t, a, pipelineTimeout)` to wait for completion
6. Use `verifyFileAtPath(t, path, sha)` for strict path + content verification

## 3. E2E Tests

**When to run:** Manually, to validate against a real Usenet provider.
Not run in CI.

**Location:** `test/e2e/`

**Build tag:** none — the package has no `//go:build` constraint and compiles
unconditionally. Tests skip at runtime based on the env vars below (e.g.,
`TestE2E_SelfPost_*` skips unless `E2E_POST=1`).

These tests download real articles from a live Usenet server. They require
a configured `gonzbd.yaml` with valid server credentials.

### Modes

| Env var | Purpose |
|---------|---------|
| (none) | Run basic download tests only |
| `E2E_POST=1` | Self-post articles then download them |
| `E2E_NZB=/path/to/file.nzb` | Download a specific NZB file |
| `E2E_DEBUG=1` | Enable pipeline debug logging |
| `E2E_KEEP_FILES=1` | Don't clean up downloaded files |

```bash
# Basic
go test -timeout=10m ./test/e2e/

# Self-post + download with debug
E2E_POST=1 E2E_DEBUG=1 go test -timeout=10m -v ./test/e2e/

# Download a specific NZB
E2E_NZB=/tmp/test.nzb go test -timeout=10m ./test/e2e/
```

## 3a. Crash-Consistency Tests (`-tags=crash`)

**When to run:** After any change to `internal/durability`, `internal/assembler`,
the checkpoint cadence in `internal/app/durability.go`, the startup resume sweep
in `internal/app/resume_startup.go`, or the queue's per-article persistence.
Not run in CI (see "Continuous Integration" in AGENTS.md — `ci.yml` is
dispatch-only for every suite, not specifically this one), but it **is** part
of `scripts/run_tests.sh` (step 4/7) — see the status note at the end of this
section for pass/fail history.

**Location:** `test/crash/`

**Build tag:** `crash` — excluded from `go test ./...` and from every other
suite. `TestMain` builds `./cmd/gonzbd` into a temp directory itself, so the
suite needs no pre-built binary, but it does need `ui/dist` to exist (the
usual worktree caveat).

**Linux only, and it fails rather than skips on another platform.** Every claim
it makes rests on SIGKILL semantics and on `posix_fadvise`.

### What it does

Each test runs the real daemon as a **child process** against an in-test mock
NNTP server, drives it through the HTTP API, and then kills or perturbs it. The
daemon must be a separate process for the kill to mean anything: an in-process
test cannot lose the memory the design's central claim is about.

| Test | Pins |
|------|------|
| `TestSIGKILL_NoArticleIsResolvedWithoutItsBytes` | S1/S2 — nothing is resolved on the strength of having entered a buffer |
| `TestSIGKILL_ReworkStaysWithinTheCheckpointBound` | B1/L3 — unacked rework is bounded, acked rework is zero; and #361, that the resume continues the same file |
| `TestExternalModification_TruncatedPartialIsRecomputed` | S4 — a recomputation supersedes a falsified cache |
| `TestExternalModification_DeletedPartialRestartsTheFile` | S3 — absence of evidence is absence |
| `TestExternalModification_AppendedGarbageIsTrimmed` | S6 — metadata may shrink a file, never grow it |
| `TestExternalModification_MtimeTouchCostsNoRefetch` | R13 — an invalidated stamp costs a recomputation, not a re-download |

The last four together discharge R33's four external modifications: truncate,
delete, append, mtime-only touch.

### What a pass does and does not bound

This is the part that decides how much a green run is worth, so it is stated
plainly.

**A pass DOES bound**, on the filesystem the tests ran on:

- That no article is resolved before its bytes have left the process. A
  SIGKILL destroys the assembler's write cache for real, with no flush, so an
  article acked early has no bytes in the file afterwards and the CRC
  read-back sees it.
- That the work a crash costs stays inside the checkpoint bound, measured at
  the wire from the mock server's per-article delivery counts rather than from
  any status the daemon reports about itself.
- That an article a completed fsync covered is never fetched again.

**A pass does NOT bound:**

- **That an fsync'd byte reached the platter.** No unprivileged userspace call
  can discard dirty page-cache data: `POSIX_FADV_DONTNEED` invalidates clean
  pages and skips dirty ones, and `/proc/sys/vm/drop_caches` skips them too.
  `O_DIRECT` does not help either: it flushes before it bypasses. The suite
  calls `fadvise(DONTNEED)` after each kill, which forces already-written-back
  ranges to be re-read from the block device — a real strengthening of the
  read-back, and not a power-loss simulation. Testing the page-cache half needs
  a device the test can cut underneath the filesystem (a device-mapper
  `log-writes` or `flakey` target), which needs root.

  This is measured, not inferred: **removing the `Sync()` syscall from the
  write path entirely left the suite byte-identical to baseline** — six passes,
  same assertions, no diagnostic difference. Read that as the bound on what a
  green run means, not as evidence that the fsync is unnecessary.
- **NFS or SMB fsync behaviour.** The bound is measured on the test's own
  filesystem only. A server that acknowledges an fsync it has not honoured is
  outside what any of this can see.
- **A full disk.** There is no unprivileged way to inject `ENOSPC` into a real
  child process: a small filesystem needs `mount`, `EISDIR` is dodged by
  unique-filename resolution, and `EACCES`/`EROFS`/`EFBIG` classify permanent
  rather than retryable. The stall path is covered in-process instead, by
  `internal/app/scenario_durability_test.go` and `internal/api/stall_test.go`.
  Tracked as issue #363, which also records the one route that would close both
  this gap and the page-cache gap above: a FUSE filesystem the harness controls.

```bash
go test -tags=crash -timeout=20m ./test/crash/ -v
```

### Status: all six pass, and two of them were red until #362 was fixed

`TestExternalModification_TruncatedPartialIsRecomputed` and
`TestExternalModification_DeletedPartialRestartsTheFile` were committed red on
purpose, against a real defect. Both produced a **completed file with a hole in
it**: the daemon declared the job finished and moved a file to the complete
directory whose destroyed region read back as zeros.

The cause was that the resume's finding was discarded rather than wrong.
`durability.Resumer` got the right answer for a file truncated in half — but
the startup sweep installed it through the **additive** seeding entry point,
which only *sets* durable bits and never clears one, while
`Store.RestoreJobProgress` had already restored every article the last barrier
acked. So the queue's restored state outranked the finding that disproved it.

The fix (#362) is `Queue.ReplaceFromRuns`: a second, **authoritative** seeding
entry point that the startup sweep uses in place of `SeedFromRuns`, because it
is the one caller that has just stat'ed the files and deleted the runs a file
contradicts. Every other seeding path — `Application.reevaluateStall`'s phase 3
— is replaying an ack that already landed and stays additive.
`TestSeedFromRuns_StaysAdditive` and
`TestSeedFromCommittedRuns_DoesNotClearAnAckThisProcessMade` are the guards
on that split; they are the only tests in the repository that redden when the
two entry points are merged.

Making the sweep authoritative also turned two of the other four tests into
real pins. `TestExternalModification_MtimeTouchCostsNoRefetch` and the
no-refetch half of `TestExternalModification_AppendedGarbageIsTrimmed` used to
pass with `durability.Resumer.Resume` neutered to return no runs, because the
restored state alone kept those articles off the wire. With the sweep
authoritative, that neutering reddens both — observed, not reasoned.

**Do not silence a failing test here by weakening it.** The assertions are
about WHICH articles came back over the wire, and a test that only checked the
final file would pass against an implementation that threw the whole file away.

## 4. Config ↔ UI Contract Tests

**When to run:** After any change to:
- Go config structs (`internal/config/`)
- Svelte config components (`ui/src/lib/components/config/`)
- Any `keyword=` prop in a `ConfigInput`, `ConfigSwitch`, or `ConfigTextarea`

**Location:** `internal/config/ui_contract_test.go`

These tests validate that every `keyword=` prop used in the Svelte UI
corresponds to a valid JSON tag in the Go config structs, and vice versa.

```bash
go test ./internal/config/ -run 'TestUIKeywordsAreValidConfigTags|TestAllFlatConfigTagsAreSettable'
```

## 5. UI Tests (`-tags=uitest`)

**When to run:** After changes to:
- Svelte UI components (`ui/src/`)
- Web handler / SPA serving (`internal/web/`)
- API response shapes that the UI consumes (`internal/api/`)
- Any visual/layout/interaction behavior

**Location:** `test/uitest/`

**Build tag:** `uitest`

Browser-based integration tests using [Playwright for Go](https://github.com/mxschmitt/playwright-go).
Tests spin up the full Go backend with the embedded Svelte SPA and drive a
real Chromium browser against it.

### Prerequisites

1. **Build the UI first:** `cd ui && bun run build`
2. **Install Playwright browsers** (cached in `~/.cache/ms-playwright-go/`):
   ```bash
   go run github.com/mxschmitt/playwright-go/cmd/playwright@v0.xxxx.x install --with-deps
   ```

### Test Files

| File | Purpose |
|------|---------|
| `harness_test.go` | Test harness: `newTestEnv()`, mock app, queue/history seeding, screenshot-on-failure |
| `uitest_test.go` | All UI test cases: page load, navigation, queue display, history, settings, API interaction |

### Running

```bash
# Full suite
go test -v -tags=uitest ./test/uitest/...

# Single test
go test -v -tags=uitest -run TestUI_QueuePage ./test/uitest/...
```

Failed tests automatically capture screenshots to `test/uitest/screenshots/`.

## 6. App Scenario Tests

**When to run:** These run as part of `go test ./...`. No special flags needed.

**Location:** `internal/app/` — nine scenario files:

| File | Focus |
|------|-------|
| `scenario_test.go` | Core download → assembly → post-processing lifecycle |
| `scenario_recovery_test.go` | Recovery after mid-job failures |
| `scenario_smoke_test.go` | Minimal smoke tests for fast feedback |
| `scenario_checkpoint_test.go` | Queue checkpoint / persistence under load |
| `scenario_durability_test.go` | Durability across restarts |
| `scenario_reload_test.go` | Config reload without restart |
| `scenario_retry_reset_test.go` | Article retry and reset logic |
| `scenario_decode_error_test.go` | Decoder error handling paths |
| `scenario_dispatch_deadlock_test.go` | Dispatch deadlock detection |

These test the full app lifecycle (download → assembly → post-processing)
using an in-process mock NNTP server. They do **not** shell out to external
tools — post-processing stages that require `par2`/`unrar`/`7z` are stubbed.

## 7. Mutation Testing (`gremlins`)

**When to run:** Not a per-commit gate — it's slow (whole-package baseline) and `--diff` is broken upstream for package-scoped runs (see below), so there's no fast incremental mode. Run it manually before submitting a change with substantial new branching/error-handling logic, or when you suspect a test is a change-detector rather than a real pin on behavior. There is no CI automation for it — it is entirely manual.

**Command:**
```bash
# Focused on a single package — always via the wrapper script, never
# `gremlins unleash` directly (see WARNING below for why).
./scripts/run_gremlins.sh ./internal/<package>
```

> [!WARNING]
> **Always invoke gremlins via `./scripts/run_gremlins.sh`, never call
> `gremlins unleash` directly.** gremlins copies the whole working directory
> into each worker's isolated build dir without respecting `.gitignore`; a
> bare invocation with its scratch dir anywhere inside the repo caused each
> worker's copy to recursively sweep up scratch data from prior workers/runs,
> producing 168–394GB of disk usage from a single run and coinciding with
> kernel OOM kills. The wrapper relocates scratch space outside the repo and
> adds a memory cap (via a `systemd --user` cgroup scope) plus a disk-usage/
> wall-clock watchdog. Tunable via `GREMLINS_WORKERS`, `GREMLINS_MEMORY_MAX`,
> `GREMLINS_DISK_MAX_MB`, `GREMLINS_TIMEOUT_SECS`, `GREMLINS_DIR` — see the
> script's header comment. Mutant-type selection and `timeout-coefficient`
> are configured project-wide in `.gremlins.yaml`, not passed as flags.

> [!WARNING]
> **NEVER run `gremlins` on the entire repository** (e.g. `./...` or `./internal/...`). Doing so will trigger parallel builds and mutant execution across dozens of packages, which rapidly consumes disk space and will fill up `/tmp` (potentially causing system hangs or build failures) even with the wrapper script's protections. Always scope it to a single focused package.

<!-- -->

> [!WARNING]
> **`--diff` is currently broken when scoped to a package** — a confirmed
> upstream bug ([go-gremlins/gremlins#278](https://github.com/go-gremlins/gremlins/issues/278))
> makes it report every mutant as `SKIPPED` instead of evaluating them. Do not
> use `--diff`; instead run the whole-package command above and manually
> cross-reference `LIVED`/`NOT COVERED` line numbers against `git diff
> origin/main -- internal/<package>`. See
> [docs/mutation-testing-playbook.md](mutation-testing-playbook.md) § Known
> limitation for the full workaround.

See [docs/mutation-testing-playbook.md](mutation-testing-playbook.md) for the detailed triage guide and playbook.

## 8. Avoiding Flaky Tests

To maintain green CI and ensure testing reliability, follow these anti-pattern avoidance rules:

1. **No Absolute Wall-Clock Thresholds:** Never assert that a task finishes in `< Nms` or use tight context timeouts (e.g. `10ms`) as correctness gates. Under parallel testing and CPU contention, scheduling noise easily violates these. Use generous context timeouts (e.g. `5s`) as hang guards only.
2. **No `time.Sleep` Polling for Assertions:** Instead of sleeping a fixed duration and asserting state, use a polling loop helper (like `waitUntil`) with a generous timeout and small sleep intervals. Better yet, use channels or sync primitives (`sync.WaitGroup`) to wait for events deterministically. *(Exception: negative quiescence assertions — asserting no background mutation occurs over duration T — are an intentional exception where `time.Sleep` is appropriate).*
3. **ETXTBSY-Safe Mock Executables:** When writing mock executables/scripts to disk for `exec.Command` in tests, writing directly to the path with `os.WriteFile` risks `text file busy` errors if run concurrently. Use the following pattern:
   - Create a temp file via `os.CreateTemp`
   - Write the bytes and `f.Chmod(0o755)`
   - Call `f.Close()`
   - Rename to final target path using `os.Rename` (atomic rename prevents ETXTBSY races)
4. **Mutex-Guarded Callback Slices:** Any slice that accumulates logs asynchronously (e.g. `job.OnOutput = func(...) { lines = append(lines, line) }`) must be protected by a `sync.Mutex` to prevent data races under `-race`.
5. **Drain/Stop Background Workers Before Assertions:** When testing background tasks (e.g. checkpoint tickers), explicitly stop or drain the workers before making final assertions to prevent late asynchronous writes from polluting your state.

## 9. Decision Guide: Which Tests to Run

| Change area | Run |
|-------------|-----|
| Any Go code change | `go test -race ./...` |
| Post-processing, unpack, par2 | Add: `go test -v -tags=integration ./test/integration/...` |
| API endpoints | Add: `go test -v -tags=integration -run TestAPI ./test/integration/...` |
| Config struct fields | Add: `go test ./internal/config/ -run 'TestUIKeywordsAreValidConfigTags\|TestAllFlatConfigTagsAreSettable'` |
| Svelte UI keyword= props | Add: `go test ./internal/config/ -run 'TestUIKeywordsAreValidConfigTags\|TestAllFlatConfigTagsAreSettable'` |
| Svelte UI components, layout | Add: `go test -v -tags=uitest ./test/uitest/...` |
| NZB parsing, file naming | Add: `go test -v -tags=integration -run TestNaming ./test/integration/...` |
| Download pipeline | Add: `go test -v -tags=integration -run TestDownload ./test/integration/...` |
| Durability, checkpoints, assembler writes, resume | Add: `go test -count=1 -tags=crash -timeout=20m ./test/crash/` (all six must pass; see §3a for what a pass does and does not bound) |
| Pre-release validation | All: unit + integration + uitest + contract |
