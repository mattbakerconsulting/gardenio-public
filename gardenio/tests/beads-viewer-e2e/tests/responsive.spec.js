// Phase T17 (ka-0000001308) of epic ka-0000004611 — responsive behavior at
// the iPhone-class 375x667 viewport. Plan of record:
// `gardenio/plans/beads-viewer-playwright-tests.md`.
//
// Four concerns, four describe blocks:
//
//   1. top nav remains reachable — at 375px the baseline skin's <nav>
//      is too wide to fit on a single line, so it must wrap (or otherwise
//      collapse) instead of clipping links off-screen. Assert every
//      top-level nav link is still visible to the user.
//   2. two-pane layouts stack vertically — `/advice` is the only canonical
//      page with a real two-pane (sidebar + list) layout. The
//      `@media (max-width: 720px)` rule in advice.html collapses
//      `.advice-layout` to a single column; assert that via computed
//      `grid-template-columns`.
//   3. comment form remains usable — on a bead detail page, the
//      <textarea> in templates/partials/comments.html must be visible,
//      sized for the viewport (not zero-width, not extending past the
//      right edge), and accept input. Submit button must remain
//      reachable.
//   4. no horizontal overflow on canonical pages — the catch-all
//      regression detector. For every canonical route, assert
//      `documentElement.scrollWidth` does not exceed the viewport.
//      A 1px tolerance covers sub-pixel rounding; anything beyond is
//      a real bug (an element with a fixed width, an unwrapped flex
//      row, an oversized image).
//
// Notes:
//   - Tests force the iPhone SE viewport via test.use({ viewport }).
//     Per-test setViewportSize() works too but adds noise and forgets
//     to scope back.
//   - Default skin is `baseline` (html[data-skin="baseline"] in
//     layout.html). Other skins ship their own narrow-viewport rules
//     (`@media (max-width: 520px)` for dark-terminal, 640px for tufte);
//     re-running this spec under a different skin would belong to a
//     follow-up if/when the suite gets multi-skin coverage.
//   - The comment form requires CanComment=true, which requires an
//     authenticated user. Same pattern as comment_write.spec.js: set
//     X-Auth-Request-Email so devBypassMiddleware sees a logged-in
//     identity.

import { test, expect } from '@playwright/test';

import { fixtureId } from './_helpers.js';

const MOBILE = { width: 375, height: 667 };

// Source of truth for canonical pages: the routes covered by the T3
// route smoke spec (routes.spec.js). `/search` and bead-detail are
// included so the overflow check covers every page a mobile user is
// likely to land on. The bead-detail route is added per-test below
// because it depends on a fixture id resolved at runtime.
const CANONICAL_ROUTES = [
  '/',
  '/search',
  '/active',
  '/decisions',
  '/agents',
  '/advice',
  '/triggers',
  '/costs',
  '/views',
  '/p/gardenio',
];

test.use({ viewport: MOBILE });

// VIBE_EMAIL is the seeded vibe-mode operator (type=user,
// preferences.vibe_mode=true — seed/fixtures.mjs, gd-1Ct0P1Q4dA). Sending it
// as X-Auth-Request-Email flips the nav to the full ("vibe") experience so the
// vibe-only links render. Keep in sync with the fixture's `email` field and
// VIBE_EMAIL in nav.spec.js.
const VIBE_EMAIL = 'e2e-vibe@example.test';

test.describe('T17 responsive @ 375x667 — top nav', () => {
  // The baseline skin's <nav> is `display: flex; flex-wrap: wrap;` —
  // links shuffle onto multiple lines instead of clipping. These tests
  // catch a regression where flex-wrap is removed (or replaced with
  // a fixed-width hamburger that hides links).
  //
  // The nav is gated on VibeMode (ka-0000003340): simple mode (default)
  // shows only the core links; the full set surfaces under VibeMode. Assert
  // the core links wrap by default, and the full set wraps under vibe mode.

  test('core nav links stay visible on the index (simple mode)', async ({ page }) => {
    await page.goto('/index');
    const nav = page.locator('nav').first();
    await expect(nav).toBeVisible();
    for (const label of ['Active', 'Feed', 'Agents', 'Threads', 'Plans', 'Decisions', 'Escalations', 'Settings']) {
      await expect(nav.getByRole('link', { name: label, exact: true }), `${label} reachable @ 375px`).toBeVisible();
    }
    // Nav search box also stays in the layout — the placeholder is the
    // cheapest "input rendered" probe.
    await expect(nav.locator('input[type="search"]')).toBeVisible();
  });

  test.describe('vibe mode', () => {
    test.use({ extraHTTPHeaders: { 'X-Auth-Request-Email': VIBE_EMAIL } });

    test('every vibe-only nav link stays visible on the index', async ({ page }) => {
      await page.goto('/index');
      const nav = page.locator('nav').first();
      await expect(nav).toBeVisible();
      for (const label of ['Agents', 'Decisions', 'Advice', 'Triggers', 'Costs', 'My Views']) {
        await expect(nav.getByRole('link', { name: label, exact: true }), `${label} reachable @ 375px`).toBeVisible();
      }
      // Nav search box also stays in the layout — the placeholder is the
      // cheapest "input rendered" probe.
      await expect(nav.locator('input[type="search"]')).toBeVisible();
    });
  });
});

