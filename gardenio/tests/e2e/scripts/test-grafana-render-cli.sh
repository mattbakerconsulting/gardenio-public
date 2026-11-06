#!/usr/bin/env bash
# test-grafana-render-cli.sh — `gb grafana render` CLI E2E (task ka-0000007821,
# coverage-backfill epic ka-0000000958).
#
# Closes the gap the W2c dashboard-render loop (commits 42257ff4 / dfec4604)
# left open: `gb grafana render` (cmd/gb/grafana.go) — the agent-facing
# author→render→look→edit affordance that renders a dashboard (addressed by
# type=dashboard bead OR raw Grafana uid) to a local PNG, detects the
# renderer-missing placeholder, and fails loudly rather than writing a broken
# file — shipped with strong UNIT coverage (grafana_test.go) but NO end-to-end
# assertion driving the REAL `gb` binary through a real HTTP round-trip. The
# only live grafana e2e that existed — test-grafana-iframe.sh (X-Frame-Options
# framing) and test-grafana-bridge.sh (dashboard SYNC / conflict-policy) —
# touch neither the render command nor the bead→uid resolution it does.
#
# ── Why a hermetic stub rig, not a live-cluster port-forward ──────────────────
#
# Two hard constraints shape this module — identical to test-grafana-bridge:
#
#   1. The Grafana image-renderer sidecar is feature-flagged OFF in the
#      per-instance ephemeral e2e namespaces (grafana.enabled=false there) and
#      even on the staging singleton /render depends on the renderer plugin
#      being installed. So there is no dependable live /render endpoint to drive
#      in the shard fan-out, and the whole point of the command — the
#      placeholder / 404 failure paths — cannot be exercised against a healthy
#      renderer at all.
#   2. The logic under test is all in the `gb` binary (resolveDashboardUID's
#      bead→dashboard_uid dereference, buildRenderURL's d/ vs d-solo routing,
#      fetchRender's placeholder + 404 detection). Driving it needs only a beads
#      daemon that answers GET /v1/beads/{id} and a Grafana that answers
#      /render/... — both trivially stubbable.
#
# So this module stands up two throwaway HTTP stubs (Python stdlib, always
# present in the e2e shard — e2e-system-deps.lock ships python3) and drives the
# REAL `gb` binary against them:
#
#   • a STUB DAEMON serving GET /v1/beads/{id} for one type=dashboard bead
#     (dashboard_uid=<uid>) and one type=task bead (the not-a-dashboard error).
#   • a STUB RENDERER with three modes on three ports: a REAL renderer that
#     honours &width/&height (a large geometry-correct PNG), a PLACEHOLDER that
#     returns a tiny fixed card ignoring geometry (the renderer-missing 200),
#     and a 404 (renderer sidecar absent).
#
# It is hermetic: ZERO pods, ZERO shared-daemon writes, ZERO live Grafana — so
# it is green today and is a HARD gate. It does NOT use the shard's beads
# port-forward tunnel at all.
#
# ── Legs (each a hard sub-gate; any FAIL reddens the shard) ───────────────────
#
#   1  bead→render     `gb grafana render <dashboard-bead>` dereferences the
#                      bead's dashboard_uid, GETs the REAL renderer's
#                      /render/d/<uid>, writes a real PNG (>placeholder size),
#                      and reports bead + uid + bytes in --json. [acceptance:
#                      "resolves the bead uid, writes a real PNG"]
#   2  panel-route     `--panel N` switches to the d-solo single-panel route
#                      (/render/d-solo/<uid>?panelId=N) and still writes a real
#                      PNG. [acceptance: "(and --uid --panel)"]
#   3  raw-uid         `--uid` forces raw-uid interpretation (no bead lookup):
#                      the arg is rendered verbatim as the Grafana uid.
#   4  placeholder     A renderer returning the <20KB fixed-size placeholder on
#                      HTTP 200 is detected: `gb` EXITS NON-ZERO, prints the
#                      actionable "placeholder image … Image Renderer plugin"
#                      error, and writes NO file. [acceptance: "fails loudly …
#                      when the renderer returns the <20KB placeholder"]
#   5  render-404      A renderer 404 (sidecar not deployed) EXITS NON-ZERO with
#                      the "image-renderer sidecar may not be deployed"
#                      actionable error and writes NO file. [acceptance: "… / 404"]
#   6  not-a-dashboard `gb grafana render <task-bead>` (a bead that exists but is
#                      type!=dashboard) is a HARD error naming the type and
#                      suggesting --uid — never a silent raw-uid render of the
#                      bead id.
#
# ── Skip-clean contract ──────────────────────────────────────────────────────
#
# The `gb` binary + python3 are present in every e2e shard container (the shard
# `use:` list includes build-gb; e2e-system-deps.lock ships python3). If either
# is somehow absent the module SKIPS CLEANLY (exit 0) with a LOUD banner — an
# absent binary must never red the suite — mirroring the grafana-bridge /
# gitlab-dark-provenance skip_all("loud") contract. When present, every leg is a
# HARD assertion.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-grafana-render-cli.sh

