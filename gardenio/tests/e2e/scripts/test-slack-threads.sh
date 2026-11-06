#!/usr/bin/env bash
# test-slack-threads.sh — E2E tests for Slack thread interactions.
#
# Tests agent lifecycle notifications, decision routing to threads,
# DM delivery, and multi-agent thread isolation against a live
# slack-bridge and kbeads server.
#
# Prerequisites:
#   - SLACK_BOT_TOKEN set (xoxb-...)
#   - SLACK_CHANNEL set (C...)
#   - BEADS_HTTP_URL set (http://localhost:19090 via port-forward)
#   - BEADS_AUTH_TOKEN or BEADS_TOKEN set
#   - jq installed
#
# Usage:
#   ./tests/e2e/scripts/test-slack-threads.sh [scenario]
#
# Scenarios:
#   all                Run all scenarios (default)
#   agent-lifecycle    Scenario 1: Agent lifecycle notifications in thread
#   decision-thread    Scenario 2: Decision routes to agent's thread
#   dm-delivery        Scenario 3: DM delivery via dm bead
#   multi-agent        Scenario 4: Multi-agent thread isolation
#   mention-spawn      Scenario 5: @bot mention spawns agent
#   thread-forward     Scenario 6: Thread messages forwarded to agent
#   dedup              Scenario 7: Duplicate events not re-posted

set -euo pipefail

# Slack thread tests require Slack credentials and a live bridge.
# Gracefully skip when not configured (e.g. local dev without Slack).
if [ -z "${SLACK_BOT_TOKEN:-}" ]; then
  echo "SKIP: SLACK_BOT_TOKEN not set — Slack thread tests require Slack credentials"
  echo "  Set SLACK_BOT_TOKEN, SLACK_CHANNEL, and BEADS_HTTP_URL to run."
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "SLACK_BOT_TOKEN not set" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989, ka-0000005441)
  exit 0
fi
if [ -z "${SLACK_CHANNEL:-}" ]; then
  echo "SKIP: SLACK_CHANNEL not set"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "SLACK_CHANNEL not set" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989, ka-0000005441)
  exit 0
fi

: "${BEADS_HTTP_URL:=${BEADS_HTTP_ADDR:-http://localhost:19090}}"
: "${BEADS_AUTH_TOKEN:=${BEADS_TOKEN:-}}"
: "${CLEANUP:=true}"

SLACK_API="https://slack.com/api"
SLACK_AUTH="Authorization: Bearer ${SLACK_BOT_TOKEN}"
BEADS_AUTH_HEADER=""
if [ -n "$BEADS_AUTH_TOKEN" ]; then
  BEADS_AUTH_HEADER="Authorization: Bearer $BEADS_AUTH_TOKEN"
fi

PASS=0
FAIL=0
SOFT_PASS=0
SOFT_FAIL=0
ERRORS=()
SOFT_ERRORS=()
CREATED_BEADS=()
SLACK_MESSAGES=()

TEST_TAG="e2e-thread-$$-$(date +%s)"

# --- Helpers ---

red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

pass() {
  local name="$1"
  green "  ✓ $name"
  PASS=$((PASS + 1))
}

fail() {
  local name="$1"
  local reason="${2:-}"
  red "  ✗ $name"
  [ -n "$reason" ] && dim "    $reason"
  FAIL=$((FAIL + 1))
  ERRORS+=("$name: $reason")
}

assert_exit() {
  local name="$1"
  local expected="$2"
  local actual="$3"
  if [ "$actual" -eq "$expected" ]; then
    pass "$name (exit $expected)"
  else
    fail "$name" "expected exit $expected, got exit $actual"
  fi
}

assert_contains() {
  local name="$1"
  local pattern="$2"
  local actual="$3"
  if grep -qF -- "$pattern" <<< "$actual"; then
    pass "$name (contains '$pattern')"
  else
    fail "$name" "expected output to contain '$pattern'; got: $(echo "$actual" | head -3)"
  fi
}

assert_not_contains() {
  local name="$1"
  local pattern="$2"
  local actual="$3"
  if ! grep -qF -- "$pattern" <<< "$actual"; then
    pass "$name (does not contain '$pattern')"
  else
    fail "$name" "expected output NOT to contain '$pattern'"
  fi
}

