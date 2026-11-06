#!/usr/bin/env bash
# test-claudemore-replay-volatile-hash.sh — E2E gate for the claudemore REPLAY
# request-hash VOLATILE-FIELD STRIPPING: two launches whose request bodies differ
# ONLY in a per-launch-volatile field (nested cache_control breakpoint PLACEMENT,
# or the cc_version billing token) must hash IDENTICALLY and REPLAY-HIT, while a
# launch differing in a NON-volatile field (real system-prompt content) must MISS.
#
# Bead ka-0000006222 (parent ka-0000000958, related ka-0000000029).
# Source: gardenio/controller/internal/claudemore/cassette.go
#   - stripVolatileForHash (:~205) recursively deletes volatileNestedKeys
#     (cache_control) at every depth AND normalizes any cc_version=<x> token to a
#     stable placeholder — for the HASH body only; the STORED body keeps them.
#   - volatileNestedKeys / ccVersionRE are the two volatility sources it neutralizes.
#   - CanonicalizeRequest (:~130) is the shared entry point BOTH the LIVE recorder
#     (cmd/claudemore/recorder.go) and the REPLAY server (cmd/claudemore/replay.go)
#     key on, so a record and a replay of the same LOGICAL request collide.
#
# ── THE GAP THIS CLOSES ───────────────────────────────────────────────────────
#
# stripVolatileForHash exists so a REPLAY cassette recorded under one `claude`
# launch still HITS on the next launch, even though the client injects
# cache_control breakpoints at run-to-run-VOLATILE positions and prepends a
# per-launch cc_version=2.1.211.<suffix> billing token. If that stripping
# regressed, every REPLAY cassette would MISS the moment an upstream `claude`
# bump moved a breakpoint or the suffix rolled — the $0/deterministic REPLAY tier
# would hard-fail 424 fleet-wide, or (if fail_on_miss were ever relaxed) silently
# spend real litellm tokens. Nothing goes red until the next `claude` version lands.
#
# Why the EXISTING module (test-claudemore-hooks.sh) cannot catch this — it
# DELIBERATELY routes AROUND the branch. It drives the REAL `claude` binary twice
# (record then replay) and, to keep the two launches deterministic, forces them to
# emit BYTE-IDENTICAL request bodies via one fixed re-seeded HOME + --setting-sources
# "" (its own comment names cache_control/cc_version as fields it works around,
# see that file's HOME-seeding block). With identical bodies the strip code is a
# NO-OP: delete stripVolatileForHash entirely and its T5/T6 replay-zero-miss legs
# stay GREEN. That is the "assertion never reaches the system" trap — the one
# module that mentions these fields cannot exercise the code that handles them.
#
# ── WHY A FROM-SOURCE, curl-DRIVEN BOOT PROBE (no `claude` binary) ────────────
#
# The whole point is to STOP forcing byte-identity and vary ONLY the volatile
# field between the recorded request and the replayed request. The real `claude`
# binary chooses cache_control/cc_version placement itself and cannot be told to
# vary ONLY that field — which is exactly why the hooks module had to force
# identity. So this module drives the canonicalizer through claudemore's OWN HTTP
# surface with hand-authored bodies: build cmd/claudemore from source, boot it
# LIVE+record against a local stub to RECORD one body, then boot it in REPLAY over
# the same FILESYSTEM cassette dir (CLAUDEMORE_CASSETTE_DIR — no S3/MinIO) with the
# upstream at a CLOSED port, and POST a DIFFERENT body with curl. The header
# X-Claudemore-Replay: hit|miss is the verdict. Same toolchain as
# test-claudemore-replay-fail-on-miss.sh, minus nothing.
#
# The hand-authored bodies are NOT invented from the consumer code — they mirror
# the shape the REAL `claude` client emits on the wire (captured through claudemore
# LIVE+record on 2026-08-06, `claude` 2.1.211): system[0] carries
# `x-anthropic-billing-header: cc_version=2.1.211.<suffix>; cc_entrypoint=sdk-cli;`,
# cache_control {"type":"ephemeral","ttl":"1h"} blocks sit on system[] and
# messages[].content[] text blocks at positions that vary per launch, and
# metadata.user_id is a per-session blob (already stripped by volatileBodyFields).
# The ONE thing this module fixes across record+replay — so the test is not
# fooled by the property-(a)/(b) HOME traps the hooks module hit (ka-0000000461 /
# gd-1B3EYCqIXu) — is that BOTH bodies are hand-authored and identical except for
# the field under test; there is no `claude` launch, no HOME, no ambient settings.
#
# ── Legs (each a hard sub-gate; any FAIL reds the shard) ──────────────────────
#
#   1 volatile-HIT   HEADLINE. Record a body with cache_control on system block A
#                    + cc_version=<suffix1>. REPLAY a body that is logically
#                    identical but has cache_control MOVED to a different block AND
#                    cc_version=<suffix2> AND an added message-level breakpoint —
#                    the exact per-launch variation the client actually produces.
#                    Assert 200 + X-Claudemore-Replay: hit. The one assertion
#                    between a $0 deterministic tier and daily-red-on-`claude`-bump.
#   2 nonvolatile-MISS  NEGATIVE CONTROL (NOT optional). Over the SAME cassette,
#                    REPLAY a body that changes a NON-volatile field — real
#                    system-prompt TEXT — and assert 424 + miss. Without this,
#                    leg 1 passes against a canonicalizer that strips EVERYTHING
#                    (the over-stripping failure), which would collide unrelated
#                    requests and mis-serve cassettes. This leg fails iff the
#                    canonicalizer stops distinguishing genuine content.
#   3 stored-fidelity  The STORED cassette body must KEEP cache_control and the
#                    original cc_version=<suffix1> (stripping is hash-only, for
#                    fidelity — cassette.go). Reads the on-disk cassette JSON and
#                    asserts both survive. Guards the "strip the stored body too"
#                    regression that a header-only HIT/MISS pair cannot see.
#
# ── ANTI-VACUITY (mutation-verified; see the MR PROOF block) ──────────────────
#
#   • stripVolatileForHash gutted to `return v, false`  → leg 1 FAILS (HIT→424).
#   • canonicalizer made to strip all content (or the neg-control fixture made
#     volatile)                                          → leg 2 FAILS (MISS→200).
#   • cache_control/cc_version stripped from the STORED body too
#                                                        → leg 3 FAILS.
# Each leg reds a DISTINCT regression; none is decorative.
#
# ── Skip-clean contract ───────────────────────────────────────────────────────
#
# The Go toolchain + cmd/claudemore source + curl + python3 are present in every
# e2e shard container (same toolchain as claudemore-replay-fail-on-miss, no
# `claude` binary needed). If any is absent the module SKIPS CLEANLY (exit 0) with
# a LOUD banner via E2E_SKIP_SENTINEL — an absent toolchain must never red the
# suite (ka-0000000989). When all are present, every leg is a HARD assertion.
#
# Standalone runnable:
#   ./gardenio/tests/e2e/scripts/test-claudemore-replay-volatile-hash.sh

