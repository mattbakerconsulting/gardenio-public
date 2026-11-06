// ka-0000004426 — /formulas desktop stats-banner density outcome test.
//
// The shared .stats-banner is a block-level flex container, so on /formulas it
// stretched to the full content width. At 1280px its three KPIs (FORMULAS /
// WITH LIVE MOLECULES / LIVE MOLECULES) huddled in the left ~40% and left a
// measured ~746px (61% of the banner's inner width) 100%-white band to their
// right — the single lowest-density chrome element above the fold on the
// catalog page (band near_white ~0.94).
//
// This is the DESKTOP horizontal counterpart of the narrow-banner fix
// ka-0000003920 (MR !5820), which explicitly left the desktop layout alone. The
// fix here (a >=481px width:max-content hug rule, mirroring the .formula-dag
// pane-hug idiom) makes the banner shrink to its content so the bordered/tinted
// card stops extending into the empty right half. This spec asserts the DENSITY
// OUTCOME on real rendered geometry — a rule-only Go test cannot prove the card
// actually stopped occupying the band — consistent with the structure-page
// density idiom (formula_dag_density.spec.js, ka-0000006902).
//
// The narrow (375px) rule (ka-0000003920) is scoped to max-width:480px and this
// fix to min-width:481px, so they never overlap; the second test pins that the
// narrow compact grid is untouched.
//
// ka-0000000836 — follow-up density fix. The hug above shrank the banner CARD but
// left the ~768px STRIP to its right (x[512-1280] at 1280px) as dead page
// background — the one remaining low-density spot above the fold. The follow-up
// wraps the KPI banner + the filter form in one .formula-catalog-header flex row
// (>=768px) so the filters fill that band instead of stacking as a separate
// sparse strip below. The third test asserts that earned-band DENSITY OUTCOME:
// the filter form sits in the banner's right band, top-aligned beside it, and its
// right edge reaches near the content edge — content anchoring BOTH ends of the
// strip rather than huddling left.

import { test, expect } from '@playwright/test';

const DESKTOP = { width: 1280, height: 900 };
const NARROW = { width: 375, height: 800 };

// The task's density bar: no all-white band wider than this to the right of the
// last stat item at desktop. The pre-fix void was ~746px; the hug fix leaves
// only the banner's right padding between the last KPI and the card edge.
const MAX_RIGHT_BAND_PX = 400;

