#!/usr/bin/env bash
# release-deploy_test.sh -- Unit tests for release-deploy.sh.
#
# `helm`, `kubectl`, `gh`, and `curl` are fully stubbed (no cluster, no
# network, no real GitHub/beads calls). Each stub logs its full argv to a
# per-test log file, which is what lets the upgrade/rollback-argv and
# POST-body assertions below check exactly what was run. Fixture files (JSON
# blobs the stubs `cat`) drive their canned responses; env vars flip
# fail/success paths without touching the fixtures.
#
# `yq` and `python3` (the two YAML->JSON converters release-deploy.sh tries,
# in order) are ALSO stubbed rather than delegated to the real tools -- this
# suite is testing our own drift-detection logic given known JSON on both
# sides, not real YAML parsing, and stubbing keeps the "neither is present"
# branch exercisable regardless of what happens to be installed on the box
# running the suite. PATH is pointed at a curated bin/ directory that holds
# ONLY symlinks to the real tools release-deploy.sh actually needs (git, jq,
# sort, diff, grep, sed, cut, paste, tail, mktemp, ...) plus our stubs, so a
# test that omits the yq/python3 stub genuinely can't find them, whatever is
# installed on the host running the suite.
#
# Baseline fixtures (see setup()): a healthy release one revision behind the
# target calver, with NO drift once global.imageTag is set aside (live
# values carry only {global:{imageTag:...}}, the overlay only {} -- both
# reduce to zero scalar leaves), a green images.yml run, and pods already on
# the target tag (so a real upgrade+verify passes end to end). Each rule's
# tests override only the fixture(s) that rule cares about.
#
# The stubs, setup/teardown and asserts live in release-deploy_test_harness.sh,
# shared with release-deploy-record_test.sh (rule 9, the deployment recorded
# on the release), which is split out to keep each file under the quench cloc
# ceiling.
#
# Usage: bash gardenio/scripts/release-deploy_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# shellcheck source=gardenio/scripts/release-deploy_test_harness.sh
. "${SCRIPT_DIR}/release-deploy_test_harness.sh"

# ═══════════════════════════════════════════════════════════════════════
# Rule 1: pause switch
# ═══════════════════════════════════════════════════════════════════════

setup
KUBECTL_PAUSE_EXISTS=1
run_under_test "$CALVER"
assert_rc 3 "rule1: paused configmap present -> exit 3"
assert_out_contains "paused" "rule1: output names the reason"
assert_file_not_contains "$HELM_CALL_LOG" "status" "rule1: never even queries helm once paused"
teardown

# Probe (fix 1): a kubectl failure that is NOT NotFound (API unreachable,
# RBAC) must not be read as "not paused" -> exit 1, and never reach helm.
setup
reaches_drift_check  # so an open gate would reach upgrade, not stop at 3b
KUBECTL_PAUSE_ERR='The connection to the server 10.0.0.1:6443 was refused - did you specify the right host or port?'
KD_URL="http://kd.example"
run_under_test "$CALVER"
assert_rc 1 "rule1: pause probe fails (API unreachable) -> exit 1, fail closed"
assert_out_contains "cannot check pause switch" "rule1: output names the pause-probe failure"
assert_file_not_contains "$HELM_CALL_LOG" "upgrade" "rule1: pause probe failure never upgrades"
assert_file_contains "$CURL_BODY_LOG" '"type":"bug"' "rule1: pause probe failure files a bug bead"
teardown

setup
reaches_drift_check  # so an open gate would reach upgrade, not stop at 3b
KUBECTL_PAUSE_ERR='Error from server (Forbidden): configmaps "release-deployer-pause" is forbidden: User "system:serviceaccount:gardenio:release-deployer" cannot get resource "configmaps"'
run_under_test "$CALVER"
assert_rc 1 "rule1: pause probe forbidden (RBAC) -> exit 1, fail closed"
teardown

# ═══════════════════════════════════════════════════════════════════════
# Calver validation (fix 5)
# ═══════════════════════════════════════════════════════════════════════

setup
reaches_drift_check
run_under_test "2026.99.1,global.foo=bar"
assert_rc 1 "calver: injection attempt '2026.99.1,global.foo=bar' -> exit 1"
assert_file_not_contains "$HELM_CALL_LOG" "upgrade" "calver: injection never reaches helm upgrade"
teardown

for bad in "2026.99" "v2026.99.1" "26.1.1" "2026.1.1 " "2026.1.1-rc1"; do
  setup
  reaches_drift_check  # so an open gate would reach upgrade, not stop at 3b
  run_under_test "$bad"
  assert_rc 1 "calver: malformed '${bad}' -> exit 1"
  assert_file_not_contains "$HELM_CALL_LOG" "upgrade" "calver: malformed '${bad}' never reaches helm upgrade"
  teardown
done

# ═══════════════════════════════════════════════════════════════════════
# Rule 2: helm busy
# ═══════════════════════════════════════════════════════════════════════

setup
printf '{"version":5,"info":{"status":"pending-upgrade"}}' > "$HELM_STATUS_FILE"
run_under_test "$CALVER"
assert_rc 3 "rule2: helm status pending-upgrade -> exit 3"
assert_out_contains "helm busy: pending-upgrade" "rule2: output names the live status"
teardown

# A `failed` release (an upgrade whose --atomic rollback also failed, as on
# 2026-11-05) is rolled back to its newest completed revision, not skipped
# forever as busy; the run stops there without upgrading.
hist_fixture() {
  HELM_HISTORY_FILE="${TMP}/helm-history.json"
  cat > "$HELM_HISTORY_FILE" <<'JSON'
[{"revision":285,"status":"superseded","app_version":"2026.309.16","description":"Upgrade complete"},
 {"revision":286,"status":"superseded","app_version":"2026.309.17","description":"Upgrade complete"},
 {"revision":287,"status":"superseded","app_version":"2026.309.18","description":"Upgrade \"gardenio\" failed: no endpoints available"},
 {"revision":288,"status":"failed","app_version":"2026.309.17","description":"Rollback \"gardenio\" failed: no endpoints available"}]
JSON
}

setup
printf '{"version":288,"info":{"status":"failed"}}' > "$HELM_STATUS_FILE"
hist_fixture
KD_URL="http://kd.example"
run_under_test "$CALVER"
assert_rc 1 "rule2: failed release -> rolled back, exit 1"
assert_file_contains "$HELM_CALL_LOG" "helm rollback gardenio 286 " "rule2: rolls back to the newest completed revision (286), not the failed 287/288"
assert_file_not_contains "$HELM_CALL_LOG" "upgrade" "rule2: no upgrade in the run that heals"
assert_out_contains "rolled back to revision 286 (2026.309.17)" "rule2: output names the revision and its tag"
assert_file_contains "$CURL_BODY_LOG" '"image_tag":"2026.309.17"' "rule2: the rollback is recorded as a deployment of 286's tag"
teardown

setup
printf '{"version":288,"info":{"status":"failed"}}' > "$HELM_STATUS_FILE"
hist_fixture
HELM_ROLLBACK_EXIT=1
run_under_test "$CALVER"
assert_rc 2 "rule2: failed release and the rollback fails too -> exit 2"
assert_out_contains "manual intervention" "rule2: a failed heal says manual intervention is needed"
teardown

setup
printf '{"version":3,"info":{"status":"failed"}}' > "$HELM_STATUS_FILE"
HELM_HISTORY_FILE="${TMP}/helm-history.json"
printf '[{"revision":2,"status":"superseded","app_version":"x","description":"Upgrade \"gardenio\" failed"},{"revision":3,"status":"failed","app_version":"x","description":"Rollback failed"}]' > "$HELM_HISTORY_FILE"
run_under_test "$CALVER"
assert_rc 2 "rule2: failed release with no completed revision in history -> exit 2"
assert_file_not_contains "$HELM_CALL_LOG" "rollback" "rule2: nothing to roll back to, no rollback call"
teardown

setup
printf '{"version":288,"info":{"status":"failed"}}' > "$HELM_STATUS_FILE"
HELM_HISTORY_FAIL=1
run_under_test "$CALVER"
assert_rc 2 "rule2: failed release and helm history errors -> exit 2"
teardown

setup
printf '{"version":288,"info":{"status":"failed"}}' > "$HELM_STATUS_FILE"
hist_fixture
run_under_test "$CALVER" --dry-run
assert_rc 3 "rule2: failed release under --dry-run -> exit 3, nothing changed"
assert_file_not_contains "$HELM_CALL_LOG" "rollback" "rule2: dry run never rolls back"
teardown

