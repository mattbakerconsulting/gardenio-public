#!/usr/bin/env bash
# Keycloak upgrade preflight (gd-s47ivaszhW, design doc "Upgrade preflight").
#
# Rolls a Postgres-backed Keycloak from the version the cluster runs today to a
# candidate, with the same env the chart's StatefulSet sets
# (templates/keycloak-runtime/statefulset.yaml), and checks what the owner would
# otherwise find out live:
#   1. the schema migration of a seeded 26.0.7 database to the candidate;
#   2. the three StatefulSet probes on the management port;
#   3. the issuer / backchannel hostname behaviour (KC_HOSTNAME full URL +
#      hostname-backchannel-dynamic + xforwarded);
#   4. every kcadm call templates/keycloak-runtime/first-admin-job.yaml makes;
#   5. a custom browser flow survives the migration unchanged (no Organization
#      sub-flow appears in it);
#   6. a client with client-session timeouts is re-accepted (26.5 validation);
#   7. the device-code endpoint `kd login` uses, the login page, metrics;
#   8. the WebAuthn Passwordless realm attributes phase 3 (S3a) will set;
#   9. that the old image will NOT start on the migrated database (rollback
#      needs the pg_dump restore, not just the old tag).
#
# Usage: run.sh <candidate-tag> [baseline-tag]
# Needs: docker, curl, jq, a Postgres on 127.0.0.1:5432 with an empty database
# `keycloak` (user/password via PGUSER/PGPASSWORD, default keycloak/keycloak).
# Output: $OUT (default ./preflight-out) holds one file per check; exit 1 when a
# hard check fails. Contains no secrets: the admin and user passwords below are
# throwaway values for a container that lives for this job only.
set -uo pipefail

TARGET="${1:?usage: run.sh <candidate-tag> [baseline-tag]}"
BASELINE="${2:-26.0.7}"
IMAGE_REPO="${KC_IMAGE_REPO:-quay.io/keycloak/keycloak}"
OUT="${OUT:-$PWD/preflight-out}"
PGUSER="${PGUSER:-keycloak}"
PGPASSWORD="${PGPASSWORD:-keycloak}"
ADMIN_USER=preflight-admin
ADMIN_PASS="preflight-$(date +%s)-throwaway"
FRONT=https://auth.clonk.tv/auth
REALM=gardenio
NAME=kc-preflight
mkdir -p "$OUT"

FAILS=0
SUMMARY="$OUT/summary.md"
: >"$SUMMARY"
say() { echo "$*" | tee -a "$SUMMARY"; }
pass() { say "- PASS  $*"; }
info() { say "- INFO  $*"; }
fail() { say "- FAIL  $*"; FAILS=$((FAILS + 1)); }

start_kc() { # <tag>
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  # Same env the StatefulSet sets. --network host so the Postgres service
  # container's published port is reachable as 127.0.0.1:5432.
  docker run -d --name "$NAME" --network host \
    -e KC_DB=postgres \
    -e KC_DB_URL="jdbc:postgresql://127.0.0.1:5432/keycloak" \
    -e KC_DB_USERNAME="$PGUSER" -e KC_DB_PASSWORD="$PGPASSWORD" \
    -e KC_BOOTSTRAP_ADMIN_USERNAME="$ADMIN_USER" -e KC_BOOTSTRAP_ADMIN_PASSWORD="$ADMIN_PASS" \
    -e KC_HOSTNAME="$FRONT" \
    -e KC_HOSTNAME_BACKCHANNEL_DYNAMIC=true \
    -e KC_HOSTNAME_STRICT=false \
    -e KC_HTTP_ENABLED=true \
    -e KC_HTTP_RELATIVE_PATH=/auth \
    -e KC_HTTP_PORT=8080 \
    -e KC_PROXY_HEADERS=xforwarded \
    -e KC_HEALTH_ENABLED=true \
    -e KC_METRICS_ENABLED=true \
    "$IMAGE_REPO:$1" start >/dev/null
}

wait_ready() { # <seconds>; echoes the seconds it took
  local t0 n=0 limit="$1"
  t0=$(date +%s)
  while [ "$n" -lt "$limit" ]; do
    if [ "$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:9000/auth/health/ready)" = 200 ]; then
      echo $(($(date +%s) - t0))
      return 0
    fi
    if [ "$(docker inspect -f '{{.State.Running}}' "$NAME" 2>/dev/null)" != true ]; then return 2; fi
    sleep 2
    n=$((n + 2))
  done
  return 1
}

