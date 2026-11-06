#!/usr/bin/env bash
set -euo pipefail

# release-cut.sh -- tag the head of main, push the tag only.
#
# Never pushes to main, never bumps Chart.yaml, never commits. Tags
# origin/main's HEAD with the next calver when it is newer than the last
# calver release tag on main, and pushes only the tag. See
# gardenio/docs/release-pipeline.md.
#
# IT DOES NOT ASK WHETHER MAIN IS GREEN (owner 2026-11-03: "we don't care for
# ci on main, rather on the release branch"). It used to tag the newest
# commit with a successful ci.yml + checks.yml push run, and main moves every
# few minutes, so the pending main run was nearly always superseded: of the
# last 30 ci.yml main push runs 19 were cancelled and 9 succeeded (checks.yml
# 10 of 15 cancelled), and the cut kept landing on a commit hours old. Main
# pushes no longer run ci.yml or checks.yml at all; the TAG push does, and
# release-deploy.sh's rule 4 rolls a tag only once the tag's own images.yml,
# ci.yml and checks.yml runs are green. A red tag is never rolled: the fix,
# or a quarantine entry, goes into the next cut.
#
# Usage: gardenio/scripts/release-cut.sh [--dry-run]
#
# Exit codes:
#   0 - tag pushed (or would be, under --dry-run)
#   3 - nothing to release (main's HEAD is the last release) -- NORMAL
#   1 - error (a git command failed; the failing command is named)
#   4 - refused: an agent session that is not the release captain
#       (release-role-guard.sh; crews never cut, gd-CVotEQ3vk9)
#   5 - refused: the gb on PATH is too old to cut onto the open release
#       (update gb; gd-6Z9rCUcfqS)
#
# Companion test: gardenio/scripts/release-cut_test.sh

DRY_RUN=false

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run)
      DRY_RUN=true
      shift
      ;;
    *)
      echo "release-cut.sh: unknown flag: $1" >&2
      exit 1
      ;;
  esac
done

# shellcheck source=SCRIPTDIR/release-role-guard.sh
. "$(dirname "${BASH_SOURCE[0]}")/release-role-guard.sh"
release_role_guard release-cut.sh "$DRY_RUN"

# A gb that cannot cut onto the project's open release mints a fresh release
# bead instead, orphaning the open one with its boarded PRs and binding no
# gardener (gd-6Z9rCUcfqS). Record's --no-gardener flag came with the
# gardener binding in 2026.308.5, after cut-open (2026.308.1), so it is the
# probe; a version string would not do, since a plain `go build` reports
# `dev`. Refused before anything is fetched or tagged. With no gb at all
# nothing is minted here (step 5 warns), and the github-bridge tag poll
# records the tag onto the open release.
if command -v gb >/dev/null 2>&1 \
  && ! gb release record --help 2>/dev/null | grep -q -- '--no-gardener'; then
  echo "release-cut.sh: refused: this gb ($(command -v gb)) cannot cut onto the open release, so its record would mint a fresh release and bind no gardener. Update gb (2026.308.5 or newer: scripts/onboard.sh, or scripts/gardenio-version.sh --build) and re-run." >&2
  exit 5
fi

ERRFILE="$(mktemp)"
trap 'rm -f "$ERRFILE"' EXIT

die() {
  echo "release-cut.sh: $*" >&2
  exit 1
}

# check <resultvar> <cmd...> -- runs the command, storing its stdout into
# <resultvar> (a caller variable name, bash "output parameter" style) on
# success. On failure, dies naming the exact failing command plus its
# stderr, so a bad `git` invocation is never mistaken for "no results".
#
# IMPORTANT: check must always be called directly (never as `x="$(check
# ...)"` ). Wrapping it in a command substitution runs it in a throwaway
# subshell, so die()'s `exit` would only kill that subshell instead of the
# whole script -- silently downgrading a hard error into empty output. The
# resultvar-assignment style below is what lets every caller avoid that trap.
check() {
  local __resultvar="$1"
  shift
  local out
  if ! out="$("$@" 2>"$ERRFILE")"; then
    die "'$*' failed: $(cat "$ERRFILE")"
  fi
  printf -v "$__resultvar" '%s' "$out"
}

