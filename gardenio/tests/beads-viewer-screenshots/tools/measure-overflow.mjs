// Measure page-level horizontal overflow (scrollWidth - innerWidth) at a
// given viewport for the formula-ux Skin gate. Emits overflow.json:
//   { "<slug>": <overflowPx>, ... }  (0 or negative == no page overflow)
import { chromium } from '@playwright/test';
import { writeFileSync } from 'fs';
const base = process.argv[2];
const out = process.argv[3];
const width = parseInt(process.env.OVERFLOW_WIDTH || '375', 10);
const height = parseInt(process.env.OVERFLOW_HEIGHT || '667', 10);
const pages = [
  ['formulas-catalog', '/formulas'],
  ['formula-dag', process.env.FORMULA_PATH],
  ['molecule-exec', process.env.MOLECULE_PATH],
];
const browser = await chromium.launch();
const result = {};
try {
  const context = await browser.newContext({ viewport: { width, height }, ignoreHTTPSErrors: true, colorScheme: 'light' });
  for (const [slug, path] of pages) {
    const page = await context.newPage();
    await page.goto(base + path, { waitUntil: 'load', timeout: 60000 });
    await page.waitForTimeout(1500);
    const m = await page.evaluate(() => ({
      scrollWidth: document.documentElement.scrollWidth,
      innerWidth: window.innerWidth,
    }));
    result[slug] = m.scrollWidth - m.innerWidth;
    console.error(`${slug}: scrollWidth=${m.scrollWidth} innerWidth=${m.innerWidth} overflow=${result[slug]}`);
    await page.close();
  }
  writeFileSync(out, JSON.stringify(result, null, 2));
  console.error(`wrote ${out}`);
} finally { await browser.close(); }
