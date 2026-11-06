# gardenio

[![daily full run: ci](https://github.com/mattbakerconsulting/gardenio/actions/workflows/ci.yml/badge.svg?event=schedule)](https://github.com/mattbakerconsulting/gardenio/actions/workflows/ci.yml?query=event%3Aschedule)
[![daily full run: checks](https://github.com/mattbakerconsulting/gardenio/actions/workflows/checks.yml/badge.svg?event=schedule)](https://github.com/mattbakerconsulting/gardenio/actions/workflows/checks.yml?query=event%3Aschedule)

Run a fleet of coding agents from Slack. Humans decide, agents build.

gardenio is a Kubernetes-native platform that runs fleets of Claude Code
agents against real repositories. The human loop lives in Slack: mention the
bot to spawn an agent, follow it in a thread, answer its decisions with a
button, approve an escalation when it needs something it is not allowed to do,
and kill it with a reaction. The work lives in one tracker, beads, where tasks,
decisions, mail, escalations, and agents are the same kind of object. The
agents live on your cluster, or on a laptop that joins the fleet.

## What it is

- **Agents as pods.** A controller watches the tracker and creates a pod per
  agent, each running Claude Code in an isolated workspace with its own git
  worktree, credentials leased by a broker, and a terminal multiplexer that
  survives crashes and resumes sessions.
- **Beads.** One work unit for everything: tasks, bugs, epics, plans,
  decisions, escalations, mail, agents, configs, schedules. Priorities,
  dependencies, labels, and a live event stream.
- **Slack as the control plane.** `@mention` to spawn, `/roster`,
  `/decisions`, threads as live agent conversations, agent cards, decision
  buttons, emoji lifecycle commands.
- **Escalations.** When an agent needs a permission it does not have, it files
  a request with the operation, the reason, and a sensitivity. A human or a
  policy approves, a privileged executor runs it, the result lands on the
  request. Agents ask for access the way engineers ask for production access.
- **Roles and plans.** Crews do tasks, captains watch plans, plans are phases
  of steps, formulas are reusable work templates.

## How it works

```text
Slack  ──mentions / buttons / reactions──▶  slack-bridge
                                                │
                                        controller ──creates──▶ agent pods (Claude Code + coop)
                                                │                      │
                                           beads daemon (kbeads) ◀─────┘  claim, squawk, decide, escalate
                                                │
                                           beads-viewer (web UI), GitHub / GitLab / Jira bridges
```

More detail in [`docs/user-guide/01-overview.md`](docs/user-guide/01-overview.md).

## Integrations

GitHub pull requests, GitLab merge requests, Jira issues, Slack. Review
comments and CI results flow back into the agent's thread.

## Try it

Clone the repo and run the onboarding script. It gets the `kd` and `gb` CLIs
(and Claude Code) talking to a gardenio daemon on Linux, macOS or WSL; it
checks first, shows its plan, and asks before each change.

```sh
git clone https://github.com/mattbakerconsulting/gardenio.git ~/gardenio
~/gardenio/scripts/onboard.sh
```

- **Quickstart**, from clone to a first agent pull request on your own
  repository in about ten minutes: [`docs/quickstart.md`](docs/quickstart.md).
- **Full operator guide:** [`docs/onboarding-mac-operator.md`](docs/onboarding-mac-operator.md).
- **Something broken?** `scripts/onboard.sh --doctor` is read-only: it
  diagnoses and prints the fix.
- **In Claude Code:** the `gardenio` skill (`/gardenio onboard`, `doctor`,
  `join`, `first-task`, `how do I`) walks through setup, joining as a local
  agent, and the `kd`/`gb` workflow.

## Repository layout

| Directory   | Language | What it is                                                        |
|-------------|----------|-------------------------------------------------------------------|
| `gardenio/` | Go       | Controller, Slack bridge, forge bridges, Helm chart, agent images |
| `coop/`     | Rust     | Agent terminal multiplexer and credential manager                 |
| `kbeads/`   | Go       | Beads daemon (HTTP and gRPC) and the `kd` CLI                     |
| `docs/`     |          | User guide, design specs, plans, ADRs                             |

See [`CLAUDE.md`](CLAUDE.md) for build and contribution details.

## Issue ids you will see

Documents and code comments reference two id namespaces. `gd-…` ids are
current and resolve in the running system. `ka-…` ids are the archived
namespace from the project's first year; they are kept for provenance and do
not resolve publicly.

## License

`gardenio/` and `kbeads/` are MIT. `coop/` is BUSL-1.1, converting to
Apache-2.0 on 2029-01-01. See [`LICENSE`](LICENSE).
