#!/usr/bin/env bash
# test-migration-coop-reachable.sh — P4.4 migration acceptance smoke:
# the coop/coopmux terminal path is reachable and healthy behind the edge on
# the new prod cluster.
#
# Bead: ka-0000004091  (epic ka-0000003481 → Phase-4 migration ka-0000002205)
#
# WHY this exists: the P4.4 cutover moves coop/coopmux off america-e2e-eks onto
# the fresh prod cluster. coopmux is the terminal-mux + credential broker every
# agent session attaches to; if it does not come up healthy after the cut, the
# whole fleet is dark. No prior smoke check probed it — test-stack-health.sh
# even declared "Scenario 6: Coopmux deployment is running" in its header but
# never implemented it (fixed in the same MR).
#
# What it asserts:
#   1. The coopmux Deployment (<release>-coopmux) has its ready replicas.
#   2. The coopmux HTTP health endpoint GET /api/v1/health returns 200 and
#      {"status":"running"} — reached by port-forwarding svc/<release>-coopmux
#      :9800 (the single port the Rust mux binds; dashboard + WS + API + health
#      all share it). /api/v1/health is auth-exempt (coop/crates/mux
#      transport/auth.rs), so no bearer is needed — it works over both the raw
#      Service and behind the edge shim.
#
# "Behind the edge": coopmux is fronted two ways — the AVA unified host
# (gardenio.<env>/mux, browser SSO) and the non-AVA CLI edge (cli.gardenio.<env>
# via the gardenio-edge-auth shim). For a cluster-reachability smoke we probe the
# Service directly (same altitude as the beads /v1/health check); an optional
# edge-host probe is enabled by exporting COOPMUX_EDGE_URL.
#
# Self-SKIPs LOUDLY (never a false red) when the coopmux Deployment/Service is
# absent — coopmux is enabled:false by default and only on in fleet/inbox
# overlays, so a minimal ephemeral e2e namespace legitimately has none.
#
# Runnable against prod.gardenio.acme.example by pointing kubectl at the prod cluster
# and exporting NAMESPACE=<prod ns>. READ-ONLY — no writes.
#
# Usage:
#   NAMESPACE=gardenio-e2e ./gardenio/tests/e2e/scripts/test-migration-coop-reachable.sh

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
RELEASE="${RELEASE:-${NAMESPACE}}"
COOPMUX_SVC="${COOPMUX_SVC:-${RELEASE}-coopmux}"
COOPMUX_DEPLOY="${COOPMUX_DEPLOY:-${RELEASE}-coopmux}"
COOPMUX_PF_PORT="${COOPMUX_PF_PORT:-19821}"
# Optional: an AVA/CLI edge URL to also probe the auth-exempt health through the
# edge (e.g. https://gardenio.<env>/mux or https://cli.gardenio.<env>/mux). Empty
# = cluster-reachability only.
COOPMUX_EDGE_URL="${COOPMUX_EDGE_URL:-}"
PASS=0
FAIL=0
SKIP=0
ERRORS=()
PF_PID=""

