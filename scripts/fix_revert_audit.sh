#!/usr/bin/env bash
#
# fix_revert_audit.sh — find bug-fix commits whose tests do NOT guard the fix.
#
# For each behavioral bug-fix commit, in a throwaway git worktree, revert ONLY
# the production (.go non-test) changes — keeping that commit's test changes —
# and run the affected packages' tests. If they still PASS, the fix is not
# guarded by any test: a regression to the old behavior would go undetected.
# (This is the failure mode the "Red-Green Discipline" section of AGENTS.md
# exists to prevent; this script audits the existing history for it.)
#
# Classifications per commit:
#   GUARDED            a test fails when the fix is reverted — good.
#   UNGUARDED          tests pass with the fix reverted — the defect.
#   UNGUARDED-no-tests no test-bearing package covers the changed code at all.
#   INCONCLUSIVE-build the test is so coupled to the fix it won't compile
#                      against the old code (arguably the tightest guarding).
#
# Test scope is reverse-dependency aware: the changed package, every package
# whose production OR test code imports it (catching black-box guards), plus the
# app/api integration hubs when they transitively depend on it.
#
# Triage notes (the UNGUARDED set is NOT a punch-list):
#   - `test-in-commit` UNGUARDED is the high-signal subset: the commit shipped a
#     test alongside the fix that fails to guard it.
#   - `NO-test-in-commit` UNGUARDED is weaker: many are hardening/concurrency/
#     security or logging changes that unit tests legitimately cannot guard.
#   - Non-behavioral commits (go fix, lint, robustness refactors with identical
#     observable behavior) are expected to pass on revert and are not defects.
#
# Limitations: only same-module unit/black-box guards are exercised; integration
# (`//go:build integration`) and e2e suites are not run. Race/flake-labeled
# fixes use `-race -count=10`, which still may not reproduce a race.
#
# Usage:  scripts/fix_revert_audit.sh [results_file]
#         (run from anywhere inside the repo; default results file is
#          ./fix_revert_audit_results.txt)

set -uo pipefail

REPO=$(git rev-parse --show-toplevel) || { echo "not in a git repo" >&2; exit 1; }
RESULTS=${1:-$REPO/fix_revert_audit_results.txt}
WT=$(mktemp -d)/wt
: > "$RESULTS"

git -C "$REPO" worktree add -q --detach "$WT" HEAD
cleanup() {
  git -C "$WT" reset -q --hard HEAD 2>/dev/null
  git -C "$REPO" worktree remove --force "$WT" 2>/dev/null
  rmdir "$(dirname "$WT")" 2>/dev/null
}
trap cleanup EXIT

mapfile -t cands < <(git -C "$REPO" log --pretty='%H %s' | grep -iE '\bfix|\bbug' | awk '{print $1}')
total=${#cands[@]}; i=0
for c in "${cands[@]}"; do
  i=$((i+1))
  subj=$(git -C "$REPO" log -1 --pretty='%s' "$c")
  # Type filter: refactor/chore/docs/style/ci/build are non-behavioral.
  echo "$subj" | grep -qiE '^(refactor|chore|docs|style|ci|build)[(:]' && continue
  git -C "$REPO" rev-parse -q --verify "$c^" >/dev/null 2>&1 || continue
  prod=$(git -C "$REPO" diff-tree --no-commit-id --name-only -r "$c" | grep -E '\.go$' | grep -vE '_test\.go$|^scripts/' || true)
  [ -z "$prod" ] && continue
  if git -C "$REPO" diff-tree --no-commit-id --name-only -r "$c" | grep -qE '_test\.go$'; then hastest=test-in-commit; else hastest=NO-test-in-commit; fi
  mode=normal
  echo "$subj" | grep -qiE 'race|flak|ETXTBSY|deadlock|goroutine leak' && mode=race

  git -C "$WT" reset -q --hard "$c" 2>/dev/null; git -C "$WT" clean -qfd 2>/dev/null
  # The UI embed (//go:embed all:dist) needs ui/dist to exist or `go list ./...`
  # fails to load the package. dist is a build artifact, absent in a fresh
  # checkout; a placeholder satisfies the embed without affecting tests.
  mkdir -p "$WT/ui/dist" 2>/dev/null; touch "$WT/ui/dist/index.html" 2>/dev/null
  # Module path changed over history (sabnzbd-go -> gonzbd); read it per commit.
  MOD=$(cd "$WT" && go list -m 2>/dev/null | head -1); [ -z "$MOD" ] && continue

  declare -A want=()
  for f in $prod; do d=$(dirname "$f"); [ -d "$WT/$d" ] && want["$MOD/$d"]=1; done
  if [ ${#want[@]} -eq 0 ]; then echo "$c INCONCLUSIVE-nopkg $hastest :: $subj" >>"$RESULTS"; unset want; continue; fi

  golist=$(cd "$WT" && go list -f '{{.ImportPath}}|{{if or .TestGoFiles .XTestGoFiles}}1{{else}}0{{end}}|{{range .Imports}}{{.}} {{end}}{{range .TestImports}}{{.}} {{end}}{{range .XTestImports}}{{.}} {{end}}|{{range .Deps}}{{.}} {{end}}' ./... 2>/dev/null)
  declare -A scope=()
  while IFS='|' read -r ip ht imps deps; do
    [ "$ht" = "1" ] || continue
    case "$ip" in *"$MOD/test/"*) continue;; esac
    inc=0
    [ -n "${want[$ip]:-}" ] && inc=1
    if [ $inc -eq 0 ]; then for w in "${!want[@]}"; do case " $imps " in *" $w "*) inc=1; break;; esac; done; fi
    if [ $inc -eq 0 ] && { [ "$ip" = "$MOD/internal/app" ] || [ "$ip" = "$MOD/internal/api" ]; }; then
      for w in "${!want[@]}"; do case " $deps " in *" $w "*) inc=1; break;; esac; done
    fi
    [ $inc -eq 1 ] && scope["$ip"]=1
  done <<< "$golist"
  nscope=${#scope[@]}

  for f in $prod; do
    if git -C "$WT" cat-file -e "$c^:$f" 2>/dev/null; then git -C "$WT" checkout -q "$c^" -- "$f" 2>/dev/null; else rm -f "$WT/$f"; fi
  done

  if [ $nscope -eq 0 ]; then
    echo "$c UNGUARDED-no-tests $hastest scope:0 :: $subj" >>"$RESULTS"
    echo "[$i/$total] UNGUARDED-no-tests $c ${subj:0:52}"
    unset want scope; continue
  fi
  scopelist="${!scope[@]}"

  if [ "$mode" = race ]; then
    out=$(cd "$WT" && go test -race -count=10 -timeout=300s $scopelist 2>&1); rc=$?
  else
    out=$(cd "$WT" && go test -count=1 -timeout=180s $scopelist 2>&1); rc=$?
  fi

  if [ $rc -eq 0 ]; then cls=UNGUARDED
  elif echo "$out" | grep -qE '\[build failed\]|cannot use|undefined:|does not implement|syntax error|build constraints exclude|imported and not used'; then cls=INCONCLUSIVE-build
  elif echo "$out" | grep -qE '^--- FAIL|^FAIL[[:space:]]'; then cls=GUARDED
  else cls=INCONCLUSIVE-other; fi
  echo "$c $cls $hastest mode:$mode scope:$nscope :: $subj" >>"$RESULTS"
  echo "[$i/$total] $cls $c ${subj:0:52}"
  unset want scope
done
echo "=== DONE ($total candidates) — results in $RESULTS ==="
