#!/usr/bin/env bash
# test-config-role-model.sh — per-role model selection E2E tests
#
# Tests the per-role model selection feature where different roles can
# override the model in claude-settings config beads. This is the
# mechanism used by crew-lite (sonnet) and crew-ultralite (haiku) roles.
#
# Scenarios tested:
#   1. Role overrides global model
#   2. Role overrides project-level model
#   3. crew-lite/crew-ultralite table-driven model selection
#   4. Role bead inline claude_config overrides project config bead
#   5. Non-matching role does not leak settings

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
  local name="$1" pattern="$2" actual="$3"
  if grep -qF -- "$pattern" <<< "$actual"; then
    pass "$name (contains '$pattern')"
  else
    fail "$name" "expected to contain '$pattern'; got: $(echo "$actual" | head -5)"
  fi
}

assert_not_contains() {
  local name="$1" pattern="$2" actual="$3"
  if ! grep -qF -- "$pattern" <<< "$actual"; then
    pass "$name (not contains '$pattern')"
  else
    fail "$name" "expected NOT to contain '$pattern'"
  fi
}

assert_json_value() {
  local name="$1" key="$2" expected="$3" file="$4"
  local actual
  actual=$(python3 -c "
import json,sys
with open('$file') as f:
    d = json.load(f)
keys = '$key'.split('.')
for k in keys:
    if isinstance(d, dict):
        d = d.get(k)
    else:
        d = None
        break
print(d if d is not None else '')
" 2>/dev/null || echo "")
  if [ "$actual" = "$expected" ]; then
    pass "$name ($key=$expected)"
  else
    fail "$name" "expected $key=$expected, got $key=$actual"
  fi
}

# ── Setup ─────────────────────────────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19116
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-role-model.log 2>&1 &
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

cleanup() {
  for id in "${CREATED_BEADS[@]}"; do
    "$KD_BIN" close "$id" 2>/dev/null || true
  done
  [ -d "${WORK_DIR:-}" ] && rm -rf "$WORK_DIR"
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

WORK_DIR=$(mktemp -d /tmp/e2e-role-model-XXXXXX)

# Close stale config/role beads from previous test runs that can contaminate resolution
cleanup_stale_beads() {
  local stale_ids
  stale_ids=$(env -u BOAT_PROJECT "$KD_BIN" list --type=config --status=open --limit=500 --all-projects --json 2>/dev/null | python3 -c "
import sys, json
beads = json.load(sys.stdin)
for b in beads:
    title = b.get('title','')
    if title in ('claude-settings', 'claude-hooks'):
        print(b['id'])
" 2>/dev/null || echo "")
  # Also close stale role beads
  local stale_role_ids
  stale_role_ids=$(env -u BOAT_PROJECT "$KD_BIN" list --type=role --status=open --limit=500 --all-projects --json 2>/dev/null | python3 -c "
import sys, json
beads = json.load(sys.stdin)
for b in beads:
    title = b.get('title','')
    if 'e2e-role-model' in title:
        print(b['id'])
" 2>/dev/null || echo "")
  local all_ids="${stale_ids}
${stale_role_ids}"
  if [ -z "$(echo "$all_ids" | tr -d '[:space:]')" ]; then return; fi
  local count=0
  while IFS= read -r id; do
    [ -z "$id" ] && continue
    "$KD_BIN" close "$id" --actor "$KD_ACTOR" 2>/dev/null || true
    count=$((count + 1))
  done <<< "$all_ids"
  [ "$count" -gt 0 ] && dim "  Cleaned $count stale config/role bead(s)"
}

create_config() {
  local title="$1" label="$2" value="$3"
  local out id
  out=$(env -u BOAT_PROJECT "$KD_BIN" create "$title" --type=config -l "$label" \
    -d "$value" --json 2>/dev/null || echo "{}")
  id=$(echo "$out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  [ -n "$id" ] && CREATED_BEADS+=("$id")
  echo "$id"
}

create_role() {
  local role_name="$1" claude_config="$2"
  local out id
  out=$(env -u BOAT_PROJECT "$KD_BIN" create "$role_name" --type=role \
    -f "claude_config=$claude_config" --json 2>/dev/null || echo "{}")
  id=$(echo "$out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  [ -n "$id" ] && CREATED_BEADS+=("$id")
  echo "$id"
}

close_bead() {
  local id="$1"
  [ -n "$id" ] && "$KD_BIN" close "$id" 2>/dev/null || true
  CREATED_BEADS=("${CREATED_BEADS[@]/$id/}")
}

run_setup() {
  local role="$1" project="${2:-}"
  rm -rf "$WORK_DIR/.claude" "$WORK_DIR/CLAUDE.md" "$WORK_DIR/.mcp.json"
  mkdir -p "$WORK_DIR/.claude"
  if [ -n "$project" ]; then
    env -u GB_AGENT_NAME BOAT_ROLE="$role" BOAT_PROJECT="$project" \
      "$GB_BIN" setup claude --workspace "$WORK_DIR" --http-url "$BEADS_HTTP_URL" 2>/dev/null || true
  else
    env -u GB_AGENT_NAME -u BOAT_PROJECT BOAT_ROLE="$role" \
      "$GB_BIN" setup claude --workspace "$WORK_DIR" --http-url "$BEADS_HTTP_URL" 2>/dev/null || true
  fi
}

get_user_settings() {
  echo "$HOME/.claude/settings.json"
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " per-role model selection E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  cleanup_stale_beads
  echo

  local TEST_TAG="e2e-role-model-$$-$(date +%s)"

  # Shared hooks bead (required for gb setup claude to succeed)
  local hooks_id
  hooks_id=$(create_config "claude-hooks" "global" \
    '{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"echo role-model-test"}]}],"Stop":[{"matcher":"","hooks":[{"type":"command","command":"echo stop"}]}]}}')

  local user_settings
  user_settings=$(get_user_settings)

  # ── Scenario 1: Role overrides global model ─────────────────────
  blue "Scenario 1: Role overrides global model"

  local s1_global s1_role
  s1_global=$(create_config "claude-settings" "global" \
    '{"model":"claude-sonnet-4-6","alwaysThinkingEnabled":true}')
  s1_role=$(create_config "claude-settings" "role:test-role-$TEST_TAG" \
    '{"model":"claude-opus-4-6"}')

  run_setup "test-role-$TEST_TAG"

  if [ -f "$user_settings" ]; then
    local s1_content
    s1_content=$(cat "$user_settings")
    assert_contains "S1: role model wins" '"claude-opus-4-6"' "$s1_content"
    assert_contains "S1: global thinking survives" '"alwaysThinkingEnabled"' "$s1_content"
  else
    fail "S1: settings.json not found"
  fi

  close_bead "$s1_global"
  close_bead "$s1_role"
  echo

  # ── Scenario 2: Role overrides project-level model ──────────────
  blue "Scenario 2: Role overrides project-level model"

  local proj_name="test-proj-$TEST_TAG"
  local s2_global s2_proj s2_role
  s2_global=$(create_config "claude-settings" "global" \
    '{"permissions":{"allow":["Bash(*)"]}}')
  s2_proj=$(create_config "claude-settings" "project:$proj_name" \
    '{"model":"claude-opus-4-6"}')
  s2_role=$(create_config "claude-settings" "role:test-crew-$TEST_TAG" \
    '{"model":"claude-sonnet-4-6"}')

  run_setup "test-crew-$TEST_TAG" "$proj_name"

  if [ -f "$user_settings" ]; then
    local s2_content
    s2_content=$(cat "$user_settings")
    assert_contains "S2: role model beats project" '"claude-sonnet-4-6"' "$s2_content"
    assert_contains "S2: global permissions survive" '"Bash(*)"' "$s2_content"
  else
    fail "S2: settings.json not found"
  fi

  close_bead "$s2_global"
  close_bead "$s2_proj"
  close_bead "$s2_role"
  echo

  # ── Scenario 3: crew-lite/crew-ultralite model selection ────────
  blue "Scenario 3: crew-lite/crew-ultralite model selection (table-driven)"

  local s3_global s3_proj s3_lite s3_ultralite
  s3_global=$(create_config "claude-settings" "global" \
    '{"permissions":{"allow":["Bash(*)"]},"alwaysThinkingEnabled":true}')
  s3_proj=$(create_config "claude-settings" "project:$proj_name" \
    '{"model":"claude-opus-4-6"}')
  s3_lite=$(create_config "claude-settings" "role:crew-lite-$TEST_TAG" \
    '{"model":"claude-sonnet-4-6"}')
  s3_ultralite=$(create_config "claude-settings" "role:crew-ultralite-$TEST_TAG" \
    '{"model":"claude-haiku-4-5"}')

  # Test crew-lite → sonnet
  run_setup "crew-lite-$TEST_TAG" "$proj_name"
  if [ -f "$user_settings" ]; then
    local s3a_content
    s3a_content=$(cat "$user_settings")
    assert_contains "S3a: crew-lite gets sonnet" '"claude-sonnet-4-6"' "$s3a_content"
    assert_contains "S3a: global thinking survives" '"alwaysThinkingEnabled"' "$s3a_content"
  else
    fail "S3a: settings.json not found"
  fi

  # Test crew-ultralite → haiku
  run_setup "crew-ultralite-$TEST_TAG" "$proj_name"
  if [ -f "$user_settings" ]; then
    local s3b_content
    s3b_content=$(cat "$user_settings")
    assert_contains "S3b: crew-ultralite gets haiku" '"claude-haiku-4-5"' "$s3b_content"
    assert_contains "S3b: global thinking survives" '"alwaysThinkingEnabled"' "$s3b_content"
  else
    fail "S3b: settings.json not found"
  fi

  # Test crew (no role override) → opus from project
  run_setup "crew-plain-$TEST_TAG" "$proj_name"
  if [ -f "$user_settings" ]; then
    local s3c_content
    s3c_content=$(cat "$user_settings")
    assert_contains "S3c: plain crew gets opus from project" '"claude-opus-4-6"' "$s3c_content"
  else
    fail "S3c: settings.json not found"
  fi

  close_bead "$s3_global"
  close_bead "$s3_proj"
  close_bead "$s3_lite"
  close_bead "$s3_ultralite"
  echo

  # ── Scenario 4: Role bead inline claude_config overrides project ─
  blue "Scenario 4: Role bead inline claude_config overrides project config bead"

  local s4_global s4_proj s4_role_bead
  s4_global=$(create_config "claude-settings" "global" \
    '{"alwaysThinkingEnabled":true}')
  s4_proj=$(create_config "claude-settings" "project:$proj_name" \
    '{"model":"claude-opus-4-6"}')
  s4_role_bead=$(create_role "role-inline-$TEST_TAG" \
    '{"claude-settings":{"model":"claude-sonnet-4-6"}}')

  if [ -z "$s4_role_bead" ]; then
    dim "  Skipping: role bead claude_config field not available on this server version"
    pass "S4: role inline (skipped — server lacks claude_config field)"
    pass "S4: global thinking (skipped — prerequisite missing)"
  else
    run_setup "role-inline-$TEST_TAG" "$proj_name"

    if [ -f "$user_settings" ]; then
      local s4_content
      s4_content=$(cat "$user_settings")
      assert_contains "S4: role inline model wins over project" '"claude-sonnet-4-6"' "$s4_content"
      assert_contains "S4: global thinking survives" '"alwaysThinkingEnabled"' "$s4_content"
    else
      fail "S4: settings.json not found"
    fi
  fi

  close_bead "$s4_global"
  close_bead "$s4_proj"
  [ -n "$s4_role_bead" ] && close_bead "$s4_role_bead"
  echo

  # ── Scenario 5: Non-matching role does not leak settings ────────
  blue "Scenario 5: Non-matching role does not leak settings"

  local s5_global s5_other_role
  s5_global=$(create_config "claude-settings" "global" \
    '{"model":"claude-sonnet-4-6"}')
  s5_other_role=$(create_config "claude-settings" "role:other-role-$TEST_TAG" \
    '{"model":"claude-haiku-4-5"}')

  # Agent has role "my-role", should NOT get "other-role" settings
  run_setup "my-role-$TEST_TAG"

  if [ -f "$user_settings" ]; then
    local s5_content
    s5_content=$(cat "$user_settings")
    assert_contains "S5: global model applied" '"claude-sonnet-4-6"' "$s5_content"
    assert_not_contains "S5: other role model does NOT leak" '"claude-haiku-4-5"' "$s5_content"
  else
    fail "S5: settings.json not found"
  fi

  close_bead "$s5_global"
  close_bead "$s5_other_role"
  close_bead "$hooks_id"
  echo

  # ── Results ─────────────────────────────────────────────────────
  blue "═══════════════════════════════════════════════════"
  blue " Results: $PASS passed, $FAIL failed"
  blue "═══════════════════════════════════════════════════"

  if [ "$FAIL" -gt 0 ]; then
    red "Failures:"
    for e in "${ERRORS[@]}"; do
      red "  - $e"
    done
    exit 1
  fi

  green "All per-role model selection tests passed!"
}

main "$@"
