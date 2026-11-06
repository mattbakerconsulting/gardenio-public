#!/usr/bin/env bash
# test-local-mode.sh — run `gb agent start --local` E2E scenarios
#
# Sibling of test-claudeless.sh. Exercises the laptop/runner lifecycle
# for the local agent path using the claudeless TOMLs in
# gardenio/tests/e2e/claudeless/local-*.toml.
#
# For each scenario, this runner:
#   1. Optionally seeds a pre-assigned task (passed via --task=<id>, which
#      sets BOAT_TASK_ID inside the agent process) or side channel
#   2. Spawns `gb agent start --local --test-scenario=<name>` (bounded by a
#      per-scenario timeout; some scenarios run in the background so the
#      runner can signal them or respond to their decisions)
#   3. Asserts the runner exited cleanly AND the spawned agent bead moved
#      through register → close
#   4. Tears down anything it seeded
#
# Invocation is identical on a laptop (with kubectl port-forward already up)
# and in CI (which exports BEADS_HTTP_URL directly). If BEADS_HTTP_URL
# is not set, the runner will try to port-forward the namespace's beads svc.
#
# Usage:
#   BEADS_HTTP_URL=http://localhost:19099 ./test-local-mode.sh
#     or
#   NAMESPACE=gardenio-e2e ./test-local-mode.sh  # auto port-forwards
#
# Prerequisites:
#   - gb, kd, coop, claudeless binaries in PATH
#   - A reachable beads daemon (BEADS_HTTP_URL or kubectl context)

set -euo pipefail

# Identify this test runner as the operator for any beads it creates.
# kbeads server enforces non-empty operated_by since ka-0000005772 (commit
# 310a72b48); without this export, `gb agent start --local` 400s with
# "operator could not be resolved". See gd-KpZoaOQjng.
#
# Since ka-0000002601 the daemon also enforces a human-operator shape:
# "slack:U<alnum>" or "oidc:<local>@<domain>". Anything else (bare names,
# "ci:*", "system:*") is rejected at the header boundary. Use an oidc-shape
# fixture so laptop runs without an explicit override don't 400. In CI this
# is pre-exported by run-shard.sh with a per-shard oidc fixture, so the
# default below is the laptop-fallback path. See ka-0000006816, gd-EBOAN3UpWC.
export BEADS_OPERATED_BY="${BEADS_OPERATED_BY:-oidc:e2e-local-mode@example.com}"

# ── Config ────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCENARIO_DIR="$SCRIPT_DIR/../claudeless"
# Monorepo root: .../gardenio/tests/e2e/scripts → repo root
WORK_DIR="$(cd "$SCRIPT_DIR/../../../.." && pwd)"
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
KD_BIN="${KD_BIN:-kd}"
GB_BIN="${GB_BIN:-gb}"
PF_PID=""
PF_PORT=""
# Sentinel file controlling the pf-supervisor lifetime. Removed by cleanup()
# to tell the supervisor to exit. See gd-FtiTtZyuj2.
PF_SENTINEL=""
TEST_TAG="local-mode-$$-$(date +%s)"
CREATED_BEADS=()
PASS=0
FAIL=0
SKIP=0
ERRORS=()

# Optional sink for per-scenario logs so CI (or `kubectl cp`) can collect
# them post-run. CI sets it to junit-results-${E2E_SHARD}/local-mode-logs.
LOG_ARCHIVE_DIR="${LOG_ARCHIVE_DIR:-}"
if [ -n "$LOG_ARCHIVE_DIR" ]; then
  mkdir -p "$LOG_ARCHIVE_DIR" || LOG_ARCHIVE_DIR=""
fi

# PTY byte-stream tracing for ka-0000002034. When LOG_ARCHIVE_DIR is set we
# pass COOP_PTY_TRACE_FILE=<archive>/<scenario>.pty.trace to each scenario so
# coop's NativePty backend appends a hex trace of every PTY master read/write.
# Pair with gardenio/tests/e2e/scripts/decode-pty-trace.py + the isolated
# repro (test-claudeless-multi-nudge-isolated.sh --trace-file) to produce a
# byte-for-byte diff between a failing E2E shard and the passing repro.
pty_trace_env_for() {
  local name="$1"
  if [ -n "$LOG_ARCHIVE_DIR" ] && [ -d "$LOG_ARCHIVE_DIR" ]; then
    printf 'COOP_PTY_TRACE_FILE=%s/%s.pty.trace' "$LOG_ARCHIVE_DIR" "$name"
  fi
}
# Cap on lines printed inline on failure so a runaway log can't drown the
# shard stdout. The full log is always preserved at $log on disk and (when
# set) copied into LOG_ARCHIVE_DIR for offline inspection.
FAIL_LOG_MAX_LINES="${FAIL_LOG_MAX_LINES:-500}"

# Per-scenario overall timeout (seconds). Each scenario is designed to
# complete well under this; the cap just prevents CI wedges.
SCENARIO_TIMEOUT="${SCENARIO_TIMEOUT:-90}"
# Grace period after SIGTERM before the runner gives up on clean shutdown.
SIGTERM_GRACE="${SIGTERM_GRACE:-15}"

# ── Helpers ───────────────────────────────────────────────────────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

pass() { green "  ✓ $1"; PASS=$((PASS + 1)); }
fail() {
  local name="$1" reason="${2:-}"
  red "  ✗ $name"
  [ -n "$reason" ] && dim "    $reason"
  FAIL=$((FAIL + 1))
  ERRORS+=("$name: $reason")
}
skip() {
  local name="$1" reason="${2:-}"
  dim "  ⊘ $name (skipped: $reason)"
  SKIP=$((SKIP + 1))
}

