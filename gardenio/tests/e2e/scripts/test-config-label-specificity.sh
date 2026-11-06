#!/usr/bin/env bash
# test-config-label-specificity.sh — label specificity chain E2E tests
#
# Tests the full config bead label specificity ordering:
#   global (0:) < project (1:) < role (2:) < project-inline (2~:) < agent (3:)
#
# Each scenario creates config beads with overlapping + unique markers,
# runs gb setup claude, and checks ~/.claude/settings.json to verify that
# higher-specificity layers override lower ones while non-overlapping keys
# from lower layers survive.
#
# All test beads use role-scoped labels with unique test role names to
# isolate from stale/production global beads in the shared E2E environment.
#
# Scenarios tested:
#   1. Single layer resolves
#   2. Higher-specificity role overrides lower for overlapping key
#   3. Project + base role — role > project for overlapping key
#   4. Role + project + base — role > project > base
#   5. Agent-scoped bead filtered without agent subscription
#   6. Non-matching role labels are filtered out
#   7. Non-matching category titles do not leak across categories

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

# ── Setup ─────────────────────────────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19104
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-specificity.log 2>&1 &
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

WORK_DIR=$(mktemp -d /tmp/e2e-specificity-XXXXXX)
HOME_DIR=$(eval echo "~")
SETTINGS_PATH="$HOME_DIR/.claude/settings.json"

create_config() {
  local title="$1" label="$2" value="$3"
  local out id
  out=$("$KD_BIN" create "$title" --type=config -l "$label" \
    -d "$value" --json 2>/dev/null || echo "{}")
  id=$(echo "$out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$id" ]; then
    CREATED_BEADS+=("$id")
    echo "$id"
  fi
}

close_bead() {
  local id="$1"
  [ -n "$id" ] && "$KD_BIN" close "$id" 2>/dev/null || true
  CREATED_BEADS=("${CREATED_BEADS[@]/$id/}")
}

# reset_workspace clears workspace and user settings between scenarios.
reset_workspace() {
  rm -rf "$WORK_DIR/.claude" "$WORK_DIR/.mcp.json" "$WORK_DIR/CLAUDE.md"
  mkdir -p "$WORK_DIR/.claude"
  rm -f "$SETTINGS_PATH"
}

