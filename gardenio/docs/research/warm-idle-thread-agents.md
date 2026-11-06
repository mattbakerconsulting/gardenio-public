# Warm-idle thread agents — research

**Epic:** ka-0000006987 · **Gardener:** ka-0000003141 · **Bundle:** ka-0000002551
**Operator:** Matt Baker (@matthew.baker) · **Origin thread:** Slack C0B5S61RVNU/1780352456.856459
**Author:** thread-1780352456-856459 · **Date:** 2026-06-02

## The ask
> "would be cool if the agents that spawn for these chat windows stayed idle for an hour or two
> before going away so they can rapidly respond. might be a good way to test a lower-latency
> gardenio flow." — Matt Baker

When a user keeps chatting in a Slack AI-pane / DM thread, the follow-up should hit a **live, warm
pod** (instant nudge) rather than a **cold respawn** (image pull → coop boot → `claude --resume` →
rehydrate context). Keep the thread agent warm for ~1–2h after the last message, then despawn.

## TL;DR
The fast path **already exists**. gardenio already nudges the live pod when it's up, and only
cold-respawns (with session resume) once the pod is gone. There is **no deliberate idle-TTL** today,
so a thread pod's warm lifetime is incidental — it stays up until an upgrade drain, crash, or the
~2h stale reaper takes it. The work is to add a *deliberate, configurable warm-idle window* and to
*measure* the warm-vs-cold latency delta to prove the lower-latency flow.

## Current delivery paths (code map)

### Inbound: Slack pane/DM → agent
`bot_assistant.go:handleAssistantMessage` (line 167) records the `gardenio_assistant_event_total`
`message` counter (the smoke-check from MR !3364), sets the "Thinking..." loading status, then
synthesizes an `@mention` and calls `handleAppMention`.

`bot_mentions.go:handleAppMention` (line 28) resolves the bound agent for the thread
(`getAgentByThread`) and branches:

| Condition | Path | Latency |
|---|---|---|
| Bound agent bead still active | `nudgeAgentForMention` (`bot_mentions.go:1429`) injects into the **running** coop session | **warm — fast** |
| `FindAgentBead` fails (pod gone) | `respawnThreadAgent` (`bot_agent_kill.go:91`) spawns a fresh pod with `COOP_S3_SESSION_KEY=<bead-id>`; coop downloads prior session and `claude --resume` rehydrates | **cold — slow** |

So the low-latency path Matt wants is **already implemented** — it just only applies *while the pod
is alive*.

### What keeps a thread pod alive today
Thread agents are instructed (role nudge + advice ka-0000006028 / ka-0000006028) to "respond then
stay alive — do NOT call `gb stop`" and wait for follow-ups. The Claude turn loop idle-waits between
messages, so the pod naturally lingers. `monitorIdle` (`init_agent.go:479`) polls coop every 10s and
writes `agent_state=idle|working` onto the agent bead.

### What ends a thread pod today (none of it is "deliberate idle-TTL")
- explicit `gb stop` / operator dismiss
- crash / `max_restarts` in the restart loop (`init_agent_loop.go`)
- **image-upgrade drain** — `reconciler/upgrade.go:240` drains pods at `idle`/`exited` state
- **stale / leaked-bead reaper** — reconciler closes terminal-state podless beads and warns on
  stale deferred agents; the broad stale window is ~2h (`reconciler.go:316`)

Net: a thread pod's warm lifetime is **incidental**, not chosen. It either stays up indefinitely
(cost) or dies on the next upgrade/crash — and then the *next* message pays the cold-respawn tax.

## Adjacent machinery we can reuse
- **`monitorIdle`** already produces the `idle` signal an idle-TTL would key off.
- **Prewarm pool** (`internal/poolmanager/`) keeps per-project pools of *prewarmed* pods
  (`AnnotationPrewarmed`, project `prewarmed_pool` config) for instant assignment — a second angle
  on the same latency problem (assign a thread to a pre-booted pod).
- **`gardenio_assistant_event_total`** (MR !3364) already instruments pane delivery — extend it with
  a `delivery_path` (warm|cold) label + a cold-respawn latency histogram.

## Proposed approach
**A) Idle-TTL warm-keepalive (core).** Configurable warm-idle window (default ~90m, per-project /
per-role tunable). While the thread agent is idle, keep the pod alive until
`now - last_inbound_message_ts > idle_ttl`, then `gb stop` cleanly. Turns "stay up forever or die on
upgrade" into "stay warm for a chosen window, then deliberately despawn." Must not despawn mid-turn,
must reset on each nudge, must leave session JSONL intact for a later cold resume, and must not
double-despawn against upgrade drain / reaper. Placement TBD: controller-reconciler vs in-pod
`gb` idle watcher (decide in the implementation MR).

**B) Measure warm-vs-cold (proof).** `delivery_path` label + cold-respawn duration histogram +
warm-hit ratio. This is the artifact that demonstrates the "lower-latency gardenio flow."

**C) (stretch) Prewarm-pool handoff for threads.** Land even a cold/new thread on a pre-booted pod.

## Acceptance
1. Thread agent stays warm-idle for the configured window; a follow-up within the window is served
   by a live nudge (no respawn) — demonstrated end-to-end in the gardenio Slack pane.
2. After the window, clean despawn (bounded cost, no zombie pods).
3. A metric distinguishes warm vs cold delivery and reports the latency delta.
4. Gardener (ka-0000003141) drives the children to merge soup-to-nuts.

## Children
- ka-0000001429 — Spike: baseline idle lifetime + cold-respawn latency (measurement)
- ka-0000005793 — Implement idle-TTL warm-keepalive + deliberate despawn (depends on spike)
- ka-0000001565 — Instrument warm-vs-cold delivery path + latency histogram (depends on impl)
- ka-0000004739 — Stretch: prewarm-pool handoff for thread spawns (depends on impl)
