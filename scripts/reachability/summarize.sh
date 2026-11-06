#!/usr/bin/env bash
# summarize.sh — render the reachability artifact as one Markdown summary
# (gd-Nd2ciHTvTE).
#
# Usage: summarize.sh <artifact-dir> [top-n] > SUMMARY.md
#
# <artifact-dir> holds what deadcode.sh, coverage-by-package.sh and
# code-size.sh wrote:
#   <module-slug>/summary-*.tsv, packages-unshipped.txt, packages-unused.txt
#   coverage/<module-slug>.tsv
#   size/components.tsv
# top-n bounds the least-covered table per module (default 25); the full
# tables stay in the artifact.

set -euo pipefail
export LC_ALL=C

if [[ $# -lt 1 ]]; then
  echo "usage: $0 <artifact-dir> [top-n]" >&2
  exit 2
fi
DIR=$1
TOP=${2:-25}

lines() { if [[ -f "$1" ]]; then wc -l <"$1" | tr -d ' '; else echo 0; fi; }

echo "# Reachability data"
echo
echo "Dead = no call path from that main reaches the function (x/tools deadcode, RTA)."
echo "Test-only = dead from the main, but reached once every library package's tests are roots."
echo "Full lists: \`<module>/dead/\`, \`<module>/test-only/\`, \`coverage/\` in this artifact."

for mod in "$DIR"/*/; do
  mod=${mod%/}
  name=$(basename "$mod")
  compgen -G "$mod/summary-*.tsv" >/dev/null || continue
  echo
  echo "## $name"
  echo
  echo "| main | part | dead funcs | test-only funcs |"
  echo "|---|---|---:|---:|"
  # Shipped mains, then their union, then every other main.
  for part in shipped union other; do
    awk -F '\t' -v part="$part" '$2 == part { print }' "$mod"/summary-*.tsv
  done
done | awk -F '\t' 'NF == 4 { printf "| `%s` | %s | %s | %s |\n", $1, $2, $3, $4; next } { print }'

for mod in "$DIR"/*/; do
  mod=${mod%/}
  name=$(basename "$mod")
  [[ -f "$mod/packages-unshipped.txt" ]] || continue
  echo
  echo "### $name: whole packages"
  echo
  echo "- imported by no shipped main: $(lines "$mod/packages-unshipped.txt") (\`$name/packages-unshipped.txt\`)"
  echo "- imported by no main at all: $(lines "$mod/packages-unused.txt") (\`$name/packages-unused.txt\`)"
  if compgen -G "$mod/errors-*.txt" >/dev/null; then
    echo "- deadcode errors: see \`$name/errors-*.txt\`"
  fi
done

if compgen -G "$DIR/coverage/*.tsv" >/dev/null; then
  echo
  echo "## Coverage by package (statements)"
  for cov in "$DIR"/coverage/*.tsv; do
    name=$(basename "$cov" .tsv)
    echo
    echo "### $name: $(awk -F '\t' '$1 == "(total)" { printf "%s%% of %s statements", $4, $2 }' "$cov")"
    echo
    echo "Least covered $TOP of $(($(lines "$cov") - 1)) packages (full table: \`coverage/$name.tsv\`):"
    echo
    echo "| package | statements | covered | % |"
    echo "|---|---:|---:|---:|"
    awk -F '\t' -v top="$TOP" '$1 != "(total)" && n < top { n++; printf "| `%s` | %s | %s | %s |\n", $1, $2, $3, $4 }' "$cov"
  done
fi

if [[ -f "$DIR/size/components.tsv" ]]; then
  echo
  echo "## Code size by component"
  echo
  echo "Lines (generated Go left out), Go funcs over 100 lines (long) and over gocyclo 30"
  echo "(complex). Targets: \`gardenio/docs/code-reduction-targets.md\`. Lists: \`size/*.tsv\`."
  echo
  echo "| component | go src | go test | long src | long test | complex src | complex test | rust src | rust test |"
  echo "|---|---:|---:|---:|---:|---:|---:|---:|---:|"
  awk -F '\t' 'NR > 1 { printf "| `%s` |", $1; for (i = 2; i <= NF; i++) printf " %s |", $i; printf "\n" }' "$DIR/size/components.tsv"
fi
