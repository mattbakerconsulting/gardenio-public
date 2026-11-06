// Shared Playwright helpers for the beads-viewer behavioral suite.
//
// htmx-idle pattern: htmx swaps are async — after triggering one, wait for
// the htmx in-flight counter to drop to zero. Sleeps are not allowed.
//
// Usage:
//   import { waitForHtmxIdle } from './_helpers.js';
//   await page.locator('[hx-get]').click();
//   await waitForHtmxIdle(page);

import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = dirname(fileURLToPath(import.meta.url));
const idsPath = resolve(__dirname, '..', 'fixtures', 'ids.json');

let cachedFixtures = null;

// Phase T2 (ka-0000006317): load the fixture id manifest emitted by
// `seed/seed.mjs`. Tests reference fixtures by stable key (e.g.
// `fixtures.schedule`) and the resolved `kd-...` id is identical across
// runs against the same daemon thanks to label-based upsert.
//
// Returns an object `{ schedule: 'kd-...', agent: 'kd-...', ... }`.
// Throws a clear error if the manifest is missing — the most likely
// cause is that globalSetup did not run (e.g. running a single spec via
// `playwright test --no-deps`). Re-run `make seed-beads-viewer-e2e`.
export function loadFixtures() {
  if (cachedFixtures) return cachedFixtures;
  let raw;
  try {
    raw = readFileSync(idsPath, 'utf8');
  } catch (err) {
    if (err.code === 'ENOENT') {
      throw new Error(
        `fixture manifest not found at ${idsPath}. Run \`make seed-beads-viewer-e2e\` ` +
          'or unset BEADS_VIEWER_E2E_SKIP_SEED so globalSetup can seed.',
      );
    }
    throw err;
  }
  cachedFixtures = JSON.parse(raw);
  return cachedFixtures;
}

// Resolve a single fixture id by key, throwing a precise error when the
// key is missing. Prefer this over `loadFixtures()[key]` so a typo in a
// test fails with a helpful message instead of a cryptic "undefined".
export function fixtureId(key) {
  const f = loadFixtures();
  if (!f[key]) {
    throw new Error(
      `fixture ${key} not in manifest (have: ${Object.keys(f).join(', ')}). ` +
        'Add it to seed/fixtures.mjs and re-seed.',
    );
  }
  return f[key];
}

export async function waitForHtmxIdle(page, timeoutMs = 5000) {
  await page.waitForFunction(
    () => {
      const htmx = window.htmx;
      if (!htmx) return true;
      // htmx exposes the in-flight request count on the body element.
      const body = document.body;
      if (!body) return true;
      const inflight = body.getAttribute('hx-in-flight');
      return inflight === null || inflight === '0';
    },
    null,
    { timeout: timeoutMs },
  );
  // One rAF tick so any swap-driven layout settles before assertions.
  await page.evaluate(() => new Promise((r) => requestAnimationFrame(() => r())));
}
