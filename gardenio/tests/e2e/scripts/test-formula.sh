#!/usr/bin/env bash
# test-formula.sh — kd formula E2E tests
#
# Tests formula create, list, show, dump, and apply against a live kd server.
#
# Scenarios tested:
#   1. kd formula create from JSON definition
#   2. kd formula list includes created formula
#   3. kd formula show displays vars and steps
#   4. kd formula dump returns valid JSON
#   5. kd formula apply creates a molecule of beads
#   6. Applied beads have substituted values

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
KD_BIN="${KD_BIN:-kd}"
PASS=0
FAIL=0
ERRORS=()
PF_PID=""
CLEANUP_BEADS=()
FORMULA_ID=""

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
  local port=19101
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-formula.log 2>&1 &
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
  for id in "${CLEANUP_BEADS[@]}"; do
    "$KD_BIN" close "$id" 2>/dev/null || true
  done
  if [ -n "$FORMULA_ID" ]; then
    "$KD_BIN" close "$FORMULA_ID" 2>/dev/null || true
  fi
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " formula E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  local TEST_TAG="e2e-formula-$$-$(date +%s)"

  # ── Scenario 1: kd formula create ─────────────────────────────
  blue "Scenario 1: kd formula create from JSON definition"

  local formula_json
  formula_json=$(cat <<FJSON
{
  "vars": [
    {"name": "component", "required": true},
    {"name": "priority", "default": "2"}
  ],
  "steps": [
    {
      "id": "fix",
      "title": "Fix {{component}} issue",
      "type": "task",
      "description": "Fix the reported issue in {{component}}",
      "labels": ["project:e2e-test", "component:{{component}}"]
    },
    {
      "id": "test",
      "title": "Test {{component}} fix",
      "type": "task",
      "description": "Verify the fix for {{component}} works correctly",
      "depends_on": ["fix"],
      "labels": ["project:e2e-test", "component:{{component}}"]
    }
  ]
}
FJSON
)

  local create_out create_rc=0
  create_out=$(echo "$formula_json" | "$KD_BIN" formula create "E2E test formula $TEST_TAG" --file - --json 2>/dev/null) || create_rc=$?
  assert_exit "kd formula create exits 0" 0 "$create_rc"

  FORMULA_ID=$(echo "$create_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -z "$FORMULA_ID" ]; then
    # Try grep fallback
    FORMULA_ID=$(echo "$create_out" | grep -oE '(gd|ka)-[A-Za-z0-9]+' | head -1 || echo "")
  fi

  if [ -n "$FORMULA_ID" ]; then
    pass "formula created ($FORMULA_ID)"
  else
    fail "formula created" "no id in output: $create_out"
  fi
  echo

  # ── Scenario 2: kd formula list ───────────────────────────────
  blue "Scenario 2: kd formula list includes created formula"
  if [ -n "$FORMULA_ID" ]; then
    local list_out list_rc=0
    list_out=$("$KD_BIN" formula list --all-projects 2>&1) || list_rc=$?
    assert_exit "kd formula list exits 0" 0 "$list_rc"
    assert_contains "formula list includes our formula" "$TEST_TAG" "$list_out"
  else
    fail "scenario 2 skipped" "no formula ID"
  fi
  echo

  # ── Scenario 3: kd formula show ───────────────────────────────
  blue "Scenario 3: kd formula show displays vars and steps"
  if [ -n "$FORMULA_ID" ]; then
    local show_out show_rc=0
    show_out=$("$KD_BIN" formula show "$FORMULA_ID" 2>&1) || show_rc=$?
    assert_exit "kd formula show exits 0" 0 "$show_rc"
    assert_contains "show includes var: component" "component" "$show_out"
    assert_contains "show includes var: priority" "priority" "$show_out"
    assert_contains "show includes step title pattern" "Fix" "$show_out"
  else
    fail "scenario 3 skipped" "no formula ID"
  fi
  echo

  # ── Scenario 4: kd formula dump ───────────────────────────────
  blue "Scenario 4: kd formula dump returns valid JSON"
  if [ -n "$FORMULA_ID" ]; then
    local dump_out dump_rc=0
    dump_out=$("$KD_BIN" formula dump "$FORMULA_ID" 2>/dev/null) || dump_rc=$?
    assert_exit "kd formula dump exits 0" 0 "$dump_rc"

    if [ -n "$dump_out" ]; then
      local valid
      valid=$(echo "$dump_out" | python3 -c "import sys,json; json.load(sys.stdin); print('valid')" 2>/dev/null || echo "invalid")
      if [ "$valid" = "valid" ]; then
        pass "formula dump is valid JSON"
      else
        fail "formula dump valid JSON" "output is not valid JSON"
      fi
    else
      fail "formula dump output" "empty output"
    fi
  else
    fail "scenario 4 skipped" "no formula ID"
  fi
  echo

  # ── Scenario 5: kd formula apply ──────────────────────────────
  blue "Scenario 5: kd formula apply creates molecule of beads"
  if [ -n "$FORMULA_ID" ]; then
    local apply_out apply_rc=0
    apply_out=$("$KD_BIN" formula apply "$FORMULA_ID" \
      --var "component=auth-service" \
      --var "priority=2" 2>&1) || apply_rc=$?
    assert_exit "kd formula apply exits 0" 0 "$apply_rc"

    # Extract created bead IDs
    local applied_ids
    applied_ids=$(echo "$apply_out" | grep -oE '(gd|ka)-[A-Za-z0-9]+' || echo "")
    local id_count
    id_count=$(echo "$applied_ids" | grep -cE '(gd|ka)-' || echo "0")

    if [ "$id_count" -ge 2 ]; then
      pass "formula apply created $id_count beads"
      # Add to cleanup
      for id in $applied_ids; do
        CLEANUP_BEADS+=("$id")
      done
    else
      fail "formula apply created beads" "expected >=2 beads, got $id_count; output: $(echo "$apply_out" | head -5)"
    fi
  else
    fail "scenario 5 skipped" "no formula ID"
  fi
  echo

  # ── Scenario 6: Applied beads have substituted values ─────────
  blue "Scenario 6: Applied beads have substituted variable values"
  if [ "${#CLEANUP_BEADS[@]}" -gt 1 ]; then
    # Find a step bead (type=task) among the created beads.
    # The apply output includes molecule, formula, and step IDs.
    local step_bead="" bead_show=""
    for check_id in "${CLEANUP_BEADS[@]}"; do
      local check_out
      check_out=$("$KD_BIN" show "$check_id" --json 2>/dev/null || echo "{}")
      local check_type
      check_type=$(echo "$check_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('type',''))" 2>/dev/null || echo "")
      if [ "$check_type" = "task" ]; then
        step_bead="$check_id"
        bead_show="$check_out"
        break
      fi
    done
    if [ -z "$step_bead" ]; then
      # Fallback: use last bead (most likely a step)
      step_bead="${CLEANUP_BEADS[-1]}"
      bead_show=$("$KD_BIN" show "$step_bead" --json 2>/dev/null || echo "{}")
    fi

    # Title should contain "auth-service" (substituted from {{component}})
    assert_contains "applied bead title has substituted value" "auth-service" "$bead_show"
    assert_contains "applied bead has project label" "project:e2e-test" "$bead_show"
  else
    fail "scenario 6 skipped" "no applied beads to check"
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