set -uo pipefail

# ── Config ────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# scripts/ -> e2e/ -> tests/ -> gardenio/ -> controller
CONTROLLER_DIR="${CONTROLLER_DIR:-}"
if [ -z "$CONTROLLER_DIR" ]; then
  CONTROLLER_DIR="$(cd "$SCRIPT_DIR/../../../controller" 2>/dev/null && pwd)" || CONTROLLER_DIR=""
fi
GO_BIN="${GO_BIN:-go}"
WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/claudemore-volatile.XXXXXX")"

# A closed port for the REPLAY upstream: nothing listens on 127.0.0.1:1, so if a
# miss ever regressed to a LIVE fall-through it would hit connection-refused
# instead of silently succeeding (the same anti-vacuity control the fail-on-miss
# module uses). REPLAY must never contact an upstream.
CLOSED_UPSTREAM="http://127.0.0.1:1"
# A distinctive probe model so the cassette key is unambiguous and cannot collide
# with an incidental default. Clean header token (no spaces/control chars).
PROBE_MODEL="claudemore-volatile-probe"
# The two per-launch cc_version suffixes (mirrors the real .ab5/.160/.172 the
# client rolls). Different between record and replay; must be normalized away.
CC_SUFFIX_RECORD="2.1.211.ab5"
CC_SUFFIX_REPLAY="2.1.211.160"
# A marker in the (non-volatile) system prompt content so the stored body can be
# asserted to carry real content, and so the negative-control's DIFFERENT content
# is unambiguous.
SYS_MARKER="VOLATILE_HASH_E2E_SYS_MARKER"

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
  blue "SKIP claudemore-replay-volatile-hash: $1"
  blue "  This module builds cmd/claudemore from source and drives its record→replay"
  blue "  request-hash canonicalization with curl; without the toolchain there is"
  blue "  nothing to drive. Skipping cleanly (exit 0) — an absent toolchain must"
  blue "  never red the suite (see test-claudemore-replay-fail-on-miss / hooks)."
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
# immediately follow the name.
header_value() {
  grep -i "^$2:" "$1" 2>/dev/null | head -1 | sed -e "s/^[^:]*: *//" -e 's/\r$//'
}

