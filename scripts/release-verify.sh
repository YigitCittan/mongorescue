#!/usr/bin/env bash
# ==============================================================================
# Release verification: proves that what is about to be published (or was just
# built) actually works, beyond unit tests.
#
#   archive BINARY VERSION        Smoke test of one unpacked release binary on any
#                                 OS: --version reports VERSION with a real commit,
#                                 the server starts on an empty temporary data
#                                 directory, GET /api/v1/health answers 200, the
#                                 embedded dashboard is served and the server stops.
#                                 Needs only bash and curl (runs on Windows/macOS).
#   roundtrip binary BINARY VERSION
#   roundtrip image  IMAGE VERSION
#                                 One backup and one safe-clone restore of a seeded
#                                 database against a disposable MongoDB (single-node
#                                 replica set) and MinIO, through the REST API, with
#                                 either a binary (linux, with the MongoDB Database
#                                 Tools on PATH) or a container image.
#   upgrade NEW_BINARY [VERSION]  Upgrade test with real previous releases: for each
#                                 of the last UPGRADE_COUNT stable releases (or the
#                                 tags in UPGRADE_FROM), download its linux archive,
#                                 verify its checksum, start it on a fresh data
#                                 directory and create a storage target, encryption,
#                                 a connection, a scheduled job, a backup and a PITR
#                                 stream; stop it, start NEW_BINARY on the same data
#                                 directory and assert that the old backup restores
#                                 into a safe clone, stored secrets still decrypt
#                                 (connection and storage target tests, the
#                                 encrypted archive), the API key and the password
#                                 still work, the schedule keeps running and a new
#                                 backup works.
#   install-tools                 Installs the MongoDB Database Tools (Linux x86_64,
#                                 apt) for the binary modes.
#   deb PACKAGE VERSION           Installs a release .deb on Debian/Ubuntu (root or sudo):
#                                 --version, the mongorescue user, /var/lib/mongorescue,
#                                 the 0600 /etc/default/mongorescue, systemd-analyze
#                                 verify and systemctl cat of the unit, the Recommends
#                                 of the Database Tools, a reinstall, a start of the
#                                 service (when systemd runs) as the mongorescue user
#                                 with health 200 and the dashboard, and a remove that
#                                 keeps the data.
#   rpm PACKAGE VERSION           The same for a release .rpm, inside a fedora
#                                 container (docker; RPM_IMAGE overrides the image),
#                                 without starting the service.
#   manifests DIR VERSION         Checks the generated package manifests next to the
#                                 release archives in DIR: the Scoop JSON (version, URL
#                                 and hash of both architectures), the winget installer
#                                 manifest and the Homebrew cask (hashes of the
#                                 archives; the cask is also parsed with ruby -c).
#
# Prerequisites of roundtrip and upgrade: docker, curl (7.75+ for --aws-sigv4), jq,
# and mongodump/mongorestore (100.12+) on PATH for the binary modes; upgrade also
# needs the gh CLI (authenticated, or GH_TOKEN) unless the archives are cached.
#
# Environment knobs:
#   MONGO_IMAGE        MongoDB image (default: mongo:8.0)
#   MINIO_IMAGE        MinIO image (default: the digest-pinned image of the e2e suite)
#   UPGRADE_COUNT      how many previous releases to upgrade from (default: 2)
#   UPGRADE_FROM       space-separated release tags instead of the latest ones
#   UPGRADE_CACHE      directory that keeps downloaded archives (default: temporary)
#   UPGRADE_PITR       0 skips the PITR stream (default: 1)
#   UPGRADE_SCHEDULE_WAIT  seconds to wait for a scheduled run after the upgrade
#                      (default: 150)
#   MONGO_TOOLS_VERSION  Database Tools version of install-tools (default: 100.12.2)
#   GITHUB_REPOSITORY  repository whose releases are downloaded
#                      (default: yigitcittan/mongorescue)
# ==============================================================================
set -euo pipefail

MONGO_IMAGE="${MONGO_IMAGE:-mongo:8.0}"
# Same digest-pinned image as the integration and e2e suites.
MINIO_IMAGE="${MINIO_IMAGE:-cgr.dev/chainguard/minio@sha256:bd014394a80898e68c149f2311fdf8d5a2c2f3bb2c33b9327ae6d02b4b065ae1}"
REPO="${GITHUB_REPOSITORY:-yigitcittan/mongorescue}"
UPGRADE_COUNT="${UPGRADE_COUNT:-2}"
UPGRADE_PITR="${UPGRADE_PITR:-1}"
UPGRADE_SCHEDULE_WAIT="${UPGRADE_SCHEDULE_WAIT:-150}"
BUCKET="mongorescue-release-verify"
SEED_DB="verify_shop"
SEED_DOCS=50
# Published ports of the disposable services and the address they are reached at;
# only a script running inside a container needs to change them (docs/testing.md).
BIND="${VERIFY_BIND:-127.0.0.1}"
ADDR="${VERIFY_ADDR:-127.0.0.1}"

PREFIX="mr-verify-$$"
WORK=""
NET=""
CONTAINERS=()
PIDS=()
SERVER_LOGS=()
CACHE=""

log() { printf '==> %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }
rand() { od -An -N"$1" -tx1 /dev/urandom | tr -d ' \n'; }

cleanup() {
  local status=$?
  if [ "$status" -ne 0 ]; then
    local f c
    for f in "${SERVER_LOGS[@]:-}"; do
      [ -n "$f" ] && [ -f "$f" ] || continue
      echo "---- server log: $f ----"
      tail -n 40 "$f" 2>/dev/null | sed -e "s/${MONGO_PW:-@@none@@}/******/g" || true
    done
    for c in "${CONTAINERS[@]:-}"; do
      [ -n "$c" ] || continue
      echo "---- container logs: $c ----"
      docker logs --tail 30 "$c" 2>&1 | sed -e "s/${MONGO_PW:-@@none@@}/******/g" || true
    done
  fi
  local p
  for p in "${PIDS[@]:-}"; do
    if [ -n "$p" ]; then kill "$p" 2>/dev/null || true; fi
  done
  for c in "${CONTAINERS[@]:-}"; do
    if [ -n "$c" ]; then docker rm -f -v "$c" >/dev/null 2>&1 || true; fi
  done
  if [ -n "$NET" ]; then docker network rm "$NET" >/dev/null 2>&1 || true; fi
  if [ -n "$WORK" ]; then rm -rf "$WORK"; fi
  exit "$status"
}
trap cleanup EXIT

need() {
  local bin
  for bin in "$@"; do
    command -v "$bin" >/dev/null 2>&1 || fail "missing prerequisite: $bin"
  done
}

is_windows() { case "$(uname -s)" in MINGW* | MSYS* | CYGWIN*) return 0 ;; *) return 1 ;; esac; }

