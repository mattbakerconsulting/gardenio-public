#!/usr/bin/env bash
# test-jack-system.sh — jack lifecycle E2E tests
#
# Tests kd jack up/down/extend/check/list against a live kbeads server.
# Jacks are temporary infrastructure change permits with TTL, extensions,
# and revert tracking.
#
# Usage:
#   ./tests/e2e/scripts/test-jack-system.sh
#
# Prerequisites:
#   - kd binary in PATH (or set KD_BIN)
#   - python3 installed
#   - BEADS_HTTP_URL set (or kubectl context for port-forward)
#
# Scenarios tested:
#   1. Create a jack with kd jack up (with TTL)
#   2. Verify kd jack list shows the active jack
#   3. Extend the jack TTL with kd jack extend
#   4. Verify kd jack check reports active status
#   5. Bring down the jack with kd jack down
#   6. Verify closed jack status and fields

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
KD_BIN="${KD_BIN:-kd}"
PASS=0
FAIL=0
ERRORS=()
PF_PID=""
CREATED_JACKS=()

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
  local port=19095
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-jack.log 2>&1 &
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
  for id in "${CREATED_JACKS[@]}"; do
    "$KD_BIN" jack down "$id" --reason "E2E cleanup — test complete" \
      --skip-revert-check --actor "$KD_ACTOR" 2>/dev/null || \
    "$KD_BIN" close "$id" 2>/dev/null || true
  done
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " jack-system E2E tests"
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

  local TEST_TAG="e2e-jack-$$-$(date +%s)"
  local TARGET="e2e/test-target-$TEST_TAG"

  # ── Scenario 1: Create a jack with kd jack up ──────────────────
  blue "Scenario 1: Create a jack with kd jack up"

  local up_out up_rc jack_id
  up_out=$("$KD_BIN" jack up "$TARGET" \
    --reason "E2E test jack $TEST_TAG" \
    --revert-plan "Remove test jack" \
    --ttl 30m \
    --actor "$KD_ACTOR" \
    --json 2>/dev/null) && up_rc=0 || up_rc=$?

  assert_exit "kd jack up exits 0" 0 "$up_rc"

  jack_id=$(echo "$up_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -z "$jack_id" ]; then
    fail "jack up returns ID" "no id in output: $(echo "$up_out" | head -3)"
    return 1
  fi
  pass "jack created ($jack_id)"
  CREATED_JACKS+=("$jack_id")

  # Verify jack fields
  local show_out
  show_out=$("$KD_BIN" show "$jack_id" --json 2>/dev/null || echo "{}")
  assert_contains "jack type is jack" '"jack"' "$show_out"
  assert_contains "jack has target field" "$TARGET" "$show_out"
  assert_contains "jack has expires_at" "jack_expires_at" "$show_out"
  assert_contains "jack has revert_plan" "jack_revert_plan" "$show_out"
  echo

  # ── Scenario 2: Verify kd jack list shows the active jack ──────
  blue "Scenario 2: kd jack list shows active jack"

  local list_out list_rc
  list_out=$("$KD_BIN" jack list --json 2>/dev/null) && list_rc=0 || list_rc=$?
  assert_exit "kd jack list exits 0" 0 "$list_rc"
  assert_contains "jack list contains jack ID" "$jack_id" "$list_out"
  echo

  # ── Scenario 3: Extend the jack TTL ────────────────────────────
  blue "Scenario 3: Extend jack TTL"

  local extend_out extend_rc
  extend_out=$("$KD_BIN" jack extend "$jack_id" \
    --ttl 15m \
    --reason "E2E test extension" \
    --actor "$KD_ACTOR" \
    --json 2>/dev/null) && extend_rc=0 || extend_rc=$?
  assert_exit "kd jack extend exits 0" 0 "$extend_rc"

  # Verify extension count incremented
  local after_extend
  after_extend=$("$KD_BIN" show "$jack_id" --json 2>/dev/null || echo "{}")

  local ext_count
  ext_count=$(echo "$after_extend" | python3 -c "
import sys, json
data = json.load(sys.stdin)
fields = data.get('fields', {})
if isinstance(fields, str):
    import json as j
    fields = j.loads(fields)
print(fields.get('jack_extension_count', 0))
" 2>/dev/null || echo "0")
  assert_eq "extension count is 1" "1" "$ext_count"
  echo

  # ── Scenario 4: Verify kd jack check reports active status ─────
  blue "Scenario 4: kd jack check reports active"

  local check_out check_rc
  check_out=$("$KD_BIN" jack check 2>/dev/null) && check_rc=0 || check_rc=$?
  assert_exit "kd jack check exits 0" 0 "$check_rc"
  assert_contains "check output shows ACTIVE" "ACTIVE" "$check_out"
  assert_contains "check output contains jack ID" "$jack_id" "$check_out"
  echo

  # ── Scenario 5: Bring down the jack ────────────────────────────
  blue "Scenario 5: kd jack down closes the jack"

  local down_out down_rc
  down_out=$("$KD_BIN" jack down "$jack_id" \
    --reason "E2E test complete — reverting changes" \
    --actor "$KD_ACTOR" \
    --json 2>/dev/null) && down_rc=0 || down_rc=$?
  assert_exit "kd jack down exits 0" 0 "$down_rc"
  # Remove from cleanup list
  CREATED_JACKS=("${CREATED_JACKS[@]/$jack_id/}")
  echo

  # ── Scenario 6: Verify closed jack status and fields ───────────
  blue "Scenario 6: Verify closed jack"

  local closed_out
  closed_out=$("$KD_BIN" show "$jack_id" --json 2>/dev/null || echo "{}")
  assert_contains "jack status is closed" '"closed"' "$closed_out"

  local reverted
  reverted=$(echo "$closed_out" | python3 -c "
import sys, json
data = json.load(sys.stdin)
fields = data.get('fields', {})
if isinstance(fields, str):
    import json as j
    fields = j.loads(fields)
print(str(fields.get('jack_reverted', '')).lower())
" 2>/dev/null || echo "")
  assert_eq "jack_reverted is true" "true" "$reverted"

  assert_contains "closed jack has closed_reason" "jack_closed_reason" "$closed_out"

  # Verify closed jack no longer in default list
  local list_after
  list_after=$("$KD_BIN" jack list --json 2>/dev/null || echo "[]")
  assert_not_contains "closed jack not in active list" "$jack_id" "$list_after"
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
