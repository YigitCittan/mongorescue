#!/usr/bin/env bash
# ==============================================================================
# Native end-to-end test: the real MongoDB Database Tools and a real mongod on
# Windows (Git Bash) and macOS, without Docker (the Windows runners cannot run Linux
# containers). It downloads the official MongoDB Community server archive, the
# Database Tools and mongosh, starts a mongod with access control on a loopback port,
# and runs the part of the integration suite that depends on the operating system:
#   - TestNativeToolchain       tool lookup (.exe), the version, the private --config file
#   - TestBackupRestoreRoundTrip, TestRestoreFidelity
#                               backup and safe-clone restore with the real tools
#   - TestPostBackupVerification, TestRestorePreflightAndVerification
#                               checksum verification of the stored archive
#   - TestCancelRealBackupMidRun, TestCancelRealRestoreMidRun,
#     TestInterruptedBackupsLeaveNothingBehind
#                               cancellation: the tool process is stopped and no
#                               partial archive is left (atomic rename on local disk)
#   - TestSelectiveRestore, TestRestoreTestEndToEnd, TestOrphanImportEndToEnd,
#     TestMongoConnProber, TestServerEndToEnd
#                               collection filters, the restore test, orphan import and
#                               the whole REST API path over a local storage target
# It also runs on Linux (the server archive is not downloaded there: set
# MONGOD_BIN_DIR), which is how the script itself is kept honest.
#
# Environment knobs:
#   MONGO_SERVER_VERSION  Community server release (default 8.0.34)
#   MONGO_TOOLS_VERSION   Database Tools release (default 100.12.2, the one the
#                         desktop packages bundle)
#   MONGOSH_VERSION       mongosh release (default 2.5.0), used to create the user
#   MONGOD_BIN_DIR, TOOLS_BIN_DIR, MONGOSH_BIN_DIR
#                         directories that already hold mongod, mongodump/mongorestore
#                         and mongosh: nothing is downloaded for that component
#   NATIVE_DIR            working directory (default: a temporary one, removed on exit;
#                         a given one is kept)
#   NATIVE_PORT           loopback port of the mongod (default 27117)
#   NATIVE_RACE           1 (default) runs go test with -race; 0 without (the race
#                         detector needs a C compiler on Windows)
#   NATIVE_TESTS          go test -run expression overriding the list above
#   IT_TIMEOUT            go test -timeout (default 25m)
# ==============================================================================
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MONGO_SERVER_VERSION="${MONGO_SERVER_VERSION:-8.0.34}"
MONGO_TOOLS_VERSION="${MONGO_TOOLS_VERSION:-100.12.2}"
MONGOSH_VERSION="${MONGOSH_VERSION:-2.5.0}"
NATIVE_PORT="${NATIVE_PORT:-27117}"
NATIVE_RACE="${NATIVE_RACE:-1}"
NATIVE_TESTS="${NATIVE_TESTS:-^(TestNativeToolchain|TestBackupRestoreRoundTrip|TestRestoreFidelity|TestPostBackupVerification|TestRestorePreflightAndVerification|TestCancelRealBackupMidRun|TestCancelRealRestoreMidRun|TestInterruptedBackupsLeaveNothingBehind|TestSelectiveRestore|TestRestoreTestEndToEnd|TestOrphanImportEndToEnd|TestMongoConnProber|TestServerEndToEnd)\$}"

log() { printf '==> %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

case "$(uname -s)" in
  MINGW* | MSYS* | CYGWIN*)
    OS=windows
    EXE=.exe
    SERVER_URL="https://fastdl.mongodb.org/windows/mongodb-windows-x86_64-${MONGO_SERVER_VERSION}.zip"
    TOOLS_URL="https://fastdl.mongodb.org/tools/db/mongodb-database-tools-windows-x86_64-${MONGO_TOOLS_VERSION}.zip"
    MONGOSH_URL="https://downloads.mongodb.com/compass/mongosh-${MONGOSH_VERSION}-win32-x64.zip"
    ;;
  Darwin)
    OS=macos
    EXE=
    case "$(uname -m)" in
      arm64) arch=arm64 mongosh_arch=arm64 ;;
      *) arch=x86_64 mongosh_arch=x64 ;;
    esac
    SERVER_URL="https://fastdl.mongodb.org/osx/mongodb-macos-${arch}-${MONGO_SERVER_VERSION}.tgz"
    TOOLS_URL="https://fastdl.mongodb.org/tools/db/mongodb-database-tools-macos-${arch}-${MONGO_TOOLS_VERSION}.zip"
    MONGOSH_URL="https://downloads.mongodb.com/compass/mongosh-${MONGOSH_VERSION}-darwin-${mongosh_arch}.zip"
    ;;
  *)
    OS=other
    EXE=
    SERVER_URL=
    TOOLS_URL=
    MONGOSH_URL=
    ;;
esac

# native_path PATH prints PATH in the form native programs expect (C:/... on Windows).
native_path() {
  if [ "$OS" = windows ]; then cygpath -m "$1"; else printf '%s\n' "$1"; fi
}

CLEAN_DIR=1
if [ -n "${NATIVE_DIR:-}" ]; then CLEAN_DIR=0; fi
NATIVE_DIR="${NATIVE_DIR:-$(mktemp -d)}"
mkdir -p "$NATIVE_DIR"
NATIVE_DIR="$(cd "$NATIVE_DIR" && pwd)"
DB_DIR="$NATIVE_DIR/data"
LOG_FILE="$NATIVE_DIR/mongod.log"
rm -rf "$DB_DIR" "$LOG_FILE"
mkdir -p "$DB_DIR"
MONGOSH=""
MONGOD_STARTED=0
ROOT_PW="nat$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"

mongosh_eval() { "$MONGOSH" --quiet --port "$NATIVE_PORT" "$@"; }

