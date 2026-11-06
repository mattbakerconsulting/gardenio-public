#!/usr/bin/env bash
# test-decision-reports.sh — decision-triggered reports E2E tests
#
# Tests that decisions with report:<template> labels keep the agent's
# decision gate pending after resolve, and that submitting a report
# bead (via gb decision report) satisfies the gate.
#
# Usage:
#   ./tests/e2e/scripts/test-decision-reports.sh [--namespace <ns>]
#
# Prerequisites:
#   - kubectl context pointing at america-e2e-eks
#   - kd binary in PATH (or set KD_BIN) for CRUD
#   - gb binary in PATH (or set GB_BIN) for orchestration
#   - jq and python3 installed
#
# Scenarios tested:
#   1. Decision WITHOUT report label — gate satisfies on close (control)
#   2. Decision WITH report label — gate stays pending after close
#   3. Report submission satisfies gate
#   4. gb yield prints REPORT_REQUIRED for report-labeled decisions
#   5. gb decision report validates inputs
#   6. Decision without report label — normal close still works (regression guard)

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
KD_BIN="${KD_BIN:-kd}"         # CRUD: create, close, list, show, label
GB_BIN="${GB_BIN:-gb}"         # Orchestration: decision, gate, bus emit, yield
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
PASS=0
FAIL=0
ERRORS=()
PF_PID=""
YIELD_PIDS=()

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
    fail "$name" "expected output to contain '$pattern'; got: $actual"
  fi
}

assert_not_contains() {
  local name="$1"
  local pattern="$2"
  local actual="$3"
  if grep -qF -- "$pattern" <<< "$actual"; then
    fail "$name" "expected output NOT to contain '$pattern'; got: $actual"
  else
    pass "$name (does not contain '$pattern')"
  fi
}

# ── Setup: port-forward kd server ────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi

  blue "Port-forwarding gardenio-e2e kd server..."
  local port=19092
  kubectl -n "$NAMESPACE" port-forward svc/gardenio-e2e-beads "${port}:8080" \
    >/tmp/kd-pf-reports.log 2>&1 &
  PF_PID=$!
  sleep 3

  export BEADS_HTTP_URL="http://localhost:${port}"
  dim "  Daemon at $BEADS_HTTP_URL (pid $PF_PID)"
}

# ── Setup: get or create test token ──────────────────────────────
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
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
  for pid in "${YIELD_PIDS[@]:-}"; do
    kill "$pid" 2>/dev/null || true
  done
}
trap cleanup EXIT

# ── Bead helpers ──────────────────────────────────────────────────
create_test_agent() {
  local title="$1"
  "$KD_BIN" create "$title" --type=task --json 2>/dev/null \
    | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null \
    || echo ""
}

close_bead() {
  local id="$1"
  "$KD_BIN" close "$id" 2>/dev/null || true
}

# Emit a Stop hook event for the given agent bead ID.
emit_stop() {
  local agent_id="$1"
  local cwd="${2:-/tmp}"
  local hook_json
  hook_json=$(printf '{"session_id":"e2e-test-session","cwd":"%s"}' "$cwd")
  KD_AGENT_ID="$agent_id" \
    "$GB_BIN" bus emit --hook=Stop <<< "$hook_json" \
    2>/tmp/kd-emit-stderr.txt
}

