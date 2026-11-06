#!/usr/bin/env bash
# land-door-verify.sh — the land-door /v1/windows checks (gd-9w3Lbt8g2D), run
# by a crew as the test owner of a land (gd-Hq1PnUVOUC, owner ruling on
# gd-ER4PMJ3vcP).
#
# It signs the land's test owner in with the password grant against the
# in-cluster Keycloak (client gardenio-land-verifier, scope land-<land>), then
# sends the four requests to the land's landgate Service, which verifies the
# bearer as the edge's land door does and forwards to kbeads:
#
#   1 list          GET  /v1/windows?land=<land>        want 200
#   2 preview       GET  /v1/windows/preview?<target>   want 200
#   3 POST preview  POST /v1/windows/preview?<target>   want 404
#   4 self/view     GET  /v1/windows/self/view          want 404
#
# <target> is plan=<LAND_VERIFIER_PLAN> when set (a plan epic labelled
# project:<land>), else land=<land>. The edge itself is not on this path:
# crew pods are fenced off it (landgate.crewDeny); landgate.verifier admits
# them to this land's landgate only.
#
# Exit status:
#   0  all four answered as wanted
#   1  at least one did not (the lines say which)
#   2  inconclusive: a setting is missing or the sign-in failed
#
# Secrets handling: the password goes to Keycloak with --data-urlencode
# name@file and the bearer to landgate with -H @file, so neither is in argv,
# stdout or stderr. Response bodies are never printed; only statuses are.
#
# Environment:
#   LAND_VERIFIER_USERNAME  the test owner (role bead secret crew-land-verifier)
#   LAND_VERIFIER_PASSWORD  its password (same Secret)
#   LAND                    default scopetest
#   LAND_VERIFIER_PLAN      optional plan id in that land, for check 2 and 3
#   LANDGATE_URL            default http://gardenio-landgate-<land>.gardenio.svc.cluster.local:8080
#   KEYCLOAK_TOKEN_URL      default the in-cluster realm gardenio token endpoint
#   LAND_VERIFIER_CLIENT    default gardenio-land-verifier
#
# Unit tests: gardenio/scripts/land-door-verify_test.sh.
set -euo pipefail
set +x

LAND="${LAND:-scopetest}"
LANDGATE_URL="${LANDGATE_URL:-http://gardenio-landgate-${LAND}.gardenio.svc.cluster.local:8080}"
KEYCLOAK_TOKEN_URL="${KEYCLOAK_TOKEN_URL:-http://gardenio-keycloak-runtime.gardenio.svc.cluster.local:8080/auth/realms/gardenio/protocol/openid-connect/token}"
CLIENT_ID="${LAND_VERIFIER_CLIENT:-gardenio-land-verifier}"

inconclusive() { echo "land-door-verify: INCONCLUSIVE: $*" >&2; exit 2; }

for v in LAND_VERIFIER_USERNAME LAND_VERIFIER_PASSWORD; do
  [ -n "${!v:-}" ] || inconclusive "$v is not set (crew role bead secret crew-land-verifier)"
done
[[ "$LAND" =~ ^[a-z][a-z0-9-]{0,62}$ ]] || inconclusive "LAND is not a land name: $LAND"
for bin in curl jq; do
  command -v "$bin" >/dev/null 2>&1 || inconclusive "$bin is not installed"
done

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
umask 077
printf '%s' "$LAND_VERIFIER_PASSWORD" >"$TMP/password"

code="$(curl -sS -m 15 -o "$TMP/token.json" -w '%{http_code}' \
  --data-urlencode grant_type=password \
  --data-urlencode "client_id=${CLIENT_ID}" \
  --data-urlencode "username=${LAND_VERIFIER_USERNAME}" \
  --data-urlencode "password@$TMP/password" \
  --data-urlencode "scope=openid land-${LAND}" \
  "$KEYCLOAK_TOKEN_URL" 2>/dev/null)" || inconclusive "token endpoint unreachable: $KEYCLOAK_TOKEN_URL"
rm -f "$TMP/password"
[ "$code" = 200 ] || inconclusive "sign-in answered HTTP $code (want 200)"
token="$(jq -r '.access_token // empty' "$TMP/token.json" 2>/dev/null)" || true
rm -f "$TMP/token.json"
[ -n "$token" ] || inconclusive "sign-in returned no access_token"
printf 'Authorization: Bearer %s\n' "$token" >"$TMP/auth"
unset token

if [ -n "${LAND_VERIFIER_PLAN:-}" ]; then
  target="plan=${LAND_VERIFIER_PLAN}"
else
  target="land=${LAND}"
fi
E="${LANDGATE_URL%/}/v1"

failed=0
check() { # check <label> <want> <curl args...>
  local label="$1" want="$2" got
  shift 2
  got="$(curl -sS -m 15 -o /dev/null -w '%{http_code}' -H "@$TMP/auth" "$@" 2>/dev/null)" || got="${got:-000}"
  echo "$label $got (want $want)"
  [ "$got" = "$want" ] || failed=1
}

check "1 list        " 200 "$E/windows?land=${LAND}"
check "2 preview     " 200 "$E/windows/preview?${target}"
check "3 POST preview" 404 -X POST "$E/windows/preview?${target}"
check "4 self/view   " 404 "$E/windows/self/view"

exit "$failed"
