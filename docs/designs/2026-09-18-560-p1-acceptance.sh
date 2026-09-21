#!/bin/bash
set -euo pipefail

# Acceptance checklist for round 1 (P1) of the issue 560 implementation
# comparison: design §3.1, docs/designs/2026-09-18-560-owner-store-design.md.
#
# Run from the root of an implementation checkout. The script need not live in
# that checkout — the implementation branches start from main, which does not
# contain this file — so fetch it from the design branch's pinned commit:
#
#   git show <design-commit>:docs/designs/2026-09-18-560-p1-acceptance.sh > /tmp/p1-accept.sh
#   bash /tmp/p1-accept.sh --base f83c366d [--static-only] [--full] [--crash N]
#
# Static checks are the design's P1 invariants that a grep can decide. They
# check what the design names (Dispatcher.Add, snapshotOrder, claimLaunched,
# removal.end), not how the code is written. Dynamic checks run the AGENTS.md
# gates the comparison holds both branches to. Every check prints PASS, FAIL or
# SKIP; the exit status is 1 if any check FAILs.
#
# A static check matches the spelling the design uses, so a sound
# implementation can fail one by being shaped differently (the unwind moved
# into a helper, say). A static FAIL is therefore a demand for an explanation in
# the PR's deviation log, not an automatic loss. A dynamic FAIL is a real one.
#
# Checked against unmodified main (f83c366d): the ten P1 requirements FAIL, and
# the five invariants that already hold and must survive (S5, S7-S10) PASS.

base="f83c366d"
static_only=false
full=false
crash_count=0

while [[ $# -gt 0 ]]; do
    case "$1" in
        --base) base="${2:?--base needs a commit}"; shift 2 ;;
        --static-only) static_only=true; shift ;;
        --full) full=true; shift ;;
        --crash) crash_count="${2:?--crash needs a run count}"; shift 2 ;;
        *) echo "unknown argument: $1" >&2; exit 2 ;;
    esac
done

root="$(git rev-parse --show-toplevel)"
cd "$root"
git rev-parse --verify --quiet "${base}^{commit}" >/dev/null || { echo "base $base is not a commit here" >&2; exit 2; }

failures=0
pass() { echo "PASS  $1"; }
fail() { echo "FAIL  $1"; failures=$((failures + 1)); }
skip() { echo "SKIP  $1"; }

# func_body PREFIX: print the body of the top-level Go function whose
# declaration line starts with the fixed string PREFIX, searching production
# files under internal/. gofmt puts a top-level function's closing brace alone
# at column 0, which is what ends the range. Prints nothing if not found.
func_body() {
    local prefix="$1" file
    file="$(git grep -lF -- "$prefix" -- 'internal/*.go' ':!*_test.go' | head -1 || true)"
    [[ -n "$file" ]] || return 0
    awk -v p="$prefix" 'index($0, p) == 1 { f = 1 } f { print } f && /^}/ { exit }' "$file"
}

# prod_count PATTERN PATHSPEC...: number of production code lines matching the
# extended regex PATTERN. Comment lines are excluded: comments in this
# repository quote call spellings (registry.go's deregister doc quotes
# `d.deregister("j1")`), and counting them would fail a correct tree.
prod_count() {
    local pattern="$1"; shift
    # `|| true` because git grep exits 1 on no matches, and under
    # `set -o pipefail` that aborted the whole script at the first check whose
    # PASSING condition is zero matches (S11) — taking S12-S14 and every
    # dynamic check with it, silently, on exactly the trees that were correct.
    { git grep -nE -- "$pattern" -- "$@" ':!*_test.go' || true; } \
        | awk -F: '{ line = $0; sub(/^[^:]*:[^:]*:/, "", line); if (line !~ /^[[:space:]]*\/\//) n++ } END { print n + 0 }'
}

echo "== static: design §3.1 invariants (base $base, head $(git rev-parse --short HEAD)) =="

add_body="$(func_body 'func (d *Dispatcher) Add(')"
if [[ -z "$add_body" ]]; then
    fail "S1 Dispatcher.Add not found"
else
    if grep -qF 'func (d *Dispatcher) Add(ctx context.Context' <<<"$add_body"; then
        pass "S1 Dispatcher.Add takes a context"
    else
        fail "S1 Dispatcher.Add does not take ctx context.Context as its first parameter"
    fi

    persist_line="$(grep -nF 'persistIfChanged(' <<<"$add_body" | head -1 | cut -d: -f1 || true)"
    kick_line="$(grep -nF 'd.kick()' <<<"$add_body" | tail -1 | cut -d: -f1 || true)"
    if [[ -n "$persist_line" ]]; then
        pass "S2 Add persists synchronously (persistIfChanged in its body)"
    else
        fail "S2 Add does not call persistIfChanged"
    fi
    if [[ -n "$persist_line" && -n "$kick_line" && "$kick_line" -gt "$persist_line" ]]; then
        pass "S3 Add kicks the tick after the persist"
    else
        fail "S3 Add has no d.kick() after persistIfChanged"
    fi
    if grep -qF 'beginRemoval(' <<<"$add_body" && grep -qF '.end()' <<<"$add_body"; then
        pass "S4 Add unwinds a failed persist through beginRemoval/removal.end"
    else
        fail "S4 Add's failure path does not go through beginRemoval and .end()"
    fi
    if grep -qE '(^|[^.a-zA-Z])d\.deregister\(' <<<"$add_body"; then
        fail "S5 Add calls d.deregister directly"
    else
        pass "S5 Add does not call d.deregister directly"
    fi
