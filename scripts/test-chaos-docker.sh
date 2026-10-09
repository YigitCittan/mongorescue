#!/usr/bin/env bash
# ==============================================================================
# Runs the fault-injection suite (internal/chaos, build tag "chaos") or the load
# test (internal/load, build tag "load") against disposable containers:
#   - MongoDB (MONGO_IMAGE, default mongo:8.0) as a single-node replica set with a
#     keyfile and a random root password
#   - MinIO as the S3 storage
#   - Toxiproxy (pinned by digest) in front of both, so the tests can cut, slow
#     down or black-hole the traffic of the MongoRescue process under test
# Containers bind to random loopback ports and are always removed on exit.
#
# The chaos suite also needs a small filesystem for the full-disk scenario: on
# Linux a tmpfs (passwordless sudo, as on CI runners), on macOS a RAM disk. When it
# cannot be created, that scenario is skipped.
#
# Prerequisites: docker, curl, Go, and mongodump/mongorestore (MongoDB Database
# Tools >= 100.12) on PATH.
#
# Environment knobs:
#   SUITE            chaos (default) or load
#   MONGO_IMAGE      MongoDB image (default: mongo:8.0)
#   CHAOS_SMALL_FS_MB  size of the small filesystem in MiB (default: 48)
#   MONGO_CACHE_GB   WiredTiger cache of each load suite member in GiB (default: 1)
#   CHAOS_REPORT_DIR where reports and server logs are written (default: ./chaos-report)
#   GOTESTFLAGS      extra flags for go test (e.g. "-run TestStorageOutage -v")
#   MONGORESCUE_LOAD_*  load test knobs (see docs/testing.md)
# ==============================================================================
set -euo pipefail

SUITE="${SUITE:-chaos}"
MONGO_IMAGE="${MONGO_IMAGE:-mongo:8.0}"
MINIO_IMAGE="${MINIO_IMAGE:-cgr.dev/chainguard/minio@sha256:bd014394a80898e68c149f2311fdf8d5a2c2f3bb2c33b9327ae6d02b4b065ae1}"
TOXIPROXY_IMAGE="${TOXIPROXY_IMAGE:-ghcr.io/shopify/toxiproxy:2.12.0@sha256:9378ed52a28bc50edc1350f936f518f31fa95f0d15917d6eb40b8e376d1a214e}"
CHAOS_SMALL_FS_MB="${CHAOS_SMALL_FS_MB:-48}"
MONGO_CACHE_GB="${MONGO_CACHE_GB:-1}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHAOS_REPORT_DIR="${CHAOS_REPORT_DIR:-$ROOT/chaos-report}"

PREFIX="mongorescue-chaos-$$"
NET="$PREFIX-net"
MONGO_PW="chPw$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"
CONTAINERS=()
SMALL_DIR=""
SMALL_DEV=""

log() { printf '==> %s\n' "$*"; }

cleanup() {
  local status=$?
  if [ "$status" -ne 0 ]; then
    for c in "${CONTAINERS[@]:-}"; do
      [ -n "$c" ] || continue
      echo "---- logs: $c ----"
      docker logs --tail 50 "$c" 2>&1 | sed "s/${MONGO_PW}/******/g" || true
    done
  fi
  for c in "${CONTAINERS[@]:-}"; do
    if [ -n "$c" ]; then docker rm -f -v "$c" >/dev/null 2>&1 || true; fi
  done
  docker network rm "$NET" >/dev/null 2>&1 || true
  if [ -n "$SMALL_DIR" ]; then
    case "$(uname -s)" in
      Linux) sudo -n umount "$SMALL_DIR" >/dev/null 2>&1 || true ;;
      Darwin) [ -z "$SMALL_DEV" ] || hdiutil detach -force "$SMALL_DEV" >/dev/null 2>&1 || true ;;
    esac
    rmdir "$SMALL_DIR" >/dev/null 2>&1 || true
  fi
  exit "$status"
}
trap cleanup EXIT

case "$SUITE" in
  chaos | load) ;;
  *) echo "unknown SUITE: $SUITE (chaos or load)" >&2; exit 1 ;;
esac
for bin in docker curl go mongodump mongorestore; do
  command -v "$bin" >/dev/null 2>&1 || { echo "missing prerequisite: $bin" >&2; exit 1; }
done
mkdir -p "$CHAOS_REPORT_DIR"

host_port() { docker port "$1" "$2" | head -n1 | sed 's/.*://'; }

