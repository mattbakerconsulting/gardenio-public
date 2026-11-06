#!/usr/bin/env bash
# Keycloak passkey runbook verification (gd-diK82eIdBJ, plan gd-QXq1fjYuRD S3b).
#
# Runs the recipes in gardenio/docs/keycloak-passkeys-runbook.md against a
# throwaway Keycloak (start-dev, same relative path and bootstrap-admin env as the
# StatefulSet), INSIDE the container the way the runbook runs them inside the pod:
# kcadm reads KC_BOOTSTRAP_ADMIN_* from the container environment. It captures
# `kcadm.sh <cmd> --help` for every subcommand the runbook uses and exercises each
# recipe, so a wrong flag fails here instead of during a lockout.
#
# Usage: run.sh <keycloak-tag>
# Needs: docker, curl, jq on the runner. Output: $OUT (default ./runbook-out).
# Contains no secrets: every credential here is a throwaway for a container that
# lives for this job only.
set -uo pipefail

TAG="${1:?usage: run.sh <keycloak-tag>}"
IMAGE="${KC_IMAGE_REPO:-quay.io/keycloak/keycloak}:$TAG"
OUT="${OUT:-$PWD/runbook-out}"
NAME=kc-runbook
REALM=gardenio
EMAIL=owner@example.invalid
ADMIN_USER=runbook-admin
ADMIN_PASS="runbook-$(date +%s)-throwaway"
mkdir -p "$OUT"

FAILS=0
SUMMARY="$OUT/summary.md"
: >"$SUMMARY"
say() { echo "$*" | tee -a "$SUMMARY"; }
pass() { say "- PASS  $*"; }
info() { say "- INFO  $*"; }
fail() { say "- FAIL  $*"; FAILS=$((FAILS + 1)); }

docker rm -f "$NAME" >/dev/null 2>&1 || true
docker run -d --name "$NAME" -p 8080:8080 \
  -e KC_BOOTSTRAP_ADMIN_USERNAME="$ADMIN_USER" -e KC_BOOTSTRAP_ADMIN_PASSWORD="$ADMIN_PASS" \
  -e KC_HTTP_RELATIVE_PATH=/auth -e KC_HTTP_ENABLED=true -e KC_HOSTNAME_STRICT=false \
  "$IMAGE" start-dev >/dev/null
say "## Keycloak $TAG runbook verification"
for _ in $(seq 1 90); do
  [ "$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8080/auth/realms/master)" = 200 ] && break
  sleep 2
done
curl -sf -o /dev/null http://127.0.0.1:8080/auth/realms/master || { fail "Keycloak $TAG never became ready"; docker logs --tail 60 "$NAME" >"$OUT/server-fail.log" 2>&1; exit 1; }
pass "Keycloak $TAG up (start-dev, KC_HTTP_RELATIVE_PATH=/auth)"

# Everything the runbook does in the pod, the harness does with `docker exec`.
pod() { docker exec -i "$NAME" bash -c "$1"; }
PRELUDE='K=/opt/keycloak/bin/kcadm.sh; C="--config /tmp/kcadm.config"; kc() { $K "$@" $C; }'

# --- 1. what the pod shell has ---------------------------------------------
tools=$(pod 'for t in awk jq python3 sed grep cut tr head tail sort uniq wc tee unzip curl script; do command -v $t >/dev/null 2>&1 && echo "have:$t" || echo "lacks:$t"; done' | tr '\n' ' ')
info "tools in the Keycloak container: $tools"
echo "$tools" >"$OUT/tools.txt"

# --- 2. --help for each subcommand the runbook uses -----------------------
for sub in "config credentials" "get" "create" "update" "delete" "set-password"; do
  f="$OUT/help-$(echo "$sub" | tr ' ' '-').txt"
  pod "/opt/keycloak/bin/kcadm.sh $sub --help" >"$f" 2>&1 || true
  info "help '$sub': $(wc -l <"$f") lines (help-$(echo "$sub" | tr ' ' '-').txt)"
done
need() { # <help-file-suffix> <flag>
  if grep -q -- "$2" "$OUT/help-$1.txt"; then pass "kcadm $1 documents $2"; else fail "kcadm $1 does NOT document $2"; fi
}
need config-credentials --server; need config-credentials --realm; need config-credentials --user; need config-credentials --password
need get --fields; need get --format; need get --noquotes; need get -q
need update -s; need set-password --username; need set-password --temporary; need set-password --new-password
need delete -r; need create -s

