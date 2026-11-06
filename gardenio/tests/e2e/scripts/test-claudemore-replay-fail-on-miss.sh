#!/usr/bin/env bash
# test-claudemore-replay-fail-on-miss.sh — E2E gate for the claudemore REPLAY-mode
# fail_on_miss HTTP contract: a cassette miss is a LOUD 424 with the REPLAY miss
# marker, NEVER a silent fall-back to LIVE. This is the single load-bearing
# safety property the whole $0/deterministic REPLAY tier rests on.
#
# Bead ka-0000006183 (parent ka-0000000958, feature ka-0000005558 / MR !6686).
# Source: gardenio/controller/cmd/claudemore/replay.go
#   - writeMiss (:148-158): sets X-Claudemore-Replay: miss,
#     X-Claudemore-Replay-Miss: <model>/<hash>, and http.StatusFailedDependency (424).
#   - serveCassette (:113-146): a HIT sets X-Claudemore-Replay: hit + serves the
#     recorded status/body byte-for-byte.
#   - ServeHTTP HEAD arm (:59-63): the SDK connectivity preflight → 200 +
#     X-Claudemore-Replay: preflight.
#
# ── THE GAP THIS CLOSES ───────────────────────────────────────────────────────
#
# claudemore's REPLAY tier serves recorded cassettes at $0 with litellm bypassed
# entirely. Its ONE non-negotiable safety property is fail_on_miss: a request
# with no matching cassette must fail LOUDLY (424 + the miss marker), never fall
# through to the LIVE upstream. A REPLAY tier that quietly reached litellm on a
# miss would be neither $0 nor deterministic — it would silently start spending
# REAL tokens against real litellm while CI reports green. That is precisely the
# invariant the tier's "$0" claim rests on.
#
# `grep -rn "X-Claudemore-Replay" gardenio/tests/` → 0 (verified). The two hits
# for StatusFailedDependency / 424 across both e2e suites
# (test-claudemore-hooks.sh:15 and the since-deleted chart-render module
# test-claudemore-replay-store-gate.sh:15) were PROSE COMMENTS, not assertions — a name-match trap.
#
# Why the EXISTING zero-miss assertion cannot catch this: test-claudemore-hooks.sh
# asserts the happy path (T5_MISS = 0) by counting `cassette MISS` LOG lines. If
# writeMiss regressed to a 404, a 200-with-empty-body, or — worst — a pass-through
# to cfg.upstreamURL, EVERY existing assertion still passes: a fall-through
# produces NO miss log and a SUCCESSFUL run. The failure mode is invisible to a
# happy-path test. This module asserts the miss contract DIRECTLY.
#
# ── WHY A FROM-SOURCE BOOT PROBE (no cluster, no `claude` binary) ─────────────
#
# claudemore is test-lane only — the chart no longer ships it (gd-Y4ZCGr2m2t) —
# so there is no live Deployment to hit. Unlike the
# rest of the claudemore cluster, the fail_on_miss HTTP contract is testable with
# NO `claude` binary — it is a plain HTTP request/response contract. So this
# module builds cmd/claudemore from source (hermetic, same shape as
# test-claudemore-hooks.sh), boots it in REPLAY
# mode over a FILESYSTEM cassette store (CLAUDEMORE_CASSETTE_DIR — no S3/MinIO),
# and drives it with plain curl. It needs no kubectl, no beads, no tunnel — it
# runs GREEN in every shard container carrying the Go toolchain + curl + python3,
# and SKIPs CLEANLY (loud) without them.
#
# ── THE ANTI-VACUITY CONTROLS (what makes each PASS non-vacuous) ──────────────
#
# Two distinct regressions could defeat fail_on_miss, and each has a dedicated
# control so a green here means BOTH are excluded:
#
#   1. Silent fall-back to LIVE. Guarded by pointing CLAUDEMORE_UPSTREAM_URL at a
#      CLOSED port (127.0.0.1:1). If writeMiss regressed to proxy the upstream,
#      the request would hit connection-refused and produce a 502/hang — NOT a
#      clean 424+miss. Without the closed port a fall-through against a real
#      litellm would SILENTLY SUCCEED (200 + real spend) and this test would not
#      catch it. This is the control the bead calls out as load-bearing.
#   2. Blanket 424 (the POST path always fails, e.g. a total REPLAY outage even
#      with a full cassette set). A miss-only test cannot tell "correctly missing"
#      from "always 424". Guarded by the HIT control (leg 3): a REAL
#      record→replay round-trip must return 200 + X-Claudemore-Replay: hit +
#      the recorded body. The cassette is produced by claudemore's OWN recorder
#      (LIVE+record against a local stub), NOT hand-authored — so leg 3 asserts
#      against the shape the producing path actually emits (ka-0000001521).
#
# ── Legs (each a hard sub-gate; any FAIL reds the shard) ──────────────────────
#
#   1 miss-424        HEADLINE. REPLAY over an EMPTY cassette dir + CLOSED upstream.
#                     POST /v1/messages for an uncassetted model → status EXACTLY
#                     424, X-Claudemore-Replay: miss, X-Claudemore-Replay-Miss
#                     starts with "<model>/". The one assertion between a $0
#                     deterministic tier and silent real spend.
#   2 preflight-200   HEAD / → 200 + X-Claudemore-Replay: preflight. Upgrades the
#                     SDK-connectivity-preflight arm from consequence-only to a
#                     direct assertion (replay.go:59-63). Same boot as leg 1.
#   3 hit-200         NON-VACUITY. Record a request through claudemore LIVE+record
#                     against a local stub, then REPLAY the SAME request over that
#                     dir with a CLOSED upstream → 200 + X-Claudemore-Replay: hit +
#                     the recorded marker in the body. Proves leg 1's 424 is
#                     MISS-specific, not a POST path that always 424s.
#   4 populated-miss  Over the SAME populated dir, POST a DIFFERENT (unrecorded)
#                     request → 424 + miss. Proves the miss is keyed to the
#                     request HASH (a genuinely-absent cassette), not merely
#                     "empty dir → 424, any populated dir → 200". Distinguishes
#                     "has THIS cassette" from "has ANY cassette".
#
# ── Skip-clean contract ───────────────────────────────────────────────────────
#
# The Go toolchain + cmd/claudemore source + curl + python3 are present in every
# e2e shard container (this is the same toolchain claudemore-hooks needs, minus
# the `claude` binary). If any is absent the module SKIPS CLEANLY (exit 0) with a
# LOUD banner — an absent toolchain must never red the suite. When all are
# present, every leg is a HARD assertion.
#
# Standalone runnable:
#   ./gardenio/tests/e2e/scripts/test-claudemore-replay-fail-on-miss.sh

