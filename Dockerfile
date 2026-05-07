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

# Download and build par2cmdline-turbo
WORKDIR /
RUN \
  echo "**** install packages ****" && \
  apk add -U --update --no-cache --virtual=build-dependencies \
    autoconf \
    automake \
    build-base \
    libffi-dev \
    openssl-dev \
    python3-dev \
    curl

RUN echo "**** install par2cmdline-turbo from source ****"
RUN PAR2_VERSION=$(curl -s https://api.github.com/repos/animetosho/par2cmdline-turbo/releases/latest \
    | awk '/tag_name/{print $4;exit}' FS='[""]'); \
    mkdir /tmp/par2cmdline && \
    curl -o /tmp/par2cmdline.tar.gz -L \
    "https://github.com/animetosho/par2cmdline-turbo/archive/${PAR2_VERSION}.tar.gz"
RUN tar xf /tmp/par2cmdline.tar.gz -C /tmp/par2cmdline --strip-components=1
WORKDIR /tmp/par2cmdline
RUN ./automake.sh
RUN ./configure
RUN make
RUN make check
RUN make install

# ---- Runtime stage ----

FROM ghcr.io/linuxserver/unrar:latest AS unrar

FROM alpine:latest


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
#   7zip            - archive extraction (7z, zip, RAR, and more)
#   ca-certificates - TLS connections to news servers
#   tzdata          - timezone support for schedules
#   su-exec         - lightweight privilege drop (like gosu)
#   shadow          - usermod/groupmod for PUID/PGID support
RUN apk add -U --update --no-cache \
    7zip \
    ca-certificates \
    tzdata \
    su-exec \
    shadow

# Default directories. Users should mount volumes over these.
RUN mkdir -p /data/downloads /data/complete /data/admin /config

COPY --from=builder /gonzbd /usr/local/bin/gonzbd
COPY --from=builder /usr/local/bin/par2 /usr/local/bin/par2
COPY entrypoint.sh /entrypoint.sh

# add unrar
COPY --from=unrar /usr/bin/unrar-alpine /usr/bin/unrar

# Default config location and port; overridable via environment.
ENV GONZBD_CONFIG=/config/gonzbd.yaml
ENV GONZBD_PORT=4289

ENTRYPOINT ["/entrypoint.sh"]
CMD ["gonzbd", "--config", "/config/gonzbd.yaml", "--serve"]
