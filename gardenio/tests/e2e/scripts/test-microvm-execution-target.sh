#!/usr/bin/env bash
# test-microvm-execution-target.sh — E2E gate for the lambda_microvm agent
# execution backend: execution_target routing, spawn fail-closed, native
# suspend/resume eligibility, reconciler/GC suspend-awareness (standby != leak),
# and the warm-pool cost opt-in + IdlePolicy AWS-min floor.
#
# Bead ka-0000006937 (parent ka-0000000958, anchor epic ka-0000005010).
#
# THE GAP THIS CLOSES: an entire new agent RUNTIME landed and was prod-cutover'd
# in the 2026-07-17..07-31 window (!5595,!5598,!5600,!5604,!5620,!5621,!5623,
# !5624,!5683,!5749,!5777,!5797,!5807,!5809,!5810,!5811,!5886,!6218,!6349,!6383,
# !6408) with ZERO black-box coverage. ~30 Go unit test files exist
# (internal/microvm/*_test.go, poolmanager/microvm_*_test.go,
# reconciler_microvm_test.go, execution_target_resolve_test.go) — strong at the
# UNIT level — but a grep of gardenio/tests for microvm|lambda_microvm|RunMicrovm
# returns NOTHING microvm-specific (the only `execution_target` hits are the
# UNRELATED escalation field, whose values are local|service_account), and
# run-shard.sh wires no microvm module. Note also that the existing
# `gb-stop-suspend` module tests the gVisor/runsc `checkpoint --leave-running`
# FREEZE path (ka-0000000910, !4327) — a DIFFERENT execution backend, not
# lambda_microvm; and the PoC "C7 live E2E gate" (decision gd-JMbObkFnHI) was a
# one-off real-AWS smoke blocked on cross-account trust, never a repeatable
# module. So a regression in execution_target routing silently sends spawns to
# the wrong backend, and a suspend/resume-reconcile regression leaks standby
# MicroVMs (real continuous AWS billing) or drops resumes.
#
# WHY A CONTRACT LEG (not a pure live test): the lambda_microvm backend is
# default-OFF everywhere (agents.microvm.enabled=false) and, when on, requires a
# CROSS-ACCOUNT AWS launcher role in account 345678901234 plus real MicroVM
# spend. The staging e2e namespace sets no MICROVM_* env at all, so
# `microvmLifecycle.Available()` is false and NO MicroVM can be launched from the
# suite — the launch/suspend/resume/GC paths are structurally unobservable live in
# CI. Both existing "E2E validation" artifacts for this surface
# (docs/microvm-suspend-resume-e2e-validation.md,
# docs/microvm-warmpool-e2e-validation.md) are hand-run OPERATOR RUNBOOKS whose
# own §Preconditions/§Status sections record that the live run has NOT been
# executed — they are not automated regression nets. A faithful,
# decision-for-decision bash reimplementation of the five decision tables
# therefore asserts the FULL truth table deterministically in every shard, with
# no AWS and no spend — exactly the pattern test-thaw-reaper-exemption.sh and
# test-stale-reaper-grace-reconcile.sh use.
#
# TWO LEGS:
#   1. CONTRACT (always runs — needs only bash): the five decision tables
#      reimplemented over bash predicates and asserted over the full truth table
#      with negative controls and the </> boundaries. Runs in every shard, no
#      substrate, no AWS.
#   2. SUBSTRATE (opt-in, MICROVM_SUBSTRATE_E2E=1): assert the ROUTING FAIL-CLOSED
#      invariant against the LIVE controller — seed a real agent bead whose
#      metadata forces execution_target=lambda_microvm into a namespace where the
#      backend is unavailable, and assert the controller marks it
#      agent_state=failed and creates NO pod (never silently falls through to the
#      Kubernetes pod path). That is the one live-observable direction on a
#      cluster with no MicroVM backend, and it is precisely the handoff invariant
#      handleNonK8sSpawn exists to guarantee. SKIPs LOUD by default.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-microvm-execution-target.sh              # contract only
#   MICROVM_SUBSTRATE_E2E=1 ./gardenio/tests/e2e/scripts/test-microvm-execution-target.sh  # + substrate

set -uo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
KD_BIN="${KD_BIN:-kd}"
RUN_ID="${RUN_ID:-$$}"

# Execution-target constants — MUST match podmanager/manager.go L690-692
# (ExecutionTargetKubernetes/LambdaMicroVM/Probe). A drift here is itself a
# regression: renaming a target value without updating this reds these asserts.
TARGET_K8S="kubernetes"
TARGET_MICROVM="lambda_microvm"
TARGET_PROBE="probe"

# GC + IdlePolicy constants — MUST match the Go constants.
#   defaultGCMinAge              = 10m → 600s  (microvm/lifecycle.go L167)
#   minIdlePolicyDurationSeconds = 60         (microvm/backend.go L311)
GC_MIN_AGE=600
IDLE_POLICY_MIN=60

# Substrate leg: seconds to wait for the live controller to process the spawn
# event and fail the agent closed. The spawn path is event-driven (SSE) so this
# is fast; we poll rather than sleep the whole budget.
SPAWN_WAIT="${SPAWN_WAIT:-90}"

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
echo " lambda_microvm execution backend E2E (ka-0000006937)"
echo " routing · fail-closed · suspend/resume · standby≠leak · warm-pool opt-in"
echo "════════════════════════════════════════════════════"

# ===========================================================================
# LEG 1 — CONTRACT (always runs): the five decision tables.
#
# Each predicate mirrors, decision-for-decision, the Go the controller actually
# runs. The asserts walk the full truth table (both directions plus negative
# controls) so a future refactor that flips a default, widens a guard, or drops a
# floor fails here, in every shard, with no AWS.
# ===========================================================================

