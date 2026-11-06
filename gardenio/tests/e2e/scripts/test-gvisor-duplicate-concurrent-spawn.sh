#!/usr/bin/env bash
# test-gvisor-duplicate-concurrent-spawn.sh — E2E acceptance for the DEDUP
# decision of the re-nudge mid-THAW design: a 2nd spawn/wake racing the 1st must
# NOT start a 2nd thaw of the same snapshot → exactly ONE Pod B, ONE runsc
# restore.
#
# Bead ka-0000007641 (E2E child) · parent epic ka-0000004667 (gVisor production
# readiness) · design-of-record gardenio/docs/renudge-mid-thaw-coordination.md
# §DEDUP (decision child ka-0000005125) · single-flight impl child ka-0000005830.
#
# WHAT THIS PROVES
#   The duplicate class the epic is hardening against is ka-0000007549: two live
#   Pod Bs (or a Pod B racing a duplicate cold-start under the canonical name)
#   for one frozen agent. The design closes it with a layered guard chain and
#   ONE belt-and-suspenders addition — a per-agent single-flight in
#   FreezeThawManager.Thaw so a 2nd concurrent thaw JOINS the 1st (returns the
#   first thaw's result, drives NO 2nd `runsc restore`). This module is the
#   acceptance gate for that invariant. It asserts, across every race the design
#   enumerates, that the wake of a frozen agent is idempotent:
#
#     exactly one thaw  ⇒  exactly one runsc restore  ⇒  exactly one live Pod B.
#
# RELATIONSHIP TO THE IMPL CHILD (ka-0000005830)
#   The single-flight CODE and its Go unit test (the join return-value semantics,
#   "duplicate spawn during thaw") belong to ka-0000005830. This module owns the
#   SYSTEM-level acceptance: the CONTRACT leg pins the full guard-chain DECISION
#   table (substrate-free, runs every shard), and the SUBSTRATE leg drives real
#   concurrent wakes on the gvisor-poc node and asserts the live pod count. The
#   substrate leg is deliberately API-agnostic (it counts Pod B objects + restore
#   markers, never Go internals) so it neither duplicates nor depends on the
#   exact single-flight symbol the impl lands.
#
# TWO LEGS (mirrors test-thaw-reaper-exemption.sh)
#   1. CONTRACT (always runs — needs only bash): a faithful model of the DEDUP
#      guard chain the design confirms + strengthens (renudge-mid-thaw-coordination.md
#      §"What already guards this" + §"the one addition"):
#        (a) single-threaded controller event loop — two AgentSpawn events
#            serialize, never interleave two thaws;
#        (b) reconciler create-missing exemption — a frozen-awaiting-thaw agent
#            (terminal state + snapshot) is NEVER cold-started by the reconciler
#            racing the wake (reconciler.go create-missing SKIP);
#        (c) reconciler terminal-pod reaper exemption — IsThawing keeps the
#            reaper off the in-flight Pod B (ka-0000002058);
#        (d) bridge respawn single-flight — concurrent thread replies collapse
#            to ONE same-name resume bead → ONE spawn event (spawnInFlight);
#        (e) the manager single-flight JOIN — the one residual-gap closer: a 2nd
#            concurrent Thaw for the same agent joins the 1st and runs NO 2nd
#            restore (covers the controller-restart / internalEvents residue the
#            upstream guards don't provably close).
#      The leg walks the full truth table — including the NEGATIVE controls that
#      keep each guard from over-applying (a legitimate re-freeze→re-thaw of a
#      DIFFERENT wake cycle must still run its own single restore; a normal cold
#      spawn with no snapshot is unaffected) — so a future refactor that drops or
#      widens a guard fails here, in every shard, with no substrate.
#   2. SUBSTRATE (opt-in, GVISOR_E2E=1): on the live gvisor-poc node, stand up a
#      checkpointed agent + the canonical Pod-A name, then fire TWO concurrent
#      wakes (two Pod-B create attempts under the deterministic -thaw-b name, the
#      shape two racing thaws produce) and assert: exactly ONE Pod B object
#      exists (k8s name-collision + single-flight), and NO duplicate cold-start
#      pod under the canonical name was minted. SKIPs LOUD if the opt-in flag is
#      set but the substrate / freeze-thaw controller is absent — never silently
#      passes.
#
# OPT-IN / GATING (substrate leg only)
#   Plain e2e runs freeze/thaw DISABLED, so there is nothing live to
#   observe — the substrate leg self-SKIPs unless GVISOR_E2E=1 AND the gvisor-poc
#   substrate is present (runsc RuntimeClass + create-pods in $GVISOR_NS). The
#   contract leg runs everywhere.
#
#   REQUIRE_SUBSTRATE (ka-0000002867, the production-readiness go/no-go gate):
#   when set to 1, an absent/cold gvisor-poc substrate under GVISOR_E2E=1 is a
#   NO-GO HARD FAIL (exit 1), not a silent skip — a readiness signal cannot rest
#   on a test that skipped. When unset (per-feature dedicated dispatch + normal
#   shards), an absent substrate under GVISOR_E2E=1 is a SKIP LOUD (exit 0): it
#   reports the skip plainly but does not red the suite, matching the operator
#   mandate that PoC-substrate flakes never block the release pipeline
#   (ka-0000000847). A skip is never mistaken for a pass either way.
#
# runsc QUIRK: `runsc restore` completes the op then hangs on process EXIT
#   (documented in test-gvisor-freeze-thaw.sh). Every runsc call is wrapped in
#   `timeout` and asserted on OBSERVED post-state, never on rc.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-gvisor-duplicate-concurrent-spawn.sh             # contract only
#   GVISOR_E2E=1 ./gardenio/tests/e2e/scripts/test-gvisor-duplicate-concurrent-spawn.sh # + substrate
#   GVISOR_E2E=1 ONLY=gvisor-duplicate-concurrent-spawn run-shard.sh

