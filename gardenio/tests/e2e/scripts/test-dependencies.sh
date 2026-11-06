#!/usr/bin/env bash
# test-dependencies.sh — dependency & blocking system E2E tests
#
# Tests kd dep add/remove/list and blocking behavior against a live kbeads server.
#
# Scenarios tested:
#   1. Create parent-child dependency
#   2. Create blocks dependency
#   3. Blocked bead excluded from gb ready
#   4. Closing blocker unblocks dependent
#   5. Remove dependency
#   6. Multiple blockers — all must close to unblock

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

# Load shared retry helpers so transient port-forward drops don't fail
# a whole scenario (kd_retry, retry_on_empty).
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib-retry.sh"

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

assert_not_contains() {
  local name="$1"
  local pattern="$2"
  local actual="$3"
  if ! grep -qF -- "$pattern" <<< "$actual"; then
    pass "$name (does not contain '$pattern')"
  else
    fail "$name" "expected output to NOT contain '$pattern'"
  fi
}

create_task() {
  local title="$1"
  local extra_args="${2:-}"
  local out
  # shellcheck disable=SC2086
  out=$(kd_retry "$KD_BIN" create "$title" --type=task -l "e2e:deps-$$" -l "project:e2e-test" $extra_args --json 2>/dev/null)
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
  local port=19094
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-deps.log 2>&1 &
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
  blue " dependencies E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  if ! kd_retry "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  # ── Scenario 1: Parent-child dependency ────────────────────────
  blue "Scenario 1: Create parent-child dependency"
  EPIC=$(create_task "e2e-deps-epic-$$" "--type epic")
  CHILD=$(create_task "e2e-deps-child-$$")
  if [ -z "$EPIC" ] || [ -z "$CHILD" ]; then
    fail "scenario-1-setup" "Could not create test beads"
  else
    dim "  Epic: $EPIC, Child: $CHILD"
    local dep_rc dep_out
    kd_retry "$KD_BIN" dep add "$CHILD" "$EPIC" --type parent-child 2>/dev/null && dep_rc=0 || dep_rc=$?
    assert_exit "kd dep add parent-child" 0 "$dep_rc"

    dep_out=$(kd_retry "$KD_BIN" dep list "$CHILD" --json 2>/dev/null || echo "[]")
    assert_contains "dep list shows parent" "$EPIC" "$dep_out"
    assert_contains "dep list shows parent-child type" "parent-child" "$dep_out"
  fi
  echo

  # ── Scenario 2: Blocks dependency ──────────────────────────────
  blue "Scenario 2: Create blocks dependency"
  BLOCKER=$(create_task "e2e-deps-blocker-$$")
  BLOCKED=$(create_task "e2e-deps-blocked-$$")
  if [ -z "$BLOCKER" ] || [ -z "$BLOCKED" ]; then
    fail "scenario-2-setup" "Could not create test beads"
  else
    dim "  Blocker: $BLOCKER, Blocked: $BLOCKED"
    kd_retry "$KD_BIN" dep add "$BLOCKED" "$BLOCKER" --type blocks 2>/dev/null && dep_rc=0 || dep_rc=$?
    assert_exit "kd dep add blocks" 0 "$dep_rc"

    dep_out=$(kd_retry "$KD_BIN" dep list "$BLOCKED" --json 2>/dev/null || echo "[]")
    assert_contains "dep list shows blocker" "$BLOCKER" "$dep_out"
    assert_contains "dep list shows blocks type" "blocks" "$dep_out"
  fi
  echo

  # ── Scenario 3: Blocked bead excluded from gb ready ────────────
  blue "Scenario 3: Blocked bead excluded from gb ready"
  if [ -n "$BLOCKER" ] && [ -n "$BLOCKED" ]; then
    local ready_out
    ready_out=$(kd_retry "$GB_BIN" ready --project e2e-test --json 2>/dev/null || echo "[]")
    assert_not_contains "blocked bead NOT in ready" "$BLOCKED" "$ready_out"
    assert_contains "blocker IS in ready" "$BLOCKER" "$ready_out"
  else
    fail "scenario-3" "Missing beads from scenario 2"
  fi
  echo

  # ── Scenario 4: Closing blocker unblocks dependent ─────────────
  blue "Scenario 4: Closing blocker unblocks dependent"
  if [ -n "$BLOCKER" ] && [ -n "$BLOCKED" ]; then
    kd_retry "$KD_BIN" close "$BLOCKER" 2>/dev/null
    CREATED_BEADS=("${CREATED_BEADS[@]/$BLOCKER/}")
    sleep 1  # allow server to process

    ready_out=$(kd_retry "$GB_BIN" ready --project e2e-test --json 2>/dev/null || echo "[]")
    assert_contains "unblocked bead now in ready" "$BLOCKED" "$ready_out"
  else
    fail "scenario-4" "Missing beads from scenario 2"
  fi
  echo

  # ── Scenario 5: Remove dependency ──────────────────────────────
  blue "Scenario 5: Remove dependency"
  DEP_A=$(create_task "e2e-deps-depA-$$")
  DEP_B=$(create_task "e2e-deps-depB-$$")
  if [ -z "$DEP_A" ] || [ -z "$DEP_B" ]; then
    fail "scenario-5-setup" "Could not create test beads"
  else
    dim "  A: $DEP_A, B: $DEP_B"
    kd_retry "$KD_BIN" dep add "$DEP_B" "$DEP_A" --type blocks 2>/dev/null

    # Verify dep exists
    dep_out=$(kd_retry "$KD_BIN" dep list "$DEP_B" --json 2>/dev/null || echo "[]")
    assert_contains "dep exists before remove" "$DEP_A" "$dep_out"

    # Wrong --type must NOT report success: the real edge is `blocks`, so a
    # parent-child remove matches no row and has to exit non-zero with the real
    # edge left intact (ka-0000001966). Run this BEFORE the real remove, and
    # WITHOUT kd_retry — retrying an intentionally-failing command just burns
    # RETRY_ATTEMPTS*RETRY_DELAY seconds before returning the same code.
    local wrong_rc
    "$KD_BIN" dep remove "$DEP_B" "$DEP_A" --type parent-child >/dev/null 2>&1 && wrong_rc=0 || wrong_rc=$?
    assert_exit "kd dep remove (wrong --type) is not success" 1 "$wrong_rc"
    dep_out=$(kd_retry "$KD_BIN" dep list "$DEP_B" --json 2>/dev/null || echo "[]")
    assert_contains "real edge survives a wrong-type remove" "$DEP_A" "$dep_out"

    # Remove it
    local rm_rc
    kd_retry "$KD_BIN" dep remove "$DEP_B" "$DEP_A" 2>/dev/null && rm_rc=0 || rm_rc=$?
    assert_exit "kd dep remove" 0 "$rm_rc"

    # Verify dep gone
    dep_out=$(kd_retry "$KD_BIN" dep list "$DEP_B" --json 2>/dev/null || echo "[]")
    assert_not_contains "dep gone after remove" "$DEP_A" "$dep_out"

    # And --if-exists makes the now-absent edge a clean exit 0 (idempotent path).
    local again_rc
    "$KD_BIN" dep remove "$DEP_B" "$DEP_A" --if-exists >/dev/null 2>&1 && again_rc=0 || again_rc=$?
    assert_exit "kd dep remove --if-exists on an absent edge" 0 "$again_rc"
  fi
  echo

  # ── Scenario 6: Multiple blockers ──────────────────────────────
  blue "Scenario 6: Multiple blockers — all must close to unblock"
  BLOCK1=$(create_task "e2e-deps-multi-block1-$$")
  BLOCK2=$(create_task "e2e-deps-multi-block2-$$")
  MULTI_BLOCKED=$(create_task "e2e-deps-multi-blocked-$$")
  if [ -z "$BLOCK1" ] || [ -z "$BLOCK2" ] || [ -z "$MULTI_BLOCKED" ]; then
    fail "scenario-6-setup" "Could not create test beads"
  else
    dim "  Blockers: $BLOCK1, $BLOCK2 → Blocked: $MULTI_BLOCKED"
    kd_retry "$KD_BIN" dep add "$MULTI_BLOCKED" "$BLOCK1" --type blocks 2>/dev/null
    kd_retry "$KD_BIN" dep add "$MULTI_BLOCKED" "$BLOCK2" --type blocks 2>/dev/null

    # Still blocked after closing only one
    kd_retry "$KD_BIN" close "$BLOCK1" 2>/dev/null
    CREATED_BEADS=("${CREATED_BEADS[@]/$BLOCK1/}")
    sleep 1

    ready_out=$(kd_retry "$GB_BIN" ready --project e2e-test --json 2>/dev/null || echo "[]")
    assert_not_contains "still blocked with 1 open blocker" "$MULTI_BLOCKED" "$ready_out"

    # Unblocked after closing both
    kd_retry "$KD_BIN" close "$BLOCK2" 2>/dev/null
    CREATED_BEADS=("${CREATED_BEADS[@]/$BLOCK2/}")
    sleep 1

    ready_out=$(kd_retry "$GB_BIN" ready --project e2e-test --json 2>/dev/null || echo "[]")
    assert_contains "unblocked after all blockers closed" "$MULTI_BLOCKED" "$ready_out"
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
