// capture-page.mjs — capture a single beads-viewer page to a PNG, with
// an optional Authorization bearer header. Used by
// gardenio/tests/e2e/scripts/fetch-beads-viewer-prod.sh for in-pod
// self-service screenshots of the PROD viewer (task ka-0000002064).
//
// The built-in `playwright screenshot` CLI cannot attach a custom
// Authorization header, which the OIDC-gated viewer requires — so this
// thin wrapper drives chromium directly with extraHTTPHeaders.
//
// Usage:
//   node tools/capture-page.mjs <url> <out.png>
//
// Env:
//   BEADS_VIEWER_BEARER   bearer token (sent as Authorization header)
//   BEADS_VIEWER_SHOT_FULLPAGE  if set, capture the full scrollable page
//   BEADS_VIEWER_SHOT_WIDTH/HEIGHT  viewport (default 1280x800)
import { chromium } from '@playwright/test';

const url = process.argv[2];
const out = process.argv[3];
if (!url || !out) {
  console.error('usage: node tools/capture-page.mjs <url> <out.png>');
  process.exit(2);
}

const bearer = process.env.BEADS_VIEWER_BEARER || '';
const fullPage = !!process.env.BEADS_VIEWER_SHOT_FULLPAGE;
const width = parseInt(process.env.BEADS_VIEWER_SHOT_WIDTH || '1280', 10);
const height = parseInt(process.env.BEADS_VIEWER_SHOT_HEIGHT || '800', 10);

const browser = await chromium.launch();
try {
  const context = await browser.newContext({
    viewport: { width, height },
    ignoreHTTPSErrors: true,
    extraHTTPHeaders: bearer ? { Authorization: `Bearer ${bearer}` } : undefined,
    reducedMotion: 'reduce',
    colorScheme: 'light',
  });
  const page = await context.newPage();
  const resp = await page.goto(url, { waitUntil: 'networkidle', timeout: 60000 });
  if (resp && resp.status() >= 400) {
    console.error(`capture-page: ${url} returned HTTP ${resp.status()}`);
    process.exit(1);
  }
  await page.screenshot({ path: out, fullPage });
  console.error(`capture-page: wrote ${out}`);
} finally {
  await browser.close();
}
