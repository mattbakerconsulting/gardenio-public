# Real-User Slack E2E Testing via Playwright

**Bead:** ka-0000006019 · **Operator:** Matt Baker · **Status:** scoping plan (draft)

## Goal

Drive the gardenio Slack surface the way a human does — type in a real Slack
workspace, click buttons, fill modals — and assert the controller/bridge
reacts correctly end-to-end. This complements (does not replace) the existing
`slackmock` + bash scripts, which exercise the bridge's *outbound* posting and
the Slack *API* contract but never the real client UI or the inbound
Socket-Mode event path under real Slack rendering.

## Why now / what's missing today

Current Slack coverage (under `gardenio/tests/e2e/scripts/`):

- `test-slack-threads.sh`, `test-slack-cards.sh`, `test-slack-dashboard.sh` —
  drive the bridge via the **Slack Web API** (or `slackmock`) and assert on
  `conversations.history`. They post *as the bot* and read back; they never
  simulate a human typing or clicking.
- `slackmock` (`gardenio/tests/slackmock/`) — a fake Slack API server so CI
  doesn't flood real channels. Great for outbound assertions, blind to the
  real client.

**The gap:** no test ever (a) renders a Block Kit card in a real Slack client,
(b) clicks a real button / submits a real modal, or (c) flows a *human-authored*
message through Slack → Socket Mode → bridge → controller. Regressions in
Block Kit payloads, action_id wiring, modal `view_submission` handling, and
mention-spawn UX can ship green.

## Key architectural enabler: Socket Mode

The bridge connects to Slack via **Socket Mode** (outbound WebSocket;
`SLACK_APP_TOKEN` = `xapp-…`, see `bot.go:347-364`). It needs **no public
ingress**. That means an ephemeral CI bridge pod can attach to a dedicated
*test* Slack app and receive real interaction events from a real workspace
without exposing anything. This is what makes real-user E2E feasible in the
existing per-instance ephemeral-namespace flow.

## Approach

Playwright drives the **Slack web client** (`app.slack.com`) authenticated as a
dedicated **test human user**, against a dedicated **test Slack workspace +
test Slack app** wired to the ephemeral bridge under test.

```text
Playwright (test user browser)
      │ types / clicks in app.slack.com
      ▼
  Slack workspace  ──Socket Mode──►  ephemeral gardenio bridge ──► kbeads/controller
      ▲                                          │
      └──────────── bot posts card/buttons ◄─────┘
      │ Playwright asserts on rendered DOM (card text, buttons, modals)
```

### Auth: the hard part (operator escalations expected here)

- A **dedicated test Slack workspace** (free tier is fine) — isolated from the
  prod gardenio workspace so tests can't spam real channels.
- A **test Slack app** in that workspace with Socket Mode enabled, the same
  scopes as prod gardenio, its own `xoxb-`/`xapp-`/signing secret. Stored in
  AWS Secrets Manager, ESO-synced into the ephemeral namespace.
- A **dedicated test human user** in that workspace. Playwright authenticates
  by **injecting a pre-captured `storageState`** (Slack session cookies `d`,
  `d-s`) rather than scripting the login form (Slack login is MFA/email-code
  gated and bot-hostile). Capture once manually, store the cookie blob in SM,
  refresh when it expires (~rolling, weeks-to-months). This is the main
  operational cost and the most likely escalation.

> Escalation list for Matt is enumerated in the epic's first child
> (kd — "Provision test Slack workspace/app/user + secrets").

### Where it runs

- New Playwright project `gardenio/tests/slack-e2e/` (mirrors the structure of
  `gardenio/tests/beads-viewer-e2e/`: `playwright.config.js`, `globalSetup`,
  `tests/*.spec.js`, seed helpers).
- Wired into `.rwx/e2e.yml` as a **new module** (`slack-ui`) selectable via the
  existing `--only` / shard mechanism in `run-shard.sh`. Gated behind the test
  Slack secrets being present; **skips cleanly** (like the existing
  `SLACK_BOT_TOKEN`-guarded scripts) when secrets are absent, so local and
  fork CI stay green.
- Likely **not** in the 5-module smoke subset initially (slower, auth-fragile);
  runs in full E2E + scheduled. Revisit once stable.

## Coverage roadmap (the "tons of coverage" ask)

Phase the scenarios so we ship value before perfecting infra.

**Tier 0 — smoke (prove the harness):**
- `@gardenio <prompt>` mention in a channel → agent card appears in thread.
- Bot posts a message → Playwright reads it rendered (sanity of the loop).

**Tier 1 — interactions (the real prize):**
- Decision card: click an option button → bead resolves, card updates.
- Decision modal: "Other" → modal opens → submit free-text → bead resolves.
- Escalation card: "View report" / "Open UI" buttons render + link correctly.
- Permission prompt: allow / deny buttons → controller sees the choice.
- Agent card lifecycle: spawn → working (tool stats) → done/failed wrap-up +
  Clear button; clicking Clear removes the card.
- `gb squawk` → message lands in the agent's thread without killing status.

**Tier 2 — slash commands & threads:**
- `/spawn`, `/agent`, `/decide`, `/roster`, `/kill`, `/comment`, `/pipeline`
  etc. — each slash command's modal/response.
- Thread reply by human → forwarded to agent (`gb slack thread` sees it).
- Multi-agent thread isolation; dedup (no double-post).

**Tier 3 — assistant / AI-app surface:**
- Assistant thread started → streaming status messages → suggested prompts.

Each tier = a child epic with one bead per spec file, so the dependency graph
stays navigable in beads-viewer.

## Risks / open questions

1. **Slack session expiry** — the cookie `storageState` is the fragile part. We
   need a documented refresh runbook and an alarm when tests start failing auth.
   (Alternative: a long-lived test user + a tiny "session minter" — out of scope
   for v1.)
2. **Slack DOM churn** — `app.slack.com` markup changes. Mitigate with
   `data-qa` attributes (Slack ships many) and resilient text/role locators;
   keep selectors centralized in one helpers module.
3. **Flake / wall-time** — real network + real Slack. Keep Tier-0/1 tight,
   retries=1, run heavier tiers in scheduled pipelines.
4. **Mapping ephemeral bridge ↔ test app** — each ephemeral namespace gets its
   own bridge; if multiple run concurrently against one test workspace, Socket
   Mode events fan out to *all* connected apps. Options: (a) one test app per
   shard, (b) serialize the slack-ui module to a single shard (simplest for
   v1 — pin to shard 2 where the other slack modules already live), (c)
   per-namespace test channels for isolation. Recommend (b)+(c) for v1.
5. **Cost of headed browsers in CI** — Playwright chromium is already used by
   `beads-viewer-e2e`; reuse the same base image / system-deps lock.

## Concrete v1 deliverable (smallest end-to-end slice)

1. Operator provisions test workspace/app/user + SM secrets (escalation).
2. `gardenio/tests/slack-e2e/` scaffold: config + `storageState` loader +
   one helper module + Tier-0 smoke spec (mention → card).
3. Bridge deploy plumbing: ephemeral namespace can opt into the test Slack app
   secret; new `slack-ui` module in `run-shard.sh` that skips when unset.
4. Green Tier-0 in a manual `rwx dispatch gardenio-e2e --only slack-ui`.
5. Then fan out Tier-1 specs.

## Non-goals (v1)

- Replacing slackmock / existing bash Slack tests (keep as fast outbound checks).
- Desktop Slack client, mobile, or Enterprise Grid.
- Scripting the interactive Slack login form (use captured storageState).
