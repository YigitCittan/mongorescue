#!/usr/bin/env bash
# ==============================================================================
# MongoRescue — installs the pinned helm binary for CI
#
# Usage: scripts/install-helm.sh <bin-dir>
#
# Environment (all required):
#   HELM_VERSION       e.g. v4.3.0
#   HELM_SHA256        SHA-256 of the official helm-<version>-linux-amd64.tar.gz
#   HELM_BIN_SHA256    SHA-256 of the helm binary inside that archive
#   HELM_IMAGE         the same release as a container image, pinned by digest
#                      (docker.io/alpine/helm:<version>@sha256:...)
#
# get.helm.sh answers GitHub's runners with 504s at times. The archive is tried
# first (with retries); when it cannot be fetched, the binary is copied out of the
# pinned image instead. Either way the binary must match HELM_BIN_SHA256, the hash
# of the binary in the official archive, so the fallback runs the same bytes.
# ==============================================================================
set -euo pipefail

bin_dir=${1:?usage: install-helm.sh <bin-dir>}
: "${HELM_VERSION:?}" "${HELM_SHA256:?}" "${HELM_BIN_SHA256:?}" "${HELM_IMAGE:?}"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$bin_dir"

if curl -fsSL --retry 5 --retry-all-errors --retry-delay 5 \
  -o "$work/helm.tgz" "https://get.helm.sh/helm-${HELM_VERSION}-linux-amd64.tar.gz"; then
  echo "${HELM_SHA256}  $work/helm.tgz" | sha256sum -c -
  tar -xzf "$work/helm.tgz" -C "$work" linux-amd64/helm
  mv "$work/linux-amd64/helm" "$work/helm"
else
  echo "::warning::get.helm.sh is unreachable; copying helm ${HELM_VERSION} out of ${HELM_IMAGE}"
  docker pull --quiet --platform linux/amd64 "$HELM_IMAGE" >/dev/null
  id=$(docker create --platform linux/amd64 "$HELM_IMAGE")
  docker cp "$id:/usr/bin/helm" "$work/helm"
  docker rm -f "$id" >/dev/null
fi

echo "${HELM_BIN_SHA256}  $work/helm" | sha256sum -c -
install -m 0755 "$work/helm" "$bin_dir/helm"
"$bin_dir/helm" version --short
