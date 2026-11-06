// capture-page-sse.mjs — like capture-page.mjs but for pages holding an
// open SSE connection (the molecule execution view keeps
// /molecule/{id}/events open, so `networkidle` never fires). Uses
// domcontentloaded + a fixed settle instead. Same env contract.
import { chromium } from '@playwright/test';

const url = process.argv[2];
const out = process.argv[3];
if (!url || !out) {
  console.error('usage: node tools/capture-page-sse.mjs <url> <out.png>');
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
  const resp = await page.goto(url, { waitUntil: 'domcontentloaded', timeout: 30000 });
  if (resp && resp.status() >= 400) {
    console.error(`capture-page-sse: ${url} returned HTTP ${resp.status()}`);
    process.exit(1);
  }
  // Let styles/fonts/SVG paint; SSE stays open but the initial DOM is done.
  await page.waitForTimeout(2500);
  await page.screenshot({ path: out, fullPage });
  console.error(`capture-page-sse: wrote ${out}`);
} finally {
  await browser.close();
}
