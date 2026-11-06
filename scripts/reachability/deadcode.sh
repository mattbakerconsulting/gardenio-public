#!/usr/bin/env bash
# deadcode.sh — reachability data for one Go module (gd-Nd2ciHTvTE).
#
# Runs golang.org/x/tools/cmd/deadcode once per main package and writes
# per-main lists of functions no call path from that main reaches. Run by
# .github/workflows/reachability.yml on the CI runner pool, never in an agent
# pod: one -test run type-checks and builds SSA for the whole module plus its
# tests, which is well past a 2Gi pod (advice gd-QLhMjEsBsR).
#
# Usage:
#   deadcode.sh <module-dir> <out-dir> <part> [shipped-main ...]
#
#   module-dir    gardenio/controller, kbeads, ...
#   out-dir       results land in <out-dir>/<module-slug>/
#   part          shipped  per shipped main: dead without -test, dead with
#                          -test, and the test-only difference; the same three
#                          for the union of shipped mains; the package lists.
#                 other    every main NOT in the shipped list: dead without
#                          -test only (gd-YzJ9Jj4gmR narrows which matter).
#   shipped-main  module-relative dirs, e.g. cmd/controller cmd/gb
#
# Output, all TSV "package<TAB>func<TAB>file:line:col", sorted:
#   dead/<main>.tsv              unreachable from <main> (no tests)
#   dead-with-tests/<main>.tsv   unreachable from <main> + every library
#                                package's tests (shipped only)
#   test-only/<main>.tsv         dead/ minus dead-with-tests/: reached ONLY by
#                                tests, the list gd-6rInaSPi7w needs
#   shipped-union.*.tsv          the same, roots = all shipped mains together;
#                                its dead list is the module-wide deletion set
#   packages-unshipped.txt       library packages no shipped main imports
#                                (deadcode never sees these: not loaded)
#   packages-unused.txt          library packages no main at all imports
#   summary-<part>.tsv           main, part, dead count, test-only count
#   errors-<part>.txt            mains deadcode failed on (run carries on)
# Per-part names because the shipped and other parts may run on different
# runners and are merged into one artifact directory afterwards.
#
# A main is named by its module-relative dir with / replaced by -, so
# cmd/slack-bridge -> cmd-slack-bridge.
#
# Env: DEADCODE (binary, default "deadcode"), JOBS (parallel no-test runs for
# part=other, default 2).

set -euo pipefail
export LC_ALL=C # sort and comm must agree on order

if [[ $# -lt 3 ]]; then
  echo "usage: $0 <module-dir> <out-dir> <shipped|other> [shipped-main ...]" >&2
  exit 2
fi

MODULE_DIR=$1
OUT_ROOT=$(mkdir -p "$2" && cd "$2" && pwd)
PART=$3
shift 3
SHIPPED=("$@")
DEADCODE=${DEADCODE:-deadcode}
JOBS=${JOBS:-2}

case "$PART" in
  shipped | other) ;;
  *) echo "part must be shipped or other, got: $PART" >&2; exit 2 ;;
