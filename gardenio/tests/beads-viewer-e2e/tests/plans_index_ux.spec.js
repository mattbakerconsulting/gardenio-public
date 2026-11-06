// /plans INDEX UX — the three headline plan shapes as stable, id-addressable
// cards (task ka-0000004239; epic gd-mQnycuToAV / gd-80TfM5mPtd lineage).
//
// This spec seeds on THREE whole-plan fixtures (seed/fixtures.mjs) by bead-id
// and asserts each renders its distinct index shape against the real daemon:
//
//   owned_plan     — a LIVE, HUMAN-OWNED plan. Pins S2 (the creator chip: an
//                    open plan attributed to a real directory operator renders
//                    a "by <human>" operator-chip-compact that links into the
//                    /plans-scoped filter, never /search) AND S1 (the owner's
//                    own default /plans scope shows it under the "mine" chip).
//                    The plan is unfinished (one open step), so the card also
//                    surfaces a "now" step + a non-100% rollup.
//   live_assistant — a LIVE plan bound to a working plan-assistant (plus a
//                    second PARKED assistant on the same plan). Pins S4: the
//                    index PickBetter (live > parked) resolves the card's
//                    plan-agent chip to the LIVE face + a reachable "terminal →"
//                    link — the distinction the ad-hoc "not started" plan cannot
//                    show.
//   all_closed     — a fully CLOSED plan whose every descendant step is closed.
//                    Pins S3: under ?status=all it renders a terminal card with
//                    a 100% rollup and NO now/next step chip (a finished plan has
//                    no current-or-next step).
//
// WHY SEEDED (not ephemeral): plans_scoping.spec.js already mints ephemeral
// owned plans per-run to drive the scope BOUNDARY; this spec pins the three
// index SHAPES against durable, label-stable fixtures so a card-render
// regression is caught by a fixed id rather than a per-run mint. All three are
// read-only here — no test mutates them — so the shared fixtures are safe under
// fullyParallel:true.
//
// AUTH / SCOPING (dev-bypass harness, playwright.config webServer):
//   BEADS_VIEWER_DEV_AUTH_BYPASS=1 promotes X-Auth-Request-Email into the
//   verified viewer identity. The /plans index default-scopes to the viewer's
//   own operated_by, and the seed attributes owned_plan / live_assistant to a
//   DIFFERENT operator (PLANS_IDX_OWNER), so every navigation that must see a
//   fixture regardless of viewer uses `/plans?operated_by=` (the empty-key
//   "show all" form) — the same pattern plans.spec.js uses. The S1 "owner sees
//   their own plan" case deliberately authenticates AS the owner so the
//   cold-load default scope surfaces it.

import { test, expect } from '@playwright/test';

import { fixtureId } from './_helpers.js';

// Keep in sync with the operatedBy on owned_plan / live_assistant in
// seed/fixtures.mjs. The owner is a real directory human on the non-fixture
// @example.test domain (the seed's global e2e-fixture identity is screened out
// of operator surfaces), so the creator chip resolves a human chip.
const PLANS_IDX_OWNER = 'e2e-plans-owner@example.test';

// Show-all: the empty operated_by key drops the default "scope to me" filter so
// a plan authored by a DIFFERENT alias renders for any viewer (handlePlans
// treats an explicit — even empty — operated_by as "the viewer meant it").
const PLANS_ALL = '/plans?operated_by=';
// All-closed plans are terminal, so the active-only default index hides them;
// ?status=all widens the daemon query to every status.
const PLANS_ALL_STATUSES = '/plans?operated_by=&status=all';
// The default /plans view is the BOARD, whose Done lane back-fills a bounded
// recent-closed fetch (plansBoardClosedLimit) — so a closed fixture leaks into
// the active-default board whenever the daemon holds fewer than that many closed
// plans (order/crowding-dependent flake, gd-Uo48luAmpW). The GRID view keeps the
// single active-scoped query with no Done-lane back-fill, so "a closed plan is
// not in the active set" is asserted deterministically here.
const PLANS_ACTIVE_GRID = '/plans?operated_by=&view=grid';

const planCard = (page, id) => page.locator(`li.plan-card[data-plan-id="${id}"]`);

