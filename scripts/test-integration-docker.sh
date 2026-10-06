#!/usr/bin/env bash
# ==============================================================================
# Runs the MongoRescue integration suite against disposable containers:
#   - MongoDB (MONGO_IMAGE, default 7) with a random root password (exercises
#     redaction and --config), standalone or a single-node replica set
#   - a second MongoDB with --tlsMode requireTLS and --auth, its certificates
#     signed by a throwaway CA generated in Go (internal/integration/gencerts),
#     for TLS with a custom CA and x509 client certificate authentication
#   - MinIO and LocalStack as S3-compatible emulators
# Containers bind to random loopback ports and are always removed on exit.
#
# The MONGORESCUE_TEST_* variables below only tell the test suite which services
# exist; the MongoRescue server under test is configured through its API and store
# (storage targets, settings, connections), never through environment variables.
#
# Prerequisites: docker, curl, Go, and mongodump/mongorestore (MongoDB Database
# Tools >= 100.3) on PATH.
#
# Environment knobs:
#   MONGO_IMAGE    MongoDB image (default: mongo:7; CI runs 5.0, 6.0, 7.0 and 8.0)
#   MONGO_TOPOLOGY standalone (default) or replset: a single-node replica set with
#                  a keyfile, initiated after start; clients use directConnection
#   IT_PROVIDERS   space-separated services to start (default: "minio localstack keycloak");
#                  "keycloak" also starts Keycloak (KEYCLOAK_IMAGE) in dev mode
#                  with the realm of internal/integration/testdata, for the
#                  single sign-on test (MONGORESCUE_TEST_KEYCLOAK_URL)
#   IT_TLS         1 (default) starts the TLS MongoDB (MONGORESCUE_TEST_TLS_*); 0 skips it
#   IT_RACE        1 (default) runs go test with -race; 0 without (memory limits of
#                  the large-data run only apply without the race detector)
#   GOTESTFLAGS    extra flags for go test (e.g. "-run TestStorageConformance -v")
# ==============================================================================
set -euo pipefail

MONGO_IMAGE="${MONGO_IMAGE:-mongo:7}"
# MinIO no longer publishes public images on Docker Hub/Quay; Chainguard's free
# image is pinned by digest for reproducible CI runs.
MINIO_IMAGE="${MINIO_IMAGE:-cgr.dev/chainguard/minio@sha256:bd014394a80898e68c149f2311fdf8d5a2c2f3bb2c33b9327ae6d02b4b065ae1}"
LOCALSTACK_IMAGE="${LOCALSTACK_IMAGE:-localstack/localstack:4.4}"
KEYCLOAK_IMAGE="${KEYCLOAK_IMAGE:-quay.io/keycloak/keycloak:26.3@sha256:357829ec7c4693397533035092ad13b0644bcc95ded311f33a3738c4d9e9bdba}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IT_PROVIDERS="${IT_PROVIDERS:-minio localstack keycloak}"
MONGO_TOPOLOGY="${MONGO_TOPOLOGY:-standalone}"
IT_RACE="${IT_RACE:-1}"
IT_TLS="${IT_TLS:-1}"
TLS_DIR=""

PREFIX="mongorescue-it-$$"
MONGO_PW="itPw$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"
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
  if [ -n "$TLS_DIR" ]; then rm -rf "$TLS_DIR"; fi
  exit "$status"
}
trap cleanup EXIT

for bin in docker curl go mongodump mongorestore; do
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

