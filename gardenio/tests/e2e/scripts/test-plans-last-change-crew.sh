#!/usr/bin/env bash
# test-plans-last-change-crew.sh — /plans "last change" stamp folds crew
# heartbeats (ka-0000003623; epic ka-0000000958). Runtime fix: MR !6757 /
# ka-0000004310 (iter-24).
#
# THE FIX UNDER TEST. Both /plans freshness surfaces fold the plan's crew
# heartbeats into the "last change" stamp:
#   • index card:  plans.go:~1098  lastActivity = laterTime(lastActivity, crews[b.ID].newestActivityAt)
#   • cockpit hdr: plans.go:~1886  lastActivity = laterTime(lastActivity, crew.newestActivityAt)
# newestActivityAt (plans_crew.go:~638 bulk / ~209 roster) is the newest
# updated_at across the plan's crew agent beads, folded UNCONDITIONALLY over
# every crew linked by an `assigned` edge (crew→step) — liveness-agnostic and
# monotonic (laterTime only moves the stamp forward).
#
# The bug it fixed (the mission's false-freshness family, temporal twin of the
# iter-19/23 false-stall): a working crew's updated_at moves every cycle while
# the STEP task bead it works sits static until a status change — so a step
# claimed a month ago whose crew is heartbeating right now rendered a pulsing
# green "● N crew · M working" live chip BESIDE "updated 1 month ago", a
# card-vs-truth self-contradiction. Folding the crew heartbeat makes the stamp
# track the live crew, so the freshness stamp can never contradict the crew
# chip. It ALSO re-freshens the within-tier attention rank (sortPlanRows reads
# lastActivityAt) so a live-crew plan sorts as recently-active.
#
# Unit tests (plans_crew_test.go, plans_test.go) cover newestActivityAt against a
# fake daemon. This module is the missing render-level gate: it seeds real bead
# state against a live kbeads daemon + beads-viewer and asserts the RENDERED
# "updated Xago" stamp — the thing an operator actually sees — on BOTH surfaces,
# in the seed-bead-state-and-assert-rendered-HTML idiom of the sibling viewer
# modules (test-plans-stall-badge.sh, test-plans-wake-button.sh). It is the
# temporal twin of test-plans-stall-badge.sh: that module asserts the STALL
# predicate, this one asserts the FRESHNESS stamp — deliberately distinct signals.
#
# Scenarios (all assert the /plans/{id} cockpit header AND the /plans index card
# — the two surfaces fold the crew via different code paths, planCrewBulk vs
# planCrew, and the fix's contract is that they AGREE):
#
#   A. Fresh crew → fresh stamp, both surfaces, in agreement. A plan whose
#      epic + step are backdated -45d (so WITHOUT the crew fold the stamp would
#      read "1 month ago") but which carries a crew whose updated_at is NOW:
#      the index card AND the cockpit header BOTH read fresh ("just now" / "N
#      minutes ago"), NOT the -45d step/epic time, and the two surfaces carry a
#      BYTE-IDENTICAL "Last change <RFC>" title (they agree on the same instant).
#      THE POINT: the crew heartbeat is folded into the freshness stamp.
#   B. NEGATIVE + load-bearing: flip the SAME crew's heartbeat stale. Backdate
#      the crew's updated_at to -60d (older than the -45d step/epic, and past
#      the liveness bar). Nothing else changes. The stamp now FALLS BACK to the
#      -45d step/epic time ("1 month ago") on BOTH surfaces — proving the crew's
#      FRESH heartbeat (not its mere existence) is what moved the stamp, i.e. the
#      scenario-A assertion is not trivially green.
#   C. Within-tier sort re-rank (ka-0000004310: "sorts as recently-active").
#      Seed a stale no-crew comparison plan (epic + one CLOSED step both -45d →
#      tier None, stamp -45d). The live-crew plan from scenario A (fresh stamp,
#      also tier None) must sort BEFORE it on the /plans index — sortPlanRows
#      drains the residual (None) tier freshest-first, and the crew fold is what
#      makes the live-crew plan read fresher. Best-effort: SKIPs (never fails)
#      when either card is absent from the rendered index.
#
# ── Why the crew fixture is closed+working+fresh (fidelity note) ──────────────
# The crew rollup's newestActivityAt fold reads a crew bead's `updated_at`
# UNCONDITIONALLY — it does not gate on status or the live/done classification
# (plans_crew.go: `roll.newestActivityAt = laterTime(roll.newestActivityAt,
# b.UpdatedAt)`). A REAL live crew heartbeats a FRESH updated_at every cycle; the
# fixture presents that SAME input — a fresh updated_at — so the fold sees
# exactly what production feeds it. The bead is created CLOSED (immediately) so
# the live controller on staging tears any spawned pod down and the
# reconciler won't re-spawn one (a closed bead is not in its desired set) — the
# SAME pod-safety pattern test-plans-stall-badge.sh:64 documents. Status=closed
# is the only divergence from a live crew, and the fold ignores it. (The crew
# CHIP's "N working" label DOES gate on status, so a closed crew reads "done" on
# the chip — this module asserts the STAMP, never the chip's working count.)
#
# ── Why this module backdates updated_at ──────────────────────────────────────
# The stamp folds the epic's own updated_at + every step's, and the live kbeads
# daemon stamps updated_at = NOW() on every write with no override (same
# constraint test-plans-stall-badge.sh:52 documents). So to make the stamp read
# stale WITHOUT the crew (the -45d floor scenarios A/B/C stand on), the ONLY way
# to age the epic + step past a window on demand is a direct SQL UPDATE against
# the in-chart Postgres. staging runs an in-cluster postgres pod
# (app.kubernetes.io/component=postgres); prod runs external RDS and a
# viewer-only cluster has no postgres. So the whole suite gates on the backdate
# capability: when the postgres pod is absent / unreachable / not exec-able
# (external RDS), it SKIPs LOUD (still exit 0 — an environment that cannot host
# the fixture must never red the suite, ka-0000000847; a skip is never mistaken
# for a pass).
#
# ── Pod-safety on a live-controller cluster ───────────────────────────────────
# staging runs a live controller that spawns a REAL pod for any OPEN
# type=agent bead. The crew this module seeds is therefore created with a
# claudeless mock_scenario (basic-echo, zero model spend) and CLOSED IMMEDIATELY
# so the controller tears its pod down; its heartbeat is then set via a direct
# SQL write (agent_state + updated_at). The cleanup trap closes every seeded
# bead (belt-and-suspenders reaps any pod) and force-deletes any pod we observe.
#
# Self-SKIPs (exit 0) when the viewer Service is absent (inert in the ephemeral
# e2e namespaces; asserts only against a long-lived viewer — staging
# singleton / prod), when the daemon is unreachable / no token, or when
# updated_at cannot be backdated (see above).
#
# Idempotency: every created bead carries the title prefix plans-lastchg-e2e-<rand>
# and is closed in the cleanup trap. Reruns do not collide.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-plans-last-change-crew.sh
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
VIEWER_PF_PORT="${VIEWER_PF_PORT:-19831}"
BEADS_PF_PORT="${BEADS_PF_PORT:-19832}"
KD_BIN="${KD_BIN:-kd}"
PROJECT="${PLANS_LASTCHG_PROJECT:-gardenio}"
OPERATOR_EMAIL="${OPERATOR_EMAIL:-e2e-plans-lastchg-op-$$@example.com}"
TITLE_PREFIX="plans-lastchg-e2e"
RANDOM_SUFFIX="$$-$(date +%s)"
# Claudeless mock scenario the (immediately-closed) crew pod runs — zero model
# spend, baked into the agent image at /scenarios/ (images/agent/Dockerfile).
PLANS_LASTCHG_MOCK_SCENARIO="${PLANS_LASTCHG_MOCK_SCENARIO:-basic-echo}"
# How far past every freshness band the epic + step are aged. 45 days lands
# squarely in humanizeTime's "month" band (server.go:181) so a fallback stamp
# reads "1 month ago"; the crew's stale heartbeat in scenario B is aged OLDER
# still (-60d) so laterTime picks the -45d step/epic floor, never the crew.
STEP_BACKDATE="${PLANS_LASTCHG_STEP_BACKDATE:-45 days}"
CREW_DEAD_BACKDATE="${PLANS_LASTCHG_CREW_DEAD_BACKDATE:-60 days}"

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
CREATED_BEADS_FILE="$(mktemp /tmp/plans-lastchg-beads-XXXXXX)"
CREATED_PODS_FILE="$(mktemp /tmp/plans-lastchg-pods-XXXXXX)"
track_bead() { [ -n "${1:-}" ] && printf '%s\n' "$1" >> "$CREATED_BEADS_FILE"; }
track_pod()  { [ -n "${1:-}" ] && printf '%s\n' "$1" >> "$CREATED_PODS_FILE"; }
# Postgres pod discovered for backdating (empty until preflight succeeds).
PG_POD=""
PG_DB="${PLANS_LASTCHG_PG_DB:-beads}"
PG_USER="${PLANS_LASTCHG_PG_USER:-postgres}"

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
    "${BEADS_PF_PORT}:8080" >/tmp/plans-lastchg-beads-pf.log 2>&1 &
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
# <interval> is a Postgres interval literal, e.g. "45 days" / "60 days".
backdate_updated_at() {
  local id="$1" interval="$2"
  kubectl -n "$NAMESPACE" exec "$PG_POD" -c postgres -- \
    psql -U "$PG_USER" -d "$PG_DB" -tAc \
    "UPDATE beads SET updated_at = NOW() - INTERVAL '${interval}' WHERE id = '${id}';" \
    >/dev/null 2>&1
}

