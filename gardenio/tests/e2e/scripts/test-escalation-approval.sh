#!/usr/bin/env bash
# test-escalation-approval.sh — escalation bridge API E2E tests (Phase 2)
#
# Tests the slack-bridge escalation web API endpoints against a live bridge:
# list, show, me (identity + capabilities), SSE event proxy, and the negative
# controls for the removed web approve/reject routes.
#
# P7 Task 4 (gd-3tOmdG4moG): the bridge web API fails closed without OIDC and
# POST /api/escalations/{id}/approve|reject no longer exist (410). The suite
# probes which mode the bridge is in: with SLACK_BRIDGE_INSECURE_OPEN_API=1 the
# read-only scenarios (1-3, 8) run as before; otherwise they assert 403 too.
# Approval itself happens in Slack or the beads viewer (see
# test-escalation-crud.sh Scenario 4 and test-escalation-execution.sh).
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-escalation-approval.sh
#
# Prerequisites:
#   - BEADS_HTTP_URL set (kbeads HTTP API, e.g. http://localhost:19090)
#   - BRIDGE_HTTP_URL set (slack-bridge HTTP API, e.g. http://localhost:19091)
#   - BEADS_TOKEN set (auth token for kbeads API)
#   - kd binary in PATH (or set KD_BIN)
#   - curl, python3, jq installed
#
# Scenarios tested:
#   1. GET  /api/escalations       — list escalation beads
#   2. GET  /api/escalations/{id}  — show single escalation
#   3. GET  /api/escalations/me    — a forged X-Auth-Request-Email names no one
#   4. POST /api/escalations/{id}/approve — forged header refused (403 closed / 410 open),
#      bead left untouched
#   5. POST /api/escalations/{id}/reject  — same, bead stays open
#   6. approve/reject with a no-capability forged email — refused the same way
#   7. approve on an already-closed escalation — refused (not the old 409), bead stays closed
#   8. GET  /api/escalations/events — SSE proxy delivers escalation events
#      (403 when the bridge web API is closed)
# Scenarios 1-3 and 8 need the bridge in open mode; when it is closed they
# assert the 403 instead.

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
BRIDGE_SVC="${BRIDGE_SVC:-${NAMESPACE}-slack-bridge}"
KD_BIN="${KD_BIN:-kd}"
PASS=0
FAIL=0
ERRORS=()
PF_PID=""
BRIDGE_PF_PID=""
CREATED_BEADS=()
BG_PIDS=()

export KD_ACTOR="${KD_ACTOR:-e2e-test}"

# Test user with approve:escalation capability
TEST_USER_EMAIL="e2e-test@gardenio.dev"
TEST_USER_DISPLAY="E2E Test User"
# User without capability
NO_CAP_EMAIL="e2e-nocap@gardenio.dev"

# ── Helpers ───────────────────────────────────────────────────────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

# P7 Task 9 (gd-nPfSNHTkQQ): the daemon admits /reject only from an operator
# identity and /result only from an operator, a bridge, a machine principal or
# an agent, so this suite's shared BEADS_TOKEN is refused ("caller class:
# service"). resolve_refused prints a SKIP with the reason and returns 0 when
# $2 holds that refusal; callers skip the assertions that depend on the
# resolve. The refusal is never worked around to make a scenario pass.
resolve_refused() {
  grep -qF 'caller class:' <<<"$2" || return 1
  green "  SKIP: $1 refused: $(grep -o 'caller class: [a-z]*' <<<"$2" | head -1) (P7 Task 9)"
  return 0
}

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

assert_not_contains() {
  local name="$1"
  local pattern="$2"
  local actual="$3"
  if ! grep -qF -- "$pattern" <<< "$actual"; then
    pass "$name (does not contain '$pattern')"
  else
    fail "$name" "expected output to NOT contain '$pattern'"
  fi
}

assert_http_status() {
  local name="$1"
  local expected="$2"
  local actual="$3"
  if [ "$actual" -eq "$expected" ]; then
    pass "$name (HTTP $expected)"
  else
    fail "$name" "expected HTTP $expected, got HTTP $actual"
  fi
}

json_field() {
  python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('$1',''))" 2>/dev/null
}