# --- MongoDB -------------------------------------------------------------------
log "starting $MONGO_IMAGE ($MONGO_TOPOLOGY)"
case "$MONGO_TOPOLOGY" in
  standalone)
    docker run -d --name "$PREFIX-mongo" -p 127.0.0.1::27017 \
      -e MONGO_INITDB_ROOT_USERNAME=root -e MONGO_INITDB_ROOT_PASSWORD="$MONGO_PW" \
      "$MONGO_IMAGE" >/dev/null
    ;;
  replset)
    # A replica set with authentication needs a keyfile owned by the mongodb user.
    # The entrypoint drops --replSet while it creates the root user.
    # shellcheck disable=SC2016 # expanded inside the container
    docker run -d --name "$PREFIX-mongo" -p 127.0.0.1::27017 \
      -e MONGO_INITDB_ROOT_USERNAME=root -e MONGO_INITDB_ROOT_PASSWORD="$MONGO_PW" \
      --entrypoint bash "$MONGO_IMAGE" -c '
        set -e
        head -c 756 /dev/urandom | base64 -w0 > /tmp/rs.key
        chmod 400 /tmp/rs.key
        chown mongodb:mongodb /tmp/rs.key
        exec docker-entrypoint.sh mongod --replSet rs0 --keyFile /tmp/rs.key --bind_ip_all' >/dev/null
    ;;
  *)
    echo "unknown MONGO_TOPOLOGY: $MONGO_TOPOLOGY (standalone or replset)" >&2
    exit 1
    ;;
esac
CONTAINERS+=("$PREFIX-mongo")

# mongo_ping runs an authenticated ping through mongosh inside the container.
mongo_ping() {
  docker exec "$PREFIX-mongo" mongosh --quiet -u root -p "$MONGO_PW" --authenticationDatabase admin \
    --eval 'quit(db.runCommand({ping: 1}).ok ? 0 : 1)' >/dev/null 2>&1
}

# wait_for_mongo waits until the final mongod answers MONGO_PINGS consecutive
# authenticated pings. The image entrypoint first runs a temporary, localhost-only
# mongod to create the root user and then restarts it, so pings are only trusted
# after "MongoDB init process complete" has been logged.
#
# The logs are captured before matching: `docker logs | grep -q` exits as soon as
# grep matches, docker logs dies of SIGPIPE and, under pipefail, the check fails
# forever once the line is present (the cause of earlier readiness timeouts).
wait_for_mongo() {
  local name="$PREFIX-mongo" timeout=180 pings_needed=3
  local deadline=$((SECONDS + timeout)) next_report=$((SECONDS + 10))
  local init_done=false streak=0 logs running
  while [ "$SECONDS" -lt "$deadline" ]; do
    running=$(docker inspect -f '{{.State.Running}}' "$name" 2>/dev/null || echo false)
    if [ "$running" != "true" ]; then
      echo "mongodb container stopped unexpectedly" >&2
      break
    fi
    if ! $init_done; then
      logs=$(docker logs "$name" 2>&1 || true)
      if [[ $logs == *"MongoDB init process complete"* ]]; then
        init_done=true
        log "mongodb: init finished after ${SECONDS}s, waiting for ${pings_needed} consecutive pings"
      fi
    fi
    if $init_done && mongo_ping; then
      streak=$((streak + 1))
      if [ "$streak" -ge "$pings_needed" ]; then
        log "mongodb ready"
        return 0
      fi
    else
      streak=0
    fi
    if [ "$SECONDS" -ge "$next_report" ]; then
      log "waiting for mongodb (${SECONDS}s elapsed, init done: ${init_done}, consecutive pings: ${streak})"
      next_report=$((SECONDS + 10))
    fi
    sleep 1
  done
  echo "timed out waiting for mongodb after ${timeout}s (init done: ${init_done})" >&2
  echo "---- docker logs: $name ----" >&2
  docker logs "$name" 2>&1 | sed "s/${MONGO_PW}/******/g" >&2 || true
  return 1
}
SECONDS=0
wait_for_mongo
mongo_port="$(host_port "$PREFIX-mongo" 27017)"
MONGO_URI_OPTIONS="authSource=admin"

# mongo_eval runs a script through mongosh inside the container as root.
mongo_eval() {
  docker exec "$PREFIX-mongo" mongosh --quiet -u root -p "$MONGO_PW" --authenticationDatabase admin --eval "$1"
}