have() { command -v "$1" >/dev/null 2>&1; }

# bead_status <id> — prints current status or empty string.
# Retries once through ensure_portforward when the first call comes back
# empty. Without this, a transient port-forward death (the gd-FtiTtZyuj2
# cascade) surfaces as status=<unreachable> in every downstream assertion
# even when the supervisor has already respawned kubectl underneath.
bead_status() {
  local id="$1" s
  s="$("$KD_BIN" show "$id" --json 2>/dev/null \
    | python3 -c "import sys,json; print(json.load(sys.stdin).get('status',''))" 2>/dev/null \
    || true)"
  if [ -z "$s" ] && [ -n "$PF_PORT" ]; then
    ensure_portforward >&2 || true
    s="$("$KD_BIN" show "$id" --json 2>/dev/null \
      | python3 -c "import sys,json; print(json.load(sys.stdin).get('status',''))" 2>/dev/null \
      || true)"
  fi
  echo "$s"
}

# Poll bead_status until it matches $2 (regex) or timeout.
wait_for_status() {
  local id="$1" want_re="$2" timeout="${3:-30}"
  local start=$SECONDS s
  while [ $((SECONDS - start)) -lt "$timeout" ]; do
    s="$(bead_status "$id")"
    if [[ "$s" =~ $want_re ]]; then
      echo "$s"
      return 0
    fi
    sleep 1
  done
  echo "$s"
  return 1
}

# Grep for the first gd-XXXX agent bead id a `gb agent start --local`
# banner emits so we can assert its lifecycle independently.
extract_agent_bead_id() {
  local log="$1"
  grep -oE 'Agent bead:[[:space:]]+(gd|ka)-[A-Za-z0-9]+' "$log" 2>/dev/null \
    | head -n1 \
    | awk '{print $NF}'
}

# ── Setup ─────────────────────────────────────────────────────────
# port_forward_alive — TCP probe for the port-forward listener.
# Returns 0 when something is listening on PF_PORT, non-zero otherwise.
# We deliberately do NOT check PF_PID: under pf-supervisor the kubectl
# pid changes on every respawn, so a stale PF_PID would cause spurious
# "not alive" reports between the supervisor noticing kubectl died and
# our next lsof refresh. The TCP probe is the source of truth.
port_forward_alive() {
  [ -n "$PF_PORT" ] || return 1
  if have nc; then
    nc -z -w1 127.0.0.1 "$PF_PORT" >/dev/null 2>&1
    return $?
  fi
  # Fallback: bash /dev/tcp probe. Quiet timeout via subshell + exec.
  ( exec 3<>/dev/tcp/127.0.0.1/"$PF_PORT" ) >/dev/null 2>&1
}

# spawn_portforward — launch pf-supervisor.sh in a detached session. The
# supervisor keeps kubectl port-forward alive in a respawn loop so a single
# kubectl death (backing pod restart, SPDY stream error, transient API blip)
# doesn't take the tunnel down for the rest of the suite (gd-FtiTtZyuj2).
# The supervisor exits cleanly when its sentinel file is removed; cleanup()
# does that on shard exit.
spawn_portforward() {
  local port="${PF_PORT:-19099}"
  PF_PORT="$port"
  local logf="/tmp/kd-pf-local-mode-${TEST_TAG}.log"
  PF_SENTINEL="/tmp/kd-pf-local-mode-${TEST_TAG}.sentinel"
  local sup="$SCRIPT_DIR/pf-supervisor.sh"

  if [ ! -x "$sup" ]; then
    red "  pf-supervisor.sh not found or not executable at $sup"
    return 1
  fi

  # If a previous supervisor for this tag is still up, tell it to exit and
  # wait briefly for the port to free up. Otherwise we'll double-bind.
  if [ -f "$PF_SENTINEL" ]; then
    rm -f "$PF_SENTINEL"
    sleep 2
  fi
  if have lsof; then
    local stale_pid
    while read -r stale_pid; do
      [ -n "$stale_pid" ] && kill "$stale_pid" 2>/dev/null || true
    done < <(lsof -t -iTCP:"$port" -sTCP:LISTEN 2>/dev/null || true)
  fi

  if have setsid; then
    setsid -f bash "$sup" "$NAMESPACE" "$BEADS_SVC" "$port" "$logf" "$PF_SENTINEL"
  else
    bash "$sup" "$NAMESPACE" "$BEADS_SVC" "$port" "$logf" "$PF_SENTINEL" &
    disown 2>/dev/null || true
  fi

  # Wait up to 15s for the supervisor's first kubectl to start listening.
  local start=$SECONDS
  while [ $((SECONDS - start)) -lt 15 ]; do
    if port_forward_alive; then
      if have lsof; then
        PF_PID="$(lsof -t -iTCP:"$port" -sTCP:LISTEN 2>/dev/null | head -n1 || true)"
      fi
      return 0
    fi
    sleep 1
  done
  return 1
}

