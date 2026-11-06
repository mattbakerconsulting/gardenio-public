#!/usr/bin/env bash
# test-mail-ready.sh — mail system & ready queue E2E tests
#
# Tests gb mail send/list and gb ready queue behavior against a live kbeads server.
#
# Scenarios tested:
#   1. Ready queue returns open tasks
#   2. Ready excludes blocked tasks
#   3. Ready respects priority ordering
#   4. Mail send creates mail bead
#   5. Mail list returns sent mail
#   6. Mail bead has correct fields

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
KD_BIN="${KD_BIN:-kd}"
GB_BIN="${GB_BIN:-gb}"
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

create_task() {
  local title="$1"
  shift
  local out
  out=$("$KD_BIN" create "$title" --type=task -l "project:e2e-test" "$@" --json 2>/dev/null)
  local id
  id=$(echo "$out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$id" ]; then
    CREATED_BEADS+=("$id")
  fi
  echo "$id"
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
    >/tmp/kd-pf-mail.log 2>&1 &
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
  blue " mail & ready-queue E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  local TEST_TAG="e2e-mail-$$-$(date +%s)"

  # ── Scenario 1: Ready queue returns open tasks ─────────────────
  blue "Scenario 1: Ready queue returns open tasks"
  TASK1=$(create_task "e2e-ready-1-$TEST_TAG" -l "e2e:$TEST_TAG")
  TASK2=$(create_task "e2e-ready-2-$TEST_TAG" -l "e2e:$TEST_TAG")
  if [ -z "$TASK1" ] || [ -z "$TASK2" ]; then
    fail "scenario-1-setup" "Could not create tasks"
  else
    dim "  Tasks: $TASK1, $TASK2"
    local ready_out
    ready_out=$("$GB_BIN" ready --project e2e-test --json 2>/dev/null || echo "[]")
    assert_contains "ready contains task 1" "$TASK1" "$ready_out"
    assert_contains "ready contains task 2" "$TASK2" "$ready_out"
  fi
  echo

  # ── Scenario 2: Ready excludes blocked tasks ───────────────────
  blue "Scenario 2: Ready excludes blocked tasks"
  BLOCKER=$(create_task "e2e-ready-blocker-$TEST_TAG" -l "e2e:$TEST_TAG")
  BLOCKED=$(create_task "e2e-ready-blocked-$TEST_TAG" -l "e2e:$TEST_TAG")
  if [ -z "$BLOCKER" ] || [ -z "$BLOCKED" ]; then
    fail "scenario-2-setup" "Could not create tasks"
  else
    dim "  Blocker: $BLOCKER, Blocked: $BLOCKED"
    "$KD_BIN" dep add "$BLOCKED" "$BLOCKER" --type blocks 2>/dev/null

    local ready_out
    ready_out=$("$GB_BIN" ready --project e2e-test --json 2>/dev/null || echo "[]")
    assert_contains "blocker IS in ready" "$BLOCKER" "$ready_out"
    assert_not_contains "blocked NOT in ready" "$BLOCKED" "$ready_out"

    # Close blocker → blocked should appear
    "$KD_BIN" close "$BLOCKER" 2>/dev/null
    CREATED_BEADS=("${CREATED_BEADS[@]/$BLOCKER/}")
    sleep 1

    ready_out=$("$GB_BIN" ready --project e2e-test --json 2>/dev/null || echo "[]")
    assert_contains "unblocked task now in ready" "$BLOCKED" "$ready_out"
  fi
  echo

  # ── Scenario 3: Ready respects priority ────────────────────────
  blue "Scenario 3: Ready respects priority ordering"
  LOW=$(create_task "e2e-ready-low-$TEST_TAG" -l "e2e:$TEST_TAG")
  HIGH=$(create_task "e2e-ready-high-$TEST_TAG" -l "e2e:$TEST_TAG")
  if [ -z "$LOW" ] || [ -z "$HIGH" ]; then
    fail "scenario-3-setup" "Could not create tasks"
  else
    # Set priorities: 1 = high, 3 = low
    "$KD_BIN" update "$HIGH" --priority 1 2>/dev/null
    "$KD_BIN" update "$LOW" --priority 3 2>/dev/null
    dim "  High ($HIGH) priority=1, Low ($LOW) priority=3"

    local ready_out
    ready_out=$("$GB_BIN" ready --project e2e-test --json 2>/dev/null || echo "[]")

    # Check that high-priority bead appears before low-priority
    local high_pos low_pos
    high_pos=$(echo "$ready_out" | python3 -c "
import sys
data = sys.stdin.read()
pos = data.find('$HIGH')
print(pos if pos >= 0 else 999999)
" 2>/dev/null || echo "999999")
    low_pos=$(echo "$ready_out" | python3 -c "
import sys
data = sys.stdin.read()
pos = data.find('$LOW')
print(pos if pos >= 0 else 999999)
" 2>/dev/null || echo "999999")

    if [ "$high_pos" -lt "$low_pos" ]; then
      pass "high-priority bead appears before low-priority"
    else
      fail "priority ordering" "high=$high_pos, low=$low_pos — expected high < low"
    fi
  fi
  echo

  # ── Scenario 4: Mail send ──────────────────────────────────────
  blue "Scenario 4: Mail send creates mail bead"
  local mail_out mail_rc mail_id
  # Send mail to ourselves (KD_ACTOR) so gb mail list can find it
  mail_out=$("$GB_BIN" mail send "${KD_ACTOR}" \
    -s "E2E test mail $TEST_TAG" \
    -b "This is a test mail body" \
    --json 2>/dev/null || echo "")
  mail_rc=$?
  assert_exit "gb mail send exits 0" 0 "$mail_rc"

  mail_id=$(echo "$mail_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$mail_id" ]; then
    CREATED_BEADS+=("$mail_id")
    pass "mail bead created ($mail_id)"
  else
    fail "mail bead ID" "no id in output: $mail_out"
  fi
  echo

  # ── Scenario 5: Mail list ──────────────────────────────────────
  blue "Scenario 5: Mail list returns sent mail"
  if [ -n "$mail_id" ]; then
    local mlist_out
    mlist_out=$("$GB_BIN" mail list --json 2>/dev/null || echo "[]")
    assert_contains "mail list contains our mail" "$mail_id" "$mlist_out"
  else
    fail "scenario-5" "No mail ID from scenario 4"
  fi
  echo

  # ── Scenario 6: Mail bead fields ───────────────────────────────
  blue "Scenario 6: Mail bead has correct fields"
  if [ -n "$mail_id" ]; then
    local mshow_out
    mshow_out=$("$KD_BIN" show "$mail_id" --json 2>/dev/null || echo "{}")
    assert_contains "mail bead has type mail" '"mail"' "$mshow_out"
    assert_contains "mail bead has subject" "$TEST_TAG" "$mshow_out"
    assert_contains "mail bead has assignee" "${KD_ACTOR}" "$mshow_out"
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