if [ "$MONGO_TOPOLOGY" = replset ]; then
  log "initiating the single-node replica set"
  mongo_eval 'rs.initiate({_id: "rs0", members: [{_id: 0, host: "127.0.0.1:27017"}]})' >/dev/null
  primary_ready() { mongo_eval 'quit(db.hello().isWritablePrimary ? 0 : 1)' >/dev/null 2>&1; }
  wait_for "replica set primary" 60 primary_ready
  # The member is known as 127.0.0.1:27017 inside the container; the host reaches it
  # through a mapped port, so clients must not follow the replica set topology.
  MONGO_URI_OPTIONS="${MONGO_URI_OPTIONS}&directConnection=true"
fi
export MONGORESCUE_TEST_MONGO_URI="mongodb://root:${MONGO_PW}@127.0.0.1:${mongo_port}/?${MONGO_URI_OPTIONS}"

# --- MongoDB over TLS (custom CA, x509 client certificates) ---------------------
if [ "$IT_TLS" = 1 ]; then
  TLS_DIR="$(mktemp -d)"
  # mongod runs as another user inside the container; only the client key and its
  # password are private (0600), the server key is a throwaway.
  chmod 755 "$TLS_DIR"
  (cd "$ROOT" && go run -tags integration ./internal/integration/gencerts "$TLS_DIR")
  log "starting $MONGO_IMAGE with --tlsMode requireTLS"
  # Started without the image entrypoint: --auth with no user yet, so the first
  # user is created through the localhost exception below. Clients without a
  # certificate are allowed (password authentication over TLS); x509 clients
  # present theirs.
  docker run -d --name "$PREFIX-mongotls" -p 127.0.0.1::27017 -v "$TLS_DIR:/certs:ro" \
    --entrypoint mongod "$MONGO_IMAGE" --auth --bind_ip_all \
    --tlsMode requireTLS --tlsCertificateKeyFile /certs/server.pem --tlsCAFile /certs/ca.pem \
    --tlsAllowConnectionsWithoutCertificates >/dev/null
  CONTAINERS+=("$PREFIX-mongotls")
  tls_eval() {
    docker exec "$PREFIX-mongotls" mongosh --quiet --tls --tlsCAFile /certs/ca.pem --host localhost --eval "$1"
  }
  tls_ping() { tls_eval 'quit(db.runCommand({ping: 1}).ok ? 0 : 1)' >/dev/null 2>&1; }
  wait_for "mongodb (TLS)" 120 tls_ping
  TLS_PW="tlsPw$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"
  TLS_SUBJECT="$(cat "$TLS_DIR/client.subject")"
  log "creating the TLS root user and the x509 user ${TLS_SUBJECT}"
  tls_eval "
    const admin = db.getSiblingDB('admin');
    admin.createUser({user: 'root', pwd: '${TLS_PW}', roles: ['root']});
    admin.auth('root', '${TLS_PW}');
    db.getSiblingDB('\$external').createUser({user: '${TLS_SUBJECT}', roles: [
      {role: 'backup', db: 'admin'}, {role: 'restore', db: 'admin'}, {role: 'readAnyDatabase', db: 'admin'}]});
  " >/dev/null
  tls_port="$(host_port "$PREFIX-mongotls" 27017)"
  export MONGORESCUE_TEST_TLS_URI="mongodb://root:${TLS_PW}@127.0.0.1:${tls_port}/?tls=true&authSource=admin"
  export MONGORESCUE_TEST_TLS_X509_URI="mongodb://127.0.0.1:${tls_port}/?tls=true&authMechanism=MONGODB-X509"
  export MONGORESCUE_TEST_TLS_DIR="$TLS_DIR"
fi

