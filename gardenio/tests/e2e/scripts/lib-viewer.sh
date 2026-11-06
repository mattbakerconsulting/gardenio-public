#!/usr/bin/env bash
# lib-viewer.sh — shared beads-viewer port-forward + authenticated-GET
# helpers for E2E / smoke modules.
#
# Three modules had grown byte-identical copies of setup_viewer_portforward
# and viewer_get (test-viewer-migration-banner.sh, test-viewer-bead-activity.sh,
# test-agents-live-view.sh), each carrying a "TODO(third caller): extract into
# gardenio/tests/e2e/scripts/lib-viewer.sh" note. The P4.4 migration post-auth
# smoke check (ka-0000008176) is the fourth caller, so the extraction is done
# here. New modules should source this lib rather than re-inline the pattern.
#
# WHY the port-forward-to-ClusterIP shape matters (ka-0000007004):
#   The public edge (AVA / oauth2-proxy in front of Traefik) 303/302-redirects
#   UNAUTHENTICATED browser traffic to OIDC and 308-redirects bearer traffic
#   before it ever reaches the viewer. That "pre-auth 303" is exactly what
#   masked the P4.3b prod 502 — every prior proof-of-life only saw the edge
#   redirect and never exercised the viewer pod -> kbeads daemon data path.
#   To assert an AUTHENTICATED 200 with a real body you must BYPASS the edge:
#   port-forward straight to svc/<release>-beads-viewer:8095 and present an
#   authenticated bearer to the viewer on localhost. In the GitLab-direct
#   model (lib-gitlab-token.sh) that identity is a GitLab-verified email, carried
#   as the edge-stamped X-Auth-Request-Email header (viewer_get adds it); the
#   transport bearer is a gitlab.com JWT or the static daemon token — never a
#   Keycloak-minted JWT (T6b, ka-0000001463).
#   (Same technique as fetch-beads-viewer-prod.sh:9-27,84-85.)
#
# Usage:
#   # shellcheck disable=SC1091
#   source "$(dirname "${BASH_SOURCE[0]}")/lib-viewer.sh"
#
#   NAMESPACE=gardenio-e2e setup_viewer_portforward || { skip "no viewer"; ... }
#   token=$(mint_user_token e2e-smoke-$$@example.com)   # from lib-gitlab-token.sh
#   resp=$(viewer_get /active "$token")
#   status=$(printf '%s\n' "$resp" | head -n1)
#   body=$(printf '%s\n' "$resp" | tail -n +2)
#   [ "$status" = 200 ] || fail "want 200 got $status"
#
# Tunables (override via env before sourcing, or per-call):
#   NAMESPACE        (default gardenio-e2e) — target namespace
#   RELEASE          (default $NAMESPACE)  — helm release name
#   VIEWER_SVC       (default ${RELEASE}-beads-viewer)
#   VIEWER_PF_PORT   (default 19801)       — local port for the forward
#   VIEWER_TARGET_PORT (default 8095)      — viewer container/service port
#   VIEWER_PF_LOG    (default /tmp/lib-viewer-pf.log)
#
# On success setup_viewer_portforward sets VIEWER_PF_PID (the caller's cleanup
# trap should `kill "$VIEWER_PF_PID"`). Callers that already define their own
# identically-named helpers keep them; this lib is only sourced by modules that
# do NOT define their own (guarded so a double-source is a no-op).

# shellcheck disable=SC2317
if [ -n "${__GARDENIO_LIB_VIEWER_LOADED:-}" ]; then
  return 0 2>/dev/null || true
fi
__GARDENIO_LIB_VIEWER_LOADED=1

: "${NAMESPACE:=gardenio-e2e}"
: "${RELEASE:=${NAMESPACE}}"
: "${VIEWER_SVC:=${RELEASE}-beads-viewer}"
: "${VIEWER_PF_PORT:=19801}"
: "${VIEWER_TARGET_PORT:=8095}"
: "${VIEWER_PF_LOG:=/tmp/lib-viewer-pf.log}"

VIEWER_PF_PID="${VIEWER_PF_PID:-}"