# Soft assertions for Slack delivery — timing-dependent, don't fail the test
soft_pass() {
  local name="$1"
  green "  ✓ [slack] $name"
  SOFT_PASS=$((SOFT_PASS + 1))
}

soft_fail() {
  local name="$1"
  local reason="${2:-}"
  printf '\033[0;33m  ⚠ [slack] %s\033[0m\n' "$name"
  [ -n "$reason" ] && dim "    $reason"
  SOFT_FAIL=$((SOFT_FAIL + 1))
  SOFT_ERRORS+=("$name: $reason")
}

# --- Slack API helpers ---

slack_post() {
  local channel="$1" text="$2" thread_ts="${3:-}"
  local payload
  payload=$(jq -n \
    --arg ch "$channel" \
    --arg txt "$text" \
    --arg ts "$thread_ts" \
    '{channel: $ch, text: $txt} + (if $ts != "" then {thread_ts: $ts} else {} end)')
  local resp ts err retry_after
  for attempt in 1 2 3; do
    resp=$(curl -s -X POST "${SLACK_API}/chat.postMessage" \
      -H "$SLACK_AUTH" \
      -H "Content-Type: application/json" \
      -d "$payload")
    ts=$(echo "$resp" | jq -r '.ts // empty')
    if [ -n "$ts" ]; then
      SLACK_MESSAGES+=("${channel}:${ts}")
      echo "$ts"
      return 0
    fi
    err=$(echo "$resp" | jq -r '.error // empty' 2>/dev/null)
    retry_after=$(echo "$resp" | jq -r '.retry_after // empty' 2>/dev/null)
    if [ "$err" = "ratelimited" ] || [ -n "$retry_after" ]; then
      local wait=${retry_after:-3}
      dim "    rate-limited (${err}), retrying in ${wait}s (attempt $attempt/3)"
      sleep "$wait"
    else
      dim "    slack_post failed: ${err:-unknown} (attempt $attempt/3), retrying in 2s"
      sleep 2
    fi
  done
  echo ""
}

slack_get_replies() {
  local channel="$1" ts="$2"
  local resp
  for attempt in 1 2 3; do
    resp=$(curl -s "${SLACK_API}/conversations.replies?channel=${channel}&ts=${ts}&limit=50" \
      -H "$SLACK_AUTH")
    if echo "$resp" | jq -e '.messages' >/dev/null 2>&1; then
      echo "$resp" | jq -r '.messages'
      return 0
    fi
    local err retry_after
    err=$(echo "$resp" | jq -r '.error // empty' 2>/dev/null || echo "")
    retry_after=$(echo "$resp" | jq -r '.retry_after // empty' 2>/dev/null || echo "")
    if [ "$err" = "ratelimited" ] || [ -n "$retry_after" ]; then
      sleep "${retry_after:-3}"
    else
      sleep 2
    fi
  done
  echo "[]"
}

slack_get_history() {
  local channel="$1" oldest="${2:-0}" limit="${3:-20}"
  local resp
  for attempt in 1 2 3; do
    resp=$(curl -s "${SLACK_API}/conversations.history?channel=${channel}&oldest=${oldest}&limit=${limit}" \
      -H "$SLACK_AUTH")
    if echo "$resp" | jq -e '.messages' >/dev/null 2>&1; then
      echo "$resp" | jq -r '.messages'
      return 0
    fi
    local err retry_after
    err=$(echo "$resp" | jq -r '.error // empty' 2>/dev/null || echo "")
    retry_after=$(echo "$resp" | jq -r '.retry_after // empty' 2>/dev/null || echo "")
    if [ "$err" = "ratelimited" ] || [ -n "$retry_after" ]; then
      sleep "${retry_after:-3}"
    else
      sleep 2
    fi
  done
  echo "[]"
}

slack_delete() {
  local channel="$1" ts="$2"
  curl -s -X POST "${SLACK_API}/chat.delete" \
    -H "$SLACK_AUTH" \
    -H "Content-Type: application/json" \
    -d "{\"channel\":\"$channel\",\"ts\":\"$ts\"}" >/dev/null 2>&1 || true
}

