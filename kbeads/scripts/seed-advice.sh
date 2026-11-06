#!/usr/bin/env bash
# seed-advice.sh — Create initial advice beads for kbeads agents.
# Run this once on a fresh kbeads installation to seed the advice system.
#
# Usage: KD_HOST=http://localhost:8080 ./scripts/seed-advice.sh
set -euo pipefail

# Check kd is available.
if ! command -v kd &>/dev/null; then
  echo "Error: kd not found in PATH" >&2
  exit 1
fi

echo "Seeding advice beads..."

# 1. Crew agent onboarding workflow
kd create "Beads crew agent onboarding: complete workflow for kbeads agents" \
  -t advice -p 2 \
  -l global \
  -d '## Kbeads Agent Workflow

### Command Split
- **kd** — CRUD operations (create, show, list, search, update, close, claim, dep, label, comment)
- **gb** — Orchestration (prime, ready, news, decision, yield, gate, bus, mail, setup, hook)

### Session Start
Your session is auto-initialized by `gb prime` (via Claude Code SessionStart hook). This injects:
- Workflow commands and patterns
- Agent-specific advice beads
- Live agent roster (to avoid work duplication)

### Finding and Claiming Work
```bash
gb ready                    # Show unblocked, unclaimed tasks
gb news                     # Check what other agents are working on
kd show <id>                # View full issue details
kd claim <id>               # Claim task (sets assignee + in_progress)
```
**Rules**: Only claim ONE task at a time. Check `gb news` first to avoid conflicts.

### Doing the Work
1. Read the issue description carefully (`kd show <id>`)
2. Make code changes in the appropriate repo
3. Test your changes locally if possible
4. Commit with descriptive messages

### Human Decisions
When you need human input (approval, choices, clarification):
```bash
gb decision create --no-wait \
  --prompt="What should I do?" \
  --options='\''[{"id":"a","short":"Option A","label":"Full description of A"}]'\''
gb yield    # Block and wait for response
```

### Completing Work
```bash
kd close <id> --reason="what was done"
git add <files> && git commit -m "feat: description" && git push
```
**CRITICAL**: Work is NOT done until `git push` succeeds.'

echo "  [1/6] Crew onboarding"

# 2. Single-task claiming rule
kd create "Single-task claiming: only kd claim one task at a time" \
  -t advice -p 2 \
  -l global \
  -d 'Only claim ONE task at a time with '\''kd claim'\''. Create sub-tasks as open beads with dependencies, but only claim the one you are actively working on. When it is done, close it with '\''kd close'\'' and claim the next. This prevents agent over-claiming and ensures clean task lifecycle tracking.'

echo "  [2/6] Single-task claiming"

# 3. Checkpoint decision protocol
kd create "Checkpoint decision protocol: When you hit a Stop hook checkpoint" \
  -t advice -p 2 \
  -l global \
  -d 'Checkpoint decision protocol: When you hit a Stop hook checkpoint, follow these steps:
1. Review the roster. Create beads for any next steps you identified.
2. Check for existing pending decisions: run '\''gb decision list'\'' first.
   Do NOT offer beads that already appear as options in another pending decision.
3. Create a decision with '\''gb decision create --no-wait'\'':
   --prompt="<what you did, blockers hit, and why these options>"
   --options='\''[{"id":"...","short":"...","label":"...","bead_id":"..."},...]'\''
4. If you are DONE with your current task, run '\''kd close <id>'\'' first.
5. Run '\''gb yield'\'' to block and wait for the human'\''s response.'

echo "  [3/6] Checkpoint protocol"

# 4. kbeads platform differences
kd create "kbeads vs beads: key platform differences for agents" \
  -t advice -p 2 \
  -l global \
  -d '## kbeads Platform Differences

kbeads uses a different architecture from beads:
- **Storage**: PostgreSQL (not Dolt) — always available, no sync needed
- **CLIs**: `kd` for CRUD, `gb` for orchestration (not `bd`)
- **Server**: HTTP API at KD_HOST (not Unix socket daemon)
- **Env vars**: KD_ACTOR, KD_AGENT_ID, KD_HOST (not BD_ACTOR, BEADS_ACTOR)
- **No .beads/ directory**: kbeads does not use local file storage
- **Config**: `kd config` backed by Postgres config table
- **Setup**: `gb setup claude --defaults` installs Claude Code hooks'

echo "  [4/6] Platform differences"

# 5. macOS binary overwrite warning
kd create "macOS SIGKILL when overwriting signed binaries — use rm+cp, not cp" \
  -t advice -p 3 \
  -l global \
  -d 'On macOS, overwriting a code-signed binary in-place with cp causes SIGKILL (exit 137) on subsequent executions. The kernel caches the code signature by inode; overwriting the file content invalidates the signature but the cache still references the old one. The fix: always rm the old binary before cp-ing the new one. Use '\''make install'\'' (which does rm -f then cp) instead of manual '\''cp ./kd $(which kd)'\''.'

echo "  [5/6] macOS binary warning"

# 6. Agent-mode optimizations
kd create "gb ready/list agent-mode optimizations: filters out other agents' work" \
  -t advice -p 2 \
  -l global \
  -d 'Agent-mode (CLAUDE_CODE or KD_AGENT_MODE=1) optimizations:

1. **gb ready** auto-filters out items assigned to other agents. Only shows: unassigned items (claimable) + items assigned to you.
2. **gb news** excludes your own work by default — shows only other agents'\'' activity.
3. **kd list** excludes advice, runbook, and molecule types by default.

These changes reduce noise for agents deciding what work to pick up.'

echo "  [6/7] Agent-mode optimizations"

# 7. Non-code text belongs in beads, not commits/MRs
kd create "Non-code text belongs in beads, not commits/MRs (CI-minute cost)" \
  -t advice -p 1 \
  -l global \
  -d 'You may get the urge to create a merge request or git commit to record documentation, reports, investigations, debugging results, plans, and similar prose. Do not. Git commits and MRs trigger CI pipelines, which incur a CI-minute penalty — they cost real money.

Instead, record this information in kd (beads), the org-wide persistent and visible data store. If the change is NOT a '\''code'\'' change, it can go in an appropriately-typed bead (type=doc, type=task, type=advice, etc.) or a bundle of beads.

Rule of thumb: only push commits/MRs for actual code (or config/chart) changes. For everything else — write-ups, findings, plans, post-mortems, design notes — create a bead or a bundle and link it to the originating work. This is faster, cheaper, and more visible (beads-viewer renders them for humans) than a doc buried in a git MR.'

echo "  [7/7] Non-code text in beads"

echo ""
echo "Done! Created 7 advice beads. Run 'kd list -t advice' to verify."
