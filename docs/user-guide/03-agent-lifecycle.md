# Agent Lifecycle

Agents move through a defined lifecycle from creation to completion. Understanding this lifecycle helps operators manage their fleet effectively.

## Agent States

```diagram
spawning → starting → working → yielding → working → done
                         │                     │
                         └──── crashed ────────┘
```

| State | Meaning |
|---|---|
| **spawning** | Pod is being created in K8s |
| **starting** | Pod is running, Claude Code session initializing |
| **working** | Agent is actively processing tasks |
| **yielding** | Agent is blocked on a decision, waiting for human input |
| **done** | Agent has finished all work and called `gb done` |
| **crashed** | Agent pod failed unexpectedly |

## How Agents Find Work

1. **Pre-assigned**: When spawned with a specific task (via @mention or `/spawn`), the task is pre-assigned
2. **Auto-assign**: On startup, `gb prime` finds the highest-priority unblocked task in the agent's project
3. **Ready queue**: After completing a task, agents run `gb ready` to find the next available bead
4. **Mail**: Other agents or operators can send work via `gb mail send`

## Agent Work Loop

A persistent agent follows this cycle:

```
1. Check for in-progress work: kd list --status=in_progress
2. If none, find new work: gb ready
3. Claim the task: kd claim <id>
4. Do the work (code, commit, push, PR)
5. Close the bead: kd close <id>
6. Loop back to step 2
7. When queue is empty: gb done
```

## Decisions — Human-in-the-Loop

When an agent encounters a choice that requires human judgment, it creates a **decision checkpoint**:

```bash
# Agent creates a decision
gb decision create --no-wait \
  --prompt="Auth refactor complete. Two options for session handling:" \
  --options='[
    {"id":"jwt","short":"Use JWT","label":"Stateless JWT tokens","artifact_type":"plan"},
    {"id":"session","short":"Sessions","label":"Redis-backed sessions","artifact_type":"plan"}
  ]'

# Agent blocks until human responds
gb yield
```

**What happens:**
1. Decision appears in Slack with option buttons
2. Agent waits (yielding state)
3. Human clicks an option, optionally adds rationale
4. Agent resumes with the chosen option
5. If an artifact is required, agent submits it via `gb decision report`

## Stopping Agents

### Graceful Stop
Agent calls `gb done` when all work is complete. This:
- Closes the agent bead
- Deletes the K8s pod
- Archives the session

### Kill via Slack
Operator uses `/kill agent-name` or the Kill button on the agent card. This:
- Sends a stop signal to the agent
- Agent gets a chance to clean up (commit, push)
- Pod is terminated

### Auto-Destruct
Operator can set a timer: `/autodestruct 30m` — agent stops after 30 minutes regardless of work state.

### Crash Recovery
If an agent pod crashes:
- The controller detects the failed pod
- Depending on config, may auto-restart with conversation resume
- `coop --resume` restores the previous Claude conversation
- Agent picks up where it left off

## Roles

### Crew
Standard worker agents. They:
- Pick up individual tasks from the ready queue
- Complete tasks autonomously
- Create decision checkpoints when blocked
- Use `gb done` when finished

### Gardener
Orchestration agents. They:
- Manage a fleet of crew agents
- Assign and prioritize work
- Monitor progress across the team
- Make strategic decisions about task allocation
- Can spawn/kill crew agents

## Session Management

Each agent runs a **coop session** — a persistent Claude Code instance managed by the coopmux terminal multiplexer.

**Session features:**
- **Resume**: After pod restart, `coop --resume` restores conversation history
- **Context recovery**: `gb prime` injects fresh workflow context after compaction
- **Credential rotation**: Coopmux manages API key rotation transparently
- **Session analysis**: `gb session analyze` reviews session efficiency

## Git Workflow

Agents use isolated git worktrees to prevent branch conflicts:

```bash
gb workspace setup <bead-id>    # Create worktree with auto-named branch
# ... do the work ...
git add && git commit && git push
glab mr create                   # Create merge request
gb mr link <bead-id> <mr-url>   # Link MR to bead for notifications
# ... after MR merged ...
gb workspace teardown <bead-id>  # Clean up worktree
```

Each agent works in its own branch, isolated from other agents working on the same repo.
