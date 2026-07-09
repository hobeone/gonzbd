#!/bin/bash
# renovate-local.sh - Preview what Renovate would do to this repo, using the
# shared self-hosted config from hobeone/renovate-config.
#
# Two modes:
#   default   Runs platform=local (extraction + dependency lookup only; this
#             is a hard limit of Renovate's local platform, not a flag we
#             control - it can never write files or make commits, regardless
#             of --dry-run). Prints a one-line-per-dependency summary.
#   --diff    Runs platform=github against the real repo (read-only API
#             calls, authenticated as you via `gh auth token`, NOT the
#             hobeone-renovate App) with dry-run=full, so Renovate computes
#             exactly what it would commit. Never writes, pushes, or opens a
#             PR - dry-run of any kind short-circuits Renovate's commit step
#             before it touches git. Prints a real unified diff per file.
#
# Requires: docker, gh, python3, and a local clone of
#   https://github.com/hobeone/renovate-config
#
# Usage:
#   ./scripts/renovate-local.sh
#   ./scripts/renovate-local.sh --diff
#   ./scripts/renovate-local.sh --diff --verbose
#
# Any other args are passed through to `renovate` as-is.
#
# Env vars:
#   RENOVATE_CONFIG_DIR   Path to local renovate-config checkout
#                         (default: ../renovate-config, sibling to this repo)

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONFIG_DIR="${RENOVATE_CONFIG_DIR:-${REPO_ROOT}/../renovate-config}"

DIFF_MODE=false
VERBOSE=false
EXTRA_ARGS=()
for arg in "$@"; do
  case "$arg" in
    --diff)
      DIFF_MODE=true
      ;;
    --verbose)
      VERBOSE=true
      ;;
    *)
      EXTRA_ARGS+=("$arg")
      ;;
  esac
done

if [ ! -f "${CONFIG_DIR}/config.js" ]; then
  echo "Error: no config.js found at ${CONFIG_DIR}" >&2
  echo "Clone it first: git clone https://github.com/hobeone/renovate-config \"${CONFIG_DIR}\"" >&2
  echo "Or point RENOVATE_CONFIG_DIR at an existing checkout." >&2
  exit 1
fi

for tool in gh docker python3; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "Error: $tool is required" >&2
    exit 1
  fi
done

GH_TOKEN="$(gh auth token)"
LOG_FILE="$(mktemp -t renovate-local-XXXXXX.jsonl)"

if [ "$DIFF_MODE" = true ]; then
  REPO_SLUG="$(git -C "${REPO_ROOT}" remote get-url origin |
    sed -E 's#^git@github\.com:##; s#^https://github\.com/##; s#\.git$##')"

  echo "Running Renovate against the real repo ${REPO_SLUG} (read-only; dry-run=full never writes or pushes)"
  echo "  config:  ${CONFIG_DIR}/config.js"
  echo "  auth:    your gh token (not the hobeone-renovate App)"
  echo

  set +e
  docker run --rm \
    -e LOG_LEVEL=debug \
    -e LOG_FORMAT=json \
    -e RENOVATE_TOKEN="${GH_TOKEN}" \
    -e GITHUB_COM_TOKEN="${GH_TOKEN}" \
    -e RENOVATE_CONFIG_FILE=/usr/src/renovate-config/config.js \
    -e RENOVATE_AUTODISCOVER=false \
    -v "${CONFIG_DIR}:/usr/src/renovate-config:ro" \
    renovate/renovate \
    --platform=github \
    --dry-run=full \
    "${EXTRA_ARGS[@]}" \
    "${REPO_SLUG}" > "$LOG_FILE" 2>&1
  RENOVATE_EXIT=$?
  set -e
else
  echo "Running Renovate locally against ${REPO_ROOT} (platform=local: extraction + lookup only, never writes)"
  echo "  config:  ${CONFIG_DIR}/config.js"
  echo

  set +e
  docker run --rm \
    -e LOG_LEVEL=debug \
    -e LOG_FORMAT=json \
    -e RENOVATE_TOKEN="${GH_TOKEN}" \
    -e GITHUB_COM_TOKEN="${GH_TOKEN}" \
    -e RENOVATE_CONFIG_FILE=/usr/src/renovate-config/config.js \
    -v "${CONFIG_DIR}:/usr/src/renovate-config:ro" \
    -v "${REPO_ROOT}:/usr/src/app" \
    -w /usr/src/app \
    renovate/renovate \
    --platform=local \
    --dry-run=full \
    "${EXTRA_ARGS[@]}" > "$LOG_FILE" 2>&1
  RENOVATE_EXIT=$?
  set -e
fi

if [ "$VERBOSE" = true ]; then
  echo "--- Full debug log ---"
  cat "$LOG_FILE"
  echo "--- End full debug log ---"
  echo
fi

python3 - "$LOG_FILE" "$REPO_ROOT" "$DIFF_MODE" <<'PYEOF'
import difflib
import json
import os
import sys

log_path, repo_root, diff_mode = sys.argv[1], sys.argv[2], sys.argv[3] == "true"
level_names = {10: "TRACE", 20: "DEBUG", 30: "INFO", 40: "WARN", 50: "ERROR", 60: "FATAL"}

entries = []
with open(log_path) as f:
    for line in f:
        line = line.strip()
        if not line:
            continue
        try:
            entries.append(json.loads(line))
        except ValueError:
            continue

for entry in entries:
    level = entry.get("level", 0)
    msg = entry.get("msg", "")
    if level >= 30 and "Would commit files" not in msg:
        print(f"{level_names.get(level, level)}: {msg}")

print()

if diff_mode:
    commits = [e for e in entries if "files" in e and "branchName" in e]
    if not commits:
        print("No changes found — everything is current.")
    else:
        print(f"Findings ({len(commits)} branch(es) Renovate would create):")
        for c in commits:
            print(f"\n=== {c.get('branchName')}: {c.get('prTitle') or c.get('message')} ===")
            for file in c.get("files", []):
                path = file.get("path")
                new_contents = file.get("rawContents", "")
                local_path = os.path.join(repo_root, path)
                try:
                    with open(local_path) as f:
                        old_contents = f.read()
                except OSError:
                    old_contents = ""
                diff = difflib.unified_diff(
                    old_contents.splitlines(keepends=True),
                    new_contents.splitlines(keepends=True),
                    fromfile=f"a/{path}",
                    tofile=f"b/{path}",
                )
                diff_text = "".join(diff)
                print(diff_text if diff_text else f"  (no textual diff for {path})")
else:
    findings = []
    for entry in entries:
        if entry.get("msg") == "packageFiles with updates":
            package_files = entry.get("config", {})
            for manager, files in package_files.items():
                for pf in files:
                    package_file = pf.get("packageFile", "?")
                    for dep in pf.get("deps", []):
                        for update in dep.get("updates") or []:
                            findings.append({
                                "manager": manager,
                                "file": package_file,
                                "depName": dep.get("depName") or dep.get("packageName") or "?",
                                "currentValue": dep.get("currentValue"),
                                "newValue": update.get("newValue"),
                                "updateType": update.get("updateType"),
                            })

    if not findings:
        print("No updates found — everything is current.")
    else:
        print(f"Findings ({len(findings)}):")
        for item in findings:
            current, new = item["currentValue"], item["newValue"]
            change = f"{current} -> {new}" if current != new else f"{current} (digest only)"
            print(f"  [{item['manager']}] {item['file']}: {item['depName']} {change} ({item['updateType']})")
PYEOF

echo
echo "Full JSON log kept at: ${LOG_FILE}"

exit "$RENOVATE_EXIT"
