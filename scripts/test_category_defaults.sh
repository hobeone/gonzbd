#!/bin/bash
# test_category_defaults.sh - Verify per-category PP, Script, and Priority
# resolution against a running gonzbd instance.
#
# This script:
#  1. Reads categories from the running config via the API
#  2. Uploads a test NZB with cat=tv but WITHOUT explicit pp/script/priority
#  3. Queries the queue to verify the job inherited the category's defaults
#  4. Uploads another NZB WITH explicit overrides and verifies they win
#  5. Cleans up the test jobs
#
# Prerequisites:
#   - gonzbd running on localhost:4289 (or set GONZBD_URL)
#   - A valid API key (set GONZBD_APIKEY, or uses localhost_bypass)
#   - curl and jq installed
#
# Usage:
#   ./scripts/test_category_defaults.sh
#   GONZBD_URL=http://192.168.1.100:4289 GONZBD_APIKEY=abc123 ./scripts/test_category_defaults.sh

set -euo pipefail

# --- Configuration ---
BASE_URL="${GONZBD_URL:-http://localhost:4289}"
API_KEY="${GONZBD_APIKEY:-}"
API="${BASE_URL}/api"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
NC='\033[0m'

PASS=0
FAIL=0

# --- Helpers ---
api() {
    local mode="$1"; shift
    local url="${API}?mode=${mode}&output=json"
    if [ -n "$API_KEY" ]; then
        url="${url}&apikey=${API_KEY}"
    fi
    for arg in "$@"; do
        url="${url}&${arg}"
    done
    curl -sSf "$url"
}

api_post() {
    local url="${API}?output=json"
    if [ -n "$API_KEY" ]; then
        url="${url}&apikey=${API_KEY}"
    fi
    curl -sSf "$url" "$@"
}

assert_eq() {
    local label="$1" got="$2" want="$3"
    if [ "$got" = "$want" ]; then
        echo -e "  ${GREEN}✓${NC} ${label}: ${got}"
        ((PASS++))
    else
        echo -e "  ${RED}✗${NC} ${label}: got '${got}', want '${want}'"
        ((FAIL++))
    fi
}

cleanup_job() {
    local nzo_id="$1"
    if [ -n "$nzo_id" ]; then
        api queue "name=delete" "value=${nzo_id}" "del_files=1" >/dev/null 2>&1 || true
    fi
}

# Minimal valid NZB for testing (no real articles, just enough to parse).
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
NZB_FILE="${REPO_ROOT}/test/fixtures/nzb/simple.nzb"

if [ ! -f "$NZB_FILE" ]; then
    echo -e "${RED}Error: NZB fixture not found at ${NZB_FILE}${NC}"
    exit 1
fi

# --- Preflight: verify the server is reachable ---
echo "=================================================="
echo "Category Defaults Integration Test"
echo "=================================================="
echo -e "Target: ${YELLOW}${BASE_URL}${NC}"

if ! api version >/dev/null 2>&1; then
    echo -e "${RED}Error: Cannot reach gonzbd at ${BASE_URL}${NC}"
    echo "Make sure gonzbd is running: ./gonzbd --config gonzbd.yaml --serve"
    exit 1
fi
echo -e "${GREEN}✓ Server reachable${NC}"

# --- Step 1: Read current category config ---
echo ""
echo "[1/5] Reading category config..."
CONFIG_JSON=$(api get_config)
TV_CAT=$(echo "$CONFIG_JSON" | jq -r '.config.categories[] | select(.name == "tv") // empty')

if [ -z "$TV_CAT" ]; then
    echo -e "${YELLOW}No 'tv' category found. Creating a test category config..."
    echo "Add to your gonzbd.yaml under 'categories:':"
    echo "  - name: tv"
    echo "    pp: 3"
    echo "    script: notify.sh"
    echo "    priority: 1"
    echo "    dir: TV"
    echo ""
    echo "Then restart gonzbd and re-run this script."
    exit 1
fi

TV_PP=$(echo "$TV_CAT" | jq -r '.pp')
TV_SCRIPT=$(echo "$TV_CAT" | jq -r '.script')
TV_PRIORITY=$(echo "$TV_CAT" | jq -r '.priority')
TV_DIR=$(echo "$TV_CAT" | jq -r '.dir')

echo "  Category 'tv' config: pp=${TV_PP}, script=${TV_SCRIPT}, priority=${TV_PRIORITY}, dir=${TV_DIR}"

