#!/usr/bin/env bash
# test-gardener-start-argv-contract.sh — the `gb gardener start` bridge-argv
# contract e2e (bead ka-0000001242, parent ka-0000000958; related ka-0000006141).
#
# ── The gap this closes ───────────────────────────────────────────────────────
#
# The documented `@gardenio <epic-id>` Slack entrypoint shells out to
# `gb gardener start <epic>` (bridge/bot_mentions.go tryHandleEpicGardenerStart →
# gardenerStartRunner → exec `gb`). The argv it hands to exec is built by the pure
# function gardenerStartArgs, and it MUST carry --i-read-the-gardener-instructions
# or the read-the-guide gate (cmd/gb/gardener.go, ka-0000007415) refuses to create
# anything and the entrypoint fails for EVERY user. That is exactly the bug that
# shipped as ka-0000006141 and was fixed in 7c6aba74a (the flag was re-added to
# gardenerStartArgs).
#
# Coverage before this was UNIT-ONLY (bot_mentions_test.go TestGardenerStartArgs,
# a reflect.DeepEqual on the argv slice). The one e2e that LOOKS like it covers
# the entrypoint — claudeless/gardener-mention-entrypoint.toml — HARDCODES the
# flag in its shell script:
#     gb gardener start "$EPIC_ID" --i-read-the-gardener-instructions
# That is a hand-copied REPLICA of the argv; it never calls gardenerStartArgs, so
# reverting the flag in gardenerStartArgs leaves it GREEN. Its own header concedes
# it "exercises the same code path the bridge would take" — a replica that can
# silently drift from the real argv, which is precisely how ka-0000006141 shipped.
#
# ── Why an EMITTER, not a hardcoded flag list (the three vacuity traps) ───────
#
# The whole value of this e2e is that the argv is EMITTED BY the production
# gardenerStartArgs — the SAME symbol the bridge's exec path calls — not re-typed
# by the shell. So an //go:build e2e emitter (internal/bridge/
# gardener_start_argv_emit_e2e_test.go, TestEmitGardenerStartArgv) calls
# gardenerStartArgs and prints its output token-by-token; this module runs
# `gb <that emitted argv>` against the LIVE daemon and asserts it survives the
# real CLI flag parser + gate and reaches a create. gardenerStartArgs is
# UNEXPORTED, so only an in-package (e2e-tagged) file can reach it without a
# production-visible export — and the e2e tag keeps it off the default
# build/lint/test surface while riding the SAME warm e2e-tagged ./internal/bridge/
# build the subagent-stop module already compiles on shard 0.
#
# The bead names three traps; this module is built to avoid all three:
#   (a) the flag is NOT hardcoded in this shell — every positive-leg token comes
#       from TestEmitGardenerStartArgv, i.e. from gardenerStartArgs.
#   (b) a NEGATIVE control: the SAME emitted argv with the ack flag STRIPPED,
#       against a SECOND fresh epic, MUST be refused with no schedule written —
#       so a globally-removed gate (which would make the positive leg pass while
#       proving nothing) reds this leg.
#   (c) a FRESH epic PER leg: `gb gardener start` is idempotent and exits
#       non-zero "already" on a second start of the same epic, so a reused epic
#       would make the exit-code assertion measure idempotency, not the gate.
#
# ── The four reverts this catches (2x2: {gardenerStartArgs flag} x {gate}) ─────
#   1. gardenerStartArgs drops the ack flag  → emitted argv lacks it → positive
#      leg's `gb gardener start` is REFUSED → positive leg reds. (The headline
#      ka-0000006141-class drift; the exact revert the unit test guards, now
#      guarded live end-to-end.)
#   2. gardenerStartArgs renames/reorders a token so the real parser rejects it
#      → positive leg reds (the emitted argv no longer parses).
#   3. the gate degrades to a warning (still creates) → negative leg's stripped
#      argv would CREATE a schedule → negative leg reds.
#   4. both revert together → the negative leg still reds on the created
#      schedule, so the pair can never be simultaneously green-vacuous.
#
# ── Scope note (from the bead) ────────────────────────────────────────────────
# The Slack ingress seam (socket-mode app_mention) is NOT e2e-testable — the
# bridge has no events HTTP route and lib-slackmock is outbound-only. This module
# does NOT attempt to inject a Slack event; it pins the bridge-side ARGV CONTRACT
# (gardenerStartArgs → real `gb` parser + gate), which is the drift-prone seam.
#
# ── Skip-clean contract ───────────────────────────────────────────────────────
# Requires a Go toolchain (to run the emitter) and a live beads daemon reachable
# by `gb`/`kd`. If the toolchain / controller source is absent, or `gb gardener`
# is a pre-T6 binary, the module SKIPS CLEANLY (exit 0) with a LOUD banner — an
# absent toolchain/daemon must never red the suite (mirrors subagent-stop /
# session-spend-attribution). When present, every leg is a HARD assertion.
#
# Standalone runnable:
#   ./gardenio/tests/e2e/scripts/test-gardener-start-argv-contract.sh

