// Backfill (ka-0000003378): behavioral coverage for the per-user homepage
// /u/{slug} (epic M2/M3, gd-fJIwOb4QO / ka-0000003587) — the page a
// clickable operator name lands on. Shipped with no dedicated e2e spec.
//
// The anchor operator is the one seeded by people_dir_open_task /
// people_dir_user: oidc:e2e-people@example.test → slug 'e2e-people', display
// 'E2E People Directory'. See tests/people.spec.js for why a dedicated
// non-fixture-domain operator is required. Keep PEOPLE_* in sync with
// seed/fixtures.mjs.
//
// What this spec covers:
//   1. /u/{slug} renders the shell + <h1>{display} + the profile card.
//   2. An unknown slug is a 404 (not a soft empty page).
//   3. The status-message block renders on the profile card for every viewer.
//   4. Self vs peer identity (dev-bypass promotes X-Auth-Request-Email into
//      the verified viewer identity): the OWNER sees the "(you)" marker + the
//      self-only status affordance; a PEER sees neither.
//   5. The magic slugs /u/me + /me 308-redirect an authenticated viewer to
//      their own /u/{slug}, and bounce an anonymous viewer to /login.

import { expect, test } from '@playwright/test';

const PEOPLE_SLUG = 'e2e-people';
const PEOPLE_EMAIL = 'e2e-people@example.test';
const PEOPLE_DISPLAY = 'E2E People Directory';
// A DIFFERENT authenticated viewer — provisioned lazily by the daemon the
// moment we send its email, but never equal to PEOPLE_EMAIL, so it is always
// a peer of /u/e2e-people.
const PEER_EMAIL = 'e2e-peer@example.test';

test.describe('GET /u/{slug} — per-user homepage', () => {
  test('renders the profile card with the operator display name', async ({ page }) => {
    const res = await page.goto(`/u/${PEOPLE_SLUG}`);
    expect(res, 'navigation response').not.toBeNull();
    expect(res.status(), '/u/{slug} HTTP status').toBeLessThan(400);
    await expect(page.locator('nav').first()).toBeVisible();
    // The h1 is the operator's display name (resolved through the user-bead
    // join), the cheapest "right page rendered" signal.
    await expect(page.locator('h1').first()).toContainText(PEOPLE_DISPLAY);
    await expect(page.locator('.profile-card').first()).toBeVisible();
    // The status-message block lives on the profile card and renders for every
    // viewer (read-only for peers). Assert it is ATTACHED, not visible: an
    // anonymous/peer viewer of an operator with no status set gets an EMPTY
    // block (no text, no self-only button) which collapses to zero size, so
    // toBeVisible would flake — presence in the DOM is the real contract.
    await expect(page.locator('#user-status-message')).toBeAttached();
  });

  test('an unknown slug is a 404', async ({ request }) => {
    const res = await request.get('/u/no-such-slug-xyz', { maxRedirects: 0 });
    expect(res.status(), 'unknown slug status').toBe(404);
  });

  test.describe('viewer identity (self vs peer)', () => {
    test('the OWNER sees the "(you)" marker and the self status affordance', async ({
      browser,
    }) => {
      // A fresh context scoped to the owner's identity header so this test's
      // viewer identity never leaks into the peer test running in parallel.
      const ctx = await browser.newContext({
        extraHTTPHeaders: { 'X-Auth-Request-Email': PEOPLE_EMAIL },
      });
      const page = await ctx.newPage();
      try {
        await page.goto(`/u/${PEOPLE_SLUG}`);
        // "(you)" marker in the h1 proves isViewerSelf resolved true.
        await expect(page.locator('h1').first()).toContainText('(you)');
        // Self-only affordance: an empty status renders the "Set a status
        // message" button; a present one renders "Edit". Either proves the
        // self editor surface is offered to the owner.
        await expect(
          page.locator(
            '.user-status-message-set-button, .user-status-message-edit-button',
          ),
        ).toBeVisible();
      } finally {
        await ctx.close();
      }
    });

    test('a PEER sees no "(you)" marker and no self status affordance', async ({
      browser,
    }) => {
      const ctx = await browser.newContext({
        extraHTTPHeaders: { 'X-Auth-Request-Email': PEER_EMAIL },
      });
      const page = await ctx.newPage();
      try {
        await page.goto(`/u/${PEOPLE_SLUG}`);
        // The page still renders (peers can view any homepage)…
        await expect(page.locator('h1').first()).toContainText(PEOPLE_DISPLAY);
        await expect(page.locator('#user-status-message')).toBeAttached();
        // …but carries none of the self-only surface.
        await expect(page.locator('h1').first()).not.toContainText('(you)');
        await expect(
          page.locator(
            '.user-status-message-set-button, .user-status-message-edit-button',
          ),
          'peer gets no self status-edit affordance',
        ).toHaveCount(0);
      } finally {
        await ctx.close();
      }
    });
  });
});

test.describe('magic slugs — /u/me and /me', () => {
  test('/u/me 308-redirects an authenticated viewer to their own homepage', async ({
    browser,
  }) => {
    const ctx = await browser.newContext({
      extraHTTPHeaders: { 'X-Auth-Request-Email': PEOPLE_EMAIL },
    });
    try {
      const res = await ctx.request.get('/u/me', { maxRedirects: 0 });
      expect(res.status(), '/u/me redirect status').toBe(308);
      expect(res.headers()['location'], '/u/me Location').toContain(
        `/u/${PEOPLE_SLUG}`,
      );
    } finally {
      await ctx.close();
    }
  });

  test('/me 308-redirects an authenticated viewer to their own homepage', async ({
    browser,
  }) => {
    const ctx = await browser.newContext({
      extraHTTPHeaders: { 'X-Auth-Request-Email': PEOPLE_EMAIL },
    });
    try {
      const res = await ctx.request.get('/me', { maxRedirects: 0 });
      expect(res.status(), '/me redirect status').toBe(308);
      expect(res.headers()['location'], '/me Location').toContain(
        `/u/${PEOPLE_SLUG}`,
      );
    } finally {
      await ctx.close();
    }
  });

  test('/me bounces an anonymous viewer to /login', async ({ request }) => {
    // No auth header → no verified identity → the resolver finds no operator,
    // so /me 302s to the login start with a next= back to /me.
    const res = await request.get('/me', { maxRedirects: 0 });
    expect(res.status(), 'anonymous /me status').toBe(302);
    expect(res.headers()['location'], 'anonymous /me Location').toContain('/login');
  });
});
