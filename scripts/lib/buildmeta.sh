#!/bin/sh
# buildmeta.sh — sourced by scripts/build.sh and scripts/docker-build.
# The one place the build metadata is derived from the checkout.
#
# Sets VERSION, COMMIT, COMMIT_TIME, DIRTY and BUILD_DATE. A value git cannot
# supply is empty (VERSION falls back to "dev"); the binary reports an empty
# field as absent. VERSION omits `--dirty`: DIRTY carries that, so the UI does
# not render the modification twice.

VERSION=$(git describe --tags --always 2>/dev/null || echo "dev")
COMMIT=$(git rev-parse --short HEAD 2>/dev/null || true)
COMMIT_TIME=$(git log -1 --format=%cI 2>/dev/null || true)
if [ -n "$(git status --porcelain 2>/dev/null)" ]; then
  DIRTY=true
else
  DIRTY=false
fi
BUILD_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)