# Release missing is an error for this script (it only upgrades), not a skip.
setup
HELM_STATUS_FAIL=1
HELM_STATUS_ERR='Error: release: not found'
run_under_test "$CALVER"
assert_rc 1 "rule2: release not found -> exit 1 (the release must exist)"
assert_out_contains "not found" "rule2: output says the release is missing"
teardown

# Probe (fix 2): cluster unreachable / RBAC must never become an exit-3 skip.
setup
HELM_STATUS_FAIL=1
HELM_STATUS_ERR='Error: Kubernetes cluster unreachable: Get "https://10.0.0.1:6443/version": dial tcp: i/o timeout'
KD_URL="http://kd.example"
run_under_test "$CALVER"
assert_rc 1 "rule2: helm status fails (cluster unreachable) -> exit 1, not a skip"
assert_file_contains "$CURL_BODY_LOG" '"type":"bug"' "rule2: helm status error files a bug bead"
teardown

setup
HELM_STATUS_FAIL=1
HELM_STATUS_ERR='Error: query: failed to query with labels: secrets is forbidden: User "system:serviceaccount:gardenio:release-deployer" cannot list resource "secrets"'
run_under_test "$CALVER"
assert_rc 1 "rule2: helm status fails (RBAC forbidden) -> exit 1, not a skip"
teardown

# Probe (fix 3): helm prints junk (not JSON) on stdout -> exit 1 with a
# report, never a set -e crash with jq's rc.
setup
printf 'this is not json' > "$HELM_STATUS_FILE"
KD_URL="http://kd.example"
run_under_test "$CALVER"
assert_rc 1 "fix3: unparseable helm status stdout -> exit 1 (not jq's rc)"
assert_file_contains "$CURL_BODY_LOG" '"type":"bug"' "fix3: unparseable helm status still reports"
teardown

setup
printf 'garbage{' > "$HELM_GET_VALUES_FILE"
run_under_test "$CALVER"
assert_rc 1 "fix3: unparseable helm get values stdout -> exit 1 (not jq's rc)"
teardown

setup
reaches_drift_check
printf '<html>rate limited</html>' > "$GH_RUNS_FILE"
run_under_test "$CALVER"
assert_rc 1 "fix3: unparseable gh api runs -> exit 1, not a silent 'images not ready'"
teardown

# Probe (fix 3): a helm WARN line on stderr next to valid JSON on stdout must
# not break parsing -- the full deploy still succeeds.
setup
reaches_drift_check
HELM_STDERR_WARN=1
run_under_test "$CALVER"
assert_rc 0 "fix3: helm stderr warning alongside valid JSON -> still deploys (exit 0)"
teardown

# ═══════════════════════════════════════════════════════════════════════
# Rule 3: not newer
# ═══════════════════════════════════════════════════════════════════════

setup
printf '{"global":{"imageTag":"%s"}}' "$CALVER" > "$HELM_GET_VALUES_FILE"
run_under_test "$CALVER"
assert_rc 3 "rule3: target equals current tag -> exit 3 not newer"
assert_out_contains "not newer" "rule3 (equal): output names the reason"
teardown

setup
printf '{"global":{"imageTag":"2026.90.1"}}' > "$HELM_GET_VALUES_FILE"
run_under_test "$CALVER"
assert_rc 3 "rule3: target older than current tag -> exit 3 not newer"
teardown

setup
reaches_drift_check
printf '{}' > "$HELM_GET_VALUES_FILE"
run_under_test "$CALVER"
assert_rc 0 "rule3: no current imageTag at all -> newer-check is skipped, deploy proceeds"
teardown

# ═══════════════════════════════════════════════════════════════════════
# Rule 3b: values drift
# ═══════════════════════════════════════════════════════════════════════

# Positive control: the live release carries a key (replicaCount) the
# overlay values file doesn't have at all -- this MUST be flagged as drift.
setup
make_yq_stub "$BIN"
printf '{"global":{"imageTag":"2026.62.4"},"replicaCount":3}' > "$HELM_GET_VALUES_FILE"
printf '{}' > "$OVERLAY_JSON"
run_under_test "$CALVER"
assert_rc 3 "rule3b: live-only key (positive control) -> exit 3 values drift"
assert_out_contains "values drift" "rule3b: output names it as drift"
assert_out_contains "replicaCount" "rule3b: output names the drifted key"
assert_out_contains "values drift: replicaCount" "rule3b: drifted key list has no empty entry"
teardown

# Same values on both sides -> no drift, deploy proceeds past this gate.
setup
make_yq_stub "$BIN"
printf '{"global":{"imageTag":"2026.62.4"},"replicaCount":3}' > "$HELM_GET_VALUES_FILE"
printf '{"replicaCount":3}' > "$OVERLAY_JSON"
run_under_test "$CALVER"
assert_rc 0 "rule3b: identical values (ignoring global.imageTag) -> no drift"
teardown

# yq unavailable, python3+yaml available -> falls back cleanly.
setup
make_python3_stub "$BIN"
printf '{"global":{"imageTag":"2026.62.4"},"replicaCount":3}' > "$HELM_GET_VALUES_FILE"
printf '{"replicaCount":3}' > "$OVERLAY_JSON"
run_under_test "$CALVER"
assert_rc 0 "rule3b: no yq, python3+yaml available -> falls back and finds no drift"
teardown

# Neither yq nor python3 -> fail closed, exit 1, never silently skip.
setup
run_under_test "$CALVER"
assert_rc 1 "rule3b: neither yq nor python3 present -> exit 1 (fail closed)"
assert_out_contains "cannot check values drift" "rule3b: output names the reason"
teardown

# python3 present but without PyYAML -> also fails closed.
setup
make_python3_stub "$BIN"
PY_IMPORT_YAML_EXIT=1
run_under_test "$CALVER"
assert_rc 1 "rule3b: python3 present but 'import yaml' fails -> exit 1 (fail closed)"
teardown

# Drift is reported as a failure (bug bead) even though the exit code is 3.
setup
make_yq_stub "$BIN"
KD_URL="http://kd.example"
KD_TOKEN="tok"
KD_OPERATED_BY="test-agent"
printf '{"global":{"imageTag":"2026.62.4"},"replicaCount":3}' > "$HELM_GET_VALUES_FILE"
printf '{}' > "$OVERLAY_JSON"
run_under_test "$CALVER"
assert_rc 3 "rule3b+8: drift still exits 3"
BUG_LINES="$(grep -c '"type":"bug"' "$CURL_BODY_LOG" 2>/dev/null || true)"
if [ "${BUG_LINES:-0}" -ge 1 ]; then
  pass "rule3b+8: drift on exit 3 still files a type=bug bead"
else
  fail "rule3b+8: drift on exit 3 still files a type=bug bead" "no bug POST found in:
$(cat "$CURL_BODY_LOG" 2>/dev/null)"
fi
teardown

# Rule 3b baseline (P-R1, gd-foTtSpeOrc): live values are compared with the
# overlay at the deployed tag (2026.62.4), so a reviewed overlay change
# applies and only a hand edit parks.

# tag_overlay <repo> <overlay-json> -- commits <overlay-json> as the overlay
# in <repo> and tags it 2026.62.4, the tag the live release runs.
tag_overlay() {
  printf '%s\n' "$2" > "$1/deploy-values/gardenio-k3s.yaml"
  PATH="${BIN}:${PATH}" git -C "$1" commit -q -am "overlay at 2026.62.4"
  PATH="${BIN}:${PATH}" git -C "$1" tag 2026.62.4
}

# Unchanged live values + a changed overlay -> applies the new overlay. The
# tag exists only on origin, as in the deployer's shallow clone, so it is
# fetched.
setup
make_yq_stub "$BIN"
ORIGIN="${TMP}/origin"
PATH="${BIN}:${PATH}" git clone -q "$REPO" "$ORIGIN"
PATH="${BIN}:${PATH}" git -C "$ORIGIN" config user.email t@t
PATH="${BIN}:${PATH}" git -C "$ORIGIN" config user.name t
tag_overlay "$ORIGIN" '{"replicaCount":3}'
PATH="${BIN}:${PATH}" git -C "$REPO" remote add origin "$ORIGIN"
printf '{"global":{"imageTag":"2026.62.4"},"replicaCount":3}' > "$HELM_GET_VALUES_FILE"
printf '{"replicaCount":5}' > "$OVERLAY_JSON"
run_under_test "$CALVER"
assert_rc 0 "rule3b: live equals the deployed tag's overlay, new overlay differs -> deploys"
assert_out_contains "values baseline: the overlay at 2026.62.4" "rule3b: compares with the overlay at the deployed tag"
assert_file_contains "$HELM_CALL_LOG" "-f ${REPO}/deploy-values/gardenio-k3s.yaml --set global.imageTag=${CALVER}" "rule3b: upgrades with the new tag's overlay"
teardown

