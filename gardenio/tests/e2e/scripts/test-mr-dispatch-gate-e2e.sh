#!/usr/bin/env bash
# test-mr-dispatch-gate-e2e.sh — E2E gate for `gb mr dispatch-gate`'s per-MR
# dispatch COOLDOWN (task ka-0000001101, MR !6600) and its one-shot aging NUDGE
# on a persistently operator-gated MR (bug ka-0000007496, MR !6599).
#
# Bead ka-0000006726 (parent ka-0000000958, E2E coverage-gap sprint).
#
# ── THE GAP THIS CLOSES ───────────────────────────────────────────────────────
#
# Both features merged 2026-08-04 with substantial Go UNIT coverage
# (mr_dispatch_cooldown_test.go, mr_dispatch_gate_test.go) but zero behavioral
# e2e: a grep of gardenio/tests/e2e for dispatch.cooldown / aging.nudge returns
# empty. The unit tests exercise the PURE evaluators
# (EvaluateDispatchCooldown / evaluateAgingNudge) in isolation; what they cannot
# reach is the LIVE CLI wiring that turns a decision into an exit code, a stamp
# comment on the MR bead, and a one-shot marker + nudge comment on the parking
# handle — the legs that actually protect against the runaway re-dispatch storm.
# Driving the real `gb mr dispatch-gate` binary against the daemon is exactly how
# this backfill found that the one-shot marker field was never registered on the
# decision/escalation type configs, so the marker write 400'd and the "one-time"
# nudge re-fired every cycle (fixed in the same MR: config.go + a builtin-field
# guard test; leg B2 is the behavioral proof).
#
# ── Legs (each a hard sub-gate; any FAIL reds the shard) ──────────────────────
#
#   COOLDOWN (Scenario A — needs a mock GitLab for a stable head SHA):
#     A1 cooldown-suppress   Same head SHA as the last stamp + dispatched inside
#                            the 50m window → may_dispatch=false,
#                            suppressed_by=cooldown. The suppression leg is the
#                            one that guards the runaway re-dispatch storm.
#     A4 mock-control        KNOWN-GOOD CONTROL (advice ka-0000002586): the mock
#                            GitLab served the head SHA A1-A3 key on — proven by
#                            head_sha in --json matching the mock. Without it,
#                            A2/A3's may_dispatch=true could be a fail-OPEN on an
#                            unresolved SHA, not the cooldown logic under test.
#     A2 cooldown-expiry     Same head SHA but the last stamp is 60m old (past the
#                            50m window) → may_dispatch=true (periodic re-look).
#     A3 head-moved          Stamp records a DIFFERENT SHA than the MR's current
#                            head → may_dispatch=true (real new code re-crews
#                            immediately, regardless of the window).
#
#   AGING NUDGE (Scenario B — no GitLab; blocking-edge signal):
#     B1 nudge-fires-once    A task blocked by an open decision, aged past the
#                            (env-lowered) threshold → suppress + ONE nudge
#                            comment on the decision + the one-shot marker stamped.
#     B2 nudge-one-shot      A SECOND dispatch-gate on the same MR → still
#                            suppress, but NO second nudge (marker present) and
#                            still exactly ONE nudge comment on the handle. The
#                            one-shot idempotency across a real re-evaluation is
#                            the whole contract; a unit test asserting one call
#                            does not prove it survives a fresh CLI process AND
#                            a real daemon field-validation round-trip.
#     B3 nudge-threshold     KNOWN-GOOD CONTROL: a fresh park re-evaluated with
#                            the threshold raised WAY above the park age emits NO
#                            nudge — proving B1's nudge was the age gate firing,
#                            not an unconditional nudge-on-every-suppress.
#
# The aging-nudge age keys on the parking handle bead's created_at, which cannot
# be backdated on the shared daemon (the wall test-stale-reaper-grace-reconcile.sh
# documents). So the gate exposes GB_DISPATCH_GATE_NUDGE_AGE — a test-only
# threshold override (mirroring gb news' GB_NEWS_ALIVE_FRESHNESS_WINDOW) — that
# lets a freshly-filed handle cross the threshold and exercise the real fire-once
# lifecycle. B3 raises it back to prove the override is the age gate, not a bypass.
#
# ── Skip-clean contract ───────────────────────────────────────────────────────
#
# Builds the real cmd/gb from source (so the compiled cooldown/gate/nudge logic
# and the env seam are what run), needs the Go toolchain + python3 (mock GitLab)
# + a reachable daemon (BEADS_HTTP_URL). Any absent → SKIP CLEANLY (exit 0) with
# a LOUD banner; an absent toolchain must never red the suite. When all are
# present every leg is a HARD assertion.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-mr-dispatch-gate-e2e.sh

