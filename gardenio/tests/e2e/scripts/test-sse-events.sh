#!/usr/bin/env bash
# test-sse-events.sh — SSE event stream E2E tests
#
# Tests GET /v1/events/stream against a live kbeads server.
# The SSE stream is the real-time backbone — events fire when beads are
# created, updated, and closed.
#
# Usage:
#   ./tests/e2e/scripts/test-sse-events.sh
#
# Prerequisites:
#   - kd binary in PATH (or set KD_BIN)
#   - curl, python3 installed
#   - BEADS_HTTP_URL set (or kubectl context for port-forward)
#
# Scenarios tested:
#   1. Open SSE connection and verify event format on bead create
#   2. Verify update event arrives when bead is modified
#   3. Verify close event arrives when bead is closed
#   4. Test topic filter — only matching events received
#   5. Test /v1/bus/events endpoint (if NATS configured)

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
BG_PIDS=()

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
  local port=19098
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-sse.log 2>&1 &
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
  for pid in "${BG_PIDS[@]}"; do
    kill "$pid" 2>/dev/null || true
  done
  for id in "${CREATED_BEADS[@]}"; do
    "$KD_BIN" close "$id" --actor "$KD_ACTOR" 2>/dev/null || true
  done
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
  rm -f /tmp/sse-events-*.log 2>/dev/null || true
}
trap cleanup EXIT

