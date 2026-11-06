/* Browser check that a two-finger pinch zooms the free camera about the fingers' midpoint on a phone, the cursor's depth
   following the zoom (cursor-grid spec revision 3, 4.3), and keeps working after a finger has been lost (gd-iYlxddKzuu), run
   from TestPinchZoomUnderChromium against a live /garden on an httptest server with the fixture farm.
   Usage: node touch_pinch_browser.js <base url> <node_modules dir>.

   Each pinch is a CDP Input.dispatchTouchEvent sequence (Playwright has no pinch). The cursor starts on plan A, among its
   project's plots, at the Project level. With motion reduced:
     1. a fourfold spread zooms 4x about the midpoint (the world point under it stays put), the camera tracking the fingers
        on the way, and across the Plan edge the cursor goes into the plot under the midpoint;
     2. a spread whose fingers land on two hot-links that are removed under them mid-gesture, the way syncHots() removes a cell
        that leaves the grid, still goes in, and so does the next -- the board forgets a finger only when its pointerup reaches
        it, and a browser may deliver a lost finger's events to the detached node, where they bubble nowhere;
     3. a spread after two ghost pointers that never came up, which used to make the gesture count the wrong fingers;
     4. a 20% spread zooms 1.2x and stays there, crossing no level: the same cursor and grid;
     5. a pinch in across the World edge comes out to the farm.
   With motion on: 6. a 20% spread stays where the fingers left it; 7. a fourfold spread goes in.
   Phone: iPhone 12 (390x844), touch on. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));
/* Every context pins the sky (web/sky.js, gd-SwRqFcjWc6) to its emulated colour scheme: light is day and dark is night
   whatever the clock says, so a run at night (CI is UTC) sees the page a run by day does. */
function skyCtx(browser, opts) {
  opts = opts || {};
  return browser.newContext(opts).then(function (ctx) {
    return ctx.addInitScript(function (s) { try { localStorage.setItem('garden.sky', s); } catch (e) { /* no storage: the emulated scheme still decides by day */ } }, opts.colorScheme === 'dark' ? 'night' : 'day').then(function () { return ctx; });
  });
}

/* Where the cursor is and what frames it: the grid's name, the cursor's name, the camera's transform, the signs shown. */
function state(page) {
  return page.evaluate(function () {
    var hots = document.getElementById('hots'), cur = hots.querySelector('.hot.cur');
    return { grid: hots.getAttribute('aria-label'), name: cur ? cur.getAttribute('aria-label') : '', hots: hots.style.transform, cam: window.GARDEN.cam().cam, vp: window.GARDEN.cam().vp, lbls: Array.prototype.map.call(document.querySelectorAll('#lbls > *'), function (l) { return l.style.display === 'none' ? '' : l.textContent; }).filter(String).sort().join(' | ') };
  });
}
/* The state once ok(state) holds, polled until it does or 20 s have gone (a loaded runner's frames are slow); then the state as
   it is, for the caller's assertion to report; ms shortens the wait (a never-true ok is a read after ms, to see it hold). A
   gesture's outcome is waited for, never slept for (gd-bNZrDyCr1m). */
async function until(page, ok, ms) {
  for (var end = Date.now() + (ms || 20000); ;) {
    var s = await state(page);
    if (ok(s) || Date.now() > end) return s;
    await new Promise(function (r) { setTimeout(r, 25); });
  }
}
/* The world point under board point p (board px) at state s's camera. */
function under(s, p) { return { x: s.cam.cx + (p[0] - s.vp.w / 2) / s.cam.z, y: s.cam.cy + (p[1] - s.vp.h / 2) / s.cam.z }; }
async function boardPt(page, p) { var b = await page.locator('#board').boundingBox(); return [p[0] - b.x, p[1] - b.y]; }
function scale(t) { var m = /scale\(([\d.e-]+)\)/.exec(t || ''); return m ? +m[1] : NaN; }

/* Two fingers down at a and b, spread apart about their midpoint by factor f in a dozen moves (gap ms apart), then lifted.
   hook() runs between the fingers landing and the first move, mid() after the sixth move. */
async function pinch(page, cdp, a, b, f, hook, gap, mid) {
  var m = [(a[0] + b[0]) / 2, (a[1] + b[1]) / 2];
  function at(p, k) { return { x: Math.round(m[0] + (p[0] - m[0]) * k), y: Math.round(m[1] + (p[1] - m[1]) * k) }; }
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [Object.assign({ id: 1 }, at(a, 1))] });
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [Object.assign({ id: 1 }, at(a, 1)), Object.assign({ id: 2 }, at(b, 1))] });
  if (hook) await hook();
  for (var i = 1; i <= 12; i++) {
    var k = 1 + (f - 1) * i / 12;
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [Object.assign({ id: 1 }, at(a, k)), Object.assign({ id: 2 }, at(b, k))] });
    if (gap) await page.waitForTimeout(gap);
    if (i === 6 && mid) await mid();
  }
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [Object.assign({ id: 2 }, at(b, f))] });
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
}

