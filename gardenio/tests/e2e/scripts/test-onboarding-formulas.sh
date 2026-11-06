#!/usr/bin/env bash
# test-onboarding-formulas.sh — pour each onboarding formula and work it to its gate
#
# The four formulas in gardenio/formulas/onboarding/ (gd-Z4fkVbFhPY) are what a
# new person pours to get a repo onto gardenio. Each mixes task steps an agent
# can run with gated steps only a person can pass: manual gates (sign in,
# install the Slack app, wire credentials) and decision gates (pick the
# account, the role, the first task). This suite pours every formula against a
# live kbeads daemon and has a claudeless local agent work the molecule up to
# its first gate, so a formula edit that drops a step or a gate goes red here.
#
# Per formula, against an expectation pinned in formula_spec() below (NOT read
# back from the formula file, so removing a step or a gate from the file fails):
#
#   1. Pour. `kd formula pour <file>` (the path the gardenio skill documents)
#      exits 0 and creates EVERY declared step: the poured step set is compared
#      to the pinned list, a count and the names (model: test-formula-gate-pour).
#   2. Gates. Every pinned gate step carries gate_type, a decision gate pours
#      as type=decision and a manual gate as type=task, and no other step is
#      gated.
#   3. Frontier. The molecule's open, unblocked steps are exactly the pinned
#      first frontier. Where that is a sign-in gate, the runner closes it as
#      the person would.
#   4. Agent. `gb agent start --local --test-scenario=onboarding-<formula>`
#      (model: test-local-mode.sh) runs a claudeless agent that claims each
#      ready task step, runs the command the step names, comments the output,
#      closes it, and stops. Its tool calls go through onboarding-agent-step.sh,
#      which refuses to claim a gate or a step outside `gb ready --plan`.
#   5. Gate reached. The agent's steps are closed with the agent as assignee
#      and a comment of the command it ran; the stop gate is still open with
#      its gate_type; the frontier is now exactly that gate; and every other
#      step is still open.
#
# Anti-vacuity: the suite finally pours two mutants of first-task.json, one
# with a step removed and one with the decision gate dropped, and asserts the
# pour checks (1 and 2) report a failure for each.
#
# Everything is poured under a throwaway project label (project:e2e-onboard-…)
# and every poured bead is closed and deleted on exit.
#
# The daemon does not refuse `kd close` on a step with open blockers, so a gate
# holds only because agents work from `gb ready`. That is why step 2 pins every
# gate rather than trusting the agent to stop.
#
# Usage:
#   BEADS_HTTP_URL=http://localhost:8080 ./gardenio/tests/e2e/scripts/test-onboarding-formulas.sh
#     or
#   NAMESPACE=gardenio-e2e ./gardenio/tests/e2e/scripts/test-onboarding-formulas.sh  # port-forwards
#
# Running it by hand against a local `kd serve` is in gardenio/tests/e2e/README.md.
#
# Prerequisites:
#   - gb, kd, coop, claudeless binaries in PATH (the module skips without them)
#   - python3
#   - A reachable beads daemon (BEADS_HTTP_URL or kubectl context)

set -euo pipefail

# kbeads refuses writes without a human-shaped operator (ka-0000005772,
# ka-0000002601); see test-local-mode.sh.
export BEADS_OPERATED_BY="${BEADS_OPERATED_BY:-oidc:e2e-onboarding@example.com}"
export KD_ACTOR="${KD_ACTOR:-e2e-test}"

# ── Config ────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCENARIO_DIR="$SCRIPT_DIR/../claudeless"
# Monorepo root: .../gardenio/tests/e2e/scripts → repo root
WORK_DIR="$(cd "$SCRIPT_DIR/../../../.." && pwd)"
FORMULA_DIR="${ONBOARD_FORMULA_DIR:-$WORK_DIR/gardenio/formulas/onboarding}"
AGENT_STEP="$SCRIPT_DIR/onboarding-agent-step.sh"
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
KD_BIN="${KD_BIN:-kd}"
GB_BIN="${GB_BIN:-gb}"
SCENARIO_TIMEOUT="${SCENARIO_TIMEOUT:-120}"
TEST_TAG="e2e-onboard-$$-$(date +%s)"
PROJECT="$TEST_TAG"
TMP_DIR="$(mktemp -d)"
# The agent's --dir. gb agent start --local writes .claude/settings.local.json and
# .state/ into it, so it gets a throwaway repo rather than this checkout.
AGENT_DIR="$TMP_DIR/repo"
GIT_URL="https://example.invalid/e2e-onboarding.git"
# Poured bead ids, one per line. A file rather than an array so pours made in
# the mutant subshells are cleaned up too.
CLEANUP_FILE="$TMP_DIR/cleanup.ids"
: >"$CLEANUP_FILE"
PF_PID=""
PASS=0
FAIL=0
ERRORS=()