test.describe('/formulas desktop stats-banner density (ka-0000004426)', () => {
  test('desktop: banner hugs its KPIs, no wide empty right-of-stats card band', async ({
    page,
  }) => {
    await page.setViewportSize(DESKTOP);
    const res = await page.goto('/formulas');
    expect(res.status(), '/formulas HTTP status').toBeLessThan(400);

    const banner = page.locator('.stats-banner-formulas');
    await expect(banner).toBeVisible();

    const items = banner.locator('.stats-banner-item');
    const itemCount = await items.count();
    expect(itemCount, 'stats banner renders its KPI items').toBeGreaterThan(0);

    const bBox = await banner.boundingBox();
    const lastItemBox = await items.nth(itemCount - 1).boundingBox();
    expect(bBox, 'banner bbox').not.toBeNull();
    expect(lastItemBox, 'last stat item bbox').not.toBeNull();

    // Primary outcome: the banner's right edge must sit close to the last stat
    // item's right edge — i.e. the card hugs its figures instead of stretching
    // full-width with an empty right half. Pre-fix this gap was ~746px.
    const rightBand = bBox.x + bBox.width - (lastItemBox.x + lastItemBox.width);
    expect(
      rightBand,
      `empty card band right of the last stat item (${Math.round(rightBand)}px) must be < ${MAX_RIGHT_BAND_PX}px — a full-width banner with a huddled-left KPI cluster (ka-0000004426)`,
    ).toBeLessThan(MAX_RIGHT_BAND_PX);

    // Corroborate: the banner must NOT span most of the content width. The h1
    // above it fills the content column, so a hugging banner is materially
    // narrower than the heading; a full-width card would match it.
    const heading = page.locator('h1').first();
    const hBox = await heading.boundingBox();
    expect(hBox, 'heading bbox').not.toBeNull();
    expect(
      bBox.width,
      `hugging banner width (${Math.round(bBox.width)}px) must be well under the content width (${Math.round(hBox.width)}px) — a full-width card matches it (ka-0000004426)`,
    ).toBeLessThan(hBox.width - MAX_RIGHT_BAND_PX);

    // Belt-and-suspenders: scan the widest CONTIGUOUS run of banner-card pixels
    // to the RIGHT of the last stat item, across scanlines within the banner's
    // vertical extent, via elementFromPoint. Post-fix the card no longer extends
    // into that band, so elementFromPoint returns the page background (body),
    // not the banner element — the empty-card run collapses to ~0. Pre-fix the
    // banner element itself filled the whole ~746px band.
    const widestCardRun = await page.evaluate((maxBand) => {
      const b = document.querySelector('.stats-banner-formulas');
      if (!b) return null;
      const items = b.querySelectorAll('.stats-banner-item');
      if (!items.length) return null;
      const br = b.getBoundingClientRect();
      const lr = items[items.length - 1].getBoundingClientRect();
      const startX = Math.ceil(lr.right);
      // Sample scanlines across the banner's vertical extent.
      const ys = [];
      const n = 7;
      for (let i = 1; i <= n; i++) ys.push(Math.round(br.top + ((br.bottom - br.top) * i) / (n + 1)));
      let widest = 0;
      for (const y of ys) {
        let run = 0;
        for (let x = startX + 2; x < window.innerWidth - 2; x += 6) {
          const el = document.elementFromPoint(x, y);
          // Count only pixels still occupied by the banner card (or its
          // children) — that is the empty right half of the card we removed.
          const isCard = el && el.closest('.stats-banner-formulas');
          if (!isCard) break;
          run = x - startX;
        }
        if (run > widest) widest = run;
      }
      return { widest, bannerWidth: br.width };
    }, MAX_RIGHT_BAND_PX);

    expect(widestCardRun, 'banner present for card-band scan').not.toBeNull();
    expect(
      widestCardRun.widest,
      `widest empty card run right of the last stat item (${Math.round(widestCardRun.widest)}px) must be < ${MAX_RIGHT_BAND_PX}px — the full-width card's empty right half (ka-0000004426)`,
    ).toBeLessThan(MAX_RIGHT_BAND_PX);
  });

  test('narrow: compact 3-up grid (ka-0000003920) is untouched, no page overflow', async ({
    page,
  }) => {
    await page.setViewportSize(NARROW);
    const res = await page.goto('/formulas');
    expect(res.status(), '/formulas HTTP status').toBeLessThan(400);

    const banner = page.locator('.stats-banner-formulas');
    await expect(banner).toBeVisible();

    // The narrow rule (max-width:480px) collapses the banner to a compact CSS
    // grid. The desktop hug (min-width:481px) must not leak down here.
    const display = await banner.evaluate((el) => getComputedStyle(el).display);
    expect(display, 'narrow banner stays a compact grid (ka-0000003920)').toBe('grid');

    // No horizontal page overflow (1px tolerance for sub-pixel rounding).
    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - window.innerWidth,
    );
    expect(overflow, 'no horizontal page overflow at 375px').toBeLessThanOrEqual(1);
  });

  test('desktop: filter form fills the band right of the hugged banner (ka-0000000836)', async ({
    page,
  }) => {
    await page.setViewportSize(DESKTOP);
    const res = await page.goto('/formulas');
    expect(res.status(), '/formulas HTTP status').toBeLessThan(400);

    const header = page.locator('.formula-catalog-header');
    await expect(header).toBeVisible();
    const banner = header.locator('.stats-banner-formulas');
    const filters = header.locator('.formula-catalog-filters');
    await expect(banner).toBeVisible();
    await expect(filters).toBeVisible();

    const hBox = await header.boundingBox();
    const bBox = await banner.boundingBox();
    const fBox = await filters.boundingBox();
    expect(hBox, 'header bbox').not.toBeNull();
    expect(bBox, 'banner bbox').not.toBeNull();
    expect(fBox, 'filter form bbox').not.toBeNull();

    // Primary outcome: the filter form sits in the RIGHT band beside the hugged
    // banner, not stacked below it. Its top must be roughly level with the
    // banner (same flex row), and its left edge must start right of the banner —
    // i.e. it occupies the ~768px band that used to be dead page background.
    expect(
      Math.abs(fBox.y - bBox.y),
      `filter form must sit BESIDE the banner in one row (top delta ${Math.round(
        Math.abs(fBox.y - bBox.y),
      )}px), not stacked below it`,
    ).toBeLessThan(bBox.height);
    expect(
      fBox.x,
      `filter form must start right of the hugged banner (filter x=${Math.round(
        fBox.x,
      )}, banner right=${Math.round(bBox.x + bBox.width)}) — filling the dead right band (ka-0000000836)`,
    ).toBeGreaterThan(bBox.x + bBox.width - 4);

    // The filter form must reach toward the right content edge — content
    // anchoring the far end of the strip, so the strip is no longer near-white
    // whitespace. margin-left:auto pushes it right; its right edge should sit
    // close to the header's right edge.
    const rightGap = hBox.x + hBox.width - (fBox.x + fBox.width);
    expect(
      rightGap,
      `filter form must reach the header's right edge (gap ${Math.round(
        rightGap,
      )}px) — the earned right band, not a huddled-left header (ka-0000000836)`,
    ).toBeLessThan(MAX_RIGHT_BAND_PX);

    // No horizontal page overflow introduced by the single-row header.
    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - window.innerWidth,
    );
    expect(overflow, 'no horizontal page overflow at 1280px').toBeLessThanOrEqual(1);
  });
});
