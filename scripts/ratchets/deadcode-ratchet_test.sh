#!/usr/bin/env bash
# deadcode-ratchet_test.sh — self-test for deadcode-ratchet.sh. A stub
# deadcode drives the check/update/init logic anywhere; when go and deadcode
# are on PATH a fixture module runs the real thing.

set -euo pipefail
export LC_ALL=C

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
FAIL=0
DR="$HERE/deadcode-ratchet.sh"

check() { # check <name> <want> <got>
  if [[ "$2" == "$3" ]]; then
    echo "PASS $1"
  else
    echo "FAIL $1"
    echo "  want: $2"
    echo "  got:  $3"
    FAIL=1
  fi
}

rows() { grep -v '^#' "$1" | tr '\t' ' ' | paste -sd '|'; }

# The defaults here and in reachability.yml name the same shipped mains.
wf="$ROOT/.github/workflows/reachability.yml"
for pair in "CONTROLLER_MAINS_DEFAULT:gardenio/controller" "KBEADS_MAINS_DEFAULT:kbeads"; do
  key=${pair%%:*} dir=${pair#*:}
  want=$(sed -n "s/^  $key: //p" "$wf")
  got=$(sed -n "s|^    \*/$dir) SHIPPED_MAINS=\"\(.*\)\" ;;|\1|p" "$DR")
  check "shipped mains of $dir match reachability.yml" "$want" "$got"
done

M="$TMP/mod"
mkdir -p "$M"
printf 'module example.com/m\n\ngo 1.22\n' >"$M/go.mod"
# The stub prints whatever $TMP/dead holds, as deadcode -f would.
cat >"$TMP/stub" <<EOF
#!/bin/sh
cat "$TMP/dead"
EOF
chmod +x "$TMP/stub"
export DEADCODE="$TMP/stub" SHIPPED_MAINS="cmd/app"

check "usage" "2" "$(set +e; "$DR" bogus "$M" >/dev/null 2>&1; echo $?)"
check "not a module" "2" "$(set +e; "$DR" check "$TMP" >/dev/null 2>&1; echo $?)"
check "check needs a baseline" "2" "$(set +e; "$DR" check "$M" >/dev/null 2>&1; echo $?)"

printf 'm/lib\tDead\t/x/lib.go:3:6\nm/lib\tAlsoDead\t/x/lib.go:5:6\nm/lib\tDead\t/x/lib.go:3:6\n' >"$TMP/dead"
"$DR" init "$M" >/dev/null
check "init writes sorted unique rows" "m/lib AlsoDead|m/lib Dead" "$(rows "$M/.deadcode-baseline.tsv")"
check "init records the mains" "# Shipped mains: cmd/app" "$(grep '^# Shipped' "$M/.deadcode-baseline.tsv")"
check "unchanged passes" "0" "$(set +e; "$DR" check "$M" >/dev/null; echo $?)"

printf 'm/lib\tDead\t/x/lib.go:3:6\nm/lib\tNew\t/x/lib.go:9:6\n' >"$TMP/dead"
out=$(set +e; "$DR" check "$M"; echo "rc=$?")
check "a new dead func fails" "rc=1" "$(tail -n 1 <<<"$out")"
check "it is named with its position" "  m/lib  New  (/x/lib.go:9:6)" "$(grep '^  m/lib' <<<"$out")"

printf 'm/lib\tDead\t/x/lib.go:3:6\n' >"$TMP/dead"
out=$("$DR" check "$M")
check "deleting dead code passes" "  1 baseline entries are no longer dead; \`$DR update $M\` shrinks the list" "$(tail -n 1 <<<"$out")"
printf 'm/lib\tDead\t/x/lib.go:3:6\nm/lib\tNew\t/x/lib.go:9:6\n' >"$TMP/dead"
"$DR" update "$M" >/dev/null
check "update shrinks and never adds" "m/lib Dead" "$(rows "$M/.deadcode-baseline.tsv")"

printf '#!/bin/sh\necho "boom" >&2; exit 1\n' >"$TMP/stub"
check "a deadcode failure fails" "1" "$(set +e; "$DR" check "$M" >/dev/null 2>&1; echo $?)"

if command -v go >/dev/null && command -v deadcode >/dev/null; then
  export DEADCODE=deadcode
  R="$TMP/real"
  mkdir -p "$R/cmd/app" "$R/lib"
  printf 'module example.com/r\n\ngo 1.22\n' >"$R/go.mod"
  printf 'package main\n\nimport "example.com/r/lib"\n\nfunc main() { lib.Used() }\n' >"$R/cmd/app/main.go"
  printf 'package lib\n\nfunc Used() {}\n\nfunc Dead() {}\n' >"$R/lib/lib.go"
  (cd "$R" && GOWORK=off "$DR" init . >/dev/null)
  check "real: baseline" "example.com/r/lib Dead" "$(rows "$R/.deadcode-baseline.tsv")"
  # The done-condition of gd-d29n0m678l: an unused exported func goes red.
  printf '\nfunc Unused() {}\n' >>"$R/lib/lib.go"
  out=$(cd "$R" && set +e; GOWORK=off "$DR" check .; echo "rc=$?")
  check "real: an unused exported func fails" "rc=1" "$(tail -n 1 <<<"$out")"
  check "real: it is named" "example.com/r/lib Unused" "$(sed -n 's/^  \(example[^ ]*\)  \([A-Za-z]*\)  .*/\1 \2/p' <<<"$out")"
else
  echo "SKIP real deadcode fixture (needs go and deadcode on PATH)"
fi

exit "$FAIL"
