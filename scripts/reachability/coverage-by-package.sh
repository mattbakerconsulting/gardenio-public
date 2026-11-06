#!/usr/bin/env bash
# coverage-by-package.sh — summarise a Go cover profile per package
# (gd-Nd2ciHTvTE).
#
# Usage: coverage-by-package.sh <profile> [<profile> ...] > table.tsv
#
# Prints "package<TAB>statements<TAB>covered<TAB>percent", sorted by percent
# ascending (least covered first), then a "(total)" row. A profile from
# `go test -coverpkg=./... ./...` repeats each block once per test binary, so
# blocks are de-duplicated by position and a block counts as covered if any
# binary ran it. Statement counts, not line counts: the same unit `go tool
# cover -func` reports.

set -euo pipefail
export LC_ALL=C

if [[ $# -lt 1 ]]; then
  echo "usage: $0 <profile> [<profile> ...]" >&2
  exit 2
fi

rows=$(mktemp)
trap 'rm -f "$rows"' EXIT

awk '
  FNR == 1 && /^mode:/ { next }
  NF == 3 {
    key = $1
    if (!(key in stmts)) {
      stmts[key] = $2
      file = key; sub(/:[^:]*$/, "", file)
      pkg = file; sub(/\/[^\/]*$/, "", pkg)
      pkgof[key] = pkg
    }
    if ($3 > 0) hit[key] = 1
  }
  END {
    for (k in stmts) {
      p = pkgof[k]
      total[p] += stmts[k]
      if (k in hit) covered[p] += stmts[k]
      all += stmts[k]
      if (k in hit) allc += stmts[k]
    }
    for (p in total) {
      pct = total[p] ? 100 * covered[p] / total[p] : 0
      printf "%s\t%d\t%d\t%.1f\n", p, total[p], covered[p], pct
    }
    printf "(total)\t%d\t%d\t%.1f\n", all, allc, (all ? 100 * allc / all : 0)
  }
' "$@" >"$rows"
grep -v '^(total)' "$rows" | sort -t "$(printf '\t')" -k4,4n -k1,1 || true
grep '^(total)' "$rows"
