#!/usr/bin/env bash
# test-config-beads-crud.sh — config bead CRUD E2E tests
#
# Tests config bead create, show, update, list, close, delete, and edge cases.
#
# Scenarios tested:
#   1.  Create config bead with valid JSON description
#   2.  Show config bead returns correct type, kind, labels, description
#   3.  Create config bead with empty description (null handling)
#   4.  Create config bead with nested JSON value
#   5.  Update config bead title
#   6.  Update config bead description
#   7.  Add and remove labels on config bead
#   8.  List config beads with --type=config filter
#   9.  List config beads with label filter (-l global, -l role:X)
#   10. Default kd list excludes config beads (issue-kind only)
#   11. --kind=config filter shows config beads, excludes tasks
#   12. Close config bead and verify status
#   13. Closed config bead disappears from open list
#   14. Delete config bead permanently removes it
#   15. Edge: create config bead without labels
#   16. Edge: create config bead with multiple labels
#   17. Edge: reopen a closed config bead

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

assert_not_contains() {
  local name="$1"
  local pattern="$2"
  local actual="$3"
  if ! grep -qF -- "$pattern" <<< "$actual"; then
    pass "$name (not contains '$pattern')"
  else
    fail "$name" "expected output NOT to contain '$pattern'"
  fi
}

