# Lands: the Commons (design spec)

Plan: gd-vmw5s07A1F (Gardenio: Lands). Task: gd-dAsgFTJ2Se (phase 0a).
Inputs: plan narrative draft 3 (gd-NcaFgzREzt), product definition (gd-jXfrmBe6Yy), Slack
parity inventory (gd-RPic1giSw5), the old agent-chat spec
(`docs/superpowers/specs/2026-09-20-agent-chat-design.md`), and the bridge code as of
`main` 5afbdddf.

Paths are relative to `gardenio/controller/` unless they start with `gardenio/`, `kbeads/` or
`docs/`. "Bridge" means `internal/bridge`.

## 0. Summary

The Commons is the Slack replacement inside `/garden`: channels, threads, live cards,
decisions, DMs and, later, presence, with people and agents as peers. This spec decides how it
is built:

1. **Beads become the source of truth for conversation, and Slack becomes a projection.**
   Today a Slack message is primary and a bead is sometimes written beside it. After the split,
   every durable thing said in a land is a bead first (`channel`, `message`, thread head, card),
   and each surface renders it.
2. **The bridge splits into a conversation core and surfaces.** Outbound, the core calls one
   `Surface` interface (post, update in place, reply in thread, delete, react, open modal, DM,
   ephemeral). Slack is implementation #1: the existing `bot_*.go` Block Kit code moved behind
   it, unchanged. The Commons is #2: it renders from beads over a per-viewer SSE stream, so its
   outbound side is thin by construction. Inbound, each surface turns its own events into one
   `Inbound` value, and one dispatcher runs the handlers that exist today.
3. **Agents get one verb, `gb say`.** `gb squawk`, `gb slack reply` and `gb dm ask` become
   aliases. Where a message shows up is routing, not the agent's problem.
4. **The migration is sliced** so each step moves one group of handlers behind the interface
   and keeps the slackmock e2e tests and the bridge unit tests green (section 5).

## 1. Decisions

Decisions kept from the old agent-chat spec, dropped from it, and taken here. Say so if one
is wrong.

| # | Decision | Status |
|---|---|---|
| D1 | One live card per bead. A card rewrites itself in place as its subject bead moves; never one card per event. | kept |
| D6 | Write path = a closed `type=message` bead attributed to the human, plus a nudge to the bound agent. | kept |
| D8 | Live updates are invalidations: events carry ids only, the client refetches the server-rendered fragment. A version poll is the floor. One render path. | kept |
| D12 | Every non-toggle button opens a server-rendered lightbox with the URL mirrored, never a navigation. This is the Commons' "modal". | kept |
| D3 | Top-level unit is the agent. | **dropped**: the top-level unit is the channel |
| D5 | Two-pane agent rail. | **dropped**: channel list + timeline + thread drawer |
| D10 | Separate route beside `/`. | **dropped**: the Commons lives inside `/garden` |
| D11 | Task as card title. | **dropped** as a rule for all cards; the agent card still leads with the task (section 2.5) |
| C1 | Conversation lives in the one kbeads daemon as beads. No new store. | new |
| C2 | A **land** is a project in v1 (1:1). Land isolation is kbeads' existing project scope. | new |
| C3 | The **conversation id** is the thread head's bead id. Existing wish heads (`type=task`, label `thread`) are reused, not replaced by a new type. This answers product-definition question 3. | new |
| C4 | A **card** is a thin `type=message` bead (`kind=card`, `subject_id`) rendered from its subject. It gives the card a stable place in a timeline and a home for per-surface refs. | new |
| C5 | Durable output is always a bead write; the surfaces project it. Only ephemerals and modals go straight to a surface, and only as the answer to an inbound event from that surface. | new |
| C6 | Commons writes happen on the signed-in human's own bearer, so kbeads enforces scope natively; the bridge only does what needs the fleet (nudge, spawn, kill), after an action-table check. | new |
| C7 | Slack mirroring is per channel and both ways during the transition; cut-over is per land, never a flag day. | new (from the narrative) |
| C8 | Card themes (Garden/Business) stay a Slack-surface concern. The Commons renders one theme, light and dark. | new (inventory says DROP) |

## 2. The conversation model

Everything here is a bead in the one daemon. Every conversation bead carries **exactly one**
`project:<land>` label (section 6.2 explains why this is the isolation wall).

### 2.1 Land

A land is a project bead. No new type. The project bead gains:

| Field | Meaning |
|---|---|
| `land_default_channel` | channel bead id the land opens on (normally `#general`) |
| `land_slack_mirror` | `on` / `off`: whether new channels in this land default to mirroring |

Both are admin-written like every project field. `gb channel seed` sets each one that is unset:
`land_default_channel` to the land's `#general`, `land_slack_mirror` to `on` when the project has
a Slack channel, else `off`. A reader that finds `land_default_channel` unset opens the land's
`#general` by name.

Membership is the existing project-scoped-humans model (gd-507PTO0036): realm group
`/project-<name>`, project member role. Land roles (owner, member, guest) arrive in Commons III
(section 7.3).

**Operator's land.** The operator is an admin principal and sees every land. "The operator's
land" in the plan means the `gardenio` project and the other projects the operator runs today;
each is its own land with its own channels, which is how Slack is already used (one channel set
per project via `channel_roles`).

### 2.2 Channel (`type=channel`)

kbeads accepts custom types already (`kbeads/internal/model/bead.go`: "custom types (e.g.
"message", "gate", "workflow") are valid"). Phase 2 registers `channel` with field validation.

| Field | Required | Meaning |
|---|---|---|
| `name` | yes | slug, unique per land (`general`, `crew`, `decisions`, `escalations`, user names) |
| `channel_kind` | yes | `general` / `crew` / `decisions` / `escalations` / `user` / `dm` |
| `channel_roles` | no | roles whose agent cards land here; replaces the per-project `channel_roles` map entry for this channel (`bot_agents.go:3199`, `:3336`) |
| `default_role` | no | role a plain `@gardenio` mention spawns here (today's per-channel default role) |
| `runtime_class` | no | today's `channel_runtime_class` (`bot_commands.go:949,965`) |
| `card_theme` | no | Slack surface only (C8) |
| `slack_channel_id` | no | the mirrored Slack channel; unique across all channel beads |
| `mirror` | no | `off` / `out` (Commons → Slack) / `both`; unset is `off` (gd-t3s76tjnz9) |
| `members` | `user`, `dm` | who may post: `oidc:<email>` or `agent:<name>`. Required on a DM (two or more, fixed at create); on a user channel it changes as people join and leave (2.2.1). Not allowed on the default channels |

Status `open` is a live channel; `closed` is archived (Slack `archive` maps onto close).
Title is the display name; description is the channel topic.

**Default channels.** Provisioning a land (phase 8, and by hand for the operator's lands in
Commons I) creates `#general`, `#crew`, `#decisions`, `#escalations`. For an existing project,
the seed binds each channel bead to the Slack channel the project already routes to, read from
the project's `channel_roles` and the bridge's channel config, with `mirror=both`.
`slack_channel_id` is unique across channel beads, so a Slack channel binds to one channel bead:
when several default channels route to the same Slack channel (today crew cards, decisions and
escalations all post to the project's primary channel), the first of `#general`, `#crew`,
`#decisions`, `#escalations` takes the binding and the rest start unbound (`mirror=off`). The
seed is `gb channel seed` (dry run by default, `--apply` to write; existing channels are never
modified).

#### 2.2.1 Channel membership (Commons III step 8)

A user channel (`channel_kind=user`) and a DM are **members-only**: only the principals in
`members` post (say or reply) in them. Everyone in the land still reads them: a channel hidden
from non-members is a private channel, which stays LATER. The default channels (`general`,
`crew`, `decisions`, `escalations`) carry no `members` and take posts from everyone in the land.

| Who | Post in a members-only channel | Change a user channel's members |
|---|---|---|
| operator (admin) | always | anyone |
| owner | when a member | add or remove anyone |
| member | when a member | join or leave itself (`channel_join`) |
| guest | when a member (an owner adds it) | leave only |

A DM's members never change after create. Reactions are not held to membership. Agents post
through the fleet, where routing puts them, and are not checked. Whoever creates a user channel
with `gb channel create` is its first member. kbeads enforces this on the person's own bearer
(`kbeads/internal/server/channel_membership.go`): a person's message whose `channel_id` names a
members-only channel without them is a 403, and a person changes a user channel only with a
patch that touches nothing but `members`, checked against the table above. The controller's
twin is `conversation.ChannelAllows` and `conversation.MembershipChangeAllows`
(`internal/conversation/membership.go`); a Commons write route calls
`viewerScope.mayInChannel` before it writes. The CLI is `gb channel join|leave|members`. Who
may create or archive a channel is still the operator's until `channel_manage` gets routes. A
land's people are its owner's (`member_manage`, §6.4; P10.C gd-wi44L8Qy4I): the owner invites
and revokes from the People lightbox (Track B §3.7), and a change to the land's own groups is an
escalation the owner approves on its page, never the operator's.

### 2.3 Thread and message

A **message** is a closed `type=message` bead, as squawks and forwarded replies are today.

| Field | Meaning |
|---|---|
| `channel_id` | the channel bead it was said in (new; every message, thread head and card carries it) |
| `thread_id` | the thread head's bead id, when the message is a reply (existing invariant, `thread_bead.go`) |
| `author` | `oidc:<email>` for a human, `agent:<name>` for an agent (today: `operated_by` / actor) |
| `kind` | `text` (default) / `card` / `notice` / `ask` |
| `subject_id` | `kind=card` only: the bead the card renders (agent, decision, escalation, permission) |
| `to` | DM target, when sent with `gb say --to` |
| `origin` | `slack` / `commons` / `cli`: where it was first said (mirror loop guard, section 4.4) |
| `slack_ts`, `slack_channel` | the Slack ref once projected (replaces `StateData` refs over time) |
| `reactions` | map of reaction → list of principals (Commons III; Slack stays authoritative for its own) |

**Who stamps a message's place.** `channel_id` and `thread_id` (and the channel's
`project:<land>` label) land when the message bead is created, written by whoever creates it. The
bridge never patches them onto a message an agent created: its fleet token cannot update another
principal's message, and widening that right would undo the write authz of the gd-74wnmpQgyT
series. So `gb squawk` and a plain `gb say` stamp them at create time (`cmd/gb/channel_stamp.go`):
a thread-bound agent's squawk gets the `slack-thread:` head and the channel bead bound to its Slack
channel; any other agent's squawk is a reply in its agent card's thread, so it gets the card bead
as `thread_id` with the card's `channel_id` and land (gd-fIM0zNikOd). The stamp is best-effort:
a refused stamped create is retried unstamped, and a squawk that comes before its card exists, or
whose card is in an unbound Slack channel, stays out of the Commons. The `slack_ts` /
`slack_channel` write-back on an agent's message is refused for the same reason and stays
best-effort.

A **thread** is a root plus every bead whose `thread_id` points at it. The root is either:

- a **wish head** (`type=task`, label `thread`, plus `slack-thread:<c>:<ts>` when it came from
  Slack) when the thread carries work: it was a mention, it spawned an agent, or a human
  marked it a wish. This is exactly today's head, minted by `thread_bead.go`; or
- a plain **message** bead for chat that never became work.

Membership stays `UNION(fields.thread_id == headID, parent-child closure)`, as the viewer
already resolves it (`cmd/beads-viewer/threads.go`). The head carries no `thread_id`, as today.
A chat root becomes a wish when someone `@`-mentions an agent in its thread: the bridge mints a
wish head with the root's text, re-points the thread (`thread_id` of the root and its replies
moves to the head; the root gets `promoted_to`), and the head inherits `channel_id`. The same
promote path is what the `slack-thread` resolve-or-create lookup does today for a reply in an
unbound Slack thread.

