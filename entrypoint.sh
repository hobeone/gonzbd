#!/bin/sh
set -e

# Default UID/GID if not specified.
PUID=${PUID:-1000}
PGID=${PGID:-1000}

# Create/update the gonzbd group and user to match the requested IDs.
# This ensures files written by the container are owned by the host
# user's UID/GID, avoiding permission issues with shared volumes
# (e.g., Plex, Sonarr, or the host user reading completed downloads).
if getent group gonzbd >/dev/null 2>&1; then
    # Group exists — update GID if it changed.
    CURRENT_GID=$(getent group gonzbd | cut -d: -f3)
    if [ "$CURRENT_GID" != "$PGID" ]; then
        groupmod -g "$PGID" gonzbd
    fi
else
    addgroup -g "$PGID" -S gonzbd
fi

if id gonzbd >/dev/null 2>&1; then
    # User exists — update UID if it changed.
    CURRENT_UID=$(id -u gonzbd)
    if [ "$CURRENT_UID" != "$PUID" ]; then
        usermod -u "$PUID" gonzbd
    fi
else
    adduser -u "$PUID" -G gonzbd -S -D gonzbd
fi

# Fix ownership of data directories. Only touch the top-level dirs
# to avoid slow recursive chown on large download volumes.
chown gonzbd:gonzbd /data /data/downloads /data/complete /data/admin /config 2>/dev/null || true

echo "Starting gonzbd (uid=$(id -u gonzbd), gid=$(id -g gonzbd))"

# Build the listen address from environment. Users set GONZBD_PORT
# to change the internal port (must match the container side of the
# docker-compose ports: mapping).
GONZBD_PORT=${GONZBD_PORT:-8080}
GONZBD_LISTEN="0.0.0.0:${GONZBD_PORT}"

# Drop privileges and exec the main process.
exec su-exec gonzbd "$@" --listen "$GONZBD_LISTEN"
