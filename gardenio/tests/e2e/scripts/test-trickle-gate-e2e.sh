#!/usr/bin/env bash
# test-trickle-gate-e2e.sh — E2E gate for `gb mr trickle-gate`'s trickle-
# eligibility bar (task ka-0000001182, follow-up to ka-0000004967; bundle
# ka-0000005053 §3/§5/§7 #4, master epic ka-0000005560).
#
# Bead ka-0000001182.
#
# ── THE GAP THIS CLOSES ───────────────────────────────────────────────────────
#
# `gb mr trickle-gate` merged (MR !7539) with substantial Go UNIT coverage
# (mr_trickle_gate_test.go for the pure EvaluateTrickleEligibility decision, and
# mr_trickle_test.go for the CLI resolvers). The unit tests exercise the PURE
# aggregator and the parse helpers in isolation; what they cannot reach is the
# LIVE CLI wiring that turns a decision into an EXIT CODE (0 eligible / 3 blocked
# under --enforce) after resolving every affordance's facts from a real GitLab
# API + the daemon-linked bead. This module drives the real `gb mr trickle-gate`
# binary against the shared daemon + a loopback mock GitLab so the whole
# resolve→decide→exit path is a behavioral fact, exactly as bundle §7 #4
# specifies: "a fixture MR missing each HARD item → exit 3 + matching reason;
# satisfied → exit 0".
#
# ── Legs (each a hard sub-gate; any FAIL reds the shard) ──────────────────────
#
#   SATISFIED (exit 0):
#     S1 eligible            An MR with EVERY HARD leg green (provenance complete,
#                            small single-concern diff, non-trivial dossier,
#                            coverage+code-review evidence supplied, security gate
#                            green, perf N/A) → --enforce exits 0 and --json
#                            reports eligible=true.
#
#   MISSING-EACH-HARD-ITEM (exit 3 + the blocking id named in the reason):
#     M-provenance   Drop the Dark-MR ancestry anchor → blocked on `provenance`.
#     M-small-diff   A >400-LOC diff with no Trickle-Size-Exception → `small-diff`.
#     M-rationale    A dossier whose Why is byte-equal to Summary → `rationale`.
#     M-coverage     No --coverage-pass flag (dark CI leg unfed) → `coverage`.
#     M-code-review  No --code-review flag → `code-review`.
#     M-security     No Security-Scan block in the description → `security`.
#     M-perf         A perf-sensitive path touched with no --perf-guard → `perf`.
#
#   CONTROL (proves the exit-3 legs are non-vacuous):
#     C1 advisory-default    The SAME fixture as a blocked leg, run WITHOUT
#                            --enforce, exits 0 (advisory rollout) while --json
#                            still reports eligible=false — proving exit 3 is the
#                            --enforce path firing, not an unconditional failure.
#
# ── Skip-clean contract ───────────────────────────────────────────────────────
#
# Builds the real cmd/gb from source (so the compiled aggregator + resolvers are
# what run), needs the Go toolchain + python3 (mock GitLab) + a reachable daemon
# (BEADS_HTTP_URL). Any absent → SKIP CLEANLY (exit 0) with a LOUD banner; an
# absent toolchain must never red the suite. When all are present every leg is a
# HARD assertion.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-trickle-gate-e2e.sh

set -uo pipefail

# ── Config ────────────────────────────────────────────────────────
CONTROLLER_DIR="${CONTROLLER_DIR:-gardenio/controller}"
KD_BIN="${KD_BIN:-kd}"
LOCK_PATH="${LOCK_PATH:-gardenio/locks/security-tools.lock}"
RUN_ID="${RUN_ID:-$$}"
TMP_PREFIX="/tmp/trickle-gate-e2e-${RUN_ID}"
GB_BIN=""
MOCK_PID=""
KD_ACTOR="${KD_ACTOR:-e2e-trickle-gate}"
export KD_ACTOR

