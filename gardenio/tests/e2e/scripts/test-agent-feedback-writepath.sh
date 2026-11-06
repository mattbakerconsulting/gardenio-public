#!/usr/bin/env bash
# test-agent-feedback-writepath.sh — E2E gate for the ALWAYS-ON agent-feedback
# write path: the real AgentFeedbackAPI handler → a stable type=rating feedback
# bead in the LIVE kbeads daemon, plus the daemon-enforced (rater, rated_bead_id)
# idempotent upsert that the feedback→bug trigger (ka-0000007151) reads.
#
# Bead ka-0000008183 (parent ka-0000000958). Guards commit de29900
# (ka-0000002210): the decoupled, unconditionally-registered
# POST /api/slack/agent-feedback endpoint.
#
# ── THE GAP THIS CLOSES ───────────────────────────────────────────────────────
#
# The write path shipped with ONE test — bot_agent_feedback_test.go — which
# drives the handler against a MOCK daemon whose CreateBead is a naive append:
# it never runs the type=rating field schema, never enforces the
# (rater, rated_bead_id) upsert, and never wires the `rates` dep edge. So the
# load-bearing behaviour the endpoint EXISTS for — "repeated feedback from the
# same rater on the same agent updates IN PLACE, not a pile of duplicate beads,
# and each bead links back to the agent it rates" — is asserted against nothing
# real. A grep of gardenio/tests for agent-feedback / slack-feedback /
# rated_bead_id / slack-feedback-only returns empty. Both failure directions hurt
# the feedback→bug pipeline: a silently-rejected write leaves the trigger with NO
# source (freeform feedback dropped), while an upsert regression mints a NEW
# rating bead per re-submission so the trigger fans out a DUPLICATE bug per edit.
#
# ── Why a from-source probe against the LIVE daemon, not a black-box HTTP test ─
#
# The bead proposed a black-box test (POST the route, assert the bead, re-POST,
# assert the upsert). That is impossible in the e2e shard: the HANDLER's host —
# the slack-bridge — is NOT DEPLOYED in the ephemeral e2e namespace where shards
# run. The e2e values ship slackBridge.enabled=
# false; only the long-lived STAGING SINGLETON
# runs the bridge. There is no /api/slack/agent-feedback endpoint to POST to —
# the same class of constraint that made the escalation-auto-reaper backfill
# (ka-0000006112) a probe rather than a live leg.
#
# But the valuable half IS live: the DAEMON that enforces the upsert, the rates
# edge, the type=rating schema, and the score=0/slack-feedback-only sentinel is
# deployed and reachable in EVERY e2e namespace. So cmd/agent-feedback-probe
# wires the REAL AgentFeedbackAPI handler (NewAgentFeedbackAPI + RegisterRoutes)
# to a REAL beadsapi.Client pointed at the LIVE daemon, and drives httptest
# requests through it. One POST therefore exercises PRODUCTION CODE on BOTH sides
# of the boundary the unit test could not reach: the handler's request→
# CreateBeadRequest mapping AND the daemon's preCreateRating. A refactor that
# drops the kind:slack-feedback label, the score=0 sentinel pairing, the rater
# precedence, the XSS escape, the rated_bead_id provenance, or the
# (rater,rated_bead_id) upsert reds this test.
#
# ── Legs (each a hard sub-gate; any FAIL reds the shard) ──────────────────────
#
#   1 writes-stable-bead   First POST → 200 {ok,bead_id}; daemon persisted a
#                          type=rating bead with feedback(==raw), feedback_raw,
#                          rated_bead_id, source=slack-feedback-only, score=0,
#                          rater=slack:<uid>, answered_at, and the
#                          kind:slack-feedback + agent:<id> + project:<p> labels.
#   2 rates-edge-wired     Daemon auto-wired the `rates` dep edge → source agent.
#   3 xss-escaped          Slack control syntax HTML-escaped in `feedback`; the
#                          original preserved in `feedback_raw`.
#   4 idempotent-upsert    Second POST from the SAME rater on the SAME agent →
#                          SAME bead_id, text updated in place, EXACTLY ONE bead.
#   5 distinct-rater-splits A DIFFERENT rater on the same agent → SEPARATE bead
#                          (key is the PAIR, not the agent alone).
#   6 rater-precedence     Explicit rater wins over slack_user_id; no-identity →
#                          anonymous fallback, bead still writable.
#   7 validation-gates     Missing agent_bead_id → 400; blank feedback → 400;
#                          unknown agent_bead_id → 404 (no bead written); GET →405.
#
# ── Skip-clean contract ───────────────────────────────────────────────────────
#
# The Go toolchain + cmd/agent-feedback-probe source are present in every e2e
# shard container. If either is absent the module SKIPS CLEANLY (exit 0) with a
# LOUD banner — an absent toolchain must never red the suite — mirroring the
# escalation-auto-reaper / gitlab-dark-provenance skip contract. The probe itself
# skips clean (still exit 0) when BEADS_HTTP_URL is unset or the live daemon is
# unreachable (an infra gap, not this module's gap). When both toolchain and
# daemon are present, every leg is a HARD assertion.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-agent-feedback-writepath.sh

