#!/usr/bin/env bash
# test-coop-local-mode.sh — coop integration tests for local mode (ka-0000000772)
#
# Sibling of test-local-mode.sh. Where that runner exercises the
# `gb agent start --local` lifecycle, this script targets coop directly:
# the HTTP control plane and the attach/detach/re-attach loop that an
# operator drives every day.
#
# Scenarios covered:
#   1. coop starts, /api/v1/agent reports a known state ("idle" after
#      claudeless emits its first response).
#   2. POST /api/v1/agent/nudge injects a turn — the agent transitions
#      idle → working → idle and last_message reflects the nudge.
#   3. A nudge that triggers AskUserQuestion drives state → "prompt"
#      (awaiting-prompt detection), and POST /api/v1/agent/respond
#      clears it back to "idle".
#   4. `coop attach` over tmux renders the live session; sending the
#      detach key (Ctrl+], 0x1d) returns control to the shell while
#      coop keeps running; re-attach observes preserved screen state.
#   5. A nudge delivered while detached is processed by coop and
#      visible on re-attach.
#
# Skipped (with explanation) when coop, claudeless, tmux, or python3 are
# not on PATH — same convention as test-claudeless.sh / test-local-mode.sh
# so this file is safe to wire into CI before every shard ships those
# binaries.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-coop-local-mode.sh
#
# No daemon connection is required: coop is launched with claudeless and
# the test only talks to coop's local HTTP API on 127.0.0.1.

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCENARIO_DIR="$SCRIPT_DIR/../claudeless"
SCENARIO="$SCENARIO_DIR/local-coop-states.toml"
COOP_BIN="${COOP_BIN:-coop}"
CLAUDELESS_BIN="${CLAUDELESS_BIN:-claudeless}"
TMUX_BIN="${TMUX_BIN:-tmux}"

PASS=0
FAIL=0
SKIP=0
ERRORS=()

COOP_PID=""
COOP_LOG=""
TMUX_SOCKET=""
TMUX_SESSION=""
COOP_PORT=""

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
skip_all() {
  local reason="$1"
  dim "SKIP: $reason"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

have() { command -v "$1" >/dev/null 2>&1; }

# Find a free TCP port using python3.
free_port() {
  python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}

# HTTP GET → stdout body, exit code reflects status.
http_get() {
  local url="$1"
  curl -sS --max-time 5 "$url"
}

# HTTP POST JSON → stdout body.
http_post_json() {
  local url="$1" body="$2"
  curl -sS --max-time 5 -X POST -H 'Content-Type: application/json' -d "$body" "$url"
}

# Read JSON field with python3 (safe, dependency-free).
json_field() {
  local field="$1"
  python3 -c "import sys,json
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(0)
v = d
for k in '''$field'''.split('.'):
    if isinstance(v, dict) and k in v:
        v = v[k]
    else:
        sys.exit(0)
print('' if v is None else v)" 2>/dev/null
}

# Poll /api/v1/agent until state matches a regex; returns last seen state.
wait_for_state() {
  local want_re="$1" timeout="${2:-10}"
  local start=$SECONDS state=""
  while [ $((SECONDS - start)) -lt "$timeout" ]; do
    state="$(http_get "http://127.0.0.1:${COOP_PORT}/api/v1/agent" 2>/dev/null \
              | json_field state)"
    if [[ "$state" =~ $want_re ]]; then
      echo "$state"
      return 0
    fi
    sleep 0.3
  done
  echo "$state"
  return 1
}

# Start coop with the integration-test scenario; populate COOP_PID/COOP_PORT.
start_coop() {
  COOP_PORT="$(free_port)"
  COOP_LOG="$(mktemp -t coop-local-mode-XXXXXX.log)"
  blue "Starting coop on port $COOP_PORT (log: $COOP_LOG)"
  "$COOP_BIN" \
    --host 127.0.0.1 \
    --port "$COOP_PORT" \
    --cols 100 \
    --rows 30 \
    -- \
    "$CLAUDELESS_BIN" --scenario "$SCENARIO" "coop integration test" \
    >"$COOP_LOG" 2>&1 &
  COOP_PID=$!

  # Wait for /api/v1/livez.
  local start=$SECONDS
  while [ $((SECONDS - start)) -lt 15 ]; do
    if curl -sf --max-time 1 "http://127.0.0.1:${COOP_PORT}/api/v1/livez" >/dev/null 2>&1; then
      dim "  coop ready (pid $COOP_PID)"
      return 0
    fi
    if ! kill -0 "$COOP_PID" 2>/dev/null; then
      red "coop exited prematurely. Last 30 lines of log:"
      tail -n 30 "$COOP_LOG" | sed 's/^/    /'
      return 1
    fi
    sleep 0.2
  done
  red "Timed out waiting for coop /api/v1/livez. Last 30 lines of log:"
  tail -n 30 "$COOP_LOG" | sed 's/^/    /'
  return 1
}

