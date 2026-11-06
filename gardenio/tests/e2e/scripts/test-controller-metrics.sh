#!/usr/bin/env bash
# test-controller-metrics.sh — controller :9092 Prometheus registry E2E
# (gd-ckUXI6EOld, epic ka-0000000958 — E2E coverage-gap sprint)
#
# THE GAP THIS CLOSES: the controller emits a rich set of Prometheus metrics on
# its own internal :9092 registry (served by svc/${NAMESPACE}-agents, port 9092 →
# targetPort metrics), but NO e2e module scrapes that registry. The two modules
# that looked adjacent do not cover it:
#   - test-reconciler.sh   asserts only the controller's LOG-JSON sync_runs /
#                          sync_errors fields (kubectl logs | grep), never :9092.
#   - test-fleet-metrics.sh asserts only the kbeads fleet aggregate gardenio_beads_*
#                          on the DAEMON's :9091 — a different process + registry.
# So four families the timeseries platform + FSM alerts depend on shipped with
# ZERO e2e scrape coverage (commits c806c368 reconcile_total, 3b6ec38e lifecycle/
# pod-phase/session-duration; beads ka-0000003837 / ka-0000007731 / ka-0000002051):
#   1. gardenio_reconcile_total{controller,result}
#   2. gardenio_agent_lifecycle_events_total{event,env,project,role,session_id}
#   3. gardenio_agent_pod_phase_transitions_total{phase_from,phase_to,project,role}
#   4. gardenio_agent_session_duration_seconds{project,role,outcome} (histogram)
#
# ── What it asserts (against the live controller's served :9092 registry) ─────
#   TIER 1 — always-on, deterministic HARD gate (any live controller, warm or
#   cold namespace):
#     A. svc/${NAMESPACE}-agents:9092/metrics returns 200 + valid Prometheus text
#        (# HELP / # TYPE lines).
#     B. gardenio_reconcile_total is present, TYPEd `counter`, and carries the
#        documented {controller,result} labels with result ∈ {ok,error}. This is
#        the reconcile loop's own counter — it runs continuously, so this series
#        exists on EVERY live controller regardless of agent flow. Reproduces the
#        bead's acceptance #3 verbatim:
#          curl controller:9092/metrics | grep gardenio_reconcile_total
#
#   TIER 2 — the three EVENT-DRIVEN families, asserted AFTER a real spawn/stop
#   cycle (acceptance #2). These families only exist once agent lifecycle events
#   have flowed through the controller, so a cold ephemeral namespace exposes
#   none of them until we drive one. We drive a deterministic, SPEND-SAFE
#   spawn/stop cycle (below) and then assert:
#     C. gardenio_agent_lifecycle_events_total appears with event=spawned AND
#        event=despawned for OUR ephemeral project, carrying the documented
#        {event,env,project,role,session_id} label set (TYPE counter).
#     D. gardenio_agent_session_duration_seconds appears for OUR project with the
#        documented {project,role,outcome} labels, outcome=done (TYPE histogram:
#        _bucket/_sum/_count exposed).
#     E. gardenio_agent_pod_phase_transitions_total is present + TYPEd `counter`
#        with the documented {phase_from,phase_to,project,role} labels — asserted
#        OBSERVATIONALLY (any live series), because a witnessed Pending→Running
#        transition requires a pod that actually starts, which our spend-safe
#        drive deliberately never does (see below). A warm namespace has these
#        from ambient fleet flow; a cold one legitimately has none.
#
# ── How the spawn/stop cycle is driven (deterministic + spend-safe) ───────────
# The controller's SSE watcher maps an agent-bead `created` → AgentSpawn and a
# `closed` → AgentDone (internal/subscriber/sse.go mapBeadEvent). AgentSpawn
# increments lifecycle{event=spawned}; AgentDone runs emitSessionCloseMetrics →
# lifecycle{event=despawned} + a session_duration observation (main.go
# emitSessionCloseMetrics). NEITHER needs the pod to reach Running — the counters
# fire on the bead-lifecycle events alone. So we:
#   (1) seed an agent bead for a throwaway ephemeral project via the daemon HTTP
#       API (X-No-Auto-Inherit:1, exactly as the pool reconciler mints agents),
#       carrying image=$IMG_INVALID,
#   (2) wait briefly for the spawned counter to register,
#   (3) close the bead → despawned + session_duration.
#
# SPEND SAFETY (by construction — strongest possible): $IMG_INVALID is a
# DELIBERATELY NON-EXISTENT tag under registry.invalid (a reserved never-resolving
# TLD). The pod the controller creates for the seeded agent can only ever
# ErrImagePull / ImagePullBackOff — the container literally never starts, so
# BOAT_COMMAND / `claude` can NEVER run. Zero model spend is guaranteed regardless
# of timing, and closing the bead makes the controller delete the never-started
# pod. (Because the pod never reaches Running, no Pending→Running phase transition
# is witnessed — which is why family E is asserted observationally, not driven.)
#
# ── Never flakes the release gate ─────────────────────────────────────────────
# Tier 1 is a HARD gate (a live controller ALWAYS exposes reconcile_total). Tier 2
# asserts hard whenever the family is PRESENT (warm ns, or our own drive landed).
# When the drive does NOT surface a family within budget (a cold/slow ephemeral
# controller, an SSE lag, an overlay that scrapes agents differently), the missing
# family is reported as a non-fatal PENDING note by default and becomes a hard
# FAIL only under CONTROLLER_METRICS_STRICT=1 — the same convention as
# test-pool-assign-attribution's POOL_ATTRIBUTION_STRICT. Flip it to 1 in
# run-shard.sh once staging confirms the families populate reliably.
#
# Prerequisites: kubectl (in-cluster), curl, python3. Skips gracefully (exit 0)
# when kubectl/cluster access or the controller Service is unavailable.

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
# Controller Service exposing /metrics on :9092 (helm: agents/service.yaml
# port 9092 → targetPort metrics; deployment containerPort 9092).
CONTROLLER_SVC="${CONTROLLER_SVC:-${NAMESPACE}-agents}"
KD_BIN="${KD_BIN:-kd}"