# A project-scoped MR IID base unique-ish per run so parallel shards do not
# collide on the same mr_url bead. Each scenario gets its OWN iid (base+offset).
MR_IID_BASE=$(( 70000 + (RUN_ID % 8000) ))
MR_PROJECT="e2e-cds/trickle-gate"

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
  red " SKIP trickle-gate-e2e (LOUD): $why"
  red " This run did NOT assert the gb mr trickle-gate eligibility bar"
  red " (satisfied→exit 0; each HARD item missing→exit 3 + matching reason)."
  red " A green suite here proves NOTHING about those legs. The e2e shard carries"
  red " a Go toolchain + python3 + a reachable daemon — if this fires in CI, that"
  red " wiring regressed."
  red "════════════════════════════════════════════════════"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

have() { command -v "$1" >/dev/null 2>&1; }

cleanup() {
  [ -n "$MOCK_PID" ] && kill "$MOCK_PID" 2>/dev/null || true
  [ -n "$GB_BIN" ] && rm -f "$GB_BIN" 2>/dev/null || true
  # mk_mr_bead runs under `$(...)` command substitution, so any CREATED_BEADS
  # append it makes happens in a SUBSHELL and never reaches this parent-scope
  # array. So the authoritative reap is a label sweep: every fixture bead carries
  # the run-scoped label `e2e:trickle-gate-$RUN_ID`, and we close whatever still
  # matches it. The array is kept as a best-effort fast path for the same-shell
  # case (and harmless when empty).
  for id in "${CREATED_BEADS[@]:-}"; do
    [ -n "$id" ] && "$KD_BIN" close "$id" --actor "$KD_ACTOR" >/dev/null 2>&1 || true
  done
  if have python3; then
    "$KD_BIN" list --status=open -l "e2e:trickle-gate-$RUN_ID" --all-types --json 2>/dev/null \
      | python3 -c "import sys,json
try:
    for b in json.load(sys.stdin): print(b.get('id',''))
except Exception:
    pass" 2>/dev/null \
      | while read -r id; do
          [ -n "$id" ] && "$KD_BIN" close "$id" --actor "$KD_ACTOR" >/dev/null 2>&1 || true
        done
  fi
  rm -f "${TMP_PREFIX}"-*.log "${TMP_PREFIX}"-*.err "${TMP_PREFIX}-mock.py" "${TMP_PREFIX}-fixtures.json" 2>/dev/null || true
}
trap cleanup EXIT

# free_port finds an unused TCP port so parallel shards do not collide. Falls
# back to the base if ss is unavailable.
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

