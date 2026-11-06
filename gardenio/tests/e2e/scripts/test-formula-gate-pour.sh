#!/usr/bin/env bash
# test-formula-gate-pour.sh — gate-bearing formula pour E2E tests
#
# Closes the recurring "undeclared step field 400s the pour mid-molecule"
# class for gate steps (ka-0000004027; fix ka-0000006567).
#
# THE BUG CLASS. A pour path stamps a field onto a step task bead that the
# builtin type:task schema never declared, so createBead 400s "unknown field"
# and the pour ABORTS MID-MOLECULE — leaving a half-built molecule whose one
# dropped step is precisely the one carrying the new field. Every unit test
# passes because the field list they check is hand-maintained; only a real pour
# against a live daemon exercises the daemon's schema validator on the actual
# stamped blob. This is the THIRD instance of that shape (waits_for/on_complete,
# the step-role dispatch fields, now gate_type/timeout).
#
# THE GATE INSTANCE (ka-0000006567). Both pour paths stamp gate_type (+ timeout
# when set) onto EVERY gate step, but only a `decision` gate flips the step's
# type to "decision" — a `manual` or `sync` gate step keeps the default
# type=task. type:decision already declared gate_type/timeout, so decision gates
# poured fine; a manual/sync gate step 400'd "gate_type: unknown field" on
# type:task and dropped the gate, silently removing the molecule's enforcement
# point (e.g. the PE-ticket-to-completion `gardener-signoff` manual gate).
#
# ANTI-VACUITY. This suite deliberately pours MANUAL and SYNC gates, never a
# `decision` gate — a decision-gate fixture would pass vacuously (type:decision
# always declared both fields) and cover nothing. The gate is the LAST step so a
# regression drops it and fails the step-count assertion, reproducing the
# partial-molecule failure mode rather than a clean top-level error. We assert:
#   1. Every declared step exists on the poured molecule (a COUNT, not just 200).
#   2. The gate step carries gate_type (and timeout when declared) as fields.
#   3. The gate step keeps type=task (proving the non-decision path was taken).
#   4. The gate's blocks edge on its upstream step survives (molecule is gated).
#
# Usage:
#   ./tests/e2e/scripts/test-formula-gate-pour.sh
#
# Prerequisites:
#   - kd binary in PATH (or set KD_BIN)
#   - python3 installed
#   - BEADS_HTTP_URL set (or kubectl context for port-forward)

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
KD_BIN="${KD_BIN:-kd}"
PASS=0
FAIL=0
ERRORS=()
PF_PID=""
CLEANUP_BEADS=()
CLEANUP_FORMULAS=()

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

assert_eq() {
  local name="$1"
  local expected="$2"
  local actual="$3"
  if [ "$expected" = "$actual" ]; then
    pass "$name (== '$expected')"
  else
    fail "$name" "expected '$expected', got '$actual'"
  fi
}

# json_field <json> <dotted.path> — read a nested field, '' if missing.
json_field() {
  local json="$1"
  local field="$2"
  echo "$json" | python3 -c "
import sys, json
data = json.load(sys.stdin)
val = data
for k in '$field'.split('.'):
    if isinstance(val, dict):
        val = val.get(k, '')
    elif isinstance(val, list):
        try:
            val = val[int(k)]
        except (ValueError, IndexError):
            val = ''
            break
    else:
        val = ''
        break
print(val if val is not None else '')
" 2>/dev/null || echo ""
}

# step_field <step-bead-id> <field> — read a custom field off a poured step.
step_field() {
  local id="$1"
  local field="$2"
  "$KD_BIN" show "$id" --json 2>/dev/null | python3 -c "
import sys, json
d = json.load(sys.stdin)
f = d.get('fields', {}) or {}
v = f.get('$field', '')
print(v if v is not None else '')
" 2>/dev/null || echo ""
}

# ── Setup ─────────────────────────────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19151
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-formula-gate-pour.log 2>&1 &
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

