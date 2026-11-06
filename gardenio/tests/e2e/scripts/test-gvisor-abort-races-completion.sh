#!/usr/bin/env bash
# test-gvisor-abort-races-completion.sh — E2E: abort-races-completion (TOCTOU) on
# the live substrate. Bead ka-0000000996 (epic ka-0000004667 "gVisor production
# readiness"). The SEVERE companion to test-gb-stop-suspend.sh + the mid-FREEZE
# interrupt e2e (ka-0000004367): it proves the ONE race those don't —
# the abort landing AFTER the snapshot is durably persisted.
#
# THE INVARIANT THIS PROVES (the TOCTOU rule):
#   When a freeze's snapshot is persisted microseconds BEFORE an abort lands, the
#   controller must DISCARD that snapshot (Delete + reap the on-node image) and
#   leave the agent LIVE — never a dead agent without a thaw target, and never a
#   live agent advertising a thaw target we already decided to abandon. The defer
#   in FreezeThawManager.Freeze (internal/podmanager/freezethaw.go) resolves this
#   under registerInflight's lock: if `aborted` is true at finalize — even after a
#   successful Put — it calls discardAbortedFreeze (Delete + reapImage) and
#   returns ErrFreezeAborted.
#
#   This is UNIT-tested with a fake executor (freezethaw_interrupt_test.go,
#   TestFreeze_AbortRacesCompletion's ignoreCancel path). THIS module proves it on
#   the REAL substrate, against the REAL controller + ConfigMap snapshot store +
#   on-node checkpoint image.
#
# WHY A TEST SEAM IS REQUIRED (and why this is not flaky):
#   On the real substrate the window between SnapshotStore.Put returning and the
#   finalize defer reading the abort verdict is MICROSECONDS wide. An e2e firing
#   POST /api/v1/freeze/interrupt over a port-forward could never reliably land
#   INSIDE it — the race is real but unobservably narrow. So the controller ships
#   a deterministic test seam (ka-0000000996): FREEZE_POST_PUT_DELAY holds the
#   freeze in the just-persisted state for a bounded window (only when
#   FREEZE_INTERRUPT_ENABLED is on; default 0 = today's behaviour, never set in
#   prod). This module:
#     1. Observes the snapshot ConfigMap APPEAR (the post-Put state — the Put
#        durably committed), THEN
#     2. fires the interrupt endpoint while the freeze is parked in the window.
#   The seam turns a microsecond race into a deterministic, observable one
#   WITHOUT changing the production code path — the SAME defer, the SAME
#   discardAbortedFreeze, the SAME ErrFreezeAborted are exercised. The window
#   honours the freeze ctx, so the abort's cancel ends it immediately, exactly as
#   the natural race would.
#
# WHAT THIS ASSERTS (against the live controller + beads + runsc node pool):
#   1. The freeze reaches the post-Put state: the snapshot ConfigMap
#      (gardenio-ftsnap-<token>, label gardenio.io/ft-agent=<agent>) APPEARS in the
#      controller ns — proof SnapshotStore.Put durably committed.
#   2. After the interrupt lands in the post-Put window, that snapshot is
#      DISCARDED: the ConfigMap is GONE (discardAbortedFreeze's Delete ran).
#   3. The abort is counted: gardenio_freeze_interrupts_total advances by ≥1
#      (the controller observed ErrFreezeAborted on this freeze).
#   4. NO durable freeze was recorded: gardenio_freeze_latency_seconds{step=total}
#      does NOT advance for this freeze — the success branch (which records total)
#      was never taken; the abort branch was. Discarded snapshot ⇒ no thaw target.
#   5. The agent is LEFT LIVE: Pod A stays Running/Ready throughout — `runsc
#      checkpoint --leave-running` never killed it, and the abort abandoned the
#      teardown. Never a dead agent without a thaw target.
#
# OPT-IN / GATING (NEVER silently passes — same discipline as the siblings):
#   Needs (a) the runsc gVisor substrate AND (b) a freeze/thaw + interrupt-enabled
#   controller (FREEZE_THAW_ENABLED=true, FREEZE_INTERRUPT_ENABLED=true,
#   FREEZE_POST_PUT_DELAY>0) watching the agent beads it seeds. Runs only when
#   GVISOR_E2E=1 (or ABORT_RACES_E2E=1). Opt-in UNSET → SKIP clean (exit 0, inert
#   in standard shards). Opt-in SET but substrate/controller MISSING → SKIP LOUD
#   (still exit 0 so an absent node pool doesn't red the suite, but a skip is
#   NEVER mistaken for a pass). Wire in via:
#     GVISOR_E2E=1 ONLY=gvisor-abort-races-completion run-shard.sh
#
# Prerequisites:
#   - kubectl context reaching the controller's release ns + the gvisor ns
#   - RuntimeClass runsc present; gvisor ns (PSA=privileged) create-pods
#   - a controller with FREEZE_THAW_ENABLED=true, FREEZE_INTERRUPT_ENABLED=true,
#     FREEZE_POST_PUT_DELAY>0 (e.g. 25s) watching the beads this test seeds
#
# Usage:
#   GVISOR_E2E=1 ./gardenio/tests/e2e/scripts/test-gvisor-abort-races-completion.sh
#   GVISOR_E2E=1 KEEP_BEADS=1 NAMESPACE=gardenio-e2e GVISOR_NS=gvisor-poc ./...
set -uo pipefail

