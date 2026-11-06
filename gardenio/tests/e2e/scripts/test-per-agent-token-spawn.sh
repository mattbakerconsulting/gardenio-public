#!/usr/bin/env bash
# test-per-agent-token-spawn.sh — spawn a REAL agent with agents.perAgentTokens=true
#
# test-per-agent-token-identity.sh proves the daemon side with a hand-registered
# token. This module proves the controller side: that a pod the controller
# actually creates mounts its OWN beads token, that the pod the reconciler
# rebuilds after a delete does too, and that closing the agent revokes it.
#
# SPEND SAFETY: the seeded agent carries a deliberately non-existent image
# (registry.invalid), so its pod can only ImagePullBackOff and never starts.
#
# Flow:
#   0. Read PER_AGENT_TOKENS off the controller Deployment. Off -> SKIP loud
#      (exit 0), unless REQUIRE_PER_AGENT_TOKENS=1, where it is a FAIL: set that
#      for the post-flip rollout verification.
#   1. Seed an agent bead; wait for its pod.
#   2. The pod's secretKeyRef set names agent-token-<bead id> for the beads
#      token and NEVER kd-beads-token, in env, envFrom or volumes.
#   3. That Secret's token authenticates as the agent: a bead it creates has
#      created_by = the agent name (a spoofed X-Actor is ignored).
#   4. Delete the pod; the reconciler's recreate must again mount the per-agent
#      Secret and not kd-beads-token (the second pod-creation door).
#   5. Close the agent bead; the Secret disappears and the token stops working.
#
# Self-SKIPs (exit 0) when kubectl/jq/the controller Deployment is unavailable.

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
CONTROLLER_DEPLOY="${CONTROLLER_DEPLOY:-${NAMESPACE}-agents}"
PROJECT_NAME="${PROJECT_NAME:-gardenio}"
SHARED_SECRET="${SHARED_SECRET:-kd-beads-token}"
IMG_INVALID="${IMG_INVALID:-registry.invalid/gardenio-e2e/agent:per-agent-token-000}"
OPERATOR="${OPERATOR:-${BEADS_OPERATED_BY:-oidc:e2e-per-agent-token@example.com}}"
POD_WAIT_SECONDS="${POD_WAIT_SECONDS:-180}"
REVOKE_WAIT_SECONDS="${REVOKE_WAIT_SECONDS:-180}"
PASS=0
FAIL=0
ERRORS=()
PF_PID=""
AGENT_ID=""
TEST_TAG="pat-spawn-$$-$(date +%s)"
AGENT_NAME="e2e-${TEST_TAG}"
AGENT_TOKEN=""
CREATED_BEADS=()

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

# The Secret name the controller derives from the bead id (lowercased: bead ids
# are mixed-case, Secret names are DNS-1123).
secret_name_for() { printf 'agent-token-%s' "$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')"; }

