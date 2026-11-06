#!/usr/bin/env bash
# test-claim-workflow.sh — bead claim/unclaim lifecycle E2E tests
#
# Tests kd claim, kd unclaim, status transitions, and assignee field
# against a live kd server.
#
# Scenarios tested:
#   1. kd claim sets status=in_progress and assignee
#   2. kd claim on already-claimed bead fails
#   3. kd unclaim resets status=open and clears assignee
#   4. kd claim with different actor after unclaim succeeds
#   5. Close a claimed bead — status transitions to closed
#   6. kd claim on closed bead fails

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

# Use a unique actor per test run so checkNoClaimed doesn't find
# in_progress beads left by other e2e test modules sharing the same actor.
export KD_ACTOR="e2e-claim-$$-$(date +%s)"

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
    fail "$name" "expected output NOT to contain '$pattern'; got: $(echo "$actual" | head -5)"
  fi
}

assert_equals() {
  local name="$1"
  local expected="$2"
  local actual="$3"
  if [ "$expected" = "$actual" ]; then
    pass "$name (== '$expected')"
  else
    fail "$name" "expected '$expected', got '$actual'"
  fi
}

# ── Setup ─────────────────────────────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19097
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-claim.log 2>&1 &
  PF_PID=$!
  sleep 3
  export BEADS_HTTP_URL="http://localhost:${port}"
  dim "  Daemon at $BEADS_HTTP_URL (pid $PF_PID)"
}

setup_token() {
  if [ -n "${BEADS_TOKEN:-}" ]; then return; fi
  local secret
  secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
    -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
  if [ -n "$secret" ]; then
    export BEADS_TOKEN="$secret"
    dim "  Token loaded from secret kd-beads-token"
  fi
}

cleanup() {
  for id in "${CREATED_BEADS[@]}"; do
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
  blue " claim workflow E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  local TEST_TAG="e2e-claim-$$-$(date +%s)"

  # Create test bead
  local bead_out bead_id
  bead_out=$("$KD_BIN" create "E2E claim test $TEST_TAG" --type=task --json 2>/dev/null || echo "{}")
  bead_id=$(echo "$bead_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")

  if [ -z "$bead_id" ]; then
    red "ERROR: Could not create test bead"
    exit 1
  fi
  CREATED_BEADS+=("$bead_id")
  dim "  Test bead: $bead_id"
  echo

  # ── Scenario 1: kd claim sets status and assignee ──────────────
  blue "Scenario 1: kd claim sets status=in_progress and assignee"
  local claim_rc=0
  "$KD_BIN" claim "$bead_id" 2>/dev/null || claim_rc=$?
  assert_exit "kd claim exits 0" 0 "$claim_rc"

  local show1
  show1=$("$KD_BIN" show "$bead_id" --json 2>/dev/null || echo "{}")
  assert_contains "status is in_progress after claim" '"in_progress"' "$show1"

  local assignee1
  assignee1=$(echo "$show1" | python3 -c "import sys,json; print(json.load(sys.stdin).get('assignee',''))" 2>/dev/null || echo "")
  if [ -n "$assignee1" ]; then
    pass "assignee set after claim ($assignee1)"
  else
    fail "assignee set after claim" "assignee is empty"
  fi
  echo

  # ── Scenario 2: kd claim on already-claimed bead fails ─────────
  blue "Scenario 2: kd claim on already-claimed bead fails"
  local claim2_rc=0
  KD_ACTOR="different-actor" "$KD_BIN" claim "$bead_id" 2>/dev/null || claim2_rc=$?
  if [ "$claim2_rc" -ne 0 ]; then
    pass "kd claim by different actor: rejected (exit $claim2_rc)"
  else
    fail "kd claim by different actor: should have been rejected" "expected non-zero exit, got 0"
  fi
  echo

  # ── Scenario 3: kd unclaim resets status and clears assignee ───
  blue "Scenario 3: kd unclaim resets status=open and clears assignee"
  local unclaim_rc=0
  "$KD_BIN" unclaim "$bead_id" 2>/dev/null || unclaim_rc=$?
  assert_exit "kd unclaim exits 0" 0 "$unclaim_rc"

  local show3
  show3=$("$KD_BIN" show "$bead_id" --json 2>/dev/null || echo "{}")
  assert_contains "status is open after unclaim" '"open"' "$show3"

  local assignee3
  assignee3=$(echo "$show3" | python3 -c "import sys,json; print(json.load(sys.stdin).get('assignee',''))" 2>/dev/null || echo "")
  assert_equals "assignee cleared after unclaim" "" "$assignee3"
  echo

  # ── Scenario 4: Re-claim by different actor succeeds ───────────
  blue "Scenario 4: kd claim by different actor after unclaim succeeds"
  local claim4_rc=0
  KD_ACTOR="other-agent" "$KD_BIN" claim "$bead_id" 2>/dev/null || claim4_rc=$?
  assert_exit "kd claim by other-agent exits 0" 0 "$claim4_rc"

  local show4
  show4=$("$KD_BIN" show "$bead_id" --json 2>/dev/null || echo "{}")
  assert_contains "status is in_progress after re-claim" '"in_progress"' "$show4"

  local assignee4
  assignee4=$(echo "$show4" | python3 -c "import sys,json; print(json.load(sys.stdin).get('assignee',''))" 2>/dev/null || echo "")
  assert_contains "assignee is other-agent" "other-agent" "$assignee4"

  # Unclaim for next scenario
  KD_ACTOR="other-agent" "$KD_BIN" unclaim "$bead_id" 2>/dev/null || true
  echo

  # ── Scenario 5: Close a claimed bead ───────────────────────────
  blue "Scenario 5: Close a claimed bead — status=closed"
  "$KD_BIN" claim "$bead_id" 2>/dev/null || true
  local close_rc=0
  "$KD_BIN" close "$bead_id" 2>/dev/null || close_rc=$?
  assert_exit "kd close exits 0" 0 "$close_rc"
  CREATED_BEADS=("${CREATED_BEADS[@]/$bead_id/}")

  local show5
  show5=$("$KD_BIN" show "$bead_id" --json 2>/dev/null || echo "{}")
  assert_contains "status is closed" '"closed"' "$show5"
  echo

  # ── Scenario 6: kd claim on closed bead fails ─────────────────
  blue "Scenario 6: kd claim on closed bead fails"
  local claim6_rc=0
  "$KD_BIN" claim "$bead_id" 2>/dev/null || claim6_rc=$?
  if [ "$claim6_rc" -ne 0 ]; then
    pass "kd claim on closed bead: rejected (exit $claim6_rc)"
  else
    fail "kd claim on closed bead: should have been rejected" "expected non-zero exit, got 0"
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
