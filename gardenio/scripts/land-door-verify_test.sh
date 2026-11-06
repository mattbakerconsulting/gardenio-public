#!/usr/bin/env bash
# land-door-verify_test.sh — unit tests for land-door-verify.sh.
#
# A python3 stub (stdlib http.server, 127.0.0.1, ephemeral port) plays both
# Keycloak's token endpoint and the landgate. STUB_MODE picks its answers, and
# it logs each request's method, path and Authorization header so the tests
# can assert what the script sent.
#
# Covered:
#   pass        list/preview 200, POST preview and self/view 404 → exit 0
#   list-403    the list is refused → exit 1 (POSITIVE CONTROL: it can fail)
#   token-401   the sign-in is refused → exit 2, nothing sent to landgate
#   no-creds    LAND_VERIFIER_PASSWORD unset → exit 2
#   plan        LAND_VERIFIER_PLAN set → the preview names the plan
#   secrecy     neither the password nor the token reaches stdout or stderr;
#               the token request asks for scope land-<land>
#
# Usage: bash gardenio/scripts/land-door-verify_test.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
UNDER_TEST="${SCRIPT_DIR}/land-door-verify.sh"

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

# Built at run time, so no literal looks like a credential to gitleaks.
TOKEN="stub-token-$$-${RANDOM}"
PASSWORD="stub-pw-$$-${RANDOM}"

cat >"$TMP/stub.py" <<'PY'
import json, os, sys, urllib.parse
from http.server import BaseHTTPRequestHandler, HTTPServer

log = open(os.environ["STUB_LOG"], "a")
token = os.environ["STUB_TOKEN"]

def mode():
    with open(os.environ["STUB_MODE_FILE"]) as f:
        return f.read().strip()

class H(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def answer(self, code, body=b""):
        self.send_response(code)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def record(self, body=""):
        log.write(json.dumps({"method": self.command, "path": self.path,
                              "auth": self.headers.get("Authorization", ""), "body": body}) + "\n")
        log.flush()

    def do_POST(self):
        n = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(n).decode()
        self.record(body)
        if self.path == "/token":
            if mode() == "token-401":
                return self.answer(401, b'{"error":"invalid_grant"}')
            return self.answer(200, json.dumps({"access_token": token}).encode())
        return self.answer(404)

    def do_GET(self):
        self.record()
        if self.headers.get("Authorization") != "Bearer " + token:
            return self.answer(401)
        p = urllib.parse.urlparse(self.path).path
        if p == "/v1/windows":
            return self.answer(403 if mode() == "list-403" else 200, b"[]")
        if p == "/v1/windows/preview":
            return self.answer(200, b"{}")
        return self.answer(404)

srv = HTTPServer(("127.0.0.1", 0), H)
with open(os.environ["STUB_PORT_FILE"], "w") as f:
    f.write(str(srv.server_address[1]))
srv.serve_forever()
PY

echo pass >"$TMP/mode"
STUB_LOG="$TMP/log" STUB_TOKEN="$TOKEN" STUB_MODE_FILE="$TMP/mode" STUB_PORT_FILE="$TMP/port" \
  python3 "$TMP/stub.py" &
STUB_PID=$!
for _ in $(seq 50); do [ -s "$TMP/port" ] && break; sleep 0.1; done
[ -s "$TMP/port" ] || { echo "stub did not start"; exit 1; }
BASE="http://127.0.0.1:$(cat "$TMP/port")"

# run <mode> [extra env...]: runs the script, sets RC, OUT and ERR.
run() {
  echo "$1" >"$TMP/mode"
  shift
  : >"$TMP/log"
  set +e
  env -i PATH="$PATH" HOME="$HOME" \
    LAND_VERIFIER_USERNAME=owner@example.test LAND_VERIFIER_PASSWORD="$PASSWORD" \
    LANDGATE_URL="$BASE" KEYCLOAK_TOKEN_URL="$BASE/token" "$@" \
    bash "$UNDER_TEST" >"$TMP/out" 2>"$TMP/err"
  RC=$?
  set -e
  OUT="$(cat "$TMP/out")"
  ERR="$(cat "$TMP/err")"
}

run pass
if [ "$RC" = 0 ] && [ "$(grep -c '(want' <<<"$OUT")" = 4 ]; then pass "pass"; else fail "pass" "rc=$RC out=$OUT err=$ERR"; fi
if grep -q '"path": "/v1/windows/preview?land=scopetest"' "$TMP/log"; then pass "default target is the land"; else fail "default target" "$(cat "$TMP/log")"; fi
if grep -q 'scope=openid+land-scopetest' "$TMP/log"; then pass "asks for the land scope"; else fail "land scope" "$(cat "$TMP/log")"; fi
if grep -qF -e "$PASSWORD" -e "$TOKEN" <<<"$OUT$ERR"; then fail "secrecy" "a secret reached the output"; else pass "secrecy"; fi

run list-403
if [ "$RC" = 1 ] && grep -q '^1 list *403 (want 200)' <<<"$OUT"; then pass "list-403"; else fail "list-403" "rc=$RC out=$OUT"; fi

run token-401
if [ "$RC" = 2 ] && ! grep -q '/v1/' "$TMP/log"; then pass "token-401"; else fail "token-401" "rc=$RC log=$(cat "$TMP/log")"; fi

run pass LAND_VERIFIER_PASSWORD=
if [ "$RC" = 2 ] && [ ! -s "$TMP/log" ]; then pass "no-creds"; else fail "no-creds" "rc=$RC"; fi

run pass LAND_VERIFIER_PLAN=gd-PLAN1
if [ "$RC" = 0 ] && grep -q '"method": "POST", "path": "/v1/windows/preview?plan=gd-PLAN1"' "$TMP/log"; then pass "plan"; else fail "plan" "rc=$RC log=$(cat "$TMP/log")"; fi

echo "passed $PASS, failed $FAIL"
[ "$FAIL" = 0 ]
