#!/usr/bin/env bash
# test-news-alive-freshness.sh — `gb news` alive-freshness bound E2E
#
# Guards the freshness clause of `gb news`'s `alive` liveness primitive
# (newsAliveFreshnessWindow, gardenio/controller/cmd/gb/news.go; shipped !6652,
# rationale ka-0000000637). `alive` is the signal gardener prompts gate dispatch
# on — a regression here silently corrupts every fleet-wide capacity decision.
# Before !6652 it was unit-tested only (news_test.go); this module is the FIRST
# e2e exercise of the bound against a live daemon + real `gb` binary.
#
# THE BOUND. buildActiveAgents treats an open type=agent bead as alive only when
# its agent_state is non-terminal AND `now - updated_at <= freshWindow`. A live
# pod's bead is refreshed by the controller every COOP_SYNC_INTERVAL (~60s), so
# a despawned agent's bead ages past the window and stops reading alive — within
# ~5m instead of waiting for the 2h stale reaper. The window is overridable via
# GB_NEWS_ALIVE_FRESHNESS_WINDOW (a Go duration; <=0 disables the bound). That
# override is the documented testing seam and is what makes this module
# DETERMINISTIC: rather than sleep out a real 5m to age a bead (flaky, slow), we
# hold ONE fixture's real updated_at fixed and vary the WINDOW. A huge window
# puts the fresh write inside the bound (alive); a sub-nanosecond window puts the
# SAME write outside it (not alive). Both sides exercise the real
# `now.Sub(updatedAt) > freshWindow` comparison in the shipped binary — no
# sleeps, so it is stable under 5x reruns.
#
# Scenarios:
#   1. ALIVE side  — fresh agent + large window: the agent reads alive:true, its
#      in-progress issue lands in `in_progress` (live), NOT in `stale`; and
#      `gb news --active-only --ids-only` includes the issue while --stale-only
#      excludes it.
#   2. STALE side  — SAME fixture + tiny window: the agent reads alive:false, the
#      issue moves to `stale` (out of `in_progress`); and `--stale-only
#      --ids-only` includes the issue while --active-only excludes it. This is
#      the boundary the bound exists to enforce.
#   3. DISABLE valve — window=0 restores the prior agent_state-only behavior:
#      even with a would-be-stale window the agent reads alive:true.
#   4. TERMINAL control — an agent_state=done bead is NEVER alive at ANY window,
#      proving `alive` reflects real state, not a window-independent constant.
#
# Prerequisites: kubectl context on the target cluster (default gardenio-e2e),
# plus kd, gb, python3, curl in PATH. When run under run-shard.sh the beads
# tunnel + BEADS_* env are already exported; standalone it self-port-forwards.
#
# Self-SKIPs (exit 0) when the beads Service is absent, so it is inert in
# ephemeral per-instance namespaces and asserts only against a live daemon.
#
# HAZARD (mirrors test-agents-live-view.sh): an OPEN type=agent bead is a
# DESIRED pod to the controller reconciler (Reconcile -> create-missing), so
# leaving one open cold-starts a real crew pod. Every seeded agent bead is
# closed the instant its assertions complete, and the cleanup trap both closes
# any stragglers AND force-deletes any pod the reconciler managed to spawn, so
# this module is a good citizen against the long-lived staging singleton.
#
# Idempotency: every created bead carries the title/agent prefix
#   news-alive-freshness-e2e-<pid>-<epoch>
# and is closed in the cleanup trap. Reruns do not collide.

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
BEADS_PF_PORT="${BEADS_PF_PORT:-19703}"
KD_BIN="${KD_BIN:-kd}"
GB_BIN="${GB_BIN:-gb}"
PROJECT="${NEWS_ALIVE_PROJECT:-gardenio}"
PREFIX="news-alive-freshness-e2e"
SUFFIX="$$-$(date +%s)"

# Windows chosen to sit unambiguously on either side of ANY realistic bead age:
#   LARGE  — 8760h (~1 year): the fresh write is always inside the bound.
#   TINY   — 1ns: the same write is always outside the bound.
#   ZERO   — 0: disables the bound entirely (safety valve).
WIN_LARGE="8760h"
WIN_TINY="1ns"
WIN_ZERO="0"

PASS=0
FAIL=0
SKIP=0
ERRORS=()
BEADS_PF_PID=""
CREATED_BEADS=()
SEEDED_AGENTS=()

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