# native_path PATH converts a path for a native Windows program (git bash).
native_path() {
  if is_windows && command -v cygpath >/dev/null 2>&1; then cygpath -m "$1"; else printf '%s' "$1"; fi
}

# free_port prints a loopback TCP port nothing listens on.
free_port() {
  local p
  for _ in $(seq 1 50); do
    p=$((20000 + RANDOM % 40000))
    if ! (echo >"/dev/tcp/127.0.0.1/$p") 2>/dev/null; then echo "$p"; return 0; fi
  done
  fail "no free port found"
}

wait_for() {
  local what=$1 tries=$2; shift 2
  for _ in $(seq 1 "$tries"); do
    if "$@" >/dev/null 2>&1; then log "$what ready"; return 0; fi
    sleep 1
  done
  fail "timed out waiting for $what"
}

# ------------------------------------------------------------------------------
# Server processes: a binary on the host or a container, one at a time.
# ------------------------------------------------------------------------------
BASE=""         # http://127.0.0.1:<port>
SERVER_PID=""   # binary mode
SERVER_CONT=""  # image mode
SERVER_LOG=""   # binary mode: the log file

health_ok() { [ "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/v1/health")" = 200 ]; }

# start_binary BINARY DATADIR [extra flags...] starts the server and waits for health.
# A port that cannot be bound (taken meanwhile, or reserved by Windows) makes the
# server exit: it is retried on another port.
start_binary() {
  local bin=$1 data=$2; shift 2
  local port attempt up
  for attempt in 1 2 3 4 5; do
    port="$(free_port)"
    SERVER_LOG="$WORK/server-$(rand 3).log"
    SERVER_LOGS+=("$SERVER_LOG")
    "$bin" --data-dir "$(native_path "$data")" --host 127.0.0.1 --port "$port" "$@" >"$SERVER_LOG" 2>&1 &
    SERVER_PID=$!
    PIDS+=("$SERVER_PID")
    BASE="http://127.0.0.1:$port"
    up=0
    for _ in $(seq 1 60); do
      kill -0 "$SERVER_PID" 2>/dev/null || break
      if health_ok; then up=1; break; fi
      sleep 1
    done
    if [ "$up" = 1 ]; then return 0; fi
    if kill -0 "$SERVER_PID" 2>/dev/null; then
      kill -KILL "$SERVER_PID" 2>/dev/null || true
      fail "$bin did not become healthy within 60s"
    fi
    wait "$SERVER_PID" 2>/dev/null || true
    SERVER_PID=""
    grep -q 'bind:' "$SERVER_LOG" || fail "$bin exited during start-up"
    log "port $port could not be bound (attempt $attempt), retrying"
  done
  fail "no port could be bound for $bin"
}

# stop_binary stops the server with SIGTERM and, outside Windows, expects a clean
# shutdown line in its log.
stop_binary() {
  [ -n "$SERVER_PID" ] || return 0
  kill -TERM "$SERVER_PID" 2>/dev/null || true
  for _ in $(seq 1 30); do
    kill -0 "$SERVER_PID" 2>/dev/null || break
    sleep 1
  done
  if kill -0 "$SERVER_PID" 2>/dev/null; then
    kill -KILL "$SERVER_PID" 2>/dev/null || true
    fail "the server ignored SIGTERM for 30s"
  fi
  wait "$SERVER_PID" 2>/dev/null || true
  if ! is_windows && ! grep -q 'shutdown complete' "$SERVER_LOG"; then
    fail "no clean shutdown in the server log"
  fi
  SERVER_PID=""
}

start_image() {
  local image=$1
  SERVER_CONT="$PREFIX-app-$(rand 3)"
  docker run -d --name "$SERVER_CONT" --network "$NET" -p "$BIND::8080" "$image" >/dev/null
  CONTAINERS+=("$SERVER_CONT")
  BASE="http://$ADDR:$(docker port "$SERVER_CONT" 8080 | head -n1 | sed 's/.*://')"
  for _ in $(seq 1 60); do
    health_ok && return 0
    sleep 1
  done
  fail "the image did not become healthy within 60s"
}

stop_image() {
  [ -n "$SERVER_CONT" ] || return 0
  docker stop -t 30 "$SERVER_CONT" >/dev/null
  docker logs "$SERVER_CONT" 2>&1 | grep -q 'shutdown complete' || fail "no clean shutdown in the container log"
  SERVER_CONT=""
}

server_log_text() {
  if [ -n "$SERVER_CONT" ]; then docker logs "$SERVER_CONT" 2>&1; else cat "$SERVER_LOG"; fi
}

# ------------------------------------------------------------------------------
# Disposable MongoDB (single-node replica set) and MinIO.
# ------------------------------------------------------------------------------
MONGO_PW=""
MONGO_CONT=""
MINIO_CONT=""
SERVER_MONGO_URI=""  # how the server reaches MongoDB
SERVER_S3_ENDPOINT="" # how the server reaches MinIO

mongo_eval() {
  docker exec "$MONGO_CONT" mongosh --quiet -u root -p "$MONGO_PW" --authenticationDatabase admin --eval "$1"
}

start_services() {
  # $1 = container: servers run in containers on NET and use the aliases; anything
  # else: servers run on the host and use the published loopback ports.
  local mode=$1
  MONGO_PW="vfy$(rand 8)"
  NET="$PREFIX-net"
  docker network create "$NET" >/dev/null
  MONGO_CONT="$PREFIX-mongo"
  MINIO_CONT="$PREFIX-minio"

  log "starting $MONGO_IMAGE (single-node replica set)"
  # A replica set with authentication needs a keyfile owned by the mongodb user; the
  # entrypoint drops --replSet while it creates the root user. GLIBC_TUNABLES: MongoDB
  # 8.0 refuses to start on Linux 6.19+ when tcmalloc uses rseq (SERVER-121912).
  # mongod logs synchronously, so a slow log driver would stall it: non-blocking mode.
  # shellcheck disable=SC2016 # expanded inside the container
  docker run -d --name "$MONGO_CONT" --network "$NET" --network-alias mongo -p "$BIND::27017" \
    --log-opt mode=non-blocking --log-opt max-buffer-size=8m -e GLIBC_TUNABLES=glibc.pthread.rseq=1 \
    -e MONGO_INITDB_ROOT_USERNAME=root -e MONGO_INITDB_ROOT_PASSWORD="$MONGO_PW" \
    --entrypoint bash "$MONGO_IMAGE" -c '
      set -e
      head -c 756 /dev/urandom | base64 -w0 > /tmp/rs.key
      chmod 400 /tmp/rs.key
      chown mongodb:mongodb /tmp/rs.key
      exec docker-entrypoint.sh mongod --replSet rs0 --keyFile /tmp/rs.key --bind_ip_all' >/dev/null
  CONTAINERS+=("$MONGO_CONT")

  log "starting MinIO"
  # Root: the image's nonroot user cannot write the fresh /data volume on some Docker
  # setups (Docker Desktop: "file access denied"); the container is disposable.
  docker run -d --name "$MINIO_CONT" --user 0:0 --network "$NET" --network-alias minio -p "$BIND::9000" \
    -e MINIO_ROOT_USER=minioadmin -e MINIO_ROOT_PASSWORD=minioadmin \
    "$MINIO_IMAGE" server /data >/dev/null
  CONTAINERS+=("$MINIO_CONT")

  # The entrypoint first runs a temporary localhost-only mongod to create the root
  # user, then restarts it: pings count only after the init finished. The logs are
  # captured before matching (grep -q under pipefail would SIGPIPE docker logs).
  mongo_ready() {
    local logs
    logs=$(docker logs "$MONGO_CONT" 2>&1 || true)
    [[ $logs == *"MongoDB init process complete"* ]] || return 1
    mongo_eval 'quit(db.runCommand({ping: 1}).ok ? 0 : 1)'
  }
  wait_for "mongodb" 180 mongo_ready
  mongo_eval 'rs.initiate({_id: "rs0", members: [{_id: 0, host: "127.0.0.1:27017"}]})' >/dev/null
  primary_ready() { mongo_eval 'quit(db.hello().isWritablePrimary ? 0 : 1)'; }
  wait_for "the replica set primary" 60 primary_ready

  mongo_eval "
    const d = db.getSiblingDB('$SEED_DB');
    d.orders.insertMany(Array.from({length: $SEED_DOCS}, (_, i) => ({n: i, item: 'item-' + i})));
    d.orders.createIndex({item: 1});" >/dev/null

  local mport minio_port
  mport="$(docker port "$MONGO_CONT" 27017 | head -n1 | sed 's/.*://')"
  minio_port="$(docker port "$MINIO_CONT" 9000 | head -n1 | sed 's/.*://')"
  # The member calls itself 127.0.0.1:27017, so clients must not follow the topology.
  if [ "$mode" = container ]; then
    SERVER_MONGO_URI="mongodb://root:${MONGO_PW}@mongo:27017/?authSource=admin&directConnection=true"
    SERVER_S3_ENDPOINT="http://minio:9000"
  else
    SERVER_MONGO_URI="mongodb://root:${MONGO_PW}@${ADDR}:${mport}/?authSource=admin&directConnection=true"
    SERVER_S3_ENDPOINT="http://${ADDR}:${minio_port}"
  fi
  wait_for "minio" 60 curl -fsS "http://${ADDR}:${minio_port}/minio/health/live"
  # The storage target form does not create buckets: create it with a signed PUT.
  curl -fsS -X PUT --aws-sigv4 "aws:amz:us-east-1:s3" --user minioadmin:minioadmin \
    "http://${ADDR}:${minio_port}/$BUCKET" >/dev/null
}

# ------------------------------------------------------------------------------
# REST API helpers (an admin API key after bootstrap).
# ------------------------------------------------------------------------------
API_KEY=""

# api METHOD PATH [JSON] prints the response body; a non-2xx answer is an error. The
# status is kept in $WORK/last_code for callers that accept some failures.
api() {
  local method=$1 path=$2 body=${3:-} code
  local args=(-sS -o "$WORK/resp.json" -w '%{http_code}' -X "$method" -H "Authorization: Bearer $API_KEY")
  if [ -n "$body" ]; then args+=(-H 'Content-Type: application/json' -d "$body"); fi
  code=$(curl "${args[@]}" "$BASE$path") || return 1
  echo "$code" >"$WORK/last_code"
  case "$code" in
    2??) cat "$WORK/resp.json" ;;
    *) echo "API $method $path -> $code: $(head -c 400 "$WORK/resp.json")" >&2; return 1 ;;
  esac
}
last_code() { cat "$WORK/last_code"; }

