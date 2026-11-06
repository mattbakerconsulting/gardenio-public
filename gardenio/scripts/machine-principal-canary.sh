#!/usr/bin/env bash
# machine-principal-canary.sh — prove that a DIFFERENT machine principal is
# refused the account-bead write the controller is now allowed (plan
# gd-UdM4LnnaGE Task 7, step gd-eiCV6zrLUS; design
# gardenio/docs/machine-principal-design.md).
#
# It authenticates to Keycloak as the confidential client gardenio-authz-canary
# (client-credentials grant), reads ONE field of the target account bead, and
# PATCHes that same field back with the same value — an idempotent write that
# kbeads must still refuse, because the canary's service account holds only
# gardenio-operator and no gardenio-machine-* grant. The expected answer is a
# visible 403 whose body names the caller:
#
#   HTTP 403
#   caller_subject: machine:gardenio-authz-canary
#   required_role: gardenio-admin
#
# Exit status (the Job's success/failure IS the verdict):
#   0  the write was denied with HTTP 403 and caller_subject == machine:<client>
#   1  the write was ALLOWED (2xx) — a different machine principal can write
#      account beads; this is the failure the canary exists to catch
#   2  inconclusive: token fetch failed, GET failed, field absent, a 403 that
#      names someone else, or any other status
#
# Secrets handling: the client secret is read from a file and sent with
# --data-urlencode name@file, the bearer token is sent with -H @file — neither
# ever appears in argv, stdout or stderr. Response bodies go to temp files that
# are deleted on exit and are NEVER printed: account beads carry `credential`
# and `litellm_key`. Only the HTTP status and, on 403, the two attribution
# fields are printed.
#
# Environment:
#   BEADS_URL                  e.g. http://gardenio-beads.gardenio.svc:8080
#   BEADS_OIDC_TOKEN_URL       Keycloak token endpoint for realm gardenio
#   TARGET                     account bead id (gd-XOnJ3PSDsn on k3s)
#   CANARY_CLIENT_ID           default gardenio-authz-canary
#   CANARY_CLIENT_SECRET_FILE  default /var/run/secrets/gardenio-machine/clientSecret
#   CANARY_FIELD               default probe_auth_status (a schema-known string
#                              field on type:account, so the PATCH reaches the
#                              authz check instead of failing validation)
#
# Run in-cluster via gardenio/helm/manifests/machine-principal-canary-job.yaml;
# unit tests: gardenio/scripts/machine-principal-canary_test.sh.
set -euo pipefail
set +x

CLIENT_ID="${CANARY_CLIENT_ID:-gardenio-authz-canary}"
SECRET_FILE="${CANARY_CLIENT_SECRET_FILE:-/var/run/secrets/gardenio-machine/clientSecret}"
FIELD="${CANARY_FIELD:-probe_auth_status}"
EXPECTED_SUBJECT="machine:${CLIENT_ID}"

inconclusive() { echo "canary: INCONCLUSIVE: $*" >&2; exit 2; }

for v in BEADS_URL BEADS_OIDC_TOKEN_URL TARGET; do
  [ -n "${!v:-}" ] || inconclusive "$v is not set"
done
for bin in curl jq; do
  command -v "$bin" >/dev/null 2>&1 || inconclusive "$bin is not installed"
done
[ -r "$SECRET_FILE" ] || inconclusive "client secret file is not readable: $SECRET_FILE"

umask 077
work="$(mktemp -d "${TMPDIR:-/tmp}/canary.XXXXXX")"
trap 'rm -rf "$work"' EXIT

# ── 1. token: client-credentials grant ─────────────────────────────────────
# A trailing newline in the mounted secret would be URL-encoded into the
# credential and fail the grant, so send a stripped copy (same 0600 tempdir).
tr -d '\n\r' < "$SECRET_FILE" > "$work/secret"
[ -s "$work/secret" ] || inconclusive "client secret file is empty: $SECRET_FILE"

code="$(curl -sS -o "$work/token.json" -w '%{http_code}' \
  --data-urlencode 'grant_type=client_credentials' \
  --data-urlencode "client_id=${CLIENT_ID}" \
  --data-urlencode "client_secret@${work}/secret" \
  "$BEADS_OIDC_TOKEN_URL")" || inconclusive "token request failed (curl exit $?)"
[ "$code" = "200" ] || inconclusive "token endpoint returned HTTP $code"

jq -r 'if (.access_token // "") == "" then empty else "Authorization: Bearer " + .access_token end' \
  "$work/token.json" > "$work/auth.hdr" 2>/dev/null || true
[ -s "$work/auth.hdr" ] || inconclusive "token endpoint returned no access_token"

# ── 2. read the current value of ONE field (never the body) ───────────────
code="$(curl -sS -o "$work/get.json" -w '%{http_code}' -H "@${work}/auth.hdr" \
  "${BEADS_URL}/v1/beads/${TARGET}")" || inconclusive "GET failed (curl exit $?)"
[ "$code" = "200" ] || inconclusive "GET ${TARGET} returned HTTP $code; cannot build an idempotent write"

current="$(jq -r --arg f "$FIELD" '.fields[$f] // empty' "$work/get.json" 2>/dev/null || true)"
[ -n "$current" ] || inconclusive "field ${FIELD} is absent on ${TARGET}; refusing to write a new value"

# ── 3. the idempotent write: same field, same value ───────────────────────
payload="$(jq -cn --arg f "$FIELD" --arg v "$current" '{fields: {($f): $v}}')"
code="$(curl -sS -o "$work/patch.json" -w '%{http_code}' -X PATCH \
  -H "@${work}/auth.hdr" -H 'Content-Type: application/json' \
  --data "$payload" "${BEADS_URL}/v1/beads/${TARGET}")" || inconclusive "PATCH failed (curl exit $?)"

echo "HTTP ${code}"
case "$code" in
  403)
    subject="$(jq -r '.caller_subject // ""' "$work/patch.json" 2>/dev/null || true)"
    role="$(jq -r '.required_role // ""' "$work/patch.json" 2>/dev/null || true)"
    echo "caller_subject: ${subject}"
    echo "required_role: ${role}"
    if [ "$subject" = "$EXPECTED_SUBJECT" ]; then
      echo "canary: PASS: update/account by ${EXPECTED_SUBJECT} was refused"
      exit 0
    fi
    inconclusive "403 but caller_subject is not ${EXPECTED_SUBJECT}"
    ;;
  2*)
    echo "canary: FAIL: update/account by ${EXPECTED_SUBJECT} was ALLOWED (HTTP ${code})" >&2
    exit 1
    ;;
  *)
    inconclusive "PATCH returned HTTP ${code}"
    ;;
esac
