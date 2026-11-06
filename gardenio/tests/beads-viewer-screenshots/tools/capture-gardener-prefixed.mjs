// capture-gardener-prefixed.mjs — capture a beads-viewer /gardener/{id} page
// when the app is reached via the in-cluster ClusterIP (served at root) but
// the rendered DOM references a base-path prefix (e.g. `/beads/...`, the
// public-ingress mount point at https://prod.gardenio.acme.example/beads).
//
// Why this exists (ka-0000001553 capture step): an agent pod without
// port-forward / get-secrets RBAC reaches beads-viewer directly at
// http://gardenio-beads-viewer.<ns>:8095, where the router is mounted at
// root. But the templates emit absolute asset + htmx URLs under the deployed
// base path `/beads/...` (correct for the ingress). On the ClusterIP those
// 404, so htmx.min.js never loads and every lazy section — the `/runs` tail
// and the "Connected beads" panel — stays stuck on its "Loading…" placeholder.
// A screenshot taken that way is a CAPTURE ARTIFACT, not the operator's real
// view, and would wrongly tank the Correctness / Relevance CDQR scores.
//
// This wrapper installs a Playwright route handler that strips a configurable
// URL prefix (default `/beads`) from every request the page makes, so assets
// and htmx GETs resolve against the ClusterIP root exactly as they would
// behind the ingress. The result is a faithful render: htmx boots, the lazy
// `/runs` tail swaps in, and the Connected-beads panel resolves.
//
// Usage:
//   node tools/capture-gardener-prefixed.mjs <url> <out.png>
//
// Env:
//   BEADS_VIEWER_BEARER          bearer token (Authorization header), optional
//   BEADS_VIEWER_STRIP_PREFIX    base path to strip from requests (default /beads)
//   BEADS_VIEWER_ORIGIN          origin to route rewritten requests to
//                                (default: the origin of <url>)
//   BEADS_VIEWER_SHOT_FULLPAGE   if set, capture the full scrollable page
//   BEADS_VIEWER_SHOT_WIDTH/HEIGHT  viewport (default 1440x900)
//   BEADS_VIEWER_DUMP_HTML       if set, write final rendered HTML to this path
//   BEADS_VIEWER_SETTLE_MS       extra settle time after load (default 3500)
import { chromium } from '@playwright/test';

const url = process.argv[2];
const out = process.argv[3];
if (!url || !out) {
  console.error('usage: node tools/capture-gardener-prefixed.mjs <url> <out.png>');
  process.exit(2);
}

const bearer = process.env.BEADS_VIEWER_BEARER || '';
const stripPrefix = process.env.BEADS_VIEWER_STRIP_PREFIX ?? '/beads';
const origin = process.env.BEADS_VIEWER_ORIGIN || new URL(url).origin;
const fullPage = !!process.env.BEADS_VIEWER_SHOT_FULLPAGE;
const width = parseInt(process.env.BEADS_VIEWER_SHOT_WIDTH || '1440', 10);
const height = parseInt(process.env.BEADS_VIEWER_SHOT_HEIGHT || '900', 10);
const dumpHtml = process.env.BEADS_VIEWER_DUMP_HTML || '';
const settleMs = parseInt(process.env.BEADS_VIEWER_SETTLE_MS || '3500', 10);

const browser = await chromium.launch();
try {
  const context = await browser.newContext({
    viewport: { width, height },
    ignoreHTTPSErrors: true,
    extraHTTPHeaders: bearer ? { Authorization: `Bearer ${bearer}` } : undefined,
    reducedMotion: 'reduce',
    colorScheme: 'light',
  });

  // Rewrite any request whose path begins with the deployed base prefix so it
  // resolves against the ClusterIP root. Only touches same-origin requests to
  // avoid disturbing external (data:, blob:) resources.
  if (stripPrefix) {
    const rewritten = [];
    await context.route('**/*', async (route) => {
      const req = route.request();
      let u;
      try {
        u = new URL(req.url());
      } catch {
        return route.continue();
      }
      if (u.origin === origin && u.pathname.startsWith(stripPrefix + '/')) {
        const target = origin + u.pathname.slice(stripPrefix.length) + u.search;
        rewritten.push(u.pathname);
        return route.continue({ url: target });
      }
      return route.continue();
    });
    context.on('close', () => {
      if (rewritten.length) {
        console.error(`capture-gardener-prefixed: rewrote ${rewritten.length} `
          + `${stripPrefix}/* requests (e.g. ${rewritten.slice(0, 4).join(', ')})`);
      }
    });
  }

  const page = await context.newPage();
  const resp = await page.goto(url, { waitUntil: 'domcontentloaded', timeout: 60000 });
  if (resp && resp.status() >= 400) {
    console.error(`capture-gardener-prefixed: ${url} returned HTTP ${resp.status()}`);
    process.exit(1);
  }
  try {
    await page.waitForSelector('.gardener-iter', { timeout: 8000 });
  } catch {
    console.error('capture-gardener-prefixed: no .gardener-iter cards within 8s — capturing as-is');
  }
  // Lazy sections use `hx-trigger="intersect once"` (IntersectionObserver), so a
  // fullPage screenshot alone never fires them — the element must actually enter
  // the viewport. This bites hardest at narrow widths, where the Connected-beads
  // panel sits far below the fold and stays stuck on "Loading connected beads…".
  // Auto-scroll the whole document so every intersect-once section triggers, then
  // scroll back to the top for the screenshot. (Without this the narrow capture is
  // a no-scroll artifact, not the operator's real view.)
  await page.evaluate(async () => {
    const step = Math.max(400, Math.floor(window.innerHeight * 0.9));
    for (let y = 0; y <= document.body.scrollHeight; y += step) {
      window.scrollTo(0, y);
      await new Promise((r) => setTimeout(r, 120));
    }
    window.scrollTo(0, 0);
  });
  // Give htmx time to boot + swap the lazy /runs tail and Connected-beads panel.
  await page.waitForTimeout(settleMs);
  if (dumpHtml) {
    const fs = await import('node:fs');
    fs.writeFileSync(dumpHtml, await page.content());
    console.error(`capture-gardener-prefixed: wrote HTML ${dumpHtml}`);
  }
  await page.screenshot({ path: out, fullPage });
  console.error(`capture-gardener-prefixed: wrote ${out}`);
} finally {
  await browser.close();
}
