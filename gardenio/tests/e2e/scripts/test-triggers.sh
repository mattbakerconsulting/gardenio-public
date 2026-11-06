#!/usr/bin/env bash
# test-triggers.sh — trigger system E2E tests
#
# Tests gb trigger CRUD lifecycle against a live kbeads server.
# Covers all four trigger create subcommands (webhook, jira,
# gitlab-pipeline, gitlab-mr) plus list, enable, disable operations.
#
# Usage:
#   ./tests/e2e/scripts/test-triggers.sh
#
# Prerequisites:
#   - gb and kd binaries in PATH (or set GB_BIN / KD_BIN)
#   - python3 installed
#   - BEADS_HTTP_URL set (or kubectl context for port-forward)
#
# Scenarios tested:
#   1. Create webhook trigger with conditions
#   2. Create JIRA trigger with JQL
#   3. Create GitLab pipeline trigger
#   4. Create GitLab MR trigger with filters
#   5. List all triggers
#   6. List triggers filtered by source
#   7. Disable a trigger
#   8. Enable a trigger
#   9. Verify trigger bead fields via kd show
#  10. Create trigger with action=spawn_agent and role
#  11. Create trigger with action=apply_formula and formula_id
#  12. Close trigger bead (cleanup lifecycle)

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
GB_BIN="${GB_BIN:-gb}"
KD_BIN="${KD_BIN:-kd}"
PASS=0
FAIL=0
ERRORS=()
PF_PID=""
CLEANUP_BEADS=()

export KD_ACTOR="${KD_ACTOR:-e2e-test}"

# ── Helpers ───────────────────────────────────────────────────────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

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

# Use pure-bash substring match rather than `echo "$actual" | grep -qF` —
# under `set -o pipefail`, if grep -q matches early and closes its stdin,
# echo on a large payload gets SIGPIPE (rc=141) and the pipeline is
# considered failed, making assert_contains flag a present pattern as
# missing. `gb trigger list --json` returns ~165KB with just-created IDs at
# the top (DESC sort), which triggered exactly this race on every run.
assert_contains() {
  local name="$1"
  local pattern="$2"
  local actual="$3"
  if [[ "$actual" == *"$pattern"* ]]; then
    pass "$name (contains '$pattern')"
  else
    fail "$name" "expected output to contain '$pattern'; got: $(printf '%s' "$actual" | head -5)"
  fi
}

assert_not_contains() {
  local name="$1"
  local pattern="$2"
  local actual="$3"
  if [[ "$actual" != *"$pattern"* ]]; then
    pass "$name (does not contain '$pattern')"
  else
    fail "$name" "expected output to NOT contain '$pattern'"
  fi
}

assert_json_field() {
  local name="$1"
  local json="$2"
  local field="$3"
  local expected="$4"
  local actual
  actual=$(echo "$json" | python3 -c "
import sys, json
d = json.load(sys.stdin)
# Handle nested field access for fields dict
parts = '$field'.split('.')
v = d
for p in parts:
    if isinstance(v, dict):
        v = v.get(p, '')
    else:
        v = ''
        break
print(v)
" 2>/dev/null || echo "")
  if [ "$actual" = "$expected" ]; then
    pass "$name ($field == '$expected')"
  else
    fail "$name" "expected $field='$expected', got '$actual'"
  fi
}

extract_id() {
  local output="$1"
  echo "$output" | grep -oE '(gd|ka)-[A-Za-z0-9]+' | head -1 || echo ""
}

# wait_for_trigger_list — poll `gb trigger list --source <source> --json`
# until the output contains every expected ID, or timeout expires. Echoes
# the final output; returns 0 if all IDs present, 1 on timeout.
#
# Needed because the beads indexer that backs `gb trigger list` can lag
# several seconds behind `kd create` under load (baseline gd-gL9mkEIrq4
# showed 19–20/20 list-after-create failures on staging). A short poll
# absorbs the lag without masking real list bugs.
#
# Usage: out=$(wait_for_trigger_list <source> <timeout_secs> <id1> [id2]...)
wait_for_trigger_list() {
  local source="$1"; shift
  local timeout="$1"; shift
  local ids=("$@")
  local elapsed=0
  local out=""
  local all_present
  local missing
  while :; do
    out=$("$GB_BIN" trigger list --source "$source" --json 2>&1 || true)
    all_present=1
    missing=""
    for id in "${ids[@]}"; do
      [ -z "$id" ] && continue
      # Pure-bash substring check — avoids the SIGPIPE/pipefail race that
      # bit `echo "$out" | grep -qF` on large payloads (see assert_contains).
      if [[ "$out" != *"$id"* ]]; then
        all_present=0
        missing="$missing $id"
      fi
    done
    if [ "$all_present" -eq 1 ]; then
      printf '%s' "$out"
      return 0
    fi
    if [ "$elapsed" -ge "$timeout" ]; then
      dim "    wait_for_trigger_list($source): timeout after ${elapsed}s, missing:$missing" >&2
      printf '%s' "$out"
      return 1
    fi
    sleep 1
    elapsed=$((elapsed + 1))
  done
}

# ── Setup ─────────────────────────────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19115
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-triggers.log 2>&1 &
  PF_PID=$!
  sleep 3
  export BEADS_HTTP_URL="http://localhost:${port}"
  dim "  Daemon at $BEADS_HTTP_URL (pid $PF_PID)"
}

