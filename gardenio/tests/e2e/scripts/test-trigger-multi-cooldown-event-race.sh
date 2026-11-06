#!/usr/bin/env bash
# test-trigger-multi-cooldown-event-race.sh — MULTI-source EventReady
# cooldown-bypass E2E (ka-0000000405, parent ka-0000000958).
#
# ── The gap ───────────────────────────────────────────────────────────────────
#
# MultiTriggerSource.EventReady (trigger_source_multi.go:268, bug ka-0000005988)
# implements the SAME eventReadySource interface the engine's cooldown gate
# switches on (triggers.go). The engine-side "pending event" bypass — the load-
# bearing branch — is already e2e-covered end-to-end by the SIBLING module
# test-trigger-cooldown-event-race.sh (ka-0000002850), but it drives the plain
# BeadStateTriggerSource: cmd/trigger-probe historically only had --source=bead.
#
# What that module does NOT reach is MultiTriggerSource.EventReady's own body,
# which delegates to scheduler.HasPendingEventMatch. That delegation was unit-
# covered (trigger_source_multi_test.go TestMultiTriggerSource_CooldownEventRace)
# but never exercised through the real cooldown gate on a live daemon. This
# module closes that residual by driving --source=multi (added alongside this
# script) through the identical two-probe cooldown-window recipe.
#
# The engine gate that closes the hole (triggers.go, "pending event" bypass):
#
#     if ers, ok := source.(eventReadySource); ok && ers.EventReady(ctx, trig) {
#         bypass = "pending event"
#     }
#
# is source-agnostic, so the log lines this module asserts on
# ("cooldown bypassed … reason=\"pending event\"", "fired=1", "skipped
# (cooldown)") are identical to the bead-source module's — only source=multi
# and the trigger:multi authoring differ. If MultiTriggerSource.EventReady
# regressed to `return false` tomorrow, the POSITIVE leg here would red while
# the bead-source module stayed green (different source), and prod would
# silently drop multi-source event fires that land mid-cooldown.
#
# ── Why the two-probe recipe (prod-faithful, not a hand-set field) ────────────
#
# The unit test hand-stamps LastChecked on a mock daemon. This module instead
# drives last_checked INTO the cooldown window the way PROD does — a REAL non-
# firing evaluation on the shared daemon stamps it (stampLastChecked) — then
# fires a matching event through the real cooldown gate. Mirrors the bead-source
# module exactly:
#
#   POSITIVE leg (fire-through-bypass): a trigger:multi with a real 30m cooldown
#   and one bead_event entry (events:[created], match label:<tag>:present),
#   NO --ignore-cooldown.
#     • Probe A injects a NON-matching event. last_checked is zero on the fresh
#       trigger, so the cooldown gate is skipped, Evaluate finds no match, and
#       the engine stamps last_checked=now — driving it into the cooldown window.
#     • Probe B (seconds later) injects a genuinely MATCHING event. Now
#       last_checked is fresh, so evalOne ENTERS the cooldown gate — and the ONLY
#       reason the fire survives is EventReady() (→ HasPendingEventMatch)
#       reporting the pending match, so bypass="pending event". Asserts probe B
#       logs `cooldown bypassed` reason "pending event" AND `fired=1`, plus a
#       daemon-side audit source=multi outcome=allow. If EventReady regressed to
#       return false, probe B is cooldown-SKIPPED (no fire) → this leg REDS.
#
#   NEGATIVE leg (no false-positive bypass): EventReady mirrors Evaluate's pre-
#   filter, so a NON-matching event in the cooldown window must NOT bypass.
#     • A second trigger:multi, same cooldown. Probe A stamps last_checked
#       (non-match). Probe B injects a NON-matching event inside the window →
#       EventReady() false → evalOne logs `skipped (cooldown)`, no bypass, no
#       fire, ZERO audits. If EventReady regressed to always-true, probe B would
#       log `cooldown bypassed` → this leg REDS. This is the other silent-failure
#       direction and is equally load-bearing.
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
#   ./gardenio/tests/e2e/scripts/test-trigger-multi-cooldown-event-race.sh

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
  red " SKIP trigger-multi-cooldown-event-race (LOUD): $why"
  red " This run did NOT assert the MULTI-source EventReady cooldown bypass"
  red " (triggers.go \"pending event\" via MultiTriggerSource.EventReady ->"
  red " scheduler.HasPendingEventMatch). A green suite here proves NOTHING"
  red " about ka-0000005988 for the multi source: a matching event arriving"
  red " inside the cooldown window could be silently dropped and this suite"
  red " would stay green. The e2e shard ships a live daemon + a pre-built"
  red " trigger-probe (build-trigger-probe in every shard's use: list) — if"
  red " this fires in CI, that wiring regressed."
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
echo " Multi-source EventReady cooldown-bypass E2E (ka-0000000405)"
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
  BUILD_LOG="/tmp/trigger-probe-build-multi-cer-${RUN_ID}.log"
  if ( cd "$CONTROLLER_DIR" && CGO_ENABLED=0 go build -o "$PROBE_BIN" ./cmd/trigger-probe/ ) >"$BUILD_LOG" 2>&1; then
    PROBE_BUILT=1
    pass "cmd/trigger-probe built from source"
  else
    skip_all "trigger-probe build failed: $(tail -4 "$BUILD_LOG" 2>/dev/null)"
  fi