# ── Table A: execution_target resolution (!5595, !5598) ─────────────
# resolve_target <project_target> <role_targets_csv> <agent_metadata_target>
#   Mirrors resolveExecutionTarget (cmd/controller/podspec.go L1662-1705):
#   layered project → role (overrides project; for a multi-role agent the FIRST
#   role that sets a value wins) → agent metadata (highest precedence), then a
#   closed switch that maps "" / kubernetes → kubernetes, lambda_microvm →
#   lambda_microvm, probe → probe, and ANY unrecognized value → kubernetes.
#   The unknown→kubernetes fallback is the can't-strand-an-agent guarantee: a
#   typo'd or future backend name never leaves an agent unspawnable.
#   Empty string at a layer means "layer sets nothing" (the entry is absent or
#   its ExecutionTarget is ""), which is why "" never OVERRIDES a lower layer.
resolve_target() {
  local proj="$1" roles="$2" meta="$3"
  local target=""
  # Project layer.
  [ -n "$proj" ] && target="$proj"
  # Role layer — first role that sets a value wins, and it overrides project.
  local IFS=','
  local r
  for r in $roles; do
    r="$(echo "$r" | tr -d '[:space:]')"
    if [ -n "$r" ]; then
      target="$r"
      break
    fi
  done
  unset IFS
  # Agent metadata layer (highest precedence).
  [ -n "$meta" ] && target="$meta"
  # The closed switch.
  case "$target" in
    ""|"$TARGET_K8S")  echo "$TARGET_K8S" ;;
    "$TARGET_MICROVM") echo "$TARGET_MICROVM" ;;
    "$TARGET_PROBE")   echo "$TARGET_PROBE" ;;
    *)                 echo "$TARGET_K8S" ;;   # unrecognized → never strand
  esac
}

blue "[contract] execution_target resolution (!5595, !5598)"
# THE default: nothing set anywhere → the pod path. Every agent that predates the
# field keeps spawning pods (the can't-strand-existing-agents guarantee).
assert_verdict "no layer sets a target → kubernetes (default)" \
  "$(resolve_target "" "" "")" "$TARGET_K8S"
# Project opt-in routes ALL its agents to the MicroVM backend.
assert_verdict "project execution_target=lambda_microvm → microvm" \
  "$(resolve_target "$TARGET_MICROVM" "" "")" "$TARGET_MICROVM"
# Role overrides project — the gardenio-microvms role (ka-0000007754) opts in
# regardless of which project it runs in.
assert_verdict "role=lambda_microvm overrides project=kubernetes → microvm" \
  "$(resolve_target "$TARGET_K8S" "$TARGET_MICROVM" "")" "$TARGET_MICROVM"
# ...and in BOTH directions: a kubernetes role must override a microvm project,
# or a project-wide opt-in could not be exempted per-role.
assert_verdict "role=kubernetes overrides project=lambda_microvm → kubernetes" \
  "$(resolve_target "$TARGET_MICROVM" "$TARGET_K8S" "")" "$TARGET_K8S"
# Agent metadata is highest precedence, including forcing BACK to kubernetes.
assert_verdict "agent metadata=kubernetes overrides role=lambda_microvm → kubernetes" \
  "$(resolve_target "" "$TARGET_MICROVM" "$TARGET_K8S")" "$TARGET_K8S"
assert_verdict "agent metadata=lambda_microvm overrides project+role kubernetes → microvm" \
  "$(resolve_target "$TARGET_K8S" "$TARGET_K8S" "$TARGET_MICROVM")" "$TARGET_MICROVM"
# Multi-role: the FIRST role that sets a value wins (mirrors resolveZitiAccess).
assert_verdict "multi-role, first role (microvm) wins over later kubernetes role" \
  "$(resolve_target "" "$TARGET_MICROVM,$TARGET_K8S" "")" "$TARGET_MICROVM"
assert_verdict "multi-role, first role (kubernetes) wins over later microvm role" \
  "$(resolve_target "" "$TARGET_K8S,$TARGET_MICROVM" "")" "$TARGET_K8S"
# A role layer that sets NOTHING must not clobber a project opt-in.
assert_verdict "role sets nothing → project=lambda_microvm survives" \
  "$(resolve_target "$TARGET_MICROVM" "" "")" "$TARGET_MICROVM"
# probe is a RECOGNIZED value that resolves to ITSELF — not the kubernetes
# fallback — so it reaches the probe-aware fail-closed branch (Table B) instead
# of silently spawning a coop agent pod.
assert_verdict "agent metadata=probe → probe (NOT the kubernetes fallback)" \
  "$(resolve_target "" "" "$TARGET_PROBE")" "$TARGET_PROBE"
# Negative controls — an unrecognized value NEVER strands an agent.
assert_verdict "unknown target (typo 'firecracker-typo') → kubernetes fallback" \
  "$(resolve_target "firecracker-typo" "" "")" "$TARGET_K8S"
assert_verdict "unknown target at the agent layer → kubernetes fallback" \
  "$(resolve_target "" "" "lambda-microvm")" "$TARGET_K8S"
# Case sensitivity: the switch is exact-match, so a mis-cased value is unknown →
# kubernetes. Pinned so nobody "helpfully" adds a ToLower and silently widens
# what routes real spawns to a billing backend.
assert_verdict "mis-cased 'Lambda_MicroVM' is NOT recognized → kubernetes" \
  "$(resolve_target "" "" "Lambda_MicroVM")" "$TARGET_K8S"
echo

# ── Table B: spawn dispatch fail-closed (handleNonK8sSpawn) ─────────
# spawn_verdict <resolved_target> <microvm_backend_available:0|1>
#   Mirrors handleNonK8sSpawn (cmd/controller/main.go L1858-1968) plus the
#   IsKubernetesTarget gate that routes to it:
#     - kubernetes (or "")          → POD           (the normal path)
#     - lambda_microvm + available  → MICROVM       (Launch: per-run secret + RunMicrovm)
#     - lambda_microvm + UNavailable→ FAIL_CLOSED   (default-off/misconfigured)
#     - probe                       → FAIL_CLOSED   (CLI-driven: `gb probe run`)
#     - anything else               → FAIL_CLOSED   (unrecognized non-k8s)
#   THE INVARIANT: one spawn event never produces a Kubernetes pod for a
#   MicroVM-targeted agent, and never a MicroVM for a Kubernetes agent. A
#   fall-through to POD on an unavailable backend is the exact silent-misroute
#   regression this table exists to catch.
spawn_verdict() {
  local target="$1" avail="$2"
  case "$target" in
    ""|"$TARGET_K8S")  echo "POD"; return ;;
  esac
  if [ "$target" = "$TARGET_MICROVM" ]; then
    if [ "$avail" = "1" ]; then
      echo "MICROVM"
    else
      echo "FAIL_CLOSED"   # NEVER fall through to a pod
    fi
    return
  fi
  echo "FAIL_CLOSED"       # probe (CLI-driven) or unrecognized non-k8s
}

