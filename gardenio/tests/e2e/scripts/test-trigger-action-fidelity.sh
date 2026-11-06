#!/usr/bin/env bash
# test-trigger-action-fidelity.sh — trigger ACTION-fidelity E2E (ka-0000006362).
#
# Three trigger-action behaviors shipped with strong UNIT coverage but NO
# end-to-end assertion that drives the REAL evaluation→fireAction loop against a
# live daemon. Existing e2e touches the ADJACENT surface but not these:
#
#   (a) create_bead field fidelity — commit 6f38541 (ka-0000003075). The
#       create_bead action honors bead_type (allowlist task|bug|feature|chore),
#       bead_priority, parent (wired parent-child), and extra_labels. The only
#       e2e that fires a create_bead trigger (trigger-source-bead-per-action.toml)
#       asserts a *task* bead + source_bead_id — never the type override, the
#       priority, the parent edge, the extra label, OR the engine allowlist that
#       rejects a disallowed bead_type at FIRE time (not just the CLI's authoring
#       guard).
#
#   (b) role → role: LABEL mirroring — commit 0fa9233b (ka-0000002971). A
#       spawn_agent trigger create mirrors the `role` FIELD into a role:<role>
#       LABEL at the single trigger-create write chokepoint (triggerBeadLabels),
#       so the label-only ground-truth query `kd list -l role:<r>` finds it. The
#       existing scenario 10 in test-triggers.sh asserts only fields.role — the
#       exact field-vs-label fidelity gap (node_vs_truth_delta) that fix closed.
#
#   (c) assert action, fire-on-VIOLATION — commit 81f4ae5 (ka-0000008157, F5).
#       `gb trigger create assert --on-violation` flips the semantic from
#       fire-on-match to fire-on-violation: the --expr clauses are an invariant
#       expected to HOLD; the trigger fires only when it is VIOLATED (a clause is
#       FALSE) or fails CLOSED (a runtime CEL error → violation, never a silent
#       pass). Every existing assert/CEL e2e drives the fire-on-MATCH path only.
#       This leg asserts the violation path: fires on a violating event with the
#       {{violated}}/{{values}} template vars persisted on the filed bead, stays
#       SILENT when the invariant holds, and fires fail-closed on a broken
#       invariant.
#
# ── Why the trigger-probe, not a live-bridge reconcile wait ───────────────────
#
# The bead-event source (BeadStateTriggerSource) reaches fireAction through the
# SSE buffer → Evaluate → match path. Driving it deterministically otherwise
# means racing a live SSE frame before the engine's per-cycle DrainPending(), or
# standing up the reconciler and sleeping ≥1 reconcile interval (~90s+, the slow
# class that quarantined trigger-source-bead-spawn-agent). Instead we use the
# purpose-built cmd/trigger-probe --source=bead (the same binary the
# trigger-source-*.toml claudeless specs use): it injects ONE synthetic bead
# event into the REAL BeadStateTriggerSource buffer and runs exactly one
# EvaluateSource("bead") pass — a genuine one-event→many-triggers fan-out through
# the real evaluation→fireAction→recordTriggerAudit loop, no reconcile wait.
#
# The triggers themselves are authored with the REAL `gb trigger create`
# subcommands (bead / schedule / assert), so leg (b)'s role→label mirroring and
# every field/flag validation runs through the production authoring chokepoint.
#
# ── Skip-clean contract ───────────────────────────────────────────────────────
#
# HARD assertions when the daemon + tooling are present; a clean SKIP (exit 0)
# with a LOUD banner otherwise — a missing toolchain must NEVER red the suite.
# One version guard is load-bearing: the create_bead trigger-provenance fields
# (trigger_id / source_bead_id / bead_event_key / assert_violated / …) must be
# declared on the FILED type's schema, or the daemon 400s the create at fire
# time. HEAD daemons declare them on every issue-kind type (ka-0000005502); a
# daemon predating that fix cannot file a typed create_bead. Leg (a)'s typed
# create and leg (c)'s create_bead-on-violation therefore probe the live
# type:bug schema first and SKIP those sub-legs LOUDLY (never fail) on a daemon
# that lacks the fields — the release/staging E2E always runs a fresh daemon at
# the release tag, where the fields are present and every leg is a hard gate.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-trigger-action-fidelity.sh

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
  red " SKIP trigger-action-fidelity (LOUD): $why"
  red " This run did NOT assert create_bead field fidelity (type/priority/parent/"
  red " labels + engine allowlist), role→role: LABEL mirroring, OR the assert"
  red " fire-on-violation path. A green suite here proves NOTHING about"
  red " ka-0000003075 / ka-0000002971 / ka-0000008157. The e2e shard ships a live"
  red " daemon + a pre-built trigger-probe (build-trigger-probe in every shard's"
  red " use: list) — if this fires in CI, that wiring regressed."
  red "════════════════════════════════════════════════════"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

