#!/usr/bin/env bash
# test-slackmock-loop.sh — the slackmock VALIDATION LOOP, closed end-to-end.
#
# ── What this proves (task gd-RhOuCeCYUC, epic gd-L1HXcTGzDV §C1) ─────────────
#   slackmock (gardenio/tests/slackmock) is the OUTBOUND prod-fixture that lets
#   E2E exercise the bridge's Slack posting without flooding real channels, and
#   lib-slackmock.sh is the query/assertion foundation the bash Slack modules
#   build on (SLACK_API / slackmock_find_message / slackmock_call_count /
#   slackmock_reset). But the loop was never CLOSED in CI:
#     • The compiled slackmock binary + the lib-slackmock.sh helpers had no
#       module that stood them BOTH up together and round-tripped a message
#       through them — lib-slackmock.sh is sourced by exactly one module today
#       (test-pool-assign-attribution.sh) and only opportunistically, so a drift
#       between the helper's endpoints (`${SLACKMOCK_URL}/mock/messages?...`,
#       `/mock/calls?method=...`) and the server's actual routes ships green.
#     • The bridge's SLACK_API_URL override — the production knob that points the
#       REAL Slack client at slackmock — is set in NO ci/e2e/helm flow, so the
#       redirect that makes slackmock a fixture at all was never exercised
#       end-to-end. The hermetic Go leg below closes THAT half.
#   This module is the loop: it builds + runs the ACTUAL slackmock binary, drives
#   it through lib-slackmock.sh exactly as a real module would, asserts the query
#   helpers round-trip, then runs the in-process Go leg that drives the bridge's
#   REAL production Slack client (buildWebhookSlackClient, honouring
#   SLACK_API_URL) against the same fixture surface.
#
# ── Hermetic HARD gate ────────────────────────────────────────────────────────
#   No pods, no live cluster, no shared-daemon writes, no network egress —
#   everything runs against a localhost slackmock + in-process httptest. So it is
#   green today and is a HARD gate: any leg failure reddens the shard. It SKIPs
#   clean (exit 0) ONLY when the Go toolchain is genuinely absent — a missing
#   toolchain must not be read as a pass, so the skip is LOUD.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-slackmock-loop.sh

set -uo pipefail

red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Repo root = .../gardenio/tests/e2e/scripts → up 4.
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../../.." && pwd)"
SLACKMOCK_DIR="${REPO_ROOT}/gardenio/tests/slackmock"
CONTROLLER_DIR="${REPO_ROOT}/gardenio/controller"
RUN_ID="${RUN_ID:-$$}"

PASS=0
FAIL=0
ERRORS=()

pass() { green "  ✓ $1"; PASS=$((PASS + 1)); }
fail() {
  red "  ✗ $1"
  [ -n "${2:-}" ] && dim "    $2"
  FAIL=$((FAIL + 1))
  ERRORS+=("$1: ${2:-}")
}

