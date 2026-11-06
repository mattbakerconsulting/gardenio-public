#!/usr/bin/env bash
# test-labels.sh — label system E2E tests
#
# Tests kd label add/remove and label-filtered queries against a live kbeads server.
#
# Scenarios tested:
#   1. Create bead with labels
#   2. Add label to existing bead
#   3. Remove label from bead
#   4. Filtered list (single label)
#   5. Filtered list (multiple labels — AND)
#   6. Labels present in JSON output

set -euo pipefail

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

# Load shared retry helpers (kd_retry, retry_on_empty) for transient PF drops.
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
  shift
  local out
  out=$(kd_retry "$KD_BIN" create "$title" --type=task "$@" --json 2>/dev/null)
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
  local port=19095
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-labels.log 2>&1 &
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
  blue " labels E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  if ! kd_retry "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  local TEST_TAG="e2e-labels-$$-$(date +%s)"

  # ── Scenario 1: Create with labels ─────────────────────────────
  blue "Scenario 1: Create bead with labels"
  BEAD1=$(create_task "label-test-1-$TEST_TAG" -l "e2e:$TEST_TAG" -l "env:staging")
  if [ -z "$BEAD1" ]; then
    fail "scenario-1-create" "Could not create bead"
  else
    dim "  Bead: $BEAD1"
    local show_out
    show_out=$(kd_retry "$KD_BIN" show "$BEAD1" --json 2>/dev/null || echo "{}")
    assert_contains "bead has e2e label" "e2e:$TEST_TAG" "$show_out"
    assert_contains "bead has env label" "env:staging" "$show_out"
  fi
  echo

  # ── Scenario 2: Add label ──────────────────────────────────────
  blue "Scenario 2: Add label to existing bead"
  if [ -n "$BEAD1" ]; then
    local add_rc
    kd_retry "$KD_BIN" label add "$BEAD1" "team:platform" 2>/dev/null && add_rc=0 || add_rc=$?
    assert_exit "kd label add" 0 "$add_rc"

    show_out=$(kd_retry "$KD_BIN" show "$BEAD1" --json 2>/dev/null || echo "{}")
    assert_contains "added label present" "team:platform" "$show_out"
    assert_contains "original label still present" "e2e:$TEST_TAG" "$show_out"
  fi
  echo

  # ── Scenario 3: Remove label ───────────────────────────────────
  blue "Scenario 3: Remove label from bead"
  if [ -n "$BEAD1" ]; then
    # Removing a label the bead never carried must NOT report success: it
    # matches no row and has to exit non-zero, leaving real labels intact
    # (ka-0000000424). Run this BEFORE the real remove, and WITHOUT kd_retry —
    # retrying an intentionally-failing command just burns
    # RETRY_ATTEMPTS*RETRY_DELAY seconds before returning the same code.
    local absent_rc
    "$KD_BIN" label remove "$BEAD1" "not-a-real-label-$$" >/dev/null 2>&1 && absent_rc=0 || absent_rc=$?
    assert_exit "kd label remove (absent label) is not success" 1 "$absent_rc"
    show_out=$(kd_retry "$KD_BIN" show "$BEAD1" --json 2>/dev/null || echo "{}")
    assert_contains "real labels survive an absent-label remove" "env:staging" "$show_out"

    local rm_rc
    kd_retry "$KD_BIN" label remove "$BEAD1" "env:staging" 2>/dev/null && rm_rc=0 || rm_rc=$?
    assert_exit "kd label remove" 0 "$rm_rc"

    show_out=$(kd_retry "$KD_BIN" show "$BEAD1" --json 2>/dev/null || echo "{}")
    assert_not_contains "removed label gone" "env:staging" "$show_out"
    assert_contains "other labels still present" "team:platform" "$show_out"

    # And --if-exists makes the now-absent label a clean exit 0 (idempotent path).
    local again_rc
    "$KD_BIN" label remove "$BEAD1" "env:staging" --if-exists >/dev/null 2>&1 && again_rc=0 || again_rc=$?
    assert_exit "kd label remove --if-exists on an absent label" 0 "$again_rc"
  fi
  echo

  # ── Scenario 4: Filtered list (single label) ───────────────────
  blue "Scenario 4: Filtered list with single label"
  local UNIQUE_A="e2e:labelA-$TEST_TAG"
  local UNIQUE_B="e2e:labelB-$TEST_TAG"
  BEAD_A=$(create_task "label-A-$TEST_TAG" -l "$UNIQUE_A")
  BEAD_B=$(create_task "label-B-$TEST_TAG" -l "$UNIQUE_B")
  if [ -z "$BEAD_A" ] || [ -z "$BEAD_B" ]; then
    fail "scenario-4-setup" "Could not create beads"
  else
    dim "  A: $BEAD_A ($UNIQUE_A), B: $BEAD_B ($UNIQUE_B)"
    local list_out
    list_out=$(kd_retry "$KD_BIN" list -l "$UNIQUE_A" --json 2>/dev/null || echo "[]")
    assert_contains "filtered list contains A" "$BEAD_A" "$list_out"
    assert_not_contains "filtered list excludes B" "$BEAD_B" "$list_out"
  fi
  echo

  # ── Scenario 5: Filtered list (multiple labels — AND) ──────────
  blue "Scenario 5: Filtered list with multiple labels (AND)"
  local SHARED="e2e:shared-$TEST_TAG"
  local EXTRA="e2e:extra-$TEST_TAG"
  BEAD_BOTH=$(create_task "label-both-$TEST_TAG" -l "$SHARED" -l "$EXTRA")
  BEAD_ONE=$(create_task "label-one-$TEST_TAG" -l "$SHARED")
  if [ -z "$BEAD_BOTH" ] || [ -z "$BEAD_ONE" ]; then
    fail "scenario-5-setup" "Could not create beads"
  else
    dim "  Both: $BEAD_BOTH, One: $BEAD_ONE"
    list_out=$(kd_retry "$KD_BIN" list -l "$SHARED" -l "$EXTRA" --json 2>/dev/null || echo "[]")
    assert_contains "AND filter contains bead with both labels" "$BEAD_BOTH" "$list_out"
    assert_not_contains "AND filter excludes bead with only one label" "$BEAD_ONE" "$list_out"
  fi
  echo

  # ── Scenario 6: Labels in JSON output ──────────────────────────
  blue "Scenario 6: Labels array in JSON output"
  if [ -n "$BEAD_BOTH" ]; then
    show_out=$(kd_retry "$KD_BIN" show "$BEAD_BOTH" --json 2>/dev/null || echo "{}")
    # Verify labels field is a JSON array containing our labels
    local labels_count
    labels_count=$(echo "$show_out" | python3 -c "
import sys, json
d = json.load(sys.stdin)
labels = d.get('labels', [])
count = sum(1 for l in labels if 'e2e:' in l)
print(count)
" 2>/dev/null || echo "0")
    if [ "$labels_count" -ge 2 ]; then
      pass "labels array has at least 2 e2e labels (got $labels_count)"
    else
      fail "labels array" "expected ≥2 e2e labels, got $labels_count"
    fi
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
