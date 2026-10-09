#!/usr/bin/env bash
# ==============================================================================
# Runs the PITR replica set scenarios (build tag replset3) or the PITR soak test
# (build tags replset3 and soak) of internal/replset against a disposable
# three-member replica set (scripts/replset/compose.yml): mongod m1, m2 and m3 on
# one network, with a keyfile and a random root password, initiated with
# rs.initiate.
#
# The tests inject failures with docker (kill, network disconnect), so they run in
# a container on the replica set's network with the docker socket mounted
# (scripts/replset/runner.Dockerfile: Ubuntu, MongoDB Database Tools TOOLS_VERSION
# and the docker CLI). The test binary is built on the host for the docker
# server's architecture. Every scenario gets a fresh replica set; the soak test
# runs once. Containers and networks are always removed on exit.
#
# Prerequisites: docker (with compose v2) and Go.
#
# Environment knobs:
#   MONGO_IMAGE     MongoDB image (default: mongo:8.0)
#   TOOLS_VERSION   MongoDB Database Tools in the runner (default: 100.12.2)
#   TOOLS_SHA256    SHA-256 of its Ubuntu 24.04 package, for a version the runner
#                   image does not know (100.12.2 and 100.19.1 are built in)
#   RS_SUITE        scenarios (default) or soak
#   RS_RUN          regular expression of the tests to run (default: all of the suite)
#   RS_LOG_DIR      where the member logs of failed tests go (default: their last
#                   lines are printed)
#   RS_OPLOG_MB     oplog size of each member in MiB (default: 1024)
#   MONGORESCUE_RS_VERBOSE=1  logs the collector and the engines at debug level
#   MONGORESCUE_SOAK_*  passed to the soak test (see internal/replset/soak_test.go),
#                   e.g. MONGORESCUE_SOAK_DURATION=6h; MONGORESCUE_SOAK_REPORT is a
#                   host path the JSON report is written to
# ==============================================================================
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE_FILE="$ROOT/scripts/replset/compose.yml"
export MONGO_IMAGE="${MONGO_IMAGE:-mongo:8.0}"
TOOLS_VERSION="${TOOLS_VERSION:-100.12.2}"
RS_SUITE="${RS_SUITE:-scenarios}"
export RS_OPLOG_MB="${RS_OPLOG_MB:-1024}"
RUNNER_IMAGE="mongorescue-rs-runner:${TOOLS_VERSION}"
WORK=""
RS_PROJECT=""
export RS_PROJECT RS_KEY RS_PASSWORD

log() { printf '==> %s\n' "$*"; }

case "$RS_SUITE" in
  scenarios) TAG=replset3; TEST_TIMEOUT=30m; RS_RUN="${RS_RUN:-.}" ;;
  soak)
    TAG=replset3,soak
    RS_RUN="${RS_RUN:-TestSoak}"
    # The test's own deadline is its duration plus its final checks.
    TEST_TIMEOUT=0
    ;;
  *) echo "unknown RS_SUITE: $RS_SUITE (scenarios or soak)" >&2; exit 1 ;;
esac

for bin in docker go; do
  command -v "$bin" >/dev/null 2>&1 || { echo "missing prerequisite: $bin" >&2; exit 1; }
done
docker compose version >/dev/null 2>&1 || { echo "missing prerequisite: docker compose v2" >&2; exit 1; }

compose() { docker compose -f "$COMPOSE_FILE" "$@"; }

stop_cluster() {
  if [ -n "$RS_PROJECT" ]; then
    compose down -v --remove-orphans >/dev/null 2>&1 || true
    RS_PROJECT=""
  fi
}

cleanup() {
  local status=$?
  stop_cluster
  if [ -n "$WORK" ]; then rm -rf "$WORK"; fi
  exit "$status"
}
trap cleanup EXIT

WORK="$(mktemp -d)"
LOG_DIR="${RS_LOG_DIR:-}"
if [ -n "$LOG_DIR" ]; then mkdir -p "$LOG_DIR"; fi

case "$(docker info --format '{{.Architecture}}')" in
  aarch64 | arm64) GOARCH_DOCKER=arm64 ;;
  *) GOARCH_DOCKER=amd64 ;;
