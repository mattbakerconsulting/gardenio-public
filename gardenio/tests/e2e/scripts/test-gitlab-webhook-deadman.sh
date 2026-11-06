#!/usr/bin/env bash
# test-gitlab-webhook-deadman.sh — E2E: GitLab inbound-webhook dark-hook deadman,
# driven end-to-end through the REAL monitor + webhook handler over a REAL
# promhttp /metrics endpoint (backfill bead ka-0000005729, GAP-C of the non-AVA
# webhook carve-out ka-0000005632; runbook bug ka-0000000426).
#
# ── The gap this closes ───────────────────────────────────────────────────────
#   The GitLab webhook dark-hook deadman shipped (commit f56dfccd2) with ZERO
#   coverage of the deployed shape: the Go verdict logic had unit coverage
#   (gitlab_webhook_deadman_test.go, testutil.ToFloat64 over the in-process
#   registry), but NOTHING proved (a) the deadman series reach the Prometheus TEXT
#   EXPOSITION the gitlab-bridge actually serves on :9092, or (b) that the emitted
#   series names still match the metric names the SHIPPED PrometheusRule
#   (helm/gardenio/templates/gitlab-bridge/webhook-deadman-alert.yaml) keys its
#   expr off. A rename on EITHER side silently un-fires the page while the unit
#   test AND the helm render test both stay green — the classic unfireable alert.
#   And there was no helm render coverage at all: the one test that LOOKS like it
#   covers the rule keys every --set on jiraBridge.* (the name-match trap), so the
#   gitlab template could be deleted outright and that test stayed green (proven
#   by the mutation test in the MR PROOF block).
#
# ── Why a go-test module, not a live-bridge port-forward ──────────────────────
#   The gitlab-bridge is OFF in the ephemeral e2e namespace (see
#   deploy-values), so there is no live /webhook to POST to and no monitor loop
#   running — the SAME constraint that makes test-gitlab-dark-provenance.sh /
#   test-jira-conversational.sh build a from-source probe instead of scraping a
#   live bridge. The logic under test (GitLabWebhookMonitor.evaluate, the real
#   GitLabWebhookHandlerWithConfig receive path, the promhttp /metrics exposition)
#   is all in-process Go with no product image. So — exactly like
#   test-session-spend-attribution.sh — this module wraps a targeted `go test
#   -run` against the REAL production code, exercised as an integration flow over
#   a REAL promhttp /metrics HTTP endpoint. It is hermetic (no pods, no cluster,
#   no shared-daemon writes), so it is green today and is a HARD gate.
#
# ── What the wrapped Go test proves (the deployed shape) ──────────────────────
#   Leg served-gauge  (internal/bridge, TestGitLabWebhookDeadmanE2E_ServedGauge*):
#     A REAL authenticated delivery POSTed through the ACTUAL
#     GitLabWebhookHandlerWithConfig stamps the receive-liveness clock, so the
#     next monitor evaluate() reads healthy and the SERVED /metrics exposition
#     reads gardenio_gitlab_webhook_dark 0 (the ANTI-VACUITY control — the ==1
#     assertion is meaningless without a proven 0 on the same endpoint). Then, with
#     no delivery in the window while upstream MR activity exists, evaluate()
#     folds the verdict to 1 and it reaches the served exposition as
#     gardenio_gitlab_webhook_dark 1, with the evaluations_total heartbeat also on
#     the wire.
#   Leg name-match  (TestGitLabWebhookDeadmanE2E_ServedNamesMatchShippedAlertExpr):
#     the metric names present on the served endpoint are exactly the strings the
#     SHIPPED PrometheusRule template keys its expr off (max_over_time(<dark>) and
#     absent(<evaluations_total>)). A one-sided rename reddens — the only test
#     that couples the emitted series to the paging rule.
#
# ── Hermetic HARD gate ────────────────────────────────────────────────────────
#   No pods, no live cluster, no shared-daemon writes — everything runs in-process
#   against httptest servers. So it is green today and is a HARD gate: any leg
#   failure reddens the shard. It SKIPs clean (exit 0) ONLY when the Go toolchain /
#   controller source is genuinely absent — a missing toolchain must not be read as
#   a pass, so the skip is LOUD.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-gitlab-webhook-deadman.sh

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
  red " SKIP gitlab-webhook-deadman (LOUD): $why"
  red " This run did NOT assert the GitLab webhook dark-hook deadman pipeline"
  red " (served gardenio_gitlab_webhook_dark 0/1 flip, the real-delivery anti-vacuity"
  red " control, the served-names ↔ shipped-alert-expr coupling). A green suite here"
  red " proves NOTHING about that pipeline. The e2e shard is built with a Go"
  red " toolchain + the controller source — if this fires in CI, that wiring"
  red " regressed."
  red "════════════════════════════════════════════════════"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

echo "════════════════════════════════════════════════════"
echo " gitlab webhook dark-hook deadman E2E (ka-0000005729)"
echo " served dark gauge flip · real-delivery anti-vacuity · alert-expr name-match"
echo "════════════════════════════════════════════════════"

# ── Preflight: Go toolchain + controller source present ─────────────────────
command -v go >/dev/null 2>&1 || skip_all "no go toolchain to run the deadman integration test"
[ -d "${CONTROLLER_DIR}/internal/bridge" ] \
  || skip_all "controller bridge source missing under ${CONTROLLER_DIR}"

# A leg = one targeted `go test -run` against a real production surface. Each is
# a HARD sub-gate; any failure reddens the shard.
run_leg() {
  local name="$1" pkg="$2" run="$3"
  local log="/tmp/gitlab-webhook-deadman-${name}-${RUN_ID}.log"
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

# Served /metrics 0→1 flip + real-delivery anti-vacuity control + served-names ↔
# shipped-alert-expr coupling. Both e2e tests live in internal/bridge and match
# the TestGitLabWebhookDeadmanE2E prefix.
run_leg "served-deadman" ./internal/bridge/ 'TestGitLabWebhookDeadmanE2E'

# Served /metrics coverage of the RECEIVE-PATH counter
# gardenio_gitlab_webhook_deliveries_total{outcome} (backfill ka-0000001762): the
# full 5-outcome label vocabulary reaches the wire (the alert's documented triage
# surface, webhook-deadman-alert.yaml:100), AND the load-bearing security property
# that a token_failed delivery is visible on the wire yet does NOT stamp the
# receive-liveness clock — so a wrong-secret hook pages the served dark gauge to 1
# rather than reading as healthy. Anti-vacuity: a real authenticated-delivery
# control proves the same served endpoint reads dark 0 when the clock IS stamped.
# Distinct TestGitLabWebhookDeliveriesE2E prefix, same internal/bridge package.
run_leg "served-deliveries" ./internal/bridge/ 'TestGitLabWebhookDeliveriesE2E'

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