# close_bead <id> — close a seeded bead and drop it from the cleanup set so the
# trap does not double-close. Best-effort; a close failure is non-fatal.
close_bead() {
  local id="$1" i new=()
  [ -z "$id" ] && return 0
  "$KD_BIN" close "$id" >/dev/null 2>&1 || true
  for i in "${CREATED_BEADS[@]}"; do
    [ "$i" != "$id" ] && new+=("$i")
  done
  CREATED_BEADS=("${new[@]}")
}

cleanup() {
  # Close any bead that wasn't already closed inline.
  for id in "${CREATED_BEADS[@]}"; do
    "$KD_BIN" close "$id" >/dev/null 2>&1 || true
  done
  # Force-delete any pod the reconciler cold-started from a seeded agent bead
  # before we managed to close it. Best-effort; --wait=false so cleanup is fast.
  for agent in "${SEEDED_AGENTS[@]}"; do
    kubectl -n "$NAMESPACE" delete pod \
      -l "gardenio.io/agent=${agent}" \
      --force --grace-period=0 --wait=false >/dev/null 2>&1 || true
  done
  [ -n "$BEADS_PF_PID" ] && kill "$BEADS_PF_PID" 2>/dev/null || true
  rm -f /tmp/"${PREFIX}"-*.json 2>/dev/null || true
}
trap cleanup EXIT

# ── Setup ─────────────────────────────────────────────────────────
setup_beads_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    dim "  using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return 0
  fi
  if ! kubectl -n "$NAMESPACE" get svc "$BEADS_SVC" >/dev/null 2>&1; then
    yellow "beads service $BEADS_SVC not found in $NAMESPACE — skipping module"
    return 1
  fi
  blue "Port-forwarding ${NAMESPACE}/${BEADS_SVC} :${BEADS_PF_PORT} → 8080..."
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" \
    "${BEADS_PF_PORT}:8080" >/tmp/${PREFIX}-pf.log 2>&1 &
  BEADS_PF_PID=$!
  sleep 3
  export BEADS_HTTP_URL="http://localhost:${BEADS_PF_PORT}"
  if [ -z "${BEADS_TOKEN:-}" ]; then
    local secret
    secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
      -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
    [ -n "$secret" ] && export BEADS_TOKEN="$secret"
  fi
  return 0
}

# gb reads its bearer from BEADS_AUTH_TOKEN / BEADS_DAEMON_TOKEN; mirror whatever
# BEADS_TOKEN we resolved into both so `gb news` authenticates the same daemon.
propagate_token() {
  if [ -n "${BEADS_TOKEN:-}" ]; then
    export BEADS_AUTH_TOKEN="${BEADS_AUTH_TOKEN:-$BEADS_TOKEN}"
    export BEADS_DAEMON_TOKEN="${BEADS_DAEMON_TOKEN:-$BEADS_TOKEN}"
  fi
}

id_from_json() { printf '%s' "$1" | python3 -c "import sys,json;print(json.load(sys.stdin).get('id',''))" 2>/dev/null || printf ''; }

# seed_agent <label> <agent_state> — create an open type=agent bead carrying the
# schema-required [agent role project] fields (the type:agent schema 400s a bare
# create). Tracks the bead for cleanup AND the agent name for pod force-delete.
# Echoes "<bead-id> <agent-name>" (empty on failure).
seed_agent() {
  local label="$1" state="$2"
  local agent="${PREFIX}-${label}-${SUFFIX}"
  local out id
  out=$("$KD_BIN" create "${PREFIX}-agent-${label}-${SUFFIX}" \
    --type=agent \
    -l "project:${PROJECT}" \
    -f "agent=${agent}" \
    -f "role=crew" \
    -f "project=${PROJECT}" \
    -f "agent_state=${state}" \
    --json 2>/dev/null || printf '{}')
  id=$(id_from_json "$out")
  if [ -n "$id" ]; then
    CREATED_BEADS+=("$id")
    SEEDED_AGENTS+=("$agent")
    printf '%s %s' "$id" "$agent"
  fi
}

