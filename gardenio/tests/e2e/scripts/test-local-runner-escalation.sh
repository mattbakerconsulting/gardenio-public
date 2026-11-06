#!/usr/bin/env bash
# test-local-runner-escalation.sh — cloud-to-local escalation end to end: an
# escalation filed for a named runner is refused to a non-owner approver, the
# runner's owner approves it, the agent joined on the runner executes it and
# reports (local-agents spec §8.2, §11 c; plan gd-fwFefkDP4H Task 18,
# gd-jW7KJ7ZdNy). (d), local-to-cloud, is milestone M2 (spec §12) and is
# recorded as a skip, never left out.
#
# Flow: register a runner from an invite and `gb runner join` a claudeless
# agent (claudeless/local-runner-escalation.toml); wait for gb's escalation
# executor; file `gb escalation create --execution-target local
# --target-runner <runner>`; try the approve as a non-owner (403, "only its
# owner may approve it"); approve as the owner. The daemon stamps
# executor=<the joined agent> and mails it control:escalation-approved; gb's
# executor nudges coop with "ESCALATION APPROVED: ..."; the scenario runs
# `gb escalation close --status success`.
#
# Approval must come from an approval-capable identity (an OIDC token from an
# allowlisted browser/Slack client, or a verified v2 edge envelope); a kd or
# shared token is refused for that reason alone (escalation_approval_authz.go).
# Pass the tokens as files so they never reach argv or this script's env:
#   E2E_OWNER_APPROVER_TOKEN_FILE     the runner owner's (E2E_RUNNER_OWNER)
#   E2E_STRANGER_APPROVER_TOKEN_FILE  any other operator's
# Without the owner's, the ambient token is tried and a refusal skips the rest.
# Without the stranger's, the non-owner refusal is a recorded skip.
#
# Needs: kd, gb, jq, coop, claudeless; a live namespace; an admin token for the
# invite (else skip). Not in the HERMETIC_ONLY subset.
#
# Usage:
#   NAMESPACE=gardenio-e2e E2E_RUNNER_OWNER=oidc:me@example.com \
#     E2E_OWNER_APPROVER_TOKEN_FILE=~/.e2e/owner.tok ./test-local-runner-escalation.sh

set -euo pipefail

# shellcheck source=SCRIPTDIR/lib-local-runner.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib-local-runner.sh"

# approve_as <token-file or ""> <escalation-id> — run the approve, print its
# output, return its exit code. An empty file means the ambient token.
approve_as() {
  local tokfile="$1" id="$2"
  if [ -n "$tokfile" ]; then
    GARDENIO_TOKEN_FILE="$tokfile" "$GB_BIN" escalation approve "$id" --execution-target local 2>&1
  else
    "$GB_BIN" escalation approve "$id" --execution-target local 2>&1
  fi
}

