#!/usr/bin/env bash
# test-idle-active-metrics.sh — controller idle/active agent measurement E2E
# (ka-0000003235, epic ka-0000000958 — E2E coverage-gap sprint)
#
# THE GAP THIS CLOSES: MR 6072815ec (ka-0000006778) added
# internal/reconciler/idle_metrics.go, emitting the Phase-0 idle/active
# measurement off the reconcile loop as TWO new Prometheus series:
#   1. gardenio_agents_by_activity{project,activity="idle"|"active"}      (gauge)
#   2. gardenio_agent_reserved_memory_bytes{project,activity}            (gauge)
# (2) is the headline number for the idle-capacity/cost story in
# gardenio/docs/idle-active-measurement.md — the reservation freeze/thaw must beat.
#
# Coverage TODAY is idle_metrics_test.go — a Go UNIT test over the classifier
# in-process. `grep -rn 'agents_by_activity|agent_reserved_memory_bytes'
# gardenio/tests/` returns ZERO hits: nothing asserts the series actually reach
# a DEPLOYED controller's /metrics with the right label cardinality — which is
# what a dashboard/alert consumes. A unit test cannot catch a registration miss,
# a renamed label, a namespace/subsystem typo, or the collector never being wired
# into the reconcile loop — every one of which silently yields an EMPTY dashboard
# rather than a failing test. This module scrapes the live registry and closes
# that gap. Distinct surface from:
#   - test-controller-metrics.sh : same :9092 registry, but the reconcile_total /
#                                  lifecycle / session_duration / pod_phase
#                                  families — NOT the two idle/active gauges.
#   - test-fleet-metrics.sh      : the kbeads DAEMON :9091 gardenio_beads_* (a
#                                  different process + registry).
#   - test-reconciler.sh         : the controller's LOG-JSON sync fields only.
#
# ── What it asserts (against the live controller's served :9092 registry) ─────
#   TIER 1 — always-on, deterministic HARD gate. The idle/active gauges are
#   emitted on EVERY reconcile pass (recordIdleMetrics runs unconditionally once
#   idleMetrics is wired), and recordIdleMetrics PRE-SEEDS both the "active" and
#   "idle" bucket for every project that has ≥1 desired agent. So a live
#   controller managing any fleet at all exposes both series with both activity
#   values — no agent flow to drive. Tier 1 asserts:
#     A. svc/${NAMESPACE}-agents:9092/metrics returns 200 + valid Prometheus text.
#     B. gardenio_agents_by_activity present, TYPEd `gauge`, carries the documented
#        {project,activity} labels, and activity ∈ {active,idle} (the enum
#        classifyActivity emits — no third value may ever appear).
#     C. gardenio_agent_reserved_memory_bytes present, TYPEd `gauge`, same
#        {project,activity} labels + enum.
#     D. PRE-SEED SYMMETRY invariant (the property idle_metrics.go's bucketFor
#        pre-seed guarantees): every project that has an "active" agents_by_activity
#        series ALSO has an "idle" one, and vice-versa. A dashboard divides
#        idle/active per project; a missing denominator/numerator would silently
#        break the ratio. This is a structural invariant of the emitter, so it is
#        a HARD gate on any warm controller.
#     E. VALUE-JOIN invariant: reserved_memory{p,a} > 0  ⇒  agents_by_activity{p,a}
#        > 0. Reserved bytes are only ever attributed to an activity bucket that
#        has at least one agent (recordIdleMetrics sums memory INTO the same
#        bucket it counted the agent in), so reserved-without-a-body is a
#        label/join bug. HARD whenever any reserved series is > 0.
#     F. reserved_memory is a BYTE count, not a page/agent count: any non-zero
#        series must be ≥ 1 MiB (a real pod memory request is ≥ Mi; a value of
#        "3" would mean the emitter is counting agents, not bytes — the exact
#        namespace/unit typo a unit test misses). HARD when a non-zero series
#        exists.
#
#   TIER 2 — drive a SPEND-SAFE agent so BOTH activity values carry OUR
#   ephemeral project's data and reserved-memory MOVES with agent count
#   (acceptance: "activity label takes both idle and active values across a
#   spawned+idle agent; reserved-memory is > 0 and moves with agent count").
#   Seeds an agent bead for a throwaway project carrying image=$IMG_INVALID:
#     G. our project appears in agents_by_activity with BOTH activity buckets
#        pre-seeded (active=1 from the working seed, idle=0), proving a fresh
#        project lands in the series the moment it has an agent.
#     H. reserved_memory{our-project,active} becomes > 0 once the (spend-safe,
#        never-starting) pod is created — the Pending pod still RESERVES its 8Gi
#        memory request, which is exactly the reserved-capacity figure — and
#        MOVES UP when a SECOND agent for our project is seeded (reserved tracks
#        pod count, the "moves with agent count" acceptance).
#   Tier 2 is non-fatal PENDING by default (cold/slow ephemeral controller,
#   reconcile-tick lag) and a hard FAIL only under IDLE_METRICS_STRICT=1 — same
#   convention as test-controller-metrics' CONTROLLER_METRICS_STRICT. Flip it in
#   run-shard.sh once staging confirms the drive lands reliably.
#
# ── SPEND SAFETY (by construction — strongest possible) ───────────────────────
# $IMG_INVALID is a DELIBERATELY NON-EXISTENT tag under registry.invalid (a
# reserved never-resolving TLD). The pod the controller creates for the seeded
# agent can only ever ErrImagePull / ImagePullBackOff — the container literally
# never starts, so BOAT_COMMAND / `claude` can NEVER run. Zero model spend is
# guaranteed regardless of timing. A Pending/ImagePullBackOff pod is NON-terminal,
# so it still contributes its memory REQUEST to reserved_memory — which is the
# whole point of the metric (it measures RESERVED capacity, not live RSS). Closing
# the seed beads makes the controller delete the never-started pods.
#
# ── Timing note ───────────────────────────────────────────────────────────────
# recordIdleMetrics runs on the periodic reconcile tick (COOP_SYNC_INTERVAL,
# default 60s), NOT per-event — and reserved_memory needs the pod to exist in the
# reconciler's actualMap, i.e. one tick AFTER the pod is created. So Tier 2 waits
# generously (multiple ticks) and treats a non-appearance as PENDING, never a red.
#
# ── Never flakes the release gate ─────────────────────────────────────────────
# Tier 1 A–F are HARD but only assert PROPERTIES that hold on ANY warm controller
# (both series exist; the pre-seed symmetry + value-join invariants). They do NOT
# require our drive to have landed. Tier 2 (our-project-specific data) is PENDING
# unless IDLE_METRICS_STRICT=1. Self-SKIPs (exit 0) when kubectl/cluster access or
# the controller Service is absent.
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
IMG_INVALID="${IMG_INVALID:-registry.invalid/gardenio-e2e/agent:idle-active-000}"

