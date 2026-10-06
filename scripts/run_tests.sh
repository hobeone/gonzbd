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
# (e.g. `//go:build linux` in internal/cmdutil/sandbox_linux.go) -- those
# for other platforms are simply skipped on this host. GoNZBD is POSIX-only
# (see AGENTS.md), so this script does not need a foreign-GOOS pass; run
# `GOOS=darwin go vet ./...` / `GOOS=freebsd go vet ./...` by hand when a
# change touches an OS-constrained file.
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
go run scripts/check_test_doubles/main.go --all

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

# The three whole-repository checks (alongside check_test_doubles above). Unlike
# the diff-scoped checks they are NOT scoped to the diff, because what they
# examine is invisible to every other gate here: comments and Markdown are
# neither compiled nor executed, so a duplicated comment block that
# authoritatively describes the wrong declaration, or an audit snapshot that
# does not admit it is frozen, passes vet, lint and the whole test suite.
# Both found a real defect on their first run.
echo -e "\nRunning Duplicated-Comment Check..."
go run ./scripts/check_dup_comments
echo -e "${GREEN}✓ Duplicated-Comment Check Passed${NC}"

echo -e "\nRunning Review-Banner Check..."
go run ./scripts/check_review_banner
echo -e "${GREEN}✓ Review-Banner Check Passed${NC}"

# Mutation Specs Check: verifies that all mutation specs belonging to THIS
# checkout compile against the codebase, match valid anchors, and kill all
# mutations.
#
# Discovery goes through git, not `find`, because a spec is only meaningful
# against the tree it was written for. `scripts/mutate` takes its root from
# `git rev-parse --show-toplevel` in the CWD (scripts/mutate/main.go, repoRoot),
# so a spec found under a nested checkout is read from there and then resolved
# and mutated HERE -- against a different branch's source. `find` walks the
# filesystem and knows nothing about git, so it descended into the sibling
# worktrees under `.claude/` (ignored by .gitignore) and ran their specs against
# this tree; that surfaced as `ANCHOR -- anchor matched no site`, and would have
# produced a bogus KILLED/LIVED verdict instead had the anchor text happened to
# exist in both branches. `git ls-files` cannot leave the current worktree.
#
# `--others --exclude-standard` keeps a newly written, not-yet-staged spec in
# scope: tracked-only discovery would silently skip the spec you just wrote,
# which is the failure mode this check exists to prevent.
#
# To minimize wall-clock time, specs are executed in parallel across isolated
# detached Git worktrees. Each worktree is created from an ephemeral snapshot
# commit of the full working tree (staged, unstaged, untracked, and deleted
# files via a temporary git index), giving complete filesystem isolation without
# touching the real index or stash. `ui/dist` is copied into each worktree to
# satisfy `//go:embed all:dist` (which rejects symlinks).
echo -e "\nRunning Mutation Specs Check..."
REPO_ROOT=$(pwd)

# Prune any stale mutation worktrees left over from previously hard-killed runs.
TMP_DIR="${TMPDIR:-/tmp}"
while IFS= read -r line; do
    case "$line" in
        worktree\ *)
            wt_path="${line#worktree }"
            case "$wt_path" in
                "$TMP_DIR"/gonzbd-mutate-wt.*)
                    base_dir="${wt_path%/wt-*}"
                    if [ -f "$base_dir/owner.pid" ]; then
                        owner_pid=$(cat "$base_dir/owner.pid" 2>/dev/null || true)
                        if [ -n "$owner_pid" ] && kill -0 "$owner_pid" 2>/dev/null; then
                            continue
                        fi
                    fi
                    git worktree remove --force "$wt_path" >/dev/null 2>&1 || true
                    ;;
            esac
            ;;
    esac
done < <(git worktree list --porcelain)
git worktree prune >/dev/null 2>&1 || true

MUTATE_BIN=$(mktemp -t gonzbd-mutate.XXXXXX)
WORKTREE_BASE=""
SNAP_INDEX=""
PIDS=()