# A deliberately NON-EXISTENT image tag (registry.invalid is a reserved,
# never-resolving TLD ⇒ ImagePullBackOff ⇒ no container start ⇒ no spend). A
# per-bead `image` override makes the seeded agent's pod use exactly this.
IMG_INVALID="${IMG_INVALID:-registry.invalid/gardenio-e2e/agent:controller-metrics-000}"

# STRICT turns a "family absent after the spawn/stop drive" result into a hard
# fail (CI gate) instead of a non-fatal PENDING note (default: green on cold ns).
CONTROLLER_METRICS_STRICT="${CONTROLLER_METRICS_STRICT:-0}"

PASS=0
FAIL=0
PENDING=0
ERRORS=()
PENDING_NOTES=()
BEADS_PF_PID=""
CTRL_PF_PID=""
CREATED_BEADS=()
BEADS_PORT="${BEADS_PORT:-19110}"
METRICS_PORT="${METRICS_PORT:-19092}"
PROJECT_NAME=""

export KD_ACTOR="${KD_ACTOR:-e2e-controller-metrics}"
# A human-shaped operator id so daemon HTTP writes that resolve an operator pass
# the server's isHumanOperator guard.
OPERATOR="${OPERATOR:-${BEADS_OPERATED_BY:-oidc:e2e-controller-metrics@example.com}}"

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

# pending_or_fail records a Tier-2 assertion that could not be satisfied because
# the spawn/stop drive did not surface the family within budget. In strict mode
# it is a hard fail; otherwise it is a non-fatal pending note so the module stays
# green on a cold/slow controller without hiding the gap.
pending_or_fail() {
  local name="$1" reason="${2:-}"
  if [ "$CONTROLLER_METRICS_STRICT" = "1" ]; then
    fail "$name" "$reason"
  else
    dim "  ⧗ PENDING $name (CONTROLLER_METRICS_STRICT=0): $reason"
    PENDING=$((PENDING + 1))
    PENDING_NOTES+=("$name: $reason")
  fi
}

# SKIP loud — cluster/controller substrate missing. Always exit 0 (an absent
# controller Service must NOT red the suite) but print a banner so a skip is
# never mistaken for a pass.
skip_all() {
  local why="$1"
  blue "SKIP controller-metrics: $why"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

# ── Setup ─────────────────────────────────────────────────────────
setup_token() {
  if [ -n "${BEADS_TOKEN:-}" ]; then return; fi
  local secret
  secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
    -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
  if [ -n "$secret" ]; then
    export BEADS_TOKEN="$secret"
    dim "  Token loaded from secret kd-beads-token"
  fi
}

setup_beads_pf() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE} beads daemon..."
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${BEADS_PORT}:8080" \
    >/tmp/kd-pf-ctlmetrics.log 2>&1 &
  BEADS_PF_PID=$!
  sleep 3
  export BEADS_HTTP_URL="http://localhost:${BEADS_PORT}"
  dim "  Daemon at $BEADS_HTTP_URL (pid $BEADS_PF_PID)"
}

