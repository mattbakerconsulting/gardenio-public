/* Browser check for cross-project irrigation channels (gd-CNhEh30Ykf), run from TestEdgesUnderChromium against a live /garden
   on an httptest server with the card fixture farm plus one edge: plan A's bug gd-A2c (gardenio) waits on gd-kbeadsa1a (kbeads).
   Usage: node edges_browser.js <base url> <node_modules dir>. GARDENHUB_SHOTS=<dir> saves the screenshots.

   Desktop (1440x900) and phone (390x844), light and dark, at the Project and World levels (?at= puts the cursor there; z= is
   ignored, and depth 0 frames the World level on a board made small enough for it): the channel's pixels are on the
   canvas where edges.js routes it (water along its middle, not beside it), computed from the page's own WORLD and EDGES and
   placed on screen through a hot link's box, and at the Project level the point where it crosses into plan A's region is on
   screen and clear of the camera buttons (gd-pyrowrsfDF). In words: plan A's plot link says "Blocked by gd-kbeadsa1a in kbeads", the
   kbeads plot's says "Blocks gd-A2c in gardenio", the gardenio region's link sums its channels (the info card that also
   listed them is gone, cursor-grid spec N-D14), the bead view's "Waits on" row says "in kbeads", and the legend has a row for the channel. Zero console errors. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert'), path = require('path');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));
var PLOT = '#hots a.w-plot', PA = PLOT + '[href="/garden?bead=gd-planA"]', PK = PLOT + '[href="/garden?bead=gd-kbeadsa"]';
var BIOME = '#hots .w-biome[aria-label^="gardenio:"]';

/* Every context pins the sky (web/sky.js) to its emulated colour scheme, as neighbours_browser.js does. */
function skyCtx(browser, opts) {
  return browser.newContext(opts).then(function (ctx) {
    return ctx.addInitScript(function (s) { try { localStorage.setItem('garden.sky', s); } catch (e) { /* no storage: the emulated scheme decides */ } }, opts.colorScheme === 'dark' ? 'night' : 'day').then(function () { return ctx; });
  });
}
async function shot(page, name) { if (process.env.GARDENHUB_SHOTS) await page.screenshot({ path: path.join(process.env.GARDENHUB_SHOTS, 'edges-' + name + '.png') }); }
/* The opening cursor's grid: a cell's control ('#hots .hot') once the farm document is laid out on a sized board and the slots'
   model is in. Not '#hots a': the opening cursor is the saved path, or depth 0 when the walk saved none, and depth 0's places
   are buttons (gd-4ADVGIiIcf, gd-BL2JQbsfyy: "The farm: 5 places", rows=2 links=0, cursor null). A timeout says which of
   those was missing, with the page's console errors. */
async function openingHots(page, name, errors) {
  try { await page.waitForSelector('#hots .hot', { state: 'attached' }); }
  catch (e) {
    var s = await page.evaluate(function () {
      var g = window.GARDEN && window.GARDEN.cam(), h = document.getElementById('hots'), b = document.getElementById('board'), ps = document.getElementById('plans-slot'), cur = null;
      try { cur = localStorage.getItem('garden.cursor.v1'); } catch (x) { /* no storage */ }
      return {
        href: location.href, ready: document.readyState, cam: g ? { level: g.level, z: g.z, vp: g.vp } : null,
        board: b ? b.clientWidth + 'x' + b.clientHeight : null, laidOut: !!(window.GARDEN && window.GARDEN.known('gd-planA').onMap),
        hots: h ? { rows: h.children.length, links: h.querySelectorAll('a').length, controls: h.querySelectorAll('.hot').length, label: h.getAttribute('aria-label') } : null,
        plans: ps ? ps.textContent.slice(0, 200) : null, cursor: cur
      };
    }).catch(function (x) { return 'unreadable: ' + x.message; });
    throw new Error(name + ': no #hots .hot after goto(/garden): ' + JSON.stringify(s) + ' console: ' + JSON.stringify(errors) + '\n' + e.message);
  }
}
async function frames(page) { await page.evaluate(function () { return new Promise(function (r) { requestAnimationFrame(function () { requestAnimationFrame(r); }); }); }); }
async function go(page, q, lvl, sel) {
  await page.goto(url + '/garden' + q);
  await page.waitForFunction(function (l) { return !!window.GARDEN && new RegExp(l).test(WORLD.LEVEL_NAMES[window.GARDEN.cam().level]); }, lvl.source);
  await page.waitForSelector(sel, { state: 'attached' });
  await frames(page);
}
/* The farm framed at the World level, as the cursor frames it (the walk does not steer the free camera): depth 0 frames every region, at the
   World level on a board small enough for that, so the board is made smaller until it is; the caller restores the size. */