# body_message — emit a Messages-API request body (JSON on one line) shaped like
# the real `claude` wire: system[0] = the cc_version billing header, system[1..2]
# = content blocks (optionally cache_control'd), messages[0].content[] = the user
# turn (optionally cache_control'd), plus a per-session metadata.user_id.
#
# Args (positional):
#   $1 cc_suffix       — the cc_version=2.1.211.<X> suffix on system[0]
#   $2 sys_cc_block    — which system block carries cache_control: "1" or "2"
#   $3 msg_breakpoint  — "yes" adds cache_control to the last user block, else "no"
#   $4 sys_text        — the (NON-volatile) system prompt text on system[2]
#   $5 user_id         — the per-session metadata.user_id blob (volatile, stripped)
body_message() {
  local cc_suffix="$1" sys_cc_block="$2" msg_breakpoint="$3" sys_text="$4" user_id="$5"
  local cc='{"type":"ephemeral","ttl":"1h"}'
  local sys1_cc="" sys2_cc="" msg_cc=""
  [ "$sys_cc_block" = "1" ] && sys1_cc=",\"cache_control\":$cc"
  [ "$sys_cc_block" = "2" ] && sys2_cc=",\"cache_control\":$cc"
  [ "$msg_breakpoint" = "yes" ] && msg_cc=",\"cache_control\":$cc"
  cat <<JSON
{"model":"$PROBE_MODEL","max_tokens":16,"metadata":{"user_id":"$user_id"},"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=$cc_suffix; cc_entrypoint=sdk-cli;"},{"type":"text","text":"You are a Claude agent, built on Anthropic's Claude Agent SDK."$sys1_cc},{"type":"text","text":"$sys_text"$sys2_cc}],"messages":[{"role":"user","content":[{"type":"text","text":"<system-reminder>context</system-reminder>"},{"type":"text","text":"Say hi."$msg_cc}]}],"stream":true}
JSON
}

# post_messages <port> <body> <hdrdump> <bodyout> — POST a Messages-API request,
# echo the HTTP status. Captures response headers to <hdrdump>, body to <bodyout>.
post_messages() {
  curl -s -o "$4" -D "$3" -w '%{http_code}' -m 10 \
    -X POST "http://127.0.0.1:$1/v1/messages" \
    -H 'content-type: application/json' \
    --data "$2" 2>/dev/null
}

# ── Preflight: skip cleanly if any prerequisite is missing ──────────
blue "╔══════════════════════════════════════════════════════════╗"
blue "║  claudemore REPLAY volatile-hash e2e (ka-0000006222)      ║"
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

