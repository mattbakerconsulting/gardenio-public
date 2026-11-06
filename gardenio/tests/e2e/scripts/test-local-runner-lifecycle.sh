#!/usr/bin/env bash
# test-local-runner-lifecycle.sh — local runner register, join, claim, squawk;
# disconnect pauses, reconnect resumes and drains mail (local-agents spec §11
# b and e; plan gd-fwFefkDP4H Task 18, gd-jW7KJ7ZdNy).
#
# (b) An admin-minted invite is redeemed with `gb runner register` into a
#     private GARDENIO_HOME, `gb runner join` starts a claudeless agent
#     (claudeless/local-runner-mail.toml) on that runner, a task placed on the
#     runner shows in `gb ready` there and nowhere else, and the agent claims
#     it and squawks when mailed.
# (e) The whole join process group is SIGSTOPped, as a closed lid stops it.
#     With no heartbeat for 90 s the runner counts as away
#     (statusreporter.runnerAwayAfter) and the controller's stale reaper,
#     which runs every 5 min, sets the agent paused. Mail sent meanwhile stays
#     undelivered. SIGCONT brings the heartbeat back, the next reaper pass sets
#     the agent working, and the subscriber drains the mail. Worst case each
#     way is 90 s + 5 min, so LR_AWAY_TIMEOUT defaults to 480 s.
#
# Every negative assertion is paired with the same query's positive control.
#
# Needs: kd, gb, jq, coop, claudeless; a live namespace with the controller
# and an admin token (type=runner_invite is admin-gated; without one the
# script skips). Not in the HERMETIC_ONLY subset.
#
# Usage:
#   NAMESPACE=gardenio-e2e ./test-local-runner-lifecycle.sh
#   BEADS_HTTP_URL=http://localhost:19099 ./test-local-runner-lifecycle.sh
#
# Env: E2E_PROJECT (gardenio), E2E_RUNNER_OWNER (BEADS_OPERATED_BY),
#      LR_AWAY_TIMEOUT (480), LOG_ARCHIVE_DIR.

set -euo pipefail

# shellcheck source=SCRIPTDIR/lib-local-runner.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib-local-runner.sh"

LR_AWAY_TIMEOUT="${LR_AWAY_TIMEOUT:-480}"

# ready_has <gardenio-home> <id> — does `gb ready`, run as an agent with that
# runner registration (or none), list the bead?
ready_has() {
  BOAT_ROLE=local GARDENIO_HOME="$1" "$GB_BIN" ready --json --project "$E2E_PROJECT" 2>/dev/null \
    | jq -e --arg id "$2" 'any(.[]; .id == $id)' >/dev/null 2>&1
}

