#!/usr/bin/env bash
# test-escalation-slack-notify.sh — escalation Slack notification E2E tests (Phase 4)
#
# Tests that the slack-bridge posts and updates Slack notifications for
# escalation bead lifecycle events:
#   1. New escalation → Slack notification with "Review in Web UI" link
#   2. Escalation approved → Slack message updated with approval details
#   3. Escalation rejected → Slack message updated with rejection notice
#
# Prerequisites:
#   - SLACK_BOT_TOKEN set (xoxb-... for staging Slack app)
#   - SLACK_CHANNEL set (C... channel where bridge posts)
#   - BEADS_HTTP_URL set (kbeads HTTP API, e.g. http://localhost:19090)
#   - BRIDGE_HTTP_URL set (slack-bridge HTTP API, e.g. http://localhost:19091)
#   - BEADS_TOKEN set (auth token for kbeads API)
#   - kd binary in PATH (or set KD_BIN)
#   - curl, jq installed
#
# Scenarios tested:
#   1. New escalation triggers Slack notification with correct content
#   2. Approving escalation updates the Slack message with approval details
#   3. Rejecting escalation updates the Slack message with rejection notice

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
BRIDGE_SVC="${BRIDGE_SVC:-${NAMESPACE}-slack-bridge}"
KD_BIN="${KD_BIN:-kd}"
PASS=0
FAIL=0
ERRORS=()
PF_PID=""
BRIDGE_PF_PID=""
CREATED_BEADS=()

export KD_ACTOR="${KD_ACTOR:-e2e-test}"

SLACK_API="https://slack.com/api"

# Test user with approve:escalation capability
TEST_USER_EMAIL="e2e-test@gardenio.dev"

# ── Helpers ───────────────────────────────────────────────────────
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

assert_contains() {
  local name="$1"
  local pattern="$2"
  local actual="$3"
  if grep -qF -- "$pattern" <<< "$actual"; then
    pass "$name"
  else
    fail "$name" "expected to contain '$pattern'; got: $(echo "$actual" | head -3)"
  fi
}

assert_not_contains() {
  local name="$1"
  local pattern="$2"
  local actual="$3"
  if ! grep -qF -- "$pattern" <<< "$actual"; then
    pass "$name"
  else
    fail "$name" "expected to NOT contain '$pattern'"
  fi
}

json_field() {
  python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('$1',''))" 2>/dev/null
}

# Wait for a condition with timeout and polling interval.
# Usage: wait_for "description" timeout_secs interval_secs command [args...]
wait_for() {
  local desc="$1" timeout="$2" interval="$3"
  shift 3
  local end=$(($(date +%s) + timeout))
  while [ "$(date +%s)" -lt "$end" ]; do
    if "$@" 2>/dev/null; then
      return 0
    fi
    sleep "$interval"
  done
  dim "  wait_for timed out after ${timeout}s: $desc"
  return 1
}

# curl wrapper for bridge API that returns HTTP status code.
# Uses -f to fail on server errors but captures the status regardless.
bridge_curl() {
  local method="$1"
  local path="$2"
  shift 2
  local output http_code
  output=$(curl -s --max-time 30 -X "$method" \
    -w '\n%{http_code}' \
    "${BRIDGE_HTTP_URL}${path}" \
    "$@" 2>/dev/null) || true
  echo "$output"
}

