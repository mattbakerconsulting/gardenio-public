#!/usr/bin/env bash
# test-escalation-execution.sh — escalation execution bridge E2E tests (Phase 3)
#
# Tests the escalation-to-agent execution pipeline: approve an escalation with an
# execution_target, then verify the bridge spawns an agent, sets executor fields,
# captures results, and nudges the requesting agent.
#
# P7 Task 4 (gd-3tOmdG4moG): the bridge web API no longer approves (POST
# /api/escalations/{id}/approve is 403 without OIDC, 410 otherwise). Approval goes
# through the real operator verb, `kd escalation approve`, the way
# test-escalation-crud.sh Scenario 4 does. Each scenario first asserts the forged
# X-Auth-Request-Email approve is refused (negative control). If the e2e daemon
# refuses the kd approve (no OIDC approver), the scenario is SKIPped with the
# reason printed; the forged path is never re-opened to make it pass.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-escalation-execution.sh
#
# Prerequisites:
#   - BEADS_HTTP_URL set (kbeads HTTP API, e.g. http://localhost:19090)
#   - BRIDGE_HTTP_URL set (slack-bridge HTTP API, e.g. http://localhost:19091)
#   - BEADS_TOKEN set (auth token for kbeads API)
#   - kd binary in PATH (or set KD_BIN)
#   - curl, python3, jq installed
#
# Scenarios tested:
#   1. Approve with execution_target=local → executor field set, agent bead created
#   2. Result capture: update escalation with result_status + result, close it
#   3. Requesting agent nudge: create escalation with requesting_agent, verify nudge fields
#   4. local fallback: no local agent → falls back to creating one
#   5. service_account target: not implemented → failure result recorded

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

# Test user with approve:escalation capability (same as Phase 2)
TEST_USER_EMAIL="e2e-test@gardenio.dev"

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
    fail "$name" "expected to NOT contain '$pattern'"
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

# approve_escalation ID TARGET — drive an approval the supported way.
# 1. Negative control: the forged-header bridge approve must be refused (403/410).
# 2. Set execution_target, then `kd escalation approve` (operator verb).
# Returns 0 when the approval landed; returns 1 (after printing a SKIP line) when
# the daemon refused it, so callers can skip the dependent assertions.
approve_escalation() {
  local id="$1"
  local target="$2"
  local forged_status
  forged_status=$(curl -s --max-time 30 -o /dev/null -w '%{http_code}' -X POST \
    "${BRIDGE_HTTP_URL}/api/escalations/$id/approve" \
    -H "X-Auth-Request-Email: $TEST_USER_EMAIL" \
    -H "Content-Type: application/json" \
    -d "{\"execution_target\":\"$target\"}" 2>/dev/null) || forged_status="000"
  if [ "$forged_status" = "403" ] || [ "$forged_status" = "410" ]; then
    pass "forged-header bridge approve refused (HTTP $forged_status)"
  else
    fail "forged-header bridge approve refused" "expected HTTP 403 or 410, got HTTP $forged_status"
  fi

  "$KD_BIN" update "$id" -f "execution_target=$target" --actor "$KD_ACTOR" >/dev/null 2>&1 || true
  local out rc
  out=$("$KD_BIN" escalation approve "$id" --as "e2e-approver-op" --actor "$KD_ACTOR" 2>&1) && rc=0 || rc=$?
  if [ "$rc" -eq 0 ]; then
    pass "kd escalation approve succeeds"
    return 0
  fi
  green "  SKIP: kd escalation approve refused (rc=$rc): $(echo "$out" | head -2 | tr '\n' ' ')"
  dim "    the e2e daemon has no OIDC approver; execution assertions skipped"
  return 1
}

