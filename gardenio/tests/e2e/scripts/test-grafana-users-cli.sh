#!/usr/bin/env bash
# test-grafana-users-cli.sh — `gb grafana users list` / `gb grafana users bump`
# CLI E2E (task ka-0000004197, coverage-backfill epic ka-0000000958).
#
# Closes the gap !6655 left open: `gb grafana users` (cmd/gb/grafana_users.go) —
# the list/bump-org-role affordance that reconciles already-provisioned Grafana
# users to a target org role (the "existing Viewers should be Admin like new
# users are" one-command reconcile, task ka-0000003341) — shipped with strong
# UNIT coverage (grafana_users_test.go, httptest fake) but ZERO end-to-end
# assertion driving the REAL `gb` binary through a real HTTP round-trip. The
# only live grafana e2e that touch the CLI — test-grafana-render-cli.sh
# (`gb grafana render`) and test-grafana-bridge.sh (dashboard SYNC) — do not
# exercise the users subcommands at all.
#
# ── Why a hermetic stub rig, not a live-cluster port-forward ──────────────────
#
# Two hard constraints shape this module — identical to test-grafana-render-cli:
#
#   1. The org-users write endpoint (PATCH /api/org/users/<id>) mutates real
#      Grafana state and requires an Org Admin caller. There is no dependable
#      Admin-token'd live Grafana to safely mutate in the shard fan-out — the
#      per-instance ephemeral e2e namespaces run grafana.enabled=false, and the
#      staging singleton / prod must NOT have its live user roles churned by
#      a test. A bump against real Grafana is destructive; it cannot be a gate.
#   2. The logic under test is all in the `gb` binary (canonicalOrgRole
#      validation, filterUsersByRole, the userSelector login/email matcher and
#      its unmatched-name error, selectBumpTargets' from/to/idempotency
#      filtering, and the --dry-run no-write path). Driving it needs only a
#      Grafana that answers GET /api/org/users and PATCH /api/org/users/<id> —
#      trivially stubbable.
#
# So this module stands up ONE throwaway HTTP stub (Python stdlib, always
# present in the e2e shard — e2e-system-deps.lock ships python3) that speaks the
# Grafana org-users API against an in-memory user table, and drives the REAL
# `gb` binary against it. Because the stub keeps state, list-after-bump and
# idempotent-re-bump are verified END-TO-END, not just by inspecting one call's
# JSON. Two test-only control endpoints — GET /__reset (re-seed the table) and
# GET /__patches (report the PATCHes applied) — let each leg start clean and
# assert exactly what was (or was NOT) written.
#
# It is hermetic: ZERO pods, ZERO shared-daemon writes, ZERO live Grafana — so
# it is green today and is a HARD gate. It does NOT use the shard's beads
# port-forward tunnel at all (the users subcommands never talk to the daemon).
#
# ── Legs (each a hard sub-gate; any FAIL reddens the shard) ───────────────────
#
#   1  list-all       `gb grafana users list --json` returns EVERY org user with
#                     the role the stub holds (userId/role/login round-trip).
#   2  list-role      `--role Viewer` filters to exactly the Viewers (case-
#                     insensitive), excluding Admin/Editor users.
#   3  list-bad-role  `--role Bogus` is a HARD error ("invalid --role"), exit
#                     non-zero — never a silent unfiltered list.
#   4  bump-dry-run   `bump --dry-run --json` reports the Viewers it WOULD bump
#                     (count>0, bumped=0, action="would-bump") and writes
#                     NOTHING — the stub records ZERO PATCHes and a re-list still
#                     shows the users at their original role.
#   5  bump-default   `bump --json` (Viewer→Admin default) PATCHes every Viewer
#                     to Admin (stub records the writes), a re-list shows zero
#                     Viewers left, and an idempotent RE-bump is a no-op
#                     (count=0, bumped=0, no new PATCH).
#   6  bump-by-name   `bump --login <one>` bumps ONLY that user (the other
#                     Viewer is untouched); a typo'd `--login <ghost>` is a HARD
#                     error ("no Grafana user matched"), never a silent no-op.
#   7  bump-bad-to    `bump --to Bogus` is a HARD error ("invalid --to"), exit
#                     non-zero — an unknown target role never reaches Grafana.
#
# ── Skip-clean contract ──────────────────────────────────────────────────────
#
# The `gb` binary + python3 are present in every e2e shard container (the shard
# `use:` list includes build-gb; e2e-system-deps.lock ships python3). If either
# is somehow absent the module SKIPS CLEANLY (exit 0) with a LOUD banner — an
# absent binary must never red the suite — mirroring the grafana-render-cli /
# grafana-bridge skip_all("loud") contract. When present, every leg is a HARD
# assertion.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-grafana-users-cli.sh

