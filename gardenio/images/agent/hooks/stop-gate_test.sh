#!/bin/bash
# stop-gate_test.sh — --format switch of stop-gate.sh (Claude exit 2 vs Codex JSON).
#
# Mocks gb and curl on PATH. stop-gate.sh keeps its cooldown/yield state in fixed
# /tmp paths, which would clobber a live agent's gate when run in a pod, so the
# script is copied with those paths redirected into a temp dir first.
# Run: bash stop-gate_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/bin" "$WORK/home"

sed "s#/tmp/stop-gate-#$WORK/stop-gate-#g; s#/home/agent/.claude/stop-gate-text.md#$WORK/no-such-text.md#" \
    "$SCRIPT_DIR/stop-gate.sh" > "$WORK/stop-gate.sh"

# gb: `bus emit` blocks (exit 2, reason JSON on stderr) unless MOCK_ALLOW is set.
cat > "$WORK/bin/gb" <<'MOCK'
#!/bin/bash
if [ "$1 $2" = "bus emit" ]; then
    cat >/dev/null
    [ -n "${MOCK_ALLOW:-}" ] && exit 0
    echo '{"decision":"block","reason":"gate says no"}' >&2
    exit 2
fi
exit 0
MOCK
printf '#!/bin/sh\nexit 1\n' > "$WORK/bin/curl"
chmod +x "$WORK/bin/gb" "$WORK/bin/curl"
export PATH="$WORK/bin:$PATH"
unset BOAT_MODE GARDENIO_HARNESS MOCK_ALLOW || true

FAIL=0
check() { # name, ok-condition-result
    if [ "$2" = ok ]; then echo "PASS: $1"; else echo "FAIL: $1"; FAIL=1; fi
}
reset() { rm -f "$WORK"/stop-gate-*; }

# Claude (default): exit 2, prose on stdout, no JSON requirement on stdout.
reset
set +e
out=$(echo '{}' | bash "$WORK/stop-gate.sh" 2>/dev/null); rc=$?
set -e
check "claude: block exits 2" "$([ "$rc" = 2 ] && echo ok)"
check "claude: stdout is checkpoint prose" "$(echo "$out" | grep -q 'STOP BLOCKED' && echo ok)"

# Codex: exit 0 and stdout is exactly one JSON block document.
reset
set +e
out=$(echo '{}' | bash "$WORK/stop-gate.sh" --format codex 2>/dev/null); rc=$?
set -e
check "codex: block exits 0" "$([ "$rc" = 0 ] && echo ok)"
check "codex: stdout is a single JSON doc" "$(echo "$out" | jq -e . >/dev/null 2>&1 && echo ok)"
check "codex: decision=block" "$([ "$(echo "$out" | jq -r .decision)" = block ] && echo ok)"
check "codex: reason has gate reason" "$(echo "$out" | jq -r .reason | grep -q 'gate says no' && echo ok)"
check "codex: reason has checkpoint prose" "$(echo "$out" | jq -r .reason | grep -q 'STOP BLOCKED' && echo ok)"

# Codex via GARDENIO_HARNESS, no flag (what hooks.json relies on as a fallback).
reset
set +e
out=$(echo '{}' | GARDENIO_HARNESS=codex bash "$WORK/stop-gate.sh" 2>/dev/null); rc=$?
set -e
check "codex via env: exit 0 + JSON" "$([ "$rc" = 0 ] && echo "$out" | jq -e '.decision=="block"' >/dev/null 2>&1 && echo ok)"

# Codex allow: no output at all, exit 0.
reset
set +e
out=$(echo '{}' | MOCK_ALLOW=1 bash "$WORK/stop-gate.sh" --format codex 2>/dev/null); rc=$?
set -e
check "codex: allow is silent exit 0" "$([ "$rc" = 0 ] && [ -z "$out" ] && echo ok)"

exit "$FAIL"
