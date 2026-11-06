#!/usr/bin/env bash
# slack-selftest.sh — autonomous end-to-end test of a gardenio-in-a-box Slack workflow.
#
# Tracking bead ka-0000005899 · epic ka-0000006506 (gasbox currency+availability).
# Operator: Matt Baker.
#
# WHAT THIS PROVES (the operator's real bar)
#   The box is NOT "working" just because ingress + login load. It is working only
#   when an operator can actually @mention the gardenio app in Slack and get a real
#   agent to spin up and reply. This script exercises that *whole* round-trip with
#   NO human in the loop:
#
#       1. POST  a "@gardenio <nonce>" mention into the box's Slack channel,
#       2. WAIT  for the bridge to react (spawn) — an :eyes:/:hourglass: reaction
#          on our message and/or a threaded bot reply,
#       3. EXIT  0 if the round-trip completed within the window, 1 otherwise.
#
# WHY A USER TOKEN (not the box's bot token)
#   The bridge DROPS any mention authored by a bot (controller/internal/bridge/
#   bot_mentions.go:31 — `if ev.BotID != "" { return }`) and ignores messages from
#   its own user id. So the box's `xoxb-` bot token CANNOT self-trigger. Driving the
#   workflow autonomously requires a SECOND, human-style identity: a Slack USER token
#   (`xoxp-…`) for a dedicated user in the same workspace, with scopes
#   `chat:write`, `channels:history`, `channels:read` (or the `groups:*` equivalents
#   for a private channel). That token is what `--token` / SLACK_SELFTEST_TOKEN holds.
#
# This runs from ANYWHERE with outbound HTTPS to slack.com — the gardener pod, CI, a
# laptop. It does NOT need to be on the box (the bridge's socket-mode connection to
# Slack is what carries the event); it only needs the user token + the channel id.
#
# Exit 0 = round-trip verified. Exit 1 = posted but no bot reaction in the window.
# Exit 2 = usage / precondition error (missing token, bad channel, Slack API error).
#
# Usage:
#   scripts/inbox/slack-selftest.sh --channel C0XXXXXXXXX [--token xoxp-…] \
#       [--timeout 180] [--text "custom mention body"] [--bot-user U0…] [--cleanup]
#
#   # Token from env (preferred — never put a token on the command line in CI):
#   SLACK_SELFTEST_TOKEN=xoxp-… scripts/inbox/slack-selftest.sh --channel C0BA2HJGLLB
#
set -uo pipefail

SLACK_API="${SLACK_API:-https://slack.com/api}"
TOKEN="${SLACK_SELFTEST_TOKEN:-}"
CHANNEL=""
TIMEOUT="${SLACK_SELFTEST_TIMEOUT:-180}"
TEXT=""
BOT_USER="${SLACK_SELFTEST_BOT_USER:-}"   # optional: pre-known bot user id (U…) to address
CLEANUP=0
POLL_INTERVAL="${SLACK_SELFTEST_POLL_INTERVAL:-5}"

while [ $# -gt 0 ]; do
  case "$1" in
    --channel) CHANNEL="$2"; shift 2;;
    --token) TOKEN="$2"; shift 2;;
    --timeout) TIMEOUT="$2"; shift 2;;
    --text) TEXT="$2"; shift 2;;
    --bot-user) BOT_USER="$2"; shift 2;;
    --cleanup) CLEANUP=1; shift;;
    -h|--help) sed -n '2,46p' "$0"; exit 0;;
    *) echo "unknown arg: $1 (try --help)" >&2; exit 2;;
  esac
done

red()  { printf '\033[0;31m%s\033[0m\n' "$*"; }
grn()  { printf '\033[0;32m%s\033[0m\n' "$*"; }
info() { printf '\033[2m%s\033[0m\n' "$*"; }

[ -n "$TOKEN" ]   || { red "FAIL: no Slack user token (set SLACK_SELFTEST_TOKEN or --token xoxp-…)"; exit 2; }
[ -n "$CHANNEL" ] || { red "FAIL: --channel C0XXXXXXXXX required"; exit 2; }
command -v curl >/dev/null 2>&1 || { red "FAIL: curl not found"; exit 2; }
command -v python3 >/dev/null 2>&1 || { red "FAIL: python3 not found"; exit 2; }

case "$TOKEN" in
  xoxp-*) ;;
  xoxb-*) red "FAIL: that is a BOT token (xoxb-). The bridge drops bot-authored mentions; you need a USER token (xoxp-)."; exit 2;;
  *) info "NOTE: token does not start with xoxp- — proceeding, but a Slack user token is expected.";;
esac

# slack_call <method> <curl-data-args...> — POST to the Slack Web API, echo the
# JSON body, and fail (return non-zero) if the API reports ok=false.
slack_call() {
  local method="$1"; shift
  local resp
  resp="$(curl -sS --max-time 15 -X POST "${SLACK_API}/${method}" \
            -H "Authorization: Bearer ${TOKEN}" \
            -H "Content-Type: application/x-www-form-urlencoded; charset=utf-8" \
            "$@" 2>/dev/null)" || { echo '{"ok":false,"error":"curl_failed"}'; return 1; }
  echo "$resp"
  echo "$resp" | python3 -c "import sys,json; sys.exit(0 if json.load(sys.stdin).get('ok') else 1)" 2>/dev/null
}

jqpy() { python3 -c "import sys,json; d=json.load(sys.stdin); print($1)" 2>/dev/null; }

# ── 0. Who are we? Confirm the token authenticates. ──────────────────────────
echo "== Slack self-test: auth =="
AUTH="$(slack_call auth.test)" || { red "FAIL: auth.test failed: $(echo "$AUTH" | jqpy "d.get('error','?')")"; exit 2; }
SELF_USER="$(echo "$AUTH" | jqpy "d.get('user_id','')")"
TEAM="$(echo "$AUTH" | jqpy "d.get('team','')")"
grn "  PASS: authenticated as user=${SELF_USER} team=${TEAM}"