# seed_issue <label> <agent-name> — create an in_progress issue assigned to the
# given agent. Echoes the issue id (empty on failure).
seed_issue() {
  local label="$1" agent="$2"
  local out id
  out=$("$KD_BIN" create "${PREFIX}-task-${label}-${SUFFIX}" \
    --type=task -l "project:${PROJECT}" --json 2>/dev/null || printf '{}')
  id=$(id_from_json "$out")
  if [ -n "$id" ]; then
    CREATED_BEADS+=("$id")
    "$KD_BIN" update "$id" --assignee "$agent" --status=in_progress >/dev/null 2>&1 || true
    printf '%s' "$id"
  fi
}

# news_json <window> — run `gb news --all-projects --json` under the given
# freshness-window override and capture stdout to a temp file. --all-projects so
# the assertions are independent of the module's default project scope; the
# fixture agent/issue are uniquely-named so cross-project noise cannot match.
# Echoes the temp-file path (empty on failure).
news_json() {
  local window="$1" f
  f=$(mktemp /tmp/${PREFIX}-news-XXXXXX.json)
  if GB_NEWS_ALIVE_FRESHNESS_WINDOW="$window" \
      "$GB_BIN" news --all-projects --json >"$f" 2>/dev/null; then
    printf '%s' "$f"
  else
    rm -f "$f"
    printf ''
  fi
}

# agent_alive <news-json-file> <agent-name> — prints "true"/"false"/"absent".
agent_alive() {
  python3 - "$1" "$2" <<'PY'
import sys, json
try:
    d = json.load(open(sys.argv[1]))
except Exception:
    print("absent"); sys.exit(0)
name = sys.argv[2].lower()
for a in d.get("agents", []):
    if a.get("name", "").lower() == name:
        print("true" if a.get("alive") else "false"); sys.exit(0)
print("absent")
PY
}

# bead_in_section <news-json-file> <section> <bead-id> — "yes"/"no" whether the
# bead id appears in the named top-level array (in_progress|stale|...).
bead_in_section() {
  python3 - "$1" "$2" "$3" <<'PY'
import sys, json
try:
    d = json.load(open(sys.argv[1]))
except Exception:
    print("no"); sys.exit(0)
section, bid = sys.argv[2], sys.argv[3]
print("yes" if any(b.get("id") == bid for b in d.get(section, [])) else "no")
PY
}

