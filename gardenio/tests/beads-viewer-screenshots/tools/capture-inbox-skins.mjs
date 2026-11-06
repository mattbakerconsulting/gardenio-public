import { chromium } from 'playwright';

const BASE = process.env.BEADS_VIEWER_URL || 'http://localhost:8095';
const BEARER = process.env.BEADS_VIEWER_BEARER;
const OUT = process.env.OUT_DIR || 'docs/info-density/20260515-cycle65/inbox';
const SKINS = ['baseline','minimal-refined','dense-admin','docs-notion','dark-terminal','tufte'];
const WIDTHS = [480, 768, 960, 1280, 1600];

import fs from 'node:fs';

const browser = await chromium.launch({ headless: true });
let failures = 0;
for (const skin of SKINS) {
  for (const width of WIDTHS) {
    const ctx = await browser.newContext({
      viewport: { width, height: 900 },
      deviceScaleFactor: 1,
      ignoreHTTPSErrors: true,
      reducedMotion: 'reduce',
      colorScheme: skin === 'dark-terminal' ? 'dark' : 'light',
      timezoneId: 'UTC',
      locale: 'en-US',
      extraHTTPHeaders: BEARER ? { Authorization: `Bearer ${BEARER}` } : undefined,
    });
    const page = await ctx.newPage();
    // Seed localStorage so the inline boot script applies the skin before render.
    await page.addInitScript((s) => {
      try { localStorage.setItem('beads-viewer:skin', s); } catch (e) {}
    }, skin);
    const url = `${BASE}/inbox`;
    try {
      // domcontentloaded — networkidle hangs on /inbox SSE. We freeze + screenshot after a small settle.
      const resp = await page.goto(url, { waitUntil: 'domcontentloaded', timeout: 30000 });
      if (!resp || resp.status() >= 400) {
        console.error(`SKIP ${skin}@${width}: HTTP ${resp ? resp.status() : 'null'}`);
        failures++;
        await ctx.close();
        continue;
      }
      // Force the skin attribute (in case the boot script ran before init script — belt+braces).
      await page.evaluate((s) => document.documentElement.setAttribute('data-skin', s), skin);
      // Disable animations and let layout settle.
      await page.addStyleTag({ content: `*,*::before,*::after{animation-duration:0s !important;transition-duration:0s !important;caret-color:transparent !important;}html{scroll-behavior:auto !important;}` });
      await page.evaluate(() => new Promise(r => requestAnimationFrame(() => r())));
      // Brief wait for HTMX-loaded streams (mode_a inbox content).
      await page.waitForTimeout(800);
      const dir = `${OUT}/${skin}`;
      fs.mkdirSync(dir, { recursive: true });
      await page.screenshot({ path: `${dir}/${width}.png`, fullPage: true });
      console.log(`OK ${skin}@${width} -> ${dir}/${width}.png`);
    } catch (e) {
      console.error(`FAIL ${skin}@${width}: ${e.message}`);
      failures++;
    }
    await ctx.close();
  }
}
await browser.close();
process.exit(failures > 0 ? 1 : 0);
