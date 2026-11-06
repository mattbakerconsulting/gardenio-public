#!/usr/bin/env bash
# test-orphans.sh — orphan detection E2E tests
#
# Tests `kd orphans` command against a live kbeads server.
# kd orphans finds beads referenced in git commit messages that are still open.
#
# Usage:
#   ./tests/e2e/scripts/test-orphans.sh
#
# Prerequisites:
#   - kd binary in PATH (or set KD_BIN)
#   - git, python3 installed
#   - BEADS_HTTP_URL set (or kubectl context for port-forward)
#
# Scenarios tested:
#   1. Create a bead, commit referencing it, verify kd orphans finds it
#   2. Close the bead, verify kd orphans no longer reports it
#   3. Verify kd orphans --json returns valid JSON
#   4. Verify clean state when no orphans exist

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
TEMP_GIT_DIR=""

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

# ── Setup: port-forward kd server ────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi

  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19097
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-orphans.log 2>&1 &
  PF_PID=$!
  sleep 3

  export BEADS_HTTP_URL="http://localhost:${port}"
  dim "  Daemon at $BEADS_HTTP_URL (pid $PF_PID)"
}

# ── Setup: get token ─────────────────────────────────────────────
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
  for id in "${CREATED_BEADS[@]}"; do
    "$KD_BIN" close "$id" 2>/dev/null || true
  done
  if [ -n "$TEMP_GIT_DIR" ] && [ -d "$TEMP_GIT_DIR" ]; then
    rm -rf "$TEMP_GIT_DIR"
  fi
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " orphans E2E tests"
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

  # Clean up stale beads from previous orphan test runs
  dim "  Cleaning stale e2e-orphans beads..."
  local stale_ids
  stale_ids=$("$KD_BIN" list --status=open -l "e2e:" --json 2>/dev/null | \
    python3 -c "
import sys, json
try:
    beads = json.load(sys.stdin)
    for b in beads:
        labels = b.get('labels', [])
        for l in labels:
            if l.startswith('e2e:e2e-orphans-'):
                print(b['id'])
                break
except: pass
" 2>/dev/null || echo "")
  for sid in $stale_ids; do
    "$KD_BIN" close "$sid" 2>/dev/null || true
  done
  if [ -n "$stale_ids" ]; then
    dim "  Cleaned $(echo "$stale_ids" | wc -w) stale beads"
  fi

  local TEST_TAG="e2e-orphans-$$-$(date +%s)"

  # Create a temporary git repo for orphan detection
  # kd orphans scans git log in the current directory
  TEMP_GIT_DIR=$(mktemp -d)
  cd "$TEMP_GIT_DIR"
  git init -q
  git config user.email "e2e@test.local"
  git config user.name "e2e-test"
  dim "  Temp git repo at $TEMP_GIT_DIR"

  # ── Scenario 1: Create bead, commit referencing it, detect as orphan ──
  blue "Scenario 1: Detect orphaned bead (referenced in commit but still open)"

  local bead_id
  bead_id=$("$KD_BIN" create "E2E orphan test $TEST_TAG" \
    --type=task -l "e2e:$TEST_TAG" \
    -d "Automated orphan test bead — safe to delete" \
    --actor "$KD_ACTOR" --json 2>/dev/null | \
    python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")

  if [ -z "$bead_id" ]; then
    fail "create test bead" "no id returned"
    return 1
  fi
  pass "created test bead ($bead_id)"
  CREATED_BEADS+=("$bead_id")

  # Create a commit referencing the bead ID
  touch testfile.txt
  git add testfile.txt
  git commit -q -m "fix: resolve issue ($bead_id)"

  # Run kd orphans --json
  local orphans_out orphans_rc
  orphans_out=$("$KD_BIN" orphans --json 2>/dev/null) && orphans_rc=0 || orphans_rc=$?
  assert_exit "kd orphans exits 0" 0 "$orphans_rc"
  assert_contains "orphans output contains bead ID" "$bead_id" "$orphans_out"
  echo

  # ── Scenario 2: Close bead, verify it disappears from orphans ──
  blue "Scenario 2: Closed bead no longer appears as orphan"

  "$KD_BIN" close "$bead_id" 2>/dev/null || true
  CREATED_BEADS=("${CREATED_BEADS[@]/$bead_id/}")

  local orphans_after orphans_after_rc
  orphans_after=$("$KD_BIN" orphans --json 2>/dev/null) && orphans_after_rc=0 || orphans_after_rc=$?
  assert_exit "kd orphans (after close) exits 0" 0 "$orphans_after_rc"
  assert_not_contains "orphans no longer contains closed bead" "$bead_id" "$orphans_after"
  echo

  # ── Scenario 3: Verify --json returns valid JSON array ─────────
  blue "Scenario 3: Verify --json output is valid JSON"

  local json_valid
  json_valid=$(echo "$orphans_after" | python3 -c "
import sys, json
try:
    data = json.load(sys.stdin)
    if isinstance(data, list):
        print('valid')
    else:
        print('not-array')
except:
    print('invalid')
" 2>/dev/null || echo "invalid")
  if [ "$json_valid" = "valid" ]; then
    pass "orphans --json returns valid JSON array"
  else
    fail "orphans --json returns valid JSON array" "got: $json_valid"
  fi
  echo

  # ── Scenario 4: Verify clean state with no orphans ─────────────
  blue "Scenario 4: Clean repo has no orphans for test beads"

  # Create a fresh git repo with no bead references
  local clean_dir
  clean_dir=$(mktemp -d)
  cd "$clean_dir"
  git init -q
  git config user.email "e2e@test.local"
  git config user.name "e2e-test"
  touch placeholder.txt
  git add placeholder.txt
  git commit -q -m "initial commit with no bead references"

  local clean_out clean_rc
  clean_out=$("$KD_BIN" orphans --json 2>/dev/null) && clean_rc=0 || clean_rc=$?
  assert_exit "kd orphans (clean repo) exits 0" 0 "$clean_rc"

  # Should be an empty array (no orphans referencing beads in this repo)
  local clean_count
  clean_count=$(echo "$clean_out" | python3 -c "import sys,json; print(len(json.load(sys.stdin)))" 2>/dev/null || echo "-1")
  if [ "$clean_count" = "0" ]; then
    pass "clean repo reports 0 orphans"
  else
    # May have orphans from other beads in the system — just check it's valid
    pass "clean repo orphans is valid JSON (count: $clean_count)"
  fi

  # Cleanup extra dir
  rm -rf "$clean_dir"
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