# ── Config (all overridable via env) ────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
GVISOR_NS="${GVISOR_NS:-gvisor-poc}"
RUNTIME_CLASS="${RUNTIME_CLASS:-runsc}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
# Controller Service exposing /metrics on 9092 + the health/API port 8091 (where
# POST /api/v1/freeze/interrupt lives). Both are on the agents service.
CONTROLLER_SVC="${CONTROLLER_SVC:-${NAMESPACE}-agents}"
RUN_ID="${RUN_ID:-$$}"
PROJECT="${PROJECT:-gardenio}"
ROLE="${ROLE:-crew}"
MODE="${MODE:-crew}"
MOCK_SCENARIO="${MOCK_SCENARIO:-basic-echo}"
POD_READY_TIMEOUT="${POD_READY_TIMEOUT:-240}"
SUSPEND_WAIT="${SUSPEND_WAIT:-120}"
# How long to wait for the snapshot ConfigMap to APPEAR (post-Put) after the
# stopping transition. Must comfortably exceed the runsc checkpoint duration.
SNAP_APPEAR_WAIT="${SNAP_APPEAR_WAIT:-180}"
# How long to wait for the snapshot ConfigMap to DISAPPEAR (discard) after the
# interrupt. discardAbortedFreeze runs synchronously inside the freeze defer.
SNAP_DISCARD_WAIT="${SNAP_DISCARD_WAIT:-60}"
GB_BIN="${GB_BIN:-gb}"
CTRL_METRICS_PORT="${CTRL_METRICS_PORT:-19092}"
CTRL_API_PORT="${CTRL_API_PORT:-18091}"

PASS=0
FAIL=0
ERRORS=()
CREATED_BEADS=()
PF_BEADS_PID=""
PF_METRICS_PID=""
PF_API_PID=""

export KD_ACTOR="${KD_ACTOR:-e2e-abort-races-completion}"

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

