#!/bin/bash
# drain-queue.sh — Drain the inject queue and output content to Claude context.
#
# Called by PostToolUse hook. Reads each JSONL entry's "content" field,
# outputs it to stdout (which Claude Code captures as hook output), then
# removes the queue file.
#
# Flags:
#   --quiet  Suppress "no queue file" messages (used by PostToolUse)
#
# Always exits 0 so hook failures don't block Claude.

set -euo pipefail

START_NS=$(date +%s%N)

# Report this run's wall-clock duration to gardenio_tool_harness_overhead_seconds
# (gd-RpflV68KX1, P3) on every exit path. One O_APPEND write of a single line to
# the per-pod spool that `gb init` flushes: no fork, no lock, no network. (A
# backgrounded `gb` here would be orphaned to PID 1, which does not reap, and
# leave a zombie per tool call.) The path must match
# harnessOverheadSpoolPath in cmd/gb/hook_harness_metrics.go. Failure to write
# is ignored so it can never trip set -e.
record_overhead() {
    local dur_ms=$(( ($(date +%s%N) - START_NS) / 1000000 ))
    { printf 'drain_queue %d.%03d\n' $((dur_ms / 1000)) $((dur_ms % 1000)) \
        >> "${GARDENIO_HARNESS_OVERHEAD_SPOOL:-/tmp/gardenio-harness-overhead.spool}"; } 2>/dev/null || true
}
trap record_overhead EXIT

QUEUE_FILE="/tmp/inject-queue.jsonl"
QUIET=false

for arg in "$@"; do
    case "${arg}" in
        --quiet) QUIET=true ;;
    esac
done

if [ ! -f "${QUEUE_FILE}" ]; then
    if [ "${QUIET}" = "false" ]; then
        echo "[drain-queue] No queue file" >&2
    fi
    exit 0
fi

# Read and output each entry's content.
while IFS= read -r line; do
    [ -z "${line}" ] && continue
    content=$(echo "${line}" | jq -r '.content // empty' 2>/dev/null) || continue
    if [ -n "${content}" ]; then
        echo "${content}"
    fi
done < "${QUEUE_FILE}"

# Remove the queue file after draining.
rm -f "${QUEUE_FILE}"

exit 0
