#!/usr/bin/env bash
# test-gvisor-controller-restart.sh — E2E: controller RESTART mid-freeze and
# mid-thaw durability on the live gvisor-poc substrate. Task ka-0000002295 (epic
# ka-0000004667 "gVisor production readiness").
#
# THE GAP THIS CLOSES (the CONTROLLER-PROCESS durability concern):
#   This is distinct from the node-runsc E2E siblings (freeze-thaw, rehydrate-ack,
#   renudge-thaw). The unique, never-proven gap is durability across a REAL
#   controller bounce MID-CYCLE:
#
#   (a) ConfigMapSnapshotStore (ka-0000001694) exists SOLELY so a freeze→thaw
#       snapshot RECORD survives a controller pod restart (deploy / rollout / OOM /
#       eviction) between an agent's freeze and its thaw. Without it the in-memory
#       store is process-local: a restart loses the record, shouldThaw() returns
#       false, and the agent SILENTLY COLD-STARTS, discarding a restorable snapshot.
#       Yet the store is unit-tested ONLY with an in-process fake clientset — its
#       durability across a REAL process boundary against a LIVE API server is never
#       proven. A controller restart is a real exec boundary, so this module drives
#       it across TWO separate OS PROCESSES: proc1 ("pre-restart controller")
#       persists a complete record via the GENUINE ConfigMapSnapshotStore into the
#       live API server then EXITS; proc2 ("post-restart controller", a FRESH store
#       with zero in-memory carry-over) reads it back. proc2 asserts the complete
#       record SURVIVED and is trusted (HasSnapshot true → thaw, not cold-start) AND
#       that a HALF-snapshot (an absent record — a freeze the restart cut off before
#       the Put committed) is NOT trusted (HasSnapshot false → clean cold start).
#
#   (b) DrainThaws + the WithoutCancel-detached post-commit ctx (the controller's
#       graceful-shutdown drain at main.go:909, ka-0000001355 harden-thaw +
#       ka-0000004088 detached teardown) keep an in-flight thaw's detached critical
#       section running after the leader-election ctx is cancelled by a SIGTERM /
#       leader-loss, so a controller roll does not abandon an in-flight restore and
#       orphan Pod B. Also unit-tested ONLY inline with a fake client — never against
#       a genuine ctx-cancel boundary the way a real roll cancels.
#
# WHY THE REAL Go CODE, NOT A BASH RE-IMPLEMENTATION:
#   This module builds + runs cmd/ftrestart-probe, which constructs the GENUINE
#   internal/podmanager.ConfigMapSnapshotStore (case a, against the LIVE API server)
#   and the GENUINE FreezeThawManager.Thaw + InflightThawCount + DrainThaws + the
#   WithoutCancel-detached dctx (case b). A regression in the store's durability or
#   in the drain/detach contract fails THIS e2e, not just the next prod canary.
#
# HOW IT WORKS:
#   case (a) is driven across a REAL process boundary IN THE TASK CONTAINER itself —
#   the probe authenticates to the live API server exactly as kubectl does here, so
#   no privileged node pod is needed (the durability is a controller↔API-server
#   property, not a node-runsc one):
#     1. Build cmd/ftrestart-probe from source.
#     2. proc1: `ftrestart-probe -mode=freeze-persist` Puts a COMPLETE snapshot
#        ConfigMap into the gvisor-poc namespace via the real store, then EXITS
#        (the mid-freeze restart boundary).
#     3. Verify the ConfigMap is really in the API server (kubectl get).
#     4. proc2: a SEPARATE `ftrestart-probe -mode=freeze-recover` invocation (a fresh
#        store, zero heap carry-over) asserts the complete record survived + is
#        trusted, and a half-snapshot is not.
#   case (b) is process-local (the drain primitives are pure mutex/map state):
#     5. `ftrestart-probe -mode=thaw-drain` drives a real Thaw parked mid-restore,
#        cancels the inbound ctx (the controller-roll moment), and asserts the slot
#        survives, the detached ctx is NOT cancelled, and DrainThaws drains in budget.
#
# OPT-IN / GATING (NEVER silently passes):
#   Targets the controller namespace on the gvisor-poc cluster ($GVISOR_NS). Runs
#   only when GVISOR_E2E=1 (or CONTROLLER_RESTART_E2E=1) AND the substrate is present
#   (a go toolchain + create/get/list/delete configmaps in $GVISOR_NS). Opt-in unset
#   → SKIP clean (exit 0, inert in the standard shards). Opt-in SET but substrate
#   ABSENT → SKIP LOUD (still exit 0 so an absent cluster does not red the suite, but
#   a skip is never mistaken for a pass).
#   Wire it in via:  GVISOR_E2E=1 ONLY=gvisor-controller-restart run-shard.sh
#
# Prerequisites:
#   - kubectl context on the gvisor-poc cluster
#   - ns gvisor-poc, with create/get/list/delete on configmaps (RBAC verified:
#     can-i create configmaps in gvisor-poc = yes)
#   - a Go toolchain in the task (to build cmd/ftrestart-probe from source)
#
# Usage:
#   GVISOR_E2E=1 ./gardenio/tests/e2e/scripts/test-gvisor-controller-restart.sh
#   GVISOR_E2E=1 KEEP_CM=1 ./...                                   # debug
set -uo pipefail

