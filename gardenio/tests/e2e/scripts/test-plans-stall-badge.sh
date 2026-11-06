#!/usr/bin/env bash
# test-plans-stall-badge.sh — /plans ⚠ stalled-badge render E2E (ka-0000006935,
# related gd-H2TKUIm1bv; epic ka-0000000958).
#
# The /plans surface renders a ⚠ stalled badge on every plan card (index) and in
# the /plans/{id} cockpit header when a plan's work can no longer make progress
# without someone noticing. The signal is derived in plans_stall.go:
#   • deriveStall — the predicate (dead-agent step / gardener paused / no gardener),
#   • planStallDeadAgentGrace (1h) — how long an in_progress step may sit under a
#     dead/absent agent before it counts as orphaned,
#   • planStallStaleThreshold (24h) — past which the badge tints LOUD
#     (.plan-stall-badge-stale),
#   • BadgeLabel() — the three/four-word face discriminator ("· 1 dead step"),
#   • the frozen-age (AgeLabel) folded onto the badge face ("· 3h"), computed from
#     the orphaned step's UpdatedAt (ka-0000005352 iter 11),
#   • the ka-0000000734 invariant (plans_crew.go): a plan with a LIVE crew agent on
#     a step must NOT read stalled.
#
# Unit tests (plans_stall_test.go, 134 lines) cover the predicate + BadgeLabel
# projection against a fake daemon. This module is the missing render-level gate:
# it seeds real bead state against a live kbeads daemon + beads-viewer and asserts
# the RENDERED HTML — the thing an operator actually sees. It follows the
# seed-bead-state-and-assert-rendered-HTML pattern of the sibling viewer modules
# (test-plans-wake-button.sh:37) so it is safe on a viewer-only cluster.
#
# Scenarios (all assert the /plans/{id} cockpit header AND, for scenario 1, the
# /plans index card — the two surfaces share the derive but call it via different
# code paths, planStallForDetail vs planStallBulk):
#
#   1. Dead-agent orphan past the 1h grace, < 24h old → the badge renders
#      `.plan-stall-badge` with the dead-agent BadgeLabel ("· 1 dead step"), a
#      frozen-age suffix that reflects UpdatedAt ("· 3h"), and is NOT stale (no
#      `.plan-stall-badge-stale` on the badge span). Asserted on BOTH surfaces.
#   2. Frozen-age reflects UpdatedAt, not bead creation — the step is CREATED now
#      but its updated_at is backdated 3h, and the age reads "3h" (not "0m"),
#      proving the age keys off the last-move instant, not the create instant.
#   3. Aged orphan past the 24h staleness bar → the badge span gains
#      `.plan-stall-badge-stale` (loud tint) and the age reads in days ("· 2d");
#      the scenario-1 fresh orphan (3h) did NOT — the contrast pins the threshold.
#   4. NEGATIVE + load-bearing (ka-0000000734): a plan whose in_progress step is
#      backdated past grace but is claimed by a LIVE crew agent renders NO stall
#      badge. A positive CONTROL then flips the SAME crew dead (backdated) and the
#      badge reappears with "· 1 dead step" — proving the crew's liveness is what
#      suppresses the badge, not some unrelated exemption (i.e. the assertion is
#      not trivially green).
#   5. RE-SCOPE (ka-0000004259): a dead-agent orphan (step A) COEXISTING with a
#      LIVE crew on ANOTHER, OPEN step (step B, via a crew→step `assigned` edge)
#      re-scopes the loud "⚠ stalled · 1 dead step" to the quiet "⚠ 1 orphaned"
#      (HeadlineLabel/HeadlineCount; FaceLabel SUPPRESSED), beside a "· 1 working"
#      crew chip — on BOTH the index card AND the cockpit header. A non-alive
#      CONTROL (flip the crew's agent_state, NOT its freshness) flips the badge
#      BACK to loud "⚠ stalled · 1 dead step". This is the two-agent shape
#      scenario 4 does NOT build (it wires the crew as the step ASSIGNEE, clearing
#      deadSteps); step B here is OPEN so deadStepCount ignores it while the
#      assigned edge still makes Crew.HasLive() true.
#
# ── Why this module needs to backdate updated_at ──────────────────────────────
# The dead-agent conditions (scenarios 1-4) require an in_progress step whose
# updated_at is OLDER than the 1h grace (and, for scenario 3, older than 24h). The
# live kbeads daemon stamps updated_at = NOW() on every write and offers no
# override (same constraint test-stale-reaper-grace-reconcile.sh:37 documents), so
# the ONLY way to age a step past a window on demand is a direct SQL UPDATE
# against the in-chart Postgres. staging runs an in-cluster postgres pod
# (app.kubernetes.io/component=postgres); prod runs external RDS and a viewer-only
# cluster has no postgres. So the WHOLE suite gates on the backdate capability:
# when the postgres pod is absent / unreachable / not exec-able (external RDS), it
# SKIPs LOUD (still exit 0 — an environment that cannot host the fixture must
# never red the suite, ka-0000000847; but a skip is never mistaken for a pass).
#
# ── Pod-safety on a live-controller cluster ───────────────────────────────────
# staging runs a live controller that spawns a REAL pod for any OPEN
# type=agent bead (ListAgentBeads → create-missing). The crew agent this module
# seeds for scenario 4 is therefore created with a claudeless mock_scenario
# (basic-echo, zero model spend) and CLOSED IMMEDIATELY so the controller tears
# its pod down; its liveness is then set via a direct SQL write (agent_state +
# fresh updated_at) — a CLOSED bead is NOT in the reconciler's desired set, so no
# pod recurs, yet the viewer's liveness predicate (isAlive: agent_state ∈
# {working,spawning,yielding} AND fresh updated_at, status-agnostic) still reads
# it LIVE. The cleanup trap closes every seeded bead (belt-and-suspenders reaps
# any pod) and force-deletes any pod we observe.
#
# Self-SKIPs (exit 0) when the viewer Service is absent (inert in the ephemeral
# e2e namespaces, asserts only against a long-lived viewer — staging singleton
# / prod), when the daemon is unreachable / no token, or when updated_at cannot be
# backdated (see above).
#
# Idempotency: every created bead carries the title prefix plans-stall-e2e-<rand>
# and is closed in the cleanup trap. Reruns do not collide.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-plans-stall-badge.sh
#
# Prerequisites:
#   - kubectl context pointing at the target cluster (default gardenio-e2e)
#   - kd, jq, curl, python3, kubectl in PATH
#   - lib-gitlab-token.sh's mint_user_token resolves a GitLab-direct bearer (the viewer
#     GET is OIDC-gated); when a token cannot be minted the module SKIPs.
#   - an in-cluster postgres pod reachable via `kubectl exec` (for the backdate).
#
# Hard rules: do NOT edit .captain/quarantines/. Do not touch any other e2e module.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib-gitlab-token.sh"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib-viewer.sh"

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
RELEASE="${RELEASE:-${NAMESPACE}}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
VIEWER_SVC="${VIEWER_SVC:-${RELEASE}-beads-viewer}"
VIEWER_PF_PORT="${VIEWER_PF_PORT:-19821}"
BEADS_PF_PORT="${BEADS_PF_PORT:-19822}"
KD_BIN="${KD_BIN:-kd}"
PROJECT="${PLANS_STALL_PROJECT:-gardenio}"
OPERATOR_EMAIL="${OPERATOR_EMAIL:-e2e-plans-stall-op-$$@example.com}"
TITLE_PREFIX="plans-stall-e2e"
RANDOM_SUFFIX="$$-$(date +%s)"
# Claudeless mock scenario the (immediately-closed) crew pod runs — zero model
# spend, baked into the agent image at /scenarios/ (images/agent/Dockerfile).
PLANS_STALL_MOCK_SCENARIO="${PLANS_STALL_MOCK_SCENARIO:-basic-echo}"

