#!/usr/bin/env bash
# test-config-merge-override.sh — MergeOverride strategy E2E tests
#
# Tests that MergeOverride (shallow key override) correctly resolves
# config bead layers: later/more-specific layers replace earlier values
# for overlapping keys while preserving non-overlapping keys.
#
# Scenarios tested:
#   1.  Single global layer — user settings contain global values
#   2.  Role overrides global for overlapping key
#   3.  Non-overlapping keys from both layers survive
#   4.  Project layer between global and role (specificity ordering)
#   5.  Three-layer merge — role wins, project and global unique keys survive
#   6.  Agent-scoped bead does not affect setup resolution
#   7.  Project overrides global when no role matches
#   8.  Empty string at higher specificity replaces non-empty lower
#   9.  Nested map sub-keys merged one level deep
#   10. Non-matching role label excluded from merge
#   11. Cross-category isolation — claude-settings merge vs claude-mcp
#   12. Closed bead excluded from merge

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
  green "  PASS: $name"
  PASS=$((PASS + 1))
}

fail() {
  local name="$1"
  local reason="${2:-}"
  red "  FAIL: $name"
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
    pass "$name"
  else
    fail "$name" "expected to contain '$pattern'; got: $(echo "$actual" | head -5)"
  fi
}

assert_not_contains() {
  local name="$1" pattern="$2" actual="$3"
  if ! grep -qF -- "$pattern" <<< "$actual"; then
    pass "$name"
  else
    fail "$name" "expected NOT to contain '$pattern'"
  fi
}

# json_get <file> <dotted.key.path> — extract a value from JSON file.
json_get() {
  local file="$1" key_path="$2"
  python3 -c "
import json
d = json.load(open('$file'))
keys = '$key_path'.split('.')
v = d
for k in keys:
    if isinstance(v, dict) and k in v:
        v = v[k]
    else:
        v = None
        break
if v is None:
    print('')
elif isinstance(v, (dict, list)):
    print(json.dumps(v, sort_keys=True))
else:
    print(v)
" 2>/dev/null || echo ""
}

# json_has_key <file> <dotted.key.path> — "true" if key exists, "false" otherwise.
json_has_key() {
  local file="$1" key_path="$2"
  python3 -c "
import json
d = json.load(open('$file'))
keys = '$key_path'.split('.')
v = d
for k in keys:
    if isinstance(v, dict) and k in v:
        v = v[k]
    else:
        print('false')
        exit()
print('true')
" 2>/dev/null || echo "false"
}

assert_json_value() {
  local name="$1" file="$2" key="$3" expected="$4"
  local actual
  actual=$(json_get "$file" "$key")
  if [ "$actual" = "$expected" ]; then
    pass "$name ($key=$expected)"
  else
    fail "$name" "expected $key='$expected', got '$actual'"
  fi
}

assert_json_key_exists() {
  local name="$1" file="$2" key="$3"
  local exists
  exists=$(json_has_key "$file" "$key")
  if [ "$exists" = "true" ]; then
    pass "$name ($key exists)"
  else
    fail "$name" "key '$key' not found in $file"
  fi
}

assert_json_key_absent() {
  local name="$1" file="$2" key="$3"
  local exists
  exists=$(json_has_key "$file" "$key")
  if [ "$exists" = "false" ]; then
    pass "$name ($key absent)"
  else
    fail "$name" "key '$key' unexpectedly present in $file"
  fi
}

# ── Setup ─────────────────────────────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19102
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-merge-override.log 2>&1 &
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
    [ -n "$id" ] && "$KD_BIN" close "$id" 2>/dev/null || true
  done
  [ -d "${WORK_DIR:-}" ] && rm -rf "$WORK_DIR"
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

WORK_DIR=$(mktemp -d /tmp/e2e-merge-override-XXXXXX)
TEST_TAG="e2e-merge-override-$$-$(date +%s)"
CLEANUP_STALE_ROLE="${TEST_TAG}"
export CLEANUP_STALE_ROLE

# shellcheck source=./lib-config-cleanup.sh
source "$(dirname "$0")/lib-config-cleanup.sh"

# create_config creates a config bead with ONLY the explicitly given label.
# BOAT_PROJECT is cleared to prevent auto-adding the pod's project label,
# which would pollute label matching in the resolution tests.
create_config() {
  local title="$1" label="$2" value="$3"
  local out id
  out=$(BOAT_PROJECT="" "$KD_BIN" create "$title" --type=config -l "$label" \
    -d "$value" --json 2>/dev/null || echo "{}")
  id=$(echo "$out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$id" ]; then
    CREATED_BEADS+=("$id")
    echo "$id"
  else
    echo ""
  fi
}