# ── 1. Resolve the bot user id so we address a real @mention. ────────────────
# If --bot-user not supplied, discover the gardenio bot in the workspace by
# scanning channel members for a bot-flagged user. Falls back to plain text
# (the bridge still treats top-level channel messages w/ a mention, and in DMs
# every message is implicitly a mention) but a real <@bot> is most faithful.
if [ -z "$BOT_USER" ]; then
  MEMBERS="$(slack_call conversations.members --data-urlencode "channel=${CHANNEL}" --data-urlencode "limit=100")" || true
  for u in $(echo "$MEMBERS" | jqpy "' '.join(d.get('members',[]))"); do
    INFO="$(curl -sS --max-time 10 "${SLACK_API}/users.info?user=${u}" -H "Authorization: Bearer ${TOKEN}" 2>/dev/null)"
    if echo "$INFO" | python3 -c "import sys,json; d=json.load(sys.stdin); u=d.get('user',{}); sys.exit(0 if (u.get('is_bot') and not u.get('deleted')) else 1)" 2>/dev/null; then
      BOT_USER="$u"; break
    fi
  done
fi
if [ -n "$BOT_USER" ]; then
  info "  bot user resolved: ${BOT_USER}"
  MENTION="<@${BOT_USER}> "
else
  info "  NOTE: could not resolve a bot user in ${CHANNEL}; posting plain text (DM/group fallback)."
  MENTION=""
fi

# ── 2. Post the mention with a unique nonce. ─────────────────────────────────
NONCE="selftest-$(date +%s)-$$"
[ -n "$TEXT" ] || TEXT="ping — autonomous gasbox slack self-test ${NONCE}. Reply 'pong' to confirm the round-trip."
BODY="${MENTION}${TEXT}"
echo "== posting mention (nonce=${NONCE}) =="
POST="$(slack_call chat.postMessage --data-urlencode "channel=${CHANNEL}" --data-urlencode "text=${BODY}")" \
  || { red "FAIL: chat.postMessage failed: $(echo "$POST" | jqpy "d.get('error','?')")"; exit 2; }
MSG_TS="$(echo "$POST" | jqpy "d.get('ts','')")"
[ -n "$MSG_TS" ] || { red "FAIL: post returned no ts"; exit 2; }
grn "  PASS: posted ts=${MSG_TS}"

# ── 3. Poll for the bot's reaction: a threaded reply OR a lifecycle reaction. ─
# The bridge signals spawn two ways, either is sufficient proof:
#   (a) a reaction (eyes/hourglass_flowing_sand/white_check_mark) on OUR message,
#   (b) a threaded reply authored by the bot under our message ts.
echo "== waiting up to ${TIMEOUT}s for the bot to react =="
elapsed=0
RESULT="timeout"
while [ "$elapsed" -lt "$TIMEOUT" ]; do
  # (a) reactions on our message
  REACTIONS="$(curl -sS --max-time 10 "${SLACK_API}/reactions.get?channel=${CHANNEL}&timestamp=${MSG_TS}" \
                 -H "Authorization: Bearer ${TOKEN}" 2>/dev/null)"
  if echo "$REACTIONS" | python3 -c "
import sys,json
d=json.load(sys.stdin)
rs=(d.get('message') or {}).get('reactions',[]) if d.get('ok') else []
names={r.get('name') for r in rs}
sys.exit(0 if names & {'eyes','hourglass_flowing_sand','hourglass','white_check_mark','robot_face'} else 1)
" 2>/dev/null; then
    RESULT="reaction"; break
  fi
  # (b) a bot-authored reply in our thread
  REPLIES="$(curl -sS --max-time 10 "${SLACK_API}/conversations.replies?channel=${CHANNEL}&ts=${MSG_TS}&limit=20" \
               -H "Authorization: Bearer ${TOKEN}" 2>/dev/null)"
  if echo "$REPLIES" | python3 -c "
import sys,json
d=json.load(sys.stdin)
msgs=d.get('messages',[]) if d.get('ok') else []
# any reply that is NOT our own original message and is bot-authored / from another user
self='${SELF_USER}'; ts='${MSG_TS}'
for m in msgs:
    if m.get('ts')==ts:
        continue
    if m.get('bot_id') or m.get('subtype')=='bot_message' or m.get('user')!=self:
        sys.exit(0)
sys.exit(1)
" 2>/dev/null; then
    RESULT="reply"; break
  fi
  sleep "$POLL_INTERVAL"
  elapsed=$((elapsed + POLL_INTERVAL))
done

# ── 4. Optional cleanup of the probe message. ────────────────────────────────
if [ "$CLEANUP" = "1" ] && [ -n "$MSG_TS" ]; then
  slack_call chat.delete --data-urlencode "channel=${CHANNEL}" --data-urlencode "ts=${MSG_TS}" >/dev/null 2>&1 || true
fi

# ── 5. Verdict. ──────────────────────────────────────────────────────────────
case "$RESULT" in
  reaction) grn "PASS: bridge reacted to the mention within ${elapsed}s (lifecycle reaction) — Slack round-trip verified."; exit 0;;
  reply)    grn "PASS: bot posted a threaded reply within ${elapsed}s — Slack round-trip verified."; exit 0;;
  *)        red "FAIL: posted mention ts=${MSG_TS} but saw no bot reaction within ${TIMEOUT}s."
            red "      The mention reached Slack but the box did not spawn/reply. Check: socket-mode connected,"
            red "      agents.enabled=true, agent pods not stuck Pending, mention-gate not denying this user."
            exit 1;;
esac