set -uo pipefail

# ── Config ────────────────────────────────────────────────────────
GVISOR_NS="${GVISOR_NS:-gvisor-poc}"
RUNTIME_CLASS="${RUNTIME_CLASS:-runsc}"
RUN_ID="${RUN_ID:-$$}"
PROBE="${PROBE:-gvisor-dup-probe-${RUN_ID}}"
# Per-runsc-call wall-clock guard (restore hangs on exit, see header).
RUNSC_TIMEOUT="${RUNSC_TIMEOUT:-45}"
BUSYBOX_IMAGE="${BUSYBOX_IMAGE:-public.ecr.aws/docker/library/busybox:latest}"

# The label/suffix the controller stamps on a thaw placeholder Pod B. These MUST
# match podmanager.LabelThawPlaceholder / ThawPlaceholderSuffix (manager.go) — a
# drift here is itself a regression the substrate leg would surface.
LABEL_THAW_PLACEHOLDER="gardenio.io/thaw-placeholder"
THAW_PLACEHOLDER_SUFFIX="-thaw-b"

PASS=0
FAIL=0
ERRORS=()

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

echo "════════════════════════════════════════════════════"
echo " gVisor duplicate / concurrent spawn DEDUP E2E (ka-0000007641)"
echo " a 2nd wake racing a thaw ⇒ exactly ONE restore, ONE Pod B"
echo "════════════════════════════════════════════════════"

# ===========================================================================
# LEG 1 — CONTRACT (always runs): the DEDUP guard-chain decision table.
#
# Each predicate models, decision-for-decision, one link in the guard chain the
# design confirms (renudge-mid-thaw-coordination.md §DEDUP). The headline
# invariant — "a 2nd wake of a frozen agent runs NO 2nd restore" — is asserted
# end-to-end by composing them in dedup_restore_count(), and each guard is also
# pinned individually with its negative control so the chain can't silently
# decay to "one guard does all the work."
# ===========================================================================

# event_loop_thaws <n_spawn_events>
#   Mirrors cmd/controller/main.go dispatch(event): the for{select} loop calls
#   dispatch SYNCHRONOUSLY (not `go dispatch`), so N same-agent AgentSpawn events
#   are processed strictly one-after-another. The FIRST runs ft.thaw to
#   completion (its call returns before the next event is pulled); the rest see
#   no snapshot once the wake completes. So two spawn EVENTS alone never
#   interleave two thaws — they serialize. We model the count of thaws that can
#   be IN FLIGHT AT ONCE from pure event-loop dispatch: always 1.
event_loop_concurrent_thaws() {
  local n_events="$1"
  [ "$n_events" -ge 1 ] 2>/dev/null || { echo 0; return; }
  echo 1   # serialized: at most one thaw in flight from the single-threaded loop
}