# bootstrap_admin does the first-run setup with the code from the server log, signs
# in, and creates an admin API key (API_KEY) for the rest of the run.
bootstrap_admin() {
  local pw=$1 code="" jar csrf status body
  jar="$WORK/jar-$(rand 3)"
  for _ in $(seq 1 15); do
    code=$(server_log_text | sed -n 's/.*setup_code=\([A-Z2-7-]*\).*/\1/p' | tail -n1)
    [ -n "$code" ] && break
    sleep 1
  done
  [ -n "$code" ] || fail "setup code not found in the server log"
  body=$(jq -nc --arg c "$code" --arg p "$pw" '{setup_code: $c, username: "admin", password: $p}')
  status=$(curl -s -o "$WORK/setup.json" -w '%{http_code}' -H 'Content-Type: application/json' -d "$body" "$BASE/api/v1/setup")
  [ "$status" = 201 ] || fail "setup returned $status: $(head -c 300 "$WORK/setup.json")"
  login_admin "$pw" "$jar"
  csrf=$(jq -r '.data.csrf_token // .csrf_token' "$WORK/login.json")
  [ -n "$csrf" ] && [ "$csrf" != null ] || fail "login returned no CSRF token"
  # Older releases may not know scopes (their keys are admin keys): retry without.
  status=$(curl -s -o "$WORK/key.json" -w '%{http_code}' -b "$jar" -H 'Content-Type: application/json' \
    -H "X-CSRF-Token: $csrf" -d '{"name":"release-verify","scope":"admin"}' "$BASE/api/v1/api-keys")
  if [ "$status" = 400 ]; then
    status=$(curl -s -o "$WORK/key.json" -w '%{http_code}' -b "$jar" -H 'Content-Type: application/json' \
      -H "X-CSRF-Token: $csrf" -d '{"name":"release-verify"}' "$BASE/api/v1/api-keys")
  fi
  [ "$status" = 201 ] || fail "creating the API key returned $status: $(head -c 300 "$WORK/key.json")"
  API_KEY=$(jq -r '.data.key // .key' "$WORK/key.json")
  [ -n "$API_KEY" ] && [ "$API_KEY" != null ] || fail "no API key in the response"
}

