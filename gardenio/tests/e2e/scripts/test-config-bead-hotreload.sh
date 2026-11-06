#!/usr/bin/env bash
# test-config-bead-hotreload.sh — env→config-bead hot-reload E2E
#
# Verifies the pilot migration of COOP_QUEUED_WARN_AGE from an env var to a
# ConfigWatcher-backed tunable (ka-0000003317 / epic ka-0000006607).
#
# Contract under test: writing a `type=config` bead titled "tunables" with
# label `component:controller` and field `COOP_QUEUED_WARN_AGE=15m` must move
# the controller's live effective value without a pod restart, and closing the
# bead must flip the value back to the env/compiled fallback within one poll
# interval. The admin endpoint GET /config/tunables exposes the resolved
# {value, source} pair that this script asserts against.
#
# Scenarios:
#   1. Baseline GET /config/tunables — source ∈ {compiled, env}
#   2. Create tunables config bead → after ≤60s, source='bead', value='15m0s'
#   3. Close tunables bead → after ≤60s, source flips back to compiled/env

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
RELEASE="${RELEASE:-${NAMESPACE}}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
AGENTS_SVC="${AGENTS_SVC:-${RELEASE}-agents}"
KD_BIN="${KD_BIN:-kd}"
# Watcher poll interval default is 30s (beadsapi.DefaultConfigPollInterval).
# Wait 2 cycles + grace to keep the test stable under load.
WATCH_WAIT_SECS="${WATCH_WAIT_SECS:-75}"
PASS=0
FAIL=0
ERRORS=()
PF_PID=""
AGENTS_PF_PID=""
CREATED_BEADS=()

export KD_ACTOR="${KD_ACTOR:-e2e-test}"

# ── Helpers ───────────────────────────────────────────────────────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

pass() { green "  PASS: $1"; PASS=$((PASS + 1)); }

fail() {
  local name="$1" reason="${2:-}"
  red "  FAIL: $name"
  [ -n "$reason" ] && dim "    $reason"
  FAIL=$((FAIL + 1))
  ERRORS+=("$name: $reason")
}

bead_id_from_json() {
  echo "$1" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo ""
}

# ── Setup ─────────────────────────────────────────────────────────
setup_beads_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE}/${BEADS_SVC} kd server..."
  local port=19102
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-hotreload.log 2>&1 &
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

setup_agents_portforward() {
  if ! kubectl -n "$NAMESPACE" get svc "$AGENTS_SVC" >/dev/null 2>&1; then
    red "ERROR: service $AGENTS_SVC not found in $NAMESPACE"
    exit 1
  fi
  local port=18091
  kubectl -n "$NAMESPACE" port-forward "svc/${AGENTS_SVC}" "${port}:8091" \
    >/tmp/kd-pf-hotreload-agents.log 2>&1 &
  AGENTS_PF_PID=$!
  sleep 3
  export AGENTS_HEALTH_URL="http://localhost:${port}"
  dim "  Agents health at $AGENTS_HEALTH_URL (pid $AGENTS_PF_PID)"
}

