#!/usr/bin/env bash
# lint-glab-mr-flags_test.sh — Unit tests for lint-glab-mr-flags.sh
#
# Each test writes a synthetic line to a temp file and asserts the lint's exit
# status when scanning that explicit file. No external services, no git.
#
# Usage:  bash scripts/lint-glab-mr-flags_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPT_UNDER_TEST="${SCRIPT_DIR}/lint-glab-mr-flags.sh"

PASS=0
FAIL=0
TMPDIR_T="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_T"' EXIT

pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1: $2"; FAIL=$((FAIL + 1)); }

# assert_exit <expected> <name> <file-content>
assert_exit() {
  local expected="$1" name="$2" content="$3"
  local f out rc
  f="$(mktemp "${TMPDIR_T}/desc.XXXXXX")"
  printf '%s\n' "$content" > "$f"
  out="$(bash "$SCRIPT_UNDER_TEST" "$f" 2>&1)" && rc=0 || rc=$?
  if [[ "$rc" == "$expected" ]]; then
    pass "$name"
  else
    fail "$name" "expected exit ${expected}, got ${rc}. Output: ${out}"
  fi
}

# --- Violations (exit 1) ---
assert_exit 1 "state=opened (gh-style, =)"      'glab mr list --state=opened --per-page 100'
assert_exit 1 "state opened (gh-style, space)"  'glab mr list --state opened'
assert_exit 1 "state=merged in KPI"             'glab mr list --state=merged --per-page=100 | grep -c merged'
assert_exit 1 "state closed with author"        'glab mr list --state closed --author group_x'
assert_exit 1 "bare --opened flag"              'glab mr list --opened --per-page=50'

# --- Clean (exit 0) ---
assert_exit 0 "bare list (open default)"        'glab mr list --per-page=50'
assert_exit 0 "correct --merged"                'glab mr list --merged --per-page=100 | grep -c "^!"'
assert_exit 0 "correct -M short flag"           'glab mr list -M --per-page 30'
assert_exit 0 "correct --closed"                'glab mr list --closed --author group_x'
assert_exit 0 "prose mentioning state word"     'The MR state column shows opened/merged.'

# --- Exempt teaching/warning lines (exit 0 despite matching the pattern) ---
assert_exit 0 "warn: Unknown flag marker"       'Do NOT use glab mr list --state=opened — it errors Unknown flag: --state.'
assert_exit 0 "warn: deprecated marker"         'glab mr list --opened is deprecated; use bare glab mr list.'
assert_exit 0 "explicit glab-lint-ok token"     'glab mr list --state=opened  # glab-lint-ok: example of the bad form'

echo
echo "lint-glab-mr-flags_test: ${PASS} passed, ${FAIL} failed"
[[ "$FAIL" -eq 0 ]]
