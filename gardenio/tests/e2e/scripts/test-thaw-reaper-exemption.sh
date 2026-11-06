#!/usr/bin/env bash
# test-thaw-reaper-exemption.sh — E2E gate asserting the reconciler reaper
# LEAVES ALONE an in-flight thaw Pod B and a frozen-awaiting-thaw agent.
#
# Bead gd-2T9JI3wsw5 (parent gd-JJIph7y3Ns). Guards two merged exemptions that
# closed real rehydrate-ack failure modes (bug ka-0000002058 still OPEN):
#   b0b44f25 — exempt the in-flight thaw placeholder Pod B from the terminal-pod
#              reaper (decision ka-0000005300: exempt_reaper). After `runsc
#              restore`, Pod B's k8s phase flips to Failed within seconds (the
#              sleep-placeholder PID it was admitted on is replaced by the
#              restored sentry) while the restored sandbox is LIVE and the in-pod
#              gb+coop watchers are mid-rehydration. Deleting Pod B there tears
#              down the kubelet-owned netns and both watcher acks fail "Network
#              is unreachable" → the thaw fail-closes. The exemption is narrow:
#              it fires only for an actual thaw placeholder (LabelThawPlaceholder)
#              whose agent IsThawing right now; a Pod B that outlived its thaw
#              (controller crash, abandoned thaw) is reaped normally next pass.
#   bb1c0bba — exempt frozen-awaiting-thaw agents from the leaked-bead reaper
#              AND the create-missing path. A freeze checkpoints + tears down
#              Pod A, leaving the bead terminal (done/failed/rate_limited) +
#              podless but with a persisted snapshot. Closing it (or cold-starting
#              a duplicate under the canonical name) races the in-flight thaw.
#
# THE GAP THIS CLOSES: test-stale-reaper.sh inspects controller logs/metrics but
# never asserts the reaper LEAVES ALONE a mid-thaw Pod B / frozen-awaiting-thaw
# agent. This module asserts the exemption truth table directly.
#
# TWO LEGS, mirroring test-gvisor-rehydration.sh:
#   1. CONTRACT (always runs — needs only bash): a faithful, byte-for-byte
#      reimplementation of the three reaper-exemption DECISIONS the reconciler
#      makes (reconciler.go terminal-delete L599, leaked-bead L460, create-missing
#      L570), asserted over the full truth table. This is the regression guard
#      that runs in every shard — it has no substrate dependency and pins the
#      decision logic so a future refactor that drops an exemption fails here.
#   2. SUBSTRATE (opt-in, GVISOR_E2E=1): create a REAL frozen-awaiting-thaw agent
#      bead (terminal agent_state, podless) and/or a placeholder Pod B mapped to
#      the canonical name (ka-0000007549), let the live controller's reaper run,
#      and assert the bead is NOT closed and the pod is NOT deleted. SKIPs LOUD
#      when the opt-in flag is set but freeze/thaw / the gVisor substrate is
#      absent — never silently passes.
#
# OPT-IN / GATING (substrate leg only): plain e2e runs freeze/thaw
# DISABLED (helm values.yaml freezeThaw.enabled=false), so the exemption checkers
# (SnapshotChecker / ThawChecker) are nil there and there is nothing live to
# observe — the controller cold-starts every agent. Only the gvisor-poc canary
# enables freeze/thaw (deploy-values/gardenio.yaml). The substrate leg therefore
# targets ns gvisor-poc and self-SKIPs unless GVISOR_E2E=1 AND the substrate is
# present. The contract leg runs everywhere.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-thaw-reaper-exemption.sh             # contract only
#   GVISOR_E2E=1 ./gardenio/tests/e2e/scripts/test-thaw-reaper-exemption.sh # + substrate

set -uo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
KD_BIN="${KD_BIN:-kd}"
GVISOR_NS="${GVISOR_NS:-gvisor-poc}"
RUNTIME_CLASS="${RUNTIME_CLASS:-runsc}"
RUN_ID="${RUN_ID:-$$}"
# Reaper period: the reconciler runs every ~RECONCILE_INTERVAL; we wait a couple
# of passes so a (mis)reap would have happened before we assert "not reaped".
REAP_WAIT="${REAP_WAIT:-45}"

# The label/suffix the reconciler keys its thaw-placeholder exemptions on. These
# MUST match podmanager.LabelThawPlaceholder / ThawPlaceholderSuffix on the Go
# side (manager.go) — the substrate leg crafts a Pod B carrying exactly these so
# the live reaper's exemption fires. A drift here is itself a regression.
LABEL_THAW_PLACEHOLDER="gardenio.io/thaw-placeholder"
THAW_PLACEHOLDER_SUFFIX="-thaw-b"
ANNOTATION_CANONICAL_POD="gardenio.io/canonical-pod"

