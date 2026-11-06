#!/usr/bin/env bash
# test-mr-close-live-recheck.sh — `kd close` merge→close race live-recheck E2E
# (task ka-0000007123, coverage-backfill epic ka-0000000958, fix bug
# ka-0000004113 / task gd-8CFK5lJxkn, MR !6776).
#
# ── The gap ───────────────────────────────────────────────────────────────
#
# `kd close` on a bead with a linked MR used to refuse purely on the CANONICAL
# type=merge-request bead, which is a bridge PROJECTION that trails a synchronous
# `glab mr merge` by tens of seconds. MR !6776 added an authoritative LIVE GitLab
# re-check (kbeads/cmd/kd/close_mr_live.go) as the LAST word before a refusal:
# only a positive `merged` acquits; every failure mode (no token, network error,
# non-200, unparseable body) degrades to mrLiveUnknown and preserves the old
# projection-only refusal.
#
# Verified HEAD 2026-08-05 (BOTH e2e suites grepped — advice ka-0000000850):
#   `has not caught up` / false-refuse / just-merged / unmerged+live-recheck
#   => 0 files in gardenio/tests/e2e/scripts/ AND gardenio/tests/beads-viewer-e2e/.
# Coverage was UNIT-ONLY (close_open_mr_test.go, with checkMRLiveState swapped
# for a Go stub). The nearest module, test-mr-reopen-drift.sh, covers a DIFFERENT
# pair (close-time refusal on projection evidence + the reopen sweep) and
# predates the live re-check — the acquittal path was entirely unexercised e2e.
#
# ── Why this one matters (observed live, not hypothetical) ──────────────────
#
# The gardener hit the acquittal path in production this cycle: closing
# ka-0000005743 seconds after self-merging MR !6781 printed the "has not caught
# up yet; closing anyway" note — the new code acquitting a real close the OLD
# guard would have refused. EVERY crew that self-merges then immediately closes
# its bead (the standard ka-0000002230 landing flow — essentially every backfill
# crew this sprint) rides this path. A regression does not corrupt data; it
# hard-BLOCKS the fleet-wide self-merge-then-close flow, with a confusing refusal
# telling the agent to pass --allow-open-mr on a bead whose MR is already merged.
#
# ── What this drives that the unit tests CANNOT ─────────────────────────────
#
# The unit suite swaps the package var checkMRLiveState for a Go stub, so it
# never exercises fetchMRLiveState: the mr_url→GitLab endpoint derivation
# (mrURLPattern + url.PathEscape of the project path), the PRIVATE-TOKEN header
# (leg 6 — the stub 401s any other header name/value), the GITLAB_TOKEN→GLAB_TOKEN
# env fallback (leg 5 — the anti-vacuity control for that branch), the 200/state
# JSON decode, and the real beads-daemon reads (GetBead of the issue bead + the
# type=merge-request field-filter list findMRBead walks). This module drives the REAL `kd close`
# binary against a LIVE beads daemon (seeded exactly like the bridge would: an
# issue bead carrying mr_url + a canonical type=merge-request bead whose
# mr_state=opened) with GitLab itself replaced by a throwaway Python-stdlib HTTP
# stub the mr_url points at. Only the GitLab wire responses are synthetic; the
# close logic, the endpoint stitching, the token plumbing, and every daemon read
# are production code paths.
#
# ── Legs (each a hard sub-gate; any FAIL reddens the shard) ─────────────────
#
#   1  acquittal       projection says `opened`, live GitLab says `merged`,
#                      token present => close SUCCEEDS and emits the
#                      "has not caught up yet" note. ZERO-coverage path; the one
#                      that runs on every crew landing.
#   2  refusal-preserved  live GitLab says `opened` (agrees with the projection)
#                      => close still REFUSES. Guards the fix against
#                      over-acquitting into a close-anything regression, which
#                      would silently re-arm drift bug ka-0000003953.
#   3a degrade-no-token  projection `opened`, GitLab reachable, but NO token
#                      (GITLAB_TOKEN and GLAB_TOKEN both unset) => mrLiveUnknown
#                      => REFUSES (never acquits on absence of evidence).
#   3b degrade-http-500  projection `opened`, token present, GitLab answers 500
#                      => mrLiveUnknown => REFUSES.
#   3c degrade-junk-body  projection `opened`, token present, GitLab answers 200
#                      with a NON-JSON body => mrLiveUnknown => REFUSES.
#   4  override-honored  --allow-open-mr on a genuinely-open MR still CLOSES and
#                      stamps mr_close_override=true (the reconcile-sweep marker).
#   5  glab-fallback     projection `opened`, live `merged`, and GLAB_TOKEN is the
#                      ONLY token set (GITLAB_TOKEN unset) => the
#                      GITLAB_TOKEN→GLAB_TOKEN fallback (close_mr_live.go:68-70)
#                      authenticates and the close ACQUITTS. Anti-vacuity control:
#                      delete the fallback branch and this leg refuses (401).
#   6  private-token-hdr the live check sends `PRIVATE-TOKEN: <token>`
#                      (close_mr_live.go:85). The stub 401s any other header
#                      name/value, so a rename to Authorization reds legs 1/5; this
#                      leg asserts the recorded header value directly (name+value).
#
# Legs 2 + 3 are what make the module LOAD-BEARING (advice ka-0000001521): a
# "kd close exits 0" assertion is born-vacuous — it passes with the guard deleted
# entirely. They pin that the acquittal is NARROW: only a positive live `merged`
# acquits, and NO failure mode ever does. Legs 5 + 6 are the anti-vacuity controls
# for the token plumbing — without them the PRIVATE-TOKEN header name and the
# GLAB_TOKEN fallback are unasserted (a rename/deletion stays GREEN, degrading the
# live check silently-and-closed back to a projection-only refusal).
#
# ── Mutation proof (advice ka-0000001521 — re-run before every push) ─────────
#
#   • make mrStillOpenLive (close.go) return false unconditionally (fail-open):
#       legs 2, 3a, 3b, 3c go RED (each is a refusal the mutant would acquit).
#   • drop the acquittal — make mrStillOpenLive always `return true`:
#       leg 1 goes RED (the merged MR would be refused).
#   • rename `PRIVATE-TOKEN` to `Authorization` (or delete the Header.Set line) in
#       close_mr_live.go:85: legs 1 + 5 + 6 go RED (the stub 401s the unauth'd
#       request → mrLiveUnknown → refuse; leg 6's record shows only __ABSENT__).
#   • delete the GITLAB_TOKEN→GLAB_TOKEN fallback (close_mr_live.go:69-71): leg 5
#       goes RED (GLAB_TOKEN-only request goes unauthenticated → 401 → refuse).
#       Legs 1-4 + 6 stay GREEN — they set GITLAB_TOKEN, so only leg 5 catches this.
#   Pre-existing spend/drift modules (test-mr-reopen-drift.sh,
#   test-session-spend-attribution.sh) stay GREEN under both mutations — they do
#   not touch close_mr_live.go.
#
# ── Fixture shape (advice ka-0000001521: assert against what prod EMITS) ─────
#
# The GitLab body is the shape gitlab.com's REST v4 GET
# /projects/:id/merge_requests/:iid actually returns — a JSON object with a
# top-level `state` in {opened,merged,closed,locked}. fetchMRLiveState decodes
# exactly `{"state": "..."}`; the merged/opened spellings are GitLab's own. The
# canonical MR bead + issue bead are seeded with the SAME mr_* fields the
# gitlab-bridge writes (mr_url, mr_state), matching test-auto-close-cascade.sh's
# merge-request fixtures.
#
# ── Skip-clean contract ────────────────────────────────────────────────────
#
# kd (KD_BIN) + python3 are present in every e2e shard (build-kd rides every
# shard's use: list; python3 is a house dep, per test-release-status.sh /
# test-auto-close-cascade.sh). A missing kd, a missing python3, or an unreachable
# daemon is a LOUD skip (exit 0) — never a red — mirroring the
# test-mr-reopen-drift.sh / test-release-status.sh skip_all("loud") contract:
# this module asserts the close-time live re-check, not toolchain availability.
# When present, every leg is a HARD assertion.
#
# Usage:
#   BEADS_HTTP_URL=http://localhost:8080 BEADS_TOKEN=... \
#     ./gardenio/tests/e2e/scripts/test-mr-close-live-recheck.sh