# ids_only_has <window> <flag> <bead-id> — run `gb news <flag> --ids-only` under
# the window and report "yes"/"no" whether the bead id is emitted. <flag> is
# --active-only or --stale-only.
ids_only_has() {
  local window="$1" flag="$2" bid="$3" out
  out=$(GB_NEWS_ALIVE_FRESHNESS_WINDOW="$window" \
    "$GB_BIN" news --all-projects "$flag" --ids-only 2>/dev/null || printf '')
  # --ids-only emits "ID<TAB>title" per line (ka-0000007728); take field 1 so the
  # whole-line exact match still works against the bare bead ID.
  if printf '%s\n' "$out" | cut -f1 | grep -qxF "$bid"; then
    printf 'yes'
  else
    printf 'no'
  fi
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " gb news alive-freshness bound E2E"
  blue "═══════════════════════════════════════════════════"

  if ! setup_beads_portforward; then
    blue "═══════════════════════════════════════════════════"
    yellow " SKIP: beads Service absent — inert in ephemeral namespace"
    blue "═══════════════════════════════════════════════════"
    exit 0
  fi
  propagate_token

  if ! "$KD_BIN" list --status=open --limit 1 --json >/dev/null 2>&1; then
    red "ERROR: cannot reach kd server at ${BEADS_HTTP_URL:-<unset>}"
    exit 1
  fi
  green "  Server reachable: ${BEADS_HTTP_URL}"

  # Guard: the installed gb must actually carry the freshness bound. On a
  # pre-!6652 binary the override is a no-op and every side reads alive, which
  # would mask a genuine regression as a spurious pass. Detect it and SKIP
  # loudly rather than assert against a binary that cannot implement the bound.
  # Detection: a fresh agent must flip alive true(large-window)->false(tiny) —
  # the one behavior a pre-bound binary cannot produce.
  echo
  blue "Preflight: confirm gb carries the freshness bound"
  local probe_pair probe_bead probe_agent probe_issue
  probe_pair=$(seed_agent "probe" "working")
  if [ -z "$probe_pair" ]; then
    fail "preflight: seed probe agent" "kd create --type=agent returned no id"
    summary; return
  fi
  probe_bead="${probe_pair%% *}"; probe_agent="${probe_pair##* }"
  probe_issue=$(seed_issue "probe" "$probe_agent")

  local pf_large pf_tiny alive_large alive_tiny
  pf_large=$(news_json "$WIN_LARGE"); alive_large=$(agent_alive "$pf_large" "$probe_agent")
  pf_tiny=$(news_json "$WIN_TINY");   alive_tiny=$(agent_alive "$pf_tiny" "$probe_agent")
  rm -f "$pf_large" "$pf_tiny" 2>/dev/null || true
  # Close the probe fixture immediately (pod-spawn hazard).
  close_bead "$probe_issue"; close_bead "$probe_bead"

  if [ "$alive_large" = "absent" ]; then
    skip "preflight: gb news did not surface the probe agent" \
      "large-window alive=absent (daemon empty or agent list capped?)"
    summary; return
  fi
  if [ "$alive_large" = "true" ] && [ "$alive_tiny" = "false" ]; then
    pass "preflight: gb honors GB_NEWS_ALIVE_FRESHNESS_WINDOW (alive true→false across the bound)"
  else
    skip "preflight: installed gb predates the freshness bound" \
      "alive did not flip across the window (large=${alive_large}, tiny=${alive_tiny}); pre-!6652 binary"
    summary; return
  fi
  echo

  # ── Scenario 1: ALIVE side (fresh write + large window) ──────────
  blue "Scenario 1: fresh agent + large window ⇒ alive, issue is live (not stale)"
  local pair bead agent issue
  pair=$(seed_agent "live" "working")
  if [ -z "$pair" ]; then
    fail "scenario 1: seed live agent" "kd create returned no id"
  else
    bead="${pair%% *}"; agent="${pair##* }"
    issue=$(seed_issue "live" "$agent")
    if [ -z "$issue" ]; then
      fail "scenario 1: seed live issue" "kd create returned no id"
    else
      local f a s1_ip s1_stale s1_active_ids s1_stale_ids
      f=$(news_json "$WIN_LARGE")
      a=$(agent_alive "$f" "$agent")
      s1_ip=$(bead_in_section "$f" "in_progress" "$issue")
      s1_stale=$(bead_in_section "$f" "stale" "$issue")
      rm -f "$f" 2>/dev/null || true
      s1_active_ids=$(ids_only_has "$WIN_LARGE" "--active-only" "$issue")
      s1_stale_ids=$(ids_only_has "$WIN_LARGE" "--stale-only" "$issue")

      [ "$a" = "true" ]        && pass "agent reads alive:true"                    || fail "agent reads alive:true" "got alive=$a"
      [ "$s1_ip" = "yes" ]     && pass "issue in in_progress (live)"               || fail "issue in in_progress" "not found in in_progress"
      [ "$s1_stale" = "no" ]   && pass "issue NOT in stale"                        || fail "issue NOT in stale" "unexpectedly in stale"
      [ "$s1_active_ids" = "yes" ] && pass "--active-only --ids-only includes it"  || fail "--active-only includes it" "issue absent from --active-only"
      [ "$s1_stale_ids" = "no" ]   && pass "--stale-only --ids-only excludes it"   || fail "--stale-only excludes it" "issue present in --stale-only"
    fi
    # Keep the fixture OPEN into scenario 2 (same bead, flipped window), then
    # close. Record ids for scenario 2 + final close.
    S2_BEAD="$bead"; S2_AGENT="$agent"; S2_ISSUE="${issue:-}"
  fi
  echo

  # ── Scenario 2: STALE side (SAME fixture + tiny window) ──────────
  blue "Scenario 2: same agent + tiny window ⇒ NOT alive, issue is stale"
  if [ -z "${S2_ISSUE:-}" ]; then
    fail "scenario 2: reuse fixture" "scenario 1 fixture missing"
  else
    local f a s2_ip s2_stale s2_active_ids s2_stale_ids
    f=$(news_json "$WIN_TINY")
    a=$(agent_alive "$f" "$S2_AGENT")
    s2_ip=$(bead_in_section "$f" "in_progress" "$S2_ISSUE")
    s2_stale=$(bead_in_section "$f" "stale" "$S2_ISSUE")
    rm -f "$f" 2>/dev/null || true
    s2_active_ids=$(ids_only_has "$WIN_TINY" "--active-only" "$S2_ISSUE")
    s2_stale_ids=$(ids_only_has "$WIN_TINY" "--stale-only" "$S2_ISSUE")

    [ "$a" = "false" ]       && pass "agent reads alive:false"               || fail "agent reads alive:false" "got alive=$a"
    [ "$s2_stale" = "yes" ]  && pass "issue moved to stale"                  || fail "issue moved to stale" "not found in stale"
    [ "$s2_ip" = "no" ]      && pass "issue NOT in in_progress"              || fail "issue NOT in in_progress" "unexpectedly in in_progress"
    [ "$s2_stale_ids" = "yes" ] && pass "--stale-only --ids-only includes it" || fail "--stale-only includes it" "issue absent from --stale-only"
    [ "$s2_active_ids" = "no" ] && pass "--active-only --ids-only excludes it" || fail "--active-only excludes it" "issue present in --active-only"
  fi

  # ── Scenario 3: DISABLE valve (window=0) ─────────────────────────
  blue "Scenario 3: window=0 disables the bound ⇒ same fixture reads alive:true"
  if [ -z "${S2_AGENT:-}" ]; then
    fail "scenario 3: reuse fixture" "fixture missing"
  else
    local f a
    f=$(news_json "$WIN_ZERO")
    a=$(agent_alive "$f" "$S2_AGENT")
    rm -f "$f" 2>/dev/null || true
    [ "$a" = "true" ] && pass "window=0 restores alive:true (bound disabled)" \
                      || fail "window=0 disables bound" "got alive=$a (expected true)"
  fi
  # Fixture done — close it now (pod-spawn hazard).
  close_bead "${S2_ISSUE:-}"; close_bead "${S2_BEAD:-}"
  echo

  # ── Scenario 4: TERMINAL control (alive is not a constant) ───────
  blue "Scenario 4: agent_state=done is NEVER alive, at any window"
  local tpair tbead tagent
  tpair=$(seed_agent "done" "done")
  if [ -z "$tpair" ]; then
    fail "scenario 4: seed done agent" "kd create returned no id"
  else
    tbead="${tpair%% *}"; tagent="${tpair##* }"
    # Close the terminal fixture BEFORE the reads — the sibling precedent this
    # module already follows (the probe close and the Scenario 3 close, both
    # tagged "pod-spawn hazard"). Unlike the non-terminal fixtures, closing FIRST is
    # load-bearing here: an OPEN agent_state=done bead is a *desired pod* to the
    # reconciler (create-missing skips only agent_state=failed, not done), so it
    # gets cold-started and its agent_state flipped off the seeded terminal value
    # within ~350ms. Asserting against the still-open fixture therefore RACES the
    # controller, and `gb news` correctly reports alive:true whenever the flip
    # lands inside the ~350-500ms assert window (ka-0000004842: 6/13 shard-0
    # attempts, masked by harness auto-retry). `gb news` queries only OPEN
    # type=agent beads, so closing first makes the fixture deterministically
    # invisible: it can never read alive on ANY window, independent of the shared
    # staging namespace's concurrent pod pressure. This does NOT weaken the
    # control — the assertion still fails if gb news ever reports this agent
    # alive:true; it only removes the reconciler's race from the fixture.
    close_bead "$tbead"
    local f a_large a_zero
    f=$(news_json "$WIN_LARGE"); a_large=$(agent_alive "$f" "$tagent"); rm -f "$f" 2>/dev/null || true
    f=$(news_json "$WIN_ZERO");  a_zero=$(agent_alive "$f" "$tagent");  rm -f "$f" 2>/dev/null || true
    # A terminal / closed agent with no in-progress issue does not appear in the
    # agents list at all (absent) — either "absent" or "false" proves it is not
    # reported alive; only "true" is a failure.
    [ "$a_large" != "true" ] && pass "done agent not alive under large window (got: $a_large)" \
                             || fail "done agent not alive (large window)" "reported alive:true"
    [ "$a_zero" != "true" ]  && pass "done agent not alive even with bound disabled (got: $a_zero)" \
                             || fail "done agent not alive (window=0)" "reported alive:true"
  fi
  echo

  summary
}

summary() {
  blue "═══════════════════════════════════════════════════"
  blue " news-alive-freshness: $PASS passed, $FAIL failed, $SKIP skipped"
  if [ "$FAIL" -gt 0 ]; then
    red "FAILURES:"
    for e in "${ERRORS[@]}"; do red "  - $e"; done
    exit 1
  fi
  blue "═══════════════════════════════════════════════════"
  exit 0
}

main "$@"