# Hand-edited live values that differ from both the deployed tag's overlay
# and the new one -> parks with one bug, which carries the short_title
# kbeads requires of a machine-filed bug (gd-440OOVBHi7).
setup
make_yq_stub "$BIN"
KD_URL="http://kd.example"; KD_TOKEN="tok"
tag_overlay "$REPO" '{"replicaCount":3,"image":{"pullPolicy":"Always"}}'
printf '{"global":{"imageTag":"2026.62.4"},"replicaCount":4,"image":{"pullPolicy":"IfNotPresent"}}' > "$HELM_GET_VALUES_FILE"
printf '{"replicaCount":5,"image":{"pullPolicy":"IfNotPresent"}}' > "$OVERLAY_JSON"
run_under_test "$CALVER"
assert_rc 3 "rule3b: live differs from both overlays (hand edit) -> exit 3"
assert_out_contains "values drift: replicaCount" "rule3b: hand edit names the drifted key"
if grep -qx "values drift: replicaCount" <<<"$OUT"; then
  pass "rule3b: a reconciled key beside it is not named as drift"
else
  fail "rule3b: a reconciled key beside it is not named as drift" "$OUT"
fi
assert_file_not_contains "$HELM_CALL_LOG" "upgrade" "rule3b: hand edit never upgrades"
BUG_LINES=$(grep -c '"type":"bug"' "$CURL_BODY_LOG" || true)
if [ "$BUG_LINES" = "1" ]; then pass "rule3b: hand edit files one bug"; else fail "rule3b: hand edit files one bug" "got ${BUG_LINES}"; fi
BUG="$(grep -F '"type":"bug"' "$CURL_BODY_LOG" | head -1)"
if jq -e '(.short_title | length) <= 40 and (.short_title | length) > 0' <<<"$BUG" >/dev/null 2>&1; then
  pass "rule3b: the drift bug carries a short_title within kbeads' 40"
else
  fail "rule3b: the drift bug carries a short_title within kbeads' 40" "$BUG"
fi
teardown

# Hand-edited live values the new overlay already carries (review adopted
# the hand edit; 2026.308.3's installationId, gd-440OOVBHi7) -> reconciled,
# not drift: deploys with the new overlay and files no bug.
setup
make_yq_stub "$BIN"
KD_URL="http://kd.example"; KD_TOKEN="tok"
tag_overlay "$REPO" '{"githubBridge":{"githubApp":{"installationId":170295668}}}'
printf '{"global":{"imageTag":"2026.62.4"},"githubBridge":{"githubApp":{"installationId":171079859}}}' > "$HELM_GET_VALUES_FILE"
printf '{"githubBridge":{"githubApp":{"installationId":171079859}}}' > "$OVERLAY_JSON"
run_under_test "$CALVER"
assert_rc 0 "rule3b: live differs from the deployed tag's overlay but equals the new one -> deploys"
assert_out_contains "values drift reconciled (live equals the new overlay): githubBridge.githubApp.installationId" "rule3b: the reconciled key is logged"
assert_file_contains "$HELM_CALL_LOG" "-f ${REPO}/deploy-values/gardenio-k3s.yaml --set global.imageTag=${CALVER}" "rule3b: reconciled drift upgrades with the new tag's overlay"
BUG_LINES=$(grep -c '"type":"bug"' "$CURL_BODY_LOG" || true)
if [ "$BUG_LINES" = "0" ]; then pass "rule3b: reconciled drift files no bug"; else fail "rule3b: reconciled drift files no bug" "got ${BUG_LINES}"; fi
teardown

# No previous tag (live values carry no global.imageTag) -> falls back to
# the new overlay, so an overlay change still parks, and the report says so.
setup
make_yq_stub "$BIN"
printf '{"replicaCount":3}' > "$HELM_GET_VALUES_FILE"
printf '{"replicaCount":5}' > "$OVERLAY_JSON"
run_under_test "$CALVER"
assert_rc 3 "rule3b: no previous tag -> falls back to the new overlay and parks"
assert_out_contains "values drift: replicaCount (compared with the new overlay: the release has no global.imageTag)" "rule3b: fallback is named in the outcome"
teardown

# The deployed tag cannot be fetched (no origin), or the overlay did not
# exist at it -> falls back too.
setup
make_yq_stub "$BIN"
printf '{"global":{"imageTag":"2026.62.4"},"replicaCount":3}' > "$HELM_GET_VALUES_FILE"
printf '{"replicaCount":5}' > "$OVERLAY_JSON"
run_under_test "$CALVER"
assert_rc 3 "rule3b: previous tag not fetchable -> parks"
assert_out_contains "(compared with the new overlay: cannot fetch previous tag 2026.62.4)" "rule3b: unfetchable tag is named"
PATH="${BIN}:${PATH}" git -C "$REPO" rm -q deploy-values/gardenio-k3s.yaml
PATH="${BIN}:${PATH}" git -C "$REPO" commit -q -m "no overlay"
PATH="${BIN}:${PATH}" git -C "$REPO" tag 2026.62.4
printf 'replicaCount: 5\n' > "${REPO}/deploy-values/gardenio-k3s.yaml"
run_under_test "$CALVER"
assert_rc 3 "rule3b: overlay absent at the previous tag -> parks"
assert_out_contains "deploy-values/gardenio-k3s.yaml is not in previous tag 2026.62.4" "rule3b: absent overlay is named"
teardown

# ═══════════════════════════════════════════════════════════════════════
# Rule 4: the tag's own images.yml, ci.yml and checks.yml runs are green
# ═══════════════════════════════════════════════════════════════════════

setup
reaches_drift_check
printf '{"workflow_runs":[]}' > "$GH_RUNS_FILE"
run_under_test "$CALVER"
assert_rc 3 "rule4: no successful images.yml run -> exit 3 images not ready"
assert_out_contains "images not ready" "rule4: output names the reason"
assert_file_contains "$GH_CALL_LOG" "workflows/images.yml/runs?branch=${CALVER}&event=push" "rule4: queries the exact expected gh api path"
teardown

setup
reaches_drift_check
printf '{"workflow_runs":[{"id":7,"conclusion":"failure","head_sha":"x"},{"id":8,"conclusion":"success","head_sha":"%s"}]}' "$TAG_SHA" > "$GH_RUNS_FILE"
run_under_test "$CALVER"
assert_rc 0 "rule4: at least one successful run among several -> proceeds"
teardown

# Fix 8: a green run for a DIFFERENT commit than the tag's does not count.
setup
reaches_drift_check
printf '{"workflow_runs":[{"conclusion":"success","head_sha":"ffffffffffffffffffffffffffffffffffffffff"}]}' > "$GH_RUNS_FILE"
run_under_test "$CALVER"
assert_rc 3 "rule4: successful run but head_sha != tag commit -> exit 3 images not ready"
assert_file_contains "$GH_CALL_LOG" "commits/${CALVER}" "rule4: resolves the tag's commit via gh api"
assert_file_not_contains "$HELM_CALL_LOG" "upgrade" "rule4: sha mismatch never upgrades"
teardown

setup
reaches_drift_check
GH_API_FAIL=1
run_under_test "$CALVER"
assert_rc 1 "rule4: gh api itself fails -> exit 1, not silently treated as not-ready"
teardown

# Owner 2026-11-03: ci.yml and checks.yml run on the TAG push, and the tag
# rolls only once both are green beside images.yml.
setup
reaches_drift_check
run_under_test "$CALVER"
assert_rc 0 "rule4: all three workflows green at the tag -> proceeds"
for wf in images.yml ci.yml checks.yml; do
  assert_file_contains "$GH_CALL_LOG" "workflows/${wf}/runs?branch=${CALVER}&event=push" "rule4: asks ${wf} for the tag's push runs"
done
assert_file_contains "$GH_CALL_LOG" "actions/runs/101/jobs?filter=latest&per_page=100" "rule4: reads the green ci.yml run's jobs"
teardown

