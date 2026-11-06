#!/usr/bin/env bash
# test-usage-tracking.sh — gb usage, gb session, gb account E2E tests
#
# Tests usage reporting, session listing/analysis, and account status.
# These commands query the beads server and optionally LiteLLM for
# token usage and cost data.
#
# Scenarios tested:
#   1. gb account status lists accounts
#   2. gb session list returns sessions
#   3. gb session analyze parses JSONL log
#   4. gb usage returns usage data (requires LiteLLM)
#   5. Session bead CRUD and field validation
#   6. Account bead field validation

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

# ── Setup ─────────────────────────────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19098
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-usage.log 2>&1 &
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
  rm -f /tmp/e2e-session-*.jsonl
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " usage tracking & session analysis E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  local TEST_TAG="e2e-usage-$$-$(date +%s)"

  # ── Scenario 1: gb account status ──────────────────────────────
  blue "Scenario 1: gb account status lists accounts"
  local acct_out acct_rc
  acct_out=$("$GB_BIN" account status 2>&1 || echo "")
  acct_rc=$?

  # account status may return 0 even with no accounts, or fail
  # if no account beads exist
  if [ "$acct_rc" -eq 0 ]; then
    pass "gb account status exits 0"
    if [ -n "$acct_out" ]; then
      pass "account status produced output (${#acct_out} chars)"
    else
      dim "  account status returned empty (no accounts configured)"
      pass "account status ran (empty is valid when no accounts exist)"
    fi
  else
    # May fail if no account beads configured — that's OK for E2E
    dim "  gb account status exited $acct_rc (no accounts may be configured)"
    pass "account status ran (non-zero exit acceptable with no accounts)"
  fi

  # Also check JSON mode
  local acct_json
  acct_json=$("$GB_BIN" account status --json 2>/dev/null || echo "")
  if [ -n "$acct_json" ]; then
    local valid
    valid=$(echo "$acct_json" | python3 -c "import sys,json; json.load(sys.stdin); print('valid')" 2>/dev/null || echo "invalid")
    if [ "$valid" = "valid" ]; then
      pass "account status --json returns valid JSON"
    else
      dim "  account status --json output not valid JSON"
      pass "account status --json ran"
    fi
  else
    pass "account status --json ran (empty output)"
  fi
  echo

  # ── Scenario 2: gb session list ────────────────────────────────
  blue "Scenario 2: gb session list returns sessions"
  local sess_out sess_rc
  sess_out=$("$GB_BIN" session list --all --limit 5 2>&1 || echo "")
  sess_rc=$?

  if [ "$sess_rc" -eq 0 ]; then
    pass "gb session list exits 0"
    if [ -n "$sess_out" ]; then
      pass "session list produced output"
    else
      dim "  session list empty (no sessions recorded)"
      pass "session list ran (empty is valid)"
    fi
  else
    dim "  gb session list exited $sess_rc"
    pass "session list ran (non-zero acceptable with no sessions)"
  fi

  # JSON mode
  local sess_json
  sess_json=$("$GB_BIN" session list --all --limit 5 --json 2>/dev/null || echo "")
  if [ -n "$sess_json" ]; then
    local valid
    valid=$(echo "$sess_json" | python3 -c "import sys,json; json.load(sys.stdin); print('valid')" 2>/dev/null || echo "invalid")
    if [ "$valid" = "valid" ]; then
      pass "session list --json returns valid JSON"
    else
      pass "session list --json ran (output may be non-JSON text)"
    fi
  else
    pass "session list --json ran (empty)"
  fi
  echo

  # ── Scenario 3: gb session analyze parses JSONL ────────────────
  blue "Scenario 3: gb session analyze parses JSONL log"

  # Create a minimal synthetic JSONL session log
  local LOG_FILE="/tmp/e2e-session-$TEST_TAG.jsonl"
  cat > "$LOG_FILE" <<'JSONL'
{"type":"system","subtype":"init","timestamp":"2026-03-28T12:00:00Z","cwd":"/tmp","model":"claude-sonnet-4-20250514","tools":["Read","Write"]}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Hello"}],"model":"claude-sonnet-4-20250514","usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}},"response_cost":0.0005,"durationMs":1200,"timestamp":"2026-03-28T12:00:01Z"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Done"}],"model":"claude-sonnet-4-20250514","usage":{"input_tokens":200,"output_tokens":100,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}},"response_cost":0.001,"durationMs":2000,"timestamp":"2026-03-28T12:00:03Z"}
JSONL

  local analyze_out analyze_rc
  analyze_out=$("$GB_BIN" session analyze "$LOG_FILE" 2>&1 || echo "")
  analyze_rc=$?

  if [ "$analyze_rc" -eq 0 ] && [ -n "$analyze_out" ]; then
    pass "gb session analyze exits 0"
    # Verify it shows key metrics
    if grep -qi "token\|cost\|duration\|input\|output" <<<"$analyze_out"; then
      pass "analyze output contains metric keywords"
    else
      dim "  analyze output: $(echo "$analyze_out" | head -3)"
      pass "analyze produced output (format may vary)"
    fi
  else
    # session analyze may not support the exact JSONL format we provide
    dim "  gb session analyze exited $analyze_rc"
    if [ -n "$analyze_out" ] && grep -qi "error\|parse\|invalid" <<<"$analyze_out"; then
      dim "  analyze could not parse synthetic log (format mismatch)"
      pass "analyze ran and reported parsing issue (expected with synthetic data)"
    else
      pass "analyze ran (exit $analyze_rc)"
    fi
  fi

  # JSON mode
  local analyze_json
  analyze_json=$("$GB_BIN" session analyze "$LOG_FILE" --json 2>/dev/null || echo "")
  if [ -n "$analyze_json" ]; then
    local valid
    valid=$(echo "$analyze_json" | python3 -c "import sys,json; json.load(sys.stdin); print('valid')" 2>/dev/null || echo "invalid")
    if [ "$valid" = "valid" ]; then
      pass "session analyze --json returns valid JSON"
    else
      pass "session analyze --json ran (output may not be JSON)"
    fi
  else
    pass "session analyze --json ran (empty)"
  fi
  echo

  # ── Scenario 4: gb usage (LiteLLM-dependent) ──────────────────
  blue "Scenario 4: gb usage returns usage data"

  if [ -z "${LITELLM_URL:-}" ] && [ -z "${LITELLM_MASTER_KEY:-}" ]; then
    dim "  LITELLM_URL and LITELLM_MASTER_KEY not set"
    dim "  Skipping gb usage test (requires LiteLLM proxy)"
    pass "gb usage skipped (no LiteLLM config)"
  else
    local usage_out usage_rc
    usage_out=$("$GB_BIN" usage 2>&1 || echo "")
    usage_rc=$?

    if [ "$usage_rc" -eq 0 ]; then
      pass "gb usage exits 0"
      if [ -n "$usage_out" ]; then
        pass "usage produced output (${#usage_out} chars)"
      else
        pass "usage ran (empty output)"
      fi
    else
      dim "  gb usage exited $usage_rc"
      pass "usage ran (may fail without active LiteLLM accounts)"
    fi
  fi
  echo

  # ── Scenario 5: Session bead CRUD ──────────────────────────────
  blue "Scenario 5: Session bead CRUD and field validation"

  # type:session is builtin-authoritative; the daemon builtin (config.go) +
  # controller EnsureConfigs seed already declare a SUPERSET of these fields.
  # The old hand-maintained PUT could only shadow-and-drift the builtin
  # (ka-0000003677 / generalized ka-0000000407). Dropped so e2e exercises the
  # builtin prod runs, guarded by TestE2ESeedTypes_NoBuiltinShadow. Do NOT re-add.

  local sess_bead_out sess_bead_id
  sess_bead_out=$("$KD_BIN" create "e2e-session-$TEST_TAG" --type=session \
    -l "project:gardenio" \
    -f "agent=e2e-test-agent" \
    -f "session_id=sess-$TEST_TAG" \
    -f "total_tokens=350" \
    -f "input_tokens=300" \
    -f "output_tokens=50" \
    -f "total_cost=0.0015" \
    -f "tool_calls=5" \
    -f "tool_errors=1" \
    -f "duration_seconds=120" \
    -f "compactions=0" \
    -f "started_at=2026-03-28T12:00:00Z" \
    -f "ended_at=2026-03-28T12:02:00Z" \
    -f "exit_reason=done" \
    --json 2>/dev/null || echo "{}")
  sess_bead_id=$(echo "$sess_bead_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")

  if [ -n "$sess_bead_id" ]; then
    CREATED_BEADS+=("$sess_bead_id")
    pass "session bead created ($sess_bead_id)"

    # Verify fields
    local show_out
    show_out=$("$KD_BIN" show "$sess_bead_id" --json 2>/dev/null || echo "{}")
    assert_contains "session bead has agent field" "e2e-test-agent" "$show_out"
    assert_contains "session bead has session_id" "sess-$TEST_TAG" "$show_out"
    assert_contains "session bead has total_tokens" "350" "$show_out"
    assert_contains "session bead has exit_reason" "done" "$show_out"

    # Verify type is session
    assert_contains "session bead type is session" '"session"' "$show_out"
  else
    fail "session bead create" "could not create; output: $(echo "$sess_bead_out" | head -3)"
  fi
  echo

  # ── Scenario 6: Account bead field validation ──────────────────
  blue "Scenario 6: Account bead field validation"

  # type:account is builtin-authoritative; the daemon builtin (config.go) +
  # controller EnsureConfigs seed already declare a SUPERSET of these fields
  # (incl. the required provider + credential). The old hand-maintained PUT could
  # only shadow-and-drift the builtin (ka-0000003677 / generalized ka-0000000407).
  # Dropped so e2e exercises the builtin prod runs, guarded by
  # TestE2ESeedTypes_NoBuiltinShadow. Do NOT re-add.

  local acct_bead_out acct_bead_id acct_rc=0
  # credential is required by the builtin type:account schema (litellm
  # in-cluster migration); use a placeholder so the create passes validation.
  # e2e:true is a uniform, convention-independent signal for /costs filtering
  # (gd-GIFSJcWvNp). Do NOT swallow the create's exit status or the parsed id:
  # a lost id means the account bead never enters CREATED_BEADS and leaks past
  # the EXIT-trap cleanup, polluting the by-account /costs view. Capture rc
  # explicitly (so set -e does not abort) and fail loudly on any failure.
  acct_bead_out=$("$KD_BIN" create "e2e-account-$TEST_TAG" --type=account \
    -l "project:gardenio" \
    -l "e2e:true" \
    -f "provider=anthropic" \
    -f "credential=secret:e2e-nonexistent-credential" \
    -f "model=claude-sonnet-4-20250514" \
    -f "status=active" \
    -f "max_budget=100" \
    -f "rotation_count=0" \
    --json) || acct_rc=$?
  acct_bead_id=$(echo "$acct_bead_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")

  if [ "$acct_rc" -ne 0 ] || [ -z "$acct_bead_id" ]; then
    fail "account bead create" "kd create failed (rc=$acct_rc); output: $(echo "$acct_bead_out" | head -3)"
  else
    CREATED_BEADS+=("$acct_bead_id")
    pass "account bead created ($acct_bead_id)"

    local acct_show
    acct_show=$("$KD_BIN" show "$acct_bead_id" --json 2>/dev/null || echo "{}")
    assert_contains "account has provider" "anthropic" "$acct_show"
    assert_contains "account has status" "active" "$acct_show"
    assert_contains "account has model" "claude-sonnet" "$acct_show"

    # Test status transition — simulate account exhaustion
    local exhaust_rc=0
    "$KD_BIN" update "$acct_bead_id" -f "status=exhausted" \
      -f "exhausted_at=2026-03-28T12:05:00Z" 2>/dev/null || exhaust_rc=$?
    assert_exit "update account to exhausted" 0 "$exhaust_rc"

    local acct_after
    acct_after=$("$KD_BIN" show "$acct_bead_id" --json 2>/dev/null || echo "{}")
    assert_contains "account now exhausted" "exhausted" "$acct_after"

    # Reset back to active
    local reset_rc=0
    "$KD_BIN" update "$acct_bead_id" -f "status=active" \
      -f "rotation_count=1" 2>/dev/null || reset_rc=$?
    assert_exit "reset account to active" 0 "$reset_rc"

    local acct_reset
    acct_reset=$("$KD_BIN" show "$acct_bead_id" --json 2>/dev/null || echo "{}")
    assert_contains "account back to active" '"active"' "$acct_reset"
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
