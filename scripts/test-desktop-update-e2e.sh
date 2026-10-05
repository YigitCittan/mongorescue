#!/usr/bin/env bash
# ==============================================================================
# Desktop update end-to-end test (docs/desktop.md#how-the-update-is-tested).
#
# Builds the desktop app twice from the working tree with the Wails CLI, stamped
# E2E_OLD_VERSION and E2E_NEW_VERSION (main.Version and wails.json productVersion,
# as the release does) and with the desktop_e2e build tag, packages each build
# exactly as the "Package artifacts" step of .github/workflows/release.yml does,
# writes the release's checksums file, and runs the Go test in
# internal/desktop/updatee2e, which installs the old build, serves the new one from
# a fake GitHub release on a loopback port and updates it through the app.
#
# The MongoDB Database Tools are replaced by small placeholder files (the update
# only moves tools/ around, it never runs them), and the Windows installer by a
# placeholder that the test checks is never downloaded or started.
#
# Prerequisites: Go, the Wails CLI (WAILS, default wails; the version in go.mod),
# jq, and the platform's webview build dependencies (see the Makefile's desktop
# target); 7z on Windows, ditto on macOS. Run it from bash (Git Bash on Windows).
#
# Environment knobs:
#   E2E_OLD_VERSION, E2E_NEW_VERSION  versions to build (default 0.0.1, 0.0.2)
#   E2E_OUT       directory for the builds and release files (default: a new
#                 temporary directory, removed afterwards unless E2E_KEEP=1)
#   E2E_KEEP      1 keeps E2E_OUT
#   E2E_PLATFORM  wails -platform (default: the release's platform for this OS)
#   DESKTOP_TAGS  build tags without desktop_e2e (default: the release's tags)
#   GOTESTFLAGS   extra go test flags (default: -v)
#
# cmd/mongorescue-desktop/build/bin and wails.json are moved aside during the
# builds and restored afterwards.
# ==============================================================================
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DESKTOP_DIR="$ROOT/cmd/mongorescue-desktop"
WAILS="${WAILS:-wails}"
OLD_VERSION="${E2E_OLD_VERSION:-0.0.1}"
NEW_VERSION="${E2E_NEW_VERSION:-0.0.2}"
GOTESTFLAGS="${GOTESTFLAGS:--v}"

log() { printf '==> %s\n' "$*"; }

case "$(uname -s)" in
  Darwin) OS=macos; platform=darwin/universal; tags=desktop ;;
  Linux) OS=linux; platform=linux/amd64; tags=desktop,webkit2_41 ;; # Ubuntu 24.04: WebKitGTK 4.1
  MINGW* | MSYS* | CYGWIN*) OS=windows; platform=windows/amd64; tags=desktop ;;
  *) echo "unsupported OS: $(uname -s)" >&2; exit 1 ;;
esac
PLATFORM="${E2E_PLATFORM:-$platform}"
TAGS="${DESKTOP_TAGS:-$tags},desktop_e2e"

if [ -n "${E2E_OUT:-}" ]; then
  OUT="$E2E_OUT"
  # Git Bash: the script works with /d/... paths, not D:\... ones.
  if [ "$OS" = windows ]; then OUT="$(cygpath -u "$OUT")"; fi
  mkdir -p "$OUT"
else
  OUT="$(mktemp -d "${TMPDIR:-/tmp}/mongorescue-desktop-e2e.XXXXXX")"
fi
SAVED="$OUT/saved"
mkdir -p "$SAVED"

restore() {
  local status=$?
  if [ -f "$SAVED/wails.json" ]; then
    mv -f "$SAVED/wails.json" "$DESKTOP_DIR/wails.json"
  fi
  rm -rf "$DESKTOP_DIR/build/bin"
  if [ -d "$SAVED/bin" ]; then
    mv "$SAVED/bin" "$DESKTOP_DIR/build/bin"
  fi
  if [ -z "${E2E_OUT:-}" ] && [ "${E2E_KEEP:-}" != "1" ]; then
    rm -rf "$OUT"
  else
    log "builds and release files kept in $OUT"
  fi
  exit "$status"
}
trap restore EXIT

