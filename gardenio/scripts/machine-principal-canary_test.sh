#!/usr/bin/env bash
# machine-principal-canary_test.sh — unit tests for machine-principal-canary.sh.
#
# A python3 stub (stdlib http.server, 127.0.0.1, ephemeral port) plays both
# Keycloak's token endpoint and the beads daemon. Its PATCH behaviour is chosen
# per test by STUB_MODE, and it logs every request's path, Authorization header
# and body to a file so the tests can assert what the script actually sent.
#
# Covered:
#   deny                403 + caller_subject=machine:gardenio-authz-canary → exit 0
#   allow               200 → exit 1 (POSITIVE CONTROL: the check can fail)
#   deny-wrong-subject  403 naming another machine → exit 2
#   token-500           token endpoint down → exit 2, nothing written
#   patch-500           daemon error → exit 2
#   secrecy             the issued token, the client secret and the bead's
#                       credential fields never appear in stdout or stderr;
#                       the secret reaches the token endpoint without its
#                       trailing newline; the PATCH is the idempotent write.
#
# Usage: bash gardenio/scripts/machine-principal-canary_test.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
UNDER_TEST="${SCRIPT_DIR}/machine-principal-canary.sh"

PASS=0
FAIL=0
pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1: $2"; FAIL=$((FAIL + 1)); }

TMP="$(mktemp -d)"
STUB_PID=""
cleanup() {
  if [ -n "$STUB_PID" ]; then kill "$STUB_PID" 2>/dev/null || true; wait "$STUB_PID" 2>/dev/null || true; fi
  rm -rf "$TMP"
}
trap cleanup EXIT

TOKEN="stub-access-token-MARKER-7f3a9c"
SECRET="s3cret-client-MARKER-b81e"
CRED_MARKER="credential-MARKER-must-never-print"
KEY_MARKER="sk-litellm-MARKER-must-never-print"
printf '%s\n' "$SECRET" > "$TMP/secret"   # trailing newline on purpose

cat > "$TMP/stub.py" <<'PY'
import json, os, sys
from http.server import BaseHTTPRequestHandler, HTTPServer

MODE = os.environ["STUB_MODE"]
TOKEN = os.environ["STUB_TOKEN"]
LOG = os.environ["STUB_LOG"]
CRED = os.environ["STUB_CRED"]
KEY = os.environ["STUB_KEY"]

DENY = {
    "denied_action": "update", "bead_type": "account",
    "required_role": "gardenio-admin", "escalatable": True,
    "error": "insufficient permissions: update on type account requires gardenio-admin",
    "advisory_text": "Requires role gardenio-admin.",
    "caller_subject": "machine:gardenio-authz-canary",
}
BEAD = {"id": "gd-TEST", "type": "account",
        "fields": {"probe_auth_status": "allowed", "credential": CRED, "litellm_key": KEY}}


