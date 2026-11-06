// /plans INDEX UX — the card arms the sibling seed-fixture specs LEFT UNCOVERED
// (task gd-lX5rwXCE0p; epic gd-mQnycuToAV / gd-80TfM5mPtd lineage).
//
// plans_index_ux.spec.js pinned the three headline card SHAPES (owned/live/
// all-closed) and plans_spawn_button_label.spec.js pinned the S5 compose-button
// label on the /plans/{id} COCKPIT. Between them the index card still had three
// per-arm gaps — each an alternate branch of a signal whose OTHER branch is
// already covered — that this spec closes against the real daemon:
//
//   S3 "next" pill (next_step_plan) — pickCurrentOrNextStep's THIRD kind. Every
//     other seeded plan's earliest open step is in_progress ("now",
//     plans_index_ux + plans.spec) or the plan is all-closed (no chip). NONE
//     drives the calm "next" arm: a plan whose next step is OPEN-but-not-started.
//     next_step_plan seeds exactly that (one plain-open step), so the card renders
//     .plan-step-kind-next + a 0/1 · 0% rollup.
//
//   S4 parked chip ON THE INDEX (parked_assistant) — the parked assistant state
//     was pinned only on the COCKPIT compose form (plans_spawn_button_label: the
//     "Wake" button) and, on the index, only via live_assistant's live-WINS case
//     (plans_index_ux). The index card's own parked plan-agent chip
//     (.plan-agent-chip-parked, a gardener-less plan whose sole assistant is
//     parked) was never asserted on the index. parked_assistant is that shape.
//
//   S2 creator-chip SUPPRESSION (owned_plan, owner scope) — plans_index_ux pins
//     the creator chip RENDERING for a non-owner viewer; its deliberate inverse —
//     planCreatorChipScoped returns "" on a board already scoped to the card's own
//     operator, so the owner never sees a redundant self-referential "by me" chip
//     — is the other half of the S2 contract and had no e2e assertion.
//
// WHY SEEDED (not ephemeral): same rationale as plans_index_ux — these pin
// durable card ARMS against label-stable fixtures so a per-arm render regression
// is caught by a fixed id, not a per-run mint. All fixtures are read-only here
// (no test mutates them), so they are safe under fullyParallel:true.
//
// AUTH / SCOPING (dev-bypass harness, playwright.config webServer):
//   BEADS_VIEWER_DEV_AUTH_BYPASS=1 promotes X-Auth-Request-Email into the
//   verified viewer identity. The /plans index default-scopes to the viewer's own
//   operated_by; the owner fixtures are attributed to PLANS_IDX_OWNER, so any
//   navigation that must see a fixture regardless of viewer uses
//   `/plans?operated_by=` (the empty-key "show all" form). The suppression case
//   deliberately authenticates AS the owner so the default (mine) scope applies.

import { test, expect } from '@playwright/test';

import { fixtureId } from './_helpers.js';

// Keep in sync with the operatedBy on the owner fixtures in seed/fixtures.mjs and
// with PLANS_IDX_OWNER in plans_index_ux.spec.js.
const PLANS_IDX_OWNER = 'e2e-plans-owner@example.test';

// Show-all: the empty operated_by key drops the default "scope to me" filter so a
// plan authored by a DIFFERENT alias renders for any viewer.
const PLANS_ALL = '/plans?operated_by=';

const planCard = (page, id) => page.locator(`li.plan-card[data-plan-id="${id}"]`);

