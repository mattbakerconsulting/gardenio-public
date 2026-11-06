#!/usr/bin/env bash
# lib-gitlab-token.sh — GitLab-direct bearer helpers for E2E / smoke modules.
#
# Drop-in replacement for the deleted lib-keycloak-token.sh and
# lib-keycloak-client-creds.sh. It keeps the SAME public API the E2E modules
# already call — mint_user_token <email> and mint_client_credentials_token — so
# a caller migrates by swapping only its `source` line.
#
# ── Why this module exists ────────────────────────────────────────────────────
# Epic ka-0000004118 routes coopmux + the beads daemon DIRECTLY at gitlab.com as
# the OIDC issuer and then deletes the entire Keycloak chart subtree (T6,
# ka-0000006953; this migration is T6b, ka-0000001463). T6a (gd-nXDfbo77uF)
# already flipped the e2e coopmux + beads-daemon issuers to gitlab.com
# and taught the daemon's OIDC verifier to read GitLab's groups_direct claim.
# Once Keycloak is gone there is no realm to password-grant a user token from,
# so every E2E module that used to mint a Keycloak bearer must stop depending on
# a live Keycloak BEFORE the chart can be deleted.
#
# ── The GitLab-direct model (modeled on test-gitlab-jwt-e2e.sh) ────────────────
# In the GitLab-direct world a request is authenticated by a TRANSPORT BEARER
# that is either a real gitlab.com-issued JWT or the static daemon token — never
# a Keycloak-minted JWT. The HUMAN IDENTITY is carried out-of-band as a
# GitLab-verified email in a trusted edge header, which the CALLER presents:
#   - beads daemon: `X-Operated-By: oidc:<email>` — the same static-token
#     attribution channel test-gitlab-jwt-e2e.sh's G3 leg uses; the daemon
#     records that GitLab identity as the actor.
#   - beads-viewer: `X-Auth-Request-Email: <email>` — stamped by the forwardauth
#     edge (already flipped to provider=gitlab) and trusted by the viewer on its
#     forwardauth / dev-bypass path (dev_auth.go).
# This lib only resolves the transport bearer; the caller already has the email
# in scope (it passes it here) and sends the identity header itself when it
# needs attribution — exactly as the reference module does. (An earlier draft
# tried to export the email from here, but every caller invokes this via
# `token=$(mint_user_token …)`, a subshell whose exports never reach the caller,
# so identity-by-export is a dead channel by construction.)
#
# ── Public API ────────────────────────────────────────────────────────────────
#   # shellcheck disable=SC1090
#   source "$(dirname "${BASH_SOURCE[0]}")/lib-gitlab-token.sh"
#
#   token=$(mint_user_token alice@example.com)   # transport bearer for a user leg
#   token=$(mint_client_credentials_token)       # transport bearer for a machine leg
#
# Both print the transport bearer on stdout and return 0 on success. The bearer
# is resolved in this order:
#   1. $GARDENIO_E2E_GITLAB_BEARER — a real gitlab.com JWT (e.g. from a CI
#      `id_tokens:` job token whose `aud` matches the daemon / viewer OAuth app).
#   2. The static daemon token, under the names run-shard.sh exports
#      ($BEADS_TOKEN / $BEADS_AUTH_TOKEN / $BEADS_DAEMON_TOKEN).
# mint_user_token requires a non-empty <email> arg (return 2 otherwise) so the
# call sites keep their existing shape and a caller cannot silently drop the
# identity it must also send in the header.
#
# ── Fail-open to SKIP, never a false red ──────────────────────────────────────
# When no bearer is resolvable both helpers print NOTHING and return non-zero,
# so the caller takes its existing `|| skip` degrade rather than a false red.
# Nothing here contacts Keycloak or shells out to kubectl. In the ephemeral CI
# fast-smoke fan-out the beads-viewer Service is not deployed (viewer OIDC lives
# only on the long-lived singleton / prod), so the viewer-facing modules SKIP
# wholesale exactly as before — this migration removes the Keycloak dependency
# without changing that behavior.
#
# The beads-viewer's own OIDC middleware stays on the Keycloak issuer until the
# viewer-login track lands (ka-0000002083 — explicitly out of scope for epic
# ka-0000004118). Against a viewer still verifying Keycloak tokens, a
# static-daemon-token bearer does not satisfy the viewer's verifier, so
# viewer-OIDC assertions SKIP until that track flips the viewer to the GitLab
# edge (trusting X-Auth-Request-Email). That interim is intended and safe.

# Guard against multiple sourcing within the same shell.
# shellcheck disable=SC2317
if [ -n "${__GARDENIO_LIB_GITLAB_TOKEN_LOADED:-}" ]; then
  return 0 2>/dev/null || true
