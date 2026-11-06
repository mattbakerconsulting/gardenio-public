#!/usr/bin/env bash
# test-plan-read-daemon-alone.sh — the LIVE beadsapi contract test for the
# decoupled plan READ path (task ka-0000004277, epic ka-0000003451 — "P0:
# decouple CLI reads").
#
# The property under test:
#
#   `gb plan show <plan>` resolves a plan + its phases + steps STRAIGHT FROM
#   THE DAEMON, authenticating with only BEADS_AUTH_TOKEN — no viewer URL. This
#   is the exact environment where every `gb plan` WRITE verb 401s: an agent pod
#   against an OIDC-gated beads-viewer (ka-0000006389). Reads must not share that
#   fate, because a plan is a plain bead graph (an epic + parent-child children +
#   a phase_order/step_order overlay in fields) the daemon already serves.
#
# The hermetic twin of this module — plan_read_contract_test.go — drives the
# REAL *beadsapi.Client against an in-process daemon in `make test`. This module
# is the from-scratch, against-a-real-Keycloak-OIDC-daemon assertion the task
# calls for: it seeds a phased plan through the daemon (kd, which needs only the
# token), then runs the ACTUAL `gb plan show` binary with BOTH BEADS_VIEWER_URL
# and BEADS_VIEWER_INTERNAL_URL UNSET, and asserts the resolved timeline matches
# what it seeded. A companion write scenario proves the decoupling is now
# SYMMETRIC: since P2 (ka-0000004615, MR !5910) repointed every `gb plan` WRITE
# verb off the viewer POST onto the daemon-direct plan_write.go helpers, the same
# seeded plan driven through `gb plan add-step` with the viewer env still unset
# now SUCCEEDS and the new step LANDS — read AND write need only the token.
#
# Scenarios:
#   1. Daemon-alone phased read — seed a 2-phase plan (phase_order over two
#      sub-epics, each with its own step_order overlay, plus a flat step and a
#      non-step doc child), run `gb plan show --json` with both viewer env
#      vars unset, and assert: 2 phases in order, each phase's steps in its
#      step_order, the flat step present, the doc dropped, plan_state +
#      project surfaced. This is the read model an operator sees in the cockpit,
#      resolved with only the token.
#   2. Bearer-only, no viewer needed (human render) — the same read via the
#      plain `gb plan show` (non-JSON) succeeds and prints the phase/step lines,
#      confirming the default human path is decoupled too.
#   3. Write-verb daemon-direct — `gb plan add-step` against the same plan, viewer
#      env still unset, SUCCEEDS (needs only BEADS_AUTH_TOKEN since ka-0000004615)
#      and the new step LANDS on a re-read. Proves the write path is decoupled too,
#      not just the read path.
#
# Self-SKIPs (exit 0) when the daemon is unreachable / no token, so it is inert
# where it cannot run and never reds the suite (ka-0000000847) — a skip is never
# mistaken for a pass.
#
# Idempotency: every seeded bead carries the title prefix plan-read-e2e-<rand>
# and is closed in the cleanup trap. Reruns do not collide.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-plan-read-daemon-alone.sh

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
BEADS_PF_PORT="${BEADS_PF_PORT:-19833}"
KD_BIN="${KD_BIN:-kd}"
GB_BIN="${GB_BIN:-gb}"
PROJECT="${PLAN_READ_PROJECT:-gardenio}"
TITLE_PREFIX="plan-read-e2e"
RANDOM_SUFFIX="$$-$(date +%s)"
OPERATOR="${OPERATOR:-oidc:e2e-plan-read@example.com}"

PASS=0
FAIL=0
SKIP=0
ERRORS=()
BEADS_PF_PID=""
CREATED_BEADS=()

export KD_ACTOR="${KD_ACTOR:-e2e-test}"
# kd/gb mutations need a non-empty operated_by (ka-0000005772). Seeding uses kd.
export BEADS_OPERATED_BY="${BEADS_OPERATED_BY:-$OPERATOR}"

