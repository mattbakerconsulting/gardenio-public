#!/usr/bin/env bash
# test-session-spend-attribution.sh — E2E: per-session spend attribution, driven
# end-to-end through the REAL controller + bridge code over REAL HTTP/NATS
# surfaces (task gd-NU1IbgLk5v).
#
# ── The gap this closes ───────────────────────────────────────────────────────
#   Session cost/spend attribution shipped end-to-end (F2/E3 counter, close-time
#   /spend/tags reconciliation, coop cost-free usage NATS event) with strong UNIT
#   coverage — but no test wired the pieces together the way the controller runs
#   them. The four existing spend/usage e2e modules each cover a different, NON-
#   overlapping surface and NONE of them exercise this live path:
#     - fleet-metrics.sh      — the kbeads /metrics fleet aggregate, NOT the
#                               controller :9092 gardenio_agent_spend_usd_total.
#     - litellm-cost-tracking — the LiteLLM proxy /key/info + /spend/logs and the
#                               spend_sync account-bead shadow_* fields.
#     - agent-cost-context    — the beads-viewer cost/context CHIPS; it WRITES
#                               total_cost_usd BY HAND via kd and never runs the
#                               close reconciliation.
#     - usage-tracking        — asserts `gb session analyze` TEXT, never the coop
#                               usage NATS payload.
#
# ── Why a go-test module, not a live-cluster full loop ────────────────────────
#   A claudeless mock agent makes ZERO real LiteLLM spend, so a live spawn→close
#   never advances the /spend/tags total and the counter increment can't be
#   proven against a real pod. The three halves also span THREE packages
#   (cmd/controller reconciler + internal/metrics counter, internal/bridge usage
#   consumer) that no single live surface exercises together. So — exactly like
#   test-pvc-gc-ttl.sh — this module wraps targeted `go test -run` against the
#   REAL production code, exercised as an integration flow over real HTTP
#   (promhttp /metrics scrape, beadsapi daemon round-trip) and a real embedded
#   NATS server. The heavy per-scenario arithmetic stays in the Go tests; this
#   wrapper makes the behaviour visible in the E2E gate alongside its siblings.
#
# ── What the wrapped Go tests prove (the CURRENT topology) ────────────────────
#   IMPORTANT: the task bead's "Proposed e2e" describes a NATS->counter forwarder
#   (spend_forwarder.go) that was DELETED in 9add5d50c (ka-0000000717) — coop
#   never emits response_cost, so it dropped 100% of events. The counter is now
#   driven SOLELY by reconcileAtClose at session close. These tests assert the
#   live path, not the removed one:
#
#   Leg A/B  (cmd/controller, session_spend_e2e_test.go) — spawn→close→scrape:
#     captureBaseline stashes the /spend/tags baseline ONTO the agent bead
#     (born-at-0 counter series), reconcileAtClose reads that daemon-PERSISTED
#     baseline BACK and settles the delta, writing total_cost_usd /
#     litellm_spend_close / litellm_spend_first_baseline onto the bead (the
#     CONTROLLER writes it from the live /spend/tags delta — NOT a hand-set kd
#     field). gardenio_agent_spend_usd_total{agent,project,role,model,account},
#     scraped over a REAL promhttp /metrics endpoint, advances by exactly the
#     settled delta; a zero-cost close does NOT advance it; a respawner
#     ACCUMULATES across sessions.
#   Leg C  (internal/bridge, hookstream_usage_contract_test.go) — usage NATS
#     model contract: coop's EXACT usage payload (coop.events.messages.<actor>.
#     usage) published through a REAL embedded NATS server + the REAL HookStream
#     subscription decodes with `model` present, renders the usage chunk, and
#     carries NO cost (a stray response_cost never surfaces; the consumer struct
#     has no cost field) — the strictly-cost-free contract (ka-0000002983).
#   Leg D  (cmd/controller, session_spend_proxy_e2e_test.go) — claudemore proxy
#     transparency (ka-0000005743 / A1, ka-0000007533): the SAME spawn→close flow
#     run TWICE, once with NO proxy_url and once with a claudemore proxy_url on the
#     SAME account, asserting the settled fields (total_cost_usd /
#     litellm_spend_close / litellm_spend_first_baseline / baseline) and the served
#     counter are BYTE-IDENTICAL, and the claudemore trap is contacted ZERO times.
#     The base URL each leg reads is resolved by the REAL production function
#     resolveSpendReadBaseURLForAccount, so a "unify the read onto proxy_url"
#     regression routes the proxied leg's read at the claudemore trap and reddens
#     it. This is the ONLY spend module that attaches a proxy to an account: the
#     other three assert spend but never proxy, so the regression is invisible to
#     them (born-vacuous, advice ka-0000001521).
#
# ── Hermetic HARD gate ────────────────────────────────────────────────────────
#   No pods, no live cluster, no shared-daemon writes — everything runs in-process
#   against httptest servers + an embedded NATS server. So it is green today and
#   is a HARD gate: any leg failure reddens the shard. It SKIPs clean (exit 0)
#   ONLY when the Go toolchain / controller source is genuinely absent — a missing
#   toolchain must not be read as a pass, so the skip is LOUD.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-session-spend-attribution.sh

