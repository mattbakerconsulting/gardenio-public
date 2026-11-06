/* Browser check for a scoped viewer's named bridge (gd-DgEhq38EnY), run from TestBridgeUnderChromium against a live /garden on
   an httptest server with the card fixture farm cut to gardenio, coop and music: plan A's bug gd-A2c waits on gd-kbeadsa1a,
   which the viewer cannot read, so the document names only its project (blocked_by_foreign ["kbeads"]) and the map has no
   kbeads region, but has neighbours the bridge must not reach.
   Usage: node bridge_browser.js <base url> <node_modules dir>. GARDENHUB_SHOTS=<dir> saves the screenshots.

   Desktop (1440x900) and phone (390x844), light and dark, at the Project and World levels: the bridge's channel is on the
   canvas where edges.js routes it (water along its middle, not beside it), its far end lies outside gardenio's region, its
   deck is drawn there where that is on screen, and its name is on the map the HUD leaves: ink in the box it is drawn in. In words, as before the bridge: plan A's
   plot link says "Blocked by a bead in kbeads", the gardenio region's link sums it, and nothing links to the hidden bead.
   Zero console errors. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert'), path = require('path');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));
var PA = '#hots a.w-plot[href="/garden?bead=gd-planA"]', BIOME = '#hots .w-biome[aria-label^="gardenio:"]';

function skyCtx(browser, opts) {
  return browser.newContext(opts).then(function (ctx) {
    return ctx.addInitScript(function (s) { try { localStorage.setItem('garden.sky', s); } catch (e) { /* no storage: the emulated scheme decides */ } }, opts.colorScheme === 'dark' ? 'night' : 'day').then(function () { return ctx; });
  });
}
async function shot(page, name) { if (process.env.GARDENHUB_SHOTS) await page.screenshot({ path: path.join(process.env.GARDENHUB_SHOTS, 'bridge-' + name + '.png') }); }
async function frames(page) { await page.evaluate(function () { return new Promise(function (r) { requestAnimationFrame(function () { requestAnimationFrame(r); }); }); }); }
async function go(page, q, lvl, sel) {
  await page.goto(url + '/garden' + q);
  await page.waitForFunction(function (l) { return !!window.GARDEN && new RegExp(l).test(WORLD.LEVEL_NAMES[window.GARDEN.cam().level]); }, lvl.source);
  await page.waitForSelector(sel, { state: 'attached' });
  await frames(page);
}
/* The farm framed at the World level: depth 0 on a board made small enough for it (edges_browser.js's worldView). */
async function worldView(page, q, sel) {
  var vs = page.viewportSize(), w = vs.width, h = vs.height;
  for (var i = 0; i < 4; i++) {
    await go(page, q, /^(World|Project)/, sel);
    if (await page.evaluate(function () { return window.GARDEN.cam().level; }) === 0) return vs;
    w = Math.round(w * 0.75); h = Math.round(h * 0.75);
    await page.setViewportSize({ width: w, height: h });
  }
  assert.fail('depth 0 never framed the World level');
}

/* The bridge at this level, recomputed in the page from its own farm document, WORLD and EDGES, put on screen through the hot
   link sel (whose world rect is anchor's); then the canvas: water along the channel's middle and three widths aside, the
   deck's planks at the far end, and ink in the name's box. */
