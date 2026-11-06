/* The owner's preview of a land window in a real Chromium (LW.5 gd-aUQRspcbeS), run from TestLandSharePreviewUnderChromium
   against the viewer's routes on an httptest server, the preview reading internal/planwindow's mixed land fixture. Usage:
   node land_share_preview_browser.js <preview url> <node_modules dir>; LAND_SHARE_SHOTS=<dir> saves a screenshot per size.

   At a phone's size and a desktop's: land.js shows the canvas and draws the World from the page's inline document in many
   colours, as /w/{token} draws it from land.json; a plan's name in the list opens its card; the page never asks for
   land.json (an owner's signed-in preview has none) nor anything outside /w/static/ and the skin, and nothing throws. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert'), path = require('path');
var shots = process.env.LAND_SHARE_SHOTS || '';
var pw = require(require.resolve('playwright-core', { paths: [mods] }));
var CELLS = [
  { name: 'phone', ctx: { viewport: { width: 390, height: 844 }, deviceScaleFactor: 2, hasTouch: true, isMobile: true } },
  { name: 'desktop', ctx: { viewport: { width: 1440, height: 900 } } },
];

function colours(page) {
  return page.evaluate(function () {
    var cv = document.getElementById('w-cv'), d = cv.getContext('2d').getImageData(0, 0, cv.width, cv.height).data, seen = {};
    for (var k = 0; k < d.length; k += 4) seen[d[k] << 16 | d[k + 1] << 8 | d[k + 2]] = 1;
    return Object.keys(seen).length;
  });
}

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); } catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  try {
    for (var cell of CELLS) {
      var ctx = await browser.newContext(cell.ctx), page = await ctx.newPage(), errs = [], asked = [];
      page.on('pageerror', function (e) { errs.push(String(e)); });
      page.on('request', function (r) { asked.push(new URL(r.url()).pathname); });
      await page.goto(url);
      await page.waitForSelector('#w-world:not([hidden])', { timeout: 15000 });
      var n = 0;
      for (var i = 0; i < 40 && n <= 12; i++) { await page.waitForTimeout(400); n = await colours(page); }
      assert.ok(n > 12, cell.name + ': only ' + n + ' colours on the canvas');
      if (shots) await page.screenshot({ path: path.join(shots, 'preview-' + cell.name + '.png') });

      await page.locator('#w-plans li[data-plan] .w-what').first().click();
      await page.waitForSelector('#w-card:not([hidden])', { timeout: 5000 });
      var title = await page.locator('#w-card h2').textContent();
      assert.strictEqual(title, 'Grow the orchard', cell.name + ': the list opened ' + JSON.stringify(title));

      var start = new URL(url).pathname;
      var stray = asked.filter(function (p) { return p !== start && !/^\/w\/static\//.test(p) && !/^\/lands\/static\//.test(p); });
      assert.deepStrictEqual(stray, [], cell.name + ': fetched outside the page, /w/static/ and the skin');
      assert.ok(!asked.some(function (p) { return /land\.json$/.test(p); }), cell.name + ': asked for land.json');
      assert.deepStrictEqual(errs, [], cell.name + ': page errors');
      console.log(cell.name + ': ' + n + ' colours; the list opened "' + title + '"; ' + asked.length + ' requests');
      await ctx.close();
    }
  } finally {
    await browser.close();
  }
})().catch(function (e) { console.error(String(e.stack || e)); process.exit(1); });
