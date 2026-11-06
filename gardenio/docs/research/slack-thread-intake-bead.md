# Slack-Thread Intake Bead — Phase 1 Research

**Bead:** ka-0000001756 (under epic ka-0000006870)
**Date:** 2026-05-08
**Scope:** State-of-the-world audit only. **No design, no implementation.** Phase 2
(design + migration plan) lives in a downstream task and fills in the placeholder
sections at the bottom of this document.

This doc answers three questions:

1. How do Slack threads, the slack-bridge, and thread-spawned agents relate to beads
   today? (§1)
2. How does that compare to the jira-bridge and gitlab-bridge intake-bead models?
   (§2)
3. What concrete agent-reliability gains would a first-class slack-thread intake
   bead unlock? (§3)

## 1. Current state — Slack threads, the bridge, and thread agents

### 1.1 Spawn flow (Slack mention → agent bead)

When an `@gardenio` mention arrives, the bridge dispatches to either a *channel
agent* (no `thread_ts`) or a *thread agent* (`ev.ThreadTimeStamp != ""`). The
thread path is the case this doc cares about.

- **Detection** — `gardenio/controller/internal/bridge/bot_mentions.go:556` reads
  `threadTS := ev.ThreadTimeStamp`; the boolean "is-this-a-thread-message?" is
  literally `threadTS != ""`.
- **Concurrent-spawn guard** — bot_mentions.go:583–618 takes `b.mu`, checks
  `b.spawnInFlight[channel+":"+threadTS]`, and also asks the in-memory state
  manager `b.state.GetThreadAgent(channel, threadTS)`. If either says "already
  spawning" or "already bound", the second mention is rejected. The
  `spawnInFlight` entry is cleared 10s later via `time.AfterFunc`.
- **Bead creation** — bot_mentions.go:801–835 creates a `type=agent` bead with:
  - **Labels:** `["slack-thread", "project:<resolved>"]`
  - **Fields (string map):**
    - `slack_thread_channel` — Slack channel ID (e.g. `C123ABC`)
    - `slack_thread_ts` — thread root timestamp (e.g. `1700000000.000001`)
    - `slack_spawn_channel` — same channel for thread spawns
    - `slack_user_id` — Slack user ID of the mention author
    - `spawn_source` — `"slack-thread"`
    - `agent` — `"thread-" + sanitizeTS(threadTS)`
    - `mode`, `role`, `project`, optional `user_bead_id`
- **Pool vs cold-start** — pool-prewarmed pods get fields applied via
  `daemon.UpdateBeadFields()` (bot_mentions.go:738–744); cold-start pods receive
  the same fields serialized into the `CreateBead` request.
- **State binding** — bot_mentions.go:848 calls
  `b.state.SetThreadAgent(channel, threadTS, agentName)` to persist the
  thread→agent mapping in `bridge/state.go`'s on-disk JSON map keyed by
  `"{channel}:{thread_ts}"`. Restart-recovery walks `type=agent` beads with valid
  `slack_thread_*` fields and re-binds.

There is **no separate "thread bead"**. The agent bead carries the thread metadata,
and the in-memory/JSON state map is the only structure indexed by
`(channel, thread_ts)`.

### 1.2 Thread-agent identification

Once a pod starts, the agent locates its thread through two paths
(`coop`/`gb` source under `gardenio/controller/cmd/gb/slack.go`):

- **Env vars** — the bridge sets `SLACK_THREAD_CHANNEL` and `SLACK_THREAD_TS` on
  the pod when those bead fields exist. `gb slack thread` reads them directly
  (slack.go:29–35, 173–176).
- **Bead-field fallback** — when env is empty, `gb` calls
  `daemon.GetBead(BOAT_AGENT_BEAD_ID)` and pulls
  `bead.Fields["slack_thread_channel" / "slack_thread_ts"]` (slack.go:179–204).

### 1.3 Posting back

Agents do **not** call Slack directly. They go through bridge HTTP endpoints
(`gardenio/controller/internal/bridge/bot_threads.go`):

- `GET /api/slack/threads?channel=…&ts=…&limit=N` — read thread (`gb slack thread`)
- `POST /api/slack/threads/reply` — post reply (`gb slack reply`)
- The bridge calls `a.api.PostMessage(req.Channel, slack.MsgOptionText(...),
  slack.MsgOptionTS(req.ThreadTS))` (bot_threads.go:234–237).

`gb squawk` flows through the same pipeline and is automatically threaded under
the agent's spawn message.

### 1.4 File roles