fi

# NOTE on probe version: --source=multi and this script land in the SAME commit,
# and the e2e shard's trigger-probe is compiled from THAT commit's checkout
# (build-trigger-probe, not the deployed image) — the EventReady logic runs
# in-process in the probe, with the daemon used only for bead CRUD. So there is
# no probe/script version skew to guard against here, and no mode-check that
# could accidentally drive EvaluateSource against the live daemon with a default
# event. If a genuinely stale probe ever lacked the mode, the probe's own
# `fatal("unknown --source")` reds the leg loudly — the correct signal.

# Per-run tag so we only match OUR synthetic events and beads.
TAG="mcer-${RUN_ID}-$(date +%s)"
echo
dim "run tag: $TAG"
echo

# CREATE_OUT holds the raw output (stdout+stderr) of the most recent
# create_multi_trigger call so a caller can surface the daemon's error when
# authoring fails (mirrors the sibling module keeping $P_OUT for diagnostics).
CREATE_OUT=""

# create_multi_trigger <title> <match-label> — authors a trigger:multi bead with
# a single bead_event entry (events:[created]) whose match is label:<match-label>
# present, a real 30m cooldown, action=notify. Echoes the trigger id (empty on
# failure; raw output left in $CREATE_OUT). The `source` FIELD is deliberately
# OMITTED: source=multi is not a valid value for the type:trigger `source` enum
# (kbeads config.go), and the engine derives the multi source from the
# trigger:multi LABEL, not the field.
create_multi_trigger() {
  local title="$1" match_label="$2"
  # triggers[] entry: bead_event on created, match on the per-run label present.
  # Same shape scheduler.ParseTriggers + eventMatchesEntry consume in prod.
  local triggers_json
  triggers_json=$(jq -cn --arg lbl "label:${match_label}" \
    '[{kind:"bead_event", events:["created"], match:{field:$lbl, op:"present"}}]')
  CREATE_OUT=$("$KD_BIN" create "$title" \
    --type trigger \
    --http-url "$BEADS_HTTP_URL" \
    --token "$BEADS_TOKEN" \
    -l "trigger:multi" \
    -l "e2e:trigger-multi-cooldown-event-race" \
    -l "e2e-tag:$TAG" \
    -f action=notify \
    -f project=gardenio \
    -f "prompt=mcer fire for {{event_bead_id}}" \
    -f enabled=true \
    -f cooldown_minutes=30 \
    -f "triggers=${triggers_json}" \
    --no-parent \
    --json 2>&1)
  echo "$CREATE_OUT" | jq -r '.id // .ID // empty' 2>/dev/null
}