set -uo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
KD_BIN="${KD_BIN:-kd}"
RUN_ID="${RUN_ID:-$$}"
PF_PID=""
STUB_PID=""

PASS=0
FAIL=0
ERRORS=()

# Fixture registry is a FILE, not an array: `ID=$(mk_bead ...)` runs mk_bead in a
# command-substitution SUBSHELL, so an array append would die with the subshell
# and leave teardown a silent no-op (the exact trap test-auto-close-cascade.sh
# documents). Appending to a file crosses the subshell boundary.
BEAD_REGISTRY="$(mktemp -t mrlive-beads.XXXXXX)"

# Per-run unique actor so our explicit closes stay distinguishable from a
# co-tenant shard's on the shared daemon (test-auto-close-cascade.sh discipline).
export KD_ACTOR="${KD_ACTOR:-e2e-rwx}-mrlive-${RUN_ID}"

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
have() { command -v "$1" >/dev/null 2>&1; }

# skip_all exits 0 (a missing toolchain/daemon must NOT red the suite) with a
# LOUD banner so a skip is never silently read as a pass.
skip_all() {
  local why="$1"
  red "════════════════════════════════════════════════════"
  red " SKIP mr-close-live-recheck (LOUD): $why"
  red " This run did NOT assert the gd-close live GitLab re-check (bug"
  red " ka-0000004113): the merge→close acquittal, the refusal-preserved"
  red " negative, the no-token / 500 / junk-body degradation legs, or the"
  red " --allow-open-mr override. A green suite here proves NOTHING about the"
  red " fix. kd + python3 ride every e2e shard — if this fires in CI, that"
  red " wiring regressed."
  red "════════════════════════════════════════════════════"
  cleanup
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

# json_field — project one dotted path out of a kd --json object; "" for missing.
json_field() {
  local json="$1" path="$2"
  echo "$json" | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    print(''); sys.exit(0)
for part in '$path'.split('.'):
    if isinstance(d, dict):
        d = d.get(part)
    else:
        d = None; break
print('' if d is None else d)
" 2>/dev/null || echo ""
}

bead_status() { json_field "$("$KD_BIN" show "$1" --json 2>/dev/null)" "status"; }
# bead_field — read one fields.<key> off a bead (mr_close_override etc.).
bead_field() {
  "$KD_BIN" show "$1" --json 2>/dev/null | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    print(''); sys.exit(0)
f = d.get('fields') or {}
print('' if f.get('$2') is None else f.get('$2'))
" 2>/dev/null || echo ""
}

# free_port asks the kernel for an unused TCP port. Bound immediately after by
# the stub, so the race window is negligible (test-release-status.sh pattern).
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
    curl -s -o /dev/null --max-time 1 "$url" 2>/dev/null && return 0
    sleep 0.1
  done
  return 1
}

