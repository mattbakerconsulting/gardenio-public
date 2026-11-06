#!/usr/bin/env bash
# test-edge-auth-box-auth.sh — E2E gate for the edge-auth BOX-AUTH profile:
# EDGE_AUTH_PAT_DISABLED (ka-0000006087 §6). Boots the REAL cmd/edge-auth binary
# from source with the box-auth env and asserts the live HTTP surface, so the
# air-gapped-box readiness contract is exercised end-to-end rather than only in
# the cmd/edge-auth/pat_disabled_test.go unit test.
#
# Bead ka-0000005667 (parent ka-0000000958). Commit 542cd546 (PAT_DISABLED
# readiness gate). Companion to the deployed-shim smoke in
# gardenio/tests/e2e/edge-auth/test-edge-auth.sh (Gates T + U).
#
# ── THE GAP THIS CLOSES ───────────────────────────────────────────────────────
#
# The box-auth profile turns the GitLab-PAT auth path OFF so an air-gapped box
# authenticates the CLI/agent edge solely via the in-box Keycloak (oauth2-proxy
# cookie): the shim makes NO gitlab.com API calls at all, so a PAT minted from
# stale org membership cannot authenticate even on a box with internet egress,
# and the readiness gate stops requiring the GitLab validator PROVIDED another
# auth mode (cookie or AVA) is configured. That runtime shipped with UNIT-only
# coverage (pat_disabled_test.go) + a helm render test (box_auth_profile_test.go);
# a grep of gardenio/tests/e2e for PAT_DISABLED / box-auth returned empty. No e2e
# ran the actual binary under the box-auth env, so the LIVE wiring the unit test
# cannot reach — /readyz transitions, the PAT-bearing 503, and the absence of any
# outbound GitLab call — was asserted against nothing.
#
# The box-auth profile is NOT deployed in any e2e namespace (the ephemeral and
# staging singleton both run the PAT-enabled edge, and only the portable Gasbox
# product flips patDisabled=true), so this is a from-source local-binary probe —
# the same shape as test-agent-feedback-writepath.sh (ka-0000008183) and the
# escalation-auto-reaper backfill: build the real cmd, boot it locally, drive its
# public surface. It needs no cluster and runs GREEN in every shard container
# that carries a Go toolchain.
#
# ── Legs (each a hard sub-gate; any FAIL reds the shard) ──────────────────────
#
#   1 flag-parses          Box-auth instance boots and logs the PAT-path-disabled
#                          line (EDGE_AUTH_PAT_DISABLED took effect).
#   2 ready-with-cookie    PAT_DISABLED + oauth2-proxy cookie configured →
#                          /readyz 200 (Ready): cookie auth stands in for the
#                          absent GitLab validator.
#   3 notready-no-cookie   PAT_DISABLED + NO cookie/AVA/Entra → /readyz 503
#                          (NotReady): a cookieless box surfaces the misconfig
#                          loudly instead of admitting no one.
#   4 pat-rejected         PAT_DISABLED: a PAT-bearing /auth/check → 503
#                          (validator_not_configured) via the nil-validator guard.
#   5 no-gitlab-call       PAT_DISABLED: after the PAT request the mock GitLab saw
#                          ZERO /api/v4/user hits — the shim made no outbound
#                          GitLab call. GUARDED by the control below.
#   6 control-gitlab-call  KNOWN-GOOD CONTROL (advice ka-0000002586): a PAT-ENABLED
#                          instance pointed at the SAME mock DOES increment the
#                          /api/v4/user counter, proving leg 5's "hits=0" is real
#                          coverage and not a mock that never counts.
#
# ── Skip-clean contract ───────────────────────────────────────────────────────
#
# The Go toolchain + cmd/edge-auth source + python3 (the mock GitLab) are present
# in every e2e shard container. If any is absent the module SKIPS CLEANLY
# (exit 0) with a LOUD banner — an absent toolchain must never red the suite,
# mirroring the agent-feedback-writepath skip contract. When all are present,
# every leg is a HARD assertion.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-edge-auth-box-auth.sh

set -uo pipefail

# ── Config ────────────────────────────────────────────────────────
CONTROLLER_DIR="${CONTROLLER_DIR:-gardenio/controller}"
RUN_ID="${RUN_ID:-$$}"
CURL_TIMEOUT="${CURL_TIMEOUT:-5}"
EA_BIN=""
MOCK_PID=""
PIDS=()
TMP_PREFIX="/tmp/edge-auth-box-auth-${RUN_ID}"

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

