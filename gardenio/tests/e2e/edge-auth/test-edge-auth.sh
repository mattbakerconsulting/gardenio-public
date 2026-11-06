#!/usr/bin/env bash
# test-edge-auth.sh — E2E smoke for the Stage 3 gardenio-edge-auth shim.
#
# Sibling of test-local-mode.sh. Drives the deployed gardenio-edge-auth
# service through its public surface (healthz + auth/check + optional
# proxy upstreams) and asserts a PAT-authenticated request reaches the
# in-cluster daemons it fronts.
#
# Scope today (ka-0000008114): scaffold-level gates only — pod deployed,
# basic curl with a PAT works against staging. Tampered-HMAC,
# revoked-PAT, and OCSF audit assertions land in follow-ups once the
# corresponding W-B-* implementations land (W-B-10 OCSF, W-B-11 daemon
# trust, W-B-12 Helm deploy, W-B-13/14 CLI --token, W-E-6 POC env).
#
# AVA forwardauth coverage (ka-0000001716, epics ka-0000004294 /
# ka-0000005862): Gates I/J/K exercise the /forwardauth/verify endpoint
# (commits a8d7e5ad8 X-Auth-Request-* emission, 08e9b496d unified-host
# Traefik middleware) and Gate L proves the AVA-mode proxy stamps the same
# HMAC envelope as PAT-mode (commit 5b7159ca1). The portable negatives
# (missing/malformed AVA token) always run; the success/parity gates skip
# cleanly until AVA mode is flipped on per env (EDGE_AUTH_AVA_TRUST_SIGNER)
# and EDGE_AUTH_AVA_TOKEN supplies a valid signed user-context JWT.
#
# Usage:
#   GARDENIO_TOKEN=<gitlab-pat> ./test-edge-auth.sh
#     # or, with explicit endpoint (skips in-cluster discovery):
#   EDGE_AUTH_URL=https://cli.gardenio-e2e.app.e2e.dev.acme.example \
#     GARDENIO_TOKEN=<gitlab-pat> ./test-edge-auth.sh
#     # or, with explicit namespace (auto-port-forwards):
#   NAMESPACE=gardenio-e2e GARDENIO_TOKEN=<gitlab-pat> ./test-edge-auth.sh
#
# Required tools: kubectl (for in-cluster gates), curl, jq, python3.
# When GARDENIO_TOKEN is unset the script SKIPs PAT-bearing gates rather
# than failing — the local/CI scaffold still wants pod-deployed coverage.

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
EDGE_AUTH_DEPLOYMENT="${EDGE_AUTH_DEPLOYMENT:-gardenio-edge-auth}"
EDGE_AUTH_SERVICE="${EDGE_AUTH_SERVICE:-gardenio-edge-auth}"
EDGE_AUTH_PORT="${EDGE_AUTH_PORT:-8080}"
# Hostname for the external gate (matches helm Ingress for the
# unifiedHost + edgeAuth route). When EDGE_AUTH_URL is set, in-cluster
# port-forward discovery is skipped entirely.
EDGE_AUTH_URL="${EDGE_AUTH_URL:-}"
# Unified-host base URL (gardenio.app.e2e.dev.acme.example / the per-run e2e
# unified host). Distinct from EDGE_AUTH_URL: that hits the cli.gardenio shim
# directly; this hits the AVA unified host whose Traefik fans /kd out to the
# shim. Used by gate_unified_kd_not_shadowed to lock in BUG#1 (ka-0000004502):
# the unified-host /kd route must outrank the landing catch-all and reach the
# shim (401), not get shadow-303'd to oauth2-proxy. Skips when unset — the
# consolidated suite (ka-0000007461) wires it in CI.
UNIFIED_HOST_URL="${UNIFIED_HOST_URL:-}"
# Path probed on the unified host. /kd is the daemon HTTP API prefix; the
# shim authenticates before proxying, so a no-auth request must be 401.
UNIFIED_KD_PROBE="${UNIFIED_KD_PROBE:-/kd/v1/health}"
# gb-login browser-session cookie (ka-0000000080). EDGE_AUTH_COOKIE_NAME is
# the oauth2-proxy session cookie name; EDGE_AUTH_TEST_COOKIE, when set, is a
# real session value used to exercise the cookie-auth SUCCESS path (gate
# gate_cookie_auth). Minting a real oauth2-proxy session non-interactively is
# the consolidated suite's job (ka-0000007461); the negative gate
# (gate_cookie_rejected) always runs and needs no real session.
EDGE_AUTH_COOKIE_NAME="${EDGE_AUTH_COOKIE_NAME:-_gardenio_oauth2}"
EDGE_AUTH_TEST_COOKIE="${EDGE_AUTH_TEST_COOKIE:-}"
# Upstream prefix to exercise via the proxy gate. The W-B-9 proxy
# registers /kd/ → kbeads-http by default; override for other upstreams.
EDGE_AUTH_PROXY_PREFIX="${EDGE_AUTH_PROXY_PREFIX:-/kd/}"
# Upstream healthcheck path appended to EDGE_AUTH_PROXY_PREFIX. /healthz
# is unauthenticated on kbeads-http, so a bearer-stamped proxy request
# proves the path reaches the daemon without requiring a writable bead.
EDGE_AUTH_PROXY_PROBE="${EDGE_AUTH_PROXY_PROBE:-v1/health}"
# AVA forwardauth endpoint (ka-0000004294 / ka-0000005862). Traefik's
# `forwardAuth` middleware on the unified host (commit 08e9b496d) calls this
# path; the shim verifies the AWS Verified Access JWT and emits
# X-Auth-Request-{User,Email,Groups}. Ships dark (404 forwardauth_not_configured)
# until EDGE_AUTH_AVA_TRUST_SIGNER is configured per env.
EDGE_AUTH_FORWARDAUTH_PATH="${EDGE_AUTH_FORWARDAUTH_PATH:-/forwardauth/verify}"
# WebSocket-through-shim probe (Gate S, ka-0000003786). The /mux fanout routes
# WS upgrades (/ws/mux, /ws/<id>) through the shim's proxy mode, where the
# caller's Authorization is stripped and identity survives only as the HMAC
# envelope. coopmux's WS handlers must honour that envelope (TrustedOperator)
# exactly like its HTTP handlers — the regression that broke browser chat was
# the WS path ignoring it (403/401 on every socket while the page HTML loaded).
# The probe is /mux/ws/mux: an authenticated upgrade must reach coopmux and get
# a 101 Switching Protocols, NOT a 401/403 from the shim or the daemon.
EDGE_AUTH_MUX_WS_PROBE="${EDGE_AUTH_MUX_WS_PROBE:-/mux/ws/mux}"
# /mux front-door content-type probe (Gate T, ka-0000007632). A PAT request to
# the coopmux SPA root must come back as a SINGLE `Content-Type: text/html`,
# never `application/json` and never a doubled `application/json, text/html`.
# handleProxy sets application/json for its own deny/error paths and
# httputil.ReverseProxy copies upstream headers with Add (not Set), so without
# the explicit Header.Del("Content-Type") before the proxy handoff the dashboard
# came back two-valued and browsers rendered the HTML as text ("front door
# returns a bunch of JavaScript"). The unit guard is
# TestProxy_ForwardsUpstreamContentType; this asserts the same at the live edge.
EDGE_AUTH_MUX_HTML_PROBE="${EDGE_AUTH_MUX_HTML_PROBE:-/mux/}"
# /kd/nats WebSocket route probe (Gate U, ka-0000006000). The shim registers
# /kd/nats/ BEFORE /kd/ (matchUpstream is first-match-by-order), so a realtime
# NATS WebSocket upgrade routes to gardenio-nats:8443 rather than falling through
# to the kbeads daemon (which has no WS handler and 502s the local nats-bridge).
# An authenticated WS upgrade to /kd/nats/ must yield 101 Switching Protocols,
# NOT a 502. The unit guard on the ordering is
# TestBuildUpstreams_NatsRoutePrecedesKbeads; this asserts it reaches NATS live.
EDGE_AUTH_KD_NATS_WS_PROBE="${EDGE_AUTH_KD_NATS_WS_PROBE:-/kd/nats/}"
# Header AWS Verified Access stamps with the signed user-context JWT.
AVA_HEADER_NAME="${AVA_HEADER_NAME:-X-Amzn-Ava-User-Context}"
# Optional valid AVA JWT to exercise the success path (202 + header emission).
# Skips when unset — minting a real ES384-signed AVA JWT is not portable
# across e2e runners, mirroring how gate_proxy_pat skips without a daemon.
EDGE_AUTH_AVA_TOKEN="${EDGE_AUTH_AVA_TOKEN:-}"
# Gate Q (GATING, ka-0000007461): forwardauth-identity WRITE attribution. The
# kbeads daemon resolves operated_by from the in-cluster forwardauth-injected
# identity header (X-Operated-By, the static-token attribution channel that the
# AVA forwardauth → kbeads hop ultimately stamps; see kbeads
# resolveOperatedByPure case "static"/"none"). This is the SERVER-SIDE
# equivalent of the gb-login cookie WRITE path: a request bearing only an
# injected human identity (no agent self-attribution) must land
# operated_by=oidc:<email> on the bead's event log. Driven UNCONDITIONALLY via
# BEADS_HTTP_URL so it is GREEN in the ephemeral e2e ns (gardenio-e2e-N),
# where edge-auth/unified-host are NOT deployed — only the long-lived
# staging singleton deploys them, so the shim gates (M–R) inherently SKIP
# there. BEADS_HTTP_URL + BEADS_TOKEN are exported by run-shard.sh; when unset
# (laptop run of this script alone) the gate SKIPs cleanly.
BEADS_HTTP_URL="${BEADS_HTTP_URL:-}"
BEADS_TOKEN="${BEADS_TOKEN:-}"
# The synthetic human identity the forwardauth path injects for the Gate Q
# write. Must be an oidc:<local>@<domain> shape so kbeads' header sanitizer
# (sanitizeOperatedByHeader / isHumanOperator) accepts it; an agent-shaped or
# sentinel value would be rejected at the boundary and the write would refuse.
# Deliberately distinct from run-shard.sh's ambient BEADS_OPERATED_BY so the
# assertion proves the per-request injected identity wins, not the shard default.
EDGE_AUTH_FWDIDENTITY_EMAIL="${EDGE_AUTH_FWDIDENTITY_EMAIL:-gb-login-e2e@gardenio.dev}"
# Gate R (ka-0000007461): unified-host PAT WRITE + operated_by attribution +
# non-member PAT → 403. UNIFIED_HOST_URL is the AVA unified host (e.g.
# gardenio.app.e2e.dev.acme.example) whose Traefik fans /kd through the edge-auth
# shim; GARDENIO_TOKEN is a member Platform PAT; EDGE_AUTH_NONMEMBER_PAT is a
# valid GitLab PAT for a user OUTSIDE edgeAuth.gitlabGroup. All three SKIP
# cleanly in the ephemeral ns (unified host not deployed, no PAT secret in
# the CI job), mirroring how shim gates M–P skip; the long-lived staging
# singleton supplies them.
EDGE_AUTH_NONMEMBER_PAT="${EDGE_AUTH_NONMEMBER_PAT:-}"
TEST_TAG="edge-auth-$$-$(date +%s)"
PF_PID=""
PF_PORT=""
CURL_TIMEOUT="${CURL_TIMEOUT:-10}"