# STRICT turns a "our-project data did not surface within budget" Tier-2 result
# into a hard fail (CI gate) instead of a non-fatal PENDING note (default: green
# on a cold ns).
IDLE_METRICS_STRICT="${IDLE_METRICS_STRICT:-0}"

PASS=0
FAIL=0
PENDING=0
ERRORS=()
PENDING_NOTES=()
BEADS_PF_PID=""
CTRL_PF_PID=""
CREATED_BEADS=()
BEADS_PORT="${BEADS_PORT:-19111}"
METRICS_PORT="${METRICS_PORT:-19093}"
PROJECT_NAME=""

export KD_ACTOR="${KD_ACTOR:-e2e-idle-active-metrics}"
# A human-shaped operator id so daemon HTTP writes that resolve an operator pass
# the server's isHumanOperator guard.
OPERATOR="${OPERATOR:-${BEADS_OPERATED_BY:-oidc:e2e-idle-active-metrics@example.com}}"

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
# our spend-safe drive did not surface OUR project's data within budget. In
# strict mode it is a hard fail; otherwise a non-fatal pending note so the module
# stays green on a cold/slow controller without hiding the gap.
pending_or_fail() {
  local name="$1" reason="${2:-}"
  if [ "$IDLE_METRICS_STRICT" = "1" ]; then
    fail "$name" "$reason"
  else
    dim "  ⧗ PENDING $name (IDLE_METRICS_STRICT=0): $reason"
    PENDING=$((PENDING + 1))
    PENDING_NOTES+=("$name: $reason")
  fi
}

