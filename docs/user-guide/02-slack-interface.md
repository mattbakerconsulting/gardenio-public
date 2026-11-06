# Slack Interface

Gardenio's primary user interface is Slack. Operators interact with agents through @mentions, slash commands, decision buttons, and thread conversations.

## Spawning Agents

### @mention (Recommended)
The simplest way to spawn an agent: mention @gardenio in any channel with a task description.

```
@gardenio fix the login page CSS on mobile — the submit button overflows on iPhone SE
```

This creates:
1. A new thread in the channel
2. A bead (type=task or based on context)
3. An agent pod in K8s
4. The agent starts working and posts updates in the thread

**Routing rules:**
- If in an agent's existing thread → message goes to that agent
- If you name a specific agent (`@gardenio agentname do X`) → routes to that agent
- If in a channel mapped to an agent → routes to that agent
- Otherwise → auto-spawns a new thread agent

### /spawn Command
For more control over agent creation:

```
/spawn fix login CSS --role crew --project demo
/spawn investigate auth failures --role gardener
```

**Flags:**
- `--role <role>` — Agent role (crew, gardener)
- `--project <project>` — Project scope
- Task description follows the command

### /start Command
Start a pre-configured agent or resume a stopped one:

```
/start <agent-name>
```

## Slash Commands Reference

| Command | Purpose | Example |
|---|---|---|
| `/spawn` | Create a new agent with a task | `/spawn fix auth bug --project demo` |
| `/start` | Start/resume an agent | `/start my-agent` |
| `/kill` | Stop an agent | `/kill agent-name` |
| `/kill-thread` | Kill agent in current thread | `/kill-thread` |
| `/roster` | Show all active agents | `/roster` |
| `/decisions` | List pending decisions | `/decisions` |
| `/unreleased` | Show unreleased commits | `/unreleased` |
| `/formula` | Apply a formula template | `/formula ka-ABC --var key=value` |
| `/schedule` | Manage scheduled agents | `/schedule list` |
| `/bouncer` | Manage auto-destruct | `/bouncer` |
| `/autodestruct` | Set agent auto-destruct timer | `/autodestruct 30m` |
| `/clear-threads` | Clean up orphaned threads | `/clear-threads` |

## Decision Notifications

When an agent needs human input, it creates a **decision bead**. The Slack bridge posts an interactive message:

**What you see in Slack:**
- A message with the agent's question/context
- Clickable buttons for each option (e.g., "Approve", "Reject", "Use JWT", "Use sessions")
- Rationale text field for your reasoning
- The agent blocks until you click a button

**How to respond:**
1. Read the agent's prompt
2. Click the appropriate option button
3. Optionally add rationale text
4. The agent resumes with your choice

**Decision management:**
- `/decisions` — See all pending decisions across agents
- Decisions auto-post to the agent's thread and any configured notification channels

## Thread Interaction

Each agent has a Slack thread. You can:
- **Send messages** in the thread — they're forwarded to the agent as nudges
- **React with emojis** — lifecycle reactions (e.g., skull to kill)
- **Reply to agent updates** — agent sees them as context

### Thread Keywords
In an agent's thread, certain keywords trigger actions:
- `kill` / `clear` — Stop the agent
- Direct messages are forwarded as prompts to the agent

## Agent Cards

The Slack bridge posts **agent cards** — rich message attachments showing:
- Agent name and status (starting, working, yielding, done)
- Current task/bead
- Session duration
- **Kill** and **Clear** buttons for quick management

## Notifications

The bridge sends notifications for:
- **Agent lifecycle**: started, stopped, crashed, restarted
- **Decisions**: new decisions posted, decisions resolved
- **Mail**: inter-agent messages forwarded to Slack
- **MR events**: GitLab review comments, pipeline status
- **Squawk**: agent status broadcasts
