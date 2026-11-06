#!/bin/bash
# Gardenio agent entrypoint — thin wrapper that hands off to gb init.
#
# gb init owns the full agent pod lifecycle:
#   - One-time setup (git, SSH, repos, PVC, credentials, hooks)
#   - Restart loop (resume discovery, session bead tracking)
#   - Per-session lifecycle (startup bypass, prompt injection, monitoring)
#   - Signal handling + graceful shutdown
#
# This script only sets env vars that must be exported from bash before
# Go can read them, then execs gb init.

set -euo pipefail

# Platform version for version commands.
if [ -f /etc/platform-version ]; then
    export BEADS_PLATFORM_VERSION
    BEADS_PLATFORM_VERSION=$(cat /etc/platform-version)
fi

# Mux session ID = the pod name (for Slack deep links and Glass, which look a
# session up by the bead's pod_name). POD_NAME (downward API) first: the
# kernel caps $HOSTNAME at 63 bytes, so a 64-char pod name (every
# <project>-plan-gardener-for-<epic>-<suffix>) registered truncated and its
# Glass never went live.
export COOP_MUX_SESSION_ID="${POD_NAME:-${HOSTNAME:-$(hostname)}}"
export COOP_LOG_LEVEL="${COOP_LOG_LEVEL:-info}"

# Slack viewer session (gd-QpZeW4qxAU). The controller mounts the test user's
# Playwright storageState as an OPTIONAL Secret volume for ux-capable roles, so
# the directory exists but the file is absent until the Secret is provisioned.
# Export SLACK_VIEW_STORAGE_STATE only when the file is really there; with it
# unset, `gb slack view` says "Slack viewer session not provisioned".
SLACK_VIEW_SESSION_FILE="/var/run/gardenio/slack-view/storage-state.json"
if [ -s "$SLACK_VIEW_SESSION_FILE" ] && [ -z "${SLACK_VIEW_STORAGE_STATE:-}" ]; then
    export SLACK_VIEW_STORAGE_STATE="$SLACK_VIEW_SESSION_FILE"
fi

# Clean stale sentinel files from previous runs.
rm -f /tmp/standby_active /tmp/standby_expired /tmp/standby_done /tmp/standby_assigned

# Runtime coverage instrumentation (C2, ka-0000000508). When the controller
# opts this pod into coverage (GARDENIO_AGENT_COVERAGE=1, mirrored from the chart
# gate), it also sets GOCOVERDIR + LLVM_PROFILE_FILE to a per-pod subtree of the
# shared coverage PVC. Create BOTH parent dirs before exec: the Go runtime
# tolerates a missing GOCOVERDIR (it logs and skips), but the LLVM profiling
# runtime does NOT create parent dirs and silently drops the .profraw if they
# are absent. mkdir -p is idempotent and cheap; guard on the gate so
# non-coverage pods are untouched.
if [ "${GARDENIO_AGENT_COVERAGE:-}" = "1" ]; then
    [ -n "${GOCOVERDIR:-}" ] && mkdir -p "$GOCOVERDIR"
    # LLVM_PROFILE_FILE is a file pattern (…/coop-%p-%m.profraw); create its
    # directory, not the pattern itself.
    [ -n "${LLVM_PROFILE_FILE:-}" ] && mkdir -p "$(dirname "$LLVM_PROFILE_FILE")"
fi

# Build gb init flags from env.
GB_INIT_FLAGS=()
if [ "${BOAT_STANDBY:-}" = "true" ] && [ -n "${BOAT_AGENT_BEAD_ID:-}" ]; then
    GB_INIT_FLAGS+=("--standby")
fi
if grep -q "claudeless" 2>/dev/null <<<"${BOAT_COMMAND:-}"; then
    GB_INIT_FLAGS+=("--mock")
fi

echo "[entrypoint] Handing off to gb init"
exec gb init "${GB_INIT_FLAGS[@]}"
