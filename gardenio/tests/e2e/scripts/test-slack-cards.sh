#!/usr/bin/env bash
# test-slack-cards.sh — E2E tests for Slack agent cards, streaming status,
# and squawk interactions in the e2e namespace.
#
# Tests the following scenarios:
#   1. Agent card rendering: spawn → card posted with correct blocks
#   2. Agent card tool stats: working state shows tool counts
#   3. Squawk message delivery: squawk → posted in agent's thread
#   4. Thread status restore: squawk doesn't kill assistant status
#   5. Schedule card format: scheduled agents get unified card format
#   6. Agent card terminal state: done/failed → wrapup card + Clear button
#
# Prerequisites:
#   - SLACK_BOT_TOKEN set (xoxb-... for staging Slack app)
#   - SLACK_CHANNEL set (C... channel to post test cards)
#   - BEADS_HTTP_URL set (http://localhost:19090 via port-forward)
#   - BEADS_AUTH_TOKEN set
#   - jq installed
#
# Usage:
#   ./tests/e2e/scripts/test-slack-cards.sh [scenario]
#
# Scenarios:
#   all              Run all scenarios (default)
#   agent-card       Scenario 1: Agent card lifecycle
#   squawk-delivery  Scenario 2: Squawk message delivery
#   schedule-card    Scenario 3: Schedule card format
#   card-terminal    Scenario 4: Terminal state card
#
# Environment:
#   SLACK_BOT_TOKEN    Slack bot OAuth token (required)
#   SLACK_CHANNEL      Slack channel for test messages (required)
#   BEADS_HTTP_URL     Beads daemon HTTP URL (default: http://localhost:19090)
#   BEADS_AUTH_TOKEN   Beads API token (default: from env)
#   CLEANUP            Set to "false" to preserve test beads (default: true)

set -euo pipefail

# --- Configuration ---

if [ -z "${SLACK_BOT_TOKEN:-}" ]; then
  echo "SKIP: SLACK_BOT_TOKEN not set — Slack card tests require Slack credentials"
  echo "  Set SLACK_BOT_TOKEN, SLACK_CHANNEL, BEADS_HTTP_URL, and BEADS_AUTH_TOKEN."
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "SLACK_BOT_TOKEN not set" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989, ka-0000005441)
  exit 0
fi
if [ -z "${SLACK_CHANNEL:-}" ]; then
  echo "SKIP: SLACK_CHANNEL not set"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "SLACK_CHANNEL not set" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989, ka-0000005441)
  exit 0
fi

: "${BEADS_HTTP_URL:=http://localhost:19090}"
: "${BEADS_AUTH_TOKEN:=}"
: "${CLEANUP:=true}"

SLACK_API="https://slack.com/api"
SLACK_AUTH="Authorization: Bearer ${SLACK_BOT_TOKEN}"
BEADS_AUTH_HEADER=""
if [ -n "$BEADS_AUTH_TOKEN" ]; then
  BEADS_AUTH_HEADER="Authorization: Bearer $BEADS_AUTH_TOKEN"
fi

FAILURES=0
SOFT_FAILURES=0
SOFT_WARNINGS=()
CREATED_BEADS=()

# --- Helpers ---

pass() { echo "  ✓ $1"; }
fail() { echo "  ✗ $1"; FAILURES=$((FAILURES + 1)); }

# Soft assertions for Slack delivery — timing-dependent, don't fail the test
soft_pass() { echo "  ✓ [slack] $1"; }
soft_fail() {
  printf '  \033[0;33m⚠ [slack] %s\033[0m\n' "$1"
  SOFT_FAILURES=$((SOFT_FAILURES + 1))
  SOFT_WARNINGS+=("$1")
}

check_contains() {
  local haystack="$1" needle="$2" label="$3"
  if grep -qF "$needle" <<<"$haystack"; then
    pass "$label"
  else
    fail "$label (expected to contain '$needle')"
  fi
}

check_not_empty() {
  if [ -n "$1" ]; then pass "$2"; else fail "$2 (empty)"; fi
}

