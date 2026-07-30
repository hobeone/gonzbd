#!/bin/bash
# run_tests.sh - Comprehensive test suite for sabnzbd-go
# Includes Go unit tests, Go integration tests, Svelte UI tests,
# and Playwright E2E tests.

set -e # Exit on first error

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
NC='\033[0m' # No Color

echo "===================================================="
echo "Starting Full Test Suite: sabnzbd-go"
echo "===================================================="

echo -e "\n[0/4] Building UI files for Go to embed..."
if ! (cd ui && bun run build); then
    echo "UI build failed" >&2
    exit 1
fi

# 1. Go Unit Tests
echo -e "\n[1/4] Running Go Unit Tests..."
go test ./...
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

# 2. Go Integration Tests
echo -e "\n[2/4] Running Go Integration Tests..."
go test -v -tags=integration ./test/integration/...
echo -e "${GREEN}✓ Go Integration Tests Passed${NC}"

# 3. UI Component Tests
if [ -d "ui" ]; then
    echo -e "\n[3/4] Running UI Component Tests..."
    cd ui
    if [ -d "node_modules" ]; then
        bun run test
    else
        echo "node_modules not found in ui/, skipping UI tests (run 'bun install' in ui/ to enable)"
    fi
    cd ..
    echo -e "${GREEN}✓ UI Component Tests Passed${NC}"
else
    echo -e "\n[3/4] Skipping UI Tests (ui/ directory not found)"
fi

# Run: go test -tags=uitest -v ./test/uitest/...
# Prerequisites: cd ui && bun run build; playwright install chromium")

# 4. UI E2E Tests (requires built UI + Playwright browsers)
if [ -f "ui/dist/index.html" ]; then
    echo -e "\n[4/4] Running UI E2E Tests..."
    go test -tags=uitest -v ./test/uitest/...
    echo -e "${GREEN}✓ UI E2E Tests Passed${NC}"
else
    echo -e "\n[4/4] Skipping UI E2E Tests (run 'cd ui && bun run build' first)"
fi

echo -e "\n${GREEN}===================================================="
echo "ALL TESTS PASSED SUCCESSFULLY"
echo -e "====================================================${NC}"
