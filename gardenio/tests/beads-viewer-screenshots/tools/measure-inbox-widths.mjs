import { chromium } from 'playwright';
const BASE = process.env.BEADS_VIEWER_URL || 'http://localhost:8095';
const BEARER = process.env.BEADS_VIEWER_BEARER;
const FOLD = 900;
const SKINS = ['baseline','minimal-refined','dense-admin','docs-notion','dark-terminal','tufte'];
const WIDTHS = [480, 768, 960, 1280, 1600];
const browser = await chromium.launch({ headless: true });
const results = [];
for (const skin of SKINS) {
  for (const width of WIDTHS) {
    const ctx = await browser.newContext({
      viewport: { width, height: 900 },
      deviceScaleFactor: 1,
      colorScheme: skin === 'dark-terminal' ? 'dark' : 'light',
      extraHTTPHeaders: BEARER ? { Authorization: `Bearer ${BEARER}` } : undefined,
    });
    const page = await ctx.newPage();
    await page.addInitScript((s) => { try { localStorage.setItem('beads-viewer:skin', s); } catch(e){} }, skin);
    const resp = await page.goto(`${BASE}/inbox`, { waitUntil: 'domcontentloaded', timeout: 30000 });
    if (!resp || resp.status() >= 400) {
      results.push({ skin, width, error: `HTTP ${resp ? resp.status() : 'null'}` });
      await ctx.close();
      continue;
    }
    await page.evaluate((s) => document.documentElement.setAttribute('data-skin', s), skin);
    await page.waitForTimeout(900);
    const m = await page.evaluate((fold) => {
      const streamRows = Array.from(document.querySelectorAll('.inbox-stream .row, .inbox-stream li, .stream .inbox-item, [data-row], .stream-row, .inbox > ul > li, .inbox-list > *'));
      const visible = streamRows.filter(el => el.offsetHeight > 12 && el.getBoundingClientRect().width > 200);
      const heights = visible.map(el => el.offsetHeight);
      const aboveFold = visible.filter(el => (el.getBoundingClientRect().top + window.scrollY) < fold).length;
      const median = (a) => { const s=[...a].sort((x,y)=>x-y); return s.length?s[Math.floor(s.length/2)]:0; };
      const firstRowH = visible.length ? visible[0].offsetHeight : 0;
      return {
        doc_h: document.documentElement.scrollHeight,
        n_visible: visible.length,
        median_row_h: median(heights),
        min_row_h: heights.length ? Math.min(...heights) : 0,
        max_row_h: heights.length ? Math.max(...heights) : 0,
        first_row_h: firstRowH,
        rows_above_fold: aboveFold,
      };
    }, FOLD);
    results.push({ skin, width, ...m });
    await ctx.close();
  }
}
await browser.close();
console.log(JSON.stringify(results, null, 2));
