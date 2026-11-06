# Configuration & Deployment

## Helm Chart Deployment

Gardenio deploys to Kubernetes via a Helm chart at `gardenio/helm/gardenio/`.

### Components Deployed

| Component | Pods | Purpose |
|---|---|---|
| **Controller** | 1 | Watches bead events, manages agent pod lifecycle |
| **Slack Bridge** | 1 | Slack bot: mentions, commands, decisions, notifications |
| **Beads Daemon** | 1 | HTTP/gRPC API server for beads data |
| **PostgreSQL** | 1 | Persistent storage for beads |
| **NATS** | 1 | Pub/sub event bus for real-time updates |
| **Coopmux** | 1 | Terminal multiplexer, session manager, credential broker |
| **Beads Viewer** | 1 | Server-rendered bead constellation / agent-card web UI |
| **GitLab Bridge** | 1 | GitLab MR sync and webhooks |
| **Jira Bridge** | 1 | Jira ticket sync and webhooks |
| **Advice Viewer** | 1 | Web UI for browsing advice beads |

### Installation

Install the chart from a checkout of this repo, with images from the registry you publish them to
(`.github/workflows/images.yml` builds them; point the `image.repository` values below at your registry):

```bash
helm install gardenio ./gardenio/helm/gardenio \
  --set global.imageTag=<calver>
```

### Key Values

```yaml
# Controller
controller:
  image:
    repository: <your-registry>/gardenio/controller
  env:
    BEADS_HTTP_ADDR: "http://gardenio-beads:8080"
    NATS_URL: "nats://gardenio-nats:4222"

# Slack Bridge
slackBridge:
  env:
    SLACK_BOAT_TOKEN: "<bot-oauth-token>"
    SLACK_CHANNEL: "<notification-channel-id>"

# Agent defaults
agent:
  image:
    repository: <your-registry>/gardenio/agent
```

## Config Beads — Layered Configuration

Config beads provide a flexible, label-scoped configuration system that replaces hardcoded settings.

### How It Works

Configuration is organized into categories, each resolved independently:

| Category | What It Controls |
|---|---|
| `claude-settings` | Model, permissions, plugins, thinking mode |
| `claude-hooks` | SessionStart, PreCompact, Stop hooks |
| `claude-mcp` | MCP server definitions |
| `claude-instructions` | Workflow context, lifecycle rules, stop gate text |
| `type` | Bead type schemas (field definitions) |
| `view` | List view definitions |

### Scope Hierarchy

More specific labels override less specific ones:

```text
global (lowest priority)
  └── rig:<name>
       └── role:<name>
            └── agent:<id> (highest priority)
```

**Example**: A `global` config sets `model: "sonnet"`. A `role:gardener` config sets `model: "opus"`. Gardeners get opus; everyone else gets sonnet.

### Managing Config

```bash
# Export all config beads
gb config dump > config.json

# Edit config.json...

# Re-import (idempotent — deduplicates by title + labels)
gb config load config.json
```

### Config JSON Format

```json
{
  "config_beads": [
    {
      "title": "claude-settings",
      "labels": ["global"],
      "value": {
        "permissions": {"allow": ["Read", "Glob", "Grep"]},
        "model": "sonnet"
      }
    },
    {
      "title": "claude-settings",
      "labels": ["role:gardener"],
      "value": {
        "model": "opus"
      }
    }
  ]
}
```

### How Config Is Applied

1. **Pod startup**: `gb setup claude` resolves all categories and writes:
   - `~/.claude/settings.json` (user settings)
   - `{workspace}/.claude/settings.json` (hooks)
   - `{workspace}/.mcp.json` (MCP servers)
   - `{workspace}/CLAUDE.md` (instructions)
2. **Session hooks**: `gb prime` injects workflow context on SessionStart/PreCompact

## Per-Project Secrets

Projects can declare secret overrides via the `secrets` field on their project bead:

```bash
kd update <project-id> -f 'secrets=[
  {"env":"GITHUB_TOKEN","secret":"demo-gh-token","key":"token"},
  {"env":"JIRA_API_TOKEN","secret":"demo-jira","key":"api-token"}
]'
```

Each entry maps a K8s Secret key to an environment variable injected into agent pods for that project.

## Multi-Repo Projects

Projects can declare multiple repositories:

```bash
kd update <project-id> -f 'repos=[
  {"url":"https://github.com/org/main-repo.git","branch":"main","role":"primary"},
  {"url":"https://github.com/org/shared-lib.git","role":"reference","name":"shared-lib"}
]'
```

- **primary**: Cloned to the workspace work directory
- **reference**: Cloned to `workspace/repos/{name}` for read-only access

## Credential Architecture

```
Account Beads → Controller → Coopmux → Agent Sessions
```

1. **Account beads** define credential accounts (provider, API key)
2. **Controller** syncs credentials to coopmux via API
3. **Coopmux** distributes credentials to sessions with pool load balancing
4. Agents transparently use the assigned credentials
