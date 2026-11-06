/* Browser check for the beacons (gd-gtr7qBCn9z), run from TestBeaconsUnderChromium against a live /garden on an httptest
   server with the goldens' world. Usage: node beacons_browser.js <base url> <node_modules dir>.

   With the cursor inside plan P (depth 2, its phases framed: cursor-grid spec 4.1) the asks that need you are off-screen, so arrows sit on the map's edge: inside the board, under
   the header, clear of the camera group and the drawers, never on each other, each saying in words how many,
   which way and what. Desktop (1440x900, reduced motion): a click is a cursor jump there (the camera cuts) and the arrow it used
   goes, because its target is now on screen; Enter on an arrow jumps there and hands focus to the cursor's link on the map.
   Phone (390x844, touch): the arrows are 44 px and clear of the tab bar, and a tap flies there. Without reduced motion the
   fly is animated: the camera passes through in-between centres before it lands. Phone landscape (750x340, the cursor on
   harvest cell gd-A1y): the arrows keep off the title bar and the camera column. */
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
var START = '/garden?at=gardenio/gd-planPa';
/* the cursor on plan P's first phase, its phases framed: an ?at= frames the cursor where Enter only zooms a step (cursor-grid
   spec revision 3) */
async function start(page) {
  await page.goto(url + START);
  await page.waitForSelector('#hots .w-bed.cur', { state: 'attached' });
  await page.waitForSelector('#bcns .bcn', { state: 'visible' });
}

async function open(ctx) {
  var page = await ctx.newPage(), errors = [];
  page.on('console', function (m) { if (m.type() === 'error') errors.push(m.text()); });
  page.on('pageerror', function (e) { errors.push(String(e)); });
  await start(page);
  return { page: page, errors: errors };
}

/* Every arrow's box and words, with the rects it must keep off. */
function arrows(page) {
  return page.evaluate(function () {
    var o = document.getElementById('board').getBoundingClientRect(), hb = document.querySelector('header.crumb').getBoundingClientRect();
    var rect = function (b) { return { x: b.left - o.left, y: b.top - o.top, w: b.width, h: b.height }; };
    var keep = Array.prototype.map.call(document.querySelectorAll('.cam,.dbtns,.dw,.tabbar,.curbar'), function (e) { return rect(e.getBoundingClientRect()); })
      .filter(function (r) { return r.w && r.h; });
    var list = Array.prototype.map.call(document.querySelectorAll('#bcns .bcn'), function (n) {
      return { key: n._key, label: n.getAttribute('aria-label'), glyph: n.querySelector('.bcn-g').textContent, box: rect(n.getBoundingClientRect()) };
    });
    return { list: list, keep: keep, w: o.width, h: o.height, top: hb.bottom - o.top };
  });
}
function hit(a, b) { return a.x < b.x + b.w && b.x < a.x + a.w && a.y < b.y + b.h && b.y < a.y + a.h; }
function check(name, s, size) {
  assert.ok(s.list.length > 0, name + ': an arrow for what is off-screen');
  s.list.forEach(function (a, i) {
    var b = a.box;
    assert.ok(Math.abs(b.w - size) < 1 && Math.abs(b.h - size) < 1, name + ': ' + size + ' px arrow: ' + JSON.stringify(b));
    assert.ok(b.x >= 0 && b.y >= s.top && b.x + b.w <= s.w && b.y + b.h <= s.h, name + ': on the board, under the header: ' + JSON.stringify([b, s.top]));
    s.keep.forEach(function (k) { assert.ok(!hit(b, k), name + ': clear of a control ' + JSON.stringify([b, k])); });
    s.list.forEach(function (c, j) { if (j > i) assert.ok(!hit(b, c.box), name + ': arrows apart'); });
    assert.ok(/^\d+ needs? you to the (north|south|east|west|north-east|north-west|south-east|south-west): .+\. Fly to .+\.$/.test(a.label), name + ': words: ' + a.label);
    assert.ok(a.glyph === '?' || a.glyph === '!', name + ': a glyph, not colour alone');
  });
}
function centre(page) { return page.evaluate(function () { return document.getElementById('hots').style.transform; }); }