cleanup_mutate() {
    # Terminate worker process groups if still running
    if [ "${#PIDS[@]}" -gt 0 ]; then
        for p in "${PIDS[@]}"; do
            kill -TERM -- "-$p" 2>/dev/null || true
        done
        for p in "${PIDS[@]}"; do
            wait "$p" 2>/dev/null || true
        done
        PIDS=()
    fi
    if [ -n "$SNAP_INDEX" ] && [ -f "$SNAP_INDEX" ]; then
        rm -f "$SNAP_INDEX"
    fi
    if [ -n "$MUTATE_BIN" ] && [ -f "$MUTATE_BIN" ]; then
        rm -f "$MUTATE_BIN"
    fi
    if [ -n "$WORKTREE_BASE" ] && [ -d "$WORKTREE_BASE" ]; then
        if [ -n "${WORKERS:-}" ]; then
            for ((w=0; w<WORKERS; w++)); do
                git worktree remove --force "$WORKTREE_BASE/wt-$w" >/dev/null 2>&1 || true
            done
        fi
        rm -rf "$WORKTREE_BASE"
        git worktree prune >/dev/null 2>&1 || true
    fi
}
trap cleanup_mutate EXIT
trap 'cleanup_mutate; exit 130' INT
trap 'cleanup_mutate; exit 143' TERM

go build -o "$MUTATE_BIN" ./scripts/mutate

# Anchor check: resolves every spec's anchors against the current source
# without compiling, running a test, or writing anything, so a stale or
# ambiguous anchor fails here in a file scan rather than minutes into the
# parallel mutation run below.
echo -e "\nRunning Mutation Anchor Check (--check-all)..."
if ! "$MUTATE_BIN" --check-all; then
    echo -e "${RED}ERROR: one or more mutation spec anchors do not resolve to exactly one site.${NC}" >&2
    cleanup_mutate
    trap - EXIT INT TERM
    exit 1
fi
echo -e "${GREEN}✓ Mutation Anchor Check Passed${NC}"

