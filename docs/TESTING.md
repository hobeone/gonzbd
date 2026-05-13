# GoNZBD Testing Guide

This document describes all test suites in the GoNZBD project, their purpose,
required tools, and when to run each one. **Agents must read this file before
running or modifying tests.**

## Quick Reference

| Suite | Command | Build Tag | External Tools | Duration |
|-------|---------|-----------|---------------|----------|
| Unit tests | `go test ./...` | none | none | ~30s |
| Unit + race | `go test -race ./...` | none | none | ~100s |
| Integration | `go test -v -tags=integration ./test/integration/...` | `integration` | par2, rar, unrar, 7z | ~35s |
| UI (Playwright) | `go test -v -tags=uitest ./test/uitest/...` | `uitest` | Chromium (Playwright), pre-built UI | ~30s |
| E2E | `go test -tags=e2e -timeout=10m ./test/e2e/` | `e2e` | live Usenet server | ~5min |
| Config contract | `go test ./internal/config/ -run 'TestUI\|TestAllFlat'` | none | none | <1s |

## 1. Unit Tests (`go test ./...`)

**When to run:** Before every commit. Required to pass with `-race`.

Standard Go unit tests across all packages. No external dependencies.
Includes app-level scenario tests (`internal/app/scenario_test.go`,
`scenario_recovery_test.go`) which use an in-process mock NNTP server
to test download → assembly → post-processing flows without shelling
out to real tools.

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
| `rss_test.go` | RSS feed processing: max age, dedup, include/exclude filters, size bounds |
| `statemachine_test.go` | Download state machine chaos testing |
| `duplicate_test.go` | Duplicate detection |
| `oneshot_test.go` | One-shot CLI download mode |
| `truncation_test.go` | Truncated article handling |
| `replay_test.go` | Historical NZB replay (requires fixtures) |

### Running

```bash
# Full suite (verbose recommended for pipeline tests)
go test -v -tags=integration ./test/integration/...

# Single test
go test -v -tags=integration -run TestPipeline_SubdirectoryUnrar ./test/integration/...

# With race detector (slower but catches concurrency bugs)
go test -v -race -tags=integration ./test/integration/...
```

### Adding New Integration Tests

1. Use `requireTool(t, "toolname")` for any external binary dependency
2. Use `NewTestAppSeparateDirs(t, addr, AppTestOpts{...})` for pipeline tests
3. Use `createRarFixture` / `create7zFixture` / `createPar2Fixture` for archive fixtures
4. Use `fixtureToTestFiles` for flat fixtures, `fixtureToTestFilesRecursive` for subdirectory structures
5. Use `waitForPostProcWithTimeout(t, a, pipelineTimeout)` to wait for completion
6. Use `verifyFileAtPath(t, path, sha)` for strict path + content verification

## 3. E2E Tests (`-tags=e2e`)

**When to run:** Manually, to validate against a real Usenet provider.
Not run in CI.

**Location:** `test/e2e/`

**Build tag:** `e2e`

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
go test -tags=e2e -timeout=10m ./test/e2e/

# Self-post + download with debug
E2E_POST=1 E2E_DEBUG=1 go test -tags=e2e -timeout=10m -v ./test/e2e/

# Download a specific NZB
E2E_NZB=/tmp/test.nzb go test -tags=e2e -timeout=10m ./test/e2e/
```

## 4. Config ↔ UI Contract Tests

**When to run:** After any change to:
- Go config structs (`internal/config/`)
- Svelte config components (`ui/src/lib/components/config/`)
- Any `keyword=` prop in a `ConfigInput`, `ConfigSwitch`, or `ConfigTextarea`

**Location:** `internal/config/ui_contract_test.go`

These tests validate that every `keyword=` prop used in the Svelte UI
corresponds to a valid JSON tag in the Go config structs, and vice versa.

```bash
go test ./internal/config/ -run 'TestUI|TestAllFlat'
```

## 5. UI Tests (`-tags=uitest`)

**When to run:** After changes to:
- Svelte UI components (`ui/src/`)
- Web handler / SPA serving (`internal/web/`)
- API response shapes that the UI consumes (`internal/api/`)
- Any visual/layout/interaction behavior

**Location:** `test/uitest/`

**Build tag:** `uitest`

Browser-based integration tests using [Playwright for Go](https://github.com/playwright-community/playwright-go).
Tests spin up the full Go backend with the embedded Svelte SPA and drive a
real Chromium browser against it.

### Prerequisites

1. **Build the UI first:** `cd ui && npm run build`
2. **Install Playwright browsers** (cached in `~/.cache/ms-playwright-go/`):
   ```bash
   go run github.com/playwright-community/playwright-go/cmd/playwright install --with-deps chromium
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

**Location:** `internal/app/scenario_test.go`, `internal/app/scenario_recovery_test.go`

These test the full app lifecycle (download → assembly → post-processing)
using an in-process mock NNTP server. They do **not** shell out to external
tools — post-processing stages that require `par2`/`unrar`/`7z` are stubbed.

## Decision Guide: Which Tests to Run

| Change area | Run |
|-------------|-----|
| Any Go code change | `go test -race ./...` |
| Post-processing, unpack, par2 | Add: `go test -v -tags=integration ./test/integration/...` |
| API endpoints | Add: `go test -v -tags=integration -run TestAPI ./test/integration/...` |
| Config struct fields | Add: `go test ./internal/config/ -run 'TestUI\|TestAllFlat'` |
| Svelte UI keyword= props | Add: `go test ./internal/config/ -run 'TestUI\|TestAllFlat'` |
| Svelte UI components, layout | Add: `go test -v -tags=uitest ./test/uitest/...` |
| NZB parsing, file naming | Add: `go test -v -tags=integration -run TestNaming ./test/integration/...` |
| Download pipeline | Add: `go test -v -tags=integration -run TestDownload ./test/integration/...` |
| RSS feed processing | Add: `go test -v -tags=integration -run TestRSS ./test/integration/...` |
| Pre-release validation | All: unit + integration + uitest + contract |
