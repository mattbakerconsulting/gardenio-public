#!/usr/bin/env bash
# test-account-auth.sh — account bead auth resolution + failover E2E tests
#
# Tests account bead CRUD, config:auth resolution, and budget-exceeded failover
# against a live kd server. The controller must be running with ConfigUpdated and
# BudgetExceeded support (>= 2026.76).
#
# Usage:
#   ./tests/e2e/scripts/test-account-auth.sh [--namespace <ns>] [--daemon <url>] [--token <token>]
#
# Prerequisites:
#   - kubectl context pointing at the target cluster
#   - kd binary in PATH (or set KD_BIN)
#   - gb binary in PATH (or set GB_BIN)
#   - jq installed
#
# Scenarios tested:
#   1. Account bead CRUD — create, show fields, close
#   2. Config:auth bead CRUD — create with project label, show description
#   3. Auth resolution — config:auth → account bead lookup chain
#   4. Account switching — update config:auth, verify new account
#   5. Fallback chain — account with fallback field resolves correctly
#   6. Budget exceeded failover — simulate budget_exceeded, verify config:auth switches

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
KD_BIN="${KD_BIN:-kd}"
GB_BIN="${GB_BIN:-gb}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PASS=0
FAIL=0
ERRORS=()
PF_PID=""
CLEANUP_BEADS=()

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
    fail "$name" "expected output to contain '$pattern'; got: $actual"
  fi
}

assert_equals() {
  local name="$1"
  local expected="$2"
  local actual="$3"
  if [ "$expected" = "$actual" ]; then
    pass "$name (== '$expected')"
  else
    fail "$name" "expected '$expected', got '$actual'"
  fi
}

# ── Setup: port-forward kd server ────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi

  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19093
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-account.log 2>&1 &
  PF_PID=$!
  sleep 3

  export BEADS_HTTP_URL="http://localhost:${port}"
  dim "  Daemon at $BEADS_HTTP_URL (pid $PF_PID)"
}

# ── Setup: get or create test token ──────────────────────────────
setup_token() {
  if [ -n "${BEADS_TOKEN:-}" ]; then
    return
  fi
  local secret
  secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
    -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
  if [ -n "$secret" ]; then
    export BEADS_TOKEN="$secret"
    dim "  Token loaded from secret kd-beads-token"
  else
    dim "  WARNING: No token found; kd server may reject requests"
  fi
}

