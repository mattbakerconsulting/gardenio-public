/* Browser check for "Go to plot" (3b R13), run from TestGoToPlotUnderChromium against /garden on cardWorld, whose
   POST /test/farm/scene?name=walkto moves g-ask-b's task from plan B to plan C.
   Usage: node goto_browser.js <base url> <node_modules dir>. GARDENHUB_SHOTS=<dir> saves a PNG after the jump, after the
   patch and after the second jump.

   Desktop: Go to plot on g-ask-b's Porch row puts the cursor inside plot B, the same camera as the bead view's "Show on the
   map", focus on the cursor (in view), and starts nothing: the camera strip has no Stop ride (the ride-along, World
   Phase 3 gd-gMMtrgIT1W, is retired). The patch moves g-ask-b to plot C; the cursor stays in plot B and the camera stays
   put (data waits for the viewer's next move, cursor-grid spec 3.4). Go to plot again puts the cursor inside plot C. No
   console errors. The walk itself is TestTransitionsUnderNode's.

   The field stands (28.12, plan gd-ryynhxINtp): inside gardenio's Growing field, g-ask-b (on plot B) stands solid, its link
   naming its plot, and the Barn holds no figure. One key, three stops (H-D21): a click on its figure opens its Glass with
   its transcript; Enter there closes it and flies to its plot figure at the map's full zoom, the cursor on plot B and focus on
   the figure; Enter again comes home to the same field figure, focused. No console errors. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert'), path = require('path');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));
var shots = process.env.GARDENHUB_SHOTS || '';
var PLAN_B = 'Second plan with a title long enough to cross its neighbours', PLAN_C = 'Third plan, also with a long title that would overlap';

async function shot(page, name) { if (shots) await page.screenshot({ path: path.join(shots, 'goto-' + name + '.png') }); }
function grid(page) { return page.evaluate(function () { return document.getElementById('hots').getAttribute('aria-label') || ''; }); }
function cam(page) { return page.evaluate(function () { return document.getElementById('hots').style.transform; }); }
function linked(page) { return page.evaluate(function () { return !!document.querySelector('#ghots [data-id="gardener:gd-gAskB"], #ghots a[href*="gd-gAskB"]'); }); }
async function inGrid(page, title) { await page.waitForFunction(function (t) { return (document.getElementById('hots').getAttribute('aria-label') || '').indexOf(t) === 0; }, title); }
async function settle(page) { var a; do { a = await cam(page); await page.waitForTimeout(300); } while (a !== await cam(page)); return a; }
async function goToPlot(page, title) {
  var go = page.locator('#pane-porch .gd-go[data-gid="gd-gAskB"]');
  if (!(await go.isVisible())) await page.click('#dbtn-porch');   // the Porch drawer
  await go.waitFor({ state: 'visible' });
  await go.click();
  await inGrid(page, title);
}

async function desktop(browser) {
  var ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } }), page = await ctx.newPage(), errors = [];
  page.on('console', function (m) { if (m.type() === 'error') errors.push(m.text()); });
  page.on('pageerror', function (e) { errors.push(String(e)); });
  await page.goto(url + '/garden');
  await page.waitForFunction(function () { return document.querySelector('#hots .hot.cur') && window.GARDEN.known('gd-gAskB').title === 'g-ask-b' && window.ANIM.opened() > 0; });
  assert.strictEqual(await page.locator('#cam-ride').count(), 0, 'the ride-along is retired: no Stop ride in the camera strip');

  // "Show on the map" first, from the top: the camera Go to plot must match
  await page.keyboard.press('0');
  await page.evaluate(function () { window.GARDEN.fly('gd-gAskB'); });
  await inGrid(page, PLAN_B);
  var shown = await settle(page);
  await page.keyboard.press('0');
  await page.waitForFunction(function (t) { return (document.getElementById('hots').getAttribute('aria-label') || '').indexOf(t) !== 0; }, PLAN_B);
  await settle(page);

  await goToPlot(page, PLAN_B);
  var before = await settle(page);
  assert.strictEqual(before, shown, 'Go to plot frames plot B as "Show on the map" does');
  await shot(page, 'jump');

  var fx = await page.evaluate(function () { var a = document.activeElement, r = a.getBoundingClientRect(); return { cur: a.matches('#hots .hot.cur'), top: r.top, bottom: r.bottom, h: innerHeight }; });
  assert.ok(fx.cur, 'focus goes to the cursor, as the outline\'s jumps hand it');
  assert.ok(fx.top >= 0 && fx.bottom <= fx.h, 'and the cursor is in view: ' + JSON.stringify(fx));

  // the patch moves g-ask-b's task to plan C: it leaves plot B, and nothing follows it
  assert.strictEqual((await page.request.post(url + '/test/farm/scene?name=walkto')).status(), 204, 'the farm published the patch');
  await page.waitForFunction(function () { var li = document.querySelector('#pane-porch .gd-go[data-gid="gd-gAskB"]'); li = li && li.closest('li'); return !!li && /gd-planC1a/.test(li.textContent); });
  await page.waitForFunction(function () { return !window.ANIM.list().length; });
  await shot(page, 'moved');
  assert.strictEqual((await grid(page)).indexOf(PLAN_B), 0, 'the cursor stays in plot B: nothing follows the gardener');
  assert.strictEqual(await settle(page), before, 'and the camera stays put');

  // Go to plot again: g-ask-b is on plot C now
  await goToPlot(page, PLAN_C);
  await settle(page);
  await shot(page, 'again');
  assert.strictEqual(await page.locator('#cam-ride').count(), 0, 'still nothing to stop');

  assert.strictEqual((await page.request.post(url + '/test/farm/scene?name=walkto&undo=1')).status(), 204, 'walkto undone');
  // the 204 says a patch went out, not that it carried the undo: the Porch row needs g-ask-b back on plot B (gd-pwH5haHGME)
  await page.waitForFunction(function () { var li = document.querySelector('#pane-porch .gd-go[data-gid="gd-gAskB"]'); li = li && li.closest('li'); return !!li && /gd-planB1a/.test(li.textContent); });
  assert.deepStrictEqual(errors, [], 'no console errors');
  await ctx.close();
}

/* Into gardenio's Growing field from ?at=gardenio/growing (the cursor on it, homestead-world-design.md 28.7): focus moves the
   cursor onto the field, Enter goes in. The cells can be up before the cursor opens (the farm landed before the viewport was
   measured), and Enter then goes nowhere: it waits until the field holds g-ask-b's stand and the open cursor is on it
   (gd-pwH5haHGME). */