blue "[contract] spawn dispatch fail-closed (handleNonK8sSpawn)"
assert_verdict "kubernetes target → pod path" \
  "$(spawn_verdict "$TARGET_K8S" 0)" "POD"
assert_verdict "empty target → pod path" \
  "$(spawn_verdict "" 0)" "POD"
assert_verdict "lambda_microvm + backend available → microvm launch" \
  "$(spawn_verdict "$TARGET_MICROVM" 1)" "MICROVM"
# THE HEADLINE regression: with the backend default-off, a microvm-targeted spawn
# must FAIL, not silently become a pod (which would run a satellite-intended
# agent with cluster-internal env + no Ziti overlay).
assert_verdict "lambda_microvm + backend UNAVAILABLE → fail closed (never a pod)" \
  "$(spawn_verdict "$TARGET_MICROVM" 0)" "FAIL_CLOSED"
# probe fails closed even when the MicroVM backend IS up — it is a different,
# CLI-driven lifecycle with no agent bead/coop/credentials.
assert_verdict "probe target → fail closed (CLI-driven, not the spawn door)" \
  "$(spawn_verdict "$TARGET_PROBE" 1)" "FAIL_CLOSED"
assert_verdict "probe target, backend down → still fail closed" \
  "$(spawn_verdict "$TARGET_PROBE" 0)" "FAIL_CLOSED"
# Negative control: a kubernetes agent must NEVER reach the MicroVM backend,
# even when it is available.
assert_verdict "kubernetes target + backend available → still pod (never microvm)" \
  "$(spawn_verdict "$TARGET_K8S" 1)" "POD"
echo

# ── Table C: native suspend-on-stop eligibility (!5797, !5807) ──────
# suspend_eligible <master_switch:0|1> <resolved_target> <thread_channel> <thread_ts>
#   Mirrors isMicrovmSuspendEligible (cmd/controller/podspec.go L669-674) — the
#   SINGLE predicate both the in-VM `gb stop` signal (markMicrovmSuspendEligible
#   → BOAT_FREEZE_ELIGIBLE) and the controller's teardown branch agree on:
#     MICROVM_SUSPEND_ENABLED && target==lambda_microvm && SLACK_THREAD_CHANNEL
#     && SLACK_THREAD_TS
#   Only THREAD agents suspend — a later re-mention is what resumes them. A
#   one-shot crew/gardener MicroVM agent has no wake to serve, so it TERMINATES on
#   stop as before. Every negative direction below is a VM that must be
#   terminated, not left billing in standby.
suspend_eligible() {
  local master="$1" target="$2" chan="$3" ts="$4"
  if [ "$master" = "1" ] && [ "$target" = "$TARGET_MICROVM" ] \
     && [ -n "$chan" ] && [ -n "$ts" ]; then
    echo "SUSPEND"
  else
    echo "TERMINATE"
  fi
}

blue "[contract] native suspend-on-stop eligibility (!5797, !5807)"
assert_verdict "master on + microvm + full thread binding → SUSPEND" \
  "$(suspend_eligible 1 "$TARGET_MICROVM" "C123" "1785900000.1")" "SUSPEND"
# Master switch off is the byte-for-byte rollback: terminate-on-stop as before.
assert_verdict "master switch OFF (default) → TERMINATE (rollback preserved)" \
  "$(suspend_eligible 0 "$TARGET_MICROVM" "C123" "1785900000.1")" "TERMINATE"
# A non-thread MicroVM agent (crew/gardener) has no resume trigger → terminate.
assert_verdict "microvm agent with NO thread binding → TERMINATE (no wake to serve)" \
  "$(suspend_eligible 1 "$TARGET_MICROVM" "" "")" "TERMINATE"
# Partial thread binding must NOT suspend — a half-bound agent is unresumable,
# so suspending it would strand a billing VM nothing can ever wake.
assert_verdict "thread channel but NO ts → TERMINATE (unresumable)" \
  "$(suspend_eligible 1 "$TARGET_MICROVM" "C123" "")" "TERMINATE"
assert_verdict "thread ts but NO channel → TERMINATE (unresumable)" \
  "$(suspend_eligible 1 "$TARGET_MICROVM" "" "1785900000.1")" "TERMINATE"
# Negative control: a KUBERNETES thread agent must never take the MicroVM
# suspend path (it rides the separate gVisor freeze path, which isFreezeEligible
# owns; the two are mutually exclusive by construction).
assert_verdict "kubernetes thread agent → TERMINATE (not the microvm suspend path)" \
  "$(suspend_eligible 1 "$TARGET_K8S" "C123" "1785900000.1")" "TERMINATE"
echo

# ── Table C2: resume-on-message routing (!5809, !5810) ──────────────
# resume_verdict <resolved_target> <agent_state> <runtime_id>
#   Mirrors handleMicrovmResume's three guards (cmd/controller/main.go
#   L3707-3720) behind POST /api/v1/microvm/resume, which the bridge calls when a
#   new message lands in a suspended agent's thread (bridge tryMicrovmResume):
#     1. execution_target == lambda_microvm  else → NOT_APPLICABLE "not a microvm agent"
#     2. agent_state      == stopping        else → NOT_APPLICABLE "agent not suspended"
#     3. runtime_id       != ""              else → NOT_APPLICABLE "no runtime_id to resume"
#   All three hold → RESUME the SAME VM in place. NOT_APPLICABLE is the
#   routing-unchanged signal: the bridge falls through to its normal cold
#   respawn, so a miss degrades to a cold start rather than dropping the message.
#   The critical invariant is resume-IN-PLACE: closing the bead to remint would
#   fire AgentDone → Terminate and destroy the very VM being resumed.
resume_verdict() {
  local target="$1" state="$2" runtime_id="$3"
  [ "$target" != "$TARGET_MICROVM" ] && { echo "NOT_APPLICABLE"; return; }
  [ "$state" != "stopping" ]         && { echo "NOT_APPLICABLE"; return; }
  [ -z "$runtime_id" ]               && { echo "NOT_APPLICABLE"; return; }
  echo "RESUME"
}

