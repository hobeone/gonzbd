#!/bin/bash
# run_tests.sh - Comprehensive test suite for sabnzbd-go
# Includes Go static analysis, linters, unit tests (-race), Go integration tests,
# the crash-consistency suite, Svelte UI checks/tests, and Playwright E2E tests.

set -e # Exit on first error

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
NC='\033[0m' # No Color

echo "===================================================="
echo "Starting Full Test Suite: sabnzbd-go"
echo "===================================================="

# Check prerequisites
echo -e "\nChecking prerequisites..."
MISSING=0

echo "Required tools:"
for cmd in go bun golangci-lint govulncheck par2 unrar 7z; do
    if cmd_path=$(command -v "$cmd" 2>/dev/null); then
        echo "  - $cmd: $cmd_path"
    else
        echo -e "${RED}ERROR: Required tool '$cmd' is missing from PATH.${NC}" >&2
        MISSING=1
    fi
done

if [ ! -d "ui" ]; then
    echo -e "${RED}ERROR: Required directory 'ui/' is missing.${NC}" >&2
    MISSING=1
fi

if [ ! -d "ui/node_modules" ]; then
    echo -e "${RED}ERROR: 'ui/node_modules/' is missing. Run 'bun install' in ui/.${NC}" >&2
    MISSING=1
fi

if [ "$MISSING" -ne 0 ]; then
    echo -e "${RED}Prerequisites check failed. Aborting test suite.${NC}" >&2
    exit 1
fi
echo -e "${GREEN}✓ All prerequisites met${NC}"

# 0. UI Validation & Build
echo -e "\n[0/7] Checking and Building UI..."
(
    cd ui
    echo "Running UI Type-Check..."
    bun run check
    echo "Building UI..."
    bun run build
)
echo -e "${GREEN}✓ UI Build & Type-Check Passed${NC}"

# 1. Static Analysis & Linters
echo -e "\n[1/7] Running Static Analysis & Linters..."
echo "Running go vet..."
go vet ./...

# The same vet again, with the three build tags this repo defines
# (integration, uitest, crash). Two things to know about it:
#
# It is a SUPERSET of `go vet ./...` above, not a complement. Build tags are
# additive and nothing here carries a negative constraint (`git grep -c
# '^//go:build.*!\(integration\|uitest\|crash\)' -- '*.go'` prints nothing
# and exits 1, which is how git grep reports no match -- do not run it under
# `set -e`), so this pass analyses every package the plain pass does.
# The plain pass is kept anyway, deliberately: it runs first, so an untagged
# problem is reported on its own before a broken tagged file can fail a whole
# package and hide it. Measured cost of keeping it, on a cold cache: 6.9 s for
# the plain pass and 2.2 s for this one after it, against 6.9 s for this one
# alone -- so the duplication is ~2.2 s, not a second full pass.
#
# What it covers, exactly: files carrying one of those three tags, built for
# the HOST GOOS/GOARCH. It says nothing about files behind an OS constraint
# (internal/fsutil/crossdevice_windows.go and eight others are invisible on
# Linux, and crossdevice_windows.go does not currently compile under
# GOOS=windows -- see #480).
#
# Why it is needed at all: the tagged suites below are each path-scoped (step 3
# to ./test/integration/... and ./internal/par2/..., step 4 to ./test/crash/,
# step 6 to ./test/uitest/...), so nothing else in this script would otherwise
# compile a tagged file outside its own path -- e.g. a build-tagged file added
# under internal/ that no path-scoped step happens to cover. That is the gap
# that let internal/app/integration_test.go sit uncompilable for six weeks
# (#475). Step 4 below also compiles test/crash/ as a side effect of running
# it, so this pass is redundant for that one tag specifically -- kept anyway
# because it is one flag away from the other two and confirms compilation
# before step 2's ~100s race run, rather than after it.
#
# -tags=e2e is absent on purpose: test/e2e carries no build constraint at all
# and is gated at runtime by E2E_CONFIG. See test/e2e/e2e_test.go's package doc.
echo "Running go vet over build-tagged files..."
go vet -tags=integration,uitest,crash ./...

echo "Running golangci-lint..."
golangci-lint run ./...

echo "Running Test Double Guard Check..."
go run scripts/check_test_doubles/main.go