# set_crew_heartbeat <bead-id> <interval-or-empty> — set the crew agent's
# updated_at to NOW() - <interval> (empty ⇒ NOW()) AND agent_state=working, in
# Postgres. A fresh (empty-interval) write makes the crew's heartbeat the newest
# instant the fold sees; an aged one flips it stale. The fold reads updated_at
# regardless of status/liveness, so a CLOSED crew set this way still contributes
# its heartbeat to the plan's freshness stamp.
set_crew_heartbeat() {
  local id="$1" interval="${2:-}"
  local expr="NOW()"
  [ -n "$interval" ] && expr="NOW() - INTERVAL '${interval}'"
  kubectl -n "$NAMESPACE" exec "$PG_POD" -c postgres -- \
    psql -U "$PG_USER" -d "$PG_DB" -tAc \
    "UPDATE beads SET updated_at = ${expr}, fields = jsonb_set(coalesce(fields,'{}'::jsonb),'{agent_state}','\"working\"') WHERE id = '${id}';" \
    >/dev/null 2>&1
}

# seed_crew <name> — create a claudeless crew agent bead, CLOSE it (so the
# controller tears down any spawned pod and the reconciler won't re-spawn), then
# stamp a FRESH heartbeat via SQL. Echoes the bead id; appends it (and any pod)
# to cleanup. Empty on failure.
seed_crew() {
  local name="$1"
  local out id
  out=$("$KD_BIN" create "$name" \
    --type=agent --project "$PROJECT" -l "role:crew" --no-parent \
    -f "agent=${name}" -f "role=crew" -f "project=${PROJECT}" \
    -f "agent_state=working" -f "mock_scenario=${PLANS_LASTCHG_MOCK_SCENARIO}" \
    -d "last-change e2e crew (${RANDOM_SUFFIX})" --json 2>/dev/null || printf '{}')
  id=$(bead_id_from_json "$out")
  [ -n "$id" ] || { printf ''; return 1; }
  track_bead "$id"
  # Close immediately: AgentDone tears the spawned pod down and drops the bead out
  # of the reconciler's desired set so no create-missing spawn recurs.
  "$KD_BIN" close "$id" >/dev/null 2>&1 || true
  track_pod "${NAMESPACE}/crew-${PROJECT}-crew-${name#"${PROJECT}-"}"
  set_crew_heartbeat "$id" "" || { printf ''; return 1; }
  printf '%s' "$id"
}

