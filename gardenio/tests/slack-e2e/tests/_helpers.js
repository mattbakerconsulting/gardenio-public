// Shared interaction helpers for the real-user Slack E2E suite.
//
// Keep DOM SELECTORS in tests/_slack.js (the `qa` map) and keep reusable
// ACTIONS/WAITS here. Specs import from both. As Tier-1+ specs land (decision
// cards, modals, slash commands — plan §Coverage roadmap), the click/submit/
// read-back helpers they share live here so a Slack interaction pattern is
// written once.

import { expect } from '@playwright/test';
import {
  qa,
  botHandle,
  AGENT_CARD_MARKER,
  SESSION_EXPIRED_HINT,
} from './_slack.js';

// assertWorkspaceLoaded — confirm the authenticated workspace mounted, not the
// signed-out login screen. Throws the runbook pointer on a lapsed session.
// The `workspace` fixture already runs this; exposed for specs that navigate
// elsewhere and want to re-assert liveness.
export async function assertWorkspaceLoaded(page) {
  const signedOut = page.locator(qa.signedOut).first();
  if (await signedOut.isVisible().catch(() => false)) {
    throw new Error(`Slack signed-out screen visible. ${SESSION_EXPIRED_HINT}`);
  }
  await expect(page.locator(qa.workspaceLoaded).first()).toBeVisible();
}

// postMessage — type into the channel composer and send. Returns the trimmed
// text so callers can assert on the rendered echo. Uses the composer's
// contenteditable (Slack's message input is not a plain <textarea>).
export async function postMessage(page, text) {
  const input = page.locator(qa.messageInput).first();
  await expect(input).toBeVisible();
  await input.click();
  await input.fill(text);
  await input.press('Enter');
  return text;
}

// waitForMessageContaining — wait until a rendered message row containing the
// given text appears in the channel. Text/role locators (not CSS classes) so
// Slack DOM churn does not break the wait (plan §Risks 2).
export async function waitForMessageContaining(page, text, timeoutMs = 30_000) {
  const msg = page.getByText(text, { exact: false }).first();
  await expect(msg).toBeVisible({ timeout: timeoutMs });
  return msg;
}

// mentionBot — type `@<handle> <prompt>` into the composer and send it AS A REAL
// APP MENTION. Typing the literal string "@gardenio" posts plain text that never
// resolves to the bot user, so no app_mention event fires; the bot only spawns
// when the token is a genuine `<@Uxxx>` user link. Slack builds that link when
// you type `@<partial>` and pick the member from the autocomplete popover, so
// this helper drives the real human path: type the handle, wait for the
// typeahead, commit the highlighted match, then the prompt, then send.
export async function mentionBot(page, prompt, handle = botHandle()) {
  const input = page.locator(qa.messageInput).first();
  await expect(input).toBeVisible();
  await input.click();

  // Trigger the member typeahead. Type char-by-char so Slack's composer fires
  // its input handlers (a bulk .fill() can skip the autocomplete entirely).
  await input.pressSequentially(`@${handle}`, { delay: 40 });

  // The autocomplete popover must surface a match — if it never appears, the
  // bot is not installed / not invited to this channel under `handle`, which is
  // a rig-provisioning problem, not a flaky selector. Fail with that pointer.
  const menu = page.locator(qa.autocompleteMenu).first();
  await expect(
    menu,
    `member autocomplete never appeared for "@${handle}" — is the test Slack ` +
      'app installed and invited to the channel under that display name? ' +
      '(override via SLACK_E2E_BOT_HANDLE). See slack-e2e-provisioning.md Part A.',
  ).toBeVisible({ timeout: 15_000 });

  // Commit the highlighted match — Slack replaces "@handle" with a real
  // `<@Uxxx>` mention chip. Enter selects the highlighted autocomplete row
  // (it does NOT send while the popover is open).
  await input.press('Enter');
  // The popover closes once the mention is inserted.
  await expect(menu).toBeHidden({ timeout: 10_000 });

  // Now the prompt, then send.
  await input.pressSequentially(` ${prompt}`, { delay: 10 });
  await input.press('Enter');
  return prompt;
}

// waitForAgentCard — wait until an agent status card mounts in the DOM and
// return its message container. The card is a bot message whose context line
// carries AGENT_CARD_MARKER ("Decisions thread below"); asserting on that
// substring proves a REAL card rendered, independent of the agent's name/state.
// Generous default budget: a cold mention spawns a pod (image pull + boot)
// before the bridge posts the "starting" card.
export async function waitForAgentCard(page, timeoutMs = 75_000) {
  const marker = page.getByText(AGENT_CARD_MARKER, { exact: false }).first();
  await expect(
    marker,
    'agent status card never rendered — no message carrying ' +
      `"${AGENT_CARD_MARKER}" appeared after the @mention. The bridge posts it ` +
      'once the mention spawns/binds an agent (buildAgentCardBlocks).',
  ).toBeVisible({ timeout: timeoutMs });
  // Return the enclosing message container so callers can scope further
  // assertions (glyph, agent slug) to the card itself.
  const card = marker.locator(`xpath=ancestor-or-self::*[@data-qa="message_container"][1]`);
  return (await card.count()) > 0 ? card.first() : marker;
}
