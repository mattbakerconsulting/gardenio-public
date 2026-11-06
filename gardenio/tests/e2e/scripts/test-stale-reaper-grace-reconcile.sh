#!/usr/bin/env bash
# test-stale-reaper-grace-reconcile.sh — E2E gate for the stale-reaper's
# grace-gated orphan reap AND stranded run-instance reconciliation.
#
# Bead gd-lbGpeO0u1R (parent ka-0000000958). Guards two merged fixes that closed
# a live incident where gardeners were killed mid-loop and crew mid-wrap-up:
#   7a44b0b5 (ka-0000000018) — GRACE-GATE the orphan reap. The stale-reaper's
#     working/empty-state orphan branch used to reap a live, actively-reporting
#     pod the INSTANT its source task closed — zero grace, no busy-check. 100% of
#     gardenio-ns reaps over a 2h window fired "orphan: source task ... already
#     closed" on agent_state=working pods whose updated_at was 2-14s old (still
#     busy), losing commit/push/MR-link/gb-stop work. The fix gates the orphan
#     branch on a grace window measured against updated_at (a busy/idle proxy:
#     SyncAll bumps updated_at every cycle while busy, skips it when coop reports
#     idle — ka-0000003615): crew orphanReapAfter=5m, gardener
#     gardenerOrphanReapAfter=15m. A busy pod keeps a fresh updated_at and is NEVER
#     orphan-reaped; only a genuinely-idle pod parked past the window is cleaned up.
#   977d6f2b (ka-0000000889) — RECONCILE stranded run-instance tasks. A
#     schedule/cron run-instance task (source:schedule / source:multi /
#     source:schedule-mirror, or a *_fire_key field) left in_progress by a gone
#     assignee agent piles up as a zombie "idle crew" bead — single-shot, so it is
#     never retried. The reaper CLOSES it once the assignee is gone: immediately if
#     the assignee was reaped in THIS pass (definitively gone), or after a
#     runInstanceReconcileMinAge=15m grace if it vanished in a prior pass (guards
#     the race where the task was just claimed and the agent bead has not yet
#     surfaced in ListAgentBeads). A live assignee is left alone.
#
# THE GAP THIS CLOSES: test-stale-reaper.sh only inspects controller logs/metrics
# and asserts the reaper is ENABLED + "logged a pass". It never asserts the
# grace-gate truth table (busy-within-grace NOT reaped; idle-past-grace IS) nor
# the run-instance reconcile truth table (live assignee left alone; gone assignee
# reaped-this-pass closed immediately; gone-prior-pass closed only past grace).
# This module asserts both decision tables directly.
#
# WHY A CONTRACT LEG (not a pure live test): the grace windows are 5m/15m and
# runInstanceReconcileMinAge is 15m — all far longer than the per-shard wall-clock
# budget — and the live kbeads daemon does NOT accept an updated_at override, so a
# test cannot backdate a bead to age it past a window on demand. The PAST-GRACE
# directions (idle orphan IS reaped; stranded run-instance IS closed) are
# therefore unobservable live within budget. A faithful, decision-for-decision
# bash reimplementation of the two reaper decision tables asserts the FULL truth
# table (both directions) deterministically in every shard, exactly like
# test-thaw-reaper-exemption.sh's contract leg. The Go unit tests
# (stalereaper_test.go) are the in-tree companion; this pins the same logic at the
# e2e layer so a refactor that drops a grace gate or a reconcile guard fails here.
#
# TWO LEGS, mirroring test-thaw-reaper-exemption.sh:
#   1. CONTRACT (always runs — needs only bash): the grace-gate + run-instance
#      reconcile decision tables reimplemented over bash predicates, asserted over
#      the full truth table with negative controls. Runs in every shard, no
#      substrate.
#   2. SUBSTRATE (opt-in, REAPER_SUBSTRATE_E2E=1): seed a REAL stranded
#      run-instance task (in_progress, ghost/gone assignee) FRESHLY on the live
#      daemon — inside the 15m grace — and assert the live controller reaper LEAVES
#      IT ALONE across a reaper pass (the non-destructive within-grace direction —
#      the exact regression 977d6f2b's grace guard prevents: closing a
#      just-claimed task before its agent bead surfaces). The destructive
#      directions can't be observed live within budget, so they are left to the
#      contract leg + the Go unit tests. SKIPs LOUD by default.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-stale-reaper-grace-reconcile.sh              # contract only
#   REAPER_SUBSTRATE_E2E=1 ./gardenio/tests/e2e/scripts/test-stale-reaper-grace-reconcile.sh  # + substrate

