# Jira conversational bridge — how @gardenio two-way works

**Epic:** ka-0000007401 — *"respond to @gardenio Jira mentions conversationally
via the jira-bridge, the same two-way experience as Slack."*

This is the operator/agent reference for the Jira conversational loop: what fires
when a human `@gardenio`s a ticket, how an agent talks back, and where each piece
lives in the code. It is deliberately short — the deep per-feature history is in
the epic's bundles (ka-0000006186, ka-0000000813, ka-0000007483).

> **Status (2026-08-08):** the full two-way loop is **merged and live in prod**.
> The `gardenio-mentions` webhook is registered and delivering real
> `comment_created` traffic (PE / DEVOPS tickets), HMAC is passing, and the
> mention handler is enabled on image `gardenio-jira-bridge:2026.220.x`. The only
> gate that ever held this back — the webhook + `agentTokenSecret` enablement in
> `acme-helm-chart` (G2, ka-0000003011) — is satisfied in prod. New tickets in an
> **un-listed project** are still deliberately *not* answered; see
> [Which projects agents act on](#which-projects-agents-act-on--and-why-not-wider).

## The loop at a glance

```text
human @gardenio a ticket
        │  (comment_created / comment_updated / issue_created / issue_updated webhook)
        ▼
jira-bridge JiraMentionHandler.HandleMention
        │  spawn-gate (deny-by-default: PE/DEVOPS/GBFSM; FSM & unknown blocked)
        │  optional "--role <name>" hint re-selects among the project's roles
        │  create task bead + spawn agent, bound to the ticket (jira_key)
        ├─▶ OnSpawn → JiraSpawnAcknowledger        ← posts "👋 On it" NOW
        ▼
agent works, then talks back:
        gb jira reply '<text>'   → POST /api/jira/reply  → internal note
        gb jira thread           → GET  /api/jira/thread → re-read the room
        gb jira show <KEY>       → GET  /api/jira/issue  → re-read the ticket body
        gb squawk '<progress>'   → SSE  → forwarded as an internal note
        │
        ▼ agent reaches its terminal close (done / failed / reaped)
        └─▶ SSE beads.bead.closed → JiraCompletionAcknowledger ← posts "✅ Done" / "❌ Failed"
        │
human comments again (follow-up)
        ▼
jira-bridge webhook: live agent → nudge; stopped agent → re-spawn
        (add "--fresh" to the mention to force a clean-context respawn)
```

Every outbound comment the bridge posts is an **internal note** (staff-only on
JSM), carries a `[gardenio agent]` prefix, and is stamped with an
`Acted by <operator>` attribution line so the ticket history stays honest about
which human drove the work despite the shared bot service account.

## The reaction lifecycle (Slack-card parity, both halves)

Slack walks the triggering message through
👀 (seen) → ⏳ (working) → ✅ (done) / ❌ (failed). Jira has no reactions, so the
bridge posts the equivalent as internal notes:

- **Front half — spawn ack.** `JiraSpawnAcknowledger`
  (`controller/internal/bridge/jira_spawn_ack.go`) is wired onto the mention
  handler's `OnSpawn` extension point and posts **one** note the instant a
  mention spawns an agent:

  > `[gardenio agent] 👋 On it — <agent> is picking this up.`
  > `Tracking bead: <ka-id>`
  > `I'll reply here as I make progress.`
  > `Acted by <operator>`

  Without it a `@gardenio`ed ticket stayed *silent* from the mention until the
  agent's first `gb jira reply` — minutes away, or never on an interrupted boot
  (common in this fleet). To the human it looked like the mention fell into a
  void.

- **Back half — completion ack.** `JiraCompletionAcknowledger`
  (`controller/internal/bridge/jira_completion_ack.go`) listens on the agent
  lifecycle **SSE stream** for the mention-spawned agent bead's terminal
  `beads.bead.closed` event (deduped — SSE can redeliver on reconnect) and posts
  a one-line outcome note, echoing the agent's wrap-up `tldr` when present:

  > `[gardenio agent] ✅ Done — <agent> finished work on this ticket.` *(clean finish)*
  > `[gardenio agent] ❌ Failed — <agent> stopped before completing.` *(crash)*
  > `[gardenio agent] ⚠️ <agent> was rate-limited and stopped before completing.`
  > `[gardenio agent] ⚠️ <agent> ended without wrapping up.` *(reaped)*

  Before this, a mention agent that crashed left the ticket stuck at "On it"
  forever. It supersedes the generic `jira_sync` "Task bead closed" note for
  mention-conversation agents so the ticket carries a real outcome signal.

Both acknowledgers respect the **same three constraints** (all pinned by tests):

- **No new write surface on the mention path.** Each is a *separate*
  collaborator, not a field on `JiraMentionHandler`, so the handler still holds
  no `*JiraClient` — `TestJiraMention_NoJiraWriteSurface` stays green. Both files
  are in `noBypassFiles`, so the source-level scan proves their comment calls are
  gate-guarded.
- **Same gates as every other outbound path.** They route through the shared
  `WritePolicyStore` reply-predicate verdicts (`AllowReplyProject` +
  `AllowReplyComment`): FSM and unknown projects are hard-blocked in every mode,
  GBFSM is eligible in both outbox-toggle states (an ack is not an FSM/outbox
  write — ka-0000007628), public comments are never posted, and a role with no
  comment grant (e.g. `fsm-triage`) posts no note. A nil store fails closed.
- **Never fails the run.** A blocked or errored note is logged and swallowed —
  the agent is already dispatched (or already terminal), so the note is UX, not
  control flow.

## Follow-ups: nudge, respawn, and `--fresh`

When a human comments again on a ticket that already had a mention agent, the
webhook follow-up loop (`jira_webhook.go` → `jira_agent_resolver.go`) resolves it:

- **Live agent → nudge.** The running pod is nudged with the new comment and its
  `jira_comment_id` reply anchor is advanced to the waking comment, so the next
  `gb jira reply` lands under the right comment.
- **Stopped agent → same-name respawn.** The stale terminal bead is closed and a
  same-named agent is respawned carrying the new anchor — session continuity.
- **`--fresh`** (mirrors Slack `@gardenio --fresh`, ka-0000008188): if the
  follow-up comment carries `--fresh`, a **respawn** starts with a clean
  conversation instead of resuming the prior session. It is **inert on the
  live-agent path** — a running pod cannot wipe its in-memory context, so the
  bridge logs that `--fresh` was ignored and delivers a normal nudge; stop the
  agent, then re-mention with `--fresh` to force a clean slate.

## Mention-time intent hints: `--role`, `--quiet`

An operator can steer the **first spawn** from the mention text itself, mirroring
the `--fresh` convention (design-doc F-E, epic ka-0000007401):

```
@gardenio --role reviewer please double-check this migration
@gardenio role:reviewer   please double-check this migration   ← equivalent
@gardenio --quiet just tell me the root cause                  ← low-noise ticket
```

### `--role`

- **What it does.** `--role <name>` (or `role:<name>` / `--role=<name>`) picks
  which agent **role** the mention spawns, instead of the project's default (the
  first enabled `spawn_agent` Jira trigger). Parsed on the first-spawn path in
  `spawnJiraMentionAgent` (`jira_mentions.go`) and resolved by
  `resolveJiraSpawnRoleHinted` (`jira_spawn_role.go`).
