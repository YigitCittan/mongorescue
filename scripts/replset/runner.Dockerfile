# The container the replica set scenarios and the soak test run in: on the
# replica set's network, with MongoDB Database Tools TOOLS_VERSION and the docker
# CLI (the tests inject failures through the mounted docker socket). The test
# binary is built on the host and mounted at /work.
#
# The base images are pinned by digest (multi-arch indexes) and the Database
# Tools package is checked against its SHA-256: the checksums of the versions CI
# uses are below; another version needs TOOLS_SHA256 (the package's checksum for
# the build architecture, from https://downloads.mongodb.org/tools/db/release.json).
FROM docker:28-cli@sha256:625d9431a9f54c5a2bc90f24f0e1c3d55b1349fd857dd85035f98c2c9acbdd4d AS docker-cli

FROM ubuntu:24.04@sha256:534baea6a22c03a63003dbc8dbe78fe34bc0d7e595d9a9dc9834884ff530eb55

ARG TOOLS_VERSION=100.12.2
ARG TOOLS_SHA256=
ARG TARGETARCH

RUN set -eux; \
    case "${TARGETARCH}" in arm64) arch=arm64 ;; *) arch=x86_64 ;; esac; \
    sum="${TOOLS_SHA256}"; \
    if [ -z "$sum" ]; then \
      case "${TOOLS_VERSION}-${arch}" in \
        100.12.2-x86_64) sum=79c5dc5d13f1ec520a6698eaf35aa8fdf0c461b51e4a886210a5dff08dc520fd ;; \
        100.12.2-arm64)  sum=8a7edd38fe2888954e8a04f89a850883ea9a85671069863c8aae070c87633c1e ;; \
        100.19.1-x86_64) sum=4bb0482aced31090b9ab1615b75db90ec3eea91c01ba45d03454748cc3658b69 ;; \
        100.19.1-arm64)  sum=c91464898fe859f0b49b0e46340ee2cbcef48fe5751e1a46c9d63309e651bdfa ;; \
        *) echo "no checksum for Database Tools ${TOOLS_VERSION} (${arch}): set TOOLS_SHA256" >&2; exit 1 ;; \
      esac; \
    fi; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl; \
    curl -fsSL -o /tmp/tools.deb \
      "https://fastdl.mongodb.org/tools/db/mongodb-database-tools-ubuntu2404-${arch}-${TOOLS_VERSION}.deb"; \
    echo "${sum}  /tmp/tools.deb" | sha256sum -c -; \
    apt-get install -y --no-install-recommends /tmp/tools.deb; \
    rm -rf /var/lib/apt/lists/* /tmp/tools.deb; \
    mongorestore --version

COPY --from=docker-cli /usr/local/bin/docker /usr/local/bin/docker

WORKDIR /work
