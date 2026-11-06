#!/usr/bin/env bash
set -euo pipefail

# verify-land-scopes-vap.sh — live acceptance of the land-scopes
# ValidatingAdmissionPolicy (gd-EXIoC2EQs3 checks 0-3).
#
# templates/controller/lands-scope-rbac.yaml renders, once
# agents.lands.provisioner.enabled is on, a Role that lets the controller
# ServiceAccount create KeycloakClientScope resources and a
# ValidatingAdmissionPolicy that holds every such write to the land scope
# shape. The policy is the only thing between the controller and a
# hardcoded realm_roles claim mapper (which kbeads trusts), so it is proved
# live in the same helm window that turns the provisioner on.
#
# Every write here is `kubectl create --dry-run=server`, impersonating the
# controller SA: admission runs, nothing is persisted. The caller needs
# cluster-admin (impersonate + get on the policy objects).
#
# Usage:
#   gardenio/scripts/verify-land-scopes-vap.sh                 # release gardenio, ns gardenio
#   gardenio/scripts/verify-land-scopes-vap.sh --namespace=X --release=Y --land=z
#
# Prints every check as "command" then its last output line, in the form
# gd-EXIoC2EQs3 asks to paste onto the bead, then PASS/FAIL per check.
# Exit 0 only when every check passed.
#
# Offline, the same policy is proved by the chart test
# TestLandScopes_AdmissionPolicyCEL (gardenio/helm/gardenio/tests); this
# script is the live counterpart against the real apiserver.

NAMESPACE="${NAMESPACE:-gardenio}"
RELEASE="${RELEASE:-gardenio}"
LAND="${LAND:-x}"

for arg in "$@"; do
  case "$arg" in
    --namespace=*) NAMESPACE="${arg#*=}" ;;
    --release=*) RELEASE="${arg#*=}" ;;
    --land=*) LAND="${arg#*=}" ;;
    -h|--help) sed -n '3,30p' "$0"; exit 0 ;;
    *) echo "Unknown flag: $arg" >&2; exit 2 ;;
  esac
done

# The chart's names: gardenio.agents.fullname is <fullname>-agents, the SA
# is <agents>-sa, the realm resource <fullname>-realm.
SA="system:serviceaccount:${NAMESPACE}:${RELEASE}-agents-sa"
POLICY="${RELEASE}-agents-land-scopes"
REALM="${RELEASE}-realm"
RESOURCE="keycloakclientscopes.v1.edp.epam.com"
DENIED="ValidatingAdmissionPolicy '${POLICY}' with binding '${POLICY}' denied request"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

pass=0
fail=0

# run NAME EXPECT_RE FORBID_RE CMD... : runs CMD, prints it (or SHOW, when
# set) and its last output line, PASS when that line matches EXPECT_RE and
# not FORBID_RE.
run() {
  local name="$1" expect="$2" forbid="$3"; shift 3
  local out last
  out="$("$@" 2>&1)" || true
  last="$(printf '%s\n' "$out" | grep -v '^$' | tail -n 1)"
  printf '\n$ %s\n%s\n' "${SHOW:-$*}" "$last"
  SHOW=""
  if [[ "$last" =~ $expect ]] && { [[ -z "$forbid" ]] || ! [[ "$last" =~ $forbid ]]; }; then
    echo "PASS $name"; pass=$((pass + 1))
  else
    echo "FAIL $name (wanted /$expect/${forbid:+ and not /$forbid/})"; fail=$((fail + 1))
  fi
}

# scope FILE [EDIT] writes the control KeycloakClientScope (exactly what
# landscope.Render(LAND, NAMESPACE, REALM) writes) to FILE, with one
# hostile EDIT applied: no-mapper, two-mappers, hardcoded-claim,
# client-audience, other-land-name.
scope() {
  local file="$1" edit="${2:-}"
  local spec_name="land-${LAND}" audience="land-${LAND}" mappers
  case "$edit" in
    other-land-name) spec_name="land-other" ;;
    client-audience) audience="gardenio-cli" ;;
  esac
  local mapper="    - name: land-audience
      protocol: openid-connect
      protocolMapper: oidc-audience-mapper
      config: {included.custom.audience: ${audience}, access.token.claim: \"true\", id.token.claim: \"false\", introspection.token.claim: \"true\"}"
  case "$edit" in
    no-mapper) mappers="  protocolMappers: []" ;;
    two-mappers) mappers="  protocolMappers:
