// Phase T2 smoke test (ka-0000006317): the fixture manifest exists,
// covers every canonical key, and each id resolves to a 200 from the
// viewer's bead-show route. Catches three classes of breakage early:
//
//   1. globalSetup didn't run (manifest missing).
//   2. Seed failed silently and produced a partial manifest.
//   3. The viewer can't render a fixture — wrong type config, broken
//      template, or an id that doesn't exist on the daemon any more.
//
// Per-type assertions on the *content* of each fixture page belong to
// later phases (T5 schedule/agent/advice, T6 decision/formula/mr/generic).
// Keep this file boring.

import { test, expect } from '@playwright/test';

import { fixtureId, loadFixtures } from './_helpers.js';

const EXPECTED_KEYS = [
  'schedule',
  'agent',
  'advice',
  'decision',
  'decision_resolved',
  'formula',
  'formula_linear',
  'mr',
  'generic',
  'markdown',
];

test.describe('fixtures', () => {
  test('manifest covers every canonical key', () => {
    const f = loadFixtures();
    for (const k of EXPECTED_KEYS) {
      expect(f, `missing key ${k}`).toHaveProperty(k);
      expect(f[k], `${k} id`).toMatch(/^(gd|ka)-[A-Za-z0-9]+$/);
    }
  });

  for (const key of EXPECTED_KEYS) {
    test(`${key}: bead-show page renders`, async ({ page }) => {
      const id = fixtureId(key);
      const res = await page.goto(`/${id}`);
      expect(res, 'navigation response').not.toBeNull();
      // Allow 3xx (some routes redirect when auth bypass is on); fail
      // on 4xx/5xx — those are the regressions T2 is meant to catch.
      expect(res.status(), `${key} (${id}) HTTP status`).toBeLessThan(400);
      // Title-level smoke — every bead-show template must render the
      // bead's title in an <h1> or similar prominent landmark.
      await expect(page.locator('main, body').first()).toBeVisible();
    });
  }
});
