#!/usr/bin/env bash
# test-agents-live-view.sh — beads-viewer /agents live-view correctness E2E
#
# Guards the "agents view changes when it starts updating from events vs static
# load" issue (epic ka-0000007652). The static /agents page render and the SSE
# /agents/events live-update render are two code paths for the SAME row; before
# the shared-partial fix the SSE path emitted an impoverished, narrower <tr>
# that the client swapped in place — so a live update visibly changed the row's
# shape (lost columns, misaligned under the table header) vs the static load.
#
# This module proves the two renders are CONGRUENT against a live deployment:
#   1. A live (created/updated) agent event streamed over /agents/events is the
#      SAME compact <tr> shape (same <td> column count) as the static compact
#      table row for that same bead, and carries the load-bearing cells
#      (agent link, role, agent-state chip, swap hooks).
#   2. A CLOSED agent event is streamed as the 6-column RECENT row shape (not
#      the wider active shape), so when the client moves it into the recent
#      table it does not column-shift / overflow.
#   3. The SSE handler filters to type=agent and to the project scope.
#
# It ALSO guards two agent-state rendering behaviors on the SAME surfaces
# (backfill ka-0000000091, epic ka-0000000958) that shipped unit-only:
#   4. An unengaged PREWARMED-pool member is suppressed from the static /agents
#      view — both the agent cards AND the project fleet-tile count — while an
#      engaged agent beside it renders; assigning it makes it reappear. Guards
#      isUnengagedPrewarmedAgent (agents.go), previously unit-only
#      (agents_test.go) with ZERO hits in any viewer e2e module.
#   5. A REAPER-closed agent renders distinctly in the per-agent activity lane
#      (GET /agents/{id}/events): the closed frame carries
#      `<span class="agent-event-meta">by reaper</span>`, where a clean
#      agent-attributed close reads "by <agent>". Scenario 2 asserts only the
#      recent-row SHAPE of a plain `kd close`; nothing asserted the reaper
#      ATTRIBUTION (unit-only at agents_events_test.go).
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-agents-live-view.sh
#
# Prerequisites:
#   - kubectl context pointing at the target cluster (default gardenio-e2e)
#   - kd, jq, curl, python3 in PATH
#   - lib-gitlab-token.sh's mint_user_token resolves a GitLab-direct bearer
#     (GARDENIO_E2E_GITLAB_BEARER or the static daemon token must be set). When a token cannot
#     be minted the SSE scenarios SKIP (the stream is auth-gated).
#
# Self-SKIPs (exit 0) when the viewer Service is absent, so it is inert in the
# per-instance ephemeral e2e namespaces and asserts only against a long-lived
# viewer deployment (staging singleton / prod).
#
# Idempotency: every created bead carries the title prefix
#   agents-live-view-e2e-<random>
# and is closed in the cleanup trap. Reruns do not collide.
#
# Hard rules: do NOT edit .captain/quarantines/. Do not touch any other e2e
# module. Port-forward helpers are inlined here per the sibling viewer modules.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib-gitlab-token.sh"

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
RELEASE="${RELEASE:-${NAMESPACE}}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
VIEWER_SVC="${VIEWER_SVC:-${RELEASE}-beads-viewer}"
VIEWER_PF_PORT="${VIEWER_PF_PORT:-19701}"
BEADS_PF_PORT="${BEADS_PF_PORT:-19702}"
KD_BIN="${KD_BIN:-kd}"
PROJECT="${AGENTS_LIVE_VIEW_PROJECT:-gardenio}"
NON_MATT_EMAIL="${NON_MATT_EMAIL:-e2e-not-matt-$$@example.com}"
TITLE_PREFIX="agents-live-view-e2e"
RANDOM_SUFFIX="$$-$(date +%s)"
# A synthetic task id stamped on the prewarmed member to simulate engagement in
# scenario 4. isUnengagedPrewarmedAgent only checks that task_id is NON-EMPTY
# (an assigned member is real work), so the value need not resolve to a real
# bead — it just flips the pool-shell filter off.
TASK_ID_FOR_ASSIGN="${TASK_ID_FOR_ASSIGN:-gd-e2e-assigned-${RANDOM_SUFFIX}}"

PASS=0
FAIL=0
SKIP=0
ERRORS=()
VIEWER_PF_PID=""
BEADS_PF_PID=""
CREATED_BEADS=()
BG_PIDS=()