# ── Helpers ───────────────────────────────────────────────────────
red()    { printf '\033[0;31m%s\033[0m\n' "$*"; }
green()  { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()   { printf '\033[0;34m%s\033[0m\n' "$*"; }
yellow() { printf '\033[0;33m%s\033[0m\n' "$*"; }
dim()    { printf '\033[2m%s\033[0m\n' "$*"; }

pass() { green "  ✓ $1"; PASS=$((PASS + 1)); }
fail() {
  local name="$1" reason="${2:-}"
  red "  ✗ $name"
  [ -n "$reason" ] && dim "    $reason"
  FAIL=$((FAIL + 1))
  ERRORS+=("$name: $reason")
}
skip() {
  local name="$1" reason="${2:-}"
  yellow "  ⊘ SKIP: $name${reason:+ — $reason}"
  SKIP=$((SKIP + 1))
}

cleanup() {
  [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null || true
}
trap cleanup EXIT

# Resolve the numeric Service port (rename-safe): fall back to 9800.
coopmux_service_port() {
  local p
  p=$(kubectl -n "$NAMESPACE" get svc "$COOPMUX_SVC" \
    -o jsonpath='{.spec.ports[0].port}' 2>/dev/null || true)
  printf '%s' "${p:-9800}"
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " migration-coop-reachable smoke (ka-0000004091)"
  blue " namespace=$NAMESPACE coopmux=$COOPMUX_SVC"
  blue "═══════════════════════════════════════════════════"
  echo

  # ── Precondition: coopmux present (else SKIP the module) ────────
  if ! kubectl -n "$NAMESPACE" get deploy "$COOPMUX_DEPLOY" >/dev/null 2>&1 \
     && ! kubectl -n "$NAMESPACE" get svc "$COOPMUX_SVC" >/dev/null 2>&1; then
    skip "coop reachable smoke" "coopmux Deployment/Service absent in $NAMESPACE (coopmux.enabled=false — only on fleet/inbox overlays / prod)"
    summary
    return 0
  fi

  # ── Scenario 1: Deployment ready ────────────────────────────────
  blue "Scenario 1: coopmux Deployment has ready replicas"
  if kubectl -n "$NAMESPACE" get deploy "$COOPMUX_DEPLOY" >/dev/null 2>&1; then
    local ready desired
    ready=$(kubectl -n "$NAMESPACE" get deploy "$COOPMUX_DEPLOY" \
      -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "")
    desired=$(kubectl -n "$NAMESPACE" get deploy "$COOPMUX_DEPLOY" \
      -o jsonpath='{.spec.replicas}' 2>/dev/null || echo "")
    if [ -n "$ready" ] && [ -n "$desired" ] && [ "$ready" -ge "$desired" ] && [ "$desired" -gt 0 ]; then
      pass "coopmux deployment ready ($ready/$desired)"
    else
      fail "coopmux deployment ready" "expected $desired ready, got ${ready:-0}"
    fi
  else
    dim "  coopmux Deployment not found (Service exists) — skipping readiness assertion"
  fi
  echo

  # ── Scenario 2: HTTP health endpoint (cluster reachability) ─────
  blue "Scenario 2: coopmux /api/v1/health returns 200 + status=running"
  local health_url
  if [ -n "${COOPMUX_HTTP_URL:-}" ]; then
    health_url="${COOPMUX_HTTP_URL%/}/api/v1/health"
    dim "  using COOPMUX_HTTP_URL=$COOPMUX_HTTP_URL"
  elif kubectl -n "$NAMESPACE" get svc "$COOPMUX_SVC" >/dev/null 2>&1; then
    local svc_port
    svc_port=$(coopmux_service_port)
    blue "  Port-forwarding ${NAMESPACE}/${COOPMUX_SVC} :${COOPMUX_PF_PORT} → ${svc_port}..."
    kubectl -n "$NAMESPACE" port-forward "svc/${COOPMUX_SVC}" \
      "${COOPMUX_PF_PORT}:${svc_port}" >/tmp/migration-coop-pf.log 2>&1 &
    PF_PID=$!
    health_url="http://localhost:${COOPMUX_PF_PORT}/api/v1/health"
    # Poll for tunnel readiness instead of a fixed sleep — the forward can take
    # >3s to bind under a cold cluster (avoid a fail-when-healthy flake). The
    # health path is auth-exempt so a 200 here needs no bearer.
    local _i
    for _i in 1 2 3 4 5 6 7 8 9 10 11 12; do
      curl -sf --max-time 1 -o /dev/null "$health_url" 2>/dev/null && break
      sleep 0.5
    done
  else
    skip "coopmux HTTP health" "coopmux Service $COOPMUX_SVC absent (deployment-only overlay)"
    health_url=""
  fi

  if [ -n "$health_url" ]; then
    local code body
    body=$(curl -sf --max-time 10 "$health_url" 2>/dev/null || echo "")
    code=$(curl -s --max-time 10 -o /dev/null -w '%{http_code}' "$health_url" 2>/dev/null || echo "000")
    # coopmux returns {"status":"running",...} (NOT "ok" like beads). Require the
    # body marker for a pass: a bare 200 could be a non-coopmux page (e.g. an SSO
    # login shell fronting the same port), which would false-green "is coopmux
    # actually healthy" (review finding, ka-0000003481).
    if grep -q '"status":[[:space:]]*"running"' <<<"$body"; then
      pass "coopmux /api/v1/health returns status=running (HTTP $code)"
    else
      fail "coopmux HTTP health" "GET /api/v1/health → HTTP $code but body lacks {\"status\":\"running\"} (got: '$(printf '%s' "$body" | head -c 120)') — not a healthy coopmux"
    fi
  fi
  echo

  # ── Scenario 3 (optional): reachable behind the edge ────────────
  if [ -n "$COOPMUX_EDGE_URL" ]; then
    blue "Scenario 3: coopmux health reachable behind the edge"
    local edge_health edge_code edge_body
    edge_health="${COOPMUX_EDGE_URL%/}/api/v1/health"
    # /api/v1/health is auth-exempt even behind the AVA/shim edge, so a bare GET
    # should reach coopmux and 200 without SSO. No -L: a redirect surfaces as 3xx.
    # Assert the coopmux BODY, not just a 200 — an edge that 200s a non-coopmux
    # page (login/SSO shell) must not pass.
    edge_body=$(curl -sk --max-time 15 "$edge_health" 2>/dev/null || echo "")
    edge_code=$(curl -sk --max-time 15 -o /dev/null -w '%{http_code}' "$edge_health" 2>/dev/null || echo "000")
    case "$edge_code" in
      200)
        if grep -q '"status":[[:space:]]*"running"' <<<"$edge_body"; then
          pass "edge $edge_health returns coopmux status=running (reachable through the edge)"
        else
          fail "edge coopmux health" "got 200 but body lacks {\"status\":\"running\"} — the edge served a non-coopmux page, coopmux not proven reachable (body: '$(printf '%s' "$edge_body" | head -c 120)')"
        fi
        ;;
      303|302|307) fail "edge coopmux health" "got $edge_code — edge redirected an auth-exempt health probe (pre-auth redirect), coopmux not reached" ;;
      *) fail "edge coopmux health" "GET $edge_health → HTTP $edge_code (want 200)" ;;
    esac
    echo
  fi

  summary
}

summary() {
  blue "═══════════════════════════════════════════════════"
  local total=$((PASS + FAIL))
  if [ "$FAIL" -eq 0 ]; then
    green " ✓ migration-coop-reachable: $PASS passed, $SKIP skipped"
  else
    red " ✗ migration-coop-reachable: $FAIL/$total failed, $SKIP skipped"
    for e in "${ERRORS[@]}"; do red "   - $e"; done
  fi
  blue "═══════════════════════════════════════════════════"
  [ "$FAIL" -eq 0 ]
}

main "$@"