# SKIP loud — cluster/controller substrate missing. Always exit 0 (an absent
# controller Service must NOT red the suite) but print a banner so a skip is
# never mistaken for a pass.
skip_all() {
  local why="$1"
  blue "SKIP idle-active-metrics: $why"
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
    >/tmp/kd-pf-idlemetrics.log 2>&1 &
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
    >/tmp/kd-pf-idlemetrics.log 2>&1 &
  BEADS_PF_PID=$!
  sleep 3
}

setup_metrics_pf() {
  blue "Port-forwarding ${NAMESPACE} controller :9092 registry..."
  kubectl -n "$NAMESPACE" port-forward "svc/${CONTROLLER_SVC}" "${METRICS_PORT}:9092" \
    >/tmp/ctl-pf-idlemetrics.log 2>&1 &
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
    >/tmp/ctl-pf-idlemetrics.log 2>&1 &
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
  # Close every agent bead we created so the controller reaps the (never-started)
  # pod and we leave no orphan. Best-effort; never fail on cleanup.
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
    [ "$PENDING" -gt 0 ] && dim "   ($PENDING pending note(s) — non-fatal; set IDLE_METRICS_STRICT=1 to enforce)"
  else
    red " ✗ $FAIL/$total assertions failed"
    echo
    for err in "${ERRORS[@]}"; do red "   - $err"; done
  fi
  if [ "$PENDING" -gt 0 ]; then
    echo
    dim " Pending (Tier-2 our-project data not surfaced by the spend-safe drive):"
    for n in "${PENDING_NOTES[@]}"; do dim "   ⧗ $n"; done
  fi
  blue "═══════════════════════════════════════════════════"
  [ "$FAIL" -eq 0 ]
}

