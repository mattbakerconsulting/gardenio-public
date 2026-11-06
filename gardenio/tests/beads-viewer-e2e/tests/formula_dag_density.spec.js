// ka-0000006902 — structure-DAG density outcome test.
//
// A strictly-linear recipe lays its step graph out VERTICALLY (formula_dag.go
// dag.Vertical), rendering a thin ~222px ribbon. Two predecessor fixes shrank
// the pieces around that ribbon but only RELOCATED the whitespace void instead
// of closing it:
//
//   - ka-0000002500 / MR !5855 hugged the .formula-dag-scroll box.
//   - ka-0000005917 / MR !5873 hugged the .formula-dag-pane — but the step-list
//     still stacked BELOW the ribbon, leaving a measured ~555×690px 100%-white
//     band to the ribbon's RIGHT at 1280px (near_white_formula_dag flat 0.873).
//
// Both predecessor tests were RULE-only (they asserted a flex declaration), so
// a relocated void shipped green. This spec asserts the DENSITY OUTCOME on real
// rendered geometry: at 1280px the step-list must sit SIDE-BY-SIDE with the
// graph and there must be no wide all-white band to the graph's right. At 375px
// the two must stack with no horizontal page overflow (no narrow regression).
//
// Uses the `formula_linear` fixture (seed/fixtures.mjs) — a strictly-linear
// 8-step chain, the shape that triggers the vertical ribbon.

import { test, expect } from '@playwright/test';

import { fixtureId } from './_helpers.js';

const DESKTOP = { width: 1280, height: 900 };
const NARROW = { width: 375, height: 800 };

// The task's density bar: no all-white band wider than this to the right of the
// graph at desktop. The pre-fix void was 555px; the fix leaves only the ~16px
// flex gap between the graph column and the step-list.
const MAX_RIGHT_BAND_PX = 400;

