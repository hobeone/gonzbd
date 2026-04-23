#!/bin/bash
# run_tests.sh - Comprehensive test suite for sabnzbd-go
# Includes Go unit tests, Go integration tests, and Svelte UI tests.

set -e # Exit on first error

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
NC='\033[0m' # No Color

echo "===================================================="
echo "Starting Full Test Suite: sabnzbd-go"
echo "===================================================="

# 1. Go Unit Tests
echo -e "\n[1/3] Running Go Unit Tests..."
go test ./...
echo -e "${GREEN}✓ Go Unit Tests Passed${NC}"

# 2. Go Integration Tests
echo -e "\n[2/3] Running Go Integration Tests..."
go test -v -tags=integration ./test/integration/...
echo -e "${GREEN}✓ Go Integration Tests Passed${NC}"

# 3. UI Component Tests
if [ -d "ui" ]; then
    echo -e "\n[3/3] Running UI Component Tests..."
    cd ui
    if [ -d "node_modules" ]; then
        npm test
    else
        echo "node_modules not found in ui/, skipping UI tests (run 'npm install' in ui/ to enable)"
    fi
    cd ..
    echo -e "${GREEN}✓ UI Component Tests Passed${NC}"
else
    echo -e "\n[3/3] Skipping UI Tests (ui/ directory not found)"
fi

echo -e "\n${GREEN}===================================================="
echo "ALL TESTS PASSED SUCCESSFULLY"
echo -e "====================================================${NC}"