PASS=0
FAIL=0
SKIP=0
ERRORS=()

# Optional sink for the curl transcripts, matched to test-local-mode.sh
# conventions. CI sets it to junit-results-${E2E_SHARD}/edge-auth-logs.
LOG_ARCHIVE_DIR="${LOG_ARCHIVE_DIR:-}"
if [ -n "$LOG_ARCHIVE_DIR" ]; then
  mkdir -p "$LOG_ARCHIVE_DIR" || LOG_ARCHIVE_DIR=""
fi

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
skip() {
  local name="$1" reason="${2:-}"
  dim "  ⊘ $name (skipped: $reason)"
  SKIP=$((SKIP + 1))
}

have() { command -v "$1" >/dev/null 2>&1; }

archive() {
  local name="$1" body="$2"
  [ -z "$LOG_ARCHIVE_DIR" ] && return 0
  [ -d "$LOG_ARCHIVE_DIR" ] || return 0
  printf '%s\n' "$body" > "$LOG_ARCHIVE_DIR/${name}.log" 2>/dev/null || true
}

# ── Port-forward management ───────────────────────────────────────
spawn_portforward() {
  local port=19100
  while ss -lnt 2>/dev/null | awk '{print $4}' | grep -q ":${port}$"; do
    port=$((port + 1))
    [ "$port" -gt 19200 ] && return 1
  done
  local logf="/tmp/edge-auth-pf-${TEST_TAG}.log"
  kubectl -n "$NAMESPACE" port-forward "svc/${EDGE_AUTH_SERVICE}" \
    "${port}:${EDGE_AUTH_PORT}" >"$logf" 2>&1 &
  PF_PID=$!
  PF_PORT=$port
  # Give kubectl up to 10s to bind.
  local start=$SECONDS
  while [ $((SECONDS - start)) -lt 10 ]; do
    if curl -sf --max-time 2 "http://127.0.0.1:${PF_PORT}/healthz" >/dev/null 2>&1; then
      return 0
    fi
    if ! kill -0 "$PF_PID" 2>/dev/null; then
      return 1
    fi
    sleep 1
  done
  return 1
}

cleanup() {
  if [ -n "$PF_PID" ] && kill -0 "$PF_PID" 2>/dev/null; then
    kill "$PF_PID" 2>/dev/null || true
    wait "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# Resolve the curl-able base URL. Either:
#   - EDGE_AUTH_URL preset (laptop/CI hitting the public Ingress); or
#   - kubectl available + svc reachable → spawn a port-forward.
resolve_endpoint() {
  if [ -n "$EDGE_AUTH_URL" ]; then
    blue "Using EDGE_AUTH_URL=$EDGE_AUTH_URL (skipping in-cluster discovery)"
    return 0
  fi
  if ! have kubectl; then
    red "ERROR: EDGE_AUTH_URL not set and kubectl not in PATH"
    exit 1
  fi
  if ! kubectl -n "$NAMESPACE" get "deploy/${EDGE_AUTH_DEPLOYMENT}" >/dev/null 2>&1; then
    skip "endpoint resolution" "deploy/${EDGE_AUTH_DEPLOYMENT} not found in ns/${NAMESPACE} — edge-auth not yet deployed (see W-B-12)"
    SKIP_REMAINING=1
    return 0
  fi
  blue "Port-forwarding ${NAMESPACE}/${EDGE_AUTH_SERVICE} → :${EDGE_AUTH_PORT}..."
  if ! spawn_portforward; then
    fail "port-forward to ${EDGE_AUTH_SERVICE}" "kubectl port-forward did not bind / healthz unreachable"
    SKIP_REMAINING=1
    return 0
  fi
  EDGE_AUTH_URL="http://127.0.0.1:${PF_PORT}"
  dim "  Endpoint: $EDGE_AUTH_URL (pf pid ${PF_PID})"
}

# ── Gates ─────────────────────────────────────────────────────────

# Gate A: deployment is present and at least one pod is Ready.
gate_pod_deployed() {
  blue "Gate A: edge-auth pod deployed"
  if [ -n "${EDGE_AUTH_URL_PRESET:-}" ]; then
    skip "pod ready in ${NAMESPACE}" "EDGE_AUTH_URL preset — no kubectl gate"
    return 0
  fi
  if ! have kubectl; then
    skip "pod ready in ${NAMESPACE}" "kubectl not in PATH"
    return 0
  fi
  if ! kubectl -n "$NAMESPACE" get "deploy/${EDGE_AUTH_DEPLOYMENT}" >/dev/null 2>&1; then
    # Scaffold-mode skip: the runner is meant to be safe to invoke
    # before the deployment lands (W-B-12). REQUIRE_DEPLOYMENT=1 flips
    # this to a hard fail for CI environments that must enforce the
    # gate (e.g. once the Helm chart deploys edge-auth in staging).
    if [ "${REQUIRE_DEPLOYMENT:-0}" = "1" ]; then
      fail "deploy/${EDGE_AUTH_DEPLOYMENT} exists in ns/${NAMESPACE}" \
        "deployment missing — gated on W-B-12 (Helm chart edge-auth deployment)"
    else
      skip "deploy/${EDGE_AUTH_DEPLOYMENT} exists in ns/${NAMESPACE}" \
        "deployment missing — gated on W-B-12 (set REQUIRE_DEPLOYMENT=1 to make this a hard fail)"
    fi
    return 0
  fi
  pass "deploy/${EDGE_AUTH_DEPLOYMENT} exists in ns/${NAMESPACE}"

  local ready
  ready="$(kubectl -n "$NAMESPACE" get "deploy/${EDGE_AUTH_DEPLOYMENT}" \
    -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)"
  ready="${ready:-0}"
  if [ "$ready" -ge 1 ]; then
    pass "deploy/${EDGE_AUTH_DEPLOYMENT} has ${ready} ready replica(s)"
  else
    fail "deploy/${EDGE_AUTH_DEPLOYMENT} has >=1 ready replica" "readyReplicas=${ready}"
  fi
}

# Gate B: /healthz reachable through the resolved endpoint.
gate_healthz() {
  blue "Gate B: /healthz reachable"
  if [ -z "$EDGE_AUTH_URL" ]; then
    skip "/healthz" "no endpoint resolved"
    return 0
  fi
  local body http_code rc=0
  body="$(curl -sS --max-time "$CURL_TIMEOUT" -w '\n__HTTP__:%{http_code}' \
    "${EDGE_AUTH_URL}/healthz" 2>&1)" || rc=$?
  http_code="$(printf '%s' "$body" | awk -F: '/__HTTP__:/{print $2}' | tr -d '\r ')"
  archive "healthz" "$body"
  if [ "$rc" -ne 0 ]; then
    fail "/healthz" "curl exit $rc — endpoint unreachable"
    return 0
  fi
  if [ "$http_code" = "200" ]; then
    pass "/healthz HTTP 200"
  else
    fail "/healthz HTTP 200" "got $http_code"
  fi
}

# Gate C: /auth/check rejects missing token with 401.
gate_authcheck_unauth() {
  blue "Gate C: /auth/check rejects missing token"
  if [ -z "$EDGE_AUTH_URL" ]; then
    skip "/auth/check no-token" "no endpoint resolved"
    return 0
  fi
  local http_code rc=0
  http_code="$(curl -sS --max-time "$CURL_TIMEOUT" -o /dev/null -w '%{http_code}' \
    "${EDGE_AUTH_URL}/auth/check" 2>/dev/null)" || rc=$?
  if [ "$rc" -ne 0 ]; then
    fail "/auth/check no-token" "curl exit $rc"
    return 0
  fi
  case "$http_code" in
    401|503)
      # 503 is acceptable when validator-not-configured (pre-secret rollout).
      pass "/auth/check no-token returns ${http_code}"
      ;;
    *)
      fail "/auth/check no-token returns 401|503" "got $http_code"
      ;;
  esac
}

# Gate D: /auth/check with GARDENIO_TOKEN returns 200 + JSON identity.
gate_authcheck_pat() {
  blue "Gate D: /auth/check with PAT"
  if [ -z "$EDGE_AUTH_URL" ]; then
    skip "/auth/check with PAT" "no endpoint resolved"
    return 0
  fi
  if [ -z "${GARDENIO_TOKEN:-}" ]; then
    skip "/auth/check with PAT" "GARDENIO_TOKEN not set — supply a GitLab PAT to exercise the auth path"
    return 0
  fi
  local body http_code rc=0
  body="$(curl -sS --max-time "$CURL_TIMEOUT" -w '\n__HTTP__:%{http_code}' \
    -H "Authorization: Bearer ${GARDENIO_TOKEN}" \
    "${EDGE_AUTH_URL}/auth/check" 2>&1)" || rc=$?
  http_code="$(printf '%s' "$body" | awk -F: '/__HTTP__:/{print $2}' | tr -d '\r ')"
  # Strip the trailer for archival/jq.
  local json
  json="$(printf '%s' "$body" | sed '/__HTTP__:/d')"
  archive "authcheck-pat" "$body"
  if [ "$rc" -ne 0 ]; then
    fail "/auth/check with PAT" "curl exit $rc"
    return 0
  fi
  if [ "$http_code" != "200" ]; then
    fail "/auth/check with PAT HTTP 200" "got $http_code — body: $(printf '%s' "$json" | head -c 200)"
    return 0
  fi
  pass "/auth/check with PAT HTTP 200"
  # Light shape check — must have a username or email field.
  if have jq; then
    local user
    user="$(printf '%s' "$json" | jq -r '.username // .email // empty' 2>/dev/null)"
    if [ -n "$user" ]; then
      pass "/auth/check returns operator identity (${user})"
    else
      fail "/auth/check returns operator identity" "no username/email in response: $(printf '%s' "$json" | head -c 200)"
    fi
  else
    skip "/auth/check identity shape" "jq not in PATH"
  fi
}