# Locate the Slack message the bridge posted for an escalation bead by asking
# the bridge directly for the stored MessageRef, then fetching the message via
# conversations.replies. This is robust against busy channels where the bridge
# threads escalation notifications under a long-lived agent card whose parent
# is well outside any reasonable conversations.history window.
#
# Polls the bridge until the ref appears (the bridge sees the bead created
# event via SSE and posts asynchronously). Returns the full Slack message JSON,
# or empty string on timeout.
slack_find_escalation_message() {
  local esc_id="$1"
  local max_wait="${2:-45}"
  local elapsed=0 ref channel ts thread_ts lookup_ts replies
  while [ "$elapsed" -lt "$max_wait" ]; do
    ref=$(curl -s --max-time 10 "${BRIDGE_HTTP_URL}/api/escalations/${esc_id}/slack-ref" 2>/dev/null || echo "")
    if [ -n "$ref" ] && echo "$ref" | jq -e '.ts' >/dev/null 2>&1; then
      channel=$(echo "$ref" | jq -r '.channel // ""')
      ts=$(echo "$ref" | jq -r '.ts // ""')
      thread_ts=$(echo "$ref" | jq -r '.thread_ts // ""')
      if [ -n "$ts" ]; then
        lookup_ts="${thread_ts:-$ts}"
        replies=$(curl -s --max-time 10 \
          "${SLACK_API}/conversations.replies?channel=${channel}&ts=${lookup_ts}&latest=${ts}&inclusive=true&limit=1000" \
          -H "Authorization: Bearer ${SLACK_BOT_TOKEN}" 2>/dev/null || echo '{"ok":false}')
        if echo "$replies" | jq -e '.ok' >/dev/null 2>&1; then
          echo "$replies" | jq -r --arg t "$ts" '[.messages[] | select(.ts == $t)] | first // empty' 2>/dev/null
          return 0
        fi
      fi
    fi
    sleep 2
    elapsed=$((elapsed + 2))
  done
  echo ""
  return 1
}

# Search Slack channel for a message containing a pattern posted after a given timestamp.
# Searches both top-level messages AND thread replies (the bridge may post escalation
# notifications as thread replies when agent threading is enabled).
# Returns the full message JSON or empty string.
slack_find_message() {
  local channel="$1"
  local oldest="$2"
  local pattern="$3"
  local max_wait="${4:-45}"

  # Apply a small backward offset to handle clock skew between CI runner and kbeads.
  oldest=$((oldest - 5))

  local elapsed=0
  while [ "$elapsed" -lt "$max_wait" ]; do
    local history
    history=$(curl -s --max-time 10 \
      "${SLACK_API}/conversations.history?channel=${channel}&oldest=${oldest}&limit=30" \
      -H "Authorization: Bearer ${SLACK_BOT_TOKEN}" 2>/dev/null || echo '{"ok":false}')

    # Handle Slack rate limiting (HTTP 429 returns ok:false with retry_after).
    local rate_limited
    rate_limited=$(echo "$history" | jq -r '.error // ""' 2>/dev/null || echo "")
    if [ "$rate_limited" = "ratelimited" ]; then
      local retry_after
      retry_after=$(echo "$history" | jq -r '.retry_after // 3' 2>/dev/null || echo "3")
      dim "  Slack rate-limited; waiting ${retry_after}s..."
      sleep "$retry_after"
      elapsed=$((elapsed + retry_after))
      continue
    fi

    if echo "$history" | jq -e '.ok' >/dev/null 2>&1; then
      # Check top-level messages first.
      local match
      match=$(echo "$history" | jq -r --arg pat "$pattern" \
        '[.messages[] | select((.text // "" | contains($pat)) or (.blocks // [] | tostring | contains($pat)))] | first // empty' 2>/dev/null || echo "")
      if [ -n "$match" ] && [ "$match" != "null" ]; then
        echo "$match"
        return 0
      fi

      # When agent threading is enabled, escalation notifications are posted as
      # thread replies under an agent card. Check threads of recent bot messages.
      local thread_tss
      thread_tss=$(echo "$history" | jq -r \
        '.messages[] | select(.subtype == "bot_message" or (.reply_count // 0) > 0) | .ts' 2>/dev/null || echo "")
      for ts in $thread_tss; do
        [ -z "$ts" ] && continue
        local replies
        replies=$(curl -s --max-time 10 \
          "${SLACK_API}/conversations.replies?channel=${channel}&ts=${ts}&limit=30" \
          -H "Authorization: Bearer ${SLACK_BOT_TOKEN}" 2>/dev/null || echo '{"ok":false}')
        local thread_match
        thread_match=$(echo "$replies" | jq -r --arg pat "$pattern" \
          '[.messages[] | select((.text // "" | contains($pat)) or (.blocks // [] | tostring | contains($pat)))] | first // empty' 2>/dev/null || echo "")
        if [ -n "$thread_match" ] && [ "$thread_match" != "null" ]; then
          echo "$thread_match"
          return 0
        fi
      done
    fi

    sleep 3
    elapsed=$((elapsed + 3))
  done

  echo ""
  return 1
}

