#!/bin/bash
# Stop hook gate: calls gb bus emit and handles block by injecting
# checkpoint protocol instructions so the agent knows what to do.
#
# Exit codes (Claude Code hook protocol):
#   0 = allow (agent may stop)
#   2 = block (agent must continue and create a decision checkpoint)

set -uo pipefail

# ── Output format ────────────────────────────────────────────────────────
# --format claude (default): block = exit 2, reason on stderr.
# --format codex: block = {"decision":"block","reason":...} as the ONLY stdout,
# exit 0. Codex treats plain stdout on Stop as invalid and exit 2 is untested
# there (ADR 0004 §8). Without --format the harness is taken from
# GARDENIO_HARNESS, same as `gb hook stop-gate`.
FORMAT="${GARDENIO_HARNESS:-claude}"
while [ $# -gt 0 ]; do
    case "$1" in
        --format) FORMAT="${2:-$FORMAT}"; shift 2 || shift ;;
        --format=*) FORMAT="${1#--format=}"; shift ;;
        *) shift ;;
    esac
done
[ "$FORMAT" = "codex" ] || FORMAT=claude

# ── Job-mode bypass ──────────────────────────────────────────────────────
# Job-mode agents (thread agents, one-shot tasks) have a different lifecycle:
# they idle and wait for follow-up messages rather than creating decision
# checkpoints. The stop gate is designed for crew-mode agents that should
# checkpoint before exiting. Allow job agents to stop freely.
if [ "${BOAT_MODE:-}" = "job" ]; then
    exit 0
fi

# ── Yield-aware fast path ────────────────────────────────────────────────
# When the agent is actively yielding on a decision, there's nothing more
# it can do — the decision exists, the agent is waiting. Exit 0 silently
# (not exit 2) to avoid the "[No stderr output]" feedback injection loop.
# gb yield blocks in the foreground and re-nudges Claude when the decision
# resolves, so allowing stop here is safe — the yield process keeps running.
YIELD_MARKER="/tmp/stop-gate-yielding"
if [ -f "$YIELD_MARKER" ]; then
    exit 0
fi

# ── Cooldown debouncing ──────────────────────────────────────────────────
# If we already blocked within the cooldown window, exit 2 silently
# (no text injection). This prevents the loop where every blocked stop
# injects the full checkpoint text, the model processes it (API cost),
# generates a response, and Claude Code immediately tries to stop again.
COOLDOWN_FILE="/tmp/stop-gate-last-block"
BLOCK_COUNT_FILE="/tmp/stop-gate-block-count"
BASE_COOLDOWN_SECS="${STOP_GATE_COOLDOWN_SECS:-30}"
MAX_BLOCKS="${STOP_GATE_MAX_BLOCKS:-5}"

# Read current block count.
block_count=0
if [ -f "$BLOCK_COUNT_FILE" ]; then
    block_count=$(cat "$BLOCK_COUNT_FILE" 2>/dev/null || echo 0)
fi

# ── Escape hatch ───────────────────────────────────────────────────────
# After too many consecutive blocks, allow the stop to prevent infinite
# cost-sink loops (decision → yield → block → repeat).
if [ "$block_count" -ge "$MAX_BLOCKS" ]; then
    echo "[stop-gate] Escape hatch: $block_count consecutive blocks, allowing stop" >&2
    echo "<system-reminder>Stop gate escape hatch activated after repeated blocks.</system-reminder>"
    rm -f "$COOLDOWN_FILE" "$BLOCK_COUNT_FILE"
    exit 0
fi

