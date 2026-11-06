#!/usr/bin/env bash
# release-deploy-edge_test.sh -- rule 4c of release-deploy.sh (gd-WHBtmG7K3Z):
# the edge manifests (gardenio-ziti/public-edge.yaml, viewer-authproxy.yaml)
# ride the release. The stubs and fixtures are release-deploy_test_harness.sh's;
# its kubectl stub keeps a fake cluster under $EDGE_FIX (live ConfigMaps,
# Running pods, `exec` for cat / nginx -t / reload).
#
# Usage: bash gardenio/scripts/release-deploy-edge_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# shellcheck source=gardenio/scripts/release-deploy_test_harness.sh
. "${SCRIPT_DIR}/release-deploy_test_harness.sh"

# line_of <file> <fixed-string> -- the first line number holding it (0: none).
line_of() {
  local n
  n="$(grep -nF -- "$2" "$1" | head -n 1 | cut -d: -f1)"
  printf '%s' "${n:-0}"
}

# edge_changed -- the tag changes public-edge's nginx config.
edge_changed() {
  edge_manifest public-edge public-edge default.conf 'server { listen 8443; # new }'
}

# Unchanged: both edges are applied, nothing is tested or reloaded.
setup
reaches_drift_check
run_under_test "$CALVER"
assert_rc 0 "rule4c: unchanged edges -> exit 0"
assert_out_contains "edge public-edge: nginx ConfigMap unchanged; no test, no reload" "rule4c: public-edge unchanged is logged"
assert_out_contains "edge viewer-authproxy: nginx ConfigMap unchanged" "rule4c: viewer-authproxy unchanged is logged"
assert_file_contains "$KUBECTL_CALL_LOG" "kubectl apply --server-side --field-manager=release-deploy --force-conflicts -f -" "rule4c: exact apply argv (server-side, from stdin)"
assert_file_contains "$KUBECTL_CALL_LOG" "kubectl applied Deployment" "rule4c: the rest of the manifest is applied"
assert_file_not_contains "$KUBECTL_CALL_LOG" "kubectl exec" "rule4c: unchanged config is never tested or reloaded"
assert_file_contains "$KUBECTL_CALL_LOG" "kubectl get configmap viewer-authproxy -n gardenio -o json" "rule4c: the live ConfigMap is read"
teardown

# Changed: wait for the mount, nginx -t, reload, and only then the Deployment.
setup
reaches_drift_check
edge_changed
run_under_test "$CALVER"
assert_rc 0 "rule4c: changed config that tests ok -> exit 0"
assert_file_contains "$KUBECTL_CALL_LOG" "kubectl exec -n public-edge public-edge-7d9f -c nginx -- cat /etc/nginx/conf.d/default.conf" "rule4c: waits on the pod's mounted file"
assert_file_contains "$KUBECTL_CALL_LOG" "kubectl exec -n public-edge public-edge-7d9f -c nginx -- nginx -t" "rule4c: nginx -t in the pod"
assert_file_contains "$KUBECTL_CALL_LOG" "kubectl exec -n public-edge public-edge-7d9f -c nginx -- nginx -s reload" "rule4c: reload in the pod"
assert_file_contains "$KUBECTL_CALL_LOG" 'kubectl get pods -n public-edge -l app.kubernetes.io/name=public-edge' "rule4c: pods by the edge's selector"
T="$(line_of "$KUBECTL_CALL_LOG" "nginx -t")"
R="$(line_of "$KUBECTL_CALL_LOG" "nginx -s reload")"
D="$(line_of "$KUBECTL_CALL_LOG" "kubectl applied Deployment")"
U="$(line_of "$HELM_CALL_LOG" "helm upgrade")"
if [ "$T" -gt 0 ] && [ "$T" -lt "$R" ] && [ "$R" -lt "$D" ] && [ "$U" -gt 0 ]; then
  pass "rule4c: test, then reload, then the Deployment, then the upgrade"
else
  fail "rule4c: order" "test=${T} reload=${R} deployment=${D} upgrade=${U}"
fi
assert_file_not_contains "$KUBECTL_CALL_LOG" "-n gardenio viewer-authproxy-a -c nginx -- nginx -s reload" "rule4c: an unchanged edge beside it is not reloaded"
teardown

