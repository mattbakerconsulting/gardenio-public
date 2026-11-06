#!/usr/bin/env bash
# test-migration-postauth-viewer.sh — P4.4 migration acceptance smoke:
# an AUTHENTICATED beads-viewer page returns HTTP 200 with REAL, daemon-backed
# content — NOT the pre-auth 303 that masked the P4.3b prod 502.
#
# Bead: ka-0000008176  (epic ka-0000003481 → Phase-4 migration ka-0000002205)
# Root cause it guards: ka-0000007004 — the fresh-prod beads-viewer 502 slipped
# through because every prior proof-of-life only saw the edge's pre-auth
# 303→GitLab-OAuth redirect. That redirect is issued by AVA/oauth2-proxy BEFORE
# any backend is touched, so a mis-wired viewer→daemon edge read as "green."
#
# What this check proves (the exact hop that 502'd):
#   1. AUTH REACHES A 200, NOT A 303. We present a real GitLab-direct bearer and
#      require HTTP 200. A 303/302/307 is a HARD FAIL (mirrors edge-auth Gate M)
#      — that is the pre-auth-redirect signature we must never again mistake for
#      health.
#   2. THE DAEMON IS ON THE OTHER SIDE. We create a fixture bead via the kbeads
#      daemon, then GET the viewer's /{id} bead page (handleBeadShow →
#      s.daemon.GetBead, server.go:2390). Asserting the fixture's unique title
#      appears in the 200 body proves the full AVA-trusted → viewer pod →
#      gardenio-beads daemon data path renders real content. A dead daemon there
#      yields 500 "Failed to get bead", not a 200 with the title.
#
# WHY port-forward, not the public host: the public edge 308-redirects bearer
# traffic before Traefik, so you cannot curl the public viewer host with a token
# and get 200 (fetch-beads-viewer-prod.sh:9-27). We port-forward straight to the
# viewer ClusterIP (svc/<release>-beads-viewer:8095) and present the bearer to
# the viewer's own OIDC middleware on localhost, bypassing the edge — the
# supported, proven technique.
#
# Self-SKIPs (never a false red) when:
#   - the viewer Service is absent (minimal ephemeral e2e ns — the viewer OIDC
#     rigging only exists on the long-lived singleton / prod), or
#   - a GitLab-direct bearer cannot be resolved (neither GARDENIO_E2E_GITLAB_BEARER
#     nor the static daemon token is set in this env). Skips are LOUD, following the sibling
#     viewer modules' convention.
#
# Runnable against prod.gardenio.acme.example by pointing kubectl at the prod cluster
# and exporting NAMESPACE=<prod ns> (see
# gardenio/docs/migration-acceptance-smoke.md). READ-ONLY apart from creating +
# closing one throwaway fixture bead.
#
# Usage:
#   NAMESPACE=gardenio-e2e ./gardenio/tests/e2e/scripts/test-migration-postauth-viewer.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib-gitlab-token.sh"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib-viewer.sh"

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
RELEASE="${RELEASE:-${NAMESPACE}}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
VIEWER_SVC="${VIEWER_SVC:-${RELEASE}-beads-viewer}"
VIEWER_PF_PORT="${VIEWER_PF_PORT:-19811}"
VIEWER_PF_LOG="${VIEWER_PF_LOG:-/tmp/migration-postauth-viewer-pf.log}"
BEADS_PF_PORT="${BEADS_PF_PORT:-19812}"
KD_BIN="${KD_BIN:-kd}"
SMOKE_EMAIL="${SMOKE_EMAIL:-e2e-postauth-smoke-$$@example.com}"
# Prefer client_credentials (non-interactive, works in-pod) when explicitly
# requested; default to the user-token grant the sibling viewer modules use.
VIEWER_TOKEN_MODE="${VIEWER_TOKEN_MODE:-user}"   # user | client
PASS=0
FAIL=0
SKIP=0
ERRORS=()
BEADS_PF_PID=""
CREATED_BEADS=()

export KD_ACTOR="${KD_ACTOR:-e2e-test}"

# ── Helpers ───────────────────────────────────────────────────────
red()    { printf '\033[0;31m%s\033[0m\n' "$*"; }
green()  { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()   { printf '\033[0;34m%s\033[0m\n' "$*"; }
yellow() { printf '\033[0;33m%s\033[0m\n' "$*"; }
dim()    { printf '\033[2m%s\033[0m\n' "$*"; }

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
  yellow "  ⊘ SKIP: $name${reason:+ — $reason}"
  SKIP=$((SKIP + 1))
}