wait_for() {
  local what=$1 tries=$2; shift 2
  for _ in $(seq 1 "$tries"); do
    if "$@" >/dev/null 2>&1; then log "$what ready"; return 0; fi
    sleep 1
  done
  echo "timed out waiting for $what" >&2
  return 1
}

docker network create "$NET" >/dev/null

# --- MongoDB ------------------------------------------------------------------
# chaos: a single-node replica set, the member known as 127.0.0.1:27017.
# load: a primary and a secondary (priority 0) known by their network aliases, so a
# dump can read from the secondary while the primary's latency is measured.
# Clients outside use directConnection=true, through Toxiproxy or a mapped port.
RS_KEY="$(head -c 756 /dev/urandom | base64 | tr -d '\n')"
# Two members share one machine in the load suite: each gets a bounded cache
# instead of half of the machine's memory.
CACHE_FLAG=""
if [ "$SUITE" = load ]; then CACHE_FLAG="--wiredTigerCacheSizeGB ${MONGO_CACHE_GB}"; fi
# shellcheck disable=SC2016 # expanded inside the container
MONGO_CMD='
  set -e
  printf "%s" "$RS_KEY" > /tmp/rs.key
  chmod 400 /tmp/rs.key
  chown mongodb:mongodb /tmp/rs.key
  exec docker-entrypoint.sh mongod --replSet rs0 --keyFile /tmp/rs.key --bind_ip_all $CACHE_FLAG'
log "starting $MONGO_IMAGE ($SUITE)"
docker run -d --name "$PREFIX-mongo" --network "$NET" --network-alias mongo -p 127.0.0.1::27017 \
  -e MONGO_INITDB_ROOT_USERNAME=root -e MONGO_INITDB_ROOT_PASSWORD="$MONGO_PW" -e RS_KEY="$RS_KEY" -e CACHE_FLAG="$CACHE_FLAG" \
  --entrypoint bash "$MONGO_IMAGE" -c "$MONGO_CMD" >/dev/null
CONTAINERS+=("$PREFIX-mongo")
if [ "$SUITE" = load ]; then
  docker run -d --name "$PREFIX-mongo2" --network "$NET" --network-alias mongo2 -p 127.0.0.1::27017 \
    -e RS_KEY="$RS_KEY" -e CACHE_FLAG="$CACHE_FLAG" --entrypoint bash "$MONGO_IMAGE" -c "$MONGO_CMD" >/dev/null
  CONTAINERS+=("$PREFIX-mongo2")
fi

mongo_eval() {
  docker exec "$PREFIX-mongo" mongosh --quiet -u root -p "$MONGO_PW" --authenticationDatabase admin --eval "$1"
}
mongo_init_done() {
  local logs
  logs=$(docker logs "$PREFIX-mongo" 2>&1 || true)
  [[ $logs == *"MongoDB init process complete"* ]] && mongo_eval 'quit(db.runCommand({ping: 1}).ok ? 0 : 1)'
}
wait_for "mongodb" 180 mongo_init_done
if [ "$SUITE" = load ]; then
  mongo_eval 'rs.initiate({_id: "rs0", members: [{_id: 0, host: "mongo:27017", priority: 2}, {_id: 1, host: "mongo2:27017", priority: 0}]})' >/dev/null
else
  mongo_eval 'rs.initiate({_id: "rs0", members: [{_id: 0, host: "127.0.0.1:27017"}]})' >/dev/null
fi
primary_ready() { mongo_eval 'quit(db.hello().isWritablePrimary ? 0 : 1)'; }
wait_for "replica set primary" 60 primary_ready
if [ "$SUITE" = load ]; then
  secondary_ready() { mongo_eval 'quit(rs.status().members.some(m => m.stateStr === "SECONDARY") ? 0 : 1)'; }
  wait_for "replica set secondary" 120 secondary_ready
  secondary_port="$(host_port "$PREFIX-mongo2" 27017)"
  export MONGORESCUE_LOAD_SECONDARY_URI="mongodb://root:${MONGO_PW}@127.0.0.1:${secondary_port}/?authSource=admin&directConnection=true"
fi
mongo_port="$(host_port "$PREFIX-mongo" 27017)"

# --- MinIO ----------------------------------------------------------------------
log "starting $MINIO_IMAGE"
docker run -d --name "$PREFIX-minio" --network "$NET" --network-alias minio -p 127.0.0.1::9000 \
  -e MINIO_ROOT_USER=minioadmin -e MINIO_ROOT_PASSWORD=minioadmin \
  "$MINIO_IMAGE" server /data >/dev/null
