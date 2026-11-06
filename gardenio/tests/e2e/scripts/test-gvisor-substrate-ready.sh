#!/usr/bin/env bash
# SC2317: node_exec and cleanup are invoked indirectly (via the EXIT trap and as
# the node-runner passed by name to gvisor_warm_node_gate). shellcheck cannot
# trace those call paths, so its "unreachable" reports here are false positives.
# shellcheck disable=SC2317
# test-gvisor-substrate-ready.sh — the gVisor PRODUCTION-READINESS substrate gate.
# Task ka-0000002867 (epic ka-0000004667). Deliverable (a): a gate that RELIABLY
# stands up the gvisor-poc substrate (or asserts it is present) and FAILS LOUD if
# it is absent — so the downstream gvisor lifecycle modules run against a real
# substrate instead of self-skipping into a meaningless green.
#
# WHY THIS EXISTS
#   Every gvisor lifecycle module (test-gvisor-freeze-thaw, -armed-guard,
#   -rehydration, -rehydrate-ack, resume-latency, userresponse-latency,
#   gb-stop-suspend) self-SKIPs (exit 0) when the gvisor-poc substrate is absent —
#   and the substrate is frequently torn down (Karpenter cold-start; the
#   controller lacks gardenio-gvisor RBAC at rest — fast-interruptible-freeze.md
#   C3). A production-readiness go/no-go cannot rest on tests that silently skip.
#   This module is the FIRST task in the gardenio-gvisor-readiness dispatch: it
#   PROVISIONS / asserts the substrate, so when the lifecycle modules run with
#   REQUIRE_SUBSTRATE=1 they actually have a substrate to assert against.
#
# WHAT IT DOES
#   1. Asserts the runsc RuntimeClass exists + create-pods in $GVISOR_NS (the same
#      substrate contract every lifecycle module checks).
#   2. PROVOKES Karpenter: schedules a privileged probe pod (nodeSelector
#      gvisor=enabled + toleration) so a gvisor=poc node is provisioned on demand.
#      Waits up to PROBE_READY_TIMEOUT for it to become Ready — this is the
#      "stand up the substrate" half: an absent node pool is scaled up here rather
#      than discovered missing later.
#   3. Runs the shared gvisor_warm_node_gate: bounded poll that the node is
#      genuinely WARM (runsc present + executable, /var/tmp writable, runsc state
#      root reachable) — not merely that the pod reported Ready while runsc was
#      still settling (the rotating cold-start flake ka-0000007234).
#
# EXIT CONTRACT (this is a GATE, not an opt-in lifecycle module)
#   GVISOR_E2E unset → inert clean skip (exit 0): not selected.
#   GVISOR_E2E=1, substrate absent, REQUIRE_SUBSTRATE unset/0 → loud SKIP (exit 0):
#     the long-standing operator mandate (ka-0000000847 / ka-0000003662) that an
#     absent pool must not red a non-readiness dispatch.
#   GVISOR_E2E=1, substrate absent, REQUIRE_SUBSTRATE=1 → NO-GO hard fail (exit 1):
#     the readiness gate. The substrate could not be stood up — do NOT flip
#     FreezeThawEnabled on this signal.
#   GVISOR_E2E=1, substrate stood up + warm → PASS (exit 0): the lifecycle modules
#     that follow in the dispatch will run against a real substrate.
#
# USAGE
#   GVISOR_E2E=1 REQUIRE_SUBSTRATE=1 ./gardenio/tests/e2e/scripts/test-gvisor-substrate-ready.sh
#   GVISOR_E2E=1 KEEP_PROBE=1 ./...          # keep the probe pod warm for the run
#
# Prerequisites (escalation ka-0000004674): kubectl context on the cluster with
# the gvisor-poc node pool; ns $GVISOR_NS (PSA=privileged); RuntimeClass runsc;
# SA gardenio-agent can create pods + pods/exec in $GVISOR_NS.

