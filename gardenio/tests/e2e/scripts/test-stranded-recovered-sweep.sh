#!/usr/bin/env bash
# test-stranded-recovered-sweep.sh — E2E: the level-triggered
# ReconcileStrandedRecovered sweep, driven end-to-end against a REAL apiserver
# through the REAL podmanager.K8sManager (bead gd-DMvno7uS3J, split from
# ka-0000001464; parent E2E coverage-gap epic ka-0000000958).
#
# ── The gap this closes ───────────────────────────────────────────────────────
#   ReconcileStrandedRecovered (controller/cmd/controller/account_rotation_restart.go)
#   is the level-triggered sweep that recycles a pod baked onto a STILL-exhausted
#   account once its PROJECT POOL regains a healthy alternative — the whole-pool
#   exhaustion case the edge-triggered accountRotationRestarter.Reconcile can never
#   catch (no active→exhausted edge ever fired, because the account was already
#   exhausted at spawn). Its decision line is "restarting agent pod stranded on
#   exhausted account; project pool has recovered".
#
#   It shipped (2026-06-29, db432b759) with ZERO e2e in EITHER suite: a HEAD grep
#   for "stranded on exhausted" / "project pool has recovered" returns 0 hits
#   across gardenio/tests/e2e/scripts + tests/beads-viewer-e2e. Its two sibling
#   sweeps (stale-vkey + keyless-gained-key) were backfilled in
#   test-credential-rotation.sh Scenario 5 (ka-0000001464); that bead explicitly
#   sanctioned splitting StrandedRecovered here because it needs a whole
#   project-pool-recovery fixture the husk-per-single-account siblings do not.
#   Unit coverage exists but every case drives a FAKE mockPodManager — nothing
#   exercised the REAL K8s list→decide→delete chain.
#
# ── Why a from-source LIVE-CLUSTER go-test module, not a shell log-poll ───────
#   The sweep runs every 5 MINUTES (strandedRecoveryInterval) and can sit out up
#   to strandedRecoveryBlindSweeps further ticks after a coopmux blip, so polling
#   the DEPLOYED controller's decision log would need a ~15-minute budget AND would
#   be at the mercy of whether the deployed image even carries the sweep (a lagging
#   staging controller may predate a later guard). Instead — exactly as the bead's
#   landing note directs ("Prove via the same in-cluster harness method (real
#   K8sManager, isolated scratch ns) since the deployed staging controller may
#   lag") — this module wraps a targeted `go test -tags e2e -run` that builds the
#   REAL podmanager.K8sManager against the live apiserver, plants husk pods in an
#   ISOLATED SCRATCH NAMESPACE, and calls ReconcileStrandedRecovered directly. It
#   is deterministic (one call, no 5-min wait), version-INDEPENDENT (drives the
#   SOURCE the release ships, not staging's image), and fully isolated (the
#   deployed controller only ever sweeps its own namespace, never the scratch ns).
#
# ── What the wrapped Go test proves (each leg on its own uniquely-named husk) ──
#   TestReconcileStrandedRecoveredE2E_RestartsWhenSiblingHealthy (POSITIVE+FOREIGN):
#     a real Pending husk baked onto the exhausted+rejected+TERMINAL account, in a
#     project whose pool has a healthy keyed sibling, is DELETED from the apiserver
#     by the real sweep (and the sweep's returned pod list names exactly it), while
#     a co-resident husk on the healthy account's own key is LEFT in place.
#   TestReconcileStrandedRecoveredE2E_NoOpWhenPoolStillExhausted (NEGATIVE control):
#     with the healthy sibling REMOVED (pool still fully exhausted), the
#     identically-baked husk is LEFT alone — without this a sweep that deleted
#     every matched pod would pass the positive leg vacuously.
#   Mutation-verified while authoring: gutting ReconcileStrandedRecovered to
#   `return nil` reds the positive leg; dropping the "no healthy alternative"
#   no-op guard reds the negative control.
#
# ── Skip-clean contract ──────────────────────────────────────────────────────
#   SKIPs CLEANLY (exit 0, LOUD banner + E2E_SKIP_SENTINEL) when the Go toolchain
#   / controller source is absent, OR no cluster is reachable — a missing
#   substrate must never be read as a pass. When both are present every leg is a
#   HARD assertion and any failure reddens the shard. The scratch namespaces are
#   always torn down by the Go test itself.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-stranded-recovered-sweep.sh