# mk_bead — create a fixture and register it for cleanup.
#
# --no-parent is REQUIRED, not cosmetic: the daemon auto-parents a new bead to
# the creating agent's task bead (ka-0000007224), which would splice every
# fixture into a live epic's subtree on the shared daemon. project:e2e-test +
# fixture:e2e:mr-close-live label them so no bridge/sweep path mistakes a fixture
# for real work (the isE2EFixture guard, ka-0000000809).
mk_bead() {
  local title="$1" type="$2"; shift 2
  local out id
  out=$("$KD_BIN" create "$title" --type="$type" --no-parent \
    -l "e2e:mrlive-${RUN_ID}" -l "project:e2e-test" \
    -l "fixture:e2e:mr-close-live" "$@" --json 2>/dev/null)
  id=$(json_field "$out" "id")
  # Command substitution runs this in a subshell — the id must go to a file.
  [ -n "$id" ] && echo "$id" >> "$BEAD_REGISTRY"
  echo "$id"
}

# seed_pair — an issue bead carrying mr_url + a canonical merge-request bead with
# mr_state=opened sharing that url. Returns "ISSUE MRB" on one line. The mr_url's
# host:port is the stub GitLab, so the live re-check hits the stub.
seed_pair() {
  local mrurl="$1" tag="$2" issue mrb
  issue=$(mk_bead "TEST-DELETE-ME mrlive ${tag} issue ${RUN_ID}" "task" \
    -f "mr_url=${mrurl}")
  mrb=$(mk_bead "TEST-DELETE-ME mrlive ${tag} mrbead ${RUN_ID}" "merge-request" \
    -f "mr_url=${mrurl}" -f 'mr_state="opened"')
  echo "$issue $mrb"
}

cleanup() {
  if [ -n "$STUB_PID" ]; then kill "$STUB_PID" 2>/dev/null || true; fi
  # DELETE fixtures (inert) rather than close them — a close would itself exercise
  # the guard against whatever graph survives a failed run.
  if [ -s "$BEAD_REGISTRY" ]; then
    local id
    while IFS= read -r id; do
      [ -n "$id" ] && "$KD_BIN" delete "$id" >/dev/null 2>&1 || true
    done < <(tac "$BEAD_REGISTRY" 2>/dev/null || tail -r "$BEAD_REGISTRY" 2>/dev/null)
  fi
  rm -f "$BEAD_REGISTRY" 2>/dev/null || true
  rm -f "${HDR_RECORD:-}" 2>/dev/null || true
  if [ -n "$PF_PID" ]; then kill "$PF_PID" 2>/dev/null || true; fi
}
trap cleanup EXIT

