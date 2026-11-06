#!/usr/bin/env bash
# Build the CHART's passkey browser flow in the harness realm (plan
# gd-QXq1fjYuRD, step S4a, gd-SR4JfUslzG).
#
#   ./build-flow.sh <container-name> [factor]      # factor defaults to passkey
#
# 1. helm-templates auth-flow-mfa.yaml with keycloak.mfa.factor=<factor> and
#    keeps the KeycloakAuthFlow CRs (the chart's intent, not a hand-copied flow);
# 2. creates the same flows, executions, requirements and authenticator configs
#    in realm `gardenio` with kcadm, parents before children, executions in
#    priority order (Keycloak orders executions by creation);
# 3. binds the top-level flow as the realm browser flow (what
#    KeycloakRealm.browserFlow does) and reads every flow back, failing if the
#    realm differs from the CRs;
# 4. dumps what Keycloak ships (the built-in browser flow's executions and the
#    config schema of `conditional-credential`) for the PR description.
#
# Writes flow-executions.json, browser-builtin-executions.json and
# conditional-credential-config.json next to this script. Throwaway credentials
# only; the container is deleted by run.sh.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/../../.." && pwd)"
CHART="$REPO/gardenio/helm/gardenio"
NAME="${1:?usage: build-flow.sh <container-name> [factor]}"
FACTOR="${2:-passkey}"
REALM=gardenio
: "${KC_ADMIN_USER:?}" "${KC_ADMIN_PASSWORD:?}"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

kc() { docker exec "$NAME" /opt/keycloak/bin/kcadm.sh "$@" --config /tmp/kcadm.config; }

docker exec "$NAME" /opt/keycloak/bin/kcadm.sh config credentials --config /tmp/kcadm.config \
  --server http://localhost:8080/auth --realm master \
  --user "$KC_ADMIN_USER" --password "$KC_ADMIN_PASSWORD" > /dev/null

# --- 1: the chart's flows ----------------------------------------------------
helm template t "$CHART" -n gardenio \
  --show-only templates/keycloak/auth-flow-mfa.yaml \
  --set keycloak.enabled=true --set keycloak.mfa.enabled=true \
  --set "keycloak.mfa.factor=$FACTOR" > "$WORK/flows.yaml"
yq -o=json -I=0 'select(.kind == "KeycloakAuthFlow") | .spec' "$WORK/flows.yaml" | jq -s '.' > "$WORK/crs.json"
n="$(jq length "$WORK/crs.json")"
[ "$n" = 3 ] || { echo "expected 3 KeycloakAuthFlow CRs for factor=$FACTOR, got $n" >&2; exit 1; }
TOP="$(jq -r '[.[] | select(.topLevel)][0].alias' "$WORK/crs.json")"
echo "== chart flows for factor=$FACTOR: $(jq -r '[.[].alias] | join(", ")' "$WORK/crs.json")"

# --- 4 (first, it reads the pristine realm): what Keycloak ships --------------
kc get authentication/flows/browser/executions -r "$REALM" > "$HERE/browser-builtin-executions.json"
kc get authentication/config-description/conditional-credential -r "$REALM" > "$HERE/conditional-credential-config.json"
echo "== conditional-credential config keys on this server: $(jq -r '[.properties[].name] | join(", ")' "$HERE/conditional-credential-config.json")"
for k in credentials included; do
  jq -e --arg k "$k" '.properties[] | select(.name == $k)' "$HERE/conditional-credential-config.json" > /dev/null \
    || { echo "FAIL: conditional-credential has no config key '$k' on this server" >&2; exit 1; }
done
echo "== built-in browser flow: $(jq -r '[.[] | select(.level == 0) | (.providerId // .displayName) + ":" + .requirement] | join(" ")' "$HERE/browser-builtin-executions.json")"

# --- 2: create flows, parents first ------------------------------------------
# The executions of one flow, level 0 only, in order.
execs() { kc get "authentication/flows/$1/executions" -r "$REALM" | jq -c '[.[] | select(.level == 0)]'; }

# set_requirement <flow alias> <execution id> <requirement>
set_requirement() {
  local body
  body="$(kc get "authentication/flows/$1/executions" -r "$REALM" \
    | jq -c --arg id "$2" --arg r "$3" '.[] | select(.id == $id) | .requirement = $r')"
  [ -n "$body" ] || { echo "execution $2 not found in flow $1" >&2; exit 1; }
  kc update "authentication/flows/$1/executions" -r "$REALM" -b "$body"
}