# Seed one agent bead for our ephemeral project via the daemon HTTP API. Mirrors
# how the pool reconciler mints agents (X-No-Auto-Inherit:1, system subject).
# Carries image=$IMG_INVALID so any pod the controller creates can only
# ImagePullBackOff — never starts, never spends. agent_state controls activity
# classification (working ⇒ active). Echoes the created bead id.
seed_agent() {
  local name="$1" role="$2" state="$3"
  local body
  body=$(NAME="$name" ROLE="$role" PROJ="$PROJECT_NAME" IMG="$IMG_INVALID" STATE="$state" python3 -c "
import json, os
print(json.dumps({
  'type': 'agent',
  'title': os.environ['NAME'],
  'labels': ['e2e', 'idle-active-metrics', 'project:' + os.environ['PROJ']],
  'fields': {
    'agent': os.environ['NAME'],
    'agent_state': os.environ['STATE'],
    'mode': 'crew',
    'role': os.environ['ROLE'],
    'project': os.environ['PROJ'],
    'image': os.environ['IMG'],
  },
}))")
  ensure_beads_pf
  curl -sf --max-time 10 -X POST "${BEADS_HTTP_URL}/v1/beads" \
    ${BEADS_TOKEN:+-H "Authorization: Bearer $BEADS_TOKEN"} \
    -H "X-Operated-By: ${OPERATOR}" \
    -H 'X-No-Auto-Inherit: 1' \
    -H 'Content-Type: application/json' \
    -d "$body" 2>/dev/null \
    | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null \
    || echo ""
}

# Sum gardenio_agent_reserved_memory_bytes over ALL activity buckets for a given
# project from a scraped body. Echoes an integer (0 if the project has no series).
# Handles prometheus float exposition (e.g. 8.589934592e+09) via python.
reserved_mem_for_project() {
  local body="$1" project="$2"
  grep -E "^gardenio_agent_reserved_memory_bytes\{[^}]*project=\"${project}\"" <<< "$body" \
    | awk '{print $NF}' \
    | python3 -c "import sys; print(int(sum(float(x) for x in sys.stdin.read().split() or ['0'])))" 2>/dev/null \
    || echo 0
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " controller idle/active metrics E2E (ka-0000003235)"
  blue "═══════════════════════════════════════════════════"

  command -v kubectl >/dev/null 2>&1 || skip_all "kubectl not available — cannot reach the controller :9092 registry"
  command -v curl    >/dev/null 2>&1 || skip_all "curl not available"
  command -v python3 >/dev/null 2>&1 || skip_all "python3 not available"
  kubectl get ns "$NAMESPACE" >/dev/null 2>&1 \
    || skip_all "namespace '$NAMESPACE' not present"
  kubectl -n "$NAMESPACE" get "svc/${CONTROLLER_SVC}" >/dev/null 2>&1 \
    || skip_all "controller Service 'svc/${CONTROLLER_SVC}' not present (no agents deployment in this overlay)"

  setup_token
  setup_metrics_pf

  local ROLE="crew"
  local TEST_TAG="e2e-idleactive-$$-$(date +%s)"
  PROJECT_NAME="e2e-idleactive-${TEST_TAG}"

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

  # ── Feature-deployed gate (SKIP if the idle_metrics.go collector predates this
  #    controller build) ───────────────────────────────────────────────────────
  # The two Phase-0 gauges are NEW (MR 6072815ec, ka-0000006778). A controller
  # image built before that MR serves a valid /metrics with NEITHER family — a
  # pre-feature soak/staging namespace (VERIFIED: staging on a 5-day-old
  # image exposes neither, while the release image 2026.217.8 on gardenio-system
  # exposes both). Total absence of BOTH families means "feature not deployed
  # here", NOT a regression, so we SKIP (exit 0) rather than red the gate —
  # exactly the convention test-semantic-search.sh uses to be safe to wire into
  # CI before the stack ships. This is a PRESENCE gate only: once EITHER family
  # is emitted, every Tier-1 assertion below (TYPE, labels, active|idle enum,
  # pre-seed symmetry, value-join, byte-unit) is HARD — a renamed label, a
  # namespace/subsystem typo, or a unit bug still emits the family (just wrong)
  # and is caught, not skipped.
  if ! grep -q '^gardenio_agents_by_activity{'          <<< "$body" \
     && ! grep -q '^# TYPE gardenio_agents_by_activity ' <<< "$body" \
     && ! grep -q '^gardenio_agent_reserved_memory_bytes{'          <<< "$body" \
     && ! grep -q '^# TYPE gardenio_agent_reserved_memory_bytes '   <<< "$body"; then
    skip_all "neither gardenio_agents_by_activity nor gardenio_agent_reserved_memory_bytes present on :9092 — controller image predates idle_metrics.go (MR 6072815ec); feature not deployed in this namespace"
  fi

  # ── Tier 1B: gardenio_agents_by_activity present + documented labels/enum ──
  blue "Tier 1B: gardenio_agents_by_activity present, TYPE gauge, {project,activity} labels, activity∈{active,idle}"
  if grep -qE '^# TYPE gardenio_agents_by_activity gauge' <<< "$body"; then
    pass "gardenio_agents_by_activity TYPEd as gauge"
  else
    fail "gardenio_agents_by_activity TYPE" "expected '# TYPE gardenio_agents_by_activity gauge'"
  fi
  local activity_lines
  activity_lines=$(grep -E '^gardenio_agents_by_activity\{' <<< "$body" || true)
  if [ -n "$activity_lines" ]; then
    pass "gardenio_agents_by_activity has data series ($(grep -c . <<< "$activity_lines") line(s))"
  else
    fail "gardenio_agents_by_activity data series" "no gardenio_agents_by_activity{...} lines on :9092 — collector not wired into the reconcile loop?"
  fi
  if grep -qE '^gardenio_agents_by_activity\{[^}]*project="[^"]*"' <<< "$activity_lines" \
     && grep -qE '^gardenio_agents_by_activity\{[^}]*activity="[^"]+"' <<< "$activity_lines"; then
    pass "documented labels present: {project, activity}"
  else
    fail "gardenio_agents_by_activity labels" "expected {project=..,activity=..}; got: $(head -1 <<< "$activity_lines")"
  fi
  # activity is an enum: classifyActivity emits ONLY "active" | "idle". A third
  # value means a renamed/typo'd label — the exact miss a unit test cannot catch.
  local bad_activity
  bad_activity=$(grep -oE 'activity="[^"]*"' <<< "$activity_lines" | sort -u | grep -vE 'activity="(active|idle)"' || true)
  if [ -z "$bad_activity" ]; then
    pass "activity label uses ONLY the documented active|idle enum"
  else
    fail "activity enum" "unexpected activity value(s): $(echo "$bad_activity" | tr '\n' ' ')"
  fi
  echo

  # ── Tier 1C: gardenio_agent_reserved_memory_bytes present + labels/enum ──
  blue "Tier 1C: gardenio_agent_reserved_memory_bytes present, TYPE gauge, {project,activity} labels"
  if grep -qE '^# TYPE gardenio_agent_reserved_memory_bytes gauge' <<< "$body"; then
    pass "gardenio_agent_reserved_memory_bytes TYPEd as gauge"
  else
    fail "gardenio_agent_reserved_memory_bytes TYPE" "expected '# TYPE gardenio_agent_reserved_memory_bytes gauge'"
  fi
  local reserved_lines
  reserved_lines=$(grep -E '^gardenio_agent_reserved_memory_bytes\{' <<< "$body" || true)
  if [ -n "$reserved_lines" ]; then
    pass "gardenio_agent_reserved_memory_bytes has data series ($(grep -c . <<< "$reserved_lines") line(s))"
  else
    fail "gardenio_agent_reserved_memory_bytes data series" "no gardenio_agent_reserved_memory_bytes{...} lines on :9092"
  fi
  if grep -qE '^gardenio_agent_reserved_memory_bytes\{[^}]*project="[^"]*"' <<< "$reserved_lines" \
     && grep -qE '^gardenio_agent_reserved_memory_bytes\{[^}]*activity="[^"]+"' <<< "$reserved_lines"; then
    pass "documented labels present: {project, activity}"
  else
    fail "gardenio_agent_reserved_memory_bytes labels" "expected {project=..,activity=..}; got: $(head -1 <<< "$reserved_lines")"
  fi
  local bad_ractivity
  bad_ractivity=$(grep -oE 'activity="[^"]*"' <<< "$reserved_lines" | sort -u | grep -vE 'activity="(active|idle)"' || true)
  if [ -z "$bad_ractivity" ]; then
    pass "reserved_memory activity label uses ONLY the documented active|idle enum"
  else
    fail "reserved_memory activity enum" "unexpected activity value(s): $(echo "$bad_ractivity" | tr '\n' ' ')"
  fi
  echo

  # ── Tier 1D: PRE-SEED SYMMETRY invariant (HARD) ───────────────────
  # idle_metrics.go's bucketFor pre-seeds BOTH {active,idle} for every project
  # with a desired agent, so a dashboard's idle/active division is well-defined.
  # Assert: the set of projects with an "active" agents_by_activity series equals
  # the set with an "idle" series. A project present in one but not the other is
  # a broken pre-seed — the ratio's numerator or denominator would be absent.
  blue "Tier 1D: pre-seed symmetry — every project has BOTH active and idle agents_by_activity series"
  if [ -n "$activity_lines" ]; then
    local active_projs idle_projs
    active_projs=$(grep -E 'activity="active"' <<< "$activity_lines" \
      | grep -oE 'project="[^"]*"' | sort -u)
    idle_projs=$(grep -E 'activity="idle"' <<< "$activity_lines" \
      | grep -oE 'project="[^"]*"' | sort -u)
    local only_active only_idle
    only_active=$(comm -23 <(echo "$active_projs") <(echo "$idle_projs") | grep -v '^$' || true)
    only_idle=$(comm -13 <(echo "$active_projs") <(echo "$idle_projs") | grep -v '^$' || true)
    if [ -z "$only_active" ] && [ -z "$only_idle" ]; then
      local nproj
      nproj=$(echo "$active_projs" | grep -c . || echo 0)
      pass "pre-seed symmetric across all $nproj project(s): active-set == idle-set"
    else
      fail "pre-seed symmetry" "projects with active-only: [$(echo "$only_active" | tr '\n' ' ')]; idle-only: [$(echo "$only_idle" | tr '\n' ' ')]"
    fi
  else
    fail "pre-seed symmetry" "no agents_by_activity series to check symmetry over"
  fi
  echo

  # ── Tier 1E: VALUE-JOIN invariant reserved>0 ⇒ count>0 (HARD) ──────
  # recordIdleMetrics sums a pod's memory INTO the same {project,activity} bucket
  # it counted the agent in. So a reserved_memory series with a positive value
  # MUST have a positive agents_by_activity count on the identical label pair.
  # reserved-bytes-without-a-body is a label/attribution bug — assert it can't
  # happen. Also (Tier 1F) any non-zero reserved series must be ≥ 1 MiB: a real
  # pod memory request is ≥ Mi, so a small integer would mean the emitter is
  # counting agents/pages, not bytes (a namespace/unit typo).
  blue "Tier 1E/F: value-join (reserved>0 ⇒ count>0) + reserved is a byte count (≥ 1 MiB)"
  local join_ok=1 unit_ok=1 checked=0 MIB=$((1024*1024))
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    local val proj act
    val=$(awk '{print $NF}' <<< "$line")
    # Normalize prometheus float exposition to an integer for comparison.
    val=$(python3 -c "import sys; print(int(float('$val')))" 2>/dev/null || echo 0)
    [ "$val" -gt 0 ] 2>/dev/null || continue
    checked=$((checked + 1))
    proj=$(grep -oE 'project="[^"]*"' <<< "$line" | head -1 | sed 's/project="//;s/"$//')
    act=$(grep -oE 'activity="[^"]*"'  <<< "$line" | head -1 | sed 's/activity="//;s/"$//')
    # matching count series must be > 0
    local cnt_line cnt
    cnt_line=$(grep -E "^gardenio_agents_by_activity\{[^}]*project=\"${proj}\"[^}]*activity=\"${act}\"" <<< "$activity_lines" | head -1 || true)
    # label order can vary; try the other ordering too
    [ -z "$cnt_line" ] && cnt_line=$(grep -E "^gardenio_agents_by_activity\{[^}]*activity=\"${act}\"[^}]*project=\"${proj}\"" <<< "$activity_lines" | head -1 || true)
    cnt=$(awk '{print $NF}' <<< "$cnt_line" 2>/dev/null || echo 0)
    cnt=$(python3 -c "import sys; print(int(float('${cnt:-0}')))" 2>/dev/null || echo 0)
    if ! [ "$cnt" -gt 0 ] 2>/dev/null; then
      join_ok=0
      dim "    reserved>0 but count=0 for {project=$proj,activity=$act} (reserved=$val)"
    fi
    if [ "$val" -lt "$MIB" ]; then
      unit_ok=0
      dim "    reserved=$val for {project=$proj,activity=$act} is < 1 MiB — byte/unit typo?"
    fi
  done <<< "$reserved_lines"

  if [ "$checked" -eq 0 ]; then
    # No non-zero reserved series (a fully-drained/cold controller). The invariant
    # is vacuously true; note it rather than asserting on absent data.
    dim "  (no non-zero reserved_memory series present — invariants vacuously hold on this controller)"
    pass "value-join invariant: no counterexample (no non-zero reserved series to check)"
  else
    if [ "$join_ok" = 1 ]; then
      pass "value-join invariant holds over $checked non-zero reserved series (reserved>0 ⇒ count>0)"
    else
      fail "value-join invariant" "a reserved_memory>0 series had a zero/absent agents_by_activity count on the same {project,activity}"
    fi
    if [ "$unit_ok" = 1 ]; then
      pass "reserved_memory is a byte count: every non-zero series ≥ 1 MiB"
    else
      fail "reserved_memory unit" "a non-zero reserved series is < 1 MiB — likely counting agents/pages, not bytes"
    fi
  fi
  echo

  # ── Tier 2: drive a SPEND-SAFE agent for OUR ephemeral project ─────
  blue "Tier 2: seed a spend-safe agent so both activity values + reserved-memory carry our project's data"
  setup_beads_pf
  # Poll the daemon health endpoint for a few seconds — a freshly-created
  # port-forward can take longer than the initial `sleep 3` to accept
  # connections on a slow/cold cluster, and a single-shot curl here would
  # spuriously PENDING an otherwise-healthy daemon (ensure_beads_pf re-forwards
  # if the pf itself died).
  local health_ok=0 h_deadline=$(( $(date +%s) + 30 ))
  while [ "$(date +%s)" -lt "$h_deadline" ]; do
    ensure_beads_pf
    if curl -sf --max-time 5 -o /dev/null "${BEADS_HTTP_URL}/v1/health" 2>/dev/null; then
      health_ok=1
      break
    fi
    sleep 3
  done
  if [ "$health_ok" != 1 ]; then
    pending_or_fail "spend-safe drive" "beads daemon ${BEADS_HTTP_URL} unreachable — cannot seed an agent bead"
    summary; return
  fi

  local AGENT1="idleactive-${TEST_TAG}-a"
  local AGENT1_ID
  AGENT1_ID=$(seed_agent "$AGENT1" "$ROLE" "working")
  if [ -z "$AGENT1_ID" ]; then
    pending_or_fail "seed agent bead" "daemon POST /v1/beads returned no id (write rejected?)"
    summary; return
  fi
  CREATED_BEADS+=("$AGENT1_ID")
  pass "seeded working agent bead ($AGENT1_ID, project=$PROJECT_NAME, image=registry.invalid → spend-safe)"

  # ── Tier 2G: our project appears with BOTH activity buckets pre-seeded ──
  # agents_by_activity is driven purely from the desired bead map (no pod), so
  # our project should appear on the NEXT reconcile tick with active=1 (the
  # working seed) AND idle=0 (pre-seeded). Wait a few ticks (default 60s each).
  blue "  awaiting our project in gardenio_agents_by_activity (both buckets, ≤150s)..."
  local proj_seen=0 both_buckets=0 g_deadline=$(( $(date +%s) + 150 ))
  local dbody=""
  while [ "$(date +%s)" -lt "$g_deadline" ]; do
    dbody=$(scrape_metrics)
    if grep -qE "^gardenio_agents_by_activity\{[^}]*project=\"${PROJECT_NAME}\"" <<< "$dbody"; then
      proj_seen=1
      local has_active has_idle
      has_active=$(grep -E "^gardenio_agents_by_activity\{[^}]*project=\"${PROJECT_NAME}\"" <<< "$dbody" | grep -cE 'activity="active"' || true)
      has_idle=$(grep -E "^gardenio_agents_by_activity\{[^}]*project=\"${PROJECT_NAME}\"" <<< "$dbody" | grep -cE 'activity="idle"' || true)
      if [ "$has_active" -ge 1 ] && [ "$has_idle" -ge 1 ]; then
        both_buckets=1
        break
      fi
    fi
    sleep 6
  done
  [ -z "$dbody" ] && dbody=$(scrape_metrics)

  if [ "$proj_seen" = 1 ]; then
    pass "our project present in gardenio_agents_by_activity"
    if [ "$both_buckets" = 1 ]; then
      pass "both activity buckets pre-seeded for our project (active AND idle series) — the acceptance 'activity label takes both idle and active values'"
      # active count should be ≥ 1 (our working seed).
      local a_line a_val
      a_line=$(grep -E "^gardenio_agents_by_activity\{[^}]*project=\"${PROJECT_NAME}\"" <<< "$dbody" | grep -E 'activity="active"' | head -1)
      a_val=$(awk '{print $NF}' <<< "$a_line" | python3 -c "import sys; print(int(float(sys.stdin.read() or '0')))" 2>/dev/null || echo 0)
      if [ "$a_val" -ge 1 ] 2>/dev/null; then
        pass "active count ≥ 1 for our project (our working seed is classified active): $a_line"
      else
        pending_or_fail "active count for our project" "expected active≥1 (working seed), got: ${a_line:-<absent>}"
      fi
    else
      pending_or_fail "both activity buckets for our project" "only one activity bucket present within budget (pre-seed lag?)"
    fi
  else
    pending_or_fail "our project in agents_by_activity" "project=$PROJECT_NAME never appeared within budget (reconcile-tick lag / SSE lag)"
    summary; return
  fi
  echo

  # ── Tier 2H: reserved_memory>0 for our project + MOVES with agent count ──
  # The working seed's pod (registry.invalid ⇒ Pending, never starts) still
  # RESERVES its 8Gi memory request, so reserved_memory{our-project,active}
  # becomes > 0 one tick after the pod is created. Then seed a SECOND agent and
  # assert reserved goes UP — the "moves with agent count" acceptance.
  blue "Tier 2H: reserved_memory>0 for our project, then MOVES UP with a 2nd agent (≤180s)..."
  local r1=0 r1_deadline=$(( $(date +%s) + 180 ))
  while [ "$(date +%s)" -lt "$r1_deadline" ]; do
    r1=$(reserved_mem_for_project "$(scrape_metrics)" "$PROJECT_NAME")
    [ "$r1" -gt 0 ] 2>/dev/null && break
    sleep 6
  done
  if [ "$r1" -gt 0 ] 2>/dev/null; then
    pass "reserved_memory>0 for our project once the (spend-safe, never-starting) pod reserves its request: ${r1} bytes"
    if [ "$r1" -ge "$MIB" ] 2>/dev/null; then
      pass "reserved value is a real memory reservation (≥ 1 MiB): ${r1} bytes ≈ $((r1 / 1024 / 1024)) MiB"
    else
      pending_or_fail "reserved value magnitude" "reserved=${r1} < 1 MiB for a real pod request?"
    fi

    # Seed a second agent for our project and assert reserved climbs.
    local AGENT2="idleactive-${TEST_TAG}-b"
    local AGENT2_ID
    AGENT2_ID=$(seed_agent "$AGENT2" "$ROLE" "working")
    if [ -n "$AGENT2_ID" ]; then
      CREATED_BEADS+=("$AGENT2_ID")
      pass "seeded a 2nd working agent ($AGENT2_ID) — reserved should climb by one pod's request"
      blue "  awaiting reserved_memory to MOVE UP (≤180s)..."
      local r2="$r1" r2_deadline=$(( $(date +%s) + 180 ))
      while [ "$(date +%s)" -lt "$r2_deadline" ]; do
        r2=$(reserved_mem_for_project "$(scrape_metrics)" "$PROJECT_NAME")
        [ "$r2" -gt "$r1" ] 2>/dev/null && break
        sleep 6
      done
      if [ "$r2" -gt "$r1" ] 2>/dev/null; then
        pass "reserved_memory MOVED with agent count: ${r1} → ${r2} bytes (+$((r2 - r1)))"
      else
        pending_or_fail "reserved_memory moves with agent count" "reserved did not climb after 2nd seed within budget (r1=$r1, r2=$r2; 2nd pod may not be scheduled/observed yet)"
      fi
    else
      pending_or_fail "seed 2nd agent" "daemon POST /v1/beads returned no id for the 2nd agent"
    fi
  else
    pending_or_fail "reserved_memory>0 for our project" "no positive reserved series for project=$PROJECT_NAME within budget (pod not created/observed — cold ns, or pod cap saturated)"
  fi

  summary
}

main "$@"
