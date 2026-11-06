#!/usr/bin/env bash
# pin-images.sh — Gardenio in a Box (import-only air-gap): pin every imported
# containerd image so the kubelet/containerd GC will not reclaim it under
# DiskPressure.
#
# Part of epic ka-0000002730 (registry-FREE import-only air-gap), task
# ka-0000006466. Source research: gardenio/docs/research/inbox-ec2-sizing-bootstrap.md
# §3 (k3s air-gapped image import + containerd GC pin labels).
#
# WHY: In --airgap-import-only mode there is NO in-box registry and
# agentPullPolicy is forced to Never. Images arrive ONLY via `k3s ctr images
# import` (and the k3s auto-load dir). Manually-imported images are GC-eligible:
# under DiskPressure the kubelet's containerd image GC can reclaim any image not
# in use, and there is no registry to re-pull from — so a reclaim wedges every
# pod that referenced it into ErrImageNeverPull / ImagePullBackOff with no
# recovery path.
#
# WHAT THIS DOES: label every image in the containerd `k8s.io` namespace with
#   io.cri.containerd.pinned=pinned   (CRI image GC: never reclaim)
#   io.cattle.k3s.pinned=pinned       (k3s-specific pin marker)
# Containerd's CRI image-GC honours `io.cri.containerd.pinned`; pinned images
# are skipped by the GC sweep regardless of DiskPressure. The k3s label is
# belt-and-suspenders for k3s' own bookkeeping.
#
# Idempotent: relabelling an already-pinned image is a no-op. Re-run after EVERY
# (re-)import — bootstrap-ec2.sh invokes it from import_images() in import-only
# mode, and the k3s auto-load dir re-imports on each k3s start (those re-imports
# also need re-pinning, which this script provides when re-run).
#
# Usage:
#   sudo scripts/inbox/pin-images.sh [--namespace NS] [--ctr CMD] [--quiet]
#
# Env / flags (flags win over env):
#   CTR_NS    containerd namespace to pin            (default: k8s.io)
#   CTR       ctr invocation                         (default: "k3s ctr")
#   QUIET     1=only log the summary line            (default: 0)
set -euo pipefail

CTR_NS="${CTR_NS:-k8s.io}"
CTR="${CTR:-k3s ctr}"
QUIET="${QUIET:-0}"

while [ $# -gt 0 ]; do
  case "$1" in
    --namespace) CTR_NS="$2"; shift 2;;
    --ctr)       CTR="$2"; shift 2;;
    --quiet)     QUIET=1; shift;;
    -h|--help)   sed -n '2,46p' "$0"; exit 0;;
    *) echo "unknown arg: $1" >&2; exit 2;;
  esac
done

log()  { [ "$QUIET" = "1" ] || printf '\033[1;36m[pin]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[pin] WARN:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[pin] FATAL:\033[0m %s\n' "$*" >&2; exit 1; }

# `ctr` may be a multi-word command ("k3s ctr"); split on whitespace into an
# array so each word is passed as a distinct argv element.
read -ra CTR_CMD <<< "$CTR"
command -v "${CTR_CMD[0]}" >/dev/null 2>&1 || die "missing dependency: ${CTR_CMD[0]} (need 'k3s ctr' or set --ctr)"

ctr() { "${CTR_CMD[@]}" -n "$CTR_NS" "$@"; }

PINS=(
  "io.cri.containerd.pinned=pinned"
  "io.cattle.k3s.pinned=pinned"
)

log "pinning containerd images in namespace '$CTR_NS' (${CTR_CMD[*]})"

# List every image reference in the namespace. `ctr images ls -q` prints one
# ref per line (no header), which is exactly what we want to feed to label.
mapfile -t REFS < <(ctr images ls -q 2>/dev/null || true)

if [ "${#REFS[@]}" -eq 0 ]; then
  warn "no images present in namespace '$CTR_NS' — nothing to pin (did import_images() run?)"
  exit 0
fi

PINNED=0
FAILED=()
for ref in "${REFS[@]}"; do
  [ -n "$ref" ] || continue
  # `ctr images label <ref> k=v ...` sets/overwrites labels and is idempotent.
  if ctr images label "$ref" "${PINS[@]}" >/dev/null 2>&1; then
    PINNED=$((PINNED + 1))
    log "  pinned $ref"
  else
    warn "  failed to pin $ref"
    FAILED+=("$ref")
  fi
done

if [ "${#FAILED[@]}" -gt 0 ]; then
  die "${#FAILED[@]}/${#REFS[@]} image(s) failed to pin: ${FAILED[*]}"
fi

log "done. pinned ${PINNED}/${#REFS[@]} image(s) (io.cri.containerd.pinned, io.cattle.k3s.pinned) — GC will not reclaim them."