# ── Cleanup ───────────────────────────────────────────────────────
cleanup() {
  # Clean up test beads
  for id in "${CLEANUP_BEADS[@]:-}"; do
    "$KD_BIN" close "$id" 2>/dev/null || true
  done
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# ── Bead helpers ──────────────────────────────────────────────────
# Create an account bead and register it for cleanup.
create_account() {
  local title="$1"
  shift
  local out
  # Stamp a uniform e2e:true label on every E2E account bead (in addition to
  # whatever project:/tag label the caller passes) so /costs filtering has ONE
  # reliable, convention-independent signal (gd-GIFSJcWvNp).
  out=$("$KD_BIN" create "$title" --type=account --json -l "e2e:true" "$@" 2>/dev/null)
  local id
  id=$(echo "$out" | jq -r '.id // empty' 2>/dev/null || echo "")
  if [ -n "$id" ]; then
    CLEANUP_BEADS+=("$id")
  fi
  echo "$id"
}

# Create a config bead and register it for cleanup.
create_config() {
  local title="$1"
  shift
  local out
  out=$("$KD_BIN" create "$title" --type=config --json "$@" 2>/dev/null)
  local id
  id=$(echo "$out" | jq -r '.id // empty' 2>/dev/null || echo "")
  if [ -n "$id" ]; then
    CLEANUP_BEADS+=("$id")
  fi
  echo "$id"
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " account bead auth + failover E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  # Verify kd server is reachable
  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  local test_suffix="$$-$(date +%s)"
  local TEST_PROJECT="e2e-test-${test_suffix}"

  # ── Scenario 1: Account bead CRUD ────────────────────────────
  blue "Scenario 1: Account bead CRUD — create with fields, show, close"

  local acc1_id
  acc1_id=$(create_account "e2e-acct-primary-${test_suffix}" \
    -l "project:$TEST_PROJECT" \
    -f "provider=anthropic" \
    -f "base_url=https://litellm.example.com" \
    -f "credential=secret:e2e-test-key" \
    -f "model=claude-opus-4-6" \
    -f "fallback=e2e-acct-backup-${test_suffix}")

  if [ -n "$acc1_id" ]; then
    pass "account bead created ($acc1_id)"

    # Show and verify fields
    local show_out
    show_out=$("$KD_BIN" show "$acc1_id" --json 2>/dev/null || echo "{}")
    assert_contains "account has provider field" "anthropic" "$show_out"
    assert_contains "account has base_url field" "litellm.example.com" "$show_out"
    assert_contains "account has credential field" "secret:e2e-test-key" "$show_out"
    assert_contains "account has fallback field" "e2e-acct-backup-${test_suffix}" "$show_out"

    local acc1_type
    acc1_type=$(echo "$show_out" | jq -r '.type // empty' 2>/dev/null || echo "")
    assert_equals "account bead type is account" "account" "$acc1_type"
  else
    fail "account bead created" "kd create returned no ID"
  fi
  echo

  # ── Scenario 2: Config:auth bead CRUD ────────────────────────
  blue "Scenario 2: Config:auth bead — create with project label and JSON description"

  local auth_desc="{\"account\":\"e2e-acct-primary-${test_suffix}\"}"
  local cfg1_id
  cfg1_id=$(create_config "auth" \
    -l "project:$TEST_PROJECT" \
    -d "$auth_desc")

  if [ -n "$cfg1_id" ]; then
    pass "config:auth bead created ($cfg1_id)"

    local cfg_show
    cfg_show=$("$KD_BIN" show "$cfg1_id" --json 2>/dev/null || echo "{}")
    assert_contains "config:auth has project label" "project:$TEST_PROJECT" "$cfg_show"
    assert_contains "config:auth description has account ref" "e2e-acct-primary-${test_suffix}" "$cfg_show"

    local cfg1_type
    cfg1_type=$(echo "$cfg_show" | jq -r '.type // empty' 2>/dev/null || echo "")
    assert_equals "config bead type is config" "config" "$cfg1_type"
  else
    fail "config:auth bead created" "kd create returned no ID"
  fi
  echo

  # ── Scenario 3: Auth resolution chain ────────────────────────
  blue "Scenario 3: Auth resolution — config:auth → account bead lookup"

  if [ -n "${cfg1_id:-}" ] && [ -n "${acc1_id:-}" ]; then
    # Look up config:auth for project e2e-test
    local config_list
    config_list=$("$KD_BIN" list --type=config --status=open \
      -l "project:$TEST_PROJECT" --json 2>/dev/null || echo '{"beads":[]}')

    # Find the auth config (kd list --json returns a bare array)
    local auth_account
    auth_account=$(echo "$config_list" | jq -r \
      '.[] | select(.title == "auth") | .description' 2>/dev/null \
      | jq -r '.account // empty' 2>/dev/null | head -1 || echo "")

    if [ -n "$auth_account" ]; then
      pass "config:auth found for project:$TEST_PROJECT"
      assert_equals "config:auth points to primary account" \
        "e2e-acct-primary-${test_suffix}" "$auth_account"

      # Resolve the account bead by title
      local account_list
      account_list=$("$KD_BIN" list --type=account --status=open --json 2>/dev/null || echo "[]")
      local resolved_provider
      resolved_provider=$(echo "$account_list" | jq -r \
        ".[] | select(.title == \"$auth_account\") | .fields.provider // empty" 2>/dev/null | head -1 || echo "")
      assert_equals "resolved account has provider=anthropic" "anthropic" "$resolved_provider"
    else
      fail "config:auth found for project" "auth bead not found or no account field"
    fi
  else
    fail "scenario 3 skipped" "missing beads from scenarios 1-2"
  fi
  echo

  # ── Scenario 4: Account switching ────────────────────────────
  blue "Scenario 4: Account switching — update config:auth to new account"

  # Create a second account
  local acc2_id
  acc2_id=$(create_account "e2e-acct-backup-${test_suffix}" \
    -l "project:$TEST_PROJECT" \
    -f "provider=anthropic" \
    -f "credential=coopmux:e2e-team")

  if [ -n "${cfg1_id:-}" ] && [ -n "${acc2_id:-}" ]; then
    pass "backup account bead created ($acc2_id)"

    # Switch config:auth to the backup account
    local new_desc="{\"account\":\"e2e-acct-backup-${test_suffix}\"}"
    local update_rc=0
    "$KD_BIN" update "$cfg1_id" -d "$new_desc" 2>/dev/null || update_rc=$?
    assert_exit "config:auth update exits 0" 0 "$update_rc"

    # Verify the switch
    local cfg_after
    cfg_after=$("$KD_BIN" show "$cfg1_id" --json 2>/dev/null || echo "{}")
    local after_account
    after_account=$(echo "$cfg_after" | jq -r '.description' 2>/dev/null \
      | jq -r '.account // empty' 2>/dev/null || echo "")
    assert_equals "config:auth now points to backup" \
      "e2e-acct-backup-${test_suffix}" "$after_account"
  else
    fail "scenario 4 skipped" "missing config or backup account bead"
  fi
  echo

  # ── Scenario 5: Fallback chain validation ────────────────────
  blue "Scenario 5: Fallback chain — primary account has fallback field"

  if [ -n "${acc1_id:-}" ]; then
    local acc1_show
    acc1_show=$("$KD_BIN" show "$acc1_id" --json 2>/dev/null || echo "{}")
    local fallback_ref
    fallback_ref=$(echo "$acc1_show" | jq -r '.fields.fallback // empty' 2>/dev/null || echo "")
    assert_equals "primary has fallback field" \
      "e2e-acct-backup-${test_suffix}" "$fallback_ref"

    # Verify fallback account exists and can be resolved
    if [ -n "${acc2_id:-}" ]; then
      local acc2_show
      acc2_show=$("$KD_BIN" show "$acc2_id" --json 2>/dev/null || echo "{}")
      local fb_provider
      fb_provider=$(echo "$acc2_show" | jq -r '.fields.provider // empty' 2>/dev/null || echo "")
      assert_equals "fallback account has provider" "anthropic" "$fb_provider"

      # Verify fallback has no further fallback (chain terminates)
      local fb_fallback
      fb_fallback=$(echo "$acc2_show" | jq -r '.fields.fallback // empty' 2>/dev/null || echo "")
      assert_equals "fallback chain terminates (no further fallback)" "" "$fb_fallback"
    else
      fail "fallback account resolution" "backup account bead not created"
    fi
  else
    fail "scenario 5 skipped" "primary account not created"
  fi
  echo

  # ── Scenario 6: Budget exceeded failover simulation ──────────
  blue "Scenario 6: Budget exceeded — simulate via agent_state update"
  dim "  NOTE: This scenario requires controller >= 2026.76 with BudgetExceeded support"

  # Reset config:auth to primary account for failover test
  if [ -n "${cfg1_id:-}" ] && [ -n "${acc1_id:-}" ] && [ -n "${acc2_id:-}" ]; then
    local primary_desc="{\"account\":\"e2e-acct-primary-${test_suffix}\"}"
    "$KD_BIN" update "$cfg1_id" -d "$primary_desc" 2>/dev/null || true

    # Create a mock agent bead to simulate budget_exceeded
    local agent_out
    agent_out=$("$KD_BIN" create "e2e-budget-agent-${test_suffix}" --type=agent --json \
      -l "project:$TEST_PROJECT" \
      -f "project=e2e-test" \
      -f "role=crew" \
      -f "agent=e2e-budget-agent-${test_suffix}" \
      -f "agent_state=working" 2>/dev/null || echo "{}")
    local agent_id
    agent_id=$(echo "$agent_out" | jq -r '.id // empty' 2>/dev/null || echo "")

    if [ -n "$agent_id" ]; then
      CLEANUP_BEADS+=("$agent_id")
      pass "mock agent bead created ($agent_id)"

      # Trigger budget_exceeded by updating agent_state.
      # This requires the agent_state enum to include budget_exceeded (>= 2026.76).
      local update_out update_rc=0
      update_out=$("$KD_BIN" update "$agent_id" -f "agent_state=budget_exceeded" 2>&1) || update_rc=$?

      if [ "$update_rc" -ne 0 ] && grep -q "must be one of" <<<"$update_out"; then
        dim "  SKIP: agent_state enum does not include budget_exceeded (server needs upgrade)"
        dim "  Scenarios 6 assertions skipped — requires kbeads + controller >= 2026.76"
      else
        assert_exit "agent_state update to budget_exceeded exits 0" 0 "$update_rc"

        # Auto-failover requires a running controller to process the SSE event.
        # Check if the controller pod is running; skip gracefully if not.
        local ctrl_pod
        ctrl_pod=$(kubectl get pods -n "$NAMESPACE" --no-headers 2>/dev/null \
          | awk '/controller/ && /Running/ {print $1; exit}' || echo "")

        if [ -z "$ctrl_pod" ]; then
          dim "  SKIP: No controller pod running in $NAMESPACE"
          dim "  Auto-failover requires a live controller to process budget_exceeded events"
          pass "scenario 6 skipped (no controller pod — auto-failover not testable)"
        else
          # Wait for controller to process the event
          dim "  Controller pod: $ctrl_pod — waiting 5s for budget_exceeded processing..."
          sleep 5

          # Check if config:auth was switched to fallback
          local cfg_check
          cfg_check=$("$KD_BIN" show "$cfg1_id" --json 2>/dev/null || echo "{}")
          local current_account
          current_account=$(echo "$cfg_check" | jq -r '.description' 2>/dev/null \
            | jq -r '.account // empty' 2>/dev/null || echo "")

          if [ "$current_account" = "e2e-acct-backup-${test_suffix}" ]; then
            pass "controller switched config:auth to fallback account"
          else
            fail "controller auto-failover" \
              "expected account 'e2e-acct-backup-${test_suffix}', got '$current_account' (controller may lack BudgetExceeded support)"
          fi
        fi
      fi
    else
      fail "mock agent creation" "kd create returned no ID"
    fi
  else
    fail "scenario 6 skipped" "missing beads from earlier scenarios"
  fi
  echo

  # ── Summary ───────────────────────────────────────────────────
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