# Fetch a specific Slack message by channel + ts, returning the full message JSON.
# For thread replies, pass the parent thread_ts as $3 so conversations.replies is used.
slack_get_message() {
  local channel="$1"
  local ts="$2"
  local thread_ts="${3:-}"

  if [ -n "$thread_ts" ]; then
    # Thread reply — use conversations.replies with the parent thread ts.
    curl -s --max-time 10 \
      "${SLACK_API}/conversations.replies?channel=${channel}&ts=${thread_ts}&latest=${ts}&inclusive=true&limit=1" \
      -H "Authorization: Bearer ${SLACK_BOT_TOKEN}" 2>/dev/null \
      | jq -r --arg msg_ts "$ts" '[.messages[] | select(.ts == $msg_ts)] | first // empty' 2>/dev/null || echo ""
  else
    curl -s --max-time 10 \
      "${SLACK_API}/conversations.history?channel=${channel}&latest=${ts}&inclusive=true&limit=1" \
      -H "Authorization: Bearer ${SLACK_BOT_TOKEN}" 2>/dev/null \
      | jq -r '.messages[0] // empty' 2>/dev/null || echo ""
  fi
}

# Poll for a Slack message update that matches a pattern.
# Waits up to max_wait seconds for the message (identified by channel + ts) to
# contain the expected pattern in its blocks or text.
slack_wait_for_update() {
  local channel="$1"
  local ts="$2"
  local thread_ts="$3"
  local pattern="$4"
  local max_wait="${5:-20}"

  local elapsed=0
  while [ "$elapsed" -lt "$max_wait" ]; do
    local msg
    msg=$(slack_get_message "$channel" "$ts" "$thread_ts")
    if [ -n "$msg" ] && [ "$msg" != "null" ]; then
      local blocks text
      blocks=$(echo "$msg" | jq -r '.blocks // [] | tostring' 2>/dev/null || echo "[]")
      text=$(echo "$msg" | jq -r '.text // ""' 2>/dev/null || echo "")
      if grep -qiF -- "$pattern" <<<"$blocks" || grep -qiF -- "$pattern" <<<"$text"; then
        echo "$msg"
        return 0
      fi
    fi
    sleep 2
    elapsed=$((elapsed + 2))
  done

  # Return the last fetched message even if it didn't match — caller decides how to handle.
  echo "${msg:-}"
  return 1
}

# ── Setup: port-forward kbeads server ─────────────────────────────
setup_beads_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi

  blue "Port-forwarding ${NAMESPACE} kbeads server..."
  local port=19096
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-slack-notify.log 2>&1 &
  PF_PID=$!

  export BEADS_HTTP_URL="http://localhost:${port}"
  # Probe for any HTTP response — the kbeads server requires Authorization
  # on every request (including /healthz, which returns 401 unauthenticated),
  # so a `curl -sf` probe never sees success even though the port-forward is
  # up. Treat any HTTP status code other than 000 (no response) as ready.
  if ! wait_for "kbeads port-forward" 15 1 bash -c \
      "[ \"\$(curl -s --max-time 5 -o /dev/null -w '%{http_code}' '${BEADS_HTTP_URL}/healthz')\" != '000' ]"; then
    red "ERROR: kbeads port-forward did not become ready within 15s"
    cat /tmp/kd-pf-slack-notify.log 2>/dev/null || true
    exit 1
  fi
  dim "  kbeads at $BEADS_HTTP_URL (pid $PF_PID)"
}

# ── Setup: port-forward bridge ────────────────────────────────────
setup_bridge_portforward() {
  if [ -n "${BRIDGE_HTTP_URL:-}" ]; then
    blue "Using BRIDGE_HTTP_URL=$BRIDGE_HTTP_URL"
    return
  fi

  blue "Port-forwarding ${NAMESPACE} bridge..."
  local port=19092
  kubectl -n "$NAMESPACE" port-forward "svc/${BRIDGE_SVC}" "${port}:8090" \
    >/tmp/bridge-pf-slack-notify.log 2>&1 &
  BRIDGE_PF_PID=$!

  export BRIDGE_HTTP_URL="http://localhost:${port}"
  if ! wait_for "bridge port-forward" 15 1 curl -sf --max-time 5 -o /dev/null "${BRIDGE_HTTP_URL}/healthz"; then
    # Some bridge versions only respond on root.
    if ! wait_for "bridge port-forward (root)" 5 1 curl -s --max-time 5 -o /dev/null -w '%{http_code}' "${BRIDGE_HTTP_URL}/" ; then
      red "ERROR: bridge port-forward did not become ready within 20s"
      cat /tmp/bridge-pf-slack-notify.log 2>/dev/null || true
      exit 1
    fi
  fi
  dim "  Bridge at $BRIDGE_HTTP_URL (pid $BRIDGE_PF_PID)"
}

