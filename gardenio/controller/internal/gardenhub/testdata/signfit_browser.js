/* The tips kept on the screen in a real browser (gd-O3nwaqbNUG), run from TestSignFitUnderChromium against a live /garden on an
   httptest server with the goldens' world. Usage: node signfit_browser.js <base url> <node_modules dir>; with SIGNFIT_SHOTS=<dir>
   it also writes a PNG per case there (the before/after evidence) and reports every miss before it fails.

   A phone (iPhone 12) upright at 390x844 and on its side at 844x390, by day and by night, reduced motion. The steward's tip
   (#stip, the one gardener with a tip) at music's homestead, opened by a hover, the camera cut so the steward's middle sits on
   each edge of the map in turn, stays 16 px in from every edge of the map and out from under what floats over it (the header,
   the camera column, the tab bar, the arrows), its pointer still at the steward; on its side, a steward with a long name and
   reason too, at the iPhone 12's landscape viewport (750x340), whose tip scrolls above the title bar with its link in sight. Zero console errors. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert'), fs = require('fs'), path = require('path');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));
/* What floats over the map, which no tip may sit under: the header, the camera column, the phone's tab bar,
   the arrows to what needs you off the screen. */
var OVER = 'header.crumb, #cam-in, #cam-out, #cam-go, #cam-menu, #tabbar, #bcns > *';
var GUT = 16, shots = process.env.SIGNFIT_SHOTS || '', misses = [], roomless = [];

/* The box of sel and of the Thing's link, in the board's px, and where the label's pointer meets its edge. */
function measure(page, sel, thing) {
  return page.evaluate(function (a) {
    var bd = document.getElementById('board').getBoundingClientRect(), e = document.querySelector(a.sel), t = a.thing ? document.querySelector(a.thing) : null;
    var rel = function (r) { return { x: r.left - bd.left, y: r.top - bd.top, w: r.width, h: r.height }; };
    if (!e || e.hidden || !e.getBoundingClientRect().width) return null;
    var cls = e.className, tick = getComputedStyle(e, '::after'), slot = (/\bs-(above|below|left|right)\b/.exec(cls) || [])[1] || '';
    var over = Array.prototype.filter.call(document.querySelectorAll(a.over), function (o) { return !o.hidden && o.getBoundingClientRect().width; }).map(function (o) { var r = rel(o.getBoundingClientRect()); r.sel = o.id || o.className; return r; });
    return { box: rel(e.getBoundingClientRect()), thing: t ? rel(t.getBoundingClientRect()) : null, w: bd.width, h: bd.height, slot: slot, over: over, map: window.GARDEN.map(),
      tick: slot === 'above' || slot === 'below' ? parseFloat(tick.left) : slot ? parseFloat(tick.top) : NaN };
  }, { sel: sel, thing: thing, over: OVER });
}
/* Cut the camera (GARDEN.look, which answers where it is) so the screen point (sx, sy) of the board moves to (tx, ty); with c, back
   to camera c. */
async function pan(page, sx, sy, tx, ty, c) {
  var at = await page.evaluate(function (a) { var c = a.c || window.GARDEN.look({}); return window.GARDEN.look(a.c ? c : { cx: c.cx + (a.sx - a.tx) / c.z, cy: c.cy + (a.sy - a.ty) / c.z }); }, { sx: sx, sy: sy, tx: tx, ty: ty, c: c || null });
  await page.waitForTimeout(120);
  return at;
}