- **Deny-by-default — the hint can only *re-select*, never widen.** The requested
  role is honored **only** if some **enabled** `spawn_agent` trigger for **this
  project** already advertises it. An unknown/unconfigured/disabled/other-project
  role is **ignored** — resolution falls back to the project default, exactly as
  an un-hinted mention would, and logs
  `mention --role hint not a configured spawn role for project`. So `--role` can
  never conjure a role the project isn't already configured to spawn, and it does
  **not** touch the PE/DEVOPS/GBFSM spawn-project gate
  ([below](#which-projects-agents-act-on--and-why-not-wider)).
- **First-spawn only.** Like `--fresh` on a live agent, `--role` is a *spawn*
  decision: it steers the pod being minted. On the follow-up nudge/respawn path
  (`jira_agent_resolver.go`) the agent's role is already baked, so the flag has
  no effect there — the follow-up loop keeps calling the un-hinted
  `resolveJiraSpawnRole`.
- **Never leaks into the prompt.** The flag token is stripped from both the plain
  and markdown instruction before the task title or the agent prompt is built —
  even when the requested role is ignored — so the agent never reads
  `--role reviewer` as part of its instruction.

### `--quiet`

`@gardenio --quiet <text>` asks for a **low-noise ticket**: the agent still does
the work and posts its explicit `gb jira reply` answer, but the three AUTOMATIC
progress-note forwarders are suppressed so the ticket is not bracketed with
lifecycle chatter.

- **What it silences.** All three automatic notes:
  - `JiraSpawnAcknowledger` — the "👋 On it" spawn note (`jira_spawn_ack.go`).
  - `JiraSquawkForwarder` — the internal-note mirrors of the agent's squawks
    (`jira_squawk_forward.go`).
  - `JiraCompletionAcknowledger` — the terminal "✅ Done" / "❌ Failed" note
    (`jira_completion_ack.go`).
- **What it does NOT silence.** An explicit `gb jira reply` from the agent — the
  operator asked a question and still gets the answer. `--quiet` only affects the
  *automatic* lifecycle notes, not direct replies.
- **How it is carried.** Parsed on the first-spawn path in `spawnJiraMentionAgent`
  (`jira_mentions.go`) by `parseQuietFlag`, exactly like `--fresh`. A true value
  stamps `jira_quiet=true` on **both** the mention task bead and the spawned agent
  bead. The spawn-ack reads it off the in-process spawn event (`JiraMentionSpawnEvent.Quiet`);
  the two SSE-driven forwarders — which resolve the ticket solely from the acting
  agent's own bead — read `jira_quiet` off that bead via `jiraQuietSuppressed`.
- **Observability.** A spawn-ack suppressed by `--quiet` increments
  `gardenio_jira_spawn_ack_total{outcome="quiet"}`, distinct from `posted` /
  `blocked` / `error`, so a quiet ticket is legible on the Jira conversation-health
  panel rather than looking like a silent gap.
- **Never leaks into the prompt.** Like `--role`, the `--quiet` token is stripped
  from both the plain and markdown instruction before the task title or agent
  prompt is built, so the agent never reads `--quiet` as instruction text.
- **First-spawn only.** `--quiet` is a *spawn* decision stamped on the agent bead
  at mint time; a follow-up mention does not re-toggle it.

> **Not yet built (design-doc F-E remainder):** `--public` (request a
> customer-visible reply — a *request* that still hits the deny-by-default role
> gate, a security decision, not a toggle). `--role` and `--quiet` ship today.

## Durability (transient-daemon retries — Slack parity)

The conversational writes are single points of failure — a daemon blip during a
rollout/restart used to silently drop a human's follow-up or fail to wake the
agent, and the webhook returns 200 either way. Two bounded-retry layers close
that (ported from Slack's `thread_reply_dlq.go` / `retryTransientDaemon`):

- **Inbound comment mirror** (`jira_inbound_comment_retry.go`, ka-0000004763):
  wraps the two comment-mirror writes (`AddComment` + the
  `jira_has_comments`/`jira_comment_id` field update) in a bounded retry
  (3 attempts, 0.3s→0.6s). Deterministic 4xx (except 429) is **not** retried.
- **Follow-up wake** (`jira_wake_retry.go`, ka-0000002721): wraps both wake ops
  (live-agent nudge, stopped-agent respawn) in `retryJiraWake`. Deliberately
  **not** `NudgeAgentWithBootRetry` — the wake is synchronous in the webhook
  handler and a ~95s backoff would risk Jira auto-disabling the webhook.

Outbound replies are protected against the *opposite* failure — a lost-200 retry
posting a **duplicate** customer-visible comment — by the reply deduper
(`jira_reply_dedup.go`, ka-0000007243): a collapsed retry returns idempotent
success without re-posting; a genuinely failed post releases its slot so a
legitimate retry still lands.

## `comment_deleted` — explicit no-op

`jira_webhook.go` handles `comment_deleted` as an explicit, logged no-op
(GAP 5, ka-0000008075): the bead conversation is an append-only mirror, so a
deleted source comment is **not** retracted from the bead, the `jira_comment_id`
reply anchor is left intact (clearing it would expand the guarded write surface
for a rare, self-healing degradation), and the event still records
`webhookOutcomeIgnored` so the deadman monitor counts it as a live authenticated
delivery. The behavioral delta vs. the anonymous default-ignore is one
operator-visible Info log, so a deleted `@gardenio` comment is traceable instead
of vanishing.

## Observability (metrics)

The conversational loop is instrumented so an operator can see it working
without reading pod logs (agents can't). All label sets are bounded enums.

| Metric | Labels | What rising means |
|---|---|---|
| `gardenio_jira_mention_spawn_total` | `outcome=ok\|error` | `error` = mentions arriving but agents not booting (conversation dies at the front half) |
| `gardenio_jira_spawn_ack_total` | `outcome=posted\|blocked\|error` | `error` = ticket stays silent despite a booted agent; `blocked` = write-policy/non-replyable refusal |
| `gardenio_jira_reply_total` | `outcome=posted\|blocked\|error`, `visibility=internal\|public\|none` | `posted{visibility=public}` = customer-facing reply volume; `blocked` = policy refusing replies |
| `gardenio_jira_followup_delivery_total` | `path=warm\|cold\|skipped` | `warm/(warm+cold)` = the Jira warm-hit ratio (live-nudge vs respawn), the conversational twin of `gardenio_mention_delivery_total` |
| `gardenio_jira_followup_wake_retries_total` / `_failures_total` | `stage=nudge\|spawn` | a follow-up wake needed retries / gave up (comment reached the bead but the agent was never woken) |
| `gardenio_jira_inbound_comment_write_retries_total` / `_failures_total` | `stage=add_comment\|set_fields` | a human follow-up mirror needed retries / was permanently dropped |
| `gardenio_jira_webhook_dark` (deadman) | — | no authenticated webhook delivery within the window — the hook is dark (deregistered, secret rotated, JQL emptied) |

## Feature inventory (where each piece lives)

| Capability | Entry point | Source |
|---|---|---|
| Spawn on `@gardenio` in a comment | webhook `comment_created` | `jira_webhook.go`, `jira_mentions.go` |
| Spawn on `@gardenio` in an **edited** comment | webhook `comment_updated` | `jira_webhook.go`, `jira_mentions.go` (`classifyPendingJiraMention` dedup) |
| Spawn on `@gardenio` in a new/edited description | `issue_created` / `issue_updated` | `jira_webhook.go`, `jira_mentions.go` (`MentionNewlyIntroduced`) |
| `comment_deleted` explicit no-op | webhook `comment_deleted` | `jira_webhook.go` (`handleJiraCommentDeletedWebhook`) |
| **Immediate spawn acknowledgment** | `OnSpawn` hook | **`jira_spawn_ack.go`** |
| **Completion / failure acknowledgment** | SSE `beads.bead.closed` | **`jira_completion_ack.go`** |
| Agent posts a reply | `gb jira reply` → `POST /api/jira/reply` | `cmd/gb/jira.go`, `jira_reply.go` |
| Reply idempotency (no dup on retry) | reply path | `jira_reply_dedup.go` |
| Agent re-reads the conversation | `gb jira thread` → `GET /api/jira/thread` | `jira_thread.go` |
| Agent re-reads the ticket body | `gb jira show` → `GET /api/jira/issue` | `jira_issue_read.go` |
| Agent status forwarded as a note | `gb squawk` (SSE) | `jira_squawk_forward.go` |
| Follow-up nudge / re-spawn / `--fresh` | webhook follow-up loop | `jira_webhook.go`, `jira_agent_resolver.go` |
| Inbound follow-up durability (retry) | webhook mirror writes | `jira_inbound_comment_retry.go` |
| Follow-up wake durability (retry) | wake path | `jira_wake_retry.go` |
| Rich markdown → ADF on replies | reply path | `jira_adf_markdown.go` |
| Per-project routing | prefix → boat project | `jira_mentions.go` (`resolveProjectFromJiraKey`) |
| Deny-by-default write + spawn policy | all outbound + spawn paths | `jira_write_policy.go`, `jira_allowlist.go` |
| Conversation metrics | all paths | `jira_conversation_metrics.go`, `jira_mention_metrics.go` |
| Webhook deadman (is the hook live?) | webhook | `jira_webhook_deadman.go` |

## Wiring (production)

Both acknowledgers are constructed in `cmd/jira-bridge/main.go`, but they attach
at **different points** — the spawn ack is a synchronous hook on the mention
handler, the completion ack is an async SSE subscriber:

```go
// Front half — threaded onto the mention handler's OnSpawn hook.
spawnAck := bridge.NewJiraSpawnAcknowledger(bridge.JiraSpawnAcknowledgerConfig{
    Jira:        jiraClient,   // same client as the reply/squawk paths
    WritePolicy: writePolicy,  // same shared store
    Logger:      logger,
})
mentionHandler := buildMentionHandler(cfg, daemon, jiraClient, logger, userDir, writePolicy, spawnAck)

// Back half — subscribes to the agent lifecycle SSE stream.
completionAck := bridge.NewJiraCompletionAcknowledger(bridge.JiraCompletionAcknowledgerConfig{
    Jira:        jiraClient,
    WritePolicy: writePolicy,
    Logger:      logger,
})
completionAck.RegisterHandlers(sseStream) // fires on the spawned agent bead's terminal close
```

A nil spawn acknowledger leaves `OnSpawn` nil — the legacy
silent-until-first-reply behavior — so unit tests and any bridge that omits it
are unaffected.

## Which projects agents act on — and why not wider

An `@gardenio` mention spawns an agent **only** on a ticket in one of three
projects: **`PE`, `DEVOPS`, `GBFSM`**. This is enforced by **two independent,
deliberately-mirrored deny-by-default gates** — knowing both is essential before
anyone tries to "widen" the surface:

1. **Inbound JQL** (Jira console) — decides whether Jira *delivers* the webhook.
   Currently `project in (PE, DEVOPS, GBFSM)`.
2. **Spawn gate** (bridge **code**) — decides whether a delivered mention
   *spawns*. `WritePolicyStore.AllowSpawn` → `projectSpawnable`, hardcoded to
   `{PE, DEVOPS, GBFSM}` in `DefaultWritePolicy`
   (`jira_write_policy.go`), wired at `cmd/jira-bridge/main.go`. Deny-by-default
   **and mode-independent** (not softened by shadow mode). An unlisted project
   logs `jira-mention: blocked mention on non-spawnable project` and drops.

Because the JQL and the code spawn-set are **already identical**, widening the
JQL alone is a **no-op that fails silently and worse than today**: Jira would
deliver mentions from the new project, the bridge would refuse to spawn at the
code gate, and `HandleMention` returns `true` (reported as *handled*, not as an
error) — so nothing spawns, nothing replies, and nothing signals failure.

> **Operator decision (Matthew.Baker, 2026-08-08 — authoritative).**
> The `PE/DEVOPS/GBFSM` scope is **correct and intended**, not an oversight. The
> question of widening was raised (decision ka-0000007763; task ka-0000004484),
> initially resolved `rescope-code`, then **superseded** the same day by a later,
> better-informed answer: **option C — do NOT widen.** Recorded on
> ka-0000004484 comment 162621 and the supersede note on ka-0000007763.
>
> - **`FSM` stays permanently blocked** (`blockedJiraProjects{FSM}` + absent from
>   the writable set). It is the customer-facing JSM service desk; FSM-bound
>   writes flow through **GBFSM** intents, never a raw mention reply.
> - Widening the spawn surface is a **code change + release + canonical-prod
>   deploy**, gated on a real answer to "which projects, and why is that safe?"
>   (the bot must also hold Add-Comment on the new project or the reply 403s).
>   It is *not* a console edit. Do **not** smuggle a widening into an unrelated
>   change, and do not add a project to `DefaultWritePolicy` without a fresh
>   operator decision.

`ProjectPolicy.Writable` is **dual-purpose**: `projectSpawnable()` (spawn + reply
pre-gate) checks `Writable` alone, while `projectWritable()` (outbound
poller/sync-back/squawk writes) checks `Writable` **and** the outbox toggle. So
naively adding a project as `Writable:true` to open spawning would *also* grant
outbound write eligibility. Any future config-driven widening must therefore
separate the spawn axis from the write axis (design sketch: ka-0000005656) — but
that remains **unbuilt by operator direction** above.

## Enabling / disabling the `comment_updated` edit path

Editing an existing comment to add `@gardenio` is handled by shipped, tested code
(`comment_updated` shares `handleJiraCommentWebhook` with `comment_created`; the
crosslink guard and bridge-own-comment guard cover the edit path; a re-added
already-present mention is caught by the 3-minute windowed dedup
`classifyPendingJiraMention`). It only *fires* if the `gardenio-mentions` webhook
is subscribed to **Comment: updated** in the Jira console
(acme.atlassian.net → Settings → System → WebHooks → `gardenio-mentions` →
Edit → tick *Comment: updated*). This needs Jira admin (browser session); the
`gardenio@acme.example` API token cannot create/update webhooks (403,
`ADMINISTER=false`) — so **do not file an access escalation for the token**;
it is an operator console action (tracked as ka-0000007501).

## Enablement note

The full stack ships in the gardenio chart and is **enabled in prod today** — the
webhook is registered, the `agentTokenSecret` is set, and real traffic flows
(G2, ka-0000003011). If the deadman metric (`gardenio_jira_webhook_dark`) fires or
`/api/jira/reply` logs `JIRA_BRIDGE_AGENT_TOKEN not set … unauthenticated`, the
hook or the token has regressed — see the enablement checklist on G2 and the
deadman alert in the alerting runbook.