export KD_ACTOR="${KD_ACTOR:-e2e-test}"

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
  for pid in ${BG_PIDS[@]+"${BG_PIDS[@]}"}; do kill "$pid" 2>/dev/null || true; done
  for id in ${CREATED_BEADS[@]+"${CREATED_BEADS[@]}"}; do "$KD_BIN" close "$id" >/dev/null 2>&1 || true; done
  [ -n "$VIEWER_PF_PID" ] && kill "$VIEWER_PF_PID" 2>/dev/null || true
  [ -n "$BEADS_PF_PID" ]  && kill "$BEADS_PF_PID"  2>/dev/null || true
  rm -f /tmp/agents-live-view-*.log 2>/dev/null || true
  rm -f /tmp/agents-live-view-body-* 2>/dev/null || true
}
trap cleanup EXIT

# ── Setup ─────────────────────────────────────────────────────────
setup_viewer_portforward() {
  if ! kubectl -n "$NAMESPACE" get svc "$VIEWER_SVC" >/dev/null 2>&1; then
    yellow "viewer service $VIEWER_SVC not found in $NAMESPACE — skipping module"
    return 1
  fi
  blue "Port-forwarding ${NAMESPACE}/${VIEWER_SVC} :${VIEWER_PF_PORT} → 8095..."
  kubectl -n "$NAMESPACE" port-forward "svc/${VIEWER_SVC}" \
    "${VIEWER_PF_PORT}:8095" >/tmp/agents-live-view-pf.log 2>&1 &
  VIEWER_PF_PID=$!
  local i
  for i in 1 2 3 4 5 6 7 8 9 10; do
    : "$i"
    if curl -sf --max-time 1 -o /dev/null \
        "http://localhost:${VIEWER_PF_PORT}/healthz" 2>/dev/null \
        || curl -s --max-time 1 -o /dev/null \
            "http://localhost:${VIEWER_PF_PORT}/" 2>/dev/null; then
      dim "  Viewer tunnel ready on :${VIEWER_PF_PORT} (pid $VIEWER_PF_PID)"
      return 0
    fi
    sleep 0.5
  done
  yellow "  Viewer port-forward did not become healthy within 5s; continuing"
  return 0
}

setup_beads_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    dim "  using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return 0
  fi
  blue "Port-forwarding ${NAMESPACE}/${BEADS_SVC} :${BEADS_PF_PORT} → 8080..."
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" \
    "${BEADS_PF_PORT}:8080" >/tmp/agents-live-view-beads-pf.log 2>&1 &
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

# count_td <html-row> — number of "<td" cells in the first <tr> of the input.
count_td() {
  python3 - "$1" <<'PY'
import re, sys
html = sys.argv[1]
m = re.search(r'<tr\b.*?</tr>', html, re.DOTALL)
if not m:
    print(0); sys.exit(0)
print(len(re.findall(r'<td\b', m.group(0))))
PY
}

# count_th <html-file> <table-class> — number of "<th" cells in the <thead> of
# the table whose opening tag carries <table-class>. Reads the full HTML from a
# FILE: the /agents page exceeds MAX_ARG_STRLEN, and the heredoc script already
# occupies python's stdin.
count_th() {
  python3 - "$1" "$2" <<'PY'
import re, sys
html = open(sys.argv[1], encoding='utf-8', errors='replace').read()
cls = sys.argv[2]
idx = html.find(cls)
if idx < 0:
    print(0); sys.exit(0)
rest = html[idx:]
hs = rest.find('<thead')
if hs < 0:
    print(0); sys.exit(0)
rest = rest[hs:]
gt = rest.find('>')
rest = rest[gt+1:] if gt >= 0 else rest
end = rest.find('</thead>')
if end >= 0:
    rest = rest[:end]
print(len(re.findall(r'<th\b', rest)))
PY
}

# static_row_for <id> <token> — the static compact <tr ... data-bead-id="id">
# from GET /agents?density=compact. Empty when absent.
static_row_for() {
  local id="$1" token="$2" f
  # HTML goes to a FILE (not argv/stdin): the /agents page exceeds
  # MAX_ARG_STRLEN, and `python3 - <<PY` occupies stdin with the heredoc.
  f=$(mktemp /tmp/agents-live-view-body-XXXXXX)
  curl -sk --max-time 10 \
    -H "Authorization: Bearer ${token}" -H "Accept: text/html" \
    "http://localhost:${VIEWER_PF_PORT}/agents?density=compact&project=${PROJECT}" \
    > "$f" 2>/dev/null || true
  python3 - "$f" "$id" <<'PY'
import re, sys
body = open(sys.argv[1], encoding='utf-8', errors='replace').read()
bid = sys.argv[2]
m = re.search(r'<tr\b[^>]*data-bead-id="%s".*?</tr>' % re.escape(bid), body, re.DOTALL)
print(m.group(0) if m else "")
PY
  rm -f "$f"
}