async function desktop(browser) {
  var ctx = await skyCtx(browser, { viewport: { width: 1440, height: 900 }, reducedMotion: 'reduce' });
  var o = await open(ctx), page = o.page, s = await arrows(page);
  check('desktop', s, 36);
  var first = s.list[0];
  var before = await centre(page);
  await page.click('#bcns .bcn >> nth=0');
  await page.waitForFunction(function (k) { return !Array.prototype.some.call(document.querySelectorAll('#bcns .bcn'), function (n) { return n._key === k; }); }, first.key, { timeout: 5000 });
  assert.notStrictEqual(await centre(page), before, 'desktop: the click moved the camera');
  console.log('desktop: ' + s.list.length + ' arrow(s); clicked "' + first.label + '"');

  // keyboard: Enter on an arrow flies there and the target's link on the map takes focus
  await start(page);
  await page.focus('#bcns .bcn');
  await page.keyboard.press('Enter');
  await page.waitForFunction(function () { var a = document.activeElement; return a && a.classList.contains('hot') && document.getElementById('hots').contains(a); }, null, { timeout: 5000 });
  console.log('desktop: Enter landed focus on ' + (await page.evaluate(function () { return document.activeElement.getAttribute('aria-label') || document.activeElement.textContent; })));
  assert.deepStrictEqual(o.errors.filter(function (e) { return !/404/.test(e); }), [], 'desktop console errors');
  await ctx.close();

  // motion allowed: the fly is animated, so the camera passes through more than its start and end. The camera is read every
  // frame until the fly lands (GARDEN.cam().anim), not for a span of wall time: on a slow runner the click's own work can
  // outlast any span before the fly's first frame (gd-Hxl1jtuWHN). The deadline only ends a fly that never lands.
  ctx = await skyCtx(browser, { viewport: { width: 1440, height: 900 }, reducedMotion: 'no-preference' });
  o = await open(ctx); page = o.page;
  var fly = await page.evaluate(function () {
    return new Promise(function (done) {
      var c = {}, t0 = Date.now(), hots = document.getElementById('hots');
      c[hots.style.transform] = 1;
      document.querySelector('#bcns .bcn').click();
      var started = GARDEN.cam().anim;
      (function step() {
        c[hots.style.transform] = 1;
        if (GARDEN.cam().anim && Date.now() - t0 < 10000) requestAnimationFrame(step);
        else done({ started: started, landed: !GARDEN.cam().anim, seen: Object.keys(c).length });
      })();
    });
  });
  assert.ok(fly.started, 'desktop: the click starts a fly when motion is allowed');
  assert.ok(fly.landed, 'desktop: the fly lands');
  assert.ok(fly.seen > 2, 'desktop: the fly animates when motion is allowed: ' + fly.seen + ' camera transforms');
  await ctx.close();
}

async function phone(browser) {
  var ctx = await skyCtx(browser, Object.assign({}, pw.devices['iPhone 12'], { viewport: { width: 390, height: 844 }, reducedMotion: 'reduce' }));
  var o = await open(ctx), page = o.page, s = await arrows(page);
  check('phone', s, 44);
  var first = s.list[0];
  await page.tap('#bcns .bcn >> nth=0');
  await page.waitForFunction(function (k) { return !Array.prototype.some.call(document.querySelectorAll('#bcns .bcn'), function (n) { return n._key === k; }); }, first.key, { timeout: 5000 });
  console.log('phone: ' + s.list.length + ' arrow(s); tapped "' + first.label + '"');
  assert.deepStrictEqual(o.errors.filter(function (e) { return !/404/.test(e); }), [], 'phone console errors');
  await ctx.close();
}

/* The cursor on a harvest cell in phone landscape: the arrows keep off the title bar and the camera column. */
async function landscape(browser) {
  var ctx = await skyCtx(browser, Object.assign({}, pw.devices['iPhone 12 landscape'], { viewport: { width: 750, height: 340 }, reducedMotion: 'reduce' }));
  var page = await ctx.newPage(), errors = [];
  page.on('pageerror', function (e) { errors.push(String(e)); });
  await page.goto(url + '/garden?at=gardenio/gd-A1y');
  await page.waitForSelector('#bcns .bcn', { state: 'visible' });
  var s = await arrows(page);
  check('landscape', s, 44);
  console.log('landscape: ' + s.list.length + ' arrow(s), clear of the controls');
  assert.deepStrictEqual(errors, [], 'landscape page errors');
  await ctx.close();
}

(async function () {
  var browser = await pw.chromium.launch();
  try { await desktop(browser); await phone(browser); await landscape(browser); } finally { await browser.close(); }
})().catch(function (e) { console.error(e); process.exit(1); });