cp "$DESKTOP_DIR/wails.json" "$SAVED/wails.json"
if [ -d "$DESKTOP_DIR/build/bin" ]; then
  mv "$DESKTOP_DIR/build/bin" "$SAVED/bin"
fi

sha256() {
  if command -v sha256sum >/dev/null; then sha256sum -- "$@"; else shasum -a 256 -- "$@"; fi
}

# build <version>: builds and packages <version> into $OUT/<version>/, with the
# release's asset names and checksums file.
build() {
  local version="$1"
  local dist="$OUT/$version"
  local bin="$DESKTOP_DIR/build/bin"
  local ext=""
  rm -rf "$bin" "$dist"
  mkdir -p "$bin/tools" "$dist"
  if [ "$OS" = windows ]; then ext=.exe; fi

  log "building the desktop app $version ($PLATFORM, tags $TAGS)"
  # Placeholders for the bundled MongoDB Database Tools, staged where the release
  # stages them (before the build).
  for t in mongodump mongorestore; do
    printf 'placeholder %s %s\n' "$t" "$version" > "$bin/tools/$t$ext"
    chmod 0755 "$bin/tools/$t$ext"
  done
  printf 'placeholder license %s\n' "$version" > "$bin/tools/LICENSE.md"

  # The release's "Build desktop app" step, without -nsis and with desktop_e2e.
  jq --arg v "$version" '.info.productVersion = $v' "$SAVED/wails.json" > "$DESKTOP_DIR/wails.json"
  (cd "$DESKTOP_DIR" && CGO_ENABLED=1 "$WAILS" build -trimpath -s -skipbindings -m -nosyncgomod \
    -platform "$PLATFORM" -tags "$TAGS" \
    -ldflags "-X main.Version=$version -X main.Commit=e2e")
  cp "$SAVED/wails.json" "$DESKTOP_DIR/wails.json"

  # The release's "Add the MongoDB Database Tools to the app bundle" and "Package
  # artifacts" steps.
  local out="$dist/MongoRescue-desktop_${version}"
  (
    cd "$bin"
    case "$OS" in
      windows)
        7z a -bso0 "${out}_windows_amd64_portable.zip" MongoRescue.exe tools
        # The swap never uses the installer; the release must list one to be
        # installable, and the test checks it is never downloaded or started.
        printf 'placeholder installer %s: never started by the update\n' "$version" > "${out}_windows_amd64_installer.exe" ;;
      macos)
        res=MongoRescue.app/Contents/Resources
        mkdir -p "$res"
        rm -rf "$res/tools"
        mv tools "$res/tools"
        if [ -d MongoRescue.app/Contents/_CodeSignature ]; then
          codesign --force --sign - MongoRescue.app
          codesign --verify --strict MongoRescue.app
        fi
        ditto -c -k --keepParent MongoRescue.app "${out}_macos_universal.zip" ;;
      linux)
        tar -czf "${out}_linux_amd64.tar.gz" MongoRescue tools ;;
    esac
  )

  # The release's "desktop-checksums" job: one sha256sum line per desktop asset.
  (
    cd "$dist"
    local prefix="MongoRescue-desktop_${version}_"
    local files=()
    for f in "$prefix"*; do files+=("$f"); done
    sha256 "${files[@]}" > "${prefix}checksums.txt"
    cat "${prefix}checksums.txt"
  )
  ls -l "$dist"
}

build "$OLD_VERSION"
build "$NEW_VERSION"

native="$OUT"
if [ "$OS" = windows ]; then native="$(cygpath -w "$OUT")"; fi
log "running the update test"
cd "$ROOT"
# shellcheck disable=SC2086 # GOTESTFLAGS is a list of flags
MONGORESCUE_E2E_ARTIFACTS="$native" \
  MONGORESCUE_E2E_OLD_VERSION="$OLD_VERSION" \
  MONGORESCUE_E2E_NEW_VERSION="$NEW_VERSION" \
  go test -tags desktop_e2e -count=1 -timeout 15m $GOTESTFLAGS ./internal/desktop/updatee2e/