remember()      { CREATED_IDS+=("$1"); }
remember_trig() { TRIGGER_IDS+=("$1"); CREATED_IDS+=("$1"); }

cleanup() {
  # Disable triggers first so no stray reconcile fires them post-run, then close
  # every bead we made (triggers, filed beads, audit beads under each trigger).
  for tid in "${TRIGGER_IDS[@]:-}"; do
    [ -n "$tid" ] && "$GB_BIN" trigger disable "$tid" --http-url "$BEADS_HTTP_URL" >/dev/null 2>&1 || true
    # Close audit beads the trigger produced so they don't accumulate.
    for bid in $("$KD_BIN" list --type=event -l "trigger:$tid" --json 2>/dev/null | jq -r '.[].id' 2>/dev/null); do
      "$KD_BIN" close "$bid" >/dev/null 2>&1 || true
    done
    # Close any bead the create_bead action filed under this trigger.
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
echo " Trigger action-fidelity E2E (ka-0000006362)"
echo " create_bead fields → role:label → assert on-violation"
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
  BUILD_LOG="/tmp/trigger-probe-build-${RUN_ID}.log"
  if ( cd "$CONTROLLER_DIR" && CGO_ENABLED=0 go build -o "$PROBE_BIN" ./cmd/trigger-probe/ ) >"$BUILD_LOG" 2>&1; then
    PROBE_BUILT=1
    pass "cmd/trigger-probe built from source"
  else
    skip_all "trigger-probe build failed: $(tail -4 "$BUILD_LOG" 2>/dev/null)"
  fi
fi

# Version guard: does the FILED type (bug) declare the trigger-provenance fields
# createTaskBead unconditionally stamps? Without them the daemon 400s a typed
# create_bead at fire time (ka-0000005502). Present on HEAD/release daemons;
# absent on daemons predating the fix. Gates ONLY the typed-create sub-legs — the
# role→label and notify-only assert legs never file a typed bead and always run.
TYPED_CREATE_OK=0
if curl -s -H "Authorization: Bearer $BEADS_TOKEN" "${BEADS_HTTP_URL}/v1/configs/type:bug" 2>/dev/null \
     | jq -e '.value.fields[]?.name | select(. == "source_bead_id")' >/dev/null 2>&1; then
  TYPED_CREATE_OK=1
  dim "  daemon type:bug declares trigger-provenance fields → typed create_bead sub-legs ARMED"
else
  red  "  ⚠ daemon type:bug LACKS trigger-provenance fields (predates ka-0000005502)"
  red  "    → typed create_bead sub-legs SKIP LOUDLY (a fresh release daemon has them)"
fi

# Per-run tag so we only match OUR synthetic events and beads.
TAG="taf-${RUN_ID}-$(date +%s)"
echo
dim "run tag: $TAG"
echo

# fire_probe <trigger-id> <bead-id> <bead-type> <event> <labels-csv> [outvar-logfile]
# Injects one synthetic bead event and runs a single EvaluateSource("bead") pass.
# Returns the probe exit code; the log path is echoed for the caller to grep.
#
# --ignore-cooldown is load-bearing here (ka-0000007982): this suite runs against
# the SHARED e2e daemon, which the deployed trigger-engine (triggerEngine.enabled
# in the ephemeral overlay, pollInterval 30s) ALSO drives. That engine evaluates
# our freshly-created trigger:bead beads on every poll and stamps last_checked on
# the no-new-matches path — and the engine floors the per-trigger cooldown at 30s
# (a --cooldown 1s is truncated to 0 minutes, so max(30s,0)=30s always applies).
# Whenever a poll lands in the ~1-3s window between our `gb trigger create` and
# this probe, the probe's single deliberate fire would be cooldown-SKIPPED
# (fired=0) — the exact cross-evaluator cooldown-starvation the engine itself
# WARN-detects (ka-0000006237). That race made this the worst flake in the suite
# (11/7d). The flag bypasses the cooldown gate for THIS --trigger only, so a
# foreign last_checked stamp can no longer suppress the fire; every other trigger
# the bead fan-out touches still honors its real cooldown. Also makes the
# silent-when-satisfied leg (c) deterministic: the trigger is genuinely evaluated
# ("no new matches") instead of masquerading-silent via a cooldown skip.
fire_probe() {
  local tid="$1" bid="$2" btype="$3" bevent="$4" blabels="$5"
  local safe_bid log
  safe_bid=$(echo "$bid" | tr -c 'a-zA-Z0-9' '_')
  log="/tmp/taf-probe-${RUN_ID}-${safe_bid}.log"
  "$PROBE_BIN" \
    --source=bead \
    --trigger="$tid" \
    --ignore-cooldown \
    --http-url="$BEADS_HTTP_URL" \
    --token="$BEADS_TOKEN" \
    --bead-id="$bid" \
    --bead-title="taf $bid" \
    --bead-type="$btype" \
    --bead-event="$bevent" \
    --bead-labels="$blabels" >"$log" 2>&1
  local rc=$?
  echo "$log"
  return $rc
}

# audit_field <trigger-id> <field> — echoes the given fields.<field> of the
# first bead-source audit for the trigger (empty if none yet).
audit_field() {
  "$KD_BIN" list --type=event -l "trigger:$1" --json 2>/dev/null | \
    jq -r "[.[] | select(.fields.source==\"bead\")][0].fields.$2 // empty" 2>/dev/null
}

# ══════════════════════════════════════════════════════════════════
# LEG (a): create_bead field fidelity + engine allowlist
# ══════════════════════════════════════════════════════════════════
blue "── LEG (a): create_bead honors bead_type/priority/parent/labels ──"

if [ "$TYPED_CREATE_OK" = "1" ]; then
  A_MATCH="taf-a-match-${TAG}"
  A_SRC="TAF-A-SRC-${TAG}"

  # Parent epic for the parent-child fidelity assertion.
  A_PARENT=$("$KD_BIN" create "taf-a parent epic $TAG" --type=epic \
    -l "e2e:trigger-action-fidelity" -l "e2e-tag:$TAG" --json 2>/dev/null | jq -r '.id // empty')
  if [ -n "$A_PARENT" ]; then
    remember "$A_PARENT"

    # create_bead trigger: file a BUG at priority 1 under A_PARENT with an extra
    # label, when a bead carrying A_MATCH is created.
    A_OUT=$("$GB_BIN" trigger create bead \
      --http-url "$BEADS_HTTP_URL" \
      --title "taf-a-$TAG" \
      --event-types created \
      --condition "label:$A_MATCH:present" \
      --action create_bead \
      --bead-type bug \
      --bead-priority 1 \
      --parent "$A_PARENT" \
      --bead-label "taf-a-extra-$TAG" \
      --cooldown 1s \
      --label "e2e:trigger-action-fidelity" --label "e2e-tag:$TAG" \
      --json 2>&1)
    A_TID=$(echo "$A_OUT" | jq -r '.id // .ID // empty' 2>/dev/null)
    if [ -n "$A_TID" ]; then
      remember_trig "$A_TID"
      pass "create_bead trigger authored ($A_TID: bug/p1/parent/extra-label)"

      A_LOG=$(fire_probe "$A_TID" "$A_SRC" task created "$A_MATCH")
      if grep -q 'fired=1' "$A_LOG" 2>/dev/null; then
        pass "create_bead trigger fired once"
      else
        fail "create_bead trigger fire" "no fired=1 in probe log: $(grep -iE 'fired|error' "$A_LOG" | tail -2)"
      fi

      # The filed bead: type=bug, priority=1, source_bead_id=A_SRC, extra label.
      CB=""
      for _ in 1 2 3 4 5 6 7 8 9 10; do
        CB=$("$KD_BIN" list --type=bug -l "trigger:$A_TID" --json 2>/dev/null | \
          jq -c "[.[] | select(.fields.source_bead_id==\"$A_SRC\")][0] // empty" 2>/dev/null)
        [ -n "$CB" ] && [ "$CB" != "null" ] && break
        sleep 1
      done
      if [ -n "$CB" ] && [ "$CB" != "null" ]; then
        CB_ID=$(echo "$CB" | jq -r '.id')
        remember "$CB_ID"
        [ "$(echo "$CB" | jq -r '.type')" = "bug" ] \
          && pass "filed bead type=bug (bead_type override honored)" \
          || fail "filed bead type" "want bug, got $(echo "$CB" | jq -r '.type')"
        [ "$(echo "$CB" | jq -r '.priority')" = "1" ] \
          && pass "filed bead priority=1 (bead_priority honored)" \
          || fail "filed bead priority" "want 1, got $(echo "$CB" | jq -r '.priority')"
        if echo "$CB" | jq -e '.labels[] | select(. == "taf-a-extra-'"$TAG"'")' >/dev/null 2>&1; then
          pass "filed bead carries extra_labels passthrough"
        else
          fail "filed bead extra label" "taf-a-extra-$TAG not in $(echo "$CB" | jq -c '.labels')"
        fi
        # Parent-child edge filed bead → A_PARENT.
        if "$KD_BIN" show "$CB_ID" --json 2>/dev/null | \
             jq -e '(.dependencies // [])[] | select(.type=="parent-child" and .depends_on_id=="'"$A_PARENT"'")' >/dev/null 2>&1; then
          pass "filed bead wired parent-child under $A_PARENT"
        else
          fail "filed bead parent edge" "no parent-child dep to $A_PARENT"
        fi
      else
        fail "create_bead side-effect" "no bug bead with source_bead_id=$A_SRC under trigger:$A_TID"
      fi
    else
      fail "create_bead trigger authoring" "no ID: $A_OUT"
    fi
  else
    fail "leg (a) parent epic" "kd create returned no id"
  fi

  # Engine allowlist: author a create_bead trigger with a DISALLOWED bead_type
  # by patching the field directly (past the CLI's authoring guard), fire it, and
  # assert the ENGINE rejects it — audit outcome=error naming the allowlist, and
  # ZERO beads filed. This is the fire-time defense createTaskBead enforces.
  A2_MATCH="taf-a2-match-${TAG}"
  A2_SRC="TAF-A2-SRC-${TAG}"
  A2_OUT=$("$GB_BIN" trigger create bead \
    --http-url "$BEADS_HTTP_URL" \
    --title "taf-a2-$TAG" \
    --event-types created \
    --condition "label:$A2_MATCH:present" \
    --action create_bead \
    --cooldown 1s \
    --label "e2e:trigger-action-fidelity" --label "e2e-tag:$TAG" \
    --json 2>&1)
  A2_TID=$(echo "$A2_OUT" | jq -r '.id // .ID // empty' 2>/dev/null)
  if [ -n "$A2_TID" ]; then
    remember_trig "$A2_TID"
    # Inject a disallowed bead_type the CLI would have rejected at authoring.
    "$KD_BIN" update "$A2_TID" --field bead_type=molecule >/dev/null 2>&1
    if [ "$("$KD_BIN" show "$A2_TID" --json 2>/dev/null | jq -r '.fields.bead_type')" = "molecule" ]; then
      # Fire the disallowed trigger; we assert on the AUDIT bead, not the log.
      fire_probe "$A2_TID" "$A2_SRC" task created "$A2_MATCH" >/dev/null
      # Audit must record an error naming the allowlist.
      A2_OUTCOME=""; A2_REASON=""
      for _ in 1 2 3 4 5 6 7 8; do
        A2_OUTCOME=$(audit_field "$A2_TID" outcome)
        [ -n "$A2_OUTCOME" ] && break
        sleep 1
      done
      A2_REASON=$(audit_field "$A2_TID" reason)
      if [ "$A2_OUTCOME" = "error" ] && grep -qi 'invalid bead_type' <<<"$A2_REASON"; then
        pass "engine allowlist rejects disallowed bead_type at fire (audit outcome=error)"
      else
        fail "engine allowlist rejection" "outcome=$A2_OUTCOME reason=$A2_REASON"
      fi
      # And NO issue-kind bead was filed under the rejected trigger.
      A2_FILED=$("$KD_BIN" list -l "trigger:$A2_TID" --json 2>/dev/null | jq 'length' 2>/dev/null)
      [ "${A2_FILED:-0}" = "0" ] \
        && pass "disallowed bead_type filed ZERO beads" \
        || fail "disallowed bead_type leak" "$A2_FILED issue-kind bead(s) filed"
    else
      fail "leg (a2) setup" "could not set bead_type=molecule on $A2_TID"
    fi
  else
    fail "leg (a2) trigger authoring" "no ID: $A2_OUT"
  fi
else
  dim "  ⊘ leg (a) SKIPPED LOUDLY — daemon lacks type:bug trigger fields (see banner above)"
fi
echo

# ══════════════════════════════════════════════════════════════════
# LEG (b): spawn_agent trigger mirrors role FIELD → role: LABEL
# ══════════════════════════════════════════════════════════════════
blue "── LEG (b): trigger create mirrors role field into a role: LABEL ──"

B_ROLE="crew"
B_OUT=$("$GB_BIN" trigger create schedule \
  --http-url "$BEADS_HTTP_URL" \
  --title "taf-b-$TAG" \
  --cron '0 9 * * *' \
  --action spawn_agent \
  --role "$B_ROLE" \
  --agent-project gardenio \
  --prompt "taf-b probe $TAG" \
  --enabled=false \
  --label "e2e:trigger-action-fidelity" --label "e2e-tag:$TAG" \
  --json 2>&1)
B_TID=$(echo "$B_OUT" | jq -r '.id // .ID // empty' 2>/dev/null)
if [ -n "$B_TID" ]; then
  remember_trig "$B_TID"
  pass "spawn_agent trigger authored ($B_TID, role=$B_ROLE)"

  # Field still set (the pre-existing coverage — the fix mirrors, not replaces).
  [ "$("$KD_BIN" show "$B_TID" --json 2>/dev/null | jq -r '.fields.role')" = "$B_ROLE" ] \
    && pass "fields.role=$B_ROLE preserved" \
    || fail "fields.role" "want $B_ROLE, got $("$KD_BIN" show "$B_TID" --json 2>/dev/null | jq -r '.fields.role')"

  # The gap: role:<role> LABEL present on the trigger bead.
  if "$KD_BIN" show "$B_TID" --json 2>/dev/null | jq -e '.labels[] | select(. == "role:'"$B_ROLE"'")' >/dev/null 2>&1; then
    pass "role:$B_ROLE LABEL mirrored onto trigger bead"
  else
    fail "role label mirror" "role:$B_ROLE not in $("$KD_BIN" show "$B_TID" --json 2>/dev/null | jq -c '.labels')"
  fi

  # The ground truth the fix restored: a label-only query finds this trigger.
  if "$KD_BIN" list -l "role:$B_ROLE" -l "e2e-tag:$TAG" --all-types --json 2>/dev/null | \
       jq -e '.[] | select(.id == "'"$B_TID"'")' >/dev/null 2>&1; then
    pass "label-only ground-truth query (kd list -l role:$B_ROLE) finds the trigger"
  else
    fail "label-only query" "kd list -l role:$B_ROLE did not return $B_TID"
  fi
else
  fail "leg (b) trigger authoring" "no ID: $B_OUT"
fi
echo

# ══════════════════════════════════════════════════════════════════
# LEG (c): assert action — fire on VIOLATION (F5)
# ══════════════════════════════════════════════════════════════════
blue "── LEG (c): assert --on-violation fires on invariant violation ──"

# Invariant expressed over a probe-settable quantity (label presence) so BOTH
# the violation and the satisfied paths are drivable with the same trigger:
#   invariant: a matched bead MUST carry the C_REVIEWED label.
C_MATCH="taf-c-match-${TAG}"
C_REVIEWED="taf-c-reviewed-${TAG}"
C_EXPR="bead.labels.exists(l, l == \"$C_REVIEWED\")"

# c.1 — notify-on-violation: always runs (files no typed bead → no version dep).
C_OUT=$("$GB_BIN" trigger create assert \
  --http-url "$BEADS_HTTP_URL" \
  --title "taf-c-notify-$TAG" \
  --event-types created \
  --condition "label:$C_MATCH:present" \
  --expr "$C_EXPR" \
  --on-violation notify \
  --prompt 'INVARIANT VIOLATED on {{bead_id}}: {{violated}} | observed: {{values}}' \
  --cooldown 1s \
  --label "e2e:trigger-action-fidelity" --label "e2e-tag:$TAG" \
  --json 2>&1)
C_TID=$(echo "$C_OUT" | jq -r '.id // .ID // empty' 2>/dev/null)
if [ -n "$C_TID" ]; then
  remember_trig "$C_TID"
  pass "assert on-violation trigger authored ($C_TID, on_violation=notify)"

  # Violation: bead carries C_MATCH but NOT C_REVIEWED → invariant false → fire.
  C_VLOG=$(fire_probe "$C_TID" "TAF-C-VIOL-${TAG}" bug created "$C_MATCH")
  if grep -q 'assertion violated, alert fired' "$C_VLOG" 2>/dev/null && grep -q 'fired=1' "$C_VLOG" 2>/dev/null; then
    pass "assert fires on VIOLATION (invariant false)"
  else
    fail "assert violation fire" "no violation fire in log: $(grep -iE 'fired|violat' "$C_VLOG" | tail -2)"
  fi
  # The audit records action=assert, outcome=allow, and {{violated}} names the clause.
  C_ACTION=""; for _ in 1 2 3 4 5 6 7 8; do C_ACTION=$(audit_field "$C_TID" action); [ -n "$C_ACTION" ] && break; sleep 1; done
  [ "$C_ACTION" = "assert" ] \
    && pass "audit action=assert, outcome=$(audit_field "$C_TID" outcome)" \
    || fail "assert audit" "action=$C_ACTION"
  # The fire log renders `violated="<clause>"` with the CEL string escaped, so
  # match on the clause's distinctive, quote-free substring rather than the
  # exact (escaped) expression.
  if grep -E 'violated=.*bead\.labels\.exists' "$C_VLOG" >/dev/null 2>&1; then
    pass "{{violated}} names the failed clause"
  else
    fail "{{violated}} rendering" "clause not surfaced in fire log: $(grep -o 'violated=[^ ]*' "$C_VLOG" | head -1)"
  fi

  # Satisfied path: bead carries BOTH C_MATCH and C_REVIEWED → invariant holds →
  # SILENT. This MUST use a DEDICATED trigger, not C_TID: the engine floors the
  # per-trigger cooldown at 30s (triggers.go triggerCooldown), so re-firing C_TID
  # inside the same run would be cooldown-SKIPPED — a false "silent" that masks a
  # real regression. A fresh trigger has its own cooldown window, so the only
  # reason it can stay silent is the invariant actually holding.
  CS_OUT=$("$GB_BIN" trigger create assert \
    --http-url "$BEADS_HTTP_URL" \
    --title "taf-c-silent-$TAG" \
    --event-types created \
    --condition "label:$C_MATCH:present" \
    --expr "$C_EXPR" \
    --on-violation notify \
    --prompt 'should never fire: {{violated}}' \
    --cooldown 1s \
    --label "e2e:trigger-action-fidelity" --label "e2e-tag:$TAG" \
    --json 2>&1)
  CS_TID=$(echo "$CS_OUT" | jq -r '.id // .ID // empty' 2>/dev/null)
  if [ -n "$CS_TID" ]; then
    remember_trig "$CS_TID"
    CS_LOG=$(fire_probe "$CS_TID" "TAF-C-OK-${TAG}" bug created "$C_MATCH,$C_REVIEWED")
    # A satisfied assert invariant yields ZERO source matches, so the engine
    # logs "evaluated, no new matches" for this trigger and NEVER "assertion
    # violated". Require the trigger to have been genuinely EVALUATED this pass
    # (that log line present) so a cooldown-skip can't masquerade as silence —
    # the exact false-green a shared-trigger version of this check produced.
    if grep -q 'assertion violated' "$CS_LOG" 2>/dev/null; then
      fail "assert silent-when-satisfied" "trigger fired though invariant held"
    elif grep -q "trigger=$CS_TID.*skipped (cooldown)" "$CS_LOG" 2>/dev/null; then
      fail "assert silent-when-satisfied" "trigger was cooldown-skipped, not genuinely silent"
    elif grep -qE "evaluated, no new matches.*trigger=$CS_TID" "$CS_LOG" 2>/dev/null; then
      pass "assert stays SILENT when invariant holds (evaluated, no new matches)"
    else
      fail "assert silent-when-satisfied" "trigger not cleanly evaluated-silent: $(grep -iE 'trigger='"$CS_TID"'' "$CS_LOG" | tail -2)"
    fi
  else
    fail "leg (c) silent trigger authoring" "no ID: $CS_OUT"
  fi
else
  fail "leg (c) notify trigger authoring" "no ID: $C_OUT"
fi

# c.2 — fail-closed: an invariant that ERRORS at runtime counts as a VIOLATION
# and fires (a broken assertion must never silently pass). Uses create_bead so
# the failed clause is persisted on the filed bead — gated on the version guard.
if [ "$TYPED_CREATE_OK" = "1" ]; then
  FC_MATCH="taf-fc-match-${TAG}"
  FC_SRC="TAF-FC-SRC-${TAG}"
  # Unguarded missing-key access errors at CEL eval time → fail-closed → fires.
  FC_EXPR='bead.nonexistent_field_xyz == "sentinel"'
  FC_OUT=$("$GB_BIN" trigger create assert \
    --http-url "$BEADS_HTTP_URL" \
    --title "taf-fc-$TAG" \
    --event-types created \
    --condition "label:$FC_MATCH:present" \
    --expr "$FC_EXPR" \
    --on-violation create_bead \
    --bead-type bug \
    --prompt 'fail-closed follow-up for {{bead_id}}: {{violated}}' \
    --cooldown 1s \
    --label "e2e:trigger-action-fidelity" --label "e2e-tag:$TAG" \
    --json 2>&1)
  FC_TID=$(echo "$FC_OUT" | jq -r '.id // .ID // empty' 2>/dev/null)
  if [ -n "$FC_TID" ]; then
    remember_trig "$FC_TID"
    FC_LOG=$(fire_probe "$FC_TID" "$FC_SRC" bug created "$FC_MATCH")
    if grep -q 'assertion violated, alert fired' "$FC_LOG" 2>/dev/null; then
      pass "fail-closed: runtime CEL error fires as a violation"
    else
      fail "fail-closed fire" "broken invariant did not fire: $(grep -iE 'fired|fail-closed|error' "$FC_LOG" | tail -2)"
    fi
    # The filed bead persists the violated clause + observed values.
    FCB=""
    for _ in 1 2 3 4 5 6 7 8 9 10; do
      FCB=$("$KD_BIN" list --type=bug -l "trigger:$FC_TID" --json 2>/dev/null | \
        jq -c "[.[] | select(.fields.source_bead_id==\"$FC_SRC\")][0] // empty" 2>/dev/null)
      [ -n "$FCB" ] && [ "$FCB" != "null" ] && break
      sleep 1
    done
    if [ -n "$FCB" ] && [ "$FCB" != "null" ]; then
      remember "$(echo "$FCB" | jq -r '.id')"
      [ "$(echo "$FCB" | jq -r '.fields.assert_violated')" = "$FC_EXPR" ] \
        && pass "filed bead persists assert_violated (the failed clause)" \
        || fail "assert_violated field" "got $(echo "$FCB" | jq -r '.fields.assert_violated')"
      echo "$FCB" | jq -e '.fields.assert_values != null and .fields.assert_values != ""' >/dev/null 2>&1 \
        && pass "filed bead persists assert_values (observed values)" \
        || fail "assert_values field" "empty"
    else
      fail "fail-closed create_bead side-effect" "no bug bead with source_bead_id=$FC_SRC"
    fi
  else
    fail "leg (c2) fail-closed trigger authoring" "no ID: $FC_OUT"
  fi
else
  dim "  ⊘ leg (c) fail-closed create_bead sub-leg SKIPPED LOUDLY — daemon lacks type:bug trigger fields"
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
green " ✓ all $PASS assertions passed — trigger action fidelity verified"
echo "════════════════════════════════════════════════════"
exit 0
