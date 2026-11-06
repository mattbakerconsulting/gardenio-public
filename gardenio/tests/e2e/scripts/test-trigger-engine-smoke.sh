#!/usr/bin/env bash
# test-trigger-engine-smoke.sh — trigger-engine deployment + liveness smoke
#
# Verifies that the gardenio-trigger-engine deployment is healthy AND that
# its evaluation loop is actually running by:
#
#   1. Asserting the trigger-engine Deployment is Ready (1/1).
#      Skips cleanly if the deployment is absent (back-compat for namespaces
#      with triggerEngine.enabled=false).
#   2. Creating a sentinel schedule trigger (cron='* * * * *',
#      action=create_bead, last_checked seeded to 1h ago to bypass runtime
#      bootstrap so the very first poll fires the action).
#   3. Polling for the spawned trigger child bead (label trigger:<TRIG_ID>)
#      AND that the trigger's last_checked field has advanced past the
#      creation timestamp — confirms the engine is alive and polling.
#
# Designed to catch ImagePullBackOff and dead-loop regressions before they
# reach prod (see ka-0000002212).
#
# Usage:
#   ./tests/e2e/scripts/test-trigger-engine-smoke.sh
#
# Prerequisites:
#   - kubectl context pointing at the target cluster
#   - kd, gb binaries on PATH
#   - jq / python3 installed
#
# Env:
#   NAMESPACE          (default: gardenio-e2e)
#   BEADS_HTTP_URL     (auto: port-forward to ${NAMESPACE}-beads if unset)
#   BEADS_TOKEN        (auto: read kd-beads-token secret if unset)
#   POLL_TIMEOUT_SEC   (default: 120) — how long to wait for first fire
#   POLL_INTERVAL_SEC  (default: 5)

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
KD_BIN="${KD_BIN:-kd}"
# shellcheck source=SCRIPTDIR/lib-kd-create.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib-kd-create.sh"
DEPLOY_NAME="${NAMESPACE}-trigger-engine"
POLL_TIMEOUT_SEC="${POLL_TIMEOUT_SEC:-120}"
POLL_INTERVAL_SEC="${POLL_INTERVAL_SEC:-5}"

PASS=0
FAIL=0
ERRORS=()
PF_PID=""
CREATED_BEADS=()

export KD_ACTOR="${KD_ACTOR:-e2e-trigger-engine-smoke}"