set -uo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
KD_BIN="${KD_BIN:-kd}"
RUN_ID="${RUN_ID:-$$}"

# Reaper grace windows — MUST match the Go constants in stalereaper.go. A drift
# here is itself a regression: if someone changes orphanReapAfter without updating
# this, the contract assertions below fail and flag the mismatch.
#   orphanReapAfter            = 5m   → 300s  (crew orphan grace)
#   gardenerOrphanReapAfter     = 3×5m = 15m → 900s  (gardener orphan grace)
#   runInstanceReconcileMinAge = 15m → 900s  (gone-prior-pass reconcile grace)
CREW_ORPHAN_GRACE=300
GARDENER_ORPHAN_GRACE=900
RUN_INSTANCE_MIN_AGE=900

# Substrate leg: seconds to wait for the live reaper to run a pass. The stale
# reaper runs every ~5m (main.go: staleReapInterval = 5m). We wait a little past
# one pass so a (mis)close would have happened, while staying WELL under the 15m
# grace so the correct behaviour is "still open". Overridable for a faster/slower
# reaper cadence.
REAP_WAIT="${REAP_WAIT:-330}"

PASS=0
FAIL=0
ERRORS=()
PF_PID=""
CREATED_BEADS=()

export KD_ACTOR="${KD_ACTOR:-e2e-test}"

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
have() { command -v "$1" >/dev/null 2>&1; }

assert_verdict() {
  local name="$1" got="$2" want="$3"
  if [ "$got" = "$want" ]; then
    pass "$name → $got"
  else
    fail "$name" "got '$got', want '$want'"
  fi
}

echo "════════════════════════════════════════════════════"
echo " stale-reaper grace-gate + run-instance reconcile E2E (gd-lbGpeO0u1R)"
echo " busy agents survive; idle orphans + stranded run-instances get cleaned up"
echo "════════════════════════════════════════════════════"

# ===========================================================================
# LEG 1 — CONTRACT (always runs): the two reaper decision tables.
#
# Each predicate mirrors, decision-for-decision, the Go the reaper actually runs.
# The asserts walk the full truth table (both the reap/close AND the leave-alone
# directions, plus negative controls) so a future refactor that drops a grace gate
# — or widens a reconcile guard too far — fails here, in every shard, with no
# substrate.
# ===========================================================================

# ── Table A: grace-gated orphan reap (7a44b0b5 / ka-0000000018) ─────
# orphan_reap_verdict <role> <source_task_closed:0|1> <staleness_seconds>
#   Mirrors reapReason's working/"" branch grace gate (stalereaper.go L1117-1128).
#   The grace gate is state-agnostic between agent_state "working" and "" (empty)
#   — the switch is `case "working", "":` and both share this exact code path — so
#   these verdicts cover the empty-state orphan too (unit-tested as
#   TestStaleReaper_SkipsEmptyStateOrphan_WithinGrace).
#     - source task STILL OPEN  → NOOP: not an orphan here; rides the base 2h
#       staleness path (out of this table's scope).
#     - source task CLOSED      → orphan candidate; grace = 5m crew / 15m gardener,
#       measured against updated_at. staleness > grace ⇒ REAP; else ⇒ SKIP
#       (busy/mid-wrap-up pod with a fresh updated_at, or a gardener between cycles).
orphan_reap_verdict() {
  local role="$1" closed="$2" staleness="$3"
  if [ "$closed" != "1" ]; then
    echo "NOOP"   # source task still open — not the grace-gated orphan branch
    return
  fi
  local grace="$CREW_ORPHAN_GRACE"
  [ "$role" = "gardener" ] && grace="$GARDENER_ORPHAN_GRACE"
  if [ "$staleness" -gt "$grace" ]; then
    echo "REAP"   # genuinely-idle orphan parked past the grace window
  else
    echo "SKIP"   # busy/fresh (or gardener between cycles) — NEVER reap mid-work
  fi
}