# Gate F: invalid PAT → 401 invalid_token at the proxy path.
# Distinct boundary from gate_authcheck_unauth (missing-token): proves the
# shim actually validates the PAT against gitlab.com /user rather than
# passing anything-looking-like-a-bearer through. Regression guard for
# the "/groups/<g> 404 → everyone denied" misconfig pattern.
gate_proxy_invalid_pat() {
  blue "Gate F: invalid PAT at proxy → 401"
  if [ -z "$EDGE_AUTH_URL" ]; then
    skip "${EDGE_AUTH_PROXY_PREFIX}healthz invalid-PAT" "no endpoint resolved"
    return 0
  fi
  local target="${EDGE_AUTH_URL%/}${EDGE_AUTH_PROXY_PREFIX}healthz"
  local http_code rc=0
  http_code="$(curl -sS --max-time "$CURL_TIMEOUT" -o /dev/null -w '%{http_code}' \
    -H "Authorization: Bearer glpat-invalid-test-token-${TEST_TAG}" \
    "$target" 2>/dev/null)" || rc=$?
  if [ "$rc" -ne 0 ]; then
    fail "invalid-PAT proxy" "curl exit $rc"
    return 0
  fi
  if [ "$http_code" = "401" ]; then
    pass "invalid PAT at proxy → 401"
  else
    fail "invalid PAT at proxy → 401" "got $http_code (shim must reject unverifiable PATs, not pass through)"
  fi
}

# Gate G: `kd --token` end-to-end through the shim.
# Locks in W-B-13 (gd-DyEsW3BsVC). Skips when `kd` not in PATH (laptops
# without the gardenio binaries; CI agent image has it).
gate_kd_token_e2e() {
  blue "Gate G: kd --token end-to-end via shim"
  if [ -z "$EDGE_AUTH_URL" ]; then
    skip "kd --token list" "no endpoint resolved"
    return 0
  fi
  if [ -z "${GARDENIO_TOKEN:-}" ]; then
    skip "kd --token list" "GARDENIO_TOKEN not set"
    return 0
  fi
  if ! have kd; then
    skip "kd --token list" "kd not in PATH"
    return 0
  fi
  local out rc=0
  out="$(kd --http-url "${EDGE_AUTH_URL%/}/kd" --token "$GARDENIO_TOKEN" \
    list --limit 1 2>&1)" || rc=$?
  archive "gd-token-list" "$out"
  if [ "$rc" -ne 0 ]; then
    fail "kd --token list via shim" "exit $rc — $(printf '%s' "$out" | head -c 200)"
    return 0
  fi
  pass "kd --token list via shim → exit 0"
}

# Gate H: `gb --token` end-to-end through the shim.
# Locks in the gb-token fix (MR !3117 against ka-0000003384 epic). Without
# this fix, gb sent only BEADS_DAEMON_TOKEN and got 401 invalid_token from
# the shim, blocking the local-agent loop off-VPN.
gate_gb_token_e2e() {
  blue "Gate H: gb --token end-to-end via shim"
  if [ -z "$EDGE_AUTH_URL" ]; then
    skip "gb --token news" "no endpoint resolved"
    return 0
  fi
  if [ -z "${GARDENIO_TOKEN:-}" ]; then
    skip "gb --token news" "GARDENIO_TOKEN not set"
    return 0
  fi
  if ! have gb; then
    skip "gb --token news" "gb not in PATH"
    return 0
  fi
  # MR !3117 added --token to gb. Skip cleanly when the binary in PATH
  # predates that change rather than failing — protects environments
  # that haven't picked up the new agent image / local gb yet.
  if ! gb --help 2>&1 | grep -q -- '--token'; then
    skip "gb --token news" "gb in PATH predates --token (MR !3117, ka-0000003384) — rebuild gb or rebuild agent image"
    return 0
  fi
  local out rc=0
  out="$(gb --http-url "${EDGE_AUTH_URL%/}/kd" --token "$GARDENIO_TOKEN" \
    news 2>&1)" || rc=$?
  archive "gb-token-news" "$out"
  if [ "$rc" -ne 0 ]; then
    fail "gb --token news via shim" "exit $rc — $(printf '%s' "$out" | head -c 200)"
    return 0
  fi
  pass "gb --token news via shim → exit 0"
}

# Gate I: forwardauth endpoint disposition for a missing AVA token.
# Locks in the AVA forwardauth path (commits a8d7e5ad8 emit headers,
# 08e9b496d wires the unified-host Traefik middleware). The endpoint is
# mounted unconditionally (server.go RegisterRoutes) so a misrouted
# IngressRoute surfaces loudly:
#   - AVA mode OFF (avaTrustSigner empty, the staging default):
#       404 {"error":"forwardauth_not_configured"}
#   - AVA mode ON but no X-Amzn-Ava-User-Context header:
#       401 {"error":"missing_ava_token"}
# Either disposition is a portable PASS — both prove the dark-ship guard
# and the no-token branch behave as designed without needing a signed JWT.
# A 2xx here would be a real bug: the endpoint must never allow a request
# that carries no AVA context.
gate_forwardauth_no_token() {
  blue "Gate I: forwardauth /verify rejects missing AVA token"
  if [ -z "$EDGE_AUTH_URL" ]; then
    skip "${EDGE_AUTH_FORWARDAUTH_PATH} no-token" "no endpoint resolved"
    return 0
  fi
  local target="${EDGE_AUTH_URL%/}${EDGE_AUTH_FORWARDAUTH_PATH}"
  local body http_code rc=0
  body="$(curl -sS --max-time "$CURL_TIMEOUT" -w '\n__HTTP__:%{http_code}' \
    "$target" 2>&1)" || rc=$?
  http_code="$(printf '%s' "$body" | awk -F: '/__HTTP__:/{print $2}' | tr -d '\r ')"
  local json
  json="$(printf '%s' "$body" | sed '/__HTTP__:/d')"
  archive "forwardauth-no-token" "$body"
  if [ "$rc" -ne 0 ]; then
    fail "forwardauth no-token" "curl exit $rc"
    return 0
  fi
  case "$http_code" in
    404)
      # Dark-ship: avaVerifier nil → forwardauth_not_configured.
      if grep -q 'forwardauth_not_configured' <<<"$json"; then
        pass "forwardauth no-token → 404 forwardauth_not_configured (AVA mode OFF)"
      else
        fail "forwardauth no-token → 404 body" \
          "expected forwardauth_not_configured, got: $(printf '%s' "$json" | head -c 200)"
      fi
      ;;
    401)
      # AVA mode ON: header absent → missing_ava_token.
      if grep -q 'missing_ava_token' <<<"$json"; then
        pass "forwardauth no-token → 401 missing_ava_token (AVA mode ON)"
      else
        fail "forwardauth no-token → 401 body" \
          "expected missing_ava_token, got: $(printf '%s' "$json" | head -c 200)"
      fi
      ;;
    *)
      fail "forwardauth no-token → 404|401" \
        "got $http_code — endpoint must reject requests with no AVA context, never 2xx"
      ;;
  esac
}

# Gate J: forwardauth endpoint rejects a malformed/untrusted AVA JWT.
# Distinct boundary from gate_forwardauth_no_token (header absent): proves
# the shim actually verifies the AVA JWT signature rather than trusting any
# value present in X-Amzn-Ava-User-Context. With AVA mode OFF the dark-ship
# 404 still wins (the verifier guard short-circuits before signature check);
# with AVA mode ON a garbage token must yield 401 invalid_ava_token (or
# missing_ava_token if the verifier rejects the shape as unparseable). A
# 202/2xx here would mean the endpoint forwards an unverified principal to
# coopmux — the exact failure the AVA path exists to prevent.
gate_forwardauth_bad_token() {
  blue "Gate J: forwardauth /verify rejects malformed AVA JWT"
  if [ -z "$EDGE_AUTH_URL" ]; then
    skip "${EDGE_AUTH_FORWARDAUTH_PATH} bad-token" "no endpoint resolved"
    return 0
  fi
  local target="${EDGE_AUTH_URL%/}${EDGE_AUTH_FORWARDAUTH_PATH}"
  local body http_code rc=0
  body="$(curl -sS --max-time "$CURL_TIMEOUT" -w '\n__HTTP__:%{http_code}' \
    -H "${AVA_HEADER_NAME}: not-a-real-jwt.${TEST_TAG}.garbage" \
    "$target" 2>&1)" || rc=$?
  http_code="$(printf '%s' "$body" | awk -F: '/__HTTP__:/{print $2}' | tr -d '\r ')"
  local json
  json="$(printf '%s' "$body" | sed '/__HTTP__:/d')"
  archive "forwardauth-bad-token" "$body"
  if [ "$rc" -ne 0 ]; then
    fail "forwardauth bad-token" "curl exit $rc"
    return 0
  fi
  case "$http_code" in
    404)
      # AVA mode OFF — dark-ship guard short-circuits before verifying.
      skip "forwardauth bad-token signature check" \
        "AVA mode OFF (404 forwardauth_not_configured) — signature path gated on EDGE_AUTH_AVA_TRUST_SIGNER"
      ;;
    401)
      if grep -qE 'invalid_ava_token|missing_ava_token' <<<"$json"; then
        pass "forwardauth malformed JWT → 401 (shim verifies signature, no pass-through)"
      else
        fail "forwardauth bad-token → 401 body" \
          "expected invalid_ava_token, got: $(printf '%s' "$json" | head -c 200)"
      fi
      ;;
    *)
      fail "forwardauth bad-token → 401" \
        "got $http_code — shim must reject unverifiable AVA JWTs, never emit a 2xx allow"
      ;;
  esac
}