PASS=0
FAIL=0
SKIP=0
ERRORS=()
BEADS_PF_PID=""
# Created beads + pods are tracked in FILES, not shell arrays: the seed helpers
# echo their result over stdout, so callers invoke them in $(...) — a SUBSHELL
# whose array appends never reach the parent. A file appended with `printf >>`
# survives the subshell, so the EXIT trap sees every seeded id and nothing leaks
# against the long-lived staging singleton.
CREATED_BEADS_FILE="$(mktemp /tmp/plans-stall-beads-XXXXXX)"
CREATED_PODS_FILE="$(mktemp /tmp/plans-stall-pods-XXXXXX)"
track_bead() { [ -n "${1:-}" ] && printf '%s\n' "$1" >> "$CREATED_BEADS_FILE"; }
track_pod()  { [ -n "${1:-}" ] && printf '%s\n' "$1" >> "$CREATED_PODS_FILE"; }
# Postgres pod discovered for backdating (empty until preflight succeeds).
PG_POD=""
PG_DB="${PLANS_STALL_PG_DB:-beads}"
PG_USER="${PLANS_STALL_PG_USER:-postgres}"

export KD_ACTOR="${KD_ACTOR:-e2e-test}"
# kd/gb mutations need a non-empty operated_by (ka-0000005772).
export BEADS_OPERATED_BY="${BEADS_OPERATED_BY:-oidc:${OPERATOR_EMAIL}}"

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
  # Close beads FIRST: a live controller reaps a closed agent bead's pod, so this
  # also tears down any real pod a seeded crew brought up. Then force-delete any
  # pod we observed, in case no controller is watching (belt-and-suspenders).
  if [ -f "$CREATED_BEADS_FILE" ]; then
    while IFS= read -r id; do
      [ -n "$id" ] || continue
      "$KD_BIN" close "$id" >/dev/null 2>&1 || true
    done < "$CREATED_BEADS_FILE"
  fi
  if [ -f "$CREATED_PODS_FILE" ]; then
    while IFS= read -r entry; do
      [ -n "$entry" ] || continue
      local pns="${entry%%/*}" pname="${entry#*/}"
      kubectl -n "$pns" delete pod "$pname" --grace-period=0 --force \
        >/dev/null 2>&1 || true
    done < "$CREATED_PODS_FILE"
  fi
  rm -f "$CREATED_BEADS_FILE" "$CREATED_PODS_FILE" 2>/dev/null || true
  [ -n "${VIEWER_PF_PID:-}" ] && kill "$VIEWER_PF_PID" 2>/dev/null || true
  [ -n "$BEADS_PF_PID"  ] && kill "$BEADS_PF_PID"  2>/dev/null || true
}
trap cleanup EXIT

setup_beads_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    dim "  using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return 0
  fi
  blue "Port-forwarding ${NAMESPACE}/${BEADS_SVC} :${BEADS_PF_PORT} → 8080..."
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" \
    "${BEADS_PF_PORT}:8080" >/tmp/plans-stall-beads-pf.log 2>&1 &
  BEADS_PF_PID=$!
  sleep 3
  export BEADS_HTTP_URL="http://localhost:${BEADS_PF_PORT}"
  if [ -z "${BEADS_TOKEN:-}" ]; then
    local secret
    secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
      -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
    [ -n "$secret" ] && export BEADS_TOKEN="$secret"
  fi
}

bead_id_from_json() { printf '%s' "$1" | jq -r '.id // empty' 2>/dev/null || printf ''; }
bead_id_from_create() { grep -oE '(gd|ka)-[A-Za-z0-9]+' <<<"$1" | head -1; }

