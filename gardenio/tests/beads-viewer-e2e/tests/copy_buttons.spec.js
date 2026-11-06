// ka-0000003083 — copy-to-clipboard buttons on bead_show.
//
// Asserts the browser-level behavior of the two buttons rendered next to
// the bead ID on the bead-show page:
//
//   1. "Copy ID" copies just the bead ID (e.g. `kd-...`).
//   2. "Copy permalink" copies the absolute URL of the current bead-show
//      page (`origin + pathname`, no query/hash).
//   3. On success, the button briefly flips to "Copied!" with the
//      `.copy-feedback` class for ~1s.
//
// We stub `navigator.clipboard.writeText` so the test does not depend on
// the host's clipboard permissions / focus state — the JS path under test
// is the same either way.

import { test, expect } from '@playwright/test';

import { fixtureId } from './_helpers.js';

const FIXTURE_KEY = 'generic';

test.describe('bead-show copy buttons', () => {
  test('Copy ID + Copy permalink populate the clipboard and flash feedback', async ({ page }) => {
    const id = fixtureId(FIXTURE_KEY);

    // Install the clipboard stub before any navigation so the script runs
    // for every page load. The stub captures the most-recent value so the
    // test can assert it after each click.
    await page.addInitScript(() => {
      window.__copied = [];
      Object.defineProperty(navigator, 'clipboard', {
        configurable: true,
        value: {
          writeText: (s) => {
            window.__copied.push(s);
            return Promise.resolve();
          },
        },
      });
    });

    await page.goto(`/${id}`);

    const idBtn = page.locator('.copy-btn[data-copy="id"]');
    const linkBtn = page.locator('.copy-btn[data-copy="permalink"]');
    await expect(idBtn).toBeVisible();
    await expect(linkBtn).toBeVisible();
    await expect(idBtn).toHaveText('Copy ID');
    await expect(linkBtn).toHaveText('Copy permalink');

    // Click "Copy ID" — clipboard should receive exactly the bead id and
    // the button should briefly become "Copied!" with the feedback class.
    await idBtn.click();
    await expect(idBtn).toHaveText('Copied!');
    await expect(idBtn).toHaveClass(/copy-feedback/);
    expect(await page.evaluate(() => window.__copied)).toEqual([id]);

    // Wait for the ~1s reset before exercising the second button so the
    // first button's reset timer doesn't race with the second click.
    await expect(idBtn).toHaveText('Copy ID');
    await expect(idBtn).not.toHaveClass(/copy-feedback/);

    // Click "Copy permalink" — clipboard should receive origin+pathname.
    const expectedPermalink = await page.evaluate(
      () => window.location.origin + window.location.pathname,
    );
    await linkBtn.click();
    await expect(linkBtn).toHaveText('Copied!');
    await expect(linkBtn).toHaveClass(/copy-feedback/);
    const copied = await page.evaluate(() => window.__copied);
    expect(copied[copied.length - 1]).toBe(expectedPermalink);
    expect(expectedPermalink.endsWith(`/${id}`)).toBe(true);
  });
});
