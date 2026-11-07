#!/bin/sh
# ==============================================================================
# MongoRescue — prints the release notes of one version from CHANGELOG.md
#
# Usage: scripts/changelog-section.sh <version> [changelog]
#
# <version> is x.y.z or vx.y.z. Takes the body of the "## [x.y.z] - YYYY-MM-DD"
# section up to the next "## [" heading and prints it as release notes in a fixed
# shape, so every release starts the same way:
#
#   ## Highlights       the section's "### Highlights" (when there is none, the
#                       first sentence of the first three Added/Changed entries)
#   ## Upgrade notes    the section's "### Upgrade notes", or a line saying that
#                       nothing is needed beyond the usual upgrade steps
#   ## Changes          every other "###" subsection (Added, Changed, Fixed, ...)
#
# Links into docs/ are made absolute (to the tag of the version), because the notes
# are shown outside the repository. The notes always point at the upgrade guide. Exits with status 1 when the
# section is missing or empty and 2 on a usage error. The release workflow passes
# the output to GoReleaser as the release notes of the hosted release;
# CONTRIBUTING.md describes the "### Highlights" and "### Upgrade notes"
# convention.
# ==============================================================================
set -eu

upgrading_url="https://yigitcittan.github.io/mongorescue/upgrading/"

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

printf '%s\n' "$section" | awk -v url="$upgrading_url" '
  # trim removes the blank lines around a block.
  function trim(s) { sub(/^\n+/, "", s); sub(/\n+$/, "", s); return s }
  # headline is the first sentence of a changelog entry, without list marker and bold.
  function headline(s,   i) {
    sub(/^- +/, "", s); gsub(/\*\*/, "", s)
    i = index(s, ". ")
    if (i > 0) s = substr(s, 1, i)
    if (length(s) > 200) s = substr(s, 1, 197) "..."
    return s
  }
  /^### / {
    name = tolower($0); sub(/^### +/, "", name); sub(/ +$/, "", name)
    mode = (name == "highlights") ? "hl" : (name == "upgrade notes") ? "up" : "rest"
    if (mode == "rest") { rest = rest $0 "\n"; auto_ok = (name == "added" || name == "changed") }
    next
  }
  {
    if (mode == "hl") hl = hl $0 "\n"
    else if (mode == "up") up = up $0 "\n"
    else {
      rest = rest $0 "\n"
      if (auto_ok && $0 ~ /^- / && autos < 3) { auto = auto "- " headline($0) "\n"; autos++ }
    }
  }
  END {
    hl = trim(hl); up = trim(up); rest = trim(rest); auto = trim(auto)
    print "## Highlights\n"
    if (hl != "") print hl
    else if (auto != "") print auto
    else print "See the changes below."
    print "\n## Upgrade notes\n"
    if (up != "") print up
    else print "Nothing to do beyond the usual upgrade steps."
    print "\nBefore upgrading, back up and read the [upgrade guide](" url "): it covers the pre-migration database copy, the order for HA and how to roll back."
    if (rest != "") print "\n## Changes\n\n" rest
  }
' | sed "s|](docs/|](https://github.com/YigitCittan/mongorescue/blob/v${version}/docs/|g"
