#!/usr/bin/env bash
# test-claudeless-scenario-isolated.sh — isolated repro for ka-0000002034
#
# Spawns claudeless --scenario inside a Python forkpty wrapper (mirroring
# coop's backend/spawn.rs::Native PTY) and drives it through the same
# byte sequence coop sends:
#
#   1. Down + Return         → dismiss bypass_permissions setup prompt
#   2. Ctrl-U + msg + \r     → coop SafeNudgeEncoder (msg bytes, delay, Enter)
#
# Asserts each scenario in gardenio/tests/e2e/claudeless/local-*.toml fires
# its FIRST `[[responses]]` rule when given its startup-prompt sidecar.
#
# This isolates the claudeless+coop byte protocol from the rest of the
# local-mode E2E stack (kubectl, staging, port-forward, agent bead
# registration, scenario assertion framework). When this passes but the
# full E2E still fails, the bug is NOT in claudeless scenario plumbing —
# it is in higher layers (typically: scenario design requiring multiple
# user prompts when local-mode only injects one startup nudge).
#
# Usage:
#   ./test-claudeless-scenario-isolated.sh                  # run all
#   ./test-claudeless-scenario-isolated.sh local-lifecycle  # one scenario
#
# Prerequisites: claudeless binary in PATH, python3.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCENARIO_DIR="$SCRIPT_DIR/../claudeless"
PYTHON="${PYTHON:-python3}"

if ! command -v claudeless >/dev/null 2>&1; then
  echo "ERROR: claudeless not in PATH" >&2
  exit 1
fi

if ! command -v "$PYTHON" >/dev/null 2>&1; then
  echo "ERROR: $PYTHON not in PATH" >&2
  exit 1
fi

PASS=0
FAIL=0
FAILED_NAMES=()

run_one() {
  local scenario="$1"
  local toml="$SCENARIO_DIR/$scenario.toml"
  local prompt_file="$SCENARIO_DIR/$scenario.startup-prompt.txt"
  local prompt expected

  if [ ! -f "$toml" ]; then
    echo "  SKIP $scenario (no .toml)"
    return 0
  fi
  if [ ! -f "$prompt_file" ]; then
    echo "  SKIP $scenario (no .startup-prompt.txt)"
    return 0
  fi
  prompt="$(cat "$prompt_file")"

  # Pull the `say` text from the first [[responses]] block — that is what
  # we expect to see rendered after the nudge fires.
  expected="$(awk '
    /^\[\[responses\]\]/ { in_block = 1; next }
    /^\[/                { in_block = 0 }
    in_block && /^say *=/ {
      sub(/^say *= *"/, ""); sub(/"$/, ""); print; exit
    }' "$toml")"
  if [ -z "$expected" ]; then
    echo "  SKIP $scenario (no first-response 'say' to match against)"
    return 0
  fi

  echo "  RUN  $scenario"
  echo "       prompt=$prompt"
  echo "       expect=$expected"

  if PROMPT="$prompt" EXPECT="$expected" TOML="$toml" "$PYTHON" - <<'PYEOF'
import os, pty, select, sys, time, signal, re

TOML = os.environ["TOML"]
PROMPT = os.environ["PROMPT"]
EXPECT = os.environ["EXPECT"]
TIMEOUT = 30

pid, master = pty.fork()
if pid == 0:
    os.environ["TERM"] = "xterm-256color"
    cmd = f"claudeless --scenario {TOML} --dangerously-skip-permissions"
    os.execvp("sh", ["sh", "-c", cmd])
    sys.exit(127)

out = bytearray()
start = time.time()
phase = "wait-setup"
setup_at = None
nudge_at = None

while time.time() - start < TIMEOUT:
    r, _, _ = select.select([master], [], [], 0.2)
    if r:
        try:
            chunk = os.read(master, 8192)
        except OSError:
            chunk = b""
        if chunk:
            out.extend(chunk)
    stripped = re.sub(r"\x1b\[[0-9;?]*[a-zA-Z]", "", out.decode("utf-8", errors="replace"))
    elapsed = time.time() - start

    if phase == "wait-setup" and "Yes, I accept" in stripped and elapsed > 1.0:
        os.write(master, b"\x1b[B\r")
        setup_at = time.time()
        phase = "setup-sent"
    elif phase == "setup-sent" and time.time() - setup_at > 2.0:
        os.write(master, b"\x15")  # Ctrl-U
        time.sleep(0.2)
        os.write(master, PROMPT.encode())
        time.sleep(0.3)
        os.write(master, b"\r")
        nudge_at = time.time()
        phase = "nudge-sent"
    elif phase == "nudge-sent" and time.time() - nudge_at > 12.0:
        break

try:
    os.kill(pid, signal.SIGTERM)
except ProcessLookupError:
    pass
try:
    os.waitpid(pid, 0)
except ChildProcessError:
    pass

stripped = re.sub(r"\x1b\[[0-9;?]*[a-zA-Z]", "", out.decode("utf-8", errors="replace"))
if EXPECT in stripped:
    print(f"       PASS first-response 'say' rendered: {EXPECT!r}")
    sys.exit(0)
print(f"       FAIL did not see expected 'say': {EXPECT!r}")
print(f"       === last 1KB of stripped output ===")
print(stripped[-1024:])
sys.exit(1)
PYEOF
  then
    PASS=$((PASS + 1))
  else
    FAIL=$((FAIL + 1))
    FAILED_NAMES+=("$scenario")
  fi
}

if [ $# -gt 0 ]; then
  for s in "$@"; do
    run_one "$s"
  done
else
  for toml in "$SCENARIO_DIR"/local-*.toml; do
    s="$(basename "$toml" .toml)"
    run_one "$s"
  done
fi

echo
echo "=== results: $PASS pass, $FAIL fail ==="
if [ $FAIL -gt 0 ]; then
  printf '  FAILED: %s\n' "${FAILED_NAMES[@]}"
  exit 1
fi
