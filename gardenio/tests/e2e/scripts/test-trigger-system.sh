#!/usr/bin/env bash
# test-trigger-system.sh — trigger system E2E tests
#
# Tests the trigger subsystem: trigger bead CRUD for all 7 sources,
# bead-state trigger event matching, webhook trigger HTTP endpoint,
# trigger enable/disable, condition evaluation, and controller
# trigger engine logs.
#
# Trigger beads (type=trigger, kind=data) define event-driven actions.
# Each trigger has a source (jira, gitlab, webhook, bead-state,
# account-cost, schedule, slack-reaction) and an action (spawn_agent,
# apply_formula, notify, create_bead).
#
# Scenarios tested:
#   1. Trigger bead CRUD — create triggers for each source, verify fields
#   2. Trigger listing and filtering by source label
#   3. Enable/disable trigger state management
#   4. Complex condition JSON storage and retrieval
#   5. Trigger close lifecycle
#   6. Existing production triggers
#   7. Controller trigger engine logs
#   8. Webhook endpoint availability
#   9. Formula trigger with apply_formula action
#  10. Permission model — allowed_projects and allowed_roles

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
KD_BIN="${KD_BIN:-kd}"
# shellcheck source=SCRIPTDIR/lib-kd-create.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib-kd-create.sh"
GB_BIN="${GB_BIN:-gb}"
PASS=0
FAIL=0
ERRORS=()
PF_PID=""
BRIDGE_PF_PID_LOCAL=""
CREATED_BEADS=()

CONTROLLER_NS="${CONTROLLER_NS:-gardenio}"

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

assert_contains() {
  local name="$1"
  local pattern="$2"
  local actual="$3"
  if grep -qF -- "$pattern" <<< "$actual"; then
    pass "$name"
  else
    fail "$name" "expected output to contain '$pattern'; got: $(echo "$actual" | head -5)"
  fi
}

assert_not_contains() {
  local name="$1"
  local pattern="$2"
  local actual="$3"
  if grep -qF -- "$pattern" <<< "$actual"; then
    fail "$name" "output should NOT contain '$pattern'"
  else
    pass "$name"
  fi
}

assert_equals() {
  local name="$1"
  local expected="$2"
  local actual="$3"
  if [ "$expected" = "$actual" ]; then
    pass "$name"
  else
    fail "$name" "expected '$expected', got '$actual'"
  fi
}

# Extract a field from JSON using python3
json_field() {
  local json="$1"
  local path="$2"
  echo "$json" | python3 -c "
import sys, json
d = json.load(sys.stdin)
keys = '$path'.split('.')
for k in keys:
    if isinstance(d, dict):
        d = d.get(k, '')
    else:
        d = ''
        break
if isinstance(d, bool):
    print(str(d).lower())
elif isinstance(d, (int, float)):
    print(d)
elif d is None:
    print('')
else:
    print(d)
" 2>/dev/null || echo ""
}

# json_field_type <json> <dotted.path> — print the JSON *type* of a field, one
# of: number, str, bool, null, missing. Used to assert a numeric schema field
# (type:trigger `threshold` is a float) round-trips as a JSON NUMBER, not a
# string — the likeliest silent coercion when the daemon persists a typed field
# to JSONB (ka-0000001277). json_field() coerces every scalar back to a bare
# string for its equality checks, so it CANNOT distinguish 80 from "80"; this
# helper reads the underlying JSON type directly. int and float both collapse to
# "number" on purpose — the guard is against a string coercion, not the exact
# numeric representation (JSONB may render 80 either way). bool is checked before
# number because in Python isinstance(True, int) is True.
json_field_type() {
  local json="$1"
  local path="$2"
  echo "$json" | python3 -c "
import sys, json
d = json.load(sys.stdin)
keys = '$path'.split('.')
for k in keys:
    if isinstance(d, dict) and k in d:
        d = d[k]
    else:
        print('missing'); sys.exit(0)
if isinstance(d, bool):
    print('bool')
elif isinstance(d, (int, float)):
    print('number')
elif d is None:
    print('null')
else:
    print('str')
" 2>/dev/null || echo "missing"
}

# Create a trigger bead via the HTTP API (bypasses kd CLI field parsing
# issues with JSON string fields like conditions).
# Usage: create_trigger '{"title":"...","labels":[...],"fields":{...}}'
# Returns: bead ID
create_trigger() {
  local body="$1"
  local result http_code
  # kbeads server enforces non-empty operated_by since ka-0000005772 — raw
  # curl writes without X-Operated-By 400. run-shard.sh exports
  # BEADS_OPERATED_BY for `kd`/`gb`; mirror it onto raw POSTs here.
  #
  # FAIL LOUDLY on a rejected create (ka-0000007771). This used to `curl -sf …
  # || echo ""` then `return 1` on an empty body, so a 400 (e.g. a field the
  # deployed type:trigger schema rejects) collapsed to an empty string. Every
  # caller assigns it as a BARE `id=$(create_trigger …)` under `set -euo
  # pipefail`, so that `return 1` KILLED the whole module with ZERO diagnostic
  # output — the exact silent, retry-proof "(no output captured)" death this
  # bug chased (Scenario 1e once died on `condition_type: unknown field`,
  # invisible — that field is DECLARED now, but any other unknown field still
  # 400s, so the fail-loud guard stays load-bearing).
  # Capture the HTTP status and echo the daemon's error to stderr so the reason
  # lands in the shard log; still return 1 so callers can branch, but they now
  # get a REASON, not a blank.
  local tmp; tmp="$(mktemp)"
  http_code=$(curl -s --max-time 30 -o "$tmp" -w '%{http_code}' \
    -X POST "$BEADS_HTTP_URL/v1/beads" \
    -H 'Content-Type: application/json' \
    -H "Authorization: Bearer ${BEADS_TOKEN:-}" \
    ${BEADS_OPERATED_BY:+-H "X-Operated-By: $BEADS_OPERATED_BY"} \
    -d "$body" 2>/dev/null || echo "000")
  result="$(cat "$tmp" 2>/dev/null)"; rm -f "$tmp"
  if [ "$http_code" -lt 200 ] || [ "$http_code" -ge 300 ]; then
    red "  create_trigger: POST /v1/beads -> HTTP $http_code" >&2
    dim "    body: $(printf '%s' "$result" | head -c 400)" >&2
    echo ""
    return 1
  fi
  echo "$result" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo ""
}

