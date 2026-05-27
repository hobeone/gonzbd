# All Alpine-based stages use the same version for consistency.
ARG ALPINE_VERSION=3.23

# ---- Build UI ----
FROM node:26-alpine${ALPINE_VERSION} AS ui-builder
WORKDIR /src/ui
COPY ui/package.json ui/package-lock.json ./
RUN npm ci
COPY ui/ .
RUN npm run build

# ---- Build Go binary ----
FROM golang:1.26-alpine${ALPINE_VERSION} AS go-builder
WORKDIR /src

# Git is needed for version stamping and private module access.
RUN apk add --no-cache git

# Layer cache: copy module files first, vendor, then copy source.
# This way source changes don't invalidate the expensive vendor layer.
COPY go.mod go.sum ./
ARG GOPRIVATE=github.com/hobeone/*
ENV GONOSUMCHECK=${GOPRIVATE} GONOSUMDB=${GOPRIVATE} GOPRIVATE=${GOPRIVATE}
RUN --mount=type=secret,id=GOPRIVATE_TOKEN \
    if [ -f /run/secrets/GOPRIVATE_TOKEN ]; then \
      git config --global url."https://$(cat /run/secrets/GOPRIVATE_TOKEN)@github.com/".insteadOf "https://github.com/"; \
    fi && \
    go mod download

COPY . .
COPY --from=ui-builder /src/ui/dist ui/dist

ARG VERSION=
ARG COMMIT=
ARG BUILD_DATE=
RUN --mount=type=secret,id=GOPRIVATE_TOKEN \
    if [ -f /run/secrets/GOPRIVATE_TOKEN ]; then \
      git config --global url."https://$(cat /run/secrets/GOPRIVATE_TOKEN)@github.com/".insteadOf "https://github.com/"; \
    fi && \
    VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}" && \
    COMMIT="${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo unknown)}" && \
    BUILD_DATE="${BUILD_DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}" && \
    CGO_ENABLED=0 go build \
      -ldflags="-s -w \
        -X main.Version=${VERSION} \
        -X main.Commit=${COMMIT} \
        -X main.Date=${BUILD_DATE}" \
      -o /gonzbd ./cmd/gonzbd

# ---- Build par2cmdline-turbo ----
FROM alpine:${ALPINE_VERSION} AS par2-builder
ARG PAR2_VERSION=v1.4.0
RUN apk add --no-cache autoconf automake build-base curl \
 && mkdir /tmp/par2 \
 && curl -L "https://github.com/animetosho/par2cmdline-turbo/archive/${PAR2_VERSION}.tar.gz" \
    | tar xz -C /tmp/par2 --strip-components=1 \
 && cd /tmp/par2 \
 && ./automake.sh && ./configure && make -j"$(nproc)" && make install \
 && rm -rf /tmp/par2

# ---- Runtime ----
FROM ghcr.io/linuxserver/unrar:latest AS unrar

FROM alpine:${ALPINE_VERSION}

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
LABEL org.opencontainers.image.title="GoNZBD" \
      org.opencontainers.image.description="A Go reimplementation of SABnzbd" \
      org.opencontainers.image.version=${VERSION} \
      org.opencontainers.image.revision=${COMMIT} \
      org.opencontainers.image.created=${BUILD_DATE} \
      org.opencontainers.image.source="https://github.com/hobeone/gonzbd" \
      org.opencontainers.image.licenses="MIT"

# Post-processing dependencies:
#   7zip            - archive extraction (7z, zip, RAR, and more)
#   ca-certificates - TLS connections to news servers
#   tzdata          - timezone support for schedules
#   su-exec         - lightweight privilege drop (like gosu)
#   shadow          - usermod/groupmod for PUID/PGID support
RUN apk add --no-cache \
    7zip \
    ca-certificates \
    tzdata \
    su-exec \
    shadow

COPY --from=go-builder /gonzbd /usr/local/bin/gonzbd
COPY --from=par2-builder /usr/local/bin/par2 /usr/local/bin/par2
COPY --from=unrar /usr/bin/unrar-alpine /usr/bin/unrar
COPY --chmod=755 entrypoint.sh /entrypoint.sh

ENV GONZBD_PORT=4289
EXPOSE 4289

ENTRYPOINT ["/entrypoint.sh"]
CMD ["gonzbd", "--config", "/config/gonzbd.yaml", "--serve"]
