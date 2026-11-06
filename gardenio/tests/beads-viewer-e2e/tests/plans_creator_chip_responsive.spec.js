// /plans INDEX UX — the S2 creator chip's RESPONSIVE COMPACT LAYOUT
// (task gd-5SQhgRqqcN; epic gd-mQnycuToAV / gd-80TfM5mPtd lineage).
//
// plans_index_ux.spec.js pins that the S2 creator chip RENDERS on a non-owner
// board (`.plan-card-creator .operator-chip-compact` with a `data-operated-by`
// id + a /plans-scoped-or-/u href, never /search) and plans_index_arms.spec.js
// pins its SUPPRESSION on the owner's own scope. Both assert at the default
// desktop viewport. What neither exercises is the OTHER half of the S2 contract
// named in the task — the chip is `operator-chip-compact`: a fixed-size initials
// PILL that must survive a phone-class viewport WITHOUT clipping off the card or
// pushing the page into a horizontal scroll. A regression that unwrapped the
// card head, dropped the compact sizing, or let the chip overrun the card edge
// would sail past the desktop-only specs; this spec catches it.
//
// WHAT "responsive compact layout" MEANS here, made concrete against the real
// render (measured 2026-08-20 against the seeded owned_plan at 375px):
//   1. the compact chip is still VISIBLE inside the card at 375x667;
//   2. it keeps its compact idiom — `.plan-card-creator` stays a flex row and
//      the chip's rendered font-size is <= its 0.75rem card-context ceiling
//      (the pill is ~22x14px, not a full-width block);
//   3. it does NOT overrun the card's right edge (no clipped / cut-off chip);
//   4. the card carrying it introduces NO page-level horizontal overflow — the
//      catch-all responsive.spec.js overflow detector, narrowed to the one card
//      that grows a creator chip.
// It also still carries the S2 DATA contract at the narrow viewport — a
// `data-operated-by` id and a plans-scoped-or-/u href, never /search — so a
// responsive reflow can't quietly strip the chip's identity or relink it.
//
// EDGE (no chip when none applies): the all_closed fixture is attributed to the
// SAME PLANS_IDX_OWNER but carries no distinguishing creator on the owner's own
// board — and, more to the point for the responsive cut, a card with NO creator
// chip must ALSO stay within its bounds at 375px. We assert the owner-scoped
// board (where planCreatorChipScoped suppresses the self-referential chip)
// renders the owned_plan card with zero `.plan-card-creator` AND no horizontal
// overflow, so "no chip" is a real absence, not a clipped one.
//
// WHY SEEDED (not ephemeral): same rationale as plans_index_ux /
// plans_index_arms — pin a durable card layout against label-stable fixtures so
// a responsive-reflow regression is caught by a fixed id, not a per-run mint.
// Every fixture is read-only here (no test mutates them), so they are safe under
// fullyParallel:true.
//
// AUTH / SCOPING (dev-bypass harness, playwright.config webServer):
//   BEADS_VIEWER_DEV_AUTH_BYPASS=1 promotes X-Auth-Request-Email into the
//   verified viewer identity. The /plans index default-scopes to the viewer's own
//   operated_by; the owner fixtures are attributed to PLANS_IDX_OWNER, so the
//   non-owner render case uses `/plans?operated_by=` (the empty-key "show all"
//   form) and the suppression/edge case authenticates AS the owner.

import { test, expect } from '@playwright/test';

import { fixtureId } from './_helpers.js';

// Keep in sync with the operatedBy on the owner fixtures in seed/fixtures.mjs and
// with PLANS_IDX_OWNER in plans_index_ux.spec.js / plans_index_arms.spec.js.
const PLANS_IDX_OWNER = 'e2e-plans-owner@example.test';

// Show-all: the empty operated_by key drops the default "scope to me" filter so a
// plan authored by a DIFFERENT alias renders its creator chip for any viewer.
const PLANS_ALL = '/plans?operated_by=';

// iPhone-SE class viewport — the same narrow target responsive.spec.js uses for
// the page-wide overflow sweep. The creator chip must stay compact + on-card here.
const MOBILE = { width: 375, height: 667 };

// The compact chip's card-context font ceiling. layout.html sets
// `.operator-chip-compact { font-size: 0.75em }` and .plan-card-creator sets its
// own `font-size: 0.75rem`, so the RESOLVED pixel size composes to <= 12px
// (0.75rem × 0.75em against the 16px root). A regression that dropped the compact
// sizing (e.g. the chip inheriting a full 1rem body size) would blow past this.
const COMPACT_FONT_CEILING_PX = 12;

const planCard = (page, id) => page.locator(`li.plan-card[data-plan-id="${id}"]`);

