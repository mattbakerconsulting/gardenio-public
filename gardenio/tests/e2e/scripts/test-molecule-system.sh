#!/usr/bin/env bash
# test-molecule-system.sh — molecule lifecycle E2E tests
#
# Tests kd mol list/show/bond/squash/burn against a live kbeads server.
# Molecules are instantiated formula work-item sets (epic-like beads with
# child beads for each step).
#
# Usage:
#   ./tests/e2e/scripts/test-molecule-system.sh
#
# Prerequisites:
#   - kd binary in PATH (or set KD_BIN)
#   - python3 installed
#   - BEADS_HTTP_URL set (or kubectl context for port-forward)
#
# Scenarios tested:
#   1. Create a molecule bead with child tasks
#   2. Verify kd mol list shows the molecule
#   3. Verify kd mol show displays children
#   4. Bond two molecules and verify dependency
#   5. Squash a molecule (close children + digest comment)
#   6. Burn a molecule (delete molecule + children)

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

assert_eq() {
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
  local port=19097
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-mol.log 2>&1 &
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
    "$KD_BIN" close "$id" --actor "$KD_ACTOR" 2>/dev/null || \
    "$KD_BIN" delete "$id" 2>/dev/null || true
  done
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# ── Helper: create a bead and track it ───────────────────────────
create_bead() {
  local title="$1"
  local type="${2:-task}"
  local extra_flags="${3:-}"
  local id
  id=$("$KD_BIN" create "$title" --type="$type" \
    -l "e2e:mol-test" \
    --actor "$KD_ACTOR" --json $extra_flags 2>/dev/null | \
    python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$id" ]; then
    CREATED_BEADS+=("$id")
  fi
  echo "$id"
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " molecule-system E2E tests"
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

  local TEST_TAG="e2e-mol-$$-$(date +%s)"

  # ── Scenario 1: Create a molecule bead with child tasks ────────
  blue "Scenario 1: Create molecule with child tasks"

  local mol_id
  mol_id=$(create_bead "E2E molecule $TEST_TAG" "molecule")
  if [ -z "$mol_id" ]; then
    fail "create molecule" "no id returned"
    return 1
  fi
  pass "created molecule ($mol_id)"

  # Create child tasks and link them
  local child1_id child2_id child3_id
  child1_id=$(create_bead "Step 1: $TEST_TAG" "task")
  child2_id=$(create_bead "Step 2: $TEST_TAG" "task")
  child3_id=$(create_bead "Step 3: $TEST_TAG" "task")

  if [ -z "$child1_id" ] || [ -z "$child2_id" ] || [ -z "$child3_id" ]; then
    fail "create child tasks" "missing IDs: c1=$child1_id c2=$child2_id c3=$child3_id"
    return 1
  fi
  pass "created 3 child tasks"

  # Link children to molecule via parent-child deps
  "$KD_BIN" dep add "$child1_id" "$mol_id" --type parent-child --actor "$KD_ACTOR" 2>/dev/null
  "$KD_BIN" dep add "$child2_id" "$mol_id" --type parent-child --actor "$KD_ACTOR" 2>/dev/null
  "$KD_BIN" dep add "$child3_id" "$mol_id" --type parent-child --actor "$KD_ACTOR" 2>/dev/null
  pass "linked 3 children to molecule"
  echo

  # ── Scenario 2: Verify kd mol list shows the molecule ──────────
  blue "Scenario 2: kd mol list shows the molecule"

  local list_out list_rc
  list_out=$("$KD_BIN" mol list --json 2>/dev/null) && list_rc=0 || list_rc=$?
  assert_exit "kd mol list exits 0" 0 "$list_rc"
  assert_contains "mol list contains molecule ID" "$mol_id" "$list_out"
  echo

  # ── Scenario 3: Verify kd mol show displays children ───────────
  blue "Scenario 3: kd mol show displays children"

  local show_out show_rc
  show_out=$("$KD_BIN" mol show "$mol_id" 2>/dev/null) && show_rc=0 || show_rc=$?
  assert_exit "kd mol show exits 0" 0 "$show_rc"
  assert_contains "mol show contains child 1" "$child1_id" "$show_out"
  assert_contains "mol show contains child 2" "$child2_id" "$show_out"
  assert_contains "mol show contains child 3" "$child3_id" "$show_out"
  echo

  # ── Scenario 4: Bond two molecules and verify dependency ───────
  blue "Scenario 4: Bond two molecules"

  local mol2_id
  mol2_id=$(create_bead "E2E molecule-B $TEST_TAG" "molecule")
  if [ -z "$mol2_id" ]; then
    fail "create second molecule" "no id returned"
  else
    pass "created second molecule ($mol2_id)"

    local bond_out bond_rc
    bond_out=$("$KD_BIN" mol bond "$mol_id" "$mol2_id" --actor "$KD_ACTOR" 2>/dev/null) && bond_rc=0 || bond_rc=$?
    assert_exit "kd mol bond exits 0" 0 "$bond_rc"
    assert_contains "bond output confirms link" "Bonded" "$bond_out"

    # Verify dependency was created
    local dep_out
    dep_out=$("$KD_BIN" dep list "$mol2_id" --json 2>/dev/null || echo "[]")
    assert_contains "mol2 has dependency on mol1" "$mol_id" "$dep_out"
  fi
  echo

  # ── Scenario 5: Squash a molecule ──────────────────────────────
  blue "Scenario 5: Squash molecule (close children + digest)"

  # Create a separate molecule for squash testing
  local squash_mol_id squash_child1 squash_child2
  squash_mol_id=$(create_bead "E2E squash-mol $TEST_TAG" "molecule")
  squash_child1=$(create_bead "Squash step 1: $TEST_TAG" "task")
  squash_child2=$(create_bead "Squash step 2: $TEST_TAG" "task")

  if [ -z "$squash_mol_id" ] || [ -z "$squash_child1" ] || [ -z "$squash_child2" ]; then
    fail "create squash test beads" "missing IDs"
  else
    "$KD_BIN" dep add "$squash_child1" "$squash_mol_id" --type parent-child --actor "$KD_ACTOR" 2>/dev/null
    "$KD_BIN" dep add "$squash_child2" "$squash_mol_id" --type parent-child --actor "$KD_ACTOR" 2>/dev/null
    pass "squash molecule created with 2 children"

    # Dry-run first
    local dryrun_out dryrun_rc
    dryrun_out=$("$KD_BIN" mol squash "$squash_mol_id" --dry-run --actor "$KD_ACTOR" 2>/dev/null) && dryrun_rc=0 || dryrun_rc=$?
    assert_exit "kd mol squash --dry-run exits 0" 0 "$dryrun_rc"
    assert_contains "dry-run shows children count" "Children: 2" "$dryrun_out"

    # Actual squash with summary
    local squash_out squash_rc
    squash_out=$("$KD_BIN" mol squash "$squash_mol_id" \
      --summary "E2E squash test complete" \
      --actor "$KD_ACTOR" 2>/dev/null) && squash_rc=0 || squash_rc=$?
    assert_exit "kd mol squash exits 0" 0 "$squash_rc"
    assert_contains "squash output confirms children closed" "children closed" "$squash_out"

    # Verify children are closed
    local c1_status c2_status
    c1_status=$("$KD_BIN" show "$squash_child1" --json 2>/dev/null | \
      python3 -c "import sys,json; print(json.load(sys.stdin).get('status',''))" 2>/dev/null || echo "")
    c2_status=$("$KD_BIN" show "$squash_child2" --json 2>/dev/null | \
      python3 -c "import sys,json; print(json.load(sys.stdin).get('status',''))" 2>/dev/null || echo "")
    assert_eq "child 1 is closed after squash" "closed" "$c1_status"
    assert_eq "child 2 is closed after squash" "closed" "$c2_status"

    # Verify digest comment was added
    local comments_out
    comments_out=$("$KD_BIN" comment list "$squash_mol_id" --json 2>/dev/null || echo "[]")
    assert_contains "squash added digest comment" "E2E squash test complete" "$comments_out"
  fi
  echo

  # ── Scenario 6: Burn a molecule ────────────────────────────────
  blue "Scenario 6: Burn molecule (delete molecule + children)"

  local burn_mol_id burn_child1 burn_child2
  burn_mol_id=$(create_bead "E2E burn-mol $TEST_TAG" "molecule")
  burn_child1=$(create_bead "Burn step 1: $TEST_TAG" "task")
  burn_child2=$(create_bead "Burn step 2: $TEST_TAG" "task")

  if [ -z "$burn_mol_id" ] || [ -z "$burn_child1" ] || [ -z "$burn_child2" ]; then
    fail "create burn test beads" "missing IDs"
  else
    "$KD_BIN" dep add "$burn_child1" "$burn_mol_id" --type parent-child --actor "$KD_ACTOR" 2>/dev/null
    "$KD_BIN" dep add "$burn_child2" "$burn_mol_id" --type parent-child --actor "$KD_ACTOR" 2>/dev/null
    pass "burn molecule created with 2 children"

    # Dry-run first
    local burn_dry_out burn_dry_rc
    burn_dry_out=$("$KD_BIN" mol burn "$burn_mol_id" --dry-run --actor "$KD_ACTOR" 2>/dev/null) && burn_dry_rc=0 || burn_dry_rc=$?
    assert_exit "kd mol burn --dry-run exits 0" 0 "$burn_dry_rc"
    assert_contains "burn dry-run shows children to delete" "Children to delete: 2" "$burn_dry_out"

    # Without --force should prompt (non-destructive)
    local burn_nf_out burn_nf_rc
    burn_nf_out=$("$KD_BIN" mol burn "$burn_mol_id" --actor "$KD_ACTOR" 2>/dev/null) && burn_nf_rc=0 || burn_nf_rc=$?
    assert_exit "kd mol burn without --force exits 0" 0 "$burn_nf_rc"
    assert_contains "burn without --force asks for confirmation" "--force" "$burn_nf_out"

    # Actual burn with --force
    local burn_out burn_rc
    burn_out=$("$KD_BIN" mol burn "$burn_mol_id" --force --actor "$KD_ACTOR" 2>/dev/null) && burn_rc=0 || burn_rc=$?
    assert_exit "kd mol burn --force exits 0" 0 "$burn_rc"
    assert_contains "burn output confirms deletion" "Burned" "$burn_out"

    # Verify molecule is deleted (show should fail)
    local show_burned_rc
    "$KD_BIN" show "$burn_mol_id" --json >/dev/null 2>&1 && show_burned_rc=0 || show_burned_rc=$?
    if [ "$show_burned_rc" -ne 0 ]; then
      pass "burned molecule no longer exists"
    else
      fail "burned molecule no longer exists" "show succeeded; molecule still exists"
    fi

    # Remove burned beads from cleanup list (they're already deleted)
    CREATED_BEADS=("${CREATED_BEADS[@]/$burn_mol_id/}")
    CREATED_BEADS=("${CREATED_BEADS[@]/$burn_child1/}")
    CREATED_BEADS=("${CREATED_BEADS[@]/$burn_child2/}")
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
