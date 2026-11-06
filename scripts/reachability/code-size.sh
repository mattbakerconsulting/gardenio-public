#!/usr/bin/env bash
# code-size.sh — measure code size per component, the baseline the
# audit-and-shrink plan's reduction targets are held to (gd-OxJf5mmbEQ).
#
# Usage: code-size.sh <repo-root> <out-dir>
#
# Counts tracked files only (git ls-files), so node_modules and build output
# never count. Go files that are generated (*.pb.go, or a
# "// Code generated ... DO NOT EDIT." line) are left out: nobody cuts them
# by hand. Writes:
#
#   <out-dir>/components.tsv    one row per component, then a "(total)" row:
#     component, go_src, go_test, long_src, long_test, complex_src,
#     complex_test, rust_src, rust_test
#   <out-dir>/long-funcs.tsv    component, file:line, func, lines   (> LONG)
#   <out-dir>/complex-funcs.tsv component, file:line, func, cyclo   (> CYCLO)
#   <out-dir>/packages.tsv      Go package dir, go_src, go_test; the
#                               per-package line budget reads it
#                               (scripts/ratchets/line-budget.sh)
#
# Lines are physical lines (wc -l), comments and blanks included. A Go
# function's length runs from its `func` line to the closing `}` in column 0,
# which gofmt guarantees. Complexity is gocyclo's (the binary $GOCYCLO names,
# default gocyclo); without it the complex_* columns read "-". Rust is lines
# only: test lines are files named *_tests.rs or under a tests/ dir (coop's
# convention).
#
# The component of a file is the first prefix in COMPONENTS that matches it.
# gardenio/docs/code-reduction-targets.md sets a target per component; keep
# the two lists in step.

set -euo pipefail
export LC_ALL=C

