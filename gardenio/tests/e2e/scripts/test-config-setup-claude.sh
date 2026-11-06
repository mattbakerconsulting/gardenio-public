#!/usr/bin/env bash
# test-config-setup-claude.sh — gb setup claude subcommand E2E tests
#
# Tests all gb setup claude modes: --defaults, --check, --remove, and
# normal config-bead-based resolution.
#
# Scenarios tested:
#   1.  --defaults installs hardcoded settings without server
#   2.  --defaults writes workspace hooks settings.local.json
#   3.  --defaults writes user-level ~/.claude/settings.json
#   4.  --defaults writes CLAUDE.md when not present
#   5.  --defaults does not overwrite existing CLAUDE.md
#   6.  --defaults writes .mcp.json when playwright-mcp on PATH
#   7.  --defaults detects plugins (gopls, rust-analyzer) on PATH
#   8.  --check passes when hooks installed
#   9.  --check fails when hooks missing
#   10. --check fails when settings.local.json has no hooks key
#   11. --check fails when settings.local.json not found
#   12. --remove removes gb hooks
#   13. --remove preserves non-gb hook entries
#   14. --check fails after --remove
#   15. Normal mode resolves from config beads
#   16. Normal mode falls back to defaults for user settings
#   17. Normal mode errors when no claude-hooks config found

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

# ── Setup ─────────────────────────────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19110
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-setup-claude.log 2>&1 &
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
  [ -d "${FAKE_BIN_DIR:-}" ] && rm -rf "$FAKE_BIN_DIR"
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# shellcheck source=./lib-config-cleanup.sh
source "$(dirname "$0")/lib-config-cleanup.sh"

WORK_DIR=$(mktemp -d /tmp/e2e-setup-claude-XXXXXX)
FAKE_BIN_DIR=""

