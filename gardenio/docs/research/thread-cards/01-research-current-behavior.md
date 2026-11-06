# Research: Agent Cards in User-Owned Slack Threads — Current Behavior & Seam Analysis

**Epic:** ka-0000001984 · **Origin thread:** ka-0000001240 · **Operator:** Matt Baker
**Date:** 2026-06-01 · **Author:** gardener thread-1780279536-660499

---

## 1. What Matt asked for

> "I love the agent card messages; I wish we had them on threads, but of course
> the top level message of a thread is the user's message... but what if in cases
> where the user owns the thread, we just put the card in the thread's messages
> as the agent's first message, and keep it updated and nice and such? So we can
> have nice tldr messages and stuff? It should be configurable too... it's a bit
> tricky to get perfect UX wise and it needs to be something we can roll back."

Three requirements fall out of this:

- **R1 — Card in thread:** For agents spawned from a Slack thread the user owns,
  post the rich agent card as the agent's **first reply** in the thread and keep
  it live-updated (tldr, status, accomplishments, MRs, wrap-up).
- **R2 — Configurable:** Toggleable behavior (per-deploy at minimum, ideally
  per-project).
- **R3 — Rollback-able:** A kill switch we can flip without a redeploy, because
  the thread UX is fiddly and we may need to back it out fast.

## 2. What the agent card *is* today

The agent card is a Block Kit message the Slack bridge posts and then live-edits
(`chat.update`) for the lifetime of an agent. It is built by:

- `buildAgentCardBlocks()` — full card for active agents
  (`gardenio/controller/internal/bridge/bot_agents.go:1561`)
- `buildCompactAgentCardBlocks()` — single-line compact form for finished agents
  (`bot_agents.go:1788`)
- `buildWrapUpAgentCardBlocks()` — wrap-up card with accomplishments / blockers /
  handoff / tldr at completion (`bot_agents_wrapup.go`)

Content shown: header (status emoji + subject/title + project + role + state),
context block (bead id, image tag, "Nm ago", bridge version, claimed task, cost,
spawn-vector line, trigger info), live tool-stats ("running commands… (44 tools)
[9m57s] · last: Bash(git status)"), and a "Clear" action for terminal cards.

**The tldr** Matt wants is already a first-class wrap-up field, promoted to a
header banner when present (`agents_card_test.go:584`). No new card content is
required — this feature is about **where** the card lives, not what it contains.

## 3. Card lifecycle (channel/scheduled agents — the working case)

1. **Post:** `ensureAgentCard()` (`bot_agents.go:366`) posts the card via
   `PostMessageContext` and records a `MessageRef`.
2. **Persist:** the `MessageRef{ChannelID, Timestamp, Agent, ThreadTS}` is stored
   in three places so it survives bridge restarts:
   - hot cache `b.agentCards[agent]` (`bot.go:90`)
   - state file `StateManager.AgentCards[agent]` (`state.go`)
   - agent bead fields `slack_card_channel` / `slack_card_ts`
     (`persistCardRef`, `bot_agents.go:~459-480`)
3. **Update:** `updateAgentCard()` → `flushAgentCard()` edits in place via
   `UpdateMessageContext` (`bot_agents.go:1219`), debounced
   (`cardUpdateMinInterval = 5s`) and globally rate-limited (`slackCardLimiter`).
4. **Wrap-up:** at terminal state, `replaceAgentCardWithWrapUp()` edits the same
   message into the wrap-up card.

This whole machinery is **edit-by-stored-ts** and already thread-agnostic — if a
card's `Timestamp` happens to be a threaded reply, the same update path works.

## 4. What happens for a thread-spawned agent TODAY (the gap)

`NotifyAgentSpawn()` (`bot_agents.go`) has an **early-return branch** for
thread-bound agents at lines **583–631**. It detects a thread agent three ways:

1. `resolveAgentThread(ctx, agent)` returns `(slackChannel, slackTS)`
   (`bot_agents.go:583`);
2. fallback to bead fields `slack_thread_channel` / `slack_thread_ts`
   (`bot_agents.go:586-590`);
3. fallback to `spawn_source` prefix `slack-thread` (`bot_agents.go:596-604`).

When matched, it:
- marks the agent thread-bound (`markThreadBound`, `bot_agents.go:611-613`);
- starts a **tool-stats streaming session** on the *user's* thread root message
  via `streamMgr.StartSession(...)` (`bot_agents.go:624`);
- sets a 👀 reaction on the user's message (`reactions.SetTarget`,
  `bot_agents.go:628`);
- **`return`s** (`bot_agents.go:631`) — *no card is ever posted in the thread.*

The only thread message the agent itself posts during the run is at the **end**:
`postThreadStateReply()` (`bot_agents.go:895`) posts the final status + wrap-up
as a fresh thread reply at done/failed, and tidies the spawn confirmation
message.

