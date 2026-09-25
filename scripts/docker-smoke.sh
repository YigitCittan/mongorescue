#!/usr/bin/env bash
# ==============================================================================
# Smoke-tests a built MongoRescue container image with zero configuration:
#   - bundled mongodump/mongorestore are >= 100.3.0 (required for --config)
#   - the server boots in setup mode; /api/v1/health and setup status are public,
#     everything else needs a session or an API key
#   - first-run setup with the setup code from the container logs, login, CSRF,
#     and creating a connection (its password is never returned)
#   - the default "Local disk" storage target, settings applied live through the
#     API and an API key created through the API
#   - API keys are read-only by default; the MCP endpoint (/mcp) takes API keys only
#     and the stdio bridge (mongorescue mcp) forwards to it
#   - the embedded dashboard is served at /
#   - the server runs as UID 10001, can write its volumes and keeps its metadata
#     database and secret key private
#
# Usage: scripts/docker-smoke.sh [image]   (default: mongorescue:smoke)
# Prerequisites: docker, curl.
# ==============================================================================
set -euo pipefail

IMAGE="${1:-mongorescue:smoke}"
NAME="mongorescue-smoke-$$"
MIN_TOOLS="100.3.0"
PASSWORD="smoke-$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')-pw"
CONN_PW="connpw$(od -An -N6 -tx1 /dev/urandom | tr -d ' \n')"
WORK="$(mktemp -d)"

