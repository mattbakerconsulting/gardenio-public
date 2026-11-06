#!/usr/bin/env bash
# One-click Become (plan gd-0SUIdKXWV3, step gd-rbML2txvGG) against the harness
# Keycloak.
#
#   ./become-client.sh <container-name>
#
# 1. takes the gardenio-become client from deploy-values/gardenio-k3s.yaml,
#    helm-templates the chart's first-admin Job with it, and extracts the text
#    between the `# --- scope-mappings:begin` / `# --- scope-mappings:end`
#    marker comments (the hook's own bytes, not a copy);
# 2. creates the client the way the operator would (public, fullScopeAllowed
#    false), runs the block twice with kcadm (idempotence), and reads the
#    client's realm-management scope mappings back;
# 3. creates a probe client carrying the overlay forwardauth client's two
#    impersonator mappers, impersonates a probe user through the admin API,
#    walks the browser code flow on the impersonated session with curl, and
#    asserts the access and ID tokens carry impersonator / impersonator_id;
#    a password sign-in of the same user (control) must carry neither.
#
# Throwaway credentials only; the container is deleted by run.sh.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/../../.." && pwd)"
CHART="$REPO/gardenio/helm/gardenio"
OVERLAY="$REPO/deploy-values/gardenio-k3s.yaml"
NAME="${1:?usage: become-client.sh <container-name>}"
REALM=gardenio
BASE="http://localhost:${KC_HTTP_PORT:-8080}/auth"
: "${KC_ADMIN_USER:?}" "${KC_ADMIN_PASSWORD:?}"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# --- 1: render the Job with the overlay's client, extract the block ----------
yq -o=json '{"keycloak": {"clients": [.keycloak.clients[] | select(.clientId == "gardenio-become")]}}' \
  "$OVERLAY" > "$WORK/become-values.json"
[ "$(jq '.keycloak.clients | length' "$WORK/become-values.json")" = 1 ] \
  || { echo "FAIL: deploy-values has no gardenio-become client" >&2; exit 1; }
helm template t "$CHART" -n gardenio \
  --show-only templates/keycloak-runtime/first-admin-job.yaml \
  --set postgres.enabled=true --set keycloak.enabled=true \
  --set keycloak.runtime.enabled=true --set keycloak.runtime.db.autoWire=true \
  --set keycloak.runtime.host=kc.svc.cluster.local \
  -f "$WORK/become-values.json" > "$WORK/job.yaml"
sed -n '/# --- scope-mappings:begin/,/# --- scope-mappings:end/p' "$WORK/job.yaml" \
  | sed 's/^ \{14\}//' > "$WORK/block.sh"
[ "$(wc -l < "$WORK/block.sh")" -gt 5 ] \
  || { echo "scope-mappings block not found in the rendered Job" >&2; cat "$WORK/job.yaml" >&2; exit 1; }
bash -n "$WORK/block.sh"
echo "== scope-mappings hook block: $(wc -l < "$WORK/block.sh") lines extracted from the rendered first-admin Job"

# --- 2: create the client, run the block twice, read the mappings back -------
cat > "$WORK/prelude.sh" <<'PRELUDE'
KC_REALM=gardenio
KCADM_CONFIG=/tmp/kcadm.config
KCADM_ERR=/tmp/kcadm.err
kcadm() { /opt/keycloak/bin/kcadm.sh "$@" --config "$KCADM_CONFIG"; }
PRELUDE

# As in passkey-policy.sh: the script goes in as a file, not on stdin.
kc_exec() {
  cat > "$WORK/exec.sh"
  docker cp "$WORK/exec.sh" "$NAME:/tmp/become-exec.sh"
  docker exec -e KC_ADMIN_USER -e KC_ADMIN_PASSWORD "$NAME" bash /tmp/become-exec.sh
}

{ cat "$WORK/prelude.sh"; cat <<'SETUP'
kcadm config credentials --server http://localhost:8080/auth --realm master \
  --user "$KC_ADMIN_USER" --password "$KC_ADMIN_PASSWORD" > /dev/null
kcadm create clients -r "$KC_REALM" -s clientId=gardenio-become -s publicClient=true \
  -s standardFlowEnabled=true -s directAccessGrantsEnabled=false -s fullScopeAllowed=false \
  -s 'redirectUris=["https://auth.clonk.tv/gardenio/become/*"]' > /dev/null
SETUP
} | kc_exec

for pass in 1 2; do
  out="$({ cat "$WORK/prelude.sh"; cat "$WORK/block.sh"; } | kc_exec 2>&1)"
  printf '%s\n' "$out" | sed "s/^/   [run $pass] /"
  case "$out" in *"WARNING:"*) echo "FAIL: the hook block warned on run $pass" >&2; exit 1 ;; esac
  if [ "$pass" = 1 ]; then want="scope mappings for realm-management added"; else want="already present"; fi
  case "$out" in *"$want"*) ;; *) echo "FAIL: run $pass did not report '$want'" >&2; exit 1 ;; esac
done

got="$({ cat "$WORK/prelude.sh"; cat <<'READ'
CID=$(kcadm get clients -r "$KC_REALM" -q clientId=gardenio-become --fields id --format csv --noquotes)
RMID=$(kcadm get clients -r "$KC_REALM" -q clientId=realm-management --fields id --format csv --noquotes)
kcadm get "clients/$CID/scope-mappings/clients/$RMID" -r "$KC_REALM" --fields name --format csv --noquotes
READ
} | kc_exec | sort | tr '\n' ' ')"
if [ "$got" != "impersonation query-users view-users " ]; then
  echo "FAIL: gardenio-become realm-management scope = '$got', want 'impersonation query-users view-users'" >&2
  exit 1