setup_portforward() {
  # When kubectl can reach the namespace, ALWAYS spawn our own
  # setsid-detached forwarder on PF_PORT and re-export BEADS_HTTP_URL.
  # Why: in CI (run-shard.sh) BEADS_HTTP_URL=http://localhost:19090 is
  # pre-exported, pointing at the suite-level forwarder. That forwarder
  # lives in run-shard.sh's process group and dies when (a) a coop
  # SIGTERM cascades from a scenario (gd-4f6rVx0u2y) or (b) the backing
  # beads pod restarts and tears its network namespace down mid-suite
  # (gd-CKf3jSzXWz). Either way, every scenario after the first then
  # fails at bead registration with "connection refused" — until the
  # next run_module call lets run-shard.sh respawn its forwarder.
  # Our setsid-detached forwarder on :19099 survives both, and
  # ensure_portforward() respawns it between scenarios. The suite-level
  # forwarder on :19090 is left alone; run-shard.sh manages its own
  # lifetime for the modules before/after local-mode.
  if have kubectl && kubectl -n "$NAMESPACE" get "svc/${BEADS_SVC}" >/dev/null 2>&1; then
    PF_PORT=19099
    blue "Port-forwarding ${NAMESPACE}/${BEADS_SVC} on :${PF_PORT} (setsid-detached)..."
    if spawn_portforward; then
      export BEADS_HTTP_URL="http://localhost:${PF_PORT}"
      dim "  Daemon at $BEADS_HTTP_URL (pf pid ${PF_PID:-detached})"
      return
    fi
    red "  kubectl port-forward did not come up on :${PF_PORT}"
    if [ -n "${BEADS_HTTP_URL:-}" ]; then
      blue "  Falling back to caller-provided BEADS_HTTP_URL=$BEADS_HTTP_URL"
      PF_PORT=""
      return
    fi
    exit 1
  fi
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL (kubectl unavailable or svc unreachable)"
    return
  fi
  red "ERROR: BEADS_HTTP_URL not set and kubectl not in PATH"
  exit 1
}

# ensure_portforward — call before each scenario (and from bead_status on
# empty response). When the operator passes BEADS_HTTP_URL directly (laptop
# run) we skip; otherwise we give the supervisor up to 8s to bring the
# listener back. The supervisor normally respawns kubectl in ~1s, so this
# is just a courtesy poll; if it's still down after 8s we forcibly relaunch
# the whole supervisor (sentinel may have been clobbered).
ensure_portforward() {
  if [ -z "$PF_PORT" ]; then
    return 0
  fi
  if port_forward_alive; then
    return 0
  fi
  local start=$SECONDS
  while [ $((SECONDS - start)) -lt 8 ]; do
    if port_forward_alive; then
      return 0
    fi
    sleep 1
  done
  dim "  port-forward not reachable on :${PF_PORT} after 8s — relaunching supervisor"
  if ! spawn_portforward; then
    red "  ERROR: port-forward respawn failed — subsequent scenarios will fail"
    return 1
  fi
  dim "  port-forward supervisor relaunched (pf pid ${PF_PID:-detached})"
}

setup_token() {
  if [ -n "${BEADS_TOKEN:-}" ]; then return; fi
  if ! have kubectl; then return; fi
  local secret
  secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
    -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
  if [ -n "$secret" ]; then
    export BEADS_TOKEN="$secret"
    dim "  Token loaded from secret kd-beads-token"
  fi
}

cleanup() {
  local id
  for id in "${CREATED_BEADS[@]}"; do
    "$KD_BIN" close "$id" >/dev/null 2>&1 || true
  done
  # Tell the pf-supervisor to exit cleanly. It checks the sentinel once a
  # second and kills its inner kubectl on removal.
  if [ -n "$PF_SENTINEL" ] && [ -f "$PF_SENTINEL" ]; then
    rm -f "$PF_SENTINEL"
    # Give the supervisor a moment to notice + kill kubectl.
    sleep 2
  fi
  # Belt-and-braces: kill anything still listening on PF_PORT (the inner
  # kubectl that may have re-spawned during the sleep above, or a stale
  # legacy forwarder from a pre-supervisor run).
  if [ -n "$PF_PORT" ] && have lsof; then
    local pid
    while read -r pid; do
      if [ -n "$pid" ]; then
        kill "$pid" 2>/dev/null || true
      fi
    done < <(lsof -t -iTCP:"$PF_PORT" -sTCP:LISTEN 2>/dev/null || true)
  fi
}
trap cleanup EXIT

# ── Scenario drivers ─────────────────────────────────────────────
#
# Each driver is responsible for:
#   • Pre-seeding state the scenario consumes (pre-assignment task, side
#     channels, signal timing, etc.)
#   • Invoking `gb agent start --local --test-scenario=<name>`
#   • Asserting inner scenario exit AND the agent bead lifecycle
#
# Drivers return 0 on pass, non-zero on fail. They must not exit.

# Log-file-safe slug so tmp logs don't collide.
log_for() { echo "/tmp/test-local-mode-${TEST_TAG}-$1.log"; }

# Dump up to FAIL_LOG_MAX_LINES of the per-scenario log inline, and copy the
# full log into LOG_ARCHIVE_DIR (when set) so post-mortem doesn't depend on
# the runner's tmpfs surviving.
#
# The previous tail -n 10 approach almost always caught only cobra's USAGE
# block (cobra prints help when RunE returns an error) and hid the actual
# error coming from coop/claudeless/registration above it. Print the entire
# log so the real failure is visible in shard stdout.
dump_scenario_log() {
  local name="$1" log="$2"
  if [ ! -s "$log" ]; then
    dim "    (log $log empty or missing)"
    return
  fi
  local total
  total="$(wc -l <"$log" 2>/dev/null || echo 0)"
  dim "  --- $log ($total lines) ---"
  if [ "$total" -le "$FAIL_LOG_MAX_LINES" ]; then
    sed 's/^/      /' "$log"
  else
    dim "  ...truncated to last ${FAIL_LOG_MAX_LINES} lines (full log archived)..."
    tail -n "$FAIL_LOG_MAX_LINES" "$log" | sed 's/^/      /'
  fi
  dim "  --- end $log ---"
  if [ -n "$LOG_ARCHIVE_DIR" ] && [ -d "$LOG_ARCHIVE_DIR" ]; then
    cp "$log" "$LOG_ARCHIVE_DIR/${name}.log" 2>/dev/null || true
  fi
}