cleanup() {
  local status=$?
  # The log first: after the shutdown its tail is only the shutdown sequence.
  if [ "$status" -ne 0 ] && [ -f "$LOG_FILE" ]; then
    echo "---- mongod log (warnings and errors, tail) ----"
    grep -E '"s":"(W|E|F)"' "$LOG_FILE" | tail -n 30 | cut -c1-400 | sed "s/${ROOT_PW}/******/g" || true
  fi
  if [ "$MONGOD_STARTED" = 1 ]; then
    # shutdownServer needs the root user; ignore failures, the process is gone anyway
    # when the runner ends.
    mongosh_eval -u root -p "$ROOT_PW" --authenticationDatabase admin \
      --eval "db.getSiblingDB('admin').shutdownServer({force: true})" >/dev/null 2>&1 || true
    sleep 2
  fi
  if [ "$CLEAN_DIR" = 1 ]; then rm -rf "$NATIVE_DIR" 2>/dev/null || true; fi
  exit "$status"
}
trap cleanup EXIT

# extract ARCHIVE DIR unpacks a .zip or .tgz into DIR with what the platform has.
extract() {
  local archive=$1 dir=$2
  mkdir -p "$dir"
  case "$archive" in
    *.tgz) tar -xzf "$archive" -C "$dir" ;;
    *.zip)
      if command -v unzip >/dev/null 2>&1; then
        unzip -q "$archive" -d "$dir"
      elif command -v powershell.exe >/dev/null 2>&1; then
        powershell.exe -NoProfile -NonInteractive -Command \
          "Expand-Archive -LiteralPath '$(native_path "$archive")' -DestinationPath '$(native_path "$dir")' -Force"
      else
        tar -xf "$archive" -C "$dir"
      fi
      ;;
    *) fail "unknown archive type: $archive" ;;
  esac
}

# fetch NAME URL DIR downloads and unpacks URL into DIR and prints its bin directory.
fetch() {
  local name=$1 url=$2 dir=$3 archive bindir
  [ -n "$url" ] || fail "no $name download for $(uname -s); point the matching *_BIN_DIR variable at an installation"
  archive="$NATIVE_DIR/$name.${url##*.}"
  log "downloading $url" >&2
  curl -fsSL --retry 3 --retry-delay 5 -o "$archive" "$url"
  extract "$archive" "$dir"
  rm -f "$archive"
  bindir="$(find "$dir" -type d -name bin | head -n 1)"
  [ -n "$bindir" ] || fail "no bin directory in $url"
  printf '%s\n' "$bindir"
}

# Short directory names: the extracted server tree is deep and Windows limits paths.
MONGOD_BIN_DIR="${MONGOD_BIN_DIR:-}"
TOOLS_BIN_DIR="${TOOLS_BIN_DIR:-}"
MONGOSH_BIN_DIR="${MONGOSH_BIN_DIR:-}"
[ -n "$MONGOD_BIN_DIR" ] || MONGOD_BIN_DIR="$(fetch mongod "$SERVER_URL" "$NATIVE_DIR/m")"
[ -n "$TOOLS_BIN_DIR" ] || TOOLS_BIN_DIR="$(fetch tools "$TOOLS_URL" "$NATIVE_DIR/t")"
[ -n "$MONGOSH_BIN_DIR" ] || MONGOSH_BIN_DIR="$(fetch mongosh "$MONGOSH_URL" "$NATIVE_DIR/s")"
MONGOSH="$MONGOSH_BIN_DIR/mongosh$EXE"

for f in "$MONGOD_BIN_DIR/mongod$EXE" "$TOOLS_BIN_DIR/mongodump$EXE" "$TOOLS_BIN_DIR/mongorestore$EXE" "$MONGOSH"; do
  [ -f "$f" ] || fail "missing $f"
done
export PATH="$TOOLS_BIN_DIR:$PATH"
log "$(mongodump --version | head -n 1) ($(command -v mongodump))"
log "$("$MONGOD_BIN_DIR/mongod$EXE" --version | head -n 1)"

# The first user is created through the localhost exception, so mongod runs with
# access control from the start: the tools then get the credentials through the
# private --config file, as in production.
log "starting mongod on 127.0.0.1:$NATIVE_PORT"
"$MONGOD_BIN_DIR/mongod$EXE" --dbpath "$(native_path "$DB_DIR")" --logpath "$(native_path "$LOG_FILE")" \
  --port "$NATIVE_PORT" --bind_ip 127.0.0.1 --auth >/dev/null 2>&1 &
MONGOD_STARTED=1

ready=0
for _ in $(seq 1 90); do
  if mongosh_eval --eval 'quit(db.runCommand({ping: 1}).ok ? 0 : 1)' >/dev/null 2>&1; then ready=1; break; fi
  sleep 1
done
[ "$ready" = 1 ] || fail "mongod did not answer on port $NATIVE_PORT within 90s"
mongosh_eval --eval "db.getSiblingDB('admin').createUser({user: 'root', pwd: '${ROOT_PW}', roles: ['root']})" >/dev/null
log "mongod ready, root user created"

export MONGORESCUE_TEST_MONGO_URI="mongodb://root:${ROOT_PW}@127.0.0.1:${NATIVE_PORT}/?authSource=admin"

RACE_FLAG=""
if [ "$NATIVE_RACE" = 1 ]; then RACE_FLAG="-race"; fi
log "running the native tests on $OS (race: $NATIVE_RACE)"
cd "$ROOT"
# shellcheck disable=SC2086 # RACE_FLAG is intentionally empty or one flag
go test $RACE_FLAG -tags=integration -count=1 -timeout "${IT_TIMEOUT:-25m}" -run "$NATIVE_TESTS" -v ./internal/integration/
