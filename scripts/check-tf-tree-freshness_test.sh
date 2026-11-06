#!/usr/bin/env bash
# check-tf-tree-freshness_test.sh
#
# Companion test for scripts/check-tf-tree-freshness.sh.
#
# The guard's whole value is that it REFUSES, so the cases that matter are the
# ones where it must exit non-zero. Each fixture below is a real git repository
# built from a real bare "origin", so the guard's actual `git fetch` runs -- the
# test does not stub the thing being tested.
#
# Both directions are controlled:
#   - NEGATIVE controls (must refuse): stale, diverged, unpushed, unreachable
#     remote, shallow clone.
#   - POSITIVE controls (must pass): a current tree, and the unpushed tree once
#     --allow-unpushed is given. Without these, a guard that refused
#     unconditionally would pass its own test suite -- which is the failure mode
#     this repo keeps hitting (a `for` loop over a glob matching zero files is
#     indistinguishable from success).
#
# The unpushed fixture is deliberately used for BOTH a refusal and a pass, which
# is what makes the refusal attributable: the same tree flips on the flag alone,
# so case 4 cannot be failing for some incidental reason about the fixture.
#
# Run: bash scripts/check-tf-tree-freshness_test.sh
# Exit status: 0 if every case behaves as specified, 1 otherwise.

set -uo pipefail

GUARD="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/check-tf-tree-freshness.sh"
[ -f "$GUARD" ] || {
	echo "FATAL: guard not found at $GUARD" >&2
	exit 1
}

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

PASS=0
FAIL=0

# git that does not read the operator's config or need an identity configured.
g() {
	git -c user.email=test@example.invalid \
		-c user.name='freshness test' \
		-c commit.gpgsign=false \
		-c protocol.file.allow=always \
		"$@"
}

# expect <label> <expected-rc-class: zero|nonzero> <expected-substring|-> -- cmd...
expect() {
	local label="$1" class="$2" want="$3"
	shift 4 # drop label, class, want, and the literal --
	local out rc
	out="$("$@" 2>&1)"
	rc=$?

	local ok=1
	case "$class" in
	zero) [ "$rc" -eq 0 ] || ok=0 ;;
	nonzero) [ "$rc" -ne 0 ] || ok=0 ;;
	esac
	# grep -q exits at the first match, so a producer piped into it takes EPIPE
	# and pipefail flips the branch; herestring instead (check-sigpipe-producers).
	if [ "$want" != "-" ] && ! grep -qF -- "$want" <<<"$out"; then
		ok=0
	fi

	if [ "$ok" -eq 1 ]; then
		PASS=$((PASS + 1))
		printf 'ok   %-46s (rc=%d)\n' "$label" "$rc"
	else
		FAIL=$((FAIL + 1))
		printf 'FAIL %-46s (rc=%d, wanted %s containing %q)\n' "$label" "$rc" "$class" "$want"
		printf '%s\n' "$out" | sed 's/^/       | /'
	fi
}

# ── build a real origin with three commits ────────────────────────────────────
BARE="$WORK/origin.git"
g init --quiet --bare -b main "$BARE"

SEED="$WORK/seed"
g clone --quiet "$BARE" "$SEED"
for n in 1 2 3; do
	echo "commit $n" >"$SEED/file$n.txt"
	g -C "$SEED" add "file$n.txt"
	g -C "$SEED" commit --quiet -m "commit $n"
done
g -C "$SEED" push --quiet origin main
TIP="$(g -C "$SEED" rev-parse HEAD)"
FIRST="$(g -C "$SEED" rev-parse HEAD~2)"

# ── fixtures ──────────────────────────────────────────────────────────────────

# current: exactly origin/main
CURRENT="$WORK/current"
g clone --quiet "$BARE" "$CURRENT"

# stale: behind origin/main (the measured hazard)
STALE="$WORK/stale"
g clone --quiet "$BARE" "$STALE"
g -C "$STALE" reset --hard --quiet "$FIRST"

# diverged: a local commit on top of a stale base
DIVERGED="$WORK/diverged"
g clone --quiet "$BARE" "$DIVERGED"
g -C "$DIVERGED" reset --hard --quiet "$FIRST"
echo local >"$DIVERGED/local.txt"
g -C "$DIVERGED" add local.txt
g -C "$DIVERGED" commit --quiet -m "local only"

# unpushed: contains origin/main, plus a local commit on top
UNPUSHED="$WORK/unpushed"
g clone --quiet "$BARE" "$UNPUSHED"
echo local >"$UNPUSHED/local.txt"
g -C "$UNPUSHED" add local.txt
g -C "$UNPUSHED" commit --quiet -m "unpushed work"

# unreachable: a valid tree whose remote does not exist
UNREACHABLE="$WORK/unreachable"
g clone --quiet "$BARE" "$UNREACHABLE"
g -C "$UNREACHABLE" remote set-url origin "$WORK/does-not-exist.git"

# shallow: truncated history, where ancestry cannot be trusted
SHALLOW="$WORK/shallow"
g clone --quiet --depth 1 "file://$BARE" "$SHALLOW"

# Fixture sanity: assert the fixtures are what their names claim BEFORE the
# guard is asked about them. Otherwise a broken fixture reads as a guard result.
[ "$(g -C "$CURRENT" rev-parse HEAD)" = "$TIP" ] || {
	echo "FATAL: current fixture is not at the tip" >&2
	exit 1
}
[ "$(g -C "$STALE" rev-parse HEAD)" = "$FIRST" ] || {
	echo "FATAL: stale fixture is not behind" >&2
	exit 1
}
[ "$(g -C "$SHALLOW" rev-parse --is-shallow-repository)" = "true" ] || {
	echo "FATAL: shallow fixture is not shallow (local clone ignored --depth?)" >&2
	exit 1
}

# ── cases ─────────────────────────────────────────────────────────────────────
echo "== negative controls: the guard must refuse =="
expect "stale tree refused" nonzero "STALE" -- \
	bash "$GUARD" --repo "$STALE"
expect "diverged tree refused" nonzero "DIVERGED" -- \
	bash "$GUARD" --repo "$DIVERGED"
expect "unpushed commits refused" nonzero "unpushed commit" -- \
	bash "$GUARD" --repo "$UNPUSHED"
expect "unreachable remote refused" nonzero "could not fetch" -- \
	bash "$GUARD" --repo "$UNREACHABLE"
expect "shallow clone refused" nonzero "shallow clone" -- \
	bash "$GUARD" --repo "$SHALLOW"
expect "missing repo dir refused" nonzero "no such directory" -- \
	bash "$GUARD" --repo "$WORK/absent"

echo "== positive controls: the guard must pass, or it is vacuous =="
expect "current tree passes" zero "OK" -- \
	bash "$GUARD" --repo "$CURRENT"
expect "unpushed passes with --allow-unpushed" zero "OK (--allow-unpushed)" -- \
	bash "$GUARD" --repo "$UNPUSHED" --allow-unpushed

echo "== the override must not rescue a stale or diverged tree =="
expect "--allow-unpushed does NOT excuse stale" nonzero "STALE" -- \
	bash "$GUARD" --repo "$STALE" --allow-unpushed
expect "--allow-unpushed does NOT excuse diverged" nonzero "DIVERGED" -- \
	bash "$GUARD" --repo "$DIVERGED" --allow-unpushed

echo
printf '%d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