# govulncheck's status is captured rather than left to `set -e`, and reported
# at the very end.
#
# It exits non-zero for any finding, including one in the Go standard library
# that is fixed only by upgrading the toolchain. Under `set -e` that aborted
# the script at this line, so steps 2-6 — every Go test, every UI test — never
# ran, and the run still looked like "the suite failed on vulnerabilities". A
# finding must not be able to hide the test results; the script still fails, it
# just fails after reporting everything.
echo "Running govulncheck..."
VULN_STATUS=0
govulncheck ./... || VULN_STATUS=$?
if [ "$VULN_STATUS" -ne 0 ]; then
    echo -e "${RED}✗ govulncheck reported findings (status $VULN_STATUS) - continuing so the tests still run${NC}"
fi
echo -e "${GREEN}✓ Static Analysis & Linters Passed${NC}"

# 2. Go Unit Tests (with Race Detector)
echo -e "\n[2/7] Running Go Unit Tests (with race detector)..."
go test -race -p 32 ./...
echo -e "${GREEN}✓ Go Unit Tests Passed${NC}"

# Go Test Alignment Check (unexported helpers coverage check).
# Scoped to changed files (git-diff mode) and gated at --min-complexity=8 so
# only substantive untested helpers block the build; trivial one-liners are
# reported for triage via `--all` but do not fail CI. Run
# `go run scripts/check_test_alignment/main.go --all --min-complexity=N` to
# survey the wider backlog.
echo -e "\nRunning Go Test Alignment Check..."
go run scripts/check_test_alignment/main.go --min-complexity=8
echo -e "${GREEN}✓ Go Test Alignment Check Passed${NC}"

# Go Test Coverage Check
echo -e "\nRunning Go Test Coverage Check..."
go run scripts/check_coverage/main.go
echo -e "${GREEN}✓ Go Test Coverage Check Passed${NC}"

# Mutex-held-during-I/O Check (see docs/go-standards.md).
echo -e "\nRunning Mutex-held-during-I/O Check..."
go run scripts/check_lock_io/main.go
echo -e "${GREEN}✓ Mutex-held-during-I/O Check Passed${NC}"

# The two whole-repository checks. Unlike the three above they are NOT scoped
# to the diff, because what they examine is invisible to every other gate here:
# comments and Markdown are neither compiled nor executed, so a duplicated
# comment block that authoritatively describes the wrong declaration, or an
# audit snapshot that does not admit it is frozen, passes vet, lint and the
# whole test suite. Both found a real defect on their first run.
echo -e "\nRunning Duplicated-Comment Check..."
go run ./scripts/check_dup_comments
echo -e "${GREEN}✓ Duplicated-Comment Check Passed${NC}"

echo -e "\nRunning Review-Banner Check..."
go run ./scripts/check_review_banner
echo -e "${GREEN}✓ Review-Banner Check Passed${NC}"

# 3. Go Integration Tests
echo -e "\n[3/7] Running Go Integration Tests..."
go test -v -tags=integration ./test/integration/... ./internal/par2/...
echo -e "${GREEN}✓ Go Integration Tests Passed${NC}"

# 4. Crash-Consistency Tests
#
# Linux-only: `TestMain` builds `./cmd/gonzbd` into a temp directory itself
# and SIGKILLs it as a real child process (docs/TESTING.md §3a), so this step
# assumes a Linux host that can build and signal a child process. It is not
# gated by `uname -s` the way the `crash && linux` build constraint itself is
# -- a non-Linux run fails loudly here rather than silently skipping, which
# is the intended behaviour: this script's other prerequisite checks (par2,
# unrar, 7z, bun) already fail the same way on a missing dependency rather
# than skip the step that needs it.
echo -e "\n[4/7] Running Crash-Consistency Tests..."
go test -tags=crash -timeout=20m ./test/crash/
echo -e "${GREEN}✓ Crash-Consistency Tests Passed${NC}"

# 5. UI Component Tests
echo -e "\n[5/7] Running UI Component Tests..."
(
    cd ui
    bun run test
)
echo -e "${GREEN}✓ UI Component Tests Passed${NC}"

# 6. UI E2E Tests (requires built UI + Playwright browsers)
echo -e "\n[6/7] Running UI E2E Tests..."
go test -tags=uitest -v ./test/uitest/...
echo -e "${GREEN}✓ UI E2E Tests Passed${NC}"

if [ "$VULN_STATUS" -ne 0 ]; then
    echo -e "\n${RED}===================================================="
    echo "TESTS PASSED, BUT govulncheck REPORTED FINDINGS"
    echo "Re-run 'govulncheck ./...' for the details."
    echo -e "====================================================${NC}"
    exit "$VULN_STATUS"
fi

echo -e "\n${GREEN}===================================================="
echo "ALL TESTS PASSED SUCCESSFULLY"
echo -e "====================================================${NC}"
