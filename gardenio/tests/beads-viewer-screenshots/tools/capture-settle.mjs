// Variant of capture-page.mjs that waits for `load` + a fixed settle,
// for pages with a persistent SSE connection (molecule exec view) where
// networkidle never fires.
import { chromium } from '@playwright/test';
const url = process.argv[2], out = process.argv[3];
if (!url || !out) { console.error('usage: node capture-settle.mjs <url> <out.png>'); process.exit(2); }
const fullPage = !!process.env.BEADS_VIEWER_SHOT_FULLPAGE;
const width = parseInt(process.env.BEADS_VIEWER_SHOT_WIDTH || '1280', 10);
const height = parseInt(process.env.BEADS_VIEWER_SHOT_HEIGHT || '800', 10);
const browser = await chromium.launch();
try {
  const context = await browser.newContext({
    viewport: { width, height }, ignoreHTTPSErrors: true,
    reducedMotion: 'reduce', colorScheme: 'light',
  });
  const page = await context.newPage();
  const resp = await page.goto(url, { waitUntil: 'load', timeout: 60000 });
  if (resp && resp.status() >= 400) { console.error(`HTTP ${resp.status()}`); process.exit(1); }
  await page.waitForTimeout(2500); // settle: DAG SVG layout + any deferred render
  await page.screenshot({ path: out, fullPage });
  console.error(`wrote ${out}`);
} finally { await browser.close(); }