var STAND = '#hots .hot[aria-label^="g-ask-b, "]', FIG = '#ghots a.w-gardener[href$="gd-gAskB"]';
function fieldCell() { return Array.prototype.filter.call(document.querySelectorAll('#hots .hot'), function (n) { return n._h && n._h.kind === 'zone' && n._h.node.zone === 'growing' && n._n && n._n.kids.indexOf('field:gd-gAskB') >= 0; })[0]; }
async function inField(page) {
  await page.goto(url + '/garden?at=gardenio/growing');
  await page.waitForFunction(fieldCell);
  var field = await page.evaluateHandle(fieldCell);
  await field.focus();
  await page.waitForFunction(function (b) { return b.isConnected && window.GARDEN.here() === b._n.key; }, field);   // the cursor is open, on the field
  await page.keyboard.press('Enter');
  await page.waitForSelector(STAND, { state: 'visible' });
}
async function stands(browser) {
  var ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } }), page = await ctx.newPage(), errors = [];
  page.on('console', function (m) { if (m.type() === 'error') errors.push(m.text()); });
  page.on('pageerror', function (e) { errors.push(String(e)); });
  await inField(page);
  var g = await page.evaluate(function (sel) { var n = document.querySelector(sel); return { id: n._h.id, kind: n._it && n._it.kind, ghost: !!(n._it && n._it.ghost), aria: n.getAttribute('aria-label'), zone: n._h.zone }; }, STAND);
  assert.deepStrictEqual([g.id, g.kind, g.ghost, g.zone], ['field:gd-gAskB', 'stand', false, 'agent'], 'g-ask-b stands in the field, solid');
  assert.ok(g.aria.indexOf(PLAN_B) >= 0 && /: watch in the Glass/.test(g.aria), 'its link names its plot and where it goes: ' + g.aria);
  var barn = await page.evaluate(function () { return Array.prototype.filter.call(document.querySelectorAll('#hots .hot'), function (n) { return n._it && n._it.kind === 'stand'; }).length; });
  assert.ok(barn >= 2, 'the field holds those at home and those out: ' + barn);
  await shot(page, 'field-stand');
  // stop 1: a click on the figure opens its Glass, its transcript under the terminal; its thread stays on its card and its Porch row
  await page.click(STAND);   // the first click puts the cursor on the figure (cursor-grid spec 3.6), the next acts
  await page.waitForFunction(function (sel) { return document.querySelector(sel).classList.contains('cur'); }, STAND);
  await page.click(STAND);
  await page.waitForFunction(function () { var p = document.getElementById('bead'); return !p.hidden && p.dataset.kind === 'glass' && !!document.querySelector('#bv-body .gl-tx') && !document.querySelector('#bv-body .gl-plot').hidden; });
  assert.strictEqual(await page.evaluate(function () { return document.getElementById('cursay').textContent; }), 'Watching g-ask-b in the Glass; Enter again goes to its plot');
  await shot(page, 'field-glass');
  // stop 2: Enter on the panel (its title holds focus) flies to the plot figure at full zoom, the cursor on plot B
  await page.focus('#bv-title');
  await page.keyboard.press('Enter');
  await page.waitForFunction(function (sel) { return document.getElementById('bead').hidden && document.activeElement && document.activeElement.matches(sel); }, FIG);
  await settle(page);
  var fx = await page.evaluate(function () {
    var a = document.activeElement, r = a.getBoundingClientRect(), c = document.querySelector('#hots .hot.cur');
    return { z: window.GARDEN.cam().z, plot: !!c && /bead=gd-planB$/.test(c.getAttribute('href') || ''), top: r.top, bottom: r.bottom, left: r.left, right: r.right, w: innerWidth, h: innerHeight, say: document.getElementById('cursay').textContent, aria: a.getAttribute('aria-label') };
  });
  assert.ok(fx.z === 4 && fx.plot && fx.top >= 0 && fx.bottom <= fx.h && fx.left >= 0 && fx.right <= fx.w, 'the plot figure, focused and in view at full zoom, the cursor on plot B: ' + JSON.stringify(fx));
  assert.ok(/^g-ask-b, .*, in the field of gardenio: go home/.test(fx.aria), 'its link says it goes home: ' + fx.aria);
  assert.ok(/^On .+, g-ask-b: Enter goes home to the yard$/.test(fx.say), fx.say);
  await shot(page, 'field-plot');
  // stop 3: Enter again comes home, the same field figure focused
  await page.keyboard.press('Enter');
  await page.waitForFunction(function (sel) { var a = document.activeElement; return !!a && a.matches(sel) && a.classList.contains('cur'); }, STAND);
  await settle(page);
  var home = await page.evaluate(function () { var a = document.activeElement, r = a.getBoundingClientRect(); return { id: a._h.id, top: r.top, bottom: r.bottom, h: innerHeight, at: new URLSearchParams(location.search).get('at') }; });
  assert.ok(home.id === 'field:gd-gAskB' && home.top >= 0 && home.bottom <= home.h, 'home on its field figure, in view: ' + JSON.stringify(home));
  await page.waitForFunction(function () { return new URLSearchParams(location.search).get('at') === 'gardenio/growing'; });   // the address names its field (28.12)
  await shot(page, 'field-home');
  assert.deepStrictEqual(errors, [], 'no console errors');
  await ctx.close();
}

