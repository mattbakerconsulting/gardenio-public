/* Browser check that a phone's pinches and toolbar never make /garden look reloaded (gd-KZDKktrdQC), run from
   TestPhonePinchUnderChromium against a live /garden on an httptest server with the fixture farm.
   Usage: node phone_pinch_browser.js <base url> <node_modules dir>.

   Phone: 393x852 at DPR 3, touch on, motion reduced; each pinch is a CDP Input.dispatchTouchEvent sequence, which goes
   through the browser's own gesture handling (Playwright has no pinch).
     1. A pinch whose fingers land on the bar along the top or on a tab bar button is no page zoom: the visual
        viewport's scale stays 1. The map is the thing that zooms (its own pinch, touch-action:none on the board);
        the page zoomed 5x around the finger and the browser redrew all of it on release, which read as a refresh.
     2. The toolbar sliding away and back resizes the board each frame: the map is painted in the frame it is resized in,
        never left blank until the next one, and the cursor and its framing stay.
     3. None of it loads the page again: the boot count the init script keeps in sessionStorage stays 1. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));

function state(page) {
  return page.evaluate(function () {
    var cur = document.querySelector('#hots .hot.cur'), vv = window.visualViewport;
    return { boots: +sessionStorage.getItem('pinch.boots'), scale: vv ? Math.round(vv.scale * 100) / 100 : 1, grid: document.getElementById('hots').getAttribute('aria-label'),
      cur: cur ? cur.getAttribute('aria-label') : '', z: Math.round(window.GARDEN.cam().z * 1000) / 1000 };
  });
}
/* Two fingers down 40 px apart about (x, y), spread to 220 px in a dozen moves, then lifted. */
async function pinch(page, cdp, x, y) {
  function at(d) { return [{ id: 1, x: Math.round(x - d / 2), y: Math.round(y) }, { id: 2, x: Math.round(x + d / 2), y: Math.round(y) }]; }
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [at(40)[0]] });
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: at(40) });
  for (var i = 1; i <= 12; i++) { await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: at(40 + 180 * i / 12) }); await page.waitForTimeout(16); }
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [at(220)[1]] });
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
  await page.waitForTimeout(400);
}
/* A point a finger on sel lands on: the first on a 12 px grid over its box where sel itself takes the touch (the farm's bar
   lets touches through to the map between its controls). */
function centre(page, sel) {
  return page.evaluate(function (s) {
    var e = document.querySelector(s), r = e && e.getBoundingClientRect();
    if (!r || !r.width || !r.height) return null;
    for (var y = r.top + 6; y < r.bottom; y += 12) for (var x = r.left + 6; x < r.right; x += 12) {
      var h = document.elementFromPoint(x, y);
      if (h && h.closest(s) && !h.closest('#board')) return { x: x, y: y, hit: h.tagName + (h.className ? '.' + String(h.className).split(' ')[0] : '') };
    }
    return null;
  }, sel);
}

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  try {
    var ctx = await browser.newContext({ viewport: { width: 393, height: 852 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true, reducedMotion: 'reduce' });
    await ctx.addInitScript(function () {
      try { localStorage.setItem('garden.sky', 'day'); sessionStorage.setItem('pinch.boots', (+sessionStorage.getItem('pinch.boots') || 0) + 1); } catch (e) { /* no storage: boots reads 0 and fails loudly */ }
    });
    var page = await ctx.newPage(), errs = [];
    page.on('pageerror', function (e) { errs.push(String(e)); });
    await page.goto(url + '/garden?at=gardenio/gd-planA');
    await page.waitForSelector('#hots .hot.cur', { state: 'attached' });
    await page.waitForTimeout(600);
    var cdp = await ctx.newCDPSession(page), start = await state(page);
    assert.strictEqual(start.boots, 1, 'one boot to start with');

    // 1. no page zoom from the chrome around the map
    for (var sel of ['.crumb', '#tabbar button']) {
      var c = await centre(page, sel);
      assert.ok(c, sel + ' is on the screen');
      await pinch(page, cdp, c.x, c.y);
      var s = await state(page);
      assert.strictEqual(s.scale, 1, 'a pinch that starts on ' + sel + ' zooms the page: visual viewport scale ' + s.scale);
      if (s.scale !== 1) await cdp.send('Emulation.setPageScaleFactor', { pageScaleFactor: 1 });
      console.log('pinch on ' + sel + ' (' + c.hit + '): page scale ' + s.scale);
    }

    // 2. the toolbar slides: every resize of the board is painted in its own frame
    await page.evaluate(function () {
      var cv = document.getElementById('cv'), x = cv.getContext('2d');
      window.__paints = [];
      // observed after the page's own observer, so this runs after its resize() in the same frame, before the paint
      new ResizeObserver(function () { window.__paints.push(cv.width + 'x' + cv.height + ':' + x.getImageData(cv.width >> 1, cv.height >> 1, 1, 1).data[3]); }).observe(document.getElementById('board'));
    });
    var before = await state(page);
    for (var h of [870, 890, 908, 890, 870, 852]) { await page.setViewportSize({ width: 393, height: h }); await page.waitForTimeout(120); }
    var paints = await page.evaluate(function () { return window.__paints; }), after = await state(page);
    console.log('toolbar resizes (canvas size:alpha at its centre): ' + paints.join(' '));
    assert.ok(paints.length >= 6, 'the board is resized with the viewport: ' + paints.join(' '));
    paints.forEach(function (p) { assert.ok(!/:0$/.test(p), 'the map is blank in the frame its board was resized in: ' + paints.join(' ')); });
    assert.deepStrictEqual([after.grid, after.cur, after.z], [before.grid, before.cur, before.z], 'the toolbar moves the cursor or its framing');

    // 3. never a reload
    assert.strictEqual((await state(page)).boots, 1, 'the page loaded again');
    assert.deepStrictEqual(errs, [], 'page errors');
    await browser.close();
  } catch (e) { console.error(e && e.stack || e); try { await browser.close(); } catch (_) { /* already gone */ } process.exit(1); }
})();
