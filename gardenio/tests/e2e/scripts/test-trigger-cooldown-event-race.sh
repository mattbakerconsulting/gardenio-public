#!/usr/bin/env bash
# test-trigger-cooldown-event-race.sh — bead-source EventReady cooldown-bypass
# E2E (ka-0000002850, parent ka-0000000958).
#
# ── The gap ───────────────────────────────────────────────────────────────────
#
# BeadStateTriggerSource.EventReady (trigger_source_bead.go:406, bug
# ka-0000001376) is the PRODUCTION fix for the bead-source cooldown/event race:
# an event arriving while last_checked is inside the cooldown floor was buffered,
# never consumed (evalOne cooldown-skips BEFORE Evaluate runs), then dropped at
# cycle end — a silently lost fire. The engine gate that closes the hole is
# triggers.go:2093:
#
#     if ers, ok := source.(eventReadySource); ok && ers.EventReady(ctx, trig) {
#         bypass = "pending event"
#     }
#
# That gate had ZERO e2e coverage. The one e2e in its blast radius
# (test-trigger-action-fidelity.sh) deliberately ROUTES AROUND it — it fires
# every probe with --ignore-cooldown, which trips the `cooldownIgnored` TEST SEAM
# at triggers.go:2085, a DIFFERENT branch of the same `if` checked BEFORE
# EventReady. So the "pending event" bypass was never exercised end-to-end: if
# EventReady regressed to `return false` tomorrow, every shell module and every
# Playwright spec would stay green while prod silently dropped bead-trigger fires
# again (the exact regression that hard-failed release 2026.218.14 on shard 3).
#
# ── Why the two-probe recipe (prod-faithful, not a hand-set field) ────────────
#
# The unit test TestBeadStateTriggerSource_CooldownEventRace hand-stamps
# LastChecked: time.Now().Add(-1*time.Minute) on a mock daemon. This module
# instead drives last_checked INTO the cooldown window the way PROD does — a
# REAL non-firing evaluation on the shared daemon stamps it (stampLastChecked at
# triggers.go:2301) — then fires a matching event through the real cooldown gate:
#
#   POSITIVE leg (fire-through-bypass): a bead trigger with a real 30m cooldown
#   (NO --ignore-cooldown).
#     • Probe A injects a NON-matching event. last_checked is zero on the fresh
#       trigger, so the cooldown gate is skipped, Evaluate finds no match, and
#       the engine stamps last_checked=now — driving it into the cooldown window.
#     • Probe B (seconds later) injects a genuinely MATCHING event. Now
#       last_checked is fresh, so evalOne ENTERS the cooldown gate — and the ONLY
#       reason the fire survives is EventReady() reporting the pending match, so
#       bypass="pending event". Asserts probe B logs `cooldown bypassed` with
#       reason "pending event" AND `fired=1`, plus a daemon-side audit
#       outcome=allow. If EventReady regressed to return false, probe B is
#       cooldown-SKIPPED (no fire) → this leg REDS.
#
#   NEGATIVE leg (no false-positive bypass): EventReady mirrors Evaluate's
#   pre-filter, so a NON-matching event in the cooldown window must NOT bypass.
#     • A second trigger, same cooldown. Probe A stamps last_checked (non-match).
#       Probe B injects a NON-matching event inside the window → EventReady()
#       false → evalOne logs `skipped (cooldown)`, no bypass, no fire, ZERO
#       audits. If EventReady regressed to always-true, probe B would log
#       `cooldown bypassed` → this leg REDS. This is the other silent-failure
#       direction and is equally load-bearing.
#
# MultiTriggerSource.EventReady (trigger_source_multi.go:268, bug ka-0000005988)
# implements the SAME eventReadySource interface the engine gate switches on, so
# the engine-side "pending event" bypass this module exercises is the identical
# code path for both sources. Its own body delegates to
# scheduler.HasPendingEventMatch (unit-covered); cmd/trigger-probe has no `multi`
# mode, so an e2e over the multi wrapper is a separate lift — tracked as a
# follow-up, not folded in here.
#
# ── Skip-clean contract ───────────────────────────────────────────────────────
#
# HARD assertions when the daemon + probe are present; a clean SKIP (exit 0) with
# a LOUD banner otherwise — a missing toolchain must NEVER red the suite. The
# release/staging E2E always runs a fresh daemon at the release tag with the
# pre-built trigger-probe on PATH (build-trigger-probe rides every shard's use:
# list), where every leg is a hard gate.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-trigger-cooldown-event-race.sh