CONTAINERS+=("$PREFIX-minio")
MINIO_URL="http://127.0.0.1:$(host_port "$PREFIX-minio" 9000)"
wait_for "minio" 60 curl -fsS "$MINIO_URL/minio/health/live"

# --- Toxiproxy ------------------------------------------------------------------
log "starting $TOXIPROXY_IMAGE"
docker run -d --name "$PREFIX-toxiproxy" --network "$NET" \
  -p 127.0.0.1::8474 -p 127.0.0.1::21017 -p 127.0.0.1::29000 \
  "$TOXIPROXY_IMAGE" -host 0.0.0.0 >/dev/null
CONTAINERS+=("$PREFIX-toxiproxy")
TOXIPROXY_URL="http://127.0.0.1:$(host_port "$PREFIX-toxiproxy" 8474)"
wait_for "toxiproxy" 60 curl -fsS "$TOXIPROXY_URL/version"
for proxy in "mongo 0.0.0.0:21017 mongo:27017" "minio 0.0.0.0:29000 minio:9000"; do
  read -r name listen upstream <<<"$proxy"
  curl -fsS -X POST "$TOXIPROXY_URL/proxies" \
    -d "{\"name\":\"$name\",\"listen\":\"$listen\",\"upstream\":\"$upstream\",\"enabled\":true}" >/dev/null
done

export MONGORESCUE_CHAOS_TOXIPROXY_URL="$TOXIPROXY_URL"
export MONGORESCUE_CHAOS_MONGO_URI="mongodb://root:${MONGO_PW}@127.0.0.1:${mongo_port}/?authSource=admin&directConnection=true"
mongo_proxy_port="$(host_port "$PREFIX-toxiproxy" 21017)"
minio_proxy_port="$(host_port "$PREFIX-toxiproxy" 29000)"
export MONGORESCUE_CHAOS_MONGO_PROXY_URI="mongodb://root:${MONGO_PW}@127.0.0.1:${mongo_proxy_port}/?authSource=admin&directConnection=true"
export MONGORESCUE_CHAOS_S3_ENDPOINT="$MINIO_URL"
export MONGORESCUE_CHAOS_S3_PROXY_ENDPOINT="http://127.0.0.1:${minio_proxy_port}"
export MONGORESCUE_CHAOS_S3_BUCKET="mongorescue-chaos"
export MONGORESCUE_CHAOS_S3_ACCESS_KEY="minioadmin"
export MONGORESCUE_CHAOS_S3_SECRET_KEY="minioadmin"
export MONGORESCUE_CHAOS_REPORT_DIR="$CHAOS_REPORT_DIR"

# --- A small filesystem for the full-disk scenario --------------------------------
if [ "$SUITE" = chaos ]; then
  SMALL_DIR="$(mktemp -d "${TMPDIR:-/tmp}/mongorescue-chaos-fs.XXXXXX")"
  case "$(uname -s)" in
    Linux)
      if sudo -n mount -t tmpfs -o "size=${CHAOS_SMALL_FS_MB}m,mode=1777" tmpfs "$SMALL_DIR"; then
        export MONGORESCUE_CHAOS_SMALL_DIR="$SMALL_DIR"
      fi
      ;;
    Darwin)
      if SMALL_DEV="$(hdiutil attach -nomount "ram://$((CHAOS_SMALL_FS_MB * 2048))" | awk '{print $1}')" &&
        newfs_hfs -v chaos "$SMALL_DEV" >/dev/null &&
        mount -t hfs -o nobrowse "$SMALL_DEV" "$SMALL_DIR"; then
        export MONGORESCUE_CHAOS_SMALL_DIR="$SMALL_DIR"
      fi
      ;;
  esac
  if [ -n "${MONGORESCUE_CHAOS_SMALL_DIR:-}" ]; then
    log "small filesystem of ${CHAOS_SMALL_FS_MB} MiB at $SMALL_DIR"
  else
    log "no small filesystem: the full-disk scenario is skipped"
  fi
fi

if [ "$SUITE" = chaos ]; then
  log "running the fault-injection suite (MongoDB $MONGO_IMAGE)"
  # shellcheck disable=SC2086 # GOTESTFLAGS is intentionally word-split
  go test -tags=chaos -count=1 -timeout 60m ${GOTESTFLAGS:-} ./internal/chaos/...
else
  log "running the load test (MongoDB $MONGO_IMAGE)"
  # shellcheck disable=SC2086 # GOTESTFLAGS is intentionally word-split
  go test -tags=load -count=1 -timeout 180m ${GOTESTFLAGS:-} ./internal/load/...
fi