# ── Config (all overridable via env) ────────────────────────────────
GVISOR_NS="${GVISOR_NS:-gvisor-poc}"
RUN_ID="${RUN_ID:-$$}"
# Controller module dir (to build cmd/ftrestart-probe from source).
CONTROLLER_DIR="${CONTROLLER_DIR:-gardenio/controller}"
# Per-run agent name so concurrent dispatches don't collide on the ConfigMap.
PROBE_AGENT="${PROBE_AGENT:-ftrestart-agent-${RUN_ID}}"
PROBE_NODE="${PROBE_NODE:-ftrestart-node-${RUN_ID}}"

PASS=0
FAIL=0
ERRORS=()

# ── Helpers ─────────────────────────────────────────────────────────
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
# SKIP loud — opt-in unset OR substrate missing. Always exit 0 (an absent cluster
# must NOT red the suite) but print a banner so a skip is never a pass.
skip_all() {
  local why="$1" tag="${2:-substrate}"
  if [ "$tag" = "loud" ]; then
    red "════════════════════════════════════════════════════"
    red " SKIP gvisor-controller-restart (LOUD): $why"
    red " OPT-IN WAS SET but the gvisor-poc substrate is absent — this run did NOT"
    red " assert controller-restart durability. A green suite here proves NOTHING"
    red " about ConfigMapSnapshotStore surviving a restart or the DrainThaws/"
    red " WithoutCancel detach. Provision kubectl context on gvisor-poc + configmap"
    red " RBAC in ns $GVISOR_NS (ka-0000002295)."
    red "════════════════════════════════════════════════════"
  else
    blue "SKIP gvisor-controller-restart: $why"
    dim  "  Opt-in module: set GVISOR_E2E=1 (or CONTROLLER_RESTART_E2E=1) and run with"
    dim  "  a go toolchain + configmap RBAC in ns $GVISOR_NS."
  fi
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

CM_NAME=""
cleanup() {
  if [ "${KEEP_CM:-0}" != "1" ]; then
    # Force-delete the snapshot ConfigMap by name in case proc2's best-effort
    # Delete did not run (e.g. an early failure). configMapToken lowercases +
    # maps '_'→'-'; PROBE_AGENT is already lowercase-safe, so the name is the
    # prefix + agent verbatim.
    if [ -n "$CM_NAME" ]; then
      kubectl -n "$GVISOR_NS" delete configmap "$CM_NAME" --ignore-not-found >/dev/null 2>&1 || true
    fi
  else
    dim "  KEEP_CM=1 — leaving snapshot ConfigMap '$CM_NAME' for debug"
  fi
}
trap cleanup EXIT

echo "════════════════════════════════════════════════════"
echo " gVisor controller-restart durability — real substrate E2E (ka-0000002295)"
echo " ns=$GVISOR_NS agent=$PROBE_AGENT node=$PROBE_NODE"
echo "════════════════════════════════════════════════════"

# ── Gate 0: opt-in + substrate preflight ────────────────────────────
if [ "${GVISOR_E2E:-0}" != "1" ] && [ "${CONTROLLER_RESTART_E2E:-0}" != "1" ]; then
  skip_all "GVISOR_E2E / CONTROLLER_RESTART_E2E != 1 (opt-in module)" off
fi
command -v kubectl >/dev/null 2>&1 || skip_all "kubectl not installed" loud
command -v go >/dev/null 2>&1 || skip_all "go toolchain not present (need it to build cmd/ftrestart-probe)" loud
kubectl get ns "$GVISOR_NS" >/dev/null 2>&1 \
  || skip_all "namespace '$GVISOR_NS' not present on this cluster" loud
[ "$(kubectl auth can-i create configmaps -n "$GVISOR_NS" 2>/dev/null)" = "yes" ] \
  || skip_all "cannot create configmaps in ns '$GVISOR_NS' (RBAC)" loud
[ "$(kubectl auth can-i get configmaps -n "$GVISOR_NS" 2>/dev/null)" = "yes" ] \
  || skip_all "cannot get configmaps in ns '$GVISOR_NS' (RBAC)" loud
[ -d "$CONTROLLER_DIR/cmd/ftrestart-probe" ] \
  || skip_all "cmd/ftrestart-probe missing under $CONTROLLER_DIR" loud

blue "Substrate present — running controller-restart durability assertions."

# ── 1. Build cmd/ftrestart-probe (the REAL store + drain driver) ────
blue "[1/5] building cmd/ftrestart-probe (real ConfigMapSnapshotStore + DrainThaws driver)"
PROBE_BIN="$(mktemp /tmp/ftrestart-probe.XXXXXX)"
if ( cd "$CONTROLLER_DIR" && CGO_ENABLED=0 go build -o "$PROBE_BIN" ./cmd/ftrestart-probe/ ) >"/tmp/ftrestart-build-${RUN_ID}.log" 2>&1; then
  pass "ftrestart-probe built ($(wc -c < "$PROBE_BIN" | tr -d ' ') bytes)"
else
  fail "build ftrestart-probe" "go build failed: $(tail -4 "/tmp/ftrestart-build-${RUN_ID}.log")"
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi

# The ConfigMap name the store will create (prefix + sanitized agent). We keep the
# agent name lowercase + dash-safe so the token equals the agent verbatim.
CM_NAME="gardenio-ftsnap-${PROBE_AGENT}"

# ── 2. case (a) proc1: persist a COMPLETE snapshot into the LIVE API server, EXIT ─
# This process IS the pre-restart controller. It Puts via the genuine store and
# exits — the snapshot now lives in the API server, independent of any process.
blue "[2/5] case (a) proc1: persist a complete snapshot via the real ConfigMapSnapshotStore, then EXIT (the mid-freeze restart boundary)"
PERSIST_OUT=$("$PROBE_BIN" -mode=freeze-persist -agent="$PROBE_AGENT" -namespace="$GVISOR_NS" -node="$PROBE_NODE" 2>&1)
PERSIST_LINE=$(printf '%s\n' "$PERSIST_OUT" | grep '^FTRESTART_PROBE' | head -1)
dim "    $PERSIST_LINE"
if grep -q 'mode=freeze-persist ok=true' <<<"$PERSIST_LINE"; then
  pass "proc1 committed a complete snapshot for $PROBE_AGENT to the live API server, then exited"
else
  fail "proc1 freeze-persist" "Put into the live API server failed: $(printf '%s\n' "$PERSIST_OUT" | tail -4)"
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi

# ── 3. Confirm the record really crossed the process boundary (it's in the API) ─
blue "[3/5] confirm the snapshot ConfigMap is durable in the API server (survives proc1's exit)"
if kubectl -n "$GVISOR_NS" get configmap "$CM_NAME" >/dev/null 2>&1; then
  CM_LABEL=$(kubectl -n "$GVISOR_NS" get configmap "$CM_NAME" \
    -o jsonpath='{.metadata.labels.gardenio\.io/managed-by}' 2>/dev/null)
  pass "snapshot ConfigMap '$CM_NAME' present in API server after proc1 exited (managed-by='$CM_LABEL')"
else
  fail "snapshot ConfigMap durable" "ConfigMap '$CM_NAME' not found in ns '$GVISOR_NS' after proc1 exited — the Put did not persist to the API server"
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi

# ── 4. case (a) proc2: a FRESH post-restart store recovers the record ───
# A SEPARATE process with zero in-memory carry-over — the post-restart controller.
blue "[4/5] case (a) proc2: a FRESH ConfigMapSnapshotStore recovers the record (the post-restart controller)"
RECOVER_KEEP=""
[ "${KEEP_CM:-0}" = "1" ] && RECOVER_KEEP="-keep"
RECOVER_OUT=$("$PROBE_BIN" -mode=freeze-recover -agent="$PROBE_AGENT" -namespace="$GVISOR_NS" -node="$PROBE_NODE" $RECOVER_KEEP 2>&1)
RECOVER_LINE=$(printf '%s\n' "$RECOVER_OUT" | grep '^FTRESTART_PROBE' | head -1)
dim "    $RECOVER_LINE"
R_HAS=$(printf '%s' "$RECOVER_LINE" | sed -n 's/.*has_complete=\([a-z]*\).*/\1/p')
R_IDENT=$(printf '%s' "$RECOVER_LINE" | sed -n 's/.*identity_ok=\([a-z]*\).*/\1/p')
R_NODE=$(printf '%s' "$RECOVER_LINE" | sed -n 's/.*node_resolved=\([a-z]*\).*/\1/p')
R_TOPO=$(printf '%s' "$RECOVER_LINE" | sed -n 's/.*topo_ok=\([a-z]*\).*/\1/p')
R_HALF=$(printf '%s' "$RECOVER_LINE" | sed -n 's/.*half_trusted=\([a-z]*\).*/\1/p')

# (a1) The complete record survived the restart and is TRUSTED (thaw, not cold-start).
if [ "$R_HAS" = "true" ] && [ "$R_IDENT" = "true" ]; then
  pass "(a) complete snapshot SURVIVED the restart + is trusted (HasSnapshot=true, identity round-trips) → post-restart wake THAWS, not silently cold-starts (ka-0000001694)"
else
  fail "complete snapshot survives restart" "has_complete='$R_HAS' identity_ok='$R_IDENT' — the record did NOT survive the controller bounce; the wake would silently cold-start: $(printf '%s\n' "$RECOVER_OUT" | tail -4)"
fi
# (a2) node + topology resolve off the recovered record (warm-graft can target it).
if [ "$R_NODE" = "true" ] && [ "$R_TOPO" = "true" ]; then
  pass "(a) SnapshotNode + SnapshotTopology resolve off the recovered record — the node-local warm-graft thaw can target it post-restart"
else
  fail "node/topology resolve post-restart" "node_resolved='$R_NODE' topo_ok='$R_TOPO' — the recovered record could not drive a node-local warm-graft"
fi
# (a3) a HALF-snapshot (freeze interrupted before the Put) is NOT trusted.
if [ "$R_HALF" = "false" ]; then
  pass "(a) a HALF-snapshot (a freeze the restart cut off before its Put) is NOT trusted (HasSnapshot=false) → clean cold start, never a half-restore"
else
  fail "half-snapshot not trusted" "half_trusted='$R_HALF' — a partial/absent record was trusted; the wake would attempt a half-restore instead of cold-starting"
fi

# ── 5. case (b): thaw-drain — drain + WithoutCancel detach (process-local) ──
blue "[5/5] case (b) thaw-drain: real Thaw parked mid-restore at inbound-ctx cancel (the controller-roll moment) → drain + detach"
DRAIN_OUT=$("$PROBE_BIN" -mode=thaw-drain -agent="$PROBE_AGENT" -namespace="$GVISOR_NS" -node="$PROBE_NODE" 2>&1)
DRAIN_LINE=$(printf '%s\n' "$DRAIN_OUT" | grep '^FTRESTART_PROBE' | head -1)
dim "    $DRAIN_LINE"
D_BEFORE=$(printf '%s' "$DRAIN_LINE" | sed -n 's/.*inflight_before=\([0-9]*\).*/\1/p')
D_AFTER_CANCEL=$(printf '%s' "$DRAIN_LINE" | sed -n 's/.*inflight_after_cancel=\([0-9]*\).*/\1/p')
D_DETACHED_CANCELLED=$(printf '%s' "$DRAIN_LINE" | sed -n 's/.*detached_ctx_cancelled=\([a-z]*\).*/\1/p')
D_SHORT=$(printf '%s' "$DRAIN_LINE" | sed -n 's/.*short_drain_remaining=\([0-9]*\).*/\1/p')
D_FULL=$(printf '%s' "$DRAIN_LINE" | sed -n 's/.*full_drain_remaining=\([0-9]*\).*/\1/p')

# (b1) the in-flight thaw slot survives the inbound-ctx cancel (a controller roll).
if [ "${D_BEFORE:-0}" -ge 1 ] && [ "${D_AFTER_CANCEL:-0}" -ge 1 ]; then
  pass "(b) InflightThawCount survived the inbound-ctx cancel (before=$D_BEFORE after_cancel=$D_AFTER_CANCEL) — DrainThaws will wait for the in-flight restore across a controller roll (Pod B not abandoned)"
else
  fail "in-flight slot survives cancel" "inflight_before='$D_BEFORE' inflight_after_cancel='$D_AFTER_CANCEL' — the parent cancel abandoned the in-flight thaw; the process would exit mid-restore and orphan Pod B"
fi
# (b2) the detached post-commit ctx is NOT cancelled by the parent cancel.
if [ "$D_DETACHED_CANCELLED" = "false" ]; then
  pass "(b) the detached post-commit ctx was NOT cancelled by the parent cancel — context.WithoutCancel decoupled the restore (no ka-0000004088 ~6.8s confound)"
else
  fail "detached ctx survives cancel" "detached_ctx_cancelled='$D_DETACHED_CANCELLED' — WithoutCancel did not decouple the restore; a controller roll would cancel the in-flight restore"
fi
# (b3) full-budget drain reaches 0; short-budget drain genuinely waited (control).
if [ "${D_FULL:-1}" = "0" ] && [ "${D_SHORT:-0}" -ge 1 ]; then
  pass "(b) DrainThaws drained to 0 within budget after release (short-budget control returned $D_SHORT while parked → it genuinely waits on in-flight work, not a no-op)"
else
  fail "DrainThaws drains in budget" "full_drain_remaining='$D_FULL' short_drain_remaining='$D_SHORT' — the drain did not reach 0 in budget, or the short-budget control wrongly reported drained: $(printf '%s\n' "$DRAIN_OUT" | tail -4)"
fi

# ── Summary ─────────────────────────────────────────────────────────
echo ""
echo "════════════════════════════════════════════════════"
echo " gVisor controller-restart Summary: $PASS passed, $FAIL failed"
echo "   agent=$PROBE_AGENT cm=$CM_NAME  has_complete=${R_HAS:-?} half_trusted=${R_HALF:-?}  drain_full=${D_FULL:-?} detached_cancelled=${D_DETACHED_CANCELLED:-?}"
echo "════════════════════════════════════════════════════"
if [ "$FAIL" -gt 0 ]; then
  for e in "${ERRORS[@]}"; do red "  - $e"; done
  exit 1
fi
green "All controller-restart durability assertions passed — ConfigMapSnapshotStore survives a real process boundary against the live API server (complete trusted, half not); DrainThaws + WithoutCancel detach keep an in-flight thaw alive across a controller roll."
exit 0
