# Where does my agent's card go?

This page tells an operator which Slack channel an agent's card goes to, and how to
change it, without reading the bridge code. It matches the code on main as of
2026-10-29 (plan gd-3cFHKIHCen).

The rules are in `explainChannelRoute`
(`gardenio/controller/internal/bridge/route_explain.go`). The bridge's `resolveChannel`
runs the same function, and so does `gb slack route`. If this page and that file ever
disagree, the file is right; fix this page.

## The rules, in order

The bridge tries each rule from the top and uses the first one that gives a channel.
The name in brackets is how `gb slack route` refers to the rule.

1. **Router override** (`router`). If the bridge has a router (helm
   `slackBridge.router`, env `SLACK_ROUTER_CONFIG`), an `overrides` entry or a
   `channels` pattern that matches the agent wins over everything else.
   Example: `overrides: {"gardenio-hq": "C0HQ"}` sends every card for `gardenio-hq` to
   `C0HQ`. No deploy sets a router today, so this rule normally does nothing.

   `channels` patterns match the agent's full `project/role/name` identity: the project
   and role the bridge has cached for the agent, then its short name. Example:
   `channels: {"gardenio/crew/*": "C0X"}` sends every gardenio crew card to `C0X`. For
   a crew agent `gardenio-hq` in project gardenio, the router checks, in order:
   1. an override on the full identity (`gardenio/crew/gardenio-hq`),
   2. an override on the short name (`gardenio-hq`),
   3. the patterns, against the full identity.

   So a short-name override still beats a pattern. If the bridge has no cached project
   or role for the agent, it matches only short-name overrides and one-segment
   patterns, and otherwise falls through to the next rule. `gb slack route` shows which
   entry matched as `matched gardenio/crew/*` or `matched (override)`.

   Bridges from release 2026.302.1 and older match on the short name only, so there a
   `project/role/name` pattern never matches and only short-name overrides work. The
   fix (PR #1403, gd-H74pBA3OyK) ships in the first release cut after 2026.302.1.

2. **Explicit spawn pin** (`spawn_channel`). The agent was given a channel when it was
   spawned. Any of these counts:
   - `gb spawn --slack-spawn-channel <#name|ID>`. Example:
     `gb spawn --role crew --task gd-X --slack-spawn-channel #gardenio-ops` puts the card
     in #gardenio-ops.
   - `/spawn` typed in a Slack channel pins that channel.
   - `gb spawn --task <id>` for a Slack-bound task copies the task's
     `slack_spawn_channel` (and its thread binding, see [Threads](#threads-and-cards)).
     Example: a task raised from a #bill-navarro thread puts its crew's card back in
     #bill-navarro.
   - A schedule's Slack channel. A trigger or plan-gardener with a channel stamps it as
     `schedule_slack_channel` on each agent it spawns. If an agent bead has both
     `schedule_slack_channel` and `slack_spawn_channel`, the schedule's channel wins.
     Example: a plan-gardener whose schedule channel is #gardenio-plans posts its
     card in #gardenio-plans.

3. **Role channel** (`role_channel`). The project's `channel_roles` maps a channel to
   the agent's role. Example: `channel_roles: {"C0B8Q04M4KZ": ["crew"]}` sends every
   gardenio crew card to #bill-navarro. If one role is listed on more than one channel,
   the channel with the lowest ID wins.

   **When an inherited spawn channel loses.** An agent spawned with `gb spawn` and no
   `--slack-spawn-channel` inherits its spawner's channel. gb copies
   `SLACK_SPAWN_CHANNEL`, or else the spawner bead's `slack_spawn_channel`,
   `schedule_slack_channel` or `slack_thread_channel`, and records the spawner's role
   in `slack_spawn_channel_inherited_from_role`. What happens next depends on the
   spawner's role:
   - **Same role as the child** (crew spawning crew): the inherited channel works as a
     pin at rule 2.
   - **Different role** (a gardener spawning crew): rule 3 runs first, so the child's
     own role channel wins. The inherited channel is used only when the child's role
     has no role channel, as rule 3b (`inherited_spawn_channel`), which still beats the
     project's primary channel.

   A human running `gb spawn` outside a pod has no `BOAT_ROLE`. Their
   `SLACK_SPAWN_CHANNEL` is written as an explicit pin, not as an inherited channel.

4. **Project primary channel** (`project_primary`). This is the first channel in the
   project bead's `slack_channel` list. Example: with `slack_channel: C0AAA,C0BBB`, a
   card that no earlier rule placed goes to `C0AAA`.

5. **Bridge default** (`router_default`, then `bot_default`). This is the router's
   `default_channel` if a router is set. Otherwise it is the bridge's own channel
   (helm `slackBridge.slack.channel`, env `SLACK_CHANNEL`). An agent ends up here only
   when it has no project, or its project has no `slack_channel`. Decision and
   escalation cards that land here log the warning
   `agent has no project; posting card to the default channel`.

The bridge caches project channels for 30 seconds, so a change to `slack_channel` or
`channel_roles` reaches new posts within about 30 seconds.

## Threads and cards

The rules above choose a channel. Two things can put the card or a reply somewhere
else.

- **Thread-bound agents.** An agent spawned from a Slack thread, or from a task that
  carries `slack_thread_channel` and `slack_thread_ts`, is thread-bound. It gets no
  card at the top of the channel. When thread cards are on (`SLACK_THREAD_CARDS`,
  chart default `all`), its card is posted as a reply in that thread. When they are
  off, its status shows on the message that started the thread.
- **A posted card stays where it was posted.** The bridge remembers each card's
  channel and timestamp. If routing changes after the card is posted, the card is not
  moved; replies keep going under the old card. Agents spawned after the change follow
  the new route.

## Where decisions, escalations, crash and death notices go

They go to the agent, wherever its card or thread is:

| Post | Where it goes |
|---|---|
| Decision (`gb decision create`) | A thread-bound agent's decision goes in its thread. Otherwise it goes under the agent's card, in the channel the rules picked, and the bridge posts the card first if the agent has none. When the decision is addressed to a person, the card with the buttons goes to that person's DM, and the thread gets a one-line pointer. |
| Escalation (`gb escalation create`), its result and its pingback | A thread-bound requester's escalation goes in its thread. Otherwise it goes under the requesting agent's card. |
| Crash notice, death notice | These go in the agent's thread, or under its existing card, which wins over the current route. If there is no card, the bridge posts one in the routed channel first. |
| Death storm roll-up | This goes to the bridge's default channel. When many agents die in a short window, the bridge stops posting one death notice each and posts a single summary line instead. |

Before a decision or escalation card is posted, the bridge reloads the agent's routing
from its agent bead if it has none cached, for example after the bridge restarts. A
card therefore no longer falls through to the default channel just because the bridge
missed the spawn.

## Check a route: `gb slack route`

`gb slack route <agent|bead>` asks the bridge (`GET /api/slack/route`) which rule wins
for an agent, and shows the candidates that lost. If you pass a bead, the command finds
its agent: the agent named on an agent bead, the assignee of a decision, the agent that
filed an escalation, or the assignee of any other bead. Only `SLACK_BRIDGE_URL` is
needed.

```text
$ gb slack route gardenio-fix-login-abc
Agent:    gardenio-fix-login-abc
Channel:  #gardenio-crew (C0CREW00001)
Rule:     role_channel — channel_roles maps role crew in project gardenio
Inputs:   project=gardenio role=crew spawn_channel=C0PLANS0001 inherited_from_role=gardener
Thread:   no
Card:     #gardenio-crew (C0CREW00001) ts 1793242141.366079

Candidates (highest precedence first):
  -     router                   no router configured
  -     spawn_channel            spawn channel C0PLANS0001 was inherited from a gardener spawner; it ranks after the role channel
  won   role_channel             #gardenio-crew (C0CREW00001)  channel_roles maps role crew in project gardenio
  lost  inherited_spawn_channel  #gardenio-plans (C0PLANS0001)  inherited from a gardener spawner
  lost  project_primary          #gardenio (C0MAIN00001)  first slack_channel of project gardenio
  -     router_default           no router default channel
  lost  bot_default              #gardenio-bridge (C0BRIDGE001)  bridge default channel
```

This example is a crew agent spawned by a plan-gardener whose schedule channel is
#gardenio-plans; the channel names and IDs are made up. `Card:` shows where the
card really is. If that differs from `Channel:`, the command says so: the card was
posted before routing changed, or the agent is thread-bound. Add `--json` for the raw
response.

## Change a route

| You want | Do this |
|---|---|
| All of a role's cards in one channel | `gb project channel add #my-channel --role <role>` (rule 3) |
| A project's catch-all channel | `gb project channel add #my-channel --primary` (rule 4) |
| See or undo the wiring | `gb project channel list`, `gb project channel rm <channel>` |
| One agent in a given channel | `gb spawn ... --slack-spawn-channel #my-channel` (rule 2) |
| A scheduled agent's cards in a channel | Set the trigger's or plan's Slack channel; it is stamped as `schedule_slack_channel` (rule 2) |

`gb project channel add` takes a channel name or ID and refuses a channel that another
project already claims. It warns when the role has no `type:role` bead. It rewrites
`slack_channel`, `channel_roles` and `channel_modes` together in one bead update and
keeps every other channel on the project. Do not edit those fields with a bare
`kd update -f`: it replaces the whole list. See
[Step 4b of the onboarding guide](onboarding-new-project-and-role.md#step-4b--optional-wire-a-slack-channel-to-a-role-as-its-default-handler).

After a change, wait about 30 seconds for the cache, spawn a fresh agent and run
`gb slack route` on it. Agents that already have a card keep it where it is.

## The Lands Commons places its own copies

The Lands Commons (`/garden`) does not use the rules above. Each card the bridge
routes also gets a copy in the Commons, and `conversation.Place`
(`gardenio/controller/internal/conversation/placement.go`) picks where that copy goes
from the land's `type=channel` beads. A subject bound to a thread goes in that
thread. Otherwise it goes to the channel bead whose `channel_roles` lists the role.
Otherwise it goes to the land's default channel: `#machines` for agents when the land
has one (else `#crew`), `#decisions` for decisions, `#escalations` for escalations.
A crash or death notice that replies under an agent's card stays in the card's channel;
a top-level one goes to the land's `#machines` when it has one. A land with no channel
beads has no Commons copy. The Slack copy always follows this page's rules, so the
Commons and Slack can put the same card in different places. Use `gb channel` for
Commons channel beads and `gb project channel` for Slack wiring.
