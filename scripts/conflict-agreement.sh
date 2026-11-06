#!/usr/bin/env bash
# conflict-agreement.sh -- does the PR conflict scanner agree with GitHub?
#
# For every open PR based on main, reads GitHub's own `mergeable` from
# GET /repos/{repo}/pulls/{n} (retrying while GitHub is still computing it, so
# while it is null) and compares it with mr_merge_state on the beads whose
# mr_url is that PR, as github-bridge's scanner writes it (docs/pr-conflicts.md).
# true agrees with clean, false with conflicting; anything else disagrees. A PR
# no bead carries is n/a and not counted: the scanner has nowhere to write.
# Prints a Markdown table (PR, GitHub mergeable, bead state, agree) ready to
# paste into a bead comment. Task gd-xrDytQgUfR.
#
# Usage: scripts/conflict-agreement.sh [owner/name]
#   default repo: mattbakerconsulting/gardenio
# Env:   AGREEMENT_RETRIES (default 10), AGREEMENT_RETRY_SECS (default 3)
# Needs: gh (authenticated), kd, jq.
# Exit:  0 every PR agrees; 1 at least one disagrees or a gh/kd call failed;
#        2 a usage error or a missing tool.
set -euo pipefail

REPO="${1:-mattbakerconsulting/gardenio}"
RETRIES="${AGREEMENT_RETRIES:-10}"
RETRY_SECS="${AGREEMENT_RETRY_SECS:-3}"

for tool in gh kd jq; do
  command -v "$tool" >/dev/null || { echo "conflict-agreement.sh: $tool not on PATH" >&2; exit 2; }
done
[[ "$REPO" == */* ]] || { echo "conflict-agreement.sh: repo must be owner/name, got '$REPO'" >&2; exit 2; }

# github_mergeable <n> -- prints true, false or null (still null after retries).
github_mergeable() {
  local n="$1" m i
  for ((i = 0; i <= RETRIES; i++)); do
    m="$(gh api "repos/${REPO}/pulls/${n}" --jq '.mergeable')"
    [ "$m" != "null" ] && [ -n "$m" ] && { echo "$m"; return; }
    [ "$i" -lt "$RETRIES" ] && sleep "$RETRY_SECS"
  done
  echo null
}

# bead_states <url> -- the distinct mr_merge_state values on the PR's beads,
# comma-joined; "-" when no bead carries the mr_url, "(unset)" for a bead the
# scanner has not written.
bead_states() {
  kd list --all-types --all-projects -f "mr_url=$1" \
    -s open -s in_progress -s blocked -s deferred -s closed --limit 50 --json |
    jq -r '[.[] | .fields.mr_merge_state // "(unset)"] | unique
           | if length == 0 then "-" else join(",") end'
}

prs="$(gh pr list --repo "$REPO" --state open --base main --limit 500 --json number --jq '.[].number' | sort -n)"

echo "| PR | GitHub mergeable | bead state | agree |"
echo "|---|---|---|---|"
total=0 agreed=0
for n in $prs; do
  url="https://github.com/${REPO}/pull/${n}"
  gm="$(github_mergeable "$n")"
  bs="$(bead_states "$url")"
  case "$gm/$bs" in
    */-) ok=n/a ;;
    true/clean | false/conflicting) ok=yes; agreed=$((agreed + 1)) ;;
    *) ok=no ;;
  esac
  [ "$ok" = n/a ] || total=$((total + 1))
  echo "| #${n} | ${gm} | ${bs} | ${ok} |"
done
echo
echo "${agreed}/${total} PRs with a bead agree ($(date -u +%Y-%m-%dT%H:%MZ), ${REPO})"
[ "$agreed" -eq "$total" ]