setup_token() {
  if [ -n "${BEADS_TOKEN:-}" ]; then return; fi
  local secret
  secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
    -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
  if [ -n "$secret" ]; then
    export BEADS_TOKEN="$secret"
    dim "  Token loaded from secret kd-beads-token"
  fi
}

ensure_server() {
  if [ -n "$PF_PID" ] && ! kill -0 "$PF_PID" 2>/dev/null; then
    blue "  Re-establishing port-forward..."
    local port=19115
    kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
      >/tmp/kd-pf-triggers.log 2>&1 &
    PF_PID=$!
    sleep 3
  fi
}

# type:trigger is builtin-AUTHORITATIVE. The daemon builtin (config.go) plus the
# controller's startup EnsureConfigs seed (bridge/init.go) already declare every
# field this module's `gb trigger create` needs — webhook_token/webhook_secret/
# conditions, allowed_projects/allowed_roles/jira_project, and much more. The old
# hand-maintained PUT here was a strict SUBSET of that schema, so it could only
# SHADOW-and-drift the builtin and make e2e exercise a schema prod does not run
# (the bug ka-0000003677 closed for the shared seed; generalized in ka-0000000407).
# Dropping it makes e2e exercise the builtin prod runs, guarded by
# TestE2ESeedTypes_NoBuiltinShadow. Do NOT resurrect the PUT.
seed_trigger_type_config() {
  dim "  Trigger type config: using daemon builtin (no shadow PUT)"
}

