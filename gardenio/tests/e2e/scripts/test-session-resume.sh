#!/usr/bin/env bash
# test-session-resume.sh — session resumption E2E tests
#
# Tests the bead lifecycle for session resumption: agent bead creation
# with resume metadata, session bead tracking with the resumed flag,
# and the agent respawn lifecycle that preserves thread context.
#
# Usage:
#   ./tests/e2e/scripts/test-session-resume.sh
#
# Prerequisites:
#   - kubectl context pointing at the target cluster
#   - kd binary in PATH (or set KD_BIN)
#   - gb binary in PATH (or set GB_BIN)
#   - jq and python3 installed
#
# Scenarios tested:
#   1. Agent bead creation with slack-thread-resume metadata
#   2. Session bead with resumed=true field
#   3. Agent respawn lifecycle (close → recreate with same name)
#   4. Decision-resume spawn source fields
#   5. Session bead close with exit code and status
#   6. Multiple resume cycles preserve thread metadata
#   7. Resume after clean exit — session chain continuity
#   8. Resume after credential switch — auth config bead lifecycle
#   9. Resume after pod restart — crash recovery with resumed session
#  10. Both arms — project session_resume=true (enabled, conversation restored)
#  11. Both arms — project session_resume=false (disabled, fresh start each pod)
#  12. Both arms — role bead session_resume=false overrides project default
#  13. Both arms — agent metadata session_resume override beats project/role

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
CREATED_BEADS=()

export KD_ACTOR="${KD_ACTOR:-e2e-test}"

# ── Helpers ───────────────────────────────────────────────────────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

# Wrapper for curl that includes auth header when BEADS_TOKEN is set.
# kbeads server enforces non-empty operated_by since ka-0000005772 — raw
# curl writes without X-Operated-By 400. run-shard.sh exports
# BEADS_OPERATED_BY for `kd`/`gb`; mirror it onto raw POSTs here.
beads_curl() {
  curl ${BEADS_TOKEN:+-H "Authorization: Bearer $BEADS_TOKEN"} \
       ${BEADS_OPERATED_BY:+-H "X-Operated-By: $BEADS_OPERATED_BY"} "$@"
}

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

assert_equals() {
  local name="$1"
  local expected="$2"
  local actual="$3"
  if [ "$actual" = "$expected" ]; then
    pass "$name"
  else
    fail "$name" "expected '$expected', got '$actual'"
  fi
}

json_field() {
  local json="$1"
  local field="$2"
  echo "$json" | python3 -c "
import sys, json
d = json.load(sys.stdin)
v = d.get('$field', '')
# Handle nested fields dict
if '$field' == 'fields' or isinstance(v, dict):
    print(json.dumps(v))
elif isinstance(v, bool):
    print(str(v).lower())
else:
    print(v)
" 2>/dev/null || echo ""
}

json_nested() {
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
elif isinstance(d, (dict, list)):
    print(json.dumps(d))
else:
    print(d)
" 2>/dev/null || echo ""
}

# ── Setup: port-forward kd server ────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi

  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19098
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-session-resume.log 2>&1 &
  PF_PID=$!
  sleep 3

  export BEADS_HTTP_URL="http://localhost:${port}"
  dim "  Daemon at $BEADS_HTTP_URL (pid $PF_PID)"
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
    dim "  WARNING: No token found; kd server may reject requests"
  fi
}

# ── Seed session type config ─────────────────────────────────────
seed_session_type() {
  # type:session is builtin-authoritative; the daemon builtin (config.go) +
  # controller EnsureConfigs seed already declare a SUPERSET of these fields.
  # The old hand-maintained PUT could only shadow-and-drift the builtin
  # (ka-0000003677 / generalized ka-0000000407). Dropped so e2e exercises the
  # builtin prod runs, guarded by TestE2ESeedTypes_NoBuiltinShadow. Do NOT re-add.
  dim "  Session type config: using daemon builtin (no shadow PUT)"
}

