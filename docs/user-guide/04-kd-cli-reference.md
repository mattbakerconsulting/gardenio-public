# kd CLI Reference — Work Management

`kd` is the CLI for the beads daemon. It manages beads (work items), dependencies, formulas, and server configuration.

## Core CRUD

### Create a Bead

```bash
kd create "Fix login page CSS" --type=task --priority=3 -l project:demo -l priority:high
kd create "Auth system overhaul" --type=epic -d "Migrate from sessions to JWT"
kd create "Button overflow on mobile" --type=bug -l component:frontend
```

**Flags:**
- `--type` — Bead type: task, bug, feature, epic, chore, advice, decision, mail, etc.
- `--priority` — Numeric priority (higher = more urgent, 0 = default)
- `-l` / `--label` — Add labels (repeatable): `-l key:value`
- `-d` / `--description` — Bead description text
- `-f` / `--field` — Set typed fields: `-f 'provider=anthropic'`
- `--json` — Output as JSON

### Show a Bead

```bash
kd show ka-ABC123           # Human-readable output
kd show ka-ABC123 --json    # JSON output with all fields
```

### Update a Bead

```bash
kd update ka-ABC123 --priority=5
kd update ka-ABC123 --status=open
kd update ka-ABC123 -f 'branch=fix/auth-bug'
kd update ka-ABC123 -l new-label:value
kd update ka-ABC123 --assignee=agent-name
```

### Close a Bead

```bash
kd close ka-ABC123
```

### Delete a Bead

```bash
kd delete ka-ABC123    # Permanent deletion
```

## Listing & Filtering

```bash
kd list                                    # Open issue-kind beads (default)
kd list --status=open                      # Explicitly open
kd list --status=closed                    # Closed beads
kd list --type=task                        # Only tasks
kd list --type=bug -l priority:high        # High-priority bugs
kd list -l project:gardenio --status=open   # Open gardenio issues
kd list --type=task --type=bug             # Multiple types
kd list --json                             # JSON output
kd list --all-types                        # Include data and config kinds
kd list --kind=data                        # Only data-kind beads
kd list --kind=config                      # Only config-kind beads
```

**Kind filter (default: issue only):**
- `--all-types` — Show all kinds
- `--kind=data` — Decisions, mail, advice, events
- `--kind=config` — Agents, roles, configs
- `--type=<specific>` — Overrides kind filter

### Search

```bash
kd search "login bug"                      # Full-text search
kd search "auth" --type=task --status=open # Filtered search
kd search --semantic "auth bugs in login"  # Embedding-based semantic search
kd similar ka-ABC123                       # Find beads similar to a given one
```

Semantic search, `kd similar`, and `kd embed sync` are covered in depth in
[12-semantic-search.md](12-semantic-search.md), including score
interpretation, deployment prereqs, and troubleshooting.

## Claim / Unclaim

Claiming atomically sets status=in_progress and assignee:

```bash
kd claim ka-ABC123       # Claim for yourself (KD_ACTOR)
kd unclaim ka-ABC123     # Release back to ready queue
```

## Dependencies

```bash
kd dep add ka-CHILD ka-PARENT --type=parent-child   # Epic → task
kd dep add ka-BLOCKED ka-BLOCKER --type=blocks       # Task B blocked by Task A
kd dep add ka-AGENT ka-TASK --type=assigned           # Pre-assign to agent
kd dep list ka-ABC123                                  # List dependencies
kd dep remove ka-ABC123 ka-DEF456                      # Remove a dependency (default --type=blocks)
kd dep remove ka-ABC123 ka-DEF456 --if-exists          # ...tolerating an already-absent edge
```

> **`kd dep remove` matches the full triple** — `(bead, depends-on, --type)`. `--type`
> defaults to `blocks`, so removing a `parent-child` edge without passing `--type` matches
> nothing. A no-match **exits 1** with `no such dependency: <child> -<type>-> <parent>`
> rather than reporting a removal that did not happen; check the real edges with
> `kd dep list`. Pass `--if-exists` for scripted/idempotent removal, where "already gone"
> is the desired end state and should exit 0.

### Dependency Types

| Type | Meaning |
|---|---|
| `parent-child` | Epic/task hierarchy |
| `blocks` | Task cannot start until blocker is closed |
| `assigned` | Pre-assigns work to an agent on startup |

### Navigation

```bash
kd children ka-EPIC_ID    # Show children of an epic
kd graph ka-ABC123         # Show dependency graph
```

## Comments

```bash
kd comment add ka-ABC123 "Found the root cause: null pointer in auth handler"
kd comment list ka-ABC123
```

## Formulas (Templates)

Formulas are reusable bead templates with variable substitution:

```bash
# Create a formula from JSON
echo '{"title":"Fix {{component}}","vars":["component","priority"],"steps":[...]}' | kd formula create --json

# List formulas
kd formula list

# Show formula details
kd formula show ka-FORMULA_ID

# Dump formula as JSON
kd formula dump ka-FORMULA_ID

# Apply formula with variable substitution
kd formula apply ka-FORMULA_ID --var component=auth --var priority=high
```

## Batch Operations

```bash
kd-batch "show ka-ABC" "show ka-DEF" "dep list ka-GHI"   # Parallel queries
```

## Fleet Management

```bash
kd fleet list              # List all agents
kd fleet show agent-name   # Show agent details
kd fleet transfer ka-TASK ka-AGENT  # Transfer task to agent
```

## Server

```bash
kd serve                   # Start the beads daemon server
kd remote add prod https://beads.example.com   # Add remote server
kd remote list             # List configured remotes
kd remote use prod         # Switch to remote
```

## Configuration

```bash
kd config                  # Show current config
kd context                 # Show current context (remote, actor)
```

## Common Patterns

### Create an Epic with Children

```bash
# Create epic
kd create "Auth system rewrite" --type=epic -l project:demo
# → ka-EPIC_ID

# Create child tasks
kd create "Extract token validation" --type=task -l project:demo
# → ka-TASK1
kd dep add ka-TASK1 ka-EPIC_ID --type=parent-child

kd create "Add JWT refresh rotation" --type=task -l project:demo
# → ka-TASK2
kd dep add ka-TASK2 ka-EPIC_ID --type=parent-child

# Block task 2 on task 1
kd dep add ka-TASK2 ka-TASK1 --type=blocks
```

### Filter by Multiple Labels

```bash
kd list -l project:demo -l priority:high --status=open --type=bug
```