# ── Helpers ───────────────────────────────────────────────────────
red()    { printf '\033[0;31m%s\033[0m\n' "$*"; }
green()  { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()   { printf '\033[0;34m%s\033[0m\n' "$*"; }
yellow() { printf '\033[0;33m%s\033[0m\n' "$*"; }
dim()    { printf '\033[2m%s\033[0m\n' "$*"; }

pass() { green "  ✓ $1"; PASS=$((PASS + 1)); }
fail() {
  local name="$1" reason="${2:-}"
  red "  ✗ $name"
  [ -n "$reason" ] && dim "    $reason"
  FAIL=$((FAIL + 1))
  ERRORS+=("$name: $reason")
}
skip() {
  local name="$1" reason="${2:-}"
  yellow "  ⊘ SKIP: $name${reason:+ — $reason}"
  SKIP=$((SKIP + 1))
}

cleanup() {
  for id in "${CREATED_BEADS[@]:-}"; do
    [ -n "$id" ] || continue
    "$KD_BIN" close "$id" >/dev/null 2>&1 || true
  done
  [ -n "$BEADS_PF_PID" ] && kill "$BEADS_PF_PID" 2>/dev/null || true
}
trap cleanup EXIT

# ── Setup: reach the daemon + load the token ──────────────────────
# If BEADS_HTTP_URL/BEADS_AUTH_TOKEN are already exported (a direct daemon run),
# use them as-is. Otherwise port-forward the in-cluster daemon and read the
# kd-beads-token secret — the same token an agent pod carries as BEADS_AUTH_TOKEN.
setup_daemon() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    dim "  using BEADS_HTTP_URL=$BEADS_HTTP_URL"
  else
    blue "Port-forwarding ${NAMESPACE}/${BEADS_SVC} :${BEADS_PF_PORT} → 8080..."
    kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" \
      "${BEADS_PF_PORT}:8080" >/tmp/plan-read-beads-pf.log 2>&1 &
    BEADS_PF_PID=$!
    sleep 3
    export BEADS_HTTP_URL="http://localhost:${BEADS_PF_PORT}"
  fi
  if [ -z "${BEADS_AUTH_TOKEN:-}" ]; then
    # Accept BEADS_TOKEN (other modules' name) as a fallback source.
    if [ -n "${BEADS_TOKEN:-}" ]; then
      export BEADS_AUTH_TOKEN="$BEADS_TOKEN"
    else
      local secret
      secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
        -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
      [ -n "$secret" ] && export BEADS_AUTH_TOKEN="$secret"
    fi
  fi
  # kd reads BEADS_HTTP_URL/BEADS_TOKEN; align both names so seeding works.
  export BEADS_TOKEN="${BEADS_TOKEN:-${BEADS_AUTH_TOKEN:-}}"
}

# daemon_reachable — a token-authenticated read must succeed, else SKIP.
daemon_reachable() {
  [ -n "${BEADS_AUTH_TOKEN:-}" ] || return 1
  curl -sf -o /dev/null --max-time 10 \
    -H "Authorization: Bearer ${BEADS_AUTH_TOKEN}" \
    "${BEADS_HTTP_URL}/v1/health" 2>/dev/null
}

bead_id_from_create() { grep -oE '(gd|ka)-[A-Za-z0-9]+' <<<"$1" | head -1; }