# ci.yml still running on the tag: skip, naming it and its state.
setup
reaches_drift_check
printf '{"workflow_runs":[{"id":102,"status":"in_progress","conclusion":null,"head_sha":"%s"}]}' "$TAG_SHA" > "${GH_RUNS_FILE}.ci.yml"
run_under_test "$CALVER"
assert_rc 3 "rule4: ci.yml in progress on the tag -> exit 3"
assert_out_contains "ci.yml not green: no successful ci.yml push run for ${CALVER} at ${TAG_SHA} (newest there: in_progress/-)" "rule4: names ci.yml and its newest run's state"
assert_file_not_contains "$HELM_CALL_LOG" "upgrade" "rule4: a ci.yml not green never upgrades"
teardown

# A red checks.yml run of the tag: never rolled.
setup
reaches_drift_check
printf '{"workflow_runs":[{"id":103,"status":"completed","conclusion":"failure","head_sha":"%s"}]}' "$TAG_SHA" > "${GH_RUNS_FILE}.checks.yml"
run_under_test "$CALVER"
assert_rc 3 "rule4: checks.yml red on the tag -> exit 3"
assert_out_contains "checks.yml not green" "rule4: names checks.yml"
assert_out_contains "(newest there: completed/failure)" "rule4: says the run failed"
assert_file_not_contains "$HELM_CALL_LOG" "upgrade" "rule4: a red checks.yml never upgrades"
teardown

# ── Rule 10: a red tag files one escalation (owner 2026-11-03) ─────────
# escalation_bodies -- the escalation POST bodies curl was handed, one per line.
escalation_bodies() { grep -F '"type":"escalation"' "$CURL_BODY_LOG" 2>/dev/null || true; }

# red_ci_fixture -- ci.yml's tag run 201 completed red; its jobs: one failed,
# one skipped, one green.
red_ci_fixture() {
  printf '{"workflow_runs":[{"id":201,"status":"completed","conclusion":"failure","head_sha":"%s","html_url":"https://github.com/mattbakerconsulting/gardenio/actions/runs/201"}]}' "$TAG_SHA" > "${GH_RUNS_FILE}.ci.yml"
  printf '{"jobs":[{"name":"build + test (go) (kbeads)","conclusion":"failure","html_url":"https://github.com/mattbakerconsulting/gardenio/actions/runs/201/job/9"},{"name":"build + test (coop)","conclusion":"skipped"},{"name":"gate: superseded? + changed paths","conclusion":"success"}]}' > "${GH_JOBS_FILE}.201"
}

# (a) Still running: quiet, as before.
setup
reaches_drift_check
KD_URL="http://kd.test"; KD_TOKEN="t"
printf '{"workflow_runs":[{"id":202,"status":"in_progress","conclusion":null,"head_sha":"%s"}]}' "$TAG_SHA" > "${GH_RUNS_FILE}.ci.yml"
run_under_test "$CALVER"
assert_rc 3 "rule10: ci.yml in progress -> exit 3"
if [ -z "$(escalation_bodies)" ]; then pass "rule10: a pending run files no escalation"; else fail "rule10: a pending run files no escalation" "$(escalation_bodies)"; fi
assert_file_not_contains "$GH_CALL_LOG" "actions/runs/202/jobs" "rule10: a pending run's jobs are never read"
teardown

# (b) A completed red ci.yml run: exactly one escalation, keyed per tag+workflow.
setup
reaches_drift_check
KD_URL="http://kd.test"; KD_TOKEN="t"
red_ci_fixture
CURL_POST_FILE="${TMP}/post.json"; printf '{"id":"gd-RedTag0001"}' > "$CURL_POST_FILE"
run_under_test "$CALVER"
assert_rc 3 "rule10: red ci.yml -> exit 3"
assert_out_contains "ci.yml not green" "rule10: the skip still names ci.yml"
ESC="$(escalation_bodies)"
if [ "$(grep -c . <<<"$ESC")" -eq 1 ]; then pass "rule10: exactly one escalation request"; else fail "rule10: exactly one escalation request" "got: ${ESC:-(none)}"; fi
if jq -e --arg k "release-red:${CALVER}:ci.yml" --arg t "Release ${CALVER} red: ci.yml failed" \
  '.idempotency_key == $k and .title == $t and .kind == "data" and .fields.operation_type == "github_actions_rerun"' <<<"$ESC" >/dev/null 2>&1; then
  pass "rule10: idempotency_key, title, kind and operation_type"
else
  fail "rule10: idempotency_key, title, kind and operation_type" "$ESC"
fi
if jq -e '(.short_title | length) <= 40 and (.short_title | length) > 0' <<<"$ESC" >/dev/null 2>&1; then pass "rule10: short_title within kbeads' 40"; else fail "rule10: short_title within kbeads' 40" "$ESC"; fi
if jq -e --arg sha "$TAG_SHA" --arg tag "$CALVER" \
  '.fields.operation_spec | .sha == $sha and .tag == $tag and .workflow == "ci.yml" and .runs[0].id == 201
   and ([.failed_jobs[].name] == ["build + test (go) (kbeads)"])
   and .commands == ["gh run rerun 201 --failed -R mattbakerconsulting/gardenio"]' <<<"$ESC" >/dev/null 2>&1; then
  pass "rule10: spec names tag, sha, run and only the failed (not skipped) job"
else
  fail "rule10: spec names tag, sha, run and only the failed (not skipped) job" "$ESC"
fi
CTX="$(jq -r '.fields.context' <<<"$ESC" 2>/dev/null)"
for needle in "https://github.com/mattbakerconsulting/gardenio/actions/runs/201 (failure)" \
  "Failed jobs: build + test (go) (kbeads) (failure)" ".github/quarantine.d/" \
  "gh run rerun 201 --failed -R mattbakerconsulting/gardenio" "No new cut." \
  "revert that PR's merge on main and cut a new tag" "the path chosen (quarantine or revert) and the culprit PR"; do
  if grep -qF -- "$needle" <<<"$CTX"; then pass "rule10: context has: ${needle}"; else fail "rule10: context has: ${needle}" "$CTX"; fi
done
assert_out_contains "red tag: escalation gd-RedTag0001 for ci.yml at ${CALVER}" "rule10: logs the filed escalation's id"
assert_file_not_contains "$HELM_CALL_LOG" "upgrade" "rule10: a red tag never upgrades"
teardown

# Cancelled and timed_out are red too; a red images.yml blocks first, and the
# scan still names a red checks.yml behind a still-running ci.yml.
setup
reaches_drift_check
KD_URL="http://kd.test"; KD_TOKEN="t"
printf '{"workflow_runs":[{"id":301,"status":"completed","conclusion":"cancelled","head_sha":"%s"}]}' "$TAG_SHA" > "${GH_RUNS_FILE}.images.yml"
printf '{"workflow_runs":[{"id":302,"status":"queued","conclusion":null,"head_sha":"%s"}]}' "$TAG_SHA" > "${GH_RUNS_FILE}.ci.yml"
printf '{"workflow_runs":[{"id":303,"status":"completed","conclusion":"timed_out","head_sha":"%s"}]}' "$TAG_SHA" > "${GH_RUNS_FILE}.checks.yml"
run_under_test "$CALVER"
assert_rc 3 "rule10: red images.yml -> exit 3"
assert_out_contains "images not ready" "rule10: the skip keeps images.yml's outcome"
ESC="$(escalation_bodies)"
if [ "$(jq -r '.idempotency_key' <<<"$ESC" 2>/dev/null | sort | paste -sd' ' -)" = "release-red:${CALVER}:checks.yml release-red:${CALVER}:images.yml" ]; then
  pass "rule10: one escalation each for the red images.yml and checks.yml, none for the queued ci.yml"
else
  fail "rule10: one escalation each for the red images.yml and checks.yml, none for the queued ci.yml" "${ESC:-(none)}"
fi
assert_file_contains "$GH_CALL_LOG" "workflows/checks.yml/runs?branch=${CALVER}&event=push" "rule10: the scan asks the workflows rule 4 never reached"
teardown

# A red run beside a green one at the tag is not red (and rolls).
setup
reaches_drift_check
KD_URL="http://kd.test"; KD_TOKEN="t"
printf '{"workflow_runs":[{"id":401,"status":"completed","conclusion":"failure","head_sha":"%s"},{"id":402,"status":"completed","conclusion":"success","head_sha":"%s"}]}' "$TAG_SHA" "$TAG_SHA" > "${GH_RUNS_FILE}.checks.yml"
run_under_test "$CALVER"
assert_rc 0 "rule10: a failed run beside a green one -> proceeds"
if [ -z "$(escalation_bodies)" ]; then pass "rule10: no escalation once a run succeeded"; else fail "rule10: no escalation once a run succeeded" "$(escalation_bodies)"; fi
teardown

