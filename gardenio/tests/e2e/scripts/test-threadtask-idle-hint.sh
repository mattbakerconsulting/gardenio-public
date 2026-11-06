#!/usr/bin/env bash
# test-threadtask-idle-hint.sh — LIVE-DAEMON E2E gate for the thread-task
# idle-hint DEMOTION path (bead ka-0000007208, parent ka-0000000958; flag flip
# ka-0000000287).
#
# ── The gap this closes ───────────────────────────────────────────────────────
#   MR 7d35eb2ce (ka-0000000287) did TWO things: it added a 9-scenario Go UNIT
#   suite (thread_task_autoclose_demoted_test.go, over a mockDaemon) AND it
#   flipped slackBridge.slack.threadTaskHintInsteadOfClose: true in
#   the staging values — inverting the LIVE behaviour of the
#   thread-task auto-closer in the EXACT namespace this suite runs against: the
#   death rule now DEMOTES an idle wish to an actionable hint and leaves it OPEN
#   instead of closing it. A selector grep for gardenio-idle-hint / idle-hint /
#   HintInsteadOfClose / threadTaskHint returned ZERO hits across tests/e2e/ and
#   tests/beads-viewer-e2e/, and the "auto-close" shell modules
#   (test-auto-close-cascade.sh, test-stale-reaper.sh) cover DIFFERENT subsystems
#   (parent-child close cascade + the stale-AGENT reaper — a `thread_task` grep
#   inside them is empty). So the demoted world had NO e2e assertion at all.
#
# ── Why a LIVE-DAEMON go:build e2e test, not a shell reimplementation ─────────
#   The demotion logic lives in the BRIDGE (internal/bridge, ThreadTaskAutoCloser),
#   driven by a 30m close window + a 15m backfill sweep with NO admin/sweep-trigger
#   endpoint — the "far past a shard's wall-clock budget" constraint that makes
#   escalation-auto-reaper a probe rather than a live-bridge leg. A shell test can
#   neither wait out the window nor reach runCheck/sweepOnce (both unexported), so
#   it could only re-implement the contract in bash. Instead — following the
#   sibling escalation /result live-daemon backfill (58054fe9f: "a mock
#   re-implementing the daemon is not evidence the daemon does it") — this module
#   wraps a `go test -tags e2e -run` that drives the REAL
#   ThreadTaskAutoCloser.runCheck (the same event-path code the bridge runs in
#   prod) against a REAL beadsapi.Client pointed at the live daemon. It exercises
#   three real daemon semantics the mock cannot model and that the demotion path
#   depends on: (1) a comment write does NOT bump the bead's updated_at (so the
#   activity-relative dedupe is deterministic); (2) a write with no operator
#   fail-closes 400, so autoCloseOperatorContext's per-call attribution is
#   load-bearing; (3) the live type=task schema REJECTS the triage_reason field,
#   so the needs-triage LABEL + COMMENT are the durable signals (not the field).
#
# ── What the wrapped Go test asserts (event path only — hermetic on a shared
#    daemon) ─────────────────────────────────────────────────────────────────
#   TestThreadTaskIdleHintDemotionLiveDaemonE2E seeds its OWN thread wish per
#   sub-test under a unique synthetic slack-thread:{channel}:{ts} label and keys
#   only on that bead id (never reads ambient prod state). runCheck is scoped to a
#   single thread bead so it touches ONLY the seeded bead; the backfill sweep
#   (which lists every open source:slack task) is deliberately NOT driven here —
#   it is not hermetic on a shared daemon, and its dedupe/positive-evidence legs
#   stay in the mock unit suite. Five legs, one per task contract point:
#     1 demotion            a done agent → 1 idle-hint COMMENT + wish stays OPEN.
#     2 marker + affordance the hint ends with the [gardenio-idle-hint] marker on
#                           its own trailing line and carries the ⏹️
#                           (:black_square_for_stop:) in-place close affordance;
#                           asserted on the MARKER, not the prose.
#     3 dedupe              two checks in one idle episode → exactly ONE hint.
#     4 fresh episode       a human reply newer than the hint opens a new episode
#                           → the next check re-hints (activity-relative, not a gag).
#     5 failed carve-out    a FAILED agent → wish stays OPEN + needs-triage label +
#                           triage comment, NEVER idle-hinted.
#   Mutation-verified while authoring: driving the failed leg with a DONE agent
#   reds leg 5 (a done agent hints, earning no triage label/comment).
#
# ── Skip-clean contract ──────────────────────────────────────────────────────
#   SKIPs CLEANLY (exit 0) with a LOUD banner when the Go toolchain / controller
#   source is absent, OR the beads daemon is unreachable — a missing prerequisite
#   must never be read as a pass. When present, every leg is a HARD assertion.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-threadtask-idle-hint.sh
#   BEADS_HTTP_URL=http://localhost:19090 ./gardenio/tests/e2e/scripts/test-threadtask-idle-hint.sh

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
RUN_FILTER="TestThreadTaskIdleHintDemotionLiveDaemonE2E"

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
  red " SKIP threadtask-idle-hint (LOUD): $why"
  red " This run did NOT assert the thread-task idle-hint DEMOTION path"
  red " (idle → hint-once + wish-stays-OPEN, the [gardenio-idle-hint] marker + ⏹️"
  red " affordance, per-episode dedupe, fresh-episode re-hint, and the failed→"
  red " needs-triage carve-out). A green suite here proves NOTHING about"
  red " ka-0000007208 / ka-0000000287. The e2e shard is built with a Go toolchain"
  red " + the controller source and a reachable beads daemon — if this fires in"
  red " CI once the daemon is up, that wiring regressed."
  red "════════════════════════════════════════════════════"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

cleanup() {
  [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null || true
  rm -f "/tmp/threadtask-idle-hint-${RUN_ID}.log" 2>/dev/null || true
}
trap cleanup EXIT

echo "════════════════════════════════════════════════════"
echo " thread-task idle-hint DEMOTION E2E (ka-0000007208)"
echo " idle → hint + wish-stays-OPEN · marker+⏹️ · dedupe · re-hint · failed→triage"
echo "════════════════════════════════════════════════════"

# ── Preflight: Go toolchain + controller source ──────────────────────────────
command -v go >/dev/null 2>&1 || skip_all "no go toolchain to run the idle-hint live-daemon test"
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
  pf_local_port=19147
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${pf_local_port}:8080" \
    >"/tmp/threadtask-idle-hint-${RUN_ID}.log" 2>&1 &
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

# ── Run the wrapped live-daemon Go test (event path, hermetic per-bead) ───────
LOG="/tmp/threadtask-idle-hint-run-${RUN_ID}.log"
blue "[run] go test -tags e2e -run ${RUN_FILTER} (drives the REAL ThreadTaskAutoCloser.runCheck)"
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

# Map each expected sub-test to a pass/fail assertion; require ALL five present so
# a test that silently drops a leg cannot pass.
EXPECTED_LEGS="demotion-hint-leaves-open marker-and-close-affordance dedupe-one-hint-per-episode fresh-activity-reopens-episode failed-agent-needs-triage-stays-open"
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