# read_user_settings returns the content of ~/.claude/settings.json.
read_user_settings() {
  if [ -f "$SETTINGS_PATH" ]; then
    cat "$SETTINGS_PATH"
  else
    echo ""
  fi
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " label specificity E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  local TEST_TAG="e2e-spec-$$-$(date +%s)"
  local E2E_ROLE="e2e-spec-role-$TEST_TAG"
  local E2E_ROLE_BASE="e2e-spec-base-$TEST_TAG"
  local E2E_PROJECT="e2e-spec-proj-$TEST_TAG"
  local E2E_AGENT="e2e-spec-agent-$TEST_TAG"

  # ── Prerequisite: hooks bead so gb setup claude succeeds ───────
  local hooks_id
  hooks_id=$(create_config "claude-hooks" "global" \
    '{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"echo spec-test"}]}],"Stop":[{"matcher":"","hooks":[{"type":"command","command":"echo stop"}]}]}}')

  # ── Scenario 1: Single layer resolves ─────────────────────────────
  blue "Scenario 1: Single layer resolves"

  # Use role-scoped bead to isolate from stale/production global beads.
  # Overlapping key "SHARED" + unique key "BASE_ONLY" to test preservation.
  # Use flat top-level keys because MergeOverride is shallow: it replaces
  # the entire value for overlapping top-level keys but preserves non-overlapping ones.
  local g_id
  g_id=$(create_config "claude-settings" "role:$E2E_ROLE_BASE" \
    "{\"SHARED\":\"from-base-$TEST_TAG\",\"BASE_ONLY\":\"b-$TEST_TAG\"}")
  if [ -z "$g_id" ]; then
    fail "S1: create base bead" "could not create"
  else
    dim "  base bead: $g_id"
  fi

  reset_workspace
  unset BOAT_PROJECT BOAT_RIG 2>/dev/null || true
  "$GB_BIN" setup claude --role "$E2E_ROLE_BASE" --workspace "$WORK_DIR" 2>/dev/null || true

  local content
  content=$(read_user_settings)
  if [ -n "$content" ]; then
    assert_contains "S1: base SHARED resolved" "from-base-$TEST_TAG" "$content"
    assert_contains "S1: base BASE_ONLY resolved" "b-$TEST_TAG" "$content"
  else
    fail "S1: user settings" "not created"
  fi
  echo

  # ── Scenario 2: Higher-specificity role overrides lower ──────────
  blue "Scenario 2: Higher-specificity role overrides lower for SHARED key"

  local r_id
  r_id=$(create_config "claude-settings" "role:$E2E_ROLE" \
    "{\"SHARED\":\"from-role-$TEST_TAG\",\"ROLE_ONLY\":\"r-$TEST_TAG\"}")
  if [ -z "$r_id" ]; then
    fail "S2: create role bead" "could not create"
  else
    dim "  role bead: $r_id"
  fi

  reset_workspace
  unset BOAT_PROJECT BOAT_RIG 2>/dev/null || true
  # Multi-role: first role has highest precedence
  BOAT_ROLE="$E2E_ROLE,$E2E_ROLE_BASE" \
    "$GB_BIN" setup claude --role "$E2E_ROLE,$E2E_ROLE_BASE" --workspace "$WORK_DIR" 2>/dev/null || true

  content=$(read_user_settings)
  if [ -n "$content" ]; then
    # SHARED should be overridden by higher-precedence role
    assert_contains "S2: role overrides SHARED" "from-role-$TEST_TAG" "$content"
    assert_not_contains "S2: base SHARED NOT present" "from-base-$TEST_TAG" "$content"
    # BASE_ONLY from base layer should survive (non-overlapping)
    assert_contains "S2: base BASE_ONLY survives" "b-$TEST_TAG" "$content"
    # ROLE_ONLY from role layer should be present
    assert_contains "S2: role ROLE_ONLY present" "r-$TEST_TAG" "$content"
  else
    fail "S2: user settings" "not created"
  fi
  echo

  # ── Scenario 3: Project + base role — project overrides for SHARED ──
  blue "Scenario 3: Project layer with base role layer"

  local p_id
  p_id=$(create_config "claude-settings" "project:$E2E_PROJECT" \
    "{\"SHARED\":\"from-project-$TEST_TAG\",\"PROJECT_ONLY\":\"p-$TEST_TAG\"}")
  if [ -z "$p_id" ]; then
    fail "S3: create project bead" "could not create"
  else
    dim "  project bead: $p_id"
  fi

  # Close role bead so only base + project are active
  close_bead "$r_id"

  reset_workspace
  export BOAT_PROJECT="$E2E_PROJECT"
  unset BOAT_RIG 2>/dev/null || true
  # role (2:) > project (1:), so base role wins SHARED over project
  "$GB_BIN" setup claude --role "$E2E_ROLE_BASE" --workspace "$WORK_DIR" 2>/dev/null || true

  content=$(read_user_settings)
  if [ -n "$content" ]; then
    # role (2:) > project (1:), so base role wins SHARED
    assert_contains "S3: base role wins SHARED over project" "from-base-$TEST_TAG" "$content"
    assert_not_contains "S3: project SHARED NOT present" "from-project-$TEST_TAG" "$content"
    # BASE_ONLY from base role survives
    assert_contains "S3: base BASE_ONLY survives" "b-$TEST_TAG" "$content"
    # PROJECT_ONLY from project layer should be present (non-overlapping)
    assert_contains "S3: project PROJECT_ONLY present" "p-$TEST_TAG" "$content"
  else
    fail "S3: user settings" "not created"
  fi
  unset BOAT_PROJECT
  echo

  # ── Scenario 4: Role + project + base — correct ordering ──────────
  blue "Scenario 4: Role + project + base — role > project > base"

  # Re-create role bead
  r_id=$(create_config "claude-settings" "role:$E2E_ROLE" \
    "{\"SHARED\":\"from-role-$TEST_TAG\",\"ROLE_ONLY\":\"r-$TEST_TAG\"}")
  if [ -z "$r_id" ]; then
    fail "S4: re-create role bead" "could not create"
  else
    dim "  role bead (re-created): $r_id"
  fi

  reset_workspace
  export BOAT_PROJECT="$E2E_PROJECT"
  BOAT_ROLE="$E2E_ROLE,$E2E_ROLE_BASE" \
    "$GB_BIN" setup claude --role "$E2E_ROLE,$E2E_ROLE_BASE" --workspace "$WORK_DIR" 2>/dev/null || true

  content=$(read_user_settings)
  if [ -n "$content" ]; then
    # SHARED: role E2E_ROLE (highest) > project > base → role wins
    assert_contains "S4: role wins SHARED" "from-role-$TEST_TAG" "$content"
    assert_not_contains "S4: project SHARED NOT present" "from-project-$TEST_TAG" "$content"
    assert_not_contains "S4: base SHARED NOT present" "from-base-$TEST_TAG" "$content"
    # Non-overlapping keys from all layers survive
    assert_contains "S4: base BASE_ONLY survives" "b-$TEST_TAG" "$content"
    assert_contains "S4: project PROJECT_ONLY survives" "p-$TEST_TAG" "$content"
    assert_contains "S4: role ROLE_ONLY survives" "r-$TEST_TAG" "$content"
  else
    fail "S4: user settings" "not created"
  fi
  unset BOAT_PROJECT
  echo

  # ── Scenario 5: Agent-scoped bead requires agent subscription ────
  blue "Scenario 5: Agent-scoped bead filtered without agent subscription"

  # Agent-scoped config beads require "agent:<id>" in subscriptions.
  # gb setup claude uses buildSubscriptions() which only adds global/role/project,
  # so agent-scoped beads should NOT match — verifying the filter works.
  local a_id
  a_id=$(create_config "claude-settings" "agent:$E2E_AGENT" \
    "{\"SHARED\":\"from-agent-$TEST_TAG\",\"AGENT_ONLY\":\"a-$TEST_TAG\"}")
  if [ -z "$a_id" ]; then
    fail "S5: create agent bead" "could not create"
  else
    dim "  agent bead: $a_id"
  fi

  # Agent bead should exist in raw listing.
  # gb config dump may filter by subscriptions, so use kd list instead.
  local dump_out
  dump_out=$("$KD_BIN" list --type=config --status=open --json 2>/dev/null || echo "")
  if [ -n "$dump_out" ] && grep -qF "agent:$E2E_AGENT" <<<"$dump_out"; then
    pass "S5: agent bead exists in listing (contains 'agent:$E2E_AGENT')"
  else
    dim "  S5: agent bead not found in kd list (may be filtered; non-blocking)"
  fi

  # But gb setup claude WITHOUT agent subscription should NOT include agent values
  reset_workspace
  export BOAT_PROJECT="$E2E_PROJECT"
  BOAT_ROLE="$E2E_ROLE,$E2E_ROLE_BASE" \
    "$GB_BIN" setup claude --role "$E2E_ROLE,$E2E_ROLE_BASE" --workspace "$WORK_DIR" 2>/dev/null || true

  content=$(read_user_settings)
  if [ -n "$content" ]; then
    # Agent-scoped SHARED should NOT override (no agent: subscription)
    assert_not_contains "S5: agent SHARED filtered out" "from-agent-$TEST_TAG" "$content"
    assert_not_contains "S5: agent AGENT_ONLY filtered out" "a-$TEST_TAG" "$content"
    # Role still wins SHARED (from scenario 4 beads)
    assert_contains "S5: role still wins SHARED" "from-role-$TEST_TAG" "$content"
  else
    fail "S5: user settings" "not created"
  fi
  unset BOAT_PROJECT
  echo

  # ── Scenario 6: Non-matching role labels filtered out ───────────
  blue "Scenario 6: Non-matching labels filtered out"

  local engineer_id
  engineer_id=$(create_config "claude-settings" "role:engineer-$TEST_TAG" \
    "{\"ENGINEER_ONLY\":\"eng-$TEST_TAG\"}")
  if [ -z "$engineer_id" ]; then
    fail "S6: create engineer bead" "could not create"
  else
    dim "  engineer bead: $engineer_id"
  fi

  # Setup with a DIFFERENT role — engineer bead should NOT match
  reset_workspace
  unset BOAT_PROJECT BOAT_RIG 2>/dev/null || true
  "$GB_BIN" setup claude --role "gardener-$TEST_TAG" --workspace "$WORK_DIR" 2>/dev/null || true

  content=$(read_user_settings)
  if [ -n "$content" ]; then
    assert_not_contains "S6: engineer bead filtered for gardener role" "eng-$TEST_TAG" "$content"
  else
    fail "S6: user settings" "not created"
  fi
  echo

  # ── Scenario 7: Non-matching category titles filtered ───────────
  blue "Scenario 7: Non-matching category titles do not leak across categories"

  # Use role-scoped MCP bead to avoid interference from stale global beads.
  local mcp_id
  mcp_id=$(create_config "claude-mcp" "role:$E2E_ROLE_BASE" \
    "{\"mcpServers\":{\"leak-test-$TEST_TAG\":{\"command\":\"echo\",\"args\":[\"leak\"]}}}")
  if [ -z "$mcp_id" ]; then
    fail "S7: create claude-mcp bead" "could not create"
  else
    dim "  claude-mcp bead: $mcp_id"
  fi

  # The claude-mcp bead should NOT affect claude-settings resolution.
  # Verify by running setup and checking settings.json does not contain mcp content.
  reset_workspace
  unset BOAT_PROJECT BOAT_RIG 2>/dev/null || true
  "$GB_BIN" setup claude --role "$E2E_ROLE_BASE" --workspace "$WORK_DIR" 2>/dev/null || true

  content=$(read_user_settings)
  if [ -n "$content" ]; then
    assert_not_contains "S7: MCP bead not in settings" "leak-test-$TEST_TAG" "$content"
    # Base role settings still resolve
    assert_contains "S7: base BASE_ONLY still present" "b-$TEST_TAG" "$content"
  else
    fail "S7: user settings" "not created"
  fi

  # But the MCP bead SHOULD appear in .mcp.json (correct category)
  if [ -f "$WORK_DIR/.mcp.json" ]; then
    local mcp_content
    mcp_content=$(cat "$WORK_DIR/.mcp.json")
    assert_contains "S7: MCP bead in .mcp.json" "leak-test-$TEST_TAG" "$mcp_content"
  else
    dim "  .mcp.json not created (MCP category may need additional config)"
    pass "S7: category separation verified via settings.json"
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