set -uo pipefail

# Shared gVisor substrate helpers: gvisor_require_substrate_guard (the readiness
# decision for an absent substrate) + gvisor_warm_node_gate (bounded warm-node
# poll). Task ka-0000002867, lib decision ka-0000000122, root flake ka-0000007234.
# shellcheck disable=SC1091
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib-gvisor-node.sh"

# ── Config (all overridable via env) ────────────────────────────────
GVISOR_NS="${GVISOR_NS:-gvisor-poc}"
RUNTIME_CLASS="${RUNTIME_CLASS:-runsc}"
RUN_ID="${RUN_ID:-$$}"
PROBE="${PROBE:-gvisor-ready-probe-${RUN_ID}}"
# How long to wait for Karpenter to provision a gvisor=poc node + the probe to go
# Ready. Karpenter cold-start (node launch + bootstrap + runsc stage) can be a few
# minutes, so this is generous; the warm-gate that follows is the finer assertion.
PROBE_READY_TIMEOUT="${PROBE_READY_TIMEOUT:-300s}"

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
# opt-in OFF → inert clean skip; substrate ABSENT → shared readiness guard
# (hard-fails under REQUIRE_SUBSTRATE=1, clean-skips otherwise).
skip_optin_off() {
  blue "SKIP gvisor-substrate-ready: $1"
  dim "  This gate is opt-in (set GVISOR_E2E=1) and is the FIRST task of the"
  dim "  gardenio-gvisor-readiness dispatch. It stands up / asserts the gvisor-poc"
  dim "  substrate so the downstream lifecycle modules run against a real node."
}
skip_all() { gvisor_require_substrate_guard gvisor-substrate-ready "$1"; }

k() { kubectl -n "$GVISOR_NS" "$@"; }
node_exec() {
  local cap="$1"; shift
  timeout "$((cap + 20))" kubectl -n "$GVISOR_NS" exec "$PROBE" -- timeout "$cap" chroot /host bash -c "$1" 2>/dev/null
}