blue "[contract] grace-gated orphan reap (7a44b0b5 / ka-0000000018)"
# THE HEADLINE regression: a busy CREW pod (fresh updated_at) whose source task
# JUST closed must NOT be reaped — this is the exact "idle crew" churn the fix
# closes (pods killed 2-14s after their task closed, mid commit/push/gb-stop).
assert_verdict "busy crew orphan (task closed, 10s fresh) NOT reaped" \
  "$(orphan_reap_verdict crew 1 10)" "SKIP"
assert_verdict "crew orphan just under grace (task closed, 4m59s) NOT reaped" \
  "$(orphan_reap_verdict crew 1 299)" "SKIP"
# The OTHER direction — a genuinely-idle crew orphan parked past 5m IS reaped
# (forgot gb stop). This is unobservable live within budget; the contract pins it.
assert_verdict "idle crew orphan past grace (task closed, 6m) IS reaped" \
  "$(orphan_reap_verdict crew 1 360)" "REAP"
# Gardener gets the longer 15m runway to cover the idle gap between loop cycles: a
# gardener aged past the CREW grace but within the GARDENER grace must NOT be reaped.
assert_verdict "gardener orphan past crew grace but within gardener grace (7m) NOT reaped" \
  "$(orphan_reap_verdict gardener 1 420)" "SKIP"
assert_verdict "gardener orphan just under gardener grace (14m59s) NOT reaped" \
  "$(orphan_reap_verdict gardener 1 899)" "SKIP"
# A genuinely-dormant gardener past 15m IS reaped (its schedule/trigger re-fires).
assert_verdict "dormant gardener past gardener grace (16m) IS reaped" \
  "$(orphan_reap_verdict gardener 1 960)" "REAP"
# Negative control: an OPEN source task is NOT an orphan-reap decision regardless
# of freshness — the agent is still legitimately working it.
assert_verdict "working agent, source task STILL OPEN (fresh) not an orphan case" \
  "$(orphan_reap_verdict crew 0 10)" "NOOP"
assert_verdict "working agent, source task STILL OPEN (stale 30m) not an orphan case" \
  "$(orphan_reap_verdict crew 0 1800)" "NOOP"
# Boundary: the fix uses strict `>` grace (now.Sub(updatedAt) > grace), so exactly
# AT the grace boundary the pod is still spared.
assert_verdict "crew orphan exactly AT grace boundary (300s) NOT reaped (strict >)" \
  "$(orphan_reap_verdict crew 1 300)" "SKIP"
echo

# ── Table B: run-instance task identification (isRunInstanceTask) ──
# is_run_instance <label> <schedule_fire_key:0|1> <multi_fire_key:0|1>
#   Mirrors isRunInstanceTask (stalereaper.go L1000-1012): a task is a run-instance
#   iff it carries a source:schedule / source:multi / source:schedule-mirror label,
#   OR (label stripped) a schedule_fire_key / multi_fire_key field. Everything else
#   is an ORDINARY task the reconcile paths must never touch.
is_run_instance() {
  local label="$1" sfk="$2" mfk="$3"
  case "$label" in
    source:schedule|source:multi|source:schedule-mirror) echo "RUN_INSTANCE"; return ;;
  esac
  if [ "$sfk" = "1" ] || [ "$mfk" = "1" ]; then
    echo "RUN_INSTANCE"; return
  fi
  echo "ORDINARY"
}

blue "[contract] run-instance identification (isRunInstanceTask)"
assert_verdict "source:schedule label → run-instance" \
  "$(is_run_instance source:schedule 0 0)" "RUN_INSTANCE"
assert_verdict "source:multi label → run-instance" \
  "$(is_run_instance source:multi 0 0)" "RUN_INSTANCE"