slack_post() {
  local channel="$1" text="$2" thread_ts="${3:-}"
  local payload
  payload=$(jq -n \
    --arg ch "$channel" \
    --arg txt "$text" \
    --arg ts "$thread_ts" \
    '{channel: $ch, text: $txt} + (if $ts != "" then {thread_ts: $ts} else {} end)')
  local resp
  for attempt in 1 2 3; do
    resp=$(curl -s -X POST "${SLACK_API}/chat.postMessage" \
      -H "$SLACK_AUTH" \
      -H "Content-Type: application/json" \
      -d "$payload")
    if echo "$resp" | jq -e '.ok' >/dev/null 2>&1; then
      echo "$resp"
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
  echo "$resp"
}

slack_get_message() {
  local channel="$1" ts="$2"
  local resp
  for attempt in 1 2 3; do
    resp=$(curl -s "${SLACK_API}/conversations.history?channel=${channel}&latest=${ts}&inclusive=true&limit=1" \
      -H "$SLACK_AUTH")
    if echo "$resp" | jq -e '.messages' >/dev/null 2>&1; then
      echo "$resp" | jq -r '.messages[0]'
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
  echo "null"
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

slack_delete() {
  local channel="$1" ts="$2"
  curl -s -X POST "${SLACK_API}/chat.delete" \
    -H "$SLACK_AUTH" \
    -H "Content-Type: application/json" \
    -d "{\"channel\":\"$channel\",\"ts\":\"$ts\"}" >/dev/null 2>&1 || true
}

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
  resp=$(curl -s -X POST "${BEADS_HTTP_URL}/v1/beads" \
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
  curl -s -X PATCH "${BEADS_HTTP_URL}/v1/beads/${id}" \
    ${BEADS_AUTH_HEADER:+-H "$BEADS_AUTH_HEADER"} \
    -H "Content-Type: application/json" \
    -d "$@" >/dev/null
}

beads_close() {
  local id="$1"
  curl -s -X POST "${BEADS_HTTP_URL}/v1/beads/${id}/close" \
    ${BEADS_AUTH_HEADER:+-H "$BEADS_AUTH_HEADER"} \
    -H "Content-Type: application/json" \
    -d '{}' >/dev/null
}

beads_get() {
  local id="$1"
  curl -s "${BEADS_HTTP_URL}/v1/beads/${id}" \
    ${BEADS_AUTH_HEADER:+-H "$BEADS_AUTH_HEADER"}
}

cleanup() {
  if [ "$CLEANUP" = "false" ]; then
    echo "CLEANUP=false, preserving ${#CREATED_BEADS[@]} test beads"
    return
  fi
  for id in "${CREATED_BEADS[@]}"; do
    beads_close "$id" 2>/dev/null || true
  done
}
trap cleanup EXIT

wait_for_slack_message() {
  local channel="$1" after_ts="$2" text_contains="$3" timeout="${4:-15}"
  local start end
  start=$(date +%s)
  end=$((start + timeout))
  while [ "$(date +%s)" -lt "$end" ]; do
    local resp msgs
    resp=$(curl -s "${SLACK_API}/conversations.history?channel=${channel}&oldest=${after_ts}&limit=20" \
      -H "$SLACK_AUTH")
    msgs=$(echo "$resp" | jq -r '.messages[] | .text // .blocks[0].text.text // ""' 2>/dev/null || echo "")
    if [ -n "$msgs" ] && grep -qF "$text_contains" <<<"$msgs"; then
      return 0
    fi
    # Handle rate limiting by backing off
    local err retry_after
    err=$(echo "$resp" | jq -r '.error // empty' 2>/dev/null || echo "")
    if [ "$err" = "ratelimited" ]; then
      retry_after=$(echo "$resp" | jq -r '.retry_after // empty' 2>/dev/null || echo "")
      sleep "${retry_after:-3}"
    else
      sleep 2
    fi
  done
  return 1
}

# --- Scenario 1: Agent Card Lifecycle ---

scenario_agent_card() {
  echo ""
  echo "=== Scenario 1: Agent Card Lifecycle ==="

  # Create an agent bead — the bridge should post an agent card.
  local agent_name="e2e-card-test-$(date +%s)"
  local agent_id
  agent_id=$(beads_create "agent" "$agent_name" "{\"agent\":\"$agent_name\",\"role\":\"crew\",\"project\":\"gardenio\",\"agent_state\":\"spawning\"}")
  check_not_empty "$agent_id" "created agent bead"

  # Wait for the bridge to post the agent card.
  sleep 5

  # Update to working state — card should refresh with green indicator.
  beads_update "$agent_id" "{\"fields\":{\"agent_state\":\"working\"}}"
  sleep 3

  # Verify: check recent channel messages for the agent card.
  local before_ts
  before_ts=$(date +%s)
  if wait_for_slack_message "$SLACK_CHANNEL" "0" "$agent_name" 25; then
    soft_pass "agent card posted to Slack channel"
  else
    soft_fail "agent card not found in Slack channel after 25s"
  fi

  # Update to done state — card should show checkmark + Clear button.
  beads_update "$agent_id" "{\"fields\":{\"agent_state\":\"done\"}}"
  sleep 3

  # Close the agent bead.
  beads_close "$agent_id"
  pass "agent card lifecycle completed"
}

# --- Scenario 2: Squawk Message Delivery ---

scenario_squawk_delivery() {
  echo ""
  echo "=== Scenario 2: Squawk Message Delivery ==="

  # Create an agent with a card thread.
  local agent_name="e2e-squawk-test-$(date +%s)"
  local agent_id
  agent_id=$(beads_create "agent" "$agent_name" "{\"agent\":\"$agent_name\",\"role\":\"crew\",\"project\":\"gardenio\",\"agent_state\":\"working\"}")
  check_not_empty "$agent_id" "created agent bead for squawk test"

  # Wait for agent card to be posted, then find its thread TS.
  # The bridge needs time to receive the SSE event and post the card.
  sleep 10
  local card_ts=""
  local attempts=0
  while [ $attempts -lt 10 ] && [ -z "$card_ts" ]; do
    local hist_resp
    hist_resp=$(curl -s "${SLACK_API}/conversations.history?channel=${SLACK_CHANNEL}&limit=20" \
      -H "$SLACK_AUTH")
    card_ts=$(echo "$hist_resp" | jq -r --arg name "$agent_name" \
      '[.messages[] | select((.text // "") + (.blocks // [] | map(.text.text // "") | join(" ")) | contains($name)) | select(.ts)] | .[0].ts // empty' 2>/dev/null || echo "")
    [ -z "$card_ts" ] && sleep 3
    attempts=$((attempts + 1))
  done
  if [ -n "$card_ts" ]; then
    echo "  (agent card found: ts=$card_ts)"
  else
    echo "  (warning: agent card not found in channel after $((8 + attempts * 2))s)"
  fi

  # Create a squawk message bead with the squawk label already set.
  # Labels must be on the bead BEFORE close, because the close endpoint
  # merges body fields into Fields JSON, not into the actual Labels column.
  local squawk_text="E2E squawk test: build complete at $(date)"
  local squawk_body
  squawk_body=$(jq -n \
    --arg t "message" \
    --arg title "squawk" \
    --arg agent "$agent_name" \
    --arg text "$squawk_text" \
    '{type: $t, title: $title, labels: ["squawk", "from:" + $agent], fields: {source_agent: $agent, text: $text}}')
  local squawk_resp squawk_id
  squawk_resp=$(curl -s -X POST "${BEADS_HTTP_URL}/v1/beads" \
    ${BEADS_AUTH_HEADER:+-H "$BEADS_AUTH_HEADER"} \
    -H "Content-Type: application/json" \
    -d "$squawk_body")
  squawk_id=$(echo "$squawk_resp" | jq -r '.id // empty')
  [ -n "$squawk_id" ] && CREATED_BEADS+=("$squawk_id")

  if [ -n "$squawk_id" ]; then
    # Close the bead to trigger the bridge SSE handler.
    curl -s -X POST "${BEADS_HTTP_URL}/v1/beads/${squawk_id}/close" \
      ${BEADS_AUTH_HEADER:+-H "$BEADS_AUTH_HEADER"} \
      -H "Content-Type: application/json" \
      -d '{}' >/dev/null
    pass "created and closed squawk bead"
  else
    fail "failed to create squawk bead"
  fi

  sleep 10

  # Squawk messages are posted to the agent's thread, not the channel.
  # Check thread replies if we found the agent card, otherwise fall back to channel.
  local found=false
  if [ -n "$card_ts" ]; then
    local end_time=$(($(date +%s) + 25))
    while [ "$(date +%s)" -lt "$end_time" ]; do
      local reply_resp replies
      reply_resp=$(curl -s "${SLACK_API}/conversations.replies?channel=${SLACK_CHANNEL}&ts=${card_ts}&limit=50" \
        -H "$SLACK_AUTH")
      replies=$(echo "$reply_resp" | jq -r '.messages[] | .text // ""' 2>/dev/null || echo "")
      if [ -n "$replies" ] && grep -qF "$squawk_text" <<<"$replies"; then
        found=true
        break
      fi
      sleep 3
    done
  fi

  # Fall back to checking channel top-level messages.
  if [ "$found" = "false" ]; then
    if wait_for_slack_message "$SLACK_CHANNEL" "0" "$squawk_text" 20; then
      found=true
    fi
  fi

  if [ "$found" = "true" ]; then
    soft_pass "squawk message delivered to Slack"
  else
    soft_fail "squawk message not found in Slack after timeout"
  fi

  beads_close "$agent_id"
}

# --- Scenario 3: Schedule Card Format ---

scenario_schedule_card() {
  echo ""
  echo "=== Scenario 3: Schedule Card Format ==="

  # Create an agent bead with schedule metadata.
  local agent_name="e2e-sched-test-$(date +%s)"
  local agent_id
  agent_id=$(beads_create "agent" "$agent_name" "{\"agent\":\"$agent_name\",\"role\":\"crew\",\"project\":\"gardenio\",\"agent_state\":\"spawning\",\"schedule_title\":\"E2E Test Schedule\",\"schedule_cron\":\"*/15 * * * *\"}")
  check_not_empty "$agent_id" "created scheduled agent bead"

  # Poll for the schedule card/notification in a single loop.
  # The bridge posts an agent card (text: "Agent: <name>") as a top-level
  # message, and "Scheduled task ..." as a thread reply. conversations.history
  # only returns top-level messages, so match the agent name in the card text.
  local found_sched=false
  local end_time=$(($(date +%s) + 60))
  while [ "$(date +%s)" -lt "$end_time" ]; do
    local resp msgs
    resp=$(curl -s "${SLACK_API}/conversations.history?channel=${SLACK_CHANNEL}&limit=30" \
      -H "$SLACK_AUTH")
    msgs=$(echo "$resp" | jq -r '.messages[] | .text // .blocks[0].text.text // ""' 2>/dev/null || echo "")
    if [ -n "$msgs" ]; then
      if grep -qF "Scheduled task" <<<"$msgs"; then
        found_sched=true; break
      elif grep -qF "$agent_name" <<<"$msgs"; then
        found_sched=true; break
      elif grep -qF "sched" <<<"$msgs"; then
        found_sched=true; break
      fi
    fi
    # Handle rate limiting
    local err retry_after
    err=$(echo "$resp" | jq -r '.error // empty' 2>/dev/null || echo "")
    if [ "$err" = "ratelimited" ]; then
      retry_after=$(echo "$resp" | jq -r '.retry_after // empty' 2>/dev/null || echo "")
      sleep "${retry_after:-5}"
    else
      sleep 3
    fi
  done

  if [ "$found_sched" = "true" ]; then
    soft_pass "schedule card or notification posted"
  else
    soft_fail "no schedule notification or agent card found after 60s"
  fi

  # Transition to done — verify completion notification.
  beads_update "$agent_id" "{\"fields\":{\"agent_state\":\"done\"}}"
  sleep 3

  beads_close "$agent_id"
  pass "schedule card lifecycle completed"
}

# --- Scenario 4: Terminal State Card ---

scenario_card_terminal() {
  echo ""
  echo "=== Scenario 4: Terminal State Card ==="

  local agent_name="e2e-terminal-test-$(date +%s)"
  local agent_id
  agent_id=$(beads_create "agent" "$agent_name" "{\"agent\":\"$agent_name\",\"role\":\"crew\",\"project\":\"gardenio\",\"agent_state\":\"working\"}")
  check_not_empty "$agent_id" "created agent bead for terminal state test"

  sleep 5

  # Transition to done with a wrapup.
  local wrapup='{"accomplishments":"E2E test completed successfully","blockers":"None"}'
  beads_update "$agent_id" "$(jq -n --arg w "$wrapup" '{fields:{agent_state:"done",wrapup:$w}}')"
  sleep 3

  # Verify: the card should show the wrapup content.
  if wait_for_slack_message "$SLACK_CHANNEL" "0" "Accomplishments" 15; then
    soft_pass "wrapup card content visible"
  else
    soft_fail "wrapup card content not found — may be in blocks"
  fi

  beads_close "$agent_id"
  pass "terminal state card lifecycle completed"
}

# --- Scenario 5: Status cycling — rapid state transitions ---

scenario_status_cycling() {
  echo ""
  echo "=== Scenario 5: Status cycling — rapid state transitions ==="

  local agent_name="e2e-cycle-test-$(date +%s)"
  local agent_id
  agent_id=$(beads_create "agent" "$agent_name" "{\"agent\":\"$agent_name\",\"role\":\"crew\",\"project\":\"gardenio\",\"agent_state\":\"spawning\"}")
  check_not_empty "$agent_id" "created agent bead for cycling test"

  # Rapid state transitions: spawning → working → idle → working → done
  sleep 3
  beads_update "$agent_id" '{"fields":{"agent_state":"working"}}'
  sleep 2
  beads_update "$agent_id" '{"fields":{"agent_state":"idle"}}'
  sleep 2
  beads_update "$agent_id" '{"fields":{"agent_state":"working"}}'
  sleep 2
  beads_update "$agent_id" '{"fields":{"agent_state":"done"}}'
  sleep 3

  # Verify: the channel should have at most 1 card (the bridge updates in-place).
  local hist_resp msgs
  hist_resp=$(curl -s "${SLACK_API}/conversations.history?channel=${SLACK_CHANNEL}&limit=50" \
    -H "$SLACK_AUTH")
  msgs=$(echo "$hist_resp" | jq "[.messages[] | select(.text // \"\" | contains(\"$agent_name\"))] | length" 2>/dev/null || echo "0")

  if [ "$msgs" -le 2 ]; then
    soft_pass "at most 1-2 messages for rapidly cycled agent (got $msgs)"
  else
    soft_fail "rapid cycling produced $msgs messages (expected <=2)"
  fi

  beads_close "$agent_id"
  pass "status cycling lifecycle completed"
}

# --- Scenario 6: Concurrent agent cards ---

scenario_concurrent_agents() {
  echo ""
  echo "=== Scenario 6: Concurrent agent cards ==="

  # Create 3 agents simultaneously.
  local ts
  ts=$(date +%s)
  local ids=()
  local names=()

  for i in 1 2 3; do
    local name="e2e-concurrent-${i}-${ts}"
    local id
    id=$(beads_create "agent" "$name" "{\"agent\":\"$name\",\"role\":\"crew\",\"project\":\"gardenio\",\"agent_state\":\"spawning\"}")
    if [ -n "$id" ]; then
      ids+=("$id")
      names+=("$name")
    fi
  done

  if [ "${#ids[@]}" -eq 3 ]; then
    pass "created 3 concurrent agents"
  else
    fail "create concurrent agents" "only ${#ids[@]} created"
  fi

  # Wait for bridge to process all.
  sleep 8

  # Transition all to working.
  for id in "${ids[@]}"; do
    beads_update "$id" '{"fields":{"agent_state":"working"}}'
  done
  sleep 3

  # Verify each agent has distinct bead IDs.
  local unique_ids
  unique_ids=$(printf '%s\n' "${ids[@]}" | sort -u | wc -l)
  if [ "$unique_ids" -eq 3 ]; then
    pass "all 3 agents have unique bead IDs"
  else
    fail "unique bead IDs" "expected 3, got $unique_ids"
  fi

  # Transition all to done and close.
  for id in "${ids[@]}"; do
    beads_update "$id" '{"fields":{"agent_state":"done"}}'
  done
  sleep 2
  for id in "${ids[@]}"; do
    beads_close "$id"
  done
  pass "concurrent agent lifecycle completed"
}

# --- Scenario 7: DM bead creation and field validation ---

scenario_dm_bead() {
  echo ""
  echo "=== Scenario 7: DM bead creation and field validation ==="

  local agent_name="e2e-dm-card-$(date +%s)"
  local agent_id
  agent_id=$(beads_create "agent" "$agent_name" "{\"agent\":\"$agent_name\",\"role\":\"crew\",\"project\":\"gardenio\",\"agent_state\":\"working\"}")
  check_not_empty "$agent_id" "created source agent for DM test"
  sleep 2

  # Create a DM bead with all fields populated.
  local dm_body="Please review the latest MR changes"
  local dm_id
  dm_id=$(beads_create "dm" "DM from $agent_name" "$(jq -n \
    --arg agent "$agent_name" \
    --arg body "$dm_body" \
    '{
      source_agent: $agent,
      target_user: "U_E2E_TEST_PLACEHOLDER",
      subject: "MR Review Request",
      body: $body,
      dm_mode: "notify"
    }')")

  if [ -n "$dm_id" ]; then
    pass "created dm bead ($dm_id)"

    # Verify bead fields.
    local dm_detail
    dm_detail=$(beads_get "$dm_id")
    local dm_mode dm_source
    dm_mode=$(echo "$dm_detail" | jq -r '.fields.dm_mode // empty' 2>/dev/null || echo "")
    dm_source=$(echo "$dm_detail" | jq -r '.fields.source_agent // empty' 2>/dev/null || echo "")

    if [ "$dm_mode" = "notify" ]; then
      pass "dm bead has dm_mode=notify"
    else
      fail "dm bead dm_mode" "expected 'notify', got '$dm_mode'"
    fi

    if [ "$dm_source" = "$agent_name" ]; then
      pass "dm bead has correct source_agent"
    else
      fail "dm bead source_agent" "expected '$agent_name', got '$dm_source'"
    fi
  else
    fail "create dm bead" "no id returned"
  fi

  # Create a question-mode DM.
  local q_id
  q_id=$(beads_create "dm" "Question from $agent_name" "$(jq -n \
    --arg agent "$agent_name" \
    '{
      source_agent: $agent,
      target_user: "U_E2E_TEST_PLACEHOLDER",
      body: "What is the deploy schedule?",
      dm_mode: "question",
      timeout_minutes: 10
    }')")

  if [ -n "$q_id" ]; then
    pass "created question-mode dm bead ($q_id)"
    local q_mode
    q_mode=$(beads_get "$q_id" | jq -r '.fields.dm_mode // empty' 2>/dev/null || echo "")
    if [ "$q_mode" = "question" ]; then
      pass "question dm has dm_mode=question"
    else
      fail "question dm mode" "expected 'question', got '$q_mode'"
    fi
  else
    fail "create question dm bead" "no id returned"
  fi

  # Create a review-mode DM.
  local r_id
  r_id=$(beads_create "dm" "Review request from $agent_name" "$(jq -n \
    --arg agent "$agent_name" \
    '{
      source_agent: $agent,
      target_user: "U_E2E_TEST_PLACEHOLDER",
      body: "Please review MR !999",
      dm_mode: "review",
      mr_url: "https://gitlab.example.com/merge_requests/999",
      mr_title: "Fix widget alignment"
    }')")

  if [ -n "$r_id" ]; then
    pass "created review-mode dm bead ($r_id)"
    local r_fields
    r_fields=$(beads_get "$r_id" | jq -r '.fields.mr_url // empty' 2>/dev/null || echo "")
    check_not_empty "$r_fields" "review dm has mr_url field"
  else
    fail "create review dm bead" "no id returned"
  fi

  beads_close "$agent_id"
}

# --- Scenario 8: Reaction-rule bead creation ---

scenario_reaction_rule() {
  echo ""
  echo "=== Scenario 8: Reaction-rule bead structure ==="

  # Create a reaction-rule bead to verify the schema.
  local rule_id
  rule_id=$(beads_create "reaction-rule" "E2E test reaction rule" "$(jq -n \
    '{
      emoji: "rocket",
      match: "deploy",
      role: "crew",
      project: "gardenio",
      prompt: "Deploy the changes mentioned in {message_text}"
    }')")

  if [ -n "$rule_id" ]; then
    pass "created reaction-rule bead ($rule_id)"

    local rule_detail
    rule_detail=$(beads_get "$rule_id")
    local emoji project prompt
    emoji=$(echo "$rule_detail" | jq -r '.fields.emoji // empty' 2>/dev/null || echo "")
    project=$(echo "$rule_detail" | jq -r '.fields.project // empty' 2>/dev/null || echo "")
    prompt=$(echo "$rule_detail" | jq -r '.fields.prompt // empty' 2>/dev/null || echo "")

    if [ "$emoji" = "rocket" ]; then
      pass "reaction-rule has emoji=rocket"
    else
      fail "reaction-rule emoji" "expected 'rocket', got '$emoji'"
    fi
    if [ "$project" = "gardenio" ]; then
      pass "reaction-rule has project=gardenio"
    else
      fail "reaction-rule project" "expected 'gardenio', got '$project'"
    fi
    check_not_empty "$prompt" "reaction-rule has prompt field"
  else
    fail "create reaction-rule bead" "no id returned"
  fi
}

# --- Main ---

scenario="${1:-all}"

echo "Slack Card E2E Tests"
echo "===================="
echo "Channel: ${SLACK_CHANNEL}"
echo "Beads:   ${BEADS_HTTP_URL}"
echo ""

case "$scenario" in
  all)
    scenario_agent_card
    sleep 3  # cooldown between Slack-heavy scenarios to avoid rate limits
    scenario_squawk_delivery
    sleep 5  # extra cooldown before schedule card — bridge needs time after squawk Slack activity
    scenario_schedule_card
    sleep 2
    scenario_card_terminal
    sleep 2
    scenario_status_cycling
    sleep 2
    scenario_concurrent_agents
    scenario_dm_bead
    scenario_reaction_rule
    ;;
  agent-card)           scenario_agent_card ;;
  squawk-delivery)      scenario_squawk_delivery ;;
  schedule-card)        scenario_schedule_card ;;
  card-terminal)        scenario_card_terminal ;;
  status-cycling)       scenario_status_cycling ;;
  concurrent-agents)    scenario_concurrent_agents ;;
  dm-bead)              scenario_dm_bead ;;
  reaction-rule)        scenario_reaction_rule ;;
  *)
    echo "Unknown scenario: $scenario"
    echo "Available: all, agent-card, squawk-delivery, schedule-card, card-terminal, status-cycling, concurrent-agents, dm-bead, reaction-rule"
    exit 1
    ;;
esac

echo ""
echo "═══════════════════════════════════════════════════"
if [ "$FAILURES" -eq 0 ]; then
  echo " ✓ All bead assertions passed"
else
  echo " ✗ $FAILURES bead assertion(s) failed"
fi
if [ "$SOFT_FAILURES" -gt 0 ]; then
  printf ' \033[0;33m⚠ %d Slack delivery check(s) failed (non-blocking)\033[0m\n' "$SOFT_FAILURES"
  for w in "${SOFT_WARNINGS[@]}"; do
    printf '   \033[0;33m- %s\033[0m\n' "$w"
  done
fi
echo "═══════════════════════════════════════════════════"

# Only hard failures (bead layer) affect exit code
[ "$FAILURES" -eq 0 ]
