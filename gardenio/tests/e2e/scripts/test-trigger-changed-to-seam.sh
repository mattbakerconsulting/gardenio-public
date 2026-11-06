#!/usr/bin/env bash
# test-trigger-changed-to-seam.sh — LIVE-DAEMON E2E gate for the declarative
# bead-trigger `changed_to` / `field_changed` operators across the four-process
# daemon→SSE→bridge seam (bead ka-0000002238, parent ka-0000000958).
#
# ── The gap this closes ───────────────────────────────────────────────────────
#   The diff-aware trigger operators field_changed / changed_to
#   (trigger_source_bead.go:634/636) had ZERO end-to-end coverage: a HEAD grep for
#   `changed_to` / `field_changed` returned 0 hits across BOTH e2e suites
#   (tests/e2e/, tests/beads-viewer-e2e/) and 0 across every *_e2e_test.go. They
#   consume the per-field `changes` overlay the daemon produces in perFieldChanges
#   (kbeads beads.go:1055, published :1241) and serializes over SSE (events.go:80
#   `changes` tag) — a seam spanning four processes that nothing exercised whole.
#
# ── Why a LIVE-DAEMON go:build e2e test, not a shell reimplementation nor the
#    trigger-probe ──────────────────────────────────────────────────────────────
#   Every test that touches these operators today starts DOWNSTREAM of the wire,
#   so none would red if the seam broke:
#     - kbeads beads_field_changes_test.go calls perFieldChanges as a PURE
#       FUNCTION (never HTTP/publish/serialize).
#     - trigger_liveness_invariants_t1_test.go (B3) hand-stuffs src.pending with a
#       precomputed Changes map — it starts downstream of the entire wire.
#     - kbeads sse_integration_test.go reads `changes` off a real SSE stream but
#       asserts changes["title"] — a first-class COLUMN from a DIFFERENT branch
#       (beads.go:983), not perFieldChanges (different-guard-same-name).
#     - the trigger-probe (--source=bead) injects a synthetic BeadEvent via
#       InjectBeadEvent with NO Changes map, so the shell trigger-action-fidelity
#       path structurally cannot reach these ops; adding a --bead-changes flag
#       would only recreate the B3 hand-stuffed-map trap past the wire.
#   So this module wraps a `go test -tags e2e -run` that drives the REAL
#   bridge.SSEStream (the exact NewSSEStream+RegisterSSE wiring prod's
#   trigger-engine uses, main.go:141-142) connected to the LIVE daemon, then
#   mutates a watched trigger bead's `enabled` field via the REAL beadsapi.Client.
#   That forces the whole seam: perFieldChanges → published `changes` overlay →
#   SSE frame → sse.go:398 decode → BeadStateTriggerSource.pending → Evaluate →
#   matchOneCondition (changed_to / field_changed). It keys every assertion on the
#   ONE watched bead it seeded (hermetic on a shared daemon) and never authors a
#   live type:trigger bead the shard engine could fire (the seeded bead carries
#   fixture:e2e: so isE2EFixture skips it).
#
# ── What the wrapped Go test asserts (each on the seeded bead only) ────────────
#   TestTriggerChangedToFieldChangedSeamLiveDaemonE2E, four legs:
#     1 changed-to-fires-on-transition               enabled:changed_to:false FIRES
#                                                     once on the real enabled=true→
#                                                     false transition.
#     2 changed-to-silent-on-unrelated-update        a note-only update to the SAME
#                                                     bead stays SILENT (anti-vacuity:
#                                                     `changes` lacks enabled).
#     3 field-changed-fires-on-transition-silent-otherwise  enabled:field_changed
#                                                     fires on the transition, silent
#                                                     on a note-only update.
#     4 changed-to-wrong-value-does-not-fire          enabled:changed_to:TRUE does NOT
#                                                     fire on a →false transition, but
#                                                     a changed_to:false control DOES —
#                                                     so the zero is a value mismatch,
#                                                     not a missed event.
#   Mutation-verified while authoring: reverting the kbeads fieldChanges overlay
#   (beads.go:1241-1249) so the daemon emits only the opaque "fields" key reds
#   legs 1 and 3 (the `changes` map no longer carries enabled →
#   changeMapLookup/changeMapHas return !ok).
#
# ── Skip-clean contract ──────────────────────────────────────────────────────
#   SKIPs CLEANLY (exit 0) with a LOUD banner when the Go toolchain / controller
#   source is absent, OR the beads daemon is unreachable — a missing prerequisite
#   must never be read as a pass. When present, every leg is a HARD assertion.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-trigger-changed-to-seam.sh
#   BEADS_HTTP_URL=http://localhost:19090 ./gardenio/tests/e2e/scripts/test-trigger-changed-to-seam.sh