set -uo pipefail

# ── Config ────────────────────────────────────────────────────────
CONTROLLER_DIR="${CONTROLLER_DIR:-gardenio/controller}"
RUN_ID="${RUN_ID:-$$}"
WORKDIR="$(mktemp -d "/tmp/grafana-render-cli.${RUN_ID}.XXXXXX")"
GB_BIN=""
GB_TEMP=""          # non-empty only when we built a throwaway binary to clean up
DASHBOARD_BEAD="gd-GrafRenderE2E"   # synthetic; served only by our stub daemon
TASK_BEAD="gd-GrafRenderNotDash"    # synthetic type=task bead (leg 6)
DASHBOARD_UID="graf-render-e2e-uid"
PIDS=()

PASS=0
FAIL=0
ERRORS=()

# ── Helpers ───────────────────────────────────────────────────────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

pass() { green "  ✓ $1"; PASS=$((PASS + 1)); }
fail() {
  red "  ✗ $1"
  [ -n "${2:-}" ] && dim "    $2"
  FAIL=$((FAIL + 1))
  ERRORS+=("$1: ${2:-}")
}

# skip_all exits 0 (a missing binary must NOT red the suite) with a LOUD banner
# so a skip is never silently read as a pass.
skip_all() {
  local why="$1"
  red "════════════════════════════════════════════════════"
  red " SKIP grafana-render-cli (LOUD): $why"
  red " This run did NOT assert the 'gb grafana render' CLI (bead→uid resolution,"
  red " d/ vs d-solo routing, and the placeholder / 404 renderer-missing failure"
  red " paths). A green suite here proves NOTHING about that command. The e2e"
  red " shard is built with the gb binary (build-gb task) + python3"
  red " (e2e-system-deps.lock) — if this fires in CI, that wiring regressed."
  red "════════════════════════════════════════════════════"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

cleanup() {
  for p in "${PIDS[@]:-}"; do
    [ -n "$p" ] && kill "$p" 2>/dev/null || true
  done
  # Only remove a gb we built ourselves; never a PATH-installed one.
  [ -n "$GB_TEMP" ] && rm -f "$GB_TEMP" 2>/dev/null || true
  rm -rf "$WORKDIR" 2>/dev/null || true
}
trap cleanup EXIT

# free_port asks the kernel for an unused TCP port by binding :0 and reading
# it back — race-free enough for a single-process test rig (we bind the real
# server immediately after) and avoids hard-coding ports that could collide
# when shards co-locate.
free_port() {
  python3 - <<'PY'
import socket
s = socket.socket()
s.bind(('127.0.0.1', 0))
print(s.getsockname()[1])
s.close()
PY
}

# wait_http polls a URL until it answers (any HTTP status) or times out.
wait_http() {
  local url="$1" tries="${2:-50}"
  for _ in $(seq 1 "$tries"); do
    if curl -s -o /dev/null --max-time 1 "$url" 2>/dev/null; then
      return 0
    fi
    sleep 0.1
  done
  return 1
}

echo "════════════════════════════════════════════════════"
echo " gb grafana render CLI E2E (ka-0000007821)"
echo " bead→render · panel-route · raw-uid · placeholder · render-404 · not-a-dashboard"
echo "════════════════════════════════════════════════════"

# ── 1. Resolve the gb binary ────────────────────────────────────────
# Prefer a binary pre-built + installed onto PATH.
# Fall back to an in-shell workspace build for local dev. Only when BOTH the
# pre-built binary is absent AND no Go toolchain/source is available do we skip.
if command -v gb >/dev/null 2>&1; then
  GB_BIN="$(command -v gb)"
  pass "using pre-built gb ($GB_BIN)"
else
  command -v go >/dev/null 2>&1 \
    || skip_all "no gb on PATH and no go toolchain to build it"
  [ -d "$CONTROLLER_DIR/cmd/gb" ] \
    || skip_all "no gb on PATH and cmd/gb missing under $CONTROLLER_DIR"
  command -v python3 >/dev/null 2>&1 \
    || skip_all "python3 absent — cannot stand up the stub daemon/renderer rig"
  blue "[build] cmd/gb (from source, workspace mode)"
  GB_TEMP="$(mktemp "/tmp/gb.${RUN_ID}.XXXXXX")"
  GB_BIN="$GB_TEMP"
  BUILD_LOG="/tmp/gb-build-${RUN_ID}.log"
  # cmd/gb imports the local clicreds module via go.work (replace => ../clicreds),
  # so it MUST build in workspace mode (GOWORK on) — a GOWORK=off build FATALs on
  # the missing replacement. Pin GOWORK to the repo-root go.work explicitly.
  if ( GOWORK="$(pwd)/go.work" CGO_ENABLED=0 go build -o "$GB_BIN" ./gardenio/controller/cmd/gb/ ) >"$BUILD_LOG" 2>&1; then
    pass "cmd/gb built from source"
  else
    fail "build gb" "go build failed: $(tail -8 "$BUILD_LOG")"
    blue "════════════════════════════════════════════════════"
    red " ✗ 1/1 assertions failed (gb build)"
    for err in "${ERRORS[@]}"; do red "   - $err"; done
    blue "════════════════════════════════════════════════════"
    exit 1
  fi
fi

command -v python3 >/dev/null 2>&1 \
  || skip_all "python3 absent — cannot stand up the stub daemon/renderer rig"
echo

# ── 2. Stand up the stub rig ────────────────────────────────────────
# Write the stub server program once; it dispatches on argv[1] (role) so a
# single file backs the daemon and all three renderer modes.
STUB_PY="$WORKDIR/stubs.py"
cat > "$STUB_PY" <<'PY'
# Throwaway HTTP stubs for the gb-grafana-render E2E. One file, four roles
# (argv[1]): a beads daemon and three Grafana image-renderer behaviours.
#
# The renderer builds a minimal-but-valid PNG whose IHDR width/height match the
# request geometry (real) or a tiny fixed card that ignores it (placeholder) —
# exactly the two shapes fetchRender's missingRendererReason discriminates on.
import http.server, json, os, struct, sys, zlib, urllib.parse as up

def png(w, h, noise=False):
    # A minimal-but-valid PNG. IHDR carries the true w/h (what fetchRender's
    # missingRendererReason keys on); each scanline is a 0x00 filter byte + RGB
    # pixels. When noise=True the pixels are incompressible (os.urandom) so a
    # genuine full-size render lands well over the 20KB placeholder threshold —
    # exercising gb's real-render acceptance path honestly; when False the flat
    # pixels crush to a few KB, standing in for the tiny fixed placeholder card.
    sig = b'\x89PNG\r\n\x1a\n'
    def chunk(t, d):
        return struct.pack('>I', len(d)) + t + d + struct.pack('>I', zlib.crc32(t + d) & 0xffffffff)
    ihdr = struct.pack('>IIBBBBB', w, h, 8, 2, 0, 0, 0)   # 8-bit truecolour
    row = (lambda: b'\x00' + os.urandom(w * 3)) if noise else (lambda: b'\x00' + b'\x00' * (w * 3))
    raw = b''.join(row() for _ in range(h))               # filter byte + pixels
    return sig + chunk(b'IHDR', ihdr) + chunk(b'IDAT', zlib.compress(raw)) + chunk(b'IEND', b'')

DASH_BEAD = 'gd-GrafRenderE2E'
TASK_BEAD = 'gd-GrafRenderNotDash'
DASH_UID  = 'graf-render-e2e-uid'

class Daemon(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def _json(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def do_GET(self):
        path = up.urlparse(self.path).path
        if path == '/v1/beads/' + DASH_BEAD:
            return self._json(200, {"id": DASH_BEAD, "type": "dashboard", "status": "open",
                                    "title": "E2E dashboard", "fields": {"dashboard_uid": DASH_UID}})
        if path == '/v1/beads/' + TASK_BEAD:
            return self._json(200, {"id": TASK_BEAD, "type": "task", "status": "open",
                                    "title": "not a dashboard", "fields": {}})
        # Any other bead id → 404 so a raw-uid arg falls through to raw-uid mode.
        return self._json(404, {"message": "not found"})

def _serve_png(handler, body):
    handler.send_response(200)
    handler.send_header('Content-Type', 'image/png')
    handler.send_header('Content-Length', str(len(body)))
    handler.end_headers()
    handler.wfile.write(body)

class RealRenderer(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_GET(self):
        q = up.parse_qs(up.urlparse(self.path).query)
        w = int(q.get('width',  ['1000'])[0])
        h = int(q.get('height', ['500'])[0])
        _serve_png(self, png(w, h, noise=True))   # geometry-correct + large → a genuine render

class PlaceholderRenderer(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_GET(self):
        _serve_png(self, png(60, 40))  # tiny fixed card, ignores geometry

class Renderer404(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_GET(self):
        body = b'{"message":"Not found"}'
        self.send_response(404)
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

ROLES = {'daemon': Daemon, 'real': RealRenderer,
         'placeholder': PlaceholderRenderer, 'notfound': Renderer404}
role, port = sys.argv[1], int(sys.argv[2])
http.server.HTTPServer(('127.0.0.1', port), ROLES[role]).serve_forever()
PY

DAEMON_PORT="$(free_port)"
REAL_PORT="$(free_port)"
PLACEHOLDER_PORT="$(free_port)"
NOTFOUND_PORT="$(free_port)"

python3 "$STUB_PY" daemon      "$DAEMON_PORT"      & PIDS+=($!)
python3 "$STUB_PY" real        "$REAL_PORT"        & PIDS+=($!)
python3 "$STUB_PY" placeholder "$PLACEHOLDER_PORT" & PIDS+=($!)
python3 "$STUB_PY" notfound    "$NOTFOUND_PORT"    & PIDS+=($!)

DAEMON_URL="http://127.0.0.1:${DAEMON_PORT}"
REAL_URL="http://127.0.0.1:${REAL_PORT}"
PLACEHOLDER_URL="http://127.0.0.1:${PLACEHOLDER_PORT}"
NOTFOUND_URL="http://127.0.0.1:${NOTFOUND_PORT}"

rig_ok=1
wait_http "${DAEMON_URL}/v1/beads/${DASHBOARD_BEAD}" || rig_ok=0
wait_http "${REAL_URL}/render/d/probe"               || rig_ok=0
wait_http "${PLACEHOLDER_URL}/render/d/probe"        || rig_ok=0
wait_http "${NOTFOUND_URL}/render/d/probe"           || rig_ok=0
if [ "$rig_ok" != 1 ]; then
  skip_all "stub daemon/renderer rig failed to come up (python3 http.server unavailable?)"
fi
pass "stub rig up (daemon:$DAEMON_PORT real:$REAL_PORT placeholder:$PLACEHOLDER_PORT notfound:$NOTFOUND_PORT)"
echo

# gb needs an operator identity to attribute daemon calls (the daemon here is
# auth-disabled, so attribution falls through to X-Operated-By). Without it the
# GetBead round-trip can 400 "operator could not be resolved" (ka-0000002601).
export BEADS_OPERATED_BY="${BEADS_OPERATED_BY:-oidc:grafana-render-e2e@example.com}"
# Belt-and-suspenders: make sure no ambient Grafana env leaks into the render
# base resolution — every leg passes --grafana-url explicitly, which wins, but
# clear these so a misconfigured shard can't shadow the stub.
unset GRAFANA_URL GRAFANA_BASE_URL GRAFANA_TOKEN GRAFANA_AUTH_PROXY_HEADER 2>/dev/null || true

# gb_render drives one render. Args: <label> <expect: ok|err> then gb args.
# Captures stdout(JSON when --json)/stderr/rc; on ok requires rc=0, on err rc!=0.
#
# The daemon override MUST reach gb three ways, in the order gb resolves them
# (main.go defaultHTTPURL): the explicit --http-url flag wins over everything, but
# run-shard.sh exports BEADS_HTTP_URL=http://localhost:19090 (the port-forwarded
# REAL staging daemon) — and defaultHTTPURL prefers BEADS_HTTP_URL over
# BEADS_HTTP_ADDR. So setting only BEADS_HTTP_ADDR here is silently shadowed: gb
# queries the real daemon, which 404s the synthetic beads, and every bead-deref
# leg falls through to raw-uid mode (rendering gd-GrafRenderE2E verbatim instead
# of graf-render-e2e-uid) — the exact "bead deref regressed" failure this module
# hit on releases 2026.217.13/.14/.15 (gd-5D5XweW2sL). Pass --http-url (the
# documented user override that wins) and pin BEADS_HTTP_URL/ADDR to the stub as
# belt-and-suspenders so no shard env can shadow it.
run_gb() {
  BEADS_HTTP_URL="$DAEMON_URL" BEADS_HTTP_ADDR="$DAEMON_URL" \
    "$GB_BIN" grafana render --http-url "$DAEMON_URL" "$@"
}

# ── Leg 1: bead→render (resolve dashboard_uid, real PNG, --json shape) ──
blue "[leg bead→render] gb grafana render $DASHBOARD_BEAD → real PNG"
OUT1="$WORKDIR/dash.png"
J1="$WORKDIR/dash.json"
if run_gb "$DASHBOARD_BEAD" --grafana-url "$REAL_URL" --out "$OUT1" \
     --width 1000 --height 500 --json >"$J1" 2>"$WORKDIR/dash.err"; then
  got_uid="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("uid",""))' "$J1" 2>/dev/null || true)"
  got_bead="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("bead",""))' "$J1" 2>/dev/null || true)"
  got_bytes="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("bytes",0))' "$J1" 2>/dev/null || echo 0)"
  if [ "$got_uid" != "$DASHBOARD_UID" ]; then
    fail "leg bead→render uid" "resolved uid=$got_uid want $DASHBOARD_UID (bead deref regressed)"
  elif [ "$got_bead" != "$DASHBOARD_BEAD" ]; then
    fail "leg bead→render bead" "reported bead=$got_bead want $DASHBOARD_BEAD"
  elif [ ! -s "$OUT1" ]; then
    fail "leg bead→render file" "no PNG written to $OUT1"
  elif ! head -c 8 "$OUT1" | grep -q 'PNG'; then
    fail "leg bead→render magic" "output is not a PNG"
  elif [ "$got_bytes" -le 20480 ]; then
    fail "leg bead→render size" "PNG is $got_bytes bytes (<=20KB placeholder threshold) — not a real render"
  else
    # Assert the intrinsic PNG geometry matches the requested 1000x500 — the
    # same signal fetchRender's missingRendererReason uses to tell a real render
    # from the fixed-size placeholder card. Read w/h from the IHDR (bytes 16-24).
    dims="$(python3 - "$OUT1" <<'PY'
import struct, sys
b = open(sys.argv[1], 'rb').read(24)
if len(b) >= 24 and b[12:16] == b'IHDR':
    print(struct.unpack('>I', b[16:20])[0], struct.unpack('>I', b[20:24])[0])
PY
)"
    if [ "$dims" = "1000 500" ]; then
      pass "leg bead→render — uid=$got_uid bead=$got_bead ${got_bytes}B PNG, geometry 1000x500"
    else
      fail "leg bead→render geometry" "PNG is ${dims:-unparseable}, requested 1000x500"
    fi
  fi
else
  fail "leg bead→render" "gb exited non-zero: $(tail -3 "$WORKDIR/dash.err" 2>/dev/null)"
fi
echo

# ── Leg 2: panel-route (--panel → d-solo?panelId, still a real PNG) ──
blue "[leg panel-route] --panel 2 → d-solo single-panel route"
OUT2="$WORKDIR/panel.png"
J2="$WORKDIR/panel.json"
if run_gb "$DASHBOARD_BEAD" --panel 2 --grafana-url "$REAL_URL" --out "$OUT2" \
     --width 800 --height 300 --json >"$J2" 2>"$WORKDIR/panel.err"; then
  got_url="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("url",""))' "$J2" 2>/dev/null || true)"
  case "$got_url" in
    *"/render/d-solo/${DASHBOARD_UID}"*panelId=2*)
      if [ -s "$OUT2" ] && head -c 8 "$OUT2" | grep -q 'PNG'; then
        pass "leg panel-route — d-solo route + panelId=2, real PNG"
      else
        fail "leg panel-route file" "d-solo route ok but no PNG at $OUT2"
      fi ;;
    *)
      fail "leg panel-route" "render url did not use d-solo/panelId route: $got_url" ;;
  esac
else
  fail "leg panel-route" "gb exited non-zero: $(tail -3 "$WORKDIR/panel.err" 2>/dev/null)"
fi
echo

# ── Leg 3: raw-uid (--uid forces raw interpretation, no bead lookup) ──
blue "[leg raw-uid] --uid renders the arg verbatim (no bead deref)"
OUT3="$WORKDIR/rawuid.png"
J3="$WORKDIR/rawuid.json"
# Pass the DASHBOARD_BEAD id but with --uid: the arg must be treated as a raw
# Grafana uid (rendered verbatim), NOT dereferenced to dashboard_uid.
if run_gb "$DASHBOARD_BEAD" --uid --grafana-url "$REAL_URL" --out "$OUT3" \
     --width 1000 --height 500 --json >"$J3" 2>"$WORKDIR/rawuid.err"; then
  got_uid="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("uid",""))' "$J3" 2>/dev/null || true)"
  if [ "$got_uid" = "$DASHBOARD_BEAD" ]; then
    pass "leg raw-uid — --uid rendered arg verbatim ($got_uid), no bead deref"
  else
    fail "leg raw-uid" "with --uid, uid=$got_uid want the raw arg $DASHBOARD_BEAD (forceUID ignored?)"
  fi
else
  fail "leg raw-uid" "gb exited non-zero: $(tail -3 "$WORKDIR/rawuid.err" 2>/dev/null)"
fi
echo

# ── Leg 4: placeholder (200 + tiny fixed PNG → loud error, NO file) ──
blue "[leg placeholder] renderer-missing placeholder → loud fail, no file"
OUT4="$WORKDIR/placeholder.png"
rm -f "$OUT4"
if run_gb "$DASHBOARD_UID" --uid --grafana-url "$PLACEHOLDER_URL" --out "$OUT4" \
     --width 1000 --height 500 >"$WORKDIR/placeholder.out" 2>"$WORKDIR/placeholder.err"; then
  fail "leg placeholder" "gb exited 0 on a placeholder render — a broken image would be reported as success"
else
  if grep -qi 'placeholder image' "$WORKDIR/placeholder.err" \
     && grep -qi 'Image Renderer plugin' "$WORKDIR/placeholder.err"; then
    if [ -e "$OUT4" ]; then
      fail "leg placeholder file" "gb failed loudly but STILL wrote $OUT4 (broken file leaked)"
    else
      pass "leg placeholder — non-zero exit, actionable error, no file written"
    fi
  else
    fail "leg placeholder msg" "non-zero exit but error not actionable: $(tail -2 "$WORKDIR/placeholder.err")"
  fi
fi
echo

# ── Leg 5: render-404 (sidecar absent → actionable error, NO file) ──
blue "[leg render-404] renderer 404 → 'sidecar may not be deployed', no file"
OUT5="$WORKDIR/notfound.png"
rm -f "$OUT5"
if run_gb "$DASHBOARD_UID" --uid --grafana-url "$NOTFOUND_URL" --out "$OUT5" \
     --width 1000 --height 500 >"$WORKDIR/notfound.out" 2>"$WORKDIR/notfound.err"; then
  fail "leg render-404" "gb exited 0 on a 404 render"
else
  if grep -qi 'image-renderer sidecar may not be deployed' "$WORKDIR/notfound.err"; then
    if [ -e "$OUT5" ]; then
      fail "leg render-404 file" "gb failed but STILL wrote $OUT5"
    else
      pass "leg render-404 — non-zero exit, actionable 404 error, no file written"
    fi
  else
    fail "leg render-404 msg" "non-zero exit but error not actionable: $(tail -2 "$WORKDIR/notfound.err")"
  fi
fi
echo

# ── Leg 6: not-a-dashboard (existing type!=dashboard bead → hard error) ──
blue "[leg not-a-dashboard] a type=task bead → hard error suggesting --uid"
OUT6="$WORKDIR/nota.png"
rm -f "$OUT6"
if run_gb "$TASK_BEAD" --grafana-url "$REAL_URL" --out "$OUT6" \
     >"$WORKDIR/nota.out" 2>"$WORKDIR/nota.err"; then
  fail "leg not-a-dashboard" "gb rendered a type=task bead — should be a hard error, never a raw-uid render of the bead id"
else
  if grep -qi 'not a dashboard' "$WORKDIR/nota.err" && grep -qi -- '--uid' "$WORKDIR/nota.err"; then
    if [ -e "$OUT6" ]; then
      fail "leg not-a-dashboard file" "hard error but STILL wrote $OUT6"
    else
      pass "leg not-a-dashboard — hard error naming the type + suggesting --uid, no file"
    fi
  else
    fail "leg not-a-dashboard msg" "non-zero exit but error unclear: $(tail -2 "$WORKDIR/nota.err")"
  fi
fi
echo

# ── Summary ─────────────────────────────────────────────────────────
blue "════════════════════════════════════════════════════"
total=$((PASS + FAIL))
if [ "$FAIL" -eq 0 ]; then
  green " ✓ All $total assertions passed"
else
  red " ✗ $FAIL/$total assertions failed"
  echo
  for err in "${ERRORS[@]}"; do red "   - $err"; done
fi
blue "════════════════════════════════════════════════════"

[ "$FAIL" -eq 0 ]
