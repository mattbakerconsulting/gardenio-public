#!/usr/bin/env bash
# lib-slackmock.sh — helper library for E2E tests that need Slack API access.
#
# When SLACKMOCK_URL is set, Slack API calls are directed to the mock server
# instead of the real Slack API. This prevents message flooding in production
# Slack channels during E2E tests.
#
# Usage in test scripts:
#   source "$(dirname "$0")/lib-slackmock.sh"
#   # SLACK_API is now set to either slackmock or real Slack
#   curl "${SLACK_API}/chat.postMessage" ...
#
# Mock query helpers:
#   slackmock_find_message <channel> <search_pattern>
#   slackmock_call_count <method>
#   slackmock_reset

# Determine Slack API base URL.
# Priority: SLACKMOCK_URL > real Slack API
if [ -n "${SLACKMOCK_URL:-}" ]; then
  SLACK_API="${SLACKMOCK_URL}/api"
  SLACK_MOCK_QUERY="${SLACKMOCK_URL}/mock"
  SLACK_IS_MOCK=true
else
  SLACK_API="https://slack.com/api"
  SLACK_MOCK_QUERY=""
  SLACK_IS_MOCK=false
fi
export SLACK_API SLACK_IS_MOCK

# slackmock_find_message searches for a message in the mock.
# Args: <channel> <search_pattern> [max_wait_seconds]
# Returns: message JSON or empty string
slackmock_find_message() {
  local channel="$1"
  local pattern="$2"
  local max_wait="${3:-15}"

  if [ "$SLACK_IS_MOCK" = "true" ]; then
    # Query the mock's search endpoint directly — much faster than polling.
    local elapsed=0
    while [ "$elapsed" -lt "$max_wait" ]; do
      local result
      result=$(curl -sf --max-time 5 \
        "${SLACK_MOCK_QUERY}/messages?channel=${channel}&search=$(python3 -c "import urllib.parse; print(urllib.parse.quote('$pattern'))" 2>/dev/null || echo "$pattern")" \
        2>/dev/null || echo '{"total":0}')
      local total
      total=$(echo "$result" | python3 -c "import sys,json; print(json.load(sys.stdin).get('total',0))" 2>/dev/null || echo "0")
      if [ "$total" -gt 0 ]; then
        echo "$result" | python3 -c "import sys,json; msgs=json.load(sys.stdin).get('messages',[]); print(json.dumps(msgs[0]) if msgs else '')" 2>/dev/null
        return 0
      fi
      sleep 2
      elapsed=$((elapsed + 2))
    done
    return 1
  else
    # Real Slack: use conversations.history polling (existing pattern).
    local oldest
    oldest=$(date +%s)
    local elapsed=0
    while [ "$elapsed" -lt "$max_wait" ]; do
      local history
      history=$(curl -s --max-time 10 \
        "${SLACK_API}/conversations.history?channel=${channel}&oldest=${oldest}&limit=20" \
        -H "Authorization: Bearer ${SLACK_BOT_TOKEN}" 2>/dev/null || echo '{"ok":false}')
      if echo "$history" | python3 -c "import sys,json; d=json.load(sys.stdin); exit(0 if d.get('ok') else 1)" 2>/dev/null; then
        local match
        match=$(echo "$history" | python3 -c "
import sys, json
d = json.load(sys.stdin)
for m in d.get('messages', []):
    if '$pattern' in m.get('text', '') or '$pattern' in json.dumps(m.get('blocks', [])):
        print(json.dumps(m))
        break
" 2>/dev/null || echo "")
        if [ -n "$match" ]; then
          echo "$match"
          return 0
        fi
      fi
      sleep 3
      elapsed=$((elapsed + 3))
    done
    return 1
  fi
}

# slackmock_call_count returns the number of times a Slack API method was called.
# Args: <method> (e.g. "chat.postMessage")
slackmock_call_count() {
  local method="$1"
  if [ "$SLACK_IS_MOCK" != "true" ]; then
    echo "-1" # Not available for real Slack
    return
  fi
  curl -sf --max-time 5 "${SLACK_MOCK_QUERY}/calls?method=${method}" 2>/dev/null \
    | python3 -c "import sys,json; print(json.load(sys.stdin).get('total',0))" 2>/dev/null \
    || echo "0"
}

# slackmock_reset clears all recorded state in the mock.
slackmock_reset() {
  if [ "$SLACK_IS_MOCK" = "true" ]; then
    curl -sf --max-time 5 -X POST "${SLACK_MOCK_QUERY}/reset" >/dev/null 2>&1 || true
  fi
}

# slackmock_stats returns the mock server's call statistics.
slackmock_stats() {
  if [ "$SLACK_IS_MOCK" = "true" ]; then
    curl -sf --max-time 5 "${SLACK_MOCK_QUERY}/stats" 2>/dev/null || echo '{"error":"unreachable"}'
  else
    echo '{"error":"not using mock"}'
  fi
}
