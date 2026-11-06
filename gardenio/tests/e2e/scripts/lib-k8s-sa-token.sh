#!/usr/bin/env bash
# lib-k8s-sa-token.sh — mint an audience-pinned projected ServiceAccount
# token for the PROD gardenio-system beads-viewer (the k8sSA bearer path).
#
# Why this exists
# ---------------
# The prod `gardenio-system` viewer is Entra-gated for browsers — agents
# cannot complete an interactive Entra code flow, and a Keycloak
# `client_credentials` bearer (lib-keycloak-client-creds.sh) is rejected
# there because the viewer accepts exactly one OIDC issuer. To let
# CDQR/audit pods authenticate to the REAL prod viewer where the rich
# gardeners live, beads-viewer grew a SECOND verified machine-token issuer:
# the EKS OIDC issuer, accepting an audience-pinned projected SA token
# (code ka-0000006288, chart `beadsViewer.k8sSA.*`, decision
# ka-0000001298 → entra-audit-client). The verified subject
# `system:serviceaccount:gardenio-system:gardenio-audit` maps to a synthetic
# operator identity with a `gardenio-viewer` role floor.
#
# This helper mints that token via the Kubernetes TokenRequest API
# (`kubectl create token`). It is the PROD counterpart of
# lib-keycloak-client-creds.sh; the Keycloak path stays as the e2e
# fallback (the e2e viewer is Keycloak-gated, not Entra-gated).
#
# Bead chain: ka-0000004324 (W4, this helper) → epic ka-0000007405 →
# decision ka-0000001298 / plan ka-0000004227.
#
# Usage:
#   # shellcheck disable=SC1091
#   source "$(dirname "${BASH_SOURCE[0]}")/lib-k8s-sa-token.sh"
#
#   token=$(mint_viewer_sa_token)
#
#   # Then call the prod viewer (port-forward to the ClusterIP first):
#   curl -H "Authorization: Bearer ${token}" http://localhost:8095/gardener/<id>
#
# Inputs (env, override before sourcing or per-call):
#   SA_TOKEN_NAMESPACE   (default gardenio-system) — namespace of the SA
#   SA_TOKEN_NAME        (default gardenio-audit)  — ServiceAccount name
#   SA_TOKEN_AUDIENCE    (default gardenio-beads-viewer-audit) — the
#                        dedicated audience the viewer's k8sSA branch pins.
#                        MUST NOT be the apiserver audience
#                        (https://kubernetes.default.svc) or the IRSA
#                        audience (sts.amazonaws.com) — the viewer rejects
#                        both (oidcauth forbiddenSAAudiences).
#   SA_TOKEN_DURATION    (default 15m) — requested token lifetime; the
#                        apiserver may clamp it to its configured max.
#
# Requirements:
#   - kubectl in PATH with `create serviceaccounts/token` on
#     SA_TOKEN_NAME in SA_TOKEN_NAMESPACE. Prod audit pods already have
#     this (verified live in ka-0000006288). Check with:
#       kubectl auth can-i create serviceaccounts/token \
#         -n gardenio-system --subresource=token
#
# Output: prints the projected SA token on stdout, nothing else. Non-zero
# exit on any error (diagnostics go to stderr — never to stdout, so the
# token capture stays clean).

# shellcheck disable=SC2317
if [ -n "${__GARDENIO_LIB_K8S_SA_TOKEN_LOADED:-}" ]; then
  return 0 2>/dev/null || true
fi
__GARDENIO_LIB_K8S_SA_TOKEN_LOADED=1

: "${SA_TOKEN_NAMESPACE:=gardenio-system}"
: "${SA_TOKEN_NAME:=gardenio-audit}"
: "${SA_TOKEN_AUDIENCE:=gardenio-beads-viewer-audit}"
: "${SA_TOKEN_DURATION:=15m}"

# Audiences the viewer's k8sSA branch refuses on purpose (mirrors
# oidcauth.forbiddenSAAudiences). Minting with one of these would produce a
# token the viewer rejects — fail loud at mint time rather than surface a
# confusing 401 downstream.
__sa_forbidden_audience() {
  case "$1" in
    "https://kubernetes.default.svc"|"sts.amazonaws.com") return 0 ;;
    *) return 1 ;;
  esac
}

# mint_viewer_sa_token — request an audience-pinned projected SA token via
# the Kubernetes TokenRequest API and print it on stdout. Non-zero on error.
mint_viewer_sa_token() {
  command -v kubectl >/dev/null 2>&1 || {
    echo "mint_viewer_sa_token: kubectl not found in PATH" >&2
    return 1
  }
  if __sa_forbidden_audience "$SA_TOKEN_AUDIENCE"; then
    echo "mint_viewer_sa_token: refusing forbidden audience '${SA_TOKEN_AUDIENCE}'" >&2
    echo "  (the apiserver/IRSA audiences are rejected by the viewer's k8sSA branch)" >&2
    return 1
  fi
  local token
  token=$(kubectl -n "$SA_TOKEN_NAMESPACE" create token "$SA_TOKEN_NAME" \
    --audience "$SA_TOKEN_AUDIENCE" \
    --duration "$SA_TOKEN_DURATION" 2>/tmp/sa-token-mint.err) || {
      echo "mint_viewer_sa_token: 'kubectl create token' failed for ${SA_TOKEN_NAMESPACE}/${SA_TOKEN_NAME} (aud=${SA_TOKEN_AUDIENCE})" >&2
      sed 's/^/  /' /tmp/sa-token-mint.err >&2 2>/dev/null || true
      echo "  (need: kubectl auth can-i create serviceaccounts/token -n ${SA_TOKEN_NAMESPACE} --subresource=token)" >&2
      return 1
    }
  [ -n "$token" ] || {
    echo "mint_viewer_sa_token: minted token was empty" >&2
    return 1
  }
  printf '%s' "$token"
}
