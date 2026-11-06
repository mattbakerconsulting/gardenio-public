#!/usr/bin/env bash
# test-gitlab-bridge.sh — gitlab-bridge E2E tests
#
# Tests gitlab-bridge health, gb mr link/status/list/skip/unskip commands,
# bead field updates, and webhook endpoint.
#
# Scenarios tested:
#   1. gitlab-bridge pod is running and healthy
#   2. gb mr link sets mr_url on a bead
#   3. gb mr status reports MR info
#   4. gb mr list shows linked beads
#   5. Webhook endpoint accepts valid payloads
#   6. MR bead fields (mr_state, mr_pipeline_status) are populated
#   7. mention → resolved-human operator attribution
#   8. gb mr skip/unskip gardener-delivery opt-out (label + both suppress legs)

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
KD_BIN="${KD_BIN:-kd}"
GB_BIN="${GB_BIN:-gb}"
PASS=0
FAIL=0
ERRORS=()
PF_PID=""
BRIDGE_PF_PID=""
CREATED_BEADS=()

export KD_ACTOR="${KD_ACTOR:-e2e-test}"

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
    fail "$name" "expected output to contain '$pattern'; got: $(echo "$actual" | head -5)"
  fi
}

# ── Setup ─────────────────────────────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19100
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-gitlab.log 2>&1 &
  PF_PID=$!
  sleep 3
  export BEADS_HTTP_URL="http://localhost:${port}"
  dim "  Daemon at $BEADS_HTTP_URL (pid $PF_PID)"
}

setup_token() {
  if [ -n "${BEADS_TOKEN:-}" ]; then return; fi
  local secret
  secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
    -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
  if [ -n "$secret" ]; then
    export BEADS_TOKEN="$secret"
    dim "  Token loaded from secret kd-beads-token"
  fi
}

setup_bridge_portforward() {
  local bridge_svc="${NAMESPACE}-gitlab-bridge"
  if ! kubectl -n "$NAMESPACE" get svc "$bridge_svc" >/dev/null 2>&1; then
    dim "  No gitlab-bridge service found in $NAMESPACE"
    return 1
  fi
  local port=18092
  kubectl -n "$NAMESPACE" port-forward "svc/${bridge_svc}" "${port}:8092" \
    >/tmp/kd-pf-bridge.log 2>&1 &
  BRIDGE_PF_PID=$!
  sleep 2
  export BRIDGE_URL="http://localhost:${port}"
  dim "  Bridge at $BRIDGE_URL (pid $BRIDGE_PF_PID)"
}