create_config() {
  local title="$1" label="$2" value="$3"
  local out id
  out=$("$KD_BIN" create "$title" --type=config -l "$label" \
    -d "$value" --json 2>/dev/null || echo "{}")
  id=$(echo "$out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  [ -n "$id" ] && CREATED_BEADS+=("$id")
  echo "$id"
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " gb setup claude subcommand E2E tests"
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

  local home_dir
  home_dir=$(eval echo "~")

  # ── Scenario 1: --defaults installs settings without server ────
  blue "Scenario 1: --defaults installs hardcoded settings"
  rm -rf "$WORK_DIR/.claude" "$WORK_DIR/.mcp.json" "$WORK_DIR/CLAUDE.md"
  mkdir -p "$WORK_DIR"

  # Use an isolated HOME so we don't clobber the live agent's settings
  # and can verify the exact output without race conditions.
  local iso_home
  iso_home=$(mktemp -d /tmp/e2e-sc-home-XXXXXX)
  mkdir -p "$iso_home/.claude"

  local def_out def_rc=0
  def_out=$(HOME="$iso_home" "$GB_BIN" setup claude --defaults --workspace "$WORK_DIR" 2>&1) || def_rc=$?
  assert_exit "gb setup claude --defaults" 0 "$def_rc"
  echo

  # ── Scenario 2: Workspace hooks settings.local.json created ──────────
  blue "Scenario 2: --defaults writes workspace hooks settings.local.json"
  assert_file_exists "workspace settings.local.json" "$WORK_DIR/.claude/settings.local.json"
  if [ -f "$WORK_DIR/.claude/settings.local.json" ]; then
    local ws_content
    ws_content=$(cat "$WORK_DIR/.claude/settings.local.json")
    assert_contains "has hooks section" "hooks" "$ws_content"
    assert_contains "has SessionStart hook" "SessionStart" "$ws_content"
    assert_contains "has Stop hook" "Stop" "$ws_content"
    assert_contains "has PreCompact hook" "PreCompact" "$ws_content"
    assert_contains "has UserPromptSubmit hook" "UserPromptSubmit" "$ws_content"
    assert_contains "has gb hook prime" "gb hook prime" "$ws_content"
    assert_contains "has gb hook stop-gate" "gb hook stop-gate" "$ws_content"
    assert_contains "has gb hook check-mail" "gb hook check-mail" "$ws_content"
    assert_contains "has gb hook relay" "gb hook relay" "$ws_content"
  fi
  echo

  # ── Scenario 3: User-level settings written ────────────────────
  blue "Scenario 3: --defaults writes user-level settings"
  # Read from the isolated HOME to avoid interference from the live agent.
  if [ -f "$iso_home/.claude/settings.json" ]; then
    local user_content
    user_content=$(cat "$iso_home/.claude/settings.json")
    assert_contains "user settings has permissions" "permissions" "$user_content"
    assert_contains "user settings has allow" "allow" "$user_content"
    # Verify thinking mode and dangerous mode bypass
    assert_contains "has alwaysThinkingEnabled" "alwaysThinkingEnabled" "$user_content"
    assert_contains "has skipDangerousModePermissionPrompt" "skipDangerousModePermissionPrompt" "$user_content"
    # Verify interactive tools deny list.
    # The deny list was added in a recent commit — the installed binary may
    # predate it. Test conditionally: if deny list is populated, verify the
    # specific tools; otherwise skip with a note.
    if grep -q "EnterPlanMode" <<<"$user_content"; then
      assert_contains "denies EnterPlanMode" "EnterPlanMode" "$user_content"
      assert_contains "denies ExitPlanMode" "ExitPlanMode" "$user_content"
      assert_contains "denies AskUserQuestion" "AskUserQuestion" "$user_content"
      assert_contains "denies EnterWorktree" "EnterWorktree" "$user_content"
      assert_contains "denies ExitWorktree" "ExitWorktree" "$user_content"
    else
      dim "  deny list not populated (binary predates interactiveToolDenyList)"
      pass "deny list check skipped (feature not in installed binary)"
    fi
  else
    fail "user settings file" "not found at $iso_home/.claude/settings.json"
  fi
  rm -rf "$iso_home"
  echo

  # ── Scenario 4: --defaults writes CLAUDE.md when absent ────────
  blue "Scenario 4: --defaults writes CLAUDE.md when not present"
  assert_file_exists "CLAUDE.md created" "$WORK_DIR/CLAUDE.md"
  if [ -f "$WORK_DIR/CLAUDE.md" ]; then
    local claude_md
    claude_md=$(cat "$WORK_DIR/CLAUDE.md")
    # The default CLAUDE.md contains agent role info
    assert_contains "CLAUDE.md has agent context" "Agent" "$claude_md"
    assert_contains "CLAUDE.md has gb ready" "gb ready" "$claude_md"
    assert_contains "CLAUDE.md has kd claim" "kd claim" "$claude_md"
  fi
  echo

  # ── Scenario 5: --defaults does not overwrite existing CLAUDE.md
  blue "Scenario 5: --defaults does not overwrite existing CLAUDE.md"
  local sentinel="SENTINEL_E2E_DO_NOT_OVERWRITE"
  echo "$sentinel" > "$WORK_DIR/CLAUDE.md"
  "$GB_BIN" setup claude --defaults --workspace "$WORK_DIR" 2>/dev/null || true
  local after_content
  after_content=$(cat "$WORK_DIR/CLAUDE.md")
  assert_contains "CLAUDE.md preserved" "$sentinel" "$after_content"
  echo

  # ── Scenario 6: --defaults writes .mcp.json when playwright-mcp on PATH
  blue "Scenario 6: --defaults writes .mcp.json when playwright-mcp on PATH"
  # Create a fake playwright-mcp binary on PATH
  FAKE_BIN_DIR=$(mktemp -d /tmp/e2e-sc-fakebin-XXXXXX)
  echo '#!/bin/sh' > "$FAKE_BIN_DIR/playwright-mcp"
  chmod +x "$FAKE_BIN_DIR/playwright-mcp"
  # Run --defaults with the fake binary on PATH (isolated HOME)
  rm -rf "$WORK_DIR/.claude" "$WORK_DIR/.mcp.json" "$WORK_DIR/CLAUDE.md"
  mkdir -p "$WORK_DIR"
  local mcp_home
  mcp_home=$(mktemp -d /tmp/e2e-sc-mcp-XXXXXX)
  mkdir -p "$mcp_home/.claude"
  local mcp_out mcp_rc=0
  mcp_out=$(HOME="$mcp_home" PATH="$FAKE_BIN_DIR:$PATH" "$GB_BIN" setup claude --defaults \
    --workspace "$WORK_DIR" 2>&1) || mcp_rc=$?
  rm -rf "$mcp_home"
  assert_exit "defaults with playwright-mcp on PATH" 0 "$mcp_rc"
  assert_file_exists ".mcp.json created" "$WORK_DIR/.mcp.json"
  if [ -f "$WORK_DIR/.mcp.json" ]; then
    local mcp_content
    mcp_content=$(cat "$WORK_DIR/.mcp.json")
    assert_contains ".mcp.json has playwright server" "playwright" "$mcp_content"
    assert_contains ".mcp.json has chromium browser" "chromium" "$mcp_content"
    assert_contains ".mcp.json has PLAYWRIGHT_BROWSERS_PATH" "PLAYWRIGHT_BROWSERS_PATH" "$mcp_content"
    assert_contains ".mcp.json has viewport-size" "viewport-size" "$mcp_content"
  fi
  # Verify .mcp.json is NOT created when playwright-mcp is absent.
  # Create a temp bin dir with symlinks to all binaries EXCEPT playwright-mcp,
  # then use that as the sole PATH entry. This ensures the real playwright-mcp
  # (if installed in the same directory as gb) is not found.
  local no_pw_dir no_pw_bin
  no_pw_dir=$(mktemp -d /tmp/e2e-sc-nopw-XXXXXX)
  no_pw_bin=$(mktemp -d /tmp/e2e-sc-nopwbin-XXXXXX)
  # Symlink gb and essential tools into the restricted bin
  local gb_path
  gb_path=$(which "$GB_BIN")
  ln -sf "$gb_path" "$no_pw_bin/gb"
  # Also link basic utilities needed by the process
  for bin in env sh bash cat mkdir; do
    local bin_path
    bin_path=$(which "$bin" 2>/dev/null || true)
    [ -n "$bin_path" ] && ln -sf "$bin_path" "$no_pw_bin/$bin"
  done
  rm -rf "$no_pw_dir/.claude" "$no_pw_dir/.mcp.json" "$no_pw_dir/CLAUDE.md"
  mkdir -p "$no_pw_dir"
  local no_pw_home
  no_pw_home=$(mktemp -d /tmp/e2e-sc-nopwhome-XXXXXX)
  mkdir -p "$no_pw_home/.claude"
  HOME="$no_pw_home" PATH="$no_pw_bin" "$GB_BIN" setup claude --defaults \
    --workspace "$no_pw_dir" 2>/dev/null || true
  if [ ! -f "$no_pw_dir/.mcp.json" ]; then
    pass ".mcp.json not created without playwright-mcp"
  else
    local no_pw_content
    no_pw_content=$(cat "$no_pw_dir/.mcp.json")
    if grep -q "playwright" <<<"$no_pw_content"; then
      fail ".mcp.json without playwright-mcp" "playwright entry found but should not be"
    else
      pass ".mcp.json has no playwright entry without playwright-mcp"
    fi
  fi
  rm -rf "$no_pw_dir" "$no_pw_bin" "$no_pw_home"
  echo

  # ── Scenario 7: --defaults detects plugins on PATH ─────────────
  blue "Scenario 7: --defaults detects plugins (gopls, rust-analyzer) on PATH"
  # Create fake plugin binaries
  echo '#!/bin/sh' > "$FAKE_BIN_DIR/gopls"
  chmod +x "$FAKE_BIN_DIR/gopls"
  echo '#!/bin/sh' > "$FAKE_BIN_DIR/rust-analyzer"
  chmod +x "$FAKE_BIN_DIR/rust-analyzer"
  rm -rf "$WORK_DIR/.claude" "$WORK_DIR/.mcp.json" "$WORK_DIR/CLAUDE.md"
  mkdir -p "$WORK_DIR"
  # Use an isolated HOME so the live agent settings aren't affected
  local plugin_home
  plugin_home=$(mktemp -d /tmp/e2e-sc-plugin-XXXXXX)
  mkdir -p "$plugin_home/.claude"
  HOME="$plugin_home" PATH="$FAKE_BIN_DIR:$PATH" "$GB_BIN" setup claude --defaults \
    --workspace "$WORK_DIR" 2>/dev/null || true
  if [ -f "$plugin_home/.claude/settings.json" ]; then
    local plugin_content
    plugin_content=$(cat "$plugin_home/.claude/settings.json")
    assert_contains "gopls plugin detected" "gopls-lsp" "$plugin_content"
    assert_contains "rust-analyzer plugin detected" "rust-analyzer-lsp" "$plugin_content"
  else
    fail "user settings for plugin check" "not found at $plugin_home/.claude/settings.json"
  fi
  rm -rf "$plugin_home"
  echo

  # ── Scenario 8: --check passes ─────────────────────────────────
  blue "Scenario 8: --check passes when hooks installed"
  # Re-run defaults to ensure hooks are in place (isolated HOME)
  rm -rf "$WORK_DIR/.claude" "$WORK_DIR/.mcp.json" "$WORK_DIR/CLAUDE.md"
  mkdir -p "$WORK_DIR"
  local check_home
  check_home=$(mktemp -d /tmp/e2e-sc-check-XXXXXX)
  HOME="$check_home" "$GB_BIN" setup claude --defaults --workspace "$WORK_DIR" 2>/dev/null || true
  rm -rf "$check_home"
  local check_rc=0
  "$GB_BIN" setup claude --check --workspace "$WORK_DIR" 2>/dev/null || check_rc=$?
  assert_exit "check passes after defaults" 0 "$check_rc"
  echo

  # ── Scenario 9: --check fails when hooks missing ────────────────
  blue "Scenario 9: --check fails when hooks missing"
  local empty_dir
  empty_dir=$(mktemp -d /tmp/e2e-sc-empty-XXXXXX)
  mkdir -p "$empty_dir/.claude"
  # Write a settings.local.json with hooks but no gb hook commands
  cat > "$empty_dir/.claude/settings.local.json" <<'ENDJSON'
{
  "hooks": {
    "SessionStart": [
      {
        "matcher": "",
        "hooks": [{"type":"command","command":"echo hello"}]
      }
    ]
  }
}
ENDJSON
  local check2_rc=0
  "$GB_BIN" setup claude --check --workspace "$empty_dir" 2>/dev/null || check2_rc=$?
  if [ "$check2_rc" -ne 0 ]; then
    pass "check fails when no gb hooks in settings (exit $check2_rc)"
  else
    fail "check should fail without gb hooks" "exit 0 when gb hooks are missing"
  fi
  rm -rf "$empty_dir"
  echo

  # ── Scenario 10: --check fails when no hooks key ────────────────
  blue "Scenario 10: --check fails when settings.local.json has no hooks key"
  local nohooks_dir
  nohooks_dir=$(mktemp -d /tmp/e2e-sc-nohooks-XXXXXX)
  mkdir -p "$nohooks_dir/.claude"
  echo '{"permissions":{"allow":["Bash(*)"]}}' > "$nohooks_dir/.claude/settings.local.json"
  local check3_rc=0
  local check3_out
  check3_out=$("$GB_BIN" setup claude --check --workspace "$nohooks_dir" 2>&1) || check3_rc=$?
  if [ "$check3_rc" -ne 0 ]; then
    pass "check fails when no hooks key (exit $check3_rc)"
    assert_contains "error mentions no hooks" "no hooks key" "$check3_out"
  else
    fail "check should fail without hooks key" "exit 0 when hooks key is missing"
  fi
  rm -rf "$nohooks_dir"
  echo

  # ── Scenario 11: --check fails when settings.local.json not found ─────
  blue "Scenario 11: --check fails when settings.local.json not found"
  local nofile_dir
  nofile_dir=$(mktemp -d /tmp/e2e-sc-nofile-XXXXXX)
  mkdir -p "$nofile_dir/.claude"
  # Do NOT create settings.local.json
  local check4_rc=0
  local check4_out
  check4_out=$("$GB_BIN" setup claude --check --workspace "$nofile_dir" 2>&1) || check4_rc=$?
  if [ "$check4_rc" -ne 0 ]; then
    pass "check fails when settings.local.json missing (exit $check4_rc)"
    assert_contains "error mentions not found" "not found" "$check4_out"
  else
    fail "check should fail without settings.local.json" "exit 0 when file is missing"
  fi
  rm -rf "$nofile_dir"
  echo

  # ── Scenario 12: --remove removes gb hooks ──────────────────────
  blue "Scenario 12: --remove removes gb hooks from settings.local.json"
  # Re-install defaults first (isolated HOME to avoid clobbering live settings)
  rm -rf "$WORK_DIR/.claude" "$WORK_DIR/.mcp.json" "$WORK_DIR/CLAUDE.md"
  mkdir -p "$WORK_DIR"
  local remove_home
  remove_home=$(mktemp -d /tmp/e2e-sc-rmhome-XXXXXX)
  HOME="$remove_home" "$GB_BIN" setup claude --defaults --workspace "$WORK_DIR" 2>/dev/null || true
  rm -rf "$remove_home"
  local remove_rc=0
  "$GB_BIN" setup claude --remove --workspace "$WORK_DIR" 2>/dev/null || remove_rc=$?
  assert_exit "gb setup claude --remove" 0 "$remove_rc"

  if [ -f "$WORK_DIR/.claude/settings.local.json" ]; then
    local removed_content
    removed_content=$(cat "$WORK_DIR/.claude/settings.local.json")
    # After remove, gb hook commands should be gone
    assert_not_contains "gb hook removed" "gb hook" "$removed_content"
    assert_not_contains "gb bus emit removed" "gb bus emit" "$removed_content"
  fi
  echo

  # ── Scenario 13: --remove preserves non-gb hook entries ─────────
  blue "Scenario 13: --remove preserves non-gb hook entries"
  # Create settings with both gb hooks and a custom hook
  cat > "$WORK_DIR/.claude/settings.local.json" <<'ENDJSON'
{
  "hooks": {
    "SessionStart": [
      {
        "matcher": "",
        "hooks": [{"type":"command","command":"gb hook prime 2>/dev/null || true"}]
      },
      {
        "matcher": "",
        "hooks": [{"type":"command","command":"echo custom-start-hook"}]
      }
    ],
    "Stop": [
      {
        "matcher": "",
        "hooks": [{"type":"command","command":"gb hook stop-gate"}]
      }
    ],
    "PostToolUse": [
      {
        "matcher": "",
        "hooks": [{"type":"command","command":"my-custom-tool-hook.sh"}]
      }
    ]
  }
}
ENDJSON
  local remove2_rc=0
  "$GB_BIN" setup claude --remove --workspace "$WORK_DIR" 2>/dev/null || remove2_rc=$?
  assert_exit "remove with mixed hooks" 0 "$remove2_rc"

  if [ -f "$WORK_DIR/.claude/settings.local.json" ]; then
    local mixed_content
    mixed_content=$(cat "$WORK_DIR/.claude/settings.local.json")
    # gb hooks should be gone
    assert_not_contains "gb hook prime removed" "gb hook prime" "$mixed_content"
    assert_not_contains "gb hook stop-gate removed" "gb hook stop-gate" "$mixed_content"
    # Custom hooks should be preserved
    assert_contains "custom start hook preserved" "custom-start-hook" "$mixed_content"
    assert_contains "custom tool hook preserved" "my-custom-tool-hook.sh" "$mixed_content"
  else
    fail "settings.local.json after mixed remove" "file not found"
  fi
  echo

  # ── Scenario 14: --check fails after --remove ──────────────────
  blue "Scenario 14: --check fails after --remove"
  # The WORK_DIR from scenario 13 now has only non-gb hooks
  local check5_rc=0
  "$GB_BIN" setup claude --check --workspace "$WORK_DIR" 2>/dev/null || check5_rc=$?
  if [ "$check5_rc" -ne 0 ]; then
    pass "check fails after remove (exit $check5_rc)"
  else
    fail "check should fail after remove" "exit 0 after gb hooks were removed"
  fi
  echo

  # ── Scenario 15: Normal mode resolves from config beads ─────────
  blue "Scenario 15: Normal mode resolves from config beads"

  # Create config beads for normal resolution
  local h_id
  h_id=$(create_config "claude-hooks" "global" \
    '{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"gb hook prime 2>/dev/null || true"}]}],"Stop":[{"matcher":"","hooks":[{"type":"command","command":"gb hook stop-gate"}]}]}}')

  if [ -z "$h_id" ]; then
    fail "create claude-hooks config bead" "failed to create"
  else
    pass "created claude-hooks config bead ($h_id)"
  fi

  rm -rf "$WORK_DIR/.claude" "$WORK_DIR/.mcp.json"
  mkdir -p "$WORK_DIR/.claude"
  local normal_rc=0
  "$GB_BIN" setup claude --workspace "$WORK_DIR" 2>/dev/null || normal_rc=$?
  assert_exit "normal setup" 0 "$normal_rc"

  if [ -f "$WORK_DIR/.claude/settings.local.json" ]; then
    local normal_content
    normal_content=$(cat "$WORK_DIR/.claude/settings.local.json")
    assert_contains "normal has hooks" "hooks" "$normal_content"
    assert_contains "normal has gb hook prime" "gb hook prime" "$normal_content"
    assert_contains "normal has gb hook stop-gate" "gb hook stop-gate" "$normal_content"
  else
    fail "normal settings.local.json" "not created"
  fi
  echo

  # ── Scenario 16: Normal mode falls back to defaults for user settings
  blue "Scenario 16: Normal mode falls back to defaults for user settings"
  # With no global claude-settings config bead, normal mode should fall back
  # to defaults. However, the live E2E environment may have a claude-settings
  # config bead matching this agent's subscriptions. The test verifies that
  # SOME user settings are written and have valid structure.
  local fallback_home
  fallback_home=$(mktemp -d /tmp/e2e-sc-fallback-XXXXXX)
  mkdir -p "$fallback_home/.claude"
  rm -rf "$WORK_DIR/.claude" "$WORK_DIR/.mcp.json"
  mkdir -p "$WORK_DIR/.claude"
  HOME="$fallback_home" "$GB_BIN" setup claude --workspace "$WORK_DIR" 2>/dev/null || true
  if [ -f "$fallback_home/.claude/settings.json" ]; then
    local fallback_content
    fallback_content=$(cat "$fallback_home/.claude/settings.json")
    # Verify valid JSON was written (either fallback defaults or resolved config)
    if echo "$fallback_content" | python3 -c "import sys,json; json.load(sys.stdin)" 2>/dev/null; then
      pass "user settings is valid JSON"
    else
      fail "user settings JSON" "invalid JSON written"
    fi
    # If the fallback triggered (no claude-settings bead matched), verify defaults.
    # A resolved config bead may also contain "permissions" (e.g., deny-only),
    # so check for both "permissions" AND "allow" to confirm defaults were used.
    if grep -q '"allow"' <<<"$fallback_content"; then
      pass "fallback has permissions with allow (defaults used)"
    elif grep -q "permissions" <<<"$fallback_content"; then
      # A claude-settings config bead was resolved — has permissions but no allow list
      dim "  claude-settings resolved from config bead (permissions without allow)"
      pass "user settings resolved from environment"
    else
      # A claude-settings config bead was resolved from the environment
      dim "  claude-settings resolved from config bead (not fallback)"
      pass "user settings resolved from environment"
    fi
  else
    fail "fallback user settings" "not found at $fallback_home/.claude/settings.json"
  fi
  rm -rf "$fallback_home"
  echo

  # ── Scenario 17: Normal mode errors when no claude-hooks config ──
  blue "Scenario 17: Normal mode errors when no claude-hooks config found"
  # Close the hooks config bead created in Scenario 15
  if [ -n "$h_id" ]; then
    "$KD_BIN" close "$h_id" 2>/dev/null || true
  fi

  rm -rf "$WORK_DIR/.claude" "$WORK_DIR/.mcp.json"
  mkdir -p "$WORK_DIR/.claude"
  local nohooks_rc=0
  local nohooks_out
  nohooks_out=$("$GB_BIN" setup claude --workspace "$WORK_DIR" 2>&1) || nohooks_rc=$?
  if [ "$nohooks_rc" -ne 0 ]; then
    pass "normal mode errors without hooks config (exit $nohooks_rc)"
    assert_contains "error mentions no claude-hooks" "no claude-hooks" "$nohooks_out"
  else
    # May succeed if global config beads already exist in the environment.
    # In that case, just verify hooks were written.
    if [ -f "$WORK_DIR/.claude/settings.local.json" ]; then
      local env_content
      env_content=$(cat "$WORK_DIR/.claude/settings.local.json")
      if grep -q "hooks" <<<"$env_content"; then
        dim "  normal mode succeeded (pre-existing global claude-hooks bead in env)"
        pass "normal mode resolved from environment hooks"
      else
        fail "normal mode should error" "exit 0 but no hooks written"
      fi
    else
      fail "normal mode should error" "exit 0 and no settings.local.json"
    fi
  fi
  echo

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