**Agent binding** moves from the bridge state file to the bead. Today `StateData.ThreadAgents`
is keyed `"{channel}:{thread_ts}"` (`state.go:125`). The agent bead already carries `thread_id`
(and the legacy alias `slack_thread_bead_id`); the bound agent of a thread is "the newest agent
bead whose `thread_id` is the head". `ThreadAgents` becomes a cache keyed by head id, with the
Slack tuple as an alias, and the respawn, DLQ, validation and 24h sweep paths key on the head
id (section 5, slice 4d).

### 2.4 DMs

A DM is a channel with `channel_kind=dm` and `members`. It belongs to one land, so a person in
two lands has separate DMs per land and nothing crosses the wall. The operator, as admin, reads
every land's DMs addressed to them; escalation DMs from a tenant land live in that land.

- Human ↔ agent: `gb say --to <person>` and `gb dm ask` write into the DM channel between the
  agent and that person, creating it on first use.
- Decision and escalation "DM-first" delivery (`bot_decisions_dm.go`, `escalation_operator_dm.go`)
  targets the addressee's DM channel in the Commons and the Slack IM, with the channel pointer
  posted where the card lives.

  For an escalation, #756's `audience` says whether it is a person's at all (only `owner` is),
  and its `held_by` lane says whose, mapped by `conversation.EscalationAddressees`:
  - `operator` → the operator seat;
  - `land-owner:<land>` → each owner of that land;
  - `steward:<land>`, `coordinator:<name>`, `requester` and `server` are agents or the daemon,
    so no person gets a DM; the card stays in `#escalations`.

  The DM is never addressed by `operated_by`. When `held_by` changes (approve, hand-up), the new
  addressee gets the DM card and the old DM card re-renders as "now waiting on …".
- Group DMs (`bot_group_open.go`, `bot_group_invite.go`) are a DM channel with more members.
  LATER for the Commons.

### 2.5 Card

A card is a `type=message` bead with `kind=card` and `subject_id`. It is unique per
`(subject_id, channel_id)`, and it is never re-posted: every change to the subject re-renders
it in place (D1). Card faces:

| Subject | Face | Built from today |
|---|---|---|
| agent bead | live face: state glyph, project/role, task title first, cost, context size, spawn vector, owner, Glass link, buttons Clear/Kill/Restart; plus the live stream line | `bot_agents.go:2409,2715,2858`, `agentstream.go:378` |
| agent bead (closed) | wrap-up face: tldr, accomplishments, blockers, handoff | `bot_agents_wrapup.go`, `bot_agents.go:1955,2130` |
| decision | priority, prompt, options as buttons, Other (lightbox), Dismiss, Promote; resolved face | `bot_decisions.go:54`, `bot_decisions_modal.go` |
| escalation | operation, creds summary, **"Waiting on <lane>"** (from `held_by`), approve/reject only when `can-approve` allows the viewer, result, pingback | `bot_escalations.go:166,385,443` |
| permission request | tool, input summary, Allow/Deny | `bot_permission.go` |
| death | cause, storm roll-up count | `bot_agent_death.go:50,103` |

**Placement** (one function, shared by both surfaces, replacing the routing in
`bot_agents.go:3199-3302`):

1. if the subject is bound to a thread (`thread_id`), the card goes in that thread's channel,
   as a reply in that thread;
2. else the land's channel whose `channel_roles` contains the agent's role;
3. else the land's `#crew` (agents), `#decisions` (decisions), `#escalations` (escalations).

Decisions and escalations get a second card in the addressee's DM channel when the addressee is
a person (DM-first); the two cards are two `kind=card` beads with the same subject, so the
existing "mirrors kept in sync" problem (`decision_message_mirrors.go`) becomes "re-render every
card whose `subject_id` changed", which is the rule anyway. For escalations, "the addressee" is
defined in §2.4 by `held_by`.

### 2.6 What is not a bead

- **Ephemerals** (130 `PostEphemeral` sites, mostly slash-command answers and refusals): never
  beads. Answered synchronously to the surface the event came from (C5).
- **Presence and typing** (Commons III): in-memory SSE state in the Commons server, from viewer
  connections and the kbeads presence tracker (`kbeads/internal/presence`) for agents.
- **Read cursors** (Commons III): one debounced field map on the user's own bead
  (`read_cursors`: channel id → last seen message id), at most one write per 10 s per user.
- **Fleet telemetry notices** (jacks, account rotation, release drought, SCIF alerts): stay
  Slack-only posts through the Slack surface in v1. LATER: an `#ops` channel in the operator's
  land, at which point they become `kind=notice` messages.

### 2.7 Scale check

The daemon carries every Slack message already. The new writes are: one channel bead per
channel (tens per land), one card bead per (subject, channel) (one or two per agent and per
decision), and nothing per card update. The narrative's revisit threshold (tens of humans and
hundreds of agents per land) stands.

## 3. The surface interface

New package `internal/conversation`. The bridge depends on it; it depends on `beadsapi` only.

### 3.1 Outbound

```go
// Ref names one rendered copy of a message on one surface.
type Ref struct {
	Surface string // "slack" | "commons"
	Channel string // surface channel id (Slack C…/D…; Commons channel bead id)
	TS      string // surface message id (Slack ts; Commons message bead id)
}

// Message is what the core asks a surface to show. Cards carry only the
// subject; each surface renders the subject bead itself (D1), so the Block Kit
// builders stay inside the Slack surface.
type Message struct {
	BeadID    string   // the message/card bead this is a copy of ("" for unrecorded notices)
	Kind      Kind     // Text | Card | Notice | Ask
	SubjectID string   // Card only
	Text      string   // markdown; also the fallback/notification text (rubric U12)
	Author    Principal
	Files     []File
}

type Surface interface {
	Name() string
	Post(ctx context.Context, ch ChannelRef, m Message) (Ref, error)
	Reply(ctx context.Context, thread Ref, m Message, broadcast bool) (Ref, error)
	Update(ctx context.Context, at Ref, m Message) error
	Delete(ctx context.Context, at Ref) error
	React(ctx context.Context, at Ref, reaction string, on bool) error
	DM(ctx context.Context, to Principal, m Message) (Ref, error)
	// Ephemeral and OpenModal answer an Inbound from this surface; Trigger
	// carries what the surface needs (Slack trigger_id / response channel,
	// Commons request id).
	Ephemeral(ctx context.Context, t Trigger, to Principal, m Message) error
	OpenModal(ctx context.Context, t Trigger, v Modal) error
}
```

`Modal` is a small neutral form model (title, text inputs, selects, hidden values, submit id),
enough for the four modals that exist: decision Other, decision Promote, manual survey,
feedback (`bot_decisions_modal.go`, `bot_human_survey_manual.go`). The Slack surface builds the
`slack.ModalViewRequest` from it; the Commons returns it as a lightbox fragment (D12).

**The router** (`conversation.Router`) is what handlers call. It:

1. writes or upserts the bead (message, card) with `channel_id`, `thread_id`, `project:` label;
2. looks up the surfaces bound to the channel: always `commons`; `slack` when the channel has
   `slack_channel_id` and `mirror` is `out` or `both` (unset is `off`), or when the land has no
   Commons yet;
3. calls each surface except the message's `origin` surface for inbound-originated messages
   (loop guard), and stores each returned `Ref` on the bead (`slack_ts`, `slack_channel`);
4. on `Update(subject)`, finds every card bead with that `subject_id` and updates each ref.

A surface failure never fails the bead write: the bead is the record, and a failed projection
is retried from the bead by the same DLQ the bridge already has (`state.go:42`). This is
"degrade, never drop" (`bot_escalations.go:166`) generalised.

### 3.2 Slack: implementation #1

