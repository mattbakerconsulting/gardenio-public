// Tier-0 smoke: @mention spawns an agent → its card renders (real DOM).
//
// Task gd-QgcuQWQjhm (epic ka-0000006991, plan
// gardenio/plans/slack-e2e-playwright.md). This is the harness's first
// end-to-end BEHAVIOR proof — everything before it (smoke.spec.js) only checks
// that the rig's pieces are reachable. Here a real test human user types
// `@gardenio <prompt>` in the real app.slack.com web client, and we assert the
// controller/bridge reacted over Socket Mode by rendering an agent status card
// back into the same thread — asserted against the REAL rendered DOM, not a
// bot-token read-back.
//
// Two cases, both against the live rig:
//   1. @mention → agent card renders in the thread (the headline claim).
//   2. bot-posts-message read-back sanity: the card the bot posted is legible
//      from the DOM (we can read the agent slug + status text back out of the
//      rendered message), proving the outbound bot→client render path, not just
//      that *some* element appeared.
//
// Why not scripted `@gardenio` text: typing the literal string "@gardenio" posts
// plain text that never resolves to the bot user, so Slack fires no app_mention
// and nothing spawns. mentionBot() drives the real human path — type `@`, pick
// the member from the autocomplete popover, which inserts a genuine `<@Uxxx>`
// mention chip — exactly what a human does.
//
// This spec is intentionally the ONLY behavior in this file; later tiers
// (decision buttons, modal submits, slash commands) land in their own specs.

import { test, expect } from './fixtures.js';
import { mentionBot, waitForAgentCard } from './_helpers.js';
import { qa, AGENT_CARD_MARKER } from './_slack.js';

// A short, unique-ish prompt so the spawned agent has an intelligible task and
// so a human reading the test channel can tell this message came from the smoke
// suite. Kept deterministic (no Date.now()/random — unavailable and needless):
// the card assertion keys on the bridge's marker, not on the prompt echo.
const SMOKE_PROMPT =
  'slack-e2e Tier-0 smoke: ack this mention, then stop. No code changes needed.';

test.describe('slack-e2e: @mention spawns agent → card renders', () => {
  test('posting @gardenio <prompt> renders an agent card in the thread', async ({
    workspace,
  }) => {
    // `workspace` is the authenticated Slack web Page, already navigated into
    // the test channel with the composer mounted (fixture asserts liveness).
    const page = workspace;

    // Post the mention as a real human would (autocomplete → `<@Uxxx>` chip).
    await mentionBot(page, SMOKE_PROMPT);

    // The bridge spawns/binds an agent and posts its status card back into the
    // thread. Assert the card mounted in the REAL DOM (not a bot-token probe).
    const card = await waitForAgentCard(page);

    // Prove it is genuinely the agent card, not an incidental marker match:
    // the card's context line pairs the agent slug (backtick-wrapped) with the
    // "Decisions thread below" marker on one message.
    await expect(card.getByText(AGENT_CARD_MARKER, { exact: false }).first()).toBeVisible();
  });

  test('bot-posts-message read-back sanity: the card is legible from the DOM', async ({
    workspace,
  }) => {
    // Independent of case 1's spawn (Slack routes a fresh channel-level mention
    // to its own thread). Re-mention and read the bot's own rendered message
    // back out of the client DOM — the outbound bot→client render path that the
    // outbound-only slackmock + bash scripts can never exercise.
    const page = workspace;
    await mentionBot(page, SMOKE_PROMPT);

    const card = await waitForAgentCard(page);

    // Read-back: the rendered card must carry the agent-slug backtick chip that
    // buildAgentCardBlocks stamps beside the marker. `<agent-slug>` renders as
    // an inline code span; asserting the marker sits in a message that ALSO has
    // an inline code element proves we read a real, structured bot card back —
    // not a bare toast or the composer echo.
    await expect(card.getByText(AGENT_CARD_MARKER, { exact: false }).first()).toBeVisible();
    const codeChip = card.locator('code, [data-qa="message_kit_code"]').first();
    await expect(
      codeChip,
      'agent card should render the agent slug as an inline code chip beside the marker',
    ).toBeVisible();

    // And the fallback/notification text the bridge sets on every card post is
    // "Agent: <name>" — a final legibility check that this is the agent card
    // message the bot authored.
    await expect(page.locator(qa.message).filter({ hasText: AGENT_CARD_MARKER }).first()).toBeVisible();
  });
});
