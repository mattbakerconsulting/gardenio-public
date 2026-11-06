#!/usr/bin/env bash
# fetch-beads-viewer-prod.sh — self-service authenticated fetch of a
# PROD beads-viewer page from inside a gardenio agent pod, for CDQR /
# visual-QA work.
#
# Why this exists
# ---------------
# beads-viewer correctly 403s unauthenticated browser traffic (OIDC
# middleware), and the public ALB 308-redirects bearer requests before
# they reach Traefik — so an agent cannot just curl the public host with a
# token. The supported path is:
#
#   1. Mint a viewer-accepted bearer (see AUTH MODES below).
#   2. Port-forward straight to the beads-viewer ClusterIP, bypassing
#      the ALB entirely.
#   3. Present the bearer to beads-viewer's own OIDC middleware on
#      localhost.
#
# This wrapper packages those three steps into one command an agent can
# run to grab either the raw DOM or a PNG screenshot of a prod page.
#
# AUTH MODES (choose with --auth, default sa):
#   sa (default)  — mint an audience-pinned projected ServiceAccount token
#                   via `kubectl create token` (lib-k8s-sa-token.sh). This
#                   is the path the REAL prod `gardenio-system` viewer
#                   accepts: it is Entra-gated for browsers, so a Keycloak
#                   client_credentials bearer is rejected there, but the
#                   viewer's k8sSA branch (code ka-0000006288, chart
#                   `beadsViewer.k8sSA.*`, decision ka-0000001298) verifies
#                   the EKS-issued SA token with a pinned audience. Requires
#                   the prod k8sSA branch to be enabled + the audit operator
#                   row provisioned (epic ka-0000007405).
#   keycloak      — mint a viewer-audience bearer via the Keycloak
#                   client_credentials grant (lib-keycloak-client-creds.sh,
#                   advice ka-0000006483). This is the e2e fallback: the
#                   e2e viewer is Keycloak-gated, not
#                   Entra-gated. Proven by the scheduled info-density audit
#                   (epic ka-0000005684). --operator implies this mode.
#
# Task: ka-0000002064 (prod variant of Track B, ka-0000008133);
#       ka-0000004324 (W4 — SA-token auth mode).
# Sister doc: gardenio/docs/inpod-beads-viewer-fetch.md,
#             gardenio/docs/info-density-audit-harness.md
#
# Usage:
#   gardenio/tests/e2e/scripts/fetch-beads-viewer-prod.sh [options] <path>
#
#   <path>   beads-viewer route to fetch, e.g. /p/gardenio, /ka-0000002064,
#            /inbox, /agents?project=gardenio. Leading slash optional.
#
# Options:
#   --dom            Fetch the rendered HTML/DOM (default). Writes to
#                    --out or stdout.
#   --screenshot     Capture a PNG screenshot via Playwright. Requires
#                    --out (a .png path). Falls back with a clear error
#                    if the Playwright harness deps are unavailable.
#   --out <file>     Write output to <file> instead of stdout (DOM) or
#                    the default screenshot path.
#   --port <n>       Local port for the forward (default 8095).
#   --no-pf          Skip port-forward; assume BEADS_VIEWER_URL already
#                    points at a reachable viewer (e.g. an existing
#                    forward, or local dev). Still mints + presents the
#                    bearer.
#   --auth <mode>    Bearer mint mode: sa (default) or keycloak. See
#                    AUTH MODES above.
#   --keycloak       Shorthand for --auth keycloak (e2e fallback).
#   --sa             Shorthand for --auth sa (prod gardenio-system).
#   --operator       Capture the OPERATOR surface: mint the bearer via the
#                    gardenio-camp-audit client (realm role gardenio-operator)
#                    instead of the default gardenio-info-density-audit
#                    (gardenio-viewer). Required to see operator-gated UI such
#                    as the /gardener/{id} lifecycle control bar — a
#                    viewer-scoped token correctly hides it, which is why the
#                    gardeners-page CAMP audit's `control_bar_present` metric
#                    needs this flag (bug ka-0000007161). Equivalent to
#                    setting KC_CLIENT_ID=gardenio-camp-audit. Forces
#                    --auth keycloak (the SA path has a fixed gardenio-viewer
#                    role floor and cannot surface operator-gated UI).
#   -h, --help       Show this help.
#
# Env (sa mode, passthrough to lib-k8s-sa-token.sh):
#   SA_TOKEN_NAMESPACE (default gardenio-system), SA_TOKEN_NAME (default
#   gardenio-audit), SA_TOKEN_AUDIENCE (default gardenio-beads-viewer-audit),
#   SA_TOKEN_DURATION (default 15m) — see that lib.
#
# Env (keycloak mode, passthrough to lib-keycloak-client-creds.sh):
#   KC_BASE_URL, KC_REALM, KC_CLIENT_ID, KC_CLIENT_SECRET,
#   KC_CLIENT_SECRET_NS, KC_RELEASE_NAME, POD_NAMESPACE — see that lib
#   and gardenio/docs/agent-pod-keycloak-auth.md. Defaults resolve to the
#   gardenio realm + the gardenio-info-density-audit client, whose
#   operator-managed Secret the agent ServiceAccount can already read.
#
# Requirements: bash, curl, jq, kubectl (for the port-forward, the SA
# token mint, and — keycloak mode — the K8s-Secret read). Screenshot mode
# additionally needs the Playwright harness under
# gardenio/tests/beads-viewer-screenshots.
#
# Output: the DOM (stdout or --out) or a PNG (--out). Diagnostics go to
# stderr. Non-zero exit on any failure.

