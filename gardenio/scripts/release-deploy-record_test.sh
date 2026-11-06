#!/usr/bin/env bash
# release-deploy-record_test.sh -- Unit tests for the deployment
# release-deploy.sh records on the release (rule 9): what it POSTs after a
# verified roll, a rollback and an --atomic rollback, and that a dry run or a
# failed write never changes the outcome. Same stubs and fixtures as
# release-deploy_test.sh (release-deploy_test_harness.sh).
#
# Usage: bash gardenio/scripts/release-deploy-record_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# shellcheck source=gardenio/scripts/release-deploy_test_harness.sh
. "${SCRIPT_DIR}/release-deploy_test_harness.sh"

# ═══════════════════════════════════════════════════════════════════════
# Rule 9: the deployment on the release
# ═══════════════════════════════════════════════════════════════════════

# A verified roll records <calver> on prod at the new helm revision, by the
# caller's own principal (no by in the body).
setup
reaches_drift_check
KD_URL="http://kd.example"
KD_TOKEN="tok"
run_under_test "$CALVER"
assert_rc 0 "rule9: verified roll exits 0"
assert_file_contains "$CURL_CALL_LOG" "http://kd.example/v1/releases/${CALVER}/deployments?project=gardenio" "rule9: POSTs the deployment of the rolled tag"
DEPLOY_BODY="$(grep '"target":' "$CURL_BODY_LOG" || true)"
if [ "$(jq -c . <<<"$DEPLOY_BODY" 2>/dev/null)" = "$(jq -nc --arg t "$CALVER" '{target:"prod",via:"script",revision:"5",image_tag:$t}')" ]; then
  pass "rule9: body is target prod, via script, the helm revision and the tag"
else
  fail "rule9: body is target prod, via script, the helm revision and the tag" "got: ${DEPLOY_BODY}"
fi
assert_out_contains "recorded deployment: ${CALVER} on prod at helm revision 5" "rule9: the run log says what it recorded"
teardown

# Verify fails and the rollback lands: the tag that ran before is recorded at
# the rollback's revision, by=rollback; nothing for the failed tag.
setup
reaches_drift_check
printf 'gardenio-controller-abc\tregistry.example/gardenio/controller:2026.62.4\n' > "$KUBECTL_PODS_FILE"
KD_URL="http://kd.example"
KD_TOKEN="tok"
run_under_test "$CALVER"
assert_rc 1 "rule9: verify failure still exits 1"
assert_file_contains "$CURL_CALL_LOG" "/v1/releases/2026.62.4/deployments?project=gardenio" "rule9: rollback records the previous tag"
assert_file_not_contains "$CURL_CALL_LOG" "/v1/releases/${CALVER}/deployments" "rule9: the failed tag gets no deployment"
assert_file_contains "$CURL_BODY_LOG" '"by":"rollback"' "rule9: the rollback row is by=rollback"
teardown

# --atomic rolled back to a deployed revision: same, at that revision.
setup
reaches_drift_check
HELM_UPGRADE_EXIT=1
HELM_STATUS_POST_FILE="${TMP}/helm-status-post.json"
printf '{"version":7,"info":{"status":"deployed"}}' > "$HELM_STATUS_POST_FILE"
KD_URL="http://kd.example"
KD_TOKEN="tok"
KD_OIDC_TOKEN_URL="http://kc.example/realms/gardenio/protocol/openid-connect/token"
KD_OIDC_CLIENT_ID="gardenio-release-deployer"
KD_OIDC_CLIENT_SECRET_FILE="${TMP}/secret"
printf 'sup3r-secret' > "$KD_OIDC_CLIENT_SECRET_FILE"
run_under_test "$CALVER"
assert_rc 1 "rule9: atomic rollback still exits 1"
assert_file_contains "$CURL_CALL_LOG" "/v1/releases/2026.62.4/deployments?project=gardenio" "rule9: atomic rollback records the previous tag"
assert_file_contains "$CURL_BODY_LOG" '"revision":"7"' "rule9: at the revision --atomic left deployed"
assert_file_contains "$CURL_CALL_LOG" "Authorization: Bearer machine-jwt" "rule9: the deployment rides the machine principal"
teardown

# The release-deployer reports nothing for a dry run, and a refused or failed
# write never changes the exit code.
setup
reaches_drift_check
KD_URL="http://kd.example"
run_under_test "$CALVER" --dry-run
assert_rc 0 "rule9: dry run exits 0"
assert_file_not_contains "$CURL_CALL_LOG" "/v1/releases/" "rule9: a dry run records no deployment"
teardown

setup
reaches_drift_check
KD_URL="http://kd.example"
CURL_FAIL=1
run_under_test "$CALVER"
assert_rc 0 "rule9: a failed deployment write leaves exit 0"
assert_out_contains "WARN: failed to record the deployment of ${CALVER}" "rule9: a failed write is logged"
teardown

# ═══════════════════════════════════════════════════════════════════════

echo ""
echo "== ${PASS} passed, ${FAIL} failed =="
[ "$FAIL" -eq 0 ]