echo "════════════════════════════════════════════════════"
echo " gd-close merge→close race live-recheck E2E (ka-0000007123 / bug ka-0000004113)"
echo " live GitLab \`merged\` acquits a lagging \`opened\` projection; every other verdict refuses"
echo "════════════════════════════════════════════════════"

# ── 0. Preconditions (skip LOUD, never red) ─────────────────────────
have "$KD_BIN" || skip_all "kd (KD_BIN=$KD_BIN) not on PATH"
have python3   || skip_all "python3 not on PATH (needed for the GitLab stub + JSON parsing)"
have curl      || skip_all "curl not on PATH (needed for the daemon/stub readiness probes)"

# ── 1. Resolve the live daemon ──────────────────────────────────────
# run-shard.sh exports BEADS_HTTP_URL + BEADS_TOKEN. Standalone, stand up our own
# port-forward so the module runs outside a shard.
if [ -z "${BEADS_HTTP_URL:-}" ]; then
  have kubectl || skip_all "no BEADS_HTTP_URL and kubectl not installed — cannot reach a beads daemon"
  blue "[daemon] port-forwarding ${NAMESPACE}/${BEADS_SVC}..."
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" 19198:8080 \
    >"/tmp/kd-pf-mrlive-${RUN_ID}.log" 2>&1 &
  PF_PID=$!
  export BEADS_HTTP_URL="http://localhost:19198"
  wait_http "${BEADS_HTTP_URL}/v1/health" 15 \
    || skip_all "daemon /v1/health never answered on ${BEADS_HTTP_URL}"
  if [ -z "${BEADS_TOKEN:-}" ]; then
    BEADS_TOKEN=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
      -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
    [ -n "$BEADS_TOKEN" ] && export BEADS_TOKEN
  fi
fi
if ! "$KD_BIN" list --status=open --limit 1 --json >/dev/null 2>&1; then
  skip_all "beads daemon unreachable at ${BEADS_HTTP_URL} (kd list failed) — this module asserts the close guard, not daemon liveness"
fi
dim "  daemon: ${BEADS_HTTP_URL}"
echo

# ── 2. Stand up the stub GitLab ─────────────────────────────────────
# One long-lived server whose verdict is selected per-request by a /<state>/
# leading PATH SEGMENT, so a single stub serves the merged / opened / http-500 /
# junk-body legs with no restart. The mr_url the runtime reads is
# http://127.0.0.1:<port>/<state>/<proj>/-/merge_requests/<iid> — mrURLPattern
# (`^https?://[^/]+/(.+?)/-/merge_requests/(\d+)`) captures "<state>/<proj>" as
# the project path, which fetchMRLiveState url.PathEscapes into
# /api/v4/projects/<state>%2F<proj>/merge_requests/<iid>. The stub keys off that
# leading <state> segment.
# The exact token close_mr_live.go must send in the PRIVATE-TOKEN header. Single
# source of truth: exported below into GITLAB_TOKEN / GLAB_TOKEN AND handed to the
# stub as its expected value, so leg 6 (header assertion) checks NAME *and* VALUE.
MRLIVE_TOKEN="e2e-mrlive-fake-token"
# The stub appends every PRIVATE-TOKEN header value it receives (or __ABSENT__) to
# this file; leg 6 asserts the runtime actually sent PRIVATE-TOKEN: <MRLIVE_TOKEN>.
HDR_RECORD="$(mktemp -t mrlive-hdr.XXXXXX)"

STUB_PY="$(mktemp -t mrlive-stub.XXXXXX.py)"
cat > "$STUB_PY" <<'PY'
# Throwaway GitLab REST v4 stub for the gd-close live-recheck E2E. The verdict is
# encoded in the request path's FIRST segment (merged|opened|http500|junk), which
# arrives url-encoded inside the /api/v4/projects/<project>/... path because the
# runtime PathEscapes the whole project path. Deterministic by construction — the
# whole point is a non-flaky verdict the live GitLab could never give.
#
# argv: <port> <expected-token> <header-record-file>. The stub AUTHENTICATES:
# gitlab.com's REST v4 returns 401 for a missing/mismatched PRIVATE-TOKEN, and so
# do we — this is what makes the header NAME load-bearing (gap 2a,
# close_mr_live.go:85). If the runtime renames the header to Authorization or drops
# the Header.Set line, `PRIVATE-TOKEN` arrives absent -> 401 -> non-200 ->
# mrLiveUnknown -> the acquittal legs (1, 5) REFUSE and go red. Every received
# header value is also logged to the record file so leg 6 can assert it directly.
import http.server, sys, json, urllib.parse as up