/* Whether a box of b's size fits somewhere in measure r's map, every 2 px, clear of each arrow and its 2 px ring. */
function roomFor(b, r) {
  var m = r.map, arrows = r.over.filter(function (o) { return o.sel === 'bcn'; }), y0 = Math.max(GUT, m.top || 0), y1 = r.h - Math.max(GUT, m.bottom || 0) - b.h;
  for (var y = y0; y <= y1; y += 2) for (var x = GUT; x <= r.w - GUT - b.w; x += 2) {
    if (arrows.every(function (o) { return x + b.w <= o.x - 2 || x >= o.x + o.w + 2 || y + b.h <= o.y - 2 || y >= o.y + o.h + 2; })) return true;
  }
  return false;
}
/* Walk sel's tip over the four edges with the Thing at thing; name is the case's name in the report and the PNG. */
async function walk(page, name, sel, thing) {
  var m0 = await measure(page, sel, thing);
  assert.ok(m0 && m0.thing, name + ': the tip and its Thing are on the map before the walk');
  var EDGES = { left: [0, null], right: [1, null], top: [null, 0], bottom: [null, 1] }, c0 = await pan(page, 0, 0, 0, 0);
  for (var edge in EDGES) {
    await pan(page, 0, 0, 0, 0, c0);   // the Thing back where the walk began
    var m = await measure(page, sel, thing);
    if (!m || !m.thing) { misses.push(name + ' at the ' + edge + ' edge: the tip or its Thing is gone'); continue; }
    var cx = m.thing.x + m.thing.w / 2, cy = m.thing.y + m.thing.h / 2, e = EDGES[edge];
    await pan(page, cx, cy, e[0] === null ? cx : e[0] * m.w, e[1] === null ? cy : e[1] * m.h);
    var r = await measure(page, sel, thing), tag = name + '-' + edge;
    if (shots) await page.screenshot({ path: path.join(shots, tag + '.png') });
    if (!r) { misses.push(tag + ': no tip'); continue; }
    var b = r.box, ok = b.x >= GUT - 0.5 && b.y >= GUT - 0.5 && b.x + b.w <= r.w - GUT + 0.5 && b.y + b.h <= r.h - GUT + 0.5;
    if (!ok) misses.push(tag + ': ' + JSON.stringify(b) + ' leaves ' + r.w + 'x' + r.h + ' less a ' + GUT + ' px gutter');
    // an arrow is cleared where there is room: a tip that no spot of the map (GARDEN.map, the gutter, the whole width) keeps
    // clear of every arrow (a wide tip on a phone, between an arrow on each side) may sit under one, as SIGNFIT.fit never
    // drops a tip
    var room = roomFor(b, r);
    r.over.forEach(function (o) {
      if (!(b.x < o.x + o.w && o.x < b.x + b.w && b.y < o.y + o.h && o.y < b.y + b.h)) return;
      if (o.sel === 'bcn' && !room) { roomless.push(tag); return; }
      misses.push(tag + ': under ' + o.sel + ' ' + JSON.stringify(o));
    });
    if (r.thing && isFinite(r.tick) && b.w > 2 * GUT) {   // the pointer still at the Thing where the Thing is under the label's edge
      var along = r.slot === 'above' || r.slot === 'below', at = along ? b.x + r.tick : b.y + r.tick, lo = along ? r.thing.x : r.thing.y, hi = lo + (along ? r.thing.w : r.thing.h);
      var lim = along ? [b.x + 6, b.x + b.w - 6] : [b.y + 6, b.y + b.h - 6];
      if (hi > lim[0] && lo < lim[1] && (at < lo - 1 || at > hi + 1)) misses.push(tag + ': its pointer at ' + Math.round(at) + ' is off its Thing ' + Math.round(lo) + '-' + Math.round(hi));
    }
  }
}

/* Open the steward's tip at music's homestead by a hover, the camera at the Bead level on its spot. */
async function stewardTip(page) {
  await page.goto(url + '/garden?at=music');
  await page.waitForSelector('#hots .w-biome.cur', { state: 'attached' });
  var at = await page.evaluate(async function () {
    var doc = await (await fetch('/garden/farm', { headers: { Accept: 'application/json' } })).json();
    var b = WORLD.layout(doc, null, { readonly: true }).index['project:music'], s = WORLD.stewardSpot(b), T = WORLD.TILE;
    return { cx: s.col * T, cy: (s.row - 1) * T };
  });
  await page.evaluate(function (c) { window.GARDEN.look({ cx: c.cx, cy: c.cy, z: 1.12 }); }, at); await page.waitForTimeout(120);
  await page.evaluate(function () { window.GARDEN.look({ z: 1 }); }); await page.waitForTimeout(120);
  await page.waitForSelector('#ghots a.w-steward', { state: 'attached' });
  await page.evaluate(function () { document.querySelector('#ghots a.w-steward').dispatchEvent(new PointerEvent('pointerenter', { pointerType: 'mouse' })); });
  await page.waitForSelector('#stip:not([hidden])');
}

/* A steward with a 150-char name, a reason at the server's 140-rune clip and its day's runs used, on a phone on its side
   (gd-DzceM7DY86): its tip is taller than the map, so it stops 16 px inside the map and above the title bar, scrolls, and its
   "Open bead" is in sight without a scroll. The map is the iPhone 12's landscape viewport (750x340), not 844x390: there the
   tip is 283 px in Liberation or FreeSans, as tall as the map's room, and does not scroll in every font (gd-n6J8iiN3TQ). */
