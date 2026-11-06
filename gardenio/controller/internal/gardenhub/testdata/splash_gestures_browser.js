/* The splash's pointer gestures in a real browser (plan gd-I5QHVfGLbD step 3, gd-lKZIU12lAT), run from
   TestSplashGesturesUnderChromium against /garden/splash on an httptest server. Usage: node splash_gestures_browser.js <base
   url> <node_modules dir>.

   The page pinned to noon UTC on 26 Oct 2026 at 45 N 0 E, at 360x240 so SwiftShader draws it quickly, under reduced motion
   so every change of the view lands at once, with a touchscreen. The canvas is touch-action none, and the menu's own box
   lets the pointer through to it. A wheel of -300 at the open canvas nearest the stone zooms in by VIEW.wheel, the
   direction under the pointer held, and writes ?z= once the burst is quiet; a ctrl wheel (a trackpad's pinch) zooms harder.
   A mouse drag of 60 px right turns the yaw negative by the fov over the height a pixel, and the address carries look=. Two
   fingers moved apart (Input.dispatchTouchEvent, so Chromium makes the pointer events) zoom by their distance. A double tap
   (two Input.dispatchTouchEvent taps stamped 100 ms apart) zooms 2x; a mouse's double click at ZOOM_MAX goes back to the default. The address, opened in a second
   page, draws the same pixels. Zero console errors. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  try {
    var errors = [], ctx = await browser.newContext({ viewport: { width: 360, height: 240 }, timezoneId: 'UTC', locale: 'en-US', reducedMotion: 'reduce', hasTouch: true });
    async function open(path) {
      var page = await ctx.newPage();
      page.on('console', function (m) { if (m.type() === 'error') errors.push(m.text()); });
      page.on('pageerror', function (e) { errors.push(String(e.message || e)); });
      await page.goto(url + path);
      // SwiftShader compiles the scene's shaders and the two shadow passes slowly
      await page.waitForFunction(function () { return document.body.dataset.gl === 'off' || document.body.dataset.gl === 'on' && +document.getElementById('splash').dataset.frames >= 1; }, null, { timeout: 240000 });
      assert.strictEqual(await page.evaluate(function () { return document.body.dataset.gl + ' ' + (document.body.dataset.why || ''); }), 'on ', 'the splash draws in WebGL');
      return page;
    }
    // state is the probe's view and the address
    function state(page) { return page.evaluate(function () { var s = window.__splash.state(); s.search = location.search; return s; }); }
    // settle looks at view s and waits for its frame; reset settles on the default view with the address as it opened
    function frames(page) { return page.evaluate(function () { return +document.getElementById('splash').dataset.frames; }); }
    function drawn(page, f) { return page.waitForFunction(function (f) { return +document.getElementById('splash').dataset.frames > f; }, f, { timeout: 120000, polling: 100 }); }
    async function settle(page, s) {
      var f = await frames(page);
      await page.evaluate(function (s) { window.__splash.look(s); }, s);
      await drawn(page, f);
    }
    async function reset(page) {
      await settle(page, {});
      await page.evaluate(function () { history.replaceState(null, '', location.pathname + '?t=2026-10-26T12:00:00Z&lat=45&lon=0'); });
    }
    // ray is the drawn camera's direction through CSS px (x, y) and where the stone stands on screen
    function ray(page, x, y) {
      return page.evaluate(function (p) {
        var v = window.SPLASH.view, cv = document.getElementById('splash'), c = v.camera, o = v.scene.getObjectByName('stone');
        var d = c.position.clone().set(2 * p[0] / cv.clientWidth - 1, 1 - 2 * p[1] / cv.clientHeight, 0.5).unproject(c).sub(c.position).normalize();
        var s = o.getWorldPosition(o.position.clone()).project(c);
        return { d: [d.x, d.y, d.z], stone: [(s.x + 1) / 2 * cv.clientWidth, (1 - s.y) / 2 * cv.clientHeight] };
      }, [x, y]);
    }
    // pixels draws the scene now and reads the canvas in the same task, before the browser clears it
    function pixels(page) {
      return page.evaluate(function () {
        var cv = document.getElementById('splash');
        window.SPLASH.view.draw();
        return { url: cv.toDataURL('image/png'), w: cv.width, h: cv.height };
      });
    }
    function near(a, b, eps) { return Math.abs(a - b) <= eps; }

    var began = Date.now(), took = {};
    function lap(k) { took[k] = Math.round((Date.now() - began) / 1000); }
    var page = await open('/garden/splash?t=2026-10-26T12:00:00Z&lat=45&lon=0');
    var css = await page.evaluate(function () { return getComputedStyle(document.getElementById('splash')).touchAction; });
    assert.strictEqual(css, 'none', 'the canvas takes its own touches');
    // the open canvas nearest the stone: at 360x240 the start menu's links lie over most of the frame
    var r0 = await ray(page, 0, 0), at = await page.evaluate(function (st) {
      var cv = document.getElementById('splash'), best = null;
      for (var y = 4; y < cv.clientHeight - 4; y += 4) for (var x = 4; x < cv.clientWidth - 4; x += 4) {
        var d = Math.hypot(x - st[0], y - st[1]);
        if (document.elementFromPoint(x, y) === cv && (!best || d < best.d)) best = { x: x, y: y, d: d };
      }
      return best;
    }, r0.stone);
    assert.ok(at && at.d < 60, 'there is open canvas near the stone at ' + JSON.stringify(r0.stone) + ': ' + JSON.stringify(at));
    var open0 = await page.evaluate(function () { return document.elementFromPoint(180, 40) === document.getElementById('splash'); });
    assert.ok(open0, 'the menu\'s title lets the pointer through to the canvas');

    // the wheel: -300 px zooms in by exp(0.6) toward the pointer, the direction under it held, then writes ?z= once quiet
    var r1 = await ray(page, at.x, at.y), f = await frames(page);
    await page.mouse.move(at.x, at.y);
    await page.mouse.wheel(0, -300);
    await page.waitForFunction(function () { return /[?&]z=/.test(location.search); }, null, { timeout: 60000, polling: 100 });
    await drawn(page, f);
    var s = await state(page), r2 = await ray(page, at.x, at.y), moved = Math.acos(Math.min(1, r1.d[0] * r2.d[0] + r1.d[1] * r2.d[1] + r1.d[2] * r2.d[2]));
    assert.ok(near(s.zoom, Math.exp(0.6), 1e-6) && moved < 2e-3, 'a wheel of -300 zooms in by exp(0.6) toward the pointer: ' + JSON.stringify(s) + ', moved ' + moved + ' rad');
    var slid = Math.hypot(r2.stone[0] - r1.stone[0], r2.stone[1] - r1.stone[1]);
    assert.ok(slid > 5, 'the frame drawn is the zoomed one: the stone, off the pointer, slid ' + slid + ' px');
    assert.ok(s.search.indexOf('z=1.82') > 0 && /look=/.test(s.search), 'the wheel writes the zoom and the look into the address: ' + s.search);
    var z0 = s.zoom;
    await page.keyboard.down('Control');
    await page.mouse.wheel(0, -30);
    await page.keyboard.up('Control');
    s = await state(page);
    assert.ok(near(s.zoom / z0, Math.exp(0.3), 1e-6), 'a ctrl wheel (a trackpad\'s pinch) zooms five times as hard: ' + JSON.stringify(s));
    lap('wheel');

    // a mouse drag of 60 px right turns the view left by the fov (50) over the height (240) a pixel, and the address says so
    await reset(page);
    await page.mouse.move(100, 12);
    await page.mouse.down();
    await page.mouse.move(130, 12);
    await page.mouse.move(160, 12);
    await page.mouse.up();
    s = await state(page);
    assert.ok(near(s.yaw, -60 * 50 / 240, 1e-6) && s.zoom === 1 && s.pitch === 0 && s.search.indexOf('look=-12.5,0') > 0 && !/[?&]z=/.test(s.search),
      'a drag of 60 px right turns the yaw 12.5 degrees left and writes look=: ' + JSON.stringify(s));
    lap('drag');

    // two fingers moved apart from 60 px to 140 px zoom by 140 / 60 toward their midpoint
    await reset(page);
    var cdp = await ctx.newCDPSession(page);
    function touch(type, d) { return cdp.send('Input.dispatchTouchEvent', { type: type, touchPoints: type === 'touchEnd' ? [] : [{ x: 180 - d, y: 12, id: 1 }, { x: 180 + d, y: 12, id: 2 }] }); }
    // twice taps (x, y) twice with touches stamped 100 ms apart: the page reads the gap from the events' timeStamp, which
    // would otherwise be the round trips' wall clock, and SwiftShader's seconds-long frames can stretch that past TAP_MS
    async function twice(x, y) {
      var t = Date.now() / 1000;
      for (var i = 0; i < 2; i++, t += 0.1) {
        await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x: x, y: y }], timestamp: t });
        await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [], timestamp: t + 0.02 });
      }
    }
    await touch('touchStart', 30);
    await touch('touchMove', 50);
    await touch('touchMove', 70);
    await touch('touchEnd');
    s = await state(page);
    assert.ok(near(s.zoom, 140 / 60, 1e-6) && /[?&]z=2.33/.test(s.search), 'a pinch from 60 px to 140 px zooms 2.33x: ' + JSON.stringify(s));
    lap('pinch');

    // a double tap at the stone zooms 2x toward it; a mouse's double click at the most zoom goes back to the default view
    await reset(page);
    await twice(at.x, at.y);
    s = await state(page);
    assert.ok(near(s.zoom, 2, 1e-9) && /[?&]z=2(&|$)/.test(s.search), 'a double tap zooms 2x: ' + JSON.stringify(s));
    var tapped = s;
    await settle(page, { zoom: 8, yaw: 5, pitch: 3 });
    await page.mouse.dblclick(at.x, at.y);
    s = await state(page);
    assert.ok(s.zoom === 1 && s.yaw === 0 && s.pitch === 0, 'a double click at the most zoom goes back to the default view: ' + JSON.stringify(s));
    lap('taps');

    // the address is a link to the frame: a second page opened on the double tap's draws the same picture
    await settle(page, { zoom: 2 });
    await twice(at.x, at.y);
    s = await state(page);
    assert.ok(near(s.zoom, 4, 1e-9), 'a second double tap zooms 2x again: ' + JSON.stringify(s));
    // the address holds the view to two decimals: the first page stands where its address says, then both are drawn
    var link = page.url(), back = await page.evaluate(function () { return window.__splash.look(window.VIEW.parse(location.search)); });
    assert.ok(near(back.zoom, s.zoom, 0.006) && near(back.yaw, s.yaw, 0.006) && near(back.pitch, s.pitch, 0.006), 'the address carries the view: ' + link);
    var a = await pixels(page), other = await open(link.slice(url.length)), o = await state(other), b = await pixels(other);
    assert.deepStrictEqual([o.zoom, o.yaw, o.pitch], [back.zoom, back.yaw, back.pitch], 'the shared link opens at the same view');
    var diff = await other.evaluate(async function (urls) {
      function load(u) { return new Promise(function (ok, no) { var im = new Image(); im.onload = function () { ok(im); }; im.onerror = no; im.src = u; }); }
      var ims = await Promise.all(urls.map(load)), data = ims.map(function (im) {
        var c = document.createElement('canvas'); c.width = im.width; c.height = im.height;
        var g = c.getContext('2d'); g.drawImage(im, 0, 0); return g.getImageData(0, 0, im.width, im.height).data;
      });
      var n = 0, most = 0;
      for (var i = 0; i < data[0].length; i += 4) {
        var d = Math.max(Math.abs(data[0][i] - data[1][i]), Math.abs(data[0][i + 1] - data[1][i + 1]), Math.abs(data[0][i + 2] - data[1][i + 2]));
        if (d > 8) n++;
        most = Math.max(most, d);
      }
      return { pixels: data[0].length / 4, off: n, most: most };
    }, [a.url, b.url]);
    assert.ok(a.w === b.w && a.h === b.h && diff.off <= diff.pixels * 0.002, 'the shared link draws the same frame: ' + JSON.stringify(diff));
    await other.close();
    lap('link');

    assert.deepStrictEqual(errors, [], 'no console errors');
    console.log('gestures: wheel to ' + z0.toFixed(3) + 'x (direction moved ' + moved.toExponential(1) + ' rad), drag yaw -12.5, pinch 2.33x, double tap ' + tapped.zoom + 'x, link ' + link.slice(url.length) + ' ' + diff.off + ' of ' + diff.pixels + ' px off (most ' + diff.most + '); seconds in ' + JSON.stringify(took));
  } finally {
    await browser.close();
  }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
