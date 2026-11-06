/* The exported splash in a real browser (gd-QDdwxnGdTY, gd-OeWiwpmr5Y), run from TestExportSplashUnderChromium against
   ExportSplash's directory served as plain files at /landing/. Usage: node splash_export_browser.js <base url> <node_modules dir>.

   The page pinned to noon UTC on 26 Oct 2026, at 360x240 so SwiftShader draws it quickly, under reduced motion. It draws in
   WebGL (body.dataset.gl on, a frame on the canvas): splash.js import()ed data-three, which is ./three.module.min.js beside
   the page. A bare specifier there resolves only by an import map, so import() rejects and the page falls back to the still
   (gl off), which fails here. three.js comes from /landing/, nothing from the app's origin; the two links are on the app's
   origin. Zero console errors. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  try {
    var errors = [], requests = [];
    var ctx = await browser.newContext({ viewport: { width: 360, height: 240 }, timezoneId: 'UTC', locale: 'en-US', reducedMotion: 'reduce' });
    var page = await ctx.newPage();
    page.on('console', function (m) { if (m.type() === 'error') errors.push(m.text()); });
    page.on('pageerror', function (e) { errors.push(String(e.message || e)); });
    page.on('request', function (r) { requests.push(r.url()); });
    await page.goto(url + '/landing/?t=2026-10-26T12:00:00Z');
    // SwiftShader compiles the scene's shaders and the two shadow passes slowly
    await page.waitForFunction(function () { return document.body.dataset.gl === 'off' || document.body.dataset.gl === 'on' && +document.getElementById('splash').dataset.frames >= 1; }, null, { timeout: 240000 });
    assert.strictEqual(await page.evaluate(function () { return document.body.dataset.gl + ' ' + (document.body.dataset.why || ''); }), 'on ', 'the exported splash draws in WebGL, three.js loaded from beside it');
    var three = requests.filter(function (u) { return /three\.module\.min\.js/.test(u); });
    assert.ok(three.length >= 1 && three.every(function (u) { return u.indexOf(url + '/landing/three.module.min.js?v=') === 0; }), 'three.js comes from beside the page: ' + JSON.stringify(three));
    var away = requests.filter(function (u) { return u.indexOf(url + '/landing/') !== 0 && !/^(data|blob):/.test(u) && u !== url + '/favicon.ico'; });
    assert.deepStrictEqual(away, [], 'the page requests nothing but its own directory');
    var links = await page.evaluate(function () { return [document.getElementById('splash-invite').href, document.getElementById('splash-signin').href]; });
    assert.deepStrictEqual(links, ['https://gardenio.clonk.tv/lands/', 'https://gardenio.clonk.tv/oauth2/start?rd=/garden'], 'the two links are on the app\'s origin');
    assert.deepStrictEqual(errors, [], 'no console errors');
    console.log('exported splash: gl on, three.js ' + three[0].slice(url.length) + ', ' + requests.length + ' requests');
  } finally {
    await browser.close();
  }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