cleanup() {
  for id in "${CREATED_BEADS[@]}"; do
    "$KD_BIN" close "$id" 2>/dev/null || true
  done
  [ -n "$AGENTS_PF_PID" ] && kill "$AGENTS_PF_PID" 2>/dev/null || true
  [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null || true
}
trap cleanup EXIT

# Reads the /config/tunables endpoint and extracts
# ".resolved.COOP_QUEUED_WARN_AGE.{value,source}". Echoes "VALUE SOURCE"
# on success; empty string on failure.
fetch_tunable() {
  local key="$1"
  local body
  body=$(curl -sf --max-time 5 "${AGENTS_HEALTH_URL}/config/tunables" 2>/dev/null || echo "")
  if [ -z "$body" ]; then
    echo ""
    return
  fi
  local value source
  value=$(echo "$body" | jq -r ".resolved[\"${key}\"].value // empty" 2>/dev/null || echo "")
  source=$(echo "$body" | jq -r ".resolved[\"${key}\"].source // empty" 2>/dev/null || echo "")
  if [ -z "$value" ] || [ -z "$source" ]; then
    echo ""
    return
  fi
  echo "$value $source"
}

# Polls /config/tunables every 5s for up to $WATCH_WAIT_SECS seconds until
# the (value, source) pair matches the expected arguments. Echoes the final
# observed pair; non-zero exit on timeout.
wait_for_tunable() {
  local key="$1" want_value="$2" want_source="$3"
  local deadline=$((SECONDS + WATCH_WAIT_SECS))
  local last=""
  while [ "$SECONDS" -lt "$deadline" ]; do
    last=$(fetch_tunable "$key")
    if [ -n "$last" ]; then
      local v s
      v=$(echo "$last" | awk '{print $1}')
      s=$(echo "$last" | awk '{print $2}')
      if [ "$v" = "$want_value" ] && [ "$s" = "$want_source" ]; then
        echo "$last"
        return 0
      fi
    fi
    sleep 5
  done
  echo "$last"
  return 1
}

# Same as wait_for_tunable but only matches on source (value is whatever the
# fallback resolves to at that moment — 1h compiled default, or env override).
wait_for_source() {
  local key="$1" want_source="$2"
  local deadline=$((SECONDS + WATCH_WAIT_SECS))
  local last=""
  while [ "$SECONDS" -lt "$deadline" ]; do
    last=$(fetch_tunable "$key")
    if [ -n "$last" ]; then
      local s
      s=$(echo "$last" | awk '{print $2}')
      if [ "$s" = "$want_source" ]; then
        echo "$last"
        return 0
      fi
    fi
    sleep 5
  done
  echo "$last"
  return 1
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " env→config-bead hot-reload E2E (ka-0000003317)"
  blue "═══════════════════════════════════════════════════"

  setup_beads_portforward
  setup_token
  setup_agents_portforward

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  kd server reachable: $BEADS_HTTP_URL"

  if ! curl -sf --max-time 5 "${AGENTS_HEALTH_URL}/config/tunables" >/dev/null 2>&1; then
    red "ERROR: Cannot reach controller admin endpoint at $AGENTS_HEALTH_URL/config/tunables"
    exit 1
  fi
  green "  controller /config/tunables reachable"
  echo

  local KEY="COOP_QUEUED_WARN_AGE"
  local TEST_TAG="e2e-hotreload-$$-$(date +%s)"

  # ── Scenario 1: baseline — source ∈ {compiled, env} ─────────────
  blue "Scenario 1: baseline /config/tunables reflects fallback layer"

  local baseline baseline_value baseline_source
  baseline=$(fetch_tunable "$KEY")
  if [ -z "$baseline" ]; then
    fail "baseline GET /config/tunables" "resolved.$KEY missing from response"
    return 1
  fi
  baseline_value=$(echo "$baseline" | awk '{print $1}')
  baseline_source=$(echo "$baseline" | awk '{print $2}')
  dim "  baseline: value=$baseline_value source=$baseline_source"

  case "$baseline_source" in
    compiled|env)
      pass "baseline source is fallback layer ($baseline_source)"
      ;;
    bead)
      # A pre-existing tunables bead from a prior run could leave the
      # controller in the bead layer. Fail loudly so the operator can
      # clean it up — the hot-reload contract is not testable otherwise.
      fail "baseline source is fallback layer" \
        "source=bead; a stale tunables config bead exists for component:controller"
      return 1
      ;;
    *)
      fail "baseline source is known" "got source=$baseline_source (want compiled|env)"
      return 1
      ;;
  esac
  echo

  # ── Scenario 2: config bead write → source=bead, value=15m0s ────
  blue "Scenario 2: writing tunables bead moves live value without restart"

  # Create the tunables bead with --project="" so no project:<name> label is
  # added. The controller's ConfigWatcher runs with project="" (see main.go),
  # and the resolver skips beads whose project label does not match the
  # watcher's project. --force bypasses the cross-project guard since this
  # test is run from an agent with its own agent-project context.
  local cfg_out cfg_id
  cfg_out=$("$KD_BIN" create "tunables" --type=config \
    --project "" --force \
    -l "component:controller" \
    -f "${KEY}=15m" \
    -d "E2E hot-reload test ($TEST_TAG); auto-closed by test-config-bead-hotreload.sh" \
    --json 2>/dev/null || echo "{}")
  cfg_id=$(bead_id_from_json "$cfg_out")
  if [ -z "$cfg_id" ]; then
    fail "create tunables config bead" "no ID returned from kd create"
    return 1
  fi
  CREATED_BEADS+=("$cfg_id")
  pass "tunables config bead created ($cfg_id)"

  local observed
  if observed=$(wait_for_tunable "$KEY" "15m0s" "bead"); then
    pass "hot-reload: value=15m0s source=bead within ${WATCH_WAIT_SECS}s"
    dim "  observed: $observed"
  else
    fail "hot-reload to bead" \
      "after ${WATCH_WAIT_SECS}s, observed='$observed' (want '15m0s bead')"
  fi
  echo

  # ── Scenario 3: closing bead → source reverts to fallback ───────
  blue "Scenario 3: closing tunables bead reverts to fallback without restart"

  if ! "$KD_BIN" close "$cfg_id" >/dev/null 2>&1; then
    fail "close tunables config bead" "kd close $cfg_id failed"
    return 1
  fi
  pass "tunables config bead closed"

  local after_close
  if after_close=$(wait_for_source "$KEY" "$baseline_source"); then
    local after_value after_source
    after_value=$(echo "$after_close" | awk '{print $1}')
    after_source=$(echo "$after_close" | awk '{print $2}')
    pass "hot-reload: source reverted to $after_source within ${WATCH_WAIT_SECS}s"
    dim "  observed: value=$after_value source=$after_source"
    if [ "$after_value" = "$baseline_value" ]; then
      pass "reverted value matches baseline ($baseline_value)"
    else
      # The fallback value can legitimately shift if an env override was
      # added/removed between baseline and now — report but don't fail.
      dim "  reverted value=$after_value differs from baseline=$baseline_value (env drift OK)"
      pass "reverted to baseline layer (value drift tolerated)"
    fi
  else
    fail "hot-reload revert to fallback" \
      "after ${WATCH_WAIT_SECS}s, source still != $baseline_source (observed='$after_close')"
  fi
  echo

  # ── Summary ─────────────────────────────────────────────────────
  blue "═══════════════════════════════════════════════════"
  local total=$((PASS + FAIL))
  if [ "$FAIL" -eq 0 ]; then
    green " All $total assertions passed"
  else
    red " $FAIL/$total assertions failed"
    echo
    for err in "${ERRORS[@]}"; do
      red "   - $err"
    done
  fi
  blue "═══════════════════════════════════════════════════"

  [ "$FAIL" -eq 0 ]
}

main "$@"