# ══ RECORD: emit ONE cassette via claudemore LIVE+record against a local stub ══
# The cassette is produced by claudemore's OWN recorder (not hand-dropped on disk),
# so the STORED shape and the recorded (model, hash) key are exactly what the
# producing path writes (ka-0000001521). The stub is a minimal fixed Anthropic
# response carrying a marker so leg 1's HIT can be confirmed by body, not just status.
blue "Recording a cassette via claudemore LIVE+record against a local stub..."
STUB="$WORKDIR/stub_upstream.py"
HIT_MARKER="CLAUDEMORE_VOLATILE_HIT_MARKER"
cat > "$STUB" <<PY
import http.server, json, os
PORT = int(os.environ["STUB_PORT"])
BODY = json.dumps({
    "id": "msg_volatile_stub",
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

CASS="$WORKDIR/cass"; mkdir -p "$CASS"
# RECORD body: cache_control on system BLOCK 1, NO message breakpoint,
# cc_version=<record suffix>, canonical system text, a per-session user_id.
REC_BODY="$(body_message "$CC_SUFFIX_RECORD" 1 no "$SYS_MARKER: canonical system prompt content." "session-RECORD-1111")"
STUB_PORT="$(free_port)"; REC_PORT="$(free_port)"

STUB_PORT="$STUB_PORT" python3 "$STUB" >"$WORKDIR/stub.log" 2>&1 &
echo $! > "$WORKDIR/stub.pid"
sleep 0.5

CLAUDEMORE_MODE=live \
CLAUDEMORE_LISTEN="127.0.0.1:$REC_PORT" \
CLAUDEMORE_UPSTREAM_URL="http://127.0.0.1:$STUB_PORT" \
CLAUDEMORE_CASSETTE_DIR="$CASS" \
LOG_LEVEL=error \
  "$CM_BIN" >"$WORKDIR/cm-record.log" 2>&1 &
echo $! > "$WORKDIR/record.pid"

REC_OK=1
if ! wait_http "http://127.0.0.1:$REC_PORT/healthz"; then
  red "  ✗ claudemore(live+record) did not come up"; REC_OK=0
fi
if [ "$REC_OK" -eq 1 ]; then
  REC_STATUS="$(post_messages "$REC_PORT" "$REC_BODY" "$WORKDIR/rec.hdr" "$WORKDIR/rec.body")"
  [ "$REC_STATUS" = "200" ] || { red "  ✗ record POST did not return 200 (got '${REC_STATUS:-<none>}')"; REC_OK=0; }
fi
sleep 1  # let the async recorder flush its Put
# Stop the recorder GRACEFULLY (SIGTERM) and wait for the FS-store drain before
# REPLAY reads the dir — the recorder enqueues to an async worker (gd-1B3EYCqIXu).
kill -TERM "$(cat "$WORKDIR/record.pid" 2>/dev/null)" >/dev/null 2>&1 || true
for _ in $(seq 1 40); do kill -0 "$(cat "$WORKDIR/record.pid" 2>/dev/null)" 2>/dev/null || break; sleep 0.25; done
kill "$(cat "$WORKDIR/stub.pid" 2>/dev/null)" >/dev/null 2>&1 || true
sleep 0.3

# Sanity: a cassette JSON must exist on disk, else every leg is vacuous. Fail
# LOUD, never skip — a broken record path is a real regression.
CASS_COUNT=0
if [ "$REC_OK" -eq 1 ]; then
  CASS_COUNT="$(find "$CASS" -type f -name '*.json' 2>/dev/null | wc -l | tr -d ' ')"
fi
if [ "$REC_OK" -ne 1 ] || [ "${CASS_COUNT:-0}" -lt 1 ]; then
  fail "record setup: a cassette was written to disk" \
       "no cassette under $CASS after LIVE+record (found ${CASS_COUNT:-0}); cannot assert the volatile-hash contract. Record log: $(tail -3 "$WORKDIR/cm-record.log" 2>/dev/null | tr '\n' ' ')"
else
  dim "  recorded $CASS_COUNT cassette(s) under $CASS"
  CASS_FILE="$(find "$CASS" -type f -name '*.json' 2>/dev/null | head -1)"
  echo ""

  # ══ REPLAY over the recorded cassette, upstream = CLOSED port ══════════════
  blue "Booting claudemore REPLAY over the recorded cassette (upstream = closed port)..."
  REP_PORT="$(free_port)"
  CLAUDEMORE_MODE=replay \
  CLAUDEMORE_LISTEN="127.0.0.1:$REP_PORT" \
  CLAUDEMORE_UPSTREAM_URL="$CLOSED_UPSTREAM" \
  CLAUDEMORE_CASSETTE_DIR="$CASS" \
  LOG_LEVEL=info \
    "$CM_BIN" >"$WORKDIR/cm-replay.log" 2>&1 &
  echo $! > "$WORKDIR/replay.pid"
  if ! wait_http "http://127.0.0.1:$REP_PORT/healthz"; then
    fail "replay boot: claudemore(replay) came up" \
         "did not answer /healthz: $(tail -5 "$WORKDIR/cm-replay.log" 2>/dev/null | tr '\n' ' ')"
  else
    dim "  up on 127.0.0.1:$REP_PORT"
    echo ""

    # ── Leg 1: HEADLINE — volatile-only-different body REPLAYs a HIT ──────────
    blue "1. volatile-HIT: a body differing ONLY in cache_control PLACEMENT + cc_version + a message breakpoint → 200 + hit..."
    # REPLAY body: SAME logical content, but cache_control MOVED to system BLOCK 2,
    # a message-level breakpoint ADDED, cc_version=<replay suffix>, and a DIFFERENT
    # per-session user_id. Every difference is a per-launch-volatile field.
    VOL_BODY="$(body_message "$CC_SUFFIX_REPLAY" 2 yes "$SYS_MARKER: canonical system prompt content." "session-REPLAY-9999")"
    VOL_HDR="$WORKDIR/vol.hdr"; VOL_BODY_OUT="$WORKDIR/vol.body"
    VOL_STATUS="$(post_messages "$REP_PORT" "$VOL_BODY" "$VOL_HDR" "$VOL_BODY_OUT")"
    VOL_MARKER="$(header_value "$VOL_HDR" "X-Claudemore-Replay")"
    _leg1_ok=1
    if [ "$VOL_STATUS" != "200" ]; then
      fail "leg1: volatile-only-different body → 200" \
           "got '${VOL_STATUS:-<none>}' — the volatile fields (cache_control placement / cc_version) leaked into the request hash, so the record and replay bodies hashed differently and MISSED. If this is a 424 the stripping regressed (stripVolatileForHash / volatileNestedKeys / ccVersionRE in cassette.go); upstream was a CLOSED port so a LIVE fall-through cannot masquerade as a hit. Miss detail: $(header_value "$VOL_HDR" "X-Claudemore-Replay-Miss")"
      _leg1_ok=0
    fi
    if [ "$VOL_MARKER" != "hit" ]; then
      fail "leg1: X-Claudemore-Replay: hit" \
           "got '${VOL_MARKER:-<absent>}' — expected a cassette HIT for a body that differs from the recorded one ONLY in per-launch-volatile fields"
      _leg1_ok=0
    fi
    if ! grep -qF "$HIT_MARKER" "$VOL_BODY_OUT" 2>/dev/null; then
      fail "leg1: recorded response body served on the volatile HIT" \
           "the recorded marker '$HIT_MARKER' is absent from the replayed body — the cassette body did not round-trip on the volatile-differing request"
      _leg1_ok=0
    fi
    if [ "$_leg1_ok" -eq 1 ]; then
      pass "volatile-only-different body (cache_control moved system[1]→system[2] + msg breakpoint added + cc_version $CC_SUFFIX_RECORD→$CC_SUFFIX_REPLAY) → 200 + hit + recorded body (volatile-strip holds; REPLAY survives a per-launch field change)"
    fi

    # ── Leg 2: NEGATIVE CONTROL — a real content difference MISSes ────────────
    blue "2. nonvolatile-MISS (negative control): a body differing in real system-prompt TEXT → 424 + miss..."
    # Same volatile shape as the record body (so ONLY the non-volatile field
    # differs), but with genuinely DIFFERENT system-prompt content. If the
    # canonicalizer over-stripped (erased real content), this would HIT — that is
    # the failure this control exists to catch.
    NEG_BODY="$(body_message "$CC_SUFFIX_RECORD" 1 no "$SYS_MARKER: DIFFERENT system prompt content entirely." "session-RECORD-1111")"
    NEG_HDR="$WORKDIR/neg.hdr"; NEG_BODY_OUT="$WORKDIR/neg.body"
    NEG_STATUS="$(post_messages "$REP_PORT" "$NEG_BODY" "$NEG_HDR" "$NEG_BODY_OUT")"
    NEG_MARKER="$(header_value "$NEG_HDR" "X-Claudemore-Replay")"
    _leg2_ok=1
    if [ "$NEG_STATUS" != "424" ]; then
      fail "leg2: real-content-different body → 424" \
           "got '${NEG_STATUS:-<none>}' — a body that changes a NON-volatile field (system-prompt TEXT) must NOT hit the recorded cassette. A 200 here means the canonicalizer is stripping genuine content, not just volatile fields — it would collide unrelated requests and mis-serve cassettes."
      _leg2_ok=0
    fi
    if [ "$NEG_MARKER" != "miss" ]; then
      fail "leg2: X-Claudemore-Replay: miss" \
           "got '${NEG_MARKER:-<absent>}' — expected a MISS for a genuinely-different request body"
      _leg2_ok=0
    fi
    if [ "$_leg2_ok" -eq 1 ]; then
      pass "real-content-different body → 424 + miss (the strip is volatile-ONLY: it does NOT erase genuine system-prompt content — leg 1's HIT is not a strip-everything artifact)"
    fi

    # ── Leg 3: STORED-body fidelity — volatile fields KEPT on disk ────────────
    blue "3. stored-fidelity: the on-disk cassette KEEPS cache_control + the original cc_version (strip is hash-only)..."
    _leg3_ok=1
    if [ -z "${CASS_FILE:-}" ] || [ ! -f "$CASS_FILE" ]; then
      fail "leg3: cassette file present to inspect" "no cassette JSON found under $CASS"
      _leg3_ok=0
    else
      if ! grep -qF "cache_control" "$CASS_FILE" 2>/dev/null; then
        fail "leg3: stored body keeps cache_control" \
             "the recorded cassette dropped cache_control from its STORED body — stripping must be HASH-ONLY (cassette.go keeps the stored body for fidelity)"
        _leg3_ok=0
      fi
      if ! grep -qF "cc_version=$CC_SUFFIX_RECORD" "$CASS_FILE" 2>/dev/null; then
        fail "leg3: stored body keeps the original cc_version" \
             "the recorded cassette lost the original 'cc_version=$CC_SUFFIX_RECORD' from its STORED body — normalization must apply to the HASH body only, not the stored one"
        _leg3_ok=0
      fi
    fi
    if [ "$_leg3_ok" -eq 1 ]; then
      pass "stored cassette keeps cache_control + cc_version=$CC_SUFFIX_RECORD (volatile-strip is hash-only; the stored body stays faithful to the wire)"
    fi

    kill "$(cat "$WORKDIR/replay.pid" 2>/dev/null)" >/dev/null 2>&1 || true
  fi
fi
echo ""

# ── Summary ──────────────────────────────────────────────────────
blue "══════════════════════════════════════════════"
if [ "$FAIL" -eq 0 ]; then
  green "  All $PASS claudemore REPLAY volatile-hash assertions passed"
else
  red "  $PASS passed, $FAIL failed"
  echo ""
  for err in "${ERRORS[@]}"; do
    red "  - $err"
  done
fi
blue "══════════════════════════════════════════════"

[ "$FAIL" -eq 0 ] || exit 1
exit 0
