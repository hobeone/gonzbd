# ---- Build stage ----
FROM golang:1.26-alpine AS builder

RUN apk add --no-cache git nodejs npm

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build the Svelte UI
WORKDIR /src/ui
RUN npm ci && npm run build

# Build the Go binary (pure Go, no CGo needed).
# When VERSION/COMMIT/BUILD_DATE are not passed via --build-arg,
# auto-derive them from git so local builds are properly stamped.
WORKDIR /src
ARG VERSION=
ARG COMMIT=
ARG BUILD_DATE=
RUN VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}" && \
    COMMIT="${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo unknown)}" && \
    BUILD_DATE="${BUILD_DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}" && \
    CGO_ENABLED=0 go build \
      -ldflags="-s -w -X main.Version=${VERSION} -X main.Commit=${COMMIT} -X main.Date=${BUILD_DATE}" \
      -o /gonzbd ./cmd/gonzbd

# ---- Runtime stage ----
FROM alpine:3.22

# OCI image metadata — queryable via `docker inspect`.
# When building locally without --build-arg, these default to empty.
# The binary itself always has correct values from the build stage.
ARG VERSION
ARG COMMIT
ARG BUILD_DATE
LABEL org.opencontainers.image.title="GoNZBD"
LABEL org.opencontainers.image.description="A Go reimplementation of SABnzbd"
LABEL org.opencontainers.image.version="${VERSION}"
LABEL org.opencontainers.image.revision="${COMMIT}"
LABEL org.opencontainers.image.created="${BUILD_DATE}"
LABEL org.opencontainers.image.source="https://github.com/hobeone/gonzbd"
LABEL org.opencontainers.image.licenses="MIT"

# Install post-processing dependencies:
#   par2cmdline     - PAR2 repair
#   7zip            - archive extraction (7z, zip, RAR, and more)
#   ca-certificates - TLS connections to news servers
#   tzdata          - timezone support for schedules
#   su-exec         - lightweight privilege drop (like gosu)
#   shadow          - usermod/groupmod for PUID/PGID support
RUN apk add --no-cache \
    par2cmdline \
    7zip \
    ca-certificates \
    tzdata \
    su-exec \
    shadow

# Default directories. Users should mount volumes over these.
RUN mkdir -p /data/downloads /data/complete /data/admin /config

COPY --from=builder /gonzbd /usr/local/bin/gonzbd
COPY entrypoint.sh /entrypoint.sh

# Default config location and port; overridable via environment.
ENV GONZBD_CONFIG=/config/gonzbd.yaml
ENV GONZBD_PORT=4289

ENTRYPOINT ["/entrypoint.sh"]
CMD ["gonzbd", "--config", "/config/gonzbd.yaml", "--serve"]