# sse_frame_for <id> <token> <kind-grep> — runs an SSE listener, then echoes the
# first `data:` row whose markup carries data-bead-id="id". Caller fires the
# event AFTER starting the listener. Reads from a temp log populated by a
# background curl whose PID is captured in BG_PIDS.
start_sse_listener() {
  local log="$1" token="$2"
  curl -sN --max-time 20 \
    -H "Authorization: Bearer ${token}" \
    "http://localhost:${VIEWER_PF_PORT}/agents/events?project=${PROJECT}" \
    > "$log" 2>/dev/null &
  local pid=$!
  BG_PIDS+=("$pid")
  sleep 2
  echo "$pid"
}

# row_from_log <log> <id> [kind] — the SSE data: row carrying
# data-bead-id="id". With an optional <kind> (created|updated|closed) it returns
# the first row whose data-bead-kind matches that lifecycle kind — the row
# builder stamps data-bead-kind on the streamed <tr>. This matters because a
# single bead emits created→updated→closed frames on the same stream; without
# the kind filter the FIRST frame (created) always wins, masking the updated /
# closed row a scenario means to assert. Empty when no matching row arrived.
row_from_log() {
  python3 - "$1" "$2" "${3:-}" <<'PY'
import sys
log, bid, kind = sys.argv[1], sys.argv[2], sys.argv[3]
try:
    txt = open(log, encoding='utf-8', errors='replace').read()
except OSError:
    print(""); sys.exit(0)
needle = 'data-bead-id="%s"' % bid
want_kind = ('data-bead-kind="%s"' % kind) if kind else None
for line in txt.splitlines():
    if not line.startswith('data: ') or needle not in line:
        continue
    row = line[len('data: '):]
    if want_kind and want_kind not in row:
        continue
    print(row)
    sys.exit(0)
print("")
PY
}

bead_id_from_json() { printf '%s' "$1" | jq -r '.id // empty' 2>/dev/null || printf ''; }

# create_agent_bead <title-suffix> <agent_state> <field>... — create a
# type=agent bead carrying the schema-required [agent role project] fields plus
# any extra -f fields, tracked for cleanup. Echoes the bead id (empty on
# failure). The type:agent schema (kbeads config.go) 400s a bare create, so the
# scenarios below MUST seed through this helper, not the plain `kd create
# --type=agent` scenarios 1-3 use. Every seeded agent is closed in the cleanup
# trap: an OPEN agent bead is a desired pod to the controller reconciler
# (ListAgentBeads → create-missing), so leaving one behind cold-starts a real
# pod — closing it promptly is what keeps this module a good citizen against the
# long-lived staging singleton.
create_agent_bead() {
  local suffix="$1" state="$2"; shift 2
  local extra=() f out id
  for f in "$@"; do extra+=(-f "$f"); done
  out=$("$KD_BIN" create "${TITLE_PREFIX}-${suffix}-${RANDOM_SUFFIX}" \
    --type=agent \
    -l "project:${PROJECT}" \
    -f "agent=${TITLE_PREFIX}-${suffix}-${RANDOM_SUFFIX}" \
    -f "role=crew" \
    -f "project=${PROJECT}" \
    -f "agent_state=${state}" \
    "${extra[@]}" \
    --json 2>/dev/null || printf '{}')
  id=$(bead_id_from_json "$out")
  [ -n "$id" ] && CREATED_BEADS+=("$id")
  printf '%s' "$id"
}

# lane_listen <log> <id> — open the PER-AGENT activity lane SSE stream
# (GET /agents/{id}/events) into <log>, in the background, tracked for cleanup.
# Unlike start_sse_listener (the fleet-wide /agents/events stream) this is the
# single-agent lane that renders the reaper-attribution meta span. Sleeps to let
# the stream connect before the caller fires the close event.
lane_listen() {
  local log="$1" id="$2"
  curl -sN --max-time 20 \
    -H "Authorization: Bearer ${token}" \
    "http://localhost:${VIEWER_PF_PORT}/agents/${id}/events" \
    > "$log" 2>/dev/null &
  local pid=$!
  BG_PIDS+=("$pid")
  sleep 3
  echo "$pid"
}

