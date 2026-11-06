// Behavioral coverage for the "maps merge request" fixture (gd-YDwA1wpnnH),
// and the CI exercise of the reusable module `_maps_mr.js`.
//
// The `maps_mr` fixture (seed/fixtures.mjs) is a type=merge-request bead that
// hangs off the in-progress maps task (`maps_task_ip`) as a parent-child child,
// part of the gardener map fixture graph (gd-EBQ0PytygF). It carries a single
// `mr_url` field and nothing else — the canonical MR *node* other suites reach
// for. This spec pins the invariants the seed guarantees so a downstream
// regression (type silently reverting, the parent-child edge dropping, the
// type_mr card losing its heading/link, the constellation node losing its
// diamond glyph) fails HERE, at the fixture, rather than as a confusing failure
// inside an unrelated consumer suite.
//
// It also serves as the executable usage example for `_maps_mr.js`: every
// assertion routes through the module's helpers, so if the module's locators
// drift from the real DOM, this gate goes red.
//
// Auth: same as every other spec in this dir — the suite boots the viewer with
// BEADS_VIEWER_DEV_AUTH_BYPASS=1 (playwright.config.js webServer env), so a
// plain page.goto() is an authenticated read.

import { test, expect } from '@playwright/test';

import { fixtureId } from './_helpers.js';
import * as mapsMR from './_maps_mr.js';

test.describe('maps merge request fixture (bead-show)', () => {
  test('resolves from the manifest to a kd- id', () => {
    expect(mapsMR.id(), 'maps_mr id').toMatch(/^(gd|ka)-[A-Za-z0-9]+$/);
    expect(mapsMR.parentTaskId(), 'maps_task_ip id').toMatch(/^(gd|ka)-[A-Za-z0-9]+$/);
  });

  test('renders the type_mr card with heading + mr_url link', async ({ page }) => {
    await mapsMR.gotoBeadShow(page);

    // The one place the type_mr contract is pinned — shared with every
    // consumer of the module. Heading, visible link, correct href, and the
    // safe external-link attributes (target=_blank + rel=noopener).
    await mapsMR.assertMRCard(page);
  });

  test('renders the merge-request type chip on the inline header', async ({ page }) => {
    await mapsMR.gotoBeadShow(page);
    await mapsMR.assertTypeChip(page);
  });

  test('renders no MR-state badge for the url-only fixture', async ({ page }) => {
    await mapsMR.gotoBeadShow(page);
    // The fixture carries only mr_url — no mr_state — so the type_mr card
    // renders no <p class="mr-state"> badge. Pin that deliberate contract.
    await mapsMR.assertNoStateBadge(page);
  });

  test('links to the parent maps task in the deps section', async ({ page }) => {
    await mapsMR.gotoBeadShow(page);
    await mapsMR.assertParentTaskLink(page);

    // And the dep link actually navigates to the parent task bead-show page.
    await mapsMR.parentTaskDepLink(page).click();
    await expect(page).toHaveURL(new RegExp(`/${mapsMR.parentTaskId()}$`));
  });
});

test.describe('maps merge request fixture (gardener connected beads)', () => {
  // Demonstrates the module driving DIFFERENT surfaces than bead-show: the
  // gardener connected-beads views. The MR is reachable from the gardener
  // fixture within a couple of hops (maps_task_ip → gardener_run_2 is a `blocks`
  // edge to a seed; maps_mr → maps_task_ip is parent-child), so a generous
  // degree surfaces it in the walk.
  test('renders the MR as a diamond node in the grouped constellation', async ({ page }) => {
    const gardenerId = fixtureId('gardener');

    // Default (grouped) view embeds the constellation star map. Fetch the
    // partial directly at a high degree so the MR node is inside the walk.
    const res = await page.goto(`/gardener/${gardenerId}/beads?degree=3`);
    expect(res.status(), 'gardener grouped-beads HTTP status').toBeLessThan(400);

    const fig = page.locator('figure.constellation');
    await expect(fig, 'constellation figure rendered').toBeVisible();

    // type=merge-request nodes render as a diamond (constellation-node-mr +
    // DiamondPath) rather than a circle — the "has-MR facet + diamond glyph"
    // the seed calls out (ka-0000004604). Assert this MR's node carries the
    // MR class and draws a <path> diamond (not a <circle>).
    const diamond = mapsMR.constellationDiamond(fig);
    await expect(diamond, 'maps MR renders as a constellation diamond').toBeVisible();
    await expect(diamond.locator('path'), 'diamond node draws an SVG path').toHaveCount(1);
    await expect(diamond.locator('circle'), 'diamond node draws no disk').toHaveCount(0);
  });

  test('surfaces the MR as a merge-request row in the dense view', async ({ page }) => {
    const gardenerId = fixtureId('gardener');

    // The dense one-line-per-bead lanes bucket by status; the MR is open, so
    // it lands in the `open` lane. Fetch the dense partial directly at a high
    // degree so the 2-hop MR is inside the walk (server.go GET
    // /gardener/{id}/beads).
    const res = await page.goto(`/gardener/${gardenerId}/beads?degree=3&view=dense`);
    expect(res.status(), 'gardener dense-beads HTTP status').toBeLessThan(400);

    const root = page.locator('.cap-beads-inner');
    await expect(root).toBeVisible();

    // The MR's one-line row is present and tagged with its `merge-request` type.
    const row = mapsMR.denseNodeRow(root);
    await expect(row, 'maps MR appears as a dense-lane row').toBeVisible();
    await expect(
      mapsMR.denseNodeType(root),
      'dense-lane row is tagged merge-request',
    ).toHaveText('merge-request');
  });
});
