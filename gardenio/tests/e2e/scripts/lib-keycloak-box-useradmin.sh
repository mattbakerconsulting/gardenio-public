#!/usr/bin/env bash
# lib-keycloak-box-useradmin.sh — agent-side helper for self-serve
# user-management on the gardenio-in-a-box (GiB) Keycloak.
#
# This is a THIN wrapper over lib-keycloak-client-creds.sh: it overrides the
# defaults (box auth host, `gardenio-inbox` realm, `gardenio-box-useradmin-svc`
# client, and the ESO-synced K8s Secret name) so an agent pod can mint a
# `client_credentials` token and drive the box Keycloak admin REST to
# create/manage users WITHOUT the master admin password and WITHOUT SSH.
#
# Bead chain: ka-0000001845 (this helper + secret provisioning) ->
# epic ka-0000001297 (self-serve box user-mgmt). Design: ka-0000005452.
# Runbook: gardenio/docs/runbooks/box-useradmin-svc-secret.md.
#
# Prereqs (all provisioned by ka-0000001845 / sibling ka-0000001926):
#   - The confidential client `gardenio-box-useradmin-svc` exists on the box
#     `gardenio-inbox` realm with serviceAccountsEnabled=true, fullScopeAllowed
#     =false, and realm-management roles {manage-users, view-users}.
#   - Its secret is synced into the agent namespace as K8s Secret
#     `gardenio-box-useradmin-svc` (key `clientSecret`) via ExternalSecret
#     (externalSecrets.boxUserAdminSvcSecret.* in the deploy overlay).
#   - The agent ServiceAccount has `get` on that Secret name (extraRules).
#
# Usage:
#   # shellcheck disable=SC1090
#   source "$(dirname "${BASH_SOURCE[0]}")/lib-keycloak-box-useradmin.sh"
#
#   token=$(mint_box_useradmin_token) || exit 1
#   box_create_user alice alice@acme.example
#   box_set_temp_password alice 'ChangeMe-123'
#   box_join_group alice gardenio-admins
#
# Inputs (env, override before sourcing):
#   KC_BASE_URL   default https://auth.192-0-2-91.sslip.io/auth (the box)
#   KC_REALM      default gardenio-inbox
#   KC_CLIENT_ID  default gardenio-box-useradmin-svc
#   KC_CLIENT_SECRET_NAME  default gardenio-box-useradmin-svc (the ESO target)
#   KC_CLIENT_SECRET       if set, used directly (skips the K8s Secret read)
#   All other KC_* inputs from lib-keycloak-client-creds.sh apply unchanged.
#
# Requirements: curl, jq, and (unless KC_CLIENT_SECRET is set) kubectl with
# `get` on the Secret. Every function prints only its intended stdout; all
# diagnostics go to stderr and non-zero exit signals failure.

# shellcheck disable=SC2317
if [ -n "${__GARDENIO_LIB_KEYCLOAK_BOX_USERADMIN_LOADED:-}" ]; then
  return 0 2>/dev/null || true
fi
__GARDENIO_LIB_KEYCLOAK_BOX_USERADMIN_LOADED=1

# Box defaults. Set these BEFORE sourcing the base helper so its `:=` defaults
# do not win. The base helper is idempotent (load-guarded), so sourcing it here
# is safe even if the caller also sourced it.
: "${KC_BASE_URL:=https://auth.192-0-2-91.sslip.io/auth}"
: "${KC_REALM:=gardenio-inbox}"
: "${KC_CLIENT_ID:=gardenio-box-useradmin-svc}"
# The box client secret is ESO-synced into a FIXED Secret name (not the
# operator-CRD-derived name the base helper would compute), so pin it here
# unless the caller overrode it.
: "${KC_CLIENT_SECRET_NAME:=gardenio-box-useradmin-svc}"

# Source the shared client_credentials primitives (kc_client_secret,
# mint_client_credentials_token, etc.).
# shellcheck disable=SC1090,SC1091
source "$(dirname "${BASH_SOURCE[0]}")/lib-keycloak-client-creds.sh"

# mint_box_useradmin_token — mint a client_credentials access token against the
# box Keycloak. Thin alias over the base primitive for a self-documenting name.
mint_box_useradmin_token() {
  mint_client_credentials_token
}