// ---------------------------------------------------------------------------
// owned_plan — S2 creator chip + S1 default-scope "mine"
// ---------------------------------------------------------------------------
test.describe('/plans index — owned live plan', () => {
  test('renders a human creator chip that links into /plans (not /search)', async ({ page }) => {
    const planId = fixtureId('owned_plan');
    // Show-all as the DEFAULT harness viewer (a different identity than the
    // owner): the creator chip is suppressed only when the card's operator
    // matches the viewer's scope, so a non-owner viewer sees the "by <human>"
    // chip.
    await page.goto(PLANS_ALL);

    const card = planCard(page, planId);
    await expect(card, 'owned plan renders on the show-all index').toBeVisible();

    // S2: the creator chip is a compact operator chip inside .plan-card-creator,
    // carrying the resolved operator id and pointing at a /plans-scoped or
    // /u/{slug} href — never a /search filter.
    const creator = card.locator('.plan-card-creator .operator-chip-compact');
    await expect(creator, 'a human creator chip renders on a non-owner board').toBeVisible();
    await expect(creator).toHaveAttribute('data-operated-by', /.+/);
    const href = await creator.getAttribute('href');
    expect(href, 'creator chip links into /plans or /u, not /search').not.toMatch(/\/search/);
    expect(href, 'creator chip is a plans-scoped or slug link').toMatch(/\/plans\?operated_by=|\/u\//);

    // The plan is unfinished: an open, in-progress step is surfaced as the "now"
    // step and the rollup is below 100% (1 step, 0 closed → 0%).
    const cardStep = card.locator('.plan-card-step');
    await expect(cardStep, 'an unfinished plan surfaces its current step').toBeVisible();
    await expect(cardStep.locator('.plan-step-kind-now')).toHaveText('now');
    await expect(card.locator('.plan-progress-label')).toHaveText(/^0\/1 steps · 0%$/);
  });

  test('the owner sees their plan on the default /plans under the "mine" chip', async ({ browser }) => {
    const planId = fixtureId('owned_plan');
    // Authenticate AS the owner: with no operated_by key the cold-load default
    // scopes the query to the viewer's own operator id, and the owned plan is
    // theirs.
    const ctx = await browser.newContext({
      extraHTTPHeaders: { 'X-Auth-Request-Email': PLANS_IDX_OWNER },
    });
    try {
      const page = await ctx.newPage();
      const res = await page.goto('/plans');
      expect(res.status(), 'default /plans HTTP status').toBe(200);

      await expect(
        planCard(page, planId),
        'the owner sees their own plan on the default (mine) scope',
      ).toBeVisible();

      // The header chip confirms the cold-load default scoped to the viewer
      // (the mechanism that made the plan visible is a real scope, not an
      // unscoped fallback).
      await expect(page.locator('span.operator-header-chip')).toHaveAttribute('data-state', 'mine');
    } finally {
      await ctx.close();
    }
  });
});

// ---------------------------------------------------------------------------
// live_assistant — S4 live-vs-parked assistant resolution
// ---------------------------------------------------------------------------
test.describe('/plans index — live plan assistant', () => {
  test('resolves the LIVE assistant (over the parked one) into the plan-agent chip + terminal link', async ({
    page,
  }) => {
    const planId = fixtureId('live_assistant');
    await page.goto(PLANS_ALL);

    const card = planCard(page, planId);
    await expect(card, 'live-assistant plan renders on the index').toBeVisible();

    // S4: two plan-assistants bind to this plan — one live (agent_state=working,
    // fresh), one parked (agent_state=stopping). The index PickBetter picks the
    // live one, so the card's plan-agent chip renders the LIVE face (green
    // running/live tone) and NEVER the parked/none face.
    await expect(
      card.locator('.plan-agent-chip.plan-agent-chip-live, .plan-agent-chip.plan-agent-chip-running'),
      'the live assistant resolves the chip to the live face',
    ).toHaveCount(1);
    await expect(
      card.locator('.plan-agent-chip.plan-agent-chip-parked'),
      'the parked co-bound assistant never wins the chip',
    ).toHaveCount(0);
    await expect(
      card.locator('.plan-agent-chip.plan-agent-chip-none'),
      'a plan with a bound assistant is not "not started"',
    ).toHaveCount(0);

    // The live agent is reachable: the "terminal →" click-through renders only
    // when an assistant is bound (AssistantURL is non-empty), pointing at the
    // agent's terminal / bead page.
    const link = card.locator('a.plan-card-agent-link');
    await expect(link, 'a reachable terminal link renders for the live assistant').toBeVisible();
    await expect(link).toHaveAttribute('href', /.+/);
  });
});

// ---------------------------------------------------------------------------
// all_closed — S3 terminal card, no now/next step chip
// ---------------------------------------------------------------------------
test.describe('/plans index — all-closed plan', () => {
  test('is hidden on the active-only index and visible under ?status=all', async ({ page }) => {
    const planId = fixtureId('all_closed');

    // Active-only view: a fully-closed plan is not in the active set. Assert on
    // the GRID view — the default board's Done lane back-fills a bounded
    // recent-closed fetch, so a closed fixture leaks into the active-default
    // board whenever the daemon holds fewer than that many closed plans (an
    // order/crowding-dependent flake). The grid keeps the single active-scoped
    // query with no Done-lane back-fill, so this leg is deterministic.
    await page.goto(PLANS_ACTIVE_GRID);
    await expect(
      planCard(page, planId),
      'a closed plan is hidden on the active-only grid index',
    ).toHaveCount(0);

    // ?status=all widens the query to every status, so the closed plan renders.
    await page.goto(PLANS_ALL_STATUSES);
    await expect(
      planCard(page, planId),
      'the closed plan renders under ?status=all',
    ).toBeVisible();
  });

  test('renders a terminal card with a 100% rollup and NO now/next step chip', async ({ page }) => {
    const planId = fixtureId('all_closed');
    await page.goto(PLANS_ALL_STATUSES);

    const card = planCard(page, planId);
    await expect(card, 'all-closed plan renders under ?status=all').toBeVisible();

    // S3: every rendered step is closed, so there is no current-or-next step —
    // the card omits the .plan-card-step chip entirely.
    await expect(
      card.locator('.plan-card-step'),
      'an all-closed plan surfaces no now/next step chip',
    ).toHaveCount(0);

    // The single step is closed → the progress rollup reads 100%.
    await expect(card.locator('.plan-progress-label')).toHaveText(/^1\/1 steps · 100%$/);
  });
});
