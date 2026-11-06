#!/usr/bin/env bash
# test-epics-children.sh — epics & parent-child E2E tests
#
# Tests epic bead creation and parent-child dependency workflows.
#
# Scenarios tested:
#   1. Create an epic bead
#   2. Create child tasks and link via parent-child dep
#   3. Children reverse lookup (kd children)
#   4. Dependency tree traversal
#   5. Close children then epic
#   6. Epic with mixed child types (task + bug)

set -euo pipefail

# shellcheck disable=SC1090
source "$(dirname "${BASH_SOURCE[0]}")/lib-retry.sh"

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
KD_BIN="${KD_BIN:-kd}"
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

create_bead() {
  local title="$1"
  local type="$2"
  shift 2
  local out
  out=$(kd_retry "$KD_BIN" create "$title" --type="$type" -l "e2e:epics-$$" -l "project:e2e-test" "$@" --json 2>/dev/null)
  local id
  id=$(echo "$out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$id" ]; then
    CREATED_BEADS+=("$id")
  fi
  echo "$id"
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
    >/tmp/kd-pf-epics.log 2>&1 &
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
    kd_retry "$KD_BIN" close "$id" 2>/dev/null || true
  done
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " epics & children E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  if ! kd_retry "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL after $RETRY_ATTEMPTS attempts"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  local TEST_TAG="e2e-epics-$$-$(date +%s)"

  # ── Scenario 1: Create an epic ─────────────────────────────────
  blue "Scenario 1: Create an epic bead"
  EPIC=$(create_bead "E2E test epic $TEST_TAG" "epic")
  if [ -z "$EPIC" ]; then
    fail "scenario-1-create" "Could not create epic"
    red "Cannot continue without epic bead"
    return 1
  fi
  dim "  Epic: $EPIC"
  local show_out
  show_out=$(kd_retry "$KD_BIN" show "$EPIC" --json 2>/dev/null || echo "{}")
  assert_contains "epic has type epic" '"epic"' "$show_out"
  echo

  # ── Scenario 2: Create child tasks ─────────────────────────────
  blue "Scenario 2: Create child tasks linked to epic"
  CHILD1=$(create_bead "E2E child-1 $TEST_TAG" "task")
  CHILD2=$(create_bead "E2E child-2 $TEST_TAG" "task")
  CHILD3=$(create_bead "E2E child-3 $TEST_TAG" "task")
  if [ -z "$CHILD1" ] || [ -z "$CHILD2" ] || [ -z "$CHILD3" ]; then
    fail "scenario-2-setup" "Could not create all 3 children"
  else
    dim "  Children: $CHILD1, $CHILD2, $CHILD3"
    for cid in "$CHILD1" "$CHILD2" "$CHILD3"; do
      local dep_rc
      kd_retry "$KD_BIN" dep add "$cid" "$EPIC" --type parent-child 2>/dev/null && dep_rc=0 || dep_rc=$?
      assert_exit "dep add $cid → $EPIC" 0 "$dep_rc"
    done

    # Verify deps
    local dep_out
    dep_out=$(kd_retry "$KD_BIN" dep list "$CHILD1" --json 2>/dev/null || echo "[]")
    assert_contains "child1 dep shows epic" "$EPIC" "$dep_out"
    assert_contains "child1 dep shows parent-child" "parent-child" "$dep_out"
  fi
  echo

  # ── Scenario 3: Children reverse lookup ─────────────────────────
  blue "Scenario 3: kd children reverse lookup"
  local children_out children_rc
  children_out=$(kd_retry "$KD_BIN" children "$EPIC" --json 2>/dev/null || echo "[]")
  children_rc=$?
  assert_exit "kd children exits 0" 0 "$children_rc"

  if [ -n "$CHILD1" ]; then
    assert_contains "children contains child1" "$CHILD1" "$children_out"
  fi
  if [ -n "$CHILD2" ]; then
    assert_contains "children contains child2" "$CHILD2" "$children_out"
  fi
  if [ -n "$CHILD3" ]; then
    assert_contains "children contains child3" "$CHILD3" "$children_out"
  fi
  echo

  # ── Scenario 4: Dependency tree ─────────────────────────────────
  blue "Scenario 4: Dependency tree traversal"
  if [ -n "$CHILD1" ]; then
    local tree_out tree_rc
    tree_out=$(kd_retry "$KD_BIN" tree "$CHILD1" 2>/dev/null || echo "")
    tree_rc=$?
    assert_exit "kd tree exits 0" 0 "$tree_rc"
    assert_contains "tree shows epic" "$EPIC" "$tree_out"
  fi
  echo

  # ── Scenario 5: Close children then epic ────────────────────────
  blue "Scenario 5: Close children then epic"
  for cid in "$CHILD1" "$CHILD2" "$CHILD3"; do
    if [ -n "$cid" ]; then
      kd_retry "$KD_BIN" close "$cid" 2>/dev/null
      CREATED_BEADS=("${CREATED_BEADS[@]/$cid/}")
    fi
  done

  # Verify children are closed
  for cid in "$CHILD1" "$CHILD2" "$CHILD3"; do
    if [ -n "$cid" ]; then
      local cstatus
      cstatus=$(kd_retry "$KD_BIN" show "$cid" --json 2>/dev/null || echo "{}")
      assert_contains "child $cid is closed" '"closed"' "$cstatus"
    fi
  done

  # Close the epic
  kd_retry "$KD_BIN" close "$EPIC" 2>/dev/null
  CREATED_BEADS=("${CREATED_BEADS[@]/$EPIC/}")
  local epic_status
  epic_status=$(kd_retry "$KD_BIN" show "$EPIC" --json 2>/dev/null || echo "{}")
  assert_contains "epic is closed" '"closed"' "$epic_status"
  echo

  # ── Scenario 6: Epic with mixed child types ─────────────────────
  blue "Scenario 6: Epic with mixed child types (task + bug)"
  EPIC2=$(create_bead "E2E mixed-epic $TEST_TAG" "epic")
  BUG_CHILD=$(create_bead "E2E bug-child $TEST_TAG" "bug")
  TASK_CHILD=$(create_bead "E2E task-child $TEST_TAG" "task")
  if [ -z "$EPIC2" ] || [ -z "$BUG_CHILD" ] || [ -z "$TASK_CHILD" ]; then
    fail "scenario-6-setup" "Could not create beads"
  else
    dim "  Epic: $EPIC2, Bug: $BUG_CHILD, Task: $TASK_CHILD"
    kd_retry "$KD_BIN" dep add "$BUG_CHILD" "$EPIC2" --type parent-child 2>/dev/null
    kd_retry "$KD_BIN" dep add "$TASK_CHILD" "$EPIC2" --type parent-child 2>/dev/null

    local mixed_children
    mixed_children=$(kd_retry "$KD_BIN" children "$EPIC2" --json 2>/dev/null || echo "[]")
    assert_contains "mixed children contains bug" "$BUG_CHILD" "$mixed_children"
    assert_contains "mixed children contains task" "$TASK_CHILD" "$mixed_children"
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