set -uo pipefail

# ── Config ────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# scripts/ -> e2e/ -> tests/ -> gardenio/ -> controller
CONTROLLER_DIR="$(cd "$SCRIPT_DIR/../../../controller" 2>/dev/null && pwd)" || CONTROLLER_DIR=""
GO_BIN="${GO_BIN:-go}"
RUN_ID="${RUN_ID:-$$}"
# A slack-shaped operator so the emitted argv is the FULL production shape
# (gardener start <epic> --i-read-the-gardener-instructions --operated-by <op>).
# Owner resolution is best-effort (a lookup miss is ownerless, never a failure —
# plangardener.ResolveGardenerOwner), so an unknown id is safe here.
OPERATED_BY="${GARDENER_ARGV_OPERATED_BY:-slack:U0OPERATOR1}"

PASS=0
FAIL=0
ERRORS=()
CREATED_EPICS=()

# ── Helpers ───────────────────────────────────────────────────────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

pass() { green "  ✓ $1"; PASS=$((PASS + 1)); }
fail() {
  local name="$1" reason="${2:-}"
  red "  ✗ $name"
  [ -n "$reason" ] && dim "    $reason"
  FAIL=$((FAIL + 1))
  ERRORS+=("$name: $reason")
}
have() { command -v "$1" >/dev/null 2>&1; }

# skip_all exits 0 (a missing toolchain/daemon must NOT red the suite) with a
# LOUD banner so a skip is never silently read as a pass.
skip_all() {
  local why="$1"
  red "════════════════════════════════════════════════════"
  red " SKIP gardener-start-argv-contract (LOUD): $why"
  red " This run did NOT assert the bridge gardener-start ARGV contract"
  red " (gardenerStartArgs → real gb parser + read-the-guide gate). A green suite"
  red " here proves NOTHING about ka-0000001242 / the ka-0000006141-class drift."
  red " The e2e shard is built with a Go toolchain + the controller source — if"
  red " this fires in CI, that wiring regressed."
  red "════════════════════════════════════════════════════"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

cleanup() {
  local e
  for e in "${CREATED_EPICS[@]:-}"; do
    [ -z "$e" ] && continue
    gb gardener stop "$e" --reason "e2e cleanup" >/dev/null 2>&1 || true
    kd close "$e" >/dev/null 2>&1 || true
  done
}
trap cleanup EXIT

echo "════════════════════════════════════════════════════"
echo " gb gardener start bridge-argv contract E2E (ka-0000001242)"
echo " emitted-by-gardenerStartArgs argv survives the real gb parser + gate"
echo "════════════════════════════════════════════════════"

# ── Preflight ───────────────────────────────────────────────────────
have gb || skip_all "gb not on PATH"
have kd || skip_all "kd not on PATH"
have jq || skip_all "jq not on PATH"
have "$GO_BIN" || skip_all "'$GO_BIN' toolchain not on PATH (cannot run the argv emitter)"
[ -n "$CONTROLLER_DIR" ] && [ -f "$CONTROLLER_DIR/go.mod" ] \
  || skip_all "controller module not found (looked at '${CONTROLLER_DIR:-<unresolved>}')"
# Pre-T6 gb binaries have no `gardener` subcommand — nothing to assert.
gb gardener --help >/dev/null 2>&1 || skip_all "gb gardener not present in this binary (pre-T6)"

dim "  go:         $(command -v "$GO_BIN")  ($("$GO_BIN" version 2>/dev/null))"
dim "  controller: $CONTROLLER_DIR"
dim "  operator:   $OPERATED_BY"
echo

# ── emit_argv <fresh-epic-id> ───────────────────────────────────────
# Run the //go:build e2e emitter and echo the argv tokens (one per line) it
# prints for the given epic. Sets EMIT_HAS_ACK=yes|no. Returns non-zero on a
# build/run failure or a self-skip (PROBE_SKIP), leaving EMIT_ARGV empty.
EMIT_ARGV=()
EMIT_HAS_ACK=""
emit_argv() {
  local epic="$1"
  local out="/tmp/gardener-argv-emit-${RUN_ID}.$$.out"
  EMIT_ARGV=()
  EMIT_HAS_ACK=""
  if ! ( cd "$CONTROLLER_DIR" \
           && GARDENER_EMIT_BEAD_ID="$epic" GARDENER_EMIT_OPERATED_BY="$OPERATED_BY" \
              "$GO_BIN" test -tags e2e -v ./internal/bridge/ \
                -run '^TestEmitGardenerStartArgv$' -count=1 -timeout 120s ) \
         >"$out" 2>&1; then
    # A dep-fetch failure on a locked-down shard is an environment gap, not a
    # regression — distinguish it from a real compile/run failure.
    if grep -qiE "cannot find module|dial tcp|no such host|timeout|connection refused|proxy" "$out"; then
      dim "    emitter build could not fetch deps (offline shard): $(tail -2 "$out" | tr '\n' ' ')"
      return 2
    fi
    dim "    emitter output: $(tail -8 "$out" | tr '\n' ' ')"
    return 1
  fi
  if grep -q 'PROBE_SKIP:' "$out"; then
    dim "    emitter self-skipped: $(grep -o 'PROBE_SKIP:.*' "$out" | head -1)"
    return 1
  fi
  # `go test -v` prefixes emitted lines with leading whitespace; strip it.
  local line tok
  while IFS= read -r line; do
    case "$line" in
      *ARGV_TOKEN\ *) tok="${line#*ARGV_TOKEN }"; EMIT_ARGV+=("$tok") ;;
      *HAS_ACK\ *)    EMIT_HAS_ACK="${line#*HAS_ACK }" ;;
    esac
  done < "$out"
  rm -f "$out" 2>/dev/null || true
  [ "${#EMIT_ARGV[@]}" -ge 3 ]
}

