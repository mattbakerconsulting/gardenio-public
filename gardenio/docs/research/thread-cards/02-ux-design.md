# UX Design: The Thread Card

**Epic:** ka-0000001984 · **Task:** ka-0000006527 · **Date:** 2026-06-01

This is the "tricky to get perfect" part Matt called out. Below are the open UX
questions, the trade-offs, and a **recommended default** for each. Recommendations
are proposals for operator sign-off, not locked decisions.

---

## The picture we're aiming for

A user opens a thread by @-mentioning the bot (or replying in a thread they own).
An agent spawns. Today the thread looks like:

```text
[user]  @gardenio please look at the flaky auth test         👀
   └ (tool-stats quietly edited onto the user's message)
   └ [agent]  ✅ Done. <wrap-up reply with accomplishments>     ← only at the end
```

The target:

```text
[user]  @gardenio please look at the flaky auth test
   └ [gardenio]  🟢 flaky-auth · working · 2m ago               ← CARD, first reply
               running commands… (31 tools) [2m] · last: Bash(go test)
               tldr: reproduced the flake; it's a goroutine leak in the pool
   └ [user]  oh nice, is it the same one as last week?
   └ [gardenio]  (card edits in place → ✅ done, tldr, MRs, accomplishments)
```

The card is the **anchor**: one message, born as the agent's first reply,
live-updated, ending as the wrap-up. Conversational `gb slack reply` messages
interleave naturally as additional replies.

---

## Q1 — One message (card→wrap-up) vs. card + separate completion reply

**Options**
- **A. Single message.** Card is posted once, edited throughout, and its final
  edit *is* the wrap-up. Cleanest thread; the card is a stable anchor.
- **B. Card + fresh completion reply.** Card stays as live status; a *new* reply
  is posted at the end with the wrap-up.

**Trade-off.** A is tidy but produces **no new notification** at completion —
Slack does not re-ping on an edit, so a user who left the thread won't know it
finished. B re-notifies but leaves two messages and can feel redundant.

**Recommendation: A (single message) + a lightweight completion ping.** Edit the
card into its wrap-up form, AND post a tiny one-line reply ("✅ done — see card
above") *only* when the run took longer than a threshold (e.g. > 2 min) or
produced MRs. This gets the notification without permanent clutter for quick
runs. The threshold is configurable. This supersedes today's full
`postThreadStateReply` for thread-card mode.

## Q2 — Tool-stat streaming: card vs. user's message

Today `streamMgr.StartSession` live-edits the **user's** root message with tool
stats — which is slightly odd (editing someone else's message) and collides with
a card.

**Recommendation:** In thread-card mode, point the stream at the **card** via
`StartCardSession(agent, channel, cardTS)` — exactly what the scheduled-agent
path already does (`bot_agents.go:660`). Tool stats then render inside the card's
context block. Stop editing the user's message. This gives a single, coherent
live surface and removes the "bot edited my message" weirdness. Respect the
existing `SuppressToolStream` notification config.

## Q3 — Thread noise / message ordering

A card as the first reply pushes the user's own follow-ups down by one and sits
between the user's question and any human replies.

**Recommendation:** Accept the single anchor card (it's the point of the
feature), but keep it **compact while working** — header line + one context line
+ tldr — and only expand to the full multi-block layout in the **wrap-up** state.
This minimizes vertical footprint during the run. Reuse
`buildCompactAgentCardBlocks()` styling for the working state in threads;
`buildWrapUpAgentCardBlocks()` for the terminal state.

## Q4 — "User owns the thread" gating

Matt: "in cases where the user owns the thread." Detection:
`slack_opener_user_id == slack_user_id` (thread root authored by the spawning
user). 

**Recommendation:** Default the thread-card to **user-owned threads only**.
- **User-owned thread** → post thread card (the feature).
- **Bot-rooted thread** (agent card already standalone) → unchanged.
- **Thread-pool spawns** (`spawn_source=slack-thread-pool`) → unchanged in v1;
  pools have shared/synthetic roots and multiple agents, so a single anchor card
  is ambiguous. Revisit after v1.

Make the gate itself a setting (`owned-only` | `all-threads` | `off`) so we can
widen later without code changes.

## Q5 — Multiple agents in one thread

If a user spawns a second agent in the same thread, do we post a second card?

**Recommendation:** Yes — one card per agent, each a distinct reply. The thread
bead already tracks `ThreadAgents` keyed by `{channel}:{thread_ts}`. Cards are
per-agent (keyed by agent identity), so this falls out naturally. No special
handling for v1.

## Q6 — Interaction with `gb slack reply`

Agents post conversational replies via `gb slack reply`. These should remain
**separate** thread replies (the human conversation), distinct from the card (the
status anchor). No change to `gb slack reply` behavior — the card is additive.

## Q7 — Failure & no-wrap-up exits

If an agent dies without a wrap-up (OOM, crash), the card must still resolve to a
sensible terminal state rather than freezing at "working".

**Recommendation:** Reuse existing terminal-state detection; on failed/orphaned,
edit the card to a failed state with whatever wrap-up exists (or "ended without
wrap-up"). This already has precedent in `postThreadStateReply` and the orphan
handling; we route it through the card edit instead.

---

## Recommended v1 UX, summarized

| Question | v1 default |
|---|---|
| Q1 message model | Single card → edited into wrap-up; tiny completion ping for long/MR runs |
| Q2 tool stats | On the card (`StartCardSession`); stop editing the user's message |
| Q3 footprint | Compact while working, full layout at wrap-up |
| Q4 gating | User-owned threads only (`owned-only` default); setting allows widening |
| Q5 multi-agent | One card per agent, no special-casing |
| Q6 `gb slack reply` | Unchanged; card is additive |
| Q7 crash exits | Card edited to failed terminal state |

All of this is behind the default-OFF master flag (see `03-config-and-rollback.md`),
so none of it ships live until the operator flips it on and eyeballs the result.

## What "perfect" looks like (acceptance for a future demo)

1. Spawn from an owned thread → a compact card appears as the first reply within
   ~2s, with 👀 on the user's message.
2. Tool stats tick on the card, not the user's message.
3. `gb slack reply` messages interleave below the card as normal replies.
4. On completion the card expands to the wrap-up with tldr at the top; a one-line
   "done" ping appears only for long/MR runs.
5. Bridge restart mid-run → card keeps updating the same message (no duplicate).
6. Flag OFF → behavior is byte-identical to today.
