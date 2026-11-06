#!/usr/bin/env bash
# test-claudeless-multi-nudge-isolated.sh — diagnostic repro for ka-0000002034
#
# Companion to test-claudeless-scenario-isolated.sh. That script tests the
# FIRST [[responses]] rule fires from a single startup nudge. This one
# tests that ALL phases of a multi-prompt scenario fire when nudges are
# delivered as a burst sequence under PTY — emulating the multi-nudge
# runner MR !2604 added to gb agent start --local.
#
# Two modes (both should pass on claudeless 0.4.0):
#
#   gated   — wait 2s after each phase fires before sending the next nudge
#             (emulates hard-idle-transition gate from MR !2633)
#   burst   — send all nudges in rapid succession before Phase 1 completes
#             (stress-tests crossterm event queueing during Responding mode)
#   coop    — burst mode, but each nudge is `<msg>\r` with NO leading Ctrl-U
#             (emulates coop's SafeNudgeEncoder exactly)
#
# Purpose: confirm whether the "16/31 red on tip-of-main ca874af42" failure
# fingerprint (ka-0000002034 comments, gardenio-e2e-regression-localmode-e6q
# 22:10 UTC) reproduces against upstream claudeless 0.4.0 under direct PTY
# write. If all three modes pass here but the full E2E still fails, the
# bug is NOT in claudeless's [[responses]] dispatch — it is in coop's
# delivery pipeline (timing, state machine, FIFO hook coordination) or in
# how the E2E harness measures step_count.
#
# Usage:
#   ./test-claudeless-multi-nudge-isolated.sh             # run all 3 modes
#   ./test-claudeless-multi-nudge-isolated.sh gated
#   ./test-claudeless-multi-nudge-isolated.sh burst
#   ./test-claudeless-multi-nudge-isolated.sh coop
#
# Prerequisites: claudeless binary in PATH, python3.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCENARIO_DIR="$SCRIPT_DIR/../claudeless"
PYTHON="${PYTHON:-python3}"
SCENARIO="${SCENARIO:-local-lifecycle}"

if ! command -v claudeless >/dev/null 2>&1; then
  echo "ERROR: claudeless not in PATH" >&2
  exit 1
fi

TOML="$SCENARIO_DIR/$SCENARIO.toml"
PROMPT_FILE="$SCENARIO_DIR/$SCENARIO.startup-prompt.txt"
NUDGES_FILE="$SCENARIO_DIR/$SCENARIO.nudges.txt"

if [[ ! -f "$TOML" ]]; then
  echo "ERROR: scenario TOML not found: $TOML" >&2
  exit 1
fi
if [[ ! -f "$PROMPT_FILE" ]]; then
  echo "ERROR: startup-prompt sidecar not found: $PROMPT_FILE" >&2
  exit 1
fi
if [[ ! -f "$NUDGES_FILE" ]]; then
  echo "ERROR: nudges sidecar not found: $NUDGES_FILE" >&2
  exit 1
fi

PROMPT="$(cat "$PROMPT_FILE")"

# Read nudges; strip comments + blanks. First nudge after startup-prompt = NUDGES[0]
mapfile -t NUDGE_LINES < <(grep -vE '^\s*(#|$)' "$NUDGES_FILE")

