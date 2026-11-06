#!/usr/bin/env bash
# test-shard-budget-guard.sh — hermetic unit test for the budget-aware in-shard
# retry guard in run-shard.sh (ka-0000003834).
#
# BACKGROUND: run_module()'s connectivity-flap retry re-runs the WHOLE module
# (after a ~180s connectivity-recovery poll) with no awareness of how much
# wall-clock the shard has left. On run 024eb6ea (tag 2026.190.4, shard 1)
# litellm-cost-tracking (~455s) failed late after a mid-module flap; the harness
# started a full re-run and the 20m shard timeout fired MID-RETRY (exit 137),
# killing the shard before its remaining 6 modules ran — those not-run modules
# counted as FAILED and reded the release gate (which is now 0-tolerance,
# ka-0000000440) with a whole shard's worth of mis-attributed blame.
#
# The fix adds shard_seconds_remaining() + retry_fits_budget(), which DECLINE an
# in-shard retry that cannot finish before the shard timeout. This test lifts both
# pure functions out of run-shard.sh (same technique as test-shard-isolation.sh)
# and drives them across the budget boundary — no kubectl, no beads daemon, no
# network. It never runs the body of run-shard.sh (that would shell out to
# kubectl); it only sources the two extracted functions.
#
# Invariants asserted:
#   1. shard_seconds_remaining() decreases as wall-clock elapses.
#   2. retry_fits_budget() FITS a fast module early in a fresh shard.
#   3. retry_fits_budget() DECLINES a slow module late in the shard (the
#      exact 024eb6ea shape: ~455s module, little budget left).
#   4. The poll budget + margin are respected: a retry is declined even for a
#      zero-duration module when the remaining budget is below poll+margin.
#   5. A wider SHARD_BUDGET_SECONDS (the ka-0000008024 20m->25m bump) flips a
#      borderline case from DECLINE back to FIT — proving the knob composes.
#   6. shard_budget_warn() (ka-0000008024) stays SILENT below the warn
#      threshold, FIRES once at/above it, and never fires twice (one-shot).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RUN_SHARD="${SCRIPT_DIR}/run-shard.sh"

if [ ! -f "$RUN_SHARD" ]; then
  echo "FAIL: run-shard.sh not found at $RUN_SHARD" >&2
  exit 1
fi

PASS=0
FAIL=0
ERRORS=()
pass() { PASS=$((PASS + 1)); echo "  PASS: $1"; }
fail() { FAIL=$((FAIL + 1)); ERRORS+=("$1"); echo "  FAIL: $1"; }

# ── Lift the two pure functions from run-shard.sh ───────────────────
# awk-extract each `name() { ... }` block by brace-at-margin, exactly like
# test-shard-isolation.sh lifts shard_for(). Neither function touches kubectl,
# beads, or the network — they only read SHARD_* variables and call `date`.
extract_func() {
  local fn="$1"
  awk -v fn="$fn" '
    $0 ~ "^" fn "\\(\\)[[:space:]]*\\{" { capture = 1 }
    capture                            { print }
    capture && /^\}/                   { exit }
  ' "$RUN_SHARD"
}

SHARD_REMAINING_FUNC=$(extract_func "shard_seconds_remaining")
RETRY_FITS_FUNC=$(extract_func "retry_fits_budget")
BUDGET_WARN_FUNC=$(extract_func "shard_budget_warn")
if [ -z "$SHARD_REMAINING_FUNC" ] || [ -z "$RETRY_FITS_FUNC" ] || [ -z "$BUDGET_WARN_FUNC" ]; then
  echo "FAIL: could not extract shard_seconds_remaining/retry_fits_budget/shard_budget_warn from $RUN_SHARD" >&2
  echo "      (did the function names or brace style change?)" >&2
  exit 1
fi
# shellcheck disable=SC2294
eval "$SHARD_REMAINING_FUNC"
# shellcheck disable=SC2294
eval "$RETRY_FITS_FUNC"
# shellcheck disable=SC2294
eval "$BUDGET_WARN_FUNC"

