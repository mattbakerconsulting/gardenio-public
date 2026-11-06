#!/usr/bin/env bash
# tf-gardenio_test.sh
#
# Companion test for scripts/tf-gardenio.sh.
#
# The guard is tested on its own in check-tf-tree-freshness_test.sh. The only
# thing left for the wrapper to get wrong is WIRING -- calling the guard in a
# form where its exit status is discarded (`guard || true`, a pipeline, an
# unchecked `if`) and running terraform regardless. That is not hypothetical:
# it is the exact shape of the ad-hoc check this whole task replaces, which
# printed "*** STALE -- refusing ***" and then ran the plan.
#
# So the assertion that matters is not "the wrapper exits non-zero" but
# "TERRAFORM WAS NEVER INVOKED". Those differ: a wrapper that runs terraform and
# then returns the guard's status would pass the first and fail the second.
# terraform is stubbed on PATH with a script that records its invocation, and
# the test reads the sentinel.
#
# Run: bash scripts/tf-gardenio_test.sh
# Exit status: 0 if every case behaves as specified, 1 otherwise.

set -uo pipefail

SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

PASS=0
FAIL=0
note() {
	if [ "$1" = ok ]; then
		PASS=$((PASS + 1))
		printf 'ok   %s\n' "$2"
	else
		FAIL=$((FAIL + 1))
		printf 'FAIL %s\n' "$2"
		[ $# -gt 2 ] && printf '%s\n' "$3" | sed 's/^/       | /'
	fi
}

g() {
	git -c user.email=test@example.invalid \
		-c user.name='tf-gardenio test' \
		-c commit.gpgsign=false \
		"$@"
}

# ── a stub terraform that records that it ran ─────────────────────────────────
STUB_DIR="$WORK/bin"
SENTINEL="$WORK/terraform-was-invoked"
mkdir -p "$STUB_DIR"
cat >"$STUB_DIR/terraform" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "$SENTINEL"
echo "STUB TERRAFORM RAN: \$*"
exit 0
EOF
chmod +x "$STUB_DIR/terraform"
export PATH="$STUB_DIR:$PATH"

# ── a fixture repo carrying real copies of the scripts ────────────────────────
BARE="$WORK/origin.git"
g init --quiet --bare -b main "$BARE"
REPO="$WORK/repo"
g clone --quiet "$BARE" "$REPO"
mkdir -p "$REPO/scripts" "$REPO/gardenio/terraform/composition/gardenio"
cp "$SRC_DIR/tf-gardenio.sh" "$SRC_DIR/check-tf-tree-freshness.sh" "$REPO/scripts/"
touch "$REPO/gardenio/terraform/composition/gardenio/main.tf"
for n in 1 2; do
	echo "$n" >"$REPO/file$n.txt"
	g -C "$REPO" add -A
	g -C "$REPO" commit --quiet -m "commit $n"
done
g -C "$REPO" push --quiet origin main

run_wrapper() { bash "$REPO/scripts/tf-gardenio.sh" "$@" 2>&1; }

# ── case 1: current tree -- terraform MUST run (positive control) ─────────────
rm -f "$SENTINEL"
OUT="$(run_wrapper plan -target=module.k3s)"
RC=$?
if [ "$RC" -eq 0 ] && [ -f "$SENTINEL" ] && grep -qF 'plan -target=module.k3s' "$SENTINEL"; then
	note ok "current tree: terraform runs, args passed through"
else
	note FAIL "current tree: terraform runs, args passed through" "rc=$RC sentinel=$([ -f "$SENTINEL" ] && cat "$SENTINEL" || echo MISSING)
$OUT"
fi

# ── case 2: stale tree -- terraform MUST NOT run ──────────────────────────────
g -C "$REPO" reset --hard --quiet HEAD~1
rm -f "$SENTINEL"
OUT="$(run_wrapper plan)"
RC=$?
if [ "$RC" -ne 0 ] && [ ! -f "$SENTINEL" ] && grep -qF 'STALE' <<<"$OUT"; then
	note ok "stale tree: refused AND terraform never invoked"
else
	note FAIL "stale tree: refused AND terraform never invoked" "rc=$RC sentinel=$([ -f "$SENTINEL" ] && cat "$SENTINEL" || echo absent)
$OUT"
fi

# ── case 3: apply is gated identically to plan ────────────────────────────────
rm -f "$SENTINEL"
OUT="$(run_wrapper apply -target=module.k3s)"
RC=$?
if [ "$RC" -ne 0 ] && [ ! -f "$SENTINEL" ]; then
	note ok "stale tree: apply refused AND terraform never invoked"
else
	note FAIL "stale tree: apply refused AND terraform never invoked" "rc=$RC
$OUT"
fi

# ── case 4: --allow-unpushed is consumed by the wrapper, not passed on ────────
g -C "$REPO" merge --ff-only --quiet origin/main
echo local >"$REPO/local.txt"
g -C "$REPO" add local.txt
g -C "$REPO" commit --quiet -m "unpushed"
rm -f "$SENTINEL"
OUT="$(run_wrapper --allow-unpushed plan)"
RC=$?
if [ "$RC" -eq 0 ] && [ -f "$SENTINEL" ] && [ "$(cat "$SENTINEL")" = "plan" ]; then
	note ok "--allow-unpushed consumed by wrapper, terraform sees 'plan'"
else
	note FAIL "--allow-unpushed consumed by wrapper, terraform sees 'plan'" "rc=$RC sentinel=$([ -f "$SENTINEL" ] && cat "$SENTINEL" || echo MISSING)
$OUT"
fi

# ── case 5: without the flag, the same tree is refused ────────────────────────
rm -f "$SENTINEL"
OUT="$(run_wrapper plan)"
RC=$?
if [ "$RC" -ne 0 ] && [ ! -f "$SENTINEL" ]; then
	note ok "unpushed without flag: refused AND terraform never invoked"
else
	note FAIL "unpushed without flag: refused AND terraform never invoked" "rc=$RC
$OUT"
fi

# ── case 6: no arguments prints usage and refuses ─────────────────────────────
rm -f "$SENTINEL"
OUT="$(run_wrapper)"
RC=$?
if [ "$RC" -ne 0 ] && [ ! -f "$SENTINEL" ]; then
	note ok "no arguments: usage, terraform never invoked"
else
	note FAIL "no arguments: usage, terraform never invoked" "rc=$RC
$OUT"
fi

echo
printf '%d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