| File | Role |
|---|---|
| `bot_threads.go` | HTTP API for read/reply that agents use via `gb slack thread` / `gb slack reply`. |
| `bot_thread_forward.go` | Handles non-mention replies in a bound thread: throttles nudges, creates `slack-thread-reply` tracking beads, posts "resume" hint when agent has exited. |
| `bot_thread_validate.go` | 6-hour validation loop that prunes `state.ThreadAgents` entries whose threads are deleted/archived or whose agents no longer exist. |
| `bot_thread_concurrent_test.go`, `bot_thread_validate_test.go`, `thread_audit_test.go` | Coverage for the spawn race, validator, and end-to-end audit scenarios. |

### 1.5 Bead linkage and lineage

A thread today exists as:

- **One agent bead per spawn**, with `slack_thread_*` fields and the
  `slack-thread` label.
- **Zero or more "spawn-tracking" beads** under thread forwarders
  (bot_thread_forward.go labels them `slack-thread-reply`).
- **An entry in the in-memory state map** keyed `{channel}:{thread_ts}`.

When an agent exits and the user replies again, the bridge spawns a *new* agent
bead with the same `slack_thread_*` fields. The two agent beads share a thread
key but have **no parent/child or sibling typed-mention edge** between them; the
only thing tying them together is the (channel, thread_ts) tuple, recoverable
only by querying agent beads with matching field values.

## 2. Comparison: jira-bridge vs gitlab-bridge vs slack-thread today

> **Direction of travel.** Per advice ka-0000003234 (2026-05-03), the jira and
> gitlab bridges are moving from "bidirectional sync" to **one-way enrichment**
> — beads as read-only mirrors of upstream tickets/MRs, with comment ingestion
> bounded, deduped, and append-only. The columns below describe **current
> code** (which still has some outbound jira writes) and call out the
> direction-of-travel where it differs.

| Aspect | jira-bridge | gitlab-bridge | slack-thread (today) |
|---|---|---|---|
| **Intake bead created?** | Yes — one task/issue bead per JIRA issue. | **No.** Bridge creates `gitlab-mention` task beads on `@gardenio` comments and updates pre-existing beads' MR fields. There is no per-MR mirror bead. | **No thread-level bead.** Only per-spawn `type=agent` beads with `slack_thread_*` fields. |
| **Source label** | `source:jira` (always) plus `jira:<KEY>`, `jira-label:*`, `jira-epic`, `jira-has-media`. | **No `source:gitlab` label.** Mention beads carry `gitlab-mention`. MR-status beads aren't labelled by the bridge at all. | `slack-thread` (on the agent bead). No `source:slack`. No thread-level identity bead to label. |
| **Stable upstream identity** | `jira:<KEY>` label + `jira_key` field (`PE-7001`). Used as idempotency key everywhere. | None. Mention beads store `gitlab_mr_url` / `gitlab_mr_iid` in fields but no label-queryable identity. MR-status beads are matched at runtime by `mr_url` field. | `(slack_thread_channel, slack_thread_ts)` field tuple. No label, no first-class entity. |
| **Key fields** | `jira_key`, `jira_project`, `jira_url`, `jira_type`, `jira_status`, `jira_epic`, `jira_reporter`, `jira_assignee`, `jira_assignee_id`, `jira_link_count`, `jira_attachment_count`, `mr_url` (when linked). | Mention beads: `gitlab_mr_url`, `gitlab_mr_iid`, `gitlab_project_id`, `mention_text`. MR-status updates touch `mr_state`, `mr_merged`, `mr_pipeline_status`, `mr_pipeline_url` on whichever bead has the matching `mr_url`. | `slack_thread_channel`, `slack_thread_ts`, `slack_spawn_channel`, `slack_user_id`, `spawn_source`. (On the agent bead, not a thread bead.) |
| **Idempotency mechanism** | Startup `CatchUp` populates `tracked[jiraKey]→beadID` from all `source:jira` beads (jira_poller.go:171–210). Each poll checks the map; CreateBead also re-queries `Labels: ["jira:<KEY>"]` server-side as a race guard (jira_poller.go:472–483). | Webhook-side dedup map + SSE `isDuplicate(...)` with TTL (gitlab_sync.go:113–117). No CatchUp; no persistent track map. | In-memory `state.ThreadAgents[{channel}:{thread_ts}]` plus `spawnInFlight` map gated by `b.mu`. Persisted to JSON. No bead-level idempotency because there is no thread bead. |
| **Sync direction (today)** | **Bidirectional** — claim comments, status transitions, optional self-assign, MR remote-link writes, close comments (jira_sync.go:171–250). | **Read-only on MR status.** Updates bead `mr_state`/`mr_merged`/pipeline fields from GitLab; does **not** write bead changes back to GitLab. Squawk-forwarder posts agent output as MR notes (agent-driven, not bead-driven). | Read of thread → bead-creation of agent. No "thread bead" exists to sync, so no sync-direction question on threads themselves. Per-message `slack-thread-reply` tracking beads are append-only. |
| **Direction of travel** (ka-0000003234) | Outbound jira writes being disabled → one-way enrichment. | Stay read-only; MR-description `<!-- gardenio -->` block defaults off, opt-in per namespace. | (This research is the prompt for the question.) |
| **Lifecycle trigger** | JIRA poller (default 60s; main.go:244–248) + `/webhook` for mentions + SSE on `beads.bead.updated`/`closed`. | Webhook (`/webhook`, main.go:138–147) + GitLab poller fallback (5m) + trigger engine (60s) + SSE on `beads.bead.updated`. | Slack Events API mention → in-process bridge dispatch. No poller; no `thread.updated` event. |
| **How agents reference it** | By bead ID; can discover by `Labels:["jira:<KEY>"]` or `Fields:{jira_key:…}`. Pre-built `view:jira:pending` and `context:jira-dispatcher` views (init.go:924–941). | Mention beads: bead ID (passed to spawn prompt at gitlab_mentions.go:307). MR-status beads: by `mr_url` field; no label-based bulk discovery. | Agent reads its own bead's `slack_thread_*` fields. To find prior thread context, it must `gb slack thread` → ingests messages directly from Slack each spawn (no bead-side cache). |
| **Cross-surface linking** | `mr_url` on the jira bead bridges JIRA↔MR. Beads-viewer can render the bead and follow typed-mention edges. | Trigger-action `create_bead` and mention beads attach via parent/blocks edges to the user's project epic, but there is no standalone MR bead to attach to. | Spawn lineage exists per agent bead, but multiple spawns in the same thread have no shared parent. No stable handle to attach MRs/decisions/follow-ups to a thread. |

