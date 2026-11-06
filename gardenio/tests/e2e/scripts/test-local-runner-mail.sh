#!/usr/bin/env bash
# test-local-runner-mail.sh — a pod agent receives nudge, decision answer and
# kill as mail, with the inbound dial deleted (local-agents spec §3, §11 a;
# plan gd-fwFefkDP4H Task 18, gd-jW7KJ7ZdNy).
#
# Seeds a crew agent bead with mock_scenario=mail-reader, so the controller
# boots a claudeless pod (gardenio/images/agent/scenarios/mail-reader.toml,
# baked into the agent image: an image older than this script's commit has no
# such scenario and the pod never comes up). Then:
#   1. no dial: the agent bead's notes carry pod_name (the statusreporter is
#      writing them) and no coop_url (it no longer writes one, gd-c8yB95OCd3)
#   2. nudge: `gb mail send --interrupt`; the pod's own mail subscriber stamps
#      delivered_at and the session reads and closes the mail
#   3. decision: a decision the agent filed is answered with
#      `gb decision respond`; the bridge mails the answer, which the pod
#      receives the same way
#   4. kill: control:kill mail stops the pod agent
#
# Needs: kd, gb, jq; a live namespace with the controller and slack-bridge
# (the bridge mails decision answers). Not in the HERMETIC_ONLY subset.
# Without a bridge Service in the namespace (the lane sets slackBridge.enabled
# false until slackmock runs there) the decision-answer mail asserts skip.
#
# The kill mail needs a credential the daemon lets author control: mail
# (kbeads mailControlDenial): an operator (oidc or edge) or a fleet listed in
# KBEADS_CONTROL_MAIL_FLEETS. A pod agent has no runner, so a runner token
# cannot send it. Put one in E2E_CONTROL_MAIL_TOKEN_FILE; without it the
# script tries the ambient token and skips the kill asserts if it is refused.
#
# Usage:
#   NAMESPACE=gardenio-e2e ./test-local-runner-mail.sh
#
# Env: E2E_PROJECT (gardenio), MAIL_POD_SCENARIO (mail-reader),
#      MAIL_POD_BOOT_TIMEOUT (240), E2E_CONTROL_MAIL_TOKEN_FILE,
#      BRIDGE_SVC (<namespace>-slack-bridge).

set -euo pipefail

# shellcheck source=SCRIPTDIR/lib-local-runner.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib-local-runner.sh"

MAIL_POD_SCENARIO="${MAIL_POD_SCENARIO:-mail-reader}"
MAIL_POD_BOOT_TIMEOUT="${MAIL_POD_BOOT_TIMEOUT:-240}"
BRIDGE_SVC="${BRIDGE_SVC:-${NAMESPACE}-slack-bridge}"

# bridge_absent — 0 only when kubectl can see the namespace and it has no
# slack-bridge Service. Unknown (no kubectl, no read access) is not absent, so
# the decision-answer wait still runs and fails loudly.
bridge_absent() {
  local out
  have kubectl || return 1
  out="$(kubectl -n "$NAMESPACE" get svc "$BRIDGE_SVC" --ignore-not-found -o name 2>/dev/null)" || return 1
  [ -z "$out" ]
}

# mail_for <agent> <title-prefix> — id of the newest mail to agent whose title
# starts with prefix, or "".
mail_for() {
  "$KD_BIN" list -t mail --assignee "$1" -s open -s closed --all-types --all-projects \
    --created-after 30m --sort=-created_at --limit 50 --json 2>/dev/null \
    | jq -r --arg p "$2" '[.[] | select(.title | startswith($p))][0].id // empty' 2>/dev/null || true
}

# assert_mail_read <name> <mail-id> — delivered by the pod's subscriber, then
# read and closed by the session.
assert_mail_read() {
  local name="$1" id="$2"
  if wait_for_jq "$id" '(.fields.delivered_at // "") != ""' 90; then
    pass "$name: the pod's mail subscriber delivered it"
  else
    fail "$name: the pod's mail subscriber delivered it" "$(bead_json "$id" | jq -c '{status, fields}' 2>/dev/null)"
  fi
  if wait_for_status "$id" closed 90; then
    pass "$name: the session read and closed it"
  else
    fail "$name: the session read and closed it" "$(bead_json "$id" | jq -c '{status, fields}' 2>/dev/null)"
  fi
}

