// ka-0000003457 — /formulas catalog row density outcome test.
//
// Before this fix the catalog wasted the middle-right ~40% of a 1280px desktop:
// every recipe's content (title, badges, N steps, M vars) packed into the left
// ~640px while `View →` floated at the far edge, leaving x[640-1152] measured
// 96-99% near-white. Two structural defects caused it:
//
//   1. `.formula-row { flex-direction: column }` stacked a SEPARATE live/pip
//      line UNDER each recipe's meta line — doubling every row's vertical
//      height and sprawling the 105-row catalog to ~7383px.
//   2. A single-column `.bead-list` left the right half of each row empty; only
//      the far-right `View →` anchored an otherwise dead band.
//
// The fix collapses each recipe to ONE dense line (live status + actions pulled
// right via .formula-live's margin-left:auto) and lays the catalog out as a
// TWO-COLUMN grid at >=1024px (.formula-catalog-list) so the empty right band
// carries a second column of recipes. This spec asserts the DENSITY OUTCOME on
// real rendered geometry — a rule-only Go test cannot prove the layout actually
// fills the band — consistent with the sibling density specs
// (formula_dag_density.spec.js ka-0000006902, formulas_banner_density.spec.js
// ka-0000004426).
//
// The hermetic seed carries two formula fixtures (seed/fixtures.mjs: `formula`
// and `formula_linear`), enough to populate both grid columns at desktop.

import { test, expect } from '@playwright/test';

const DESKTOP = { width: 1280, height: 900 };
const NARROW = { width: 375, height: 800 };

test.describe('/formulas catalog row density (ka-0000003457)', () => {
  test('desktop: catalog is a two-column grid of single-line recipes, no dead right band', async ({
    page,
  }) => {
    await page.setViewportSize(DESKTOP);
    const res = await page.goto('/formulas');
    expect(res.status(), '/formulas HTTP status').toBeLessThan(400);

    const list = page.locator('.formula-catalog-list');
    await expect(list).toBeVisible();

    // The catalog must be a two-column grid at desktop, so recipes fill the
    // right half of the viewport instead of huddling in the left ~640px.
    const gridCols = await list.evaluate(
      (el) => getComputedStyle(el).gridTemplateColumns,
    );
    const trackCount = gridCols.split(/\s+/).filter(Boolean).length;
    expect(
      trackCount,
      `catalog grid must have 2 columns at 1280px (got "${gridCols}") — a single-column list wastes the right ~40% (ka-0000003457)`,
    ).toBe(2);

    const rows = list.locator('.formula-row');
    const rowCount = await rows.count();
    expect(rowCount, 'catalog renders recipe rows').toBeGreaterThanOrEqual(2);

    // With a two-column grid the second recipe must sit in the RIGHT column,
    // top-aligned with the first — proving the right band carries content, not
    // whitespace. (Pre-fix all rows stacked in one left-hand column.)
    const r0 = await rows.nth(0).boundingBox();
    const r1 = await rows.nth(1).boundingBox();
    expect(r0, 'row 0 bbox').not.toBeNull();
    expect(r1, 'row 1 bbox').not.toBeNull();
    expect(
      r1.x,
      `second recipe must start in the right column (x=${Math.round(r1.x)}) — well right of the first (x=${Math.round(r0.x)})`,
    ).toBeGreaterThan(r0.x + r0.width - 4);
    expect(
      Math.abs(r1.y - r0.y),
      'the two columns must be top-aligned (side-by-side rows, not stacked)',
    ).toBeLessThan(20);

    // The live-status cluster must be PULLED RIGHT within its cell
    // (margin-left:auto), not rendered as a full-width block stacked at the
    // cell's left edge the way the old flex-COLUMN layout did. Two robust
    // geometric signatures of the pull-right (both true regardless of whether a
    // long title wraps the cluster onto a second visual line inside the narrow
    // grid cell):
    //   (a) its LEFT edge sits well right of the cell's left edge, and
    //   (b) its RIGHT edge reaches near the cell's right edge.
    // Pre-fix .formula-live was a block at x≈cell-left on its own line below the
    // meta line — failing both.
    const live0 = rows.nth(0).locator('.formula-live');
    const lBox = await live0.boundingBox();
    expect(lBox, 'live-status bbox').not.toBeNull();
    expect(
      lBox.x - r0.x,
      `live-status must be pulled right, not stacked at the cell's left edge (left offset ${Math.round(
        lBox.x - r0.x,
      )}px) — the old two-line layout rendered it flush-left below the meta line (ka-0000003457)`,
    ).toBeGreaterThan(r0.width * 0.25);
    const rightGap = r0.x + r0.width - (lBox.x + lBox.width);
    expect(
      rightGap,
      `live-status must reach the right edge of its column (gap ${Math.round(
        rightGap,
      )}px) — content that fills the width, not a huddled-left row (ka-0000003457)`,
    ).toBeLessThan(200);
  });

  test('narrow: single-column catalog, no horizontal page overflow', async ({
    page,
  }) => {
    await page.setViewportSize(NARROW);
    const res = await page.goto('/formulas');
    expect(res.status(), '/formulas HTTP status').toBeLessThan(400);

    const list = page.locator('.formula-catalog-list');
    await expect(list).toBeVisible();

    // The two-column grid is scoped to >=1024px; at 375px the catalog is a
    // plain single-column list (grid rule does not apply).
    const gridCols = await list.evaluate(
      (el) => getComputedStyle(el).gridTemplateColumns,
    );
    // A non-grid <ul> reports 'none'; if some skin sets a grid it must be a
    // single track at narrow width.
    const trackCount =
      gridCols === 'none' ? 1 : gridCols.split(/\s+/).filter(Boolean).length;
    expect(
      trackCount,
      `narrow catalog must be a single column (got "${gridCols}")`,
    ).toBe(1);

    // No horizontal page overflow (1px tolerance for sub-pixel rounding).
    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - window.innerWidth,
    );
    expect(overflow, 'no horizontal page overflow at 375px').toBeLessThanOrEqual(1);
  });
});