# seed_live_crew_plan <tag> — the scenario-A/B fixture. Create a plan epic (plan
# label) + one in_progress step, wire a crew via a reverse `assigned` edge
# (crew→step, THE signal the rollup reads — NOT the step assignee), backdate BOTH
# the epic AND the step to $STEP_BACKDATE (so the stamp would read stale WITHOUT
# the crew), and stamp the crew's heartbeat FRESH last. Echoes "<plan> <step>
# <crew>"; appends all three to CREATED_BEADS. Empty on failure.
seed_live_crew_plan() {
  local tag="$1"
  local plan step crew crew_name
  plan=$(bead_id_from_json "$("$KD_BIN" create "${TITLE_PREFIX}-${tag}-plan-${RANDOM_SUFFIX}" \
    --type=epic --project "$PROJECT" -l "plan" --no-parent \
    -d "last-change e2e ${tag} plan (${RANDOM_SUFFIX})" --json 2>/dev/null || printf '{}')")
  [ -n "$plan" ] || { printf ''; return 1; }
  track_bead "$plan"
  step=$(bead_id_from_create "$("$KD_BIN" create "${TITLE_PREFIX}-${tag}-step-${RANDOM_SUFFIX}" \
    --type=task --project "$PROJECT" --no-parent 2>&1)")
  [ -n "$step" ] || { printf ''; return 1; }
  track_bead "$step"
  "$KD_BIN" dep add "$step" "$plan" --type parent-child >/dev/null 2>&1 || true

  crew_name="${PROJECT}-lastchg-crew-${tag}-${RANDOM_SUFFIX}"
  crew=$(seed_crew "$crew_name") || true
  [ -n "$crew" ] || { printf ''; return 1; }

  # The load-bearing edge: crew --assigned--> step. The crew rollup reads the
  # step's REVERSE deps for `assigned` edges (plans_crew.go), so this is what
  # folds the crew's heartbeat into the plan's freshness — independent of the
  # step's own assignee field.
  "$KD_BIN" dep add "$crew" "$step" --type assigned >/dev/null 2>&1 || true
  # Also single-claim the step to the crew name (a real dispatched crew does),
  # for fidelity — not load-bearing for the stamp (the edge above is).
  "$KD_BIN" update "$step" --status in_progress --assignee "$crew_name" >/dev/null 2>&1 || true

  # Backdate AFTER every structural/status write (each stamps a fresh updated_at):
  # step first, then epic, so BOTH read stale. The crew heartbeat is re-stamped
  # FRESH last so it is unambiguously the newest instant the fold sees.
  backdate_updated_at "$step" "$STEP_BACKDATE" || { printf ''; return 1; }
  backdate_updated_at "$plan" "$STEP_BACKDATE" || { printf ''; return 1; }
  set_crew_heartbeat "$crew" "" || { printf ''; return 1; }
  printf '%s %s %s' "$plan" "$step" "$crew"
}