`slackSurface` wraps the `*slack.Client` the bridge already builds (`bot.go:501`) and is the
only code that calls `PostMessage`, `UpdateMessage`, `DeleteMessage`, `AddReaction`,
`RemoveReaction`, `OpenView`, `OpenConversation` and `PostEphemeral` once the migration ends.
Its `Post`/`Update` for `Kind=Card` call the existing builders (`theme.go`, the
`build*Blocks` functions in `bot_agents.go`, `bot_decisions.go`, `bot_escalations.go`) with the
subject bead and the channel's `card_theme`. Nothing about how a card looks in Slack changes;
the Block Kit code moves file, not shape.

Slack-only machinery stays inside the Slack surface and its adapter: thread status
(`ThreadStatusSetter`), chat streaming (`agentstream.go` `StreamClient`), canvases
(`canvas.go`), pins (`dashboard.go`), unfurls (`link_unfurl.go`), the assistant pane
(`bot_assistant.go`).

The existing narrow interfaces are the seams this generalises, and they keep compiling during
the migration: `Notifier` (`decisions.go`), `AgentNotifier` (`agents.go`),
`EscalationNotifier` (`bot_escalations.go`), `ThreadMessagePoster` / `AgentCardRefresher`
(`agentstream.go`), `EscalationDMSender`, `CrossOperatorSender`. Each is today implemented by
`*Bot` straight against Slack; each becomes implemented by core code that calls the `Router`.

### 3.3 Commons: implementation #2

The Commons reads beads. Its server lives in beads-viewer (which already serves `/garden`), in
a new `internal/commons` package beside `internal/gardenhub`. It renders channel timelines,
threads and card faces from beads on the viewer's own bearer and pushes id-only invalidations
over SSE (D8): `message <id>`, `card <id>`, `channel <id>`, `presence <channel>`.

**The read model as built (Commons I step 8, gd-LCtVzVbuWM).** `internal/commons` serves the
read routes of section 6.3 as server-rendered HTML fragments (`cm-*` classes and `data-id`,
`data-channel`, `data-thread`, `data-land` hooks, pinned by `internal/commons/testdata/*.golden.html`).
`{channel}` in a URL is the channel's `name` or its bead id. A timeline is a channel's newest 400
beads with that `channel_id`. It shows the top-level ones (messages, cards and wish heads, oldest
first, at most 60), and it counts each root's replies from the same window. A thread is the root
plus the `type=message` beads whose `thread_id` names it. Thread tasks and agents are left out.
A bead with no `project:` label, or with two, is never rendered. The event stream
`GET /garden/c/events` sends `event: message|card|channel` with
`data: {"id","channel","thread","land"}`. It sends `ping` every 15 s, and `replayed` after a
replay. Each event's `id:` is a time. A reconnect's `Last-Event-ID`, or `?since=<RFC3339>`, replays
from the durable event log on the viewer's own bearer, one event per bead. The replay goes back at
most 1 h and covers at most 200 beads. Past either limit it sends `reset`, which means the client
must refetch everything on screen. The live half rides the feed hub's single upstream connection.
Each event's land is read from the daemon as the system, never from the payload, and the cut is
per subscriber. A subscriber whose scope does not admit the land never receives the id.

**Presence as built (Commons III step 4.3, gd-SFaw19TOiP).** `internal/commons/presence.go` holds
who is where, in memory. A person is present while an event stream that names
`?land=<land>&channel=<name or id>` stays open (the channel is resolved on the viewer's own
bearer, so a members-only channel admits only its members; a land outside the scope records
nothing), and for 20 s after it closes, so the reconnect at bearer expiry does not flicker. An
agent is present while the kbeads presence tracker (`GET /v1/agents/roster`) has heard from it in
the last 10 min: its land is its agent bead's project label, and its channel is where its card is
placed (`conversation.Place`, section 2.5); the roster is re-read every 15 s as the system. A change
sends `event: presence` with `data: {"id","channel","land"}` (`id` and `channel` empty when only
the land changed) through the hub, cut by land and never replayed. The client refetches
`GET /garden/c/{land}/{channel}/presence`: the channel's people, then agents, as avatars (at most
12, then `+N`), and a count of the rest of the land, never their names or channels. Typing (step
4.4) reuses this state (`Presence.Present`, `Hub.Publish`). The pane (`gardenhub/web/commons.js`) opens its one stream on the channel on screen;
showing another channel reopens it there with `?since=` its last event id (a `Last-Event-ID`
header wins over `?since=`, so the browser's own reconnect replays from the newer point), and
folding the pane keeps only `?land=`. It fills the header's `[data-slot=presence]` from the
fragment and refetches it on `event: presence` for that channel or its land. Each avatar carries
the name (and "(agent)") as screen-reader text and as its `title`.

**Typing as built (Commons III step 4.4, gd-3t9c8kGYVl).** `internal/commons/typing.go` keeps who is
typing on `Presence`, in memory, beside who is here; it is never a bead and never replayed. A
composer sends `POST /garden/c/{land}/{channel}/typing` (form `thread=<root id>` in a thread, and
`stop=1` when it is emptied) at most every 3 s while its text changes. The route is `routeScoped`
and gated like say and reply (the land-role action table and channel membership), and it admits a
signal only from a viewer holding an event stream on that channel (`Presence.Present`, else 409),
so typing is tied to a live connection. It is rate-limited per principal, which covers all of its
connections. A principal has two composers on a channel, as the pane does: the channel's and one
thread's (the drawer). Each lets one signal through per 2 s, however many tabs, and a faster one answers
429 with `Retry-After`. The thread composer's limit is the same whichever thread a signal names, and a
signal in another thread moves the thread line there (a stop for the old line, a start for the new);
were the limit keyed on the thread id, a fresh id per request would get through every time and flood
every subscriber's buffer in the land with typing events. One
signal keeps its sender typing for 6 s; sending the message (say, reply) or `stop=1` ends it at
once, and stopping does not reset the rate limit. Only a start or a stop is news: it sends
`event: typing` with `data: {"id","channel","thread","land"}` (`thread` absent for the channel's
own composer) through the hub, cut by land. The client refetches
`GET /garden/c/{land}/{channel}/typing[?thread=]`, a one-line fragment that leaves out the viewer:
"ann is typing…", "ann and bo are typing…", up to three names, then "Several people are typing…";
each name is cut at 20 characters, so a long one never pushes "are typing" out of the line.
The pane puts the line's text into the timeline's or the drawer's `[data-slot=typing]`, a polite
`aria-live` line between the messages and the composer (two lines at most in a narrow pane) that
folds away when empty and does not animate, and refetches both lines after a replay, and a line still
showing 7 s after it was fetched (6 s plus a margin), in case a stop was missed. Agents do not type
(LATER, if `gb say` ever streams).

**Mentions of people as built (Commons III step 6, gd-4fmDZb5C52).** A land's people are the
`type:user` beads whose `allowed_projects` names it (the UserProvisioner writes it from the realm
groups at sign-in, so owners, members and guests); a wildcard grant is not membership. kbeads answers
`GET /v1/lands/{land}/members` (`kbeads/internal/server/notification.go`) only to a caller whose
scope admits the land, and 404 for any other. The composer's @ list
(`GET /garden/c/{land}/{channel}/mentions`) offers them, on the viewer's bearer, to anyone who may post
there, guests too: `@gardenio`, then the people but the viewer, then the agents. A person's handle is
the email's local part in any script, or the whole email with a dot for its `@` when two people share
one. A say or reply that @-mentions people writes one `type=notification` bead per person
(`to`, `reason=mention`, `message_id`, `channel_id`, `thread_id`), on the author's bearer, in the
message's land, never to the author. kbeads refuses one that is not to a member of that land, not
about a message of that land, or not about the caller's own words; nobody scoped edits one, and none
moves between lands. Owners, members and guests may create one (the action table). Only a message
with an ASCII `@name` goes to the bridge, which reads no other. Delivery is step 7's.

Because the router has already written the bead, the bridge-side `commonsSurface` is thin:

| Method | Commons behaviour |
|---|---|
| `Post`, `Reply`, `DM` | no-op beyond the bead the router wrote; returns `Ref{Surface:"commons", Channel: channel_id, TS: bead id}`. The Commons SSE hub sees the bead event and invalidates. |
| `Update` | no-op: a card re-renders when its subject bead changes; a text edit is a bead update. |
| `Delete` | closes the message bead with `deleted=true`; the timeline renders a tombstone. |
| `React` | updates the message bead's `reactions` field. |
| `Ephemeral`, `OpenModal` | returned in the HTTP response to the inbound call (section 3.4) and rendered as a toast or lightbox for that viewer only. |

That thinness is the point of C5: a second surface costs a reader, not a second copy of sixty
handlers.

### 3.4 Inbound

Every surface turns its events into one value:

```go
type Inbound struct {
	Kind     InboundKind // Compose | Reply | Action | Submit | React | Join
	Surface  string
	Land     string      // project
	Channel  string      // channel bead id
	Thread   string      // thread head bead id ("" for a top-level post)
	Message  string      // the message bead id the human's words were written to
	Actor    Principal   // oidc:<email> (Slack: resolved through the account link)
	Text     string
	Files    []File
	ActionID string      // Action/Submit: today's Slack action_id / callback_id
	Value    string
	Values   map[string]string // Submit
	Reaction string
	Trigger  Trigger
}
```

- **Slack adapter.** `handleEventsAPI`, `handleInteraction` and `handleSlashCommand`
  (`bot.go:1094`, `:1602`) keep receiving Socket Mode events. The adapter resolves the Slack
  channel to a channel bead (`slack_channel_id`), `(channel, thread_ts)` to a thread head (the
  `slack-thread:` label lookup that exists), the Slack user to `oidc:<email>` (the account link,
  `slack_link.go`), writes the human's words as a message bead when they are a compose or reply,
  and calls the dispatcher.
- **Commons adapter.** The Commons writes the human's message bead itself on the human's bearer
  (C6), as the `/threads` composer already does with its `thread-say` bead
  (`thread_message_forward.go`), then calls the bridge at `POST /api/conversation/inbound` with the `Inbound` and the bead
  id. The call carries the edge-auth principal; the bridge authorizes the action against the
  action table (section 6.3) before it does anything as the fleet. The response carries any
  ephemeral or modal. If the bridge is down, the bead is still written and a watcher sweep
  (message beads with `origin=commons` and no `handled_at`) picks it up, the same way the
  thread-reply DLQ works today.
- **Dispatcher.** `conversation.Dispatch(Inbound)` runs the handler for the kind and
  `ActionID`. The handlers are today's, re-signed from `slackevents.*` to `Inbound`.

Slash commands are not ported (section 5, slice 6): they stay Slack-adapter-only and use
`Surface.Ephemeral` for their answers, so the Slack client still has one call path.

## 4. Agents: `gb say`

### 4.1 The verb

```
gb say "text"                         # the agent's home thread (today's squawk target)
gb say --thread <head-id> "text"      # a specific thread
gb say --channel <name|id> "text"     # a new top-level post in a channel of the agent's land
gb say --to <person|agent> "text"     # a DM
gb say --to <person> --ask "q?"       # a DM question; the reply is captured and nudges the agent
gb say --file path [...]              # attach files (today: gb slack upload)
     [--json] [--no-comment]          # squawk parity
```

It writes one closed `type=message` bead with `channel_id` / `thread_id` / `to`, labels `say`
and `project:<land>`, `author=agent:<name>`, and the existing squawk lineage
(`cmd/gb/squawk.go:144`). The bridge's message projector, today's squawk watcher
(`squawk.go`, which already accepts the `say` label), resolves the target and calls the router.
The agent never names a surface.

**Home thread** resolution extends today's (`cmd/gb/squawk.go:197`, `resolveThreadCoords`): the
agent bead's `thread_id` first; else `SLACK_THREAD_CHANNEL`/`SLACK_THREAD_TS` (or, for a
pool-assigned pod whose env is empty, the agent bead's `slack_thread_channel`/`slack_thread_ts`)
mapped to a head through the `slack-thread:` label; else the agent's card thread; else the
land's `#crew`. *As built (gd-fIM0zNikOd):* the card-thread step is the agent's newest top-level
card bead in a Commons channel (`agentCardPlace`), stamped by gb at create time (2.3). The
`#crew` step is not built: with no card, a squawk carries no place.

**As built (slice 5, gd-RFynVHDLXW).** A plain `gb say "x"` is today's squawk bead and
projector, unchanged. `--thread`, `--channel` and `--file` call `POST /api/conversation/say`
(agent bearer, `api_conversation_say.go`), which resolves the target, writes the message bead
through the Router (`origin=cli`, `author=agent:<name>`, triggered-by lineage) and answers with
the bead id and the Slack ref; `gb slack reply` and `gb slack upload` call it with the home
thread's Slack coordinates. Rule 4 of 6.2 applies to targets named by bead (a head or a
channel); Slack coordinates are exempt, since an agent can be bound to a thread in another
land's channel. Files are shared by the Slack surface (files.upload v2, one
`completeUploadExternal` per message); a file share returns no message ts, so it stores no
Slack ref. The `/api/slack/threads*` routes stay for older gb binaries, sharing the reply
deduper; gb falls back to them when the bridge has no `/api/conversation` routes. `--to` and
`--ask` arrived with DMs (7.2 step 8, as built there).