set -uo pipefail

# ── Config ────────────────────────────────────────────────────────
CONTROLLER_DIR="${CONTROLLER_DIR:-gardenio/controller}"
GB_BIN="${GB_BIN:-gb}"
KD_BIN="${KD_BIN:-kd}"
RUN_ID="${RUN_ID:-$$}"
PROBE_BIN=""
PROBE_BUILT=0   # 1 iff we built a temp probe that cleanup() must rm

PASS=0
FAIL=0
ERRORS=()
CREATED_IDS=()   # every trigger / bead we create, closed on exit
TRIGGER_IDS=()   # trigger beads, disabled on exit before close

# ── Helpers ───────────────────────────────────────────────────────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

pass() { green "  ✓ $1"; PASS=$((PASS + 1)); }
fail() {
  red "  ✗ $1"
  [ -n "${2:-}" ] && dim "    $2"
  FAIL=$((FAIL + 1))
  ERRORS+=("$1: ${2:-}")
}

# skip_all exits 0 (a missing daemon/toolchain must NOT red the suite) with a
# LOUD banner so a skip is never silently read as a pass.
skip_all() {
  local why="$1"
  red "════════════════════════════════════════════════════"
  red " SKIP trigger-cooldown-event-race (LOUD): $why"
  red " This run did NOT assert the bead-source EventReady cooldown bypass"
  red " (triggers.go:2093 \"pending event\"). A green suite here proves NOTHING"
  red " about ka-0000001376: a matching event arriving inside the cooldown"
  red " window could be silently dropped and this suite would stay green. The"
  red " e2e shard ships a live daemon + a pre-built trigger-probe"
  red " (build-trigger-probe in every shard's use: list) — if this fires in CI,"
  red " that wiring regressed."
  red "════════════════════════════════════════════════════"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

remember()      { CREATED_IDS+=("$1"); }
remember_trig() { TRIGGER_IDS+=("$1"); CREATED_IDS+=("$1"); }

cleanup() {
  # Disable triggers first so no stray reconcile fires them post-run, then close
  # every bead we made (triggers, audit beads under each trigger).
  for tid in "${TRIGGER_IDS[@]:-}"; do
    [ -n "$tid" ] && "$GB_BIN" trigger disable "$tid" --http-url "$BEADS_HTTP_URL" >/dev/null 2>&1 || true
    for bid in $("$KD_BIN" list --type=event -l "trigger:$tid" --json 2>/dev/null | jq -r '.[].id' 2>/dev/null); do
      "$KD_BIN" close "$bid" >/dev/null 2>&1 || true
    done
    for bid in $("$KD_BIN" list -l "trigger:$tid" --all-types --json 2>/dev/null | jq -r '.[].id' 2>/dev/null); do
      "$KD_BIN" close "$bid" >/dev/null 2>&1 || true
    done
  done
  for id in "${CREATED_IDS[@]:-}"; do
    [ -n "$id" ] && "$KD_BIN" close "$id" >/dev/null 2>&1 || true
  done
  [ "$PROBE_BUILT" = "1" ] && [ -n "$PROBE_BIN" ] && rm -f "$PROBE_BIN" 2>/dev/null || true
}
trap cleanup EXIT

echo "════════════════════════════════════════════════════"
echo " Bead-source EventReady cooldown-bypass E2E (ka-0000002850)"
echo " a matching event inside the cooldown window still fires"
echo "════════════════════════════════════════════════════"

# ── Pre-flight: daemon + tooling ──────────────────────────────────
[ -n "${BEADS_HTTP_URL:-}" ] || skip_all "BEADS_HTTP_URL not set (no live daemon)"
[ -n "${BEADS_TOKEN:-}" ]    || skip_all "BEADS_TOKEN not set"
command -v "$GB_BIN" >/dev/null 2>&1 || skip_all "gb not present"
command -v "$KD_BIN" >/dev/null 2>&1 || skip_all "kd not present"
command -v jq        >/dev/null 2>&1 || skip_all "jq not present"

# Resolve the probe: prefer the pre-built build-trigger-probe binary on PATH
# (rides every shard's use: list), fall back to an in-shell build from source.
if command -v trigger-probe >/dev/null 2>&1; then
  PROBE_BIN="$(command -v trigger-probe)"
  pass "using pre-built trigger-probe ($PROBE_BIN)"
elif [ -x /usr/local/bin/trigger-probe ]; then
  PROBE_BIN=/usr/local/bin/trigger-probe
  pass "using trigger-probe ($PROBE_BIN)"
else
  command -v go >/dev/null 2>&1 \
    || skip_all "no pre-built trigger-probe on PATH and no go toolchain to build it"
  [ -d "$CONTROLLER_DIR/cmd/trigger-probe" ] \
    || skip_all "no pre-built trigger-probe and cmd/trigger-probe missing under $CONTROLLER_DIR"
  blue "[build] cmd/trigger-probe (from source)"
  PROBE_BIN="$(mktemp "/tmp/trigger-probe.${RUN_ID}.XXXXXX")"
  BUILD_LOG="/tmp/trigger-probe-build-cer-${RUN_ID}.log"
  if ( cd "$CONTROLLER_DIR" && CGO_ENABLED=0 go build -o "$PROBE_BIN" ./cmd/trigger-probe/ ) >"$BUILD_LOG" 2>&1; then
    PROBE_BUILT=1
    pass "cmd/trigger-probe built from source"
  else
    skip_all "trigger-probe build failed: $(tail -4 "$BUILD_LOG" 2>/dev/null)"
  fi
fi

# Per-run tag so we only match OUR synthetic events and beads.
TAG="cer-${RUN_ID}-$(date +%s)"
echo
dim "run tag: $TAG"
echo

# fire_probe_nocd <trigger-id> <bead-id> <bead-event> <labels-csv>
# Injects one synthetic bead event and runs a single EvaluateSource("bead") pass
# WITHOUT --ignore-cooldown, so the real per-trigger cooldown gate (and thus the
# EventReady bypass) is exercised. Echoes the per-probe log path; returns the
# probe exit code.
fire_probe_nocd() {
  local tid="$1" bid="$2" bevent="$3" blabels="$4"
  local safe_bid log
  safe_bid=$(echo "$bid" | tr -c 'a-zA-Z0-9' '_')
  log="/tmp/cer-probe-${RUN_ID}-${safe_bid}.log"
  "$PROBE_BIN" \
    --source=bead \
    --trigger="$tid" \
    --http-url="$BEADS_HTTP_URL" \
    --token="$BEADS_TOKEN" \
    --bead-id="$bid" \
    --bead-title="cer $bid" \
    --bead-type=task \
    --bead-event="$bevent" \
    --bead-labels="$blabels" >"$log" 2>&1
  local rc=$?
  echo "$log"
  return $rc
}

# audit_count <trigger-id> — number of bead-source audit beads (type=event) the
# trigger has recorded. A notify fire records exactly one audit (outcome=allow);
# a cooldown-skip records none.
audit_count() {
  "$KD_BIN" list --type=event -l "trigger:$1" --json 2>/dev/null | \
    jq '[.[] | select(.fields.source=="bead")] | length' 2>/dev/null
}
audit_outcome() {
  "$KD_BIN" list --type=event -l "trigger:$1" --json 2>/dev/null | \
    jq -r '[.[] | select(.fields.source=="bead")][0].fields.outcome // empty' 2>/dev/null
}

# Wait until the daemon reflects our first stampLastChecked write, so probe B
# reliably sees a fresh last_checked (the cooldown-gate precondition). The stamp
# is a synchronous UpdateBeadFields inside probe A's EvaluateSource, so it is
# durable the instant probe A exits; poll briefly to tolerate read-replica lag.
wait_last_checked_set() {
  local tid="$1"
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    local lc
    lc=$("$KD_BIN" show "$tid" --json 2>/dev/null | jq -r '.fields.last_checked // empty' 2>/dev/null)
    [ -n "$lc" ] && { echo "$lc"; return 0; }
    sleep 1
  done
  echo ""
  return 1
}

# ══════════════════════════════════════════════════════════════════
# POSITIVE LEG: a matching event inside the cooldown window still FIRES
#               (via the EventReady "pending event" bypass)
# ══════════════════════════════════════════════════════════════════
blue "── POSITIVE: matching event in cooldown window fires (pending-event bypass) ──"

P_MATCH="cer-p-match-${TAG}"
# 30m cooldown: comfortably longer than the seconds between probe A and probe B,
# so probe B is unambiguously INSIDE the window. Only the EventReady bypass can
# let it fire.
P_OUT=$("$GB_BIN" trigger create bead \
  --http-url "$BEADS_HTTP_URL" \
  --title "cer-positive-$TAG" \
  --event-types created \
  --condition "label:$P_MATCH:present" \
  --action notify \
  --prompt "cer positive fire for {{bead_id}}" \
  --cooldown 30m \
  --label "e2e:trigger-cooldown-event-race" --label "e2e-tag:$TAG" \
  --json 2>&1)
P_TID=$(echo "$P_OUT" | jq -r '.id // .ID // empty' 2>/dev/null)
if [ -n "$P_TID" ]; then
  remember_trig "$P_TID"
  pass "positive bead trigger authored ($P_TID, cooldown=30m, action=notify)"

  # Probe A — NON-matching event, drives last_checked INTO the cooldown window
  # via a REAL non-firing evaluation exactly as prod does. On a fresh trigger
  # last_checked is zero, so the cooldown gate is skipped, Evaluate finds
  # nothing, and the engine stamps last_checked=now. NOTE: this suite runs on
  # the SHARED e2e daemon, which a live trigger-engine (30s poll) also drives —
  # so it may have stamped last_checked FIRST, in which case probe A instead
  # cleanly cooldown-skips its own non-matching event. Both outcomes leave
  # last_checked set, which is all the positive leg needs; the log line is
  # informational only (asserting it would re-introduce the cross-evaluator
  # flake that forced trigger-action-fidelity onto --ignore-cooldown).
  PA_LOG=$(fire_probe_nocd "$P_TID" "CER-P-WARM-${TAG}" created "cer-nomatch-${TAG}")
  dim "  probe A: $(grep -oE "(evaluated, no new matches|skipped \(cooldown\))" "$PA_LOG" 2>/dev/null | head -1 || echo 'eval ran')"

  # The load-bearing precondition: last_checked is set (by probe A OR the live
  # engine), so the trigger is genuinely inside its 30m cooldown window and
  # probe B must pass through the cooldown gate to fire.
  P_LC=$(wait_last_checked_set "$P_TID")
  if [ -n "$P_LC" ]; then
    pass "last_checked set on daemon ($P_LC) — trigger is inside its cooldown window"
  else
    fail "last_checked set" "trigger $P_TID has no last_checked after probe A — cannot exercise the cooldown gate"
  fi

  # Probe B — genuinely MATCHING event, still inside the window. last_checked is
  # fresh, so evalOne ENTERS the cooldown gate; the fire survives ONLY because
  # EventReady() reports the pending match → bypass="pending event".
  PB_LOG=$(fire_probe_nocd "$P_TID" "CER-P-FIRE-${TAG}" created "$P_MATCH")

  # (1) The engine took the EventReady branch specifically (not the cron-due or
  # test-seam branch): "cooldown bypassed" with reason "pending event".
  if grep -qE "cooldown bypassed.*trigger=$P_TID.*reason=.?pending event" "$PB_LOG" 2>/dev/null \
     || { grep -qE "cooldown bypassed.*trigger=$P_TID" "$PB_LOG" 2>/dev/null && grep -q 'reason="pending event"' "$PB_LOG" 2>/dev/null; }; then
    pass "probe B: cooldown bypassed via EventReady (reason=\"pending event\")"
  else
    fail "pending-event bypass" "no 'cooldown bypassed … pending event' for $P_TID — EventReady may have regressed to return false: $(grep -iE "cooldown|bypass|skipped" "$PB_LOG" | tail -3)"
  fi

  # (2) It actually FIRED this pass (fired=1), not merely evaluated.
  if grep -qE "evaluated, fired actions.*trigger=$P_TID.*fired=1" "$PB_LOG" 2>/dev/null \
     || grep -q 'fired=1' "$PB_LOG" 2>/dev/null; then
    pass "probe B: trigger fired inside the cooldown window (fired=1)"
  else
    fail "in-cooldown fire" "matching event inside window did not fire: $(grep -iE 'fired|skipped|bypass' "$PB_LOG" | tail -3)"
  fi

  # (3) It must NOT have been cooldown-skipped — the silent-drop regression.
  if grep -qE "skipped \(cooldown\).*trigger=$P_TID" "$PB_LOG" 2>/dev/null; then
    fail "silent drop regression" "trigger $P_TID was cooldown-SKIPPED with a matching event pending (EventReady bypass failed)"
  else
    pass "probe B: NOT cooldown-skipped (the buffered match was not silently dropped)"
  fi

  # (4) Daemon-side ground truth: exactly one bead-source audit, outcome=allow.
  PB_AUDITS=""
  for _ in 1 2 3 4 5 6 7 8; do
    PB_AUDITS=$(audit_count "$P_TID"); [ "${PB_AUDITS:-0}" -ge 1 ] 2>/dev/null && break; sleep 1
  done
  if [ "${PB_AUDITS:-0}" -ge 1 ] 2>/dev/null && [ "$(audit_outcome "$P_TID")" = "allow" ]; then
    pass "daemon audit recorded the fire (outcome=allow, count=$PB_AUDITS)"
  else
    fail "fire audit" "expected ≥1 bead audit outcome=allow, got count=${PB_AUDITS:-0} outcome=$(audit_outcome "$P_TID")"
  fi
else
  fail "positive trigger authoring" "no ID: $P_OUT"
fi
echo

# ══════════════════════════════════════════════════════════════════
# NEGATIVE LEG: a NON-matching event inside the window does NOT bypass
#               (EventReady mirrors Evaluate's pre-filter — no false positive)
# ══════════════════════════════════════════════════════════════════
blue "── NEGATIVE: non-matching event in cooldown window does NOT bypass/fire ──"

# A DEDICATED trigger (not the positive one): the positive trigger already fired
# and stamped its seen-set, so reusing it would confound the assertion. A fresh
# trigger has its own clean cooldown window and seen-set.
N_MATCH="cer-n-match-${TAG}"
N_OTHER="cer-n-other-${TAG}"
N_OUT=$("$GB_BIN" trigger create bead \
  --http-url "$BEADS_HTTP_URL" \
  --title "cer-negative-$TAG" \
  --event-types created \
  --condition "label:$N_MATCH:present" \
  --action notify \
  --prompt "cer NEGATIVE should never fire for {{bead_id}}" \
  --cooldown 30m \
  --label "e2e:trigger-cooldown-event-race" --label "e2e-tag:$TAG" \
  --json 2>&1)
N_TID=$(echo "$N_OUT" | jq -r '.id // .ID // empty' 2>/dev/null)
if [ -n "$N_TID" ]; then
  remember_trig "$N_TID"
  pass "negative bead trigger authored ($N_TID, cooldown=30m)"

  # Probe A — non-matching event: stamps last_checked, no fire (drives it into
  # the cooldown window, same as the positive leg).
  NA_LOG=$(fire_probe_nocd "$N_TID" "CER-N-WARM-${TAG}" created "$N_OTHER")
  if grep -qE "evaluated, no new matches.*trigger=$N_TID" "$NA_LOG" 2>/dev/null; then
    pass "probe A: non-matching event evaluated cleanly, stamps last_checked"
  else
    fail "negative warm-up eval" "expected 'evaluated, no new matches' for $N_TID: $(grep -iE "trigger=$N_TID" "$NA_LOG" | tail -2)"
  fi
  N_LC=$(wait_last_checked_set "$N_TID")
  [ -n "$N_LC" ] \
    && pass "last_checked set on daemon ($N_LC) — negative trigger inside its window" \
    || fail "negative last_checked" "trigger $N_TID has no last_checked after probe A"

  # Probe B — a DIFFERENT non-matching event, still inside the window. EventReady
  # must report NO pending match (it mirrors Evaluate's pre-filter), so evalOne
  # cooldown-SKIPS: no bypass, no fire.
  NB_LOG=$(fire_probe_nocd "$N_TID" "CER-N-PROBE-${TAG}" created "$N_OTHER-2")

  # (1) Must be cooldown-skipped, NOT bypassed.
  if grep -qE "cooldown bypassed.*trigger=$N_TID" "$NB_LOG" 2>/dev/null; then
    fail "false-positive bypass" "trigger $N_TID bypassed cooldown for a NON-matching event — EventReady may have regressed to always-true: $(grep -iE 'bypass' "$NB_LOG" | tail -2)"
  elif grep -qE "skipped \(cooldown\).*trigger=$N_TID" "$NB_LOG" 2>/dev/null; then
    pass "probe B: non-matching event correctly cooldown-skipped (no false-positive bypass)"
  else
    fail "negative skip" "trigger $N_TID neither bypassed nor cleanly cooldown-skipped: $(grep -iE "trigger=$N_TID" "$NB_LOG" | tail -3)"
  fi

  # (2) It did NOT fire.
  if grep -qE "evaluated, fired actions.*trigger=$N_TID" "$NB_LOG" 2>/dev/null; then
    fail "negative fire" "trigger $N_TID fired on a non-matching event"
  else
    pass "probe B: trigger did NOT fire (non-matching event in window stays silent)"
  fi

  # (3) Daemon-side ground truth: ZERO bead-source audits under this trigger.
  sleep 2
  NB_AUDITS=$(audit_count "$N_TID")
  [ "${NB_AUDITS:-0}" = "0" ] \
    && pass "daemon recorded ZERO audits for the negative trigger" \
    || fail "negative audit leak" "expected 0 bead audits, got ${NB_AUDITS}"
else
  fail "negative trigger authoring" "no ID: $N_OUT"
fi
echo

# ── Summary ────────────────────────────────────────────────────────
echo "════════════════════════════════════════════════════"
if [ "$FAIL" -gt 0 ]; then
  red " ✗ $PASS passed, $FAIL failed"
  for err in "${ERRORS[@]}"; do red "   - $err"; done
  echo "════════════════════════════════════════════════════"
  exit 1
fi
green " ✓ all $PASS assertions passed — bead-source EventReady cooldown bypass verified"
echo "════════════════════════════════════════════════════"
exit 0