fi
echo "   ok   gardenio-become realm-management scope: $got"

# --- 3: an impersonated session's tokens carry the impersonator --------------
# The probe client carries the overlay forwardauth client's impersonator
# mappers, byte for byte.
yq -o=json '[.keycloak.clients[] | select(.clientId == "gardenio-traefik-forwardauth")
             | .protocolMappers[] | select(.protocolMapper == "oidc-usersessionmodel-note-mapper")]' \
  "$OVERLAY" > "$WORK/mappers.json"
[ "$(jq length "$WORK/mappers.json")" = 2 ] \
  || { echo "FAIL: overlay forwardauth client should carry 2 session-note mappers" >&2; cat "$WORK/mappers.json" >&2; exit 1; }
jq '{clientId: "become-claim-probe", publicClient: true, standardFlowEnabled: true,
     directAccessGrantsEnabled: true, redirectUris: ["http://localhost/cb"],
     protocolMappers: .}' "$WORK/mappers.json" > "$WORK/probe-client.json"
docker cp "$WORK/probe-client.json" "$NAME:/tmp/become-probe-client.json"
{ cat "$WORK/prelude.sh"; cat <<'PROBE'
kcadm create clients -r "$KC_REALM" -f /tmp/become-probe-client.json > /dev/null
kcadm create users -r "$KC_REALM" -s username=become-tenant-probe -s enabled=true \
  -s email=become-tenant-probe@example.invalid -s emailVerified=true \
  -s firstName=Become -s lastName=Probe > /dev/null
kcadm set-password -r "$KC_REALM" --username become-tenant-probe --new-password 'throwaway-Become-1'
PROBE
} | kc_exec

admin_token="$(curl -fsS "$BASE/realms/master/protocol/openid-connect/token" \
  -d grant_type=password -d client_id=admin-cli \
  --data-urlencode "username=$KC_ADMIN_USER" --data-urlencode "password=$KC_ADMIN_PASSWORD" | jq -r .access_token)"
uid="$(curl -fsS -H "Authorization: Bearer $admin_token" \
  "$BASE/admin/realms/$REALM/users?username=become-tenant-probe&exact=true" | jq -r '.[0].id')"

# Impersonation answers with the target realm's session cookies.
jar="$WORK/cookies"
curl -fsS -c "$jar" -X POST -H "Authorization: Bearer $admin_token" \
  "$BASE/admin/realms/$REALM/users/$uid/impersonation" > /dev/null
loc="$(curl -sS -b "$jar" -c "$jar" -o /dev/null -w '%{redirect_url}' \
  "$BASE/realms/$REALM/protocol/openid-connect/auth?client_id=become-claim-probe&response_type=code&scope=openid&redirect_uri=http%3A%2F%2Flocalhost%2Fcb")"
code="$(printf '%s' "$loc" | sed -n 's/.*[?&]code=\([^&]*\).*/\1/p')"
[ -n "$code" ] || { echo "FAIL: the impersonated session got no auth code (redirect: '$loc')" >&2; exit 1; }
tokens="$(curl -fsS "$BASE/realms/$REALM/protocol/openid-connect/token" \
  -d grant_type=authorization_code -d client_id=become-claim-probe \
  -d redirect_uri=http://localhost/cb --data-urlencode "code=$code")"

claims() { # <jwt>: its payload as JSON
  local p
  p="$(printf '%s' "$1" | cut -d. -f2 | tr '_-' '/+')"
  while [ $(( ${#p} % 4 )) -ne 0 ]; do p="$p="; done
  printf '%s' "$p" | base64 -d
}
fail=0
for tok in access_token id_token; do
  c="$(claims "$(jq -r ".$tok" <<<"$tokens")")"
  imp="$(jq -r '.impersonator // ""' <<<"$c")"
  impid="$(jq -r '.impersonator_id // ""' <<<"$c")"
  sub="$(jq -r '.preferred_username // ""' <<<"$c")"
  if [ "$imp" = "$KC_ADMIN_USER" ] && [ -n "$impid" ] && [ "$sub" = become-tenant-probe ]; then
    echo "   ok   impersonated $tok: preferred_username=$sub impersonator=$imp impersonator_id=$impid"
  else
    echo "   FAIL impersonated $tok: preferred_username=$sub impersonator='$imp' impersonator_id='$impid'" >&2
    fail=1
  fi
done

# Control: the same user's own sign-in carries neither claim.
own="$(curl -fsS "$BASE/realms/$REALM/protocol/openid-connect/token" \
  -d grant_type=password -d client_id=become-claim-probe -d scope=openid \
  -d username=become-tenant-probe --data-urlencode 'password=throwaway-Become-1')"
for tok in access_token id_token; do
  c="$(claims "$(jq -r ".$tok" <<<"$own")")"
  if jq -e 'has("impersonator") or has("impersonator_id")' <<<"$c" > /dev/null; then
    echo "   FAIL own-session $tok carries an impersonator claim" >&2
    fail=1
  else
    echo "   ok   own-session $tok: no impersonator claim"
  fi
done
[ "$fail" = 0 ] || { echo "impersonator claim check failed" >&2; exit 1; }
echo "== become: scope mappings applied twice and read back; impersonated tokens carry impersonator, own tokens do not"
