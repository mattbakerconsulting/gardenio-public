#!/usr/bin/env bash
# quarantine_test.sh -- scripts/quarantine.sh against fixtures: main's
# .github/quarantine.d wins over this checkout's, malformed rows are dropped, the
# regex, and the rows it prints for a failing log, and a dispatched branch's own
# deletions.
set -euo pipefail
unset GITHUB_REF_TYPE GITHUB_EVENT_NAME # CI's own; the cases below set them

here=$(cd "$(dirname "$0")" && pwd)
q=$here/quarantine.sh
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fail=0
check() { # name want got
	if [ "$2" != "$3" ]; then
		printf 'FAIL %s\n  want: %s\n  got:  %s\n' "$1" "$2" "$3"
		fail=1
	fi
}
# row REPO KIND NAME [BEAD [EXPIRES]]: REPO's file for that row, as the list has it
row() {
	mkdir -p "$1/.github/quarantine.d"
	printf '%s\t%s\t%s\t2026-11-01\t%s\trun\n' "$2" "$3" "${4-gd-abc123}" "${5:-2099-01-01}" >"$1/.github/quarantine.d/$2-$3.tsv"
}

# An origin whose main has one quarantined test, and a clone whose own copy
# (a PR head) does not have it.
git init -q -b main "$tmp/origin"
mkdir -p "$tmp/origin/.github/quarantine.d"
printf '# Known-failing tests\n' >"$tmp/origin/.github/quarantine.d/README.md"
git -C "$tmp/origin" add . && git -C "$tmp/origin" -c user.name=t -c user.email=t@t commit -qm head
git clone -q "$tmp/origin" "$tmp/clone"
row "$tmp/origin" go TestFlaky
row "$tmp/origin" golden land-phone-dark.png gd-abc123 2000-01-01
printf '# a comment\ngo\tTest.*\tgd-abc123\t2026-11-01\t2026-11-15\twidens the skip' >"$tmp/origin/.github/quarantine.d/go-wide.tsv" # no final newline
row "$tmp/origin" go TestNoBead ''
git -C "$tmp/origin" add . && git -C "$tmp/origin" -c user.name=t -c user.email=t@t commit -qm 'quarantine on main'

out=$(cd "$tmp/clone" && "$q" fetch "$tmp/q.tsv" 2>"$tmp/err")
check "fetch prints its output path" "$tmp/q.tsv" "$out"
check "main's go row, not the checkout's empty list" '^(TestFlaky)$' "$("$q" regex go "$tmp/q.tsv")"
check "golden regex" '^(land-phone-dark.png)$' "$("$q" regex golden "$tmp/q.tsv")"
check "two malformed rows dropped" 2 "$(grep -c 'dropping malformed row' "$tmp/err")"
check "expired row warned" 1 "$(grep -c 'past expiry 2000-01-01: golden land-phone-dark.png' "$tmp/err")"
check "no go rows: empty regex" '' "$("$q" regex go /dev/null)"

# A full clone stays full: fetch never deepens it to depth 1 (file:// so that
# --depth would apply, as it does over the network).
git clone -q "file://$tmp/origin" "$tmp/full"
(cd "$tmp/full" && "$q" fetch "$tmp/q3.tsv" 2>/dev/null >/dev/null)
check "full clone not made shallow" false "$(git -C "$tmp/full" rev-parse --is-shallow-repository)"
check "full clone reads main's row" '^(TestFlaky)$' "$("$q" regex go "$tmp/q3.tsv")"

# A writer on an older prompt re-creates the single-file list on main: its rows
# still count, with a warning to move them.
printf '# old header\ngo\tTestLegacy\tgd-abc123\t2026-11-01\t2099-01-01\trun\n' >"$tmp/origin/.github/quarantine.tsv"
git -C "$tmp/origin" add . && git -C "$tmp/origin" -c user.name=t -c user.email=t@t commit -qm 'legacy row'
(cd "$tmp/full" && "$q" fetch "$tmp/q3b.tsv" 2>"$tmp/err3b" >/dev/null)
check "main's legacy list is read too" '^(TestFlaky|TestLegacy)$' "$("$q" regex go "$tmp/q3b.tsv")"
check "main's legacy list warns" 1 "$(grep -c 'main still has .github/quarantine.tsv' "$tmp/err3b")"
git -C "$tmp/origin" rm -q .github/quarantine.tsv && git -C "$tmp/origin" -c user.name=t -c user.email=t@t commit -qm 'moved'

# No origin to fetch from: this checkout's copy, with a warning.
git -C "$tmp/clone" remote set-url origin "$tmp/nowhere"
(cd "$tmp/clone" && "$q" fetch "$tmp/q2.tsv" 2>"$tmp/err2" >/dev/null)
check "fallback warns" 1 "$(grep -c "could not read main's" "$tmp/err2")"
check "fallback is the checkout's (empty) list" '' "$("$q" regex go "$tmp/q2.tsv")"

