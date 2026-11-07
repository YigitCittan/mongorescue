#!/usr/bin/env bash
# Fails when the release Go toolchain is not defined in exactly one place.
#
# The source of truth is .go-version. Every workflow reads it through
# actions/setup-go (go-version-file), the Dockerfile default must equal it, and
# the pinned govulncheck version must match between ci.yml and the Makefile.
# The CI test matrix (the go.mod minimum next to the "release" cell) is exempt.
set -euo pipefail

cd "$(dirname "$0")/.."

fail=0
err() { echo "go-version drift: $*" >&2; fail=1; }

want=$(tr -d '[:space:]' < .go-version)
[[ "$want" =~ ^1\.[0-9]+\.[0-9]+$ ]] || err ".go-version must be a full version such as 1.27.3, got '$want'"

# go.mod's minimum must not exceed the toolchain.
mod=$(sed -n 's/^go \([0-9.]*\)\r\{0,1\}$/\1/p' go.mod)
if [[ -n "$mod" && "$(printf '%s\n%s\n' "$mod" "$want" | sort -V | tail -n1)" != "$want" ]]; then
  err "go.mod requires go $mod, newer than .go-version $want"
fi

# Dockerfile default build arg and its use in the builder stage.
df=$(sed -n 's/^ARG GO_VERSION=\(.*\)$/\1/p' Dockerfile | tr -d '\r')
[[ "$df" == "$want" ]] || err "Dockerfile ARG GO_VERSION is '$df', .go-version is '$want'"
grep -Eq '^FROM (--platform=\$BUILDPLATFORM )?golang:\$\{GO_VERSION\}-alpine@sha256:' Dockerfile \
  || err "Dockerfile builder stage must be 'FROM [--platform=\$BUILDPLATFORM ]golang:\${GO_VERSION}-alpine@sha256:...'"

# The image workflow must pass the version as a build arg.
# shellcheck disable=SC2016 # ${{ }} is GitHub Actions syntax, matched literally
grep -q 'GO_VERSION=\${{ steps.goversion.outputs.version }}' .github/workflows/docker.yml \
  || err "docker.yml must pass GO_VERSION from .go-version as a build arg"

# No workflow hard-codes a Go version, except the test matrix cells in ci.yml.
for f in .github/workflows/*.yml; do
  while IFS= read -r hit; do
    [[ -n "$hit" ]] || continue
    case "$f:$hit" in
      *ci.yml:*"'release'"*) ;; # the matrix: go.mod minimum and the release cell
      *) err "$f hard-codes a Go version: $hit" ;;
    esac
  done < <(grep -nE "(^|[[:space:]])go-version:[[:space:]]*\[?['\"]?[0-9]" "$f" || true)
  if grep -qE '^[[:space:]]*GO_VERSION:' "$f"; then
    err "$f defines GO_VERSION; read .go-version instead"
  fi
  if grep -qE '(GOTOOLCHAIN|toolchain)[:=][[:space:]]*(go)?1\.' "$f"; then
    err "$f pins a toolchain; read .go-version instead"
  fi
done

# govulncheck: pinned, and the same version in ci.yml and the Makefile.
ci=$(sed -n 's/.*govulncheck@\(v[0-9][0-9.]*\).*/\1/p' .github/workflows/ci.yml | sort -u | tr -d '\r')
mk=$(sed -n 's/^GOVULNCHECK_VERSION[[:space:]]*?\{0,1\}=[[:space:]]*\(v[0-9][0-9.]*\).*/\1/p' Makefile | tr -d '\r')
[[ -n "$ci" && "$ci" != *$'\n'* ]] || err "ci.yml must pin govulncheck to one released version, found '${ci:-none}'"
[[ -n "$mk" ]] || err "Makefile must define GOVULNCHECK_VERSION as a released version"
[[ "$ci" == "$mk" ]] || err "govulncheck is '$ci' in ci.yml but '$mk' in the Makefile"

if [[ $fail -ne 0 ]]; then
  exit 1
fi
echo "go toolchain consistent: go $want, govulncheck $ci"
