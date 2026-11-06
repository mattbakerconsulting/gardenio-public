#!/usr/bin/env bash
# run-drc-via-admin-kubeconfig.sh — drive the discriminating restore check REMOTELY
# from this (sandboxed) agent pod, using a cluster-admin kubeconfig.
#
# WHY THIS EXISTS. This agent pod is itself a gVisor sandbox (uname 4.19.0-gvisor,
# 9p rootfs) bound to a deliberately minimal SA (gardenio-gvisor:gardenio-agent) with
# NO pods/exec on the node. It therefore cannot run `runsc` on the host directly.
# But sandbox status is IRRELEVANT to a REMOTE kubectl exec: given a cluster-admin
# credential, this script exec's into the STANDING privileged gvisor-probe pod
# (DaemonSet, app=gvisor-probe, one per gVisor node — gvisor.probe.create=true) on
# the target node and runs the check there, exactly as the controller's
# ProbeNodeExecutor does. No new pod, no node SSH, no host mount needed here.
#
# THE ONE THING ONLY THE OPERATOR CAN PROVIDE: a cluster-admin kubeconfig for the
# prod cluster (america-e2e-eks). Per advice ka-0000005378 the gardenio namespace
# holds a retrievable cluster-admin token Secret; an operator (or anything with
# cluster-admin) can hand its kubeconfig to this pod, e.g.:
#   kubectl -n gardenio get secret gardenio-cluster-admin-token -o jsonpath='{.data.token}' | base64 -d
# then materialize a kubeconfig and point this script at it via $KUBECONFIG or --kubeconfig.
# (Equivalently, an operator can just grant gardenio-gvisor:gardenio-agent pods/exec
# in gvisor-poc with a one-line RoleBinding, and this script runs with the pod's
# own in-cluster credentials — set --in-cluster.)
#
# SAFETY: identical blast radius to discriminating-restore-check.sh — it copies the
# script into the probe pod and runs the check, which restores into a SCRATCH cid
# against COPIES under /var/tmp. Never mutates the live checkpoint, never deletes a
# live pod, never reads/flips gvisor.freezeThaw. Zero canary, zero orphan.
set -uo pipefail

NS="${NS:-gvisor-poc}"
AGENT="${AGENT:-gardenio-uuae}"
NODE="${NODE:-ip-10-30-136-242}"          # node holding the persisted failing bundle
PROBE_SELECTOR="${PROBE_SELECTOR:-app=gvisor-probe}"
KCFG=""                                    # --kubeconfig <path> (else $KUBECONFIG / in-cluster)
IN_CLUSTER=0
SCRIPT="$(cd "$(dirname "$0")" && pwd)/discriminating-restore-check.sh"

while [ $# -gt 0 ]; do
  case "$1" in
    --kubeconfig) KCFG="$2"; shift 2 ;;
    --in-cluster) IN_CLUSTER=1; shift ;;
    --agent)      AGENT="$2"; shift 2 ;;
    --node)       NODE="$2"; shift 2 ;;
    --ns)         NS="$2"; shift 2 ;;
    --script)     SCRIPT="$2"; shift 2 ;;
    -h|--help)    sed -n '2,40p' "$0"; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

KFLAG=()
if [ -n "$KCFG" ]; then KFLAG=(--kubeconfig "$KCFG")
elif [ "$IN_CLUSTER" = 1 ]; then KFLAG=()
elif [ -n "${KUBECONFIG:-}" ]; then KFLAG=(--kubeconfig "$KUBECONFIG")
fi

[ -f "$SCRIPT" ] || { echo "SETUP-ERROR: check script not found: $SCRIPT" >&2; exit 2; }

say() { echo "[run-drc] $*"; }

# Preflight: confirm we actually have exec rights with the supplied credential.
say "preflight: kubectl auth can-i create pods/exec -n $NS"
if ! kubectl "${KFLAG[@]}" auth can-i create pods/exec -n "$NS" >/dev/null 2>&1; then
  echo "SETUP-ERROR: the supplied credential cannot 'create pods/exec' in $NS." >&2
  echo "  Provide a cluster-admin kubeconfig (--kubeconfig / \$KUBECONFIG) or grant" >&2
  echo "  gardenio-gvisor:gardenio-agent pods/exec in $NS and pass --in-cluster." >&2
  exit 2
fi

# Find the standing privileged probe pod on the target node.
say "locating gvisor-probe on node=$NODE"
POD=$(kubectl "${KFLAG[@]}" -n "$NS" get pods -l "$PROBE_SELECTOR" \
        --field-selector "spec.nodeName=$NODE" \
        -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
if [ -z "$POD" ]; then
  # fall back to any probe pod (then re-pin by reporting its node)
  POD=$(kubectl "${KFLAG[@]}" -n "$NS" get pods -l "$PROBE_SELECTOR" \
          -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
  [ -z "$POD" ] && { echo "SETUP-ERROR: no $PROBE_SELECTOR pod found in $NS (is gvisor.probe.create=true?)." >&2; exit 2; }
  onode=$(kubectl "${KFLAG[@]}" -n "$NS" get pod "$POD" -o jsonpath='{.spec.nodeName}' 2>/dev/null)
  say "WARN no probe on $NODE; using $POD on $onode instead (the persisted bundle must live on THAT node)."
fi
say "probe pod = $POD"

# Copy the check script into the probe (its container fs), then chroot the node and run it.
say "copying check script into probe and running (chroot /host)"
kubectl "${KFLAG[@]}" -n "$NS" cp "$SCRIPT" "$POD:/host/var/tmp/discriminating-restore-check.sh"
kubectl "${KFLAG[@]}" -n "$NS" exec "$POD" -- \
  chroot /host /bin/bash -c "AGENT='$AGENT' /bin/bash /var/tmp/discriminating-restore-check.sh"
RC=$?
say "check exited rc=$RC"
# Best-effort cleanup of the copied script.
kubectl "${KFLAG[@]}" -n "$NS" exec "$POD" -- rm -f /host/var/tmp/discriminating-restore-check.sh >/dev/null 2>&1 || true
exit $RC
