# Multi-stage lightweight Dockerfile for MongoRescue
# Produces a self-contained image (about 230 MB unpacked, 65 MB compressed;
# mostly the MongoDB database tools) with mongodump and mongorestore installed.

# Stage 1: Build static Go binary
# The Go toolchain: the default must equal .go-version (scripts/check-go-version.sh);
# the Docker workflow passes it explicitly. The digest pins golang:<GO_VERSION>-alpine;
# refresh it together with GO_VERSION (docker buildx imagetools inspect).
ARG GO_VERSION=1.27.3
# The builder always runs on the build host's architecture and cross-compiles for
# the target, so the Go build never runs under QEMU.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine@sha256:fa3d596898d9f126af7c01d14ee7597bab3f39cccf392ef36b69fe77cd681f18 AS builder

WORKDIR /src

# Cache dependency layer
COPY go.mod go.sum ./
RUN go mod download

# Copy source tree
COPY . .

# Version metadata comes from the build (.git is not in the context): the release
# workflow passes the tag's version, other builds report "dev".
ARG VERSION=dev
ARG COMMIT=unknown
# Commit time in Unix seconds (git log -1 --format=%ct): the build date derives from
# it, so rebuilding a commit yields the same binary. 0 means unknown.
ARG SOURCE_DATE_EPOCH=0
# Set by BuildKit from --platform.
ARG TARGETOS=linux
ARG TARGETARCH

# Build stripped, reproducible static binary (-trimpath drops the build paths).
RUN if [ "${SOURCE_DATE_EPOCH}" = "0" ]; then BUILD_DATE=unknown; \
    else BUILD_DATE="$(date -u -d "@${SOURCE_DATE_EPOCH}" +%Y-%m-%d)"; fi \
    && CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" go build -trimpath \
    -ldflags="-s -w -X main.Version=${VERSION} -X main.Commit=${COMMIT} -X main.Date=${BUILD_DATE}" \
    -o /bin/mongorescue ./cmd/mongorescue

# Stage 2: Minimal runtime image
FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

# Upgrade base image packages to pull security fixes (e.g. zlib CVE-2026-85091)
# and install official MongoDB database tools (mongodump, mongorestore) and TLS certs
RUN apk --no-cache upgrade \
    && apk add --no-cache \
        mongodb-tools \
        ca-certificates \
        tzdata

WORKDIR /app

# Copy binary from builder
COPY --from=builder /bin/mongorescue /usr/local/bin/mongorescue

# Unprivileged runtime user. The fixed UID/GID lets operators prepare bind mounts
# (chown 10001:10001) before the first start.
RUN addgroup -S -g 10001 mongorescue \
    && adduser -S -D -H -u 10001 -G mongorescue -s /sbin/nologin mongorescue

# Default directories for internal metadata and local backups. They are owned by the
# runtime user before VOLUME, so fresh named volumes inherit the ownership.
RUN mkdir -p /data /backups \
    && chown mongorescue:mongorescue /data /backups \
    && chmod 750 /data /backups

# Bootstrap options only; everything else is configured in the dashboard. The default
# "Local disk" storage target is created next to the data directory, i.e. /backups.
# The port is set through the environment (not a -port flag) so the server and the
# health check below always agree on it. The image is the only distribution that
# serves the web dashboard over HTTP (installer-based installs use the desktop app).
ENV MONGORESCUE_DATA_DIR=/data \
    MONGORESCUE_SERVER_PORT=8080 \
    MONGORESCUE_DASHBOARD=true

VOLUME ["/data", "/backups"]

EXPOSE 8080

USER 10001:10001

# busybox wget ships with the Alpine base image; /api/v1/health needs no API key.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- "http://127.0.0.1:${MONGORESCUE_SERVER_PORT:-8080}/api/v1/health" >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/mongorescue"]