# skip_all exits 0 (a missing toolchain/dep must NOT red the suite) with a LOUD
# banner so a skip is never silently read as a pass.
skip_all() {
  local why="$1"
  red "════════════════════════════════════════════════════"
  red " SKIP edge-auth-box-auth (LOUD): $why"
  red " This run did NOT assert the edge-auth BOX-AUTH profile"
  red " (EDGE_AUTH_PAT_DISABLED readiness: Ready-with-cookie, NotReady-without,"
  red " PAT-bearing 503, and ZERO outbound gitlab.com calls — ka-0000006087 §6)."
  red " A green suite here proves NOTHING about the air-gapped box readiness"
  red " contract. The e2e shard is built with a Go toolchain + the cmd/edge-auth"
  red " source + python3 — if this fires in CI, that wiring regressed."
  red "════════════════════════════════════════════════════"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

have() { command -v "$1" >/dev/null 2>&1; }

cleanup() {
  for p in "${PIDS[@]:-}"; do
    [ -n "$p" ] && kill "$p" 2>/dev/null || true
  done
  [ -n "$MOCK_PID" ] && kill "$MOCK_PID" 2>/dev/null || true
  [ -n "$EA_BIN" ] && rm -f "$EA_BIN" 2>/dev/null || true
  rm -f "${TMP_PREFIX}"-*.log "${TMP_PREFIX}-mock.py" "${TMP_PREFIX}-hits" 2>/dev/null || true
}
trap cleanup EXIT

# free_port finds an unused TCP port in a small range so parallel shards do not
# collide. Falls back to a fixed base if ss is unavailable.
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

# wait_healthz polls a booting edge-auth until /healthz answers (or times out).
wait_healthz() {
  local base="$1" _i
  for _i in $(seq 1 30); do
    [ "$(curl -s --max-time 2 -o /dev/null -w '%{http_code}' "${base}/healthz" 2>/dev/null)" = "200" ] && return 0
    sleep 0.2
  done
  return 1
}

# http_code returns the status of a GET, optionally with a Bearer token.
http_code() {
  local url="$1" token="${2:-}"
  if [ -n "$token" ]; then
    curl -s --max-time "$CURL_TIMEOUT" -o /dev/null -w '%{http_code}' \
      -H "Authorization: Bearer ${token}" "$url" 2>/dev/null
  else
    curl -s --max-time "$CURL_TIMEOUT" -o /dev/null -w '%{http_code}' "$url" 2>/dev/null
  fi
}

echo "════════════════════════════════════════════════════"
echo " edge-auth BOX-AUTH profile E2E (ka-0000005667)"
echo " EDGE_AUTH_PAT_DISABLED readiness + zero outbound gitlab.com calls"
echo "════════════════════════════════════════════════════"

# ── Preconditions (skip clean) ─────────────────────────────────────
have curl || skip_all "curl not in PATH"
have python3 || skip_all "python3 not in PATH (mock GitLab)"
have go || skip_all "no go toolchain to build cmd/edge-auth"
[ -d "$CONTROLLER_DIR/cmd/edge-auth" ] \
  || skip_all "cmd/edge-auth source missing under $CONTROLLER_DIR"

# ── Build the real edge-auth binary from source ────────────────────
blue "[build] cmd/edge-auth (from source)"
EA_BIN="$(mktemp "${TMP_PREFIX}-bin.XXXXXX")"
BUILD_LOG="${TMP_PREFIX}-build.log"
if ( cd "$CONTROLLER_DIR" && CGO_ENABLED=0 go build -o "$EA_BIN" ./cmd/edge-auth/ ) >"$BUILD_LOG" 2>&1; then
  pass "cmd/edge-auth built from source"
else
  fail "build edge-auth" "go build failed: $(tail -8 "$BUILD_LOG")"
  blue "════════════════════════════════════════════════════"
  red " ✗ build failed — cannot exercise the box-auth profile"
  for err in "${ERRORS[@]}"; do red "   - $err"; done
  blue "════════════════════════════════════════════════════"
  exit 1
fi
echo

# ── Mock GitLab: counts GET /api/v4/user hits into a file ──────────
# The counter file is the observable that makes leg 5 (no-gitlab-call) a real
# assertion: leg 6's control proves the same mock DOES tick when a PAT-enabled
# validator is wired against it.
HITS_FILE="${TMP_PREFIX}-hits"
: >"$HITS_FILE"
MOCK_PY="${TMP_PREFIX}-mock.py"
cat >"$MOCK_PY" <<PY
import http.server, socketserver, sys, os
HITS = sys.argv[2]
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path.startswith("/api/v4/user"):
            with open(HITS, "a") as f:
                f.write("1")
            # Minimal valid /user body so the validator proceeds to the group
            # check (which also hits this mock and 404s → ErrNotInGroup). Either
            # way the /user hit is recorded, which is all the control asserts.
            body = b'{"id":1,"username":"ctl","email":"ctl@example.com","name":"ctl"}'
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        else:
            self.send_response(404); self.end_headers()
    def log_message(self, *a): pass
socketserver.TCPServer.allow_reuse_address = True
socketserver.TCPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
PY
MOCK_PORT="$(free_port 18820)"
python3 "$MOCK_PY" "$MOCK_PORT" "$HITS_FILE" >"${TMP_PREFIX}-mock.log" 2>&1 &
MOCK_PID=$!
MOCK_URL="http://127.0.0.1:${MOCK_PORT}"
# Confirm the mock is up (a 404 on / is fine — it proves it's listening).
mock_up=0
for _i in $(seq 1 20); do
  [ "$(curl -s --max-time 2 -o /dev/null -w '%{http_code}' "${MOCK_URL}/" 2>/dev/null)" = "404" ] && { mock_up=1; break; }
  sleep 0.2
done
[ "$mock_up" = "1" ] || skip_all "mock GitLab did not come up on ${MOCK_URL}"
dim "  mock GitLab: ${MOCK_URL} (counts GET /api/v4/user → ${HITS_FILE})"
echo

hits_count() { wc -c <"$HITS_FILE" 2>/dev/null | tr -d ' '; }

# ── Instance A: box-auth + cookie configured ───────────────────────
# PAT_DISABLED with the in-box oauth2-proxy cookie wired. GITLAB_BASE_URL points
# at the mock so any accidental outbound call would be counted; the box-auth
# profile must make none.
blue "[boot] instance A — PAT_DISABLED=true + oauth2-proxy cookie, GitLab base → mock"
PORT_A="$(free_port 18830)"
BASE_A="http://127.0.0.1:${PORT_A}"
EDGE_AUTH_PAT_DISABLED=true \
  EDGE_AUTH_OAUTH2_PROXY_AUTH_URL=http://gardenio-forwardauth:4180/oauth2/auth \
  EDGE_AUTH_OAUTH2_PROXY_COOKIE_NAME=_gardenio_oauth2 \
  GITLAB_BASE_URL="$MOCK_URL" \
  EDGE_AUTH_GITLAB_GROUP=gardenio \
  LISTEN_ADDR="127.0.0.1:${PORT_A}" \
  "$EA_BIN" >"${TMP_PREFIX}-A.log" 2>&1 &
PIDS+=("$!")
if ! wait_healthz "$BASE_A"; then
  fail "instance A boot" "healthz never answered on ${BASE_A}: $(tail -4 "${TMP_PREFIX}-A.log")"
else
  # Leg 1: flag-parses — the PAT-path-disabled log line proves the flag took.
  if grep -q 'PAT path disabled' "${TMP_PREFIX}-A.log"; then
    pass "flag-parses: EDGE_AUTH_PAT_DISABLED logged the PAT-path-disabled line"
  else
    fail "flag-parses" "no 'PAT path disabled' line in instance A log: $(tail -4 "${TMP_PREFIX}-A.log")"
  fi

  # Leg 2: ready-with-cookie — cookie mode satisfies readiness in place of PAT.
  code="$(http_code "${BASE_A}/readyz")"
  if [ "$code" = "200" ]; then
    pass "ready-with-cookie: PAT_DISABLED + oauth2-proxy cookie → /readyz 200 (Ready)"
  else
    fail "ready-with-cookie" "want /readyz 200, got $code — body: $(curl -s --max-time "$CURL_TIMEOUT" "${BASE_A}/readyz")"
  fi

  # Leg 4: pat-rejected — a PAT-bearing /auth/check → 503 (nil validator guard).
  code="$(http_code "${BASE_A}/auth/check" "stale-pat-from-revoked-membership")"
  if [ "$code" = "503" ]; then
    pass "pat-rejected: PAT-bearing /auth/check → 503 (validator_not_configured)"
  else
    fail "pat-rejected" "want 503, got $code — a PAT must not authenticate on the box-auth edge"
  fi

  # Leg 5: no-gitlab-call — the shim made ZERO outbound GitLab calls.
  gh="$(hits_count)"
  if [ "${gh:-0}" -eq 0 ]; then
    pass "no-gitlab-call: box-auth edge made 0 GET /api/v4/user calls (hits=${gh:-0})"
  else
    fail "no-gitlab-call" "mock GitLab saw ${gh} /api/v4/user hit(s) — box-auth must make NO gitlab.com calls (ka-0000006087 §6)"
  fi
fi
echo

# ── Instance B: box-auth + NO cookie/AVA ───────────────────────────
blue "[boot] instance B — PAT_DISABLED=true, no cookie/AVA/Entra"
PORT_B="$(free_port 18840)"
BASE_B="http://127.0.0.1:${PORT_B}"
EDGE_AUTH_PAT_DISABLED=true \
  LISTEN_ADDR="127.0.0.1:${PORT_B}" \
  "$EA_BIN" >"${TMP_PREFIX}-B.log" 2>&1 &
PIDS+=("$!")
if ! wait_healthz "$BASE_B"; then
  fail "instance B boot" "healthz never answered on ${BASE_B}: $(tail -4 "${TMP_PREFIX}-B.log")"
else
  # Leg 3: notready-no-cookie — a cookieless box must surface NotReady loudly.
  code="$(http_code "${BASE_B}/readyz")"
  body="$(curl -s --max-time "$CURL_TIMEOUT" "${BASE_B}/readyz" 2>/dev/null)"
  if [ "$code" = "503" ] && grep -qi 'PAT disabled' <<<"$body"; then
    pass "notready-no-cookie: PAT_DISABLED + no other auth mode → /readyz 503 (NotReady), reason surfaced"
  elif [ "$code" = "503" ]; then
    fail "notready-no-cookie" "got 503 but reason did not mention PAT-disabled: $body"
  else
    fail "notready-no-cookie" "want /readyz 503 (NotReady), got $code — a cookieless box must not report Ready"
  fi
fi
echo

# ── Instance C: KNOWN-GOOD CONTROL — PAT ENABLED against the mock ──
# Advice ka-0000002586: verify leg 5's "hits=0" with a control whose counter
# MUST increment. With the PAT path ENABLED and GITLAB_BASE_URL → mock, a
# PAT-bearing /auth/check builds the GitLab validator and calls /api/v4/user, so
# the counter ticks. If it does NOT tick, leg 5 was measuring a dead mock and is
# void — so this leg failing invalidates the box-auth no-call assertion.
blue "[boot] instance C — control: PAT ENABLED, GitLab base → mock (counter must tick)"
PORT_C="$(free_port 18850)"
BASE_C="http://127.0.0.1:${PORT_C}"
: >"$HITS_FILE"  # reset so the control measures its own calls
EDGE_AUTH_PAT_DISABLED=false \
  GITLAB_BASE_URL="$MOCK_URL" \
  EDGE_AUTH_GITLAB_GROUP=gardenio \
  LISTEN_ADDR="127.0.0.1:${PORT_C}" \
  "$EA_BIN" >"${TMP_PREFIX}-C.log" 2>&1 &
PIDS+=("$!")
if ! wait_healthz "$BASE_C"; then
  fail "instance C boot" "healthz never answered on ${BASE_C}: $(tail -4 "${TMP_PREFIX}-C.log")"
else
  # Drive a PAT request; the validator calls the mock's /api/v4/user.
  http_code "${BASE_C}/auth/check" "any-pat" >/dev/null
  gh="$(hits_count)"
  if [ "${gh:-0}" -ge 1 ]; then
    pass "control-gitlab-call: PAT-enabled edge hit mock /api/v4/user (${gh}) — leg 5's hits=0 is real coverage"
  else
    fail "control-gitlab-call" "PAT-enabled control did NOT hit the mock (hits=${gh:-0}) — the mock counter is dead, so the no-gitlab-call assertion is VOID"
  fi
fi
echo

# ── Summary ────────────────────────────────────────────────────────
blue "════════════════════════════════════════════════════"
total=$((PASS + FAIL))
if [ "$FAIL" -eq 0 ]; then
  green "  ✓ All $total box-auth assertions passed"
else
  red "  ✗ $FAIL/$total box-auth assertions failed"
  echo
  for err in "${ERRORS[@]}"; do red "   - $err"; done
fi
blue "════════════════════════════════════════════════════"

[ "$FAIL" -eq 0 ]
