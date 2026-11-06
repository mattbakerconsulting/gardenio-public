#!/usr/bin/env bash
# test-config-subscriptions.sh — subscription building & filtering E2E tests
#
# Tests that buildSubscriptions() and config resolution correctly build
# subscription labels from environment variables and filter config beads.
#
# Scenarios tested:
#   1. No env vars → subscriptions=[global]
#   2. BOAT_ROLE=crew → subscriptions=[global, role:crew]
#   3. BOAT_ROLE=thread,crew → subscriptions=[global, role:thread, role:crew]
#   4. BOAT_PROJECT=gardenio → subscriptions=[global, project:gardenio]
#   5. KD_PROJECT=gardenio → subscriptions=[global, project:gardenio]
#   6. All set: BOAT_ROLE=crew, BOAT_PROJECT=gardenio → [global, role:crew, project:gardenio]
#   7. Verify correct config beads are matched for each subscription set

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
  local name="$1" expected="$2" actual="$3"
  if [ "$actual" -eq "$expected" ]; then
    pass "$name (exit $expected)"
  else
    fail "$name" "expected exit $expected, got exit $actual"
  fi
}

assert_contains() {
  local name="$1" pattern="$2" actual="$3"
  if grep -qF -- "$pattern" <<< "$actual"; then
    pass "$name (contains '$pattern')"
  else
    fail "$name" "expected to contain '$pattern'; got: $(echo "$actual" | head -5)"
  fi
}

assert_not_contains() {
  local name="$1" pattern="$2" actual="$3"
  if ! grep -qF -- "$pattern" <<< "$actual"; then
    pass "$name (not contains '$pattern')"
  else
    fail "$name" "expected NOT to contain '$pattern'"
  fi
}

# ── Setup ─────────────────────────────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19112
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-subscriptions.log 2>&1 &
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
    "$KD_BIN" close "$id" 2>/dev/null || true
  done
  [ -d "${WORK_DIR:-}" ] && rm -rf "$WORK_DIR"
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
  if [ -n "${LOCAL_PF_PID:-}" ]; then
    kill "$LOCAL_PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

WORK_DIR=$(mktemp -d /tmp/e2e-subscriptions-XXXXXX)
HOME_DIR=$(eval echo "~")
SETTINGS_PATH="$HOME_DIR/.claude/settings.json"

