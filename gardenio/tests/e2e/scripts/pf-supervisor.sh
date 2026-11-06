#!/usr/bin/env bash
# pf-supervisor.sh — keep a kubectl port-forward alive AND healthy in a
# respawn loop.
#
# Used by test-local-mode.sh (gd-FtiTtZyuj2) and run-shard.sh (ka-0000001942)
# because plain `setsid -f kubectl port-forward` is immune to SIGTERM cascades
# but still dies whenever kubectl itself exits — and kubectl exits routinely
# under load (backing pod restart, SPDY stream errors, transient API blips).
# When that happens mid-scenario, the next ~90s of harness + agent traffic to
# localhost:$PORT silently fails and every assertion downstream reports
# status=<unreachable>.
#
# This supervisor closes two distinct gaps:
#
#   1. kubectl EXITS — respawn it (with backoff) so the tunnel comes back.
#
#   2. kubectl is ALIVE but the tunnel is WEDGED — this is the failure mode
#      behind the cascading-module flaps (gd-VdqESzcc7a, ka-0000006320): after
#      a connectivity flap (node NoExecute taint + beads pod reschedule, SPDY
#      stream break, "lost connection to pod ...", "error forwarding port"),
#      `kubectl port-forward` frequently keeps its process alive — it logs the
#      error and goes on listening on the local port — but every connection it
#      accepts now fails. A `kill -0 $pid` liveness check stays TRUE forever,
#      so the old respawn loop NEVER healed it; the consumer's heal-waits in
#      run-shard.sh (the 10s ensure_port_forward budget and even the ~180s
#      post-flap retry from MR !4434) all expired against a permanently-wedged
#      kubectl, and 6-7 downstream modules cascade-failed on "beads
#      unreachable". We now ACTIVELY PROBE the tunnel end-to-end (an HTTP
#      health hit on the forwarded local port); if it stays unhealthy past a
#      short grace window while kubectl is still alive, we treat kubectl as
#      wedged, kill it, and let the loop respawn a fresh port-forward.
#
# It exits cleanly only when its sentinel file is removed. It's intended to be
# launched via `setsid -f` so it can survive coop shutdowns and lives in its
# own session.
#
# Usage:
#   pf-supervisor.sh <namespace> <svc> <local-port> <log-file> <sentinel-file> [target-port] [health-path]
#
# <target-port> is the service port to forward to (default 8080, the kbeads
# daemon). Pass 8090 to supervise the slack-bridge tunnel (ka-0000001942).
#
# <health-path> is the HTTP path probed on localhost:<local-port> to decide
# whether the tunnel is actually carrying traffic. When omitted it is derived
# from <target-port> (8080 -> /v1/health, 8090 -> /healthz). Pass the empty
# string "" or "-" to DISABLE active probing and fall back to liveness-only
# (kill-on-exit) supervision — e.g. for a non-HTTP service.
#
# Lifecycle:
#   - Touch <sentinel-file>, enter respawn loop.
#   - On each iteration: spawn kubectl port-forward in the background, then
#     poll once per second checking (a) the kubectl pid, (b) the sentinel,
#     and (c) — after a short establish grace — the tunnel health probe.
#   - When the sentinel disappears, kill kubectl and exit 0.
#   - When kubectl exits on its own, OR the tunnel is wedged-but-alive past the
#     unhealthy grace, kill kubectl and respawn after an exponential backoff
#     (reset to the floor as soon as a healthy probe is seen).

set -u

if [ "$#" -lt 5 ] || [ "$#" -gt 7 ]; then
  echo "usage: $0 <namespace> <svc> <local-port> <log-file> <sentinel-file> [target-port] [health-path]" >&2
  exit 2
fi

NAMESPACE="$1"
SVC="$2"
PORT="$3"
LOGF="$4"
SENTINEL="$5"
# Service-side target port. Defaults to 8080 (kbeads daemon) so existing
# 5-arg callers (test-local-mode.sh) are unaffected; run-shard.sh passes
# 8090 for the slack-bridge tunnel. ka-0000001942.
TARGET_PORT="${6:-8080}"

# Health probe path on localhost:$PORT. Derive a sensible default from the
# target port when not supplied (or supplied as the legacy 6-arg form). Passing
# "" or "-" disables active probing (liveness-only supervision).
if [ "$#" -ge 7 ]; then
  HEALTH_PATH="$7"
else
  case "$TARGET_PORT" in
    8080) HEALTH_PATH="/v1/health" ;;
    8090) HEALTH_PATH="/healthz" ;;
    *)    HEALTH_PATH="" ;;
  esac
fi
[ "$HEALTH_PATH" = "-" ] && HEALTH_PATH=""

# Active probing requires curl. If it's missing (or no health path is known),
# degrade gracefully to liveness-only supervision rather than killing a tunnel
# we can't actually assess.
PROBE_ENABLED=1
if [ -z "$HEALTH_PATH" ] || ! command -v curl >/dev/null 2>&1; then
  PROBE_ENABLED=0
fi