# ── Helpers ───────────────────────────────────────────────────────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

pass() { green "  ✓ $1"; PASS=$((PASS + 1)); }
fail() {
  local name="$1" reason="${2:-}"
  red "  ✗ $name"
  [ -n "$reason" ] && dim "    $reason"
  FAIL=$((FAIL + 1))
  ERRORS+=("$name: $reason")
}

assert_eq() {
  local name="$1" expected="$2" actual="$3"
  if [ "$expected" = "$actual" ]; then
    pass "$name (== '$expected')"
  else
    fail "$name" "expected '$expected', got '$actual'"
  fi
}

have() { command -v "$1" >/dev/null 2>&1; }

skip_module() {
  blue "SKIP onboarding-formulas: $1"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

# ── Expectations ──────────────────────────────────────────────────
# formula_spec <formula> — pin what a pour of <formula> must look like. Sets:
#   SPEC_STEPS     every step id the formula declares
#   SPEC_GATES     <step>=<gate type> for every gated step
#   SPEC_FRONTIER  open, unblocked steps right after the pour
#   SPEC_PERSON    frontier gates the runner closes as the person (sign-in)
#   SPEC_AGENT     task steps the agent claims, runs and closes
#   SPEC_STOP      the gate the agent stops at
#   SPEC_VARS      --var arguments for the pour
# Edit this when a formula changes on purpose; a change it does not expect is
# what this suite exists to catch.
formula_spec() {
  SPEC_VARS=(--var "project=$PROJECT")
  case "$1" in
    register-project)
      SPEC_STEPS="sign-in check-setup list-accounts pick-account dry-run create wire-credentials verify"
      SPEC_GATES="sign-in=manual pick-account=decision wire-credentials=manual"
      SPEC_FRONTIER="sign-in"
      SPEC_PERSON="sign-in"
      SPEC_AGENT="check-setup list-accounts"
      SPEC_STOP="pick-account"
      SPEC_VARS+=(--var "git_url=$GIT_URL"
                  --var "account=e2e-onboarding-account")
      ;;
    add-local-agent)
      SPEC_STEPS="sign-in doctor list-roles pick-role start verify-roster"
      SPEC_GATES="sign-in=manual pick-role=decision"
      SPEC_FRONTIER="sign-in"
      SPEC_PERSON="sign-in"
      SPEC_AGENT="doctor list-roles"
      SPEC_STOP="pick-role"
      SPEC_VARS+=(--var "role=crew")
      ;;
    first-task)
      SPEC_STEPS="suggest pick-task create-bead assign confirm-loop review-pr"
      SPEC_GATES="pick-task=decision review-pr=manual"
      SPEC_FRONTIER="suggest"
      SPEC_PERSON=""
      SPEC_AGENT="suggest"
      SPEC_STOP="pick-task"
      SPEC_VARS+=(--var "role=crew")
      ;;
    enable-slack)
      SPEC_STEPS="install-app bridge-tokens invite-app find-project bind-channel verify"
      SPEC_GATES="install-app=manual bridge-tokens=manual invite-app=manual bind-channel=manual"
      SPEC_FRONTIER="find-project install-app"
      SPEC_PERSON=""
      SPEC_AGENT="find-project"
      SPEC_STOP="install-app"
      SPEC_VARS+=(--var "channel_id=C0E2E0NB0RD")
      ;;
    *)
      echo "formula_spec: unknown formula $1" >&2
      return 1
      ;;
  esac
}

# sorted_words <words…> — the words, sorted and space-joined, for set compares.
sorted_words() { printf '%s\n' "$@" | sed '/^$/d' | sort | tr '\n' ' ' | sed 's/ $//'; }

# ── Daemon reads ──────────────────────────────────────────────────
# bead_json <id> — kd show --json, '{}' when unreachable.
bead_json() { "$KD_BIN" show "$1" --json 2>/dev/null || echo '{}'; }