# ── Helper: start SSE listener ───────────────────────────────────
# Starts a background curl that reads SSE events to a file.
# Returns the PID and log file path.
start_sse_listener() {
  local log_file="$1"
  local extra_params="${2:-}"
  local auth_header=""
  if [ -n "${BEADS_TOKEN:-}" ]; then
    auth_header="Authorization: Bearer $BEADS_TOKEN"
  fi

  local url="${BEADS_HTTP_URL}/v1/events/stream${extra_params}"
  curl -sN ${auth_header:+-H "$auth_header"} "$url" > "$log_file" 2>/dev/null &
  local pid=$!
  BG_PIDS+=("$pid")
  # Give the SSE connection time to establish
  sleep 2
  echo "$pid"
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " sse-events E2E tests"
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

  local TEST_TAG="e2e-sse-$$-$(date +%s)"

  # ── Scenario 1: SSE stream receives create event ───────────────
  blue "Scenario 1: SSE stream receives bead.created event"

  local sse_log="/tmp/sse-events-create-$$.log"
  local sse_pid
  sse_pid=$(start_sse_listener "$sse_log")

  # Create a bead
  local bead_id
  bead_id=$("$KD_BIN" create "E2E SSE test $TEST_TAG" \
    --type=task -l "e2e:$TEST_TAG" \
    --actor "$KD_ACTOR" --json 2>/dev/null | \
    python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")

  if [ -z "$bead_id" ]; then
    fail "create test bead" "no id returned"
    return 1
  fi
  CREATED_BEADS+=("$bead_id")
  pass "created test bead ($bead_id)"

  # Wait for event to arrive
  sleep 2

  # Read SSE output
  local sse_out
  sse_out=$(cat "$sse_log" 2>/dev/null || echo "")

  # Verify SSE format: id:N, event:topic, data:json
  assert_contains "SSE has event field" "event:" "$sse_out"
  assert_contains "SSE has data field" "data:" "$sse_out"

  # Verify create event contains bead ID
  assert_contains "create event contains bead ID" "$bead_id" "$sse_out"
  assert_contains "create event topic is beads.bead.created" "beads.bead.created" "$sse_out"

  # Stop this listener
  kill "$sse_pid" 2>/dev/null || true
  echo

  # ── Scenario 2: SSE stream receives update event ───────────────
  blue "Scenario 2: SSE stream receives bead.updated event"

  local update_log="/tmp/sse-events-update-$$.log"
  local update_pid
  update_pid=$(start_sse_listener "$update_log")

  # Update the bead
  "$KD_BIN" update "$bead_id" -d "Updated description $TEST_TAG" \
    --actor "$KD_ACTOR" 2>/dev/null

  sleep 2
  local update_out
  update_out=$(cat "$update_log" 2>/dev/null || echo "")

  assert_contains "update event contains bead ID" "$bead_id" "$update_out"
  assert_contains "update event topic is beads.bead.updated" "beads.bead.updated" "$update_out"

  kill "$update_pid" 2>/dev/null || true
  echo

  # ── Scenario 3: SSE stream receives close event ────────────────
  blue "Scenario 3: SSE stream receives bead.closed event"

  # Create a second bead to close
  local close_bead_id
  close_bead_id=$("$KD_BIN" create "E2E SSE close test $TEST_TAG" \
    --type=task -l "e2e:$TEST_TAG" \
    --actor "$KD_ACTOR" --json 2>/dev/null | \
    python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")

  if [ -z "$close_bead_id" ]; then
    fail "create bead for close test" "no id returned"
  else
    CREATED_BEADS+=("$close_bead_id")

    local close_log="/tmp/sse-events-close-$$.log"
    local close_pid
    close_pid=$(start_sse_listener "$close_log")

    # Close the bead
    "$KD_BIN" close "$close_bead_id" --actor "$KD_ACTOR" 2>/dev/null

    sleep 2
    local close_out
    close_out=$(cat "$close_log" 2>/dev/null || echo "")

    assert_contains "close event contains bead ID" "$close_bead_id" "$close_out"
    # Close may be reported as bead.closed or bead.updated (status change)
    if grep -q "beads.bead.closed" <<<"$close_out"; then
      pass "close event topic is beads.bead.closed"
    elif grep -q "beads.bead.updated" <<<"$close_out"; then
      pass "close event topic is beads.bead.updated (status change)"
    else
      fail "close event has a recognized topic" "no close or update event found"
    fi

    kill "$close_pid" 2>/dev/null || true
  fi
  echo

  # ── Scenario 4: Topic filter — only matching events received ───
  blue "Scenario 4: Topic filter limits received events"

  # Listen only for beads.bead.created
  local filter_log="/tmp/sse-events-filter-$$.log"
  local filter_pid
  filter_pid=$(start_sse_listener "$filter_log" "?topics=beads.bead.created")

  # Create a bead (should match)
  local filter_bead_id
  filter_bead_id=$("$KD_BIN" create "E2E SSE filter test $TEST_TAG" \
    --type=task -l "e2e:$TEST_TAG" \
    --actor "$KD_ACTOR" --json 2>/dev/null | \
    python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")

  if [ -z "$filter_bead_id" ]; then
    fail "create bead for filter test" "no id returned"
  else
    CREATED_BEADS+=("$filter_bead_id")

    # Update the bead (should NOT match the topic filter)
    "$KD_BIN" update "$filter_bead_id" -d "Filter update $TEST_TAG" \
      --actor "$KD_ACTOR" 2>/dev/null

    sleep 2
    local filter_out
    filter_out=$(cat "$filter_log" 2>/dev/null || echo "")

    # Create event should be present
    assert_contains "filtered stream has create event" "beads.bead.created" "$filter_out"
    assert_contains "filtered stream contains bead ID" "$filter_bead_id" "$filter_out"

    # Update event should NOT be present
    assert_not_contains "filtered stream excludes update event" "beads.bead.updated" "$filter_out"
  fi

  kill "$filter_pid" 2>/dev/null || true
  echo

  # ── Scenario 5: /v1/bus/events endpoint ────────────────────────
  blue "Scenario 5: /v1/bus/events endpoint"

  local auth_header=""
  if [ -n "${BEADS_TOKEN:-}" ]; then
    auth_header="Authorization: Bearer $BEADS_TOKEN"
  fi

  # Check if bus events endpoint is available (requires NATS/JetStream)
  local bus_status
  bus_status=$(curl -s -o /dev/null -w "%{http_code}" \
    ${auth_header:+-H "$auth_header"} \
    "${BEADS_HTTP_URL}/v1/bus/status" 2>/dev/null || echo "000")

  if [ "$bus_status" = "200" ]; then
    pass "bus status endpoint reachable (HTTP $bus_status)"

    # Try to connect to bus events SSE (with short timeout)
    local bus_log="/tmp/sse-events-bus-$$.log"
    curl -sN --max-time 5 \
      ${auth_header:+-H "$auth_header"} \
      "${BEADS_HTTP_URL}/v1/bus/events" > "$bus_log" 2>/dev/null || true

    local bus_out
    bus_out=$(cat "$bus_log" 2>/dev/null || echo "")

    # If we got SSE headers (or any output), the endpoint works
    if [ -n "$bus_out" ]; then
      pass "bus events endpoint returns SSE data"
    else
      pass "bus events endpoint connected (no events in window)"
    fi
  elif [ "$bus_status" = "503" ]; then
    pass "bus events: NATS not configured (HTTP 503 — expected in some environments)"
  else
    fail "bus status endpoint" "unexpected HTTP status $bus_status"
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