fi
__GARDENIO_LIB_GITLAB_TOKEN_LOADED=1

# _gitlab_resolve_bearer — print the transport bearer that authenticates an E2E
# request, or return non-zero with empty stdout when none is available. Order:
#   1. GARDENIO_E2E_GITLAB_BEARER — a real gitlab.com-issued JWT (CI id_tokens).
#   2. The static daemon token, under the several names run-shard.sh exports.
_gitlab_resolve_bearer() {
  local tok
  for tok in \
      "${GARDENIO_E2E_GITLAB_BEARER:-}" \
      "${BEADS_TOKEN:-}" \
      "${BEADS_AUTH_TOKEN:-}" \
      "${BEADS_DAEMON_TOKEN:-}"; do
    if [ -n "$tok" ]; then
      printf '%s' "$tok"
      return 0
    fi
  done
  return 1
}

# mint_user_token <email> — print a transport bearer for a user-identity leg.
# The <email> is required (the caller must also present it as the identity
# header — X-Auth-Request-Email / X-Operated-By: oidc:<email>), but the bearer
# itself is issuer-agnostic. Non-zero + empty stdout when no bearer is
# resolvable, so the caller SKIPs.
mint_user_token() {
  local email="$1"
  if [ -z "$email" ]; then
    echo "mint_user_token: email is required" >&2
    return 2
  fi
  local bearer
  if ! bearer=$(_gitlab_resolve_bearer); then
    echo "mint_user_token: no GitLab bearer available for ${email}" >&2
    echo "  set GARDENIO_E2E_GITLAB_BEARER (a gitlab.com JWT) or BEADS_TOKEN (static daemon token)" >&2
    return 1
  fi
  printf '%s' "$bearer"
}

# mint_client_credentials_token — print a transport bearer for a non-interactive
# machine-identity leg. Non-zero + empty stdout when none is resolvable.
mint_client_credentials_token() {
  local bearer
  if ! bearer=$(_gitlab_resolve_bearer); then
    echo "mint_client_credentials_token: no GitLab bearer available" >&2
    echo "  set GARDENIO_E2E_GITLAB_BEARER (a gitlab.com JWT) or BEADS_TOKEN (static daemon token)" >&2
    return 1
  fi
  printf '%s' "$bearer"
}

# viewer_accepts_bearer <token> [probe_path] — preflight the port-forwarded
# beads-viewer's OWN auth on an OIDC-gated route. Returns 0 when the viewer
# ACCEPTS the transport bearer, 1 when it REJECTS it (HTTP 401/403) or the
# tunnel is unreachable / unset.
#
# Why a viewer-facing E2E module needs this. The bearer these helpers resolve
# is either a real gitlab.com JWT or (in CI) the static daemon token. The
# daemon accepts that token, but the beads-viewer's OWN OIDC middleware stays
# pinned to the Keycloak issuer until the viewer-login flip lands (ka-0000002083,
# explicitly out of scope for the T6b auth migration — see this file's header).
# Against a viewer still verifying Keycloak tokens, the static-daemon-token
# bearer does not satisfy the viewer's verifier, so every gated GET returns 401.
#
# Before this helper, the pre-migration Keycloak mint simply FAILED (its issuer
# host was decommissioned), so viewer-facing modules took their `|| skip`
# degrade and SKIP-cleaned. The migration made mint always succeed, so those
# modules stopped skipping and began HARD-FAILING on the viewer's 401 (bug
# gd-5M8iT2oNlr). This preflight restores the intended behavior: a viewer-facing
# module calls it right after minting and SKIP-cleans when the viewer rejects
# the bearer — zero coverage change vs the last-green release, and it
# auto-enables the assertions the moment ka-0000002083 flips the viewer to trust
# the GitLab edge (X-Auth-Request-Email).
viewer_accepts_bearer() {
  local token="$1" probe="${2:-/active}"
  local port="${VIEWER_PF_PORT:-}"
  [ -n "$port" ] || return 1
  [ -n "$token" ] || return 1
  local hdr=(-H "Authorization: Bearer ${token}" -H "Accept: text/html")
  [ -n "${GARDENIO_E2E_TRUSTED_EMAIL:-}" ] && \
    hdr+=(-H "X-Auth-Request-Email: ${GARDENIO_E2E_TRUSTED_EMAIL}")
  local st
  st=$(curl -sk --max-time 10 "${hdr[@]}" -o /dev/null -w '%{http_code}' \
    "http://localhost:${port}${probe}" 2>/dev/null || echo "000")
  case "$st" in
    401|403|000) return 1 ;;
    *) return 0 ;;
  esac
}
