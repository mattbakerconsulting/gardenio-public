#!/usr/bin/env bash
# lint-public-strings_test.sh — planted positive controls for lint-public-strings.sh.
# Copies the tree, asserts the guard is green on the copy, then plants one
# finding per mode and asserts the guard turns red naming the planted file.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
GUARD="${SCRIPT_DIR}/lint-public-strings.sh"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
ALLOWLIST="docs/public-readiness/allowlist.txt"
PLANTED="gardenio/docs/planted.md"
# Split so this file never matches the guard itself.
ACCOUNT_ID='360190''250083'
DEAD_HOST='prod.gardenio.ac''me.example'
GROUP_PATH='gitlab.com/Ac''me/Platform'
TESTDATA="gardenio/controller/testdata/planted.txt"

PASS=0; FAIL=0
pass() { echo "PASS: $1"; PASS=$((PASS+1)); }
fail() { echo "FAIL: $1: $2"; FAIL=$((FAIL+1)); }

# One copy of the working tree (the guard reads files only, never git); each
# case resets the planted file and the allowlist instead of re-copying.
mkdir -p "$TMP/tree"
tar -C "$REPO_ROOT" --exclude=.git --exclude=.worktrees --exclude=.gb --exclude=node_modules --exclude=target -cf - . \
  | tar -C "$TMP/tree" -xf -
reset_copy() {
  rm -f "$TMP/tree/$PLANTED" "$TMP/tree/$TESTDATA"
  cp "$REPO_ROOT/$ALLOWLIST" "$TMP/tree/$ALLOWLIST"
}

# --- infra-ids -------------------------------------------------------------
reset_copy
if out="$(bash "$GUARD" --infra-ids "$TMP/tree" 2>&1)"; then pass "infra-ids clean on committed tree"
else fail "infra-ids clean on committed tree" "$out"; fi

reset_copy
echo 'the box was i-0a''bc1234def5678ab at the time' > "$TMP/tree/$PLANTED"
out="$(bash "$GUARD" --infra-ids "$TMP/tree" 2>&1)"; rc=$?
if [ "$rc" = 1 ] && grep -F "$PLANTED" <<<"$out" >/dev/null; then pass "planted instance id is caught"
else fail "planted instance id is caught" "rc=$rc out=$out"; fi

reset_copy
echo "bucket s3://something-${ACCOUNT_ID}/x" > "$TMP/tree/$PLANTED"
out="$(bash "$GUARD" --infra-ids "$TMP/tree" 2>&1)"; rc=$?
if [ "$rc" = 1 ] && grep -F "$PLANTED" <<<"$out" >/dev/null; then pass "planted account id is caught"
else fail "planted account id is caught" "rc=$rc out=$out"; fi

reset_copy
echo "bucket s3://something-${ACCOUNT_ID}/x" > "$TMP/tree/$PLANTED"
echo "$PLANTED:1" >> "$TMP/tree/$ALLOWLIST"
if out="$(bash "$GUARD" --infra-ids "$TMP/tree" 2>&1)"; then pass "allowlisted path:line is not a finding"
else fail "allowlisted path:line is not a finding" "$out"; fi

reset_copy
echo "$PLANTED:does-not-exist" >> "$TMP/tree/$ALLOWLIST"
out="$(bash "$GUARD" --infra-ids "$TMP/tree" 2>&1)"; rc=$?
if [ "$rc" = 1 ] && grep -F 'stale allowlist' <<<"$out" >/dev/null; then pass "stale allowlist entry is a finding"
else fail "stale allowlist entry is a finding" "rc=$rc out=$out"; fi

reset_copy
echo 'fixture i-0123456789abcdef0, subnet-0123456789abcdef1 and sg-00000000 are fine' > "$TMP/tree/$PLANTED"
if out="$(bash "$GUARD" --infra-ids "$TMP/tree" 2>&1)"; then pass "fixture spellings are not findings"
else fail "fixture spellings are not findings" "$out"; fi

out="$(bash "$GUARD" --bogus "$TMP/tree" 2>&1)"; rc=$?
if [ "$rc" = 2 ]; then pass "unknown mode is a usage error"
else fail "unknown mode is a usage error" "rc=$rc out=$out"; fi

# --- strings ---------------------------------------------------------------
reset_copy
if out="$(bash "$GUARD" --strings "$TMP/tree" 2>&1)"; then pass "strings clean on committed tree"
else fail "strings clean on committed tree" "$out"; fi

reset_copy
echo "see https://${DEAD_HOST}/beads for the UI" > "$TMP/tree/$PLANTED"
out="$(bash "$GUARD" --strings "$TMP/tree" 2>&1)"; rc=$?
if [ "$rc" = 1 ] && grep -F "$PLANTED" <<<"$out" >/dev/null; then pass "planted dead host is caught"
else fail "planted dead host is caught" "rc=$rc out=$out"; fi

reset_copy
echo "clone https://${GROUP_PATH}/gardenio" > "$TMP/tree/$PLANTED"
out="$(bash "$GUARD" --strings "$TMP/tree" 2>&1)"; rc=$?
if [ "$rc" = 1 ] && grep -F "$PLANTED" <<<"$out" >/dev/null; then pass "planted GitLab group path is caught"
else fail "planted GitLab group path is caught" "rc=$rc out=$out"; fi

reset_copy
echo "see https://${DEAD_HOST}/beads for the UI" > "$TMP/tree/$PLANTED"
out="$(bash "$GUARD" --all "$TMP/tree" 2>&1)"; rc=$?
if [ "$rc" = 1 ] && grep -F "$PLANTED" <<<"$out" >/dev/null; then pass "--all runs the strings mode"
else fail "--all runs the strings mode" "rc=$rc out=$out"; fi

reset_copy
printf 'acmeServer: https://acme-v02.api.letsencrypt.org/directory\npath: /.well-known/acme-challenge/x\n' > "$TMP/tree/$PLANTED"
if out="$(bash "$GUARD" --strings "$TMP/tree" 2>&1)"; then pass "Let's Encrypt acme is not a finding"
else fail "Let's Encrypt acme is not a finding" "$out"; fi

reset_copy
mkdir -p "$(dirname "$TMP/tree/$TESTDATA")"
echo "fixture host ${DEAD_HOST}" > "$TMP/tree/$TESTDATA"
if out="$(bash "$GUARD" --strings "$TMP/tree" 2>&1)"; then pass "testdata is exempt from --strings"
else fail "testdata is exempt from --strings" "$out"; fi

echo "lint-public-strings_test: $PASS passed, $FAIL failed"
[ "$FAIL" = 0 ]