# (c) Filing fails: logged, the exit code is rule 4's.
setup
reaches_drift_check
KD_URL="http://kd.test"; KD_TOKEN="t"
red_ci_fixture
CURL_FAIL=1
run_under_test "$CALVER"
assert_rc 3 "rule10: a failed escalation POST still exits 3"
assert_out_contains "WARN: failed to file the red-tag escalation release-red:${CALVER}:ci.yml to http://kd.test" "rule10: the filing failure is logged"
teardown

# A jobs lookup failing still files (best effort), naming no jobs.
setup
reaches_drift_check
KD_URL="http://kd.test"; KD_TOKEN="t"
red_ci_fixture
GH_JOBS_FAIL=1
run_under_test "$CALVER"
assert_rc 3 "rule10: jobs api failing on a red run -> still exit 3, not 1"
if jq -e '.fields.context | contains("Failed jobs: (none listed)")' <<<"$(escalation_bodies)" >/dev/null 2>&1; then pass "rule10: files without the job names"; else fail "rule10: files without the job names" "$(escalation_bodies)"; fi
assert_out_contains "WARN: red-tag check: cannot list the jobs of ci.yml run 201" "rule10: the jobs failure is logged"
teardown

# (d) --dry-run files nothing.
setup
reaches_drift_check
KD_URL="http://kd.test"; KD_TOKEN="t"
red_ci_fixture
run_under_test "$CALVER" --dry-run
assert_rc 3 "rule10: --dry-run on a red tag -> exit 3"
if [ -z "$(escalation_bodies)" ]; then pass "rule10: --dry-run files no escalation"; else fail "rule10: --dry-run files no escalation" "$(escalation_bodies)"; fi
assert_out_contains "dry run, escalation release-red:${CALVER}:ci.yml not filed" "rule10: --dry-run says what it did not file"
teardown

# Without KD_URL there is nowhere to file: no extra gh calls either.
setup
reaches_drift_check
red_ci_fixture
run_under_test "$CALVER"
assert_rc 3 "rule10: red tag without KD_URL -> exit 3"
assert_file_not_contains "$GH_CALL_LOG" "actions/runs/201/jobs" "rule10: without KD_URL the red run's jobs are not read"
teardown

# (e) The green path is unchanged: rolls, and files no escalation.
setup
reaches_drift_check
KD_URL="http://kd.test"; KD_TOKEN="t"
run_under_test "$CALVER"
assert_rc 0 "rule10: all green with KD_URL -> exit 0"
if [ -z "$(escalation_bodies)" ]; then pass "rule10: a green tag files no escalation"; else fail "rule10: a green tag files no escalation" "$(escalation_bodies)"; fi
teardown

# A green ci.yml run at a different commit than the tag's does not count.
setup
reaches_drift_check
printf '{"workflow_runs":[{"id":104,"status":"completed","conclusion":"success","head_sha":"ffffffffffffffffffffffffffffffffffffffff"}]}' > "${GH_RUNS_FILE}.ci.yml"
run_under_test "$CALVER"
assert_rc 3 "rule4: green ci.yml at another commit -> exit 3"
assert_out_contains "(newest there: none)" "rule4: no ci.yml run at the tag's commit at all"
teardown

# ci.yml concluded success but built nothing: an in-job sentinel skipped...
setup
reaches_drift_check
printf '{"jobs":[{"name":"build + test (go) (kbeads)","steps":[{"name":"heavy work starting","conclusion":"success"}]},{"name":"build + test (go) (controller)","steps":[{"name":"heavy work starting","conclusion":"skipped"}]}]}' > "$GH_JOBS_FILE"
run_under_test "$CALVER"
assert_rc 3 "rule4: ci.yml success with a sentinel skipped -> exit 3"
assert_out_contains "but built nothing" "rule4: says ci.yml built nothing"
assert_file_not_contains "$HELM_CALL_LOG" "upgrade" "rule4: an unbuilt ci.yml never upgrades"
teardown

# ...or no sentinel ran at all (every heavy job skipped at the gate)...
setup
reaches_drift_check
printf '{"jobs":[{"name":"gate: superseded? + changed paths","steps":[]},{"name":"build + test (go) (kbeads)","conclusion":"skipped","steps":[]}]}' > "$GH_JOBS_FILE"
run_under_test "$CALVER"
assert_rc 3 "rule4: ci.yml success with no sentinel success -> exit 3"
teardown

# ...or the gate's run-wide skip fired.
setup
reaches_drift_check
printf '{"jobs":[{"name":"gate: superseded? + changed paths","steps":[{"name":"superseded -- run-wide skip","conclusion":"success"}]},{"name":"build + test (coop)","steps":[{"name":"heavy work starting","conclusion":"success"}]}]}' > "$GH_JOBS_FILE"
run_under_test "$CALVER"
assert_rc 3 "rule4: ci.yml run-wide skip fired -> exit 3"
teardown

# Fail closed: the jobs lookup failing, or answering garbage, is exit 1.
setup
reaches_drift_check
GH_JOBS_FAIL=1
run_under_test "$CALVER"
assert_rc 1 "rule4: ci.yml jobs api fails -> exit 1, not a skip"
assert_out_contains "gh api ci.yml run 101 jobs failed" "rule4: names the failing jobs call"
teardown

setup
reaches_drift_check
printf '<html>rate limited</html>' > "$GH_JOBS_FILE"
run_under_test "$CALVER"
assert_rc 1 "rule4: unparseable ci.yml jobs -> exit 1"
teardown

setup
reaches_drift_check
printf '<html>rate limited</html>' > "${GH_RUNS_FILE}.checks.yml"
run_under_test "$CALVER"
assert_rc 1 "rule4: unparseable checks.yml runs -> exit 1"
teardown

# ═══════════════════════════════════════════════════════════════════════
# Rule 5/6: upgrade + verify (happy path), exact argv
# ═══════════════════════════════════════════════════════════════════════

setup
reaches_drift_check
run_under_test "$CALVER" --namespace gardenio-staging --release gardenio
assert_rc 0 "rule5/6: full happy path -> exit 0"
EXPECTED_UPGRADE="/gardenio-${CALVER}.tgz -n gardenio-staging -f ${REPO}/deploy-values/gardenio-k3s.yaml --set global.imageTag=${CALVER} --atomic --wait --timeout 20m"
assert_file_contains "$HELM_CALL_LOG" "helm upgrade gardenio " "rule5: helm upgrade names the release"
assert_file_contains "$HELM_CALL_LOG" "$EXPECTED_UPGRADE" "rule5: exact helm upgrade argv, on the packed .tgz"
assert_file_not_contains "$HELM_CALL_LOG" "upgrade gardenio ${REPO}/gardenio/helm/gardenio " "rule5: never upgrades from the raw chart dir (release Secret overflows 1 MiB)"
assert_file_contains "$PACK_CALL_LOG" "pack ${REPO}/gardenio/helm/gardenio " "rule5: packs the chart from the checkout"
assert_file_contains "$PACK_CALL_LOG" "--version ${CALVER} --app-version ${CALVER}" "rule5: pack stamps the calver as chart version"
teardown

# Pack fails (e.g. the comment strip changed the render) -> exit 1, reported,
# and helm upgrade never runs.
setup
reaches_drift_check
PACK_FAIL=1
run_under_test "$CALVER"
assert_rc 1 "rule5: chart pack fails -> exit 1"
assert_out_contains "chart pack failed" "rule5: output names the pack failure"
assert_out_contains "changed helm template render" "rule5: output carries the pack's stderr"
assert_file_not_contains "$HELM_CALL_LOG" "upgrade" "rule5: pack failure never upgrades"
teardown

# Pack exits 0 but the .tgz it names is missing -> exit 1, no upgrade.
setup
reaches_drift_check
PACK_NO_FILE=1
run_under_test "$CALVER"
assert_rc 1 "rule5: pack names a missing .tgz -> exit 1"
assert_out_contains "chart pack produced no file" "rule5: output names the missing .tgz"
assert_file_not_contains "$HELM_CALL_LOG" "upgrade" "rule5: missing .tgz never upgrades"
teardown