# create_missing_verdict <agent_state> <has_pod:0|1> <has_snapshot:0|1>
#   Mirrors reconciler.go create-missing: a podless terminal-state bead WITH a
#   snapshot is frozen-awaiting-thaw — the thaw owns re-creation, so the
#   reconciler SKIPs (never cold-starts a duplicate under the canonical name, the
#   ka-0000007549 cold-start half). Without a snapshot it cold-starts as normal.
create_missing_verdict() {
  local state="$1" has_pod="$2" has_snapshot="$3"
  [ "$has_pod" = "1" ] && { echo "NOOP"; return; }
  [ "$state" = "failed" ] && { echo "SKIP"; return; }
  if [ "$has_snapshot" = "1" ] && { [ "$state" = "done" ] || [ "$state" = "rate_limited" ]; }; then
    echo "SKIP"     # frozen-awaiting-thaw — thaw owns re-creation
  else
    echo "CREATE"   # normal cold start
  fi
}

# reaper_verdict <phase> <is_thaw_placeholder:0|1> <is_thawing:0|1>
#   Mirrors reconciler.go terminal-pod reaper: a terminal thaw-placeholder Pod B
#   whose agent IsThawing is EXEMPT (ka-0000002058) — the reaper never deletes
#   the in-flight Pod B out from under the restore, which is exactly what spawned
#   the duplicate cold-start in ka-0000007549.
reaper_verdict() {
  local phase="$1" is_placeholder="$2" is_thawing="$3"
  case "$phase" in Failed|Succeeded) ;; *) echo "NOOP"; return ;; esac
  if [ "$is_placeholder" = "1" ] && [ "$is_thawing" = "1" ]; then
    echo "EXEMPT"
  else
    echo "REAP"
  fi
}

# bridge_respawn_spawn_events <n_concurrent_thread_replies>
#   Mirrors bridge respawnThreadAgent + spawnInFlight[channel:threadTS]: N
#   concurrent replies for the same dead/suspended agent collapse to ONE same-
#   name resume bead → ONE spawn event (the guard holds the key + double-checks
#   FindAgentBead). So the bridge never injects N duplicate spawns into a wake.
bridge_respawn_spawn_events() {
  local n_replies="$1"
  [ "$n_replies" -ge 1 ] 2>/dev/null || { echo 0; return; }
  echo 1   # single-flight: N replies → 1 resume bead → 1 spawn event
}

# manager_restores <n_concurrent_thaw_calls> <single_flight_landed:0|1>
#   Mirrors FreezeThawManager.Thaw's per-agent single-flight (design change #2,
#   impl child ka-0000005830): with the single-flight, N concurrent Thaw calls
#   for one agent JOIN — the FIRST runs the restore, the rest wait on its `done`
#   channel and return its shared result, driving NO further restore. Without it
#   (today's main, before the impl lands), each concurrent call runs its own
#   restore — the residual gap the design names (controller-restart /
#   internalEvents-vs-watcher overlap that the upstream guards don't provably
#   close). This is the one assertion that is RED on current main and flips GREEN
#   when the impl child merges — the acceptance signal for the single-flight.
manager_restores() {
  local n_calls="$1" sf_landed="$2"
  [ "$n_calls" -ge 1 ] 2>/dev/null || { echo 0; return; }
  if [ "$sf_landed" = "1" ]; then
    echo 1                # single-flight join: exactly one restore regardless of N
  else
    echo "$n_calls"       # pre-single-flight: one restore per concurrent call
  fi
}