async function longSteward(page, tag) {
  await page.route('**/garden/farm', async function (route) {
    var res = await route.fetch(), doc = await res.json();
    (doc.cost && doc.cost.projects || []).forEach(function (r) {
      if (r.project !== 'music' || !r.steward) return;
      r.steward.agent = 'music-steward-' + 'with-a-very-long-land-name-'.repeat(6).slice(0, 136);
      r.steward.reason = { kind: 'in_flight', text: ('Waiting on CI for https://github.com/acme/music/pull/4242/checks?check_run_id=0123456789abcdef and gd-AbCdEfGhIj, ' + 'then a review '.repeat(4)).slice(0, 140) };
      r.steward.capped = true; r.steward.max_runs_per_day = 24; r.steward.runs_today = 24;
    });
    await route.fulfill({ response: res, json: doc });
  });
  await stewardTip(page);
  if (shots) await page.screenshot({ path: path.join(shots, tag + '.png') });
  var r = await page.evaluate(function () {
    var bd = document.getElementById('board').getBoundingClientRect(), t = document.getElementById('stip'), b = t.getBoundingClientRect(), cb = document.getElementById('cursor-bar'), tb = document.getElementById('tabbar').getBoundingClientRect(), foot = bd.bottom;
    if (tb.height && tb.width > tb.height) foot = Math.min(foot, tb.top);   // the tab bar while it is a row
    var cr = cb.hidden ? null : cb.getBoundingClientRect(); if (cr && cr.height && cr.top > bd.top + bd.height / 2) foot = Math.min(foot, cr.top);   // the title bar on it
    var open = Array.prototype.filter.call(t.querySelectorAll('a'), function (a) { return a.textContent === 'Open bead'; })[0], o = open && open.getBoundingClientRect();
    return { top: b.top - bd.top, bottom: b.bottom - bd.top, h: bd.height, foot: foot - bd.top, scrolls: t.scrollHeight > t.clientHeight, open: o ? { top: o.top - b.top, bottom: o.bottom - b.top } : null, inner: t.clientHeight };
  });
  await page.unroute('**/garden/farm');
  if (!r.scrolls) misses.push(tag + ': the tip does not scroll; the case wants one taller than the map');
  if (r.top < GUT - 0.5 || r.bottom > Math.min(r.h, r.foot) - GUT + 0.5) misses.push(tag + ': the tip ' + Math.round(r.top) + '-' + Math.round(r.bottom) + ' leaves the map above the title bar at ' + Math.round(r.foot) + ', less ' + GUT + ' px');
  if (!r.open || r.open.top < 0 || r.open.bottom > r.inner + 6) misses.push(tag + ': its Open bead is out of sight: ' + JSON.stringify(r.open) + ' in ' + r.inner + ' px');
}

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  try {
    if (shots) fs.mkdirSync(shots, { recursive: true });
    var errors = [], cases = 0;
    var base = Object.assign({}, pw.devices['iPhone 12'], { deviceScaleFactor: 1 });
    var DEVS = { phone: { width: 390, height: 844 }, side: { width: 844, height: 390 } };
    var LONG = pw.devices['iPhone 12 landscape'].viewport;   // its browser on its side, under Safari's bars: the long tip's map
    for (var dev in DEVS) for (var sky of ['day', 'night']) {
      var ctx = await browser.newContext(Object.assign({}, base, { viewport: DEVS[dev], screen: DEVS[dev], reducedMotion: 'reduce', colorScheme: sky === 'night' ? 'dark' : 'light' }));
      await ctx.addInitScript(function (s) { try { localStorage.setItem('garden.sky', s); } catch (e) { /* the scheme decides */ } }, sky);
      var page = await ctx.newPage();
      page.on('console', function (m) { if (m.type() === 'error') errors.push(m.text()); });
      page.on('pageerror', function (e) { errors.push(String(e)); });
      await page.goto(url + '/garden');
      await page.waitForSelector('#cursor-bar .cb-me');
      // the steward's tip at music's homestead, opened by a hover (the goldens' steward row)
      await stewardTip(page);
      await walk(page, 'steward-' + dev + '-' + sky, '#stip', '#ghots a.w-steward'); cases++;
      if (dev === 'side') { await page.setViewportSize(LONG); await longSteward(page, 'stewardlong-' + dev + '-' + sky); cases++; }
      await ctx.close();
    }
    assert.deepStrictEqual(errors, [], 'no console errors');
    if (misses.length) { console.log(misses.join('\n')); process.exit(1); }
    console.log('signfit: ' + cases + ' cases x 4 edges, every tip 16 px inside the map' + (roomless.length ? '; under an arrow with no room to clear it: ' + roomless.join(', ') : ''));
  } finally {
    await browser.close();
  }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
