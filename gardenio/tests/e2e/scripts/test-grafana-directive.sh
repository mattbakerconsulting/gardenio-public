#!/usr/bin/env bash
# test-grafana-directive.sh — the markdown `:::grafana <uid>` embed directive,
# end-to-end through the REAL beads-viewer (task ka-0000007821, coverage-backfill
# epic ka-0000000958).
#
# Closes the gap the W2c LinkifyHTML directive post-pass (commit 43d7ac036) left
# open: the `:::grafana` markdown directive — which every linkified surface
# (bead descriptions, comments, bundle md docs) expands into a sandboxed Grafana
# iframe when GRAFANA_BASE_URL is set, and degrades to a quiet "not configured"
# note when it is not — shipped with strong UNIT coverage (dashboard_view_test.go
# TestEmbedGrafanaDirectives_*) but NO end-to-end assertion driving the REAL
# beads-viewer binary rendering a real bead's description through the real
# goldmark → LinkifyHTML → embedGrafanaDirectives pipeline. The two live grafana
# modules assert a DIFFERENT path: test-grafana-iframe.sh renders from a
# type=dashboard bead's dashboard_uid FIELD (dashboard_view.go EmbedURL), and
# test-costs-spend-page.sh renders via renderCostsChartPanels — neither exercises
# the markdown-directive path (embedGrafanaDirectives / grafanaDirectiveRe).
#
# ── Why a hermetic stub-daemon rig, not a live-cluster port-forward ───────────
#
# Two constraints, both decisive:
#
#   1. The directive's iframe-vs-degraded behaviour is gated on GRAFANA_BASE_URL,
#      read from the viewer's process env at render time. To assert BOTH paths we
#      must boot the viewer twice with different env — impossible against one
#      long-lived deployed viewer, and the deployed viewer's OIDC surface is
#      singleton/prod-only anyway (absent in the ephemeral e2e namespaces).
#   2. The logic under test is a pure server-side render: bead.Description →
#      goldmark → LinkifyHTML → embedGrafanaDirectives → HTML. A browser adds
#      nothing a curl of the bead-show page does not already see (this is exactly
#      why test-grafana-iframe.sh asserts the framing headers via raw curl). So
#      driving it needs only a beads daemon that answers GET /v1/beads/{id} with a
#      bead whose description carries the directive.
#
# So this module stands up a throwaway beads-daemon stub (Python stdlib, always
# present in the e2e shard — e2e-system-deps.lock ships python3) serving one
# task bead whose description packs the directive shapes, boots the REAL
# beads-viewer binary against it TWICE — once with GRAFANA_BASE_URL set
# (iframe path) and once without (degraded note) — and curls the bead-show page
# in each. It is hermetic: ZERO pods, ZERO shared-daemon writes, ZERO live
# Grafana, and does NOT use the shard's beads tunnel.
#
# ── The bead description under test ───────────────────────────────────────────
#
# Five directive lines exercising every branch of grafanaDirectiveRe /
# parseGrafanaEmbedOpts:
#
#   :::grafana cost-overview                     → whole-dashboard  /d/<uid>?kiosk
#   :::grafana cost-overview panel=2             → single panel     /d-solo/<uid>?panelId=2&kiosk
#   :::grafana cost-overview var-agent=<name>    → var-scoped embed …&var-agent=<name>
#   :::grafana bad/uid/here                      → INVALID uid (slash fails the
#                                                  whole-directive match) → NOT an
#                                                  iframe; survives as plain text
#   :::grafana cost-overview panel=abc           → non-numeric panel dropped by
#                                                  Atoi → degrades to whole-dashboard
#
# So a correctly-configured render emits exactly FOUR iframes (the four valid
# directives; the slash-uid one is never an iframe), each pointed ONLY at the
# configured GRAFANA_BASE_URL host.
#
# ── Legs (each a hard sub-gate; any FAIL reddens the shard) ───────────────────
#
#   1  configured-iframes   With GRAFANA_BASE_URL set, the description expands to
#                           exactly 4 <iframe class="grafana-embed-frame">, one
#                           each for whole / panel=2 / var-agent / panel=abc→whole.
#   2  url-shapes           The whole-dashboard iframe src is <base>/d/<uid>?kiosk;
#                           the panel iframe is <base>/d-solo/<uid>?panelId=2&kiosk;
#                           the var iframe carries &var-agent=<name>. [acceptance:
#                           "d-solo/panel/var URL build"]
#   3  base-url-confinement EVERY grafana iframe src points ONLY at the configured
#                           GRAFANA_BASE_URL origin — no iframe leaks a different
#                           host, and the invalid slash-uid never reaches a src.
#   4  invalid-uid-degrades The slash-bearing uid is NOT rendered as an iframe
#                           (charset guard) — it survives as literal text, so a
#                           malformed directive degrades safely, never a broken
#                           embed. [acceptance: "charset-validated uid"]
#   5  unconfigured-degrade With GRAFANA_BASE_URL UNSET, the SAME description emits
#                           ZERO grafana iframes and instead renders the quiet
#                           "grafana-embed--unconfigured" note ("Grafana not
#                           configured") carrying the escaped uid. [acceptance:
#                           "with GRAFANA_BASE_URL unset it degrades"]
#
# ── Skip-clean contract ──────────────────────────────────────────────────────
#
# The beads-viewer binary source + a Go toolchain + python3 are present in the
# e2e shard (build tasks + e2e-system-deps.lock). If the viewer cannot be built
# or python3 is absent the module SKIPS CLEANLY (exit 0) with a LOUD banner — a
# missing toolchain must never red the suite — mirroring the grafana-bridge
# skip_all("loud") contract. When present, every leg is a HARD assertion.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-grafana-directive.sh

