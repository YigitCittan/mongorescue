#!/bin/sh
# ==============================================================================
# MongoRescue — breaking-change check of the REST API description
#
# Usage: scripts/check-openapi-breaking.sh [base-ref]
#
# Compares docs/openapi.yaml at <base-ref> (default: the last v* tag reachable from
# HEAD) with the working copy, using oasdiff in its container image. When the base
# has no docs/openapi.yaml (the spec is newer than the last tag), or there is no tag,
# the check passes. Breaking changes pass only when the [Unreleased] section of
# CHANGELOG.md has an entry that starts with the "**Breaking" marker (the convention of every
# "**Breaking:**" entry), so a breaking change cannot be merged unannounced.
#
# Needs docker and a clone that has the tags (fetch-depth: 0 in CI).
# Exit status: 0 compatible or announced, 1 unannounced breaking changes, 2 usage or
# tooling error.
# ==============================================================================
set -eu

# Pinned by version; bump it deliberately, never use latest.
OASDIFF_IMAGE=${OASDIFF_IMAGE:-tufin/oasdiff:v1.35.0}
SPEC=docs/openapi.yaml

if [ "$#" -gt 1 ]; then
  echo "usage: $0 [base-ref]" >&2
  exit 2
fi
base=${1:-$(git describe --tags --abbrev=0 --match 'v[0-9]*' 2>/dev/null || true)}
if [ -z "$base" ]; then
  echo "No release tag to compare with; the API has no baseline yet."
  exit 0
fi

workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' EXIT INT TERM
if ! git show "$base:$SPEC" >"$workdir/base.yaml" 2>/dev/null; then
  echo "$SPEC does not exist at $base; nothing to compare, passing."
  exit 0
fi
cp "$SPEC" "$workdir/head.yaml"

echo "Comparing $SPEC at $base with the working copy ($OASDIFF_IMAGE)"
status=0
docker run --rm -v "$workdir:/spec:ro" "$OASDIFF_IMAGE" \
  breaking /spec/base.yaml /spec/head.yaml --fail-on ERR >"$workdir/report.txt" 2>&1 || status=$?
cat "$workdir/report.txt"
if [ "$status" -eq 0 ]; then
  echo "No breaking changes."
  exit 0
fi
# oasdiff exits 1 when --fail-on is hit; any other status is a tooling or parse error.
if [ "$status" -ne 1 ]; then
  echo "oasdiff failed (exit status $status)." >&2
  exit 2
fi

if scripts/changelog-section.sh Unreleased 2>/dev/null | grep -qF '**Breaking'; then
  echo "Breaking changes are announced by a **Breaking:** entry in CHANGELOG.md [Unreleased]."
  exit 0
fi
echo "::error::Breaking API changes since $base need a **Breaking:** entry in the [Unreleased] section of CHANGELOG.md." >&2
exit 1