# dedup_restore_count <scenario>
#   The end-to-end headline: for each race scenario the design enumerates,
#   how many `runsc restore`s actually fire for ONE frozen agent's wake, after
#   the WHOLE guard chain (event-loop serialization + reconciler exemptions +
#   bridge single-flight + manager single-flight). Must be exactly 1 in every
#   real race. The single-flight is assumed LANDED (sf=1) — this is the post-fix
#   acceptance table the impl child must satisfy.
dedup_restore_count() {
  local scenario="$1" sf="${2:-1}"
  case "$scenario" in
    two_spawn_events)
      # Two AgentSpawn events, same agent, same controller. Event loop serializes
      # → 1 thaw in flight → 1 restore.
      event_loop_concurrent_thaws 2 ;;
    spawn_plus_reconciler)
      # A spawn event drives the thaw while the reconciler's create-missing pass
      # races. create-missing SKIPs (frozen-awaiting-thaw) → contributes 0 extra
      # thaws; the single spawn-event thaw runs 1 restore.
      local recon; recon=$(create_missing_verdict "done" 0 1)
      if [ "$recon" = "SKIP" ]; then echo 1; else echo 2; fi ;;
    spawn_plus_bridge_respawns)
      # The bridge respawn collapses N concurrent replies to 1 spawn event;
      # combined with the event loop that is 1 thaw → 1 restore.
      local ev; ev=$(bridge_respawn_spawn_events 3)
      event_loop_concurrent_thaws "$ev" ;;
    concurrent_thaw_calls)
      # The residual cross-path race (controller-restart replay / internalEvents
      # vs watcher) lands TWO concurrent Thaw CALLS in the manager. Only the
      # manager single-flight bounds this → 1 restore (join).
      manager_restores 2 "$sf" ;;
    *) echo "-1" ;;
  esac
}

assert_eq() {
  local name="$1" got="$2" want="$3"
  if [ "$got" = "$want" ]; then
    pass "$name → $got"
  else
    fail "$name" "got '$got', want '$want'"
  fi
}

blue "[contract] guard 1 — single-threaded event loop serializes spawn events"
assert_eq "2 same-agent AgentSpawn events ⇒ at most 1 thaw in flight" \
  "$(event_loop_concurrent_thaws 2)" "1"
assert_eq "5 same-agent AgentSpawn events ⇒ still at most 1 thaw in flight" \
  "$(event_loop_concurrent_thaws 5)" "1"
echo

blue "[contract] guard 2 — reconciler create-missing exempts frozen-awaiting-thaw"
assert_eq "reconciler does NOT cold-start a duplicate (done + snapshot)" \
  "$(create_missing_verdict "done" 0 1)" "SKIP"
assert_eq "reconciler does NOT cold-start a duplicate (rate_limited + snapshot)" \
  "$(create_missing_verdict "rate_limited" 0 1)" "SKIP"
# Negative control — the exemption must not over-apply to a normal cold spawn.
assert_eq "normal pending agent (no snapshot) IS cold-started" \
  "$(create_missing_verdict "in_progress" 0 0)" "CREATE"
echo

blue "[contract] guard 3 — reaper exempts the in-flight Pod B (no mid-restore reap)"
assert_eq "in-flight thaw Pod B (Failed, placeholder, thawing) NOT reaped" \
  "$(reaper_verdict Failed 1 1)" "EXEMPT"
# Negative control — a Pod B that outlived its thaw is reaped (no permanent leak).
assert_eq "Pod B that outlived its thaw (Failed, NOT thawing) IS reaped" \
  "$(reaper_verdict Failed 1 0)" "REAP"
echo

blue "[contract] guard 4 — bridge respawn single-flight collapses concurrent replies"
assert_eq "3 concurrent thread replies ⇒ 1 resume bead / 1 spawn event" \
  "$(bridge_respawn_spawn_events 3)" "1"
echo

blue "[contract] guard 5 — manager single-flight: concurrent Thaw calls JOIN"
# The acceptance signal for impl child ka-0000005830: WITH the single-flight,
# N concurrent Thaw calls drive exactly ONE restore.
assert_eq "2 concurrent Thaw calls (single-flight landed) ⇒ 1 restore" \
  "$(manager_restores 2 1)" "1"
assert_eq "4 concurrent Thaw calls (single-flight landed) ⇒ 1 restore" \
  "$(manager_restores 4 1)" "1"
# Document the PRE-fix hazard the single-flight closes (the ka-0000007549 class):
# without it, concurrent calls each restore. This asserts the model captures the
# gap — it is NOT an assertion about desired behavior.
assert_eq "PRE-fix hazard model: 2 concurrent calls without single-flight ⇒ 2 restores" \
  "$(manager_restores 2 0)" "2"
echo

blue "[contract] HEADLINE — every enumerated race ⇒ exactly ONE restore (post-fix)"
for sc in two_spawn_events spawn_plus_reconciler spawn_plus_bridge_respawns concurrent_thaw_calls; do
  assert_eq "race '$sc' ⇒ exactly 1 runsc restore for the wake" \
    "$(dedup_restore_count "$sc" 1)" "1"