**Reading** gets a twin, `gb thread [<head-id>]`, reading message beads in the thread (and for a
mirrored Slack thread with history older than the mirror, falling back to today's
`/api/slack/threads` fetch). `gb slack thread` becomes its alias.

### 4.2 Aliases

| Old | Becomes | Note |
|---|---|---|
| `gb squawk "x"` | `gb say "x"` | keeps writing the `squawk` label too until every bridge is on the projector |
| `gb slack reply "x"` | `gb say --thread <home> "x"` | `--channel C --ts T` resolves to the head by `slack-thread:C:T` |
| `gb slack upload` | `gb say --file` | |
| `gb dm ask` | `gb say --to <p> --ask` | reply capture moves from `dm_delivery.go` to the DM channel's reply handler |
| `gb slack thread` | `gb thread` | |
| `gb slack channels/channel/search/read-thread/delete` | unchanged, Slack-only | LATER for the Commons |

Aliases are permanent and documented (owner decision gd-FuxIp9SMtV, alias-forever). New docs
and role prompts teach `gb say`. Existing prompts, advice and the prerun preamble keep `gb
squawk` and are not swept, so no prompt has to change for the Commons to work.

## 5. Handler inventory and migration order

### 5.1 Inventory

Every non-test `bot_*.go` file, plus the non-`bot_` files that call Slack. "Calls" counts the
Slack Web API call sites in the file (`grep` over `main` 5afbdddf; 298 in `bot_*.go`, 328
bridge-wide). "Ops" is the interface method each needs. "Slice" is the migration slice in
section 5.2. "Commons" is the verdict for the Commons (section 6.1 has the full list).

**Outbound notifiers (bead event → message):**

| File | What it does | Calls | Ops | Slice | Commons |
|---|---|---|---|---|---|
| `squawk.go` | closed `squawk`/`say` message bead → thread post | via `Bot` | Reply | 1 | MUST (I) |
| `comment_forward.go`, `chat.go` | bead comment / chat-close relayed into the thread | 1 | Reply | 1 | MUST (I) |
| `thread_message_forward.go` | the `/threads` composer's `thread-say` message bead → Slack thread (the viewer holds no Slack token) | 1 | Reply | 1 | MUST (I); it is the Commons write path in miniature |
| `bot_agent_death.go` | death report with storm roll-up | 2 | Post, Reply | 1 | MUST (II) |
| `bot_notifications.go` | crash notice (MUST); jacks, account rotation/throttle (ops) | 12 | Post | 1 | crash MUST (II); rest Slack-only |
| `bot_agents.go` | agent card spawn/state/task, warm card, thread spawn link, state replies, wrap-up blocks | 14 | Post, Reply, Update, Delete | 2 | MUST (II) |
| `bot_agents_wrapup.go`, `bot_agents_usage.go` | wrap-up face; cost/context on the card | 0 | (card render) | 2 | MUST (II) |
| `agentstream.go`, `hookstream.go` | live tool-stream status in the thread | 2 + stream API | Update; Slack-only streaming | 2 | MUST (II), Commons reads coop state |
| `bot_reactions_lifecycle.go` | working/done/failed reactions on the card | 2 | React | 2 | LATER (a state chip replaces it) |
| `bot_beadactivity.go` | bead activity notices, vibe mode | 6 | Post, Update | 2 | LATER |
| `bot_decisions.go` | decision card, update, dismiss, reports, tldr, canvas | 10 | Post, Reply, Update, Delete | 3 | MUST (II); canvas LATER |
| `bot_decisions_dm.go`, `decision_message_mirrors.go` | DM-first copy, mirror sync | 4 | DM, Update | 3 | MUST (II) |
| `bot_decisions_operated_by.go`, `bot_decisions_permission.go` | operator attribution; permissive answer policy | 0 | — | 3 | MUST (II) |
| `bot_escalations.go` | escalation card, approved, result, pingback, queue alert, stale digest | 12 | Post, Reply, Update, Delete | 3 | MUST (II); digest LATER |
| `bot_escalation_operator_dm.go`, `escalation_operator_dm.go` | DM to the addressed operator | 0 (via `SendDM`) | DM | 3 | MUST (II) |
| `bot_permission.go` | tool-permission Allow/Deny card + callback API | 2 | Post, Update | 3 | MUST (II) |
| `bot_dm.go` | `SendDM`, `SendDMBlocks`, `SendGroupDM`, `ReplyInBeadThread` | 6 | DM, Reply | 3 | MUST (II) |
| `bot_cross_operator_notify.go` | notify the other operator's thread | 0 | Reply | 3 | LATER |
| `bot_human_survey.go`, `bot_human_survey_rating.go` | exit-survey prompt and rating buttons | 4 | Post, Update | 6 | LATER |

**Inbound handlers (surface event → action):**

| File | What it does | Calls | Ops | Slice | Commons |
|---|---|---|---|---|---|
| `bot_decisions_modal.go` | block-action router (clear/kill/restart, permission, dismiss, promote, resolve, survey, concierge, codex, autodestruct) and view submissions | 7 | Update, Delete, Ephemeral, OpenModal | 4a | MUST (II) for card actions |
| `bot_agent_kill.go` | clear/kill/restart, respawn-with-resume, freeze/thaw | 21 | Post, Reply, Update, Delete, Ephemeral | 4a | MUST (II) |
| `bot_reopen_debounce.go` | debounced restart | 0 | — | 4a | MUST (II) |
| `bot_reactions_wish_close.go` | ⏹️ closes the thread head | 0 | — (React inbound) | 4b | MUST (II) as a Done button |
| `bot_reactions.go`, `bot_reaction_trigger.go` | reaction-rule beads spawn agents | 2 | Post, Ephemeral | 4b | LATER |
| `bot_thread_forward.go` | thread reply → conversation bead + nudge; resume hint | 2 | Reply | 4c | MUST (I) |
| `bot.go` `handleThreadReply` | thread reply resolves a decision | — | Update | 4c | MUST (II) |
| `bot_threads_dedup.go`, `bot_thread_validate.go`, `bot_message_subtype.go`, `bot_recover.go` | reply dedup, binding validation, subtype filter, panic recovery | 1 | — | 4c | MUST (I) (reliability) |
| `bot_mentions.go` | `@gardenio` spawn, routing, flags, in-thread kill/clear/restart, link/unlink/whoami | 38 | Post, Reply, Ephemeral, React | 4d | MUST (II); `--project/--ultracode/--backend/--fresh/--quiet/--listen` LATER |
| `bot_mentions_model.go`, `bot_mentions_harness.go` | `--model` alias allowlist; per-role harness | 0 | — | 4d | MUST (II) |
| `bot_mention_summarize.go` | `@gardenio summarize` | 2 | Reply | 6 | LATER |
| `bot_person_identity.go`, `bot_user_resolver.go`, `slack_link.go` | account link, whoami, unlinked refusal; Slack user → email | 6 | Post, Ephemeral | 4d | Slack adapter only; the Commons has OIDC |
| `bot_channel_invite.go` | invite bot → bound gardener | 3 | Post | 6 | LATER |
| `bot_concierge.go` | Start/Dismiss on un-mentioned messages | 7 | Post, Update, Delete | 6 | LATER |
| `bot_assistant.go` | App Home / assistant pane | 1 | Post | — | DROP |
| `bot_commands.go`, `bot_gate.go`, `bot_autodestruct.go`, `bot_formula.go`, `bot_trigger_cmd.go`, `bot_schedule_cmd.go`, `bot_unreleased.go`, `bot_codex_login.go` | slash commands | 107 | Ephemeral | 6 | LATER (`gb`/`kd` cover them) |