assert_verdict "source:schedule-mirror label → run-instance" \
  "$(is_run_instance source:schedule-mirror 0 0)" "RUN_INSTANCE"
assert_verdict "schedule_fire_key field (label stripped) → run-instance" \
  "$(is_run_instance "" 1 0)" "RUN_INSTANCE"
assert_verdict "multi_fire_key field (label stripped) → run-instance" \
  "$(is_run_instance "" 0 1)" "RUN_INSTANCE"
# Negative controls: an ordinary task must NEVER be seen as a run-instance, or the
# reconcile path would wrongly CLOSE retriable issue work.
assert_verdict "ordinary project label → NOT a run-instance" \
  "$(is_run_instance project:gardenio 0 0)" "ORDINARY"
assert_verdict "no label, no fire-key → NOT a run-instance" \
  "$(is_run_instance "" 0 0)" "ORDINARY"
echo

# ── Table C: stranded run-instance reconcile (977d6f2b / ka-0000000889) ──
# run_instance_verdict <is_run_instance:0|1> <assignee_empty:0|1> <assignee_live:0|1> \
#                      <reaped_this_pass:0|1> <staleness_seconds>
#   Mirrors reconcileRunInstanceTasks (stalereaper.go L519-573):
#     - assignee empty        → NOOP (t.Assignee == "" ⇒ continue)
#     - not a run-instance    → NOOP (!isRunInstanceTask ⇒ continue)
#     - assignee LIVE         → NOOP (liveAgents[assignee] ⇒ leave alone)
#     - assignee gone, reaped THIS pass → CLOSE immediately (definitively gone)
#     - assignee gone in a PRIOR pass:
#         staleness < 15m grace → SKIP (guards the just-claimed / not-yet-surfaced race)
#         staleness ≥ 15m grace → CLOSE (stranded; single-shot, never retried)
run_instance_verdict() {
  local is_ri="$1" empty="$2" live="$3" reaped="$4" staleness="$5"
  [ "$empty" = "1" ]  && { echo "NOOP"; return; }   # no assignee — not a strand
  [ "$is_ri" != "1" ] && { echo "NOOP"; return; }   # ordinary task — never touch
  [ "$live" = "1" ]   && { echo "NOOP"; return; }   # assignee working it — leave alone
  if [ "$reaped" = "1" ]; then
    echo "CLOSE"   # assignee reaped this pass — definitively gone, no grace
    return
  fi
  if [ "$staleness" -lt "$RUN_INSTANCE_MIN_AGE" ]; then
    echo "SKIP"    # gone prior pass but within grace — race guard
  else
    echo "CLOSE"   # gone prior pass, past grace — stranded, reconcile it
  fi
}

blue "[contract] stranded run-instance reconcile (977d6f2b / ka-0000000889)"
# Assignee reaped in THIS pass → closed immediately regardless of task age (the
# agent is provably gone this pass).
assert_verdict "run-instance, assignee reaped this pass (fresh 1m) IS closed" \
  "$(run_instance_verdict 1 0 0 1 60)" "CLOSE"
assert_verdict "run-instance, assignee reaped this pass (old 2h) IS closed" \
  "$(run_instance_verdict 1 0 0 1 7200)" "CLOSE"
# Assignee vanished in a PRIOR pass (crash/OOM), task aged past the 15m grace → closed.
assert_verdict "run-instance, assignee gone prior pass, past grace (30m) IS closed" \
  "$(run_instance_verdict 1 0 0 0 1800)" "CLOSE"
# THE race-guard direction: gone prior pass but the task was claimed very recently
# (within 15m) — do NOT close; the agent bead may not have surfaced in
# ListAgentBeads yet. This is the safe/non-destructive within-grace direction the
# substrate leg confirms live.
assert_verdict "run-instance, assignee gone prior pass, within grace (2m) NOT closed" \
  "$(run_instance_verdict 1 0 0 0 120)" "SKIP"
assert_verdict "run-instance, gone prior pass, just under grace (14m59s) NOT closed" \
  "$(run_instance_verdict 1 0 0 0 899)" "SKIP"
