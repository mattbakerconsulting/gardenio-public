#!/usr/bin/env bash
# test-hooks-system.sh — hooks API E2E tests
#
# Tests POST /v1/hooks/emit, /v1/hooks/execute, /v1/hooks/publish against
# a live kbeads server. The hooks system drives agent lifecycle events
# (SessionStart, Stop, PreCompact, UserPromptSubmit).
#
# Usage:
#   ./tests/e2e/scripts/test-hooks-system.sh
#
# Prerequisites:
#   - kd binary in PATH (or set KD_BIN)
#   - curl, python3 installed
#   - BEADS_HTTP_URL set (or kubectl context for port-forward)
#
# Scenarios tested:
#   1. POST /v1/hooks/emit with SessionStart — non-blocking response
#   2. POST /v1/hooks/emit with UserPromptSubmit — non-blocking response
#   3. POST /v1/hooks/emit with PreCompact — non-blocking response
#   4. POST /v1/hooks/execute with session-end trigger
#   5. POST /v1/hooks/publish — publish event to bus (handles NATS unavailable)
#   6. Verify emit with missing fields returns 400

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
KD_BIN="${KD_BIN:-kd}"
PASS=0
FAIL=0
ERRORS=()
PF_PID=""
CREATED_BEADS=()

export KD_ACTOR="${KD_ACTOR:-e2e-test}"

# ── Helpers ───────────────────────────────────────────────────────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

pass() {
  local name="$1"
  green "  ✓ $name"
  PASS=$((PASS + 1))
}

fail() {
  local name="$1"
  local reason="${2:-}"
  red "  ✗ $name"
  [ -n "$reason" ] && dim "    $reason"
  FAIL=$((FAIL + 1))
  ERRORS+=("$name: $reason")
}

assert_exit() {
  local name="$1"
  local expected="$2"
  local actual="$3"
  if [ "$actual" -eq "$expected" ]; then
    pass "$name (exit $expected)"
  else
    fail "$name" "expected exit $expected, got exit $actual"
  fi
}

assert_contains() {
  local name="$1"
  local pattern="$2"
  local actual="$3"
  if grep -qF -- "$pattern" <<< "$actual"; then
    pass "$name (contains '$pattern')"
  else
    fail "$name" "expected output to contain '$pattern'; got: $(echo "$actual" | head -5)"
  fi
}

assert_eq() {
  local name="$1"
  local expected="$2"
  local actual="$3"
  if [ "$expected" = "$actual" ]; then
    pass "$name (== '$expected')"
  else
    fail "$name" "expected '$expected', got '$actual'"
  fi
}

# ── Setup: port-forward kd server ────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi

  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19099
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-hooks.log 2>&1 &
  PF_PID=$!
  sleep 3

  export BEADS_HTTP_URL="http://localhost:${port}"
  dim "  Daemon at $BEADS_HTTP_URL (pid $PF_PID)"
}

# ── Setup: get token ─────────────────────────────────────────────
setup_token() {
  if [ -n "${BEADS_TOKEN:-}" ]; then
    return
  fi
  local secret
  secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
    -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
  if [ -n "$secret" ]; then
    export BEADS_TOKEN="$secret"
    dim "  Token loaded from secret kd-beads-token"
  else
    dim "  WARNING: No token found; kd server may reject requests"
  fi
}