set -uo pipefail

red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
CONTROLLER_DIR="${REPO_ROOT}/controller"
RUN_ID="${RUN_ID:-$$}"

PASS=0
FAIL=0
ERRORS=()

pass() { green "  ✓ $1"; PASS=$((PASS + 1)); }
fail() {
  red "  ✗ $1"
  [ -n "${2:-}" ] && dim "    $2"
  FAIL=$((FAIL + 1))
  ERRORS+=("$1: ${2:-}")
}

# LOUD skip — exit 0 (a missing toolchain must NOT red the suite) with a banner
# so a skip is never silently read as a pass.
skip_all() {
  local why="$1"
  red "════════════════════════════════════════════════════"
  red " SKIP session-spend-attribution (LOUD): $why"
  red " This run did NOT assert the session spend attribution pipeline"
  red " (gardenio_agent_spend_usd_total counter, /spend/tags close reconciliation,"
  red " coop cost-free usage NATS model). A green suite here proves NOTHING about"
  red " that pipeline. The e2e shard is built with a Go toolchain + the controller"
  red " source — if this fires in CI, that wiring regressed."
  red "════════════════════════════════════════════════════"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

echo "════════════════════════════════════════════════════"
echo " session spend attribution E2E (gd-NU1IbgLk5v, ka-0000005743)"
echo " spend counter · /spend/tags close reconciliation · usage NATS model"
echo " · claudemore proxy transparency (spend read bypasses proxy_url)"
echo "════════════════════════════════════════════════════"

# ── Preflight: Go toolchain + controller source present ─────────────────────
command -v go >/dev/null 2>&1 || skip_all "no go toolchain to run the spend integration tests"
[ -d "${CONTROLLER_DIR}/cmd/controller" ] \
  || skip_all "controller source missing under ${CONTROLLER_DIR}"

# A leg = one targeted `go test -run` against a real production surface. Each is
# a HARD sub-gate; any failure reddens the shard.
run_leg() {
  local name="$1" pkg="$2" run="$3"
  local log="/tmp/session-spend-${name}-${RUN_ID}.log"
  blue "[leg ${name}] go test ${pkg} -run '${run}'"
  if (cd "${CONTROLLER_DIR}" && go test -v "${pkg}" -run "${run}" -count=1 -timeout 180s) \
      >"${log}" 2>&1; then
    local n
    n=$(grep -cE '^--- PASS' "${log}" 2>/dev/null | tr -d '[:space:]')
    pass "leg ${name} — ${n:-?} test(s) green"
  else
    fail "leg ${name}" "go test failed:
$(tail -25 "${log}" | sed 's/^/      /')"
  fi
}

# Leg A/B: spawn→close→served-/metrics scrape + close reconciliation round-trip.
run_leg "counter-reconcile" ./cmd/controller/ 'TestSessionSpendE2E'

# Leg C: coop usage NATS model + cost-free contract through the real consumer.
run_leg "usage-model" ./internal/bridge/ 'TestUsageContract'

# Leg D: claudemore proxy transparency (ka-0000005743 / A1, ka-0000007533). Drives
# the SAME spawn→close→served-counter flow TWICE — once with NO proxy_url, once
# with a claudemore proxy_url attached to the SAME account — and asserts the
# settled fields + counter are BYTE-IDENTICAL and the claudemore trap is contacted
# ZERO times, because the spend read resolves litellm DIRECTLY via the production
# resolveSpendReadBaseURLForAccount (base_url), never proxy_url. This is the ONLY
# leg that attaches a proxy to an account: the three pre-existing spend modules all
# assert spend but never proxy an account, so a "route spend through claudemore"
# regression leaves them green while proxied metering silently zeros out.
run_leg "proxy-transparency" ./cmd/controller/ 'TestSessionSpendProxyTransparency|TestSpendReadBaseURLForAccount_IgnoresProxyURL'

echo
blue "════════════════════════════════════════════════════"
total=$((PASS + FAIL))
if [ "$FAIL" -eq 0 ]; then
  green " ✓ All $total legs passed"
else
  red " ✗ $FAIL/$total legs failed"
  echo
  for err in "${ERRORS[@]}"; do red "   - $err"; done
fi
blue "════════════════════════════════════════════════════"

[ "$FAIL" -eq 0 ]