cleanup() {
  if [ "${KEEP_PROBE:-0}" != "1" ]; then
    k delete pod "$PROBE" --grace-period=0 --force >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

echo "════════════════════════════════════════════════════"
echo " gVisor PRODUCTION-READINESS substrate gate (ka-0000002867)"
echo " ns=$GVISOR_NS  runtimeClass=$RUNTIME_CLASS  require_substrate=${REQUIRE_SUBSTRATE:-0}"
echo "════════════════════════════════════════════════════"

# ── Gate 0: opt-in + readiness-only + substrate contract preflight ──
# opt-in OFF → inert clean skip.
[ "${GVISOR_E2E:-0}" = "1" ] || { skip_optin_off "GVISOR_E2E != 1 (opt-in module)"; exit 0; }
# READINESS-ONLY: unlike the lifecycle modules (which assert behaviour and are
# meaningful under any dispatch), this preflight's only job is to STAND UP the
# substrate (it actively schedules a probe to provoke Karpenter). A full-suite
# dispatch runs every module, so without this guard it would needlessly schedule
# a probe pod on every gardenio-e2e / per-feature run. It is only wanted inside the
# readiness gate, so it is inert unless REQUIRE_SUBSTRATE=1 (task ka-0000002867).
if [ "${REQUIRE_SUBSTRATE:-0}" != "1" ]; then
  blue "SKIP gvisor-substrate-ready: REQUIRE_SUBSTRATE != 1"
  dim "  This provisioning preflight runs ONLY under the gardenio-gvisor-readiness"
  dim "  dispatch (REQUIRE_SUBSTRATE=1). It stands up / asserts the gvisor-poc"
  dim "  substrate so the lifecycle modules that follow run against a real node."
  exit 0
fi
command -v kubectl >/dev/null 2>&1 || skip_all "kubectl not installed"
kubectl get runtimeclass "$RUNTIME_CLASS" >/dev/null 2>&1 \
  || skip_all "RuntimeClass '$RUNTIME_CLASS' not present on this cluster"
[ "$(kubectl auth can-i create pods -n "$GVISOR_NS" 2>/dev/null)" = "yes" ] \
  || skip_all "cannot create pods in ns '$GVISOR_NS'"
pass "substrate contract present (RuntimeClass '$RUNTIME_CLASS' + create-pods in '$GVISOR_NS')"

# ── 1. Provoke Karpenter: schedule the probe to force a gvisor=poc node ──
blue "[1/2] scheduling privileged probe '$PROBE' (provokes Karpenter to scale a gvisor=poc node)"
cat <<YAML | k apply -f - >/dev/null 2>&1
apiVersion: v1
kind: Pod
metadata:
  name: ${PROBE}
  namespace: ${GVISOR_NS}
  labels: {app: gvisor-ready-probe, test: ka-0000002867}
spec:
  restartPolicy: Never
  nodeSelector: {gvisor: enabled}
  tolerations:
  - {key: gvisor, operator: Equal, value: enabled, effect: NoSchedule}
  hostPID: true
  hostNetwork: true
  containers:
  - name: probe
    image: public.ecr.aws/docker/library/ubuntu:22.04
    command: ["sleep", "1800"]
    securityContext: {privileged: true}
    volumeMounts:
    - {name: host, mountPath: /host}
    - {name: run, mountPath: /run}
  volumes:
  - {name: host, hostPath: {path: /}}
  - {name: run, hostPath: {path: /run}}
YAML
if ! k wait --for=condition=Ready "pod/$PROBE" --timeout="$PROBE_READY_TIMEOUT" >/dev/null 2>&1; then
  # Karpenter never produced a Ready gvisor=poc node in the window. Show the
  # diagnostics, then make the readiness decision: NO-GO under the gate.
  k get pod "$PROBE" -o wide 2>&1 | sed 's/^/    /'
  k get events --field-selector "involvedObject.name=$PROBE" 2>/dev/null | tail -5 | sed 's/^/    /'
  skip_all "no gvisor=poc node became Ready in $PROBE_READY_TIMEOUT (Karpenter did not provision / runsc node pool absent)"
fi
pass "probe scheduled + Ready on a gvisor=poc node (Karpenter stood up the substrate)"

# ── 2. Warm-node gate: prove runsc is genuinely staged, not just pod-Ready ──
blue "[2/2] warm-node gate (runsc present + /var/tmp writable + runsc state root reachable)"
if gvisor_warm_node_gate node_exec; then
  pass "gvisor-poc node WARM — substrate is ready for the lifecycle modules"
else
  # The node came up but runsc never finished staging (a true substrate fault,
  # not a transient — the warm-gate already rode out the cold-start window). NO-GO
  # under the gate.
  fail "gvisor-poc node warm-gate" "$GVISOR_WARM_DIAG"
fi

# ── Summary ─────────────────────────────────────────────────────────
echo ""
echo "════════════════════════════════════════════════════"
echo " gVisor substrate-ready Summary: $PASS passed, $FAIL failed"
echo "════════════════════════════════════════════════════"
if [ "$FAIL" -gt 0 ]; then
  for e in "${ERRORS[@]}"; do red "  - $e"; done
  if [ "${REQUIRE_SUBSTRATE:-0}" = "1" ]; then
    red "NO-GO: the readiness gate could not stand up a warm gvisor-poc substrate."
    red "Do NOT flip FreezeThawEnabled for real agents. See escalation ka-0000004674."
    exit 1
  fi
  # Outside the gate a warm-up failure is a loud advisory, not a hard block.
  blue "Substrate not warm, but REQUIRE_SUBSTRATE!=1 — advisory only (exit 0)."
  exit 0
fi
green "gVisor substrate stood up and WARM — ready for the lifecycle modules."
exit 0