# mk_mr_bead creates a merge-request bead carrying mr_url + operated_by (the
# provenance facts the gate reads off the linked bead) and registers it for
# cleanup. Echoes the id (or '').
mk_mr_bead() {
  local title="$1" mr_url="$2"
  local id
  id=$("$KD_BIN" create "$title" --type=merge-request --no-parent \
        -l "e2e:trickle-gate-$RUN_ID" \
        -f "mr_url=${mr_url}" -f "operated_by=slack:U0OPERATOR1" \
        --actor "$KD_ACTOR" --json 2>/dev/null \
        | python3 -c "import sys,json
try: print(json.load(sys.stdin).get('id',''))
except Exception: print('')")
  [ -n "$id" ] && CREATED_BEADS+=("$id")
  printf '%s' "$id"
}

# jq_get pulls a top-level scalar from --json on stdin. '' on any parse error.
jq_get() {
  python3 -c "import sys,json
try:
    d=json.load(sys.stdin); v=d.get('$1','')
    print(v if v is not None else '')
except Exception:
    print('')" 2>/dev/null
}

echo "════════════════════════════════════════════════════"
echo " gb mr trickle-gate eligibility-bar E2E (ka-0000001182)"
echo " satisfied→exit 0 · each HARD item missing→exit 3 + matching reason"
echo "════════════════════════════════════════════════════"

# ── Preconditions (skip clean) ─────────────────────────────────────
have go        || skip_all "no go toolchain to build cmd/gb"
have python3   || skip_all "python3 not in PATH (mock GitLab + json)"
have "$KD_BIN" || skip_all "kd not in PATH (set KD_BIN)"
[ -d "$CONTROLLER_DIR/cmd/gb" ] || skip_all "cmd/gb source missing under $CONTROLLER_DIR"
[ -f "$LOCK_PATH" ] || skip_all "security-tools.lock missing at $LOCK_PATH (composed security leg)"
[ -n "${BEADS_HTTP_URL:-}" ] || skip_all "BEADS_HTTP_URL unset — no daemon to link the provenance bead against"
if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
  skip_all "daemon at ${BEADS_HTTP_URL} not reachable via kd"
fi
green "  Daemon reachable: ${BEADS_HTTP_URL}"

# ── Read the lock pins so the fixture Security-Scan block matches exactly ──────
# The composed security gate rejects any recorded scanner version that drifts
# from gardenio/locks/security-tools.lock, so the fixture must echo the live pins.
read_pin() { grep -E "^${1}=" "$LOCK_PATH" | head -1 | cut -d= -f2 | tr -d '"'"'"; }
GITLEAKS_V="$(read_pin GITLEAKS_VERSION)"
GOVULN_V="$(read_pin GOVULNCHECK_VERSION)"
OSV_V="$(read_pin OSV_SCANNER_VERSION)"
CARGO_AUDIT_V="$(read_pin CARGO_AUDIT_VERSION)"
CARGO_DENY_V="$(read_pin CARGO_DENY_VERSION)"
if [ -z "$GITLEAKS_V" ] || [ -z "$GOVULN_V" ] || [ -z "$OSV_V" ] || [ -z "$CARGO_AUDIT_V" ] || [ -z "$CARGO_DENY_V" ]; then
  skip_all "could not read all scanner pins from $LOCK_PATH — cannot build a matching Security-Scan block"
fi
SEC_SCAN_LINE="Security-Scan: deadbeef gitleaks=${GITLEAKS_V}=clean govulncheck=${GOVULN_V}=0-reachable osv-scanner=${OSV_V}=clean cargo-audit=${CARGO_AUDIT_V}=clean cargo-deny=${CARGO_DENY_V}=pass"
dim "  Security-Scan fixture pins: gitleaks=${GITLEAKS_V} govulncheck=${GOVULN_V} osv-scanner=${OSV_V} cargo-audit=${CARGO_AUDIT_V} cargo-deny=${CARGO_DENY_V}"

# ── Build the real gb binary from source ───────────────────────────
blue "[build] cmd/gb (from source — exercises the compiled aggregator + resolvers)"
GB_BIN="$(mktemp "${TMP_PREFIX}-gb.XXXXXX")"
BUILD_LOG="${TMP_PREFIX}-build.log"
if ( cd "$CONTROLLER_DIR" && CGO_ENABLED=0 go build -o "$GB_BIN" ./cmd/gb/ ) >"$BUILD_LOG" 2>&1; then
  pass "cmd/gb built from source"
else
  fail "build gb" "go build failed: $(tail -8 "$BUILD_LOG")"
  red " ✗ build failed — cannot exercise the trickle gate"
  exit 1
fi
echo

# ── Fixtures: a JSON map keyed by IID → the MR the mock serves ────────
# Each scenario writes one entry: {sha, description, changes:[{new_path,diff}],
# statuses:[{name,status}]}. The mock serves the MR object (GetMergeRequestByPath),
# the /changes payload (ListMergeRequestChangedFiles + GetMergeRequestDiffStats),
# and the commit statuses (ListCommitStatusesByPath, composed security leg).
FIXTURES="${TMP_PREFIX}-fixtures.json"
python3 -c "import json; open('$FIXTURES','w').write('{}')"

# fixture_put registers one scenario's MR into the fixtures map. Args:
#   iid, sha, description, changes_json, statuses_json
fixture_put() {
  python3 - "$FIXTURES" "$1" "$2" "$3" "$4" "$5" <<'PY'
import json, sys
path, iid, sha, desc, changes, statuses = sys.argv[1:7]
d = json.load(open(path))
d[str(iid)] = {"iid": int(iid), "sha": sha, "state": "opened",
               "description": desc,
               "changes": json.loads(changes),
               "statuses": json.loads(statuses)}
json.dump(d, open(path, "w"))
PY
}

# A green statuses payload: all five composed security status checks success.
GREEN_STATUSES='[{"name":"CI / Gitleaks","status":"success"},{"name":"CI / Go-Vuln","status":"success"},{"name":"CI / OSV-Scan","status":"success"},{"name":"CI / Cargo-Audit","status":"success"},{"name":"CI / Cargo-Deny","status":"success"}]'

# A tiny single-concern, non-perf, non-security-sensitive counted diff (1 file,
# well under the 400-LOC / 10-file budget).
SMALL_CHANGES='[{"new_path":"docs/trickle-fixture.md","diff":"@@ -0,0 +1,3 @@\n+one\n+two\n+three"}]'

# A well-formed non-trivial trust dossier (Summary != Why) + provenance anchors.
# Heredoc-built so the multi-line markdown survives into the fixture description.
dossier_desc() {
  # $1 = include Dark-MR anchor? (yes|no)
  # $2 = include Security-Scan block? (yes|no)
  # $3 = make Why byte-equal to Summary? (yes|no)
  local anchor="$1" secblock="$2" trivial="$3"
  local why="This peel is safe because the change is a docs-only fixture with no runtime surface; alternatives (leaving it undocumented) were rejected."
  [ "$trivial" = "yes" ] && why="Adds a fixture doc for the trickle bar."
  {
    echo "## Summary"
    echo "Adds a fixture doc for the trickle bar."
    echo ""
    echo "## Why"
    echo "$why"
    echo ""
    echo "## Alternatives considered"
    echo "Doing nothing; rejected because the bar needs a fixture."
    echo ""
    echo "## Risk assessment"
    echo "None — docs-only."
    echo ""
    echo "## Evidence"
    echo "See the e2e run."
    echo ""
    echo "## Provenance"
    echo "Operator: slack:U0OPERATOR1"
    echo "Bead: ka-0000001182"
    [ "$anchor" = "yes" ] && echo "Dark-MR: https://gitlab-dark.example/monorepo-dark/monorepo/-/merge_requests/1"
    echo ""
    [ "$secblock" = "yes" ] && echo "$SEC_SCAN_LINE"
  }
}

# ── Mock GitLab: serves the fixture map ──────────────────────────────
MOCK_PY="${TMP_PREFIX}-mock.py"
cat >"$MOCK_PY" <<'PY'
import http.server, socketserver, sys, json, re
FIXTURES = json.load(open(sys.argv[2]))
MR_RE   = re.compile(r"/merge_requests/(\d+)$")
CHG_RE  = re.compile(r"/merge_requests/(\d+)/changes$")
STAT_RE = re.compile(r"/repository/commits/([^/]+)/statuses")
def _mr_by_sha(sha):
    for v in FIXTURES.values():
        if v.get("sha") == sha:
            return v
    return None
class H(http.server.BaseHTTPRequestHandler):
    def _send(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def do_GET(self):
        path = self.path.split("?", 1)[0].rstrip("/")
        m = CHG_RE.search(path)
        if m:
            v = FIXTURES.get(m.group(1))
            if v is None:
                return self._send(404, {"message": "404 Not Found"})
            return self._send(200, {"iid": v["iid"], "sha": v["sha"],
                                    "changes": v.get("changes", [])})
        m = STAT_RE.search(path)
        if m:
            v = _mr_by_sha(m.group(1))
            return self._send(200, (v or {}).get("statuses", []))
        m = MR_RE.search(path)
        if m:
            v = FIXTURES.get(m.group(1))
            if v is None:
                return self._send(404, {"message": "404 Not Found"})
            return self._send(200, v)
        self._send(404, {"message": "404 Not Found"})
    def log_message(self, *a): pass
socketserver.TCPServer.allow_reuse_address = True
socketserver.TCPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
PY

MOCK_PORT="$(free_port 18860)"
MOCK_BASE="http://127.0.0.1:${MOCK_PORT}"
mr_url_for() { printf '%s/%s/-/merge_requests/%s' "$MOCK_BASE" "$MR_PROJECT" "$1"; }

# ── Build every scenario's fixture BEFORE starting the mock ─────────
# Each scenario: iid, sha, description, changes, statuses.
IID_S1=$(( MR_IID_BASE + 0 ));  SHA_S1="s1s1s1s1s1s1s1s1s1s1s1s1s1s1s1s1s1s1s1s1"
IID_PROV=$(( MR_IID_BASE + 1 )); SHA_PROV="prov00000000000000000000000000000000prov"
IID_SIZE=$(( MR_IID_BASE + 2 )); SHA_SIZE="size00000000000000000000000000000000size"
IID_RAT=$(( MR_IID_BASE + 3 ));  SHA_RAT="rat000000000000000000000000000000000rat0"
IID_COV=$(( MR_IID_BASE + 4 ));  SHA_COV="cov000000000000000000000000000000000cov0"
IID_CR=$(( MR_IID_BASE + 5 ));   SHA_CR="cr0000000000000000000000000000000000cr00"
IID_SEC=$(( MR_IID_BASE + 6 ));  SHA_SEC="sec000000000000000000000000000000000sec0"
IID_PERF=$(( MR_IID_BASE + 7 ));  SHA_PERF="perf0000000000000000000000000000000perf"

# A >400-LOC diff for the small-diff-fail case.
BIG_DIFF=$(python3 -c "print('@@ -0,0 +1,450 @@\n' + '\n'.join('+line %d' % i for i in range(450)))")
BIG_CHANGES=$(python3 -c "import json,sys; print(json.dumps([{'new_path':'docs/big-fixture.md','diff':sys.argv[1]}]))" "$BIG_DIFF")
# A perf-sensitive counted diff (touches a beads-viewer handler).
PERF_CHANGES='[{"new_path":"gardenio/controller/cmd/beads-viewer/fixture.go","diff":"@@ -0,0 +1,3 @@\n+package main\n+\n+// fixture"}]'

# S1 satisfied: all anchors + security block + non-trivial dossier + green statuses.
fixture_put "$IID_S1"   "$SHA_S1"   "$(dossier_desc yes yes no)"  "$SMALL_CHANGES" "$GREEN_STATUSES"
# M-provenance: drop the Dark-MR anchor (everything else green).
fixture_put "$IID_PROV" "$SHA_PROV" "$(dossier_desc no  yes no)"  "$SMALL_CHANGES" "$GREEN_STATUSES"
# M-small-diff: >400 LOC, no Trickle-Size-Exception (everything else green).
fixture_put "$IID_SIZE" "$SHA_SIZE" "$(dossier_desc yes yes no)"  "$BIG_CHANGES"   "$GREEN_STATUSES"
# M-rationale: Why byte-equal to Summary (everything else green).
fixture_put "$IID_RAT"  "$SHA_RAT"  "$(dossier_desc yes yes yes)" "$SMALL_CHANGES" "$GREEN_STATUSES"
# M-coverage: coverage flag withheld at call time (fixture is otherwise green).
fixture_put "$IID_COV"  "$SHA_COV"  "$(dossier_desc yes yes no)"  "$SMALL_CHANGES" "$GREEN_STATUSES"
# M-code-review: code-review flag withheld at call time.
fixture_put "$IID_CR"   "$SHA_CR"   "$(dossier_desc yes yes no)"  "$SMALL_CHANGES" "$GREEN_STATUSES"
# M-security: no Security-Scan block in the description.
fixture_put "$IID_SEC"  "$SHA_SEC"  "$(dossier_desc yes no  no)"  "$SMALL_CHANGES" "$GREEN_STATUSES"
# M-perf: touches a perf-sensitive path, no --perf-guard supplied.
fixture_put "$IID_PERF" "$SHA_PERF" "$(dossier_desc yes yes no)"  "$PERF_CHANGES"  "$GREEN_STATUSES"

python3 "$MOCK_PY" "$MOCK_PORT" "$FIXTURES" >"${TMP_PREFIX}-mock.log" 2>&1 &
MOCK_PID=$!
mock_up=0
for _i in $(seq 1 25); do
  if curl -s --max-time 2 "${MOCK_BASE}/api/v4/projects/x/merge_requests/${IID_S1}" 2>/dev/null | grep -q "$SHA_S1"; then
    mock_up=1; break
  fi
  sleep 0.2
done
[ "$mock_up" = "1" ] || skip_all "mock GitLab did not serve the fixture MR on ${MOCK_BASE}"
dim "  mock GitLab: ${MOCK_BASE} serves ${MR_PROJECT} fixtures"
# Point gb's GitLab client at the mock; the mock ignores auth.
export GITLAB_BASE_URL="$MOCK_BASE"
echo

# gate runs `gb mr trickle-gate <bead> [extra flags...]` and prints the exit code
# on the LAST line so callers read both the JSON and the code. --lock points at
# the real security-tools.lock (composed security leg).
gate() {
  local bead="$1"; shift
  local out rc
  out=$("$GB_BIN" mr trickle-gate "$bead" --json --lock "$LOCK_PATH" "$@" 2>>"${TMP_PREFIX}-gate.err")
  rc=$?
  printf '%s\n__RC__=%s\n' "$out" "$rc"
}
gate_json() { printf '%s' "$1" | sed '/^__RC__=/d'; }
gate_rc()   { printf '%s' "$1" | sed -n 's/^__RC__=//p'; }

# ── The all-HARD-green flag set (S1 + the blocked-leg fixtures reuse it,
#    each dropping exactly the flag under test) ──────────────────────
GREEN_FLAGS=(--coverage-pass true --e2e true --standalone-build true --code-review true --unresolved-findings 0)

# ═══════════════════════════════════════════════════════════════════
# S1 — fully-satisfied MR → exit 0 (eligible)
# ═══════════════════════════════════════════════════════════════════
blue "S1 — fully-satisfied MR under --enforce (expect exit 0, eligible=true)"
BEAD_S1=$(mk_mr_bead "TEST-DELETE-ME trickle-gate satisfied $RUN_ID" "$(mr_url_for "$IID_S1")")
if [ -z "$BEAD_S1" ]; then
  fail "S1 setup" "could not create satisfied MR bead"
else
  r=$(gate "$BEAD_S1" --enforce "${GREEN_FLAGS[@]}")
  j=$(gate_json "$r"); rc=$(gate_rc "$r")
  elig=$(printf '%s' "$j" | jq_get eligible)
  hp=$(printf '%s' "$j" | jq_get hard_pass); ht=$(printf '%s' "$j" | jq_get hard_total)
  if [ "$rc" = "0" ]; then
    pass "S1 exit 0 under --enforce (all HARD legs green)"
  else
    fail "S1 exit code" "want 0, got '$rc' — reason: $(printf '%s' "$j" | jq_get reason)"
  fi
  if [ "$elig" = "True" ] || [ "$elig" = "true" ]; then
    pass "S1 --json eligible=true (${hp}/${ht} HARD passing)"
  else
    fail "S1 eligible" "want eligible=true, got '$elig' — reason: $(printf '%s' "$j" | jq_get reason)"
  fi
fi
echo

# ═══════════════════════════════════════════════════════════════════
# Missing-each-HARD-item → exit 3 + the blocking id named in the reason
# ═══════════════════════════════════════════════════════════════════
# assert_blocked <label> <bead> <expect-id> <flags...>
assert_blocked() {
  local label="$1" bead="$2" want="$3"; shift 3
  local r j rc elig reason
  r=$(gate "$bead" --enforce "$@")
  j=$(gate_json "$r"); rc=$(gate_rc "$r")
  elig=$(printf '%s' "$j" | jq_get eligible)
  reason=$(printf '%s' "$j" | jq_get reason)
  if [ "$rc" = "3" ]; then
    pass "$label: exit 3 under --enforce"
  else
    fail "$label exit code" "want 3, got '$rc' (eligible=$elig reason=$reason)"
  fi
  if [ "$elig" = "False" ] || [ "$elig" = "false" ]; then
    pass "$label: --json eligible=false"
  else
    fail "$label eligible" "want eligible=false, got '$elig'"
  fi
  case "$reason" in
    *"$want"*) pass "$label: reason names the blocking item '$want'" ;;
    *) fail "$label reason" "want reason to mention '$want', got: $reason" ;;
  esac
}

