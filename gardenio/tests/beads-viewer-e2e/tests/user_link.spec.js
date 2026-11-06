// Backfill (ka-0000003378): clickable operator names → /u/{slug} navigation
// (ka-0000007502, ka-0000007787). Operator names across the app became links
// to the per-user homepage; the end-to-end "click a name, land on the right
// homepage" flow shipped with no behavioral spec.
//
// CANONICAL clickable name = the /people directory row name (a.people-row-name),
// which reliably renders in the harness and is the primary entry point to a
// person's homepage. This spec drives the real click and asserts the resulting
// navigation lands on /u/{slug} and renders that operator's page.
//
// The renderUserLink helper (user_link.go) emits a SECOND clickable-name shape,
// `a.user-link`, on /agents / /gardeners / comment authors. That variant only
// renders when the bead's operator carries a non-empty display in the operators
// table — a daemon-side field the hermetic harness does not populate for its
// seeded fixtures — so it is unit-tested (user_link_test.go) rather than pinned
// here. The final test defensively verifies that IF any a.user-link is present,
// it points at a /u/ homepage (a cheap guard that never blocks on absence).
//
// Keep PEOPLE_* in sync with seed/fixtures.mjs.

import { expect, test } from '@playwright/test';

const PEOPLE_SLUG = 'e2e-people';
const PEOPLE_DISPLAY = 'E2E People Directory';

test.describe('clickable operator name → /u/{slug}', () => {
  test('the directory name link carries the /u/{slug} href', async ({ page }) => {
    await page.goto('/people');
    const nameLink = page.locator(`a.people-row-name[href="/u/${PEOPLE_SLUG}"]`);
    await expect(nameLink, 'operator name is a link to the homepage').toBeVisible();
    await expect(nameLink).toHaveText(PEOPLE_DISPLAY);
  });

  test('clicking the operator name navigates to their homepage', async ({ page }) => {
    await page.goto('/people');
    const nameLink = page.locator(`a.people-row-name[href="/u/${PEOPLE_SLUG}"]`);
    await expect(nameLink).toBeVisible();

    await Promise.all([
      page.waitForURL(`**/u/${PEOPLE_SLUG}`),
      nameLink.click(),
    ]);

    // Landed on the operator's per-user homepage: the h1 is their display name.
    await expect(page.locator('h1').first()).toContainText(PEOPLE_DISPLAY);
    await expect(page.locator('.profile-card').first()).toBeVisible();
  });

  test('any rendered a.user-link points at a /u/ homepage', async ({ page }) => {
    // Defensive, absence-tolerant: renderUserLink's a.user-link shape needs a
    // daemon-populated operator display the harness fixtures do not carry, so
    // this asserts the INVARIANT (every user-link is a homepage link) over
    // whatever links happen to render, and no-ops cleanly when none do.
    for (const path of ['/agents', '/gardeners', '/decisions']) {
      await page.goto(path);
      const links = page.locator('a.user-link');
      const n = await links.count();
      for (let i = 0; i < n; i += 1) {
        const href = await links.nth(i).getAttribute('href');
        expect(href, `${path} user-link #${i} targets a homepage`).toMatch(
          /\/u\/[^/]+$/,
        );
      }
    }
  });
});