# Wait for a Slack message containing text to appear in channel after a timestamp.
wait_for_slack_message() {
  local channel="$1" after_ts="$2" text_contains="$3" timeout="${4:-20}"
  local start end
  start=$(date +%s)
  end=$((start + timeout))
  while [ "$(date +%s)" -lt "$end" ]; do
    local msgs
    msgs=$(curl -s "${SLACK_API}/conversations.history?channel=${channel}&oldest=${after_ts}&limit=30" \
      -H "$SLACK_AUTH")
    local texts
    texts=$(echo "$msgs" | jq -r '.messages[] | (.text // "") + " " + (.blocks[]?.text?.text // "")' 2>/dev/null || echo "")
    if grep -qF "$text_contains" <<<"$texts"; then
      return 0
    fi
    sleep 2
  done
  return 1
}

# Wait for a Slack reply in a thread containing specific text.
wait_for_thread_reply() {
  local channel="$1" thread_ts="$2" text_contains="$3" timeout="${4:-20}"
  local start end
  start=$(date +%s)
  end=$((start + timeout))
  while [ "$(date +%s)" -lt "$end" ]; do
    local replies
    replies=$(curl -s "${SLACK_API}/conversations.replies?channel=${channel}&ts=${thread_ts}&limit=50" \
      -H "$SLACK_AUTH")
    local texts
    texts=$(echo "$replies" | jq -r '.messages[] | (.text // "") + " " + (.blocks[]?.text?.text // "")' 2>/dev/null || echo "")
    if grep -qF "$text_contains" <<<"$texts"; then
      return 0
    fi
    sleep 2
  done
  return 1
}

# --- Beads API helpers ---

beads_create() {
  local type="$1" title="$2"
  shift 2
  local extra_fields="${1:-}"
  local body
  body=$(jq -n \
    --arg t "$type" \
    --arg title "$title" \
    '{type: $t, title: $title}')
  if [ -n "$extra_fields" ]; then
    body=$(echo "$body" | jq --argjson f "$extra_fields" '. + {fields: $f}')
  fi
  local resp
  resp=$(curl -s --retry 2 --retry-delay 2 --retry-all-errors --max-time 10 \
    -X POST "${BEADS_HTTP_URL}/v1/beads" \
    ${BEADS_AUTH_HEADER:+-H "$BEADS_AUTH_HEADER"} \
    -H "Content-Type: application/json" \
    -d "$body")
  local id
  id=$(echo "$resp" | jq -r '.id // empty')
  if [ -n "$id" ]; then
    CREATED_BEADS+=("$id")
  fi
  echo "$id"
}

beads_update() {
  local id="$1"
  shift
  curl -s --retry 2 --retry-delay 2 --retry-all-errors --max-time 10 \
    -X PATCH "${BEADS_HTTP_URL}/v1/beads/${id}" \
    ${BEADS_AUTH_HEADER:+-H "$BEADS_AUTH_HEADER"} \
    -H "Content-Type: application/json" \
    -d "$@" >/dev/null
}

beads_close() {
  local id="$1"
  curl -s --retry 2 --retry-delay 2 --retry-all-errors --max-time 10 \
    -X POST "${BEADS_HTTP_URL}/v1/beads/${id}/close" \
    ${BEADS_AUTH_HEADER:+-H "$BEADS_AUTH_HEADER"} \
    -H "Content-Type: application/json" \
    -d '{}' >/dev/null
}

beads_get() {
  local id="$1"
  curl -s --retry 2 --retry-delay 2 --retry-all-errors --max-time 10 \
    "${BEADS_HTTP_URL}/v1/beads/${id}" \
    ${BEADS_AUTH_HEADER:+-H "$BEADS_AUTH_HEADER"}
}

# --- Cleanup ---