async function worldView(page, q, sel) {
  var vs = page.viewportSize(), w = vs.width, h = vs.height;
  for (var i = 0; i < 4; i++) {
    await go(page, q, /^(World|Project)/, sel);
    if (await page.evaluate(function () { return window.GARDEN.cam().level; }) === 0) return vs;
    w = Math.round(w * 0.75); h = Math.round(h * 0.75);
    await page.setViewportSize({ width: w, height: h });
  }
  assert.fail('depth 0 never framed the World level: ' + JSON.stringify(await page.evaluate(function () { return window.GARDEN.cam(); })));
}

/* The channel at this level, recomputed in the page (its farm document, layout memo, WORLD and EDGES) and put on screen through
   the hot link sel, whose world rect is anchor's (a plot, or a region's foot). Then the canvas under points along the channel's
   middle, and the same points pushed three widths aside: how many are water. */
async function channelPixels(page, sel, anchor, night) {
  return page.evaluate(async function (a) {
    var doc = await (await fetch('/garden/farm', { headers: { Accept: 'application/json' } })).json();
    var memo = null; try { memo = JSON.parse(localStorage.getItem('garden.world.v1')); } catch (e) { /* no memo: a fresh layout */ }
    var w = WORLD.layout(doc, memo, { readonly: true }), ix = EDGES.index(doc, w), T = WORLD.TILE || 16;
    var it = w.index[a.anchor], r = it.kind === 'biome' ? it.foot : it, box = document.querySelector(a.sel).getBoundingClientRect();
    var z = box.width / (r.w * T), lv = window.GARDEN.cam().level === 0 ? 0 : 1;
    var set = ix.levels[lv].shown;
    if (set.length !== 1) return { err: 'want one channel at level ' + lv + ', got ' + set.length + ': ' + set.map(function (c) { return c.id; }) };
    var c = set[0], W = EDGES.width(z, lv, T), p = EDGES.lanePts(c, T, W);
    var toS = function (q) { return [box.left + (q[0] - r.x * T) * z, box.top + (q[1] - r.y * T) * z]; };
    var cv = document.getElementById('cv'), cr = cv.getBoundingClientRect(), k = cv.width / cr.width, x = cv.getContext('2d');
    var col = a.night ? [[0x5a, 0xb0, 0xff], [0xd8, 0xee, 0xff]] : [[0x3a, 0x8f, 0xd6], [0xbf, 0xe6, 0xff]];
    var water = function (s) {
      var d = x.getImageData(Math.round((s[0] - cr.left) * k), Math.round((s[1] - cr.top) * k), 1, 1).data;
      /* water, a ripple, or the blend of the two a fractional ripple edge leaves: near the colour line from one to the other */
      var w = col[0], r = col[1], v = [r[0] - w[0], r[1] - w[1], r[2] - w[2]], q = [d[0] - w[0], d[1] - w[1], d[2] - w[2]];
      var t = Math.max(0, Math.min(1, (q[0] * v[0] + q[1] * v[1] + q[2] * v[2]) / (v[0] * v[0] + v[1] * v[1] + v[2] * v[2])));
      return Math.hypot(q[0] - t * v[0], q[1] - t * v[1], q[2] - t * v[2]) < 35;
    };
    var seen = 0, on = 0, off = 0;
    for (var i = 0; i + 1 < p.length; i++) {
      var A = p[i], B = p[i + 1], h = A[1] === B[1], len = Math.abs(h ? B[0] - A[0] : B[1] - A[1]);
      if (len < 2 * W) continue;
      for (var t = 0.1; t < 0.95; t += 0.05) {
        var q = [A[0] + (B[0] - A[0]) * t, A[1] + (B[1] - A[1]) * t], s = toS(q), s2 = toS(h ? [q[0], q[1] + 3 * W] : [q[0] + 3 * W, q[1]]);
        if (s[0] < cr.left + 2 || s[1] < cr.top + 2 || s[0] > cr.right - 2 || s[1] > cr.bottom - 2) continue;
        /* a plot's needs-you ring is painted over the channel's mouth at its gate; at the phone's framing (0.22x) it is a
           quarter of what is on screen, so the samples under the plot's box (and the ring around it) do not count */
        if (it.kind !== 'biome' && s[0] > box.left - 8 && s[0] < box.right + 8 && s[1] > box.top - 8 && s[1] < box.bottom + 8) continue;
        seen++; if (water(s)) on++; if (water(s2)) off++;
      }
    }
    /* where the channel crosses into the waiting step's region (gd-pyrowrsfDF): the last run that comes in from outside the
       region's foot, cut at the foot's edge, on screen; and the camera buttons' boxes, which must leave that point in view */
    var R = { x: c.dstReg.x * T, y: c.dstReg.y * T, w: c.dstReg.w * T, h: c.dstReg.h * T }, exit = null;
    var inR = function (q) { return q[0] >= R.x && q[0] <= R.x + R.w && q[1] >= R.y && q[1] <= R.y + R.h; };
    for (var e = p.length - 1; e > 0 && !exit; e--) {
      var P = p[e - 1], Q = p[e];
      if (!inR(Q) || inR(P)) continue;
      exit = P[1] === Q[1] ? [P[0] < R.x ? R.x : R.x + R.w, Q[1]] : [Q[0], P[1] < R.y ? R.y : R.y + R.h];
    }
    var cams = Array.prototype.map.call(document.querySelectorAll('.cam .cb'), function (b) { var q = b.getBoundingClientRect(); return { id: b.id, x: q.left, y: q.top, r: q.right, b: q.bottom }; })
      .filter(function (q) { return q.r > q.x && q.b > q.y; });
    return { id: c.id, level: lv, z: z, seen: seen, on: on, off: off, check: Math.abs(box.height / (r.h * T) - z) < 0.01 * z,
      exit: exit && toS(exit), view: { x: cr.left, y: cr.top, r: cr.right, b: cr.bottom }, cams: cams };
  }, { sel: sel, anchor: anchor, night: night });
}
function assertChannel(name, px) {
  assert.ok(!px.err, name + ': ' + px.err);
  assert.ok(px.check, name + ': the link box maps world to screen');
  assert.ok(px.seen >= 4, name + ': too little of the channel on screen: ' + JSON.stringify(px));
  assert.ok(px.on >= px.seen * 0.7, name + ': water along the channel: ' + JSON.stringify(px));
  assert.ok(px.off <= px.seen * 0.2, name + ': no water beside it: ' + JSON.stringify(px));
}
/* The point where the channel crosses into plan A's region is on the map, not under the camera buttons: on a phone they are a
   column on the right edge, the side the channel from kbeads comes in on (gd-pyrowrsfDF). */