// ---------------------------------------------------------------------------
// next_step_plan — S3 "next" pill (the ready-but-not-started arm)
// ---------------------------------------------------------------------------
test.describe('/plans index — next-step (ready, not started) plan', () => {
  test('surfaces the calm "next" pill (not "now") for an open, unstarted step', async ({ page }) => {
    const planId = fixtureId('next_step_plan');
    await page.goto(PLANS_ALL);

    const card = planCard(page, planId);
    await expect(card, 'next-step plan renders on the show-all index').toBeVisible();

    // S3: the earliest open step is plain `open` (not in_progress, not blocked),
    // so pickCurrentOrNextStep + Kind() resolve to the "next" arm — the calm blue
    // pill — NOT the orange "now" arm that every other seeded plan drives.
    const cardStep = card.locator('.plan-card-step');
    await expect(cardStep, 'a plan with an open next step surfaces a step chip').toBeVisible();
    await expect(cardStep.locator('.plan-step-kind-next')).toHaveText('next');
    await expect(
      cardStep.locator('.plan-step-kind-now'),
      'a ready (not-started) step is not the "now" kind',
    ).toHaveCount(0);
    await expect(
      cardStep.locator('.plan-step-kind-blocked'),
      'a plain-open step is not the "blocked" kind',
    ).toHaveCount(0);

    // Nothing has started: 1 step, 0 closed → 0%.
    await expect(card.locator('.plan-progress-label')).toHaveText(/^0\/1 steps · 0%$/);
  });
});

// ---------------------------------------------------------------------------
// parked_assistant — S4 parked assistant chip ON THE INDEX card
// ---------------------------------------------------------------------------
test.describe('/plans index — parked assistant', () => {
  test('renders the parked plan-agent chip (never live/none) on the index card', async ({ page }) => {
    const planId = fixtureId('parked_assistant');
    await page.goto(PLANS_ALL);

    const card = planCard(page, planId);
    await expect(card, 'parked-assistant plan renders on the index').toBeVisible();

    // S4: the plan's sole bound assistant is parked (agent_state=stopping, not in
    // the alive bucket) and no gardener loop is bound, so the unified plan-agent
    // chip's Tone follows the agent → the parked face.
    await expect(
      card.locator('.plan-agent-chip.plan-agent-chip-parked'),
      'a parked-only plan resolves the index chip to the parked face',
    ).toHaveCount(1);
    // It must NOT read live (no working iteration) and must NOT read "not started"
    // (an assistant IS bound — the none-face is for a plan with no assistant).
    await expect(
      card.locator('.plan-agent-chip.plan-agent-chip-live, .plan-agent-chip.plan-agent-chip-running'),
      'a parked assistant never resolves to the live face',
    ).toHaveCount(0);
    await expect(
      card.locator('.plan-agent-chip.plan-agent-chip-none'),
      'a plan with a bound (parked) assistant is not "not started"',
    ).toHaveCount(0);

    // A parked assistant is still reachable — the "terminal →" link renders
    // whenever an assistant is bound (AssistantURL non-empty), parked or live.
    const link = card.locator('a.plan-card-agent-link');
    await expect(link, 'a reachable terminal link renders for the parked assistant').toBeVisible();
    await expect(link).toHaveAttribute('href', /.+/);
  });
});

// ---------------------------------------------------------------------------
// owned_plan (owner scope) — S2 creator-chip SUPPRESSION (the inverse of the
// non-owner render pinned in plans_index_ux.spec.js)
// ---------------------------------------------------------------------------
test.describe('/plans index — creator chip suppression on the owner scope', () => {
  test('the owner does NOT see a redundant self-referential creator chip', async ({ browser }) => {
    const planId = fixtureId('owned_plan');
    // Authenticate AS the owner so the cold-load default scopes the board to the
    // viewer's own operator id (S1 "mine"). planCreatorChipScoped returns "" when
    // the card's operator matches the board scope, so the "by <me>" chip drops.
    const ctx = await browser.newContext({
      extraHTTPHeaders: { 'X-Auth-Request-Email': PLANS_IDX_OWNER },
    });
    try {
      const page = await ctx.newPage();
      const res = await page.goto('/plans');
      expect(res.status(), 'default /plans HTTP status').toBe(200);

      const card = planCard(page, planId);
      await expect(card, 'the owner sees their own plan on the default (mine) scope').toBeVisible();

      // The board is scoped to the owner (the header chip confirms it), so the
      // per-card creator chip is redundant self-reference and is suppressed.
      await expect(page.locator('span.operator-header-chip')).toHaveAttribute('data-state', 'mine');
      await expect(
        card.locator('.plan-card-creator'),
        'the creator chip is suppressed on a board already scoped to this operator',
      ).toHaveCount(0);
    } finally {
      await ctx.close();
    }
  });
});
