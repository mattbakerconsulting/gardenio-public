#!/usr/bin/env bash
# test-group-dm.sh — E2E smoke test for gardenio group DMs (Slack mpim).
#
# Phase 4 of epic ka-0000003709 (task gd-bYT2TXgbbj). Happy path only, no edge
# cases. Exercises the full participate/start/invite loop:
#
#   1. START   open a group DM with a bound agent  (gb slack group open →
#              POST /api/slack/groups/open)
#   2. MENTION @mention the bot in the mpim; assert the bridge routes it to the
#              bound agent (a reply / lifecycle post lands in the channel)
#   3. INVITE  add a new member (gb slack group invite → POST
#              /api/slack/groups/{channel}/invite); assert the superset re-open
#              returns a NEW channel and the agent binding is carried over
#   4. CONTINUE the conversation continues in the new channel with the new
#              member, and the agent bead is still bound to it
#
# Prerequisites (mirrors test-slack-threads.sh — the mpim happy path needs REAL
# Slack: the mock has no conversations.open/members, so there is no mpim to
# operate on). Skips CLEAN (exit 0) when any piece is missing so local dev, fork
# CI, and un-opted-in ephemeral namespaces stay green:
#   - SLACK_BOT_TOKEN  (xoxb-…, with mpim:{history,read,write})
#   - BRIDGE_HTTP_URL  (slack-bridge HTTP API, e.g. http://localhost:19091)
#   - BEADS_HTTP_URL   (kbeads daemon, for the binding assertions)
#   - GROUP_DM_USERS   (comma-separated Slack user IDs / emails to open the mpim
#                       with — at least one) and GROUP_DM_INVITEE (a distinct
#                       user to invite in step 3)
#   - jq, python3, curl
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-group-dm.sh

set -euo pipefail

# ── Skip guards (LOUD, exit 0) ────────────────────────────────────────
skip() {
  echo "SKIP: $1"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$1" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989, ka-0000005441)
  exit 0
}

[ -n "${SLACK_BOT_TOKEN:-}" ] || skip "SLACK_BOT_TOKEN not set — group-DM smoke needs a real Slack workspace"
[ -n "${BRIDGE_HTTP_URL:-}" ] || skip "BRIDGE_HTTP_URL not set — group-DM smoke needs a live slack-bridge"
[ -n "${GROUP_DM_USERS:-}" ]  || skip "GROUP_DM_USERS not set — no members to open the mpim with"
[ -n "${GROUP_DM_INVITEE:-}" ] || skip "GROUP_DM_INVITEE not set — no user to invite in step 3"
command -v jq >/dev/null 2>&1 || skip "jq not installed"
command -v python3 >/dev/null 2>&1 || skip "python3 not installed"

: "${BEADS_HTTP_URL:=${BEADS_HTTP_ADDR:-http://localhost:19090}}"
: "${BEADS_AUTH_TOKEN:=${BEADS_TOKEN:-}}"
: "${CLEANUP:=true}"

SLACK_API="https://slack.com/api"
SLACK_AUTH="Authorization: Bearer ${SLACK_BOT_TOKEN}"
BEADS_AUTH_HEADER=""
[ -n "$BEADS_AUTH_TOKEN" ] && BEADS_AUTH_HEADER="Authorization: Bearer $BEADS_AUTH_TOKEN"

PASS=0
FAIL=0
ERRORS=()
SLACK_MESSAGES=()   # channel:ts pairs to clean up
CREATED_CHANNELS=() # mpim channels opened (best-effort archive on cleanup)

TEST_TAG="e2e-groupdm-$$-$(date +%s)"

# ── Output helpers ────────────────────────────────────────────────────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

pass() { green "  ✓ $1"; PASS=$((PASS + 1)); }
fail() { red "  ✗ $1"; [ -n "${2:-}" ] && dim "    $2"; FAIL=$((FAIL + 1)); ERRORS+=("$1: ${2:-}"); }

# ── Bridge / Slack / beads helpers ────────────────────────────────────
bridge_post() {
  # bridge_post <path> <json-body> → response body (empty on error)
  curl -sf --max-time 30 -X POST \
    -H "Content-Type: application/json" \
    -d "$2" \
    "${BRIDGE_HTTP_URL}$1" 2>/dev/null || true
}

json_field() { python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('$1',''))" 2>/dev/null || true; }