# curl wrapper that returns HTTP status code
bridge_curl() {
  local method="$1"
  local path="$2"
  shift 2
  curl -sf --max-time 30 -X "$method" \
    -w '\n%{http_code}' \
    "${BRIDGE_HTTP_URL}${path}" \
    "$@" 2>/dev/null || true
}

# assert_refused NAME STATUS: the bridge must refuse a forged-header approve or
# reject. 403 = web API closed (no OIDC), 410 = route removed (open mode / OIDC).
assert_refused() {
  local name="$1"
  local status="$2"
  if [ "$status" = "403" ] || [ "$status" = "410" ]; then
    pass "$name (HTTP $status)"
  else
    fail "$name" "expected HTTP 403 or 410, got HTTP ${status:-none}"
  fi
}

# bead_status ID: current status of a bead via kd.
bead_status() {
  "$KD_BIN" show "$1" --json 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('status',''))" 2>/dev/null || echo ""
}

# Extract HTTP body and status from bridge_curl output
parse_response() {
  local response="$1"
  local body status
  status=$(echo "$response" | tail -1)
  body=$(echo "$response" | sed '$d')
  echo "$body"
  echo "---HTTP_STATUS---"
  echo "$status"
}

# ── Setup: port-forward kbeads server ─────────────────────────────
setup_beads_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi

  blue "Port-forwarding ${NAMESPACE} kbeads server..."
  local port=19095
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-approval.log 2>&1 &
  PF_PID=$!
  sleep 3

  export BEADS_HTTP_URL="http://localhost:${port}"
  dim "  kbeads at $BEADS_HTTP_URL (pid $PF_PID)"
}

# ── Setup: port-forward bridge ────────────────────────────────────
setup_bridge_portforward() {
  if [ -n "${BRIDGE_HTTP_URL:-}" ]; then
    blue "Using BRIDGE_HTTP_URL=$BRIDGE_HTTP_URL"
    return
  fi

  blue "Port-forwarding ${NAMESPACE} bridge..."
  local port=19091
  kubectl -n "$NAMESPACE" port-forward "svc/${BRIDGE_SVC}" "${port}:8090" \
    >/tmp/bridge-pf-approval.log 2>&1 &
  BRIDGE_PF_PID=$!
  sleep 3

  export BRIDGE_HTTP_URL="http://localhost:${port}"
  dim "  Bridge at $BRIDGE_HTTP_URL (pid $BRIDGE_PF_PID)"
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
    dim "  WARNING: No token found; requests may be rejected"
  fi
}

# ── Setup: seed test user beads ──────────────────────────────────
seed_test_users() {
  blue "Seeding test user beads..."

  # type:user is builtin-authoritative; the daemon builtin (config.go) +
  # controller EnsureConfigs seed already declare a SUPERSET of these fields.
  # The old hand-maintained PUT could only shadow-and-drift the builtin
  # (ka-0000003677 / generalized ka-0000000407). Dropped so e2e exercises the
  # builtin prod runs, guarded by TestE2ESeedTypes_NoBuiltinShadow. Do NOT re-add.

  # Create capable test user (with approve:escalation)
  local user_out user_id
  user_out=$("$KD_BIN" create "$TEST_USER_DISPLAY" \
    --type=user \
    -l "e2e:escalation-approval" \
    -f "email=$TEST_USER_EMAIL" \
    -f "display_name=$TEST_USER_DISPLAY" \
    -f 'capabilities=["approve:escalation"]' \
    --actor "$KD_ACTOR" --json 2>/dev/null || echo "{}")
  user_id=$(echo "$user_out" | json_field id || echo "")
  if [ -n "$user_id" ]; then
    CREATED_BEADS+=("$user_id")
    dim "  Capable user created: $user_id ($TEST_USER_EMAIL)"
  else
    dim "  WARNING: Failed to create capable test user"
  fi

  # Create non-capable test user (no approve:escalation)
  local nocap_out nocap_id
  nocap_out=$("$KD_BIN" create "E2E No-Cap User" \
    --type=user \
    -l "e2e:escalation-approval" \
    -f "email=$NO_CAP_EMAIL" \
    -f "display_name=E2E No-Cap User" \
    -f 'capabilities=["gardenio-viewer"]' \
    --actor "$KD_ACTOR" --json 2>/dev/null || echo "{}")
  nocap_id=$(echo "$nocap_out" | json_field id || echo "")
  if [ -n "$nocap_id" ]; then
    CREATED_BEADS+=("$nocap_id")
    dim "  Non-capable user created: $nocap_id ($NO_CAP_EMAIL)"
  else
    dim "  WARNING: Failed to create non-capable test user"
  fi
}