blue "M-* — each HARD item missing → exit 3 + matching reason"

BEAD_PROV=$(mk_mr_bead "TEST-DELETE-ME trickle-gate no-provenance $RUN_ID" "$(mr_url_for "$IID_PROV")")
[ -n "$BEAD_PROV" ] && assert_blocked "M-provenance" "$BEAD_PROV" "provenance" "${GREEN_FLAGS[@]}" \
  || fail "M-provenance setup" "could not create bead"

BEAD_SIZE=$(mk_mr_bead "TEST-DELETE-ME trickle-gate big-diff $RUN_ID" "$(mr_url_for "$IID_SIZE")")
[ -n "$BEAD_SIZE" ] && assert_blocked "M-small-diff" "$BEAD_SIZE" "small-diff" "${GREEN_FLAGS[@]}" \
  || fail "M-small-diff setup" "could not create bead"

BEAD_RAT=$(mk_mr_bead "TEST-DELETE-ME trickle-gate trivial-why $RUN_ID" "$(mr_url_for "$IID_RAT")")
[ -n "$BEAD_RAT" ] && assert_blocked "M-rationale" "$BEAD_RAT" "rationale" "${GREEN_FLAGS[@]}" \
  || fail "M-rationale setup" "could not create bead"

# M-coverage: drop --coverage-pass (leaves the coverage leg PENDING → blocks).
BEAD_COV=$(mk_mr_bead "TEST-DELETE-ME trickle-gate no-coverage $RUN_ID" "$(mr_url_for "$IID_COV")")
[ -n "$BEAD_COV" ] && assert_blocked "M-coverage" "$BEAD_COV" "coverage" \
  --e2e true --standalone-build true --code-review true --unresolved-findings 0 \
  || fail "M-coverage setup" "could not create bead"