# run_scenario_basic <scenario-name>
# Foreground invocation with timeout + agent-bead lifecycle assertion.
# Extra env vars are inherited from the caller's environment.
run_scenario_basic() {
  local name="$1"
  local log; log="$(log_for "$name")"

  blue "Scenario: $name"
  local rc=0
  env $(pty_trace_env_for "$name") \
    timeout "$SCENARIO_TIMEOUT" "$GB_BIN" agent start --local \
    --name="e2e-local-${TEST_TAG}-${name}" \
    --role=crew \
    --dir="$WORK_DIR" \
    --test-scenario="$name" \
    --test-scenario-dir="$SCENARIO_DIR" \
    </dev/null >"$log" 2>&1 || rc=$?

  assert_scenario_result "$name" "$rc" "$log"
}

# assert_scenario_result <name> <exit-code> <log-path>
assert_scenario_result() {
  local name="$1" rc="$2" log="$3"
  local dumped=0

  if [ "$rc" -eq 0 ]; then
    pass "$name: gb agent start exited 0"
  elif [ "$rc" -eq 124 ]; then
    fail "$name" "timed out after ${SCENARIO_TIMEOUT}s"
    dump_scenario_log "$name" "$log"; dumped=1
  else
    fail "$name" "exit code $rc"
    dump_scenario_log "$name" "$log"; dumped=1
  fi

  # Agent-bead lifecycle: `gb agent start` logs "Agent bead: gd-XXXX"
  # on registration and "Agent bead gd-XXXX closed." on cleanup.
  local agent_id
  agent_id="$(extract_agent_bead_id "$log")"
  if [ -z "$agent_id" ]; then
    fail "$name: agent bead registered" "no 'Agent bead:' banner in log"
    [ "$dumped" -eq 0 ] && { dump_scenario_log "$name" "$log"; dumped=1; }
    return
  fi
  pass "$name: agent bead registered ($agent_id)"

  # Scenario-driven "bead closed on exit" marker in the log.
  if grep -q "Agent bead ${agent_id} closed" "$log"; then
    pass "$name: agent bead closed on exit"
  else
    fail "$name: agent bead closed on exit" "no 'Agent bead <id> closed' marker in log"
    [ "$dumped" -eq 0 ] && { dump_scenario_log "$name" "$log"; dumped=1; }
  fi

  # Independent check: the server reflects closed status.
  local final
  final="$(bead_status "$agent_id")"
  if [ "$final" = "closed" ]; then
    pass "$name: server reflects status=closed"
  else
    fail "$name: server reflects status=closed" "status=${final:-<unreachable>}"
    [ "$dumped" -eq 0 ] && { dump_scenario_log "$name" "$log"; dumped=1; }
    # Best-effort cleanup so we don't leave a dangling agent bead.
    "$KD_BIN" close "$agent_id" >/dev/null 2>&1 || true
  fi

  # On a fully-passing run, still archive the log when configured. Useful
  # when looking back at a flaky-but-eventually-green retry attempt.
  if [ "$dumped" -eq 0 ] && [ -n "$LOG_ARCHIVE_DIR" ] && [ -d "$LOG_ARCHIVE_DIR" ]; then
    cp "$log" "$LOG_ARCHIVE_DIR/${name}.log" 2>/dev/null || true
  fi
}

# run_scenario_lifecycle — pre-creates a task, passes via --task (BOAT_TASK_ID).
run_scenario_lifecycle() {
  local name="local-lifecycle"
  blue "Scenario: $name"

  # Pre-create a task the scenario will claim & close.
  local task_out task_id
  task_out="$("$KD_BIN" create "e2e-local-lifecycle-task-$TEST_TAG" \
    --type=task -l "project:gardenio" --json 2>/dev/null || echo '{}')"
  task_id="$(echo "$task_out" | python3 -c \
    'import sys,json; print(json.load(sys.stdin).get("id",""))' 2>/dev/null || true)"
  if [ -z "$task_id" ]; then
    fail "$name: seed pre-assigned task" "could not create"
    return
  fi
  CREATED_BEADS+=("$task_id")
  dim "  seeded BOAT_TASK_ID=$task_id"

  local log; log="$(log_for "$name")"
  local rc=0
  env $(pty_trace_env_for "$name") \
    timeout "$SCENARIO_TIMEOUT" "$GB_BIN" agent start --local \
      --name="e2e-local-${TEST_TAG}-${name}" \
      --role=crew \
      --dir="$WORK_DIR" \
      --task="$task_id" \
      --test-scenario="$name" \
      --test-scenario-dir="$SCENARIO_DIR" \
      </dev/null >"$log" 2>&1 || rc=$?

  assert_scenario_result "$name" "$rc" "$log"

  # Scenario-specific: the pre-assigned task should end up closed.
  local final
  final="$(bead_status "$task_id")"
  if [ "$final" = "closed" ]; then
    pass "$name: pre-assigned task closed by agent"
  else
    fail "$name: pre-assigned task closed by agent" "task status=${final:-<unreachable>}"
  fi
}