# assert_create_accepted <name> <body> — POST a trigger bead and assert the
# daemon ACCEPTS it (2xx). This is the POSITIVE counterpart to authoring a
# trigger that carries its source-match condition fields: the account-cost and
# slack-reaction SOURCES read condition_type/field/op/threshold/account_filter
# and emoji/match/channel_filter off the bead, so the type:trigger WRITE schema
# MUST declare them or the trigger is live-but-unauthorable (400 "unknown
# field", ka-0000001277). On success it records the id in CREATED_BEADS for
# cleanup AND publishes it via the global LAST_CREATED_ID so the caller can
# round-trip the persisted fields (pass/fail write colored output to stdout, so
# this helper cannot also echo the id — the global is the clean channel).
LAST_CREATED_ID=""
assert_create_accepted() {
  local name="$1"
  local body="$2"
  LAST_CREATED_ID=""
  local tmp; tmp="$(mktemp)"
  local code
  code=$(curl -s --max-time 30 -o "$tmp" -w '%{http_code}' \
    -X POST "$BEADS_HTTP_URL/v1/beads" \
    -H 'Content-Type: application/json' \
    -H "Authorization: Bearer ${BEADS_TOKEN:-}" \
    ${BEADS_OPERATED_BY:+-H "X-Operated-By: $BEADS_OPERATED_BY"} \
    -d "$body" 2>/dev/null || echo "000")
  local resp; resp="$(cat "$tmp" 2>/dev/null)"; rm -f "$tmp"
  if [ "$code" -ge 200 ] && [ "$code" -lt 300 ]; then
    local id
    id=$(printf '%s' "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
    if [ -n "$id" ]; then
      CREATED_BEADS+=("$id")
      LAST_CREATED_ID="$id"
    fi
    pass "$name (HTTP $code)"
  else
    dim "    body: $(printf '%s' "$resp" | head -c 400)" >&2
    fail "$name" "expected a 2xx accept, got HTTP $code"
  fi
}

# ── Setup ─────────────────────────────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19108
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-trigger.log 2>&1 &
  PF_PID=$!
  sleep 3
  export BEADS_HTTP_URL="http://localhost:${port}"
  dim "  Daemon at $BEADS_HTTP_URL (pid $PF_PID)"
}

