#!/usr/bin/env bash
# test-graph-stats.sh — graph traversal and stats API E2E tests
#
# Tests /v1/graph, /v1/stats, and kd tree against a live kbeads server.
#
# Usage:
#   ./tests/e2e/scripts/test-graph-stats.sh
#
# Prerequisites:
#   - kd binary in PATH (or set KD_BIN)
#   - curl, jq, and python3 installed
#   - BEADS_HTTP_URL set (or kubectl context for port-forward)
#
# Scenarios tested:
#   1. Create a chain of beads with dependencies (A blocks B blocks C)
#   2. Query /v1/graph and verify the dependency graph is returned correctly
#   3. Verify kd tree output renders the hierarchy
#   4. Query /v1/stats and verify it returns bead counts by status
#   5. Verify stats update after creating/closing beads

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
  local port=19099
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-graph.log 2>&1 &
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
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# ── Helpers: HTTP API calls ──────────────────────────────────────
api_get() {
  local path="$1"
  local auth_header=""
  if [ -n "${BEADS_TOKEN:-}" ]; then
    auth_header="Authorization: Bearer $BEADS_TOKEN"
  fi
  curl -sf -H "Content-Type: application/json" \
    ${auth_header:+-H "$auth_header"} \
    "${BEADS_HTTP_URL}${path}" 2>/dev/null || echo "{}"
}

api_post() {
  local path="$1"
  local body="$2"
  local auth_header=""
  if [ -n "${BEADS_TOKEN:-}" ]; then
    auth_header="Authorization: Bearer $BEADS_TOKEN"
  fi
  curl -sf -X POST -H "Content-Type: application/json" \
    ${auth_header:+-H "$auth_header"} \
    -d "$body" \
    "${BEADS_HTTP_URL}${path}" 2>/dev/null || echo "{}"
}

