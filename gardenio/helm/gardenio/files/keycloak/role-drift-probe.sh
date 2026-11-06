#!/usr/bin/env bash
# role-drift-probe.sh: the keycloak-role-drift CronJob's check (gd-L2MyfEe9lL;
# threat model gardenio/docs/security/lands-threat-model.md).
#
# A realm user who holds gardenio-viewer and no gardenio-project-member is an
# unrestricted reader in kbeads and the viewer, and every Lands applicant is a
# fresh user. Every new user holds default-roles-<realm>, so:
#
#   R1  default-roles-<realm> composes no gardenio-* role
#
# printed as one PASS or FAIL line. The chart never declares such a composite
# and the EDP operator never prunes one, so a hand change or old state would
# persist unseen. This is the continuous twin of lands-fence-matrix.sh K1.
# A grant through a realm default group is out of scope here (operator decision
# gd-TgsaQWJPQ3: read-only, no canary user); the chart refuses to render this
# probe while a declared default group holds a gardenio-* role, and K2 covers
# it at check-in.
#
# It signs in to the realm as its own confidential client, whose service
# account holds realm-management view-realm only: it reads, never writes.
#
# Exit 0 only when R1 passes. Exit 1 on drift or on any error: a probe that
# cannot look must not look like a clean one. The alert reads the CronJob's
# last successful run (vmalert gapAlerts.keycloakRoleDrift).
#
# Env: KC_SERVER, KC_REALM, KC_CLIENT_ID, KC_CLIENT_SECRET; optional
# TRUSTSTORE_FILE + TRUSTSTORE_PASSWORD; KCADM (default the image's kcadm.sh)
# and KCADM_CONFIG (default $HOME/kcadm.config).
set -uo pipefail

KCADM="${KCADM:-/opt/keycloak/bin/kcadm.sh}"
KCADM_CONFIG="${KCADM_CONFIG:-${HOME:-/tmp}/kcadm.config}"
KCADM_ERR="${KCADM_ERR:-${HOME:-/tmp}/kcadm.err}"

# --config goes LAST: before a two-word subcommand (config credentials) kcadm
# rejects it (see first-admin-job.yaml).
kcadm() { "$KCADM" "$@" --config "$KCADM_CONFIG"; }

die() {
  echo "[role-drift] FAIL $1"
  [ -s "$KCADM_ERR" ] && sed 's/^/[role-drift]   kcadm> /' "$KCADM_ERR"
  exit 1
}

for v in KC_SERVER KC_REALM KC_CLIENT_ID KC_CLIENT_SECRET; do
  [ -n "${!v:-}" ] || die "setup: $v is empty"
done

if [ -n "${TRUSTSTORE_FILE:-}" ]; then
  kcadm config truststore --trustpass "${TRUSTSTORE_PASSWORD:-}" "$TRUSTSTORE_FILE" >/dev/null 2>"$KCADM_ERR" ||
    die "setup: could not load truststore $TRUSTSTORE_FILE"
fi
kcadm config credentials --server "$KC_SERVER" --realm "$KC_REALM" \
  --client "$KC_CLIENT_ID" --secret "$KC_CLIENT_SECRET" >/dev/null 2>"$KCADM_ERR" ||
  die "setup: client $KC_CLIENT_ID could not sign in to realm $KC_REALM at $KC_SERVER"

if ! out="$(kcadm get "roles/default-roles-$KC_REALM/composites" -r "$KC_REALM" \
  --fields name --format csv --noquotes 2>"$KCADM_ERR")"; then
  die "R1 could not read default-roles-$KC_REALM composites"
fi
bad="$(printf '%s\n' "$out" | grep -E '^gardenio-' | tr '\n' ' ' || true)"
if [ -n "$bad" ]; then
  echo "[role-drift] FAIL R1 default-roles-$KC_REALM composes ${bad}: every new user holds it"
  exit 1
fi
echo "[role-drift] PASS R1 default-roles-$KC_REALM composes no gardenio-* role ($(printf '%s' "$out" | tr '\n' ' '))"
