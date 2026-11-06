#!/usr/bin/env bash
# lib-keycloak-client-creds.sh — client_credentials grant helper for
# agent pods that need a non-interactive Keycloak bearer.
#
# SCOPE (post-T6b, ka-0000001463): the gardenio-cluster CI E2E test modules
# have migrated OFF Keycloak to the GitLab-direct flow (lib-gitlab-token.sh);
# this helper is RETAINED for the two paths that still legitimately need a real
# Keycloak-minted bearer:
#   1. gardenio-in-a-box (GiB) self-serve user management
#      (lib-keycloak-box-useradmin.sh), which drives the GiB's OWN in-box
#      Keycloak — a deliberately zero-external-deps component that is NOT part
#      of the T6 cluster-Keycloak chart deletion (advice ka-0000005379); and
#   2. fetch-beads-viewer-prod.sh, the prod agent-pod viewer-fetch tool, which
#      port-forwards to the PROD beads-viewer ClusterIP and presents the bearer
#      to the viewer's OWN OIDC middleware (bypassing the edge). That middleware
#      still verifies Keycloak-issued tokens until the viewer-login migration
#      lands (ka-0000002083, explicitly out of scope for epic ka-0000004118), so
#      a GitLab-direct bearer would be rejected there; it migrates WITH the
#      viewer, not in T6b.
# Do NOT source this from a gardenio-cluster CI E2E test module; use
# lib-gitlab-token.sh instead.
#
# Companion to the (deleted) lib-keycloak-token.sh user-token helper. Use this
# when there is no human session — agent pods running in K8s cannot
# do interactive OIDC, but the keycloak-operator gives every
# `serviceAccount.enabled: true` KeycloakClient an auto-rotated
# client_secret in a K8s Secret that an appropriately RBAC'd
# ServiceAccount can read.
#
# Bead chain: gd-FlaSY3fKqC (this helper) → ka-0000008133 (Track B
# epic) → ka-0000003037 (plan).
#
# Usage:
#   # shellcheck disable=SC1090
#   source "$(dirname "${BASH_SOURCE[0]}")/lib-keycloak-client-creds.sh"
#
#   token=$(mint_client_credentials_token)
#
#   # Then call beads-viewer or any aud=gardenio-beads-viewer endpoint:
#   curl -H "Authorization: Bearer ${token}" \
#     https://beads-viewer.gardenio-e2e.app.e2e.dev.acme.example/api/...
#
# Inputs (env, override before sourcing or per-call):
#   KC_BASE_URL        (default https://auth.e2e.dev.acme.example)
#   KC_REALM           (default gardenio)
#   KC_CLIENT_ID       (default gardenio-info-density-audit)
#   KC_CLIENT_SECRET   if set, used directly (skips the K8s Secret read)
#   KC_CLIENT_SECRET_NS    namespace holding the operator-managed Secret
#                          (default: $POD_NAMESPACE, then current kubectl ctx)
#   KC_CLIENT_SECRET_NAME  Secret name (default derived from KC_CLIENT_ID and
#                          KC_RELEASE_NAME: keycloak-client-<release>-client-<clientId>-secret)
#   KC_CLIENT_SECRET_KEY   key within the Secret (default clientSecret)
#   KC_RELEASE_NAME    helm release name (default $RELEASE_NAME, then
#                      $POD_NAMESPACE — the chart names KeycloakClient
#                      CRDs with the release fullname prefix)
#
# Requirements:
#   - curl, jq in PATH.
#   - When KC_CLIENT_SECRET is unset: kubectl in PATH with permission to
#     `get secrets` on KC_CLIENT_SECRET_NAME in KC_CLIENT_SECRET_NS.
#   - In-cluster pods: agents.agentServiceAccount.extraRules must
#     include get on that Secret.
#
# Output: prints the access_token on stdout, nothing else. Non-zero on
# any error (writes diagnostics to stderr — never to stdout).

# shellcheck disable=SC2317
if [ -n "${__GARDENIO_LIB_KEYCLOAK_CLIENT_CREDS_LOADED:-}" ]; then
  return 0 2>/dev/null || true
fi
__GARDENIO_LIB_KEYCLOAK_CLIENT_CREDS_LOADED=1

: "${KC_BASE_URL:=https://auth.e2e.dev.acme.example}"
: "${KC_REALM:=gardenio}"
: "${KC_CLIENT_ID:=gardenio-info-density-audit}"
: "${KC_CLIENT_SECRET_KEY:=clientSecret}"

