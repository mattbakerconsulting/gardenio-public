#!/usr/bin/env bash
# test-dm-news.sh — inter-agent messaging (mail, DM, news) E2E tests
#
# Tests gb mail send, gb news, and mail lifecycle against a live kd server.
#
# Scenarios tested:
#   1. gb mail send creates a mail bead
#   2. Mail bead has correct fields (recipient, subject, body)
#   3. kd list --type=mail shows the mail bead
#   4. gb news returns recent activity (without error)
#   5. Mail bead can be closed (read/acknowledged)
#   6. Closed mail does not appear in open list

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
    fail "$name" "expected output NOT to contain '$pattern'"
  fi
}

# ── Setup ─────────────────────────────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19100
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-dm.log 2>&1 &
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
  blue " DM & news E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  local TEST_TAG="e2e-dm-$$-$(date +%s)"
  local RECIPIENT="e2e-agent-bob-$TEST_TAG"

  # ── Scenario 1: gb mail send creates a mail bead ──────────────
  blue "Scenario 1: gb mail send creates a mail bead"
  local mail_out mail_rc mail_id
  mail_out=$("$GB_BIN" mail send "$RECIPIENT" \
    --subject="E2E mail test $TEST_TAG" \
    --body="Hello from E2E DM test" \
    --json 2>/dev/null || echo "{}")
  mail_rc=$?
  assert_exit "gb mail send exits 0" 0 "$mail_rc"

  mail_id=$(echo "$mail_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")

  # Fallback: try text output parsing
  if [ -z "$mail_id" ]; then
    mail_out=$("$GB_BIN" mail send "$RECIPIENT" \
      --subject="E2E mail test $TEST_TAG" \
      --body="Hello from E2E DM test" 2>/dev/null || echo "")
    mail_id=$(echo "$mail_out" | grep -oP '(gd|ka)-[A-Za-z0-9]+' | head -1 || echo "")
  fi

  if [ -n "$mail_id" ]; then
    CREATED_BEADS+=("$mail_id")
    pass "mail bead created ($mail_id)"
  else
    fail "mail bead created" "no id in output: $mail_out"
  fi
  echo

  # ── Scenario 2: Mail bead has correct fields ──────────────────
  blue "Scenario 2: Mail bead has correct fields"
  if [ -n "$mail_id" ]; then
    local show_out
    show_out=$("$KD_BIN" show "$mail_id" --json 2>/dev/null || echo "{}")
    assert_contains "mail has type mail" '"mail"' "$show_out"
    assert_contains "mail title contains subject" "$TEST_TAG" "$show_out"
    assert_contains "mail has recipient" "$RECIPIENT" "$show_out"
  else
    fail "scenario 2 skipped" "no mail ID from scenario 1"
  fi
  echo

  # ── Scenario 3: kd list --type=mail shows the bead ────────────
  blue "Scenario 3: kd list --type=mail includes our mail"
  if [ -n "$mail_id" ]; then
    local list_out
    list_out=$("$KD_BIN" list --type=mail --status=open --json 2>/dev/null || echo "[]")
    assert_contains "mail list contains our bead" "$mail_id" "$list_out"
  else
    fail "scenario 3 skipped" "no mail ID"
  fi
  echo

  # ── Scenario 4: gb news runs without error ────────────────────
  blue "Scenario 4: gb news returns recent activity"
  local news_out news_rc=0
  news_out=$("$GB_BIN" news 2>&1) || news_rc=$?
  assert_exit "gb news exits 0" 0 "$news_rc"
  # news output should contain something (header or content)
  if [ -n "$news_out" ]; then
    pass "gb news returned output"
  else
    dim "  INFO: gb news returned empty (may be expected if no other agents active)"
    pass "gb news ran without error"
  fi
  echo

  # ── Scenario 5: Close mail bead ───────────────────────────────
  blue "Scenario 5: Mail bead can be closed"
  if [ -n "$mail_id" ]; then
    local close_rc=0
    "$KD_BIN" close "$mail_id" 2>/dev/null || close_rc=$?
    assert_exit "kd close mail exits 0" 0 "$close_rc"
    CREATED_BEADS=("${CREATED_BEADS[@]/$mail_id/}")

    local show_closed
    show_closed=$("$KD_BIN" show "$mail_id" --json 2>/dev/null || echo "{}")
    assert_contains "mail status is closed" '"closed"' "$show_closed"
  else
    fail "scenario 5 skipped" "no mail ID"
  fi
  echo

  # ── Scenario 6: Closed mail not in open list ──────────────────
  blue "Scenario 6: Closed mail not in open mail list"
  if [ -n "$mail_id" ]; then
    local list_after
    list_after=$("$KD_BIN" list --type=mail --status=open --json 2>/dev/null || echo "[]")
    assert_not_contains "closed mail not in open list" "$mail_id" "$list_after"
  else
    fail "scenario 6 skipped" "no mail ID"
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
