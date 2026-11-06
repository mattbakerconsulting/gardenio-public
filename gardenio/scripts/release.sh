#!/usr/bin/env bash
set -euo pipefail

# release.sh — Compute next calver tag and bump Chart.yaml
#
# Usage:
#   scripts/release.sh              # bump, lint, commit, tag
#   scripts/release.sh --dry-run    # show what would happen

# Resolve paths from the repo root rather than CWD: this script is invoked
# both as `./gardenio/scripts/release.sh` (from the monorepo root, e.g. via
# `make release`) and as `./release.sh` (from inside gardenio/), and a
# CWD-relative CHART path silently mismatched the real file in the former
# case (gd-YNvE0ZsPj5: tag 2026.263.1 was cut with Chart.yaml never bumped).
REPO_ROOT="$(git rev-parse --show-toplevel)"
CHART="${REPO_ROOT}/gardenio/helm/gardenio/Chart.yaml"
CHART_DIR="${REPO_ROOT}/gardenio/helm/gardenio"
DRY_RUN=false

for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY_RUN=true ;;
    *) echo "Unknown flag: $arg"; exit 1 ;;
  esac
done

# shellcheck source=SCRIPTDIR/release-role-guard.sh
. "$(dirname "${BASH_SOURCE[0]}")/release-role-guard.sh"
release_role_guard release.sh "$DRY_RUN"

if [ ! -f "$CHART" ]; then
  echo "release.sh: chart not found at ${CHART} -- refusing to tag" >&2
  exit 1
fi

# --- Compute next calver tag: YYYY.DDD.N ---

YEAR=$(date -u +%Y)
DOY=$(date -u +%-j)  # day-of-year without leading zeros
PREFIX="${YEAR}.${DOY}"

# Find highest build number for today's prefix among existing git tags.
# Check both bare (2026.58.N) and v-prefixed (v2026.58.N) tags.
LAST_N=$({ git tag -l "${PREFIX}.*"; git tag -l "v${PREFIX}.*"; } 2>/dev/null \
  | sed -E "s/^v?${PREFIX}\.//" \
  | sort -n \
  | tail -1)

if [ -z "$LAST_N" ]; then
  NEXT_N=1
else
  NEXT_N=$((LAST_N + 1))
fi

TAG="${PREFIX}.${NEXT_N}"

echo "Next release: ${TAG}"

if $DRY_RUN; then
  echo "(dry-run) Would update ${CHART}:"
  echo "  version: ${TAG}"
  echo "  appVersion: \"${TAG}\""
  echo "(dry-run) Would commit: chore: release ${TAG}"
  echo "(dry-run) Would create git tag: ${TAG}"
  exit 0
fi

# --- Bump Chart.yaml ---

sed -i.bak -E "s/^version: .+/version: ${TAG}/" "$CHART"
sed -i.bak -E "s/^appVersion: .+/appVersion: \"${TAG}\"/" "$CHART"
rm -f "${CHART}.bak"

echo "Updated ${CHART}"

# --- Lint ---

helm lint "$CHART_DIR"
echo "Helm lint passed"

# --- Commit & tag ---

git add "$CHART"
git commit -m "chore: release ${TAG}"
git tag "$TAG"

echo ""
echo "Created commit and tag: ${TAG}"
# Use --atomic so main + tag land in one transaction: a bare
# 'git push origin main ${TAG}' pushes the refs independently, and if the main
# update is rejected the tag STILL lands, orphaning it off-main (ka-0000001299).
echo "To publish: git push --atomic origin main ${TAG}"