# seed_stale_plan <tag> — the scenario-C comparison plan: a plan epic + ONE
# CLOSED step, both backdated $STEP_BACKDATE, NO crew. Renders tier None (no open
# work, no crew, no gardener) with a stale -45d stamp. Echoes "<plan> <step>".
seed_stale_plan() {
  local tag="$1"
  local plan step
  plan=$(bead_id_from_json "$("$KD_BIN" create "${TITLE_PREFIX}-${tag}-plan-${RANDOM_SUFFIX}" \
    --type=epic --project "$PROJECT" -l "plan" --no-parent \
    -d "last-change e2e ${tag} stale plan (${RANDOM_SUFFIX})" --json 2>/dev/null || printf '{}')")
  [ -n "$plan" ] || { printf ''; return 1; }
  track_bead "$plan"
  step=$(bead_id_from_create "$("$KD_BIN" create "${TITLE_PREFIX}-${tag}-step-${RANDOM_SUFFIX}" \
    --type=task --project "$PROJECT" --no-parent 2>&1)")
  [ -n "$step" ] || { printf ''; return 1; }
  track_bead "$step"
  "$KD_BIN" dep add "$step" "$plan" --type parent-child >/dev/null 2>&1 || true
  "$KD_BIN" close "$step" >/dev/null 2>&1 || true
  backdate_updated_at "$step" "$STEP_BACKDATE" || { printf ''; return 1; }
  backdate_updated_at "$plan" "$STEP_BACKDATE" || { printf ''; return 1; }
  printf '%s %s' "$plan" "$step"
}

# ── Stamp extractors ──────────────────────────────────────────────
# Each echoes "<visible-text>|||<rfc-title>" for the "updated Xago" freshness
# span, or "" when the span is absent. The visible text is what the operator
# reads ("just now" / "1 month ago"); the RFC title is the exact instant both
# surfaces must AGREE on.