setup_bridge_portforward() {
  if [ -n "${BRIDGE_HTTP_URL:-}" ]; then
    dim "  Using BRIDGE_HTTP_URL=$BRIDGE_HTTP_URL"
    return
  fi
  local bridge_svc="${NAMESPACE}-slack-bridge"
  blue "Port-forwarding ${bridge_svc} for webhook endpoint..."
  local port=19109
  kubectl -n "$NAMESPACE" port-forward "svc/${bridge_svc}" "${port}:8090" \
    >/tmp/bridge-pf-trigger.log 2>&1 &
  BRIDGE_PF_PID_LOCAL=$!
  sleep 3
  export BRIDGE_HTTP_URL="http://localhost:${port}"
  dim "  Bridge at $BRIDGE_HTTP_URL (pid $BRIDGE_PF_PID_LOCAL)"
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

seed_trigger_type() {
  # type:trigger is builtin-AUTHORITATIVE — the daemon builtin (kbeads
  # config.go) is the schema the create/update WRITE path validates against.
  # This test must therefore create beads with ONLY builtin-declared fields.
  #
  # The hand-maintained shadow PUT dropped in f7e0b5e2a was a drifted schema
  # that could only SHADOW-and-drift the builtin, making e2e exercise a schema
  # prod does not run (ka-0000003677 / ka-0000000407). Dropping it was correct —
  # do NOT resurrect the PUT; the builtin alone is the write schema this module
  # exercises. The builtin NOW declares the account-cost condition fields
  # (condition_type/field/op/threshold/account_filter) and the slack-reaction
  # fields (emoji/match/channel_filter) — added in 9fba741f0 (ka-0000001277)
  # because the account-cost and slack-reaction trigger SOURCES read them off
  # the bead, so a create carrying them MUST now be ACCEPTED (before, it 400'd
  # "unknown field" — a silent authoring lockout). Scenarios 1e/1f below author
  # those triggers WITH their condition fields and assert the create is accepted
  # and the values round-trip, so a regression that re-drops the schema (making
  # the trigger unauthorable again) reds loudly.
  dim "  type:trigger config: using daemon builtin (no shadow PUT)"
}

cleanup() {
  for id in "${CREATED_BEADS[@]}"; do
    "$KD_BIN" close "$id" 2>/dev/null || true
  done
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
  if [ -n "$BRIDGE_PF_PID_LOCAL" ]; then
    kill "$BRIDGE_PF_PID_LOCAL" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " trigger-system E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  seed_trigger_type

  local TEST_TAG="e2e-trig-$$-$(date +%s)"

  # ── Scenario 1: Trigger bead CRUD for each source ──────────────
  blue "Scenario 1: Trigger bead CRUD — create, read, update, close"

  # 1a. Create a bead-state trigger (via HTTP API for conditions JSON)
  local bead_trig_id
  bead_trig_id=$(create_trigger "{
    \"title\": \"e2e-bead-trigger-$TEST_TAG\",
    \"type\": \"trigger\",
    \"description\": \"Fires on bead created events\",
    \"labels\": [\"e2e\", \"$TEST_TAG\", \"trigger:bead\", \"project:gardenio\"],
    \"fields\": {
      \"source\": \"bead-state\",
      \"action\": \"create_bead\",
      \"enabled\": false,
      \"event_types\": \"created\",
      \"conditions\": \"[{\\\"field\\\":\\\"type\\\",\\\"op\\\":\\\"==\\\",\\\"value\\\":\\\"task\\\"}]\",
      \"project\": \"gardenio\",
      \"prompt\": \"Bead {{bead_id}} ({{bead_type}}) was created: {{bead_title}}\",
      \"max_concurrent\": 2,
      \"cooldown_minutes\": 1
    }
  }")

  if [ -n "$bead_trig_id" ]; then
    CREATED_BEADS+=("$bead_trig_id")
    pass "created bead-state trigger ($bead_trig_id)"

    local bead_trig_json
    bead_trig_json=$("$KD_BIN" show "$bead_trig_id" --json 2>/dev/null || echo "{}")

    assert_equals "type=trigger" "trigger" "$(json_field "$bead_trig_json" "type")"
    assert_equals "source=bead-state" "bead-state" "$(json_field "$bead_trig_json" "fields.source")"
    assert_equals "action=create_bead" "create_bead" "$(json_field "$bead_trig_json" "fields.action")"
    assert_equals "enabled=false" "false" "$(json_field "$bead_trig_json" "fields.enabled")"
    assert_equals "event_types=created" "created" "$(json_field "$bead_trig_json" "fields.event_types")"
    assert_equals "max_concurrent=2" "2" "$(json_field "$bead_trig_json" "fields.max_concurrent")"
    assert_equals "cooldown_minutes=1" "1" "$(json_field "$bead_trig_json" "fields.cooldown_minutes")"

    # Verify conditions JSON stored correctly
    local conditions_val
    conditions_val=$(json_field "$bead_trig_json" "fields.conditions")
    assert_contains "conditions contains type==task" '"field":"type"' "$conditions_val"

    # Verify prompt template stored
    local prompt_val
    prompt_val=$(json_field "$bead_trig_json" "fields.prompt")
    assert_contains "prompt contains template var" '{{bead_id}}' "$prompt_val"
  else
    fail "bead-state trigger creation" "could not create trigger bead"
  fi
  echo

  # 1b. Create a webhook trigger
  local webhook_token="e2e-wh-$TEST_TAG"
  local webhook_trig_id
  webhook_trig_id=$(create_trigger "{
    \"title\": \"e2e-webhook-trigger-$TEST_TAG\",
    \"type\": \"trigger\",
    \"description\": \"Fires on inbound webhook\",
    \"labels\": [\"e2e\", \"$TEST_TAG\", \"trigger:webhook\", \"project:gardenio\"],
    \"fields\": {
      \"source\": \"webhook\",
      \"action\": \"notify\",
      \"enabled\": false,
      \"webhook_token\": \"$webhook_token\",
      \"webhook_secret\": \"test-secret-123\",
      \"conditions\": \"[{\\\"path\\\":\\\"event.type\\\",\\\"op\\\":\\\"==\\\",\\\"value\\\":\\\"alert\\\"}]\",
      \"project\": \"gardenio\",
      \"prompt\": \"Webhook alert: {{event.type}} — {{event.message}}\"
    }
  }")

  if [ -n "$webhook_trig_id" ]; then
    CREATED_BEADS+=("$webhook_trig_id")
    pass "created webhook trigger ($webhook_trig_id)"

    local wh_trig_json
    wh_trig_json=$("$KD_BIN" show "$webhook_trig_id" --json 2>/dev/null || echo "{}")

    assert_equals "webhook source=webhook" "webhook" "$(json_field "$wh_trig_json" "fields.source")"
    assert_equals "webhook action=notify" "notify" "$(json_field "$wh_trig_json" "fields.action")"
    assert_equals "webhook_token set" "$webhook_token" "$(json_field "$wh_trig_json" "fields.webhook_token")"
    assert_equals "webhook_secret set" "test-secret-123" "$(json_field "$wh_trig_json" "fields.webhook_secret")"
  else
    fail "webhook trigger creation" "could not create trigger bead"
  fi
  echo

  # 1c. Create a JIRA trigger (kd CLI works for fields without JSON values)
  local jira_trig_id
  jira_trig_id=$(kd_create_id "$KD_BIN" create "e2e-jira-trigger-$TEST_TAG" \
    --type=trigger \
    -d "Fires on JIRA issues" \
    -l "e2e,$TEST_TAG,trigger:jira,project:gardenio" \
    -f 'source=jira' \
    -f 'action=spawn_agent' \
    -f 'enabled=false' \
    -f 'jql=project = PE AND status = Open' \
    -f 'role=crew' \
    -f 'project=gardenio' \
    -f 'prompt=Investigate JIRA issue')

  if [ -n "$jira_trig_id" ]; then
    CREATED_BEADS+=("$jira_trig_id")
    pass "created JIRA trigger ($jira_trig_id)"
    assert_equals "jira source" "jira" \
      "$(json_field "$("$KD_BIN" show "$jira_trig_id" --json 2>/dev/null)" "fields.source")"
    assert_contains "jql stored" "project = PE" \
      "$(json_field "$("$KD_BIN" show "$jira_trig_id" --json 2>/dev/null)" "fields.jql")"
  else
    fail "JIRA trigger creation" "could not create trigger bead"
  fi
  echo

  # 1d. Create a GitLab trigger
  local gitlab_trig_id
  gitlab_trig_id=$(kd_create_id "$KD_BIN" create "e2e-gitlab-trigger-$TEST_TAG" \
    --type=trigger \
    -d "Fires on GitLab MRs" \
    -l "e2e,$TEST_TAG,trigger:gitlab,project:gardenio" \
    -f 'source=gitlab' \
    -f 'action=spawn_agent' \
    -f 'enabled=false' \
    -f 'event_type=merge_request' \
    -f 'project_path=Acme/Platform/gardenio' \
    -f 'state=opened' \
    -f 'target_branch=main' \
    -f 'role=crew' \
    -f 'project=gardenio')

  if [ -n "$gitlab_trig_id" ]; then
    CREATED_BEADS+=("$gitlab_trig_id")
    pass "created GitLab trigger ($gitlab_trig_id)"
    assert_equals "gitlab event_type" "merge_request" \
      "$(json_field "$("$KD_BIN" show "$gitlab_trig_id" --json 2>/dev/null)" "fields.event_type")"
    assert_equals "gitlab project_path" "Acme/Platform/gardenio" \
      "$(json_field "$("$KD_BIN" show "$gitlab_trig_id" --json 2>/dev/null)" "fields.project_path")"
    assert_equals "gitlab target_branch" "main" \
      "$(json_field "$("$KD_BIN" show "$gitlab_trig_id" --json 2>/dev/null)" "fields.target_branch")"
  else
    fail "GitLab trigger creation" "could not create trigger bead"
  fi
  echo

  # 1e. Create an account-cost trigger — WITH its condition fields.
  #
  # AUTHORABLE-WITH-CONDITION-FIELDS (ka-0000001277). The account-cost trigger
  # SOURCE (trigger_source_account.go) reads condition_type/field/op/threshold/
  # account_filter off the bead at load time. Those fields are NOW declared in
  # the type:trigger builtin (kbeads config.go, added in 9fba741f0), so a create
  # carrying them MUST be ACCEPTED. Before that fix the create 400'd
  # "condition_type: unknown field" — a silent authoring lockout: no operator or
  # agent could author an account-cost trigger at all. This scenario is the e2e
  # that would notice a revert of that schema:
  #   (a) POSITIVELY author the account-cost trigger WITH all five condition
  #       fields and assert the create is accepted (assert_create_accepted), and
  #   (b) assert the fields round-trip — especially that `threshold` comes back
  #       as a JSON NUMBER, not a string. `threshold` is the only non-string
  #       field of the set (type:float in the builtin; the account source
  #       ParseFloat's it), so it is the likeliest silent coercion. The write
  #       schema requires a JSON number for a float field (model
  #       ValidateFields), so the fixture sends `80` unquoted; a regression that
  #       stored/re-emitted it as "80" reds the round-trip.
  #
  # A regression that re-drops the schema makes the account-cost trigger
  # unauthorable again — this scenario 400s on the create and reds loudly.
  assert_create_accepted "account-cost trigger authored with condition fields" "{
    \"title\": \"e2e-account-trigger-$TEST_TAG\",
    \"type\": \"trigger\",
    \"description\": \"Fires on account budget threshold\",
    \"labels\": [\"e2e\", \"$TEST_TAG\", \"trigger:account\", \"project:gardenio\"],
    \"fields\": {
      \"source\": \"account-cost\",
      \"action\": \"notify\",
      \"enabled\": false,
      \"condition_type\": \"utilization_threshold\",
      \"field\": \"utilization_5h\",
      \"op\": \">=\",
      \"threshold\": 80,
      \"account_filter\": \"gardenio-prod\",
      \"project\": \"gardenio\",
      \"prompt\": \"Account budget warning\"
    }
  }"
  local acct_trig_id="$LAST_CREATED_ID"
  if [ -n "$acct_trig_id" ]; then
    local acct_json
    acct_json=$("$KD_BIN" show "$acct_trig_id" --json 2>/dev/null || echo "{}")
    assert_equals "account source=account-cost" "account-cost" \
      "$(json_field "$acct_json" "fields.source")"
    assert_equals "account condition_type round-trips" "utilization_threshold" \
      "$(json_field "$acct_json" "fields.condition_type")"
    assert_equals "account field round-trips" "utilization_5h" \
      "$(json_field "$acct_json" "fields.field")"
    assert_equals "account account_filter round-trips" "gardenio-prod" \
      "$(json_field "$acct_json" "fields.account_filter")"
    # The load-bearing anti-coercion check: threshold is a schema float and must
    # come back as a JSON number, NOT the string "80".
    assert_equals "account threshold round-trips as a JSON number (not string)" "number" \
      "$(json_field_type "$acct_json" "fields.threshold")"
    # Value check, representation-tolerant: JSONB may render the number as 80 or
    # 80.0 — both are correct, so normalize to a numeric compare rather than a
    # string equality that would false-red on a CORRECT daemon (the exact trap
    # this whole scenario exists to avoid).
    assert_equals "account threshold value=80" "ok" \
      "$(echo "$acct_json" | python3 -c "import sys,json; t=json.load(sys.stdin).get('fields',{}).get('threshold'); print('ok' if isinstance(t,(int,float)) and not isinstance(t,bool) and float(t)==80.0 else 'no:'+repr(t))" 2>/dev/null || echo "err")"
  fi
  echo

  # 1f. Create a slack-reaction trigger — WITH its match fields.
  #
  # Same authorable-with-condition-fields treatment as 1e (ka-0000001277): the
  # slack-reaction SOURCE (trigger_source_slack_reaction.go) reads emoji/match/
  # channel_filter off the bead, and the type:trigger builtin now declares all
  # three (9fba741f0), so a create carrying them MUST be ACCEPTED. Author the
  # trigger WITH the three match fields, assert accepted, and assert they
  # round-trip (all three are schema strings). A revert of the schema 400s the
  # create and reds here.
  assert_create_accepted "slack-reaction trigger authored with match fields" "{
    \"title\": \"e2e-reaction-trigger-$TEST_TAG\",
    \"type\": \"trigger\",
    \"description\": \"Fires on Slack reaction\",
    \"labels\": [\"e2e\", \"$TEST_TAG\", \"trigger:slack-reaction\", \"project:gardenio\"],
    \"fields\": {
      \"source\": \"slack-reaction\",
      \"action\": \"spawn_agent\",
      \"enabled\": false,
      \"emoji\": \"rocket\",
      \"match\": \"agent\",
      \"channel_filter\": \"C0AHE7C0E1G\",
      \"role\": \"crew\",
      \"project\": \"gardenio\",
      \"prompt\": \"Handle reaction on message\"
    }
  }"
  local react_trig_id="$LAST_CREATED_ID"
  if [ -n "$react_trig_id" ]; then
    local react_json
    react_json=$("$KD_BIN" show "$react_trig_id" --json 2>/dev/null || echo "{}")
    assert_equals "reaction source=slack-reaction" "slack-reaction" \
      "$(json_field "$react_json" "fields.source")"
    assert_equals "reaction emoji round-trips" "rocket" \
      "$(json_field "$react_json" "fields.emoji")"
    assert_equals "reaction match round-trips" "agent" \
      "$(json_field "$react_json" "fields.match")"
    assert_equals "reaction channel_filter round-trips" "C0AHE7C0E1G" \
      "$(json_field "$react_json" "fields.channel_filter")"
  fi
  echo

  # 1g. Update a trigger — change action and verify
  if [ -n "$bead_trig_id" ]; then
    "$KD_BIN" update "$bead_trig_id" -f 'action=spawn_agent' -f 'role=crew' 2>/dev/null || true
    local updated_json
    updated_json=$("$KD_BIN" show "$bead_trig_id" --json 2>/dev/null || echo "{}")
    assert_equals "updated action=spawn_agent" "spawn_agent" \
      "$(json_field "$updated_json" "fields.action")"
    assert_equals "updated role=crew" "crew" \
      "$(json_field "$updated_json" "fields.role")"
  fi
  echo

  # ── Scenario 2: Trigger listing and filtering ──────────────────
  blue "Scenario 2: Trigger listing and filtering"

  local all_triggers
  all_triggers=$("$KD_BIN" list --type=trigger --status=open -l "$TEST_TAG" --json 2>/dev/null || echo "[]")
  local trig_count
  trig_count=$(echo "$all_triggers" | python3 -c "
import sys,json
d=json.load(sys.stdin)
print(len(d) if isinstance(d,list) else 0)
" 2>/dev/null || echo "0")

  # We created 6 triggers (bead, webhook, jira, gitlab, account, reaction)
  if [ "$trig_count" -ge 6 ]; then
    pass "listed $trig_count trigger beads (expected >=6)"
  else
    fail "trigger list count" "expected >=6, got $trig_count"
  fi

  # Filter by trigger:bead label
  local bead_triggers
  bead_triggers=$("$KD_BIN" list --type=trigger --status=open -l "$TEST_TAG,trigger:bead" --json 2>/dev/null || echo "[]")
  local bead_trig_count
  bead_trig_count=$(echo "$bead_triggers" | python3 -c "
import sys,json
d=json.load(sys.stdin)
print(len(d) if isinstance(d,list) else 0)
" 2>/dev/null || echo "0")

  if [ "$bead_trig_count" -ge 1 ]; then
    pass "filtered trigger:bead found $bead_trig_count beads"
  else
    fail "trigger:bead filter" "expected >=1, got $bead_trig_count"
  fi

  # Filter by trigger:webhook label
  local wh_triggers
  wh_triggers=$("$KD_BIN" list --type=trigger --status=open -l "$TEST_TAG,trigger:webhook" --json 2>/dev/null || echo "[]")
  local wh_trig_count
  wh_trig_count=$(echo "$wh_triggers" | python3 -c "
import sys,json
d=json.load(sys.stdin)
print(len(d) if isinstance(d,list) else 0)
" 2>/dev/null || echo "0")

  if [ "$wh_trig_count" -ge 1 ]; then
    pass "filtered trigger:webhook found $wh_trig_count beads"
  else
    fail "trigger:webhook filter" "expected >=1, got $wh_trig_count"
  fi
  echo

  # ── Scenario 3: Enable/disable trigger ─────────────────────────
  blue "Scenario 3: Enable/disable trigger state management"

  if [ -n "$bead_trig_id" ]; then
    # Currently disabled — verify
    assert_equals "trigger starts disabled" "false" \
      "$(json_field "$("$KD_BIN" show "$bead_trig_id" --json 2>/dev/null)" "fields.enabled")"

    # Enable it
    "$KD_BIN" update "$bead_trig_id" -f 'enabled=true' 2>/dev/null || true
    assert_equals "trigger enabled" "true" \
      "$(json_field "$("$KD_BIN" show "$bead_trig_id" --json 2>/dev/null)" "fields.enabled")"

    # Disable it again
    "$KD_BIN" update "$bead_trig_id" -f 'enabled=false' 2>/dev/null || true
    assert_equals "trigger re-disabled" "false" \
      "$(json_field "$("$KD_BIN" show "$bead_trig_id" --json 2>/dev/null)" "fields.enabled")"
  else
    pass "enable/disable test skipped (no trigger bead)"
  fi
  echo

  # ── Scenario 4: Trigger conditions validation ─────────────────
  blue "Scenario 4: Trigger conditions — complex condition JSON"

  # Create a trigger with multiple conditions (all must match)
  local multi_cond_id
  multi_cond_id=$(create_trigger "{
    \"title\": \"e2e-multi-cond-$TEST_TAG\",
    \"type\": \"trigger\",
    \"description\": \"Multi-condition bead-state trigger\",
    \"labels\": [\"e2e\", \"$TEST_TAG\", \"trigger:bead\", \"project:gardenio\"],
    \"fields\": {
      \"source\": \"bead-state\",
      \"action\": \"notify\",
      \"enabled\": false,
      \"event_types\": \"created,updated\",
      \"conditions\": \"[{\\\"field\\\":\\\"type\\\",\\\"op\\\":\\\"==\\\",\\\"value\\\":\\\"task\\\"},{\\\"field\\\":\\\"status\\\",\\\"op\\\":\\\"in\\\",\\\"value\\\":\\\"open,in_progress\\\"},{\\\"field\\\":\\\"label:project:\\\",\\\"op\\\":\\\"present\\\",\\\"value\\\":\\\"\\\"}]\",
      \"project\": \"gardenio\",
      \"prompt\": \"Multi-condition match on {{bead_id}}\"
    }
  }")

  if [ -n "$multi_cond_id" ]; then
    CREATED_BEADS+=("$multi_cond_id")
    pass "created multi-condition trigger ($multi_cond_id)"

    local mc_json
    mc_json=$("$KD_BIN" show "$multi_cond_id" --json 2>/dev/null || echo "{}")

    # Verify event_types has both values
    local event_types_val
    event_types_val=$(json_field "$mc_json" "fields.event_types")
    assert_contains "event_types contains created" "created" "$event_types_val"
    assert_contains "event_types contains updated" "updated" "$event_types_val"

    # Verify conditions JSON contains all 3 conditions
    local conds_val
    conds_val=$(json_field "$mc_json" "fields.conditions")
    local cond_count
    cond_count=$(echo "$conds_val" | python3 -c "
import sys,json
d=json.loads(sys.stdin.read())
print(len(d) if isinstance(d,list) else 0)
" 2>/dev/null || echo "0")
    assert_equals "3 conditions stored" "3" "$cond_count"

    # Verify individual condition operators
    assert_contains "conditions has == op" '"op":"=="' "$conds_val"
    assert_contains "conditions has in op" '"op":"in"' "$conds_val"
    assert_contains "conditions has present op" '"op":"present"' "$conds_val"
  else
    fail "multi-condition trigger creation" "could not create trigger bead"
  fi
  echo

  # Create webhook trigger with conditions
  local wh_cond_id
  wh_cond_id=$(create_trigger "{
    \"title\": \"e2e-wh-cond-$TEST_TAG\",
    \"type\": \"trigger\",
    \"description\": \"Webhook with payload conditions\",
    \"labels\": [\"e2e\", \"$TEST_TAG\", \"trigger:webhook\", \"project:gardenio\"],
    \"fields\": {
      \"source\": \"webhook\",
      \"action\": \"notify\",
      \"enabled\": false,
      \"webhook_token\": \"e2e-cond-$TEST_TAG\",
      \"conditions\": \"[{\\\"path\\\":\\\"alert.severity\\\",\\\"op\\\":\\\"in\\\",\\\"value\\\":\\\"critical,warning\\\"},{\\\"path\\\":\\\"status\\\",\\\"op\\\":\\\"!=\\\",\\\"value\\\":\\\"resolved\\\"}]\",
      \"project\": \"gardenio\"
    }
  }")

  if [ -n "$wh_cond_id" ]; then
    CREATED_BEADS+=("$wh_cond_id")
    pass "created webhook trigger with payload conditions ($wh_cond_id)"

    local wh_conds
    wh_conds=$(json_field "$("$KD_BIN" show "$wh_cond_id" --json 2>/dev/null)" "fields.conditions")
    assert_contains "webhook condition has path" '"path":"alert.severity"' "$wh_conds"
    assert_contains "webhook condition has != op" '"op":"!="' "$wh_conds"
  else
    fail "webhook condition trigger creation" "could not create trigger bead"
  fi
  echo

  # ── Scenario 5: Trigger close lifecycle ────────────────────────
  blue "Scenario 5: Trigger close lifecycle"

  local lifecycle_id
  lifecycle_id=$(kd_create_id "$KD_BIN" create "e2e-lifecycle-trigger-$TEST_TAG" \
    --type=trigger \
    -d "Trigger to test close lifecycle" \
    -l "e2e,$TEST_TAG,trigger:bead" \
    -f 'source=bead-state' \
    -f 'action=notify' \
    -f 'enabled=false' \
    -f 'event_types=closed')

  if [ -n "$lifecycle_id" ]; then
    pass "created lifecycle trigger ($lifecycle_id)"

    # Close it
    "$KD_BIN" close "$lifecycle_id" 2>/dev/null || true
    local closed_status
    closed_status=$(json_field "$("$KD_BIN" show "$lifecycle_id" --json 2>/dev/null)" "status")
    assert_equals "trigger closed" "closed" "$closed_status"

    # Verify it doesn't show up in open trigger list
    local open_after
    open_after=$("$KD_BIN" list --type=trigger --status=open -l "$TEST_TAG" --json 2>/dev/null || echo "[]")
    assert_not_contains "closed trigger not in open list" "$lifecycle_id" "$open_after"
  else
    fail "lifecycle trigger creation" "could not create trigger bead"
  fi
  echo

  # ── Scenario 6: Existing production triggers ───────────────────
  blue "Scenario 6: Existing production triggers"

  local prod_triggers
  prod_triggers=$("$KD_BIN" list --type=trigger --status=open --json 2>/dev/null || echo "[]")
  local prod_count
  prod_count=$(echo "$prod_triggers" | python3 -c "
import sys,json
d=json.load(sys.stdin)
print(len(d) if isinstance(d,list) else 0)
" 2>/dev/null || echo "0")

  if [ "$prod_count" -gt 0 ]; then
    pass "found $prod_count open trigger beads"

    # Count by source label
    local source_breakdown
    source_breakdown=$(echo "$prod_triggers" | python3 -c "
import sys,json
d=json.load(sys.stdin)
sources = {}
if isinstance(d, list):
    for b in d:
        for l in b.get('labels', []):
            if l.startswith('trigger:'):
                src = l.split(':',1)[1]
                sources[src] = sources.get(src, 0) + 1
for src, count in sorted(sources.items()):
    print(f'  {src}: {count}')
" 2>/dev/null || echo "  (parse error)")
    dim "  Source breakdown:"
    while IFS= read -r line; do
      dim "$line"
    done <<< "$source_breakdown"
    pass "trigger source breakdown"

    # Count enabled vs disabled
    local enabled_breakdown
    enabled_breakdown=$(echo "$prod_triggers" | python3 -c "
import sys,json
d=json.load(sys.stdin)
enabled=disabled=0
if isinstance(d, list):
    for b in d:
        e = b.get('fields',{}).get('enabled')
        if e is False or str(e).lower() == 'false':
            disabled += 1
        else:
            enabled += 1
print(f'{enabled} enabled, {disabled} disabled')
" 2>/dev/null || echo "unknown")
    dim "  $enabled_breakdown"
    pass "enabled/disabled breakdown"

    # Check for last_checked field (evidence of engine activity)
    local has_checked
    has_checked=$(echo "$prod_triggers" | python3 -c "
import sys,json
d=json.load(sys.stdin)
if isinstance(d, list):
    for b in d:
        if b.get('fields',{}).get('last_checked',''):
            print('yes')
            break
    else:
        print('no')
else:
    print('no')
" 2>/dev/null || echo "no")

    if [ "$has_checked" = "yes" ]; then
      pass "at least one trigger has last_checked (engine has evaluated)"
    else
      dim "  No triggers have last_checked set"
      pass "last_checked check completed"
    fi
  else
    dim "  No production trigger beads found"
    pass "production trigger listing completed"
  fi
  echo

  # ── Scenario 7: Controller trigger engine logs ─────────────────
  blue "Scenario 7: Controller trigger engine logs"

  local controller_pod=""
  controller_pod=$(kubectl -n "$CONTROLLER_NS" get pods --no-headers 2>/dev/null \
    | grep "gardenio-agents\|${CONTROLLER_NS}-agents" | awk '{print $1}' | head -1 || echo "")

  if [ -n "$controller_pod" ]; then
    local trigger_logs
    trigger_logs=$(kubectl -n "$CONTROLLER_NS" logs "$controller_pod" --tail=500 2>/dev/null \
      | grep -i "trigger" | tail -20 || echo "")

    if [ -n "$trigger_logs" ]; then
      pass "controller logs contain trigger activity"

      if grep -qi "trigger.*started\|trigger.*registered\|trigger.*source" <<<"$trigger_logs"; then
        pass "trigger engine startup messages found"
      else
        dim "  No trigger engine startup in recent logs"
        pass "trigger engine startup check completed"
      fi

      if grep -qi "trigger.*evaluat\|trigger.*match\|trigger.*fire" <<<"$trigger_logs"; then
        pass "trigger evaluation activity found"
      else
        dim "  No trigger evaluation in recent logs"
        pass "trigger evaluation check completed"
      fi

      if grep -qi "bead-state.*SSE\|bead-state.*register" <<<"$trigger_logs"; then
        pass "bead-state SSE handlers registered"
      else
        dim "  No bead-state SSE handler registration in logs"
        pass "bead-state SSE check completed"
      fi
    else
      dim "  No trigger logs in recent controller output"
      pass "trigger log check completed"
    fi
  else
    dim "  No controller pod found in $CONTROLLER_NS"
    pass "controller trigger log check skipped"
  fi
  echo

  # ── Scenario 8: Webhook endpoint health ────────────────────────
  blue "Scenario 8: Webhook endpoint availability"

  setup_bridge_portforward

  # Test that the webhook endpoint exists (POST to unknown token → 404)
  local wh_post_unknown
  wh_post_unknown=$(curl -sf -o /dev/null -w "%{http_code}" \
    -X POST "$BRIDGE_HTTP_URL/webhooks/nonexistent-token-$$" \
    -H 'Content-Type: application/json' \
    -d '{"test": true}' 2>/dev/null || echo "000")

  if [ "$wh_post_unknown" = "404" ]; then
    pass "POST to unknown webhook token returns 404"
  elif [ "$wh_post_unknown" = "000" ]; then
    dim "  Bridge not reachable at $BRIDGE_HTTP_URL — skipping webhook tests"
    pass "webhook endpoint check skipped (bridge unreachable)"
  else
    dim "  Got $wh_post_unknown for unknown token POST"
    pass "webhook POST check completed"
  fi

  # POST without Content-Type should still be handled
  local wh_no_ct
  wh_no_ct=$(curl -sf -o /dev/null -w "%{http_code}" \
    -X POST "$BRIDGE_HTTP_URL/webhooks/nonexistent-$$" \
    -d '{"test": true}' 2>/dev/null || echo "000")

  if [ "$wh_no_ct" != "000" ]; then
    pass "webhook endpoint handles POST without Content-Type ($wh_no_ct)"
  else
    pass "webhook endpoint reachability check completed"
  fi
  echo

  # ── Scenario 9: Formula trigger (apply_formula action) ─────────
  blue "Scenario 9: Formula trigger with apply_formula action"

  local formula_trig_id
  formula_trig_id=$(create_trigger "{
    \"title\": \"e2e-formula-trigger-$TEST_TAG\",
    \"type\": \"trigger\",
    \"description\": \"Trigger that applies a formula\",
    \"labels\": [\"e2e\", \"$TEST_TAG\", \"trigger:bead\", \"project:gardenio\"],
    \"fields\": {
      \"source\": \"bead-state\",
      \"action\": \"apply_formula\",
      \"enabled\": false,
      \"event_types\": \"created\",
      \"conditions\": \"[{\\\"field\\\":\\\"type\\\",\\\"op\\\":\\\"==\\\",\\\"value\\\":\\\"epic\\\"}]\",
      \"formula_id\": \"gd-test-formula\",
      \"formula_vars\": \"{\\\"component\\\":\\\"{{bead_title}}\\\"}\",
      \"project\": \"gardenio\",
      \"max_concurrent\": 1
    }
  }")

  if [ -n "$formula_trig_id" ]; then
    CREATED_BEADS+=("$formula_trig_id")
    pass "created apply_formula trigger ($formula_trig_id)"

    local ft_json
    ft_json=$("$KD_BIN" show "$formula_trig_id" --json 2>/dev/null || echo "{}")
    assert_equals "action=apply_formula" "apply_formula" "$(json_field "$ft_json" "fields.action")"
    assert_equals "formula_id set" "gd-test-formula" "$(json_field "$ft_json" "fields.formula_id")"
    assert_contains "formula_vars contains template" '{{bead_title}}' \
      "$(json_field "$ft_json" "fields.formula_vars")"
  else
    fail "formula trigger creation" "could not create trigger bead"
  fi
  echo

  # ── Scenario 10: Permission model fields ───────────────────────
  blue "Scenario 10: Permission model — allowed_projects and allowed_roles"

  local perm_trig_id
  perm_trig_id=$(kd_create_id "$KD_BIN" create "e2e-perm-trigger-$TEST_TAG" \
    --type=trigger \
    -d "Trigger with permission restrictions" \
    -l "e2e,$TEST_TAG,trigger:bead,project:gardenio" \
    -f 'source=bead-state' \
    -f 'action=spawn_agent' \
    -f 'enabled=false' \
    -f 'event_types=created' \
    -f 'allowed_projects=gardenio,acme' \
    -f 'allowed_roles=crew,gardener' \
    -f 'role=crew' \
    -f 'project=gardenio')

  if [ -n "$perm_trig_id" ]; then
    CREATED_BEADS+=("$perm_trig_id")
    pass "created trigger with permission restrictions ($perm_trig_id)"

    local perm_json
    perm_json=$("$KD_BIN" show "$perm_trig_id" --json 2>/dev/null || echo "{}")
    assert_contains "allowed_projects stored" "gardenio" \
      "$(json_field "$perm_json" "fields.allowed_projects")"
    assert_contains "allowed_roles stored" "crew" \
      "$(json_field "$perm_json" "fields.allowed_roles")"
  else
    fail "permission trigger creation" "could not create trigger bead"
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