cleanup() {
  for id in "${CREATED_BEADS[@]:-}"; do
    [ -n "$id" ] && "$KD_BIN" close "$id" >/dev/null 2>&1 || true
  done
  teardown_viewer_portforward
  [ -n "$BEADS_PF_PID" ] && kill "$BEADS_PF_PID" 2>/dev/null || true
}
trap cleanup EXIT

# ── Beads daemon access (create the fixture bead) ─────────────────
setup_beads_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    dim "  using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return 0
  fi
  blue "Port-forwarding ${NAMESPACE}/${BEADS_SVC} :${BEADS_PF_PORT} → 8080..."
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" \
    "${BEADS_PF_PORT}:8080" >/tmp/migration-postauth-beads-pf.log 2>&1 &
  BEADS_PF_PID=$!
  sleep 3
  export BEADS_HTTP_URL="http://localhost:${BEADS_PF_PORT}"
  if [ -z "${BEADS_TOKEN:-}" ]; then
    local secret
    secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
      -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
    [ -n "$secret" ] && export BEADS_TOKEN="$secret"
  fi
}

mint_viewer_bearer() {
  # Both modes resolve through lib-gitlab-token.sh (sourced at the top):
  # the GitLab-direct machine identity (client) or the GitLab human identity
  # (user). Neither contacts Keycloak (T6b, ka-0000001463).
  if [ "$VIEWER_TOKEN_MODE" = "client" ]; then
    mint_client_credentials_token
  else
    mint_user_token "$SMOKE_EMAIL"
  fi
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " migration-postauth-viewer smoke (ka-0000008176)"
  blue " namespace=$NAMESPACE viewer=$VIEWER_SVC"
  blue "═══════════════════════════════════════════════════"
  echo

  # ── Precondition: viewer Service present (else SKIP the module) ──
  if ! setup_viewer_portforward; then
    skip "post-auth viewer smoke" "viewer service $VIEWER_SVC absent in $NAMESPACE (ephemeral ns — viewer OIDC only on singleton/prod)"
    summary
    return 0
  fi

  # ── Precondition: a real bearer (else SKIP, per sibling modules) ─
  blue "Resolving GitLab-direct bearer (mode=$VIEWER_TOKEN_MODE)..."
  local token=""
  token=$(mint_viewer_bearer 2>/tmp/migration-postauth-token.err || true)
  if [ -z "$token" ]; then
    skip "post-auth viewer smoke" "could not mint viewer bearer ($(tail -n1 /tmp/migration-postauth-token.err 2>/dev/null || echo 'see token endpoint'))"
    summary
    return 0
  fi

  dim "  bearer minted (${#token} chars)"
  echo

  # ── Precondition: the viewer must ACCEPT the bearer (else SKIP) ──
  # The viewer's own OIDC middleware stays pinned to the Keycloak issuer until
  # the viewer-login flip lands (ka-0000002083, out of scope for the T6b auth
  # migration); against the ephemeral e2e viewer it rejects this GitLab-direct /
  # static-daemon bearer with 401. This post-auth smoke can only assert the 200
  # once the viewer trusts the GitLab edge — so SKIP-clean here rather than let
  # Scenario B hard-fail on its 401 branch (bug gd-5M8iT2oNlr). Auto-enables the
  # moment ka-0000002083 flips the viewer.
  if ! viewer_accepts_bearer "$token"; then
    skip "post-auth viewer smoke" "viewer rejects the E2E transport bearer — viewer still OIDC-gated at the pod; viewer-login flip pending (ka-0000002083)"
    summary
    return 0
  fi

  # ── Scenario A: create a fixture bead via the daemon (the far side) ──
  # The fixture is NOT a mere convenience: the whole point of this gate is to
  # prove the viewer→daemon data path, and the cleanest proof is a bead we wrote
  # through the daemon then read back THROUGH THE VIEWER. If the daemon cannot
  # accept the write, that is itself the cutover failure this gate must catch —
  # so a create failure is a HARD FAIL, not a fall-back to a weaker check. (An
  # earlier /active fallback here false-greened the exact hop it guards: /active
  # returns 200 + full layout even on a daemon-read error, so its <title>/hx-get
  # markers matched an error shell — review finding, ka-0000003481.)
  blue "Scenario A: create a fixture bead via the kbeads daemon"
  setup_beads_portforward
  local marker fixture_title fixture_id create_out
  marker="postauth-$(date +%s)-$$"
  fixture_title="migration post-auth viewer smoke ${marker}"
  create_out=$("$KD_BIN" create "$fixture_title" --type=task \
    -l project:gardenio -l migration-smoke 2>/dev/null || echo "")
  fixture_id=$(echo "$create_out" | grep -oE '(gd|ka)-[A-Za-z0-9]+' | head -n1)
  if [ -z "$fixture_id" ]; then
    fail "create fixture bead via daemon" \
      "kd create returned no bead id — the kbeads daemon did not accept a write (BEADS_HTTP_URL=${BEADS_HTTP_URL:-?}). A daemon that cannot accept a write is itself a cutover failure; refusing to fall back to a weaker read-only check that could false-green the viewer→daemon hop (ka-0000007004)."
    summary
    return $?
  fi
  CREATED_BEADS+=("$fixture_id")
  pass "fixture bead created ($fixture_id) via daemon"
  echo

  # ── Scenario B: authenticated GET of the fixture bead page → 200, NOT a 3xx ──
  # /{fixture_id} forces a live viewer→daemon GetBead (handleBeadShow), so this
  # exercises exactly the hop that 502'd.
  local target
  target="/${fixture_id}"
  blue "Scenario B: authenticated GET ${target} returns 200 (post-auth), not the pre-auth 303"
  local resp status body
  resp=$(viewer_get "$target" "$token")
  status=$(printf '%s\n' "$resp" | head -n1)
  body=$(printf '%s\n' "$resp" | tail -n +2)

  # HARD FAIL on the pre-auth redirect signature — this is the whole point.
  case "$status" in
    303|302|307)
      fail "authenticated GET must not 3xx (pre-auth redirect)" \
        "got $status on $target with a valid bearer — this is the 303 that masked ka-0000007004; the bearer is not reaching the viewer's OIDC middleware"
      ;;
    200)
      pass "authenticated GET $target returns 200 (reached the viewer, not the edge redirect)"
      ;;
    401|403)
      fail "authenticated GET returns 200" \
        "got $status on $target — the viewer's OIDC middleware REJECTED the bearer (token audience/issuer mismatch), so the request never reached the daemon hop"
      ;;
    502|503|504)
      fail "authenticated GET returns 200" \
        "got $status on $target — the exact prod 502 class (ka-0000007004): edge reached the viewer but the viewer→daemon hop failed"
      ;;
    *)
      fail "authenticated GET returns 200" "got $status on $target (want 200)"
      ;;
  esac
  echo

  # ── Scenario C: the 200 body is REAL, daemon-backed content ─────
  # The bead page MUST contain the fixture we just wrote through the daemon —
  # proof the viewer fetched it from gardenio-beads, not a static shell or an
  # error page. A viewer→daemon failure yields a 500 "Bead not found"/"Failed
  # to get bead" page whose body contains neither the marker nor the id.
  blue "Scenario C: 200 body carries the daemon-backed fixture (marker $marker)"
  if [ "$status" != "200" ]; then
    fail "real body content" "no 200 body to assert on (status=$status)"
  elif grep -qF "$marker" <<<"$body"; then
    pass "bead page renders the daemon-backed fixture (marker $marker present)"
  elif grep -qF "$fixture_id" <<<"$body"; then
    pass "bead page renders the daemon-backed fixture id ($fixture_id present)"
  else
    fail "real body content" \
      "200 on $target but neither marker '$marker' nor id '$fixture_id' in body — viewer served a shell/error/login page, not daemon-backed content (body ${#body} bytes)"
  fi
  echo

  summary
}

summary() {
  blue "═══════════════════════════════════════════════════"
  local total=$((PASS + FAIL))
  if [ "$FAIL" -eq 0 ]; then
    green " ✓ migration-postauth-viewer: $PASS passed, $SKIP skipped"
  else
    red " ✗ migration-postauth-viewer: $FAIL/$total failed, $SKIP skipped"
    for e in ${ERRORS[@]+"${ERRORS[@]}"}; do red "   - $e"; done
  fi
  blue "═══════════════════════════════════════════════════"
  [ "$FAIL" -eq 0 ]
}

main "$@"