# ═══════════════════════════════════════════════════════════════════════
# Rule 4b: the Keycloak theme ConfigMap (gd-qiUiXvTX0k) is rendered from the
# checkout and server-side applied before the upgrade, after every gate.
# ═══════════════════════════════════════════════════════════════════════

setup
reaches_drift_check
run_under_test "$CALVER" --namespace gardenio-staging --release gardenio
assert_rc 0 "rule4b: happy path still exit 0"
assert_file_contains "$THEME_CALL_LOG" "theme --release gardenio --namespace gardenio-staging" "rule4b: theme generator gets the release and namespace"
assert_file_contains "$KUBECTL_CALL_LOG" "kubectl apply --server-side --field-manager=release-deploy --force-conflicts -n gardenio-staging -f -" "rule4b: exact kubectl apply argv (server-side, from stdin)"
assert_file_contains "$KUBECTL_APPLY_LOG" '"kind":"ConfigMap"' "rule4b: the rendered ConfigMap is what reaches kubectl's stdin"
assert_file_not_contains "$KUBECTL_CALL_LOG" "dry-run" "rule4b: a real run applies for real"
teardown

# The apply happens before the pack and the upgrade: a failed render stops
# everything with nothing packed and nothing upgraded.
setup
reaches_drift_check
THEME_FAIL=1
run_under_test "$CALVER"
assert_rc 1 "rule4b: theme render fails -> exit 1"
assert_out_contains "theme configmap render failed" "rule4b: output names the render failure"
assert_out_contains "contains '__'" "rule4b: output carries the generator's stderr"
assert_file_not_contains "$KUBECTL_CALL_LOG" "apply" "rule4b: nothing applied after a failed render"
assert_file_not_contains "$PACK_CALL_LOG" "pack" "rule4b: nothing packed after a failed render"
assert_file_not_contains "$HELM_CALL_LOG" "upgrade" "rule4b: no upgrade after a failed render"
teardown

setup
reaches_drift_check
KUBECTL_APPLY_FAIL=1
run_under_test "$CALVER"
assert_rc 1 "rule4b: kubectl apply fails -> exit 1"
assert_out_contains "theme configmap apply failed" "rule4b: output names the apply failure"
assert_out_contains "is forbidden" "rule4b: output carries kubectl's stderr"
assert_file_not_contains "$HELM_CALL_LOG" "upgrade" "rule4b: no upgrade after a failed apply"
teardown

# A skipped run (any gate) never renders or applies the theme.
setup
KUBECTL_PAUSE_EXISTS=1
run_under_test "$CALVER"
assert_rc 3 "rule4b: paused -> exit 3"
assert_file_not_contains "$THEME_CALL_LOG" "theme" "rule4b: paused run never renders the theme"
assert_file_not_contains "$KUBECTL_CALL_LOG" "apply" "rule4b: paused run never applies the theme"
teardown

# --dry-run: the apply is a server-side dry run (validated, not persisted).
setup
reaches_drift_check
run_under_test "$CALVER" --dry-run
assert_rc 0 "rule4b: dry run -> exit 0"
assert_file_contains "$KUBECTL_CALL_LOG" "kubectl apply --server-side --field-manager=release-deploy --force-conflicts -n gardenio -f - --dry-run=server" "rule4b: dry run applies with --dry-run=server"
assert_file_contains "$HELM_CALL_LOG" "--dry-run=server" "rule4b: dry run still reaches the helm dry run"
teardown

# Dry run also uses the packed chart (it must preview what will really ship).
setup
reaches_drift_check
run_under_test "$CALVER" --dry-run
assert_rc 0 "rule5: dry run -> exit 0"
assert_file_contains "$HELM_CALL_LOG" "/gardenio-${CALVER}.tgz -n gardenio -f" "rule5: dry run upgrades from the packed .tgz"
teardown

# Custom --values file is passed straight through into the upgrade argv.
setup
make_yq_stub "$BIN"
CUSTOM_VALUES="${TMP}/custom-values.yaml"
printf 'replicaCount: 3\n' > "$CUSTOM_VALUES"
printf '{}' > "${CUSTOM_VALUES}.json"
run_under_test "$CALVER" --values "$CUSTOM_VALUES"
assert_rc 0 "rule5: custom --values accepted"
assert_file_contains "$HELM_CALL_LOG" "-f ${CUSTOM_VALUES} --set global.imageTag=${CALVER}" "rule5: custom values file lands in argv"
teardown

# ═══════════════════════════════════════════════════════════════════════
# Rule 6: verify failure -> rollback, exact argv
# ═══════════════════════════════════════════════════════════════════════

setup
reaches_drift_check
printf 'gardenio-controller-abc\tregistry.example/gardenio/controller:2026.62.4\n' > "$KUBECTL_PODS_FILE"
run_under_test "$CALVER" --namespace gardenio-staging
assert_rc 1 "rule6: verify fails (stale tag) -> rollback succeeds -> exit 1"
assert_file_contains "$HELM_CALL_LOG" "helm rollback gardenio 0 -n gardenio-staging --wait --timeout 20m" "rule6: exact helm rollback argv"
assert_out_contains "rolled back" "rule6: output says it rolled back"
teardown

# Rollback itself fails -> exit 2.
setup
reaches_drift_check
printf 'gardenio-controller-abc\tregistry.example/gardenio/controller:2026.62.4\n' > "$KUBECTL_PODS_FILE"
HELM_ROLLBACK_EXIT=1
run_under_test "$CALVER"
assert_rc 2 "rule6: verify fails and rollback ALSO fails -> exit 2"
teardown

# helm upgrade itself fails (atomic already rolled back internally) -> exit 1,
# and no separate manual `helm rollback` call is made.
setup
reaches_drift_check
HELM_UPGRADE_EXIT=1
HELM_UPGRADE_OUTPUT="Error: UPGRADE FAILED: timed out waiting for condition"
HELM_STATUS_POST_FILE="${TMP}/helm-status-post.json"
printf '{"version":7,"info":{"status":"deployed"}}' > "$HELM_STATUS_POST_FILE"
run_under_test "$CALVER"
assert_rc 1 "rule5: helm upgrade fails, atomic rollback left it deployed -> exit 1"
assert_file_not_contains "$HELM_CALL_LOG" "rollback" "rule5: --atomic already rolled back; no manual rollback call"
teardown

# Probe (fix 4): the atomic rollback itself failed -- release is left
# `failed` -> exit 2, and the P1 bug says manual intervention is needed.
setup
reaches_drift_check
HELM_UPGRADE_EXIT=1
HELM_UPGRADE_OUTPUT="Error: UPGRADE FAILED: release gardenio failed, and has been rolled back due to atomic being set: rollback failed"
HELM_STATUS_POST_FILE="${TMP}/helm-status-post.json"
printf '{"version":7,"info":{"status":"failed"}}' > "$HELM_STATUS_POST_FILE"
KD_URL="http://kd.example"
run_under_test "$CALVER"
assert_rc 2 "rule5: upgrade fails and release is NOT deployed afterwards -> exit 2"
assert_file_contains "$CURL_BODY_LOG" '"type":"bug"' "rule5: failed atomic rollback files a bug bead"
assert_file_contains "$CURL_BODY_LOG" "manual intervention" "rule5: bug body says manual intervention is needed"
teardown

setup
reaches_drift_check
HELM_UPGRADE_EXIT=1
HELM_STATUS_POST_FILE="${TMP}/helm-status-post.json"
HELM_STATUS_POST_FAIL=1
run_under_test "$CALVER"
assert_rc 2 "rule5: upgrade fails and post-upgrade status is unreadable -> exit 2"
teardown

# ═══════════════════════════════════════════════════════════════════════
# Rule 7: --dry-run
# ═══════════════════════════════════════════════════════════════════════

setup
reaches_drift_check
run_under_test "$CALVER" --dry-run
assert_rc 0 "rule7: dry-run happy path -> exit 0"
assert_file_contains "$HELM_CALL_LOG" "--dry-run=server --hide-secret" "rule7: dry-run passes --hide-secret (no Secrets in logs/beads)"
assert_file_not_contains "$HELM_CALL_LOG" "--atomic" "rule7: dry-run never passes --atomic"
assert_file_not_contains "$KUBECTL_CALL_LOG" "pods" "rule7: no verify step under --dry-run"
teardown

# Gates 1-4 are still evaluated for real under --dry-run.
setup
KUBECTL_PAUSE_EXISTS=1
run_under_test "$CALVER" --dry-run
assert_rc 3 "rule7: pause gate still real under --dry-run"
teardown

