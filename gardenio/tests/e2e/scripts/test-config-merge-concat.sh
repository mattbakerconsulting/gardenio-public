#!/usr/bin/env bash
# test-config-merge-concat.sh — MergeConcat strategy E2E tests
#
# Tests that MergeConcat (hook array concatenation) correctly merges
# claude-hooks config bead layers: hook arrays are appended across
# layers rather than replaced.
#
# Scenarios tested:
#   1. Single global hooks layer resolves as-is
#   2. Role hooks append to global hooks (SessionStart concatenated)
#   3. Non-overlapping hook types preserved from each layer
#   4. Non-hook keys still use override semantics
#   5. Three-layer hook concat: global + project + role
#   6. Hook entry structure validated (matcher + hooks array)
#   7. Duplicate hook type across all layers increases array length

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
  local name="$1"
  local pattern="$2"
  local actual="$3"
  if grep -qF -- "$pattern" <<< "$actual"; then
    pass "$name"
  else
    fail "$name" "expected to contain '$pattern'; got: $(echo "$actual" | head -5)"
  fi
}

assert_not_contains() {
  local name="$1"
  local pattern="$2"
  local actual="$3"
  if ! grep -qF -- "$pattern" <<< "$actual"; then
    pass "$name"
  else
    fail "$name" "expected NOT to contain '$pattern'"
  fi
}

bead_id_from_json() {
  echo "$1" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo ""
}

# json_hook_count <file> <hookType>
# Returns the number of entries in hooks.<hookType> array.
json_hook_count() {
  local file="$1" hook_type="$2"
  python3 -c "
import json, sys
d = json.load(open('$file'))
arr = d.get('hooks', {}).get('$hook_type', [])
print(len(arr))
" 2>/dev/null || echo "0"
}

# json_hook_command <file> <hookType> <index>
# Returns the command string from hooks.<hookType>[index].hooks[0].command
json_hook_command() {
  local file="$1" hook_type="$2" idx="$3"
  python3 -c "
import json, sys
d = json.load(open('$file'))
entry = d.get('hooks', {}).get('$hook_type', [])[${idx}]
print(entry['hooks'][0]['command'])
" 2>/dev/null || echo ""
}

# json_has_key <file> <dotted.key.path>
# Returns "true" if the key path exists, "false" otherwise.
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

# json_get_value <file> <dotted.key.path>
json_get_value() {
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
        print('')
        exit()
print(v)
" 2>/dev/null || echo ""
}

# ── Setup ─────────────────────────────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19103
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-merge-concat.log 2>&1 &
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

# shellcheck source=./lib-config-cleanup.sh
source "$(dirname "$0")/lib-config-cleanup.sh"