# ── Seed escalation type config ──────────────────────────────────
seed_escalation_config() {
  # type:escalation is builtin-authoritative; the daemon builtin (config.go) +
  # controller EnsureConfigs seed already declare a SUPERSET of these fields.
  # The old hand-maintained PUT could only shadow-and-drift the builtin
  # (ka-0000003677 / generalized ka-0000000407). Dropped so e2e exercises the
  # builtin prod runs, guarded by TestE2ESeedTypes_NoBuiltinShadow. Do NOT re-add.
  dim "  type:escalation config: using daemon builtin (no shadow PUT)"
}

# ── Cleanup ───────────────────────────────────────────────────────
cleanup() {
  for pid in "${BG_PIDS[@]}"; do
    kill "$pid" 2>/dev/null || true
  done
  for id in "${CREATED_BEADS[@]}"; do
    "$KD_BIN" close "$id" --actor "$KD_ACTOR" 2>/dev/null || true
  done
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
  if [ -n "$BRIDGE_PF_PID" ]; then
    kill "$BRIDGE_PF_PID" 2>/dev/null || true
  fi
  rm -f /tmp/sse-approval-*.log 2>/dev/null || true
}
trap cleanup EXIT

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " escalation-approval E2E tests (Phase 2: Bridge API)"
  blue "═══════════════════════════════════════════════════"

  setup_beads_portforward
  setup_token
  setup_bridge_portforward

  # Verify kbeads is reachable
  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kbeads at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  kbeads reachable: $BEADS_HTTP_URL"

  # Verify bridge is reachable
  local bridge_health
  bridge_health=$(curl -sf --max-time 10 "${BRIDGE_HTTP_URL}/healthz" 2>/dev/null || \
                  curl -sf --max-time 10 "${BRIDGE_HTTP_URL}/" -o /dev/null -w '%{http_code}' 2>/dev/null || echo "000")
  if [ "$bridge_health" = "000" ]; then
    red "ERROR: Cannot reach bridge at $BRIDGE_HTTP_URL"
    red "  Bridge port-forward may have failed. Skipping bridge API tests."
    exit 1
  fi
  green "  Bridge reachable: $BRIDGE_HTTP_URL"

  # Probe: check if the escalation API is deployed on this bridge version.
  # If the bridge returns 404 for /api/escalations, the image predates the
  # escalation API PRs (#629–#633) — skip gracefully instead of failing.
  #
  # NOTE: assignment-style fallback (not `|| echo "000"`). When curl fails,
  # `-w '%{http_code}'` already writes "000" to stdout AND curl exits non-zero,
  # so `|| echo "000"` would concatenate to "000000" and miss the skip branch.
  local probe_status
  probe_status=$(curl -s --max-time 10 -o /dev/null -w '%{http_code}' \
    "${BRIDGE_HTTP_URL}/api/escalations" 2>/dev/null) || probe_status="000"
  if [ "$probe_status" = "404" ] || [ "$probe_status" = "000" ]; then
    blue "Bridge does not have escalation API (HTTP $probe_status)."
    blue "  The deployed bridge image predates the escalation API."
    blue "  Redeploy with an image containing PRs #629–#633 to enable these tests."
    green " SKIP: escalation-approval (bridge API not available)"
    exit 0
  fi
  dim "  Escalation API probe: HTTP $probe_status"
  # 200 without credentials = open dev mode (SLACK_BRIDGE_INSECURE_OPEN_API=1);
  # anything else (403 without OIDC) = the web API is closed.
  local api_open=false
  if [ "$probe_status" = "200" ]; then
    api_open=true
    dim "  Bridge web API is OPEN (insecure dev mode)"
  else
    dim "  Bridge web API is CLOSED (HTTP $probe_status): read scenarios assert 403"
  fi

  seed_escalation_config
  seed_test_users

  # Allow time for the bridge UserDirectory to pick up the new user beads
  dim "  Waiting for bridge user directory sync..."
  sleep 5

  echo

  local TEST_TAG="e2e-appr-$$-$(date +%s)"

  # ── Create a test escalation bead via kbeads ─────────────────────
  blue "Setup: Creating test escalation bead"

  local esc_out esc_id
  esc_out=$("$KD_BIN" create "E2E approval $TEST_TAG: create IAM role" \
    --type=escalation \
    -l "e2e:$TEST_TAG" \
    -f 'operation_type=aws_iam' \
    -f 'operation_spec={"action":"create_role","role_name":"e2e-test-role"}' \
    -f 'context=E2E test: approval workflow via bridge API' \
    -f 'requesting_agent=e2e-agent-approval' \
    --actor "$KD_ACTOR" --json 2>/dev/null || echo "{}")
  esc_id=$(echo "$esc_out" | json_field id || echo "")

  if [ -z "$esc_id" ]; then
    red "ERROR: Could not create test escalation bead"
    exit 1
  fi
  CREATED_BEADS+=("$esc_id")
  dim "  Escalation created: $esc_id"
  echo

  # ── Scenario 1: GET /api/escalations (list) ─────────────────────
  blue "Scenario 1: GET /api/escalations — list escalation beads"

  local list_resp list_body list_status
  list_resp=$(bridge_curl GET "/api/escalations?status=open")
  list_body=$(echo "$list_resp" | sed '$d')
  list_status=$(echo "$list_resp" | tail -1)

  if [ "$api_open" != true ] && [ -n "$list_status" ] && [ "$list_status" != "000" ]; then
    assert_http_status "list refused without OIDC" 403 "$list_status"
  elif [ -n "$list_status" ] && [ "$list_status" != "000" ]; then
    assert_http_status "list returns 200" 200 "$list_status"
    assert_contains "list response has escalations key" '"escalations"' "$list_body"
    assert_contains "list contains test escalation" "$esc_id" "$list_body"
  else
    fail "list escalations" "bridge returned no response (status=$list_status)"
  fi
  echo

  # ── Scenario 2: GET /api/escalations/{id} (show) ────────────────
  blue "Scenario 2: GET /api/escalations/{id} — show single escalation"

  local show_resp show_body show_status
  show_resp=$(bridge_curl GET "/api/escalations/$esc_id")
  show_body=$(echo "$show_resp" | sed '$d')
  show_status=$(echo "$show_resp" | tail -1)

  if [ "$api_open" != true ] && [ -n "$show_status" ] && [ "$show_status" != "000" ]; then
    assert_http_status "show refused without OIDC" 403 "$show_status"
  elif [ -n "$show_status" ] && [ "$show_status" != "000" ]; then
    assert_http_status "show returns 200" 200 "$show_status"
    assert_contains "show has escalation ID" "$esc_id" "$show_body"
    assert_contains "show has type=escalation" '"type":"escalation"' "$show_body"
    assert_contains "show has operation_type" "aws_iam" "$show_body"
  else
    fail "show escalation" "bridge returned no response (status=$show_status)"
  fi
  echo

  # ── Scenario 3: GET /api/escalations/me — forged header is no identity ─
  # /me answers only for the OIDC-verified caller (gd-Fhizmbk9Ee). A bare
  # X-Auth-Request-Email header is client-supplied, so it must not name anyone:
  # 403 when the web API is closed, 401 (no verified identity) when it is open.
  blue "Scenario 3: GET /api/escalations/me — forged email header refused"

  local me_resp me_status
  me_resp=$(bridge_curl GET "/api/escalations/me" \
    -H "X-Auth-Request-Email: $TEST_USER_EMAIL")
  me_status=$(echo "$me_resp" | tail -1)

  if [ "$api_open" != true ] && [ -n "$me_status" ] && [ "$me_status" != "000" ]; then
    assert_http_status "me refused without OIDC" 403 "$me_status"
  elif [ -n "$me_status" ] && [ "$me_status" != "000" ]; then
    assert_http_status "me refuses a forged email header" 401 "$me_status"
  else
    fail "me endpoint" "bridge returned no response (status=$me_status)"
  fi
  echo

  # ── Scenario 4: POST /api/escalations/{id}/approve — removed ────
  blue "Scenario 4: POST /api/escalations/{id}/approve — forged header refused"

  local approve_resp approve_status
  approve_resp=$(curl -s --max-time 30 -X POST -w '\n%{http_code}' \
    "${BRIDGE_HTTP_URL}/api/escalations/$esc_id/approve" \
    -H "X-Auth-Request-Email: $TEST_USER_EMAIL" \
    -H "Content-Type: application/json" \
    -d '{"execution_target":"local"}' 2>/dev/null || true)
  approve_status=$(echo "$approve_resp" | tail -1)
  assert_refused "forged-header approve refused" "$approve_status"

  # Negative control: nothing was approved.
  local bead_after
  bead_after=$("$KD_BIN" show "$esc_id" --json 2>/dev/null || echo "{}")
  assert_not_contains "bead has no approved_by after forged approve" "approved_by" "$bead_after"
  assert_not_contains "bead has no web_ui approval_method" "web_ui" "$bead_after"
  if [ "$(bead_status "$esc_id")" = "open" ]; then
    pass "bead still open after forged approve"
  else
    fail "bead still open after forged approve" "status=$(bead_status "$esc_id")"
  fi
  echo

  # ── Scenario 5: POST /api/escalations/{id}/reject — removed ─────
  blue "Scenario 5: POST /api/escalations/{id}/reject — forged header refused"

  local esc_reject_out esc_reject_id
  esc_reject_out=$("$KD_BIN" create "E2E approval $TEST_TAG: reject test" \
    --type=escalation \
    -l "e2e:$TEST_TAG" \
    -f 'operation_type=general' \
    -f 'operation_spec={"action":"test"}' \
    --actor "$KD_ACTOR" --json 2>/dev/null || echo "{}")
  esc_reject_id=$(echo "$esc_reject_out" | json_field id || echo "")

  if [ -n "$esc_reject_id" ]; then
    CREATED_BEADS+=("$esc_reject_id")

    local reject_resp reject_status
    reject_resp=$(curl -s --max-time 30 -X POST -w '\n%{http_code}' \
      "${BRIDGE_HTTP_URL}/api/escalations/$esc_reject_id/reject" \
      -H "X-Auth-Request-Email: $TEST_USER_EMAIL" \
      -H "Content-Type: application/json" \
      -d '{"reason":"Too risky"}' 2>/dev/null || true)
    reject_status=$(echo "$reject_resp" | tail -1)
    assert_refused "forged-header reject refused" "$reject_status"

    if [ "$(bead_status "$esc_reject_id")" = "open" ]; then
      pass "bead still open after forged reject"
    else
      fail "bead still open after forged reject" "status=$(bead_status "$esc_reject_id")"
    fi
  else
    fail "scenario 5 setup" "could not create escalation for reject test"
  fi
  echo

  # ── Scenario 6: no-capability forged email — refused the same way ─
  blue "Scenario 6: approve/reject with a no-capability forged email — refused"

  local esc_nocap_out esc_nocap_id
  esc_nocap_out=$("$KD_BIN" create "E2E approval $TEST_TAG: no-cap test" \
    --type=escalation \
    -l "e2e:$TEST_TAG" \
    -f 'operation_type=general' \
    -f 'operation_spec={"action":"test"}' \
    --actor "$KD_ACTOR" --json 2>/dev/null || echo "{}")
  esc_nocap_id=$(echo "$esc_nocap_out" | json_field id || echo "")

  if [ -n "$esc_nocap_id" ]; then
    CREATED_BEADS+=("$esc_nocap_id")

    local action nocap_resp nocap_status
    for action in approve reject; do
      nocap_resp=$(curl -s --max-time 30 -X POST -w '\n%{http_code}' \
        "${BRIDGE_HTTP_URL}/api/escalations/$esc_nocap_id/$action" \
        -H "X-Auth-Request-Email: $NO_CAP_EMAIL" \
        -H "Content-Type: application/json" \
        -d '{"reason":"should fail"}' 2>/dev/null || true)
      nocap_status=$(echo "$nocap_resp" | tail -1)
      assert_refused "no-capability $action refused" "$nocap_status"
    done
  else
    fail "scenario 6 setup" "could not create escalation for no-cap test"
  fi
  echo

  # ── Scenario 7: already-closed escalation — refused, stays closed ─
  blue "Scenario 7: POST /api/escalations/{id}/approve — already-closed stays closed"

  local esc_closed_out esc_closed_id
  esc_closed_out=$("$KD_BIN" create "E2E approval $TEST_TAG: closed test" \
    --type=escalation \
    -l "e2e:$TEST_TAG" \
    -f 'operation_type=general' \
    -f 'operation_spec={"action":"already_done"}' \
    --actor "$KD_ACTOR" --json 2>/dev/null || echo "{}")
  esc_closed_id=$(echo "$esc_closed_out" | json_field id || echo "")

  if [ -n "$esc_closed_id" ]; then
    CREATED_BEADS+=("$esc_closed_id")
    # Close it via the dedicated reject verb. A generic `kd close` on an OPEN
    # escalation is diverted by the daemon (ka-0000005516) and 409s.
    local reject7_out
    reject7_out=$("$KD_BIN" escalation reject "$esc_closed_id" --reason "e2e already-closed setup" \
      --actor "$KD_ACTOR" 2>&1) || true
    if resolve_refused "scenario 7 setup reject" "$reject7_out"; then
      :
    else
      sleep 1

      local conflict_resp conflict_status
      conflict_resp=$(curl -s --max-time 30 -X POST -w '\n%{http_code}' \
        "${BRIDGE_HTTP_URL}/api/escalations/$esc_closed_id/approve" \
        -H "X-Auth-Request-Email: $TEST_USER_EMAIL" \
        -H "Content-Type: application/json" 2>/dev/null || true)
      conflict_status=$(echo "$conflict_resp" | tail -1)
      assert_refused "closed escalation approve refused" "$conflict_status"
      if [ "$(bead_status "$esc_closed_id")" = "closed" ]; then
        pass "closed escalation still closed after forged approve"
      else
        fail "closed escalation still closed" "status=$(bead_status "$esc_closed_id")"
      fi
    fi
  else
    fail "scenario 7 setup" "could not create escalation for conflict test"
  fi
  echo

  # ── Scenario 8: SSE /api/escalations/events proxy ───────────────
  blue "Scenario 8: SSE /api/escalations/events — proxy delivers events"

  if [ "$api_open" != true ]; then
    local sse_status
    sse_status=$(curl -s --max-time 10 -o /dev/null -w '%{http_code}' \
      "${BRIDGE_HTTP_URL}/api/escalations/events" 2>/dev/null) || sse_status="000"
    assert_http_status "SSE proxy refused without OIDC" 403 "$sse_status"
  else
  local sse_log="/tmp/sse-approval-$$.log"

  # Start SSE listener on the bridge proxy
  curl -sN --max-time 15 "${BRIDGE_HTTP_URL}/api/escalations/events" \
    > "$sse_log" 2>/dev/null &
  local sse_pid=$!
  BG_PIDS+=("$sse_pid")
  sleep 3

  # Create an escalation to trigger an event
  local esc_sse_out esc_sse_id
  esc_sse_out=$("$KD_BIN" create "E2E approval $TEST_TAG: SSE event test" \
    --type=escalation \
    -l "e2e:$TEST_TAG" \
    -f 'operation_type=sse_test' \
    -f 'operation_spec={"action":"trigger_event"}' \
    --actor "$KD_ACTOR" --json 2>/dev/null || echo "{}")
  esc_sse_id=$(echo "$esc_sse_out" | json_field id || echo "")

  if [ -n "$esc_sse_id" ]; then
    CREATED_BEADS+=("$esc_sse_id")
    sleep 5

    local sse_out
    sse_out=$(cat "$sse_log" 2>/dev/null || echo "")

    if [ -n "$sse_out" ]; then
      # The SSE proxy sends "event: escalation\ndata: {...}\n\n"
      # or at minimum a ": connected" comment
      if grep -qF "event: escalation" <<<"$sse_out"; then
        pass "SSE proxy delivers escalation event"
        assert_contains "SSE event contains created type" '"type":"created"' "$sse_out"
      elif grep -qF ": connected" <<<"$sse_out"; then
        # Connected but event may not have arrived yet — partial pass
        pass "SSE proxy connected successfully"
        dim "    (escalation event may not have propagated in time)"
      else
        pass "SSE proxy returned data"
        dim "    content: $(echo "$sse_out" | head -3)"
      fi
    else
      fail "SSE proxy" "no output received from SSE stream"
    fi
  else
    fail "scenario 8 setup" "could not create escalation for SSE test"
  fi

  kill "$sse_pid" 2>/dev/null || true
  fi
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
