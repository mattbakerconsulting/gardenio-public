# Inter-Agent Communication

Gardenio agents coordinate through several communication channels: mail (DMs), news (activity feed), squawk (operator broadcasts), and Slack thread interaction.

## Mail — Direct Messages Between Agents

Mail beads are the primary way to send targeted messages to specific agents.

### Sending Mail

```bash
gb mail send <recipient-agent-name> \
  --subject="Task reassignment" \
  --body="Please take over ka-ABC123 — I'm blocked on credentials"
```

**Flags:**
- `--subject` / `-s` — Mail subject line
- `--body` / `-b` — Mail body content
- `--json` — Output created mail bead as JSON

### How Mail Delivery Works

1. Sender creates a mail bead addressed to the recipient
2. Recipient's `check-mail.sh` hook runs on `UserPromptSubmit` and `SessionStart`
3. Hook finds open mail beads for this agent
4. Mail content is injected into the agent's conversation
5. Agent processes the mail and can act on it

### Viewing Mail

```bash
kd list --type=mail --status=open    # All open mail
kd show ka-MAIL_ID --json           # Show specific mail
kd close ka-MAIL_ID                 # Mark as read/acknowledged
```

## News — Fleet Activity Feed

`gb news` provides a real-time activity digest showing what agents across the fleet are doing.

```bash
gb news
```

**Output includes:**
- Recently completed tasks
- Active agent status
- New decisions pending
- Recent squawk messages
- MR activity

Use `gb news` at session start to understand the current state of the fleet before picking up work.

## Squawk — Operator Broadcasts

Agents use `gb squawk` to send short status updates to the operator. These appear in Slack.

```bash
gb squawk "PR #42 merged, moving to next task"
gb squawk "Found critical bug in auth handler — creating decision checkpoint"
```

**Best practices:**
- Use for progress updates, completion notices, and important findings
- Keep messages short and actionable
- Don't use for questions (use `gb decision` instead)
- Don't use for long reports (use `gb decision report` instead)

## Slack Thread Interaction

Thread-spawned agents can read and reply to their Slack thread:

```bash
gb slack thread              # Read all messages from your thread
gb slack reply "Update: tests passing, pushing now"  # Post to thread
```

## Communication Patterns

### Assigning Work to a Running Agent

```bash
# Create the task
kd create "Fix auth bug" --type=task -l project:gardenio
# → ka-NEWTASK

# Send mail to the agent
gb mail send target-agent \
  -s "New task assigned" \
  -b "Please work on ka-NEWTASK: Fix auth bug"
```

### Operator → Agent (via Slack)

Simply post in the agent's Slack thread. The message is forwarded as a nudge to the agent's Claude session.

### Agent → Operator (Decision)

```bash
gb decision create --no-wait \
  --prompt="Found 3 approaches. Which do you prefer?" \
  --options='[...]'
gb yield
```

### Agent → Agent (via Mail)

```bash
gb mail send other-agent -s "Handoff" -b "I finished the API, you can start the frontend"
```
