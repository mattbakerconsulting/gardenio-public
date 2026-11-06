// Phase T6 (gd-qWeCK0KlRy) of epic ka-0000004611 — ratings tab + per-bead
// rating widget browser-level coverage.
//
// The handlers under test live in gardenio/controller/cmd/beads-viewer/ratings.go:
//
//   GET  /ratings                — list page (handleRatings)
//   GET  /b/{id}/rating          — widget partial (handleRatingWidget)
//   POST /b/{id}/rating          — create-or-update (handleSubmitRating)
//
// Go unit tests already cover the handler surface — see ratings_test.go,
// ratings_submit_test.go, and ratings_helpers_test.go. This spec asserts the
// browser-level end-to-end flow those Go tests cannot:
//
//   1. nav presence — top-nav exposes a /ratings link (covered alongside the
//      other top-nav items in nav.spec.js's TOP_NAV table).
//   2. write-then-read round trip — submit a rating via the form on a seeded
//      bead, see it land in the swapped widget partial as the operator's
//      "Your rating" panel, and then GET /b/{id}/rating standalone (the htmx
//      partial endpoint) to confirm the rating is visible from a fresh
//      fetch (not just from the swap response).
//   3. auth gate — POST /b/{id}/rating WITHOUT an X-Auth-Request-Email
//      header is rejected with 401. The handler has its own defense-in-depth
//      check (`email == ""`) on top of the OIDC middleware; this spec
//      protects that check so a regression does not let unauth POSTs through
//      the dev-bypass.
//   4. htmx swap — clicking the form button replaces #rating-widget via
//      outerHTML, without a full page reload, and the post-swap DOM contains
//      the freshly-rendered "Your rating" panel.
//
// Auth: dev-bypass mode is on (BEADS_VIEWER_DEV_AUTH_BYPASS=1). With no
// header, EmailFromRequest returns "" — `CanRate=false` would hide the form
// entirely. Set X-Auth-Request-Email at the test level so the form renders
// and the handler accepts the POST. The auth-gate test deliberately drops
// the header by issuing the POST through `request.newContext()`.

import { test, expect } from '@playwright/test';

import { fixtureId, waitForHtmxIdle } from './_helpers.js';

// Use the `generic` fixture (a plain task bead with no assignee/actor) so
// the self-rating guard (isSelfRating) does not reject the submit. Per-bead
// widget renders the same way regardless of fixture type — picking the
// simplest keeps the spec focused on the rating contract.
const FIXTURE_KEY = 'generic';

// Per-test rater identities so concurrent workers do not race on the same
// (rated_bead_id, rater) upsert key. The same email used twice across two
// runs is fine — handleSubmitRating treats it as an update and re-renders.
const ROUND_TRIP_EMAIL = 'e2e-t6-rt@example.test';
const HTMX_EMAIL = 'e2e-t6-htmx@example.test';
const PARTIAL_EMAIL = 'e2e-t6-partial@example.test';