set -uo pipefail

# ── Config ────────────────────────────────────────────────────────
CONTROLLER_DIR="${CONTROLLER_DIR:-gardenio/controller}"
RUN_ID="${RUN_ID:-$$}"
WORKDIR="$(mktemp -d "/tmp/grafana-users-cli.${RUN_ID}.XXXXXX")"
GB_BIN=""
GB_TEMP=""          # non-empty only when we built a throwaway binary to clean up
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
  red " SKIP grafana-users-cli (LOUD): $why"
  red " This run did NOT assert the 'gb grafana users' CLI (list role filtering,"
  red " bump from/to/idempotency selection, --dry-run no-write, and the"
  red " unmatched-name / invalid-role hard errors). A green suite here proves"
  red " NOTHING about those commands. The e2e shard is built with the gb binary"
  red " (build-gb task) + python3 (e2e-system-deps.lock) — if this fires in CI,"
  red " that wiring regressed."
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

# free_port asks the kernel for an unused TCP port by binding :0 and reading it
# back — race-free enough for a single-process test rig (we bind the real server
# immediately after) and avoids hard-coding ports that could collide when shards
# co-locate.
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

# jget reads one top-level key (or a jq-ish path via python) out of a JSON file.
# Uses python3 (already a hard dep of this rig) so we do not require jq.
jget() {
  python3 - "$1" "$2" <<'PY'
import json, sys
try:
    obj = json.load(open(sys.argv[1]))
except Exception:
    print(""); sys.exit(0)
key = sys.argv[2]
# Support a single [index] or .key traversal; good enough for these assertions.
cur = obj
for part in key.replace('[', '.').replace(']', '').split('.'):
    if part == '':
        continue
    if isinstance(cur, list):
        try:
            cur = cur[int(part)]
        except Exception:
            print(""); sys.exit(0)
    elif isinstance(cur, dict):
        cur = cur.get(part, "")
    else:
        print(""); sys.exit(0)
print(cur if cur is not None else "")
PY
}

echo "════════════════════════════════════════════════════"
echo " gb grafana users CLI E2E (ka-0000004197)"
echo " list-all · list-role · list-bad-role · bump-dry-run · bump-default · bump-by-name · bump-bad-to"
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
    || skip_all "python3 absent — cannot stand up the stub org-users server"
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
  || skip_all "python3 absent — cannot stand up the stub org-users server"
echo

# ── 2. Stand up the stub Grafana org-users server ───────────────────
# A single stateful HTTP server that speaks the two Grafana org-users endpoints
# the gb binary drives, plus two test-only control endpoints:
#   GET   /api/org/users        → the current user table
#   PATCH /api/org/users/<id>   → set {"role":...} on a user, record the PATCH
#   GET   /__reset              → re-seed the table + clear the PATCH log
#   GET   /__patches            → {"count":N,"patches":[{"user_id":..,"role":..}]}
STUB_PY="$WORKDIR/org_users_stub.py"
cat > "$STUB_PY" <<'PY'
# Throwaway stateful stub of the Grafana org-users API for the gb-grafana-users
# E2E. Keeps an in-memory user table so list-after-bump and idempotent re-bump
# are verifiable end-to-end; records every PATCH so a --dry-run leg can prove
# NOTHING was written. argv[1] = port.
import http.server, json, re, sys, urllib.parse as up

# Seed: two Viewers (the bump targets), one Admin (idempotency), one Editor
# (a non-target the default --from Viewer must skip).
SEED = [
    {"userId": 1, "email": "alice@example.com", "login": "alice", "role": "Viewer"},
    {"userId": 2, "email": "bob@example.com",   "login": "bob",   "role": "Viewer"},
    {"userId": 3, "email": "carol@example.com", "login": "carol", "role": "Admin"},
    {"userId": 4, "email": "dave@example.com",  "login": "dave",  "role": "Editor"},
]

STATE = {"users": [], "patches": []}

def reseed():
    STATE["users"] = [dict(u) for u in SEED]
    STATE["patches"] = []

reseed()

PATCH_RE = re.compile(r"^/api/org/users/(\d+)$")