function assertExitClear(name, px) {
  var x = px.exit, v = px.view;
  assert.ok(x, name + ': the channel crosses the region\'s edge: ' + JSON.stringify(px));
  assert.ok(x[0] >= v.x + 2 && x[0] <= v.r - 2 && x[1] >= v.y + 2 && x[1] <= v.b - 2, name + ': where the channel leaves the region is on screen: ' + JSON.stringify(x));
  px.cams.forEach(function (q) {
    assert.ok(!(x[0] > q.x - 4 && x[0] < q.r + 4 && x[1] > q.y - 4 && x[1] < q.b + 4), name + ': ' + q.id + ' covers where the channel leaves the region: ' + JSON.stringify({ exit: x, button: q }));
  });
}

async function walk(browser, name, opts) {
  var ctx = await skyCtx(browser, Object.assign({ reducedMotion: 'reduce' }, opts)), page = await ctx.newPage(), errors = [], night = opts.colorScheme === 'dark';
  page.on('console', function (m) { if (m.type() === 'error') errors.push(m.text()); });
  page.on('pageerror', function (e) { errors.push(String(e)); });

  // Project level, on plan A: plot to plot
  await go(page, '?at=gardenio/gd-planA', /^Project/, PA);
  var pj = await channelPixels(page, PA, 'gd-planA', night);
  await shot(page, name + '-project');
  assertChannel(name + ' Project', pj);
  assertExitClear(name + ' Project', pj);
  assert.strictEqual(pj.id, 'gd-kbeadsa>gd-planA', name + ': the channel runs from the blocker\'s plot to plan A');
  var la = await page.getAttribute(PA, 'aria-label');
  assert.ok(la.indexOf('Blocked by gd-kbeadsa1a in kbeads.') >= 0, name + ': plan A\'s link says what it waits on: ' + la);
  await go(page, '?at=kbeads/gd-kbeadsa', /^(Project|Plan)/, PK);
  var lk = await page.getAttribute(PK, 'aria-label');
  assert.ok(lk.indexOf('Blocks gd-A2c in gardenio.') >= 0, name + ': the blocker\'s plot says what waits on it: ' + lk);

  // World level: region to region. The cursor on the gardenio region at depth 0 frames every region (worldView).
  var vs = await worldView(page, '?at=gardenio', BIOME);
  var wd = await channelPixels(page, BIOME, 'project:gardenio', night);
  await shot(page, name + '-world');
  assertChannel(name + ' World', wd);
  assert.strictEqual(wd.id, 'project:kbeads>project:gardenio', name + ': region to region');
  var lb = await page.getAttribute(BIOME, 'aria-label');
  assert.ok(lb.indexOf('Irrigation channels: 1 step here waits on beads in kbeads.') >= 0, name + ': the region says it: ' + lb);
  await page.setViewportSize(vs);

  // the Bead level draws it too (a shot), and the bead view says the project
  await go(page, '?at=gd-A2c', /^Bead/, '#hots');
  await shot(page, name + '-bead');
  await page.goto(url + '/garden?bead=gd-A2c');
  await page.waitForSelector('.bv-rels li', { state: 'visible' });
  var rows = await page.$$eval('.bv-rels li', function (l) { return l.map(function (e) { return e.textContent; }); });
  assert.ok(rows.some(function (r) { return r.indexOf('gd-kbeadsa1a') >= 0 && / · in kbeads$/.test(r); }), name + ': the bead view says the blocker is in kbeads: ' + rows);
  await shot(page, name + '-beadview');

  // the legend
  await page.goto(url + '/garden');
  await openingHots(page, name, errors);   // the opening cursor's grid, whatever its depth
  await page.click('#cam-menu'); await page.click('#cam-help');   // Legend & keys is in the camera's Menu (3b R9)
  var leg = await page.$$eval('#legend li', function (l) { return l.map(function (e) { return e.textContent; }); });
  assert.ok(leg.indexOf('Irrigation channel: a step waits on a bead in another project; the gate is at the end that waits') >= 0, name + ': a legend row: ' + leg);

  assert.deepStrictEqual(errors.filter(function (e) { return !/404/.test(e); }), [], name + ': console errors');
  console.log(name + ': ok (Project ' + pj.on + '/' + pj.seen + ' water, ' + pj.off + ' beside; World ' + wd.on + '/' + wd.seen + ', ' + wd.off + ')');
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
    console.log('edges browser checks passed');
  } finally { await browser.close(); }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