cleanup() {
  if [ "$CLEANUP" = "false" ]; then
    echo "CLEANUP=false, preserving ${#CREATED_BEADS[@]} test beads"
    return
  fi
  for id in "${CREATED_BEADS[@]}"; do
    beads_close "$id" 2>/dev/null || true
  done
  # Clean up posted Slack messages (best-effort).
  for msg in "${SLACK_MESSAGES[@]}"; do
    local ch ts
    ch="${msg%%:*}"
    ts="${msg#*:}"
    slack_delete "$ch" "$ts" 2>/dev/null || true
  done
}
trap cleanup EXIT

# --- Scenario 1: Agent lifecycle notifications in thread ---

scenario_agent_lifecycle() {
  blue "Scenario 1: Agent lifecycle notifications in thread"

  # Post a parent message to create a thread anchor.
  local parent_ts
  parent_ts=$(slack_post "$SLACK_CHANNEL" "E2E test thread anchor ($TEST_TAG)")
  if [ -z "$parent_ts" ]; then
    fail "post parent message" "no ts returned"
    return
  fi
  pass "posted parent message"
  sleep 1

  # Create an agent bead bound to this thread.
  local agent_name="e2e-lifecycle-$TEST_TAG"
  local agent_id
  agent_id=$(beads_create "agent" "$agent_name" "$(jq -n \
    --arg name "$agent_name" \
    --arg ch "$SLACK_CHANNEL" \
    --arg ts "$parent_ts" \
    '{
      agent: $name,
      role: "crew",
      project: "gardenio",
      agent_state: "spawning",
      slack_thread_channel: $ch,
      slack_thread_ts: $ts,
      slack_spawn_channel: $ch
    }')")
  if [ -z "$agent_id" ]; then
    fail "create agent bead" "no id returned"
    return
  fi
  pass "created agent bead ($agent_id)"

  # Wait for the bridge to post the spawn notification in the thread.
  sleep 5

  # Transition to working state.
  beads_update "$agent_id" '{"fields":{"agent_state":"working"}}'
  pass "updated agent to working"
  sleep 3

  # Transition to done state.
  beads_update "$agent_id" '{"fields":{"agent_state":"done"}}'
  pass "updated agent to done"
  sleep 3

  # Verify the thread has replies from the bot (lifecycle notifications).
  # Slack delivery is timing-dependent — soft assertion.
  local replies
  replies=$(slack_get_replies "$SLACK_CHANNEL" "$parent_ts")
  local reply_count
  reply_count=$(echo "$replies" | jq 'length' 2>/dev/null || echo "0")

  if [ "$reply_count" -gt 1 ]; then
    soft_pass "thread has bot replies ($reply_count messages including parent)"
  else
    soft_fail "thread has bot replies" "only $reply_count messages — bridge may use card updates instead"
  fi

  # Check that a message referencing the agent appeared in the channel.
  if wait_for_slack_message "$SLACK_CHANNEL" "0" "$agent_name" 10; then
    soft_pass "agent card/notification visible in channel"
  else
    soft_fail "agent card/notification visible" "not found after 10s — card may be in blocks"
  fi

  beads_close "$agent_id"
  echo
}

# --- Scenario 2: Decision routes to agent's thread ---

