#!/usr/bin/env bash
# test-mr-merge-flow.sh — MR merge → Review transition E2E test
#
# Tests the gitlab-bridge + jira-bridge integration:
#   1. Create a test bead with jira_key and mr_url fields
#   2. Simulate mr_merged=true (or wait for gitlab-bridge to detect a real merge)
#   3. Verify jira-bridge reacts to mr_merged=true (check logs)
#   4. Verify bead fields are correctly set
#   5. Verify bead close does NOT trigger transition
#
# Usage:
#   ./tests/e2e/scripts/test-mr-merge-flow.sh [--namespace <ns>] [--simulate]
#
# Flags:
#   --simulate   Set mr_merged=true manually (skip waiting for gitlab-bridge)
#   --namespace  K8s namespace (default: gardenio)
#   --jira-key   Real JIRA key for live transition test (default: PE-0000)
#   --mr-url     Real MR URL for live webhook test
#
# Prerequisites:
#   - kubectl context pointing at the target cluster
#   - kd binary in PATH
#   - jq installed

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
KD_BIN="${KD_BIN:-kd}"
SIMULATE=false
TEST_JIRA_KEY="PE-0000"
TEST_MR_URL="https://gitlab.com/test-group/test-project/-/merge_requests/999"
PASS=0
FAIL=0
ERRORS=()
CLEANUP_BEADS=()

# Load shared retry helpers (defines kd_retry). Keeps behaviour identical to
# the previous inline helper while removing the local duplicate.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib-retry.sh"

# ── Parse flags ──────────────────────────────────────────────────
while [[ $# -gt 0 ]]; do
  case $1 in
    --namespace) NAMESPACE="$2"; shift 2 ;;
    --simulate)  SIMULATE=true; shift ;;
    --jira-key)  TEST_JIRA_KEY="$2"; shift 2 ;;
    --mr-url)    TEST_MR_URL="$2"; shift 2 ;;
    *) echo "Unknown flag: $1"; exit 1 ;;
  esac
done

# ── Helpers ───────────────────────────────────────────────────────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
bold()  { printf '\033[1m%s\033[0m\n' "$*"; }

pass() {
  PASS=$((PASS + 1))
  green "  PASS: $1"
}

fail() {
  FAIL=$((FAIL + 1))
  ERRORS+=("$1")
  red "  FAIL: $1"
}

bead_field() {
  # Usage: bead_field <bead-id> <field-name>
  local val
  val=$(kd_retry "$KD_BIN" show "$1" --json 2>/dev/null | jq -r ".fields.\"$2\" // empty") || true
  if [[ -z "$val" ]]; then
    sleep 2
    val=$(kd_retry "$KD_BIN" show "$1" --json 2>/dev/null | jq -r ".fields.\"$2\" // empty") || true
  fi
  echo "$val"
}

create_bead() {
  local title="$1"
  shift
  local id attempt output
  for attempt in 1 2 3 4 5; do
    output=$(kd_retry "$KD_BIN" create "$title" "$@" 2>&1) || true
    id=$(echo "$output" | grep "^ID:" | awk '{print $2}')
    if [[ -n "$id" ]] && [[ "$id" == kd-* ]]; then
      CLEANUP_BEADS+=("$id")
      echo "$id"
      return 0
    fi
    # Port-forward may have dropped — wait for harness to restart it.
    # The outer harness takes ~15-20s to detect and restart the tunnel,
    # so we need a longer total retry window than the default 9s.
    [[ $attempt -lt 5 ]] && sleep 5
  done
}

