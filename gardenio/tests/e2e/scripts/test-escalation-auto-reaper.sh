#!/usr/bin/env bash
# test-escalation-auto-reaper.sh — E2E gate for the escalation AUTO-REAPER:
# the sweep that auto-closes an escalation whose ask a human already satisfied
# OUT-OF-BAND (the MR got merged / the approval landed / the operator did it by
# hand), plus its two companion detectors.
#
# Bead ka-0000006112 (parent ka-0000000958). Guards three merged changes:
#   011b9e33b (ka-0000004105) — the MR-witness reaper. An approve/merge ask whose
#     MR an operator merged in the GitLab UI emits no reverse event, so the
#     escalation sat open at P0 forever. Age-ordering sorts those dead entries to
#     the TOP of the operator queue (~15% phantom rate, measured 2026-07-26),
#     training the operator to distrust the whole queue.
#   00b86ed3c (ka-0000007203) — the ORCHESTRATION-layer witness. Reaps on a
#     signal the operator left on the BEAD (a terminal result_status on a
#     still-open escalation, or a human-operator resolve-comment), so it covers
#     the apply/deploy/mint asks no MR state can attest to.
#   ccfc8411a (ka-0000002563) — the MR-landed REVIEW FLAG, the complement: a
#     NON-reapable merge-then-apply ask whose MR has landed is flipped to
#     needs-review — it is NEVER closed, because the apply step may still stand.
#
# THE GAP THIS CLOSES: the five escalation modules (test-escalation-crud /
# -approval / -execution / -v2 / -slack-notify) exercise the create / approve /
# execute / resolve verbs and never once assert that an out-of-band-satisfied
# escalation gets auto-closed. A grep of gardenio/tests for reap|mr_landed|
# needs-review returns ONLY the unrelated agent-pod stale-reaper. So a reaper
# regression is invisible to the suite in either direction, and both directions
# hurt: under-reaping leaves satisfied escalations re-nudging the operator, while
# OVER-reaping silently closes a live P0.
#
# ── Why a built-from-source probe, not a leg on test-escalation-execution.sh ──
#
# The bead proposed adding a reaper leg to test-escalation-execution.sh ("create
# an escalation, satisfy it out-of-band, run the reaper, assert auto-closed").
# That is structurally impossible here, for three INDEPENDENT reasons — each one
# sufficient on its own:
#
#   1. The reaper runs inside gitlab-bridge, and gitlab-bridge is NOT DEPLOYED in
#      the ephemeral e2e namespace (`gitlabBridge.enabled: false`; every
#      template is gated on it). There is no
#      process running the sweep to observe — the same constraint that makes
#      test-gitlab-dark-provenance.sh and test-jira-conversational.sh probes.
#   2. The reaper HARD-SKIPS e2e fixtures. evaluateBead's first guard is
#      isE2EEscalation(createdBy, labels), which matches any createdBy with an
#      "e2e-" prefix — and every escalation the suite creates is authored by
#      KD_ACTOR, which defaults to "e2e-test". So even with the bridge running, a
#      suite-created escalation could never be reaped, BY DESIGN. (That guard is
#      itself asserted, as leg 5, with a non-fixture positive control.)
#   3. gitlab-bridge exposes only /healthz, /readyz, /webhook and a separate
#      :9092 /metrics — no admin or sweep-trigger endpoint — and the sweep
#      interval is 10 minutes, far past any shard's wall-clock budget.
#
# So this follows the EXACT precedent of cmd/gitlab-prov-probe: it builds and
# runs cmd/escalation-reap-probe, a TEST-ONLY binary that drives the REAL
# EscalationReaper (the exported NewEscalationReaper + Run, i.e. the same code
# path gitlab-bridge runs in prod) against a STUB GitLab httptest server and an
# in-process mock beads daemon that RECORDS every write. Because Run() sweeps
# immediately and then blocks on its ticker, the probe gets exactly one
# observable sweep per invocation.
#
# This drives PRODUCTION CODE, not a bash reimplementation of it: a refactor that
# drops the verb allowlist, the human-operator gate, the host scope, or the review
# flag's never-close rule reds this module. It is hermetic — ZERO pods, ZERO
# shared-daemon writes, no network beyond loopback — so it is a HARD gate in every
# shard rather than an opt-in.
#
# ── Legs (each a hard sub-gate; any FAIL reddens the shard) ───────────────────
#
#   1 merge-verb-reap          A merged MR auto-closes an approve_and_merge_mr ask
#                              with a full audit trail (reaped/reaped_signal/
#                              approved_by=the merger, so the Slack card renders
#                              Complete not Rejected); a DECLINED MR is reaped as
#                              moot with approved_by=out-of-band; a still-`opened`
#                              MR is left alone.
#   2 apply-verb-never-reaped  THE load-bearing guard: an apply / no-action
#                              (the real ka-0000001987 shape) / mint ask is NEVER
#                              reaped even though its MR IS merged. The VERB is
#                              the discriminator, not the MR state — false-reaping
#                              here silently drops a live P0.
#   3 approve-only-approvals   approve_only_do_not_merge reads the APPROVALS
#                              endpoint, never MR state (ka-0000000844 requires
#                              its MR to stay draft+unmerged); an unapproved MR
#                              and a Community-Edition 404 both stay open.
#   4 orchestration-witness    result_status=success reaps and =failure does NOT
#                              (a failed ask is unfinished work); a HUMAN
#                              operator's resolve directive reaps, while the SAME
#                              directive from an AGENT does not; zero GitLab calls.
#   5 e2e-fixture-skip         Fixtures (e2e- actor prefix, e2e:true label) are
#                              skipped even with a merged MR + terminal
#                              result_status — with a non-fixture control proving
#                              the sweep actually ran.
#   6 review-flag-never-closes A landed merge-then-apply ask flips to needs-review
#                              (mr_landed + exactly ONE pingback + label) and is
#                              NEVER closed; the latch is one-shot across sweeps.
#   7 idempotent-sweep         A reaped bead leaves the open-set: no double-close,
#                              no double-comment, no repeat GitLab GET.
#   8 host-scope-and-dryrun    A foreign-host ref is skipped before any API call
#                              (dual-GitLab safety) while a hostless structured
#                              ref still reaps; dry-run CHECKS but closes,
#                              comments, flags and labels nothing.
#
# ── Skip-clean contract ──────────────────────────────────────────────────────
#
# The Go toolchain + cmd/escalation-reap-probe source are present in every e2e
# shard container. If either is somehow absent the module SKIPS CLEANLY (exit 0)
# with a LOUD banner — an absent toolchain must never red the suite — mirroring
# the gitlab-dark-provenance / jira-conversational skip_all("loud") contract.
# When present, every leg is a HARD assertion.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-escalation-auto-reaper.sh

