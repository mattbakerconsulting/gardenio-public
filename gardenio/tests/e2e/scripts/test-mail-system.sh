#!/usr/bin/env bash
# test-mail-system.sh — comprehensive gb mail E2E tests
#
# Tests the full gb mail lifecycle (send, inbox, read, list) against a live
# kbeads server.  This is a dedicated mail-focused module that exercises
# routing, lifecycle, status filtering, JSON output, and error handling.
#
# Scenarios tested:
#   1.  mail send creates a mail bead with correct fields
#   2.  mail send --json returns bead id
#   3.  mail send without --subject fails
#   4.  mail inbox shows mail addressed to current actor
#   5.  mail inbox excludes mail addressed to other actors
#   6.  mail read displays correct from/to/subject/body
#   7.  mail read --json returns full bead object
#   8.  mail list defaults to open status
#   9.  mail list --status=closed shows closed mail
#   10. mail lifecycle: open → close → only in closed list
#   11. inbox ordering: newest mail first
#   12. from:<sender> label is set correctly

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

ACTOR_A="${KD_ACTOR:-e2e-mail-a}"
ACTOR_B="e2e-mail-b-$$"

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
    pass "$name"
  else
    fail "$name" "expected exit $expected, got $actual"
  fi
}

assert_contains() {
  local name="$1"
  local pattern="$2"
  local actual="$3"
  if grep -qF -- "$pattern" <<< "$actual"; then
    pass "$name"
  else
    fail "$name" "expected to contain '$pattern'; got: $(echo "$actual" | head -3)"
  fi
}

assert_not_contains() {
  local name="$1"
  local pattern="$2"
  local actual="$3"
  if ! grep -qF -- "$pattern" <<< "$actual"; then
    pass "$name"
  else
    fail "$name" "expected NOT to contain '$pattern'"
  fi
}