# Poll a bead field until it matches expected value or timeout
poll_bead_field() {
  local bead_id="$1"
  local field="$2"
  local expected="$3"
  local timeout="${4:-30}"
  local interval=2
  local elapsed=0

  while [ "$elapsed" -lt "$timeout" ]; do
    local bead_json
    bead_json=$("$KD_BIN" show "$bead_id" --json 2>/dev/null || echo "{}")
    local value
    value=$(echo "$bead_json" | python3 -c "
import sys,json
d=json.load(sys.stdin)
fields=d.get('fields',{})
print(fields.get('$field',''))
" 2>/dev/null || echo "")
    if [ -n "$value" ] && grep -qF "$expected" <<<"$value"; then
      echo "$value"
      return 0
    fi
    sleep "$interval"
    elapsed=$((elapsed + interval))
  done
  return 1
}

# ── Setup: port-forward kbeads server ─────────────────────────────
setup_beads_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi

  blue "Port-forwarding ${NAMESPACE} kbeads server..."
  local port=19096
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-execution.log 2>&1 &
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
  local port=19092
  kubectl -n "$NAMESPACE" port-forward "svc/${BRIDGE_SVC}" "${port}:8090" \
    >/tmp/bridge-pf-execution.log 2>&1 &
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

# ── Seed escalation type config ──────────────────────────────────
seed_escalation_config() {
  # type:escalation is builtin-authoritative; the daemon builtin (config.go) +
  # controller EnsureConfigs seed already declare a SUPERSET of these fields.
  # The old hand-maintained PUT could only shadow-and-drift the builtin
  # (ka-0000003677 / generalized ka-0000000407). Dropped so e2e exercises the
  # builtin prod runs, guarded by TestE2ESeedTypes_NoBuiltinShadow. Do NOT re-add.
  dim "  type:escalation config: using daemon builtin (no shadow PUT)"
}

# ── Seed test user (reuse Phase 2 users if present) ──────────────
seed_test_user() {
  blue "Seeding test user bead..."

  # type:user is builtin-authoritative; the daemon builtin (config.go) +
  # controller EnsureConfigs seed already declare a SUPERSET of these fields.
  # The old hand-maintained PUT could only shadow-and-drift the builtin
  # (ka-0000003677 / generalized ka-0000000407). Dropped so e2e exercises the
  # builtin prod runs, guarded by TestE2ESeedTypes_NoBuiltinShadow. Do NOT re-add.

  # Create capable test user
  local user_out user_id
  user_out=$("$KD_BIN" create "E2E Execution Test User" \
    --type=user \
    -l "e2e:escalation-execution" \
    -f "email=$TEST_USER_EMAIL" \
    -f "display_name=E2E Execution Test User" \
    -f 'capabilities=["approve:escalation"]' \
    --actor "$KD_ACTOR" --json 2>/dev/null || echo "{}")
  user_id=$(echo "$user_out" | json_field id || echo "")
  if [ -n "$user_id" ]; then
    CREATED_BEADS+=("$user_id")
    dim "  Test user created: $user_id ($TEST_USER_EMAIL)"
  else
    dim "  WARNING: Failed to create test user"
  fi
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
}
trap cleanup EXIT

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " escalation-execution E2E tests (Phase 3: Execution Bridge)"
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

  # Verify bridge is reachable.
  # curl with -w '%{http_code}' writes "000" on connection failure AND exits
  # non-zero, so a naive `$(curl ... || echo "000")` captures "000\n000" and
  # never matches the skip guard — see gd-3RxELxfbCe. Use `|| true` to swallow
  # the non-zero exit, then default empty result to "000".
  local bridge_health
  bridge_health=$(curl -sf --max-time 10 -o /dev/null -w '%{http_code}' \
    "${BRIDGE_HTTP_URL}/healthz" 2>/dev/null || true)
  bridge_health="${bridge_health:-000}"
  if [ "$bridge_health" = "000" ]; then
    blue "Bridge unreachable at $BRIDGE_HTTP_URL (HTTP $bridge_health)."
    blue "  slack-bridge is disabled in this overlay (e.g. ephemeral/per-instance)."
    green " SKIP: escalation-execution (bridge unavailable)"
    exit 0
  fi
  green "  Bridge reachable: $BRIDGE_HTTP_URL"

  # Probe: check if escalation API is deployed
  local probe_status
  probe_status=$(curl -s --max-time 10 -o /dev/null -w '%{http_code}' \
    "${BRIDGE_HTTP_URL}/api/escalations" 2>/dev/null || true)
  probe_status="${probe_status:-000}"
  if [ "$probe_status" = "404" ] || [ "$probe_status" = "000" ]; then
    blue "Bridge does not have escalation API (HTTP $probe_status)."
    blue "  The deployed bridge image predates the escalation API."
    green " SKIP: escalation-execution (bridge API not available)"
    exit 0
  fi
  dim "  Escalation API probe: HTTP $probe_status"

  seed_escalation_config
  seed_test_user

  # Allow user directory sync
  dim "  Waiting for bridge user directory sync..."
  sleep 5

  echo

  local TEST_TAG="e2e-exec-$$-$(date +%s)"

  # ── Scenario 1: Approve with execution_target=local ──────
  blue "Scenario 1: Approve → executor field set + agent bead created"

  # Create a test escalation
  local esc1_out esc1_id
  esc1_out=$("$KD_BIN" create "E2E execution $TEST_TAG: local spawn" \
    --type=escalation \
    -l "e2e:$TEST_TAG" \
    -l "project:gardenio" \
    -f 'operation_type=k8s_rbac' \
    -f 'operation_spec={"action":"create_role","role_name":"e2e-test-role"}' \
    -f 'context=E2E test: verify agent spawn after approval' \
    --actor "$KD_ACTOR" --json 2>/dev/null || echo "{}")
  esc1_id=$(echo "$esc1_out" | json_field id || echo "")

  if [ -z "$esc1_id" ]; then
    red "ERROR: Could not create test escalation"
    exit 1
  fi
  CREATED_BEADS+=("$esc1_id")
  dim "  Escalation created: $esc1_id"

  # Approve with execution_target=local (negative control + kd approve)
  local approved=true
  approve_escalation "$esc1_id" local || approved=false

  if [ "$approved" = true ]; then
  # Wait for the bridge execution watcher to set the executor field.
  # The bridge watches SSE events and reacts asynchronously, so we poll.
  dim "  Waiting for executor field to be set (up to 45s)..."
  local executor_value
  if executor_value=$(poll_bead_field "$esc1_id" "executor" "escalation-" 45); then
    pass "executor field set on escalation"
    dim "    executor=$executor_value"
  else
    fail "executor field set" "executor not set within 45s"
  fi

  # Check if the bridge spawned an agent bead linked to this escalation.
  # Search for agent beads with escalation_id field matching our escalation.
  dim "  Checking for spawned agent bead..."
  sleep 5  # Allow agent creation to propagate

  local agent_beads agent_found
  agent_found="false"
  agent_beads=$("$KD_BIN" list --type=agent --status=open,in_progress --json 2>/dev/null || echo "[]")

  # Look for an agent bead with escalation_id matching our escalation
  local matching_agent
  matching_agent=$(echo "$agent_beads" | python3 -c "
import sys,json
beads=json.load(sys.stdin)
if isinstance(beads, dict):
    beads=beads.get('beads',[])
for b in beads:
    fields=b.get('fields',{})
    if fields.get('escalation_id','')=='$esc1_id':
        print(b.get('id',''))
        break
" 2>/dev/null || echo "")

  if [ -n "$matching_agent" ]; then
    pass "agent bead created for escalation"
    dim "    agent_bead=$matching_agent"
    CREATED_BEADS+=("$matching_agent")

    # Verify the agent has escalation dependency
    local deps
    deps=$("$KD_BIN" dep list "$matching_agent" --json 2>/dev/null || echo "[]")
    if grep -qF "$esc1_id" <<<"$deps"; then
      pass "agent bead has escalation dependency"
    else
      fail "agent bead dependency" "agent $matching_agent missing dependency on $esc1_id"
    fi
  else
    # Agent spawn may fail in E2E (no controller running), check if escalation
    # recorded a failure result instead
    local esc1_after
    esc1_after=$("$KD_BIN" show "$esc1_id" --json 2>/dev/null || echo "{}")
    if grep -qF '"result_status"' <<<"$esc1_after"; then
      local result_status
      result_status=$(echo "$esc1_after" | python3 -c "
import sys,json
d=json.load(sys.stdin)
print(d.get('fields',{}).get('result_status',''))
" 2>/dev/null || echo "")
      if [ "$result_status" = "failure" ]; then
        pass "agent spawn failed gracefully (expected in E2E without controller)"
        dim "    result_status=failure (spawn failed, result captured)"
      else
        fail "agent spawn" "unexpected result_status=$result_status"
      fi
    else
      fail "agent spawn" "no agent bead created and no failure result recorded"
    fi
  fi
  fi
  echo

  # ── Scenario 2: Result capture — update escalation and close ────
  blue "Scenario 2: Result capture — update escalation with result and close"

  # Create a new escalation. Do NOT approve via bridge API — the bridge's
  # SSE-triggered execution races with our manual field updates because
  # SpawnAgent fails in E2E (no controller), causing recordResult to close
  # the bead with result_status=failure before our kd update runs.
  local esc2_out esc2_id
  esc2_out=$("$KD_BIN" create "E2E execution $TEST_TAG: result capture" \
    --type=escalation \
    -l "e2e:$TEST_TAG" \
    -f 'operation_type=helm_deploy' \
    -f 'operation_spec={"chart":"gardenio","version":"test"}' \
    --actor "$KD_ACTOR" --json 2>/dev/null || echo "{}")
  esc2_id=$(echo "$esc2_out" | json_field id || echo "")

  if [ -n "$esc2_id" ]; then
    CREATED_BEADS+=("$esc2_id")

    # Simulate agent completing execution: record result_status + result and
    # close via the dedicated resolve verb (POST /v1/escalations/{id}/result).
    # A generic `kd close` on an OPEN escalation is now diverted by the daemon
    # (ka-0000005516) — it 409s and leaves the bead open — so resolve is the
    # correct close-with-result surface (mirrors the crud/v2 fix in !6561).
    local resolve2_out
    resolve2_out=$("$KD_BIN" escalation resolve "$esc2_id" \
      --status success \
      --result '{"output":"helm upgrade completed","version":"2026.96.1"}' \
      --actor "escalation-agent" 2>&1) || true
    if resolve_refused "scenario 2 resolve" "$resolve2_out"; then
      :
    else
      sleep 2

      # Verify result fields persisted (kd show --json uses spaces in key-value pairs)
      local esc2_after
      esc2_after=$("$KD_BIN" show "$esc2_id" --json 2>/dev/null || echo "{}")

      assert_contains "result_status=success persisted" '"result_status": "success"' "$esc2_after"
      assert_contains "result JSON persisted" "helm upgrade completed" "$esc2_after"
      assert_contains "escalation is closed" '"status": "closed"' "$esc2_after"
    fi
  else
    fail "scenario 2 setup" "could not create escalation"
  fi
  echo

  # ── Scenario 3: Requesting agent nudge path ────────────────────
  blue "Scenario 3: Requesting agent nudge — verify nudge fields on completion"

  # Create a fake requesting agent bead first (type=agent requires role + project fields)
  local agent_out agent_id
  agent_out=$("$KD_BIN" create "E2E requesting agent $TEST_TAG" \
    --type=agent \
    -l "e2e:$TEST_TAG" \
    -f "agent=e2e-requesting-agent-$TEST_TAG" \
    -f "agent_state=working" \
    -f "role=crew" \
    -f "project=gardenio" \
    --actor "$KD_ACTOR" --json 2>/dev/null || echo "{}")
  agent_id=$(echo "$agent_out" | json_field id || echo "")

  if [ -n "$agent_id" ]; then
    CREATED_BEADS+=("$agent_id")
    dim "  Requesting agent bead: $agent_id"

    # Create escalation with requesting_agent pointing to our agent bead
    local esc3_out esc3_id
    esc3_out=$("$KD_BIN" create "E2E execution $TEST_TAG: nudge test" \
      --type=escalation \
      -l "e2e:$TEST_TAG" \
      -f 'operation_type=general' \
      -f 'operation_spec={"action":"test_nudge"}' \
      -f "requesting_agent=$agent_id" \
      --actor "$KD_ACTOR" --json 2>/dev/null || echo "{}")
    esc3_id=$(echo "$esc3_out" | json_field id || echo "")

    if [ -n "$esc3_id" ]; then
      CREATED_BEADS+=("$esc3_id")
      dim "  Escalation with requesting_agent: $esc3_id"

      # Verify requesting_agent field was stored
      local esc3_check
      esc3_check=$("$KD_BIN" show "$esc3_id" --json 2>/dev/null || echo "{}")
      assert_contains "requesting_agent field stored" "$agent_id" "$esc3_check"

      # Simulate completion directly (skip bridge approval to avoid the
      # same race as Scenario 2 — bridge execution fails and closes the
      # bead before our resolve runs). Record result + close via the dedicated
      # resolve verb; a generic `kd close` on an OPEN escalation is diverted by
      # the daemon (ka-0000005516). requesting_agent was set at creation and is
      # preserved through resolve (additive mergeBeadFields).
      local resolve3_out
      resolve3_out=$("$KD_BIN" escalation resolve "$esc3_id" \
        --status success \
        --result '{"output":"nudge test completed"}' \
        --actor "escalation-agent" 2>&1) || true
      if resolve_refused "scenario 3 resolve" "$resolve3_out"; then
        :
      else
        sleep 2

        # Verify the escalation closed with fields preserved.
        # (Actual nudge delivery cannot be verified without a live coop endpoint;
        # the nudge path is covered by unit tests in escalations_test.go.)
        local esc3_final
        esc3_final=$("$KD_BIN" show "$esc3_id" --json 2>/dev/null || echo "{}")
        assert_contains "escalation closed with result" '"status": "closed"' "$esc3_final"
        assert_contains "result_status preserved after close" "success" "$esc3_final"
        assert_contains "requesting_agent preserved" "$agent_id" "$esc3_final"
        pass "requesting agent nudge path exercised"
        dim "    (nudge delivery cannot be verified without live coop endpoint)"
      fi
    else
      fail "scenario 3 setup" "could not create escalation with requesting_agent"
    fi
  else
    fail "scenario 3 setup" "could not create requesting agent bead"
  fi
  echo

  # ── Scenario 4: local fallback ─────────────────────────
  blue "Scenario 4: local fallback — no local agent, falls back to spawn"

  local esc4_out esc4_id
  esc4_out=$("$KD_BIN" create "E2E execution $TEST_TAG: gardenio fallback" \
    --type=escalation \
    -l "e2e:$TEST_TAG" \
    -l "project:e2e-nonexistent-project" \
    -f 'operation_type=terraform' \
    -f 'operation_spec={"action":"plan","workspace":"staging"}' \
    -f 'context=E2E test: local with no matching agent' \
    --actor "$KD_ACTOR" --json 2>/dev/null || echo "{}")
  esc4_id=$(echo "$esc4_out" | json_field id || echo "")

  if [ -n "$esc4_id" ]; then
    CREATED_BEADS+=("$esc4_id")
    dim "  Escalation created: $esc4_id"

    # Approve with execution_target=local (negative control + kd approve)
    local gb_approved=true
    approve_escalation "$esc4_id" local || gb_approved=false

    if [ "$gb_approved" = true ]; then
    # Wait for the bridge to process. Since no local agent with
    # execution_target:local label exists, it should fall back to
    # spawning a new agent (local path).
    dim "  Waiting for fallback execution (up to 30s)..."
    sleep 10

    local esc4_after
    esc4_after=$("$KD_BIN" show "$esc4_id" --json 2>/dev/null || echo "{}")

    # The escalation should have either:
    # - executor field set (fallback spawned an agent)
    # - result_status=failure (spawn failed, which is OK in E2E)
    local has_executor has_result
    has_executor=$(echo "$esc4_after" | python3 -c "
import sys,json
d=json.load(sys.stdin)
print(d.get('fields',{}).get('executor',''))
" 2>/dev/null || echo "")
    has_result=$(echo "$esc4_after" | python3 -c "
import sys,json
d=json.load(sys.stdin)
print(d.get('fields',{}).get('result_status',''))
" 2>/dev/null || echo "")

    if [ -n "$has_executor" ]; then
      pass "local fallback: executor set (agent spawned)"
      dim "    executor=$has_executor"
    elif [ "$has_result" = "failure" ]; then
      pass "local fallback: spawn attempted and failed gracefully"
      dim "    result_status=failure (expected in E2E without controller)"
    else
      # The bridge may still be processing. Check if approved_by was set.
      if grep -qF '"approved_by"' <<<"$esc4_after"; then
        pass "local approval accepted (execution pending)"
        dim "    Bridge accepted the approval; execution may still be processing"
      else
        fail "local fallback" "no executor, no result, no approval evidence"
      fi
    fi
    fi
  else
    fail "scenario 4 setup" "could not create escalation"
  fi
  echo

  # ── Scenario 5: service_account target → not implemented ───────
  blue "Scenario 5: service_account target — not implemented, records failure"

  local esc5_out esc5_id
  esc5_out=$("$KD_BIN" create "E2E execution $TEST_TAG: service_account" \
    --type=escalation \
    -l "e2e:$TEST_TAG" \
    -f 'operation_type=aws_iam' \
    -f 'operation_spec={"action":"assume_role","role_arn":"arn:aws:iam::test"}' \
    --actor "$KD_ACTOR" --json 2>/dev/null || echo "{}")
  esc5_id=$(echo "$esc5_out" | json_field id || echo "")

  if [ -n "$esc5_id" ]; then
    CREATED_BEADS+=("$esc5_id")

    # Approve with execution_target=service_account (negative control + kd approve)
    local sa_approved=true
    approve_escalation "$esc5_id" service_account || sa_approved=false

    if [ "$sa_approved" = true ]; then
    # The bridge should record failure (service_account not implemented)
    dim "  Waiting for service_account failure result (up to 30s)..."
    local sa_result
    if sa_result=$(poll_bead_field "$esc5_id" "result_status" "failure" 30); then
      pass "service_account records failure result"

      # Verify the error message mentions not implemented
      local esc5_after
      esc5_after=$("$KD_BIN" show "$esc5_id" --json 2>/dev/null || echo "{}")
      assert_contains "failure result mentions not implemented" "not yet implemented" "$esc5_after"
    else
      # Bridge may not have processed the SSE event yet, or the approve
      # endpoint handled it synchronously
      local esc5_check
      esc5_check=$("$KD_BIN" show "$esc5_id" --json 2>/dev/null || echo "{}")
      if grep -qF "not yet implemented" <<<"$esc5_check"; then
        pass "service_account failure recorded"
      elif grep -qF '"approved_by"' <<<"$esc5_check"; then
        pass "service_account approval accepted (result pending)"
        dim "    Bridge may process asynchronously; result not yet recorded"
      else
        fail "service_account failure" "no failure result within 30s"
      fi
    fi
    fi
  else
    fail "scenario 5 setup" "could not create escalation"
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