kc() { docker exec "$NAME" /opt/keycloak/bin/kcadm.sh "$@" --config /tmp/kcadm.config; }
kclogin() { kc config credentials --server http://127.0.0.1:8080/auth --realm master --user "$ADMIN_USER" --password "$ADMIN_PASS" >/dev/null 2>"$OUT/kcadm.err"; }

# --- 1. baseline ----------------------------------------------------------
say "## Keycloak $BASELINE -> $TARGET (image $IMAGE_REPO)"
say ""
start_kc "$BASELINE"
if ! SECS=$(wait_ready 240); then
  fail "baseline $BASELINE never became ready"
  docker logs "$NAME" >"$OUT/baseline-fail.log" 2>&1
  exit 1
fi
pass "baseline $BASELINE ready in ${SECS}s"
kclogin || { fail "baseline kcadm login"; exit 1; }

# Seed what the chart + operator produce, enough to see the migration touch it.
# Seeded over the admin REST API (curl), not kcadm: kcadm is the thing under test
# below, and a seed that fails must not look like an upgrade finding.
API=http://127.0.0.1:8080/auth/admin/realms
TOKEN=$(curl -s -d client_id=admin-cli -d grant_type=password -d username="$ADMIN_USER" -d password="$ADMIN_PASS" \
  http://127.0.0.1:8080/auth/realms/master/protocol/openid-connect/token | jq -r .access_token)
[ -n "$TOKEN" ] && [ "$TOKEN" != null ] || { fail "seed: could not get an admin token"; exit 1; }
rest() { # <method> <path> [json]; logs the status line, never the body of a credential call
  local code
  code=$(curl -s -o "$OUT/seed-last.json" -w '%{http_code}' -X "$1" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' ${3:+-d "$3"} "$API$2")
  echo "$1 $2 -> $code" >>"$OUT/seed.log"
  [ "$code" -lt 300 ]
}
rest POST "" '{"realm":"'$REALM'","enabled":true}' \
  || { fail "seed: realm $REALM was not created ($(head -c 200 "$OUT/seed-last.json"))"; docker logs --tail 80 "$NAME" >"$OUT/seed-fail-server.log" 2>&1; exit 1; }
rest PUT /$REALM '{"realm":"'$REALM'","verifyEmail":true,"loginWithEmailAllowed":true,"ssoSessionIdleTimeout":28800,"ssoSessionMaxLifespan":604800}' \
  || { fail "seed: realm settings not applied ($(head -c 200 "$OUT/seed-last.json"))"; docker logs --tail 80 "$NAME" >"$OUT/seed-fail-server.log" 2>&1; exit 1; }
rest POST /$REALM/groups '{"name":"gardenio-admins"}'
rest POST /$REALM/roles '{"name":"gardenio-admin"}'
rest POST /$REALM/authentication/flows/browser/copy '{"newName":"gardenio-mfa-browser"}'
rest PUT /$REALM '{"realm":"'$REALM'","browserFlow":"gardenio-mfa-browser"}'
# The browser SSO client (exact redirect, post-logout "+") and the kd login
# device client (device grant, client session timeouts equal to the realm's SSO).
rest POST /$REALM/clients '{"clientId":"gardenio-web","enabled":true,"publicClient":false,"redirectUris":["https://gardenio.clonk.tv/oauth2/callback"],"webOrigins":["https://gardenio.clonk.tv"],"attributes":{"post.logout.redirect.uris":"+"}}'
rest POST /$REALM/clients '{"clientId":"gardenio-cli","enabled":true,"publicClient":true,"standardFlowEnabled":false,"attributes":{"oauth2.device.authorization.grant.enabled":"true","oauth2.device.code.lifespan":"600","access.token.lifespan":"300","client.session.idle.timeout":"28800","client.session.max.lifespan":"604800"}}'
rest POST /$REALM/users '{"username":"seed-user","email":"seed-user@example.invalid","emailVerified":true,"enabled":true,"credentials":[{"type":"password","value":"seed-'"$(date +%s)"'-throwaway","temporary":false}]}'
grep -c -- '-> 2' "$OUT/seed.log" | sed 's/^/seed calls ok: /' >>"$OUT/seed.log"

flow_shape() { kc get authentication/flows/gardenio-mfa-browser/executions -r $REALM --fields displayName,requirement,level,index 2>/dev/null | jq -c '[.[]|[.displayName,.requirement,.level]]'; }
flow_before=$(flow_shape)
echo "$flow_before" >"$OUT/flow-before.json"
if [ -z "$flow_before" ] || [ "$flow_before" = "[]" ]; then fail "seed: custom browser flow has no executions at baseline"; else pass "seeded realm $REALM, custom flow gardenio-mfa-browser ($(echo "$flow_before" | jq 'length') executions), 2 clients, 1 user"; fi

# kcadm exercise = the calls first-admin-job.yaml makes, normalised so the
# baseline and target outputs can be diffed (ids and timestamps removed).
exercise() { # <label>
  local f="$OUT/kcadm-$1.txt" gid uid line
  : >"$f"
  kclogin && echo "1 config credentials: ok" >>"$f" || echo "1 config credentials: FAIL $(head -c 200 "$OUT/kcadm.err")" >>"$f"
  kc get realms/$REALM --fields id >/dev/null 2>&1 && echo "2 get realm --fields id: ok" >>"$f" || echo "2 get realm: FAIL" >>"$f"
  if kc update realms/$REALM -s bruteForceProtected=true -s permanentLockout=false \
    -s failureFactor=10 -s waitIncrementSeconds=60 -s maxFailureWaitSeconds=900 -s maxDeltaTimeSeconds=43200 >/dev/null 2>"$OUT/kcadm.err"; then
    echo "3 update realm brute-force: ok" >>"$f"
  else echo "3 update realm brute-force: FAIL $(head -c 200 "$OUT/kcadm.err")" >>"$f"; fi
  echo "3b brute-force readback: $(kc get realms/$REALM --fields bruteForceProtected,permanentLockout,failureFactor,waitIncrementSeconds,maxFailureWaitSeconds,maxDeltaTimeSeconds 2>/dev/null | jq -cS .)" >>"$f"
  line=$(kc get groups -r $REALM -q "search=gardenio-admins" --fields id,name --format csv --noquotes 2>/dev/null | grep -m1 -- ",gardenio-admins$")
  gid=$(echo "$line" | cut -d, -f1)
  if grep -Eq '^[0-9a-f-]{36},gardenio-admins$' <<<"$line"; then echo "4 get groups -q search= csv shape: ok" >>"$f"; else echo "4 get groups csv shape: FAIL ($line)" >>"$f"; fi
  uid=$(kc get users -r $REALM -q "username=seed-user" --fields id --format csv --noquotes 2>/dev/null | head -1)
  if grep -Eq '^[0-9a-f-]{36}$' <<<"$uid"; then echo "5 get users -q username= csv shape: ok" >>"$f"; else echo "5 get users csv shape: FAIL ($uid)" >>"$f"; fi
  # create + set-password + credential check + update + group join (-n), on a throwaway user per run
  local u="exercise-$1"
  kc create users -r $REALM -s username="$u" -s email="$u@example.invalid" -s emailVerified=true -s enabled=true >/dev/null 2>&1 && echo "6 create users: ok" >>"$f" || echo "6 create users: FAIL" >>"$f"
  local nuid out
  nuid=$(kc get users -r $REALM -q "username=$u" --fields id --format csv --noquotes 2>/dev/null | head -1)
  out=$(kc set-password -r $REALM --username "$u" --new-password "ex-$(date +%s)-throwaway" 2>&1) || true
  case "$out" in *"Invalid password"*|*rror*) echo "7 set-password: FAIL ($out)" >>"$f" ;; *) echo "7 set-password: ok (output: '${out:-<empty>}')" >>"$f" ;; esac
  [ -n "$(kc get "users/$nuid/credentials" -r $REALM --fields type --format csv --noquotes 2>/dev/null | head -1)" ] && echo "8 credentials listed: ok" >>"$f" || echo "8 credentials listed: FAIL" >>"$f"
  kc update "users/$nuid" -r $REALM -s enabled=true -s emailVerified=true >/dev/null 2>&1 && echo "9 update user: ok" >>"$f" || echo "9 update user: FAIL" >>"$f"
  kc update "users/$nuid/groups/$gid" -r $REALM -s realm=$REALM -s userId="$nuid" -s groupId="$gid" -n >/dev/null 2>&1 && echo "10 join group (-n): ok" >>"$f" || echo "10 join group: FAIL" >>"$f"
  [ "$(kc get "users/$nuid/groups" -r $REALM --fields name --format csv --noquotes 2>/dev/null | head -1)" = gardenio-admins ] && echo "11 group membership readback: ok" >>"$f" || echo "11 group membership readback: FAIL" >>"$f"
}
exercise baseline
cp "$OUT/kcadm-baseline.txt" "$OUT/kcadm-baseline.keep"
if grep -q FAIL "$OUT/kcadm-baseline.txt"; then info "kcadm exercise had failures at the BASELINE (script issue, not an upgrade finding): $(grep FAIL "$OUT/kcadm-baseline.txt" | head -3 | tr '\n' ';')"; else pass "kcadm exercise on $BASELINE: all 11 calls ok"; fi

probe_status() { # <label>
  local f="$OUT/probes-$1.txt" p
  : >"$f"
  for p in started live ready; do echo "management :9000 /auth/health/$p -> $(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:9000/auth/health/$p)" >>"$f"; done
  echo "main :8080 /auth/health/ready -> $(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8080/auth/health/ready)" >>"$f"
  echo "management :9000 /auth/metrics -> $(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:9000/auth/metrics)" >>"$f"
}
discovery() { # <label>
  local f="$OUT/discovery-$1.txt" base=http://127.0.0.1:8080/auth/realms/$REALM
  {
    echo "via proxy headers  issuer: $(curl -s -H 'X-Forwarded-Host: auth.clonk.tv' -H 'X-Forwarded-Proto: https' $base/.well-known/openid-configuration | jq -r .issuer)"
    echo "via proxy headers  authorization_endpoint: $(curl -s -H 'X-Forwarded-Host: auth.clonk.tv' -H 'X-Forwarded-Proto: https' $base/.well-known/openid-configuration | jq -r .authorization_endpoint)"
    echo "in-cluster (Host)  issuer: $(curl -s -H 'Host: gardenio-keycloak-runtime.gardenio.svc.cluster.local:8080' $base/.well-known/openid-configuration | jq -r .issuer)"
    echo "in-cluster (Host)  token_endpoint: $(curl -s -H 'Host: gardenio-keycloak-runtime.gardenio.svc.cluster.local:8080' $base/.well-known/openid-configuration | jq -r .token_endpoint)"
    echo "in-cluster (Host)  jwks_uri: $(curl -s -H 'Host: gardenio-keycloak-runtime.gardenio.svc.cluster.local:8080' $base/.well-known/openid-configuration | jq -r .jwks_uri)"
    echo "XFH without port   authorization_endpoint: $(curl -s -H 'X-Forwarded-Host: auth.example.test' -H 'X-Forwarded-Proto: https' $base/.well-known/openid-configuration | jq -r .authorization_endpoint)"
  } >"$f"
}
probe_status baseline
discovery baseline

# --- 2. the roll ------------------------------------------------------------
docker stop -t 30 "$NAME" >/dev/null 2>&1
docker logs "$NAME" >"$OUT/baseline.log" 2>&1
start_kc "$TARGET"
SECS=$(wait_ready 600)
rc=$?
docker logs "$NAME" >"$OUT/target.log" 2>&1
if [ "$rc" -ne 0 ]; then
  fail "$TARGET did not become ready on the migrated $BASELINE database (rc=$rc); see target.log"
  exit 1
fi
pass "$TARGET ready on the migrated $BASELINE database in ${SECS}s (single replica: this is the failed-sign-in window)"
grep -E "Migrating|migration|Updating database" "$OUT/target.log" | head -5 | sed 's/^/        /' >>"$SUMMARY" || true
echo "ERROR/WARN lines in the target start log (deduped, timestamps stripped):" >"$OUT/target-log-findings.txt"
grep -E " (ERROR|WARN) " "$OUT/target.log" | sed -E 's/^[0-9: ,.-]+//' | sort | uniq -c | sort -rn | head -40 >>"$OUT/target-log-findings.txt"
nerr=$(grep -c " ERROR " "$OUT/target.log")
if [ "$nerr" -gt 0 ]; then info "$nerr ERROR line(s) in the target start log (target-log-findings.txt)"; else pass "no ERROR lines in the target start log"; fi

# --- 3. checks on the target -------------------------------------------------
probe_status target
discovery target
if diff <(sed 's/ -> .*//' "$OUT/probes-baseline.txt") <(sed 's/ -> .*//' "$OUT/probes-target.txt") >/dev/null \
  && grep -q "health/started -> 200" "$OUT/probes-target.txt" && grep -q "health/live -> 200" "$OUT/probes-target.txt" && grep -q "health/ready -> 200" "$OUT/probes-target.txt"; then
  pass "StatefulSet probes /auth/health/{started,live,ready} on :9000 all 200 (main port /auth/health/ready -> $(grep 'main :8080' "$OUT/probes-target.txt" | sed 's/.*-> //'), baseline $(grep 'main :8080' "$OUT/probes-baseline.txt" | sed 's/.*-> //'))"
else fail "probe set differs or is not 200 (probes-target.txt)"; fi
grep -q "metrics -> 200" "$OUT/probes-target.txt" && pass "metrics on :9000/auth/metrics -> 200 (ServiceMonitor path)" || fail "metrics path not 200"
if diff "$OUT/discovery-baseline.txt" "$OUT/discovery-target.txt" >"$OUT/discovery.diff"; then pass "issuer/endpoint hostnames identical to $BASELINE (proxy-header, in-cluster and X-Forwarded-Host-without-port cases)"; else fail "discovery document hostnames changed vs $BASELINE (discovery.diff)"; fi
grep -q "^via proxy headers  issuer: $FRONT/realms/$REALM\$" "$OUT/discovery-target.txt" && pass "issuer is $FRONT/realms/$REALM" || fail "issuer is not $FRONT/realms/$REALM"

exercise target
if diff "$OUT/kcadm-baseline.keep" "$OUT/kcadm-target.txt" >"$OUT/kcadm.diff"; then pass "all 11 first-admin kcadm calls behave exactly as on $BASELINE (kcadm-target.txt)"; else fail "kcadm behaviour changed vs $BASELINE (kcadm.diff): $(grep '^>' "$OUT/kcadm.diff" | head -3 | tr '\n' ';')"; fi

flow_after=$(flow_shape)
echo "$flow_after" >"$OUT/flow-after.json"
if [ "$flow_before" = "$flow_after" ]; then pass "custom browser flow gardenio-mfa-browser unchanged by the migration (no Organization sub-flow)"; else fail "custom browser flow changed by the migration (flow-before/after.json)"; fi
bound=$(kc get realms/$REALM --fields browserFlow 2>/dev/null | jq -r .browserFlow)
[ "$bound" = gardenio-mfa-browser ] && pass "realm browserFlow binding survived (gardenio-mfa-browser)" || fail "realm browserFlow is '$bound'"

# 26.5 client-session-timeout validation: the operator re-PUTs the client on every sync.
cid=$(kc get clients -r $REALM -q clientId=gardenio-cli --fields id --format csv --noquotes 2>/dev/null | head -1)
if kc update "clients/$cid" -r $REALM -s 'attributes."client.session.idle.timeout"=28800' -s 'attributes."client.session.max.lifespan"=604800' >/dev/null 2>"$OUT/kcadm.err"; then pass "client with client.session.idle=28800/max=604800 (== realm SSO) re-accepted"; else fail "client session timeouts rejected: $(head -c 300 "$OUT/kcadm.err")"; fi
if kc update "clients/$cid" -r $REALM -s 'attributes."client.session.idle.timeout"=999999' >/dev/null 2>"$OUT/kcadm.err"; then info "client idle timeout ABOVE the realm SSO idle was accepted (no 26.5 validation on this version)"; else info "client idle timeout above the realm SSO idle is rejected on $TARGET (26.5+ validation): this is why the chart's client values must stay <= realm sessions"; fi
kc update "clients/$cid" -r $REALM -s 'attributes."client.session.idle.timeout"=28800' >/dev/null 2>&1

# kd login device grant, login page
dev=$(curl -s -X POST -d client_id=gardenio-cli -d scope=openid http://127.0.0.1:8080/auth/realms/$REALM/protocol/openid-connect/auth/device)
echo "$dev" | jq -e 'has("user_code") and has("device_code")' >/dev/null 2>&1 \
  && pass "device authorization endpoint (kd login) returns user_code; verification_uri host: $(echo "$dev" | jq -r .verification_uri | sed -E 's#(https?://[^/]+).*#\1#')" \
  || fail "device authorization endpoint failed: $(echo "$dev" | head -c 200)"
code=$(curl -s -o "$OUT/login-page.html" -w '%{http_code}' -H 'X-Forwarded-Host: auth.clonk.tv' -H 'X-Forwarded-Proto: https' \
  "http://127.0.0.1:8080/auth/realms/$REALM/protocol/openid-connect/auth?client_id=gardenio-web&redirect_uri=https%3A%2F%2Fgardenio.clonk.tv%2Foauth2%2Fcallback&response_type=code&scope=openid")
if [ "$code" = 200 ] && grep -q 'name="username"' "$OUT/login-page.html"; then pass "login page renders (200, username field present, via proxy headers)"; else fail "login page: HTTP $code"; fi
grep -o 'href="[^"]*resources[^"]*\.css"' "$OUT/login-page.html" | head -2 | sed 's/^/        /' >>"$SUMMARY"

# path hygiene (26.4.3 rejects non-normalized paths, 26.5.2 rejects ';'): informational, the edge only forwards ^~ /auth/realms/gardenio/ and /auth/resources/
info "double slash   GET /auth//realms/$REALM -> $(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:8080/auth//realms/$REALM")"
info "semicolon      GET /auth/realms/$REALM;x=1 -> $(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:8080/auth/realms/$REALM;x=1")"