set -uo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
CONTROLLER_DIR="${CONTROLLER_DIR:-gardenio/controller}"
RUN_ID="${RUN_ID:-$$}"
PROBE_BIN=""
PATH_INSTALLED=0
PF_PID=""

PASS=0
FAIL=0
ERRORS=()

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

# skip_all exits 0 (a missing toolchain/daemon must NOT red the suite) with a
# LOUD banner so a skip is never silently read as a pass.
skip_all() {
  local why="$1"
  red "════════════════════════════════════════════════════"
  red " SKIP agent-feedback-writepath (LOUD): $why"
  red " This run did NOT assert the always-on agent-feedback write path"
  red " (POST-shape → stable type=rating feedback bead: kind:slack-feedback label,"
  red " rated_bead_id provenance, rates edge, or the daemon-enforced"
  red " (rater, rated_bead_id) idempotent upsert). A green suite here proves"
  red " NOTHING about ka-0000002210 / the feedback→bug source ka-0000007151. The"
  red " e2e shard is built with a Go toolchain + the cmd/agent-feedback-probe"
  red " source and a reachable daemon — if this fires in CI, that wiring regressed."
  red "════════════════════════════════════════════════════"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

cleanup() {
  # Only remove a temp binary we built; never a PATH-installed one.
  [ "$PATH_INSTALLED" != "1" ] && [ -n "$PROBE_BIN" ] && rm -f "$PROBE_BIN" 2>/dev/null || true
  [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null || true
  rm -f "/tmp/agent-feedback-probe-${RUN_ID}.out" \
        "/tmp/agent-feedback-probe-${RUN_ID}.err" \
        "/tmp/agent-feedback-probe-build-${RUN_ID}.log" \
        "/tmp/agent-feedback-beads-pf-${RUN_ID}.log" 2>/dev/null || true
}
trap cleanup EXIT

echo "════════════════════════════════════════════════════"
echo " agent-feedback write-path E2E (ka-0000008183)"
echo " POST-shape → stable type=rating feedback bead; idempotent upsert per rater"
echo "════════════════════════════════════════════════════"

# ── 1. Ensure a live daemon URL (skip clean if none) ───────────────
# run-shard.sh exports BEADS_HTTP_URL (+ BEADS_TOKEN); when run standalone we
# port-forward the daemon ourselves. A missing daemon is an infra gap → skip.
if [ -z "${BEADS_HTTP_URL:-}" ]; then
  if kubectl -n "$NAMESPACE" get "svc/${BEADS_SVC}" >/dev/null 2>&1; then
    blue "[setup] port-forwarding ${NAMESPACE} daemon..."
    kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" 19096:8080 \
      >"/tmp/agent-feedback-beads-pf-${RUN_ID}.log" 2>&1 &
    PF_PID=$!
    export BEADS_HTTP_URL="http://localhost:19096"
    for _i in $(seq 1 15); do
      [ "$(curl -s --max-time 2 -o /dev/null -w '%{http_code}' "${BEADS_HTTP_URL}/healthz" 2>/dev/null)" != "000" ] && break
      sleep 1
    done
    if [ -z "${BEADS_TOKEN:-}" ]; then
      BEADS_TOKEN="$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
        -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)"
      export BEADS_TOKEN
    fi
  else
    skip_all "no BEADS_HTTP_URL and daemon Service '${BEADS_SVC}' absent in '${NAMESPACE}'"
  fi
fi
dim "  daemon: ${BEADS_HTTP_URL}"

# ── 2. Resolve the probe binary ────────────────────────────────────
# Prefer a binary pre-built onto PATH by a CI build step (the escalation-reap-
# probe / gitlab-prov-probe precedent, with per-source caching). Fall back to an
# in-shell build from source for local dev. Only when BOTH are unavailable do we
# skip clean.
if command -v agent-feedback-probe >/dev/null 2>&1; then
  PROBE_BIN="$(command -v agent-feedback-probe)"
  PATH_INSTALLED=1
  pass "using pre-built agent-feedback-probe ($PROBE_BIN)"
else
  PATH_INSTALLED=0
  command -v go >/dev/null 2>&1 \
    || skip_all "no pre-built agent-feedback-probe on PATH and no go toolchain to build it"
  [ -d "$CONTROLLER_DIR/cmd/agent-feedback-probe" ] \
    || skip_all "no pre-built agent-feedback-probe on PATH and cmd/agent-feedback-probe missing under $CONTROLLER_DIR"
  blue "[build] cmd/agent-feedback-probe (from source)"
  PROBE_BIN="$(mktemp "/tmp/agent-feedback-probe.${RUN_ID}.XXXXXX")"
  BUILD_LOG="/tmp/agent-feedback-probe-build-${RUN_ID}.log"
  if ( cd "$CONTROLLER_DIR" && CGO_ENABLED=0 go build -o "$PROBE_BIN" ./cmd/agent-feedback-probe/ ) >"$BUILD_LOG" 2>&1; then
    pass "cmd/agent-feedback-probe built from source"
  else
    fail "build agent-feedback-probe" "go build failed: $(tail -8 "$BUILD_LOG")"
    blue "════════════════════════════════════════════════════"
    red " ✗ 1/1 assertions failed (probe build)"
    for err in "${ERRORS[@]}"; do red "   - $err"; done
    blue "════════════════════════════════════════════════════"
    exit 1
  fi
fi
echo

# ── 3. Run the probe; map each leg line to an assertion ────────────
# The probe prints one "LEG <name>: PASS|FAIL <detail>" line per leg and exits
# non-zero if any leg failed. We require ALL seven expected legs to be present so
# a probe that silently drops a leg cannot pass. A "PROBE_SKIP:" line means the
# probe self-skipped clean (no live daemon) — mirror that with skip_all.
blue "[run] driving the REAL AgentFeedbackAPI handler against the LIVE daemon (7 legs)"
PROBE_OUT="/tmp/agent-feedback-probe-${RUN_ID}.out"
PROBE_ERR="/tmp/agent-feedback-probe-${RUN_ID}.err"
probe_rc=0
"$PROBE_BIN" >"$PROBE_OUT" 2>"$PROBE_ERR" || probe_rc=$?

if grep -q '^PROBE_SKIP:' "$PROBE_OUT" 2>/dev/null; then
  skip_all "probe self-skipped: $(grep '^PROBE_SKIP:' "$PROBE_OUT" | head -1 | sed 's/^PROBE_SKIP: //')"
fi

EXPECTED_LEGS="writes-stable-bead rates-edge-wired xss-escaped idempotent-upsert distinct-rater-splits rater-precedence validation-gates"
for leg in $EXPECTED_LEGS; do
  line=$(grep -E "^LEG ${leg}: " "$PROBE_OUT" 2>/dev/null | head -1 || true)
  if [ -z "$line" ]; then
    fail "leg $leg" "no result line emitted by the probe (leg dropped)"
    continue
  fi
  detail="${line#LEG "${leg}": }"
  case "$detail" in
    PASS*) pass "leg $leg — ${detail#PASS }" ;;
    FAIL*) fail "leg $leg" "${detail#FAIL }" ;;
    *)     fail "leg $leg" "unrecognized result: $detail" ;;
  esac
done

# Belt-and-suspenders: the probe's exit code must agree with our parse.
if [ "$probe_rc" -ne 0 ] && [ "$FAIL" -eq 0 ]; then
  fail "probe exit code" "probe exited $probe_rc but all parsed legs passed — probe-level error (stderr: $(tail -3 "$PROBE_ERR" 2>/dev/null))"
fi
echo

# ── Summary ─────────────────────────────────────────────────────────
blue "════════════════════════════════════════════════════"
total=$((PASS + FAIL))
if [ "$FAIL" -eq 0 ]; then
  green " ✓ All $total assertions passed"
else
  red " ✗ $FAIL/$total assertions failed"
  echo
  for err in "${ERRORS[@]}"; do red "   - $err"; done
  echo
  dim "  Probe output:"
  sed 's/^/    /' "$PROBE_OUT" 2>/dev/null || true
fi
blue "════════════════════════════════════════════════════"
[ "$FAIL" -eq 0 ] || exit 1
exit 0
