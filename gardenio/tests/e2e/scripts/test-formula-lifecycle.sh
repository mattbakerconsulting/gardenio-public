#!/usr/bin/env bash
# test-formula-lifecycle.sh — formula lifecycle E2E tests
#
# Tests advanced formula features beyond basic CRUD: dependency chains,
# conditions, dry-run preview, pour/wisp, formula update, formula dump
# round-trip, extends inheritance, mol bond types, and mol squash with
# --close-mol.
#
# Usage:
#   ./tests/e2e/scripts/test-formula-lifecycle.sh
#
# Prerequisites:
#   - kd binary in PATH (or set KD_BIN)
#   - python3 installed
#   - BEADS_HTTP_URL set (or kubectl context for port-forward)
#
# Scenarios tested:
#   1. depends_on creates inter-step "blocks" dependencies
#   2. Three-step dependency chain (build → test → deploy)
#   3. Conditional steps skipped when condition is false
#   4. Conditional steps created when condition is true
#   5. --dry-run previews without creating beads
#   6. Pour creates persistent molecule, wisp creates ephemeral
#   7. Formula update modifies definition
#   8. Formula dump → create round-trip preserves definition
#   9. Mol bond parallel creates "related" dependency
#  10. Mol squash --close-mol closes molecule and children
#  11. extends inheritance round-trips through formula update (ka-0000005592)
#  12. Pour-time dangling depends_on is rejected BEFORE any bead is created —
#      a step whose depends_on names an undeclared step aborts the pour instead
#      of silently dropping the gating edge and pouring an ungated molecule
#      (ka-0000002771; fix 92e072a48). This is the POUR path (kd formula
#      apply / pour), distinct from Scenario 11's `formula update`
#      AUTHORING-time control above.

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
CLEANUP_FORMULAS=()

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

json_field() {
  local json="$1"
  local field="$2"
  echo "$json" | python3 -c "
import sys, json
data = json.load(sys.stdin)
keys = '$field'.split('.')
val = data
for k in keys:
    if isinstance(val, dict):
        val = val.get(k, '')
    elif isinstance(val, list):
        try:
            val = val[int(k)]
        except (ValueError, IndexError):
            val = ''
            break
    else:
        val = ''
        break
print(val if val is not None else '')
" 2>/dev/null || echo ""
}

# Helper: create formula from JSON and track for cleanup
#
# On stdout: the created formula id (empty string on failure) — captured by
# callers via $(create_formula ...), so NOTHING else may go to stdout here.
# On failure the captured kd stderr is echoed to fd 2 (the test log, not the
# caller's command substitution) so transient "no id returned" failures are
# diagnosable instead of silently swallowed. Motivated by gd-TAfS8r14Oc: a
# single transient create on 2026.165.11 reported only "no id returned" with
# no root cause (it passed cleanly on captain-retry, confirming a flake).
create_formula() {
  local title="$1"
  local json="$2"
  local out rc=0 err
  err=$(mktemp)
  out=$(echo "$json" | "$KD_BIN" formula create "$title" --file - --json 2>"$err") || rc=$?
  if [ "$rc" -ne 0 ]; then
    dim "    create_formula '$title' exited $rc: $(tr '\n' ' ' < "$err")" >&2
    rm -f "$err"
    echo ""
    return
  fi
  rm -f "$err"
  local fid
  fid=$(json_field "$out" "id")
  if [ -z "$fid" ]; then
    fid=$(echo "$out" | grep -oE '(gd|ka)-[A-Za-z0-9]+' | head -1 || echo "")
  fi
  if [ -z "$fid" ]; then
    dim "    create_formula '$title' returned no id; output: $(echo "$out" | tr '\n' ' ' | head -c 400)" >&2
  fi
  if [ -n "$fid" ]; then
    CLEANUP_FORMULAS+=("$fid")
  fi
  echo "$fid"
}

# Helper: extract all bead IDs from command output
extract_ids() {
  echo "$1" | grep -oE '(gd|ka)-[A-Za-z0-9]+' || echo ""
}

# Helper: find bead by title substring among a set of IDs
find_bead_by_title() {
  local pattern="$1"
  shift
  for check_id in "$@"; do
    local title
    title=$("$KD_BIN" show "$check_id" --json 2>/dev/null | \
      python3 -c "import sys,json; print(json.load(sys.stdin).get('title',''))" 2>/dev/null || echo "")
    if grep -qF -- "$pattern" <<<"$title"; then
      echo "$check_id"
      return
    fi
  done
  echo ""
}