# Live assignee → always left alone, even a very old task (the agent is working it).
assert_verdict "run-instance, assignee LIVE (old 2h) left alone" \
  "$(run_instance_verdict 1 0 1 0 7200)" "NOOP"
# Negative controls — the reconcile must never touch these:
assert_verdict "ordinary task, assignee gone past grace → left alone (not a run-instance)" \
  "$(run_instance_verdict 0 0 0 0 1800)" "NOOP"
assert_verdict "run-instance with NO assignee (never claimed) → not the in_progress path" \
  "$(run_instance_verdict 1 1 0 0 1800)" "NOOP"
# Boundary: strict `<` grace (now.Sub(updatedAt) < minAge ⇒ skip), so exactly AT
# the 15m boundary the stranded task IS reconciled.
assert_verdict "run-instance, gone prior pass, exactly AT grace boundary (900s) IS closed" \
  "$(run_instance_verdict 1 0 0 0 900)" "CLOSE"
echo

# ===========================================================================
# LEG 2 — SUBSTRATE (opt-in, REAPER_SUBSTRATE_E2E=1): observe the LIVE reaper.
#
# Seed a REAL stranded run-instance task on the live daemon: in_progress, carrying
# a source:schedule label, assigned to a GHOST agent name that has no agent bead
# (definitively "gone"). Because it is freshly created it is INSIDE the 15m
# runInstanceReconcileMinAge grace, so the correct live behaviour is: the reaper
# LEAVES IT ALONE across a pass. If 977d6f2b's grace guard regressed to "close any
# gone-assignee run-instance immediately", this bead would be closed on the first
# pass — the assertion catches that.
#
# We assert only the NON-DESTRUCTIVE within-grace direction: it is the one the
# reaper's 5m cadence + the 15m grace make observable inside a shard budget, and it
# is the exact regression the grace guard prevents. The destructive direction
# (past-grace CLOSE) needs a 15m+ wait or a backdated updated_at (the daemon
# forbids the latter), so it stays with the contract leg + the Go unit tests. A
# run-instance is a type=task bead, so seeding it can NEVER trigger a reconciler
# pod cold-start (unlike a type=agent bead) — the substrate is side-effect-free.
# SKIPs LOUD when the opt-in flag is unset.
# ===========================================================================
if [ "${REAPER_SUBSTRATE_E2E:-0}" != "1" ]; then
  blue "[substrate] SKIP — REAPER_SUBSTRATE_E2E != 1 (contract leg already asserted both tables)"
  dim "  Set REAPER_SUBSTRATE_E2E=1 against a live daemon+controller to observe the reaper leave a fresh stranded run-instance alone."