set -uo pipefail

# ── Config ────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# scripts/ -> e2e/ -> tests/ -> gardenio/ -> controller
CONTROLLER_DIR="${CONTROLLER_DIR:-}"
if [ -z "$CONTROLLER_DIR" ]; then
  CONTROLLER_DIR="$(cd "$SCRIPT_DIR/../../../controller" 2>/dev/null && pwd)" || CONTROLLER_DIR=""
fi
GO_BIN="${GO_BIN:-go}"
WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/claudemore-failonmiss.XXXXXX")"

# A closed port for the upstream: nothing listens on 127.0.0.1:1, so any
# fall-through to LIVE hits connection-refused instead of silently succeeding.
CLOSED_UPSTREAM="http://127.0.0.1:1"
# A distinctive probe model so the X-Claudemore-Replay-Miss "<model>/" prefix is
# unambiguous and can never collide with an incidental default. It is a clean
# header token (no spaces/control chars) so it survives headerSafe verbatim.
PROBE_MODEL="claudemore-failonmiss-probe"
# A marker the stub embeds in its recorded response body so the HIT leg can
# assert the cassette body — not just the status — round-trips.
HIT_MARKER="CLAUDEMORE_FAILONMISS_HIT_MARKER"

PASS=0
FAIL=0
ERRORS=()
CM_BIN=""