class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass

    def _json(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        path = up.urlparse(self.path).path
        if path == "/api/org/users":
            return self._json(200, STATE["users"])
        if path == "/__reset":
            reseed()
            return self._json(200, {"ok": True})
        if path == "/__patches":
            return self._json(200, {"count": len(STATE["patches"]),
                                    "patches": STATE["patches"]})
        return self._json(404, {"message": "not found"})

    def do_PATCH(self):
        path = up.urlparse(self.path).path
        m = PATCH_RE.match(path)
        if not m:
            return self._json(404, {"message": "not found"})
        uid = int(m.group(1))
        length = int(self.headers.get("Content-Length", 0) or 0)
        raw = self.rfile.read(length) if length else b"{}"
        try:
            role = json.loads(raw).get("role", "")
        except Exception:
            return self._json(400, {"message": "bad json"})
        # Apply the role to the matching user (mirrors Grafana's PATCH semantics)
        # and record the write for /__patches introspection.
        found = False
        for u in STATE["users"]:
            if u["userId"] == uid:
                u["role"] = role
                found = True
                break
        if not found:
            return self._json(404, {"message": "user not found"})
        STATE["patches"].append({"user_id": uid, "role": role})
        return self._json(200, {"message": "Organization user updated"})

port = int(sys.argv[1])
http.server.HTTPServer(("127.0.0.1", port), Handler).serve_forever()
PY

ORG_PORT="$(free_port)"
python3 "$STUB_PY" "$ORG_PORT" & PIDS+=($!)
ORG_URL="http://127.0.0.1:${ORG_PORT}"

if ! wait_http "${ORG_URL}/api/org/users"; then
  skip_all "stub org-users server failed to come up (python3 http.server unavailable?)"
fi
pass "stub org-users server up ($ORG_URL)"
echo

# gb resolves auth from env (GRAFANA_TOKEN → auth-proxy header → AVA cookie).
# The stub is auth-disabled, and every leg passes --grafana-url explicitly, but
# clear these so a misconfigured shard cannot shadow the stub or add unexpected
# headers (mirrors test-grafana-render-cli).
unset GRAFANA_URL GRAFANA_BASE_URL GRAFANA_TOKEN GRAFANA_AUTH_PROXY_HEADER GRAFANA_USER 2>/dev/null || true

# Convenience wrappers around the real binary.
gb_users() { "$GB_BIN" grafana users "$@"; }
reset_stub() { curl -s -o /dev/null "${ORG_URL}/__reset"; }
patch_count() { curl -s "${ORG_URL}/__patches" | python3 -c 'import json,sys; print(json.load(sys.stdin)["count"])' 2>/dev/null || echo -1; }

# ── Leg 1: list-all (every org user, role round-trip) ───────────────
blue "[leg list-all] gb grafana users list --json → all 4 users"
reset_stub
L1="$WORKDIR/list-all.json"
if gb_users list --grafana-url "$ORG_URL" --json >"$L1" 2>"$WORKDIR/list-all.err"; then
  n="$(python3 -c 'import json,sys; print(len(json.load(open(sys.argv[1]))))' "$L1" 2>/dev/null || echo -1)"
  # Build a login→role map to assert the round-trip regardless of ordering.
  roles="$(python3 - "$L1" <<'PY'
import json, sys
users = json.load(open(sys.argv[1]))
m = {u.get("login"): u.get("role") for u in users}
print(f'{m.get("alice")},{m.get("bob")},{m.get("carol")},{m.get("dave")}')
PY
)"
  if [ "$n" != 4 ]; then
    fail "leg list-all count" "listed $n users, want 4"
  elif [ "$roles" != "Viewer,Viewer,Admin,Editor" ]; then
    fail "leg list-all roles" "alice,bob,carol,dave roles = $roles, want Viewer,Viewer,Admin,Editor"
  else
    pass "leg list-all — 4 users, roles round-tripped ($roles)"
  fi
else
  fail "leg list-all" "gb exited non-zero: $(tail -3 "$WORKDIR/list-all.err" 2>/dev/null)"
fi
echo

# ── Leg 2: list-role (—role Viewer filters to exactly the Viewers) ──
blue "[leg list-role] --role Viewer → exactly alice + bob"
reset_stub
L2="$WORKDIR/list-role.json"
if gb_users list --grafana-url "$ORG_URL" --role Viewer --json >"$L2" 2>"$WORKDIR/list-role.err"; then
  logins="$(python3 -c 'import json,sys; us=json.load(open(sys.argv[1])); print(",".join(sorted(u.get("login","") for u in us)))' "$L2" 2>/dev/null || true)"
  allviewer="$(python3 -c 'import json,sys; us=json.load(open(sys.argv[1])); print(all(u.get("role")=="Viewer" for u in us) and len(us)>0)' "$L2" 2>/dev/null || echo False)"
  if [ "$logins" = "alice,bob" ] && [ "$allviewer" = "True" ]; then
    pass "leg list-role — filtered to exactly the Viewers ($logins)"
  else
    fail "leg list-role" "role=Viewer filter returned logins=[$logins] all-viewer=$allviewer, want alice,bob / True"
  fi