# M-code-review: drop --code-review (leaves the code-review leg PENDING → blocks).
BEAD_CR=$(mk_mr_bead "TEST-DELETE-ME trickle-gate no-code-review $RUN_ID" "$(mr_url_for "$IID_CR")")
[ -n "$BEAD_CR" ] && assert_blocked "M-code-review" "$BEAD_CR" "code-review" \
  --coverage-pass true --e2e true --standalone-build true \
  || fail "M-code-review setup" "could not create bead"

# M-security: no Security-Scan block in the description → composed security fails.
BEAD_SEC=$(mk_mr_bead "TEST-DELETE-ME trickle-gate no-security-block $RUN_ID" "$(mr_url_for "$IID_SEC")")
[ -n "$BEAD_SEC" ] && assert_blocked "M-security" "$BEAD_SEC" "security" "${GREEN_FLAGS[@]}" \
  || fail "M-security setup" "could not create bead"

# M-perf: perf-sensitive path touched, no --perf-guard → perf triggered-but-pending.
BEAD_PERF=$(mk_mr_bead "TEST-DELETE-ME trickle-gate perf-unfed $RUN_ID" "$(mr_url_for "$IID_PERF")")
[ -n "$BEAD_PERF" ] && assert_blocked "M-perf" "$BEAD_PERF" "perf" "${GREEN_FLAGS[@]}" \
  || fail "M-perf setup" "could not create bead"