# Extract expected `say` strings from each [[responses]].steps[*].say entry
# so we can assert each phase fires. Tolerates whitespace + quote variants.
mapfile -t EXPECTS < <(
  awk '/say *=/ { sub(/^[^"]*"/, ""); sub(/"[^"]*$/, ""); print }' "$TOML"
)
# Trim trailing idle phrase if scenario has one (we only assert through phase N nudges + 1).
NUDGE_COUNT=${#NUDGE_LINES[@]}
KEEP=$((NUDGE_COUNT + 1))
if (( ${#EXPECTS[@]} > KEEP )); then
  EXPECTS=("${EXPECTS[@]:0:KEEP}")
fi

if (( ${#EXPECTS[@]} == 0 )); then
  echo "ERROR: no say= phrases parsed from $TOML" >&2
  exit 1
fi

MODE="${1:-all}"

run_mode() {
  local mode="$1"
  echo "=== mode: $mode ==="
  "$PYTHON" - "$TOML" "$mode" "$PROMPT" "${EXPECTS[@]}" "${NUDGE_LINES[@]}" <<'PYEOF'
import os, pty, select, sys, time, signal, re

argv = sys.argv[1:]
toml = argv[0]
mode = argv[1]
prompt = argv[2]

# Optional v1 PTY trace, matching coop's NativePty tracer format
# (gardenio/coop/crates/cli/src/backend/spawn.rs). Same env var name so the
# isolated repro and the failing E2E shard produce byte-comparable artifacts
# for diagnostic ka-0000002034.
_trace_path = os.environ.get("COOP_PTY_TRACE_FILE") or ""
_trace_fh = None
if _trace_path:
    try:
        _trace_fh = open(_trace_path, "ab")
        _now_us = int(time.time() * 1_000_000)
        _trace_fh.write(f"# coop pty trace v1 pid={os.getpid()} ts_micros={_now_us}\n".encode())
        _trace_fh.flush()
    except OSError:
        _trace_fh = None

def _trace(direction, data):
    if _trace_fh is None or not data:
        return
    try:
        now_us = int(time.time() * 1_000_000)
        _trace_fh.write(f"{now_us}\t{direction}\t{data.hex()}\n".encode())
        _trace_fh.flush()
    except OSError:
        pass
# Remaining: EXPECTS then NUDGES. We can't know the split without metadata,
# so the calling shell separates them by inserting a literal "--" sentinel
# we don't use here; instead we expect EXPECTS first matching say= count
# and NUDGES filling the rest. We rely on the runner to pass them in order
# and the test scenario to have N expects = N+1 say lines vs N nudges (off-by-one).
# Simpler: hard-code for local-lifecycle (5 expects + 4 nudges) and bail otherwise.
rest = argv[3:]

# Heuristic split: scenarios we test here have one more say-phrase than nudge,
# because Phase 1 fires from startup-prompt (no nudge needed).
# Caller must verify this invariant before adding scenarios.
n_expects = (len(rest) + 1) // 2
expects = rest[:n_expects]
nudges  = rest[n_expects:]

pid, master = pty.fork()
if pid == 0:
    os.environ["TERM"] = "xterm-256color"
    os.environ["KD_AGENT_ID"] = "gd-fake"
    cmd = f"claudeless --scenario {toml} --dangerously-skip-permissions"
    os.execvp("sh", ["sh", "-c", cmd])
    sys.exit(127)

out = bytearray()
start = time.time()
phase = "wait-setup"
setup_at = None
TIMEOUT = 90
seen = [False] * len(expects)

def _w(data):
    _trace("W", data)
    os.write(master, data)

def send_with_ctrl_u(text):
    _w(b"\x15"); time.sleep(0.05)
    _w(text.encode()); time.sleep(0.05)
    _w(b"\r")

def send_coop_style(text):
    # SafeNudgeEncoder: message bytes + \r, NO Ctrl-U
    _w(text.encode()); time.sleep(0.05)
    _w(b"\r")

burst_sent = False
nudge_idx = 0
sender = send_coop_style if mode == "coop" else send_with_ctrl_u

while time.time() - start < TIMEOUT:
    r, _, _ = select.select([master], [], [], 0.2)
    if r:
        try: chunk = os.read(master, 8192)
        except OSError: chunk = b""
        if chunk:
            _trace("R", chunk)
            out.extend(chunk)
    stripped = re.sub(r"\x1b\[[0-9;?]*[a-zA-Z]", "", out.decode("utf-8", errors="replace"))
    elapsed = time.time() - start

    for i, e in enumerate(expects):
        if not seen[i] and e in stripped:
            seen[i] = True
            print(f"  [{elapsed:5.1f}s] phase{i+1}: {e!r}")

    if phase == "wait-setup" and "Yes, I accept" in stripped and elapsed > 1.0:
        _w(b"\x1b[B\r")
        setup_at = time.time()
        phase = "setup-sent"
    elif phase == "setup-sent" and time.time() - setup_at > 2.0:
        if mode in ("burst", "coop"):
            print(f"  [{elapsed:5.1f}s] burst: startup-prompt + {len(nudges)} nudges back-to-back")
            sender(prompt); time.sleep(0.1)
            for n in nudges:
                sender(n); time.sleep(0.05)
            burst_sent = True
            phase = "wait"
        else:  # gated
            sender(prompt)
            phase = "gated-prompt"
            burst_sent = True
    elif phase == "gated-prompt" and burst_sent:
        # Wait for phase[nudge_idx+1] to fire, then send next nudge
        if nudge_idx < len(nudges) and seen[nudge_idx]:
            time.sleep(2.0)
            print(f"  [{elapsed:5.1f}s] gated: sending nudge {nudge_idx+1}/{len(nudges)}")
            sender(nudges[nudge_idx])
            nudge_idx += 1
        if nudge_idx >= len(nudges) and elapsed > 20:
            phase = "wait"
    elif phase == "wait" and elapsed > 35:
        break
    if all(seen): break

try: os.kill(pid, signal.SIGTERM)
except ProcessLookupError: pass
try: os.waitpid(pid, 0)
except ChildProcessError: pass

print()
for i, e in enumerate(expects):
    mark = "✓" if seen[i] else "✗"
    print(f"  {mark} phase {i+1}: {e}")
total = sum(seen)
print(f"\nResult: {total}/{len(expects)} phases observed [mode={mode}]")
sys.exit(0 if total == len(expects) else 1)
PYEOF
}

if [[ "$MODE" == "all" ]]; then
  FAIL=0
  for m in gated burst coop; do
    if ! run_mode "$m"; then FAIL=$((FAIL+1)); fi
    echo
  done
  if (( FAIL > 0 )); then
    echo "FAIL: $FAIL/3 modes failed"
    exit 1
  fi
  echo "PASS: 3/3 modes (gated, burst, coop)"
else
  run_mode "$MODE"
fi