# ── Helpers ───────────────────────────────────────────────────────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

pass() { green "  ✓ $1"; PASS=$((PASS + 1)); }
fail() {
  local name="$1" reason="${2:-}"
  red "  ✗ $name"
  [ -n "$reason" ] && dim "    $reason"
  FAIL=$((FAIL + 1))
  ERRORS+=("$name: $reason")
}

skip_clean() {
  blue "SKIP claudemore-replay-fail-on-miss: $1"
  blue "  This module builds cmd/claudemore from source and drives its REPLAY"
  blue "  fail_on_miss HTTP contract with curl; without the toolchain there is"
  blue "  nothing to drive. Skipping cleanly (exit 0) — an absent toolchain must"
  blue "  never red the suite (see test-claudemore-hooks)."
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

cleanup() {
  for pidf in "$WORKDIR"/*.pid; do
    [ -f "$pidf" ] || continue
    local p; p="$(cat "$pidf" 2>/dev/null)"
    [ -n "$p" ] && kill "$p" >/dev/null 2>&1 || true
  done
  rm -rf "$WORKDIR" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# free_port — echo an ephemeral TCP port that is currently free.
free_port() {
  python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}

# wait_http — poll a URL until it answers or a timeout elapses.
wait_http() {
  local url="$1" tries="${2:-40}"
  for _ in $(seq 1 "$tries"); do
    if curl -s -o /dev/null -m 1 "$url" 2>/dev/null; then return 0; fi
    sleep 0.25
  done
  return 1
}

# header_value <headerdump> <name> — echo the value of a response header (CR
# trimmed), or empty if absent. The "^name:" anchor requires the colon to
# immediately follow the name, so "X-Claudemore-Replay:" does NOT match the
# distinct "X-Claudemore-Replay-Miss:" header.
header_value() {
  grep -i "^$2:" "$1" 2>/dev/null | head -1 | sed -e "s/^[^:]*: *//" -e 's/\r$//'
}

# ── Preflight: skip cleanly if any prerequisite is missing ──────────
blue "╔══════════════════════════════════════════════════════════╗"
blue "║  claudemore REPLAY fail_on_miss e2e (ka-0000006183)       ║"
blue "╚══════════════════════════════════════════════════════════╝"
echo ""

command -v "$GO_BIN" >/dev/null 2>&1 || skip_clean "'$GO_BIN' toolchain not on PATH"
command -v curl      >/dev/null 2>&1 || skip_clean "curl not on PATH"
command -v python3   >/dev/null 2>&1 || skip_clean "python3 not on PATH"
[ -n "$CONTROLLER_DIR" ] && [ -f "$CONTROLLER_DIR/go.mod" ] || skip_clean "controller module not found (looked at '${CONTROLLER_DIR:-<unresolved>}')"
[ -d "$CONTROLLER_DIR/cmd/claudemore" ] || skip_clean "cmd/claudemore source not found under $CONTROLLER_DIR"

dim "  go:         $(command -v "$GO_BIN")  ($("$GO_BIN" version 2>/dev/null | awk '{print $3}'))"
dim "  controller: $CONTROLLER_DIR"
dim "  workdir:    $WORKDIR"
echo ""

# ── Build claudemore from source ────────────────────────────────────
blue "Building claudemore from source..."
CM_BIN="$WORKDIR/claudemore"
BUILD_LOG="$WORKDIR/build.log"
if ! ( cd "$CONTROLLER_DIR" && CGO_ENABLED=0 "$GO_BIN" build -o "$CM_BIN" ./cmd/claudemore/ ) >"$BUILD_LOG" 2>&1; then
  # A missing network / module-download failure in a locked-down shard is an
  # environment gap that must not red the suite; distinguish by grepping the log.
  if grep -qiE "cannot find module|dial tcp|no such host|timeout|connection refused|proxy" "$BUILD_LOG"; then
    skip_clean "go build could not fetch deps (offline shard): $(tail -3 "$BUILD_LOG" | tr '\n' ' ')"
  fi
  red "  ✗ build failed:"
  dim "$(tail -15 "$BUILD_LOG")"
  exit 1
fi
green "  ✓ built claudemore"
echo ""

# launch_replay <cassette_dir> <upstream_url> <tag> — boot claudemore in REPLAY
# mode over the given dir, echo the chosen port. Records the pid in
# $WORKDIR/<tag>.pid for cleanup. CLAUDEMORE_CASSETTE_DIR takes precedence over
# any ambient COOP_S3_* (s3client.go:29-36), so the store is the local dir even
# in a shard where an S3 bucket env is set — the run stays hermetic.
launch_replay() {
  local dir="$1" upstream="$2" tag="$3" port
  port="$(free_port)"
  CLAUDEMORE_MODE=replay \
  CLAUDEMORE_LISTEN="127.0.0.1:$port" \
  CLAUDEMORE_UPSTREAM_URL="$upstream" \
  CLAUDEMORE_CASSETTE_DIR="$dir" \
  LOG_LEVEL=info \
    "$CM_BIN" >"$WORKDIR/cm-$tag.log" 2>&1 &
  echo $! > "$WORKDIR/$tag.pid"
  echo "$port"
}

# post_messages <port> <body> <hdrdump> <bodyout> — POST a Messages-API request,
# echo the HTTP status. Captures response headers to <hdrdump> and body to
# <bodyout>.
post_messages() {
  curl -s -o "$4" -D "$3" -w '%{http_code}' -m 10 \
    -X POST "http://127.0.0.1:$1/v1/messages" \
    -H 'content-type: application/json' \
    --data "$2" 2>/dev/null
}

# ══ LEG GROUP A: EMPTY store — headline miss (leg 1) + preflight (leg 2) ══════
CASS_EMPTY="$WORKDIR/cass-empty"; mkdir -p "$CASS_EMPTY"
blue "Booting claudemore REPLAY over an EMPTY cassette dir (upstream = closed port)..."
EMPTY_PORT="$(launch_replay "$CASS_EMPTY" "$CLOSED_UPSTREAM" empty)"
if ! wait_http "http://127.0.0.1:$EMPTY_PORT/healthz"; then
  red "  ✗ claudemore(replay, empty) did not come up — cannot assert the miss contract"
  dim "$(tail -8 "$WORKDIR/cm-empty.log" 2>/dev/null)"
  exit 1
fi
dim "  up on 127.0.0.1:$EMPTY_PORT"
echo ""

# ── Leg 1: HEADLINE — uncassetted POST → 424 + miss markers ──────────
blue "1. HEADLINE: uncassetted POST /v1/messages → 424 + X-Claudemore-Replay: miss + miss header..."
MISS_HDR="$WORKDIR/miss.hdr"; MISS_BODY="$WORKDIR/miss.body"
MISS_REQ="{\"model\":\"$PROBE_MODEL\",\"max_tokens\":16,\"messages\":[{\"role\":\"user\",\"content\":\"fail_on_miss headline probe\"}]}"
MISS_STATUS="$(post_messages "$EMPTY_PORT" "$MISS_REQ" "$MISS_HDR" "$MISS_BODY")"
MISS_MARKER="$(header_value "$MISS_HDR" "X-Claudemore-Replay")"
MISS_DETAIL="$(header_value "$MISS_HDR" "X-Claudemore-Replay-Miss")"
_leg1_ok=1
if [ "$MISS_STATUS" != "424" ]; then
  fail "leg1: status is exactly 424" \
       "got '${MISS_STATUS:-<none>}' — a miss must be http.StatusFailedDependency (replay.go:157). A 200/404/5xx or hang here means fail_on_miss regressed; upstream was a CLOSED port, so a fall-through to LIVE cannot masquerade as success."
  _leg1_ok=0
fi
if [ "$MISS_MARKER" != "miss" ]; then
  fail "leg1: X-Claudemore-Replay: miss" \
       "got '${MISS_MARKER:-<absent>}' — the miss marker header regressed (replay.go:150)"
  _leg1_ok=0
fi
case "$MISS_DETAIL" in
  "$PROBE_MODEL"/*) : ;;
  *)
    fail "leg1: X-Claudemore-Replay-Miss starts with '$PROBE_MODEL/'" \
         "got '${MISS_DETAIL:-<absent>}' — the diagnostic <model>/<hash> header regressed (replay.go:154)"
    _leg1_ok=0
    ;;
esac
if [ "$_leg1_ok" -eq 1 ]; then
  pass "uncassetted POST → 424, X-Claudemore-Replay: miss, X-Claudemore-Replay-Miss=$MISS_DETAIL (fail_on_miss holds; no silent LIVE fall-back)"
fi

# ── Leg 2: preflight HEAD → 200 + preflight marker ──────────────────
blue "2. preflight: HEAD / → 200 + X-Claudemore-Replay: preflight..."
PRE_HDR="$WORKDIR/preflight.hdr"
curl -s -I -m 10 "http://127.0.0.1:$EMPTY_PORT/" >"$PRE_HDR" 2>/dev/null
PRE_STATUS="$(awk 'NR==1{print $2}' "$PRE_HDR" 2>/dev/null)"
PRE_MARKER="$(header_value "$PRE_HDR" "X-Claudemore-Replay")"
_leg2_ok=1
if [ "$PRE_STATUS" != "200" ]; then
  fail "leg2: HEAD / → 200" \
       "got '${PRE_STATUS:-<none>}' — the SDK-connectivity preflight arm regressed (replay.go:59-63); a HEAD must never be routed to the cassette lookup (would spuriously 424)"
  _leg2_ok=0
fi
if [ "$PRE_MARKER" != "preflight" ]; then
  fail "leg2: X-Claudemore-Replay: preflight" \
       "got '${PRE_MARKER:-<absent>}' — the preflight marker regressed (replay.go:61)"
  _leg2_ok=0
fi
if [ "$_leg2_ok" -eq 1 ]; then
  pass "HEAD / → 200 + X-Claudemore-Replay: preflight (preflight arm holds; miss signal stays reserved for a genuine POST miss)"
fi

kill "$(cat "$WORKDIR/empty.pid" 2>/dev/null)" >/dev/null 2>&1 || true
echo ""

# ══ LEG GROUP B: POPULATED store — hit control (leg 3) + populated-miss (leg 4) ══
# Produce a REAL cassette via claudemore's own recorder, then replay it. The
# cassette is emitted by the LIVE record path, NOT hand-authored, so leg 3
# asserts against the shape the producing system actually writes (ka-0000001521).
blue "Recording a cassette via claudemore LIVE+record against a local stub..."

# A minimal deterministic stub upstream: one fixed non-streaming Anthropic
# Messages response carrying the HIT marker. Written to the workdir (no
# checked-in fixture needed — the record path is what makes the cassette real).
STUB="$WORKDIR/stub_upstream.py"
cat > "$STUB" <<PY
import http.server, json, os
PORT = int(os.environ["STUB_PORT"])
BODY = json.dumps({
    "id": "msg_failonmiss_stub",
    "type": "message",
    "role": "assistant",
    "model": "$PROBE_MODEL",
    "content": [{"type": "text", "text": "$HIT_MARKER"}],
    "stop_reason": "end_turn",
    "usage": {"input_tokens": 5, "output_tokens": 3},
}).encode()
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("content-length", "0"))
        self.rfile.read(n)
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(BODY)))
        self.end_headers()
        self.wfile.write(BODY)
    def log_message(self, *a):
        pass
http.server.HTTPServer(("127.0.0.1", PORT), H).serve_forever()
PY

CASS_POP="$WORKDIR/cass-pop"; mkdir -p "$CASS_POP"
HIT_REQ="{\"model\":\"$PROBE_MODEL\",\"max_tokens\":16,\"messages\":[{\"role\":\"user\",\"content\":\"cassette hit probe\"}]}"
STUB_PORT="$(free_port)"; REC_PORT="$(free_port)"

STUB_PORT="$STUB_PORT" python3 "$STUB" >"$WORKDIR/stub.log" 2>&1 &
echo $! > "$WORKDIR/stub.pid"
sleep 0.5

CLAUDEMORE_MODE=live \
CLAUDEMORE_LISTEN="127.0.0.1:$REC_PORT" \
CLAUDEMORE_UPSTREAM_URL="http://127.0.0.1:$STUB_PORT" \
CLAUDEMORE_CASSETTE_DIR="$CASS_POP" \
LOG_LEVEL=error \
  "$CM_BIN" >"$WORKDIR/cm-record.log" 2>&1 &
echo $! > "$WORKDIR/record.pid"

REC_OK=1
if ! wait_http "http://127.0.0.1:$REC_PORT/healthz"; then
  red "  ✗ claudemore(live+record) did not come up"; REC_OK=0
fi
if [ "$REC_OK" -eq 1 ]; then
  REC_STATUS="$(post_messages "$REC_PORT" "$HIT_REQ" "$WORKDIR/rec.hdr" "$WORKDIR/rec.body")"
  [ "$REC_STATUS" = "200" ] || { red "  ✗ record POST did not return 200 (got '${REC_STATUS:-<none>}')"; REC_OK=0; }
fi
sleep 1  # let the async recorder flush its Put
kill "$(cat "$WORKDIR/stub.pid" 2>/dev/null)" "$(cat "$WORKDIR/record.pid" 2>/dev/null)" >/dev/null 2>&1 || true
sleep 0.3

# Sanity: a cassette JSON must exist on disk, else legs 3+4 would be vacuous
# (an empty "populated" dir just reproduces leg 1). Fail LOUD, never skip — a
# broken record path is a real regression and a vacuous green would hide it.
CASS_COUNT=0
if [ "$REC_OK" -eq 1 ]; then
  CASS_COUNT="$(find "$CASS_POP" -type f -name '*.json' 2>/dev/null | wc -l | tr -d ' ')"
fi
if [ "$REC_OK" -ne 1 ] || [ "${CASS_COUNT:-0}" -lt 1 ]; then
  fail "record→replay setup: a cassette was written to disk" \
       "no cassette under $CASS_POP after LIVE+record (found ${CASS_COUNT:-0}); cannot assert the HIT/populated-miss controls. Record path log: $(tail -3 "$WORKDIR/cm-record.log" 2>/dev/null | tr '\n' ' ')"
else
  dim "  recorded $CASS_COUNT cassette(s) under $CASS_POP"
  echo ""

  blue "Booting claudemore REPLAY over the POPULATED dir (upstream = closed port)..."
  POP_PORT="$(launch_replay "$CASS_POP" "$CLOSED_UPSTREAM" pop)"
  if ! wait_http "http://127.0.0.1:$POP_PORT/healthz"; then
    fail "leg3/4 boot: claudemore(replay, populated) came up" \
         "did not answer /healthz: $(tail -5 "$WORKDIR/cm-pop.log" 2>/dev/null | tr '\n' ' ')"
  else
    dim "  up on 127.0.0.1:$POP_PORT"
    echo ""

    # ── Leg 3: HIT — the recorded request replays 200 + hit + body marker ──
    blue "3. HIT control: the recorded request → 200 + X-Claudemore-Replay: hit + recorded body..."
    HIT_HDR="$WORKDIR/hit.hdr"; HIT_BODY="$WORKDIR/hit.body"
    HIT_STATUS="$(post_messages "$POP_PORT" "$HIT_REQ" "$HIT_HDR" "$HIT_BODY")"
    HIT_HEADER="$(header_value "$HIT_HDR" "X-Claudemore-Replay")"
    _leg3_ok=1
    if [ "$HIT_STATUS" != "200" ]; then
      fail "leg3: recorded request → 200" \
           "got '${HIT_STATUS:-<none>}' — a recorded request must serve its cassette (serveCassette). A 424 here means the POST path always fails: leg 1's miss is a blanket outage, not fail_on_miss."
      _leg3_ok=0
    fi
    if [ "$HIT_HEADER" != "hit" ]; then
      fail "leg3: X-Claudemore-Replay: hit" \
           "got '${HIT_HEADER:-<absent>}' — the hit marker regressed (replay.go:127)"
      _leg3_ok=0
    fi
    if ! grep -qF "$HIT_MARKER" "$HIT_BODY" 2>/dev/null; then
      fail "leg3: cassette body served byte-for-byte" \
           "the recorded marker '$HIT_MARKER' is absent from the replayed body — the cassette body did not round-trip"
      _leg3_ok=0
    fi
    if [ "$_leg3_ok" -eq 1 ]; then
      pass "recorded request → 200, X-Claudemore-Replay: hit, body carries '$HIT_MARKER' (proves leg 1's 424 is MISS-specific, not a POST path that always 424s)"
    fi

    # ── Leg 4: populated-miss — a DIFFERENT request still 424s ──────────
    blue "4. populated-miss control: a DIFFERENT (unrecorded) request over the SAME dir → 424 + miss..."
    PM_HDR="$WORKDIR/popmiss.hdr"; PM_BODY="$WORKDIR/popmiss.body"
    PM_REQ="{\"model\":\"$PROBE_MODEL\",\"max_tokens\":16,\"messages\":[{\"role\":\"user\",\"content\":\"a different prompt that was never recorded\"}]}"
    PM_STATUS="$(post_messages "$POP_PORT" "$PM_REQ" "$PM_HDR" "$PM_BODY")"
    PM_MARKER="$(header_value "$PM_HDR" "X-Claudemore-Replay")"
    _leg4_ok=1
    if [ "$PM_STATUS" != "424" ]; then
      fail "leg4: unrecorded request over a populated store → 424" \
           "got '${PM_STATUS:-<none>}' — the miss is not keyed to the request hash; a populated store must still 424 a request it has no cassette for"
      _leg4_ok=0
    fi
    if [ "$PM_MARKER" != "miss" ]; then
      fail "leg4: X-Claudemore-Replay: miss on the populated-store miss" \
           "got '${PM_MARKER:-<absent>}' — the miss marker regressed for a per-hash miss"
      _leg4_ok=0
    fi
    if [ "$_leg4_ok" -eq 1 ]; then
      pass "different request over the populated dir → 424 + miss (the miss is per request-hash: 'has THIS cassette', not 'has ANY cassette')"
    fi

    kill "$(cat "$WORKDIR/pop.pid" 2>/dev/null)" >/dev/null 2>&1 || true
  fi
fi
echo ""

# ── Summary ──────────────────────────────────────────────────────
blue "══════════════════════════════════════════════"
if [ "$FAIL" -eq 0 ]; then
  green "  All $PASS claudemore REPLAY fail_on_miss assertions passed"
else
  red "  $PASS passed, $FAIL failed"
  echo ""
  for err in "${ERRORS[@]}"; do
    red "  - $err"
  done
fi
blue "══════════════════════════════════════════════"

exit "$FAIL"