set -uo pipefail

# ── Config ────────────────────────────────────────────────────────
CONTROLLER_DIR="${CONTROLLER_DIR:-gardenio/controller}"
KD_BIN="${KD_BIN:-kd}"
RUN_ID="${RUN_ID:-$$}"
TMP_PREFIX="/tmp/mr-dispatch-gate-e2e-${RUN_ID}"
GB_BIN=""
MOCK_PID=""
KD_ACTOR="${KD_ACTOR:-e2e-mr-dispatch-gate}"
export KD_ACTOR

# A project-scoped MR IID unique-ish per run so parallel shards do not collide on
# the same mr_url bead. The mock serves this IID; the beads reference it.
MR_IID=$(( 90000 + (RUN_ID % 8000) ))
MR_PROJECT="e2e-cds/mr-dispatch-gate"
MR_URL=""   # finalized after the mock port is known

# The head SHA the mock serves; A1-A3 stamp comments key off this exact value.
MOCK_HEAD_SHA="cafe1234deadbeef5678cafe1234deadbeef5678"
# A deliberately-different SHA for the head-moved leg (A3).
STALE_HEAD_SHA="0000ffff0000ffff0000ffff0000ffff0000ffff"

PASS=0
FAIL=0
ERRORS=()
CREATED_BEADS=()

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

