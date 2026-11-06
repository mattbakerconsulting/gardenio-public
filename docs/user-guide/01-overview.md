# Gardenio Overview

Gardenio is a Kubernetes-native AI agent orchestration platform. It manages fleets of Claude Code agents that autonomously work on software engineering tasks — writing code, fixing bugs, creating merge requests, and coordinating with humans through Slack.

## What It Does

- **Spawns AI agents** as K8s pods, each running Claude Code in an isolated environment
- **Manages work** through "beads" — a unified issue/task tracking system with priorities, dependencies, and labels
- **Coordinates humans and agents** via Slack: decisions, notifications, mentions, and live dashboards
- **Integrates with dev tools**: GitHub PRs, GitLab MRs, Jira tickets, CI pipelines
- **Provides observability**: real-time bead constellation UI, agent session logs, fleet status

## Issue ids you will see

Two id namespaces appear throughout the docs and code. `gd-…` ids are current
and resolve in the running system. `ka-…` ids are the archived namespace from
the project's first year; they are kept for provenance and do not resolve
publicly. See the README's ["Issue ids you will see"](../../README.md#issue-ids-you-will-see).

## Architecture

```
┌─────────────────────────────────────────────────────────┐
│                    Slack Workspace                       │
│  @mentions  /spawn  /decisions  /roster  /kill          │
└────────────────────┬────────────────────────────────────┘
                     │
┌────────────────────▼────────────────────────────────────┐
│                 Slack Bridge                             │
│  Mentions → agent spawn    Decisions → Slack buttons    │
│  Slash commands            Thread forwarding            │
│  GitLab/Jira webhooks      Mail notifications           │
└────────────────────┬────────────────────────────────────┘
                     │
┌────────────────────▼────────────────────────────────────┐
│               Controller (gardenio)                      │
│  Pod lifecycle    Reconciler     Status reporter        │
│  Credential sync  Event stream   Config resolution      │
└────────────┬───────────────────┬────────────────────────┘
             │                   │
┌────────────▼──────┐  ┌────────▼─────────────────────────┐
│   Agent Pods      │  │      Beads Daemon (kbeads)       │
│  Claude Code      │  │  gRPC/HTTP API    PostgreSQL     │
│  coop (mux)       │  │  SSE events       NATS pub/sub   │
│  gb + kd CLI      │  │  Config store     Search         │
└───────────────────┘  └──────────┬────────────────────────┘
                                  │
                       ┌──────────▼────────────────────────┐
                       │          Beads Viewer             │
                       │  Constellation UI Agent cards     │
                       │  Decision UI      Live events     │
                       └───────────────────────────────────┘
```

### Component Summary

| Component | What It Does |
|---|---|
| **Controller** | Watches bead events, creates/deletes K8s pods for agents |
| **Slack Bridge** | Connects Slack to beads: spawns agents from mentions, posts decision buttons, forwards threads |
| **Beads Daemon** | Central database for all work items (beads), configs, agents, decisions |
| **Coopmux** | Terminal multiplexer that manages Claude Code sessions, credentials, and session resume |
| **Agent Pods** | K8s pods running Claude Code with gb/kd CLI tools, git, and language runtimes |
| **Beads Viewer** | Server-rendered web UI (bead constellation, agent cards, decisions) with live agent monitoring |
| **GitLab Bridge** | Syncs GitLab MRs, forwards review comments, updates MR status on beads |
| **Jira Bridge** | Imports Jira tickets as beads, syncs status bidirectionally |

## Key Concepts

### Beads
A **bead** is the universal work unit. Everything is a bead: tasks, bugs, features, epics, decisions, mail, agents, configs, schedules. Each bead has:
- **ID**: Unique identifier (e.g., `ka-ABC123`)
- **Type**: task, bug, feature, epic, decision, agent, config, etc.
- **Kind**: issue (actionable work), data (information), config (infrastructure)
- **Status**: open or closed
- **Priority**: 0 (default) to N (higher = more urgent)
- **Labels**: Key-value pairs for filtering (e.g., `project:gardenio`, `priority:high`)
- **Fields**: Type-specific structured data
- **Dependencies**: blocks, parent-child, assigned relationships

### Agents
An **agent** is a Claude Code instance running in a K8s pod. Agents:
- Are represented as beads (type=agent)
- Have roles (crew, gardener) and belong to projects
- Claim work from the ready queue, complete it, and loop
- Communicate via decisions (human input), mail (inter-agent), and squawk (status updates)
- Have isolated git worktrees per task

### Decisions
A **decision** is a checkpoint where an agent pauses for human input. Decisions appear in Slack with clickable option buttons. The agent blocks (via `gb yield`) until the human resolves the decision.

### Config Beads
Configuration is managed through **config beads** — layered settings that cascade from global → rig → role → agent scope. Config beads control Claude settings, hooks, MCP servers, and instructions.
