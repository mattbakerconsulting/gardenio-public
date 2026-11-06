/* The splash's shadows in a real browser (plan gd-EJGA3zDGCk step 7), run from TestSplashShadowsUnderChromium against
   /garden/splash on an httptest server. Usage: node splash_shadows_browser.js <base url> <node_modules dir>.

   The page pinned to 07:00 UTC on the 2026 March equinox at 45 N 0 E (?t=&lat=&lon=), at 480x300 so SwiftShader draws it in
   reasonable time: it draws in WebGL, its two shadow cascades and the patched light loop compiling (no console error), and
   window.__splash reads the stone's shadow from the drawn scene: cast by the sun, along the almanac's direction for that
   instant, its tip west of the stone. Relit at 17:00 by __splash.at(), drawn again, the tip is east of it. Zero console
   errors. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));
var AM = Date.UTC(2026, 2, 20, 7), PM = Date.UTC(2026, 2, 20, 17);

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  try {
    var errors = [], page = await browser.newPage({ viewport: { width: 480, height: 300 } });
    page.on('console', function (m) { if (m.type() === 'error') errors.push(m.text()); });
    page.on('pageerror', function (e) { errors.push(String(e.message || e)); });
    await page.goto(url + '/garden/splash?t=2026-03-20T07:00:00Z&lat=45&lon=0');
    // SwiftShader compiles the scene's shaders and the two shadow passes slowly
    await page.waitForFunction(function () { return document.body.dataset.gl === 'off' || document.body.dataset.gl === 'on' && +document.getElementById('splash').dataset.frames >= 1; }, null, { timeout: 240000 });
    assert.strictEqual(await page.evaluate(function () { return document.body.dataset.gl + ' ' + (document.body.dataset.why || ''); }), 'on ', 'the splash draws in WebGL');
    function read(t) {
      return page.evaluate(function (t) {
        var frames = +document.getElementById('splash').dataset.frames;
        if (t) window.__splash.at(t);
        var gl = window.SPLASH.view.renderer.getContext();
        gl.readPixels(0, 0, 1, 1, gl.RGBA, gl.UNSIGNED_BYTE, new Uint8Array(4)); // the frame drawn through
        var l = window.__splash.light(), want = window.ALMANAC.skyPalette(l.t, 45, 0).direction;
        return { light: l, want: want, shadow: window.__splash.shadow(), drew: +document.getElementById('splash').dataset.frames - frames };
      }, t || 0);
    }
    function along(r) { return Math.max.apply(null, r.want.map(function (w, i) { return Math.abs(w - r.light.direction[i]); })); }

    var am = await read();
    assert.ok(am.light.t === AM && am.light.source === 'sun' && am.light.strength > 2 && along(am) < 1e-9, 'at 07:00 the sun lights the scene along the almanac\'s direction: ' + JSON.stringify(am.light));
    assert.ok(am.shadow && am.shadow.source === 'sun' && am.shadow.west && am.shadow.bearing > 225 && am.shadow.bearing < 315, 'at 07:00 the stone\'s shadow ends west of it: ' + JSON.stringify(am.shadow));
    var pm = await read(PM);
    assert.ok(pm.drew === 1 && pm.light.t === PM && pm.light.source === 'sun' && along(pm) < 1e-9, 'relit and drawn at 17:00: ' + JSON.stringify(pm.light));
    assert.ok(pm.shadow && pm.shadow.east && pm.shadow.bearing > 45 && pm.shadow.bearing < 135, 'at 17:00 the stone\'s shadow ends east of it: ' + JSON.stringify(pm.shadow));
    assert.deepStrictEqual(errors, [], 'no console errors (a shader that fails to compile logs one)');
    console.log('shadow tip: 07:00 bearing ' + am.shadow.bearing.toFixed(1) + ' at ' + am.shadow.length.toFixed(1) + ' m (x ' + am.shadow.tip[0].toFixed(1) + ', the stone at ' + am.shadow.stone[0] +
      '); 17:00 bearing ' + pm.shadow.bearing.toFixed(1) + ' at ' + pm.shadow.length.toFixed(1) + ' m (x ' + pm.shadow.tip[0].toFixed(1) + ')');
  } finally {
    await browser.close();
  }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