# ── Seed a phased plan straight through the daemon (kd, token-only) ─
# Returns the plan epic id via stdout; appends every created id to
# CREATED_BEADS. Uses `kd -f` to set the phase_order/step_order/plan_state
# fields the resolver reads — no viewer POST anywhere in the seeding path.
PLAN_ID="" PH1="" PH2="" S1="" S2="" S3="" FLAT="" NONSTEP=""
seed_plan() {
  local tag="${TITLE_PREFIX}-${RANDOM_SUFFIX}"

  # Steps first (so we can name them in the epics' step_order overlays).
  S1=$(bead_id_from_create "$("$KD_BIN" create "${tag}-s1" --type=task \
      -l "project:${PROJECT}" --no-parent 2>&1)")
  S2=$(bead_id_from_create "$("$KD_BIN" create "${tag}-s2" --type=task \
      -l "project:${PROJECT}" --no-parent 2>&1)")
  S3=$(bead_id_from_create "$("$KD_BIN" create "${tag}-s3" --type=bug \
      -l "project:${PROJECT}" --no-parent 2>&1)")
  FLAT=$(bead_id_from_create "$("$KD_BIN" create "${tag}-flat" --type=task \
      -l "project:${PROJECT}" --no-parent 2>&1)")
  # A non-step direct child (type=doc): the resolver must drop it from the
  # timeline, since only task/bug/feature/chore render as steps (isPlanStepType).
  NONSTEP=$(bead_id_from_create "$("$KD_BIN" create "${tag}-doc" --type=doc \
      -l "project:${PROJECT}" --no-parent 2>&1)")

  # Phase sub-epics. PH1 carries a step_order that flips S1/S2 → S2,S1.
  PH1=$(bead_id_from_create "$("$KD_BIN" create "${tag}-ph1" --type=epic \
      -l "project:${PROJECT}" --no-parent -f "step_order=[\"${S2}\",\"${S1}\"]" 2>&1)")
  PH2=$(bead_id_from_create "$("$KD_BIN" create "${tag}-ph2" --type=epic \
      -l "project:${PROJECT}" --no-parent 2>&1)")

  # The plan epic: phase_order over [PH1,PH2], step_order over [FLAT], and a
  # plan_state so `gb plan show` surfaces the lifecycle chip.
  PLAN_ID=$(bead_id_from_create "$("$KD_BIN" create "${tag}-plan" --type=epic \
      -l "plan" -l "project:${PROJECT}" --no-parent \
      -f "plan_state=building" \
      -f "phase_order=[\"${PH1}\",\"${PH2}\"]" \
      -f "step_order=[\"${FLAT}\"]" 2>&1)")

  for id in "$S1" "$S2" "$S3" "$FLAT" "$NONSTEP" "$PH1" "$PH2" "$PLAN_ID"; do
    [ -n "$id" ] && CREATED_BEADS+=("$id")
  done
  [ -n "$PLAN_ID" ] && [ -n "$PH1" ] && [ -n "$PH2" ] || return 1

  # Wire the parent-child graph: plan → {PH1,PH2,FLAT,NONSTEP}, phases → steps.
  "$KD_BIN" dep add "$PH1" "$PLAN_ID" --type parent-child >/dev/null 2>&1
  "$KD_BIN" dep add "$PH2" "$PLAN_ID" --type parent-child >/dev/null 2>&1
  "$KD_BIN" dep add "$FLAT" "$PLAN_ID" --type parent-child >/dev/null 2>&1
  "$KD_BIN" dep add "$NONSTEP" "$PLAN_ID" --type parent-child >/dev/null 2>&1
  "$KD_BIN" dep add "$S1" "$PH1" --type parent-child >/dev/null 2>&1
  "$KD_BIN" dep add "$S2" "$PH1" --type parent-child >/dev/null 2>&1
  "$KD_BIN" dep add "$S3" "$PH2" --type parent-child >/dev/null 2>&1
  printf '%s' "$PLAN_ID"
}

# gb_show_json — run `gb plan show --json` in a scrubbed env: BOTH viewer env
# vars UNSET, only BEADS_AUTH_TOKEN + BEADS_HTTP_URL surviving. This is the crux
# — if the read path secretly needed the viewer, it would fail here.
gb_show_json() {
  env -u BEADS_VIEWER_URL -u BEADS_VIEWER_INTERNAL_URL \
    BEADS_HTTP_URL="$BEADS_HTTP_URL" \
    BEADS_AUTH_TOKEN="$BEADS_AUTH_TOKEN" \
    BEADS_OPERATED_BY="$OPERATOR" \
    "$GB_BIN" plan show "$1" --json 2>&1
}
gb_show_human() {
  env -u BEADS_VIEWER_URL -u BEADS_VIEWER_INTERNAL_URL \
    BEADS_HTTP_URL="$BEADS_HTTP_URL" \
    BEADS_AUTH_TOKEN="$BEADS_AUTH_TOKEN" \
    BEADS_OPERATED_BY="$OPERATOR" \
    "$GB_BIN" plan show "$1" 2>&1
}

# ══════════════════════════════════════════════════════════════════
# Scenario 1: daemon-alone phased read (the contract)
# ══════════════════════════════════════════════════════════════════
scenario_phased_read() {
  blue "Scenario 1: gb plan show resolves the phased plan daemon-alone (no viewer)"

  local out
  out=$(gb_show_json "$PLAN_ID") || {
    fail "gb plan show --json (viewer env unset)" "exited non-zero: $(head -3 <<<"$out")"
    return
  }
  if ! jq -e . >/dev/null 2>&1 <<<"$out"; then
    fail "gb plan show --json returns valid JSON" "got: $(head -3 <<<"$out")"
    return
  fi
  pass "gb plan show --json succeeds with only BEADS_AUTH_TOKEN (both BEADS_VIEWER_* unset)"

  # Header: plan_state + project surfaced.
  local ps proj status
  ps=$(jq -r '.plan_state // empty' <<<"$out")
  proj=$(jq -r '.project // empty' <<<"$out")
  status=$(jq -r '.status // empty' <<<"$out")
  [ "$ps" = "building" ] && pass "plan_state=building surfaced" \
    || fail "plan_state" "want building, got '$ps'"
  [ "$proj" = "$PROJECT" ] && pass "project=$PROJECT inherited" \
    || fail "project" "want $PROJECT, got '$proj'"
  [ "$status" = "open" ] && pass "status=open surfaced" \
    || fail "status" "want open, got '$status'"

  # Two phases, in phase_order.
  local nphases p1id p2id
  nphases=$(jq '.phases | length' <<<"$out")
  [ "$nphases" = "2" ] && pass "resolved 2 phases" \
    || { fail "phase count" "want 2, got $nphases"; return; }
  p1id=$(jq -r '.phases[0].id' <<<"$out")
  p2id=$(jq -r '.phases[1].id' <<<"$out")
  [ "$p1id" = "$PH1" ] && pass "phase 1 is PH1 ($PH1)" \
    || fail "phase 1 id" "want $PH1, got $p1id"
  [ "$p2id" = "$PH2" ] && pass "phase 2 is PH2 ($PH2)" \
    || fail "phase 2 id" "want $PH2, got $p2id"

  # Phase 1 steps honor its step_order overlay (S2 before S1).
  local p1s0 p1s1
  p1s0=$(jq -r '.phases[0].steps[0].id' <<<"$out")
  p1s1=$(jq -r '.phases[0].steps[1].id' <<<"$out")
  { [ "$p1s0" = "$S2" ] && [ "$p1s1" = "$S1" ]; } \
    && pass "phase 1 steps ordered by step_order (S2 then S1)" \
    || fail "phase 1 step order" "want [$S2 $S1], got [$p1s0 $p1s1]"

  # Phase 2 has exactly its one step (S3).
  local p2n p2s0
  p2n=$(jq '.phases[1].steps | length' <<<"$out")
  p2s0=$(jq -r '.phases[1].steps[0].id // empty' <<<"$out")
  { [ "$p2n" = "1" ] && [ "$p2s0" = "$S3" ]; } \
    && pass "phase 2 has its single step S3" \
    || fail "phase 2 steps" "want 1 step $S3, got $p2n steps ($p2s0)"

  # Flat step present; bundle dropped as a non-step; phase steps excluded.
  local flatn flat0 has_nonstep
  flatn=$(jq '.steps | length' <<<"$out")
  flat0=$(jq -r '.steps[0].id // empty' <<<"$out")
  { [ "$flatn" = "1" ] && [ "$flat0" = "$FLAT" ]; } \
    && pass "flat/unphased steps = [FLAT] (phase steps excluded)" \
    || fail "flat steps" "want 1 flat step $FLAT, got $flatn ($flat0)"
  has_nonstep=$(jq --arg b "$NONSTEP" \
    '[ (.steps[]?, .phases[].steps[]?) | select(.id == $b) ] | length' <<<"$out")
  [ "$has_nonstep" = "0" ] && pass "doc child dropped (non-step type)" \
    || fail "bundle drop" "doc $NONSTEP leaked into the timeline ($has_nonstep occurrences)"
}

# ══════════════════════════════════════════════════════════════════
# Scenario 2: the human (non-JSON) render is decoupled too
# ══════════════════════════════════════════════════════════════════
scenario_human_render() {
  blue "Scenario 2: human-readable gb plan show is daemon-alone too"
  local out
  out=$(gb_show_human "$PLAN_ID") || {
    fail "gb plan show (human, viewer env unset)" "exited non-zero: $(head -3 <<<"$out")"
    return
  }
  grep -q "Phase 1:" <<<"$out" && grep -q "$PH1" <<<"$out" \
    && pass "human render prints 'Phase 1:' with PH1" \
    || fail "human render phases" "missing Phase 1/PH1 in: $(head -5 <<<"$out")"
  grep -q "$FLAT" <<<"$out" \
    && pass "human render lists the flat step" \
    || fail "human render flat step" "missing $FLAT in output"
}

# ══════════════════════════════════════════════════════════════════
# Scenario 3: write verb (add-step) is ALSO daemon-direct — no viewer
# ══════════════════════════════════════════════════════════════════
# The SAME plan, driven through a WRITE verb (add-step) with BOTH viewer env
# vars still unset, now SUCCEEDS straight against the daemon — and the new step
# actually LANDS (a re-read surfaces it under the plan). Since P2 (ka-0000004615,
# MR !5910) repointed every `gb plan` write verb off the viewer POST onto the
# daemon-direct plan_write.go helpers, add-step needs only BEADS_AUTH_TOKEN and
# no longer touches a viewer URL — resolvePlanViewerURL and the whole viewer-POST
# machinery were deleted. So BOTH the read (scenarios 1/2) and the write path are
# decoupled: the OIDC-401 blocker (ka-0000006389) is closed for reads AND writes.
# This scenario asserts that symmetry rather than the old read/write asymmetry.
scenario_write_daemon_direct() {
  blue "Scenario 3: write verb (add-step) succeeds daemon-direct (viewer env unset)"
  local step_title="plan-read-e2e-addstep-${RANDOM_SUFFIX}"
  local out rc=0
  out=$(env -u BEADS_VIEWER_URL -u BEADS_VIEWER_INTERNAL_URL \
    BEADS_HTTP_URL="$BEADS_HTTP_URL" \
    BEADS_AUTH_TOKEN="$BEADS_AUTH_TOKEN" \
    BEADS_OPERATED_BY="$OPERATOR" \
    "$GB_BIN" plan add-step "$PLAN_ID" --title "$step_title" --json 2>&1) || rc=$?
  if [ "$rc" -ne 0 ]; then
    fail "add-step daemon-direct" "add-step FAILED with viewer env unset (needs only BEADS_AUTH_TOKEN since ka-0000004615): $(head -3 <<<"$out")"
    return
  fi
  local new_step
  new_step=$(jq -r '.id // empty' <<<"$out" 2>/dev/null)
  if [ -z "$new_step" ]; then
    fail "add-step daemon-direct" "add-step returned no step id; got: $(head -3 <<<"$out")"
    return
  fi
  # Track for the cleanup trap so reruns don't accumulate open steps.
  CREATED_BEADS+=("$new_step")
  pass "add-step succeeds with only BEADS_AUTH_TOKEN (both BEADS_VIEWER_* unset) → $new_step"

  # The step must actually LAND: a fresh daemon-alone read surfaces it as a flat
  # step of the plan (the resolver includes every direct-child step, ordered by
  # the plan's step_order overlay — a bare add appends without touching it).
  local reread landed
  reread=$(gb_show_json "$PLAN_ID") || {
    fail "add-step landed" "re-read gb plan show --json exited non-zero: $(head -3 <<<"$reread")"
    return
  }
  landed=$(jq --arg b "$new_step" \
    '[ (.steps[]?, .phases[].steps[]?) | select(.id == $b) ] | length' <<<"$reread")
  [ "$landed" = "1" ] && pass "new step $new_step lands under the plan (re-read surfaces it)" \
    || fail "add-step landed" "new step $new_step not found in the re-read timeline ($landed occurrences)"
}

# ── Main ──────────────────────────────────────────────────────────
main() {
  blue "╔══════════════════════════════════════════════════════╗"
  blue "║   Plan READ daemon-alone contract (ka-0000004277)     ║"
  blue "╚══════════════════════════════════════════════════════╝"

  setup_daemon
  if ! daemon_reachable; then
    skip "plan-read-daemon-alone" "daemon unreachable or no BEADS_AUTH_TOKEN at ${BEADS_HTTP_URL:-unset}"
    echo ""; blue "Passed: $PASS  Skipped: $SKIP"
    return 0
  fi

  if ! seed_plan >/dev/null; then
    fail "seed phased plan" "kd create/dep failed (plan=$PLAN_ID ph1=$PH1 ph2=$PH2)"
    echo ""; red "Failed: $FAIL"; return 1
  fi
  dim "  seeded plan $PLAN_ID (phases $PH1,$PH2; steps $S1,$S2,$S3,$FLAT; doc $NONSTEP)"

  scenario_phased_read
  scenario_human_render
  scenario_write_daemon_direct

  echo ""
  blue "────────────────────────────────────────────────────"
  green "Passed: $PASS"
  [ "$SKIP" -gt 0 ] && yellow "Skipped: $SKIP"
  if [ "$FAIL" -gt 0 ]; then
    red "Failed: $FAIL"
    for e in "${ERRORS[@]}"; do red "  ✗ $e"; done
  fi
  blue "────────────────────────────────────────────────────"
  [ "$FAIL" -eq 0 ]
}

main