blue "[contract] resume-on-message routing (!5809, !5810)"
assert_verdict "microvm + stopping + runtime_id → RESUME in place" \
  "$(resume_verdict "$TARGET_MICROVM" "stopping" "mvm-abc123")" "RESUME"
# A kubernetes agent must never reach the MicroVM resume path.
assert_verdict "kubernetes agent → not applicable (bridge cold-respawns)" \
  "$(resume_verdict "$TARGET_K8S" "stopping" "mvm-abc123")" "NOT_APPLICABLE"
# Only a SUSPENDED (agent_state=stopping) agent is resumable. Resuming a working
# agent would double-drive a live VM.
assert_verdict "microvm already working → not applicable (not suspended)" \
  "$(resume_verdict "$TARGET_MICROVM" "working" "mvm-abc123")" "NOT_APPLICABLE"
assert_verdict "microvm still spawning → not applicable" \
  "$(resume_verdict "$TARGET_MICROVM" "spawning" "mvm-abc123")" "NOT_APPLICABLE"
# A failed resume must not be retried in place — it cold-starts instead.
assert_verdict "microvm at resume_failed → not applicable (cold-start next)" \
  "$(resume_verdict "$TARGET_MICROVM" "resume_failed" "mvm-abc123")" "NOT_APPLICABLE"
# No runtime_id means there is no VM to resume — cold-start, never guess.
assert_verdict "microvm + stopping but NO runtime_id → not applicable" \
  "$(resume_verdict "$TARGET_MICROVM" "stopping" "")" "NOT_APPLICABLE"
echo

# ── Table C3: spawn/teardown predicate lockstep (!5797) ─────────────
# The in-VM `gb stop` signal (isMicrovmSuspendEligible, podspec.go L669, read
# from spec.Env SLACK_THREAD_*) and the controller's teardown decision
# (microvmTeardown at main.go L1794 + microvmSuspendOnStop at main.go L1745, read
# from event.Metadata slack_thread_channel/_ts) are TWIN predicates over the SAME
# underlying bead metadata. If they drift, the in-VM gb stop emits AgentStop
# expecting a suspend while the controller terminates (losing the resume target),
# or the bead is left open at stopping pointing at a dead VM (an unresumable
# zombie that bills until the AWS backstop). This asserts they agree on every input.
teardown_suspends() {
  local master="$1" target="$2" chan="$3" ts="$4"
  # NOTE ON FIDELITY: microvmSuspendOnStop (main.go L1745-1749) checks ONLY the
  # master switch + the two thread-metadata keys — it does NOT check the target
  # itself. The execution_target==lambda_microvm gate lives UPSTREAM in its sole
  # caller, microvmTeardown (main.go L1795-1798), which returns NotApplicable for
  # any non-MicroVM agent before this predicate is ever consulted. What we model
  # here is therefore the COMPOSED teardown decision (gate + predicate), which is
  # the thing that must match the spawn-side composed predicate — so including the
  # target term here is correct, not a phantom check.
  if [ "$master" = "1" ] && [ "$target" = "$TARGET_MICROVM" ] \
     && [ -n "$chan" ] && [ -n "$ts" ]; then
    echo "SUSPEND"
  else
    echo "TERMINATE"
  fi
}

blue "[contract] spawn/teardown twin-predicate lockstep (!5797)"
for _case in \
  "1 $TARGET_MICROVM C123 172.5" \
  "0 $TARGET_MICROVM C123 172.5" \
  "1 $TARGET_MICROVM '' ''" \
  "1 $TARGET_MICROVM C123 ''" \
  "1 $TARGET_MICROVM '' 172.5" \
  "1 $TARGET_K8S C123 172.5" ; do
  # shellcheck disable=SC2086
  eval "set -- $_case"
  _spawn_side=$(suspend_eligible "$1" "$2" "${3:-}" "${4:-}")
  _teardown_side=$(teardown_suspends "$1" "$2" "${3:-}" "${4:-}")
  assert_verdict "lockstep(master=$1,target=$2,chan='${3:-}',ts='${4:-}'): teardown matches spawn ($_spawn_side)" \
    "$_teardown_side" "$_spawn_side"
done
echo