# passkey support
kc get authentication/authenticator-providers -r $REALM 2>/dev/null | jq -r '.[].id' | sort >"$OUT/authenticator-providers.txt"
for p in webauthn-authenticator webauthn-authenticator-passwordless; do
  grep -qx "$p" "$OUT/authenticator-providers.txt" && pass "authenticator provider $p present" || fail "authenticator provider $p missing"
done
grep -qx "conditional-credential" "$OUT/authenticator-providers.txt" && info "authenticator provider conditional-credential (\"Conditional - credential\") present" || info "authenticator provider conditional-credential absent (ids: $(grep -i -E 'cond|passkey' "$OUT/authenticator-providers.txt" | tr '\n' ' '))"
PK=(-s webAuthnPolicyPasswordlessRpId=auth.clonk.tv -s webAuthnPolicyPasswordlessRpEntityName=gardenio
  -s webAuthnPolicyPasswordlessUserVerificationRequirement=required -s webAuthnPolicyPasswordlessAttestationConveyancePreference=none
  -s 'webAuthnPolicyPasswordlessSignatureAlgorithms=["ES256","RS256"]' -s webAuthnPolicyPasswordlessAvoidSameAuthenticatorRegister=true)
if kc update realms/$REALM "${PK[@]}" -s webAuthnPolicyPasswordlessResidentKey=required >/dev/null 2>"$OUT/kcadm.err"; then
  pass "kcadm accepts the S3a WebAuthn Passwordless attributes incl. webAuthnPolicyPasswordlessResidentKey=required"