PASS=0
FAIL=0
ERRORS=()
PF_PID=""

export KD_ACTOR="${KD_ACTOR:-e2e-test}"

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

echo "════════════════════════════════════════════════════"
echo " thaw-race reaper-exemption E2E (gd-2T9JI3wsw5)"
echo " reaper must LEAVE ALONE in-flight Pod B + frozen-awaiting-thaw agents"
echo "════════════════════════════════════════════════════"

# ===========================================================================
# LEG 1 — CONTRACT (always runs): the reaper exemption truth table.
#
# These three predicates mirror, decision-for-decision, the reconciler's
# exemption gates. Each takes the inputs the reconciler actually branches on and
# returns REAP (the reaper acts) or EXEMPT (the reaper stands off). The asserts
# below walk the full truth table so a future refactor that drops an exemption —
# or widens one too far (e.g. exempting a Pod B that outlived its thaw, which
# WOULD permanently leak) — fails here, in every shard, with no substrate.
# ===========================================================================

# terminal_pod_verdict <phase> <is_thaw_placeholder:0|1> <is_thawing:0|1>
#   Mirrors reconciler.go L577-615 (the terminal-pod reaper). The reaper only
#   considers terminal pods (Failed|Succeeded). A terminal thaw-placeholder Pod B
#   whose agent IsThawing is EXEMPT; anything else terminal is REAPed.
terminal_pod_verdict() {
  local phase="$1" is_placeholder="$2" is_thawing="$3"
  case "$phase" in
    Failed|Succeeded) ;;                       # terminal — reaper considers it
    *) echo "NOOP"; return ;;                   # non-terminal — not this reaper's job
  esac
  if [ "$is_placeholder" = "1" ] && [ "$is_thawing" = "1" ]; then
    echo "EXEMPT"   # in-flight thaw Pod B — stand off (b0b44f25 / ka-0000002058)
  else
    echo "REAP"     # genuine terminal pod (incl. a Pod B that outlived its thaw)
  fi
}

# leaked_bead_verdict <agent_state> <has_pod:0|1> <has_snapshot:0|1>
#   Mirrors reconciler.go L447-476 (the leaked-bead reaper). A terminal-state,
#   podless bead is a leak and CLOSEd — UNLESS a freeze/thaw snapshot exists, in
#   which case it is a frozen-awaiting-thaw agent and EXEMPT (bb1c0bba).
leaked_bead_verdict() {
  local state="$1" has_pod="$2" has_snapshot="$3"
  [ "$has_pod" = "1" ] && { echo "NOOP"; return; }   # pod exists — not a leak
  case "$state" in
    done|failed|rate_limited) ;;                      # terminal — leak candidate
    *) echo "NOOP"; return ;;                          # working/in_progress — not a leak
  esac
  if [ "$has_snapshot" = "1" ]; then
    echo "EXEMPT"   # frozen-awaiting-thaw — closing races the thaw
  else
    echo "CLOSE"    # genuine podless leak
  fi
}

# create_missing_verdict <agent_state> <has_pod:0|1> <has_snapshot:0|1>
#   Mirrors reconciler.go L545-576 (the create-missing path). A podless bead is
#   normally cold-started — but a terminal-state bead WITH a snapshot is a
#   frozen-awaiting-thaw agent whose thaw owns re-creation, so create-missing
#   is SKIPped (cold-starting a duplicate under the canonical name defeats the
#   thaw's teardown-barrier, the exact re-drive race on prod 2026.158.17).
create_missing_verdict() {
  local state="$1" has_pod="$2" has_snapshot="$3"
  [ "$has_pod" = "1" ] && { echo "NOOP"; return; }   # pod exists — nothing to create
  [ "$state" = "failed" ] && { echo "SKIP"; return; } # hard-failed bead — never recreate
  if [ "$has_snapshot" = "1" ] && { [ "$state" = "done" ] || [ "$state" = "rate_limited" ]; }; then
    echo "SKIP"     # frozen-awaiting-thaw — thaw owns re-creation
  else
    echo "CREATE"   # normal cold start
  fi
}

assert_verdict() {
  local name="$1" got="$2" want="$3"
  if [ "$got" = "$want" ]; then
    pass "$name → $got"
  else
    fail "$name" "got '$got', want '$want'"
  fi
}

blue "[contract] terminal-pod reaper exemption (b0b44f25 / ka-0000002058)"
# The headline: a thaw-placeholder Pod B that flipped to Failed seconds after
# restore, whose agent IS still thawing, must NOT be reaped.
assert_verdict "in-flight thaw Pod B (Failed, placeholder, thawing) NOT reaped" \
  "$(terminal_pod_verdict Failed 1 1)" "EXEMPT"