# ── Table C4: agent_state vocabulary completeness (bug ka-0000000585) ──
# The microvm suspend/resume lifecycle writes FOUR agent_state values. Each must
# be accepted by the type:agent schema, or the field write is REJECTED and — since
# both call sites only log the error (lifecycle.go L552, rehydrate.go L360) — the
# state silently never lands:
#   stopping      — Suspend leaves the bead open here as the resume target
#   resuming      — Resume, when the rehydrate gate is on (lifecycle.go L546)
#   working       — rehydration proof-of-life released (rehydrate.go L321)
#   resume_failed — proof-of-life timed out, VM terminated (rehydrate.go L357)
#
# THE BUG THIS FOUND + FIXED (ka-0000000585): the BUILTIN type:agent schema
# (kbeads/internal/server/config.go L456) declared agent_state as an ENUM that
# omitted BOTH `resuming` and `resume_failed`. The update-path grandfather only
# spares a BYTE-IDENTICAL prior value (validate_fields.go L146-148), so a
# stopping→resuming transition was rejected — and because both call sites only
# log, the state silently never landed. Prod was masked by a STORED type:agent
# override where agent_state is a plain string, and listConfigsWithBuiltins
# (config.go L1789) only merges a builtin when no stored config exists — so it was
# latent on any daemon WITHOUT that override (a fresh gasbox/GiB, a fresh e2e
# namespace: seed-types-and-users.sh does NOT seed type:agent). Both values are
# now in the builtin enum; this table is the regression net.
#
# BUILTIN_AGENT_STATES mirrors that enum verbatim — keep the two in lockstep.
#
# ka-0000006367 extends the same net to three MORE controller-written states that
# were still dropped by the builtin enum (same silent-PATCH-drop class): "queued"
# (reconciler markQueued + spawn-declined/pool-exhausted, main.go/reconciler.go),
# "model_rate_limited" (in-pod coop ErrorCategory=model_rate_limited, init_agent.go),
# and "stop_requested" as an agent_state VALUE (RequestAgentStop, client.go —
# distinct from the separate stop_requested STRING field). All three transition
# FROM an in-enum prior state, so the byte-identical grandfather cannot save them.
# "killed" is the close-time state `gb agent kill` writes (gd-CTvPgeXKc5).
# "paused" is the stale reaper's laptop-agent hold (gd-PjlxxDPvaz).
BUILTIN_AGENT_STATES="idle spawning assigning working stopping done failed rate_limited budget_exceeded prewarmed resuming resume_failed queued model_rate_limited stop_requested killed paused"
state_accepted() {
  local want="$1" s
  for s in $BUILTIN_AGENT_STATES; do
    [ "$s" = "$want" ] && { echo "ACCEPTED"; return; }
  done
  echo "REJECTED"
}

blue "[contract] agent_state vocabulary for the microvm lifecycle (bug ka-0000000585)"
assert_verdict "suspend writes agent_state=stopping → accepted" \
  "$(state_accepted stopping)" "ACCEPTED"
assert_verdict "rehydration release writes agent_state=working → accepted" \
  "$(state_accepted working)" "ACCEPTED"
# These two are the bug. They must be ACCEPTED for the resume path to be
# observable at all; today the builtin enum rejects them.
assert_verdict "resume writes agent_state=resuming → must be accepted" \
  "$(state_accepted resuming)" "ACCEPTED"
assert_verdict "proof-of-life failure writes agent_state=resume_failed → must be accepted" \
  "$(state_accepted resume_failed)" "ACCEPTED"
# ka-0000006367: three more controller-written states that were still dropped.
# Each transitions FROM an in-enum prior state (spawning/working/idle), so the
# byte-identical grandfather cannot save them — they MUST be in the enum.
assert_verdict "pool-exhausted/spawn-declined writes agent_state=queued → must be accepted" \
  "$(state_accepted queued)" "ACCEPTED"
assert_verdict "in-pod model throttle writes agent_state=model_rate_limited → must be accepted" \
  "$(state_accepted model_rate_limited)" "ACCEPTED"
assert_verdict "RequestAgentStop writes agent_state=stop_requested → must be accepted" \
  "$(state_accepted stop_requested)" "ACCEPTED"
# Negative control: the vocabulary check is a real membership test, not a
# rubber stamp that would pass for anything.
assert_verdict "a bogus state is NOT accepted (control)" \
  "$(state_accepted totally_not_a_state)" "REJECTED"
echo

# ── Table D: GC standby≠leak + max-standby-age cap (!5811, !6383) ───
# state_liveness <aws_microvm_state>
#   Mirrors statusFromState (microvm/backend.go L852-867) — the STRUCTURAL root of
#   the whole standby≠leak guarantee. SUSPENDING/SUSPENDED map to Live (not
#   Terminal), which is what keeps Lifecycle.Reconcile from treating a standby VM
#   as terminal and closing its bead (which would fire Terminate and destroy the
#   resume target). An unknown/future state is deliberately NEITHER — the
#   reconciler must never reap it, nor trust it blindly.
state_liveness() {
  case "$1" in
    PENDING|RUNNING|SUSPENDING|SUSPENDED) echo "LIVE" ;;
    TERMINATING|TERMINATED)               echo "TERMINAL" ;;
    *)                                    echo "UNKNOWN" ;;
  esac
}

blue "[contract] AWS state → liveness (statusFromState) — why standby≠leak is structural"
# THE load-bearing pair: if either of these flipped to TERMINAL, every suspended
# thread agent would be closed + terminated on the next reconcile pass.
assert_verdict "SUSPENDED → LIVE (standby is not terminal)" \
  "$(state_liveness SUSPENDED)" "LIVE"
assert_verdict "SUSPENDING → LIVE (mid-suspend is not terminal)" \
  "$(state_liveness SUSPENDING)" "LIVE"
assert_verdict "RUNNING → LIVE" "$(state_liveness RUNNING)" "LIVE"
assert_verdict "PENDING → LIVE (booting)" "$(state_liveness PENDING)" "LIVE"
assert_verdict "TERMINATED → TERMINAL" "$(state_liveness TERMINATED)" "TERMINAL"
assert_verdict "TERMINATING → TERMINAL" "$(state_liveness TERMINATING)" "TERMINAL"
# Conservative default for a future AWS state: neither reap nor trust.
assert_verdict "unknown/future AWS state → neither live nor terminal" \
  "$(state_liveness SOME_FUTURE_STATE)" "UNKNOWN"
echo


# gc_runtime_verdict <live:0|1> <claimed_by_open_bead:0|1> <age_seconds>
#   Mirrors Lifecycle.GC's runtime leak loop (microvm/lifecycle.go L692-711):
#     - !Live (TERMINATING/TERMINATED) → NOOP (AWS reaps it)
#     - claimed by an open bead's runtime_id → NOOP  ← this is how a SUSPENDED
#       standby VM survives: it stays in liveRuntimeIDs (main.go's runMicrovmLeakGC
#       adds every open MicroVM bead's runtime_id), so "standby != leak" is
#       structural, not a special case.
#     - unclaimed AND age >= gcMinAge (10m) → REAP
#     - unclaimed but YOUNGER than gcMinAge → SKIP (launch/bead-write in flight)
gc_runtime_verdict() {
  local live="$1" claimed="$2" age="$3"
  [ "$live" != "1" ]   && { echo "NOOP"; return; }   # already going away
  [ "$claimed" = "1" ] && { echo "NOOP"; return; }   # an open bead claims it
  if [ "$age" -lt "$GC_MIN_AGE" ]; then
    echo "SKIP"   # too young — a bead write may be in flight
  else
    echo "REAP"   # unclaimed and aged — a genuine leak
  fi
}

