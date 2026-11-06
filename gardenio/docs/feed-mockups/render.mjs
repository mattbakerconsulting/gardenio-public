// render.mjs — rasterize each *.svg in this dir to a same-named *.png.
// Usage: node render.mjs
// Requires the global playwright module + an installed chromium headless shell.
import pkg from '/usr/local/lib/node_modules/playwright/index.js';
const { chromium } = pkg;
import { readdirSync, readFileSync, writeFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const dir = dirname(fileURLToPath(import.meta.url));
const svgs = readdirSync(dir).filter(f => f.endsWith('.svg') && !f.startsWith('_'));

const browser = await chromium.launch({
  executablePath: process.env.CHROME_BIN,
  args: ['--no-sandbox'],
});
const page = await browser.newPage({ deviceScaleFactor: 2 });
for (const f of svgs) {
  const svg = readFileSync(join(dir, f), 'utf8');
  const m = svg.match(/width="(\d+)"[^>]*height="(\d+)"/);
  const w = m ? +m[1] : 1200, h = m ? +m[2] : 800;
  await page.setViewportSize({ width: w, height: h });
  await page.setContent(
    `<!doctype html><html><body style="margin:0">${svg}</body></html>`,
    { waitUntil: 'networkidle' });
  const el = await page.$('svg');
  await el.screenshot({ path: join(dir, f.replace(/\.svg$/, '.png')) });
  console.log('rendered', f, `${w}x${h}`);
}
await browser.close();