# ── Setup ─────────────────────────────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19117
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-formula-lifecycle.log 2>&1 &
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
    "$KD_BIN" close "$id" --actor "$KD_ACTOR" 2>/dev/null || true
  done
  for id in "${CLEANUP_FORMULAS[@]}"; do
    "$KD_BIN" close "$id" --actor "$KD_ACTOR" 2>/dev/null || true
  done
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " formula-lifecycle E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  local TEST_TAG="e2e-lifecycle-$$-$(date +%s)"

  # ── Create shared formula for dependency tests ──────────────────
  local chain_formula_json
  chain_formula_json=$(cat <<'FJSON'
{
  "vars": [
    {"name": "tag", "required": true}
  ],
  "steps": [
    {
      "id": "build",
      "title": "Build {{tag}}",
      "type": "task",
      "labels": ["project:e2e-test"]
    },
    {
      "id": "test",
      "title": "Test {{tag}}",
      "type": "task",
      "depends_on": ["build"],
      "labels": ["project:e2e-test"]
    },
    {
      "id": "deploy",
      "title": "Deploy {{tag}}",
      "type": "task",
      "depends_on": ["test"],
      "labels": ["project:e2e-test"]
    }
  ]
}
FJSON
)

  local chain_formula_id
  chain_formula_id=$(create_formula "E2E chain formula $TEST_TAG" "$chain_formula_json")
  if [ -z "$chain_formula_id" ]; then
    red "ERROR: Could not create chain formula"
    exit 1
  fi
  dim "  Chain formula: $chain_formula_id"

  # ── Scenario 1: depends_on creates "blocks" dependencies ────────
  blue "Scenario 1: depends_on creates blocks dependencies"

  local apply_out apply_rc=0
  apply_out=$("$KD_BIN" formula apply "$chain_formula_id" \
    --var "tag=$TEST_TAG" 2>&1) || apply_rc=$?
  assert_exit "formula apply with deps exits 0" 0 "$apply_rc"

  local applied_ids_str
  applied_ids_str=$(extract_ids "$apply_out")
  # shellcheck disable=SC2206
  local applied_ids=($applied_ids_str)
  local id_count=${#applied_ids[@]}

  if [ "$id_count" -ge 4 ]; then
    pass "apply created molecule + 3 step beads ($id_count IDs)"
  else
    fail "apply created expected beads" "expected >=4, got $id_count"
  fi

  for id in "${applied_ids[@]}"; do
    CLEANUP_BEADS+=("$id")
  done

  # Find beads by title
  local build_bead test_bead deploy_bead
  build_bead=$(find_bead_by_title "Build " "${applied_ids[@]}")
  test_bead=$(find_bead_by_title "Test " "${applied_ids[@]}")
  deploy_bead=$(find_bead_by_title "Deploy " "${applied_ids[@]}")

  if [ -n "$test_bead" ] && [ -n "$build_bead" ]; then
    local dep_list
    dep_list=$("$KD_BIN" dep list "$test_bead" --json 2>/dev/null || echo "[]")
    assert_contains "test step has blocks dep on build" "$build_bead" "$dep_list"
    assert_contains "dependency type is blocks" "blocks" "$dep_list"
  else
    fail "find build/test beads" "build=$build_bead test=$test_bead"
  fi
  echo

  # ── Scenario 2: Three-step chain: deploy → test → build ────────
  blue "Scenario 2: Three-step dependency chain verified"

  if [ -n "$deploy_bead" ] && [ -n "$test_bead" ]; then
    local deploy_deps
    deploy_deps=$("$KD_BIN" dep list "$deploy_bead" --json 2>/dev/null || echo "[]")
    assert_contains "deploy depends on test" "$test_bead" "$deploy_deps"

    # Build should have NO blocks deps (only parent-child to molecule)
    if [ -n "$build_bead" ]; then
      local build_deps
      build_deps=$("$KD_BIN" dep list "$build_bead" --json 2>/dev/null || echo "[]")
      assert_not_contains "build has no blocks dependency" '"type": "blocks"' "$build_deps"
    fi
  else
    fail "find deploy/test beads" "deploy=$deploy_bead test=$test_bead"
  fi
  echo

  # ── Scenario 3: Conditional step skipped when false ─────────────
  blue "Scenario 3: Conditional step skipped when condition is false"

  local cond_formula_json
  cond_formula_json=$(cat <<'FJSON'
{
  "vars": [
    {"name": "tag", "required": true},
    {"name": "run_deploy", "default": "false"}
  ],
  "steps": [
    {
      "id": "build",
      "title": "CBuild {{tag}}",
      "type": "task",
      "labels": ["project:e2e-test"]
    },
    {
      "id": "deploy",
      "title": "CDeploy {{tag}}",
      "type": "task",
      "condition": "{{run_deploy}} == true",
      "depends_on": ["build"],
      "labels": ["project:e2e-test"]
    }
  ]
}
FJSON
)

  local cond_formula_id
  cond_formula_id=$(create_formula "E2E condition formula $TEST_TAG" "$cond_formula_json")
  if [ -z "$cond_formula_id" ]; then
    fail "create condition formula" "no id returned"
  else
    pass "created condition formula ($cond_formula_id)"

    # Apply with default run_deploy=false → deploy should be skipped
    local cond_out cond_rc=0
    cond_out=$("$KD_BIN" formula apply "$cond_formula_id" \
      --var "tag=$TEST_TAG" 2>&1) || cond_rc=$?
    assert_exit "apply with condition=false exits 0" 0 "$cond_rc"

    # Check step count from output
    local step_count
    step_count=$(echo "$cond_out" | grep -c '→' || echo "0")
    assert_eq "only 1 step created (deploy skipped)" "1" "$step_count"

    local cond_ids_str
    cond_ids_str=$(extract_ids "$cond_out")
    for id in $cond_ids_str; do
      CLEANUP_BEADS+=("$id")
    done

    # Verify no deploy bead was created
    # shellcheck disable=SC2206
    local cond_ids=($cond_ids_str)
    local deploy_found
    deploy_found=$(find_bead_by_title "CDeploy " "${cond_ids[@]}")
    if [ -z "$deploy_found" ]; then
      pass "deploy step not created (condition=false)"
    else
      fail "deploy step skipped" "deploy bead exists: $deploy_found"
    fi
  fi
  echo

  # ── Scenario 4: Conditional step created when true ──────────────
  blue "Scenario 4: Conditional step created when condition is true"

  if [ -n "${cond_formula_id:-}" ]; then
    local cond_out2 cond_rc2=0
    cond_out2=$("$KD_BIN" formula apply "$cond_formula_id" \
      --var "tag=${TEST_TAG}-cond-true" \
      --var "run_deploy=true" 2>&1) || cond_rc2=$?
    assert_exit "apply with condition=true exits 0" 0 "$cond_rc2"

    local step_count2
    step_count2=$(echo "$cond_out2" | grep -c '→' || echo "0")
    assert_eq "2 steps created when condition=true" "2" "$step_count2"

    local cond_ids2_str
    cond_ids2_str=$(extract_ids "$cond_out2")
    for id in $cond_ids2_str; do
      CLEANUP_BEADS+=("$id")
    done

    # shellcheck disable=SC2206
    local cond_ids2=($cond_ids2_str)
    local deploy_found2
    deploy_found2=$(find_bead_by_title "CDeploy " "${cond_ids2[@]}")
    if [ -n "$deploy_found2" ]; then
      pass "deploy step created when condition=true"
    else
      fail "deploy step created" "deploy bead missing"
    fi
  else
    fail "scenario 4 skipped" "no condition formula"
  fi
  echo

  # ── Scenario 5: --dry-run previews without creating beads ───────
  blue "Scenario 5: --dry-run previews without creating beads"

  # Use a tag unique to THIS test invocation ($TEST_TAG already embeds $$ +
  # epoch) so the "created nothing" assertion can scope to beads only this
  # dry-run could have produced. A global count of all open project:e2e-test
  # beads is NOT safe here: 7 other e2e scripts (test-formula.sh,
  # test-formula-dedup.sh, test-dependencies.sh, test-epics-children.sh,
  # test-mail-ready.sh, test-triggers.sh, test-credential-rotation.sh) also
  # create/close project:e2e-test beads concurrently on the shared e2e
  # daemon across other shards. A concurrent create/close between a before/after
  # count flips the equality and fails this module — the intermittent 1/102
  # flake that repeatedly blocked golden promotion (ka-0000004111, ka-0000005986).
  local dry_tag="dry-run-${TEST_TAG}"

  local dry_out dry_rc=0
  dry_out=$("$KD_BIN" formula apply "$chain_formula_id" \
    --var "tag=$dry_tag" --dry-run 2>&1) || dry_rc=$?
  assert_exit "formula apply --dry-run exits 0" 0 "$dry_rc"
  assert_contains "dry-run shows molecule info" "Would create" "$dry_out"
  assert_contains "dry-run shows build step" "Build" "$dry_out"
  assert_contains "dry-run shows step count" "3 steps" "$dry_out"

  # Pollution-immune check: count beads whose title carries this run's unique
  # dry-run tag. If --dry-run wrongly created beads they'd be titled
  # "Build $dry_tag" / "Test $dry_tag" / "Deploy $dry_tag"; a correct dry-run
  # creates zero. Other shards' beads never carry $dry_tag, so concurrent
  # writes cannot perturb this count.
  local dry_created
  dry_created=$("$KD_BIN" list --status=open -l "project:e2e-test" --json 2>/dev/null | \
    python3 -c "import sys,json; print(sum(1 for b in json.load(sys.stdin) if '$dry_tag' in b.get('title','')))" 2>/dev/null || echo "-1")
  assert_eq "dry-run creates no new beads" "0" "$dry_created"
  echo

  # ── Scenario 6: Pour vs Wisp — ephemeral flag ──────────────────
  blue "Scenario 6: Pour creates persistent, wisp creates ephemeral"

  local simple_formula_json
  simple_formula_json=$(cat <<'FJSON'
{
  "vars": [
    {"name": "tag", "required": true}
  ],
  "steps": [
    {
      "id": "work",
      "title": "Work item {{tag}}",
      "type": "task",
      "labels": ["project:e2e-test"]
    }
  ]
}
FJSON
)

  local simple_formula_id
  simple_formula_id=$(create_formula "E2E simple formula $TEST_TAG" "$simple_formula_json")
  if [ -z "$simple_formula_id" ]; then
    fail "create simple formula" "no id returned"
  else
    # Pour (persistent)
    local pour_out pour_rc=0
    pour_out=$("$KD_BIN" formula pour "$simple_formula_id" \
      --var "tag=pour-$TEST_TAG" 2>&1) || pour_rc=$?
    assert_exit "kd formula pour exits 0" 0 "$pour_rc"

    local pour_ids_str
    pour_ids_str=$(extract_ids "$pour_out")
    # shellcheck disable=SC2206
    local pour_ids=($pour_ids_str)
    local pour_mol=""
    for id in "${pour_ids[@]}"; do
      CLEANUP_BEADS+=("$id")
      local btype
      btype=$("$KD_BIN" show "$id" --json 2>/dev/null | \
        python3 -c "import sys,json; print(json.load(sys.stdin).get('type',''))" 2>/dev/null || echo "")
      if [ "$btype" = "molecule" ]; then
        pour_mol="$id"
      fi
    done

    if [ -n "$pour_mol" ]; then
      local pour_ephemeral
      pour_ephemeral=$("$KD_BIN" show "$pour_mol" --json 2>/dev/null | python3 -c "
import sys, json
data = json.load(sys.stdin)
fields = data.get('fields', {})
print(str(fields.get('ephemeral', 'false')).lower())
" 2>/dev/null || echo "")
      if [ "$pour_ephemeral" = "false" ] || [ -z "$pour_ephemeral" ]; then
        pass "pour molecule is not ephemeral"
      else
        fail "pour molecule ephemeral" "expected false, got '$pour_ephemeral'"
      fi
    else
      fail "pour created molecule" "no molecule bead found"
    fi

    # Wisp (ephemeral)
    local wisp_out wisp_rc=0
    wisp_out=$("$KD_BIN" formula wisp "$simple_formula_id" \
      --var "tag=wisp-$TEST_TAG" 2>&1) || wisp_rc=$?
    assert_exit "kd formula wisp exits 0" 0 "$wisp_rc"

    local wisp_ids_str
    wisp_ids_str=$(extract_ids "$wisp_out")
    # shellcheck disable=SC2206
    local wisp_ids=($wisp_ids_str)
    local wisp_mol=""
    for id in "${wisp_ids[@]}"; do
      CLEANUP_BEADS+=("$id")
      local btype
      btype=$("$KD_BIN" show "$id" --json 2>/dev/null | \
        python3 -c "import sys,json; print(json.load(sys.stdin).get('type',''))" 2>/dev/null || echo "")
      if [ "$btype" = "molecule" ]; then
        wisp_mol="$id"
      fi
    done

    if [ -n "$wisp_mol" ]; then
      local wisp_ephemeral
      wisp_ephemeral=$("$KD_BIN" show "$wisp_mol" --json 2>/dev/null | python3 -c "
import sys, json
data = json.load(sys.stdin)
fields = data.get('fields', {})
print(str(fields.get('ephemeral', 'false')).lower())
" 2>/dev/null || echo "")
      if [ "$wisp_ephemeral" = "true" ]; then
        pass "wisp molecule is ephemeral"
      else
        fail "wisp molecule ephemeral" "expected true, got '$wisp_ephemeral'"
      fi
    else
      fail "wisp created molecule" "no molecule bead found"
    fi
  fi
  echo

  # ── Scenario 7: Formula update modifies definition ──────────────
  blue "Scenario 7: Formula update modifies definition"

  local update_formula_json
  update_formula_json=$(cat <<'FJSON'
{
  "vars": [
    {"name": "tag", "required": true}
  ],
  "steps": [
    {
      "id": "original",
      "title": "Original {{tag}}",
      "type": "task",
      "labels": ["project:e2e-test"]
    }
  ]
}
FJSON
)

  local update_formula_id
  update_formula_id=$(create_formula "E2E update formula $TEST_TAG" "$update_formula_json")
  if [ -z "$update_formula_id" ]; then
    fail "create update formula" "no id returned"
  else
    pass "created update formula ($update_formula_id)"

    # Verify original step
    local show_before
    show_before=$("$KD_BIN" formula show "$update_formula_id" 2>&1)
    assert_contains "original formula has step 'Original'" "Original" "$show_before"

    # Update with new definition
    local updated_json
    updated_json=$(cat <<'UJSON'
{
  "vars": [
    {"name": "tag", "required": true},
    {"name": "env", "default": "staging"}
  ],
  "steps": [
    {
      "id": "updated",
      "title": "Updated {{tag}} ({{env}})",
      "type": "task",
      "labels": ["project:e2e-test"]
    }
  ]
}
UJSON
)

    local update_out update_rc=0
    update_out=$(echo "$updated_json" | "$KD_BIN" formula update "$update_formula_id" \
      --file - 2>&1) || update_rc=$?
    assert_exit "kd formula update exits 0" 0 "$update_rc"

    # Verify updated definition
    local show_after
    show_after=$("$KD_BIN" formula show "$update_formula_id" 2>&1)
    assert_contains "updated formula has step 'Updated'" "Updated" "$show_after"
    assert_contains "updated formula has new var 'env'" "env" "$show_after"

    # Apply updated formula to verify it works
    local updated_apply_out updated_apply_rc=0
    updated_apply_out=$("$KD_BIN" formula apply "$update_formula_id" \
      --var "tag=after-update" 2>&1) || updated_apply_rc=$?
    assert_exit "apply updated formula exits 0" 0 "$updated_apply_rc"

    local updated_ids_str
    updated_ids_str=$(extract_ids "$updated_apply_out")
    for id in $updated_ids_str; do
      CLEANUP_BEADS+=("$id")
    done

    # Verify the step title uses the updated template with default env
    assert_contains "updated apply uses new title" "Updated after-update" "$updated_apply_out"
  fi
  echo

  # ── Scenario 8: Formula dump → create round-trip ────────────────
  blue "Scenario 8: Formula dump → create round-trip preserves definition"

  if [ -n "${chain_formula_id:-}" ]; then
    # Dump the chain formula
    local dump_out dump_rc=0
    dump_out=$("$KD_BIN" formula dump "$chain_formula_id" 2>/dev/null) || dump_rc=$?
    assert_exit "kd formula dump exits 0" 0 "$dump_rc"

    # Verify it's valid JSON
    local valid
    valid=$(echo "$dump_out" | python3 -c "import sys,json; json.load(sys.stdin); print('valid')" 2>/dev/null || echo "invalid")
    assert_eq "dump produces valid JSON" "valid" "$valid"

    # Create a new formula from the dump. The dump->create round-trip
    # occasionally yields no id under a transient kd port-forward / daemon-write
    # hiccup (gd-hxpITpblz3) — same flake passes cleanly on retry. Treat an
    # empty id as retryable: a few attempts with short backoff before failing.
    local roundtrip_id="" attempt
    for attempt in 1 2 3; do
      roundtrip_id=$(create_formula "E2E roundtrip formula $TEST_TAG" "$dump_out")
      [ -n "$roundtrip_id" ] && break
      dim "    create-from-dump returned no id (attempt $attempt/3); retrying after backoff" >&2
      sleep $((attempt * 2))
    done
    if [ -n "$roundtrip_id" ]; then
      pass "created formula from dump ($roundtrip_id)"

      # Show both and verify they have the same structure
      local orig_show rt_show
      orig_show=$("$KD_BIN" formula show "$chain_formula_id" 2>&1)
      rt_show=$("$KD_BIN" formula show "$roundtrip_id" 2>&1)

      # Both should have the same vars and steps
      assert_contains "roundtrip formula has 'tag' var" "tag" "$rt_show"
      assert_contains "roundtrip formula has build step" "Build" "$rt_show"
      assert_contains "roundtrip formula has test step" "Test" "$rt_show"
      assert_contains "roundtrip formula has deploy step" "Deploy" "$rt_show"
    else
      fail "create formula from dump" "no id returned"
    fi
  else
    fail "scenario 8 skipped" "no chain formula"
  fi
  echo

  # ── Scenario 9: Mol bond parallel creates "related" dependency ──
  blue "Scenario 9: Mol bond parallel creates related dependency"

  local bond_formula_json
  bond_formula_json=$(cat <<'FJSON'
{
  "vars": [
    {"name": "label", "required": true}
  ],
  "steps": [
    {
      "id": "work",
      "title": "BondWork {{label}}",
      "type": "task",
      "labels": ["project:e2e-test"]
    }
  ]
}
FJSON
)

  local bond_formula_id
  bond_formula_id=$(create_formula "E2E bond formula $TEST_TAG" "$bond_formula_json")
  if [ -z "$bond_formula_id" ]; then
    fail "create bond formula" "no id returned"
  else
    # Create two molecules
    local mol_a_out mol_a_rc=0
    mol_a_out=$("$KD_BIN" formula apply "$bond_formula_id" \
      --var "label=alpha-$TEST_TAG" 2>&1) || mol_a_rc=$?

    local mol_a_ids_str
    mol_a_ids_str=$(extract_ids "$mol_a_out")
    local mol_a=""
    for id in $mol_a_ids_str; do
      CLEANUP_BEADS+=("$id")
      local btype
      btype=$("$KD_BIN" show "$id" --json 2>/dev/null | \
        python3 -c "import sys,json; print(json.load(sys.stdin).get('type',''))" 2>/dev/null || echo "")
      if [ "$btype" = "molecule" ]; then
        mol_a="$id"
      fi
    done

    local mol_b_out mol_b_rc=0
    mol_b_out=$("$KD_BIN" formula apply "$bond_formula_id" \
      --var "label=beta-$TEST_TAG" 2>&1) || mol_b_rc=$?

    local mol_b_ids_str
    mol_b_ids_str=$(extract_ids "$mol_b_out")
    local mol_b=""
    for id in $mol_b_ids_str; do
      CLEANUP_BEADS+=("$id")
      local btype
      btype=$("$KD_BIN" show "$id" --json 2>/dev/null | \
        python3 -c "import sys,json; print(json.load(sys.stdin).get('type',''))" 2>/dev/null || echo "")
      if [ "$btype" = "molecule" ]; then
        mol_b="$id"
      fi
    done

    if [ -n "$mol_a" ] && [ -n "$mol_b" ]; then
      pass "created molecules alpha ($mol_a) and beta ($mol_b)"

      # Bond as sequential (default) → should create "blocks" dep
      local seq_bond_out seq_bond_rc=0
      seq_bond_out=$("$KD_BIN" mol bond "$mol_a" "$mol_b" \
        --actor "$KD_ACTOR" 2>&1) || seq_bond_rc=$?
      assert_exit "sequential bond exits 0" 0 "$seq_bond_rc"
      assert_contains "sequential bond confirms link" "Bonded" "$seq_bond_out"

      local seq_deps
      seq_deps=$("$KD_BIN" dep list "$mol_b" --json 2>/dev/null || echo "[]")
      assert_contains "sequential bond creates blocks dep" "blocks" "$seq_deps"
      assert_contains "mol_b depends on mol_a" "$mol_a" "$seq_deps"

      # Create a third molecule for parallel bond
      local mol_c_out mol_c_rc=0
      mol_c_out=$("$KD_BIN" formula apply "$bond_formula_id" \
        --var "label=gamma-$TEST_TAG" 2>&1) || mol_c_rc=$?

      local mol_c_ids_str
      mol_c_ids_str=$(extract_ids "$mol_c_out")
      local mol_c=""
      for id in $mol_c_ids_str; do
        CLEANUP_BEADS+=("$id")
        local btype
        btype=$("$KD_BIN" show "$id" --json 2>/dev/null | \
          python3 -c "import sys,json; print(json.load(sys.stdin).get('type',''))" 2>/dev/null || echo "")
        if [ "$btype" = "molecule" ]; then
          mol_c="$id"
        fi
      done

      if [ -n "$mol_c" ]; then
        local par_bond_out par_bond_rc=0
        par_bond_out=$("$KD_BIN" mol bond "$mol_a" "$mol_c" \
          --type parallel --actor "$KD_ACTOR" 2>&1) || par_bond_rc=$?
        assert_exit "parallel bond exits 0" 0 "$par_bond_rc"

        local par_deps
        par_deps=$("$KD_BIN" dep list "$mol_c" --json 2>/dev/null || echo "[]")
        assert_contains "parallel bond creates related dep" "related" "$par_deps"
      else
        fail "create third molecule for parallel bond" "no id returned"
      fi
    else
      fail "create bond molecules" "mol_a=$mol_a mol_b=$mol_b"
    fi
  fi
  echo

  # ── Scenario 10: Mol squash --close-mol ─────────────────────────
  blue "Scenario 10: Mol squash --close-mol closes molecule and children"

  local squash_formula_json
  squash_formula_json=$(cat <<'FJSON'
{
  "vars": [
    {"name": "tag", "required": true}
  ],
  "steps": [
    {
      "id": "step-a",
      "title": "Squash-A {{tag}}",
      "type": "task",
      "labels": ["project:e2e-test"]
    },
    {
      "id": "step-b",
      "title": "Squash-B {{tag}}",
      "type": "task",
      "labels": ["project:e2e-test"]
    }
  ]
}
FJSON
)

  local squash_formula_id
  squash_formula_id=$(create_formula "E2E squash formula $TEST_TAG" "$squash_formula_json")
  if [ -z "$squash_formula_id" ]; then
    fail "create squash formula" "no id returned"
  else
    local squash_apply_out squash_apply_rc=0
    squash_apply_out=$("$KD_BIN" formula apply "$squash_formula_id" \
      --var "tag=$TEST_TAG" 2>&1) || squash_apply_rc=$?
    assert_exit "apply for squash exits 0" 0 "$squash_apply_rc"

    local squash_ids_str
    squash_ids_str=$(extract_ids "$squash_apply_out")
    # shellcheck disable=SC2206
    local squash_ids=($squash_ids_str)

    local squash_mol=""
    local squash_children=()
    for id in "${squash_ids[@]}"; do
      CLEANUP_BEADS+=("$id")
      local btype
      btype=$("$KD_BIN" show "$id" --json 2>/dev/null | \
        python3 -c "import sys,json; print(json.load(sys.stdin).get('type',''))" 2>/dev/null || echo "")
      if [ "$btype" = "molecule" ]; then
        squash_mol="$id"
      elif [ "$btype" = "task" ]; then
        squash_children+=("$id")
      fi
    done

    if [ -n "$squash_mol" ] && [ "${#squash_children[@]}" -ge 2 ]; then
      pass "created squash molecule ($squash_mol) with ${#squash_children[@]} children"

      # Squash with --close-mol and --summary
      local squash_out squash_rc=0
      squash_out=$("$KD_BIN" mol squash "$squash_mol" \
        --close-mol \
        --summary "E2E squash lifecycle test" \
        --actor "$KD_ACTOR" 2>&1) || squash_rc=$?
      assert_exit "kd mol squash --close-mol exits 0" 0 "$squash_rc"
      assert_contains "squash output confirms children closed" "children closed" "$squash_out"

      # Verify children are closed
      for child_id in "${squash_children[@]}"; do
        local child_status
        child_status=$("$KD_BIN" show "$child_id" --json 2>/dev/null | \
          python3 -c "import sys,json; print(json.load(sys.stdin).get('status',''))" 2>/dev/null || echo "")
        assert_eq "child $child_id is closed after squash" "closed" "$child_status"
      done

      # Verify molecule itself is closed (--close-mol flag)
      local mol_status
      mol_status=$("$KD_BIN" show "$squash_mol" --json 2>/dev/null | \
        python3 -c "import sys,json; print(json.load(sys.stdin).get('status',''))" 2>/dev/null || echo "")
      assert_eq "molecule is closed after squash --close-mol" "closed" "$mol_status"

      # Verify digest comment was added
      local comments_out
      comments_out=$("$KD_BIN" comment list "$squash_mol" --json 2>/dev/null || echo "[]")
      assert_contains "squash added digest comment" "E2E squash lifecycle test" "$comments_out"
    else
      fail "create squash beads" "mol=$squash_mol children=${#squash_children[@]}"
    fi
  fi
  echo

  # ── Scenario 11: extends inheritance round-trips through update ─
  # Regression guard for ka-0000005592. `kd formula update` rejected any
  # formula using `extends`, in two distinct ways — so dumping an extends-based
  # formula and updating it straight back with ZERO edits failed, making every
  # such formula un-editable through the CLI. Update never called
  # resolveInheritance the way create/cook/apply do, so:
  #   #2 a child step depending on an INHERITED (parent) step was rejected with
  #      "depends_on unknown step" (higher-value half — the regression IS the
  #      dump->update round-trip below);
  #   #1 a child with zero OWN steps but a non-empty `extends` was rejected with
  #      "formula must have at least one step" (create already allowed this).
  # This exercises both, proves inheritance actually resolves (apply merges
  # parent+child steps), and keeps a NEGATIVE CONTROL so the fix cannot degrade
  # to "stopped validating depends_on at all".
  blue "Scenario 11: extends inheritance round-trips through formula update"

  # Parent provides two steps; ExtInvestigate depends_on ExtCheck. Distinctive
  # title prefixes ("Ext…") keep the apply-merge grep unambiguous vs other
  # scenarios' Build/Test/Deploy/CBuild titles.
  local ext_parent_json
  ext_parent_json=$(cat <<'FJSON'
{
  "vars": [
    {"name": "tag", "required": true}
  ],
  "steps": [
    {
      "id": "check",
      "title": "ExtCheck {{tag}}",
      "type": "task",
      "labels": ["project:e2e-test"]
    },
    {
      "id": "investigate-fix",
      "title": "ExtInvestigate {{tag}}",
      "type": "task",
      "depends_on": ["check"],
      "labels": ["project:e2e-test"]
    }
  ]
}
FJSON
)

  # Reuse scenario 8's retry-on-empty-id pattern (flake gd-hxpITpblz3) for both
  # creates — a transient daemon hiccup here would otherwise sink the scenario.
  local ext_parent_id="" ext_attempt
  for ext_attempt in 1 2 3; do
    ext_parent_id=$(create_formula "E2E extends parent $TEST_TAG" "$ext_parent_json")
    [ -n "$ext_parent_id" ] && break
    dim "    create extends parent returned no id (attempt $ext_attempt/3); retrying" >&2
    sleep $((ext_attempt * 2))
  done

  if [ -z "$ext_parent_id" ]; then
    fail "create extends parent formula" "no id returned"
  else
    pass "created extends parent formula ($ext_parent_id)"

    # Child extends the parent and adds ONE own step whose depends_on names the
    # INHERITED "investigate-fix" step — breakage #2's exact shape. Unquoted
    # heredoc so $ext_parent_id expands; formula "{{tag}}" braces stay literal.
    local ext_child_json
    ext_child_json=$(cat <<FJSON
{
  "extends": ["$ext_parent_id"],
  "vars": [
    {"name": "tag", "required": true}
  ],
  "steps": [
    {
      "id": "fix-critic",
      "title": "ExtCritic {{tag}}",
      "type": "task",
      "depends_on": ["investigate-fix"],
      "labels": ["project:e2e-test"]
    }
  ]
}
FJSON
)

    local ext_child_id=""
    for ext_attempt in 1 2 3; do
      ext_child_id=$(create_formula "E2E extends child $TEST_TAG" "$ext_child_json")
      [ -n "$ext_child_id" ] && break
      dim "    create extends child returned no id (attempt $ext_attempt/3); retrying" >&2
      sleep $((ext_attempt * 2))
    done

    if [ -z "$ext_child_id" ]; then
      fail "create extends child formula" "no id returned"
    else
      pass "created extends child formula ($ext_child_id)"

      # ── #2 — the core regression: dump child, feed straight back to update
      # with ZERO edits. depends_on names an inherited step; update must resolve
      # the extends chain to know it. Exit 0 is the whole assertion.
      local ext_dump ext_dump_rc=0
      ext_dump=$("$KD_BIN" formula dump "$ext_child_id" 2>/dev/null) || ext_dump_rc=$?
      assert_exit "dump extends child exits 0" 0 "$ext_dump_rc"
      assert_contains "dump preserves extends key" "extends" "$ext_dump"

      local ext_rt_out ext_rt_rc=0
      ext_rt_out=$(echo "$ext_dump" | "$KD_BIN" formula update "$ext_child_id" \
        --file - 2>&1) || ext_rt_rc=$?
      assert_exit "dump->update round-trip on extends child exits 0" 0 "$ext_rt_rc"

      # ── Prove inheritance actually RESOLVES (not merely not-erroring): apply
      # the child and confirm the merged step set carries parent + child steps.
      local ext_apply_out ext_apply_rc=0
      ext_apply_out=$("$KD_BIN" formula apply "$ext_child_id" \
        --var "tag=$TEST_TAG" 2>&1) || ext_apply_rc=$?
      assert_exit "apply extends child exits 0" 0 "$ext_apply_rc"
      for id in $(extract_ids "$ext_apply_out"); do
        CLEANUP_BEADS+=("$id")
      done
      assert_contains "apply merges inherited parent step (check)" "ExtCheck" "$ext_apply_out"
      assert_contains "apply merges inherited parent step (investigate-fix)" "ExtInvestigate" "$ext_apply_out"
      assert_contains "apply includes child's own step" "ExtCritic" "$ext_apply_out"

      # ── NEGATIVE CONTROL — a depends_on matching NEITHER a child nor an
      # inherited step MUST still be rejected. Without this we cannot tell
      # "resolves inherited IDs correctly" from "stopped validating deps at all".
      local ext_bad_json
      ext_bad_json=$(cat <<FJSON
{
  "extends": ["$ext_parent_id"],
  "vars": [
    {"name": "tag", "required": true}
  ],
  "steps": [
    {
      "id": "fix-critic",
      "title": "ExtCritic {{tag}}",
      "type": "task",
      "depends_on": ["does-not-exist"],
      "labels": ["project:e2e-test"]
    }
  ]
}
FJSON
)
      local ext_bad_out ext_bad_rc=0
      ext_bad_out=$(echo "$ext_bad_json" | "$KD_BIN" formula update "$ext_child_id" \
        --file - 2>&1) || ext_bad_rc=$?
      if [ "$ext_bad_rc" -ne 0 ]; then
        pass "update rejects dep on truly-unknown step (exit $ext_bad_rc)"
      else
        fail "update rejects dep on truly-unknown step" "expected nonzero exit, got 0"
      fi
      assert_contains "rejection names the unknown step" "unknown step" "$ext_bad_out"

      # ── #1 — extends with ZERO own steps (overrides only vars) must be
      # accepted; the old "formula must have at least one step" check rejected
      # it. Run LAST — it empties the child's own steps.
      local ext_novars_json
      ext_novars_json=$(cat <<FJSON
{
  "extends": ["$ext_parent_id"],
  "vars": [
    {"name": "tag", "required": true},
    {"name": "env", "default": "staging"}
  ],
  "steps": []
}
FJSON
)
      local ext_empty_out ext_empty_rc=0
      ext_empty_out=$(echo "$ext_novars_json" | "$KD_BIN" formula update "$ext_child_id" \
        --file - 2>&1) || ext_empty_rc=$?
      assert_exit "update extends-only formula (zero own steps) exits 0" 0 "$ext_empty_rc"
    fi
  fi
  echo

  # ── Scenario 12: pour-time dangling depends_on is rejected BEFORE
  #    any bead is created (ka-0000002771; fix 92e072a48) ────────────
  #
  # THE BUG. The pour path creates all step beads, then wires blocks edges in a
  # SECOND pass keyed by an in-memory step-id -> bead-id map. A `depends_on`
  # that named no created step used to hit `if !ok { continue }` and vanish —
  # the molecule came out looking correct (right steps, titles, parent) but with
  # a step->step gate that gated NOTHING, handing a late step to `gb ready`
  # before its prerequisite. The fix runs formula.ValidateDependencies on the
  # active (post-ExpandSteps) tree BEFORE any create, so a dangling ref fails the
  # whole pour loudly.
  #
  # This is the POUR path (`kd formula apply`), NOT Scenario 11's `formula
  # update` — that validates at AUTHORING time (`validateStepDeps`, a different
  # code path with a different "unknown step" message). The pour-time guard is
  # DEFENCE-IN-DEPTH: `kd formula create`/`update` reject a dangling formula so
  # it can never be PERSISTED as a bead — but the file-load apply path
  # (`kd formula apply ./f.json`, loadFormulaFromFile) skips authoring
  # validation, so a dangling formula reaches the pour with NO prior gate. That
  # file path is exactly where the original bug poured an ungated molecule, so
  # the dangling legs below drive the pour from a FILE (the e2e analogue of how
  # the unit/server tests inject a dangling formula directly). Grep confirms
  # this is the only e2e leg exercising `refusing to pour`/`ungated molecule`
  # on a live pour.
  #
  # We assert four legs, each with the control that makes it non-vacuous:
  #   A. POSITIVE control — a persisted formula whose depends_on DOES resolve
  #      pours and the gating blocks edge exists. Without it a "pour failed"
  #      assertion cannot tell the guard firing from a broken daemon.
  #   B. DANGLING leg — apply a FILE whose step depends_on an undeclared step:
  #      nonzero exit, the error names the unresolvable step id, and — the
  #      anti-vacuity assertion that matters — NO step beads were created (the
  #      fix is fail-BEFORE-create).
  #   C. DRY-RUN leg — the guard runs on --dry-run too; the preview surfaces the
  #      same authoring bug without creating anything.
  #   D. CONDITION-SKIPPED false-positive control — ExpandSteps strips
  #      depends_on refs to condition-skipped steps, so a ref to a skipped step
  #      must NOT be flagged as dangling; the pour still succeeds.
  blue "Scenario 12: pour-time dangling depends_on rejected before create (ka-0000002771)"

  # ── 12A: POSITIVE control — a resolvable depends_on pours + gates ──
  local ok_json
  ok_json=$(cat <<FJSON
{
  "steps": [
    {
      "id": "investigate",
      "title": "PTInvestigate $TEST_TAG",
      "type": "task",
      "labels": ["project:e2e-test"]
    },
    {
      "id": "tag-reviewer",
      "title": "PTReviewer $TEST_TAG",
      "type": "task",
      "depends_on": ["investigate"],
      "labels": ["project:e2e-test"]
    }
  ]
}
FJSON
)
  local ok_fid
  ok_fid=$(create_formula "E2E pour-dangling positive $TEST_TAG" "$ok_json")
  if [ -z "$ok_fid" ]; then
    fail "scenario 12A: create positive-control formula" "no id returned"
  else
    local ok_out ok_rc=0
    ok_out=$("$KD_BIN" formula apply "$ok_fid" 2>&1) || ok_rc=$?
    assert_exit "12A: resolvable depends_on pours (positive control)" 0 "$ok_rc"

    local ok_ids_str
    ok_ids_str=$(extract_ids "$ok_out")
    for id in $ok_ids_str; do
      CLEANUP_BEADS+=("$id")
    done
    # shellcheck disable=SC2206
    local ok_ids=($ok_ids_str)

    # The gating blocks edge must actually exist — proves the guard is
    # validating a real edge, not vacuously passing.
    local ptrev_bead ptinv_bead
    ptrev_bead=$(find_bead_by_title "PTReviewer " "${ok_ids[@]}")
    ptinv_bead=$(find_bead_by_title "PTInvestigate " "${ok_ids[@]}")
    if [ -n "$ptrev_bead" ] && [ -n "$ptinv_bead" ]; then
      local ok_deps
      ok_deps=$("$KD_BIN" dep list "$ptrev_bead" --json 2>/dev/null || echo "[]")
      assert_contains "12A: reviewer step gated on investigate (blocks edge exists)" \
        "$ptinv_bead" "$ok_deps"
      assert_contains "12A: gating edge type is blocks" "blocks" "$ok_deps"
    else
      fail "12A: positive-control step beads located" \
        "reviewer='$ptrev_bead' investigate='$ptinv_bead'"
    fi
  fi
  echo

  # ── 12B: DANGLING leg — undeclared depends_on, fail BEFORE create ──
  # `kd formula create`/`update` reject this formula at authoring time, so it
  # can never become a bead — we drive the pour from a FILE, the path
  # (loadFormulaFromFile) that skips authoring validation and is exactly where
  # the original bug poured an ungated molecule. tag-reviewer gates on
  # `gardener-signoff`, which is never declared. Every poured step is stamped
  # `source_formula: <path>` (the bare apply argument, verified empirically —
  # NOT prefixed with file:), so a server-side fielded count on that value is
  # the pagination-proof way to prove NO step bead was created.
  local bad_file="/tmp/e2e-pour-dangling-$$-$RANDOM.json"
  cat > "$bad_file" <<FJSON
{
  "steps": [
    {
      "id": "investigate",
      "title": "PTInvestigate $TEST_TAG dangle",
      "type": "task",
      "labels": ["project:e2e-test"]
    },
    {
      "id": "tag-reviewer",
      "title": "PTReviewer $TEST_TAG dangle",
      "type": "task",
      "depends_on": ["gardener-signoff"],
      "labels": ["project:e2e-test"]
    }
  ]
}
FJSON
  local bad_out bad_rc=0
  bad_out=$("$KD_BIN" formula apply "$bad_file" 2>&1) || bad_rc=$?
  # In case the guard ever regresses and a molecule IS poured, capture any
  # created ids for cleanup.
  for id in $(extract_ids "$bad_out"); do
    CLEANUP_BEADS+=("$id")
  done

  if [ "$bad_rc" -ne 0 ]; then
    pass "12B: pour (from file) with dangling depends_on fails (exit $bad_rc)"
  else
    fail "12B: pour (from file) with dangling depends_on fails" "expected nonzero exit, got 0"
  fi
  # The error must name the unresolvable step id — not a generic failure.
  assert_contains "12B: error names the unresolvable step (gardener-signoff)" \
    "gardener-signoff" "$bad_out"

  # THE ANTI-VACUITY ASSERTION: fail BEFORE create means ZERO step beads carry
  # this file-formula's id. `kd count -f` is a server-side count (no paging, no
  # default-limit false-empty) over the `source_formula` field every poured
  # step gets stamped with — the bare file path (see verified note above).
  local bad_step_count
  bad_step_count=$("$KD_BIN" count --all-types -f "source_formula=$bad_file" 2>/dev/null || echo "?")
  assert_eq "12B: NO step bead created (fail-before-create; source_formula count)" \
    "0" "$bad_step_count"
  echo

  # ── 12C: DRY-RUN leg — the guard runs on --dry-run too ────────────
  # Reuse the dangling file: --dry-run must surface the same authoring bug
  # (nonzero exit naming the ref) so a confirm-before-pour preview blocks it.
  local dry_out dry_rc=0
  dry_out=$("$KD_BIN" formula apply "$bad_file" --dry-run 2>&1) || dry_rc=$?
  if [ "$dry_rc" -ne 0 ]; then
    pass "12C: --dry-run also rejects dangling depends_on (exit $dry_rc)"
  else
    fail "12C: --dry-run rejects dangling depends_on" "expected nonzero exit, got 0"
  fi
  assert_contains "12C: dry-run error names the unresolvable step" \
    "gardener-signoff" "$dry_out"
  rm -f "$bad_file"
  echo

  # ── 12D: FALSE-POSITIVE control — depends_on a CONDITION-SKIPPED step
  #    must NOT be flagged as dangling ────────────────────────────────
  # ExpandSteps drops the skipped step AND the depends_on refs pointing at it,
  # so `after` ends up with no unresolved edge. A guard that flagged this would
  # be a false positive; the pour must succeed. `run_maybe` defaults to false so
  # `maybe` is skipped and `after`'s depends_on:[maybe] is stripped.
  local skip_json
  skip_json=$(cat <<FJSON
{
  "vars": [
    {"name": "run_maybe", "default": "false"}
  ],
  "steps": [
    {
      "id": "always",
      "title": "PTAlways $TEST_TAG",
      "type": "task",
      "labels": ["project:e2e-test"]
    },
    {
      "id": "maybe",
      "title": "PTMaybe $TEST_TAG",
      "type": "task",
      "condition": "{{run_maybe}} == true",
      "depends_on": ["always"],
      "labels": ["project:e2e-test"]
    },
    {
      "id": "after",
      "title": "PTAfter $TEST_TAG",
      "type": "task",
      "depends_on": ["maybe"],
      "labels": ["project:e2e-test"]
    }
  ]
}
FJSON
)
  local skip_fid
  skip_fid=$(create_formula "E2E pour-dangling condition-skip $TEST_TAG" "$skip_json")
  if [ -z "$skip_fid" ]; then
    fail "scenario 12D: create condition-skip formula" "no id returned"
  else
    local skip_out skip_rc=0
    skip_out=$("$KD_BIN" formula apply "$skip_fid" 2>&1) || skip_rc=$?
    for id in $(extract_ids "$skip_out"); do
      [ "$id" = "$skip_fid" ] || CLEANUP_BEADS+=("$id")
    done
    assert_exit "12D: depends_on a condition-skipped step is NOT dangling (pours)" \
      0 "$skip_rc"
    assert_not_contains "12D: no ungated-molecule rejection (false-positive control)" \
      "ungated molecule" "$skip_out"
    # The skipped step must genuinely be absent, and `after` present — proving
    # the ref was stripped by the skip, not left dangling.
    assert_not_contains "12D: skipped step (PTMaybe) not created" "PTMaybe " "$skip_out"
    assert_contains "12D: downstream step (PTAfter) created" "PTAfter " "$skip_out"
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
