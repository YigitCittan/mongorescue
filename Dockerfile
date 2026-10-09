# Multi-stage lightweight Dockerfile for MongoRescue
# Produces a self-contained image (<30MB) with pre-installed MongoDB database tools.

# Stage 1: Build static Go binary
FROM golang:1.27-alpine@sha256:738d1cf061836894ff6bb8c33881080ac66de8cf0586615012a0c8f592649cfa AS builder

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

# Build stripped static binary
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X main.Version=${VERSION} -X main.Commit=${COMMIT} -X main.Date=$(date -u +%Y-%m-%d)" \
    -o /bin/mongorescue ./cmd/mongorescue

# Stage 2: Minimal runtime image
FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

# Install official MongoDB database tools (mongodump, mongorestore) and TLS certs
RUN apk add --no-cache \
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
