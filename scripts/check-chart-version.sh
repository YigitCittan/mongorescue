#!/usr/bin/env bash
# Fails when the Helm chart changed relative to a base ref without a higher
# `version` in deploy/helm/mongorescue/Chart.yaml (rule: docs/versioning.md#helm-chart-versions).
#
# Usage: scripts/check-chart-version.sh [base-ref]   (default: origin/main)
#
# Changes that only touch the chart's ci/ test values or Markdown files are exempt:
# they are not part of the packaged chart's behaviour.
set -euo pipefail

base="${1:-origin/main}"
chart="deploy/helm/mongorescue"

mb="$(git merge-base "$base" HEAD)"

# Committed changes plus uncommitted ones in the working tree.
changed="$(git diff --name-only "$mb" -- "$chart" | grep -v -e "^${chart}/ci/" -e '\.md$' || true)"
if [ -z "$changed" ]; then
  echo "No chart changes relative to ${base}; no version bump needed."
  exit 0
fi

chart_version() { sed -n 's/^version:[[:space:]]*"\{0,1\}\([^"[:space:]]*\)"\{0,1\}[[:space:]]*$/\1/p' | head -n1; }
old="$(git show "${mb}:${chart}/Chart.yaml" | chart_version)"
new="$(chart_version < "${chart}/Chart.yaml")"

if [ -z "$old" ] || [ -z "$new" ]; then
  echo "::error::could not read the chart version (base '${old}', head '${new}')"
  exit 1
fi

echo "Chart files changed relative to ${base}:"
mapfile -t files <<< "$changed"
printf '  %s\n' "${files[@]}"
echo "Chart version: ${old} -> ${new}"

highest="$(printf '%s\n%s\n' "$old" "$new" | sort -V | tail -n1)"
if [ "$old" = "$new" ] || [ "$highest" != "$new" ]; then
  echo "::error file=${chart}/Chart.yaml::the chart changed but its version did not increase (${old} -> ${new}). Bump 'version' in Chart.yaml: minor for new values or features, major for breaking value changes, patch for fixes (docs/versioning.md#helm-chart-versions)."
  exit 1
fi
echo "OK: the chart version increased."
