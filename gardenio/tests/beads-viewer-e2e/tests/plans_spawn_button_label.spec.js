// /plans cockpit — the compose button's label adapts to the plan assistant's
// state (S5, ka-0000003325): the button names what pressing it DOES —
//
//   none   → "Spawn" — no assistant bead yet; the first message cold-starts one.
//   parked → "Wake"  — a parked (dormant) assistant; the message thaws it.
//   live   → "Send"  — a live assistant that takes the message directly.
//
// plus the operator-gating half: the sensitive WAKE transition (reviving a
// parked/dormant pod someone let go idle) carries an hx-confirm, while the
// expected first-contact Spawn and the instant live Send stay single-tap. The
// confirm lives on the FORM (the hx-post element) so htmx resolves it off the
// request-initiating element and it gates both the click and the Enter-to-send
// path — and, deliberately, only the Wake state, so it never blocks the existing
// spawn/compose send flows (a headless confirm-dismiss would otherwise swallow
// the POST).
//
// The button lives in the merged plan-agent card (partials/plan_agent.html) on
// the /plans/{id} cockpit — NOT the index card, whose free-text box was replaced
// by the one-tap "Dispatch #N →" button. This spec seeds on three plan shapes,
// one per state, so each label + gate is pinned against the real daemon render.
//
// FIXTURES (seed/fixtures.mjs):
//   plan_epic        — no bound assistant           → none  → "Spawn"
//   parked_assistant — one parked (stopping) agent  → parked → "Wake"
//   live_assistant   — a live (working) agent       → live  → "Send"
//
// AUTH: BEADS_VIEWER_DEV_AUTH_BYPASS promotes X-Auth-Request-Email into the
// verified viewer identity; any non-empty email is an operator under dev-bypass.

import { test, expect } from '@playwright/test';

import { fixtureId, waitForHtmxIdle } from './_helpers.js';

const OPERATOR_EMAIL = 'e2e-plans@example.test';

// The compose form + its submit button on the cockpit plan-agent card. The
// adaptive LABEL lives on the button; the operator-gating hx-confirm lives on the
// form (the element that carries hx-post, so htmx resolves the confirm there —
// and it gates both the click and the Enter-to-send path).
const composeForm = (page) => page.locator('section#plan-agent form.plan-assistant-form');
const sendButton = (page) => composeForm(page).locator('button.plan-assistant-send');

test.describe('/plans cockpit — spawn-button label adapts to assistant state', () => {
  test.use({ extraHTTPHeaders: { 'X-Auth-Request-Email': OPERATOR_EMAIL } });

  test('none state → button reads "Spawn" and is NOT confirm-gated', async ({ page }) => {
    const planId = fixtureId('plan_epic');
    await page.goto(`/plans/${planId}`);
    await waitForHtmxIdle(page);

    // The lane starts in the `none` state (no assistant bound), so the button
    // says what the first message does: Spawn a fresh agent. First contact is
    // the expected path, so it is not confirm-gated.
    await expect(page.locator('section#plan-agent')).toHaveAttribute('data-state', 'none');
    const btn = sendButton(page);
    await expect(btn, 'the compose button renders for an operator').toBeVisible();
    await expect(btn).toHaveText('Spawn');
    await expect(btn).toHaveAttribute('data-send-state', 'none');
    // No Wake gate on a first-contact Spawn — the compose form's only confirm copy
    // is the Wake gate, so its absence proves Spawn is ungated.
    const confirm = await composeForm(page).getAttribute('hx-confirm');
    expect(confirm, 'a first-contact Spawn form carries no Wake confirm gate').toBeNull();
  });

  test('parked state → button reads "Wake" and IS gated by a wake confirm', async ({ page }) => {
    const planId = fixtureId('parked_assistant');
    await page.goto(`/plans/${planId}`);
    await waitForHtmxIdle(page);

    // The sole bound assistant is parked (agent_state=stopping), so the message
    // thaws it: the button reads Wake.
    await expect(page.locator('section#plan-agent')).toHaveAttribute('data-state', 'parked');
    const btn = sendButton(page);
    await expect(btn).toBeVisible();
    await expect(btn).toHaveText('Wake');
    await expect(btn).toHaveAttribute('data-send-state', 'parked');
    // Operator-gating: thawing a parked pod is a deliberate act — the confirm
    // lives on the form (where hx-post is) so it gates the request (both the click
    // and the Enter-to-send path).
    await expect(composeForm(page)).toHaveAttribute(
      'hx-confirm',
      /Wake the plan agent with this message\?/,
    );
  });

  test('live state → button reads "Send" and carries NO confirm gate', async ({ page }) => {
    const planId = fixtureId('live_assistant');
    await page.goto(`/plans/${planId}`);
    await waitForHtmxIdle(page);

    // A live assistant takes the message directly — a single-tap nudge, so the
    // button reads Send with no confirm.
    await expect(page.locator('section#plan-agent')).toHaveAttribute('data-state', 'live');
    const btn = sendButton(page);
    await expect(btn).toBeVisible();
    await expect(btn).toHaveText('Send');
    await expect(btn).toHaveAttribute('data-send-state', 'live');
    // A live Send is not gated: the form carries no Wake confirm.
    const confirm = await composeForm(page).getAttribute('hx-confirm');
    expect(confirm, 'a live Send form carries no confirm gate').toBeNull();
  });
});