# A failing server-side dry run is reported as a failure, not a silent pass.
setup
reaches_drift_check
HELM_UPGRADE_EXIT=1
HELM_UPGRADE_OUTPUT="Error: template: bad indentation"
run_under_test "$CALVER" --dry-run
assert_rc 1 "rule7: failing server-side dry-run -> exit 1"
teardown

# ═══════════════════════════════════════════════════════════════════════
# Rule 8: reporting
# ═══════════════════════════════════════════════════════════════════════

# Success: one event bead, no bug bead.
setup
reaches_drift_check
KD_URL="http://kd.example"
KD_TOKEN="tok"
KD_OPERATED_BY="test-agent"
run_under_test "$CALVER"
assert_rc 0 "rule8: happy path still exits 0 with reporting enabled"
assert_file_contains "$CURL_CALL_LOG" "http://kd.example/v1/beads" "rule8: POSTs to \$KD_URL/v1/beads"
assert_file_contains "$CURL_CALL_LOG" "Authorization: Bearer tok" "rule8: bearer token header"
assert_file_contains "$CURL_CALL_LOG" "X-Operated-By: test-agent" "rule8: X-Operated-By header"
assert_file_contains "$CURL_BODY_LOG" "\"type\":\"event\"" "rule8: files a type=event bead"
assert_file_contains "$CURL_BODY_LOG" "release-deploy ${CALVER}: deployed and verified" "rule8: title names calver + outcome"
EVENT_COUNT="$(grep -c '"type":' "$CURL_BODY_LOG" || true)"
if [ "$EVENT_COUNT" -eq 1 ]; then
  pass "rule8: success files exactly one bead (no bug)"
else
  fail "rule8: success files exactly one bead (no bug)" "found ${EVENT_COUNT} POST bodies:
$(cat "$CURL_BODY_LOG")"
fi
teardown

# Failure (verify fails, rollback ok): event bead AND a P1 bug bead.
setup
reaches_drift_check
printf 'gardenio-controller-abc\tregistry.example/gardenio/controller:2026.62.4\n' > "$KUBECTL_PODS_FILE"
KD_URL="http://kd.example"
KD_TOKEN="tok"
KD_OPERATED_BY="test-agent"
run_under_test "$CALVER"
assert_rc 1 "rule8: failure path still exits 1 with reporting enabled"
assert_file_contains "$CURL_BODY_LOG" "\"type\":\"bug\"" "rule8: failure files a type=bug bead"
assert_file_contains "$CURL_BODY_LOG" "\"priority\":1" "rule8: bug bead is priority 1"
BODY_COUNT="$(grep -c '"type":' "$CURL_BODY_LOG" || true)"
if [ "$BODY_COUNT" -eq 2 ]; then
  pass "rule8: failure files exactly two beads (event + bug)"
else
  fail "rule8: failure files exactly two beads (event + bug)" "found ${BODY_COUNT} POST bodies:
$(cat "$CURL_BODY_LOG")"
fi
teardown

# Fix 7: a multi-line error lands in the title as its first line only; the
# full text stays in the description.
setup
HELM_STATUS_FAIL=1
HELM_STATUS_ERR=$'Error: Kubernetes cluster unreachable\nSECOND-LINE-DETAIL'
KD_URL="http://kd.example"
run_under_test "$CALVER"
assert_rc 1 "rule8: multi-line error -> exit 1"
TITLES="$(jq -r '.title' "$CURL_BODY_LOG" 2>/dev/null || true)"
if [ -n "$TITLES" ] && ! grep -qF "SECOND-LINE-DETAIL" <<<"$TITLES" && grep -qF "cluster unreachable" <<<"$TITLES"; then
  pass "rule8: bead titles carry only the first line of the error"
else
  fail "rule8: bead titles carry only the first line of the error" "titles:
${TITLES}"
fi
assert_file_contains "$CURL_BODY_LOG" "SECOND-LINE-DETAIL" "rule8: full error text stays in the description"
teardown

# A broad values drift lists every key in the outcome; kbeads rejects bead
# titles over 500 characters, so the title is capped and the full key list
# stays in the description.
setup
make_yq_stub "$BIN"
python3 -I -c 'import json; print(json.dumps({"global":{"imageTag":"2026.62.4"},"wide":{"drifted_key_number_%02d" % i: i for i in range(60)}}))' > "$HELM_GET_VALUES_FILE"
printf '{}' > "$OVERLAY_JSON"
KD_URL="http://kd.example"
run_under_test "$CALVER"
assert_rc 3 "rule8: 60-key drift -> exit 3"
LONGEST="$(jq -r '.title | length' "$CURL_BODY_LOG" 2>/dev/null | sort -n | tail -1)"
if [ -n "$LONGEST" ] && [ "$LONGEST" -le 500 ] && [ "$LONGEST" -gt 400 ]; then
  pass "rule8: a long drift title is capped at kbeads' 500-character limit"
else
  fail "rule8: a long drift title is capped at kbeads' 500-character limit" "longest title: ${LONGEST:-none}"
fi
assert_file_contains "$CURL_BODY_LOG" "drifted_key_number_59" "rule8: the full drift key list stays in the description"
teardown

# KD_TOKEN is never printed.
setup
reaches_drift_check
KD_URL="http://kd.example"
KD_TOKEN="sekrit-token-value"
printf 'gardenio-controller-abc\tregistry.example/gardenio/controller:2026.62.4\n' > "$KUBECTL_PODS_FILE"
run_under_test "$CALVER"
if grep -qF "sekrit-token-value" <<<"$OUT" || grep -qF "sekrit-token-value" "$CURL_BODY_LOG"; then
  fail "rule8: KD_TOKEN never printed or put in a bead" "token leaked"
else
  pass "rule8: KD_TOKEN never printed or put in a bead"
fi
teardown

# No KD_URL -> no reporting attempted at all, exit code unaffected.
setup
reaches_drift_check
run_under_test "$CALVER" --dry-run
assert_rc 0 "rule8: no KD_URL -> still succeeds"
if [ -s "$CURL_CALL_LOG" ]; then
  fail "rule8: no KD_URL -> curl never invoked" "curl was called:
$(cat "$CURL_CALL_LOG")"
else
  pass "rule8: no KD_URL -> curl never invoked"
fi
teardown

# Reporting failure (curl itself fails) never changes the exit code.
setup
reaches_drift_check
KD_URL="http://kd.example"
KD_TOKEN="tok"
KD_OPERATED_BY="test-agent"
CURL_FAIL=1
run_under_test "$CALVER"
assert_rc 0 "rule8: reporting POST failing never changes the exit code"
teardown

# ═══════════════════════════════════════════════════════════════════════
# Cron quieting: steady-state skips are silent, repeat bugs are deduped
# ═══════════════════════════════════════════════════════════════════════

# assert_no_curl <desc> -- nothing at all was sent to kbeads.
assert_no_curl() {
  if [ -s "$CURL_CALL_LOG" ]; then
    fail "$1" "curl was called:
$(cat "$CURL_CALL_LOG")"
  else
    pass "$1"
  fi
}

setup
KD_URL="http://kd.example"
KUBECTL_PAUSE_EXISTS=1
run_under_test "$CALVER"
assert_rc 3 "quiet: paused still exits 3"
assert_no_curl "quiet: paused files no bead"
teardown

setup
KD_URL="http://kd.example"
printf '{"global":{"imageTag":"%s"}}' "$CALVER" > "$HELM_GET_VALUES_FILE"
run_under_test "$CALVER"
assert_rc 3 "quiet: not newer still exits 3"
assert_no_curl "quiet: not newer files no bead"
teardown

# Positive control: other exit-3 skips (images not ready) still report.
setup
reaches_drift_check
KD_URL="http://kd.example"
printf '{"workflow_runs":[]}' > "$GH_RUNS_FILE"
run_under_test "$CALVER"
assert_rc 3 "quiet: images not ready exits 3"
assert_file_contains "$CURL_BODY_LOG" '"type":"event"' "quiet: images not ready still files an event bead"
teardown

# drift_setup -- the rule3b positive-control fixture (replicaCount drift).
drift_setup() {
  make_yq_stub "$BIN"
  printf '{"global":{"imageTag":"2026.62.4"},"replicaCount":3}' > "$HELM_GET_VALUES_FILE"
  printf '{}' > "$OVERLAY_JSON"
  KD_URL="http://kd.example"
}