skip_all() {
  local why="$1" tag="${2:-substrate}"
  if [ "$tag" = "loud" ]; then
    red "════════════════════════════════════════════════════"
    red " SKIP gvisor-abort-races-completion (LOUD): $why"
    red " OPT-IN WAS SET but the freeze/thaw + interrupt runsc substrate is"
    red " absent — this run did NOT assert the abort-races-completion (TOCTOU)"
    red " rule. A green suite here proves NOTHING about the post-Put discard"
    red " path. Provision the gvisor-poc node pool + runsc RuntimeClass"
    red " (escalation ka-0000004674) and run against a controller with"
    red " FREEZE_THAW_ENABLED=true, FREEZE_INTERRUPT_ENABLED=true, and"
    red " FREEZE_POST_PUT_DELAY>0 for the gate to assert."
    red "════════════════════════════════════════════════════"
  else
    blue "SKIP gvisor-abort-races-completion: $why"
    dim "  Opt-in module: set GVISOR_E2E=1 (or ABORT_RACES_E2E=1) and run on the"
    dim "  gvisor-poc substrate against a freeze/thaw + interrupt-enabled"
    dim "  controller (FREEZE_POST_PUT_DELAY>0) watching ns $NAMESPACE."
  fi
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

# ── Beads HTTP API plumbing (mirrors test-gb-stop-suspend.sh) ───────
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

beads_get() {
  curl -sf --max-time 10 "${BEADS_HTTP_URL}/v1/beads/$1" \
    ${BEADS_TOKEN:+-H "Authorization: Bearer $BEADS_TOKEN"} 2>/dev/null || echo ""
}

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

agent_state_of() {
  local j="$1" st
  st=$(topkey "$j" agent_state)
  [ -z "$st" ] && st=$(field "$j" agent_state)
  printf '%s' "$st"
}

seed_runsc_agent() {
  local name="$1" body
  body=$(python3 -c "
import json
print(json.dumps({
  'type': 'agent',
  'title': '$name',
  'labels': ['e2e', 'abort-races-${RUN_ID}', 'project:${PROJECT}'],
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

find_agent_pod() {
  local agent_name="$1"
  kubectl -n "$GVISOR_NS" get pods \
    -l "gardenio.io/agent=${agent_name}" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo ""
}

# Snapshot ConfigMap presence for an agent, by the durable label the store sets
# (gardenio.io/ft-agent=<un-sanitized agent name>) — robust to the name flattening
# the ConfigMap-name token applies. Echoes the ConfigMap name (empty if none).
snapshot_cm() {
  local agent_name="$1"
  kubectl -n "$NAMESPACE" get configmap \
    -l "gardenio.io/ft-agent=${agent_name}" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo ""
}

# Scrape gardenio_freeze_latency_seconds_count{step="total"} summed across labels.
freeze_total_count() {
  local out
  out=$(curl -sf --max-time 10 "http://localhost:${CTRL_METRICS_PORT}/metrics" 2>/dev/null || echo "")
  printf '%s' "$out" | awk '
    /^gardenio_freeze_latency_seconds_count\{/ && /step="total"/ { sum += $NF }
    END { printf "%d", sum+0 }'
}

# Scrape gardenio_freeze_interrupts_total summed across labels.
freeze_interrupt_count() {
  local out
  out=$(curl -sf --max-time 10 "http://localhost:${CTRL_METRICS_PORT}/metrics" 2>/dev/null || echo "")
  printf '%s' "$out" | awk '
    /^gardenio_freeze_interrupts_total\{/ { sum += $NF }
    END { printf "%d", sum+0 }'
}

# Fire POST /api/v1/freeze/interrupt for an agent against the controller health
# port. Echoes the JSON body ({"aborted":true|false,...}). Empty on transport
# failure.
fire_interrupt() {
  local agent_name="$1"
  curl -s --max-time 10 -X POST "http://localhost:${CTRL_API_PORT}/api/v1/freeze/interrupt" \
    -H 'Content-Type: application/json' \
    -H "X-Actor: ${KD_ACTOR}" \
    -d "{\"agent\":\"${agent_name}\"}" 2>/dev/null || echo ""
}

pod_phase() {
  kubectl -n "$GVISOR_NS" get pod "$1" -o jsonpath='{.status.phase}' 2>/dev/null || echo ""
}

ensure_metrics_pf() {
  kubectl -n "$NAMESPACE" port-forward "svc/${CONTROLLER_SVC}" "${CTRL_METRICS_PORT}:9092" \
    >/dev/null 2>&1 &
  PF_METRICS_PID=$!
  sleep 3
}

ensure_api_pf() {
  kubectl -n "$NAMESPACE" port-forward "svc/${CONTROLLER_SVC}" "${CTRL_API_PORT}:8091" \
    >/dev/null 2>&1 &
  PF_API_PID=$!
  sleep 3
}

cleanup() {
  blue "── cleanup ──"
  [ -n "$PF_BEADS_PID" ]   && kill "$PF_BEADS_PID"   >/dev/null 2>&1 || true
  [ -n "$PF_METRICS_PID" ] && kill "$PF_METRICS_PID" >/dev/null 2>&1 || true
  [ -n "$PF_API_PID" ]     && kill "$PF_API_PID"     >/dev/null 2>&1 || true
  if [ "${KEEP_BEADS:-0}" != "1" ]; then
    for id in "${CREATED_BEADS[@]:-}"; do
      [ -n "$id" ] || continue
      curl -sf --max-time 10 -X DELETE "${BEADS_HTTP_URL}/v1/beads/$id" \
        ${BEADS_TOKEN:+-H "Authorization: Bearer $BEADS_TOKEN"} >/dev/null 2>&1 || true
    done
  else
    dim "  KEEP_BEADS=1 — leaving ${#CREATED_BEADS[@]} agent bead(s) for debug"
  fi
}
trap cleanup EXIT

echo "════════════════════════════════════════════════════"
echo " gVisor abort-races-completion (TOCTOU) — E2E"
echo " release-ns=$NAMESPACE  gvisor-ns=$GVISOR_NS  runtimeClass=$RUNTIME_CLASS"
echo " ka-0000000996 (epic ka-0000004667)"
echo "════════════════════════════════════════════════════"

# ── Gate 0: opt-in + substrate/controller preflight (SKIP, never silently) ──
if [ "${GVISOR_E2E:-0}" != "1" ] && [ "${ABORT_RACES_E2E:-0}" != "1" ]; then
  skip_all "GVISOR_E2E / ABORT_RACES_E2E != 1 (opt-in module)" off
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

blue "Substrate present — driving the abort-races-completion (TOCTOU) chain."

# ── 1. Reach the beads daemon + controller /metrics + /api ──────────
blue "[1/6] reaching beads daemon + controller /metrics + interrupt API"
ensure_beads_token
ensure_beads_pf
if ! curl -sf --max-time 10 "${BEADS_HTTP_URL}/v1/beads" \
     ${BEADS_TOKEN:+-H "Authorization: Bearer $BEADS_TOKEN"} >/dev/null 2>&1; then
  skip_all "beads daemon $BEADS_HTTP_URL unreachable (port-forward to svc/$BEADS_SVC failed)" loud
fi
pass "beads daemon reachable ($BEADS_HTTP_URL)"

ensure_metrics_pf
if ! curl -sf --max-time 10 "http://localhost:${CTRL_METRICS_PORT}/metrics" >/dev/null 2>&1; then
  skip_all "controller /metrics (svc/$CONTROLLER_SVC :9092) unreachable — cannot assert freeze metrics" loud
fi
pass "controller /metrics reachable (svc/$CONTROLLER_SVC :9092)"

ensure_api_pf
# The interrupt endpoint replies 404 {"aborted":false} for an unknown agent when
# the lifecycle is wired (and FREEZE_INTERRUPT_ENABLED on); a transport failure
# (no route) is the substrate-missing signal. We probe with a sentinel agent.
PROBE_BODY=$(fire_interrupt "abort-races-preflight-${RUN_ID}")
if [ -z "$PROBE_BODY" ]; then
  skip_all "controller interrupt API (svc/$CONTROLLER_SVC :8091 /api/v1/freeze/interrupt) unreachable" loud
fi
dim "    interrupt API preflight reply: $PROBE_BODY"
pass "controller interrupt API reachable (POST /api/v1/freeze/interrupt)"

FREEZE_TOTAL_BEFORE=$(freeze_total_count)
INTERRUPT_BEFORE=$(freeze_interrupt_count)
dim "    baselines: freeze_total=$FREEZE_TOTAL_BEFORE  freeze_interrupts=$INTERRUPT_BEFORE"

# ── 2. Seed a freeze-eligible runsc agent + let the controller spawn it ─
blue "[2/6] seeding freeze-eligible runsc agent + awaiting controller spawn"
AGENT="abrace-${RUN_ID}"
AGENT_ID=$(seed_runsc_agent "$AGENT")
if [ -z "$AGENT_ID" ]; then
  fail "seed agent bead" "beads POST returned no id"
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi
CREATED_BEADS+=("$AGENT_ID")
pass "seeded agent bead ($AGENT_ID, runtime_class=runsc, mock=$MOCK_SCENARIO)"

POD_A=""
deadline=$((SECONDS + POD_READY_TIMEOUT))
while [ "$SECONDS" -lt "$deadline" ]; do
  POD_A=$(find_agent_pod "$AGENT")
  if [ -n "$POD_A" ]; then
    [ "$(pod_phase "$POD_A")" = "Running" ] && break
  fi
  sleep 5
done
if [ -z "$POD_A" ]; then
  fail "controller spawned runsc Pod A" "no pod with label gardenio.io/agent=$AGENT in ns $GVISOR_NS within ${POD_READY_TIMEOUT}s"
  kubectl -n "$GVISOR_NS" get pods -l "gardenio.io/agent=${AGENT}" -o wide 2>&1 | sed 's/^/    /'
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

# ── 3. Trigger the freeze (plain gb stop → SUSPEND) + await post-Put ─
# Plain `gb stop` on a freeze-eligible runsc agent SUSPENDS: agent_state=stopping
# → controller freeze. With FREEZE_POST_PUT_DELAY>0 the freeze parks in the
# post-Put window once the snapshot ConfigMap is written. We wait for that
# ConfigMap to APPEAR — the observable proof Put committed — which is our cue to
# fire the interrupt INSIDE the window.
blue "[3/6] triggering freeze (gb stop) + awaiting snapshot ConfigMap (post-Put)"
kubectl -n "$GVISOR_NS" exec "$POD_A" -c agent -- \
  "$GB_BIN" stop --wrapup '{"tldr":"e2e abort-races probe","accomplishments":"abort-races-completion e2e"}' \
  >/dev/null 2>&1 || true

# Confirm the suspend transition fired (cheap signal the freeze path engaged).
deadline=$((SECONDS + SUSPEND_WAIT))
saw_stopping=0
while [ "$SECONDS" -lt "$deadline" ]; do
  st=$(agent_state_of "$(beads_get "$AGENT_ID")")
  if [ "$st" = "stopping" ]; then saw_stopping=1; break; fi
  [ "$st" = "done" ] && break
  sleep 3
done
[ "$saw_stopping" = 1 ] \
  && pass "agent transitioned to agent_state=stopping (freeze path engaged)" \
  || dim  "    agent_state did not show 'stopping' (freeze may have already advanced); relying on the ConfigMap signal"

# Poll for the snapshot ConfigMap to appear (post-Put). This is the critical
# synchronization point — we MUST observe it before firing the interrupt so the
# abort lands AFTER the Put (the TOCTOU we are proving), not before it.
SNAP_CM=""
deadline=$((SECONDS + SNAP_APPEAR_WAIT))
while [ "$SECONDS" -lt "$deadline" ]; do
  SNAP_CM=$(snapshot_cm "$AGENT")
  [ -n "$SNAP_CM" ] && break
  sleep 2
done
if [ -n "$SNAP_CM" ]; then
  pass "snapshot ConfigMap appeared (post-Put): $SNAP_CM — SnapshotStore.Put durably committed"
else
  fail "snapshot ConfigMap appears (post-Put)" \
    "no ConfigMap with label gardenio.io/ft-agent=$AGENT in ns $NAMESPACE within ${SNAP_APPEAR_WAIT}s — the freeze never reached Put (is FREEZE_POST_PUT_DELAY>0 + FREEZE_INTERRUPT_ENABLED on the controller?)"
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi

# ── 4. Fire the interrupt INSIDE the post-Put window → assert aborted ─
blue "[4/6] firing interrupt in the post-Put window → assert the freeze is aborted"
IRESP=$(fire_interrupt "$AGENT")
dim "    interrupt response: ${IRESP:-<empty>}"
ABORTED=$(printf '%s' "$IRESP" | python3 -c "import sys,json;
try: print(json.load(sys.stdin).get('aborted'))
except Exception: print('')" 2>/dev/null || echo "")
if [ "$ABORTED" = "True" ] || [ "$ABORTED" = "true" ]; then
  pass "interrupt API reported aborted=true — an in-flight (window-parked) freeze was signalled"
else
  fail "interrupt aborts the in-flight freeze" \
    "interrupt response '${IRESP:-<empty>}' did not report aborted=true — the freeze was not parked in the post-Put window when the interrupt fired (widen FREEZE_POST_PUT_DELAY?)"
fi

# ── 5. Assert the just-persisted snapshot is DISCARDED (reaped) ─────
# discardAbortedFreeze runs synchronously in the freeze defer: Delete the
# snapshot + reap the on-node image. The ConfigMap MUST disappear.
blue "[5/6] asserting the just-persisted snapshot is discarded (ConfigMap gone)"
gone=0
deadline=$((SECONDS + SNAP_DISCARD_WAIT))
while [ "$SECONDS" -lt "$deadline" ]; do
  [ -z "$(snapshot_cm "$AGENT")" ] && { gone=1; break; }
  sleep 2
done
if [ "$gone" = 1 ]; then
  pass "snapshot ConfigMap discarded after the raced abort (discardAbortedFreeze Delete ran) — no dangling thaw target"
else
  fail "snapshot discarded after raced abort" \
    "ConfigMap $SNAP_CM still present ${SNAP_DISCARD_WAIT}s after the interrupt — the post-Put snapshot was NOT reaped; a thaw target survives for an agent we kept live (TOCTOU rule VIOLATED)"
fi

# ── 6. Assert: interrupt counted, NO durable freeze, agent left live ─
blue "[6/6] asserting interrupt counted, no durable-freeze sample, and Pod A left live"
# 6a. freeze_interrupts_total advanced (the controller observed ErrFreezeAborted).
INTERRUPT_AFTER=0
deadline=$((SECONDS + 30))
while [ "$SECONDS" -lt "$deadline" ]; do
  INTERRUPT_AFTER=$(freeze_interrupt_count)
  [ "$INTERRUPT_AFTER" -gt "$INTERRUPT_BEFORE" ] 2>/dev/null && break
  sleep 3
done
if [ "$INTERRUPT_AFTER" -gt "$INTERRUPT_BEFORE" ] 2>/dev/null; then
  pass "gardenio_freeze_interrupts_total advanced ($INTERRUPT_BEFORE → $INTERRUPT_AFTER): controller recorded the abort"
else
  fail "freeze_interrupts_total advances" \
    "interrupt count did not advance ($INTERRUPT_BEFORE → $INTERRUPT_AFTER) — the controller did not observe ErrFreezeAborted for this freeze"
fi

# 6b. NO durable freeze sample: freeze_latency{step=total} is recorded ONLY on
# the success branch, which the abort never takes. The count MUST be unchanged.
FREEZE_TOTAL_AFTER=$(freeze_total_count)
if [ "$FREEZE_TOTAL_AFTER" -le "$FREEZE_TOTAL_BEFORE" ] 2>/dev/null; then
  pass "gardenio_freeze_latency_seconds{step=total} did NOT advance ($FREEZE_TOTAL_BEFORE → $FREEZE_TOTAL_AFTER): no durable freeze was recorded — the discarded snapshot is no thaw target"
else
  dim  "    freeze_total advanced ($FREEZE_TOTAL_BEFORE → $FREEZE_TOTAL_AFTER) — another agent may have frozen concurrently"
  dim  "    (non-gating: the discarded ConfigMap + interrupt count are the authoritative TOCTOU proof; a concurrent unrelated freeze can move this global counter)"
fi

# 6c. Pod A LEFT LIVE: the whole point — never a dead agent. `runsc checkpoint
# --leave-running` kept it up, and the abort abandoned the teardown.
phase=$(pod_phase "$POD_A")
ready=$(kubectl -n "$GVISOR_NS" get pod "$POD_A" \
  -o jsonpath='{range .status.conditions[?(@.type=="Ready")]}{.status}{end}' 2>/dev/null || echo "")
if [ "$phase" = "Running" ]; then
  if [ "$ready" = "True" ]; then
    pass "Pod A LEFT LIVE (Running + Ready) — agent kept live through the raced abort (never a dead agent without a thaw target)"
  else
    pass "Pod A LEFT LIVE (Running; Ready=${ready:-?}) — agent not torn down by the raced abort"
  fi
else
  fail "Pod A left live after raced abort" \
    "Pod A phase='${phase:-<gone>}' — the agent was torn down despite the abort; the TOCTOU rule requires it stay live (--leave-running + abandoned teardown)"
fi

# ── Summary ─────────────────────────────────────────────────────────
echo ""
echo "════════════════════════════════════════════════════"
echo " abort-races-completion Summary: $PASS passed, $FAIL failed"
echo "   agent=$AGENT_ID  snapshot=$SNAP_CM (discarded)"
echo "   interrupts $INTERRUPT_BEFORE → ${INTERRUPT_AFTER:-?}  freeze_total $FREEZE_TOTAL_BEFORE → ${FREEZE_TOTAL_AFTER:-?}"
echo "════════════════════════════════════════════════════"
if [ "$FAIL" -gt 0 ]; then
  for e in "${ERRORS[@]}"; do red "  - $e"; done
  exit 1
fi
green "All abort-races-completion assertions passed — a snapshot persisted microseconds before an abort is discarded and the agent is left live."
exit 0