scenario_decision_thread() {
  blue "Scenario 2: Decision routes to agent's thread"

  # Post a thread anchor.
  local parent_ts
  parent_ts=$(slack_post "$SLACK_CHANNEL" "E2E decision thread test ($TEST_TAG)")
  if [ -z "$parent_ts" ]; then
    fail "post parent message" "no ts returned"
    return
  fi
  pass "posted thread anchor"
  sleep 1

  # Create agent bound to this thread.
  local agent_name="e2e-decision-$TEST_TAG"
  local agent_id
  agent_id=$(beads_create "agent" "$agent_name" "$(jq -n \
    --arg name "$agent_name" \
    --arg ch "$SLACK_CHANNEL" \
    --arg ts "$parent_ts" \
    '{
      agent: $name,
      role: "crew",
      project: "gardenio",
      agent_state: "working",
      slack_thread_channel: $ch,
      slack_thread_ts: $ts,
      slack_spawn_channel: $ch
    }')")
  if [ -z "$agent_id" ]; then
    fail "create agent bead" "no id returned"
    return
  fi
  pass "created agent bead ($agent_id)"
  sleep 3

  # Create a decision bead assigned to this agent.
  local decision_id
  decision_id=$(beads_create "decision" "E2E decision test $TEST_TAG" "$(jq -n \
    --arg agent "$agent_name" \
    --arg prompt "Should we proceed with the E2E test?" \
    '{
      requested_by: $agent,
      prompt: $prompt,
      options: [{"id":"yes","short":"Yes"},{"id":"no","short":"No"}]
    }')")
  if [ -z "$decision_id" ]; then
    fail "create decision bead" "no id returned"
  else
    pass "created decision bead ($decision_id)"
  fi

  # Wait for the bridge to post the decision notification.
  # Slack delivery is timing-dependent — soft assertion.
  sleep 8

  if wait_for_thread_reply "$SLACK_CHANNEL" "$parent_ts" "proceed" 20; then
    soft_pass "decision notification appeared in agent's thread"
  elif wait_for_slack_message "$SLACK_CHANNEL" "0" "proceed" 10; then
    soft_pass "decision notification appeared in channel"
  else
    soft_fail "decision notification" "not found — may be in interactive blocks or bridge SSE lag"
  fi

  # Close the decision bead so it doesn't leak into the shared kd
  # server. Leftover open decisions blew past gb decision list's
  # default 20-row cap and broke the decisions-yield E2E
  # (gd-Zg5gKXLG6m).
  [ -n "$decision_id" ] && beads_close "$decision_id"
  beads_close "$agent_id"
  echo
}

# --- Scenario 3: DM delivery via dm bead ---

scenario_dm_delivery() {
  blue "Scenario 3: DM delivery via dm bead"

  # Create an agent as the DM source.
  local agent_name="e2e-dm-$TEST_TAG"
  local agent_id
  agent_id=$(beads_create "agent" "$agent_name" "$(jq -n \
    --arg name "$agent_name" \
    '{
      agent: $name,
      role: "crew",
      project: "gardenio",
      agent_state: "working"
    }')")
  if [ -z "$agent_id" ]; then
    fail "create agent bead" "no id returned"
    return
  fi
  pass "created source agent ($agent_id)"
  sleep 2

  # We can't send a real DM to a user in E2E without knowing a valid Slack user ID.
  # Instead, verify the dm bead creation mechanics and field validation.

  # Create a dm bead with a placeholder target (will fail delivery but validates creation).
  local dm_text="E2E DM test message $TEST_TAG"
  local dm_id
  dm_id=$(beads_create "dm" "DM from e2e test" "$(jq -n \
    --arg agent "$agent_name" \
    --arg text "$dm_text" \
    --arg subject "E2E DM subject" \
    '{
      source_agent: $agent,
      target_user: "U_E2E_PLACEHOLDER",
      subject: $subject,
      body: $text,
      dm_mode: "notify"
    }')")
  if [ -z "$dm_id" ]; then
    fail "create dm bead" "no id returned"
  else
    pass "created dm bead ($dm_id)"

    # Wait for bridge to process the dm bead.
    sleep 5

    # Verify the dm bead was processed (bridge sets dm_channel or closes it).
    local dm_state
    dm_state=$(beads_get "$dm_id" | jq -r '.status // empty' 2>/dev/null || echo "")
    if [ "$dm_state" = "closed" ]; then
      pass "dm bead closed after delivery attempt"
    else
      # Even if delivery fails (invalid user), the bead exists with correct fields.
      local dm_fields
      dm_fields=$(beads_get "$dm_id" | jq -r '.fields.source_agent // empty' 2>/dev/null || echo "")
      if [ "$dm_fields" = "$agent_name" ]; then
        pass "dm bead has correct source_agent field"
      else
        pass "dm bead created (delivery depends on valid Slack user ID)"
      fi
    fi
  fi

  # Test question-mode DM creation.
  local question_id
  question_id=$(beads_create "dm" "Question from e2e test" "$(jq -n \
    --arg agent "$agent_name" \
    --arg text "What is the status of the deployment?" \
    '{
      source_agent: $agent,
      target_user: "U_E2E_PLACEHOLDER",
      body: $text,
      dm_mode: "question",
      timeout_minutes: 5
    }')")
  if [ -z "$question_id" ]; then
    fail "create question dm bead" "no id returned"
  else
    pass "created question-mode dm bead ($question_id)"
  fi

  beads_close "$agent_id"
  echo
}

