#!/bin/bash
# run_tests_parallel.sh - Parallel variant of run_tests.sh.
#
# The original script runs four phases head-to-tail:
#   Go unit -> Go integration -> UI vitest -> bun run build -> UI E2E (uitest)
#
# Only one real ordering constraint exists: the UI E2E tests embed
# ui/dist/ via //go:embed at compile time, so they must run AFTER
# 'bun run build' completes. Everything else is independent.
#
# This script kicks off all independent phases concurrently, then runs
# the E2E tests once the build finishes. Output from each phase is
# tee'd to logs/<phase>.log; on failure the relevant log is printed.

set -u   # not -e: we manage failures manually so we can wait on every job
         # and report which phase(s) failed.

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
NC='\033[0m'

LOG_DIR="logs"
mkdir -p "$LOG_DIR"
rm -f "$LOG_DIR"/*.log

start_ms() { date +%s%3N; }
elapsed_ms() { echo $(( $(date +%s%3N) - $1 )); }

# run_phase <name> <log file> <command...>
# Runs the command in the background, redirecting all output to the
# log file. Stores its PID and start time in shadow arrays keyed by
# name, so the main flow can wait on it later and report timing.
declare -A PHASE_PID
declare -A PHASE_START
declare -A PHASE_LOG
declare -A PHASE_END_FILE
run_phase() {
    local name="$1" log="$2"
    shift 2
    PHASE_LOG[$name]="$log"
    PHASE_START[$name]=$(start_ms)
    PHASE_END_FILE[$name]="$LOG_DIR/$name.end"
    rm -f "${PHASE_END_FILE[$name]}"
    # Record completion timestamp inside the subshell so per-phase wall
    # time reflects when the command actually exited, not when the main
    # script's wait happened to notice.
    (
        "$@" >"$log" 2>&1
        rc=$?
        date +%s%3N > "${PHASE_END_FILE[$name]}"
        exit $rc
    ) &
    PHASE_PID[$name]=$!
}

# wait_phase <name> -> writes elapsed time, returns command exit status
wait_phase() {
    local name="$1"
    local pid="${PHASE_PID[$name]}"
    wait "$pid"
    local rc=$?
    local end="" elapsed
    if [ -r "${PHASE_END_FILE[$name]}" ]; then
        end=$(cat "${PHASE_END_FILE[$name]}")
    fi
    if [ -n "$end" ]; then
        elapsed=$(( end - PHASE_START[$name] ))
    else
        elapsed=$(elapsed_ms "${PHASE_START[$name]}")
    fi
    if [ $rc -eq 0 ]; then
        printf "${GREEN}OK${NC}    %-18s %6d ms\n" "$name" "$elapsed"
    else
        printf "${RED}FAIL${NC}  %-18s %6d ms  (see ${PHASE_LOG[$name]})\n" \
            "$name" "$elapsed"
    fi
    return $rc
}

echo "===================================================="
echo "Parallel Test Suite: gonzbd"
echo "===================================================="
total_start=$(start_ms)

# --- Group A: phases that have no dependencies on each other ---
# Kick all four off immediately; the bun build is the gating dependency
# for the uitest phase, so we want it running first.
run_phase "bun-build"      "$LOG_DIR/bun-build.log"      bash -c 'cd ui && bun run build'
run_phase "go-unit"        "$LOG_DIR/go-unit.log"        go test ./...
run_phase "go-integration" "$LOG_DIR/go-integration.log" go test -tags=integration ./test/integration/... ./internal/par2/...

# UI vitest only runs if node_modules exists (parity with original script).
if [ -d "ui/node_modules" ]; then
    run_phase "ui-vitest"  "$LOG_DIR/ui-vitest.log"      bash -c 'cd ui && bun run test -- --run'
else
    echo -e "${YELLOW}SKIP${NC}  ui-vitest          (ui/node_modules missing)"
fi

# --- Wait for the build before launching the dependent uitest phase. ---
fail=0
if ! wait_phase "bun-build"; then
    fail=1
fi

# --- Group B: dependent on bun build ---
# Only launch the uitest phase if the build succeeded; otherwise the
# embedded dist would be wrong and the failure would be misleading.
if [ $fail -eq 0 ] && [ -f "ui/dist/index.html" ]; then
    run_phase "go-uitest"  "$LOG_DIR/go-uitest.log"      go test -tags=uitest ./test/uitest/...
elif [ $fail -ne 0 ]; then
    echo -e "${YELLOW}SKIP${NC}  go-uitest          (build failed)"
else
    echo -e "${YELLOW}SKIP${NC}  go-uitest          (ui/dist/index.html missing)"
fi

# --- Wait on the remaining concurrent jobs in any order. ---
wait_phase "go-unit"        || fail=1
wait_phase "go-integration" || fail=1
[ -d "ui/node_modules" ]    && { wait_phase "ui-vitest" || fail=1; }
[ -n "${PHASE_PID[go-uitest]:-}" ] && { wait_phase "go-uitest" || fail=1; }

total_elapsed=$(elapsed_ms "$total_start")
echo "----------------------------------------------------"
printf "TOTAL: %d ms\n" "$total_elapsed"
echo "----------------------------------------------------"

if [ $fail -eq 0 ]; then
    echo -e "${GREEN}ALL TESTS PASSED${NC}"
    exit 0
fi

# On failure, dump each failed phase's log so CI output is self-contained.
echo -e "${RED}One or more phases failed. Logs:${NC}"
for name in "${!PHASE_LOG[@]}"; do
    log="${PHASE_LOG[$name]}"
    if [ -s "$log" ]; then
        echo "===== $name ($log) ====="
        cat "$log"
    fi
done
exit 1