set -euo pipefail

# This script lives at gardenio/tests/e2e/scripts/, so the monorepo root is
# four levels up (scripts → e2e → tests → gardenio → <root>). The screenshot
# harness path below is anchored at ${REPO_ROOT}/gardenio/..., so a 3-level
# REPO_ROOT (landing on gardenio/) double-prepends gardenio/ and the harness is
# never found (gd-S0RlCFqN5x).
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../../.." && pwd)"

# Prod beads-viewer ClusterIP service + port. The public ingress host is
# deliberately NOT used (ALB 308-redirects bearer traffic, see header).
: "${BEADS_VIEWER_SVC:=svc/gardenio-beads-viewer}"
: "${BEADS_VIEWER_PORT:=8095}"

mode="dom"
out=""
local_port="$BEADS_VIEWER_PORT"
do_pf=1
path=""
auth_mode="${VIEWER_AUTH_MODE:-sa}"

die() { echo "fetch-beads-viewer-prod: $*" >&2; exit 1; }

usage() { sed -n '2,99p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; }

while [ $# -gt 0 ]; do
  case "$1" in
    --dom)        mode="dom"; shift ;;
    --screenshot) mode="screenshot"; shift ;;
    --out)        out="${2:?--out needs a path}"; shift 2 ;;
    --port)       local_port="${2:?--port needs a value}"; shift 2 ;;
    --no-pf)      do_pf=0; shift ;;
    --auth)       auth_mode="${2:?--auth needs sa|keycloak}"; shift 2 ;;
    --keycloak)   auth_mode="keycloak"; shift ;;
    --sa)         auth_mode="sa"; shift ;;
    # --operator can only be honored on the keycloak path (the SA branch
    # has a fixed gardenio-viewer role floor), so it forces keycloak mode.
    --operator)   KC_CLIENT_ID="gardenio-camp-audit"; export KC_CLIENT_ID; auth_mode="keycloak"; shift ;;
    -h|--help)    usage; exit 0 ;;
    --*)          die "unknown option: $1 (try --help)" ;;
    *)            path="$1"; shift ;;
  esac
done

case "$auth_mode" in
  sa|keycloak) ;;
  *) die "unknown --auth mode: ${auth_mode} (expected sa or keycloak)" ;;
esac

