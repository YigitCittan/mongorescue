# The container the replica set scenarios and the soak test run in: on the
# replica set's network, with MongoDB Database Tools TOOLS_VERSION and the docker
# CLI (the tests inject failures through the mounted docker socket). The test
# binary is built on the host and mounted at /work.
FROM ubuntu:24.04

ARG TOOLS_VERSION=100.12.2
ARG TARGETARCH

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl; \
    case "${TARGETARCH}" in arm64) arch=arm64 ;; *) arch=x86_64 ;; esac; \
    curl -fsSL -o /tmp/tools.deb \
      "https://fastdl.mongodb.org/tools/db/mongodb-database-tools-ubuntu2404-${arch}-${TOOLS_VERSION}.deb"; \
    apt-get install -y --no-install-recommends /tmp/tools.deb; \
    rm -rf /var/lib/apt/lists/* /tmp/tools.deb; \
    mongorestore --version

COPY --from=docker:28-cli /usr/local/bin/docker /usr/local/bin/docker

WORKDIR /work
