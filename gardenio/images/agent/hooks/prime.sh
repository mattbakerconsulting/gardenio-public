#!/bin/bash
# prime.sh — SessionStart hook that outputs role-specific priming context.
#
# Renders the full prime context via `gb prime` (workflow context, advice,
# jacks, roster, auto-assign), then shows this agent's own bead (assignment).
#
# Env:
#   BOAT_ROLE           — agent role (gardener, crew, job)
#   BOAT_AGENT_BEAD_ID  — this agent's bead ID (set by controller)
#
# Always exits 0 so hook failures don't block Claude.

set -euo pipefail

BEAD_ID="${BOAT_AGENT_BEAD_ID:-}"

# Render the full prime context (workflow, advice, roster, auto-assign).
gb prime 2>/dev/null || true

# Show this agent's own bead — title is the task, description holds
# instructions, and dependencies are the assigned work beads.
if [ -n "${BEAD_ID}" ]; then
    echo ""
    echo "## Assignment"
    echo ""
    kd show "${BEAD_ID}" 2>/dev/null || true
fi

exit 0
