#!/bin/sh
# build.sh — builds the gonzbd binary with version, commit, commit time,
# modified flag and build time stamped in, so the About dialog, the footer
# and `gonzbd --version` can show them. Builds only the Go binary: run
# `cd ui && bun run build` first when ui/dist is missing or stale.
#
# Usage: ./scripts/build.sh [extra go build args...]
#
# Examples:
#   ./scripts/build.sh
#   ./scripts/build.sh -race
#   ./scripts/build.sh -o /usr/local/bin/gonzbd

set -e

. "$(dirname "$0")/lib/buildmeta.sh"

echo "Building gonzbd"
echo "  VERSION:     ${VERSION}"
echo "  COMMIT:      ${COMMIT}"
echo "  COMMIT_TIME: ${COMMIT_TIME}"
echo "  DIRTY:       ${DIRTY}"
echo "  BUILD_DATE:  ${BUILD_DATE}"

exec go build \
  -ldflags "-X main.Version=${VERSION} -X main.Commit=${COMMIT} -X main.CommitTime=${COMMIT_TIME} -X main.Dirty=${DIRTY} -X main.Date=${BUILD_DATE}" \
  -o gonzbd \
  "$@" \
  ./cmd/gonzbd