/* A fourfold spread from the project's plots (the Project level) crosses the Plan edge: into a plot's phases. */
async function pinchIn(page, cdp, name, a, b, hook) {
  var before = await state(page);
  await pinch(page, cdp, a, b, 4, hook);
  var after = await until(page, function (s) { return s.grid !== before.grid; });
  assert.ok(/^gardenio: /.test(before.grid) && / phases?, \d+ by \d+$/.test(after.grid), name + ': the spread goes into a plot: ' + JSON.stringify([before.grid, after.grid]));
  console.log(name + ': ' + before.grid + ' -> ' + after.grid);
  return after;
}

/* Hot-links a finger can land on, clear of the strips at the top and bottom of the map, planted plots first. */
async function reachableHots(page) {
  return page.evaluate(function () {
    var b = document.getElementById('board').getBoundingClientRect(), out = [];
    Array.prototype.forEach.call(document.querySelectorAll('#hots a'), function (a) {
      var r = a.getBoundingClientRect(), x = r.left + r.width / 2, y = r.top + r.height / 2;
      if (r.width < 8 || r.height < 8 || x < b.left + 40 || x > b.right - 40 || y < b.top + 120 || y > b.bottom - 160) return;
      var hit = document.elementFromPoint(x, y);
      if (hit && hit.closest('#hots a') === a) out.push({ x: x, y: y, id: a.textContent.trim().slice(0, 30), name: a.getAttribute('aria-label'), plot: a.classList.contains('w-plot') && !a.classList.contains('w-staked') });   // a staked plot has no planted phases to go into
    });
    return out.sort(function (p, q) { return q.plot - p.plot; });
  });
}

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  try {
    var errors = [];
    async function phone(reduce) {
      var ctx = await skyCtx(browser, Object.assign({}, pw.devices['iPhone 12'], { viewport: { width: 390, height: 844 }, reducedMotion: reduce ? 'reduce' : 'no-preference' }));
      var page = await ctx.newPage();
      page.on('console', function (m) { if (m.type() === 'error') errors.push(m.text()); });
      page.on('pageerror', function (e) { errors.push(String(e)); });
      return { ctx: ctx, page: page };
    }
    /* The cursor on plan A, framed, with its signs placed: they are placed a beat after the framing (a setTimeout(0) task), so a
       state read before them differs from one read after any gesture by the signs alone (gd-bNZrDyCr1m). Then a frame with the
       camera still. */
    async function open(ph) {
      await ph.page.goto(url + '/garden?at=gardenio/gd-planA');   // the cursor on plan A: its project's plots are the grid's links
      await ph.page.waitForFunction(function () { return document.querySelectorAll('#hots a').length > 1 && /scale/.test(document.getElementById('hots').style.transform) && document.querySelector('#cursor-bar .cb-me'); });
      await ph.page.evaluate(function () { return new Promise(function (r) { setTimeout(r, 0); }); });
      await ph.page.waitForFunction(function () { var t = document.getElementById('hots').style.transform, still = t === window.__pinchT; window.__pinchT = t; return still; }, null, { polling: 'raf' });
      return ph.ctx.newCDPSession(ph.page);
    }

    var ph = await phone(true), page = ph.page, cdp = await open(ph);
    /* not a staked plot: its grid is named for its short title (world.js), its link for its whole title */
    var hots = (await reachableHots(page)).filter(function (h) { return !/: Staked out/.test(h.name); });
    assert.ok(hots.length, 'a plot a finger can reach: ' + JSON.stringify(hots));
    var c = [hots[0].x, hots[0].y], A = [c[0] - 20, c[1] - 14], B = [c[0] + 20, c[1] + 14];   // 48 px apart about a plot's middle

    // 1. A full spread goes into the plot under the midpoint, and the camera tracks the fingers on the way.
    var s0 = await state(page), mid = null;
    await pinch(page, cdp, A, B, 4, null, 0, async function () { mid = await state(page); });
    var s = await until(page, function (s) { return s.grid !== s0.grid; });
    assert.strictEqual(s.grid.indexOf(hots[0].name.split(':')[0] + ':'), 0, 'the spread goes into the plot under the fingers: ' + s.grid + ' / ' + hots[0].name);
    assert.ok(scale(mid.hots) > scale(s0.hots) && scale(mid.hots) < scale(s.hots), 'mid-gesture the camera is between: ' + [s0.hots, mid.hots, s.hots].join(' | '));
    s = await until(page, function (x) { return Math.abs(x.cam.z / s0.cam.z - 4) < 0.2; });
    assert.ok(Math.abs(s.cam.z / s0.cam.z - 4) < 0.2, 'a fourfold spread zooms 4x: ' + [s0.cam.z, s.cam.z]);
    var m = await boardPt(page, c), w0 = under(s0, m), w1 = under(s, m);
    assert.ok(Math.hypot(w1.x - w0.x, w1.y - w0.y) * s.cam.z < 3, 'about the midpoint: the world point under it stays: ' + JSON.stringify([w0, w1]));
    console.log('phone spread: ' + s0.grid + ' -> ' + s.grid);

    // 2. The fingers land on two hot-links that vanish under them mid-gesture: the spread still goes in, and so does the next.
    cdp = await open(ph);
    hots = await reachableHots(page);
    var pair = null;
    for (var i = 0; i < hots.length && !pair; i++) for (var j = i + 1; j < hots.length && !pair; j++) {
      var d = Math.hypot(hots[i].x - hots[j].x, hots[i].y - hots[j].y);
      if (d >= 40 && d <= 120) pair = [hots[i], hots[j]];
    }
    assert.ok(pair, 'two hot-links a finger can land on, 40-120 px apart: ' + JSON.stringify(hots));
    await pinchIn(page, cdp, 'phone spread over vanishing hots', [pair[0].x, pair[0].y], [pair[1].x, pair[1].y], async function () {
      var gone = await page.evaluate(function (pts) {
        var n = 0;
        pts.forEach(function (p) { var e = document.elementFromPoint(p.x, p.y), a = e && e.closest('#hots a'); if (a) { a.closest('[role=gridcell]').remove(); n++; } });   // as syncHots() removes a cell
        return n;
      }, pair);
      assert.strictEqual(gone, 2, 'both hot-links under the fingers were removed');
    });
    cdp = await open(ph);
    await pinchIn(page, cdp, 'phone spread after vanishing hots', A, B);

    // 3. Two ghost pointers (fingers whose pointerup never reached the board), 200 px apart, must not warp the next gesture.
    cdp = await open(ph);
    await page.evaluate(function (c) {
      var cv = document.getElementById('cv');
      [[c[0] - 100, c[1]], [c[0] + 100, c[1]]].forEach(function (p, i) {
        cv.dispatchEvent(new PointerEvent('pointerdown', { pointerId: 901 + i, pointerType: 'touch', isPrimary: false, clientX: p[0], clientY: p[1], bubbles: true, cancelable: true }));
      });
    }, c);
    await pinchIn(page, cdp, 'phone spread after ghost pointers', A, B);

    // 4. A 20% spread zooms 1.2x and stays; it crosses no level, so the cursor and its grid stay.
    cdp = await open(ph);
    s0 = await state(page);
    await pinch(page, cdp, A, B, 1.2);
    s = await until(page, function (x) { return Math.abs(x.cam.z / s0.cam.z - 1.2) < 0.06; });
    s = await until(page, function () { return false; }, 300);
    assert.ok(Math.abs(s.cam.z / s0.cam.z - 1.2) < 0.06, 'a 20% spread zooms 1.2x and stays: ' + [s0.cam.z, s.cam.z]);
    assert.deepStrictEqual([s.grid, s.name], [s0.grid, s0.name], 'and moves no cursor');
    console.log('phone 20% spread: z ' + s0.cam.z + ' -> ' + s.cam.z);

    // 5. A pinch in comes out one step: from the plots to the farm.
    await pinch(page, cdp, [c[0] - 60, c[1] - 40], [c[0] + 60, c[1] + 40], 0.3);
    s = await until(page, function (s) { return s.grid !== s0.grid; });
    assert.ok(/^The farm: /.test(s.grid), 'a pinch in comes out to the farm: ' + s.grid);
    console.log('phone pinch in: ' + s0.grid + ' -> ' + s.grid);
    await ph.ctx.close();

    // With motion on: 6. a 20% spread stays where the fingers left it; 7. a full spread goes in.
    ph = await phone(false); page = ph.page; cdp = await open(ph);
    s0 = await state(page);
    await pinch(page, cdp, A, B, 1.2);
    s = await until(page, function () { return false; }, 400);
    assert.ok(Math.abs(s.cam.z / s0.cam.z - 1.2) < 0.06 && s.grid === s0.grid && s.name === s0.name, 'with motion, a 20% spread stays where the fingers left it: ' + [s0.cam.z, s.cam.z]);
    await pinch(page, cdp, A, B, 4);
    s = await until(page, function (s) { return s.grid !== s0.grid; });
    assert.ok(/ phases?, \d+ by \d+$/.test(s.grid), 'with motion, a full spread goes in: ' + s.grid);
    console.log('phone with motion: 20% stays, a full spread goes in');
    await ph.ctx.close();

    assert.deepStrictEqual(errors.filter(function (e) { return !/404/.test(e); }), [], 'phone console errors');
    console.log('touch pinch zoom checks passed');
  } finally { await browser.close(); }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