# _box_admin_base — the admin REST base for the box realm.
_box_admin_base() {
  printf '%s/admin/realms/%s' "${KC_BASE_URL}" "${KC_REALM}"
}

# box_create_user <username> [email] — create an enabled user. Idempotent-ish:
# returns 0 if the user already exists (Keycloak replies 409). Prints nothing on
# success.
box_create_user() {
  local username="$1" email="${2:-}" token body http
  if [ -z "$username" ]; then
    echo "box_create_user: username required" >&2
    return 2
  fi
  token=$(mint_box_useradmin_token) || return 1
  body=$(jq -nc --arg u "$username" --arg e "$email" \
    '{username:$u, enabled:true} + (if $e=="" then {} else {email:$e, emailVerified:true} end)')
  http=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$(_box_admin_base)/users" \
    -H "Authorization: Bearer ${token}" -H 'Content-Type: application/json' \
    -d "$body") || {
      echo "box_create_user: request failed for ${username}" >&2
      return 1
    }
  case "$http" in
    201) return 0 ;;
    409) echo "box_create_user: ${username} already exists (409)" >&2; return 0 ;;
    *)   echo "box_create_user: unexpected HTTP ${http} for ${username}" >&2; return 1 ;;
  esac
}

# box_user_id <username> — print the Keycloak user id for an exact username.
box_user_id() {
  local username="$1" token
  if [ -z "$username" ]; then
    echo "box_user_id: username required" >&2
    return 2
  fi
  token=$(mint_box_useradmin_token) || return 1
  curl -sS --fail-with-body \
    "$(_box_admin_base)/users?username=${username}&exact=true" \
    -H "Authorization: Bearer ${token}" \
    | jq -er '.[0].id // empty' || {
      echo "box_user_id: no user found for ${username}" >&2
      return 1
    }
}

# box_set_temp_password <username> <password> — set a TEMPORARY password (user
# must change at next login).
box_set_temp_password() {
  local username="$1" password="$2" token uid http
  if [ -z "$username" ] || [ -z "$password" ]; then
    echo "box_set_temp_password: username and password required" >&2
    return 2
  fi
  token=$(mint_box_useradmin_token) || return 1
  uid=$(box_user_id "$username") || return 1
  http=$(curl -sS -o /dev/null -w '%{http_code}' -X PUT \
    "$(_box_admin_base)/users/${uid}/reset-password" \
    -H "Authorization: Bearer ${token}" -H 'Content-Type: application/json' \
    -d "$(jq -nc --arg p "$password" '{type:"password", value:$p, temporary:true}')") || {
      echo "box_set_temp_password: request failed for ${username}" >&2
      return 1
    }
  case "$http" in
    204) return 0 ;;
    *)   echo "box_set_temp_password: unexpected HTTP ${http} for ${username}" >&2; return 1 ;;
  esac
}

# box_group_id <group-name> — print the id of a top-level group by exact name.
box_group_id() {
  local group="$1" token
  if [ -z "$group" ]; then
    echo "box_group_id: group name required" >&2
    return 2
  fi
  token=$(mint_box_useradmin_token) || return 1
  curl -sS --fail-with-body \
    "$(_box_admin_base)/groups?search=${group}" \
    -H "Authorization: Bearer ${token}" \
    | jq -er --arg g "$group" '(.[] | select(.name==$g) | .id) // empty' | head -n1 || {
      echo "box_group_id: no group found for ${group}" >&2
      return 1
    }
}

# box_join_group <username> <group-name> — add a user to a group by name.
box_join_group() {
  local username="$1" group="$2" token uid gid http
  if [ -z "$username" ] || [ -z "$group" ]; then
    echo "box_join_group: username and group required" >&2
    return 2
  fi
  token=$(mint_box_useradmin_token) || return 1
  uid=$(box_user_id "$username") || return 1
  gid=$(box_group_id "$group") || return 1
  http=$(curl -sS -o /dev/null -w '%{http_code}' -X PUT \
    "$(_box_admin_base)/users/${uid}/groups/${gid}" \
    -H "Authorization: Bearer ${token}") || {
      echo "box_join_group: request failed (${username} -> ${group})" >&2
      return 1
    }
  case "$http" in
    204) return 0 ;;
    *)   echo "box_join_group: unexpected HTTP ${http} (${username} -> ${group})" >&2; return 1 ;;
  esac
}