# run_scenario_yield — resolve the agent's decision out of band.
run_scenario_yield() {
  local name="local-yield-close"
  blue "Scenario: $name"

  # Pre-create a task so the scenario can close something on exit.
  local task_out task_id
  task_out="$("$KD_BIN" create "e2e-local-yield-task-$TEST_TAG" \
    --type=task -l "project:gardenio" --json 2>/dev/null || echo '{}')"
  task_id="$(echo "$task_out" | python3 -c \
    'import sys,json; print(json.load(sys.stdin).get("id",""))' 2>/dev/null || true)"
  if [ -n "$task_id" ]; then
    CREATED_BEADS+=("$task_id")
  fi

  # Clear any stale side-channel file from a previous run.
  rm -f /tmp/local-yield-decision-id

  local log; log="$(log_for "$name")"
  env $(pty_trace_env_for "$name") \
    "$GB_BIN" agent start --local \
      --name="e2e-local-${TEST_TAG}-${name}" \
      --role=crew \
      --dir="$WORK_DIR" \
      ${task_id:+--task="$task_id"} \
      --test-scenario="$name" \
      --test-scenario-dir="$SCENARIO_DIR" \
      </dev/null >"$log" 2>&1 &
  local runner_pid=$!

  # Wait for the scenario to emit the decision id (side-channel file).
  local start=$SECONDS dec_id=""
  while [ $((SECONDS - start)) -lt "$SCENARIO_TIMEOUT" ]; do
    if [ -s /tmp/local-yield-decision-id ]; then
      dec_id="$(cat /tmp/local-yield-decision-id 2>/dev/null || true)"
      [ -n "$dec_id" ] && break
    fi
    if ! kill -0 "$runner_pid" 2>/dev/null; then
      break  # runner exited already
    fi
    sleep 1
  done

  if [ -n "$dec_id" ]; then
    pass "$name: decision created ($dec_id)"
    # Resolve as if a human clicked the first option.
    if "$GB_BIN" decision respond "$dec_id" --select approve >/dev/null 2>&1; then
      pass "$name: decision resolved out-of-band"
    else
      fail "$name: decision resolved out-of-band" "gb decision respond --select approve failed"
      # Keep going — maybe the scenario exits on yield timeout anyway.
    fi
  else
    fail "$name: decision created" "side-channel /tmp/local-yield-decision-id never populated"
  fi

  # Wait for runner to exit.
  local rc=0
  local wait_start=$SECONDS
  while kill -0 "$runner_pid" 2>/dev/null; do
    if [ $((SECONDS - wait_start)) -ge "$SCENARIO_TIMEOUT" ]; then
      dim "  runner exceeded timeout — sending TERM"
      kill -TERM "$runner_pid" 2>/dev/null || true
      sleep 3
      kill -KILL "$runner_pid" 2>/dev/null || true
      rc=124
      break
    fi
    sleep 1
  done
  if [ "$rc" -ne 124 ]; then
    wait "$runner_pid" 2>/dev/null
    rc=$?
  fi

  rm -f /tmp/local-yield-decision-id
  assert_scenario_result "$name" "$rc" "$log"
}

# seed_escalation_runner — set ESC_RUNNER_ID to the runner bead the
# escalation round-trip runs on (a variable, not a $(...) capture, so the
# CREATED_BEADS append survives). execution_target=local must name a runner
# since #1783, and since gd-yNRhwrTKEN only an agent started with --runner
# executes one. E2E_RUNNER_ID names a registered runner (its owner,
# E2E_RUNNER_OWNER, must be the identity that approves); otherwise a fake one
# is created, owned by BEADS_OPERATED_BY. type:runner is admin-only through
# the bead API, so the fake needs an admin token; without one ESC_RUNNER_ID
# stays empty and the caller skips. Cleanup closes a created runner.
seed_escalation_runner() {
  local owner="$1"
  ESC_RUNNER_ID="${E2E_RUNNER_ID:-}"
  if [ -n "$ESC_RUNNER_ID" ]; then
    return
  fi
  local out id
  out="$("$KD_BIN" create "e2e-local-runner-$TEST_TAG" \
    --type=runner \
    -f "owner=$owner" \
    -f "hostname=e2e-local-mode-$$" \
    -f 'projects=["gardenio"]' \
    -f "state=online" \
    -f "last_seen_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --json 2>&1 || true)"
  id="$(echo "$out" | python3 -c \
    'import sys,json; print(json.load(sys.stdin).get("id",""))' 2>/dev/null || true)"
  if [ -z "$id" ]; then
    dim "  could not create a runner bead: $(echo "$out" | head -2 | tr '\n' ' ')"
    return
  fi
  CREATED_BEADS+=("$id")
  ESC_RUNNER_ID="$id"
}