**Code citations** (so each row is verifiable):

- jira-bridge fields: `gardenio/controller/cmd/jira-bridge/jira_poller.go:472–569,
  601–610`
- jira-bridge sync-back: `gardenio/controller/cmd/jira-bridge/jira_sync.go:150–250`
- gitlab-bridge mention beads: `gardenio/controller/cmd/gitlab-bridge/gitlab_mentions.go:206–246, 307`
- gitlab-bridge MR-status updates: `gardenio/controller/cmd/gitlab-bridge/gitlab_sync.go:88–151`
- slack-thread bead-field write: `gardenio/controller/internal/bridge/bot_mentions.go:801–835, 848`
- slack-thread validation loop: `gardenio/controller/internal/bridge/bot_thread_validate.go:32–73`
- read-only-mirror direction-of-travel: advice ka-0000003234

## 3. Reliability gains a slack-thread intake bead would unlock

Each gain below is a **failure mode that currently exists in code**, with the
specific mechanism today and what a thread-level intake bead would change. This
section makes no design claim about *how* such a bead should look — only what it
would mitigate.

### 3.1 Thread context is re-derived from Slack on every spawn

**Today.** `gb slack thread` (slack.go) calls
`/api/slack/threads?channel=…&ts=…&limit=N`, which calls Slack's
`conversations.replies` (bot_threads.go:40–44). Every spawn — including
respawns after compaction or pod restart — fetches the entire thread from
Slack and the agent re-reads it. There is no bead-side snapshot the agent
can read instead.

**With a thread bead.** Decisions, MR links, follow-ups, and prior agent
conclusions could live as comments/edges on a single thread bead, recoverable
without a Slack round-trip.

### 3.2 Agents lose visibility of decisions made by previous agents in the same thread

**Today.** Each thread spawn produces a new `type=agent` bead. There is no
parent/child or typed-mention edge between agent N and agent N-1; only the
`(channel, thread_ts)` tuple ties them together (`state.ThreadAgents` map).
Decision beads created by a prior agent are auto-linked to that prior agent's
bead, not to "the thread", so a fresh agent reading its own bead does not
reach them via dependency edges.

**With a thread bead.** Decisions could parent on the thread bead, surfacing
to every subsequent agent via standard `kd dep` traversal — and to humans via
beads-viewer.

### 3.3 No durable handle to attach MRs / decisions / follow-up tasks to a thread

**Today.** When a thread agent opens an MR, calls `gb mr link`, files a
follow-up task, or makes a `gb decision`, those artefacts attach to the
*agent bead*, not the thread. Once the agent finishes and exits, the next
spawn has no first-class link from "this thread" to "the MR that was opened
last time". Agents work around this by re-reading thread messages and parsing
permalinks.