else
  fail "leg list-role" "gb exited non-zero: $(tail -3 "$WORKDIR/list-role.err" 2>/dev/null)"
fi
echo

# ── Leg 3: list-bad-role (—role Bogus → hard error, no silent list) ──
blue "[leg list-bad-role] --role Bogus → hard 'invalid --role' error"
reset_stub
if gb_users list --grafana-url "$ORG_URL" --role Bogus --json >"$WORKDIR/list-bad.out" 2>"$WORKDIR/list-bad.err"; then
  fail "leg list-bad-role" "gb exited 0 on --role Bogus — an unknown role must be a hard error, not a silent unfiltered list"
else
  if grep -qi 'invalid --role' "$WORKDIR/list-bad.err"; then
    pass "leg list-bad-role — non-zero exit, actionable 'invalid --role' error"
  else
    fail "leg list-bad-role msg" "non-zero exit but error not actionable: $(tail -2 "$WORKDIR/list-bad.err")"
  fi
fi
echo

# ── Leg 4: bump-dry-run (reports would-bump, writes NOTHING) ─────────
blue "[leg bump-dry-run] bump --dry-run → would-bump the Viewers, ZERO writes"
reset_stub
D4="$WORKDIR/bump-dry.json"
if gb_users bump --grafana-url "$ORG_URL" --dry-run --json >"$D4" 2>"$WORKDIR/bump-dry.err"; then
  cnt="$(jget "$D4" count)"
  bumped="$(jget "$D4" bumped)"
  dry="$(jget "$D4" dry_run)"
  action0="$(jget "$D4" 'results[0].action')"
  writes="$(patch_count)"
  # After a dry-run the two Viewers must still be Viewers (nothing written).
  reL="$WORKDIR/bump-dry-relist.json"
  gb_users list --grafana-url "$ORG_URL" --role Viewer --json >"$reL" 2>/dev/null
  still="$(python3 -c 'import json,sys; print(len(json.load(open(sys.argv[1]))))' "$reL" 2>/dev/null || echo -1)"
  if [ "$cnt" = 2 ] && [ "$bumped" = 0 ] && [ "$dry" = "True" ] \
     && [ "$action0" = "would-bump" ] && [ "$writes" = 0 ] && [ "$still" = 2 ]; then
    pass "leg bump-dry-run — count=2 bumped=0 action=would-bump, 0 PATCHes, Viewers unchanged"
  else
    fail "leg bump-dry-run" "count=$cnt bumped=$bumped dry=$dry action0=$action0 writes=$writes still-viewers=$still (want 2/0/True/would-bump/0/2)"
  fi
else
  fail "leg bump-dry-run" "gb exited non-zero: $(tail -3 "$WORKDIR/bump-dry.err" 2>/dev/null)"
fi
echo

# ── Leg 5: bump-default (Viewer→Admin, writes, then idempotent) ─────
blue "[leg bump-default] bump → all Viewers→Admin (real PATCH), re-bump is a no-op"
reset_stub
B5="$WORKDIR/bump-default.json"
if gb_users bump --grafana-url "$ORG_URL" --json >"$B5" 2>"$WORKDIR/bump-default.err"; then
  cnt="$(jget "$B5" count)"
  bumped="$(jget "$B5" bumped)"
  to="$(jget "$B5" to)"
  writes="$(patch_count)"
  # Re-list Viewers: alice+bob are now Admin, so ZERO Viewers remain.
  reL="$WORKDIR/bump-default-relist.json"
  gb_users list --grafana-url "$ORG_URL" --role Viewer --json >"$reL" 2>/dev/null
  left="$(python3 -c 'import json,sys; print(len(json.load(open(sys.argv[1]))))' "$reL" 2>/dev/null || echo -1)"
  if [ "$cnt" = 2 ] && [ "$bumped" = 2 ] && [ "$to" = "Admin" ] && [ "$writes" = 2 ] && [ "$left" = 0 ]; then
    # Idempotent re-bump against the now-mutated stub: nobody at Viewer → 0.
    B5b="$WORKDIR/bump-default-idem.json"
    if gb_users bump --grafana-url "$ORG_URL" --json >"$B5b" 2>/dev/null; then
      cnt2="$(jget "$B5b" count)"
      bumped2="$(jget "$B5b" bumped)"
      writes2="$(patch_count)"
      if [ "$cnt2" = 0 ] && [ "$bumped2" = 0 ] && [ "$writes2" = 2 ]; then
        pass "leg bump-default — bumped 2→Admin (2 PATCHes), 0 Viewers left, re-bump no-op (0 new writes)"
      else
        fail "leg bump-default idempotency" "re-bump count=$cnt2 bumped=$bumped2 total-writes=$writes2 (want 0/0/2 — a second bump must not re-PATCH)"
      fi
    else
      fail "leg bump-default idempotency" "idempotent re-bump exited non-zero"
    fi
  else
    fail "leg bump-default" "count=$cnt bumped=$bumped to=$to writes=$writes viewers-left=$left (want 2/2/Admin/2/0)"
  fi