# run_scenario_escalation_roundtrip — pre-create an escalation on a runner,
# start the local agent on that runner (--runner), approve the escalation
# out-of-band, and assert the scenario received the nudge and the
# escalation closed. The daemon mails the agent joined on the runner
# control:escalation-approved, and its escalation executor nudges coop.
run_scenario_escalation_roundtrip() {
  local name="local-escalation-roundtrip"
  blue "Scenario: $name"

  local runner_owner="${E2E_RUNNER_OWNER:-$BEADS_OPERATED_BY}"
  local ESC_RUNNER_ID="" runner_id
  seed_escalation_runner "$runner_owner"
  runner_id="$ESC_RUNNER_ID"
  if [ -z "$runner_id" ]; then
    skip "$name" "no runner bead (set E2E_RUNNER_ID, or run with an admin token so a fake one can be created)"
    return
  fi
  dim "  runner=$runner_id (owner $runner_owner)"

  # Pre-create a task bead — the scenario closes it as part of its exit path.
  local task_out task_id
  task_out="$("$KD_BIN" create "e2e-local-escalation-task-$TEST_TAG" \
    --type=task -l "project:gardenio" --json 2>/dev/null || echo '{}')"
  task_id="$(echo "$task_out" | python3 -c \
    'import sys,json; print(json.load(sys.stdin).get("id",""))' 2>/dev/null || true)"
  if [ -n "$task_id" ]; then
    CREATED_BEADS+=("$task_id")
    dim "  seeded BOAT_TASK_ID=$task_id"
  fi

  # Pre-create the escalation with execution_target=local on the runner.
  # Approval dispatches it to the agent joined on that runner, and only
  # that one.
  local esc_out esc_id
  esc_out="$("$GB_BIN" escalation create \
    --title "e2e-local-escalation-roundtrip-$TEST_TAG" \
    --operation-type manual_test \
    --operation-spec '{"scenario":"local-escalation-roundtrip"}' \
    --context "Local-mode escalation round-trip E2E" \
    --execution-target local \
    --target-runner "$runner_id" \
    --label "project:gardenio" 2>&1 || true)"
  esc_id="$(echo "$esc_out" | grep -oE 'escalation_id=(gd|ka)-[A-Za-z0-9]+' \
    | head -n1 | cut -d= -f2 || true)"
  if [ -z "$esc_id" ]; then
    esc_id="$(echo "$esc_out" | grep -oE '(gd|ka)-[A-Za-z0-9]+' | grep -vx "$runner_id" | head -n1 || true)"
  fi
  if [ -z "$esc_id" ]; then
    fail "$name: seed escalation bead" "could not parse escalation id: $esc_out"
    return
  fi
  CREATED_BEADS+=("$esc_id")
  dim "  seeded escalation=$esc_id"

  # Side channel: scenario reads this to know which escalation to close.
  echo "$esc_id" > /tmp/local-escalation-id
  # Clear any stale handled marker from a previous run.
  rm -f /tmp/local-escalation-handled

  local log; log="$(log_for "$name")"
  env $(pty_trace_env_for "$name") \
    "$GB_BIN" agent start --local \
      --name="e2e-local-${TEST_TAG}-${name}" \
      --role=crew \
      --dir="$WORK_DIR" \
      --project=gardenio \
      --runner="$runner_id" \
      --owner="$runner_owner" \
      ${task_id:+--task="$task_id"} \
      --test-scenario="$name" \
      --test-scenario-dir="$SCENARIO_DIR" \
      </dev/null >"$log" 2>&1 &
  local runner_pid=$!

  # Wait for the agent bead (runtime_id=<runner>) and the executor before
  # approving. Approved with no agent joined on the runner, the mail is
  # parked as runner:<id> and the agent claims it on its next drain, so
  # this is not a race, but it keeps the timeline short and readable.
  local start=$SECONDS executor_ready=0
  while [ $((SECONDS - start)) -lt "$SCENARIO_TIMEOUT" ]; do
    if grep -q "Escalation executor started" "$log" 2>/dev/null; then
      executor_ready=1
      break
    fi
    if ! kill -0 "$runner_pid" 2>/dev/null; then
      break
    fi
    sleep 1
  done

  if [ "$executor_ready" -eq 1 ]; then
    pass "$name: escalation executor started"
  else
    fail "$name: escalation executor started" "no 'Escalation executor started' banner in log"
  fi

  # Approve the escalation as if the runner's owner clicked "approve". The
  # daemon derives the approver from the caller's identity and lets only the
  # runner's owner approve, through an approval-capable OIDC client. A daemon
  # with no such approver refuses: stop the agent and skip the rest, as
  # test-escalation-execution.sh does.
  local approve_out approve_rc
  approve_out="$("$GB_BIN" escalation approve "$esc_id" \
      --execution-target local 2>&1)" && approve_rc=0 || approve_rc=$?
  echo "$approve_out" >>"$log"
  if [ "$approve_rc" -eq 0 ]; then
    pass "$name: escalation approved out-of-band"
  else
    skip "$name: escalation round-trip" \
      "approve refused (rc=$approve_rc): $(echo "$approve_out" | head -2 | tr '\n' ' ')"
    kill -TERM "$runner_pid" 2>/dev/null || true
    local stop_start=$SECONDS
    while kill -0 "$runner_pid" 2>/dev/null; do
      if [ $((SECONDS - stop_start)) -ge "$SIGTERM_GRACE" ]; then
        kill -KILL "$runner_pid" 2>/dev/null || true
        break
      fi
      sleep 1
    done
    wait "$runner_pid" 2>/dev/null || true
    rm -f /tmp/local-escalation-id /tmp/local-escalation-handled
    return
  fi

  # Wait for runner to exit — the scenario calls gb stop after handling.
  local rc=0
  local wait_start=$SECONDS
  while kill -0 "$runner_pid" 2>/dev/null; do
    if [ $((SECONDS - wait_start)) -ge "$SCENARIO_TIMEOUT" ]; then
      dim "  runner exceeded timeout — sending TERM"
      kill -TERM "$runner_pid" 2>/dev/null || true
      sleep 3
      kill -KILL "$runner_pid" 2>/dev/null || true
      rc=124
      break
    fi
    sleep 1
  done
  if [ "$rc" -ne 124 ]; then
    wait "$runner_pid" 2>/dev/null
    rc=$?
  fi

  # Scenario-specific: confirm the nudge reached the scenario (independent
  # of the prompt_delivered_at stamp the executor records).
  if [ -s /tmp/local-escalation-handled ]; then
    local handled; handled="$(cat /tmp/local-escalation-handled 2>/dev/null || true)"
    if [ "$handled" = "$esc_id" ]; then
      pass "$name: scenario received escalation nudge"
    else
      fail "$name: scenario received escalation nudge" \
        "handled id=$handled, expected $esc_id"
    fi
  else
    fail "$name: scenario received escalation nudge" \
      "/tmp/local-escalation-handled never written"
  fi

  # Escalation bead should be closed with a captured result. The executor
  # never closes it on delivery; the scenario's `gb escalation close` does.
  # On observed CI runs the close has lagged ~5s behind runner exit, so
  # poll for up to 30s instead of a single point-in-time check.
  local esc_status
  esc_status="$(wait_for_status "$esc_id" '^closed$' 30 || true)"
  if [ "$esc_status" = "closed" ]; then
    pass "$name: escalation bead closed"
  else
    fail "$name: escalation bead closed" "status=${esc_status:-<unreachable>}"
  fi

  # Cleanup side-channel files.
  rm -f /tmp/local-escalation-id /tmp/local-escalation-handled

  assert_scenario_result "$name" "$rc" "$log"
}