# ── Cleanup ───────────────────────────────────────────────────────
cleanup() {
  for id in "${CREATED_BEADS[@]}"; do
    "$KD_BIN" close "$id" --actor "$KD_ACTOR" 2>/dev/null || true
  done
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# ── Helpers: HTTP API calls ──────────────────────────────────────
api_post() {
  local path="$1"
  local body="$2"
  local auth_header=""
  if [ -n "${BEADS_TOKEN:-}" ]; then
    auth_header="Authorization: Bearer $BEADS_TOKEN"
  fi
  curl -sf -X POST -H "Content-Type: application/json" \
    ${auth_header:+-H "$auth_header"} \
    -d "$body" \
    "${BEADS_HTTP_URL}${path}" 2>/dev/null || echo "{}"
}

api_post_status() {
  local path="$1"
  local body="$2"
  local auth_header=""
  if [ -n "${BEADS_TOKEN:-}" ]; then
    auth_header="Authorization: Bearer $BEADS_TOKEN"
  fi
  curl -s -o /dev/null -w "%{http_code}" -X POST \
    -H "Content-Type: application/json" \
    ${auth_header:+-H "$auth_header"} \
    -d "$body" \
    "${BEADS_HTTP_URL}${path}" 2>/dev/null || echo "000"
}

api_post_full() {
  local path="$1"
  local body="$2"
  local auth_header=""
  if [ -n "${BEADS_TOKEN:-}" ]; then
    auth_header="Authorization: Bearer $BEADS_TOKEN"
  fi
  curl -s -w "\n%{http_code}" -X POST \
    -H "Content-Type: application/json" \
    ${auth_header:+-H "$auth_header"} \
    -d "$body" \
    "${BEADS_HTTP_URL}${path}" 2>/dev/null || echo -e "{}\n000"
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " hooks-system E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  # Verify kd server is reachable
  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  local TEST_TAG="e2e-hooks-$$-$(date +%s)"

  # ── Scenario 1: Emit SessionStart hook ─────────────────────────
  blue "Scenario 1: POST /v1/hooks/emit — SessionStart"

  local emit_out emit_status
  emit_out=$(api_post_full "/v1/hooks/emit" \
    "{\"hook_type\":\"SessionStart\",\"actor\":\"$KD_ACTOR\",\"claude_session_id\":\"e2e-$TEST_TAG\",\"cwd\":\"/tmp\"}")

  emit_status=$(echo "$emit_out" | tail -1)
  local emit_body
  emit_body=$(echo "$emit_out" | sed '$d')

  assert_eq "SessionStart returns HTTP 200" "200" "$emit_status"

  # Verify response is not blocking
  local is_blocked
  is_blocked=$(echo "$emit_body" | python3 -c "import sys,json; print(json.load(sys.stdin).get('block', False))" 2>/dev/null || echo "error")
  assert_eq "SessionStart does not block" "False" "$is_blocked"
  echo

  # ── Scenario 2: Emit UserPromptSubmit hook ─────────────────────
  blue "Scenario 2: POST /v1/hooks/emit — UserPromptSubmit"

  emit_out=$(api_post_full "/v1/hooks/emit" \
    "{\"hook_type\":\"UserPromptSubmit\",\"actor\":\"$KD_ACTOR\",\"claude_session_id\":\"e2e-$TEST_TAG\",\"cwd\":\"/tmp\"}")

  emit_status=$(echo "$emit_out" | tail -1)
  emit_body=$(echo "$emit_out" | sed '$d')

  assert_eq "UserPromptSubmit returns HTTP 200" "200" "$emit_status"

  is_blocked=$(echo "$emit_body" | python3 -c "import sys,json; print(json.load(sys.stdin).get('block', False))" 2>/dev/null || echo "error")
  assert_eq "UserPromptSubmit does not block" "False" "$is_blocked"
  echo

  # ── Scenario 3: Emit PreCompact hook ───────────────────────────
  blue "Scenario 3: POST /v1/hooks/emit — PreCompact"

  emit_out=$(api_post_full "/v1/hooks/emit" \
    "{\"hook_type\":\"PreCompact\",\"actor\":\"$KD_ACTOR\",\"claude_session_id\":\"e2e-$TEST_TAG\",\"cwd\":\"/tmp\"}")

  emit_status=$(echo "$emit_out" | tail -1)
  emit_body=$(echo "$emit_out" | sed '$d')

  assert_eq "PreCompact returns HTTP 200" "200" "$emit_status"

  is_blocked=$(echo "$emit_body" | python3 -c "import sys,json; print(json.load(sys.stdin).get('block', False))" 2>/dev/null || echo "error")
  assert_eq "PreCompact does not block" "False" "$is_blocked"
  echo

  # ── Scenario 4: Execute hooks for session-end trigger ──────────
  blue "Scenario 4: POST /v1/hooks/execute — session-end trigger"

  local exec_out exec_status
  exec_out=$(api_post_full "/v1/hooks/execute" \
    "{\"agent_id\":\"$KD_ACTOR\",\"trigger\":\"session-end\",\"cwd\":\"/tmp\"}")

  exec_status=$(echo "$exec_out" | tail -1)
  local exec_body
  exec_body=$(echo "$exec_out" | sed '$d')

  assert_eq "execute returns HTTP 200" "200" "$exec_status"

  # Verify we get a valid JSON response
  local exec_valid
  exec_valid=$(echo "$exec_body" | python3 -c "import sys,json; d=json.load(sys.stdin); print('valid')" 2>/dev/null || echo "invalid")
  assert_eq "execute response is valid JSON" "valid" "$exec_valid"
  echo

  # ── Scenario 5: Publish hook event to bus ──────────────────────
  blue "Scenario 5: POST /v1/hooks/publish"

  local pub_status
  pub_status=$(api_post_status "/v1/hooks/publish" \
    "{\"subject\":\"hooks.e2e-test.$TEST_TAG\",\"payload\":{\"test\":true,\"tag\":\"$TEST_TAG\"}}")

  if [ "$pub_status" = "204" ]; then
    pass "publish returns HTTP 204 (event published)"
  elif [ "$pub_status" = "503" ]; then
    pass "publish returns HTTP 503 (NATS not configured — expected in some environments)"
  else
    fail "publish returns expected status" "got HTTP $pub_status (expected 204 or 503)"
  fi
  echo

  # ── Scenario 6: Emit with missing fields returns 400 ───────────
  blue "Scenario 6: Validation — missing/invalid fields"

  # Execute with missing agent_id
  local bad_exec_status
  bad_exec_status=$(api_post_status "/v1/hooks/execute" \
    '{"trigger":"session-end"}')
  assert_eq "execute without agent_id returns 400" "400" "$bad_exec_status"

  # Execute with missing trigger
  bad_exec_status=$(api_post_status "/v1/hooks/execute" \
    '{"agent_id":"test"}')
  assert_eq "execute without trigger returns 400" "400" "$bad_exec_status"

  # Publish with missing subject
  local bad_pub_status
  bad_pub_status=$(api_post_status "/v1/hooks/publish" \
    '{"payload":{"test":true}}')
  assert_eq "publish without subject returns 400" "400" "$bad_pub_status"

  # Publish with missing payload
  bad_pub_status=$(api_post_status "/v1/hooks/publish" \
    '{"subject":"hooks.test"}')
  assert_eq "publish without payload returns 400" "400" "$bad_pub_status"

  # Emit with invalid JSON
  local bad_emit_status
  bad_emit_status=$(api_post_status "/v1/hooks/emit" "not json at all")
  assert_eq "emit with invalid JSON returns 400" "400" "$bad_emit_status"
  echo

  # ── Summary ─────────────────────────────────────────────────────
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