# --- Step 1: fetch ---------------------------------------------------------

check _UNUSED git fetch origin main --tags

# --- Step 2: is main's HEAD unreleased? -------------------------------------

check RELEASE_SHA git rev-parse "origin/main^{commit}"

check ALL_TAGS git tag -l
CALVER_TAGS="$(printf '%s\n' "$ALL_TAGS" | grep -E '^20[0-9]{2}\.[0-9]+\.[0-9]+$' || true)"

# The last release is the newest calver tag main contains. A calver tag that
# is not on main (cut from a branch) says nothing about what main released.
LAST_TAG=""
LAST_SHA=""
if [ -n "$CALVER_TAGS" ]; then
  while IFS= read -r t; do
    [ -z "$t" ] && continue
    if git merge-base --is-ancestor "$t" origin/main 2>/dev/null; then
      LAST_TAG="$t"
      check LAST_SHA git rev-parse "${t}^{commit}"
      break
    fi
  done < <(printf '%s\n' "$CALVER_TAGS" | sort -rV)
fi

if [ -n "$LAST_SHA" ]; then
  check UNRELEASED git rev-list --first-parent --count "${LAST_SHA}..origin/main"
  if [ "$UNRELEASED" -eq 0 ]; then
    echo "nothing to release"
    exit 3
  fi
  echo "release-cut.sh: ${UNRELEASED} first-parent commit(s) on main since ${LAST_TAG}" >&2
fi

# --- Step 3: compute the next calver tag ------------------------------------
# Copied from release.sh's calver logic (not sourced -- release.sh also
# bumps Chart.yaml and commits, neither of which this script does).

compute_tag() {
  local year doy prefix last_n next_n
  year=$(date -u +%Y)
  doy=$(date -u +%-j) # day-of-year without leading zeros
  prefix="${year}.${doy}"

  last_n=$({ git tag -l "${prefix}.*"; git tag -l "v${prefix}.*"; } 2>/dev/null \
    | sed -E "s/^v?${prefix}\.//" \
    | sort -n \
    | tail -1)

  if [ -z "$last_n" ]; then
    next_n=1
  else
    next_n=$((last_n + 1))
  fi

  TAG="${prefix}.${next_n}"
}

compute_tag

if $DRY_RUN; then
  echo "TAG=${TAG}"
  echo "SHA=${RELEASE_SHA}"
  echo "(dry-run) would create tag ${TAG} at ${RELEASE_SHA} and push refs/tags/${TAG}"
  exit 0
fi

# --- Step 4: tag and push the tag only --------------------------------------

push_tag() {
  local tag="$1"
  if PUSH_OUT="$(git push origin "refs/tags/${tag}" 2>&1)"; then
    return 0
  fi
  return 1
}

FINAL_TAG="$TAG"
check _UNUSED git tag -a "$FINAL_TAG" "$RELEASE_SHA" -m "release ${FINAL_TAG}"

if ! push_tag "$FINAL_TAG"; then
  if grep -qi "already exists" <<<"$PUSH_OUT"; then
    # Race: a peer cut the same day's tag between our compute and our push.
    # Recompute once against the now-current remote tag state and retry once.
    git tag -d "$FINAL_TAG" >/dev/null 2>&1 || true
    check _UNUSED git fetch origin main --tags
    compute_tag
    FINAL_TAG="$TAG"
    check _UNUSED git tag -a "$FINAL_TAG" "$RELEASE_SHA" -m "release ${FINAL_TAG}"
    if ! push_tag "$FINAL_TAG"; then
      die "'git push origin refs/tags/${FINAL_TAG}' failed: ${PUSH_OUT}"
    fi
  else
    die "'git push origin refs/tags/${FINAL_TAG}' failed: ${PUSH_OUT}"
  fi
fi

