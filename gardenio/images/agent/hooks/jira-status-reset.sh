#!/usr/bin/env bash
# jira-status-reset.sh — PostToolUse hook (matcher: Bash)
#
# After glab mr create / git push commands that reference PE-XXXX tickets,
# spawns a background process that waits for the JIRA-GitLab automation
# to fire, then restores the ticket to its pre-command status.
#
# Works in tandem with jira-status-save.sh (PreToolUse) which saves the
# original status before the command runs.

set -euo pipefail

# Only run if JIRA credentials are available
if [ -z "${JIRA_BASE_URL:-}" ] || [ -z "${JIRA_EMAIL:-}" ] || [ -z "${JIRA_API_TOKEN:-}" ]; then
    exit 0
fi

SAVE_DIR="/tmp/jira-pre-status"

# If no saved statuses exist, nothing to do
if [ ! -d "$SAVE_DIR" ] || [ -z "$(ls -A "$SAVE_DIR" 2>/dev/null)" ]; then
    exit 0
fi

INPUT=$(cat 2>/dev/null) || true
TOOL_NAME=$(echo "$INPUT" | jq -r '.tool_name // empty' 2>/dev/null) || true

# Only process after Bash tool calls
if [ "$TOOL_NAME" != "Bash" ]; then
    exit 0
fi

CMD=$(echo "$INPUT" | jq -r '.tool_input.command // empty' 2>/dev/null) || true

# Only trigger on glab mr create or git push commands
is_mr_create=false
is_git_push=false

case "$CMD" in
    *glab*mr*create*) is_mr_create=true ;;
    *git*push*)       is_git_push=true ;;
    *rtk*git*push*)   is_git_push=true ;;
esac

if [ "$is_mr_create" = "false" ] && [ "$is_git_push" = "false" ]; then
    exit 0
fi

# Map status names to JIRA transition IDs
status_to_transition_id() {
    case "$1" in
        "To Do")                   echo "11" ;;
        "In Progress")             echo "21" ;;
        "Done")                    echo "31" ;;
        "Review")                  echo "41" ;;
        "Validate")                echo "51" ;;
        "Blocked")                 echo "61" ;;
        "Feature Decomposition")   echo "81" ;;
        "Ready for Development")   echo "91" ;;
        "Duplicate")               echo "2" ;;
        "Won't Fix")               echo "101" ;;
        *)                         echo "" ;;
    esac
}

restored_tickets=""

for status_file in "$SAVE_DIR"/PE-*; do
    [ -f "$status_file" ] || continue

    ticket=$(basename "$status_file")
    saved_status=$(cat "$status_file" 2>/dev/null) || continue

    if [ -z "$saved_status" ]; then
        rm -f "$status_file"
        continue
    fi

    transition_id=$(status_to_transition_id "$saved_status")
    if [ -z "$transition_id" ]; then
        rm -f "$status_file"
        continue
    fi

    # Defense-in-depth: only proceed if ticket/transition are well-formed.
    # Ticket is used in a URL path; transition_id is injected into a JSON body.
    if ! [[ "$ticket" =~ ^PE-[0-9]+$ ]] || ! [[ "$transition_id" =~ ^[0-9]+$ ]]; then
        rm -f "$status_file"
        continue
    fi

    # Spawn background process: wait for JIRA-GitLab automation, then restore.
    # The bash -c template is single-quoted (no outer interpolation); ticket,
    # saved_status, and transition_id are passed as positional arguments so a
    # crafted JIRA field cannot escape the shell. TLS verification is on
    # (no -k), and the JSON body is built with jq to avoid quote injection.
    nohup bash -c '
        ticket=$1
        saved_status=$2
        transition_id=$3
        sleep 15
        current_status=$(/usr/bin/curl -sf \
            -u "${JIRA_EMAIL}:${JIRA_API_TOKEN}" \
            "${JIRA_BASE_URL}/rest/api/2/issue/${ticket}?fields=status" 2>/dev/null \
            | jq -r ".fields.status.name // empty" 2>/dev/null) || exit 0

        if [ "$current_status" = "$saved_status" ]; then
            exit 0
        fi

        body=$(jq -cn --arg id "$transition_id" "{transition:{id:\$id}}") || exit 0

        /usr/bin/curl -sf \
            -u "${JIRA_EMAIL}:${JIRA_API_TOKEN}" \
            -X POST \
            -H "Content-Type: application/json" \
            -d "$body" \
            "${JIRA_BASE_URL}/rest/api/2/issue/${ticket}/transitions" >/dev/null 2>&1 || true
    ' _ "$ticket" "$saved_status" "$transition_id" >/dev/null 2>&1 &

    restored_tickets="${restored_tickets:+${restored_tickets}, }${ticket} (was: ${saved_status})"

    # Clean up the temp file
    rm -f "$status_file"
done

# Output info message for the agent
if [ -n "$restored_tickets" ]; then
    echo "<system-reminder>JIRA auto-transition prevention: scheduled status restore for ${restored_tickets}. The JIRA-GitLab automation will be reverted in ~15 seconds.</system-reminder>"
fi

exit 0
