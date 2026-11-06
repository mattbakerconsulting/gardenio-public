# Config & Rollback Design

**Epic:** ka-0000001984 · **Task:** ka-0000000339 · **Date:** 2026-06-01

Matt's hard constraint: *"it should be configurable too… it needs to be something
we can roll back for that reason."* This doc specifies the toggle surface and the
rollback story. The governing principle: **default OFF, and a kill switch that
needs no redeploy.**

---

## 1. Two-layer toggle

### Layer 1 — Env flag (deploy-time master switch)

Reuse the existing threading config seam (`SLACK_THREADING_MODE`,
`cmd/slack-bridge/main.go:563` → `BotConfig.ThreadingMode`). Add a sibling env
var dedicated to thread cards so we don't overload the existing semantics:

```
SLACK_THREAD_CARDS=off        # default — today's behavior, byte-identical
SLACK_THREAD_CARDS=owned      # post cards only in user-owned threads (recommended v1)
SLACK_THREAD_CARDS=all        # post cards in all thread spawns (future)
```

- Surfaced as `BotConfig.ThreadCardsMode`, checked via a helper
  `b.threadCardsMode()` mirroring `agentThreadingEnabled()`.
- Set in the Helm values (`acme-helm-chart/charts/gardenio/values/gardenio.yaml`)
  on the slack-bridge deployment. Default `off`.
- Rollback at this layer = revert the values change + redeploy. Always works,
  but is a redeploy. This is the **floor**, not the fast path.

### Layer 2 — Config bead (runtime kill switch, no redeploy)

Precedent: the bridge already reads `bridge-notifications` config beads at init
(`SuppressCostReporting`, `SuppressToolStream`). Add a `bridge-thread-cards`
config key the bridge reads (and re-reads on change) so an operator can:

```
# disable instantly, no redeploy:
kd config set bridge-thread-cards.enabled false

# scope by project:
kd config set bridge-thread-cards.projects "gardenio,gardenio-rwx"

# tune the terminal completion-ping threshold (seconds); 0 = ping only for
# runs that produced MRs/PRs. Default 120 (2 min). See doc 02 §Q1.
kd config set bridge-thread-cards.ping_threshold_seconds 120
```

**Resolution order (most-specific wins):**
1. config bead `enabled=false` → OFF everywhere (the kill switch).
2. config bead `projects` allow-list → on only for listed projects.
3. env `SLACK_THREAD_CARDS` mode → the behavior when enabled.
4. absence of all → `off`.

This gives R3: a single `kd config set ... enabled false` turns the feature off
fleet-wide within one bridge poll interval, **no pod restart**. That is the
rollback path Matt wants for a "tricky" UX feature.

> Note: per advice ka-0000007027, this config key is bridge-runtime, env-specific
> behavior; if a gardenio→gardenio-next mirror exists, treat `bridge-thread-cards`
> as **not mirrored** (per-env, like other Slack/bridge keys).

## 2. Per-project granularity

Matt wants it configurable; per-project is the most useful granularity (try it in
`gardenio-rwx` first, then promote to `gardenio`). Delivered by the
`bridge-thread-cards.projects` allow-list above. The agent bead's `project` field
is already available at card-post time, so gating is a cheap string check.

## 3. Phased rollout (maps to the rollback story)

1. **Ship dark.** Land the code with `SLACK_THREAD_CARDS=off` everywhere. Zero
   behavior change. Mergeable + releasable immediately; nothing to roll back.
2. **Canary in gardenio-rwx.** `projects="gardenio-rwx"` via config bead. Spawn a
   few thread agents, eyeball the UX against the `02-ux-design.md` acceptance
   checklist. Kill switch one command away.
3. **Production canary.** Add `gardenio` to the allow-list. Watch a day.
4. **Default-on (optional, later).** Only after the UX is judged "perfect," flip
   the env default to `owned`. Even then the config-bead kill switch remains.

At every phase, rollback = `kd config set bridge-thread-cards.enabled false`
(instant) or revert the values bump (redeploy). No data migration, no message
cleanup needed — turning it off simply restores the early-return path; existing
cards are inert (they just stop updating).

## 4. What "off" must guarantee (regression contract)

When OFF (env or config), the thread-spawn branch must take the **exact** current
path: early return after 👀 + `StartSession` on the user's message + terminal
`postThreadStateReply`. A test in `bot_agents_orphan_test.go` /
`bot_agents_notify_test.go` must assert: flag-off ⇒ no `ensureAgentCard` call for
thread agents, behavior identical to baseline. This is the safety net that makes
"ship dark" honestly zero-risk.

## 5. Observability for the rollout

- Log a one-line structured event when a thread card is posted/suppressed with
  the resolved mode + project, so we can confirm the gate is doing what we think.
- No new metrics required for v1; the bridge's existing card-update logging
  covers the edit path.

## 6. Decision points for the operator

Before implementation starts, the operator should confirm:

- **D1.** Two-layer toggle (env master + config-bead kill switch) — approve?
- **D2.** v1 gating default = `owned-only`, canary in `gardenio-rwx` first — approve?
- **D3.** Q1 UX (single card → wrap-up + conditional completion ping) — approve,
  or prefer always a fresh completion reply?

These are surfaced as a `gb decision` when the bundle is delivered, so the
implementation epic starts from a signed-off UX + config contract.