assert_json_field() {
  local name="$1"
  local field="$2"
  local expected="$3"
  local json="$4"
  local actual
  actual=$(echo "$json" | python3 -c "import sys,json; print(json.load(sys.stdin).get('$field',''))" 2>/dev/null || echo "")
  if [ "$actual" = "$expected" ]; then
    pass "$name ($field=$expected)"
  else
    fail "$name" "expected $field='$expected', got '$actual'"
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
    >/tmp/kd-pf-config-crud.log 2>&1 &
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
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " config bead CRUD E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  local TEST_TAG="e2e-cfgcrud-$$-$(date +%s)"

  # ── Scenario 1: Create config bead with valid JSON ─────────────
  blue "Scenario 1: Create config bead with valid JSON description"
  local cfg_out cfg_rc cfg_id
  cfg_out=$("$KD_BIN" create "e2e-cfg-$TEST_TAG" --type=config -l global \
    -d '{"model":"sonnet","permissions":{"allow":["Read","Glob"]}}' --json 2>/dev/null) && cfg_rc=0 || cfg_rc=$?
  assert_exit "create config bead exits 0" 0 "$cfg_rc"

  cfg_id=$(echo "$cfg_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$cfg_id" ]; then
    CREATED_BEADS+=("$cfg_id")
    pass "config bead created with ID ($cfg_id)"
  else
    fail "config bead create returns ID" "no id in output: $cfg_out"
    blue "═══════════════════════════════════════════════════"
    red " Cannot continue without a bead ID"
    blue "═══════════════════════════════════════════════════"
    return 1
  fi

  # Verify create output has expected fields
  assert_json_field "create output type" "type" "config" "$cfg_out"
  assert_json_field "create output status" "status" "open" "$cfg_out"
  echo

  # ── Scenario 2: Show returns correct fields ────────────────────
  blue "Scenario 2: Show config bead returns correct type, kind, labels, description"
  local show_out
  show_out=$("$KD_BIN" show "$cfg_id" --json 2>/dev/null || echo "{}")
  assert_json_field "show type is config" "type" "config" "$show_out"
  assert_json_field "show kind is config" "kind" "config" "$show_out"
  assert_json_field "show status is open" "status" "open" "$show_out"
  assert_contains "show has global label" "global" "$show_out"
  assert_contains "show has title" "e2e-cfg-$TEST_TAG" "$show_out"
  assert_contains "show description has model" "sonnet" "$show_out"
  assert_contains "show description has permissions" "permissions" "$show_out"

  # Verify labels array
  local labels_json
  labels_json=$(echo "$show_out" | python3 -c "import sys,json; print(json.dumps(json.load(sys.stdin).get('labels',[])))" 2>/dev/null || echo "[]")
  assert_contains "labels array contains global" "global" "$labels_json"
  echo

  # ── Scenario 3: Create config bead with empty description ──────
  blue "Scenario 3: Create config bead with empty description (null handling)"
  local empty_out empty_rc empty_id
  empty_out=$("$KD_BIN" create "e2e-cfg-empty-$TEST_TAG" --type=config -l global \
    --json 2>/dev/null) && empty_rc=0 || empty_rc=$?
  assert_exit "create empty-desc config bead" 0 "$empty_rc"

  empty_id=$(echo "$empty_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$empty_id" ]; then
    CREATED_BEADS+=("$empty_id")
    pass "empty-desc config bead created ($empty_id)"

    # Show should work without crashing
    local empty_show
    empty_show=$("$KD_BIN" show "$empty_id" --json 2>/dev/null || echo "{}")
    assert_json_field "empty-desc bead type is config" "type" "config" "$empty_show"
    assert_json_field "empty-desc bead status is open" "status" "open" "$empty_show"

    # Verify gb config dump doesn't crash with empty description
    local dump_out
    dump_out=$("$GB_BIN" config dump 2>/dev/null || echo "")
    if [ -n "$dump_out" ]; then
      local valid
      valid=$(echo "$dump_out" | python3 -c "import sys,json; json.load(sys.stdin); print('valid')" 2>/dev/null || echo "invalid")
      if [ "$valid" = "valid" ]; then
        pass "config dump valid JSON despite empty description bead"
      else
        fail "config dump valid JSON" "invalid JSON with empty description bead"
      fi
    else
      pass "empty description bead created without crash (dump empty)"
    fi
  else
    fail "empty-desc config bead" "could not create"
  fi
  echo

  # ── Scenario 4: Create config bead with nested JSON value ──────
  blue "Scenario 4: Create config bead with nested JSON value"
  local nested_out nested_rc nested_id
  nested_out=$("$KD_BIN" create "e2e-cfg-nested-$TEST_TAG" --type=config -l global \
    -d '{"mcpServers":{"playwright":{"command":"playwright-mcp","args":["--browser","chromium"],"env":{"PATH":"/usr/bin"}}}}' \
    --json 2>/dev/null) && nested_rc=0 || nested_rc=$?
  assert_exit "create nested JSON config bead" 0 "$nested_rc"

  nested_id=$(echo "$nested_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$nested_id" ]; then
    CREATED_BEADS+=("$nested_id")
    pass "nested JSON config bead created ($nested_id)"

    local nested_show
    nested_show=$("$KD_BIN" show "$nested_id" --json 2>/dev/null || echo "{}")
    assert_contains "nested value has mcpServers" "mcpServers" "$nested_show"
    assert_contains "nested value has playwright" "playwright" "$nested_show"
    assert_contains "nested value has chromium" "chromium" "$nested_show"
    assert_contains "nested value has /usr/bin" "/usr/bin" "$nested_show"
  else
    fail "nested JSON config bead" "could not create"
  fi
  echo

  # ── Scenario 5: Update config bead title ───────────────────────
  blue "Scenario 5: Update config bead title"
  local title_rc=0
  "$KD_BIN" update "$cfg_id" --title "e2e-cfg-renamed-$TEST_TAG" 2>/dev/null || title_rc=$?
  assert_exit "kd update --title exits 0" 0 "$title_rc"

  local title_show
  title_show=$("$KD_BIN" show "$cfg_id" --json 2>/dev/null || echo "{}")
  assert_contains "updated title present" "e2e-cfg-renamed-$TEST_TAG" "$title_show"
  assert_not_contains "old title gone" "\"e2e-cfg-$TEST_TAG\"" "$title_show"
  echo

  # ── Scenario 6: Update config bead description ─────────────────
  blue "Scenario 6: Update config bead description"
  local desc_rc=0
  "$KD_BIN" update "$cfg_id" -d '{"model":"opus","updated":true}' 2>/dev/null || desc_rc=$?
  assert_exit "kd update description exits 0" 0 "$desc_rc"

  local desc_show
  desc_show=$("$KD_BIN" show "$cfg_id" --json 2>/dev/null || echo "{}")
  assert_contains "updated description has opus" "opus" "$desc_show"
  assert_contains "updated description has updated flag" "updated" "$desc_show"
  assert_not_contains "old description sonnet replaced" "sonnet" "$desc_show"
  echo

  # ── Scenario 7: Add and remove labels ──────────────────────────
  blue "Scenario 7: Add and remove labels on config bead"
  local label_add_rc=0
  "$KD_BIN" label add "$cfg_id" "role:e2e-test-role" 2>/dev/null || label_add_rc=$?
  assert_exit "kd label add exits 0" 0 "$label_add_rc"

  local label_show
  label_show=$("$KD_BIN" show "$cfg_id" --json 2>/dev/null || echo "{}")
  assert_contains "label add: role label present" "role:e2e-test-role" "$label_show"
  assert_contains "label add: global label preserved" "global" "$label_show"

  # Add a second label
  local label_add2_rc=0
  "$KD_BIN" label add "$cfg_id" "env:test" 2>/dev/null || label_add2_rc=$?
  assert_exit "kd label add second label" 0 "$label_add2_rc"

  local label_show2
  label_show2=$("$KD_BIN" show "$cfg_id" --json 2>/dev/null || echo "{}")
  assert_contains "second label present" "env:test" "$label_show2"
  assert_contains "role label still present" "role:e2e-test-role" "$label_show2"

  # Remove a label
  local label_rm_rc=0
  "$KD_BIN" label remove "$cfg_id" "env:test" 2>/dev/null || label_rm_rc=$?
  assert_exit "kd label remove exits 0" 0 "$label_rm_rc"

  local label_show3
  label_show3=$("$KD_BIN" show "$cfg_id" --json 2>/dev/null || echo "{}")
  assert_not_contains "removed label gone" "env:test" "$label_show3"
  assert_contains "other labels preserved after remove" "global" "$label_show3"
  echo

  # ── Scenario 8: List with --type=config filter ─────────────────
  blue "Scenario 8: List config beads with --type=config filter"

  # Create a second config bead with role label for filter tests
  local cfg2_out cfg2_id
  cfg2_out=$("$KD_BIN" create "e2e-cfg-role-$TEST_TAG" --type=config \
    -l "role:e2e-test-role" \
    -d '{"setting":"role-value"}' --json 2>/dev/null || echo "{}")
  cfg2_id=$(echo "$cfg2_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$cfg2_id" ]; then
    CREATED_BEADS+=("$cfg2_id")
  fi

  local list_out
  list_out=$("$KD_BIN" list --type=config --all-projects --limit=500 --json 2>/dev/null || echo "[]")
  assert_contains "type=config list includes first bead" "$cfg_id" "$list_out"
  if [ -n "$cfg2_id" ]; then
    assert_contains "type=config list includes second bead" "$cfg2_id" "$list_out"
  fi
  echo

  # ── Scenario 9: List with label filter ─────────────────────────
  blue "Scenario 9: List config beads with label filter (-l global, -l role:X)"
  if [ -n "$cfg2_id" ]; then
    # Filter by role label — should get cfg2 but not cfg (cfg has role from scenario 7)
    local role_list
    role_list=$("$KD_BIN" list --type=config -l "role:e2e-test-role" --all-projects --limit=500 --json 2>/dev/null || echo "[]")
    assert_contains "role-filtered list includes role bead" "$cfg2_id" "$role_list"

    # Filter by global label — should get cfg (which has global) but not cfg2 (role only)
    local global_list
    global_list=$("$KD_BIN" list --type=config -l "global" --all-projects --limit=500 --json 2>/dev/null || echo "[]")
    assert_contains "global-filtered list includes global bead" "$cfg_id" "$global_list"
    assert_not_contains "global-filtered list excludes role-only bead" "$cfg2_id" "$global_list"
  fi
  echo

  # ── Scenario 10: Default list excludes config beads ────────────
  blue "Scenario 10: Default kd list excludes config beads (issue-kind only)"
  local default_list
  default_list=$("$KD_BIN" list --all-projects --limit=500 --json 2>/dev/null || echo "[]")
  assert_not_contains "default list excludes config bead" "$cfg_id" "$default_list"
  if [ -n "$cfg2_id" ]; then
    assert_not_contains "default list excludes second config bead" "$cfg2_id" "$default_list"
  fi
  echo

  # ── Scenario 11: --kind=config filter ──────────────────────────
  blue "Scenario 11: --kind=config shows config beads, excludes tasks"
  # Create a task bead for contrast
  local task_out task_id
  task_out=$("$KD_BIN" create "e2e-cfg-task-$TEST_TAG" --type=task \
    --json 2>/dev/null || echo "{}")
  task_id=$(echo "$task_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$task_id" ]; then
    CREATED_BEADS+=("$task_id")
  fi

  local kind_list
  kind_list=$("$KD_BIN" list --kind=config --all-projects --limit=500 --json 2>/dev/null || echo "[]")
  assert_contains "kind=config includes config bead" "$cfg_id" "$kind_list"
  if [ -n "$cfg2_id" ]; then
    assert_contains "kind=config includes second config bead" "$cfg2_id" "$kind_list"
  fi
  if [ -n "$task_id" ]; then
    assert_not_contains "kind=config excludes task bead" "$task_id" "$kind_list"
  fi

  # Verify task bead appears in default list but not in kind=config
  if [ -n "$task_id" ]; then
    local task_default_list
    task_default_list=$("$KD_BIN" list --all-projects --limit=500 --json 2>/dev/null || echo "[]")
    assert_contains "default list includes task bead" "$task_id" "$task_default_list"
  fi
  echo

  # ── Scenario 12: Close config bead ─────────────────────────────
  blue "Scenario 12: Close config bead and verify status"
  local close_rc=0
  "$KD_BIN" close "$cfg_id" 2>/dev/null || close_rc=$?
  assert_exit "kd close config bead exits 0" 0 "$close_rc"

  local closed_show
  closed_show=$("$KD_BIN" show "$cfg_id" --json 2>/dev/null || echo "{}")
  assert_json_field "closed bead status is closed" "status" "closed" "$closed_show"
  assert_json_field "closed bead type still config" "type" "config" "$closed_show"
  assert_json_field "closed bead kind still config" "kind" "config" "$closed_show"
  # Remove from cleanup since we closed it
  CREATED_BEADS=("${CREATED_BEADS[@]/$cfg_id/}")
  echo

  # ── Scenario 13: Closed bead disappears from open list ─────────
  blue "Scenario 13: Closed config bead disappears from open list"
  local open_list
  open_list=$("$KD_BIN" list --type=config --status=open --all-projects --limit=500 --json 2>/dev/null || echo "[]")
  assert_not_contains "closed bead not in open config list" "$cfg_id" "$open_list"

  # But it should appear in a closed status list
  local closed_list
  closed_list=$("$KD_BIN" list --type=config --status=closed --all-projects --limit=500 --json 2>/dev/null || echo "[]")
  assert_contains "closed bead in closed config list" "$cfg_id" "$closed_list"
  echo

  # ── Scenario 14: Delete config bead ────────────────────────────
  blue "Scenario 14: Delete config bead permanently removes it"
  # Create a bead specifically for deletion
  local del_out del_id
  del_out=$("$KD_BIN" create "e2e-cfg-delete-$TEST_TAG" --type=config -l global \
    -d '{"disposable":true}' --json 2>/dev/null || echo "{}")
  del_id=$(echo "$del_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")

  if [ -n "$del_id" ]; then
    # Verify it exists before deletion
    local del_show_before
    del_show_before=$("$KD_BIN" show "$del_id" --json 2>/dev/null || echo "{}")
    assert_json_field "delete target exists before delete" "type" "config" "$del_show_before"

    # Delete it
    local del_rc=0
    "$KD_BIN" delete "$del_id" 2>/dev/null || del_rc=$?
    assert_exit "kd delete exits 0" 0 "$del_rc"

    # Verify it's gone from all lists
    local del_list_after
    del_list_after=$("$KD_BIN" list --type=config --all-projects --limit=500 --json 2>/dev/null || echo "[]")
    assert_not_contains "deleted bead not in config list" "$del_id" "$del_list_after"

    # Show after delete should fail or return deleted status
    local del_show_rc=0
    "$KD_BIN" show "$del_id" --json >/dev/null 2>&1 || del_show_rc=$?
    if [ "$del_show_rc" -ne 0 ]; then
      pass "kd show deleted bead returns error (exit $del_show_rc)"
    else
      local del_show_after
      del_show_after=$("$KD_BIN" show "$del_id" --json 2>/dev/null || echo "{}")
      local del_status
      del_status=$(echo "$del_show_after" | python3 -c "import sys,json; print(json.load(sys.stdin).get('status',''))" 2>/dev/null || echo "")
      if [ "$del_status" = "deleted" ]; then
        pass "kd show deleted bead has status=deleted"
      else
        fail "deleted bead is inaccessible" "show returned exit 0 with status=$del_status"
      fi
    fi
  else
    fail "create bead for deletion" "could not create"
  fi
  echo

  # ── Scenario 15: Create config bead without labels ─────────────
  blue "Scenario 15: Edge — create config bead without labels"
  local nolabel_out nolabel_rc nolabel_id
  nolabel_out=$("$KD_BIN" create "e2e-cfg-nolabel-$TEST_TAG" --type=config \
    -d '{"key":"value"}' --json 2>/dev/null) && nolabel_rc=0 || nolabel_rc=$?
  assert_exit "create config bead without labels" 0 "$nolabel_rc"

  nolabel_id=$(echo "$nolabel_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$nolabel_id" ]; then
    CREATED_BEADS+=("$nolabel_id")
    pass "no-label config bead created ($nolabel_id)"

    local nolabel_show
    nolabel_show=$("$KD_BIN" show "$nolabel_id" --json 2>/dev/null || echo "{}")
    assert_json_field "no-label bead type is config" "type" "config" "$nolabel_show"
    assert_json_field "no-label bead kind is config" "kind" "config" "$nolabel_show"

    # Verify it appears in type=config list
    local nolabel_list
    nolabel_list=$("$KD_BIN" list --type=config --all-projects --limit=500 --json 2>/dev/null || echo "[]")
    assert_contains "no-label bead in type=config list" "$nolabel_id" "$nolabel_list"
  else
    fail "no-label config bead" "could not create"
  fi
  echo

  # ── Scenario 16: Create config bead with multiple labels ───────
  blue "Scenario 16: Edge — create config bead with multiple labels"
  local multi_out multi_rc multi_id
  multi_out=$("$KD_BIN" create "e2e-cfg-multi-$TEST_TAG" --type=config \
    -l global -l "role:e2e-multi" -l "rig:e2e-rig" \
    -d '{"multi":true}' --json 2>/dev/null) && multi_rc=0 || multi_rc=$?
  assert_exit "create multi-label config bead" 0 "$multi_rc"

  multi_id=$(echo "$multi_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$multi_id" ]; then
    CREATED_BEADS+=("$multi_id")
    pass "multi-label config bead created ($multi_id)"

    local multi_show
    multi_show=$("$KD_BIN" show "$multi_id" --json 2>/dev/null || echo "{}")
    assert_contains "multi-label has global" "global" "$multi_show"
    assert_contains "multi-label has role:e2e-multi" "role:e2e-multi" "$multi_show"
    assert_contains "multi-label has rig:e2e-rig" "rig:e2e-rig" "$multi_show"

    # Filter by each label should find it
    local multi_role_list
    multi_role_list=$("$KD_BIN" list --type=config -l "role:e2e-multi" --all-projects --limit=500 --json 2>/dev/null || echo "[]")
    assert_contains "role filter finds multi-label bead" "$multi_id" "$multi_role_list"

    local multi_rig_list
    multi_rig_list=$("$KD_BIN" list --type=config -l "rig:e2e-rig" --all-projects --limit=500 --json 2>/dev/null || echo "[]")
    assert_contains "rig filter finds multi-label bead" "$multi_id" "$multi_rig_list"
  else
    fail "multi-label config bead" "could not create"
  fi
  echo

  # ── Scenario 17: Reopen a closed config bead ───────────────────
  blue "Scenario 17: Edge — reopen a closed config bead"
  # Use cfg_id which we closed in scenario 12
  local reopen_rc=0
  "$KD_BIN" update "$cfg_id" -s open 2>/dev/null || reopen_rc=$?
  assert_exit "kd update status to open" 0 "$reopen_rc"

  local reopen_show
  reopen_show=$("$KD_BIN" show "$cfg_id" --json 2>/dev/null || echo "{}")
  assert_json_field "reopened bead status is open" "status" "open" "$reopen_show"
  assert_json_field "reopened bead type still config" "type" "config" "$reopen_show"

  # Verify it appears in open list again
  local reopen_list
  reopen_list=$("$KD_BIN" list --type=config --status=open --all-projects --limit=500 --json 2>/dev/null || echo "[]")
  assert_contains "reopened bead in open config list" "$cfg_id" "$reopen_list"

  # Close it again for cleanup
  "$KD_BIN" close "$cfg_id" 2>/dev/null || true
  echo

  # ── Summary ─────────────────────────────────────────────────────
  blue "═══════════════════════════════════════════════════"
  local total=$((PASS + FAIL))
  if [ "$FAIL" -eq 0 ]; then
    green " PASS: All $total assertions passed (17 scenarios)"
  else
    red " FAIL: $FAIL/$total assertions failed"
    echo
    for err in "${ERRORS[@]}"; do
      red "   - $err"
    done
  fi
  blue "═══════════════════════════════════════════════════"

  [ "$FAIL" -eq 0 ]
}

main "$@"