# Helper to extract ID from kd create --json output
extract_id() {
  python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " graph-stats E2E tests"
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

  local TEST_TAG="e2e-graph-$$-$(date +%s)"

  # ── Scenario 1: Create a chain of beads with dependencies ──────
  blue "Scenario 1: Create bead chain A blocks B blocks C"

  local id_a id_b id_c

  id_a=$("$KD_BIN" create "E2E graph A $TEST_TAG" \
    --type=task -l "e2e:$TEST_TAG" \
    -d "Chain root — blocker A" \
    --actor "$KD_ACTOR" --json 2>/dev/null | extract_id)

  if [ -z "$id_a" ]; then
    fail "create bead A" "no id returned"
    return 1
  fi
  pass "created bead A ($id_a)"
  CREATED_BEADS+=("$id_a")

  id_b=$("$KD_BIN" create "E2E graph B $TEST_TAG" \
    --type=task -l "e2e:$TEST_TAG" \
    -d "Middle — blocked by A, blocks C" \
    --actor "$KD_ACTOR" --json 2>/dev/null | extract_id)

  if [ -z "$id_b" ]; then
    fail "create bead B" "no id returned"
    return 1
  fi
  pass "created bead B ($id_b)"
  CREATED_BEADS+=("$id_b")

  id_c=$("$KD_BIN" create "E2E graph C $TEST_TAG" \
    --type=task -l "e2e:$TEST_TAG" \
    -d "Tail — blocked by B" \
    --actor "$KD_ACTOR" --json 2>/dev/null | extract_id)

  if [ -z "$id_c" ]; then
    fail "create bead C" "no id returned"
    return 1
  fi
  pass "created bead C ($id_c)"
  CREATED_BEADS+=("$id_c")

  # Add blocking dependencies: B is blocked by A, C is blocked by B
  local dep_rc
  "$KD_BIN" dep add "$id_b" "$id_a" --type blocks 2>/dev/null && dep_rc=0 || dep_rc=$?
  assert_exit "dep add B blocked-by A" 0 "$dep_rc"

  "$KD_BIN" dep add "$id_c" "$id_b" --type blocks 2>/dev/null && dep_rc=0 || dep_rc=$?
  assert_exit "dep add C blocked-by B" 0 "$dep_rc"
  echo

  # ── Scenario 2: Query /v1/graph and verify the graph ───────────
  blue "Scenario 2: Query /v1/graph"

  # Use POST with label filter to scope to our test beads
  local graph_out
  graph_out=$(api_post "/v1/graph" "{\"labels\":[\"e2e:$TEST_TAG\"],\"status\":[\"open\",\"in_progress\",\"blocked\"],\"include_deps\":true}")

  # Verify nodes contain our beads
  assert_contains "graph contains bead A" "$id_a" "$graph_out"
  assert_contains "graph contains bead B" "$id_b" "$graph_out"
  assert_contains "graph contains bead C" "$id_c" "$graph_out"

  # Verify exactly 3 nodes carry our test label. We count LABEL-SCOPED nodes,
  # not the total node count, because include_deps:true is *designed* to pull
  # dependency targets into the node set even when they fall outside the label
  # filter (that is the visualization contract, and the API is behaving
  # correctly). A/B/C acquire an out-of-label parent-child edge whenever the
  # creating caller has an active task — e.g. an agent-context run where
  # `kd create` defaults --parent-task=$BOAT_TASK_ID, or the server-side
  # auto-parent step firing for a static-token agent carrying a task_id — and
  # a genuine concurrent mutation on a busy cluster can pull in yet more
  # dependency neighbours. Any of those inflate the *total* node count past 3
  # while the label-scoped count stays deterministically 3. This mirrors the
  # Scenario-5 hardening below ("label-scoped counts are deterministic; global
  # counts are racy"). include_deps:true stays on so the dep-pulling path is
  # still exercised.
  local scoped_node_count
  scoped_node_count=$(echo "$graph_out" | python3 -c "
import sys, json
tag = '$TEST_TAG'
nodes = json.load(sys.stdin).get('nodes', [])
print(sum(1 for n in nodes if ('e2e:' + tag) in (n.get('labels') or [])))
" 2>/dev/null || echo "0")
  assert_eq "graph label-scoped node count is 3" "3" "$scoped_node_count"

  # Verify edges exist (blocks dependencies)
  assert_contains "graph contains edges" '"edges"' "$graph_out"

  local edge_count
  edge_count=$(echo "$graph_out" | python3 -c "import sys,json; print(len(json.load(sys.stdin).get('edges',[])))" 2>/dev/null || echo "0")
  if [ "$edge_count" -ge 2 ]; then
    pass "graph has >= 2 edges (got $edge_count)"
  else
    fail "graph has >= 2 edges" "got $edge_count"
  fi

  # Verify edge types are blocks
  assert_contains "graph edges have blocks type" "blocks" "$graph_out"

  # Verify graph response has stats
  assert_contains "graph response has stats" '"stats"' "$graph_out"
  echo

  # ── Scenario 3: Verify kd tree renders the hierarchy ───────────
  blue "Scenario 3: kd tree renders hierarchy"

  # kd tree shows forward dependencies (what a bead depends ON)
  local tree_out tree_rc
  tree_out=$("$KD_BIN" tree "$id_b" 2>/dev/null) && tree_rc=0 || tree_rc=$?
  assert_exit "kd tree B exits 0" 0 "$tree_rc"

  # B depends on A (blocks), so A should appear in the tree output
  assert_contains "tree B shows bead A" "$id_a" "$tree_out"

  # Test kd tree --flat for JSON output
  local flat_out flat_rc
  flat_out=$("$KD_BIN" tree "$id_c" --flat --json 2>/dev/null) && flat_rc=0 || flat_rc=$?
  assert_exit "kd tree C --flat --json exits 0" 0 "$flat_rc"

  # C depends on B, so B should appear
  assert_contains "tree C flat shows bead B" "$id_b" "$flat_out"
  assert_contains "tree C flat shows blocks type" "blocks" "$flat_out"
  echo

  # ── Scenario 4: Query /v1/stats and verify counts ──────────────
  blue "Scenario 4: Query /v1/stats"

  local stats_out
  stats_out=$(api_get "/v1/stats")

  # Verify stats contains expected fields
  assert_contains "stats has total_open" "total_open" "$stats_out"
  assert_contains "stats has total_in_progress" "total_in_progress" "$stats_out"
  assert_contains "stats has total_closed" "total_closed" "$stats_out"
  assert_contains "stats has total_deferred" "total_deferred" "$stats_out"

  # total_open should be > 0 since we just created 3 beads
  local total_open
  total_open=$(echo "$stats_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('total_open',0))" 2>/dev/null || echo "0")
  if [ "$total_open" -gt 0 ]; then
    pass "stats total_open > 0 (got $total_open)"
  else
    fail "stats total_open > 0" "got $total_open"
  fi

  # Capture current closed count for the delta check in scenario 5
  local closed_before
  closed_before=$(echo "$stats_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('total_closed',0))" 2>/dev/null || echo "0")
  dim "  stats before close: open=$total_open, closed=$closed_before"
  echo

  # ── Scenario 5: Verify stats update after closing beads ────────
  blue "Scenario 5: Verify stats update after closing beads"

  # Label-scoped open count BEFORE close: should be 3 (A, B, C). Label-scoped
  # counts are deterministic; global /v1/stats total_open is racy on busy
  # clusters where concurrent agents create beads faster than we close ours.
  local scoped_open_before
  scoped_open_before=$("$KD_BIN" list -l "e2e:$TEST_TAG" --status=open --all-projects --limit 100 --json 2>/dev/null \
    | python3 -c "import sys,json; print(len(json.load(sys.stdin)))" 2>/dev/null || echo "0")
  dim "  scoped open before close: $scoped_open_before"

  # Close bead A
  "$KD_BIN" close "$id_a" 2>/dev/null
  CREATED_BEADS=("${CREATED_BEADS[@]/$id_a/}")

  local stats_after
  stats_after=$(api_get "/v1/stats")

  local closed_after
  closed_after=$(echo "$stats_after" | python3 -c "import sys,json; print(json.load(sys.stdin).get('total_closed',0))" 2>/dev/null || echo "0")
  dim "  stats after close: closed=$closed_after (was $closed_before)"

  if [ "$closed_after" -gt "$closed_before" ]; then
    pass "stats total_closed increased after closing bead ($closed_before -> $closed_after)"
  else
    fail "stats total_closed increased" "before=$closed_before, after=$closed_after"
  fi

  local scoped_open_after
  scoped_open_after=$("$KD_BIN" list -l "e2e:$TEST_TAG" --status=open --all-projects --limit 100 --json 2>/dev/null \
    | python3 -c "import sys,json; print(len(json.load(sys.stdin)))" 2>/dev/null || echo "0")
  dim "  scoped open after close: $scoped_open_after"

  if [ "$scoped_open_after" -lt "$scoped_open_before" ]; then
    pass "scoped open count decreased after closing bead ($scoped_open_before -> $scoped_open_after)"
  else
    fail "scoped open count decreased" "before=$scoped_open_before, after=$scoped_open_after"
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