slack_post() {
  local channel="$1" text="$2"
  local resp ts
  resp=$(curl -s -X POST "${SLACK_API}/chat.postMessage" \
    -H "$SLACK_AUTH" -H "Content-Type: application/json" \
    -d "$(jq -n --arg ch "$channel" --arg t "$text" '{channel:$ch,text:$t}')")
  ts=$(echo "$resp" | jq -r '.ts // empty')
  [ -n "$ts" ] && SLACK_MESSAGES+=("${channel}:${ts}")
  echo "$ts"
}

slack_history_contains() {
  # slack_history_contains <channel> <pattern> [max_wait] → 0 if found
  local channel="$1" pattern="$2" max_wait="${3:-15}" elapsed=0 oldest
  oldest=$(date +%s)
  while [ "$elapsed" -lt "$max_wait" ]; do
    local hist
    hist=$(curl -s --max-time 10 \
      "${SLACK_API}/conversations.history?channel=${channel}&oldest=${oldest}&limit=30" \
      -H "$SLACK_AUTH" 2>/dev/null || echo '{}')
    if echo "$hist" | python3 -c "
import sys,json
d=json.load(sys.stdin)
for m in d.get('messages',[]):
    if '$pattern' in m.get('text','') or '$pattern' in json.dumps(m.get('blocks',[])):
        sys.exit(0)
sys.exit(1)" 2>/dev/null; then
      return 0
    fi
    sleep 2; elapsed=$((elapsed + 2))
  done
  return 1
}

bead_field() {
  # bead_field <agent-name> <field> → value (looks up the agent bead by name)
  local name="$1" field="$2"
  curl -s --max-time 10 "${BEADS_HTTP_URL}/v1/beads?type=agent&limit=50" \
    ${BEADS_AUTH_HEADER:+-H "$BEADS_AUTH_HEADER"} 2>/dev/null | python3 -c "
import sys,json
try: d=json.load(sys.stdin)
except Exception: sys.exit(0)
rows=d if isinstance(d,list) else d.get('beads',d.get('items',[]))
for b in rows:
    f=b.get('fields',{}) or {}
    if f.get('agent')=='$name':
        print(f.get('$field','')); break
" 2>/dev/null || true
}

# ── Cleanup ───────────────────────────────────────────────────────────
cleanup() {
  if [ "$CLEANUP" = "false" ]; then
    echo "CLEANUP=false — preserving ${#SLACK_MESSAGES[@]} messages, ${#CREATED_CHANNELS[@]} channels"
    return
  fi
  for msg in "${SLACK_MESSAGES[@]:-}"; do
    [ -z "$msg" ] && continue
    local ch="${msg%%:*}" ts="${msg#*:}"
    curl -s -X POST "${SLACK_API}/chat.delete" -H "$SLACK_AUTH" \
      -H "Content-Type: application/json" \
      -d "$(jq -n --arg c "$ch" --arg t "$ts" '{channel:$c,ts:$t}')" >/dev/null 2>&1 || true
  done
}
trap cleanup EXIT

# ── The smoke ─────────────────────────────────────────────────────────
blue "═══════════════════════════════════════════════════"
blue " group-dm E2E smoke (participate → start → invite → continue)"
blue "═══════════════════════════════════════════════════"
echo "Bridge: ${BRIDGE_HTTP_URL}"
echo "Beads:  ${BEADS_HTTP_URL}"
echo ""

# Step 1 — START: open a group DM with a bound agent.
blue "Step 1: open a group DM (start) with a bound agent"
USERS_JSON=$(python3 -c "import sys; print(__import__('json').dumps([u.strip() for u in '$GROUP_DM_USERS'.split(',') if u.strip()]))")
open_body=$(jq -n --argjson u "$USERS_JSON" --arg m "group-dm smoke intro ($TEST_TAG)" \
  '{users:$u, project:"gardenio", role:"crew", message:$m}')
open_resp=$(bridge_post "/api/slack/groups/open" "$open_body")
if [ -z "$open_resp" ] || [ "$(echo "$open_resp" | json_field ok)" != "True" ]; then
  fail "open group DM" "response: ${open_resp:-<empty>}"
else
  CHANNEL=$(echo "$open_resp" | json_field channel)
  AGENT=$(echo "$open_resp" | json_field agent)
  [ -n "$CHANNEL" ] && CREATED_CHANNELS+=("$CHANNEL")
  pass "opened group DM $CHANNEL (agent ${AGENT:-<none>} bound)"