main() {
  lr_init "Local runner escalation: cloud to local, owner-only approval (spec §11 c; d is M2)" coop claudeless
  trap lr_cleanup EXIT

  local rc=0
  lr_register_runner || rc=$?
  if [ "$rc" -eq 1 ]; then
    skip "(d) local-to-cloud escalation" "milestone M2 (spec §12)"
    skip_all "could not create a type=runner_invite bead (admin-gated); run with an admin token"
  elif [ "$rc" -ne 0 ]; then
    lr_summary
  fi
  if ! lr_join "e2e-lr-esc-$TEST_TAG" "$SCENARIO_DIR/local-runner-escalation.toml"; then
    lr_summary
  fi
  if wait_for_log "$JOIN_LOG" "Escalation executor started" 30 "$JOIN_PID"; then
    pass "gb started the escalation executor for runner $RUNNER_ID"
  else
    fail "gb started the escalation executor" "no 'Escalation executor started' banner"
    dump_log "join-$AGENT_NAME" "$JOIN_LOG"
    lr_summary
  fi

  # ── file ────────────────────────────────────────────────────────
  blue "(c) file a cloud-to-local escalation on the runner"
  local out esc
  out="$("$GB_BIN" escalation create \
    --title "e2e local escalation $TEST_TAG" \
    --operation-type manual_test \
    --operation-spec '{"cmd":"echo e2e"}' \
    --context "Local runner cloud-to-local escalation e2e ($TEST_TAG)" \
    --execution-target local \
    --target-runner "$RUNNER_ID" \
    --label "project:$E2E_PROJECT" 2>&1 || true)"
  esc="$(printf '%s' "$out" | grep -oE 'escalation_id=(gd|ka)-[A-Za-z0-9]+' | head -n1 | cut -d= -f2 || true)"
  if [ -z "$esc" ]; then
    esc="$(printf '%s' "$out" | grep -oE '(gd|ka)-[A-Za-z0-9]+' | grep -vx "$RUNNER_ID" | head -n1 || true)"
  fi
  if [ -z "$esc" ]; then
    fail "gb escalation create files the escalation" "$out"
    lr_summary
  fi
  CREATED_BEADS+=("$esc")
  printf '%s\n' "$esc" >"$RUNNER_HOME/e2e-escalation-id"
  if bead_json "$esc" | jq -e --arg r "$RUNNER_ID" \
      '.fields.execution_target == "local" and .fields.target_runner == $r and .status == "open"' >/dev/null 2>&1; then
    pass "escalation $esc names runner $RUNNER_ID and waits for approval"
  else
    fail "escalation fields" "$(bead_json "$esc" | jq -c '{status, fields}' 2>/dev/null)"
  fi

  # ── non-owner (negative, paired with the owner below) ───────────
  blue "(c) approval: a non-owner is refused"
  local stranger="${E2E_STRANGER_APPROVER_TOKEN_FILE:-}"
  if [ -z "$stranger" ] || [ ! -r "$stranger" ]; then
    skip "a non-owner's approve is refused" "set E2E_STRANGER_APPROVER_TOKEN_FILE to an approval-capable token of another operator"
  else
    local srefuse src=0
    srefuse="$(approve_as "$stranger" "$esc")" || src=$?
    if [ "$src" -ne 0 ] && grep -q "only its owner may approve" <<<"$srefuse"; then
      pass "a non-owner's approve is refused (only the runner's owner may approve)"
    elif [ "$src" -ne 0 ]; then
      fail "a non-owner's approve is refused for not owning the runner" "refused for another reason: $srefuse"
    else
      fail "a non-owner's approve is refused" "the approve succeeded: $srefuse"
    fi
    if bead_json "$esc" | jq -e '.status == "open" and ((.fields.executor_mail_id // "") == "")' >/dev/null 2>&1; then
      pass "the refused approve dispatched nothing"
    else
      fail "the refused approve dispatched nothing" "$(bead_json "$esc" | jq -c '{status, fields}' 2>/dev/null)"
    fi
  fi

  # ── owner (positive control) ────────────────────────────────────
  blue "(c) approval: the owner approves"
  local owner="${E2E_OWNER_APPROVER_TOKEN_FILE:-}" aout arc=0
  if [ -n "$owner" ] && [ ! -r "$owner" ]; then
    fail "E2E_OWNER_APPROVER_TOKEN_FILE is readable" "$owner"
    lr_summary
  fi
  aout="$(approve_as "$owner" "$esc")" || arc=$?
  if [ "$arc" -ne 0 ]; then
    if [ -z "$owner" ]; then
      skip "the owner approves and the local agent executes" \
        "ambient token cannot approve (rc=$arc): $(printf '%s' "$aout" | head -2 | tr '\n' ' '); set E2E_OWNER_APPROVER_TOKEN_FILE"
    else
      fail "the runner's owner approves" "$aout"
    fi
    skip "(d) local-to-cloud escalation" "milestone M2 (spec §12)"
    lr_summary
  fi
  pass "the runner's owner approved $esc"

  # ── execution ───────────────────────────────────────────────────
  if wait_for_field "$esc" executor "$AGENT_NAME" 30; then
    pass "the daemon stamped the agent joined on the runner as executor"
  else
    fail "executor is the agent joined on the runner" "executor=$(bead_field "$esc" executor), want $AGENT_NAME"
  fi
  if wait_for_field "$esc" prompt_delivered_to "$AGENT_NAME" 150; then
    pass "gb's executor nudged the local session with the escalation"
  else
    fail "gb's executor nudged the local session" "$(bead_json "$esc" | jq -c '.fields' 2>/dev/null)"
    dump_log "join-$AGENT_NAME" "$JOIN_LOG"
  fi
  if wait_for_jq "$esc" '.status == "closed" and .fields.result_status == "success"' 120; then
    pass "the local agent executed the escalation and reported success"
  else
    fail "the local agent executed the escalation and reported success" "$(bead_json "$esc" | jq -c '{status, fields}' 2>/dev/null)"
    dump_log "join-$AGENT_NAME" "$JOIN_LOG"
  fi

  local mail_id mails
  mail_id="$(bead_field "$esc" executor_mail_id)"
  if [ -n "$mail_id" ] && bead_json "$mail_id" | jq -e --arg a "$AGENT_NAME" --arg e "$esc" \
      '.assignee == $a and (.labels | index("control:escalation-approved")) != null and .fields.escalation_id == $e' >/dev/null 2>&1; then
    pass "the executor mail is control:escalation-approved, addressed to the agent"
  else
    fail "the executor mail" "executor_mail_id=$mail_id: $(bead_json "$mail_id" | jq -c '{assignee, labels, fields}' 2>/dev/null)"
  fi
  mails="$("$KD_BIN" list -t mail -l control:escalation-approved -s open -s closed --all-types --all-projects \
    --created-after 30m --limit 200 --json 2>/dev/null \
    | jq --arg e "$esc" '[.[] | select(.fields.escalation_id == $e)] | length' 2>/dev/null || echo "?")"
  if [ "$mails" = 1 ]; then
    pass "exactly one executor mail for the escalation"
  else
    fail "exactly one executor mail for the escalation" "got $mails"
  fi

  skip "(d) local-to-cloud escalation" "milestone M2 (spec §12)"
  lr_summary
}

main "$@"