# A tag cut before a fix keeps its own rows that main has since removed; a PR
# head (not on main) never adds its own.
git init -q -b main "$tmp/o2"
row "$tmp/o2" go TestOld
row "$tmp/o2" go TestKept
git -C "$tmp/o2" add . && git -C "$tmp/o2" -c user.name=t -c user.email=t@t commit -qm tag
git clone -q "$tmp/o2" "$tmp/tag"
git -C "$tmp/o2" rm -q .github/quarantine.d/go-TestOld.tsv
git -C "$tmp/o2" -c user.name=t -c user.email=t@t commit -qm 'fix TestOld; unquarantine'
(cd "$tmp/tag" && "$q" fetch "$tmp/q4.tsv" 2>"$tmp/err4" >/dev/null)
check "a commit on main keeps its own removed row" '^(TestKept|TestOld)$' "$("$q" regex go "$tmp/q4.tsv")"
check "added rows reported" 1 "$(grep -c '+ 1 row' "$tmp/err4")"
git -C "$tmp/tag" -c user.name=t -c user.email=t@t checkout -qb pr
row "$tmp/tag" go TestMine
git -C "$tmp/tag" add . && git -C "$tmp/tag" -c user.name=t -c user.email=t@t commit -qm 'pr quarantines its own test'
(cd "$tmp/tag" && "$q" fetch "$tmp/q5.tsv" 2>/dev/null >/dev/null)
check "a PR head adds none of its own rows" '^(TestKept)$' "$("$q" regex go "$tmp/q5.tsv")"
(cd "$tmp/tag" && GITHUB_REF_TYPE=tag "$q" fetch "$tmp/q6.tsv" 2>/dev/null >/dev/null)
check "GITHUB_REF_TYPE=tag adds them (a shallow tag checkout)" '^(TestKept|TestMine|TestOld)$' "$("$q" regex go "$tmp/q6.tsv")"
# Main with no rows left (only its README): still main's list, and a tag
# still adds every row of its own.
printf '# list\n' >"$tmp/o2/.github/quarantine.d/README.md"
git -C "$tmp/o2" rm -q .github/quarantine.d/go-TestKept.tsv
git -C "$tmp/o2" add . && git -C "$tmp/o2" -c user.name=t -c user.email=t@t commit -qm 'fix TestKept; unquarantine'
(cd "$tmp/tag" && GITHUB_REF_TYPE=tag "$q" fetch "$tmp/q6b.tsv" 2>"$tmp/err6b" >/dev/null)
check "an empty list on main: a tag adds its own rows" '^(TestKept|TestMine|TestOld)$' "$("$q" regex go "$tmp/q6b.tsv")"
check "an empty list on main is main's, not a fallback" 0 "$(grep -c "could not read" "$tmp/err6b" || true)"

# A dispatched fix branch, in a depth-1 clone, drops the row it deleted
# (in its merge-base with main, not in its HEAD), so its test runs; a row main
# added after the branch was cut still skips. A pull_request run (no
# GITHUB_EVENT_NAME=workflow_dispatch) keeps main's list whole.
git init -q -b main "$tmp/o3"
row "$tmp/o3" go TestFixed
row "$tmp/o3" go TestStill
git -C "$tmp/o3" add . && git -C "$tmp/o3" -c user.name=t -c user.email=t@t commit -qm base
git -C "$tmp/o3" checkout -qb fix
git -C "$tmp/o3" rm -q .github/quarantine.d/go-TestFixed.tsv
git -C "$tmp/o3" -c user.name=t -c user.email=t@t commit -qm 'fix TestFixed; unquarantine'
git -C "$tmp/o3" -c user.name=t -c user.email=t@t commit -q --allow-empty -m 'more fix'
git -C "$tmp/o3" checkout -q main
for i in 1 2 3; do git -C "$tmp/o3" -c user.name=t -c user.email=t@t commit -q --allow-empty -m "main $i"; done
row "$tmp/o3" go TestLater
git -C "$tmp/o3" add . && git -C "$tmp/o3" -c user.name=t -c user.email=t@t commit -qm 'quarantine TestLater on main'
git clone -q --depth=1 --branch fix "file://$tmp/o3" "$tmp/fix"
(cd "$tmp/fix" && "$q" fetch "$tmp/q7.tsv" 2>/dev/null >/dev/null)
check "a pull_request run keeps main's list" '^(TestFixed|TestLater|TestStill)$' "$("$q" regex go "$tmp/q7.tsv")"
(cd "$tmp/fix" && GITHUB_EVENT_NAME=workflow_dispatch "$q" fetch "$tmp/q8.tsv" 2>"$tmp/err8" >/dev/null)
check "a dispatch drops the row its branch deleted, not one main added since" '^(TestLater|TestStill)$' "$("$q" regex go "$tmp/q8.tsv")"
check "dropped rows reported" 1 "$(grep -c 'quarantine: - 1 row(s) this branch removes' "$tmp/err8")"
check "dispatch in a shallow clone deepened to the merge-base" "$(git -C "$tmp/o3" merge-base main fix)" "$(git -C "$tmp/fix" merge-base HEAD "$(git -C "$tmp/o3" rev-parse main)")"
(cd "$tmp/fix" && GITHUB_EVENT_NAME=workflow_dispatch "$q" fetch "$tmp/q9.tsv" 2>/dev/null >/dev/null)
check "a second fetch in the same job agrees" '^(TestLater|TestStill)$' "$("$q" regex go "$tmp/q9.tsv")"
# No shared history (no merge-base): main's list, with a warning.
git -C "$tmp/fix" -c user.name=t -c user.email=t@t checkout -q --orphan alone
git -C "$tmp/fix" -c user.name=t -c user.email=t@t commit -qm alone
(cd "$tmp/fix" && GITHUB_EVENT_NAME=workflow_dispatch "$q" fetch "$tmp/q10.tsv" 2>"$tmp/err10" >/dev/null)
check "no merge-base keeps main's list" '^(TestFixed|TestLater|TestStill)$' "$("$q" regex go "$tmp/q10.tsv")"
check "no merge-base warns" 1 "$(grep -c 'no merge-base with main' "$tmp/err10")"