/* The same three stops from a phone (390 x 844, touch; gd-a628FpJRO5), which has no Enter: Go's empty list on plan A names
   gardenio's home and, under it, its field stands, and a tap on g-ask-b's row lands the cursor on its field figure. Into the
   field from its ?at= by a tap, the cursor reaches every stand in reading order. Two taps on g-ask-b's figure open its Glass,
   the sheet over the map, the transcript folded under the terminal and the sign naming the Go to its plot button; that button
   closes the sheet and lands the camera on its plot figure at full zoom, in view and focused; a tap on that figure comes back
   to the strip with the same field figure focused. The aria names are the desktop's. A tap on the camera column's In over a
   gardener drawn under it pins no card. No console errors. */
async function phone(browser) {
  var ctx = await browser.newContext({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true, deviceScaleFactor: 1 }), page = await ctx.newPage(), errors = [];
  page.on('console', function (m) { if (m.type() === 'error') errors.push(m.text()); });
  page.on('pageerror', function (e) { errors.push(String(e)); });
  await page.goto(url + '/garden?at=gardenio/gd-planA');
  await page.waitForSelector('#cursor-bar .cb-me');
  await page.waitForFunction(function () { return window.GARDEN.known('gd-gAskB').title === 'g-ask-b'; });
  await page.locator('#cam-go').tap();
  var rows = await page.evaluate(function () { return Array.prototype.map.call(document.querySelectorAll('#jump-list .jump-it'), function (li) { return [li.querySelector('.jk').textContent, li.querySelector('.jt').textContent, li.classList.contains('jump-sub')]; }); });
  assert.deepStrictEqual(rows[0], ['home', 'gardenio home', false], 'Go leads with the cursor\'s home: ' + JSON.stringify(rows));
  var subs = rows.slice(1).filter(function (r) { return r[0] === 'gardener'; });
  assert.ok(subs.length === 5 && rows.slice(1, 6).every(function (r) { return r[0] === 'gardener' && r[2]; }), 'then five of its field stands, under it: ' + JSON.stringify(rows));
  assert.ok(rows.slice(6).some(function (r) { return r[0] === 'home'; }), 'and the other homes after them: ' + JSON.stringify(rows));
  await shot(page, 'phone-go');
  await page.locator('#jump-list .jump-it.jump-sub').first().tap();
  await page.waitForFunction(function () { var c = document.querySelector('#hots .hot.cur'); return document.getElementById('jump').hidden && c && c._n && /^field:/.test(c._n.id); });

  await page.goto(url + '/garden?at=gardenio/growing');
  await page.waitForFunction(fieldCell);
  await page.waitForFunction(function () { var c = document.querySelector('#hots .hot.cur'); return c && c._n && c._n.zone === 'growing' && c._n.kids.indexOf('field:gd-gAskB') >= 0; });
  await page.locator('#hots .hot.cur').tap();   // the cursor's cell: a tap goes in
  await page.waitForSelector(STAND, { state: 'visible' });
  await settle(page);
  var order = await page.evaluate(function () {
    return Array.prototype.filter.call(document.querySelectorAll('#hots .hot'), function (n) { return n._it && n._it.kind === 'stand'; }).map(function (n) { var r = n.getBoundingClientRect(); return { id: n._h.id, x: r.left, y: r.top, w: r.width, h: r.height, in: r.left >= 0 && r.right <= innerWidth }; });
  });
  assert.ok(order.length >= 2 && order.every(function (o) { return o.in && o.w >= 44 && o.h >= 44; }), 'every stand on the phone\'s width, a 44 px target: ' + JSON.stringify(order));
  assert.ok(order.every(function (o, i) { var p = order[i - 1]; return !p || o.y > p.y + 1 || (Math.abs(o.y - p.y) <= 1 && o.x > p.x); }), 'the grid holds the stands in reading order');
  await shot(page, 'phone-field');

  await page.tap(STAND);   // the first tap puts the cursor on the figure, the next acts
  await page.waitForFunction(function (sel) { return document.querySelector(sel).classList.contains('cur'); }, STAND);
  var aria = await page.getAttribute(STAND, 'aria-label');
  assert.ok(/^g-ask-b, working .*: watch in the Glass/.test(aria), 'the desktop\'s aria name: ' + aria);
  await page.tap(STAND);
  await page.waitForFunction(function () { var p = document.getElementById('bead'), t = document.querySelector('#bv-body .gl-tx'); return !p.hidden && p.dataset.kind === 'glass' && t && !/Loading/.test(t.textContent) && !document.querySelector('#bv-body .gl-plot').hidden; });
  var g = await page.evaluate(function () {
    var p = document.getElementById('bead').getBoundingClientRect(), tx = document.querySelector('#bv-body .gl-tx'), term = document.querySelector('#bv-body .gl-term'), b = document.querySelector('#bv-body .gl-plot').getBoundingClientRect();
    return { w: p.width, shut: tx.classList.contains('shut'), under: !!(term.compareDocumentPosition(tx) & Node.DOCUMENT_POSITION_FOLLOWING), btn: b.height, say: document.getElementById('cursay').textContent };
  });
  assert.ok(g.w === 390 && g.shut && g.under && g.btn >= 44, 'the Glass is the phone\'s sheet, its transcript folded under the terminal, Go to its plot 44 px: ' + JSON.stringify(g));
  assert.strictEqual(g.say, 'Watching g-ask-b in the Glass; Go to its plot takes you there', 'a phone has no Enter: the sign names the button');
  await shot(page, 'phone-glass');

  await page.locator('#bv-body .gl-plot').scrollIntoViewIfNeeded();
  await page.locator('#bv-body .gl-plot').tap();
  await page.waitForFunction(function (sel) { var a = document.activeElement; return document.getElementById('bead').hidden && a && a.matches(sel); }, FIG);
  await settle(page);
  var fx = await page.evaluate(function () {
    var a = document.activeElement, r = a.getBoundingClientRect(), c = document.querySelector('#hots .hot.cur');
    return { z: window.GARDEN.cam().z, plot: !!c && /bead=gd-planB$/.test(c.getAttribute('href') || ''), top: r.top, bottom: r.bottom, left: r.left, right: r.right, w: innerWidth, h: innerHeight, say: document.getElementById('cursay').textContent, aria: a.getAttribute('aria-label') };
  });
  assert.ok(fx.z === 4 && fx.plot && fx.top >= 0 && fx.bottom <= fx.h && fx.left >= 0 && fx.right <= fx.w, 'the plot figure, focused and in view at full zoom, the cursor on plot B: ' + JSON.stringify(fx));
  assert.ok(/^g-ask-b, .*, in the field of gardenio: go home/.test(fx.aria), 'the desktop\'s aria name: ' + fx.aria);
  assert.ok(/^On .+, g-ask-b: tap it to go home to the yard$/.test(fx.say), fx.say);
  await shot(page, 'phone-plot');

  await page.tap(FIG);
  await page.waitForFunction(function (sel) { var a = document.activeElement; return !!a && a.matches(sel) && a.classList.contains('cur'); }, STAND);
  await settle(page);
  var home = await page.evaluate(function () { var a = document.activeElement, r = a.getBoundingClientRect(); return { id: a._h.id, top: r.top, bottom: r.bottom, h: innerHeight }; });
  assert.ok(home.id === 'field:gd-gAskB' && home.top >= 0 && home.bottom <= home.h, 'home on its field figure, in view: ' + JSON.stringify(home));
  await page.waitForFunction(function () { return new URLSearchParams(location.search).get('at') === 'gardenio/growing'; });
  await shot(page, 'phone-home');

  // a tap on a control over the map is no tap on the gardener drawn under it: In over g-ask-b's plot figure pins no card
  await page.evaluate(function () { var a = document.activeElement; if (a && a.blur) a.blur(); });
  await page.waitForFunction(function () { var c = document.querySelector('.wl-card'); return !c || c.hidden; });
  var under = await page.evaluate(function () {
    var G = window.GARDEN.cam(), h = G.ghots.filter(function (x) { return x.kind === 'gardener' && x.g && x.g.id === 'gd-gAskB'; })[0];
    var b = document.getElementById('cam-in').getBoundingClientRect(), bd = document.getElementById('board').getBoundingClientRect(), z = G.cam.z;
    var sx = b.left + b.width / 2 - bd.left, sy = b.top + b.height / 2 - bd.top, wx = h.x + h.w / 2, wy = h.y + h.h / 2;
    window.GARDEN.look({ cx: wx - (sx - G.vp.w / 2) / z, cy: wy - (sy - G.vp.h / 2) / z, z: z });
    var c = window.GARDEN.cam(), w = window.WORLD.toWorld(c.cam, c.vp, sx, sy);
    return window.WORLD.hit(h, { x: w.x, y: w.y, w: 0, h: 0 });
  });
  assert.ok(under, 'g-ask-b\'s plot figure is drawn under the In button');
  await page.locator('#cam-in').tap();
  await page.waitForTimeout(400);
  assert.ok(await page.evaluate(function () { var c = document.querySelector('.wl-card'); return !c || c.hidden; }), 'the In button\'s tap pins no gardener\'s card');
  assert.deepStrictEqual(errors, [], 'no console errors');
  await ctx.close();
}

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  try { await desktop(browser); await stands(browser); await phone(browser); console.log('go to plot browser checks passed'); }
  finally { await browser.close(); }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