# ── Fixtures: the retry guard reads these globals ───────────────────
# Match run-shard.sh's defaults so the test exercises the real thresholds.
# These four are consumed ONLY inside the eval'd shard_seconds_remaining /
# retry_fits_budget bodies, which shellcheck cannot see — hence the SC2034
# disables (they are genuinely used at eval time, not dead).
SHARD_BUDGET_SECONDS=1200            # 20m test baseline (invariants 1-4 pin
                                     # their numbers to it; the live shard
                                     # timeout is now 25m/1500 — ka-0000008024 —
                                     # and invariant 5 exercises that value too)
# shellcheck disable=SC2034  # read inside eval'd retry_fits_budget
SHARD_BUDGET_MARGIN_SECONDS=90       # post-module JUnit/log/teardown reserve
# shellcheck disable=SC2034  # read inside eval'd retry_fits_budget
SHARD_RETRY_POLL_BUDGET_SECONDS=180  # worst-case connectivity-recovery poll
# Consumed only inside the eval'd shard_budget_warn body (invariant 6).
# shellcheck disable=SC2034  # read inside eval'd shard_budget_warn
SHARD_BUDGET_WARN_PCT=80             # warn once ≥80% of the shard budget is used
# shellcheck disable=SC2034  # read inside eval'd shard_budget_warn (echo label)
E2E_SHARD=0                          # shard id for the warning line

# Helper: pin the shard "start" so shard_seconds_remaining() reports a chosen
# remaining budget. remaining = BUDGET - (now - START) => START = now - elapsed,
# where elapsed = BUDGET - remaining.
set_remaining() {
  local want_remaining="$1"
  local now elapsed
  now=$(date +%s)
  elapsed=$(( SHARD_BUDGET_SECONDS - want_remaining ))
  # shellcheck disable=SC2034  # read inside eval'd shard_seconds_remaining
  SHARD_START_TS=$(( now - elapsed ))
}

# ── Invariant 1: remaining decreases over elapsed wall-clock ────────
set_remaining 600
r1=$(shard_seconds_remaining)
# 600 requested; allow ±2s for the date calls straddling a second boundary.
if [ "$r1" -ge 598 ] && [ "$r1" -le 601 ]; then
  pass "shard_seconds_remaining reflects elapsed time (~600s left, got ${r1}s)"
else
  fail "shard_seconds_remaining: expected ~600s, got ${r1}s"
fi

# ── Invariant 2: fast module early in a fresh shard FITS ────────────
# 19m45s left, a 30s module: 180+30+90 = 300s need << 1185s remaining.
set_remaining 1185
if retry_fits_budget 30; then
  pass "retry_fits_budget: 30s module with 1185s left FITS (retry allowed)"
else
  fail "retry_fits_budget: 30s module with 1185s left should FIT"
fi

# ── Invariant 3: the 024eb6ea shape — slow module late DECLINES ─────
# litellm ~455s failing with ~300s of the 20m budget left: need =
# 180+455+90 = 725s >> 300s remaining => must DECLINE.
set_remaining 300
if retry_fits_budget 455; then
  fail "retry_fits_budget: 455s module with 300s left should DECLINE (024eb6ea cascade)"
else
  pass "retry_fits_budget: 455s module with 300s left DECLINES (prevents 20m timeout cascade)"
fi

# ── Invariant 4: poll+margin floor respected even for a 0s module ───
# need for a 0s module = 180+0+90 = 270s. With 269s left => DECLINE;
# with 271s left => FIT. Proves the floor is exactly poll+margin.
set_remaining 269
if retry_fits_budget 0; then
  fail "retry_fits_budget: 0s module with 269s left should DECLINE (below poll+margin floor of 270s)"
else
  pass "retry_fits_budget: 0s module with 269s left DECLINES (poll+margin floor respected)"