create_config() {
  local title="$1" label="$2" value="$3"
  local out id
  # --project "" suppresses the auto-applied project:$BOAT_PROJECT label so the
  # bead carries only the scope label passed in $label. Without this, role:* beads
  # also get project:gardenio (from the agent's $BOAT_PROJECT), and ungrouped-label
  # OR semantics make them match any [global, project:gardenio] subscriber.
  out=$("$KD_BIN" create "$title" --type=config --project "" -l "$label" \
    -d "$value" --json 2>/dev/null || echo "{}")
  id=$(echo "$out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  [ -n "$id" ] && CREATED_BEADS+=("$id")
  echo "$id"
}

# cleanup_stale_beads closes any leftover claude-settings config beads from
# prior interrupted runs of this test. Identified by the "e2e-sub-" marker
# prefix written into the bead's description. Without this, stale role:*
# beads from earlier runs leak into assertions (their MARKER strings remain
# visible and assert_not_contains hits them even after we create fresh ones).
cleanup_stale_beads() {
  local stale
  # --limit 500 because kd list defaults to 20; stale beads from interrupted
  # prior runs accumulate past that cap and get missed otherwise.
  stale=$("$KD_BIN" list --type=config --status=open --limit 500 --json 2>/dev/null | \
    python3 -c "
import sys, json
try: beads = json.load(sys.stdin)
except Exception: sys.exit(0)
for b in beads:
    if b.get('title') == 'claude-settings' and 'e2e-sub-' in (b.get('description') or ''):
        print(b['id'])
" 2>/dev/null || true)
  if [ -n "$stale" ]; then
    local n=0
    for id in $stale; do
      "$KD_BIN" close "$id" >/dev/null 2>&1 && n=$((n+1))
    done
    dim "  closed $n stale e2e config bead(s) from prior runs"
  fi
}

# reset_workspace clears workspace and user settings between scenarios.
reset_workspace() {
  rm -rf "$WORK_DIR/.claude" "$WORK_DIR/.mcp.json" "$WORK_DIR/CLAUDE.md"
  mkdir -p "$WORK_DIR/.claude"
  rm -f "$SETTINGS_PATH"
}

# ensure_daemon_reachable refreshes BEADS_HTTP_URL when the inherited shard-level
# port-forward (managed by run-shard.sh) dies mid-module. The shard runner only
# health-checks the tunnel between modules — when the beads pod's network sandbox
# rotates partway through this test, gb setup claude silently fails (errors are
# suppressed) and settings.json is never written, surfacing as "user settings: not
# created" failures from S6 onward. See gd-f2vO398ZTA: tag 2026.115.2 run
# 4829f9f8e7294450a04c5587fb53b16d shard 3, sandbox-not-found error mid-S5.
ensure_daemon_reachable() {
  if curl -sf --max-time 2 -o /dev/null "${BEADS_HTTP_URL}/v1/health" 2>/dev/null; then
    return 0
  fi
  red "  daemon unreachable at $BEADS_HTTP_URL — refreshing test-local port-forward"
  if [ -n "${LOCAL_PF_PID:-}" ]; then
    kill "$LOCAL_PF_PID" 2>/dev/null || true
    wait "$LOCAL_PF_PID" 2>/dev/null || true
  fi
  local port=19112
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-subscriptions-recover.log 2>&1 &
  LOCAL_PF_PID=$!
  sleep 3
  export BEADS_HTTP_URL="http://localhost:${port}"
  dim "  daemon now at $BEADS_HTTP_URL (pid $LOCAL_PF_PID)"
}

# read_user_settings returns the content of ~/.claude/settings.json.
read_user_settings() {
  if [ -f "$SETTINGS_PATH" ]; then
    cat "$SETTINGS_PATH"
  else
    echo ""
  fi
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " subscription building & filtering E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"

  cleanup_stale_beads
  echo

  local TEST_TAG="e2e-sub-$$-$(date +%s)"

  # ── Prerequisite: hooks bead so gb setup claude succeeds ───────
  local hooks_id
  hooks_id=$(create_config "claude-hooks" "global" \
    '{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"echo sub-test"}]}],"Stop":[{"matcher":"","hooks":[{"type":"command","command":"echo stop"}]}]}}')

  # ── Create scoped config beads with unique markers ─────────────
  blue "Setup: Creating config beads with unique markers"

  local global_id crew_id thread_id proj_id gardener_id
  global_id=$(create_config "claude-settings" "global" \
    "{\"env\":{\"GLOBAL_MARKER\":\"global-$TEST_TAG\"}}")
  [ -n "$global_id" ] && dim "  global bead: $global_id"

  crew_id=$(create_config "claude-settings" "role:crew" \
    "{\"env\":{\"CREW_MARKER\":\"crew-$TEST_TAG\"}}")
  [ -n "$crew_id" ] && dim "  role:crew bead: $crew_id"

  thread_id=$(create_config "claude-settings" "role:thread" \
    "{\"env\":{\"THREAD_MARKER\":\"thread-$TEST_TAG\"}}")
  [ -n "$thread_id" ] && dim "  role:thread bead: $thread_id"

  proj_id=$(create_config "claude-settings" "project:gardenio" \
    "{\"env\":{\"PROJECT_MARKER\":\"project-$TEST_TAG\"}}")
  [ -n "$proj_id" ] && dim "  project:gardenio bead: $proj_id"

  gardener_id=$(create_config "claude-settings" "role:gardener" \
    "{\"env\":{\"GARDENER_MARKER\":\"gardener-$TEST_TAG\"}}")
  [ -n "$gardener_id" ] && dim "  role:gardener bead: $gardener_id"
  echo

  # ── Scenario 1: No env vars → subscriptions=[global] ──────────
  blue "Scenario 1: No env vars → subscriptions=[global]"

  unset BOAT_ROLE BOAT_PROJECT KD_PROJECT 2>/dev/null || true
  reset_workspace
  ensure_daemon_reachable
  "$GB_BIN" setup claude --role "" --workspace "$WORK_DIR" 2>/dev/null || true

  local content
  content=$(read_user_settings)
  if [ -n "$content" ]; then
    assert_contains "S1: global bead resolved" "GLOBAL_MARKER" "$content"
    assert_not_contains "S1: crew bead NOT resolved" "CREW_MARKER" "$content"
    assert_not_contains "S1: thread bead NOT resolved" "THREAD_MARKER" "$content"
    assert_not_contains "S1: project bead NOT resolved" "PROJECT_MARKER" "$content"
    assert_not_contains "S1: gardener bead NOT resolved" "GARDENER_MARKER" "$content"
  else
    fail "S1: user settings" "not created"
  fi
  echo

  # ── Scenario 2: BOAT_ROLE=crew → [global, role:crew] ──────────
  blue "Scenario 2: BOAT_ROLE=crew → subscriptions=[global, role:crew]"

  unset BOAT_PROJECT KD_PROJECT 2>/dev/null || true
  reset_workspace
  ensure_daemon_reachable
  "$GB_BIN" setup claude --role "crew" --workspace "$WORK_DIR" 2>/dev/null || true

  content=$(read_user_settings)
  if [ -n "$content" ]; then
    assert_contains "S2: global bead resolved" "GLOBAL_MARKER" "$content"
    assert_contains "S2: crew bead resolved" "CREW_MARKER" "$content"
    assert_not_contains "S2: thread bead NOT resolved" "THREAD_MARKER" "$content"
    assert_not_contains "S2: project bead NOT resolved" "PROJECT_MARKER" "$content"
    assert_not_contains "S2: gardener bead NOT resolved" "GARDENER_MARKER" "$content"
  else
    fail "S2: user settings" "not created"
  fi
  echo

  # ── Scenario 3: BOAT_ROLE=thread,crew → [global, role:thread, role:crew]
  blue "Scenario 3: BOAT_ROLE=thread,crew → subscriptions=[global, role:thread, role:crew]"

  unset BOAT_PROJECT KD_PROJECT 2>/dev/null || true
  reset_workspace
  ensure_daemon_reachable
  "$GB_BIN" setup claude --role "thread,crew" --workspace "$WORK_DIR" 2>/dev/null || true

  content=$(read_user_settings)
  if [ -n "$content" ]; then
    assert_contains "S3: global bead resolved" "GLOBAL_MARKER" "$content"
    assert_contains "S3: crew bead resolved" "CREW_MARKER" "$content"
    assert_contains "S3: thread bead resolved" "THREAD_MARKER" "$content"
    assert_not_contains "S3: project bead NOT resolved" "PROJECT_MARKER" "$content"
    assert_not_contains "S3: gardener bead NOT resolved" "GARDENER_MARKER" "$content"
  else
    fail "S3: user settings" "not created"
  fi
  echo

  # ── Scenario 4: BOAT_PROJECT=gardenio → [global, project:gardenio]
  blue "Scenario 4: BOAT_PROJECT=gardenio → subscriptions=[global, project:gardenio]"

  export BOAT_PROJECT=gardenio
  unset KD_PROJECT 2>/dev/null || true
  reset_workspace
  ensure_daemon_reachable
  "$GB_BIN" setup claude --role "" --workspace "$WORK_DIR" 2>/dev/null || true

  content=$(read_user_settings)
  if [ -n "$content" ]; then
    assert_contains "S4: global bead resolved" "GLOBAL_MARKER" "$content"
    assert_contains "S4: project bead resolved" "PROJECT_MARKER" "$content"
    assert_not_contains "S4: crew bead NOT resolved" "CREW_MARKER" "$content"
    assert_not_contains "S4: thread bead NOT resolved" "THREAD_MARKER" "$content"
  else
    fail "S4: user settings" "not created"
  fi
  unset BOAT_PROJECT
  echo

  # ── Scenario 5: KD_PROJECT=gardenio → [global, project:gardenio]
  blue "Scenario 5: KD_PROJECT=gardenio → subscriptions=[global, project:gardenio]"

  unset BOAT_PROJECT 2>/dev/null || true
  export KD_PROJECT=gardenio
  reset_workspace
  ensure_daemon_reachable
  "$GB_BIN" setup claude --role "" --workspace "$WORK_DIR" 2>/dev/null || true

  content=$(read_user_settings)
  if [ -n "$content" ]; then
    assert_contains "S5: global bead resolved" "GLOBAL_MARKER" "$content"
    assert_contains "S5: project bead resolved via KD_PROJECT" "PROJECT_MARKER" "$content"
    assert_not_contains "S5: crew bead NOT resolved" "CREW_MARKER" "$content"
  else
    fail "S5: user settings" "not created"
  fi
  unset KD_PROJECT
  echo

  # ── Scenario 6: All set → [global, role:crew, project:gardenio] ─
  blue "Scenario 6: All set: BOAT_ROLE=crew, BOAT_PROJECT=gardenio → [global, role:crew, project:gardenio]"

  export BOAT_PROJECT=gardenio
  reset_workspace
  ensure_daemon_reachable
  "$GB_BIN" setup claude --role "crew" --workspace "$WORK_DIR" 2>/dev/null || true

  content=$(read_user_settings)
  if [ -n "$content" ]; then
    assert_contains "S6: global bead resolved" "GLOBAL_MARKER" "$content"
    assert_contains "S6: crew bead resolved" "CREW_MARKER" "$content"
    assert_contains "S6: project bead resolved" "PROJECT_MARKER" "$content"
    assert_not_contains "S6: thread bead NOT resolved" "THREAD_MARKER" "$content"
    assert_not_contains "S6: gardener bead NOT resolved" "GARDENER_MARKER" "$content"
  else
    fail "S6: user settings" "not created"
  fi
  unset BOAT_PROJECT
  echo

  # ── Scenario 7: Verify correct config beads matched per subscription set
  blue "Scenario 7: Verify correct config beads are matched for each subscription set"

  # 7a: role:gardener filtered out when role=crew
  reset_workspace
  ensure_daemon_reachable
  "$GB_BIN" setup claude --role "crew" --workspace "$WORK_DIR" 2>/dev/null || true

  content=$(read_user_settings)
  if [ -n "$content" ]; then
    assert_not_contains "S7a: gardener bead filtered when role=crew" "GARDENER_MARKER" "$content"
    assert_contains "S7a: crew bead matched when role=crew" "CREW_MARKER" "$content"
  else
    fail "S7a: user settings" "not created"
  fi

  # 7b: project:gardenio NOT matched when BOAT_PROJECT is different
  export BOAT_PROJECT=otherproject
  reset_workspace
  ensure_daemon_reachable
  "$GB_BIN" setup claude --role "" --workspace "$WORK_DIR" 2>/dev/null || true

  content=$(read_user_settings)
  if [ -n "$content" ]; then
    assert_not_contains "S7b: gardenio project bead filtered for otherproject" "PROJECT_MARKER" "$content"
    assert_contains "S7b: global bead still resolved" "GLOBAL_MARKER" "$content"
  else
    fail "S7b: user settings" "not created"
  fi
  unset BOAT_PROJECT

  # 7c: KD_PROJECT takes precedence over BOAT_PROJECT
  export KD_PROJECT=gardenio
  export BOAT_PROJECT=otherproject
  reset_workspace
  ensure_daemon_reachable
  "$GB_BIN" setup claude --role "" --workspace "$WORK_DIR" 2>/dev/null || true

  content=$(read_user_settings)
  if [ -n "$content" ]; then
    assert_contains "S7c: KD_PROJECT wins over BOAT_PROJECT" "PROJECT_MARKER" "$content"
  else
    fail "S7c: user settings" "not created"
  fi
  unset BOAT_PROJECT KD_PROJECT
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
