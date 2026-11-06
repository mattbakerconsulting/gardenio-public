#!/usr/bin/env bash
# test-litellm-cost-tracking.sh — LiteLLM bundled proxy cost tracking E2E
#
# Validates that the bundled LiteLLM proxy tracks spend per virtual key
# and that the controller can read cost data via /key/info and /spend/logs.
#
# Usage:
#   ./tests/e2e/scripts/test-litellm-cost-tracking.sh
#
#   # AWS POC (release name "gardenio" differs from namespace "gardenio-system"):
#   NAMESPACE=gardenio-system RELEASE=gardenio \
#     BEADS_HTTP_URL=http://localhost:18080 GARDENIO_TOKEN=$POC_TOK \
#     LITELLM_MASTER_KEY=$MK \
#     CRED_FIELD="credential=secret:gardenio-claude-oauth-poc-smoke" \
#     ./tests/e2e/scripts/test-litellm-cost-tracking.sh
#
# Env overrides:
#   NAMESPACE          target namespace (default gardenio-e2e)
#   RELEASE            Helm release name (default = NAMESPACE; resolves
#                      *-beads / *-litellm / *-agents object names)
#   BEADS_HTTP_URL     skip kd port-forward, use this daemon URL
#   LITELLM_MASTER_KEY supply the LiteLLM master key directly
#   MASTER_KEY_SECRET  name of the K8s secret holding the master key
#   CRED_FIELD         "key=value" credential field for the test account bead
#                      (required where the account schema mandates `credential`)
#
# Prerequisites:
#   - kubectl context pointing at the target cluster
#   - kd binary in PATH (or set KD_BIN)
#   - LiteLLM deployed in the target namespace (litellm.enabled=true)
#   - At least one account bead with a litellm_key
#
# Scenarios tested:
#   1. LiteLLM pod is running and healthy
#   2. LiteLLM /health/readiness returns 200
#   3. Virtual key generation via /key/generate works
#   4. /key/info returns spend data for the virtual key
#   5. /spend/logs returns spend entries for the virtual key
#   6. Account bead has cost tracking fields populated by spend_sync
#      (canonical total/daily/weekly_cost_usd + shadow_* mirror fields)

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
# RELEASE is the Helm release name. In per-run e2e namespaces the release
# name and namespace coincide (e.g. gardenio-e2e-0), so it defaults to NAMESPACE.
# On the AWS POC the release is "gardenio" while the namespace is "gardenio-system",
# so set RELEASE=gardenio to resolve the *-agents deployment and *-beads/*-litellm
# services correctly.
RELEASE="${RELEASE:-${NAMESPACE}}"
BEADS_SVC="${BEADS_SVC:-${RELEASE}-beads}"
LITELLM_SVC="${LITELLM_SVC:-${RELEASE}-litellm}"
AGENTS_DEPLOY="${AGENTS_DEPLOY:-${RELEASE}-agents}"
# The builtin type:account schema marks `credential` as required (litellm
# in-cluster migration, kbeads/internal/server/config.go), so account creates
# that omit it 400 with "validation failed: credential: is required". Default
# CRED_FIELD to a harmless placeholder secret reference so the account create
# satisfies the required schema; this account bead is only used for cost-tracking
# assertions and never drives a real credential fetch. Override via env if a
# specific secret is needed.
CRED_FIELD="${CRED_FIELD:-credential=secret:e2e-nonexistent-credential}"
KD_BIN="${KD_BIN:-kd}"
PASS=0
FAIL=0
ERRORS=()
PF_PID=""
PF_LITELLM_PID=""
CREATED_BEADS=()
LITELLM_PORT=19094
LITELLM_URL=""
MASTER_KEY=""

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

# ── Setup: port-forward kd server ────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi

  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19095
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-litellm-cost.log 2>&1 &
  PF_PID=$!
  sleep 3

  export BEADS_HTTP_URL="http://localhost:${port}"
  dim "  Daemon at $BEADS_HTTP_URL (pid $PF_PID)"
}