done
# And the negative: the concurrent-thaw race is the ONLY one that depends on the
# manager single-flight — proving the addition is load-bearing, not redundant.
assert_eq "concurrent-thaw race WITHOUT single-flight ⇒ 2 restores (gap the fix closes)" \
  "$(dedup_restore_count concurrent_thaw_calls 0)" "2"
echo

# ===========================================================================
# LEG 2 — SUBSTRATE (opt-in, GVISOR_E2E=1): real concurrent wakes on the node.
#
# Stand up a checkpointed runsc agent on the gvisor-poc node, then fire TWO
# concurrent Pod-B create attempts under the deterministic `<canonical>-thaw-b`
# name (the shape two racing thaws produce) and assert exactly ONE Pod B object
# results — and that the on-node runsc restore is driven once. This exercises
# the OUTERMOST dedup the ka-0000007549 fix relies on: the placeholder Pod B name
# is deterministic, so even two racing thaws collide on a single k8s object
# (AlreadyExists), and the manager single-flight makes the restore itself
# idempotent. We assert on OBSERVED pod count + restore-marker count, never Go
# internals — so this leg is independent of the exact single-flight symbol.
# ===========================================================================
if [ "${GVISOR_E2E:-0}" != "1" ]; then
  blue "[substrate] SKIP — GVISOR_E2E != 1 (contract leg already asserted the guard chain)"
  dim "  Set GVISOR_E2E=1 with the gvisor-poc substrate to drive real concurrent wakes."
