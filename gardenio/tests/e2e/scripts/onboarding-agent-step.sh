#!/usr/bin/env bash
# onboarding-agent-step.sh — the agent's hands in the onboarding-formula e2e
#
# Called from the Bash tool calls of the claudeless scenarios
# gardenio/tests/e2e/claudeless/onboarding-*.toml, which play a local agent
# working a poured onboarding molecule up to its first gate. The runner,
# test-onboarding-formulas.sh, exports:
#
#   ONBOARD_MOLECULE   id of the poured molecule the agent works
#   ONBOARD_STATE_DIR  scratch dir: claimed step ids and the agent log
#
# Subcommands (one per scenario phase):
#
#   ready                 show the molecule's ready steps (gb ready --plan)
#   claim <step>          claim the step whose source_step_id is <step>. It
#                         must be in the molecule's ready set and carry no
#                         gate: an agent never works a gate or a blocked step
#   run <step> <command>  run the command the step names; comment its exit
#                         code and output tail on the step
#   close <step>          close the claimed step
#   gate                  show what is left ready (the gate the agent stops at)
#   verdict               write pass/fail to GB_SCENARIO_RESULT_FILE
#
# Every phase appends "ok <phase>" or "FAIL <phase>: <reason>" to
# $ONBOARD_STATE_DIR/agent.log. A claudeless tool call's exit code never
# reaches the runner, so `verdict` turns that log into the scenario result
# that `gb agent start` reports (agent_start_scenario_result.go).

set -uo pipefail

MOL="${ONBOARD_MOLECULE:?ONBOARD_MOLECULE not set (run via test-onboarding-formulas.sh)}"
STATE="${ONBOARD_STATE_DIR:?ONBOARD_STATE_DIR not set (run via test-onboarding-formulas.sh)}"
LOG="$STATE/agent.log"

ok()   { echo "ok $1" >>"$LOG"; echo "ok $1"; }
bad()  { echo "FAIL $1: $2" >>"$LOG"; echo "FAIL $1: $2" >&2; }

# ready_step_id <step> — bead id of <step> if it is in the molecule's ready
# set and is not a gate. Prints "gate" for a ready gate step, "" otherwise.
ready_step_id() {
  gb ready --plan "$MOL" --json 2>/dev/null | python3 -c "
import sys, json
want = sys.argv[1]
for b in json.load(sys.stdin) or []:
    f = b.get('fields') or {}
    if f.get('source_step_id') == want:
        print('gate' if f.get('gate_type') else b['id'])
        break
" "$1" 2>/dev/null || true
}

step_id() { cat "$STATE/$1.id" 2>/dev/null || true; }

cmd="${1:-}"
shift || true

case "$cmd" in
  ready)
    if gb ready --plan "$MOL" 2>&1; then ok ready; else bad ready "gb ready --plan $MOL failed"; fi
    ;;

  claim)
    step="$1"
    id="$(ready_step_id "$step")"
    if [ "$id" = "gate" ]; then
      bad "claim $step" "step is a gate; an agent must stop there"
    elif [ -z "$id" ]; then
      bad "claim $step" "not in the ready set of $MOL (blocked, missing or closed)"
    elif kd claim "$id" 2>&1; then
      echo "$id" >"$STATE/$step.id"
      ok "claim $step"
    else
      bad "claim $step" "kd claim $id failed"
    fi
    ;;

  run)
    step="$1" command="$2"
    id="$(step_id "$step")"
    if [ -z "$id" ]; then
      bad "run $step" "step was never claimed"
      exit 0
    fi
    out="$(bash -c "$command" 2>&1)"
    rc=$?
    printf '%s\n' "$out"
    body="$(printf 'Ran `%s` (exit %s):\n\n```\n%s\n```\n' "$command" "$rc" "$(printf '%s\n' "$out" | tail -n 20)")"
    kd comment add "$id" --body "$body" >/dev/null 2>&1 || bad "run $step" "kd comment add $id failed"
    if [ "$rc" -eq 0 ]; then ok "run $step"; else bad "run $step" "command exited $rc"; fi
    ;;

  close)
    step="$1"
    id="$(step_id "$step")"
    if [ -z "$id" ]; then
      bad "close $step" "step was never claimed"
    elif kd close "$id" 2>&1; then
      ok "close $step"
    else
      bad "close $step" "kd close $id failed"
    fi
    ;;

  gate)
    # gb ready hides decision beads, so list the open unblocked steps of every
    # type as well: a decision gate only shows there.
    gb ready --plan "$MOL" 2>&1 || true
    kd list --parent "$MOL" --no-blockers --all-types -s open 2>&1 || true
    ok gate
    ;;

  verdict)
    verdict=pass
    if [ ! -s "$LOG" ] || grep -q '^FAIL ' "$LOG"; then
      verdict=fail
    fi
    cat "$LOG" 2>/dev/null || true
    echo "verdict: $verdict"
    if [ -n "${GB_SCENARIO_RESULT_FILE:-}" ]; then
      echo "$verdict" >"$GB_SCENARIO_RESULT_FILE"
    fi
    ;;

  *)
    echo "usage: $0 ready|claim <step>|run <step> <command>|close <step>|gate|verdict" >&2
    exit 2
    ;;
esac
