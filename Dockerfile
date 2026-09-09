# Governs the plain `alpine` stages below only. Not interpolated into the
# go-builder FROM line, which pins its own alpine-variant tag independently
# instead — bump both together by hand when moving to a new Alpine release.
ARG ALPINE_VERSION=3.24

# ---- Build UI ----
# --platform=$BUILDPLATFORM: bun/vite's output (static JS/HTML/CSS) is the
# same regardless of the final image's target platform, so this stage always
# runs natively on the build host instead of under QEMU emulation for the
# non-native leg of a multi-arch build.
FROM --platform=$BUILDPLATFORM oven/bun:alpine AS ui-builder
WORKDIR /src/ui
COPY ui/package.json ui/bun.lock ./
RUN bun install --frozen-lockfile
COPY ui/ .
RUN bun run build

# ---- Build Go binary ----
# --platform=$BUILDPLATFORM + explicit GOOS/GOARCH below: CGO_ENABLED=0 makes
# Go natively cross-compile, so there's no need to run this stage under QEMU
# emulation for the non-native leg of a multi-arch build either — only the
# resulting binary needs to target the other platform, not the compiler
# toolchain running it. TARGETOS/TARGETARCH are buildx's implicit global
# args; they must be re-declared with ARG inside this stage to be visible.
FROM --platform=$BUILDPLATFORM golang:1.27.1-alpine3.24 AS go-builder
WORKDIR /src
ARG TARGETOS
ARG TARGETARCH

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
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
      -ldflags="-s -w \
        -X main.Version=${VERSION} \
        -X main.Commit=${COMMIT} \
        -X main.Date=${BUILD_DATE}" \
      -o /gonzbd ./cmd/gonzbd

# ---- Build par2cmdline-turbo ----
FROM alpine:${ALPINE_VERSION} AS par2-builder
# Its own RUN, before the PAR2_VERSION/PAR2_SHA256 ARGs below: neither ARG
# is referenced here, but both are referenced in the RUN that follows them,
# so keeping the two RUNs separate means a version/checksum bump can't bust
# this toolchain-install layer's cache the way a single fused RUN would.
RUN apk add --no-cache autoconf automake build-base curl
ARG PAR2_VERSION=v1.5.0
# SHA256 of the official release source tarball for PAR2_VERSION, as
# published in GitHub's own `assets[].digest` for that release (see
# `curl -s https://api.github.com/repos/animetosho/par2cmdline-turbo/releases/tags/${PAR2_VERSION}`).
# This is an uploaded release asset, not the `/archive/${ref}.tar.gz`
# codeload snapshot GitHub regenerates on demand — codeload's gzip bytes
# (hence its sha256) can drift with no underlying code change, which is
# what broke this build under the previous archive-URL approach.
# IMPORTANT: bumping PAR2_VERSION requires copying the new release's
# published digest in the same change. Renovate's custom regex manager
# (renovate.json) only tracks the version string and cannot discover a new
# checksum on its own, so a Renovate-driven version bump PR will fail this
# build (sha256sum -c) until the checksum below is updated to match —
# loudly, on purpose, rather than silently building an unverified tarball.
# NOTE: upstream only started publishing this source-tarball asset at
# v1.5.0 — v1.4.0 and earlier shipped prebuilt platform zips only, so a
# downgrade below v1.5.0 needs the codeload archive path restored instead.
ARG PAR2_SHA256=035584144d3ac3b08fb5d7d4b844f60cf97404b6469f6aed7d9b9430a25bef0e
RUN mkdir /tmp/par2 \
 && curl -L -o /tmp/par2.tar.xz "https://github.com/animetosho/par2cmdline-turbo/releases/download/${PAR2_VERSION}/par2cmdline-turbo-${PAR2_VERSION#v}.tar.xz" \
 && echo "${PAR2_SHA256}  /tmp/par2.tar.xz" | sha256sum -c - \
 && tar xJ -C /tmp/par2 --strip-components=1 -f /tmp/par2.tar.xz \
 && rm /tmp/par2.tar.xz \
 && cd /tmp/par2 \
 && ./automake.sh \
 && CFLAGS="-O3" CXXFLAGS="-O3" ./configure \
 && make -j"$(nproc)" && make install-strip \
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
  CMD wget --spider -q "http://localhost:${GONZBD_PORT}/api?mode=health&output=json" || exit 1

STOPSIGNAL SIGTERM

ENTRYPOINT ["/entrypoint.sh"]
CMD ["gonzbd", "--config", "/config/gonzbd.yaml", "--serve"]