# Every Secret name a pod references, one per line: env secretKeyRef, envFrom
# secretRef, and secret volumes (including projected sources).
pod_secret_refs() {
  jq -r '
    [ (.spec.containers[]?, .spec.initContainers[]?)
        | ((.env // [])[]? | select(.valueFrom.secretKeyRef) | .valueFrom.secretKeyRef.name),
          ((.envFrom // [])[]? | select(.secretRef) | .secretRef.name)
    ] + [ (.spec.volumes // [])[]?
        | (select(.secret) | .secret.secretName),
          ((.projected.sources // [])[]? | select(.secret) | .secret.name)
    ] | .[]' <<<"$1" | sort -u
}

pod_json_for_agent() {
  kubectl -n "$NAMESPACE" get pods -l "gardenio.io/agent=${AGENT_NAME}" -o json 2>/dev/null \
    | jq -c '.items[0] // empty' 2>/dev/null || true
}

wait_for_pod() {
  local deadline=$(( $(date +%s) + POD_WAIT_SECONDS )) pod=""
  while [ "$(date +%s)" -lt "$deadline" ]; do
    pod="$(pod_json_for_agent)"
    [ -n "$pod" ] && { printf '%s' "$pod"; return 0; }
    sleep 5
  done
  return 1
}

# ── Setup ─────────────────────────────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE} kbeads daemon..."
  local port=19142
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-agent-token-spawn.log 2>&1 &
  PF_PID=$!
  sleep 3
  export BEADS_HTTP_URL="http://localhost:${port}"
  dim "  Daemon at $BEADS_HTTP_URL (pid $PF_PID)"
}

setup_admin_token() {
  if [ -n "${BEADS_ADMIN_TOKEN:-}" ]; then return 0; fi
  if [ -n "${BEADS_TOKEN:-}" ]; then export BEADS_ADMIN_TOKEN="$BEADS_TOKEN"; return 0; fi
  local secret
  secret=$(kubectl -n "$NAMESPACE" get secret "$SHARED_SECRET" \
    -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
  if [ -n "$secret" ]; then export BEADS_ADMIN_TOKEN="$secret"; return 0; fi
  return 1
}

cleanup() {
  if [ -n "${BEADS_ADMIN_TOKEN:-}" ] && [ -n "${BEADS_HTTP_URL:-}" ]; then
    local id
    for id in ${AGENT_ID:+"$AGENT_ID"} "${CREATED_BEADS[@]}"; do
      [ -n "$id" ] || continue
      curl -s -o /dev/null -X POST "${BEADS_HTTP_URL}/v1/beads/${id}/close" \
        -H "Authorization: Bearer ${BEADS_ADMIN_TOKEN}" -H "X-Operated-By: ${OPERATOR}" \
        -H 'Content-Type: application/json' -d '{}' || true
    done
  fi
  if [ -n "$PF_PID" ]; then kill "$PF_PID" 2>/dev/null || true; fi
}
trap cleanup EXIT

# ── Scenarios ─────────────────────────────────────────────────────
scenario_flag_state() {
  blue "Step 0: is the controller running with PER_AGENT_TOKENS=true?"
  local val
  val=$(kubectl -n "$NAMESPACE" get deploy "$CONTROLLER_DEPLOY" -o json 2>/dev/null \
    | jq -r '[.spec.template.spec.containers[].env[]? | select(.name=="PER_AGENT_TOKENS") | .value] | first // ""' 2>/dev/null || true)
  if [ "$val" = "true" ]; then
    pass "controller ${CONTROLLER_DEPLOY} has PER_AGENT_TOKENS=true"
    return 0
  fi
  if [ "${REQUIRE_PER_AGENT_TOKENS:-}" = "1" ]; then
    fail "PER_AGENT_TOKENS" "controller ${CONTROLLER_DEPLOY} reports '${val:-unset}', REQUIRE_PER_AGENT_TOKENS=1 wants true"
    return 1
  fi
  dim "  SKIP: PER_AGENT_TOKENS='${val:-unset}' on ${CONTROLLER_DEPLOY} (flag off, or deployment not found); set REQUIRE_PER_AGENT_TOKENS=1 to make this a failure"
  return 2
}

seed_agent() {
  blue "Step 1: seed a spend-safe agent bead and wait for its pod"
  local body
  body=$(NAME="$AGENT_NAME" PROJ="$PROJECT_NAME" IMG="$IMG_INVALID" TAG="$TEST_TAG" jq -n '{
    type: "agent", title: env.NAME,
    labels: ["e2e", "per-agent-token-spawn", ("project:" + env.PROJ)],
    fields: {agent: env.NAME, agent_state: "spawning", mode: "crew", role: "crew", project: env.PROJ, image: env.IMG}}')
  AGENT_ID=$(curl -sf --max-time 10 -X POST "${BEADS_HTTP_URL}/v1/beads" \
    -H "Authorization: Bearer ${BEADS_ADMIN_TOKEN}" -H "X-Operated-By: ${OPERATOR}" \
    -H 'X-No-Auto-Inherit: 1' -H 'Content-Type: application/json' -d "$body" \
    | jq -r '.id // empty' 2>/dev/null || true)
  if [ -z "$AGENT_ID" ]; then
    fail "seed agent bead" "POST /v1/beads returned no id"
    return 1
  fi
  pass "seeded agent bead ${AGENT_ID} (${AGENT_NAME}, image=registry.invalid)"
}

assert_pod_uses_agent_secret() {
  local label="$1" pod="$2" want refs
  want="$(secret_name_for "$AGENT_ID")"
  refs="$(pod_secret_refs "$pod")"
  if grep -qx -- "$want" <<<"$refs"; then
    pass "${label}: pod references ${want}"
  else
    fail "${label}: per-agent Secret" "pod does not reference ${want}; refs: $(tr '\n' ' ' <<<"$refs")"
  fi
  if grep -qx -- "$SHARED_SECRET" <<<"$refs"; then
    fail "${label}: shared token" "pod still references ${SHARED_SECRET} (the primary beads token is in an agent pod)"
  else
    pass "${label}: pod does not reference ${SHARED_SECRET}"
  fi
}

scenario_first_pod() {
  local pod
  if ! pod="$(wait_for_pod)"; then
    fail "pod created" "no pod labelled gardenio.io/agent=${AGENT_NAME} within ${POD_WAIT_SECONDS}s"
    return 1
  fi
  pass "controller created a pod for the agent"
  blue "Step 2: the pod mounts its own beads token, not the shared one"
  assert_pod_uses_agent_secret "spawn" "$pod"
}

scenario_token_acts_as_agent() {
  blue "Step 3: the per-agent Secret's token acts as the agent"
  AGENT_TOKEN=$(kubectl -n "$NAMESPACE" get secret "$(secret_name_for "$AGENT_ID")" \
    -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
  if [ -z "$AGENT_TOKEN" ]; then
    fail "per-agent Secret" "$(secret_name_for "$AGENT_ID") has no token"
    return 1
  fi
  local out id
  out=$(curl -s -X POST "${BEADS_HTTP_URL}/v1/beads" -H "Authorization: Bearer ${AGENT_TOKEN}" \
    -H 'Content-Type: application/json' -H 'X-Actor: impostor' -H "X-Operated-By: ${OPERATOR}" \
    -d "{\"title\":\"e2e ${TEST_TAG}\",\"type\":\"task\",\"labels\":[\"e2e:${TEST_TAG}\"]}")
  id=$(jq -r '.id // empty' <<<"$out" 2>/dev/null || true)
  [ -n "$id" ] && CREATED_BEADS+=("$id")
  if [ "$(jq -r '.created_by // empty' <<<"$out" 2>/dev/null)" = "$AGENT_NAME" ]; then
    pass "a bead created with the Secret's token has created_by=${AGENT_NAME} (X-Actor ignored)"
  else
    fail "token identity" "$(jq -c '{created_by, error}' <<<"$out" 2>/dev/null || echo "$out")"
  fi
  # No project label was sent: the project-scoped token must still see its own bead.
  local code
  code=$(curl -s -o /dev/null -w '%{http_code}' "${BEADS_HTTP_URL}/v1/beads/${id:-none}" \
    -H "Authorization: Bearer ${AGENT_TOKEN}")
  if [ "$code" = "200" ]; then
    pass "the agent reads back the bead it created without a project label"
  else
    fail "read-back of own bead" "status $code (want 200)"
  fi
}

scenario_reconciler_recreate() {
  blue "Step 4: delete the pod; the reconciler's recreate must use the per-agent Secret too"
  local old new deadline
  old=$(pod_json_for_agent | jq -r '.metadata.uid // empty')
  kubectl -n "$NAMESPACE" delete pod -l "gardenio.io/agent=${AGENT_NAME}" --wait=true >/dev/null 2>&1 || true
  deadline=$(( $(date +%s) + POD_WAIT_SECONDS ))
  new=""
  while [ "$(date +%s)" -lt "$deadline" ]; do
    new="$(pod_json_for_agent)"
    if [ -n "$new" ] && [ "$(jq -r '.metadata.uid' <<<"$new")" != "$old" ]; then break; fi
    new=""
    sleep 5
  done
  if [ -z "$new" ]; then
    fail "recreate" "no replacement pod within ${POD_WAIT_SECONDS}s"
    return 1
  fi
  pass "a replacement pod was created"
  assert_pod_uses_agent_secret "recreate" "$new"
}

scenario_revoked_on_close() {
  blue "Step 5: closing the agent revokes its token and deletes the Secret"
  curl -s -o /dev/null -X POST "${BEADS_HTTP_URL}/v1/beads/${AGENT_ID}/close" \
    -H "Authorization: Bearer ${BEADS_ADMIN_TOKEN}" -H "X-Operated-By: ${OPERATOR}" \
    -H 'Content-Type: application/json' -d '{}' || true
  local deadline=$(( $(date +%s) + REVOKE_WAIT_SECONDS )) gone=0 code=""
  while [ "$(date +%s)" -lt "$deadline" ]; do
    if ! kubectl -n "$NAMESPACE" get secret "$(secret_name_for "$AGENT_ID")" >/dev/null 2>&1; then gone=1; break; fi
    sleep 5
  done
  if [ "$gone" = "1" ]; then
    pass "Secret $(secret_name_for "$AGENT_ID") was deleted after close"
  else
    fail "Secret cleanup" "still present ${REVOKE_WAIT_SECONDS}s after close"
  fi
  code=$(curl -s -o /dev/null -w '%{http_code}' "${BEADS_HTTP_URL}/v1/beads?limit=1" \
    -H "Authorization: Bearer ${AGENT_TOKEN}")
  if [ "$code" = "401" ]; then
    pass "the revoked token no longer authenticates (401)"
  else
    fail "token revoked" "status $code (want 401)"
  fi
}

for tool in kubectl jq curl; do
  command -v "$tool" >/dev/null 2>&1 || { dim "SKIP: ${tool} not available"; exit 0; }
done

rc=0
scenario_flag_state || rc=$?
if [ "$rc" = "2" ]; then exit 0; fi
if [ "$rc" = "0" ]; then
  setup_portforward
  if setup_admin_token; then
    if seed_agent && scenario_first_pod; then
      scenario_token_acts_as_agent || true
      scenario_reconciler_recreate || true
      scenario_revoked_on_close || true
    fi
  else
    dim "no admin token reachable; skipping"
    exit 0
  fi
fi

echo "PASS=$PASS FAIL=$FAIL"
if [ "$FAIL" -gt 0 ]; then
  printf '  %s\n' "${ERRORS[@]}"
fi
[ "$FAIL" -eq 0 ]