# setup_viewer_portforward — port-forward svc/${VIEWER_SVC}:${VIEWER_TARGET_PORT}
# to localhost:${VIEWER_PF_PORT}. Returns non-zero (caller should SKIP) when the
# viewer Service is absent — the viewer OIDC path is only deployed on the
# long-lived singleton / prod, never in the minimal ephemeral e2e namespaces
# (edge-auth/README.md:99-105). Sets VIEWER_PF_PID on success.
setup_viewer_portforward() {
  if ! kubectl -n "$NAMESPACE" get svc "$VIEWER_SVC" >/dev/null 2>&1; then
    printf '\033[0;33mviewer service %s not found in %s — skipping module\033[0m\n' \
      "$VIEWER_SVC" "$NAMESPACE" >&2
    return 1
  fi
  printf '\033[0;34mPort-forwarding %s/%s :%s → %s...\033[0m\n' \
    "$NAMESPACE" "$VIEWER_SVC" "$VIEWER_PF_PORT" "$VIEWER_TARGET_PORT" >&2
  kubectl -n "$NAMESPACE" port-forward "svc/${VIEWER_SVC}" \
    "${VIEWER_PF_PORT}:${VIEWER_TARGET_PORT}" >"$VIEWER_PF_LOG" 2>&1 &
  VIEWER_PF_PID=$!
  local i
  for i in 1 2 3 4 5 6 7 8 9 10; do
    # /healthz is unauthenticated on the viewer; a plain / connect also proves
    # the tunnel is up even if healthz is gated on a given build.
    if curl -sf --max-time 1 -o /dev/null \
        "http://localhost:${VIEWER_PF_PORT}/healthz" 2>/dev/null \
        || curl -s --max-time 1 -o /dev/null \
            "http://localhost:${VIEWER_PF_PORT}/" 2>/dev/null; then
      printf '\033[2m  Viewer tunnel ready on :%s (pid %s)\033[0m\n' \
        "$VIEWER_PF_PORT" "$VIEWER_PF_PID" >&2
      return 0
    fi
    sleep 0.5
  done
  printf '\033[0;33m  Viewer port-forward did not become healthy within 5s; continuing\033[0m\n' >&2
  return 0
}

# teardown_viewer_portforward — kill the forward if we started one.
teardown_viewer_portforward() {
  [ -n "${VIEWER_PF_PID:-}" ] && kill "$VIEWER_PF_PID" 2>/dev/null || true
  VIEWER_PF_PID=""
}

# viewer_get <path> <token> [extra_curl_args...]
# GETs http://localhost:$VIEWER_PF_PORT$path with bearer auth.
# Echoes "<status>\n<body>" on stdout (status on first line, body on rest).
#
# CRITICAL: no -L. A redirect (the pre-auth 303 that masked ka-0000007004) must
# surface as the HTTP status, NOT be silently followed to a downstream 200.
# In the GitLab-direct model (lib-gitlab-token.sh) the forwardauth edge —
# already flipped to provider=gitlab — verifies the GitLab login and stamps the
# viewer-trusted X-Auth-Request-Email header (viewer dev_auth.go trusts it on
# the forwardauth/dev-bypass path). A caller that wants to exercise that
# dev-bypass identity path directly against a port-forwarded viewer may EXPORT
# GARDENIO_E2E_TRUSTED_EMAIL before calling viewer_get; when set, it is presented
# here so an authenticated GET carries the GitLab identity even when the
# transport bearer is the static daemon token rather than a Keycloak-minted JWT.
# (It is NOT auto-derived from mint_user_token — every caller invokes that via
# `token=$(mint_user_token …)`, a subshell whose exports never reach the caller,
# so identity-by-export from the minter is a dead channel; the caller sets it.)
# Additive and safe: unset by default it sends nothing, a viewer running its own
# OIDC verifier ignores this header, and a client-facing AVA edge strips it.
_viewer_trusted_email_header() {
  if [ -n "${GARDENIO_E2E_TRUSTED_EMAIL:-}" ]; then
    printf 'X-Auth-Request-Email: %s' "$GARDENIO_E2E_TRUSTED_EMAIL"
  fi
}

viewer_get() {
  local path="$1" token="$2"
  shift 2
  local out status email_hdr
  out=$(mktemp)
  email_hdr="$(_viewer_trusted_email_header)"
  status=$(curl -sk --max-time 10 \
    -H "Authorization: Bearer ${token}" \
    ${email_hdr:+-H "$email_hdr"} \
    -H "Accept: text/html" \
    -o "$out" \
    -w '%{http_code}' \
    "$@" \
    "http://localhost:${VIEWER_PF_PORT}${path}" 2>/dev/null || echo "000")
  printf '%s\n' "$status"
  cat "$out"
  rm -f "$out"
}

viewer_status() {
  local path="$1" token="$2"
  shift 2
  local email_hdr
  email_hdr="$(_viewer_trusted_email_header)"
  curl -sk --max-time 10 \
    -H "Authorization: Bearer ${token}" \
    ${email_hdr:+-H "$email_hdr"} \
    -H "Accept: text/html" \
    -o /dev/null \
    -w '%{http_code}' \
    "$@" \
    "http://localhost:${VIEWER_PF_PORT}${path}" 2>/dev/null || echo "000"
}