test.describe('T17 responsive @ 375x667 — two-pane stacking', () => {
  // The only canonical page with a true two-pane layout is /advice
  // (`.advice-layout { grid-template-columns: 14rem 1fr; }`). Below
  // 720px it collapses to a single column. Assert via computed style
  // — robust to skin and to renamed CSS classes elsewhere.
  test('/advice .advice-layout collapses to a single column', async ({ page }) => {
    await page.goto('/advice');
    const layout = page.locator('.advice-layout');
    await expect(layout).toBeVisible();
    const cols = await layout.evaluate((el) => getComputedStyle(el).gridTemplateColumns);
    // `grid-template-columns` resolves to space-separated track sizes
    // (e.g. "224px 736px" for two columns, "375px" for one). Counting
    // tokens is the simplest "one vs. many tracks" check.
    const trackCount = cols.trim().split(/\s+/).length;
    expect(trackCount, `advice-layout tracks @ 375px: "${cols}"`).toBe(1);
  });
});

test.describe('T17 responsive @ 375x667 — comment form usable', () => {
  test.use({
    extraHTTPHeaders: {
      // CanComment requires a logged-in user. Same pattern as
      // comment_write.spec.js — devBypassMiddleware passes the header
      // through to oidcauth.EmailFromRequest.
      'X-Auth-Request-Email': 'e2e-t17@example.test',
    },
  });

  test('textarea is sized for the viewport and accepts input', async ({ page }) => {
    const id = fixtureId('generic');
    await page.goto(`/${id}`);

    const ta = page.locator('textarea#comment-text');
    await expect(ta).toBeVisible();

    const box = await ta.boundingBox();
    expect(box, 'textarea has a layout box').not.toBeNull();
    // Width should be substantial (> 200px on a 375px viewport — body
    // padding is 1rem so content area is ~343px). Catches a regression
    // where the textarea is shrunk to a single column or hidden.
    expect(box.width, `textarea width @ 375px`).toBeGreaterThan(200);
    // Right edge must not extend past the viewport (1px tolerance for
    // sub-pixel rounding).
    expect(box.x + box.width, `textarea right edge`).toBeLessThanOrEqual(MOBILE.width + 1);

    await ta.fill('mobile e2e draft');
    await expect(ta).toHaveValue('mobile e2e draft');

    // Submit button stays reachable; it's the immediate next sibling
    // in the form, so on narrow viewports it sits below the textarea.
    const submit = page.locator('form.comment-form button[type="submit"]');
    await expect(submit).toBeVisible();
    const sBox = await submit.boundingBox();
    expect(sBox, 'submit has a layout box').not.toBeNull();
    expect(sBox.x + sBox.width, `submit right edge`).toBeLessThanOrEqual(MOBILE.width + 1);
  });
});

test.describe('T17 responsive @ 375x667 — no horizontal overflow', () => {
  // The catch-all check: documentElement.scrollWidth must not exceed
  // the viewport width. Anything wider triggers a horizontal scrollbar
  // on mobile — typically caused by an unwrapped flex row, a
  // fixed-pixel width, or a long unbroken token in user content.
  // Allow 1px tolerance for sub-pixel rendering; anything beyond is
  // a real bug.
  for (const route of CANONICAL_ROUTES) {
    test(`${route} fits within the 375px viewport`, async ({ page }) => {
      await page.goto(route);
      const m = await page.evaluate(() => ({
        scrollWidth: document.documentElement.scrollWidth,
        innerWidth: window.innerWidth,
      }));
      expect(
        m.scrollWidth,
        `${route} scrollWidth=${m.scrollWidth} > innerWidth=${m.innerWidth}`,
      ).toBeLessThanOrEqual(m.innerWidth + 1);
    });
  }

  test('bead-show fits within the 375px viewport', async ({ page }) => {
    const id = fixtureId('generic');
    await page.goto(`/${id}`);
    const m = await page.evaluate(() => ({
      scrollWidth: document.documentElement.scrollWidth,
      innerWidth: window.innerWidth,
    }));
    expect(
      m.scrollWidth,
      `bead-show scrollWidth=${m.scrollWidth} > innerWidth=${m.innerWidth}`,
    ).toBeLessThanOrEqual(m.innerWidth + 1);
  });
});