# Gate K: forwardauth success path stamps oauth2-proxy-shaped headers.
# Locks in commit a8d7e5ad8: a verified AVA JWT yields 202 + the trio of
# X-Auth-Request-{User,Email,Groups} headers that coopmux + viewer consume
# (the same headers oauth2-proxy emits today, so no Rust changes). Skips
# cleanly when no valid AVA token is available — minting an ES384-signed
# AVA user-context JWT is not portable across e2e runners. Supply
# EDGE_AUTH_AVA_TOKEN (and an AVA-mode-ON endpoint) to exercise it.
gate_forwardauth_success() {
  blue "Gate K: forwardauth /verify success → 202 + X-Auth-Request-* headers"
  if [ -z "$EDGE_AUTH_URL" ]; then
    skip "${EDGE_AUTH_FORWARDAUTH_PATH} success" "no endpoint resolved"
    return 0
  fi
  if [ -z "$EDGE_AUTH_AVA_TOKEN" ]; then
    skip "${EDGE_AUTH_FORWARDAUTH_PATH} success" \
      "EDGE_AUTH_AVA_TOKEN not set — supply a valid AVA user-context JWT to exercise header emission (commit a8d7e5ad8)"
    return 0
  fi
  local target="${EDGE_AUTH_URL%/}${EDGE_AUTH_FORWARDAUTH_PATH}"
  local headers_file="/tmp/edge-auth-forwardauth-ok-${TEST_TAG}.headers"
  local http_code rc=0
  http_code="$(curl -sS --max-time "$CURL_TIMEOUT" -o /dev/null -D "$headers_file" \
    -w '%{http_code}' \
    -H "${AVA_HEADER_NAME}: ${EDGE_AUTH_AVA_TOKEN}" \
    "$target" 2>/dev/null)" || rc=$?
  archive "forwardauth-success-headers" "$(cat "$headers_file" 2>/dev/null)"
  if [ "$rc" -ne 0 ]; then
    fail "forwardauth success" "curl exit $rc"
    return 0
  fi
  case "$http_code" in
    404|502|503)
      skip "forwardauth success" \
        "HTTP ${http_code} — AVA mode not active / verifier unavailable (gated on EDGE_AUTH_AVA_TRUST_SIGNER + reachable AVA JWKS)"
      return 0
      ;;
    202|200)
      pass "forwardauth verified AVA JWT → ${http_code} (2xx allow)"
      ;;
    *)
      fail "forwardauth success → 202" "got $http_code with a token expected to verify"
      return 0
      ;;
  esac
  # oauth2-proxy parity: User + Email must both be present and equal (the
  # shim mirrors Email into User); Groups is emitted only when non-empty so
  # we assert presence of the User/Email pair, not Groups.
  local user email
  user="$(awk -F': ' 'tolower($1)=="x-auth-request-user"{gsub(/\r/,"");print $2}' "$headers_file" 2>/dev/null | head -1)"
  email="$(awk -F': ' 'tolower($1)=="x-auth-request-email"{gsub(/\r/,"");print $2}' "$headers_file" 2>/dev/null | head -1)"
  if [ -n "$user" ]; then
    pass "forwardauth 202 emits X-Auth-Request-User (${user})"
  else
    fail "forwardauth X-Auth-Request-User" "header missing on 202 response"
  fi
  if [ -n "$email" ]; then
    pass "forwardauth 202 emits X-Auth-Request-Email (${email})"
  else
    fail "forwardauth X-Auth-Request-Email" "header missing on 202 response"
  fi
  if grep -qiE '^X-Auth-Audit-Id:' "$headers_file" 2>/dev/null; then
    pass "forwardauth 202 emits X-Auth-Audit-Id"
  else
    fail "forwardauth X-Auth-Audit-Id" "audit-id header missing on 202 response"
  fi
}

# Gate L: AVA-mode proxy request reaches the daemon via the same HMAC
# envelope as PAT-mode. Locks in commit 5b7159ca1: the AVA arm of
# handleProxy stamps the identical X-Operator / X-Auth-Audit-Id /
# X-Edge-Auth-Sig / X-Edge-Auth-Ts envelope the kbeads daemon already
# trusts, so the daemon-side validator is unchanged and indifferent to
# which edge a request crossed. This drives the SAME proxy upstream as
# gate_proxy_pat but authenticates with an AVA JWT instead of a PAT —
# a 200 proves HMAC parity end-to-end. Skips without an AVA token /
# AVA-mode-ON endpoint (not portable across e2e runners).
gate_proxy_ava_hmac_parity() {
  blue "Gate L: AVA-mode proxy reaches daemon (HMAC envelope parity)"
  if [ -z "$EDGE_AUTH_URL" ]; then
    skip "${EDGE_AUTH_PROXY_PREFIX}${EDGE_AUTH_PROXY_PROBE} via AVA" "no endpoint resolved"
    return 0
  fi
  if [ -z "$EDGE_AUTH_AVA_TOKEN" ]; then
    skip "${EDGE_AUTH_PROXY_PREFIX}${EDGE_AUTH_PROXY_PROBE} via AVA" \
      "EDGE_AUTH_AVA_TOKEN not set — supply a valid AVA user-context JWT to prove HMAC parity (commit 5b7159ca1)"
    return 0
  fi
  local target="${EDGE_AUTH_URL%/}${EDGE_AUTH_PROXY_PREFIX}${EDGE_AUTH_PROXY_PROBE}"
  local http_code rc=0
  http_code="$(curl -sS --max-time "$CURL_TIMEOUT" -o /dev/null -w '%{http_code}' \
    -H "${AVA_HEADER_NAME}: ${EDGE_AUTH_AVA_TOKEN}" \
    "$target" 2>/dev/null)" || rc=$?
  if [ "$rc" -ne 0 ]; then
    fail "${EDGE_AUTH_PROXY_PREFIX}${EDGE_AUTH_PROXY_PROBE} via AVA" "curl exit $rc"
    return 0
  fi
  case "$http_code" in
    200)
      pass "${EDGE_AUTH_PROXY_PREFIX}${EDGE_AUTH_PROXY_PROBE} via AVA → 200 (HMAC envelope accepted by daemon, parity with PAT-mode)"
      ;;
    401|404|503)
      # AVA mode off / upstream not wired / daemon trust not yet enabled.
      skip "${EDGE_AUTH_PROXY_PREFIX}${EDGE_AUTH_PROXY_PROBE} via AVA" \
        "HTTP ${http_code} — AVA mode or proxy upstream not active (gated on EDGE_AUTH_AVA_TRUST_SIGNER + W-B-11/12)"
      ;;
    *)
      fail "${EDGE_AUTH_PROXY_PREFIX}${EDGE_AUTH_PROXY_PROBE} via AVA → 200" "got $http_code"
      ;;
  esac
}

# Gate E: PAT-stamped proxy request reaches a fronted daemon.
# /kd/healthz (or whatever EDGE_AUTH_PROXY_PREFIX+PROBE point to) should
# respond 200 when edge-auth has stamped X-Operator + HMAC and the daemon
# trusts the header. Until W-B-11/12 land the daemon-side trust + Helm
# deploy this will skip cleanly.
gate_proxy_pat() {
  blue "Gate E: PAT-stamped proxy request reaches daemon"
  if [ -z "$EDGE_AUTH_URL" ]; then
    skip "${EDGE_AUTH_PROXY_PREFIX}${EDGE_AUTH_PROXY_PROBE}" "no endpoint resolved"
    return 0
  fi
  if [ -z "${GARDENIO_TOKEN:-}" ]; then
    skip "${EDGE_AUTH_PROXY_PREFIX}${EDGE_AUTH_PROXY_PROBE}" "GARDENIO_TOKEN not set"
    return 0
  fi
  local target="${EDGE_AUTH_URL%/}${EDGE_AUTH_PROXY_PREFIX}${EDGE_AUTH_PROXY_PROBE}"
  local http_code rc=0
  http_code="$(curl -sS --max-time "$CURL_TIMEOUT" -o /dev/null -w '%{http_code}' \
    -H "Authorization: Bearer ${GARDENIO_TOKEN}" \
    "$target" 2>/dev/null)" || rc=$?
  if [ "$rc" -ne 0 ]; then
    fail "${EDGE_AUTH_PROXY_PREFIX}${EDGE_AUTH_PROXY_PROBE}" "curl exit $rc"
    return 0
  fi
  case "$http_code" in
    200)
      pass "${EDGE_AUTH_PROXY_PREFIX}${EDGE_AUTH_PROXY_PROBE} via PAT → 200"
      ;;
    404|503)
      # Proxy upstream not wired yet — gated on W-B-11/12/E-6.
      skip "${EDGE_AUTH_PROXY_PREFIX}${EDGE_AUTH_PROXY_PROBE} via PAT" \
        "HTTP ${http_code} — upstream not configured (W-B-12 Helm + W-B-11 daemon trust + W-E-6 POC env)"
      ;;
    *)
      fail "${EDGE_AUTH_PROXY_PREFIX}${EDGE_AUTH_PROXY_PROBE} via PAT → 200" "got $http_code"
      ;;
  esac
}

# Gate M: unified-host /kd is NOT shadowed by the landing catch-all.
# Locks in BUG#1 (ka-0000004502): the landing IngressRoute is a
# Host(`unifiedHost`) catch-all at priority 10 with the oauth2-proxy
# forwardauth middleware. Before the fix, the unified-host /kd route had no
# explicit priority and lost to landing, so gardenio.app/kd 303'd to
# gitlab.com/oauth/authorize instead of reaching the shim. The fix stamps
# priority 100 on the viaShim routes; this gate proves a no-auth request to
# the unified host's /kd path reaches the shim (401 invalid/missing token),
# NOT a 303 redirect to OIDC. A 303 here is the BUG#1 regression. Skips when
# UNIFIED_HOST_URL is unset — the consolidated suite (ka-0000007461) wires it.
gate_unified_kd_not_shadowed() {
  blue "Gate M: unified-host /kd reaches shim (not oauth2-proxy shadow)"
  if [ -z "$UNIFIED_HOST_URL" ]; then
    skip "unified-host ${UNIFIED_KD_PROBE} not shadowed" \
      "UNIFIED_HOST_URL not set — supply the AVA unified host (e.g. gardenio.app.e2e.dev.acme.example) to exercise BUG#1"
    return 0
  fi
  local target="${UNIFIED_HOST_URL%/}${UNIFIED_KD_PROBE}"
  # -sS but NO -L: a 303 must surface as the status, not be followed.
  local http_code rc=0
  http_code="$(curl -sS --max-time "$CURL_TIMEOUT" -o /dev/null -w '%{http_code}' \
    "$target" 2>/dev/null)" || rc=$?
  if [ "$rc" -ne 0 ]; then
    fail "unified-host ${UNIFIED_KD_PROBE} no-auth" "curl exit $rc — $target unreachable"
    return 0
  fi
  case "$http_code" in
    303|302|307)
      fail "unified-host ${UNIFIED_KD_PROBE} reaches shim (401), not OIDC redirect" \
        "got $http_code — BUG#1 regression: /kd is shadow-redirected to oauth2-proxy by the landing catch-all (ka-0000004502). The viaShim route needs priority > landing (10)."
      ;;
    401)
      pass "unified-host ${UNIFIED_KD_PROBE} no-auth → 401 (reaches shim, not shadowed)"
      ;;
    200)
      # Acceptable only if the probe path is unauthenticated upstream; still
      # proves the route is NOT the oauth2-proxy 303 shadow.
      pass "unified-host ${UNIFIED_KD_PROBE} no-auth → 200 (reaches upstream, not shadowed)"
      ;;
    *)
      fail "unified-host ${UNIFIED_KD_PROBE} no-auth → 401 (shim)" \
        "got $http_code — expected 401 from the shim; a 3xx would be the oauth2-proxy shadow (BUG#1)"
      ;;
  esac
}