# kc_release_name — best-effort lookup of the helm release name. The
# EDP keycloak-operator names auto-generated client secrets after the
# KeycloakClient CRD's metadata.name, which the chart templates as
# `{{ $fullname }}-client-{{ .clientId }}`. The chart fullname is the
# release name in this repo's deploy values.
kc_release_name() {
  if [ -n "${KC_RELEASE_NAME:-}" ]; then
    printf '%s' "$KC_RELEASE_NAME"
    return 0
  fi
  if [ -n "${RELEASE_NAME:-}" ]; then
    printf '%s' "$RELEASE_NAME"
    return 0
  fi
  if [ -n "${POD_NAMESPACE:-}" ]; then
    printf '%s' "$POD_NAMESPACE"
    return 0
  fi
  echo "kc_release_name: set KC_RELEASE_NAME, RELEASE_NAME, or POD_NAMESPACE" >&2
  return 1
}

# kc_client_secret_namespace — namespace holding the operator-managed
# secret. Defaults to KC_CLIENT_SECRET_NS, then $POD_NAMESPACE (set by
# the downward API in agent pods), then the current kubectl context's
# default namespace.
kc_client_secret_namespace() {
  if [ -n "${KC_CLIENT_SECRET_NS:-}" ]; then
    printf '%s' "$KC_CLIENT_SECRET_NS"
    return 0
  fi
  if [ -n "${POD_NAMESPACE:-}" ]; then
    printf '%s' "$POD_NAMESPACE"
    return 0
  fi
  local ns
  ns=$(kubectl config view --minify -o jsonpath='{..namespace}' 2>/dev/null || true)
  if [ -n "$ns" ]; then
    printf '%s' "$ns"
    return 0
  fi
  echo "kc_client_secret_namespace: set KC_CLIENT_SECRET_NS or POD_NAMESPACE" >&2
  return 1
}

# kc_client_secret_name — derive the K8s Secret name created by the
# keycloak-operator for KC_CLIENT_ID. Override via
# KC_CLIENT_SECRET_NAME if the operator naming changes.
kc_client_secret_name() {
  if [ -n "${KC_CLIENT_SECRET_NAME:-}" ]; then
    printf '%s' "$KC_CLIENT_SECRET_NAME"
    return 0
  fi
  local release
  release=$(kc_release_name) || return 1
  printf 'keycloak-client-%s-client-%s-secret' "$release" "$KC_CLIENT_ID"
}

# kc_client_secret — read the client_secret value, preferring the env
# override KC_CLIENT_SECRET (handy for local testing or CI where the
# secret is materialized into env). Falls back to reading the
# operator-managed K8s Secret via kubectl.
kc_client_secret() {
  if [ -n "${KC_CLIENT_SECRET:-}" ]; then
    printf '%s' "$KC_CLIENT_SECRET"
    return 0
  fi
  local ns name pw
  ns=$(kc_client_secret_namespace) || return 1
  name=$(kc_client_secret_name) || return 1
  pw=$(kubectl -n "$ns" get secret "$name" \
    -o jsonpath="{.data.${KC_CLIENT_SECRET_KEY}}" 2>/dev/null \
    | base64 -d 2>/dev/null) || true
  if [ -z "$pw" ]; then
    echo "kc_client_secret: failed to read ${ns}/${name}#${KC_CLIENT_SECRET_KEY}" >&2
    echo "  (set KC_CLIENT_SECRET to bypass kubectl, or grant agent SA get on this Secret)" >&2
    return 1
  fi
  printf '%s' "$pw"
}

# mint_client_credentials_token — POST a client_credentials grant to
# KC_REALM with (KC_CLIENT_ID, KC_CLIENT_SECRET) and print the
# access_token on stdout. Non-zero exit on error.
mint_client_credentials_token() {
  local secret
  secret=$(kc_client_secret) || return 1
  local resp
  resp=$(curl -sS --fail-with-body \
    -H "Content-Type: application/x-www-form-urlencoded" \
    --data-urlencode "grant_type=client_credentials" \
    --data-urlencode "client_id=${KC_CLIENT_ID}" \
    --data-urlencode "client_secret=${secret}" \
    "${KC_BASE_URL}/auth/realms/${KC_REALM}/protocol/openid-connect/token") || {
      echo "mint_client_credentials_token: token endpoint failed (realm=${KC_REALM}, client=${KC_CLIENT_ID})" >&2
      echo "  response: $resp" >&2
      return 1
    }
  local token
  token=$(printf '%s' "$resp" | jq -er '.access_token // empty') || {
    echo "mint_client_credentials_token: no access_token in response: $resp" >&2
    return 1
  }
  printf '%s' "$token"
}