echo

# ═══════════════════════════════════════════════════════════════════
# C1 — advisory default (no --enforce) → exit 0 even when blocked
# ═══════════════════════════════════════════════════════════════════
blue "C1 — advisory rollout control: a blocked MR WITHOUT --enforce exits 0 (eligible=false)"
if [ -n "${BEAD_PROV:-}" ]; then
  r=$(gate "$BEAD_PROV" "${GREEN_FLAGS[@]}")   # note: no --enforce
  j=$(gate_json "$r"); rc=$(gate_rc "$r")
  elig=$(printf '%s' "$j" | jq_get eligible)
  if [ "$rc" = "0" ] && { [ "$elig" = "False" ] || [ "$elig" = "false" ]; }; then
    pass "C1 advisory: same fixture without --enforce → exit 0 but eligible=false (exit 3 is the --enforce path)"
  else
    fail "C1 advisory" "want exit 0 + eligible=false, got rc=$rc eligible=$elig"
  fi
else
  fail "C1 setup" "no provenance-fixture bead to reuse"
fi
echo

# ── Summary ────────────────────────────────────────────────────────
blue "════════════════════════════════════════════════════"
total=$((PASS + FAIL))
if [ "$FAIL" -eq 0 ]; then
  green " ✓ All $total trickle-gate eligibility-bar assertions passed"
else
  red " ✗ $FAIL/$total assertions failed"
  for err in "${ERRORS[@]}"; do red "   - $err"; done
fi
blue "════════════════════════════════════════════════════"

[ "$FAIL" -eq 0 ]