fi
set_remaining 271
if retry_fits_budget 0; then
  pass "retry_fits_budget: 0s module with 271s left FITS (just above poll+margin floor)"
else
  fail "retry_fits_budget: 0s module with 271s left should FIT"
fi

# ── Invariant 5: the 20m->25m timeout bump (ka-0000008024) composes ──
# A 455s module needs 180+455+90 = 725s to retry. Take a failure instant 700s
# into the shard: under the 20m budget only 500s remain (DECLINE), but the 25m
# budget leaves 800s (FIT). Same module, same failure instant, opposite verdict
# — proving SHARD_BUDGET_SECONDS is the single knob the timeout bump turns.
SHARD_BUDGET_SECONDS=1200  # 20m
set_remaining 500          # 700s elapsed, 500s of the 20m budget left
if retry_fits_budget 455; then
  fail "retry_fits_budget: 455s module with 500s left under a 20m budget should DECLINE"
else
  pass "retry_fits_budget: 455s module, 20m budget, 500s left DECLINES (baseline before bump)"
fi
SHARD_BUDGET_SECONDS=1500  # 25m (ka-0000008024)
set_remaining 800          # same 700s elapsed, 800s of the 25m budget left
if retry_fits_budget 455; then
  pass "retry_fits_budget: 455s module, 25m budget, 800s left FITS (timeout-bump knob composes)"
else
  fail "retry_fits_budget: 455s module with 800s left under a 25m budget should FIT"
fi
SHARD_BUDGET_SECONDS=1200  # restore

# ── Invariant 6: shard_budget_warn one-shot at the 80% threshold ─────
# 20m budget, warn at 80% => threshold 960s elapsed (240s remaining).
# Below threshold: silent. At/above: fires exactly once, never twice.
# NB: capture with `> file` (a plain redirection runs in THIS shell), NOT
# `$(...)` — command substitution subshells and would hide the one-shot
# SHARD_BUDGET_WARNED mutation the function relies on in the real caller.
SHARD_BUDGET_SECONDS=1200
SHARD_BUDGET_WARNED=0
WARN_OUT_FILE=$(mktemp)
trap 'rm -f "$WARN_OUT_FILE"' EXIT

set_remaining 300          # 900s elapsed < 960s threshold => silent
shard_budget_warn > "$WARN_OUT_FILE"
if [ ! -s "$WARN_OUT_FILE" ] && [ "$SHARD_BUDGET_WARNED" = 0 ]; then
  pass "shard_budget_warn: silent at 75% used (below the 80% threshold)"
else
  fail "shard_budget_warn: should be SILENT at 75% used (got '$(cat "$WARN_OUT_FILE")', warned=${SHARD_BUDGET_WARNED})"
fi

set_remaining 200          # 1000s elapsed >= 960s threshold => fires once
shard_budget_warn > "$WARN_OUT_FILE"
if [ -s "$WARN_OUT_FILE" ] && [ "$SHARD_BUDGET_WARNED" = 1 ]; then
  pass "shard_budget_warn: fires once at ≥80% used"
else
  fail "shard_budget_warn: should FIRE at ≥80% used (got '$(cat "$WARN_OUT_FILE")', warned=${SHARD_BUDGET_WARNED})"
fi

set_remaining 100          # even deeper past threshold — must not fire again
shard_budget_warn > "$WARN_OUT_FILE"
if [ ! -s "$WARN_OUT_FILE" ]; then
  pass "shard_budget_warn: one-shot — does not re-fire once already warned"
else
  fail "shard_budget_warn: should NOT re-fire after warning once (got '$(cat "$WARN_OUT_FILE")')"
fi
SHARD_BUDGET_SECONDS=1200  # restore

# ── Summary ─────────────────────────────────────────────────────────
echo
echo "Results: $PASS passed, $FAIL failed"
if [ "$FAIL" -gt 0 ]; then
  for err in "${ERRORS[@]}"; do
    echo "  - $err"
  done
  exit 1
fi
echo "OK: shard budget-aware retry guard invariants hold"
