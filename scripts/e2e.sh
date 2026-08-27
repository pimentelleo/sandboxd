#!/usr/bin/env bash
# End-to-end smoke test for sandboxd against a real Docker daemon.
# Exercises the create -> seed -> install -> serve -> wake lifecycle with
# NO model credentials required, so it is deterministic in CI.
#
# Inputs (env):
#   SANDBOXD_IMAGE  built base image tag (e.g. sandboxd-base:ci)
#   SANDBOXD_BIN    path to a built sandboxd binary
#   DOCKER           container CLI (default: docker; e.g. podman)
#
# Run locally:  SANDBOXD_IMAGE=sandboxd-base:starter SANDBOXD_BIN=/tmp/sandboxd sudo -E bash scripts/e2e.sh
set -uo pipefail

IMG="${SANDBOXD_IMAGE:?set SANDBOXD_IMAGE}"
BIN="${SANDBOXD_BIN:?set SANDBOXD_BIN}"
DOCKER="${DOCKER:-docker}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
DD="$(mktemp -d)"
NET="sbxe2e-$$"
API="http://127.0.0.1:9191"
SBX_PID=""
declare -a IDS=()

log() { echo "── $*"; }
fail() { echo "❌ FAIL: $*"; exit 1; }

cleanup() {
  for id in "${IDS[@]:-}"; do
    [ -n "$id" ] && curl -s -m30 -o /dev/null -XPOST "$API/sandbox/$id/purge" >/dev/null 2>&1 || true
  done
  [ -n "$SBX_PID" ] && kill "$SBX_PID" >/dev/null 2>&1 || true
  $DOCKER network rm "$NET" >/dev/null 2>&1 || true
  rm -rf "$DD" || true
}
trap cleanup EXIT

mk() { curl -s -m120 -XPOST "$API/sandbox" -H 'content-type: application/json' -d "$1" | grep -oE '"id":"[^"]+"' | head -1 | cut -d'"' -f4; }
ipof() { $DOCKER inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "s-$1" 2>/dev/null; }
status() { curl -s -m5 "$API/v1/sandboxes/$1" | grep -oE '"status":"[a-z]+"' | head -1 | cut -d'"' -f4; }

# 1. Image smoke: agent CLIs, toolchain, and built-in templates present.
log "image smoke: required binaries + preset templates"
$DOCKER run --rm "$IMG" bash -lc '
  set -e
  for b in runtimed opencode claude node pnpm git rg; do
    command -v "$b" >/dev/null || { echo "MISSING $b on PATH"; exit 1; }
  done
  test -f /opt/templates/react-standard/package.json
  test -f /opt/templates/adonisjs-hypermedia-standard/bootstrap.sh
  bash -n /opt/templates/adonisjs-hypermedia-standard/bootstrap.sh
' || fail "image smoke (missing binary or template)"

# Start sandboxd. Idle reaper pushed out so it cannot interfere mid-test.
# API auth is on by default and there is no loopback bypass, so this lifecycle
# test (which drives the API over 127.0.0.1 without a credential) runs with auth
# explicitly disabled — the auth behaviour itself is covered by Go unit tests.
mkdir -p "$DD/log" "$DD/library"
$DOCKER network create "$NET" >/dev/null
SANDBOXD_ADDR=127.0.0.1:9191 SANDBOXD_IMAGE="$IMG" SANDBOXD_DATA_DIR="$DD" \
  SANDBOXD_LOG_DIR="$DD/log" SANDBOXD_NETWORK="$NET" SANDBOXD_USERNS=host \
  SANDBOXD_LIBRARY_DIR="$DD/library" SANDBOXD_MIGRATIONS="$HERE/control-plane/migrations" \
  SANDBOXD_DB="$DD/sandboxd.db" SANDBOXD_IDLE_THRESHOLD_SECONDS=3600 \
  SANDBOXD_API_AUTH_DISABLED=true \
  SANDBOXD_TELEMETRY=off \
  "$BIN" >"$DD/sandboxd.log" 2>&1 &
SBX_PID=$!
for i in $(seq 1 60); do curl -s -m2 "$API/healthz" 2>/dev/null | grep -q ok && break; sleep 0.5; done
curl -s -m3 "$API/healthz" | grep -q ok || { cat "$DD/sandboxd.log"; fail "sandboxd never became healthy"; }
log "sandboxd healthy"

