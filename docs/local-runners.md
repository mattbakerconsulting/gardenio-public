# Local runners: operator runbook

A **runner** is a person's own machine registered with the fleet. A **local
agent** is a Claude Code session on that machine that the fleet can see: it has
an agent bead, a thread and a card, and it works beads like a pod agent does. It
runs **as its owner**, with the owner's Claude login (D1) and the owner's
beads-viewer authorization (D6).

Design: [`docs/superpowers/specs/2026-10-11-local-agents-escalations-design.md`](superpowers/specs/2026-10-11-local-agents-escalations-design.md)
(§4 runners, §5 lifecycle, §6 capabilities). Plan epic: gd-ofkNSDsj2N.

## Lifecycle

| Step | Who | Command |
|---|---|---|
| Invite | operator, in Slack | `/runner invite @person <project>...` (prints an invite code and the runner daemon URL; `/runner revoke` undoes it) |
| Register | owner, once per machine | `gb runner register --invite <code> --daemon <url> [--tags gpu,mac]` |
| Join | owner, per session | `gb runner join [--project p] [--dir .] [--name n]` (attached; Ctrl-C leaves) |
| Status | owner | `gb runner status` (runner bead and its local agents) |
| Leave | owner | `gb runner leave` (stops this runner's local agents, marks the runner away) |

`gb runner join` calls `gb agent start --local --role local --runner <id>
--owner <owner>`. That registers the agent bead:

- `type=agent`, title and assignee = the agent name, labels
  `execution_target:laptop` and `project:<p>`;
- fields `agent`, `role=local`, `project`, `mode=crew`, `agent_state=working`,
  `execution_target=laptop`, `runtime_id=<runner bead id>`,
  `operated_by=<owner>`.

Because it is a `type=agent` bead, `ListAgentBeads`, the roster and cards see
it. The escalation bridge's `triggerLocal` lookup does not: it matches the
`execution_target:local` label, and a laptop agent is not a fleet executor for
other people's escalations. gb closes the bead when the session
exits. A hand-run `gb agent start --local --runner <id>` with no `--role` also
takes `local`; without `--runner` the legacy default stays `crew`. `--docker`
still registers the old `kd:agent` task bead.

## The controller leaves local agents alone

`execution_target=laptop` (`podmanager.ExecutionTargetLaptop`) means the
controller runs nothing for the bead. The marker is deliberately not `local`:
the escalation bridge stamps `execution_target=local` on the fleet pods it
spawns for a local-fallback escalation, and those still need a pod. A trigger
or plan gardener whose `execution_target` is `laptop` has it dropped (with a
warning), since nothing would ever run its agents. For a laptop bead:

- the SSE subscriber emits no lifecycle event for it (no spawn, stop, teardown
  or auth failover);
- the reconciler leaves it out of desired state (no pod cold-start, no
  podless-leak close);
- the stale reaper never reaps it;
- a bridge restart or respawn of its thread re-mints on the fleet and does not
  carry `laptop` forward;
- a decision or escalation that resolves while it is not running never spawns
  a pod: the bridge writes it one interrupt mail carrying the outcome
  (`reviveLocalAgentByMail`), and the next session under that name drains it;
- urgent mail it has not picked up is held, with a note in the thread, rather
  than respawned.

## The `local` role

Create it once per fleet, with no overrides:

```bash
gb role scaffold local
```

The `config:claude-instructions:role:local` and `config:nudge-prompts:role:local`
beads are seeded by the bridge (#1294), so scaffold reports them as existing.

Do **not** give the `local` role `enabled_skills`, `privileged`, `secrets`,
an image, a model or an account. None of them would apply. A local agent is not
a sandboxed role with a capability list: it runs on the owner's laptop as the
owner (D6), so its ceiling is whatever the owner may do in beads-viewer. Pod
overrides never reach it because the controller never builds a pod for it.

## What a local agent can and cannot do (D6)

- **Can:** everything its owner can do as a beads-viewer user, in the
  projects its runner was invited to: read, claim and update beads, comment,
  squawk, file escalations. Writes are attributed to the owner
  (`operated_by`). Git pushes and PRs carry the owner's identity.
- **Cannot:** anything the owner cannot do from the viewer. It cannot read
  other projects, list runners, touch config beads, approve escalations,
  spawn a privileged role, read a Secret or run `kubectl`. A runner token may
  set `runtime_id` only to its own runner, on its own agents. Cluster-side
  work goes local-to-cloud through an escalation (spec §8.3).

## Placement is explicit (D4)

Work reaches a runner only when someone chose it. Pool pressure, cluster
capacity and taints never pick a runner, and an explicit choice that cannot be
honoured is said and stops; it does not fall back to a pod.

- **The task says so.** `placement=local` plus `runner_id=<runner bead id>` or
  `runner_tag=<tag>`. `gb ready` in that runner's session lists it; `gb ready`
  in a cluster agent hides it; `gb spawn --task` / `gb start --task` refuse it
  ("task <id> is placed on runner <id>; it will be picked up by gb runner join
  there").
- **`/spawn ... --runner <hostname>`.** Only one of your own runners. The bridge
  stamps `placement=local` and `runner_id` on the task and mails the runner's
  agent instead of spawning a pod. `/start` refuses `--runner`.
- **The owner's own thread.** When you mention the bot in a thread you opened
  and one of your runners is online with an agent joined, the thread is bound
  to that agent and the mention is mailed to it. With several online the bot
  asks which (`--runner <hostname>` in your next mention); with one online but
  no agent joined it says to run `gb runner join` there. If all your runners
  are offline it says so and spawns on the cluster; someone with no runner hears
  nothing new. `--runner <hostname>` in a mention works the same in any thread.

A runner is online while `state=online` and its last heartbeat is under 90 s
old. Placement code lives in `gardenio/controller/internal/bridge/placement.go`.

An unplaced task shows in a runner's `gb ready` only when it is in one of the
runner's projects and carries `fields.opened_by_operator` equal to the owner.
Nothing stamps that field yet, so today a runner's queue holds placed work only.
A human's own `gb ready` on a registered laptop is never narrowed.

## Disconnects hold work (D5)

A runner that goes away (lid closed, network lost) **holds** its task: the
bead stays assigned and nothing re-places it. The fleet does not reap a local
agent for being quiet.

The controller's stale reaper reads each laptop agent's runner (its
`runtime_id`) on every pass:

| Runner | Agent |
|---|---|
| `state=away`, or no heartbeat for 90 s | set to `agent_state=paused` and held, with no timeout. The bridge posts once in the agent's thread: "*agent* is paused: *host* is offline. Work is held until it is back; `/kill agent` releases it." The card reads "paused (laptop offline)". |
| online again | a paused agent goes back to `working`; its mail subscriber drains what arrived meanwhile when it reconnects. |
| `state=revoked`, closed or missing | the agent bead is closed (`failed`, `close_reason` "local runner revoked or gone") and its task returns to ready. |

A runner that cannot be read in a pass (a daemon error) leaves its agents as
they are. A laptop agent with no `runtime_id`, or one already stopping or
finished, is left to gb.

To release held work, a human runs `/kill <agent>`, which closes the agent
bead and returns the task to ready (spec §5). A paused agent is closed at once,
without waiting for it to read a stop mail.

## Owner-billed

A local agent runs on the owner's own Claude login, so the fleet pays nothing
for it. Its bead never carries `total_cost_usd` or other fleet cost fields.
Roster and cards (plan Task 14) read `execution_target`, `runtime_id` and
`operated_by` from the agent bead and show it as owner-billed rather than as $0
of fleet spend.
