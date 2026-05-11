#!/bin/bash
# run_fuzz.sh - Run fuzz tests against untrusted-input parsers.
#
# Usage:
#   ./scripts/run_fuzz.sh              # 60s per target (default)
#   ./scripts/run_fuzz.sh 5m           # 5 minutes per target
#   ./scripts/run_fuzz.sh 0            # run until interrupted (Ctrl-C)
#
# Fuzz targets cover the four packages that parse untrusted data from
# the internet: NZB XML, yEnc articles, UU-encoded articles, and raw
# NNTP wire protocol.

set -e

FUZZTIME="${1:-60s}"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

TARGETS=(
    "FuzzParse:./internal/nzb/"
    "FuzzDecodeArticle:./internal/decoder/"
    "FuzzDecodeUU:./internal/decoder/"
    "FuzzReadDotStuffedBody:./internal/nntp/"
)

echo "===================================================="
echo "GoNZBD Fuzz Test Suite"
echo "===================================================="
echo -e "Duration per target: ${YELLOW}${FUZZTIME}${NC}"
echo -e "Targets: ${#TARGETS[@]}"
echo ""

FAILED=0
PASSED=0

for entry in "${TARGETS[@]}"; do
    FUNC="${entry%%:*}"
    PKG="${entry##*:}"
    echo -e "[$(( PASSED + FAILED + 1 ))/${#TARGETS[@]}] Fuzzing ${YELLOW}${FUNC}${NC} in ${PKG}..."

    if [ "$FUZZTIME" = "0" ]; then
        # Run indefinitely — user must Ctrl-C.
        go test -fuzz="^${FUNC}$" "$PKG" -parallel=4
    else
        if go test -fuzz="^${FUNC}$" "$PKG" -fuzztime="$FUZZTIME" -parallel=4; then
            echo -e "  ${GREEN}✓ ${FUNC} passed${NC}"
            (( PASSED++ ))
        else
            echo -e "  ${RED}✗ ${FUNC} FAILED — check testdata/ for crashing inputs${NC}"
            (( FAILED++ ))
        fi
    fi
    echo ""
done

echo "===================================================="
if [ "$FAILED" -eq 0 ]; then
    echo -e "${GREEN}All ${PASSED} fuzz targets passed (${FUZZTIME} each)${NC}"
else
    echo -e "${RED}${FAILED} fuzz target(s) found issues${NC}"
    echo "Crashing inputs are saved in the package's testdata/fuzz/ directory."
    echo "Re-run 'go test ./internal/...' to reproduce failures as unit tests."
    exit 1
fi