# Gate N: unified-host /kd with a PAT reaches the daemon through the shim.
# Companion to Gate M's negative: once the route is un-shadowed, a valid PAT
# must traverse the unified host → shim → daemon exactly like the cli.gardenio
# path (Gate E). Skips without UNIFIED_HOST_URL or GARDENIO_TOKEN.
gate_unified_kd_pat() {
  blue "Gate N: unified-host /kd with PAT reaches daemon"
  if [ -z "$UNIFIED_HOST_URL" ]; then
    skip "unified-host ${UNIFIED_KD_PROBE} with PAT" "UNIFIED_HOST_URL not set"
    return 0
  fi
  if [ -z "${GARDENIO_TOKEN:-}" ]; then
    skip "unified-host ${UNIFIED_KD_PROBE} with PAT" "GARDENIO_TOKEN not set"
    return 0
  fi
  local target="${UNIFIED_HOST_URL%/}${UNIFIED_KD_PROBE}"
  local http_code rc=0
  http_code="$(curl -sS --max-time "$CURL_TIMEOUT" -o /dev/null -w '%{http_code}' \
    -H "Authorization: Bearer ${GARDENIO_TOKEN}" \
    "$target" 2>/dev/null)" || rc=$?
  if [ "$rc" -ne 0 ]; then
    fail "unified-host ${UNIFIED_KD_PROBE} with PAT" "curl exit $rc"
    return 0
  fi
  case "$http_code" in
    200)
      pass "unified-host ${UNIFIED_KD_PROBE} via PAT → 200 (shim → daemon, unified host)"
      ;;
    303|302|307)
      fail "unified-host ${UNIFIED_KD_PROBE} via PAT → 200" \
        "got $http_code — PAT request shadow-redirected to OIDC (BUG#1, ka-0000004502)"
      ;;
    401|404|503)
      skip "unified-host ${UNIFIED_KD_PROBE} via PAT" \
        "HTTP ${http_code} — upstream/daemon trust not active in this env (gated like Gate E)"
      ;;
    *)
      fail "unified-host ${UNIFIED_KD_PROBE} via PAT → 200" "got $http_code"
      ;;
  esac
}

# Gate S: an authenticated WebSocket upgrade reaches coopmux THROUGH the shim
# (ka-0000003786). This is the regression that broke browser chat: /mux routes
# WS upgrades through the shim's proxy mode, which strips the caller's
# Authorization and forwards identity only as the HMAC envelope. coopmux's HTTP
# handlers honoured that envelope (TrustedOperator) but its WS handlers did not
# — so the dashboard HTML loaded while every chat/feed socket 401'd. A PAT
# crossing the shim must yield 101 Switching Protocols on /mux/ws/mux, NOT a
# 401/403. A non-101 here is the regression. Skips when the shim has no PAT or
# the coopmux upstream isn't wired (mirrors Gate E gating).
gate_mux_ws_through_shim() {
  blue "Gate S: WebSocket upgrade reaches coopmux through the shim (101)"
  if [ -z "$EDGE_AUTH_URL" ]; then
    skip "${EDGE_AUTH_MUX_WS_PROBE} WS upgrade" "no endpoint resolved"
    return 0
  fi
  if [ -z "${GARDENIO_TOKEN:-}" ]; then
    skip "${EDGE_AUTH_MUX_WS_PROBE} WS upgrade" "GARDENIO_TOKEN not set"
    return 0
  fi
  local target="${EDGE_AUTH_URL%/}${EDGE_AUTH_MUX_WS_PROBE}"
  # Send a real WS upgrade handshake. A successful upgrade returns 101; the
  # shim/daemon reject (401/403) surfaces as that status. --max-time bounds the
  # socket so a successful 101 (which would otherwise hold the connection open)
  # returns promptly. We do NOT follow redirects: a 3xx is itself a failure.
  local http_code rc=0
  http_code="$(curl -sS --max-time "$CURL_TIMEOUT" -o /dev/null -w '%{http_code}' \
    -H "Authorization: Bearer ${GARDENIO_TOKEN}" \
    -H "Connection: Upgrade" \
    -H "Upgrade: websocket" \
    -H "Sec-WebSocket-Version: 13" \
    -H "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==" \
    "$target" 2>/dev/null)" || rc=$?
  # curl exit 28 (timeout) on a held-open 101 socket is acceptable when the
  # status line was already 101; -w prints it before the timeout fires.
  if [ "$rc" -ne 0 ] && [ "$http_code" != "101" ]; then
    fail "${EDGE_AUTH_MUX_WS_PROBE} WS upgrade" "curl exit $rc — $(printf 'code=%s' "$http_code")"
    return 0
  fi
  case "$http_code" in
    101)
      pass "${EDGE_AUTH_MUX_WS_PROBE} via PAT → 101 (WS reaches coopmux through shim)"
      ;;
    401|403)
      fail "${EDGE_AUTH_MUX_WS_PROBE} via PAT → 101" \
        "got $http_code — coopmux WS handler rejected the shim's HMAC envelope (ka-0000003786 regression: /ws/* must honour TrustedOperator like the HTTP path)"
      ;;
    404|503)
      skip "${EDGE_AUTH_MUX_WS_PROBE} WS upgrade" \
        "HTTP ${http_code} — coopmux upstream not configured in this env (gated like Gate E)"
      ;;
    *)
      fail "${EDGE_AUTH_MUX_WS_PROBE} via PAT → 101" "got $http_code"
      ;;
  esac
}

# Gate T: the /mux front door is served as a SINGLE text/html content-type
# (ka-0000007632). A PAT request to the coopmux SPA root must return 200 with
# EXACTLY ONE `Content-Type` header whose value is text/html — never
# application/json, never a doubled `application/json, text/html`. This locks in
# the front-door regression fix: handleProxy sets application/json for its
# deny/error paths and httputil.ReverseProxy copies upstream headers with Add
# (not Set), so absent the explicit Header.Del("Content-Type") the dashboard SPA
# came back two-valued and browsers rendered the markup as plain text (the
# "coopmux front door returns a bunch of JavaScript" report). The unit-level
# guard is TestProxy_ForwardsUpstreamContentType in
# gardenio/controller/cmd/edge-auth/proxy_test.go; this is the live-edge assertion
# that the fix survives the real ReverseProxy header merge. SKIPs when the shim
# has no PAT or the coopmux upstream isn't wired (mirrors Gate E/S gating).
gate_mux_html_content_type() {
  blue "Gate T: /mux front door served as single text/html (no doubled Content-Type)"
  if [ -z "$EDGE_AUTH_URL" ]; then
    skip "${EDGE_AUTH_MUX_HTML_PROBE} content-type" "no endpoint resolved"
    return 0
  fi
  if [ -z "${GARDENIO_TOKEN:-}" ]; then
    skip "${EDGE_AUTH_MUX_HTML_PROBE} content-type" "GARDENIO_TOKEN not set"
    return 0
  fi
  local target="${EDGE_AUTH_URL%/}${EDGE_AUTH_MUX_HTML_PROBE}"
  # -D - dumps response headers so we can count Content-Type occurrences and read
  # the value; -o /dev/null drops the (HTML) body. No -L: a redirect is not a
  # front-door render and must not be silently followed into a 200.
  local hdrs http_code rc=0
  hdrs="$(curl -sS --max-time "$CURL_TIMEOUT" -o /dev/null -D - -w 'HTTP_CODE:%{http_code}' \
    -H "Authorization: Bearer ${GARDENIO_TOKEN}" \
    "$target" 2>/dev/null)" || rc=$?
  if [ "$rc" -ne 0 ]; then
    fail "${EDGE_AUTH_MUX_HTML_PROBE} content-type" "curl exit $rc"
    return 0
  fi
  archive "gate_mux_html_content_type" "$hdrs"
  http_code="$(printf '%s' "$hdrs" | sed -n 's/.*HTTP_CODE:\([0-9]*\).*/\1/p' | tail -1)"
  # Count and capture the Content-Type header(s). Header names are
  # case-insensitive; grep -i. Trim CRs so the value compare is clean.
  local ct_lines ct_count ct_value
  ct_lines="$(printf '%s' "$hdrs" | grep -i '^content-type:' | tr -d '\r' || true)"
  ct_count="$(printf '%s' "$ct_lines" | grep -c . || true)"
  ct_value="$(printf '%s' "$ct_lines" | sed -n 's/^[Cc]ontent-[Tt]ype:[[:space:]]*//p' | tr '[:upper:]' '[:lower:]' | tr '\n' ',' )"
  case "$http_code" in
    404|503)
      skip "${EDGE_AUTH_MUX_HTML_PROBE} content-type" \
        "HTTP ${http_code} — coopmux upstream not configured in this env (gated like Gate E)"
      return 0
      ;;
    200) ;;
    *)
      fail "${EDGE_AUTH_MUX_HTML_PROBE} → 200 text/html" "got $http_code"
      return 0
      ;;
  esac
  # A doubled Content-Type (Add-not-Set regression) shows as two header lines OR
  # a single comma-joined `application/json, text/html`. Reject both.
  if [ "$ct_count" -gt 1 ]; then
    fail "${EDGE_AUTH_MUX_HTML_PROBE} single Content-Type" \
      "got ${ct_count} Content-Type headers ($ct_value) — doubled header regression (ka-0000007632): handleProxy must Header.Del before the ReverseProxy handoff"
    return 0
  fi
  case "$ct_value" in
    *application/json*)
      fail "${EDGE_AUTH_MUX_HTML_PROBE} Content-Type text/html" \
        "got '${ct_value%,}' — the shim's deny/error application/json leaked onto a proxied 200 (ka-0000007632)"
      ;;
    *text/html*)
      pass "${EDGE_AUTH_MUX_HTML_PROBE} via PAT → 200, single Content-Type text/html"
      ;;
    "")
      fail "${EDGE_AUTH_MUX_HTML_PROBE} Content-Type text/html" "no Content-Type header on the 200 response"
      ;;
    *)
      fail "${EDGE_AUTH_MUX_HTML_PROBE} Content-Type text/html" "got '${ct_value%,}', want text/html"
      ;;
  esac
}

