#!/usr/bin/env bash
# test-mr-reopen-drift.sh — E2E gate for the gitlab-bridge MR-reopen drift sweep
# (bead ka-0000006088, parent ka-0000000958).
#
# Guards the REOPEN leg of the MR/bead close-drift pair (bug ka-0000003953):
#   direction 1 (MR !6190, kbeads close.go) — `kd close` REFUSES to close a bead
#     whose linked MR is still open, unless --allow-open-mr is passed (which
#     stamps mr_close_override=true). Prevents NEW drift.
#   direction 2 (MR !6202, ka-0000000267, gitlab_mr_reopen_drift.go) — a reconcile
#     sweep RE-OPENS a bead that was ALREADY closed while its MR sits open/unmerged.
#     Repairs drift the close-time guard cannot reach, because once a bead is
#     closed nothing in the lane ever looks at it again — a real fix whose bead was
#     closed with its MR unmerged is one stale branch away from silent deletion
#     (the ~50min near-loss of MR !6183 that motivated the fix).
#
# THE GAP THIS CLOSES: test-mr-merge-flow.sh asserts only the FORWARD direction
# (an MR that closed without merging must not transition JIRA). Nothing in the
# e2e suite exercised the reopen sweep — a grep of test-mr-merge-flow.sh for
# reopen/re-open returns nothing. The risk is data-integrity and SILENT: if the
# sweep regresses, a bead closed prematurely while its MR is still in review just
# stays closed, and the fleet loses track of unfinished work with NO signal.
#
# ── Why a built-from-source probe, not a live-bridge observation ─────────────
#
# Two hard constraints, the same pair that shape test-gitlab-dark-provenance.sh:
#
#   1. gitlabBridge.enabled is FALSE in the ephemeral e2e namespace — there
#      is no sweep pod to observe, the same reason test-gitlab-bridge.sh's mention leg skips.
#   2. Even against a live bridge the sweep is driven by gitlab.com's real
#      open-MR list on a 15m interval. A test cannot make a real MR stay
#      `opened` on demand, and 15m exceeds the per-shard wall-clock budget.
#
# So this module follows the cmd/gitlab-prov-probe / cmd/jira-conv-probe
# precedent: cmd/mr-reopen-probe builds the REAL MRReopenDriftReconciler over the
# REAL beadsapi client pointed at the shard's LIVE beads daemon, plus the REAL
# bridge.GitLabClient pointed at an httptest stub standing in for gitlab.com.
# Only the GitLab wire responses are synthetic — the sweep logic, the daemon
# queries, and every bead write are production code paths.
#
# ── What this covers that the unit tests CANNOT ─────────────────────────────
#
# gitlab_mr_reopen_drift_test.go (10 tests) drives the sweep against a hand-rolled
# fakeReopenDaemon that REIMPLEMENTS the status+field filter in Go. That fake is
# itself the assumption under test here: the sweep asks the daemon for
# `status=closed` + `field=mr_url=<url>` and trusts it to return exactly the
# owning issue bead(s). If the real daemon's field filter were loose
# (prefix/substring), missing, or scoped so it dropped cross-project beads, every
# unit test would still pass while the production sweep re-opened the WRONG bead
# or none at all. This module pins that cross-component seam against the real
# daemon — including the real type=merge-request bead that carries the SAME
# mr_url (on live data a single query returns BOTH, so the sweep's type guard is
# load-bearing, not theoretical; the mr-bead-guard leg asserts that premise
# explicitly before relying on it).
#
# ── Legs (each a hard sub-gate; any FAIL reddens the shard) ─────────────────
#
#   1  reopen         A closed issue bead whose MR is still `opened` IS re-opened,
#                     with the audit comment attached. The headline repair, and
#                     the exact leg the bead calls out as missing.
#   2  merged-stays-closed  The negative the bead also asks for: a closed bead
#                     whose MR MERGED stays closed. Guards the widening
#                     regression (state=opened → state=all) that would re-open
#                     every correctly-closed bead in the fleet.
#   3  override-respected   A bead closed deliberately via `kd close
#                     --allow-open-mr` (mr_close_override=true) stays closed.
#                     Without this, abandoning an MR is impossible — the operator
#                     closes the bead and the sweep re-opens it every 15m.
#   4  mr-bead-guard  The canonical type=merge-request bead sharing the mr_url is
#                     NEVER re-opened, while the issue bead it tracks IS.
#   5  reverify-race  An MR listed as `opened` but `merged` on the per-MR
#                     re-verify does NOT get re-opened — the stale-snapshot guard.
#
# Mutation-tested (2026-08-05): removing the override guard, the
# type=merge-request guard, or the re-verify state check each reds exactly the
# corresponding leg; widening the listing to state=all, or regressing the repair
# to a no-op, reds legs 1+4. No mutant survived.
#
# ── Skip-clean contract ────────────────────────────────────────────────────
#
# The Go toolchain + cmd/mr-reopen-probe source are present in every e2e shard
# container (shard `use:` list includes build-mr-reopen-probe). If either is
# somehow absent the module SKIPS CLEANLY (exit 0) with a LOUD banner — an absent
# toolchain must never red the suite — mirroring the gitlab-dark-provenance /
# jira-conversational skip_all("loud") contract. A daemon that is unreachable is
# likewise a LOUD skip, not a failure: this module asserts the sweep, not daemon
# availability (test-stack-health.sh owns that). When present, every leg is a
# HARD assertion.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-mr-reopen-drift.sh

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
have() { command -v "$1" >/dev/null 2>&1; }