set -uo pipefail

red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
CONTROLLER_DIR="${CONTROLLER_DIR:-${REPO_ROOT}/controller}"
RUN_ID="${RUN_ID:-$$}"
RUN_FILTER="TestReconcileStrandedRecoveredE2E"

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

# LOUD skip — exit 0 (a missing prerequisite must NOT red the suite) with a
# banner so a skip is never silently read as a pass.
skip_all() {
  local why="$1"
  red "════════════════════════════════════════════════════"
  red " SKIP stranded-recovered-sweep (LOUD): $why"
  red " This run did NOT assert the level-triggered ReconcileStrandedRecovered"
  red " sweep (real K8sManager list→decide→delete over a scratch ns, positive +"
  red " negative-control legs). A green suite here proves NOTHING about"
  red " gd-DMvno7uS3J. The e2e shard is built with a Go toolchain + the controller"
  red " source and reaches the live apiserver — if this fires in CI, that wiring"
  red " regressed."
  red "════════════════════════════════════════════════════"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

echo "════════════════════════════════════════════════════"
echo " stranded-recovered sweep E2E (gd-DMvno7uS3J)"
echo " real K8sManager · scratch ns · project-pool recovery · negative control"
echo "════════════════════════════════════════════════════"

# ── Preflight: Go toolchain + controller source present ─────────────────────
command -v go >/dev/null 2>&1 || skip_all "no go toolchain to run the stranded-recovered sweep test"
[ -d "${CONTROLLER_DIR}/cmd/controller" ] \
  || skip_all "controller source missing under ${CONTROLLER_DIR}"

LOG="/tmp/stranded-recovered-sweep-${RUN_ID}.log"
blue "[run] go test -tags e2e -run ${RUN_FILTER} (real K8sManager against the live apiserver)"
(cd "${CONTROLLER_DIR}" && go test -tags e2e -v ./cmd/controller/ -run "${RUN_FILTER}" -count=1 -timeout 180s) \
  >"${LOG}" 2>&1
test_rc=$?

# Classify the outcome. The Go test t.Skip's when no cluster is reachable — that
# is a substrate gap (exit 0 + SKIP lines, no PASS), NOT a pass. Route it to the
# LOUD skip so it is never read as a green assertion.
n_pass=$(grep -cE '^\s*--- PASS' "${LOG}" 2>/dev/null | tr -d '[:space:]')
n_skip=$(grep -cE '^\s*--- SKIP' "${LOG}" 2>/dev/null | tr -d '[:space:]')
: "${n_pass:=0}"; : "${n_skip:=0}"

if [ "$test_rc" -ne 0 ]; then
  fail "stranded-recovered sweep" "go test failed:
$(tail -30 "${LOG}" | sed 's/^/      /')"
elif [ "$n_pass" -gt 0 ]; then
  pass "stranded-recovered sweep — ${n_pass} leg(s) green (positive + negative control)"
  [ "$n_skip" -gt 0 ] && dim "    (${n_skip} leg(s) self-skipped — partial substrate)"
else
  # exit 0 but nothing PASSed → every leg self-skipped (no cluster). LOUD skip.
  skip_all "go test reachable but every leg self-skipped (no cluster from the shard) — tail:
$(tail -6 "${LOG}" 2>/dev/null | tr '\n' ' ')"
fi

echo
blue "════════════════════════════════════════════════════"
total=$((PASS + FAIL))
if [ "$FAIL" -eq 0 ]; then
  green " ✓ All $total leg(s) passed"
else
  red " ✗ $FAIL/$total leg(s) failed"
  echo
  for err in "${ERRORS[@]}"; do red "   - $err"; done
fi
blue "════════════════════════════════════════════════════"

[ "$FAIL" -eq 0 ]