# discover_postgres_pod — the in-chart postgres pod name, or "" when absent
# (external RDS / viewer-only cluster). Sets PG_POD as a side effect.
discover_postgres_pod() {
  PG_POD=$(kubectl -n "$NAMESPACE" get pods \
    -l app.kubernetes.io/component=postgres \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
  [ -n "$PG_POD" ]
}

# backdate_updated_at <bead-id> <interval> — SET updated_at = NOW() - <interval>
# on the bead, straight in Postgres (the daemon offers no updated_at override).
# <interval> is a Postgres interval literal, e.g. "3 hours" / "48 hours".
# Returns non-zero on failure.
backdate_updated_at() {
  local id="$1" interval="$2"
  kubectl -n "$NAMESPACE" exec "$PG_POD" -c postgres -- \
    psql -U "$PG_USER" -d "$PG_DB" -tAc \
    "UPDATE beads SET updated_at = NOW() - INTERVAL '${interval}' WHERE id = '${id}';" \
    >/dev/null 2>&1
}

# set_agent_live <bead-id> — mark a (closed) agent bead LIVE in Postgres:
# agent_state=working AND a fresh updated_at. The viewer's liveness predicate is
# status-agnostic (agent_state ∈ alive set AND fresh), so a CLOSED bead set this
# way reads live WITHOUT the reconciler re-spawning a pod (a closed bead is not in
# its desired set). Returns non-zero on failure.
set_agent_live() {
  local id="$1"
  kubectl -n "$NAMESPACE" exec "$PG_POD" -c postgres -- \
    psql -U "$PG_USER" -d "$PG_DB" -tAc \
    "UPDATE beads SET updated_at = NOW(), fields = jsonb_set(coalesce(fields,'{}'::jsonb),'{agent_state}','\"working\"') WHERE id = '${id}';" \
    >/dev/null 2>&1
}

# set_agent_state <bead-id> <state> — set the agent bead's agent_state to <state>
# with a FRESH updated_at, in Postgres. Scenario 5's negative control flips a
# working crew to a NON-alive state (e.g. "idle") to drop it off Crew.HasLive().
# This is the CORRECT control: the crew rollup's "N working" (planCrewBulk) reads
# agentStateBucket (agent_state ∈ {working,spawning,yielding}), which is
# freshness-AGNOSTIC — so aging the heartbeat would NOT flip HasLive() and the
# re-scoped badge would stay "orphaned" (a vacuously-green control). Flipping the
# STATE is what actually clears the coexisting-live-crew condition. Returns
# non-zero on failure.
set_agent_state() {
  local id="$1" state="$2"
  kubectl -n "$NAMESPACE" exec "$PG_POD" -c postgres -- \
    psql -U "$PG_USER" -d "$PG_DB" -tAc \
    "UPDATE beads SET updated_at = NOW(), fields = jsonb_set(coalesce(fields,'{}'::jsonb),'{agent_state}','\"${state}\"') WHERE id = '${id}';" \
    >/dev/null 2>&1
}

# seed_plan_with_step <title-tag> <assignee> <backdate-interval> — create a plan
# epic (plan label) + one in_progress step assigned to <assignee>, wire the
# parent-child edge, and backdate the step's updated_at by <backdate-interval>.
# Echoes "<plan-id> <step-id>"; appends both to CREATED_BEADS. Empty on failure.
seed_plan_with_step() {
  local tag="$1" assignee="$2" interval="$3"
  local plan step
  plan=$(bead_id_from_json "$("$KD_BIN" create "${TITLE_PREFIX}-${tag}-plan-${RANDOM_SUFFIX}" \
    --type=epic --project "$PROJECT" -l "plan" --no-parent \
    -d "stall-badge e2e ${tag} plan (${RANDOM_SUFFIX})" --json 2>/dev/null || printf '{}')")
  [ -n "$plan" ] || { printf ''; return 1; }
  track_bead "$plan"
  step=$(bead_id_from_create "$("$KD_BIN" create "${TITLE_PREFIX}-${tag}-step-${RANDOM_SUFFIX}" \
    --type=task --project "$PROJECT" --no-parent 2>&1)")
  [ -n "$step" ] || { printf ''; return 1; }
  track_bead "$step"
  "$KD_BIN" dep add "$step" "$plan" --type parent-child >/dev/null 2>&1 || true
  "$KD_BIN" update "$step" --status in_progress --assignee "$assignee" >/dev/null 2>&1 || true
  # Backdate AFTER the status/assignee write (which stamps a fresh updated_at).
  backdate_updated_at "$step" "$interval" || { printf ''; return 1; }
  printf '%s %s' "$plan" "$step"
}

# seed_orphan_plus_crew_plan <ghost-assignee> <crew-bead-id> — the scenario-5
# fixture: a plan epic (plan label) with TWO steps —
#   step A: in_progress, assignee=<ghost-assignee> (no agent bead → dead),
#           updated_at backdated −3h (past the 1h grace) → deadStepCount == 1,
#   step B: OPEN (not in_progress), wired to <crew-bead-id> via a
#           crew --assigned--> stepB edge → the crew rollup counts the crew as
#           working, but deadStepCount (which inspects ONLY in_progress steps)
#           never sees step B, so deadSteps stays 1.
# The coexistence (deadSteps==1 AND Crew.HasLive()) is exactly what
# reconcileStallWithCrew re-scopes to "orphaned". Step B is OPEN on purpose: an
# in_progress step-B assigned to the crew would clear deadSteps (scenario 4's
# path); a no-assignee in_progress step-B would count as a SECOND orphan. The
# assigned edge points FROM the crew's BEAD ID (planCrewBulk hydrates the reverse
# dep's BeadID as the crew agent), so the caller passes seed_live_crew's returned
# id, not the name. Echoes "<plan-id> <stepA-id> <stepB-id>"; tracks every bead.
# Empty on failure.
seed_orphan_plus_crew_plan() {
  local ghost="$1" crew_id="$2"
  local plan stepA stepB
  plan=$(bead_id_from_json "$("$KD_BIN" create "${TITLE_PREFIX}-orphanx-plan-${RANDOM_SUFFIX}" \
    --type=epic --project "$PROJECT" -l "plan" --no-parent \
    -d "stall-badge e2e orphaned-rescope plan (${RANDOM_SUFFIX})" --json 2>/dev/null || printf '{}')")
  [ -n "$plan" ] || { printf ''; return 1; }
  track_bead "$plan"
  # Step A: the dead-agent orphan.
  stepA=$(bead_id_from_create "$("$KD_BIN" create "${TITLE_PREFIX}-orphanx-stepA-${RANDOM_SUFFIX}" \
    --type=task --project "$PROJECT" --no-parent 2>&1)")
  [ -n "$stepA" ] || { printf ''; return 1; }
  track_bead "$stepA"
  "$KD_BIN" dep add "$stepA" "$plan" --type parent-child >/dev/null 2>&1 || true
  "$KD_BIN" update "$stepA" --status in_progress --assignee "$ghost" >/dev/null 2>&1 || true
  backdate_updated_at "$stepA" "3 hours" || { printf ''; return 1; }
  # Step B: the OPEN step the live crew is assigned to (stays open — never
  # transitioned to in_progress, so deadStepCount ignores it).
  stepB=$(bead_id_from_create "$("$KD_BIN" create "${TITLE_PREFIX}-orphanx-stepB-${RANDOM_SUFFIX}" \
    --type=task --project "$PROJECT" --no-parent 2>&1)")
  [ -n "$stepB" ] || { printf ''; return 1; }
  track_bead "$stepB"
  "$KD_BIN" dep add "$stepB" "$plan" --type parent-child >/dev/null 2>&1 || true
  # The load-bearing edge: crew --assigned--> stepB. The crew rollup reads the
  # step's REVERSE deps for `assigned` edges (planCrewBulk), so this is what makes
  # Crew.HasLive() true and triggers the re-scope.
  "$KD_BIN" dep add "$crew_id" "$stepB" --type assigned >/dev/null 2>&1 || true
  printf '%s %s %s' "$plan" "$stepA" "$stepB"
}

# seed_live_crew <name> — create a claudeless crew agent bead, CLOSE it (so the
# controller tears down any spawned pod and the reconciler won't re-spawn), then
# set it LIVE via SQL. Echoes the bead id; appends it (and any pod) to cleanup.
# Empty on failure.
seed_live_crew() {
  local name="$1"
  local out id
  out=$("$KD_BIN" create "$name" \
    --type=agent --project "$PROJECT" -l "role:crew" --no-parent \
    -f "agent=${name}" -f "role=crew" -f "project=${PROJECT}" \
    -f "agent_state=working" -f "mock_scenario=${PLANS_STALL_MOCK_SCENARIO}" \
    -d "stall-badge e2e live crew (${RANDOM_SUFFIX})" --json 2>/dev/null || printf '{}')
  id=$(bead_id_from_json "$out")
  [ -n "$id" ] || { printf ''; return 1; }
  track_bead "$id"
  # Close immediately: AgentDone tears the spawned pod down and drops the bead out
  # of the reconciler's desired set so no create-missing spawn recurs.
  "$KD_BIN" close "$id" >/dev/null 2>&1 || true
  # Track the canonical pod name for a belt-and-suspenders reap (best-effort — the
  # close above is the primary teardown).
  track_pod "${NAMESPACE}/crew-${PROJECT}-crew-${name#"${PROJECT}-"}"
  set_agent_live "$id" || { printf ''; return 1; }
  printf '%s' "$id"
}

# fetch_detail <plan-id> <token> — GET the /plans/{id} cockpit HTML into a temp
# file and echo the FILE PATH (the cockpit page can exceed MAX_ARG_STRLEN).
fetch_detail() {
  local plan="$1" token="$2" f
  f=$(mktemp /tmp/plans-stall-detail-XXXXXX)
  curl -sk --max-time 12 \
    -H "Authorization: Bearer ${token}" -H "Accept: text/html" \
    -o "$f" \
    "http://localhost:${VIEWER_PF_PORT}/plans/${plan}" 2>/dev/null || true
  printf '%s' "$f"
}

# stall_badge_span <html-file> — echo the single rendered stall-badge SPAN
# (`<span class="plan-stall-badge ...">…</span>`), or "" when absent. Matches the
# element only, never the .plan-stall-badge* CSS rules in the <style> block (which
# would false-positive a raw `grep plan-stall-badge`).
stall_badge_span() {
  python3 - "$1" <<'PY'
import re, sys
try:
    html = open(sys.argv[1], encoding='utf-8', errors='replace').read()
except OSError:
    print(""); sys.exit(0)
m = re.search(r'<span class="plan-stall-badge[^"]*"[^>]*>.*?</span>\s*</span>', html, re.DOTALL)
if not m:
    m = re.search(r'<span class="plan-stall-badge[^"]*"[^>]*>.*?</span>', html, re.DOTALL)
print(m.group(0) if m else "")
PY
}

# index_card_badge <plan-id> <token> — echo the stall-badge SPAN as rendered on
# the /plans INDEX card for <plan-id> (planStallBulk path), or "" when absent.
# Scoped to the card region so a badge on a DIFFERENT plan can't leak in.
index_card_badge() {
  local plan="$1" token="$2" f
  f=$(mktemp /tmp/plans-stall-index-XXXXXX)
  curl -sk --max-time 15 \
    -H "Authorization: Bearer ${token}" -H "Accept: text/html" \
    -o "$f" \
    "http://localhost:${VIEWER_PF_PORT}/plans?project=${PROJECT}&operated_by=" 2>/dev/null || true
  python3 - "$f" "$plan" <<'PY'
import re, sys
try:
    html = open(sys.argv[1], encoding='utf-8', errors='replace').read()
except OSError:
    print(""); sys.exit(0)
pid = sys.argv[2]
# The card is a <li class="plan-card" ... data-plan-id="pid"> ... </li>.
m = re.search(r'<li class="plan-card"[^>]*data-plan-id="%s".*?</li>' % re.escape(pid),
              html, re.DOTALL)
if not m:
    print(""); sys.exit(0)
card = m.group(0)
b = re.search(r'<span class="plan-stall-badge[^"]*"[^>]*>.*?</span>\s*</span>', card, re.DOTALL)
if not b:
    b = re.search(r'<span class="plan-stall-badge[^"]*"[^>]*>.*?</span>', card, re.DOTALL)
print(b.group(0) if b else "")
PY
  rm -f "$f"
}

# index_card_html <plan-id> <token> — echo the WHOLE /plans INDEX card <li> for
# <plan-id> (planStallBulk + planCrewBulk paths), or "" when absent. Scenario 5
# asserts the stall badge AND the crew-working chip COEXIST on the SAME card (the
# coexistence the re-scope exists to make coherent), so it needs the whole card
# region, not just the badge span index_card_badge returns.
index_card_html() {
  local plan="$1" token="$2" f
  f=$(mktemp /tmp/plans-stall-cardhtml-XXXXXX)
  curl -sk --max-time 15 \
    -H "Authorization: Bearer ${token}" -H "Accept: text/html" \
    -o "$f" \
    "http://localhost:${VIEWER_PF_PORT}/plans?project=${PROJECT}&operated_by=" 2>/dev/null || true
  python3 - "$f" "$plan" <<'PY'
import re, sys
try:
    html = open(sys.argv[1], encoding='utf-8', errors='replace').read()
except OSError:
    print(""); sys.exit(0)
pid = sys.argv[2]
m = re.search(r'<li class="plan-card"[^>]*data-plan-id="%s".*?</li>' % re.escape(pid),
              html, re.DOTALL)
print(m.group(0) if m else "")
PY
  rm -f "$f"
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " /plans ⚠ stalled-badge render E2E (ka-0000006935)"
  blue "═══════════════════════════════════════════════════"

  if ! setup_viewer_portforward; then
    skip "plans-stall-badge module" "viewer service $VIEWER_SVC absent"
    summary
    return 0
  fi
  setup_beads_portforward

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    skip "plans-stall-badge module" \
      "kd cannot reach beads daemon at ${BEADS_HTTP_URL:-unset}"
    summary
    return 0
  fi

  # The dead-agent fixture needs a step aged past the 1h/24h windows, which the
  # daemon won't let us set — SO the whole suite gates on an exec-able in-chart
  # Postgres. External RDS / viewer-only clusters cannot host the fixture.
  if ! command -v kubectl >/dev/null 2>&1; then
    skip "plans-stall-badge module" "kubectl not installed (need SQL backdate of updated_at)"
    summary
    return 0
  fi
  if ! discover_postgres_pod; then
    skip "plans-stall-badge module" \
      "no in-chart postgres pod in $NAMESPACE (external RDS?) — cannot backdate updated_at past the stall grace"
    summary
    return 0
  fi
  if ! kubectl -n "$NAMESPACE" exec "$PG_POD" -c postgres -- \
        psql -U "$PG_USER" -d "$PG_DB" -tAc "SELECT 1;" >/dev/null 2>&1; then
    skip "plans-stall-badge module" \
      "cannot exec psql in ${NAMESPACE}/${PG_POD} — cannot backdate updated_at"
    summary
    return 0
  fi
  dim "  backdate substrate present — postgres pod ${NAMESPACE}/${PG_POD}"

  # ── Mint an operator token for the OIDC-gated viewer GET. ─────────
  local op_token=""
  if ! op_token=$(mint_user_token "$OPERATOR_EMAIL" 2>/tmp/plans-stall-mint.err); then
    yellow "mint_user_token failed for ${OPERATOR_EMAIL} — see /tmp/plans-stall-mint.err"
    skip "scenario 1: dead-agent orphan badge" "no usable GitLab-direct bearer"
    skip "scenario 2: frozen-age reflects UpdatedAt" "no usable GitLab-direct bearer"
    skip "scenario 3: aged orphan loud stale tint" "no usable GitLab-direct bearer"
    skip "scenario 4: live crew suppresses badge"  "no usable GitLab-direct bearer"
    skip "scenario 5: orphaned re-scope"           "no usable GitLab-direct bearer"
    summary
    return 0
  fi
  dim "  minted operator token (sub=${OPERATOR_EMAIL})"
  # The viewer's own OIDC middleware stays pinned to the Keycloak issuer until
  # the viewer-login flip lands (ka-0000002083); it rejects this GitLab-direct /
  # static-daemon bearer with 401. SKIP-clean rather than hard-fail every gated
  # /plans GET (bug gd-5M8iT2oNlr). Auto-enables when the viewer trusts the
  # GitLab edge.
  if ! viewer_accepts_bearer "$op_token" "/plans?project=${PROJECT}&operated_by="; then
    skip "plans-stall-badge module" "viewer rejects the E2E transport bearer — viewer still OIDC-gated at the pod; viewer-login flip pending (ka-0000002083)"
    summary
    return 0
  fi

  scenario_dead_agent_orphan "$op_token"
  scenario_aged_orphan_stale "$op_token"
  scenario_live_crew_negative "$op_token"
  scenario_orphaned_rescope "$op_token"

  summary
}

# ══════════════════════════════════════════════════════════════════
# Scenarios 1 & 2: dead-agent orphan past the 1h grace (< 24h) renders the badge
# with the "· 1 dead step" discriminator + a frozen-age that reflects UpdatedAt
# ("· 3h"), on BOTH the cockpit header and the index card, and is NOT stale.
# ══════════════════════════════════════════════════════════════════
scenario_dead_agent_orphan() {
  local token="$1"
  blue "Scenario 1+2: dead-agent orphan (3h past grace) → badge '· 1 dead step · 3h', not stale, both surfaces"

  # A ghost assignee: an agent-shaped name with no agent bead → the viewer's
  # liveness lookup resolves it as absent → the step counts as a dead orphan.
  local ghost="gardenio-stall-ghost-${RANDOM_SUFFIX}"
  local ids plan step
  ids=$(seed_plan_with_step "orphan" "$ghost" "3 hours") || true
  plan=$(awk '{print $1}' <<<"$ids")
  step=$(awk '{print $2}' <<<"$ids")
  if [ -z "$plan" ] || [ -z "$step" ]; then
    fail "scenario 1: seed dead-agent orphan plan" "seed returned no ids (plan=$plan step=$step)"
    return 0
  fi
  pass "seeded orphan plan ($plan) + in_progress step ($step, ghost assignee, updated_at −3h)"

  # ── Cockpit header (planStallForDetail path). ────────────────────
  local dfile badge
  dfile=$(fetch_detail "$plan" "$token")
  badge=$(stall_badge_span "$dfile")
  rm -f "$dfile"
  if [ -z "$badge" ]; then
    fail "scenario 1: cockpit renders the stall badge" \
      "no .plan-stall-badge span on /plans/${plan}"
  else
    pass "scenario 1: cockpit renders the ⚠ stall badge"
    if grep -q 'plan-stall-badge-reason">· 1 dead step' <<<"$badge"; then
      pass "scenario 1: badge face carries the dead-agent BadgeLabel '· 1 dead step'"
    else
      fail "scenario 1: badge face carries '· 1 dead step'" "badge: $badge"
    fi
    # Frozen-age (scenario 2): reflects updated_at (−3h), NOT create (~0m).
    if grep -qE 'plan-stall-badge-age">· 3h' <<<"$badge"; then
      pass "scenario 2: frozen-age reflects UpdatedAt (· 3h), not bead creation"
    else
      fail "scenario 2: frozen-age reflects UpdatedAt (· 3h)" \
        "badge age not '· 3h' (created now, updated_at −3h): $badge"
    fi
    # A 3h orphan is past grace but well under the 24h stale bar — the badge span
    # must NOT carry the loud-tint modifier.
    if grep -q 'plan-stall-badge-stale' <<<"$badge"; then
      fail "scenario 3 (contrast): a 3h orphan must NOT be stale" \
        "badge span carries .plan-stall-badge-stale at 3h: $badge"
    else
      pass "scenario 3 (contrast): a 3h orphan is NOT loud-tinted (no .plan-stall-badge-stale)"
    fi
  fi

  # ── Index card (planStallBulk path) — the OTHER derive code path. ─
  local ibadge
  ibadge=$(index_card_badge "$plan" "$token")
  if [ -z "$ibadge" ]; then
    fail "scenario 1: index card renders the stall badge" \
      "no .plan-stall-badge span on the /plans card for ${plan}"
  else
    pass "scenario 1: index card renders the ⚠ stall badge (planStallBulk path)"
    if grep -q 'plan-stall-badge-reason">· 1 dead step' <<<"$ibadge" \
       && grep -qE 'plan-stall-badge-age">· 3h' <<<"$ibadge"; then
      pass "scenario 1: index badge carries '· 1 dead step · 3h' (agrees with cockpit)"
    else
      fail "scenario 1: index badge carries '· 1 dead step · 3h'" "index badge: $ibadge"
    fi
  fi
  echo
}

# ══════════════════════════════════════════════════════════════════
# Scenario 3: an orphan aged past the 24h staleness bar tints the badge LOUD
# (.plan-stall-badge-stale) and reads its age in days ("· 2d"). Contrast with
# scenario 1's fresh 3h orphan (asserted NOT stale there) pins the threshold.
# ══════════════════════════════════════════════════════════════════
scenario_aged_orphan_stale() {
  local token="$1"
  blue "Scenario 3: aged orphan (48h) → loud stale tint (.plan-stall-badge-stale · 2d)"

  local ghost="gardenio-stall-ghost-stale-${RANDOM_SUFFIX}"
  local ids plan step
  ids=$(seed_plan_with_step "stale" "$ghost" "48 hours") || true
  plan=$(awk '{print $1}' <<<"$ids")
  step=$(awk '{print $2}' <<<"$ids")
  if [ -z "$plan" ] || [ -z "$step" ]; then
    fail "scenario 3: seed aged orphan plan" "seed returned no ids (plan=$plan step=$step)"
    return 0
  fi
  pass "seeded aged orphan plan ($plan) + step ($step, updated_at −48h)"

  local dfile badge
  dfile=$(fetch_detail "$plan" "$token")
  badge=$(stall_badge_span "$dfile")
  rm -f "$dfile"
  if [ -z "$badge" ]; then
    fail "scenario 3: cockpit renders the stall badge on the aged orphan" \
      "no .plan-stall-badge span on /plans/${plan}"
    echo
    return 0
  fi
  if grep -q 'class="plan-stall-badge plan-stall-badge-stale"' <<<"$badge"; then
    pass "scenario 3: badge span gains the loud .plan-stall-badge-stale modifier past 24h"
  else
    fail "scenario 3: aged orphan badge is loud-tinted (.plan-stall-badge-stale)" \
      "badge span not stale at 48h: $badge"
  fi
  if grep -qE 'plan-stall-badge-age">· 2d' <<<"$badge"; then
    pass "scenario 3: frozen-age reads in days (· 2d)"
  else
    fail "scenario 3: frozen-age reads '· 2d'" "badge age not '· 2d' at 48h: $badge"
  fi
  echo
}

# ══════════════════════════════════════════════════════════════════
# Scenario 4: NEGATIVE + load-bearing (ka-0000000734). A plan whose in_progress
# step is backdated past grace but is claimed by a LIVE crew agent renders NO
# stall badge. A positive CONTROL then flips the SAME crew dead and the badge
# reappears — proving the crew's liveness is what suppresses it.
# ══════════════════════════════════════════════════════════════════
scenario_live_crew_negative() {
  local token="$1"
  blue "Scenario 4: live crew on a past-grace step suppresses the badge (ka-0000000734), + dead-crew control"

  local crew_name="${PROJECT}-stall-crew-${RANDOM_SUFFIX}"
  local crew_id
  crew_id=$(seed_live_crew "$crew_name") || true
  if [ -z "$crew_id" ]; then
    fail "scenario 4: seed live crew agent" "seed_live_crew returned no id"
    echo
    return 0
  fi
  pass "seeded live crew ($crew_id, name=$crew_name, closed+working+fresh → reads live, no pod)"

  local ids plan step
  ids=$(seed_plan_with_step "crew" "$crew_name" "3 hours") || true
  plan=$(awk '{print $1}' <<<"$ids")
  step=$(awk '{print $2}' <<<"$ids")
  if [ -z "$plan" ] || [ -z "$step" ]; then
    fail "scenario 4: seed crew-claimed plan" "seed returned no ids (plan=$plan step=$step)"
    echo
    return 0
  fi
  # seed_plan_with_step backdated the step to −3h; re-assert the crew is live (the
  # step writes above do not touch the crew bead, but re-stamp to be safe).
  set_agent_live "$crew_id" || true
  pass "seeded crew-claimed plan ($plan) + step ($step, past grace, assigned to the live crew)"

  # ── A) Live crew → NO badge. ─────────────────────────────────────
  local dfile badge
  dfile=$(fetch_detail "$plan" "$token")
  badge=$(stall_badge_span "$dfile")
  rm -f "$dfile"
  if [ -z "$badge" ]; then
    pass "scenario 4: NO stall badge while the crew is live (ka-0000000734 invariant holds)"
  else
    fail "scenario 4: live crew must suppress the stall badge" \
      "a badge rendered despite a live crew on the step: $badge"
  fi

  # ── B) Positive control: flip the SAME crew dead → badge reappears. ─
  # Backdate the crew agent's updated_at past the 1h stale threshold so the viewer
  # reads it NOT live. Nothing else changes — so a badge appearing now proves the
  # crew's liveness (not some unrelated exemption) was suppressing it.
  if ! backdate_updated_at "$crew_id" "3 hours"; then
    skip "scenario 4: dead-crew control" "could not backdate the crew agent to flip it dead"
    echo
    return 0
  fi
  dfile=$(fetch_detail "$plan" "$token")
  badge=$(stall_badge_span "$dfile")
  rm -f "$dfile"
  if [ -z "$badge" ]; then
    fail "scenario 4 (control): flipping the crew dead must re-raise the badge" \
      "no badge after the crew went stale — the negative assertion may be trivially green"
  else
    pass "scenario 4 (control): flipping the SAME crew dead re-raises the badge (crew liveness is load-bearing)"
    if grep -q 'plan-stall-badge-reason">· 1 dead step' <<<"$badge"; then
      pass "scenario 4 (control): the re-raised badge reads '· 1 dead step'"
    else
      fail "scenario 4 (control): re-raised badge reads '· 1 dead step'" "badge: $badge"
    fi
  fi
  echo
}

# ══════════════════════════════════════════════════════════════════
# Scenario 5: the iteration-23 "⚠ N orphaned" RE-SCOPE (ka-0000005352, task
# ka-0000004259). A dead-agent orphan (step A) COEXISTING with a live crew on
# ANOTHER step (step B) re-scopes the loud plan-level "⚠ stalled · 1 dead step"
# to the quiet per-step "⚠ 1 orphaned" — killing the self-contradicting card
# that reads "· 1 working · ⚠ stalled · 1 dead step". Runtime:
# reconcileStallWithCrew (plans_stall.go:521) flips Orphaned when Stalled &&
# DeadSteps>=1 && Crew.HasLive(); HeadlineLabel/HeadlineCount render "N orphaned"
# and FaceLabel is SUPPRESSED (no "· N dead step" reason). Rendered on BOTH the
# index card (plan_card.html:137, planStallBulk + planCrewBulk) and the cockpit
# header (plan_detail.html:616, planStallForDetail + planCrew) off the SAME
# projection — so a template drift on one surface is otherwise unguarded.
#
# The re-scope was ENTIRELY uncovered before this scenario: every `orphaned` hit
# in the e2e corpus was a comment, and the two adjacent modules both route around
# the branch in writing (plans_signal_strip.spec.js:303 asserts the UN-rescoped
# "⚠ stalled"; scenario 4 above seeds a dead step + live crew but wires the crew
# as the step ASSIGNEE — clearing deadSteps — and asserts NO badge, never the
# re-scope). This fixture is the two-agent shape neither builds: step A orphaned
# under a dead ghost + step B OPEN with a crew on an `assigned` edge.
#
# NEGATIVE CONTROL (required, else this degrades to "a badge renders"): flip the
# crew's agent_state to a NON-alive value (idle) → Crew.HasLive() false → the
# badge flips BACK to the loud "⚠ stalled · 1 dead step". NOTE this control flips
# the STATE, not freshness: Crew.HasLive() (planCrewBulk "N working") reads
# agentStateBucket, which is freshness-AGNOSTIC — aging the crew's heartbeat would
# leave HasLive() true and the badge stuck on "orphaned" (a vacuously-green
# control). Verified against the live render path before authoring (ka-0000004259
# comment 156041).
# ══════════════════════════════════════════════════════════════════
scenario_orphaned_rescope() {
  local token="$1"
  blue "Scenario 5: dead-agent orphan + live crew on another step → '⚠ 1 orphaned' re-scope (ka-0000004259), + non-alive control"

  # A live crew (closed+working+fresh → reads live, no pod) on step B.
  local crew_name="${PROJECT}-orphanx-crew-${RANDOM_SUFFIX}"
  local crew_id
  crew_id=$(seed_live_crew "$crew_name") || true
  if [ -z "$crew_id" ]; then
    fail "scenario 5: seed live crew agent" "seed_live_crew returned no id"
    echo
    return 0
  fi
  pass "seeded live crew ($crew_id, name=$crew_name, closed+working+fresh → reads live, no pod)"

  # A ghost assignee for step A: an agent-shaped name with no agent bead → the
  # viewer's liveness lookup resolves it as absent → step A counts as a dead orphan.
  local ghost="gardenio-orphanx-ghost-${RANDOM_SUFFIX}"
  local ids plan stepA stepB
  ids=$(seed_orphan_plus_crew_plan "$ghost" "$crew_id") || true
  plan=$(awk '{print $1}' <<<"$ids")
  stepA=$(awk '{print $2}' <<<"$ids")
  stepB=$(awk '{print $3}' <<<"$ids")
  if [ -z "$plan" ] || [ -z "$stepA" ] || [ -z "$stepB" ]; then
    fail "scenario 5: seed orphan+crew plan" "seed returned no ids (plan=$plan A=$stepA B=$stepB)"
    echo
    return 0
  fi
  # seed_orphan_plus_crew_plan's step writes do not touch the crew bead, but
  # re-stamp it live to be safe (freshness + agent_state=working).
  set_agent_live "$crew_id" || true
  pass "seeded orphan+crew plan ($plan): stepA=$stepA (dead ghost, −3h), stepB=$stepB (open, crew assigned)"

  # ── A) The re-scope, on the INDEX card (planStallBulk + planCrewBulk). ──────
  local ibadge icard
  ibadge=$(index_card_badge "$plan" "$token")
  icard=$(index_card_html "$plan" "$token")
  if [ -z "$ibadge" ]; then
    fail "scenario 5: index card renders the stall badge" \
      "no .plan-stall-badge span on the /plans card for ${plan}"
  else
    # The re-scoped headline: exactly "⚠ 1 orphaned".
    if grep -qE '⚠ 1 orphaned' <<<"$ibadge"; then
      pass "scenario 5: index badge headline re-scoped to '⚠ 1 orphaned'"
    else
      fail "scenario 5: index badge reads '⚠ 1 orphaned'" "badge: $ibadge"
    fi
    # FaceLabel SUPPRESSED — no "· N dead step" reason child under the soft label.
    if grep -q 'plan-stall-badge-reason' <<<"$ibadge"; then
      fail "scenario 5: the re-scoped badge must NOT carry a .plan-stall-badge-reason child" \
        "badge: $ibadge"
    else
      pass "scenario 5: FaceLabel suppressed — no .plan-stall-badge-reason on the orphaned badge"
    fi
    if grep -q 'dead step' <<<"$ibadge"; then
      fail "scenario 5: the 'dead step' phrasing must be dropped under the soft label" \
        "badge: $ibadge"
    else
      pass "scenario 5: 'dead step' phrasing dropped under the re-scoped label"
    fi
    # The title-prefix that names the coexistence.
    if grep -q 'orphaned under a dead agent (other crew still working the plan)' <<<"$ibadge"; then
      pass "scenario 5: badge title-prefix names the coexistence"
    else
      fail "scenario 5: badge title-prefix present" "badge: $ibadge"
    fi
  fi
  # The live crew renders "· 1 working" on the SAME index card — the coexistence
  # the re-scope exists to make coherent.
  if grep -q 'plan-card-crew-working">· 1 working' <<<"$icard"; then
    pass "scenario 5: the coexisting crew chip reads '· 1 working' on the SAME card"
  else
    local chip
    chip=$(grep -oE 'plan-card-crew-working">[^<]*' <<<"$icard" || true)
    fail "scenario 5: crew chip '· 1 working' on the same card" \
      "card crew chip: ${chip:-<none found>}"
  fi

  # ── B) The re-scope, on the COCKPIT header (planStallForDetail + planCrew). ─
  local dfile dbadge dhtml
  dfile=$(fetch_detail "$plan" "$token")
  dbadge=$(stall_badge_span "$dfile")
  dhtml=$(cat "$dfile" 2>/dev/null || true)
  rm -f "$dfile"
  if [ -z "$dbadge" ]; then
    fail "scenario 5: cockpit renders the stall badge" \
      "no .plan-stall-badge span on /plans/${plan}"
  else
    if grep -qE '⚠ 1 orphaned' <<<"$dbadge" && ! grep -q 'plan-stall-badge-reason' <<<"$dbadge"; then
      pass "scenario 5: cockpit header carries the SAME '⚠ 1 orphaned' re-scope (both surfaces agree)"
    else
      fail "scenario 5: cockpit header reads '⚠ 1 orphaned' with no reason child" "badge: $dbadge"
    fi
  fi
  if grep -q 'plan-crew-count-working' <<<"$dhtml"; then
    pass "scenario 5: cockpit crew roster shows the coexisting working crew"
  else
    fail "scenario 5: cockpit shows the working crew" "no .plan-crew-count-working in cockpit"
  fi

  # ── C) Negative control: flip the crew NON-alive → badge flips back to loud. ─
  # Set agent_state=idle (a non-alive bucket) with a FRESH updated_at. Crew.HasLive()
  # reads agent_state (freshness-agnostic), so this — not aging — is what drops the
  # crew off "N working" and clears the coexistence condition. Nothing else changes,
  # so a badge that flips back to "⚠ stalled · 1 dead step" proves the crew's
  # LIVENESS (not some unrelated exemption) drove the re-scope.
  if ! set_agent_state "$crew_id" "idle"; then
    skip "scenario 5: non-alive control" "could not flip the crew agent_state to idle"
    echo
    return 0
  fi
  local ibadge2
  ibadge2=$(index_card_badge "$plan" "$token")
  if [ -z "$ibadge2" ]; then
    fail "scenario 5 (control): flipping the crew non-alive must keep a badge" \
      "no badge after the crew went idle — the re-scope assertion may be trivially green"
  else
    if grep -q '⚠ stalled' <<<"$ibadge2" && ! grep -q 'orphaned' <<<"$ibadge2"; then
      pass "scenario 5 (control): a non-alive crew flips the badge BACK to loud '⚠ stalled' (re-scope is crew-driven)"
    else
      fail "scenario 5 (control): non-alive crew must un-scope to '⚠ stalled' (not 'orphaned')" \
        "badge: $ibadge2"
    fi
    if grep -q 'plan-stall-badge-reason">· 1 dead step' <<<"$ibadge2"; then
      pass "scenario 5 (control): the re-raised loud badge reads '· 1 dead step'"
    else
      fail "scenario 5 (control): re-raised badge reads '· 1 dead step'" "badge: $ibadge2"
    fi
  fi
  echo
}

summary() {
  blue "═══════════════════════════════════════════════════"
  blue " plans-stall-badge: $PASS passed, $FAIL failed, $SKIP skipped"
  if [ "$FAIL" -gt 0 ]; then
    red "FAILURES:"
    for e in "${ERRORS[@]}"; do red "  - $e"; done
    exit 1
  fi
  blue "═══════════════════════════════════════════════════"
}

main "$@"
