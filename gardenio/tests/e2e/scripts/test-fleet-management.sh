#!/usr/bin/env bash
# test-fleet-management.sh — fleet management E2E tests
#
# Tests kd fleet list/show/transfer against a live kbeads server.
# Fleet beads represent logical agent groupings; transfers move beads
# between fleets atomically.
#
# Usage:
#   ./tests/e2e/scripts/test-fleet-management.sh
#
# Prerequisites:
#   - kd binary in PATH (or set KD_BIN)
#   - curl, python3 installed
#   - BEADS_HTTP_URL set (or kubectl context for port-forward)
#
# Scenarios tested:
#   1. Create fleet beads and verify kd fleet list
#   2. Verify kd fleet show returns fleet details
#   3. Transfer a bead between fleets via kd fleet transfer
#   4. Verify kd fleet list --json output format
#   5. Verify transfer updates bead's fleet field
#   6. Verify fleet transfer event in event log

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
  local port=19100
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-fleet.log 2>&1 &
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
    "$KD_BIN" close "$id" --actor "$KD_ACTOR" 2>/dev/null || \
    "$KD_BIN" delete "$id" 2>/dev/null || true
  done
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# ── Helpers: HTTP API calls ──────────────────────────────────────
api_get() {
  local path="$1"
  local auth_header=""
  if [ -n "${BEADS_TOKEN:-}" ]; then
    auth_header="Authorization: Bearer $BEADS_TOKEN"
  fi
  curl -sf -H "Content-Type: application/json" \
    ${auth_header:+-H "$auth_header"} \
    "${BEADS_HTTP_URL}${path}" 2>/dev/null || echo "{}"
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " fleet-management E2E tests"
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

  local TEST_TAG="e2e-fleet-$$-$(date +%s)"

  # Check if fleet type is configured on this server
  local fleet_probe
  fleet_probe=$("$KD_BIN" create "probe-fleet-$$" --type=fleet \
    -f "namespace=probe" -f "token_secret=probe" --json 2>&1) || true
  if grep -q "unknown bead type" <<<"$fleet_probe"; then
    red "SKIP: kd server does not support fleet type."
    red "  Deploy a kbeads image with the fleet type configured first."
    return 0
  fi
  # Clean up probe bead
  local probe_id
  probe_id=$(echo "$fleet_probe" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  [ -n "$probe_id" ] && "$KD_BIN" close "$probe_id" 2>/dev/null || true

  # ── Scenario 1: Create fleet beads and verify kd fleet list ────
  blue "Scenario 1: Create fleet beads and verify kd fleet list"

  # Create a fleet bead
  local fleet_id
  fleet_id=$("$KD_BIN" create "E2E fleet $TEST_TAG" \
    --type=fleet -l "e2e:$TEST_TAG" \
    -f "namespace=e2e-$TEST_TAG" \
    -f "token_secret=e2e-fleet-token" \
    --actor "$KD_ACTOR" --json 2>/dev/null | \
    python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")

  if [ -z "$fleet_id" ]; then
    fail "create fleet bead" "no id returned"
    return 1
  fi
  CREATED_BEADS+=("$fleet_id")
  pass "created fleet bead ($fleet_id)"

  sleep 1  # brief delay for server indexing

  local list_out list_rc
  list_out=$("$KD_BIN" fleet list --json 2>/dev/null) && list_rc=0 || list_rc=$?
  assert_exit "kd fleet list exits 0" 0 "$list_rc"
  # Use jq to check for the fleet ID in JSON array (more robust than grep on raw JSON).
  if printf '%s' "$list_out" | jq -e --arg id "$fleet_id" 'map(select(.id == $id)) | length > 0' >/dev/null 2>&1; then
    pass "fleet list contains fleet ID (contains '$fleet_id')"
  else
    fail "fleet list contains fleet ID" "expected output to contain '$fleet_id'; got: $(printf '%s' "$list_out" | head -5)"
  fi
  echo

  # ── Scenario 2: Verify kd fleet show returns fleet details ─────
  blue "Scenario 2: kd fleet show returns details"

  local show_out show_rc
  show_out=$("$KD_BIN" fleet show "$fleet_id" --json 2>/dev/null) && show_rc=0 || show_rc=$?
  assert_exit "kd fleet show exits 0" 0 "$show_rc"
  assert_contains "fleet show contains fleet ID" "$fleet_id" "$show_out"
  assert_contains "fleet show has type fleet" "fleet" "$show_out"

  # Verify title
  local show_title
  show_title=$(echo "$show_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('title',''))" 2>/dev/null || echo "")
  assert_contains "fleet show has correct title" "$TEST_TAG" "$show_title"
  echo

  # ── Scenario 3: Transfer a bead between fleets ────────────────
  blue "Scenario 3: Transfer bead between fleets via kd fleet transfer"

  # Create a task bead to transfer
  local task_id
  task_id=$("$KD_BIN" create "E2E transfer target $TEST_TAG" \
    --type=task -l "e2e:$TEST_TAG" \
    --actor "$KD_ACTOR" --json 2>/dev/null | \
    python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")

  if [ -z "$task_id" ]; then
    fail "create task for transfer" "no id returned"
  else
    CREATED_BEADS+=("$task_id")
    pass "created task bead for transfer ($task_id)"

    local transfer_out transfer_rc
    transfer_out=$("$KD_BIN" fleet transfer "$task_id" \
      --to-fleet "e2e-target-fleet-$TEST_TAG" \
      --json --actor "$KD_ACTOR" 2>/dev/null) && transfer_rc=0 || transfer_rc=$?
    assert_exit "kd fleet transfer exits 0" 0 "$transfer_rc"
    assert_contains "transfer output contains bead ID" "$task_id" "$transfer_out"
  fi
  echo

  # ── Scenario 4: Verify kd fleet list --json output format ──────
  blue "Scenario 4: kd fleet list --json output format"

  local json_out json_rc
  json_out=$("$KD_BIN" fleet list --json 2>/dev/null) && json_rc=0 || json_rc=$?
  assert_exit "kd fleet list --json exits 0" 0 "$json_rc"

  # Verify it's valid JSON array
  local valid_json
  valid_json=$(echo "$json_out" | python3 -c "
import sys, json
data = json.load(sys.stdin)
if isinstance(data, list):
    print('valid_array')
elif isinstance(data, dict) and 'beads' in data:
    print('valid_object')
else:
    print('unknown')
" 2>/dev/null || echo "invalid")

  if [ "$valid_json" = "valid_array" ] || [ "$valid_json" = "valid_object" ]; then
    pass "fleet list --json returns valid JSON"
  else
    fail "fleet list --json returns valid JSON" "got: $valid_json"
  fi
  echo

  # ── Scenario 5: Verify transfer updates bead's fleet field ─────
  blue "Scenario 5: Verify transfer updated fleet field"

  if [ -n "${task_id:-}" ]; then
    local after_out
    after_out=$("$KD_BIN" show "$task_id" --json 2>/dev/null || echo "{}")

    local bead_fleet
    bead_fleet=$(echo "$after_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('fleet',''))" 2>/dev/null || echo "")
    assert_eq "bead fleet updated after transfer" "e2e-target-fleet-$TEST_TAG" "$bead_fleet"
  else
    fail "verify fleet field" "no task bead available"
  fi
  echo

  # ── Scenario 6: Verify transfer event in event log ─────────────
  blue "Scenario 6: Verify transfer event in event log"

  if [ -n "${task_id:-}" ]; then
    local events_out
    events_out=$(api_get "/v1/beads/${task_id}/events")
    assert_contains "events contain bead.transferred" "beads.bead.transferred" "$events_out"
    assert_contains "transfer event contains target fleet" "e2e-target-fleet-$TEST_TAG" "$events_out"
  else
    fail "verify transfer event" "no task bead available"
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