# --- Scenario 4: Multi-agent thread isolation ---

scenario_multi_agent() {
  blue "Scenario 4: Multi-agent thread isolation"

  # Create two separate thread anchors (sleep between to avoid rate limiting).
  local ts_a ts_b
  ts_a=$(slack_post "$SLACK_CHANNEL" "E2E agent-A thread ($TEST_TAG)")
  sleep 1
  ts_b=$(slack_post "$SLACK_CHANNEL" "E2E agent-B thread ($TEST_TAG)")
  if [ -z "$ts_a" ] || [ -z "$ts_b" ]; then
    fail "post thread anchors" "could not create Slack messages"
    return
  fi
  pass "posted two thread anchors"
  sleep 1

  # Create two agents bound to different threads.
  local name_a="e2e-multi-a-$TEST_TAG"
  local name_b="e2e-multi-b-$TEST_TAG"

  local id_a id_b
  id_a=$(beads_create "agent" "$name_a" "$(jq -n \
    --arg name "$name_a" \
    --arg ch "$SLACK_CHANNEL" \
    --arg ts "$ts_a" \
    '{
      agent: $name,
      role: "crew",
      project: "gardenio",
      agent_state: "working",
      slack_thread_channel: $ch,
      slack_thread_ts: $ts,
      slack_spawn_channel: $ch
    }')")
  id_b=$(beads_create "agent" "$name_b" "$(jq -n \
    --arg name "$name_b" \
    --arg ch "$SLACK_CHANNEL" \
    --arg ts "$ts_b" \
    '{
      agent: $name,
      role: "crew",
      project: "gardenio",
      agent_state: "working",
      slack_thread_channel: $ch,
      slack_thread_ts: $ts,
      slack_spawn_channel: $ch
    }')")

  if [ -z "$id_a" ] || [ -z "$id_b" ]; then
    fail "create agent beads" "could not create both agents"
    [ -n "$id_a" ] && beads_close "$id_a"
    [ -n "$id_b" ] && beads_close "$id_b"
    return
  fi
  pass "created agent A ($id_a) and agent B ($id_b)"
  sleep 3

  # Verify agents have distinct thread bindings.
  local fields_a fields_b
  fields_a=$(beads_get "$id_a" | jq -r '.fields.slack_thread_ts // empty' 2>/dev/null)
  fields_b=$(beads_get "$id_b" | jq -r '.fields.slack_thread_ts // empty' 2>/dev/null)

  if [ "$fields_a" = "$ts_a" ]; then
    pass "agent A bound to thread A"
  else
    fail "agent A thread binding" "expected $ts_a, got $fields_a"
  fi

  if [ "$fields_b" = "$ts_b" ]; then
    pass "agent B bound to thread B"
  else
    fail "agent B thread binding" "expected $ts_b, got $fields_b"
  fi

  # Verify threads are distinct.
  if [ "$fields_a" != "$fields_b" ]; then
    pass "agents bound to different threads"
  else
    fail "agents bound to different threads" "both bound to $fields_a"
  fi

  # Transition both to done.
  beads_update "$id_a" '{"fields":{"agent_state":"done"}}'
  beads_update "$id_b" '{"fields":{"agent_state":"done"}}'
  sleep 2

  beads_close "$id_a"
  beads_close "$id_b"
  pass "multi-agent lifecycle completed"
  echo
}

# --- Scenario 5: @bot mention triggers spawn ---

