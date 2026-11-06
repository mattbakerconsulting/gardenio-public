#!/usr/bin/env node
// Render SVG files to PNG using headless Chromium via playwright-core.
//
// Used by docs that embed SVG diagrams to also publish rasterized PNGs
// alongside the SVG sources, so reviewers in viewers that do not render
// SVGs (GitLab MR diff thumbnails, some local readers) can still see the
// mockups. SVGs remain the source of truth.
//
// Usage:
//   node scripts/render-svgs.mjs <svg-dir> <out-dir> [scale]
//
// Example:
//   node scripts/render-svgs.mjs \
//     gardenio/docs/design/inbox-visual \
//     gardenio/docs/design/inbox-visual/rendered \
//     2
//
// Requires the agent image (or any environment that has playwright-core
// + a Chromium binary at /ms-playwright/chromium-1222/chrome-linux64/chrome
// or /opt/google/chrome/chrome). On a contributor laptop, run inside the
// agent container or set CHROME_PATH to a local Chrome.

import fs from 'fs/promises';
import path from 'path';

const PLAYWRIGHT_CORE =
  '/usr/local/lib/node_modules/playwright/node_modules/playwright-core/index.mjs';

const CHROME_CANDIDATES = [
  process.env.CHROME_PATH,
  '/ms-playwright/chromium-1222/chrome-linux64/chrome',
  '/ms-playwright/chromium-1217/chrome-linux64/chrome',
  '/opt/google/chrome/chrome',
].filter(Boolean);

async function pickChrome() {
  for (const p of CHROME_CANDIDATES) {
    try {
      await fs.access(p);
      return p;
    } catch (_) {}
  }
  throw new Error(
    `no chromium binary found; tried ${CHROME_CANDIDATES.join(', ')}`,
  );
}

const [, , svgDir, outDir, scaleStr] = process.argv;
const scale = parseFloat(scaleStr || '2');
if (!svgDir || !outDir) {
  console.error('usage: node scripts/render-svgs.mjs <svg-dir> <out-dir> [scale]');
  process.exit(2);
}

const { chromium } = await import(PLAYWRIGHT_CORE);
const executablePath = await pickChrome();

await fs.mkdir(outDir, { recursive: true });
const entries = (await fs.readdir(svgDir)).filter((f) => f.endsWith('.svg'));

const browser = await chromium.launch({
  executablePath,
  args: ['--no-sandbox', '--disable-dev-shm-usage'],
});

for (const file of entries) {
  const svgPath = path.join(svgDir, file);
  const svg = await fs.readFile(svgPath, 'utf8');
  const m = svg.match(/viewBox="([\d.\-]+)\s+([\d.\-]+)\s+([\d.\-]+)\s+([\d.\-]+)"/);
  if (!m) {
    console.warn(`skip ${file}: no viewBox`);
    continue;
  }
  const w = Math.round(parseFloat(m[3]));
  const h = Math.round(parseFloat(m[4]));

  const html = `<!doctype html><html><head><meta charset="utf-8"><style>
    html,body{margin:0;padding:0;background:#fff}
    svg{display:block;width:${w}px;height:${h}px}
  </style></head><body>${svg}</body></html>`;

  const ctx = await browser.newContext({
    viewport: { width: w, height: h },
    deviceScaleFactor: scale,
  });
  const page = await ctx.newPage();
  await page.setContent(html, { waitUntil: 'networkidle' });
  await page.waitForTimeout(50);
  const pngPath = path.join(outDir, file.replace(/\.svg$/, '.png'));
  await page.screenshot({
    path: pngPath,
    omitBackground: false,
    clip: { x: 0, y: 0, width: w, height: h },
  });
  await ctx.close();
  console.log(`rendered ${file} -> ${pngPath} (${w}x${h} @${scale}x)`);
}

await browser.close();
