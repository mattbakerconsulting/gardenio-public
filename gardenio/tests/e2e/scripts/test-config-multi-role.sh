#!/usr/bin/env bash
# test-config-multi-role.sh — multi-role precedence E2E tests
#
# Tests that when BOAT_ROLE contains comma-separated roles, the first
# role has higher precedence (its values override later roles).
#
# Specificity order within multi-role: first role in BOAT_ROLE wins.
# The resolution logic inverts role indices so earlier roles sort last
# (later items override earlier in MergeOverride), giving first role
# the final say.
#
# Scenarios tested:
#   1.  Single role resolves normally — role overrides global
#   2.  First role wins for overlapping keys (A,B → A wins)
#   3.  Reversed order changes winner (B,A → B wins)
#   4.  Non-overlapping keys from both roles survive
#   5.  Global base keys survive under both roles
#   6.  Three-layer: global + two roles — global unique keys persist
#   7.  MergeConcat: hooks from both roles concatenate (not override)
#   8.  Global hooks included alongside both role hooks
#   9.  Project layer + multi-role — role still overrides project
#   10. Closed role bead excluded from multi-role merge
#   11. Non-matching third role filtered out
#   12. Empty BOAT_ROLE falls back to global only

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
# Track created beads in a FILE (not a bash array) because create_config is
# invoked via command substitution `id=$(create_config ...)`, which runs in a
# subshell. Array mutations made in that subshell would be lost in the parent,
# leaking beads at exit cleanup. See peer fix for test-config-mcp.sh
# (gd-WqpCJLNP3J, commit 58e63cb3).
CREATED_BEADS_FILE=$(mktemp /tmp/e2e-multi-role-created-XXXXXX)

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
  local port=19105
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-multi-role.log 2>&1 &
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
  if [ -f "$CREATED_BEADS_FILE" ]; then
    while IFS= read -r id; do
      [ -n "$id" ] && "$KD_BIN" close "$id" 2>/dev/null || true
    done < "$CREATED_BEADS_FILE"
    rm -f "$CREATED_BEADS_FILE"
  fi
  [ -d "${WORK_DIR:-}" ] && rm -rf "$WORK_DIR"
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# shellcheck source=./lib-config-cleanup.sh
source "$(dirname "$0")/lib-config-cleanup.sh"

WORK_DIR=$(mktemp -d /tmp/e2e-multi-role-XXXXXX)

# create_config creates a config bead with ONLY the explicitly given label.
# All project-injecting env vars are unset and --project "" is passed
# explicitly so kd cannot auto-add a project:<X> label that would force
# MatchesSubscriptions to drop the bead during setup.
create_config() {
  local title="$1" label="$2" value="$3"
  local out id
  out=$(env -u BOAT_PROJECT -u KD_PROJECT -u BOAT_RIG "$KD_BIN" create "$title" \
    --type=config -l "$label" --project "" -d "$value" --json 2>/dev/null || echo "{}")
  id=$(echo "$out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$id" ]; then
    # Append to the tracking file (not a bash array) so the mutation survives
    # the subshell created by `id=$(create_config ...)` in the caller.
    echo "$id" >> "$CREATED_BEADS_FILE"
    echo "$id"
  else
    echo ""
  fi
}

# cleanup_stale_beads closes any open claude-* config bead in the cluster
# EXCEPT those owned by this test run (tracked in CREATED_BEADS_FILE).
# Called inside run_setup to defend against stale beads leaking in from
# parallel shards / aborted prior runs between scenarios — without
# touching the in-flight beads the current scenario depends on.
cleanup_stale_beads() {
  local stale_ids
  stale_ids=$(env -u BOAT_PROJECT -u KD_PROJECT -u BOAT_RIG "$KD_BIN" list \
    --type=config --status=open --limit=500 --all-projects --json 2>/dev/null | python3 -c "
import sys, json
try:
    beads = json.load(sys.stdin)
except Exception:
    sys.exit(0)
for b in beads:
    title = b.get('title','')
    if title in ('claude-settings', 'claude-hooks', 'claude-mcp', 'claude-instructions'):
        print(b['id'])
" 2>/dev/null || echo "")
  if [ -z "$stale_ids" ]; then return 0; fi
  local preserve=",,"
  if [ -s "$CREATED_BEADS_FILE" ]; then
    preserve=",$(tr '\n' ',' < "$CREATED_BEADS_FILE")"
  fi
  local count=0
  while IFS= read -r id; do
    [ -z "$id" ] && continue
    case "$preserve" in
      *",$id,"*) continue ;;  # test owns this bead, skip
    esac
    "$KD_BIN" close "$id" --actor "$KD_ACTOR" >/dev/null 2>&1 || true
    count=$((count + 1))
  done <<< "$stale_ids"
  if [ "$count" -gt 0 ]; then
    dim "  Cleaned $count stale config bead(s) before scenario"
  fi
  return 0
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
  iso_home=$(mktemp -d /tmp/e2e-mr-home-XXXXXX)
  mkdir -p "$iso_home/.claude"
  echo "$iso_home"
}