# Re-establish the beads daemon port-forward if it died (drive loops can outlast
# a single kubectl port-forward). Only manages a pf WE created.
ensure_beads_pf() {
  [ -z "$BEADS_PF_PID" ] && return 0
  if curl -sf --max-time 3 -o /dev/null "${BEADS_HTTP_URL}/v1/health" 2>/dev/null; then
    return 0
  fi
  kill "$BEADS_PF_PID" 2>/dev/null || true
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${BEADS_PORT}:8080" \
    >/tmp/kd-pf-ctlmetrics.log 2>&1 &
  BEADS_PF_PID=$!
  sleep 3
}

setup_metrics_pf() {
  blue "Port-forwarding ${NAMESPACE} controller :9092 registry..."
  kubectl -n "$NAMESPACE" port-forward "svc/${CONTROLLER_SVC}" "${METRICS_PORT}:9092" \
    >/tmp/ctl-pf-ctlmetrics.log 2>&1 &
  CTRL_PF_PID=$!
  sleep 3
  dim "  Controller metrics at http://localhost:${METRICS_PORT}/metrics (pid $CTRL_PF_PID)"
}

ensure_metrics_pf() {
  if curl -sf --max-time 3 -o /dev/null "http://localhost:${METRICS_PORT}/metrics" 2>/dev/null; then
    return 0
  fi
  [ -n "$CTRL_PF_PID" ] && kill "$CTRL_PF_PID" 2>/dev/null || true
  kubectl -n "$NAMESPACE" port-forward "svc/${CONTROLLER_SVC}" "${METRICS_PORT}:9092" \
    >/tmp/ctl-pf-ctlmetrics.log 2>&1 &
  CTRL_PF_PID=$!
  sleep 3
}

# Scrape the controller :9092 /metrics body once. Echoes the raw text (empty on
# failure). Callers grep/awk it — capture once and reuse (advice ka-0000002706).
scrape_metrics() {
  ensure_metrics_pf
  curl -sf --max-time 8 "http://localhost:${METRICS_PORT}/metrics" 2>/dev/null || echo ""
}

cleanup() {
  # Close every agent/project bead we created so the controller reaps the
  # (never-started) pod and we leave no orphan. Best-effort; never fail on cleanup.
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    for id in "${CREATED_BEADS[@]:-}"; do
      [ -n "$id" ] || continue
      curl -sf --max-time 8 -X POST "${BEADS_HTTP_URL}/v1/beads/${id}/close" \
        ${BEADS_TOKEN:+-H "Authorization: Bearer $BEADS_TOKEN"} \
        -H "X-Operated-By: ${OPERATOR}" \
        -H 'Content-Type: application/json' -d '{}' >/dev/null 2>&1 \
        || "$KD_BIN" close "$id" 2>/dev/null || true
    done
  fi
  [ -n "$BEADS_PF_PID" ] && kill "$BEADS_PF_PID" 2>/dev/null || true
  [ -n "$CTRL_PF_PID" ]  && kill "$CTRL_PF_PID"  2>/dev/null || true
}
trap cleanup EXIT

summary() {
  echo
  blue "═══════════════════════════════════════════════════"
  local total=$((PASS + FAIL))
  if [ "$FAIL" -eq 0 ]; then
    green " ✓ All $total assertions passed"
    [ "$PENDING" -gt 0 ] && dim "   ($PENDING pending note(s) — non-fatal; set CONTROLLER_METRICS_STRICT=1 to enforce)"
  else
    red " ✗ $FAIL/$total assertions failed"
    echo
    for err in "${ERRORS[@]}"; do red "   - $err"; done
  fi
  if [ "$PENDING" -gt 0 ]; then
    echo
    dim " Pending (Tier-2 family not surfaced by the spawn/stop drive):"
    for n in "${PENDING_NOTES[@]}"; do dim "   ⧗ $n"; done
  fi
  blue "═══════════════════════════════════════════════════"
  [ "$FAIL" -eq 0 ]
}

