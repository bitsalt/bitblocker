# syntax=docker/dockerfile:1

# BitBlocker — multi-stage build producing a static, non-root Docker image.
#
# Build stage compiles a CGO-free static binary; the runtime stage is
# distroless (no shell, no package manager) and runs as a fixed non-root
# UID. See docs/traefik-integration.md (Sprint 4 Technical Writer pass) for
# the full Traefik forwardAuth wiring and README for install/config
# instructions.

# --- Build stage -------------------------------------------------------
#
# --platform=$BUILDPLATFORM pins this stage to the build host's native
# platform even when the image is built multi-arch via `buildx build
# --platform linux/amd64,linux/arm64`. Go cross-compiles via GOOS/GOARCH
# without needing QEMU emulation of the build stage itself (CGO_ENABLED=0
# needs no C cross-compiler) — TARGETOS/TARGETARCH are populated
# automatically by BuildKit per requested target platform.
FROM --platform=$BUILDPLATFORM golang:1.25.11-alpine@sha256:523c3effe300580ed375e43f43b1c9b091b68e935a7c3a92bfcc4e7ed55b18c2 AS build

WORKDIR /src

# Cache module downloads in their own layer, invalidated only by go.mod/go.sum.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/bitblocker ./cmd/bitblocker

# Pre-create the on-disk blocklist cache directory with the runtime
# nonroot UID/GID (65532, baked into gcr.io/distroless/static-debian12's
# ":nonroot" variant) so the daemon can write its DB-IP MMDB snapshot
# (config `cache.path`, default /var/cache/bitblocker/dbip-country-lite.mmdb;
# ADR 0003 — the filename carries a date-versioned DB-IP artifact, not a
# GeoLite2 one) without a root-owned directory blocking the write.
# Satisfies the Docker half of OQ-CACHE-3; the systemd half is
# packaging/systemd/bitblocker.service's CacheDirectory=.
RUN mkdir -p /out/cache && chown 65532:65532 /out/cache

# --- Runtime stage -------------------------------------------------------
#
# distroless/static-debian12:nonroot has no shell, no package manager, and
# defaults USER to uid:gid 65532:65532 ("nonroot") already — no separate
# `useradd` step is possible or necessary in this base.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:aef9602f8710ec12bde19d593fed1f76c708531bb7aba205110f1029786ead7b

COPY --from=build /out/bitblocker /usr/local/bin/bitblocker
COPY --from=build --chown=65532:65532 /out/cache /var/cache/bitblocker

# Writable cache directory (OQ-CACHE-3, Docker half). Declaring it a
# VOLUME means a container started with no explicit mount still gets a
# writable anonymous volume here (rather than falling back to whatever
# the storage driver does with a container-layer write) — for
# persistence across `docker rm`/recreate, mount a named volume or bind
# mount at this path instead, e.g.:
#   docker run -v bitblocker-cache:/var/cache/bitblocker ...
VOLUME ["/var/cache/bitblocker"]

# The daemon reads its YAML config from the path given by --config
# (default /etc/bitblocker/config.yaml — see cmd/bitblocker/main.go). No
# config ships in this image; supply one via bind mount:
#   docker run -v /path/to/config.yaml:/etc/bitblocker/config.yaml:ro ...
# or override the path entirely with a custom CMD / --config flag.

# Explicit for clarity and to survive a future base-image change; the
# distroless:nonroot base already defaults to this UID:GID.
USER 65532:65532

ENTRYPOINT ["/usr/local/bin/bitblocker"]
CMD ["--config", "/etc/bitblocker/config.yaml"]