**Agent-facing HTTP API:**

| File | Routes | Slice | Commons |
|---|---|---|---|
| `bot_threads.go` | `/api/slack/threads`, `/reply`, `/upload`, `/files/` | 5 | MUST (I): `gb say`, `gb thread` |
| `bot_channels.go` | `/api/slack/channels*`, `/messages/delete` | 6 | LATER |
| `bot_channels_manage.go` | `/api/slack/channels/create|archive|unarchive|invite` | 6 | LATER: `gb channel` over channel beads |
| `bot_group_open.go`, `bot_group_invite.go` | `/api/slack/groups/*` | 6 | LATER |
| `bot_agent_feedback.go` | `/api/slack/agent-feedback` | 6 | LATER |
| `api_decisions.go`, `api_escalations.go` | `/api/decisions*`, `/api/escalations*` | — | already surface-neutral |

**Slack-only, not ported:** `dashboard.go` (pinned roster, DROP), `canvas.go` (long reports,
LATER as a page), `link_unfurl.go`, `bare_id_preview.go` (LATER: the Commons renders bead refs
natively), `refusal_log.go`, `model_fallback.go`, `scif_gate_alert.go`,
`scif_release_alert.go`, `coverage_alert.go`, `sla_nudge_notify.go`, `notice.go`,
`slack_operator_resync.go` (ops telemetry; they move to `Surface.Post` in slice 6 only so the
Slack client has one call path).

### 5.2 Migration order

Each slice is one PR (or a short stack). The rule for every slice: no Slack-visible change, the
slackmock e2e tests green (`cmd/slack-bridge/slackmock_loop_e2e_test.go`,
`bot_reopen_debounce_e2e_test.go`, `tests/e2e/scripts/test-slackmock-loop.sh`,
`test-slack-ui.sh`), and the bridge unit tests green, including the 16 test files that fake
Slack with `OptionAPIURL`. Each slice adds a **Slack payload golden** for the handlers it moves:
the recorded slackmock call (method + JSON body) before and after the move must be identical.
That golden is what makes "moved file, not shape" checkable.

| Slice | Moves | Why this order | Lands in |
|---|---|---|---|
| **0. Seam** | `internal/conversation`: `Surface`, `Router`, `Inbound`, `Ref`, `Modal`; `slackSurface` over the existing client; a `surfacetest.Recorder` fake; a contract test run against both the recorder and the slackmock twin. No caller changes. | Nothing can move without it; zero behaviour risk. | Commons I |
| **1. Leaf outbound** | squawk projector (+ `say`), comment/chat-close relay, thread message forward, death report, crash notice. | Append-only posts, no refs to keep, no inbound. The smallest real use of the router. | Commons I |
| **2. Agent cards** | `bot_agents.go` notifiers, wrap-up, usage, stream status `Update`, lifecycle reactions. Card beads (`kind=card`) introduced; `StateData.AgentCards` refs dual-written to card beads, read from beads first, state file second, for one release. | Cards are the highest-traffic update-in-place path; doing them before decisions proves `Update` and refs on the simplest subject. | Commons II |
| **3. Decisions, escalations, permission, DMs** | `bot_decisions*.go` outbound, mirrors, reports (canvas stays Slack-only), `bot_escalations.go`, operator DMs, `bot_permission.go`, `bot_dm.go`. `StateData` decision refs and mirrors → card beads. | Same mechanics as cards plus DM-first; mirrors collapse into "every card of this subject". | Commons II |
| **4a. Inbound: actions and submissions** | `handleBlockActions` and `handleViewSubmission` re-signed to `Inbound{Action,Submit}`; `bot_agent_kill.go` clear/kill/restart; reopen debounce. | Well-delimited, one entry point, already id-keyed (`action_id`). | Commons II |
| **4b. Inbound: reactions** | wish-close as `Inbound{React}`; reaction rules stay Slack-adapter-only. | Small; gives the Done button. | Commons II |
| **4c. Inbound: replies** | `bot_thread_forward.go` and the decision-reply path in `handleThreadReply` become `HandleReply(Inbound)`; dedup/validate/subtype/recover move with it. | Needed for humans to talk to agents in the Commons at all. Ships in Commons I **before** 2–4b, right after slice 1, because replies only need a thread id and a nudge. | Commons I |
| **4d. Inbound: mentions and the binding re-key** | `handleAppMention`, `handleThreadSpawn`, mention flags, in-thread verbs → `HandleCompose(Inbound)`; `ThreadAgents` keyed by head id with the Slack tuple as alias; respawn/DLQ/validation/24h sweep on head id. | The largest file (3121 lines, 38 call sites) and the riskiest re-key; last, with everything else already proven. | Commons II |
| **5. Agent API** | `/api/conversation/say`, `/api/conversation/thread`; `gb say`, `gb thread`; `/api/slack/threads*` kept as thin aliases. | Needs slice 1's projector. | Commons I |
| **6. The rest** | slash commands, channel management, groups, concierge, surveys, summarize, invite-spawn, ops notices: only their Slack calls move behind `slackSurface` (mostly `Ephemeral`). No `Inbound` port. | Keeps one Slack call path; no Commons value yet. | after Commons II, opportunistic |

Actual order of PRs: 0 → 1 → 4c → 5 (Commons I), then 2 → 3 → 4a → 4b → 4d (Commons II), then 6.

## 6. Parity, route classification and land isolation

### 6.1 Slack parity: MUST / LATER / DROP for the Commons

Built from the Slack inventory (gd-RPic1giSw5). Where this list differs from the inventory,
the reason is given. The phase is when it ships.

**MUST**

| Capability | Phase | Note |
|---|---|---|
| Channels per land (`#general`, `#crew`, `#decisions`, `#escalations`, user-created) | I | |
| Threads; every card has a thread | I | |
| Thread replies (text, files, images) forwarded to the bound agent; reply resurrects the agent (respawn with resume, freeze/thaw) | I | slice 4c |
| Agent read/write API: `gb say`, `gb thread`, files; `gb squawk` progress line | I | slice 5 |
| Bead-comment and chat-close relay into the thread | I | slice 1 |
| History survives pods and bridge state loss; search over beads scoped by land | I | free with C1 |
| Phone-width first | I | |
| Reliability: panic recovery, reply dedup, DLQ, binding validation | I | |
| Identity: every write attributed `oidc:<email>` | I | the Commons has OIDC; Slack keeps the account link |
| Agent card (state, project/role, task, cost, context, spawn vector, owner, Glass link) with Clear/Kill/Restart, wrap-up face on exit | II | |
| Live stream status in the thread | II | from coop state, not Slack streaming |
| Decision card: priority, options, Other, Dismiss, Promote; free-text reply resolves; DM-first with pointer | II | |
| Escalation card, approve/reject, result, pingback; operator DM | II | |
| Tool-permission Allow/Deny | II | |
| Mention spawns a thread agent; `--role`, `--model` (alias allowlist) | II | slice 4d |
| In-thread kill/restart/stop/clear, debounced | II | |
| Done button on a thread head (replaces ⏹️) | II | |
| DMs human ↔ agent, `gb say --to --ask` | II | |
| Death report with storm roll-up, crash notice | II | |
| Channel-bound persistent crew agents | II | |
| Card routing by `channel_roles` | II | inventory says LATER; it is the placement rule (2.5) and costs nothing once channel beads exist |
| Two-way Slack mirroring per channel | II | |
| @mentions of people, unread counts, badges, notifications (native push via push-bridge, email digest), phone-answerable cards | III | native push before the Commons takes over decision and DM delivery (gd-t4lzcEOEFT) |
| Presence (who is here) and typing | III | inventory says DROP because the bridge never used them; the owner named many humans in one land as the point, and presence is what makes that legible |
| Multi-human land with land roles | III | |

**LATER**: `summarize`; `<epic-id>` gardener start; `--project/--ultracode/--backend/--fresh/
--quiet/--listen` (`--fresh` first); rate/exit surveys; `/decisions` list (the Workbench has the
queue); reports as pages (canvas replacement); reaction rules; lifecycle reactions (a state chip
on the card first); invite-spawns-gardener ("create a channel, get a gardener"); concierge;
channel read/search/manage API (`gb channel` over beads); group DMs; private channels; ops
telemetry as an `#ops` channel; link unfurl / bare-id preview (native bead refs first); noise
config and vibe mode (ported as a delivery policy in III, configuration later); slash commands.

**DROP**: Garden/Business card themes in the Commons (C8; Slack keeps them); App Home /
assistant pane; pinned roster dashboard; `gb slack view` and U1–U12 for the Commons (C1–C12,
task 0c, replaces them; Slack keeps U1–U12 while it mirrors).

### 6.2 Land isolation