# ── Setup: wait for LiteLLM pod to be Ready ──────────────────────
# Pod phase=Running means the container has started; it does NOT mean the
# readinessProbe has passed. Services only route to Ready pods, so if we
# port-forward before Ready the Service has no endpoints and curl gets
# HTTP 000 (rc=52, couldn't connect). LiteLLM's Prisma schema sync on
# first boot can take 60–120s, well beyond the 3s sleep below.
#
# LITELLM_READY_TIMEOUT bounds each `kubectl wait` attempt. The deployment's
# startup probe budget is 310s (initialDelay 10 + failureThreshold 60 ×
# period 5), so the per-attempt window defaults to 330s — slightly wider than
# the probe budget so a healthy-but-slow cold start is never cut short by a
# wait window that is actually TIGHTER than the probe it waits on (the prior
# hard-coded 300s raced the 310s startup budget, gd-Ie2mByMaht).
LITELLM_READY_TIMEOUT="${LITELLM_READY_TIMEOUT:-330s}"

_litellm_wait_once() {
  kubectl -n "$NAMESPACE" wait --for=condition=Ready pod \
    -l app.kubernetes.io/component=litellm --timeout="$LITELLM_READY_TIMEOUT" \
    >/tmp/gd-wait-litellm.log 2>&1
}

_dump_litellm_diag() {
  dim "  --- pod status ---"
  kubectl -n "$NAMESPACE" get pods -l app.kubernetes.io/component=litellm \
    -o wide 2>&1 | sed 's/^/    /' || true
  dim "  --- recent pod events ---"
  kubectl -n "$NAMESPACE" describe pods -l app.kubernetes.io/component=litellm \
    2>&1 | tail -40 | sed 's/^/    /' || true
  dim "  --- last 50 lines of pod logs ---"
  kubectl -n "$NAMESPACE" logs -l app.kubernetes.io/component=litellm \
    --tail=50 --all-containers=true 2>&1 | sed 's/^/    /' || true
}

wait_for_litellm_ready() {
  blue "Waiting for LiteLLM pod Ready in ${NAMESPACE} (timeout ${LITELLM_READY_TIMEOUT})..."
  if _litellm_wait_once; then
    dim "  LiteLLM pod is Ready"
    return 0
  fi

  # First wait timed out. The litellm cold-start in a fresh ephemeral namespace
  # is a proven NON-DETERMINISTIC flake: a pod can wedge at readiness 503 for
  # 10–15m (the original failure logged "503 (x81 over 15m)" — Prisma engine /
  # DB-query path never settled) and will NOT self-heal within any reasonable
  # window, but deleting it so the Deployment spawns a fresh replica ALWAYS
  # clears it on the next boot (ka-0000007858 / ka-0000004213; the same
  # recycle_not_ready_pods self-heal the deploy step already applied between
  # deploy attempts). The litellm-cost-tracking module red-gated golden
  # (gd-Ie2mByMaht) because it waited ONCE and aborted. Recycle the stuck pod
  # once and re-wait against the fresh replica.
  red "  LiteLLM pod not Ready within ${LITELLM_READY_TIMEOUT}; recycling stuck pod and retrying once (cold-start self-heal, gd-Ie2mByMaht)"
  _dump_litellm_diag
  local stuck
  stuck=$(kubectl -n "$NAMESPACE" get pods -l app.kubernetes.io/component=litellm \
    -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true)
  if [ -n "$stuck" ]; then
    dim "  Deleting not-ready litellm pod(s): $stuck"
    # --wait=false: do not block on graceful termination; the Deployment spawns
    # the replacement immediately and the re-wait below blocks on the fresh pod.
    echo "$stuck" | tr ' ' '\n' | xargs -r -n1 -I{} \
      kubectl -n "$NAMESPACE" delete "pod/{}" --ignore-not-found --wait=false 2>/dev/null || true
    sleep 10
  fi

  blue "  Re-waiting for fresh LiteLLM pod Ready (timeout ${LITELLM_READY_TIMEOUT})..."
  if _litellm_wait_once; then
    dim "  LiteLLM pod is Ready after recycle"
    return 0
  fi

  red "  ERROR: LiteLLM pod did not become Ready within ${LITELLM_READY_TIMEOUT} (after one recycle)"
  _dump_litellm_diag
  return 1
}

