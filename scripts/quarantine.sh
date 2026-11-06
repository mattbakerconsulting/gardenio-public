#!/usr/bin/env bash
# quarantine.sh -- the known-failing tests list, .github/quarantine.d/*.tsv (one
# file per row, so concurrent PRs never conflict on it: gd-3lWbWYIFoT), as CI
# reads it: the files concatenated into one TSV.
#
# CI reads MAIN's copy, not the PR head's: a row pushed to main unblocks every
# open PR at once (owner rule 2026-11-01: a test that has blocked one PR, or
# failed on main, never blocks a second PR). Policy: .github/known-failures.md,
# "Quarantine".
#
# Usage:
#   scripts/quarantine.sh fetch OUT      write main's copy to OUT (this checkout's
#                                        if main cannot be fetched), plus this
#                                        checkout's own rows on a tag or a main
#                                        commit, less the rows a dispatched branch
#                                        deleted itself; warn on rows past expiry,
#                                        print OUT
#   scripts/quarantine.sh regex KIND F   ^(a|b)$ over KIND's names in F, for go
#                                        test -run/-skip; empty when none
#   scripts/quarantine.sh rows LOG       a ready-to-paste row for each top-level
#                                        `--- FAIL: Test...` and each failing
#                                        golden shot in a go test log
#   scripts/quarantine.sh verdicts F LOG...
#                                        "kind name bead pass|fail" for each row
#                                        of F that the logs report on (go test -v
#                                        lines, golden.Check's quarantine lines;
#                                        `gh run view --log` prefixes are
#                                        stripped); one fail anywhere is fail.
#                                        The dequarantine evidence: the
#                                        quarantine warden (docs/quarantine-warden.md)
set -euo pipefail

root=$(git rev-parse --show-toplevel)
dir=.github/quarantine.d
# legacy: the single-file list $dir replaced. Still read, so a writer on an older
# prompt or branch loses no row; drop it once no installed prompt writes it.
legacy=.github/quarantine.tsv