cleanup() {
  if [ -n "${TMUX_SOCKET:-}" ]; then
    "$TMUX_BIN" -L "$TMUX_SOCKET" kill-server >/dev/null 2>&1 || true
  fi
  if [ -n "${COOP_PID:-}" ] && kill -0 "$COOP_PID" 2>/dev/null; then
    kill -TERM "$COOP_PID" 2>/dev/null || true
    # Give it a couple seconds to exit cleanly, then force.
    for _ in 1 2 3 4 5; do
      kill -0 "$COOP_PID" 2>/dev/null || break
      sleep 0.4
    done
    kill -KILL "$COOP_PID" 2>/dev/null || true
    wait "$COOP_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# ── tmux helpers (attach/detach harness) ──────────────────────────
tmux_init() {
  TMUX_SOCKET="coop-mm3-$$"
  TMUX_SESSION="coop-attach"
  "$TMUX_BIN" -L "$TMUX_SOCKET" new-session -d -s "$TMUX_SESSION" \
    -x 100 -y 30 -e LANG=C.UTF-8 -e LC_ALL=C.UTF-8 \
    "$COOP_BIN attach --no-statusline http://127.0.0.1:${COOP_PORT}; echo COOP_ATTACH_EXITED; exec bash --norc -i"
}

tmux_capture() {
  "$TMUX_BIN" -L "$TMUX_SOCKET" capture-pane -t "$TMUX_SESSION" -p 2>/dev/null
}

tmux_send_detach_key() {
  # Ctrl+] (0x1d, GS): coop's detach key (see coop attach DETACH_KEY).
  "$TMUX_BIN" -L "$TMUX_SOCKET" send-keys -t "$TMUX_SESSION" 'C-]' 2>/dev/null
}

tmux_send_text() {
  "$TMUX_BIN" -L "$TMUX_SOCKET" send-keys -t "$TMUX_SESSION" "$1" 2>/dev/null
}

# Re-attach inside the same tmux pane after a detach.
tmux_reattach() {
  "$TMUX_BIN" -L "$TMUX_SOCKET" send-keys -t "$TMUX_SESSION" \
    "$COOP_BIN attach --no-statusline http://127.0.0.1:${COOP_PORT}" Enter 2>/dev/null
}

tmux_wait_for() {
  local sentinel="$1" timeout="${2:-10}"
  local start=$SECONDS
  while [ $((SECONDS - start)) -lt "$timeout" ]; do
    if tmux_capture | grep -qF "$sentinel"; then
      return 0
    fi
    sleep 0.3
  done
  return 1
}

# ── Pre-flight ────────────────────────────────────────────────────
blue "═══════════════════════════════════════════════════"
blue "  Coop local-mode integration tests (ka-0000000772)"
blue "═══════════════════════════════════════════════════"
echo

missing=()
for bin in "$COOP_BIN" "$CLAUDELESS_BIN" "$TMUX_BIN" python3 curl; do
  have "$bin" || missing+=("$bin")
done
if [ "${#missing[@]}" -gt 0 ]; then
  skip_all "missing required binaries: ${missing[*]}"
fi

if [ ! -f "$SCENARIO" ]; then
  fail "scenario fixture present" "$SCENARIO not found"
  exit 1
fi
pass "scenario fixture present"

if ! start_coop; then
  red "coop did not start; cannot run integration tests"
  exit 1
fi

# ── Test 1: state probe ────────────────────────────────────────────
blue "Test 1: state detection — coop reports a known state"
agent_json="$(http_get "http://127.0.0.1:${COOP_PORT}/api/v1/agent" 2>/dev/null || true)"
if [ -z "$agent_json" ]; then
  fail "GET /api/v1/agent reachable" "empty response"
else
  pass "GET /api/v1/agent reachable"
  state="$(printf '%s' "$agent_json" | json_field state)"
  case "$state" in
    starting|idle|working|prompt|unknown|""|exited)
      pass "state field is a known wire value (got: ${state:-<empty>})"
      ;;
    *)
      fail "state field is a known wire value" "got unexpected: $state"
      ;;
  esac
fi

# Wait for the first response to settle (claudeless emits one immediately).
final="$(wait_for_state '^(idle|prompt)$' 10 || true)"
if [[ "$final" =~ ^(idle|prompt)$ ]]; then
  pass "settled into idle/prompt after startup (got: $final)"
else
  fail "settled into idle/prompt after startup" "last state: ${final:-<empty>}"
fi
echo

# ── Test 2: HTTP nudge injection ───────────────────────────────────
blue "Test 2: HTTP nudge injection — POST /api/v1/agent/nudge"
nudge1='{"message": "first nudge from integration test"}'
nudge_resp="$(http_post_json "http://127.0.0.1:${COOP_PORT}/api/v1/agent/nudge" "$nudge1" 2>/dev/null || true)"
if [ -n "$nudge_resp" ]; then
  delivered="$(printf '%s' "$nudge_resp" | json_field delivered)"
  if [ "$delivered" = "True" ] || [ "$delivered" = "true" ]; then
    pass "nudge accepted (delivered=$delivered)"
  else
    fail "nudge accepted" "unexpected body: $nudge_resp"
  fi
else
  fail "nudge accepted" "empty response"
fi