The wall is kbeads' project scope, which already fails closed for scoped principals. Four rules
make it hold for conversation:

1. **Exactly one `project:` label on every channel, message and card bead.** kbeads' read cut
   for scoped principals is `LabelsOrNoProject` (`kbeads/internal/server/read_authz_scope.go:51`):
   a bead with *no* project label is visible to every scoped principal. A label-less message
   would leak to every land. Commons I adds daemon-side validation: `type=channel` and
   `type=message` with `channel_id` are rejected on create without exactly one `project:` label,
   and a message's project must equal its channel's.
2. **Lands never share a channel.** A channel bead belongs to one project; `slack_channel_id`
   is unique across channel beads, so one Slack channel cannot mirror into two lands. Only an
   admin sets, changes or clears `slack_channel_id` or `mirror` (gd-ecFKkjtdvS,
   `kbeads/internal/server/channel_binding_authz.go`), on every write route and both transports:
   uniqueness alone would let a land claim an unbound operator channel first.
3. **The Commons SSE stream is cut per principal.** Events carry labels; each connection drops
   events outside the principal's projects before they leave the server, as the feed hub does
   (`cmd/beads-viewer/bead_feed_hub.go`). The shared gardenhub snapshot is not reused for the
   Commons, because `/garden*` is not yet audited for a per-principal cut.
4. **Agents speak only in their own land.** `gb say` resolves channels within the agent's
   project; the projector refuses a message whose target channel's project differs from the
   author agent's project.

### 6.3 Route classification

Every Commons route is in `viewerRouteClasses` (`cmd/beads-viewer/route_classes.go`) before it
merges; `TestEveryViewerRouteHasClass` enforces it.

| Route | Class (Commons I–II) | Class (Commons III) |
|---|---|---|
| `GET /garden/c` (land + channel list), `GET /garden/c/{land}/{channel}`, `GET /garden/c/{land}/{channel}/t/{thread}`, `GET /garden/c/m/{id}` (message/card fragment) | `routeScoped`: every read is on the viewer's bearer | `routeScoped` |
| `GET /garden/c/events` (SSE, id-only) | `routeScoped`, per-principal cut (rule 3) | `routeScoped` |
| `POST /garden/c/{land}/{channel}/say`, `POST /garden/c/t/{thread}/reply` | `routeAdminOnly` | `routeScoped`, after kbeads lets scoped principals create `message` beads (`scopedWritableTypes`, `kbeads/internal/server/scoped_principal.go:68`) |
| `POST /garden/c/action/{id}` (card buttons), `POST /garden/c/submit/{id}` (lightbox forms) | `routeScoped` from Commons II step 5, gated per action-table entry for the card's land (7.2 step 5) | `routeScoped` per action-table entry (6.4) |
| `GET /garden/c/lightbox/{kind}/{id}` | `routeScoped` | `routeScoped` |

The comment above `viewerRouteClasses` says "every POST is admin-only"; Commons III changes that
policy for these routes on purpose, with scoped write tests next to `scoped_writes_test.go` in
kbeads and a viewer test per route.

On the bridge, `POST /api/conversation/inbound` is internal: it accepts only the beads-viewer
service identity with the edge-auth principal attached, and checks the action table, which is
the start of the product definition's "action table across four surfaces" (its hard problem 3).

### 6.4 Land roles and the action table

Built in Commons III step 2 (gd-9aCcG1zCCY). A land role comes from the realm groups on the
verified token, read the same way by kbeads (`kbeads/internal/server/land_role.go`) and the
controller (`internal/conversation/landrole.go`, `LandRolesFromGroups`):

| Group | Role |
|---|---|
| `/project-<land>` | member |
| `/project-<land>/owners` | owner |
| `/project-<land>/guests` (or any other subgroup) | guest |

Keycloak subgroups inherit the parent's `gardenio-project-member` role, so an owner or guest
needs no direct membership of `/project-<land>`. The highest role wins; a guest in one land
can be a member of another. An admin principal acts as `operator` in every land.

The action table (`conversation.Allows`) is the least role each action needs; an action not in
it is operator-only:

| Action | Guest | Member | Owner |
|---|---|---|---|
| `say`, `reply`, `react` | yes | yes | yes |
| `done`, `spawn` (Q8c), `agent_control`, `decision_respond`, `decision_dismiss`, `decision_promote`, `permission_answer`, `escalation_answer`, `channel_join` | no | yes | yes |
| `channel_manage`, `member_manage`, `steward_delegate` | no | no | yes |

`escalation_answer` is the role gate only. The button also needs the daemon's `can-approve` yes
for that viewer and escalation, so a member sees Approve only on an escalation whose `approvers`
include `land-owner:<land>` and whose tier allows it.

`steward_delegate` sets `land_steward_approves` through
`POST /v1/lands/{land}/steward-delegation`. It is owner-only, and it can never include an
operator-only operation type.

`member_manage` is the owner's hold on the land's people (P10.C gd-wi44L8Qy4I, owner ruling
gd-CJUlVSMzEe). Its routes are the People lightbox's invite and revoke (Track B §3.7), on the
page session. Anything else about who is in the land (a second owner, moving a member to
`/guests`, taking someone out at once) is a `keycloak_group_membership` escalation filed in the
land (`gl`/`kd` file it and hand the page link). The owner floor keeps
`keycloak_group_membership` for global roles and gardenio's own groups. An escalation is off the
floor only when all of these hold: it carries exactly one `project:<land>` label, of a tenant
land; every group in its `operation_spec` (`group` or `groups`) is `/project-<land>` or one
subgroup of it; and it has no other keys besides `user`/`users`, `action` (`add` or `remove`)
and `reason` (`escalationLandPeople`, `kbeads/internal/server/land_people.go`). Such an
escalation is routed in code beside the floor (`policy_rule=land:people`):
`approvers=["land-owner:<land>"]`, held by that lane, and executed by `coordinator:laptop`, which
runs the Keycloak change. An owner of the land approves it on the page. Their `gl`/`kd` login is
refused with the link, and a member or guest is refused. It is never delegable to the steward:
`land_steward_approves` refuses it as before. Anything else stays on the floor.

kbeads enforces the same split on the person's own bearer, through one tier-keyed table,
`actionTable[tier][type][action]` in `kbeads/internal/server/action_table.go` (D.3,
gd-QjqeHIEmWC): a guest may only create `message` beads, change its own reactions and leave a
channel, and is refused agent spawns and escalation approval. In a members-only channel `say`
and `reply` also need membership (2.2.1). Every scoped write check, escalation approval and
the spawn gate call `scopedTierDenial`; `TestActionTable_AllTypesAllTiers` pins every tier ×
type × action. `GET /v1/me` reports the caller's lands and tier in each, from the same
`scopeFromContext` call the read scope uses. A new principal class (the land principal, 10.11;
the `lands-tenant` tier, 10.1) is a new row, not a new code path. The
purse check and per-member rate limit on spawns are Phase 8's. A Commons write route is
`routeScoped` and calls `viewerScope.may(land, action)` before it writes or calls the bridge;
the routes in the table above take that class as they land (the gate exists before them).

## 7. Steps for phases 2–4

The plan-gardener mints these as child tasks of the phase epics. Each step is one PR unless it
says otherwise, and every UI step ships with goldens under the looking glass (phase 1, task 1a)
and a C1–C12 grade (task 0c).

### 7.1 Commons I, the room (gd-KoOR5SPlNE)

1. **Seam.** `internal/conversation` with `Surface`, `Router`, `Inbound`, `Ref`, `Modal`;
   `slackSurface`; recorder fake; contract tests against the recorder and the slackmock twin.
   (slice 0)
2. **Channel type.** Register `type=channel` in kbeads with field validation; the
   one-`project:`-label rule for `channel` and `message`-with-`channel_id` (6.2 rule 1);
   `gb channel create|list|archive`.
3. **Seed the operator's lands.** A script that creates the four default channels per existing
   project and binds each to its current Slack channel (`mirror=both`), reading `channel_roles`
   and the bridge channel config. Idempotent; dry-run first.
4. **Stamp `channel_id`.** The bridge stamps `channel_id` on every thread head, forwarded
   message and squawk it writes (dual-write, nothing reads it yet); backfill for open thread
   heads from `slack_thread_channel`.
5. **Leaf outbound through the router.** Squawk projector (labels `squawk` and `say`),
   comment/chat-close relay, thread message forward, death report, crash notice. Slack payload
   goldens unchanged. (slice 1)
6. **Inbound replies.** Slack thread replies normalised to `Inbound{Reply}`; `HandleReply` holds
   the thread-forward core (conversation bead, nudge, respawn with resume); dedup, validation,
   subtype filter and recovery move with it; `POST /api/conversation/inbound` with the sweep
   safety net. (slice 4c)
7. **`gb say` and `gb thread`.** `/api/conversation/say|thread`; the verb and aliases (`gb
   squawk`, `gb slack reply`, `gb slack upload`, `gb slack thread`). (slice 5)
8. **Commons read model.** `internal/commons` in beads-viewer: channel list, timeline, thread,
   message fragment, per-principal SSE with `?since=` replay; route classes; scoped acceptance
   test proving a scoped principal sees only its land.
9. **The Commons pane in `/garden`.** Channel list, timeline, thread drawer, composer (plain
   messages and replies), docked on desktop, full screen on phone (section 8); goldens at phone
   and desktop, light and dark.
