#!/usr/bin/env bash
# test-outbox-shape-selftest.sh — HERMETIC main-push gate for the 1:1 GBFSM
# Analysis CREATE-shape self-test (Phase 5c, ka-0000003147; epic ka-0000002637).
#
# ── What this is ──────────────────────────────────────────────────────────────
#
# The bot-ownable half of the 1:1 outbox reshape is the CREATE payload the outbox
# fabricates — fixed by MERGED code (jira_outbox.go) + the FROZEN schema, NOT by the
# operator's Automation rules. lib-outbox-shape.sh::shape_selftest asserts that
# payload faithfully with ZERO credentials and ZERO network. This runner is the
# always-on, hermetic wrapper that lets checks.yml script-selftests run
# those assertions on every push — the coverage that was previously missing because
# the E2E outbox scripts run only in the full/scheduled suite (run-shard.sh), and
# the self-test was trapped inside each script's live-cluster main().
#
# ── gvisor-style gate: NO SILENT SKIP ─────────────────────────────────────────
#
# Modeled on the old gvisor-readiness gate (REQUIRE_SUBSTRATE=1):
# a gate that can silently skip proves nothing. This runner therefore:
#   - runs with ZERO creds/network (it is pure by construction — no rig to be
#     absent, so there is nothing to skip);
#   - asserts a MINIMUM number of assertions actually executed (SHAPE_MIN_ASSERTIONS)
#     — so a future refactor that accidentally guts shape_selftest into a no-op
#     REDDENS here instead of passing vacuously;
#   - exits NON-ZERO on ANY failed assertion.
# There is no rig, no cred, and no environment in which this gate legitimately
# skips: green means the 6 shape assertions ran and passed; anything else is red.
#
# ── Relationship to the E2E scripts ───────────────────────────────────────────
#
# This runner and test-jira-outbox-{positive,negative}.sh source the SAME
# lib-outbox-shape.sh, so the shape the main-push gate proves is byte-for-byte the
# shape the live E2E suite fabricates. No drift is possible: there is one
# definition of gfo_analysis_payload / shape_assert / shape_selftest.
#
# Usage:  bash gardenio/tests/e2e/scripts/test-outbox-shape-selftest.sh
# Exit:   0 = all shape assertions ran and passed; 1 = a failure or a vacuous run.

set -euo pipefail

# ── Reporting helpers (the caller contract lib-outbox-shape.sh depends on) ──────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

PASS=0
FAIL=0
ERRORS=()
pass() { green "  ✓ $1"; PASS=$((PASS + 1)); }
fail() {
  red "  ✗ $1"
  [ -n "${2:-}" ] && dim "    $2"
  FAIL=$((FAIL + 1))
  ERRORS+=("$1: ${2:-}")
}

# The shape self-test records exactly SIX always-on assertions (T-1to1, T-link,
# no-customer-surface, T7 repoint-defense, T-upsert identity, discriminator). We
# require at least this many so a gutted shape_selftest cannot pass vacuously.
SHAPE_MIN_ASSERTIONS="${SHAPE_MIN_ASSERTIONS:-6}"

# shellcheck source=./lib-outbox-shape.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib-outbox-shape.sh"

main() {
  blue "═══════════════════════════════════════════════════"
  blue " Hermetic 1:1 GBFSM Analysis CREATE-shape self-test"
  blue " ka-0000003147 — always-on main-push gate (no creds)"
  blue "═══════════════════════════════════════════════════"
  echo

  # Prove python3 is present — the shape builder/inspector depends on it, and a
  # missing interpreter would make gfo_analysis_payload echo '' and the whole
  # gate degrade to a vacuous skip. Fail LOUDLY instead.
  if ! command -v python3 >/dev/null 2>&1; then
    red "  ✗ python3 is required for the shape self-test and is not on PATH"
    exit 1
  fi

  shape_selftest
  echo

  blue "═══════════════════════════════════════════════════"
  local total=$((PASS + FAIL))
  if [ "$FAIL" -ne 0 ]; then
    red " ✗ $FAIL/$total shape assertions failed"
    echo
    for err in "${ERRORS[@]}"; do red "   - $err"; done
    blue "═══════════════════════════════════════════════════"
    exit 1
  fi
  # NO-SILENT-SKIP gate: the run must have actually exercised the self-test.
  if [ "$PASS" -lt "$SHAPE_MIN_ASSERTIONS" ]; then
    red " ✗ only $PASS shape assertions ran (expected >= $SHAPE_MIN_ASSERTIONS) — the"
    red "   self-test was gutted into a near-no-op; refusing a vacuous green."
    blue "═══════════════════════════════════════════════════"
    exit 1
  fi
  green " ✓ All $total shape assertions passed (>= $SHAPE_MIN_ASSERTIONS required)"
  blue "═══════════════════════════════════════════════════"
}

main "$@"