**With a thread bead.** MRs, decisions, and follow-up tasks could attach to
the thread bead via typed mentions, giving every subsequent agent a single
hop to all derived work.

### 3.4 Concurrent spawns are guarded only at the in-memory layer

**Today.** The `spawnInFlight[channel+":"+threadTS]` map plus
`state.GetThreadAgent` is the only deduplication. Both live inside the bridge
process. A bridge restart between the read of `state.ThreadAgents` and the
`CreateBead` call would race — though the validator and reconcile-on-startup
path will ultimately sweep duplicates. There is no DB-level uniqueness on
`(slack_thread_channel, slack_thread_ts)`.

**With a thread bead.** A unique label like `slack-thread:<channel>:<ts>` (or
the equivalent of jira-bridge's `jira:<KEY>` server-side `ListBeadsFiltered`
race-guard at jira_poller.go:472–483) would push idempotency into the
database, surviving bridge restarts and crash-windows.

### 3.5 Thread context disappears after compaction or transcript clear

**Today.** Agent transcripts compact aggressively. Without a bead snapshot,
post-compaction the agent recovers via `gb prime` + `gb slack thread`, which
re-fetches Slack messages but cannot recover the *bead-graph context* that
agent N built (decisions weighed, options rejected, MRs attempted but not
landed).

**With a thread bead.** Comments + linked beads on the thread bead survive
compaction — the agent reads the thread bead and sees the journey, not just
the messages.

### 3.6 Beads-viewer cannot navigate from a Slack conversation to derived work

**Today.** Per advice ka-0000000831, beads-viewer is now user-facing. A user
opening beads-viewer cannot start at "the Slack conversation about X" and
fan out to all derived work — they must already know the agent bead ID, and
even then they only see one spawn's lineage. Multiple spawns in the same
thread look like unrelated work.

**With a thread bead.** The thread bead becomes the navigable root: every
agent spawn is a child, every decision/MR/follow-up is one or two hops away.

### 3.7 No symmetry with jira/gitlab in cross-surface tooling

**Today.** Tools like `view:jira:pending`, `context:jira-dispatcher`
(init.go:924–941), or any future "all open MRs" view rely on the
`source:jira` / `mr_url` axis. Slack threads have no equivalent — there is
no "all open Slack-thread workitems" filter possible because there is no
per-thread bead carrying a status field.

**With a thread bead.** A `source:slack` filter or `view:slack:pending`
becomes mechanical, mirroring the jira/gitlab pattern.

---

## 4. Design — proposed slack-thread intake bead shape

Phase-2 design. The shape rhymes deliberately with jira-bridge (the closer
analogue — jira creates one bead per upstream ticket; gitlab-bridge creates
mention beads, not per-MR beads). All field and label names below match the
existing slack-bridge convention (`slack_thread_*`) so the bridge code can
reuse string keys it already emits.

### 4.1 Bead type — reuse `task`

| Option | Pros | Cons |
|---|---|---|
| **Reuse `task` (recommended)** | Zero migration cost; existing kd CLI / beads-viewer / inbox / ready-queue surfaces work out of the box; mirrors jira-bridge which uses `task`/`bug`/`feature` reflecting the upstream type. | "Task" is a slight semantic stretch — a thread is more of a conversation than a unit of work. Mitigated by `source:slack` label disambiguating in viewer/CLI. |
| New type (`thread`) | Type field carries semantic intent; would let beads-viewer treat thread cards distinctly. | Schema/proto/migration churn across kbeads, daemon, viewer, CLI; surfaces (inbox, ready, search) need to learn a new type; gives no new capability that a label can't express. |
| Reuse `agent` | Avoids a second bead per spawn. | `agent` is per-pod; threads outlive agents. Wrong granularity. |

**Recommendation: `type=task`.** Same rationale as jira-bridge. The
distinguishing axis is `source:slack`, not the type field.

### 4.2 Labels

Recommended label set on a thread bead:

| Label | Purpose | Mirrors |
|---|---|---|
| `source:slack` | Marks the bead as a one-way mirror of an upstream Slack thread. Drives `view:slack:*` surfaces, beads-viewer source-icon selection, and read-only-mirror semantics per ka-0000003234. | `source:jira` (always-on for jira beads) |
| `slack-thread:{channel}:{thread_ts}` | Stable upstream-identity label. Server-side `ListBeadsFiltered(Labels: …)` race-guard for idempotent CreateBead. | `jira:<KEY>` (jira_poller.go:472–483) |
| `project:<resolved>` | Routing — already produced by existing bridge logic that resolves project per channel/user. | jira-bridge's project label |
| `slack-channel:<channel-name>` *(optional, low priority)* | Human-friendly filter. Channel name can be resolved at ingest via `conversations.info`. | `jira-project:<key>` |