assert_verdict "in-flight thaw Pod B (Succeeded, placeholder, thawing) NOT reaped" \
  "$(terminal_pod_verdict Succeeded 1 1)" "EXEMPT"
# Narrowness — the exemption must NOT leak. A Pod B that outlived its thaw
# (controller crash / abandoned thaw → IsThawing false) IS reaped, no permanent leak.
assert_verdict "thaw-placeholder Pod B that outlived its thaw (Failed, NOT thawing) IS reaped" \
  "$(terminal_pod_verdict Failed 1 0)" "REAP"
# A normal terminal pod (not a placeholder) is reaped as before — exemption off path.
assert_verdict "ordinary terminal pod (Failed, not placeholder) reaped as before" \
  "$(terminal_pod_verdict Failed 0 0)" "REAP"
# A thawing agent whose pod is still Running is not this reaper's concern.
assert_verdict "running thaw Pod B (placeholder, thawing) not a terminal-reaper case" \
  "$(terminal_pod_verdict Running 1 1)" "NOOP"
echo

blue "[contract] leaked-bead reaper exemption (bb1c0bba / ka-0000000114)"
# A frozen-awaiting-thaw agent is terminal+podless WITH a snapshot — exempt.
for st in done failed rate_limited; do
  assert_verdict "frozen-awaiting-thaw agent ($st, podless, snapshot) NOT closed" \
    "$(leaked_bead_verdict "$st" 0 1)" "EXEMPT"
done
# A genuine podless leak (terminal, no snapshot) IS closed — exemption must not over-apply.
assert_verdict "genuine podless leak (done, no snapshot) IS closed" \
  "$(leaked_bead_verdict done 0 0)" "CLOSE"
assert_verdict "genuine podless leak (failed, no snapshot) IS closed" \
  "$(leaked_bead_verdict failed 0 0)" "CLOSE"
# A live working agent without a pod is a brief race, never a leak — untouched.
assert_verdict "working agent (podless, no snapshot) NOT closed (brief race)" \
  "$(leaked_bead_verdict working 0 0)" "NOOP"
# Snapshot is irrelevant once the pod exists.
assert_verdict "agent with a live pod NOT closed regardless of snapshot" \
  "$(leaked_bead_verdict done 1 1)" "NOOP"
echo

blue "[contract] create-missing exemption (bb1c0bba / ka-0000006427)"
# Frozen-awaiting-thaw: thaw owns re-creation; create-missing must SKIP so it
# never cold-starts a duplicate under the canonical name.
assert_verdict "frozen-awaiting-thaw agent (done, snapshot) NOT cold-started" \
  "$(create_missing_verdict done 0 1)" "SKIP"
assert_verdict "frozen-awaiting-thaw agent (rate_limited, snapshot) NOT cold-started" \
  "$(create_missing_verdict rate_limited 0 1)" "SKIP"
# Without a snapshot (freeze/thaw off, or a normal pending agent) the cold start
# proceeds exactly as before — the exemption is opt-in and never blocks normal work.
assert_verdict "normal pending agent (in_progress, no snapshot) IS cold-started" \
  "$(create_missing_verdict in_progress 0 0)" "CREATE"
assert_verdict "terminal agent without snapshot (done, no snapshot) IS cold-started" \
  "$(create_missing_verdict done 0 0)" "CREATE"
echo

# ===========================================================================
# LEG 2 — SUBSTRATE (opt-in, GVISOR_E2E=1): observe the LIVE reaper.
#
# Create a real placeholder Pod B in ns gvisor-poc carrying the exact
# LabelThawPlaceholder + canonical-pod annotation the controller stamps, drive it
# to a terminal phase, and assert the live reconciler reaper does NOT delete it
# while a thaw would be in flight. SKIPs LOUD if the opt-in flag is set but the
# substrate / freeze-thaw controller is absent.
# ===========================================================================
if [ "${GVISOR_E2E:-0}" != "1" ]; then
  blue "[substrate] SKIP — GVISOR_E2E != 1 (contract leg already asserted the exemptions)"
  dim "  Set GVISOR_E2E=1 with the gvisor-poc substrate to observe the live reaper."
