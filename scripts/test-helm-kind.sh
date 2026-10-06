#!/usr/bin/env bash
# ==============================================================================
# Smoke-tests the Helm chart (deploy/helm/mongorescue) on a kind cluster:
#   - builds the image from this checkout and loads it into the cluster
#   - runs a MongoDB pod (MONGO_IMAGE) with a random root password and a seeded
#     "shop" database
#   - installs the chart and waits for the StatefulSet to pass its startup,
#     liveness and readiness probes, then runs `helm test`
#   - checks the security context (UID 10001, read-only root filesystem)
#   - signs in through the setup API with the setup code from the pod log, adds a
#     connection, runs a backup to the default "Local disk" target and waits for it
#     to complete
#   - restarts the pod (rolling update with the graceful shutdown) and checks the
#     backup is still listed: the database volume and the secret key survived
#
# Usage: scripts/test-helm-kind.sh
# Prerequisites: docker, kind, kubectl, helm, curl and jq.
#
# Environment knobs:
#   KIND_CLUSTER     kind cluster name (default: mongorescue-helm); created when it
#                    does not exist and deleted afterwards unless KEEP_CLUSTER=1
#   KIND_NODE_IMAGE  node image for a new cluster (default: kind's default)
#   IMAGE            image name to build and load (default: mongorescue:kind)
#   SKIP_BUILD       1 uses an IMAGE that is already built
#   MONGO_IMAGE      MongoDB image (default: mongo:7, pinned by digest)
#   NAMESPACE        namespace of the test (default: mongorescue-smoke)
# ==============================================================================
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHART="$ROOT/deploy/helm/mongorescue"
KIND_CLUSTER="${KIND_CLUSTER:-mongorescue-helm}"
IMAGE="${IMAGE:-mongorescue:kind}"
MONGO_IMAGE="${MONGO_IMAGE:-mongo:7@sha256:1f995ad6fdb93244a1addab1b58f934a0bc2f5643c38e02f5e9d7f0c7d227a7b}"
NAMESPACE="${NAMESPACE:-mongorescue-smoke}"
RELEASE="smoke"
FULLNAME="$RELEASE-mongorescue"
MONGO_PW="kindPw$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"
ADMIN_PW="kind-$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')-pw"
WORK="$(mktemp -d)"
PF_PID=""
CREATED_CLUSTER=0

