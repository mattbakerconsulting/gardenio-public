// Standalone Home-index capture for the home-beauty audit against a LIVE prod
// beads-viewer.
//
// TWO correctness notes this script pins down (both bit the 2026-08-24T1825
// iteration):
//   1. SURFACE: the Home page (handleIndex — the "Live agents" card grid + stats
//      banner) is served at `/index`, NOT `/`. `GET /` is the Activity feed
//      (title "Activity feed"). The home-beauty molecule audits `/index`, so
//      CAPTURE_PATH defaults to `/index`.
//   2. WAIT: the canonical-pages spec captures with waitUntil 'networkidle',
//      which never resolves against a live daemon because both surfaces mount a
//      long-lived SSE bead-feed channel (bead_feed_hub.go). This script uses
//      'load' + an explicit settle delay (the same treatment the spec already
//      gives SSE pages like /feed and /agents).
//
// Output layout matches the harness so the measure/judge steps consume it
// unchanged:
//   test-results/beads-viewer-screenshots/<viewport>/index.{png,html}
import { chromium } from '@playwright/test';
import fs from 'node:fs';
import path from 'node:path';

const BASE = process.env.BEADS_VIEWER_URL || 'http://localhost:8095';
const CAPTURE_PATH = process.env.CAPTURE_PATH || '/index';
const SETTLE_MS = Number(process.env.SETTLE_MS || 4000);
// The live prod /index fans out per-project bead counts against the daemon, so
// first-byte can take 30-40s over a port-forward — well past Playwright's 30s
// default nav timeout. Make it env-configurable and default it comfortably high.
const NAV_TIMEOUT_MS = Number(process.env.NAV_TIMEOUT_MS || 90000);
const OUTPUT_ROOT = path.resolve(
  process.cwd(),
  'test-results/beads-viewer-screenshots',
);

const VIEWPORTS = {
  desktop: { width: 1280, height: 800 },
  narrow: { width: 480, height: 800 },
};

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

for (const [vp, viewport] of Object.entries(VIEWPORTS)) {
  const browser = await chromium.launch();
  const context = await browser.newContext({
    viewport,
    deviceScaleFactor: 1,
    reducedMotion: 'reduce',
    colorScheme: 'light',
    locale: 'en-US',
    timezoneId: 'UTC',
    ignoreHTTPSErrors: true,
  });
  const page = await context.newPage();
  const url = `${BASE}${CAPTURE_PATH}`;
  const response = await page.goto(url, {
    waitUntil: 'load',
    timeout: NAV_TIMEOUT_MS,
  });
  if (!response) throw new Error(`no response for ${url}`);
  if (response.status() >= 400) {
    throw new Error(`HTTP ${response.status()} for ${url}`);
  }
  await page.waitForTimeout(SETTLE_MS);
  await page.addStyleTag({ content: FREEZE_CSS });
  await page.evaluate(
    () => new Promise((r) => requestAnimationFrame(() => r())),
  );

  const dir = path.join(OUTPUT_ROOT, vp);
  fs.mkdirSync(dir, { recursive: true });
  const png = path.join(dir, 'index.png');
  const html = path.join(dir, 'index.html');
  await page.screenshot({ path: png, fullPage: true });
  fs.writeFileSync(html, await page.content());
  console.log(`[${vp}] ${png} + ${html} (status ${response.status()})`);

  await browser.close();
}