# skip_all exits 0 (a missing toolchain/dep/daemon must NOT red the suite) with a
# LOUD banner so a skip is never silently read as a pass.
skip_all() {
  local why="$1"
  red "════════════════════════════════════════════════════"
  red " SKIP mr-dispatch-gate-e2e (LOUD): $why"
  red " This run did NOT assert the gb mr dispatch-gate COOLDOWN"
  red " (same-SHA-in-window suppress; expiry + head-moved re-dispatch) nor the"
  red " one-shot aging NUDGE on a persistently operator-gated MR."
  red " A green suite here proves NOTHING about those legs. The e2e shard carries"
  red " a Go toolchain + python3 + a reachable daemon — if this fires in CI, that"
  red " wiring regressed."
  red "════════════════════════════════════════════════════"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

have() { command -v "$1" >/dev/null 2>&1; }

# jq_get pulls a top-level scalar (bool→'True'/'False', str, int) from --json on
# stdin. Returns '' on any parse error or missing key.
jq_get() {
  python3 -c "import sys,json
try:
    d=json.load(sys.stdin); v=d.get('$1','')
    print(v if v is not None else '')
except Exception:
    print('')" 2>/dev/null
}

# nudge_get pulls a field out of the nested \"nudge\" object (or '' if no nudge).
nudge_get() {
  python3 -c "import sys,json
try:
    d=json.load(sys.stdin); n=d.get('nudge') or {}; v=n.get('$1','')
    print(v if v is not None else '')
except Exception:
    print('')" 2>/dev/null
}

# has_nudge prints 'yes' when --json carries a non-empty nudge object, else 'no'.
has_nudge() {
  python3 -c "import sys,json
try:
    d=json.load(sys.stdin); print('yes' if d.get('nudge') else 'no')
except Exception:
    print('err')" 2>/dev/null
}

cleanup() {
  [ -n "$MOCK_PID" ] && kill "$MOCK_PID" 2>/dev/null || true
  [ -n "$GB_BIN" ] && rm -f "$GB_BIN" 2>/dev/null || true
  for id in "${CREATED_BEADS[@]:-}"; do
    [ -n "$id" ] && "$KD_BIN" close "$id" --actor "$KD_ACTOR" >/dev/null 2>&1 || true
  done
  rm -f "${TMP_PREFIX}"-*.log "${TMP_PREFIX}"-*.err "${TMP_PREFIX}-mock.py" 2>/dev/null || true
}
trap cleanup EXIT

# free_port finds an unused TCP port in a small range so parallel shards do not
# collide. Falls back to the base if ss is unavailable.
free_port() {
  local port="$1"
  if have ss; then
    while ss -lnt 2>/dev/null | awk '{print $4}' | grep -q ":${port}$"; do
      port=$((port + 1))
      [ "$port" -gt "$(( $1 + 200 ))" ] && break
    done
  fi
  printf '%s' "$port"
}

# rfc3339_ago prints an RFC3339 UTC timestamp N minutes in the past, for crafting
# a cooldown stamp whose dispatched_at is inside/outside the 50m window.
rfc3339_ago() {
  python3 -c "import datetime; print((datetime.datetime.now(datetime.timezone.utc) - datetime.timedelta(minutes=$1)).strftime('%Y-%m-%dT%H:%M:%SZ'))"
}

# mk_bead creates a bead and registers it for cleanup. Echoes the id (or '').
mk_bead() {
  local title="$1" type="$2"; shift 2
  local id
  id=$("$KD_BIN" create "$title" --type="$type" --no-parent \
        -l "e2e:mr-dispatch-gate-$RUN_ID" "$@" --actor "$KD_ACTOR" --json 2>/dev/null \
        | python3 -c "import sys,json
try: print(json.load(sys.stdin).get('id',''))
except Exception: print('')")
  [ -n "$id" ] && CREATED_BEADS+=("$id")
  printf '%s' "$id"
}

# stamp_cooldown writes a cooldown stamp comment on the MR bead, reproducing
# bridge.FormatCooldownStamp's layout EXACTLY — the marker on line 1, then the
# head_sha=/dispatched_at= tokens ParseCooldownStamp keys on — so the live gate
# reads it as a genuine prior dispatch. head_sha + dispatched_at are the two knobs
# the cooldown decision turns on.
COOLDOWN_MARKER="<!-- gardener-delivery-cooldown -->"
stamp_cooldown() {
  local bead="$1" sha="$2" at="$3"
  "$KD_BIN" comment add "$bead" \
    "${COOLDOWN_MARKER}
gardener-delivery dispatch cooldown: head_sha=${sha} dispatched_at=${at} (skip a same-SHA re-dispatch for 50m)" \
    --actor "$KD_ACTOR" >/dev/null 2>&1
}

# count_nudge_comments counts the aging-nudge comments on a handle bead. The nudge
# comment leads with the "aging nudge" phrase (formatDispatchGateNudge). Exactly
# one is the one-shot contract.
count_nudge_comments() {
  "$KD_BIN" show "$1" --json 2>/dev/null \
    | python3 -c "import sys,json
try:
    d=json.load(sys.stdin); cs=d.get('comments') or []
    print(sum(1 for c in cs if 'aging nudge' in (c.get('text') or '')))
except Exception:
    print(-1)"
}

# handle_marker prints the one-shot nudge marker field on the handle bead, or ''.
handle_marker() {
  "$KD_BIN" show "$1" --json 2>/dev/null \
    | python3 -c "import sys,json
try:
    d=json.load(sys.stdin); print((d.get('fields') or {}).get('gardener_delivery_gate_nudged_at',''))
except Exception:
    print('')"
}

echo "════════════════════════════════════════════════════"
echo " gb mr dispatch-gate COOLDOWN + one-shot NUDGE E2E (ka-0000006726)"
echo " cooldown suppress/expiry/head-moved · aging-nudge fire-once"
echo "════════════════════════════════════════════════════"

# ── Preconditions (skip clean) ─────────────────────────────────────
have go        || skip_all "no go toolchain to build cmd/gb"
have python3   || skip_all "python3 not in PATH (mock GitLab + json)"
have "$KD_BIN" || skip_all "kd not in PATH (set KD_BIN)"
[ -d "$CONTROLLER_DIR/cmd/gb" ] || skip_all "cmd/gb source missing under $CONTROLLER_DIR"
[ -n "${BEADS_HTTP_URL:-}" ] || skip_all "BEADS_HTTP_URL unset — no daemon to drive the gate against"
if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
  skip_all "daemon at ${BEADS_HTTP_URL} not reachable via kd"
fi
green "  Daemon reachable: ${BEADS_HTTP_URL}"

# ── Build the real gb binary from source ───────────────────────────
blue "[build] cmd/gb (from source — exercises the compiled cooldown/gate/nudge logic)"
GB_BIN="$(mktemp "${TMP_PREFIX}-gb.XXXXXX")"
BUILD_LOG="${TMP_PREFIX}-build.log"
if ( cd "$CONTROLLER_DIR" && CGO_ENABLED=0 go build -o "$GB_BIN" ./cmd/gb/ ) >"$BUILD_LOG" 2>&1; then
  pass "cmd/gb built from source"
else
  fail "build gb" "go build failed: $(tail -8 "$BUILD_LOG")"
  red " ✗ build failed — cannot exercise the dispatch gate"
  exit 1
fi
echo

# ── Mock GitLab: serves GET /api/v4/projects/:enc/merge_requests/:iid ──────────
# with a stable head SHA. The cooldown stage fetches this to compare against the
# stamped SHA; A4 asserts the served value round-trips into head_sha.
MOCK_PY="${TMP_PREFIX}-mock.py"
cat >"$MOCK_PY" <<PY
import http.server, socketserver, sys, json, re
SHA = sys.argv[2]
# Serve ANY /merge_requests/<iid> at the same head SHA, echoing the requested
# IID. Scenario A gives each leg (A1/A2/A3) its OWN iid/mr_url so the daemon's
# one-bead-per-mr_url invariant (kbeads migration 042, ka-0000005686) keeps them
# as SEPARATE beads — a single shared mr_url would collapse all three onto one
# bead and cross-contaminate their cooldown stamps (bug gd-gQJbDP80HW).
IID_RE = re.compile(r"/merge_requests/(\d+)")
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        m = IID_RE.search(self.path.rstrip("/"))
        if m:
            body = json.dumps({"iid": int(m.group(1)), "sha": SHA, "state": "opened"}).encode()
            self.send_response(200); self.send_header("Content-Type","application/json")
            self.send_header("Content-Length", str(len(body))); self.end_headers()
            self.wfile.write(body)
        else:
            self.send_response(404); self.end_headers()
    def log_message(self, *a): pass
socketserver.TCPServer.allow_reuse_address = True
socketserver.TCPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
PY
MOCK_PORT="$(free_port 18960)"
python3 "$MOCK_PY" "$MOCK_PORT" "$MOCK_HEAD_SHA" >"${TMP_PREFIX}-mock.log" 2>&1 &
MOCK_PID=$!
MOCK_BASE="http://127.0.0.1:${MOCK_PORT}"
# Per-leg mr_urls (distinct IIDs) so the daemon keeps A1/A2/A3 as SEPARATE
# merge-request beads. Before kbeads migration 042 the store had no uniqueness
# guard so three creates on ONE mr_url each landed a distinct bead; post-042 the
# store enforces one bead per mr_url and a same-url re-create returns the FIRST
# bead — collapsing the legs and cross-contaminating their cooldown stamps
# (bug gd-gQJbDP80HW: A2's 60m stamp landed on A1's bead alongside A1's 1m stamp,
# so LatestCooldownStamp picked the 1m one and A2 wrongly SUPPRESSED).
mr_url_for() { printf '%s/%s/-/merge_requests/%s' "$MOCK_BASE" "$MR_PROJECT" "$1"; }
MR_IID_A1="$MR_IID"
MR_IID_A2=$(( MR_IID + 10 ))
MR_IID_A3=$(( MR_IID + 11 ))
MR_URL="$(mr_url_for "$MR_IID_A1")"
MR_URL_A2="$(mr_url_for "$MR_IID_A2")"
MR_URL_A3="$(mr_url_for "$MR_IID_A3")"
mock_up=0
for _i in $(seq 1 25); do
  if curl -s --max-time 2 "${MOCK_BASE}/api/v4/projects/x/merge_requests/${MR_IID}" 2>/dev/null | grep -q "$MOCK_HEAD_SHA"; then
    mock_up=1; break
  fi
  sleep 0.2
done
[ "$mock_up" = "1" ] || skip_all "mock GitLab did not serve the MR on ${MOCK_BASE}"
dim "  mock GitLab: ${MOCK_BASE} serves MR !${MR_IID} head_sha=${MOCK_HEAD_SHA:0:12}"
# Point gb's GitLab client at the mock. resolveMRGitLabToken tolerates an empty
# token; the mock ignores auth.
export GITLAB_BASE_URL="$MOCK_BASE"
echo

# gate runs `gb mr dispatch-gate <arg> --json` with an optional nudge-age
# override; stdout is captured. Exit 3 = suppress (expected on suppress legs);
# assertions key on the JSON, not the exit code (set has no -e so exit 3 is fine).
gate() {
  local arg="$1" nudge_age="${2:-}"
  GB_DISPATCH_GATE_NUDGE_AGE="$nudge_age" "$GB_BIN" mr dispatch-gate "$arg" --json 2>>"${TMP_PREFIX}-gate.err"
}

# ═══════════════════════════════════════════════════════════════════
# Scenario A — per-MR dispatch COOLDOWN
# ═══════════════════════════════════════════════════════════════════
blue "Scenario A — per-MR dispatch cooldown (mock GitLab head_sha=${MOCK_HEAD_SHA:0:12})"

# A merge-request bead carrying mr_url → the arg gb resolves through for BOTH the
# head-SHA fetch (via mr_url) and the comment-stamp read. No producing task ⇒ the
# operator-gate signal is empty, so the cooldown is the ONLY thing that can
# suppress — isolating the leg under test.
MR_BEAD=$(mk_bead "TEST-DELETE-ME dispatch-gate cooldown MR $RUN_ID" merge-request -f "mr_url=${MR_URL}")
if [ -z "$MR_BEAD" ]; then
  fail "scenario-A-setup" "could not create merge-request bead"
else
  dim "  MR bead: $MR_BEAD  mr_url=${MR_URL}"

  # ── A1: same SHA, dispatched 1m ago (inside window) → SUPPRESS (cooldown) ──
  stamp_cooldown "$MR_BEAD" "$MOCK_HEAD_SHA" "$(rfc3339_ago 1)"
  a1=$(gate "$MR_BEAD")
  a1_may=$(printf '%s' "$a1" | jq_get may_dispatch)
  a1_by=$(printf '%s' "$a1" | jq_get suppressed_by)
  a1_head=$(printf '%s' "$a1" | jq_get head_sha)
  if [ "$a1_may" = "False" ] || [ "$a1_may" = "false" ]; then
    pass "A1 cooldown-suppress: same head SHA + dispatched 1m ago → may_dispatch=false"
  else
    fail "A1 cooldown-suppress" "want may_dispatch=false, got '$a1_may' (json: $(printf '%s' "$a1" | tr -d '\n' | head -c 300))"
  fi
  if [ "$a1_by" = "cooldown" ]; then
    pass "A1 suppressed_by=cooldown (not an operator gate)"
  else
    fail "A1 suppressed_by=cooldown" "want 'cooldown', got '$a1_by'"
  fi
  # ── A4: mock-control — the served SHA round-trips into head_sha ──
  if [ "$a1_head" = "$MOCK_HEAD_SHA" ]; then
    pass "A4 mock-control: gate resolved head_sha=${MOCK_HEAD_SHA:0:12} from the mock (A2/A3 are non-vacuous)"
  else
    fail "A4 mock-control" "gate head_sha='$a1_head' != mock '$MOCK_HEAD_SHA' — cooldown may be failing OPEN on an unresolved SHA, voiding A2/A3"
  fi

  # ── A2: same SHA, dispatched 60m ago (past the 50m window) → DISPATCH ──
  # A separate MR bead so its lone stamp (60m ago) is authoritative and never
  # shadowed by A1's newer one.
  MR_BEAD_A2=$(mk_bead "TEST-DELETE-ME dispatch-gate cooldown-expiry MR $RUN_ID" merge-request -f "mr_url=${MR_URL_A2}")
  if [ -n "$MR_BEAD_A2" ]; then
    stamp_cooldown "$MR_BEAD_A2" "$MOCK_HEAD_SHA" "$(rfc3339_ago 60)"
    a2=$(gate "$MR_BEAD_A2")
    a2_may=$(printf '%s' "$a2" | jq_get may_dispatch)
    if [ "$a2_may" = "True" ] || [ "$a2_may" = "true" ]; then
      pass "A2 cooldown-expiry: same head SHA but last dispatch 60m ago (> 50m) → may_dispatch=true"
    else
      fail "A2 cooldown-expiry" "want may_dispatch=true, got '$a2_may' (json: $(printf '%s' "$a2" | tr -d '\n' | head -c 300))"
    fi
  else
    fail "A2 setup" "could not create cooldown-expiry MR bead"
  fi

  # ── A3: stamp records a DIFFERENT SHA than the mock's head → DISPATCH ──
  MR_BEAD_A3=$(mk_bead "TEST-DELETE-ME dispatch-gate head-moved MR $RUN_ID" merge-request -f "mr_url=${MR_URL_A3}")
  if [ -n "$MR_BEAD_A3" ]; then
    # Stale sha, dispatched 1m ago (well inside the window) — only the SHA move
    # should re-dispatch.
    stamp_cooldown "$MR_BEAD_A3" "$STALE_HEAD_SHA" "$(rfc3339_ago 1)"
    a3=$(gate "$MR_BEAD_A3")
    a3_may=$(printf '%s' "$a3" | jq_get may_dispatch)
    a3_last=$(printf '%s' "$a3" | jq_get last_head_sha)
    if [ "$a3_may" = "True" ] || [ "$a3_may" = "true" ]; then
      pass "A3 head-moved: stamp sha != current head → may_dispatch=true (new code re-crews inside the window)"
    else
      fail "A3 head-moved" "want may_dispatch=true, got '$a3_may' (json: $(printf '%s' "$a3" | tr -d '\n' | head -c 300))"
    fi
    if [ "$a3_last" = "$STALE_HEAD_SHA" ]; then
      pass "A3 read back the stale stamp SHA (${STALE_HEAD_SHA:0:12}) — the move is real, not an empty read"
    else
      fail "A3 stamp read" "want last_head_sha=${STALE_HEAD_SHA:0:12}, got '$a3_last'"
    fi
  else
    fail "A3 setup" "could not create head-moved MR bead"
  fi
fi
echo

# ═══════════════════════════════════════════════════════════════════
# Scenario B — one-shot aging NUDGE on a persistently operator-gated MR
# ═══════════════════════════════════════════════════════════════════
blue "Scenario B — one-shot aging nudge (blocking-edge signal; GB_DISPATCH_GATE_NUDGE_AGE=1s)"

# A plain task blocked by an open decision = the operator merge-gate the dispatch
# gate suppresses on. The decision is freshly filed (created_at≈now), so only the
# env-lowered threshold lets its age cross — B3 proves that. (No mr_url on the
# task: an mr_url that resolves to no merge-request bead would error the resolve;
# the blocking EDGE alone drives suppression + nudge.)
NUDGE_IID=$(( MR_IID + 1 ))
TASK_BEAD=$(mk_bead "TEST-DELETE-ME dispatch-gate nudge task $RUN_ID" task)
DEC_BEAD=$(mk_bead "TEST-DELETE-ME merge MR !${NUDGE_IID}? dispatch-gate nudge $RUN_ID" decision \
  -f "prompt=Merge MR !${NUDGE_IID}?" -f 'options=[{"id":"a","label":"Merge"},{"id":"b","label":"Hold"}]')

if [ -z "$TASK_BEAD" ] || [ -z "$DEC_BEAD" ]; then
  fail "scenario-B-setup" "could not create task ($TASK_BEAD) + decision ($DEC_BEAD) beads"
else
  # dep add <bead> <depends-on>: the task is blocked BY the open decision, so the
  # gate's forward blocking-dep scan (blocks/blocked-by/depends-on) sees the open
  # decision parking the task.
  "$KD_BIN" dep add "$TASK_BEAD" "$DEC_BEAD" --type blocks >/dev/null 2>&1
  dim "  task $TASK_BEAD blocked by open decision $DEC_BEAD"
  # Let the park age past the 1s threshold (setup latency alone usually suffices;
  # the sleep gives a safe margin so age >> 1s regardless of daemon speed).
  sleep 2

  # ── B1: first dispatch-gate, threshold lowered to 1s → SUPPRESS + ONE nudge ──
  b1=$(gate "$TASK_BEAD" "1s")
  b1_may=$(printf '%s' "$b1" | jq_get may_dispatch)
  b1_by=$(printf '%s' "$b1" | jq_get suppressed_by)
  b1_posted=$(printf '%s' "$b1" | nudge_get posted)
  if [ "$b1_may" = "False" ] || [ "$b1_may" = "false" ]; then
    pass "B1 suppress: task blocked by open decision → may_dispatch=false"
  else
    fail "B1 suppress" "want may_dispatch=false, got '$b1_may' (json: $(printf '%s' "$b1" | tr -d '\n' | head -c 400))"
  fi
  if [ "$b1_by" = "operator-gate" ]; then
    pass "B1 suppressed_by=operator-gate"
  else
    fail "B1 suppressed_by" "want 'operator-gate', got '$b1_by'"
  fi
  if [ "$b1_posted" = "True" ] || [ "$b1_posted" = "true" ]; then
    pass "B1 nudge-posted: --json reports nudge.posted=true"
  else
    fail "B1 nudge-posted" "want nudge.posted=true, got '$b1_posted' — stderr: $(tail -2 "${TMP_PREFIX}-gate.err" 2>/dev/null)"
  fi
  # The durable side effects: exactly ONE nudge comment + the one-shot marker.
  # (The marker write is precisely the path that regressed when the field was
  # undeclared — a 400 there leaves the marker empty and re-nudges every cycle.)
  n1=$(count_nudge_comments "$DEC_BEAD")
  m1=$(handle_marker "$DEC_BEAD")
  if [ "$n1" = "1" ]; then
    pass "B1 exactly one aging-nudge comment landed on the decision"
  else
    fail "B1 nudge comment" "want exactly 1 aging-nudge comment on $DEC_BEAD, got '$n1'"
  fi
  if [ -n "$m1" ]; then
    pass "B1 one-shot marker persisted (gardener_delivery_gate_nudged_at=${m1})"
  else
    fail "B1 one-shot marker" "gardener_delivery_gate_nudged_at unset on $DEC_BEAD after a nudge — the marker write likely 400'd (is the field declared on type:decision? see kbeads config.go)"
  fi

  # ── B2: SECOND dispatch-gate, same lowered threshold → SUPPRESS, NO 2nd nudge ──
  b2=$(gate "$TASK_BEAD" "1s")
  b2_may=$(printf '%s' "$b2" | jq_get may_dispatch)
  b2_has=$(printf '%s' "$b2" | has_nudge)
  if [ "$b2_may" = "False" ] || [ "$b2_may" = "false" ]; then
    pass "B2 still-suppress: re-evaluation still may_dispatch=false (park unchanged)"
  else
    fail "B2 still-suppress" "want may_dispatch=false, got '$b2_may'"
  fi
  if [ "$b2_has" = "no" ]; then
    pass "B2 one-shot: second evaluation flags NO nudge (marker already set)"
  else
    fail "B2 one-shot" "want no nudge object on the second pass, got '$b2_has' (json: $(printf '%s' "$b2" | tr -d '\n' | head -c 400))"
  fi
  n2=$(count_nudge_comments "$DEC_BEAD")
  if [ "$n2" = "1" ]; then
    pass "B2 one-shot: STILL exactly one aging-nudge comment (no duplicate across a real re-run)"
  else
    fail "B2 one-shot comment count" "want exactly 1 aging-nudge comment after re-run, got '$n2' — a value of 2 is the 'nudge re-fires every cycle' regression"
  fi

  # ── B3: known-good control — raise the threshold above the park age → NO nudge ──
  # A fresh, untouched park (age is seconds) with a 24h threshold must NOT nudge —
  # proving B1's nudge was the AGE GATE firing, not an unconditional nudge.
  CTRL_IID=$(( MR_IID + 2 ))
  CTRL_TASK=$(mk_bead "TEST-DELETE-ME dispatch-gate nudge-control task $RUN_ID" task)
  CTRL_DEC=$(mk_bead "TEST-DELETE-ME merge MR !${CTRL_IID}? nudge-control $RUN_ID" decision \
    -f "prompt=Merge MR !${CTRL_IID}?" -f 'options=[{"id":"a","label":"Merge"},{"id":"b","label":"Hold"}]')
  if [ -n "$CTRL_TASK" ] && [ -n "$CTRL_DEC" ]; then
    "$KD_BIN" dep add "$CTRL_TASK" "$CTRL_DEC" --type blocks >/dev/null 2>&1
    b3=$(gate "$CTRL_TASK" "24h")
    b3_may=$(printf '%s' "$b3" | jq_get may_dispatch)
    b3_has=$(printf '%s' "$b3" | has_nudge)
    n3=$(count_nudge_comments "$CTRL_DEC")
    if { [ "$b3_may" = "False" ] || [ "$b3_may" = "false" ]; } && [ "$b3_has" = "no" ] && [ "$n3" = "0" ]; then
      pass "B3 nudge-threshold control: fresh park under a 24h threshold → suppress but NO nudge (B1 was the age gate)"
    else
      fail "B3 nudge-threshold control" "want suppress + no nudge + 0 comments; got may=$b3_may has_nudge=$b3_has comments=$n3"
    fi
  else
    fail "B3 setup" "could not create nudge-control task ($CTRL_TASK) + decision ($CTRL_DEC)"
  fi
fi
echo

# ── Summary ────────────────────────────────────────────────────────
blue "════════════════════════════════════════════════════"
total=$((PASS + FAIL))
if [ "$FAIL" -eq 0 ]; then
  green " ✓ All $total dispatch-gate cooldown + nudge assertions passed"
else
  red " ✗ $FAIL/$total assertions failed"
  for err in "${ERRORS[@]}"; do red "   - $err"; done
fi
blue "════════════════════════════════════════════════════"

[ "$FAIL" -eq 0 ]