test.describe('formula structure DAG density (ka-0000006902)', () => {
  test('desktop: linear step-list sits beside the graph, no wide right-of-graph void', async ({ page }) => {
    await page.setViewportSize(DESKTOP);
    const id = fixtureId('formula_linear');
    const res = await page.goto(`/${id}`);
    expect(res.status(), 'bead-show HTTP status').toBeLessThan(400);

    const pane = page.locator('.formula-dag-pane');
    await expect(pane).toBeVisible();

    // The graph ribbon and the step-list must both be present…
    const graph = page.locator('.formula-dag-scroll');
    const stepList = page.locator('.formula-step-list');
    await expect(graph).toBeVisible();
    await expect(stepList).toBeVisible();

    const gBox = await graph.boundingBox();
    const sBox = await stepList.boundingBox();
    expect(gBox, 'graph bbox').not.toBeNull();
    expect(sBox, 'step-list bbox').not.toBeNull();

    // …and the step-list must sit BESIDE the graph (its left edge starts at or
    // to the right of the graph's right edge, and its top aligns near the
    // graph's top) — NOT stacked below it. A stacked step-list (the relocated
    // void) would have sBox.y >= gBox.y + gBox.height.
    expect(
      sBox.y,
      'step-list must sit beside the graph (top-aligned), not stacked below it',
    ).toBeLessThan(gBox.y + gBox.height - 20);
    expect(
      sBox.x,
      'step-list must start at/after the graph right edge (side-by-side)',
    ).toBeGreaterThanOrEqual(gBox.x + gBox.width - 4);

    // The all-white band to the RIGHT of the graph, within the graph's vertical
    // extent, must be narrow. The step-list fills it, so the widest gap should
    // be roughly the flex gap, well under MAX_RIGHT_BAND_PX.
    const rightBand = sBox.x - (gBox.x + gBox.width);
    expect(
      rightBand,
      `whitespace band right of the graph (${Math.round(rightBand)}px) must be < ${MAX_RIGHT_BAND_PX}px`,
    ).toBeLessThan(MAX_RIGHT_BAND_PX);

    // Belt-and-suspenders: measure the widest CONTIGUOUS run of empty space to
    // the right of the graph, sampled across scanlines, via elementFromPoint —
    // this catches a void even if the flex math looks right but content fails
    // to land in the band.
    //
    // Scope the scan to the OVERLAP of the graph's and the step-list's vertical
    // extents — i.e. the exact band the step-list is meant to fill. A linear
    // ribbon (one node per stage) is intentionally TALLER than its step-list, so
    // the whitespace beside the graph's lower half is inherent to the vertical
    // layout (the gold-standard molecule page, ka-0000006510, has the same tall
    // ribbon and is the densest of the three). The DEFECT this guards is the
    // band ALONGSIDE the step-list — pre-fix the step-list stacked below, so
    // that band was 100% white; post-fix it is only the flex gap.
    const widestRun = await page.evaluate((maxBand) => {
      const g = document.querySelector('.formula-dag-scroll');
      const s = document.querySelector('.formula-step-list');
      if (!g || !s) return null;
      // elementFromPoint only hit-tests the viewport: a point below the fold
      // returns null, which this scan would read as white space. The page chrome
      // above the pane (nav, banners) puts the band's lower scanlines past the
      // bottom of a 900px viewport, so bring the step-list to the top first.
      s.scrollIntoView({ block: 'start' });
      const gr = g.getBoundingClientRect();
      const sr = s.getBoundingClientRect();
      const rightEdge = Math.ceil(gr.right);
      const viewportW = window.innerWidth;
      // Overlap of the graph and step-list vertical extents.
      const top = Math.max(gr.top, sr.top);
      const bot = Math.min(gr.bottom, sr.bottom, window.innerHeight);
      // If the step-list does not overlap the graph vertically at all, it is
      // stacked below (the pre-fix void) — report the full band width as the
      // failure signal.
      if (bot - top < 20) return { widest: viewportW - rightEdge, stacked: true };
      const ys = [];
      const n = 9;
      for (let i = 1; i <= n; i++) ys.push(Math.round(top + ((bot - top) * i) / (n + 1)));
      let widest = 0;
      for (const y of ys) {
        let run = 0;
        for (let x = rightEdge + 2; x < viewportW - 2; x += 6) {
          const el = document.elementFromPoint(x, y);
          const isContent =
            el &&
            el !== document.body &&
            el !== document.documentElement &&
            el.closest('.formula-step-list, .formula-side-rail, .formula-steps');
          if (isContent) break;
          run = x - rightEdge;
        }
        if (run > widest) widest = run;
      }
      return { widest, stacked: false };
    }, MAX_RIGHT_BAND_PX);

    expect(widestRun, 'graph + step-list present for band scan').not.toBeNull();
    expect(
      widestRun.stacked,
      'step-list must overlap the graph vertically (side-by-side), not stack below it',
    ).toBe(false);
    expect(
      widestRun.widest,
      `widest all-white run alongside the step-list (${Math.round(widestRun.widest)}px) must be < ${MAX_RIGHT_BAND_PX}px — a relocated void (ka-0000006902)`,
    ).toBeLessThan(MAX_RIGHT_BAND_PX);
  });

  test('narrow: graph and step-list stack with no horizontal page overflow', async ({ page }) => {
    await page.setViewportSize(NARROW);
    const id = fixtureId('formula_linear');
    const res = await page.goto(`/${id}`);
    expect(res.status(), 'bead-show HTTP status').toBeLessThan(400);

    const graph = page.locator('.formula-dag-scroll');
    const stepList = page.locator('.formula-step-list');
    await expect(graph).toBeVisible();
    await expect(stepList).toBeVisible();

    const gBox = await graph.boundingBox();
    const sBox = await stepList.boundingBox();
    // At narrow width the two must stack — the step-list drops BELOW the graph.
    expect(sBox.y, 'step-list stacks below the graph at 375px').toBeGreaterThan(gBox.y);

    // No horizontal page overflow (1px tolerance for sub-pixel rounding).
    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - window.innerWidth,
    );
    expect(overflow, 'no horizontal page overflow at 375px').toBeLessThanOrEqual(1);
  });

  // ka-0000001826 — a LONG linear chain (> the 8-node wrap threshold) must lay
  // out as a WRAPPED multi-column block, not a tall single-column ribbon that
  // strands a bottom-right void. This is the geometric root-cause fix: five
  // prior CSS fixes only relocated the whitespace around the ribbon; wrapping
  // the graph itself makes it short + wide so it fills the recipe page's right
  // band. Uses the `formula_linear_long` fixture (11 steps).
  test('desktop: a long linear chain wraps into a short, wide multi-column block', async ({ page }) => {
    await page.setViewportSize(DESKTOP);
    const id = fixtureId('formula_linear_long');
    const res = await page.goto(`/${id}`);
    expect(res.status(), 'bead-show HTTP status').toBeLessThan(400);

    const svg = page.locator('.formula-dag-svg');
    await expect(svg).toBeVisible();

    // The wrapped graph must be WIDER than a single node column (~204px) and
    // MUCH shorter than the single-column 11-node ribbon (~874px). We read the
    // SVG's intrinsic viewBox so the assertion is DPR- and CSS-independent.
    const dims = await page.evaluate(() => {
      const el = document.querySelector('.formula-dag-svg');
      const vb = (el.getAttribute('viewBox') || '').split(/\s+/).map(Number);
      return { w: vb[2], h: vb[3] };
    });
    expect(dims.w, 'wrapped graph width should span more than one node column').toBeGreaterThan(300);
    expect(dims.h, 'wrapped graph should be far shorter than the 11-node ribbon').toBeLessThan(600);
    // A short + wide block: the aspect ratio flips from tall (h≫w) to landscape.
    expect(dims.w, 'wrapped graph should read as landscape, not a tall ribbon').toBeGreaterThan(dims.h * 0.7);

    // Density outcome: the wrapped graph is a short, wide block whose bottom
    // sits NEAR the step-list's — not the tall ribbon that hung hundreds of px
    // below it (the void this fix removed). The graph and the step-list are
    // independently-sized flex columns — a fixed-geometry ~474px SVG box vs. an
    // 11-row text list — so their bottoms never align to the pixel: the measured
    // overhang is ~55–70px across engines (fixed ~492px graph box, ~460px
    // step-list, graph starting ~25px lower under its "Structure" heading).
    //
    // A generous tolerance — well above the wrapped block's ~70px, far below a
    // single-column ribbon's ~450px (an 11-node ribbon renders ~880px tall) —
    // catches the tall-ribbon regression without demanding false pixel-parity.
    // The original +8px was ~60px too tight for the real rendered layout and
    // self-merged red (gd-5VS8s9Sh3Q). The short+wide shape is already asserted
    // above on the intrinsic SVG viewBox; this is the rendered-DOM backstop.
    const MAX_GRAPH_OVERHANG_PX = 160;
    const gBox = await page.locator('.formula-dag-scroll').boundingBox();
    const sBox = await page.locator('.formula-step-list').boundingBox();
    const overhang = gBox.y + gBox.height - (sBox.y + sBox.height);
    expect(
      overhang,
      `wrapped graph bottom hangs ${Math.round(overhang)}px below the step-list — ` +
        `must be < ${MAX_GRAPH_OVERHANG_PX}px (a tall-ribbon void hangs ~450px)`,
    ).toBeLessThan(MAX_GRAPH_OVERHANG_PX);
  });

  test('narrow: a long wrapped chain has no horizontal page overflow', async ({ page }) => {
    await page.setViewportSize(NARROW);
    const id = fixtureId('formula_linear_long');
    const res = await page.goto(`/${id}`);
    expect(res.status(), 'bead-show HTTP status').toBeLessThan(400);

    await expect(page.locator('.formula-dag-scroll')).toBeVisible();
    // The wrapped graph is wide enough to hit max-width:100% at 375px; the
    // box-sizing:border-box fix keeps its padding+border inside that clamp so
    // the page does not overflow horizontally (ka-0000001826).
    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - window.innerWidth,
    );
    expect(overflow, 'no horizontal page overflow at 375px for a wrapped chain').toBeLessThanOrEqual(1);
  });
});
