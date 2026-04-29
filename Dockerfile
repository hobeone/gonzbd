# ---- Build stage ----
FROM golang:1.25-alpine AS builder

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
#   par2cmdline - PAR2 repair
#   unrar       - RAR extraction
#   7zip        - 7z/zip extraction
#   ca-certificates - TLS connections to news servers
#   tzdata      - timezone support for schedules
RUN apk add --no-cache \
    par2cmdline \
    unrar \
    7zip \
    ca-certificates \
    tzdata

# Create a non-root user for the daemon.
RUN addgroup -S gonzbd && adduser -S -G gonzbd gonzbd

# Default directories. Users should mount volumes over these.
RUN mkdir -p /data/downloads /data/complete /data/admin /config \
    && chown -R gonzbd:gonzbd /data /config

COPY --from=builder /gonzbd /usr/local/bin/gonzbd

# HTTP API + Web UI
EXPOSE 8080
# HTTPS (optional, enable via config)
EXPOSE 9090

USER gonzbd

# Default config location; overridable via --config flag.
ENV GONZBD_CONFIG=/config/gonzbd.yaml

ENTRYPOINT ["gonzbd"]
CMD ["--config", "/config/gonzbd.yaml", "--serve", "--listen", "0.0.0.0:8080"]