# run_scenario_sigterm — background spawn, signal once ready.
run_scenario_sigterm() {
  local name="local-sigterm"
  blue "Scenario: $name"

  # Side-channel marker file the scenario writes from its Bash tool. We
  # cannot grep $log for LOCAL_AGENT_READY: gb agent start --local is
  # invoked with stdin from /dev/null, so coop attach fails to enter raw
  # mode (tcgetattr ENOTTY on stdin) and PTY-rendered Bash output never
  # reaches the runner's stdout. Same side-channel pattern as
  # local-yield-close (/tmp/local-yield-decision-id) and
  # local-escalation-roundtrip (/tmp/local-escalation-handled).
  local marker=/tmp/local-sigterm-marker
  rm -f "$marker"

  local log; log="$(log_for "$name")"
  env $(pty_trace_env_for "$name") \
    "$GB_BIN" agent start --local \
    --name="e2e-local-${TEST_TAG}-${name}" \
    --role=crew \
    --dir="$WORK_DIR" \
    --test-scenario="$name" \
    --test-scenario-dir="$SCENARIO_DIR" \
    </dev/null >"$log" 2>&1 &
  local runner_pid=$!

  # Wait for the scenario's READY marker on the side channel.
  local start=$SECONDS ready=0
  while [ $((SECONDS - start)) -lt "$SCENARIO_TIMEOUT" ]; do
    if [ -s "$marker" ] && grep -q "LOCAL_AGENT_READY" "$marker" 2>/dev/null; then
      ready=1
      break
    fi
    if ! kill -0 "$runner_pid" 2>/dev/null; then
      break
    fi
    sleep 1
  done

  if [ "$ready" -eq 1 ]; then
    pass "$name: agent reached LOCAL_AGENT_READY"
  else
    fail "$name: agent reached LOCAL_AGENT_READY" "marker never appeared in $marker"
    dump_scenario_log "$name" "$log"
    # Runner already exited or hung — fall through to collect whatever rc it had.
  fi

  # Send SIGTERM to the runner and wait for clean shutdown.
  if kill -0 "$runner_pid" 2>/dev/null; then
    kill -TERM "$runner_pid" 2>/dev/null || true
  fi

  local wait_start=$SECONDS rc=0
  while kill -0 "$runner_pid" 2>/dev/null; do
    if [ $((SECONDS - wait_start)) -ge "$SIGTERM_GRACE" ]; then
      dim "  runner did not exit within ${SIGTERM_GRACE}s — sending KILL"
      kill -KILL "$runner_pid" 2>/dev/null || true
      rc=137
      break
    fi
    sleep 1
  done
  if [ "$rc" -ne 137 ]; then
    wait "$runner_pid" 2>/dev/null
    rc=$?
  fi

  # Scenario-specific: child should have written the signal marker to the
  # side channel.
  if [ -s "$marker" ] && grep -q "LOCAL_SIGTERM_RECEIVED" "$marker" 2>/dev/null; then
    pass "$name: SIGTERM propagated to child"
  else
    fail "$name: SIGTERM propagated to child" "no LOCAL_SIGTERM_RECEIVED marker in $marker"
  fi

  # Archive the marker alongside the scenario log for post-mortem.
  if [ -n "$LOG_ARCHIVE_DIR" ] && [ -d "$LOG_ARCHIVE_DIR" ] && [ -s "$marker" ]; then
    cp "$marker" "$LOG_ARCHIVE_DIR/${name}.marker" 2>/dev/null || true
  fi
  rm -f "$marker"

  # For sigterm, the runner exiting due to signal is the pass case.
  # We still want the agent bead closed.
  local agent_id
  agent_id="$(extract_agent_bead_id "$log")"
  if [ -z "$agent_id" ]; then
    fail "$name: agent bead registered" "no 'Agent bead:' banner in log"
  else
    pass "$name: agent bead registered ($agent_id)"
    # On SIGTERM the close may happen after the wait, poll briefly.
    if wait_for_status "$agent_id" '^closed$' 15 >/dev/null; then
      pass "$name: agent bead closed after SIGTERM"
    else
      fail "$name: agent bead closed after SIGTERM" "status never reached 'closed'"
      "$KD_BIN" close "$agent_id" >/dev/null 2>&1 || true
    fi
  fi

  # Exit code diagnostics — SIGTERM can surface as 143 or an error string.
  dim "  runner exit code: $rc"
  # If the runner exited unexpectedly (not via signal, not clean), dump the
  # log so the failure is debuggable from shard stdout. 0/137/143 are the
  # expected cases for a SIGTERM-driven shutdown.
  case "$rc" in
    0|137|143) ;;
    *) dump_scenario_log "$name" "$log" ;;
  esac
}

