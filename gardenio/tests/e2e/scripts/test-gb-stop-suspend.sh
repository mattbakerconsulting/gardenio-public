#!/usr/bin/env bash
# test-gb-stop-suspend.sh — E2E: plain `gb stop` on a freeze-eligible runsc agent
# SUSPENDS (drives agent_state=stopping → AgentStop → controller `runsc
# checkpoint --leave-running` → persisted thaw-able snapshot), NOT the terminal
# done path. Bead ka-0000000910 (follow-up from decision ka-0000003208 on the
# merged gb-stop-suspend change ka-0000005754 / MR !4327; fast-hot-resume epic
# ka-0000003080).
#
# WHY THIS MODULE EXISTS (the gap it closes):
#   MR !4327 made `gb stop` SUSPEND a freeze-eligible agent: shouldSuspend()
#   (cmd/gb/stop.go) transitions agent_state=stopping (the SOLE AgentStop
#   trigger, subscriber/sse.go) WITHOUT CloseBead-to-done, WITHOUT setting
#   stop_requested, and WITHOUT requesting coop shutdown. The controller observes
#   the stopping transition (main.go handleEvent: AgentStop + ft.shouldFreeze),
#   runs `runsc checkpoint --leave-running`, persists a snapshot, records the
#   gardenio_freeze_latency_seconds histogram, and tears down Pod A.
#   The eligibility signal is BOAT_FREEZE_ELIGIBLE, injected by markFreezeEligible
#   (cmd/controller/podspec.go) into BOTH spec builders and read by gb stop via
#   freezeEligible() (cmd/gb/stop.go).
#
#   The merged change shipped with UNIT coverage (TestShouldSuspend,
#   TestFreezeEligible, podspec_freeze_eligible_test.go) but NO end-to-end test
#   of the actual gb-stop→bead→AgentStop→controller-freeze chain. The existing
#   gvisor e2e modules (test-gvisor-freeze-thaw.sh, test-resume-latency.sh,
#   test-gvisor-rehydration.sh, test-gvisor-rehydrate-ack.sh) all drive `runsc
#   checkpoint/restore` DIRECTLY at the substrate via the hostPID probe — NONE
#   exercise the gb stop → bead transition → AgentStop → controller-driven freeze
#   chain that !4327 introduced. So a regression in shouldSuspend /
#   markFreezeEligible / the stopping transition would NOT be caught by CI.
#   THIS module asserts that full chain end-to-end.
#
# WHAT THIS PROVES (against the live controller + beads + runsc node pool):
#   1. The controller injects BOAT_FREEZE_ELIGIBLE=true into a runsc + freeze/thaw
#      agent's pod env (guards markFreezeEligible regressions).
#   2. Plain `gb stop` (no --done/--clean) inside that agent SUSPENDS: the agent
#      bead transitions to agent_state=stopping, is NOT closed to done, and
#      stop_requested is NOT set (this is the distinction from the done path —
#      see cmd/gb/stop.go suspendAgent vs the terminal CloseBead branch).
#   3. The controller-driven freeze actually fired: a durable snapshot is
#      persisted AND the gardenio_freeze_latency_seconds histogram
#      (controller/internal/metrics/freeze_latency.go) records a fresh sample for
#      this freeze (count delta ≥ 1 on the step="total" series).
#   4. NEGATIVE CONTROL: `gb stop --done` on a sibling freeze-eligible agent takes
#      the UNCHANGED terminal done path — bead closes to agent_state=done, NO
#      stopping transition, and no incremental freeze_latency sample for it.
#      Proves suspend is GATED on plain `gb stop`, not universal.
#   5. SUCCESSION CHAIN (ka-0000002157): `gb stop --successor=new` on a freeze-
#      eligible agent OVERRIDES the suspend branch (stop.go:303 `&& !wantSuccessor
#      New`) — it mints a fresh successor agent BEFORE closing, then closes the
#      predecessor terminally with close_reason "superseded: continued as <id>".
#      The leg reads BOTH beads back from the daemon: the predecessor closed
#      (never stopping) with the superseded close_reason, and the successor OPEN
#      with prior_agent_id == predecessor id + a non-empty lineage_root_id. Because
#      the mint failure is warning-only (stop.go:350), a leg that trusted the exit
#      code would be BORN VACUOUS; reading the successor bead back is what makes it
#      real (persistent-agents Phase 1, task ka-0000006931; runtime 10395a980 +
#      834d21600 + a6c6c0de8).
#   6. VALIDATION: `gb stop --successor=bogus` exits non-zero and names the valid
#      enum (none/same/new) on stderr — pins ValidateSuccessor at the real CLI
#      boundary (applySuccessorFlag), which an in-process unit test cannot reach.
#
# HOW IT DRIVES THE REAL CHAIN (NOT a direct-runsc probe):
#   Unlike the sibling gvisor modules, this one does NOT drive `runsc` directly.
#   It seeds a REAL agent bead (type=agent) with runtime_class=runsc +
#   mock_scenario (claudeless, zero model spend) via the beads HTTP API, lets the
#   CONTROLLER spawn the runsc Pod A into the gvisor namespace, then runs `gb
#   stop` INSIDE that pod via `kubectl exec`. The assertions read the bead state
#   (beads HTTP API) and the controller's /metrics endpoint — the genuine
#   production surfaces, so a regression anywhere in the gb→bead→controller chain
#   surfaces here.
#
# OPT-IN / GATING (NEVER silently passes — same discipline as the siblings):
#   This module needs (a) the runsc gVisor substrate AND (b) a freeze/thaw-enabled
#   controller that watches the agent beads it seeds. It runs only when
#   GVISOR_E2E=1 (or GB_STOP_SUSPEND_E2E=1). When the opt-in flag is UNSET it
#   SKIPs cleanly (exit 0, inert in the standard shards). When the opt-in flag IS
#   SET but the substrate/controller is MISSING it SKIPs LOUD (still exit 0 so an
#   absent node pool does not red the suite, but a skip is NEVER mistaken for a
#   pass). Wire it in via:  GVISOR_E2E=1 ONLY=gb-stop-suspend run-shard.sh
#
# Prerequisites (gvisor-poc substrate per escalation ka-0000004674; controller
# freeze/thaw per the merged !4327 + FREEZE_THAW_ENABLED on the watching
# controller):
#   - kubectl context reaching the controller's release namespace + the gvisor ns
#   - RuntimeClass runsc present; gvisor namespace (PSA=privileged) create-pods
#   - a freeze/thaw-enabled controller (FREEZE_THAW_ENABLED=true) watching the
#     beads the test seeds — i.e. the same namespace whose controller +
#     beads daemon NAMESPACE points at.
#
# Usage:
#   GVISOR_E2E=1 ./gardenio/tests/e2e/scripts/test-gb-stop-suspend.sh
#   GVISOR_E2E=1 KEEP_BEADS=1 NAMESPACE=gardenio-e2e GVISOR_NS=gvisor-poc ./...
set -uo pipefail

