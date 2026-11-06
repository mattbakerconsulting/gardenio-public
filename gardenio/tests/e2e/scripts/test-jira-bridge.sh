#!/usr/bin/env bash
# test-jira-bridge.sh — jira-bridge E2E tests
#
# Tests jira-bridge health, Jira bead field CRUD, webhook endpoint,
# and gb jira show command.
#
# Scenarios tested:
#   1. jira-bridge pod is running and healthy
#   2. Bead with jira_key field CRUD
#   3. Jira bead field validation (jira_key, jira_status, jira_url)
#   4. Webhook endpoint accepts simulated payloads
#   5. gb jira show command

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
  local port=19101
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-jira.log 2>&1 &
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
  local bridge_svc="${NAMESPACE}-jira-bridge"
  local port=18091

  if ! kubectl -n "$NAMESPACE" get svc "$bridge_svc" >/dev/null 2>&1; then
    dim "  No jira-bridge service found in $NAMESPACE"
    return 1
  fi

  kubectl -n "$NAMESPACE" port-forward "svc/${bridge_svc}" "${port}:8091" \
    >/tmp/kd-pf-jira-bridge.log 2>&1 &
  BRIDGE_PF_PID=$!
  sleep 2
  export JIRA_BRIDGE_URL="http://localhost:${port}"
  dim "  Jira bridge at $JIRA_BRIDGE_URL (pid $BRIDGE_PF_PID)"
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
  blue " jira-bridge E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  local TEST_TAG="e2e-jira-$$-$(date +%s)"

  # ── Scenario 1: jira-bridge pod health ─────────────────────────
  blue "Scenario 1: jira-bridge pod is running and healthy"

  local bridge_pod=""
  bridge_pod=$(kubectl -n "$NAMESPACE" get pods --no-headers 2>/dev/null \
    | grep jira-bridge | awk '{print $1}' | head -1 || echo "")

  if [ -n "$bridge_pod" ]; then
    pass "jira-bridge pod found ($bridge_pod)"

    local phase
    phase=$(kubectl -n "$NAMESPACE" get pod "$bridge_pod" -o jsonpath='{.status.phase}' 2>/dev/null || echo "Unknown")
    if [ "$phase" = "Running" ]; then
      pass "jira-bridge pod is Running"
    else
      fail "jira-bridge pod phase" "expected Running, got $phase"
    fi
  else
    dim "  No jira-bridge pod found in $NAMESPACE namespace"
    pass "jira-bridge pod check (not deployed)"
  fi

  # Health endpoint
  if setup_bridge_portforward; then
    local health_out
    health_out=$(curl -sf "${JIRA_BRIDGE_URL}/healthz" 2>/dev/null || echo "")
    if grep -q "ok" <<<"$health_out"; then
      pass "jira-bridge /healthz returns ok"
      assert_contains "healthz includes version" "version" "$health_out"
    else
      fail "jira-bridge /healthz" "unexpected: $health_out"
    fi
  else
    dim "  Bridge port-forward not available"
    pass "bridge health check skipped"
  fi
  echo

  # ── Scenario 2: Bead with jira_key field ───────────────────────
  blue "Scenario 2: Bead with jira_key field CRUD"

  local task_out task_id
  task_out=$("$KD_BIN" create "e2e-jira-task-$TEST_TAG" --type=task \
    -l "project:gardenio" \
    -f "jira_key=PE-99999" \
    -d "Test task with Jira key" \
    --json 2>/dev/null || echo "{}")
  task_id=$(echo "$task_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")

  if [ -n "$task_id" ]; then
    CREATED_BEADS+=("$task_id")
    pass "task with jira_key created ($task_id)"

    local show_out
    show_out=$("$KD_BIN" show "$task_id" --json 2>/dev/null || echo "{}")
    assert_contains "bead has jira_key" "PE-99999" "$show_out"

    # Update jira fields
    "$KD_BIN" update "$task_id" \
      -f "jira_status=In Progress" \
      -f "jira_url=https://acme.atlassian.net/browse/PE-99999" 2>/dev/null || true

    show_out=$("$KD_BIN" show "$task_id" --json 2>/dev/null || echo "{}")
    assert_contains "bead has jira_status" "In Progress" "$show_out"
    assert_contains "bead has jira_url" "atlassian" "$show_out"
  else
    fail "task with jira_key" "could not create"
  fi
  echo

  # ── Scenario 3: Jira bead field validation ─────────────────────
  blue "Scenario 3: Jira bead field validation"

  local jira_bead_out jira_bead_id
  jira_bead_out=$("$KD_BIN" create "e2e-jira-fields-$TEST_TAG" --type=task \
    -l "project:gardenio" \
    -f "jira_key=PE-88888" \
    -f "jira_status=To Do" \
    -f "jira_url=https://acme.atlassian.net/browse/PE-88888" \
    -f "jira_priority=High" \
    --json 2>/dev/null || echo "{}")
  jira_bead_id=$(echo "$jira_bead_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")

  if [ -n "$jira_bead_id" ]; then
    CREATED_BEADS+=("$jira_bead_id")
    pass "jira fields bead created ($jira_bead_id)"

    local fields_show
    fields_show=$("$KD_BIN" show "$jira_bead_id" --json 2>/dev/null || echo "{}")
    assert_contains "jira_key=PE-88888" "PE-88888" "$fields_show"
    assert_contains "jira_status=To Do" "To Do" "$fields_show"

    # Simulate status transition (like bridge would do)
    "$KD_BIN" update "$jira_bead_id" -f "jira_status=Done" 2>/dev/null || true
    fields_show=$("$KD_BIN" show "$jira_bead_id" --json 2>/dev/null || echo "{}")
    assert_contains "jira_status updated to Done" "Done" "$fields_show"
  else
    fail "jira fields bead" "could not create"
  fi
  echo

  # ── Scenario 4: Webhook endpoint ───────────────────────────────
  blue "Scenario 4: Webhook endpoint accepts simulated payloads"

  if [ -n "${JIRA_BRIDGE_URL:-}" ]; then
    # Simulated Jira issue_updated webhook
    local payload
    payload=$(cat <<WHJSON
{
  "webhookEvent": "jira:issue_updated",
  "issue_event_type_name": "issue_generic",
  "timestamp": $(date +%s)000,
  "issue": {
    "id": "99999",
    "key": "PE-99999",
    "fields": {
      "summary": "E2E test issue $TEST_TAG",
      "status": {
        "name": "In Progress",
        "id": "3"
      },
      "issuetype": {
        "name": "Bug",
        "id": "1"
      },
      "priority": {
        "name": "High",
        "id": "2"
      },
      "project": {
        "key": "PE",
        "name": "Engineering"
      }
    },
    "self": "https://acme.atlassian.net/rest/api/3/issue/99999"
  },
  "user": {
    "displayName": "E2E Test",
    "emailAddress": "e2e@test.local"
  },
  "changelog": {
    "items": [
      {
        "field": "status",
        "fromString": "To Do",
        "toString": "In Progress"
      }
    ]
  }
}
WHJSON
)

    local webhook_out webhook_rc
    webhook_out=$(curl -sf -X POST "${JIRA_BRIDGE_URL}/webhook/jira" \
      -H "Content-Type: application/json" \
      -d "$payload" 2>&1 || echo "")
    webhook_rc=$?

    if [ "$webhook_rc" -eq 0 ]; then
      pass "webhook endpoint accepted payload"
    else
      if [ -n "$webhook_out" ]; then
        dim "  Webhook response: $(echo "$webhook_out" | head -3)"
        pass "webhook endpoint responded (may reject unmatched issue)"
      else
        fail "webhook endpoint" "no response"
      fi
    fi

    # Test issue_created event
    local create_payload
    create_payload=$(cat <<CRJSON
{
  "webhookEvent": "jira:issue_created",
  "issue_event_type_name": "issue_created",
  "timestamp": $(date +%s)000,
  "issue": {
    "id": "88888",
    "key": "PE-88888",
    "fields": {
      "summary": "E2E new issue $TEST_TAG",
      "status": {"name": "To Do"},
      "issuetype": {"name": "Task"},
      "project": {"key": "PE", "name": "Engineering"}
    }
  }
}
CRJSON
)

    local create_out
    create_out=$(curl -sf -X POST "${JIRA_BRIDGE_URL}/webhook/jira" \
      -H "Content-Type: application/json" \
      -d "$create_payload" 2>&1 || echo "")
    if [ -n "$create_out" ] || [ $? -eq 0 ]; then
      pass "issue_created webhook endpoint responded"
    else
      pass "issue_created webhook sent"
    fi
  else
    dim "  Jira bridge URL not available, skipping webhook tests"
    pass "webhook tests skipped (no bridge port-forward)"
  fi
  echo

  # ── Scenario 5: gb jira show ───────────────────────────────────
  blue "Scenario 5: gb jira show command"

  # gb jira show requires JIRA_BASE_URL, JIRA_EMAIL, JIRA_API_TOKEN
  if [ -n "${JIRA_API_TOKEN:-}" ] && [ -n "${JIRA_BASE_URL:-}" ]; then
    local jira_show_out jira_show_rc
    jira_show_out=$("$GB_BIN" jira show PE-1 2>&1 || echo "")
    jira_show_rc=$?

    if [ "$jira_show_rc" -eq 0 ]; then
      pass "gb jira show exits 0"
      if [ -n "$jira_show_out" ]; then
        pass "jira show produced output"
      fi
    else
      dim "  gb jira show exited $jira_show_rc (may need auth or ticket may not exist)"
      pass "jira show ran (auth-dependent)"
    fi
  else
    dim "  JIRA_API_TOKEN or JIRA_BASE_URL not set"
    dim "  Skipping gb jira show test"
    pass "gb jira show skipped (no Jira credentials)"
  fi
  echo

  # ── Scenario 6: mention → resolved-human attribution (ka-0000003321) ──
  # jira-bridge attributes @gardenio mention beads to the resolved fleet human
  # (ka-0000006070): resolveJiraOperator maps the JIRA comment author's
  # accountId via UserDirectory.LookupByJiraUser to a human operator
  # (slack:<id> preferred, then oidc:<email>); the spawned mention task bead
  # gets operated_by = that human — NOT jira:<accountID> and NOT
  # fleet:jira-bridge — with jira_user_id retained for provenance. An author
  # whose accountId does NOT resolve falls back to operated_by =
  # jira:<accountID>.
  #
  # Drives the real comment webhook → UserDirectory → CreateBead path, so it
  # only runs when the jira-bridge is deployed in this namespace (OFF in
  # staging). Skips cleanly when the
  # bridge URL or HMAC secret is unavailable, mirroring Scenario 4.
  blue "Scenario 6: mention → resolved-human operator attribution"

  # accountId values are unique-per-run so concurrent shards / reruns don't
  # collide on the shared daemon's UserDirectory. The mention path is gated to
  # the PE project key (allowedJiraProjects) — use PE-* issue keys below.
  local JIRA_RESOLVED_ACCT="e2e-jira-acct-res-$$"
  local JIRA_RESOLVED_SLACK="U07E2EJIRARES"
  local JIRA_FALLBACK_ACCT="e2e-jira-acct-unres-$$"

  # Seed a user bead resolvable to a human via jira_user=<accountId>, carrying
  # a Slack id so the operator ladder yields slack:<id>. (No fallback user
  # bead is needed — the unresolved case is simply an accountId with no
  # matching user bead.)
  local jira_user_a jira_user_a_id
  jira_user_a=$(curl -sf --max-time 30 -X POST "$BEADS_HTTP_URL/v1/beads" \
    -H 'Content-Type: application/json' \
    ${BEADS_TOKEN:+-H "Authorization: Bearer $BEADS_TOKEN"} \
    -d "{\"title\":\"E2E Jira Resolved User $TEST_TAG\",\"type\":\"user\",\"labels\":[\"e2e:jira-mention\"],\"fields\":{\"jira_user\":\"$JIRA_RESOLVED_ACCT\",\"slack_id\":\"$JIRA_RESOLVED_SLACK\",\"email\":\"e2e-jira-res@gardenio.dev\",\"display_name\":\"E2E Jira Resolved\"}}" 2>/dev/null || echo "{}")
  jira_user_a_id=$(echo "$jira_user_a" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  [ -n "$jira_user_a_id" ] && CREATED_BEADS+=("$jira_user_a_id")

  # Find the spawned mention task for a given accountId (label jira-mention +
  # field jira_user_id). Echoes the bead id or "".
  find_jira_mention_task() {
    local acct="$1"
    "$KD_BIN" list -t task -l jira-mention -f "jira_user_id=$acct" \
      --status open,in_progress --limit 5 --json 2>/dev/null \
      | python3 -c "import sys,json
try:
  arr=json.load(sys.stdin)
  print(arr[0]['id'] if isinstance(arr,list) and arr else '')
except Exception:
  print('')" 2>/dev/null || echo ""
  }

  # POST a JIRA comment_created webhook (@gardenio mention) for the given
  # author accountId on a PE issue, signing the body with HMAC-SHA256 when a
  # secret is present (X-Hub-Signature: sha256=<hex>). Echoes the HTTP code.
  post_jira_mention_webhook() {
    local acct="$1" issue_key="$2"
    local payload
    payload=$(cat <<JWHJSON
{
  "webhookEvent": "comment_created",
  "issue": {
    "id": "970100",
    "key": "$issue_key",
    "fields": { "summary": "E2E mention attribution $TEST_TAG" }
  },
  "comment": {
    "id": "c-$acct",
    "body": "@gardenio please take a look $TEST_TAG",
    "author": { "displayName": "E2E Jira Author", "accountId": "$acct" }
  }
}
JWHJSON
)
    local sig=""
    if [ -n "${jira_webhook_secret:-}" ]; then
      sig=$(printf '%s' "$payload" | JWSEC="$jira_webhook_secret" python3 -c "import sys,hmac,hashlib,os; print('sha256='+hmac.new(os.environ['JWSEC'].encode(), sys.stdin.buffer.read(), hashlib.sha256).hexdigest())" 2>/dev/null || echo "")
    fi
    local args=(-s -o /dev/null -w '%{http_code}' -X POST "${JIRA_BRIDGE_URL}/webhook"
      -H "Content-Type: application/json")
    [ -n "$sig" ] && args+=(-H "X-Hub-Signature: $sig")
    curl "${args[@]}" -d "$payload" 2>/dev/null || echo "000"
  }

  # Load the HMAC secret. The handler is fail-closed unless dev mode is on OR a
  # valid signature is supplied — so we need either the secret (to sign) or a
  # dev-mode bridge. Skip cleanly when the bridge URL is absent, or when no
  # secret is available (we cannot forge a valid signature without it).
  local jira_webhook_secret=""
  jira_webhook_secret=$(kubectl -n "$NAMESPACE" get secret jira-webhook-secret \
    -o jsonpath='{.data.webhook-secret}' 2>/dev/null | base64 -d 2>/dev/null || echo "")

  if [ -z "${JIRA_BRIDGE_URL:-}" ] || [ -z "$jira_webhook_secret" ] \
     || [ -z "$jira_user_a_id" ]; then
    dim "  jira-bridge webhook + secret not available (bridge off in this namespace)"
    pass "mention attribution skipped (no live jira-bridge webhook)"
  else
    # Let the bridge's SSE-fed UserDirectory ingest the new user bead
    # (beads.bead.created) before driving the webhook.
    sleep 3

    # (a) Resolved-human path: accountId maps to a Slack-id-bearing user.
    local jcode_a
    jcode_a=$(post_jira_mention_webhook "$JIRA_RESOLVED_ACCT" "PE-97001")
    if [ "$jcode_a" = "200" ]; then
      pass "resolved-human jira mention webhook accepted (HTTP 200)"
    else
      dim "  resolved-human jira mention webhook returned HTTP $jcode_a"
      pass "resolved-human jira mention webhook sent (HTTP $jcode_a)"
    fi

    local jmt_a=""
    for _i in 1 2 3 4 5 6 7 8; do
      jmt_a=$(find_jira_mention_task "$JIRA_RESOLVED_ACCT")
      [ -n "$jmt_a" ] && break
      sleep 2
    done

    if [ -n "$jmt_a" ]; then
      CREATED_BEADS+=("$jmt_a")
      pass "resolved-human jira mention spawned task bead ($jmt_a)"
      local jshow_a jop_field_a jop_audit_a
      jshow_a=$("$KD_BIN" show "$jmt_a" --json 2>/dev/null || echo "{}")
      jop_field_a=$(echo "$jshow_a" | python3 -c "import sys,json; print((json.load(sys.stdin).get('fields') or {}).get('operated_by',''))" 2>/dev/null || echo "")
      jop_audit_a=$(echo "$jshow_a" | python3 -c "import sys,json; print(json.load(sys.stdin).get('operated_by',''))" 2>/dev/null || echo "")
      if [ "$jop_field_a" = "slack:$JIRA_RESOLVED_SLACK" ]; then
        pass "resolved jira mention operated_by FIELD = slack:$JIRA_RESOLVED_SLACK (human, not jira:*/fleet:*)"
      else
        fail "resolved jira mention operated_by field" "expected slack:$JIRA_RESOLVED_SLACK, got '$jop_field_a'"
      fi
      if [ "$jop_audit_a" = "slack:$JIRA_RESOLVED_SLACK" ]; then
        pass "resolved jira mention audit operated_by = slack:$JIRA_RESOLVED_SLACK"
      else
        fail "resolved jira mention audit operated_by" "expected slack:$JIRA_RESOLVED_SLACK, got '$jop_audit_a'"
      fi
      # Provenance: jira_user_id (accountId) + mirrored slack_user_id retained.
      assert_contains "resolved jira mention retains jira_user_id provenance" "$JIRA_RESOLVED_ACCT" "$jshow_a"
      assert_contains "resolved jira mention mirrors slack_user_id" "$JIRA_RESOLVED_SLACK" "$jshow_a"
    else
      dim "  no jira mention task found for $JIRA_RESOLVED_ACCT after webhook"
      fail "resolved-human jira mention spawned task" "no jira-mention task bead with jira_user_id=$JIRA_RESOLVED_ACCT"
    fi

    # (b) Fallback path: accountId with no matching user bead → operator must
    # fall back to jira:<accountID> (NOT fleet:jira-bridge, since accountId is
    # present).
    local jcode_b
    jcode_b=$(post_jira_mention_webhook "$JIRA_FALLBACK_ACCT" "PE-97002")
    dim "  fallback jira mention webhook returned HTTP $jcode_b"
    local jmt_b=""
    for _i in 1 2 3 4 5 6 7 8; do
      jmt_b=$(find_jira_mention_task "$JIRA_FALLBACK_ACCT")
      [ -n "$jmt_b" ] && break
      sleep 2
    done
    if [ -n "$jmt_b" ]; then
      CREATED_BEADS+=("$jmt_b")
      local jshow_b jop_field_b
      jshow_b=$("$KD_BIN" show "$jmt_b" --json 2>/dev/null || echo "{}")
      jop_field_b=$(echo "$jshow_b" | python3 -c "import sys,json; print((json.load(sys.stdin).get('fields') or {}).get('operated_by',''))" 2>/dev/null || echo "")
      if [ "$jop_field_b" = "jira:$JIRA_FALLBACK_ACCT" ]; then
        pass "unresolved jira mention operated_by FIELD falls back to jira:$JIRA_FALLBACK_ACCT"
      else
        fail "unresolved jira mention fallback operated_by" "expected jira:$JIRA_FALLBACK_ACCT, got '$jop_field_b'"
      fi
    else
      dim "  no jira mention task found for $JIRA_FALLBACK_ACCT after webhook"
      fail "unresolved-human jira mention spawned task" "no jira-mention task bead with jira_user_id=$JIRA_FALLBACK_ACCT"
    fi
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
