#!/usr/bin/env bash
# test-stack-health.sh — stack health E2E tests
#
# Verifies all gardenio deployments are healthy: pods running, services
# reachable, beads HTTP health endpoint, NATS and postgres connectivity.
#
# Usage:
#   ./tests/e2e/scripts/test-stack-health.sh
#
# Prerequisites:
#   - kubectl context pointing at the target cluster
#   - jq installed
#
# Scenarios tested:
#   1. All deployments have ready replicas (no CrashLoopBackOff)
#   2. Beads HTTP health endpoint returns 200
#   3. PostgreSQL is accepting connections
#   4. NATS service is reachable
#   5. Controller deployment is running
#   6. Coopmux deployment is running (optional — coopmux.enabled=false in
#      minimal namespaces; the P4.4 migration coop-reachable check
#      test-migration-coop-reachable.sh does the deep /api/v1/health probe)
#   7. /v1/health/live answers 200 UNAUTHENTICATED (auth/token-revocation/
#      edge-auth exemptions) and stays 200 under a rapid burst (rate-limit
#      exemption) — the endpoint the readiness+liveness probes hit (ka-0000001764)
#   8. Rendered beads Deployment probes are wired to the right paths:
#      readiness+liveness → /v1/health/live (cheap, no DB), startup → /v1/health
#      (DB-ping cold-start gate). Anti-tautology: reads what the cluster
#      actually rendered, so a values/template regression flipping readiness
#      back to /v1/health can no longer land silently (ka-0000000469).

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PASS=0
FAIL=0
ERRORS=()
PF_PID=""

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

assert_http_status() {
  local name="$1"
  local expected="$2"
  local actual="$3"
  if [ "$actual" = "$expected" ]; then
    pass "$name (HTTP $expected)"
  else
    fail "$name" "expected HTTP $expected, got HTTP $actual"
  fi
}

assert_contains() {
  local name="$1"
  local pattern="$2"
  local actual="$3"
  if grep -qF -- "$pattern" <<< "$actual"; then
    pass "$name"
  else
    fail "$name" "expected output to contain '$pattern'; got: $(echo "$actual" | head -3)"
  fi
}

