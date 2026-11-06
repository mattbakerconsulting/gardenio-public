#!/usr/bin/env bash
# test-config-project-inline.sh — project inline config override E2E tests
#
# Tests that project beads with claude_config field inject inline overrides
# at project-inline specificity (between role and agent levels).
#
# Scenarios tested:
#   1.  Project bead with claude_config overrides global claude-settings
#   2.  Project inline overrides role-scoped config bead for overlapping keys
#   3.  Non-overlapping keys from global/role survive alongside project inline
#   4.  Agent-scoped config bead overrides project inline
#   5.  Project inline MCP servers merged into .mcp.json
#   6.  Project inline hooks concatenated with global (MergeConcat)
#   7.  No claude_config on project bead → normal resolution only
#   8.  Empty claude_config on project bead → no extra layers
#   9.  Project inline + label-scoped project: config bead coexistence
#  10.  Multiple categories in one claude_config field
#  11.  Non-existent BOAT_PROJECT → no project inline layers

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

assert_exit() {
  local name="$1" expected="$2" actual="$3"
  if [ "$actual" -eq "$expected" ]; then
    pass "$name (exit $expected)"
  else
    fail "$name" "expected exit $expected, got exit $actual"
  fi
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

assert_valid_json() {
  local name="$1" content="$2"
  if echo "$content" | python3 -c "import sys,json; json.load(sys.stdin)" 2>/dev/null; then
    pass "$name (valid JSON)"
  else
    fail "$name" "invalid JSON"
  fi
}

assert_json_field() {
  local name="$1" field="$2" expected="$3" json_str="$4"
  local actual
  actual=$(echo "$json_str" | python3 -c "
import sys, json
data = json.load(sys.stdin)
keys = '$field'.split('.')
v = data
for k in keys:
    if isinstance(v, dict):
        v = v.get(k, '')
    else:
        v = ''
        break
print(v)
" 2>/dev/null || echo "")
  if [ "$actual" = "$expected" ]; then
    pass "$name ($field == '$expected')"
  else
    fail "$name" "expected $field='$expected', got '$actual'"
  fi
}

assert_json_has_key() {
  local name="$1" field="$2" json_str="$3"
  local found
  found=$(echo "$json_str" | python3 -c "
import sys, json
data = json.load(sys.stdin)
keys = '$field'.split('.')
v = data
for k in keys:
    if isinstance(v, dict) and k in v:
        v = v[k]
    else:
        print('no')
        sys.exit()
print('yes')
" 2>/dev/null || echo "no")
  if [ "$found" = "yes" ]; then
    pass "$name (has key '$field')"
  else
    fail "$name" "expected key '$field' to exist"
  fi
}

# ── Setup ─────────────────────────────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19114
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-project-inline.log 2>&1 &
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
  # Restore env
  unset BOAT_PROJECT BOAT_ROLE BOAT_AGENT 2>/dev/null || true
}
trap cleanup EXIT

# shellcheck source=./lib-config-cleanup.sh
source "$(dirname "$0")/lib-config-cleanup.sh"

WORK_DIR=$(mktemp -d /tmp/e2e-project-inline-XXXXXX)

create_config() {
  local title="$1" label="$2" value="$3"
  local out id
  # Use --project "" to prevent auto-adding project:$BOAT_PROJECT label,
  # which would scope the config bead to a specific project and make it
  # invisible when BOAT_PROJECT changes in later scenarios.
  out=$("$KD_BIN" create "$title" --type=config -l "$label" \
    --project "" -d "$value" --json 2>/dev/null || echo "{}")
  id=$(echo "$out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  [ -n "$id" ] && CREATED_BEADS+=("$id")
  echo "$id"
}

create_project() {
  local title="$1" claude_config="${2:-}"
  local out id
  if [ -n "$claude_config" ]; then
    out=$("$KD_BIN" create "$title" --type=project \
      --project "" -f "claude_config=$claude_config" --json 2>/dev/null || echo "{}")
  else
    out=$("$KD_BIN" create "$title" --type=project --project "" --json 2>/dev/null || echo "{}")
  fi
  id=$(echo "$out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  [ -n "$id" ] && CREATED_BEADS+=("$id")
  echo "$id"
}

seed_project_type() {
  # type:project is builtin-authoritative and already declares claude_config: the
  # daemon builtin (config.go) + controller EnsureConfigs seed carry it plus a
  # SUPERSET of the fields this PUT listed, so `kd create --type=project -f
  # claude_config=...` validates against the builtin. The old hand-maintained PUT
  # could only shadow-and-drift the builtin (ka-0000003677 / generalized
  # ka-0000000407). Dropped so e2e exercises the builtin prod runs, guarded by
  # TestE2ESeedTypes_NoBuiltinShadow. Do NOT re-add the PUT.
  dim "  project type schema: using daemon builtin (no shadow PUT)"
}

close_and_remove() {
  local id="$1"
  "$KD_BIN" close "$id" 2>/dev/null || true
  local new=()
  for b in "${CREATED_BEADS[@]}"; do
    [ "$b" != "$id" ] && new+=("$b")
  done
  CREATED_BEADS=("${new[@]+"${new[@]}"}")
}

run_setup() {
  rm -rf "$WORK_DIR/.claude" "$WORK_DIR/.mcp.json"
  mkdir -p "$WORK_DIR/.claude"
  "$GB_BIN" setup claude --workspace "$WORK_DIR" 2>/dev/null || true
}

read_user_settings() {
  local home_dir
  home_dir=$(eval echo "~")
  local path="$home_dir/.claude/settings.json"
  if [ -f "$path" ]; then
    cat "$path"
  else
    echo ""
  fi
}

read_ws_settings() {
  if [ -f "$WORK_DIR/.claude/settings.local.json" ]; then
    cat "$WORK_DIR/.claude/settings.local.json"
  else
    echo ""
  fi
}

read_mcp() {
  if [ -f "$WORK_DIR/.mcp.json" ]; then
    cat "$WORK_DIR/.mcp.json"
  else
    echo ""
  fi
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " project inline config override E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token
  seed_project_type

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  cleanup_stale_claude_configs

  local TEST_TAG="e2e-pi-$$-$(date +%s)"
  local PROJ_NAME="e2e-inline-$TEST_TAG"

  # Need hooks for setup to succeed
  local hooks_id
  hooks_id=$(create_config "claude-hooks" "global" \
    '{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"echo pi-test"}]}],"Stop":[{"matcher":"","hooks":[{"type":"command","command":"echo stop"}]}]}}')

  # ── Scenario 1: Project bead claude_config overrides global ────
  blue "Scenario 1: Project bead with claude_config overrides global claude-settings"

  # Create global settings bead
  local global_id
  global_id=$(create_config "claude-settings" "global" \
    '{"model":"sonnet","env":{"GLOBAL_MARKER":"from-global"}}')

  # Create project bead with inline claude_config that overrides model
  local proj_bead_id
  proj_bead_id=$(create_project "$PROJ_NAME" \
    '{"claude-settings":{"model":"opus-from-project"}}')
  [ -n "$proj_bead_id" ] && pass "project bead created ($proj_bead_id)"

  # Verify the field was set
  local proj_show
  proj_show=$("$KD_BIN" show "$proj_bead_id" --json 2>/dev/null || echo "{}")
  assert_contains "claude_config field stored" "claude-settings" "$proj_show"

  export BOAT_PROJECT="$PROJ_NAME"
  unset BOAT_ROLE BOAT_AGENT 2>/dev/null || true

  run_setup

  local user_settings
  user_settings=$(read_user_settings)
  if [ -n "$user_settings" ]; then
    assert_valid_json "user settings is valid JSON" "$user_settings"
    # Project inline specificity (2~:) > global (0:), so model should be overridden
    assert_contains "model overridden by project inline" "opus-from-project" "$user_settings"
  else
    fail "user settings" "not written"
  fi
  echo

  # ── Scenario 2: Project inline overrides role-scoped config ────
  blue "Scenario 2: Project inline overrides role-scoped config for overlapping keys"

  # Create role-scoped settings with a model override.
  # Use a TEST_TAG-suffixed role to avoid cross-shard pollution: sibling shards
  # running other config-* tests also create role:crew claude-settings beads
  # with different content (no model field), and the 120s cleanup age filter
  # cannot evict in-flight cross-shard beads. A newer sibling-shard bead at
  # role:crew specificity would override this test's role bead via "newer
  # wins" sort, stripping `model: haiku-from-role` from scenarios 7/8/11.
  # See gd-2GrDgSa3ke for the regression that motivated this fix.
  local role_label="role:crew-pi-$TEST_TAG"
  local role_id
  role_id=$(create_config "claude-settings" "$role_label" \
    '{"model":"haiku-from-role","env":{"ROLE_MARKER":"from-role"}}')

  export BOAT_PROJECT="$PROJ_NAME"
  export BOAT_ROLE="crew-pi-$TEST_TAG"

  run_setup

  user_settings=$(read_user_settings)
  if [ -n "$user_settings" ]; then
    # Project inline specificity (2~:) > role (2:), so project wins for model
    assert_contains "model from project inline (not role)" "opus-from-project" "$user_settings"
    assert_not_contains "role model not present" "haiku-from-role" "$user_settings"
  else
    fail "user settings" "not written"
  fi
  echo

  # ── Scenario 3: Non-overlapping keys survive from all layers ───
  blue "Scenario 3: Non-overlapping keys from global/role survive alongside project inline"

  user_settings=$(read_user_settings)
  if [ -n "$user_settings" ]; then
    # GLOBAL_MARKER from global should survive (not overridden)
    assert_contains "global env survives" "GLOBAL_MARKER" "$user_settings"
    # ROLE_MARKER from role should survive
    assert_contains "role env survives" "ROLE_MARKER" "$user_settings"
  else
    fail "user settings" "not written"
  fi
  echo

  # ── Scenario 4: Agent-scoped config overrides project inline ───
  blue "Scenario 4: Agent-scoped config bead overrides project inline"

  # Create agent-scoped settings
  local agent_bead_name="e2e-agent-$TEST_TAG"
  local agent_id
  agent_id=$(create_config "claude-settings" "agent:$agent_bead_name" \
    '{"model":"agent-model-override"}')

  export BOAT_PROJECT="$PROJ_NAME"
  export BOAT_ROLE="crew-pi-$TEST_TAG"
  export BOAT_AGENT="$agent_bead_name"

  run_setup

  user_settings=$(read_user_settings)
  if [ -n "$user_settings" ]; then
    # Agent specificity (3:) > project inline (2~:), so agent wins
    assert_contains "agent model overrides project inline" "agent-model-override" "$user_settings"
    assert_not_contains "project model not present" "opus-from-project" "$user_settings"
  else
    fail "user settings" "not written"
  fi

  close_and_remove "$agent_id"
  unset BOAT_AGENT 2>/dev/null || true
  echo

  # ── Scenario 5: Project inline MCP servers in .mcp.json ────────
  blue "Scenario 5: Project inline MCP servers merged into .mcp.json"

  # Create global MCP bead
  local global_mcp_id
  global_mcp_id=$(create_config "claude-mcp" "global" \
    '{"mcpServers":{"global-server":{"command":"echo","args":["global"]}}}')

  # Update the project bead to include claude-mcp in claude_config
  "$KD_BIN" update "$proj_bead_id" \
    -f "claude_config=$(cat <<'INLINE'
{"claude-settings":{"model":"opus-from-project"},"claude-mcp":{"mcpServers":{"project-mcp-server":{"command":"proj-cmd","args":["--project"]}}}}
INLINE
)" 2>/dev/null || true

  export BOAT_PROJECT="$PROJ_NAME"
  export BOAT_ROLE="crew-pi-$TEST_TAG"

  run_setup

  local mcp_content
  mcp_content=$(read_mcp)
  if [ -n "$mcp_content" ]; then
    assert_valid_json "MCP config is valid JSON" "$mcp_content"
    # Project inline MCP (specificity 2~:) overrides global (0:)
    # MergeOverride means project's mcpServers replaces global's
    assert_json_has_key "project MCP server present" "mcpServers.project-mcp-server" "$mcp_content"
    assert_json_field "project MCP command" "mcpServers.project-mcp-server.command" "proj-cmd" "$mcp_content"
  else
    fail "MCP config" "not written"
  fi
  echo

  # ── Scenario 6: Project inline hooks concatenated (MergeConcat) ─
  blue "Scenario 6: Project inline hooks concatenated with global (MergeConcat)"

  # Update project bead claude_config to include hooks
  "$KD_BIN" update "$proj_bead_id" \
    -f "claude_config=$(cat <<'INLINE'
{"claude-settings":{"model":"opus-from-project"},"claude-hooks":{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"echo project-inline-hook"}]}]}}}
INLINE
)" 2>/dev/null || true

  export BOAT_PROJECT="$PROJ_NAME"
  export BOAT_ROLE="crew-pi-$TEST_TAG"

  run_setup

  local ws_settings
  ws_settings=$(read_ws_settings)
  if [ -n "$ws_settings" ]; then
    assert_valid_json "workspace settings is valid JSON" "$ws_settings"
    # MergeConcat: both global and project inline hooks present
    assert_contains "global hook present" "pi-test" "$ws_settings"
    assert_contains "project inline hook concatenated" "project-inline-hook" "$ws_settings"
  else
    fail "workspace settings" "not written"
  fi
  echo

  # ── Scenario 7: No claude_config → normal resolution only ──────
  blue "Scenario 7: No claude_config on project bead → normal resolution"

  # Create a project bead WITHOUT claude_config
  local bare_proj_name="e2e-bare-$TEST_TAG"
  local bare_proj_id
  bare_proj_id=$(create_project "$bare_proj_name")

  export BOAT_PROJECT="$bare_proj_name"
  export BOAT_ROLE="crew-pi-$TEST_TAG"

  run_setup

  user_settings=$(read_user_settings)
  if [ -n "$user_settings" ]; then
    assert_valid_json "settings valid without project inline" "$user_settings"
    # Should fall back to role config since no project inline
    assert_contains "role model used as fallback" "haiku-from-role" "$user_settings"
    assert_not_contains "no project model" "opus-from-project" "$user_settings"
    pass "normal resolution without project inline works"
  else
    fail "user settings" "not written"
  fi
  close_and_remove "$bare_proj_id"
  echo

  # ── Scenario 8: Empty claude_config → no extra layers ──────────
  blue "Scenario 8: Empty claude_config on project bead → no extra layers"

  local empty_proj_name="e2e-empty-$TEST_TAG"
  local empty_proj_id
  empty_proj_id=$(create_project "$empty_proj_name" '{}')

  export BOAT_PROJECT="$empty_proj_name"
  export BOAT_ROLE="crew-pi-$TEST_TAG"

  run_setup

  user_settings=$(read_user_settings)
  if [ -n "$user_settings" ]; then
    # Empty claude_config = no project-inline layers, role wins
    assert_contains "role model with empty project config" "haiku-from-role" "$user_settings"
    pass "empty claude_config produces no extra layers"
  else
    fail "user settings" "not written"
  fi
  close_and_remove "$empty_proj_id"
  echo

  # ── Scenario 9: Project inline + label-scoped coexistence ──────
  blue "Scenario 9: Project inline + label-scoped project: config bead coexistence"

  # Create a label-scoped project:PROJ_NAME config bead
  local label_proj_id
  label_proj_id=$(create_config "claude-settings" "project:$PROJ_NAME" \
    '{"env":{"LABEL_PROJ_MARKER":"from-label-scope"}}')

  export BOAT_PROJECT="$PROJ_NAME"
  export BOAT_ROLE="crew-pi-$TEST_TAG"

  run_setup

  user_settings=$(read_user_settings)
  if [ -n "$user_settings" ]; then
    # Label-scoped project config (specificity 1:) and project-inline (2~:)
    # should both contribute. Project-inline has higher specificity.
    assert_contains "label-scoped project env present" "LABEL_PROJ_MARKER" "$user_settings"
    # Project inline model should still win over global/role
    assert_contains "project inline model still wins" "opus-from-project" "$user_settings"
  else
    fail "user settings" "not written"
  fi
  close_and_remove "$label_proj_id"
  echo

  # ── Scenario 10: Multiple categories in one claude_config ──────
  blue "Scenario 10: Multiple categories in single claude_config field"

  # Update project to have settings + mcp in one field
  "$KD_BIN" update "$proj_bead_id" \
    -f "claude_config=$(cat <<'INLINE'
{"claude-settings":{"model":"multi-cat-model","env":{"MULTI_CAT":"yes"}},"claude-mcp":{"mcpServers":{"multi-cat-server":{"command":"multi-cmd"}}}}
INLINE
)" 2>/dev/null || true

  export BOAT_PROJECT="$PROJ_NAME"
  export BOAT_ROLE="crew-pi-$TEST_TAG"

  run_setup

  user_settings=$(read_user_settings)
  mcp_content=$(read_mcp)
  if [ -n "$user_settings" ]; then
    assert_contains "multi-category: settings model applied" "multi-cat-model" "$user_settings"
    assert_contains "multi-category: settings env applied" "MULTI_CAT" "$user_settings"
  else
    fail "multi-category user settings" "not written"
  fi
  if [ -n "$mcp_content" ]; then
    assert_json_has_key "multi-category: MCP server from same field" "mcpServers.multi-cat-server" "$mcp_content"
    assert_json_field "multi-category: MCP command" "mcpServers.multi-cat-server.command" "multi-cmd" "$mcp_content"
  else
    fail "multi-category MCP" "not written"
  fi
  echo

  # ── Scenario 11: Non-existent BOAT_PROJECT → no inline ─────────
  blue "Scenario 11: Non-existent BOAT_PROJECT → no project inline layers"

  export BOAT_PROJECT="does-not-exist-$TEST_TAG"
  export BOAT_ROLE="crew-pi-$TEST_TAG"

  run_setup

  user_settings=$(read_user_settings)
  if [ -n "$user_settings" ]; then
    # No project bead found, so role config wins
    assert_contains "role model fallback" "haiku-from-role" "$user_settings"
    assert_not_contains "no project model" "multi-cat-model" "$user_settings"
    pass "non-existent project → no inline layers"
  else
    fail "user settings" "not written"
  fi
  unset BOAT_PROJECT BOAT_ROLE 2>/dev/null || true
  echo

  # ── Summary ─────────────────────────────────────────────────────
  blue "═══════════════════════════════════════════════════"
  local total=$((PASS + FAIL))
  if [ "$FAIL" -eq 0 ]; then
    green " ✓ All $total assertions passed (11 scenarios)"
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
