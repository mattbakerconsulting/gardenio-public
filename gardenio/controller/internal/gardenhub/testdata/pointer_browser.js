/* Pointer and touch drive the free camera and the cursor in a real browser (gardenio/docs/superpowers/specs/2026-10-20-lands-cursor-grid-design.md
   revision 3, sections 4.3 and 7), run from TestCursorPointerUnderChromium against a live /garden on an httptest server with
   the goldens' world. Usage: node pointer_browser.js <base url> <node_modules dir>.

   Desktop 1280x800, reduced motion: a click on a cell moves the cursor there and neither opens nor moves the camera, a second
   click on it goes in, and on a step opens the bead view; a double-click is those two clicks; a click on a Thing outside the
   current grid jumps the cursor to it; a mouse drag of 100 px pans the camera 100/z world px and moves no cursor; a wheel
   zooms about the pointer, the world point under it staying put, and a sideways wheel is nothing; a wheel in across a level
   boundary takes the cursor into the cell under the pointer, and out across it brings it back; an arrow moves the cursor and
   not the camera; Enter goes in and zooms a step about the cursor's Thing, Escape comes out.
   Phone (iPhone 12, 390x844, touch), reduced motion: a tap moves the cursor and a second tap goes in; one finger pans the
   camera and moves no cursor. Zero console errors. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));
var A = '#hots a.w-plot[href="/garden?bead=gd-planA"]';

/* What the cursor is: the grid's name, the cursor's name and coordinates, the zoom the camera strip shows, the bead view. */
function state(page) {
  return page.evaluate(function () {
    var hots = document.getElementById('hots'), cur = hots.querySelector('.hot.cur'), bv = document.getElementById('bead'), gc = cur && cur.parentNode;
    return { grid: hots.getAttribute('aria-label'), name: cur ? cur.getAttribute('aria-label') : '', kind: cur ? cur.className : '',
      row: gc && gc.parentNode ? +gc.parentNode.getAttribute('aria-rowindex') : 0, col: gc ? +gc.getAttribute('aria-colindex') : 0,
      level: WORLD.LEVEL_NAMES[window.GARDEN.cam().level] + ' · ' + Math.round(window.GARDEN.cam().z * 100) / 100, bead: !!(bv && !bv.hidden), hots: hots.style.transform, cam: window.GARDEN.cam().cam };
  });
}
/* A point of each of the current grid's cells a pointer can reach (clear of the map's strips), the cursor's first: the centre,
   else the point nearest it that the cell itself takes (a child label or a glyph can sit inside it). */
function cells(page) {
  return page.evaluate(function () {
    var b = document.getElementById('board').getBoundingClientRect(), out = [];
    Array.prototype.forEach.call(document.querySelectorAll('#hots .hot'), function (a) {
      var r = a.getBoundingClientRect(), cx = r.left + r.width / 2, cy = r.top + r.height / 2, pts = [];
      if (r.width < 8 || r.height < 8 || cx < b.left + 30 || cx > b.right - 30 || cy < b.top + 90 || cy > b.bottom - 130) return;
      for (var i = 1; i < 8; i++) for (var j = 1; j < 8; j++) pts.push([r.left + r.width * i / 8, r.top + r.height * j / 8]);
      pts.sort(function (p, q) { return Math.hypot(p[0] - cx, p[1] - cy) - Math.hypot(q[0] - cx, q[1] - cy); });
      var at = pts.filter(function (p) { var hit = document.elementFromPoint(p[0], p[1]); return hit && hit.closest('.hot') === a; })[0];
      if (at) out.push({ x: at[0], y: at[1], cur: a.classList.contains('cur'), name: a.getAttribute('aria-label'), plot: a.classList.contains('w-plot') && !a.classList.contains('w-staked') });   // a staked plot has no planted phases to go into
    });
    return out.sort(function (p, q) { return q.cur - p.cur; });
  });
}
/* A plot other than the cursor's that a pointer can reach. */
async function other(page) {
  var cs = await cells(page), c = cs.filter(function (x) { return !x.cur && x.plot; })[0];
  assert.ok(c, 'a plot other than the cursor\'s a pointer can reach: ' + JSON.stringify(cs));
  return c;
}
async function settle(page, ms) { await page.waitForTimeout(ms || 120); return state(page); }
/* The state once ok(state) holds, or the last one after ms. A wheel gesture ends GEST_IDLE ms after its last event on a timer,
   which a loaded runner fires late; a fixed wait reads the camera mid-gesture (gd-0aFW0WhV5d). */