# fire_probe_nocd <trigger-id> <bead-id> <bead-event> <labels-csv>
# Injects one synthetic bead event into the MULTI source and runs a single
# EvaluateSource("multi") pass WITHOUT --ignore-cooldown, so the real per-trigger
# cooldown gate (and thus MultiTriggerSource.EventReady) is exercised. Echoes the
# per-probe log path; returns the probe exit code.
fire_probe_nocd() {
  local tid="$1" bid="$2" bevent="$3" blabels="$4"
  local safe_bid log
  safe_bid=$(echo "$bid" | tr -c 'a-zA-Z0-9' '_')
  log="/tmp/mcer-probe-${RUN_ID}-${safe_bid}.log"
  "$PROBE_BIN" \
    --source=multi \
    --trigger="$tid" \
    --http-url="$BEADS_HTTP_URL" \
    --token="$BEADS_TOKEN" \
    --bead-id="$bid" \
    --bead-title="mcer $bid" \
    --bead-type=task \
    --bead-event="$bevent" \
    --bead-labels="$blabels" >"$log" 2>&1
  local rc=$?
  echo "$log"
  return $rc
}

# audit_count <trigger-id> — number of multi-source audit beads (type=event) the
# trigger has recorded. A notify fire records exactly one audit (outcome=allow);
# a cooldown-skip records none.
audit_count() {
  "$KD_BIN" list --type=event -l "trigger:$1" --json 2>/dev/null | \
    jq '[.[] | select(.fields.source=="multi")] | length' 2>/dev/null
}
audit_outcome() {
  "$KD_BIN" list --type=event -l "trigger:$1" --json 2>/dev/null | \
    jq -r '[.[] | select(.fields.source=="multi")][0].fields.outcome // empty' 2>/dev/null
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
#               (via MultiTriggerSource.EventReady "pending event" bypass)
# ══════════════════════════════════════════════════════════════════
blue "── POSITIVE: matching event in cooldown window fires (pending-event bypass) ──"

P_MATCH="mcer-p-match-${TAG}"
# 30m cooldown: comfortably longer than the seconds between probe A and probe B,
# so probe B is unambiguously INSIDE the window. Only the EventReady bypass can
# let it fire.
P_TID=$(create_multi_trigger "mcer-positive-$TAG" "$P_MATCH")
if [ -n "$P_TID" ]; then
  remember_trig "$P_TID"
  pass "positive trigger:multi authored ($P_TID, cooldown=30m, bead_event entry, action=notify)"

  # Probe A — NON-matching event, drives last_checked INTO the cooldown window
  # via a REAL non-firing evaluation exactly as prod does. On a fresh trigger
  # last_checked is zero, so the cooldown gate is skipped, Evaluate finds
  # nothing, and the engine stamps last_checked=now. NOTE: this suite runs on
  # the SHARED e2e daemon, which a live trigger-engine (30s poll) also drives —
  # so it may have stamped last_checked FIRST, in which case probe A instead
  # cleanly cooldown-skips its own non-matching event. Both outcomes leave
  # last_checked set, which is all the positive leg needs; the log line is
  # informational only.
  PA_LOG=$(fire_probe_nocd "$P_TID" "MCER-P-WARM-${TAG}" created "mcer-nomatch-${TAG}")
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
  # MultiTriggerSource.EventReady() reports the pending match → bypass="pending
  # event".
  PB_LOG=$(fire_probe_nocd "$P_TID" "MCER-P-FIRE-${TAG}" created "$P_MATCH")

  # (1) The engine took the EventReady branch specifically (not the cron-due or
  # test-seam branch): "cooldown bypassed" with reason "pending event".
  if grep -qE "cooldown bypassed.*trigger=$P_TID.*reason=.?pending event" "$PB_LOG" 2>/dev/null \
     || { grep -qE "cooldown bypassed.*trigger=$P_TID" "$PB_LOG" 2>/dev/null && grep -q 'reason="pending event"' "$PB_LOG" 2>/dev/null; }; then
    pass "probe B: cooldown bypassed via EventReady (reason=\"pending event\")"
  else
    fail "pending-event bypass" "no 'cooldown bypassed … pending event' for $P_TID — MultiTriggerSource.EventReady may have regressed to return false: $(grep -iE "cooldown|bypass|skipped" "$PB_LOG" | tail -3)"
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

  # (4) Daemon-side ground truth: exactly one multi-source audit, outcome=allow.
  PB_AUDITS=""
  for _ in 1 2 3 4 5 6 7 8; do
    PB_AUDITS=$(audit_count "$P_TID"); [ "${PB_AUDITS:-0}" -ge 1 ] 2>/dev/null && break; sleep 1
  done
  if [ "${PB_AUDITS:-0}" -ge 1 ] 2>/dev/null && [ "$(audit_outcome "$P_TID")" = "allow" ]; then
    pass "daemon audit recorded the fire (source=multi outcome=allow, count=$PB_AUDITS)"
  else
    fail "fire audit" "expected ≥1 multi audit outcome=allow, got count=${PB_AUDITS:-0} outcome=$(audit_outcome "$P_TID")"
  fi
else
  fail "positive trigger authoring" "no ID: $CREATE_OUT"
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
N_MATCH="mcer-n-match-${TAG}"
N_OTHER="mcer-n-other-${TAG}"
N_TID=$(create_multi_trigger "mcer-negative-$TAG" "$N_MATCH")
if [ -n "$N_TID" ]; then
  remember_trig "$N_TID"
  pass "negative trigger:multi authored ($N_TID, cooldown=30m)"

  # Probe A — non-matching event: drives last_checked into the cooldown window,
  # no fire. On this SHARED daemon the live trigger-engine (30s poll) may have
  # stamped last_checked FIRST, in which case probe A cleanly cooldown-skips its
  # own non-matching event instead of logging "evaluated, no new matches". BOTH
  # outcomes leave last_checked set (asserted next) and NEITHER fires — so accept
  # either warm-up shape and assert the load-bearing invariant directly: probe A
  # did NOT fire. (Asserting one specific shape would re-introduce the cross-
  # evaluator flake the positive leg's probe A comment already calls out.)
  NA_LOG=$(fire_probe_nocd "$N_TID" "MCER-N-WARM-${TAG}" created "$N_OTHER")
  if grep -qE "(evaluated, no new matches|skipped \(cooldown\)).*trigger=$N_TID" "$NA_LOG" 2>/dev/null; then
    dim "  probe A: $(grep -oE "(evaluated, no new matches|skipped \(cooldown\))" "$NA_LOG" 2>/dev/null | head -1)"
    pass "probe A: non-matching event ran without firing, drives last_checked"
  else
    fail "negative warm-up eval" "expected 'evaluated, no new matches' or 'skipped (cooldown)' for $N_TID: $(grep -iE "trigger=$N_TID" "$NA_LOG" | tail -2)"
  fi
  if grep -qE "evaluated, fired actions.*trigger=$N_TID" "$NA_LOG" 2>/dev/null; then
    fail "negative warm-up fire" "trigger $N_TID FIRED on the warm-up non-matching event — the label match should have gated it"
  fi
  N_LC=$(wait_last_checked_set "$N_TID")
  [ -n "$N_LC" ] \
    && pass "last_checked set on daemon ($N_LC) — negative trigger inside its window" \
    || fail "negative last_checked" "trigger $N_TID has no last_checked after probe A"

  # Probe B — a DIFFERENT non-matching event, still inside the window. EventReady
  # must report NO pending match (it mirrors Evaluate's pre-filter), so evalOne
  # cooldown-SKIPS: no bypass, no fire.
  NB_LOG=$(fire_probe_nocd "$N_TID" "MCER-N-PROBE-${TAG}" created "$N_OTHER-2")

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

  # (3) Daemon-side ground truth: ZERO multi-source audits under this trigger.
  sleep 2
  NB_AUDITS=$(audit_count "$N_TID")
  [ "${NB_AUDITS:-0}" = "0" ] \
    && pass "daemon recorded ZERO audits for the negative trigger" \
    || fail "negative audit leak" "expected 0 multi audits, got ${NB_AUDITS}"
else
  fail "negative trigger authoring" "no ID: $CREATE_OUT"
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
green " ✓ all $PASS assertions passed — MULTI-source EventReady cooldown bypass verified"
echo "════════════════════════════════════════════════════"
exit 0