# detail_stamp <plan-id> <token> — the /plans/{id} cockpit header
# `.plan-header-updated` span (plan_detail.html).
detail_stamp() {
  local plan="$1" token="$2" f
  f=$(mktemp /tmp/plans-lastchg-detail-XXXXXX)
  curl -sk --max-time 12 \
    -H "Authorization: Bearer ${token}" -H "Accept: text/html" \
    -o "$f" \
    "http://localhost:${VIEWER_PF_PORT}/plans/${plan}" 2>/dev/null || true
  python3 - "$f" <<'PY'
import re, sys
try:
    html = open(sys.argv[1], encoding='utf-8', errors='replace').read()
except OSError:
    print(""); sys.exit(0)
m = re.search(r'<span class="meta plan-header-updated"([^>]*)>(.*?)</span>', html, re.DOTALL)
if not m:
    print(""); sys.exit(0)
attrs, text = m.group(1), re.sub(r'\s+', ' ', m.group(2)).strip()
rfc = ""
t = re.search(r'title="Last change ([^"]*)"', attrs)
if t:
    rfc = t.group(1)
print(f"{text}|||{rfc}")
PY
  rm -f "$f"
}

# index_stamp <plan-id> <token> — the `.plan-card-updated` span on the /plans
# INDEX card for <plan-id> (planCrewBulk path). Scoped to the card region so a
# stamp on a DIFFERENT plan can't leak in.
index_stamp() {
  local plan="$1" token="$2" f
  f=$(mktemp /tmp/plans-lastchg-index-XXXXXX)
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
if not m:
    print(""); sys.exit(0)
card = m.group(0)
s = re.search(r'<span class="meta plan-card-updated"([^>]*)>(.*?)</span>', card, re.DOTALL)
if not s:
    print(""); sys.exit(0)
attrs, text = s.group(1), re.sub(r'\s+', ' ', s.group(2)).strip()
rfc = ""
t = re.search(r'title="Last change ([^"]*)"', attrs)
if t:
    rfc = t.group(1)
print(f"{text}|||{rfc}")
PY
  rm -f "$f"
}

# index_plan_order <plan-a> <plan-b> <token> — echo the 0-based render positions
# of the two plan cards on the /plans index as "<pos-a> <pos-b>" (each "-1" when
# absent), so a caller can assert the sort order. Reads the SAME index page.
index_plan_order() {
  local a="$1" b="$2" token="$3" f
  f=$(mktemp /tmp/plans-lastchg-order-XXXXXX)
  curl -sk --max-time 15 \
    -H "Authorization: Bearer ${token}" -H "Accept: text/html" \
    -o "$f" \
    "http://localhost:${VIEWER_PF_PORT}/plans?project=${PROJECT}&operated_by=" 2>/dev/null || true
  python3 - "$f" "$a" "$b" <<'PY'
import re, sys
try:
    html = open(sys.argv[1], encoding='utf-8', errors='replace').read()
except OSError:
    print("-1 -1"); sys.exit(0)
a, b = sys.argv[2], sys.argv[3]
ids = re.findall(r'<li class="plan-card"[^>]*data-plan-id="((gd|ka)-[A-Za-z0-9]+)"', html)
pa = ids.index(a) if a in ids else -1
pb = ids.index(b) if b in ids else -1
print(f"{pa} {pb}")
PY
  rm -f "$f"
}

# index_card_stalled <plan-id> <token> — echo "1" when the /plans index card for
# <plan-id> carries a .plan-stall-badge span (tier Stalled), "0" when it does not,
# "" when the card is absent. Scenario C uses this to PIN the tier: sortPlanRows'
# primary key is the attention tier (Stalled sorts before None), so "live sorts
# first" only proves the freshness fold when BOTH plans share the None tier. A
# stalled live card would sort first for the WRONG reason (tier jump), so C
# asserts the live card is NOT stalled before trusting the order.
index_card_stalled() {
  local plan="$1" token="$2" f
  f=$(mktemp /tmp/plans-lastchg-tier-XXXXXX)
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
if not m:
    print(""); sys.exit(0)
print("1" if re.search(r'<span class="plan-stall-badge', m.group(0)) else "0")
PY
  rm -f "$f"
}

