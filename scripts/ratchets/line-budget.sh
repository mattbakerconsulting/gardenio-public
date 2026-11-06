#!/usr/bin/env bash
# line-budget.sh — per-package line budget, the ratchet that keeps the
# audit-and-shrink plan's line counts from drifting back up (gd-d29n0m678l).
#
# Usage:
#   line-budget.sh check  [budget-file]   fail if a Go package grew past its budget
#   line-budget.sh update [budget-file]   rewrite the budget from the tree; never raises a row
#
# budget-file defaults to scripts/ratchets/line-budget.tsv; its grants are the
# *.tsv files in the directory beside it named for it (line-budget.d). The tree measured
# is the git checkout that holds it. Lines come from
# scripts/reachability/code-size.sh (its packages.tsv), the same counter the
# reduction targets use: physical lines of tracked Go files, generated code
# left out, src and _test.go counted apart.
#
# Budget file: "package<TAB>src<TAB>test" rows, "#" lines are comments. A
# package is a directory holding Go files. A grant file has the same rows, and
# each adds its lines to the package's row (or to the 500/500 of a package with
# none). A PR that has to raise a row adds line-budget.d/<bead-id>.tsv instead of
# editing the shared row, so PRs that grow one package side by side touch
# different files and git merges them (gd-jMdrPDGpPw).
#
# THE RULE (gardenio/docs/code-reduction-targets.md, "Ratchets"):
#   * the budget of a measured count n is n + max(5% of n, 500), rounded up to
#     a multiple of 100. Every feature PR runs this check, so each package
#     carries that much room before it goes red.
#   * a package not in the file is held to the budget of 0 lines: 500 src,
#     500 test. A new package past that needs a row (run `update`).
#   * `update` sets each row to min(row + its grants, budget of today's count),
#     adds new packages, drops gone ones and deletes the grant files it folded
#     in. It never raises a row past row + grants, so deletions only ever
#     tighten the budget. To raise one, add a grant file and say why in the PR.
#
# Env: CODE_SIZE (default scripts/reachability/code-size.sh next to this
# script), for tests.

set -euo pipefail
export LC_ALL=C

HERE=$(cd "$(dirname "$0")" && pwd)
MODE=${1:-}
BUDGET=${2:-$HERE/line-budget.tsv}
CODE_SIZE=${CODE_SIZE:-$HERE/../reachability/code-size.sh}

case "$MODE" in
  check | update) ;;
  *) echo "usage: $0 <check|update> [budget-file]" >&2; exit 2 ;;
esac
if [[ "$MODE" == check && ! -f "$BUDGET" ]]; then
  echo "line-budget: no budget file at $BUDGET" >&2
  exit 2
fi

ROOT=$(git -C "$(dirname "$BUDGET")" rev-parse --show-toplevel)
GRANTS=${BUDGET%.tsv}.d
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# No gocyclo: only the line counts are read, and skipping it keeps this cheap.
if ! GOCYCLO=line-budget-no-gocyclo "$CODE_SIZE" "$ROOT" "$TMP/size" 2>"$TMP/code-size.log"; then
  cat "$TMP/code-size.log" >&2
  exit 1
fi

# One awk for both modes: the budget file first (if any), its grants, then
# packages.tsv.
touch "$TMP/budget" "$TMP/grants"
[[ -f "$BUDGET" ]] && cp "$BUDGET" "$TMP/budget"
shopt -s nullglob
grants=("$GRANTS"/*.tsv)
shopt -u nullglob
((${#grants[@]})) && awk 1 "${grants[@]}" >"$TMP/grants"
awk -F '\t' -v mode="$MODE" '
  function budget(n,   b) { b = n * 0.05; if (b < 500) b = 500; b = n + b; return int((b + 99) / 100) * 100 }
  function min(a, b) { return a < b ? a : b }
  FILENAME == ARGV[1] { if ($0 !~ /^#/ && NF >= 3) { have[$1] = 1; bs[$1] = $2; bt[$1] = $3 } next }
  FILENAME == ARGV[2] { if ($0 !~ /^#/ && NF >= 3) { gs[$1] += $2; gt[$1] += $3 } next }
  {
    p = $1; s = $2; t = $3; seen[p] = 1
    if (mode == "update") {
      ns = budget(s); nt = budget(t)
      if (p in have) { ns = min(ns, bs[p] + gs[p]); nt = min(nt, bt[p] + gt[p]) }
      printf "%s\t%d\t%d\n", p, ns, nt
      next
    }
    cs = ((p in have) ? bs[p] : budget(0)) + gs[p]; ct = ((p in have) ? bt[p] : budget(0)) + gt[p]
    tag = (p in have) ? "" : " (new package, no row)"
    if (s > cs) printf "%s\tsrc\t%d\t%d%s\n", p, s, cs, tag
    if (t > ct) printf "%s\ttest\t%d\t%d%s\n", p, t, ct, tag
  }
' "$TMP/budget" "$TMP/grants" "$TMP/size/packages.tsv" >"$TMP/out"

if [[ "$MODE" == update ]]; then
  {
    cat <<'HDR'
# Per-package line budget (gd-d29n0m678l). Checked by
# scripts/ratchets/line-budget.sh in checks.yml's quench job; the rule is in
# that script's header and in gardenio/docs/code-reduction-targets.md.
# Regenerate with `scripts/ratchets/line-budget.sh update` (it only ever
# lowers a row, and folds in the grants). Do not raise a row here: add
# line-budget.d/<bead-id>.tsv with the same columns, holding the lines your PR
# adds, and say why in the PR.
# package	src	test
HDR
    sort "$TMP/out"
  } >"$TMP/new"
  mv "$TMP/new" "$BUDGET"
  ((${#grants[@]})) && rm -f "${grants[@]}"
  echo "line-budget: wrote $(wc -l <"$TMP/out" | tr -d ' ') packages to $BUDGET, folding in ${#grants[@]} grant file(s)"
  exit 0
fi

n=$(awk -F '\t' '$1 != "" { c++ } END { print c + 0 }' "$TMP/size/packages.tsv")
if [[ ! -s "$TMP/out" ]]; then
  echo "line-budget: all $n Go packages are within budget ($BUDGET)"
  exit 0
fi
echo "line-budget: over budget (package, kind, lines, budget):"
sort "$TMP/out" | while IFS=$'\t' read -r p kind lines cap; do
  echo "  $p  $kind  $lines > $cap"
  if [[ -n "${GITHUB_ACTIONS:-}" ]]; then
    echo "::error::line budget: $p $kind is $lines lines, budget $cap"
  fi
done
cat <<EOF

Cut lines from the package, or, if it really has to grow, add
${GRANTS#"$ROOT"/}/<bead-id>.tsv with a "package<TAB>src<TAB>test" row
holding the lines it adds (0 for the side that did not grow), and say why in
the PR. Do not edit the shared row: every concurrent PR conflicts on it. A new
package gets a row from \`scripts/ratchets/line-budget.sh update\`. The rule:
gardenio/docs/code-reduction-targets.md, "Ratchets".
EOF
exit 1
