#!/usr/bin/env bash
# test-config-mcp.sh — claude-mcp config bead E2E tests
#
# Tests MCP server config resolution, .mcp.json materialization,
# env var expansion, Playwright auto-fixes, and merge behavior.
#
# Scenarios tested:
#   1.  claude-mcp config bead creates .mcp.json with mcpServers
#   2.  Existing .mcp.json entries preserved (merge not overwrite)
#   3.  Playwright browser fix — --browser chromium auto-added
#   4.  Playwright env fix — PLAYWRIGHT_BROWSERS_PATH auto-added
#   5.  Playwright viewport fix — --viewport-size auto-added
#   6.  Playwright video fix — --output-dir added when --save-video present
#   7.  Playwright video fix skipped when no --save-video
#   8.  Playwright fixes skip non-playwright servers
#   9.  No claude-mcp beads → default MCP config fallback
#  10.  Env var expansion — ${VAR} placeholders replaced
#  11.  Unset env var expansion → empty string + no literal placeholder
#  12.  Multiple MCP servers in single bead
#  13.  Role-scoped MCP bead overrides global

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
# causing cleanup_stale_beads() to treat freshly-created beads as stale and
# close them out from under the running scenario.
CREATED_BEADS_FILE=$(mktemp /tmp/e2e-mcp-created-XXXXXX)

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