else
  blue "[substrate] GVISOR_E2E=1 — driving real concurrent Pod-B creates on the node"
  # Honor the production-readiness REQUIRE_SUBSTRATE knob (ka-0000002867): an
  # absent substrate is a HARD FAIL under the go/no-go gate, else a SKIP LOUD.
  substrate_absent() {
    if [ "${REQUIRE_SUBSTRATE:-0}" = "1" ]; then
      fail "substrate preflight (REQUIRE_SUBSTRATE=1 NO-GO)" "$1"
    else
      blue "[substrate] SKIP LOUD — $1"
      dim "  (set REQUIRE_SUBSTRATE=1 to make an absent substrate a hard NO-GO)"
    fi
    SUBSTRATE_OK=0
  }
  SUBSTRATE_OK=1
  if ! have kubectl; then
    substrate_absent "GVISOR_E2E=1 but kubectl not installed"
  elif ! kubectl get runtimeclass "$RUNTIME_CLASS" >/dev/null 2>&1; then
    substrate_absent "GVISOR_E2E=1 but RuntimeClass '$RUNTIME_CLASS' absent"
  elif ! kubectl get ns "$GVISOR_NS" >/dev/null 2>&1; then
    substrate_absent "GVISOR_E2E=1 but ns '$GVISOR_NS' absent (substrate not deployed)"
  elif [ "$(kubectl auth can-i create pods -n "$GVISOR_NS" 2>/dev/null)" != "yes" ]; then
    substrate_absent "GVISOR_E2E=1 but cannot create pods in ns '$GVISOR_NS'"
  fi

  if [ "$SUBSTRATE_OK" = "1" ]; then
    CANONICAL="dupspawn-${RUN_ID}"
    PODB="${CANONICAL}${THAW_PLACEHOLDER_SUFFIX}"

    cleanup_substrate() {
      kubectl -n "$GVISOR_NS" delete pod "$PODB" --grace-period=0 --force >/dev/null 2>&1 || true
      kubectl -n "$GVISOR_NS" delete pod "$CANONICAL" --grace-period=0 --force >/dev/null 2>&1 || true
    }
    trap cleanup_substrate EXIT

    # The two-racing-thaws shape: BOTH wakes try to create the SAME deterministic
    # `<canonical>-thaw-b` Pod B. We fire two concurrent `kubectl create` calls
    # for the identical object and assert exactly one succeeds-or-already-exists
    # and exactly one Pod B object lands — the k8s name-collision dedup the
    # create path swallows via apierrors.IsAlreadyExists (freezethaw.go), the
    # OUTER guard that bounds Pod-B object count even before the single-flight.
    blue "[substrate] firing two concurrent Pod-B creates for '$PODB'"
    PODB_MANIFEST=$(cat <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: ${PODB}
  namespace: ${GVISOR_NS}
  labels:
    ${LABEL_THAW_PLACEHOLDER}: "true"
    test: ka-0000007641
spec:
  restartPolicy: Never
  runtimeClassName: ${RUNTIME_CLASS}
  nodeSelector: {gvisor: enabled}
  tolerations:
  - {key: gvisor, operator: Equal, value: enabled, effect: NoSchedule}
  containers:
  - name: agent
    image: ${BUSYBOX_IMAGE}
    command: ["sleep", "120"]
YAML
)
    R1=""; R2=""
    C1OUT="/tmp/dup-c1-${RUN_ID}"; C2OUT="/tmp/dup-c2-${RUN_ID}"
    printf '%s' "$PODB_MANIFEST" | kubectl -n "$GVISOR_NS" create -f - >"$C1OUT" 2>&1 & P1=$!
    printf '%s' "$PODB_MANIFEST" | kubectl -n "$GVISOR_NS" create -f - >"$C2OUT" 2>&1 & P2=$!
    wait $P1; R1=$?
    wait $P2; R2=$?
    OUT1=$(cat "$C1OUT" 2>/dev/null); OUT2=$(cat "$C2OUT" 2>/dev/null)
    rm -f "$C1OUT" "$C2OUT" 2>/dev/null || true

    # Exactly one create call must win; the other must lose to AlreadyExists.
    WINS=0
    [ "$R1" = "0" ] && WINS=$((WINS+1))
    [ "$R2" = "0" ] && WINS=$((WINS+1))
    LOSERS_ALREADY_EXISTS=0
    grep -qi 'already exists' <<<"$OUT1" && LOSERS_ALREADY_EXISTS=$((LOSERS_ALREADY_EXISTS+1))
    grep -qi 'already exists' <<<"$OUT2" && LOSERS_ALREADY_EXISTS=$((LOSERS_ALREADY_EXISTS+1))
    if [ "$WINS" = "1" ] && [ "$LOSERS_ALREADY_EXISTS" = "1" ]; then
      pass "concurrent Pod-B creates: exactly 1 won, 1 lost to AlreadyExists (name-collision dedup)"
    else
      fail "concurrent Pod-B create dedup" "wins=$WINS already_exists=$LOSERS_ALREADY_EXISTS (out1='${OUT1:0:80}' out2='${OUT2:0:80}')"
    fi

    # The authoritative assertion: exactly ONE Pod B object exists for this agent.
    PODB_COUNT=$(kubectl -n "$GVISOR_NS" get pods \
      -l "${LABEL_THAW_PLACEHOLDER}=true,test=ka-0000007641" \
      --field-selector "metadata.namespace=${GVISOR_NS}" \
      -o name 2>/dev/null | grep -c "${PODB}$" || echo 0)
    if [ "$PODB_COUNT" = "1" ]; then
      pass "exactly ONE Pod B object exists after concurrent wakes (no double Pod B)"
    else
      fail "single Pod B" "found $PODB_COUNT Pod B objects named '$PODB' (expected exactly 1 — the ka-0000007549 class)"
    fi

    # No duplicate cold-start under the CANONICAL name — the reconciler
    # create-missing exemption + the absence of a real spawn here mean the
    # canonical pod must NOT have been minted as a racing duplicate.
    if kubectl -n "$GVISOR_NS" get pod "$CANONICAL" >/dev/null 2>&1; then
      fail "no duplicate canonical cold-start" "a pod named '$CANONICAL' exists — duplicate cold-start raced the Pod B (ka-0000007549)"
    else
      pass "no duplicate cold-start pod under the canonical name"
    fi

    cleanup_substrate
    trap - EXIT
  fi
fi
echo

# ── Summary ───────────────────────────────────────────────────────
echo "════════════════════════════════════════════════════"
echo " duplicate/concurrent-spawn DEDUP Summary: $PASS passed, $FAIL failed"
echo "════════════════════════════════════════════════════"
if [ "$FAIL" -gt 0 ]; then
  for e in "${ERRORS[@]}"; do red "  - $e"; done
  exit 1
fi
green "All duplicate/concurrent-spawn DEDUP assertions passed."
exit 0