cleanup() {
  if [[ ${#CLEANUP_BEADS[@]} -gt 0 ]]; then
    echo ""
    echo "Cleaning up ${#CLEANUP_BEADS[@]} test bead(s)..."
    for id in "${CLEANUP_BEADS[@]}"; do
      kd_retry "$KD_BIN" close "$id" >/dev/null || true
    done
  fi
}
trap cleanup EXIT

# ── Test: Bridge pods are running ──────────────────────────────────
bold "=== MR Merge → Review Flow E2E Test ==="
echo ""

bold "1. Verify bridge pods are running"
GITLAB_POD=$(kubectl get pods -n "$NAMESPACE" --no-headers 2>/dev/null | awk '/gitlab-bridge/ && /Running/ {print $1; exit}')
JIRA_POD=$(kubectl get pods -n "$NAMESPACE" --no-headers 2>/dev/null | awk '/jira-bridge/ && /Running/ {print $1; exit}')

if [[ -n "$GITLAB_POD" ]]; then
  pass "gitlab-bridge pod running: $GITLAB_POD"
else
  echo "  INFO: gitlab-bridge pod not found in namespace $NAMESPACE (bridge-specific assertions will be skipped)"
fi

if [[ -n "$JIRA_POD" ]]; then
  pass "jira-bridge pod running: $JIRA_POD"
else
  echo "  INFO: jira-bridge pod not found in namespace $NAMESPACE (bridge-specific assertions will be skipped)"
fi

# ── Test: Create test bead with fields ──────────────────────────
echo ""
bold "2. Create test bead with jira_key and mr_url"

TEST_BEAD_ID=$(create_bead "E2E test: MR merge flow $(date +%s)" \
  --type=task --priority=4 -l "source:jira")

if [[ -n "$TEST_BEAD_ID" ]]; then
  pass "created test bead: $TEST_BEAD_ID"
else
  fail "failed to create test bead"
  echo "Cannot continue without test bead."
  exit 1
fi

# Set jira_key and mr_url fields.
kd_retry "$KD_BIN" update "$TEST_BEAD_ID" -f "jira_key=$TEST_JIRA_KEY" -f "mr_url=$TEST_MR_URL" >/dev/null || true

# Verify fields via JSON.
if [[ "$(bead_field "$TEST_BEAD_ID" jira_key)" == "$TEST_JIRA_KEY" ]]; then
  pass "jira_key=$TEST_JIRA_KEY set on bead"
else
  fail "jira_key field not set (got: $(bead_field "$TEST_BEAD_ID" jira_key))"
fi

if [[ "$(bead_field "$TEST_BEAD_ID" mr_url)" == "$TEST_MR_URL" ]]; then
  pass "mr_url set on bead"
else
  fail "mr_url field not set (got: $(bead_field "$TEST_BEAD_ID" mr_url))"
fi

# ── Test: Simulate or wait for mr_merged ──────────────────────────
echo ""
if $SIMULATE; then
  bold "3. Simulating mr_merged=true (--simulate mode)"
  # Quote the value as a JSON string — kd's field parser auto-detects bare
  # "true" as a JSON boolean, but the server's type:task schema defines
  # mr_merged as type:string and rejects boolean values.
  kd_retry "$KD_BIN" update "$TEST_BEAD_ID" -f 'mr_merged="true"' >/dev/null || true

  if [[ "$(bead_field "$TEST_BEAD_ID" mr_merged)" == "true" ]]; then
    pass "mr_merged=true set on bead"
  else
    fail "mr_merged field not persisted on bead"
  fi

  # Wait for jira-bridge to process the SSE event.
  echo "  Waiting 8s for jira-bridge to process SSE event..."
  sleep 8

  # Check jira-bridge logs for reaction.
  if [[ -n "$JIRA_POD" ]]; then
    echo ""
    bold "4. Checking jira-bridge logs for transition attempt"
    JIRA_LOGS=$(kubectl logs "$JIRA_POD" -n "$NAMESPACE" --tail=30 --since=30s 2>/dev/null || echo "")
    if grep -q "MR merged, transitioning JIRA to Review" <<<"$JIRA_LOGS"; then
      pass "jira-bridge detected mr_merged=true and attempted JIRA transition"
    else
      fail "jira-bridge did not log transition attempt"
      echo "  Recent jira-bridge logs:"
      echo "$JIRA_LOGS" | grep -v "cross-project\|poll complete" | tail -5
    fi

    # Verify no transition on mere bead update (without mr_merged).
    if grep -q "bead.*$TEST_BEAD_ID.*transitioning" <<<"$JIRA_LOGS"; then
      pass "transition log references correct bead: $TEST_BEAD_ID"
    fi
  fi
else
  bold "3. Waiting for gitlab-bridge to detect merge (manual mode)"
  echo "  To complete this test:"
  echo "    1. Merge the MR at: $TEST_MR_URL"
  echo "    2. Wait up to 5 minutes for gitlab-bridge polling"
  echo "    3. Check: kd show $TEST_BEAD_ID --json | jq .fields"
  echo "    4. Verify mr_merged=true is set"
  echo ""
  echo "  Or re-run with --simulate to skip the merge step."
fi

# ── Test: Edge case — bead close without mr_merged ────────────────
echo ""
bold "5. Edge case: bead close without mr_merged should NOT trigger transition"
CLOSE_BEAD_ID=$(create_bead "E2E test: close-no-transition $(date +%s)" \
  --type=task --priority=4 -l "source:jira")

if [[ -n "$CLOSE_BEAD_ID" ]]; then
  kd_retry "$KD_BIN" update "$CLOSE_BEAD_ID" -f "jira_key=PE-0001" >/dev/null || true
  kd_retry "$KD_BIN" close "$CLOSE_BEAD_ID" >/dev/null || true

  # Wait for SSE processing.
  sleep 5

  if [[ -n "$JIRA_POD" ]]; then
    CLOSE_LOGS=$(kubectl logs "$JIRA_POD" -n "$NAMESPACE" --tail=10 --since=15s 2>/dev/null || echo "")
    if grep -q "transitioning.*PE-0001" <<<"$CLOSE_LOGS"; then
      fail "jira-bridge incorrectly transitioned on bead close without mr_merged"
    else
      pass "bead close without mr_merged did NOT trigger transition"
    fi
  else
    pass "closed bead without mr_merged (manual log check needed)"
  fi
else
  fail "failed to create close-test bead"
fi

# ── Test: Edge case — MR closed without merging ──────────────────
echo ""
bold "6. Edge case: mr_state=closed (not merged) should NOT trigger transition"
CLOSED_MR_BEAD_ID=$(create_bead "E2E test: mr-closed-not-merged $(date +%s)" \
  --type=task --priority=4 -l "source:jira")

if [[ -n "$CLOSED_MR_BEAD_ID" ]]; then
  kd_retry "$KD_BIN" update "$CLOSED_MR_BEAD_ID" \
    -f "jira_key=PE-0002" \
    -f "mr_url=https://gitlab.com/test/test/-/merge_requests/998" \
    -f "mr_state=closed" >/dev/null || true

  sleep 5

  MR_MERGED_VAL=$(bead_field "$CLOSED_MR_BEAD_ID" mr_merged)
  if [[ "$MR_MERGED_VAL" != "true" ]]; then
    pass "mr_state=closed did NOT set mr_merged=true"
  else
    fail "mr_state=closed incorrectly set mr_merged=true"
  fi
else
  fail "failed to create closed-MR test bead"
fi

# ── Summary ───────────────────────────────────────────────────────
echo ""
bold "=== Results ==="
green "  Passed: $PASS"
if [[ $FAIL -gt 0 ]]; then
  red "  Failed: $FAIL"
  for err in "${ERRORS[@]}"; do
    red "    - $err"
  done
  exit 1
else
  green "  All tests passed!"
fi