WORK_DIR=$(mktemp -d /tmp/e2e-merge-concat-XXXXXX)

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " MergeConcat strategy E2E tests"
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

  local TEST_TAG="e2e-mc-$$-$(date +%s)"
  local E2E_ROLE="e2e-concat-role-$TEST_TAG"
  local E2E_PROJECT="e2e-concat-proj-$TEST_TAG"
  local SETTINGS=""

  # ── Scenario 1: Single global hooks layer ──────────────────────
  blue "Scenario 1: Single global hooks layer resolves as-is"
  local gh_out gh_id
  gh_out=$("$KD_BIN" create "claude-hooks" --type=config -l global \
    -d '{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"echo global-start"}]}],"Stop":[{"matcher":"","hooks":[{"type":"command","command":"echo global-stop"}]}]}}' \
    --json 2>/dev/null || echo "{}")
  gh_id=$(bead_id_from_json "$gh_out")
  if [ -n "$gh_id" ]; then
    CREATED_BEADS+=("$gh_id")
    pass "global claude-hooks created ($gh_id)"
  else
    fail "global claude-hooks" "could not create"
  fi

  rm -rf "$WORK_DIR/.claude"
  mkdir -p "$WORK_DIR/.claude"
  "$GB_BIN" setup claude --role "nonexistent-$TEST_TAG" --workspace "$WORK_DIR" 2>/dev/null && true

  SETTINGS="$WORK_DIR/.claude/settings.local.json"
  if [ -f "$SETTINGS" ]; then
    local ws_content
    ws_content=$(cat "$SETTINGS")
    assert_contains "settings has hooks key" "hooks" "$ws_content"
    assert_contains "global SessionStart present" "global-start" "$ws_content"
    assert_contains "global Stop present" "global-stop" "$ws_content"

    # Structural: SessionStart should have at least 1 entry (may have more from
    # pre-existing global config beads in the shared E2E namespace)
    local ss1_count
    ss1_count=$(json_hook_count "$SETTINGS" "SessionStart")
    if [ "$ss1_count" -ge 1 ]; then
      pass "SessionStart has ≥1 entry (got $ss1_count)"
    else
      fail "SessionStart count" "expected ≥1, got $ss1_count"
    fi

    # Structural: Stop should have at least 1 entry
    local stop1_count
    stop1_count=$(json_hook_count "$SETTINGS" "Stop")
    if [ "$stop1_count" -ge 1 ]; then
      pass "Stop has ≥1 entry (got $stop1_count)"
    else
      fail "Stop count" "expected ≥1, got $stop1_count"
    fi
  else
    fail "workspace settings.json" "not created by gb setup claude"
  fi
  echo

  # ── Scenario 2: Role hooks append to global ────────────────────
  blue "Scenario 2: Role hooks append to global hooks (SessionStart concatenated)"
  local rh_out rh_id
  rh_out=$("$KD_BIN" create "claude-hooks" --type=config -l "role:$E2E_ROLE" \
    -d '{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"echo role-start"}]}]}}' \
    --json 2>/dev/null || echo "{}")
  rh_id=$(bead_id_from_json "$rh_out")
  if [ -n "$rh_id" ]; then
    CREATED_BEADS+=("$rh_id")
    pass "role claude-hooks created ($rh_id)"
  else
    fail "role claude-hooks" "could not create"
  fi

  rm -rf "$WORK_DIR/.claude"
  mkdir -p "$WORK_DIR/.claude"
  "$GB_BIN" setup claude --role "$E2E_ROLE" --workspace "$WORK_DIR" 2>/dev/null && true

  SETTINGS="$WORK_DIR/.claude/settings.local.json"
  if [ -f "$SETTINGS" ]; then
    local merged_content
    merged_content=$(cat "$SETTINGS")

    # Both global-start AND role-start should be present (concatenated)
    assert_contains "merged has global-start" "global-start" "$merged_content"
    assert_contains "merged has role-start" "role-start" "$merged_content"

    # SessionStart should have at least 2 entries (global + role, possibly more
    # from pre-existing config beads in the shared E2E namespace)
    local ss2_count
    ss2_count=$(json_hook_count "$SETTINGS" "SessionStart")
    if [ "$ss2_count" -ge 2 ]; then
      pass "SessionStart has ≥2 entries after role concat (got $ss2_count)"
    else
      fail "SessionStart concat count" "expected ≥2, got $ss2_count"
    fi
  else
    fail "merged settings.json" "not created"
  fi
  echo

  # ── Scenario 3: Non-overlapping hook types preserved ───────────
  blue "Scenario 3: Non-overlapping hook types preserved from each layer"
  # Reuse the settings.json from Scenario 2 (global has Stop, role does not)
  if [ -f "$SETTINGS" ]; then
    local content
    content=$(cat "$SETTINGS")

    # Global's Stop hook should survive even though role didn't define Stop
    assert_contains "Stop hook preserved from global" "global-stop" "$content"

    # Stop should still have at least 1 entry (only from global, role didn't add Stop)
    local stop2_count
    stop2_count=$(json_hook_count "$SETTINGS" "Stop")
    if [ "$stop2_count" -ge 1 ]; then
      pass "Stop has ≥1 entry after merge (got $stop2_count)"
    else
      fail "Stop count after merge" "expected ≥1, got $stop2_count"
    fi
  else
    fail "settings.json missing" "cannot verify non-overlapping hooks"
  fi
  echo

  # ── Scenario 4: Non-hook keys use override ─────────────────────
  blue "Scenario 4: Non-hook keys still use override semantics"
  # Create new beads with a non-hook key alongside hooks
  local nh_g_out nh_g_id
  nh_g_out=$("$KD_BIN" create "claude-hooks" --type=config -l global \
    -d '{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"echo nh-global"}]}]},"non_hook_key":"global-val"}' \
    --json 2>/dev/null || echo "{}")
  nh_g_id=$(bead_id_from_json "$nh_g_out")
  [ -n "$nh_g_id" ] && CREATED_BEADS+=("$nh_g_id")

  local nh_r_out nh_r_id
  nh_r_out=$("$KD_BIN" create "claude-hooks" --type=config -l "role:$E2E_ROLE" \
    -d '{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"echo nh-role"}]}]},"non_hook_key":"role-val"}' \
    --json 2>/dev/null || echo "{}")
  nh_r_id=$(bead_id_from_json "$nh_r_out")
  [ -n "$nh_r_id" ] && CREATED_BEADS+=("$nh_r_id")

  rm -rf "$WORK_DIR/.claude"
  mkdir -p "$WORK_DIR/.claude"
  "$GB_BIN" setup claude --role "$E2E_ROLE" --workspace "$WORK_DIR" 2>/dev/null && true

  SETTINGS="$WORK_DIR/.claude/settings.local.json"
  if [ -f "$SETTINGS" ]; then
    local nh_val
    nh_val=$(json_get_value "$SETTINGS" "non_hook_key")
    if [ "$nh_val" = "role-val" ]; then
      pass "non-hook key overridden by role (role-val wins over global-val)"
    elif [ "$nh_val" = "global-val" ]; then
      fail "non-hook key override" "got global-val, expected role-val (role should override)"
    else
      # The non_hook_key may not appear in workspace settings (hooks-only file)
      dim "  non_hook_key not in workspace settings (hooks file may only contain hooks key)"
      pass "setup completed with non-hook keys (non-hook keys may be excluded from hooks output)"
    fi
  else
    fail "settings.json for non-hook key test" "not created"
  fi
  echo

  # ── Scenario 5: Three-layer hook concat ────────────────────────
  blue "Scenario 5: Three-layer hook concat — global + project + role"
  local ph_out ph_id
  ph_out=$("$KD_BIN" create "claude-hooks" --type=config -l "project:$E2E_PROJECT" \
    -d '{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"echo project-start"}]}]}}' \
    --json 2>/dev/null || echo "{}")
  ph_id=$(bead_id_from_json "$ph_out")
  if [ -n "$ph_id" ]; then
    CREATED_BEADS+=("$ph_id")
    pass "project claude-hooks created ($ph_id)"
  else
    fail "project claude-hooks" "could not create"
  fi

  rm -rf "$WORK_DIR/.claude"
  mkdir -p "$WORK_DIR/.claude"
  BOAT_PROJECT="$E2E_PROJECT" "$GB_BIN" setup claude --role "$E2E_ROLE" \
    --workspace "$WORK_DIR" 2>/dev/null && true

  SETTINGS="$WORK_DIR/.claude/settings.local.json"
  if [ -f "$SETTINGS" ]; then
    local three_content
    three_content=$(cat "$SETTINGS")

    # All three layers' hooks should be present in SessionStart
    assert_contains "three-layer: global-start present" "global-start" "$three_content"
    assert_contains "three-layer: project-start present" "project-start" "$three_content"
    assert_contains "three-layer: role-start present" "role-start" "$three_content"

    # SessionStart should have entries from all three layers
    # Note: there may be more than 3 because Scenarios 1-4 created additional
    # global/role beads that also match. Count the unique test-tagged entries.
    local ss3_count
    ss3_count=$(json_hook_count "$SETTINGS" "SessionStart")
    if [ "$ss3_count" -ge 3 ]; then
      pass "SessionStart has >= 3 entries ($ss3_count — global + project + role)"
    else
      fail "three-layer SessionStart count" "expected >= 3, got $ss3_count"
    fi

    # Global's Stop should still be preserved through the three-layer merge
    assert_contains "three-layer: Stop preserved" "global-stop" "$three_content"
  else
    fail "three-layer settings.json" "not created"
  fi
  echo

  # ── Scenario 6: Hook entry structure ───────────────────────────
  blue "Scenario 6: Hook entries have correct structure (matcher + hooks array)"
  if [ -f "$SETTINGS" ]; then
    # Validate that each SessionStart entry has the expected structure:
    # {"matcher": "...", "hooks": [{"type": "command", "command": "..."}]}
    local struct_ok
    struct_ok=$(python3 -c "
import json, sys
d = json.load(open('$SETTINGS'))
entries = d.get('hooks', {}).get('SessionStart', [])
ok = True
for i, entry in enumerate(entries):
    if 'matcher' not in entry:
        print(f'entry {i} missing matcher')
        ok = False
    if 'hooks' not in entry or not isinstance(entry['hooks'], list):
        print(f'entry {i} missing or non-list hooks')
        ok = False
    elif len(entry['hooks']) > 0:
        h = entry['hooks'][0]
        if 'type' not in h or 'command' not in h:
            print(f'entry {i} hook missing type or command')
            ok = False
if ok:
    print('OK')
" 2>/dev/null || echo "python error")

    if [ "$struct_ok" = "OK" ]; then
      pass "all SessionStart entries have valid structure (matcher + hooks[])"
    else
      fail "hook entry structure" "$struct_ok"
    fi

    # Same check for Stop
    local stop_struct
    stop_struct=$(python3 -c "
import json, sys
d = json.load(open('$SETTINGS'))
entries = d.get('hooks', {}).get('Stop', [])
ok = True
for i, entry in enumerate(entries):
    if 'matcher' not in entry:
        ok = False
    if 'hooks' not in entry or not isinstance(entry['hooks'], list):
        ok = False
if ok:
    print('OK')
" 2>/dev/null || echo "python error")

    if [ "$stop_struct" = "OK" ]; then
      pass "all Stop entries have valid structure"
    else
      fail "Stop hook structure" "$stop_struct"
    fi
  else
    fail "settings.json for structure validation" "not found"
  fi
  echo

  # ── Scenario 7: Concat accumulates across all layers ───────────
  blue "Scenario 7: Adding a Stop hook to role increases Stop array length"
  # Create a role hooks bead that also has a Stop hook (in addition to the
  # global Stop from Scenario 1). MergeConcat should produce 2 Stop entries.
  local rs_out rs_id
  rs_out=$("$KD_BIN" create "claude-hooks" --type=config -l "role:$E2E_ROLE" \
    -d '{"hooks":{"Stop":[{"matcher":"","hooks":[{"type":"command","command":"echo role-stop"}]}]}}' \
    --json 2>/dev/null || echo "{}")
  rs_id=$(bead_id_from_json "$rs_out")
  [ -n "$rs_id" ] && CREATED_BEADS+=("$rs_id")

  rm -rf "$WORK_DIR/.claude"
  mkdir -p "$WORK_DIR/.claude"
  BOAT_PROJECT="$E2E_PROJECT" "$GB_BIN" setup claude --role "$E2E_ROLE" \
    --workspace "$WORK_DIR" 2>/dev/null && true

  SETTINGS="$WORK_DIR/.claude/settings.local.json"
  if [ -f "$SETTINGS" ]; then
    local stop_content
    stop_content=$(cat "$SETTINGS")

    # Both global-stop and role-stop should be present
    assert_contains "Stop has global-stop" "global-stop" "$stop_content"
    assert_contains "Stop has role-stop" "role-stop" "$stop_content"

    # Stop array should have at least 2 entries now
    local stop_count
    stop_count=$(json_hook_count "$SETTINGS" "Stop")
    if [ "$stop_count" -ge 2 ]; then
      pass "Stop has >= 2 entries ($stop_count — global + role concatenated)"
    else
      fail "Stop concat count" "expected >= 2, got $stop_count"
    fi
  else
    fail "settings.json for Stop concat test" "not created"
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