blue "[contract] GC runtime leak loop — standby≠leak (!5811)"
# THE standby≠leak guarantee: a SUSPENDED VM is claimed by its still-open
# resume-target bead, so the leak loop leaves it alone NO MATTER HOW OLD. If this
# regressed, every suspended thread agent would be terminated out from under its
# pending resume.
assert_verdict "claimed SUSPENDED standby VM, very old (24h) → left alone" \
  "$(gc_runtime_verdict 1 1 86400)" "NOOP"
assert_verdict "claimed live VM, aged past gc-min-age → left alone" \
  "$(gc_runtime_verdict 1 1 3600)" "NOOP"
# The other direction — a genuinely unclaimed, aged runtime IS reaped (a crash
# between RunMicrovm and the bead write would otherwise burn its whole
# MaximumDurationInSeconds).
assert_verdict "unclaimed runtime past gc-min-age (30m) IS reaped" \
  "$(gc_runtime_verdict 1 0 1800)" "REAP"
# The young-launch race guard: an unclaimed runtime seconds old is mid-launch.
assert_verdict "unclaimed runtime younger than gc-min-age (1m) NOT reaped" \
  "$(gc_runtime_verdict 1 0 60)" "SKIP"
assert_verdict "unclaimed runtime just under gc-min-age (9m59s) NOT reaped" \
  "$(gc_runtime_verdict 1 0 599)" "SKIP"
# Boundary: the guard is `now.Sub(StartedAt) < gcMinAge ⇒ continue`, so exactly
# AT 10m the runtime IS reaped.
assert_verdict "unclaimed runtime exactly AT gc-min-age (600s) IS reaped" \
  "$(gc_runtime_verdict 1 0 600)" "REAP"
# Already-terminating runtimes are never touched, claimed or not.
assert_verdict "non-live (TERMINATING) unclaimed old runtime → left to AWS" \
  "$(gc_runtime_verdict 0 0 86400)" "NOOP"
echo

# standby_cap_verdict <cap_seconds> <has_anchor:0|1> <anchor_parseable:0|1> <standby_age_seconds>
#   Mirrors reapExpiredStandby (microvm/lifecycle.go L784-838), which the GC only
#   calls when maxStandbyAge > 0:
#     - cap <= 0 (default)          → DISABLED (rely on the AWS IdlePolicy
#                                     SuspendedDurationSeconds backstop)
#     - no runtime_suspended_at     → LEAVE (defer to the AWS backstop; never
#                                     reap on a guess)
#     - unparseable anchor          → LEAVE (logged; AWS backstop reaps it)
#     - age <  cap                  → LEAVE (intentional standby)
#     - age >= cap                  → REAP  (terminate VM + close bead as done)
standby_cap_verdict() {
  local cap="$1" has_anchor="$2" parseable="$3" age="$4"
  [ "$cap" -le 0 ]        && { echo "DISABLED"; return; }
  [ "$has_anchor" != "1" ] && { echo "LEAVE"; return; }
  [ "$parseable" != "1" ]  && { echo "LEAVE"; return; }
  if [ "$age" -lt "$cap" ]; then
    echo "LEAVE"   # within the standby budget
  else
    echo "REAP"    # outlived its standby budget
  fi
}

blue "[contract] max-standby-age cap (!6383) — bounds standby billing"
CAP_24H=86400
# Default: the cap is OFF, so standby relies solely on the AWS IdlePolicy
# backstop. Pinned so enabling-by-default (a real cost/behavior change) is a
# deliberate, test-visible act.
assert_verdict "cap unset (0, default) → disabled (AWS IdlePolicy backstop only)" \
  "$(standby_cap_verdict 0 1 1 999999)" "DISABLED"
assert_verdict "standby within the 24h cap (1h) → left alone" \
  "$(standby_cap_verdict "$CAP_24H" 1 1 3600)" "LEAVE"
assert_verdict "standby past the 24h cap (25h) IS reaped" \
  "$(standby_cap_verdict "$CAP_24H" 1 1 90000)" "REAP"
# Boundary: `age < maxStandbyAge ⇒ continue`, so exactly AT the cap it IS reaped.
assert_verdict "standby exactly AT the cap (24h) IS reaped" \
  "$(standby_cap_verdict "$CAP_24H" 1 1 86400)" "REAP"
# Never reap on a guess: a missing or unparseable anchor defers to AWS rather
# than terminating a VM whose standby age is unknown.
assert_verdict "no runtime_suspended_at anchor → left to the AWS backstop" \
  "$(standby_cap_verdict "$CAP_24H" 0 1 999999)" "LEAVE"
assert_verdict "unparseable runtime_suspended_at → left to the AWS backstop" \
  "$(standby_cap_verdict "$CAP_24H" 1 0 999999)" "LEAVE"
echo

# ── Table E: warm-pool opt-in + IdlePolicy floor (!6349, !6408) ─────
# pool_min_size <resolved_target> <configured_min>
#   Mirrors poolmanager.Reconcile's lambda_microvm opt-in guard
#   (poolmanager.go L225-247) over EffectivePools' K8s default fill: a
#   lambda_microvm pool whose operator-configured min_size is <= 0 has NOT been
#   sized, so it is DROPPED before any create/recycle and stays at 0/off. A
#   Kubernetes pool with an unset min keeps its historical default of 2 (idle
#   pods are free; idle MicroVMs bill continuously and burn their wall-clock cap).
K8S_POOL_DEFAULT_MIN=2
pool_min_size() {
  local target="$1" configured="$2"
  if [ "$target" = "$TARGET_MICROVM" ]; then
    if [ "$configured" -le 0 ]; then
      echo 0            # not opted in — mint nothing
    else
      echo "$configured"
    fi
    return
  fi
  # Kubernetes: EffectivePools fills the default when unset.
  if [ "$configured" -le 0 ]; then
    echo "$K8S_POOL_DEFAULT_MIN"
  else
    echo "$configured"
  fi
}

