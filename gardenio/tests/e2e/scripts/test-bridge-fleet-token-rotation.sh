#!/usr/bin/env bash
# test-bridge-fleet-token-rotation.sh — bridge token rotation E2E
#
# NOTE (ka-0000005788): the credential concept was renamed fleet→bridge.
# This script deliberately exercises the LEGACY wire surface
# (POST /v1/fleet-tokens, `fleet` field) which the daemon dual-accepts
# through the compat window, so it validates the alias keeps working. The
# canonical route is /v1/bridge-tokens. Filename/module name kept stable so
# run-shard.sh keeps finding it.
#
# Daemon-side rail for the bridge token-rotation cron (ka-0000002353 /
# gd-37h1LUVQXa). The cron mints a new short-TTL access token via
# refresh on each fire; if any step here breaks the bridges silently
# 401 in steady-state.
#
# Flow:
#   1. Register a fleet token with ttl=20s, with_refresh=true,
#      subject=bridge:test via POST /v1/fleet-tokens.
#   2. Positive control: GET /v1/beads with the access token → 200.
#   3. Wait past TTL → GET /v1/beads → 401 with
#      WWW-Authenticate: Bearer error="invalid_token" and a
#      refresh_endpoint hint pointing at /v1/auth/refresh.
#   4. POST /v1/auth/refresh with the stored refresh token → 200 +
#      fresh access_token.
#   5. GET /v1/beads with the fresh access token → 200.
#
# Skips gracefully when the admin token isn't reachable (staging
# clusters that do not expose kd-beads-token to the e2e job).

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
TTL_SECONDS="${ROTATION_TTL_SECONDS:-20}"
WAIT_PADDING_SECONDS=3
PASS=0
FAIL=0
ERRORS=()
PF_PID=""
TEST_TAG="bridge-rotate-$$-$(date +%s)"
FLEET_NAME="bridge-rotation-e2e-${TEST_TAG}"
ACCESS_TOKEN=""
REFRESH_TOKEN=""

# ── Helpers ───────────────────────────────────────────────────────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

pass() { green "  ✓ $1"; PASS=$((PASS + 1)); }
fail() {
  red "  ✗ $1"
  [ -n "${2:-}" ] && dim "    $2"
  FAIL=$((FAIL + 1))
  ERRORS+=("$1: ${2:-}")
}

# ── Setup: port-forward kbeads daemon ────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE} kbeads daemon..."
  local port=19140
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-bridge-rotation.log 2>&1 &
  PF_PID=$!
  sleep 3
  export BEADS_HTTP_URL="http://localhost:${port}"
  dim "  Daemon at $BEADS_HTTP_URL (pid $PF_PID)"
}

# ── Setup: discover admin token ──────────────────────────────────
setup_admin_token() {
  if [ -n "${BEADS_ADMIN_TOKEN:-}" ]; then
    return 0
  fi
  if [ -n "${BEADS_TOKEN:-}" ]; then
    export BEADS_ADMIN_TOKEN="$BEADS_TOKEN"
    return 0
  fi
  local secret
  secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
    -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
  if [ -n "$secret" ]; then
    export BEADS_ADMIN_TOKEN="$secret"
    dim "  Admin token loaded from secret kd-beads-token"
    return 0
  fi
  return 1
}