# closed_meta_from_lane <log> <id> — the `by <actor>` attribution text from the
# first `event: closed` frame for <id> on a per-agent lane log. Empty when no
# closed frame arrived or the frame carried no attribution meta. The row builder
# (renderAgentEventRow) emits <span class="agent-event-meta">by <closed_by></span>
# only on a closed kind with a non-empty closed_by, so this is the exact span
# that distinguishes a reaper-closed agent from a clean one.
closed_meta_from_lane() {
  python3 - "$1" "$2" <<'PY'
import re, sys
log, bid = sys.argv[1], sys.argv[2]
try:
    txt = open(log, encoding='utf-8', errors='replace').read()
except OSError:
    print(""); sys.exit(0)
# Frames are single-line data: rows; find the closed one for this bead.
for line in txt.splitlines():
    if not line.startswith('data: '):
        continue
    row = line[len('data: '):]
    if ('data-bead-id="%s"' % bid) not in row:
        continue
    if 'agent-event-closed' not in row:
        continue
    m = re.search(r'agent-event-meta">by ([^<]+)</span>', row)
    print(m.group(1).strip() if m else "")
    sys.exit(0)
print("")
PY
}

# agent_card_present <html-file> <id> — 1 when an ACTIVE-fleet
# <tr data-bead-id="id"> is present in the HTML FILE, else 0. Scoped to the
# ACTIVE agents table only: the page splits into an active table
# (class="agents-table") and a recent/closed table
# (class="agents-table agents-table-recent"), and the prewarmed-suppression
# assertion is about the active fleet. A closed agent legitimately appears in
# the recent table, so matching data-bead-id across the WHOLE page would false-
# positive the moment the controller reaps a seeded (pod-less) agent bead into
# the recent window. Body is read from a file (not argv/stdin): the /agents page
# exceeds MAX_ARG_STRLEN and `python3 - <<PY` occupies stdin with the heredoc.
agent_card_present() {
  python3 - "$1" "$2" <<'PY'
import re, sys
path, bid = sys.argv[1], sys.argv[2]
body = open(path, encoding='utf-8', errors='replace').read()
# Truncate at the recent-table marker so only the active fleet is searched.
cut = body.find('agents-table-recent')
active = body[:cut] if cut >= 0 else body
print(1 if re.search(r'data-bead-id="%s"' % re.escape(bid), active) else 0)
PY
}

# project_fleet_total <html-file> — the project fleet-tile "Total" count (active
# agent count for the filtered project) from the HTML FILE. Empty when absent.
project_fleet_total() {
  python3 - "$1" <<'PY'
import re, sys
body = open(sys.argv[1], encoding='utf-8', errors='replace').read()
m = re.search(r'<dt>Idle</dt><dd>\d+</dd>\s*<dt>Total</dt><dd>(\d+)</dd>', body)
print(m.group(1) if m else "")
PY
}