log() { printf '==> %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

cleanup() {
  local status=$?
  if [ "$status" -ne 0 ]; then
    echo "---- container logs ----"
    docker logs "$NAME" 2>&1 | tail -n 50 || true
  fi
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  rm -rf "$WORK"
  exit "$status"
}
trap cleanup EXIT

for tool in mongodump mongorestore; do
  # Capture the full output first: an early-exiting reader would SIGPIPE docker under pipefail.
  out=$(docker run --rm --entrypoint "$tool" "$IMAGE" --version)
  version=$(printf '%s\n' "$out" | awk '/version:/ && !found {print $NF; found=1}')
  [ -n "$version" ] || fail "could not determine $tool version"
  printf '%s\n%s\n' "$MIN_TOOLS" "$version" | sort -V -C || fail "$tool $version < $MIN_TOOLS"
  log "$tool $version (>= $MIN_TOOLS)"
done

# No environment at all: the image must start and wait for setup.
docker run -d --name "$NAME" -p 127.0.0.1::8080 "$IMAGE" >/dev/null
BASE="http://127.0.0.1:$(docker port "$NAME" 8080 | head -n1 | sed 's/.*://')"

for _ in $(seq 1 30); do
  if [ "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/v1/health")" = "200" ]; then break; fi
  sleep 1
done

code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }
# json_field <field> reads a string field from JSON on stdin (no jq needed).
json_field() { sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p" | head -n1; }

curl -fsS "$BASE/api/v1/health" | grep -q '"status":"healthy"' || fail "health endpoint not healthy"
log "GET /api/v1/health -> healthy"

curl -fsS "$BASE/api/v1/setup/status" | grep -q '"setup_required":true' || fail "fresh container is not in setup mode"
log "GET /api/v1/setup/status -> setup_required: true"

[ "$(code "$BASE/api/v1/jobs")" = "401" ] || fail "/api/v1/jobs without credentials not 401"
[ "$(code "$BASE/metrics")" = "401" ] || fail "/metrics without credentials not 401"
log "protected endpoints -> 401"

SETUP_CODE=""
for _ in $(seq 1 10); do
  SETUP_CODE=$(docker logs "$NAME" 2>&1 | sed -n 's/.*setup_code=\([A-Z2-7-]*\).*/\1/p' | tail -n1)
  [ -n "$SETUP_CODE" ] && break
  sleep 1
done
[ -n "$SETUP_CODE" ] || fail "setup code not found in the container logs"
log "setup code read from the container logs"

JAR="$WORK/cookies"
setup_body=$(printf '{"setup_code":"%s","username":"admin","password":"%s"}' "$SETUP_CODE" "$PASSWORD")
status=$(curl -s -o "$WORK/setup.json" -w '%{http_code}' -c "$JAR" -H 'Content-Type: application/json' \
  -d "$setup_body" "$BASE/api/v1/setup")
[ "$status" = "201" ] || fail "setup returned $status: $(cat "$WORK/setup.json")"
grep -qi 'mr_session' "$JAR" || fail "setup did not set the session cookie"
log "POST /api/v1/setup -> 201 with session cookie"

[ "$(code -H 'Content-Type: application/json' -d "$setup_body" "$BASE/api/v1/setup")" = "409" ] || fail "second setup not 409"
log "second setup -> 409"

JAR2="$WORK/cookies2"
status=$(curl -s -o "$WORK/login.json" -w '%{http_code}' -c "$JAR2" -H 'Content-Type: application/json' \
  -d "$(printf '{"username":"admin","password":"%s"}' "$PASSWORD")" "$BASE/api/v1/auth/login")
[ "$status" = "200" ] || fail "login returned $status"
CSRF=$(json_field csrf_token < "$WORK/login.json")
[ -n "$CSRF" ] || fail "login response has no csrf_token"
log "POST /api/v1/auth/login -> 200"

conn_body=$(printf '{"name":"smoke","uri":"mongodb://smoke:%s@mongo.invalid:27017/?authSource=admin"}' "$CONN_PW")
[ "$(code -b "$JAR2" -H 'Content-Type: application/json' -d "$conn_body" "$BASE/api/v1/connections")" = "403" ] \
  || fail "cookie POST without X-CSRF-Token not 403"
status=$(curl -s -o "$WORK/conn.json" -w '%{http_code}' -b "$JAR2" -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $CSRF" -d "$conn_body" "$BASE/api/v1/connections")
[ "$status" = "201" ] || fail "create connection returned $status: $(cat "$WORK/conn.json")"
grep -q "$CONN_PW" "$WORK/conn.json" && fail "connection password returned by the API"
curl -fsS -b "$JAR2" "$BASE/api/v1/connections" | grep -q '"name":"smoke"' || fail "connection not listed"
log "POST /api/v1/connections -> 201 (CSRF enforced, password masked)"

# Zero configuration: the default "Local disk" target writes to the /backups volume.
curl -fsS -b "$JAR2" "$BASE/api/v1/storage-targets" > "$WORK/targets.json"
for want in '"name":"Local disk"' '"path":"/backups"' '"is_default":true'; do
  grep -qF "$want" "$WORK/targets.json" || fail "default storage target missing $want: $(cat "$WORK/targets.json")"
done
TARGET_ID=$(json_field id < "$WORK/targets.json")
curl -fsS -b "$JAR2" -H "X-CSRF-Token: $CSRF" -X POST "$BASE/api/v1/storage-targets/$TARGET_ID/test" | grep -q '"ok":true' \
  || fail "default storage target test failed"
log "default storage target \"Local disk\" at /backups (test ok)"

# Settings live in the database and apply without a restart.
curl -fsS -b "$JAR2" "$BASE/api/v1/settings" | grep -q '"backup_timeout":"6h0m0s"' || fail "settings defaults missing"
status=$(curl -s -o "$WORK/settings.json" -w '%{http_code}' -b "$JAR2" -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $CSRF" -X PUT -d '{"security":{"metrics_public":true}}' "$BASE/api/v1/settings")
[ "$status" = "200" ] || fail "PUT /api/v1/settings returned $status: $(cat "$WORK/settings.json")"
[ "$(code "$BASE/metrics")" = "200" ] || fail "metrics_public did not apply without a restart"
curl -fsS -b "$JAR2" -H 'Content-Type: application/json' -H "X-CSRF-Token: $CSRF" -X PUT \
  -d '{"security":{"metrics_public":false}}' "$BASE/api/v1/settings" >/dev/null
[ "$(code "$BASE/metrics")" = "401" ] || fail "/metrics public after switching it off"
log "PUT /api/v1/settings applies live (metrics_public on/off)"

# Automation uses an API key created in the dashboard (no environment variable).
curl -fsS -b "$JAR2" -H 'Content-Type: application/json' -H "X-CSRF-Token: $CSRF" \
  -d '{"name":"smoke"}' "$BASE/api/v1/api-keys" > "$WORK/key.json"
API_KEY=$(json_field key < "$WORK/key.json")
[ -n "$API_KEY" ] || fail "api key not created"
[ "$(code -H "Authorization: Bearer $API_KEY" "$BASE/metrics")" = "200" ] || fail "/metrics with an api key not 200"
grep -q '"scope":"read"' "$WORK/key.json" || fail "a key created without a scope is not read-only: $(cat "$WORK/key.json")"
[ "$(code -H "Authorization: Bearer $API_KEY" -H 'Content-Type: application/json' -d '{}' "$BASE/api/v1/backups")" = "403" ] \
  || fail "a read-only key may start a backup"
log "API key created through the API works for /metrics and is read-only by default"

# MCP: Streamable HTTP at /mcp for API keys only, and the stdio bridge (mongorescue mcp).
MCP_INIT='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}'
MCP_LIST='{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
mcp_post() { curl -s -o "$WORK/mcp.json" -w '%{http_code}' -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' "$@" "$BASE/mcp"; }
[ "$(mcp_post -d "$MCP_INIT")" = "401" ] || fail "/mcp without credentials not 401"
[ "$(mcp_post -b "$JAR2" -H "X-CSRF-Token: $CSRF" -d "$MCP_INIT")" = "401" ] || fail "/mcp accepted a session cookie"
[ "$(mcp_post -H "Authorization: Bearer $API_KEY" -H 'Origin: https://evil.example' -d "$MCP_INIT")" = "403" ] \
  || fail "/mcp accepted a cross-origin request"
[ "$(mcp_post -H "Authorization: Bearer $API_KEY" -d "$MCP_INIT")" = "200" ] || fail "/mcp initialize failed: $(cat "$WORK/mcp.json")"
grep -q '"name":"mongorescue"' "$WORK/mcp.json" || fail "/mcp initialize did not identify the server: $(cat "$WORK/mcp.json")"
[ "$(mcp_post -H "Authorization: Bearer $API_KEY" -d "$MCP_LIST")" = "200" ] || fail "/mcp tools/list failed"
grep -q '"get_status"' "$WORK/mcp.json" || fail "/mcp tools/list lacks get_status"
grep -q '"start_backup"' "$WORK/mcp.json" && fail "/mcp lists start_backup to a read-only key"
bridge_out=$({ printf '%s\n%s\n' "$MCP_INIT" "$MCP_LIST"; sleep 3; } | docker exec -i -e "MONGORESCUE_MCP_API_KEY=$API_KEY" "$NAME" \
  mongorescue mcp --url http://127.0.0.1:8080 2>"$WORK/bridge.err" || true)
printf '%s' "$bridge_out" | grep -q '"get_status"' || fail "stdio bridge did not list tools: $bridge_out $(cat "$WORK/bridge.err")"
grep -qF "$API_KEY" "$WORK/bridge.err" && fail "the stdio bridge logged the API key"
log "MCP: /mcp needs an API key (no cookies, no cross-origin), scopes filter tools, stdio bridge works"

curl -fsS "$BASE/" | grep -q 'app.js' || fail "/ does not serve the embedded dashboard"
log "GET / -> embedded dashboard"

curl -fsS "$BASE/api/v1/health" | grep -q '"status":"healthy"' || fail "health not healthy after setup"
log "GET /api/v1/health -> healthy"

[ "$(docker exec "$NAME" id -u)" = "10001" ] || fail "container does not run as UID 10001"
docker exec "$NAME" sh -c 'test -w /data && test -w /backups' || fail "/data or /backups not writable by the runtime user"
for f in mongorescue.db secret.key; do
  mode=$(docker exec "$NAME" stat -c '%a' "/data/$f") || fail "/data/$f missing"
  [ "$mode" = "600" ] || fail "/data/$f has mode $mode, want 600"
done
log "runs as UID 10001; /data/mongorescue.db and /data/secret.key are 0600"

logs=$(docker logs "$NAME" 2>&1)
if printf '%s' "$logs" | grep -q 'deprecated'; then fail "a zero-configuration start logged deprecation warnings"; fi
for secret in "$PASSWORD" "$CONN_PW" "$CSRF" "$API_KEY"; do
  if printf '%s' "$logs" | grep -qF "$secret"; then fail "a secret leaked into the container logs"; fi
done
log "smoke test passed"
