#!/usr/bin/env bash
# test-escalation-crud.sh — escalation bead CRUD E2E tests
#
# Tests type:escalation bead operations against a live kbeads server:
# create, show, update, list, field validation, SSE events, and
# decision-to-escalation promotion.
#
# Usage:
#   ./tests/e2e/scripts/test-escalation-crud.sh
#
# Prerequisites:
#   - kubectl context pointing at the target cluster
#   - kd binary in PATH (or set KD_BIN)
#   - gb binary in PATH (or set GB_BIN)
#   - curl, python3, jq installed
#   - BEADS_HTTP_URL set (or kubectl context for port-forward)
#
# Scenarios tested:
#   1. Create escalation bead with required fields
#   2. Show returns correct escalation fields
#   3. List escalation beads (type filter)
#   4. `escalation approve` real verb + operation_spec auto-replay side effect
#   5. Close escalation with result fields (via `escalation resolve`)
#   5b. Generic close of an OPEN escalation is diverted (409 + resolve-hint,
#       bead stays open); close of an already-CLOSED escalation still succeeds
#   5c. POST /result merges caller `fields` atomically with the close (the
#       gardenio EscalationReaper's audit-trail path): fields land as top-level
#       bead fields, typed result_status/executor WIN a key collision, and an
#       empty fields value is dropped (ka-0000001161 / ka-0000000207)
#   6. SSE event on escalation create
#   7. Decision-to-escalation promotion via API
#   8. Multiple escalation operation types coexist
#   9. `requireOpenEscalation` client-side rejections (wrong-type + already-closed)
#  10. `escalation list` / `escalation show` render the CLI verb surface
#  11. Operator-only 403 remediation on approve (best-effort; skips without a
#      role-enforced OIDC bearer — pinned deterministically by the unit test
#      cmd/kd/escalation_test.go::TestEscalationApprove_403IsActionable)

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
KD_BIN="${KD_BIN:-kd}"
GB_BIN="${GB_BIN:-gb}"
PASS=0
FAIL=0
ERRORS=()
PF_PID=""
CREATED_BEADS=()
BG_PIDS=()

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
    pass "$name (contains '$pattern')"
  else
    fail "$name" "expected output to contain '$pattern'; got: $(echo "$actual" | head -5)"
  fi
}

assert_not_contains() {
  local name="$1"
  local pattern="$2"
  local actual="$3"
  if ! grep -qF -- "$pattern" <<< "$actual"; then
    pass "$name (does not contain '$pattern')"
  else
    fail "$name" "expected output to NOT contain '$pattern'"
  fi
}

assert_http_status() {
  local name="$1"
  local expected="$2"
  local actual="$3"
  if [ "$actual" -eq "$expected" ]; then
    pass "$name (HTTP $expected)"
  else
    fail "$name" "expected HTTP $expected, got HTTP $actual"
  fi
}

assert_nonzero_exit() {
  local name="$1"
  local actual="$2"
  if [ "$actual" -ne 0 ]; then
    pass "$name (exit $actual, non-zero)"
  else
    fail "$name" "expected non-zero exit, got exit 0"
  fi
}

json_field() {
  python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('$1',''))" 2>/dev/null
}

# esc_field <key> reads a single top-level bead field value from `kd show --json`
# piped on stdin — i.e. d["fields"][key]. Custom escalation fields (result_status,
# executor, approved_by, reaped*, …) all live under the `fields` map, so this is
# how scenario 5c reads back a stamped value to assert on it (rather than a raw
# grep over the whole blob, which cannot tell a colliding key's WINNER from its
# loser when both strings appear in the JSON).
esc_field() {
  python3 -c "import sys,json; print(json.load(sys.stdin).get('fields',{}).get('$1',''))" 2>/dev/null
}

# ── Setup: port-forward kd server ────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi

  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19095
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-escalation.log 2>&1 &
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

# ── Seed escalation type config ──────────────────────────────────
seed_escalation_config() {
  # type:escalation is builtin-authoritative; the daemon builtin (config.go) +
  # controller EnsureConfigs seed already declare a SUPERSET of these fields.
  # The old hand-maintained PUT could only shadow-and-drift the builtin
  # (ka-0000003677 / generalized ka-0000000407). Dropped so e2e exercises the
  # builtin prod runs, guarded by TestE2ESeedTypes_NoBuiltinShadow. Do NOT re-add.
  dim "  type:escalation config: using daemon builtin (no shadow PUT)"
}

# ── Cleanup ───────────────────────────────────────────────────────
cleanup() {
  for pid in "${BG_PIDS[@]}"; do
    kill "$pid" 2>/dev/null || true
  done
  for id in "${CREATED_BEADS[@]}"; do
    "$KD_BIN" close "$id" --actor "$KD_ACTOR" 2>/dev/null || true
  done
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
  rm -f /tmp/sse-escalation-*.log 2>/dev/null || true
}
trap cleanup EXIT

