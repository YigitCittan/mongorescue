#!/usr/bin/env bash
# ==============================================================================
# Runs the Playwright browser suite (e2e/) against the real MongoRescue binary and
# disposable containers:
#   - MongoDB (MONGO_IMAGE, default mongo:7) with a random root password, seeded
#     with a small "e2e_shop" database to back up
#   - MinIO as the S3-compatible storage target, with the bucket created up front
# Containers bind to random loopback ports and are always removed on exit. The
# server itself is built here (make build) and started by the suite's globalSetup
# on a random port with a temporary data directory (see e2e/global-setup.ts).
#
# Prerequisites: docker, curl (with --aws-sigv4, 7.75+), Go, Node.js and npm, and
# mongodump/mongorestore (MongoDB Database Tools >= 100.3) on PATH.
#
# Environment knobs:
#   MONGO_IMAGE      MongoDB image (default: mongo:7)
#   E2E_SKIP_INSTALL 1 skips npm ci and the Chromium download (already installed)
#   PLAYWRIGHT_ARGS  extra arguments for playwright test (e.g. "--headed", "-g backup")
#   E2E_KEYCLOAK     1 also starts Keycloak (KEYCLOAK_IMAGE) for the single sign-on
#                    spec, which is skipped without it
# ==============================================================================
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MONGO_IMAGE="${MONGO_IMAGE:-mongo:7}"
# Same digest-pinned image as the integration suite.
MINIO_IMAGE="${MINIO_IMAGE:-cgr.dev/chainguard/minio@sha256:bd014394a80898e68c149f2311fdf8d5a2c2f3bb2c33b9327ae6d02b4b065ae1}"
KEYCLOAK_IMAGE="${KEYCLOAK_IMAGE:-quay.io/keycloak/keycloak:26.3@sha256:357829ec7c4693397533035092ad13b0644bcc95ded311f33a3738c4d9e9bdba}"
MINIO_BUCKET="mongorescue-e2e"

PREFIX="mongorescue-e2e-$$"
MONGO_PW="e2ePw$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"
CONTAINERS=()

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
    if [ -n "$c" ]; then docker rm -f "$c" >/dev/null 2>&1 || true; fi
  done
  exit "$status"
}
trap cleanup EXIT

for bin in docker curl go node npm mongodump mongorestore; do
  command -v "$bin" >/dev/null 2>&1 || { echo "missing prerequisite: $bin" >&2; exit 1; }
done

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

log "building the server"
make -C "$ROOT" build

# --- MongoDB -------------------------------------------------------------------
log "starting $MONGO_IMAGE"
docker run -d --name "$PREFIX-mongo" -p 127.0.0.1::27017 \
  -e MONGO_INITDB_ROOT_USERNAME=root -e MONGO_INITDB_ROOT_PASSWORD="$MONGO_PW" \
  "$MONGO_IMAGE" >/dev/null
CONTAINERS+=("$PREFIX-mongo")

mongo_eval() {
  docker exec "$PREFIX-mongo" mongosh --quiet -u root -p "$MONGO_PW" --authenticationDatabase admin --eval "$1"
}

# The image entrypoint first runs a temporary, localhost-only mongod to create the
# root user and then restarts it; pings are only trusted after the init finished.
# The logs are captured before matching (grep -q under pipefail; see
# test-integration-docker.sh).
mongo_ready() {
  local logs
  logs=$(docker logs "$PREFIX-mongo" 2>&1 || true)
  [[ $logs == *"MongoDB init process complete"* ]] || return 1
  mongo_eval 'quit(db.runCommand({ping: 1}).ok ? 0 : 1)'
}
wait_for "mongodb" 180 mongo_ready
mongo_port="$(host_port "$PREFIX-mongo" 27017)"

log "seeding the e2e_shop database"
mongo_eval '
  const shop = db.getSiblingDB("e2e_shop");
  shop.orders.insertMany(Array.from({length: 50}, (_, i) => ({n: i, item: "item-" + i, qty: i % 7})));
  shop.customers.insertMany([{name: "Ada"}, {name: "Grace"}, {name: "Linus"}]);
  shop.orders.createIndex({item: 1});
' >/dev/null
export MONGORESCUE_E2E_MONGO_URI="mongodb://root:${MONGO_PW}@127.0.0.1:${mongo_port}/?authSource=admin"
export MONGORESCUE_E2E_DATABASE="e2e_shop"

# --- MinIO ---------------------------------------------------------------------
log "starting $MINIO_IMAGE"
docker run -d --name "$PREFIX-minio" -p 127.0.0.1::9000 \
  -e MINIO_ROOT_USER=minioadmin -e MINIO_ROOT_PASSWORD=minioadmin \
  "$MINIO_IMAGE" server /data >/dev/null
CONTAINERS+=("$PREFIX-minio")
MINIO_URL="http://127.0.0.1:$(host_port "$PREFIX-minio" 9000)"
wait_for "minio" 60 curl -fsS "$MINIO_URL/minio/health/live"

# The storage target form does not create buckets: create it with a signed PUT.
log "creating bucket $MINIO_BUCKET"
curl -fsS -X PUT --aws-sigv4 "aws:amz:us-east-1:s3" --user minioadmin:minioadmin \
  "$MINIO_URL/$MINIO_BUCKET" >/dev/null
export MONGORESCUE_E2E_S3_ENDPOINT="$MINIO_URL"
export MONGORESCUE_E2E_S3_BUCKET="$MINIO_BUCKET"
export MONGORESCUE_E2E_S3_ACCESS_KEY="minioadmin"
export MONGORESCUE_E2E_S3_SECRET_KEY="minioadmin"

# --- Keycloak (optional) -------------------------------------------------------
# E2E_KEYCLOAK=1 also starts Keycloak in dev mode with the integration test realm,
# for the single sign-on spec (10-sso.spec.ts), which is skipped otherwise.
if [ "${E2E_KEYCLOAK:-0}" = 1 ]; then
  log "starting $KEYCLOAK_IMAGE"
  docker run -d --name "$PREFIX-keycloak" -p 127.0.0.1::8080 \
    -e KC_BOOTSTRAP_ADMIN_USERNAME=admin -e KC_BOOTSTRAP_ADMIN_PASSWORD="kc$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')" \
    -v "$ROOT/internal/integration/testdata/keycloak-realm.json:/opt/keycloak/data/import/realm.json:ro" \
    "$KEYCLOAK_IMAGE" start-dev --import-realm >/dev/null
  CONTAINERS+=("$PREFIX-keycloak")
  KEYCLOAK_URL="http://127.0.0.1:$(host_port "$PREFIX-keycloak" 8080)"
  wait_for "keycloak" 180 curl -fsS "$KEYCLOAK_URL/realms/mongorescue/.well-known/openid-configuration"
  export MONGORESCUE_E2E_KEYCLOAK_URL="$KEYCLOAK_URL"
fi

# --- Playwright ----------------------------------------------------------------
cd "$ROOT/e2e"
if [ "${E2E_SKIP_INSTALL:-0}" != 1 ]; then
  log "installing the e2e dependencies and Chromium"
  npm ci --no-audit --no-fund
  npx playwright install chromium
fi
log "running the Playwright suite (MongoDB $MONGO_IMAGE, MinIO)"
# shellcheck disable=SC2086 # PLAYWRIGHT_ARGS is intentionally word-split
npx playwright test ${PLAYWRIGHT_ARGS:-}
