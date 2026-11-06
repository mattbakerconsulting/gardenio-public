#!/usr/bin/env bash
# check-large-tracked-files_test.sh — Unit tests for
# check-large-tracked-files.sh
#
# Each test writes a synthetic file of a known size to a temp dir and asserts
# the guard's exit status when pointed at it (with an explicit-file argument, so
# the test never depends on the real git index). No external services. Runs in
# the main shell so the pass/fail counters are reliable.
#
# The final test invokes the guard against the REAL tracked tree with the
# production 3 MiB ceiling — a regression guard so a future large-blob commit
# reds immediately, not just synthetic fixtures.
#
# Usage:  bash scripts/check-large-tracked-files_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPT_UNDER_TEST="${SCRIPT_DIR}/check-large-tracked-files.sh"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

PASS=0
FAIL=0
TMPDIR_T="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_T"' EXIT

pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1: $2"; FAIL=$((FAIL + 1)); }

# assert_exit <expected> <name> <max_bytes> <file..>
assert_exit() {
  local expected="$1" name="$2" max="$3"; shift 3
  local out rc
  out="$(MAX_BYTES="$max" bash "$SCRIPT_UNDER_TEST" "$@" 2>&1)" && rc=0 || rc=$?
  if [[ "$rc" == "$expected" ]]; then
    pass "$name"
  else
    fail "$name" "expected exit ${expected}, got ${rc}. Output: ${out}"
  fi
}

# Build a small (1 KiB) and a large (4 MiB) fixture file.
small="${TMPDIR_T}/small.bin"
large="${TMPDIR_T}/large.bin"
head -c 1024 /dev/zero >"$small"
head -c 4194304 /dev/zero >"$large"   # 4 MiB > 3 MiB default ceiling

# 1. A file under the ceiling passes.
assert_exit 0 "under-ceiling file passes" 3145728 "$small"

# 2. A file over the ceiling fails LOUDLY.
assert_exit 1 "over-ceiling file fails" 3145728 "$large"

# 3. The ceiling is honored: same large file passes when MAX_BYTES is raised.
assert_exit 0 "raising MAX_BYTES lets a big file pass" 5242880 "$large"

# 4. A mix (one small, one large) fails because at least one violates.
assert_exit 1 "mixed batch with one violation fails" 3145728 "$small" "$large"

# 5. REGRESSION GUARD: the real tracked tree must be clean at the production
#    3 MiB ceiling. If a future commit adds a >3MB blob this reds immediately.
#    (No explicit file args -> the guard enumerates `git ls-files` from cwd.)
if command -v git >/dev/null 2>&1 && git -C "$REPO_ROOT" rev-parse >/dev/null 2>&1; then
  out="$(cd "$REPO_ROOT" && MAX_BYTES=3145728 bash "$SCRIPT_UNDER_TEST" 2>&1)" && rc=0 || rc=$?
  if [[ "$rc" == "0" ]]; then
    pass "live tracked tree is under the 3MB budget"
  else
    fail "live tracked tree is under the 3MB budget" "guard reported violations: ${out}"
  fi

  # 6. ...and it must have actually READ that tree. A guard whose file list
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

  # 7. The guard reads real sizes, not just the synthetic fixtures: driving
  #    the ceiling under the tree's biggest files must red. (This used to also
  #    prove the allowlist exempted the vendored mermaid bundle; that file is
  #    fetched at build time now and the allowlist is empty, gd-wNQzR9ptZI.)
  out="$(cd "$REPO_ROOT" && MAX_BYTES=100000 bash "$SCRIPT_UNDER_TEST" 2>&1)" && rc=0 || rc=$?
  # Substring match with a bash builtin rather than `printf | grep -q`: the
  # early-exiting grep would SIGPIPE the producer, which is what
  # check-sigpipe-producers.py exists to catch (and did catch here).
  if [[ "$rc" == "1" && "$out" == *"TOO LARGE:"* ]]; then
    pass "live tree reds when the ceiling drops under its largest files"
  else
    fail "live tree reds when the ceiling drops under its largest files" \
      "expected exit 1 with a TOO LARGE report, got rc=${rc}. Output: ${out}"
  fi
else
  echo "SKIP: not in a git repo; skipping live-tree regression check"
fi

echo ""
echo "PASS=${PASS} FAIL=${FAIL}"
[[ "$FAIL" -eq 0 ]]