# rows_at REV: every $dir/*.tsv in REV's tree, then $legacy, each ending in a
# newline; fails when REV has neither.
rows_at() {
	local names f
	names=$(git -C "$root" ls-tree --full-tree --name-only "$1" -- "$dir/" "$legacy" 2>/dev/null) && [ -n "$names" ] || return 1
	while IFS= read -r f; do
		case $f in "$dir"/*.tsv | "$legacy") git -C "$root" show "$1:$f" | awk 1 ;; esac
	done <<<"$names"
}

# rows_here: the same for this checkout's working tree.
rows_here() {
	local f
	for f in "$root/$dir"/*.tsv "$root/$legacy"; do
		[ ! -f "$f" ] || awk 1 "$f"
	done
}

# merge_base MAIN: HEAD's merge-base with MAIN, deepening a shallow clone (both
# sides) until there is one; empty when there is none within ~1100 commits.
merge_base() {
	local n
	for n in 0 100 1000; do
		if [ "$n" -gt 0 ]; then
			[ "$(git -C "$root" rev-parse --is-shallow-repository)" = true ] || return 0
			git -C "$root" fetch -q --no-tags --deepen="$n" origin "$1" "$(git -C "$root" rev-parse HEAD)" 2>/dev/null || return 0
		fi
		git -C "$root" merge-base HEAD "$1" 2>/dev/null && return 0
	done
	return 0
}

case "${1:-}" in
fetch)
	out=${2:?fetch OUT}
	# --depth=1 only in a shallow clone: in a full one it would make the clone
	# shallow at main's head and break a later merge-base (ci.yml draft-light).
	depth=
	[ "$(git -C "$root" rev-parse --is-shallow-repository)" = false ] || depth=--depth=1
	main=
	if git -C "$root" fetch -q ${depth:+"$depth"} --no-tags origin main 2>/dev/null &&
		main=$(git -C "$root" rev-parse FETCH_HEAD) &&
		rows_at "$main" >"$out"; then
		echo "quarantine: main's $dir" >&2
		! git -C "$root" cat-file -e "$main:$legacy" 2>/dev/null ||
			echo "::warning::quarantine: main still has $legacy; move each of its rows into its own file in $dir" >&2
	else
		rows_here >"$out"
		echo "::warning::quarantine: could not read main's $dir; using this checkout's" >&2
	fi
	# A tag, or any commit already on main, also keeps its own tree's rows. A
	# fix and its row's removal land in one commit, so a tag cut before the fix
	# still fails the test that main no longer lists (2026.309.22 and
	# TestGoatsUnderChromium). A PR head never adds its own rows: a PR cannot
	# quarantine the test it breaks. A shallow clone cannot answer the ancestor
	# question, hence GITHUB_REF_TYPE.
	if [ "${GITHUB_REF_TYPE:-}" = tag ] || { [ -n "$main" ] && git -C "$root" merge-base --is-ancestor HEAD "$main" 2>/dev/null; }; then
		rows_here >"$out.here"
		awk -F'\t' 'FILENAME == ARGV[1] { if (!/^#/ && NF) on[$1 "\t" $2] = 1; next }
			!/^#/ && NF && !(($1 "\t" $2) in on)' "$out" "$out.here" >"$out.own"
		if [ -s "$out.own" ]; then
			echo "quarantine: + $(wc -l <"$out.own") row(s) of this checkout's $dir that main no longer has" >&2
			cat "$out.own" >>"$out"
		fi
		rm -f "$out.own" "$out.here"
	# A dispatched branch (a fix's proof run, docs/quarantine-warden.md "Fix
	# plan") drops each row it deleted itself, so the test under repair runs.
	# Deleted means in its merge-base with main and not in HEAD: a row main
	# added after the branch was cut still skips. A pull_request run never
	# gets here, so a PR cannot unquarantine a test by deleting its row.
	elif [ "${GITHUB_EVENT_NAME:-}" = workflow_dispatch ] && [ -n "$main" ]; then
		base=$(merge_base "$main")
		if [ -z "$base" ]; then
			echo "::warning::quarantine: no merge-base with main; keeping every row of main's $dir" >&2
		else
			rows_at "$base" >"$out.base" || : >"$out.base"
			rows_here >"$out.here"
			awk -F'\t' 'FILENAME == ARGV[1] { if (!/^#/ && NF) have[$1 "\t" $2] = 1; next }
				FILENAME == ARGV[2] { if (!/^#/ && NF && !(($1 "\t" $2) in have)) gone[$1 "\t" $2] = 1; next }
				/^#/ || !NF || !(($1 "\t" $2) in gone) { print; next }
				{ n++; printf "quarantine: - %s %s\n", $1, $2 > "/dev/stderr" }
				END { if (n) printf "quarantine: - %d row(s) this branch removes\n", n > "/dev/stderr" }' \
				"$out.here" "$out.base" "$out" >"$out.mine"
			mv "$out.mine" "$out"
			rm -f "$out.base" "$out.here"
		fi
	fi
	# A malformed row is dropped, not obeyed: a name reaches go test -skip as a
	# regex, and nothing in it may widen that past the one test.
	today=$(date -u +%F)
	awk -F'\t' -v today="$today" '
		/^#/ || NF == 0 { print; next }
		!(($1 == "go" && $2 ~ /^Test[A-Za-z0-9_]+$/) || ($1 == "golden" && $2 ~ /^[a-z0-9]+(-[a-z0-9]+)*\.png$/)) || $3 !~ /^[a-z]+-[A-Za-z0-9]+$/ {
			printf "::warning::quarantine: dropping malformed row: %s\n", $0 > "/dev/stderr"; next }
		NF >= 5 && $5 < today {
			printf "::warning::quarantine row past expiry %s: %s %s (%s): fix the test or renew the row\n", $5, $1, $2, $3 > "/dev/stderr" }
		{ print }' "$out" >"$out.ok"
	mv "$out.ok" "$out"
	echo "$out"
	;;
regex)
	kind=${2:?regex KIND F}
	names=$(awk -F'\t' -v k="$kind" '!/^#/ && $1 == k && $2 != "" { print $2 }' "${3:?regex KIND F}" | paste -sd'|' -)
	[ -z "$names" ] || echo "^($names)\$"
	;;
rows)
	today=$(date -u +%F)
	expires=$(date -u -d "$today + 14 days" +%F)
	run="${GITHUB_SERVER_URL:-https://github.com}/${GITHUB_REPOSITORY:-}/actions/runs/${GITHUB_RUN_ID:-}"
	log=${2:?rows LOG}
	{
		# the golden tests themselves are quarantined a shot at a time, below, never whole
		grep -oE '^--- FAIL: Test[A-Za-z0-9_]+' "$log" | awk '$3 !~ /Goldens$/ { print "go", $3 }' || true
		# golden.Check's failures, one indented "<shot>.png: <verdict>" line each
		grep -E '^\s+[a-z0-9]+(-[a-z0-9]+)*\.png: ' "$log" | grep -vE '\.png: (updated|removed)' |
			awk '{ sub(/:$/, "", $1); print "golden", $1 }' || true
	} | sort -u | while read -r kind name; do
		printf '%s\t%s\t<bug bead>\t%s\t%s\t%s\n' "$kind" "$name" "$today" "$expires" "$run"
	done | sed '1i # Unrelated to this change? Push each row to main as its own file, .github/quarantine.d/<kind>-<name>.tsv (file the bug bead first):'
	;;
verdicts)
	f=${2:?verdicts F LOG...}
	shift 2
	[ "$#" -gt 0 ] || { echo "verdicts F LOG...: no log" >&2; exit 2; }
	# `gh run view --log` lines are "<job>\t<step>\t<RFC 3339 time> <line>"; a
	# raw job log has only the time. Strip both, then keep only top-level
	# tests: a subtest's "--- PASS" is indented.
	sed -E 's/^([^\t]*\t[^\t]*\t)?(\xEF\xBB\xBF)?[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:.]+Z //' "$@" |
		awk -F'\t' -v list="$f" '
			FILENAME == list { if (!/^#/ && NF >= 3) bead[$1 "\t" $2] = $3; next }
			match($0, /^--- (PASS|FAIL): Test[A-Za-z0-9_]+/) {
				split(substr($0, 1, RLENGTH), w, " ")
				seen("go", w[3], w[2] == "PASS:" ? "pass" : "fail")
			}
			match($0, /quarantined [a-z0-9]+(-[a-z0-9]+)*\.png passed/) {
				split(substr($0, RSTART, RLENGTH), w, " "); seen("golden", w[2], "pass")
			}
			match($0, /QUARANTINED [a-z0-9]+(-[a-z0-9]+)*\.png:/) {
				split(substr($0, RSTART, RLENGTH - 1), w, " "); seen("golden", w[2], "fail")
			}
			function seen(kind, name, v,   k) {
				k = kind "\t" name
				if (!(k in bead)) return
				if (v == "fail" || !(k in got)) got[k] = v
			}
			END { for (k in got) print k "\t" bead[k] "\t" got[k] }' "$f" - | sort
	;;
*)
	sed -n '2,/^set -euo/p' "$0" | sed '$d'
	exit 2
	;;
esac
