#!/usr/bin/env bash
# SC2317: the pass/fail/skip stubs are invoked INDIRECTLY (by outbox_rig_guard,
#         inside subshells captured with $(...)), which shellcheck can't trace —
#         the "unreachable" reports on their bodies are false positives.
# shellcheck disable=SC2317
# test-lib-outbox-rig.sh — cluster-free, credless unit test for lib-outbox-rig.sh.
#
# The HERMETIC, always-on proof that the REQUIRE_OUTBOX_RIG fail-loud hook
# (ka-0000007489, epic ka-0000002637) actually flips a MANDATORY-but-absent rig
# from today's skip-PASS to a hard NO-GO FAIL — and does NOTHING when unset. This
# is the no-silent-skip guarantee for the guard mechanism itself: it runs on every
# push (checks.yml script-selftests) with ZERO creds/network, so
# a future edit that guts the guard back into an unconditional skip REDDENS here
# instead of passing vacuously — the same rigor test-outbox-shape-selftest.sh's
# SHAPE_MIN_ASSERTIONS gives the shape self-test.
#
# Runs like test-lib-gvisor-node.sh: stub the caller-contract helpers (red/skip/
# fail), source the lib, drive outbox_rig_guard under each REQUIRE_OUTBOX_RIG
# value, and assert which of skip/fail fired.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ── Outer harness bookkeeping ─────────────────────────────────────────
T_PASS=0
T_FAIL=0
ERRORS=()
ok()  { T_PASS=$((T_PASS + 1)); echo "  PASS: $1"; }
bad() { T_FAIL=$((T_FAIL + 1)); ERRORS+=("$1"); echo "  FAIL: $1"; }

# ── Caller-contract stubs the guard uses (red/skip/fail) ──────────────
# We record which helper fired (and its message) into single-run globals so the
# test can assert the guard's decision. skip/fail mirror the real scripts: both
# emit a line; skip records a PASS, fail records a FAIL (the bug the gate closes).
LAST_SKIP=""
LAST_FAIL=""
red()  { :; }  # the NO-GO banner lines — silenced in the unit test
skip() { LAST_SKIP="$1"; }
fail() { LAST_FAIL="$1"; }

# shellcheck source=./lib-outbox-rig.sh
source "$SCRIPT_DIR/lib-outbox-rig.sh"

# reset_probe clears the per-run recorders before each guard invocation.
reset_probe() { LAST_SKIP=""; LAST_FAIL=""; }

# ── outbox_rig_required: truthy-token matrix (case-insensitive; 1 AND true) ──
# A CI boolean input yields the literal string "true"/"false", NOT "1"; the
# guard MUST arm on both, or wiring it through `env: REQUIRE_OUTBOX_RIG:
# ${{ inputs.require-outbox-rig }}` would silently never fire (the exact trap the
# sibling REQUIRE_SUBSTRATE `= "1"`-only test falls into).
# Includes whitespace-wrapped forms ("true ", " true") — workflow `${{ inputs.x }}` /
# GitLab schedule-variable interpolation can carry a stray space, and a gate that
# silently fails to ARM is the vacuous-green mode this guard exists to close.
for v in 1 true TRUE True yes YES on ON "true " " true" " 1 "; do
  if REQUIRE_OUTBOX_RIG="$v" outbox_rig_required; then
    ok "outbox_rig_required: '$v' is truthy (arms the gate)"
  else
    bad "outbox_rig_required: '$v' should be truthy"
  fi
done
for v in "" 0 false FALSE no off garbage 2; do
  if REQUIRE_OUTBOX_RIG="$v" outbox_rig_required; then
    bad "outbox_rig_required: '$v' should be falsy (gate stays opt-in)"
  else
    ok "outbox_rig_required: '$v' is falsy (opt-in skip-clean preserved)"
  fi