# 2. Default create -> react-standard seeded -> dev server installs + serves 200.
log "create (default template) -> preview returns 200"
ID="$(mk '{"ports":[3000]}')"; [ -n "$ID" ] || fail "create returned no id"; IDS+=("$ID")
IP="$(ipof "$ID")"; [ -n "$IP" ] || fail "no container IP for $ID"
up=""
for i in $(seq 1 90); do
  [ "$(curl -s -m3 -o /dev/null -w '%{http_code}' "http://$IP:3000/" 2>/dev/null)" = "200" ] && { up=1; break; }
  sleep 3
done
[ -n "$up" ] || { $DOCKER logs "s-$ID" 2>&1 | tail -25; fail "preview never returned 200"; }
log "preview 200 ✓"

# 3. AdonisJS preset -> generated Hypermedia app, persistent SQLite, stop/start.
log "create (AdonisJS Hypermedia) -> preview + persistent SQLite"
IDA="$(mk '{"ports":[3000],"runtime_preset":"adonisjs"}')"; [ -n "$IDA" ] || fail "AdonisJS create returned no id"; IDS+=("$IDA")
AIP="$(ipof "$IDA")"; [ -n "$AIP" ] || fail "no container IP for AdonisJS sandbox"
adonis_up=""
for i in $(seq 1 180); do
  [ "$(curl -s -m3 -o /dev/null -w '%{http_code}' "http://$AIP:3000/" 2>/dev/null)" = "200" ] && { adonis_up=1; break; }
  sleep 3
done
[ -n "$adonis_up" ] || { $DOCKER logs "s-$IDA" 2>&1 | tail -50; fail "AdonisJS preview never returned 200"; }
ADONIS_APP="$DD/workspaces/$IDA/workspace/app"
test -f "$ADONIS_APP/tmp/db.sqlite3" || fail "AdonisJS SQLite database was not created in persistent tmp/"
test -L "$ADONIS_APP/build/tmp" || fail "AdonisJS build/tmp is not a persistent SQLite symlink"
[ "$(readlink "$ADONIS_APP/build/tmp")" = "../tmp" ] || fail "AdonisJS build/tmp target is not ../tmp"
curl -s -m20 -o /dev/null -XPOST "$API/v1/sandboxes/$IDA/stop"
for i in $(seq 1 30); do [ "$(status "$IDA")" = "stopped" ] && break; sleep 1; done
[ "$(status "$IDA")" = "stopped" ] || fail "AdonisJS sandbox did not stop"
curl -s -m120 -o /dev/null -XPOST "$API/v1/sandboxes/$IDA/start"
adonis_restarted=""
for i in $(seq 1 60); do
  AIP="$(ipof "$IDA")"
  [ -n "$AIP" ] && [ "$(curl -s -m3 -o /dev/null -w '%{http_code}' "http://$AIP:3000/" 2>/dev/null)" = "200" ] && { adonis_restarted=1; break; }
  sleep 2
done
[ -n "$adonis_restarted" ] || { $DOCKER logs "s-$IDA" 2>&1 | tail -50; fail "AdonisJS preview did not recover after start"; }
test -f "$ADONIS_APP/tmp/db.sqlite3" || fail "AdonisJS SQLite database did not persist after restart"
log "AdonisJS lifecycle ✓"

# 4. template:blank -> empty workspace/app.
log "create (template=blank) -> empty app"
IDB="$(mk '{"ports":[3000],"template":"blank"}')"; [ -n "$IDB" ] || fail "blank create returned no id"; IDS+=("$IDB")
sleep 5
[ -z "$(ls -A "$DD/workspaces/$IDB/workspace/app" 2>/dev/null)" ] || fail "template=blank produced a non-empty app"
log "blank empty ✓"

# 5. Invalid create payload -> 400 (no partial state).
log "invalid create payload -> 400"
code="$(curl -s -m10 -o /dev/null -w '%{http_code}' -XPOST "$API/sandbox" -H 'content-type: application/json' -d '{"ports":"nope"}')"
[ "$code" = "400" ] || fail "bad payload returned $code, want 400"
log "bad payload 400 ✓"

# 6. Stop -> submit task -> wakes to running (wake path, no model needed).
log "stopped sandbox -> submit task -> wakes"
curl -s -m20 -o /dev/null -XPOST "$API/v1/sandboxes/$ID/stop"
for i in $(seq 1 15); do [ "$(status "$ID")" = "stopped" ] && break; sleep 1; done
[ "$(status "$ID")" = "stopped" ] || log "warn: sandbox not 'stopped' before wake test"
curl -s -m60 -o /dev/null -XPOST "$API/v1/sandboxes/$ID/tasks" -H 'content-type: application/json' -d '{"prompt":"noop","timeout_s":30}' || true
woke=""
for i in $(seq 1 25); do [ "$(status "$ID")" = "running" ] && { woke=1; break; }; sleep 2; done
[ -n "$woke" ] || fail "sandbox did not wake on task submit"
log "wake-on-task ✓"

# 7. App model: a durable app above sandboxes (create → attach → survive).
log "app: create → attach sandbox → 409 on second → survives delete"
APP="$(curl -s -m10 -XPOST "$API/v1/apps" -H 'content-type: application/json' -d '{"name":"E2E App","tags":["e2e"]}' | grep -oE '"id":"[^"]+"' | head -1 | cut -d'"' -f4)"
[ -n "$APP" ] || fail "create app returned no id"
curl -s -m10 "$API/v1/apps" | grep -q "\"$APP\"" || fail "app not in tenant list"
sbcode="$(curl -s -m120 -o /tmp/appsb.json -w '%{http_code}' -XPOST "$API/v1/apps/$APP/sandbox" -H 'content-type: application/json' -d '{}')"
[ "$sbcode" = "201" ] || { cat /tmp/appsb.json; fail "attach sandbox returned $sbcode, want 201"; }
ASB="$(grep -oE '"id":"[^"]+"' /tmp/appsb.json | head -1 | cut -d'"' -f4)"; IDS+=("$ASB")
curl -s -m10 "$API/v1/apps/$APP" | grep -q "\"current_sandbox_id\":\"$ASB\"" || fail "app current_sandbox_id not set"
dup="$(curl -s -m10 -o /dev/null -w '%{http_code}' -XPOST "$API/v1/apps/$APP/sandbox" -H 'content-type: application/json' -d '{}')"
[ "$dup" = "409" ] || fail "second attach returned $dup, want 409 (one live sandbox per app)"
curl -s -m60 -o /dev/null -XDELETE "$API/sandbox/$ASB"
gone=""
for i in $(seq 1 30); do
  curl -s -m10 "$API/v1/apps/$APP" | grep -q 'current_sandbox_id' || { gone=1; break; }
  sleep 1
done
[ -n "$gone" ] || fail "current_sandbox_id still present after sandbox delete"
curl -s -m10 "$API/v1/apps/$APP" | grep -q "\"id\":\"$APP\"" || fail "app did not survive sandbox delete"
log "app lifecycle ✓"

echo "✅ E2E PASSED"