10. **Retire `/agents/chat`'s read path.** The porch chat (`internal/gardenhub/web/chat.js`)
    reads Commons thread fragments; `/agents/chat*` redirect to the matching Commons thread;
    remove the old handlers once nothing links to them.
    *As built (gd-EwISaXh6Xk):* `GET /garden/c/bead/{id}` (`internal/commons/place.go`) finds a
    bead's thread on the viewer's bearer: a message's thread, a wish head's own, an agent's by
    `thread_id` (else `slack_thread_bead_id`). It answers JSON to a client that asks and 303s
    anything else. A Porch row asks it and opens the Commons pane on that thread (no second
    view); a gardener bound to no thread opens its own bead. `/agents/chat*` 303 to the same
    place, else `/garden?bead=<id>`, else `/garden`; the page, its fragments, its JSON face and
    its templates are deleted. Decisions in the porch are answered from the Mailbox until
    decision cards reach the Commons (Commons II slice 3).
11. **Check-in.** Screenshots from the harness in #gardenio-homestead; slackmock e2e green;
    a reply typed in the Commons nudges a live agent and resurrects a dead one.

### 7.2 Commons II, the crew in the room (gd-xh6Ldnpjow)

1. **Card beads and placement.** `kind=card` with `subject_id`, unique per
   `(subject_id, channel_id)`; the placement function (2.5) shared by both surfaces.
2. **Agent cards.** Slice 2: notifiers through the router, refs dual-written to card beads;
   Commons agent card live face, wrap-up face, stream status line from coop state.
3. **Decisions and permission.** Slice 3 for decisions, mirrors, reports, permission; Commons
   decision card with option buttons, Other and Promote lightboxes, Dismiss; permission card.
4. **Escalations and operator DMs.** Slice 3 for escalations; Commons escalation card.
   *As built (gd-1oIKTXObpw):* the escalation card goes through the Router as a card bead of the
   escalation (`internal/bridge/escalation_cards.go`); a card Slack refuses is posted again as its
   plain text and that copy is stamped on the card bead. The operator DM is recorded on its own
   card bead (`RecordDM`). A `sensitive` or `secret` escalation keeps its Slack refs off every bead
   (ka-0000003667): its card bead is written in the bound Commons channel with no Slack ref, the
   Slack copy is posted directly, and its DM is not recorded. The approved, result and rejected
   edits and the pingback still go to the Slack copy the Bot tracks; the queue alert and stale
   digest stay Slack-only. `TestEscalationCardCallsGolden` pins the Slack calls. The Commons face
   (`internal/commons/escalation_cards.go`): waiting (operation, where it runs, sensitivity, up to
   four credentials, context, merge request, blocked task, pingbacks; Approve and Reject on the
   card action route, 501 until step 5; Review on the escalation's page), approved, done (only
   `redacted_summary` for a sensitive one, never `result`) and rejected.
5. **Inbound actions and submissions.** Slice 4a; Commons card buttons and lightboxes through
   the inbound endpoint; the first action-table entries (clear/kill/restart, resolve, dismiss,
   promote, allow/deny, approve/reject).
   *As built (gd-amX5uMx4KM, `internal/bridge/inbound_action.go`):* Slack block actions and view
   submissions are `Inbound{Action}` and `Inbound{Submit}` (`ActionID` is the action_id or the
   callback_id; `Raw` carries the interaction callback), dispatched to `HandleAction` and
   `HandleSubmit`. The Slack handlers under them are unchanged, and the clear/kill/restart cores
   take the Slack channel and thread and a `say` instead of the callback;
   `TestInboundActionCallsGolden` pins their Slack calls, coop calls and bead closes. `POST
   /api/conversation/inbound` takes `{"kind":"action"|"submit","card","action","value","values"}`:
   the bridge reads the card bead, its one land and its subject itself, maps the button to its
   action-table entry (`CommonsCardAction`: decision resolve and Other `decision_respond`, Dismiss
   `decision_dismiss`, Escalate `decision_promote`, Allow/Deny `permission_answer`,
   clear/kill/restart `agent_control`), and checks it against the caller's role in that land, read
   off their verified token (`landRoleFromToken`, as kbeads reads it), before it runs anything; the
   handler runs as the person and on their bearer. A Commons permission answer records the
   verified `oidc:` principal and edits the Slack prompt; a decision's Slack copy is re-rendered by
   the decision watcher when the decision closes. bridgeRBAC admits any signed-in person to the
   route; a reply there stays the operator's until spawns are gated by land role (slice 4d). The
   Commons side (`internal/commons/card_actions.go`): `POST /garden/c/action|submit/{id}` are
   `routeScoped`; the card is read on the viewer's bearer, the button gated by `Gate.MayAct`
   (`viewerScope.may`), and the action sent to the bridge (`SLACK_BRIDGE_URL`; unset answers 501,
   as before) on the viewer's bearer. An escalation's Approve and Reject need nothing of the fleet:
   they call the daemon's escalation routes on the viewer's bearer behind the bead page's
   `approve:escalation` check. The answer is the card as it now renders, with the bridge's words in
   `X-Commons-Note` (percent-encoded), which the pane toasts. A live agent card offers Kill, and
   Restart when it is bound to a Slack thread; Clear stays Slack's (it clears a Slack card).
6. **Done button.** Slice 4b: wish-close as `Inbound{React}`; Done on the thread head.
   *As built (gd-959e8tp0Us, `internal/bridge/inbound_react.go`):* marking a wish done is
   `Inbound{React}` with `Reaction` `conversation.ReactionDone`, dispatched to `HandleReact`. The
   Slack adapter turns a ⏹️ (`black_square_for_stop`) into it, with the event in `Raw`, and the
   handler under it is unchanged: a ⏹️ that resolves to no wish head is not taken and falls through
   to the reaction rules, which stay Slack's. Both surfaces close through one core (`closeWish`):
   `close_reason=slack-thread-user-close`, a closed wish a no-op, a `needs-triage` wish refused.
   `TestInboundReactCallsGolden` pins the closes and that nothing is posted to Slack. `POST
   /api/conversation/inbound` takes `{"kind":"react","message":<head>,"reaction":"done"}`: the
   bridge reads the head itself (a wish head with exactly one land, else 422), checks `done` against
   the caller's land role, and closes it on their bearer; done already or held for triage answer
   409. The Commons side (`internal/commons/done.go`): an open wish head, in the timeline, its thread
   and its refetch, offers **Mark done** to a viewer `Gate.MayAct` admits for `done`, when the land is
   not frozen for them and the bridge is configured (`SLACK_BRIDGE_URL`). It posts to `POST
   /garden/c/t/{thread}/done` (`routeScoped`, refused cross-origin), and the answer is the closed
   head, with the bridge's words in `X-Commons-Note`. The pane replaces every copy of the head on
   screen (timeline and thread root), and a head's own event refetches the open thread's root too.
7. **Mention to spawn.** Slice 4d: `HandleCompose`, mention flags (`--role`, `--model`),
   in-thread verbs; `ThreadAgents` re-keyed by head id; Commons composer with `@agent`
   autocomplete; chat root promoted to a wish head on mention (2.3).