# ── Helpers ───────────────────────────────────────────────────────
red()    { printf '\033[0;31m%s\033[0m\n' "$*"; }
green()  { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()   { printf '\033[0;34m%s\033[0m\n' "$*"; }
yellow() { printf '\033[0;33m%s\033[0m\n' "$*"; }
dim()    { printf '\033[2m%s\033[0m\n' "$*"; }

pass() { green "  ✓ $1"; PASS=$((PASS + 1)); }
fail() {
  local name="$1" reason="${2:-}"
  red "  ✗ $name"
  [ -n "$reason" ] && dim "    $reason"
  FAIL=$((FAIL + 1))
  ERRORS+=("$name: $reason")
}

cleanup() {
  for id in "${CREATED_BEADS[@]}"; do
    [ -n "$id" ] && "$KD_BIN" close "$id" 2>/dev/null || true
  done
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# ── Setup helpers (mirror test-mail-ready.sh / test-trigger-system.sh) ──
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    dim "  Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  local port=19112
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-trigger-engine-smoke.log 2>&1 &
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

# Extract a JSON field via python3 (matches existing test helpers).
json_field() {
  local body="$1" path="$2"
  echo "$body" | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    print('')
    sys.exit(0)
parts = '$path'.split('.')
for p in parts:
    if isinstance(d, dict) and p in d:
        d = d[p]
    elif isinstance(d, list):
        try:
            d = d[int(p)]
        except Exception:
            d = ''
            break
    else:
        d = ''
        break
print(d if not isinstance(d, (dict, list)) else json.dumps(d))
" 2>/dev/null || echo ""
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " trigger-engine smoke E2E tests"
  blue "═══════════════════════════════════════════════════"
  echo

  # ── Scenario 1: Deployment exists & is Ready ────────────────────
  blue "Scenario 1: trigger-engine deployment is Ready 1/1"
  if ! kubectl -n "$NAMESPACE" get deploy "$DEPLOY_NAME" >/dev/null 2>&1; then
    yellow "  SKIP: deployment $DEPLOY_NAME not present in $NAMESPACE"
    yellow "  (set triggerEngine.enabled=true to enable; this smoke test is a no-op otherwise)"
    blue "═══════════════════════════════════════════════════"
    green " ✓ skipped (deployment absent — back-compat)"
    blue "═══════════════════════════════════════════════════"
    return 0
  fi

  # Wait up to 60s for Available=True. Fail loudly if it doesn't become
  # Available — this catches ImagePullBackOff and crashloop regressions.
  if kubectl -n "$NAMESPACE" wait --for=condition=Available \
       "deploy/$DEPLOY_NAME" --timeout=60s >/dev/null 2>&1; then
    pass "$DEPLOY_NAME deployment Available"
  else
    fail "$DEPLOY_NAME Available" "deployment did not reach Available within 60s — likely ImagePullBackOff or crashloop"
    dim "  --- pod status ---"
    kubectl -n "$NAMESPACE" get pods -l app.kubernetes.io/component=trigger-engine \
      -o wide 2>&1 | sed 's/^/    /' || true
    dim "  --- recent events ---"
    kubectl -n "$NAMESPACE" get events --sort-by=.lastTimestamp \
      --field-selector involvedObject.name="$DEPLOY_NAME" 2>&1 | tail -10 \
      | sed 's/^/    /' || true
    return 1
  fi

  local ready desired
  ready=$(kubectl -n "$NAMESPACE" get deploy "$DEPLOY_NAME" \
    -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
  desired=$(kubectl -n "$NAMESPACE" get deploy "$DEPLOY_NAME" \
    -o jsonpath='{.spec.replicas}' 2>/dev/null || echo "0")
  ready="${ready:-0}"
  desired="${desired:-0}"

  if [ "$ready" -ge "$desired" ] && [ "$desired" -gt 0 ]; then
    pass "trigger-engine ready replicas ($ready/$desired)"
  else
    fail "trigger-engine ready replicas" "got $ready/$desired"
    return 1
  fi
  echo

  # ── Setup for live-engine tests ─────────────────────────────────
  setup_portforward
  setup_token

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    fail "kd reachable" "Cannot reach kd server at $BEADS_HTTP_URL"
    return 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  local TEST_TAG="e2e-trigeng-smoke-$$-$(date +%s)"

  # ── Scenario 2: Engine evaluates a sentinel schedule trigger ────
  blue "Scenario 2: trigger-engine fires a sentinel schedule trigger"

  # Seed last_checked to 1h ago so the engine bypasses runtime bootstrap
  # and fires on the very first poll cycle. Without this, the first poll
  # is suppressed by ensurePastLastChecked / bootstrapTrigger and we'd
  # have to wait for a second poll cycle.
  local seeded_lc
  seeded_lc=$(date -u -d '1 hour ago' +%Y-%m-%dT%H:%M:%SZ 2>/dev/null \
    || date -u -v-1H +%Y-%m-%dT%H:%M:%SZ 2>/dev/null \
    || echo "")
  if [ -z "$seeded_lc" ]; then
    fail "seed last_checked" "could not compute 1h-ago timestamp"
    return 1
  fi
  dim "  Seeding last_checked=$seeded_lc"

  # Record a creation timestamp BEFORE the engine could possibly poll us;
  # we'll later assert last_checked is strictly after this.
  local pre_create_ts
  pre_create_ts=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  dim "  pre_create_ts=$pre_create_ts"

  # Use action=create_bead — produces an observable bead with a
  # deterministic label (trigger:<TRIG_ID>). action=notify only emits
  # to Slack, which is harder to verify in CI without a mock.
  local trig_id
  trig_id=$(kd_create_id "$KD_BIN" create "e2e-trigger-engine-smoke-$TEST_TAG" \
    --type=trigger \
    -d "Sentinel schedule trigger — verifies trigger-engine evaluation loop." \
    -l "e2e,$TEST_TAG,trigger:schedule,project:gardenio,smoke:trigger-engine" \
    -f 'source=schedule' \
    -f 'action=create_bead' \
    -f 'enabled=true' \
    -f 'cron=* * * * *' \
    -f 'project=gardenio' \
    -f 'max_concurrent=1' \
    -f "last_checked=$seeded_lc")

  if [ -z "$trig_id" ]; then
    fail "sentinel trigger creation" "kd create returned no id"
    return 1
  fi
  CREATED_BEADS+=("$trig_id")
  pass "sentinel trigger created ($trig_id)"
  dim "  cron='* * * * *', action=create_bead, last_checked=$seeded_lc"
  echo

  # ── Scenario 3: last_checked advances past creation timestamp ───
  blue "Scenario 3: trigger.last_checked advances within poll interval"
  local deadline=$((SECONDS + POLL_TIMEOUT_SEC))
  local lc_advanced=0 last_lc=""
  while [ $SECONDS -lt $deadline ]; do
    local trig_json
    trig_json=$("$KD_BIN" show "$trig_id" --json 2>/dev/null || echo "{}")
    last_lc=$(json_field "$trig_json" "fields.last_checked")
    # Engine writes last_checked as RFC3339 (second-precision). We can't use
    # strict `>` against pre_create_ts because the engine often polls within
    # the same wall-clock second as creation. Instead, assert the value moved
    # off the seeded 1h-ago sentinel — any update by the engine proves it
    # polled (engine writes time.Now()).
    if [ -n "$last_lc" ] && [ "$last_lc" != "$seeded_lc" ]; then
      lc_advanced=1
      break
    fi
    sleep "$POLL_INTERVAL_SEC"
  done

  if [ $lc_advanced -eq 1 ]; then
    pass "last_checked advanced ($last_lc != seeded $seeded_lc)"
  else
    fail "last_checked advanced" \
      "last_checked='$last_lc', seeded='$seeded_lc' — engine appears NOT to be polling"
    dim "  --- trigger-engine pod logs (last 50 lines) ---"
    kubectl -n "$NAMESPACE" logs -l app.kubernetes.io/component=trigger-engine \
      --tail=50 2>&1 | sed 's/^/    /' || true
  fi
  echo

  # ── Scenario 4: create_bead action produced a child bead ────────
  blue "Scenario 4: create_bead action produced an observable child bead"
  deadline=$((SECONDS + POLL_TIMEOUT_SEC))
  local child_count=0 children_json="[]"
  while [ $SECONDS -lt $deadline ]; do
    children_json=$("$KD_BIN" list -l "trigger:$trig_id" --json 2>/dev/null || echo "[]")
    child_count=$(echo "$children_json" \
      | python3 -c "import sys,json; print(len(json.load(sys.stdin)))" 2>/dev/null || echo "0")
    if [ "${child_count:-0}" -ge 1 ]; then
      break
    fi
    sleep "$POLL_INTERVAL_SEC"
  done

  if [ "${child_count:-0}" -ge 1 ]; then
    pass "create_bead action fired ($child_count child bead(s) with label trigger:$trig_id)"
    # Track child beads for cleanup.
    while IFS= read -r child_id; do
      [ -n "$child_id" ] && CREATED_BEADS+=("$child_id")
    done < <(echo "$children_json" \
      | python3 -c "import sys,json; [print(b.get('id','')) for b in json.load(sys.stdin)]" 2>/dev/null)
  else
    fail "create_bead action fired" \
      "no child beads found with label trigger:$trig_id within ${POLL_TIMEOUT_SEC}s"
    dim "  --- trigger-engine pod logs (last 50 lines) ---"
    kubectl -n "$NAMESPACE" logs -l app.kubernetes.io/component=trigger-engine \
      --tail=50 2>&1 | sed 's/^/    /' || true
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