# bead_get <json> <expr> — evaluate a python expression over the bead dict `b`.
bead_get() {
  echo "$1" | python3 -c "
import sys, json
b = json.load(sys.stdin)
f = b.get('fields') or {}
v = $2
print(v if v is not None else '')
" 2>/dev/null || echo ""
}

# frontier <molecule-id> — source_step_ids of the molecule's open, unblocked
# steps, sorted. --all-types because a decision gate is kind=data and the
# default issue-kind filter hides it.
frontier() {
  "$KD_BIN" list --parent "$1" --no-blockers --all-types -s open --json 2>/dev/null | python3 -c "
import sys, json
rows = json.load(sys.stdin) or []
print(' '.join(sorted((r.get('fields') or {}).get('source_step_id', '?') for r in rows)))
" 2>/dev/null || echo "<unreachable>"
}

# ── Setup ─────────────────────────────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19152
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-onboarding-formulas.log 2>&1 &
  PF_PID=$!
  sleep 3
  export BEADS_HTTP_URL="http://localhost:${port}"
  dim "  Daemon at $BEADS_HTTP_URL (pid $PF_PID)"
}

setup_token() {
  if [ -n "${BEADS_TOKEN:-}" ]; then return; fi
  if ! have kubectl; then return; fi
  local secret
  secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
    -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
  if [ -n "$secret" ]; then
    export BEADS_TOKEN="$secret"
    dim "  Token loaded from secret kd-beads-token"
  fi
}