test.describe('ratings — write-then-read round trip', () => {
  test.use({ extraHTTPHeaders: { 'X-Auth-Request-Email': ROUND_TRIP_EMAIL } });

  test('POST /b/{id}/rating creates rating and widget reflects it', async ({ page }) => {
    const id = fixtureId(FIXTURE_KEY);
    await page.goto(`/${id}`);

    // Widget renders server-side via the ratings_summary partial. If the
    // form is missing, CanRate gating regressed (or the X-Auth-Request-Email
    // header didn't reach the handler) — bail before the POST so the
    // failure is "form not rendered", not "server rejected".
    const widget = page.locator('#rating-widget');
    await expect(widget).toBeVisible();
    await expect(widget.locator('form.rating-form')).toBeVisible();
    await expect(widget.locator('legend')).toContainText(ROUND_TRIP_EMAIL);

    const score = '4';
    // Unique marker so we can prove the rationale we just submitted is the
    // one rendered back, regardless of any rating lingering from prior
    // runs against the same fixture.
    const marker = `t6-rt-${Date.now()}`;

    await widget.locator('select[name="score"]').selectOption(score);
    await widget.locator('textarea[name="rationale"]').fill(`Round-trip ${marker}`);

    const responsePromise = page.waitForResponse(
      (r) => r.url().includes(`/b/${id}/rating`) && r.request().method() === 'POST',
    );
    await widget.locator('button[type="submit"]').click();
    const resp = await responsePromise;
    expect(resp.status(), 'rating POST status').toBe(200);
    await waitForHtmxIdle(page);

    // Widget swapped via outerHTML — re-locate from the page root so the
    // stale ElementHandle does not poison the next assertion.
    const swapped = page.locator('#rating-widget');
    await expect(swapped).toBeVisible();

    // "Your rating" panel surfaces only when MyRating is set — its presence
    // is the contract that the rating-bead-by-rater lookup found our submit.
    const myRating = swapped.locator('.rating-my');
    await expect(myRating).toBeVisible();
    await expect(myRating.locator('.rating-score-label')).toHaveText(`${score}/5`);
    // Stars helper renders score 4 as ★★★★☆ — assert via the count of filled
    // stars rather than the literal so a font-glyph swap does not flake.
    await expect(myRating.locator('.rating-stars')).toHaveAttribute(
      'aria-label',
      `${score} of 5 stars`,
    );
    await expect(myRating.locator('.rating-rationale')).toContainText(marker);

    // Aggregate updated — Count >= 1, MeanLabel is no longer the empty "—"
    // placeholder. Cannot assert exact count without a clean fixture, but
    // "at least one" + "label is numeric" together prove the aggregate
    // refresh path ran.
    const meanLabel = await swapped.locator('.rating-mean').textContent();
    expect(meanLabel?.trim(), 'mean label is numeric after submit').toMatch(/^\d/);
    await expect(swapped.locator('.rating-count')).toContainText(/\d+ rating/);

    // Form re-rendered in update mode so the next submit replaces (not
    // appends) — legend flips to "Update your rating".
    await expect(swapped.locator('form.rating-form legend')).toContainText(/Update/i);
  });

});

test.describe('ratings — partial endpoint', () => {
  test.use({ extraHTTPHeaders: { 'X-Auth-Request-Email': PARTIAL_EMAIL } });

  test('GET /b/{id}/rating after submit returns fragment with new rating', async ({
    page,
    request,
  }) => {
    const id = fixtureId(FIXTURE_KEY);

    await page.goto(`/${id}`);
    const widget = page.locator('#rating-widget');
    await expect(widget).toBeVisible();
    await widget.locator('select[name="score"]').selectOption('5');
    const marker = `t6-partial-${Date.now()}`;
    await widget.locator('textarea[name="rationale"]').fill(`Partial ${marker}`);
    const responsePromise = page.waitForResponse(
      (r) => r.url().includes(`/b/${id}/rating`) && r.request().method() === 'POST',
    );
    await widget.locator('button[type="submit"]').click();
    const resp = await responsePromise;
    expect(resp.status()).toBe(200);
    await waitForHtmxIdle(page);

    // Hit the htmx partial endpoint directly — handleRatingWidget should
    // emit the same fragment the server-side render produced, with no
    // <html>/<head> chrome. `request` (browser-bound APIRequestContext)
    // inherits extraHTTPHeaders from test.use() so PARTIAL_EMAIL applies.
    const partialRes = await request.get(`/b/${id}/rating`);
    expect(partialRes.status()).toBe(200);
    const body = await partialRes.text();
    // Fragment-only contract — same shape as comments/deps partial endpoints
    // (see htmx_swaps.spec.js). Layout chrome leaking into a partial is the
    // regression that turns an htmx swap into a "bead page inside a bead page".
    expect(body, 'partial must not include <html>').not.toMatch(/<html\b/i);
    expect(body, 'partial must not include <nav>').not.toMatch(/<nav\b/i);
    // Fragment carries the freshly-submitted rating in the MyRating slot.
    expect(body).toContain('rating-my');
    expect(body).toContain(marker);
  });
});