fi

snap_body="$(func_body 'func (d *Dispatcher) snapshotOrder(')"
if grep -qF 'd.written[' <<<"$snap_body"; then
    pass "S6 snapshotOrder filters on d.written"
else
    fail "S6 snapshotOrder does not consult d.written"
fi

claim_body="$(func_body 'func (d *Dispatcher) claimLaunched(')"
if [[ -z "$claim_body" ]]; then
    skip "S7 claimLaunched not found"
elif grep -qF 'd.written' <<<"$claim_body"; then
    fail "S7 claimLaunched also checks d.written (a second enforcement point; design §3.1)"
else
    pass "S7 claimLaunched carries no second written-row check"
fi

n="$(prod_count '\.deregister\(' 'internal/dispatch/*.go')"
if [[ "$n" == "1" ]] && git grep -qF 'r.d.deregister(r.id)' -- 'internal/dispatch/*.go' ':!*_test.go'; then
    pass "S8 deregister's only production caller is removal.end"
else
    fail "S8 deregister has $n production call sites, or removal.end is not the one"
fi

n="$(prod_count 'INSERT INTO dispatch_jobs' '*.go')"
if [[ "$n" == "1" ]]; then pass "S9 dispatch_jobs keeps one INSERT"; else fail "S9 dispatch_jobs has $n INSERT sites"; fi

n="$(prod_count 'd\.launch\(' 'internal/dispatch/*.go')"
if [[ "$n" == "1" ]]; then pass "S10 only the tick launches (one d.launch call)"; else fail "S10 d.launch has $n production call sites"; fi

n="$(prod_count 'dispatcher\.Add\([A-Za-z_]+, [A-Za-z_]+\)' 'internal/*.go' 'cmd/*.go')"
if [[ "$n" == "0" ]]; then pass "S11 no caller uses the context-free Add(j, h) form"; else fail "S11 $n call sites still call dispatcher.Add without a context"; fi

for fn in 'func (app *Application) AddJob(' 'func (app *Application) RetryHistoryJob('; do
    name="${fn#func (app \*Application) }"; name="${name%(}"
    body="$(func_body "$fn")"
    if [[ -z "$body" ]]; then
        skip "S12 $name not found"
    elif grep -qF 'context.WithoutCancel' <<<"$body"; then
        pass "S12 $name detaches the context it gives Add"
    else
        fail "S12 $name has no context.WithoutCancel (design: a detached, bounded context for Add)"
    fi
done

if git grep -qE 'func TestSIGKILL_AddThenImmediateKill' -- 'test/crash/*_test.go'; then
    pass "S13 crash regression test is declared"
else
    fail "S13 no TestSIGKILL_AddThenImmediateKill* in test/crash"
fi

mapfile -t specs < <(git diff --name-only --diff-filter=AM "$base"...HEAD -- '*.spec')
if [[ "${#specs[@]}" -eq 0 ]]; then
    fail "S14 no scripts/mutate spec added or changed since $base"
elif grep -lF 'd.written' "${specs[@]}" >/dev/null 2>&1; then
    pass "S14 a mutate spec targets the written-row gate (${#specs[@]} spec(s) changed)"
else
    fail "S14 no changed spec mutates the d.written gate"
fi

if $static_only; then
    echo "== dynamic checks skipped (--static-only) =="
else
    echo "== dynamic: gates =="
    run() {
        local id="$1"; shift
        local log="/tmp/p1-accept.$$.${id}.log"
        if "$@" >"$log" 2>&1; then
            pass "$id $*"
            rm -f "$log"
        else
            fail "$id $* (log kept: $log)"
        fi
    }

    run D1 go build ./...
    run D2 go vet ./...
    run D3 go vet -tags=integration,uitest,crash ./...
    run D4 go test -race -count=1 ./internal/dispatch/... ./internal/app/...
    for s in "${specs[@]}"; do run D5 go run ./scripts/mutate "$s"; done
    run D6 go run ./scripts/check_dup_comments
    run D7 go run ./scripts/check_citations
    run D8 go run ./scripts/check_doc_citations
    run D9 go run ./scripts/check_test_doubles --all
    run D10 go run ./scripts/check_review_banner
    if command -v golangci-lint >/dev/null 2>&1; then
        run D11 golangci-lint run ./...
    else
        skip "D11 golangci-lint not installed"
    fi
    if $full; then
        run D12 go test -race -count=1 ./...
        run D13 ./scripts/run_tests.sh
    else
        skip "D12/D13 full suites (pass --full)"
    fi
    if [[ "$crash_count" -gt 0 ]]; then
        run D14 go test -tags=crash -count="$crash_count" -timeout=60m -run TestSIGKILL_AddThenImmediateKill ./test/crash/
    else
        skip "D14 crash test (pass --crash N)"
    fi
fi

echo "== $failures failure(s) =="
[[ "$failures" -eq 0 ]]
