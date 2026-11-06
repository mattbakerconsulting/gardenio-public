#!/usr/bin/env bash
# test-pf-supervisor.sh — unit test for pf-supervisor.sh's heal logic.
#
# Covers the two failure modes the supervisor must heal (gd-VdqESzcc7a):
#   1. kubectl EXITS         -> supervisor respawns a fresh port-forward.
#   2. kubectl ALIVE, tunnel WEDGED (process stays up but stops carrying
#      traffic) -> supervisor actively probes, detects the wedge, kills the
#      stuck kubectl, and respawns — recovering the tunnel without operator
#      intervention. This is the case the old liveness-only loop could NEVER
#      heal.
#
# The test stubs `kubectl` with a fake that, on `port-forward`, runs a tiny
# python HTTP server on the local port whose health responses are driven by a
# control file we flip from the test. No cluster required.
#
# Run: bash gardenio/tests/e2e/scripts/test-pf-supervisor.sh
set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SUP="$SCRIPT_DIR/pf-supervisor.sh"

WORK="$(mktemp -d)"
PORT="${PF_TEST_PORT:-19987}"
LOGF="$WORK/sup.log"
SENTINEL="$WORK/sentinel"
HEALTH_CTL="$WORK/health_state"   # contains "ok" or "wedged"
PIDLOG="$WORK/kubectl-pids.log"   # one line per fake-kubectl pid spawned

PASS=0
FAIL=0
ok()   { echo "  PASS: $*"; PASS=$((PASS + 1)); }
bad()  { echo "  FAIL: $*"; FAIL=$((FAIL + 1)); }

cleanup() {
  rm -f "$SENTINEL" 2>/dev/null || true
  [ -n "${SUP_PID:-}" ] && kill "$SUP_PID" 2>/dev/null || true
  # Kill any stray fake kubectl http servers.
  if [ -f "$PIDLOG" ]; then
    while read -r p; do kill "$p" 2>/dev/null || true; done < "$PIDLOG"
  fi
  pkill -f "pf_test_server.py" 2>/dev/null || true
  rm -rf "$WORK" 2>/dev/null || true
}
trap cleanup EXIT

command -v python3 >/dev/null 2>&1 || { echo "SKIP: python3 required"; exit 0; }

# ── Fake health server: serves 200 when control file says "ok", else 503.
cat > "$WORK/pf_test_server.py" <<PYEOF
import http.server, os, sys
port = int(sys.argv[1]); ctl = sys.argv[2]
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        state = "ok"
        try:
            with open(ctl) as f: state = f.read().strip()
        except OSError: pass
        code = 200 if state == "ok" else 503
        self.send_response(code); self.end_headers(); self.wfile.write(b"x")
    def log_message(self, *a): pass
http.server.HTTPServer(("127.0.0.1", port), H).serve_forever()
PYEOF

# ── Fake kubectl: only handles `port-forward`. Records its own pid, then runs
#    the health server in the foreground so killing this process frees the port.
FAKEBIN="$WORK/bin"
mkdir -p "$FAKEBIN"
cat > "$FAKEBIN/kubectl" <<KEOF
#!/usr/bin/env bash
# args: -n NS port-forward svc/NAME LPORT:TPORT  (we only care it's port-forward)
for a in "\$@"; do [ "\$a" = "port-forward" ] && pf=1; done
if [ "\${pf:-0}" = 1 ]; then
  echo "\$\$" >> "$PIDLOG"
  exec python3 "$WORK/pf_test_server.py" "$PORT" "$HEALTH_CTL"
fi
exit 0
KEOF
chmod +x "$FAKEBIN/kubectl"
export PATH="$FAKEBIN:$PATH"

probe() { curl -sf --max-time 2 -o /dev/null "http://localhost:${PORT}/v1/health" 2>/dev/null; }
wait_for() { # <desc> <timeout-s> <cmd...>
  local desc="$1" t="$2"; shift 2
  local i=0
  while [ "$i" -lt "$t" ]; do "$@" && return 0; sleep 1; i=$((i + 1)); done
  return 1
}
count_pids() { wc -l < "$PIDLOG" 2>/dev/null | tr -d ' '; }

echo "ok" > "$HEALTH_CTL"
: > "$PIDLOG"

# Fast timings so the test runs in seconds, not the production defaults.
export PF_ESTABLISH_GRACE=2 PF_UNHEALTHY_GRACE=3 PF_BACKOFF_FLOOR=1 PF_BACKOFF_CAP=2

echo "==> launching supervisor"
bash "$SUP" testns testsvc "$PORT" "$LOGF" "$SENTINEL" 8080 /v1/health &
SUP_PID=$!

if wait_for "tunnel up" 15 probe; then ok "tunnel established"; else
  bad "tunnel never came up"; cat "$LOGF" 2>/dev/null; exit 1
fi
pids_after_start=$(count_pids)

# ── Case 1: kubectl exits on its own -> respawn.
echo "==> Case 1: kill fake kubectl, expect respawn"
kill "$(tail -n1 "$PIDLOG")" 2>/dev/null || true
sleep 1
if wait_for "respawn after exit" 15 probe; then ok "tunnel recovered after kubectl exit"; else
  bad "tunnel did NOT recover after kubectl exit"; fi
if [ "$(count_pids)" -gt "$pids_after_start" ]; then ok "supervisor spawned a fresh kubectl (exit case)"; else
  bad "no respawn observed after exit"; fi

# ── Case 2: wedged-but-alive tunnel -> supervisor must kill+respawn.
echo "==> Case 2: wedge the tunnel (kubectl stays alive), expect heal"
pids_before_wedge=$(count_pids)
echo "wedged" > "$HEALTH_CTL"     # server now 503s; kubectl process unaffected
# Confirm it's actually unhealthy now.
if probe; then bad "probe still healthy right after wedge (test bug)"; else ok "tunnel observed unhealthy after wedge"; fi
# Now un-wedge AFTER the supervisor should have killed the stuck server. The
# respawned kubectl re-reads the control file; flip back so the new server is
# healthy.
sleep 6                            # > establish_grace + unhealthy_grace
echo "ok" > "$HEALTH_CTL"
if wait_for "heal after wedge" 20 probe; then ok "tunnel HEALED after wedge (kill+respawn)"; else
  bad "tunnel did NOT heal after wedge"; cat "$LOGF" 2>/dev/null; fi
if [ "$(count_pids)" -gt "$pids_before_wedge" ]; then ok "supervisor respawned a fresh kubectl (wedge case)"; else
  bad "no respawn observed after wedge — heal logic did not fire"; fi
if grep -q "tunnel WEDGED" "$LOGF"; then ok "supervisor logged WEDGED detection"; else
  bad "no WEDGED log line"; fi

# ── Case 3: clean shutdown via sentinel removal.
echo "==> Case 3: remove sentinel, expect clean exit"
rm -f "$SENTINEL"
if wait_for "supervisor exit" 10 sh -c '! kill -0 '"$SUP_PID"' 2>/dev/null'; then
  ok "supervisor exited after sentinel removal"; else
  bad "supervisor still running after sentinel removal"; fi

echo ""
echo "==== pf-supervisor test: $PASS passed, $FAIL failed ===="
[ "$FAIL" -eq 0 ]