# --- 3. seed (REST, throwaway) --------------------------------------------
TOKEN=$(curl -s -d client_id=admin-cli -d grant_type=password -d username="$ADMIN_USER" -d password="$ADMIN_PASS" \
  http://127.0.0.1:8080/auth/realms/master/protocol/openid-connect/token | jq -r .access_token)
[ -n "$TOKEN" ] && [ "$TOKEN" != null ] || { fail "seed: no admin token"; exit 1; }
API=http://127.0.0.1:8080/auth/admin/realms
rest() { curl -s -o "$OUT/last.json" -w '%{http_code}' -X "$1" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' ${3:+-d "$3"} "$API$2"; }
rest POST "" '{"realm":"'$REALM'","enabled":true,"loginWithEmailAllowed":true}' >/dev/null
rest POST /$REALM/authentication/flows/browser/copy '{"newName":"gardenio-mfa-browser"}' >/dev/null
rest PUT /$REALM '{"realm":"'$REALM'","browserFlow":"gardenio-mfa-browser"}' >/dev/null
# A decoy whose username contains the owner's as a substring: `get users -q username=`
# is a substring search, so the runbook must filter to the exact row.
rest POST /$REALM/users '{"username":"x'$EMAIL'","email":"x'$EMAIL'","enabled":true}' >/dev/null
# The owner, with a password and TWO faux passkeys: the same credential rows an
# enrolled WebAuthn Passwordless authenticator leaves (type + label). The admin API
# will not mint a real one without a browser ceremony, so try the import shapes.
mkuser() { # <label> <credentials-json>
  jq -nc --arg e "$EMAIL" --argjson c "$2" '{username:$e,email:$e,enabled:true,emailVerified:true,credentials:$c}' >"$OUT/user.json"
  local code; code=$(rest POST /$REALM/users "$(cat "$OUT/user.json")")
  info "seed variant $1: HTTP $code $(head -c 160 "$OUT/last.json")"
  [ "$code" = 201 ]
}
PW='{"type":"password","value":"seed-throwaway-1","temporary":false}'
PK1='{"type":"webauthn-passwordless","userLabel":"laptop","secretData":"{}","credentialData":"{}"}'
PK2='{"type":"webauthn-passwordless","userLabel":"phone","secretData":"{}","credentialData":"{}"}'
PK1O='{"type":"webauthn-passwordless","userLabel":"laptop","secretData":{},"credentialData":{}}'
PK2O='{"type":"webauthn-passwordless","userLabel":"phone","secretData":{},"credentialData":{}}'
if mkuser strings "[$PW,$PK1,$PK2]"; then FAUX=webauthn-passwordless
else
  U1=$(curl -s -H "Authorization: Bearer $TOKEN" "$API/$REALM/users?username=$EMAIL&exact=true" | jq -r '.[0].id // empty')
  if [ -z "$U1" ] && mkuser objects "[$PW,$PK1O,$PK2O]"; then FAUX=webauthn-passwordless
  else
    U1=$(curl -s -H "Authorization: Bearer $TOKEN" "$API/$REALM/users?username=$EMAIL&exact=true" | jq -r '.[0].id // empty')
    [ -z "$U1" ] && mkuser password-only "[$PW]"
    FAUX=none
  fi
fi
if [ "$FAUX" = none ]; then
  fail "could not seed faux passkey credentials; the delete-by-type loop below runs against type 'password' instead"
else
  pass "seeded owner with a password and 2 faux $FAUX credentials"
fi

# --- 4. run the runbook's own blocks ---------------------------------------
# The recipes are pulled out of the markdown (fenced blocks tagged "bash <name>"),
# so the doc cannot drift from what CI ran. They run inside the container, with the
# bootstrap admin only in the container environment, as in the pod.
DOC="${DOC:-$(dirname "$0")/../../gardenio/docs/keycloak-passkeys-runbook.md}"
block() { awk -v t="$1" '$0 == "```bash " t {f=1; next} /^```$/ {f=0} f' "$DOC"; }
for b in prelude drill lost-all flow-stock flow-restore; do
  [ -n "$(block $b)" ] && pass "runbook has a '$b' block" || fail "runbook has no '$b' block"
done
# A second user whose name merely STARTS with the owner's: `uid` must not pick it.
rest POST /$REALM/users '{"username":"'$EMAIL'.au","email":"'$EMAIL'.au","enabled":true}' >/dev/null
tr_() { sed 's/^/    /'; }

say ""; say "### drill (before)"
{ block prelude; block drill; echo "OWNER=$EMAIL; drill"; } >"$OUT/script.sh"
docker exec -i "$NAME" bash <"$OUT/script.sh" >"$OUT/drill-before.txt" 2>&1; tr_ <"$OUT/drill-before.txt" >>"$SUMMARY"
grep -qx 'authenticated: yes' "$OUT/drill-before.txt" && pass "drill: authenticated: yes" || fail "drill did not authenticate"
grep -qx 'owner credential types: password,webauthn-passwordless,webauthn-passwordless' "$OUT/drill-before.txt" && pass "drill: owner credential types line (exact owner row, decoys ignored)" || fail "drill types line wrong: $(grep 'credential types' "$OUT/drill-before.txt")"
grep -qx 'browserFlow: gardenio-mfa-browser' "$OUT/drill-before.txt" && pass "drill: browserFlow line" || fail "drill browserFlow line wrong"
[ "$(grep -c . "$OUT/drill-before.txt")" -le 4 ] && pass "drill prints three report lines plus the terminal-only labels line" || fail "drill printed more than expected"
grep -qE '[0-9a-f]{8}-[0-9a-f]{4}-' "$OUT/drill-before.txt" && fail "drill output contains something shaped like an id" || pass "drill output carries no id"

say ""; say "### lost-all-passkeys"
{ block prelude; block lost-all; echo "OWNER=$EMAIL; lostall"; } >"$OUT/script.sh"
docker exec -i -e KC_CLI_PASSWORD='Throwaway-reset-1' "$NAME" bash <"$OUT/script.sh" >"$OUT/lostall.txt" 2>&1; tr_ <"$OUT/lostall.txt" >>"$SUMMARY"
[ "$(grep -c '^removed one webauthn-passwordless' "$OUT/lostall.txt")" = 2 ] && pass "lost-all: removed both passkeys" || fail "lost-all: did not remove exactly two passkeys"
grep -qx 'credential types now: password' "$OUT/lostall.txt" && pass "lost-all: only the password credential remains" || fail "lost-all: credential types after: $(grep 'types now' "$OUT/lostall.txt")"
grep -q '^required actions now: .*UPDATE_PASSWORD' "$OUT/lostall.txt" && pass "lost-all: UPDATE_PASSWORD is pending (temporary password)" || fail "lost-all: no UPDATE_PASSWORD required action"
grep -qE '[0-9a-f]{8}-[0-9a-f]{4}-' "$OUT/lostall.txt" && fail "lost-all output contains something shaped like an id" || pass "lost-all output carries no id"
# the decoy must be untouched, and the password the recipe set must actually work
dec=$(curl -s -H "Authorization: Bearer $TOKEN" "$API/$REALM/users?username=$EMAIL.au&exact=true" | jq -r '.[0].requiredActions|length')
[ "$dec" = 0 ] && pass "lost-all: the look-alike user ($EMAIL.au) was not touched" || fail "lost-all: the look-alike user got required actions"
gc=$(curl -s -o "$OUT/grant.json" -w '%{http_code}' -d client_id=admin-cli -d grant_type=password -d username="$EMAIL" -d password='Throwaway-reset-1' http://127.0.0.1:8080/auth/realms/$REALM/protocol/openid-connect/token)
info "password grant with the reset password: HTTP $gc ($(jq -r '.error_description // .error // "token"' "$OUT/grant.json")) - a temporary password is refused by the direct grant until changed, which is the expected non-200"

say ""; say "### drill (after)"
{ block prelude; block drill; echo "OWNER=$EMAIL; drill"; } >"$OUT/script.sh"
docker exec -i "$NAME" bash <"$OUT/script.sh" >"$OUT/drill-after.txt" 2>&1; tr_ <"$OUT/drill-after.txt" >>"$SUMMARY"
grep -qx 'owner credential types: password' "$OUT/drill-after.txt" && pass "drill after: types are password only" || fail "drill after: types wrong"

say ""; say "### break-glass flow switch"
# flow-stock defines flowstock(); flow-restore runs inline and needs a login first
{ block prelude; block flow-stock; block flow-restore; echo 'rm -f /tmp/kcadm.config'; } >"$OUT/script.sh"
docker exec -i "$NAME" bash <"$OUT/script.sh" >"$OUT/flow.txt" 2>&1; tr_ <"$OUT/flow.txt" >>"$SUMMARY"
[ "$(grep -c '^browserFlow now: browser$' "$OUT/flow.txt")" = 1 ] && pass "flow: realm switched to the stock browser flow" || fail "flow: stock switch not confirmed"
[ "$(grep -c '^browserFlow now: gardenio-mfa-browser$' "$OUT/flow.txt")" = 1 ] && pass "flow: restored to gardenio-mfa-browser" || fail "flow: restore not confirmed"
[ ! -e /dev/null ] || docker exec "$NAME" test ! -e /tmp/kcadm.config && pass "config file with the session token is removed" || fail "kcadm.config left behind"

# --- 5. interactive password prompt (needs a TTY) --------------------------
if command -v script >/dev/null 2>&1; then
  printf 'Prompted-reset-1\nPrompted-reset-1\n' | script -qec "docker exec -it $NAME bash -c '$PRELUDE; kc config credentials --server http://localhost:8080/auth --realm master --user \"\$KC_BOOTSTRAP_ADMIN_USERNAME\" --password \"\$KC_BOOTSTRAP_ADMIN_PASSWORD\" >/dev/null; kc set-password -r gardenio --username $EMAIL --temporary; echo \"prompt rc=\$?\"'" /dev/null >"$OUT/prompt.txt" 2>&1
  info "set-password prompt form (TTY): $(tr -d '\r' <"$OUT/prompt.txt" | head -c 400 | tr '\n' '|')"
else
  info "no 'script' on the runner: prompt form not exercised"
fi

# --- 6. wording of the account console and login page ----------------------
mkdir -p "$OUT/jars"
docker cp "$NAME:/opt/keycloak/lib/lib/main/." "$OUT/jars" >/dev/null 2>&1 || true
: >"$OUT/messages-passkey.txt"
for j in "$OUT"/jars/*.jar; do
  for f in $(unzip -Z1 "$j" 2>/dev/null | grep -E '(messages_en\.properties|en/translation\.json|messages/en\.json)$'); do
    unzip -p "$j" "$f" 2>/dev/null | grep -i -E 'passkey|passwordless|webauthn' | sed "s|^|[$(basename "$j")!$f] |" >>"$OUT/messages-passkey.txt"
  done
done
info "passkey wording: $(wc -l <"$OUT/messages-passkey.txt") lines in messages-passkey.txt"
rm -rf "$OUT/jars"

mkdir -p "$OUT/jars"
docker cp "$NAME:/opt/keycloak/lib/lib/main/." "$OUT/jars" >/dev/null 2>&1 || true
for j in "$OUT"/jars/org.keycloak.keycloak-account-ui-*.jar; do
  unzip -p "$j" theme/keycloak.v3/account/messages/messages_en.properties >"$OUT/account-messages_en.properties" 2>/dev/null
done
rm -rf "$OUT/jars"
info "account-ui messages saved: $(wc -l <"$OUT/account-messages_en.properties" 2>/dev/null || echo 0) lines"

# --- 7. can the master console be reached through a port-forward? ----------
# The StatefulSet sets KC_HOSTNAME to the public URL, so the console may point
# browsers at the edge (which 404s /auth/admin and realms/master). Measure it with
# the live hostname env, with and without KC_HOSTNAME_ADMIN.
console_probe() { # <label> <extra docker args...>
  local label="$1"; shift
  docker rm -f kc-live >/dev/null 2>&1 || true
  docker run -d --name kc-live -p 8081:8080 \
    -e KC_BOOTSTRAP_ADMIN_USERNAME=live-admin -e KC_BOOTSTRAP_ADMIN_PASSWORD=live-throwaway \
    -e KC_HOSTNAME=https://auth.clonk.tv/auth -e KC_HOSTNAME_BACKCHANNEL_DYNAMIC=true -e KC_HOSTNAME_STRICT=false \
    -e KC_HTTP_ENABLED=true -e KC_HTTP_RELATIVE_PATH=/auth -e KC_PROXY_HEADERS=xforwarded "$@" \
    "$IMAGE" start-dev >/dev/null
  for _ in $(seq 1 90); do
    [ "$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8081/auth/realms/master)" = 200 ] && break
    sleep 2
  done
  local page="$OUT/console-$label.html"
  curl -s -L -o "$page" -w '' http://127.0.0.1:8081/auth/admin/master/console/ || true
  info "console probe [$label]: GET /auth/admin/master/console/ -> $(curl -s -o /dev/null -w '%{http_code} redirect=%{redirect_url}' http://127.0.0.1:8081/auth/admin/master/console/)"
  info "console probe [$label]: page config: $(sed 's/&quot;/"/g' "$page" | grep -o '"\(authUrl\|authServerUrl\|serverBaseUrl\|adminBaseUrl\|consoleBaseUrl\|resourceUrl\|realm\)" *: *"[^"]*"' | sort -u | tr '\n' ' ') (page is $(wc -c <"$page") bytes)"
  info "console probe [$label]: master OIDC issuer via localhost: $(curl -s http://127.0.0.1:8081/auth/realms/master/.well-known/openid-configuration | jq -r '.issuer + "  authorization_endpoint=" + .authorization_endpoint')"
  docker rm -f kc-live >/dev/null 2>&1 || true
}
console_probe live-env
console_probe with-hostname-admin -e KC_HOSTNAME_ADMIN=http://localhost:8081/auth

docker logs --tail 40 "$NAME" >"$OUT/server-tail.log" 2>&1
docker rm -f "$NAME" >/dev/null 2>&1 || true
say ""
say "failures: $FAILS"
[ "$FAILS" -eq 0 ]