log() { printf '==> %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

stop_port_forward() {
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" >/dev/null 2>&1 || true
    wait "$PF_PID" 2>/dev/null || true
    PF_PID=""
  fi
}

cleanup() {
  local status=$?
  stop_port_forward
  if [ "$status" -ne 0 ]; then
    echo "---- pods ----"
    kubectl -n "$NAMESPACE" get pods -o wide 2>&1 || true
    echo "---- events ----"
    kubectl -n "$NAMESPACE" get events --sort-by=.lastTimestamp 2>&1 | tail -n 30 || true
    echo "---- mongorescue logs ----"
    kubectl -n "$NAMESPACE" logs "statefulset/$FULLNAME" --tail=80 2>&1 | grep -v 'setup_code' || true
  fi
  if [ "$CREATED_CLUSTER" = 1 ] && [ "${KEEP_CLUSTER:-0}" != 1 ]; then
    kind delete cluster --name "$KIND_CLUSTER" >/dev/null 2>&1 || true
  fi
  rm -rf "$WORK"
  exit "$status"
}
trap cleanup EXIT

for tool in docker kind kubectl helm curl jq; do
  command -v "$tool" >/dev/null || fail "$tool is required"
done

if ! kind get clusters 2>/dev/null | grep -qx "$KIND_CLUSTER"; then
  log "creating kind cluster $KIND_CLUSTER"
  args=(create cluster --name "$KIND_CLUSTER" --wait 120s)
  if [ -n "${KIND_NODE_IMAGE:-}" ]; then args+=(--image "$KIND_NODE_IMAGE"); fi
  kind "${args[@]}"
  CREATED_CLUSTER=1
fi
kubectl config use-context "kind-$KIND_CLUSTER" >/dev/null

if [ "${SKIP_BUILD:-0}" != 1 ]; then
  log "building $IMAGE"
  docker build -q -t "$IMAGE" "$ROOT" >/dev/null
fi
kind load docker-image "$IMAGE" --name "$KIND_CLUSTER" >/dev/null
log "image $IMAGE loaded into the cluster"

kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n "$NAMESPACE" create secret generic mongo-root --from-literal=password="$MONGO_PW" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n "$NAMESPACE" apply -f - >/dev/null <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: mongo
spec:
  replicas: 1
  selector:
    matchLabels: {app: mongo}
  template:
    metadata:
      labels: {app: mongo}
    spec:
      containers:
        - name: mongo
          image: $MONGO_IMAGE
          env:
            - {name: MONGO_INITDB_ROOT_USERNAME, value: root}
            - name: MONGO_INITDB_ROOT_PASSWORD
              valueFrom: {secretKeyRef: {name: mongo-root, key: password}}
          ports:
            - containerPort: 27017
          readinessProbe:
            exec:
              command: [mongosh, --quiet, --eval, "db.adminCommand('ping').ok"]
            periodSeconds: 5
---
apiVersion: v1
kind: Service
metadata:
  name: mongo
spec:
  selector: {app: mongo}
  ports:
    - port: 27017
EOF
kubectl -n "$NAMESPACE" rollout status deployment/mongo --timeout=300s >/dev/null
# The image's first start runs a temporary mongod without the root user (which the
# readiness probe can already reach) and then restarts with authentication: retry.
seeded=0
for _ in $(seq 1 60); do
  if kubectl -n "$NAMESPACE" exec deploy/mongo -- mongosh --quiet -u root -p "$MONGO_PW" --authenticationDatabase admin --eval \
    'db.getSiblingDB("shop").orders.insertMany(Array.from({length: 500}, (_, i) => ({n: i, item: "widget-" + i})))' >/dev/null 2>&1; then
    seeded=1
    break
  fi
  sleep 2
done
[ "$seeded" = 1 ] || fail "could not seed the shop database"
log "MongoDB is ready with a seeded shop database"

helm upgrade --install "$RELEASE" "$CHART" --namespace "$NAMESPACE" \
  --set image.repository="${IMAGE%:*}" --set image.tag="${IMAGE##*:}" --set image.pullPolicy=Never \
  --set terminationGracePeriodSeconds=120 \
  --set persistence.size=1Gi --set backups.persistence.size=1Gi \
  --wait --timeout 6m >/dev/null
kubectl -n "$NAMESPACE" rollout status "statefulset/$FULLNAME" --timeout=300s >/dev/null
log "chart installed; the pod passed its probes"

grace=$(kubectl -n "$NAMESPACE" get pod "$FULLNAME-0" -o jsonpath='{.spec.containers[0].env[?(@.name=="MONGORESCUE_SHUTDOWN_GRACE")].value}')
[ "$grace" = "60s" ] || fail "MONGORESCUE_SHUTDOWN_GRACE is $grace, want 60s (terminationGracePeriodSeconds 120 - 60)"
[ "$(kubectl -n "$NAMESPACE" exec "$FULLNAME-0" -- id -u)" = "10001" ] || fail "the container does not run as UID 10001"
if kubectl -n "$NAMESPACE" exec "$FULLNAME-0" -- touch /usr/local/bin/x 2>/dev/null; then fail "the root filesystem is writable"; fi
kubectl -n "$NAMESPACE" exec "$FULLNAME-0" -- sh -c 'test -w /data && test -w /backups && test -w /tmp' || fail "/data, /backups or /tmp not writable"
log "runs as UID 10001 with a read-only root filesystem; /data, /backups and /tmp are writable"

helm test "$RELEASE" --namespace "$NAMESPACE" --timeout 2m >/dev/null || fail "helm test failed"
log "helm test passed"

PORT=$(( 20000 + RANDOM % 20000 ))
kubectl -n "$NAMESPACE" port-forward "svc/$FULLNAME" "$PORT:8080" >"$WORK/pf.log" 2>&1 &
PF_PID=$!
BASE="http://127.0.0.1:$PORT"
for _ in $(seq 1 30); do
  if curl -fsS "$BASE/api/v1/health" >/dev/null 2>&1; then break; fi
  sleep 1
done
curl -fsS "$BASE/api/v1/health" | jq -e '.data.status == "healthy"' >/dev/null || fail "health endpoint not healthy"

SETUP_CODE=$(kubectl -n "$NAMESPACE" logs "$FULLNAME-0" | sed -n 's/.*setup_code=\([A-Z2-7-]*\).*/\1/p' | tail -n1)
[ -n "$SETUP_CODE" ] || fail "setup code not found in the pod log"
JAR="$WORK/cookies"
status=$(curl -s -o "$WORK/setup.json" -w '%{http_code}' -c "$JAR" -H 'Content-Type: application/json' \
  -d "$(jq -n --arg c "$SETUP_CODE" --arg p "$ADMIN_PW" '{setup_code: $c, username: "admin", password: $p}')" "$BASE/api/v1/setup")
[ "$status" = "201" ] || fail "setup returned $status: $(cat "$WORK/setup.json")"
CSRF=$(jq -r '.data.csrf_token' "$WORK/setup.json")
if [ -z "$CSRF" ] || [ "$CSRF" = null ]; then fail "setup response has no csrf_token"; fi
log "signed in through the setup API"

api() { # api METHOD PATH [BODY]: prints the body, fails on a non-2xx status
  local method=$1 path=$2 body=${3:-}
  local args=(-s -o "$WORK/res.json" -w '%{http_code}' -b "$JAR" -H "X-CSRF-Token: $CSRF" -X "$method")
  if [ -n "$body" ]; then args+=(-H 'Content-Type: application/json' -d "$body"); fi
  local code
  code=$(curl "${args[@]}" "$BASE$path")
  case "$code" in 2*) cat "$WORK/res.json" ;; *) fail "$method $path returned $code: $(cat "$WORK/res.json")" ;; esac
}

