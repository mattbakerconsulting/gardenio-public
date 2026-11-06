# Implementation Plan: Thread Cards

**Epic:** ka-0000001984 · **Task:** ka-0000004825 · **Date:** 2026-06-01

Concrete change-set for a follow-up implementation epic. All paths relative to
`gardenio/controller/internal/bridge/` unless noted. **Nothing here lands without
operator sign-off on the bundle** (see `03-config-and-rollback.md` §6).

---

## Change 1 — Config plumbing (default OFF)

- `cmd/slack-bridge/main.go:~563`: read `SLACK_THREAD_CARDS` env (default `off`),
  set `cfg.threadCardsMode`.
- `cmd/slack-bridge/helpers.go:~190`: pass through to `BotConfig.ThreadCardsMode`.
  Add to `helpers_test.go` env-parse coverage (mirror the existing
  `TestParseConfig_ThreadingModeOverride` at line ~604).
- `bot.go`: add `threadCardsMode string` field (next to `threadingMode`,
  `bot.go:62`); wire from `cfg.ThreadCardsMode` (`bot.go:364`).
- Add helper `func (b *Bot) threadCardsEnabled(project string) bool` resolving:
  config-bead kill switch → config-bead project allow-list → env mode → off
  (resolution order per `03-config-and-rollback.md` §1).
- Config-bead read: extend the existing bridge config-bead loader
  (the one that populates `notifConfig` / `bridge-notifications`) to also load
  `bridge-thread-cards` (`enabled`, `projects`).

## Change 2 — Post the card in the thread-spawn branch

In `NotifyAgentSpawn` (`bot_agents.go:583-631`), the thread-bound branch:

```go
// after markThreadBound, before the current early `return`:
if b.threadCardsEnabled(bead.Fields["project"]) && b.threadCardOwned(bead) {
    cardTS, err := b.ensureAgentCard(ctx, agent, slackChannel, slackTS) // inThreadTS!
    if err != nil {
        b.logger.Error("failed to post thread agent card", "agent", agent, "error", err)
    } else if cardTS != "" {
        // point tool-stats at the CARD, not the user's message (UX Q2)
        if b.streamMgr != nil && !b.notifConfig.SuppressToolStream {
            b.streamMgr.StartCardSession(agent, slackChannel, cardTS)
        }
        if b.reactions != nil {
            b.reactions.SetTarget(agent, slackChannel, cardTS)
            b.reactions.TransitionState(agent, "spawning")
        }
    }
    return // card path taken; skip the legacy StartSession-on-user-message branch
}
// else: existing early-return behavior (the regression contract, §4)
```

- `threadCardOwned(bead)` = `bead.Fields["slack_opener_user_id"] != "" &&
  bead.Fields["slack_opener_user_id"] == bead.Fields["slack_user_id"]`
  (UX Q4). For `owned` mode this gates; for `all` mode it's bypassed.
- Mind the **race**: the existing code notes handleThreadSpawn may not have
  registered thread mapping yet. We already have channel/ts here from the
  resolve/fallback chain, so post is safe; `ensureAgentCard` is idempotent on the
  hot cache so a double-fire won't double-post.

## Change 3 — Compact-while-working rendering (UX Q3)

- `buildAgentCardBlocks` already exists; for thread cards in the *working* state,
  render the compact variant. Two options:
  - (a) pass a `compact bool` / context into the builder, or
  - (b) call `buildCompactAgentCardBlocks` from the thread-card update path.
- Keep wrap-up state on the full `buildWrapUpAgentCardBlocks`.
- The update path (`updateAgentCard`/`flushAgentCard`, `bot_agents.go:1131-1252`)
  already edits by stored ts — no change to the edit mechanics, only block
  selection.

## Change 4 — Terminal state: edit card, conditional ping (UX Q1)

- `bot_agents.go:835` currently calls `postThreadStateReply` at terminal state
  for thread agents. When the thread-card path is active, instead:
  - resolve the card ref (`resolveAgentCardRef`, hot cache → state → bead fields;
    `TestResolveAgentCardRef_ThreadFallback` covers cold-cache) and call
    `replaceAgentCardWithWrapUp` to edit the card in place.
  - post a **one-line** completion reply only if run duration > threshold OR
    `pull_requests` present (conditional ping). Threshold is a config value.
- Leave `postThreadStateReply` intact for the flag-off path.

## Change 5 — Restart resilience

- No new code expected: `resolveAgentCardRef` already falls back to bead fields
  `slack_card_*` and then `slack_thread_*`. Confirm `persistCardRef` writes
  `slack_card_ts`/`slack_card_channel` for thread cards too (it should, since it
  runs inside `ensureAgentCard`). Add a test asserting post-restart edit hits the
  same ts.

## Tests

| File | New/extended assertion |
|---|---|
| `cmd/slack-bridge/helpers_test.go` | `SLACK_THREAD_CARDS` parsed into `BotConfig` (default off) |
| `bot_agents_notify_test.go` | flag=`owned` + owned thread ⇒ `ensureAgentCard` called with `threadTS`; updates edit same ts; terminal edits in place (no double post) |
| `bot_agents_notify_test.go` | flag=`off` ⇒ NO card call for thread agent; behavior == baseline (regression contract) |
| `bot_agents_notify_test.go` | flag=`owned` + NON-owned thread ⇒ no card (gating) |
| `agents_card_wrapup_edit_test.go` | `ThreadTS` set on thread card; state round-trips; wrap-up resolves via `ThreadFallback` after cold cache |
| `agents_card_test.go` | compact-while-working block selection for thread cards |
| `bot_agents_orphan_test.go` | crash/orphan with thread card ⇒ card edited to failed terminal state |

## Effort & sequencing

- **Change 1 (config)** is independent and small — land first.
- **Change 2 + 3 (post + render)** are the core; behind the flag, so safe to land
  dark.
- **Change 4 (terminal reconciliation)** is the fiddliest (double-post avoidance)
  — land with thorough tests.
- **Change 5** is mostly verification.

Rough size: ~150–250 LOC of bridge code + ~6 test additions. No schema changes,
no migrations, no Helm chart structural changes (only a values flag, default
off).

## Rollback recap

- Code is inert with `SLACK_THREAD_CARDS=off` (default) and/or
  `bridge-thread-cards.enabled=false`.
- Instant kill switch: `kd config set bridge-thread-cards.enabled false` (no
  redeploy).
- Hard revert: drop the values flag + redeploy. Existing cards go stale (stop
  updating) but cause no errors.

## Suggested follow-up beads (created when operator approves the bundle)

1. `feat(bridge): SLACK_THREAD_CARDS config plumbing (default off)` — Change 1.
2. `feat(bridge): post agent card in user-owned thread + card-anchored tool stats`
   — Changes 2–3.
3. `feat(bridge): thread-card terminal reconciliation (wrap-up edit + conditional
   ping)` — Change 4.
4. `test(bridge): thread-card regression + gating coverage` — Change 5 + tests.
5. `chore(helm): add SLACK_THREAD_CARDS flag to slack-bridge values (off)`.