8. **DMs.** `channel_kind=dm`; `gb say --to` and `--ask` (reply capture replaces
   `dm_delivery.go`'s); DM-first decision and escalation delivery into the DM channel.
   *As built (gd-F4oSp9S96G):* a DM channel is found or made by `conversation.EnsureDM`
   (`internal/conversation/dm.go`): `channel_kind=dm`, `members` the sorted principals, and
   the name `dm-` plus 16 hex digits of their hash, so the same members find the same channel
   in a land and the slug names nobody. The bridge closes a DM that is done with every 30
   minutes (`conversation.DMCloser`, gd-ZUywEoAvCi): its agents' beads are closed, its newest
   top-level entry is no card on an open subject, and each person has read it to the end;
   the same members writing again reopen it, and a successor agent gets its own. `gb say --to <who>
   ["--ask"]` posts to `/api/conversation/say` with `to` (and `ask`, `task`)
   (`internal/bridge/api_conversation_dm.go`): `to` is `agent:<name>` (an agent of the
   speaker's land, else 403), an email or `oidc:<email>`, a Slack user id, or a GitLab or
   GitHub username from the user directory (a person needs an email, their member id). The
   message bead is `kind=text` or `kind=ask` with `to`, in the DM between `agent:<speaker>` and
   the target. **Slack stays the primary delivery** (Q8b gd-t4lzcEOEFT): a person with a Slack
   id gets the Slack IM with exactly `gb dm send`'s or `gb dm ask`'s face, its ref stamped on
   the bead; an agent is nudged. Reply capture (`internal/bridge/dm_replies.go`): a reply in
   the thread of an ask's Slack IM is written under the ask in the DM (`origin=slack`), the
   asker is nudged, and the thread is told it was captured; a person's Commons message in a DM,
   top-level or a reply, is dispatched like any Commons reply (an SSE watcher, the inbound
   call, and the sweep, which now takes top-level DM messages too) and nudges every agent in
   the DM but its author. The agent reads the exchange with `gb thread <ask-id>`. The DM-first
   decision card and the escalation operator DM are recorded on card beads in the DM between
   the requesting agent and the addressee (`recordDMCard`, `internal/bridge/dm_channels.go`),
   else, with no agent or no email, keyed by the Slack IM alone as before; the Slack calls do
   not change (`TestDecisionCardCallsGolden`, `TestEscalationCardCallsGolden`). `type:message`
   declares `to`. Not done here: `gb dm ask`/`send` still write `type=dm` beads delivered and
   reply-tracked by `dm_delivery.go` (they become aliases when that path retires); a DM carries
   no files yet; the ask reminder (`timeout_minutes`) is `gb dm ask`'s only; the Commons pane
   already lists DMs in its Direct section (gd-9ZaWoQ5VBT), so no UI changed.
9. **Two-way mirroring.** Slack messages in a `mirror=both` channel become message beads (not
   only forwarded thread replies, as today); Commons messages post to Slack as "*Name* (via
   Commons)" (or with `chat:write.customize` username and icon, if the scope is granted); the
   `origin` loop guard; a test that a message never echoes.
   **As built (gd-t3s76tjnz9, `internal/bridge/commons_mirror.go`).** Slack → Commons: in a
   `mirror=both` channel, `handleMessageEvent` writes a person's top-level
   message, and any reply whose root the Commons has (a message or card bead whose
   `slack_channel`/`slack_ts` is the thread), as a closed `origin=slack` message bead with
   its Slack copy as `slack_channel`/`slack_ts` (idempotent on the Slack ts). Replies under
   a wish head stay the thread forward's, now stamped `origin=slack`; mentions stay the
   mention path's; a reply whose root predates the mirror is not written. Commons → Slack:
   the `CommonsMirror` watcher projects an `origin=commons` message through the Router
   into a channel with `slack_channel_id` and `mirror` `out` or `both` (a reply under its
   root's Slack copy), as `*Name* (via Commons)` then the escaped text; a `thread-say`
   reply is `ThreadMessageForward`'s and gets the same attribution. Name is the user
   directory's display name, else the email's local part. Loop guard, one per hop: a bot's
   post (`bot_id`) is never mirrored in, only `origin=commons` goes out, and the Router
   skips the origin surface (`TestMirror_NeverEchoes`). An unset `mirror` is `off` both ways,
   in the Router too, so binding `slack_channel_id` alone copies nothing
   (`TestMirror_UnsetOrUnboundCopiesNothing`); the binding itself is admin-only in kbeads
   (gd-ecFKkjtdvS). `chat:write.customize` is not
   used. Attachments travel as links, never bytes (gd-2Ie1cDAm4K): a Slack message's files
   go in the bead's `files` field (name, mimetype, size, Slack permalink), a file-only post
   is mirrored too, and the Commons shows them as links; a Commons message's `files` go out
   as links under its text. The Commons composer attaches files (gd-S8ufEc3k6s): say and
   reply take multipart/form-data once the Gate has let the viewer post (10 files, 10 MB
   each, 25 MB a message), the bytes go to the object store under
   `commons/files/<land>/<channel id>/<random id>/<name>` (beads-viewer's
   `COMMONS_FILES_BUCKET`/`COMMONS_FILES_PREFIX`, else the session bucket), and `files`
   records `/garden/c/f/{land}/{channel}/{id}/{name}` as an absolute URL. That route reads
   the channel on the viewer's bearer first, so a file opens for whoever may read its
   timeline; only a raster image is served inline, anything else as a download. In Slack
   such a link asks for a viewer sign-in. `CommonsMirror.Sweep`
   (every 2 min, 6 h window, 1 min grace) posts an `origin=commons` message with no
   `slack_ts`; before posting it reads the channel's (or thread's) Slack history for the
   bot's copy and stores that ref instead, so a post whose ref stamp was lost is never
   posted twice (`TestCommonsMirror_Sweep`).

   **Snapshots** (gd-GbF2JS04h5). The farm's camera Menu has a Snap item (`s` on the board,
   ignored while focus is in a field; signed-in viewers only, hidden in the Ledger). It turns
   the farm canvas as drawn (`#cv`, device pixels; not the labels, links or minimap) into a
   PNG named `farm-<land>-<at or yard>-<YYYYMMDD-HHMM>.png` and picks it into the composer the
   viewer writes in, opening the pane first on the last channel, else the first listed
   (`#general`), with the field focused for optional words. A PNG over the 10 MB file cap is
   drawn again at DPR 1, unsmoothed. Send posts the form's `context` field beside it: a JSON
   object of at most 2 KB, `{kind: "farm", url, at, z, who, night, framed: [{kind, id,
   title}], vp: [w, h], taken_at}`, `framed` being the hot rects in the viewport, at most 24.
   say and reply keep it as the message's `snap` field (declared on `type:message`) only
   beside a file: each key is read and bounded, an unknown one dropped, and `url` kept only
   as a path under this viewer's `/garden`, stored absolute. The person's words stay `text`; a
   snapshot with none is titled "Snapshot of the farm at <place>". The timeline captions it
   under its files ("Farm at <place> (z2, Mine, night) · 3 plans, 7 beads framed · Open the
   view"); the Commons mirror, the thread forward and the inbound sweep put the view link
   after the file links, so an agent nudged by the message gets the picture and the view.
10. **Death and crash in the Commons.** Death face with storm roll-up; crash notice.
11. **Parity check.** Walk the MUST rows marked I and II in 6.1 on the operator's land; goldens
    for every card face, both themes of the page, both widths.

### 7.3 Commons III, many people (gd-lDmTW3GkPq)

1. **Scoped writes in kbeads.** `message` joins `scopedWritableTypes` as create-only (plus
   `reactions` update on messages); tests in `scoped_writes_test.go`.
2. **Land roles.** owner / member / guest on project membership (realm groups under
   `/project-<name>`); the action table keyed on land role; Commons write routes move to
   `routeScoped` (6.3). The model and table are in 6.4.
3. **Presence.** Who is in the land and in the channel, from SSE connections; agents from the
   kbeads presence tracker; avatars in the channel header.
4. **Typing.** Ephemeral SSE events, never beads, rate-limited per connection.
5. **Read markers and unread.** `read_cursors` on the user bead, debounced; unread counts and
   a badge per channel and per land.
6. **@mentions of people.** Autocomplete over land members; a mention creates a notification
   record for the person.
7. **Notifications.** Native push first (owner decision gd-t4lzcEOEFT, native-push): the
   existing APNs `push-bridge` and iOS Gardenio app, with devices registered per user and
   alerts sent only to the land's members, deep-linking into the `/garden` Commons pane. The
   chart ships `pushBridge` disabled with no credentials. Enabling it needs an operator
   escalation for the APNs key, team id and bundle id. Email digest; per-user quiet rules;
   storm roll-up and dedup ported from the bridge as a delivery policy. The Commons does not
   take decision or DM delivery from Slack until native push works. Slack stays the fallback
   until then (permanent mirror, gd-78Tb2kbQrR).
8. **Channel membership.** Members-only user channels; private channels stay LATER. The rules
   are in 2.2.1. The Commons pane renders join and leave once it exists (7.1 step 9).
9. **First multi-human land.** The operator and the ClaimLayer teammate in one land for a week;
   check-in with screenshots and what broke.

## 8. Where the Commons is read

The Commons is read in the workbench, a page of its own beside `/garden`, not a pane inside it. The band that showed it
along the bottom of the World (and as a sheet on a phone) was replaced, not repaired:
[`adr-commons-band-replaced.md`](../../adr-commons-band-replaced.md) (plan gd-lRaCxsiLvC, step 8).

- **The workbench, `/garden/c/n/{id}`** (`web/commons2.js`, `web/node.html`). Any bead is a root: a plan and its phases
  and steps, a phase, an epic and its tasks, a thread and the agents it spawned. Beside the tree: the selected node's
  transcript (its thread, read from the thread fragment) with its composer, and the Jobs, Plan and Open rails. On a phone
  the same model is a push stack (tree, node, transcript).
- **The steward, `/garden/c/s` and `/garden/c/s/{land}`.** The land's steward as a root: the `#general` threads that
  address it, a composer that posts to `#general` with `@steward`, its plans and gardeners, and what it asks for you.
- **Ways in from `/garden`.** The Commons tab (phone) and the HUD's Commons link (desktop) open the viewer's steward (the
  page's `?land=`, else the viewer's land). A Porch row and a gardener's sprite open the gardener's node; a bead view's
  thread row, Adopt and Tend's "Follow it in the Commons" and the welcome tour's end open the thread's node or the land's
  steward. Snap and Postcard post the picture into `#general` or the plan's thread and open the node.
- **URLs.** Every older Commons address still lands: a browser navigation to `/garden/c` goes to `/garden/c/s`, to
  `/garden/c/{land}/{channel}` to `/garden/c/s/{land}`, to `/garden/c/{land}/{channel}/t/{thread}` to
  `/garden/c/n/{thread}`, and `/garden/c/m/{id}` to the node of the message's thread (302s), so a Slack link, a push
  notification or a card link opens the right place. `/agents/chat*` redirects here (7.1 step 10).
- **Rendering.** Server-rendered fragments (D8), fetched by the workbench with `X-Commons: fragment` and patched from the
  one event stream (`/garden/c/events`, which carries each message); lightboxes for every non-toggle action (D12).

## 9. Open questions this spec does not settle

These went to the owner through task 0d. The answers are recorded inline.

1. **Land = project, 1:1?** Recommended: yes for v1 (C2). A tenant with several repos gets
   several lands until there is a reason for a land of many projects.
2. **Who may spawn in a land?** Answered (gd-fPXoeHDQS3, land-role): owner and member may
   spawn, guest may not. Every spawn passes the purse check and a per-member rate limit. The
   steward spawns within budget.
3. **Does Slack stay a permanent mirror?** Answered (gd-78Tb2kbQrR, permanent-mirror): Slack
   stays a supported surface per land. `slack_*` fields stay as aliases of the conversation
   id, and "cut-over" for a land means re-pointing its bindings, not retiring the Slack
   surface.
4. **Mirror attribution.** Bot post with a name prefix, or `chat:write.customize` so Commons
   authors show with their own name and avatar? The second is a Slack scope change (an
   escalation, not code).

## 10. Risks

- **Open-heart surgery on the bridge.** Mitigated by the slice rule: no Slack-visible change,
  a Slack payload golden per moved handler, slackmock e2e green per slice, and the inbound
  mention path last.
- **Dual state during the re-key.** Card refs and thread bindings live in both the state file
  and beads for one release; reads prefer beads and fall back, and a mismatch logs a metric
  rather than failing.
- **A label-less conversation bead leaks across lands.** Closed by the daemon validation in
  Commons I step 2, which ships before any Commons read route.
- **Write amplification.** Card beads are written once per subject and channel; updates write
  nothing new; read cursors are debounced. Measure write rate on the operator's lands at the
  Commons II check-in.
- **Mirror echo loops.** The `origin` guard and a test that a message posted on either side
  appears exactly once on each.