esac
log "building the test binary (tag $TAG, linux/$GOARCH_DOCKER)"
(cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH_DOCKER" \
  go test -c -tags "$TAG" -o "$WORK/replset.test" ./internal/replset)

log "building the runner image (Database Tools $TOOLS_VERSION)"
docker build -q --build-arg TOOLS_VERSION="$TOOLS_VERSION" --build-arg TOOLS_SHA256="${TOOLS_SHA256:-}" -t "$RUNNER_IMAGE" \
  -f "$ROOT/scripts/replset/runner.Dockerfile" "$ROOT/scripts/replset" >/dev/null

# member_eval runs a script through mongosh in a member, authenticated as root
# when auth is "auth".
member_eval() {
  local svc=$1 auth=$2 script=$3
  if [ "$auth" = auth ]; then
    compose exec -T "$svc" mongosh --quiet -u root -p "$RS_PASSWORD" --authenticationDatabase admin --eval "$script"
  else
    compose exec -T "$svc" mongosh --quiet --eval "$script"
  fi
}

wait_until() {
  local what=$1 tries=$2; shift 2
  for _ in $(seq 1 "$tries"); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 1
  done
  echo "timed out waiting for $what" >&2
  return 1
}

m1_initialized() {
  local logs
  # Captured before matching: grep -q would kill docker logs with SIGPIPE.
  logs=$(compose logs --no-color m1 2>&1 || true)
  [[ $logs == *"MongoDB init process complete"* ]] && member_eval m1 auth 'quit(db.runCommand({ping: 1}).ok ? 0 : 1)'
}
member_up() { member_eval "$1" noauth 'quit(db.runCommand({ping: 1}).ok ? 0 : 1)'; }
set_healthy() {
  member_eval m1 auth '
    const s = rs.status();
    const ok = s.members.length === 3 && s.members.every(m => m.state === 1 || m.state === 2) &&
      s.members.filter(m => m.state === 1).length === 1;
    quit(ok ? 0 : 1)'
}

start_cluster() {
  RS_PROJECT="mongorescue-rs-$$-$1"
  RS_KEY="$(head -c 756 /dev/urandom | base64 | tr -d '\n')"
  RS_PASSWORD="rsPw$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"
  log "starting the replica set $RS_PROJECT ($MONGO_IMAGE)"
  compose up -d --quiet-pull >/dev/null
  wait_until "m1 (root user created)" 180 m1_initialized
  wait_until "m2" 120 member_up m2
  wait_until "m3" 120 member_up m3
  member_eval m1 auth 'rs.initiate({_id: "rs0", members: [
      {_id: 0, host: "m1:27017"}, {_id: 1, host: "m2:27017"}, {_id: 2, host: "m3:27017"}],
    settings: {electionTimeoutMillis: 5000}})' >/dev/null
  wait_until "three healthy members" 180 set_healthy
  log "replica set ready"
}

save_logs() {
  if [ -z "$LOG_DIR" ]; then
    echo "---- member logs (last 100 lines each) ----"
    compose logs --no-color --timestamps --tail 100 2>&1 | sed "s/${RS_PASSWORD}/******/g" || true
    return
  fi
  local file="$LOG_DIR/$1.log"
  compose logs --no-color --timestamps 2>&1 | sed "s/${RS_PASSWORD}/******/g" >"$file" || true
  log "member logs: $file"
}

run_test() {
  local name=$1 network members
  network="${RS_PROJECT}_rs"
  members="m1:27017=$(compose ps -q m1),m2:27017=$(compose ps -q m2),m3:27017=$(compose ps -q m3)"
  local env_args=(
    -e "MONGORESCUE_RS_URI=mongodb://root:${RS_PASSWORD}@m1:27017,m2:27017,m3:27017/?replicaSet=rs0&authSource=admin"
    -e "MONGORESCUE_RS_MEMBERS=$members"
    -e "MONGORESCUE_RS_NETWORK=$network"
    -e "MONGORESCUE_RS_PASSWORD=$RS_PASSWORD"
  )
  while IFS='=' read -r key _; do
    [ "$key" = MONGORESCUE_SOAK_REPORT ] || env_args+=(-e "$key")
  done < <(env | grep -E '^MONGORESCUE_(SOAK_|RS_VERBOSE=)' || true)
  # The soak report is written to a mounted directory.
  local mounts=(-v /var/run/docker.sock:/var/run/docker.sock -v "$WORK:/work")
  if [ -n "${MONGORESCUE_SOAK_REPORT:-}" ]; then
    local report_dir
    report_dir="$(cd "$(dirname "$MONGORESCUE_SOAK_REPORT")" && pwd)"
    mounts+=(-v "$report_dir:/report")
    env_args+=(-e "MONGORESCUE_SOAK_REPORT=/report/$(basename "$MONGORESCUE_SOAK_REPORT")")
  fi
  log "running $name"
  docker run --rm --name "${RS_PROJECT}-runner" --network "$network" \
    "${mounts[@]}" "${env_args[@]}" "$RUNNER_IMAGE" \
    /work/replset.test -test.v -test.count=1 -test.timeout "$TEST_TIMEOUT" -test.run "^${name}\$"
}

TESTS=()
while IFS= read -r name; do
  TESTS+=("$name")
done < <(docker run --rm -v "$WORK:/work" "$RUNNER_IMAGE" \
  /work/replset.test -test.list "$RS_RUN" | grep '^Test' || true)
if [ "${#TESTS[@]}" -eq 0 ]; then
  echo "no test of the $RS_SUITE suite matches $RS_RUN" >&2
  exit 1
fi

failed=()
n=0
for name in "${TESTS[@]}"; do
  n=$((n + 1))
  start_cluster "$n"
  if run_test "$name"; then
    log "$name passed"
  else
    failed+=("$name")
    save_logs "$name"
  fi
  stop_cluster
done

if [ "${#failed[@]}" -gt 0 ]; then
  echo "failed: ${failed[*]}" >&2
  exit 1
fi
log "all ${#TESTS[@]} test(s) of the $RS_SUITE suite passed (MongoDB $MONGO_IMAGE, Database Tools $TOOLS_VERSION)"
