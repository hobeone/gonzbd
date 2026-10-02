#!/bin/sh
# build.sh — builds the gonzbd binary with version, commit and build time
# stamped in, so the About dialog, the footer and `gonzbd --version` can show
# them. Builds only the Go binary: run `cd ui && bun run build` first when
# ui/dist is missing or stale.
#
# Usage: ./scripts/build.sh [extra go build args...]
#
# Examples:
#   ./scripts/build.sh
#   ./scripts/build.sh -race
#   ./scripts/build.sh -o /usr/local/bin/gonzbd

set -e

VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)

echo "Building gonzbd"
echo "  VERSION:    ${VERSION}"
echo "  COMMIT:     ${COMMIT}"
echo "  BUILD_DATE: ${BUILD_DATE}"

exec go build \
  -ldflags "-X main.Version=${VERSION} -X main.Commit=${COMMIT} -X main.Date=${BUILD_DATE}" \
  -o gonzbd \
  "$@" \
  ./cmd/gonzbd