# ── mk_epic <label> — create a FRESH, PROJECT-LABELLED epic, print its id ──────
# The epic MUST carry a project: label. `gb gardener start` hard-refuses a
# project-less gardener (cmd/gb/gardener.go, guard ka-0000007480: "project is
# empty; pass --project, set $KD_PROJECT or $BOAT_PROJECT, or add a project:
# label to the epic"), and the production entrypoint this module pins —
# @gardenio <epic-id> → gardenerStartArgs → exec gb — shells out WITHOUT
# --project (bot_mentions.go), while the bridge pod sets no $BOAT_PROJECT /
# $KD_PROJECT, so a mentioned epic resolves its project SOLELY from its own
# project: label. gardenerStartArgs is therefore correct to omit --project; the
# fixture epic must supply the label, exactly as a real @gardenio-mentioned epic
# does. The bare CI shard shell also has no project env, so without this label
# the positive leg's `gb gardener start` is refused and this module reds — the
# ka-0000005065 failure. Use $KD_PROJECT/$BOAT_PROJECT when present so a
# non-gardenio rig stays correct, else default to gardenio.
mk_epic() {
  local label="$1"
  local project="${KD_PROJECT:-${BOAT_PROJECT:-gardenio}}"
  kd create "e2e-gardener-argv-${label}-${RUN_ID}-$(date +%s)" --type=epic \
    -l "project:${project}" --json 2>/dev/null \
    | jq -r '.id // empty'
}

# ══════════════════════════════════════════════════════════════════
#  Positive leg — the EMITTED argv reaches a create on a FRESH epic
# ══════════════════════════════════════════════════════════════════
blue "[positive] emitted gardenerStartArgs argv → gb create → schedule exists"
POS_EPIC="$(mk_epic pos)"
[ -n "$POS_EPIC" ] || skip_all "could not create positive-leg epic (daemon unreachable?)"
CREATED_EPICS+=("$POS_EPIC")
dim "  epic: $POS_EPIC"

emit_rc=0
emit_argv "$POS_EPIC" || emit_rc=$?
if [ "$emit_rc" -eq 2 ]; then
  skip_all "argv emitter could not build offline — no argv to drive"
elif [ "$emit_rc" -ne 0 ] || [ "${#EMIT_ARGV[@]}" -lt 3 ]; then
  fail "emit argv for $POS_EPIC" "emitter produced no usable argv (rc=$emit_rc)"