class H(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def _send(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _body(self):
        n = int(self.headers.get("Content-Length", "0"))
        return self.rfile.read(n).decode() if n else ""

    def _record(self, verb, body):
        with open(LOG, "a") as f:
            f.write("%s %s auth=[%s] body=[%s]\n" % (verb, self.path, self.headers.get("Authorization", ""), body))

    def do_POST(self):
        body = self._body()
        self._record("POST", body)
        if self.path != "/token":
            return self._send(404, {"error": "not found"})
        if MODE == "token-500":
            return self._send(500, {"error": "keycloak down"})
        self._send(200, {"access_token": TOKEN, "expires_in": 300, "token_type": "Bearer"})

    def do_GET(self):
        self._record("GET", "")
        if not self.path.startswith("/v1/beads/"):
            return self._send(404, {"error": "not found"})
        self._send(200, BEAD)

    def do_PATCH(self):
        body = self._body()
        self._record("PATCH", body)
        if MODE == "allow":
            return self._send(200, BEAD)
        if MODE == "deny-wrong-subject":
            return self._send(403, dict(DENY, caller_subject="machine:gardenio-controller"))
        if MODE == "patch-500":
            return self._send(500, {"error": "boom"})
        self._send(403, DENY)


srv = HTTPServer(("127.0.0.1", 0), H)
print(srv.server_address[1], flush=True)
srv.serve_forever()
PY

# start_stub MODE → sets PORT and LOG; the stub prints its port once bound.
start_stub() {
  LOG="$TMP/$1.log"; : > "$LOG"
  STUB_MODE="$1" STUB_TOKEN="$TOKEN" STUB_LOG="$LOG" STUB_CRED="$CRED_MARKER" STUB_KEY="$KEY_MARKER" \
    python3 "$TMP/stub.py" > "$TMP/$1.port" &
  STUB_PID=$!
  local i=0
  while [ ! -s "$TMP/$1.port" ]; do
    i=$((i + 1)); [ "$i" -lt 100 ] || { echo "stub did not start"; exit 1; }
    sleep 0.05
  done
  PORT="$(cat "$TMP/$1.port")"
}
stop_stub() {
  kill "$STUB_PID" 2>/dev/null || true; wait "$STUB_PID" 2>/dev/null || true; STUB_PID=""
}

# run_canary MODE → OUT (combined stdout+stderr file) and RC
run_canary() {
  start_stub "$1"
  OUT="$TMP/$1.out"
  set +e
  BEADS_URL="http://127.0.0.1:${PORT}" \
  BEADS_OIDC_TOKEN_URL="http://127.0.0.1:${PORT}/token" \
  TARGET="gd-TEST" \
  CANARY_CLIENT_SECRET_FILE="$TMP/secret" \
    bash "$UNDER_TEST" > "$OUT" 2>&1
  RC=$?
  set -e
  stop_stub
}

assert_rc() { # name expected
  if [ "$RC" -eq "$2" ]; then pass "$1: exit $2"; else fail "$1" "expected exit $2, got $RC: $(cat "$OUT")"; fi
}
assert_out_has() { # name pattern
  if grep -qF -- "$2" "$OUT"; then pass "$1: output has '$2'"; else fail "$1" "output lacks '$2': $(cat "$OUT")"; fi
}
assert_no_leak() { # name
  local leaked=0 m
  for m in "$TOKEN" "$SECRET" "$CRED_MARKER" "$KEY_MARKER"; do
    if grep -qF -- "$m" "$OUT"; then fail "$1" "secret material leaked to output: $m"; leaked=1; fi
  done
  [ "$leaked" -eq 0 ] && pass "$1: no token/secret/credential in output"
}

# ── deny: the expected live outcome ───────────────────────────────────────
run_canary deny
assert_rc deny 0
assert_out_has deny "HTTP 403"
assert_out_has deny "caller_subject: machine:gardenio-authz-canary"
assert_out_has deny "required_role: gardenio-admin"
assert_no_leak deny
if grep -qF -- "PATCH /v1/beads/gd-TEST auth=[Bearer ${TOKEN}]" "$LOG"; then
  pass "deny: PATCH carried the issued bearer token"
else fail deny "PATCH did not carry the bearer token: $(cat "$LOG")"; fi
if grep -qF -- "GET /v1/beads/gd-TEST auth=[Bearer ${TOKEN}]" "$LOG"; then
  pass "deny: GET carried the issued bearer token"
else fail deny "GET did not carry the bearer token: $(cat "$LOG")"; fi
if grep -qF -- 'body=[{"fields":{"probe_auth_status":"allowed"}}]' "$LOG"; then
  pass "deny: PATCH is the idempotent write of the value read"
else fail deny "PATCH body is not the idempotent write: $(cat "$LOG")"; fi
if grep -qF -- "client_secret=${SECRET}&" "$LOG" || grep -qF -- "client_secret=${SECRET}]" "$LOG"; then
  pass "deny: client secret reached the token endpoint without its trailing newline"
else fail deny "token request body unexpected: $(grep POST "$LOG")"; fi
if grep -qF -- "%0A" "$LOG"; then fail deny "trailing newline leaked into the grant"; else pass "deny: no newline in grant"; fi

# ── allow: POSITIVE CONTROL — the check must be able to fail ──────────────
run_canary allow
assert_rc allow 1
assert_out_has allow "HTTP 200"
assert_out_has allow "ALLOWED"
assert_no_leak allow

# ── deny-wrong-subject: a 403 that names someone else proves nothing ──────
run_canary deny-wrong-subject
assert_rc deny-wrong-subject 2
assert_out_has deny-wrong-subject "caller_subject: machine:gardenio-controller"
assert_no_leak deny-wrong-subject

# ── token-500: no token → inconclusive, and nothing is written ────────────
run_canary token-500
assert_rc token-500 2
assert_out_has token-500 "token endpoint returned HTTP 500"
assert_no_leak token-500
if grep -q "PATCH" "$LOG"; then fail token-500 "a PATCH was sent without a token"; else pass "token-500: no write attempted"; fi

# ── patch-500: daemon error is inconclusive, not a pass ───────────────────
run_canary patch-500
assert_rc patch-500 2
assert_out_has patch-500 "HTTP 500"
assert_no_leak patch-500

echo
echo "machine-principal-canary_test: ${PASS} passed, ${FAIL} failed"
[ "$FAIL" -eq 0 ]
