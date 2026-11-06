# Plan: slack-thread intake bead — minimal first-step rollout

> **Status:** plan-of-record. **Plan only — no code lands until the operator
> green-lights specific build steps via `gb decision`.** Branches off main;
> `agent/` prefix.
>
> **Tracking bead:** ka-0000006870 (research epic, parent of this plan).
> **Phase 1 audit:** ka-0000001756 — `gardenio/docs/research/slack-thread-intake-bead.md`
> §§1–3 (current state, jira/gitlab comparison, reliability gains).
> **Phase 2 design:** ka-0000003435 (this plan + research-doc §§4–6).
> **Builds on:**
> - Advice [ka-0000003234](#) — read-only mirror semantics for `source:*` beads.
> - Plan `gardenio/plans/typed-mentions-graph-edges-and-cli.md` — typed-mention
>   edges in beads-viewer (used by Step 3 rendering).
> - Closed reliability gains audit at research-doc §3.

This plan converts the §4 design + §5 ADR into a buildable sequence. It
deliberately **does not** schedule any of the steps below — each step ships
behind an explicit operator green-light. The plan exists so when the operator
says "go", the ordering, scope, and dependencies are pre-thought-through.

## 1. Goal

Give Slack threads the same first-class bead-graph presence that Jira tickets
have today, while changing the spawn flow as little as possible. Concretely:

1. Each `@gardenio`-mentioned Slack thread is a `task` bead with
   `source:slack` and `slack-thread:{channel}:{ts}` labels.
2. Every per-spawn `type=agent` bead has `parent: <thread-bead-id>` typed
   mention.
3. Decisions, MRs, and follow-up tasks created during a thread spawn can
   discover the thread bead in one hop and attach to it via typed mentions.
4. Beads-viewer renders `source:slack` threads as a navigable hub.

The success criterion for the **minimal first step** is **#1 alone** —
threads visible as beads, with `parent` lineage from agent beads. Everything
else stages after operator green-light on Step 1.

## 2. Sequencing checklist

The plan runs in five stages. Stage 1 is the **minimal first step**; stages
2–5 are independently sequenced post-Step-1 follow-ups.

### Stage S1 — minimal first step: threads visible as beads

| Predicate | How to check | Owner |
| --- | --- | --- |
| **S1.a** Bridge resolves-or-creates a thread bead on `@gardenio` mention with `source:slack` + `slack-thread:{channel}:{ts}` labels | Send a fresh `@gardenio` thread mention; `kd list -l 'slack-thread:{channel}:{ts}'` returns one bead | inbox-track child of [ka-0000006870](#) |
| **S1.b** Idempotent on repeated mentions: a second `@gardenio` in the same thread reuses the existing thread bead, no duplicate created | Send two mentions in same thread; only one `slack-thread:` label exists | same |
| **S1.c** Each `type=agent` bead spawned for the thread has `parent: <thread-bead-id>` in description and the typed-mention edge resolves | `kd refs mentioned-by <thread-bead-id> --type parent` lists the spawn agents | same |
| **S1.d** `slack-thread-reply` tracking beads gain `related: <thread-bead-id>` typed mention | `kd refs mentioned-by <thread-bead-id> --type related` lists tracking beads | same |
| **S1.e** `SLACK_THREAD_BEAD_ID` is set in pod env for every thread spawn | `kubectl exec <pod> -- env \| grep SLACK_THREAD_BEAD_ID` | same |
| **S1.f** Bridge restart preserves thread-bead identity (label-lookup race-guard works after process restart) | Restart bridge; send another mention in pre-existing thread; no duplicate | same |
| **S1.g** Existing `state.ThreadAgents` JSON map continues to drive spawn dedup; in-memory fast-path unchanged | bot_thread_concurrent_test.go and bot_thread_validate_test.go pass | same |

Stage S1 is **the** check-able artifact for "this design is real". It is the
only stage that should land before promotion-style review of stages 2–5.

### Stage S2 — bridge maintains thread bead state

| Predicate | Spec source |
| --- | --- |
| **S2.a** Thread bead `slack_last_activity_at`, `slack_last_activity_ts`, `slack_message_count` updated on every reply event seen by `bot_thread_forward.go` | Research-doc §4.4 |
| **S2.b** Validator (`bot_thread_validate.go` 6h sweep) sets `slack_thread_status = closed` when Slack reports thread deleted/archived | Research-doc §4.4 |
| **S2.c** Optional active/stalled heuristic gated behind a config flag (default off) | Research-doc §4.4 |
| **S2.d** Update writes are idempotent and rate-bounded (per-thread writer goroutine; throttle on burst) | Research-doc §4.4 + slack-bridge throttle pattern |

### Stage S3 — beads-viewer integration

| Predicate | Spec source |
| --- | --- |
| **S3.a** beads-viewer renders `source:slack` icon on thread beads (parity with `source:jira`/`source:gitlab`) | Research-doc §6.3 |
| **S3.b** Thread-bead detail page renders `slack_root_text`, `slack_permalink` (clickable), last-activity timestamp, status chip | Research-doc §6.3 |
| **S3.c** New saved view `view:slack:open` filters `Labels:["source:slack"]` ∧ `slack_thread_status != closed` | Research-doc §6.3 |
| **S3.d** Thread bead is eligible for the inbox category (category=intake → operator routing on `slack_opener_operator_id`); requires Option B P2 (ka-0000003625) | Research-doc §6.3 + inbox-v1-option-b-sequencing.md |
| **S3.e** Constellation renders thread bead as hub with agent-spawn children fanning out via `parent` typed-mention edges (uses typed-mention edge work from `typed-mentions-graph-edges-and-cli.md` E1) | Research-doc §4.6 |

### Stage S4 — agent-side affordances

| Predicate | Spec source |
| --- | --- |
| **S4.a** `gb slack thread-bead` prints the thread bead ID for the running agent | Research-doc §4.7 |
| **S4.b** `gb` CLI fallback: when `SLACK_THREAD_BEAD_ID` is empty, lookup by label `slack-thread:{channel}:{ts}` | Research-doc §4.7 |
| **S4.c** Agent prime nudge includes thread-bead ID + one-line summary of recent decisions/MRs attached (via `kd refs mentioned-by`) | Research-doc §4.7 |
| **S4.d** Gardener prompt + global advice nudges agents to typed-mention the thread bead from any decision/MR/follow-up they file | Research-doc §4.6 + advice corpus pattern |

### Stage S5 — deferred / optional follow-ups

| Stage | Description | Trigger |
| --- | --- | --- |
| **S5.a** | Backfill historical `slack-thread`-labelled agent beads into thread beads (one-shot admin command) | Operator request only |
| **S5.b** | Drop `state.ThreadAgents` JSON in favour of label-lookup; refactor restart-recovery | Pain signal from JSON file (none today) |
| **S5.c** | Slack reply → bead comment ingestion with `[slack:{channel}#{ts} by {user} at {time}]` provenance prefix | Downstream feature requires it |
| **S5.d** | `kd close <thread-bead>` honoured by bridge as "stop spawning from this thread" | Operator ergonomic request |
| **S5.e** | Per-channel routing rules: `source:slack` + channel label → project/operator override | Operator request |

## 3. Detailed scope per stage

### S1 — minimal first step

**Gardener role:** crew (delivery)
**Repo:** gardenio monorepo
**Branches off:** main, prefix `agent/`
**Rough sizing:** ~1 crew session (one MR; touches 4 files in
`gardenio/controller/internal/bridge/`, plus tests; no schema or proto changes).

**Code locations to touch:**

| File | Change |
| --- | --- |
| `gardenio/controller/internal/bridge/bot_mentions.go` (lines 583–618 and 801–848) | Insert resolve-or-create thread-bead step ahead of agent-bead create. Pass thread-bead-id into the agent-bead `CreateBead` Fields and into the agent-bead description as `parent: <id>` typed mention. |
| `gardenio/controller/internal/bridge/bot_thread_forward.go` | When creating `slack-thread-reply` tracking beads, look up thread-bead-id (label query) and add `related: <thread-bead-id>` to description. |
| `gardenio/controller/internal/bridge/state.go` | Optional: extend restart-recovery walk to additionally resolve thread-bead-id from the label (so thread-bead lookup is process-local cached). |
| `gardenio/controller/internal/bridge/bot_thread_concurrent_test.go` | Add coverage: two concurrent `@gardenio` mentions in the same thread produce one thread bead, two agent beads. |
| `gardenio/controller/internal/bridge/bot_thread_validate_test.go` | Add coverage: validator does not duplicate or delete thread beads on routine sweeps (S1 leaves thread-bead `status` open until S2). |
| New: `gardenio/controller/internal/bridge/thread_bead.go` | Helper: `getOrCreateThreadBead(ctx, channel, ts, opener, rootMsg) (beadID, error)`. Encapsulates the label race-guard pattern from jira_poller.go:472–483. |
| `gardenio/controller/cmd/gb/slack.go` | Surface `SLACK_THREAD_BEAD_ID` env reading + fallback by label query. |

**Out of scope at S1** (lifted from research-doc §6.1):

- Field updates beyond create-time → S2.
- Validator status writes → S2.
- Beads-viewer rendering changes → S3.
- Agent-side UX additions beyond env passing → S4.
- Backfill of existing agent beads → S5.

**Acceptance:** all S1.a–S1.g predicates checkable; `make build && make test`
green; one fresh thread mention in gardenio-rwx Slack produces one thread bead
+ one agent bead with `parent` linkage in beads-viewer.

### S2 — bridge maintains thread bead state

**Gardener role:** crew (delivery)
**Branches off:** main; depends on S1 merged.
**Rough sizing:** ~1 crew session; touches 2 files (`bot_thread_forward.go`,
`bot_thread_validate.go`); adds throttle goroutine; adds tests.

**Detail:** see research-doc §4.4 lifecycle table.

### S3 — beads-viewer integration

**Gardener role:** crew (delivery)
**Branches off:** main; **soft-depends** on `typed-mentions-graph-edges-and-cli.md`
E1 for hub-style constellation rendering. The icon/permalink/saved-view bits
do not depend on E1.
**Rough sizing:** ~1 crew session; touches `gardenio/controller/cmd/beads-viewer/`
templates and `constellation.go`. New `view:slack:open` saved view filed via
`kd formula apply`.

**Detail:** see research-doc §6.3.

### S4 — agent-side affordances

**Gardener role:** crew (delivery) for CLI; gardener (advice) for prompts.
**Branches off:** main; depends on S1.
**Rough sizing:** ~0.5 crew session for CLI; the advice + prompt updates are a
separate beads-only change.

**Detail:** see research-doc §6.4.

### S5 — deferred / optional

Each S5.* is independently filable as needed. Each lands as its own MR; none
are scheduled unless a downstream feature or operator request triggers them.

## 4. Sub-task breakdown (rough sizing, dependencies)

> Filed as beads only after operator green-light on this plan. The IDs below
> are placeholders; actual bead IDs will be allocated at file time.

| ID (placeholder) | Stage | Title | Sizing | Depends on |
| --- | --- | --- | --- | --- |
| `T-S1-1` | S1 | Implement `getOrCreateThreadBead` helper and wire it into `bot_mentions.go` ahead of agent-bead create | M | — |
| `T-S1-2` | S1 | Add `parent: <thread-bead-id>` typed mention to agent-bead description on create | S | T-S1-1 |
| `T-S1-3` | S1 | Surface `SLACK_THREAD_BEAD_ID` env on pod create (cold-start + pre-warm `UpdateBeadFields` paths) | S | T-S1-1 |
| `T-S1-4` | S1 | Add `related: <thread-bead-id>` typed mention to `slack-thread-reply` tracking beads in `bot_thread_forward.go` | S | T-S1-1 |
| `T-S1-5` | S1 | Tests: bot_thread_concurrent_test.go + bot_thread_validate_test.go cover dedup, restart-recovery, no-spurious-deletes | M | T-S1-1..4 |
| `T-S1-6` | S1 | New global advice: "treat `source:slack` thread beads as read-only mirrors" (extend ka-0000003234 corpus or file companion) | S | T-S1-1 (so the advice can name the surface that exists) |
| `T-S2-1` | S2 | Wire reply-event path to `UpdateBeadFields(slack_last_activity_*, slack_message_count)` with per-thread throttle | M | S1 merged |
| `T-S2-2` | S2 | Wire 6h validator to write `slack_thread_status=closed` on deleted/archived sweeps | S | T-S2-1 |
| `T-S2-3` | S2 | Optional active/stalled heuristic behind config flag, default off | S | T-S2-1 |
| `T-S3-1` | S3 | beads-viewer source-icon for `source:slack` | S | S1 merged |
| `T-S3-2` | S3 | Thread-bead detail page renders root_text, permalink, last-activity, status chip | M | S2 merged |
| `T-S3-3` | S3 | Saved view `view:slack:open` via `kd formula apply` | S | S2 merged |
| `T-S3-4` | S3 | Inbox category routing for `source:slack` thread beads | M | S2 merged + Option B P2 (ka-0000003625) |
| `T-S3-5` | S3 | Constellation hub rendering tweak (typed-mention edges to spawned agents) | S | typed-mentions-graph-edges-and-cli.md E1 merged |
| `T-S4-1` | S4 | `gb slack thread-bead` CLI subcommand + label-fallback in slack.go | S | S1 merged |
| `T-S4-2` | S4 | Agent prime nudge: include thread-bead ID + recent-derivations summary | S | T-S4-1 |
| `T-S4-3` | S4 | Gardener prompt + global advice updates: typed-mention thread bead from decisions/MRs/follow-ups | S | T-S4-1 |
| `T-S5-*` | S5 | Filed on demand only (see §3 above). Not pre-scheduled. | varies | per S5 entry |

Sizing: **S** = ≤ 0.5 crew session, **M** = ~ 1 crew session, **L** = > 1 (none here).

## 5. Migration / backfill story

Per research-doc §6.5, the **default v1 stance is no backfill**. Pre-existing
threads remain represented by their existing `type=agent` beads (with the
`slack-thread` label). Only threads that receive a *new* `@gardenio` mention
post-S1 ship get a thread bead.

**If a backfill is later required** (S5.a):

1. Walk `Labels:["slack-thread"]` `type=agent` beads.
2. Group by `(slack_thread_channel, slack_thread_ts)` field tuple.
3. For each group, create the thread bead idempotently (label race-guard
   stops it duplicating if another mention already created one).
4. Add `parent: <thread-bead-id>` typed mention to each agent bead in the
   group via `kd update --description` (additive; existing description
   preserved).
5. Record metrics: rows processed, thread beads created, agent beads
   updated, duplicates skipped.

Estimated row-count: bounded by historical thread-spawn count. As of
2026-05-08, beads with `slack-thread` label: bounded by `kd list -l
slack-thread --status closed,open | wc -l` — likely in the low thousands.
A single admin command run is sufficient; no incremental backfill needed.

## 6. Open questions

The build cannot start cleanly until these resolve. Each is a candidate
`gb decision create` once the operator green-lights this plan as a whole.

### OQ-1. Is `task` the right type, or should we wait for a `thread` type?

Plan picks `task`. Operator confirmation requested before S1 lands. Trade-off
in research-doc §4.1.

### OQ-2. Should `kd close <thread-bead>` actually suppress further agent dispatch from that thread?

Listed as S5.d (deferred). Picking "yes at v1" would move it into S1 and
require the bridge to consult the thread bead's status at spawn time. Picking
"no" keeps S1 minimal. **Plan recommends: defer.** Operator confirmation
requested; not blocking.

### OQ-3. Should the bridge resolve and stamp `slack_opener_operator_id` at v1?

Requires Option B P2 ([ka-0000003625](#)) to have shipped operator-FK writes.
If P2 has not landed when S1 starts, leave the field empty and stamp it later
at S2/S3 (when operator-routing inbox category fires). **Plan: leave empty
at S1 unless P2 has already shipped.** No coordination needed; bridge writes
are additive.

### OQ-4. Is `slack-channel:<channel-name>` worth ingesting?

Optional label per research-doc §4.2. Adds a `conversations.info` API call at
create-time, low cost. Filter convenience in beads-viewer / kd. **Plan:
include if the resolver path already exists; otherwise defer.** Operator
confirmation not strictly required.

### OQ-5. Does S3.d (inbox routing) need its own design pass?

The inbox category for `source:slack` has the same shape as `source:jira`,
but the "owner" routing field (`slack_opener_operator_id` vs `jira_assignee_id`)
diverges. **Plan: file a 1-page design note inside S3 scope before T-S3-4
lands.** Mirrors how inbox-v1-option-b-sequencing.md treated the resolver
question.

### OQ-6. Should the global advice (T-S1-6) extend ka-0000003234 or be a fresh advice?

ka-0000003234 currently names jira/gitlab only. Two options: (a) edit
ka-0000003234's title and body to add Slack; (b) file a sibling advice that
points back to it. **Plan: option (a)**, since ka-0000003234 explicitly
generalises "treat source:X beads as read-only mirrors". Operator
confirmation not strictly required; either choice is fine.

## 7. Sequencing summary diagram

```
   [Phase 1 audit, MERGED]
            │
            ▼
   [Phase 2 design + plan, THIS MR]
            │ operator green-light required
            ▼
   S1   minimal first step ───── threads visible as beads
            │
            ▼ (independent post-S1; can run in parallel)
   ┌────────┼────────┬────────┐
   ▼        ▼        ▼        ▼
   S2       S3       S4       S5  (deferred / on-demand)
   state    viewer   agent
   updates  render   UX
```

## 8. Out of scope

- **Implementation of any stage.** Plan only; each stage ships behind a
  separate operator green-light.
- **Bidirectional Slack writes** (per ADR §5.3 in research doc).
- **Replacing `state.ThreadAgents` JSON map** at any v1 stage (additive only).
- **Changing how thread agents are spawned** (spawn flow stays as-is; thread
  bead is a sibling artifact).
- **Schema/migration changes** to kbeads beyond the additive labels and
  fields already supported.
- **`beads3d/`** (deprecated repo per CLAUDE.md).
- **Per-message intake beads** (existing `slack-thread-reply` tracking
  pattern covers this; out of scope for thread-level design).
- **DM (non-channel) routing** (DMs follow a different bridge path; out of
  scope for v1).

## 9. Files & paths (reference index)

| Concern | Path |
| --- | --- |
| Thread agent spawn | `gardenio/controller/internal/bridge/bot_mentions.go` (lines 556, 583–618, 801–848) |
| Thread reply forwarder | `gardenio/controller/internal/bridge/bot_thread_forward.go` |
| Thread validator | `gardenio/controller/internal/bridge/bot_thread_validate.go` |
| Thread state map | `gardenio/controller/internal/bridge/state.go` |
| Slack HTTP API for agents | `gardenio/controller/internal/bridge/bot_threads.go` |
| `gb slack thread` CLI | `gardenio/controller/cmd/gb/slack.go` |
| jira-bridge label race-guard pattern | `gardenio/controller/cmd/jira-bridge/jira_poller.go:472–483` |
| Read-only-mirror advice | bead `ka-0000003234` |
| Research doc | `gardenio/docs/research/slack-thread-intake-bead.md` (§4 design, §5 ADR, §6 migration) |
| Typed-mention edges in beads-viewer | `gardenio/plans/typed-mentions-graph-edges-and-cli.md` |
| Inbox routing background | `gardenio/plans/inbox-v1-option-b-sequencing.md` |
| Option B operator-FK substrate | bead [ka-0000003625](#) |