# ── Exponential cooldown debouncing ────────────────────────────────────
# Cooldown doubles with each block: 30s, 60s, 120s, 240s, 300s (cap).
if [ -f "$COOLDOWN_FILE" ]; then
    last_block=$(cat "$COOLDOWN_FILE" 2>/dev/null || echo 0)
    now=$(date +%s)
    elapsed=$(( now - last_block ))
    # Calculate exponential cooldown: base * 2^(block_count-1), cap at 300s.
    cooldown=$BASE_COOLDOWN_SECS
    i=1
    while [ "$i" -lt "$block_count" ] && [ "$cooldown" -lt 300 ]; do
        cooldown=$(( cooldown * 2 ))
        i=$(( i + 1 ))
    done
    [ "$cooldown" -gt 300 ] && cooldown=300
    if [ "$elapsed" -lt "$cooldown" ]; then
        # Still within cooldown — allow stop silently. Exit 0 (not 2) because
        # exit 2 doesn't actually prevent continuation when other Stop hooks
        # (e.g. coop pipe) run later and exit 0, but it DOES generate a
        # "[No stderr output]" feedback injection that wakes Claude up and
        # creates a rapid-fire stop loop. The gate already injected its text
        # on the first block; if Claude still wants to stop, let it.
        exit 0
    fi
fi

# ── Rate-limit escape hatch ─────────────────────────────────────────────
# If the agent is rate-limited, allow the stop unconditionally.
# This prevents the infinite loop: rate limit -> try to stop -> gate blocks ->
# try to create decision -> rate limit again.
_agent_state=$(curl -sf http://localhost:8080/api/v1/agent 2>/dev/null || echo '{}')
_error_cat=$(echo "$_agent_state" | jq -r '.error_category // empty' 2>/dev/null)
if [ "${_error_cat}" = "rate_limited" ]; then
    echo "[stop-gate] Agent is rate-limited, allowing stop without checkpoint" >&2
    gb gate clear decision 2>/dev/null || true
    rm -f "$COOLDOWN_FILE"
    exit 0
fi

# Read stdin (Claude Code hook JSON) and forward to gb bus emit.
# stderr flows through so Claude Code sees the block reason.
_stdin=$(cat)

if [ "$FORMAT" = "codex" ]; then
    # Keep the gate's stderr: it carries the block reason JSON.
    _emit_err=$(echo "$_stdin" | gb bus emit --hook=Stop 2>&1 >/dev/null)
    _rc=$?
else
    echo "$_stdin" | gb bus emit --hook=Stop
    _rc=$?
fi

if [ $_rc -eq 2 ]; then
    # Record block time and increment counter for exponential backoff.
    date +%s > "$COOLDOWN_FILE"
    block_count=$(( block_count + 1 ))
    echo "$block_count" > "$BLOCK_COUNT_FILE"

    # Gate blocked — inject checkpoint instructions into the conversation via stdout.
    # Prefer config-bead-materialized file; fall back to minimal inline text.
    # Claude prints this to stdout as it goes; Codex needs stdout to be a single
    # JSON document, so the same text is collected and folded into the reason.
    STOP_GATE_TEXT="/home/agent/.claude/stop-gate-text.md"
    _text=""
    if [ -f "$STOP_GATE_TEXT" ]; then
        _text=$(cat "$STOP_GATE_TEXT")
    else
        _text="<system-reminder>STOP BLOCKED — decision gate unsatisfied. Create a decision checkpoint (gb decision create + gb yield) or call gb done if work is complete.</system-reminder>"
    fi

    # After repeated blocks, add escalating guidance.
    if [ "$block_count" -gt 1 ]; then
        _text="${_text}
<system-reminder>You have been blocked ${block_count} time(s). After gb yield returns, CONTINUE WORKING on the decision outcome. Only call gb done when all work is truly complete.</system-reminder>"
    fi

    if [ "$FORMAT" = "codex" ]; then
        # The gate's stderr is {"decision":"block","reason":...}; pull the reason.
        _reason=$(printf '%s' "$_emit_err" | jq -r '.reason // empty' 2>/dev/null)
        [ -n "$_reason" ] || _reason="$_emit_err"
        jq -cn --arg reason "${_text}
${_reason}" '{decision:"block",reason:$reason}'
        exit 0
    fi

    echo "$_text"
    exit 2
fi

# Gate allowed — clear cooldown and block count files.
rm -f "$COOLDOWN_FILE" "$BLOCK_COUNT_FILE"

# Clear any remaining gate state so the next session must re-satisfy from scratch.
gb gate clear decision 2>/dev/null || true

exit 0