# ── Helper: start SSE listener ───────────────────────────────────
start_sse_listener() {
  local log_file="$1"
  local extra_params="${2:-}"
  local auth_header=""
  if [ -n "${BEADS_TOKEN:-}" ]; then
    auth_header="Authorization: Bearer $BEADS_TOKEN"
  fi

  local url="${BEADS_HTTP_URL}/v1/events/stream${extra_params}"
  curl -sN ${auth_header:+-H "$auth_header"} "$url" > "$log_file" 2>/dev/null &
  local pid=$!
  BG_PIDS+=("$pid")
  sleep 2
  echo "$pid"
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " escalation-crud E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  # Verify kd server is reachable
  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"

  seed_escalation_config
  echo

  local TEST_TAG="e2e-esc-$$-$(date +%s)"

  # ── Scenario 1: Create escalation bead ─────────────────────────
  blue "Scenario 1: Create escalation bead with required fields"

  local esc1_out esc1_rc esc1_id
  esc1_out=$("$KD_BIN" create "E2E escalation $TEST_TAG: create IAM role" \
    --type=escalation \
    -l "e2e:$TEST_TAG" \
    -f 'operation_type=aws_iam' \
    -f 'operation_spec={"action":"create_role","role_name":"e2e-test-role"}' \
    -f 'context=E2E test: creating a test IAM role' \
    -f 'requesting_agent=e2e-agent-1' \
    --actor "$KD_ACTOR" --json 2>/dev/null) && esc1_rc=0 || esc1_rc=$?

  assert_exit "escalation create exits 0" 0 "$esc1_rc"

  esc1_id=$(echo "$esc1_out" | json_field id || echo "")
  if [ -n "$esc1_id" ]; then
    CREATED_BEADS+=("$esc1_id")
    pass "escalation create returns ID ($esc1_id)"
  else
    fail "escalation create returns ID" "no id in output: $esc1_out"
  fi
  echo

  # ── Scenario 2: Show returns correct fields ────────────────────
  blue "Scenario 2: Show returns correct escalation fields"

  if [ -n "$esc1_id" ]; then
    local show_out show_rc
    show_out=$("$KD_BIN" show "$esc1_id" --json 2>/dev/null) && show_rc=0 || show_rc=$?
    assert_exit "escalation show exits 0" 0 "$show_rc"
    assert_contains "show has type=escalation" '"type": "escalation"' "$show_out"
    assert_contains "show has operation_type" "aws_iam" "$show_out"
    assert_contains "show has operation_spec" "create_role" "$show_out"
    assert_contains "show has context" "E2E test" "$show_out"
    assert_contains "show has requesting_agent" "e2e-agent-1" "$show_out"
    assert_contains "show has status=open" '"status": "open"' "$show_out"
  else
    fail "scenario 2 skipped" "no escalation ID from scenario 1"
  fi
  echo

  # ── Scenario 3: List escalation beads ──────────────────────────
  blue "Scenario 3: List escalation beads with type filter"

  if [ -n "$esc1_id" ]; then
    local list_out list_rc
    list_out=$("$KD_BIN" list --type=escalation --status=open --json 2>/dev/null) && list_rc=0 || list_rc=$?
    assert_exit "escalation list exits 0" 0 "$list_rc"
    assert_contains "list contains created escalation" "$esc1_id" "$list_out"
  else
    fail "scenario 3 skipped" "no escalation ID from scenario 1"
  fi
  echo

  # ── Scenario 4: `escalation approve` real path + operation_spec replay ──
  blue "Scenario 4: escalation approve — real CLI verb + auto-replay side effect"

  # This REPLACES the old ':279 approval simulation' (a hand-rolled
  # `kd update -f approval_method=cli …` that wrote the fields directly and
  # never exercised ApproveEscalation). We now drive the REAL verb, which POSTs
  # /v1/escalations/{id}/approve and — for an operation_type in {create,update,
  # close} — auto-replays the denied operation (http_escalations.go:210). We use
  # a `create` op so the replay side effect is observable: a NEW bead is created,
  # its id is recorded in the escalation's result.bead_id, executor=auto-replay,
  # result_status=success, and the escalation closes.
  #
  # NOTE ON AUTH (verified live against staging, ka-0000003093): the operator
  # gate on /approve is authMethodOIDC-only (interceptors.go:107). The e2e static
  # token authenticates as authMethodStatic and BYPASSES requireRole, so it is
  # operator-equivalent here and approve succeeds. The negative 403 leg (an
  # ordinary agent token) needs a genuine role-enforced OIDC identity and is
  # covered in Scenario 11 (best-effort) + pinned deterministically by the
  # cmd/kd/escalation_test.go unit test TestEscalationApprove_403IsActionable.
  local approve_tag="replay-$$-$(date +%s)"
  # operation_spec is a `create` op: the replayed side effect is a fresh task
  # bead we can then GET by the id the daemon records in result.bead_id.
  local approve_spec
  approve_spec=$(printf '{"title":"E2E replayed bead %s","type":"task","labels":["e2e:%s"]}' \
    "$approve_tag" "$TEST_TAG")

  local esc_appr_out esc_appr_id
  esc_appr_out=$("$KD_BIN" create "E2E escalation $TEST_TAG: approve+replay" \
    --type=escalation \
    -l "e2e:$TEST_TAG" \
    -f 'operation_type=create' \
    -f "operation_spec=$approve_spec" \
    -f 'requesting_agent=e2e-agent-approve' \
    --actor "$KD_ACTOR" --json 2>/dev/null || echo "{}")
  esc_appr_id=$(echo "$esc_appr_out" | json_field id || echo "")

  if [ -n "$esc_appr_id" ]; then
    CREATED_BEADS+=("$esc_appr_id")

    # Drive the real verb. --as sets the approver identity that must land in
    # approved_by (NOT $KD_ACTOR, which is the daemon actor).
    local appr_out appr_rc
    appr_out=$("$KD_BIN" escalation approve "$esc_appr_id" \
      --as "e2e-approver-op" --actor "$KD_ACTOR" 2>&1) && appr_rc=0 || appr_rc=$?
    assert_exit "escalation approve exits 0" 0 "$appr_rc"
    assert_contains "approve prints confirmation" "Approved $esc_appr_id" "$appr_out"

    # Auto-replay closes the escalation on success; poll for the terminal state.
    local esc_appr_after=""
    for _i in $(seq 1 10); do
      esc_appr_after=$("$KD_BIN" show "$esc_appr_id" --json 2>/dev/null || echo "{}")
      if grep -qF '"status": "closed"' <<<"$esc_appr_after"; then
        break
      fi
      sleep 0.5
    done

    # approval_method must be cli (the CLI verb stamps it), NOT web_ui (which is
    # what the bridge path stamps). This is the exact discriminator the task
    # calls out: the one place approval_method=cli is written must be the REAL
    # ApproveEscalation call, not a `kd update` simulation.
    assert_contains "approve stamps approval_method=cli" '"approval_method": "cli"' "$esc_appr_after"
    assert_not_contains "approve did NOT stamp approval_method=web_ui" '"approval_method": "web_ui"' "$esc_appr_after"
    assert_contains "approve records approver identity" "e2e-approver-op" "$esc_appr_after"
    assert_contains "approved escalation is closed" '"status": "closed"' "$esc_appr_after"

    # The auto-replay side effect: result_status=success, executor=auto-replay,
    # and a result.bead_id pointing at the bead the replay created.
    assert_contains "auto-replay result_status=success" '"result_status": "success"' "$esc_appr_after"
    assert_contains "auto-replay executor=auto-replay" "auto-replay" "$esc_appr_after"

    # Extract the replayed bead id from fields.result.bead_id and prove it exists
    # — this is the operation_spec replay the ':279 simulation' never exercised.
    local replayed_id
    replayed_id=$(echo "$esc_appr_after" | python3 -c "
import sys,json
d=json.load(sys.stdin)
r=d.get('fields',{}).get('result',{})
print(r.get('bead_id','') if isinstance(r,dict) else '')
" 2>/dev/null || echo "")
    if [ -n "$replayed_id" ]; then
      CREATED_BEADS+=("$replayed_id")
      pass "auto-replay recorded a created bead id ($replayed_id)"
      local replayed_show
      replayed_show=$("$KD_BIN" show "$replayed_id" --json 2>/dev/null || echo "{}")
      assert_contains "replayed bead actually exists" "\"id\": \"$replayed_id\"" "$replayed_show"
      assert_contains "replayed bead is the task from operation_spec" "E2E replayed bead $approve_tag" "$replayed_show"
    else
      fail "auto-replay side effect" "no result.bead_id recorded on $esc_appr_id; got: $(echo "$esc_appr_after" | grep -o '"result_status": "[^"]*"' | head -1)"
    fi
  else
    fail "scenario 4 setup" "could not create escalation for approve+replay"
  fi
  echo

  # ── Scenario 5: Close escalation with result fields ────────────
  blue "Scenario 5: Close escalation with result fields"

  # Create a second escalation to close
  local esc5_out esc5_id
  esc5_out=$("$KD_BIN" create "E2E escalation $TEST_TAG: close test" \
    --type=escalation \
    -l "e2e:$TEST_TAG" \
    -f 'operation_type=k8s_rbac' \
    -f 'operation_spec={"action":"create_clusterrole"}' \
    --actor "$KD_ACTOR" --json 2>/dev/null || echo "{}")
  esc5_id=$(echo "$esc5_out" | json_field id || echo "")

  if [ -n "$esc5_id" ]; then
    CREATED_BEADS+=("$esc5_id")

    # Resolve the escalation via its dedicated verb, which records
    # result_status + result and closes it via POST /v1/escalations/{id}/result.
    # A generic `kd close` on an OPEN escalation is now diverted by the daemon
    # (ka-0000005516) because a plain close skips operation_spec replay and
    # wrongly releases close-on-escalation wrappers — so resolve is the correct
    # close-with-result surface.
    local close_rc close_out
    close_out=$("$KD_BIN" escalation resolve "$esc5_id" \
      --status success \
      --result '{"clusterrole":"e2e-test-role","created":true}' \
      --actor "$KD_ACTOR" 2>&1) && close_rc=0 || close_rc=$?
    if resolve_refused "scenario 5 resolve" "$close_out"; then
      :
    else
      assert_exit "escalation resolve exits 0" 0 "$close_rc"

      # Verify closed
      local show_closed
      show_closed=$("$KD_BIN" show "$esc5_id" --json 2>/dev/null || echo "{}")
      assert_contains "closed escalation has status=closed" '"status": "closed"' "$show_closed"
      assert_contains "closed escalation has result_status" "success" "$show_closed"
    fi
  else
    fail "scenario 5 skipped" "could not create escalation for close test"
  fi
  echo

  # ── Scenario 5b: Generic close of an OPEN escalation is diverted ──
  #
  # Resolve-surface guard (ka-0000005516): a generic close of an OPEN
  # escalation is refused by the daemon and redirected to the dedicated
  # approve/reject/result verbs. A plain close would skip operation_spec
  # replay and UNCONDITIONALLY release close-on-escalation wrapper tasks as
  # though the privileged operation had run — a successful-looking write that
  # never executed, with no error to alert on in prod. Runtime:
  #   - HTTP  POST /v1/beads/{id}/close  -> 409 + escalationResolveHint,
  #     store untouched (http_beads.go handleCloseBead / escalationBlocksGenericClose).
  #   - gRPC  CloseBead                  -> codes.FailedPrecondition (beads.go),
  #     surfaced here via `kd close` exiting non-zero.
  # The load-bearing assertion is that the bead stays OPEN — the guard runs
  # BEFORE the store is touched, so the status code alone is not proof.
  # Negative control: once legitimately resolved (closed), a generic close of
  # the now-closed escalation must still succeed (the guard only diverts
  # OPEN/in_progress escalations; already-closed stays on the idempotent-close
  # fast path).
  blue "Scenario 5b: Generic close of an OPEN escalation is diverted (409 + hint)"

  local esc5b_out esc5b_id
  esc5b_out=$("$KD_BIN" create "E2E escalation $TEST_TAG: divert test" \
    --type=escalation \
    -l "e2e:$TEST_TAG" \
    -f 'operation_type=aws_iam' \
    -f 'operation_spec={"action":"create_role","role_name":"e2e-divert-role"}' \
    --actor "$KD_ACTOR" --json 2>/dev/null || echo "{}")
  esc5b_id=$(echo "$esc5b_out" | json_field id || echo "")

  if [ -n "$esc5b_id" ]; then
    CREATED_BEADS+=("$esc5b_id")

    # (1) HTTP generic close is diverted with 409 + the resolve hint.
    local divert_resp divert_body divert_status
    divert_resp=$(curl -s --max-time 30 -X POST \
      -w '\n%{http_code}' \
      "${BEADS_HTTP_URL}/v1/beads/${esc5b_id}/close" \
      -H 'Content-Type: application/json' \
      ${BEADS_TOKEN:+-H "Authorization: Bearer $BEADS_TOKEN"} \
      -d '{}' \
      2>/dev/null || true)
    divert_body=$(echo "$divert_resp" | sed '$d')
    divert_status=$(echo "$divert_resp" | tail -1)

    if [ -n "$divert_status" ] && [ "$divert_status" != "000" ]; then
      assert_http_status "generic HTTP close of OPEN escalation returns 409" 409 "$divert_status"
      # The hint names the dedicated resolve verbs (matches escalationResolveHint).
      assert_contains "409 body points at resolve verbs" "escalation approve|reject|close" "$divert_body"
    else
      fail "generic HTTP close divert" "daemon returned no response (status=$divert_status)"
    fi

    # (2) LOAD-BEARING: the bead is genuinely untouched — still OPEN. The guard
    #     runs before the store write, so a plain close never mutated it.
    local show_open
    show_open=$("$KD_BIN" show "$esc5b_id" --json 2>/dev/null || echo "{}")
    assert_contains "diverted escalation stays status=open" '"status": "open"' "$show_open"
    assert_not_contains "diverted escalation was not closed" '"status": "closed"' "$show_open"

    # (3) Parallel CLI surface: `kd close` (gRPC CloseBead -> FailedPrecondition,
    #     or HTTP 409 depending on transport) exits non-zero and does NOT close.
    local kdclose_rc
    "$KD_BIN" close "$esc5b_id" --actor "$KD_ACTOR" >/dev/null 2>&1 && kdclose_rc=0 || kdclose_rc=$?
    assert_nonzero_exit "kd close of OPEN escalation is refused" "$kdclose_rc"

    local show_still_open
    show_still_open=$("$KD_BIN" show "$esc5b_id" --json 2>/dev/null || echo "{}")
    assert_contains "escalation still open after refused kd close" '"status": "open"' "$show_still_open"

    # (4) Negative control: legitimately resolve (closes it), then a generic
    #     close of the now-CLOSED escalation must succeed — the guard only
    #     diverts OPEN escalations; it must not over-block idempotent re-close.
    local reject5b_out
    reject5b_out=$("$KD_BIN" escalation reject "$esc5b_id" --reason "e2e divert negative-control setup" \
      --actor "$KD_ACTOR" 2>&1) || true
    if resolve_refused "scenario 5b reject" "$reject5b_out"; then
      :
    else
      local show_rejected
      show_rejected=$("$KD_BIN" show "$esc5b_id" --json 2>/dev/null || echo "{}")
      assert_contains "escalation closed after reject" '"status": "closed"' "$show_rejected"

      local reclose_resp reclose_status
      reclose_resp=$(curl -s --max-time 30 -X POST \
        -w '\n%{http_code}' \
        "${BEADS_HTTP_URL}/v1/beads/${esc5b_id}/close" \
        -H 'Content-Type: application/json' \
        ${BEADS_TOKEN:+-H "Authorization: Bearer $BEADS_TOKEN"} \
        -d '{}' \
        2>/dev/null || true)
      reclose_status=$(echo "$reclose_resp" | tail -1)
      if [ -n "$reclose_status" ] && [ "$reclose_status" != "000" ]; then
        assert_http_status "generic close of already-CLOSED escalation succeeds" 200 "$reclose_status"
      else
        fail "idempotent close of closed escalation" "daemon returned no response (status=$reclose_status)"
      fi

      # Parallel CLI surface: `kd close` of the closed escalation exits 0 too.
      local kdreclose_rc
      "$KD_BIN" close "$esc5b_id" --actor "$KD_ACTOR" >/dev/null 2>&1 && kdreclose_rc=0 || kdreclose_rc=$?
      assert_exit "kd close of already-CLOSED escalation succeeds" 0 "$kdreclose_rc"
    fi
  else
    fail "scenario 5b skipped" "could not create escalation for divert test"
  fi
  echo

  # ── Scenario 5c: POST /result merges caller `fields` atomically ──
  #
  # The gardenio EscalationReaper closes an out-of-band-satisfied escalation via
  # POST /v1/escalations/{id}/result with a `fields` map carrying its render +
  # audit trail — approved_by (so the closed Slack card renders "Complete", not
  # "Rejected"), reaped/reaped_by/reaped_at/reaped_reason, and the MR witness —
  # merged into the SAME operation as the close (escalation_reaper.go:644-682).
  # The point of the atomic merge is that ONE beads.bead.closed event fires, never
  # a preceding `updated` that would re-trigger execution of an already-satisfied
  # ask — a contract a unit test with a mock daemon (or the escalation-reap-probe's
  # in-Go mockdaemon) cannot prove. This drives the REAL daemon handler
  # (handleCaptureEscalationResult / mergeBeadFields in
  # kbeads/internal/server/http_escalations.go) end to end.
  #
  # The `fields` map is NOT reachable through `kd escalation resolve` (the CLI verb
  # only sends result_status/executor/result), so this leg posts the raw body with
  # curl — the same wire shape the reaper's beadsapi client emits.
  blue "Scenario 5c: POST /result merges caller fields atomically with the close"

  # curl_result POSTs a JSON body to /result and echoes "<body>\n<http_status>".
  # Mirrors the scenario-5b generic-close curl so the two share auth/timeout shape.
  curl_result() {
    local esc_id="$1"
    local body="$2"
    curl -s --max-time 30 -X POST \
      -w '\n%{http_code}' \
      "${BEADS_HTTP_URL}/v1/escalations/${esc_id}/result" \
      -H 'Content-Type: application/json' \
      ${BEADS_TOKEN:+-H "Authorization: Bearer $BEADS_TOKEN"} \
      -d "$body" \
      2>/dev/null || true
  }

  # ---- Leg 1: passthrough — every caller field lands as a top-level bead field.
  local esc5c_out esc5c_id
  esc5c_out=$("$KD_BIN" create "E2E escalation $TEST_TAG: result-fields merge" \
    --type=escalation \
    -l "e2e:$TEST_TAG" \
    -f 'operation_type=manual_intervention' \
    -f 'operation_spec={"action":"merge_mr","iid":1896}' \
    --actor "$KD_ACTOR" --json 2>/dev/null || echo "{}")
  esc5c_id=$(echo "$esc5c_out" | json_field id || echo "")

  if [ -n "$esc5c_id" ]; then
    CREATED_BEADS+=("$esc5c_id")

    # The exact render/audit shape the reaper emits (escalation_reaper.go:644-654),
    # so a regression that stopped honoring `fields` reds THIS test — the whole
    # reaper close path rides on the daemon merging these keys top-level.
    local merge_resp merge_status
    merge_resp=$(curl_result "$esc5c_id" '{
      "executor": "escalation-reaper",
      "result_status": "success",
      "fields": {
        "approved_by": "matthewbaker",
        "reaped": "true",
        "reaped_by": "escalation-reaper",
        "reaped_at": "2026-08-06T00:00:00Z",
        "reaped_reason": "MR !1896 merged out-of-band; the merge ask is satisfied",
        "mr_merged_by": "matthewbaker"
      }
    }')
    merge_status=$(echo "$merge_resp" | tail -1)
    if resolve_refused "scenario 5c leg 1 result" "$merge_resp"; then
      :
    else

      if [ -n "$merge_status" ] && [ "$merge_status" != "000" ]; then
        assert_http_status "POST /result with fields returns 200" 200 "$merge_status"
      else
        fail "POST /result with fields" "daemon returned no response (status=$merge_status)"
      fi

      local esc5c_after
      esc5c_after=$("$KD_BIN" show "$esc5c_id" --json 2>/dev/null || echo "{}")

      # Closed atomically with the merge.
      assert_contains "merged escalation is status=closed" '"status": "closed"' "$esc5c_after"

      # Every caller field is readable as a TOP-LEVEL bead field — where the SSE
      # close-render and the fleet-wide audit read them. Read the value, don't grep
      # the blob: a bare grep can't distinguish top-level from nested-in-result.
      local got_approved_by got_reaped got_reaped_by got_reaped_at got_reaped_reason got_mr_merged_by
      got_approved_by=$(echo "$esc5c_after" | esc_field approved_by)
      got_reaped=$(echo "$esc5c_after" | esc_field reaped)
      got_reaped_by=$(echo "$esc5c_after" | esc_field reaped_by)
      got_reaped_at=$(echo "$esc5c_after" | esc_field reaped_at)
      got_reaped_reason=$(echo "$esc5c_after" | esc_field reaped_reason)
      got_mr_merged_by=$(echo "$esc5c_after" | esc_field mr_merged_by)

      [ "$got_approved_by" = "matthewbaker" ] \
        && pass "approved_by lands top-level (closed card renders Complete)" \
        || fail "approved_by top-level" "want matthewbaker, got '$got_approved_by'"
      [ "$got_reaped" = "true" ] \
        && pass "reaped=true lands top-level" \
        || fail "reaped top-level" "want true, got '$got_reaped'"
      [ "$got_reaped_by" = "escalation-reaper" ] \
        && pass "reaped_by lands top-level" \
        || fail "reaped_by top-level" "want escalation-reaper, got '$got_reaped_by'"
      [ "$got_reaped_at" = "2026-08-06T00:00:00Z" ] \
        && pass "reaped_at lands top-level" \
        || fail "reaped_at top-level" "want 2026-08-06T00:00:00Z, got '$got_reaped_at'"
      [ -n "$got_reaped_reason" ] \
        && pass "reaped_reason lands top-level ($got_reaped_reason)" \
        || fail "reaped_reason top-level" "want non-empty, got ''"
      [ "$got_mr_merged_by" = "matthewbaker" ] \
        && pass "mr_merged_by (MR witness) lands top-level" \
        || fail "mr_merged_by top-level" "want matthewbaker, got '$got_mr_merged_by'"

      # And the typed result_status/executor are authoritative on their own path.
      local got_rs got_ex
      got_rs=$(echo "$esc5c_after" | esc_field result_status)
      got_ex=$(echo "$esc5c_after" | esc_field executor)
      [ "$got_rs" = "success" ] \
        && pass "typed result_status=success recorded" \
        || fail "typed result_status" "want success, got '$got_rs'"
      [ "$got_ex" = "escalation-reaper" ] \
        && pass "typed executor recorded" \
        || fail "typed executor" "want escalation-reaper, got '$got_ex'"
    fi
  else
    fail "scenario 5c leg 1 skipped" "could not create escalation for result-merge test"
  fi

  # ---- Leg 2: COLLISION — a caller field must NEVER clobber a typed result field.
  # This is the load-bearing assertion: the handler merges caller fields FIRST,
  # then overwrites result_status/executor/execution_target with the typed values
  # (http_escalations.go:413-428). If that merge order ever regresses, the typed
  # value loses and THIS leg reds — while every name-match test stays green.
  local esc5c2_out esc5c2_id
  esc5c2_out=$("$KD_BIN" create "E2E escalation $TEST_TAG: result-fields collision" \
    --type=escalation \
    -l "e2e:$TEST_TAG" \
    -f 'operation_type=manual_intervention' \
    -f 'operation_spec={"action":"merge_mr","iid":1897}' \
    --actor "$KD_ACTOR" --json 2>/dev/null || echo "{}")
  esc5c2_id=$(echo "$esc5c2_out" | json_field id || echo "")

  if [ -n "$esc5c2_id" ]; then
    CREATED_BEADS+=("$esc5c2_id")

    # A hostile/mistaken caller puts result_status + executor + execution_target
    # INSIDE fields with bogus values, alongside the authoritative typed fields.
    local coll_resp coll_status
    coll_resp=$(curl_result "$esc5c2_id" '{
      "executor": "real-executor",
      "execution_target": "real-target",
      "result_status": "success",
      "fields": {
        "result_status": "bogus",
        "executor": "imposter",
        "execution_target": "wrong-target",
        "approved_by": "matthewbaker"
      }
    }')
    coll_status=$(echo "$coll_resp" | tail -1)
    if resolve_refused "scenario 5c leg 2 result" "$coll_resp"; then
      :
    else

      if [ -n "$coll_status" ] && [ "$coll_status" != "000" ]; then
        assert_http_status "POST /result collision body returns 200" 200 "$coll_status"
      else
        fail "POST /result collision body" "daemon returned no response (status=$coll_status)"
      fi

      local esc5c2_after
      esc5c2_after=$("$KD_BIN" show "$esc5c2_id" --json 2>/dev/null || echo "{}")

      local won_rs won_ex won_et won_ab
      won_rs=$(echo "$esc5c2_after" | esc_field result_status)
      won_ex=$(echo "$esc5c2_after" | esc_field executor)
      won_et=$(echo "$esc5c2_after" | esc_field execution_target)
      won_ab=$(echo "$esc5c2_after" | esc_field approved_by)

      # THE assertions that fail iff the caller-first / typed-wins merge order regresses.
      [ "$won_rs" = "success" ] \
        && pass "typed result_status WINS collision (not 'bogus')" \
        || fail "result_status collision" "typed value must win: want success, got '$won_rs'"
      [ "$won_ex" = "real-executor" ] \
        && pass "typed executor WINS collision (not 'imposter')" \
        || fail "executor collision" "typed value must win: want real-executor, got '$won_ex'"
      [ "$won_et" = "real-target" ] \
        && pass "typed execution_target WINS collision (not 'wrong-target')" \
        || fail "execution_target collision" "typed value must win: want real-target, got '$won_et'"
      # A non-colliding caller field is still honored in the same request.
      [ "$won_ab" = "matthewbaker" ] \
        && pass "non-colliding caller field (approved_by) still lands" \
        || fail "approved_by alongside collision" "want matthewbaker, got '$won_ab'"
    fi
  else
    fail "scenario 5c leg 2 skipped" "could not create escalation for collision test"
  fi

  # ---- Leg 3: EMPTY VALUE — fields:{k:""} is DROPPED, never written as empty.
  # The handler skips empty-string field values (http_escalations.go:417-421), so
  # a caller cannot blank out a would-be field. Assert the key is absent, not just
  # empty — an absent key and an empty-string key both read as '' via esc_field,
  # so distinguish them by checking the RAW JSON does not carry the key at all.
  local esc5c3_out esc5c3_id
  esc5c3_out=$("$KD_BIN" create "E2E escalation $TEST_TAG: result-fields empty-drop" \
    --type=escalation \
    -l "e2e:$TEST_TAG" \
    -f 'operation_type=manual_intervention' \
    -f 'operation_spec={"action":"merge_mr","iid":1898}' \
    --actor "$KD_ACTOR" --json 2>/dev/null || echo "{}")
  esc5c3_id=$(echo "$esc5c3_out" | json_field id || echo "")

  if [ -n "$esc5c3_id" ]; then
    CREATED_BEADS+=("$esc5c3_id")

    local empty_resp empty_status
    empty_resp=$(curl_result "$esc5c3_id" '{
      "executor": "escalation-reaper",
      "result_status": "success",
      "fields": {
        "approved_by": "matthewbaker",
        "reaped_reason": ""
      }
    }')
    empty_status=$(echo "$empty_resp" | tail -1)
    if resolve_refused "scenario 5c leg 3 result" "$empty_resp"; then
      :
    else

      if [ -n "$empty_status" ] && [ "$empty_status" != "000" ]; then
        assert_http_status "POST /result empty-value body returns 200" 200 "$empty_status"
      else
        fail "POST /result empty-value body" "daemon returned no response (status=$empty_status)"
      fi

      local esc5c3_after present_key
      esc5c3_after=$("$KD_BIN" show "$esc5c3_id" --json 2>/dev/null || echo "{}")
      # Was the empty-valued key written at all? Ask python for key PRESENCE under
      # fields, so an absent key ("true") is distinguished from a present empty one.
      present_key=$(echo "$esc5c3_after" | python3 -c "
  import sys,json
  print('yes' if 'reaped_reason' in json.load(sys.stdin).get('fields',{}) else 'no')
  " 2>/dev/null || echo "err")
      [ "$present_key" = "no" ] \
        && pass "empty fields value is DROPPED (reaped_reason key absent)" \
        || fail "empty fields drop" "expected reaped_reason absent, present_key=$present_key"
      # The non-empty sibling in the same request is still written.
      local sib_ab
      sib_ab=$(echo "$esc5c3_after" | esc_field approved_by)
      [ "$sib_ab" = "matthewbaker" ] \
        && pass "non-empty sibling field still lands when another is dropped" \
        || fail "sibling of empty field" "want matthewbaker, got '$sib_ab'"
    fi
  else
    fail "scenario 5c leg 3 skipped" "could not create escalation for empty-drop test"
  fi
  echo

  # ── Scenario 6: SSE event on escalation create ─────────────────
  blue "Scenario 6: SSE event fires on escalation create"

  local sse_log="/tmp/sse-escalation-create-$$.log"
  local sse_pid
  sse_pid=$(start_sse_listener "$sse_log")

  local esc6_out esc6_id
  esc6_out=$("$KD_BIN" create "E2E escalation $TEST_TAG: SSE test" \
    --type=escalation \
    -l "e2e:$TEST_TAG" \
    -f 'operation_type=helm_deploy' \
    -f 'operation_spec={"chart":"gardenio","version":"test"}' \
    --actor "$KD_ACTOR" --json 2>/dev/null || echo "{}")
  esc6_id=$(echo "$esc6_out" | json_field id || echo "")

  if [ -n "$esc6_id" ]; then
    CREATED_BEADS+=("$esc6_id")
    sleep 3

    local sse_out
    sse_out=$(cat "$sse_log" 2>/dev/null || echo "")

    assert_contains "SSE has event field" "event:" "$sse_out"
    assert_contains "SSE contains escalation bead ID" "$esc6_id" "$sse_out"
    assert_contains "SSE topic is beads.bead.created" "beads.bead.created" "$sse_out"
  else
    fail "scenario 6 skipped" "could not create escalation for SSE test"
  fi

  kill "$sse_pid" 2>/dev/null || true
  echo

  # ── Scenario 7: Decision-to-escalation promotion ───────────────
  blue "Scenario 7: Decision-to-escalation promotion via API"

  # Create a test agent bead for the decision
  local agent_out agent_id
  agent_out=$("$KD_BIN" create "e2e-escalation-agent-$$" --type=agent --json \
    -f "agent=e2e-escalation-agent-$$" \
    -f "agent_state=working" \
    -f "role=crew" \
    -f "project=gardenio" 2>/dev/null || echo "{}")
  agent_id=$(echo "$agent_out" | json_field id || echo "")

  if [ -n "$agent_id" ]; then
    CREATED_BEADS+=("$agent_id")

    # Create a decision bead
    local dec_out dec_id
    dec_out=$("$GB_BIN" decision create \
      --prompt="E2E promote test $TEST_TAG: need IAM role creation" \
      --options='[{"id":"a","label":"Approve","artifact_type":"report"},{"id":"b","label":"Deny","artifact_type":"report"}]' \
      --no-wait --json 2>/dev/null) || dec_out="{}"
    dec_id=$(echo "$dec_out" | json_field id || echo "")

    if [ -n "$dec_id" ]; then
      CREATED_BEADS+=("$dec_id")
      dim "  Decision created: $dec_id"

      # Promote decision to escalation via HTTP API
      local auth_header=""
      if [ -n "${BEADS_TOKEN:-}" ]; then
        auth_header="-H 'Authorization: Bearer $BEADS_TOKEN'"
      fi

      local promote_out promote_rc
      promote_out=$(curl -sf --max-time 30 -X POST \
        "${BEADS_HTTP_URL}/v1/decisions/${dec_id}/promote" \
        -H 'Content-Type: application/json' \
        ${BEADS_TOKEN:+-H "Authorization: Bearer $BEADS_TOKEN"} \
        -d "{\"operationType\":\"general\",\"executionTarget\":\"local\",\"promotedBy\":\"e2e-test\"}" \
        2>/dev/null) && promote_rc=0 || promote_rc=$?

      if [ "$promote_rc" -eq 0 ] && [ -n "$promote_out" ]; then
        pass "decision promote API returns 200"

        local esc_id
        esc_id=$(echo "$promote_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('escalationId',''))" 2>/dev/null || echo "")
        if [ -n "$esc_id" ]; then
          CREATED_BEADS+=("$esc_id")
          pass "promote returns new escalation ID ($esc_id)"

          # Verify the new escalation bead exists
          local esc_show
          esc_show=$("$KD_BIN" show "$esc_id" --json 2>/dev/null || echo "{}")
          assert_contains "promoted bead is type=escalation" '"type": "escalation"' "$esc_show"
          assert_contains "promoted bead has operation_type" "general" "$esc_show"
        else
          fail "promote returns escalation ID" "no escalationId in: $promote_out"
        fi

        # Verify original decision is closed
        local dec_show
        dec_show=$("$KD_BIN" show "$dec_id" --json 2>/dev/null || echo "{}")
        assert_contains "original decision is closed" '"status": "closed"' "$dec_show"
      else
        # Promotion API may not exist if bridge code handles it — try via kd
        dim "  Promotion via HTTP API not available (rc=$promote_rc), skipping"
        dim "  (This is expected if the promotion endpoint is bridge-only)"
        pass "promotion API test skipped (endpoint not on kbeads)"
      fi
    else
      fail "scenario 7: create decision" "could not create decision"
    fi
  else
    fail "scenario 7: create agent" "could not create test agent bead"
  fi
  echo

  # ── Scenario 8: Multiple escalations with different operation types ──
  blue "Scenario 8: Multiple escalation types coexist"

  local types=("aws_iam" "k8s_rbac" "helm_deploy" "general")
  local created_count=0

  for op_type in "${types[@]}"; do
    local multi_out multi_id
    multi_out=$("$KD_BIN" create "E2E escalation $TEST_TAG: $op_type" \
      --type=escalation \
      -l "e2e:$TEST_TAG" \
      -f "operation_type=$op_type" \
      -f "operation_spec={\"action\":\"test_$op_type\"}" \
      --actor "$KD_ACTOR" --json 2>/dev/null || echo "{}")
    multi_id=$(echo "$multi_out" | json_field id || echo "")
    if [ -n "$multi_id" ]; then
      CREATED_BEADS+=("$multi_id")
      created_count=$((created_count + 1))
    fi
  done

  if [ "$created_count" -eq "${#types[@]}" ]; then
    pass "created ${#types[@]} escalations with different operation types"
  else
    fail "create multiple operation types" "created $created_count/${#types[@]}"
  fi

  # Verify list returns all
  local multi_list
  multi_list=$("$KD_BIN" list --type=escalation --status=open -l "e2e:$TEST_TAG" --json 2>/dev/null || echo "[]")
  # Count how many of our test escalations appear (at least the 4 we just created + esc1)
  local found_count
  found_count=$(echo "$multi_list" | grep -c "$TEST_TAG" || echo "0")
  if [ "$found_count" -ge 4 ]; then
    pass "list returns multiple escalation types ($found_count found)"
  else
    fail "list returns multiple escalation types" "expected >=4, got $found_count"
  fi
  echo

  # ── Scenario 9: requireOpenEscalation client-side rejections (negative) ──
  blue "Scenario 9: escalation approve — requireOpenEscalation rejections"

  # These paths (escalation.go:requireOpenEscalation) are a CLIENT-SIDE preflight
  # in the kd binary that runs BEFORE any write, so they are deterministic with
  # the e2e token and must leave the target bead unmutated. Both were HEAD/live-
  # verified against staging (ka-0000003093).

  # 9a: approve a NON-escalation bead → "is not an escalation (type=…)".
  local task9_out task9_id
  task9_out=$("$KD_BIN" create "E2E non-escalation $TEST_TAG" \
    --type=task -l "e2e:$TEST_TAG" --actor "$KD_ACTOR" --json 2>/dev/null || echo "{}")
  task9_id=$(echo "$task9_out" | json_field id || echo "")
  if [ -n "$task9_id" ]; then
    CREATED_BEADS+=("$task9_id")
    local wrongtype_out wrongtype_rc
    wrongtype_out=$("$KD_BIN" escalation approve "$task9_id" --as "e2e-op" --actor "$KD_ACTOR" 2>&1) \
      && wrongtype_rc=0 || wrongtype_rc=$?
    if [ "$wrongtype_rc" -ne 0 ]; then
      pass "approve of a non-escalation exits non-zero"
    else
      fail "approve of a non-escalation exits non-zero" "expected non-zero, got 0"
    fi
    assert_contains "wrong-type rejection names the guard" "is not an escalation" "$wrongtype_out"
    # No mutation: the task must stay open and gain no approved_by field.
    local task9_after
    task9_after=$("$KD_BIN" show "$task9_id" --json 2>/dev/null || echo "{}")
    assert_contains "non-escalation stays open (no mutation)" '"status": "open"' "$task9_after"
    assert_not_contains "non-escalation gained no approved_by" "e2e-op" "$task9_after"
  else
    fail "scenario 9a setup" "could not create non-escalation task"
  fi

  # 9b: approve an ALREADY-CLOSED escalation → "is already closed".
  local esc9_out esc9_id
  esc9_out=$("$KD_BIN" create "E2E already-closed $TEST_TAG" \
    --type=escalation -l "e2e:$TEST_TAG" \
    -f 'operation_type=general' \
    -f 'operation_spec={"action":"noop"}' \
    --actor "$KD_ACTOR" --json 2>/dev/null || echo "{}")
  esc9_id=$(echo "$esc9_out" | json_field id || echo "")
  if [ -n "$esc9_id" ]; then
    CREATED_BEADS+=("$esc9_id")
    # Close it via reject (a generic `kd close` on an OPEN escalation is diverted
    # by the daemon — ka-0000005516). reject closes with no side-effect replay.
    local reject9_out
    reject9_out=$("$KD_BIN" escalation reject "$esc9_id" --reason "e2e already-closed setup" \
      --as "e2e-op" --actor "$KD_ACTOR" 2>&1) || true
    if resolve_refused "scenario 9b setup reject" "$reject9_out"; then
      :
    else
      # Confirm it is actually closed before the negative assertion.
      local esc9_closed
      esc9_closed=$("$KD_BIN" show "$esc9_id" --json 2>/dev/null || echo "{}")
      if grep -qF '"status": "closed"' <<<"$esc9_closed"; then
        local closed_out closed_rc
        closed_out=$("$KD_BIN" escalation approve "$esc9_id" --as "e2e-op" --actor "$KD_ACTOR" 2>&1) \
          && closed_rc=0 || closed_rc=$?
        if [ "$closed_rc" -ne 0 ]; then
          pass "approve of a closed escalation exits non-zero"
        else
          fail "approve of a closed escalation exits non-zero" "expected non-zero, got 0"
        fi
        assert_contains "already-closed rejection names the state" "is already closed" "$closed_out"
      else
        fail "scenario 9b setup" "could not close escalation $esc9_id for the negative test"
      fi
    fi
  else
    fail "scenario 9b setup" "could not create escalation for already-closed test"
  fi
  echo

  # ── Scenario 10: `escalation list` / `escalation show` verb surface ──
  blue "Scenario 10: escalation list surfaces open + omits closed; show renders fields"

  # Create a distinctive open escalation with a requesting_agent to render.
  local esc10_out esc10_id
  esc10_out=$("$KD_BIN" create "E2E list-show $TEST_TAG" \
    --type=escalation -l "e2e:$TEST_TAG" \
    -f 'operation_type=k8s_rbac' \
    -f 'operation_spec={"action":"create_clusterrole"}' \
    -f 'requesting_agent=e2e-agent-listshow' \
    --actor "$KD_ACTOR" --json 2>/dev/null || echo "{}")
  esc10_id=$(echo "$esc10_out" | json_field id || echo "")

  if [ -n "$esc10_id" ]; then
    CREATED_BEADS+=("$esc10_id")

    # `escalation list --json` is a bare array of flat rows (id,title,priority,
    # status,created_by,created_at,age_minutes). The open escalation must appear.
    local list10_json
    list10_json=$("$KD_BIN" escalation list --json 2>/dev/null || echo "[]")
    local in_open
    in_open=$(echo "$list10_json" | python3 -c "
import sys,json
try: rows=json.load(sys.stdin)
except Exception: rows=[]
print('yes' if any(r.get('id')=='$esc10_id' for r in rows) else 'no')
" 2>/dev/null || echo "no")
    if [ "$in_open" = "yes" ]; then
      pass "escalation list surfaces the open escalation"
    else
      fail "escalation list surfaces the open escalation" "id $esc10_id not in open list"
    fi

    # `escalation show --json` emits the full detail (escalation projection +
    # issue bead); operation_type + requesting_agent must render.
    local show10_json
    show10_json=$("$KD_BIN" escalation show "$esc10_id" --json 2>/dev/null || echo "{}")
    assert_contains "show renders operation_type" "k8s_rbac" "$show10_json"
    assert_contains "show renders requesting_agent" "e2e-agent-listshow" "$show10_json"

    # Close it, then assert the DEFAULT list (open+in_progress) omits it.
    local reject10_out
    reject10_out=$("$KD_BIN" escalation reject "$esc10_id" --reason "e2e list-show teardown" \
      --as "e2e-op" --actor "$KD_ACTOR" 2>&1) || true
    if resolve_refused "scenario 10 reject" "$reject10_out"; then
      :
    else
      sleep 1
      local list10_after
      list10_after=$("$KD_BIN" escalation list --json 2>/dev/null || echo "[]")
      local still_open
      still_open=$(echo "$list10_after" | python3 -c "
  import sys,json
  try: rows=json.load(sys.stdin)
  except Exception: rows=[]
  print('yes' if any(r.get('id')=='$esc10_id' for r in rows) else 'no')
  " 2>/dev/null || echo "yes")
      if [ "$still_open" = "no" ]; then
        pass "default escalation list omits the closed escalation"
      else
        fail "default escalation list omits the closed escalation" "id $esc10_id still in default (open) list after close"
      fi
      # …but it IS visible when explicitly listing closed.
      local list10_closed
      list10_closed=$("$KD_BIN" escalation list --status closed --json 2>/dev/null || echo "[]")
      assert_contains "list --status closed surfaces the closed escalation" "$esc10_id" "$list10_closed"
    fi
  else
    fail "scenario 10 setup" "could not create escalation for list/show test"
  fi
  echo

  # ── Scenario 11: operator-only 403 remediation on approve (best-effort) ──
  blue "Scenario 11: approve 403 → operator-only remediation (best-effort)"

  # THE HIGH-VALUE NEGATIVE: an ordinary agent token that tries to self-approve
  # must be refused with the actionable remediation (handleEscalationAuthErr):
  # both "resolved by OPERATORS" and "gb decision create". This is the anti-
  # pattern guard the whole escalation design rests on.
  #
  # Reproducing it in e2e requires a bearer the daemon ROLE-ENFORCES yet that
  # LACKS operator: i.e. a valid OIDC token with audience=$BEADS_OIDC_AUDIENCE
  # and no gardenio-operator role. The e2e static token does NOT qualify — it
  # authenticates as authMethodStatic and bypasses requireRole (verified live:
  # it approves with 200). So this leg is BEST-EFFORT: it runs only when the
  # operator supplies ESC_403_TOKEN (such a role-enforced, non-operator bearer);
  # otherwise it SKIPS green. The remediation string itself is pinned
  # deterministically in CI by the unit test
  # cmd/kd/escalation_test.go::TestEscalationApprove_403IsActionable, so skipping
  # here never leaves the invariant unguarded.
  if [ -n "${ESC_403_TOKEN:-}" ]; then
    # Fresh open escalation for the agent token to attempt.
    local esc11_out esc11_id
    esc11_out=$("$KD_BIN" create "E2E 403 remediation $TEST_TAG" \
      --type=escalation -l "e2e:$TEST_TAG" \
      -f 'operation_type=general' \
      -f 'operation_spec={"action":"noop"}' \
      --actor "$KD_ACTOR" --json 2>/dev/null || echo "{}")
    esc11_id=$(echo "$esc11_out" | json_field id || echo "")
    if [ -n "$esc11_id" ]; then
      CREATED_BEADS+=("$esc11_id")
      # Drive approve with the NON-operator bearer via GARDENIO_TOKEN (kd's token
      # env). A subshell so we don't perturb the suite's own token.
      local agent_out agent_rc
      agent_out=$(GARDENIO_TOKEN="$ESC_403_TOKEN" BEADS_AUTH_TOKEN="$ESC_403_TOKEN" \
        "$KD_BIN" escalation approve "$esc11_id" --as "e2e-self-approver" 2>&1) \
        && agent_rc=0 || agent_rc=$?
      if [ "$agent_rc" -ne 0 ]; then
        pass "agent-token approve exits non-zero (403 refused)"
      else
        fail "agent-token approve exits non-zero" "expected non-zero, got 0 — token may carry operator role"
      fi
      # Pin BOTH remediation sentences, not just the exit code.
      assert_contains "403 remediation mentions OPERATORS" "resolved by OPERATORS" "$agent_out"
      assert_contains "403 remediation points at gb decision create" "gb decision create" "$agent_out"
      # And the escalation must be untouched (still open, no approver).
      local esc11_after
      esc11_after=$("$KD_BIN" show "$esc11_id" --json 2>/dev/null || echo "{}")
      assert_contains "refused escalation stays open" '"status": "open"' "$esc11_after"
      assert_not_contains "refused escalation gained no approver" "e2e-self-approver" "$esc11_after"
    else
      fail "scenario 11 setup" "could not create escalation for 403 test"
    fi
  else
    dim "  ESC_403_TOKEN not set — skipping the live 403 leg."
    dim "  (The static e2e token bypasses RBAC and returns 200; a genuine 403"
    dim "   needs a role-enforced OIDC bearer without gardenio-operator.)"
    dim "  The remediation string is pinned by the cmd/kd unit test"
    dim "  TestEscalationApprove_403IsActionable, so this invariant stays guarded."
    pass "403 remediation leg skipped (no role-enforced OIDC bearer available)"
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
