/* The viewer's two renderings of a Slack wish, shot for plan gd-gO1fTT7oGC (TestWishViewerShoot): its bead page /<head> and
   its fused thread /threads/<head>, at a phone's size (390 px) and a desktop's, in the day skin, full page, signed in.
   Usage: node wish_shoot_browser.js <server url> <node_modules dir> <out dir> <head id>. */
'use strict';
var base = process.argv[2], mods = process.argv[3], out = process.argv[4], head = process.argv[5], path = require('path');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));
var CELLS = [
  { name: 'phone-light', ctx: { viewport: { width: 390, height: 844 }, deviceScaleFactor: 2, hasTouch: true, isMobile: true } },
  { name: 'desktop-light', ctx: { viewport: { width: 1440, height: 900 } } },
];
var VIEWS = [{ name: 'viewerbead', path: '/' + head }, { name: 'viewerthread', path: '/threads/' + head }];

(async function () {
  var browser = await pw.chromium.launch();
  try {
    for (var cell of CELLS) {
      var ctx = await browser.newContext(Object.assign({ extraHTTPHeaders: { 'X-Auth-Request-Email': 'matt@example.com' } }, cell.ctx));
      for (var v of VIEWS) {
        var page = await ctx.newPage(), res = await page.goto(base + v.path, { waitUntil: 'networkidle' });
        if (!res.ok()) throw new Error(v.path + ': HTTP ' + res.status());
        await page.evaluate(function () { document.documentElement.setAttribute('data-skin', 'homestead'); });
        var file = v.name + '-' + cell.name + '.png';
        await page.screenshot({ path: path.join(out, file), fullPage: true });
        console.log('shot ' + file);
        await page.close();
      }
      await ctx.close();
    }
  } finally {
    await browser.close();
  }
})().catch(function (e) { console.error(e); process.exit(1); });