else
  pass "gardenerStartArgs emitted argv: ${EMIT_ARGV[*]}"

  # (Independent of the assertions below) the emitted argv MUST carry the ack
  # flag — this is the ka-0000006141 invariant, checked on the emitter side.
  if [ "$EMIT_HAS_ACK" = "yes" ]; then
    pass "emitted argv carries --i-read-the-gardener-instructions"
  else
    fail "emitted argv carries --i-read-the-gardener-instructions" \
      "HAS_ACK=$EMIT_HAS_ACK — gardenerStartArgs dropped the ack flag (ka-0000006141 regression)"
  fi

  # Drive the REAL `gb` with the emitted argv — token-for-token, no reconstruction.
  START_OUT="$(gb "${EMIT_ARGV[@]}" 2>&1)"
  START_RC=$?
  if [ "$START_RC" -eq 0 ]; then
    pass "gb <emitted argv> exited 0 (parser + gate accepted it)"
  else
    fail "gb <emitted argv> exited 0" "rc=$START_RC out=$START_OUT"
  fi

  # The create must be observable: a schedule now resolves for the epic. status
  # exits non-zero when no schedule exists, so rc=0 here proves a create landed.
  STATUS_JSON="$(gb gardener status "$POS_EPIC" --json 2>&1)"
  STATUS_RC=$?
  if [ "$STATUS_RC" -eq 0 ]; then
    pass "gb gardener status exited 0 (schedule resolved)"
  else
    fail "gb gardener status exited 0" "rc=$STATUS_RC out=$STATUS_JSON"
  fi
  SCHED_ID="$(echo "$STATUS_JSON" | jq -r '.id // empty' 2>/dev/null || true)"
  if [ -n "$SCHED_ID" ]; then
    pass "schedule has an id ($SCHED_ID)"
  else
    fail "schedule has an id" "status json: $STATUS_JSON"
  fi
  SCHED_ENABLED="$(echo "$STATUS_JSON" | jq -r '.enabled // empty' 2>/dev/null || true)"
  if [ "$SCHED_ENABLED" = "true" ]; then
    pass "schedule is enabled"
  else
    fail "schedule is enabled" "enabled=$SCHED_ENABLED"
  fi
fi
echo

# ══════════════════════════════════════════════════════════════════
#  Negative control — the SAME argv with the ack flag STRIPPED is REFUSED
# ══════════════════════════════════════════════════════════════════
# A SECOND fresh epic (idempotency trap (c)): the positive epic already has a
# schedule, so a start there would fail on "already" no matter the gate.
blue "[negative] emitted argv MINUS the ack flag → gb refuses → NO schedule"
NEG_EPIC="$(mk_epic neg)"
[ -n "$NEG_EPIC" ] || skip_all "could not create negative-leg epic (daemon unreachable?)"
CREATED_EPICS+=("$NEG_EPIC")
dim "  epic: $NEG_EPIC"

emit_rc=0
emit_argv "$NEG_EPIC" || emit_rc=$?
if [ "$emit_rc" -ne 0 ] || [ "${#EMIT_ARGV[@]}" -lt 3 ]; then
  fail "emit argv for $NEG_EPIC" "emitter produced no usable argv (rc=$emit_rc)"
else
  # Strip the ack flag from the EMITTED argv (do NOT hand-build the argv — start
  # from what gardenerStartArgs actually emitted, then remove exactly one token).
  STRIPPED=()
  removed=0
  for tok in "${EMIT_ARGV[@]}"; do
    if [ "$tok" = "--i-read-the-gardener-instructions" ]; then
      removed=1
      continue
    fi
    STRIPPED+=("$tok")
  done
  if [ "$removed" -eq 1 ]; then
    pass "stripped --i-read-the-gardener-instructions from the emitted argv"
  else
    # If the emitter didn't emit the flag, the positive leg already failed the
    # ka-0000006141 invariant; the negative control is moot but must not green.
    fail "stripped ack flag from emitted argv" \
      "emitted argv had no --i-read-the-gardener-instructions to strip: ${EMIT_ARGV[*]}"
  fi

  NEG_OUT="$(gb "${STRIPPED[@]}" 2>&1)"
  NEG_RC=$?
  if [ "$NEG_RC" -ne 0 ]; then
    pass "gb <argv w/o ack> exited non-zero (gate refused)"
  else
    fail "gb <argv w/o ack> exited non-zero" "rc=0 (gate did NOT refuse), out=$NEG_OUT"
  fi
  # Pin the flag NAME in the refusal so a bare non-zero from any unrelated error
  # can't satisfy this — the refusal must be THE gate.
  if grep -q -- "--i-read-the-gardener-instructions" <<<"$NEG_OUT"; then
    pass "refusal names --i-read-the-gardener-instructions"
  else
    fail "refusal names --i-read-the-gardener-instructions" "out=$NEG_OUT"
  fi
  # Load-bearing half: the refusal must write NOTHING. status exits non-zero when
  # no schedule exists, so a non-zero status proves the stripped start created none.
  NEG_STATUS_OUT="$(gb gardener status "$NEG_EPIC" 2>&1)"
  NEG_STATUS_RC=$?
  if [ "$NEG_STATUS_RC" -ne 0 ]; then
    pass "stripped start created NO schedule (status resolves none)"
  else
    fail "stripped start created NO schedule" "status rc=0, out=$NEG_STATUS_OUT"
  fi
fi
echo

# ── Summary ──────────────────────────────────────────────────────
echo "════════════════════════════════════════════════════"
echo " gardener-start-argv-contract Summary: $PASS passed, $FAIL failed"
echo "════════════════════════════════════════════════════"
if [ "$FAIL" -gt 0 ]; then
  for e in "${ERRORS[@]}"; do red "  - $e"; done
  exit 1
fi
green "All gardener-start-argv-contract assertions passed."
exit 0