EXPECTED = sys.argv[2] if len(sys.argv) > 2 else ''
REC = sys.argv[3] if len(sys.argv) > 3 else ''

def verdict_of(path):
    # path e.g. /api/v4/projects/merged%2Facme%2Fapp/merge_requests/6781
    dec = up.unquote(path)
    parts = dec.strip('/').split('/')
    # find the segment right after 'projects'
    try:
        i = parts.index('projects')
        return parts[i + 1]  # "<state>" — the first sub-segment of the project path
    except (ValueError, IndexError):
        return ''

class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_GET(self):
        p = up.urlparse(self.path).path
        if '/api/v4/projects/' in p and '/merge_requests/' in p:
            tok = self.headers.get('PRIVATE-TOKEN')
            if REC:
                # Record NAME+VALUE so leg 6 asserts the runtime sent
                # PRIVATE-TOKEN: <EXPECTED>. Absent header -> __ABSENT__.
                with open(REC, 'a') as fh:
                    fh.write((tok if tok is not None else '__ABSENT__') + '\n')
            if tok != EXPECTED:
                # Missing/mismatched PRIVATE-TOKEN -> 401, exactly like gitlab.com.
                self.send_response(401)
                self.send_header('Content-Type', 'application/json'); self.end_headers()
                self.wfile.write(b'{"message":"401 Unauthorized"}'); return
            v = verdict_of(p)
            if v == 'http500':
                self.send_response(500); self.end_headers()
                self.wfile.write(b'{"message":"500 Internal Server Error"}'); return
            if v == 'junk':
                body = b'this is not json'
            elif v in ('merged', 'opened', 'closed', 'locked'):
                body = json.dumps({"state": v, "iid": 1}).encode()
            else:
                # Unknown verdict segment: 404 so a mis-seeded mr_url fails LOUD
                # rather than silently degrading to mrLiveUnknown.
                self.send_response(404); self.end_headers()
                self.wfile.write(('{"message":"unexpected project seg: %s"}' % v).encode()); return
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(body)))
            self.end_headers(); self.wfile.write(body); return
        self.send_response(404); self.end_headers()
        self.wfile.write(('{"message":"unexpected path: %s"}' % p).encode())

port = int(sys.argv[1])
http.server.HTTPServer(('127.0.0.1', port), H).serve_forever()
PY

STUB_PORT="$(free_port)"
python3 "$STUB_PY" "$STUB_PORT" "$MRLIVE_TOKEN" "$HDR_RECORD" >/dev/null 2>&1 & STUB_PID=$!
STUB_HOST="127.0.0.1:${STUB_PORT}"
# Readiness probe against a merged verdict path. It carries no PRIVATE-TOKEN, so
# the stub 401s it (recording __ABSENT__) — fine: wait_http accepts ANY HTTP
# status, and leg 6 asserts the token is PRESENT in the record, not that every
# line matched.
if ! wait_http "http://${STUB_HOST}/api/v4/projects/merged%2Fx/merge_requests/1" 50; then
  rm -f "$STUB_PY" 2>/dev/null || true
  skip_all "stub GitLab failed to come up on ${STUB_HOST} (python3 http.server unavailable?)"
fi
rm -f "$STUB_PY" 2>/dev/null || true
pass "stub GitLab up (${STUB_HOST})"
# The token fetchMRLiveState reads. Its PRESENCE separates leg 3a (no token) from
# the rest; its VALUE + header name are asserted by the stub's 401 + leg 6. Leg 5
# re-exports it as GLAB_TOKEN alone to drive the fallback branch.
export GITLAB_TOKEN="$MRLIVE_TOKEN"
unset GLAB_TOKEN 2>/dev/null || true
echo

# mrurl_for — build the seed mr_url whose leading project segment is <state>, so
# the stub returns that verdict. A per-run nonce in the iid keeps fixtures
# distinct across concurrent shards. RUN_ID may be any string (run-shard.sh
# passes $$, but a caller can pass anything), so hash it through cksum rather
# than arithmetic modulo — a non-numeric RUN_ID would abort `$(( … ))` under
# `set -u`. The iid is cosmetic (the stub keys only off the <state> segment); it
# just keeps concurrent fixtures' urls distinct.
IID_BASE=$(( ($(echo -n "$RUN_ID" | cksum | awk '{print $1}') % 9000) + 1000 ))
mrurl_for() { echo "http://${STUB_HOST}/$1/e2e-mrlive/-/merge_requests/$2"; }

