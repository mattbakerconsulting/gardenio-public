#!/usr/bin/env bash
# regen-baselines_test.sh — self-test for regen-baselines.sh with a stub gh
# and a stub coverage-baselines, so it needs no network and no go build.

set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
FAIL=0

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

# Stub gh: green release-tag runs 3 and 4, nightly runs 2 and 1, so newest
# first 3, 2, 4, 1; run 2 has no artifacts (expired). Run 7 (an old main
# push) and run 8 (a PR run) must never be taken. Downloads are logged.
cat >"$TMP/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "$1 $2" in
  "run list")
    [[ " $* " == *" --branch "* ]] && { echo "run list must not filter by branch" >&2; exit 2; }
    case " $* " in
      *" --event push "*)
        printf '2026-11-03T04:01:00Z\t3\tccc\tpush\t2026.307.1\n'
        printf '2026-11-03T05:00:00Z\t7\tggg\tpush\tmain\n'
        printf '2026-11-03T06:00:00Z\t8\thhh\tpull_request\tagent/x\n'
        printf '2026-11-01T16:02:00Z\t4\tddd\tpush\t2026.305.2\n' ;;
      *" --event schedule "*)
        printf '2026-11-02T10:43:00Z\t2\tbbb\tschedule\tmain\n'
        printf '2026-10-31T10:43:00Z\t1\taaa\tschedule\tmain\n' ;;
      *) echo "run list without a release or nightly --event: $*" >&2; exit 2 ;;
    esac ;;
  "run download")
    id=$3; dir=${*: -1}
    echo "$id" >>"${0%/*}/downloads"
    [[ $id == 2 ]] && exit 1
    mkdir -p "$dir/coverage-lcov-kbeads" "$dir/coverage-lcov-controller"
    printf 'SF:kbeads/a.go\nDA:1,1\nend_of_record\n' >"$dir/coverage-lcov-kbeads/kbeads.lcov"
    printf 'SF:gardenio/controller/b.go\nDA:1,0\nend_of_record\n' >"$dir/coverage-lcov-controller/controller.lcov"
    ;;
  *) echo "unexpected gh $*" >&2; exit 2 ;;
esac
EOF
# Stub coverage-baselines: record what the window held.
cat >"$TMP/cb" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
while [[ $# -gt 0 ]]; do
  case $1 in
    --inputs-dir) in=$2 ;; --commits) commits=$2 ;; --output) out=$2 ;;
  esac
  shift 2
done
{ (cd "$in" && ls); cat "$commits"; grep -c '^SF:' "$in"/3.lcov; } >"$out"
EOF
chmod +x "$TMP/gh" "$TMP/cb"

export GH="$TMP/gh" COVERAGE_BASELINES_BIN="$TMP/cb" REPO=o/r
out="$TMP/out.json"

err=$("$HERE/regen-baselines.sh" 2 "$out" 2>&1)
check "takes tag and nightly runs newest first, skips the run without artifacts, stops at 2" \
  "3.lcov|4.lcov|3	ccc|4	ddd|2" "$(paste -sd '|' "$out")"
check "says which run it skipped" "run 2: no coverage artifacts, skipped" "$err"

rm -f "$TMP/downloads"
err=$("$HERE/regen-baselines.sh" 5 "$out" 2>&1)
check "warns on a short window" "1" "$(grep -c 'only 3 of 5 runs' <<<"$err")"
check "never takes a main push or PR run" "3|2|4|1" "$(paste -sd '|' "$TMP/downloads")"

cat >"$TMP/gh" <<'EOF'
#!/usr/bin/env bash
case "$1 $2 $*" in
  "run list "*"--event push"*) printf '2026-11-03T06:00:00Z\t9\tzzz\tpush\t2026.307.1\n' ;;
  "run list "*) ;;
  *) exit 1 ;;
esac
EOF
check "no artifacts at all fails" "1" "$(set +e; "$HERE/regen-baselines.sh" 3 "$out" >/dev/null 2>&1; echo $?)"

cat >"$TMP/gh" <<'EOF'
#!/usr/bin/env bash
case "$1 $2" in
  "run list") printf '2026-11-03T06:00:00Z\t8\thhh\tpull_request\tagent/x\n2026-11-03T05:00:00Z\t7\tggg\tpush\tmain\n' ;;
  *) echo "downloaded $3" >&2; exit 1 ;;
esac
EOF
err=$(set +e; "$HERE/regen-baselines.sh" 3 "$out" 2>&1; echo "rc=$?")
check "only PR and main-push runs listed: fails, downloads none" "rc=1|0" \
  "$(tail -n1 <<<"$err")|$(grep -c '^downloaded' <<<"$err")"

exit $FAIL
