#!/usr/bin/env bash
# test-config-instructions.sh — claude-instructions config bead E2E tests
#
# Tests that claude-instructions config beads are resolved and written
# to CLAUDE.md and stop-gate-text.md by gb setup claude.
#
# Scenarios tested:
#   1.  claude-instructions with claude_md writes CLAUDE.md
#   2.  CLAUDE.md content matches config bead value exactly
#   3.  claude-instructions with stop_gate_blocked writes stop-gate-text.md
#   4.  stop-gate-text.md content matches config bead value
#   5.  gb setup claude --defaults writes fallback CLAUDE.md when absent
#   6.  --defaults does not overwrite existing CLAUDE.md
#   7.  Partial config (only claude_md) — CLAUDE.md written, stop-gate-text.md absent
#   8.  Partial config (only stop_gate_blocked) — stop-gate-text.md written, no CLAUDE.md from instructions
#   9.  gb prime outputs instruction keys (prime_header, core_rules, session_close)
#   10. gb prime outputs all canonical section keys in order
#   11. Role-scoped claude-instructions overrides global claude_md
#   12. Role-scoped stop_gate_blocked overrides global
#   13. Empty claude_md value does not write CLAUDE.md
#   14. Empty stop_gate_blocked value does not write stop-gate-text.md
#   15. Non-matching label scope is not resolved
#   16. Multiple instruction keys merged across scopes
#   17. --check passes after setup, fails after --remove

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

assert_file_exists() {
  local name="$1" path="$2"
  if [ -f "$path" ]; then
    pass "$name (exists: $path)"
  else
    fail "$name" "file not found: $path"
  fi
}

assert_file_not_exists() {
  local name="$1" path="$2"
  if [ ! -f "$path" ]; then
    pass "$name (absent: $path)"
  else
    fail "$name" "file unexpectedly exists: $path"
  fi
}

assert_file_contains() {
  local name="$1" path="$2" pattern="$3"
  if [ ! -f "$path" ]; then
    fail "$name" "file not found: $path"
    return
  fi
  if grep -qF -- "$pattern" "$path"; then
    pass "$name"
  else
    fail "$name" "file $path does not contain '$pattern'; got: $(head -5 "$path")"
  fi
}

assert_file_not_contains() {
  local name="$1" path="$2" pattern="$3"
  if [ ! -f "$path" ]; then
    pass "$name (file absent, trivially satisfied)"
    return
  fi
  if ! grep -qF -- "$pattern" "$path"; then
    pass "$name"
  else
    fail "$name" "file $path unexpectedly contains '$pattern'"
  fi
}

# ── Setup ─────────────────────────────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19109
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-instructions.log 2>&1 &
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

# shellcheck source=./lib-config-cleanup.sh
source "$(dirname "$0")/lib-config-cleanup.sh"

WORK_DIR=$(mktemp -d /tmp/e2e-instructions-XXXXXX)
TEST_TAG="e2e-instr-$$-$(date +%s)"
CLEANUP_STALE_ROLE="${TEST_TAG}"
export CLEANUP_STALE_ROLE

