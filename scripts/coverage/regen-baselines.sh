#!/usr/bin/env bash
# regen-baselines.sh — rebuild the patch-coverage baselines from a window of
# green release-tag and nightly ci.yml runs (gd-roE5roXBKQ, gd-X7nrzthzhp).
#
# Usage: regen-baselines.sh [runs] [output]
#   runs    how many green release-tag or nightly ci.yml runs to take (default 10)
#   output  default scripts/ratchets/coverage-baselines.json
#
# ci.yml runs on a calver tag push (tags 20*) and the nightly schedule on
# main, not on main pushes; on those two events its `go` job uploads each
# module's LCOV as the artifact coverage-lcov-<module> (14 days). This takes
# the newest green runs of either event (any other run is skipped), downloads
# the artifacts from those that still have one, concatenates each run's arms
# into <run-id>.lcov (the parser merges records per file) and hands the window to
# cmd/coverage-baselines, which takes the median per component and sets each
# threshold to max(60, median - 10). Commit the output in its own PR and quote
# the printed baselines there; the gate's bar moves only when someone does.
#
# Needs gh (signed in, actions:read) and go. Env: REPO (default: the origin
# remote's owner/name), GH (default gh), for tests.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
RUNS="${1:-10}"
OUTPUT="${2:-$ROOT/scripts/ratchets/coverage-baselines.json}"
GH="${GH:-gh}"
if [[ -z "${REPO:-}" ]]; then
  REPO=$(git -C "$ROOT" remote get-url origin | sed -E 's#(\.git)?$##; s#.*[:/]([^/]+/[^/]+)$#\1#')
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/window"

# Over-fetch: runs older than the artifacts' retention have none. A tag push
# run's headBranch is the tag; the filter re-checks event and ref so only a
# release tag or a scheduled main run gets in. Newest first across both.
for event in push schedule; do
  "$GH" run list --repo "$REPO" --workflow ci.yml --event "$event" \
    --status success --limit $((RUNS * 4)) --json databaseId,headSha,headBranch,event,createdAt \
    --jq '.[] | "\(.createdAt)\t\(.databaseId)\t\(.headSha)\t\(.event)\t\(.headBranch)"'
done | awk -F'\t' '($4 == "push" && $5 ~ /^20/) || ($4 == "schedule" && $5 == "main")' |
  sort -r | cut -f2,3 >"$work/runs.tsv"

got=0
while IFS=$'\t' read -r id sha; do
  [[ $got -lt $RUNS ]] || break
  if ! "$GH" run download "$id" --repo "$REPO" --pattern 'coverage-lcov-*' \
       --dir "$work/dl/$id" >/dev/null 2>&1; then
    echo "run $id: no coverage artifacts, skipped" >&2
    continue
  fi
  shopt -s nullglob globstar
  lcovs=("$work/dl/$id"/**/*.lcov)
  shopt -u nullglob globstar
  if [[ ${#lcovs[@]} -eq 0 ]]; then
    echo "run $id: artifacts hold no .lcov, skipped" >&2
    continue
  fi
  cat "${lcovs[@]}" >"$work/window/$id.lcov"
  printf '%s\t%s\n' "$id" "$sha" >>"$work/commits.tsv"
  got=$((got + 1))
done <"$work/runs.tsv"

if [[ $got -eq 0 ]]; then
  echo "error: no green release-tag or nightly ci.yml run of $REPO has coverage-lcov-* artifacts yet" >&2
  exit 1
fi
if [[ $got -lt $RUNS ]]; then
  echo "warning: only $got of $RUNS runs had artifacts; the median is over $got" >&2
fi

if [[ -n "${COVERAGE_BASELINES_BIN:-}" ]]; then
  "$COVERAGE_BASELINES_BIN" --inputs-dir "$work/window" --commits "$work/commits.tsv" --output "$OUTPUT"
else
  (cd "$ROOT/gardenio/controller" &&
    go run ./cmd/coverage-baselines --inputs-dir "$work/window" --commits "$work/commits.tsv" --output "$OUTPUT")
fi