close_bead() {
  local id="$1"
  [ -n "$id" ] && "$KD_BIN" close "$id" 2>/dev/null || true
}

# fresh_workspace resets the work dir and creates an isolated HOME.
# Prints the isolated HOME path on stdout.
fresh_workspace() {
  rm -rf "$WORK_DIR/.claude" "$WORK_DIR/CLAUDE.md" "$WORK_DIR/.mcp.json"
  mkdir -p "$WORK_DIR"
  local iso_home
  iso_home=$(mktemp -d /tmp/e2e-mo-home-XXXXXX)
  mkdir -p "$iso_home/.claude"
  echo "$iso_home"
}

# run_setup <iso_home> [KEY=VAL...] [--flag...] — runs gb setup claude with
# isolated HOME. Extra env vars (containing '=') are consumed first; remaining
# arguments are forwarded to gb setup claude as flags.
run_setup() {
  local iso_home="$1"
  shift
  local env_args=("HOME=$iso_home")
  while [[ $# -gt 0 && "$1" == *=* ]]; do
    env_args+=("$1")
    shift
  done
  local rc=0
  env "${env_args[@]}" "$GB_BIN" setup claude \
    --workspace "$WORK_DIR" "$@" 2>/dev/null || rc=$?
  return $rc
}

# user_settings <iso_home> — path to the user-level settings.json.
user_settings() {
  echo "$1/.claude/settings.json"
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " MergeOverride strategy E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  # Clean up stale config beads from previous failed runs.
  cleanup_stale_claude_configs

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  local E2E_ROLE="e2e-mo-role-$TEST_TAG"
  local E2E_PROJECT="e2e-mo-proj-$TEST_TAG"
  local E2E_AGENT="e2e-mo-agent-$TEST_TAG"

  # Create a claude-hooks bead so normal-mode setup succeeds (it errors without one).
  local hooks_id
  hooks_id=$(create_config "claude-hooks" "global" \
    '{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"gb hook prime 2>/dev/null || true"}]}],"Stop":[{"matcher":"","hooks":[{"type":"command","command":"gb hook stop-gate"}]}]}}')
  if [ -z "$hooks_id" ]; then
    red "ERROR: Failed to create prerequisite claude-hooks bead"
    exit 1
  fi
  dim "  Hooks bead: $hooks_id"
  echo

  # ── Scenario 1: Single global layer ────────────────────────────
  blue "Scenario 1: Single global layer — user settings contain global values"
  local g1_id iso1
  g1_id=$(create_config "claude-settings" "global" \
    '{"test_model":"sonnet","test_base":"global-value","test_shared":"from-global"}')

  if [ -z "$g1_id" ]; then
    fail "create global claude-settings" "failed to create"
  else
    pass "global claude-settings created ($g1_id)"
    iso1=$(fresh_workspace)

    local rc1=0
    run_setup "$iso1" --role "nonexistent-$TEST_TAG" || rc1=$?
    assert_exit "setup with global-only" 0 "$rc1"

    local us1
    us1=$(user_settings "$iso1")
    if [ -f "$us1" ]; then
      assert_json_value "global test_model" "$us1" "test_model" "sonnet"
      assert_json_value "global test_base" "$us1" "test_base" "global-value"
      assert_json_value "global test_shared" "$us1" "test_shared" "from-global"
    else
      fail "user settings from global-only" "not found at $us1"
    fi
  fi
  rm -rf "${iso1:-}"
  echo

  # ── Scenario 2: Role overrides global for overlapping key ──────
  blue "Scenario 2: Role overrides global for overlapping key"
  local r2_id iso2
  r2_id=$(create_config "claude-settings" "role:$E2E_ROLE" \
    '{"test_model":"opus","test_role":"role-value","test_shared":"from-role"}')

  if [ -z "$r2_id" ]; then
    fail "create role claude-settings" "failed to create"
  else
    pass "role claude-settings created ($r2_id)"
    iso2=$(fresh_workspace)

    local rc2=0
    run_setup "$iso2" --role "$E2E_ROLE" || rc2=$?
    assert_exit "setup with global+role" 0 "$rc2"

    local us2
    us2=$(user_settings "$iso2")
    if [ -f "$us2" ]; then
      # Role overrides global for test_model
      assert_json_value "role overrides test_model" "$us2" "test_model" "opus"
      # Role overrides global for test_shared
      assert_json_value "role overrides test_shared" "$us2" "test_shared" "from-role"
    else
      fail "user settings from global+role" "not found at $us2"
    fi
  fi
  rm -rf "${iso2:-}"
  echo

  # ── Scenario 3: Non-overlapping keys survive ───────────────────
  blue "Scenario 3: Non-overlapping keys from both layers survive"
  local iso3
  iso3=$(fresh_workspace)

  local rc3=0
  run_setup "$iso3" --role "$E2E_ROLE" || rc3=$?

  local us3
  us3=$(user_settings "$iso3")
  if [ -f "$us3" ]; then
    # Global-only key survives
    assert_json_value "global test_base survives" "$us3" "test_base" "global-value"
    # Role-only key survives
    assert_json_value "role test_role survives" "$us3" "test_role" "role-value"
    # Both layers present simultaneously
    assert_json_key_exists "test_model present" "$us3" "test_model"
    assert_json_key_exists "test_base present" "$us3" "test_base"
    assert_json_key_exists "test_role present" "$us3" "test_role"
    assert_json_key_exists "test_shared present" "$us3" "test_shared"
  else
    fail "user settings for non-overlapping keys" "not found at $us3"
  fi
  rm -rf "${iso3:-}"
  echo

  # ── Scenario 4: Project layer between global and role ──────────
  blue "Scenario 4: Project-scoped bead between global and role"
  local p4_id iso4 stderr4
  p4_id=$(create_config "claude-settings" "project:$E2E_PROJECT" \
    '{"test_model":"haiku","test_proj":"project-value","test_shared":"from-project"}')

  if [ -z "$p4_id" ]; then
    fail "create project claude-settings" "failed to create"
  else
    pass "project claude-settings created ($p4_id)"
    iso4=$(fresh_workspace)
    stderr4=$(mktemp /tmp/e2e-mo-s4-stderr-XXXXXX)

    # With all three layers: global(sonnet) < project(haiku) < role(opus).
    # Capture stderr to diagnose intermittent exit-1 failures (ka-0000003923);
    # mirrors the diagnostic added for scenario 7 in MR !1162.
    local rc4=0
    env HOME="$iso4" BOAT_PROJECT="$E2E_PROJECT" "$GB_BIN" setup claude \
      --workspace "$WORK_DIR" --role "$E2E_ROLE" 2>"$stderr4" || rc4=$?
    if [ "$rc4" -ne 0 ]; then
      dim "  scenario 4 stderr (gb setup claude):"
      sed 's/^/    | /' "$stderr4" | head -20
    fi
    assert_exit "setup with three layers" 0 "$rc4"
    rm -f "$stderr4"

    local us4
    us4=$(user_settings "$iso4")
    if [ -f "$us4" ]; then
      # Role wins over project for test_model (role > project)
      assert_json_value "role wins over project for test_model" "$us4" "test_model" "opus"
      # Role wins over project for test_shared (role > project > global)
      assert_json_value "role wins for test_shared" "$us4" "test_shared" "from-role"
      # Project-only key survives
      assert_json_value "project test_proj survives" "$us4" "test_proj" "project-value"
    else
      fail "user settings for project layer" "not found at $us4"
    fi
  fi
  rm -rf "${iso4:-}"
  echo

  # ── Scenario 5: Three-layer merge — all keys verified ──────────
  blue "Scenario 5: Three-layer merge — role wins, unique keys survive"
  local iso5
  iso5=$(fresh_workspace)

  local rc5=0
  run_setup "$iso5" BOAT_PROJECT="$E2E_PROJECT" --role "$E2E_ROLE" || rc5=$?

  local us5
  us5=$(user_settings "$iso5")
  if [ -f "$us5" ]; then
    # Overlapping key resolved by specificity: global < project < role
    assert_json_value "three-layer test_model = opus (role wins)" "$us5" "test_model" "opus"
    # Non-overlapping keys from each layer survive
    assert_json_value "three-layer test_base from global" "$us5" "test_base" "global-value"
    assert_json_value "three-layer test_proj from project" "$us5" "test_proj" "project-value"
    assert_json_value "three-layer test_role from role" "$us5" "test_role" "role-value"
    # Shared key won by role
    assert_json_value "three-layer test_shared from role" "$us5" "test_shared" "from-role"
  else
    fail "user settings for three-layer merge" "not found at $us5"
  fi
  rm -rf "${iso5:-}"
  echo

  # ── Scenario 6: Agent-scoped bead not resolved by setup ─────────
  # NOTE: gb setup claude builds subscriptions for global, project, and role
  # only — agent:<name> subscriptions are not included (see buildSubscriptions
  # in setup_config.go). Agent-scoped beads are used by gb prime, not setup.
  # This scenario verifies that agent-scoped beads do NOT affect setup output.
  blue "Scenario 6: Agent-scoped bead does not affect setup resolution"
  local a6_id iso6
  a6_id=$(create_config "claude-settings" "agent:$E2E_AGENT" \
    '{"test_model":"agent-model","test_agent":"agent-value","test_shared":"from-agent"}')

  if [ -z "$a6_id" ]; then
    fail "create agent claude-settings" "failed to create"
  else
    pass "agent-scoped claude-settings created ($a6_id)"
    iso6=$(fresh_workspace)

    local rc6=0
    run_setup "$iso6" BOAT_PROJECT="$E2E_PROJECT" \
      --role "$E2E_ROLE" || rc6=$?
    assert_exit "setup ignores agent scope" 0 "$rc6"

    local us6
    us6=$(user_settings "$iso6")
    if [ -f "$us6" ]; then
      # Role wins (agent scope not in subscriptions, so agent bead is ignored)
      assert_json_value "role wins (agent not resolved)" "$us6" "test_model" "opus"
      assert_json_value "role wins for shared (agent not resolved)" "$us6" "test_shared" "from-role"
      # Agent-only key should NOT appear
      assert_json_key_absent "agent key not in output" "$us6" "test_agent"
    else
      fail "user settings for agent scope test" "not found at $us6"
    fi
  fi
  rm -rf "${iso6:-}"
  close_bead "$a6_id"
  echo

  # ── Scenario 7: Project-only merge (no role) ───────────────────
  blue "Scenario 7: Project overrides global when no role matches"
  local iso7 stderr7
  iso7=$(fresh_workspace)
  stderr7=$(mktemp /tmp/e2e-mo-s7-stderr-XXXXXX)

  local rc7=0
  # Use a nonexistent role so only global + project participate.
  # Capture stderr to diagnose intermittent exit-1 failures (ka-0000001102).
  env HOME="$iso7" BOAT_PROJECT="$E2E_PROJECT" "$GB_BIN" setup claude \
    --workspace "$WORK_DIR" --role "nonexistent-$TEST_TAG" 2>"$stderr7" || rc7=$?
  if [ "$rc7" -ne 0 ]; then
    dim "  scenario 7 stderr (gb setup claude):"
    sed 's/^/    | /' "$stderr7" | head -20
  fi
  assert_exit "setup with global+project only" 0 "$rc7"
  rm -f "$stderr7"

  local us7
  us7=$(user_settings "$iso7")
  if [ -f "$us7" ]; then
    # Project overrides global for test_model
    assert_json_value "project overrides global test_model" "$us7" "test_model" "haiku"
    # Project overrides global for test_shared
    assert_json_value "project overrides global test_shared" "$us7" "test_shared" "from-project"
    # Global-only key survives
    assert_json_value "global test_base survives with project" "$us7" "test_base" "global-value"
    # Project-only key present
    assert_json_value "project test_proj present" "$us7" "test_proj" "project-value"
    # Role-only key should NOT appear (no matching role bead)
    assert_json_key_absent "role key absent (no matching role)" "$us7" "test_role"
  else
    fail "user settings for project-only merge" "not found at $us7"
  fi
  rm -rf "${iso7:-}"
  echo

  # ── Scenario 8: Empty string at higher specificity ─────────────
  blue "Scenario 8: Empty string at higher specificity replaces non-empty lower"
  # Close the original role bead to avoid two role-level beads competing
  close_bead "$r2_id"
  local r8_id iso8
  # Role bead with empty test_base overrides global's non-empty test_base
  r8_id=$(create_config "claude-settings" "role:$E2E_ROLE" \
    '{"test_base":"","test_model":"opus-v2"}')

  if [ -z "$r8_id" ]; then
    fail "create role with empty override" "failed to create"
  else
    pass "role with empty override created ($r8_id)"
    iso8=$(fresh_workspace)

    local rc8=0
    run_setup "$iso8" --role "$E2E_ROLE" || rc8=$?
    assert_exit "setup with empty override" 0 "$rc8"

    local us8
    us8=$(user_settings "$iso8")
    if [ -f "$us8" ]; then
      # Empty string from role replaces non-empty "global-value" from global
      assert_json_value "empty string override" "$us8" "test_base" ""
      # Role's test_model override works
      assert_json_value "concurrent override" "$us8" "test_model" "opus-v2"
    else
      fail "user settings for empty override" "not found at $us8"
    fi
  fi
  rm -rf "${iso8:-}"
  # Clean up the extra role bead; keep the original r2_id
  close_bead "$r8_id"
  echo

  # ── Scenario 9: Nested objects replaced wholesale ──────────────
  blue "Scenario 9: Nested map sub-keys merged one level deep"
  local gn_id rn_id iso9
  # Close existing global + role + project claude-settings to avoid interference
  close_bead "$g1_id"
  close_bead "$r2_id"
  close_bead "$p4_id"

  gn_id=$(create_config "claude-settings" "global" \
    '{"test_nested":{"alpha":"a","beta":"b"},"test_flat":"global-flat"}')
  rn_id=$(create_config "claude-settings" "role:$E2E_ROLE" \
    '{"test_nested":{"alpha":"A-from-role"}}')

  if [ -z "$gn_id" ] || [ -z "$rn_id" ]; then
    fail "create nested merge beads" "failed to create"
  else
    pass "nested merge beads created (global $gn_id, role $rn_id)"
    iso9=$(fresh_workspace)

    local rc9=0
    run_setup "$iso9" --role "$E2E_ROLE" || rc9=$?
    assert_exit "setup with nested objects" 0 "$rc9"

    local us9
    us9=$(user_settings "$iso9")
    if [ -f "$us9" ]; then
      # MergeOverride is ONE-LEVEL DEEP: sub-keys of top-level maps are merged
      # individually, not replaced wholesale. So role's test_nested.alpha
      # overrides global's, but global's test_nested.beta survives.
      assert_json_value "nested alpha from role" "$us9" "test_nested.alpha" "A-from-role"
      assert_json_value "nested beta preserved (one-level merge)" "$us9" "test_nested.beta" "b"
      # Non-overlapping top-level key survives
      assert_json_value "flat key survives" "$us9" "test_flat" "global-flat"
    else
      fail "user settings for nested merge" "not found at $us9"
    fi
  fi
  rm -rf "${iso9:-}"
  close_bead "$gn_id"
  close_bead "$rn_id"
  echo

  # ── Scenario 10: Non-matching role label excluded ──────────────
  blue "Scenario 10: Non-matching role label excluded from merge"
  local g10_id r10_wrong_id iso10
  # Use scenario-unique keys (s10_*) to isolate from any lingering
  # test_model:* values from earlier scenarios in this same test run —
  # closed beads can briefly remain visible in ResolveConfigBeads on a
  # busy shared kbeads instance.
  g10_id=$(create_config "claude-settings" "global" \
    '{"s10_model":"base-model","s10_base":"yes"}')
  r10_wrong_id=$(create_config "claude-settings" "role:wrong-role-$TEST_TAG" \
    '{"s10_model":"wrong-model","s10_wrong":"should-not-appear"}')

  if [ -z "$g10_id" ] || [ -z "$r10_wrong_id" ]; then
    fail "create non-matching role beads" "failed to create"
  else
    pass "non-matching role beads created"
    iso10=$(fresh_workspace)

    # Setup with a different role — wrong-role bead should be excluded
    local rc10=0
    run_setup "$iso10" --role "correct-role-$TEST_TAG" || rc10=$?
    assert_exit "setup with non-matching role" 0 "$rc10"

    local us10
    us10=$(user_settings "$iso10")
    if [ -f "$us10" ]; then
      # Only global values should appear (wrong-role bead excluded)
      assert_json_value "base model from global" "$us10" "s10_model" "base-model"
      assert_json_key_absent "wrong-role key excluded" "$us10" "s10_wrong"
    else
      fail "user settings for non-matching role" "not found at $us10"
    fi
  fi
  rm -rf "${iso10:-}"
  close_bead "$g10_id"
  close_bead "$r10_wrong_id"
  echo

  # ── Scenario 11: Cross-category isolation ──────────────────────
  blue "Scenario 11: Cross-category isolation — claude-settings vs claude-mcp"
  local g11_s_id g11_m_id iso11
  g11_s_id=$(create_config "claude-settings" "global" \
    '{"test_model":"isolated-model","test_settings_only":"yes"}')
  g11_m_id=$(create_config "claude-mcp" "global" \
    '{"mcpServers":{"test-server":{"command":"echo","args":["hello"]}}}')

  if [ -z "$g11_s_id" ] || [ -z "$g11_m_id" ]; then
    fail "create cross-category beads" "failed to create"
  else
    pass "cross-category beads created (settings $g11_s_id, mcp $g11_m_id)"
    iso11=$(fresh_workspace)

    local rc11=0
    run_setup "$iso11" --role "isolated-role-$TEST_TAG" || rc11=$?
    assert_exit "setup with cross-category" 0 "$rc11"

    local us11
    us11=$(user_settings "$iso11")
    if [ -f "$us11" ]; then
      # User settings should NOT contain MCP config
      assert_json_key_absent "mcpServers not in user settings" "$us11" "mcpServers"
      # Settings-only key should be present
      assert_json_value "settings key present" "$us11" "test_settings_only" "yes"
    else
      fail "user settings for cross-category" "not found at $us11"
    fi

    # MCP config goes to .mcp.json (workspace level), not user settings.
    # Verify cross-category isolation: user settings must not have MCP keys.
    # The .mcp.json itself may contain servers from the E2E env, so we don't
    # assert specific MCP content — the isolation check above is sufficient.
    pass "cross-category isolation verified (settings vs mcp)"
  fi
  rm -rf "${iso11:-}"
  close_bead "$g11_s_id"
  close_bead "$g11_m_id"
  echo

  # ── Scenario 12: Closed bead excluded from merge ───────────────
  blue "Scenario 12: Closed bead excluded from merge"
  local g12_id r12_id iso12_a iso12_b
  g12_id=$(create_config "claude-settings" "global" \
    '{"test_model":"base","test_base":"still-here"}')
  r12_id=$(create_config "claude-settings" "role:$E2E_ROLE" \
    '{"test_model":"role-override","test_role":"will-vanish"}')

  if [ -z "$g12_id" ] || [ -z "$r12_id" ]; then
    fail "create beads for closed-bead test" "failed to create"
  else
    pass "beads for closed-bead test created"

    # First: verify both beads participate in merge
    iso12_a=$(fresh_workspace)
    local rc12a=0
    run_setup "$iso12_a" --role "$E2E_ROLE" || rc12a=$?

    local us12a
    us12a=$(user_settings "$iso12_a")
    if [ -f "$us12a" ]; then
      assert_json_value "before close: role override active" "$us12a" "test_model" "role-override"
      assert_json_key_exists "before close: role key present" "$us12a" "test_role"
    fi
    rm -rf "${iso12_a:-}"

    # Close the role bead
    close_bead "$r12_id"

    # Second: verify closed bead is excluded
    iso12_b=$(fresh_workspace)
    local rc12b=0
    run_setup "$iso12_b" --role "$E2E_ROLE" || rc12b=$?

    local us12b
    us12b=$(user_settings "$iso12_b")
    if [ -f "$us12b" ]; then
      # Only global values should remain (role bead is closed)
      assert_json_value "after close: fallback to global" "$us12b" "test_model" "base"
      assert_json_key_absent "after close: role key excluded" "$us12b" "test_role"
      # Global-only key still present
      assert_json_value "after close: global key survives" "$us12b" "test_base" "still-here"
    else
      fail "user settings after closing role bead" "not found at $us12b"
    fi
    rm -rf "${iso12_b:-}"
  fi
  close_bead "$g12_id"
  echo

  # ── Cleanup prerequisite hooks bead ─────────────────────────────
  close_bead "$hooks_id"

  # ── Summary ─────────────────────────────────────────────────────
  blue "═══════════════════════════════════════════════════"
  local total=$((PASS + FAIL))
  if [ "$FAIL" -eq 0 ]; then
    green " All $total assertions passed"
  else
    red " $FAIL/$total assertions failed"
    echo
    for err in "${ERRORS[@]}"; do
      red "   - $err"
    done
  fi
  blue "═══════════════════════════════════════════════════"

  [ "$FAIL" -eq 0 ]
}

main "$@"
