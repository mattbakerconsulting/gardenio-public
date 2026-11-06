# Scheduling & Automation

Gardenio supports automated agent spawning on schedules, auto-assignment of work, and formula-based task templating.

## Scheduled Agents

Schedule beads define cron-based agent spawning. When a schedule fires, it creates an agent with the configured role, project, and prompt.

### Creating Schedules

Via Slack:
```
/schedule create --cron "0 9 * * 1-5" --role crew --project demo --prompt "Run morning health checks"
```

Via kd:
```bash
kd create "Morning health check" --type=schedule \
  -f 'cron=0 9 * * 1-5' \
  -f 'enabled=true' \
  -f 'project=demo' \
  -f 'role=crew' \
  -f 'prompt=Run morning health checks and report status' \
  -f 'timezone=America/Los_Angeles'
```

### Schedule Fields

| Field | Description | Example |
|---|---|---|
| `cron` | Standard 5-field cron expression | `0 9 * * 1-5` (weekdays 9am) |
| `enabled` | Whether schedule is active | `true` / `false` |
| `project` | Target project | `demo` |
| `role` | Agent role | `crew`, `gardener` |
| `prompt` | Task prompt for spawned agent | `Run health checks` |
| `timezone` | IANA timezone name | `America/Los_Angeles` |

### Managing Schedules

```bash
/schedule list                # List all schedules
/schedule show <id>           # Show schedule details
/schedule enable <id>         # Enable a schedule
/schedule disable <id>        # Disable a schedule

kd list --type=schedule       # List via kd
kd show <schedule-id> --json  # View schedule details
kd update <id> -f 'enabled=false'  # Disable
```

### Timezone Handling

- Use IANA timezone names (e.g., `America/Los_Angeles`, not `PST`)
- Cron hours are evaluated in the schedule's local timezone
- DST transitions are handled automatically by Go's `time.LoadLocation`
- If no timezone is set, the project's `default_timezone` is used as fallback

## Auto-Assignment

Agents automatically find and claim work through the ready queue:

1. Agent calls `gb ready` → returns highest-priority unblocked beads
2. Agent picks the top result and claims it: `kd claim <id>`
3. Claiming atomically sets status=in_progress and assignee

**Priority ordering:**
- Higher numeric priority = more urgent = served first
- Beads with dependencies on closed beads become unblocked
- Only beads matching the agent's project scope are shown

## Formulas — Templated Work

Formulas are reusable templates for creating sets of related beads with variable substitution.

### Formula Definition

```json
{
  "title": "Bug Fix MR Quality Gates",
  "vars": ["ticket", "component"],
  "steps": [
    {
      "title": "Fix {{ticket}} in {{component}}",
      "type": "task",
      "description": "Implement the fix for {{ticket}}",
      "labels": ["project:demo", "component:{{component}}"]
    },
    {
      "title": "Test {{ticket}} fix",
      "type": "task",
      "description": "Write and run tests for the {{ticket}} fix",
      "labels": ["project:demo", "testing"]
    },
    {
      "title": "Create MR for {{ticket}}",
      "type": "task",
      "description": "Open merge request with the fix",
      "labels": ["project:demo", "mr"]
    }
  ]
}
```

### Using Formulas

```bash
# Create formula
echo '<json>' | kd formula create --json

# Apply with variables
kd formula apply ka-FORMULA_ID --var ticket=PE-1234 --var component=auth

# Via Slack
/formula ka-FORMULA_ID --var ticket=PE-1234 --var component=auth
```

### Release Formula

The gardenio release process has a golden formula:

```bash
kd formula apply ka-0000000359 \
  --var version=YYYY.DDD.N \
  --http-url https://gardenio.example.com/kd
```

## Auto-Destruct

Set a timer for automatic agent shutdown:

```
/autodestruct 30m    # Agent stops after 30 minutes
/autodestruct 2h     # Agent stops after 2 hours
```

Useful for time-boxed investigations or when you want agents to stop after a reasonable period.

## Bouncer

The bouncer manages thread cleanup and agent lifecycle boundaries:

```
/bouncer             # Show bouncer status and configuration
```