esac
if [[ "$PART" == shipped && ${#SHIPPED[@]} -eq 0 ]]; then
  echo "part=shipped needs at least one shipped main" >&2
  exit 2
fi

cd "$MODULE_DIR"
OUT="$OUT_ROOT/$(echo "$MODULE_DIR" | tr / -)"
mkdir -p "$OUT/dead" "$OUT/dead-with-tests" "$OUT/test-only"
SUMMARY="$OUT/summary-$PART.tsv"
ERRORS="$OUT/errors-$PART.txt"
: >"$SUMMARY"
rm -f "$ERRORS"

# One record per dead function; TSV so set differences are a plain comm.
FMT='{{range .Funcs}}{{$.Path}}	{{.Name}}	{{.Position}}{{println}}{{end}}'

# Not `go list -m`: under the repo's go.work that prints every workspace
# module, not this one.
MOD=$(awk '$1 == "module" { print $2; exit }' go.mod)
mapfile -t ALL_MAINS < <(go list -f '{{if eq .Name "main"}}{{.ImportPath}}{{end}}' ./... | sed "s|^$MOD/||")
mapfile -t LIBS < <(go list -f '{{if ne .Name "main"}}{{.ImportPath}}{{end}}' ./...)
if [[ -z "$MOD" || ${#ALL_MAINS[@]} -eq 0 ]]; then
  echo "found no main packages in module '${MOD}' at $MODULE_DIR" >&2
  exit 1
fi

slug() { echo "$1" | tr / -; }

is_shipped() {
  local m
  for m in "${SHIPPED[@]}"; do [[ "$m" == "$1" ]] && return 0; done
  return 1
}

# run_dead <out-file> <deadcode args...>: sorted TSV, or an entry in $ERRORS.
run_dead() {
  local out=$1
  shift
  local tmp
  tmp=$(mktemp)
  if "$DEADCODE" -f "$FMT" "$@" >"$tmp" 2>"$tmp.err"; then
    sort -u "$tmp" >"$out"
  else
    { echo "== $*"; cat "$tmp.err"; } >>"$ERRORS"
    rm -f "$out"
  fi
  rm -f "$tmp" "$tmp.err"
}

count() { if [[ -f "$1" ]]; then wc -l <"$1" | tr -d ' '; else echo error; fi; }

# test_only <name>: dead without tests, alive once tests are roots.
test_only() {
  local n=$1
  if [[ -f "$OUT/dead/$n.tsv" && -f "$OUT/dead-with-tests/$n.tsv" ]]; then
    comm -23 "$OUT/dead/$n.tsv" "$OUT/dead-with-tests/$n.tsv" >"$OUT/test-only/$n.tsv"
  fi
}

if [[ "$PART" == shipped ]]; then
  for m in "${SHIPPED[@]}"; do
    if [[ ! " ${ALL_MAINS[*]} " == *" $m "* ]]; then
      echo "== $m: not a main package in $MOD" >>"$ERRORS"
      continue
    fi
    n=$(slug "$m")
    echo "deadcode $MODULE_DIR/$m"
    run_dead "$OUT/dead/$n.tsv" "./$m"
    # Tests of every library package are roots too, so a function that only
    # a _test.go calls is alive here and lands in test-only/.
    run_dead "$OUT/dead-with-tests/$n.tsv" -test "./$m" "${LIBS[@]}"
    test_only "$n"
    printf '%s\t%s\t%s\t%s\n' "$m" shipped "$(count "$OUT/dead/$n.tsv")" "$(count "$OUT/test-only/$n.tsv")" >>"$SUMMARY"
  done

  roots=()
  for m in "${SHIPPED[@]}"; do roots+=("./$m"); done
  echo "deadcode $MODULE_DIR shipped-union"
  run_dead "$OUT/dead/shipped-union.tsv" "${roots[@]}"
  run_dead "$OUT/dead-with-tests/shipped-union.tsv" -test "${roots[@]}" "${LIBS[@]}"
  test_only shipped-union
  printf '%s\t%s\t%s\t%s\n' "(all shipped)" union "$(count "$OUT/dead/shipped-union.tsv")" "$(count "$OUT/test-only/shipped-union.tsv")" >>"$SUMMARY"

  # Whole packages that deadcode cannot report because nothing loads them.
  printf '%s\n' "${LIBS[@]}" | sort -u >"$OUT/.libs"
  go list -deps -f '{{.ImportPath}}' "${roots[@]}" | sort -u >"$OUT/.shipped-deps"
  comm -23 "$OUT/.libs" "$OUT/.shipped-deps" >"$OUT/packages-unshipped.txt"
  all_roots=()
  for m in "${ALL_MAINS[@]}"; do all_roots+=("./$m"); done
  go list -deps -f '{{.ImportPath}}' "${all_roots[@]}" | sort -u >"$OUT/.all-deps"
  comm -23 "$OUT/.libs" "$OUT/.all-deps" >"$OUT/packages-unused.txt"
  rm -f "$OUT/.libs" "$OUT/.shipped-deps" "$OUT/.all-deps"
else
  others=()
  for m in "${ALL_MAINS[@]}"; do is_shipped "$m" || others+=("$m"); done
  # run_other <main>: one no-test run, called by the xargs below.
  run_other() {
    echo "deadcode $1"
    run_dead "$OUT/dead/$(echo "$1" | tr / -).tsv" "./$1"
  }
  export DEADCODE FMT OUT ERRORS
  export -f run_dead run_other
  # No -test here: each run loads only that main's deps, so two at a time
  # fit a heavy runner comfortably.
  if [[ ${#others[@]} -gt 0 ]]; then
    printf '%s\n' "${others[@]}" | xargs -P "$JOBS" -I{} bash -c "run_other \"\$1\"" _ {}
  fi
  for m in "${others[@]}"; do
    n=$(slug "$m")
    printf '%s\t%s\t%s\t%s\n' "$m" other "$(count "$OUT/dead/$n.tsv")" - >>"$SUMMARY"
  done
fi

sort -o "$SUMMARY" "$SUMMARY"
if [[ -s "$ERRORS" ]]; then
  echo "::warning::deadcode failed for some mains in $MODULE_DIR; see $(basename "$OUT")/errors-$PART.txt"
fi