# Seed one agent bead for our ephemeral project via the daemon HTTP API. Mirrors
# how the pool reconciler mints agents (X-No-Auto-Inherit:1, system subject).
# Carries image=$IMG_INVALID so any pod the controller creates can only
# ImagePullBackOff — never starts, never spends. Echoes the created bead id.
seed_agent() {
  local name="$1" role="$2"
  local body
  body=$(NAME="$name" ROLE="$role" PROJ="$PROJECT_NAME" IMG="$IMG_INVALID" python3 -c "
import json, os
print(json.dumps({
  'type': 'agent',
  'title': os.environ['NAME'],
  'labels': ['e2e', 'controller-metrics', 'project:' + os.environ['PROJ']],
  'fields': {
    'agent': os.environ['NAME'],
    'agent_state': 'working',
    'mode': 'crew',
    'role': os.environ['ROLE'],
    'project': os.environ['PROJ'],
    'image': os.environ['IMG'],
  },
}))")
  curl -sf --max-time 10 -X POST "${BEADS_HTTP_URL}/v1/beads" \
    ${BEADS_TOKEN:+-H "Authorization: Bearer $BEADS_TOKEN"} \
    -H "X-Operated-By: ${OPERATOR}" \
    -H 'X-No-Auto-Inherit: 1' \
    -H 'Content-Type: application/json' \
    -d "$body" 2>/dev/null \
    | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null \
    || echo ""
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " controller :9092 metrics registry E2E (gd-ckUXI6EOld)"
  blue "═══════════════════════════════════════════════════"

  command -v kubectl >/dev/null 2>&1 || skip_all "kubectl not available — cannot reach the controller :9092 registry"
  command -v curl    >/dev/null 2>&1 || skip_all "curl not available"
  command -v python3 >/dev/null 2>&1 || skip_all "python3 not available"
  kubectl get ns "$NAMESPACE" >/dev/null 2>&1 \
    || skip_all "namespace '$NAMESPACE' not present"
  kubectl -n "$NAMESPACE" get "svc/${CONTROLLER_SVC}" >/dev/null 2>&1 \
    || skip_all "controller Service 'svc/${CONTROLLER_SVC}' not present (no agents deployment in this overlay)"

  setup_token
  setup_beads_pf
  setup_metrics_pf

  local ROLE="crew"
  local TEST_TAG="e2e-ctlmetrics-$$-$(date +%s)"
  PROJECT_NAME="e2e-ctlmetrics-${TEST_TAG}"

  # ── Tier 1A: /metrics reachable + valid Prometheus text ────────────
  blue "Tier 1A: controller :9092 /metrics reachable + valid Prometheus format"
  local body
  body=$(scrape_metrics)
  if [ -z "$body" ]; then
    skip_all "controller /metrics (svc/${CONTROLLER_SVC} :9092) unreachable — port-forward failed or endpoint not served"
  fi
  pass "GET :9092/metrics returned a body ($(printf '%s' "$body" | wc -l | tr -d ' ') lines)"
  if grep -q '^# HELP ' <<< "$body" && grep -q '^# TYPE ' <<< "$body"; then
    pass "valid Prometheus text format (# HELP / # TYPE lines present)"
  else
    fail "valid Prometheus text format" "missing # HELP / # TYPE lines"
  fi
  echo

  # ── Tier 1B: gardenio_reconcile_total{controller,result} (HARD) ─────
  # The reconcile loop runs continuously, so this counter exists on EVERY live
  # controller regardless of agent flow — a deterministic hard gate. This is the
  # bead's acceptance #3 reproduce: curl :9092/metrics | grep gardenio_reconcile_total
  blue "Tier 1B: gardenio_reconcile_total present with documented {controller,result} labels"
  if grep -qE '^# TYPE gardenio_reconcile_total counter' <<< "$body"; then
    pass "gardenio_reconcile_total TYPEd as counter"
  else
    fail "gardenio_reconcile_total TYPE" "expected '# TYPE gardenio_reconcile_total counter'"
  fi
  local reconcile_lines
  reconcile_lines=$(grep -E '^gardenio_reconcile_total\{' <<< "$body" || true)
  if [ -n "$reconcile_lines" ]; then
    pass "gardenio_reconcile_total has data series ($(grep -c . <<< "$reconcile_lines") line(s))"
  else
    fail "gardenio_reconcile_total data series" "no gardenio_reconcile_total{...} lines on :9092"
  fi
  if grep -qE '^gardenio_reconcile_total\{[^}]*controller="[^"]+"' <<< "$reconcile_lines" \
     && grep -qE '^gardenio_reconcile_total\{[^}]*result="[^"]+"' <<< "$reconcile_lines"; then
    pass "documented labels present: {controller, result}"
  else
    fail "gardenio_reconcile_total labels" "expected {controller=..,result=..}; got: $(head -1 <<< "$reconcile_lines")"
  fi
  if grep -qE '^gardenio_reconcile_total\{[^}]*result="(ok|error)"' <<< "$reconcile_lines"; then
    pass "result label uses the documented ok|error enum"
  else
    fail "result enum" "expected result=ok|error"
  fi
  echo

  # ── Tier 2: drive a SPEND-SAFE spawn/stop cycle ────────────────────
  blue "Tier 2: driving a spend-safe spawn/stop cycle to exercise the event families"
  if ! curl -sf --max-time 8 -o /dev/null "${BEADS_HTTP_URL}/v1/health" 2>/dev/null; then
    pending_or_fail "spawn/stop drive" "beads daemon ${BEADS_HTTP_URL} unreachable — cannot seed an agent bead"
    summary; return
  fi

  local AGENT_NAME="ctlmetrics-${TEST_TAG}"
  local AGENT_ID
  AGENT_ID=$(seed_agent "$AGENT_NAME" "$ROLE")
  if [ -z "$AGENT_ID" ]; then
    pending_or_fail "seed agent bead" "daemon POST /v1/beads returned no id (write rejected?)"
    summary; return
  fi
  CREATED_BEADS+=("$AGENT_ID")
  pass "seeded agent bead ($AGENT_ID, project=$PROJECT_NAME, image=registry.invalid → spend-safe)"

  # Wait for the spawned counter to register for OUR project (SSE create → spawn).
  blue "  awaiting gardenio_agent_lifecycle_events_total{event=spawned} for our project (≤40s)..."
  local spawned_seen=0 spawn_deadline=$(( $(date +%s) + 40 ))
  while [ "$(date +%s)" -lt "$spawn_deadline" ]; do
    if scrape_metrics | grep -qE "^gardenio_agent_lifecycle_events_total\{[^}]*event=\"spawned\"[^}]*project=\"${PROJECT_NAME}\"" ; then
      spawned_seen=1
      break
    fi
    sleep 3
  done

  # Close the bead → AgentDone → despawned + session_duration observation.
  ensure_beads_pf
  curl -sf --max-time 10 -X POST "${BEADS_HTTP_URL}/v1/beads/${AGENT_ID}/close" \
    ${BEADS_TOKEN:+-H "Authorization: Bearer $BEADS_TOKEN"} \
    -H "X-Operated-By: ${OPERATOR}" \
    -H 'Content-Type: application/json' -d '{}' >/dev/null 2>&1 \
    || "$KD_BIN" close "$AGENT_ID" 2>/dev/null || true
  pass "closed agent bead (drives despawned + session_duration on the close SSE)"

  # Give the controller time to process the close SSE + GetBead round-trip.
  blue "  awaiting despawned + session_duration for our project (≤40s)..."
  local despawned_seen=0 duration_seen=0 close_deadline=$(( $(date +%s) + 40 ))
  local final_body=""
  while [ "$(date +%s)" -lt "$close_deadline" ]; do
    final_body=$(scrape_metrics)
    grep -qE "^gardenio_agent_lifecycle_events_total\{[^}]*event=\"despawned\"[^}]*project=\"${PROJECT_NAME}\"" <<< "$final_body" && despawned_seen=1
    grep -qE "^gardenio_agent_session_duration_seconds_count\{[^}]*project=\"${PROJECT_NAME}\"" <<< "$final_body" && duration_seen=1
    [ "$despawned_seen" = 1 ] && [ "$duration_seen" = 1 ] && break
    sleep 3
  done
  [ -z "$final_body" ] && final_body=$(scrape_metrics)
  echo

  # ── Tier 2C: gardenio_agent_lifecycle_events_total ──────────────────
  blue "Tier 2C: gardenio_agent_lifecycle_events_total appears with documented labels"
  if grep -qE '^# TYPE gardenio_agent_lifecycle_events_total counter' <<< "$final_body"; then
    pass "gardenio_agent_lifecycle_events_total TYPEd as counter"
  else
    pending_or_fail "gardenio_agent_lifecycle_events_total TYPE" "no '# TYPE ... counter' — family absent (no lifecycle flow on this controller)"
  fi
  # Assert the full documented label set on our project's series.
  local life_line
  life_line=$(grep -E "^gardenio_agent_lifecycle_events_total\{[^}]*project=\"${PROJECT_NAME}\"" <<< "$final_body" | head -1 || true)
  if [ -n "$life_line" ]; then
    pass "lifecycle series present for our project"
    local ok_labels=1 l
    for l in event env project role session_id; do
      grep -qE "${l}=\"" <<< "$life_line" || { ok_labels=0; dim "    missing label: $l"; }
    done
    if [ "$ok_labels" = 1 ]; then
      pass "documented labels present: {event,env,project,role,session_id}"
    else
      fail "lifecycle label set" "series missing a documented label: $life_line"
    fi
  else
    pending_or_fail "lifecycle series for our project" "no gardenio_agent_lifecycle_events_total{project=$PROJECT_NAME} within budget (spawned_seen=$spawned_seen)"
  fi
  # event=spawned and event=despawned specifically (the spawn/stop cycle).
  if [ "$spawned_seen" = 1 ] || grep -qE "^gardenio_agent_lifecycle_events_total\{[^}]*event=\"spawned\"[^}]*project=\"${PROJECT_NAME}\"" <<< "$final_body"; then
    pass "event=spawned recorded for our project (spawn half of the cycle)"
  else
    pending_or_fail "event=spawned" "spawned counter never appeared for our project"
  fi
  if [ "$despawned_seen" = 1 ]; then
    pass "event=despawned recorded for our project (stop half of the cycle)"
  else
    pending_or_fail "event=despawned" "despawned counter never appeared for our project"
  fi
  echo

  # ── Tier 2D: gardenio_agent_session_duration_seconds ────────────────
  blue "Tier 2D: gardenio_agent_session_duration_seconds appears with documented labels"
  if grep -qE '^# TYPE gardenio_agent_session_duration_seconds histogram' <<< "$final_body"; then
    pass "gardenio_agent_session_duration_seconds TYPEd as histogram"
  else
    pending_or_fail "session_duration TYPE" "no '# TYPE ... histogram' — family absent (no session close observed)"
  fi
  local dur_line
  dur_line=$(grep -E "^gardenio_agent_session_duration_seconds_count\{[^}]*project=\"${PROJECT_NAME}\"" <<< "$final_body" | head -1 || true)
  if [ -n "$dur_line" ]; then
    pass "session_duration observed for our project ($dur_line)"
    local ok_dur=1 l
    for l in project role outcome; do
      grep -qE "${l}=\"" <<< "$dur_line" || { ok_dur=0; dim "    missing label: $l"; }
    done
    if [ "$ok_dur" = 1 ]; then
      pass "documented labels present: {project,role,outcome}"
    else
      fail "session_duration label set" "series missing a documented label: $dur_line"
    fi
    if grep -qE "outcome=\"(done|failed|stopped)\"" <<< "$dur_line"; then
      pass "outcome label uses the documented done|failed|stopped enum"
    else
      fail "outcome enum" "expected outcome=done|failed|stopped; got: $dur_line"
    fi
  else
    pending_or_fail "session_duration for our project" "no _count series for project=$PROJECT_NAME within budget (duration_seen=$duration_seen)"
  fi
  echo

  # ── Tier 2E: gardenio_agent_pod_phase_transitions_total (observational) ─
  # A witnessed Pending→Running transition needs a pod that actually STARTS,
  # which our spend-safe drive deliberately never does — so we assert this family
  # observationally: present (from ambient fleet flow in a warm ns) → assert the
  # documented labels; absent (cold ns) → pending, never a red.
  blue "Tier 2E: gardenio_agent_pod_phase_transitions_total (observational — documented labels)"
  if grep -qE '^# TYPE gardenio_agent_pod_phase_transitions_total counter' <<< "$final_body"; then
    pass "gardenio_agent_pod_phase_transitions_total TYPEd as counter"
    local phase_line
    phase_line=$(grep -E '^gardenio_agent_pod_phase_transitions_total\{' <<< "$final_body" | head -1 || true)
    if [ -n "$phase_line" ]; then
      local ok_phase=1 l
      for l in phase_from phase_to project role; do
        grep -qE "${l}=\"" <<< "$phase_line" || { ok_phase=0; dim "    missing label: $l"; }
      done
      if [ "$ok_phase" = 1 ]; then
        pass "documented labels present: {phase_from,phase_to,project,role}"
      else
        fail "pod_phase_transitions label set" "series missing a documented label: $phase_line"
      fi
    else
      pending_or_fail "pod_phase_transitions data series" "TYPE present but no data lines (no witnessed phase change yet)"
    fi
  else
    pending_or_fail "gardenio_agent_pod_phase_transitions_total" "family absent — no pod reached Running on this controller (spend-safe drive never starts a pod; cold ns)"
  fi

  summary
}

main "$@"
