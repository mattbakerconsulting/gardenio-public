#!/usr/bin/env bash
# lib-outbox-rig.sh — the fail-loud "is the sandbox rig REQUIRED here?" decision
# for the LIVE OUTBOX security matrix, factored into ONE sourced library
# (ka-0000007489; epic ka-0000002637).
#
# ── Why this library exists ───────────────────────────────────────────────────
#
# The live outbox security matrix (test-jira-outbox-{positive,negative}.sh) only
# ever skip-PASSES in automated pipelines: skip() increments PASS, and when the
# sandbox rig is absent the ENTIRE live matrix collapses to a single skip. Since
# the sandbox creds (JIRA_SANDBOX_* + the gardenio2 / browse seats) are provisioned
# in ZERO CI files today, every automated run (main-push, scheduled, release)
# skip-PASSES the whole matrix — "E2E suite green" is VACUOUS-green for the live
# legs (parent bead ka-0000007489; Phase 5 close ka-0000006784).
#
# This guard is the durable fail-loud hook. It is modeled EXACTLY on
# lib-gvisor-node.sh::gvisor_require_substrate_guard (the gVisor production-
# readiness go/no-go, task ka-0000002867) and the test-outbox-shape-selftest.sh
# no-silent-skip pattern already in this repo: a gate that can silently skip
# proves nothing, so once the rig is EXPECTED (a scheduled readiness run), an
# absent rig is a HARD FAIL, not a skip-PASS.
#
# ── The knob: REQUIRE_OUTBOX_RIG (default OFF — backward compatible) ───────────
#
#   unset / 0 / false  (DEFAULT):
#       the module keeps today's SKIP-CLEAN contract — an absent sandbox rig is a
#       loud, clearly-labelled skip recorded as a PASS (rigging absent != failure),
#       so main-push / scheduled / release stay green while the creds are still
#       operator-side. ZERO behaviour change from before this bead.
#
#   1 / true / yes / on  (the STANDING REGRESSION GATE):
#       an absent rig/seat that is SUPPOSED to be present becomes a loud red NO-GO
#       recorded as a FAIL, so the module reddens instead of passing vacuously.
#       The operator flips this on (alongside provisioning the sandbox creds into
#       the E2E namespace — step 1 of ka-0000007489) to turn the matrix into a
#       real standing regression guard for T-1to1/T-upsert/T-link + T3/T7/T10/
#       T11/T12.
#
# The token is matched case-insensitively and accepts BOTH the shell `=1` idiom
# and the CI-input `"true"` string convention, so wiring the knob through a
# workflow input (which yields the literal "true"/"false") arms it correctly — a deliberate hardening over the
# sibling REQUIRE_SUBSTRATE guard, which only tests `= "1"`.
#
# ── Caller contract ───────────────────────────────────────────────────────────
#
# Source AFTER the caller has defined the color/reporting helpers this file uses:
#   red   "<msg>"          — print a red line (the NO-GO banner)
#   skip  "<msg>" "<why>"  — record a cleanly-skipped gate as a PASS
#   fail  "<msg>" "<why>"  — record a failing assertion
# (test-jira-outbox-{positive,negative}.sh define these before sourcing; the
# hermetic unit test test-lib-outbox-rig.sh stubs them.) The guard NEVER exits the
# script — it records via skip/fail and lets the caller's existing summary/exit
# contract (main returns `[ "$FAIL" -eq 0 ]`) decide the outcome, so the always-on
# hermetic scenarios (0a–0e) that ran BEFORE the live matrix are still reported.
#
# Usage:
#   # shellcheck source=./lib-outbox-rig.sh
#   source "$(dirname "${BASH_SOURCE[0]}")/lib-outbox-rig.sh"
#   ...
#   if ! sandbox_rig_ready; then
#     outbox_rig_guard "live outbox negative gates" "sandbox rig absent (...)"
#   else
#     ... run the live matrix ...
#   fi

# Guard against multiple sourcing within the same shell (mirrors lib-outbox-shape.sh).
# shellcheck disable=SC2317
if [ -n "${__GARDENIO_LIB_OUTBOX_RIG_LOADED:-}" ]; then
  return 0 2>/dev/null || true
fi
__GARDENIO_LIB_OUTBOX_RIG_LOADED=1

# outbox_rig_required — returns 0 (true) iff REQUIRE_OUTBOX_RIG is a truthy token
# (1/true/yes/on, case-insensitive), else 1. This is the single definition of
# "is the rig mandatory on this run?" — used by the guard and exposed so callers
# can log the mode in their summary.
outbox_rig_required() {
  # Lowercase AND strip surrounding whitespace: workflow `${{ inputs.x }}` interpolation
  # and GitLab CI schedule variables can carry a stray leading/trailing space, and
  # a gate that silently FAILS TO ARM (skip-clean) is the exact vacuous-green mode
  # this guard closes — so "true " must still arm, not fall through to opt-in.
  local v
  v="$(printf '%s' "${REQUIRE_OUTBOX_RIG:-0}" | tr '[:upper:]' '[:lower:]' | tr -d '[:space:]')"
  case "$v" in
    1|true|yes|on) return 0 ;;
    *)             return 1 ;;
  esac
}

# outbox_rig_guard <label> <why> — the decision for a MANDATORY live rig/seat that
# is ABSENT. Callers invoke it in place of a bare skip at the points where the
# live matrix collapses because a rig/seat it REQUIRES is missing.
#   REQUIRE_OUTBOX_RIG off  -> skip "<label>" "<why>"  (today's clean skip-PASS)
#   REQUIRE_OUTBOX_RIG on   -> loud red NO-GO banner + fail "<label> ..." "<why>"
# NB: only call this for rig/seat conditions that are TRULY mandatory when the rig
# is expected. Optional enhancers (e.g. the negative suite's FSM-side browse
# corroboration) must stay a plain skip — their absence is never a NO-GO.
outbox_rig_guard() {
  local label="$1" why="${2:-}"
  if outbox_rig_required; then
    red "════════════════════════════════════════════════════"
    red " NO-GO ${label} (REQUIRE_OUTBOX_RIG=on): ${why}"
    red " The live outbox security matrix is the STANDING REGRESSION GATE"
    red " (ka-0000007489, epic ka-0000002637). It must PROVE the live apply /"
    red " allowlist / fail-closed / write-isolation paths actually run — a silent"
    red " SKIP is NOT a pass here. An absent sandbox rig/seat that is SUPPOSED to"
    red " be present is a NO-GO: provision the sandbox + browse creds into the E2E"
    red " namespace (from SM gardenio/jira-sandbox-api-token — step 1 of"
    red " ka-0000007489) and re-run, or unset REQUIRE_OUTBOX_RIG to restore the"
    red " opt-in skip-clean contract."
    red "════════════════════════════════════════════════════"
    fail "${label} (REQUIRE_OUTBOX_RIG=on)" "$why"
    return 0
  fi
  skip "$label" "$why"
}
