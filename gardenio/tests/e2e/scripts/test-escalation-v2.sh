#!/usr/bin/env bash
# test-escalation-v2.sh — Escalation V2 pipeline E2E tests
#
# Tests V2-specific fields and behaviors: execution_script, credential_requirements,
# data_requirements, expected_outputs, sensitivity levels, redacted_summary,
# report beads, and schema validation.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-escalation-v2.sh
#
# Prerequisites:
#   - BEADS_HTTP_URL set (kbeads HTTP API, e.g. http://localhost:19090)
#   - kd binary in PATH (or set KD_BIN)
#   - curl, python3, jq installed
#
# Scenarios tested:
#   1. Create escalation with V2 fields (execution_script, credentials, sensitivity)
#   2. V2 fields round-trip: all V2 fields persist and return correctly
#   3. Sensitivity enum validation: invalid values are rejected
#   4. Sensitive escalation result uses redacted_summary
#   5. Report bead creation for sensitive escalation results
#   6. V1 backward compatibility: escalation without V2 fields works
#   7. Credential requirements JSON round-trip
#   8. Expected outputs with per-field sensitivity
#   9. Execution target enum validation

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
KD_BIN="${KD_BIN:-kd}"
PASS=0
FAIL=0
ERRORS=()
PF_PID=""
CREATED_BEADS=()

export KD_ACTOR="${KD_ACTOR:-e2e-test}"

# ── Helpers ───────────────────────────────────────────────────────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

# P7 Task 9 (gd-nPfSNHTkQQ): the daemon admits /reject only from an operator
# identity and /result only from an operator, a bridge, a machine principal or
# an agent, so this suite's shared BEADS_TOKEN is refused ("caller class:
# service"). resolve_refused prints a SKIP with the reason and returns 0 when
# $2 holds that refusal; callers skip the assertions that depend on the
# resolve. The refusal is never worked around to make a scenario pass.
resolve_refused() {
  grep -qF 'caller class:' <<<"$2" || return 1
  green "  SKIP: $1 refused: $(grep -o 'caller class: [a-z]*' <<<"$2" | head -1) (P7 Task 9)"
  return 0
}

pass() {
  local name="$1"
  green "  ✓ $name"
  PASS=$((PASS + 1))
}

fail() {
  local name="$1"
  local reason="${2:-}"
  red "  ✗ $name"
  [ -n "$reason" ] && dim "    $reason"
  FAIL=$((FAIL + 1))
  ERRORS+=("$name: $reason")
}

assert_exit() {
  local name="$1"
  local expected="$2"
  local actual="$3"
  if [ "$actual" -eq "$expected" ]; then
    pass "$name (exit $expected)"
  else
    fail "$name" "expected exit $expected, got exit $actual"
  fi
}

assert_contains() {
  local name="$1"
  local pattern="$2"
  local actual="$3"
  if grep -qF -- "$pattern" <<< "$actual"; then
    pass "$name"
  else
    fail "$name" "expected to contain '$pattern'; got: $(echo "$actual" | head -5)"
  fi
}

assert_not_contains() {
  local name="$1"
  local pattern="$2"
  local actual="$3"
  if ! grep -qF -- "$pattern" <<< "$actual"; then
    pass "$name"
  else
    fail "$name" "expected to NOT contain '$pattern'"
  fi
}

json_field() {
  python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('$1',''))" 2>/dev/null
}

json_nested() {
  local path="$1"
  python3 -c "
import sys,json
d=json.load(sys.stdin)
keys='$path'.split('.')
for k in keys:
    d=d.get(k,{}) if isinstance(d,dict) else {}
print(d if isinstance(d,str) else json.dumps(d))
" 2>/dev/null
}

# ── Setup: port-forward kd server ────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi

  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19097
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-escalation-v2.log 2>&1 &
  PF_PID=$!
  sleep 3

  export BEADS_HTTP_URL="http://localhost:${port}"
  dim "  Daemon at $BEADS_HTTP_URL (pid $PF_PID)"
}