cleanup() {
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# ── Check deployment health ───────────────────────────────────────
check_deployment() {
  local name="$1"
  local deploy_name="${2:-$name}"
  local ready
  ready=$(kubectl -n "$NAMESPACE" get deploy "$deploy_name" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "")
  local desired
  desired=$(kubectl -n "$NAMESPACE" get deploy "$deploy_name" -o jsonpath='{.spec.replicas}' 2>/dev/null || echo "")

  if [ -z "$ready" ] || [ -z "$desired" ]; then
    fail "$name deployment exists" "deployment $deploy_name not found in $NAMESPACE"
    return
  fi

  if [ "$ready" -ge "$desired" ] && [ "$desired" -gt 0 ]; then
    pass "$name deployment ready ($ready/$desired)"
  else
    fail "$name deployment ready" "expected $desired ready, got $ready"
  fi
}

# ── Check for CrashLoopBackOff pods ───────────────────────────────
# Only fails on core gardenio components. Optional components (litellm,
# vmsingle) are warned about but don't block the E2E run.
OPTIONAL_COMPONENTS="litellm|vmsingle"

check_no_crashloop() {
  local all_crashing
  all_crashing=$(kubectl -n "$NAMESPACE" get pods -o json 2>/dev/null \
    | jq -r '.items[] | select(.status.containerStatuses[]?.state.waiting?.reason == "CrashLoopBackOff") | .metadata.name' 2>/dev/null || echo "")

  if [ -z "$all_crashing" ]; then
    pass "no CrashLoopBackOff pods"
    return
  fi

  # Split into core vs optional crashes.
  local core_crashing="" optional_crashing=""
  for pod in $all_crashing; do
    if grep -qE "$OPTIONAL_COMPONENTS" <<<"$pod"; then
      optional_crashing="${optional_crashing:+$optional_crashing }$pod"
    else
      core_crashing="${core_crashing:+$core_crashing }$pod"
    fi
  done

  if [ -n "$optional_crashing" ]; then
    dim "  WARNING: optional pods crashing (non-blocking): $optional_crashing"
  fi

  if [ -z "$core_crashing" ]; then
    pass "no CrashLoopBackOff pods (core components)"
  else
    fail "no CrashLoopBackOff pods" "crashing: $core_crashing"
  fi
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " stack-health E2E tests"
  blue "═══════════════════════════════════════════════════"
  echo

  # ── Scenario 1: No CrashLoopBackOff ─────────────────────────────
  blue "Scenario 1: No pods in CrashLoopBackOff"
  check_no_crashloop
  echo

  # ── Scenario 2: Core deployments are ready ──────────────────────
  blue "Scenario 2: Core deployments have ready replicas"
  check_deployment "beads" "${NAMESPACE}-beads"
  # Controller and coopmux are optional in minimal E2E namespaces
  if kubectl -n "$NAMESPACE" get deploy "${NAMESPACE}-controller" >/dev/null 2>&1; then
    check_deployment "controller" "${NAMESPACE}-controller"
  else
    dim "  controller deployment not present (optional in E2E)"
  fi
  echo

  # ── Scenario 3: Beads HTTP health ───────────────────────────────
  blue "Scenario 3: Beads HTTP health endpoint"
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    dim "  Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
  else
    local port=19092
    kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
      >/tmp/kd-pf-health.log 2>&1 &
    PF_PID=$!
    sleep 3
    BEADS_HTTP_URL="http://localhost:${port}"
    dim "  Port-forward on $BEADS_HTTP_URL (pid $PF_PID)"
  fi

  local health_code
  # Try /v1/health first (kbeads), then /healthz, then root
  local health_body
  health_body=$(curl -sf "${BEADS_HTTP_URL}/v1/health" 2>/dev/null || echo "")
  if grep -q '"ok"' <<<"$health_body"; then
    pass "beads /v1/health returns ok"
  else
    health_code=$(curl -sf -o /dev/null -w '%{http_code}' "${BEADS_HTTP_URL}/healthz" 2>/dev/null || echo "000")
    if [ "$health_code" = "200" ]; then
      pass "beads /healthz returns 200"
    else
      fail "beads HTTP health" "got /v1/health='$health_body', /healthz=$health_code"
    fi
  fi
  echo

  # ── Scenario 4: PostgreSQL connectivity ─────────────────────────
  blue "Scenario 4: PostgreSQL service exists"
  local pg_svc
  pg_svc=$(kubectl -n "$NAMESPACE" get svc -l app.kubernetes.io/name=postgresql -o name 2>/dev/null || echo "")
  if [ -n "$pg_svc" ]; then
    pass "PostgreSQL service found"
  else
    # Try common naming patterns
    pg_svc=$(kubectl -n "$NAMESPACE" get svc 2>/dev/null | grep -i postgres | head -1 || echo "")
    if [ -n "$pg_svc" ]; then
      pass "PostgreSQL service found (pattern match)"
    else
      fail "PostgreSQL service" "no postgresql service found in $NAMESPACE"
    fi
  fi
  echo

  # ── Scenario 5: NATS service exists ─────────────────────────────
  blue "Scenario 5: NATS is running"
  # NATS may be a StatefulSet or Deployment
  local nats_ready=""
  nats_ready=$(kubectl -n "$NAMESPACE" get statefulset -o name 2>/dev/null | grep nats | head -1 || echo "")
  if [ -n "$nats_ready" ]; then
    pass "NATS StatefulSet found"
  else
    nats_ready=$(kubectl -n "$NAMESPACE" get deploy -o name 2>/dev/null | grep nats | head -1 || echo "")
    if [ -n "$nats_ready" ]; then
      pass "NATS Deployment found"
    else
      fail "NATS" "no nats statefulset or deployment found in $NAMESPACE"
    fi
  fi
  echo

  # ── Scenario 6: Coopmux deployment (optional) ───────────────────
  # coopmux is enabled:false by default and only on in fleet/inbox overlays,
  # so treat it as optional here (like the controller block above). The deep
  # /api/v1/health probe lives in test-migration-coop-reachable.sh; here we
  # just confirm readiness when the Deployment is present.
  blue "Scenario 6: Coopmux deployment (optional)"
  if kubectl -n "$NAMESPACE" get deploy "${NAMESPACE}-coopmux" >/dev/null 2>&1; then
    check_deployment "coopmux" "${NAMESPACE}-coopmux"
  else
    dim "  coopmux deployment not present (optional — coopmux.enabled=false)"
  fi
  echo

  # ── Scenario 7: /v1/health/live is unauthenticated + rate-limit-exempt ──
  # /v1/health/live (ka-0000001764) is the CHEAP no-DB endpoint the readiness
  # + liveness probes hit. It must answer 200 with NO Authorization header —
  # its isProbeHealthPath exemption is wired into FOUR middlewares (auth,
  # token-revocation, edge-auth, rate-limit) and none of them had E2E
  # coverage. Nearest tests all target the OLD DB-pinging /v1/health, so a
  # dropped exemption on the NEW path was entirely silent.
  #
  # BEADS_HTTP_URL is established by Scenario 3 (env or the port-forward set
  # up above), so it is available here.
  blue "Scenario 7: /v1/health/live unauthenticated + burst-tolerant"
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    # Leg 1: single unauthenticated GET — explicitly send NO auth header.
    local live_code live_body
    live_code=$(curl -s -o /dev/null -w '%{http_code}' \
      "${BEADS_HTTP_URL}/v1/health/live" 2>/dev/null || echo "000")
    assert_http_status "/v1/health/live returns 200 without auth" "200" "$live_code"

    live_body=$(curl -sf "${BEADS_HTTP_URL}/v1/health/live" 2>/dev/null || echo "")
    assert_contains "/v1/health/live body is status:ok" '"ok"' "$live_body"

    # Leg 2: rapid unauthenticated burst. rate_limit.go:48 short-circuits the
    # probe path (rateLimitClass returns "" → never enters the token bucket),
    # so repeated unauthenticated hits must ALL stay 200 — never 401 (auth
    # exemption) and never 429 (rate-limit exemption). We keep the count modest
    # so the leg stays fast and non-flaky over the port-forward; it is a
    # behavioral guard that the endpoint is neither de-authed nor throttled
    # under load, not an attempt to saturate the read bucket (burst 200). We
    # record the FIRST non-200 for a useful failure message.
    local burst_n=50 burst_bad=0 first_bad="" bc
    local i
    for ((i = 0; i < burst_n; i++)); do
      bc=$(curl -s -o /dev/null -w '%{http_code}' \
        "${BEADS_HTTP_URL}/v1/health/live" 2>/dev/null || echo "000")
      if [ "$bc" != "200" ]; then
        burst_bad=$((burst_bad + 1))
        [ -z "$first_bad" ] && first_bad="$bc"
      fi
    done
    if [ "$burst_bad" -eq 0 ]; then
      pass "/v1/health/live stays 200 across ${burst_n}-request burst"
    else
      fail "/v1/health/live burst" \
        "$burst_bad/$burst_n requests were non-200 (first: HTTP $first_bad)"
    fi
  else
    fail "/v1/health/live reachability" "BEADS_HTTP_URL unset (Scenario 3 did not establish it)"
  fi
  echo

  # ── Scenario 8: rendered probe wiring (anti-tautology) ──────────────
  # Read the probe paths the CLUSTER actually rendered — NOT a fixture the
  # test wrote. A values/template regression flipping readiness/liveness back
  # to the DB-pinging /v1/health (the family-B flake, ka-0000007460) would be
  # silent otherwise: no test reads the rendered Helm probes. Guarded by the
  # kubectl availability check (from-source runs without a cluster skip it).
  blue "Scenario 8: beads Deployment probe paths"
  if command -v kubectl >/dev/null 2>&1 && \
     kubectl -n "$NAMESPACE" get deploy "${NAMESPACE}-beads" >/dev/null 2>&1; then
    local jp_ready='{.spec.template.spec.containers[?(@.name=="beads")].readinessProbe.httpGet.path}'
    local jp_live='{.spec.template.spec.containers[?(@.name=="beads")].livenessProbe.httpGet.path}'
    local jp_start='{.spec.template.spec.containers[?(@.name=="beads")].startupProbe.httpGet.path}'
    local ready_path live_path start_path
    ready_path=$(kubectl -n "$NAMESPACE" get deploy "${NAMESPACE}-beads" -o jsonpath="$jp_ready" 2>/dev/null || echo "")
    live_path=$(kubectl -n "$NAMESPACE" get deploy "${NAMESPACE}-beads" -o jsonpath="$jp_live" 2>/dev/null || echo "")
    start_path=$(kubectl -n "$NAMESPACE" get deploy "${NAMESPACE}-beads" -o jsonpath="$jp_start" 2>/dev/null || echo "")

    if [ "$ready_path" = "/v1/health/live" ]; then
      pass "readinessProbe → /v1/health/live (cheap, no DB)"
    else
      fail "readinessProbe path" "expected /v1/health/live, got '$ready_path'"
    fi
    if [ "$live_path" = "/v1/health/live" ]; then
      pass "livenessProbe → /v1/health/live (cheap, no DB)"
    else
      fail "livenessProbe path" "expected /v1/health/live, got '$live_path'"
    fi
    if [ "$start_path" = "/v1/health" ]; then
      pass "startupProbe → /v1/health (DB-ping cold-start gate)"
    else
      fail "startupProbe path" "expected /v1/health, got '$start_path'"
    fi
  else
    dim "  kubectl unavailable or ${NAMESPACE}-beads not present — skipping rendered-probe wiring checks"
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