else
  fail "leg bump-default" "gb exited non-zero: $(tail -3 "$WORKDIR/bump-default.err" 2>/dev/null)"
fi
echo

# ── Leg 6: bump-by-name (—login scopes to one; typo is a hard error) ─
blue "[leg bump-by-name] bump --login alice → only alice; --login ghost → hard error"
reset_stub
B6="$WORKDIR/bump-alice.json"
if gb_users bump --grafana-url "$ORG_URL" --login alice --json >"$B6" 2>"$WORKDIR/bump-alice.err"; then
  cnt="$(jget "$B6" count)"
  bumped="$(jget "$B6" bumped)"
  who="$(jget "$B6" 'results[0].login')"
  # bob (the other Viewer) must remain a Viewer — the selector scoped the bump.
  reL="$WORKDIR/bump-alice-relist.json"
  gb_users list --grafana-url "$ORG_URL" --role Viewer --json >"$reL" 2>/dev/null
  bob_still="$(python3 -c 'import json,sys; us=json.load(open(sys.argv[1])); print("bob" in [u.get("login") for u in us])' "$reL" 2>/dev/null || echo False)"
  if [ "$cnt" = 1 ] && [ "$bumped" = 1 ] && [ "$who" = "alice" ] && [ "$bob_still" = "True" ]; then
    # A typo'd login must fail loudly (unmatchedErr), never silently bump nobody.
    if gb_users bump --grafana-url "$ORG_URL" --login ghostuser --json >"$WORKDIR/bump-ghost.out" 2>"$WORKDIR/bump-ghost.err"; then
      fail "leg bump-by-name typo" "bump --login ghostuser exited 0 — an unmatched name must be a hard error, not a silent no-op"
    elif grep -qi 'no Grafana user matched' "$WORKDIR/bump-ghost.err"; then
      pass "leg bump-by-name — scoped to alice only (bob untouched), typo'd login hard-errors"
    else
      fail "leg bump-by-name typo msg" "non-zero exit but not the unmatched-name error: $(tail -2 "$WORKDIR/bump-ghost.err")"
    fi
  else
    fail "leg bump-by-name" "count=$cnt bumped=$bumped who=$who bob-still-viewer=$bob_still (want 1/1/alice/True)"
  fi
else
  fail "leg bump-by-name" "gb exited non-zero: $(tail -3 "$WORKDIR/bump-alice.err" 2>/dev/null)"
fi
echo

# ── Leg 7: bump-bad-to (—to Bogus → hard error, nothing reaches Grafana) ──
blue "[leg bump-bad-to] bump --to Bogus → hard 'invalid --to' error, no writes"
reset_stub
if gb_users bump --grafana-url "$ORG_URL" --to Bogus --json >"$WORKDIR/bump-badto.out" 2>"$WORKDIR/bump-badto.err"; then
  fail "leg bump-bad-to" "gb exited 0 on --to Bogus — an unknown target role must be a hard error before any PATCH"
else
  writes="$(patch_count)"
  if grep -qi 'invalid --to' "$WORKDIR/bump-badto.err" && [ "$writes" = 0 ]; then
    pass "leg bump-bad-to — non-zero exit, 'invalid --to' error, 0 PATCHes"
  else
    fail "leg bump-bad-to" "err=$(tail -1 "$WORKDIR/bump-badto.err") writes=$writes (want 'invalid --to' + 0 writes)"
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
