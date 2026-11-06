#!/usr/bin/env bash
# test-gitlab-dark-provenance.sh — E-PROV forward-provenance + host-scoped
# gitlab-dark bridge E2E (task ka-0000005448, E-PROV epic).
#
# Closes the gap the E-PROV landing left open (commits 9754ec0 / ac7b40d /
# 668fc9c / 0de8ee7, FW-1/2/3/6): the forward-provenance stack — host-scoped MR
# routing, the upstream webhook merge GATE, the scoped backstop poller, the
# reverse-provenance acceptance record, and the peel-backlink false-attach guard
# — shipped with strong UNIT coverage (gitlab_dark_hostscope_test.go,
# gitlab_upstream_poller_test.go, gitlab_upstream_acceptance_test.go,
# gitlab_mr_backlink_peelprov_test.go) but NO end-to-end assertion that wires the
# pieces together the way production does.
#
# ── Why a built-from-source probe, not a live-bridge port-forward ─────────────
#
# Two hard constraints shape this module — identical to test-jira-conversational:
#
#   1. Both gitlab-bridges (the prod one AND the second gitlab-dark-pointed one
#      the E-PROV topology adds) are OFF in the ephemeral e2e namespace
#      (see deploy-values), so there is no live /webhook to POST to and no
#      SSE-driven sync running — the same reason test-gitlab-bridge.sh's mention
#      leg skips.
#   2. The E-PROV logic under test is all in-process Go (GitLabSync host scope,
#      GitLabWebhookHandlerWithConfig's merge path, UpstreamAcceptanceReconciler,
#      GitLabUpstreamPoller, MRBacklinkReconciler). Driving it against the SHARED
#      daemon would require a live bridge; `kd serve` needs Postgres, so an
#      isolated daemon is not cheap to stand up in a shard.
#
# So this module follows the EXACT precedent of cmd/jira-conv-probe /
# cmd/trigger-probe: it builds and runs cmd/gitlab-prov-probe, a TEST-ONLY binary
# that wires the REAL bridge components through the REAL SSE stream + the REAL
# webhook handler, against STUB GitLab httptest servers (one per host) + an
# in-process mock daemon that RECORDS every provenance write. It is hermetic:
# ZERO pods, ZERO shared-daemon writes, ZERO sandbox rig — so it is green today
# and is a HARD gate.
#
# ── Legs (each a hard sub-gate; any FAIL reddens the shard) ───────────────────
#
#   1  hostscope       Two host-scoped GitLabSyncs (dark + upstream) on ONE SSE
#                      stream + ONE beads daemon each mint a produces edge +
#                      type=merge-request bead ONLY for their own host's mr_url,
#                      and NEVER GET/PUT the foreign host's MR (FW-1). [acceptance
#                      criterion: "Dark bridge routes MR events correctly scoped"]
#   2  webhook-gate    A real merge_request=merged webhook closes the upstream
#                      mirror bead (mr_outcome=merged) AND the acceptance
#                      reconciler stamps the peel-request (upstream_accepted_at /
#                      upstream_mr_url / upstream_merged_by) without closing the
#                      peel-request or the dark work bead (FW-2 + FW-3).
#                      [acceptance criterion: "Upstream MR provenance is tracked
#                      end-to-end"]
#   3  backstop-poll   The scoped GitLabUpstreamPoller lists ONLY upstream:true
#                      mirror beads, closes a merged one, skips a foreign-host
#                      mirror, and drives acceptance with an empty merged_by that
#                      never clobbers a webhook value (FW-2 backstop + FW-3).
#   4  peel-backlink   The MRBacklinkReconciler links a peel MR to its UPSTREAM
#                      peel-request bead and NEVER false-attaches to the DARK
#                      anchor in the §7 provenance block (FW-6).
#
# ── Skip-clean contract ──────────────────────────────────────────────────────
#
# The Go toolchain + cmd/gitlab-prov-probe source are present in every e2e shard
# container (the shard `use:` list includes build-gitlab-prov-probe, and go is
# installed). If either is somehow absent the module SKIPS CLEANLY (exit 0) with
# a LOUD banner — an absent toolchain must never red the suite — mirroring the
# jira-conversational / gvisor-probe skip_all("loud") contract. When present,
# every leg is a HARD assertion.
#
# Usage:
#   ./tests/e2e/scripts/test-gitlab-dark-provenance.sh

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
  red " SKIP gitlab-dark-provenance (LOUD): $why"
  red " This run did NOT assert the E-PROV forward-provenance stack (host-scoped"
  red " routing, upstream webhook gate, backstop poller, acceptance record, peel"
  red " backlink guard). A green suite here proves NOTHING about FW-1/2/3/6. The"
  red " e2e shard is built with a Go toolchain + the cmd/gitlab-prov-probe source"
  red " (build-gitlab-prov-probe task) — if this fires in CI, that wiring regressed."
  red "════════════════════════════════════════════════════"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