// Assert the page has no horizontal overflow (1px tolerance for sub-pixel
// rounding), mirroring responsive.spec.js's catch-all detector.
async function expectNoHorizontalOverflow(page) {
  const { scrollWidth, clientWidth } = await page.evaluate(() => ({
    scrollWidth: document.documentElement.scrollWidth,
    clientWidth: document.documentElement.clientWidth,
  }));
  expect(scrollWidth, 'no page-level horizontal overflow at 375px').toBeLessThanOrEqual(
    clientWidth + 1,
  );
}

test.use({ viewport: MOBILE });

// ---------------------------------------------------------------------------
// owned_plan (non-owner board) — the creator chip renders COMPACT + on-card at
// a phone viewport, and keeps its S2 data contract.
// ---------------------------------------------------------------------------
test.describe('/plans index — creator chip responsive compact layout @ 375px', () => {
  test('the creator chip stays a compact, on-card pill at 375px', async ({ page }) => {
    const planId = fixtureId('owned_plan');
    // Show-all as the default harness viewer (a DIFFERENT identity than the
    // owner) so the "by <human>" creator chip is present (it is suppressed only
    // on a board already scoped to the card's own operator).
    await page.goto(PLANS_ALL);

    const card = planCard(page, planId);
    await expect(card, 'owned plan renders on the show-all index @ 375px').toBeVisible();
    // Bring the card into view so its bounding box is real (an off-screen box
    // can report a stale/zero rect on some engines).
    await card.scrollIntoViewIfNeeded();

    const creator = card.locator('.plan-card-creator');
    const chip = creator.locator('.operator-chip-compact');
    await expect(chip, 'the compact creator chip is visible at a phone width').toBeVisible();

    // (2) compact idiom — the creator group is a flex row (not a stacked/block
    // reflow) and the chip's resolved font size stays within the compact ceiling.
    await expect(creator).toHaveCSS('display', 'flex');
    const fontPx = await chip.evaluate((el) => parseFloat(getComputedStyle(el).fontSize));
    expect(fontPx, 'compact chip font stays within its card-context ceiling').toBeLessThanOrEqual(
      COMPACT_FONT_CEILING_PX,
    );

    // (3) the chip does not overrun the card's right edge — a clipped/cut-off
    // chip is the classic narrow-viewport regression.
    const chipBox = await chip.boundingBox();
    const cardBox = await card.boundingBox();
    expect(chipBox, 'chip has a rendered box').not.toBeNull();
    expect(cardBox, 'card has a rendered box').not.toBeNull();
    expect(
      chipBox.x + chipBox.width,
      'the compact chip stays within the card right edge (not clipped)',
    ).toBeLessThanOrEqual(cardBox.x + cardBox.width + 1);

    // (1)+(4): the chip is on-card AND the card grows no page-level horizontal
    // scroll at 375px.
    await expectNoHorizontalOverflow(page);

    // The S2 DATA contract survives the narrow reflow: a resolved operator id and
    // a plans-scoped-or-/u href, never a /search filter.
    await expect(chip).toHaveAttribute('data-operated-by', /.+/);
    const href = await chip.getAttribute('href');
    expect(href, 'creator chip links into /plans or /u, not /search @ 375px').not.toMatch(
      /\/search/,
    );
    expect(href, 'creator chip is a plans-scoped or slug link @ 375px').toMatch(
      /\/plans\?operated_by=|\/u\//,
    );
  });

  // -------------------------------------------------------------------------
  // EDGE — no creator chip when none applies: on the owner's OWN board the
  // self-referential chip is suppressed, and its absence must be a real
  // absence (zero elements), not a clipped one — and the card still fits 375px.
  // -------------------------------------------------------------------------
  test('a card with no creator chip still fits the viewport at 375px', async ({ browser }) => {
    const planId = fixtureId('owned_plan');
    // Authenticate AS the owner so the cold-load default scopes the board to the
    // viewer's own operator id; planCreatorChipScoped then returns "" for the
    // card's own operator, so the "by <me>" chip drops entirely.
    const ctx = await browser.newContext({
      viewport: MOBILE,
      extraHTTPHeaders: { 'X-Auth-Request-Email': PLANS_IDX_OWNER },
    });
    try {
      const page = await ctx.newPage();
      const res = await page.goto('/plans');
      expect(res.status(), 'default /plans HTTP status @ 375px').toBe(200);

      const card = planCard(page, planId);
      await expect(card, 'the owner sees their own plan on the default scope @ 375px').toBeVisible();
      await card.scrollIntoViewIfNeeded();

      // The board is scoped to the owner, so the per-card creator chip is
      // suppressed — a true absence, not a chip clipped off the card.
      await expect(page.locator('span.operator-header-chip')).toHaveAttribute('data-state', 'mine');
      await expect(
        card.locator('.plan-card-creator'),
        'the creator chip is suppressed (absent, not clipped) on the owner scope',
      ).toHaveCount(0);

      // A chip-less card still fits the narrow viewport with no horizontal scroll.
      await expectNoHorizontalOverflow(page);
    } finally {
      await ctx.close();
    }
  });
});
