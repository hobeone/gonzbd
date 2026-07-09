#!/bin/bash
# renovate-local.sh - Run Renovate against this checkout locally, using the
# shared self-hosted config from hobeone/renovate-config, without touching
# GitHub (no branches, no PRs).
#
# Requires: docker, gh (for a token), and a local clone of
#   https://github.com/hobeone/renovate-config
#
# Usage:
#   ./scripts/renovate-local.sh
#   ./scripts/renovate-local.sh --dry-run=false   # actually write changes locally
#
# Env vars:
#   RENOVATE_CONFIG_DIR   Path to local renovate-config checkout
#                         (default: ../renovate-config, sibling to this repo)
#   RENOVATE_LOG_LEVEL    LOG_LEVEL passed to Renovate (default: debug)
#
# Notes:
#   - --dry-run=full is the default so nothing is written; pass --dry-run=false
#     (as an extra arg) to let it make real local git commits in this checkout
#     (it still never pushes or opens a PR, since there's no platform).
#   - Extra args are passed through to `renovate` as-is, so passing --dry-run
#     again overrides the default (yargs takes the last occurrence).

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONFIG_DIR="${RENOVATE_CONFIG_DIR:-${REPO_ROOT}/../renovate-config}"
LOG_LEVEL="${RENOVATE_LOG_LEVEL:-debug}"

if [ ! -f "${CONFIG_DIR}/config.js" ]; then
  echo "Error: no config.js found at ${CONFIG_DIR}" >&2
  echo "Clone it first: git clone https://github.com/hobeone/renovate-config \"${CONFIG_DIR}\"" >&2
  echo "Or point RENOVATE_CONFIG_DIR at an existing checkout." >&2
  exit 1
fi

if ! command -v gh >/dev/null 2>&1; then
  echo "Error: gh CLI is required to mint a token" >&2
  exit 1
fi

if ! command -v docker >/dev/null 2>&1; then
  echo "Error: docker is required" >&2
  exit 1
fi

GH_TOKEN="$(gh auth token)"

echo "Running Renovate locally against ${REPO_ROOT}"
echo "  config:    ${CONFIG_DIR}/config.js"
echo "  log level: ${LOG_LEVEL}"

exec docker run --rm \
  -e LOG_LEVEL="${LOG_LEVEL}" \
  -e RENOVATE_TOKEN="${GH_TOKEN}" \
  -e GITHUB_COM_TOKEN="${GH_TOKEN}" \
  -e RENOVATE_CONFIG_FILE=/usr/src/renovate-config/config.js \
  -v "${CONFIG_DIR}:/usr/src/renovate-config:ro" \
  -v "${REPO_ROOT}:/usr/src/app" \
  -w /usr/src/app \
  renovate/renovate \
  --platform=local \
  --dry-run=full \
  "$@"