# ── Leg 1: acquittal — projection opened, live merged => CLOSE + note ────────
blue "Leg 1: acquittal — live \`merged\` acquits a lagging \`opened\` projection"
URL1="$(mrurl_for merged $((IID_BASE + 1)))"
read -r I1 M1 <<<"$(seed_pair "$URL1" acquittal)"
if [ -z "$I1" ] || [ -z "$M1" ]; then
  fail "leg1 setup" "could not seed issue+mr beads (issue='$I1' mr='$M1')"
else
  dim "  issue=$I1 mrbead=$M1 mr_url=$URL1"
  OUT1="$("$KD_BIN" close "$I1" 2>&1)"; RC1=$?
  if [ "$RC1" -ne 0 ]; then
    fail "leg1 acquittal close succeeds" "close exited $RC1: $OUT1"
  else
    pass "acquittal close exits 0"
  fi
  # Match the acquittal note precisely: "…closing anyway" is emitted ONLY by
  # mrStillOpenLive on acquittal (close.go:207). "has not caught up yet" ALSO
  # appears in the REFUSAL's guidance text, so grepping that alone would false-
  # match a refusal — "closing anyway" is the unambiguous acquittal signature.
  if grep -q "is merged on GitLab.*closing anyway" <<<"$OUT1"; then
    pass "acquittal emits the 'is merged … closing anyway' note"
  else
    fail "acquittal note" "missing the 'is merged … closing anyway' note in: $OUT1"
  fi
  ST1="$(bead_status "$I1")"
  [ "$ST1" = "closed" ] && pass "acquittal bead is now closed" \
    || fail "acquittal bead closed" "status=$ST1 (want closed)"
  # A correctly-shipped bead must NOT be stamped mr_close_override — that marker
  # tells the reconcile sweep to stop re-verifying (a false stamp is real damage).
  OV1="$(bead_field "$I1" mr_close_override)"
  [ -z "$OV1" ] && pass "acquittal does NOT stamp mr_close_override (correctly-shipped, not an override)" \
    || fail "acquittal override stamp" "mr_close_override=$OV1 (want empty)"
fi
echo

# ── Leg 2: refusal-preserved — projection opened, live opened => REFUSE ──────
blue "Leg 2: refusal-preserved — live \`opened\` (agrees with projection) still refuses"
URL2="$(mrurl_for opened $((IID_BASE + 2)))"
read -r I2 M2 <<<"$(seed_pair "$URL2" refusal)"
if [ -z "$I2" ] || [ -z "$M2" ]; then
  fail "leg2 setup" "could not seed issue+mr beads"
else
  dim "  issue=$I2 mrbead=$M2 mr_url=$URL2"
  OUT2="$("$KD_BIN" close "$I2" 2>&1)"; RC2=$?
  if [ "$RC2" -eq 0 ]; then
    fail "leg2 refusal preserved" "close SUCCEEDED (rc=0) on a live-\`opened\` MR — the fix over-acquitted: $OUT2"
  else
    pass "live-\`opened\` MR is still refused (rc=$RC2)"
  fi
  grep -q "refusing to close" <<<"$OUT2" \
    && pass "refusal names the guard ('refusing to close')" \
    || fail "refusal message" "missing 'refusing to close' in: $OUT2"
  ST2="$(bead_status "$I2")"
  [ "$ST2" != "closed" ] && pass "refused bead stays un-closed (status=$ST2)" \
    || fail "leg2 bead un-closed" "bead is closed but the MR is live-open"
fi
echo

# ── Leg 3: degradation — mrLiveUnknown NEVER flips a verdict on its own ──────
# Each sub-leg keeps the projection at `opened` and makes the live check
# UNVERIFIABLE a different way; every one must fall back to the projection-only
# REFUSAL, never acquit. A fail-open here turns a data-integrity guard into a
# no-op the moment GitLab is unreachable — the worst outcome.

# 3a: no token → fetchMRLiveState returns mrLiveUnknown before any request.
blue "Leg 3a: degrade (no token) — unverifiable must REFUSE, never acquit"
URL3A="$(mrurl_for merged $((IID_BASE + 30)))"   # stub WOULD say merged, but no token means we never ask
read -r I3A M3A <<<"$(seed_pair "$URL3A" notoken)"
if [ -z "$I3A" ] || [ -z "$M3A" ]; then
  fail "leg3a setup" "could not seed issue+mr beads"
