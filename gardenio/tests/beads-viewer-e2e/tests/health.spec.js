// Phase T4 (ka-0000000085) — /health page behavioral coverage.
//
// /health renders one tile per entry in TILE_FRAGMENTS, populated
// server-side from a single /v1/health/snapshot fetch, with htmx
// polling each tile fragment every 30s. This spec asserts:
//
//   1. The full page returns < 400 with the layout shell + h1 "Health".
//   2. All tile headings are rendered on first paint (no
//      hydration-required content).
//   3. Each per-tile fragment endpoint (/health/tile/<name>) returns
//      < 400 — these are what the htmx pollers hit. A regression in
//      any one of them silently breaks tile refresh in the browser.
//   4. The tile fragments are partials, not full pages — a runaway
//      `{{template "layout"}}` in a partial would re-render <nav>
//      inside the htmx swap target.
//
// Stale-pill / error-banner rendering is asserted by Go unit tests in
// `health_test.go` (which can deterministically craft a stale snapshot);
// the e2e suite only guarantees the wiring renders against the live
// daemon and that the per-tile fragment endpoints are healthy.

import { expect, test } from '@playwright/test';

// Keep aligned with templates/health.html and server.go route
// registrations (handleHealthTile*). Costs / Build / Escalations
// were added after the original 7-tile shell.
const TILE_HEADINGS = [
  /Open beads/i,
  /Agent fleet/i,
  /Bead-event throughput/i,
  /Recent incidents/i,
  /Bridges/i,
  /Decisions waiting/i,
  /Gardener cycles/i,
  /Cost burn/i,
  /Build/i,
  /Escalations waiting/i,
  /Operator hygiene/i,
];

const TILE_FRAGMENTS = [
  '/health/tile/open-beads',
  '/health/tile/fleet',
  '/health/tile/events',
  '/health/tile/incidents',
  '/health/tile/bridges',
  '/health/tile/decisions',
  '/health/tile/gardeners',
  '/health/tile/costs',
  '/health/tile/build',
  '/health/tile/escalations',
  '/health/tile/operator-signals',
];

test.describe('GET /health', () => {
  test('renders layout shell + h1 + all tile headings', async ({ page }) => {
    const res = await page.goto('/health');
    expect(res, 'navigation response for /health').not.toBeNull();
    expect(res.status(), '/health HTTP status').toBeLessThan(400);

    await expect(page.locator('nav').first()).toBeVisible();
    await expect(page.locator('h1').first()).toContainText(/Health/i);

    for (const heading of TILE_HEADINGS) {
      // h2 lives inside each .health-tile section; assert at least one
      // h2 matches the heading pattern.
      await expect(page.locator('h2').filter({ hasText: heading }).first())
        .toBeVisible();
    }

    // The grid container is the layout sentinel — if a refactor drops
    // the .health-grid class the responsive CSS regresses silently.
    await expect(page.locator('section.health-grid')).toHaveCount(1);
    await expect(page.locator('.health-tile')).toHaveCount(TILE_FRAGMENTS.length);
  });

  test('each tile fragment endpoint returns a partial', async ({ request }) => {
    for (const url of TILE_FRAGMENTS) {
      const res = await request.get(url);
      expect(res.status(), `${url} HTTP status`).toBeLessThan(400);
      const body = await res.text();
      // Fragments must NOT include the layout chrome — a stray
      // `{{template "layout"}}` in a partial would emit <html> /
      // top-level <nav>, which would re-render the whole shell on
      // every htmx swap.
      expect(body, `${url} fragment must not include <html>`).not.toMatch(/<html\b/i);
      expect(body, `${url} fragment must not include top <nav>`).not.toMatch(/<nav\b/i);
      // Every tile fragment renders an h2 heading at the top.
      expect(body, `${url} must contain an h2 heading`).toMatch(/<h2\b/i);
    }
  });
});