main() {
  lr_init "Pod agent mail: nudge, decision, kill with no dial (spec §11 a)"
  trap lr_cleanup EXIT

  local agent="e2e-mail-$TEST_TAG" bead
  bead="$(kd_new "$agent" --type=agent -l "project:$E2E_PROJECT" -l e2e \
    -f "agent=$agent" -f agent_state=spawning -f mode=crew -f role=crew \
    -f "project=$E2E_PROJECT" -f "mock_scenario=$MAIL_POD_SCENARIO")"
  if [ -z "$bead" ]; then
    fail "seed the mock pod agent bead"
    lr_summary
  fi
  dim "  agent $agent ($bead), mock_scenario=$MAIL_POD_SCENARIO"
  if wait_for_jq "$bead" '(.notes // "" | test("pod_name:")) and (.fields.agent_state == "working" or .fields.agent_state == "idle")' "$MAIL_POD_BOOT_TIMEOUT"; then
    pass "the controller booted the claudeless pod agent"
  else
    fail "the controller booted the claudeless pod agent" \
      "$(bead_json "$bead" | jq -c '{status, state: .fields.agent_state, notes}' 2>/dev/null); an agent image without /scenarios/$MAIL_POD_SCENARIO.toml never boots"
    lr_summary
  fi

  # ── 1. no dial ──────────────────────────────────────────────────
  blue "(a) no dial"
  # pod_name above is the positive control: the reporter writes these notes.
  if bead_json "$bead" | jq -e '(.notes // "" | test("coop_url")) or (.fields.coop_url // "" | . != "")' >/dev/null 2>&1; then
    fail "the agent bead carries no coop_url to dial" "$(bead_attr "$bead" .notes)"
  else
    pass "the agent bead carries no coop_url to dial"
  fi

  # ── 2. nudge ────────────────────────────────────────────────────
  blue "(a) nudge as mail"
  local nudge
  nudge="$(mail_send "$agent" "E2E-READ nudge $TEST_TAG" "hello from e2e" --interrupt)"
  if [ -n "$nudge" ]; then
    assert_mail_read "nudge" "$nudge"
  else
    fail "send the nudge mail"
  fi

  # ── 3. decision ─────────────────────────────────────────────────
  blue "(a) decision answer as mail"
  local dout derr dec dmail="" start
  # stdout only: gb warns on stderr that the decision has no requesting task,
  # which would break the --json parse.
  derr="$(mktemp)"
  dout="$(KD_AGENT_ID="$bead" "$GB_BIN" --actor "$agent" decision create \
    --prompt "E2E-READ decision $TEST_TAG: pick one" \
    --options '[{"id":"a","label":"Alpha","artifact_type":"report"},{"id":"b","label":"Beta","artifact_type":"report"}]' \
    --no-wait --json 2>"$derr" || true)"
  dec="$(printf '%s' "$dout" | jq -r '.id // empty' 2>/dev/null || true)"
  if [ -z "$dec" ]; then
    fail "file a decision as the pod agent" "$dout $(head -c 300 "$derr")"
    rm -f "$derr"
  else
    rm -f "$derr"
    CREATED_BEADS+=("$dec")
    if bead_json "$dec" | jq -e --arg a "$agent" '.assignee == $a or .created_by == $a' >/dev/null 2>&1; then
      pass "decision $dec is the pod agent's"
    else
      fail "decision $dec is the pod agent's" "$(bead_json "$dec" | jq -c '{assignee, created_by}' 2>/dev/null)"
    fi
    if "$GB_BIN" decision respond "$dec" --select a >/dev/null 2>&1; then
      pass "decision answered"
    else
      fail "decision answered" "gb decision respond --select a failed"
    fi
    if bridge_absent; then
      skip "the bridge mailed the answer to the pod agent" \
        "no $BRIDGE_SVC Service in $NAMESPACE: the bridge writes the 'Decision <id> resolved' mail, and the lane runs without it until slackmock does"
    else
      start=$SECONDS
      while [ $((SECONDS - start)) -lt 90 ] && [ -z "$dmail" ]; do
        dmail="$(mail_for "$agent" "Decision $dec resolved")"
        [ -n "$dmail" ] || sleep 3
      done
      if [ -n "$dmail" ]; then
        CREATED_BEADS+=("$dmail")
        pass "the bridge mailed the answer to the pod agent ($dmail)"
        assert_mail_read "decision answer" "$dmail"
      else
        fail "the bridge mailed the answer to the pod agent" "no 'Decision $dec resolved' mail to $agent within 90s"
      fi
    fi
  fi

  # ── 4. kill ─────────────────────────────────────────────────────
  blue "(a) kill as mail"
  local kill_mail kerr tokfile="${E2E_CONTROL_MAIL_TOKEN_FILE:-}"
  kerr="$(mktemp)"
  if [ -n "$tokfile" ] && [ ! -r "$tokfile" ]; then
    fail "E2E_CONTROL_MAIL_TOKEN_FILE is readable" "$tokfile"
    rm -f "$kerr"
    lr_summary
  fi
  if [ -n "$tokfile" ]; then
    kill_mail="$(GARDENIO_TOKEN_FILE="$tokfile" mail_send "$agent" Stop "e2e kill $TEST_TAG" --label control:kill 2>"$kerr")"
  else
    kill_mail="$(mail_send "$agent" Stop "e2e kill $TEST_TAG" --label control:kill 2>"$kerr")"
  fi
  if [ -z "$kill_mail" ] && [ -z "$tokfile" ] && grep -qF 'control: labels are reserved' "$kerr"; then
    skip "control:kill stops the pod agent" \
      "the ambient token cannot author control: mail (HTTP 403, not an operator or control-mail fleet); set E2E_CONTROL_MAIL_TOKEN_FILE"
    skip "the pod acted on the kill mail" "no kill mail was sent"
    rm -f "$kerr"
    lr_summary
  fi
  if [ -z "$kill_mail" ]; then
    fail "send the control:kill mail" "$(head -c 400 "$kerr")"
    rm -f "$kerr"
    lr_summary
  fi
  rm -f "$kerr"
  CREATED_BEADS+=("$kill_mail")
  if wait_for_jq "$bead" '.status == "closed" or (.fields.agent_state | IN("done", "killed", "stopping", "stop_requested", "failed"))' 120; then
    pass "control:kill stops the pod agent"
  else
    fail "control:kill stops the pod agent" "$(bead_json "$bead" | jq -c '{status, state: .fields.agent_state}' 2>/dev/null)"
  fi
  if wait_for_jq "$kill_mail" '(.fields.control_acted // "") != "" or .status == "closed"' 60; then
    pass "the pod acted on the kill mail"
  else
    fail "the pod acted on the kill mail" "$(bead_json "$kill_mail" | jq -c '{status, fields}' 2>/dev/null)"
  fi

  lr_summary
}

main "$@"
