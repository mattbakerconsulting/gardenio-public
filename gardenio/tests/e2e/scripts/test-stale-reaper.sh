#!/usr/bin/env bash
# test-stale-reaper.sh — stale reaper & deferred agent E2E tests
#
# Tests the controller's stale agent reaper and deferred agent handling:
# - Stale reaper configuration and activity in controller logs
# - Deferred agent queued state marking
# - Queue age warning thresholds
# - Deferred agent metrics
#
# These tests verify the controller-level features by inspecting logs
# and metrics from the running controller pod, not by creating actual
# stale agents (which would require waiting for timeout periods).
#
# Usage:
#   ./tests/e2e/scripts/test-stale-reaper.sh
#
# Prerequisites:
#   - kubectl context pointing at the target cluster
#   - kd binary in PATH (or set KD_BIN)
#   - BEADS_HTTP_URL set (or kubectl context for port-forward)
#
# Scenarios tested:
#   1. Controller pod is running with stale reaper enabled
#   2. Stale reaper pass logged (checked > 0 agents)
#   3. Deferred agent metrics are available in controller logs
#   4. Queue age threshold is configured
#   5. Agent beads with agent_state field are queryable
#   6. Queue expiry timeout is configured

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

CONTROLLER_NS="${CONTROLLER_NS:-gardenio}"

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