set -uo pipefail

# ── Config ────────────────────────────────────────────────────────
CONTROLLER_DIR="${CONTROLLER_DIR:-gardenio/controller}"
RUN_ID="${RUN_ID:-$$}"
PROBE_BIN=""

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

# skip_all exits 0 (a missing toolchain must NOT red the suite) with a LOUD
# banner so a skip is never silently read as a pass.
skip_all() {
  local why="$1"
  red "════════════════════════════════════════════════════"
  red " SKIP escalation-auto-reaper (LOUD): $why"
  red " This run did NOT assert the escalation auto-reaper (MR-witness reap, the"
  red " apply-verb never-reap guard, approve-only approvals, the orchestration"
  red " witness, e2e-fixture skip, the never-close review flag, sweep idempotency,"
  red " or host-scope/dry-run). A green suite here proves NOTHING about"
  red " ka-0000004105 / ka-0000007203 / ka-0000002563. The e2e shard is built with"
  red " a Go toolchain + the cmd/escalation-reap-probe source — if this fires in"
  red " CI, that wiring regressed."
  red "════════════════════════════════════════════════════"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

PATH_INSTALLED=0
cleanup() {
  # Only remove a temp binary we built; never a PATH-installed one.
  [ "$PATH_INSTALLED" != "1" ] && [ -n "$PROBE_BIN" ] && rm -f "$PROBE_BIN" 2>/dev/null || true
  rm -f "/tmp/escalation-reap-probe-${RUN_ID}.out" \
        "/tmp/escalation-reap-probe-${RUN_ID}.err" \
        "/tmp/escalation-reap-probe-build-${RUN_ID}.log" 2>/dev/null || true
}
trap cleanup EXIT

echo "════════════════════════════════════════════════════"
echo " escalation auto-reaper E2E (ka-0000006112)"
echo " satisfied out-of-band ⇒ auto-closed; apply asks NEVER reaped"
echo "════════════════════════════════════════════════════"

# ── 1. Resolve the probe binary ─────────────────────────────────────
# Prefer a binary pre-built onto PATH by a CI build step (the
# gitlab-prov-probe / jira-conv-probe precedent, with per-source caching). Fall
# back to an in-shell build from source for local dev and for robustness if the
# pre-built binary is absent. Only when BOTH are unavailable do we skip clean.
if command -v escalation-reap-probe >/dev/null 2>&1; then
  PROBE_BIN="$(command -v escalation-reap-probe)"
  # PROBE_BIN is on PATH, not a temp file — do not let cleanup() rm it.
  PATH_INSTALLED=1
  pass "using pre-built escalation-reap-probe ($PROBE_BIN)"
else
  PATH_INSTALLED=0
  command -v go >/dev/null 2>&1 \
    || skip_all "no pre-built escalation-reap-probe on PATH and no go toolchain to build it"
  [ -d "$CONTROLLER_DIR/cmd/escalation-reap-probe" ] \
    || skip_all "no pre-built escalation-reap-probe on PATH and cmd/escalation-reap-probe missing under $CONTROLLER_DIR"
  blue "[build] cmd/escalation-reap-probe (from source)"
  PROBE_BIN="$(mktemp "/tmp/escalation-reap-probe.${RUN_ID}.XXXXXX")"
  BUILD_LOG="/tmp/escalation-reap-probe-build-${RUN_ID}.log"
  if ( cd "$CONTROLLER_DIR" && CGO_ENABLED=0 go build -o "$PROBE_BIN" ./cmd/escalation-reap-probe/ ) >"$BUILD_LOG" 2>&1; then
    pass "cmd/escalation-reap-probe built from source"
  else
    fail "build escalation-reap-probe" "go build failed: $(tail -8 "$BUILD_LOG")"
    # A build failure IS a hard failure — the probe is the whole module.
    blue "════════════════════════════════════════════════════"
    red " ✗ 1/1 assertions failed (probe build)"
    for err in "${ERRORS[@]}"; do red "   - $err"; done
    blue "════════════════════════════════════════════════════"
    exit 1
  fi
fi
echo

# ── 2. Run the probe; capture per-leg lines ─────────────────────────
# The probe prints one "LEG <name>: PASS|FAIL <detail>" line per leg and exits
# non-zero if any leg failed. We map each leg line to a pass/fail assertion and
# require ALL eight expected legs to be present, so a probe that silently drops a
# leg cannot pass.
blue "[run] driving the REAL EscalationReaper over 8 legs"
PROBE_OUT="/tmp/escalation-reap-probe-${RUN_ID}.out"
PROBE_ERR="/tmp/escalation-reap-probe-${RUN_ID}.err"
probe_rc=0
"$PROBE_BIN" >"$PROBE_OUT" 2>"$PROBE_ERR" || probe_rc=$?

EXPECTED_LEGS="merge-verb-reap apply-verb-never-reaped approve-only-approvals orchestration-witness e2e-fixture-skip review-flag-never-closes idempotent-sweep host-scope-and-dryrun"
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

# Belt-and-suspenders: the probe's own exit code must agree with our parse. If
# the probe exited non-zero but every leg line parsed PASS (should be
# impossible), treat it as a failure so a probe-level error is never masked.
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