# viewer-authproxy: re-render the envsubst template in every pod, then reload.
setup
reaches_drift_check
edge_manifest viewer-authproxy gardenio default.conf.template 'server { listen 8097; # new }'
run_under_test "$CALVER"
assert_rc 0 "rule4c: viewer change -> exit 0"
assert_file_contains "$KUBECTL_CALL_LOG" "-n gardenio viewer-authproxy-a -c nginx -- cat /etc/nginx/templates/default.conf.template" "rule4c: viewer waits on the template mount"
assert_file_contains "$KUBECTL_CALL_LOG" "-n gardenio viewer-authproxy-b -c nginx -- sh -c rm -rf /tmp/release-deploy-conf.d && cp -a /etc/nginx/conf.d /tmp/release-deploy-conf.d && /docker-entrypoint.d/20-envsubst-on-templates.sh >/dev/null && nginx -t" "rule4c: viewer renders the template then tests, in each pod"
assert_file_contains "$KUBECTL_CALL_LOG" "-n gardenio viewer-authproxy-b -c nginx -- nginx -s reload" "rule4c: every viewer pod reloads"
teardown

# A failed test rolls the ConfigMap back: nothing reloaded, the rest of the
# manifest untouched, no upgrade, the live ConfigMap as before.
setup
reaches_drift_check
edge_changed
EDGE_NGINX_T_FAIL=public-edge
KD_URL="http://kd.example"
run_under_test "$CALVER"
assert_rc 1 "rule4c: nginx -t fails -> exit 1"
assert_out_contains "edge public-edge: nginx -t failed in public-edge-7d9f; ConfigMap rolled back" "rule4c: output names the failed test and the rollback"
assert_out_contains 'unknown directive "bogus"' "rule4c: output carries nginx's error"
assert_file_contains "${EDGE_FIX}/live/public-edge_public-edge.json" 'listen 8443; }' "rule4c: the live ConfigMap is back to the old config"
assert_file_not_contains "${EDGE_FIX}/live/public-edge_public-edge.json" 'resourceVersion' "rule4c: the rollback re-applies data, not server fields"
assert_file_not_contains "$KUBECTL_CALL_LOG" "nginx -s reload" "rule4c: a failed test never reloads"
assert_file_not_contains "$KUBECTL_CALL_LOG" "kubectl applied Deployment" "rule4c: a failed test never applies the Deployment"
assert_file_not_contains "$HELM_CALL_LOG" "upgrade" "rule4c: a failed test stops the release"
assert_file_contains "$CURL_BODY_LOG" '"type":"bug"' "rule4c: a failed test files a bug"
teardown

# viewer-authproxy's failed test also restores each tested pod's conf.d.
setup
reaches_drift_check
edge_manifest viewer-authproxy gardenio default.conf.template 'server { bogus; }'
EDGE_NGINX_T_FAIL=gardenio
run_under_test "$CALVER"
assert_rc 1 "rule4c: viewer nginx -t fails -> exit 1"
assert_file_contains "$KUBECTL_CALL_LOG" "-n gardenio viewer-authproxy-a -c nginx -- sh -c rm -f /etc/nginx/conf.d/* && cp -a /tmp/release-deploy-conf.d/. /etc/nginx/conf.d/" "rule4c: the tested pod's conf.d is restored"
assert_file_contains "${EDGE_FIX}/live/gardenio_viewer-authproxy.json" 'listen 8097; }' "rule4c: viewer ConfigMap rolled back"
teardown

# The rollback apply itself failing is manual intervention.
setup
reaches_drift_check
edge_changed
EDGE_NGINX_T_FAIL=public-edge
# Only the rollback carries the old config.
KUBECTL_APPLY_FAIL_MATCH='"default.conf":"server { listen 8443; }"'
run_under_test "$CALVER"
assert_rc 2 "rule4c: failed test and failed ConfigMap rollback -> exit 2"
assert_out_contains "the ConfigMap rollback failed -- manual intervention needed" "rule4c: output asks for manual intervention"
teardown

# A pod that never mounts the new file is a failed test (bounded wait).
setup
reaches_drift_check
edge_changed
EDGE_EXEC_STALE=1
EDGE_SYNC_TIMEOUT=0
run_under_test "$CALVER"
assert_rc 1 "rule4c: mount never updates -> exit 1"
assert_out_contains "public-edge-7d9f did not mount the new default.conf within 0s; ConfigMap rolled back" "rule4c: output names the stale mount"
assert_file_not_contains "$KUBECTL_CALL_LOG" "nginx -t" "rule4c: never tests a stale file"
teardown

# No running pod: nothing to test the config in -> roll back.
setup
reaches_drift_check
edge_changed
: > "${EDGE_FIX}/pods-public-edge.txt"
run_under_test "$CALVER"
assert_rc 1 "rule4c: no running edge pod -> exit 1"
assert_out_contains "no running pod to test the config in; ConfigMap rolled back" "rule4c: output names the missing pod"
teardown