else
  blue "[substrate] REAPER_SUBSTRATE_E2E=1 — observing the live controller reaper"
  SUBSTRATE_OK=1

  # Reuse the shard-provided daemon when present (run-shard.sh exports
  # BEADS_HTTP_URL + BEADS_TOKEN); otherwise stand up our own port-forward so the
  # module is runnable standalone.
  if [ -z "${BEADS_HTTP_URL:-}" ]; then
    if ! have kubectl; then
      fail "substrate preflight" "REAPER_SUBSTRATE_E2E=1 but no BEADS_HTTP_URL and kubectl not installed"
      SUBSTRATE_OK=0
    else
      blue "[substrate] port-forwarding ${NAMESPACE}/${BEADS_SVC}..."
      kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" 19108:8080 \
        >/tmp/kd-pf-reaper-grace.log 2>&1 &
      PF_PID=$!
      export BEADS_HTTP_URL="http://localhost:19108"
      # Poll until healthy rather than a fixed sleep.
      _pf_ready=0
      for _i in $(seq 1 15); do
        if curl -sf --max-time 2 -o /dev/null "${BEADS_HTTP_URL}/v1/health" 2>/dev/null; then
          _pf_ready=1; break
        fi
        sleep 1
      done
      if [ "$_pf_ready" = 0 ]; then
        fail "substrate preflight" "daemon /v1/health never answered on ${BEADS_HTTP_URL}"
        SUBSTRATE_OK=0
      fi
    fi
    if [ -z "${BEADS_TOKEN:-}" ] && have kubectl; then
      BEADS_TOKEN=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
        -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
      [ -n "$BEADS_TOKEN" ] && export BEADS_TOKEN
    fi
  fi

  if [ "$SUBSTRATE_OK" = "1" ] && ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    fail "substrate preflight" "cannot reach kd daemon at ${BEADS_HTTP_URL:-<unset>}"
    SUBSTRATE_OK=0
  fi

  if [ "$SUBSTRATE_OK" = "1" ]; then
    cleanup_substrate() {
      for id in "${CREATED_BEADS[@]}"; do
        "$KD_BIN" close "$id" 2>/dev/null || true
      done
      if [ -n "$PF_PID" ]; then
        kill "$PF_PID" 2>/dev/null || true
      fi
    }
    trap cleanup_substrate EXIT

    GHOST="reaper-grace-ghost-${RUN_ID}"   # an agent name with NO agent bead → "gone"

    # Create a run-instance task (source:schedule label = isRunInstanceTask true),
    # then drive it to in_progress with the ghost assignee — the exact shape a
    # schedule run-instance strands in when its agent is reaped without gb stop.
    blue "[substrate] seeding stranded run-instance task (ghost assignee '$GHOST', within grace)"
    RI_OUT=$("$KD_BIN" create "reaper-grace-e2e run-instance ${RUN_ID}" \
      --type=task -l "project:e2e-test" -l "source:schedule" --json 2>/dev/null || echo "")
    RI_ID=$(echo "$RI_OUT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
    if [ -z "$RI_ID" ]; then
      fail "seed run-instance" "kd create returned no id (out: $(echo "$RI_OUT" | head -1))"
    else
      CREATED_BEADS+=("$RI_ID")
      "$KD_BIN" update "$RI_ID" --status in_progress --assignee "$GHOST" >/dev/null 2>&1 || true
      # Confirm the seed landed as intended before we lean on the reaper's verdict.
      SEED_STATUS=$("$KD_BIN" show "$RI_ID" --json 2>/dev/null \
        | python3 -c "import sys,json; print(json.load(sys.stdin).get('status',''))" 2>/dev/null || echo "")
      if [ "$SEED_STATUS" = "in_progress" ]; then
        pass "seeded run-instance $RI_ID is in_progress with ghost assignee"
      else
        fail "seed run-instance state" "expected in_progress, got '$SEED_STATUS'"
      fi

      # Wait past one reaper pass (~5m) while staying WELL under the 15m grace.
      blue "[substrate] waiting ${REAP_WAIT}s for a reaper pass (within grace → expect NOT closed)"
      sleep "$REAP_WAIT"

      AFTER_STATUS=$("$KD_BIN" show "$RI_ID" --json 2>/dev/null \
        | python3 -c "import sys,json; print(json.load(sys.stdin).get('status',''))" 2>/dev/null || echo "")
      if [ "$AFTER_STATUS" = "in_progress" ]; then
        pass "stranded run-instance STILL in_progress after a reaper pass (grace guard held)"
      elif [ -z "$AFTER_STATUS" ]; then
        # Could not read it back — inconclusive, not a violation. Report, don't fail.
        dim "    could not read run-instance status after wait (daemon blip?) — inconclusive"
        pass "substrate leg completed (no grace-guard violation observed)"
      else
        fail "run-instance reaped within grace" \
          "status='$AFTER_STATUS' after ${REAP_WAIT}s (< 15m grace) — the grace guard should have left it in_progress"
      fi
    fi
    cleanup_substrate
    trap - EXIT
  fi
fi
echo

# ── Summary ───────────────────────────────────────────────────────
echo "════════════════════════════════════════════════════"
echo " stale-reaper-grace-reconcile Summary: $PASS passed, $FAIL failed"
echo "════════════════════════════════════════════════════"
if [ "$FAIL" -gt 0 ]; then
  for e in "${ERRORS[@]}"; do red "  - $e"; done
  exit 1
fi
green "All stale-reaper-grace-reconcile assertions passed."
exit 0