# Shared gVisor substrate helpers — incl. gvisor_require_substrate_guard, the
# readiness-gate decision for a missing substrate (REQUIRE_SUBSTRATE=1 → hard
# fail; default → clean skip). Task ka-0000002867, lib decision ka-0000000122.
# shellcheck disable=SC1091
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib-gvisor-node.sh"

# ── Config (all overridable via env) ────────────────────────────────
# Release namespace: where the controller + beads daemon + /metrics live and
# where the seeded agent bead is watched. The controller ROUTES runsc agents
# into GVISOR_NS, so Pod A lands there even though the bead is watched here.
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
GVISOR_NS="${GVISOR_NS:-gvisor-poc}"
RUNTIME_CLASS="${RUNTIME_CLASS:-runsc}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
# Controller Service exposing /metrics on port 9092 (helm
# gardenio.agents.fullname → "<release>-agents", service.yaml port name=metrics).
CONTROLLER_SVC="${CONTROLLER_SVC:-${NAMESPACE}-agents}"
RUN_ID="${RUN_ID:-$$}"
PROJECT="${PROJECT:-gardenio}"
ROLE="${ROLE:-crew}"
MODE="${MODE:-crew}"
# Mock scenario so the spawned pod runs claudeless (zero model spend), not real
# Claude. Any scenario that boots + idles is fine; we drive gb stop ourselves.
MOCK_SCENARIO="${MOCK_SCENARIO:-basic-echo}"
# How long to wait for the controller to spawn Pod A (Ready) after seeding.
POD_READY_TIMEOUT="${POD_READY_TIMEOUT:-240}"
# How long to wait for the bead to reach agent_state=stopping after `gb stop`.
SUSPEND_WAIT="${SUSPEND_WAIT:-120}"
# How long to wait for the controller freeze to persist a snapshot + bump the
# freeze_latency histogram after the stopping transition.
FREEZE_WAIT="${FREEZE_WAIT:-180}"
# How long to wait for a `gb stop --successor=new` predecessor to close terminally
# with the "superseded: continued as <id>" close_reason (successor-chain leg).
SUCC_WAIT="${SUCC_WAIT:-120}"
GB_BIN="${GB_BIN:-gb}"

PASS=0
FAIL=0
ERRORS=()
CREATED_BEADS=()
# Successor agent beads minted by `gb stop --successor=new`. The mint copies
# neither mock_scenario NOR runtime_class from the predecessor, so a minted
# successor would boot a REAL (billed) claude pod if left open. We reap it the
# instant we finish asserting on it (reap_successor), and this list is the
# cleanup backstop for any we discovered but did not inline-delete.
SUCCESSOR_BEADS=()
PF_BEADS_PID=""
PF_CTRL_PID=""

export KD_ACTOR="${KD_ACTOR:-e2e-gb-stop-suspend}"

# ── Helpers ─────────────────────────────────────────────────────────
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

# SKIP loud — opt-in unset OR substrate/controller missing. Always exit 0 (an
# absent node pool / disabled controller must NOT red the suite) but print a
# banner so a skip is never mistaken for a pass. The "$2" tag distinguishes
# "opt-in off" (inert, expected in the standard shards) from "substrate missing"
# (loud — the gated dispatch expected it to assert).
skip_all() {
  local why="$1" tag="${2:-substrate}"
  # "loud" == opt-in SET but the substrate/controller is absent → shared
  # readiness guard (hard-fail under REQUIRE_SUBSTRATE=1, loud skip otherwise;
  # task ka-0000002867). "off" == opt-in unset: inert clean skip.
  if [ "$tag" = "loud" ]; then
    gvisor_require_substrate_guard gb-stop-suspend "$why"
  else
    blue "SKIP gb-stop-suspend: $why"
    dim "  Opt-in module: set GVISOR_E2E=1 (or GB_STOP_SUSPEND_E2E=1) and run on the"
    dim "  gvisor-poc substrate (runsc RuntimeClass) against a freeze/thaw-enabled"
    dim "  controller watching ns $NAMESPACE."
    if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
    exit 0
  fi
}

# ── Beads HTTP API plumbing (mirrors test-agent-lifecycle.sh) ───────
ensure_beads_token() {
  if [ -n "${BEADS_TOKEN:-}" ]; then return; fi
  local secret
  secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
    -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || echo "")
  [ -n "$secret" ] && export BEADS_TOKEN="$secret"
}

ensure_beads_pf() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then return; fi
  local port="${BEADS_PORT:-18080}"
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/dev/null 2>&1 &
  PF_BEADS_PID=$!
  sleep 3
  export BEADS_HTTP_URL="http://localhost:${port}"
  dim "  beads daemon at $BEADS_HTTP_URL (pid $PF_BEADS_PID)"
}

# beads_get <agentID> — print the bead JSON (empty on failure).
beads_get() {
  curl -sf --max-time 10 "${BEADS_HTTP_URL}/v1/beads/$1" \
    ${BEADS_TOKEN:+-H "Authorization: Bearer $BEADS_TOKEN"} 2>/dev/null || echo ""
}

# field <json> <fieldname> — extract a fields.<name> value via python (robust to
# the field-blob shape). Empty if absent.
field() {
  printf '%s' "$1" | python3 -c "
import sys,json
try:
  b=json.load(sys.stdin)
except Exception:
  print(''); sys.exit(0)
f=b.get('fields') or {}
print(f.get('$2',''))
" 2>/dev/null || echo ""
}

