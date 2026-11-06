#!/bin/bash
# wiki-sync.sh — Stop hook that publishes queued wiki pages to GitLab.
#
# During a session, agents queue wiki updates by appending JSONL lines to
# /tmp/wiki-sync-queue.jsonl. Each line is a JSON object:
#   {"slug": "services/data-dictionary", "title": "Data Dictionary Service", "content": "# ..."}
#
# On Stop, this hook reads the queue and publishes each page via the
# GitLab wiki API (PUT to update, POST to create if 404).
#
# Requires: GITLAB_TOKEN, curl, jq
# Config: WIKI_PROJECT_ID (default: 79511414 = example-org/monorepo)

set -uo pipefail

WIKI_QUEUE="${WIKI_SYNC_QUEUE:-/tmp/wiki-sync-queue.jsonl}"
WIKI_PROJECT_ID="${WIKI_PROJECT_ID:-79511414}"
WIKI_API="https://gitlab.com/api/v4/projects/${WIKI_PROJECT_ID}/wikis"

# Nothing to do if no queue file or no token.
[ -f "$WIKI_QUEUE" ] || exit 0
[ -n "${GITLAB_TOKEN:-}" ] || { echo "[wiki-sync] GITLAB_TOKEN not set, skipping" >&2; exit 0; }

synced=0
failed=0

while IFS= read -r entry; do
    # Skip empty lines.
    [ -z "$entry" ] && continue

    slug=$(echo "$entry" | jq -r '.slug // empty')
    title=$(echo "$entry" | jq -r '.title // empty')
    content=$(echo "$entry" | jq -r '.content // empty')

    if [ -z "$slug" ] || [ -z "$title" ] || [ -z "$content" ]; then
        echo "[wiki-sync] skipping malformed entry: missing slug, title, or content" >&2
        continue
    fi

    # Try PUT (update) first; create via POST if 404.
    status=$(curl -s -o /dev/null -w '%{http_code}' \
        -H "PRIVATE-TOKEN: $GITLAB_TOKEN" -X PUT \
        "$WIKI_API/$slug" \
        --data-urlencode "title=$title" \
        --data-urlencode "content=$content" \
        -d "format=markdown")

    if [ "$status" = "404" ]; then
        # Page doesn't exist yet — create it.
        status=$(curl -s -o /dev/null -w '%{http_code}' \
            -H "PRIVATE-TOKEN: $GITLAB_TOKEN" -X POST \
            "$WIKI_API" \
            --data-urlencode "title=$title" \
            --data-urlencode "content=$content" \
            -d "format=markdown")
    fi

    if [ "$status" -ge 200 ] && [ "$status" -lt 300 ]; then
        synced=$((synced + 1))
    else
        echo "[wiki-sync] failed to publish '$slug' (HTTP $status)" >&2
        failed=$((failed + 1))
    fi
done < "$WIKI_QUEUE"

# Clean up the queue.
rm -f "$WIKI_QUEUE"

if [ "$synced" -gt 0 ]; then
    echo "[wiki-sync] published $synced wiki page(s)" >&2
fi
if [ "$failed" -gt 0 ]; then
    echo "[wiki-sync] $failed page(s) failed to publish" >&2
fi

exit 0
