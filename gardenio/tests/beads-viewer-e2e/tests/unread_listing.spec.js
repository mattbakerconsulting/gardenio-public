// Phase of ka-0000002605 — per-bead unread-comment indicator on listings
// (server bead-show writer + JS listing decorator) browser-level coverage.
//
// What this asserts:
//   1. A listing row whose `data-last-activity-ts` is later than anything in
//      `localStorage["bv:lastSeen:v1"]` for that bead is decorated with an
//      unread dot — `.bead-row-unread-dot` is the load-bearing visual.
//   2. Visiting the bead-show page advances `lastSeen[beadId]` to the
//      max of `data-comments-latest` and `data-bead-updated-at`. After that,
//      the dot does not return on a fresh listing render.
//   3. Pre-seeding lastSeen[id] with a future stamp suppresses the dot on a
//      cold listing render — the decorator is reading the map, not just any
//      "no record yet" state.
//
// We test against `/decisions` because the `decision` fixture is an open
// decision bead and the page lists all open decisions regardless of project
// label — so the row is reliably present without a per-project filter.
// The same JS handles every listing page (active / decisions / agents /
// advice / schedules / project), so one happy-path covers them.

import { test, expect } from '@playwright/test';

import { fixtureId, waitForHtmxIdle } from './_helpers.js';

const LISTING_PATH = '/decisions';

test.describe('per-bead unread-comment indicator', () => {
  test.beforeEach(async ({ page }) => {
    // Each test starts with a clean localStorage slate so prior runs
    // (and other suites) don't leak `bv:lastSeen:v1` state across tests.
    // Goto a same-origin page first; localStorage on about:blank is a no-op.
    await page.goto(LISTING_PATH);
    await page.evaluate(() => window.localStorage.removeItem('bv:lastSeen:v1'));
  });

  test('unread row gets dot; visiting bead-show clears it', async ({ page }) => {
    const id = fixtureId('decision');

    // Initial visit to the listing — with no prior `lastSeen` for this bead,
    // and a non-empty `data-last-activity-ts`, the row should pick up a dot
    // after the JS runs.
    await page.goto(LISTING_PATH);
    await waitForHtmxIdle(page);

    const row = page.locator(`li.bead-row[data-bead-id="${id}"]`);
    await expect(row, 'fixture row must render on /decisions').toBeVisible();
    await expect(row).toHaveClass(/bead-row-unread/);
    await expect(row.locator(':scope > .bead-row-unread-dot')).toHaveCount(1);

    // Visit the bead-show page — the writer should advance lastSeen[id] to
    // max(data-comments-latest, data-bead-updated-at). We reach in via
    // localStorage to confirm the writer fired (the dot disappearing on
    // the listing is the user-visible signal, asserted below).
    await page.goto(`/${id}`);
    await waitForHtmxIdle(page);

    const seen = await page.evaluate(() => {
      try {
        return JSON.parse(window.localStorage.getItem('bv:lastSeen:v1') || '{}');
      } catch (e) {
        return null;
      }
    });
    expect(seen, 'localStorage map must parse').toBeTruthy();
    expect(seen[id], `lastSeen[${id}] must be advanced by bead-show visit`).toBeTruthy();

    // Re-render the listing — the dot must be gone now that the row's
    // data-last-activity-ts is no longer later than lastSeen[id].
    await page.goto(LISTING_PATH);
    await waitForHtmxIdle(page);
    const rowAfter = page.locator(`li.bead-row[data-bead-id="${id}"]`);
    await expect(rowAfter).toBeVisible();
    await expect(rowAfter).not.toHaveClass(/bead-row-unread/);
    await expect(rowAfter.locator(':scope > .bead-row-unread-dot')).toHaveCount(0);
  });

  test('localStorage stamp suppresses dot on a fresh listing render', async ({ page }) => {
    const id = fixtureId('decision');

    // Pre-seed `lastSeen[id]` to a far-future timestamp so the listing's
    // `data-last-activity-ts` (the bead's UpdatedAt) is older. This proves
    // the decorator reads the localStorage map (not just any state) and
    // the dot is suppressed without round-tripping bead-show.
    await page.evaluate(({ key, beadId }) => {
      const m = { [beadId]: '2099-12-31T23:59:59.999999999Z' };
      window.localStorage.setItem(key, JSON.stringify(m));
    }, { key: 'bv:lastSeen:v1', beadId: id });

    await page.goto(LISTING_PATH);
    await waitForHtmxIdle(page);

    const row = page.locator(`li.bead-row[data-bead-id="${id}"]`);
    await expect(row).toBeVisible();
    await expect(row).not.toHaveClass(/bead-row-unread/);
    await expect(row.locator(':scope > .bead-row-unread-dot')).toHaveCount(0);
  });
});
