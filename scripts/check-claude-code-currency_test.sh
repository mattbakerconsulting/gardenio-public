#!/usr/bin/env bash
# check-claude-code-currency_test.sh — Unit tests for check-claude-code-currency.sh
#
# Each test builds a synthetic mini-repo (just versions.env) in a temp dir and
# injects the upstream version via CLAUDE_CODE_UPSTREAM, so NO test touches the
# network or the real npm registry — the suite is hermetic and runs offline.
#
# The last test points the guard at the REAL repo with an injected upstream
# equal to its own pin, proving the real versions.env is parseable (a rename or
# reformat of the pin line reds here rather than in a release cycle).
#
# Usage:  bash scripts/check-claude-code-currency_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPT_UNDER_TEST="${SCRIPT_DIR}/check-claude-code-currency.sh"
REAL_REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

PASS=0
FAIL=0
TMPDIR_T="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_T"' EXIT

pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1: $2"; FAIL=$((FAIL + 1)); }

# make_fixture <dir> <pinned_version_line>
#   <pinned_version_line> is written verbatim after `export CLAUDE_CODE_VERSION=`
#   so tests can exercise quoting and malformed values, not just clean semver.
make_fixture() {
  local dir="$1" pin="$2"
  mkdir -p "${dir}/gardenio/images/agent/install"
  printf 'export NODE_FULL_VERSION=24.15.0\nexport CLAUDE_CODE_VERSION=%s\nexport YQ_VERSION=1.2.3\n' \
    "$pin" > "${dir}/gardenio/images/agent/install/versions.env"
}

# assert_exit <expected> <name> <fixture_dir> <upstream>
assert_exit() {
  local expected="$1" name="$2" dir="$3" upstream="$4"
  local out rc
  out="$(REPO_ROOT="$dir" CLAUDE_CODE_UPSTREAM="$upstream" bash "$SCRIPT_UNDER_TEST" 2>&1)" && rc=0 || rc=$?
  if [[ "$rc" == "$expected" ]]; then
    pass "$name"
  else
    fail "$name" "expected exit ${expected}, got ${rc}. Output: ${out}"
  fi
}

# ── 1. Pin equal to upstream stable is in sync ──────────────────────────
D="${TMPDIR_T}/insync"
make_fixture "$D" 2.1.231
assert_exit 0 "pin == upstream stable passes" "$D" 2.1.231

# ── 2. THE REGRESSION (ka-0000007531): the exact measured drift ─────────
# On 2026-08-25 the image pinned 2.1.211 while npm `stable` was 2.1.231, and
# nothing anywhere noticed. This is the case the guard exists for.
D="${TMPDIR_T}/drift"
make_fixture "$D" 2.1.211
assert_exit 2 "the measured 2.1.211 -> 2.1.231 drift is caught" "$D" 2.1.231

# ── 3. A pin AHEAD of stable is deliberate, not drift ───────────────────
# Bumping to a `latest`-channel build ahead of `stable` is a real thing the
# fleet has done for early access; it must not red the release cycle.
D="${TMPDIR_T}/ahead"
make_fixture "$D" 2.1.246
assert_exit 0 "a pin ahead of stable is reported, not failed" "$D" 2.1.231

# ── 4. Version comparison is numeric, not lexicographic ─────────────────
# The trap: string-compare says "2.1.9" > "2.1.10". A lexicographic guard would
# call this drift-free while the image sits 21 releases behind.
D="${TMPDIR_T}/numeric"
make_fixture "$D" 2.1.9
assert_exit 2 "2.1.9 is behind 2.1.10 (numeric compare, not string)" "$D" 2.1.10

# ── 5. …and in the other direction ──────────────────────────────────────
D="${TMPDIR_T}/numeric-ahead"
make_fixture "$D" 2.1.100
assert_exit 0 "2.1.100 is ahead of 2.1.99 (numeric compare)" "$D" 2.1.99

# ── 6. A quoted pin parses ──────────────────────────────────────────────
D="${TMPDIR_T}/quoted"
make_fixture "$D" '"2.1.231"'
assert_exit 0 "a double-quoted pin value parses" "$D" 2.1.231

# ── 7. Missing versions.env fails loudly rather than passing vacuously ──
D="${TMPDIR_T}/novers"
mkdir -p "$D"
assert_exit 1 "missing versions.env fails loudly" "$D" 2.1.231

# ── 8. An unset pin fails loudly (not treated as in-sync) ───────────────
D="${TMPDIR_T}/nopin"
mkdir -p "${D}/gardenio/images/agent/install"
printf 'export NODE_FULL_VERSION=24.15.0\n' > "${D}/gardenio/images/agent/install/versions.env"
assert_exit 1 "an absent CLAUDE_CODE_VERSION fails loudly" "$D" 2.1.231

# ── 9. A non-semver pin fails loudly rather than mis-comparing ──────────
D="${TMPDIR_T}/garbage"
make_fixture "$D" 'latest'
assert_exit 1 "a non-semver pin fails loudly" "$D" 2.1.231

# ── 10. A non-semver upstream fails loudly ──────────────────────────────
# Guards against a registry response shape change silently reading as in-sync.
D="${TMPDIR_T}/garbage-upstream"
make_fixture "$D" 2.1.231
assert_exit 1 "a non-semver upstream fails loudly" "$D" 'null'

# ── 11. --json emits the documented shape ───────────────────────────────
D="${TMPDIR_T}/json"
make_fixture "$D" 2.1.211
JSON_OUT="$(REPO_ROOT="$D" CLAUDE_CODE_UPSTREAM=2.1.231 bash "$SCRIPT_UNDER_TEST" --json 2>&1)" || true
if [[ "$(printf '%s' "$JSON_OUT" | jq -r '[.pinned, .upstream, .status, (.drift|tostring)] | join(",")')" \
      == "2.1.211,2.1.231,behind,true" ]]; then
  pass "--json emits pinned/upstream/status/drift"
else
  fail "--json emits pinned/upstream/status/drift" "got: ${JSON_OUT}"
fi

# ── 12. An unknown argument is rejected ─────────────────────────────────
D="${TMPDIR_T}/badarg"
make_fixture "$D" 2.1.231
if REPO_ROOT="$D" CLAUDE_CODE_UPSTREAM=2.1.231 bash "$SCRIPT_UNDER_TEST" --bogus >/dev/null 2>&1; then
  fail "an unknown argument is rejected" "expected non-zero exit"
else
  pass "an unknown argument is rejected"
fi

# ── 13. The REAL repo's versions.env is parseable ───────────────────────
# Injects the repo's own pin as upstream, so this asserts PARSEABILITY only and
# never reds merely because upstream moved (that drift is the release-owner's
# cycle signal, not a unit-test failure).
REAL_PIN="$(sed -n 's/^export CLAUDE_CODE_VERSION=//p' \
  "${REAL_REPO_ROOT}/gardenio/images/agent/install/versions.env" | head -1 | tr -d '"'"'"' \t\r')"
if [[ -n "$REAL_PIN" ]]; then
  assert_exit 0 "real repo versions.env pin (${REAL_PIN}) is parseable" "$REAL_REPO_ROOT" "$REAL_PIN"
else
  fail "real repo versions.env pin is parseable" "could not read CLAUDE_CODE_VERSION"
fi

echo
echo "check-claude-code-currency_test: ${PASS} passed, ${FAIL} failed"
[[ "$FAIL" -eq 0 ]]