# run_setup <iso_home> [KEY=VAL...] [--flag...] — runs gb setup claude with
# isolated HOME. Extra env vars (containing '=') are consumed first; remaining
# arguments are forwarded to gb setup claude as flags.
#
# Calls cleanup_stale_beads first to close any leaked claude-* config beads
# from parallel shards / prior runs while preserving this test's own beads
# (tracked in CREATED_BEADS_FILE). This is what kept S7/S8/S10 stable in
# test-config-mcp.sh after gd-WqpCJLNP3J (commit 58e63cb3) and was missing
# here.
run_setup() {
  local iso_home="$1"
  shift
  cleanup_stale_beads
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

# ws_settings — path to the workspace-level settings.local.json (hooks).
ws_settings() {
  echo "$WORK_DIR/.claude/settings.local.json"
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " multi-role precedence E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  cleanup_stale_claude_configs

  local TEST_TAG="e2e-mr-$$-$(date +%s)"
  local ROLE_A="e2e-mrA-$TEST_TAG"
  local ROLE_B="e2e-mrB-$TEST_TAG"
  local ROLE_C="e2e-mrC-$TEST_TAG"
  local E2E_PROJECT="e2e-mr-proj-$TEST_TAG"

  # Create a claude-hooks bead so normal-mode setup succeeds.
  local hooks_id
  hooks_id=$(create_config "claude-hooks" "global" \
    '{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"gb hook prime 2>/dev/null || true"}]}],"Stop":[{"matcher":"","hooks":[{"type":"command","command":"gb hook stop-gate"}]}]}}')
  if [ -z "$hooks_id" ]; then
    red "ERROR: Failed to create prerequisite claude-hooks bead"
    exit 1
  fi
  dim "  Hooks bead: $hooks_id"
  echo

  # ── Setup: create config beads ──────────────────────────────────
  blue "Setup: Creating config beads for roles $ROLE_A, $ROLE_B, and global"

  local g_id ra_id rb_id
  g_id=$(create_config "claude-settings" "global" \
    '{"test_model":"sonnet","test_base":"global-value","test_shared":"from-global"}')
  [ -n "$g_id" ] && dim "  global bead: $g_id"

  ra_id=$(create_config "claude-settings" "role:$ROLE_A" \
    '{"test_model":"opus","test_roleA":"roleA-value","test_shared":"from-roleA"}')
  [ -n "$ra_id" ] && dim "  roleA bead: $ra_id"

  rb_id=$(create_config "claude-settings" "role:$ROLE_B" \
    '{"test_model":"haiku","test_roleB":"roleB-value","test_shared":"from-roleB"}')
  [ -n "$rb_id" ] && dim "  roleB bead: $rb_id"
  echo

  # ── Scenario 1: Single role resolves normally ───────────────────
  blue "Scenario 1: Single role resolves — role overrides global"
  local iso1
  iso1=$(fresh_workspace)

  local rc1=0
  run_setup "$iso1" BOAT_PROJECT="" --role "$ROLE_A" || rc1=$?
  assert_exit "S1: setup with single role" 0 "$rc1"

  local us1
  us1=$(user_settings "$iso1")
  if [ -f "$us1" ]; then
    assert_json_value "S1: roleA overrides model" "$us1" "test_model" "opus"
    assert_json_value "S1: roleA overrides shared" "$us1" "test_shared" "from-roleA"
    assert_json_value "S1: global base survives" "$us1" "test_base" "global-value"
    assert_json_value "S1: roleA unique key present" "$us1" "test_roleA" "roleA-value"
    assert_json_key_absent "S1: roleB key absent" "$us1" "test_roleB"
  else
    fail "S1: user settings" "not found at $us1"
  fi
  rm -rf "${iso1:-}"
  echo

  # ── Scenario 2: First role wins (A,B → A wins) ─────────────────
  blue "Scenario 2: First role wins for overlapping keys ($ROLE_A,$ROLE_B)"
  local iso2
  iso2=$(fresh_workspace)

  local rc2=0
  run_setup "$iso2" BOAT_PROJECT="" --role "$ROLE_A,$ROLE_B" || rc2=$?
  assert_exit "S2: multi-role setup A,B" 0 "$rc2"

  local us2
  us2=$(user_settings "$iso2")
  if [ -f "$us2" ]; then
    # First role (A) should win for overlapping keys
    assert_json_value "S2: first role A wins model" "$us2" "test_model" "opus"
    assert_json_value "S2: first role A wins shared" "$us2" "test_shared" "from-roleA"
    # Non-overlapping keys from both roles survive
    assert_json_key_exists "S2: roleA unique key present" "$us2" "test_roleA"
    assert_json_key_exists "S2: roleB unique key present" "$us2" "test_roleB"
    # Global base survives
    assert_json_value "S2: global base survives" "$us2" "test_base" "global-value"
  else
    fail "S2: user settings" "not found at $us2"
  fi
  rm -rf "${iso2:-}"
  echo

  # ── Scenario 3: Reversed order changes winner (B,A → B wins) ───
  blue "Scenario 3: Reversed order ($ROLE_B,$ROLE_A → $ROLE_B wins)"
  local iso3
  iso3=$(fresh_workspace)

  local rc3=0
  run_setup "$iso3" BOAT_PROJECT="" --role "$ROLE_B,$ROLE_A" || rc3=$?
  assert_exit "S3: multi-role setup B,A" 0 "$rc3"

  local us3
  us3=$(user_settings "$iso3")
  if [ -f "$us3" ]; then
    # First role (B) should now win for overlapping keys
    assert_json_value "S3: first role B wins model" "$us3" "test_model" "haiku"
    assert_json_value "S3: first role B wins shared" "$us3" "test_shared" "from-roleB"
    # Non-overlapping keys from both roles still survive
    assert_json_value "S3: roleA unique key survives" "$us3" "test_roleA" "roleA-value"
    assert_json_value "S3: roleB unique key survives" "$us3" "test_roleB" "roleB-value"
    # Global base survives
    assert_json_value "S3: global base survives" "$us3" "test_base" "global-value"
  else
    fail "S3: user settings" "not found at $us3"
  fi
  rm -rf "${iso3:-}"
  echo

  # ── Scenario 4: Non-overlapping keys from both roles survive ────
  blue "Scenario 4: Non-overlapping keys from both roles survive"
  local iso4
  iso4=$(fresh_workspace)

  local rc4=0
  run_setup "$iso4" BOAT_PROJECT="" --role "$ROLE_A,$ROLE_B" || rc4=$?

  local us4
  us4=$(user_settings "$iso4")
  if [ -f "$us4" ]; then
    assert_json_value "S4: roleA unique key" "$us4" "test_roleA" "roleA-value"
    assert_json_value "S4: roleB unique key" "$us4" "test_roleB" "roleB-value"
    # All three layers contribute simultaneously
    assert_json_key_exists "S4: test_model present" "$us4" "test_model"
    assert_json_key_exists "S4: test_base present" "$us4" "test_base"
    assert_json_key_exists "S4: test_shared present" "$us4" "test_shared"
    assert_json_key_exists "S4: test_roleA present" "$us4" "test_roleA"
    assert_json_key_exists "S4: test_roleB present" "$us4" "test_roleB"
  else
    fail "S4: user settings" "not found at $us4"
  fi
  rm -rf "${iso4:-}"
  echo

  # ── Scenario 5: Global base keys survive under both roles ───────
  blue "Scenario 5: Global base keys survive under both roles"
  local iso5
  iso5=$(fresh_workspace)

  local rc5=0
  run_setup "$iso5" BOAT_PROJECT="" --role "$ROLE_A,$ROLE_B" || rc5=$?

  local us5
  us5=$(user_settings "$iso5")
  if [ -f "$us5" ]; then
    # Global-only key (test_base) should survive since neither role overrides it
    assert_json_value "S5: global test_base survives" "$us5" "test_base" "global-value"
    # Global's test_shared is overridden by the first role
    assert_json_value "S5: global shared overridden by first role" "$us5" "test_shared" "from-roleA"
  else
    fail "S5: user settings" "not found at $us5"
  fi
  rm -rf "${iso5:-}"
  echo

  # ── Scenario 6: Three-layer: global + two roles ─────────────────
  blue "Scenario 6: Three-layer merge — all layers contribute"
  local iso6
  iso6=$(fresh_workspace)

  local rc6=0
  run_setup "$iso6" BOAT_PROJECT="" --role "$ROLE_A,$ROLE_B" || rc6=$?

  local us6
  us6=$(user_settings "$iso6")
  if [ -f "$us6" ]; then
    # Overlapping: first role wins
    assert_json_value "S6: first role wins model" "$us6" "test_model" "opus"
    # Each layer's unique key survives
    assert_json_value "S6: global base" "$us6" "test_base" "global-value"
    assert_json_value "S6: roleA unique" "$us6" "test_roleA" "roleA-value"
    assert_json_value "S6: roleB unique" "$us6" "test_roleB" "roleB-value"
    # Shared won by first role
    assert_json_value "S6: shared from first role" "$us6" "test_shared" "from-roleA"
  else
    fail "S6: user settings" "not found at $us6"
  fi
  rm -rf "${iso6:-}"
  echo

  # ── Scenario 7: MergeConcat: hooks from both roles concatenate ──
  blue "Scenario 7: Hooks from both roles concatenate (MergeConcat)"
  local ha_id hb_id
  ha_id=$(create_config "claude-hooks" "role:$ROLE_A" \
    '{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"echo hookA"}]}]}}')
  [ -n "$ha_id" ] && dim "  roleA hooks bead: $ha_id"

  hb_id=$(create_config "claude-hooks" "role:$ROLE_B" \
    '{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"echo hookB"}]}]}}')
  [ -n "$hb_id" ] && dim "  roleB hooks bead: $hb_id"

  local iso7
  iso7=$(fresh_workspace)

  local rc7=0
  run_setup "$iso7" BOAT_PROJECT="" --role "$ROLE_A,$ROLE_B" || rc7=$?
  assert_exit "S7: multi-role hooks setup" 0 "$rc7"

  local ws7
  ws7=$(ws_settings)
  if [ -f "$ws7" ]; then
    local hooks_content
    hooks_content=$(cat "$ws7")
    # All hooks should be concatenated (not overridden)
    assert_contains "S7: hookA present" "hookA" "$hooks_content"
    assert_contains "S7: hookB present" "hookB" "$hooks_content"
  else
    fail "S7: workspace settings.json" "not created"
  fi
  rm -rf "${iso7:-}"
  echo

  # ── Scenario 8: Global hooks included alongside both role hooks ─
  blue "Scenario 8: Global hooks included alongside both role hooks"
  local iso8
  iso8=$(fresh_workspace)

  local rc8=0
  run_setup "$iso8" BOAT_PROJECT="" --role "$ROLE_A,$ROLE_B" || rc8=$?

  local ws8
  ws8=$(ws_settings)
  if [ -f "$ws8" ]; then
    local hooks8
    hooks8=$(cat "$ws8")
    # Global hooks (from prerequisite hooks bead) + both role hooks
    assert_contains "S8: global hooks present" "gb hook prime" "$hooks8"
    assert_contains "S8: roleA hooks present" "hookA" "$hooks8"
    assert_contains "S8: roleB hooks present" "hookB" "$hooks8"
  else
    fail "S8: workspace settings.json" "not created"
  fi
  rm -rf "${iso8:-}"
  # Clean up role hooks beads
  close_bead "$ha_id"
  close_bead "$hb_id"
  echo

  # ── Scenario 9: Project layer + multi-role ──────────────────────
  blue "Scenario 9: Project layer + multi-role — first role still wins over project"
  local p_id
  p_id=$(create_config "claude-settings" "project:$E2E_PROJECT" \
    '{"test_model":"project-model","test_proj":"project-value","test_shared":"from-project"}')
  [ -n "$p_id" ] && dim "  project bead: $p_id"

  local iso9
  iso9=$(fresh_workspace)

  local rc9=0
  run_setup "$iso9" BOAT_PROJECT="$E2E_PROJECT" --role "$ROLE_A,$ROLE_B" || rc9=$?
  assert_exit "S9: project + multi-role setup" 0 "$rc9"

  local us9
  us9=$(user_settings "$iso9")
  if [ -f "$us9" ]; then
    # Role overrides project for overlapping keys (role > project in specificity)
    assert_json_value "S9: first role wins model over project" "$us9" "test_model" "opus"
    assert_json_value "S9: first role wins shared over project" "$us9" "test_shared" "from-roleA"
    # Project unique key survives
    assert_json_value "S9: project unique key survives" "$us9" "test_proj" "project-value"
    # Global base survives
    assert_json_value "S9: global base survives" "$us9" "test_base" "global-value"
  else
    fail "S9: user settings" "not found at $us9"
  fi
  rm -rf "${iso9:-}"
  close_bead "$p_id"
  echo

  # ── Scenario 10: Closed role bead excluded ──────────────────────
  blue "Scenario 10: Closed role bead excluded from multi-role merge"

  # Close roleA bead — only roleB should participate
  close_bead "$ra_id"

  local iso10
  iso10=$(fresh_workspace)

  local rc10=0
  run_setup "$iso10" BOAT_PROJECT="" --role "$ROLE_A,$ROLE_B" || rc10=$?
  assert_exit "S10: setup after closing roleA" 0 "$rc10"

  local us10
  us10=$(user_settings "$iso10")
  if [ -f "$us10" ]; then
    # With roleA closed, roleB should win for model (only remaining role)
    assert_json_value "S10: roleB wins model (roleA closed)" "$us10" "test_model" "haiku"
    assert_json_value "S10: roleB wins shared (roleA closed)" "$us10" "test_shared" "from-roleB"
    # roleA unique key should NOT appear (bead is closed)
    assert_json_key_absent "S10: roleA key excluded (closed)" "$us10" "test_roleA"
    # roleB unique key should appear
    assert_json_value "S10: roleB unique key present" "$us10" "test_roleB" "roleB-value"
    # Global base survives
    assert_json_value "S10: global base survives" "$us10" "test_base" "global-value"
  else
    fail "S10: user settings" "not found at $us10"
  fi
  rm -rf "${iso10:-}"

  # Re-create roleA for remaining scenarios
  ra_id=$(create_config "claude-settings" "role:$ROLE_A" \
    '{"test_model":"opus","test_roleA":"roleA-value","test_shared":"from-roleA"}')
  [ -n "$ra_id" ] && dim "  roleA bead re-created: $ra_id"
  echo

  # ── Scenario 11: Non-matching third role filtered out ───────────
  blue "Scenario 11: Non-matching third role filtered out"

  # roleC exists but is NOT in subscriptions — its values should not appear
  local rc_id
  rc_id=$(create_config "claude-settings" "role:$ROLE_C" \
    '{"test_model":"wrong-model","test_roleC":"should-not-appear"}')
  [ -n "$rc_id" ] && dim "  roleC bead: $rc_id"

  local iso11
  iso11=$(fresh_workspace)

  local rc11=0
  # Only subscribe to A,B — roleC should be excluded
  run_setup "$iso11" BOAT_PROJECT="" --role "$ROLE_A,$ROLE_B" || rc11=$?
  assert_exit "S11: setup with non-matching roleC" 0 "$rc11"

  local us11
  us11=$(user_settings "$iso11")
  if [ -f "$us11" ]; then
    # First role A still wins
    assert_json_value "S11: first role A wins model" "$us11" "test_model" "opus"
    # roleC key should NOT appear
    assert_json_key_absent "S11: roleC key excluded" "$us11" "test_roleC"
  else
    fail "S11: user settings" "not found at $us11"
  fi
  rm -rf "${iso11:-}"
  close_bead "$rc_id"
  echo

  # ── Scenario 12: Empty BOAT_ROLE falls back to global only ──────
  blue "Scenario 12: Empty BOAT_ROLE falls back to global only"
  local iso12
  iso12=$(fresh_workspace)

  local rc12=0
  run_setup "$iso12" BOAT_PROJECT="" --role "" || rc12=$?
  assert_exit "S12: setup with empty role" 0 "$rc12"

  local us12
  us12=$(user_settings "$iso12")
  if [ -f "$us12" ]; then
    # Only global values should appear
    assert_json_value "S12: global model (no role)" "$us12" "test_model" "sonnet"
    assert_json_value "S12: global shared (no role)" "$us12" "test_shared" "from-global"
    assert_json_value "S12: global base (no role)" "$us12" "test_base" "global-value"
    # Role keys should NOT appear
    assert_json_key_absent "S12: roleA key absent" "$us12" "test_roleA"
    assert_json_key_absent "S12: roleB key absent" "$us12" "test_roleB"
  else
    fail "S12: user settings" "not found at $us12"
  fi
  rm -rf "${iso12:-}"
  echo

  # ── Cleanup ──────────────────────────────────────────────────────
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