[ -n "$path" ] || die "missing <path> argument (try --help)"
# Normalize leading slash.
case "$path" in /*) ;; *) path="/$path" ;; esac

# Resolve --out to an absolute path now: the screenshot capture runs
# from the harness directory (cd), so a relative --out would otherwise
# land there instead of the caller's cwd.
if [ -n "$out" ]; then
  case "$out" in
    /*) ;;
    *)  out="$(pwd)/$out" ;;
  esac
fi

for bin in curl jq; do
  command -v "$bin" >/dev/null 2>&1 || die "missing required binary: $bin"
done

# 1. Mint the viewer-accepted bearer for the chosen auth mode.
#    POD_NAMESPACE defaults to the downward-API namespace, which in the
#    prod gardenio namespace equals the release name (so the derived
#    Keycloak Secret name resolves correctly, and the port-forward targets
#    the right namespace).
if [ -z "${POD_NAMESPACE:-}" ] && [ -r /var/run/secrets/kubernetes.io/serviceaccount/namespace ]; then
  POD_NAMESPACE="$(cat /var/run/secrets/kubernetes.io/serviceaccount/namespace)"
  export POD_NAMESPACE
fi

if [ "$auth_mode" = "sa" ]; then
  # PROD gardenio-system: audience-pinned projected SA token (k8sSA branch).
  # shellcheck disable=SC1091
  source "${SCRIPT_DIR}/lib-k8s-sa-token.sh"
  # Default the port-forward namespace to the SA's namespace when the
  # downward-API value is unavailable (e.g. running out-of-cluster).
  : "${POD_NAMESPACE:=${SA_TOKEN_NAMESPACE}}"
  export POD_NAMESPACE
  echo "fetch-beads-viewer-prod: minting projected SA token (${SA_TOKEN_NAMESPACE}/${SA_TOKEN_NAME}, aud=${SA_TOKEN_AUDIENCE}, dur=${SA_TOKEN_DURATION})" >&2
  bearer="$(mint_viewer_sa_token)" || die "failed to mint SA token (see stderr above)"
else
  # e2e fallback: Keycloak client_credentials bearer.
  #    KC_RELEASE_NAME defaults to POD_NAMESPACE (the chart names the
  #    KeycloakClient CRD with the release fullname prefix).
  : "${KC_RELEASE_NAME:=${POD_NAMESPACE:-}}"
  export KC_RELEASE_NAME
  # shellcheck disable=SC1091
  source "${SCRIPT_DIR}/lib-keycloak-client-creds.sh"
  echo "fetch-beads-viewer-prod: minting client_credentials bearer (realm=${KC_REALM:-gardenio}, client=${KC_CLIENT_ID:-gardenio-info-density-audit})" >&2
  bearer="$(mint_client_credentials_token)" || die "failed to mint bearer (see stderr above)"
fi
[ -n "$bearer" ] || die "minted bearer was empty"

# 2. Port-forward to the ClusterIP unless told not to.
base_url="${BEADS_VIEWER_URL:-http://localhost:${local_port}}"
pf_pid=""
cleanup() { if [ -n "$pf_pid" ]; then kill "$pf_pid" 2>/dev/null || true; fi; }
trap cleanup EXIT

if [ "$do_pf" -eq 1 ]; then
  command -v kubectl >/dev/null 2>&1 || die "kubectl required for port-forward (use --no-pf if the viewer is already reachable)"
  [ -n "${POD_NAMESPACE:-}" ] || die "POD_NAMESPACE unset; cannot port-forward (set it or use --no-pf)"
  echo "fetch-beads-viewer-prod: port-forwarding ${POD_NAMESPACE}/${BEADS_VIEWER_SVC} ${local_port}:${BEADS_VIEWER_PORT}" >&2
  kubectl -n "$POD_NAMESPACE" port-forward "$BEADS_VIEWER_SVC" \
    "${local_port}:${BEADS_VIEWER_PORT}" >/tmp/beads-viewer-prod-pf.log 2>&1 &
  pf_pid=$!
  base_url="http://localhost:${local_port}"
  # Wait for the health endpoint (authenticated) to answer.
  ready=0
  for _ in $(seq 1 30); do
    if curl -fsS -o /dev/null -H "Authorization: Bearer ${bearer}" \
        "${base_url}/health" 2>/dev/null; then
      ready=1; break
    fi
    sleep 1
  done
  [ "$ready" -eq 1 ] || die "viewer not ready on ${base_url} after 30s (pf log: /tmp/beads-viewer-prod-pf.log)"
fi

url="${base_url}${path}"

case "$mode" in
  dom)
    echo "fetch-beads-viewer-prod: GET ${url}" >&2
    if [ -n "$out" ]; then
      curl -fsS -H "Authorization: Bearer ${bearer}" "$url" -o "$out" \
        || die "fetch failed for ${url}"
      echo "fetch-beads-viewer-prod: wrote DOM to ${out}" >&2
    else
      curl -fsS -H "Authorization: Bearer ${bearer}" "$url" \
        || die "fetch failed for ${url}"
    fi
    ;;
  screenshot)
    [ -n "$out" ] || die "--screenshot requires --out <file.png>"
    harness="${REPO_ROOT}/gardenio/tests/beads-viewer-screenshots"
    capture="${harness}/tools/capture-page.mjs"
    [ -f "$capture" ] || die "screenshot harness not found at ${capture}"
    command -v node >/dev/null 2>&1 || die "--screenshot needs node + the Playwright harness deps (npm ci in ${harness})"
    [ -d "${harness}/node_modules/@playwright" ] || die "Playwright not installed; run: (cd ${harness} && npm ci && npx playwright install chromium)"
    echo "fetch-beads-viewer-prod: capturing screenshot of ${url} -> ${out}" >&2
    # Single-page capture via tools/capture-page.mjs, which attaches the
    # bearer as an Authorization extraHTTPHeader (the built-in
    # `playwright screenshot` CLI cannot set custom headers).
    ( cd "$harness" && BEADS_VIEWER_BEARER="$bearer" \
        node tools/capture-page.mjs "$url" "$out" ) \
      || die "screenshot capture failed (deps installed? see ${harness}/README.md)"
    echo "fetch-beads-viewer-prod: wrote screenshot to ${out}" >&2
    ;;
  *) die "unknown mode: ${mode}" ;;
esac