cleanup() {
  local id
  while read -r id; do
    [ -n "$id" ] || continue
    "$KD_BIN" close "$id" >/dev/null 2>&1 || true
    "$KD_BIN" delete "$id" >/dev/null 2>&1 || true
  done < <(sort -u "$CLEANUP_FILE" 2>/dev/null)
  rm -rf "$TMP_DIR"
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# ── Pour ──────────────────────────────────────────────────────────
# pour_and_check <formula> <file> — pour <file> and run checks 1 and 2 against
# formula_spec <formula>. Sets MOL (molecule id, '' on failure) and STEP_IDS
# ("<step>=<bead-id> …").
pour_and_check() {
  local name="$1" file="$2"
  MOL="" STEP_IDS=""
  formula_spec "$name"

  local out rc=0
  out=$("$KD_BIN" formula pour "$file" "${SPEC_VARS[@]}" -l "project:$PROJECT" --json 2>&1) || rc=$?
  if [ "$rc" -ne 0 ]; then
    fail "$name: pour exits 0" "exit $rc: $(echo "$out" | tr '\n' ' ' | head -c 300)"
    return
  fi
  pass "$name: pour exits 0"

  echo "$out" | python3 -c "
import sys, json
d = json.load(sys.stdin)
mol = d.get('molecule') or {}
if mol.get('id'):
    print(mol['id'])
for v in (d.get('steps') or {}).values():
    print(v)
" >>"$CLEANUP_FILE" 2>/dev/null || true

  MOL=$(echo "$out" | python3 -c "import sys,json; print((json.load(sys.stdin).get('molecule') or {}).get('id',''))" 2>/dev/null || echo "")
  STEP_IDS=$(echo "$out" | python3 -c "
import sys, json
print(' '.join(f'{k}={v}' for k, v in sorted((json.load(sys.stdin).get('steps') or {}).items())))
" 2>/dev/null || echo "")
  if [ -z "$MOL" ]; then
    fail "$name: molecule created" "no molecule id in pour output"
    return
  fi
  pass "$name: molecule created ($MOL)"

  # 1. Every declared step exists: count and names.
  local want_steps got_steps
  # shellcheck disable=SC2086
  want_steps=$(sorted_words $SPEC_STEPS)
  # shellcheck disable=SC2046,SC2086
  got_steps=$(sorted_words $(printf '%s\n' $STEP_IDS | cut -d= -f1))
  assert_eq "$name: all $(wc -w <<<"$SPEC_STEPS" | tr -d ' ') steps poured" "$want_steps" "$got_steps"

  # 2. Gates: every pinned gate carries its gate_type on the right bead type,
  #    and no unpinned step is gated.
  local pair step want_gate id json got_gate got_type want_type
  for step in $SPEC_STEPS; do
    id=$(step_id "$step")
    [ -n "$id" ] || continue
    json=$(bead_json "$id")
    got_gate=$(bead_get "$json" "f.get('gate_type', '')")
    want_gate=""
    for pair in $SPEC_GATES; do
      if [ "${pair%%=*}" = "$step" ]; then want_gate="${pair#*=}"; fi
    done
    if [ -z "$want_gate" ]; then
      assert_eq "$name: $step is not gated" "" "$got_gate"
      continue
    fi
    assert_eq "$name: $step carries gate_type=$want_gate" "$want_gate" "$got_gate"
    got_type=$(bead_get "$json" "b.get('type', '')")
    want_type=task
    if [ "$want_gate" = "decision" ]; then want_type=decision; fi
    assert_eq "$name: $step pours as type=$want_type" "$want_type" "$got_type"
  done
}

# step_id <step> — bead id of <step> in the last pour.
step_id() {
  local pair
  for pair in $STEP_IDS; do
    if [ "${pair%%=*}" = "$1" ]; then
      echo "${pair#*=}"
      return
    fi
  done
}

# ── Agent ─────────────────────────────────────────────────────────
# setup_agent_dir — the repo the agent runs in: one tracked README.md and an
# origin of $GIT_URL, enough for the git commands the steps name.
setup_agent_dir() {
  mkdir -p "$AGENT_DIR"
  git -C "$AGENT_DIR" init -q
  git -C "$AGENT_DIR" remote add origin "$GIT_URL"
  echo "# e2e onboarding" >"$AGENT_DIR/README.md"
  git -C "$AGENT_DIR" add README.md
  git -C "$AGENT_DIR" -c user.name=e2e -c user.email=e2e@example.invalid commit -qm init
}

# run_agent <formula> — run the onboarding-<formula> claudeless scenario as a
# local agent on $MOL. Sets AGENT_NAME.
run_agent() {
  local name="$1"
  local scenario="onboarding-$name"
  local state="$TMP_DIR/$name"
  local log="$TMP_DIR/$name.agent.log"
  mkdir -p "$state"
  AGENT_NAME="$TEST_TAG-$name"

  local rc=0
  env ONBOARD_MOLECULE="$MOL" ONBOARD_STATE_DIR="$state" ONBOARD_AGENT_STEP="$AGENT_STEP" \
    timeout "$SCENARIO_TIMEOUT" "$GB_BIN" agent start --local \
      --name="$AGENT_NAME" \
      --role=crew \
      --dir="$AGENT_DIR" \
      --test-scenario="$scenario" \
      --test-scenario-dir="$SCENARIO_DIR" \
      </dev/null >"$log" 2>&1 || rc=$?

  if [ "$rc" -eq 0 ]; then
    pass "$name: agent scenario passed (gb agent start exited 0)"
  else
    local why="exit $rc"
    if [ "$rc" -eq 124 ]; then why="timed out after ${SCENARIO_TIMEOUT}s"; fi
    fail "$name: agent scenario passed" "$why; agent log: $(tr '\n' ' ' <"$state/agent.log" 2>/dev/null)"
    dim "  --- $log (last 40 lines) ---"
    tail -n 40 "$log" | sed 's/^/      /'
  fi

  local agent_id
  agent_id=$(grep -oE 'Agent bead:[[:space:]]+(gd|ka)-[A-Za-z0-9]+' "$log" 2>/dev/null | head -n1 | awk '{print $NF}' || true)
  if [ -z "$agent_id" ]; then
    fail "$name: agent bead registered" "no 'Agent bead:' banner in $log"
    return
  fi
  echo "$agent_id" >>"$CLEANUP_FILE"
  assert_eq "$name: agent bead $agent_id closed on exit" "closed" \
    "$(bead_get "$(bead_json "$agent_id")" "b.get('status', '')")"
}

# ── One formula ───────────────────────────────────────────────────
check_formula() {
  local name="$1"
  local file="$FORMULA_DIR/$name.json"
  blue "Formula: $name ($file)"

  if [ ! -f "$file" ]; then
    fail "$name: formula file exists" "$file not found"
    return
  fi

  pour_and_check "$name" "$file"
  [ -n "$MOL" ] || return 0

  # 3. The first frontier is the pinned one; the person passes a sign-in gate.
  # shellcheck disable=SC2086
  assert_eq "$name: ready frontier after pour" "$(sorted_words $SPEC_FRONTIER)" "$(frontier "$MOL")"
  local step id
  for step in $SPEC_PERSON; do
    id=$(step_id "$step")
    if [ -n "$id" ] && "$KD_BIN" close "$id" >/dev/null 2>&1; then
      pass "$name: person passes the $step gate"
    else
      fail "$name: person passes the $step gate" "kd close ${id:-<missing>} failed"
    fi
  done

  # 4. The agent works the molecule.
  run_agent "$name"

  # 5. The agent closed its steps and stopped at the gate.
  local json
  for step in $SPEC_AGENT; do
    id=$(step_id "$step")
    json=$(bead_json "$id")
    assert_eq "$name: agent closed $step" "closed" "$(bead_get "$json" "b.get('status', '')")"
    assert_eq "$name: $step was claimed by the agent" "$AGENT_NAME" "$(bead_get "$json" "b.get('assignee', '')")"
    assert_eq "$name: $step carries the agent's command output" "yes" \
      "$(bead_get "$json" "'yes' if any((c.get('text') or '').startswith('Ran \`') and '(exit 0)' in (c.get('text') or '') for c in (b.get('comments') or [])) else 'no'")"
  done

  id=$(step_id "$SPEC_STOP")
  json=$(bead_json "$id")
  local want_gate=""
  local pair
  for pair in $SPEC_GATES; do
    if [ "${pair%%=*}" = "$SPEC_STOP" ]; then want_gate="${pair#*=}"; fi
  done
  assert_eq "$name: gate $SPEC_STOP left open" "open" "$(bead_get "$json" "b.get('status', '')")"
  assert_eq "$name: gate $SPEC_STOP keeps gate_type=$want_gate" "$want_gate" "$(bead_get "$json" "f.get('gate_type', '')")"
  assert_eq "$name: ready frontier is the $SPEC_STOP gate" "$SPEC_STOP" "$(frontier "$MOL")"

  local done_steps=" $SPEC_PERSON $SPEC_AGENT "
  for step in $SPEC_STEPS; do
    case "$done_steps" in *" $step "*) continue ;; esac
    id=$(step_id "$step")
    assert_eq "$name: $step still open" "open" "$(bead_get "$(bead_json "$id")" "b.get('status', '')")"
  done
}

# ── Anti-vacuity ──────────────────────────────────────────────────
# mutant <label> <python-edit> — pour a copy of first-task.json edited by
# <python-edit> (over the parsed formula `d`) and assert checks 1 and 2 fail.
mutant() {
  local label="$1" edit="$2"
  local file="$TMP_DIR/first-task.$label.json"
  python3 -c "
import json, sys
d = json.load(open(sys.argv[1]))
steps = {s['id']: s for s in d['steps']}
$edit
json.dump(d, open(sys.argv[2], 'w'), indent=2)
" "$FORMULA_DIR/first-task.json" "$file"

  # The subshell keeps the mutant's expected failures out of the suite's FAIL.
  # shellcheck disable=SC2030
  if ( FAIL=0; pour_and_check first-task "$file" >/dev/null 2>&1; [ "$FAIL" -gt 0 ] ); then
    pass "mutant '$label' is caught by the pour checks"
  else
    fail "mutant '$label' is caught by the pour checks" "a first-task.json with $label passed checks 1 and 2"
  fi
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " onboarding-formulas E2E tests"
  blue "═══════════════════════════════════════════════════"

  local missing=() bin
  for bin in "$GB_BIN" "$KD_BIN" coop claudeless python3 git; do
    have "$bin" || missing+=("$bin")
  done
  if [ "${#missing[@]}" -gt 0 ]; then
    skip_module "missing required binaries: ${missing[*]}"
  fi

  setup_portforward
  setup_token

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  dim "  Project label: project:$PROJECT"
  setup_agent_dir
  echo

  local name
  for name in register-project add-local-agent first-task enable-slack; do
    check_formula "$name"
    echo
  done

  blue "Anti-vacuity: mutants of first-task.json must fail the pour checks"
  mutant "create-bead removed" "
d['steps'] = [s for s in d['steps'] if s['id'] != 'create-bead']
steps['assign']['needs'] = ['pick-task']"
  mutant "pick-task gate dropped" "
del steps['pick-task']['gate']
steps['pick-task']['type'] = 'task'"
  echo

  # ── Summary ─────────────────────────────────────────────────────
  blue "═══════════════════════════════════════════════════"
  local total=$((PASS + FAIL))
  if [ "$FAIL" -eq 0 ]; then
    green " ✓ All $total assertions passed"
  else
    red " ✗ $FAIL/$total assertions failed"
    echo
    local err
    for err in "${ERRORS[@]}"; do
      red "   - $err"
    done
  fi
  blue "═══════════════════════════════════════════════════"

  [ "$FAIL" -eq 0 ]
}

main "$@"
