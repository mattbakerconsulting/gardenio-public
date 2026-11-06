#!/usr/bin/env bash
# test-per-agent-token-identity.sh — per-agent token identity E2E (P7 Task 2)
#
# Daemon-side rail for agents.perAgentTokens=true (spec D1): an agent-class
# token (fleet name `agent-<bead-id>`, subject = the agent name) must act as
# that agent and nothing more.
#
# Flow:
#   1. Positive control: register an agent-class token with the admin token via
#      POST /v1/bridge-tokens -> 201.
#   2. POST /v1/beads with the agent token and a spoofed X-Actor -> created_by
#      is the agent name, not the header.
#   3. POST /v1/bridge-tokens with the agent token -> 403 (same request as the
#      step 1 positive control, different bearer).
#
# Skips gracefully (exit 0) when the admin token isn't reachable.

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
PASS=0
FAIL=0
ERRORS=()
PF_PID=""
TEST_TAG="agent-ident-$$-$(date +%s)"
AGENT_NAME="e2e-agent-${TEST_TAG}"
AGENT_FLEET="agent-gd-e2e${TEST_TAG//[^a-z0-9]/}"
AGENT_TOKEN=""

# ── Helpers ───────────────────────────────────────────────────────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

pass() { green "  ✓ $1"; PASS=$((PASS + 1)); }
fail() {
  red "  ✗ $1"
  [ -n "${2:-}" ] && dim "    $2"
  FAIL=$((FAIL + 1))
  ERRORS+=("$1: ${2:-}")
}

# ── Setup: port-forward kbeads daemon ────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE} kbeads daemon..."
  local port=19141
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-agent-identity.log 2>&1 &
  PF_PID=$!
  sleep 3
  export BEADS_HTTP_URL="http://localhost:${port}"
  dim "  Daemon at $BEADS_HTTP_URL (pid $PF_PID)"
}

# ── Setup: discover admin token ──────────────────────────────────
setup_admin_token() {
  if [ -n "${BEADS_ADMIN_TOKEN:-}" ]; then
    return 0
  fi
  if [ -n "${BEADS_TOKEN:-}" ]; then
    export BEADS_ADMIN_TOKEN="$BEADS_TOKEN"
    return 0
  fi
  local secret
  secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
    -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
  if [ -n "$secret" ]; then
    export BEADS_ADMIN_TOKEN="$secret"
    dim "  Admin token loaded from secret kd-beads-token"
    return 0
  fi
  return 1
}

# ── Cleanup ───────────────────────────────────────────────────────
cleanup() {
  if [ -n "${BEADS_ADMIN_TOKEN:-}" ] && [ -n "${BEADS_HTTP_URL:-}" ]; then
    curl -s -o /dev/null -X DELETE \
      -H "Authorization: Bearer $BEADS_ADMIN_TOKEN" \
      "${BEADS_HTTP_URL}/v1/bridge-tokens/${AGENT_FLEET}" || true
  fi
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# ── Scenarios ─────────────────────────────────────────────────────
register_agent_token() {
  blue "Step 1: register an agent-class token (positive control for the 403 below)"
  AGENT_TOKEN="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  local code
  code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "${BEADS_HTTP_URL}/v1/bridge-tokens" \
    -H "Authorization: Bearer ${BEADS_ADMIN_TOKEN}" -H 'Content-Type: application/json' \
    -d "{\"token\":\"${AGENT_TOKEN}\",\"bridge\":\"${AGENT_FLEET}\",\"project\":\"gardenio\",\"subject\":\"${AGENT_NAME}\",\"ttl\":\"10m\"}")
  if [ "$code" = "201" ]; then
    pass "registered an agent-class token"
  else
    fail "register agent token" "status $code (want 201)"
    return 1
  fi
}

scenario_created_by_is_agent() {
  blue "Step 2: created_by is the agent name; X-Actor is ignored"
  local out
  out=$(curl -s -X POST "${BEADS_HTTP_URL}/v1/beads" -H "Authorization: Bearer ${AGENT_TOKEN}" \
    -H 'Content-Type: application/json' -H 'X-Actor: impostor' \
    -d "{\"title\":\"e2e ${TEST_TAG}\",\"type\":\"task\",\"labels\":[\"project:gardenio\",\"e2e:${TEST_TAG}\"]}")
  if [ "$(jq -r '.created_by' <<<"$out")" = "$AGENT_NAME" ]; then
    pass "created_by is the agent name, X-Actor ignored"
  else
    fail "created_by" "$(jq -c '{created_by}' <<<"$out" 2>/dev/null || echo "$out")"
  fi
}

scenario_cannot_mint() {
  blue "Step 3: an agent token cannot mint a bridge token"
  local code
  code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "${BEADS_HTTP_URL}/v1/bridge-tokens" \
    -H "Authorization: Bearer ${AGENT_TOKEN}" -H 'Content-Type: application/json' \
    -d '{"token":"should-not-exist-'"$TEST_TAG"'","bridge":"slack-bridge"}')
  if [ "$code" = "403" ]; then
    pass "agent token cannot mint a bridge token"
  else
    fail "agent mint" "status $code (want 403)"
  fi
}

setup_portforward
if ! setup_admin_token; then
  dim "no admin token reachable; skipping"
  exit 0
fi
if register_agent_token; then
  scenario_created_by_is_agent
  scenario_cannot_mint
fi

echo "PASS=$PASS FAIL=$FAIL"
if [ "$FAIL" -gt 0 ]; then
  printf '  %s\n' "${ERRORS[@]}"
fi
[ "$FAIL" -eq 0 ]
