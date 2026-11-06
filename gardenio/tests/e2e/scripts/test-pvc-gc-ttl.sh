#!/usr/bin/env bash
# test-pvc-gc-ttl.sh — E2E: workspace PVC garbage collector deletes only
# stale, pod-less PVCs and refreshes the timer when an agent re-spawns.
#
# Background:
#   Commit d8db8bb7 (2026-04-12) set ownerReference on workspace PVCs to
#   the pod, so K8s GC wiped the PVC every time the pod terminated. That
#   broke session resume for thread-agent respawns. The replacement
#   approach (gd-qQTNQNVHxK):
#     1. PVC ownerReference is NOT set; PVC outlives the pod.
#     2. ensurePVC stamps gardenio.io/pvc-last-active on every pod create
#        (refreshed on re-spawn).
#     3. The pvcgc.Collector reaps PVCs whose last-active annotation is
#        older than PVC_GC_TTL. Bound PVCs (active pod) are skipped.
#
# This script is a lightweight K8s-fake assertion suite. It does NOT
# require a live cluster — it shells out to a tiny Go test runner that
# exercises the collector against a fake clientset. The "real" PVC
# lifetime under TTL pressure is exercised by the unit tests in
# pvcgc_test.go (TestCollect_AnnotationPreservesPVCAfterRespawn,
# TestCollect_AnnotationDeletesIdlePVC) and the podmanager unit test
# TestPVCGC_RefreshOnRespawnPreservesPVC. This script wraps them so the
# behaviour is visible in the E2E gate alongside the existing
# test-thread-respawn-resume.sh / test-session-archival.sh modules.
#
# Usage:
#   ./tests/e2e/scripts/test-pvc-gc-ttl.sh

set -euo pipefail

red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
CONTROLLER_DIR="${REPO_ROOT}/controller"

if [[ ! -d "${CONTROLLER_DIR}/internal/pvcgc" ]]; then
  red "FAIL: cannot find controller dir at ${CONTROLLER_DIR}"
  exit 1
fi

blue "[1/2] PVC GC: annotation-based TTL (refresh-on-respawn) — pvcgc collector"
if (cd "${CONTROLLER_DIR}" && go test ./internal/pvcgc/ \
       -run 'TestCollect_AnnotationPreservesPVCAfterRespawn|TestCollect_AnnotationDeletesIdlePVC|TestCollect_OrphanAboveTTL|TestCollect_NoOrphans' \
       -count=1 -timeout 60s); then
  green "  PASS: pvcgc collector honours gardenio.io/pvc-last-active annotation"
else
  red "  FAIL: pvcgc collector tests"
  exit 1
fi

blue "[2/2] PVC GC: ensurePVC refresh + GC end-to-end (podmanager)"
if (cd "${CONTROLLER_DIR}" && go test ./internal/podmanager/ \
       -run 'TestEnsurePVC_RefreshesLastActive|TestCreateAgentPod_WithPVC' \
       -count=1 -timeout 60s); then
  green "  PASS: ensurePVC refreshes last-active on every pod create; no ownerReference set"
else
  red "  FAIL: podmanager PVC tests"
  exit 1
fi

green "ALL PASS — PVC GC behaviour verified"
