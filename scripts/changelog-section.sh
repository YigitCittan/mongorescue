#!/bin/sh
# ==============================================================================
# MongoRescue — prints the CHANGELOG.md section of one version
#
# Usage: scripts/changelog-section.sh <version> [changelog]
#
# <version> is x.y.z or vx.y.z. Prints the body of the "## [x.y.z] - YYYY-MM-DD"
# section up to the next "## [" heading, without the heading and without leading
# and trailing blank lines. Exits with status 1 when the section is missing or
# empty and 2 on a usage error. The release workflow passes the output to
# GoReleaser as the GitHub release notes.
# ==============================================================================
set -eu

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ] || [ -z "$1" ]; then
  echo "usage: $0 <version> [changelog]" >&2
  exit 2
fi
version=${1#v}
changelog=${2:-CHANGELOG.md}
if [ ! -r "$changelog" ]; then
  echo "$0: cannot read $changelog" >&2
  exit 2
fi

section=$(awk -v heading="## [$version]" '
  # A heading of this version: "## [x.y.z]" followed by the end of the line or a space.
  function is_version(line) {
    return index(line, heading) == 1 && (length(line) == length(heading) || substr(line, length(heading) + 1, 1) == " ")
  }
  /^## \[/ { if (found) exit; if (is_version($0)) { found = 1; next } }
  found {
    if ($0 ~ /^[[:space:]]*$/) { blank++; next }  # hold blank lines back
    if (started) { while (blank-- > 0) print "" }
    blank = 0; started = 1
    print
  }
' "$changelog")

if [ -z "$section" ]; then
  echo "$0: $changelog has no or an empty \"## [$version]\" section" >&2
  exit 1
fi
printf '%s\n' "$section"