# is_fresh <visible-text> — true when the stamp reads within the minute/just-now
# band (the crew heartbeat won). Anchored so "1 month" etc. never match.
is_fresh() { grep -Eq 'updated (just now|[0-9]+ minutes? ago)' <<<"$1"; }
# is_stale <visible-text> — true when the stamp reads in a hour/day/week/month/
# year band (the -45d step/epic floor, crew NOT folded / stale).
is_stale() { grep -Eq 'updated [0-9]+ (hours?|days?|weeks?|months?|years?) ago' <<<"$1"; }

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " /plans 'last change' folds crew heartbeats (ka-0000003623)"
  blue "═══════════════════════════════════════════════════"

  if ! setup_viewer_portforward; then
    skip "plans-last-change-crew module" "viewer service $VIEWER_SVC absent"
    summary
    return 0
  fi
  setup_beads_portforward

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    skip "plans-last-change-crew module" \
      "kd cannot reach beads daemon at ${BEADS_HTTP_URL:-unset}"
    summary
    return 0
  fi

  # The stamp needs an epic + step aged past the freshness bands, which the daemon
  # won't let us set — SO the whole suite gates on an exec-able in-chart Postgres.
  if ! command -v kubectl >/dev/null 2>&1; then
    skip "plans-last-change-crew module" "kubectl not installed (need SQL backdate of updated_at)"
    summary
    return 0
  fi
  if ! discover_postgres_pod; then
    skip "plans-last-change-crew module" \
      "no in-chart postgres pod in $NAMESPACE (external RDS?) — cannot backdate updated_at"
    summary
    return 0
  fi
  if ! kubectl -n "$NAMESPACE" exec "$PG_POD" -c postgres -- \
        psql -U "$PG_USER" -d "$PG_DB" -tAc "SELECT 1;" >/dev/null 2>&1; then
    skip "plans-last-change-crew module" \
      "cannot exec psql in ${NAMESPACE}/${PG_POD} — cannot backdate updated_at"
    summary
    return 0
  fi
  dim "  backdate substrate present — postgres pod ${NAMESPACE}/${PG_POD}"

  # ── Mint an operator token for the OIDC-gated viewer GET. ─────────
  local op_token=""
  if ! op_token=$(mint_user_token "$OPERATOR_EMAIL" 2>/tmp/plans-lastchg-mint.err); then
    yellow "mint_user_token failed for ${OPERATOR_EMAIL} — see /tmp/plans-lastchg-mint.err"
    skip "scenario A: fresh crew → fresh stamp, both surfaces agree" "no usable GitLab-direct bearer"
    skip "scenario B: dead-crew control → stamp falls back to step time" "no usable GitLab-direct bearer"
    skip "scenario C: live-crew plan sorts recently-active" "no usable GitLab-direct bearer"
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
    skip "plans-last-change-crew module" "viewer rejects the E2E transport bearer — viewer still OIDC-gated at the pod; viewer-login flip pending (ka-0000002083)"
    summary
    return 0
  fi

  # Seed the scenario-A/B fixture ONCE; scenario A asserts fresh, C reuses its
  # fresh state for the sort, then B mutates its crew stale. Order A → C → B keeps
  # the crew fresh through the sort assertion.
  local ids plan step crew
  ids=$(seed_live_crew_plan "live") || true
  plan=$(awk '{print $1}' <<<"$ids")
  step=$(awk '{print $2}' <<<"$ids")
  crew=$(awk '{print $3}' <<<"$ids")
  if [ -z "$plan" ] || [ -z "$step" ] || [ -z "$crew" ]; then
    fail "seed live-crew plan" "seed returned no ids (plan=$plan step=$step crew=$crew)"
    summary
    return 0
  fi
  pass "seeded live-crew plan ($plan) + in_progress step ($step, updated_at −${STEP_BACKDATE}) + crew ($crew, assigned edge, heartbeat NOW)"
  echo

  scenario_fresh_stamp "$op_token" "$plan"
  scenario_sort_rerank "$op_token" "$plan"
  scenario_dead_crew_fallback "$op_token" "$plan" "$crew"

  summary
}

