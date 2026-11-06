#!/usr/bin/env bash
# test-bead-transfer.sh — bead transfer API E2E tests
#
# Tests POST /v1/beads/{id}/transfer against a live kbeads server.
# Transfer moves a bead between fleets atomically.
#
# Usage:
#   ./tests/e2e/scripts/test-bead-transfer.sh
#
# Prerequisites:
#   - kd binary in PATH (or set KD_BIN)
#   - curl, python3 installed
#   - BEADS_HTTP_URL set (or kubectl context for port-forward)
#   - BEADS_ADMIN_OIDC_TOKEN: an OIDC gardenio-admin access token. Transfer is
#     OIDC-admin only (gd-i6GZX5jzHM); without it only the static-token
#     refusal is checked.
#
# Scenarios tested:
#   1. Create a bead in fleet A, transfer to fleet B
#   2. Verify the bead's fleet changed after transfer
#   3. Verify the transfer event is recorded in event log
#   4. Verify transfer with missing to_fleet returns 400
#   5. Verify transfer of non-existent bead returns 404

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
  local port=19096
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-transfer.log 2>&1 &
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
# Build the standard curl header arg list. The kd CLI auto-injects
# Authorization, X-Actor, and X-Operated-By; raw curl callers must do the same
# so writes pass the R2 fail-closed operator resolver (ka-0000005772). Without
# X-Operated-By, mutation handlers skip RecordEvent (server.go), so the
# bead.transferred event never lands in /v1/beads/{id}/events and Scenario 3
# fails even though the transfer itself succeeded.
_api_headers() {
  local -a headers=(-H "Content-Type: application/json")
  if [ -n "${BEADS_TOKEN:-}" ]; then
    headers+=(-H "Authorization: Bearer ${BEADS_TOKEN}")
  fi
  if [ -n "${BEADS_OPERATED_BY:-}" ]; then
    headers+=(-H "X-Operated-By: ${BEADS_OPERATED_BY}")
  fi
  if [ -n "${KD_ACTOR:-}" ]; then
    headers+=(-H "X-Actor: ${KD_ACTOR}")
  fi
  printf '%s\n' "${headers[@]}"
}

api_get() {
  local path="$1"
  local -a headers=()
  mapfile -t headers < <(_api_headers)
  curl -sf "${headers[@]}" \
    "${BEADS_HTTP_URL}${path}" 2>/dev/null || echo "{}"
}

api_post() {
  local path="$1"
  local body="$2"
  local -a headers=()
  mapfile -t headers < <(_api_headers)
  curl -sf -X POST "${headers[@]}" \
    -d "$body" \
    "${BEADS_HTTP_URL}${path}" 2>/dev/null || echo "{}"
}

# Like api_post but returns HTTP status code
api_post_status() {
  local path="$1"
  local body="$2"
  local -a headers=()
  mapfile -t headers < <(_api_headers)
  curl -s -o /dev/null -w "%{http_code}" -X POST \
    "${headers[@]}" \
    -d "$body" \
    "${BEADS_HTTP_URL}${path}" 2>/dev/null || echo "000"
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " bead-transfer E2E tests"
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

  local TEST_TAG="e2e-transfer-$$-$(date +%s)"
  local FLEET_A="e2e-fleet-a-$TEST_TAG"
  local FLEET_B="e2e-fleet-b-$TEST_TAG"

  # ── Scenario 1: Create bead and transfer between fleets ────────
  blue "Scenario 1: Create bead in fleet A, transfer to fleet B"

  local bead_id
  bead_id=$("$KD_BIN" create "E2E transfer test $TEST_TAG" \
    --type=task -l "e2e:$TEST_TAG" \
    -d "Automated transfer test bead — safe to delete" \
    --actor "$KD_ACTOR" --json 2>/dev/null | \
    python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")

  if [ -z "$bead_id" ]; then
    fail "create test bead" "no id returned"
    return 1
  fi
  pass "created test bead ($bead_id)"
  CREATED_BEADS+=("$bead_id")

  # Probe: check if transfer API exists
  local probe_status
  probe_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
    -H "Content-Type: application/json" \
    ${BEADS_TOKEN:+-H "Authorization: Bearer $BEADS_TOKEN"} \
    -d "{\"to_fleet\":\"probe\"}" \
    "${BEADS_HTTP_URL}/v1/beads/${bead_id}/transfer" 2>/dev/null || echo "000")
  if [ "$probe_status" = "404" ]; then
    red "SKIP: kd server does not support bead transfer API."
    red "  Deploy a kbeads image with the transfer endpoint first."
    return 0
  fi

  # Transfer is OIDC-admin only (gd-i6GZX5jzHM): the static token is refused,
  # and the remaining scenarios need an OIDC gardenio-admin bearer.
  assert_eq "static token is refused transfer" "403" "$probe_status"
  if [ -z "${BEADS_ADMIN_OIDC_TOKEN:-}" ]; then
    red "SKIP: set BEADS_ADMIN_OIDC_TOKEN (an OIDC gardenio-admin access token) to run the transfer scenarios."
    [ "$FAIL" -eq 0 ]
    return
  fi
  export BEADS_TOKEN="$BEADS_ADMIN_OIDC_TOKEN"

  # Transfer to fleet B
  local transfer_out
  transfer_out=$(api_post "/v1/beads/${bead_id}/transfer" \
    "{\"to_fleet\":\"$FLEET_B\"}")
  assert_contains "transfer response contains bead ID" "$bead_id" "$transfer_out"
  echo

  # ── Scenario 2: Verify the bead's fleet changed ────────────────
  blue "Scenario 2: Verify bead fleet changed after transfer"

  local show_out
  show_out=$("$KD_BIN" show "$bead_id" --json 2>/dev/null || echo "{}")

  local bead_fleet
  bead_fleet=$(echo "$show_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('fleet',''))" 2>/dev/null || echo "")
  assert_eq "bead fleet is now fleet B" "$FLEET_B" "$bead_fleet"
  echo

  # ── Scenario 3: Verify transfer event is recorded ──────────────
  blue "Scenario 3: Verify transfer event in event log"

  local events_out
  events_out=$(api_get "/v1/beads/${bead_id}/events")
  assert_contains "events contain bead.transferred" "beads.bead.transferred" "$events_out"

  # Verify event payload includes from/to fleet
  assert_contains "transfer event contains to_fleet" "$FLEET_B" "$events_out"
  echo

  # ── Scenario 4: Transfer with missing to_fleet returns 400 ─────
  blue "Scenario 4: Missing to_fleet returns 400"

  local status_missing
  status_missing=$(api_post_status "/v1/beads/${bead_id}/transfer" '{}')
  assert_eq "missing to_fleet returns 400" "400" "$status_missing"
  echo

  # ── Scenario 5: Transfer non-existent bead returns 404 ─────────
  blue "Scenario 5: Non-existent bead returns 404"

  local status_notfound
  status_notfound=$(api_post_status "/v1/beads/gd-nonexistent-transfer-test/transfer" \
    "{\"to_fleet\":\"$FLEET_B\"}")
  assert_eq "non-existent bead returns 404" "404" "$status_notfound"
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