# login_admin PASSWORD JAR signs in; the response is kept in $WORK/login.json.
login_admin() {
  local status
  status=$(curl -s -o "$WORK/login.json" -w '%{http_code}' -c "$2" -H 'Content-Type: application/json' \
    -d "$(jq -nc --arg p "$1" '{username: "admin", password: $p}')" "$BASE/api/v1/auth/login")
  [ "$status" = 200 ] || fail "login returned $status"
}

# configure_basics creates the MinIO storage target (default), the connection and,
# with $1 = encrypt, age encryption. Sets TARGET_ID and CONN_ID.
TARGET_ID="" CONN_ID=""
configure_basics() {
  local body key
  body=$(jq -nc --arg e "$SERVER_S3_ENDPOINT" --arg b "$BUCKET" \
    '{name: "minio", type: "s3", s3: {endpoint: $e, region: "us-east-1", bucket: $b, prefix: "verify",
      access_key_id: "minioadmin", secret_access_key: "minioadmin", use_path_style: true}}')
  TARGET_ID=$(api POST /api/v1/storage-targets "$body" | jq -r '.data.id // .id')
  api POST "/api/v1/storage-targets/$TARGET_ID/default" >/dev/null
  [ "$(api POST "/api/v1/storage-targets/$TARGET_ID/test" | jq -r '.data.ok // .ok')" = true ] \
    || fail "the MinIO storage target test failed"

  if [ "${1:-}" = encrypt ]; then
    key=$(api POST /api/v1/settings/encryption/generate-key)
    body=$(jq -nc --argjson k "$key" '($k.data // $k) as $d
      | {encryption: {enabled: true, mode: "x25519", recipients: [$d.recipient], identity: $d.identity}}')
    api PUT /api/v1/settings "$body" >/dev/null
  fi

  body=$(jq -nc --arg u "$SERVER_MONGO_URI" '{name: "verify-mongo", uri: $u}')
  CONN_ID=$(api POST /api/v1/connections "$body" | jq -r '.data.id // .id')
  [ "$(api POST "/api/v1/connections/$CONN_ID/test" | jq -r '.data.ok // .ok')" = true ] \
    || fail "the MongoDB connection test failed"
}

# wait_backup ID waits until the backup is completed.
wait_backup() {
  local id=$1 st=""
  for _ in $(seq 1 120); do
    st=$(api GET "/api/v1/backups?id=$id" | jq -r '.data[0].status // empty')
    case "$st" in
      completed) return 0 ;;
      failed | cancelled) fail "backup $id ended $st: $(api GET "/api/v1/backups/$id/log?tail=20" 2>&1 | head -c 600)" ;;
    esac
    sleep 2
  done
  fail "backup $id did not complete (last status: ${st:-unknown})"
}

# backup_now prints the ID of a completed backup of the seeded database.
backup_now() {
  local body id
  body=$(jq -nc --arg c "$CONN_ID" --arg t "$TARGET_ID" --arg d "$SEED_DB" \
    '{connection_id: $c, database: $d, storage_target_id: $t, gzip: true}')
  id=$(api POST /api/v1/backups "$body" | jq -r '.data.id // .id')
  wait_backup "$id"
  echo "$id"
}

# restore_clone BACKUPID restores into a safe clone (the default), checks the clone's
# document count and index, and drops it.
restore_clone() {
  local backup=$1 rid st="" clone count
  rid=$(api POST /api/v1/restore "$(jq -nc --arg b "$backup" '{backup_id: $b}')" | jq -r '.data.id // .id')
  for _ in $(seq 1 120); do
    st=$(api GET "/api/v1/restores?id=$rid" | jq -r '.data[0].status // empty')
    case "$st" in
      completed) break ;;
      failed | cancelled) fail "restore $rid ended $st: $(api GET "/api/v1/restores/$rid/log?tail=20" 2>&1 | head -c 600)" ;;
    esac
    sleep 2
  done
  [ "$st" = completed ] || fail "restore $rid did not complete (last status: ${st:-unknown})"
  clone=$(api GET "/api/v1/restores?id=$rid" | jq -r '.data[0].target_database')
  [[ $clone == "${SEED_DB}_rescue_"* ]] || fail "restore $rid went to '$clone', not a safe clone of $SEED_DB"
  count=$(mongo_eval "print(db.getSiblingDB('$clone').orders.countDocuments())" | tail -n1 | tr -d '\r')
  [ "$count" = "$SEED_DOCS" ] || fail "clone $clone has $count documents, want $SEED_DOCS"
  mongo_eval "print(db.getSiblingDB('$clone').orders.getIndexes().length)" | tail -n1 | grep -q '^2' \
    || fail "clone $clone lost the index on item"
  mongo_eval "db.getSiblingDB('$clone').dropDatabase()" >/dev/null
  log "restore $rid -> $clone: $count documents and the index are back"
}