# No open bug with this title -> the drift bug IS filed (positive control).
setup
drift_setup
CURL_GET_FILE="${TMP}/open-bugs.json"
printf '{"beads":[{"title":"release-deploy %s: something else"}],"total":1}' "$CALVER" > "$CURL_GET_FILE"
run_under_test "$CALVER"
assert_rc 3 "dedup: drift with no matching open bug exits 3"
assert_file_contains "$CURL_CALL_LOG" "/v1/beads?type=bug&status=open&labels=release-deploy" "dedup: queries open release-deploy bugs"
assert_file_contains "$CURL_BODY_LOG" '"type":"bug"' "dedup: drift bug filed when none is open"
teardown

# Same title already open -> nothing is POSTed.
setup
drift_setup
run_under_test "$CALVER"
DRIFT_TITLE="$(jq -r 'select(.type=="bug") | .title' "$CURL_BODY_LOG")"
teardown
setup
drift_setup
CURL_GET_FILE="${TMP}/open-bugs.json"
jq -nc --arg t "$DRIFT_TITLE" '{beads:[{title:$t}],total:1}' > "$CURL_GET_FILE"
run_under_test "$CALVER"
assert_rc 3 "dedup: drift with a matching open bug still exits 3"
if [ -n "$DRIFT_TITLE" ] && [ ! -s "$CURL_BODY_LOG" ]; then
  pass "dedup: matching open bug -> no event and no bug POSTed"
else
  fail "dedup: matching open bug -> no event and no bug POSTed" "title='${DRIFT_TITLE}' bodies:
$(cat "$CURL_BODY_LOG")"
fi
teardown

# Dedup query fails -> fail open, the bug is still filed.
setup
drift_setup
CURL_GET_FILE="${TMP}/open-bugs.json"
printf '<html>502</html>' > "$CURL_GET_FILE"
run_under_test "$CALVER"
assert_file_contains "$CURL_BODY_LOG" '"type":"bug"' "dedup: unparseable query result fails open (bug still filed)"
teardown

# ═══════════════════════════════════════════════════════════════════════
# Verify: explicit deploy-values pins and Job pods
# ═══════════════════════════════════════════════════════════════════════

# pin_setup -- overlay AND live both pin beads.image.tag=e27ec943 (no drift).
pin_setup() {
  make_yq_stub "$BIN"
  printf '{"beads":{"image":{"tag":"e27ec943"}}}' > "$OVERLAY_JSON"
  printf '{"global":{"imageTag":"2026.62.4"},"beads":{"image":{"tag":"e27ec943"}}}' > "$HELM_GET_VALUES_FILE"
}

setup
pin_setup
printf 'gardenio-controller-abc\tregistry.example/gardenio/controller:%s\ngardenio-beads-abc\tregistry.example/gardenio/kbeads:e27ec943\n' "$CALVER" > "$KUBECTL_PODS_FILE"
run_under_test "$CALVER"
assert_rc 0 "verify: pod on a tag the overlay pins passes"
assert_file_not_contains "$HELM_CALL_LOG" "rollback" "verify: pinned pod never triggers a rollback"
teardown

# Positive control: the same pod on a tag the overlay does NOT pin still fails.
setup
pin_setup
printf 'gardenio-controller-abc\tregistry.example/gardenio/controller:%s\ngardenio-beads-abc\tregistry.example/gardenio/kbeads:0ldbeef\n' "$CALVER" > "$KUBECTL_PODS_FILE"
run_under_test "$CALVER"
assert_rc 1 "verify: pod on an UNpinned stale tag still fails (positive control)"
teardown

# A global.imageTag is never an allow-tag (it IS the calver being replaced).
setup
make_yq_stub "$BIN"
printf '{"global":{"image":{"tag":"2026.62.4"}}}' > "$OVERLAY_JSON"
printf '{"global":{"imageTag":"2026.62.4","image":{"tag":"2026.62.4"}}}' > "$HELM_GET_VALUES_FILE"
printf 'gardenio-controller-abc\tregistry.example/gardenio/controller:2026.62.4\n' > "$KUBECTL_PODS_FILE"
run_under_test "$CALVER"
assert_rc 1 "verify: global.* image tags are never allow-listed"
teardown

# Job-owned pod on the previous tag (the deployer's own run, a CronJob run,
# a hook) is reported but not gated.
setup
reaches_drift_check
printf 'gardenio-controller-abc\tregistry.example/gardenio/controller:%s\t\ngardenio-release-deployer-123-x\tregistry.example/gardenio/agent:2026.62.4\tJob\n' "$CALVER" > "$KUBECTL_PODS_FILE"
run_under_test "$CALVER"
assert_rc 0 "verify: Job pod on the previous tag does not fail the gate"
teardown

# Positive control: the same stale pod owned by a ReplicaSet fails.
setup
reaches_drift_check
printf 'gardenio-controller-abc\tregistry.example/gardenio/controller:%s\t\ngardenio-agents-123-x\tregistry.example/gardenio/agent:2026.62.4\tReplicaSet\n' "$CALVER" > "$KUBECTL_PODS_FILE"
run_under_test "$CALVER"
assert_rc 1 "verify: ReplicaSet pod on the previous tag fails (positive control)"
teardown

# ── machine principal reporting (gd-5QYwXtV7IA) ───────────────────────
# A failing pause probe files a bug; with KD_OIDC_* set it must authenticate
# as the machine principal: the token fetched from Keycloak is the bearer, the
# legacy KD_TOKEN and X-Operated-By are never sent, and the secret FILE path
# (not its content) is the only secret-bearing arg.
setup
KUBECTL_PAUSE_ERR="connection refused"
KD_URL="http://kd.test"; KD_TOKEN="legacy-static"; KD_OPERATED_BY="oidc:x@example.com"
KD_OIDC_TOKEN_URL="http://kc.test/auth/realms/gardenio/protocol/openid-connect/token"
KD_OIDC_CLIENT_ID="gardenio-release-deployer"; KD_OIDC_CLIENT_SECRET_FILE="${TMP}/client-secret"
printf 'sup3r-secret' > "$KD_OIDC_CLIENT_SECRET_FILE"
run_under_test "$CALVER"
assert_file_contains "$CURL_CALL_LOG" "Authorization: Bearer machine-jwt" "machine: daemon call carries the machine token"
assert_file_not_contains "$CURL_CALL_LOG" "legacy-static" "machine: legacy KD_TOKEN is never sent"
assert_file_not_contains "$CURL_CALL_LOG" "X-Operated-By" "machine: no X-Operated-By header"
assert_file_not_contains "$CURL_CALL_LOG" "sup3r-secret" "machine: secret content never in argv"
assert_file_contains "$CURL_CALL_LOG" "client_secret@${KD_OIDC_CLIENT_SECRET_FILE}" "machine: secret is passed by file reference"
assert_file_contains "$CURL_BODY_LOG" '"type":"bug"' "machine: the bug is still filed"
assert_file_not_contains "$CURL_CALL_LOG" "--cacert" "machine: no --cacert without KD_OIDC_CA_FILE"
teardown

# A failed token fetch never falls back to the static token; KD_OIDC_CA_FILE is curl's --cacert (gd-lYPZlslOM7).
setup
KUBECTL_PAUSE_ERR="connection refused"
KD_URL="http://kd.test"; KD_TOKEN="legacy-static"; KD_OPERATED_BY="oidc:x@example.com"
KD_OIDC_TOKEN_URL="http://kc.test/auth/realms/gardenio/protocol/openid-connect/token"
KD_OIDC_CLIENT_ID="gardenio-release-deployer"; KD_OIDC_CLIENT_SECRET_FILE="${TMP}/client-secret"; KD_OIDC_CA_FILE="/etc/gardenio/keycloak-runtime-ca/ca.crt"
printf 'sup3r-secret' > "$KD_OIDC_CLIENT_SECRET_FILE"
CURL_TOKEN_FAIL=1
run_under_test "$CALVER"
assert_rc 1 "machine: token fetch failure keeps the original exit code"
assert_file_not_contains "$CURL_CALL_LOG" "legacy-static" "machine: failed fetch never substitutes KD_TOKEN"
assert_file_contains "$CURL_CALL_LOG" "--cacert ${KD_OIDC_CA_FILE}" "machine: token fetch trusts KD_OIDC_CA_FILE"
teardown

# ═══════════════════════════════════════════════════════════════════════

echo ""
echo "== ${PASS} passed, ${FAIL} failed =="
[ "$FAIL" -eq 0 ]