async function bridgePixels(page, sel, anchor, night) {
  return page.evaluate(async function (a) {
    var doc = await (await fetch('/garden/farm', { headers: { Accept: 'application/json' } })).json();
    var memo = null; try { memo = JSON.parse(localStorage.getItem('garden.world.v1')); } catch (e) { /* no memo: a fresh layout */ }
    var w = WORLD.layout(doc, memo, { readonly: true }), ix = EDGES.index(doc, w), T = WORLD.TILE || 16;
    var it = w.index[a.anchor], r = it.kind === 'biome' ? it.foot : it, box = document.querySelector(a.sel).getBoundingClientRect();
    var z = box.width / (r.w * T), lv = window.GARDEN.cam().level === 0 ? 0 : 1, set = ix.levels[lv].shown;
    if (set.length !== 1) return { err: 'want one channel at level ' + lv + ', got ' + set.map(function (c) { return c.id; }) };
    var c = set[0], W = EDGES.width(z, lv, T), p = EDGES.lanePts(c, T, W);
    var toS = function (q) { return [box.left + (q[0] - r.x * T) * z, box.top + (q[1] - r.y * T) * z]; };
    var cv = document.getElementById('cv'), cr = cv.getBoundingClientRect(), k = cv.width / cr.width, x = cv.getContext('2d');
    var px = function (s) { return x.getImageData(Math.round((s[0] - cr.left) * k), Math.round((s[1] - cr.top) * k), 1, 1).data; };
    var near = function (d, c0, tol) { return Math.hypot(d[0] - c0[0], d[1] - c0[1], d[2] - c0[2]) < tol; };
    var col = a.night ? [[0x5a, 0xb0, 0xff], [0xd8, 0xee, 0xff]] : [[0x3a, 0x8f, 0xd6], [0xbf, 0xe6, 0xff]];
    var water = function (s) {
      var d = px(s), w0 = col[0], r0 = col[1], v = [r0[0] - w0[0], r0[1] - w0[1], r0[2] - w0[2]], q = [d[0] - w0[0], d[1] - w0[1], d[2] - w0[2]];
      var t = Math.max(0, Math.min(1, (q[0] * v[0] + q[1] * v[1] + q[2] * v[2]) / (v[0] * v[0] + v[1] * v[1] + v[2] * v[2])));
      return Math.hypot(q[0] - t * v[0], q[1] - t * v[1], q[2] - t * v[2]) < 35;
    };
    var seen = 0, on = 0, off = 0, inView = function (s) { return s[0] > cr.left + 2 && s[1] > cr.top + 2 && s[0] < cr.right - 2 && s[1] < cr.bottom - 2; };
    for (var i = 0; i + 1 < p.length; i++) {
      var A = p[i], B = p[i + 1], h = A[1] === B[1], len = Math.abs(h ? B[0] - A[0] : B[1] - A[1]);
      if (len < 2 * W) continue;
      for (var t = 0.1; t < 0.95; t += 0.05) {
        var q = [A[0] + (B[0] - A[0]) * t, A[1] + (B[1] - A[1]) * t], s = toS(q), s2 = toS(h ? [q[0], q[1] + 3 * W] : [q[0] + 3 * W, q[1]]);
        if (!inView(s)) continue;
        if (it.kind !== 'biome' && s[0] > box.left - 8 && s[0] < box.right + 8 && s[1] > box.top - 8 && s[1] < box.bottom + 8) continue;   // the plot's ring over the channel's mouth
        var dq = q[0] - p[0][0] + q[1] - p[0][1];
        if (Math.abs(dq) < 3 * W) continue;   // the deck lies across the far end
        seen++; if (water(s)) on++; if (water(s2)) off++;
      }
    }
    /* the deck: a plank's colour a width and a half in from the far end, off the plank gaps */
    var e = p[0], f = p[1], hz = e[1] === f[1], dir = hz ? Math.sign(e[0] - f[0]) : Math.sign(e[1] - f[1]);
    var dk = toS(hz ? [e[0] - dir * W * 1.5, e[1] + W * 0.6] : [e[0] + W * 0.6, e[1] - dir * W * 1.5]);   // the middle plank
    var plank = a.night ? [0xb0, 0x7a, 0x3c] : [0x9a, 0x66, 0x30], deck = near(px(dk), plank, 40);
    /* the name: ink pixels in the box drawNames gives it at this camera, which is on the map the HUD leaves */
    var cam = window.GARDEN.cam(), safe = cam.safe;
    var nb = EDGES.drawNames({}, ix, { level: cam.level, z: cam.z, T: T, night: a.night, dpr: window.devicePixelRatio || 1, view: WORLD.viewRect(cam.cam, cam.vp, 0), safe: safe })[0];
    var n0 = toS([nb.x, nb.y]), n1 = toS([nb.x + nb.w, nb.y + nb.h]), sv = safe && [toS([safe.x, safe.y]), toS([safe.x + safe.w, safe.y + safe.h])];
    var ink = a.night ? [0xf2, 0xf2, 0xee] : [0x2a, 0x1a, 0x14], inked = 0;
    for (var yy = n0[1] + 2; yy < n1[1] - 2; yy += 1) for (var xx = n0[0] + 2; xx < n1[0] - 2; xx += 1) if (near(px([xx, yy]), ink, 30)) inked++;
    var nameOnMap = !!sv && n0[0] >= sv[0][0] - 1 && n0[1] >= sv[0][1] - 1 && n1[0] <= sv[1][0] + 1 && n1[1] <= sv[1][1] + 1;
    var reg = w.index['project:gardenio'].foot, far = toS(p[0]), footS = [toS([reg.x * T, reg.y * T]), toS([(reg.x + reg.w) * T, (reg.y + reg.h) * T])];
    var outside = far[0] < footS[0][0] || far[0] > footS[1][0] || far[1] < footS[0][1] || far[1] > footS[1][1];
    return { id: c.id, side: c.side, free: c.free, name: EDGES.nameOf(c), level: lv, z: z, seen: seen, on: on, off: off, deck: inView(toS(p[0])) ? deck : null, inked: inked,
      nameBox: [n0, n1], safe: sv, nameOnMap: nameOnMap, far: far, outside: outside, check: Math.abs(box.height / (r.h * T) - z) < 0.01 * z };
  }, { sel: sel, anchor: anchor, night: night });
}
function assertBridge(name, px) {
  assert.ok(!px.err, name + ': ' + px.err);
  assert.ok(px.check, name + ': the link box maps world to screen');
  assert.strictEqual(px.name, 'kbeads', name + ': the bridge is named for the project');
  assert.ok(px.free, name + ': it leaves by a side no other region lies beyond: ' + JSON.stringify(px));
  assert.ok(px.seen >= 3, name + ': too little of the channel on screen: ' + JSON.stringify(px));
  assert.ok(px.on >= px.seen * 0.7, name + ': water along the channel: ' + JSON.stringify(px));
  assert.ok(px.off <= px.seen * 0.2, name + ': no water beside it: ' + JSON.stringify(px));
  assert.ok(px.outside, name + ': its far end is past the region: ' + JSON.stringify(px));
  assert.ok(px.deck !== false, name + ': a deck across the far end, where it is on screen: ' + JSON.stringify(px));
  assert.ok(px.nameOnMap, name + ': its name is on the map the header, camera row and Commons leave: ' + JSON.stringify(px));
  assert.ok(px.inked >= 8, name + ': its name is drawn: ' + JSON.stringify(px));
}

