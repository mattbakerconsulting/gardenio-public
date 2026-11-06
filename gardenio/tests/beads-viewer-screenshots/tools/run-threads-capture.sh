#!/usr/bin/env bash
# run-threads-capture.sh — one-shot orchestrator for the hermetic /threads
# visual capture matrix (task 3-3, ka-0000004063).
#
# Boots, in order:
#   1. the hermetic mock beads daemon (threads-mock-daemon.mjs) on :8080,
#   2. the REAL beads-viewer binary (go run ./cmd/beads-viewer) on :8095 with
#      BEADS_VIEWER_DEV_AUTH_BYPASS=1 pointed at the mock daemon,
#   3. the Playwright capture driver (capture-threads.mjs).
#
# It tears everything down on exit. No Postgres, no Keycloak, no port-forward.
#
# Inside an agent pod a K8s SA token is mounted, which trips the viewer's
# prod-env guard; we set BEADS_VIEWER_DEV_AUTH_BYPASS_ACK_AGENT_POD=1 (which
# forces the listen addr to loopback) so the capture still runs. Outside a pod
# the ACK is harmless.
#
# Usage:
#   gardenio/tests/beads-viewer-screenshots/tools/run-threads-capture.sh
#
# Env knobs:
#   MOCK_DAEMON_PORT (default 8080)   VIEWER_PORT (default 8095)
#   OUT_DIR (default test-results/threads, relative to the harness dir)
set -euo pipefail

HARNESS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO_ROOT="$(cd "$HARNESS_DIR/../../.." && pwd)"
VIEWER_DIR="$REPO_ROOT/gardenio/controller"

# Default the mock daemon to :18080, NOT :8080 — an agent pod usually already
# has a real beads daemon (or a port-forward) on :8080, and we must never serve
# fixtures over, or read fixtures from, that. :18080 is the harness's own port.
MOCK_DAEMON_PORT="${MOCK_DAEMON_PORT:-18080}"
VIEWER_PORT="${VIEWER_PORT:-8095}"
OUT_DIR="${OUT_DIR:-test-results/threads}"

DAEMON_URL="http://localhost:${MOCK_DAEMON_PORT}"
VIEWER_URL="http://localhost:${VIEWER_PORT}"

log() { echo "[run-threads-capture] $*" >&2; }

PIDS=()
cleanup() {
  log "tearing down (pids: ${PIDS[*]:-none})"
  for pid in "${PIDS[@]:-}"; do
    [ -n "$pid" ] && kill "$pid" 2>/dev/null || true
  done
  wait 2>/dev/null || true
}
trap cleanup EXIT INT TERM

# --- 1. mock daemon --------------------------------------------------------
log "starting mock daemon on :${MOCK_DAEMON_PORT}"
( cd "$HARNESS_DIR" && MOCK_DAEMON_PORT="$MOCK_DAEMON_PORT" node tools/threads-mock-daemon.mjs ) &
PIDS+=("$!")

for i in $(seq 1 50); do
  if curl -fsS "${DAEMON_URL}/v1/health" >/dev/null 2>&1; then
    log "mock daemon healthy"
    break
  fi
  sleep 0.2
  [ "$i" = 50 ] && { log "mock daemon never came up"; exit 1; }
done

# --- 2. beads-viewer -------------------------------------------------------
# Prefer a prebuilt binary if VIEWER_BIN points at one (much faster than
# `go run`, which recompiles every invocation); otherwise `go run` from source.
log "starting beads-viewer on :${VIEWER_PORT} (dev auth bypass)"
if [ -n "${VIEWER_BIN:-}" ] && [ -x "${VIEWER_BIN}" ]; then
  log "using prebuilt viewer binary ${VIEWER_BIN}"
  (
    BEADS_VIEWER_DEV_AUTH_BYPASS=1 \
    BEADS_VIEWER_DEV_AUTH_BYPASS_ACK_AGENT_POD=1 \
    BEADS_HTTP_ADDR="$DAEMON_URL" \
    LISTEN_ADDR=":${VIEWER_PORT}" \
    LOG_LEVEL=warn \
    "${VIEWER_BIN}"
  ) &
else
  (
    cd "$VIEWER_DIR" &&
    BEADS_VIEWER_DEV_AUTH_BYPASS=1 \
    BEADS_VIEWER_DEV_AUTH_BYPASS_ACK_AGENT_POD=1 \
    BEADS_HTTP_ADDR="$DAEMON_URL" \
    LISTEN_ADDR=":${VIEWER_PORT}" \
    LOG_LEVEL=warn \
    go run ./cmd/beads-viewer/
  ) &
fi
PIDS+=("$!")

for i in $(seq 1 120); do
  if curl -fsS "${VIEWER_URL}/healthz" >/dev/null 2>&1 || curl -fsS "${VIEWER_URL}/threads" >/dev/null 2>&1; then
    log "beads-viewer healthy"
    break
  fi
  sleep 0.5
  [ "$i" = 120 ] && { log "beads-viewer never came up"; exit 1; }
done

# --- 3. capture ------------------------------------------------------------
log "running capture-threads.mjs -> ${OUT_DIR}"
(
  cd "$HARNESS_DIR" &&
  BEADS_VIEWER_URL="$VIEWER_URL" \
  MOCK_DAEMON_URL="$DAEMON_URL" \
  OUT_DIR="$OUT_DIR" \
  node tools/capture-threads.mjs
)

log "capture complete -> ${HARNESS_DIR}/${OUT_DIR}"