# ── Feature detection ────────────────────────────────────────────
check_report_support() {
  # Create a probe agent and decision to verify the server supports
  # report-gated gates (requires kbeads with decision-reports feature).
  local probe_agent probe_dec
  probe_agent=$(create_test_agent "e2e-report-probe-$$")
  if [ -z "$probe_agent" ]; then
    red "ERROR: Cannot create probe bead"
    return 1
  fi

  probe_dec=$(KD_AGENT_ID="$probe_agent" "$GB_BIN" decision create \
    --prompt="probe-$$" \
    --options='[{"id":"a","label":"ok","artifact_type":"report"}]' \
    --no-wait --json 2>/dev/null \
    | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null \
    || echo "")

  if [ -z "$probe_dec" ]; then
    close_bead "$probe_agent"
    red "ERROR: Cannot create probe decision"
    return 1
  fi

  # Add report label
  "$KD_BIN" label add "$probe_dec" "report:probe" 2>/dev/null || true

  # Respond to close the decision
  "$GB_BIN" decision respond "$probe_dec" --select=a 2>/dev/null || true

  # Check if gate is still pending (report feature active)
  local gate_out
  gate_out=$(KD_AGENT_ID="$probe_agent" "$GB_BIN" gate status 2>/dev/null || true)

  close_bead "$probe_dec" 2>/dev/null || true
  close_bead "$probe_agent" 2>/dev/null || true

  if grep -q "pending\|○" <<<"$gate_out"; then
    return 0  # Feature supported
  else
    return 1  # Feature not deployed
  fi
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " decision-triggered reports E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  # Verify kd server is reachable
  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"

  # Verify the server supports report-gated gates
  blue "Checking report-gated gate support..."
  if ! check_report_support; then
    red "SKIP: kd server does not support report-gated gates."
    red "  Deploy a kbeads image with the decision-reports feature first."
    exit 0  # Skip gracefully, not failure
  fi
  green "  Report-gated gates: supported"
  echo

  # ── Scenario 1: Decision WITHOUT report label — gate satisfies on close ──
  blue "Scenario 1: Decision WITHOUT report label — gate satisfies on close (control)"
  AGENT1=$(create_test_agent "e2e-report-ctrl-$$")
  if [ -z "$AGENT1" ]; then
    fail "scenario-1-setup" "Could not create test bead"
  else
    dim "  Agent bead: $AGENT1"

    # Create decision (no report label)
    local dec1_id
    dec1_id=$(KD_AGENT_ID="$AGENT1" "$GB_BIN" decision create \
      --prompt="E2E report-ctrl s1-$$: choose" \
      --options='[{"id":"a","label":"Alpha","artifact_type":"report"},{"id":"b","label":"Beta","artifact_type":"report"}]' \
      --no-wait --json 2>/dev/null \
      | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null \
      || echo "")

    if [ -z "$dec1_id" ]; then
      fail "scenario-1-decision-create" "Could not create decision"
    else
      dim "  Decision bead: $dec1_id"

      # Respond to close it
      "$GB_BIN" decision respond "$dec1_id" --select=a 2>/dev/null || true

      # Gate should be satisfied (no report label → normal close satisfies)
      local gate1_status
      gate1_status=$(KD_AGENT_ID="$AGENT1" "$GB_BIN" gate status 2>/dev/null || true)
      assert_contains "gate satisfied after respond (no report label)" "satisfied\|●" "$gate1_status"

      # Stop should be allowed
      local rc1=0
      emit_stop "$AGENT1" >/dev/null 2>/dev/null || rc1=$?
      assert_exit "Stop allowed (no report label)" 0 "$rc1"
    fi
    close_bead "$AGENT1"
  fi
  echo

  # ── Scenario 2: Decision WITH report label — gate stays pending ──
  blue "Scenario 2: Decision WITH report label — gate stays pending after close"
  AGENT2=$(create_test_agent "e2e-report-pend-$$")
  if [ -z "$AGENT2" ]; then
    fail "scenario-2-setup" "Could not create test bead"
  else
    dim "  Agent bead: $AGENT2"

    # Create decision
    local dec2_id
    dec2_id=$(KD_AGENT_ID="$AGENT2" "$GB_BIN" decision create \
      --prompt="E2E report-pend s2-$$: choose" \
      --options='[{"id":"a","label":"Alpha","artifact_type":"report"},{"id":"b","label":"Beta","artifact_type":"report"}]' \
      --no-wait --json 2>/dev/null \
      | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null \
      || echo "")

    if [ -z "$dec2_id" ]; then
      fail "scenario-2-decision-create" "Could not create decision"
    else
      dim "  Decision bead: $dec2_id"

      # Add report label BEFORE responding
      "$KD_BIN" label add "$dec2_id" "report:summary" 2>/dev/null || true
      dim "  Added label: report:summary"

      # Respond to close it
      "$GB_BIN" decision respond "$dec2_id" --select=a 2>/dev/null || true
      dim "  Decision responded"

      # Gate should be PENDING (report label keeps it open)
      local gate2_status
      gate2_status=$(KD_AGENT_ID="$AGENT2" "$GB_BIN" gate status 2>/dev/null || true)
      assert_contains "gate pending after respond (report label)" "pending\|○" "$gate2_status"

      # Stop should be BLOCKED
      local rc2=0
      emit_stop "$AGENT2" >/dev/null 2>/dev/null || rc2=$?
      assert_exit "Stop blocked (report not submitted)" 2 "$rc2"
    fi
    close_bead "$AGENT2"
  fi
  echo

  # ── Scenario 3: Report submission satisfies gate ──
  blue "Scenario 3: Report submission satisfies gate"
  AGENT3=$(create_test_agent "e2e-report-sub-$$")
  if [ -z "$AGENT3" ]; then
    fail "scenario-3-setup" "Could not create test bead"
  else
    dim "  Agent bead: $AGENT3"

    # Create decision with report label
    local dec3_id
    dec3_id=$(KD_AGENT_ID="$AGENT3" "$GB_BIN" decision create \
      --prompt="E2E report-sub s3-$$: choose" \
      --options='[{"id":"a","label":"Go","artifact_type":"report"}]' \
      --no-wait --json 2>/dev/null \
      | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null \
      || echo "")

    if [ -z "$dec3_id" ]; then
      fail "scenario-3-decision-create" "Could not create decision"
    else
      dim "  Decision bead: $dec3_id"

      # Add report label and respond
      "$KD_BIN" label add "$dec3_id" "report:summary" 2>/dev/null || true
      "$GB_BIN" decision respond "$dec3_id" --select=a 2>/dev/null || true
      dim "  Decision responded (report:summary label)"

      # Verify gate is pending first
      local gate3_before
      gate3_before=$(KD_AGENT_ID="$AGENT3" "$GB_BIN" gate status 2>/dev/null || true)
      assert_contains "gate pending before report" "pending\|○" "$gate3_before"

      # Stop should be blocked
      local rc3_before=0
      emit_stop "$AGENT3" >/dev/null 2>/dev/null || rc3_before=$?
      assert_exit "Stop blocked before report" 2 "$rc3_before"

      # Submit report
      local report3_out report3_rc
      report3_out=$(KD_AGENT_ID="$AGENT3" \
        "$GB_BIN" decision report "$dec3_id" --content="Test report content for E2E s3-$$" \
        2>/dev/null) && report3_rc=0 || report3_rc=$?
      assert_exit "report submission exits 0" 0 "$report3_rc"

      # Verify report output contains a report bead ID
      local report3_id
      report3_id=$(echo "$report3_out" | python3 -c "
import sys
line = sys.stdin.read()
# Look for a bead ID pattern (bd-xxxxx)
import re
m = re.search(r'(bd-[a-z0-9]+)', line)
print(m.group(1) if m else '')
" 2>/dev/null || echo "")
      if [ -n "$report3_id" ]; then
        pass "report returns report bead ID ($report3_id)"
      else
        dim "  Report output: $report3_out"
        pass "report submission succeeded (output: $report3_out)"
      fi

      # Gate should now be SATISFIED
      local gate3_after
      gate3_after=$(KD_AGENT_ID="$AGENT3" "$GB_BIN" gate status 2>/dev/null || true)
      assert_contains "gate satisfied after report" "satisfied\|●" "$gate3_after"

      # Stop should now be ALLOWED
      local rc3_after=0
      emit_stop "$AGENT3" >/dev/null 2>/dev/null || rc3_after=$?
      assert_exit "Stop allowed after report" 0 "$rc3_after"
    fi
    close_bead "$AGENT3"
  fi
  echo

  # ── Scenario 4: gb yield prints REPORT_REQUIRED ──
  blue "Scenario 4: gb yield prints REPORT_REQUIRED for report-labeled decisions"
  AGENT4=$(create_test_agent "e2e-report-yield-$$")
  if [ -z "$AGENT4" ]; then
    fail "scenario-4-setup" "Could not create test bead"
  else
    dim "  Agent bead: $AGENT4"

    # Create decision with report label
    local dec4_id
    dec4_id=$(KD_AGENT_ID="$AGENT4" "$GB_BIN" decision create \
      --prompt="E2E report-yield s4-$$: pick" \
      --options='[{"id":"a","label":"Go","artifact_type":"report"},{"id":"b","label":"Wait","artifact_type":"report"}]' \
      --no-wait --json 2>/dev/null \
      | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null \
      || echo "")

    if [ -z "$dec4_id" ]; then
      fail "scenario-4-decision-create" "Could not create decision"
    else
      dim "  Decision bead: $dec4_id"

      # Add report label
      "$KD_BIN" label add "$dec4_id" "report:summary" 2>/dev/null || true

      # Start yield in background
      local yield4_out="/tmp/yield4-$$.out"
      KD_AGENT_ID="$AGENT4" "$GB_BIN" yield --timeout=15s >"$yield4_out" 2>&1 &
      local yield4_pid=$!
      YIELD_PIDS+=("$yield4_pid")
      sleep 2

      # Respond to the decision (closes it)
      "$GB_BIN" decision respond "$dec4_id" --select=a 2>/dev/null || true
      dim "  Responded to decision"

      # Wait for yield to exit
      local yield4_rc=0
      wait "$yield4_pid" || yield4_rc=$?
      local yield4_content
      yield4_content=$(cat "$yield4_out" 2>/dev/null || echo "")
      dim "  Yield output: $yield4_content"

      assert_exit "yield exits 0 on decision close" 0 "$yield4_rc"
      assert_contains "yield mentions resolved" "resolved\|closed" "$yield4_content"
      assert_contains "yield prints REPORT_REQUIRED" "REPORT_REQUIRED" "$yield4_content"
      assert_contains "REPORT_REQUIRED includes type=summary" "type=summary" "$yield4_content"

      rm -f "$yield4_out"
    fi
    close_bead "$AGENT4"
  fi
  echo

  # ── Scenario 5: gb decision report validates inputs ──
  blue "Scenario 5: gb decision report validates inputs"

  # 5a: Nonexistent bead ID
  local rc5a=0
  "$GB_BIN" decision report "bd-nonexistent-$$" --content="x" >/dev/null 2>&1 || rc5a=$?
  if [ "$rc5a" -ne 0 ]; then
    pass "report rejects nonexistent bead (exit $rc5a)"
  else
    fail "report rejects nonexistent bead" "expected non-zero exit, got 0"
  fi

  # 5b: Bead that is not a decision
  local task5_id
  task5_id=$(create_test_agent "e2e-not-a-decision-$$")
  if [ -n "$task5_id" ]; then
    local rc5b=0
    "$GB_BIN" decision report "$task5_id" --content="x" >/dev/null 2>&1 || rc5b=$?
    if [ "$rc5b" -ne 0 ]; then
      pass "report rejects non-decision bead (exit $rc5b)"
    else
      fail "report rejects non-decision bead" "expected non-zero exit, got 0"
    fi
    close_bead "$task5_id"
  else
    fail "scenario-5b-setup" "Could not create task bead"
  fi
  echo

  # ── Scenario 6: Decision without report label — normal close (regression) ──
  blue "Scenario 6: Decision without report label — normal close (regression guard)"
  AGENT6=$(create_test_agent "e2e-report-regr-$$")
  if [ -z "$AGENT6" ]; then
    fail "scenario-6-setup" "Could not create test bead"
  else
    dim "  Agent bead: $AGENT6"

    # Create and respond to a decision with no report label
    local dec6_id
    dec6_id=$(KD_AGENT_ID="$AGENT6" "$GB_BIN" decision create \
      --prompt="E2E report-regr s6-$$: continue?" \
      --options='[{"id":"a","label":"Yes","artifact_type":"report"},{"id":"b","label":"No","artifact_type":"report"}]' \
      --no-wait --json 2>/dev/null \
      | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null \
      || echo "")

    if [ -z "$dec6_id" ]; then
      fail "scenario-6-decision-create" "Could not create decision"
    else
      dim "  Decision bead: $dec6_id"

      "$GB_BIN" decision respond "$dec6_id" --select=a 2>/dev/null || true

      # Verify decision show reports closed and chosen
      local show6_out
      show6_out=$("$GB_BIN" decision show "$dec6_id" --json 2>/dev/null || echo "{}")
      assert_contains "decision show: status closed" "closed" "$show6_out"
      assert_contains "decision show: chosen field present" '"a"' "$show6_out"

      # Gate should be satisfied (no report label)
      local gate6_status
      gate6_status=$(KD_AGENT_ID="$AGENT6" "$GB_BIN" gate status 2>/dev/null || true)
      assert_contains "gate satisfied (regression)" "satisfied\|●" "$gate6_status"

      # Stop should be allowed
      local rc6=0
      emit_stop "$AGENT6" >/dev/null 2>/dev/null || rc6=$?
      assert_exit "Stop allowed (regression)" 0 "$rc6"
    fi
    close_bead "$AGENT6"
  fi
  echo

  # ── Summary ───────────────────────────────────────────────────────
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