# Tunable timings (env-overridable for tests). All in seconds.
#   ESTABLISH_GRACE  — after spawning kubectl, wait this long before the first
#                      health probe, so a normally-establishing tunnel isn't
#                      mistaken for wedged.
#   UNHEALTHY_GRACE  — consecutive failed probes (1s apart) tolerated on a LIVE
#                      kubectl before declaring it wedged and respawning.
#   BACKOFF_FLOOR    — first respawn delay; doubles each consecutive respawn.
#   BACKOFF_CAP      — maximum respawn delay.
ESTABLISH_GRACE="${PF_ESTABLISH_GRACE:-5}"
UNHEALTHY_GRACE="${PF_UNHEALTHY_GRACE:-8}"
BACKOFF_FLOOR="${PF_BACKOFF_FLOOR:-1}"
BACKOFF_CAP="${PF_BACKOFF_CAP:-15}"

log() {
  printf '[%s] pf-supervisor: %s\n' "$(date -u +%FT%TZ)" "$*" >>"$LOGF"
}

probe_tunnel() {
  # 0 = healthy, non-zero = unhealthy. Cheap, short-timeout HTTP GET against
  # the forwarded local port.
  curl -sf --max-time 2 -o /dev/null "http://localhost:${PORT}${HEALTH_PATH}" 2>/dev/null
}

shutdown() {
  log "received signal, removing sentinel and exiting"
  rm -f "$SENTINEL"
  exit 0
}
trap shutdown TERM INT HUP

touch "$SENTINEL"
log "starting respawn loop for ${NAMESPACE}/svc/${SVC} ${PORT}:${TARGET_PORT} (probe=${PROBE_ENABLED} path='${HEALTH_PATH}' establish_grace=${ESTABLISH_GRACE}s unhealthy_grace=${UNHEALTHY_GRACE}s)"

backoff="$BACKOFF_FLOOR"

while [ -f "$SENTINEL" ]; do
  log "spawning kubectl port-forward"
  kubectl -n "$NAMESPACE" port-forward "svc/${SVC}" "${PORT}:${TARGET_PORT}" >>"$LOGF" 2>&1 &
  KPID=$!
  spawn_ts=$(date +%s)
  unhealthy_streak=0
  wedged=0
  # Inner watch loop: tick every 1s, exiting when kubectl dies, the sentinel
  # is removed, or the tunnel is detected wedged-but-alive. Checking the
  # sentinel here (rather than blocking on `wait`) makes shutdown deterministic.
  while kill -0 "$KPID" 2>/dev/null; do
    if [ ! -f "$SENTINEL" ]; then
      log "sentinel removed, killing kubectl pid $KPID"
      kill "$KPID" 2>/dev/null || true
      # Give it a moment to exit cleanly.
      for _ in 1 2 3; do
        kill -0 "$KPID" 2>/dev/null || break
        sleep 1
      done
      kill -KILL "$KPID" 2>/dev/null || true
      log "exiting cleanly"
      exit 0
    fi
    # Active wedged-tunnel detection (gd-VdqESzcc7a). Only after the establish
    # grace, and only while kubectl is alive.
    if [ "$PROBE_ENABLED" = 1 ]; then
      now_ts=$(date +%s)
      if [ "$((now_ts - spawn_ts))" -ge "$ESTABLISH_GRACE" ]; then
        if probe_tunnel; then
          if [ "$unhealthy_streak" -gt 0 ]; then
            log "tunnel healthy again after ${unhealthy_streak} failed probe(s)"
          fi
          unhealthy_streak=0
          # A tunnel that has proven healthy gets the backoff reset, so the
          # NEXT respawn (whenever it comes) starts from the floor again.
          backoff="$BACKOFF_FLOOR"
        else
          unhealthy_streak=$((unhealthy_streak + 1))
          if [ "$unhealthy_streak" -ge "$UNHEALTHY_GRACE" ]; then
            log "tunnel WEDGED: kubectl pid $KPID alive but health probe failed ${unhealthy_streak}x (>= ${UNHEALTHY_GRACE}); killing to force respawn"
            kill "$KPID" 2>/dev/null || true
            for _ in 1 2 3; do
              kill -0 "$KPID" 2>/dev/null || break
              sleep 1
            done
            kill -KILL "$KPID" 2>/dev/null || true
            wedged=1
            break
          fi
        fi
      fi
    fi
    sleep 1
  done

  # Reap whatever exit status kubectl had (whether it died on its own or we
  # just killed a wedged instance).
  wait "$KPID" 2>/dev/null
  rc=$?

  if [ ! -f "$SENTINEL" ]; then
    log "sentinel missing after watch loop, exiting"
    exit 0
  fi

  if [ "$wedged" = 1 ]; then
    log "respawning after wedged-tunnel kill, backing off ${backoff}s"
  else
    log "kubectl exited rc=$rc, backing off ${backoff}s before respawn"
  fi
  sleep "$backoff"
  # Exponential backoff so a genuinely-down environment isn't hammered. A
  # subsequently-healthy tunnel resets this to the floor (above).
  backoff=$((backoff * 2))
  [ "$backoff" -gt "$BACKOFF_CAP" ] && backoff="$BACKOFF_CAP"
done

log "sentinel missing at top of loop, exiting"
exit 0
