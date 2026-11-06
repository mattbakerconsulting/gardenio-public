# gb CLI Reference — Agent Orchestration

`gb` is the orchestration CLI. It manages agent lifecycle, work assignment, inter-agent communication, configuration, and integration with external tools.

## Work Discovery

```bash
gb ready              # Show available (unblocked, unclaimed) beads
gb news               # Recent fleet activity feed
```

## Decisions

```bash
# Create a decision checkpoint
gb decision create --no-wait \
  --prompt="How should we handle auth?" \
  --options='[
    {"id":"jwt","short":"JWT","label":"Use JWT tokens","artifact_type":"plan"},
    {"id":"session","short":"Sessions","label":"Use server sessions","artifact_type":"plan"}
  ]'

# Wait for human resolution
gb yield

# Submit required artifact after decision
gb decision report ka-DECISION_ID --content "Implementation plan..."

# List/show decisions
gb decision list
gb decision show ka-DECISION_ID

# Watch decisions in real-time
gb decision watch
```

## Inter-Agent Communication

### Mail (DMs)

```bash
gb mail send agent-name --subject="New task" --body="Please work on ka-XYZ"
gb mail send agent-name -s "Review needed" -b "PR #123 needs review" --json
```

### Say and Read Threads

`gb say` is the one verb for talking. You name a thread or a channel, never a
surface: the bridge records the message as a bead and shows it in Slack and the
Commons, wherever the thread lives.

```bash
gb say "Deployment complete, all tests passing"   # progress line in your home thread
gb say --thread gd-abc123 "Fixed in #612"         # reply in another thread (its head bead id)
gb say --channel general "Release 2026.292.1 cut" # new post in a channel of your land
gb say --file shot.png "The new login page"       # attach files (repeat --file)
gb thread                                         # read your home thread
gb thread gd-abc123                               # read another thread
```

`gb squawk`, `gb slack reply`, `gb slack upload` and `gb slack thread` keep
working as permanent aliases (`gb squawk` = `gb say`, `gb slack reply` = `gb say`
into your home thread, `gb slack upload` = `gb say --file`, `gb slack thread` =
`gb thread`).

### Slack Channels

```bash
gb slack channels          # List available channels
gb slack search "keyword"  # Search Slack messages
```

## Agent Management

```bash
gb agent roster            # Show all active agents
gb fleet                   # Fleet-wide status
gb spawn --role crew --project demo "Fix bug"  # Spawn new agent
gb stop                    # Graceful shutdown (alias: gb done)
gb done                    # Signal work complete, despawn
```

## Workspace (Git Worktrees)

```bash
gb workspace setup ka-ABC123         # Create isolated worktree for bead
gb workspace setup ka-ABC123 --repo /path/to/other/repo  # Different repo
gb workspace list                     # Show all active worktrees
gb workspace teardown ka-ABC123       # Remove worktree after PR merged
gb workspace audit                    # Clean up orphaned worktrees
```

## Configuration

### Config Beads (Layered Settings)

```bash
gb config dump > config-backup.json   # Export all config beads as JSON
gb config load config.json            # Import config beads (idempotent)
```

**Config categories:**
- `claude-settings` — Model, permissions, plugins
- `claude-hooks` — SessionStart, PreCompact, Stop hooks
- `claude-mcp` — MCP server definitions
- `claude-instructions` — Workflow context, lifecycle rules

**Scope hierarchy (most specific wins):**
```
global < rig:<name> < role:<name> < agent:<id>
```

### Setup

```bash
gb setup claude            # Resolve config beads → write settings files
gb prime                   # Inject workflow context (run after compaction/clear)
```

## Advice (Persistent Knowledge)

```bash
gb advice add "Use feature branches for all changes" --project gardenio
gb advice add "kubectl set image requires decision checkpoint" -l global
gb advice list             # List all advice beads
gb advice show ka-ADV_ID   # Show advice details
gb advice remove ka-ADV_ID # Remove advice
```

Advice beads are distributed to matching agents via `gb prime` and survive pod restarts.

## Merge Requests

```bash
gb mr link ka-ABC123 https://gitlab.com/.../merge_requests/42  # Link MR to bead
gb mr status ka-ABC123     # Show MR status (bead id or MR URL, positionally)
gb mr status --bead ka-ABC123                       # ...or via the --bead alias
gb mr status --mr https://gitlab.com/.../merge_requests/42  # ...or the --mr alias
gb mr list                 # List linked MRs
gb mr env                  # Show MR-related env vars
```

## Pipelines

```bash
gb pipeline artifacts      # List CI pipeline artifacts
gb pipeline log            # Show pipeline log
```

## Session Management

```bash
gb session list            # List sessions
gb session show <id>       # Show session details
gb session register        # Register new session
gb session close           # Close current session
gb session analyze         # Analyze session efficiency
```

## Account Management

```bash
gb account                 # Show current credential account
```

## Gates

```bash
gb gate status             # Show gate status (what's blocking agent stop)
gb gate mark <gate> --force  # Mark gate satisfied (operator only)
gb gate clear <gate>       # Clear gate (operator only)
```

## Pool Management

```bash
gb pool status             # Show credential pool status
gb pool disable <account>  # Disable an account
gb pool enable <account>   # Re-enable an account
gb pool flush              # Flush pool state
gb pool assign <agent> <account>  # Assign account to agent
```

## Auditing

```bash
gb audit list              # List audit entries
gb audit fetch             # Fetch audit data
```

## Jira Integration

```bash
gb jira show <key>         # Show Jira ticket details
```

## State Management

```bash
gb state set key value     # Set agent state key
gb state get key           # Get agent state key
gb state list              # List all state keys
```

## Identity

```bash
gb identity                # Show current agent identity
gb identity add / seed     # Create user identity beads — needs an OIDC gardenio-admin (403 for static/fleet tokens)
```

Identity fields on user beads (`email`, `slack_id`, `gitlab_username`, ...) are
admin-only; see [`gardenio/docs/security/user-identity-fields.md`](../../gardenio/docs/security/user-identity-fields.md).

## Peek (Inspect)

```bash
gb peek                    # Inspect current agent state and context
```
