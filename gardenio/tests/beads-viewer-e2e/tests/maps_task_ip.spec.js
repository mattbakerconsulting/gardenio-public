// Behavioral coverage for the "maps in-progress task" fixture (gd-k5YNKNMI7J),
// and the CI exercise of the reusable module `_maps_task_ip.js`.
//
// The `maps_task_ip` fixture (seed/fixtures.mjs) is an in_progress P0 task
// parented under the maps epic, part of the gardener map fixture graph
// (gd-EBQ0PytygF). It is the canonical LIVE, high-priority task node that other
// suites reach for. This spec pins the invariants the seed guarantees so a
// downstream regression (status silently reverting to open, the parent-child
// edge dropping, the in_progress status chip losing its CSS hook) fails HERE,
// at the fixture, rather than as a confusing failure inside an unrelated
// consumer suite.
//
// It also serves as the executable usage example for `_maps_task_ip.js`: every
// assertion routes through the module's helpers, so if the module's locators
// drift from the real DOM, this gate goes red.
//
// Auth: same as every other spec in this dir — the suite boots the viewer with
// BEADS_VIEWER_DEV_AUTH_BYPASS=1 (playwright.config.js webServer env), so a
// plain page.goto() is an authenticated read.

import { test, expect } from '@playwright/test';

import { fixtureId } from './_helpers.js';
import * as mapsTaskIP from './_maps_task_ip.js';

test.describe('maps in-progress task fixture (bead-show)', () => {
  test('resolves from the manifest to a kd- id', () => {
    expect(mapsTaskIP.id(), 'maps_task_ip id').toMatch(/^(gd|ka)-[A-Za-z0-9]+$/);
    expect(mapsTaskIP.parentEpicId(), 'maps_epic id').toMatch(/^(gd|ka)-[A-Za-z0-9]+$/);
  });

  test('renders the in_progress status chip on the bead-show page', async ({ page }) => {
    await mapsTaskIP.gotoBeadShow(page);

    // The one place the in_progress contract is pinned — shared with every
    // consumer of the module.
    await mapsTaskIP.assertInProgressBadge(page);

    // The sidebar Status field agrees with the inline-header chip.
    await expect(mapsTaskIP.statusFieldValue(page)).toContainText(mapsTaskIP.EXPECTED.status);
  });

  test('renders no inline-header priority chip for the P0 fixture', async ({ page }) => {
    await mapsTaskIP.gotoBeadShow(page);
    // Go template `{{if .Bead.Priority}}` treats the int 0 as falsy, so P0
    // renders no chip. Pin that deliberate contract.
    await mapsTaskIP.assertPriorityChipAbsent(page);
  });

  test('links to the parent maps epic in the deps section', async ({ page }) => {
    await mapsTaskIP.gotoBeadShow(page);
    await mapsTaskIP.assertParentEpicLink(page);

    // And the dep link actually navigates to the parent epic bead-show page.
    await mapsTaskIP.parentEpicDepLink(page).click();
    await expect(page).toHaveURL(new RegExp(`/${mapsTaskIP.parentEpicId()}$`));
  });
});

test.describe('maps in-progress task fixture (gardener dense lane)', () => {
  // Demonstrates the module driving a DIFFERENT surface than bead-show: the
  // gardener connected-beads dense view, where the in-progress task lands in the
  // `in_progress` status lane. The task is a 2-hop-reachable node from the
  // gardener fixture (maps_epic → gardener_run_1; maps_task_ip → maps_epic), so a
  // generous degree surfaces it.
  test('surfaces the task in the gardener in_progress lane', async ({ page }) => {
    const gardenerId = fixtureId('gardener');

    // Fetch the dense connected-beads partial directly at a high degree so the
    // 2-hop task is inside the walk. This is the same endpoint the gardener page
    // lazy-loads into #cap-beads-host (server.go GET /gardener/{id}/beads).
    const res = await page.goto(`/gardener/${gardenerId}/beads?degree=3&view=dense`);
    expect(res.status(), 'gardener dense-beads HTTP status').toBeLessThan(400);

    const root = page.locator('.cap-beads-inner');
    await expect(root).toBeVisible();

    // The in_progress lane is present and contains this task's one-line row.
    const lane = mapsTaskIP.denseInProgressLane(root);
    await expect(lane, 'in_progress lane rendered').toBeVisible();
    await expect(
      mapsTaskIP.denseNodeRow(lane),
      'maps in-progress task appears in the in_progress lane',
    ).toBeVisible();
  });
});
