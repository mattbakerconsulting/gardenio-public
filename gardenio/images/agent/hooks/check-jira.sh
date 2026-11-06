#!/bin/bash
# check-jira.sh — Query daemon for pending JIRA tasks and enqueue system-reminders.
#
# Called by UserPromptSubmit hook. Formats pending JIRA-sourced tasks as a
# <system-reminder> block and appends to the inject queue (JSONL) for
# drain-queue.sh to pick up on the next PostToolUse hook.
#
# Env: BEADS_AGENT_NAME — agent identity (e.g. "myproject/builder")
#
# Always exits 0 so hook failures don't block Claude.

set -euo pipefail

QUEUE_FILE="/tmp/inject-queue.jsonl"
AGENT_ID="${BEADS_AGENT_NAME:-}"

if [ -z "${AGENT_ID}" ]; then
    exit 0
fi

# Ensure gb can resolve the agent identity.
export KD_ACTOR="${KD_ACTOR:-${AGENT_ID}}"

# Query daemon for open task beads with source:jira label.
tasks_json=$(gb ready --type task --json 2>/dev/null) || exit 0

# Check if result is an empty array or empty string.
if [ -z "${tasks_json}" ] || [ "${tasks_json}" = "[]" ] || [ "${tasks_json}" = "null" ]; then
    exit 0
fi

# Filter for source:jira beads and format.
bullets=$(echo "${tasks_json}" | jq -r '
    [.[] | select(.labels // [] | any(. == "source:jira"))] |
    if length == 0 then empty else
    .[] |
    . as $bead |
    ($bead.fields.jira_key // "unknown") as $key |
    ($bead.fields.jira_type // "task") as $type |
    "- \($bead.id) [\($key)] (\($type)): \($bead.title)"
    end
' 2>/dev/null) || exit 0

if [ -z "${bullets}" ]; then
    exit 0
fi

count=$(echo "${bullets}" | wc -l | tr -d ' ')

# Build the system-reminder content.
reminder="<system-reminder>
You have ${count} pending JIRA task(s) available.

${bullets}

Run 'kd show <id>' to view task details. Use 'gb ready' to claim a task.
</system-reminder>"

# Build JSONL entry and append to queue.
entry=$(jq -n --arg type "jira" --arg content "${reminder}" '{type: $type, content: $content}') || exit 0
echo "${entry}" >> "${QUEUE_FILE}"

exit 0
