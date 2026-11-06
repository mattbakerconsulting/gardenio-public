import { chromium } from 'playwright';
const BASE = process.env.BEADS_VIEWER_URL || 'http://localhost:8095';
const BEARER = process.env.BEADS_VIEWER_BEARER;
const FOLD = 900;
const browser = await chromium.launch({ headless: true });
const results = [];
const SKINS = ['baseline','minimal-refined','dense-admin','docs-notion','dark-terminal','tufte'];
for (const skin of SKINS) {
  const ctx = await browser.newContext({
    viewport: { width: 1280, height: 900 },
    deviceScaleFactor: 1,
    colorScheme: skin === 'dark-terminal' ? 'dark' : 'light',
    extraHTTPHeaders: BEARER ? { Authorization: `Bearer ${BEARER}` } : undefined,
  });
  const page = await ctx.newPage();
  await page.addInitScript((s) => { try { localStorage.setItem('beads-viewer:skin', s); } catch(e){} }, skin);
  const resp = await page.goto(`${BASE}/inbox`, { waitUntil: 'domcontentloaded', timeout: 30000 });
  await page.evaluate((s) => document.documentElement.setAttribute('data-skin', s), skin);
  await page.waitForTimeout(900);
  const m = await page.evaluate((fold) => {
    const rows = Array.from(document.querySelectorAll('.inbox-row, [data-inbox-row], .mention-row, .mine-row, li.row, .row, article'));
    // narrower: identify inbox stream rows by stable class
    const streamRows = Array.from(document.querySelectorAll('.inbox-stream .row, .inbox-stream li, .stream .inbox-item, [data-row], .stream-row, .inbox > ul > li, .inbox-list > *'));
    const candidates = streamRows.length ? streamRows : rows;
    // pick rows whose offsetParent is body (visible) and height > 12
    const visible = candidates.filter(el => el.offsetHeight > 12 && el.getBoundingClientRect().width > 200);
    const heights = visible.map(el => el.offsetHeight);
    const tops = visible.map(el => el.getBoundingClientRect().top + window.scrollY);
    const aboveFold = visible.filter(el => (el.getBoundingClientRect().top + window.scrollY) < fold).length;
    const median = (a) => { const s=[...a].sort((x,y)=>x-y); return s.length?s[Math.floor(s.length/2)]:0; };
    const min = (a) => a.length ? Math.min(...a) : 0;
    return {
      doc_h: document.documentElement.scrollHeight,
      doc_w: document.documentElement.scrollWidth,
      n_candidates: candidates.length,
      n_visible: visible.length,
      median_row_h: median(heights),
      min_row_h: min(heights),
      max_row_h: heights.length ? Math.max(...heights) : 0,
      rows_above_fold: aboveFold,
      row_selectors_used: streamRows.length ? 'stream' : 'fallback',
    };
  }, FOLD);
  results.push({ skin, ...m });
  await ctx.close();
}
await browser.close();
console.log(JSON.stringify(results, null, 2));