set -uo pipefail

red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"
CONTROLLER_DIR="${CONTROLLER_DIR:-${REPO_ROOT}/controller}"
RUN_ID="${RUN_ID:-$$}"
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
RUN_FILTER="TestTriggerChangedToFieldChangedSeamLiveDaemonE2E"

PASS=0
FAIL=0
ERRORS=()
PF_PID=""

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
  red " SKIP trigger-changed-to-seam (LOUD): $why"
  red " This run did NOT assert the changed_to / field_changed operators across"
  red " the daemon->SSE->bridge seam (perFieldChanges -> SSE changes -> sse.go:398"
  red " -> matchOneCondition). A green suite here proves NOTHING about"
  red " ka-0000002238. The e2e shard is built with a Go toolchain + the controller"
  red " source and a reachable beads daemon — if this fires in CI once the daemon"
  red " is up, that wiring regressed."
  red "════════════════════════════════════════════════════"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

cleanup() {
  [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null || true
  rm -f "/tmp/trigger-changed-to-seam-${RUN_ID}.log" 2>/dev/null || true
}
trap cleanup EXIT

echo "════════════════════════════════════════════════════"
echo " trigger changed_to / field_changed SEAM E2E (ka-0000002238)"
echo " daemon perFieldChanges → SSE changes → sse.go:398 → matchOneCondition"
echo "════════════════════════════════════════════════════"

# ── Preflight: Go toolchain + controller source ──────────────────────────────
command -v go >/dev/null 2>&1 || skip_all "no go toolchain to run the changed_to-seam live-daemon test"
[ -d "${CONTROLLER_DIR}/internal/bridge" ] \
  || skip_all "controller source missing under ${CONTROLLER_DIR}"

# ── Resolve the daemon: prefer an exported BEADS_HTTP_URL (in-shard), else
#    port-forward the namespace beads service (standalone/local dev). ──────────
if [ -z "${BEADS_HTTP_URL:-}" ] && [ -n "${BEADS_HTTP_ADDR:-}" ]; then
  BEADS_HTTP_URL="$BEADS_HTTP_ADDR"
fi
if [ -z "${BEADS_HTTP_URL:-}" ]; then
  command -v kubectl >/dev/null 2>&1 \
    || skip_all "no BEADS_HTTP_URL and no kubectl to port-forward the daemon"
  blue "[pf] port-forwarding ${NAMESPACE} svc/${BEADS_SVC}"
  pf_local_port=19163
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${pf_local_port}:8080" \
    >"/tmp/trigger-changed-to-seam-${RUN_ID}.log" 2>&1 &
  PF_PID=$!
  BEADS_HTTP_URL="http://localhost:${pf_local_port}"
  # Wait for readiness (healthz answers 200 or 401 — either proves reachability).
  ready=0
  for _ in $(seq 1 15); do
    code=$(curl -s -o /dev/null -w "%{http_code}" "${BEADS_HTTP_URL}/healthz" 2>/dev/null || echo 000)
    if [ "$code" = "200" ] || [ "$code" = "401" ]; then ready=1; break; fi
    sleep 1
  done
  [ "$ready" = "1" ] || skip_all "port-forward to ${NAMESPACE}/${BEADS_SVC} never became ready"
fi
export BEADS_HTTP_URL
blue "[daemon] $BEADS_HTTP_URL"

# Token: the shard exports BEADS_AUTH_TOKEN/BEADS_TOKEN; the Go test reads either
# (plus BEADS_DAEMON_TOKEN). Surface a hint if none is present, but let the test's
# own skip-vs-fail logic decide (an authless daemon is a legitimate local case).
if [ -z "${BEADS_AUTH_TOKEN:-}${BEADS_TOKEN:-}${BEADS_DAEMON_TOKEN:-}" ]; then
  dim "  (no BEADS_AUTH_TOKEN/BEADS_TOKEN set — the daemon may reject writes)"
fi

# ── Run the wrapped live-daemon Go test (real SSE stream, hermetic per-bead) ──
LOG="/tmp/trigger-changed-to-seam-run-${RUN_ID}.log"
blue "[run] go test -tags e2e -run ${RUN_FILTER} (drives the REAL SSEStream + BeadStateTriggerSource)"
if (cd "${CONTROLLER_DIR}" && go test -tags e2e -v ./internal/bridge/ -run "${RUN_FILTER}" -count=1 -timeout 180s) \
    >"${LOG}" 2>&1; then
  test_rc=0
else
  test_rc=$?
fi

# A t.Skip inside the test (unreachable daemon / missing token) prints SKIP and
# still exits 0 — surface that as a LOUD module skip rather than a silent pass.
if grep -qE '^\s*--- SKIP: '"${RUN_FILTER}"'\b' "${LOG}" 2>/dev/null \
   || grep -qE 'SKIP \(LOUD\):' "${LOG}" 2>/dev/null; then
  why=$(grep -oE 'SKIP \(LOUD\):.*' "${LOG}" 2>/dev/null | head -1 || echo "test self-skipped")
  skip_all "${why}"
fi

# Map each expected sub-test to a pass/fail assertion; require ALL four present so
# a test that silently drops a leg cannot pass.
EXPECTED_LEGS="changed-to-fires-on-transition changed-to-silent-on-unrelated-update field-changed-fires-on-transition-silent-otherwise changed-to-wrong-value-does-not-fire"
for leg in $EXPECTED_LEGS; do
  if grep -qE '^\s*--- PASS: '"${RUN_FILTER}"'/'"${leg}"'\b' "${LOG}" 2>/dev/null; then
    pass "leg ${leg}"
  elif grep -qE '^\s*--- FAIL: '"${RUN_FILTER}"'/'"${leg}"'\b' "${LOG}" 2>/dev/null; then
    detail=$(grep -A3 -E '/'"${leg}"'\b' "${LOG}" 2>/dev/null | grep -E '\.go:[0-9]+:' | head -1 | sed 's/^[[:space:]]*//' || true)
    fail "leg ${leg}" "${detail:-see log}"
  else
    fail "leg ${leg}" "no PASS/FAIL result for this sub-test (leg dropped)"
  fi
done

# Belt-and-suspenders: the go-test exit code must agree with our parse.
if [ "${test_rc}" -ne 0 ] && [ "$FAIL" -eq 0 ]; then
  fail "go test exit code" "go test exited ${test_rc} but every parsed leg passed — test-level error: $(tail -6 "${LOG}" 2>/dev/null | tr '\n' ' ')"
fi

echo
blue "════════════════════════════════════════════════════"
total=$((PASS + FAIL))
if [ "$FAIL" -eq 0 ]; then
  green " ✓ All $total legs passed"
else
  red " ✗ $FAIL/$total legs failed"
  echo
  for err in "${ERRORS[@]}"; do red "   - $err"; done
  echo
  dim "  go test output (tail):"
  tail -30 "${LOG}" 2>/dev/null | sed 's/^/    /' || true
fi
blue "════════════════════════════════════════════════════"
[ "$FAIL" -eq 0 ]