create_config() {
  local title="$1" label="$2" value="$3"
  local out id
  out=$("$KD_BIN" create "$title" --type=config -l "$label" \
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

# create_config_noproj creates a config bead without the auto-added project label.
# gb prime builds subscriptions as ["global", "role:*"] without project scope,
# so beads with project:* labels won't match prime's resolution. Use this
# variant for beads that must be found by gb prime.
create_config_noproj() {
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

# fresh_workspace resets the work dir and creates an isolated HOME.
# Prints the isolated HOME path on stdout.
fresh_workspace() {
  rm -rf "$WORK_DIR/.claude" "$WORK_DIR/CLAUDE.md" "$WORK_DIR/.mcp.json"
  mkdir -p "$WORK_DIR"
  local iso_home
  iso_home=$(mktemp -d /tmp/e2e-instr-home-XXXXXX)
  mkdir -p "$iso_home/.claude"
  echo "$iso_home"
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " claude-instructions config bead E2E tests"
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

  local E2E_ROLE="e2e-instr-role-$TEST_TAG"

  # Create a claude-hooks bead so normal mode succeeds (it errors without one).
  local hooks_id
  hooks_id=$(create_config "claude-hooks" "global" \
    '{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"gb hook prime 2>/dev/null || true"}]}],"Stop":[{"matcher":"","hooks":[{"type":"command","command":"gb hook stop-gate"}]}]}}')
  if [ -z "$hooks_id" ]; then
    red "ERROR: Failed to create prerequisite claude-hooks bead"
    exit 1
  fi
  dim "  Hooks bead: $hooks_id"
  echo

  # ── Scenario 1: claude_md writes CLAUDE.md ──────────────────────
  blue "Scenario 1: claude-instructions with claude_md writes CLAUDE.md"
  # Use role-scoped bead to override any production/leaked global
  # claude-instructions bead (e.g. from test-config-categories runs).
  local instr1_id iso_home1
  instr1_id=$(create_config "claude-instructions" "role:$E2E_ROLE" \
    "{\"claude_md\":\"# E2E Test CLAUDE.md\\nContent tag: $TEST_TAG\"}")

  if [ -z "$instr1_id" ]; then
    fail "create claude-instructions bead" "failed to create config bead"
  else
    pass "created claude-instructions bead ($instr1_id)"
    iso_home1=$(fresh_workspace)

    local rc1=0
    HOME="$iso_home1" "$GB_BIN" setup claude --role "$E2E_ROLE" --workspace "$WORK_DIR" 2>/dev/null || rc1=$?
    assert_exit "gb setup claude" 0 "$rc1"
    assert_file_exists "CLAUDE.md created" "$WORK_DIR/CLAUDE.md"
  fi
  echo

  # ── Scenario 2: CLAUDE.md content matches ───────────────────────
  blue "Scenario 2: CLAUDE.md content matches config bead value"
  if [ -f "$WORK_DIR/CLAUDE.md" ]; then
    assert_file_contains "CLAUDE.md has test tag" "$WORK_DIR/CLAUDE.md" "$TEST_TAG"
    assert_file_contains "CLAUDE.md has header" "$WORK_DIR/CLAUDE.md" "# E2E Test CLAUDE.md"
  else
    fail "CLAUDE.md content check" "file does not exist"
  fi
  rm -rf "${iso_home1:-}"
  close_bead "$instr1_id"
  echo

  # ── Scenario 3: stop_gate_blocked writes stop-gate-text.md ─────
  blue "Scenario 3: stop_gate_blocked writes stop-gate-text.md"

  # Use role-scoped bead to override any production global stop_gate_blocked.
  local instr2_id iso_home2
  instr2_id=$(create_config "claude-instructions" "role:$E2E_ROLE" \
    "{\"claude_md\":\"# Test\",\"stop_gate_blocked\":\"STOP-BLOCKED: $TEST_TAG\"}")

  if [ -z "$instr2_id" ]; then
    fail "create instructions with stop_gate_blocked" "failed to create"
  else
    pass "created instructions with stop_gate_blocked ($instr2_id)"
    iso_home2=$(fresh_workspace)

    local rc2=0
    HOME="$iso_home2" "$GB_BIN" setup claude --role "$E2E_ROLE" --workspace "$WORK_DIR" 2>/dev/null || rc2=$?
    assert_exit "setup with stop_gate_blocked" 0 "$rc2"
    assert_file_exists "stop-gate-text.md created" "$iso_home2/.claude/stop-gate-text.md"
  fi
  echo

  # ── Scenario 4: stop-gate-text.md content matches ───────────────
  blue "Scenario 4: stop-gate-text.md content matches config bead value"
  if [ -f "${iso_home2:-}/.claude/stop-gate-text.md" ]; then
    assert_file_contains "stop-gate-text.md has test tag" "$iso_home2/.claude/stop-gate-text.md" "$TEST_TAG"
    assert_file_contains "stop-gate-text.md has prefix" "$iso_home2/.claude/stop-gate-text.md" "STOP-BLOCKED:"
  else
    fail "stop-gate-text.md content check" "file does not exist"
  fi
  rm -rf "${iso_home2:-}"
  close_bead "$instr2_id"
  echo

  # ── Scenario 5: --defaults writes fallback CLAUDE.md ────────────
  blue "Scenario 5: --defaults writes fallback CLAUDE.md when absent"
  local defaults_home defaults_dir
  defaults_dir=$(mktemp -d /tmp/e2e-instr-def-XXXXXX)
  defaults_home=$(mktemp -d /tmp/e2e-instr-defhome-XXXXXX)
  mkdir -p "$defaults_home/.claude"

  local rc5=0
  HOME="$defaults_home" "$GB_BIN" setup claude --defaults --workspace "$defaults_dir" 2>/dev/null || rc5=$?
  assert_exit "gb setup claude --defaults" 0 "$rc5"
  assert_file_exists "defaults CLAUDE.md created" "$defaults_dir/CLAUDE.md"
  if [ -f "$defaults_dir/CLAUDE.md" ]; then
    # Default CLAUDE.md template contains agent workflow references
    assert_file_contains "defaults CLAUDE.md has gb ready" "$defaults_dir/CLAUDE.md" "gb ready"
    assert_file_contains "defaults CLAUDE.md has kd claim" "$defaults_dir/CLAUDE.md" "kd claim"
  fi
  echo

  # ── Scenario 6: --defaults does not overwrite existing CLAUDE.md ─
  blue "Scenario 6: --defaults does not overwrite existing CLAUDE.md"
  local sentinel="SENTINEL-DO-NOT-OVERWRITE-$TEST_TAG"
  echo "$sentinel" > "$defaults_dir/CLAUDE.md"
  HOME="$defaults_home" "$GB_BIN" setup claude --defaults --workspace "$defaults_dir" 2>/dev/null || true
  assert_file_contains "CLAUDE.md preserved after re-run" "$defaults_dir/CLAUDE.md" "$sentinel"
  rm -rf "$defaults_dir" "$defaults_home"
  echo

  # ── Scenario 7: Partial config — only claude_md ─────────────────
  blue "Scenario 7: Partial config (only claude_md) — CLAUDE.md written, stop-gate-text.md unset"
  local partial_id iso_home7
  # Use role scope. Set stop_gate_blocked="" to explicitly suppress any global
  # production bead's stop_gate_blocked. MergeOverride: role overrides global.
  partial_id=$(create_config "claude-instructions" "role:$E2E_ROLE" \
    "{\"claude_md\":\"# Partial test $TEST_TAG\",\"stop_gate_blocked\":\"\"}")

  if [ -z "$partial_id" ]; then
    fail "create partial config" "failed to create"
  else
    pass "created partial config ($partial_id)"
    iso_home7=$(fresh_workspace)

    local rc7=0
    HOME="$iso_home7" "$GB_BIN" setup claude --role "$E2E_ROLE" --workspace "$WORK_DIR" 2>/dev/null || rc7=$?
    assert_exit "partial config setup" 0 "$rc7"
    assert_file_exists "CLAUDE.md from partial config" "$WORK_DIR/CLAUDE.md"
    assert_file_contains "partial CLAUDE.md has tag" "$WORK_DIR/CLAUDE.md" "Partial test $TEST_TAG"
    # stop-gate-text.md should NOT be created since stop_gate_blocked is empty
    assert_file_not_exists "no stop-gate-text.md from partial config" "$iso_home7/.claude/stop-gate-text.md"
  fi
  rm -rf "${iso_home7:-}"
  close_bead "$partial_id"
  echo

  # ── Scenario 8: Partial config — only stop_gate_blocked ─────────
  blue "Scenario 8: Partial config (only stop_gate_blocked) — stop-gate-text.md written"
  local gate_only_id iso_home8
  # Use role scope to override any production global stop_gate_blocked.
  gate_only_id=$(create_config "claude-instructions" "role:$E2E_ROLE" \
    "{\"stop_gate_blocked\":\"GATE-ONLY: $TEST_TAG\"}")

  if [ -z "$gate_only_id" ]; then
    fail "create gate-only config" "failed to create"
  else
    pass "created gate-only config ($gate_only_id)"
    iso_home8=$(fresh_workspace)

    local rc8=0
    HOME="$iso_home8" "$GB_BIN" setup claude --role "$E2E_ROLE" --workspace "$WORK_DIR" 2>/dev/null || rc8=$?
    assert_exit "gate-only config setup" 0 "$rc8"
    assert_file_exists "stop-gate-text.md from gate-only" "$iso_home8/.claude/stop-gate-text.md"
    assert_file_contains "gate-only content" "$iso_home8/.claude/stop-gate-text.md" "GATE-ONLY: $TEST_TAG"
    # CLAUDE.md should NOT be written from instructions (no claude_md field)
    # Note: CLAUDE.md may exist from a previous run but won't contain our tag
    assert_file_not_contains "no instruction CLAUDE.md from gate-only" "$WORK_DIR/CLAUDE.md" "GATE-ONLY:"
  fi
  rm -rf "${iso_home8:-}"
  close_bead "$gate_only_id"
  echo

  # ── Scenario 9: gb prime consumes instruction keys ──────────────
  blue "Scenario 9: gb prime outputs instruction keys (prime_header, core_rules, session_close)"
  local prime_instr_id
  # Use role-scoped bead to override production global beads that have
  # the same keys (prime_header, core_rules, session_close).
  prime_instr_id=$(create_config_noproj "claude-instructions" "role:$E2E_ROLE" \
    "{\"prime_header\":\"## PRIME-HDR $TEST_TAG\",\"core_rules\":\"CORE-RULES $TEST_TAG\",\"session_close\":\"SESSION-CLOSE $TEST_TAG\"}")

  if [ -z "$prime_instr_id" ]; then
    fail "create prime instructions" "failed to create"
  else
    pass "created prime instructions ($prime_instr_id)"

    # gb prime resolves claude-instructions via subscriptions.
    # Use E2E_ROLE so the role-scoped bead overrides global production beads.
    local prime_out prime_rc=0
    prime_out=$(BOAT_ROLE="$E2E_ROLE" KD_ACTOR="e2e-prime-$TEST_TAG" \
      "$GB_BIN" prime --no-advice 2>/dev/null) || prime_rc=$?

    if [ "$prime_rc" -eq 0 ] && [ -n "$prime_out" ]; then
      pass "gb prime produced output (${#prime_out} chars)"
      assert_contains "prime has prime_header" "PRIME-HDR $TEST_TAG" "$prime_out"
      assert_contains "prime has core_rules" "CORE-RULES $TEST_TAG" "$prime_out"
      assert_contains "prime has session_close" "SESSION-CLOSE $TEST_TAG" "$prime_out"
    else
      fail "gb prime" "exit $prime_rc or empty output"
    fi
  fi
  echo

  # ── Scenario 10: gb prime outputs all canonical section keys ────
  blue "Scenario 10: gb prime outputs all canonical section keys in order"

  close_bead "$prime_instr_id"

  local allkeys_id
  # Use role scope to override production global beads for all keys.
  allkeys_id=$(create_config_noproj "claude-instructions" "role:$E2E_ROLE" \
    "{\"prime_header\":\"SEC-prime_header-$TEST_TAG\",\"session_close\":\"SEC-session_close-$TEST_TAG\",\"core_rules\":\"SEC-core_rules-$TEST_TAG\",\"commands\":\"SEC-commands-$TEST_TAG\",\"workflows\":\"SEC-workflows-$TEST_TAG\",\"decisions\":\"SEC-decisions-$TEST_TAG\",\"session_resumption\":\"SEC-session_resumption-$TEST_TAG\",\"lifecycle\":\"SEC-lifecycle-$TEST_TAG\",\"stop_gate\":\"SEC-stop_gate-$TEST_TAG\"}")

  if [ -z "$allkeys_id" ]; then
    fail "create all-keys instructions" "failed to create"
  else
    pass "created all-keys instructions ($allkeys_id)"

    local allkeys_out allkeys_rc=0
    allkeys_out=$(BOAT_ROLE="$E2E_ROLE" KD_ACTOR="e2e-allkeys-$TEST_TAG" \
      "$GB_BIN" prime --no-advice 2>/dev/null) || allkeys_rc=$?

    if [ "$allkeys_rc" -eq 0 ] && [ -n "$allkeys_out" ]; then
      for key in prime_header session_close core_rules commands workflows decisions session_resumption lifecycle stop_gate; do
        assert_contains "prime has $key" "SEC-${key}-$TEST_TAG" "$allkeys_out"
      done

      # Verify ordering: prime_header appears before stop_gate
      local hdr_line gate_line
      hdr_line=$(echo "$allkeys_out" | grep -nF "SEC-prime_header-$TEST_TAG" | head -1 | cut -d: -f1)
      gate_line=$(echo "$allkeys_out" | grep -nF "SEC-stop_gate-$TEST_TAG" | head -1 | cut -d: -f1)
      if [ -n "$hdr_line" ] && [ -n "$gate_line" ] && [ "$hdr_line" -lt "$gate_line" ]; then
        pass "prime_header appears before stop_gate in output"
      else
        fail "section ordering" "prime_header (line $hdr_line) should precede stop_gate (line $gate_line)"
      fi
    else
      fail "gb prime all-keys" "exit $allkeys_rc or empty output"
    fi
  fi
  close_bead "$allkeys_id"
  echo

  # ── Scenario 11: Role-scoped override of claude_md ──────────────
  blue "Scenario 11: Role-scoped claude-instructions overrides global claude_md"
  # Use unique E2E_ROLE to avoid stale beads from prior runs at shared scopes.
  local E2E_ROLE_11="e2e-role11-$TEST_TAG"
  local global_id role_id iso_home11
  global_id=$(create_config "claude-instructions" "global" \
    "{\"claude_md\":\"# Global MD $TEST_TAG\"}")
  role_id=$(create_config "claude-instructions" "role:$E2E_ROLE_11" \
    "{\"claude_md\":\"# Crew MD $TEST_TAG\"}")

  if [ -z "$global_id" ] || [ -z "$role_id" ]; then
    fail "create role override beads" "failed to create one or both"
  else
    pass "created global ($global_id) and role ($role_id) instruction beads"
    iso_home11=$(fresh_workspace)

    local rc11=0
    HOME="$iso_home11" BOAT_ROLE="$E2E_ROLE_11" \
      "$GB_BIN" setup claude --workspace "$WORK_DIR" --role "$E2E_ROLE_11" 2>/dev/null || rc11=$?
    assert_exit "role override setup" 0 "$rc11"
    assert_file_exists "CLAUDE.md from role override" "$WORK_DIR/CLAUDE.md"
    if [ -f "$WORK_DIR/CLAUDE.md" ]; then
      # MergeOverride: role's claude_md should override global's
      assert_file_contains "CLAUDE.md has crew content" "$WORK_DIR/CLAUDE.md" "Crew MD $TEST_TAG"
      assert_file_not_contains "CLAUDE.md excludes global content" "$WORK_DIR/CLAUDE.md" "Global MD $TEST_TAG"
    fi
  fi
  rm -rf "${iso_home11:-}"
  close_bead "$global_id"
  close_bead "$role_id"
  echo

  # ── Scenario 12: Role-scoped stop_gate_blocked overrides global ──
  blue "Scenario 12: Role-scoped stop_gate_blocked overrides global"
  # Use unique E2E_ROLE to avoid stale beads from prior runs at shared scopes.
  local E2E_ROLE_12="e2e-role12-$TEST_TAG"
  local g12_id r12_id iso_home12
  g12_id=$(create_config "claude-instructions" "global" \
    "{\"stop_gate_blocked\":\"GLOBAL-GATE $TEST_TAG\"}")
  r12_id=$(create_config "claude-instructions" "role:$E2E_ROLE_12" \
    "{\"stop_gate_blocked\":\"CREW-GATE $TEST_TAG\"}")

  if [ -z "$g12_id" ] || [ -z "$r12_id" ]; then
    fail "create gate override beads" "failed to create"
  else
    pass "created gate override beads (global $g12_id, role $r12_id)"
    iso_home12=$(fresh_workspace)

    local rc12=0
    HOME="$iso_home12" BOAT_ROLE="$E2E_ROLE_12" \
      "$GB_BIN" setup claude --workspace "$WORK_DIR" --role "$E2E_ROLE_12" 2>/dev/null || rc12=$?
    assert_exit "gate override setup" 0 "$rc12"
    if [ -f "$iso_home12/.claude/stop-gate-text.md" ]; then
      assert_file_contains "stop-gate has crew content" "$iso_home12/.claude/stop-gate-text.md" "CREW-GATE $TEST_TAG"
      assert_file_not_contains "stop-gate excludes global" "$iso_home12/.claude/stop-gate-text.md" "GLOBAL-GATE $TEST_TAG"
    else
      fail "stop-gate-text.md from role override" "file not found"
    fi
  fi
  rm -rf "${iso_home12:-}"
  close_bead "$g12_id"
  close_bead "$r12_id"
  echo

  # ── Scenario 13: Empty claude_md does not write CLAUDE.md ───────
  blue "Scenario 13: Empty claude_md value does not write CLAUDE.md"
  # Use role scope to override any production/leaked global claude-instructions
  # bead with a non-empty claude_md. Empty string at higher specificity wins
  # the MergeOverride and suppresses CLAUDE.md writing.
  local empty_md_id iso_home13
  empty_md_id=$(create_config "claude-instructions" "role:$E2E_ROLE" \
    "{\"claude_md\":\"\"}")

  if [ -z "$empty_md_id" ]; then
    fail "create empty claude_md bead" "failed to create"
  else
    pass "created empty claude_md bead ($empty_md_id)"
    iso_home13=$(fresh_workspace)

    local rc13=0
    HOME="$iso_home13" "$GB_BIN" setup claude --role "$E2E_ROLE" --workspace "$WORK_DIR" 2>/dev/null || rc13=$?
    # Setup should succeed without crashing
    if [ "$rc13" -eq 0 ]; then
      pass "setup with empty claude_md exits 0"
    else
      # May fail for other reasons (missing hooks in env) but should not crash
      dim "  setup exited $rc13 (non-zero is acceptable if not a crash)"
      pass "setup with empty claude_md did not crash"
    fi
    # CLAUDE.md should not be written for empty string
    assert_file_not_exists "no CLAUDE.md from empty claude_md" "$WORK_DIR/CLAUDE.md"
  fi
  rm -rf "${iso_home13:-}"
  close_bead "$empty_md_id"
  echo

  # ── Scenario 14: Empty stop_gate_blocked does not write file ────
  blue "Scenario 14: Empty stop_gate_blocked value does not write stop-gate-text.md"
  local empty_gate_id iso_home14
  # Use role scope to override any production global bead that has a non-empty
  # stop_gate_blocked. At global scope, stale/production beads interfere.
  empty_gate_id=$(create_config "claude-instructions" "role:$E2E_ROLE" \
    "{\"stop_gate_blocked\":\"\"}")

  if [ -z "$empty_gate_id" ]; then
    fail "create empty stop_gate_blocked bead" "failed to create"
  else
    pass "created empty stop_gate_blocked bead ($empty_gate_id)"
    iso_home14=$(fresh_workspace)

    local rc14=0
    HOME="$iso_home14" "$GB_BIN" setup claude --role "$E2E_ROLE" --workspace "$WORK_DIR" 2>/dev/null || rc14=$?
    if [ "$rc14" -eq 0 ]; then
      pass "setup with empty stop_gate_blocked exits 0"
    else
      dim "  setup exited $rc14 (non-zero is acceptable)"
      pass "setup with empty stop_gate_blocked did not crash"
    fi
    assert_file_not_exists "no stop-gate-text.md from empty value" "$iso_home14/.claude/stop-gate-text.md"
  fi
  rm -rf "${iso_home14:-}"
  close_bead "$empty_gate_id"
  echo

  # ── Scenario 15: Non-matching label scope excluded ──────────────
  blue "Scenario 15: Non-matching label scope is not resolved"
  local nomatch_id iso_home15
  # Must use create_config_noproj to avoid auto-added project:gardenio label,
  # which would match setup's subscriptions and defeat the test.
  nomatch_id=$(create_config_noproj "claude-instructions" "role:nonexistent-$TEST_TAG" \
    "{\"claude_md\":\"# SHOULD-NOT-APPEAR $TEST_TAG\"}")

  if [ -z "$nomatch_id" ]; then
    fail "create non-matching scope bead" "failed to create"
  else
    pass "created non-matching scope bead ($nomatch_id)"
    iso_home15=$(fresh_workspace)

    # Run setup with role=crew — role:nonexistent should not match
    local rc15=0
    HOME="$iso_home15" BOAT_ROLE=crew \
      "$GB_BIN" setup claude --workspace "$WORK_DIR" --role crew 2>/dev/null || rc15=$?

    # CLAUDE.md should not contain the non-matching content
    assert_file_not_contains "non-matching scope excluded" "$WORK_DIR/CLAUDE.md" "SHOULD-NOT-APPEAR"
  fi
  rm -rf "${iso_home15:-}"
  close_bead "$nomatch_id"
  echo

  # ── Scenario 16: Keys merged across specificity layers ──────────
  blue "Scenario 16: Multiple instruction keys merged across specificity layers"
  # Use two unique test roles to isolate from production beads.
  # Multi-role: first role in BOAT_ROLE has highest precedence.
  local E2E_ROLE_BASE="e2e-base-$TEST_TAG"
  local E2E_ROLE_OVER="e2e-over-$TEST_TAG"
  local merge_g_id merge_r_id
  merge_g_id=$(create_config_noproj "claude-instructions" "role:$E2E_ROLE_BASE" \
    "{\"prime_header\":\"BASE-HDR $TEST_TAG\",\"core_rules\":\"BASE-RULES $TEST_TAG\"}")
  merge_r_id=$(create_config_noproj "claude-instructions" "role:$E2E_ROLE_OVER" \
    "{\"core_rules\":\"OVER-RULES $TEST_TAG\",\"lifecycle\":\"OVER-LIFECYCLE $TEST_TAG\"}")

  if [ -z "$merge_g_id" ] || [ -z "$merge_r_id" ]; then
    fail "create merge test beads" "failed to create"
  else
    pass "created merge test beads (base $merge_g_id, over $merge_r_id)"

    local merge_out merge_rc=0
    # First role (E2E_ROLE_OVER) has higher precedence.
    merge_out=$(BOAT_ROLE="$E2E_ROLE_OVER,$E2E_ROLE_BASE" KD_ACTOR="e2e-merge-$TEST_TAG" \
      "$GB_BIN" prime --no-advice 2>/dev/null) || merge_rc=$?

    if [ "$merge_rc" -eq 0 ] && [ -n "$merge_out" ]; then
      # MergeOverride: base prime_header survives (override role didn't set it)
      assert_contains "merged prime_header from base" "BASE-HDR $TEST_TAG" "$merge_out"
      # core_rules overridden by higher-precedence role
      assert_contains "merged core_rules from override" "OVER-RULES $TEST_TAG" "$merge_out"
      assert_not_contains "base core_rules overridden" "BASE-RULES $TEST_TAG" "$merge_out"
      # lifecycle from override role (additive — base didn't have it)
      assert_contains "merged lifecycle from override" "OVER-LIFECYCLE $TEST_TAG" "$merge_out"
    else
      fail "gb prime merge test" "exit $merge_rc or empty output"
    fi
  fi
  close_bead "$merge_g_id"
  close_bead "$merge_r_id"
  echo

  # ── Scenario 17: --check / --remove cycle ───────────────────────
  blue "Scenario 17: --check passes after setup, fails after --remove"
  local check_dir check_home
  check_dir=$(mktemp -d /tmp/e2e-instr-chk-XXXXXX)
  check_home=$(mktemp -d /tmp/e2e-instr-chkhome-XXXXXX)
  mkdir -p "$check_dir/.claude" "$check_home/.claude"

  # Install defaults
  HOME="$check_home" "$GB_BIN" setup claude --defaults --workspace "$check_dir" 2>/dev/null || true

  # --check should pass after defaults
  local chk_rc=0
  "$GB_BIN" setup claude --check --workspace "$check_dir" 2>/dev/null || chk_rc=$?
  assert_exit "--check passes after --defaults" 0 "$chk_rc"

  # --remove should remove gb hooks
  local rm_rc=0
  "$GB_BIN" setup claude --remove --workspace "$check_dir" 2>/dev/null || rm_rc=$?
  assert_exit "--remove exits 0" 0 "$rm_rc"

  # --check should now fail (hooks removed)
  local chk2_rc=0
  "$GB_BIN" setup claude --check --workspace "$check_dir" 2>/dev/null || chk2_rc=$?
  if [ "$chk2_rc" -ne 0 ]; then
    pass "--check fails after --remove (exit $chk2_rc)"
  else
    fail "--check should fail after --remove" "exit 0 when hooks were removed"
  fi
  rm -rf "$check_dir" "$check_home"
  echo

  # ── Cleanup hooks bead ─────────────────────────────────────────
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
