// cap-cached.mjs — capture a page using an explicit cached chrome binary.
// Usage: node tools/cap-cached.mjs <url> <out.png> <width> <height>
import { chromium } from '@playwright/test';

const [url, out, w, h] = process.argv.slice(2);
const width = parseInt(w || '1280', 10);
const height = parseInt(h || '800', 10);
const exe = process.env.CHROME_EXE;

const browser = await chromium.launch({ executablePath: exe });
try {
  const context = await browser.newContext({
    viewport: { width, height },
    ignoreHTTPSErrors: true,
    reducedMotion: 'reduce',
    colorScheme: 'light',
    deviceScaleFactor: 1,
  });
  const page = await context.newPage();
  const resp = await page.goto(url, { waitUntil: 'networkidle', timeout: 60000 });
  if (resp && resp.status() >= 400) {
    console.error(`cap-cached: ${url} returned HTTP ${resp.status()}`);
    process.exit(1);
  }
  await page.screenshot({ path: out, fullPage: true });
  console.error(`cap-cached: wrote ${out} (${width}x${height})`);
} finally {
  await browser.close();
}
