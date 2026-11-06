#!/usr/bin/env bash
# test-gvisor-ns-sweep.sh — E2E: the controller mirrors/sweeps across the
# gVisor namespace as well as the primary release namespace.
#
# Background (gd-obdTf3fIcu, backfilling ka-0000007922 + ka-0000008076):
#   A prewarmed-pool standby or any runsc/gVisor agent (role=gvisor-poc) routes
#   its pod to cfg.GVisorNamespace (gardenio-gvisor) via routeGVisorNamespace, not
#   the primary release namespace. Two commits taught the controller to treat
#   that second namespace as first-class:
#
#     - 9862042a secretreconciler: SetExtraNamespaces mirrors every managed
#       ExternalSecret into the gVisor ns too, so a runsc pod does not stall
#       forever on "secret(s) not yet synced in namespace gardenio-gvisor".
#     - 554fb95e reconciler / stale-reaper / pvc-gc: SetExtraNamespaces /
#       SetGVisorNamespace so orphan cleanup, drift recreation, stale-pod
#       deletion and workspace-PVC GC all reach the gVisor ns — and are a
#       clean no-op (single-namespace behaviour) when the gVisor ns is unset.
#
# The four E2E modules that cover these subsystems in the live-cluster gate
# (test-secret-reconciler.sh, test-reconciler.sh, test-stale-reaper.sh,
# test-pvc-gc-ttl.sh) are all single-namespace (NAMESPACE=gardenio-e2e). None
# assert the gVisor-ns paths, so a regression that dropped the mirror/sweep for
# the second namespace — the exact failure that stalled a live pooltest
# gvisor-poc pool — would sail through the gate unseen.
#
# Like test-pvc-gc-ttl.sh, this is a hermetic K8s-fake assertion suite: it does
# NOT require a live cluster. It shells out to the package unit tests that
# exercise each subsystem against a fake clientset / httptest daemon, wrapping
# them so the multi-namespace behaviour is visible in the E2E gate alongside the
# single-namespace modules. The "real" gVisor freeze/thaw pod lifecycle is
# exercised separately by the opt-in test-gvisor-*.sh suite (GVISOR_E2E=1).
#
# Assertions, one subsystem per step:
#   1. secretreconciler — ExternalSecret mirrored into BOTH namespaces; opt-in.
#   2. reconciler       — pod present/orphan swept in the gVisor ns; opt-in.
#   3. stale-reaper     — stale runsc pod deleted from the gVisor ns; opt-in.
#   4. pvc-gc           — orphan workspace PVC GC'd in the gVisor ns; opt-in.
#
# Usage:
#   ./tests/e2e/scripts/test-gvisor-ns-sweep.sh

set -euo pipefail

red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
CONTROLLER_DIR="${REPO_ROOT}/controller"

if [[ ! -d "${CONTROLLER_DIR}/internal/secretreconciler" ]]; then
  red "FAIL: cannot find controller dir at ${CONTROLLER_DIR}"
  exit 1
fi

# run_pkg <label> <package> <run-regex> <success-msg>
run_pkg() {
  local label="$1" pkg="$2" regex="$3" ok="$4"
  blue "$label"
  if (cd "${CONTROLLER_DIR}" && go test "$pkg" -run "$regex" -count=1 -timeout 120s); then
    green "  PASS: $ok"
  else
    red "  FAIL: $pkg ($regex)"
    exit 1
  fi
}

# 1. secretreconciler: every managed ExternalSecret is materialized into the
#    gVisor ns as well as the primary ns (ka-0000007922), and NOT into the gVisor
#    ns when the mirror is not enabled (clean single-namespace no-op).
run_pkg "[1/4] secretreconciler: ExternalSecret mirrored into gVisor ns (+ opt-in guard)" \
  ./internal/secretreconciler/ \
  'TestReconcile_MirrorsIntoExtraNamespaces|TestReconcile_ExtraNamespaceOptIn' \
  "managed ExternalSecrets mirror into gardenio-gvisor; primary-only when unset"

# 2. reconciler: desired-vs-actual sweep counts a gVisor-ns pod as present (no
#    duplicate create), deletes a gVisor-ns orphan in its own namespace, and is
#    invisible to the gVisor ns when SetExtraNamespaces is not called.
run_pkg "[2/4] reconciler: gVisor-ns pod present/orphan sweep (+ opt-in guard)" \
  ./internal/reconciler/ \
  'TestReconcile_ExtraNamespacePodCountedAsPresent|TestReconcile_DeletesOrphanInExtraNamespace|TestReconcile_ExtraNamespaceOptIn|TestReconcile_SetExtraNamespacesIgnoresEmptyAndPrimary|TestAutodestruct_SweepsExtraNamespace' \
  "reconciler sweeps gardenio-gvisor; single-namespace no-op when unset"

# 3. stale-reaper: a stale runsc agent's pod is deleted from the gVisor ns
#    (routeGVisorNamespace), a non-runsc agent stays in the primary ns, a pinned
#    namespace wins, and routing is opt-in.
run_pkg "[3/4] stale-reaper: runsc pod deleted from gVisor ns (+ pin/opt-in guards)" \
  ./internal/statusreporter/ \
  'TestStaleReaper_RoutesRunscDeleteToGVisorNamespace|TestStaleReaper_GVisorNamespaceOptIn|TestStaleReaper_PinnedNamespaceWins|TestStaleReaper_SetGVisorNamespaceIgnoresEmptyAndPrimary' \
  "stale runsc pods reaped in gardenio-gvisor; primary-ns default when unset"

# 4. pvc-gc: an orphan workspace PVC in the gVisor ns is GC'd, a PVC still
#    mounted by a pod in its own namespace is not, and the sweep is opt-in.
run_pkg "[4/4] pvc-gc: orphan workspace PVC GC'd in gVisor ns (+ in-use/opt-in guards)" \
  ./internal/pvcgc/ \
  'TestCollect_SweepsExtraNamespace|TestCollect_ExtraNamespaceOptIn|TestCollect_ExtraNamespacePVCInUse|TestSetExtraNamespaces_IgnoresEmptyAndPrimary' \
  "orphan PVCs GC'd in gardenio-gvisor; untouched when unset or in-use"

green "ALL PASS — gVisor-namespace mirror + multi-ns reconcile/reap/PVC-GC verified"
