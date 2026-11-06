// T6 (ka-0000007526) of epic ka-0000004611 — bundle viewer TOC + deep-link
// navigation behavior.
//
// What this asserts:
//   1. /bundle/<id> renders a left-rail TOC listing each doc in
//      fields.position order.
//   2. Clicking a TOC link applies the `bundle-doc-target` highlight
//      class to the target <article> (mirrors the typed-mention flash
//      idiom from highlight.js).
//   3. Initial-load fragment nav (/bundle/<id>#doc-<doc-id>) lands with
//      the highlight class applied — covers permalink resolution.
//
// Fixture: seed/fixtures.mjs `bundle_toc` (10-doc bundle), each doc
// keyed `bundle_doc_01..10` with `position` 1..10.

import { expect, test } from '@playwright/test';

import { fixtureId, loadFixtures } from './_helpers.js';

test.describe('GET /bundle/<id> — T6 TOC + deep-link', () => {
  test('renders left-rail TOC in position order with anchor + view-raw', async ({ page }) => {
    const bundleID = fixtureId('bundle_toc');
    const res = await page.goto(`/bundle/${bundleID}`);
    expect(res, 'navigation response').not.toBeNull();
    expect(res.status()).toBeLessThan(400);

    await expect(page.locator('.bundle-layout')).toBeVisible();
    await expect(page.locator('.bundle-toc-rail')).toBeVisible();

    const links = page.locator('.bundle-toc .bundle-toc-link');
    await expect(links).toHaveCount(10);

    // First TOC entry should be position 1 (Bundle doc 1).
    await expect(links.first()).toContainText('Bundle doc 1');
    // Last TOC entry should be position 10.
    await expect(links.last()).toContainText('Bundle doc 10');

    // Each card has a "view raw" link to the underlying doc bead.
    await expect(page.locator('article.bundle-doc .bundle-doc-raw').first()).toBeVisible();
  });

  test('clicking a TOC link applies the highlight class to the target article', async ({ page }) => {
    const fixtures = loadFixtures();
    const bundleID = fixtures.bundle_toc;
    const targetDocID = fixtures.bundle_doc_05;
    expect(bundleID, 'bundle_toc fixture id').toBeTruthy();
    expect(targetDocID, 'bundle_doc_05 fixture id').toBeTruthy();

    const res = await page.goto(`/bundle/${bundleID}`);
    expect(res.status()).toBeLessThan(400);

    const target = page.locator(`article#doc-${targetDocID}`);
    await expect(target).toHaveCount(1);
    await expect(target).not.toHaveClass(/bundle-doc-target/);

    await page.locator(`.bundle-toc-link[href="#doc-${targetDocID}"]`).click();

    // The hashchange handler in bundle_toc.js paints `bundle-doc-target`
    // for ~3s. Assert it lands within the flash window.
    await expect(target).toHaveClass(/bundle-doc-target/);
  });

  test('initial-load fragment nav applies the highlight class', async ({ page }) => {
    const fixtures = loadFixtures();
    const bundleID = fixtures.bundle_toc;
    const targetDocID = fixtures.bundle_doc_03;
    expect(bundleID).toBeTruthy();
    expect(targetDocID).toBeTruthy();

    const res = await page.goto(`/bundle/${bundleID}#doc-${targetDocID}`);
    expect(res.status()).toBeLessThan(400);

    const target = page.locator(`article#doc-${targetDocID}`);
    await expect(target).toHaveClass(/bundle-doc-target/);
  });
});