# ══════════════════════════════════════════════════════════════════
# Scenario A: a plan whose epic + step are backdated −45d but carries a crew
# heartbeating NOW reads FRESH on BOTH surfaces (crew fold), and the two surfaces
# agree on the same instant (byte-identical "Last change <RFC>" title).
# ══════════════════════════════════════════════════════════════════
scenario_fresh_stamp() {
  local token="$1" plan="$2"
  blue "Scenario A: fresh crew heartbeat → fresh stamp on both surfaces, in agreement"

  local draw dtext drfc iraw itext irfc
  draw=$(detail_stamp "$plan" "$token")
  dtext="${draw%%|||*}"; drfc="${draw##*|||}"
  iraw=$(index_stamp "$plan" "$token")
  itext="${iraw%%|||*}"; irfc="${iraw##*|||}"

  if [ -z "$draw" ]; then
    fail "scenario A: cockpit header renders a 'last change' stamp" \
      "no .plan-header-updated span on /plans/${plan}"
  elif is_fresh "$dtext"; then
    pass "scenario A: cockpit header reads FRESH ('${dtext}') — crew heartbeat folded, not the −${STEP_BACKDATE} step/epic"
  else
    fail "scenario A: cockpit header reads fresh" \
      "stamp not fresh (crew fold missing?): '${dtext}'"
  fi

  if [ -z "$iraw" ]; then
    fail "scenario A: index card renders a 'last change' stamp" \
      "no .plan-card-updated span on the /plans card for ${plan}"
  elif is_fresh "$itext"; then
    pass "scenario A: index card reads FRESH ('${itext}') — planCrewBulk fold, not the −${STEP_BACKDATE} step/epic"
  else
    fail "scenario A: index card reads fresh" \
      "stamp not fresh (planCrewBulk fold missing?): '${itext}'"
  fi

  # Agreement: the two surfaces fold the SAME crew bead via different code paths
  # (planCrew vs planCrewBulk), so their RFC instant must be byte-identical.
  if [ -n "$drfc" ] && [ -n "$irfc" ]; then
    if [ "$drfc" = "$irfc" ]; then
      pass "scenario A: both surfaces AGREE on the same instant (Last change ${drfc})"
    else
      fail "scenario A: cockpit + index agree on the last-change instant" \
        "detail RFC='${drfc}' vs index RFC='${irfc}'"
    fi
  else
    fail "scenario A: both surfaces carry a 'Last change <RFC>' title" \
      "detail RFC='${drfc}' index RFC='${irfc}'"
  fi
  echo
}

# ══════════════════════════════════════════════════════════════════
# Scenario C (best-effort): the live-crew plan (fresh stamp, tier None) sorts
# BEFORE a stale no-crew comparison plan (tier None) on the /plans index —
# sortPlanRows drains the residual tier freshest-first, and the crew fold is what
# makes the live-crew plan read fresher. SKIPs (never fails) if either card is
# absent (the index may paginate / the comparison plan may not render).
# ══════════════════════════════════════════════════════════════════
scenario_sort_rerank() {
  local token="$1" live_plan="$2"
  blue "Scenario C: live-crew plan sorts as recently-active (before a stale no-crew plan)"

  local ids stale_plan stale_step
  ids=$(seed_stale_plan "stale") || true
  stale_plan=$(awk '{print $1}' <<<"$ids")
  stale_step=$(awk '{print $2}' <<<"$ids")
  if [ -z "$stale_plan" ] || [ -z "$stale_step" ]; then
    skip "scenario C: sort re-rank" "could not seed the stale comparison plan"
    echo
    return 0
  fi
  dim "  seeded stale no-crew comparison plan ($stale_plan, updated_at −${STEP_BACKDATE})"

  # Sanity: the comparison plan must actually read stale (its fixture floor), else
  # the sort contrast is meaningless.
  local sraw stext
  sraw=$(index_stamp "$stale_plan" "$token")
  stext="${sraw%%|||*}"
  if [ -n "$sraw" ] && ! is_stale "$stext"; then
    skip "scenario C: sort re-rank" "comparison plan did not read stale ('${stext}') — cannot pin the contrast"
    echo
    return 0
  fi

  # Pin the TIER: sortPlanRows' primary key is the attention tier (Stalled sorts
  # before None regardless of freshness). "Live sorts first" only proves the
  # freshness fold when BOTH plans share the None tier — so if EITHER card is
  # stalled, the order would be a tier jump, not the fold. SKIP rather than assert
  # a possibly-tier-driven order (best-effort scenario).
  local live_stalled stale_stalled
  live_stalled=$(index_card_stalled "$live_plan" "$token")
  stale_stalled=$(index_card_stalled "$stale_plan" "$token")
  if [ "$live_stalled" = "1" ] || [ "$stale_stalled" = "1" ]; then
    skip "scenario C: sort re-rank" \
      "a card is in the Stalled tier (live=$live_stalled stale=$stale_stalled) — order would be a tier jump, not the freshness fold"
    echo
    return 0
  fi
  dim "  both cards are in the None tier (no stall badge) — order reflects within-tier freshness"

  local order pos_live pos_stale
  order=$(index_plan_order "$live_plan" "$stale_plan" "$token")
  pos_live=$(awk '{print $1}' <<<"$order")
  pos_stale=$(awk '{print $2}' <<<"$order")
  if [ "$pos_live" = "-1" ] || [ "$pos_stale" = "-1" ]; then
    skip "scenario C: sort re-rank" \
      "both cards not on the rendered index (live pos=$pos_live, stale pos=$pos_stale)"
    echo
    return 0
  fi
  if [ "$pos_live" -lt "$pos_stale" ]; then
    pass "scenario C: live-crew plan (pos $pos_live) sorts before the stale plan (pos $pos_stale) — crew fold re-freshens the rank"
  else
    fail "scenario C: live-crew plan sorts before the stale plan" \
      "live pos=$pos_live is NOT before stale pos=$pos_stale"
  fi
  echo
}

