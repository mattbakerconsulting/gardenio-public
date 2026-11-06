// Focused per-user-homepage capture for the Users-FLAT audit (gd-4GJtA5pnGV).
//
// Replicates the canonical-pages.spec.js capture() behavior (networkidle wait,
// animation freeze, one rAF tick, full-page PNG + post-htmx page.content() DOM)
// but drives ONLY /u/<slug> at both viewports, and lets the caller supply the
// Authorization bearer + output prefix so we can capture the SELF view and the
// CROSS view (a distinct non-owner viewer subject) into separate files.
//
// Usage:
//   BEADS_VIEWER_URL=http://localhost:8095 \
//   BEARER=<jwt> SLUG=avery.locke OUT_PREFIX=user-home \
//   OUT_DIR=docs/users-flat/<iter>/captures \
//   node tools/capture-user-home.mjs
import { chromium } from '@playwright/test';
import fs from 'node:fs';
import path from 'node:path';

const baseURL = process.env.BEADS_VIEWER_URL || 'http://localhost:8095';
const bearer = process.env.BEARER || '';
const slug = process.env.SLUG;
const outPrefix = process.env.OUT_PREFIX || 'user-home';
const outDir = process.env.OUT_DIR || '.';
if (!slug) {
  console.error('SLUG env var is required');
  process.exit(2);
}

const VIEWPORTS = [
  { name: 'desktop', width: 1280, height: 800 },
  { name: 'narrow', width: 480, height: 800 },
];

const FREEZE_CSS = `
  *, *::before, *::after {
    animation-duration: 0s !important;
    animation-delay: 0s !important;
    transition-duration: 0s !important;
    transition-delay: 0s !important;
    caret-color: transparent !important;
    scroll-behavior: auto !important;
  }
  html { scroll-behavior: auto !important; }
`;

const url = `${baseURL}/u/${encodeURIComponent(slug)}`;

const browser = await chromium.launch();
let failed = false;
try {
  for (const vp of VIEWPORTS) {
    const context = await browser.newContext({
      viewport: { width: vp.width, height: vp.height },
      deviceScaleFactor: 1,
      ignoreHTTPSErrors: true,
      reducedMotion: 'reduce',
      colorScheme: 'light',
      locale: 'en-US',
      timezoneId: 'UTC',
      extraHTTPHeaders: bearer ? { Authorization: `Bearer ${bearer}` } : undefined,
    });
    const page = await context.newPage();
    const response = await page.goto(url, { waitUntil: 'networkidle' });
    if (!response) throw new Error(`no response for ${url}`);
    const status = response.status();
    if (status >= 400) throw new Error(`HTTP ${status} for ${url}`);
    await page.addStyleTag({ content: FREEZE_CSS });
    await page.evaluate(() => new Promise((r) => requestAnimationFrame(() => r())));

    const pngDir = path.join(outDir, 'screenshots', vp.name);
    const htmlDir = path.join(outDir, 'html');
    fs.mkdirSync(pngDir, { recursive: true });
    fs.mkdirSync(htmlDir, { recursive: true });
    const pngPath = path.join(pngDir, `${outPrefix}.png`);
    await page.screenshot({ path: pngPath, fullPage: true });
    // Post-render DOM (picks up htmx-loaded partials), same as harness.
    const html = await page.content();
    fs.writeFileSync(path.join(htmlDir, `${outPrefix}.html`), html);
    console.log(`captured ${vp.name} ${outPrefix} <- ${url} (HTTP ${status}) -> ${pngPath}`);
    await context.close();
  }
} catch (e) {
  console.error(`CAPTURE FAILED: ${e.message}`);
  failed = true;
} finally {
  await browser.close();
}
process.exit(failed ? 1 : 0);