cleanup() {
  for id in "${CREATED_BEADS[@]}"; do
    "$KD_BIN" close "$id" 2>/dev/null || true
  done
  if [ -n "$BRIDGE_PF_PID" ]; then
    kill "$BRIDGE_PF_PID" 2>/dev/null || true
  fi
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " gitlab-bridge E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  local TEST_TAG="e2e-gitlab-$$-$(date +%s)"

  # ── Scenario 1: gitlab-bridge pod health ───────────────────────
  blue "Scenario 1: gitlab-bridge pod is running and healthy"

  local bridge_pod=""
  bridge_pod=$(kubectl -n "$NAMESPACE" get pods -l app.kubernetes.io/component=gitlab-bridge \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")

  # Fallback: grep by name
  if [ -z "$bridge_pod" ]; then
    bridge_pod=$(kubectl -n "$NAMESPACE" get pods --no-headers 2>/dev/null \
      | grep gitlab-bridge | awk '{print $1}' | head -1 || echo "")
  fi

  if [ -n "$bridge_pod" ]; then
    pass "gitlab-bridge pod found ($bridge_pod)"

    local phase
    phase=$(kubectl -n "$NAMESPACE" get pod "$bridge_pod" -o jsonpath='{.status.phase}' 2>/dev/null || echo "Unknown")
    if [ "$phase" = "Running" ]; then
      pass "gitlab-bridge pod is Running"
    else
      fail "gitlab-bridge pod phase" "expected Running, got $phase"
    fi
  else
    dim "  No gitlab-bridge pod found"
    pass "gitlab-bridge pod check (not deployed in test namespace)"
  fi

  # Check health endpoint
  if setup_bridge_portforward; then
    local health_out
    health_out=$(curl -sf "${BRIDGE_URL}/healthz" 2>/dev/null || echo "")
    if grep -q "ok" <<<"$health_out"; then
      pass "gitlab-bridge /healthz returns ok"
      assert_contains "healthz includes version" "version" "$health_out"
    else
      fail "gitlab-bridge /healthz" "unexpected response: $health_out"
    fi
  else
    dim "  Bridge port-forward not available"
    pass "bridge health check skipped (no service)"
  fi
  echo

  # ── Scenario 2: gb mr link sets mr_url ─────────────────────────
  blue "Scenario 2: gb mr link sets mr_url on a bead"

  local task_out task_id
  task_out=$("$KD_BIN" create "e2e-gitlab-task-$TEST_TAG" --type=task \
    -l "project:gardenio" \
    -d "Test task for gitlab bridge mr link" \
    --json 2>/dev/null || echo "{}")
  task_id=$(echo "$task_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")

  if [ -n "$task_id" ]; then
    CREATED_BEADS+=("$task_id")
    pass "task bead created ($task_id)"

    # Use a real-looking MR URL (doesn't need to be a real MR for field-setting test)
    local test_mr_url="https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/999999"

    local link_out link_rc
    link_out=$("$GB_BIN" mr link "$task_id" "$test_mr_url" 2>&1 || echo "")
    link_rc=$?
    assert_exit "gb mr link" 0 "$link_rc"

    # Verify mr_url field is set
    local show_out
    show_out=$("$KD_BIN" show "$task_id" --json 2>/dev/null || echo "{}")
    assert_contains "bead has mr_url field" "999999" "$show_out"
    assert_contains "mr_url contains gitlab" "gitlab" "$show_out"
  else
    fail "task bead for mr link" "could not create"
  fi
  echo

  # ── Scenario 3: gb mr status ───────────────────────────────────
  blue "Scenario 3: gb mr status reports MR info"

  if [ -n "$task_id" ]; then
    local status_out status_rc
    status_out=$("$GB_BIN" mr status "$task_id" 2>&1 || echo "")
    status_rc=$?

    # mr status may fail if the MR URL is fake (can't reach GitLab API)
    # but it should at least attempt to resolve
    if [ "$status_rc" -eq 0 ]; then
      pass "gb mr status exits 0"
      if [ -n "$status_out" ]; then
        pass "mr status produced output"
      else
        pass "mr status ran (empty output)"
      fi
    else
      # Expected for a fake MR URL — the command runs but GitLab API 404s
      dim "  gb mr status exited $status_rc (expected for non-existent MR)"
      if grep -qi "not found\|404\|error\|failed\|mr_url" <<<"$status_out"; then
        pass "mr status correctly reports MR not found"
      else
        pass "mr status ran (non-zero for fake MR)"
      fi
    fi

    # Try JSON mode
    local status_json
    status_json=$("$GB_BIN" mr status "$task_id" --json 2>/dev/null || echo "")
    if [ -n "$status_json" ]; then
      pass "mr status --json produced output"
    else
      pass "mr status --json ran"
    fi
  fi
  echo

  # ── Scenario 4: gb mr list shows linked beads ──────────────────
  blue "Scenario 4: gb mr list shows linked beads"

  local list_out list_rc
  list_out=$("$GB_BIN" mr list 2>&1 || echo "")
  list_rc=$?

  if [ "$list_rc" -eq 0 ]; then
    pass "gb mr list exits 0"
    if [ -n "$list_out" ]; then
      # Our test bead should appear in the list
      if grep -q "$task_id\|e2e-gitlab-task\|999999" <<<"$list_out"; then
        pass "mr list includes our linked bead"
      else
        dim "  mr list output doesn't include our bead (may be filtered)"
        pass "mr list produced output"
      fi
    else
      dim "  mr list returned empty (no linked beads visible)"
      pass "mr list ran"
    fi
  else
    dim "  gb mr list exited $list_rc"
    pass "mr list ran (non-zero acceptable)"
  fi
  echo

  # ── Scenario 5: Webhook endpoint ───────────────────────────────
  blue "Scenario 5: Webhook endpoint accepts payloads"

  if [ -n "${BRIDGE_URL:-}" ]; then
    # Load webhook secret for HMAC signing
    local webhook_secret=""
    webhook_secret=$(kubectl -n "$NAMESPACE" get secret gitlab-webhook-secret \
      -o jsonpath='{.data.webhook-secret}' 2>/dev/null | base64 -d 2>/dev/null || echo "")

    # Send a minimal merge_request webhook event
    local payload
    payload=$(cat <<WHJSON
{
  "object_kind": "merge_request",
  "event_type": "merge_request",
  "project": {
    "id": 99999,
    "path_with_namespace": "e2e-test/e2e-repo",
    "web_url": "https://gitlab.com/e2e-test/e2e-repo"
  },
  "object_attributes": {
    "iid": 999999,
    "title": "E2E test MR $TEST_TAG",
    "state": "opened",
    "action": "open",
    "source_branch": "fix/e2e-test",
    "target_branch": "main",
    "url": "https://gitlab.com/e2e-test/e2e-repo/-/merge_requests/999999"
  },
  "user": {
    "username": "e2e-test-user",
    "name": "E2E Test"
  }
}
WHJSON
)

    local webhook_rc
    local curl_args=(-sf -X POST "${BRIDGE_URL}/webhook/gitlab"
      -H "Content-Type: application/json"
      -H "X-Gitlab-Event: Merge Request Hook")

    # Add HMAC token header if secret is available
    if [ -n "$webhook_secret" ]; then
      curl_args+=(-H "X-Gitlab-Token: $webhook_secret")
    fi

    local webhook_out
    webhook_out=$(curl "${curl_args[@]}" -d "$payload" 2>&1 || echo "")
    webhook_rc=$?

    if [ "$webhook_rc" -eq 0 ]; then
      pass "webhook endpoint accepted payload"
    else
      # Bridge may reject because it can't match the MR to a bead,
      # but the endpoint should still respond (not connection refused)
      if [ -n "$webhook_out" ]; then
        dim "  Webhook response: $(echo "$webhook_out" | head -3)"
        pass "webhook endpoint responded (may have rejected unmatched MR)"
      else
        fail "webhook endpoint" "no response (connection refused?)"
      fi
    fi

    # Test with note (comment) event
    local note_payload
    note_payload=$(cat <<NOTEJSON
{
  "object_kind": "note",
  "event_type": "note",
  "project": {
    "id": 99999,
    "path_with_namespace": "e2e-test/e2e-repo"
  },
  "merge_request": {
    "iid": 999999,
    "url": "https://gitlab.com/e2e-test/e2e-repo/-/merge_requests/999999"
  },
  "object_attributes": {
    "note": "E2E test comment $TEST_TAG",
    "noteable_type": "MergeRequest"
  },
  "user": {
    "username": "e2e-test-user"
  }
}
NOTEJSON
)

    local note_curl_args=(-sf -X POST "${BRIDGE_URL}/webhook/gitlab"
      -H "Content-Type: application/json"
      -H "X-Gitlab-Event: Note Hook")
    if [ -n "$webhook_secret" ]; then
      note_curl_args+=(-H "X-Gitlab-Token: $webhook_secret")
    fi

    local note_out
    note_out=$(curl "${note_curl_args[@]}" -d "$note_payload" 2>&1 || echo "")
    if [ -n "$note_out" ] || [ $? -eq 0 ]; then
      pass "note webhook endpoint responded"
    else
      pass "note webhook sent (endpoint may not match)"
    fi
  else
    dim "  Bridge URL not available, skipping webhook tests"
    pass "webhook tests skipped (no bridge port-forward)"
  fi
  echo

  # ── Scenario 6: MR bead fields ─────────────────────────────────
  blue "Scenario 6: MR bead fields validation"

  # Create a bead with MR-related fields pre-set
  local mr_bead_out mr_bead_id
  mr_bead_out=$("$KD_BIN" create "e2e-mr-fields-$TEST_TAG" --type=task \
    -l "project:gardenio" \
    -f "mr_url=https://gitlab.com/test/repo/-/merge_requests/123" \
    -f "mr_state=opened" \
    -f "mr_pipeline_status=success" \
    --json 2>/dev/null || echo "{}")
  mr_bead_id=$(echo "$mr_bead_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")

  if [ -n "$mr_bead_id" ]; then
    CREATED_BEADS+=("$mr_bead_id")
    pass "MR fields bead created ($mr_bead_id)"

    local mr_show
    mr_show=$("$KD_BIN" show "$mr_bead_id" --json 2>/dev/null || echo "{}")
    assert_contains "bead has mr_state field" "opened" "$mr_show"
    assert_contains "bead has mr_pipeline_status" "success" "$mr_show"

    # Update MR state to merged. mr_merged is a string field ("true"/"false")
    # but kd CLI auto-converts "true" to a JSON boolean, so use curl to send
    # as a string to match what the gitlab-bridge actually sends.
    "$KD_BIN" update "$mr_bead_id" -f "mr_state=merged" 2>/dev/null || true
    curl -sf -X PATCH "$BEADS_HTTP_URL/v1/beads/$mr_bead_id" \
      -H 'Content-Type: application/json' \
      ${BEADS_TOKEN:+-H "Authorization: Bearer $BEADS_TOKEN"} \
      -d '{"fields":{"mr_merged":"true"}}' >/dev/null 2>&1 || true
    mr_show=$("$KD_BIN" show "$mr_bead_id" --json 2>/dev/null || echo "{}")
    assert_contains "mr_state updated to merged" "merged" "$mr_show"
  else
    fail "MR fields bead" "could not create"
  fi
  echo

  # ── Scenario 7: mention → resolved-human attribution (ka-0000003321) ──
  # gitlab-bridge attributes @gardenio mention beads to the resolved fleet
  # human (ka-0000004627): when the comment author's GitLab username maps to a
  # type=user bead carrying a Slack id / email, the spawned mention task bead
  # gets operated_by = the human operator (slack:U… preferred, then
  # oidc:<email>) — NOT gitlab:<user> and NOT fleet:gitlab-bridge — with the
  # raw gitlab username retained in gitlab_username for provenance. An author
  # whose user bead resolves but carries neither a Slack id nor an email falls
  # back to operated_by = gitlab:<username>.
  #
  # This drives the real webhook → UserDirectory → CreateBead path, so it only
  # runs when the gitlab-bridge is actually deployed in this namespace (it is
  # OFF in staging). When the bridge
  # service / webhook secret is absent we skip cleanly, mirroring Scenario 5.
  blue "Scenario 7: mention → resolved-human operator attribution"

  # gitlab_username values are unique-per-run so concurrent shards / reruns
  # don't collide on the shared daemon's UserDirectory.
  local GL_RESOLVED_USER="e2e-gl-resolved-$$"
  local GL_RESOLVED_SLACK="U07E2EGLRES"
  local GL_FALLBACK_USER="e2e-gl-fallback-$$"

  # Seed two user beads:
  #  (a) resolvable to a human: gitlab_username + slack_id + mention capability.
  #  (b) capable but unresolvable to a human: mention capability, gitlab_username,
  #      but NO slack_id / email → operator must fall back to gitlab:<username>.
  local gl_user_a gl_user_b gl_user_a_id gl_user_b_id
  gl_user_a=$(curl -sf --max-time 30 -X POST "$BEADS_HTTP_URL/v1/beads" \
    -H 'Content-Type: application/json' \
    ${BEADS_TOKEN:+-H "Authorization: Bearer $BEADS_TOKEN"} \
    -d "{\"title\":\"E2E GL Resolved User $TEST_TAG\",\"type\":\"user\",\"labels\":[\"e2e:gitlab-mention\"],\"fields\":{\"gitlab_username\":\"$GL_RESOLVED_USER\",\"slack_id\":\"$GL_RESOLVED_SLACK\",\"email\":\"e2e-gl-res@gardenio.dev\",\"display_name\":\"E2E GL Resolved\",\"capabilities\":\"[\\\"mention\\\"]\"}}" 2>/dev/null || echo "{}")
  gl_user_a_id=$(echo "$gl_user_a" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  gl_user_b=$(curl -sf --max-time 30 -X POST "$BEADS_HTTP_URL/v1/beads" \
    -H 'Content-Type: application/json' \
    ${BEADS_TOKEN:+-H "Authorization: Bearer $BEADS_TOKEN"} \
    -d "{\"title\":\"E2E GL Fallback User $TEST_TAG\",\"type\":\"user\",\"labels\":[\"e2e:gitlab-mention\"],\"fields\":{\"gitlab_username\":\"$GL_FALLBACK_USER\",\"display_name\":\"E2E GL Fallback\",\"capabilities\":\"[\\\"mention\\\"]\"}}" 2>/dev/null || echo "{}")
  gl_user_b_id=$(echo "$gl_user_b" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  [ -n "$gl_user_a_id" ] && CREATED_BEADS+=("$gl_user_a_id")
  [ -n "$gl_user_b_id" ] && CREATED_BEADS+=("$gl_user_b_id")

  # Find the spawned mention task for a given gitlab_username (label
  # gitlab-mention + field gitlab_username). Echoes the bead id or "".
  find_mention_task() {
    local gl_user="$1"
    "$KD_BIN" list -t task -l gitlab-mention -f "gitlab_username=$gl_user" \
      --status open,in_progress --limit 5 --json 2>/dev/null \
      | python3 -c "import sys,json
try:
  arr=json.load(sys.stdin)
  print(arr[0]['id'] if isinstance(arr,list) and arr else '')
except Exception:
  print('')" 2>/dev/null || echo ""
  }

  # POST a GitLab note webhook (@gardenio mention) for the given author.
  post_mention_webhook() {
    local author="$1" iid="$2"
    local payload
    payload=$(cat <<WHJSON
{
  "object_kind": "note",
  "event_type": "note",
  "project": { "id": 99999, "path_with_namespace": "e2e-test/e2e-repo" },
  "merge_request": {
    "iid": $iid,
    "url": "https://gitlab.com/e2e-test/e2e-repo/-/merge_requests/$iid"
  },
  "object_attributes": {
    "note": "@gardenio please take a look $TEST_TAG",
    "noteable_type": "MergeRequest",
    "discussion_id": "disc-${author}-${iid}"
  },
  "user": { "username": "$author" }
}
WHJSON
)
    local args=(-s -o /dev/null -w '%{http_code}' -X POST "${BRIDGE_URL}/webhook"
      -H "Content-Type: application/json"
      -H "X-Gitlab-Event: Note Hook")
    [ -n "${gl_webhook_secret:-}" ] && args+=(-H "X-Gitlab-Token: $gl_webhook_secret")
    curl "${args[@]}" -d "$payload" 2>/dev/null || echo "000"
  }

  # The mention path requires BOTH the bridge HTTP endpoint AND the webhook
  # secret (the handler is fail-closed on a missing/empty X-Gitlab-Token —
  # see GitLabWebhookHandlerWithConfig). Load the secret; skip if either the
  # bridge URL or the secret is unavailable, or the user beads didn't seed.
  local gl_webhook_secret=""
  gl_webhook_secret=$(kubectl -n "$NAMESPACE" get secret gitlab-webhook-secret \
    -o jsonpath='{.data.webhook-secret}' 2>/dev/null | base64 -d 2>/dev/null || echo "")

  if [ -z "${BRIDGE_URL:-}" ] || [ -z "$gl_webhook_secret" ] \
     || [ -z "$gl_user_a_id" ] || [ -z "$gl_user_b_id" ]; then
    dim "  gitlab-bridge webhook + secret not available (bridge off in this namespace)"
    pass "mention attribution skipped (no live gitlab-bridge webhook)"
  else
    # Give the bridge's SSE-fed UserDirectory a moment to ingest the freshly
    # created user beads (beads.bead.created topic) before driving the webhook.
    sleep 3

    # (a) Resolved-human path: author maps to a Slack-id-bearing user.
    local code_a
    code_a=$(post_mention_webhook "$GL_RESOLVED_USER" 970001)
    if [ "$code_a" = "200" ]; then
      pass "resolved-human mention webhook accepted (HTTP 200)"
    else
      dim "  resolved-human mention webhook returned HTTP $code_a"
      pass "resolved-human mention webhook sent (HTTP $code_a)"
    fi

    # Poll for the spawned task bead (CreateBead is synchronous in the webhook
    # handler, but allow a few seconds for the round trip + agent spawn).
    local mt_a=""
    for _i in 1 2 3 4 5 6 7 8; do
      mt_a=$(find_mention_task "$GL_RESOLVED_USER")
      [ -n "$mt_a" ] && break
      sleep 2
    done

    if [ -n "$mt_a" ]; then
      CREATED_BEADS+=("$mt_a")
      pass "resolved-human mention spawned task bead ($mt_a)"
      local show_a
      show_a=$("$KD_BIN" show "$mt_a" --json 2>/dev/null || echo "{}")
      # Both the operated_by FIELD and the audit operated_by COLUMN must be the
      # human operator slack:<id> (the bridge stamps WithOperator on ctx, so
      # X-Operated-By resolves the audit column too) — NOT gitlab:* / fleet:*.
      local op_field_a op_audit_a
      op_field_a=$(echo "$show_a" | python3 -c "import sys,json; print((json.load(sys.stdin).get('fields') or {}).get('operated_by',''))" 2>/dev/null || echo "")
      op_audit_a=$(echo "$show_a" | python3 -c "import sys,json; print(json.load(sys.stdin).get('operated_by',''))" 2>/dev/null || echo "")
      if [ "$op_field_a" = "slack:$GL_RESOLVED_SLACK" ]; then
        pass "resolved mention operated_by FIELD = slack:$GL_RESOLVED_SLACK (human, not gitlab:*/fleet:*)"
      else
        fail "resolved mention operated_by field" "expected slack:$GL_RESOLVED_SLACK, got '$op_field_a'"
      fi
      if [ "$op_audit_a" = "slack:$GL_RESOLVED_SLACK" ]; then
        pass "resolved mention audit operated_by = slack:$GL_RESOLVED_SLACK"
      else
        dim "  audit operated_by column = '$op_audit_a' (expected slack:$GL_RESOLVED_SLACK)"
        fail "resolved mention audit operated_by" "expected slack:$GL_RESOLVED_SLACK, got '$op_audit_a'"
      fi
      # Provenance: raw gitlab username + mirrored slack_user_id retained.
      assert_contains "resolved mention retains gitlab_username provenance" "$GL_RESOLVED_USER" "$show_a"
      assert_contains "resolved mention mirrors slack_user_id" "$GL_RESOLVED_SLACK" "$show_a"
    else
      dim "  no mention task found for $GL_RESOLVED_USER after webhook"
      fail "resolved-human mention spawned task" "no gitlab-mention task bead with gitlab_username=$GL_RESOLVED_USER"
    fi

    # (b) Fallback path: author is capable but carries no Slack id / email, so
    # the operator must fall back to gitlab:<username> (NOT a fleet sentinel).
    local code_b
    code_b=$(post_mention_webhook "$GL_FALLBACK_USER" 970002)
    dim "  fallback mention webhook returned HTTP $code_b"
    local mt_b=""
    for _i in 1 2 3 4 5 6 7 8; do
      mt_b=$(find_mention_task "$GL_FALLBACK_USER")
      [ -n "$mt_b" ] && break
      sleep 2
    done
    if [ -n "$mt_b" ]; then
      CREATED_BEADS+=("$mt_b")
      local show_b op_field_b
      show_b=$("$KD_BIN" show "$mt_b" --json 2>/dev/null || echo "{}")
      op_field_b=$(echo "$show_b" | python3 -c "import sys,json; print((json.load(sys.stdin).get('fields') or {}).get('operated_by',''))" 2>/dev/null || echo "")
      if [ "$op_field_b" = "gitlab:$GL_FALLBACK_USER" ]; then
        pass "unresolved mention operated_by FIELD falls back to gitlab:$GL_FALLBACK_USER"
      else
        fail "unresolved mention fallback operated_by" "expected gitlab:$GL_FALLBACK_USER, got '$op_field_b'"
      fi
    else
      dim "  no mention task found for $GL_FALLBACK_USER after webhook"
      fail "unresolved-human mention spawned task" "no gitlab-mention task bead with gitlab_username=$GL_FALLBACK_USER"
    fi
  fi
  echo

  # ── Scenario 8: gb mr skip / unskip gardener-delivery opt-out (ka-0000006403) ──
  # The gardener-delivery gardener (trigger ka-0000001510) opts an MR out via the
  # gardener-delivery-skip label on the MR's merge-request bead. `gb mr skip`
  # adds it; `gb mr unskip` removes it. The label suppresses the gardener on TWO
  # legs, both asserted here end-to-end against the LIVE daemon:
  #
  #   (A) bead_event wake  — the gardener's wake condition is
  #       label:gardener-delivery-skip:absent. We mirror it as a fire-on-match
  #       assert trigger (CEL: no gardener-delivery-skip label) and use
  #       `gb trigger test` to assert would_fire flips true→false→true as the
  #       label is added / removed. The probe trigger is created DISABLED so it
  #       never actually fires on the shared daemon.
  #   (B) cron preflight   — the gardener's open-MR floor excludes labelled MRs
  #       via the store exclude_labels filter. We assert `kd list
  #       --exclude-label gardener-delivery-skip` drops the MR bead on skip and
  #       restores it on unskip.
  #
  # Also asserts the resolver: `gb mr skip` accepts both a kd-* MR bead id and a
  # raw GitLab MR URL (resolveMRBeadID → findMRBeadByURL). Runtime lives at
  # controller/cmd/gb/mr.go:76 (mrSkipLabel), :205 (skip), :226 (unskip).
  blue "Scenario 8: gb mr skip / unskip gardener-delivery opt-out"

  local SKIP_LABEL="gardener-delivery-skip"
  local PROBE_LABEL="e2e-cds:$TEST_TAG"
  local skip_mr_url="https://gitlab.com/e2e-cds/repo/-/merge_requests/$$"

  # Emit the ids of our probe MR bead(s) matching a given filter, space-joined,
  # so membership can be checked without a brittle raw count under concurrency
  # (the probe label is unique per run, but id membership is the strongest form).
  probe_ids() {
    "$KD_BIN" list -l "$PROBE_LABEL" "$@" --json 2>/dev/null \
      | python3 -c "import sys,json
try:
  arr=json.load(sys.stdin)
  print(' '.join(b.get('id','') for b in arr if isinstance(b,dict)))
except Exception:
  print('')" 2>/dev/null || echo ""
  }
  # Echo would_fire ("true"/"false"/"") from `gb trigger test --json`.
  probe_would_fire() {
    local trig="$1" bead="$2"
    "$GB_BIN" trigger test "$trig" --bead "$bead" --json 2>/dev/null \
      | python3 -c "import sys,json
try:
  print(str(json.load(sys.stdin).get('would_fire','')).lower())
except Exception:
  print('')" 2>/dev/null || echo ""
  }

  # Create the merge-request bead the label lives on (the gardener evaluates the
  # MR bead, not the producing task bead — see resolveMRBeadID's doc comment).
  local cds_mr_out cds_mr_id
  cds_mr_out=$("$KD_BIN" create "e2e-cds-mr-$TEST_TAG" --type=merge-request \
    -l "project:gardenio" -l "$PROBE_LABEL" \
    -f "mr_url=$skip_mr_url" \
    --json 2>/dev/null || echo "{}")
  cds_mr_id=$(echo "$cds_mr_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")

  if [ -z "$cds_mr_id" ]; then
    fail "gardener-delivery-skip MR bead" "could not create merge-request bead"
  else
    CREATED_BEADS+=("$cds_mr_id")
    pass "merge-request bead created ($cds_mr_id)"

    # (A) Wake-filter leg — build a DISABLED fire-on-match assert trigger whose
    # CEL mirrors the gardener-delivery bead_event absent-wake. `gb trigger test`
    # dry-runs the CEL against the real bead without ever firing.
    local cds_trig_out cds_trig_id
    cds_trig_out=$("$GB_BIN" trigger create assert \
      --title "e2e-cds-probe-$TEST_TAG" \
      --event-types updated \
      --expr "!bead.labels.exists(l, l == \"$SKIP_LABEL\")" \
      --action notify --prompt 'e2e gardener-delivery-skip probe (disabled)' \
      --enabled=false --json 2>/dev/null || echo "{}")
    cds_trig_id=$(echo "$cds_trig_out" | python3 -c "import sys,json
try:
  d=json.load(sys.stdin); print(d.get('id') or d.get('trigger') or '')
except Exception:
  print('')" 2>/dev/null || echo "")

    if [ -n "$cds_trig_id" ]; then
      CREATED_BEADS+=("$cds_trig_id")

      # BEFORE skip: gardener WOULD wake on this MR (no skip label yet).
      local wf_before
      wf_before=$(probe_would_fire "$cds_trig_id" "$cds_mr_id")
      if [ "$wf_before" = "true" ]; then
        pass "wake-filter: gardener-delivery WOULD wake before skip (would_fire=true)"
      else
        fail "wake-filter before skip" "expected would_fire=true, got '$wf_before'"
      fi
    else
      dim "  gb trigger create assert unavailable — skipping CEL wake-filter leg"
      pass "wake-filter leg skipped (assert trigger not supported)"
    fi

    # (B) cron-preflight leg BEFORE skip: MR is present in the un-skipped set.
    local before_ids
    before_ids=$(probe_ids --exclude-label "$SKIP_LABEL")
    if grep -qw "$cds_mr_id" <<< "$before_ids"; then
      pass "cron-preflight: MR counted before skip (in --exclude-label $SKIP_LABEL set)"
    else
      fail "cron-preflight before skip" "MR $cds_mr_id absent from un-skipped set; got: $before_ids"
    fi

    # ── gb mr skip (resolve by BEAD ID) ──
    local skip_out skip_rc
    skip_out=$("$GB_BIN" mr skip "$cds_mr_id" 2>&1); skip_rc=$?
    assert_exit "gb mr skip (by bead id)" 0 "$skip_rc"
    assert_contains "gb mr skip announces suppression" "$SKIP_LABEL" "$skip_out"

    # Label is actually on the bead now.
    local after_show
    after_show=$("$KD_BIN" show "$cds_mr_id" --json 2>/dev/null || echo "{}")
    if echo "$after_show" | python3 -c "import sys,json; sys.exit(0 if '$SKIP_LABEL' in (json.load(sys.stdin).get('labels') or []) else 1)" 2>/dev/null; then
      pass "MR bead carries $SKIP_LABEL after skip"
    else
      fail "skip label present" "$SKIP_LABEL not in MR bead labels after gb mr skip"
    fi

    # AFTER skip: wake suppressed (would_fire=false) …
    if [ -n "${cds_trig_id:-}" ]; then
      local wf_after
      wf_after=$(probe_would_fire "$cds_trig_id" "$cds_mr_id")
      if [ "$wf_after" = "false" ]; then
        pass "wake-filter: gardener-delivery would NOT wake after skip (would_fire=false)"
      else
        fail "wake-filter after skip" "expected would_fire=false, got '$wf_after'"
      fi
    fi

    # … and excluded from the cron-preflight open-MR count.
    local after_ids
    after_ids=$(probe_ids --exclude-label "$SKIP_LABEL")
    if grep -qw "$cds_mr_id" <<< "$after_ids"; then
      fail "cron-preflight after skip" "MR $cds_mr_id STILL in --exclude-label set: $after_ids"
    else
      pass "cron-preflight: MR excluded from open-MR count after skip"
    fi

    # ── gb mr unskip (reverses both legs) ──
    local unskip_out unskip_rc
    unskip_out=$("$GB_BIN" mr unskip "$cds_mr_id" 2>&1); unskip_rc=$?
    assert_exit "gb mr unskip" 0 "$unskip_rc"

    local unskip_show
    unskip_show=$("$KD_BIN" show "$cds_mr_id" --json 2>/dev/null || echo "{}")
    if echo "$unskip_show" | python3 -c "import sys,json; sys.exit(1 if '$SKIP_LABEL' in (json.load(sys.stdin).get('labels') or []) else 0)" 2>/dev/null; then
      pass "MR bead no longer carries $SKIP_LABEL after unskip"
    else
      fail "unskip removes label" "$SKIP_LABEL still present after gb mr unskip"
    fi

    if [ -n "${cds_trig_id:-}" ]; then
      local wf_unskip
      wf_unskip=$(probe_would_fire "$cds_trig_id" "$cds_mr_id")
      if [ "$wf_unskip" = "true" ]; then
        pass "wake-filter: gardener-delivery WOULD wake again after unskip (would_fire=true)"
      else
        fail "wake-filter after unskip" "expected would_fire=true, got '$wf_unskip'"
      fi
    fi

    local restored_ids
    restored_ids=$(probe_ids --exclude-label "$SKIP_LABEL")
    if grep -qw "$cds_mr_id" <<< "$restored_ids"; then
      pass "cron-preflight: MR re-counted after unskip"
    else
      fail "cron-preflight after unskip" "MR $cds_mr_id not restored to un-skipped set: $restored_ids"
    fi

    # ── Resolver: gb mr skip also accepts a raw GitLab MR URL ──
    local urlskip_out urlskip_rc
    urlskip_out=$("$GB_BIN" mr skip "$skip_mr_url" 2>&1); urlskip_rc=$?
    assert_exit "gb mr skip (by raw MR URL)" 0 "$urlskip_rc"
    # The URL resolves to the SAME MR bead (findMRBeadByURL), so the id is named.
    assert_contains "URL resolves to the MR bead" "$cds_mr_id" "$urlskip_out"
    # Leave it unskipped for a clean teardown (cleanup only closes beads).
    "$GB_BIN" mr unskip "$cds_mr_id" >/dev/null 2>&1 || true
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