The existing `slack-thread` label on agent beads stays untouched — it remains
the marker for "this is a thread-spawned agent". Only the new thread bead
carries `source:slack`. This mirrors the gitlab pattern of one label-axis per
intent (action vs identity) rather than overloading.

### 4.3 Key metadata fields

All fields go on the thread bead's `Fields` map (string→string), matching the
existing slack-bridge convention. Bridge writes are append-style (`UpdateBeadFields`
with the touched key) so concurrent updaters do not stomp.

| Field | Required | Source | Example |
|---|---|---|---|
| `slack_thread_channel` | ✅ | `ev.Channel` (or webhook event) | `C07K1RLQAKF` |
| `slack_thread_ts` | ✅ | `ev.ThreadTimeStamp` | `1700000000.000001` |
| `slack_permalink` | ✅ | `chat.getPermalink` API call | `https://piheal.slack.com/archives/C07K…/p1700000000000001` |
| `slack_workspace_id` | ✅ | `ev.Team` or workspace config | `T012345` |
| `slack_opener_user_id` | ✅ | Slack user ID of root-message author | `U0OPERATOR1` |
| `slack_opener_operator_id` | optional | Resolved `operators.id` once Option B P2 lands; nullable today | `42` |
| `slack_root_text` | ✅ | First ~280 chars of root message text, sanitized | `"@gardenio we have a 'missing rhyme' in the slack thread model.."` |
| `slack_root_at` | ✅ | RFC3339 timestamp of root message | `2026-05-08T17:43:55Z` |
| `slack_last_activity_at` | bridge-maintained | RFC3339 of latest reply seen | `2026-05-08T18:30:12Z` |
| `slack_last_activity_ts` | bridge-maintained | Slack `ts` of latest reply | `1700001234.000200` |
| `slack_message_count` | bridge-maintained | Cached reply count (incl. root) | `7` |
| `slack_thread_status` | bridge-maintained | `open` \| `active` \| `stalled` \| `closed` (bridge-derived; see §4.4) | `active` |
| `spawn_source` | static | `slack-thread` | `slack-thread` |

Existing per-spawn `type=agent` beads keep their existing fields verbatim. The
thread bead is **additive**, not a replacement.

### 4.4 Lifecycle

**Created** on the first `@gardenio` mention in a thread — *not* on every Slack
message in the thread. Mirrors jira-bridge, which creates the bead on first
sync rather than first comment. Rationale: agents only care about threads
agents have been invited into; pre-existing un-mentioned threads are noise.

```text
Slack mention arrives
  ├─ bot_mentions.go:583–618 takes b.mu, dedup-checks
  ├─ NEW: ListBeadsFiltered(Labels: ["slack-thread:{channel}:{ts}"])
  │   ├─ exists → reuse thread-bead-id
  │   └─ missing → CreateBead(type=task, labels=[…], fields=…) → thread-bead-id
  ├─ existing CreateBead path for the agent bead, with new typed mention
  │     parent: <thread-bead-id>
  └─ existing state.SetThreadAgent(channel, ts, agent) — kept as fast-path
```

**Updated** on:

- Every subsequent message in the thread → `slack_last_activity_at`,
  `slack_last_activity_ts`, `slack_message_count` bumped via
  `UpdateBeadFields`. (At v1, the bridge can update on every event; the writer
  is per-thread so the throughput is bounded.)
- Bridge's existing 6-hour validator (`bot_thread_validate.go`) → set
  `slack_thread_status = closed` if Slack reports thread deleted/archived.
- Last-activity heuristic (deferred to v2) → flip `active`↔`stalled` based on
  message age relative to a threshold.

**Closed (`status=closed` on the bead)** when:

- Slack thread is deleted/archived (validator-driven).
- An operator runs `kd close <thread-bead-id>` to signal "no further agent
  dispatching from this thread". Per ADR §5: the bead status reflects local
  intent, not Slack state — read-only-mirror semantics.

The thread bead is **never** auto-closed because an agent finished. Threads
outlive agents.

### 4.5 Sync direction — read-only mirror

Per the ADR in §5: the bead is a **read-only mirror** of the Slack thread,
following ka-0000003234. Bridge writes flow inbound only:

- **Inbound (bridge → bead):** root metadata at create; `slack_last_activity_*`
  and `slack_message_count` on each reply event; `slack_thread_status` from
  the validator.
- **Outbound (bead → Slack):** none. Closing the bead does not close the
  Slack thread (Slack has no "closed" concept anyway). Bead comments are not
  cross-posted to Slack — agents already use `gb slack reply` for that, and
  that path stays untouched.

Comments ingested into the bead (deferred — not in v1) would carry the
provenance prefix pattern from ka-0000003234:
`[slack:{channel}#{ts} by {user} at {time}] …`. v1 does **not** ingest replies
as bead comments — that's a v3 enrichment.

### 4.6 Relationship to spawn-lineage agent beads

The per-spawn `type=agent` bead (e.g. ka-0000001150 shape) is unchanged. New:
each agent bead gains a `parent: <thread-bead-id>` typed mention in its
description, so:

- `kd refs mentioned-by <thread-bead-id> --type parent` returns every agent
  spawned for the thread.
- Beads-viewer's constellation renders the thread bead as the hub of all
  agent spawns, and (once typed-mention edges land via plan
  `typed-mentions-graph-edges-and-cli.md`) of every decision/MR/follow-up the
  agents created during their spawns.
- Decision beads, follow-up tasks, and `gb mr link` artefacts created during a
  spawn can additionally typed-mention the thread bead (`related: <thread-bead-id>`)
  so they fan out from the thread, not just the per-spawn agent.

The in-memory `state.ThreadAgents[{channel}:{thread_ts}]` map stays as the
fast-path for spawn-time dedup; the bead-graph is the durable source of truth.
Bridge restart-recovery walks `Labels: ["slack-thread:*"]` (or queries by
field) instead of the JSON file to rebuild the map.

### 4.7 How thread agents discover and reference the thread bead

- **Pod env:** add `SLACK_THREAD_BEAD_ID` alongside the existing
  `SLACK_THREAD_CHANNEL` / `SLACK_THREAD_TS` env vars. Cold-start pods receive
  it via the `CreateBead` request fields path; pre-warmed pods get it via
  `daemon.UpdateBeadFields()` on the agent bead.
- **`gb slack thread`** — already reads `(channel, ts)` from env. Add a
  companion `gb slack thread-bead` that prints the thread bead ID, and a
  fallback in `gb` that does `daemon.GetBeadsFiltered(Labels: ["slack-thread:{channel}:{ts}"])`
  if the env var is empty.
- **Agent reasoning loop:** by convention, on startup the thread agent reads
  the thread bead's comments (decisions made by prior agents in this thread,
  MR pointers) and lists `kd refs mentioned-by <thread-bead-id>` to enumerate
  all derived work. This is the §3.1–§3.3 reliability gain made concrete.

### 4.8 Idempotency

`slack-thread:{channel}:{thread_ts}` label + server-side `ListBeadsFiltered`
race-guard — exactly the jira-bridge pattern at `jira_poller.go:472–483`.
Survives bridge restarts and crash-windows because the dedup key lives in
Postgres, not the bridge process. The existing `state.ThreadAgents` map +
`spawnInFlight` mutex stay as the in-process fast path.

## 5. ADR — read-only mirror vs bidirectional sync

> **Status:** Accepted. Recommendation = **read-only mirror**.
> **Date:** 2026-05-08. **Bead:** ka-0000003435 (this design). **Epic:** ka-0000006870.
> **Supersedes:** none (new surface). **Mirrors:** ka-0000003234.

### 5.1 Context

Slack threads are an external work-item surface — like Jira tickets and GitLab
MRs. The proposed thread bead (§4) mirrors a Slack thread into the beads
graph. The question is whether bead-side mutations (close, reopen, comment,
field-edit) should propagate back to Slack, or whether the bead is a one-way
mirror like jira/gitlab beads under ka-0000003234.

### 5.2 Options

#### Option A — Read-only mirror (recommended)