declare -A CREATED=()
for i in $(seq 0 $((n - 1))); do
  cr="$(jq -c ".[$i]" "$WORK/crs.json")"
  alias="$(jq -r .alias <<< "$cr")"
  top="$(jq -r .topLevel <<< "$cr")"
  desc="$(jq -r '.description // ""' <<< "$cr")"
  if [ "$top" = true ]; then
    kc create authentication/flows -r "$REALM" \
      -b "$(jq -nc --arg a "$alias" --arg d "$desc" '{alias:$a, description:$d, providerId:"basic-flow", topLevel:true, builtIn:false}')" > /dev/null
  else
    parent="$(jq -r .parentName <<< "$cr")"
    [ -n "${CREATED[$parent]:-}" ] || { echo "flow $alias: parent $parent is not created yet (CR order)" >&2; exit 1; }
    kc create "authentication/flows/$parent/executions/flow" -r "$REALM" \
      -b "$(jq -nc --arg a "$alias" --arg d "$desc" --arg t "$(jq -r '.childType // "basic-flow"' <<< "$cr")" \
            '{alias:$a, description:$d, type:$t, provider:"registration-page-form"}')" > /dev/null
    sub_id="$(kc get "authentication/flows/$parent/executions" -r "$REALM" | jq -r --arg a "$alias" '.[] | select(.displayName == $a) | .id')"
    [ -n "$sub_id" ] || { echo "subflow $alias not found under $parent" >&2; exit 1; }
    set_requirement "$parent" "$sub_id" "$(jq -r .childRequirement <<< "$cr")"
  fi
  CREATED[$alias]=1

  jq -c '.authenticationExecutions | sort_by(.priority) | .[]' <<< "$cr" | while read -r ex; do
    prov="$(jq -r .authenticator <<< "$ex")"
    id="$(kc create "authentication/flows/$alias/executions/execution" -r "$REALM" -b "$(jq -nc --arg p "$prov" '{provider:$p}')" -i)"
    set_requirement "$alias" "$id" "$(jq -r .requirement <<< "$ex")"
    if [ "$(jq 'has("authenticatorConfig")' <<< "$ex")" = true ]; then
      kc create "authentication/executions/$id/config" -r "$REALM" -b "$(jq -c .authenticatorConfig <<< "$ex")" > /dev/null
    fi
    echo "   $alias: $prov $(jq -r .requirement <<< "$ex")"
  done
done

# --- 3: bind it, read everything back ----------------------------------------
kc update "realms/$REALM" -s "browserFlow=$TOP" > /dev/null
[ "$(kc get "realms/$REALM" | jq -r .browserFlow)" = "$TOP" ] || { echo "FAIL: browserFlow is not $TOP" >&2; exit 1; }

fail=0
echo '{}' > "$HERE/flow-executions.json"
for i in $(seq 0 $((n - 1))); do
  cr="$(jq -c ".[$i]" "$WORK/crs.json")"
  alias="$(jq -r .alias <<< "$cr")"
  got="$(execs "$alias")"
  jq --arg a "$alias" --argjson g "$got" '. + {($a): $g}' "$HERE/flow-executions.json" > "$WORK/fe.json" && mv "$WORK/fe.json" "$HERE/flow-executions.json"
  # The children (subflows) the CR does not list as executions come back too.
  want="$(jq -c '[.authenticationExecutions | sort_by(.priority) | .[] | {p: .authenticator, r: .requirement}]' <<< "$cr")"
  have="$(jq -c '[.[] | select(.authenticationFlow != true) | {p: .providerId, r: .requirement}]' <<< "$got")"
  if [ "$want" = "$have" ]; then
    echo "   ok   $alias executions match the chart: $have"
  else
    echo "   FAIL $alias: chart wants $want, realm has $have" >&2; fail=1
  fi
  # Authenticator configs round-trip.
  jq -c '.authenticationExecutions[] | select(has("authenticatorConfig")) | .authenticatorConfig' <<< "$cr" | while read -r ac; do
    cid="$(jq -r --arg p "$(jq -r --argjson a "$ac" '.authenticationExecutions[] | select(.authenticatorConfig == $a) | .authenticator' <<< "$cr")" \
      '.[] | select(.providerId == $p) | .authenticationConfig // empty' <<< "$got")"
    [ -n "$cid" ] || { echo "   FAIL $alias: no config bound for $(jq -r .alias <<< "$ac")" >&2; exit 1; }
    gotc="$(kc get "authentication/config/$cid" -r "$REALM" | jq -c '{alias, config: (.config | del(.["kc.config.version"]))}')"
    wantc="$(jq -c '{alias, config}' <<< "$ac")"
    [ "$gotc" = "$wantc" ] || { echo "   FAIL $alias: config $gotc != chart $wantc" >&2; exit 1; }
    echo "   ok   config $(jq -r .alias <<< "$ac") = $(jq -c .config <<< "$ac")"
  done || fail=1
done
[ "$fail" = 0 ] || { echo "the realm's flows differ from the chart's" >&2; exit 1; }
echo "== flow '$TOP' built from the chart (factor=$FACTOR), bound as the realm browser flow"
