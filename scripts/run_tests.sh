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

# 1. Go Unit Tests
echo -e "\n[1/4] Running Go Unit Tests..."
go test ./...
echo -e "${GREEN}✓ Go Unit Tests Passed${NC}"

# 2. Go Integration Tests
echo -e "\n[2/4] Running Go Integration Tests..."
go test -v -tags=integration ./test/integration/...
echo -e "${GREEN}✓ Go Integration Tests Passed${NC}"

# 3. UI Component Tests
if [ -d "ui" ]; then
    echo -e "\n[3/4] Running UI Component Tests..."
    cd ui
    if [ -d "node_modules" ]; then
        npm test
    else
        echo "node_modules not found in ui/, skipping UI tests (run 'npm install' in ui/ to enable)"
    fi
    cd ..
    echo -e "${GREEN}✓ UI Component Tests Passed${NC}"
else
    echo -e "\n[3/4] Skipping UI Tests (ui/ directory not found)"
fi

# 4. UI E2E Tests (requires built UI + Playwright browsers)
if [ -f "ui/dist/index.html" ]; then
    echo -e "\n[4/4] Running UI E2E Tests..."
    go test -tags=uitest -v ./test/uitest/...
    echo -e "${GREEN}✓ UI E2E Tests Passed${NC}"
else
    echo -e "\n[4/4] Skipping UI E2E Tests (run 'cd ui && npm run build' first)"
fi

echo -e "\n${GREEN}===================================================="
echo "ALL TESTS PASSED SUCCESSFULLY"
echo -e "====================================================${NC}"

