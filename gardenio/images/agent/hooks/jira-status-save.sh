#!/usr/bin/env bash
# jira-status-save.sh — PreToolUse hook (matcher: Bash)
#
# Captures the current JIRA ticket status BEFORE glab/git push commands
# that reference PE-XXXX tickets. Saves status to a temp file so the
# PostToolUse hook (jira-status-reset.sh) can restore it after the
# JIRA-GitLab automation fires.
#
# Runs silently — exits 0 with no output to avoid interfering with
# other PreToolUse hooks.

set -euo pipefail

# Only run if JIRA credentials are available
if [ -z "${JIRA_BASE_URL:-}" ] || [ -z "${JIRA_EMAIL:-}" ] || [ -z "${JIRA_API_TOKEN:-}" ]; then
    exit 0
fi

INPUT=$(cat)
CMD=$(echo "$INPUT" | jq -r '.tool_input.command // empty' 2>/dev/null) || exit 0

if [ -z "$CMD" ]; then
    exit 0
fi

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

# Extract PE-XXXX ticket IDs from the command (case-insensitive)
# Normalize to uppercase PE-XXXX for JIRA API compatibility
tickets=$(echo "$CMD" | grep -ioE 'PE-[0-9]+' | tr '[:lower:]' '[:upper:]' | sort -u || true)

# Also check the current git branch for PE-XXXX references
branch=$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo "")
if [ -n "$branch" ]; then
    branch_tickets=$(echo "$branch" | grep -ioE 'PE-[0-9]+' | tr '[:lower:]' '[:upper:]' | sort -u || true)
    if [ -n "$branch_tickets" ]; then
        tickets=$(printf '%s\n%s' "$tickets" "$branch_tickets" | sort -u | grep -v '^$' || true)
    fi
fi

if [ -z "$tickets" ]; then
    exit 0
fi

# Create temp directory for status saves
SAVE_DIR="/tmp/jira-pre-status"
mkdir -p "$SAVE_DIR"

# Save current status for each ticket
while IFS= read -r ticket; do
    [ -z "$ticket" ] && continue

    # Validate ticket format before using it in a URL path
    if ! [[ "$ticket" =~ ^PE-[0-9]+$ ]]; then
        continue
    fi

    # Get current JIRA status (TLS verification enabled)
    status=$(/usr/bin/curl -sf \
        -u "${JIRA_EMAIL}:${JIRA_API_TOKEN}" \
        "${JIRA_BASE_URL}/rest/api/2/issue/${ticket}?fields=status" 2>/dev/null \
        | jq -r '.fields.status.name // empty' 2>/dev/null) || continue

    if [ -n "$status" ]; then
        echo "$status" > "${SAVE_DIR}/${ticket}"
    fi
done <<< "$tickets"

# Exit silently — do not produce any output
exit 0
