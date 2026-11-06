/* The splash's time controls in a real browser (plan gd-EJGA3zDGCk step 8), run from TestSplashControlsUnderChromium against
   /garden/splash on an httptest server. Usage: node splash_controls_browser.js <base url> <node_modules dir>.

   The page pinned to noon UTC on 26 Oct 2026 at 45 N 0 E, in UTC, at 360x240 so SwiftShader draws it quickly,
   under reduced motion so nothing animates unasked. The controls show once the scene draws, and the readout says the time,
   the date, the twilight, the sun's height and the moon as the almanac has them. Tab reaches the start menu's links, then the
   view's Zoom in and Zoom out (Reset view hidden at the default view), Play, Now and the scrubber;
   on the scrubber ArrowRight moves the scene 10 minutes on, Shift+ArrowRight an hour, ArrowLeft 10 minutes back, each
   relighting the scene along the almanac's direction, updating the readout and writing ?t= into the address. That
   address, opened in a second page, draws the same frame (the canvases compared pixel by pixel) with the same readout. A
   click on the scrubber's track at a quarter of the way pins the scene near 06:00 and writes ?t= on release. Play (Enter)
   runs the day, a day a minute, and says so (aria-pressed, ?play=1); Play again (Space) pauses and writes ?t= back. Now
   follows the browser's clock again and drops ?t= and ?play=. The view's buttons (plan gd-I5QHVfGLbD, gd-Xs9LA8ONjn): Zoom in
   zooms 1.5x, shows Reset view and the readout's zoom line and writes ?z=, the column clear of the readout (unwrapped at noon)
   and of Play and Now at 360 px, short readout or long; Zoom out goes back to zoom 1 and Reset view hides again; ArrowLeft on a view button turns the view left and
   writes look=; Escape resets it, and Reset view resets and hands the focus to Zoom in. The canvas takes no focus. Zero
   console errors. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));
var NOON = Date.UTC(2026, 9, 26, 12), MIN = 60000, DAY = 86400000;

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  try {
    var errors = [], ctx = await browser.newContext({ viewport: { width: 360, height: 240 }, timezoneId: 'UTC', locale: 'en-US', reducedMotion: 'reduce' });
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
    // state reads the controls and the scene: the readout's two lines, the scrubber, Play, the address, the scene's clock and
    // light, and what the almanac says for that instant
    function state(page) {
      return page.evaluate(function () {
        var v = window.SPLASH.view, l = window.__splash.light(), p = window.ALMANAC.skyPalette(v.t, 45, 0), s = document.getElementById('splash-scrub'), b = document.getElementById('splash-play');
        return {
          shown: !document.getElementById('splash-time').hidden, when: document.querySelector('#splash-readout .when').textContent, sky: document.querySelector('#splash-readout .sky').textContent,
          scrub: +s.value, valuetext: s.getAttribute('aria-valuetext'), play: b.getAttribute('aria-pressed') + ' ' + b.textContent, search: location.search,
          t: v.t, rate: v.rate, now: Date.now(), frames: +document.getElementById('splash').dataset.frames, along: Math.max.apply(null, p.direction.map(function (d, i) { return Math.abs(d - l.direction[i]); })),
          want: { el: Math.round(p.sun.elevation), twilight: p.twilight, phase: p.moon.phase, lit: Math.round(p.moon.fraction * 100) }
        };
      });
    }
    function iso(t) { return new Date(t).toISOString().replace('.000Z', 'Z'); }
    function said(s) {
      var tw = { day: 'day', golden: 'golden hour', civil: 'civil twilight', nautical: 'nautical twilight', astronomical: 'astronomical twilight', night: 'night' }[s.want.twilight];
      var el = 'sun ' + (s.want.el < 0 ? '−' : '') + Math.abs(s.want.el) + '°';
      return tw + ' · ' + el + ' · ' + s.want.phase + ' moon ' + s.want.lit + '%';
    }
    // pixels draws the scene now and reads the canvas in the same task, before the browser clears it
    function pixels(page) {
      return page.evaluate(function () {
        var cv = document.getElementById('splash');
        window.SPLASH.view.draw();
        return { url: cv.toDataURL('image/png'), w: cv.width, h: cv.height };
      });
    }

    var began = Date.now(), took = {};
    function lap(k) { took[k] = Math.round((Date.now() - began) / 1000); }
    var page = await open('/garden/splash?t=2026-10-26T12:00:00Z&lat=45&lon=0'), s = await state(page);
    assert.ok(s.shown && s.t === NOON && s.scrub === 720 && s.valuetext === '12:00' && s.play === 'false Play', 'the controls show at the pinned noon: ' + JSON.stringify(s));
    assert.strictEqual(s.when, '12:00 · 26 Oct', 'the readout says the local time and the date');
    assert.strictEqual(s.sky, said(s), 'the readout says the twilight, the sun\'s height and the moon as the almanac has them');

    lap('open');
    // the keyboard: Tab reaches the start menu's links first (gd-Fw6fffLZ3p, gd-QDdwxnGdTY), then the scrubber, whose arrows move it by 10 minutes, Shift by an hour
    var order = [];
    for (var i = 0; i < 10 && order[order.length - 1] !== 'splash-scrub'; i++) {
      await page.keyboard.press('Tab');
      order.push(await page.evaluate(function () { return document.activeElement && document.activeElement.id; }));
    }
    assert.deepStrictEqual(order, ['splash-invite', 'splash-signin', 'splash-in', 'splash-out', 'splash-play', 'splash-now', 'splash-scrub'], 'Tab reaches the menu, the view\'s buttons, then Play, Now and the scrubber in turn');
    var f0 = s.frames;
    await page.keyboard.press('ArrowRight');
    s = await state(page);
    assert.ok(s.t === NOON + 10 * MIN && s.scrub === 730 && s.when === '12:10 · 26 Oct' && s.search === '?t=2026-10-26T12:10:00Z&lat=45&lon=0', 'ArrowRight: 10 minutes on, in the readout and the address: ' + JSON.stringify(s));
    await page.keyboard.press('Shift+ArrowRight');
    s = await state(page);
    assert.ok(s.t === NOON + 70 * MIN && s.scrub === 790 && s.when === '13:10 · 26 Oct' && s.search.indexOf('t=2026-10-26T13:10:00Z') === 1, 'Shift+ArrowRight: an hour on: ' + JSON.stringify(s));
    await page.keyboard.press('ArrowLeft');
    s = await state(page);
    assert.ok(s.t === NOON + 60 * MIN && s.when === '13:00 · 26 Oct' && s.sky === said(s) && s.along < 1e-9 && s.search === '?t=2026-10-26T13:00:00Z&lat=45&lon=0',
      'ArrowLeft: 10 minutes back, the scene lit along the almanac\'s direction at 13:00, the readout following: ' + JSON.stringify(s));
    await page.waitForFunction(function (f0) { return +document.getElementById('splash').dataset.frames > f0; }, f0, { timeout: 120000, polling: 250 });

    lap('keys');
    // the address is a link to the frame: a second page opened on it draws the same picture with the same readout
    var link = page.url(), a = await pixels(page), other = await open(link.slice(url.length)), o = await state(other), b = await pixels(other);
    assert.ok(o.t === s.t && o.when === s.when && o.sky === s.sky && o.scrub === s.scrub, 'the shared link pins the same instant: ' + JSON.stringify(o));
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
      return { size: ims[0].width + 'x' + ims[0].height + ' / ' + ims[1].width + 'x' + ims[1].height, pixels: data[0].length / 4, off: n, most: most };
    }, [a.url, b.url]);
    assert.ok(a.w === b.w && a.h === b.h && diff.off <= diff.pixels * 0.002, 'the shared link draws the same frame: ' + JSON.stringify(diff));
    await other.close();
    lap('link');

    // the pointer: a click a quarter of the way along the track pins the scene near 06:00 and writes ?t= on release
    var box = await page.locator('#splash-scrub').boundingBox(), thumb = 12;
    await page.mouse.click(box.x + thumb / 2 + (box.width - thumb) * 360 / 1439, box.y + box.height / 2);
    s = await state(page);
    assert.ok(Math.abs(s.scrub - 360) <= 3 && s.t === Date.UTC(2026, 9, 26) + s.scrub * MIN && s.sky === said(s) && s.search.indexOf('?t=' + iso(s.t) + '&') === 0, 'a click on the track pins the scene there: ' + JSON.stringify(s));
    var clicked = s.when.slice(0, 5);

    // Play runs the day a day a minute, and says so; Play again pauses it and writes ?t= back
    await page.focus('#splash-play');
    await page.keyboard.press('Enter');
    s = await state(page);
    var t0 = s.t;
    assert.ok(s.rate === 1440 && s.play === 'true Pause' && /[?&]play=1(&|$)/.test(s.search), 'Enter on Play plays: ' + JSON.stringify(s));
    // the played clock moves only on an animation frame, and a SwiftShader frame can outlast the two key presses: wait for
    // the day to get a whole second on (the pause stands on the whole second) before pausing it (gd-bNZrDyCr1m)
    await page.waitForFunction(function (a) { return ((window.SPLASH.view.t - a[0]) % a[1] + a[1]) % a[1] >= 1000; }, [t0, DAY], { timeout: 120000, polling: 250 });
    await page.keyboard.press('Space');
    s = await state(page);
    // the day goes round: how far it got is counted round the clock, as SwiftShader's slow frames hold the key a while
    var got = ((s.t - t0) % DAY + DAY) % DAY;
    assert.ok(s.rate === 0 && s.play === 'false Play' && got > 0 && s.search.indexOf('?t=' + iso(s.t) + '&') === 0 && !/play=/.test(s.search), 'Space on Play pauses where the day had got to: ' + JSON.stringify(s));
    var played = Math.round(got / MIN);
    lap('play');

    // Now follows the browser's clock again
    await page.focus('#splash-now');
    await page.keyboard.press('Enter');
    s = await state(page);
    // the clock moves its light once a second, between SwiftShader's slow frames: it is no later than the browser's and
    // runs on with it
    var when = new Date(s.t).toISOString().slice(11, 16);
    await page.waitForFunction(function (t) { return window.SPLASH.view.t > t; }, s.t, { timeout: 120000, polling: 250 });
    assert.ok(s.rate === 1 && s.now - s.t >= 0 && s.now - s.t < 60000 && s.search === '?lat=45&lon=0' && s.when.indexOf(when) === 0, 'Now follows the clock and drops ?t=: ' + JSON.stringify(s));

    lap('now');

    // the view's buttons, under reduced motion so each change lands at once
    function seen(page) {
      return page.evaluate(function () {
        var z = document.querySelector('#splash-readout .zoom'), cv = document.getElementById('splash');
        function box(id) { var r = document.getElementById(id).getBoundingClientRect(); return [r.left, r.top, r.right, r.bottom]; }
        return { view: window.__splash.state(), shown: !document.getElementById('splash-view').hidden, reset: !document.getElementById('splash-reset').hidden,
          zoom: z.hidden ? null : z.textContent, search: location.search, focus: document.activeElement && document.activeElement.id, tabindex: cv.getAttribute('tabindex'),
          boxes: { view: box('splash-view'), readout: box('splash-readout'), play: box('splash-play'), now: box('splash-now') }, lines: Object.keys({ when: 1, sky: 1, zoom: 1 }).map(function (k) {
            var e = document.querySelector('#splash-readout .' + k); return Math.round(e.getBoundingClientRect().height / parseFloat(getComputedStyle(e).lineHeight));
          }) };
      });
    }
    function apart(a, b) { return a[2] <= b[0] || b[2] <= a[0] || a[3] <= b[1] || b[3] <= a[1]; }
    var v = await seen(page);
    assert.ok(v.shown && !v.reset && v.zoom === null && v.view.zoom === 1 && v.tabindex === null, 'the view\'s buttons show at the default view, Reset view hidden, the canvas taking no focus: ' + JSON.stringify(v));
    await page.focus('#splash-in');
    await page.keyboard.press('Enter');
    v = await seen(page);
    assert.ok(v.view.zoom === 1.5 && v.reset && v.zoom === 'zoom 1.5×' && /[?&]z=1\.5(&|$)/.test(v.search) && !/look=/.test(v.search), 'Zoom in: 1.5x, Reset view and the zoom line show, ?z= written: ' + JSON.stringify(v));
    // at 360 px the column stands clear of the readout and of Play and Now: at noon beside them, the readout's three lines
    // unwrapped, and in astronomical twilight, whose long readout Play and Now go under
    for (var at of [NOON, NOON - 7 * 60 * MIN]) {
      await page.evaluate(function (t) { window.SPLASH.view.set(t, 0); }, at);
      v = await seen(page);
      assert.ok(apart(v.boxes.view, v.boxes.readout) && apart(v.boxes.view, v.boxes.play) && apart(v.boxes.view, v.boxes.now) && v.boxes.view[2] <= 360 && (at !== NOON || v.lines.join() === '1,1,1'),
        'at 360 px the view\'s column stands clear of the readout and of Play and Now: ' + JSON.stringify(v));
    }
    await page.focus('#splash-out');
    await page.keyboard.press('Enter');
    v = await seen(page);
    assert.ok(Math.abs(v.view.zoom - 1) < 1e-9 && !v.reset && v.zoom === null && !/[?&](z|look)=/.test(v.search), 'Zoom out: back to zoom 1, Reset view hidden again, nothing in the address: ' + JSON.stringify(v));
    await page.keyboard.press('ArrowLeft');
    v = await seen(page);
    var yaw = v.view.yaw;
    assert.ok(yaw < -1 && v.reset && /[?&]look=-[0-9.]+,/.test(v.search) && v.focus === 'splash-out', 'ArrowLeft on a view button turns the view left and writes look=: ' + JSON.stringify(v));
    await page.keyboard.press('Shift+ArrowLeft');
    v = await seen(page);
    assert.ok(Math.abs(v.view.yaw - yaw * (1 + 10 / 3)) < 1e-6, 'Shift+ArrowLeft turns it a third of the frame, a tenth without: ' + JSON.stringify(v));
    await page.keyboard.press('Escape');
    v = await seen(page);
    assert.ok(v.view.zoom === 1 && v.view.yaw === 0 && v.view.pitch === 0 && !v.reset && !/look=/.test(v.search), 'Escape resets the view: ' + JSON.stringify(v));
    await page.keyboard.press('ArrowUp');
    await page.focus('#splash-reset');
    await page.keyboard.press('Enter');
    v = await seen(page);
    assert.ok(v.view.pitch === 0 && !v.reset && v.focus === 'splash-in' && !/look=/.test(v.search), 'Reset view resets and hands the focus to Zoom in: ' + JSON.stringify(v));
    lap('view');

    assert.deepStrictEqual(errors, [], 'no console errors');
    console.log('controls: keyboard 12:00 -> 13:00, shared link ' + diff.off + ' of ' + diff.pixels + ' px off (most ' + diff.most + '), click at ' + clicked + ', played ' + played + ' min, view turned ' + yaw.toFixed(2) + ' deg a key; seconds in ' + JSON.stringify(took));
  } finally {
    await browser.close();
  }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
