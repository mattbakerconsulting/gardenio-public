#!/usr/bin/env bash
set -euo pipefail

# garden-goldens-from-ci.sh -- take the golden screenshots CI rendered.
#
# The goldens in gardenio/controller/internal/gardenhub/testdata/golden/ (/garden,
# TestGardenGoldens) and gardenio/controller/cmd/beads-viewer/testdata/golden/
# (the empty composer, TestComposerGoldens; its lands/ subdirectory, the Lands
# front of house, TestLandsGoldens; its arms/ subdirectory, the pictures of
# arms, TestArmsGoldens) are rendered by Chromium on Linux,
# and both tests skip anywhere else, so a macOS laptop cannot run
# `make update-garden-goldens` itself. When the `gardenhub browser (chromium)`
# job of ci.yml fails on goldens, it uploads a `garden-goldens` artifact holding
# <gate>/<shot>.actual.png for every failing shot; this script downloads it and
# copies each actual over its golden. Look at the
# diffs first: the point of the gate is that a change was meant.
#
# A golden whose surface was removed is not in the artifact: delete it by hand
# (the CI failure names it).
#
# Usage: gardenio/scripts/garden-goldens-from-ci.sh [run-id]
#   run-id defaults to the newest ci.yml run on the current branch.
#
# Exit codes: 0 goldens copied; 1 error, or the run uploaded no actuals.
# Task: gd-D953GccBTY. How-to: gardenio/tests/gardenhub-browser/GOLDENS.md.

root=$(git rev-parse --show-toplevel)
# Artifact subdirectory (golden.ArtifactsDir) -> golden directory.
declare -A dest=(
  [garden]="$root/gardenio/controller/internal/gardenhub/testdata/golden"
  [composer]="$root/gardenio/controller/cmd/beads-viewer/testdata/golden"
  [lands]="$root/gardenio/controller/cmd/beads-viewer/testdata/golden/lands"
  [arms]="$root/gardenio/controller/cmd/beads-viewer/testdata/golden/arms"
)
run="${1:-}"
if [[ -z "$run" ]]; then
  branch=$(git rev-parse --abbrev-ref HEAD)
  run=$(gh run list --workflow ci.yml --branch "$branch" --limit 1 --json databaseId --jq '.[0].databaseId')
  [[ -n "$run" ]] || { echo "no ci.yml run on branch $branch" >&2; exit 1; }
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
gh run download "$run" --name garden-goldens --dir "$tmp"

n=0
for gate in "${!dest[@]}"; do
  for f in "$tmp/$gate"/*.actual.png; do
    [[ -e "$f" ]] || continue
    name=$(basename "$f" .actual.png).png
    mkdir -p "${dest[$gate]}"
    cp "$f" "${dest[$gate]}/$name"
    echo "updated $gate/$name"
    n=$((n + 1))
  done
done
if [[ "$n" -eq 0 ]]; then
  echo "run $run uploaded no actual screenshots" >&2
  exit 1
fi
echo "$n golden(s) copied from run $run; review with git diff --stat, then commit them in this PR"