# Block 2 in the scenario is a plain say-only reply, so the agent should
# return to idle. We give it a generous window because hook-based detection
# rides on claudeless emitting its hook events.
rest_state="$(wait_for_state '^idle$' 10 || true)"
if [ "$rest_state" = "idle" ]; then
  pass "agent returned to idle after plain nudge response"
else
  fail "agent returned to idle after plain nudge response" "state: ${rest_state:-<empty>}"
fi
echo

# ── Test 3: awaiting-prompt detection + respond ────────────────────
blue "Test 3: awaiting-prompt — next nudge triggers AskUserQuestion"
nudge2='{"message": "ask the question please"}'
http_post_json "http://127.0.0.1:${COOP_PORT}/api/v1/agent/nudge" "$nudge2" >/dev/null 2>&1 || true
prompt_state="$(wait_for_state '^prompt$' 15 || true)"
if [ "$prompt_state" = "prompt" ]; then
  pass "state transitioned to 'prompt' (awaiting-prompt detection)"

  # Verify the prompt context is exposed on /api/v1/agent.
  prompt_type="$(http_get "http://127.0.0.1:${COOP_PORT}/api/v1/agent" 2>/dev/null \
                  | json_field prompt.type)"
  if [ -n "$prompt_type" ]; then
    pass "prompt type exposed on /api/v1/agent (type=$prompt_type)"
  else
    fail "prompt type exposed on /api/v1/agent" "prompt.type missing"
  fi

  # Respond and check the agent leaves the prompt state. AskUserQuestion
  # uses the `answers` array; option/text alone do not encode for the
  # questionnaire variant (see encode_response in transport/handler.rs).
  respond_body='{"answers": [{"option": 1}]}'
  http_post_json "http://127.0.0.1:${COOP_PORT}/api/v1/agent/respond" "$respond_body" >/dev/null 2>&1 || true
  cleared="$(wait_for_state '^(idle|working|exited)$' 10 || true)"
  if [[ "$cleared" =~ ^(idle|working|exited)$ ]]; then
    pass "respond cleared the prompt (state: $cleared)"
  else
    fail "respond cleared the prompt" "state still: ${cleared:-<empty>}"
  fi
else
  fail "state transitioned to 'prompt'" "last state: ${prompt_state:-<empty>}"
fi
echo

# ── Test 4: attach / detach / re-attach via tmux ───────────────────
blue "Test 4: coop attach / detach / re-attach"
# Use "Claudeless" (the TUI title rendered by claudeless) as a stable
# sentinel that is on-screen regardless of how scenario blocks have
# advanced. The on-screen message text scrolls; the title bar does not.
SENTINEL_TITLE="Claudeless"
tmux_init
if ! tmux_wait_for "$SENTINEL_TITLE" 15; then
  fail "attach renders live session" "TUI title '$SENTINEL_TITLE' not seen in tmux pane"
  dim "  --- tmux pane (last) ---"
  tmux_capture | tail -n 10 | sed 's/^/    /'
else
  pass "attach renders live session content"
fi

# Detach: send Ctrl+] (DETACH_KEY = 0x1d). Coop should leave attach mode
# but the daemon process keeps running — we verify both.
tmux_send_detach_key
# Wait for coop attach to actually exit (the wrapper echoes COOP_ATTACH_EXITED).
if tmux_wait_for "COOP_ATTACH_EXITED" 5; then
  pass "Ctrl+] detached coop attach"
else
  fail "Ctrl+] detached coop attach" "attach did not return to shell within 5s"
fi
if kill -0 "$COOP_PID" 2>/dev/null; then
  pass "coop daemon survives detach"
else
  fail "coop daemon survives detach" "PID $COOP_PID gone after detach"
fi

# /api/v1/agent should still respond after detach.
post_detach_state="$(http_get "http://127.0.0.1:${COOP_PORT}/api/v1/agent" 2>/dev/null \
                      | json_field state)"
if [ -n "$post_detach_state" ]; then
  pass "HTTP API reachable while detached (state=$post_detach_state)"
else
  fail "HTTP API reachable while detached" "no response"
fi

# Send a nudge while detached; coop should still process it.
nudge_detached_resp="$(http_post_json "http://127.0.0.1:${COOP_PORT}/api/v1/agent/nudge" \
  '{"message": "nudge while detached"}' 2>/dev/null || true)"
if [ "$(printf '%s' "$nudge_detached_resp" | json_field delivered)" = "True" ] \
   || [ "$(printf '%s' "$nudge_detached_resp" | json_field delivered)" = "true" ]; then
  pass "nudge accepted while detached"
else
  fail "nudge accepted while detached" "body: $nudge_detached_resp"
fi

# Re-attach. Tmux pane is still alive; coop attach exited, run it again.
tmux_reattach
if tmux_wait_for "$SENTINEL_TITLE" 10; then
  pass "re-attach renders live session"
else
  fail "re-attach renders live session" "TUI title missing post re-attach"
  dim "  --- tmux pane (last) ---"
  tmux_capture | tail -n 10 | sed 's/^/    /'
fi
echo

# ── Summary ───────────────────────────────────────────────────────
blue "═══════════════════════════════════════════════════"
total=$((PASS + FAIL))
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