# --- MinIO ---------------------------------------------------------------------
if [[ " $IT_PROVIDERS " == *" minio "* ]]; then
  log "starting $MINIO_IMAGE"
  docker run -d --name "$PREFIX-minio" -p 127.0.0.1::9000 \
    -e MINIO_ROOT_USER=minioadmin -e MINIO_ROOT_PASSWORD=minioadmin \
    "$MINIO_IMAGE" server /data >/dev/null
  CONTAINERS+=("$PREFIX-minio")
  MINIO_URL="http://127.0.0.1:$(host_port "$PREFIX-minio" 9000)"
  wait_for "minio" 60 curl -fsS "$MINIO_URL/minio/health/live"
  export MONGORESCUE_TEST_S3_MINIO_ENDPOINT="$MINIO_URL"
  export MONGORESCUE_TEST_S3_MINIO_BUCKET="mongorescue-it"
  export MONGORESCUE_TEST_S3_MINIO_ACCESS_KEY="minioadmin"
  export MONGORESCUE_TEST_S3_MINIO_SECRET_KEY="minioadmin"
  export MONGORESCUE_TEST_S3_MINIO_PATH_STYLE="true"
  export MONGORESCUE_TEST_S3_MINIO_CREATE_BUCKET="true"
  # A second bucket, created by the tests with S3 Object Lock enabled (like
  # mc mb --with-lock), for the immutable backup tests.
  export MONGORESCUE_TEST_S3_MINIO_LOCK_BUCKET="mongorescue-it-lock"
fi

# --- LocalStack ----------------------------------------------------------------
if [[ " $IT_PROVIDERS " == *" localstack "* ]]; then
  log "starting $LOCALSTACK_IMAGE"
  docker run -d --name "$PREFIX-localstack" -p 127.0.0.1::4566 -e SERVICES=s3 \
    "$LOCALSTACK_IMAGE" >/dev/null
  CONTAINERS+=("$PREFIX-localstack")
  LOCALSTACK_URL="http://127.0.0.1:$(host_port "$PREFIX-localstack" 4566)"
  # Captured before matching for the same SIGPIPE/pipefail reason as the MongoDB check.
  localstack_ready() {
    local health
    health=$(curl -fsS "$LOCALSTACK_URL/_localstack/health") || return 1
    grep -Eq '"s3": *"(available|running)"' <<<"$health"
  }
  wait_for "localstack" 120 localstack_ready
  export MONGORESCUE_TEST_S3_LOCALSTACK_ENDPOINT="$LOCALSTACK_URL"
  export MONGORESCUE_TEST_S3_LOCALSTACK_BUCKET="mongorescue-it"
  export MONGORESCUE_TEST_S3_LOCALSTACK_ACCESS_KEY="test"
  export MONGORESCUE_TEST_S3_LOCALSTACK_SECRET_KEY="test"
  export MONGORESCUE_TEST_S3_LOCALSTACK_PATH_STYLE="true"
  export MONGORESCUE_TEST_S3_LOCALSTACK_CREATE_BUCKET="true"
fi

# --- Keycloak (single sign-on) ---------------------------------------------------
if [[ " $IT_PROVIDERS " == *" keycloak "* ]]; then
  log "starting $KEYCLOAK_IMAGE"
  # Dev mode (HTTP, in-memory database) with the test realm imported at start. The
  # realm only holds test users and a test client secret.
  docker run -d --name "$PREFIX-keycloak" -p 127.0.0.1::8080 \
    -e KC_BOOTSTRAP_ADMIN_USERNAME=admin -e KC_BOOTSTRAP_ADMIN_PASSWORD="kc$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')" \
    -v "$ROOT/internal/integration/testdata/keycloak-realm.json:/opt/keycloak/data/import/realm.json:ro" \
    "$KEYCLOAK_IMAGE" start-dev --import-realm >/dev/null
  CONTAINERS+=("$PREFIX-keycloak")
  KEYCLOAK_URL="http://127.0.0.1:$(host_port "$PREFIX-keycloak" 8080)"
  wait_for "keycloak" 180 curl -fsS "$KEYCLOAK_URL/realms/mongorescue/.well-known/openid-configuration"
  export MONGORESCUE_TEST_KEYCLOAK_URL="$KEYCLOAK_URL"
fi

RACE_FLAG="-race"
if [ "$IT_RACE" = 0 ]; then RACE_FLAG=""; fi
log "running integration suite (MongoDB $MONGO_IMAGE $MONGO_TOPOLOGY, providers: local ${IT_PROVIDERS}, race: ${IT_RACE})"
# shellcheck disable=SC2086 # RACE_FLAG and GOTESTFLAGS are intentionally word-split
go test $RACE_FLAG -tags=integration -count=1 ${GOTESTFLAGS:-} ./...