no_secret_leak() {
  local logs
  logs=$(server_log_text)
  for s in "$MONGO_PW" "$API_KEY" "minioadmin"; do
    if [ -n "$s" ] && printf '%s' "$logs" | grep -qF "$s"; then fail "a secret leaked into the server log"; fi
  done
}

# ------------------------------------------------------------------------------
# archive
# ------------------------------------------------------------------------------
cmd_archive() {
  [ $# -eq 2 ] || fail "usage: archive BINARY VERSION"
  need curl
  local bin=$1 version=${2#v} out data
  WORK="$(mktemp -d)"
  out=$("$bin" --version)
  log "$out"
  [[ $out == "MongoRescue v${version} (commit: "* ]] || fail "--version reports '$out', want version $version"
  [[ $out != *"commit: unknown"* && $out != *"commit: )"* ]] || fail "--version has no commit: a broken ldflag"
  [[ $out != *"built: unknown"* ]] || fail "--version has no build date: a broken ldflag"

  data="$WORK/data"
  mkdir -p "$data"
  start_binary "$bin" "$data" --dashboard
  curl -fsS "$BASE/api/v1/health" >"$WORK/health.json"
  grep -q '"status":"healthy"' "$WORK/health.json" || fail "health is not healthy: $(cat "$WORK/health.json")"
  grep -q "\"version\":\"${version}\"" "$WORK/health.json" || fail "health reports another version: $(cat "$WORK/health.json")"
  log "GET /api/v1/health -> 200, version $version"
  curl -fsS "$BASE/api/v1/setup/status" | grep -q '"setup_required":true' || fail "a fresh data directory is not in setup mode"
  curl -fsS "$BASE/" | grep -q 'app.js' || fail "/ does not serve the embedded dashboard"
  log "GET / -> the embedded dashboard"
  [ -s "$data/mongorescue.db" ] || [ -n "$(ls "$data" 2>/dev/null)" ] || fail "the server left the data directory empty"
  stop_binary
  log "archive smoke test passed"
}

# ------------------------------------------------------------------------------
# roundtrip
# ------------------------------------------------------------------------------
cmd_roundtrip() {
  [ $# -eq 3 ] || fail "usage: roundtrip binary|image TARGET VERSION"
  need docker curl jq
  local kind=$1 target=$2 version=${3#v} pw backup out
  WORK="$(mktemp -d)"
  pw="pw-$(rand 8)"
  case "$kind" in
    binary)
      need mongodump mongorestore
      out=$("$target" --version)
      [[ $out == "MongoRescue v${version} "* ]] || fail "--version reports '$out', want version $version"
      start_services host
      mkdir -p "$WORK/data" "$WORK/backups"
      start_binary "$target" "$WORK/data" --dashboard
      ;;
    image)
      out=$(docker run --rm "$target" --version)
      [[ $out == "MongoRescue v${version} "* ]] || fail "image --version reports '$out', want version $version"
      start_services container
      start_image "$target"
      ;;
    *) fail "unknown roundtrip kind: $kind (binary or image)" ;;
  esac
  log "$kind: $out"
  bootstrap_admin "$pw"
  configure_basics encrypt
  backup=$(backup_now)
  log "backup $backup completed (encrypted, gzip, MinIO)"
  restore_clone "$backup"
  no_secret_leak
  if [ "$kind" = binary ]; then stop_binary; else stop_image; fi
  log "roundtrip ($kind) passed"
}

# ------------------------------------------------------------------------------
# upgrade
# ------------------------------------------------------------------------------
host_arch() {
  case "$(uname -m)" in
    x86_64 | amd64) echo amd64 ;;
    aarch64 | arm64) echo arm64 ;;
    *) fail "unsupported architecture $(uname -m)" ;;
  esac
}