assert_json_field() {
  local name="$1"
  local jq_expr="$2"
  local expected="$3"
  local json="$4"
  local actual
  actual=$(echo "$json" | python3 -c "
import sys, json
data = json.load(sys.stdin)
# Navigate dotted path
obj = data
for key in '$jq_expr'.split('.'):
    if isinstance(obj, list):
        obj = obj[int(key)]
    else:
        obj = obj.get(key, '')
print(obj)
" 2>/dev/null || echo "PARSE_ERROR")
  if [ "$actual" = "$expected" ]; then
    pass "$name"
  else
    fail "$name" "expected $jq_expr='$expected', got '$actual'"
  fi
}

track_bead() {
  local id="$1"
  if [ -n "$id" ]; then
    CREATED_BEADS+=("$id")
  fi
}

# Run gb mail with a specific actor
gb_as() {
  local who="$1"; shift
  KD_ACTOR="$who" "$GB_BIN" "$@"
}

# ── Setup ─────────────────────────────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19098
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-mail-system.log 2>&1 &
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
  blue " gb mail E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  export KD_ACTOR="$ACTOR_A"

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  local TEST_TAG="e2e-mail-sys-$$-$(date +%s)"

  # ── Scenario 1: mail send creates bead with correct fields ─────
  blue "Scenario 1: mail send creates a mail bead"
  local send_out send_rc mail1_id
  send_out=$(gb_as "$ACTOR_A" mail send "$ACTOR_B" \
    -s "Test subject $TEST_TAG" \
    -b "Test body content" \
    --json 2>&1) || true
  send_rc=$?
  assert_exit "mail send exits 0" 0 "$send_rc"

  mail1_id=$(echo "$send_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  track_bead "$mail1_id"
  if [ -n "$mail1_id" ]; then
    pass "mail send returned bead ID ($mail1_id)"
  else
    fail "mail send returned bead ID" "no id in: $send_out"
  fi

  # Verify the bead via kd show
  if [ -n "$mail1_id" ]; then
    local show_out
    show_out=$("$KD_BIN" show "$mail1_id" --json 2>/dev/null || echo "{}")
    assert_json_field "bead type is mail" "type" "mail" "$show_out"
    assert_json_field "bead assignee is recipient" "assignee" "$ACTOR_B" "$show_out"
    assert_contains "bead title matches subject" "Test subject $TEST_TAG" "$show_out"
    assert_contains "bead description has body" "Test body content" "$show_out"
    assert_contains "bead has from label" "from:$ACTOR_A" "$show_out"
    assert_json_field "bead status is open" "status" "open" "$show_out"
  fi
  echo

  # ── Scenario 2: mail send --json output format ─────────────────
  blue "Scenario 2: mail send --json returns valid JSON with id"
  local send2_out mail2_id
  send2_out=$(gb_as "$ACTOR_A" mail send "$ACTOR_B" \
    -s "JSON test $TEST_TAG" \
    --json 2>/dev/null || echo "{}")
  mail2_id=$(echo "$send2_out" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('id',''))" 2>/dev/null || echo "")
  track_bead "$mail2_id"
  if [ -n "$mail2_id" ]; then
    pass "send --json returns parseable JSON with id"
  else
    fail "send --json format" "could not parse id from: $send2_out"
  fi
  echo

  # ── Scenario 3: mail send without subject fails ────────────────
  blue "Scenario 3: mail send without --subject fails"
  local err_out err_rc=0
  err_out=$(gb_as "$ACTOR_A" mail send "$ACTOR_B" 2>&1) || err_rc=$?
  if [ "$err_rc" -ne 0 ]; then
    pass "mail send without subject exits non-zero ($err_rc)"
  else
    fail "mail send without subject should fail" "exited 0"
  fi
  assert_contains "error mentions subject" "subject" "$err_out"
  echo

  # ── Scenario 4: inbox shows mail for current actor ─────────────
  blue "Scenario 4: inbox shows mail addressed to actor"
  # Send mail from A to B, check B's inbox
  local inbox_mail_id
  inbox_mail_id=$(gb_as "$ACTOR_A" mail send "$ACTOR_B" \
    -s "Inbox test $TEST_TAG" \
    -b "Inbox body" \
    --json 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  track_bead "$inbox_mail_id"

  if [ -n "$inbox_mail_id" ]; then
    local inbox_out
    inbox_out=$(gb_as "$ACTOR_B" mail inbox --json 2>/dev/null || echo "[]")
    assert_contains "B's inbox contains the mail" "$inbox_mail_id" "$inbox_out"
  else
    fail "scenario-4-setup" "could not create test mail"
  fi
  echo

  # ── Scenario 5: inbox excludes mail for other actors ───────────
  blue "Scenario 5: inbox excludes mail for other actors"
  if [ -n "$inbox_mail_id" ]; then
    local inbox_a_out
    inbox_a_out=$(gb_as "$ACTOR_A" mail inbox --json 2>/dev/null || echo "[]")
    assert_not_contains "A's inbox does NOT contain mail sent to B" "$inbox_mail_id" "$inbox_a_out"
  else
    fail "scenario-5-setup" "no inbox mail from scenario 4"
  fi
  echo

  # ── Scenario 6: mail read shows detail ─────────────────────────
  blue "Scenario 6: mail read shows correct fields"
  if [ -n "$mail1_id" ]; then
    local read_out
    read_out=$(gb_as "$ACTOR_B" mail read "$mail1_id" 2>/dev/null || echo "")
    assert_contains "read output has sender" "$ACTOR_A" "$read_out"
    assert_contains "read output has recipient" "$ACTOR_B" "$read_out"
    assert_contains "read output has subject" "Test subject $TEST_TAG" "$read_out"
    assert_contains "read output has body" "Test body content" "$read_out"
  else
    fail "scenario-6-setup" "no mail1_id from scenario 1"
  fi
  echo

  # ── Scenario 7: mail read --json ───────────────────────────────
  blue "Scenario 7: mail read --json returns full bead"
  if [ -n "$mail1_id" ]; then
    local read_json
    read_json=$(gb_as "$ACTOR_B" mail read "$mail1_id" --json 2>/dev/null || echo "{}")
    assert_json_field "read JSON has correct id" "id" "$mail1_id" "$read_json"
    assert_json_field "read JSON has type mail" "type" "mail" "$read_json"
    assert_contains "read JSON has description" "Test body content" "$read_json"
  else
    fail "scenario-7-setup" "no mail1_id"
  fi
  echo

  # ── Scenario 8: mail list defaults to open ─────────────────────
  blue "Scenario 8: mail list defaults to open status"
  if [ -n "$inbox_mail_id" ]; then
    local list_out
    list_out=$(gb_as "$ACTOR_B" mail list --json 2>/dev/null || echo "[]")
    assert_contains "open mail appears in default list" "$inbox_mail_id" "$list_out"
  else
    fail "scenario-8-setup" "no inbox mail"
  fi
  echo

  # ── Scenario 9: mail list --status=closed shows closed ─────────
  blue "Scenario 9: mail list with status=closed"
  # Create a mail, close it, verify it shows up in closed list
  local close_mail_id
  close_mail_id=$(gb_as "$ACTOR_A" mail send "$ACTOR_B" \
    -s "Close test $TEST_TAG" \
    --json 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  track_bead "$close_mail_id"

  if [ -n "$close_mail_id" ]; then
    # Close it
    "$KD_BIN" close "$close_mail_id" 2>/dev/null
    CREATED_BEADS=("${CREATED_BEADS[@]/$close_mail_id/}")
    sleep 1

    local closed_list
    closed_list=$(gb_as "$ACTOR_B" mail list --status=closed --json 2>/dev/null || echo "[]")
    assert_contains "closed mail in --status=closed" "$close_mail_id" "$closed_list"

    local open_list
    open_list=$(gb_as "$ACTOR_B" mail list --json 2>/dev/null || echo "[]")
    assert_not_contains "closed mail NOT in default open list" "$close_mail_id" "$open_list"
  else
    fail "scenario-9-setup" "could not create close test mail"
  fi
  echo

  # ── Scenario 10: full lifecycle ────────────────────────────────
  blue "Scenario 10: mail lifecycle open → close"
  local lc_mail_id
  lc_mail_id=$(gb_as "$ACTOR_A" mail send "$ACTOR_B" \
    -s "Lifecycle $TEST_TAG" \
    -b "Lifecycle body" \
    --json 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  track_bead "$lc_mail_id"

  if [ -n "$lc_mail_id" ]; then
    # Verify open initially
    local lc_show
    lc_show=$("$KD_BIN" show "$lc_mail_id" --json 2>/dev/null || echo "{}")
    assert_json_field "lifecycle: initially open" "status" "open" "$lc_show"

    # Verify in inbox
    local lc_inbox
    lc_inbox=$(gb_as "$ACTOR_B" mail inbox --json 2>/dev/null || echo "[]")
    assert_contains "lifecycle: in inbox when open" "$lc_mail_id" "$lc_inbox"

    # Close it
    "$KD_BIN" close "$lc_mail_id" 2>/dev/null
    CREATED_BEADS=("${CREATED_BEADS[@]/$lc_mail_id/}")
    sleep 1

    # Verify closed
    lc_show=$("$KD_BIN" show "$lc_mail_id" --json 2>/dev/null || echo "{}")
    assert_json_field "lifecycle: status is closed" "status" "closed" "$lc_show"

    # Verify NOT in inbox (inbox defaults to open)
    lc_inbox=$(gb_as "$ACTOR_B" mail inbox --json 2>/dev/null || echo "[]")
    assert_not_contains "lifecycle: not in inbox after close" "$lc_mail_id" "$lc_inbox"
  else
    fail "scenario-10-setup" "could not create lifecycle mail"
  fi
  echo

  # ── Scenario 11: inbox ordering (newest first) ─────────────────
  blue "Scenario 11: inbox returns newest mail first"
  local order_id1 order_id2
  order_id1=$(gb_as "$ACTOR_A" mail send "$ACTOR_B" \
    -s "Order first $TEST_TAG" \
    --json 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  track_bead "$order_id1"
  sleep 1
  order_id2=$(gb_as "$ACTOR_A" mail send "$ACTOR_B" \
    -s "Order second $TEST_TAG" \
    --json 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  track_bead "$order_id2"

  if [ -n "$order_id1" ] && [ -n "$order_id2" ]; then
    local order_out
    order_out=$(gb_as "$ACTOR_B" mail inbox --json 2>/dev/null || echo "[]")

    local pos1 pos2
    pos1=$(echo "$order_out" | python3 -c "
import sys; data=sys.stdin.read(); pos=data.find('$order_id1'); print(pos if pos>=0 else 999999)
" 2>/dev/null || echo "999999")
    pos2=$(echo "$order_out" | python3 -c "
import sys; data=sys.stdin.read(); pos=data.find('$order_id2'); print(pos if pos>=0 else 999999)
" 2>/dev/null || echo "999999")

    if [ "$pos2" -lt "$pos1" ]; then
      pass "newer mail (order_id2) appears before older (order_id1)"
    else
      fail "inbox ordering" "pos2=$pos2 should be < pos1=$pos1 (newest first)"
    fi
  else
    fail "scenario-11-setup" "could not create ordering mails"
  fi
  echo

  # ── Scenario 12: from label ────────────────────────────────────
  blue "Scenario 12: from:<sender> label is set correctly"
  local from_mail_id
  from_mail_id=$(gb_as "$ACTOR_A" mail send "$ACTOR_B" \
    -s "From label test $TEST_TAG" \
    --json 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  track_bead "$from_mail_id"

  if [ -n "$from_mail_id" ]; then
    local from_show
    from_show=$("$KD_BIN" show "$from_mail_id" --json 2>/dev/null || echo "{}")

    # Check that labels array contains from:<actor_a>
    local has_from
    has_from=$(echo "$from_show" | python3 -c "
import sys, json
d = json.load(sys.stdin)
labels = d.get('labels', [])
print('yes' if 'from:$ACTOR_A' in labels else 'no')
" 2>/dev/null || echo "no")

    if [ "$has_from" = "yes" ]; then
      pass "from:$ACTOR_A label present in mail bead"
    else
      fail "from label" "expected from:$ACTOR_A in labels"
    fi
  else
    fail "scenario-12-setup" "could not create from-label mail"
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