PATH_INSTALLED=0
cleanup() {
  # Only remove a temp binary we built; never a PATH-installed one.
  [ "$PATH_INSTALLED" != "1" ] && [ -n "$PROBE_BIN" ] && rm -f "$PROBE_BIN" 2>/dev/null || true
}
trap cleanup EXIT

echo "════════════════════════════════════════════════════"
echo " E-PROV forward-provenance + host-scoped dark bridge E2E (ka-0000005448)"
echo " hostscope -> webhook-gate -> backstop-poll -> peel-backlink"
echo "════════════════════════════════════════════════════"

# ── 1. Resolve the probe binary ─────────────────────────────────────
# Prefer a binary pre-built + smoke-checked
# onto PATH (the trigger-probe / jira-conv-probe precedent, with per-source
# caching). Fall back to an in-shell build from source for local dev and for
# robustness if the pre-built binary is absent. Only when BOTH the pre-built
# binary is absent AND no Go toolchain/source is available do we skip clean.
if command -v gitlab-prov-probe >/dev/null 2>&1; then
  PROBE_BIN="$(command -v gitlab-prov-probe)"
  # PROBE_BIN is on PATH, not a temp file — do not let cleanup() rm it.
  PATH_INSTALLED=1
  pass "using pre-built gitlab-prov-probe ($PROBE_BIN)"
else
  PATH_INSTALLED=0
  command -v go >/dev/null 2>&1 \
    || skip_all "no pre-built gitlab-prov-probe on PATH and no go toolchain to build it"
  [ -d "$CONTROLLER_DIR/cmd/gitlab-prov-probe" ] \
    || skip_all "no pre-built gitlab-prov-probe on PATH and cmd/gitlab-prov-probe missing under $CONTROLLER_DIR"
  blue "[build] cmd/gitlab-prov-probe (from source)"
  PROBE_BIN="$(mktemp "/tmp/gitlab-prov-probe.${RUN_ID}.XXXXXX")"
  BUILD_LOG="/tmp/gitlab-prov-probe-build-${RUN_ID}.log"
  if ( cd "$CONTROLLER_DIR" && CGO_ENABLED=0 go build -o "$PROBE_BIN" ./cmd/gitlab-prov-probe/ ) >"$BUILD_LOG" 2>&1; then
    pass "cmd/gitlab-prov-probe built from source"
  else
    fail "build gitlab-prov-probe" "go build failed: $(tail -8 "$BUILD_LOG")"
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
# non-zero if any leg failed. We tee its output, map each leg line to a pass/fail
# assertion, and require ALL four expected legs to be present (a missing leg is a
# failure, so a probe that silently drops a leg cannot pass).
blue "[run] driving hostscope -> webhook-gate -> backstop-poll -> peel-backlink"
PROBE_OUT="/tmp/gitlab-prov-probe-${RUN_ID}.out"
probe_rc=0
"$PROBE_BIN" >"$PROBE_OUT" 2>/tmp/gitlab-prov-probe-${RUN_ID}.err || probe_rc=$?

EXPECTED_LEGS="hostscope webhook-gate backstop-poll peel-backlink"
for leg in $EXPECTED_LEGS; do
  line=$(grep -E "^LEG ${leg}: " "$PROBE_OUT" 2>/dev/null | head -1 || true)
  if [ -z "$line" ]; then
    fail "leg $leg" "no result line emitted by the probe (leg dropped)"
    continue
  fi
  detail="${line#LEG ${leg}: }"
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
  fail "probe exit code" "probe exited $probe_rc but all parsed legs passed — probe-level error (stderr: $(tail -3 /tmp/gitlab-prov-probe-${RUN_ID}.err 2>/dev/null))"
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

[ "$FAIL" -eq 0 ]