**Net effect today:** a user-owned thread gets 👀 → ephemeral tool-stat edits on
the user's own message → one final wrap-up reply. There is **no persistent,
live-updated card** in the thread while the agent works. This is exactly the gap
Matt is pointing at.

## 5. The seam — most of the plumbing already exists

`ensureAgentCard()` **already accepts an optional `inThreadTS`**:

```go
// bot_agents.go:366
func (b *Bot) ensureAgentCard(ctx context.Context, agent, channelID string, inThreadTS ...string) (string, error)
// bot_agents.go:429-438
if len(inThreadTS) > 0 && inThreadTS[0] != "" {
    parentTS = inThreadTS[0]
    opts = append(opts, slack.MsgOptionTS(parentTS))     // <-- posts as a thread reply
}
ref := MessageRef{ChannelID: cardChannel, Timestamp: ts, Agent: agent, ThreadTS: parentTS}
```

`MessageRef.ThreadTS` is purpose-built for this (`state.go`). And the existing
scheduled-agent path already demonstrates the exact pattern we want — it posts a
card and threads follow-ups under it (`bot_agents.go:645-676`). So the missing
work is small and well-contained:

1. In the thread-spawn branch, **call `ensureAgentCard(ctx, agent, channel, slackTS)`**
   to post the card as the first reply, instead of returning early.
2. **Reconcile with the streamMgr session** so tool-stats render on the *card*
   (use `StartCardSession(agent, channel, cardTS)` like the scheduled path,
   `bot_agents.go:660`) rather than on the user's message — avoids a double
   live-edit surface.
3. **Reconcile with `postThreadStateReply`** so we don't post a card *and* a
   separate terminal reply (decision deferred to the UX doc).
4. **Gate it** behind a new flag, default OFF (config + rollback doc).

## 6. Configurability precedent

Threading itself is already env-gated: `SLACK_THREADING_MODE`
(`cmd/slack-bridge/main.go:563`, default `"agent"`), surfaced as
`BotConfig.ThreadingMode` (`helpers.go:190`) and checked via
`agentThreadingEnabled()` (`bot_agents.go:40`, `== "agent"`). This is the natural
home for a new sub-mode/flag, and proves the rollback-by-env pattern. There are
also `bridge-notifications` config beads (`SuppressCostReporting`,
`SuppressToolStream`) read at bridge init — precedent for **bead-driven**,
no-redeploy toggles, which matters for R3.

## 7. "User owns the thread" — how we'd detect it

The thread bead records `slack_opener_user_id` (`thread_bead.go:77`) and mirrors
it to `slack_user_id`. The agent bead carries `slack_user_id` (the spawning
user). Matt's framing — "in cases where the user owns the thread" — maps to:
post a thread card **when the thread root was authored by the user who spawned
the agent** (`slack_opener_user_id == slack_user_id`). For bot-rooted or
pool-spawned threads we keep current behavior. (Final policy in the UX doc.)

## 8. Tests that will need extending

- `agents_card_test.go` — card block rendering (no change to content, but assert
  it renders in a thread context).
- `agents_card_wrapup_edit_test.go` — `ThreadTS` populated when posted in-thread;
  state round-trips `ThreadTS`; wrap-up replacement resolves the card via
  `ThreadTS` when hot cache is cold (`TestResolveAgentCardRef_ThreadFallback`
  already exists at line ~87).
- `bot_agents_notify_test.go` — spawn-from-thread posts a card with `threadTS`;
  updates edit the same ts; terminal state replaces in place (not double-post).
- `bot_agents_orphan_test.go` — flag OFF preserves today's early-return behavior.

## 9. Risks / sharp edges (feed into UX + plan docs)

- **Double live-edit surface:** streamMgr currently edits the user's message; if
  the card also edits, we must pick ONE surface. (UX doc Q2.)
- **Thread noise / ordering:** a card as the first reply pushes the user's own
  follow-ups down and competes with `gb slack reply` conversational replies.
- **Terminal double-post:** card-becomes-wrapup vs. card + fresh completion ping
  — fresh reply re-notifies the user but leaves two messages. (UX doc Q1.)
- **Restart resilience:** card ref must resolve from bead fields after a bridge
  restart so we edit, not re-post (the `ThreadFallback` path covers this).
- **Pool / non-owned threads:** must not regress the thread-pool experience.

## 10. Bottom line

The feature is **mostly a re-wiring of existing primitives**, not new
infrastructure: stop returning early for thread agents, post the card in the
thread via the already-supported `inThreadTS`, point the stream session at the
card, and resolve the terminal double-post — all behind a default-off flag. The
hard part is **UX policy**, not code volume. See `02-ux-design.md` and
`03-config-and-rollback.md`; the concrete change-set is in `04-implementation-plan.md`.
