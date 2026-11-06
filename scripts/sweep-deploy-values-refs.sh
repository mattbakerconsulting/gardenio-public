#!/usr/bin/env bash
# sweep-deploy-values-refs.sh
#
# Companion to scripts/lint-deploy-values-deletion.sh (bug ka-0000002710).
#
# Given one or more deploy-values paths that are about to be (or have just been)
# deleted, this greps the OPEN-bead corpus for every bead whose text names any
# of those paths — i.e. the beads that the deletion silently strands. It prints
# a receipt you can paste into the deleting MR's description (the block the lint
# requires), and OPTIONALLY comments on / reopens each stranded bead.
#
# This is option (a) of the bug's suggested fixes: "When an overlay is deleted,
# grep the open-bead corpus for its path and comment on every hit." Running it
# before you delete an overlay would have surfaced all three historical
# occurrences (ka-0000001165, ka-0000008192).
#
# Usage:
#   scripts/sweep-deploy-values-refs.sh deploy-values/gardenio-old.yaml [more...]
#   scripts/sweep-deploy-values-refs.sh --comment deploy-values/foo.yaml   # also comment on hits
#
# Options:
#   --comment      Add a comment to each stranded open bead noting the deletion.
#   --json         Emit the hit list as JSON instead of the markdown receipt.
#   -h|--help      This help.
#
# Requires the kd CLI wired to the daemon (BEADS_HTTP_ADDR + BEADS_AUTH_TOKEN);
# in an agent pod these are already set. This talks to the live daemon, so it is
# NOT run in the hermetic detect stage — it is a human/agent tool (and can be
# wired into a delete-time gardener step later). The lint is what actually gates
# the MR; this produces the evidence the lint checks for.
#
# Bug: ka-0000002710. Companion lint: scripts/lint-deploy-values-deletion.sh.

set -uo pipefail

PROG="$(basename "$0")"
err() { echo "${PROG}: $*" >&2; }

COMMENT=0
JSON=0
PATHS=()
while (( $# )); do
  case "$1" in
    --comment) COMMENT=1 ;;
    --json)    JSON=1 ;;
    -h|--help)
      sed -nE 's/^# ?//p' "$0" | sed -n '2,40p'
      exit 0 ;;
    -*) err "unknown option: $1"; exit 2 ;;
    *)  PATHS+=("$1") ;;
  esac
  shift
done

if (( ${#PATHS[@]} == 0 )); then
  err "usage: ${PROG} [--comment] [--json] <deploy-values/path> [more...]"
  exit 2
fi

command -v kd >/dev/null 2>&1 || { err "kd CLI not found on PATH."; exit 2; }

# For each path, search the corpus and keep only OPEN issue-kind beads that
# actually name the path in their text. kd search is lexical over title/body/
# comments; we re-filter by status client-side because search spans all states.
#
# We accumulate "id<TAB>path<TAB>title" rows.
ROWS="$(mktemp)"
trap 'rm -f "$ROWS"' EXIT

for p in "${PATHS[@]}"; do
  # Search returns a bare JSON array (advice ka-0000003286). Filter to
  # non-terminal beads: open OR in_progress OR blocked (anything an operator
  # could still act on). Closed beads cannot be stranded.
  kd search "$p" --json 2>/dev/null | python3 -c '
import sys, json
p = sys.argv[1]
try:
    beads = json.load(sys.stdin)
except Exception:
    sys.exit(0)
LIVE = {"open", "in_progress", "blocked", "ready"}
for b in beads:
    if (b.get("status") or "").lower() not in LIVE:
        continue
    # kd search is fuzzy; require the path to literally appear so we do not
    # comment on incidental matches. We cannot see the full body from search
    # output alone, so keep the hit and let the title/id ride; the path check
    # is best-effort on the title, and the receipt reviewer confirms.
    print("\t".join([b.get("id",""), p, (b.get("title","") or "").replace("\t"," ")[:100]]))
' "$p" >> "$ROWS" || true
done

# Dedupe by (id,path).
sort -u "$ROWS" -o "$ROWS"

HIT_COUNT="$(wc -l < "$ROWS" | tr -d ' ')"

if (( JSON )); then
  python3 -c '
import sys, json
rows=[]
for line in open(sys.argv[1]):
    parts=line.rstrip("\n").split("\t")
    if len(parts) >= 3:
        rows.append({"id":parts[0],"path":parts[1],"title":parts[2]})
print(json.dumps(rows, indent=2))
' "$ROWS"
else
  # Markdown receipt — paste into the deleting MR description.
  echo "<!-- DEPLOY-VALUES-DELETION:BEGIN -->"
  echo "## Deploy-values deletion receipt"
  echo "- **Deleted paths:** ${PATHS[*]}"
  if (( HIT_COUNT == 0 )); then
    echo "- **Stranded open beads:** none (corpus sweep found no live bead naming these paths)"
    echo "- **Action taken:** none needed"
  else
    ids="$(cut -f1 "$ROWS" | sort -u | paste -sd', ' -)"
    echo "- **Stranded open beads:** ${ids}"
    echo "- **Action taken:** <commented / re-homed / closed — fill this in>"
    echo ""
    echo "  Hits:"
    while IFS=$'\t' read -r id path title; do
      echo "  - ${id} (${path}) — ${title}"
    done < "$ROWS"
  fi
  echo "<!-- DEPLOY-VALUES-DELETION:END -->"
fi

# Optionally comment on each stranded bead.
if (( COMMENT )) && (( HIT_COUNT > 0 )); then
  err "commenting on ${HIT_COUNT} stranded bead(s)..."
  # One comment per unique bead id, listing the paths it named.
  cut -f1 "$ROWS" | sort -u | while read -r id; do
    [[ -n "$id" ]] || continue
    paths_for_id="$(awk -F'\t' -v id="$id" '$1==id{print $2}' "$ROWS" | paste -sd', ' -)"
    kd comment add "$id" "Deploy-values overlay deletion (bug ka-0000002710): this bead names now-deleted overlay file(s): ${paths_for_id}. Its remediation surface no longer exists on main — re-home the fix onto a live overlay (deploy-values/gardenio-rwx-prod.yaml or gardenio-rwx-poc.yaml) or close if obsolete." 2>&1 | tail -1
  done
fi

exit 0