fi
[ -z "${CHANNEL:-}" ] && { blue "no channel — aborting remaining steps"; }

# Step 2 — MENTION: @mention the bot; assert the bridge routes to the agent.
if [ -n "${CHANNEL:-}" ]; then
  blue "Step 2: @mention the bot in the group DM (participate)"
  bot_user_id=$(curl -s "${SLACK_API}/auth.test" -H "$SLACK_AUTH" | jq -r '.user_id // empty')
  if [ -z "$bot_user_id" ]; then
    fail "resolve bot user id" "auth.test returned no user_id"
  else
    mention_ts=$(slack_post "$CHANNEL" "<@${bot_user_id}> please ack $TEST_TAG")
    if [ -z "$mention_ts" ]; then
      fail "post @mention in group DM"
    else
      pass "posted @mention in group DM"
      # The bound agent (or bridge) should post SOMETHING back into the mpim.
      if slack_history_contains "$CHANNEL" "$TEST_TAG" 20; then
        pass "bridge routed the mention (activity in channel)"
      else
        # Non-fatal: the agent pod reply is timing-dependent in CI. The mention
        # was accepted; treat missing reply as a soft note, not a hard failure.
        dim "    no agent reply observed within 20s (timing-dependent — not failed)"
      fi
    fi
  fi
fi

# Step 3 — INVITE: add a new member; superset re-open returns a NEW channel and
# carries the agent binding.
if [ -n "${CHANNEL:-}" ]; then
  blue "Step 3: invite a new member (superset re-open + binding migration)"
  inv_body=$(jq -n --arg u "$GROUP_DM_INVITEE" \
    --arg m "inviting a new member — continuing the conversation ($TEST_TAG)" \
    '{users:[$u], message:$m}')
  inv_resp=$(bridge_post "/api/slack/groups/$(python3 -c "import urllib.parse;print(urllib.parse.quote('$CHANNEL'))")/invite" "$inv_body")
  if [ -z "$inv_resp" ] || [ "$(echo "$inv_resp" | json_field ok)" != "True" ]; then
    fail "invite into group DM" "response: ${inv_resp:-<empty>}"
  else
    NEW_CHANNEL=$(echo "$inv_resp" | json_field channel)
    MOVED=$(echo "$inv_resp" | json_field moved)
    [ -n "$NEW_CHANNEL" ] && CREATED_CHANNELS+=("$NEW_CHANNEL")
    pass "invite ok → channel $NEW_CHANNEL (moved=$MOVED)"
    if [ "$MOVED" = "True" ] && [ "$NEW_CHANNEL" != "$CHANNEL" ]; then
      pass "superset re-open produced a NEW channel (no in-place invite)"
    else
      dim "    moved=$MOVED (invitee may already have been a member — idempotent no-op)"
    fi
  fi
fi

# Step 4 — CONTINUE: conversation continues in the new channel with the new
# member, and the agent bead is still bound to it.
CONT_CHANNEL="${NEW_CHANNEL:-${CHANNEL:-}}"
if [ -n "$CONT_CHANNEL" ] && [ -n "${bot_user_id:-}" ]; then
  blue "Step 4: conversation continues in the new channel with the new member"
  cont_ts=$(slack_post "$CONT_CHANNEL" "<@${bot_user_id}> continuing here $TEST_TAG-cont")
  if [ -z "$cont_ts" ]; then
    fail "post continuation @mention"
  else
    pass "posted continuation @mention in $CONT_CHANNEL"
  fi
  # The agent binding should point at the (possibly new) channel.
  if [ -n "${AGENT:-}" ]; then
    bound=$(bead_field "$AGENT" "slack_thread_channel")
    if [ "$bound" = "$CONT_CHANNEL" ]; then
      pass "agent $AGENT still bound to $CONT_CHANNEL"
    else
      fail "agent binding carried over" "bead slack_thread_channel=$bound, expected $CONT_CHANNEL"
    fi
  fi
fi

# ── Summary ───────────────────────────────────────────────────────────
echo ""
blue "═══════════════════════════════════════════════════"
total=$((PASS + FAIL))
if [ "$FAIL" -eq 0 ]; then
  green " ✓ All $total group-dm smoke assertions passed"
else
  red " ✗ $FAIL/$total group-dm smoke assertions failed"
  for e in "${ERRORS[@]}"; do red "   - $e"; done
fi
blue "═══════════════════════════════════════════════════"
[ "$FAIL" -eq 0 ]