main() {
  lr_init "Local runner lifecycle: register, join, claim, squawk; away and resume (spec §11 b, e)" coop claudeless
  trap lr_cleanup EXIT

  # ── (b) register ────────────────────────────────────────────────
  blue "(b) register with an invite"
  local rc=0
  lr_register_runner || rc=$?
  if [ "$rc" -eq 1 ]; then
    skip_all "could not create a type=runner_invite bead (admin-gated); run with an admin token"
  elif [ "$rc" -ne 0 ]; then
    lr_summary
  fi

  local mode_tok mode_json
  mode_tok="$(file_mode "$RUNNER_HOME/runner.token")"
  mode_json="$(file_mode "$RUNNER_HOME/runner.json")"
  if [ "$mode_tok" = 600 ] && [ "$mode_json" = 600 ]; then
    pass "runner.token and runner.json are mode 0600"
  else
    fail "runner registration files are mode 0600" "runner.token=$mode_tok runner.json=$mode_json"
  fi
  # Positive control: the token file does hold a runner bearer.
  if grep -q '^gr_' "$RUNNER_HOME/runner.token" 2>/dev/null; then
    pass "runner.token holds a gr_ runner bearer"
  else
    fail "runner.token holds a gr_ runner bearer"
  fi
  if grep -q 'gr_' "$RUNNER_HOME/runner.json" 2>/dev/null; then
    fail "runner.json carries no token" "a gr_ string is in runner.json"
  else
    pass "runner.json carries no token"
  fi

  if bead_json "$RUNNER_ID" | jq -e --arg o "$RUNNER_OWNER" --arg p "$E2E_PROJECT" \
      '.type == "runner" and .fields.owner == $o and (.fields.projects | if type == "string" then fromjson else . end | index($p)) != null' >/dev/null 2>&1; then
    pass "runner bead is owned by $RUNNER_OWNER and allowlists $E2E_PROJECT"
  else
    fail "runner bead owner and projects" "$(bead_json "$RUNNER_ID" | jq -c '{type, fields}' 2>/dev/null)"
  fi

  # The invite is single-use: a second redeem of the same code is refused.
  local replay_home replay_out
  replay_home="$(mktemp -d -t gardenio-home-XXXXXX)"
  if replay_out="$(GARDENIO_HOME="$replay_home" "$GB_BIN" runner register \
      --invite "$INVITE_CODE" --daemon "$BEADS_HTTP_URL" 2>&1)"; then
    fail "a redeemed invite cannot register a second runner" "$replay_out"
    CREATED_BEADS+=("$(jq -r '.runner_id // empty' "$replay_home/runner.json" 2>/dev/null || true)")
  else
    pass "a redeemed invite cannot register a second runner"
  fi
  rm -rf "$replay_home"

  # ── (b) placement ───────────────────────────────────────────────
  blue "(b) placement: gb ready on and off the runner"
  local placed unplaced no_home
  placed="$(kd_new "e2e local task $TEST_TAG" --type=task --short-title "e2e local task" \
    -l "project:$E2E_PROJECT" -f placement=local -f "runner_id=$RUNNER_ID")"
  unplaced="$(kd_new "e2e cluster task $TEST_TAG" --type=task --short-title "e2e cluster task" \
    -l "project:$E2E_PROJECT")"
  if [ -z "$placed" ] || [ -z "$unplaced" ]; then
    fail "seed the placed and unplaced tasks" "placed=$placed unplaced=$unplaced"
    lr_summary
  fi
  no_home="$(mktemp -d -t gardenio-home-XXXXXX)"
  if ready_has "$RUNNER_HOME" "$placed"; then
    pass "gb ready on the runner lists the task placed on it"
  else
    fail "gb ready on the runner lists the task placed on it"
  fi
  if ready_has "$RUNNER_HOME" "$unplaced"; then
    fail "gb ready on the runner hides an unplaced task" "$unplaced listed"
  else
    pass "gb ready on the runner hides an unplaced task"
  fi
  if ready_has "$no_home" "$unplaced"; then
    pass "gb ready off the runner lists the unplaced task"
  else
    fail "gb ready off the runner lists the unplaced task"
  fi
  if ready_has "$no_home" "$placed"; then
    fail "gb ready off the runner hides the placed task" "$placed listed"
  else
    pass "gb ready off the runner hides the placed task"
  fi
  rm -rf "$no_home"

  # ── (b) join ────────────────────────────────────────────────────
  blue "(b) join"
  if ! lr_join "e2e-lr-$TEST_TAG" "$SCENARIO_DIR/local-runner-mail.toml"; then
    lr_summary
  fi
  if bead_json "$AGENT_BEAD" | jq -e --arg r "$RUNNER_ID" --arg o "$RUNNER_OWNER" \
      '.fields.execution_target == "laptop" and .fields.runtime_id == $r and .fields.role == "local" and .fields.operated_by == $o' >/dev/null 2>&1; then
    pass "agent bead is a laptop agent on this runner, role local, operated by the owner"
  else
    fail "agent bead fields" "$(bead_json "$AGENT_BEAD" | jq -c '.fields' 2>/dev/null)"
  fi
  if wait_for_field "$RUNNER_ID" state online 60; then
    pass "join heartbeats the runner online"
  else
    fail "join heartbeats the runner online" "state=$(bead_field "$RUNNER_ID" state)"
  fi

  # ── (b) claim and squawk ────────────────────────────────────────
  blue "(b) claim and squawk"
  printf '%s\n' "$placed" >"$RUNNER_HOME/e2e-claim-task"
  local claim_mail
  claim_mail="$(mail_send "$AGENT_NAME" "E2E-CLAIM $TEST_TAG" "claim $placed and squawk" --interrupt)"
  if [ -z "$claim_mail" ]; then
    fail "send the claim mail"
  elif wait_for_status "$claim_mail" closed 120; then
    pass "interrupt mail reached the session and was read"
  else
    fail "interrupt mail reached the session and was read" "$(bead_json "$claim_mail" | jq -c '{status, fields}' 2>/dev/null)"
    dump_log "join-$AGENT_NAME" "$JOIN_LOG"
  fi
  if wait_for_jq "$placed" ".assignee == \"$AGENT_NAME\" and .status == \"in_progress\"" 60; then
    pass "the local agent claimed the task gb ready showed it"
  else
    fail "the local agent claimed the task gb ready showed it" "$(bead_json "$placed" | jq -c '{status, assignee}' 2>/dev/null)"
  fi
  if "$KD_BIN" list -t message -l squawk -l "from:$AGENT_NAME" --all-types --all-projects \
      --created-after 30m --limit 50 --json 2>/dev/null \
      | jq -e --arg m "E2E-SQUAWK $placed" 'any(.[]; .title | contains($m))' >/dev/null 2>&1; then
    pass "the agent's squawk was posted"
  else
    fail "the agent's squawk was posted" "no squawk message bead from $AGENT_NAME"
  fi
  skip "squawk rendered in the Slack thread" "needs a Slack workspace; /slack-ux-acceptance covers rendering"

  # ── (e) disconnect ──────────────────────────────────────────────
  blue "(e) disconnect: heartbeats stop (up to ${LR_AWAY_TIMEOUT}s for the reaper)"
  lr_pause
  if wait_for_field "$AGENT_BEAD" agent_state paused "$LR_AWAY_TIMEOUT"; then
    pass "stale reaper paused the agent while its runner was away"
  else
    fail "stale reaper paused the agent while its runner was away" "agent_state=$(bead_field "$AGENT_BEAD" agent_state)"
  fi
  if [ "$(bead_attr "$AGENT_BEAD" .status)" != closed ]; then
    pass "the paused agent is held, not reaped"
  else
    fail "the paused agent is held, not reaped" "agent bead closed while away"
  fi
  if [ "$(bead_attr "$placed" .assignee)" = "$AGENT_NAME" ]; then
    pass "the claimed task stays assigned while the agent is paused"
  else
    fail "the claimed task stays assigned while the agent is paused" "assignee=$(bead_attr "$placed" .assignee)"
  fi

  local away_mail
  away_mail="$(mail_send "$AGENT_NAME" "E2E-READ while away $TEST_TAG" "catch up when you are back" --interrupt)"
  sleep 15
  # The claim mail above is the positive control: same path, delivered.
  if [ -n "$away_mail" ] && bead_json "$away_mail" | jq -e '.status == "open" and ((.fields.delivered_at // "") == "")' >/dev/null 2>&1; then
    pass "mail sent while away stays open and undelivered"
  else
    fail "mail sent while away stays open and undelivered" "$(bead_json "$away_mail" | jq -c '{status, fields}' 2>/dev/null)"
  fi

  blue "(e) reconnect"
  lr_resume
  if wait_for_field "$AGENT_BEAD" agent_state working "$LR_AWAY_TIMEOUT"; then
    pass "the agent resumed when the runner's heartbeat returned"
  else
    fail "the agent resumed when the runner's heartbeat returned" "agent_state=$(bead_field "$AGENT_BEAD" agent_state)"
  fi
  if [ -n "$away_mail" ] && wait_for_status "$away_mail" closed 180; then
    pass "mail sent while away was drained and read on resume"
  else
    fail "mail sent while away was drained and read on resume" "$(bead_json "$away_mail" | jq -c '{status, fields}' 2>/dev/null)"
    dump_log "join-$AGENT_NAME" "$JOIN_LOG"
  fi

  # ── leave ───────────────────────────────────────────────────────
  blue "leave"
  lr_leave
  if kill -0 "$JOIN_PID" 2>/dev/null; then
    fail "gb runner leave stops the joined agent" "join pid $JOIN_PID still running"
  else
    pass "gb runner leave stops the joined agent"
  fi
  JOIN_PID=""
  if wait_for_field "$RUNNER_ID" state away 60; then
    pass "gb runner leave marks the runner away"
  else
    fail "gb runner leave marks the runner away" "state=$(bead_field "$RUNNER_ID" state)"
  fi
  if wait_for_status "$AGENT_BEAD" closed 60; then
    pass "the agent bead closes on leave"
  else
    fail "the agent bead closes on leave" "status=$(bead_attr "$AGENT_BEAD" .status)"
  fi

  lr_summary
}

main "$@"