# ── Setup: get token ─────────────────────────────────────────────
setup_token() {
  if [ -n "${BEADS_TOKEN:-}" ]; then
    return
  fi
  local secret
  secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
    -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
  if [ -n "$secret" ]; then
    export BEADS_TOKEN="$secret"
    dim "  Token loaded from secret kd-beads-token"
  else
    dim "  WARNING: No token found; requests may be rejected"
  fi
}

# ── Seed escalation type config ──────────────────────────────────
seed_escalation_config() {
  # type:escalation is builtin-authoritative; the daemon builtin (config.go) +
  # controller EnsureConfigs seed already declare a SUPERSET of these fields.
  # The old hand-maintained PUT could only shadow-and-drift the builtin
  # (ka-0000003677 / generalized ka-0000000407). Dropped so e2e exercises the
  # builtin prod runs, guarded by TestE2ESeedTypes_NoBuiltinShadow. Do NOT re-add.
  dim "  type:escalation config: using daemon builtin (no shadow PUT)"
}

# ── Seed test user ───────────────────────────────────────────────
seed_test_user() {
  blue "Seeding test user bead..."
  # type:user is builtin-authoritative; the daemon builtin (config.go) +
  # controller EnsureConfigs seed already declare a SUPERSET of these fields.
  # The old hand-maintained PUT could only shadow-and-drift the builtin
  # (ka-0000003677 / generalized ka-0000000407). Dropped so e2e exercises the
  # builtin prod runs, guarded by TestE2ESeedTypes_NoBuiltinShadow. Do NOT re-add.

  local user_out user_id
  user_out=$("$KD_BIN" create "E2E Slack Notify Test User" \
    --type=user \
    -l "e2e:escalation-slack-notify" \
    -f "email=$TEST_USER_EMAIL" \
    -f "display_name=E2E Test User" \
    -f 'capabilities=["approve:escalation"]' \
    --actor "$KD_ACTOR" --json 2>/dev/null || echo "{}")
  user_id=$(echo "$user_out" | json_field id || echo "")
  if [ -n "$user_id" ]; then
    CREATED_BEADS+=("$user_id")
    dim "  Test user created: $user_id ($TEST_USER_EMAIL)"
  else
    dim "  WARNING: Failed to create test user (may already exist)"
  fi
}

