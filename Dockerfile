# Governs the plain `alpine` stages below only. Deliberately NOT interpolated
# into the go-builder FROM line: Renovate's Docker manager pins a digest onto
# this ARG's value, and that digest is only valid for the `alpine` image
# itself — splicing it into a different repository's tag (golang:1.26-alpine
# ${ALPINE_VERSION}) produced an invalid, nonexistent image reference. The
# golang stage below pins its own alpine-variant tag independently instead;
# bump both together by hand when moving to a new Alpine release.
ARG ALPINE_VERSION=3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b

# ---- Build UI ----
FROM oven/bun:alpine@sha256:5acc90a93e91ff07bf72aa90a7c9f0fa189765aec90b47bdbf2152d2196383c0 AS ui-builder
WORKDIR /src/ui
COPY ui/package.json ui/bun.lock ./
RUN bun install --frozen-lockfile
COPY ui/ .
RUN bun run build

# ---- Build Go binary ----
FROM golang:1.26-alpine3.24@sha256:3ad57304ad93bbec8548a0437ad9e06a455660655d9af011d58b993f6f615648 AS go-builder
WORKDIR /src

# Git is needed for version stamping.
RUN apk add --no-cache git

# Layer cache: copy module files first, download, then copy source.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
COPY --from=ui-builder /src/ui/dist ui/dist

ARG VERSION=
ARG COMMIT=
ARG BUILD_DATE=
RUN VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}" && \
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
FROM ghcr.io/linuxserver/unrar:latest@sha256:22e6e76f2f2372a7cd6e046b10025e8bde8a04a2b2b2c6072fca6821da5747f7 AS unrar

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
#
# Deliberately NOT installed: bubblewrap. internal/cmdutil sandboxes external
# unrar/7z subprocesses with `bwrap` on Linux, but bwrap needs to create an
# unprivileged user+mount namespace — something a normal (non-`--privileged`)
# `docker run`/`docker compose` container's default seccomp/AppArmor profile
# blocks. Installing bwrap here would not make sandboxing work; it would only
# make extraction silently fail on every job (bwrap exits 1 before it even
# execs unrar/7z). See "Configuration for Docker" in README.md and
# docs/ARCHITECTURE.md's Post-Processing section for the containment model
# actually in effect here (per-job containment check, not OS sandboxing).
RUN apk add --no-cache \
    7zip \
    ca-certificates \
    tzdata \
    su-exec \
    shadow

# Signals to the Go binary (see internal/config/defaults.go) that this is the
# official container image, so a brand-new config defaults strict_sandbox to
# false instead of aborting extraction when bwrap (deliberately absent, above)
# can't be found. Existing config files are never modified by this — a user
# who explicitly sets strict_sandbox: true (e.g. running --privileged with
# their own bwrap-enabled image) keeps that choice.
ENV GONZBD_DOCKER=1

COPY --from=go-builder /gonzbd /usr/local/bin/gonzbd
COPY --from=par2-builder /usr/local/bin/par2 /usr/local/bin/par2
COPY --from=unrar /usr/bin/unrar-alpine /usr/bin/unrar
COPY --chmod=755 entrypoint.sh /entrypoint.sh

ENV GONZBD_PORT=4289
EXPOSE 4289

# Mirrors docker-compose-example.yml's healthcheck so `docker run` users (not
# just compose) get the same coverage without extra setup. Shell form (not
# exec-array) so $GONZBD_PORT expands; busybox wget (already in the base
# image) needs no extra package.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget --spider -q "http://localhost:${GONZBD_PORT}/api?mode=version&output=json" || exit 1

ENTRYPOINT ["/entrypoint.sh"]
CMD ["gonzbd", "--config", "/config/gonzbd.yaml", "--serve"]