done
# Unset (not just empty) must also be falsy — the default in every pipeline today.
if (unset REQUIRE_OUTBOX_RIG; outbox_rig_required); then
  bad "outbox_rig_required: UNSET should be falsy (today's default)"
else
  ok "outbox_rig_required: UNSET is falsy (today's default — behaviour unchanged)"
fi

# ── outbox_rig_guard: DEFAULT (unset) → clean SKIP, never FAIL ────────
# Run in THIS shell (no subshell) so the skip/fail recorders the guard sets stick.
reset_probe
unset REQUIRE_OUTBOX_RIG
outbox_rig_guard "live outbox negative gates" "sandbox rig absent"
if [ -n "$LAST_SKIP" ] && [ -z "$LAST_FAIL" ]; then
  ok "guard: UNSET → clean SKIP (skip='$LAST_SKIP', no FAIL) — today's skip-PASS contract"
else
  bad "guard: UNSET must SKIP not FAIL (skip='$LAST_SKIP' fail='$LAST_FAIL')"
fi

# ── outbox_rig_guard: explicit 0 / false → clean SKIP ─────────────────
for off in 0 false; do
  reset_probe
  REQUIRE_OUTBOX_RIG="$off" outbox_rig_guard "live outbox positive apply gates" "browse seat absent"
  # NB: the env-prefixed call runs in the SAME shell (no subshell), so recorders stick.
  if [ -n "$LAST_SKIP" ] && [ -z "$LAST_FAIL" ]; then
    ok "guard: REQUIRE_OUTBOX_RIG=$off → clean SKIP"
  else
    bad "guard: REQUIRE_OUTBOX_RIG=$off must SKIP (skip='$LAST_SKIP' fail='$LAST_FAIL')"
  fi
done

# ── outbox_rig_guard: on (1 / true) → hard FAIL, never a silent SKIP ──
for on in 1 true; do
  reset_probe
  REQUIRE_OUTBOX_RIG="$on" outbox_rig_guard "live outbox negative gates" "sandbox rig absent"
  if [ -n "$LAST_FAIL" ] && [ -z "$LAST_SKIP" ]; then
    case "$LAST_FAIL" in
      *"live outbox negative gates"*REQUIRE_OUTBOX_RIG*)
        ok "guard: REQUIRE_OUTBOX_RIG=$on → hard FAIL (fail names the gate + knob: '$LAST_FAIL')" ;;
      *)
        bad "guard: REQUIRE_OUTBOX_RIG=$on failed but the message is unclear: '$LAST_FAIL'" ;;
    esac
  else
    bad "guard: REQUIRE_OUTBOX_RIG=$on must FAIL not SKIP (skip='$LAST_SKIP' fail='$LAST_FAIL')"
  fi
done

# ── Summary (no-silent-skip: the test itself must have run its checks) ──
echo "════════════════════════════════════════════════════"
TOTAL=$((T_PASS + T_FAIL))
if [ "$T_FAIL" -ne 0 ]; then
  echo "✗ $T_FAIL/$TOTAL lib-outbox-rig checks failed"
  for e in "${ERRORS[@]}"; do echo "   - $e"; done
  echo "════════════════════════════════════════════════════"
  exit 1
fi
# A gutted test that asserted nothing must not pass green. 25 checks today
# (11 truthy incl. whitespace forms + 8 falsy + 1 unset for outbox_rig_required;
# 1 unset + 2 off + 2 on for the guard); a floor a hair below that catches an
# accidental gutting without tripping on every added assertion.
MIN_CHECKS=20
if [ "$T_PASS" -lt "$MIN_CHECKS" ]; then
  echo "✗ only $T_PASS checks ran (expected >= $MIN_CHECKS) — refusing a vacuous green"
  echo "════════════════════════════════════════════════════"
  exit 1
fi
echo "✓ All $TOTAL lib-outbox-rig checks passed (>= $MIN_CHECKS required)"
echo "════════════════════════════════════════════════════"
