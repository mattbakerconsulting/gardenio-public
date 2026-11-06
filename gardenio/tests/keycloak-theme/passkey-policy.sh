#!/usr/bin/env bash
# Run the SHIPPED passkey-policy hook block against the harness Keycloak (plan
# gd-QXq1fjYuRD, step S3a, gd-h9M9eTTun8).
#
#   ./passkey-policy.sh <container-name>
#
# 1. helm-templates the chart's first-admin Job with realmHardening.passkeys on
#    and hostnameUrl pointing at the harness (RP ID = localhost, so the passkey
#    spec that follows can enrol against it);
# 2. takes the text between the `# --- passkeys:begin` / `# --- passkeys:end`
#    marker comments (the hook's own bytes, not a copy);
# 3. runs it with kcadm inside the container, twice (idempotence);
# 4. reads the realm back and asserts every attribute.
#
# Writes realm-webauthn-after-hook.json (the webAuthnPolicy* keys) next to this
# script. Throwaway credentials only; the container is deleted by run.sh.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/../../.." && pwd)"
CHART="$REPO/gardenio/helm/gardenio"
NAME="${1:?usage: passkey-policy.sh <container-name>}"
REALM=gardenio
: "${KC_ADMIN_USER:?}" "${KC_ADMIN_PASSWORD:?}"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# --- 1+2: render the Job, extract the block ----------------------------------
helm template t "$CHART" -n gardenio \
  --show-only templates/keycloak-runtime/first-admin-job.yaml \
  --set postgres.enabled=true --set keycloak.enabled=true \
  --set keycloak.runtime.enabled=true --set keycloak.runtime.db.autoWire=true \
  --set keycloak.runtime.host=kc.svc.cluster.local \
  --set keycloak.runtime.hostnameUrl=http://localhost:8080/auth \
  --set keycloak.runtime.realmHardening.passkeys.enabled=true \
  > "$WORK/job.yaml"
sed -n '/# --- passkeys:begin/,/# --- passkeys:end/p' "$WORK/job.yaml" \
  | sed 's/^ \{14\}//' > "$WORK/block.sh"
[ "$(wc -l < "$WORK/block.sh")" -gt 5 ] \
  || { echo "passkeys block not found in the rendered Job" >&2; cat "$WORK/job.yaml" >&2; exit 1; }
bash -n "$WORK/block.sh"
echo "== passkey hook block: $(wc -l < "$WORK/block.sh") lines extracted from the rendered first-admin Job"

# --- 3: run it in the container, same kcadm wrapper shape as the hook --------
cat > "$WORK/prelude.sh" <<'PRELUDE'
KC_REALM=gardenio
KCADM_CONFIG=/tmp/kcadm.config
KCADM_ERR=/tmp/kcadm.err
kcadm() { /opt/keycloak/bin/kcadm.sh "$@" --config "$KCADM_CONFIG"; }
PRELUDE

# The script goes in as a file (docker cp), not on stdin: kcadm is a JVM launcher
# and could swallow the rest of a script piped to `bash -s`.
kc_exec() {
  cat > "$WORK/exec.sh"
  docker cp "$WORK/exec.sh" "$NAME:/tmp/passkey-exec.sh"
  docker exec -e KC_ADMIN_USER -e KC_ADMIN_PASSWORD "$NAME" bash /tmp/passkey-exec.sh
}

{ cat "$WORK/prelude.sh"; cat <<'LOGIN'
kcadm config credentials --server http://localhost:8080/auth --realm master \
  --user "$KC_ADMIN_USER" --password "$KC_ADMIN_PASSWORD" > /dev/null
LOGIN
} | kc_exec

run_block() {
  { cat "$WORK/prelude.sh"; cat "$WORK/block.sh"; } | kc_exec 2>&1
}
for pass in 1 2; do
  out="$(run_block)"
  printf '%s\n' "$out" | sed "s/^/   [run $pass] /"
  case "$out" in
    *"WARNING:"*) echo "FAIL: the hook block warned on run $pass" >&2; exit 1 ;;
    *"passkey (WebAuthn passwordless) policy applied"*) ;;
    *) echo "FAIL: the hook block did not report success on run $pass" >&2; exit 1 ;;
  esac
done

# --- 4: read the realm back and assert every attribute -----------------------
{ cat "$WORK/prelude.sh"; echo "kcadm get realms/$REALM"; } | kc_exec \
  | jq 'with_entries(select(.key | test("^webAuthnPolicy")))' > "$HERE/realm-webauthn-after-hook.json"

fail=0
check() { # <jq attr> <expected json>
  local got
  got="$(jq -c --arg k "$1" '.[$k]' "$HERE/realm-webauthn-after-hook.json")"
  if [ "$got" = "$2" ]; then echo "   ok   $1 = $got"; else echo "   FAIL $1 = $got (want $2)" >&2; fail=1; fi
}
P=webAuthnPolicyPasswordless
check ${P}PasskeysEnabled true
check ${P}RpId '"localhost"'
check ${P}RpEntityName '"gardenio"'
check ${P}UserVerificationRequirement '"required"'
check ${P}ResidentKey '"required"'
check ${P}AttestationConveyancePreference '"none"'
check ${P}AuthenticatorAttachment '"not specified"'
check ${P}SignatureAlgorithms '["ES256","RS256"]'
check ${P}AvoidSameAuthenticatorRegister true
[ "$fail" = 0 ] || { echo "passkey policy read-back mismatch" >&2; exit 1; }
echo "== passkey policy: shipped hook block applied twice, every attribute read back as expected"
