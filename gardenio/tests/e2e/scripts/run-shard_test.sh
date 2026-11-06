#!/usr/bin/env bash
# run-shard_test.sh -- every module run-shard.sh registers names a script that
# exists. A module whose script was deleted without its registry entry would
# otherwise fail only when its shard runs (the e2e workflow is optional).
#
# Usage: gardenio/tests/e2e/scripts/run-shard_test.sh   (from anywhere)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
SHARD="${ROOT}/gardenio/tests/e2e/scripts/run-shard.sh"

PASS=0
FAIL=0
pass() { PASS=$((PASS + 1)); echo "PASS: $1"; }
fail() { FAIL=$((FAIL + 1)); echo "FAIL: $1 -- $2"; }

# run_module "<name>" "<path>" lines; paths are relative to the repo root.
mapfile -t ENTRIES < <(grep -o 'run_module "[^"]*" "[^"]*"' "$SHARD" | awk -F'"' '{print $2 " " $4}')

if [ "${#ENTRIES[@]}" -gt 0 ]; then
  pass "registry_found (${#ENTRIES[@]} modules)"
else
  fail "registry_found" "no run_module \"<name>\" \"<path>\" lines in ${SHARD}"
fi

missing=()
for entry in "${ENTRIES[@]}"; do
  name="${entry%% *}"
  path="${entry#* }"
  [ -f "${ROOT}/${path#./}" ] || missing+=("${name} -> ${path}")
done
if [ "${#missing[@]}" -eq 0 ]; then
  pass "every_module_script_exists"
else
  fail "every_module_script_exists" "$(printf '%s; ' "${missing[@]}")"
fi

# The retired gb release status module stays gone (plan gd-UZKZ9H25UD step 3).
if grep -q 'test-release-status.sh' "$SHARD"; then
  fail "release_status_module_retired" "run-shard.sh still runs test-release-status.sh"
else
  pass "release_status_module_retired"
fi

echo
echo "run-shard_test: ${PASS} passed, ${FAIL} failed"
[ "$FAIL" -eq 0 ]