# The reload failing after a passed test: exit 1, the tested config stays.
setup
reaches_drift_check
edge_changed
EDGE_RELOAD_FAIL=public-edge
run_under_test "$CALVER"
assert_rc 1 "rule4c: reload fails -> exit 1"
assert_out_contains "nginx reload failed in public-edge-7d9f (the tested config stays mounted)" "rule4c: output names the reload failure"
assert_file_contains "${EDGE_FIX}/live/public-edge_public-edge.json" '# new' "rule4c: a tested config is not rolled back"
teardown

# Fail closed before anything is applied for that edge.
setup
reaches_drift_check
EDGE_CM_GET_FAIL=1
run_under_test "$CALVER"
assert_rc 1 "rule4c: live ConfigMap unreadable -> exit 1"
assert_out_contains "cannot read the live ConfigMap public-edge/public-edge" "rule4c: output names the unreadable ConfigMap"
assert_file_not_contains "$KUBECTL_APPLY_LOG" '"kind":"List"' "rule4c: nothing edge-side applied"
teardown

setup
reaches_drift_check
rm "${REPO}/gardenio/helm/gardenio-ziti/viewer-authproxy.yaml"
run_under_test "$CALVER"
assert_rc 1 "rule4c: manifest missing from the checkout -> exit 1"
assert_out_contains "edge manifest missing from the checkout" "rule4c: output names the missing manifest"
teardown

setup
reaches_drift_check
jq -nc '{apiVersion: "apps/v1", kind: "Deployment", metadata: {name: "public-edge", namespace: "public-edge"}}' \
  > "${REPO}/gardenio/helm/gardenio-ziti/public-edge.yaml.json"
run_under_test "$CALVER"
assert_rc 1 "rule4c: manifest without the nginx ConfigMap -> exit 1"
assert_out_contains "has no ConfigMap public-edge/public-edge" "rule4c: output names the missing ConfigMap"
teardown

setup
reaches_drift_check
edge_changed
KUBECTL_APPLY_FAIL_MATCH='# new'
run_under_test "$CALVER"
assert_rc 1 "rule4c: ConfigMap apply refused -> exit 1"
assert_out_contains "edge public-edge: ConfigMap apply failed" "rule4c: output names the refused apply"
assert_file_not_contains "$KUBECTL_CALL_LOG" "kubectl exec" "rule4c: a refused apply is never tested"
teardown

# --dry-run: each whole manifest is a server-side dry run; no live read, no exec.
setup
reaches_drift_check
edge_changed
run_under_test "$CALVER" --dry-run
assert_rc 0 "rule4c: dry run -> exit 0"
assert_file_contains "$KUBECTL_CALL_LOG" "kubectl apply --server-side --field-manager=release-deploy --force-conflicts -f - --dry-run=server" "rule4c: dry run applies with --dry-run=server"
assert_file_contains "$KUBECTL_CALL_LOG" "kubectl applied ConfigMap,Deployment" "rule4c: dry run sends the whole manifest at once"
assert_file_not_contains "$KUBECTL_CALL_LOG" "kubectl exec" "rule4c: dry run never tests or reloads"
assert_file_not_contains "$KUBECTL_CALL_LOG" "get configmap public-edge" "rule4c: dry run never reads the live ConfigMap"
assert_file_contains "${EDGE_FIX}/live/public-edge_public-edge.json" 'listen 8443; }' "rule4c: dry run changes nothing"
teardown

# The manifests front namespace gardenio; another release skips the rule.
setup
reaches_drift_check
edge_changed
run_under_test "$CALVER" --namespace gardenio-staging
assert_rc 0 "rule4c: other namespace -> exit 0"
assert_out_contains "edge manifests: they front namespace gardenio, not gardenio-staging; skipped" "rule4c: output says why it skipped"
assert_file_not_contains "$KUBECTL_CALL_LOG" "-f - --dry-run" "rule4c: other namespace applies no edge"
assert_file_not_contains "$KUBECTL_CALL_LOG" "kubectl exec" "rule4c: other namespace tests nothing"
teardown

# A skipped run (any gate) never touches the edge.
setup
KUBECTL_PAUSE_EXISTS=1
edge_changed
run_under_test "$CALVER"
assert_rc 3 "rule4c: paused -> exit 3"
assert_file_not_contains "$KUBECTL_CALL_LOG" "kubectl applied" "rule4c: paused run applies nothing"
teardown

echo ""
echo "== ${PASS} passed, ${FAIL} failed =="
[ "$FAIL" -eq 0 ]