else
  dim "  issue=$I3A mr_url=$URL3A (GITLAB_TOKEN+GLAB_TOKEN unset for this close)"
  OUT3A="$(env -u GITLAB_TOKEN -u GLAB_TOKEN "$KD_BIN" close "$I3A" 2>&1)"; RC3A=$?
  if [ "$RC3A" -eq 0 ]; then
    fail "leg3a no-token refuses" "close SUCCEEDED without a token — a fail-OPEN: an unverifiable MR must NOT acquit: $OUT3A"
  else
    pass "no-token close is refused (mrLiveUnknown, rc=$RC3A)"
  fi
  # It must NOT print the acquittal note — the live check should never have run.
  # ("closing anyway" is the acquittal-only signature; see leg 1.)
  grep -q "closing anyway" <<<"$OUT3A" \
    && fail "leg3a note" "printed the acquittal note without a token — the check ran and acquitted" \
    || pass "no-token close prints NO acquittal note"
fi
echo

# 3b: token present, GitLab answers HTTP 500 → non-200 → mrLiveUnknown.
blue "Leg 3b: degrade (HTTP 500) — non-200 must REFUSE"
URL3B="$(mrurl_for http500 $((IID_BASE + 31)))"
read -r I3B M3B <<<"$(seed_pair "$URL3B" http500)"
if [ -z "$I3B" ] || [ -z "$M3B" ]; then
  fail "leg3b setup" "could not seed issue+mr beads"
else
  dim "  issue=$I3B mr_url=$URL3B (stub returns 500)"
  OUT3B="$("$KD_BIN" close "$I3B" 2>&1)"; RC3B=$?
  [ "$RC3B" -ne 0 ] && pass "HTTP-500 live check refuses (mrLiveUnknown, rc=$RC3B)" \
    || fail "leg3b 500 refuses" "close SUCCEEDED on a 500 — a non-200 must degrade to refusal, not acquit: $OUT3B"
  grep -q "closing anyway" <<<"$OUT3B" \
    && fail "leg3b note" "acquittal note printed on a 500 response" \
    || pass "HTTP-500 close prints NO acquittal note"
fi
echo

# 3c: token present, GitLab answers 200 with a NON-JSON body → decode fails →
# mrLiveUnknown.
blue "Leg 3c: degrade (unparseable body) — 200 + junk must REFUSE"
URL3C="$(mrurl_for junk $((IID_BASE + 32)))"
read -r I3C M3C <<<"$(seed_pair "$URL3C" junk)"
if [ -z "$I3C" ] || [ -z "$M3C" ]; then
  fail "leg3c setup" "could not seed issue+mr beads"
else
  dim "  issue=$I3C mr_url=$URL3C (stub returns 200 + non-JSON)"
  OUT3C="$("$KD_BIN" close "$I3C" 2>&1)"; RC3C=$?
  [ "$RC3C" -ne 0 ] && pass "unparseable-body live check refuses (mrLiveUnknown, rc=$RC3C)" \
    || fail "leg3c junk refuses" "close SUCCEEDED on an unparseable 200 body — must degrade to refusal: $OUT3C"
  grep -q "closing anyway" <<<"$OUT3C" \
    && fail "leg3c note" "acquittal note printed on an unparseable body" \
    || pass "unparseable-body close prints NO acquittal note"
fi
echo

# ── Leg 4: override still honored — --allow-open-mr closes + stamps ──────────
# The deliberate escape hatch must keep working on a GENUINELY open MR (live
# `opened`), and must stamp mr_close_override=true so the reconcile sweep leaves
# the close alone. This is the negative half of leg 1's "no stamp on acquittal":
# together they pin the stamp fires iff the guard actually fired.
blue "Leg 4: --allow-open-mr on a live-\`opened\` MR closes AND stamps mr_close_override"
URL4="$(mrurl_for opened $((IID_BASE + 4)))"
read -r I4 M4 <<<"$(seed_pair "$URL4" override)"
if [ -z "$I4" ] || [ -z "$M4" ]; then
  fail "leg4 setup" "could not seed issue+mr beads"
else
  dim "  issue=$I4 mrbead=$M4 mr_url=$URL4"
  OUT4="$("$KD_BIN" close "$I4" --allow-open-mr 2>&1)"; RC4=$?
  [ "$RC4" -eq 0 ] && pass "--allow-open-mr close exits 0 on a genuinely-open MR" \
    || fail "leg4 override closes" "close exited $RC4: $OUT4"
  ST4="$(bead_status "$I4")"
  [ "$ST4" = "closed" ] && pass "override bead is now closed" \
    || fail "leg4 bead closed" "status=$ST4 (want closed)"
  OV4="$(bead_field "$I4" mr_close_override)"
  [ "$OV4" = "true" ] && pass "override stamps mr_close_override=true (sweep marker)" \
    || fail "leg4 override stamp" "mr_close_override='$OV4' (want 'true')"
fi
echo

