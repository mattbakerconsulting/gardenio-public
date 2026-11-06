// capture-plans.mjs — capture the /plans index + /plans/{id} cockpit at
// desktop (1280x900) and narrow (375x667) for the plans-ux-audit (PACES)
// harness, on BOTH the LIGHT and DARK skins, and emit the render-metrics.json
// (overflow + grid-track count) that measure-plans.py consumes for the S
// (Skin/responsive) dimension.
//
// Companion to measure-plans.py. The plain `playwright screenshot` CLI
// cannot attach an Authorization header, and the S-dimension needs the
// in-page scrollWidth/innerWidth + computed grid-template-columns which a
// static screenshot can't give — so this drives chromium directly.
//
// SKIN AXIS (ka-0000000260): before this pass the driver hardcoded
// colorScheme:'light' and only ever rendered the LIGHT skin, so the rubric's
// "Skin" dimension was structurally blind to dark-skin regressions — exactly
// the debt iteration-16 (ka-0000007937) had to pay off retroactively across
// 16 chips (white cards on a black page). We now render BOTH skins so a dark
// regression shows up as a scored dimension, not an invisible gap. The dark
// pass mirrors the sibling tool capture-inbox-skins.mjs exactly: seed
// localStorage['beads-viewer:skin']='dark-terminal' before first paint (the
// layout.html boot script applies it), force data-skin after each nav
// (belt+braces in case the boot script raced), and use colorScheme:'dark'.
//
// The LIGHT pass is byte-identical to before (same colorScheme:'light', NO
// skin seed, NO data-skin force) so existing light metrics don't move; only
// its output path moves under screenshots/light/. The dark pass is additive.
//
// Usage:
//   node tools/capture-plans.mjs
//
// Env:
//   TARGET_URL            viewer base (default http://127.0.0.1:8095)
//   PLAN_ID               cockpit plan epic id (required)
//   OUT_DIR               output root; writes OUT_DIR/captures/... (required)
//   BEADS_VIEWER_BEARER   optional Authorization bearer (omit under dev-auth-bypass)
import { chromium } from '@playwright/test';
import fs from 'fs';

const BASE = process.env.TARGET_URL || 'http://127.0.0.1:8095';
const PLAN_ID = process.env.PLAN_ID;
const OUT = process.env.OUT_DIR;
const bearer = process.env.BEADS_VIEWER_BEARER || '';
if (!PLAN_ID || !OUT) {
  console.error('usage: PLAN_ID=<id> OUT_DIR=<dir> node tools/capture-plans.mjs');
  process.exit(2);
}

const pages = [
  { slug: 'plans-index', path: '/plans?operated_by=' },
  { slug: 'plan-detail', path: `/plans/${PLAN_ID}` },
];
const viewports = [
  { name: 'desktop', width: 1280, height: 900 },
  { name: 'narrow', width: 375, height: 667 },
];
// The skin axis. `light` is byte-identical to the historical single-skin pass
// (no localStorage seed, no data-skin force). `dark` mirrors the skin-switch
// mechanism proven in capture-inbox-skins.mjs — 'dark-terminal' is the dark
// skin in that tool's SKINS list.
const skins = [
  { name: 'light', colorScheme: 'light', skinKey: null, dataSkin: null },
  { name: 'dark', colorScheme: 'dark', skinKey: 'dark-terminal', dataSkin: 'dark-terminal' },
];

const metrics = {};
const browser = await chromium.launch();
try {
  for (const skin of skins) {
    for (const vp of viewports) {
      const ctx = await browser.newContext({
        viewport: { width: vp.width, height: vp.height },
        ignoreHTTPSErrors: true,
        extraHTTPHeaders: bearer ? { Authorization: `Bearer ${bearer}` } : undefined,
        reducedMotion: 'reduce',
        colorScheme: skin.colorScheme,
      });
      const page = await ctx.newPage();
      // Seed the skin into localStorage BEFORE first paint so layout.html's
      // inline boot script applies data-skin with no FOUC. Light seeds nothing
      // (renders the default 'baseline' skin), keeping it byte-identical.
      if (skin.skinKey) {
        await page.addInitScript((s) => {
          try { localStorage.setItem('beads-viewer:skin', s); } catch (e) { /* private-mode */ }
        }, skin.skinKey);
      }
      for (const p of pages) {
        const url = BASE + p.path;
        await page.goto(url, { waitUntil: 'networkidle', timeout: 30000 }).catch(async () => {
          await page.goto(url, { waitUntil: 'domcontentloaded', timeout: 30000 });
        });
        // Belt+braces: force the skin attribute in case the boot script ran
        // before the init script seeded localStorage (mirror capture-inbox-skins).
        if (skin.dataSkin) {
          await page.evaluate((s) => document.documentElement.setAttribute('data-skin', s), skin.dataSkin);
        }
        await page.waitForTimeout(800);
        const dir = `${OUT}/captures/screenshots/${skin.name}/${vp.name}`;
        fs.mkdirSync(dir, { recursive: true });
        await page.screenshot({ path: `${dir}/${p.slug}.png`, fullPage: true });
        const m = await page.evaluate(() => {
          const de = document.documentElement;
          const grid = document.querySelector('.plan-grid');
          let tracks = null;
          if (grid) {
            const tc = getComputedStyle(grid).gridTemplateColumns || '';
            tracks = tc.trim() === '' || tc === 'none' ? 0 : tc.split(/\s+/).filter(Boolean).length;
          }
          return {
            scrollWidth: de.scrollWidth,
            innerWidth: window.innerWidth,
            overflow: de.scrollWidth - window.innerWidth,
            grid_tracks: tracks,
            skin: de.getAttribute('data-skin'),
          };
        });
        // Keep the LIGHT key format byte-identical (`${slug}__${vp}`) so the
        // existing measure_skin light reads don't move; suffix dark keys.
        const key = skin.name === 'light' ? `${p.slug}__${vp.name}` : `${p.slug}__${vp.name}__${skin.name}`;
        metrics[key] = m;
        console.error(`[${skin.name}] ${p.slug} @ ${vp.name}: overflow=${m.overflow} tracks=${m.grid_tracks} skin=${m.skin}`);
      }
      await ctx.close();
    }
  }
} finally {
  await browser.close();
}
fs.mkdirSync(`${OUT}/captures`, { recursive: true });
fs.writeFileSync(`${OUT}/captures/render-metrics.json`, JSON.stringify(metrics, null, 2));
console.error('wrote render-metrics.json');
