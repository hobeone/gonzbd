#!/bin/bash
# run_tests.sh - Comprehensive test suite for sabnzbd-go
# Includes Go static analysis, linters, unit tests (-race), Go integration tests,
# Svelte UI checks/tests, and Playwright E2E tests.

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
echo -e "\n[0/6] Checking and Building UI..."
(
    cd ui
    echo "Running UI Type-Check..."
    bun run check
    echo "Building UI..."
    bun run build
)
echo -e "${GREEN}✓ UI Build & Type-Check Passed${NC}"

# 1. Static Analysis & Linters
echo -e "\n[1/6] Running Static Analysis & Linters..."
echo "Running go vet..."
go vet ./...

# The same vet again, with every build tag in use. `go vet ./...` above sees
# only untagged files, and the tagged suites below are each path-scoped
# (step 3 to ./test/integration/..., step 5 to ./test/uitest/...), so six
# tagged files are compiled by nothing else in this script:
# internal/par2/par2_integration_test.go and all five of test/crash/. That gap
# let internal/app/integration_test.go sit uncompilable for six weeks (#475).
#
# This matters most for test/crash/, which is deliberately NOT run here (see
# the note before the summary block below) — so on a machine where the crash
# suite cannot run at all, this is the only thing that compiles it.
#
# Compiles without running, so it needs none of the tools those suites need.
# -tags=e2e is absent on purpose: test/e2e carries no build constraint
# (`git grep -c '^//go:build' -- 'test/e2e/*.go'` returns 0 matching lines)
# and is gated at runtime by E2E_CONFIG instead.
echo "Running go vet over build-tagged files..."
go vet -tags=integration,uitest,crash ./...

echo "Running golangci-lint..."
golangci-lint run ./...

# govulncheck's status is captured rather than left to `set -e`, and reported
# at the very end.
#
# It exits non-zero for any finding, including one in the Go standard library
# that is fixed only by upgrading the toolchain. Under `set -e` that aborted
# the script at this line, so steps 2-5 — every Go test, every UI test — never
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
echo -e "\n[2/6] Running Go Unit Tests (with race detector)..."
go test -race ./...
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
echo -e "\n[3/6] Running Go Integration Tests..."
go test -v -tags=integration ./test/integration/...
echo -e "${GREEN}✓ Go Integration Tests Passed${NC}"

# 4. UI Component Tests
echo -e "\n[4/6] Running UI Component Tests..."
(
    cd ui
    bun run test
)
echo -e "${GREEN}✓ UI Component Tests Passed${NC}"

# 5. UI E2E Tests (requires built UI + Playwright browsers)
echo -e "\n[5/6] Running UI E2E Tests..."
go test -tags=uitest -v ./test/uitest/...
echo -e "${GREEN}✓ UI E2E Tests Passed${NC}"

# The crash-consistency suite (`-tags=crash`, test/crash/) is deliberately NOT
# run here. It builds and SIGKILLs a real child process, so it needs a working
# `go build` of ./cmd/gonzbd and is Linux-only, and its cost is dominated by
# process startup rather than by anything this script already does. Keeping it
# out means this script stays runnable on a machine where the child cannot be
# built or killed. All six of its tests pass (docs/TESTING.md §3a). Run it
# directly:
#
#   go test -tags=crash -timeout=20m ./test/crash/

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