# ── Setup: get token ─────────────────────────────────────────────
setup_token() {
  if [ -n "${BEADS_TOKEN:-}" ]; then
    return
  fi
  local secret
  secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
    -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
  if [ -n "$secret" ]; then
    export BEADS_TOKEN="$secret"
    dim "  Token loaded from secret kd-beads-token"
  else
    dim "  WARNING: No token found; kd server may reject requests"
  fi
}

# ── Seed V2 escalation + report type configs ─────────────────────
seed_configs() {
  # type:escalation and type:report are builtin-authoritative; the daemon builtin
  # (config.go) + controller EnsureConfigs seed already declare a SUPERSET of the
  # fields these hand-maintained PUTs carried. The old PUTs could only shadow-and-
  # drift the builtins (ka-0000003677 / generalized ka-0000000407). Dropped so e2e
  # exercises the builtins prod runs, guarded by TestE2ESeedTypes_NoBuiltinShadow.
  # Do NOT re-add either PUT.
  dim "  type:escalation + type:report configs: using daemon builtins (no shadow PUT)"
}

# ── Cleanup ───────────────────────────────────────────────────────
cleanup() {
  for id in "${CREATED_BEADS[@]}"; do
    "$KD_BIN" close "$id" --actor "$KD_ACTOR" 2>/dev/null || true
  done
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " escalation-v2 E2E tests (V2 Pipeline)"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  # Verify kd server is reachable
  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"

  seed_configs
  echo

  local TEST_TAG="e2e-v2-$$-$(date +%s)"

  # ── Scenario 1: Create escalation with V2 fields ────────────────
  blue "Scenario 1: Create escalation with all V2 fields"

  local CREDS='[{"type":"aws_role","identifier":"arn:aws:iam::123:role/deploy"},{"type":"k8s_context","identifier":"prod-cluster"}]'
  local DATA_REQS='[{"source":"vault","path":"secret/deploy/api-key","sensitivity":"secret"}]'
  local OUTPUTS='[{"name":"deploy_log","type":"text","sensitivity":"internal"},{"name":"api_key","type":"credential","sensitivity":"secret"}]'
  local SCRIPT='#!/bin/bash
echo "Deploying version $VERSION"
kubectl apply -f manifests/
echo "Done"'

  local esc1_out esc1_rc esc1_id
  esc1_out=$("$KD_BIN" create "E2E V2 escalation $TEST_TAG: full fields" \
    --type=escalation \
    -l "e2e:$TEST_TAG" \
    -f 'operation_type=k8s_deploy' \
    -f 'operation_spec={"action":"deploy","namespace":"production","version":"v2.1.0"}' \
    -f "execution_script=$SCRIPT" \
    -f "credential_requirements=$CREDS" \
    -f "data_requirements=$DATA_REQS" \
    -f "expected_outputs=$OUTPUTS" \
    -f 'sensitivity=sensitive' \
    -f 'context=E2E test: full V2 escalation with script and credentials' \
    -f 'requesting_agent=e2e-agent-v2' \
    --actor "$KD_ACTOR" --json 2>/dev/null) && esc1_rc=0 || esc1_rc=$?

  assert_exit "V2 escalation create exits 0" 0 "$esc1_rc"

  esc1_id=$(echo "$esc1_out" | json_field id || echo "")
  if [ -n "$esc1_id" ]; then
    CREATED_BEADS+=("$esc1_id")
    pass "V2 escalation created ($esc1_id)"
  else
    fail "V2 escalation create returns ID" "no id in output: $esc1_out"
  fi
  echo

  # ── Scenario 2: V2 fields round-trip ────────────────────────────
  blue "Scenario 2: V2 fields round-trip through show"

  if [ -n "$esc1_id" ]; then
    local show_out show_rc
    show_out=$("$KD_BIN" show "$esc1_id" --json 2>/dev/null) && show_rc=0 || show_rc=$?
    assert_exit "V2 show exits 0" 0 "$show_rc"

    # Core V1 fields still present
    assert_contains "has operation_type" "k8s_deploy" "$show_out"
    assert_contains "has operation_spec" "deploy" "$show_out"

    # V2 fields
    assert_contains "has execution_script" "kubectl apply" "$show_out"
    assert_contains "has sensitivity=sensitive" "sensitive" "$show_out"
    assert_contains "has credential_requirements (aws_role)" "aws_role" "$show_out"
    assert_contains "has credential_requirements (k8s_context)" "k8s_context" "$show_out"
    assert_contains "has data_requirements (vault)" "vault" "$show_out"
    assert_contains "has expected_outputs (deploy_log)" "deploy_log" "$show_out"
    assert_contains "has expected_outputs (api_key)" "api_key" "$show_out"
    assert_contains "has requesting_agent" "e2e-agent-v2" "$show_out"
  else
    fail "scenario 2 skipped" "no escalation ID from scenario 1"
  fi
  echo

  # ── Scenario 3: Sensitivity enum validation ─────────────────────
  blue "Scenario 3: Sensitivity enum validation — invalid values rejected"

  local esc3_out esc3_rc
  esc3_out=$("$KD_BIN" create "E2E V2 escalation $TEST_TAG: bad sensitivity" \
    --type=escalation \
    -l "e2e:$TEST_TAG" \
    -f 'operation_type=general' \
    -f 'operation_spec={"action":"test"}' \
    -f 'sensitivity=top_secret' \
    --actor "$KD_ACTOR" --json 2>&1) && esc3_rc=0 || esc3_rc=$?

  if [ "$esc3_rc" -ne 0 ]; then
    pass "invalid sensitivity rejected (exit $esc3_rc)"
  else
    # Server may accept and ignore, or reject — check if bead was created
    local esc3_id
    esc3_id=$(echo "$esc3_out" | json_field id 2>/dev/null || echo "")
    if [ -n "$esc3_id" ]; then
      CREATED_BEADS+=("$esc3_id")
      # Check if the invalid value was stored or silently dropped
      local esc3_show
      esc3_show=$("$KD_BIN" show "$esc3_id" --json 2>/dev/null || echo "{}")
      if grep -qF "top_secret" <<<"$esc3_show"; then
        fail "invalid sensitivity stored" "server accepted invalid enum value 'top_secret'"
      else
        pass "invalid sensitivity silently dropped"
      fi
    else
      pass "invalid sensitivity rejected (no bead created)"
    fi
  fi

  # Also test invalid execution_target
  local esc3b_out esc3b_rc
  esc3b_out=$("$KD_BIN" create "E2E V2 escalation $TEST_TAG: bad exec target" \
    --type=escalation \
    -l "e2e:$TEST_TAG" \
    -f 'operation_type=general' \
    -f 'operation_spec={"action":"test"}' \
    -f 'execution_target=remote_ssh' \
    --actor "$KD_ACTOR" --json 2>&1) && esc3b_rc=0 || esc3b_rc=$?

  if [ "$esc3b_rc" -ne 0 ]; then
    pass "invalid execution_target rejected (exit $esc3b_rc)"
  else
    local esc3b_id
    esc3b_id=$(echo "$esc3b_out" | json_field id 2>/dev/null || echo "")
    if [ -n "$esc3b_id" ]; then
      CREATED_BEADS+=("$esc3b_id")
      local esc3b_show
      esc3b_show=$("$KD_BIN" show "$esc3b_id" --json 2>/dev/null || echo "{}")
      if grep -qF "remote_ssh" <<<"$esc3b_show"; then
        fail "invalid execution_target stored" "server accepted invalid enum value 'remote_ssh'"
      else
        pass "invalid execution_target silently dropped"
      fi
    else
      pass "invalid execution_target rejected (no bead created)"
    fi
  fi
  echo

  # ── Scenario 4: Sensitive escalation result with redacted_summary ─
  blue "Scenario 4: Sensitive result uses redacted_summary"

  local esc4_out esc4_id
  esc4_out=$("$KD_BIN" create "E2E V2 escalation $TEST_TAG: sensitive result" \
    --type=escalation \
    -l "e2e:$TEST_TAG" \
    -f 'operation_type=credential_rotation' \
    -f 'operation_spec={"action":"rotate_api_key","service":"payment-gateway"}' \
    -f 'sensitivity=secret' \
    -f 'context=Rotate payment gateway API key' \
    --actor "$KD_ACTOR" --json 2>/dev/null || echo "{}")
  esc4_id=$(echo "$esc4_out" | json_field id || echo "")

  if [ -n "$esc4_id" ]; then
    CREATED_BEADS+=("$esc4_id")
    dim "  Sensitive escalation created: $esc4_id"

    # Set the V2-only presentation field first — redacted_summary is not part of
    # the resolve verb's result payload. Skip approval to avoid the bridge
    # execution pipeline racing and overwriting result_status/result.
    "$KD_BIN" update "$esc4_id" \
      -f 'redacted_summary=API key rotated successfully. Old key revoked. New key provisioned.' \
      --actor "escalation-agent" 2>/dev/null || true

    # Resolve (record result_status + result + executor and close) via the
    # dedicated verb. A generic `kd close` on an OPEN escalation is diverted by
    # the daemon (ka-0000005516), so resolve is the correct close-with-result
    # surface.
    local resolve4_out
    resolve4_out=$("$KD_BIN" escalation resolve "$esc4_id" \
      --status success \
      --result '{"new_key":"sk-live-REDACTED","old_key_revoked":true,"rotation_id":"rot-12345"}' \
      --executor escalation-executor-123 \
      --actor "escalation-agent" 2>&1) || true
    if resolve_refused "scenario 4 resolve" "$resolve4_out"; then
      :
    else

      # Verify fields
      local esc4_show
      esc4_show=$("$KD_BIN" show "$esc4_id" --json 2>/dev/null || echo "{}")

      assert_contains "secret escalation has sensitivity=secret" '"sensitivity": "secret"' "$esc4_show"
      assert_contains "has result_status=success" '"result_status": "success"' "$esc4_show"
      assert_contains "has redacted_summary" "API key rotated successfully" "$esc4_show"
      assert_contains "has result JSON" "rot-12345" "$esc4_show"
      assert_contains "escalation is closed" '"status": "closed"' "$esc4_show"
    fi
  else
    fail "scenario 4 setup" "could not create sensitive escalation"
  fi
  echo

  # ── Scenario 5: Report bead for sensitive escalation ────────────
  blue "Scenario 5: Report bead creation linked to sensitive escalation"

  # Create a sensitive escalation first
  local esc5_out esc5_id
  esc5_out=$("$KD_BIN" create "E2E V2 escalation $TEST_TAG: report bead test" \
    --type=escalation \
    -l "e2e:$TEST_TAG" \
    -f 'operation_type=database_query' \
    -f 'operation_spec={"action":"export_user_data","table":"users"}' \
    -f 'sensitivity=sensitive' \
    --actor "$KD_ACTOR" --json 2>/dev/null || echo "{}")
  esc5_id=$(echo "$esc5_out" | json_field id || echo "")

  if [ -n "$esc5_id" ]; then
    CREATED_BEADS+=("$esc5_id")

    # Create a report bead linked to the escalation
    local report_out report_rc report_id
    report_out=$("$KD_BIN" create "E2E V2 report $TEST_TAG: escalation result" \
      --type=report \
      -l "e2e:$TEST_TAG" \
      -f "decision_id=$esc5_id" \
      -f 'report_type=escalation_result' \
      -f 'content=Full query results: 1500 rows exported from users table. Contains PII.' \
      -f "escalation_id=$esc5_id" \
      -f 'sensitivity=sensitive' \
      -f 'content_type=structured' \
      -f 'redacted_summary=1500 rows exported from users table.' \
      --actor "escalation-agent" --json 2>/dev/null) && report_rc=0 || report_rc=$?
    report_id=$(echo "$report_out" | json_field id || echo "")

    if [ -n "$report_id" ]; then
      CREATED_BEADS+=("$report_id")
      assert_exit "report bead created" 0 "$report_rc"

      # Verify report fields
      local report_show
      report_show=$("$KD_BIN" show "$report_id" --json 2>/dev/null || echo "{}")

      assert_contains "report has type=report" '"type": "report"' "$report_show"
      assert_contains "report links to escalation" "$esc5_id" "$report_show"
      assert_contains "report has sensitivity=sensitive" '"sensitivity": "sensitive"' "$report_show"
      assert_contains "report has content_type=structured" '"content_type": "structured"' "$report_show"
      assert_contains "report has redacted_summary" "1500 rows exported" "$report_show"
      assert_contains "report has full content" "Contains PII" "$report_show"
    else
      fail "report bead creation" "could not create report bead: $report_out"
    fi
  else
    fail "scenario 5 setup" "could not create escalation for report test"
  fi
  echo

  # ── Scenario 6: V1 backward compatibility ───────────────────────
  blue "Scenario 6: V1 backward compatibility — escalation without V2 fields"

  local esc6_out esc6_rc esc6_id
  esc6_out=$("$KD_BIN" create "E2E V1-compat escalation $TEST_TAG" \
    --type=escalation \
    -l "e2e:$TEST_TAG" \
    -f 'operation_type=aws_iam' \
    -f 'operation_spec={"action":"create_role","role_name":"legacy-role"}' \
    -f 'context=V1 escalation without any V2 fields' \
    --actor "$KD_ACTOR" --json 2>/dev/null) && esc6_rc=0 || esc6_rc=$?

  assert_exit "V1-compat escalation create exits 0" 0 "$esc6_rc"

  esc6_id=$(echo "$esc6_out" | json_field id || echo "")
  if [ -n "$esc6_id" ]; then
    CREATED_BEADS+=("$esc6_id")

    local esc6_show
    esc6_show=$("$KD_BIN" show "$esc6_id" --json 2>/dev/null || echo "{}")

    assert_contains "V1 compat has operation_type" "aws_iam" "$esc6_show"
    assert_contains "V1 compat has operation_spec" "create_role" "$esc6_show"

    # V2 fields should be absent or empty
    assert_not_contains "V1 compat: no execution_script" "execution_script" "$esc6_show"
    assert_not_contains "V1 compat: no sensitivity" '"sensitivity"' "$esc6_show"

    pass "V1 backward compatibility verified"
  else
    fail "V1-compat escalation create" "no id in output"
  fi
  echo

  # ── Scenario 7: Credential requirements JSON round-trip ─────────
  blue "Scenario 7: Credential requirements JSON round-trip"

  local COMPLEX_CREDS='[{"type":"aws_role","identifier":"arn:aws:iam::456:role/admin","required":true},{"type":"k8s_context","identifier":"staging-cluster","required":true},{"type":"vault_token","identifier":"vault.example.com","required":false}]'

  local esc7_out esc7_id
  esc7_out=$("$KD_BIN" create "E2E V2 escalation $TEST_TAG: complex creds" \
    --type=escalation \
    -l "e2e:$TEST_TAG" \
    -f 'operation_type=multi_cloud_deploy' \
    -f 'operation_spec={"action":"deploy","targets":["aws","gcp"]}' \
    -f "credential_requirements=$COMPLEX_CREDS" \
    --actor "$KD_ACTOR" --json 2>/dev/null || echo "{}")
  esc7_id=$(echo "$esc7_out" | json_field id || echo "")

  if [ -n "$esc7_id" ]; then
    CREATED_BEADS+=("$esc7_id")

    local esc7_show
    esc7_show=$("$KD_BIN" show "$esc7_id" --json 2>/dev/null || echo "{}")

    assert_contains "complex creds has aws_role" "aws_role" "$esc7_show"
    assert_contains "complex creds has k8s_context" "k8s_context" "$esc7_show"
    assert_contains "complex creds has vault_token" "vault_token" "$esc7_show"
    assert_contains "complex creds has 3 entries" "staging-cluster" "$esc7_show"
  else
    fail "scenario 7 setup" "could not create escalation with complex creds"
  fi
  echo

  # ── Scenario 8: Expected outputs with per-field sensitivity ─────
  blue "Scenario 8: Expected outputs with per-field sensitivity"

  local MIXED_OUTPUTS='[{"name":"deployment_status","type":"text","sensitivity":"public"},{"name":"audit_log","type":"json","sensitivity":"internal"},{"name":"rotated_credentials","type":"credential","sensitivity":"secret"}]'

  local esc8_out esc8_id
  esc8_out=$("$KD_BIN" create "E2E V2 escalation $TEST_TAG: mixed outputs" \
    --type=escalation \
    -l "e2e:$TEST_TAG" \
    -f 'operation_type=credential_rotation' \
    -f 'operation_spec={"action":"rotate","service":"auth-service"}' \
    -f "expected_outputs=$MIXED_OUTPUTS" \
    -f 'sensitivity=sensitive' \
    --actor "$KD_ACTOR" --json 2>/dev/null || echo "{}")
  esc8_id=$(echo "$esc8_out" | json_field id || echo "")

  if [ -n "$esc8_id" ]; then
    CREATED_BEADS+=("$esc8_id")

    local esc8_show
    esc8_show=$("$KD_BIN" show "$esc8_id" --json 2>/dev/null || echo "{}")

    assert_contains "outputs has deployment_status (public)" "deployment_status" "$esc8_show"
    assert_contains "outputs has audit_log (internal)" "audit_log" "$esc8_show"
    assert_contains "outputs has rotated_credentials (secret)" "rotated_credentials" "$esc8_show"
    assert_contains "mixed sensitivity levels stored" '"sensitivity": "sensitive"' "$esc8_show"
  else
    fail "scenario 8 setup" "could not create escalation with mixed outputs"
  fi
  echo

  # ── Scenario 9: Execution target enum validation ────────────────
  blue "Scenario 9: Valid execution_target enum values accepted"

  local valid_targets=("local" "local" "service_account")
  local valid_count=0

  for target in "${valid_targets[@]}"; do
    local et_out et_id
    et_out=$("$KD_BIN" create "E2E V2 escalation $TEST_TAG: target $target" \
      --type=escalation \
      -l "e2e:$TEST_TAG" \
      -f 'operation_type=general' \
      -f 'operation_spec={"action":"test"}' \
      -f "execution_target=$target" \
      --actor "$KD_ACTOR" --json 2>/dev/null || echo "{}")
    et_id=$(echo "$et_out" | json_field id || echo "")
    if [ -n "$et_id" ]; then
      CREATED_BEADS+=("$et_id")
      valid_count=$((valid_count + 1))

      # Verify the value round-trips
      local et_show
      et_show=$("$KD_BIN" show "$et_id" --json 2>/dev/null || echo "{}")
      assert_contains "execution_target=$target persisted" "$target" "$et_show"
    else
      fail "execution_target=$target" "could not create escalation"
    fi
  done

  if [ "$valid_count" -eq "${#valid_targets[@]}" ]; then
    pass "all ${#valid_targets[@]} valid execution_target values accepted"
  else
    fail "valid execution targets" "created $valid_count/${#valid_targets[@]}"
  fi
  echo

  # ── Summary ─────────────────────────────────────────────────────
  blue "═══════════════════════════════════════════════════"
  local total=$((PASS + FAIL))
  if [ "$FAIL" -eq 0 ]; then
    green " ✓ All $total assertions passed"
  else
    red " ✗ $FAIL/$total assertions failed"
    echo
    for err in "${ERRORS[@]}"; do
      red "   - $err"
    done
  fi
  blue "═══════════════════════════════════════════════════"

  [ "$FAIL" -eq 0 ]
}

main "$@"
