#!/usr/bin/env bash
set -euo pipefail

# verify-image-tags.sh — Verify all gardenio pod images match the expected calver tag.
#
# Usage:
#   scripts/verify-image-tags.sh 2026.62.1
#   scripts/verify-image-tags.sh 2026.62.1 --namespace=gardenio-staging
#   scripts/verify-image-tags.sh 2026.62.1 --allow-tag=e27ec943...   (repeatable)
#
# --allow-tag: a tag deploy-values pins EXPLICITLY for one component (e.g.
# beads.image.tag). Such a pod runs that tag, not the calver, by design, so
# it passes. release-deploy.sh passes every non-global *.image.tag in the
# overlay it deploys.
#
# Pods owned by a Job (CronJob runs, helm hooks, the release-deployer's own
# run) are reported but not gated. A Job pod started before the upgrade
# carries the previous tag and would otherwise fail every rollout.

EXPECTED_VERSION="${1:-}"
NAMESPACE="${NAMESPACE:-gardenio}"
ALLOW_TAGS=()

if [ -z "$EXPECTED_VERSION" ]; then
  echo "Usage: $0 <expected-calver-version> [--namespace=<ns>]"
  echo "Example: $0 2026.62.1"
  exit 1
fi

shift
for arg in "$@"; do
  case "$arg" in
    --namespace=*) NAMESPACE="${arg#*=}" ;;
    --allow-tag=*) ALLOW_TAGS+=("${arg#*=}") ;;
    *) echo "Unknown flag: $arg"; exit 1 ;;
  esac
done

echo "=== Image Tag Verification ==="
echo "Namespace:        ${NAMESPACE}"
echo "Expected version: ${EXPECTED_VERSION}"
echo ""

# Gather all pod names and their container images
POD_IMAGES=$(kubectl get pods -n "$NAMESPACE" \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.containers[*].image}{"\t"}{.metadata.ownerReferences[0].kind}{"\n"}{end}')

if [ -z "$POD_IMAGES" ]; then
  echo "FAIL: No pods found in namespace ${NAMESPACE}"
  exit 1
fi

MISMATCHES=()

# tag_allowed <tag> -- true when <tag> was passed via --allow-tag.
tag_allowed() {
  local t
  for t in ${ALLOW_TAGS[@]+"${ALLOW_TAGS[@]}"}; do
    [ "$t" = "$1" ] && return 0
  done
  return 1
}
CREW_STALE=()
JOB_STALE=()
CHECKED=0

while IFS=$'\t' read -r pod_name images owner_kind; do
  [ -z "$pod_name" ] && continue
  for img in $images; do
    # Only check gardenio-managed images. Match on the "/gardenio/<component>"
    # repo path rather than a hardcoded registry host: the registry has already
    # moved twice (<registry>/gardenio -> the k3s cluster's
    # ECR pull-through cache, 360190250083.dkr.ecr.*.amazonaws.com/ghcr/joyousaversion/gardenio,
    # -> .../ghcr/mattbakerconsulting/gardenio after the 2026-10-13 org move)
    # and a host-anchored match silently matched zero pods after the first move.
    case "$img" in
      */gardenio/*)
        # crew-* pods are not managed by this helm release: their image is
        # fixed at spawn time, and a long-lived crew will always straddle an
        # upgrade. Counting them in the pass/fail gate would block every real
        # deploy on a busy cluster, so they're reported separately.
        case "$pod_name" in
          crew-*)
            TAG="${img##*:}"
            if [ "$TAG" != "$EXPECTED_VERSION" ]; then
              CREW_STALE+=("${pod_name}: ${img}")
            fi
            continue
            ;;
        esac
        if [ "${owner_kind:-}" = "Job" ]; then
          TAG="${img##*:}"
          if [ "$TAG" != "$EXPECTED_VERSION" ]; then
            JOB_STALE+=("${pod_name}: ${img}")
          fi
          continue
        fi
        CHECKED=$((CHECKED + 1))
        TAG="${img##*:}"
        if [ "$TAG" = "$EXPECTED_VERSION" ]; then
          echo "  OK:   ${pod_name}  ${img}"
        elif tag_allowed "$TAG"; then
          echo "  OK:   ${pod_name}  ${img}  (pinned by deploy-values)"
        else
          echo "  FAIL: ${pod_name}  ${img}  (expected tag: ${EXPECTED_VERSION})"
          MISMATCHES+=("${pod_name}: ${img} (expected :${EXPECTED_VERSION})")
        fi
        ;;
      *)
        # Skip non-gardenio images (e.g. pause, coredns)
        ;;
    esac
  done
done <<< "$POD_IMAGES"

echo ""

if [ "$CHECKED" -eq 0 ]; then
  echo "FAIL: No */gardenio/* release-managed images found in namespace ${NAMESPACE}"
  exit 1
fi

if [ ${#CREW_STALE[@]} -gt 0 ]; then
  echo "INFO: ${#CREW_STALE[@]} crew pod(s) on older tags, expected to drain (not release-managed, excluded from the gate):"
  for c in "${CREW_STALE[@]}"; do
    echo "  - $c"
  done
  echo ""
fi

if [ ${#JOB_STALE[@]} -gt 0 ]; then
  echo "INFO: ${#JOB_STALE[@]} Job pod(s) on other tags, one-shot (excluded from the gate):"
  for c in "${JOB_STALE[@]}"; do
    echo "  - $c"
  done
  echo ""
fi

if [ ${#MISMATCHES[@]} -gt 0 ]; then
  echo "FAIL: ${#MISMATCHES[@]} image(s) do not match expected version ${EXPECTED_VERSION}:"
  for m in "${MISMATCHES[@]}"; do
    echo "  - $m"
  done
  exit 1
fi

echo "SUCCESS: All ${CHECKED} release-managed gardenio image(s) are tagged ${EXPECTED_VERSION}"
