// Behavioral coverage for the Maps page — the gardeners-as-connected-cards board
// (epic ka-0000001644; handler gardenio/controller/cmd/beads-viewer/maps.go,
// view builder maps_view.go, template templates/maps.html).
//
// HISTORY (gd-LqtDbCyIk1): /maps was ORIGINALLY a gardener picker + a per-gardener
// radial constellation at /maps/{gardenerID} with Phase 3 filter chips and Phase 4
// insight overlays. That whole surface was torn down in ka-0000000247 and /maps
// was promoted to the canonical gardeners BOARD in ka-0000002415. The per-gardener
// constellation now lives on the gardener life-thread page (/gardener/{id}), and
// there is no /maps/{id} route any more. This spec was rewritten from the stale
// picker/constellation assertions (which timed out on every MR pipeline,
// reddening the fleet-wide gate) to the board the handler actually serves.
//
// "Maps" is the operator's "every gardener at a glance" view. Each gardener is one
// connected card that reads left→right: a TRIGGER node (what fires it, linking to
// /triggers) → an IDENTITY + verdict body (what it is, how healthy, linking to
// the gardener's life-thread /gardener/{id}) → a RECENT-RUN strip (how the last few
// runs went). A fleet-health summary leads the page and a scope toggle switches
// between the LIVE fleet (open + enabled + type=trigger gardeners) and ALL
// gardeners (?all=1, including disabled/closed/legacy).
//
// Auth: same as every other spec in this dir — the suite boots the viewer with
// BEADS_VIEWER_DEV_AUTH_BYPASS=1 (playwright.config.js webServer env), so a
// plain page.goto() is an authenticated read. The board is fully server-
// rendered, so every card / link / scope chip is a real <a> we assert on.
//
// Fixtures (seed/fixtures.mjs): the `gardener` fixture is an enabled type=trigger
// gardener (trigger:schedule, role:gardener) with three closed gardener_run_*
// iterations forming its life-thread — so it lands in the LIVE fleet and renders
// a card with a recent-run strip and a /gardener/{id} drill-in.

import { test, expect } from '@playwright/test';

import { fixtureId } from './_helpers.js';

test.describe('/maps board', () => {
  test('renders the fleet summary, scope toggle, and a gardener card', async ({ page }) => {
    const res = await page.goto('/maps');
    expect(res.status(), 'maps HTTP status').toBeLessThan(400);

    // Page identity + the fleet-health summary strip lead the board.
    await expect(page.locator('.maps-page h1')).toHaveText('Maps');
    await expect(page.locator('.maps-summary')).toBeVisible();
    await expect(page.locator('.maps-summary-tile.tile-total .maps-summary-num')).toBeVisible();

    // The scope toggle offers Live fleet (active by default) and All gardeners.
    // Scope the active-chip lookup to `.maps-scope-links` — the sibling
    // group-by-owner and runs toggles inside `.maps-scope` ALSO carry
    // `.is-active`, so a bare `.maps-scope a.is-active` matches more than one
    // anchor and `toHaveText` (strict, single-element) fails (ka-0000002434).
    const scope = page.locator('.maps-scope-links');
    await expect(scope.locator('a.is-active')).toHaveText('Live fleet');
    await expect(scope.locator(`a[href$="/maps?all=1"]`)).toBeVisible();

    // The enabled type=trigger gardener fixture lands in the live fleet as a card.
    await expect(page.locator('.maps-board .maps-card').first()).toBeVisible();
  });

  test('the gardener card drills into its life-thread (/gardener/{id})', async ({ page }) => {
    await page.goto('/maps');

    // The board replaced the old /maps/{id} radial map: a gardener's identity
    // body now links to its life-thread page at /gardener/{id} (server.go route
    // GET /gardener/{id}). At least one such drill-in must be present and visible.
    const id = fixtureId('gardener');
    const drillIn = page.locator(`.maps-card .maps-name a[href$="/gardener/${id}"]`).first();
    await expect(drillIn, 'card links to the gardener life-thread').toBeVisible();

    // And it actually navigates to the gardener page.
    await drillIn.click();
    await expect(page).toHaveURL(new RegExp(`/gardener/${id}$`));
  });

  test("the card's trigger node links to /triggers", async ({ page }) => {
    await page.goto('/maps');
    // Each card's left lobe is the trigger node, which links to the Triggers
    // page (the firing primitive behind the gardener).
    const trigger = page.locator('.maps-card a.maps-trigger[href$="/triggers"]').first();
    await expect(trigger).toBeVisible();
  });
});

test.describe('/maps scope toggle', () => {
  test('?all=1 widens to every gardener and marks the All scope active', async ({ page }) => {
    const res = await page.goto('/maps?all=1');
    expect(res.status(), 'maps?all=1 HTTP status').toBeLessThan(400);

    // The All scope is now the active chip; the live gardener still appears.
    // `.maps-scope-links` fences the scope toggle off from the sibling
    // group/runs toggles that also use `.is-active` (ka-0000002434).
    await expect(page.locator('.maps-scope-links a.is-active')).toHaveText('All gardeners');
    const id = fixtureId('gardener');
    await expect(
      page.locator(`.maps-card .maps-name a[href$="/gardener/${id}"]`).first(),
    ).toBeVisible();

    // Clicking back to "Live fleet" round-trips to the default scope.
    await page.locator('.maps-scope-links a', { hasText: 'Live fleet' }).click();
    await expect(page).toHaveURL(/\/maps$/);
    await expect(page.locator('.maps-scope-links a.is-active')).toHaveText('Live fleet');
  });
});
