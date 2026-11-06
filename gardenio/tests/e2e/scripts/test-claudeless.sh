#!/usr/bin/env bash
# test-claudeless.sh — Run all claudeless TOML scenarios
#
# Executes each .toml file in gardenio/tests/e2e/claudeless/ using the
# claudeless binary. These scenarios simulate Claude Code sessions without
# API calls, testing hook behavior, gate enforcement, and agent lifecycle.
#
# Usage:
#   ./tests/e2e/scripts/test-claudeless.sh [--namespace <ns>]
#
# Prerequisites:
#   - claudeless binary in PATH (ships with Claude Code)
#   - .claude/settings.local.json at the repo root (for hook config)

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCENARIO_DIR="$SCRIPT_DIR/../claudeless"
PASS=0
FAIL=0
SKIP=0
ERRORS=()

# Scenarios that require external driving (e.g. SIGTERM from a parent runner)
# and must NOT be auto-discovered by this standalone runner. They are exercised
# by test-local-mode.sh, which invokes them through `gb agent start --local`
# and signals the wrapping process directly.
#
# QUARANTINE (ka-0000005245): the gardener-trigger-* live-flow scenarios added
# by d343eb18 (ka-0000002765) each wait through ≥1 scheduler reconcile tick
# (~60s COOP_SYNC_INTERVAL) plus assertion polling — every one of them runs
# ~150–240s. Adding nine of them to the `claudeless` module on shard 0 pushed
# the shard past its 15-min shard timeout (the shard wedged mid-suite on the
# 2026.134.2 release; see ka-0000005245). Quarantining them here while the
# follow-up (faster reconcile in test mode, or a dedicated sub-shard) lands.
SKIP_SCENARIOS=(
  "local-sigterm"                              # needs external SIGTERM; sleeps 60s otherwise → false-positive failure
  "onboarding-register-project"                # needs a poured molecule: test-onboarding-formulas.sh drives these four
  "onboarding-add-local-agent"
  "onboarding-first-task"
  "onboarding-enable-slack"
  "gardener-trigger-bead-closed-match-parent"   # ka-0000005245: ~158s, reconcile-bound
  "gardener-trigger-bead-created-match-parent"  # ka-0000005245: ~157s, reconcile-bound
  "gardener-trigger-bead-event-in-subtree"      # ka-0000005245: ~158s, reconcile-bound
  "gardener-trigger-bead-event-label-predicate" # ka-0000005245: ~240s (90s NEG + 150s POS)
  "gardener-trigger-debounce-coalesce"          # ka-0000005245: ~180s
  "gardener-trigger-decision-option-chosen"     # ka-0000005245: ~240s (90s NEG + 150s POS)
  "gardener-trigger-decision-resolved-match"    # ka-0000005245: ~150s
  "gardener-trigger-fan-in-cron-precedence"     # ka-0000005245: ~150s
  "gardener-cron-floor-preflight-suppress"      # gd-1FuVhV0aAG: ~180s (90s suppress + 90s fire), cron-boundary-bound
  "gardener-trigger-throttle-burst"             # ka-0000005245: ~225s
  "gardener-mode-flip-event-fires-once"         # gd-BvYhL4uRfu: ~300s (fire poll 150s + out-of-subtree hold 130s), reconcile-bound
  "trigger-source-bead-spawn-agent"            # gd-DLMCirbsRl: ~160s, reconcile/restart-bound (same class as gardener-trigger-*)
)

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

skip() {
  local name="$1"
  local reason="${2:-}"
  dim "  ⊘ $name (skipped: $reason)"
  SKIP=$((SKIP + 1))
}

# ── Pre-flight ────────────────────────────────────────────────────
blue "═══════════════════════════════════════════════════"
blue "  Claudeless Scenario Runner"
blue "═══════════════════════════════════════════════════"
echo

# Verify claudeless is available — skip gracefully if not present (e.g. in CI)
if ! command -v claudeless &>/dev/null; then
  dim "SKIP: claudeless binary not found in PATH"
  exit 0
fi
dim "claudeless: $(which claudeless)"

# Verify scenario directory exists
if [ ! -d "$SCENARIO_DIR" ]; then
  red "ERROR: Scenario directory not found: $SCENARIO_DIR"
  exit 1
fi

# Count scenarios
TOML_FILES=("$SCENARIO_DIR"/*.toml)
if [ ${#TOML_FILES[@]} -eq 0 ]; then
  red "ERROR: No .toml files found in $SCENARIO_DIR"
  exit 1
fi
blue "Found ${#TOML_FILES[@]} scenario(s) in $SCENARIO_DIR"
echo

# ── Run each scenario ────────────────────────────────────────────
for toml in "${TOML_FILES[@]}"; do
  name=$(basename "$toml" .toml)

  # Skip scenarios that require external driving (see SKIP_SCENARIOS above).
  # Exact match per element: a "${SKIP_SCENARIOS[@]}" here-string joins the
  # whole array onto ONE line, which grep -x never matches, so nothing was
  # skipped; a loop also avoids piping a producer into an early-exit grep.
  skip_this=0
  for skip_name in "${SKIP_SCENARIOS[@]}"; do
    if [ "$skip_name" = "$name" ]; then skip_this=1; break; fi
  done
  if [ "$skip_this" -eq 1 ]; then
    blue "Scenario: $name"
    skip "$name" "requires external driver (test-local-mode.sh)"
    echo
    continue
  fi

  blue "Scenario: $name"

  # Run claudeless with a timeout. The default of 60s is too tight for the
  # trigger-source-* scenarios, which wait through ≥1 cron boundary
  # (cron='* * * * *' worst case ~75s before first fire) plus engine restart
  # and post-disable settle windows. 180s gives the slowest scenario
  # (trigger-source-disable-stop, ~150s) headroom.
  local_log="/tmp/claudeless-${name}.log"
  if timeout 180 claudeless --scenario "$toml" \
    -p "Run all test steps: check, decision, cooldown, yield, done" \
    > "$local_log" 2>&1; then
    pass "$name"
    dim "  Log: $local_log"
  else
    rc=$?
    if [ $rc -eq 124 ]; then
      fail "$name" "timed out after 180s"
    else
      fail "$name" "exit code $rc"
    fi
    # Show last 5 lines of output for debugging
    dim "  --- last 5 lines ---"
    tail -5 "$local_log" 2>/dev/null | while IFS= read -r line; do
      dim "  $line"
    done
  fi
  echo
done

# ── Summary ──────────────────────────────────────────────────────
echo
blue "═══════════════════════════════════════════════════"
blue "  Results: $PASS passed, $FAIL failed, $SKIP skipped"
blue "  Total:   ${#TOML_FILES[@]} scenarios"
blue "═══════════════════════════════════════════════════"

if [ $FAIL -gt 0 ]; then
  echo
  red "Failures:"
  for err in "${ERRORS[@]}"; do
    red "  - $err"
  done
  exit 1
fi

exit 0