if [[ $# -ne 2 ]]; then
  echo "usage: $0 <repo-root> <out-dir>" >&2
  exit 2
fi
ROOT=$1
OUT=$2
LONG=${LONG:-100}
CYCLO=${CYCLO:-30}
GOCYCLO=${GOCYCLO:-gocyclo}

COMPONENTS='
beads-viewer          gardenio/controller/cmd/beads-viewer/
gb                    gardenio/controller/cmd/gb/
controller            gardenio/controller/cmd/controller/
controller-cmd-other  gardenio/controller/cmd/
bridge                gardenio/controller/internal/bridge/
controller-internal   gardenio/controller/
kbeads                kbeads/
helm-tests            gardenio/helm/gardenio/tests/
coop                  coop/
other                 *
'

mkdir -p "$OUT"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# component <path-list> — prefix every path with its component and a tab.
component() {
  awk -v spec="$COMPONENTS" '
    BEGIN { n = split(spec, rows, "\n"); for (i = 1; i <= n; i++) if (split(rows[i], f, " ") == 2) { k++; name[k] = f[1]; pre[k] = f[2] } }
    {
      for (i = 1; i <= k; i++) if (pre[i] == "*" || index($0, pre[i]) == 1) { print name[i] "\t" $0; next }
    }'
}

cd "$ROOT"

git ls-files -z -- '*.go' | tr '\0' '\n' | grep -v '\.pb\.go$' >"$TMP/go.all" || true
# grep -L lists the files with no generated-code header.
tr '\n' '\0' <"$TMP/go.all" | xargs -0 -r grep -L -E '^// Code generated .* DO NOT EDIT\.$' >"$TMP/go.files" || true

# One line per file ("L file lines") and per top-level func with a body
# ("F file:line name lines"). xargs may split the list, so each awk only
# reports what it read.
# The program goes in a file (a quoted heredoc, so the shell expands nothing)
# because xargs, not awk, is the command here.
cat >"$TMP/scan.awk" <<'AWK'
  function flush() { if (file != "") print "L\t" file "\t" n }
  # scan: lex one line far enough to track whether it ends inside a `raw
  # string` or a /* block comment */, where a column-0 "}" (embedded JSON or
  # JS) does not close the func. "Strings", runes and // comments are skipped
  # so a backtick inside them does not count.
  function scan(s,   i, m, c, q) {
    m = length(s); i = 1
    while (i <= m) {
      c = substr(s, i, 1)
      if (raw) { if (c == "`") raw = 0; i++; continue }
      if (blk) { if (substr(s, i, 2) == "*/") { blk = 0; i += 2 } else i++; continue }
      if (c == "`") { raw = 1; i++; continue }
      if (substr(s, i, 2) == "//") return
      if (substr(s, i, 2) == "/*") { blk = 1; i += 2; continue }
      if (c == "\"" || c == "\047") {
        q = c; i++
        while (i <= m) { c = substr(s, i, 1); if (c == "\\") i += 2; else { i++; if (c == q) break } }
        continue
      }
      i++
    }
  }
  FNR == 1 { flush(); file = FILENAME; n = 0; start = 0; raw = 0; blk = 0 }
  { n++; was = raw || blk; if (was || index($0, "`") || index($0, "/*")) scan($0) }
  was { next }
  start && /^}$/ { print "F\t" file ":" start "\t" fname "\t" (FNR - start + 1); start = 0; next }
  /^func / {
    h = substr($0, 6); recv = ""
    if (substr(h, 1, 1) == "(") {
      r = substr(h, 2, index(h, ")") - 2); h = substr(h, index(h, ")") + 1); sub(/^ +/, "", h)
      m = split(r, w, " "); t = w[m]; sub(/\[.*/, "", t); recv = "(" t ")."
    }
    fname = h; sub(/[\[(].*/, "", fname); fname = recv fname
    # A one-line func is complete; a body-less one (assembly) is skipped.
    if ($0 ~ /}[[:space:]]*$/) print "F\t" file ":" FNR "\t" fname "\t1"
    else start = FNR
  }
  END { flush() }
AWK
tr '\n' '\0' <"$TMP/go.files" | xargs -0 -r awk -f "$TMP/scan.awk" >"$TMP/go.scan"

awk -F '\t' '$1 == "L" { print $2 "\t" $3 }' "$TMP/go.scan" >"$TMP/go.lines"
# A Go package is a directory; root-level files count as ".".
awk -F '\t' '
  { d = $1; if (!sub(/\/[^\/]*$/, "", d)) d = "."; if ($1 ~ /_test\.go$/) t[d] += $2; else s[d] += $2; seen[d] = 1 }
  END { for (d in seen) print d "\t" s[d] + 0 "\t" t[d] + 0 }' "$TMP/go.lines" | sort >"$OUT/packages.tsv"
awk -F '\t' -v long="$LONG" '$1 == "F" && $4 > long { print $2 "\t" $3 "\t" $4 }' "$TMP/go.scan" |
  component | sort -t "$(printf '\t')" -k1,1 -k4,4nr -k2,2 >"$OUT/long-funcs.tsv"

if command -v "$GOCYCLO" >/dev/null 2>&1; then
  # gocyclo prints "<cyclo> <pkg> <func> <file:line:col>" and exits 1 when
  # it printed anything, so a wrapper maps 1 to 0 for xargs.
  cat >"$TMP/gocyclo.sh" <<'SH'
#!/bin/sh
g=$1 c=$2; shift 2; "$g" -over "$c" "$@"; [ $? -le 1 ]
SH
  tr '\n' '\0' <"$TMP/go.files" | xargs -0 -r sh "$TMP/gocyclo.sh" "$GOCYCLO" "$CYCLO" |
    awk '{ pos = $4; sub(/:[0-9]+$/, "", pos); print pos "\t" $3 "\t" $1 }' |
    component | sort -t "$(printf '\t')" -k1,1 -k4,4nr -k2,2 >"$OUT/complex-funcs.tsv"
  have_cyclo=1
else
  echo "code-size.sh: $GOCYCLO not on PATH; complexity columns left as -" >&2
  : >"$OUT/complex-funcs.tsv"
  have_cyclo=0
fi

git ls-files -z -- '*.rs' | xargs -0 -r wc -l | awk '$2 != "total" { print $2 "\t" $1 }' >"$TMP/rs.lines"

{
  # kind<TAB>component<TAB>... rows for the final roll-up.
  component <"$TMP/go.lines" | awk -F '\t' '{ print ($2 ~ /_test\.go$/ ? "go_test" : "go_src") "\t" $1 "\t" $3 }'
  component <"$TMP/rs.lines" | awk -F '\t' '{ print ($2 ~ /(_tests\.rs|(^|\/)tests\/.*)$/ ? "rust_test" : "rust_src") "\t" $1 "\t" $3 }'
  awk -F '\t' '{ print ($2 ~ /_test\.go:/ ? "long_test" : "long_src") "\t" $1 "\t1" }' "$OUT/long-funcs.tsv"
  awk -F '\t' '{ print ($2 ~ /_test\.go:/ ? "complex_test" : "complex_src") "\t" $1 "\t1" }' "$OUT/complex-funcs.tsv"
} | awk -F '\t' -v spec="$COMPONENTS" -v cyclo="$have_cyclo" '
  BEGIN {
    n = split(spec, rows, "\n"); for (i = 1; i <= n; i++) if (split(rows[i], f, " ") == 2) order[++k] = f[1]
    ncol = split("go_src go_test long_src long_test complex_src complex_test rust_src rust_test", col, " ")
  }
  { sum[$2, $1] += $3; tot[$1] += $3 }
  END {
    printf "component"; for (c = 1; c <= ncol; c++) printf "\t%s", col[c]; printf "\n"
    for (i = 1; i <= k; i++) {
      printf "%s", order[i]
      for (c = 1; c <= ncol; c++) printf "\t%s", (!cyclo && col[c] ~ /^complex/) ? "-" : sum[order[i], col[c]] + 0
      printf "\n"
    }
    printf "(total)"
    for (c = 1; c <= ncol; c++) printf "\t%s", (!cyclo && col[c] ~ /^complex/) ? "-" : tot[col[c]] + 0
    printf "\n"
  }
' >"$OUT/components.tsv"

cat "$OUT/components.tsv" >&2
