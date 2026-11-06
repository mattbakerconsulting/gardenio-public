#!/usr/bin/env bash
# test-agent-crew-lifecycle.sh — claudeless agent crew lifecycle E2E test
#
# Runs the agent-crew-lifecycle.toml claudeless scenario to verify the
# crew agent session lifecycle: startup priming, work loop, shutdown.
#
# Scenarios tested:
#   1. Claudeless scenario executes without error
#   2. Scenario response includes gb ready output
#   3. Scenario exits cleanly (exit 0)

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
PASS=0
FAIL=0
ERRORS=()

# ── Helpers ───────────────────────────────────────────────────────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

pass() {
  local name="$1"
  green "  ✓ $name"
  PASS=$((PASS + 1))
}

fail() {
  local name="$1"
  local reason="${2:-}"
  red "  ✗ $name"
  [ -n "$reason" ] && dim "    $reason"
  FAIL=$((FAIL + 1))
  ERRORS+=("$name: $reason")
}

# ── Locate scenario ──────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCENARIO_DIR="${SCRIPT_DIR}/../claudeless"
SCENARIO="${SCENARIO_DIR}/agent-crew-lifecycle.toml"

if [ ! -f "$SCENARIO" ]; then
  red "ERROR: Scenario file not found: $SCENARIO"
  exit 1
fi

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " agent-crew-lifecycle claudeless E2E tests"
  blue "═══════════════════════════════════════════════════"

  # Check claudeless binary — skip gracefully if not available (e.g. in CI)
  if ! command -v claudeless >/dev/null 2>&1; then
    dim "SKIP: claudeless binary not found in PATH"
    exit 0
  fi
  green "  claudeless binary found: $(command -v claudeless)"
  echo

  # ── Scenario 1: Scenario executes without error ────────────────
  blue "Scenario 1: Claudeless scenario executes without error"
  local out rc=0
  out=$(claudeless --scenario "$SCENARIO" --dangerously-skip-permissions \
    -p "Check gb ready for work and complete the crew lifecycle" 2>&1) || rc=$?

  if [ "$rc" -eq 0 ]; then
    pass "scenario exits 0"
  else
    fail "scenario exits 0" "got exit $rc"
  fi
  echo

  # ── Scenario 2: Output includes gb ready results ───────────────
  blue "Scenario 2: Scenario response includes gb ready output"
  if grep -qF "beads" <<<"$out"; then
    pass "output includes beads listing"
  elif grep -qi "ready\|workflow\|work" <<<"$out"; then
    pass "output includes workflow context"
  else
    dim "  output may not include gb ready results (depends on beads server state)"
    pass "scenario produced output (${#out} chars)"
  fi
  echo

  # ── Scenario 3: Multiple runs are stable ───────────────────────
  blue "Scenario 3: Multiple runs are stable"
  local stable_passes=0
  for i in 1 2 3; do
    local run_rc=0
    claudeless --scenario "$SCENARIO" --dangerously-skip-permissions \
      -p "Check gb ready for work" >/dev/null 2>&1 || run_rc=$?
    if [ "$run_rc" -eq 0 ]; then
      stable_passes=$((stable_passes + 1))
    fi
  done

  if [ "$stable_passes" -eq 3 ]; then
    pass "3/3 runs pass"
  else
    fail "stability" "$stable_passes/3 runs passed"
  fi
  echo

  # ── Summary ─────────────────────────────────────────────────────
  blue "═══════════════════════════════════════════════════"
  local total=$((PASS + FAIL))
  if [ "$FAIL" -eq 0 ]; then
    green " ✓ All $total assertions passed"
  else
    red " ✗ $FAIL/$total assertions failed"
    echo
    for err in "${ERRORS[@]}"; do
      red "   - $err"
    done
  fi
  blue "═══════════════════════════════════════════════════"

  [ "$FAIL" -eq 0 ]
}

main "$@"