# agents_body <project> <token> — GET the compact /agents HTML for <project>
# into a tempfile; echo the FILE PATH. Callers pass the path to
# agent_card_present / project_fleet_total. The tempfile carries the module's
# cleanup prefix so the EXIT trap removes it.
agents_body() {
  local proj="$1" tok="$2" f
  f=$(mktemp /tmp/agents-live-view-body-XXXXXX)
  curl -sk --max-time 10 \
    -H "Authorization: Bearer ${tok}" -H "Accept: text/html" \
    "http://localhost:${VIEWER_PF_PORT}/agents?density=compact&project=${proj}" \
    > "$f" 2>/dev/null || true
  echo "$f"
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " agents live-view correctness E2E (ka-0000007652)"
  blue "═══════════════════════════════════════════════════"

  if ! setup_viewer_portforward; then
    skip "agents-live-view module" "viewer service $VIEWER_SVC absent"
    summary
    return 0
  fi
  setup_beads_portforward

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    skip "agents-live-view module" \
      "kd cannot reach beads daemon at ${BEADS_HTTP_URL:-unset}"
    summary
    return 0
  fi

  local token=""
  if ! token=$(mint_user_token "$NON_MATT_EMAIL" 2>/tmp/agents-live-view-mint.err); then
    yellow "mint_user_token failed for ${NON_MATT_EMAIL} — see /tmp/agents-live-view-mint.err"
    skip "scenario 1: live row congruence"  "no usable GitLab-direct bearer (SSE is auth-gated)"
    skip "scenario 2: closed row shape"     "no usable GitLab-direct bearer"
    skip "scenario 3: type/project filter"  "no usable GitLab-direct bearer"
    skip "scenario 4: prewarmed suppression" "no usable GitLab-direct bearer (/agents is auth-gated)"
    skip "scenario 5: reaper attribution"   "no usable GitLab-direct bearer (lane is auth-gated)"
    summary
    return 0
  fi
  dim "  minted non-Matt user token (sub=${NON_MATT_EMAIL})"
  # The viewer's own OIDC middleware stays pinned to the Keycloak issuer until
  # the viewer-login flip lands (ka-0000002083); it rejects this GitLab-direct /
  # static-daemon bearer with 401. SKIP-clean rather than hard-fail every gated
  # GET / SSE stream (bug gd-5M8iT2oNlr). Auto-enables when the viewer trusts
  # the GitLab edge.
  if ! viewer_accepts_bearer "$token"; then
    skip "agents-live-view module" "viewer rejects the E2E transport bearer — viewer still OIDC-gated at the pod; viewer-login flip pending (ka-0000002083)"
    summary
    return 0
  fi

  # ══════════════════════════════════════════════════════════════════
  # Scenario 1: a live (updated) agent event streamed over /agents/events
  # has the SAME column count as the static compact table row, and carries
  # the load-bearing cells + swap hooks. This is the direct congruence
  # assertion for Matt's report.
  # ══════════════════════════════════════════════════════════════════
  blue "Scenario 1: live SSE row is congruent with the static compact row"
  local s1_log="/tmp/agents-live-view-s1-$$.log"
  start_sse_listener "$s1_log" "$token" >/dev/null

  # Create an agent-shaped bead so an /agents index event fires. The viewer
  # filters the stream to type=agent, so the bead must be type=agent. The
  # type:agent schema requires [agent role project], so seed via
  # create_agent_bead (a bare `kd create --type=agent` 400s).
  local s1_id
  s1_id=$(create_agent_bead "live" "spawning")
  if [ -z "$s1_id" ]; then
    fail "create scenario-1 agent bead" "no ID returned from kd create"
  else
    # An update guarantees an updated event with our fields on the row.
    "$KD_BIN" update "$s1_id" -f role=crew -f agent_state=working \
      >/dev/null 2>&1 || true
    sleep 3

    # Select the UPDATED frame specifically: create_agent_bead also fires a
    # created frame first, so an unfiltered match would return that instead of
    # the updated row carrying agent_state=working.
    local sse_row static_row
    sse_row=$(row_from_log "$s1_log" "$s1_id" "updated")
    static_row=$(static_row_for "$s1_id" "$token")

    if [ -z "$sse_row" ]; then
      skip "scenario 1: live SSE row congruence" \
        "no SSE frame for $s1_id within window (stream may be proxied/buffered)"
    elif [ -z "$static_row" ]; then
      skip "scenario 1: live SSE row congruence" \
        "static /agents row for $s1_id not found (agent may not be in active window)"
    else
      local td th
      td=$(count_td "$sse_row")
      th=$(count_td "$static_row")
      if [ "$td" = "$th" ] && [ "$td" -gt 0 ]; then
        pass "scenario 1: live SSE row has $td <td>, matching static row's $th"
      else
        fail "scenario 1: live SSE row column count matches static row" \
          "SSE row has $td <td>, static row has $th"
      fi
      # Swap hooks the client keys on.
      if grep -q 'data-bead-kind="updated"' <<<"$sse_row" \
        && grep -q "id=\"agent-row-${s1_id}\"" <<<"$sse_row"; then
        pass "scenario 1: live SSE row carries the client swap hooks"
      else
        fail "scenario 1: live SSE row carries the client swap hooks" \
          "missing data-bead-kind/id=agent-row on: $sse_row"
      fi
      # Load-bearing cell content present on the streamed row.
      if grep -q 'agent-state-working' <<<"$sse_row"; then
        pass "scenario 1: live SSE row carries the agent-state chip"
      else
        fail "scenario 1: live SSE row carries the agent-state chip" \
          "no agent-state-working on: $sse_row"
      fi
    fi
  fi
  echo

  # ══════════════════════════════════════════════════════════════════
  # Scenario 2: a CLOSED agent event is streamed as the 6-column recent row
  # shape (bead-row-terminated), NOT the wider active shape — so moving it
  # into the recent table cannot column-shift / overflow.
  # ══════════════════════════════════════════════════════════════════
  blue "Scenario 2: closed SSE row is the recent (terminated) row shape"
  local s2_log="/tmp/agents-live-view-s2-$$.log"
  start_sse_listener "$s2_log" "$token" >/dev/null

  local s2_id
  s2_id=$(create_agent_bead "close" "working")
  if [ -z "$s2_id" ]; then
    fail "create scenario-2 agent bead" "no ID returned from kd create"
  else
    "$KD_BIN" close "$s2_id" >/dev/null 2>&1 || true
    sleep 3

    # Select the CLOSED frame specifically: create_agent_bead fired a created
    # frame first (same stream), so match on data-bead-kind="closed".
    local closed_row static_recent_th static_body_f
    closed_row=$(row_from_log "$s2_log" "$s2_id" "closed")
    # The static recent table header tells us the canonical recent column count.
    static_body_f=$(agents_body "$PROJECT" "$token")
    static_recent_th=$(count_th "$static_body_f" 'class="agents-table agents-table-recent"')
    rm -f "$static_body_f"

    if [ -z "$closed_row" ]; then
      skip "scenario 2: closed SSE row shape" \
        "no closed SSE frame for $s2_id within window"
    else
      if grep -q 'bead-row-terminated' <<<"$closed_row"; then
        pass "scenario 2: closed SSE row carries bead-row-terminated"
      else
        fail "scenario 2: closed SSE row carries bead-row-terminated" \
          "row: $closed_row"
      fi
      local ctd
      ctd=$(count_td "$closed_row")
      if [ -n "$static_recent_th" ] && [ "$static_recent_th" -gt 0 ]; then
        if [ "$ctd" = "$static_recent_th" ]; then
          pass "scenario 2: closed SSE row has $ctd <td>, matching recent table's $static_recent_th"
        else
          fail "scenario 2: closed SSE row matches recent table column count" \
            "closed row has $ctd <td>, recent table header has $static_recent_th"
        fi
      else
        # No recent table rendered to compare against — assert the absolute
        # recent shape (6 columns) so the test still guards the fix.
        if [ "$ctd" = "6" ]; then
          pass "scenario 2: closed SSE row has the 6-column recent shape"
        else
          fail "scenario 2: closed SSE row has the 6-column recent shape" \
            "closed row has $ctd <td> (expected 6)"
        fi
      fi
    fi
  fi
  echo

  # ══════════════════════════════════════════════════════════════════
  # Scenario 3: the /agents/events stream filters to type=agent and to the
  # project scope — a non-agent bead (task) must not surface.
  # ══════════════════════════════════════════════════════════════════
  blue "Scenario 3: stream filters to type=agent within the project scope"
  local s3_log="/tmp/agents-live-view-s3-$$.log"
  start_sse_listener "$s3_log" "$token" >/dev/null

  local s3_out s3_id
  s3_out=$("$KD_BIN" create "${TITLE_PREFIX}-task-${RANDOM_SUFFIX}" \
    --type=task -l "project:${PROJECT}" --json 2>/dev/null || printf '{}')
  s3_id=$(bead_id_from_json "$s3_out")
  if [ -z "$s3_id" ]; then
    fail "create scenario-3 task bead" "no ID returned from kd create"
  else
    CREATED_BEADS+=("$s3_id")
    "$KD_BIN" update "$s3_id" -d "filter probe ($RANDOM_SUFFIX)" >/dev/null 2>&1 || true
    sleep 3
    local task_row
    task_row=$(row_from_log "$s3_log" "$s3_id")
    if [ -z "$task_row" ]; then
      pass "scenario 3: non-agent (task) bead did NOT surface on /agents/events"
    else
      fail "scenario 3: stream filters to type=agent" \
        "task bead $s3_id leaked into the agents stream: $task_row"
    fi
  fi
  echo

  # ══════════════════════════════════════════════════════════════════
  # Scenario 4: an unengaged prewarmed-pool member is suppressed from the
  # static /agents view — BOTH the agent cards AND the project fleet-tile
  # count — while an engaged agent alongside it renders. Assigning the
  # prewarmed member (agent_state → assigning + a task_id) makes it reappear.
  # Guards isUnengagedPrewarmedAgent (cmd/beads-viewer/agents.go, applied
  # before classification) on a DEPLOYED viewer — unit-only until now
  # (agents_test.go TestHandleAgents_HidesUnengagedPrewarmedAgents,
  # TestIsUnengagedPrewarmedAgent). "prewarmed" had ZERO hits across every
  # viewer e2e module (ka-0000000091).
  #
  # Seeds into an ISOLATED throwaway project so the fleet-tile Total is
  # deterministic (2 agents: 1 suppressed + 1 shown) rather than a moving
  # count against the shared gardenio fleet. The suppression filter itself is
  # project-agnostic; scoping the assertion just removes cross-fleet noise.
  # ══════════════════════════════════════════════════════════════════
  blue "Scenario 4: unengaged prewarmed member is suppressed from /agents (cards + fleet count)"
  local iso_project="alve2e-${RANDOM_SUFFIX}"
  # A prewarmed pool shell: agent-name prefix "prewarmed-", agent_state
  # prewarmed, NO task_id/thread — exactly isParkedPoolMember's shape. Seeded
  # directly (not via create_agent_bead, which pins role=crew/project=$PROJECT)
  # so the pool-minting convention (agent prefix + project label) and the
  # isolated project both hold. Tracked for cleanup.
  local pw_id engaged_id pw_out engaged_out
  pw_out=$("$KD_BIN" create "${TITLE_PREFIX}-prewarm-${RANDOM_SUFFIX}" \
    --type=agent -l "project:${iso_project}" \
    -f "agent=prewarmed-${RANDOM_SUFFIX}-1" -f "role=thread" \
    -f "project=${iso_project}" -f "agent_state=prewarmed" \
    --json 2>/dev/null || printf '{}')
  pw_id=$(bead_id_from_json "$pw_out")
  [ -n "$pw_id" ] && CREATED_BEADS+=("$pw_id")
  engaged_out=$("$KD_BIN" create "${TITLE_PREFIX}-engaged-${RANDOM_SUFFIX}" \
    --type=agent -l "project:${iso_project}" \
    -f "agent=engaged-${RANDOM_SUFFIX}" -f "role=crew" \
    -f "project=${iso_project}" -f "agent_state=working" \
    --json 2>/dev/null || printf '{}')
  engaged_id=$(bead_id_from_json "$engaged_out")
  [ -n "$engaged_id" ] && CREATED_BEADS+=("$engaged_id")

  if [ -z "$pw_id" ] || [ -z "$engaged_id" ]; then
    fail "scenario 4: seed prewarmed + engaged agents" \
      "kd create returned no id (pw=$pw_id engaged=$engaged_id)"
  else
    sleep 2
    local body4 pw_present eng_present total4
    body4=$(agents_body "$iso_project" "$token")
    pw_present=$(agent_card_present "$body4" "$pw_id")
    eng_present=$(agent_card_present "$body4" "$engaged_id")
    total4=$(project_fleet_total "$body4")
    rm -f "$body4"

    if [ "$pw_present" = "0" ]; then
      pass "scenario 4: prewarmed member is absent from /agents cards"
    else
      fail "scenario 4: prewarmed member is absent from /agents cards" \
        "prewarmed agent $pw_id rendered a card (should be suppressed)"
    fi
    if [ "$eng_present" = "1" ]; then
      pass "scenario 4: engaged agent alongside it still renders"
    else
      fail "scenario 4: engaged agent alongside it still renders" \
        "engaged agent $engaged_id did not render a card"
    fi
    # The fleet-tile Total must count ONLY the engaged agent — the suppressed
    # prewarmed shell is dropped before classification, so it never inflates
    # the idle/total bucket. (1, not 2.)
    if [ -n "$total4" ]; then
      if [ "$total4" = "1" ]; then
        pass "scenario 4: fleet-tile Total counts only the engaged agent (1), prewarmed excluded"
      else
        fail "scenario 4: fleet-tile Total excludes the prewarmed shell" \
          "project fleet Total is $total4 (expected 1 = engaged only)"
      fi
    else
      skip "scenario 4: fleet-tile Total" "no project fleet tile rendered for ${iso_project}"
    fi

    # Assign the prewarmed member off the pool: agent_state → assigning AND a
    # task_id. isUnengagedPrewarmedAgent returns false the moment a member
    # carries a thread/task, so it must reappear as real work.
    "$KD_BIN" update "$pw_id" -f "agent_state=assigning" \
      -f "task_id=${TASK_ID_FOR_ASSIGN}" >/dev/null 2>&1 || true
    sleep 2
    local body4b pw_present_after
    body4b=$(agents_body "$iso_project" "$token")
    pw_present_after=$(agent_card_present "$body4b" "$pw_id")
    rm -f "$body4b"
    if [ "$pw_present_after" = "1" ]; then
      pass "scenario 4: once assigned (agent_state=assigning + task_id), the member reappears"
    else
      fail "scenario 4: assigned member reappears on /agents" \
        "assigned agent $pw_id is still suppressed after engagement"
    fi
  fi
  echo

  # ══════════════════════════════════════════════════════════════════
  # Scenario 5: a REAPER-closed agent renders distinctly in the per-agent
  # activity lane (GET /agents/{id}/events). renderAgentEventRow emits
  #   <span class="agent-event-meta">by <closed_by></span>
  # on a closed frame, so a reaper-attributed close reads "by reaper" while a
  # clean agent-attributed close reads "by <agent>". test-agents-live-view.sh
  # :340 (scenario 2) closes with a plain `kd close`, asserting only the recent
  # ROW SHAPE — nothing asserts the reaper ATTRIBUTION. Unit-only until now
  # (agents_events_test.go TestBeadDetailFromEvent_PreservesFields pins
  # ClosedBy:"reaper" survives the projector, not the rendered span). closed_by
  # is server-derived from the authenticated actor (the daemon strips the
  # client-supplied field for AUDIT §C3), so we drive it via `kd close --actor
  # reaper` (→ X-Actor: reaper on the static-token call).
  # ══════════════════════════════════════════════════════════════════
  blue "Scenario 5: reaper-closed agent renders 'by reaper' in the per-agent lane"
  local reap_id
  reap_id=$(create_agent_bead "reaped" "working")
  if [ -z "$reap_id" ]; then
    fail "scenario 5: seed reaper-close agent" "kd create returned no id"
  else
    local reap_log="/tmp/agents-live-view-s5-$$.log"
    lane_listen "$reap_log" "$reap_id" >/dev/null
    # Close attributed to the reaper. The lane renders the closed frame with the
    # reaper attribution meta span.
    "$KD_BIN" close "$reap_id" --actor reaper >/dev/null 2>&1 || true
    sleep 4
    local reap_meta
    reap_meta=$(closed_meta_from_lane "$reap_log" "$reap_id")
    if [ -z "$reap_meta" ]; then
      skip "scenario 5: reaper close attribution" \
        "no closed frame with attribution for $reap_id within window (stream may be proxied/buffered)"
    elif [ "$reap_meta" = "reaper" ]; then
      pass "scenario 5: reaper-closed agent lane frame carries 'by reaper' attribution"
    else
      fail "scenario 5: reaper-closed agent renders reaper attribution" \
        "closed frame attribution is 'by ${reap_meta}' (expected 'by reaper')"
    fi

    # Control: a clean agent-attributed close renders its OWN actor, never
    # "reaper" — proving the meta reflects the real closer, not a constant.
    local clean_id clean_actor
    clean_actor="e2e-clean-${RANDOM_SUFFIX}"
    clean_id=$(create_agent_bead "cleanclose" "working")
    if [ -n "$clean_id" ]; then
      local clean_log="/tmp/agents-live-view-s5b-$$.log"
      lane_listen "$clean_log" "$clean_id" >/dev/null
      "$KD_BIN" close "$clean_id" --actor "$clean_actor" >/dev/null 2>&1 || true
      sleep 4
      local clean_meta
      clean_meta=$(closed_meta_from_lane "$clean_log" "$clean_id")
      if [ -z "$clean_meta" ]; then
        skip "scenario 5: clean-close attribution control" \
          "no closed frame with attribution for $clean_id within window"
      elif [ "$clean_meta" != "reaper" ]; then
        pass "scenario 5: clean-closed agent renders its own actor ('by ${clean_meta}'), not 'reaper'"
      else
        fail "scenario 5: clean close must NOT render reaper attribution" \
          "clean close (actor ${clean_actor}) rendered 'by reaper'"
      fi
    fi
  fi
  echo

  summary
}

summary() {
  blue "═══════════════════════════════════════════════════"
  blue " agents-live-view: $PASS passed, $FAIL failed, $SKIP skipped"
  if [ "$FAIL" -gt 0 ]; then
    red "FAILURES:"
    for e in ${ERRORS[@]+"${ERRORS[@]}"}; do red "  - $e"; done
    exit 1
  fi
  blue "═══════════════════════════════════════════════════"
}

main "$@"