test.describe('ratings — auth gate', () => {
  test('POST without X-Auth-Request-Email returns 401', async ({ playwright, baseURL }) => {
    const id = fixtureId(FIXTURE_KEY);
    // Build a clean request context with NO extraHTTPHeaders so the
    // server sees no auth header. Cannot just `request.post(...)` here:
    // any header from a parent test.use() would still apply.
    const ctx = await playwright.request.newContext({ baseURL });
    try {
      const res = await ctx.post(`/b/${id}/rating`, {
        form: { score: '3', rationale: 'gate-test' },
      });
      // handleSubmitRating defense-in-depth: empty email → 401. The OIDC
      // middleware would normally short-circuit before reaching the
      // handler, but in BEADS_VIEWER_DEV_AUTH_BYPASS the dev-bypass
      // passthrough lets the request through and the handler's own check
      // is the last line of defense — exactly what we want to guard.
      expect(res.status(), 'unauth POST must be rejected').toBe(401);
    } finally {
      await ctx.dispose();
    }
  });

  test('GET widget partial without auth omits the form (CanRate=false)', async ({ playwright, baseURL }) => {
    const id = fixtureId(FIXTURE_KEY);
    const ctx = await playwright.request.newContext({ baseURL });
    try {
      const res = await ctx.get(`/b/${id}/rating`);
      expect(res.status()).toBe(200);
      const body = await res.text();
      // CanRate gates the form — when email is empty the widget renders
      // the read-only summary plus the "Sign in to rate this bead." hint
      // instead. Form leaking through here would let an unauth user see
      // the submit button (cosmetic regression but worth catching).
      expect(body, 'no rating form for unauth viewer').not.toMatch(/<form[^>]*class="rating-form"/);
      expect(body, 'sign-in hint surfaces for unauth viewer').toContain('Sign in to rate');
    } finally {
      await ctx.dispose();
    }
  });
});

test.describe('ratings — htmx swap', () => {
  test.use({ extraHTTPHeaders: { 'X-Auth-Request-Email': HTMX_EMAIL } });

  test('submit replaces #rating-widget without a full reload', async ({ page }) => {
    const id = fixtureId(FIXTURE_KEY);
    await page.goto(`/${id}`);
    await expect(page.locator('#rating-widget')).toBeVisible();

    // Instrumentation mirrors htmx_swaps.spec.js — a window-scope sentinel
    // and the document's navigationStart prove the page did not reload.
    // afterSwap targets prove htmx fired the lifecycle event on the right
    // element (a missing/wrong target is the "swap-but-not-our-target"
    // regression that lets stale fragments stick).
    await page.evaluate(() => {
      window.__navSentinel = 'preserved';
      window.__afterSwapTargets = [];
      document.body.addEventListener('htmx:afterSwap', (e) => {
        const t = e.target;
        window.__afterSwapTargets.push((t && t.id) || (t && t.tagName) || 'unknown');
      });
    });

    const widget = page.locator('#rating-widget');
    await widget.locator('select[name="score"]').selectOption('3');
    await widget.locator('textarea[name="rationale"]').fill(`htmx swap ${Date.now()}`);
    const responsePromise = page.waitForResponse(
      (r) => r.url().includes(`/b/${id}/rating`) && r.request().method() === 'POST',
    );
    await widget.locator('button[type="submit"]').click();
    const resp = await responsePromise;
    expect(resp.status()).toBe(200);
    await waitForHtmxIdle(page);

    // Sentinel preserved → no full reload (which would wipe window).
    const sentinel = await page.evaluate(() => window.__navSentinel);
    expect(sentinel, 'window scope survived swap').toBe('preserved');

    // afterSwap fired with #rating-widget as the target. Form-target swaps
    // bubble to the htmx-resolved target id, not the originating button.
    const targets = await page.evaluate(() => window.__afterSwapTargets);
    expect(targets, 'afterSwap fired on the widget').toContain('rating-widget');

    // Post-swap DOM has the new MyRating panel — proves the swap landed
    // and the response body actually replaced the existing widget.
    const swapped = page.locator('#rating-widget .rating-my');
    await expect(swapped).toBeVisible();
    await expect(swapped.locator('.rating-score-label')).toHaveText('3/5');

    // Top-<nav> chrome still rendered — a partial swap that accidentally
    // included the layout would double-render the nav. Assert a CORE link:
    // this viewer (HTMX_EMAIL) is not a vibe user, so the Ratings link is
    // gated out by {{if .VibeMode}} (ka-0000003340). The Ratings nav link's
    // own visibility is covered in nav.spec.js's vibe-mode describe.
    await expect(page.locator('nav').first()).toBeVisible();
    await expect(page.locator('nav').first().getByRole('link', { name: 'Decisions', exact: true })).toBeVisible();
  });
});