# Map numeric priority to the string the queue API returns.
# See internal/constants/priority.go for the full list.
priority_name() {
    case "$1" in
         3) echo "Repair" ;;
         2) echo "Force" ;;
         1) echo "High" ;;
         0) echo "Normal" ;;
        -1) echo "Low" ;;
        -2) echo "Paused" ;;
        *) echo "Normal" ;;
    esac
}
WANT_PRIORITY_STR=$(priority_name "$TV_PRIORITY")

# --- Step 2: Upload NZB with cat=tv, NO explicit pp/script/priority ---
echo ""
echo "[2/5] Uploading NZB with cat=tv (no explicit pp/script/priority)..."
UPLOAD1=$(api_post \
    -F "mode=addfile" \
    -F "cat=tv" \
    -F "nzbfile=@${NZB_FILE};filename=category_test_inherit.nzb")
NZO_ID1=$(echo "$UPLOAD1" | jq -r '.nzo_ids[0] // empty')

if [ -z "$NZO_ID1" ]; then
    echo -e "${RED}Error: Upload failed: ${UPLOAD1}${NC}"
    exit 1
fi
echo "  Job ID: ${NZO_ID1}"

# Pause it immediately so it doesn't try to download.
api queue "name=pause" "value=${NZO_ID1}" >/dev/null

# --- Step 3: Verify inherited values ---
echo ""
echo "[3/5] Verifying inherited category defaults..."
QUEUE_JSON=$(api queue "nzo_ids=${NZO_ID1}")
SLOT=$(echo "$QUEUE_JSON" | jq -r '.queue.slots[0]')

GOT_PP=$(echo "$SLOT" | jq -r '.pp')
GOT_SCRIPT=$(echo "$SLOT" | jq -r '.script')
GOT_PRIORITY=$(echo "$SLOT" | jq -r '.priority')
GOT_CATEGORY=$(echo "$SLOT" | jq -r '.cat')

assert_eq "category" "$GOT_CATEGORY" "tv"
assert_eq "pp (inherited)" "$GOT_PP" "$TV_PP"

# Script "None" (case-insensitive) means no script; API returns "none".
WANT_SCRIPT="$TV_SCRIPT"
if [ "$(echo "$WANT_SCRIPT" | tr '[:upper:]' '[:lower:]')" = "none" ]; then
    WANT_SCRIPT="none"
fi
assert_eq "script (inherited)" "$GOT_SCRIPT" "$WANT_SCRIPT"
assert_eq "priority (inherited)" "$GOT_PRIORITY" "$WANT_PRIORITY_STR"

# --- Step 4: Upload with explicit overrides ---
echo ""
echo "[4/5] Uploading NZB with explicit pp=1, script=custom.sh, priority=2..."
UPLOAD2=$(api_post \
    -F "mode=addfile" \
    -F "cat=tv" \
    -F "pp=1" \
    -F "script=custom.sh" \
    -F "priority=2" \
    -F "nzbfile=@${NZB_FILE};filename=category_test_override.nzb")
NZO_ID2=$(echo "$UPLOAD2" | jq -r '.nzo_ids[0] // empty')

if [ -z "$NZO_ID2" ]; then
    echo -e "${RED}Error: Upload failed: ${UPLOAD2}${NC}"
    cleanup_job "$NZO_ID1"
    exit 1
fi
echo "  Job ID: ${NZO_ID2}"
api queue "name=pause" "value=${NZO_ID2}" >/dev/null

QUEUE_JSON2=$(api queue "nzo_ids=${NZO_ID2}")
SLOT2=$(echo "$QUEUE_JSON2" | jq -r '.queue.slots[0]')

GOT_PP2=$(echo "$SLOT2" | jq -r '.pp')
GOT_SCRIPT2=$(echo "$SLOT2" | jq -r '.script')
GOT_PRIORITY2=$(echo "$SLOT2" | jq -r '.priority')

assert_eq "pp (explicit)" "$GOT_PP2" "1"
assert_eq "script (explicit)" "$GOT_SCRIPT2" "custom.sh"
assert_eq "priority (explicit)" "$GOT_PRIORITY2" "High"

# --- Step 5: Cleanup ---
echo ""
echo "[5/5] Cleaning up test jobs..."
cleanup_job "$NZO_ID1"
cleanup_job "$NZO_ID2"
echo -e "  ${GREEN}✓${NC} Cleaned up"

# --- Summary ---
echo ""
echo "=================================================="
TOTAL=$((PASS + FAIL))
if [ "$FAIL" -eq 0 ]; then
    echo -e "${GREEN}ALL ${TOTAL} CHECKS PASSED${NC}"
else
    echo -e "${RED}${FAIL}/${TOTAL} CHECKS FAILED${NC}"
fi
echo "=================================================="
exit "$FAIL"