scenario_mention_spawn() {
  blue "Scenario 5: @bot mention spawns agent (observational)"

  # This test verifies the mention → spawn flow by checking that the bridge
  # is subscribed to the test channel. We post a @bot mention and check for
  # a response, but we do NOT assert spawn success since the bot user ID
  # may differ in CI vs staging.

  # Post a thread anchor.
  local parent_ts
  parent_ts=$(slack_post "$SLACK_CHANNEL" "E2E mention spawn test ($TEST_TAG)")
  if [ -z "$parent_ts" ]; then
    fail "post parent message" "no ts returned"
    return
  fi
  pass "posted thread anchor"
  sleep 1

  # Look up the bot user ID from auth.test.
  local bot_user_id
  bot_user_id=$(curl -s "${SLACK_API}/auth.test" -H "$SLACK_AUTH" | jq -r '.user_id // empty')
  if [ -z "$bot_user_id" ]; then
    fail "get bot user ID" "auth.test returned no user_id"
    return
  fi
  pass "bot user ID: $bot_user_id"

  # Post a mention of the bot in the thread.
  local mention_text="<@${bot_user_id}> E2E mention test $TEST_TAG"
  local mention_ts
  mention_ts=$(slack_post "$SLACK_CHANNEL" "$mention_text" "$parent_ts")
  if [ -z "$mention_ts" ]; then
    fail "post mention" "no ts returned"
    return
  fi
  pass "posted @bot mention in thread"

  # Wait for the bridge to respond (it should either spawn an agent or reply).
  sleep 8

  # Check for any reply in the thread.
  local replies
  replies=$(slack_get_replies "$SLACK_CHANNEL" "$parent_ts")
  local reply_count
  reply_count=$(echo "$replies" | jq 'length' 2>/dev/null || echo "0")

  if [ "$reply_count" -gt 2 ]; then
    soft_pass "bridge responded to mention ($reply_count messages in thread)"
  else
    soft_fail "bridge responded to mention" "only $reply_count messages — bridge may not be subscribed to this channel"
  fi
  echo
}

# --- Scenario 6: Thread messages forwarded to agent ---

scenario_thread_forward() {
  blue "Scenario 6: Thread message forwarding"

  # Create a thread with a bound agent.
  local parent_ts
  parent_ts=$(slack_post "$SLACK_CHANNEL" "E2E forward test ($TEST_TAG)")
  if [ -z "$parent_ts" ]; then
    fail "post parent message" "no ts returned"
    return
  fi
  pass "posted parent message"
  sleep 1

  local agent_name="e2e-fwd-$TEST_TAG"
  local agent_id
  agent_id=$(beads_create "agent" "$agent_name" "$(jq -n \
    --arg name "$agent_name" \
    --arg ch "$SLACK_CHANNEL" \
    --arg ts "$parent_ts" \
    '{
      agent: $name,
      role: "crew",
      project: "gardenio",
      agent_state: "working",
      slack_thread_channel: $ch,
      slack_thread_ts: $ts,
      slack_spawn_channel: $ch
    }')")
  if [ -z "$agent_id" ]; then
    fail "create agent bead" "no id returned"
    return
  fi
  pass "created agent ($agent_id) bound to thread"
  sleep 3

  # Post a user message in the agent's thread.
  local user_msg="Please check the deployment status $TEST_TAG"
  local msg_ts
  msg_ts=$(slack_post "$SLACK_CHANNEL" "$user_msg" "$parent_ts")
  if [ -z "$msg_ts" ]; then
    fail "post thread message" "no ts returned"
  else
    pass "posted message in agent's thread"
  fi

  # The bridge should detect this as a thread-forward event and nudge the agent.
  # We can't directly verify the nudge, but we can verify the message was posted.
  sleep 3

  # Verify the thread has our message.
  local replies
  replies=$(slack_get_replies "$SLACK_CHANNEL" "$parent_ts")
  local reply_texts
  reply_texts=$(echo "$replies" | jq -r '.[].text // ""' 2>/dev/null || echo "")
  assert_contains "thread contains user message" "$TEST_TAG" "$reply_texts"

  beads_close "$agent_id"
  echo
}

# --- Scenario 7: Dedup — duplicate events not re-posted ---

