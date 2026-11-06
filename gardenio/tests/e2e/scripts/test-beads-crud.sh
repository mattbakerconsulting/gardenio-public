#!/usr/bin/env bash
# test-beads-crud.sh — beads CRUD E2E tests
#
# Tests kd CLI CRUD operations against a live kbeads server:
# create, show, update, list, search, and close.
#
# Usage:
#   ./tests/e2e/scripts/test-beads-crud.sh
#
# Prerequisites:
#   - kubectl context pointing at the target cluster
#   - kd binary in PATH (or set KD_BIN)
#   - jq and python3 installed
#
# Scenarios tested:
#   1. Create a task bead
#   2. Show returns correct fields
#   3. Update bead description
#   4. List includes the created bead
#   5. Close the bead
#   6. Closed bead has status=closed

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
KD_BIN="${KD_BIN:-kd}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PASS=0
FAIL=0
ERRORS=()
PF_PID=""
CREATED_BEADS=()

export KD_ACTOR="${KD_ACTOR:-e2e-test}"

# Load shared retry helpers (kd_retry) to absorb transient PF drops.
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib-retry.sh"

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

# ── Setup: port-forward kd server ────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi

  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19093
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-crud.log 2>&1 &
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
  # Close any beads we created
  for id in "${CREATED_BEADS[@]}"; do
    kd_retry "$KD_BIN" close "$id" 2>/dev/null || true
  done
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " beads-crud E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  # Verify kd server is reachable
  if ! kd_retry "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  local TEST_TAG="e2e-crud-$$-$(date +%s)"

  # ── Scenario 1: Create a task bead ──────────────────────────────
  blue "Scenario 1: Create a task bead"
  local create_out create_rc bead_id
  create_out=$(kd_retry "$KD_BIN" create "E2E CRUD test $TEST_TAG" \
    --type=task -l "e2e:$TEST_TAG" \
    -d "Automated E2E CRUD test bead — safe to delete" \
    --json 2>/dev/null) && create_rc=0 || create_rc=$?

  assert_exit "kd create exits 0" 0 "$create_rc"

  bead_id=$(echo "$create_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$bead_id" ]; then
    pass "kd create returns ID ($bead_id)"
    CREATED_BEADS+=("$bead_id")
  else
    fail "kd create returns ID" "no id in output: $create_out"
    # Can't continue without an ID
    blue "═══════════════════════════════════════════════════"
    red " Cannot continue without a bead ID"
    blue "═══════════════════════════════════════════════════"
    return 1
  fi
  echo

  # ── Scenario 2: Show returns correct fields ─────────────────────
  blue "Scenario 2: kd show returns correct fields"
  local show_out show_rc
  show_out=$(kd_retry "$KD_BIN" show "$bead_id" --json 2>/dev/null) && show_rc=0 || show_rc=$?
  assert_exit "kd show exits 0" 0 "$show_rc"
  assert_contains "show contains title" "E2E CRUD test $TEST_TAG" "$show_out"
  assert_contains "show contains type task" '"task"' "$show_out"
  assert_contains "show contains status open" '"open"' "$show_out"
  echo

  # ── Scenario 3: Update bead description ─────────────────────────
  blue "Scenario 3: Update bead description"
  local update_out update_rc
  update_out=$(kd_retry "$KD_BIN" update "$bead_id" \
    -d "Updated: E2E CRUD test $TEST_TAG — verified update" \
    2>/dev/null) && update_rc=0 || update_rc=$?
  assert_exit "kd update exits 0" 0 "$update_rc"

  # Verify update stuck
  local show_after
  show_after=$(kd_retry "$KD_BIN" show "$bead_id" --json 2>/dev/null || echo "{}")
  assert_contains "updated description present" "verified update" "$show_after"
  echo

  # ── Scenario 4: List includes the created bead ──────────────────
  blue "Scenario 4: kd list includes created bead"
  local list_out
  list_out=$(kd_retry "$KD_BIN" list --status=open --json 2>/dev/null || echo "[]")
  assert_contains "list contains bead ID" "$bead_id" "$list_out"
  echo

  # ── Scenario 5: Close the bead ─────────────────────────────────
  blue "Scenario 5: Close the bead"
  local close_out close_rc
  close_out=$(kd_retry "$KD_BIN" close "$bead_id" 2>/dev/null) && close_rc=0 || close_rc=$?
  assert_exit "kd close exits 0" 0 "$close_rc"
  # Remove from cleanup list since we closed it
  CREATED_BEADS=("${CREATED_BEADS[@]/$bead_id/}")
  echo

  # ── Scenario 6: Closed bead has status=closed ───────────────────
  blue "Scenario 6: Closed bead has status=closed"
  local closed_out
  closed_out=$(kd_retry "$KD_BIN" show "$bead_id" --json 2>/dev/null || echo "{}")
  assert_contains "bead status is closed" '"closed"' "$closed_out"
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
