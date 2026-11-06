#!/usr/bin/env bash
# check-large-tracked-files.sh
#
# LOUD guard against committing large binary blobs into the monorepo.
#
# WHY THIS EXISTS (ka-0000000431): the agent pod entrypoint (`gb init`) does a
# blobless partial clone (`git clone --filter=blob:none`) of this repo into the
# pod workspace before coop starts (controller initclone.go, epic ka-0000005334).
# The blob:none filter defers *historical* file content, but git still downloads
# the entire *tip-tree snapshot* to populate the working directory on checkout.
# When machine-generated UI-audit media (info-density / maps-beauty /
# gardeners-camp screenshots + DOM dumps) and a stray 78MB compiled `controller`
# binary were committed, the tip tree ballooned to ~1.55GB. On a throttled-egress
# cluster (~0.5MB/s) that clone took >15min and blew the coop startup-probe
# budget, crash-looping every fresh agent pod. The blob:none filter does NOT
# help here: the checkout still backfills every *tip* blob to populate the working
# tree. The only durable fix is to keep the tip tree small — this guard enforces
# that.
#
# It fails LOUDLY if any *tracked* file exceeds MAX_BYTES, unless the path is on
# the explicit allowlist below. Binary media (screenshots, DOM dumps, compiled
# binaries) belong in bead attachments (`gb image upload`, ka-0000002145) or CI
# artifacts — never in the repo agents clone on every cold start.
#
# Usage:
#   ./scripts/check-large-tracked-files.sh            # scan all tracked files
#   ./scripts/check-large-tracked-files.sh <file..>   # scan specific files
#   MAX_BYTES=1048576 ./scripts/check-large-tracked-files.sh
#
# Exit status: 0 if all scanned files are within budget, 1 otherwise.
#
# Companion test: scripts/check-large-tracked-files_test.sh
# Task: ka-0000000431.

set -euo pipefail

# Default ceiling: 3 MiB. Nothing an agent needs to clone on cold start should be
# bigger — source, docs prose, and small fixtures all fit well under this. Tune
# via the MAX_BYTES env var (the companion test drives it down to exercise the
# guard against synthetic fixtures).
MAX_BYTES="${MAX_BYTES:-3145728}"

# Allowlist of tracked paths permitted to exceed MAX_BYTES. Keep this SHORT and
# justified — every entry is a blob every agent pod re-downloads on cold start.
# Match is exact repo-relative path. Prefer removing the file over adding it here.
#
# Empty today. Its one entry, beads-viewer's 3.5 MB mermaid.min.js, is now
# fetched at build time by gardenio/controller/cmd/beads-viewer/fetch-vendored.sh
# and gitignored (gd-wNQzR9ptZI) -- do that for an upstream file before you
# reach for this list.
ALLOWLIST=()

is_allowlisted() {
  local path="$1" allow
  # The ${..+..} form: an empty array under `set -u` is an unbound-variable
  # error in the bash 3.2 a Mac ships.
  for allow in ${ALLOWLIST[@]+"${ALLOWLIST[@]}"}; do
    [[ "$path" == "$allow" ]] && return 0
  done
  return 1
}

# Resolve the file list: explicit args, or all tracked files under git.
files=()
if [[ "$#" -gt 0 ]]; then
  files=("$@")
else
  if ! command -v git >/dev/null 2>&1; then
    echo "TOOL-MISSING: git not on PATH — cannot enumerate tracked files" >&2
    exit 1
  fi
  # -z / mapfile -d '' so paths with spaces are handled correctly.
  mapfile -d '' -t files < <(git ls-files -z)
fi

# Filter with bash builtins only -- `[[ -f ]]` costs no process -- so the
# expensive part is one stat per BATCH rather than one per file.
#
# The old shape ran `size="$(stat ...)"` inside the loop: a subshell plus a
# stat exec for every tracked file, ~15,300 processes on this 7,654-file tree,
# measured at 20.2s locally and worse on a contended runner. It is the same
# fork-per-file pathology as check-merge-conflict-markers.sh, and both of them
# ran on every `checks` run.
scan=()
for f in "${files[@]}"; do
  [[ -z "$f" ]] && continue
  [[ -f "$f" ]] || continue
  scan+=("$f")
done

violations=0

# report <size> <path> -- shared by both branches below.
report() {
  local size="$1" path="$2"
  [[ "$size" -gt "$MAX_BYTES" ]] || return 0
  if is_allowlisted "$path"; then
    return 0
  fi
  printf 'TOO LARGE: %s (%s bytes > %s max)\n' "$path" "$size" "$MAX_BYTES" >&2
  violations=$((violations + 1))
}

if [[ "${#scan[@]}" -eq 0 ]]; then
  :
elif stat --printf='%s' /dev/null >/dev/null 2>&1; then
  # GNU stat (Linux, and every CI runner): one process per xargs batch, with
  # NUL-terminated records so a path containing a newline still reads back as
  # a single record. Verified against GNU coreutils 9.4 in the runner image.
  tab=$'\t'
  while IFS= read -r -d '' rec; do
    report "${rec%%"$tab"*}" "${rec#*"$tab"}"
  done < <(printf '%s\0' "${scan[@]}" | xargs -0 stat --printf='%s\t%n\0' -- || true)
else
  # BSD/macOS stat has no --printf, so fall back to the historical per-file
  # loop: slow (a fork per file) but correct. CI is Linux and never takes this
  # branch; it exists so the guard still works on a Mac.
  for f in "${scan[@]}"; do
    report "$(stat -f %z "$f" 2>/dev/null || wc -c <"$f")" "$f"
  done
fi

if [[ "$violations" -gt 0 ]]; then
  cat >&2 <<EOF

Found ${violations} tracked file(s) over the ${MAX_BYTES}-byte budget.
Large binaries balloon the agent cold-start clone (ka-0000000431). Instead:
  - screenshots / diagrams  -> attach to a bead with 'gb image upload' (ka-0000002145)
  - build artifacts         -> add to .gitignore, produce in CI
  - genuinely-needed asset  -> add its exact path to ALLOWLIST in this script,
                               with a one-line justification.
EOF
  exit 1
fi

# Report the count, not just "OK". A guard whose file list silently collapsed
# to zero reports success in exactly the same words as a real clean scan.
echo "OK: no tracked file exceeds ${MAX_BYTES} bytes (scanned ${#scan[@]} files)"