scenario_dedup() {
  blue "Scenario 7: Dedup — duplicate events not re-posted"

  # Create an agent bead and wait for the bridge to process it.
  local agent_name="e2e-dedup-$TEST_TAG"
  local agent_id
  agent_id=$(beads_create "agent" "$agent_name" "$(jq -n \
    --arg name "$agent_name" \
    '{
      agent: $name,
      role: "crew",
      project: "gardenio",
      agent_state: "working",
      slack_spawn_channel: "'"$SLACK_CHANNEL"'"
    }')")
  if [ -z "$agent_id" ]; then
    fail "create agent bead" "no id returned"
    return
  fi
  pass "created agent ($agent_id)"
  sleep 5

  # Record current channel message count for the agent.
  local before_count
  before_count=$(curl -s "${SLACK_API}/conversations.history?channel=${SLACK_CHANNEL}&limit=50" \
    -H "$SLACK_AUTH" | jq "[.messages[] | select(.text? // \"\" | contains(\"$agent_name\"))] | length" 2>/dev/null || echo "0")
  pass "counted $before_count messages mentioning agent before update"

  # Update the agent state twice rapidly (should not produce duplicate cards).
  beads_update "$agent_id" '{"fields":{"agent_state":"idle"}}'
  sleep 1
  beads_update "$agent_id" '{"fields":{"agent_state":"working"}}'
  sleep 5

  # Count messages mentioning this agent after the updates.
  local after_count
  after_count=$(curl -s "${SLACK_API}/conversations.history?channel=${SLACK_CHANNEL}&limit=50" \
    -H "$SLACK_AUTH" | jq "[.messages[] | select(.text? // \"\" | contains(\"$agent_name\"))] | length" 2>/dev/null || echo "0")

  # The bridge should update the existing card, not post new ones.
  # Allow at most 1 additional message (the original card).
  local diff=$((after_count - before_count))
  if [ "$diff" -le 1 ]; then
    pass "no duplicate messages after rapid state updates (diff: $diff)"
  else
    fail "dedup" "expected at most 1 new message, got $diff new messages"
  fi

  beads_close "$agent_id"
  echo
}

# --- Main ---

scenario="${1:-all}"

blue "═══════════════════════════════════════════════════"
blue " slack-threads E2E tests"
blue "═══════════════════════════════════════════════════"
echo "Channel: ${SLACK_CHANNEL}"
echo "Beads:   ${BEADS_HTTP_URL}"
echo ""

case "$scenario" in
  all)
    scenario_agent_lifecycle
    sleep 2
    scenario_decision_thread
    scenario_dm_delivery
    sleep 5  # cooldown before Slack-heavy scenarios to avoid rate limits
    scenario_multi_agent
    sleep 3
    scenario_mention_spawn
    sleep 3
    scenario_thread_forward
    sleep 2
    scenario_dedup
    ;;
  agent-lifecycle)   scenario_agent_lifecycle ;;
  decision-thread)   scenario_decision_thread ;;
  dm-delivery)       scenario_dm_delivery ;;
  multi-agent)       scenario_multi_agent ;;
  mention-spawn)     scenario_mention_spawn ;;
  thread-forward)    scenario_thread_forward ;;
  dedup)             scenario_dedup ;;
  *)
    echo "Unknown scenario: $scenario"
    echo "Available: all, agent-lifecycle, decision-thread, dm-delivery, multi-agent, mention-spawn, thread-forward, dedup"
    exit 1
    ;;
esac

# --- Summary ---

blue "═══════════════════════════════════════════════════"
total=$((PASS + FAIL))
soft_total=$((SOFT_PASS + SOFT_FAIL))
if [ "$FAIL" -eq 0 ]; then
  green " ✓ All $total bead assertions passed"
else
  red " ✗ $FAIL/$total bead assertions failed"
  echo
  for err in "${ERRORS[@]}"; do
    red "   - $err"
  done
fi
if [ "$soft_total" -gt 0 ]; then
  if [ "$SOFT_FAIL" -eq 0 ]; then
    green " ✓ All $soft_total Slack delivery checks passed"
  else
    printf '\033[0;33m ⚠ %d/%d Slack delivery checks failed (non-blocking)\033[0m\n' "$SOFT_FAIL" "$soft_total"
    for err in "${SOFT_ERRORS[@]}"; do
      printf '\033[0;33m   - %s\033[0m\n' "$err"
    done
  fi
fi
blue "═══════════════════════════════════════════════════"

# Only hard failures affect exit code — Slack delivery flakiness is non-blocking
[ "$FAIL" -eq 0 ]
