#!/usr/bin/env bash
# check-merge-conflict-markers_test.sh — Unit tests for
# check-merge-conflict-markers.sh
#
# Each test writes a synthetic file to a temp dir and asserts the guard's exit
# status when pointed at it (with an explicit-file argument, so the test never
# depends on the real git index). No external services. Runs in the main shell
# so the pass/fail counters are reliable.
#
# The final test invokes the guard against the REAL tracked tree — a regression
# guard so a future conflict-marker commit reds immediately, not just synthetic
# fixtures. This is exactly the check that would have caught be105c652 before it
# skip-cascaded release e2e (ka-0000001064).
#
# Usage:  bash scripts/check-merge-conflict-markers_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPT_UNDER_TEST="${SCRIPT_DIR}/check-merge-conflict-markers.sh"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

PASS=0
FAIL=0
TMPDIR_T="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_T"' EXIT

pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1: $2"; FAIL=$((FAIL + 1)); }

# assert_exit <expected> <name> <file..>
assert_exit() {
  local expected="$1" name="$2"; shift 2
  local out rc
  out="$(bash "$SCRIPT_UNDER_TEST" "$@" 2>&1)" && rc=0 || rc=$?
  if [[ "$rc" == "$expected" ]]; then
    pass "$name"
  else
    fail "$name" "expected exit ${expected}, got ${rc}. Output: ${out}"
  fi
}

# The seven-sigil markers are assembled at runtime (never written as a raw
# line-leading literal in this file) so the guard's own live-tree scan below
# does not flag this test as a violation.
OPEN="$(printf '%s' '<<<<<<<') HEAD"
DIV="$(printf '%s' '=======')"
CLOSE="$(printf '%s' '>>>>>>>') cb955cac8 (fix (ka-0000000829))"

# 1. A clean file passes.
clean="${TMPDIR_T}/clean.yaml"
printf 'a: 1\nb: 2\n' >"$clean"
assert_exit 0 "clean file passes" "$clean"

# 2. A file with a full conflict block fails LOUDLY.
conflict="${TMPDIR_T}/conflict.yaml"
{
  printf 'key:\n'
  printf '%s\n' "$OPEN"
  printf '  # ours\n'
  printf '%s\n' "$DIV"
  printf '  # theirs\n'
  printf '%s\n' "$CLOSE"
  printf '  value: x\n'
} >"$conflict"
assert_exit 1 "full conflict block fails" "$conflict"

# 3. Just the opener marker (partial/hand-mangled resolution) still fails.
opener_only="${TMPDIR_T}/opener.txt"
{ printf 'top\n'; printf '%s\n' "$OPEN"; printf 'bottom\n'; } >"$opener_only"
assert_exit 1 "opener-only marker fails" "$opener_only"

# 4. Just the closer marker still fails.
closer_only="${TMPDIR_T}/closer.txt"
{ printf 'top\n'; printf '%s\n' "$CLOSE"; printf 'bottom\n'; } >"$closer_only"
assert_exit 1 "closer-only marker fails" "$closer_only"

# 5. A Markdown setext H1 underline (a legit run of '=') must NOT false-positive.
setext="${TMPDIR_T}/README.md"
{ printf 'Title\n'; printf '==========\n'; printf '\nbody\n'; } >"$setext"
assert_exit 0 "markdown setext underline does not false-positive" "$setext"

# 6. A binary file carrying marker bytes must NOT be reported.
#    This pins the BEHAVIOUR, not the `-I` flag: deleting -I was measured to
#    leave this case green on both greps to hand (GNU grep 3.11 in the runner
#    image routes `binary file matches` to stderr, and the ugrep that shadows
#    grep on at least one dev box skips binaries unconditionally). Do not read
#    a pass here as evidence that -I is still doing something -- the guard
#    explains separately why it is kept.
#    The leading bytes are NULs from /dev/zero, not /dev/urandom: 64 random
#    bytes contain no NUL about 78% of the time, so a urandom fixture is
#    "binary" only most of the time and the case would pass for the wrong
#    reason on the other runs.
binary="${TMPDIR_T}/blob.dat"
{ head -c 64 /dev/zero; printf '%s\n' "$OPEN"; } >"$binary"
assert_exit 0 "a binary file containing a marker is skipped" "$binary"

# 7. A path with a space in it must still be scanned. The whole-tree scan pipes
#    paths through `xargs -0`; a regression to newline or whitespace splitting
#    would silently stop scanning such files rather than fail.
spaced="${TMPDIR_T}/sp ace.yaml"
{ printf 'top\n'; printf '%s\n' "$CLOSE"; } >"$spaced"
assert_exit 1 "a path containing a space is still scanned" "$spaced"

# 8. REGRESSION GUARD: the real tracked tree must be free of conflict markers.
#    (No explicit file args -> the guard enumerates `git ls-files` from cwd.)
if command -v git >/dev/null 2>&1 && git -C "$REPO_ROOT" rev-parse >/dev/null 2>&1; then
  out="$(cd "$REPO_ROOT" && bash "$SCRIPT_UNDER_TEST" 2>&1)" && rc=0 || rc=$?
  if [[ "$rc" == "0" ]]; then
    pass "live tracked tree has no conflict markers"
  else
    fail "live tracked tree has no conflict markers" "guard reported violations: ${out}"
  fi

  # 9. ...and it must have actually READ that tree. A guard whose file list
  #    collapsed to zero exits 0 and prints the same success line as a real
  #    clean scan, so the count is the only thing separating "clean" from
  #    "scanned nothing". The floor is deliberately far below the ~7,650 files
  #    tracked today; it is a collapse detector, not a size assertion.
  scanned="$(printf '%s' "$out" | sed -n 's/.*scanned \([0-9]\{1,\}\) files.*/\1/p')"
  if [[ -n "$scanned" && "$scanned" -ge 1000 ]]; then
    pass "live-tree scan actually read the tree (${scanned} files)"
  else
    fail "live-tree scan actually read the tree" \
      "expected a 'scanned N files' count >= 1000, got '${scanned:-<none>}'. Output: ${out}"
  fi
else
  echo "SKIP: not in a git repo; skipping live-tree regression check"
fi

echo ""
echo "PASS=${PASS} FAIL=${FAIL}"
[[ "$FAIL" -eq 0 ]]
