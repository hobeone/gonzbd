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

# Build the Go binary (pure Go, no CGo needed)
WORKDIR /src
ARG VERSION=dev
RUN CGO_ENABLED=0 go build \
    -ldflags="-s -w -X main.Version=${VERSION}" \
    -o /gonzbd ./cmd/gonzbd

# ---- Runtime stage ----
FROM alpine:3.22

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