${mapper}
${mapper}" ;;
    hardcoded-claim) mappers="  protocolMappers:
    - name: land-audience
      protocol: openid-connect
      protocolMapper: oidc-hardcoded-claim-mapper
      config: {claim.name: realm_roles, claim.value: admin, access.token.claim: \"true\"}" ;;
    *) mappers="  protocolMappers:
${mapper}" ;;
  esac
  cat >"$file" <<EOF
apiVersion: v1.edp.epam.com/v1
kind: KeycloakClientScope
metadata:
  name: gardenio-land-scope-${LAND}
  namespace: ${NAMESPACE}
  labels: {gardenio.io/land: ${LAND}, gardenio.io/land-scope: "true", app.kubernetes.io/managed-by: gardenio-controller}
  annotations: {edp.epam.com/preserve-resources-on-deletion: "true"}
spec:
  name: ${spec_name}
  realmRef: {name: ${REALM}, kind: KeycloakRealm}
  protocol: openid-connect
  type: optional
  description: Mints a token for the land ${LAND} alone (aud land-${LAND})
  attributes: {gardenio_land: ${LAND}, include.in.token.scope: "true", display.on.consent.screen: "false"}
${mappers}
EOF
}

# Every write under test is a server dry run as the controller SA, run from
# WORK so the record names control.yaml, no-mapper.yaml, ... as the bead does.
DRYRUN=(kubectl -n "$NAMESPACE" create --dry-run=server --as="$SA" -f)
cd "$WORK"

echo "land-scopes VAP acceptance: namespace=${NAMESPACE} release=${RELEASE} land=${LAND}"
echo "SA=${SA} policy=${POLICY} realm=${REALM}"

echo
echo "## 0. Preconditions"
SHOW='kubectl version -o json   # serverVersion.minor' \
run "0a server 1.30+ (VAP v1)" '^(30|[3-9][1-9]|[4-9][0-9])$' '' \
  bash -c 'kubectl version -o json 2>/dev/null | sed -n "s/.*\"minor\": *\"\([0-9]*\).*/\1/p" | head -n1'
run "0b CRD ${RESOURCE}" "^customresourcedefinition.*${RESOURCE}" '' \
  kubectl get crd "$RESOURCE" -o name

echo
echo "## 1. Objects (proves a later denial is the VAP's, not RBAC's)"
run "1a policy exists" "^validatingadmissionpolicy.*/${POLICY}$" '' \
  kubectl get validatingadmissionpolicy "$POLICY" -o name
run "1b binding denies" '^\["Deny"\]$' '' \
  kubectl get validatingadmissionpolicybinding "$POLICY" -o jsonpath='{.spec.validationActions}'
run "1c role exists" "^role.*/${POLICY}$" '' \
  kubectl -n "$NAMESPACE" get role "$POLICY" -o name
run "1d rolebinding exists" "^rolebinding.*/${POLICY}$" '' \
  kubectl -n "$NAMESPACE" get rolebinding "$POLICY" -o name
run "1e SA can create (RBAC grants it)" '^yes$' '' \
  kubectl auth can-i create "$RESOURCE" -n "$NAMESPACE" --as="$SA"

echo
echo "## 2. Control: exactly what landscope.Render writes (must PASS admission)"
scope control.yaml
run "2 control created (server dry run)" 'created \(server dry run\)$' 'denied request|[Ff]orbidden' \
  "${DRYRUN[@]}" control.yaml

echo
echo "## 3. Hostile variants (each DENIED by the VAP, never Forbidden by RBAC)"
MAPPER_MSG="exactly one oidc-audience-mapper"
NAME_MSG="spec.name must be land-<land>"
# The apiserver reports only the FIRST failing validation; the policy's
# rule order puts the land/spec.name rule first, the mapper rule last.
for v in "a no-mapper ${MAPPER_MSG}" "b two-mappers ${MAPPER_MSG}" "c hardcoded-claim ${MAPPER_MSG}" "d client-audience ${MAPPER_MSG}" "e other-land-name ${NAME_MSG}"; do
  read -r letter edit msg <<<"$v"
  scope "$edit.yaml" "$edit"
  run "3${letter} ${edit} denied" "${DENIED}.*${msg}" '[Ff]orbidden|cannot create resource' \
    "${DRYRUN[@]}" "$edit.yaml"
done

echo
echo "## 4. Sanity note (not a check): the same hostile create as the caller (no --as)"
echo "   is not matched by the policy (matchConditions only-controller-sa). By design."

echo
echo "RESULT: ${pass} passed, ${fail} failed"
echo "Paste the blocks above onto gd-EXIoC2EQs3 and gd-QsUNbShM2J; a FAIL is a bug under Fences gd-Gx6arHBvzJ."
[[ "$fail" -eq 0 ]]