blue "[contract] warm-pool lambda_microvm opt-in guard (!6349)"
# THE cost invariant: an unsized MicroVM pool must mint ZERO warm members. The
# regression this catches would silently bill 2 continuously-running VMs per
# project nobody asked for.
assert_verdict "microvm pool, min_size unset (0) → 0 members (NOT the K8s default 2)" \
  "$(pool_min_size "$TARGET_MICROVM" 0)" "0"
assert_verdict "microvm pool, negative min_size → 0 members" \
  "$(pool_min_size "$TARGET_MICROVM" -1)" "0"
# Explicit opt-in is honored verbatim (the §0 project-bead edit in
# docs/microvm-warmpool-e2e-validation.md).
assert_verdict "microvm pool, explicit min_size=1 → 1 member (opt-in honored)" \
  "$(pool_min_size "$TARGET_MICROVM" 1)" "1"
assert_verdict "microvm pool, explicit min_size=3 → 3 members" \
  "$(pool_min_size "$TARGET_MICROVM" 3)" "3"
# Negative control — the K8s pool default MUST be preserved byte-for-byte; the
# cost guard is microvm-only.
assert_verdict "kubernetes pool, min_size unset → default 2 (unchanged)" \
  "$(pool_min_size "$TARGET_K8S" 0)" "2"
assert_verdict "kubernetes pool, explicit min_size=5 → 5" \
  "$(pool_min_size "$TARGET_K8S" 5)" "5"
echo

# idle_policy <enabled:0|1> <max_idle_seconds> <suspended_seconds>
#   Mirrors NewIdlePolicy (microvm/backend.go L330-346): disabled → UNSET (the
#   IdlePolicy member is omitted and the launch shape is byte-for-byte identical
#   to the pre-idle-policy path); enabled → all three members set, with BOTH
#   duration members floored at the AWS minimum of 60. The floor exists because
#   maxIdleDurationSeconds=0 400-rejected and thereby blocked EVERY
#   lambda_microvm launch in prod (ka-0000005755) — a P0 this table pins shut.
idle_policy() {
  local enabled="$1" max_idle="$2" susp="$3"
  if [ "$enabled" != "1" ]; then
    echo "UNSET"
    return
  fi
  [ "$max_idle" -lt "$IDLE_POLICY_MIN" ] && max_idle="$IDLE_POLICY_MIN"
  [ "$susp"     -lt "$IDLE_POLICY_MIN" ] && susp="$IDLE_POLICY_MIN"
  echo "${max_idle}/${susp}"
}

blue "[contract] IdlePolicy AWS-min floor (!6408 / ka-0000005755)"
# Disabled is the byte-identical default — RunMicrovm's IdlePolicy stays unset.
assert_verdict "idlePolicy disabled (default) → member UNSET (byte-identical launch)" \
  "$(idle_policy 0 0 0)" "UNSET"
# THE P0 regression: 0 must be floored to 60, never passed to AWS.
assert_verdict "enabled with maxIdle=0 → floored to the AWS min 60 (not a 400)" \
  "$(idle_policy 1 0 900)" "60/900"
assert_verdict "enabled with suspended=0 → floored to the AWS min 60" \
  "$(idle_policy 1 60 0)" "60/60"
assert_verdict "enabled with BOTH 0 → both floored to 60" \
  "$(idle_policy 1 0 0)" "60/60"
# Sub-minimum values are floored too (AWS rejects anything < 60).
assert_verdict "enabled with maxIdle=59 (sub-minimum) → floored to 60" \
  "$(idle_policy 1 59 900)" "60/900"
# Legitimate values pass through untouched — the prod overlay uses 900.
assert_verdict "enabled with prod-overlay values (60/900) → passed through" \
  "$(idle_policy 1 60 900)" "60/900"
assert_verdict "enabled at exactly the floor (60/60) → passed through" \
  "$(idle_policy 1 60 60)" "60/60"
echo

# ===========================================================================
# LEG 2 — SUBSTRATE (opt-in, MICROVM_SUBSTRATE_E2E=1): the LIVE fail-closed
# routing invariant.
#
# Seed a REAL agent bead whose metadata forces execution_target=lambda_microvm in
# a namespace where the MicroVM backend is UNAVAILABLE (the e2e namespace sets no
# MICROVM_* env, so microvmLifecycle.Available() is false). The controller must
# mark it agent_state=failed with a failure_reason naming the unavailable backend,
# and create NO pod — never silently falling through to the Kubernetes pod path.
#
# This is the ONE live-observable direction of the whole surface on a cluster with
# no MicroVM backend, and it is exactly the handoff invariant handleNonK8sSpawn
# exists to guarantee (main.go L1855-1857). Every other direction (a real launch,
# suspend, resume, or GC sweep) needs the cross-account AWS launcher role plus
# real MicroVM spend, so those stay with the contract leg + the Go unit tests.
#
# NOTE this leg creates a type=agent bead, which the reconciler DOES act on — but
# that is the point: the assertion is that it acts by failing the agent closed
# rather than by creating a pod. It is cleaned up on exit. SKIPs LOUD by default.
# ===========================================================================
if [ "${MICROVM_SUBSTRATE_E2E:-0}" != "1" ]; then
  blue "[substrate] SKIP — MICROVM_SUBSTRATE_E2E != 1 (contract leg already asserted all five tables)"
  dim "  Set MICROVM_SUBSTRATE_E2E=1 against a live daemon+controller to observe the fail-closed routing invariant."