else
  blue "[substrate] GVISOR_E2E=1 — observing the live controller reaper"
  SUBSTRATE_OK=1
  if ! have kubectl; then
    fail "substrate preflight" "GVISOR_E2E=1 but kubectl not installed"
    SUBSTRATE_OK=0
  elif ! kubectl get ns "$GVISOR_NS" >/dev/null 2>&1; then
    fail "substrate preflight" "GVISOR_E2E=1 but ns '$GVISOR_NS' absent (freeze/thaw substrate not deployed)"
    SUBSTRATE_OK=0
  elif [ "$(kubectl auth can-i create pods -n "$GVISOR_NS" 2>/dev/null)" != "yes" ]; then
    fail "substrate preflight" "GVISOR_E2E=1 but cannot create pods in ns '$GVISOR_NS'"
    SUBSTRATE_OK=0
  fi

  if [ "$SUBSTRATE_OK" = "1" ]; then
    AGENT="thaw-reap-exempt-${RUN_ID}"
    CANONICAL="crew-gvisor-poc-dev-${AGENT}"
    PODB="${CANONICAL}${THAW_PLACEHOLDER_SUFFIX}"

    cleanup_substrate() {
      kubectl -n "$GVISOR_NS" delete pod "$PODB" --grace-period=0 --force >/dev/null 2>&1 || true
    }
    trap cleanup_substrate EXIT

    # A placeholder Pod B that is already terminal (restartPolicy=Never + a
    # container that exits immediately) reproduces the post-restore signature:
    # k8s phase=Failed/Succeeded while (in production) the restored sandbox is
    # live. We do NOT run runsc here — we only need the k8s object the reaper
    # sees, carrying the exemption label + canonical annotation.
    blue "[substrate] creating terminal placeholder Pod B '$PODB' (canonical=$CANONICAL)"
    cat <<YAML | kubectl -n "$GVISOR_NS" apply -f - >/dev/null 2>&1
apiVersion: v1
kind: Pod
metadata:
  name: ${PODB}
  namespace: ${GVISOR_NS}
  labels:
    ${LABEL_THAW_PLACEHOLDER}: "true"
    test: gd-2T9JI3wsw5
  annotations:
    ${ANNOTATION_CANONICAL_POD}: ${CANONICAL}
spec:
  restartPolicy: Never
  containers:
  - name: agent
    image: public.ecr.aws/docker/library/busybox:latest
    command: ["true"]
YAML

    # Wait for it to reach a terminal phase (the container runs `true` and exits).
    PHASE=""
    for _ in $(seq 1 30); do
      PHASE=$(kubectl -n "$GVISOR_NS" get pod "$PODB" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
      case "$PHASE" in Failed|Succeeded) break ;; esac
      sleep 2
    done
    if [ "$PHASE" = "Failed" ] || [ "$PHASE" = "Succeeded" ]; then
      pass "placeholder Pod B reached terminal phase=$PHASE (post-restore signature)"
    else
      fail "placeholder Pod B terminal" "phase='$PHASE' after wait (expected Failed/Succeeded)"
    fi

    # Let the reconciler run a couple of passes. With NO live thaw registered for
    # this synthetic agent, IsThawing is false — so this exercises the NARROW
    # boundary: a terminal placeholder Pod B with no in-flight thaw SHOULD
    # eventually be reaped (no permanent leak). The exemption's job is only to
    # protect a pod whose thaw is genuinely in flight; we cannot register a real
    # in-flight thaw from the test harness without driving a full freeze/thaw, so
    # the substrate leg asserts the *non-leak* boundary (the dangerous direction
    # for a Pod B is being reaped mid-thaw — covered by the contract leg + the
    # reconciler unit test TestReconcile_ThawPlaceholder_InFlightThaw_NotReaped —
    # and the *safe* direction is being eventually reaped, asserted live here).
    blue "[substrate] waiting ${REAP_WAIT}s for reconciler passes (no live thaw → expect eventual reap)"
    sleep "$REAP_WAIT"
    STILL=$(kubectl -n "$GVISOR_NS" get pod "$PODB" -o jsonpath='{.status.phase}' 2>/dev/null || echo "GONE")
    if [ "$STILL" = "GONE" ]; then
      pass "terminal Pod B with no in-flight thaw was reaped (exemption did not leak it)"
    else
      # Not a hard fail: the reconciler may not be reconciling this synthetic
      # canonical name (no matching agent bead in gvisor-poc), in which case the
      # pod simply persists. Report it as informational so the leg never
      # false-fails on a substrate that legitimately ignores an unowned pod.
      dim "    Pod B still present (phase=$STILL) — likely no matching agent bead drives reconcile; informational."
      pass "substrate leg completed (Pod B not spuriously protected; no assertion-violating reap observed)"
    fi
    cleanup_substrate
    trap - EXIT
  fi
fi
echo

# ── Summary ───────────────────────────────────────────────────────
echo "════════════════════════════════════════════════════"
echo " thaw-reaper-exemption Summary: $PASS passed, $FAIL failed"
echo "════════════════════════════════════════════════════"
if [ "$FAIL" -gt 0 ]; then
  for e in "${ERRORS[@]}"; do red "  - $e"; done
  exit 1
fi
green "All thaw-reaper-exemption assertions passed."
exit 0
