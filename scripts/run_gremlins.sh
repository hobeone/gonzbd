#!/bin/bash
# run_gremlins.sh - Run gremlins mutation testing with optimized environment.
# Sets custom TMPDIR and GOCACHE to prevent disk space exhaustion in /tmp.

set -e

if [ $# -lt 1 ]; then
    echo "Usage: $0 ./internal/<package>"
    exit 1
fi

pkg="$1"

# Allow overriding the base directory for gremlins storage via GREMLINS_DIR.
# Default to a local .gremlins/ directory in the workspace root.
BASE_DIR="${GREMLINS_DIR:-$(git rev-parse --show-toplevel)/.gremlins}"
export TMPDIR="${BASE_DIR}/tmp"
export GOCACHE="${BASE_DIR}/gocache"

mkdir -p "$TMPDIR" "$GOCACHE"

echo "Running gremlins on $pkg with GOCACHE=$GOCACHE and TMPDIR=$TMPDIR"
gremlins unleash "$pkg"