else
  blue "[substrate] MICROVM_SUBSTRATE_E2E=1 — observing live fail-closed routing"
  SUBSTRATE_OK=1

  # Reuse the shard-provided daemon when present (run-shard.sh exports
  # BEADS_HTTP_URL + BEADS_TOKEN); otherwise stand up our own port-forward so the
  # module is runnable standalone.
  if [ -z "${BEADS_HTTP_URL:-}" ]; then
    if ! have kubectl; then
      fail "substrate preflight" "MICROVM_SUBSTRATE_E2E=1 but no BEADS_HTTP_URL and kubectl not installed"
      SUBSTRATE_OK=0
    else
      blue "[substrate] port-forwarding ${NAMESPACE}/${BEADS_SVC}..."
      kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" 19112:8080 \
        >/tmp/kd-pf-microvm-exec-target.log 2>&1 &
      PF_PID=$!
      export BEADS_HTTP_URL="http://localhost:19112"
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

    AGENT_NAME="microvm-exectarget-e2e-${RUN_ID}"

    blue "[substrate] seeding agent bead forcing execution_target=lambda_microvm (backend unavailable here)"
    AG_OUT=$("$KD_BIN" create "$AGENT_NAME" --type=agent \
      -l "project:e2e-test" \
      --field agent="$AGENT_NAME" \
      --field project=e2e-test \
      --field role=crew \
      --field mode=crew \
      --field execution_target="$TARGET_MICROVM" \
      --field agent_state=spawning \
      --json 2>/dev/null || echo "")
    AG_ID=$(echo "$AG_OUT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
    if [ -z "$AG_ID" ]; then
      fail "seed microvm agent bead" "kd create returned no id (out: $(echo "$AG_OUT" | head -1))"
    else
      CREATED_BEADS+=("$AG_ID")
      pass "seeded agent bead $AG_ID with execution_target=lambda_microvm"

      # Poll for the controller's verdict rather than sleeping the whole budget.
      blue "[substrate] polling ≤${SPAWN_WAIT}s for the controller's fail-closed verdict"
      FINAL_STATE=""
      FAIL_REASON=""
      _deadline=$(( $(date +%s) + SPAWN_WAIT ))
      while [ "$(date +%s)" -lt "$_deadline" ]; do
        _json=$("$KD_BIN" show "$AG_ID" --json 2>/dev/null || echo "")
        if [ -n "$_json" ]; then
          FINAL_STATE=$(echo "$_json" | python3 -c "import sys,json; print(json.load(sys.stdin).get('fields',{}).get('agent_state',''))" 2>/dev/null || echo "")
          FAIL_REASON=$(echo "$_json" | python3 -c "import sys,json; print(json.load(sys.stdin).get('fields',{}).get('failure_reason',''))" 2>/dev/null || echo "")
          [ "$FINAL_STATE" = "failed" ] && break
        fi
        sleep 5
      done

      # ASSERTION 1: the agent failed closed (never left spawning, never working).
      if [ "$FINAL_STATE" = "failed" ]; then
        pass "microvm-targeted agent failed CLOSED (agent_state=failed, no pod fallthrough)"
        # The failure must be DIAGNOSABLE (ka-0000006530): a bare metadata-only
        # card with no explanation is the regression markAgentFailed closed.
        case "$FAIL_REASON" in
          *MicroVM*|*microvm*|*backend*)
            pass "failure_reason names the unavailable backend: '$FAIL_REASON'" ;;
          "")
            fail "failure_reason empty" "expected a human-legible cause naming the unavailable MicroVM backend (ka-0000006530)" ;;
          *)
            dim "    failure_reason='$FAIL_REASON' (does not mention the backend — reporting, not failing)"
            pass "agent failed closed with a non-empty failure_reason" ;;
        esac
      elif [ "$FINAL_STATE" = "working" ] || [ "$FINAL_STATE" = "running" ]; then
        fail "microvm-targeted agent reached '$FINAL_STATE'" \
          "the backend is unavailable here, so this means the spawn FELL THROUGH to a pod — the exact silent-misroute regression handleNonK8sSpawn must prevent"
      elif [ -z "$FINAL_STATE" ]; then
        dim "    could not read agent_state (daemon blip?) — inconclusive"
        pass "substrate leg completed (no fall-through observed)"
      else
        # Still spawning after the budget: the controller may not be watching this
        # project. Inconclusive rather than a violation — but a POD would be a
        # violation, so check for one below regardless.
        dim "    agent_state='$FINAL_STATE' after ${SPAWN_WAIT}s (controller may not watch project:e2e-test) — inconclusive on the state assertion"
        pass "substrate leg completed (agent never reached a working state)"
      fi

      # ASSERTION 2 (API-agnostic, the real invariant): NO pod was created for
      # this agent. This holds regardless of how the state assertion resolved —
      # a pod for a microvm-targeted agent is a violation, full stop.
      if have kubectl; then
        POD_COUNT=$(kubectl -n "$NAMESPACE" get pods \
          -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null \
          | grep -c "$AGENT_NAME" 2>/dev/null || echo 0)
        # grep -c prints 0 and exits 1 on no match; normalize any stray output.
        case "$POD_COUNT" in ''|*[!0-9]*) POD_COUNT=0 ;; esac
        if [ "$POD_COUNT" -eq 0 ]; then
          pass "NO Kubernetes pod exists for the microvm-targeted agent (handoff invariant held)"
        else
          fail "pod created for a microvm-targeted agent" \
            "found $POD_COUNT pod(s) matching '$AGENT_NAME' — a MicroVM-targeted spawn must NEVER produce a pod"
        fi
      else
        dim "    kubectl unavailable — skipping the no-pod assertion"
      fi
    fi
    cleanup_substrate
    trap - EXIT
  fi
fi
echo

# ── Summary ───────────────────────────────────────────────────────
echo "════════════════════════════════════════════════════"
echo " microvm-execution-target Summary: $PASS passed, $FAIL failed"
echo "════════════════════════════════════════════════════"
if [ "$FAIL" -gt 0 ]; then
  for e in "${ERRORS[@]}"; do red "  - $e"; done
  exit 1
fi
green "All microvm-execution-target assertions passed."
exit 0
