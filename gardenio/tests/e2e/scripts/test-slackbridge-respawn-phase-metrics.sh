#!/usr/bin/env bash
# test-slackbridge-respawn-phase-metrics.sh — E2E: slack-bridge cold-respawn
# PHASE-BREAKDOWN metrics, driven end-to-end through the REAL recordRespawnPhase
# over a REAL promhttp /metrics endpoint (backfill bead ka-0000003422, epic
# ka-0000000958; feature merge 8edcbe2e1 / task ka-0000001457).
#
# ── The gap this closes ───────────────────────────────────────────────────────
#   The slack-bridge per-phase respawn-latency instrumentation shipped
#   (respawn_phase_metrics.go: histogram gardenio_slackbridge_respawn_phase_seconds
#   {phase,role} + counter gardenio_slackbridge_respawn_phase_slow_total{phase},
#   alerted on by the SlackBridgeRespawnPhaseSlow vmalert rule) with ZERO
#   end-to-end coverage: the Go verdict logic had unit coverage
#   (respawn_phase_metrics_test.go, testutil over the in-process registry) and the
#   helm rule had a render test, but NOTHING proved (a) the series reach the
#   Prometheus TEXT EXPOSITION the bridge serves on :9093, or (b) that the emitted
#   series names still match the metric names the SHIPPED vmalert rule keys its
#   expr off. A rename on EITHER side silently un-fires the page while the unit
#   test AND the helm render test both stay green — the classic unfireable alert.
#   `grep -rn respawn_phase gardenio/tests/` was 0 hits at HEAD 509231f5a.
#
# ── Why a go-test module, NOT a live-bridge port-forward ──────────────────────
#   The task's original feasibility note proposed scraping BRIDGE_HTTP_URL/metrics
#   (run-shard.sh's 19091->8090 tunnel). That is unreachable, verified against the
#   live gardenio-system bridge:
#     • WRONG PORT — 19091 forwards to the bridge's :8090 escalation API + /healthz
#       mux; :8090/metrics is HTTP 404. /metrics is a SEPARATE http.Server on :9093
#       (cmd/slack-bridge/main.go, resolveMetricsAddr default ":9093"), which NO
#       shard port-forwards (run-shard.sh:2499 documents the only :9093 in the
#       suite is coop's AGENT-pod port, not the bridge).
#     • EMPTY UNTIL A REAL RESPAWN — respawnPhaseSeconds/respawnPhaseSlowTotal are a
#       promauto HistogramVec/CounterVec with no zero-init; a *Vec with zero child
#       series marshals NOTHING to /metrics until the first observation, and the
#       only producer is a real cold thread/DM respawn completing
#       (agentstream.go:1211), which no e2e module drives.
#   So — exactly like test-gitlab-webhook-deadman.sh / test-session-spend-
#   attribution.sh — this module wraps a targeted `go test -run` against the REAL
#   production code (recordRespawnPhase + a promhttp DefaultGatherer endpoint),
#   exercised as an integration flow over a REAL /metrics HTTP endpoint. Hermetic
#   (no pods, no cluster, no shared-daemon writes), so it is green today and is a
#   HARD gate.
#
# ── What the wrapped Go legs prove (the deployed shape) ───────────────────────
#   Leg exposure   (TestSlackBridgeRespawnPhaseE2E_ExposureAndAntiVacuity):
#     both series are ABSENT from the served exposition before any observation (the
#     anti-vacuity control), and PRESENT with HELP/TYPE after one real observation
#     per phase — the registry-regression guard.
#   Leg bounding   (TestSlackBridgeRespawnPhaseE2E_LabelBounding):
#     every served _seconds sample carries a phase in {dispatch,boot,total}, an
#     empty role collapses to role="unknown", and an out-of-enum phase driven
#     through the real entry point never reaches the wire — the cardinality guard.
#   Leg slow       (TestSlackBridgeRespawnPhaseE2E_SlowCounterSemantics):
#     an under-threshold phase does NOT bump the served _slow_total; an over-
#     threshold phase bumps it by exactly 1 and it is monotonic — asserted BY VALUE.
#   Leg name-match (TestSlackBridgeRespawnPhaseE2E_ServedNamesMatchShippedAlertExpr):
#     the served series names are exactly the strings the SHIPPED vmalert rule
#     (SlackBridgeRespawnPhaseSlow) keys off — a one-sided rename reddens. The
#     cross-tree helm read SKIPs LOUD only in a layer that omits helm/ (never a
#     pass for a rename); the served-name half hard-asserts everywhere.
#
# ── Hermetic HARD gate ────────────────────────────────────────────────────────
#   No pods, no live cluster, no shared-daemon writes — everything runs in-process
#   against httptest servers. So it is green today and is a HARD gate: any leg
#   failure reddens the shard. It SKIPs clean (exit 0) ONLY when the Go toolchain /
#   controller source is genuinely absent — a missing toolchain must not be read as
#   a pass, so the skip is LOUD.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-slackbridge-respawn-phase-metrics.sh

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
  red " SKIP slackbridge-respawn-phase-metrics (LOUD): $why"
  red " This run did NOT assert the slack-bridge respawn-phase metrics pipeline"
  red " (served gardenio_slackbridge_respawn_phase_seconds/_slow_total exposure +"
  red " anti-vacuity control, phase/role label bounding, slow-counter semantics,"
  red " served-names ↔ shipped-vmalert-expr coupling). A green suite here proves"
  red " NOTHING about that pipeline. The e2e shard is built with a Go toolchain +"
  red " the controller source — if this fires in CI, that wiring regressed."
  red "════════════════════════════════════════════════════"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

echo "════════════════════════════════════════════════════"
echo " slack-bridge respawn-phase metrics E2E (ka-0000003422)"
echo " served exposure+anti-vacuity · label bounding · slow-counter · alert-expr name-match"
echo "════════════════════════════════════════════════════"

# ── Preflight: Go toolchain + controller source present ─────────────────────
command -v go >/dev/null 2>&1 || skip_all "no go toolchain to run the respawn-phase integration test"
[ -d "${CONTROLLER_DIR}/internal/bridge" ] \
  || skip_all "controller bridge source missing under ${CONTROLLER_DIR}"

# A leg = one targeted `go test -run` against a real production surface. Each is
# a HARD sub-gate; any failure reddens the shard.
run_leg() {
  local name="$1" pkg="$2" run="$3"
  local log="/tmp/slackbridge-respawn-phase-${name}-${RUN_ID}.log"
  blue "[leg ${name}] go test ${pkg} -run '${run}'"
  if (cd "${CONTROLLER_DIR}" && go test -v "${pkg}" -run "${run}" -count=1 -timeout 120s) \
      >"${log}" 2>&1; then
    local n
    n=$(grep -cE '^--- PASS' "${log}" 2>/dev/null | tr -d '[:space:]')
    pass "leg ${name} — ${n:-?} test(s) green"
  else
    fail "leg ${name}" "go test failed:
$(tail -25 "${log}" | sed 's/^/      /')"
  fi
}

# All four legs live in internal/bridge and share the TestSlackBridgeRespawnPhaseE2E
# prefix (respawn_phase_e2e_test.go): served exposure+anti-vacuity, phase/role
# label bounding, slow-counter monotonicity, and served-names ↔ shipped-vmalert
# name-match.
run_leg "respawn-phase" ./internal/bridge/ 'TestSlackBridgeRespawnPhaseE2E'

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