The bead is a **read-only mirror of upstream thread state**. Bridge writes
flow inbound only. `kd close <thread-bead>` records local intent ("agents
should stop dispatching from this thread") but takes no action against Slack.
Bead comments are native; they are never cross-posted to Slack.

**Pros:**
- **Vocabulary symmetry** with jira/gitlab — agents already learn "source:X
  beads are read-only mirrors" once. No special-case for slack.
- **No race conditions** — the bead's state machine has only one writer
  direction (bridge → bead). No reconciler needed.
- **No invented state** — Slack threads have no upstream "closed" concept.
  Pretending the bead can close a Slack thread invents semantics that don't
  exist on the source surface.
- **Slack-API blast radius is small** — bridge keeps the existing `chat.postMessage`
  surface for `gb slack reply` / `gb squawk`. No new Slack scopes needed.
- **Failure mode is benign** — if the bridge drops a `last_activity` update,
  the bead is mildly stale, not actively wrong.

**Cons:**
- Operators cannot "close out a thread" by closing the bead. (Mitigated:
  `kd close` records local intent, which is enough to suppress further spawns
  via a bridge-side check; the Slack thread is left alone, which is correct.)
- Bead → Slack auto-link-back of MRs ("agent ka-X opened MR !123") would have
  to keep flowing through `gb slack reply` / `gb squawk` — not via bead-graph
  mutation. That's how it already works.

#### Option B — Bidirectional

Bead-side mutations propagate back to Slack: close-the-bead posts a
"resolved" reply, comments cross-post, status field changes get reflected in
the thread.

**Pros:**
- Operators can drive thread state from the bead surface. (Niche — operators
  already have Slack open if they're triaging.)
- Could enable richer "agent-as-tracker" workflows (auto-post weekly summary
  to thread on bead update). Out of scope today.

**Cons:**
- **Inconsistent with ka-0000003234** — bidirectional jira and gitlab were
  the very surfaces that the global advice retired. Re-introducing it on
  slack would split the read-only-mirror story.
- **Race conditions** — bead-write and Slack-write would need a reconciler
  (which write wins on concurrent edits?), a tombstone protocol for bead-side
  comment deletes, and an idempotency layer for cross-post replies.
- **Bridge complexity** — every bead mutation (`kd update`, `kd comment add`,
  `kd close`) would need to fan a Slack write. Current bridge does not have
  this fan-out.
- **No upstream state model to mirror** — Slack threads are append-only
  message lists. There is no "closed" / "in-progress" status to bidirectionally
  sync. Any state we mirror is invented by us; "closed bead → posted resolved
  message" is a UX choice, not a sync.
- **Slack-API rate-limit and scope expansion** — write-on-mutate would push
  bot-token write volume up materially.

#### Option C — Hybrid (bead bridge writes are read-only; some bead actions trigger Slack notifications)

Like Option A but `gb decision report`, `kd close`, etc. fire a Slack
notification reply. Same race + complexity issues as Option B in miniature.
Strictly worse than Option A's existing `gb squawk` plumbing.

### 5.3 Decision

**Option A — read-only mirror, with a narrowly scoped bridge-derived status field.**

The thread bead is a one-way mirror of the Slack thread. The single departure
from "purely read-only" is `slack_thread_status`, which the bridge derives
(open/active/stalled/closed) from validator sweeps and last-activity
heuristics — bridge-derived enrichment, not bidirectional sync. `kd close`
records local intent only and does not touch Slack.

### 5.4 Rationale summary

1. Symmetry with ka-0000003234 and the jira/gitlab direction-of-travel: one
   advice covers all source-X beads.
2. The Slack surface has no upstream state machine to mirror; there is
   nothing for bidirectional sync to be *correct against*.
3. Existing agent-driven Slack write plumbing (`gb slack reply`, `gb squawk`)
   already covers the use-case bidirectional sync would target — "agent
   surfaced something to the thread". Adding a second path would duplicate
   it.
4. Read-only mirror keeps bridge code minimal — the dominant constraint
   per epic ka-0000006870 ("the slack-bridge code is large; minimize churn
   for the minimal first step").

### 5.5 Consequences

- A future operator request "make `kd close <thread-bead>` post a `done` reply
  to Slack" would file as a follow-up ergonomic helper (not a sync change). The
  helper would call `slack-bridge`'s existing `chat.postMessage` path explicitly,
  not piggyback on bead mutations.
- Comment ingestion (Slack reply → bead comment) is **not** part of this
  decision. It is a v3 enrichment; if added, it follows the ka-0000003234
  provenance-prefix pattern (`[slack:{channel}#{ts} by {user} at {time}] …`).
- Add a global advice bead at v1 ship: "treat `source:slack` thread beads as
  read-only mirrors" — single line addition to the ka-0000003234 corpus, or
  a fresh advice that names slack alongside jira/gitlab.

## 6. Migration plan

This section sketches the staged rollout. The detailed task breakdown,
sub-tasks, and dependency graph live in
[`gardenio/plans/slack-thread-intake-bead.md`](../../plans/slack-thread-intake-bead.md);
this section is the high-level shape. None of these steps are scheduled —
operator green-light is required before any of them moves out of "plan".

### 6.1 Step 1 — minimal first step: "threads visible as beads"

**Goal:** every new `@gardenio` mention in a thread produces a thread bead in
addition to the existing agent bead, with no behavioural change for agents.

**Scope:**

- Bridge code: in `bot_mentions.go:583–618` ahead of the agent-bead create,
  resolve-or-create the thread bead via `slack-thread:{channel}:{ts}` label
  race-guard. Add `slack_thread_*` and `slack_root_*` fields plus
  `source:slack`, `slack-thread:{channel}:{ts}`, and `project:<resolved>` labels.
- Add `parent: <thread-bead-id>` to the agent bead's description on create.
- Surface `SLACK_THREAD_BEAD_ID` to the pod env.
- `slack-thread-reply` tracking beads (bot_thread_forward.go) gain a
  `related: <thread-bead-id>` typed mention so they cluster with the thread.

**Out of scope at Step 1:** no field updates beyond create-time; no validator
status writes; no beads-viewer rendering changes; no agent-side UX additions
beyond env passing; no existing-bead backfill.

**Why this is "minimal":** every change is additive. Removing it is trivial
(stop creating thread beads; agent beads keep working; existing `state.ThreadAgents`
map keeps the spawn flow correct). The user-visible payoff is non-zero —
beads-viewer immediately shows threads as beads with correct lineage.

### 6.2 Step 2 — bridge maintains thread bead state

**Goal:** thread bead reflects current Slack state (last activity, message
count, status).

**Scope:**

- Wire `bot_thread_forward.go`'s reply-handling path to call
  `daemon.UpdateBeadFields(thread-bead-id, {slack_last_activity_*,
  slack_message_count})`.
- Wire `bot_thread_validate.go`'s 6-hour sweep to set `slack_thread_status =
  closed` when the validator flips an entry to deleted/archived.
- Add a stalled/active heuristic (configurable threshold; default off until
  operator dials it in).

### 6.3 Step 3 — beads-viewer integration

**Goal:** make thread beads first-class in the human-facing UI.

**Scope:**

- Source-icon for `source:slack` (matches existing `source:jira` /
  `source:gitlab` patterns in the viewer).
- Thread-bead detail page: render `slack_root_text`, link `slack_permalink`,
  show last-activity timestamp, render `slack_thread_status` chip.
- New saved view: `view:slack:open` — `Labels:["source:slack"]` filtered by
  `slack_thread_status != closed`.
- Inbox treatment: thread beads are eligible for the inbox category
  (`source:slack` → category=intake), routed to operator on `slack_opener_operator_id`
  once Option B P2 lands. Mirrors the jira/gitlab inbox category behaviour.

### 6.4 Step 4 — agent-side affordances

**Goal:** agents can navigate from "I am a thread agent" to "what work has
been derived from this thread" in one hop.

**Scope:**

- `gb slack thread-bead` — print thread bead ID; convenience wrapper.
- Agent prime nudge: include thread-bead ID and a one-line summary of recent
  decisions/MRs attached to it.
- Convention (advice + gardener prompts): when an agent files a follow-up
  task, decision, or `gb mr link`, also emit a `related: <thread-bead-id>`
  typed mention.

### 6.5 Step 5+ — deferred / optional

- **Backfill** of historic `slack-thread`-labelled agent beads into thread
  beads: low-priority. Can run as a one-shot admin command if/when symmetry
  with jira's catch-up matters.
- **Drop in-memory `state.ThreadAgents`** in favour of label lookup: bigger
  refactor; not needed until the JSON file is causing pain.
- **Comment ingestion** (Slack replies → bead comments with provenance
  prefix per ka-0000003234): heavy-weight; only if a downstream feature
  actually needs it.
- **`kd close <thread-bead>` honoured by bridge as "stop spawning from this
  thread":** ergonomic helper; one-line check in `bot_mentions.go` to skip
  spawn if thread bead is closed.

### 6.6 Explicit non-goals (from epic ka-0000006870)

- Bidirectional Slack writes (per ADR §5.3).
- Replacing the existing `state.ThreadAgents` map at any step in v1
  (additive only).
- Changing how thread agents are spawned. (Spawn flow stays the way it is;
  the thread bead is a sibling artefact.)
- Schema/migration changes to kbeads beyond the additive labels and fields
  already supported by the existing bead model.
- Beads3d work (deprecated repo per CLAUDE.md).
- Per-message intake beads (the existing `slack-thread-reply` tracking bead
  shape covers per-message tracking; this design is about thread-level
  beads).
- Auto-merging the thread bead with mention-context in DM channels (DMs
  follow a different routing path; out of scope).