# ── Cleanup ───────────────────────────────────────────────────────
cleanup() {
  for id in "${CREATED_BEADS[@]}"; do
    "$KD_BIN" close "$id" 2>/dev/null || true
  done
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " session-resume E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token
  seed_session_type

  # Verify kd server is reachable
  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  local TEST_TAG="e2e-session-resume-$$-$(date +%s)"
  local AGENT_NAME="e2e-resume-agent-$TEST_TAG"

  # ── Scenario 1: Agent bead with slack-thread-resume metadata ────
  blue "Scenario 1: Agent bead with slack-thread-resume metadata"

  local create_out create_rc agent_id
  create_out=$(beads_curl -sf -X POST "$BEADS_HTTP_URL/v1/beads" \
    -H 'Content-Type: application/json' \
    -d "$(cat <<CREOF
{
  "title": "$AGENT_NAME",
  "type": "agent",
  "description": "E2E session-resume test agent — safe to delete",
  "labels": ["e2e:$TEST_TAG", "slack-thread", "project:gardenio"],
  "fields": {
    "agent": "$AGENT_NAME",
    "agent_state": "spawning",
    "mode": "job",
    "project": "gardenio",
    "role": "thread",
    "slack_thread_channel": "C0TEST123",
    "slack_thread_ts": "1700000000.000001",
    "slack_spawn_channel": "C0TEST123",
    "spawn_source": "slack-thread-resume",
    "slack_user_id": "U0TEST456"
  }
}
CREOF
  )") && create_rc=0 || create_rc=$?

  assert_exit "create agent bead with resume metadata" 0 "$create_rc"

  agent_id=$(echo "$create_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$agent_id" ]; then
    pass "agent bead created ($agent_id)"
    CREATED_BEADS+=("$agent_id")
  else
    fail "agent bead created" "no id in response: $create_out"
    return 1
  fi

  # Verify fields via kd show
  local show_out
  show_out=$("$KD_BIN" show "$agent_id" --json 2>/dev/null || echo "{}")
  assert_contains "spawn_source is slack-thread-resume" "slack-thread-resume" "$show_out"
  assert_contains "slack_thread_channel preserved" "C0TEST123" "$show_out"
  assert_contains "slack_thread_ts preserved" "1700000000.000001" "$show_out"
  assert_contains "agent_state is spawning" "spawning" "$show_out"
  assert_contains "role is thread" '"thread"' "$show_out"
  echo

  # ── Scenario 2: Session bead with resumed=true ──────────────────
  blue "Scenario 2: Session bead with resumed=true"

  local session_out session_id
  session_out=$(beads_curl -sf -X POST "$BEADS_HTTP_URL/v1/beads" \
    -H 'Content-Type: application/json' \
    -d "$(cat <<SEOF
{
  "title": "session-1-$TEST_TAG",
  "type": "session",
  "labels": ["e2e:$TEST_TAG"],
  "fields": {
    "agent": "$AGENT_NAME",
    "agent_bead_id": "$agent_id",
    "project": "gardenio",
    "role": "thread",
    "hostname": "e2e-test-pod",
    "started_at": "2026-03-25T10:00:00Z",
    "resumed": false,
    "status": "active"
  }
}
SEOF
  )") || true

  session_id=$(echo "$session_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$session_id" ]; then
    pass "session bead created ($session_id)"
    CREATED_BEADS+=("$session_id")
  else
    fail "session bead created" "response: $session_out"
    return 1
  fi

  # Verify resumed=false initially
  local session_show
  session_show=$("$KD_BIN" show "$session_id" --json 2>/dev/null || echo "{}")
  local resumed_val
  resumed_val=$(json_nested "$session_show" "fields.resumed")
  assert_equals "initial session resumed=false" "false" "$resumed_val"

  # Close the initial session (simulating agent exit)
  "$KD_BIN" update "$session_id" -f 'status=completed' -f 'exit_code=0' -f "ended_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)" 2>/dev/null || true
  "$KD_BIN" close "$session_id" 2>/dev/null || true

  # Create a resumed session (simulating coop restart with --resume)
  local resume_session_out resume_session_id
  resume_session_out=$(beads_curl -sf -X POST "$BEADS_HTTP_URL/v1/beads" \
    -H 'Content-Type: application/json' \
    -d "$(cat <<RSEOF
{
  "title": "session-2-$TEST_TAG",
  "type": "session",
  "labels": ["e2e:$TEST_TAG"],
  "fields": {
    "agent": "$AGENT_NAME",
    "agent_bead_id": "$agent_id",
    "project": "gardenio",
    "role": "thread",
    "hostname": "e2e-test-pod",
    "started_at": "2026-03-25T10:05:00Z",
    "resumed": true,
    "status": "active"
  }
}
RSEOF
  )") || true

  resume_session_id=$(echo "$resume_session_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$resume_session_id" ]; then
    pass "resumed session bead created ($resume_session_id)"
    CREATED_BEADS+=("$resume_session_id")
  else
    fail "resumed session bead created" "response: $resume_session_out"
  fi

  # Verify resumed=true on the new session
  local resume_show
  resume_show=$("$KD_BIN" show "$resume_session_id" --json 2>/dev/null || echo "{}")
  resumed_val=$(json_nested "$resume_show" "fields.resumed")
  assert_equals "resumed session has resumed=true" "true" "$resumed_val"

  # Verify agent_bead_id links back to the agent
  local linked_agent
  linked_agent=$(json_nested "$resume_show" "fields.agent_bead_id")
  assert_equals "resumed session links to agent bead" "$agent_id" "$linked_agent"
  echo

  # ── Scenario 3: Agent respawn lifecycle ─────────────────────────
  blue "Scenario 3: Agent respawn lifecycle (close → recreate with same name)"

  # Close the original agent bead (simulating gb done / agent exit)
  "$KD_BIN" close "$agent_id" 2>/dev/null || true
  local closed_show
  closed_show=$("$KD_BIN" show "$agent_id" --json 2>/dev/null || echo "{}")
  assert_contains "original agent is closed" '"closed"' "$closed_show"

  # Respawn with same name (simulating bridge.respawnThreadAgent)
  local respawn_out respawn_id
  respawn_out=$(beads_curl -sf -X POST "$BEADS_HTTP_URL/v1/beads" \
    -H 'Content-Type: application/json' \
    -d "$(cat <<RPEOF
{
  "title": "$AGENT_NAME",
  "type": "agent",
  "description": "Session-resumed thread agent.",
  "labels": ["e2e:$TEST_TAG", "slack-thread", "project:gardenio"],
  "fields": {
    "agent": "$AGENT_NAME",
    "agent_state": "spawning",
    "mode": "job",
    "project": "gardenio",
    "role": "thread",
    "slack_thread_channel": "C0TEST123",
    "slack_thread_ts": "1700000000.000001",
    "slack_spawn_channel": "C0TEST123",
    "spawn_source": "slack-thread-resume",
    "slack_user_id": "U0TEST456"
  }
}
RPEOF
  )") || true

  respawn_id=$(echo "$respawn_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$respawn_id" ]; then
    pass "respawned agent bead created ($respawn_id)"
    CREATED_BEADS+=("$respawn_id")
  else
    fail "respawned agent bead created" "response: $respawn_out"
  fi

  # Verify the respawned agent has the same thread metadata
  local respawn_show
  respawn_show=$("$KD_BIN" show "$respawn_id" --json 2>/dev/null || echo "{}")
  assert_contains "respawned agent has same name" "$AGENT_NAME" "$respawn_show"
  assert_contains "respawned agent has slack-thread-resume" "slack-thread-resume" "$respawn_show"
  assert_contains "respawned agent preserves thread_channel" "C0TEST123" "$respawn_show"
  assert_contains "respawned agent preserves thread_ts" "1700000000.000001" "$respawn_show"

  # Verify original and respawned are different beads
  if [ "$agent_id" != "$respawn_id" ]; then
    pass "respawned agent has new bead ID"
  else
    fail "respawned agent has new bead ID" "same ID: $agent_id"
  fi
  echo

  # ── Scenario 4: Decision-resume spawn source ────────────────────
  blue "Scenario 4: Decision-resume spawn source fields"

  local decision_agent_out decision_agent_id
  decision_agent_out=$(beads_curl -sf -X POST "$BEADS_HTTP_URL/v1/beads" \
    -H 'Content-Type: application/json' \
    -d "$(cat <<DREOF
{
  "title": "e2e-decision-agent-$TEST_TAG",
  "type": "agent",
  "description": "Restarted thread agent (session resume via decision).",
  "labels": ["e2e:$TEST_TAG", "slack-thread", "project:gardenio"],
  "fields": {
    "agent": "e2e-decision-agent-$TEST_TAG",
    "agent_state": "spawning",
    "mode": "job",
    "project": "gardenio",
    "role": "thread",
    "slack_thread_channel": "C0DECISION",
    "slack_thread_ts": "1700000000.000002",
    "spawn_source": "decision-resume",
    "decision_bead_id": "gd-FAKE-DECISION"
  }
}
DREOF
  )") || true

  decision_agent_id=$(echo "$decision_agent_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$decision_agent_id" ]; then
    pass "decision-resume agent bead created ($decision_agent_id)"
    CREATED_BEADS+=("$decision_agent_id")
  else
    fail "decision-resume agent bead created" "response: $decision_agent_out"
  fi

  local decision_show
  decision_show=$("$KD_BIN" show "$decision_agent_id" --json 2>/dev/null || echo "{}")
  assert_contains "spawn_source is decision-resume" "decision-resume" "$decision_show"
  assert_contains "decision_bead_id preserved" "gd-FAKE-DECISION" "$decision_show"
  echo

  # ── Scenario 5: Session bead close with exit code and status ────
  blue "Scenario 5: Session bead close with exit code and status"

  # Close the resumed session with metrics
  "$KD_BIN" update "$resume_session_id" \
    -f 'status=completed' \
    -f 'exit_code=0' \
    -f "ended_at=2026-03-25T10:15:00Z" \
    -f 'input_tokens=5000' \
    -f 'output_tokens=2000' \
    -f 'request_count=10' \
    2>/dev/null || true
  "$KD_BIN" close "$resume_session_id" 2>/dev/null || true

  local closed_session
  closed_session=$("$KD_BIN" show "$resume_session_id" --json 2>/dev/null || echo "{}")
  assert_contains "closed session has status closed" '"closed"' "$closed_session"

  local exit_code_val
  exit_code_val=$(json_nested "$closed_session" "fields.exit_code")
  assert_equals "session exit_code=0" "0" "$exit_code_val"

  local session_status
  session_status=$(json_nested "$closed_session" "fields.status")
  assert_equals "session status=completed" "completed" "$session_status"

  # Verify resumed flag survives close
  resumed_val=$(json_nested "$closed_session" "fields.resumed")
  assert_equals "resumed flag survives close" "true" "$resumed_val"
  echo

  # ── Scenario 6: Multiple resume cycles ──────────────────────────
  blue "Scenario 6: Multiple resume cycles preserve thread metadata"

  # Simulate a second respawn (third agent bead, same name)
  "$KD_BIN" close "$respawn_id" 2>/dev/null || true

  local cycle3_out cycle3_id
  cycle3_out=$(beads_curl -sf -X POST "$BEADS_HTTP_URL/v1/beads" \
    -H 'Content-Type: application/json' \
    -d "$(cat <<C3EOF
{
  "title": "$AGENT_NAME",
  "type": "agent",
  "description": "Session-resumed thread agent (cycle 3).",
  "labels": ["e2e:$TEST_TAG", "slack-thread", "project:gardenio"],
  "fields": {
    "agent": "$AGENT_NAME",
    "agent_state": "spawning",
    "mode": "job",
    "project": "gardenio",
    "role": "thread",
    "slack_thread_channel": "C0TEST123",
    "slack_thread_ts": "1700000000.000001",
    "slack_spawn_channel": "C0TEST123",
    "spawn_source": "slack-thread-resume",
    "slack_user_id": "U0TEST456"
  }
}
C3EOF
  )") || true

  cycle3_id=$(echo "$cycle3_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$cycle3_id" ]; then
    pass "cycle-3 agent bead created ($cycle3_id)"
    CREATED_BEADS+=("$cycle3_id")
  else
    fail "cycle-3 agent bead created" "response: $cycle3_out"
  fi

  # All three agent beads should exist with the same name
  local search_out match_count
  search_out=$("$KD_BIN" list --all-types --json -l "e2e:$TEST_TAG" 2>/dev/null || echo "[]")
  match_count=$(echo "$search_out" | python3 -c "
import sys, json
beads = json.load(sys.stdin)
if isinstance(beads, dict) and 'items' in beads:
    beads = beads['items']
count = sum(1 for b in beads if b.get('title') == '$AGENT_NAME' and b.get('type') == 'agent')
print(count)
" 2>/dev/null || echo "0")

  if [ "$match_count" -ge 3 ]; then
    pass "3 agent beads with same name exist ($match_count found)"
  else
    fail "3 agent beads with same name exist" "expected >=3, found $match_count"
  fi

  # Verify the latest one has the thread metadata intact
  local cycle3_show
  cycle3_show=$("$KD_BIN" show "$cycle3_id" --json 2>/dev/null || echo "{}")
  assert_contains "cycle-3 preserves thread_channel" "C0TEST123" "$cycle3_show"
  assert_contains "cycle-3 preserves thread_ts" "1700000000.000001" "$cycle3_show"
  assert_contains "cycle-3 has slack-thread-resume" "slack-thread-resume" "$cycle3_show"
  echo

  # ── Scenario 7: Resume after clean exit — session chain ────────
  blue "Scenario 7: Resume after clean exit — session chain continuity"

  # Create a fresh persistent agent (crew role with workspace PVC)
  local clean_agent_name="e2e-clean-exit-$TEST_TAG"
  local clean_agent_out clean_agent_id
  clean_agent_out=$(beads_curl -sf -X POST "$BEADS_HTTP_URL/v1/beads" \
    -H 'Content-Type: application/json' \
    -d "$(cat <<CE1EOF
{
  "title": "$clean_agent_name",
  "type": "agent",
  "description": "E2E clean-exit resume test — safe to delete",
  "labels": ["e2e:$TEST_TAG", "project:gardenio"],
  "fields": {
    "agent": "$clean_agent_name",
    "agent_state": "working",
    "mode": "crew",
    "project": "gardenio",
    "role": "crew"
  }
}
CE1EOF
  )") || true
  clean_agent_id=$(echo "$clean_agent_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$clean_agent_id" ]; then
    pass "clean-exit agent created ($clean_agent_id)"
    CREATED_BEADS+=("$clean_agent_id")
  else
    fail "clean-exit agent created" "response: $clean_agent_out"
  fi

  # Session 1: initial session, runs normally, exits cleanly
  local ce_s1_out ce_s1_id
  ce_s1_out=$(beads_curl -sf -X POST "$BEADS_HTTP_URL/v1/beads" \
    -H 'Content-Type: application/json' \
    -d "$(cat <<CE2EOF
{
  "title": "session-clean-1-$TEST_TAG",
  "type": "session",
  "labels": ["e2e:$TEST_TAG"],
  "fields": {
    "agent": "$clean_agent_name",
    "agent_bead_id": "$clean_agent_id",
    "project": "gardenio",
    "role": "crew",
    "hostname": "e2e-pod-1",
    "started_at": "2026-03-25T12:00:00Z",
    "resumed": false,
    "status": "active",
    "input_tokens": 0,
    "output_tokens": 0
  }
}
CE2EOF
  )") || true
  ce_s1_id=$(echo "$ce_s1_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$ce_s1_id" ]; then
    pass "clean-exit session 1 created ($ce_s1_id)"
    CREATED_BEADS+=("$ce_s1_id")
  else
    fail "clean-exit session 1 created" "response: $ce_s1_out"
  fi

  # Agent does gb done → session ends cleanly
  "$KD_BIN" update "$ce_s1_id" \
    -f 'status=completed' -f 'exit_code=0' \
    -f "ended_at=2026-03-25T12:30:00Z" \
    -f 'input_tokens=10000' -f 'output_tokens=5000' \
    -f 'request_count=25' -f 'compaction_count=1' \
    2>/dev/null || true
  "$KD_BIN" close "$ce_s1_id" 2>/dev/null || true

  # Controller restarts pod with BOAT_SESSION_RESUME=1 →
  # gb init finds session log → coop --resume → new session
  local ce_s2_out ce_s2_id
  ce_s2_out=$(beads_curl -sf -X POST "$BEADS_HTTP_URL/v1/beads" \
    -H 'Content-Type: application/json' \
    -d "$(cat <<CE3EOF
{
  "title": "session-clean-2-$TEST_TAG",
  "type": "session",
  "labels": ["e2e:$TEST_TAG"],
  "fields": {
    "agent": "$clean_agent_name",
    "agent_bead_id": "$clean_agent_id",
    "project": "gardenio",
    "role": "crew",
    "hostname": "e2e-pod-1",
    "started_at": "2026-03-25T12:31:00Z",
    "resumed": true,
    "status": "active",
    "input_tokens": 0,
    "output_tokens": 0
  }
}
CE3EOF
  )") || true
  ce_s2_id=$(echo "$ce_s2_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$ce_s2_id" ]; then
    pass "clean-exit resumed session created ($ce_s2_id)"
    CREATED_BEADS+=("$ce_s2_id")
  else
    fail "clean-exit resumed session created" "response: $ce_s2_out"
  fi

  # Verify session chain: s1 closed, s2 resumed, same agent
  local ce_s1_show ce_s2_show
  ce_s1_show=$("$KD_BIN" show "$ce_s1_id" --json 2>/dev/null || echo "{}")
  ce_s2_show=$("$KD_BIN" show "$ce_s2_id" --json 2>/dev/null || echo "{}")

  local s1_status s2_resumed s2_agent_bead s1_compaction
  s1_status=$(json_nested "$ce_s1_show" "fields.status")
  assert_equals "session 1 status=completed after clean exit" "completed" "$s1_status"

  s2_resumed=$(json_nested "$ce_s2_show" "fields.resumed")
  assert_equals "session 2 has resumed=true" "true" "$s2_resumed"

  s2_agent_bead=$(json_nested "$ce_s2_show" "fields.agent_bead_id")
  assert_equals "session 2 links to same agent" "$clean_agent_id" "$s2_agent_bead"

  # Verify session 1 metrics survived close
  s1_compaction=$(json_nested "$ce_s1_show" "fields.compaction_count")
  assert_equals "session 1 compaction_count preserved" "1" "$s1_compaction"

  # Verify the resumed session runs on the same hostname (same pod/PVC)
  local s1_host s2_host
  s1_host=$(json_nested "$ce_s1_show" "fields.hostname")
  s2_host=$(json_nested "$ce_s2_show" "fields.hostname")
  assert_equals "resumed session on same host" "$s1_host" "$s2_host"
  echo

  # ── Scenario 8: Resume after credential switch ──────────────────
  blue "Scenario 8: Resume after credential switch — auth config lifecycle"

  # Create a config:auth bead simulating a project's auth state
  local auth_bead_out auth_bead_id
  auth_bead_out=$(beads_curl -sf -X POST "$BEADS_HTTP_URL/v1/beads" \
    -H 'Content-Type: application/json' \
    -d "$(cat <<AU1EOF
{
  "title": "auth",
  "type": "config",
  "description": "E2E auth config for credential switch test",
  "labels": ["e2e:$TEST_TAG", "project:gardenio"],
  "fields": {
    "value": {"primary_account": "gardenio", "active_account": "gardenio", "failed_over_at": "", "failover_reason": ""}
  }
}
AU1EOF
  )") || true
  auth_bead_id=$(echo "$auth_bead_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$auth_bead_id" ]; then
    pass "auth config bead created ($auth_bead_id)"
    CREATED_BEADS+=("$auth_bead_id")
  else
    fail "auth config bead created" "response: $auth_bead_out"
  fi

  # Create session that's active during credential switch
  local cred_agent_name="e2e-cred-switch-$TEST_TAG"
  local cred_agent_out cred_agent_id
  cred_agent_out=$(beads_curl -sf -X POST "$BEADS_HTTP_URL/v1/beads" \
    -H 'Content-Type: application/json' \
    -d "$(cat <<CR1EOF
{
  "title": "$cred_agent_name",
  "type": "agent",
  "description": "E2E credential switch test agent — safe to delete",
  "labels": ["e2e:$TEST_TAG", "project:gardenio"],
  "fields": {
    "agent": "$cred_agent_name",
    "agent_state": "working",
    "mode": "crew",
    "project": "gardenio",
    "role": "crew"
  }
}
CR1EOF
  )") || true
  cred_agent_id=$(echo "$cred_agent_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$cred_agent_id" ]; then
    pass "cred-switch agent created ($cred_agent_id)"
    CREATED_BEADS+=("$cred_agent_id")
  else
    fail "cred-switch agent created" "response: $cred_agent_out"
  fi

  # Session 1: running with primary credential (gardenio account)
  local cr_s1_out cr_s1_id
  cr_s1_out=$(beads_curl -sf -X POST "$BEADS_HTTP_URL/v1/beads" \
    -H 'Content-Type: application/json' \
    -d "$(cat <<CR2EOF
{
  "title": "session-cred-1-$TEST_TAG",
  "type": "session",
  "labels": ["e2e:$TEST_TAG"],
  "fields": {
    "agent": "$cred_agent_name",
    "agent_bead_id": "$cred_agent_id",
    "project": "gardenio",
    "role": "crew",
    "hostname": "e2e-cred-pod",
    "started_at": "2026-03-25T14:00:00Z",
    "resumed": false,
    "status": "active"
  }
}
CR2EOF
  )") || true
  cr_s1_id=$(echo "$cr_s1_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$cr_s1_id" ]; then
    pass "cred-switch session 1 created ($cr_s1_id)"
    CREATED_BEADS+=("$cr_s1_id")
  else
    fail "cred-switch session 1 created" "response: $cr_s1_out"
  fi

  # Simulate credential switch: budget_exceeded → failover to acme-shared
  # Update the auth config bead's value field (what authFailover.Handle does)
  beads_curl -sf -X PATCH "$BEADS_HTTP_URL/v1/beads/$auth_bead_id" \
    -H 'Content-Type: application/json' \
    -d '{"fields":{"value":{"primary_account":"gardenio","active_account":"acme-shared","failed_over_at":"2026-03-25T14:15:00Z","failover_reason":"budget_exceeded"}}}' \
    >/dev/null 2>&1 || true

  # Session ends (model-only deferred = coop exits for token refresh)
  "$KD_BIN" update "$cr_s1_id" \
    -f 'status=completed' -f 'exit_code=0' \
    -f "ended_at=2026-03-25T14:15:30Z" \
    2>/dev/null || true
  "$KD_BIN" close "$cr_s1_id" 2>/dev/null || true

  # Session 2: resumes with new credential (acme-shared)
  local cr_s2_out cr_s2_id
  cr_s2_out=$(beads_curl -sf -X POST "$BEADS_HTTP_URL/v1/beads" \
    -H 'Content-Type: application/json' \
    -d "$(cat <<CR3EOF
{
  "title": "session-cred-2-$TEST_TAG",
  "type": "session",
  "labels": ["e2e:$TEST_TAG"],
  "fields": {
    "agent": "$cred_agent_name",
    "agent_bead_id": "$cred_agent_id",
    "project": "gardenio",
    "role": "crew",
    "hostname": "e2e-cred-pod",
    "started_at": "2026-03-25T14:16:00Z",
    "resumed": true,
    "status": "active"
  }
}
CR3EOF
  )") || true
  cr_s2_id=$(echo "$cr_s2_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$cr_s2_id" ]; then
    pass "cred-switch resumed session created ($cr_s2_id)"
    CREATED_BEADS+=("$cr_s2_id")
  else
    fail "cred-switch resumed session created" "response: $cr_s2_out"
  fi

  # Verify auth config shows failover state
  local auth_show
  auth_show=$("$KD_BIN" show "$auth_bead_id" --json 2>/dev/null || echo "{}")
  local active_acct failover_reason
  active_acct=$(json_nested "$auth_show" "fields.value.active_account")
  assert_equals "auth switched to acme-shared" "acme-shared" "$active_acct"
  failover_reason=$(json_nested "$auth_show" "fields.value.failover_reason")
  assert_equals "failover reason is budget_exceeded" "budget_exceeded" "$failover_reason"

  # Verify resumed session links to same agent
  local cr_s2_show cr_s2_resumed cr_s2_agent
  cr_s2_show=$("$KD_BIN" show "$cr_s2_id" --json 2>/dev/null || echo "{}")
  cr_s2_resumed=$(json_nested "$cr_s2_show" "fields.resumed")
  assert_equals "cred-switch session 2 has resumed=true" "true" "$cr_s2_resumed"
  cr_s2_agent=$(json_nested "$cr_s2_show" "fields.agent_bead_id")
  assert_equals "cred-switch session 2 links to same agent" "$cred_agent_id" "$cr_s2_agent"

  # Verify same hostname (PVC preserved across credential switch)
  local cr_s2_host
  cr_s2_host=$(json_nested "$cr_s2_show" "fields.hostname")
  assert_equals "cred-switch resumed on same host" "e2e-cred-pod" "$cr_s2_host"
  echo

  # ── Scenario 9: Resume after pod restart (crash recovery) ───────
  blue "Scenario 9: Resume after pod restart — crash recovery"

  # Create agent that simulates a pod crash (OOM kill, etc.)
  local crash_agent_name="e2e-crash-resume-$TEST_TAG"
  local crash_agent_out crash_agent_id
  crash_agent_out=$(beads_curl -sf -X POST "$BEADS_HTTP_URL/v1/beads" \
    -H 'Content-Type: application/json' \
    -d "$(cat <<PD1EOF
{
  "title": "$crash_agent_name",
  "type": "agent",
  "description": "E2E pod restart test agent — safe to delete",
  "labels": ["e2e:$TEST_TAG", "project:gardenio"],
  "fields": {
    "agent": "$crash_agent_name",
    "agent_state": "working",
    "mode": "crew",
    "project": "gardenio",
    "role": "crew"
  }
}
PD1EOF
  )") || true
  crash_agent_id=$(echo "$crash_agent_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$crash_agent_id" ]; then
    pass "crash-resume agent created ($crash_agent_id)"
    CREATED_BEADS+=("$crash_agent_id")
  else
    fail "crash-resume agent created" "response: $crash_agent_out"
  fi

  # Session 1: was running, then pod gets OOMKilled
  local pd_s1_out pd_s1_id
  pd_s1_out=$(beads_curl -sf -X POST "$BEADS_HTTP_URL/v1/beads" \
    -H 'Content-Type: application/json' \
    -d "$(cat <<PD2EOF
{
  "title": "session-crash-1-$TEST_TAG",
  "type": "session",
  "labels": ["e2e:$TEST_TAG"],
  "fields": {
    "agent": "$crash_agent_name",
    "agent_bead_id": "$crash_agent_id",
    "project": "gardenio",
    "role": "crew",
    "hostname": "e2e-crash-pod",
    "started_at": "2026-03-25T16:00:00Z",
    "resumed": false,
    "status": "active",
    "input_tokens": 8000,
    "output_tokens": 3000,
    "request_count": 15
  }
}
PD2EOF
  )") || true
  pd_s1_id=$(echo "$pd_s1_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$pd_s1_id" ]; then
    pass "crash session 1 created ($pd_s1_id)"
    CREATED_BEADS+=("$pd_s1_id")
  else
    fail "crash session 1 created" "response: $pd_s1_out"
  fi

  # Pod crashes — session ends with exit_code=137 (SIGKILL), status=crashed
  "$KD_BIN" update "$pd_s1_id" \
    -f 'status=crashed' -f 'exit_code=137' \
    -f "ended_at=2026-03-25T16:20:00Z" \
    2>/dev/null || true
  "$KD_BIN" close "$pd_s1_id" 2>/dev/null || true

  # Verify crashed session state
  local pd_s1_show
  pd_s1_show=$("$KD_BIN" show "$pd_s1_id" --json 2>/dev/null || echo "{}")
  local crash_exit crash_status
  crash_exit=$(json_nested "$pd_s1_show" "fields.exit_code")
  assert_equals "crashed session exit_code=137" "137" "$crash_exit"
  crash_status=$(json_nested "$pd_s1_show" "fields.status")
  assert_equals "crashed session status=crashed" "crashed" "$crash_status"

  # K8s restarts pod → gb init finds session log → coop --resume.
  # In a real cluster the controller's statusreporter writes agent_state from
  # pod phase, so the test cannot deterministically simulate the transition
  # here — the reconciler will overwrite any value we set. We only assert on
  # the session bead's resume metadata below; agent_state recovery is covered
  # by the agent-lifecycle suite which spins up real pods.

  # New session starts with resumed=true on the restarted pod
  local pd_s2_out pd_s2_id
  pd_s2_out=$(beads_curl -sf -X POST "$BEADS_HTTP_URL/v1/beads" \
    -H 'Content-Type: application/json' \
    -d "$(cat <<PD3EOF
{
  "title": "session-crash-2-$TEST_TAG",
  "type": "session",
  "labels": ["e2e:$TEST_TAG"],
  "fields": {
    "agent": "$crash_agent_name",
    "agent_bead_id": "$crash_agent_id",
    "project": "gardenio",
    "role": "crew",
    "hostname": "e2e-crash-pod",
    "started_at": "2026-03-25T16:21:00Z",
    "resumed": true,
    "status": "active",
    "input_tokens": 0,
    "output_tokens": 0,
    "request_count": 0
  }
}
PD3EOF
  )") || true
  pd_s2_id=$(echo "$pd_s2_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$pd_s2_id" ]; then
    pass "crash-resume session 2 created ($pd_s2_id)"
    CREATED_BEADS+=("$pd_s2_id")
  else
    fail "crash-resume session 2 created" "response: $pd_s2_out"
  fi

  # Verify the full crash → resume chain
  local pd_s2_show
  pd_s2_show=$("$KD_BIN" show "$pd_s2_id" --json 2>/dev/null || echo "{}")
  local pd_s2_resumed pd_s2_agent pd_s2_host
  pd_s2_resumed=$(json_nested "$pd_s2_show" "fields.resumed")
  assert_equals "crash-resume session has resumed=true" "true" "$pd_s2_resumed"

  pd_s2_agent=$(json_nested "$pd_s2_show" "fields.agent_bead_id")
  assert_equals "crash-resume links to same agent" "$crash_agent_id" "$pd_s2_agent"

  pd_s2_host=$(json_nested "$pd_s2_show" "fields.hostname")
  assert_equals "crash-resume on same host (PVC preserved)" "e2e-crash-pod" "$pd_s2_host"

  # Verify the resumed session starts with fresh counters
  local pd_s2_tokens
  pd_s2_tokens=$(json_nested "$pd_s2_show" "fields.input_tokens")
  assert_equals "crash-resume starts with fresh token count" "0" "$pd_s2_tokens"

  # Count total sessions for this agent — should be exactly 2
  local crash_sessions crash_session_count
  crash_sessions=$("$KD_BIN" list --all-types --json -l "e2e:$TEST_TAG" 2>/dev/null || echo "[]")
  crash_session_count=$(echo "$crash_sessions" | python3 -c "
import sys, json
beads = json.load(sys.stdin)
if isinstance(beads, dict) and 'items' in beads:
    beads = beads['items']
count = sum(1 for b in beads if b.get('type') == 'session' and '$crash_agent_name' in json.dumps(b.get('fields', {})))
print(count)
" 2>/dev/null || echo "0")
  if [ "$crash_session_count" -ge 2 ]; then
    pass "crash agent has 2 sessions ($crash_session_count found)"
  else
    fail "crash agent has 2 sessions" "expected >=2, found $crash_session_count"
  fi
  echo

  # ── Scenario 10: Both arms — project session_resume=true (enabled)
  blue "Scenario 10: Both arms — project session_resume=true (enabled arm)"

  local on_project="e2e-resume-on-$TEST_TAG"
  local on_proj_out on_proj_id
  on_proj_out=$(beads_curl -sf -X POST "$BEADS_HTTP_URL/v1/beads" \
    -H 'Content-Type: application/json' \
    -d "$(cat <<P10EOF
{
  "title": "$on_project",
  "type": "project",
  "description": "E2E session-resume enabled-arm project — safe to delete",
  "labels": ["e2e:$TEST_TAG"],
  "fields": {
    "prefix": "kd",
    "git_url": "https://example.invalid/$on_project.git",
    "default_branch": "main",
    "session_resume": "true"
  }
}
P10EOF
  )") || true
  on_proj_id=$(echo "$on_proj_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$on_proj_id" ]; then
    pass "enabled-arm project bead created ($on_proj_id)"
    CREATED_BEADS+=("$on_proj_id")
  else
    fail "enabled-arm project bead created" "response: $on_proj_out"
  fi

  local on_proj_show on_proj_resume
  on_proj_show=$("$KD_BIN" show "$on_proj_id" --json 2>/dev/null || echo "{}")
  on_proj_resume=$(json_nested "$on_proj_show" "fields.session_resume")
  assert_equals "enabled-arm project session_resume=true" "true" "$on_proj_resume"

  # Simulate the enabled-arm session lifecycle: agent + workspace storage,
  # initial session ends, next session resumes. With session_resume=true and
  # workspace_storage present, the controller would set BOAT_SESSION_RESUME=1.
  local on_agent_name="e2e-resume-on-agent-$TEST_TAG"
  local on_agent_out on_agent_id
  on_agent_out=$(beads_curl -sf -X POST "$BEADS_HTTP_URL/v1/beads" \
    -H 'Content-Type: application/json' \
    -d "$(cat <<P10AEOF
{
  "title": "$on_agent_name",
  "type": "agent",
  "description": "E2E enabled-arm agent — safe to delete",
  "labels": ["e2e:$TEST_TAG", "project:$on_project"],
  "fields": {
    "agent": "$on_agent_name",
    "agent_state": "working",
    "mode": "crew",
    "project": "$on_project",
    "role": "crew"
  }
}
P10AEOF
  )") || true
  on_agent_id=$(echo "$on_agent_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$on_agent_id" ]; then
    pass "enabled-arm agent created ($on_agent_id)"
    CREATED_BEADS+=("$on_agent_id")
  else
    fail "enabled-arm agent created" "response: $on_agent_out"
  fi

  local on_s1_out on_s1_id
  on_s1_out=$(beads_curl -sf -X POST "$BEADS_HTTP_URL/v1/beads" \
    -H 'Content-Type: application/json' \
    -d "$(cat <<P10S1EOF
{
  "title": "session-on-1-$TEST_TAG",
  "type": "session",
  "labels": ["e2e:$TEST_TAG"],
  "fields": {
    "agent": "$on_agent_name",
    "agent_bead_id": "$on_agent_id",
    "project": "$on_project",
    "role": "crew",
    "hostname": "e2e-resume-on-pod",
    "started_at": "2026-04-01T08:00:00Z",
    "resumed": false,
    "status": "active",
    "input_tokens": 4000,
    "output_tokens": 1500
  }
}
P10S1EOF
  )") || true
  on_s1_id=$(echo "$on_s1_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$on_s1_id" ]; then
    pass "enabled-arm session 1 created ($on_s1_id)"
    CREATED_BEADS+=("$on_s1_id")
  else
    fail "enabled-arm session 1 created" "response: $on_s1_out"
  fi

  # Pod restart — with session_resume=true, the next session is resumed.
  "$KD_BIN" update "$on_s1_id" -f 'status=completed' -f 'exit_code=0' \
    -f "ended_at=2026-04-01T08:30:00Z" 2>/dev/null || true
  "$KD_BIN" close "$on_s1_id" 2>/dev/null || true

  local on_s2_out on_s2_id
  on_s2_out=$(beads_curl -sf -X POST "$BEADS_HTTP_URL/v1/beads" \
    -H 'Content-Type: application/json' \
    -d "$(cat <<P10S2EOF
{
  "title": "session-on-2-$TEST_TAG",
  "type": "session",
  "labels": ["e2e:$TEST_TAG"],
  "fields": {
    "agent": "$on_agent_name",
    "agent_bead_id": "$on_agent_id",
    "project": "$on_project",
    "role": "crew",
    "hostname": "e2e-resume-on-pod",
    "started_at": "2026-04-01T08:31:00Z",
    "resumed": true,
    "status": "active"
  }
}
P10S2EOF
  )") || true
  on_s2_id=$(echo "$on_s2_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$on_s2_id" ]; then
    pass "enabled-arm session 2 created ($on_s2_id)"
    CREATED_BEADS+=("$on_s2_id")
  else
    fail "enabled-arm session 2 created" "response: $on_s2_out"
  fi

  local on_s2_show on_s2_resumed
  on_s2_show=$("$KD_BIN" show "$on_s2_id" --json 2>/dev/null || echo "{}")
  on_s2_resumed=$(json_nested "$on_s2_show" "fields.resumed")
  assert_equals "enabled arm: session 2 has resumed=true" "true" "$on_s2_resumed"
  echo

  # ── Scenario 11: Both arms — project session_resume=false (disabled)
  blue "Scenario 11: Both arms — project session_resume=false (disabled arm)"

  local off_project="e2e-resume-off-$TEST_TAG"
  local off_proj_out off_proj_id
  off_proj_out=$(beads_curl -sf -X POST "$BEADS_HTTP_URL/v1/beads" \
    -H 'Content-Type: application/json' \
    -d "$(cat <<P11EOF
{
  "title": "$off_project",
  "type": "project",
  "description": "E2E session-resume disabled-arm project — safe to delete",
  "labels": ["e2e:$TEST_TAG"],
  "fields": {
    "prefix": "kd",
    "git_url": "https://example.invalid/$off_project.git",
    "default_branch": "main",
    "session_resume": "false"
  }
}
P11EOF
  )") || true
  off_proj_id=$(echo "$off_proj_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$off_proj_id" ]; then
    pass "disabled-arm project bead created ($off_proj_id)"
    CREATED_BEADS+=("$off_proj_id")
  else
    fail "disabled-arm project bead created" "response: $off_proj_out"
  fi

  local off_proj_show off_proj_resume
  off_proj_show=$("$KD_BIN" show "$off_proj_id" --json 2>/dev/null || echo "{}")
  off_proj_resume=$(json_nested "$off_proj_show" "fields.session_resume")
  assert_equals "disabled-arm project session_resume=false" "false" "$off_proj_resume"

  # Disabled arm: every pod start is a fresh session. Session 2 has resumed=false
  # even though the workspace PVC carries prior state — the controller signals
  # BOAT_SESSION_RESUME=0 and gb init does not pass --resume to coop.
  local off_agent_name="e2e-resume-off-agent-$TEST_TAG"
  local off_agent_out off_agent_id
  off_agent_out=$(beads_curl -sf -X POST "$BEADS_HTTP_URL/v1/beads" \
    -H 'Content-Type: application/json' \
    -d "$(cat <<P11AEOF
{
  "title": "$off_agent_name",
  "type": "agent",
  "description": "E2E disabled-arm agent — safe to delete",
  "labels": ["e2e:$TEST_TAG", "project:$off_project"],
  "fields": {
    "agent": "$off_agent_name",
    "agent_state": "working",
    "mode": "crew",
    "project": "$off_project",
    "role": "crew"
  }
}
P11AEOF
  )") || true
  off_agent_id=$(echo "$off_agent_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$off_agent_id" ]; then
    pass "disabled-arm agent created ($off_agent_id)"
    CREATED_BEADS+=("$off_agent_id")
  else
    fail "disabled-arm agent created" "response: $off_agent_out"
  fi

  local off_s1_out off_s1_id
  off_s1_out=$(beads_curl -sf -X POST "$BEADS_HTTP_URL/v1/beads" \
    -H 'Content-Type: application/json' \
    -d "$(cat <<P11S1EOF
{
  "title": "session-off-1-$TEST_TAG",
  "type": "session",
  "labels": ["e2e:$TEST_TAG"],
  "fields": {
    "agent": "$off_agent_name",
    "agent_bead_id": "$off_agent_id",
    "project": "$off_project",
    "role": "crew",
    "hostname": "e2e-resume-off-pod",
    "started_at": "2026-04-01T09:00:00Z",
    "resumed": false,
    "status": "active",
    "input_tokens": 6000,
    "output_tokens": 2500
  }
}
P11S1EOF
  )") || true
  off_s1_id=$(echo "$off_s1_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$off_s1_id" ]; then
    pass "disabled-arm session 1 created ($off_s1_id)"
    CREATED_BEADS+=("$off_s1_id")
  else
    fail "disabled-arm session 1 created" "response: $off_s1_out"
  fi

  "$KD_BIN" update "$off_s1_id" -f 'status=completed' -f 'exit_code=0' \
    -f "ended_at=2026-04-01T09:30:00Z" 2>/dev/null || true
  "$KD_BIN" close "$off_s1_id" 2>/dev/null || true

  # Pod restart with session_resume=false → fresh session, NOT resumed.
  local off_s2_out off_s2_id
  off_s2_out=$(beads_curl -sf -X POST "$BEADS_HTTP_URL/v1/beads" \
    -H 'Content-Type: application/json' \
    -d "$(cat <<P11S2EOF
{
  "title": "session-off-2-$TEST_TAG",
  "type": "session",
  "labels": ["e2e:$TEST_TAG"],
  "fields": {
    "agent": "$off_agent_name",
    "agent_bead_id": "$off_agent_id",
    "project": "$off_project",
    "role": "crew",
    "hostname": "e2e-resume-off-pod",
    "started_at": "2026-04-01T09:31:00Z",
    "resumed": false,
    "status": "active",
    "input_tokens": 0,
    "output_tokens": 0
  }
}
P11S2EOF
  )") || true
  off_s2_id=$(echo "$off_s2_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$off_s2_id" ]; then
    pass "disabled-arm session 2 created ($off_s2_id)"
    CREATED_BEADS+=("$off_s2_id")
  else
    fail "disabled-arm session 2 created" "response: $off_s2_out"
  fi

  local off_s2_show off_s2_resumed off_s2_tokens off_s2_host
  off_s2_show=$("$KD_BIN" show "$off_s2_id" --json 2>/dev/null || echo "{}")
  off_s2_resumed=$(json_nested "$off_s2_show" "fields.resumed")
  assert_equals "disabled arm: session 2 has resumed=false (fresh start)" \
    "false" "$off_s2_resumed"

  # Counters reset on fresh start (no prior session to inherit metrics from).
  off_s2_tokens=$(json_nested "$off_s2_show" "fields.input_tokens")
  assert_equals "disabled arm: session 2 starts with fresh token count" \
    "0" "$off_s2_tokens"

  # Same hostname (the PVC persists; only the conversation does not).
  off_s2_host=$(json_nested "$off_s2_show" "fields.hostname")
  assert_equals "disabled arm: session 2 still runs on same host" \
    "e2e-resume-off-pod" "$off_s2_host"

  # Cross-arm sanity: the two project beads carry opposite session_resume values.
  if [ "$on_proj_resume" = "true" ] && [ "$off_proj_resume" = "false" ]; then
    pass "both arms: project beads carry opposite session_resume values"
  else
    fail "both arms: project beads carry opposite session_resume values" \
      "on=$on_proj_resume off=$off_proj_resume"
  fi
  echo

  # ── Scenario 12: Role bead session_resume=false overrides project ───
  blue "Scenario 12: Role bead session_resume=false overrides project default"

  # Project unset (defaults to enabled when workspace PVC is present), role
  # explicitly disables. Role takes precedence — project < role < agent.
  local mixed_project="e2e-resume-mixed-$TEST_TAG"
  local mixed_proj_out mixed_proj_id
  mixed_proj_out=$(beads_curl -sf -X POST "$BEADS_HTTP_URL/v1/beads" \
    -H 'Content-Type: application/json' \
    -d "$(cat <<P12PEOF
{
  "title": "$mixed_project",
  "type": "project",
  "description": "E2E mixed project (no session_resume override) — safe to delete",
  "labels": ["e2e:$TEST_TAG"],
  "fields": {
    "prefix": "kd",
    "git_url": "https://example.invalid/$mixed_project.git",
    "default_branch": "main"
  }
}
P12PEOF
  )") || true
  mixed_proj_id=$(echo "$mixed_proj_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$mixed_proj_id" ]; then
    pass "mixed project bead created ($mixed_proj_id)"
    CREATED_BEADS+=("$mixed_proj_id")
  else
    fail "mixed project bead created" "response: $mixed_proj_out"
  fi

  local ephemeral_role="e2e-ephemeral-$TEST_TAG"
  local role_out role_id
  role_out=$(beads_curl -sf -X POST "$BEADS_HTTP_URL/v1/beads" \
    -H 'Content-Type: application/json' \
    -d "$(cat <<P12REOF
{
  "title": "$ephemeral_role",
  "type": "role",
  "description": "E2E ephemeral role with session_resume=false — safe to delete",
  "labels": ["e2e:$TEST_TAG"],
  "fields": {
    "session_resume": "false"
  }
}
P12REOF
  )") || true
  role_id=$(echo "$role_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$role_id" ]; then
    pass "ephemeral role bead created ($role_id)"
    CREATED_BEADS+=("$role_id")
  else
    fail "ephemeral role bead created" "response: $role_out"
  fi

  local role_show role_resume mixed_proj_show mixed_proj_resume
  role_show=$("$KD_BIN" show "$role_id" --json 2>/dev/null || echo "{}")
  role_resume=$(json_nested "$role_show" "fields.session_resume")
  assert_equals "ephemeral role session_resume=false" "false" "$role_resume"

  mixed_proj_show=$("$KD_BIN" show "$mixed_proj_id" --json 2>/dev/null || echo "{}")
  mixed_proj_resume=$(json_nested "$mixed_proj_show" "fields.session_resume")
  assert_equals "mixed project leaves session_resume unset" "" "$mixed_proj_resume"
  echo

  # ── Scenario 13: Agent metadata session_resume override ─────────────
  blue "Scenario 13: Agent metadata session_resume beats project/role"

  # Agent in the enabled-arm project, but the agent bead itself opts out via
  # the session_resume metadata field. The pod manager honors agent metadata
  # over project/role defaults.
  local override_agent_name="e2e-resume-override-$TEST_TAG"
  local override_out override_id
  override_out=$(beads_curl -sf -X POST "$BEADS_HTTP_URL/v1/beads" \
    -H 'Content-Type: application/json' \
    -d "$(cat <<P13EOF
{
  "title": "$override_agent_name",
  "type": "agent",
  "description": "E2E agent with explicit session_resume=false — safe to delete",
  "labels": ["e2e:$TEST_TAG", "project:$on_project"],
  "fields": {
    "agent": "$override_agent_name",
    "agent_state": "working",
    "mode": "crew",
    "project": "$on_project",
    "role": "crew",
    "session_resume": "false"
  }
}
P13EOF
  )") || true
  override_id=$(echo "$override_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$override_id" ]; then
    pass "agent-metadata-override bead created ($override_id)"
    CREATED_BEADS+=("$override_id")
  else
    fail "agent-metadata-override bead created" "response: $override_out"
  fi

  local override_show override_resume override_proj
  override_show=$("$KD_BIN" show "$override_id" --json 2>/dev/null || echo "{}")
  override_resume=$(json_nested "$override_show" "fields.session_resume")
  assert_equals "agent metadata session_resume=false survives create" \
    "false" "$override_resume"

  override_proj=$(json_nested "$override_show" "fields.project")
  assert_equals "override agent still labeled with enabled-arm project" \
    "$on_project" "$override_proj"
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