async function until(page, ok, ms) {
  var end = Date.now() + (ms || 5000), s;
  for (;;) { s = await state(page); if (ok(s) || Date.now() > end) return s; await page.waitForTimeout(50); }
}
/* The world point under board point (x, y) at camera c, and the board's box. */
function under(c, vp, x, y) { return { x: c.cx + (x - vp.w / 2) / c.z, y: c.cy + (y - vp.h / 2) / c.z }; }
function near(a, b, eps, what) { assert.ok(Math.abs(a - b) <= eps, what + ': ' + a + ' vs ' + b); }

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  try {
    var errors = [];
    async function context(opts) {
      var ctx = await browser.newContext(Object.assign({ reducedMotion: 'reduce' }, opts));
      await ctx.addInitScript(function () { try { localStorage.setItem('garden.sky', 'day'); } catch (e) { /* the scheme decides */ } });
      var page = await ctx.newPage();
      page.on('console', function (m) { if (m.type() === 'error') errors.push(m.text()); });
      page.on('pageerror', function (e) { errors.push(String(e)); });
      return { ctx: ctx, page: page };
    }
    async function open(page) {
      await page.goto(url + '/garden?at=gardenio/gd-planA');
      await page.waitForSelector(A + '.cur', { state: 'attached' });
      await page.waitForFunction(function () { return /scale/.test(document.getElementById('hots').style.transform); });
      return state(page);
    }

    /* ---------- desktop ---------- */
    var d = await context({ viewport: { width: 1280, height: 800 } }), page = d.page;
    var s0 = await open(page), plots = s0.grid;

    // 1. A click on another plot moves the cursor there and opens nothing; a second click goes into its phases.
    var c = await other(page);
    await page.mouse.click(c.x, c.y);
    var s = await settle(page);
    assert.strictEqual(s.name, c.name, 'a click moves the cursor to the cell: ' + JSON.stringify(s));
    assert.ok(s.grid === plots && !s.bead, 'and opens nothing: ' + JSON.stringify(s));
    assert.strictEqual(s.hots, s0.hots, 'and moves no camera');
    await page.mouse.click(c.x, c.y);
    s = await settle(page);
    assert.ok(/ phases?, \d+ by \d+$/.test(s.grid) && /w-bed/.test(s.kind), 'a second click on the cursor goes in: ' + JSON.stringify(s));
    var phases = s.grid;

    // 2. Into a phase the same way, then a click on a step moves the cursor and a second opens the bead view. Two gardeners
    //    on one task stand side by side (world.js gardenerSpots), so a two-step bed can have a gardener over every step and no
    //    step a pointer reaches; then the walk comes back to plan A's project and takes the next plot (gd-wDdun2AVwq).
    var cs = [], step = null;
    for (var pi = 0; !step; pi++) {
      if (pi) {
        await open(page);
        var cands = (await cells(page)).filter(function (x) { return !x.cur && x.plot; });
        assert.ok(pi < cands.length, 'a plot whose current bed has a step a pointer reaches: none of ' + pi);
        await page.mouse.click(cands[pi].x, cands[pi].y); await settle(page);
        await page.mouse.click(cands[pi].x, cands[pi].y); s = await settle(page);
        if (!/ phases?, \d+ by \d+$/.test(s.grid)) continue;
      }
      c = (await cells(page))[0];
      if (!c || !c.cur) continue;
      await page.mouse.click(c.x, c.y);
      s = await settle(page);
      if (pi && !/ steps?, \d+ by \d+$/.test(s.grid)) continue;   // a plan with no steps yet has an empty bed to go into
      assert.ok(/ steps?, \d+ by \d+$/.test(s.grid) && /w-task/.test(s.kind), 'a click on the cursor\'s phase goes in: ' + JSON.stringify(s));
      cs = await cells(page); step = cs.filter(function (x) { return !x.cur; })[0] || cs[0] || null;
      if (!step) console.log('desktop: every step of ' + s.grid.split(':')[0] + '\'s bed has a gardener on it; the next plot');
    }
    if (!step.cur) { await page.mouse.click(step.x, step.y); s = await settle(page); assert.ok(s.name === step.name && !s.bead, 'a click on a step moves the cursor: ' + JSON.stringify(s)); }
    await page.mouse.click(step.x, step.y);
    await page.locator('#bead').waitFor({ state: 'visible', timeout: 3000 });
    await page.keyboard.press('Escape');
    await page.locator('#bead').waitFor({ state: 'hidden', timeout: 3000 });
    console.log('desktop: click moves, click again goes in, and opens a step');

    // 3. Back at plan A's project, a double-click on another plot is the two clicks: the cursor moves there and goes in.
    await open(page);
    c = await other(page);
    await page.mouse.dblclick(c.x, c.y);
    s = await settle(page);
    assert.ok(/ phases?, \d+ by \d+$/.test(s.grid), 'a double-click goes into the plot: ' + JSON.stringify(s));
    assert.strictEqual(s.grid.indexOf(c.name.split(':')[0] + ':'), 0, 'into the plot under the pointer: ' + s.grid + ' / ' + c.name);
    assert.ok(s.cam.z < 3 * s0.cam.z, 'a double-click that goes in zooms one step, not two: ' + [s0.cam.z, s.cam.z]);

    // 4. A click on a Thing outside the current grid (on the canvas, where the grid has no links) jumps the cursor to it: from
    //    inside a plan, another plot of the project; from the project's plots, another project or the Commons.
    async function outside() {
      return page.evaluate(function () {
        var hots = document.getElementById('hots'), tf = /translate\(([-\d.]+)px, ?([-\d.]+)px\) scale\(([-\d.]+)\)/.exec(hots.style.transform);
        var b = document.getElementById('board').getBoundingClientRect(), grid = hots.getAttribute('aria-label'), here = grid.split(':')[0], T = WORLD.TILE || 16, cv = document.getElementById('cv');
        return fetch('/garden/farm', { headers: { Accept: 'application/json' } }).then(function (r) { return r.json(); }).then(function (doc) {
          var memo = null; try { memo = JSON.parse(localStorage.getItem('garden.world.v1')); } catch (e) { /* a fresh layout */ }
          var w = WORLD.layout(doc, memo, { readonly: true }), inPlan = here !== 'gardenio', cands = [];
          if (inPlan) w.index['project:gardenio'].plots.forEach(function (p) { if (grid.indexOf(p.plan.title + ':') !== 0) cands.push({ r: p, title: p.plan.title }); });   // a title can hold a colon
          else { w.biomes.forEach(function (bi) { if (bi.key !== 'gardenio') cands.push({ r: bi.foot, title: bi.name }); }); }
          for (var i = 0; i < cands.length; i++) for (var u = 1; u < 6; u++) for (var v = 1; v < 6; v++) {
            var r = cands[i].r, x = b.left + +tf[1] + (r.x + r.w * u / 6) * T * +tf[3], y = b.top + +tf[2] + (r.y + r.h * v / 6) * T * +tf[3];
            if (x < b.left + 30 || x > b.right - 30 || y < b.top + 90 || y > b.bottom - 130 || document.elementFromPoint(x, y) !== cv) continue;
            return { x: x, y: y, title: cands[i].title };
          }
          return null;
        });
      });
    }
    var target = await outside();
    if (!target) { await page.keyboard.press('Escape'); await settle(page); target = await outside(); }
    assert.ok(target, 'a Thing outside the current grid shows on the canvas, inside a plan or from the project\'s plots');
    var before = await state(page);
    await page.mouse.click(target.x, target.y);
    s = await settle(page);
    assert.ok(s.grid !== before.grid || s.name !== before.name, 'a click outside the grid jumps the cursor: ' + JSON.stringify([before, s, target]));
    assert.ok(s.name.indexOf(target.title) === 0 || s.grid.indexOf(target.title + ':') === 0, 'onto what was under the pointer: ' + JSON.stringify([s, target]));
    assert.strictEqual(s.hots, before.hots, 'a jump by a click moves no camera');
    console.log('desktop: a click outside the grid jumped to ' + target.title + ' (' + s.grid + ')');

    // 5. A mouse drag of 100 px right and 40 down pans the camera 100/z and 40/z world px the other way; the cursor stays, though
    // the press lands on another cell's link (the farmstead, at the board's centre since the camera frames it, HW 29.6).
    s0 = await open(page);
    var b = await page.locator('#board').boundingBox(), vpw = { w: b.width, h: b.height };
    await page.mouse.move(b.x + b.width / 2, b.y + b.height / 2);
    await page.mouse.down();
    for (var i = 1; i <= 10; i++) await page.mouse.move(b.x + b.width / 2 + i * 10, b.y + b.height / 2 + i * 4);
    await page.mouse.up();
    s = await settle(page);
    near(s.cam.cx, s0.cam.cx - 100 / s0.cam.z, 0.5, 'a 100 px drag pans cx by 100/z');
    near(s.cam.cy, s0.cam.cy - 40 / s0.cam.z, 0.5, 'a 40 px drag pans cy by 40/z');
    assert.deepStrictEqual([s.cam.z, s.name, s.grid, s.bead], [s0.cam.z, s0.name, s0.grid, false], 'a drag zooms nothing, moves no cursor and opens nothing');
    console.log('desktop: a 100 px drag panned ' + Math.round((s0.cam.cx - s.cam.cx) * 100) / 100 + ' world px at z ' + s0.cam.z);

    // 6. A wheel zooms about the pointer: the world point under it stays put. A sideways wheel is nothing.
    s0 = await open(page);
    var px = b.width * 0.4, py = b.height * 0.55, w0 = under(s0.cam, vpw, px, py);
    await page.mouse.move(b.x + px, b.y + py);
    await page.mouse.wheel(0, -40);
    s = await until(page, function (x) { return x.cam.z !== s0.cam.z; });
    var w1 = under(s.cam, vpw, px, py);
    assert.ok(s.cam.z > s0.cam.z, 'a wheel up zooms in: ' + [s0.cam.z, s.cam.z]);
    near(w1.x, w0.x, 0.5, 'the world x under the pointer stays');
    near(w1.y, w0.y, 0.5, 'the world y under the pointer stays');
    var s6 = s;
    await page.mouse.wheel(300, 0);
    s = await settle(page, 200);
    assert.deepStrictEqual([s.grid, s.name, s.hots], [s6.grid, s6.name, s6.hots], 'a sideways wheel is nothing');
    console.log('desktop: a 40 px wheel zoomed ' + s0.cam.z + ' -> ' + Math.round(s.cam.z * 1000) / 1000 + ' about the pointer');

    // 7. A wheel in across the Plan edge (z 0.41) over another plot takes the cursor into that plot; out across it brings the
    //    cursor back to the plots, onto it.
    s0 = await open(page);
    c = await other(page);
    await page.mouse.move(c.x, c.y);
    for (i = 0; i < 40 && !(/ phases?, \d+ by \d+$/.test((await state(page)).grid)); i++) await page.mouse.wheel(0, -60);
    s = await state(page);
    assert.ok(/^(Plan|Bead) /.test(s.level) && / phases?, \d+ by \d+$/.test(s.grid), 'a wheel across the Plan edge goes in: ' + JSON.stringify(s));
    assert.strictEqual(s.grid.indexOf(c.name.split(':')[0] + ':'), 0, 'into the plot under the pointer: ' + s.grid + ' / ' + c.name);
    for (i = 0; i < 40 && /^(Plan|Bead) /.test((await state(page)).level); i++) await page.mouse.wheel(0, 60);
    s = await until(page, function (x) { return x.grid === plots; });
    assert.ok(/^(Project|World) /.test(s.level) && s.grid === plots && s.name === c.name, 'a wheel out across it comes out onto the plot: ' + JSON.stringify(s));
    console.log('desktop: the wheel across the Plan edge went into ' + c.name.split(':')[0] + ' and back out');

    // 8. An arrow moves the cursor and not the camera; Shift and an arrow too.
    var keyed = 0;
    for (var k of ['ArrowRight', 'ArrowLeft', 'ArrowDown', 'ArrowUp', 'Shift+ArrowRight']) {
      s0 = await open(page);
      await page.keyboard.press(k);
      s = await settle(page);
      assert.strictEqual(s.hots, s0.hots, k + ' moves no camera: ' + JSON.stringify([s0, s]));
      if (s.name !== s0.name) keyed++;
    }
    assert.ok(keyed >= 1, 'some arrow moved the cursor');

    // 9. Enter goes in, zooming a step about the cursor's Thing (its middle stays where it was on the board); Escape comes out.
    s0 = await open(page);
    var at = await page.evaluate(function (sel) { var r = document.querySelector(sel).getBoundingClientRect(), o = document.getElementById('board').getBoundingClientRect(); return [r.left - o.left + r.width / 2, r.top - o.top + r.height / 2]; }, A);
    await page.focus(A);
    await page.keyboard.press('Enter');
    s = await settle(page);
    assert.ok(/ phases?, \d+ by \d+$/.test(s.grid) && s.cam.z >= 2 * s0.cam.z - 1e-9, 'Enter goes in and zooms a step: ' + JSON.stringify([s0.cam, s.cam]));
    var wa = under(s0.cam, vpw, at[0], at[1]), wb = under(s.cam, vpw, at[0], at[1]);
    near(wb.x, wa.x, 1, 'about plan A: its middle stays (x)'); near(wb.y, wa.y, 1, 'about plan A: its middle stays (y)');
    await page.keyboard.press('Escape');
    s = await settle(page);
    assert.ok(s.grid === plots && s.cam.z < s0.cam.z * 1.5, 'Escape comes out and zooms back: ' + JSON.stringify(s));
    console.log('desktop: drag pans, wheel zooms about the pointer and follows depth, arrows keep the camera, Enter and Escape step');
    await d.ctx.close();

    /* ---------- phone ---------- */
    var ph = await context(Object.assign({}, pw.devices['iPhone 12'], { viewport: { width: 390, height: 844 } }));
    page = ph.page;
    s0 = await open(page);
    var cdp = await ph.ctx.newCDPSession(page);

    // 8. A tap moves the cursor, a second tap goes in.
    c = await other(page);
    await page.touchscreen.tap(c.x, c.y);
    s = await settle(page);
    assert.ok(s.name === c.name && s.grid === s0.grid && !s.bead, 'a tap moves the cursor: ' + JSON.stringify(s));
    await page.touchscreen.tap(c.x, c.y);
    s = await settle(page);
    assert.ok(/ phases?, \d+ by \d+$/.test(s.grid), 'a second tap goes in: ' + JSON.stringify(s));
    console.log('phone: tap moves, tap again goes in');

    // 9. One finger pans: a 60 px drag up moves the camera 60/z down the world, and no cursor.
    await page.keyboard.press('Escape');
    s0 = await settle(page, 300);
    var bb = await page.locator('#board').boundingBox(), x = Math.round(bb.x + bb.width / 2), y = Math.round(bb.y + bb.height / 2);
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ id: 1, x: x, y: y }] });
    for (var k = 1; k <= 6; k++) await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ id: 1, x: x, y: y - 10 * k }] });
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
    s = await settle(page);
    near(s.cam.cy, s0.cam.cy + 60 / s0.cam.z, 0.5, 'one finger pans');
    assert.deepStrictEqual([s.cam.z, s.name, s.grid], [s0.cam.z, s0.name, s0.grid], 'one finger zooms nothing and moves no cursor');
    console.log('phone: one finger panned ' + Math.round((s.cam.cy - s0.cam.cy) * 100) / 100 + ' world px at z ' + s0.cam.z);
    await ph.ctx.close();

    assert.deepStrictEqual(errors.filter(function (e) { return !/404/.test(e); }), [], 'console errors');
    console.log('pointer browser checks passed');
  } finally { await browser.close(); }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