# ── Cleanup ───────────────────────────────────────────────────────
cleanup() {
  # Best-effort delete the fleet (releases any lingering tokens server-side).
  if [ -n "${BEADS_ADMIN_TOKEN:-}" ] && [ -n "$FLEET_NAME" ]; then
    curl -sf -X DELETE \
      -H "Authorization: Bearer $BEADS_ADMIN_TOKEN" \
      "${BEADS_HTTP_URL}/v1/fleet-tokens/${FLEET_NAME}" >/dev/null 2>&1 || true
  fi
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# ── Test phases ───────────────────────────────────────────────────
register_fleet_token() {
  blue "Step 1: register fleet token (ttl=${TTL_SECONDS}s, with_refresh=true)"

  local plaintext="bridge-rot-${TEST_TAG}"
  local body http_code response
  body=$(python3 -c "
import json,sys
print(json.dumps({
  'token': '${plaintext}',
  'fleet': '${FLEET_NAME}',
  'subject': 'bridge:test',
  'ttl':   '${TTL_SECONDS}s',
  'with_refresh': True,
}))
")
  response=$(mktemp)
  http_code=$(curl -s -o "$response" -w '%{http_code}' \
    -X POST "${BEADS_HTTP_URL}/v1/fleet-tokens" \
    -H "Authorization: Bearer ${BEADS_ADMIN_TOKEN}" \
    -H "Content-Type: application/json" \
    -d "$body")

  if [ "$http_code" != "201" ]; then
    fail "register fleet token" "expected HTTP 201, got $http_code; body: $(cat "$response")"
    rm -f "$response"
    return 1
  fi
  pass "register fleet token (HTTP 201)"

  REFRESH_TOKEN=$(python3 -c "import json,sys; print(json.load(open('${response}')).get('refresh_token',''))")
  ACCESS_TOKEN="$plaintext"
  rm -f "$response"
  if [ -z "$REFRESH_TOKEN" ]; then
    fail "refresh_token in response" "response did not include refresh_token"
    return 1
  fi
  pass "refresh_token returned"
}

check_authed() {
  local label="$1"
  local token="$2"
  local expected="$3"
  local hdr_file body_file http_code
  hdr_file=$(mktemp)
  body_file=$(mktemp)
  http_code=$(curl -s -o "$body_file" -D "$hdr_file" -w '%{http_code}' \
    -H "Authorization: Bearer $token" \
    "${BEADS_HTTP_URL}/v1/beads?limit=1")

  if [ "$http_code" = "$expected" ]; then
    pass "$label (HTTP $expected)"
  else
    fail "$label" "expected HTTP $expected, got $http_code; body: $(head -c 400 "$body_file")"
  fi

  # Emit WWW-Authenticate header to stdout for downstream assertions.
  grep -i '^www-authenticate:' "$hdr_file" | head -n1 || true
  rm -f "$hdr_file" "$body_file"
}

verify_pre_expiry() {
  blue "Step 2: positive control — access token authenticates"
  check_authed "pre-expiry GET /v1/beads" "$ACCESS_TOKEN" "200" >/dev/null
}

verify_expired_401() {
  blue "Step 3: wait $((TTL_SECONDS + WAIT_PADDING_SECONDS))s past TTL → expect 401 invalid_token"
  sleep "$((TTL_SECONDS + WAIT_PADDING_SECONDS))"

  local www_auth
  www_auth=$(check_authed "post-expiry GET /v1/beads" "$ACCESS_TOKEN" "401")

  if grep -q 'error="invalid_token"' <<<"$www_auth"; then
    pass "WWW-Authenticate contains error=\"invalid_token\""
  else
    fail "WWW-Authenticate has invalid_token" "got: $www_auth"
  fi
  if grep -q '/v1/auth/refresh' <<<"$www_auth"; then
    pass "WWW-Authenticate hints at /v1/auth/refresh"
  else
    fail "WWW-Authenticate hints at refresh endpoint" "got: $www_auth"
  fi
}

refresh_access_token() {
  blue "Step 4: exchange refresh token at /v1/auth/refresh"

  local response http_code body
  body=$(python3 -c "import json; print(json.dumps({'refresh_token': '${REFRESH_TOKEN}'}))")
  response=$(mktemp)
  http_code=$(curl -s -o "$response" -w '%{http_code}' \
    -X POST "${BEADS_HTTP_URL}/v1/auth/refresh" \
    -H "Content-Type: application/json" \
    -d "$body")

  if [ "$http_code" != "200" ]; then
    fail "refresh exchange" "expected HTTP 200, got $http_code; body: $(cat "$response")"
    rm -f "$response"
    return 1
  fi
  pass "refresh exchange (HTTP 200)"

  local new_access token_type
  new_access=$(python3 -c "import json; print(json.load(open('${response}')).get('access_token',''))")
  token_type=$(python3 -c "import json; print(json.load(open('${response}')).get('token_type',''))")
  rm -f "$response"

  if [ -z "$new_access" ]; then
    fail "fresh access_token returned" "empty access_token in refresh response"
    return 1
  fi
  pass "fresh access_token returned"

  if [ "$token_type" = "Bearer" ]; then
    pass "token_type=Bearer"
  else
    fail "token_type=Bearer" "got: $token_type"
  fi

  ACCESS_TOKEN="$new_access"
}

verify_post_refresh() {
  blue "Step 5: fresh access token authenticates"
  check_authed "post-refresh GET /v1/beads" "$ACCESS_TOKEN" "200" >/dev/null
}

# ── Main ──────────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " bridge-fleet-token-rotation E2E"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  if ! setup_admin_token; then
    red "SKIP: no admin token available (kd-beads-token secret missing)."
    red "  Cannot register fleet tokens without admin auth."
    return 0
  fi

  # Probe the daemon health endpoint to make sure we're talking to it.
  if ! curl -sf "${BEADS_HTTP_URL}/v1/health" >/dev/null 2>&1; then
    red "ERROR: kbeads daemon at $BEADS_HTTP_URL not reachable"
    exit 1
  fi
  green "  Daemon reachable: $BEADS_HTTP_URL"
  echo

  register_fleet_token   || true
  verify_pre_expiry      || true
  verify_expired_401     || true
  refresh_access_token   || true
  verify_post_refresh    || true

  echo
  blue "═══════════════════════════════════════════════════"
  local total=$((PASS + FAIL))
  if [ "$FAIL" -eq 0 ]; then
    green " ✓ All $total assertions passed"
  else
    red " ✗ $FAIL/$total assertions failed"
    echo
    for err in "${ERRORS[@]}"; do
      red "   - $err"
    done
  fi
  blue "═══════════════════════════════════════════════════"

  [ "$FAIL" -eq 0 ]
}

main "$@"