cleanup() {
  for id in "${CLEANUP_BEADS[@]}"; do
    "$KD_BIN" close "$id" --actor "$KD_ACTOR" 2>/dev/null || true
  done
  for id in "${CLEANUP_FORMULAS[@]}"; do
    "$KD_BIN" close "$id" --actor "$KD_ACTOR" 2>/dev/null || true
  done
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# create_formula <title> <json-def> — echoes the created formula id ('' on fail).
# Nothing but the id may reach stdout (callers capture via $()), so the caller
# is responsible for registering the returned id via track_formula — a `+=` here
# would run in the command-substitution subshell and never reach the parent's
# CLEANUP_FORMULAS array, leaking the formula bead past the trap.
create_formula() {
  local title="$1"
  local json="$2"
  local out rc=0 err
  err=$(mktemp)
  out=$(echo "$json" | "$KD_BIN" formula create "$title" --file - --json 2>"$err") || rc=$?
  if [ "$rc" -ne 0 ]; then
    dim "    create_formula '$title' exited $rc: $(tr '\n' ' ' < "$err")" >&2
    rm -f "$err"
    echo ""
    return
  fi
  rm -f "$err"
  local fid
  fid=$(json_field "$out" "id")
  if [ -z "$fid" ]; then
    fid=$(echo "$out" | grep -oE '(gd|ka)-[A-Za-z0-9]+' | head -1 || echo "")
  fi
  echo "$fid"
}

# track_formula <formula-id> — register a formula for cleanup from the PARENT
# shell (create_formula cannot, being wrapped in $()).
track_formula() {
  [ -n "$1" ] && CLEANUP_FORMULAS+=("$1")
}

# track_poured <pour-json> — register the molecule + every step for cleanup.
track_poured() {
  local pour_json="$1"
  local ids
  ids=$(echo "$pour_json" | python3 -c "
import sys, json
d = json.load(sys.stdin)
mol = d.get('molecule') or d.get('wisp') or {}
if mol.get('id'):
    print(mol['id'])
for v in (d.get('steps') or {}).values():
    if v:
        print(v)
" 2>/dev/null || echo "")
  for id in $ids; do
    CLEANUP_BEADS+=("$id")
  done
}

# ── Shared gate-pour assertions ───────────────────────────────────
# assert_gate_pour <scenario> <formula-id> <expected-steps> <gate-step-id>
#                  <expected-gate-type> <expected-timeout> <upstream-step-id>
#
# Pours the formula and runs the full anti-vacuity assertion set. Pass an
# empty <expected-timeout> to assert the gate step carries no timeout field.
assert_gate_pour() {
  local scenario="$1" fid="$2" expected_steps="$3" gate_step_id="$4"
  local want_gate_type="$5" want_timeout="$6" upstream_step_id="$7"

  local pour_out pour_rc=0
  pour_out=$("$KD_BIN" formula pour "$fid" --json 2>&1) || pour_rc=$?
  assert_exit "$scenario: pour exits 0 (no mid-molecule 400)" 0 "$pour_rc"
  if [ "$pour_rc" -ne 0 ]; then
    fail "$scenario: pour output" "$(echo "$pour_out" | tr '\n' ' ' | head -c 300)"
    return
  fi
  track_poured "$pour_out"

  # 1. The molecule bead exists.
  local mol_id
  mol_id=$(json_field "$pour_out" "molecule.id")
  if [ -n "$mol_id" ]; then
    pass "$scenario: molecule created ($mol_id)"
  else
    fail "$scenario: molecule created" "no molecule id in pour output"
  fi

  # 2. EVERY declared step exists — a COUNT check. The regression dropped
  #    exactly one step (the gate), so total_steps would be short by one.
  local step_count
  step_count=$(echo "$pour_out" | python3 -c "
import sys, json
print(len((json.load(sys.stdin).get('steps') or {})))
" 2>/dev/null || echo "0")
  assert_eq "$scenario: all $expected_steps steps poured (none dropped mid-molecule)" \
    "$expected_steps" "$step_count"

  # 3. The gate step itself survived and is addressable.
  local gate_id
  gate_id=$(json_field "$pour_out" "steps.$gate_step_id")
  if [ -z "$gate_id" ]; then
    fail "$scenario: gate step poured" \
      "step '$gate_step_id' missing — pour dropped the molecule's enforcement point"
    return
  fi
  pass "$scenario: gate step poured ($gate_id)"

  # 4. The gate step keeps type=task — proving the NON-decision path was
  #    exercised. Only a `decision` gate flips to type=decision; a manual/sync
  #    gate must land on type:task, which is exactly where the field was
  #    undeclared. A decision-gate fixture would pass vacuously here.
  local gate_type_bead
  gate_type_bead=$("$KD_BIN" show "$gate_id" --json 2>/dev/null | \
    python3 -c "import sys,json; print(json.load(sys.stdin).get('type',''))" 2>/dev/null || echo "")
  assert_eq "$scenario: gate step keeps type=task (non-decision path)" "task" "$gate_type_bead"

  # 5. The gate step carries gate_type as a readable field.
  local got_gate_type
  got_gate_type=$(step_field "$gate_id" "gate_type")
  assert_eq "$scenario: gate step carries gate_type=$want_gate_type" \
    "$want_gate_type" "$got_gate_type"

  # 6. timeout: present-and-equal when declared, absent otherwise.
  local got_timeout
  got_timeout=$(step_field "$gate_id" "timeout")
  if [ -n "$want_timeout" ]; then
    assert_eq "$scenario: gate step carries timeout=$want_timeout" \
      "$want_timeout" "$got_timeout"
  else
    assert_eq "$scenario: gate step has no timeout (none declared)" "" "$got_timeout"
  fi

  # 7. The gate's blocks edge on its upstream step survives, so the molecule is
  #    actually GATED (not poured as a bag of independent steps).
  local upstream_id
  upstream_id=$(json_field "$pour_out" "steps.$upstream_step_id")
  local has_blocks
  has_blocks=$("$KD_BIN" dep list "$gate_id" --json 2>/dev/null | python3 -c "
import sys, json
deps = json.load(sys.stdin)
up = '$upstream_id'
print('yes' if any(d.get('type')=='blocks' and d.get('depends_on_id')==up for d in deps) else 'no')
" 2>/dev/null || echo "no")
  assert_eq "$scenario: gate step blocks on its upstream step (molecule is gated)" \
    "yes" "$has_blocks"
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " formula-gate-pour E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  local TEST_TAG="e2e-gate-pour-$$-$(date +%s)"

  # ── Scenario 1: MANUAL gate with timeout ──────────────────────────
  # Models the PE-ticket-to-completion `gardener-signoff` step:
  # {"type":"manual","timeout":"72h"}. Gate is the LAST step so a regression
  # drops it (total_steps=1) rather than surfacing a clean top-level error.
  blue "Scenario 1: pour a MANUAL gate formula (gate_type=manual, timeout=72h)"
  local manual_json
  manual_json=$(cat <<FJSON
{
  "steps": [
    {
      "id": "build",
      "title": "Build $TEST_TAG",
      "type": "task",
      "labels": ["project:e2e-test"]
    },
    {
      "id": "gardener-signoff",
      "title": "Gardener sign-off $TEST_TAG",
      "depends_on": ["build"],
      "labels": ["project:e2e-test"],
      "gate": {"type": "manual", "timeout": "72h"}
    }
  ]
}
FJSON
)
  local manual_fid
  manual_fid=$(create_formula "E2E manual-gate formula $TEST_TAG" "$manual_json")
  track_formula "$manual_fid"
  if [ -z "$manual_fid" ]; then
    fail "scenario 1: create manual-gate formula" "no id returned"
  else
    assert_gate_pour "manual-gate" "$manual_fid" 2 "gardener-signoff" \
      "manual" "72h" "build"
  fi
  echo

  # ── Scenario 2: SYNC gate, no timeout ─────────────────────────────
  # A second non-decision gate kind, and no timeout — asserts the timeout
  # field is genuinely absent (not defaulted) while gate_type still lands.
  blue "Scenario 2: pour a SYNC gate formula (gate_type=sync, no timeout)"
  local sync_json
  sync_json=$(cat <<FJSON
{
  "steps": [
    {
      "id": "fanout",
      "title": "Fan out $TEST_TAG",
      "type": "task",
      "labels": ["project:e2e-test"]
    },
    {
      "id": "join",
      "title": "Join $TEST_TAG",
      "depends_on": ["fanout"],
      "labels": ["project:e2e-test"],
      "gate": {"type": "sync"}
    }
  ]
}
FJSON
)
  local sync_fid
  sync_fid=$(create_formula "E2E sync-gate formula $TEST_TAG" "$sync_json")
  track_formula "$sync_fid"
  if [ -z "$sync_fid" ]; then
    fail "scenario 2: create sync-gate formula" "no id returned"
  else
    assert_gate_pour "sync-gate" "$sync_fid" 2 "join" \
      "sync" "" "fanout"
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