# top-level bead key (status, agent_state may be promoted to top-level too).
topkey() {
  printf '%s' "$1" | python3 -c "
import sys,json
try:
  b=json.load(sys.stdin)
except Exception:
  print(''); sys.exit(0)
print(b.get('$2',''))
" 2>/dev/null || echo ""
}

# agent_state, resilient to whether it sits at top-level or in fields.
agent_state_of() {
  local j="$1" st
  st=$(topkey "$j" agent_state)
  [ -z "$st" ] && st=$(field "$j" agent_state)
  printf '%s' "$st"
}

# close_reason lives in the field blob (successor mint writes it via
# closePredecessorAsSuperseded, cmd/gb/stop_successor.go). Resilient to a
# top-level promotion just in case.
close_reason_of() {
  local j="$1" cr
  cr=$(field "$j" close_reason)
  [ -z "$cr" ] && cr=$(topkey "$j" close_reason)
  printf '%s' "$cr"
}

# Parse the successor bead id out of a "superseded: continued as <id>"
# close_reason — mirrors successorIDFromCloseReason (internal/bridge/
# bot_agents_wrapup.go), the load-bearing cross-package contract. Echoes the
# trimmed successor id, or empty if the prefix does not match.
successor_id_from_close_reason() {
  local cr="$1" prefix="superseded: continued as "
  case "$cr" in
    "$prefix"*) printf '%s' "${cr#"$prefix"}" | tr -d '[:space:]' ;;
    *) printf '' ;;
  esac
}