# LOUD skip — exit 0 (a missing toolchain must NOT red the suite) with a banner
# so a skip is never silently read as a pass.
skip_all() {
  local why="$1"
  red "════════════════════════════════════════════════════"
  red " SKIP slackmock-loop (LOUD): $why"
  red " This run did NOT assert the slackmock validation loop (compiled fixture"
  red " + lib-slackmock.sh round-trip + bridge SLACK_API_URL override). A green"
  red " suite here proves NOTHING about that loop. The e2e shard is built with a"
  red " Go toolchain + the controller source — if this fires in CI, that regressed."
  red "════════════════════════════════════════════════════"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$why" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

echo "════════════════════════════════════════════════════"
echo " slackmock validation loop E2E (gd-RhOuCeCYUC)"
echo " compiled fixture · lib-slackmock.sh round-trip · bridge SLACK_API_URL override"
echo "════════════════════════════════════════════════════"

# ── Preflight: Go toolchain + both source trees present ─────────────────────
command -v go >/dev/null 2>&1 || skip_all "no go toolchain to build slackmock / run the loop"
[ -f "${SLACKMOCK_DIR}/main.go" ] || skip_all "slackmock source missing under ${SLACKMOCK_DIR}"
[ -d "${CONTROLLER_DIR}/cmd/slack-bridge" ] || skip_all "controller slack-bridge source missing under ${CONTROLLER_DIR}"

# ── Cleanup: kill the mock + scrub the temp bin on exit ─────────────────────
MOCK_PID=""
MOCK_BIN=""
cleanup() {
  [ -n "$MOCK_PID" ] && kill "$MOCK_PID" 2>/dev/null || true
  [ -n "$MOCK_BIN" ] && rm -f "$MOCK_BIN" 2>/dev/null || true
}
trap cleanup EXIT

# ── Leg 1: build + boot the ACTUAL slackmock binary ─────────────────────────
# slackmock is its own go module (`module slackmock`, NOT in go.work), so it is
# built with GOWORK=off — mirroring how its Dockerfile builds it standalone.
blue "[leg build] go build ./gardenio/tests/slackmock (GOWORK=off, standalone module)"
MOCK_BIN="$(mktemp -d)/slackmock"
if (cd "${SLACKMOCK_DIR}" && GOWORK=off go build -o "$MOCK_BIN" . ) 2>"/tmp/slackmock-loop-build-${RUN_ID}.log"; then
  pass "leg build — slackmock compiled ($(stat -c%s "$MOCK_BIN" 2>/dev/null || echo '?') bytes)"
else
  fail "leg build" "go build failed:
$(tail -20 "/tmp/slackmock-loop-build-${RUN_ID}.log" | sed 's/^/      /')"
  # Without the binary the fixture legs cannot run; report + bail with the Go leg
  # still attempted below so a build break is attributed precisely.
fi

# Pick a free-ish localhost port (RUN_ID-derived, clamped into the ephemeral
# range) so parallel shards don't collide. `date`-free for determinism.
MOCK_PORT=$(( 18000 + (RUN_ID % 1000) ))
MOCK_URL="http://127.0.0.1:${MOCK_PORT}"

if [ -x "$MOCK_BIN" ]; then
  blue "[leg boot] slackmock -addr 127.0.0.1:${MOCK_PORT}"
  "$MOCK_BIN" -addr "127.0.0.1:${MOCK_PORT}" >"/tmp/slackmock-loop-server-${RUN_ID}.log" 2>&1 &
  MOCK_PID=$!
  # Poll /healthz until the listener is up (fast; server binds in <1s locally).
  _up=0
  for _i in $(seq 1 20); do
    if curl -sf --max-time 2 -o /dev/null "${MOCK_URL}/healthz" 2>/dev/null; then _up=1; break; fi
    sleep 0.25
  done
  if [ "$_up" = 1 ]; then
    pass "leg boot — slackmock /healthz OK on :${MOCK_PORT}"
  else
    fail "leg boot" "slackmock did not answer /healthz within 5s:
$(tail -10 "/tmp/slackmock-loop-server-${RUN_ID}.log" | sed 's/^/      /')"
  fi
fi

# ── Leg 2: lib-slackmock.sh round-trips against the live compiled binary ────
# Source the LIBRARY UNDER TEST with SLACKMOCK_URL set — the exact contract a
# real module uses. This wires SLACK_API=${SLACKMOCK_URL}/api, SLACK_IS_MOCK=true,
# and the slackmock_* query helpers. We then post via the mock's Slack surface
# and assert the helpers see it — catching any drift between the helper's
# endpoints and the server's routes.
if [ "${_up:-0}" = 1 ]; then
  export SLACKMOCK_URL="$MOCK_URL"
  # shellcheck source=lib-slackmock.sh
  source "${SCRIPT_DIR}/lib-slackmock.sh"

  if [ "${SLACK_IS_MOCK:-false}" = "true" ] && [ "${SLACK_API}" = "${MOCK_URL}/api" ]; then
    pass "leg lib — lib-slackmock.sh selected mock (SLACK_API=${SLACK_API})"
  else
    fail "leg lib" "lib-slackmock.sh did not select the mock: SLACK_IS_MOCK=${SLACK_IS_MOCK:-} SLACK_API=${SLACK_API:-}"
  fi

  slackmock_reset

  CHANNEL="C0LOOPBASH"
  MARKER="slackmock-loop bash probe ${RUN_ID}"

  # Post through the mock's Slack API surface (form-encoded, as slack-go does).
  post_resp="$(curl -sf --max-time 5 -X POST "${SLACK_API}/chat.postMessage" \
    --data-urlencode "channel=${CHANNEL}" \
    --data-urlencode "text=${MARKER}" 2>/dev/null || echo '{"ok":false}')"
  if echo "$post_resp" | python3 -c "import sys,json; sys.exit(0 if json.load(sys.stdin).get('ok') else 1)" 2>/dev/null; then
    pass "leg post — chat.postMessage accepted by the fixture"
  else
    fail "leg post" "chat.postMessage rejected: ${post_resp}"
  fi

  # slackmock_find_message — the outbound-assertion helper the bash modules use.
  found="$(slackmock_find_message "$CHANNEL" "$MARKER" 5 || echo "")"
  if [ -n "$found" ] && grep -q "$MARKER" <<<"$found"; then
    pass "leg find — slackmock_find_message round-tripped the posted message"
  else
    fail "leg find" "slackmock_find_message returned nothing for marker '${MARKER}' (helper↔server endpoint drift?)"
  fi

  # slackmock_call_count — the call-tally helper.
  n="$(slackmock_call_count chat.postMessage)"
  if [ "$n" = "1" ]; then
    pass "leg count — slackmock_call_count chat.postMessage = 1"
  else
    fail "leg count" "slackmock_call_count chat.postMessage = ${n}, want 1"
  fi

  # slackmock_reset must actually clear recorded state.
  slackmock_reset
  n_after="$(slackmock_call_count chat.postMessage)"
  if [ "$n_after" = "0" ]; then
    pass "leg reset — slackmock_reset cleared recorded calls (0 after reset)"
  else
    fail "leg reset" "slackmock_call_count after reset = ${n_after}, want 0"
  fi
fi

# ── Leg 3: the bridge's REAL production Slack client honours SLACK_API_URL ────
# Hermetic in-process Go integration test: drives buildWebhookSlackClient (the
# exact constructor main() uses) against a faithful slackmock twin and asserts
# the override redirect + full outbound round-trip. This is the half the bash
# legs cannot reach (they exercise the fixture from the CLIENT side via curl; the
# Go leg proves the BRIDGE's own client is wired to the fixture).
blue "[leg bridge-client] go test ./cmd/slack-bridge -run TestSlackmockLoopE2E"
golog="/tmp/slackmock-loop-gotest-${RUN_ID}.log"
if (cd "${CONTROLLER_DIR}" && go test -v ./cmd/slack-bridge/ -run 'TestSlackmockLoopE2E' -count=1 -timeout 120s) \
    >"$golog" 2>&1; then
  n=$(grep -cE '^--- PASS' "$golog" 2>/dev/null | tr -d '[:space:]')
  pass "leg bridge-client — ${n:-?} test(s) green (production client honours SLACK_API_URL)"
else
  fail "leg bridge-client" "go test failed:
$(tail -25 "$golog" | sed 's/^/      /')"
fi

echo
blue "════════════════════════════════════════════════════"
total=$((PASS + FAIL))
if [ "$FAIL" -eq 0 ]; then
  green " ✓ All $total legs passed — slackmock validation loop is closed"
else
  red " ✗ $FAIL/$total legs failed"
  echo
  for err in "${ERRORS[@]}"; do red "   - $err"; done
fi
blue "════════════════════════════════════════════════════"

[ "$FAIL" -eq 0 ]