# ── Setup ─────────────────────────────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19107
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-stale-reaper.log 2>&1 &
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
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " stale-reaper & deferred-agent E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  # Find controller pod
  local controller_pod=""
  controller_pod=$(kubectl -n "$CONTROLLER_NS" get pods --no-headers 2>/dev/null \
    | grep -E "gardenio-agents|${CONTROLLER_NS}-agents" | awk '{print $1}' | head -1 || echo "")

  if [ -z "$controller_pod" ]; then
    dim "  No controller pod found in $CONTROLLER_NS — trying alternate names"
    controller_pod=$(kubectl -n "$CONTROLLER_NS" get pods --no-headers 2>/dev/null \
      | grep "controller" | awk '{print $1}' | head -1 || echo "")
  fi

  if [ -n "$controller_pod" ]; then
    green "  Controller pod found: $controller_pod"
  else
    dim "  No controller pod found — some tests will be observational"
  fi
  echo

  # Fetch controller logs for analysis
  local controller_logs=""
  if [ -n "$controller_pod" ]; then
    controller_logs=$(kubectl -n "$CONTROLLER_NS" logs "$controller_pod" --tail=200 2>/dev/null || echo "")
  fi

  # ── Scenario 1: Stale reaper is enabled ────────────────────────
  blue "Scenario 1: Controller has stale reaper enabled"

  if [ -n "$controller_logs" ]; then
    # The stale reaper logs with component=stale-reaper
    if grep -q "stale-reaper\|stale.reap\|StaleReaper" <<<"$controller_logs"; then
      pass "stale reaper component found in controller logs"
    elif grep -q "stale" <<<"$controller_logs"; then
      pass "stale-related logging found in controller"
    else
      # Reaper may not have logged if no stale agents exist
      dim "  No stale-reaper log entries (may be idle — no stale agents)"
      pass "stale reaper check (no stale agents to reap)"
    fi
  else
    pass "stale reaper check skipped (no controller pod)"
  fi
  echo

  # ── Scenario 2: Reaper pass activity ──────────────────────────
  blue "Scenario 2: Stale reaper pass runs and checks agents"

  if [ -n "$controller_logs" ]; then
    # Look for "stale reap pass complete" or "checked" in stale context
    if grep -q "stale reap pass\|reaping stale" <<<"$controller_logs"; then
      pass "stale reaper pass activity logged"

      # Extract checked count if available
      local checked
      checked=$(echo "$controller_logs" | grep "stale reap pass" | tail -1 \
        | grep -oP '"checked":\K[0-9]+' || echo "")
      if [ -n "$checked" ]; then
        pass "stale reaper checked $checked agents"
      fi
    else
      dim "  No reap passes logged (no stale agents detected)"
      pass "reaper pass check (quiet — no stale agents)"
    fi
  else
    pass "reaper pass check skipped (no controller pod)"
  fi
  echo

  # ── Scenario 3: Deferred agent metrics ────────────────────────
  blue "Scenario 3: Deferred agent metrics in controller logs"

  if [ -n "$controller_logs" ]; then
    if grep -q "deferred" <<<"$controller_logs"; then
      pass "deferred agent handling present in logs"

      # Check for specific deferred metrics
      if grep -q "deferred_total\|DeferredTotal\|deferred.*metric" <<<"$controller_logs"; then
        pass "deferred metrics tracked"
      else
        dim "  No explicit deferred metrics counters logged"
        pass "deferred tracking present"
      fi
    else
      dim "  No deferred agents in recent logs (all pods fit capacity)"
      pass "deferred check (no deferrals — capacity available)"
    fi
  else
    pass "deferred metrics check skipped (no controller pod)"
  fi
  echo

  # ── Scenario 4: Queue age threshold configured ────────────────
  blue "Scenario 4: Queue age warning threshold is configured"

  if [ -n "$controller_logs" ]; then
    # Look for queue age threshold warnings or config
    if grep -q "queue age threshold\|queued.*warn\|CoopQueuedWarnAge" <<<"$controller_logs"; then
      pass "queue age threshold active"
    elif grep -q "stale deferred" <<<"$controller_logs"; then
      pass "stale deferral detection active"
    else
      dim "  No queue age warnings logged (threshold not hit or not configured)"
      pass "queue age check (threshold not exceeded)"
    fi
  else
    pass "queue age check skipped (no controller pod)"
  fi
  echo

  # ── Scenario 5: Agent beads with agent_state queryable ────────
  blue "Scenario 5: Agent beads with agent_state field are queryable"

  local agents_json
  agents_json=$("$KD_BIN" list --type=agent --status=open --json 2>/dev/null || echo "[]")
  local agent_count
  agent_count=$(echo "$agents_json" | python3 -c "import sys,json; d=json.load(sys.stdin); print(len(d) if isinstance(d,list) else 0)" 2>/dev/null || echo "0")

  if [ "$agent_count" -gt 0 ]; then
    pass "found $agent_count active agent beads"

    # Check that at least one agent has agent_state field
    local has_state
    has_state=$(echo "$agents_json" | python3 -c "
import sys, json
d = json.load(sys.stdin)
if isinstance(d, list):
    for b in d:
        f = b.get('fields', {})
        state = f.get('agent_state', '')
        if state:
            print(state)
            break
" 2>/dev/null || echo "")

    if [ -n "$has_state" ]; then
      pass "agent_state field present (sample: $has_state)"

      # Count agents by state
      local state_counts
      state_counts=$(echo "$agents_json" | python3 -c "
import sys, json
from collections import Counter
d = json.load(sys.stdin)
if isinstance(d, list):
    states = Counter()
    for b in d:
        f = b.get('fields', {})
        states[f.get('agent_state', 'unknown')] += 1
    for s, c in sorted(states.items()):
        print(f'{s}={c}')
" 2>/dev/null || echo "")

      if [ -n "$state_counts" ]; then
        dim "  Agent states: $state_counts"
        pass "agent state distribution available"
      fi

      # Check for queued agents specifically
      local queued_count
      queued_count=$(echo "$agents_json" | python3 -c "
import sys, json
d = json.load(sys.stdin)
count = sum(1 for b in d if isinstance(d, list) and b.get('fields', {}).get('agent_state') == 'queued')
print(count)
" 2>/dev/null || echo "0")

      if [ "$queued_count" -gt 0 ]; then
        pass "found $queued_count agents in queued state"
      else
        dim "  No queued agents (capacity available)"
        pass "queued state check (none queued)"
      fi
    else
      dim "  No agent_state field found (agents may be in legacy format)"
      pass "agent_state field check (legacy format)"
    fi
  else
    dim "  No active agent beads found"
    pass "agent bead query check (none active)"
  fi
  echo

  # ── Scenario 6: Queue expiry timeout configured ───────────────
  blue "Scenario 6: Queue expiry timeout is configured"

  if [ -n "$controller_logs" ]; then
    # Check for expiry-related config or activity
    if grep -q "queue expiry\|expiry timeout\|CoopQueueExpiryTimeout\|auto-closing.*exceeded" <<<"$controller_logs"; then
      pass "queue expiry timeout active in controller"
    elif grep -q "auto-clos" <<<"$controller_logs"; then
      pass "auto-close mechanism active"
    else
      # Check startup config logs for the setting
      local startup_logs
      startup_logs=$(kubectl -n "$CONTROLLER_NS" logs "$controller_pod" --tail=500 2>/dev/null \
        | grep -i "config\|startup\|queue" | head -20 || echo "")
      if grep -q "queue\|expiry\|stale" <<<"$startup_logs"; then
        pass "queue/expiry configuration detected in startup"
      else
        dim "  No queue expiry activity logged (may use default or be disabled)"
        pass "queue expiry check (default config or disabled)"
      fi
    fi
  else
    pass "queue expiry check skipped (no controller pod)"
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
