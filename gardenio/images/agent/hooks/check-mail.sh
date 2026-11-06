#!/bin/bash
# check-mail.sh — Query daemon for unread mail and enqueue system-reminders.
#
# Called by SessionStart and UserPromptSubmit hooks. Formats unread messages
# as a <system-reminder> block and appends to the inject queue (JSONL) for
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

# Query daemon for open mail assigned to this agent.
mail_json=$(kd list --type mail --status open --assignee "${AGENT_ID}" --json 2>/dev/null) || exit 0

# Check if result is an empty array or empty string.
if [ -z "${mail_json}" ] || [ "${mail_json}" = "[]" ] || [ "${mail_json}" = "null" ]; then
    exit 0
fi

# Count messages.
count=$(echo "${mail_json}" | jq 'length' 2>/dev/null) || exit 0
if [ "${count}" -eq 0 ]; then
    exit 0
fi

# Format each message as a bullet line.
bullets=$(echo "${mail_json}" | jq -r '
    .[] |
    . as $bead |
    ($bead.labels // [] | map(select(startswith("from:"))) | first // "from:unknown") as $from_label |
    ($from_label | ltrimstr("from:")) as $sender |
    "- \($bead.id) from \($sender): \($bead.title)"
' 2>/dev/null) || exit 0

if [ -z "${bullets}" ]; then
    exit 0
fi

# Build the system-reminder content.
reminder="<system-reminder>
You have ${count} unread message(s) in your inbox.

${bullets}

Run 'kd show <id>' to read a message. Run 'kd close <id>' to mark as read.
</system-reminder>"

# Build JSONL entry and append to queue.
entry=$(jq -n --arg type "mail" --arg content "${reminder}" '{type: $type, content: $content}') || exit 0
echo "${entry}" >> "${QUEUE_FILE}"

exit 0