async function walk(browser, name, opts) {
  var ctx = await skyCtx(browser, Object.assign({ reducedMotion: 'reduce' }, opts)), page = await ctx.newPage(), errors = [], night = opts.colorScheme === 'dark';
  page.on('console', function (m) { if (m.type() === 'error') errors.push(m.text()); });
  page.on('pageerror', function (e) { errors.push(String(e)); });

  // Project level, on plan A: a bridge from its plot out of the region
  await go(page, '?at=gardenio/gd-planA', /^Project/, PA);
  var pj = await bridgePixels(page, PA, 'gd-planA', night);
  await shot(page, name + '-project');
  assertBridge(name + ' Project', pj);
  assert.strictEqual(pj.id, 'bridge>gd-planA', name + ': the bridge is plan A\'s');
  var la = await page.getAttribute(PA, 'aria-label');
  assert.ok(la.indexOf('Blocked by a bead in kbeads.') >= 0, name + ': plan A\'s link says what it waits on: ' + la);
  assert.strictEqual(await page.$$eval('a[href*="gd-kbeadsa"]', function (l) { return l.length; }), 0, name + ': nothing links to the hidden bead');

  // World level: from the region's middle out past its edge
  var vs = await worldView(page, '?at=gardenio', BIOME);
  var wd = await bridgePixels(page, BIOME, 'project:gardenio', night);
  await shot(page, name + '-world');
  assertBridge(name + ' World', wd);
  assert.strictEqual(wd.id, 'bridge>project:gardenio', name + ': the region\'s bridge');
  var lb = await page.getAttribute(BIOME, 'aria-label');
  assert.ok(lb.indexOf('Irrigation channels: 1 step here waits on beads in kbeads.') >= 0, name + ': the region says it: ' + lb);
  await page.setViewportSize(vs);

  // the Bead level draws it too (a shot)
  await go(page, '?at=gd-A2c', /^Bead/, '#hots');
  await shot(page, name + '-bead');

  assert.deepStrictEqual(errors.filter(function (e) { return !/40[34]/.test(e); }), [], name + ': console errors');
  console.log(name + ': ok (Project ' + pj.side + ' ' + pj.on + '/' + pj.seen + ' water, name ' + pj.inked + ' px; World ' + wd.side + ' ' + wd.on + '/' + wd.seen + ', name ' + wd.inked + ' px)');
  await ctx.close();
}

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  try {
    await walk(browser, 'desktop-light', { viewport: { width: 1440, height: 900 } });
    await walk(browser, 'desktop-dark', { viewport: { width: 1440, height: 900 }, colorScheme: 'dark' });
    await walk(browser, 'phone-light', Object.assign({}, pw.devices['iPhone 12'], { viewport: { width: 390, height: 844 } }));
    await walk(browser, 'phone-dark', Object.assign({}, pw.devices['iPhone 12'], { viewport: { width: 390, height: 844 }, colorScheme: 'dark' }));
    console.log('bridge browser checks passed');
  } finally { await browser.close(); }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