# Seed a freeze-eligible runsc agent bead via the beads HTTP API. Echoes the
# bead id (empty on failure). agent_state=spawning so the controller's AgentSpawn
# path creates Pod A; runtime_class=runsc routes it to the gVisor ns + makes it
# freeze-eligible; mock_scenario keeps the pod claudeless (no model spend).
seed_runsc_agent() {
  local name="$1" body
  body=$(python3 -c "
import json
print(json.dumps({
  'type': 'agent',
  'title': '$name',
  'labels': ['e2e', 'gb-stop-suspend-${RUN_ID}', 'project:${PROJECT}'],
  'fields': {
    'agent': '$name',
    'agent_state': 'spawning',
    'mode': '$MODE',
    'role': '$ROLE',
    'project': '$PROJECT',
    'runtime_class': '$RUNTIME_CLASS',
    'mock_scenario': '$MOCK_SCENARIO',
  },
}))")
  curl -sf --max-time 10 -X POST "${BEADS_HTTP_URL}/v1/beads" \
    ${BEADS_TOKEN:+-H "Authorization: Bearer $BEADS_TOKEN"} \
    -H 'Content-Type: application/json' \
    -d "$body" 2>/dev/null \
    | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null \
    || echo ""
}

# Find the runsc Pod A the controller spawned for an agent bead. The pod name is
# "<mode>-<project>-<role>-<agentName>" (manager.go PodName, project prefix
# stripped from agentName), landing in the gVisor ns. We resolve by the agent
# label rather than reconstructing the name to stay robust to sanitization.
find_agent_pod() {
  local agent_name="$1"
  kubectl -n "$GVISOR_NS" get pods \
    -l "gardenio.io/agent=${agent_name}" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo ""
}

# Scrape the controller's freeze_latency histogram total count (step="total",
# summed across role/runtime/env label sets). Echoes an integer (0 if the metric
# is absent — e.g. no freeze has happened yet this controller lifetime).
freeze_total_count() {
  local port="${CTRL_PORT:-19092}" out
  out=$(curl -sf --max-time 10 "http://localhost:${port}/metrics" 2>/dev/null || echo "")
  printf '%s' "$out" | awk '
    /^gardenio_freeze_latency_seconds_count\{/ && /step="total"/ { sum += $NF }
    END { printf "%d", sum+0 }'
}

ensure_ctrl_pf() {
  local port="${CTRL_PORT:-19092}"
  kubectl -n "$NAMESPACE" port-forward "svc/${CONTROLLER_SVC}" "${port}:9092" \
    >/dev/null 2>&1 &
  PF_CTRL_PID=$!
  sleep 3
}

# reap_successor <successorID> — DELETE a minted successor agent bead the moment
# we finish asserting on it, so the controller never boots (or promptly tears
# down) its non-mock, non-runsc real-claude pod. Best-effort; also records the id
# in SUCCESSOR_BEADS as a cleanup backstop. Called inline in the positive leg AND
# repeated in cleanup() so a mid-run failure still reaps.
reap_successor() {
  local id="$1"
  [ -n "$id" ] || return 0
  SUCCESSOR_BEADS+=("$id")
  [ "${KEEP_BEADS:-0}" = "1" ] && return 0
  curl -sf --max-time 10 -X DELETE "${BEADS_HTTP_URL}/v1/beads/$id" \
    ${BEADS_TOKEN:+-H "Authorization: Bearer $BEADS_TOKEN"} >/dev/null 2>&1 || true
}

cleanup() {
  blue "── cleanup ──"
  # Reap successor beads FIRST (before dropping the port-forward) — a minted
  # successor has neither mock_scenario nor runtime_class, so a lingering one
  # boots a real billed pod. reap_successor is idempotent (DELETE of an already-
  # deleted id is a harmless 404).
  if [ "${KEEP_BEADS:-0}" != "1" ]; then
    for id in "${SUCCESSOR_BEADS[@]:-}"; do
      [ -n "$id" ] || continue
      curl -sf --max-time 10 -X DELETE "${BEADS_HTTP_URL}/v1/beads/$id" \
        ${BEADS_TOKEN:+-H "Authorization: Bearer $BEADS_TOKEN"} >/dev/null 2>&1 || true
    done
    for id in "${CREATED_BEADS[@]:-}"; do
      [ -n "$id" ] || continue
      # Close + delete the agent bead so the reconciler tears the pod down and
      # we leave no orphan. Best-effort; never fail the test on cleanup.
      curl -sf --max-time 10 -X DELETE "${BEADS_HTTP_URL}/v1/beads/$id" \
        ${BEADS_TOKEN:+-H "Authorization: Bearer $BEADS_TOKEN"} >/dev/null 2>&1 || true
    done
  else
    dim "  KEEP_BEADS=1 — leaving ${#CREATED_BEADS[@]} agent + ${#SUCCESSOR_BEADS[@]} successor bead(s) for debug"
  fi
  [ -n "$PF_BEADS_PID" ] && kill "$PF_BEADS_PID" >/dev/null 2>&1 || true
  [ -n "$PF_CTRL_PID" ] && kill "$PF_CTRL_PID" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "════════════════════════════════════════════════════"
echo " gb stop → SUSPEND (freeze) + SUCCESSOR-CHAIN on a freeze-eligible runsc agent — E2E"
echo " release-ns=$NAMESPACE  gvisor-ns=$GVISOR_NS  runtimeClass=$RUNTIME_CLASS"
echo " ka-0000000910 (epic ka-0000003080) + ka-0000002157 (successor=new)"
echo "════════════════════════════════════════════════════"

# ── Gate 0: opt-in + substrate/controller preflight (SKIP, never silently) ──
if [ "${GVISOR_E2E:-0}" != "1" ] && [ "${GB_STOP_SUSPEND_E2E:-0}" != "1" ]; then
  skip_all "GVISOR_E2E / GB_STOP_SUSPEND_E2E != 1 (opt-in module)" off
fi
command -v kubectl >/dev/null 2>&1 || skip_all "kubectl not installed" loud
command -v curl    >/dev/null 2>&1 || skip_all "curl not installed" loud
command -v python3 >/dev/null 2>&1 || skip_all "python3 not installed" loud
kubectl get runtimeclass "$RUNTIME_CLASS" >/dev/null 2>&1 \
  || skip_all "RuntimeClass '$RUNTIME_CLASS' not present on this cluster" loud
[ "$(kubectl auth can-i create pods -n "$GVISOR_NS" 2>/dev/null)" = "yes" ] \
  || skip_all "cannot create pods in gVisor ns '$GVISOR_NS'" loud
kubectl get ns "$NAMESPACE" >/dev/null 2>&1 \
  || skip_all "release ns '$NAMESPACE' (controller + beads) not present" loud

blue "Substrate present — driving the gb stop → suspend chain."

# ── 1. Reach the beads daemon + controller /metrics ─────────────────
blue "[1/6] reaching beads daemon + controller /metrics"
ensure_beads_token
ensure_beads_pf
# A reachable daemon answers the list endpoint (200) or at least connects; a hard
# connection failure (port-forward to svc/$BEADS_SVC could not be established) is
# the substrate-missing signal.
if ! curl -sf --max-time 10 "${BEADS_HTTP_URL}/v1/beads" \
     ${BEADS_TOKEN:+-H "Authorization: Bearer $BEADS_TOKEN"} >/dev/null 2>&1; then
  skip_all "beads daemon $BEADS_HTTP_URL unreachable (port-forward to svc/$BEADS_SVC failed)" loud
fi
pass "beads daemon reachable ($BEADS_HTTP_URL)"

ensure_ctrl_pf
# The freeze_latency metric may be absent until the first freeze — that is fine.
# We only require the /metrics endpoint to be reachable so the count delta is
# meaningful. An unreachable endpoint means we cannot assert acceptance #4 →
# skip loud rather than silently drop the metric assertion.
if ! curl -sf --max-time 10 "http://localhost:${CTRL_PORT:-19092}/metrics" >/dev/null 2>&1; then
  skip_all "controller /metrics (svc/$CONTROLLER_SVC :9092) unreachable — cannot assert freeze_latency sample" loud
fi
pass "controller /metrics reachable (svc/$CONTROLLER_SVC :9092)"

FREEZE_BEFORE=$(freeze_total_count)
dim "    freeze_latency total count baseline: $FREEZE_BEFORE"

# ── 2. Seed a freeze-eligible runsc agent + let the controller spawn it ─
blue "[2/6] seeding freeze-eligible runsc agent + awaiting controller spawn"
SUSPEND_AGENT="gbsusp-${RUN_ID}-s"
SUSPEND_ID=$(seed_runsc_agent "$SUSPEND_AGENT")
if [ -z "$SUSPEND_ID" ]; then
  fail "seed suspend agent bead" "beads POST returned no id"
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi
CREATED_BEADS+=("$SUSPEND_ID")
pass "seeded suspend agent bead ($SUSPEND_ID, runtime_class=runsc, mock=$MOCK_SCENARIO)"

# Wait for the controller to spawn Pod A (Ready) in the gVisor ns.
POD_A=""
deadline=$((SECONDS + POD_READY_TIMEOUT))
while [ "$SECONDS" -lt "$deadline" ]; do
  POD_A=$(find_agent_pod "$SUSPEND_AGENT")
  if [ -n "$POD_A" ]; then
    phase=$(kubectl -n "$GVISOR_NS" get pod "$POD_A" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
    [ "$phase" = "Running" ] && break
  fi
  sleep 5
done
if [ -z "$POD_A" ]; then
  fail "controller spawned runsc Pod A" "no pod with label gardenio.io/agent=$SUSPEND_AGENT in ns $GVISOR_NS within ${POD_READY_TIMEOUT}s"
  kubectl -n "$GVISOR_NS" get pods -l "gardenio.io/agent=${SUSPEND_AGENT}" -o wide 2>&1 | sed 's/^/    /'
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi
if ! kubectl -n "$GVISOR_NS" wait --for=condition=Ready "pod/$POD_A" --timeout=120s >/dev/null 2>&1; then
  fail "runsc Pod A Ready" "pod $POD_A did not become Ready"
  kubectl -n "$GVISOR_NS" get pod "$POD_A" -o wide 2>&1 | sed 's/^/    /'
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi
pass "controller spawned + Ready runsc Pod A ($POD_A in ns $GVISOR_NS)"

# ── 3. Assert the controller injected BOAT_FREEZE_ELIGIBLE=true ─────
# Guards markFreezeEligible (podspec.go): a runsc + freeze/thaw agent's pod env
# MUST carry the signal gb stop reads to choose SUSPEND. Absent → the in-pod
# suspend decision can never fire, regardless of the rest of the chain.
blue "[3/6] asserting controller injected BOAT_FREEZE_ELIGIBLE=true into the pod"
ELIGIBLE=$(kubectl -n "$GVISOR_NS" get pod "$POD_A" \
  -o jsonpath='{range .spec.containers[?(@.name=="agent")].env[?(@.name=="BOAT_FREEZE_ELIGIBLE")]}{.value}{end}' 2>/dev/null || echo "")
if [ "$ELIGIBLE" = "true" ]; then
  pass "BOAT_FREEZE_ELIGIBLE=true present in agent container env (markFreezeEligible fired)"
else
  fail "BOAT_FREEZE_ELIGIBLE=true in pod env" \
    "got '${ELIGIBLE:-<absent>}' — markFreezeEligible did not inject the freeze-eligible signal; gb stop would never SUSPEND"
fi

# ── 4. Run plain `gb stop` inside the agent → assert SUSPEND, not done ─
blue "[4/6] running plain 'gb stop' inside the agent + asserting agent_state=stopping"
# Exec gb stop in Pod A. The suspend path satisfies the decision gate itself, so
# a plain `gb stop` is enough; we ignore exec rc and assert on OBSERVED bead
# state (the controller may tear Pod A down mid-exec once it freezes).
kubectl -n "$GVISOR_NS" exec "$POD_A" -c agent -- \
  "$GB_BIN" stop --wrapup '{"tldr":"e2e suspend probe","accomplishments":"gb-stop-suspend e2e"}' \
  >/dev/null 2>&1 || true

SAW_STOPPING=0
state=""
deadline=$((SECONDS + SUSPEND_WAIT))
while [ "$SECONDS" -lt "$deadline" ]; do
  j=$(beads_get "$SUSPEND_ID")
  state=$(agent_state_of "$j")
  st_status=$(topkey "$j" status)
  if [ "$state" = "stopping" ]; then SAW_STOPPING=1; break; fi
  # If it closed straight to done, the suspend path did NOT fire — fail fast.
  if [ "$state" = "done" ] || [ "$st_status" = "closed" ]; then break; fi
  sleep 3
done

if [ "$SAW_STOPPING" = 1 ]; then
  pass "agent transitioned to agent_state=stopping (the sole AgentStop trigger)"
else
  fail "agent_state=stopping after plain 'gb stop'" \
    "observed agent_state='${state:-<none>}' — plain gb stop did NOT SUSPEND (suspendAgent/shouldSuspend regression?)"
fi

# ASSERT NOT done + stop_requested NOT set — the distinction from the done path.
j=$(beads_get "$SUSPEND_ID")
state=$(agent_state_of "$j")
st_status=$(topkey "$j" status)
stop_req=$(field "$j" stop_requested)
if [ "$state" != "done" ] && [ "$st_status" != "closed" ]; then
  pass "bead NOT closed to done (suspend keeps the bead open + thaw-able)"
else
  fail "bead not closed to done" "agent_state='$state' status='$st_status' — suspend must NOT CloseBead"
fi
if [ "$stop_req" != "true" ]; then
  pass "stop_requested NOT set (suspend path skips the entrypoint done loop)"
else
  fail "stop_requested unset on suspend" \
    "stop_requested='true' — suspendAgent must NOT set it (it drives the terminal done path)"
fi

# ── 5. Assert the controller froze: snapshot persisted + freeze metric sample ─
blue "[5/6] asserting controller freeze fired (snapshot persisted + freeze_latency sample)"
# The headline durability proof is the freeze_latency histogram: the controller
# records step="total" ONLY after freeze() returns having persisted the Snapshot
# (main.go handleEvent default branch). A count delta ≥ 1 means a freeze durably
# persisted a snapshot during this run.
FREEZE_AFTER=0
deadline=$((SECONDS + FREEZE_WAIT))
while [ "$SECONDS" -lt "$deadline" ]; do
  FREEZE_AFTER=$(freeze_total_count)
  [ "$FREEZE_AFTER" -gt "$FREEZE_BEFORE" ] 2>/dev/null && break
  sleep 5
done
if [ "$FREEZE_AFTER" -gt "$FREEZE_BEFORE" ] 2>/dev/null; then
  pass "gardenio_freeze_latency_seconds (step=total) recorded a fresh sample ($FREEZE_BEFORE → $FREEZE_AFTER): snapshot durably persisted"
else
  fail "freeze_latency histogram sample" \
    "step=total count did not advance ($FREEZE_BEFORE → $FREEZE_AFTER) within ${FREEZE_WAIT}s — controller freeze (runsc checkpoint --leave-running → persist) did not fire"
fi

# Corroborating durable-snapshot signal: Pod A is torn down only AFTER freeze()
# persists the snapshot (teardown-safety ordering, main.go). Its disappearance
# after a stopping transition is the observable proof the freeze→teardown path
# ran to completion. (If freeze had failed, the pod would still be torn down, so
# this is corroborating, not the headline — the metric delta above is.)
if [ "$SAW_STOPPING" = 1 ]; then
  gone=0
  for _ in 1 2 3 4 5 6 7 8 9 10 11 12; do
    p=$(kubectl -n "$GVISOR_NS" get pod "$POD_A" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
    if [ -z "$p" ] || [ "$p" = "Succeeded" ]; then gone=1; break; fi
    sleep 5
  done
  [ "$gone" = 1 ] \
    && pass "Pod A torn down after the persisted checkpoint (freeze→teardown ordering held)" \
    || dim  "    Pod A ($POD_A) still present — teardown may lag; metric delta is the durability proof"
fi

# ── 6. NEGATIVE CONTROL: `gb stop --done` takes the unchanged terminal path ─
# Prove SUSPEND is GATED on plain gb stop, not universal: a sibling freeze-
# eligible agent stopped with --done must close to agent_state=done, NOT
# transition to stopping, and NOT add a freeze_latency sample.
blue "[6/6] negative control: 'gb stop --done' on a freeze-eligible agent → terminal done, no freeze"
DONE_AGENT="gbsusp-${RUN_ID}-d"
DONE_ID=$(seed_runsc_agent "$DONE_AGENT")
if [ -z "$DONE_ID" ]; then
  fail "seed negative-control agent bead" "beads POST returned no id"
else
  CREATED_BEADS+=("$DONE_ID")
  POD_D=""
  deadline=$((SECONDS + POD_READY_TIMEOUT))
  while [ "$SECONDS" -lt "$deadline" ]; do
    POD_D=$(find_agent_pod "$DONE_AGENT")
    if [ -n "$POD_D" ]; then
      phase=$(kubectl -n "$GVISOR_NS" get pod "$POD_D" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
      [ "$phase" = "Running" ] && break
    fi
    sleep 5
  done
  if [ -z "$POD_D" ]; then
    fail "negative-control Pod spawned" "no pod for $DONE_AGENT in ns $GVISOR_NS within ${POD_READY_TIMEOUT}s"
  else
    kubectl -n "$GVISOR_NS" wait --for=condition=Ready "pod/$POD_D" --timeout=120s >/dev/null 2>&1 || true
    NEG_FREEZE_BEFORE=$(freeze_total_count)
    kubectl -n "$GVISOR_NS" exec "$POD_D" -c agent -- \
      "$GB_BIN" stop --done --wrapup '{"tldr":"e2e negative control","accomplishments":"gb-stop --done terminal path"}' \
      >/dev/null 2>&1 || true

    SAW_DONE=0
    saw_stopping_neg=0
    nstate=""
    deadline=$((SECONDS + SUSPEND_WAIT))
    while [ "$SECONDS" -lt "$deadline" ]; do
      j=$(beads_get "$DONE_ID")
      nstate=$(agent_state_of "$j")
      nstatus=$(topkey "$j" status)
      [ "$nstate" = "stopping" ] && saw_stopping_neg=1
      if [ "$nstate" = "done" ] || [ "$nstatus" = "closed" ]; then SAW_DONE=1; break; fi
      sleep 3
    done
    if [ "$SAW_DONE" = 1 ] && [ "$saw_stopping_neg" = 0 ]; then
      pass "gb stop --done → agent_state=done (terminal path), never transited stopping"
    elif [ "$saw_stopping_neg" = 1 ]; then
      fail "negative control terminal done" "--done agent transited stopping — SUSPEND is NOT gated on plain gb stop (--done leaked into the suspend path)"
    else
      fail "negative control terminal done" "observed agent_state='${nstate:-<none>}' — gb stop --done did not reach the terminal done path"
    fi

    # No incremental freeze sample for the --done agent (allow for an unrelated
    # concurrent freeze by requiring the count to NOT advance during a short
    # settle window AFTER done was observed).
    sleep 10
    NEG_FREEZE_AFTER=$(freeze_total_count)
    if [ "$NEG_FREEZE_AFTER" -le "$NEG_FREEZE_BEFORE" ] 2>/dev/null; then
      pass "no freeze_latency sample attributable to the --done agent ($NEG_FREEZE_BEFORE → $NEG_FREEZE_AFTER): done path takes no snapshot"
    else
      dim  "    freeze_latency advanced ($NEG_FREEZE_BEFORE → $NEG_FREEZE_AFTER) during the --done window"
      dim  "    (non-gating: another agent may have frozen concurrently; the --done bead reaching done with NO stopping transition is the authoritative negative proof)"
    fi
  fi
fi

# ════════════════════════════════════════════════════════════════════
# SUCCESSION CHAIN — `gb stop --successor=new` (ka-0000002157)
# ════════════════════════════════════════════════════════════════════
# WHAT THESE TWO LEGS ADD (over the suspend legs above):
#   The suspend legs prove plain `gb stop` SUSPENDS a freeze-eligible runsc
#   agent. This section proves the OTHER branch merged for persistent-agents
#   Phase 1 (task ka-0000006931; runtime 10395a980 + 834d21600 + a6c6c0de8):
#   `gb stop --successor=new` OVERRIDES that suspend branch on the SAME freeze-
#   eligible agent — it mints a fresh successor agent BEFORE closing, then closes
#   the predecessor terminally with close_reason "superseded: continued as <id>".
#
# WHY AN E2E IS REQUIRED (not just the existing stop_successor_test.go units):
#   The mint failure is SWALLOWED as a warning (cmd/gb/stop.go:350-352:
#   `Warning: successor=new mint failed, closing without continuation` → falls
#   through to a plain terminal close). So if `prior_agent_id` / `lineage_root_id`
#   ever drift out of the type:agent builtin schema (declared in BOTH
#   kbeads/internal/server/config.go AND controller/internal/bridge/init.go),
#   mintSuccessorAgent 400s against a REAL daemon, the operator still sees a normal
#   done card, the continuation is silently lost, and NOTHING reddens. The unit
#   tests are fake-daemon only — none write to a real daemon, so none catch a
#   builtin-schema drift. THIS is that missing coverage.
#
# ANTI-VACUITY (the trap the task names explicitly):
#   Because the mint failure is warning-only, a leg that checks only `gb stop`'s
#   exit code is BORN VACUOUS — it passes on a broken system. So the positive leg
#   below does NOT trust the exit code: it reads the PREDECESSOR bead back from
#   the daemon, parses the successor id out of its close_reason, then reads the
#   SUCCESSOR bead back and asserts the lineage stamps. If the mint silently
#   failed, the predecessor closes with a PLAIN "done" (no "superseded:" prefix),
#   successor_id_from_close_reason returns empty, and the leg reddens. That is the
#   built-in negative control, exercised by MUTATION in the sibling reddens-proof
#   test (see PROOF packet): comment out the prior_agent_id line in the builtin
#   and this leg goes red exactly as designed.

# ── 7. SUCCESSION CHAIN: `gb stop --successor=new` (+ --successor=bogus reject) ──
blue "[7/7] gb stop --successor=new: mint successor + supersede predecessor (override suspend)"
SUCC_HEIR=""   # set below once parsed from the predecessor close_reason (summary-safe under set -u)
SUCC_AGENT="gbsucc-${RUN_ID}-s"
SUCC_ID=$(seed_runsc_agent "$SUCC_AGENT")
if [ -z "$SUCC_ID" ]; then
  fail "seed successor-chain predecessor bead" "beads POST returned no id"
else
  CREATED_BEADS+=("$SUCC_ID")
  # Wait for the controller to spawn + ready the freeze-eligible predecessor pod,
  # so `gb stop --successor=new` runs on a genuinely freeze-eligible agent — that
  # is what makes the "override the suspend branch" assertion (leg c) load-bearing
  # rather than trivially true.
  POD_S=""
  deadline=$((SECONDS + POD_READY_TIMEOUT))
  while [ "$SECONDS" -lt "$deadline" ]; do
    POD_S=$(find_agent_pod "$SUCC_AGENT")
    if [ -n "$POD_S" ]; then
      phase=$(kubectl -n "$GVISOR_NS" get pod "$POD_S" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
      [ "$phase" = "Running" ] && break
    fi
    sleep 5
  done
  if [ -z "$POD_S" ]; then
    fail "successor-chain predecessor Pod spawned" "no pod for $SUCC_AGENT in ns $GVISOR_NS within ${POD_READY_TIMEOUT}s"
  else
    kubectl -n "$GVISOR_NS" wait --for=condition=Ready "pod/$POD_S" --timeout=120s >/dev/null 2>&1 || true

    # Confirm the predecessor is freeze-eligible — otherwise the "successor=new
    # beat the suspend branch" assertion (leg c) is vacuous (a runc agent would
    # take the terminal path anyway). This mirrors step [3/6].
    ELIG_S=$(kubectl -n "$GVISOR_NS" get pod "$POD_S" \
      -o jsonpath='{range .spec.containers[?(@.name=="agent")].env[?(@.name=="BOAT_FREEZE_ELIGIBLE")]}{.value}{end}' 2>/dev/null || echo "")
    if [ "$ELIG_S" = "true" ]; then
      pass "successor-chain predecessor is FREEZE-ELIGIBLE (so the override is load-bearing)"
    else
      fail "successor-chain predecessor freeze-eligible" \
        "BOAT_FREEZE_ELIGIBLE='${ELIG_S:-<absent>}' — without it the successor=new override assertion (leg c) is vacuous"
    fi

    # ── NEGATIVE/VALIDATION leg (run BEFORE the destructive stop) ──────
    # `gb stop --successor=bogus` MUST be rejected at the REAL CLI boundary
    # (ValidateSuccessor via applySuccessorFlag, stop.go:96) — an in-process unit
    # test structurally cannot reach this: it never crosses the cobra flag-parse +
    # process-exit boundary. An unknown --successor must (1) exit NON-ZERO and (2)
    # name the valid enum (none/same/new) on stderr. We run it HERE, inside the
    # live freeze-eligible predecessor pod, BEFORE the real --successor=new stop:
    # applySuccessorFlag rejects BEFORE agent-id-gated mutation / any close, so the
    # bead stays pristine and POD_S is still valid for the real stop below. (Doing
    # it after would have to reuse a torn-down POD_A/POD_D and fail for the wrong
    # reason.)
    val_out=$(kubectl -n "$GVISOR_NS" exec "$POD_S" -c agent -- \
      "$GB_BIN" stop --successor=bogus \
        --wrapup '{"tldr":"e2e successor validation","accomplishments":"should never reach close"}' 2>&1)
    val_rc=$?
    if [ "$val_rc" -ne 0 ]; then
      pass "gb stop --successor=bogus exited non-zero (rc=$val_rc): the stop was refused at the CLI boundary"
    else
      fail "gb stop --successor=bogus non-zero exit" \
        "rc=0 — an invalid successor was ACCEPTED; ValidateSuccessor did not reject at the CLI boundary"
    fi
    # stderr must name the accepted enum. ValidateSuccessor emits
    # 'invalid successor "bogus" — must be one of none, same, new'.
    if grep -qiE 'invalid.*successor' <<<"$val_out" \
       && grep -q 'none' <<<"$val_out" \
       && grep -q 'same' <<<"$val_out" \
       && grep -q 'new' <<<"$val_out"; then
      pass "rejection stderr names the valid enum (none/same/new)"
    else
      fail "rejection names valid enum" \
        "stderr did not name the none/same/new enum: $(printf '%s' "$val_out" | tr '\n' ' ' | head -c 300)"
    fi
    # Sanity: the rejected validation must NOT have mutated the predecessor bead —
    # it should still be open (not closed, not stopping). If it drifted, the real
    # stop below would assert against a dirty starting state.
    vj=$(beads_get "$SUCC_ID")
    v_status=$(topkey "$vj" status)
    v_state=$(agent_state_of "$vj")
    if [ "$v_status" != "closed" ] && [ "$v_state" != "stopping" ] && [ "$v_state" != "done" ]; then
      pass "rejected --successor=bogus left the predecessor bead pristine (status=$v_status state=${v_state:-open})"
    else
      fail "rejected validation left bead pristine" \
        "predecessor mutated by the rejected stop (status='$v_status' state='$v_state') — applySuccessorFlag rejected too late"
    fi

    # Drive `gb stop --successor=new` INSIDE the freeze-eligible predecessor pod.
    # accomplishments present → this is a DONE wrap-up, so the decision gate is
    # carved out (decisionGateRequired/isDoneWrapUp) and a plain in-pod stop is
    # accepted. We ignore exec rc and assert on OBSERVED daemon state — the mint
    # + close race the pod teardown, and (critically) the mint failure is warning-
    # only so exec rc is NOT evidence either way.
    kubectl -n "$GVISOR_NS" exec "$POD_S" -c agent -- \
      "$GB_BIN" stop --successor=new \
        --wrapup '{"tldr":"e2e successor-chain probe","accomplishments":"gb-stop --successor=new e2e","handoff_notes":"continue the e2e successor-chain probe"}' \
      >/dev/null 2>&1 || true

    # Poll the PREDECESSOR until it closes terminally. The override forces the
    # terminal path (stop.go:303 `if shouldSuspend() && !wantSuccessorNew`), so a
    # freeze-eligible predecessor must close to done — NOT transition to stopping.
    saw_stopping_succ=0
    pred_state=""
    pred_status=""
    pred_close=""
    deadline=$((SECONDS + SUCC_WAIT))
    while [ "$SECONDS" -lt "$deadline" ]; do
      j=$(beads_get "$SUCC_ID")
      pred_state=$(agent_state_of "$j")
      pred_status=$(topkey "$j" status)
      pred_close=$(close_reason_of "$j")
      [ "$pred_state" = "stopping" ] && saw_stopping_succ=1
      if [ "$pred_status" = "closed" ] || [ "$pred_state" = "done" ]; then break; fi
      sleep 3
    done

    # (c) OVERRIDE: the freeze-eligible predecessor took the terminal path, NOT
    # the suspend branch. This is the assertion that PINS successor=new beating
    # shouldSuspend() — the whole reason stop.go:303 has the `&& !wantSuccessorNew`
    # guard. If successor=new had NOT overridden suspend, this agent (freeze-
    # eligible) would have transited stopping like the [4/6] suspend leg did.
    if [ "$saw_stopping_succ" = 0 ] && { [ "$pred_status" = "closed" ] || [ "$pred_state" = "done" ]; }; then
      pass "successor=new OVERRODE suspend: freeze-eligible predecessor closed terminally, never transited stopping"
    elif [ "$saw_stopping_succ" = 1 ]; then
      fail "successor=new override of suspend" \
        "predecessor transited agent_state=stopping — successor=new did NOT override the suspend branch (stop.go:303 '&& !wantSuccessorNew' regression)"
    else
      fail "successor=new predecessor closed terminally" \
        "observed agent_state='${pred_state:-<none>}' status='${pred_status:-<none>}' within ${SUCC_WAIT}s — predecessor never reached the terminal close"
    fi

    # (b) SUPERSEDED CLOSE: the predecessor's close_reason matches
    # "^superseded: continued as kd-" — the load-bearing cross-package contract
    # the bridge card-chaining renderer parses (successorIDFromCloseReason). This
    # is ALSO the anti-vacuity guard: a silently-failed mint closes with a plain
    # "done" and NO "superseded:" prefix, so SUCC_HEIR comes back empty below and
    # every successor assertion reddens.
    SUCC_HEIR=$(successor_id_from_close_reason "$pred_close")
    if grep -Eq '^superseded: continued as kd-' <<<"$pred_close"; then
      pass "predecessor closed with superseded close_reason ('$pred_close')"
    else
      fail "predecessor superseded close_reason" \
        "close_reason='${pred_close:-<empty>}' does not match '^superseded: continued as kd-' — mint likely failed and fell through to a PLAIN close (the warning-only failure the operator would never see)"
    fi

    # (a) SUCCESSOR EXISTS + carries the lineage stamps. Read the successor bead
    # back from the daemon (NOT the exit code) and assert BY SHAPE — the mint id
    # is nondeterministic, so we never assert a literal id:
    #   - successor id has the kd- prefix AND != predecessor id
    #   - prior_agent_id == predecessor id (the one lineage back-pointer)
    #   - lineage_root_id is NON-EMPTY (anchors the chain; == predecessor id here
    #     because the predecessor was itself a root — no inherited lineage_root_id)
    #   - the successor is OPEN (a real continuation, not a closed stub)
    if [ -n "$SUCC_HEIR" ]; then
      # Reap the successor the instant we have its id — it has neither
      # mock_scenario nor runtime_class, so leaving it open boots a real pod.
      hj=$(beads_get "$SUCC_HEIR")
      h_prior=$(field "$hj" prior_agent_id)
      h_root=$(field "$hj" lineage_root_id)
      h_type=$(topkey "$hj" type)
      h_status=$(topkey "$hj" status)
      reap_successor "$SUCC_HEIR"

      if [ -z "$hj" ] || [ "$hj" = "" ]; then
        fail "successor bead readable from daemon" "GET $SUCC_HEIR returned empty — the id parsed from close_reason does not resolve to a bead"
      else
        pass "successor bead exists + readable from daemon ($SUCC_HEIR, type=${h_type:-?})"
      fi

      case "$SUCC_HEIR" in
        kd-*)
          if [ "$SUCC_HEIR" != "$SUCC_ID" ]; then
            pass "successor id is a fresh kd- bead distinct from the predecessor ($SUCC_HEIR != $SUCC_ID)"
          else
            fail "successor id distinct from predecessor" "successor id == predecessor id ($SUCC_ID) — no fresh bead was minted"
          fi ;;
        *)
          fail "successor id shape" "successor id '$SUCC_HEIR' lacks the kd- prefix" ;;
      esac

      if [ "$h_prior" = "$SUCC_ID" ]; then
        pass "successor prior_agent_id == predecessor id (lineage back-pointer stamped: $h_prior)"
      else
        fail "successor prior_agent_id stamp" \
          "prior_agent_id='${h_prior:-<empty>}' != predecessor '$SUCC_ID' — the lineage back-pointer drifted out of the builtin (400-on-mint class) OR was not stamped"
      fi

      if [ -n "$h_root" ]; then
        pass "successor lineage_root_id is non-empty (chain anchored: $h_root)"
      else
        fail "successor lineage_root_id stamp" \
          "lineage_root_id is empty — the chain anchor drifted out of the builtin schema OR was not stamped"
      fi

      if [ "$h_status" != "closed" ]; then
        pass "successor bead is OPEN (a live continuation, not a closed stub)"
      else
        fail "successor bead open" "successor status='$h_status' — a superseded successor should be an open continuation"
      fi
    else
      # No parsable successor id → every successor-side assertion is unprovable.
      # Emit them as explicit FAILs (not skips) so a silently-failed mint reddens
      # loudly rather than quietly dropping the coverage.
      fail "successor bead exists" "no successor id parsed from predecessor close_reason — mint failed silently (warning-only path) or close_reason contract drifted"
      fail "successor lineage stamps" "cannot verify prior_agent_id / lineage_root_id without a successor id"
    fi
  fi
fi

# ── Summary ─────────────────────────────────────────────────────────
echo ""
echo "════════════════════════════════════════════════════"
echo " gb-stop-suspend Summary: $PASS passed, $FAIL failed"
echo "   suspend agent=$SUSPEND_ID  freeze_count $FREEZE_BEFORE → ${FREEZE_AFTER:-?}"
echo "   successor-chain predecessor=${SUCC_ID:-?}  heir=${SUCC_HEIR:-<none>}"
echo "════════════════════════════════════════════════════"
if [ "$FAIL" -gt 0 ]; then
  for e in "${ERRORS[@]}"; do red "  - $e"; done
  exit 1
fi
green "All gb-stop-suspend assertions passed — plain 'gb stop' SUSPENDS, --successor=new CHAINS, --successor=bogus is REJECTED."
exit 0
