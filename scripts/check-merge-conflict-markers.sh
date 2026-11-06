#!/usr/bin/env bash
# check-merge-conflict-markers.sh
#
# LOUD guard against committing unresolved Git merge conflict markers.
#
# WHY THIS EXISTS (ka-0000001064): a botched conflict resolution landed the
# literal marker triad
#     <<<<<<< HEAD
#     =======
#     >>>>>>> cb955cac8 (...)
# into deploy-values/gardenio-rwx-ephemeral-{prod,poc}.yaml.tmpl on main
# (commit be105c652, MR !6927 — itself the fix for the .26 realm 404). The
# conflict was purely inside a YAML *comment*, so it was invisible in review,
# but at tag-cut time the RWX pipeline expanded the overlay and helm parsed
# it — and the marker lines are invalid YAML:
#     Error: failed to parse ...-values.yaml: error converting YAML to JSON:
#     yaml: line 1296: could not find expected ':'
# That hard-fails `deploy-ephemeral` and skip-cascades e2e on EVERY tag after
# the marker landed (.29/.30/.31 all red). The existing realm-consistency guard
# (ka-0000007361) did NOT catch it: yq cannot parse a marker-corrupted file, so
# the guard read `realm.create != true` and silently SKIPed the overlay.
#
# A merge conflict marker is never legitimate content in a tracked source file,
# so a whole-tree scan is the durable fix. Matching the opener (`<<<<<<< `) and
# closer (`>>>>>>> `) markers — each 7 sigil chars followed by a space or EOL —
# has zero false positives on real content (verified against the whole tree).
# The bare `=======` divider is intentionally NOT matched on its own to avoid
# false-positiving Markdown setext H1 underlines; a real conflict always carries
# an opener and a closer, either of which trips this guard.
#
# Usage:
#   ./scripts/check-merge-conflict-markers.sh            # scan all tracked files
#   ./scripts/check-merge-conflict-markers.sh <file..>   # scan specific files
#
# Exit status: 0 if no marker found, 1 otherwise.
#
# Companion test: scripts/check-merge-conflict-markers_test.sh
# Task: ka-0000001064.

set -euo pipefail

# The opener and closer marker lines. Each is 7 identical sigil characters
# followed by a space (git always appends a ref/label) or end-of-line. This
# script itself and its companion test reference the markers only inside quoted
# regex/here-doc fragments that never begin a line with 7 raw sigils, so a
# whole-tree scan does not flag the guard's own source.
OPENER_RE='^<<<<<<<( |$)'
CLOSER_RE='^>>>>>>>( |$)'

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

# Filter with bash builtins only -- `[[ -f ]]` costs no process -- then hand
# the whole list to ONE grep.
#
# This loop used to spawn a binary probe AND a match pass for every tracked
# file: ~15,300 processes on this 7,654-file tree. Measured 17.5s locally and
# 21-185s as the dominant step of the `merge-conflict-marker guard` job on a
# contended medium runner, which made a guard that reads 30 MB of text the 9th
# largest consumer of the whole CI pool. Nearly all of it was fork/exec, not
# scanning.
scan=()
for f in "${files[@]}"; do
  [[ -z "$f" ]] && continue
  [[ -f "$f" ]] || continue
  scan+=("$f")
done

violations=0
if [[ "${#scan[@]}" -gt 0 ]]; then
  # The grep flags:
  #   -I  treat binary files as non-matching, replacing the removed NUL probe.
  #       Measured in the runner image (GNU grep 3.11): on a NUL-bearing file
  #       whose bytes contain a marker line, WITHOUT -I grep prints
  #       `grep: f: binary file matches` to stderr and exits 0; WITH -I it
  #       prints nothing and exits 1. So on grep >= 3.5 the flag changes only
  #       the noise. It is kept for grep < 3.5, where that line went to STDOUT
  #       and this loop would have counted it as a violation against a path
  #       that does not contain one.
  #       NOTE: the companion test's binary case pins the BEHAVIOUR (a binary
  #       blob carrying marker bytes must not red the build), not this flag --
  #       dropping -I leaves that case green on both greps tried.
  #   -H  force the filename prefix. xargs splits the list into batches and the
  #       last batch can hold a single file, where grep would otherwise drop
  #       the name and the report would read `CONFLICT MARKER: 12:<<<<<<< HEAD`.
  #   --  end of options, so a path starting with `-` is a file, not a flag.
  # grep's stderr is deliberately NOT discarded: `[[ -f ]]` above already rules
  # out the directory args that produced most of the old noise, so what is left
  # is an unreadable file -- a silently unscanned one -- and that belongs in the
  # job log. xargs exits 123 when a batch matched nothing, which is the healthy
  # case, so the status is discarded and violations come from the output.
  while IFS= read -r hit; do
    printf 'CONFLICT MARKER: %s\n' "$hit" >&2
    violations=$((violations + 1))
  done < <(printf '%s\0' "${scan[@]}" \
    | xargs -0 grep -HnIE -e "${OPENER_RE}" -e "${CLOSER_RE}" -- \
    || true)
fi

if [[ "$violations" -gt 0 ]]; then
  cat >&2 <<'EOF'

Found unresolved Git merge conflict marker(s) in tracked files.
These are never valid source content and break YAML/helm/compile at the worst
possible time — a marker in a deploy-values overlay skip-cascades the entire
release e2e run (ka-0000001064). Resolve the conflict and delete every
conflict-marker line (`<<<<<<<`, `=======`, `>>>>>>>`) before committing.
EOF
  exit 1
fi

# Report the count, not just "OK". A guard whose file list silently collapsed
# to zero -- a renamed path, a `git ls-files` run outside the repo, a bad
# filter -- otherwise reports success in exactly the same words as a real
# clean scan. The companion test asserts a floor on this number.
echo "OK: no unresolved merge conflict markers (scanned ${#scan[@]} files)"