set -uo pipefail

# ── Config ────────────────────────────────────────────────────────
CONTROLLER_DIR="${CONTROLLER_DIR:-gardenio/controller}"
RUN_ID="${RUN_ID:-$$}"
WORKDIR="$(mktemp -d "/tmp/grafana-directive.${RUN_ID}.XXXXXX")"
VIEWER_BIN=""
VIEWER_TEMP=""       # non-empty only when we built a throwaway binary to clean up
BEAD_ID="gd-GrafDirectiveE2E"      # synthetic; served only by our stub daemon
DASH_UID="cost-overview"
VAR_AGENT="gardenio-foo-123"
GRAFANA_BASE="https://grafana.directive-e2e.example/grafana"
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

skip_all() {
  local why="$1"
  red "════════════════════════════════════════════════════"
  red " SKIP grafana-directive (LOUD): $why"
  red " This run did NOT assert the ':::grafana' markdown embed directive"
  red " (goldmark → LinkifyHTML → embedGrafanaDirectives: iframe when"
  red " GRAFANA_BASE_URL is set, degraded note when it is not). A green suite"
  red " here proves NOTHING about that path. The e2e shard has a Go toolchain +"
  red " the beads-viewer source + python3 — if this fires in CI, that regressed."
  red "════════════════════════════════════════════════════"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

cleanup() {
  for p in "${PIDS[@]:-}"; do
    [ -n "$p" ] && kill "$p" 2>/dev/null || true
  done
  [ -n "$VIEWER_TEMP" ] && rm -f "$VIEWER_TEMP" 2>/dev/null || true
  rm -rf "$WORKDIR" 2>/dev/null || true
}
trap cleanup EXIT

free_port() {
  python3 - <<'PY'
import socket
s = socket.socket(); s.bind(('127.0.0.1', 0)); print(s.getsockname()[1]); s.close()
PY
}

wait_http() {
  local url="$1" tries="${2:-80}"
  for _ in $(seq 1 "$tries"); do
    if curl -s -o /dev/null --max-time 1 "$url" 2>/dev/null; then return 0; fi
    sleep 0.1
  done
  return 1
}

echo "════════════════════════════════════════════════════"
echo " :::grafana directive E2E (ka-0000007821)"
echo " configured-iframes · url-shapes · base-confinement · invalid-uid · unconfigured-degrade"
echo "════════════════════════════════════════════════════"

command -v python3 >/dev/null 2>&1 \
  || skip_all "python3 absent — cannot stand up the stub beads-daemon"

# ── 1. Resolve the beads-viewer binary ──────────────────────────────
# Prefer a pre-built beads-viewer on PATH / at $BEADS_VIEWER_BIN; else build
# from source in workspace mode (the viewer imports go.work modules).
if [ -n "${BEADS_VIEWER_BIN:-}" ] && [ -x "${BEADS_VIEWER_BIN:-}" ]; then
  VIEWER_BIN="$BEADS_VIEWER_BIN"
  pass "using pre-built beads-viewer (\$BEADS_VIEWER_BIN=$VIEWER_BIN)"
elif command -v beads-viewer >/dev/null 2>&1; then
  VIEWER_BIN="$(command -v beads-viewer)"
  pass "using pre-built beads-viewer ($VIEWER_BIN)"
else
  command -v go >/dev/null 2>&1 \
    || skip_all "no beads-viewer on PATH/\$BEADS_VIEWER_BIN and no go toolchain to build it"
  [ -d "$CONTROLLER_DIR/cmd/beads-viewer" ] \
    || skip_all "no beads-viewer binary and cmd/beads-viewer missing under $CONTROLLER_DIR"
  [ -f go.work ] \
    || skip_all "no beads-viewer binary and go.work missing (workspace build needs it)"
  blue "[build] cmd/beads-viewer (from source, workspace mode)"
  VIEWER_TEMP="$(mktemp "/tmp/beads-viewer.${RUN_ID}.XXXXXX")"
  VIEWER_BIN="$VIEWER_TEMP"
  BUILD_LOG="/tmp/beads-viewer-build-${RUN_ID}.log"
  if ( GOWORK="$(pwd)/go.work" CGO_ENABLED=0 go build -o "$VIEWER_BIN" ./gardenio/controller/cmd/beads-viewer/ ) >"$BUILD_LOG" 2>&1; then
    pass "cmd/beads-viewer built from source"
  else
    fail "build beads-viewer" "go build failed: $(tail -8 "$BUILD_LOG")"
    blue "════════════════════════════════════════════════════"
    red " ✗ 1/1 assertions failed (beads-viewer build)"
    for err in "${ERRORS[@]}"; do red "   - $err"; done
    blue "════════════════════════════════════════════════════"
    exit 1
  fi
fi
echo

# ── 2. Write the stub daemon ────────────────────────────────────────
# Serves GET /v1/beads/{BEAD_ID} (the only load-bearing endpoint in
# handleBeadShow — everything else is best-effort and answered with empty
# shapes / 404 so the page still renders). The bead's description packs the five
# directive shapes described in the header.
STUB_PY="$WORKDIR/stub-daemon.py"
cat > "$STUB_PY" <<PY
import http.server, json, sys, urllib.parse as up
BEAD_ID = "${BEAD_ID}"
DESC = (
    "Dashboard embeds under test.\n\n"
    ":::grafana ${DASH_UID}\n\n"
    "single panel:\n\n"
    ":::grafana ${DASH_UID} panel=2\n\n"
    "scoped to one agent:\n\n"
    ":::grafana ${DASH_UID} var-agent=${VAR_AGENT}\n\n"
    "an INVALID uid (contains slashes) must NOT become an iframe:\n\n"
    ":::grafana bad/uid/here\n\n"
    "a non-numeric panel degrades to the whole dashboard:\n\n"
    ":::grafana ${DASH_UID} panel=abc\n"
)
def bead():
    return {"id": BEAD_ID, "type": "task", "kind": "issue", "status": "open",
            "title": "Grafana directive E2E", "description": DESC, "fields": {},
            "labels": [], "priority": 2, "created_by": "e2e"}
class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def _j(self, c, o):
        b = json.dumps(o).encode(); self.send_response(c)
        self.send_header("Content-Type", "application/json"); self.send_header("Content-Length", str(len(b)))
        self.end_headers(); self.wfile.write(b)
    def do_GET(self):
        p = up.urlparse(self.path).path
        if p in ("/healthz", "/v1/health"): return self._j(200, {"status": "ok"})
        if p == "/v1/beads/" + BEAD_ID: return self._j(200, bead())
        if p.endswith(("/dependencies", "/reverse-dependencies")): return self._j(200, {"dependencies": [], "reverse_dependencies": []})
        if p.endswith("/comments"): return self._j(200, {"comments": []})
        if p.endswith("/events"): return self._j(200, {"events": []})
        if p.endswith(("/mentions", "/mentioned-by")): return self._j(200, {"beads": [], "mentions": [], "total": 0})
        if p.endswith("/similar"): return self._j(200, {"results": []})
        if p.endswith("/followers"): return self._j(200, {"followers": []})
        return self._j(404, {"message": "not found"})
    def do_POST(self): return self._j(200, {"beads": {}})
http.server.HTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
PY

DAEMON_PORT="$(free_port)"
python3 "$STUB_PY" "$DAEMON_PORT" & PIDS+=($!)
DAEMON_URL="http://127.0.0.1:${DAEMON_PORT}"
if ! wait_http "${DAEMON_URL}/v1/beads/${BEAD_ID}"; then
  skip_all "stub beads-daemon failed to come up (python3 http.server unavailable?)"
fi
pass "stub beads-daemon up (port $DAEMON_PORT)"
echo

# render_bead_show boots the viewer with the given GRAFANA_BASE_URL (empty for
# the degraded path), curls the bead-show page into $2, and stops the viewer.
# Returns non-zero if the viewer never became healthy or the page was not 200.
#   $1 = GRAFANA_BASE_URL value ("" → unconfigured)
#   $2 = output HTML path
render_bead_show() {
  local grafana_base="$1" out="$2"
  local port; port="$(free_port)"
  local log="$WORKDIR/viewer-${port}.log"
  # BEADS_VIEWER_DEV_AUTH_BYPASS + the agent-pod ACK: the shard runs inside a
  # pod with a mounted SA token, which the plain bypass refuses (prod-like-env
  # guard). The ACK opt-in allows it and forces LISTEN_ADDR to loopback — which
  # is exactly what we want for a throwaway local viewer. The edge-stamped
  # X-Auth-Request-Email header (sent on the curl below) supplies the identity.
  GRAFANA_BASE_URL="$grafana_base" \
  BEADS_VIEWER_DEV_AUTH_BYPASS=1 \
  BEADS_VIEWER_DEV_AUTH_BYPASS_ACK_AGENT_POD=1 \
  BEADS_HTTP_ADDR="$DAEMON_URL" \
  LISTEN_ADDR="127.0.0.1:${port}" \
  LOG_LEVEL=error \
  "$VIEWER_BIN" >"$log" 2>&1 &
  local vpid=$!
  PIDS+=("$vpid")
  if ! wait_http "http://127.0.0.1:${port}/healthz"; then
    kill "$vpid" 2>/dev/null || true
    return 1
  fi
  local code
  code="$(curl -s -o "$out" -w '%{http_code}' \
    -H 'X-Auth-Request-Email: grafana-directive-e2e@example.com' \
    "http://127.0.0.1:${port}/${BEAD_ID}")"
  kill "$vpid" 2>/dev/null || true
  [ "$code" = "200" ]
}

# count_iframes counts REAL grafana embed iframe TAGS (not CSS/template mentions
# of the class name — the stylesheet defines .grafana-embed-frame, so a bare
# substring grep over-counts). Anchoring on `<iframe class="grafana-embed-frame"`
# matches only emitted elements. Counts OCCURRENCES (grep -o | wc -l), not lines,
# so a template that ever packs two iframes on one line is still counted right.
# Piping grep into wc means the pipeline's final exit is wc/tr's success even on
# a zero-match grep, so nothing trips `set -o pipefail` (the script sets -uo
# pipefail, no -e, but this keeps the value clean regardless).
count_iframes() { grep -o '<iframe class="grafana-embed-frame"' "$1" 2>/dev/null | wc -l | tr -d ' '; }

# ── Render the CONFIGURED page ──────────────────────────────────────
CONF_HTML="$WORKDIR/configured.html"
blue "[render] viewer with GRAFANA_BASE_URL=$GRAFANA_BASE"
if ! render_bead_show "$GRAFANA_BASE" "$CONF_HTML"; then
  fail "render configured" "viewer did not serve a 200 bead-show page (log: $(tail -3 "$WORKDIR"/viewer-*.log 2>/dev/null))"
fi

# ── Leg 1: configured-iframes (exactly 4 real iframes) ──
blue "[leg configured-iframes] exactly 4 grafana-embed-frame iframes"
n_iframes="$(count_iframes "$CONF_HTML")"
if [ "${n_iframes:-0}" = "4" ]; then
  pass "leg configured-iframes — 4 iframes (whole · panel=2 · var-agent · panel=abc→whole)"
else
  fail "leg configured-iframes" "got ${n_iframes:-0} grafana-embed-frame iframes, want 4"
fi
echo

# ── Leg 2: url-shapes (whole / d-solo panel / var query) ──
blue "[leg url-shapes] d/ vs d-solo routing + var-agent query"
whole_ok=0; panel_ok=0; var_ok=0
grep -q "<iframe class=\"grafana-embed-frame\" src=\"${GRAFANA_BASE}/d/${DASH_UID}?kiosk\"" "$CONF_HTML" && whole_ok=1
# The panel & var srcs carry a '&' HTML-escaped to '&amp;' as an attribute value.
grep -q "src=\"${GRAFANA_BASE}/d-solo/${DASH_UID}?panelId=2&amp;kiosk\"" "$CONF_HTML" && panel_ok=1
grep -q "src=\"${GRAFANA_BASE}/d/${DASH_UID}?kiosk&amp;var-agent=${VAR_AGENT}\"" "$CONF_HTML" && var_ok=1
if [ "$whole_ok" = 1 ] && [ "$panel_ok" = 1 ] && [ "$var_ok" = 1 ]; then
  pass "leg url-shapes — /d/<uid>?kiosk, /d-solo/<uid>?panelId=2&kiosk, &var-agent=<name>"
else
  fail "leg url-shapes" "whole=$whole_ok panel=$panel_ok var=$var_ok (a URL shape regressed)"
fi
echo

# ── Leg 3: base-url-confinement (every iframe src → configured host only) ──
blue "[leg base-confinement] every grafana iframe src points only at GRAFANA_BASE_URL"
# Extract every grafana-embed-frame src; assert each begins with the configured
# base. A src to any other host is a leak.
mapfile -t SRCS < <(grep -oE '<iframe class="grafana-embed-frame" src="[^"]*"' "$CONF_HTML" \
                    | sed -E 's/.*src="([^"]*)".*/\1/')
leak=""
for s in "${SRCS[@]:-}"; do
  case "$s" in
    "${GRAFANA_BASE}"/*) : ;;         # ok
    *) leak="$s"; break ;;
  esac
done
if [ "${#SRCS[@]}" -ge 1 ] && [ -z "$leak" ]; then
  pass "leg base-confinement — all ${#SRCS[@]} iframe srcs under $GRAFANA_BASE"
else
  fail "leg base-confinement" "iframe src escaped the configured base: ${leak:-<no iframes found>}"
fi
echo

# ── Leg 4: invalid-uid-degrades (slash uid never an iframe) ──
blue "[leg invalid-uid] slash-bearing uid degrades to text, never an iframe"
# 'bad/uid/here' must never appear inside an iframe src; it survives as the
# literal directive text (charset guard rejected the whole-directive match).
if grep -qE '<iframe[^>]*src="[^"]*bad/uid/here' "$CONF_HTML"; then
  fail "leg invalid-uid" "the invalid slash-uid leaked into an iframe src (charset guard bypassed)"
elif grep -q ':::grafana bad/uid/here' "$CONF_HTML"; then
  pass "leg invalid-uid — slash-uid not an iframe; survives as literal text"
else
  # Not an iframe AND not literal text is still safe (no broken embed), but the
  # literal-text survival is the documented degrade — flag if neither holds so a
  # silent change in behaviour is noticed.
  fail "leg invalid-uid" "slash-uid neither rendered as text nor as an iframe — behaviour changed unexpectedly"
fi
echo

# ── Render the UNCONFIGURED page + Leg 5 ──
UNCONF_HTML="$WORKDIR/unconfigured.html"
blue "[render] viewer with GRAFANA_BASE_URL unset (degraded path)"
if ! render_bead_show "" "$UNCONF_HTML"; then
  fail "render unconfigured" "viewer did not serve a 200 bead-show page unconfigured (log: $(tail -3 "$WORKDIR"/viewer-*.log 2>/dev/null))"
fi
blue "[leg unconfigured-degrade] no iframes; quiet 'not configured' note instead"
u_iframes="$(count_iframes "$UNCONF_HTML")"
u_notes="$(grep -c 'grafana-embed--unconfigured' "$UNCONF_HTML" 2>/dev/null || true)"
u_msg="$(grep -c 'Grafana not configured' "$UNCONF_HTML" 2>/dev/null || true)"
u_uid="$(grep -c "<code>${DASH_UID}</code>" "$UNCONF_HTML" 2>/dev/null || true)"
if [ "${u_iframes:-0}" != "0" ]; then
  fail "leg unconfigured-degrade" "unconfigured page emitted ${u_iframes} grafana iframes — must be 0"
elif [ "${u_notes:-0}" -lt 1 ] || [ "${u_msg:-0}" -lt 1 ]; then
  fail "leg unconfigured-degrade" "missing the 'grafana-embed--unconfigured' note (notes=${u_notes:-0} msg=${u_msg:-0})"
elif [ "${u_uid:-0}" -lt 1 ]; then
  fail "leg unconfigured-degrade" "degraded note did not carry the escaped uid <code>${DASH_UID}</code>"
else
  pass "leg unconfigured-degrade — 0 iframes, ${u_notes} unconfigured notes carrying the uid"
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
