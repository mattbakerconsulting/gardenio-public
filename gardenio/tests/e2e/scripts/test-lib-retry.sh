#!/usr/bin/env bash
# test-lib-retry.sh — unit test for lib-retry.sh.
#
# Exercises retry_cmd, kd_retry, curl_retry, retry_on_empty without any
# cluster dependency. Can run in CI (lint job) or on a dev laptop.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib-retry.sh"

# Use short delays so the test runs fast.
export RETRY_ATTEMPTS=3
export RETRY_DELAY=0

PASS=0
FAIL=0
ERRORS=()

pass() { PASS=$((PASS + 1)); echo "  PASS: $1"; }
fail() { FAIL=$((FAIL + 1)); ERRORS+=("$1"); echo "  FAIL: $1"; }

# ── retry_cmd: success on first try ───────────────────────────────
if retry_cmd true; then
  pass "retry_cmd: true succeeds on first try"
else
  fail "retry_cmd: true should succeed"
fi

# ── retry_cmd: failure after all attempts ─────────────────────────
if retry_cmd false; then
  fail "retry_cmd: false should never succeed"
else
  rc=$?
  if [ "$rc" -eq 1 ]; then
    pass "retry_cmd: propagates exit code (got $rc)"
  else
    fail "retry_cmd: expected exit 1, got $rc"
  fi
fi

# ── retry_cmd: flaky command that succeeds on retry ───────────────
FLAKY_STATE_FILE=$(mktemp)
trap 'rm -f "$FLAKY_STATE_FILE"' EXIT
echo 0 > "$FLAKY_STATE_FILE"
flaky() {
  local n
  n=$(cat "$FLAKY_STATE_FILE")
  n=$((n + 1))
  echo "$n" > "$FLAKY_STATE_FILE"
  # fail first attempt, succeed second
  [ "$n" -ge 2 ]
}
if retry_cmd flaky; then
  attempts=$(cat "$FLAKY_STATE_FILE")
  if [ "$attempts" -eq 2 ]; then
    pass "retry_cmd: flaky cmd succeeds on 2nd attempt"
  else
    fail "retry_cmd: flaky should have run 2x, ran ${attempts}x"
  fi
else
  fail "retry_cmd: flaky should eventually succeed"
fi

# ── retry_cmd: stdout passes through on success ───────────────────
out=$(retry_cmd printf 'hello\n')
if [ "$out" = "hello" ]; then
  pass "retry_cmd: stdout passes through"
else
  fail "retry_cmd: expected 'hello', got '$out'"
fi

# ── kd_retry: delegates to retry_cmd (smoke) ──────────────────────
if kd_retry true; then
  pass "kd_retry: delegates to retry_cmd"
else
  fail "kd_retry: true should succeed"
fi

# ── retry_on_empty: returns output on first non-empty ─────────────
out=$(retry_on_empty printf 'data\n')
if [ "$out" = "data" ]; then
  pass "retry_on_empty: returns non-empty output"
else
  fail "retry_on_empty: expected 'data', got '$out'"
fi

# ── retry_on_empty: retries on empty output ───────────────────────
echo 0 > "$FLAKY_STATE_FILE"
flaky_output() {
  local n
  n=$(cat "$FLAKY_STATE_FILE")
  n=$((n + 1))
  echo "$n" > "$FLAKY_STATE_FILE"
  # empty on attempt 1, "ok" on attempt 2+
  if [ "$n" -ge 2 ]; then printf 'ok\n'; fi
}
out=$(retry_on_empty flaky_output)
attempts=$(cat "$FLAKY_STATE_FILE")
if [ "$out" = "ok" ] && [ "$attempts" -eq 2 ]; then
  pass "retry_on_empty: retries past empty output, got '$out' in ${attempts} attempt(s)"
else
  fail "retry_on_empty: expected 'ok' in 2 attempts, got '$out' in $attempts"
fi

# ── retry_on_empty: gives up after RETRY_ATTEMPTS ─────────────────
out=$(retry_on_empty true)   # true exits 0 with empty stdout
if [ -z "$out" ]; then
  pass "retry_on_empty: empty output after all retries returns empty"
else
  fail "retry_on_empty: expected empty, got '$out'"
fi

# ── curl_retry: uses retry_cmd, fails cleanly on bogus URL ────────
# Short-circuits via RETRY_ATTEMPTS=3 and curl --max-time so this stays fast.
# We hit a loopback port that should not be listening — connection refused.
if curl_retry -sf "http://127.0.0.1:1/__nope" >/dev/null 2>&1; then
  fail "curl_retry: bogus URL should not succeed"
else
  pass "curl_retry: propagates failure after retries"
fi

# ── Double-source guard ───────────────────────────────────────────
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib-retry.sh"
if [ "${__GARDENIO_LIB_RETRY_LOADED}" = "1" ]; then
  pass "lib-retry.sh: double-source guard intact"
else
  fail "lib-retry.sh: guard variable missing after re-source"
fi

# ── Summary ───────────────────────────────────────────────────────
echo
echo "Results: $PASS passed, $FAIL failed"
if [ "$FAIL" -gt 0 ]; then
  for err in "${ERRORS[@]}"; do
    echo "  - $err"
  done
  exit 1
fi