# ── Leg 5: GLAB_TOKEN-only fallback acquits — anti-vacuity for the fallback ──
# close_mr_live.go:68-70 reads GITLAB_TOKEN, then falls back to GLAB_TOKEN (the
# spelling `glab` itself uses — the exact caller who hits this race). Every other
# leg sets GITLAB_TOKEN, so the fallback branch was UNEXERCISED: delete it and
# leg 1 stays green. This leg is its anti-vacuity control — it drives the SAME
# `merged` acquittal path as leg 1 but with GLAB_TOKEN as the *only* token set.
# Deleting the fallback (or misreading GLAB_TOKEN) sends the request unauthenticated
# -> the stub 401s -> non-200 -> mrLiveUnknown -> REFUSE, reddening this leg.
blue "Leg 5: GLAB_TOKEN-only — the GITLAB_TOKEN→GLAB_TOKEN fallback acquits a lagging \`opened\`"
URL5="$(mrurl_for merged $((IID_BASE + 5)))"
read -r I5 M5 <<<"$(seed_pair "$URL5" glabfallback)"
if [ -z "$I5" ] || [ -z "$M5" ]; then
  fail "leg5 setup" "could not seed issue+mr beads (issue='$I5' mr='$M5')"
else
  dim "  issue=$I5 mrbead=$M5 mr_url=$URL5 (GITLAB_TOKEN unset, GLAB_TOKEN=<tok> for this close)"
  OUT5="$(env -u GITLAB_TOKEN GLAB_TOKEN="$MRLIVE_TOKEN" "$KD_BIN" close "$I5" 2>&1)"; RC5=$?
  if [ "$RC5" -ne 0 ]; then
    fail "leg5 GLAB_TOKEN fallback close succeeds" "close exited $RC5 with only GLAB_TOKEN set — the GITLAB_TOKEN→GLAB_TOKEN fallback did not authenticate (branch missing/broken?): $OUT5"
  else
    pass "GLAB_TOKEN-only close exits 0 (fallback authenticated)"
  fi
  if grep -q "is merged on GitLab.*closing anyway" <<<"$OUT5"; then
    pass "GLAB_TOKEN-only acquittal emits the 'is merged … closing anyway' note (fallback reached GitLab)"
  else
    fail "leg5 acquittal note" "missing the acquittal note with GLAB_TOKEN alone — the fallback token never reached the live check: $OUT5"
  fi
  ST5="$(bead_status "$I5")"
  [ "$ST5" = "closed" ] && pass "GLAB_TOKEN-only acquittal bead is now closed" \
    || fail "leg5 bead closed" "status=$ST5 (want closed)"
fi
echo

# ── Leg 6: PRIVATE-TOKEN header is sent with the right NAME + VALUE ──────────
# close_mr_live.go:85 sets `req.Header.Set("PRIVATE-TOKEN", token)`. The stub
# already 401s any request whose PRIVATE-TOKEN != MRLIVE_TOKEN, so a rename to
# Authorization (or a dropped Header.Set) reds legs 1 + 5 directly (401 → refuse).
# This leg is the explicit, unmissable assertion of the same fact: the record file
# the stub kept must contain at least one line that is EXACTLY the token we sent
# under the PRIVATE-TOKEN header — proving name AND value, not merely "some header".
blue "Leg 6: the live check sends PRIVATE-TOKEN: <token> (header name + value)"
if [ ! -s "$HDR_RECORD" ]; then
  fail "leg6 header record" "no PRIVATE-TOKEN header values were recorded — the stub was never hit by an authenticated live check (legs 1/5 should have)"
elif grep -qx "$MRLIVE_TOKEN" "$HDR_RECORD"; then
  pass "runtime sent PRIVATE-TOKEN: <token> — header name and value both correct"
else
  # Only __ABSENT__ / mismatched values were seen: the header was renamed, dropped,
  # or carries the wrong value. Show the distinct values for the failure message.
  seen="$(sort -u "$HDR_RECORD" | tr '\n' ',' | sed 's/,$//')"
  fail "leg6 PRIVATE-TOKEN header" "no request carried PRIVATE-TOKEN: <MRLIVE_TOKEN>; recorded header values were: [$seen] (header renamed/dropped in close_mr_live.go?)"
fi
echo

# ── Summary ───────────────────────────────────────────────────────
echo "════════════════════════════════════════════════════"
echo " mr-close-live-recheck Summary: $PASS passed, $FAIL failed"
echo "════════════════════════════════════════════════════"
if [ "$FAIL" -gt 0 ]; then
  for e in "${ERRORS[@]}"; do red "  - $e"; done
  exit 1
fi
green "All mr-close-live-recheck assertions passed."
exit 0