elif kc update realms/$REALM "${PK[@]}" -s webAuthnPolicyPasswordlessRequireResidentKey=Yes >/dev/null 2>"$OUT/kcadm.err"; then
  info "webAuthnPolicyPasswordlessResidentKey is NOT accepted on $TARGET; the deprecated webAuthnPolicyPasswordlessRequireResidentKey=Yes is"
else fail "kcadm rejected the WebAuthn Passwordless attributes: $(head -c 300 "$OUT/kcadm.err")"; fi
kc get realms/$REALM 2>/dev/null | jq -S 'with_entries(select(.key|test("^webAuthnPolicyPasswordless")))' >"$OUT/webauthn-passwordless-attrs.json"

# --- 4. rollback shape --------------------------------------------------------
docker stop -t 30 "$NAME" >/dev/null 2>&1
start_kc "$BASELINE"
if SECS=$(wait_ready 150); then
  info "ROLLBACK: $BASELINE started on the $TARGET-migrated database (ready in ${SECS}s). Still, schema migration is forward-only; rely on the pg_dump restore, not on this."
else
  pass "ROLLBACK shape confirmed: $BASELINE does NOT come up on a $TARGET-migrated database, so S2c must restore the pg_dump to roll back, not just re-pin the tag"
  docker logs "$NAME" 2>&1 | grep -iE "ERROR|schema|version|migrat" | tail -5 | sed -E 's/^[0-9: ,.-]+//' >"$OUT/rollback-refusal.txt"
fi
docker rm -f "$NAME" >/dev/null 2>&1

say ""
say "Hard failures: $FAILS"
[ "$FAILS" -eq 0 ]