cleanup() {
  # Only remove a temp binary we built; never a PATH-installed one.
  if [ "$PATH_INSTALLED" != "1" ] && [ -n "$PROBE_BIN" ]; then
    rm -f "$PROBE_BIN" 2>/dev/null || true
  fi
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# skip_all exits 0 (a missing toolchain/daemon must NOT red the suite) with a
# LOUD banner so a skip is never silently read as a pass.
skip_all() {
  local why="$1"
  red "════════════════════════════════════════════════════"
  red " SKIP mr-reopen-drift (LOUD): $why"
  red " This run did NOT assert the MR-reopen drift sweep (reopen of a closed"
  red " bead whose MR is still open, the merged/override/MR-bead/re-verify"
  red " negatives). A green suite here proves NOTHING about ka-0000000267."
  red " The e2e shard is built with a Go toolchain + the cmd/mr-reopen-probe"
  red " source (build-mr-reopen-probe task) — if this fires in CI, that wiring"
  red " regressed."
  red "════════════════════════════════════════════════════"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

echo "════════════════════════════════════════════════════"
echo " gitlab-bridge MR-reopen drift sweep E2E (ka-0000006088)"
echo " a bead closed while its MR is still open gets re-opened; merged/deliberate stay closed"
echo "════════════════════════════════════════════════════"

# ── 1. Resolve the probe binary ─────────────────────────────────────
# Prefer a binary pre-built onto PATH (the
# gitlab-prov-probe / jira-conv-probe precedent, with per-source caching). Fall
# back to an in-shell build for local dev. Only when BOTH are unavailable do we
# skip clean.
if have mr-reopen-probe; then
  PROBE_BIN="$(command -v mr-reopen-probe)"
  PATH_INSTALLED=1
  pass "using pre-built mr-reopen-probe ($PROBE_BIN)"
else
  have go || skip_all "no pre-built mr-reopen-probe on PATH and no go toolchain to build it"
  [ -d "$CONTROLLER_DIR/cmd/mr-reopen-probe" ] \
    || skip_all "no pre-built mr-reopen-probe on PATH and cmd/mr-reopen-probe missing under $CONTROLLER_DIR"
  blue "[build] cmd/mr-reopen-probe (from source)"
  PROBE_BIN="$(mktemp "/tmp/mr-reopen-probe.${RUN_ID}.XXXXXX")"
  BUILD_LOG="/tmp/mr-reopen-probe-build-${RUN_ID}.log"
  if ( cd "$CONTROLLER_DIR" && CGO_ENABLED=0 go build -o "$PROBE_BIN" ./cmd/mr-reopen-probe/ ) >"$BUILD_LOG" 2>&1; then
    pass "cmd/mr-reopen-probe built from source"
  else
    # A build failure IS a hard failure — the probe is the whole module.
    fail "build mr-reopen-probe" "go build failed: $(tail -8 "$BUILD_LOG")"
    blue "════════════════════════════════════════════════════"
    red " ✗ 1/1 assertions failed (probe build)"
    for err in "${ERRORS[@]}"; do red "   - $err"; done
    blue "════════════════════════════════════════════════════"
    exit 1
  fi
fi
echo

# ── 2. Resolve the live daemon ──────────────────────────────────────
# run-shard.sh exports BEADS_HTTP_URL + BEADS_TOKEN. Standalone, stand up our own
# port-forward so the module is runnable outside a shard.
if [ -z "${BEADS_HTTP_URL:-}" ]; then
  have kubectl || skip_all "no BEADS_HTTP_URL and kubectl not installed — cannot reach a beads daemon"
  blue "[daemon] port-forwarding ${NAMESPACE}/${BEADS_SVC}..."
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" 19112:8080 \
    >"/tmp/kd-pf-mr-reopen-${RUN_ID}.log" 2>&1 &
  PF_PID=$!
  export BEADS_HTTP_URL="http://localhost:19112"
  pf_ready=0
  for _ in $(seq 1 15); do
    if curl -sf --max-time 2 -o /dev/null "${BEADS_HTTP_URL}/v1/health" 2>/dev/null; then
      pf_ready=1; break
    fi
    sleep 1
  done
  [ "$pf_ready" = 1 ] || skip_all "daemon /v1/health never answered on ${BEADS_HTTP_URL}"
  if [ -z "${BEADS_TOKEN:-}" ] && have kubectl; then
    BEADS_TOKEN=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
      -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
    [ -n "$BEADS_TOKEN" ] && export BEADS_TOKEN
  fi
fi
dim "  daemon: ${BEADS_HTTP_URL}"
echo

# ── 3. Run the probe; map each leg to an assertion ──────────────────
# The probe prints one "LEG <name>: PASS|FAIL <detail>" line per leg and exits
# non-zero if any leg failed. We require ALL five expected legs to be present, so
# a probe that silently drops a leg cannot pass.
blue "[run] reopen -> merged-stays-closed -> override-respected -> mr-bead-guard -> reverify-race"
PROBE_OUT="/tmp/mr-reopen-probe-${RUN_ID}.out"
PROBE_ERR="/tmp/mr-reopen-probe-${RUN_ID}.err"
probe_rc=0
"$PROBE_BIN" >"$PROBE_OUT" 2>"$PROBE_ERR" || probe_rc=$?

# An unreachable daemon is an environment gap, not a sweep regression: skip LOUD
# rather than reddening the suite (test-stack-health.sh owns daemon liveness).
if grep -q "daemon unreachable" "$PROBE_ERR" 2>/dev/null; then
  skip_all "beads daemon unreachable at ${BEADS_HTTP_URL} ($(head -1 "$PROBE_ERR"))"
fi

EXPECTED_LEGS="reopen merged-stays-closed override-respected mr-bead-guard reverify-race"
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

# Belt-and-suspenders: the probe's exit code must agree with our parse. A
# non-zero exit with all legs parsed PASS means the probe failed somewhere
# outside a leg (setup, cleanup, panic) and we must not report green.
if [ "$probe_rc" -ne 0 ] && [ "$FAIL" -eq 0 ]; then
  fail "probe exit code" "probe exited $probe_rc but every leg line parsed PASS: $(tail -3 "$PROBE_ERR" 2>/dev/null)"
fi
echo

# ── Summary ───────────────────────────────────────────────────────
echo "════════════════════════════════════════════════════"
echo " mr-reopen-drift Summary: $PASS passed, $FAIL failed"
echo "════════════════════════════════════════════════════"
if [ "$FAIL" -gt 0 ]; then
  for e in "${ERRORS[@]}"; do red "  - $e"; done
  dim "  probe stdout: $PROBE_OUT"
  dim "  probe stderr: $PROBE_ERR"
  exit 1
fi
green "All mr-reopen-drift assertions passed."
exit 0