URI="mongodb://root:$MONGO_PW@mongo.$NAMESPACE.svc.cluster.local:27017/?authSource=admin"
CONN_ID=$(api POST /api/v1/connections "$(jq -n --arg u "$URI" '{name: "kind", uri: $u}')" | jq -r '.data.id')
if [ -z "$CONN_ID" ] || [ "$CONN_ID" = null ]; then fail "connection not created"; fi
log "connection $CONN_ID added"

BACKUP_ID=$(api POST /api/v1/backups "$(jq -n --arg c "$CONN_ID" '{connection_id: $c, database: "shop"}')" | jq -r '.data.id')
if [ -z "$BACKUP_ID" ] || [ "$BACKUP_ID" = null ]; then fail "backup not started"; fi
log "backup $BACKUP_ID started"

backup_status() {
  api GET "/api/v1/backups?search=$BACKUP_ID" | jq -r --arg id "$BACKUP_ID" '[.data[] | select(.id == $id)][0] | "\(.status) \(.size_bytes) \(.error_message // "")"'
}
state=""
for _ in $(seq 1 90); do
  state=$(backup_status)
  case "$state" in
    completed\ *) break ;;
    failed\ *|cancelled\ *) fail "backup $BACKUP_ID ended as $state" ;;
  esac
  sleep 2
done
case "$state" in completed\ *) ;; *) fail "backup $BACKUP_ID did not complete: $state" ;; esac
size=$(echo "$state" | awk '{print $2}')
[ "$size" -gt 0 ] || fail "completed backup has size $size"
log "backup completed ($size bytes)"

stop_port_forward
kubectl -n "$NAMESPACE" rollout restart "statefulset/$FULLNAME" >/dev/null
kubectl -n "$NAMESPACE" rollout status "statefulset/$FULLNAME" --timeout=300s >/dev/null
kubectl -n "$NAMESPACE" port-forward "svc/$FULLNAME" "$PORT:8080" >"$WORK/pf.log" 2>&1 &
PF_PID=$!
for _ in $(seq 1 30); do
  if curl -fsS "$BASE/api/v1/health" >/dev/null 2>&1; then break; fi
  sleep 1
done
state=$(backup_status)
case "$state" in completed\ *) ;; *) fail "after a restart the backup is $state" ;; esac
log "after a rolling restart the session, the database and the backup are intact"

log "helm kind smoke test passed"