# --- Step 5: record the release ---------------------------------------------
# `gb release record` writes the type=release bead: the tag, the previous
# release tag and the merged PRs between them, joined to their beads and plans
# (gardenio/docs/releases-design.md, plan gd-UZKZ9H25UD step 3). Best effort:
# the tag is pushed, so a failure is a warning, never a failed cut. Re-running
# it (or `gb release backfill`) fills the record in; it is idempotent.
#
# Then backfill the newest RELEASE_BACKFILL_LAST tags. A cut whose own record
# failed (a gb older than 2026.304.1 has no `release record`, a 403, a daemon
# outage) stays unrecorded until something writes it, and the github-bridge tag
# poll only records tags above the newest recorded one. 2026.303.1, 304.1,
# 304.3 and 305.1 were cut that way and missed (gd-SLPVJe1TRh); the next cut
# from a current gb now fills them. Recorded tags are skipped, and a backfill
# writes no plan comments.
#
# --built marks each of those tags built (cut -> built, built_at and images)
# once its images.yml push run is green, at that run's finish, with one ref per
# image in the tag's own .github/images.json. The tag just cut has no green run
# yet, so the next cut marks it (gd-F7kqCQPhLt, releases-design.md §3.4).
# The registry is the IMAGE_PREFIX images.yml pushes to, read at the tag; with
# none the releases are marked built without image refs.
RELEASE_PROJECT="${RELEASE_PROJECT:-gardenio}"
RELEASE_BACKFILL_LAST="${RELEASE_BACKFILL_LAST:-8}"
if [[ -z "${RELEASE_IMAGE_REGISTRY:-}" ]]; then
  RELEASE_IMAGE_REGISTRY="$(git show "${FINAL_TAG}:.github/workflows/images.yml" 2>/dev/null \
    | sed -n 's/^  IMAGE_PREFIX: *//p' | head -n 1)" || RELEASE_IMAGE_REGISTRY=""
fi
if command -v gb >/dev/null 2>&1; then
  if REC_OUT="$(gb release record "$FINAL_TAG" --project "$RELEASE_PROJECT" 2>&1)"; then
    printf '%s\n' "$REC_OUT" >&2
  else
    echo "release-cut.sh: warning: 'gb release record ${FINAL_TAG} --project ${RELEASE_PROJECT}' failed; the tag is pushed, re-run it: ${REC_OUT}" >&2
  fi
  BF_ARGS=(release backfill --project "$RELEASE_PROJECT" --last "$RELEASE_BACKFILL_LAST")
  BF_BUILT=(--built)
  if [[ -n "$RELEASE_IMAGE_REGISTRY" ]]; then
    BF_BUILT+=(--images-file .github/images.json --images-from-tag --registry "$RELEASE_IMAGE_REGISTRY")
  fi
  BF_OK=true
  if ! BF_OUT="$(gb "${BF_ARGS[@]}" "${BF_BUILT[@]}" 2>&1)"; then
    BF_OK=false
    # A gb older than this script has no --images-from-tag; still record.
    if grep -q "unknown flag" <<<"$BF_OUT"; then
      echo "release-cut.sh: warning: this gb cannot mark releases built (update gb); backfilling the records only" >&2
      BF_OUT="$(gb "${BF_ARGS[@]}" 2>&1)" && BF_OK=true
    fi
  fi
  if "$BF_OK"; then
    # Only the summary line: each tag's contents are already in its record.
    tail -n 1 <<<"$BF_OUT" >&2
  else
    echo "release-cut.sh: warning: 'gb release backfill --project ${RELEASE_PROJECT} --last ${RELEASE_BACKFILL_LAST}' failed; earlier unrecorded or unbuilt cuts stay that way until it is re-run: ${BF_OUT}" >&2
  fi
else
  echo "release-cut.sh: warning: gb is not on PATH; record the release with 'gb release record ${FINAL_TAG} --project ${RELEASE_PROJECT}'" >&2
fi

echo "TAG=${FINAL_TAG}"
echo "SHA=${RELEASE_SHA}"
exit 0