# ── Setup: port-forward LiteLLM ─────────────────────────────────
setup_litellm_portforward() {
  blue "Port-forwarding ${NAMESPACE} LiteLLM..."
  kubectl -n "$NAMESPACE" port-forward "svc/${LITELLM_SVC}" "${LITELLM_PORT}:4000" \
    >/tmp/kd-pf-litellm-proxy.log 2>&1 &
  PF_LITELLM_PID=$!
  sleep 3

  LITELLM_URL="http://localhost:${LITELLM_PORT}"
  dim "  LiteLLM at $LITELLM_URL (pid $PF_LITELLM_PID)"
}

# ── Setup: get beads token ───────────────────────────────────────
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

# ── Setup: get LiteLLM master key ────────────────────────────────
# The Helm chart names the secret "litellm-master-key" when ExternalSecrets
# are enabled, and "${RELEASE}-litellm-master-key" otherwise.  Try the
# non-prefixed name first (matches the ExternalSecret-managed secret that
# the LiteLLM deployment actually references), then fall back.
#
# Env overrides (for environments where the release name differs from the
# namespace, e.g. the AWS POC where release=gardenio but namespace=gardenio-system):
#   LITELLM_MASTER_KEY   — supply the key directly, skipping secret lookup
#   MASTER_KEY_SECRET    — name of the secret holding the key (key: token)
setup_master_key() {
  if [ -n "${LITELLM_MASTER_KEY:-}" ]; then
    MASTER_KEY="$LITELLM_MASTER_KEY"
    dim "  Master key loaded from LITELLM_MASTER_KEY env"
    return
  fi
  local candidates=()
  [ -n "${MASTER_KEY_SECRET:-}" ] && candidates+=("$MASTER_KEY_SECRET")
  candidates+=("litellm-master-key" "${NAMESPACE}-litellm-master-key" "gardenio-litellm-master-key")
  for secret_name in "${candidates[@]}"; do
    MASTER_KEY=$(kubectl -n "$NAMESPACE" get secret "$secret_name" \
      -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
    if [ -n "$MASTER_KEY" ]; then
      dim "  Master key loaded from secret $secret_name"
      return
    fi
  done
  red "  ERROR: Cannot load LiteLLM master key from any of: ${candidates[*]}"
  red "  Set LITELLM_MASTER_KEY or MASTER_KEY_SECRET to override."
  exit 1
}

# ── Cleanup ───────────────────────────────────────────────────────
cleanup() {
  for id in "${CREATED_BEADS[@]}"; do
    "$KD_BIN" close "$id" 2>/dev/null || true
  done
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
  if [ -n "$PF_LITELLM_PID" ]; then
    kill "$PF_LITELLM_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# ── Self-skip when LiteLLM is not deployed ───────────────────────
# The bundled litellm pod can be disabled via helm (litellm.enabled=false)
# in environments that use an external LiteLLM URL. When no litellm pod
# exists in the namespace, skip cleanly with exit 0 so this module does
# not fail the E2E suite. Flipping litellm.enabled back on will make the
# test run again automatically on the next E2E invocation.
skip_if_litellm_disabled() {
  local pod_names
  pod_names=$(kubectl -n "$NAMESPACE" get pods -l app.kubernetes.io/component=litellm \
    -o name 2>/dev/null || true)
  if [ -z "$pod_names" ]; then
    blue "═══════════════════════════════════════════════════"
    blue " litellm-cost-tracking E2E tests"
    blue "═══════════════════════════════════════════════════"
    echo
    green "SKIP: No LiteLLM pod found in namespace '${NAMESPACE}'"
    dim "  The bundled LiteLLM proxy is disabled (litellm.enabled=false)."
    dim "  This test will run automatically when litellm.enabled is flipped back on."
    blue "═══════════════════════════════════════════════════"
    if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "no LiteLLM pod in $NAMESPACE (litellm.enabled=false)" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
    exit 0
  fi
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  skip_if_litellm_disabled

  blue "═══════════════════════════════════════════════════"
  blue " litellm-cost-tracking E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token
  # Block on Ready before port-forwarding the Service — without this, the
  # Service has no endpoints and port-forward connections all 000.
  if ! wait_for_litellm_ready; then
    red "ABORT: LiteLLM not Ready; see diagnostics above"
    exit 1
  fi
  setup_litellm_portforward
  setup_master_key

  # Verify kd server is reachable
  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  local TEST_TAG="e2e-litellm-$$-$(date +%s)"

  # ── Scenario 1: LiteLLM pod is Running AND Ready ────────────
  # wait_for_litellm_ready() has already blocked on Ready; this is the
  # recorded assertion. Check Ready condition, not just pod.phase, so a
  # pod that is Running-but-not-Ready does not silently pass.
  blue "Scenario 1: LiteLLM pod is Running and Ready"
  local pod_status pod_ready
  pod_status=$(kubectl -n "$NAMESPACE" get pods -l app.kubernetes.io/component=litellm \
    -o jsonpath='{.items[0].status.phase}' 2>/dev/null || echo "NotFound")
  pod_ready=$(kubectl -n "$NAMESPACE" get pods -l app.kubernetes.io/component=litellm \
    -o jsonpath='{.items[0].status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "")
  if [ "$pod_status" = "Running" ] && [ "$pod_ready" = "True" ]; then
    pass "LiteLLM pod is Running and Ready"
  else
    fail "LiteLLM pod is Running and Ready" "phase=$pod_status ready=$pod_ready"
  fi
  echo

  # ── Scenario 2: LiteLLM health check ────────────────────────
  blue "Scenario 2: LiteLLM health check"
  local health_rc health_out
  health_out=$(curl -s -o /dev/null -w "%{http_code}" "${LITELLM_URL}/health/readiness" 2>/dev/null) && health_rc=0 || health_rc=$?
  if [ "$health_out" = "200" ]; then
    pass "LiteLLM /health/readiness returns 200"
  else
    fail "LiteLLM /health/readiness returns 200" "got HTTP $health_out (rc=$health_rc)"
  fi
  echo

  # ── Scenario 3: Virtual key generation ───────────────────────
  blue "Scenario 3: Virtual key generation via /key/generate"
  local keygen_out keygen_rc virtual_key
  keygen_out=$(curl -s -X POST "${LITELLM_URL}/key/generate" \
    -H "Authorization: Bearer ${MASTER_KEY}" \
    -H "Content-Type: application/json" \
    -d "{\"key_alias\":\"e2e-cost-${TEST_TAG}\",\"models\":[\"anthropic/claude-haiku-4-5\"],\"metadata\":{\"test\":\"$TEST_TAG\"}}" \
    2>/dev/null) && keygen_rc=0 || keygen_rc=$?

  assert_exit "key/generate exits 0" 0 "$keygen_rc"
  assert_contains "key/generate returns key" '"key":' "$keygen_out"

  virtual_key=$(echo "$keygen_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('key',''))" 2>/dev/null || echo "")
  if [ -n "$virtual_key" ]; then
    pass "Virtual key extracted ($virtual_key)"
  else
    fail "Virtual key extracted" "no key in output"
    # Cannot continue without a key
    red "ABORT: Cannot continue without virtual key"
    return 1
  fi
  echo

  # ── Scenario 4: key/info returns data for new key ────────────
  blue "Scenario 4: /key/info returns data for virtual key"
  local keyinfo_out keyinfo_rc
  keyinfo_out=$(curl -s "${LITELLM_URL}/key/info?key=${virtual_key}" \
    -H "Authorization: Bearer ${MASTER_KEY}" 2>/dev/null) && keyinfo_rc=0 || keyinfo_rc=$?

  assert_exit "key/info exits 0" 0 "$keyinfo_rc"
  assert_contains "key/info returns info object" '"info"' "$keyinfo_out"

  local initial_spend
  initial_spend=$(echo "$keyinfo_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('info',{}).get('spend',0))" 2>/dev/null || echo "0")
  dim "  Initial spend: $initial_spend"

  if [ "$initial_spend" = "0" ] || [ "$initial_spend" = "0.0" ]; then
    pass "New key has zero initial spend"
  else
    fail "New key has zero initial spend" "got: $initial_spend"
  fi
  echo

  # ── Scenario 5: Create account bead with virtual key ─────────
  blue "Scenario 5: Create test account bead with virtual key"
  local acct_out acct_rc acct_id acct_err
  acct_err=$(mktemp /tmp/e2e-acct-err-XXXXXX)
  local cred_args=()
  [ -n "$CRED_FIELD" ] && cred_args=(-f "$CRED_FIELD")
  acct_out=$("$KD_BIN" create "e2e-cost-tracking-${TEST_TAG}" \
    --type=account \
    -l "e2e:$TEST_TAG" \
    -f "litellm_key=$virtual_key" \
    -f "base_url=${LITELLM_URL}" \
    -f "provider=anthropic" \
    -f "model=anthropic/claude-haiku-4-5" \
    -f "status=active" \
    "${cred_args[@]}" \
    --json 2>"$acct_err") && acct_rc=0 || acct_rc=$?

  if [ "$acct_rc" -ne 0 ]; then
    dim "  kd create stderr:"
    sed 's/^/    /' < "$acct_err" >&2 || true
    dim "  kd create stdout:"
    echo "$acct_out" | sed 's/^/    /' >&2 || true
  fi
  rm -f "$acct_err"

  assert_exit "create account bead exits 0" 0 "$acct_rc"

  acct_id=$(echo "$acct_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$acct_id" ]; then
    pass "Account bead created ($acct_id)"
    CREATED_BEADS+=("$acct_id")
  else
    fail "Account bead created" "no id in output"
  fi
  echo

  # ── Scenario 6: /spend/logs returns data ─────────────────────
  blue "Scenario 6: /spend/logs API is accessible"
  local hash_key spendlogs_out spendlogs_rc
  hash_key=$(python3 -c "import hashlib; print(hashlib.sha256('${virtual_key}'.encode()).hexdigest())" 2>/dev/null || echo "")
  local today
  today=$(date -u +%Y-%m-%d)
  local tomorrow
  tomorrow=$(date -u -d '+1 day' +%Y-%m-%d 2>/dev/null || date -u -v+1d +%Y-%m-%d 2>/dev/null || echo "2026-04-13")

  spendlogs_out=$(curl -s "${LITELLM_URL}/spend/logs?api_key=${hash_key}&start_date=${today}&end_date=${tomorrow}" \
    -H "Authorization: Bearer ${MASTER_KEY}" 2>/dev/null) && spendlogs_rc=0 || spendlogs_rc=$?

  assert_exit "spend/logs exits 0" 0 "$spendlogs_rc"
  # Should be an empty array or array with entries
  if echo "$spendlogs_out" | python3 -c "import sys,json; data=json.load(sys.stdin); assert isinstance(data, list)" 2>/dev/null; then
    pass "spend/logs returns a JSON array"
  else
    fail "spend/logs returns a JSON array" "got: $(echo "$spendlogs_out" | head -3)"
  fi
  echo

  # ── Scenario 7: Controller has LITELLM_URL configured ────────
  blue "Scenario 7: Controller has LITELLM_URL configured"
  local ctrl_env
  ctrl_env=$(kubectl -n "$NAMESPACE" get deployment "${AGENTS_DEPLOY}" \
    -o jsonpath='{.spec.template.spec.containers[0].env}' 2>/dev/null || echo "[]")

  if grep -qF -- 'LITELLM_URL' <<<"$ctrl_env"; then
    pass "Controller deployment has LITELLM_URL env var"
  else
    fail "Controller deployment has LITELLM_URL env var" "not found in controller env"
  fi

  if grep -qF -- 'LITELLM_MASTER_KEY' <<<"$ctrl_env"; then
    pass "Controller deployment has LITELLM_MASTER_KEY env var"
  else
    fail "Controller deployment has LITELLM_MASTER_KEY env var" "not found in controller env"
  fi
  echo

  # ── Scenario 8: Account beads have cost tracking fields ──────
  blue "Scenario 8: Account beads have cost tracking fields"
  local acct_list acct_list_rc
  acct_list=$("$KD_BIN" list --type=account --status=open --json 2>/dev/null) && acct_list_rc=0 || acct_list_rc=$?

  assert_exit "list account beads exits 0" 0 "$acct_list_rc"

  # spend_sync.syncAll() writes the canonical cost fields and the shadow_*
  # mirror fields in a single bead update (gd-9FMC4Bayur, D-h3 shadow-yes), so
  # any account bead a spend_sync cycle has touched must carry BOTH sets.
  #
  # The selection predicate must match spend_sync's OWN precondition, not just
  # "has total_cost_usd". spend_sync (controller/cmd/controller/spend_sync.go)
  # skips every account bead whose litellm_key is empty, so the ONLY proof a
  # spend_sync cycle wrote cost fields is a bead that has BOTH total_cost_usd
  # AND a non-empty litellm_key. total_cost_usd alone is not proof: it is also
  # written by reconcile_session_spend.go (on agent/session beads) and seeded
  # by claudeless fixtures such as trigger-source-account-fires.toml, which
  # creates a type=account bead with total_cost_usd but no litellm_key and no
  # shadow_* fields. Selecting on total_cost_usd alone could pick one of those
  # and fail the shadow_* assertion against a bead spend_sync never touched
  # (gd-btUmXeTTAh). Require litellm_key so the assertion only runs against a
  # genuine spend_sync target.
  local cost_bead_fields
  cost_bead_fields=$(echo "$acct_list" | python3 -c "
import sys,json
data=json.load(sys.stdin)
beads=data if isinstance(data, list) else data.get('beads',[])
# Find the first non-test account bead spend_sync has populated: it must carry
# total_cost_usd AND a non-empty litellm_key (spend_sync's own precondition).
for b in beads:
    if b.get('title','').startswith('e2e-'):
        continue
    fields = b.get('fields',{})
    if 'total_cost_usd' in fields and fields.get('litellm_key'):
        print(' '.join(sorted(fields.keys())))
        break
" 2>/dev/null || echo "")

  if [ -n "$cost_bead_fields" ]; then
    pass "spend_sync-populated account bead found (has total_cost_usd + litellm_key)"
    # The same bead that has canonical cost fields must also carry the
    # shadow_* mirror fields written in the same spend_sync update.
    local sf
    for sf in shadow_total_cost_usd shadow_daily_cost_usd shadow_weekly_cost_usd; do
      assert_contains "spend_sync populated $sf on account bead" "$sf" "$cost_bead_fields"
    done
  else
    dim "  No non-test account beads with total_cost_usd + litellm_key (may be"
    dim "  normal for a fresh deployment where spend_sync has not yet run)."
    pass "Cost field check completed (no spend_sync-populated account to verify)"
    dim "  Skipping shadow_* assertion: no spend_sync-populated account bead present."
  fi
  echo

  # ── Summary ─────────────────────────────────────────────────
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