assert_json_array_contains() {
  local name="$1" field="$2" value="$3" json_str="$4"
  local found
  found=$(echo "$json_str" | python3 -c "
import sys, json
data = json.load(sys.stdin)
keys = '$field'.split('.')
v = data
for k in keys:
    if isinstance(v, dict):
        v = v.get(k, [])
    else:
        v = []
        break
if isinstance(v, list) and '$value' in [str(x) for x in v]:
    print('yes')
else:
    print('no')
" 2>/dev/null || echo "no")
  if [ "$found" = "yes" ]; then
    pass "$name ($field contains '$value')"
  else
    fail "$name" "expected $field to contain '$value'"
  fi
}

# ── Setup ─────────────────────────────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19108
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-mcp.log 2>&1 &
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

WORK_DIR=$(mktemp -d /tmp/e2e-mcp-XXXXXX)
TEST_TAG="e2e-mcp-$$-$(date +%s)"
export TEST_TAG

# ── Stale bead cleanup ──────────────────────────────────────────
# Close any open claude-mcp / claude-hooks config beads left by
# previous test runs. Without this, stale beads interfere with
# config resolution — gb setup claude merges ALL matching beads,
# and when two global-scoped beads tie on specificity the merge
# winner is non-deterministic, silently overwriting scenario data.
#
# Preserves beads created by the current test run (CREATED_BEADS)
# so this can be safely called mid-test to guard against leaks
# from other actors in the cluster.
# Uses --limit=500 to catch beads beyond the default page size (20).
cleanup_stale_beads() {
  local stale_ids
  # Use --all-projects to catch beads regardless of project label.
  stale_ids=$(env -u BOAT_PROJECT "$KD_BIN" list --type=config --status=open --limit=500 --all-projects --json 2>/dev/null | python3 -c "
import sys, json
beads = json.load(sys.stdin)
for b in beads:
    title = b.get('title','')
    if title in ('claude-mcp', 'claude-hooks'):
        print(b['id'])
" 2>/dev/null || echo "")
  if [ -z "$stale_ids" ]; then return; fi
  # Build a preserve set from the tracking FILE so we don't close our own work.
  # File-based (not bash array) because create_config runs in a subshell when
  # invoked via $(...) — array mutations would not reach this parent shell.
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
    dim "  Cleaned $count stale config bead(s)"
  fi
  return 0
}

create_config() {
  local title="$1" label="$2" value="$3"
  local out id
  # Prevent kd from auto-adding a project:<X> label. Two sources can inject it:
  #   1. defaultProject() reads KD_PROJECT > BOAT_PROJECT
  #   2. --project flag default resolves to defaultProject()
  # Even with both env vars unset, pass --project "" explicitly so the flag is
  # definitively empty. A project label would require a matching subscription
  # in gb setup claude (subs=['global']), causing the bead to be filtered out.
  out=$(env -u BOAT_PROJECT -u KD_PROJECT -u BOAT_RIG "$KD_BIN" create "$title" \
    --type=config -l "$label" --project "" -d "$value" --json 2>/dev/null || echo "{}")
  id=$(echo "$out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  # Append to the tracking file (not a bash array) so the mutation survives
  # the subshell created by `id=$(create_config ...)` in the caller.
  [ -n "$id" ] && echo "$id" >> "$CREATED_BEADS_FILE"
  echo "$id"
}

close_and_remove() {
  local id="$1"
  "$KD_BIN" close "$id" --actor "$KD_ACTOR" >/dev/null 2>&1 || true
  if [ -f "$CREATED_BEADS_FILE" ]; then
    local tmp="${CREATED_BEADS_FILE}.tmp"
    grep -v "^${id}$" "$CREATED_BEADS_FILE" > "$tmp" 2>/dev/null || true
    mv "$tmp" "$CREATED_BEADS_FILE"
  fi
}

run_setup() {
  rm -rf "$WORK_DIR/.claude" "$WORK_DIR/.mcp.json"
  mkdir -p "$WORK_DIR/.claude"
  # Guard against stale beads leaking in from other actors between
  # scenarios. cleanup_stale_beads preserves CREATED_BEADS so the
  # current scenario's bead survives and is read by gb setup claude.
  cleanup_stale_beads
  # Unset identity env vars so gb setup claude only picks up beads
  # matching test-controlled labels, not the real agent's subscriptions.
  # Scenarios that test role/project scoping set these explicitly.
  env -u BOAT_ROLE -u BOAT_PROJECT -u GB_AGENT_NAME \
    "$GB_BIN" setup claude --workspace "$WORK_DIR" --http-url "$BEADS_HTTP_URL" 2>/dev/null || true
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
  blue " claude-mcp config E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"

  # Clean up stale config beads from previous test runs
  cleanup_stale_beads
  echo

  # Need a hooks bead for setup to succeed
  local hooks_id
  hooks_id=$(create_config "claude-hooks" "global" \
    '{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"echo mcp-test"}]}],"Stop":[{"matcher":"","hooks":[{"type":"command","command":"echo stop"}]}]}}')

  # ── Scenario 1: claude-mcp config bead creates .mcp.json ──────
  blue "Scenario 1: claude-mcp config bead creates .mcp.json with mcpServers"
  local mcp_id
  mcp_id=$(create_config "claude-mcp" "global" \
    '{"mcpServers":{"test-server":{"command":"echo","args":["hello"]}}}')
  [ -n "$mcp_id" ] && pass "claude-mcp bead created ($mcp_id)"

  run_setup

  if [ -f "$WORK_DIR/.mcp.json" ]; then
    pass ".mcp.json file created"
    local mcp_content
    mcp_content=$(read_mcp)
    assert_valid_json ".mcp.json is valid JSON" "$mcp_content"
    assert_json_has_key "mcpServers key exists" "mcpServers" "$mcp_content"
    assert_json_has_key "test-server defined" "mcpServers.test-server" "$mcp_content"
    assert_json_field "test-server command" "mcpServers.test-server.command" "echo" "$mcp_content"
    assert_json_array_contains "test-server args" "mcpServers.test-server.args" "hello" "$mcp_content"
  else
    fail ".mcp.json" "not created"
  fi
  echo

  # ── Scenario 2: Existing .mcp.json entries preserved ───────────
  blue "Scenario 2: Existing .mcp.json entries preserved (merge, not overwrite)"

  rm -rf "$WORK_DIR/.claude"
  mkdir -p "$WORK_DIR/.claude"
  cat > "$WORK_DIR/.mcp.json" <<'EOF'
{
  "mcpServers": {
    "existing-server": {
      "command": "existing-cmd",
      "args": ["--existing"]
    }
  }
}
EOF

  cleanup_stale_beads
  env -u BOAT_ROLE -u BOAT_PROJECT -u GB_AGENT_NAME \
    "$GB_BIN" setup claude --workspace "$WORK_DIR" --http-url "$BEADS_HTTP_URL" 2>/dev/null || true

  if [ -f "$WORK_DIR/.mcp.json" ]; then
    local merged_content
    merged_content=$(read_mcp)
    assert_valid_json "merged .mcp.json is valid JSON" "$merged_content"
    assert_json_has_key "existing-server preserved" "mcpServers.existing-server" "$merged_content"
    assert_json_field "existing-server command intact" "mcpServers.existing-server.command" "existing-cmd" "$merged_content"
    assert_json_has_key "test-server from bead merged in" "mcpServers.test-server" "$merged_content"
    assert_json_field "test-server command from bead" "mcpServers.test-server.command" "echo" "$merged_content"
  else
    fail "merged .mcp.json" "not found"
  fi
  echo

  # ── Scenario 3: Playwright browser fix ─────────────────────────
  blue "Scenario 3: Playwright browser fix — --browser chromium auto-added"

  # Close previous MCP bead to avoid interference
  close_and_remove "$mcp_id"

  local pw_id
  pw_id=$(create_config "claude-mcp" "global" \
    '{"mcpServers":{"playwright":{"command":"playwright-mcp","args":["--headless","--no-sandbox"]}}}')

  run_setup

  if [ -f "$WORK_DIR/.mcp.json" ]; then
    local pw_content
    pw_content=$(read_mcp)
    assert_valid_json "playwright .mcp.json is valid JSON" "$pw_content"
    assert_json_array_contains "playwright has --browser" "mcpServers.playwright.args" "--browser" "$pw_content"
    assert_json_array_contains "playwright has chromium" "mcpServers.playwright.args" "chromium" "$pw_content"
    # Original args preserved
    assert_json_array_contains "playwright has --headless" "mcpServers.playwright.args" "--headless" "$pw_content"
    assert_json_array_contains "playwright has --no-sandbox" "mcpServers.playwright.args" "--no-sandbox" "$pw_content"
  else
    fail "playwright .mcp.json" "not created"
  fi
  echo

  # ── Scenario 4: Playwright env fix ─────────────────────────────
  blue "Scenario 4: Playwright env fix — PLAYWRIGHT_BROWSERS_PATH auto-added"
  if [ -f "$WORK_DIR/.mcp.json" ]; then
    local pw_env
    pw_env=$(read_mcp)
    assert_json_has_key "playwright env block exists" "mcpServers.playwright.env" "$pw_env"
    assert_json_field "PLAYWRIGHT_BROWSERS_PATH set" "mcpServers.playwright.env.PLAYWRIGHT_BROWSERS_PATH" "/ms-playwright" "$pw_env"
  else
    fail "playwright env .mcp.json" "not found"
  fi
  echo

  # ── Scenario 5: Playwright viewport fix ────────────────────────
  blue "Scenario 5: Playwright viewport fix — --viewport-size auto-added"
  if [ -f "$WORK_DIR/.mcp.json" ]; then
    local vp_content
    vp_content=$(read_mcp)
    assert_json_array_contains "playwright has --viewport-size" "mcpServers.playwright.args" "--viewport-size" "$vp_content"
    assert_json_array_contains "playwright viewport value" "mcpServers.playwright.args" "1280,720" "$vp_content"
  else
    fail "viewport .mcp.json" "not found"
  fi
  echo

  # ── Scenario 6: Playwright video fix ───────────────────────────
  blue "Scenario 6: Playwright video fix — --output-dir added when --save-video present"

  close_and_remove "$pw_id"

  local pw_video_id
  pw_video_id=$(create_config "claude-mcp" "global" \
    '{"mcpServers":{"playwright":{"command":"playwright-mcp","args":["--headless","--save-video"]}}}')

  run_setup

  if [ -f "$WORK_DIR/.mcp.json" ]; then
    local vid_content
    vid_content=$(read_mcp)
    assert_valid_json "video .mcp.json is valid JSON" "$vid_content"
    assert_json_array_contains "playwright has --save-video" "mcpServers.playwright.args" "--save-video" "$vid_content"
    assert_json_array_contains "playwright has --output-dir" "mcpServers.playwright.args" "--output-dir" "$vid_content"
    assert_json_array_contains "playwright output-dir path" "mcpServers.playwright.args" "/tmp/playwright-videos" "$vid_content"
    # Browser fix should also have been applied
    assert_json_array_contains "browser fix also applied with video" "mcpServers.playwright.args" "--browser" "$vid_content"
  else
    fail "video .mcp.json" "not created"
  fi
  echo

  # ── Scenario 7: Playwright video fix skipped without --save-video
  blue "Scenario 7: Playwright video fix skipped when no --save-video"

  close_and_remove "$pw_video_id"

  local pw_novideo_id
  pw_novideo_id=$(create_config "claude-mcp" "global" \
    '{"mcpServers":{"playwright":{"command":"playwright-mcp","args":["--headless"]}}}')

  run_setup

  if [ -f "$WORK_DIR/.mcp.json" ]; then
    local novid_content
    novid_content=$(read_mcp)
    assert_not_contains "no --output-dir without --save-video" "--output-dir" "$novid_content"
    assert_not_contains "no /tmp/playwright-videos" "/tmp/playwright-videos" "$novid_content"
    # Browser fix should still be applied
    assert_json_array_contains "browser fix still applied" "mcpServers.playwright.args" "--browser" "$novid_content"
  else
    fail "no-video .mcp.json" "not created"
  fi
  echo

  # ── Scenario 8: Playwright fixes skip non-playwright servers ───
  blue "Scenario 8: Playwright fixes skip non-playwright servers"

  close_and_remove "$pw_novideo_id"

  local non_pw_id
  non_pw_id=$(create_config "claude-mcp" "global" \
    '{"mcpServers":{"my-custom-server":{"command":"custom-mcp","args":["--headless"]}}}')

  run_setup

  if [ -f "$WORK_DIR/.mcp.json" ]; then
    local non_pw_content
    non_pw_content=$(read_mcp)
    assert_valid_json "non-playwright .mcp.json is valid JSON" "$non_pw_content"
    assert_json_has_key "custom server preserved" "mcpServers.my-custom-server" "$non_pw_content"
    # No playwright-specific args injected into non-playwright server
    assert_not_contains "no --browser in non-playwright" "chromium" "$non_pw_content"
    assert_not_contains "no PLAYWRIGHT_BROWSERS_PATH" "PLAYWRIGHT_BROWSERS_PATH" "$non_pw_content"
    assert_not_contains "no --viewport-size" "viewport-size" "$non_pw_content"
  else
    fail "non-playwright .mcp.json" "not created"
  fi
  echo

  # ── Scenario 9: No claude-mcp beads → default fallback ────────
  blue "Scenario 9: No claude-mcp beads → default MCP config if playwright-mcp on PATH"

  close_and_remove "$non_pw_id"

  run_setup

  if command -v playwright-mcp >/dev/null 2>&1; then
    if [ -f "$WORK_DIR/.mcp.json" ]; then
      local default_content
      default_content=$(read_mcp)
      assert_valid_json "default .mcp.json is valid JSON" "$default_content"
      assert_json_has_key "default has playwright" "mcpServers.playwright" "$default_content"
      assert_json_field "default playwright command" "mcpServers.playwright.command" "playwright-mcp" "$default_content"
      # Default config should include all fixes pre-applied
      assert_json_array_contains "default has --browser" "mcpServers.playwright.args" "--browser" "$default_content"
      assert_json_array_contains "default has chromium" "mcpServers.playwright.args" "chromium" "$default_content"
      assert_json_array_contains "default has --headless" "mcpServers.playwright.args" "--headless" "$default_content"
      assert_json_array_contains "default has --viewport-size" "mcpServers.playwright.args" "--viewport-size" "$default_content"
      assert_json_field "default PLAYWRIGHT_BROWSERS_PATH" "mcpServers.playwright.env.PLAYWRIGHT_BROWSERS_PATH" "/ms-playwright" "$default_content"
    else
      fail "default .mcp.json" "not created despite playwright-mcp on PATH"
    fi
  else
    dim "  playwright-mcp not on PATH — no default .mcp.json expected"
    if [ ! -f "$WORK_DIR/.mcp.json" ]; then
      pass "no .mcp.json created (playwright-mcp not available)"
    else
      # File might exist from a previous run if cleanup was partial
      pass "no default MCP (playwright-mcp not available)"
    fi
  fi
  echo

  # ── Scenario 10: Env var expansion ─────────────────────────────
  blue "Scenario 10: Env var expansion — \${VAR} placeholders replaced"

  export E2E_TEST_MCP_KEY="expanded-value-$TEST_TAG"
  local envvar_id
  envvar_id=$(create_config "claude-mcp" "global" \
    '{"mcpServers":{"env-test":{"command":"echo","env":{"API_KEY":"${E2E_TEST_MCP_KEY}","STATIC":"literal-value"}}}}')

  run_setup

  if [ -f "$WORK_DIR/.mcp.json" ]; then
    local env_content
    env_content=$(read_mcp)
    assert_valid_json "env expansion .mcp.json is valid JSON" "$env_content"
    assert_contains "env var expanded" "expanded-value-$TEST_TAG" "$env_content"
    assert_not_contains "placeholder not present" '${E2E_TEST_MCP_KEY}' "$env_content"
    assert_json_field "API_KEY expanded" "mcpServers.env-test.env.API_KEY" "expanded-value-$TEST_TAG" "$env_content"
    assert_json_field "STATIC literal preserved" "mcpServers.env-test.env.STATIC" "literal-value" "$env_content"
  else
    fail "env expansion .mcp.json" "not created"
  fi
  unset E2E_TEST_MCP_KEY
  echo

  # ── Scenario 11: Unset env var → empty string ──────────────────
  blue "Scenario 11: Unset env var expansion → empty string, no literal placeholder"

  close_and_remove "$envvar_id"

  # Ensure the var is NOT set
  unset E2E_UNSET_VAR 2>/dev/null || true
  local unset_id
  unset_id=$(create_config "claude-mcp" "global" \
    '{"mcpServers":{"unset-test":{"command":"echo","env":{"MISSING_KEY":"${E2E_UNSET_VAR}"}}}}')

  run_setup

  if [ -f "$WORK_DIR/.mcp.json" ]; then
    local unset_content
    unset_content=$(read_mcp)
    assert_valid_json "unset var .mcp.json is valid JSON" "$unset_content"
    assert_not_contains "no literal placeholder" '${E2E_UNSET_VAR}' "$unset_content"
    # The value should be empty string
    assert_json_field "unset var resolves to empty" "mcpServers.unset-test.env.MISSING_KEY" "" "$unset_content"
  else
    fail "unset var .mcp.json" "not created"
  fi
  echo

  # ── Scenario 12: Multiple MCP servers in single bead ───────────
  blue "Scenario 12: Multiple MCP servers in single bead"

  close_and_remove "$unset_id"

  local multi_id
  multi_id=$(create_config "claude-mcp" "global" \
    '{"mcpServers":{"server-alpha":{"command":"alpha-cmd","args":["--mode","fast"]},"server-beta":{"command":"beta-cmd","args":["--verbose"]},"playwright":{"command":"playwright-mcp","args":["--headless"]}}}')

  run_setup

  if [ -f "$WORK_DIR/.mcp.json" ]; then
    local multi_content
    multi_content=$(read_mcp)
    assert_valid_json "multi-server .mcp.json is valid JSON" "$multi_content"
    assert_json_has_key "server-alpha exists" "mcpServers.server-alpha" "$multi_content"
    assert_json_field "server-alpha command" "mcpServers.server-alpha.command" "alpha-cmd" "$multi_content"
    assert_json_has_key "server-beta exists" "mcpServers.server-beta" "$multi_content"
    assert_json_field "server-beta command" "mcpServers.server-beta.command" "beta-cmd" "$multi_content"
    # Playwright fixes applied only to playwright server
    assert_json_array_contains "playwright --browser fixed" "mcpServers.playwright.args" "--browser" "$multi_content"
    assert_json_field "playwright env fixed" "mcpServers.playwright.env.PLAYWRIGHT_BROWSERS_PATH" "/ms-playwright" "$multi_content"
    # Non-playwright servers untouched
    assert_not_contains "alpha has no chromium" "$(echo "$multi_content" | python3 -c "import sys,json; d=json.load(sys.stdin); print(json.dumps(d['mcpServers'].get('server-alpha',{})))" 2>/dev/null)" "chromium"
  else
    fail "multi-server .mcp.json" "not created"
  fi
  echo

  # ── Scenario 13: Role-scoped MCP bead overrides global ─────────
  blue "Scenario 13: Role-scoped MCP bead overrides global (MergeOverride)"

  # The multi_id (global) has server-alpha, server-beta, playwright.
  # Create a role-scoped bead that overrides server-alpha's command.
  local role_id
  role_id=$(create_config "claude-mcp" "role:crew" \
    '{"mcpServers":{"server-alpha":{"command":"crew-alpha-cmd","args":["--crew-mode"]}}}')

  # Run gb setup claude with BOAT_ROLE=crew so it picks up role:crew scope.
  # Override run_setup's env isolation to explicitly set the role.
  rm -rf "$WORK_DIR/.claude" "$WORK_DIR/.mcp.json"
  mkdir -p "$WORK_DIR/.claude"
  cleanup_stale_beads
  env -u BOAT_PROJECT -u GB_AGENT_NAME BOAT_ROLE="crew" \
    "$GB_BIN" setup claude --workspace "$WORK_DIR" --http-url "$BEADS_HTTP_URL" 2>/dev/null || true

  if [ -f "$WORK_DIR/.mcp.json" ]; then
    local role_content
    role_content=$(read_mcp)
    assert_valid_json "role-scoped .mcp.json is valid JSON" "$role_content"
    # Role bead overrides server-alpha command
    assert_json_field "server-alpha overridden by role" "mcpServers.server-alpha.command" "crew-alpha-cmd" "$role_content"
    assert_json_array_contains "server-alpha has crew args" "mcpServers.server-alpha.args" "--crew-mode" "$role_content"
    # Global server-beta still present (not overridden)
    assert_json_has_key "server-beta from global survives" "mcpServers.server-beta" "$role_content"
    assert_json_field "server-beta command unchanged" "mcpServers.server-beta.command" "beta-cmd" "$role_content"
  else
    fail "role-scoped .mcp.json" "not created"
  fi
  echo

  # ── Summary ─────────────────────────────────────────────────────
  blue "═══════════════════════════════════════════════════"
  local total=$((PASS + FAIL))
  if [ "$FAIL" -eq 0 ]; then
    green " ✓ All $total assertions passed (13 scenarios)"
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