# ══════════════════════════════════════════════════════════════════
# Scenario B: NEGATIVE + load-bearing. Flip the SAME crew's heartbeat stale
# (updated_at −60d, older than the −45d step/epic). Nothing else changes. The
# stamp FALLS BACK to the −45d step/epic time on both surfaces — proving the
# crew's FRESH heartbeat, not its mere existence, was moving the stamp.
# ══════════════════════════════════════════════════════════════════
scenario_dead_crew_fallback() {
  local token="$1" plan="$2" crew="$3"
  blue "Scenario B (negative control): stale crew heartbeat → stamp falls back to the step/epic time"

  if ! backdate_updated_at "$crew" "$CREW_DEAD_BACKDATE"; then
    skip "scenario B: dead-crew control" "could not backdate the crew heartbeat to flip it stale"
    echo
    return 0
  fi
  dim "  backdated crew ($crew) heartbeat to −${CREW_DEAD_BACKDATE} (older than the −${STEP_BACKDATE} step/epic)"

  local draw dtext iraw itext
  draw=$(detail_stamp "$plan" "$token")
  dtext="${draw%%|||*}"
  iraw=$(index_stamp "$plan" "$token")
  itext="${iraw%%|||*}"

  if [ -z "$draw" ]; then
    fail "scenario B: cockpit header still renders a 'last change' stamp" \
      "no .plan-header-updated span on /plans/${plan}"
  elif is_stale "$dtext" && ! is_fresh "$dtext"; then
    pass "scenario B: cockpit header falls back to the step/epic time ('${dtext}') — the fresh heartbeat was load-bearing"
  else
    fail "scenario B: cockpit header falls back to the step/epic time" \
      "stamp did not go stale after the crew's heartbeat aged: '${dtext}' — scenario A may be trivially green"
  fi

  if [ -z "$iraw" ]; then
    fail "scenario B: index card still renders a 'last change' stamp" \
      "no .plan-card-updated span on the /plans card for ${plan}"
  elif is_stale "$itext" && ! is_fresh "$itext"; then
    pass "scenario B: index card falls back to the step/epic time ('${itext}')"
  else
    fail "scenario B: index card falls back to the step/epic time" \
      "stamp did not go stale: '${itext}'"
  fi
  echo
}

summary() {
  blue "═══════════════════════════════════════════════════"
  blue " plans-last-change-crew: $PASS passed, $FAIL failed, $SKIP skipped"
  if [ "$FAIL" -gt 0 ]; then
    red "FAILURES:"
    for e in "${ERRORS[@]}"; do red "  - $e"; done
    exit 1
  fi
  blue "═══════════════════════════════════════════════════"
}

main "$@"
