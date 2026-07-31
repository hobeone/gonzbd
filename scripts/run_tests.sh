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

# 0. UI Validation & Build
if [ -d "ui" ]; then
    echo -e "\n[0/6] Checking and Building UI..."
    (
        cd ui
        if [ -d "node_modules" ]; then
            echo "Running UI Type-Check..."
            bun run check
        fi
        echo "Building UI..."
        bun run build
    )
    echo -e "${GREEN}✓ UI Build & Type-Check Passed${NC}"
else
    echo -e "\n[0/6] Skipping UI Build (ui/ directory not found)"
fi

# 1. Static Analysis & Linters
echo -e "\n[1/6] Running Static Analysis & Linters..."
echo "Running go vet..."
go vet ./...

if command -v golangci-lint >/dev/null 2>&1; then
    echo "Running golangci-lint..."
    golangci-lint run ./...
else
    echo "golangci-lint not found in PATH, skipping"
fi

if command -v govulncheck >/dev/null 2>&1; then
    echo "Running govulncheck..."
    govulncheck ./...
else
    echo "govulncheck not found in PATH, skipping"
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

# 3. Go Integration Tests
echo -e "\n[3/6] Running Go Integration Tests..."
go test -v -tags=integration ./test/integration/...
echo -e "${GREEN}✓ Go Integration Tests Passed${NC}"

# 4. UI Component Tests
if [ -d "ui" ]; then
    echo -e "\n[4/6] Running UI Component Tests..."
    cd ui
    if [ -d "node_modules" ]; then
        bun run test
    else
        echo "node_modules not found in ui/, skipping UI tests (run 'bun install' in ui/ to enable)"
    fi
    cd ..
    echo -e "${GREEN}✓ UI Component Tests Passed${NC}"
else
    echo -e "\n[4/6] Skipping UI Component Tests (ui/ directory not found)"
fi

# 5. UI E2E Tests (requires built UI + Playwright browsers)
if [ -f "ui/dist/index.html" ]; then
    echo -e "\n[5/6] Running UI E2E Tests..."
    go test -tags=uitest -v ./test/uitest/...
    echo -e "${GREEN}✓ UI E2E Tests Passed${NC}"
else
    echo -e "\n[5/6] Skipping UI E2E Tests (run 'cd ui && bun run build' first)"
fi

echo -e "\n${GREEN}===================================================="
echo "ALL TESTS PASSED SUCCESSFULLY"
echo -e "====================================================${NC}"

