// Phase T1 smoke test (ka-0000005681): proves the harness boots beads-viewer
// and the index page renders end-to-end.
//
// Coverage expansion lives in later phases (T3 route smoke, T4 nav, etc.).
// Do NOT bulk up this file — keep T1 minimal so the harness stays the focus.

import { test, expect } from '@playwright/test';

test.describe('harness smoke', () => {
  test('healthz returns ok', async ({ request }) => {
    const res = await request.get('/healthz');
    expect(res.status()).toBe(200);
    const body = await res.json();
    expect(body.status).toBe('ok');
  });

  test('index page / renders the layout', async ({ page }) => {
    const res = await page.goto('/');
    expect(res, 'navigation response').not.toBeNull();
    expect(res.status()).toBeLessThan(400);
    // Layout-level landmark — the viewer's base template renders a <nav>
    // and a body with at least one heading or link. Asserting the <nav>
    // exists is the cheapest "the template rendered" signal.
    await expect(page.locator('nav').first()).toBeVisible();
  });
});
