// capture-gardener.mjs — capture a beads-viewer /gardener/{id} page to a PNG
// with an Authorization bearer header.
//
// The generic capture-page.mjs waits for `networkidle`, which never fires
// on the gardener page because it polls via SSE/htmx — the network is never
// idle. This wrapper instead waits for `domcontentloaded` plus the
// `.gardener-iter` life-thread cards (or a short grace period if the page
// has no iterations yet), then screenshots.
//
// Usage:
//   node tools/capture-gardener.mjs <url> <out.png>
//
// Env:
//   BEADS_VIEWER_BEARER          bearer token (Authorization header)
//   BEADS_VIEWER_SHOT_FULLPAGE   if set, capture the full scrollable page
//   BEADS_VIEWER_SHOT_WIDTH/HEIGHT  viewport (default 1440x900)
//   BEADS_VIEWER_DUMP_HTML       if set, write page HTML to this path
import { chromium } from '@playwright/test';

const url = process.argv[2];
const out = process.argv[3];
if (!url || !out) {
  console.error('usage: node tools/capture-gardener.mjs <url> <out.png>');
  process.exit(2);
}

const bearer = process.env.BEADS_VIEWER_BEARER || '';
const fullPage = !!process.env.BEADS_VIEWER_SHOT_FULLPAGE;
const width = parseInt(process.env.BEADS_VIEWER_SHOT_WIDTH || '1440', 10);
const height = parseInt(process.env.BEADS_VIEWER_SHOT_HEIGHT || '900', 10);
const dumpHtml = process.env.BEADS_VIEWER_DUMP_HTML || '';

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
  const resp = await page.goto(url, { waitUntil: 'domcontentloaded', timeout: 60000 });
  if (resp && resp.status() >= 400) {
    console.error(`capture-gardener: ${url} returned HTTP ${resp.status()}`);
    process.exit(1);
  }
  // Wait for life-thread cards; tolerate a gardener with no iterations.
  try {
    await page.waitForSelector('.gardener-iter', { timeout: 8000 });
  } catch {
    console.error('capture-gardener: no .gardener-iter cards within 8s — capturing as-is');
  }
  // Let any in-flight htmx swaps settle.
  await page.waitForTimeout(1500);
  if (dumpHtml) {
    const fs = await import('node:fs');
    fs.writeFileSync(dumpHtml, await page.content());
    console.error(`capture-gardener: wrote HTML ${dumpHtml}`);
  }
  await page.screenshot({ path: out, fullPage });
  console.error(`capture-gardener: wrote ${out}`);
} finally {
  await browser.close();
}