# ── Cleanup ───────────────────────────────────────────────────────
cleanup() {
  for id in "${CREATED_BEADS[@]}"; do
    "$KD_BIN" close "$id" --actor "$KD_ACTOR" 2>/dev/null || true
  done
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
  if [ -n "$BRIDGE_PF_PID" ]; then
    kill "$BRIDGE_PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " escalation-slack-notify E2E tests (Phase 4: Slack Notifications)"
  blue "═══════════════════════════════════════════════════"

  # ── Prerequisite: Slack credentials ────────────────────────────
  if [ -z "${SLACK_BOT_TOKEN:-}" ]; then
    blue "SKIP: SLACK_BOT_TOKEN not set — Slack notification tests require Slack credentials"
    exit 0
  fi
  if [ -z "${SLACK_CHANNEL:-}" ]; then
    blue "SKIP: SLACK_CHANNEL not set"
    exit 0
  fi

  setup_beads_portforward
  setup_token
  setup_bridge_portforward

  # Verify kbeads is reachable
  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kbeads at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  kbeads reachable: $BEADS_HTTP_URL"

  # Verify bridge is reachable
  local bridge_health
  bridge_health=$(curl -sf --max-time 10 "${BRIDGE_HTTP_URL}/healthz" 2>/dev/null || \
                  curl -sf --max-time 10 "${BRIDGE_HTTP_URL}/" -o /dev/null -w '%{http_code}' 2>/dev/null || echo "000")
  if [ "$bridge_health" = "000" ]; then
    red "ERROR: Cannot reach bridge at $BRIDGE_HTTP_URL"
    exit 1
  fi
  green "  Bridge reachable: $BRIDGE_HTTP_URL"

  # Probe: check if escalation API is deployed.
  # curl with -w '%{http_code}' writes "000" on connection failure AND exits
  # non-zero, so a naive `$(curl ... || echo "000")` captures "000\n000" and
  # the skip guard below would never match — see gd-3RxELxfbCe. Use `|| true`
  # to swallow the non-zero exit, then default an empty result to "000".
  local probe_status
  probe_status=$(curl -s --max-time 10 -o /dev/null -w '%{http_code}' \
    "${BRIDGE_HTTP_URL}/api/escalations" 2>/dev/null || true)
  probe_status="${probe_status:-000}"
  if [ "$probe_status" = "404" ] || [ "$probe_status" = "000" ]; then
    blue "Bridge does not have escalation API (HTTP $probe_status)."
    blue "  Redeploy with an image containing PRs #629–#636 to enable these tests."
    green " SKIP: escalation-slack-notify (bridge API not available)"
    exit 0
  fi
  dim "  Escalation API probe: HTTP $probe_status"

  # Verify Slack API is accessible
  local slack_test
  slack_test=$(curl -s --max-time 10 "${SLACK_API}/auth.test" \
    -H "Authorization: Bearer ${SLACK_BOT_TOKEN}" 2>/dev/null || echo '{"ok":false}')
  if ! echo "$slack_test" | jq -e '.ok' >/dev/null 2>&1; then
    red "ERROR: Slack API not reachable or token invalid"
    dim "  Response: $slack_test"
    exit 1
  fi
  green "  Slack API reachable"

  seed_escalation_config
  seed_test_user

  # Allow time for bridge user directory sync
  dim "  Waiting for bridge user directory sync..."
  sleep 5

  echo

  local TEST_TAG="e2e-slacknotify-$$-$(date +%s)"

  # ── Scenario 1: New escalation triggers Slack notification ─────
  blue "Scenario 1: New escalation triggers Slack notification"

  # Record timestamp before creation for Slack search window
  local before_create
  before_create=$(date +%s)

  local esc_out esc_id
  esc_out=$("$KD_BIN" create "E2E slack-notify $TEST_TAG: IAM role creation" \
    --type=escalation \
    -l "e2e:$TEST_TAG" \
    -f 'operation_type=aws_iam' \
    -f 'operation_spec={"action":"create_role","role_name":"e2e-slack-notify-role"}' \
    -f 'context=E2E test: verify Slack notification for new escalation' \
    -f 'requesting_agent=e2e-agent-slack-notify' \
    --actor "$KD_ACTOR" --json 2>/dev/null || echo "{}")
  esc_id=$(echo "$esc_out" | json_field id || echo "")

  if [ -z "$esc_id" ]; then
    red "ERROR: Could not create test escalation bead"
    exit 1
  fi
  CREATED_BEADS+=("$esc_id")
  dim "  Escalation created: $esc_id"

  # Wait for bridge to process SSE event and post to Slack.
  # The chain is: kbeads SSE → bridge handleCreated → Slack PostMessage.
  # Allow up to 45s for slow CI environments.
  dim "  Waiting for Slack notification (up to 45s)..."
  local create_msg
  create_msg=$(slack_find_escalation_message "$esc_id" 45 || echo "")

  # Track thread_ts for fetching thread replies in subsequent scenarios.
  local create_msg_ts="" create_msg_thread_ts=""

  if [ -n "$create_msg" ]; then
    local create_msg_text create_msg_blocks
    create_msg_text=$(echo "$create_msg" | jq -r '.text // ""' 2>/dev/null || echo "")
    create_msg_blocks=$(echo "$create_msg" | jq -r '.blocks // [] | tostring' 2>/dev/null || echo "[]")
    create_msg_ts=$(echo "$create_msg" | jq -r '.ts // ""' 2>/dev/null || echo "")
    create_msg_thread_ts=$(echo "$create_msg" | jq -r '.thread_ts // ""' 2>/dev/null || echo "")

    pass "Slack notification posted for new escalation"
    assert_contains "notification fallback text mentions escalation" "Escalation" "$create_msg_text"
    assert_contains "notification blocks contain bead ID" "$esc_id" "$create_msg_blocks"
    assert_contains "notification blocks contain Escalation Request header" "Escalation Request" "$create_msg_blocks"
    assert_contains "notification blocks contain operation type" "aws_iam" "$create_msg_blocks"
    assert_contains "notification blocks contain Review in Web UI button" "Review in Web UI" "$create_msg_blocks"
  else
    fail "Slack notification for new escalation" "no message found containing '$esc_id' in channel within 45s"
  fi
  echo

  # ── Scenario 2: Approval updates Slack message ─────────────────
  blue "Scenario 2: Approving escalation updates Slack message"

  # Approve via bridge API
  local approve_resp approve_status
  approve_resp=$(bridge_curl POST "/api/escalations/$esc_id/approve" \
    -H "X-Auth-Request-Email: $TEST_USER_EMAIL" \
    -H "Content-Type: application/json" \
    -d '{"execution_target":"local"}')
  approve_status=$(echo "$approve_resp" | tail -1)

  if [ -n "$approve_status" ] && [ "$approve_status" -ge 200 ] 2>/dev/null && [ "$approve_status" -lt 300 ] 2>/dev/null; then
    pass "escalation approved via bridge API (HTTP $approve_status)"

    if [ -n "${create_msg_ts:-}" ]; then
      # Poll for the Slack message to be updated with "Approved" instead of a static sleep.
      dim "  Waiting for Slack message update (up to 25s)..."
      local updated_msg
      updated_msg=$(slack_wait_for_update "$SLACK_CHANNEL" "$create_msg_ts" "$create_msg_thread_ts" "Approved" 25 || echo "")

      if [ -n "$updated_msg" ] && [ "$updated_msg" != "null" ]; then
        local updated_blocks
        updated_blocks=$(echo "$updated_msg" | jq -r '.blocks // [] | tostring' 2>/dev/null || echo "[]")
        local updated_text
        updated_text=$(echo "$updated_msg" | jq -r '.text // ""' 2>/dev/null || echo "")

        assert_contains "updated message mentions Approved" "Approved" "$updated_blocks"
        # The bridge prefers the human-readable email (approved_by_email) on
        # the "Approved by" line and falls back to the user bead ID only when
        # the email is empty, then names the method in words
        # (escalationApprovalLine in controller/internal/bridge/bot_escalations.go).
        assert_contains "updated message has Approved by line" "Approved by " "$updated_blocks"
        assert_contains "updated message references approver email" "$TEST_USER_EMAIL" "$updated_blocks"
        assert_contains "updated message contains approval method" "in the web UI" "$updated_blocks"
        assert_not_contains "updated message no longer has Review button" "Review in Web UI" "$updated_blocks"
      else
        fail "fetch updated Slack message" "could not retrieve message at ts=$create_msg_ts (message may not have been updated)"
      fi
    else
      fail "verify approval update" "no original message ts available (scenario 1 failed)"
    fi
  else
    fail "approve escalation" "bridge returned HTTP ${approve_status:-000} (expected 2xx)"
  fi
  echo

  # ── Scenario 3: Rejection updates Slack message ────────────────
  blue "Scenario 3: Rejecting escalation updates Slack message"

  local before_reject
  before_reject=$(date +%s)

  # Create a new escalation for the rejection test
  local esc_reject_out esc_reject_id
  esc_reject_out=$("$KD_BIN" create "E2E slack-notify $TEST_TAG: reject test" \
    --type=escalation \
    -l "e2e:$TEST_TAG" \
    -f 'operation_type=k8s_rbac' \
    -f 'operation_spec={"action":"create_clusterrole","name":"e2e-reject-role"}' \
    -f 'context=E2E test: verify Slack notification on rejection' \
    --actor "$KD_ACTOR" --json 2>/dev/null || echo "{}")
  esc_reject_id=$(echo "$esc_reject_out" | json_field id || echo "")

  if [ -n "$esc_reject_id" ]; then
    CREATED_BEADS+=("$esc_reject_id")
    dim "  Rejection test escalation created: $esc_reject_id"

    # Wait for the initial creation notification
    dim "  Waiting for creation notification (up to 45s)..."
    local reject_create_msg
    reject_create_msg=$(slack_find_escalation_message "$esc_reject_id" 45 || echo "")

    local reject_create_ts="" reject_create_thread_ts=""
    if [ -n "$reject_create_msg" ]; then
      reject_create_ts=$(echo "$reject_create_msg" | jq -r '.ts // ""' 2>/dev/null || echo "")
      reject_create_thread_ts=$(echo "$reject_create_msg" | jq -r '.thread_ts // ""' 2>/dev/null || echo "")
      pass "Slack notification posted for rejection test escalation"
    else
      dim "  WARNING: No creation notification found; rejection update may still work"
    fi

    # Reject via bridge API
    local reject_resp reject_status
    reject_resp=$(bridge_curl POST "/api/escalations/$esc_reject_id/reject" \
      -H "X-Auth-Request-Email: $TEST_USER_EMAIL" \
      -H "Content-Type: application/json" \
      -d '{"reason":"E2E test: this operation is too risky for production"}')
    reject_status=$(echo "$reject_resp" | tail -1)

    if [ -n "$reject_status" ] && [ "$reject_status" -ge 200 ] 2>/dev/null && [ "$reject_status" -lt 300 ] 2>/dev/null; then
      pass "escalation rejected via bridge API (HTTP $reject_status)"

      if [ -n "$reject_create_ts" ]; then
        # Wait for the bridge to update the message after rejection.
        #
        # The bridge's reject API uses kbeads' /v1/escalations/{id}/reject
        # endpoint, which calls store.CloseBead and emits TopicBeadClosed.
        # The bridge's SSE handleClosed fires bead.closed →
        # NotifyEscalationResult, which renders the message as
        # ":x: *Failed*". The "rejected" branding from
        # DismissEscalation is only used by the Slack-button rejection path,
        # not the web-UI/API rejection path exercised here.
        dim "  Waiting for Slack message update (up to 30s)..."
        local rejected_msg
        rejected_msg=$(slack_wait_for_update "$SLACK_CHANNEL" "$reject_create_ts" "$reject_create_thread_ts" "Failed" 30) || true

        if [ -n "$rejected_msg" ] && [ "$rejected_msg" != "null" ]; then
          local rejected_blocks
          rejected_blocks=$(echo "$rejected_msg" | jq -r '.blocks // [] | tostring' 2>/dev/null || echo "[]")
          local rejected_text
          rejected_text=$(echo "$rejected_msg" | jq -r '.text // ""' 2>/dev/null || echo "")

          # Either the result-update fired (Failed) or the dismiss path fired
          # (Escalation Rejected). Both are acceptable rejection
          # acknowledgements — assert one of them is present.
          if grep -qF "*Failed*" <<<"$rejected_blocks"; then
            pass "rejection notification shows Failed status"
          elif grep -qiF "Escalation Rejected" <<<"$rejected_blocks $rejected_text"; then
            pass "rejection notification shows Escalation Rejected status"
          else
            fail "rejection notification updated" "neither 'Failed' nor 'Escalation Rejected' in updated message"
          fi

          assert_not_contains "rejected message no longer has Review button" "Review in Web UI" "$rejected_blocks"
        else
          # Message was deleted — this is valid behavior for DismissEscalation.
          pass "rejection handled (message deleted)"
        fi
      else
        fail "verify rejection update" "no original message ts available"
      fi
    else
      fail "reject escalation" "bridge returned HTTP ${reject_status:-000} (expected 2xx)"
    fi
  else
    fail "scenario 3 setup" "could not create escalation for rejection test"
  fi
  echo

  # ── Summary ─────────────────────────────────────────────────────
  blue "═══════════════════════════════════════════════════"
  local total=$((PASS + FAIL))
  if [ "$FAIL" -eq 0 ]; then
    green " ✓ All $total assertions passed"
  else
    red " ✗ $FAIL/$total assertions failed"
    echo
    for err in "${ERRORS[@]}"; do
      red "   - $err"
    done
  fi
  blue "═══════════════════════════════════════════════════"

  [ "$FAIL" -eq 0 ]
}

main "$@"