# Gate U: an authenticated WebSocket upgrade to /kd/nats/ reaches the NATS
# WebSocket service THROUGH the shim (ka-0000006000), proving the /kd/nats/
# route is registered BEFORE /kd/ so it is not shadowed by the kbeads daemon.
# matchUpstream is first-match-by-order; the shim registers /kd/nats/ ahead of
# /kd/ (buildUpstreams spec order), so the realtime channel routes to
# gardenio-nats:8443 instead of falling through to the kbeads daemon — which has
# no WS handler and 502s `gb agent start --local`'s local nats-bridge. A PAT
# crossing the shim on /kd/nats/ must yield 101 Switching Protocols, NOT a 502
# (fell through to kbeads) and NOT a 401/403 (auth). The unit guard on the
# ordering is TestBuildUpstreams_NatsRoutePrecedesKbeads; this is the live-edge
# assertion that the route actually reaches NATS. SKIPs when the shim has no PAT
# or the NATS upstream isn't wired (nats.websocket.enabled=false), mirroring
# Gate E/S gating.
gate_kd_nats_ws_route() {
  blue "Gate U: /kd/nats WS upgrade reaches NATS through the shim (101, ordered before /kd/)"
  if [ -z "$EDGE_AUTH_URL" ]; then
    skip "${EDGE_AUTH_KD_NATS_WS_PROBE} WS upgrade" "no endpoint resolved"
    return 0
  fi
  if [ -z "${GARDENIO_TOKEN:-}" ]; then
    skip "${EDGE_AUTH_KD_NATS_WS_PROBE} WS upgrade" "GARDENIO_TOKEN not set"
    return 0
  fi
  local target="${EDGE_AUTH_URL%/}${EDGE_AUTH_KD_NATS_WS_PROBE}"
  # Real WS handshake; a successful upgrade returns 101. --max-time bounds the
  # socket so a held-open 101 returns promptly (curl -w prints the code before
  # the timeout fires). No -L: a 3xx is itself a failure.
  local http_code rc=0
  http_code="$(curl -sS --max-time "$CURL_TIMEOUT" -o /dev/null -w '%{http_code}' \
    -H "Authorization: Bearer ${GARDENIO_TOKEN}" \
    -H "Connection: Upgrade" \
    -H "Upgrade: websocket" \
    -H "Sec-WebSocket-Version: 13" \
    -H "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==" \
    "$target" 2>/dev/null)" || rc=$?
  if [ "$rc" -ne 0 ] && [ "$http_code" != "101" ]; then
    fail "${EDGE_AUTH_KD_NATS_WS_PROBE} WS upgrade" "curl exit $rc — $(printf 'code=%s' "$http_code")"
    return 0
  fi
  case "$http_code" in
    101)
      pass "${EDGE_AUTH_KD_NATS_WS_PROBE} via PAT → 101 (WS reaches NATS through shim, /kd/nats/ ordered before /kd/)"
      ;;
    502)
      fail "${EDGE_AUTH_KD_NATS_WS_PROBE} via PAT → 101" \
        "got 502 — /kd/nats fell through to the kbeads daemon (no WS handler); the /kd/nats/ route must be registered BEFORE /kd/ (ka-0000006000)"
      ;;
    401|403)
      fail "${EDGE_AUTH_KD_NATS_WS_PROBE} via PAT → 101" \
        "got $http_code — shim rejected the authenticated upgrade before it could reach NATS"
      ;;
    404|503)
      skip "${EDGE_AUTH_KD_NATS_WS_PROBE} WS upgrade" \
        "HTTP ${http_code} — NATS WS upstream not configured in this env (nats.websocket.enabled=false; gated like Gate E)"
      ;;
    *)
      fail "${EDGE_AUTH_KD_NATS_WS_PROBE} via PAT → 101" "got $http_code"
      ;;
  esac
}

# Gate O: a forged/bogus browser-session cookie is rejected at the proxy.
# Locks in the cookie-auth deny path (ka-0000000080): a request carrying a
# junk session cookie and no Bearer PAT must 401 — invalid_session when cookie
# mode is ON, missing_token when it is OFF. Either way a 401 proves the shim
# never trusts an unverified cookie. A 2xx here would mean the shim forwarded
# an unauthenticated request — the exact failure cookie validation prevents.
# Portable: needs no real session, always runs when an endpoint resolves.
gate_cookie_rejected() {
  blue "Gate O: forged session cookie rejected at proxy"
  if [ -z "$EDGE_AUTH_URL" ]; then
    skip "${EDGE_AUTH_PROXY_PREFIX}${EDGE_AUTH_PROXY_PROBE} forged-cookie" "no endpoint resolved"
    return 0
  fi
  local target="${EDGE_AUTH_URL%/}${EDGE_AUTH_PROXY_PREFIX}${EDGE_AUTH_PROXY_PROBE}"
  local http_code rc=0
  http_code="$(curl -sS --max-time "$CURL_TIMEOUT" -o /dev/null -w '%{http_code}' \
    -H "Cookie: ${EDGE_AUTH_COOKIE_NAME}=forged-${TEST_TAG}" \
    "$target" 2>/dev/null)" || rc=$?
  if [ "$rc" -ne 0 ]; then
    fail "forged-cookie proxy" "curl exit $rc"
    return 0
  fi
  case "$http_code" in
    401|403)
      pass "forged session cookie → ${http_code} (shim rejects unverified cookies)"
      ;;
    *)
      fail "forged session cookie → 401" \
        "got $http_code — the shim must never forward a request bearing only an unverified cookie"
      ;;
  esac
}

# Gate P: a valid browser-session cookie authenticates through the shim.
# Companion success path to Gate O. With EDGE_AUTH_TEST_COOKIE set to a real
# oauth2-proxy session value, a no-PAT request must reach the daemon (200) or
# the dry-run short-circuit (200 + X-Edge-Auth-Dry-Run) — proving gb-login's
# cookie drives gardenio.app/kd with no PAT. Skips without a real session
# (minting one is the consolidated suite's job, ka-0000007461).
gate_cookie_auth() {
  blue "Gate P: valid session cookie authenticates via shim"
  if [ -z "$EDGE_AUTH_URL" ]; then
    skip "${EDGE_AUTH_PROXY_PREFIX}${EDGE_AUTH_PROXY_PROBE} cookie-auth" "no endpoint resolved"
    return 0
  fi
  if [ -z "$EDGE_AUTH_TEST_COOKIE" ]; then
    skip "${EDGE_AUTH_PROXY_PREFIX}${EDGE_AUTH_PROXY_PROBE} cookie-auth" \
      "EDGE_AUTH_TEST_COOKIE not set — supply a real oauth2-proxy session to exercise the cookie success path"
    return 0
  fi
  local target="${EDGE_AUTH_URL%/}${EDGE_AUTH_PROXY_PREFIX}${EDGE_AUTH_PROXY_PROBE}"
  local http_code rc=0
  http_code="$(curl -sS --max-time "$CURL_TIMEOUT" -o /dev/null -w '%{http_code}' \
    -H "Cookie: ${EDGE_AUTH_COOKIE_NAME}=${EDGE_AUTH_TEST_COOKIE}" \
    "$target" 2>/dev/null)" || rc=$?
  if [ "$rc" -ne 0 ]; then
    fail "cookie-auth proxy" "curl exit $rc"
    return 0
  fi
  case "$http_code" in
    200)
      pass "valid session cookie → 200 (cookie path reaches daemon/dry-run, no PAT)"
      ;;
    401|404|503)
      skip "cookie-auth proxy" \
        "HTTP ${http_code} — cookie mode off / upstream not wired in this env (gated like Gate E)"
      ;;
    *)
      fail "valid session cookie → 200" "got $http_code"
      ;;
  esac
}