cleanup() {
  for id in "${CLEANUP_BEADS[@]}"; do
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
  blue " triggers E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"

  seed_trigger_type_config
  echo

  local TEST_TAG="e2e-trig-$$-$(date +%s)"

  # ── Scenario 1: Create webhook trigger ──────────────────────────
  blue "Scenario 1: Create webhook trigger with conditions"

  local wh_out wh_rc=0
  wh_out=$("$GB_BIN" trigger create webhook \
    --title "E2E webhook $TEST_TAG" \
    --token "e2e-test-${TEST_TAG}" \
    --condition "event.type:==:deploy_failed" \
    --condition "severity:==:critical" \
    --action notify \
    --prompt "Deploy failed: {{event.type}}" \
    --cooldown 10m \
    --max-concurrent 2 \
    --json 2>&1) || wh_rc=$?
  assert_exit "webhook create" 0 "$wh_rc"

  local WH_ID
  WH_ID=$(extract_id "$wh_out")
  if [ -n "$WH_ID" ]; then
    pass "webhook trigger created ($WH_ID)"
    CLEANUP_BEADS+=("$WH_ID")
  else
    fail "webhook trigger created" "no ID in output: $wh_out"
  fi
  echo

  # ── Scenario 2: Create JIRA trigger ────────────────────────────
  blue "Scenario 2: Create JIRA trigger with JQL"

  local jira_out jira_rc=0
  jira_out=$("$GB_BIN" trigger create jira \
    --title "E2E jira $TEST_TAG" \
    --jql "project = PE AND status = Open" \
    --jira-project PE \
    --action notify \
    --prompt "JIRA issue found: {{jira_key}}" \
    --cooldown 15m \
    --json 2>&1) || jira_rc=$?
  assert_exit "jira create" 0 "$jira_rc"

  local JIRA_ID
  JIRA_ID=$(extract_id "$jira_out")
  if [ -n "$JIRA_ID" ]; then
    pass "jira trigger created ($JIRA_ID)"
    CLEANUP_BEADS+=("$JIRA_ID")
  else
    fail "jira trigger created" "no ID in output: $jira_out"
  fi
  echo

  # ── Scenario 3: Create GitLab pipeline trigger ─────────────────
  blue "Scenario 3: Create GitLab pipeline trigger"

  local glp_out glp_rc=0
  glp_out=$("$GB_BIN" trigger create gitlab-pipeline \
    --title "E2E pipeline $TEST_TAG" \
    --project "Acme/Platform/gardenio" \
    --status failed \
    --ref main \
    --action notify \
    --prompt "Pipeline failed on main" \
    --json 2>&1) || glp_rc=$?
  assert_exit "gitlab-pipeline create" 0 "$glp_rc"

  local GLP_ID
  GLP_ID=$(extract_id "$glp_out")
  if [ -n "$GLP_ID" ]; then
    pass "gitlab-pipeline trigger created ($GLP_ID)"
    CLEANUP_BEADS+=("$GLP_ID")
  else
    fail "gitlab-pipeline trigger created" "no ID in output: $glp_out"
  fi
  echo

  # ── Scenario 4: Create GitLab MR trigger with filters ──────────
  blue "Scenario 4: Create GitLab MR trigger with filters"

  local glmr_out glmr_rc=0
  glmr_out=$("$GB_BIN" trigger create gitlab-mr \
    --title "E2E mr $TEST_TAG" \
    --project "Acme/Platform/gardenio" \
    --state opened \
    --target-branch main \
    --mr-label "needs-review" \
    --action notify \
    --prompt "New MR needs review" \
    --json 2>&1) || glmr_rc=$?
  assert_exit "gitlab-mr create" 0 "$glmr_rc"

  local GLMR_ID
  GLMR_ID=$(extract_id "$glmr_out")
  if [ -n "$GLMR_ID" ]; then
    pass "gitlab-mr trigger created ($GLMR_ID)"
    CLEANUP_BEADS+=("$GLMR_ID")
  else
    fail "gitlab-mr trigger created" "no ID in output: $glmr_out"
  fi
  echo

  ensure_server

  # ── Scenario 5: List all triggers ──────────────────────────────
  blue "Scenario 5: List all triggers"

  # Use source-filtered list for reliability (unfiltered list may be very large).
  # Verify at least one source returns expected triggers.
  local list_gl_check list_gl_rc=0
  list_gl_check=$("$GB_BIN" trigger list --source gitlab --json 2>&1) || list_gl_rc=$?
  assert_exit "trigger list --source gitlab" 0 "$list_gl_rc"

  # Poll the list for the newly-created triggers before asserting. Under
  # load on the shared e2e namespace the `gb trigger list` call
  # occasionally returns a response that does not yet include just-created
  # triggers (see bug bead referenced in baseline gd-gL9mkEIrq4). A short
  # poll absorbs the transient; `|| true` keeps errexit happy on timeout
  # so the final output still flows into assert_contains.
  list_gl_check=$(wait_for_trigger_list gitlab 30 "$GLP_ID" "$GLMR_ID" || true)

  if [ -n "$GLP_ID" ]; then
    assert_contains "list finds gitlab-pipeline trigger" "$GLP_ID" "$list_gl_check"
  fi
  if [ -n "$GLMR_ID" ]; then
    assert_contains "list finds gitlab-mr trigger" "$GLMR_ID" "$list_gl_check"
  fi
  echo

  # ── Scenario 6: List triggers filtered by source ───────────────
  blue "Scenario 6: List triggers filtered by source"

  local list_wh_out list_wh_rc=0
  list_wh_out=$("$GB_BIN" trigger list --source webhook --json 2>&1) || list_wh_rc=$?
  assert_exit "trigger list --source webhook" 0 "$list_wh_rc"

  # Poll for the webhook trigger in case the indexer is still catching up.
  list_wh_out=$(wait_for_trigger_list webhook 30 "$WH_ID" || true)

  if [ -n "$WH_ID" ]; then
    assert_contains "webhook filter includes webhook" "$WH_ID" "$list_wh_out"
  fi
  if [ -n "$JIRA_ID" ]; then
    assert_not_contains "webhook filter excludes jira" "$JIRA_ID" "$list_wh_out"
  fi

  local list_gl_out list_gl_rc=0
  list_gl_out=$("$GB_BIN" trigger list --source gitlab --json 2>&1) || list_gl_rc=$?
  assert_exit "trigger list --source gitlab" 0 "$list_gl_rc"

  # Poll for both gitlab triggers in case the indexer is still catching up.
  list_gl_out=$(wait_for_trigger_list gitlab 30 "$GLP_ID" "$GLMR_ID" || true)

  if [ -n "$GLP_ID" ]; then
    assert_contains "gitlab filter includes pipeline" "$GLP_ID" "$list_gl_out"
  fi
  if [ -n "$GLMR_ID" ]; then
    assert_contains "gitlab filter includes mr" "$GLMR_ID" "$list_gl_out"
  fi
  if [ -n "$WH_ID" ]; then
    assert_not_contains "gitlab filter excludes webhook" "$WH_ID" "$list_gl_out"
  fi
  echo

  # ── Scenario 7: Disable a trigger ──────────────────────────────
  blue "Scenario 7: Disable a trigger"

  if [ -n "$WH_ID" ]; then
    local disable_out disable_rc=0
    disable_out=$("$GB_BIN" trigger disable "$WH_ID" --json 2>&1) || disable_rc=$?
    assert_exit "trigger disable" 0 "$disable_rc"

    # Verify disabled via kd show
    local show_out
    show_out=$("$KD_BIN" show "$WH_ID" --json 2>&1) || true
    local enabled_val
    enabled_val=$(echo "$show_out" | python3 -c "
import sys, json
d = json.load(sys.stdin)
fields = d.get('fields', {})
v = fields.get('enabled', True)
# Handle both string 'false' and boolean False
print('false' if v is False or str(v).lower() == 'false' else 'true')
" 2>/dev/null || echo "unknown")
    if [ "$enabled_val" = "false" ]; then
      pass "trigger shows enabled=false after disable"
    else
      fail "trigger shows enabled=false after disable" "got enabled=$enabled_val"
    fi
  else
    fail "disable test skipped" "no webhook ID"
  fi
  echo

  # ── Scenario 8: Enable a trigger ───────────────────────────────
  blue "Scenario 8: Enable a trigger"

  if [ -n "$WH_ID" ]; then
    local enable_out enable_rc=0
    enable_out=$("$GB_BIN" trigger enable "$WH_ID" --json 2>&1) || enable_rc=$?
    assert_exit "trigger enable" 0 "$enable_rc"

    # Verify enabled via kd show
    local show_out2
    show_out2=$("$KD_BIN" show "$WH_ID" --json 2>&1) || true
    local enabled_val2
    enabled_val2=$(echo "$show_out2" | python3 -c "
import sys, json
d = json.load(sys.stdin)
fields = d.get('fields', {})
v = fields.get('enabled', False)
# Handle both string 'true' and boolean True
print('true' if v is True or str(v).lower() == 'true' else 'false')
" 2>/dev/null || echo "unknown")
    if [ "$enabled_val2" = "true" ]; then
      pass "trigger shows enabled=true after enable"
    else
      fail "trigger shows enabled=true after enable" "got enabled=$enabled_val2"
    fi
  else
    fail "enable test skipped" "no webhook ID"
  fi
  echo

  ensure_server

  # ── Scenario 9: Verify trigger bead fields via kd show ────────
  blue "Scenario 9: Verify trigger bead fields"

  # Check webhook trigger fields
  if [ -n "$WH_ID" ]; then
    local wh_show wh_show_rc=0
    wh_show=$("$KD_BIN" show "$WH_ID" --json 2>&1) || wh_show_rc=$?
    assert_exit "kd show webhook trigger" 0 "$wh_show_rc"

    # Verify type and kind
    local wh_type
    wh_type=$(echo "$wh_show" | python3 -c "import sys,json; print(json.load(sys.stdin).get('type',''))" 2>/dev/null || echo "")
    if [ "$wh_type" = "trigger" ]; then
      pass "webhook bead type is 'trigger'"
    else
      fail "webhook bead type is 'trigger'" "got '$wh_type'"
    fi

    local wh_kind
    wh_kind=$(echo "$wh_show" | python3 -c "import sys,json; print(json.load(sys.stdin).get('kind',''))" 2>/dev/null || echo "")
    if [ "$wh_kind" = "data" ]; then
      pass "webhook bead kind is 'data'"
    else
      fail "webhook bead kind is 'data'" "got '$wh_kind'"
    fi

    # Verify webhook-specific fields
    local wh_fields
    wh_fields=$(echo "$wh_show" | python3 -c "
import sys, json
d = json.load(sys.stdin)
f = d.get('fields', {})
print(f.get('webhook_token', ''))
" 2>/dev/null || echo "")
    assert_contains "webhook_token field set" "e2e-test-" "$wh_fields"

    local wh_action
    wh_action=$(echo "$wh_show" | python3 -c "
import sys, json
print(json.load(sys.stdin).get('fields', {}).get('action', ''))
" 2>/dev/null || echo "")
    if [ "$wh_action" = "notify" ]; then
      pass "webhook action field is 'notify'"
    else
      fail "webhook action field is 'notify'" "got '$wh_action'"
    fi

    local wh_cooldown
    wh_cooldown=$(echo "$wh_show" | python3 -c "
import sys, json
print(json.load(sys.stdin).get('fields', {}).get('cooldown_minutes', ''))
" 2>/dev/null || echo "")
    if [ "$wh_cooldown" = "10" ]; then
      pass "webhook cooldown_minutes is '10'"
    else
      fail "webhook cooldown_minutes is '10'" "got '$wh_cooldown'"
    fi

    local wh_maxc
    wh_maxc=$(echo "$wh_show" | python3 -c "
import sys, json
print(json.load(sys.stdin).get('fields', {}).get('max_concurrent', ''))
" 2>/dev/null || echo "")
    if [ "$wh_maxc" = "2" ]; then
      pass "webhook max_concurrent is '2'"
    else
      fail "webhook max_concurrent is '2'" "got '$wh_maxc'"
    fi

    # Verify conditions were stored
    local wh_conds
    wh_conds=$(echo "$wh_show" | python3 -c "
import sys, json
print(json.load(sys.stdin).get('fields', {}).get('conditions', ''))
" 2>/dev/null || echo "")
    assert_contains "webhook conditions contain event.type" "event.type" "$wh_conds"
    assert_contains "webhook conditions contain severity" "severity" "$wh_conds"
    assert_contains "webhook conditions contain deploy_failed" "deploy_failed" "$wh_conds"

    # Verify labels
    local wh_labels
    wh_labels=$(echo "$wh_show" | python3 -c "
import sys, json
labels = json.load(sys.stdin).get('labels', [])
print(' '.join(labels))
" 2>/dev/null || echo "")
    assert_contains "webhook has trigger:webhook label" "trigger:webhook" "$wh_labels"
  fi

  # Check JIRA trigger fields
  if [ -n "$JIRA_ID" ]; then
    local jira_show jira_show_rc=0
    jira_show=$("$KD_BIN" show "$JIRA_ID" --json 2>&1) || jira_show_rc=$?

    local jira_jql
    jira_jql=$(echo "$jira_show" | python3 -c "
import sys, json
print(json.load(sys.stdin).get('fields', {}).get('jql', ''))
" 2>/dev/null || echo "")
    assert_contains "jira trigger has JQL field" "project = PE" "$jira_jql"

    local jira_labels
    jira_labels=$(echo "$jira_show" | python3 -c "
import sys, json
labels = json.load(sys.stdin).get('labels', [])
print(' '.join(labels))
" 2>/dev/null || echo "")
    assert_contains "jira has trigger:jira label" "trigger:jira" "$jira_labels"
  fi

  # Check GitLab pipeline trigger fields
  if [ -n "$GLP_ID" ]; then
    local glp_show glp_show_rc=0
    glp_show=$("$KD_BIN" show "$GLP_ID" --json 2>&1) || glp_show_rc=$?

    local glp_project_path
    glp_project_path=$(echo "$glp_show" | python3 -c "
import sys, json
print(json.load(sys.stdin).get('fields', {}).get('project_path', ''))
" 2>/dev/null || echo "")
    if [ "$glp_project_path" = "Acme/Platform/gardenio" ]; then
      pass "pipeline trigger project_path correct"
    else
      fail "pipeline trigger project_path correct" "got '$glp_project_path'"
    fi

    local glp_status
    glp_status=$(echo "$glp_show" | python3 -c "
import sys, json
print(json.load(sys.stdin).get('fields', {}).get('pipeline_status', ''))
" 2>/dev/null || echo "")
    if [ "$glp_status" = "failed" ]; then
      pass "pipeline trigger pipeline_status is 'failed'"
    else
      fail "pipeline trigger pipeline_status is 'failed'" "got '$glp_status'"
    fi

    local glp_ref
    glp_ref=$(echo "$glp_show" | python3 -c "
import sys, json
print(json.load(sys.stdin).get('fields', {}).get('pipeline_ref', ''))
" 2>/dev/null || echo "")
    if [ "$glp_ref" = "main" ]; then
      pass "pipeline trigger pipeline_ref is 'main'"
    else
      fail "pipeline trigger pipeline_ref is 'main'" "got '$glp_ref'"
    fi

    local glp_labels
    glp_labels=$(echo "$glp_show" | python3 -c "
import sys, json
labels = json.load(sys.stdin).get('labels', [])
print(' '.join(labels))
" 2>/dev/null || echo "")
    assert_contains "pipeline has trigger:gitlab label" "trigger:gitlab" "$glp_labels"
    assert_contains "pipeline has trigger-source label" "trigger-source:gitlab-pipeline" "$glp_labels"
  fi

  # Check GitLab MR trigger fields
  if [ -n "$GLMR_ID" ]; then
    local glmr_show glmr_show_rc=0
    glmr_show=$("$KD_BIN" show "$GLMR_ID" --json 2>&1) || glmr_show_rc=$?

    local glmr_state
    glmr_state=$(echo "$glmr_show" | python3 -c "
import sys, json
print(json.load(sys.stdin).get('fields', {}).get('state', ''))
" 2>/dev/null || echo "")
    if [ "$glmr_state" = "opened" ]; then
      pass "mr trigger state is 'opened'"
    else
      fail "mr trigger state is 'opened'" "got '$glmr_state'"
    fi

    local glmr_tb
    glmr_tb=$(echo "$glmr_show" | python3 -c "
import sys, json
print(json.load(sys.stdin).get('fields', {}).get('target_branch', ''))
" 2>/dev/null || echo "")
    if [ "$glmr_tb" = "main" ]; then
      pass "mr trigger target_branch is 'main'"
    else
      fail "mr trigger target_branch is 'main'" "got '$glmr_tb'"
    fi

    local glmr_labels
    glmr_labels=$(echo "$glmr_show" | python3 -c "
import sys, json
labels = json.load(sys.stdin).get('labels', [])
print(' '.join(labels))
" 2>/dev/null || echo "")
    assert_contains "mr has trigger:gitlab label" "trigger:gitlab" "$glmr_labels"
    assert_contains "mr has trigger-source:gitlab-mr label" "trigger-source:gitlab-mr" "$glmr_labels"
  fi
  echo

  ensure_server

  # ── Scenario 10: Create trigger with action=spawn_agent ────────
  blue "Scenario 10: Create trigger with action=spawn_agent and role"

  local spawn_out spawn_rc=0
  spawn_out=$("$GB_BIN" trigger create webhook \
    --title "E2E spawn $TEST_TAG" \
    --token "e2e-spawn-${TEST_TAG}" \
    --action spawn_agent \
    --role crew \
    --agent-project gardenio \
    --prompt "Handle this event" \
    --json 2>&1) || spawn_rc=$?
  assert_exit "spawn_agent trigger create" 0 "$spawn_rc"

  local SPAWN_ID
  SPAWN_ID=$(extract_id "$spawn_out")
  if [ -n "$SPAWN_ID" ]; then
    pass "spawn_agent trigger created ($SPAWN_ID)"
    CLEANUP_BEADS+=("$SPAWN_ID")

    # Verify spawn-specific fields
    local spawn_show
    spawn_show=$("$KD_BIN" show "$SPAWN_ID" --json 2>&1) || true
    local spawn_action
    spawn_action=$(echo "$spawn_show" | python3 -c "
import sys, json
print(json.load(sys.stdin).get('fields', {}).get('action', ''))
" 2>/dev/null || echo "")
    if [ "$spawn_action" = "spawn_agent" ]; then
      pass "spawn trigger action is 'spawn_agent'"
    else
      fail "spawn trigger action is 'spawn_agent'" "got '$spawn_action'"
    fi

    local spawn_role
    spawn_role=$(echo "$spawn_show" | python3 -c "
import sys, json
print(json.load(sys.stdin).get('fields', {}).get('role', ''))
" 2>/dev/null || echo "")
    if [ "$spawn_role" = "crew" ]; then
      pass "spawn trigger role is 'crew'"
    else
      fail "spawn trigger role is 'crew'" "got '$spawn_role'"
    fi
  else
    fail "spawn_agent trigger created" "no ID in output"
  fi
  echo

  # ── Scenario 11: Create trigger with action=apply_formula ──────
  blue "Scenario 11: Create trigger with action=apply_formula"

  # Create a dummy formula first
  local formula_json
  formula_json=$(cat <<FJSON
{
  "vars": [{"name": "ticket", "required": true}],
  "steps": [
    {"id": "fix", "title": "Fix {{ticket}}", "type": "task", "labels": ["project:e2e-test"]}
  ]
}
FJSON
)
  local formula_out formula_rc=0
  formula_out=$(echo "$formula_json" | "$KD_BIN" formula create "E2E trigger formula $TEST_TAG" --file - --json 2>/dev/null) || formula_rc=$?
  local FORMULA_ID=""
  if [ "$formula_rc" -eq 0 ]; then
    FORMULA_ID=$(extract_id "$formula_out")
    if [ -n "$FORMULA_ID" ]; then
      CLEANUP_BEADS+=("$FORMULA_ID")
      dim "  Created test formula: $FORMULA_ID"
    fi
  fi

  if [ -n "$FORMULA_ID" ]; then
    local apply_out apply_rc=0
    apply_out=$("$GB_BIN" trigger create jira \
      --title "E2E formula-action $TEST_TAG" \
      --jql "project = PE AND type = Bug" \
      --action apply_formula \
      --formula "$FORMULA_ID" \
      --agent-project gardenio \
      --var "ticket={{jira_key}}" \
      --json 2>&1) || apply_rc=$?
    assert_exit "apply_formula trigger create" 0 "$apply_rc"

    local APPLY_ID
    APPLY_ID=$(extract_id "$apply_out")
    if [ -n "$APPLY_ID" ]; then
      pass "apply_formula trigger created ($APPLY_ID)"
      CLEANUP_BEADS+=("$APPLY_ID")

      # Verify formula fields
      local apply_show
      apply_show=$("$KD_BIN" show "$APPLY_ID" --json 2>&1) || true
      local apply_action
      apply_action=$(echo "$apply_show" | python3 -c "
import sys, json
print(json.load(sys.stdin).get('fields', {}).get('action', ''))
" 2>/dev/null || echo "")
      if [ "$apply_action" = "apply_formula" ]; then
        pass "formula trigger action is 'apply_formula'"
      else
        fail "formula trigger action is 'apply_formula'" "got '$apply_action'"
      fi

      local apply_fid
      apply_fid=$(echo "$apply_show" | python3 -c "
import sys, json
print(json.load(sys.stdin).get('fields', {}).get('formula_id', ''))
" 2>/dev/null || echo "")
      if [ "$apply_fid" = "$FORMULA_ID" ]; then
        pass "formula trigger formula_id matches"
      else
        fail "formula trigger formula_id matches" "expected '$FORMULA_ID', got '$apply_fid'"
      fi
    else
      fail "apply_formula trigger created" "no ID in output"
    fi
  else
    dim "  Skipping apply_formula test (formula creation failed)"
  fi
  echo

  # ── Scenario 12: Close trigger bead (cleanup lifecycle) ────────
  blue "Scenario 12: Close trigger bead verifies lifecycle"

  if [ -n "$WH_ID" ]; then
    local close_rc=0
    "$KD_BIN" close "$WH_ID" --actor "$KD_ACTOR" 2>/dev/null || close_rc=$?
    assert_exit "close trigger bead" 0 "$close_rc"

    # Verify it's closed
    local closed_show
    closed_show=$("$KD_BIN" show "$WH_ID" --json 2>&1) || true
    local closed_status
    closed_status=$(echo "$closed_show" | python3 -c "
import sys, json
print(json.load(sys.stdin).get('status', ''))
" 2>/dev/null || echo "")
    if [ "$closed_status" = "closed" ]; then
      pass "closed trigger shows status=closed"
    else
      fail "closed trigger shows status=closed" "got '$closed_status'"
    fi

    # Verify the trigger status is closed via kd show
    pass "trigger close lifecycle complete"

    # Remove from cleanup since we already closed it
    local new_cleanup=()
    for id in "${CLEANUP_BEADS[@]}"; do
      if [ "$id" != "$WH_ID" ]; then
        new_cleanup+=("$id")
      fi
    done
    CLEANUP_BEADS=("${new_cleanup[@]}")
  else
    fail "close lifecycle test skipped" "no webhook ID"
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