# rows: one per failing top-level test and failing golden shot; never a
# subtest, never a whole golden test, never an updated shot.
cat >"$tmp/log" <<'EOF'
--- FAIL: TestAlpha (0.01s)
    --- FAIL: TestAlpha/sub (0.00s)
--- FAIL: TestGardenGoldens (512.00s)
    golden_browser_test.go:313: 2 golden screenshot(s) differ; actual/expected/diff PNGs are in /x.
          land-phone-dark.png: 13659 of 329160 pixels differ, tolerance 16
          wall-desktop-dark.png: no golden
    golden_browser_test.go:306: land-phone-light.png          12 of  329160 px differ (tolerance 16)
    golden_browser_test.go:306: bead-phone-light.png: updated (1 of 2 pixels differ)
--- FAIL: TestAlpha (0.01s)
EOF
got=$(GITHUB_RUN_ID=7 GITHUB_REPOSITORY=o/r "$q" rows "$tmp/log" | cut -f1,2,6)
want=$(printf '%s\n' '# Unrelated to this change? Push each row to main as its own file, .github/quarantine.d/<kind>-<name>.tsv (file the bug bead first):' \
	"go	TestAlpha	https://github.com/o/r/actions/runs/7" \
	"golden	land-phone-dark.png	https://github.com/o/r/actions/runs/7" \
	"golden	wall-desktop-dark.png	https://github.com/o/r/actions/runs/7")
check "rows" "$want" "$got"
check "rows: nothing for a clean log" '' "$(printf -- '--- PASS: TestA\n' >"$tmp/ok" && "$q" rows "$tmp/ok")"

# verdicts: the listed rows a log reports on, from `go test -v` lines and
# golden.Check's quarantine lines, with `gh run view --log` prefixes (a step's
# first line starts with a BOM); a fail
# anywhere beats a pass, a subtest or an unlisted test is ignored.
printf 'go\tTestFlaky\tgd-a\ngo\tTestShaky\tgd-b\ngo\tTestQuiet\tgd-c\ngolden\tland-phone-dark.png\tgd-d\ngolden\twall-desktop-dark.png\tgd-d\n' >"$tmp/v.tsv"
cat >"$tmp/v1" <<'EOF2'
build + test (kbeads)	quarantined tests (advisory)	﻿2026-11-05T19:00:01.1234567Z --- PASS: TestFlaky (0.30s)
build + test (kbeads)	quarantined tests (advisory)	2026-11-05T19:00:01.2Z     --- FAIL: TestShaky/sub (0.00s)
build + test (kbeads)	quarantined tests (advisory)	2026-11-05T19:00:01.3Z --- PASS: TestShaky (0.10s)
build + test (kbeads)	quarantined tests (advisory)	2026-11-05T19:00:01.4Z --- FAIL: TestUnlisted (0.10s)
2026-11-05T19:00:02.0Z     golden_browser_test.go:308: quarantined land-phone-dark.png passed; delete .github/quarantine.d/golden-land-phone-dark.png.tsv once gd-d is fixed
2026-11-05T19:00:02.1Z     golden_browser_test.go:308: QUARANTINED wall-desktop-dark.png: 9 of 10 pixels differ, tolerance 1 (gd-d, .github/quarantine.d); not failing the test
EOF2
printf -- '--- FAIL: TestFlaky (1.00s)\n' >"$tmp/v2"
check "verdicts" "$(printf 'go\tTestFlaky\tgd-a\tfail\ngo\tTestShaky\tgd-b\tpass\ngolden\tland-phone-dark.png\tgd-d\tpass\ngolden\twall-desktop-dark.png\tgd-d\tfail')" "$("$q" verdicts "$tmp/v.tsv" "$tmp/v1" "$tmp/v2")"
check "verdicts: one log, its pass" "$(printf 'go\tTestFlaky\tgd-a\tpass')" "$("$q" verdicts "$tmp/v.tsv" "$tmp/v1" | grep TestFlaky)"

[ "$fail" = 0 ] && echo "quarantine_test: ok"
exit "$fail"