# selftest_portforward_resilience — regression check for gd-FtiTtZyuj2.
# Forcibly kills the kubectl process behind our supervised port-forward and
# asserts that (a) a new kubectl is listening on the same port within ~10s
# and (b) `kd` can talk to the server through it. If this self-test ever
# fails, the cascade is back and downstream status=<unreachable> failures
# are imminent — better to catch it here than at scenario 6 of 7.
#
# Skipped when there's no supervisor (caller-provided BEADS_HTTP_URL) or
# when lsof is unavailable (can't identify the kubectl pid to kill).
selftest_portforward_resilience() {
  blue "Self-test: port-forward survives a forced kubectl kill"
  if [ -z "$PF_PORT" ]; then
    skip "selftest: port-forward respawn" "no supervisor (caller-provided BEADS_HTTP_URL)"
    return 0
  fi
  if ! have lsof; then
    skip "selftest: port-forward respawn" "lsof not available"
    return 0
  fi

  if ! "$KD_BIN" list --status=open --limit 1 --json >/dev/null 2>&1; then
    fail "selftest: baseline kd reachable" "kd unreachable before kill"
    return 1
  fi
  pass "selftest: baseline kd reachable"

  local before_pid after_pid
  before_pid="$(lsof -t -iTCP:"$PF_PORT" -sTCP:LISTEN 2>/dev/null | head -n1 || true)"
  if [ -z "$before_pid" ]; then
    fail "selftest: locate kubectl pid" "no listener on :$PF_PORT"
    return 1
  fi
  dim "  killing kubectl pid $before_pid on :$PF_PORT"
  kill -KILL "$before_pid" 2>/dev/null || true

  # Supervisor backoff is 1s; allow up to 10s for the new kubectl to come
  # up AND bind the port AND be reachable from kd.
  local start=$SECONDS
  while [ $((SECONDS - start)) -lt 10 ]; do
    after_pid="$(lsof -t -iTCP:"$PF_PORT" -sTCP:LISTEN 2>/dev/null | head -n1 || true)"
    if [ -n "$after_pid" ] && [ "$after_pid" != "$before_pid" ]; then
      break
    fi
    sleep 1
  done
  if [ -z "$after_pid" ] || [ "$after_pid" = "$before_pid" ]; then
    fail "selftest: kubectl respawned" \
      "listener never replaced on :$PF_PORT (before=$before_pid, after=${after_pid:-<none>})"
    return 1
  fi
  pass "selftest: kubectl respawned ($before_pid → $after_pid)"

  # Give the new kubectl a couple seconds to wire up its SPDY stream.
  start=$SECONDS
  while [ $((SECONDS - start)) -lt 8 ]; do
    if "$KD_BIN" list --status=open --limit 1 --json >/dev/null 2>&1; then
      pass "selftest: kd reachable after respawn"
      return 0
    fi
    sleep 1
  done
  fail "selftest: kd reachable after respawn" "kd still failing 8s after new kubectl bound :$PF_PORT"
  return 1
}

# ── Main ──────────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue "  gb agent start --local Scenario Runner"
  blue "═══════════════════════════════════════════════════"
  echo

  # Verify required binaries.
  local missing=()
  for bin in gb kd coop claudeless; do
    have "$bin" || missing+=("$bin")
  done
  if [ "${#missing[@]}" -gt 0 ]; then
    dim "SKIP: missing required binaries: ${missing[*]}"
    exit 0
  fi

  setup_portforward
  setup_token

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  # Regression check (gd-FtiTtZyuj2): prove the supervisor recovers from a
  # forced kubectl kill before we start eating into the per-scenario budget.
  selftest_portforward_resilience || true
  echo

  # Drivers in a stable order. Each is independent — one failing does not
  # short-circuit the rest. We re-check the port-forward between scenarios
  # because a coop shutdown in one scenario has been observed (gd-4f6rVx0u2y)
  # to take down the kubectl port-forward, cascading every subsequent
  # scenario into "connection refused" at bead registration.
  ensure_portforward || true
  run_scenario_lifecycle         || true
  echo
  ensure_portforward || true
  run_scenario_basic "local-prime-fetch"      || true
  echo
  ensure_portforward || true
  run_scenario_yield             || true
  echo
  ensure_portforward || true
  run_scenario_sigterm           || true
  echo
  ensure_portforward || true
  run_scenario_basic "local-orphan-cleanup"   || true
  echo
  ensure_portforward || true
  run_scenario_escalation_roundtrip || true
  echo

  # ── Summary ─────────────────────────────────────────────────────
  blue "═══════════════════════════════════════════════════"
  local total=$((PASS + FAIL))
  if [ "$FAIL" -eq 0 ]; then
    green "  ✓ All $total assertions passed ($SKIP skipped)"
  else
    red "  ✗ $FAIL/$total assertions failed ($SKIP skipped)"
    echo
    for err in "${ERRORS[@]}"; do
      red "   - $err"
    done
  fi
  blue "═══════════════════════════════════════════════════"

  [ "$FAIL" -eq 0 ]
}

main "$@"