mapfile -t SPECS < <(git ls-files --cached --others --exclude-standard -- '*testdata/*.spec' | sort -u)
NUM_SPECS=${#SPECS[@]}

if [ "$NUM_SPECS" -eq 0 ]; then
    echo "No mutation specs found."
    cleanup_mutate
    trap - EXIT INT TERM
    echo -e "${GREEN}✓ No Mutation Specs Found${NC}"
else
    if [ -n "${MUTATE_PARALLEL_WORKERS:-}" ]; then
        if ! [[ "$MUTATE_PARALLEL_WORKERS" =~ ^[0-9]+$ ]] || [ "$MUTATE_PARALLEL_WORKERS" -lt 1 ]; then
            echo -e "${RED}ERROR: MUTATE_PARALLEL_WORKERS must be a positive integer, got '$MUTATE_PARALLEL_WORKERS'${NC}" >&2
            exit 1
        fi
        WORKERS="$MUTATE_PARALLEL_WORKERS"
    else
        NUM_CPUS=$(nproc 2>/dev/null || echo 4)
        if [ "$NUM_CPUS" -ge 4 ]; then
            WORKERS=$(( NUM_CPUS / 2 ))
            if [ "$WORKERS" -gt 16 ]; then WORKERS=16; fi
        else
            WORKERS="$NUM_CPUS"
        fi
    fi
    if [ "$WORKERS" -gt "$NUM_SPECS" ]; then WORKERS="$NUM_SPECS"; fi

    # Compute per-worker CPU budget to prevent oversubscription timeouts
    NUM_CPUS=$(nproc 2>/dev/null || echo 4)
    CPU_BUDGET=$(( NUM_CPUS / WORKERS ))
    if [ "$CPU_BUDGET" -lt 1 ]; then CPU_BUDGET=1; fi

    WORKTREE_BASE=$(mktemp -d -t gonzbd-mutate-wt.XXXXXX)
    echo "$$" > "$WORKTREE_BASE/owner.pid"
    mkdir -p "$WORKTREE_BASE/logs"

    QUEUE_FILE="$WORKTREE_BASE/queue.txt"
    QUEUE_LOCK="$WORKTREE_BASE/queue.lock"
    RESULTS_FILE="$WORKTREE_BASE/results.txt"
    touch "$QUEUE_LOCK" "$RESULTS_FILE"
    # Schedule heaviest specs first (LPT) to minimize makespan and avoid tail stragglers
    for s in "${SPECS[@]}"; do
        echo "$(grep -c '^\[' "$s") $s"
    done | sort -rn | awk '{print $2}' > "$QUEUE_FILE"

    echo "Running $NUM_SPECS mutation specs in parallel across $WORKERS git worktrees..."

    # Create snapshot commit from a temporary index
    SNAP_INDEX=$(mktemp -t gonzbd-index.XXXXXX)
    cp "$(git rev-parse --git-path index)" "$SNAP_INDEX"
    GIT_INDEX_FILE="$SNAP_INDEX" git add -A
    SNAP_TREE=$(GIT_INDEX_FILE="$SNAP_INDEX" git write-tree)
    SNAP_COMMIT=$(git commit-tree "$SNAP_TREE" -p HEAD -m "mutation-worktree-snapshot")
    rm -f "$SNAP_INDEX"
    SNAP_INDEX=""

    for ((w=0; w<WORKERS; w++)); do
        wt="$WORKTREE_BASE/wt-$w"
        git worktree add -q --detach "$wt" "$SNAP_COMMIT"
        if [ -d "$REPO_ROOT/ui/dist" ]; then
            cp -a "$REPO_ROOT/ui/dist" "$wt/ui/dist"
        fi
    done

    # Pop next spec atomically from shared queue
    pop_spec() {
        (
            flock -x 200
            if [ -s "$QUEUE_FILE" ]; then
                head -n 1 "$QUEUE_FILE"
                sed -i '1d' "$QUEUE_FILE"
            fi
        ) 200>"$QUEUE_LOCK"
    }

    # Enable job control so each worker subshell runs in its own process group
    set -m
    for ((w=0; w<WORKERS; w++)); do
        (
            export GOMAXPROCS="$CPU_BUDGET"
            export GOFLAGS="${GOFLAGS:+$GOFLAGS }-p=$CPU_BUDGET"
            set -o pipefail
            cd "$WORKTREE_BASE/wt-$w"
            while true; do
                spec=$(pop_spec)
                [ -n "$spec" ] || break
                log_file="$WORKTREE_BASE/logs/$(echo "$spec" | tr '/' '_').log"
                # -q gives a passing spec one `ok` line and a failing one its
                # failing rows plus a rerun command. Output goes to a log and is
                # printed with one cat, which keeps workers' lines apart in the
                # common case of a short `ok` line. It is not atomic: a long
                # log, or the FAILED header followed by the log, can interleave.
                if "$MUTATE_BIN" -q -skip-runfilter "$spec" >"$log_file" 2>&1; then
                    cat "$log_file"
                    echo "$spec PASSED" >> "$RESULTS_FILE"
                else
                    echo -e "${RED}FAILED: $spec${NC}" >&2
                    cat "$log_file" >&2
                    echo "$spec FAILED" >> "$RESULTS_FILE"
                    exit 1
                fi
            done
        ) &
        PIDS+=($!)
    done
    set +m

    # Wait for workers using wait -n to catch failures immediately
    ACTIVE_PIDS=("${PIDS[@]}")
    FAILED=0
    while [ "${#ACTIVE_PIDS[@]}" -gt 0 ]; do
        wait_status=0
        wait -n -p done_pid "${ACTIVE_PIDS[@]}" || wait_status=$?
        NEW_ACTIVE=()
        for p in "${ACTIVE_PIDS[@]}"; do
            if [ "$p" -ne "$done_pid" ]; then
                NEW_ACTIVE+=("$p")
            fi
        done
        ACTIVE_PIDS=("${NEW_ACTIVE[@]}")

        if [ "$wait_status" -ne 0 ]; then
            FAILED=1
            for p in "${ACTIVE_PIDS[@]}"; do
                kill -TERM -- "-$p" 2>/dev/null || true
            done
            for p in "${ACTIVE_PIDS[@]}"; do
                wait "$p" 2>/dev/null || true
            done
            break
        fi
    done

    if [ "$FAILED" -ne 0 ]; then
        echo -e "${RED}ERROR: One or more mutation specs failed.${NC}" >&2
        echo "Unrun / interrupted specs:" >&2
        for spec in "${SPECS[@]}"; do
            if ! grep -q "^$spec " "$RESULTS_FILE" 2>/dev/null; then
                echo "  [not run] $spec" >&2
            fi
        done
        cleanup_mutate
        trap - EXIT INT TERM
        exit 1
    fi

    # Assert that all expected specs passed via set equality
    PASSED_SPECS=$(sed -n 's/ PASSED$//p' "$RESULTS_FILE" | sort -u)
    EXPECTED_SPECS=$(printf '%s\n' "${SPECS[@]}" | sort -u)
    if [ "$PASSED_SPECS" != "$EXPECTED_SPECS" ]; then
        echo -e "${RED}ERROR: Executed specs do not match expected spec list.${NC}" >&2
        cleanup_mutate
        trap - EXIT INT TERM
        exit 1
    fi
    RUN_COUNT=$(printf '%s\n' "$PASSED_SPECS" | grep -c . || echo 0)

    cleanup_mutate
    trap - EXIT INT TERM

    echo -e "${GREEN}✓ All Mutation Specs Killed ($RUN_COUNT/$NUM_SPECS)${NC}"
fi

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
