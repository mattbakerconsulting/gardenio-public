#!/usr/bin/env bash
# run-e2e-seeded.sh — orchestrator for the Phase 3 task 3-6 (ka-0000006435)
# closing end-to-end verification: seed a THROWAWAY persisting beads daemon with
# a multi-member thread, boot the REAL beads-viewer over it (dev-auth bypass),
# and drive the LIVE compose -> ?since= poll -> audit-PASS chain.
#
# Boots, in order:
#   1. e2e-seeded-daemon.mjs (persisting) on :18080
#   2. the beads-viewer binary (VIEWER_BIN) on :8095 with dev-auth bypass
#   3. drive-threads-e2e.mjs  (the live compose/poll/audit driver)
#
# Tears everything down on exit. No Postgres, no Keycloak, no port-forward, no
# prod contact. Set VIEWER_BIN to a prebuilt binary (much faster than go run).
set -euo pipefail

HARNESS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO_ROOT="$(cd "$HARNESS_DIR/../../.." && pwd)"
VIEWER_DIR="$REPO_ROOT/gardenio/controller"

MOCK_DAEMON_PORT="${MOCK_DAEMON_PORT:-18080}"
VIEWER_PORT="${VIEWER_PORT:-8095}"
OUT_DIR="${OUT_DIR:-test-results/threads}"

DAEMON_URL="http://localhost:${MOCK_DAEMON_PORT}"
VIEWER_URL="http://localhost:${VIEWER_PORT}"

log() { echo "[run-e2e-seeded] $*" >&2; }

PIDS=()
cleanup() {
  log "tearing down (pids: ${PIDS[*]:-none})"
  for pid in "${PIDS[@]:-}"; do
    [ -n "$pid" ] && kill "$pid" 2>/dev/null || true
  done
  wait 2>/dev/null || true
}
trap cleanup EXIT INT TERM

# --- 1. persisting seeded daemon -------------------------------------------
log "starting persisting seeded daemon on :${MOCK_DAEMON_PORT}"
( cd "$HARNESS_DIR" && MOCK_DAEMON_PORT="$MOCK_DAEMON_PORT" node tools/e2e-seeded-daemon.mjs ) &
PIDS+=("$!")

for i in $(seq 1 50); do
  if curl -fsS "${DAEMON_URL}/v1/health" >/dev/null 2>&1; then log "seeded daemon healthy"; break; fi
  sleep 0.2
  [ "$i" = 50 ] && { log "seeded daemon never came up"; exit 1; }
done

# --- 2. beads-viewer -------------------------------------------------------
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
    log "beads-viewer healthy"; break
  fi
  sleep 0.5
  [ "$i" = 120 ] && { log "beads-viewer never came up"; exit 1; }
done

# --- 3. drive the live compose/poll/audit chain ----------------------------
log "driving live compose -> poll -> audit"
(
  cd "$HARNESS_DIR" &&
  BEADS_VIEWER_URL="$VIEWER_URL" \
  OUT_DIR="$OUT_DIR" \
  node tools/drive-threads-e2e.mjs
)
rc=$?
log "driver exit rc=$rc -> ${HARNESS_DIR}/${OUT_DIR}"
exit $rc