# Gate Q: forwardauth-identity WRITE attribution (GATING, ka-0000007461).
# THE green-able gating assertion of the consolidated auth matrix. Runs
# UNCONDITIONALLY against BEADS_HTTP_URL (the in-cluster kbeads daemon the
# shard already talks to) — OUTSIDE the EDGE_AUTH_URL endpoint guard — so it is
# GREEN in the ephemeral e2e ns where edge-auth/unified-host are NOT deployed.
#
# This is the SERVER-SIDE EQUIVALENT of the gb-login cookie WRITE path. In
# prod the AVA forwardauth verifies the browser session and the unified-host
# → kbeads hop carries the resolved human identity, which kbeads maps to
# operated_by=oidc:<email>. Here we exercise the SAME kbeads resolution
# contract directly: a write that carries ONLY an injected human identity
# header (X-Operated-By, the static-token attribution channel —
# resolveOperatedByPure case "static"/"none") must record
# operated_by=oidc:<email> on the bead's event log, NOT the ambient shard
# operator and NOT a refusal.
#
# Steps: create a bead + add a comment carrying X-Operated-By=oidc:<email>,
# then GET /v1/beads/{id}/events and assert every write event resolved to that
# identity. A wrong/empty operator, or the shard-default operator, is a hard
# fail — that is the exact attribution bug the gb-login work exists to prevent.
gate_forwardauth_identity_write() {
  blue "Gate Q: forwardauth-identity WRITE → operated_by=oidc:<email> (GATING)"
  if ! have curl || ! have jq; then
    skip "forwardauth-identity write" "curl/jq not in PATH"
    return 0
  fi
  if [ -z "$BEADS_HTTP_URL" ]; then
    skip "forwardauth-identity write" \
      "BEADS_HTTP_URL not set — run-shard.sh exports it; supply it to exercise the gating write path"
    return 0
  fi
  local op="oidc:${EDGE_AUTH_FWDIDENTITY_EMAIL#oidc:}"
  # Header set forged at the in-cluster forwardauth boundary. X-Operated-By is
  # the human-identity attribution channel kbeads' static-token path honors;
  # this is exactly what the AVA forwardauth → kbeads hop stamps for a
  # browser-session (cookie) write. The Bearer token is the shard's daemon
  # token (static primary) — present so the request authenticates; attribution
  # comes from X-Operated-By, mirroring the prod forwardauth envelope.
  local -a wh=(-H "Content-Type: application/json" -H "X-Operated-By: ${op}")
  if [ -n "$BEADS_TOKEN" ]; then
    wh+=(-H "Authorization: Bearer ${BEADS_TOKEN}")
  fi

  # ── Create a bead carrying the injected identity ──────────────────
  local create_body create_http create_json bead_id rc=0
  create_body="$(curl -sS --max-time "$CURL_TIMEOUT" -w '\n__HTTP__:%{http_code}' \
    -X POST "${wh[@]}" \
    -d "{\"title\":\"edge-auth Gate Q fwdauth-identity ${TEST_TAG}\",\"type\":\"task\",\"labels\":[\"e2e:edge-auth-gateq\",\"e2e-tag:${TEST_TAG}\"],\"description\":\"Gate Q forwardauth-identity WRITE attribution probe — safe to delete\"}" \
    "${BEADS_HTTP_URL%/}/v1/beads" 2>&1)" || rc=$?
  create_http="$(printf '%s' "$create_body" | awk -F: '/__HTTP__:/{print $2}' | tr -d '\r ')"
  create_json="$(printf '%s' "$create_body" | sed '/__HTTP__:/d')"
  archive "gateq-create" "$create_body"
  if [ "$rc" -ne 0 ]; then
    fail "Gate Q create bead" "curl exit $rc"
    return 0
  fi
  if [ "$create_http" != "200" ] && [ "$create_http" != "201" ]; then
    fail "Gate Q create bead → 2xx" \
      "got $create_http — a forwardauth-identity write must succeed, not refuse: $(printf '%s' "$create_json" | head -c 200)"
    return 0
  fi
  bead_id="$(printf '%s' "$create_json" | jq -r '.id // empty' 2>/dev/null)"
  if [ -z "$bead_id" ]; then
    fail "Gate Q create returns bead id" "no .id in response: $(printf '%s' "$create_json" | head -c 200)"
    return 0
  fi
  pass "Gate Q create bead via forwardauth identity → ${bead_id}"

  # ── Add a comment carrying the same injected identity ─────────────
  local comment_http comment_body rc2=0
  comment_body="$(curl -sS --max-time "$CURL_TIMEOUT" -w '\n__HTTP__:%{http_code}' \
    -X POST "${wh[@]}" \
    -d '{"text":"Gate Q comment via forwardauth identity"}' \
    "${BEADS_HTTP_URL%/}/v1/beads/${bead_id}/comments" 2>&1)" || rc2=$?
  comment_http="$(printf '%s' "$comment_body" | awk -F: '/__HTTP__:/{print $2}' | tr -d '\r ')"
  archive "gateq-comment" "$comment_body"
  if [ "$rc2" -eq 0 ] && { [ "$comment_http" = "200" ] || [ "$comment_http" = "201" ]; }; then
    pass "Gate Q add comment via forwardauth identity → ${comment_http}"
  else
    fail "Gate Q add comment → 2xx" \
      "got ${comment_http:-curl-exit-$rc2} — write must succeed: $(printf '%s' "$comment_body" | sed '/__HTTP__:/d' | head -c 200)"
  fi

  # ── Assert operated_by attribution on the event log ───────────────
  local events_json events_http rc3=0
  events_json="$(curl -sS --max-time "$CURL_TIMEOUT" -w '\n__HTTP__:%{http_code}' \
    "${wh[@]}" \
    "${BEADS_HTTP_URL%/}/v1/beads/${bead_id}/events" 2>&1)" || rc3=$?
  events_http="$(printf '%s' "$events_json" | awk -F: '/__HTTP__:/{print $2}' | tr -d '\r ')"
  events_json="$(printf '%s' "$events_json" | sed '/__HTTP__:/d')"
  archive "gateq-events" "$events_json"
  if [ "$rc3" -ne 0 ] || [ "$events_http" != "200" ]; then
    fail "Gate Q GET events → 200" "got ${events_http:-curl-exit-$rc3}"
    return 0
  fi
  # Every write event recorded on this bead must resolve to the injected
  # identity. Empty / wrong / shard-default operator is the attribution bug.
  local distinct_ops mismatched
  distinct_ops="$(printf '%s' "$events_json" | jq -r \
    '[.events[]? | select((.topic // "") | test("created|updated|comment")) | .operated_by // ""] | unique | .[]' 2>/dev/null)"
  if [ -z "$distinct_ops" ]; then
    fail "Gate Q events carry operated_by" \
      "no write events with operated_by found: $(printf '%s' "$events_json" | head -c 300)"
    return 0
  fi
  mismatched="$(printf '%s\n' "$distinct_ops" | grep -v -x "$op" || true)"
  if [ -z "$mismatched" ]; then
    pass "Gate Q events attribute to forwardauth identity (operated_by=${op})"
  else
    fail "Gate Q events attribute to ${op}" \
      "found other operator(s): $(printf '%s' "$mismatched" | tr '\n' ' ') — forwardauth identity not honored (the gb-login attribution bug)"
  fi
}

# Gate R: unified-host PAT WRITE + attribution + non-member → 403 (ka-0000007461).
# The PAT companion to Gate Q's forwardauth-identity write, exercised against
# the AVA unified host (gardenio.app/kd) — the front door the epic is making
# work. Three assertions:
#   R1. A member Platform PAT WRITE (create bead) through the unified host → 2xx.
#   R2. The write attributes operated_by=oidc:<email> for the PAT's GitLab
#       identity (the edge-auth PAT branch resolves the GitLab user and stamps
#       the HMAC envelope kbeads trusts).
#   R3. A valid-but-non-member PAT → 403 (authenticated, not authorized).
# SKIPs cleanly in the ephemeral ns exactly like shim gates M–P: the unified
# host is not deployed there and the CI job carries no GitLab PAT secret.
# Only the long-lived staging singleton supplies UNIFIED_HOST_URL +
# GARDENIO_TOKEN + EDGE_AUTH_NONMEMBER_PAT.
gate_unified_pat_write_attribution() {
  blue "Gate R: unified-host PAT WRITE + attribution + non-member 403"
  if ! have curl || ! have jq; then
    skip "unified-host PAT write" "curl/jq not in PATH"
    return 0
  fi
  if [ -z "$UNIFIED_HOST_URL" ]; then
    skip "unified-host PAT write" \
      "UNIFIED_HOST_URL not set — supply the AVA unified host (gardenio.app/kd); ephemeral ns has no unified host (like Gates M–P)"
    return 0
  fi
  if [ -z "${GARDENIO_TOKEN:-}" ]; then
    skip "unified-host PAT write" "GARDENIO_TOKEN not set — supply a member Platform PAT"
    return 0
  fi
  local kd_base="${UNIFIED_HOST_URL%/}/kd"

  # ── R1/R2: member PAT WRITE + attribution ─────────────────────────
  local create_body create_http create_json bead_id rc=0
  create_body="$(curl -sS --max-time "$CURL_TIMEOUT" -w '\n__HTTP__:%{http_code}' \
    -X POST -H "Content-Type: application/json" \
    -H "Authorization: Bearer ${GARDENIO_TOKEN}" \
    -d "{\"title\":\"edge-auth Gate R unified-PAT ${TEST_TAG}\",\"type\":\"task\",\"labels\":[\"e2e:edge-auth-gater\",\"e2e-tag:${TEST_TAG}\"],\"description\":\"Gate R unified-host PAT WRITE attribution probe — safe to delete\"}" \
    "${kd_base}/v1/beads" 2>&1)" || rc=$?
  create_http="$(printf '%s' "$create_body" | awk -F: '/__HTTP__:/{print $2}' | tr -d '\r ')"
  create_json="$(printf '%s' "$create_body" | sed '/__HTTP__:/d')"
  archive "gater-create" "$create_body"
  if [ "$rc" -ne 0 ]; then
    fail "Gate R unified-host create" "curl exit $rc"
    return 0
  fi
  case "$create_http" in
    303|302|307)
      fail "Gate R unified-host PAT write → 2xx" \
        "got $create_http — PAT write shadow-redirected to OIDC (BUG#1, ka-0000004502)"
      return 0
      ;;
    401|404|503)
      skip "unified-host PAT write" \
        "HTTP $create_http — unified host / edge-auth not active in this env (gated like Gates M–P; only the staging singleton deploys it)"
      return 0
      ;;
    200|201) : ;;
    *)
      fail "Gate R unified-host PAT write → 2xx" "got $create_http: $(printf '%s' "$create_json" | head -c 200)"
      return 0
      ;;
  esac
  bead_id="$(printf '%s' "$create_json" | jq -r '.id // empty' 2>/dev/null)"
  if [ -z "$bead_id" ]; then
    fail "Gate R create returns bead id" "no .id: $(printf '%s' "$create_json" | head -c 200)"
    return 0
  fi
  pass "Gate R unified-host member PAT WRITE → ${bead_id}"

  # R2: attribution on the event log resolves to a human oidc:<email>.
  local events_json events_http op rc2=0
  events_json="$(curl -sS --max-time "$CURL_TIMEOUT" -w '\n__HTTP__:%{http_code}' \
    -H "Authorization: Bearer ${GARDENIO_TOKEN}" \
    "${kd_base}/v1/beads/${bead_id}/events" 2>&1)" || rc2=$?
  events_http="$(printf '%s' "$events_json" | awk -F: '/__HTTP__:/{print $2}' | tr -d '\r ')"
  events_json="$(printf '%s' "$events_json" | sed '/__HTTP__:/d')"
  archive "gater-events" "$events_json"
  if [ "$rc2" -eq 0 ] && [ "$events_http" = "200" ]; then
    op="$(printf '%s' "$events_json" | jq -r \
      '[.events[]? | select((.topic // "") | test("created")) | .operated_by // ""] | map(select(. != "")) | first // ""' 2>/dev/null)"
    if grep -qE '^oidc:[^@]+@[^@]+\.[^@]+$' <<<"$op"; then
      pass "Gate R unified-host PAT write attributes operated_by=${op}"
    else
      fail "Gate R unified-host PAT write attribution" \
        "operated_by='${op}' is not an oidc:<email> human identity — PAT GitLab identity not resolved"
    fi
  else
    fail "Gate R GET events → 200" "got ${events_http:-curl-exit-$rc2}"
  fi

  # ── R3: non-member PAT → 403 ──────────────────────────────────────
  if [ -z "$EDGE_AUTH_NONMEMBER_PAT" ]; then
    skip "unified-host non-member PAT → 403" \
      "EDGE_AUTH_NONMEMBER_PAT not set — supply a valid GitLab PAT for a user outside edgeAuth.gitlabGroup"
    return 0
  fi
  local nm_http rc3=0
  nm_http="$(curl -sS --max-time "$CURL_TIMEOUT" -o /dev/null -w '%{http_code}' \
    -X POST -H "Content-Type: application/json" \
    -H "Authorization: Bearer ${EDGE_AUTH_NONMEMBER_PAT}" \
    -d "{\"title\":\"edge-auth Gate R nonmember ${TEST_TAG}\",\"type\":\"task\"}" \
    "${kd_base}/v1/beads" 2>/dev/null)" || rc3=$?
  if [ "$rc3" -ne 0 ]; then
    fail "Gate R non-member PAT" "curl exit $rc3"
    return 0
  fi
  case "$nm_http" in
    403)
      pass "Gate R non-member PAT → 403 (authenticated, not authorized)"
      ;;
    401)
      # Acceptable only if the env rejects the non-member at the auth layer;
      # still proves the non-member cannot write. Note the distinction.
      pass "Gate R non-member PAT → 401 (rejected; not a member-grade write)"
      ;;
    200|201)
      fail "Gate R non-member PAT → 403" \
        "got $nm_http — a non-member PAT must NOT be able to write through the unified host"
      ;;
    *)
      fail "Gate R non-member PAT → 403" "got $nm_http"
      ;;
  esac
}