# fetch_release TAG prints the path of the release's binary: downloaded, checked
# against checksums.txt and unpacked into the cache.
fetch_release() {
  local tag=$1 ver=${1#v} arch dir asset
  arch="$(host_arch)"
  asset="mongorescue_${ver}_linux_${arch}.tar.gz"
  dir="$CACHE/$tag"
  mkdir -p "$dir"
  if [ ! -f "$dir/$asset" ] || [ ! -f "$dir/checksums.txt" ]; then
    gh release download "$tag" --repo "$REPO" --dir "$dir" --clobber -p "$asset" -p checksums.txt >&2
  fi
  (cd "$dir" && grep " $asset\$" checksums.txt | sha256sum -c - >&2) || fail "$asset does not match checksums.txt"
  mkdir -p "$dir/unpacked"
  tar -xzf "$dir/$asset" -C "$dir/unpacked" mongorescue
  echo "$dir/unpacked/mongorescue"
}

# previous_releases prints the tags to upgrade from.
previous_releases() {
  if [ -n "${UPGRADE_FROM:-}" ]; then tr ' ' '\n' <<<"$UPGRADE_FROM"; return; fi
  gh release list --repo "$REPO" --exclude-drafts --exclude-pre-releases --limit "$((UPGRADE_COUNT + 1))" \
    --json tagName --jq '.[].tagName'
}

scheduled_count() { # completed backups of the job
  api GET "/api/v1/backups?job_id=$JOB_ID&status=completed" | jq '.data | length'
}

wait_idle() { # no backup is running
  for _ in $(seq 1 90); do
    [ "$(api GET '/api/v1/backups?status=in_progress' | jq '.data | length')" = 0 ] && return 0
    sleep 2
  done
  fail "a backup is still running"
}

JOB_ID="" STREAM_ID="" OLD_BACKUP="" PW=""
cmd_upgrade_one() {
  local tag=$1 newbin=$2 newver=$3 oldbin data before body code t0 out
  oldbin="$(fetch_release "$tag")"
  out=$("$oldbin" --version)
  log "---- upgrade from $tag: $out"
  data="$WORK/data-$tag"
  mkdir -p "$data"
  PW="pw-$(rand 8)"

  # --- the old release writes its state ---
  start_binary "$oldbin" "$data"
  bootstrap_admin "$PW"
  configure_basics encrypt
  body=$(jq -nc --arg c "$CONN_ID" --arg t "$TARGET_ID" --arg d "$SEED_DB" \
    '{name: "verify-job", connection_id: $c, database: $d, cron_expression: "* * * * *", storage_target_id: $t,
      retention_days: 7, retention_count: 20, gzip: true, enabled: true}')
  JOB_ID=$(api POST /api/v1/jobs "$body" | jq -r '.data.id // .id')
  OLD_BACKUP=$(backup_now)
  log "$tag: backup $OLD_BACKUP completed; job $JOB_ID"
  STREAM_ID=""
  if [ "$UPGRADE_PITR" = 1 ]; then
    set +e
    body=$(jq -nc --arg c "$CONN_ID" '{connection_id: $c, chunk_seconds: 15}')
    api POST /api/v1/pitr/streams "$body" >"$WORK/stream.json"
    code=$?
    set -e
    if [ "$code" -eq 0 ]; then
      STREAM_ID=$(jq -r '.data.id // .data.stream.id // .id' "$WORK/stream.json")
      log "$tag: PITR stream $STREAM_ID created"
      sleep 20 # a chunk or two before the upgrade
    else
      log "$tag: no PITR stream (HTTP $(last_code)); skipped"
    fi
  fi
  wait_idle
  before=$(scheduled_count)
  stop_binary

  # --- the new build opens the same data directory ---
  log "starting $newbin on the data directory of $tag"
  t0=$SECONDS
  start_binary "$newbin" "$data" --dashboard
  log "the new build is healthy after $((SECONDS - t0))s"
  curl -fsS "$BASE/api/v1/health" | grep -q "\"version\":\"${newver}\"" || fail "the new build reports another version"

  # The API key and the password survive.
  [ "$(api GET /api/v1/auth/me | jq -r '.data.auth // .auth')" = api_key ] || fail "the old API key no longer works"
  login_admin "$PW" "$WORK/jar-after"
  log "the old API key and the admin password still work"

  # Stored secrets decrypt: the connection URI, the S3 credentials, the age identity.
  [ "$(api POST "/api/v1/connections/$CONN_ID/test" | jq -r '.data.ok // .ok')" = true ] \
    || fail "the stored connection no longer connects (its URI did not decrypt?)"
  [ "$(api POST "/api/v1/storage-targets/$TARGET_ID/test" | jq -r '.data.ok // .ok')" = true ] \
    || fail "the stored storage target no longer works (its credentials did not decrypt?)"
  api GET /api/v1/settings | jq -e '(.data // .) | .encryption.enabled == true' >/dev/null \
    || fail "encryption settings were lost"
  api GET "/api/v1/backups?id=$OLD_BACKUP" | jq -e '.data[0].status == "completed"' >/dev/null \
    || fail "the old backup is no longer completed"
  log "stored secrets decrypt (connection, storage target, encryption)"

  # The old backup (encrypted with the old key material) restores.
  restore_clone "$OLD_BACKUP"

  # Schedules continue.
  api GET "/api/v1/jobs/$JOB_ID" | jq -e '(.data // .) | (.enabled == true) and ((.next_run // "") != "")' >/dev/null \
    || fail "the job lost its schedule"
  log "waiting up to ${UPGRADE_SCHEDULE_WAIT}s for a scheduled run of the old job"
  for _ in $(seq 1 "$((UPGRADE_SCHEDULE_WAIT / 5))"); do
    [ "$(scheduled_count)" -gt "$before" ] && break
    sleep 5
  done
  [ "$(scheduled_count)" -gt "$before" ] || fail "the old job did not run after the upgrade ($before completed backups before)"
  log "the schedule continued: a new backup of the old job completed"

  # PITR stream state survives.
  if [ -n "$STREAM_ID" ]; then
    api GET "/api/v1/pitr/streams/$STREAM_ID" >"$WORK/stream.json" || fail "the PITR stream is gone"
    local status
    status=$(jq -r '.data.state.status // .state.status // empty' "$WORK/stream.json")
    [ "$status" = running ] || fail "the PITR stream is '$status' after the upgrade: $(jq -c '.data.state // .state' "$WORK/stream.json" | head -c 400)"
    log "the PITR stream is running"
  fi

  # And the new build backs up and restores on its own.
  restore_clone "$(backup_now)"
  no_secret_leak
  wait_idle
  stop_binary
  log "upgrade from $tag passed"
}

cmd_upgrade() {
  [ $# -ge 1 ] || fail "usage: upgrade NEW_BINARY [VERSION]"
  need docker curl jq tar sha256sum mongodump mongorestore
  # gh lists and downloads the releases; with UPGRADE_FROM and a filled UPGRADE_CACHE
  # it is not needed.
  if [ -z "${UPGRADE_FROM:-}" ] || [ -z "${UPGRADE_CACHE:-}" ]; then need gh; fi
  [ "$(uname -s)" = Linux ] || fail "the upgrade test runs on Linux"
  local newbin=$1 newver="${2:-}" out tag tags=()
  WORK="$(mktemp -d)"
  CACHE="${UPGRADE_CACHE:-$WORK/releases}"
  mkdir -p "$CACHE"
  out=$("$newbin" --version)
  log "new build: $out"
  if [ -z "$newver" ]; then newver=$(sed -n 's/^MongoRescue v\([^ ]*\) .*/\1/p' <<<"$out"); fi
  while IFS= read -r tag; do
    # The build under test may already be a published release (nightly after a tag).
    if [ -n "$tag" ] && [ "${tag#v}" != "$newver" ] && [ "${#tags[@]}" -lt "$UPGRADE_COUNT" ]; then tags+=("$tag"); fi
  done < <(previous_releases)
  [ "${#tags[@]}" -gt 0 ] || fail "no previous release found"
  log "upgrading from: ${tags[*]}"
  start_services host
  for tag in "${tags[@]}"; do
    cmd_upgrade_one "$tag" "$newbin" "$newver"
  done
  log "all upgrades passed (${tags[*]})"
}

# ------------------------------------------------------------------------------
# install-tools
# ------------------------------------------------------------------------------
cmd_install_tools() {
  local version="${MONGO_TOOLS_VERSION:-100.12.2}" sudo="sudo" d url deb
  [ "$(id -u)" -ne 0 ] || sudo=""
  [ "$(uname -m)" = x86_64 ] || fail "install-tools supports x86_64 only"
  WORK="$(mktemp -d)"
  deb="$WORK/tools.deb"
  # shellcheck disable=SC1091
  . /etc/os-release
  # Older Tools releases have no package for the newest runner image; the binaries
  # are static, so fall back to the Ubuntu 24.04 package.
  for d in "${ID}${VERSION_ID//./}" ubuntu2404; do
    url="https://fastdl.mongodb.org/tools/db/mongodb-database-tools-${d}-x86_64-${version}.deb"
    log "downloading $url"
    if curl -fsSL -o "$deb" "$url"; then break; fi
    [ "$d" != ubuntu2404 ] || fail "no Database Tools package found"
  done
  $sudo apt-get install -y "$deb"
  mongodump --version
}

# ------------------------------------------------------------------------------
# deb, rpm: the native packages
# ------------------------------------------------------------------------------

# check_installed_package VERSION: what the deb and the rpm must both have put in
# place; runs as root on the system the package was installed on.
check_installed_package() {
  local version=${1#v} out unit=/usr/lib/systemd/system/mongorescue.service
  out=$(/usr/bin/mongorescue --version)
  log "$out"
  [[ $out == "MongoRescue v${version} (commit: "* ]] || fail "--version reports '$out', want version $version"
  [[ $out != *"commit: unknown"* ]] || fail "--version has no commit: a broken ldflag"
  id mongorescue >/dev/null || fail "the mongorescue user was not created"
  [ "$(stat -c '%U:%G %a' /var/lib/mongorescue)" = "mongorescue:mongorescue 750" ] ||
    fail "/var/lib/mongorescue is $(stat -c '%U:%G %a' /var/lib/mongorescue), want mongorescue:mongorescue 750"
  [ "$(stat -c '%U %a' /etc/default/mongorescue)" = "root 600" ] || fail "/etc/default/mongorescue is not root 0600"
  [ -f "$unit" ] || fail "the systemd unit is missing"
  grep -q '^User=mongorescue$' "$unit" || fail "the unit does not run as mongorescue"
  # Only deprecation-free, loadable units pass; the binary must exist and be executable.
  systemd-analyze verify "$unit" || fail "systemd-analyze verify rejected the unit"
  log "systemd-analyze verify: the unit is valid"
}

cmd_deb() {
  [ $# -eq 2 ] || fail "usage: deb PACKAGE.deb VERSION   (Debian or Ubuntu, root or sudo, systemd)"
  need curl dpkg
  local pkg version=${2#v} sudo="sudo" rec
  pkg=$(readlink -f "$1")
  [ "$(id -u)" -ne 0 ] || sudo=""
  rec=$(dpkg-deb -f "$pkg" Recommends)
  [[ $rec == *mongodb-database-tools* ]] || fail "the package does not recommend mongodb-database-tools (Recommends: '$rec')"
  [ -z "$(dpkg-deb -f "$pkg" Depends)" ] || log "Depends: $(dpkg-deb -f "$pkg" Depends)"
  $sudo env DEBIAN_FRONTEND=noninteractive apt-get install -y "$pkg"
  $sudo bash -c "$(declare -f log fail check_installed_package); check_installed_package '$version'"
  $sudo systemctl cat mongorescue.service | grep -q '^ExecStart=/usr/bin/mongorescue ' || fail "systemctl cat: ExecStart is not /usr/bin/mongorescue"
  # A second install over the first (the same scripts as an upgrade) is idempotent.
  $sudo env DEBIAN_FRONTEND=noninteractive apt-get install -y --reinstall "$pkg"

  BASE="http://127.0.0.1:8080"
  if [ -d /run/systemd/system ]; then
    $sudo systemctl start mongorescue.service
    wait_for "the packaged service" 60 health_ok
    curl -fsS "$BASE/" | grep -q 'app.js' || fail "/ does not serve the dashboard (MONGORESCUE_DASHBOARD in /etc/default/mongorescue)"
    local pid
    pid=$($sudo systemctl show -p MainPID --value mongorescue.service)
    [ "$(ps -o user= -p "$pid")" = mongorescue ] || fail "the service runs as $(ps -o user= -p "$pid"), want mongorescue"
    [ -s /var/lib/mongorescue/data/mongorescue.db ] || fail "no database in /var/lib/mongorescue/data"
    log "the service runs as mongorescue under the hardened unit: health 200, dashboard served"
    $sudo systemctl stop mongorescue.service
  else
    log "systemd is not running here: the service start check is skipped"
  fi

  $sudo env DEBIAN_FRONTEND=noninteractive apt-get remove -y mongorescue
  [ ! -e /usr/bin/mongorescue ] || fail "remove left the binary"
  [ ! -e /usr/lib/systemd/system/mongorescue.service ] || fail "remove left the unit"
  [ -d /var/lib/mongorescue ] || fail "remove deleted the data directory"
  log "deb verification passed (version $version)"
}

cmd_rpm() {
  [ $# -eq 2 ] || fail "usage: rpm PACKAGE.rpm VERSION   (needs docker; installs it in a fedora container)"
  need docker
  local pkg version=${2#v} image="${RPM_IMAGE:-fedora:latest}"
  pkg=$(readlink -f "$1")
  log "installing $(basename "$pkg") in $image"
  docker run --rm -i -e VERSION="$version" -v "$(native_path "$pkg"):/pkg/mongorescue.rpm:ro" "$image" bash -s <<'EOS'
set -euo pipefail
fail() { echo "FAIL: $*" >&2; exit 1; }
log() { printf '==> %s\n' "$*"; }
dnf install -y systemd >/dev/null
rec=$(rpm -qp --recommends /pkg/mongorescue.rpm)
case "$rec" in *mongodb-database-tools*) ;; *) fail "the package does not recommend mongodb-database-tools ($rec)" ;; esac
dnf install -y --setopt=install_weak_deps=False /pkg/mongorescue.rpm
out=$(/usr/bin/mongorescue --version)
log "$out"
case "$out" in "MongoRescue v${VERSION} (commit: "*) ;; *) fail "--version reports '$out', want $VERSION" ;; esac
case "$out" in *"commit: unknown"*) fail "--version has no commit" ;; esac
id mongorescue >/dev/null || fail "the mongorescue user was not created"
[ "$(stat -c '%U:%G %a' /var/lib/mongorescue)" = "mongorescue:mongorescue 750" ] || fail "/var/lib/mongorescue has the wrong owner or mode"
[ "$(stat -c '%U %a' /etc/default/mongorescue)" = "root 600" ] || fail "/etc/default/mongorescue is not root 0600"
rpm -qc mongorescue | grep -qx /etc/default/mongorescue || fail "/etc/default/mongorescue is not a config file"
rpm -qp --queryformat '[%{FILEFLAGS:fflags} %{FILENAMES}\n]' /pkg/mongorescue.rpm | grep ' /etc/default/mongorescue$' | grep -q n || fail "/etc/default/mongorescue is not noreplace"
unit=/usr/lib/systemd/system/mongorescue.service
grep -q '^User=mongorescue$' "$unit" || fail "the unit does not run as mongorescue"
systemd-analyze verify "$unit" || fail "systemd-analyze verify rejected the unit"
log "systemd-analyze verify: the unit is valid"
# Reinstall: the scripts are idempotent.
dnf reinstall -y /pkg/mongorescue.rpm >/dev/null
dnf remove -y mongorescue >/dev/null
[ ! -e /usr/bin/mongorescue ] || fail "erase left the binary"
[ -d /var/lib/mongorescue ] || fail "erase deleted the data directory"
log "rpm verification passed (version ${VERSION})"
EOS
}

# ------------------------------------------------------------------------------
# manifests: Scoop, winget and Homebrew
# ------------------------------------------------------------------------------

# sha_of FILE prints the SHA-256 of FILE.
sha_of() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }

cmd_manifests() {
  [ $# -eq 2 ] || fail "usage: manifests DIR VERSION   (DIR holds the archives and the generated manifests)"
  need jq
  local dir=$1 version=${2#v} key url file want got n=0
  # Scoop: valid JSON, the version, and the hash of every architecture's archive.
  [ -s "$dir/mongorescue.scoop.json" ] || fail "mongorescue.scoop.json is missing"
  jq -e . "$dir/mongorescue.scoop.json" >/dev/null || fail "the Scoop manifest is not valid JSON"
  [ "$(jq -r .version "$dir/mongorescue.scoop.json")" = "$version" ] || fail "the Scoop manifest has another version"
  for key in 64bit arm64; do
    url=$(jq -r ".architecture[\"$key\"].url" "$dir/mongorescue.scoop.json")
    want=$(jq -r ".architecture[\"$key\"].hash" "$dir/mongorescue.scoop.json")
    jq -e ".architecture[\"$key\"].bin | index(\"mongorescue.exe\")" "$dir/mongorescue.scoop.json" >/dev/null || fail "the Scoop $key entry does not expose mongorescue.exe"
    file="$dir/$(basename "$url")"
    [[ $url == https://github.com/*/releases/download/v${version}/mongorescue_${version}_windows_* ]] || fail "unexpected Scoop URL $url"
    [ -f "$file" ] || fail "no downloaded archive for $url"
    got=$(sha_of "$file")
    [ "$got" = "$want" ] || fail "Scoop $key hash $want does not match $(basename "$file") ($got)"
    log "scoop $key: $(basename "$file") hash matches"
    n=$((n + 1))
  done
  # winget: the installer manifest names the version and the same archives.
  local inst="$dir/YigitCittan.MongoRescue.installer.yaml"
  [ -s "$inst" ] || fail "the winget installer manifest is missing"
  grep -q "^PackageVersion: ${version}\$" "$inst" || fail "the winget installer manifest has another version"
  grep -q '^PackageIdentifier: YigitCittan.MongoRescue$' "$inst" || fail "unexpected winget PackageIdentifier"
  for file in "$dir"/mongorescue_"${version}"_windows_*.zip; do
    got=$(sha_of "$file")
    grep -qi "InstallerSha256: ${got}\$" "$inst" || fail "winget manifest lacks the hash of $(basename "$file")"
    log "winget: $(basename "$file") hash matches"
  done
  [ -s "$dir/YigitCittan.MongoRescue.yaml" ] && [ -s "$dir/YigitCittan.MongoRescue.locale.en-US.yaml" ] || fail "a winget manifest file is missing"
  # Homebrew cask: the version and the hash of every macOS and Linux archive.
  local cask="$dir/mongorescue.cask.rb"
  [ -s "$cask" ] || fail "the Homebrew cask is missing"
  grep -q "version \"${version}\"" "$cask" || fail "the cask has another version"
  for file in "$dir"/mongorescue_"${version}"_darwin_*.tar.gz "$dir"/mongorescue_"${version}"_linux_*.tar.gz; do
    got=$(sha_of "$file")
    grep -q "$got" "$cask" || fail "the cask lacks the hash of $(basename "$file")"
    log "cask: $(basename "$file") hash matches"
  done
  if command -v ruby >/dev/null 2>&1; then ruby -c "$cask" >/dev/null || fail "the cask is not valid Ruby"; fi
  log "manifests verification passed (version $version)"
}

main() {
  local cmd=${1:-}
  [ $# -gt 0 ] && shift
  case "$cmd" in
    archive) cmd_archive "$@" ;;
    roundtrip) cmd_roundtrip "$@" ;;
    upgrade) cmd_upgrade "$@" ;;
    install-tools) cmd_install_tools ;;
    deb) cmd_deb "$@" ;;
    rpm) cmd_rpm "$@" ;;
    manifests) cmd_manifests "$@" ;;
    *) sed -n '3,/^# ====/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//' | head -n 80; exit 2 ;;
  esac
}
main "$@"