# Gate S: gb-login cookie WRITE + attribution through the shim (ka-0000001722).
# The COOKIE companion to Gate R's PAT-write — and the end-to-end browser-half
# of Gate Q's server-side write-attribution. Gate P proves a cookie request
# READS (200); this proves the gb-login cookie WRITE path: a no-PAT request
# carrying a real oauth2-proxy session must (W1) create a bead → 2xx through the
# shim's cookie branch, and (W2) attribute operated_by=oidc:<email> for the
# email oauth2-proxy attests — NOT the ambient shard operator, NOT empty, NOT a
# refusal. The shim validates the cookie via a /oauth2/auth subrequest, lifts
# the attested X-Auth-Request-Email, and stamps it into the SAME HMAC envelope
# kbeads already trusts (commit d52f2a60f); so a green Gate S proves the cookie
# WRITE resolves identically to a PAT write.
#
# SKIPs cleanly in the ephemeral e2e ns exactly like Gates M–P and Gate R: the
# real session cookie (_gardenio_oauth2, gardenio.app-scoped) is minted only by a
# browser + Cloudflare OAuth dance, which cannot run headlessly in a CI
# runner, and the unified host / edge-auth cookie mode is not deployed there.
# Only a run supplied with EDGE_AUTH_TEST_COOKIE against an endpoint with
# cookieAuth.enabled exercises it; the gating green-able half is Gate Q (server
# side) added in MR !3419. The real-browser cookie WRITE + >3m refresh against
# live gardenio.app was the operator-only validation step in
# gardenio/docs/gb-login-runbook.md (escalation under epic ka-0000006278); that
# runbook dropped it when gb login moved to the Keycloak device grant
# (gd-5FThTGbOOP), see its git history.
gate_cookie_write_attribution() {
  blue "Gate S: gb-login cookie WRITE → operated_by=oidc:<email> via shim"
  if ! have curl || ! have jq; then
    skip "cookie write attribution" "curl/jq not in PATH"
    return 0
  fi
  if [ -z "$EDGE_AUTH_URL" ]; then
    skip "cookie write attribution" "no endpoint resolved"
    return 0
  fi
  if [ -z "$EDGE_AUTH_TEST_COOKIE" ]; then
    skip "cookie write attribution" \
      "EDGE_AUTH_TEST_COOKIE not set — a real oauth2-proxy session is minted only by a browser+Cloudflare OAuth dance, not reproducible headlessly (see Gate P / the cookie gap in README); the server-side equivalent is Gate Q"
    return 0
  fi
  local kd_base="${EDGE_AUTH_URL%/}${EDGE_AUTH_PROXY_PREFIX%/}"
  local -a ch=(-H "Content-Type: application/json" \
    -H "Cookie: ${EDGE_AUTH_COOKIE_NAME}=${EDGE_AUTH_TEST_COOKIE}")

  # ── W1: create a bead via the cookie path (no PAT) ────────────────
  local create_body create_http create_json bead_id rc=0
  create_body="$(curl -sS --max-time "$CURL_TIMEOUT" -w '\n__HTTP__:%{http_code}' \
    -X POST "${ch[@]}" \
    -d "{\"title\":\"edge-auth Gate S cookie-write ${TEST_TAG}\",\"type\":\"task\",\"labels\":[\"e2e:edge-auth-gates\",\"e2e-tag:${TEST_TAG}\"],\"description\":\"Gate S gb-login cookie WRITE attribution probe — safe to delete\"}" \
    "${kd_base}/v1/beads" 2>&1)" || rc=$?
  create_http="$(printf '%s' "$create_body" | awk -F: '/__HTTP__:/{print $2}' | tr -d '\r ')"
  create_json="$(printf '%s' "$create_body" | sed '/__HTTP__:/d')"
  archive "gates-create" "$create_body"
  if [ "$rc" -ne 0 ]; then
    fail "Gate S create bead" "curl exit $rc"
    return 0
  fi
  case "$create_http" in
    303|302|307)
      fail "Gate S cookie write → 2xx" \
        "got $create_http — cookie write shadow-redirected to OIDC (BUG#1, ka-0000004502)"
      return 0
      ;;
    401|404|503)
      skip "cookie write attribution" \
        "HTTP $create_http — cookie mode off / upstream not wired in this env (gated like Gates M–P/R; only the staging singleton deploys cookieAuth.enabled)"
      return 0
      ;;
    200|201) : ;;
    *)
      fail "Gate S cookie write → 2xx" "got $create_http: $(printf '%s' "$create_json" | head -c 200)"
      return 0
      ;;
  esac
  bead_id="$(printf '%s' "$create_json" | jq -r '.id // empty' 2>/dev/null)"
  if [ -z "$bead_id" ]; then
    fail "Gate S create returns bead id" "no .id: $(printf '%s' "$create_json" | head -c 200)"
    return 0
  fi
  pass "Gate S gb-login cookie WRITE (no PAT) → ${bead_id}"

  # ── W2: attribution on the event log resolves to a human oidc:<email> ──
  local events_json events_http op rc2=0
  events_json="$(curl -sS --max-time "$CURL_TIMEOUT" -w '\n__HTTP__:%{http_code}' \
    "${ch[@]}" \
    "${kd_base}/v1/beads/${bead_id}/events" 2>&1)" || rc2=$?
  events_http="$(printf '%s' "$events_json" | awk -F: '/__HTTP__:/{print $2}' | tr -d '\r ')"
  events_json="$(printf '%s' "$events_json" | sed '/__HTTP__:/d')"
  archive "gates-events" "$events_json"
  if [ "$rc2" -ne 0 ] || [ "$events_http" != "200" ]; then
    fail "Gate S GET events → 200" "got ${events_http:-curl-exit-$rc2}"
    return 0
  fi
  # The cookie write must attribute to the oauth2-proxy-attested human identity
  # (oidc:<email>), proving the shim lifted X-Auth-Request-Email and stamped it
  # — not the ambient shard operator, not empty. That is the exact gb-login
  # attribution invariant this epic exists to prove.
  op="$(printf '%s' "$events_json" | jq -r \
    '[.events[]? | select((.topic // "") | test("created")) | .operated_by // ""] | map(select(. != "")) | first // ""' 2>/dev/null)"
  if grep -qE '^oidc:[^@]+@[^@]+\.[^@]+$' <<<"$op"; then
    pass "Gate S cookie write attributes operated_by=${op} (oauth2-proxy-attested identity)"
  else
    fail "Gate S cookie write attribution" \
      "operated_by='${op}' is not an oidc:<email> human identity — cookie-attested email not resolved at the shim"
  fi
}

# ── Main ──────────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue "  gardenio-edge-auth E2E (scaffold — ka-0000008114)"
  blue "═══════════════════════════════════════════════════"
  echo

  local missing=()
  for bin in curl python3; do
    have "$bin" || missing+=("$bin")
  done
  if [ "${#missing[@]}" -gt 0 ]; then
    dim "SKIP: missing required binaries: ${missing[*]}"
    exit 0
  fi

  SKIP_REMAINING=0
  if [ -n "$EDGE_AUTH_URL" ]; then
    EDGE_AUTH_URL_PRESET=1
  fi
  resolve_endpoint
  echo

  gate_pod_deployed
  echo

  if [ "$SKIP_REMAINING" -eq 1 ]; then
    blue "Subsequent gates skipped — environment not ready."
  else
    gate_healthz
    echo
    gate_authcheck_unauth
    echo
    gate_authcheck_pat
    echo
    gate_proxy_invalid_pat
    echo
    gate_proxy_pat
    echo
    gate_unified_kd_not_shadowed
    echo
    gate_unified_kd_pat
    echo
    gate_mux_ws_through_shim
    echo
    gate_mux_html_content_type
    echo
    gate_kd_nats_ws_route
    echo
    gate_cookie_rejected
    echo
    gate_cookie_auth
    echo
    gate_cookie_write_attribution
    echo
    gate_forwardauth_no_token
    echo
    gate_forwardauth_bad_token
    echo
    gate_forwardauth_success
    echo
    gate_proxy_ava_hmac_parity
    echo
    gate_kd_token_e2e
    echo
    gate_gb_token_e2e
    echo
  fi

  # ── Consolidated auth-matrix gates (ka-0000007461) ────────────────
  # Gate Q runs UNCONDITIONALLY via BEADS_HTTP_URL — OUTSIDE the edge-auth
  # endpoint guard above — because it is the green-able GATING assertion in
  # the ephemeral e2e ns, where edge-auth/unified-host are not deployed and
  # the shim gates (E–R) all SKIP. Gate R is keyed on UNIFIED_HOST_URL and
  # SKIPs cleanly in that same ephemeral ns; only the long-lived staging
  # singleton supplies the unified host + PATs that make it run.
  gate_forwardauth_identity_write
  echo
  gate_unified_pat_write_attribution
  echo

  # ── Summary ─────────────────────────────────────────────────────
  blue "═══════════════════════════════════════════════════"
  local total=$((PASS + FAIL))
  if [ "$FAIL" -eq 0 ]; then
    green "  ✓ All $total assertions passed ($SKIP skipped)"
  else
    red "  ✗ $FAIL/$total assertions failed ($SKIP skipped)"
    echo
    for err in "${ERRORS[@]}"; do
      red "   - $err"
    done
  fi
  blue "═══════════════════════════════════════════════════"

  [ "$FAIL" -eq 0 ]
}

main "$@"
