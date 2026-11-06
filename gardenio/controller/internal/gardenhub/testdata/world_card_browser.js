/* Browser check for the cursor's signs on the map (cursor-grid spec section 6, build step 5; the info card of gd-D3G9dMA1TU is
   gone, N-D14), run from TestCardUnderChromium against a live /garden on an httptest server with the fixture farm.
   Usage: node world_card_browser.js <base url> <node_modules dir>.

   The cursor's Thing has no plate on the map (gd-zDdiA3HBh5): the title bar names it, its lineage, whole title and status line.
   Opening (gd-N0AEWzgVsg): /garden frames every project; no plate beside the cursor's Thing, and the title bar names it,
   clear of the camera buttons, through a ledger refresh. On a phone, fully zoomed out, no board lists the cursor project's
   plans over the map; its link reads them (gd-OOt8Mx2Mps).
   Desktop (mouse, reduced motion): the title bar follows the cursor (focus, arrows), never the mouse; it reads the Thing's
   status line, the need words first; no plan title is on the map; a farm refresh keeps it; Escape ascends and the bar names
   the project.
   Phone (Pixel 5, touch): a tap on a plot moves the cursor there and the bar follows; a tap on the bar opens the Thing in the
   bead view (spec 7 Start); the bar is a strip of two rows at most on the tab bar, under the header.
   Camera strip (gd-OrAAMhVNPD): on a phone and at 820x1180 touch every camera button, In and Out with them, is tappable, clear of the tab bar.
   Phases (gd-kIUS2y1z7I under the cursor): inside a nine-phase plan the title bar names each phase in turn, whole, as the
   arrows walk the beds, in readable ink; the plot's accessible name lists every phase.
   The bead view (gd-BdhucNqv0i): a click on a plot opens the panel inside the map window with the server's description and
   comments, the address bar carries ?bead=, Escape closes it, a reload of ?bead= reopens it, a cross-reference in the panel
   opens that bead and Back returns to the first. No request (page or fetch) ever leaves /garden. Zero console errors.
   Mailbox and Porch on a phone (gd-e3LWfAZczq): a bead opened from either shows the farm; closing returns to that screen. */
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

var PLOT = '#hots a.w-plot', A = PLOT + '[href="/garden?bead=gd-planA"]', B = PLOT + '[href="/garden?bead=gd-planB"]';

function overlaps(a, b) { return a.x < b.x + b.width && b.x < a.x + a.width && a.y < b.y + b.height && b.y < a.y + a.height; }
function inside(a, b) { return a.x >= b.x - 1 && a.y >= b.y - 1 && a.x + a.width <= b.x + b.width + 1 && a.y + a.height <= b.y + b.height + 1; }

async function open(ctx, path) {
  var page = await ctx.newPage(), errors = [];
  page.on('console', function (m) { if (m.type() === 'error') errors.push(m.text()); });
  page.on('pageerror', function (e) { errors.push(String(e)); });
  // every request on this origin must stay inside the garden: /garden, /garden?..., /garden/*; the one exception is the
  // in-place answer, which posts to the viewer's own decision / escalation endpoints (garden.js act())
  page.on('request', function (r) { var u = new URL(r.url()); if (u.origin === new URL(url).origin && u.pathname !== '/garden' && u.pathname.indexOf('/garden/') !== 0 && !(r.method() === 'POST' && /^\/b\/[^/]+\/(decision\/resolve|escalation\/(approve|reject))$/.test(u.pathname))) errors.push('left the garden: ' + u.pathname + u.search); });
  await page.goto(url + path);
  await page.waitForSelector(A, { state: 'attached' });
  await page.waitForFunction(function () { return window.GARDEN && window.GARDEN.cam().level === 1; });
  // The board fills the viewport since gd-rdMNXor7Ip; scrolling it to the top is a no-op there and keeps the phone's fixed
  // tab bar off the plots should a layout ever put the board further down the page again.
  await page.evaluate(function () { document.getElementById('board').scrollIntoView({ block: 'start' }); });
  return { page: page, errors: errors };
}
async function box(page, sel) { var b = await page.locator(sel).first().boundingBox(); assert.ok(b, sel + ' has no box'); return b; }
/* The title bar (gd-zDdiA3HBh5) and the plates on the map: n, the plates beside the cursor's Thing (any sign but a child label
   or a World header: none); all, the signs of any tier #lbls holds; the bar's text, its title (l1, ttl, the
   cursor Thing's whole title), its lineage (pre, the parts before the title joined " › "), its status words (st), the title's
   box and the bar's. */
async function full(page) {
  return page.evaluate(function () {
    var b = document.getElementById('cursor-bar'), me = b.querySelector('.cb-me'), r = me ? me.getBoundingClientRect() : null, br = b.getBoundingClientRect();
    return { n: document.querySelectorAll('#lbls .sign:not(.child):not(.head)').length, all: document.querySelectorAll('#lbls .sign').length, text: b.textContent,
      l1: me ? me.textContent : '', ttl: me ? me.textContent : '', pre: [].filter.call(b.querySelectorAll('.cb-p'), function (a) { return a !== me; }).map(function (a) { return a.textContent; }).join(' › '),
      st: (b.querySelector('.cb-sw') || {}).textContent || '', box: r ? { x: r.x, y: r.y, width: r.width, height: r.height } : null, bar: { x: br.x, y: br.y, width: br.width, height: br.height } };
  });
}
/* aria-expanded on sel once it reads 'true', or as it is after 5 s for the caller's assertion to report. Plans and Legend & keys
   set it on their drawer's toggle event, a task after the tap, so a read straight after page.tap() can still see 'false'
   (gd-k99rrYKtMp): the state is waited for, never read on the tap. */
async function expanded(page, sel) {
  await page.waitForFunction(function (s) { var e = document.querySelector(s); return e && e.getAttribute('aria-expanded') === 'true'; }, sel, { timeout: 5000 }).catch(function () {});
  return page.getAttribute(sel, 'aria-expanded');
}
/* A timer task, then two frames. A cursor move's signs are refreshed in a setTimeout(0) task of their own (gd-fsuxCGGbZr), and
   timers of equal delay run in the order they were set, so a label read after this sees the move's label, not the last one. */
async function frames(page) { await page.evaluate(function () { return new Promise(function (r) { setTimeout(function () { requestAnimationFrame(function () { requestAnimationFrame(r); }); }, 0); }); }); }

/* The opening view (gd-N0AEWzgVsg): /garden with no address frames every project. After a ledger refresh there is no plate
   beside the cursor's Thing and the title bar names it, clear of the camera buttons; the header counts in words that agree
   with the number. GARDENHUB_SHOTS=<dir> saves a screenshot of it. */
async function opening(browser, opts, name) {
  var ctx = await skyCtx(browser, Object.assign({ reducedMotion: 'reduce' }, opts)), page = await ctx.newPage(), errors = [];
  page.on('pageerror', function (e) { errors.push(String(e)); });
  await page.goto(url + '/garden');
  await page.waitForSelector('#cursor-bar .cb-me');
  // a ledger refresh rebuilds the strips' links and refills the signs: the scope buttons run one, twice, then two frames
  await page.evaluate(function () { document.getElementById('b-mine').click(); document.getElementById('b-all').click(); });
  await frames(page);
  var got = await page.evaluate(async function () {
    var doc = await (await fetch('/garden/farm', { headers: { Accept: 'application/json' } })).json();
    var w = WORLD.layout(doc, null, { readonly: true }), T = WORLD.TILE, vw = innerWidth, vh = innerHeight;
    // the camera, from the transform camChanged puts on #hots: translate(tx, ty) scale(z), relative to the board
    var m = document.getElementById('hots').style.transform.match(/translate\(([-\d.e]+)px, ([-\d.e]+)px\) scale\(([\d.e-]+)\)/);
    var bd = document.getElementById('board').getBoundingClientRect(), top = document.querySelector('header.crumb').getBoundingClientRect().bottom;
    var tx = +m[1], ty = +m[2], z = +m[3];
    var onScreen = function (x, y, ww, hh) { var sx = bd.left + tx + x * T * z, sy = bd.top + ty + y * T * z; return sx >= 0 && sx + ww * T * z <= vw && sy >= top && sy + hh * T * z <= vh; };
    var cams = Array.prototype.map.call(document.querySelectorAll('.cam > *'), function (e) { return e.getBoundingClientRect().toJSON(); }).filter(function (r) { return r.width > 0; });
    var cur = document.querySelector('#hots .hot.cur');
    var tall = Array.prototype.filter.call(document.querySelectorAll('#lbls .sign'), function (e) { return e.getBoundingClientRect().height > 30; }).map(function (e) { return e.className + ' ' + Math.round(e.getBoundingClientRect().height); });
    return { level: WORLD.LEVEL_NAMES[window.GARDEN.cam().level] + ' · ' + Math.round(window.GARDEN.cam().z * 100) / 100, head: document.getElementById('sub').textContent, cams: cams, top: top, cur: cur ? cur.getAttribute('aria-label') : '',
      desc: cur ? cur.getAttribute('aria-description') || '' : '', tall: tall,
      at: (document.querySelector('#cursor-bar .cb-me') || { dataset: {} }).dataset.hot || '',
      biomes: w.biomes.map(function (b) { return { id: b.id, name: b.name, c: b.cell.c, on: onScreen(b.sign.x, b.sign.y, b.sign.w, b.sign.h) }; }) };
  });
  var f = await full(page), bd = await box(page, '#board');
  if (process.env.GARDENHUB_SHOTS) await page.screenshot({ path: require('path').join(process.env.GARDENHUB_SHOTS, 'opening-' + name + '.png') });
  var on = got.biomes.filter(function (b) { return b.on; });
  console.log(name + ' opening at ' + got.level + ': ' + f.all + ' signs; ' + on.length + ' of ' + got.biomes.length + ' projects on screen; "' + f.text + '"');
  assert.ok(got.biomes.length >= 4, 'the fixture has four projects: ' + JSON.stringify(got.biomes));
  /* the opening cursor is depth 0 on the first project (cursor-grid spec 3.5), framed on the lattice's used bounds (4.1): every
     project on screen, at the Project level on a desktop and the World level on a phone. The fixture's lattice is wider than a
     390 px phone holds at Z_MIN, so the phone frames a window of it (4.1, 12.3): whole columns, the cursor's among them, four of
     the five (gd-KO6GFI4Eq6) */
  assert.ok(name === 'desktop' ? /^Project/.test(got.level) : /^World/.test(got.level), name + ': the opening frames the lattice: ' + got.level);
  var cols = function (l) { return l.map(function (b) { return b.c; }).filter(function (c, i, a) { return a.indexOf(c) === i; }).sort(function (a, b) { return a - b; }); };
  var inCols = got.biomes.filter(function (b) { return cols(on).indexOf(b.c) >= 0; }), oc = cols(on);
  assert.ok(name === 'desktop' ? on.length === got.biomes.length : inCols.length === on.length && oc.length >= 4 && oc[oc.length - 1] - oc[0] === oc.length - 1 && on.some(function (b) { return b.id === got.at; }),
    name + ': projects on screen at the opening (' + got.at + '): ' + JSON.stringify(got.biomes));
  assert.strictEqual(f.n, 0, name + ': no plate beside the cursor\'s Thing after a ledger refresh');
  assert.deepStrictEqual(got.tall, [], name + ': every sign on the map is one line, nothing lists plans over it');
  if (name !== 'desktop') assert.ok(/% done/.test(got.desc), name + ': the cursor project\'s link reads its plans: ' + got.desc);
  assert.ok(f.l1 && got.cur.indexOf(f.l1) === 0, name + ': the title bar names the cursor\'s Thing: ' + JSON.stringify([f.text, got.cur]));
  assert.ok(inside(f.box, bd) && f.box.y >= got.top - 1, name + ': the bar\'s title is inside the map, under the header: ' + JSON.stringify([f.box, bd, got.top]));
  got.cams.forEach(function (c) { var r = f.box; assert.ok(r.x + r.width <= c.left || r.x >= c.right || r.y + r.height <= c.top || r.y >= c.bottom, name + ': the bar\'s title runs under a camera button: ' + JSON.stringify([r, c])); });
  assert.ok(!/(^|\D)1 gardeners/.test(got.head) && /gardeners?/.test(got.head), name + ' header pluralises: ' + got.head);
  assert.deepStrictEqual(errors, [], name + ' opening errors');
  await ctx.close();
}

/* The camera strip on touch layouts (gd-OrAAMhVNPD, gd-wAN7P4Armw; cursor-grid spec 4.3): at 390x844 and at 820x1180 it is
   In, Out, Go and the Menu (3b R8), each a 44 px target, clear of the tab bar and of the title bar on it. Every camera
   button, and with the Menu open every item in it, is the top element at its centre; a tap on Outline in the Menu opens the
   outline and closes the Menu. */
function topAtCentre(sel) {
  return Array.prototype.filter.call(document.querySelectorAll(sel), function (b) {
    var r = b.getBoundingClientRect();
    if (!r.width) return false;   // Snap is hidden for a viewer who cannot post
    return !b.contains(document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2));
  }).map(function (b) { return b.id; });
}
async function cameraClear(browser, opts, name) {
  var ctx = await skyCtx(browser, Object.assign({ reducedMotion: 'reduce' }, opts)), page = await ctx.newPage();
  await page.goto(url + '/garden');
  await page.waitForSelector('#cam-menu', { state: 'visible' });
  assert.deepStrictEqual(await page.evaluate(topAtCentre, '.cam .cb'), [], name + ': camera buttons under another control');
  assert.deepStrictEqual(await page.evaluate(function () { return Array.prototype.filter.call(document.querySelectorAll('.cam .cb'), function (b) { return b.getBoundingClientRect().width > 0; }).map(function (b) { return b.textContent; }); }), ['In', 'Out', 'Go', 'Menu'], name + ': the strip is In, Out, Go and Menu');
  var cbs = await page.evaluate(function () { return Array.prototype.map.call(document.querySelectorAll('#cam-in,#cam-out'), function (b) { var r = b.getBoundingClientRect(); return { w: r.width, h: r.height }; }); });
  assert.ok(cbs.length === 2 && cbs.every(function (r) { return r.w >= 44 && r.h >= 44; }), name + ': In and Out are 44 px targets: ' + JSON.stringify(cbs));
  for (var cb of ['#cam-in', '#cam-out']) {
    assert.ok(!overlaps(await box(page, cb), await box(page, '#tabbar')), name + ': ' + cb + ' meets the tab bar');
    var bar = await page.locator('#cursor-bar').boundingBox();   // null while the bar is hidden
    if (bar) assert.ok(!overlaps(await box(page, cb), bar), name + ': ' + cb + ' meets the title bar');
  }
  await page.tap('#cam-menu', { timeout: 5000 });
  assert.strictEqual(await expanded(page, '#cam-menu'), 'true', name + ': a tap on Menu opens it');
  assert.deepStrictEqual(await page.evaluate(topAtCentre, '#menu-list .mi'), [], name + ': Menu items under another control');
  assert.ok(inside(await box(page, '#menu-list'), await box(page, '#board')), name + ': the Menu is inside the map');
  await page.tap('#cam-outline', { timeout: 5000 });
  assert.strictEqual(await expanded(page, '#cam-outline'), 'true', name + ': a tap on Outline opens it');
  assert.ok(await page.locator('#menu-list').isHidden(), name + ': a choice closes the Menu');
  await ctx.close();
}

/* The Menu stays on a short phone screen (gd-49tZCLLeA1): it opens upward from its button, and held sideways (750x340) or under
   Safari's bars (390x664) its items are taller than the room above the button. The list is capped at that room, under the
   header and inside the map, and scrolls: its first item is the top element at its centre, and a tap reaches Plans and the last
   item (Playwright scrolls the list to them) and opens Plans. */
async function menuFits(browser, opts, name) {
  var ctx = await skyCtx(browser, Object.assign({ reducedMotion: 'reduce' }, opts)), page = await ctx.newPage();
  await page.goto(url + '/garden');
  await page.waitForSelector('#cam-menu', { state: 'visible' });
  await page.tap('#cam-menu', { timeout: 5000 });
  assert.strictEqual(await expanded(page, '#cam-menu'), 'true', name + ': a tap on Menu opens it');
  var list = await box(page, '#menu-list'), head = await box(page, 'header.crumb');
  assert.ok(inside(list, await box(page, '#board')), name + ': the Menu is inside the map: ' + JSON.stringify(list));
  assert.ok(list.y >= head.y + head.height - 1, name + ': the Menu clears the header: ' + JSON.stringify([list, head]));
  assert.deepStrictEqual(await page.evaluate(topAtCentre, '#cam-home'), [], name + ': the Menu\'s first item is under another control');
  var last = await page.evaluate(function () { var l = Array.prototype.filter.call(document.querySelectorAll('#menu-list .mi'), function (b) { return b.getBoundingClientRect().width > 0; }); return l[l.length - 1].id; });
  await page.locator('#' + last).scrollIntoViewIfNeeded();
  assert.deepStrictEqual(await page.evaluate(topAtCentre, '#' + last), [], name + ': the Menu\'s last item is under another control');
  await page.tap('#cam-plans', { timeout: 5000 });
  assert.strictEqual(await expanded(page, '#cam-plans'), 'true', name + ': a tap on Plans opens the Plans drawer');
  assert.ok(await page.locator('#menu-list').isHidden(), name + ': a choice closes the Menu');
  // held sideways the open drawer covers the header, as the bead view does, so the plans get the screen's height
  // (gd-oXOAE9dvEE); upright it stays under the header. Either way its summary, which closes it, is on top.
  var dw = await box(page, '#dw-plans');
  if (opts.viewport.height < 481) assert.ok(dw.y <= 1, name + ': the Plans drawer covers the header: ' + JSON.stringify([dw, head]));
  else assert.ok(dw.y >= head.y + head.height - 1, name + ': the Plans drawer is under the header: ' + JSON.stringify([dw, head]));
  assert.deepStrictEqual(await page.evaluate(topAtCentre, '#dw-plans > summary'), [], name + ': the Plans summary is under another control');
  await ctx.close();
}

/* The phases under the cursor (gd-kIUS2y1z7I, cursor-grid spec 6.1). Plan P has nine phases, some far wider than a label's cap.
   Inside it, on a desktop and a 390 px phone, in light and dark, the arrows walk its 3x3 beds and at each the title bar names
   that phase whole (the plan its lineage), in readable ink; no phase has a text label on the map (the beds are glyph-tier: the
   stakes' ordinals). Plan P's accessible name carries every phase in full.
   GARDENHUB_SHOTS saves each view. */
var PLAN_P = '#hots a.w-plot[href="/garden?bead=gd-planP"]';   // a cell of its project's grid (cursor-grid spec 3.6)
async function phaseRows(browser, opts, name) {
  var ctx = await skyCtx(browser, Object.assign({ reducedMotion: 'reduce' }, opts)), page = await ctx.newPage(), errors = [];
  page.on('pageerror', function (e) { errors.push(String(e)); });
  await page.goto(url + '/garden?at=gardenio/gd-planP');
  await page.waitForSelector(PLAN_P, { state: 'attached' });
  var titles = await page.evaluate(async function () {
    var doc = await (await fetch('/garden/farm', { headers: { Accept: 'application/json' } })).json();
    return doc.plans.filter(function (p) { return p.id === 'gd-planP'; })[0].phases.map(function (ph) { return ph.title; });
  });
  assert.strictEqual(titles.length, 9, name + ': plan P has nine phases');
  await page.focus(PLAN_P);
  await page.keyboard.press('Enter');
  await page.waitForSelector('#hots .w-bed.cur', { state: 'attached' });
  var seen = {}, bd = await box(page, '#board');
  var look = async function (tag) {
    await frames(page);
    var f = await full(page), d = name + ' ' + tag + ': ';
    var plate = await page.evaluate(function () {
      var e = document.getElementById('cursor-bar'), cs = getComputedStyle(e), me = getComputedStyle(e.querySelector('.cb-me'));
      var rgba = function (s) { var v = s.match(/[\d.]+/g).map(Number); return { r: v[0], g: v[1], b: v[2], a: v.length > 3 ? v[3] : 1 }; };
      var lum = function (c) { return [c.r, c.g, c.b].map(function (x) { x /= 255; return x <= 0.03928 ? x / 12.92 : Math.pow((x + 0.055) / 1.055, 2.4); }).reduce(function (s, x, i) { return s + x * [0.2126, 0.7152, 0.0722][i]; }, 0); };
      var bg = rgba(cs.backgroundColor), fg = rgba(me.color), l1 = lum(bg), l2 = lum(fg);
      return { bgA: bg.a, contrast: (Math.max(l1, l2) + 0.05) / (Math.min(l1, l2) + 0.05) };
    });
    assert.strictEqual(f.n, 0, d + 'no plate beside the phase');
    var t = f.ttl, ph = titles.filter(function (x) { return x === t; })[0];
    assert.ok(/^Homes/.test(f.pre), d + 'the plan is the bar\'s lineage: ' + f.text);
    assert.ok(ph, d + 'the bar names a phase of plan P, whole: ' + JSON.stringify(f));
    assert.ok(inside(f.box, bd), d + 'the bar\'s title is inside the map: ' + JSON.stringify([f.box, bd]));
    assert.ok(plate.bgA >= 0.8 && plate.contrast >= 4.5, d + 'the bar is readable: ' + JSON.stringify(plate));
    assert.strictEqual(f.all, 0, d + 'no phase wears text on the map (the beds are glyph-tier)');
    seen[ph] = 1;
    if (process.env.GARDENHUB_SHOTS) await page.screenshot({ path: require('path').join(process.env.GARDENHUB_SHOTS, 'phases-' + name + '-' + tag + '.png') });
  };
  await look('first');
  for (var k of ['ArrowRight', 'ArrowRight', 'ArrowDown', 'ArrowLeft', 'ArrowLeft', 'ArrowDown', 'ArrowRight', 'ArrowRight']) { await page.keyboard.press(k); await look(k); }
  assert.strictEqual(Object.keys(seen).length, 9, name + ': the walk named all nine phases: ' + JSON.stringify(Object.keys(seen)));
  console.log(name + ': nine phases named in turn');
  // out to the project (Escape): plan P's link names every phase in its accessible name
  await page.keyboard.press('Escape');
  await page.waitForSelector(PLAN_P + '.cur', { state: 'attached' });
  var aria = await page.getAttribute(PLAN_P, 'aria-label');
  ['0. Ground rules', '6. The closing walk-through with the owner at every device'].forEach(function (t) { assert.ok(aria.indexOf(t) >= 0, name + ': the plot\'s accessible name lacks ' + t + ': ' + aria); });
  assert.deepStrictEqual(errors, [], name + ' phase rows errors');
  await ctx.close();
}

async function desktop(browser) {
  var ctx = await skyCtx(browser, { viewport: { width: 1280, height: 800 }, reducedMotion: 'reduce' });
  var o = await open(ctx, '/garden?at=gardenio/gd-planA'), page = o.page;

  // 1. The cursor on plan A: no plate beside it; the title bar reads it whole with its status line, the need words first, inside
  // the board and off the plot. Hovering another plot changes nothing.
  await page.locator(A).focus();
  assert.ok(await page.locator('#cam-in').isVisible() && await page.locator('#cam-out').isVisible(), 'a desktop: In and Out are in the camera strip');
  var f = await full(page), a = await box(page, A), board = await box(page, '#board');
  assert.strictEqual(f.n, 0, 'no plate beside plan A');
  assert.ok(f.ttl === 'Edge IdP rollout' && /^1 needs you · /.test(f.st), 'the bar is plan A with its status, the need words first: ' + f.text);
  assert.ok(!overlaps(f.bar, a), 'the bar keeps off the plot: ' + JSON.stringify([f.bar, a]));
  assert.ok(inside(f.box, board), 'the bar stays in the board: ' + JSON.stringify([f.box, board]));
  await page.hover(B);
  await page.waitForTimeout(200);
  assert.strictEqual((await full(page)).text, f.text, 'the mouse moves no word of the bar');

  // 2. Siblings are glyph-tier: no other plan's title is on the map.
  var texts = await page.evaluate(function () { return Array.prototype.map.call(document.querySelectorAll('#lbls .sign'), function (e) { return e.textContent; }); });
  assert.ok(!texts.some(function (t) { return /Second plan/.test(t); }), 'no sibling plot wears its title: ' + JSON.stringify(texts));

  // 3. Focus B: the cursor and the bar move with it. Its only open item is an escalation the owner already approved
  // (gd-TEl9BfDL2z): nothing calls it "needs you" or rings it gold.
  await page.locator(B).focus();
  await page.waitForFunction(function () { var e = document.querySelector('#cursor-bar .cb-me'); return e && /Second plan/.test(e.textContent); });
  f = await full(page);
  assert.ok(!/needs you|need you/.test(f.text), 'an approved escalation is not a need: ' + f.text);
  var ring = await page.evaluate(function () { return window.GARDEN.known('gd-planB').needs; });
  assert.strictEqual(ring, 0, 'plot B has no gold "needs you" ring');
  assert.strictEqual(await page.evaluate(function () { return window.GARDEN.known('gd-planA').needs; }), 1, 'plot A (an unanswered decision) still needs you');

  // 4. A blur does not move the cursor, so the bar stays; the mouse off the board changes nothing.
  await page.evaluate(function () { document.activeElement.blur(); });
  await page.mouse.move(2, 2);
  await page.waitForTimeout(200);
  assert.ok(/Second plan/.test((await full(page)).text), 'the bar is the cursor\'s, not the focus\'s');

  // 5. A refresh of the farm (the scope buttons' recount, as a live patch does) keeps the bar (gd-Z1YadeeIFf). Escape on the
  // focused link is the cursor's ascend (cursor-grid spec 3.2): the bar names the project; Enter comes back to A.
  await page.locator(A).focus();
  await page.waitForFunction(function () { var e = document.querySelector('#cursor-bar .cb-me'); return e && /Edge IdP rollout/.test(e.textContent); });
  f = await full(page);
  await page.evaluate(function () { document.getElementById('b-all').click(); });
  await frames(page);
  assert.strictEqual((await full(page)).text, f.text, 'the bar survives a farm refresh');
  await page.keyboard.press('Escape');
  await page.waitForFunction(function () { var a = document.activeElement; return a && a.classList.contains('w-biome') && a.classList.contains('cur') && a.tabIndex === 0; });
  // the bar is refilled on the move: wait for it, or as it is after 5 s for the assertion to report
  await page.waitForFunction(function () { var e = document.querySelector('#cursor-bar .cb-me'); return e && e.textContent === 'gardenio'; }, null, { timeout: 5000 }).catch(function () {});
  f = await full(page);
  assert.ok(f.l1 === 'gardenio' && /plans?/.test(f.st), 'ascended: the bar names the project and its plans: ' + f.text);
  await page.keyboard.press('Enter');
  await page.waitForFunction(function (sel) { var a = document.activeElement; return a && a.matches(sel) && a.tabIndex === 0; }, A);

  await beadView(page);
  assert.deepStrictEqual(o.errors.filter(function (e) { return !/404/.test(e) || /left the garden/.test(e); }), [], 'desktop console errors (the unknown bead is a 404 on purpose)');
  await ctx.close();
}

function beadOf(page) { return new URL(page.url()).searchParams.get('bead'); }
async function panel(page, title) {
  var bv = page.locator('#bead');
  await bv.waitFor({ state: 'visible' });
  if (title) await page.waitForFunction(function (t) { return document.getElementById('bv-title').textContent === t; }, title);
  await page.waitForFunction(function () { return !/Loading/.test(document.getElementById('bv-body').textContent); });
  return { text: (await bv.textContent()).replace(/\s+/g, ' '), box: await bv.boundingBox() };
}

/* The in-garden bead view, desktop. */
async function beadView(page) {
  // 1. Space on plot A, the cursor, opens it in the panel (a click on a plot goes into its phases, cursor-grid spec 3.2), inside
  //    the window, with what the server knows; the page does not navigate.
  await page.focus(A);
  await page.keyboard.press(' ');
  var p = await panel(page, 'Edge IdP rollout'), win = await box(page, '#main');
  assert.ok(beadOf(page) === 'gd-planA' && new URL(page.url()).pathname === '/garden', 'the address carries the bead: ' + page.url());
  assert.ok(inside(p.box, win), 'the panel sits inside the map window: ' + JSON.stringify([p.box, win]));
  assert.ok(/Roll the edge IdP out\./.test(p.text), 'description: ' + p.text);
  assert.ok(/Phase one is in\./.test(p.text) && /Comments \(1\)/.test(p.text), 'comments: ' + p.text);
  // the children the Phases list already names are not listed again; the rest are Other children (gd-cNbP4dtM8E)
  assert.ok(/Other children \(2\)/.test(p.text) && !/Children \(/.test(p.text), 'children listed once: ' + p.text);
  // Free text's URL and bead id are links (gd-6DquTUojWU): the URL by a short label into a new tab, the id inside the garden,
  // by its title when the page knows one and the id in its tooltip (gd-9yOATV2qCs; this one the daemon does not return, so it
  // is the id, and says it is not visible to you: gd-wbu9kFaKLH).
  var links = await page.locator('#bead .bv-comments .bv-text a').evaluateAll(function (as) { return as.map(function (a) { return [a.textContent, a.getAttribute('href'), a.target, a.title]; }); });
  assert.deepStrictEqual(links, [['github.com/…/pull/42', 'https://github.com/acme/edge/pull/42', '_blank', 'https://github.com/acme/edge/pull/42'], ['gd-EdgeIdP002', '/garden?bead=gd-EdgeIdP002', '', 'gd-EdgeIdP002 (not visible to you)']], 'comment links: ' + JSON.stringify(links));
  assert.strictEqual(await page.locator('#bead .bv-comments .bv-text .bv-unseen').textContent(), ' (not visible to you)', 'an unseen id says so');
  assert.ok(/Show on the map/.test(p.text) && !/Phases and steps/.test(p.text), 'the farm actions, and no plan modal (3b R12): ' + p.text);
  assert.ok(/\d+ of \d+ steps closed/.test(p.text), 'plan progress: ' + p.text);
  assert.strictEqual(await page.evaluate(function () { return document.activeElement && document.activeElement.id; }), 'bv-title', 'focus moves to the panel');

  // 2. Escape closes it and the address drops ?bead=.
  await page.keyboard.press('Escape');
  await page.locator('#bead').waitFor({ state: 'hidden' });
  assert.strictEqual(beadOf(page), null, 'closed: ' + page.url());

  // 3. A reload of /garden?bead=<id> opens the panel again.
  await page.goto(url + '/garden?bead=gd-planA');
  p = await panel(page, 'Edge IdP rollout');
  assert.ok(/Roll the edge IdP out\./.test(p.text), 'deep link description: ' + p.text);

  // 4. A child in the panel opens in the panel (not the viewer); Back returns to the plan.
  var child = page.locator('#bead .bv-rels a.bl').first(), childHref = await child.getAttribute('href');
  assert.ok(/^\/garden\?bead=/.test(childHref), 'a cross-reference stays in the garden: ' + childHref);
  var childId = decodeURIComponent(childHref.split('bead=')[1]);
  await child.click();
  await page.waitForFunction(function (id) { return new URLSearchParams(location.search).get('bead') === id && (document.querySelector('#bv-body .bv-id') || {}).title === id; }, childId);   // its id: the Copy id button's tooltip
  await page.goBack();
  await panel(page, 'Edge IdP rollout');
  assert.strictEqual(beadOf(page), 'gd-planA', 'Back returns to the plan: ' + page.url());

  // 5. The close button, then a bead that is not on the map, from the jump palette: it opens in the panel too.
  await page.click('#bv-x');
  await page.locator('#bead').waitFor({ state: 'hidden' });
  await page.keyboard.press('/');
  await page.fill('#jump-q', 'gd-nowhere1');
  await page.keyboard.press('Enter');
  p = await panel(page, 'gd-nowhere1');
  assert.ok(/There is no bead gd-nowhere1/.test(p.text), 'an unknown id says so in the panel: ' + p.text);
  await page.keyboard.press('Escape');

  // 6. A Plans drawer card opens the same bead view of its plan, not a modal of its own (3b R12). Plans is a Menu item (3b R9):
  //    the drawer has no button on the map, and the item opens it with focus on its header.
  await page.locator('#bead').waitFor({ state: 'hidden' });
  assert.ok(!(await page.locator('#dw-plans > summary').isVisible()) && !(await page.locator('#dw-help > summary').isVisible()), 'no drawer button on the map');
  await page.click('#cam-menu');
  await page.click('#cam-plans');
  await page.waitForFunction(function () { return document.getElementById('cam-plans').getAttribute('aria-expanded') === 'true'; });   // set on the drawer's toggle event, a task later
  assert.ok(await page.locator('#menu-list').isHidden(), 'a choice closes the Menu');
  assert.ok(await page.evaluate(function () { return document.activeElement === document.querySelector('#dw-plans > summary'); }), 'focus is on the drawer\'s header');
  await page.click('#plans-slot .crop[data-id="gd-planA"]');
  p = await panel(page, 'Edge IdP rollout');
  assert.strictEqual(beadOf(page), 'gd-planA', 'the card opens the plan in the bead view: ' + page.url());
  assert.ok(/Phases \(\d+\)/.test(p.text), 'its phases are listed, each a link: ' + p.text);
  assert.strictEqual(await page.locator('.plan-modal').count(), 0, 'no plan modal');
  await page.keyboard.press('Escape');

  // 7. Legend & keys from the keyboard (3b R9): Enter opens the Menu, Up wraps to its last item (Old viewer, the link out to the
  //    viewer's LATER pages, Lands 5.13) and Up again reaches Legend & keys, Enter opens the drawer in place of Plans (one drawer
  //    at a time), and Esc closes it and hands focus back to the Menu.
  await page.locator('#bead').waitFor({ state: 'hidden' });
  await page.focus('#cam-menu');
  await page.keyboard.press('Enter');
  await page.keyboard.press('ArrowUp');
  assert.strictEqual(await page.evaluate(function () { return document.activeElement.id; }), 'cam-old', 'Old viewer is the Menu\'s last item');
  await page.keyboard.press('ArrowUp');
  assert.strictEqual(await page.evaluate(function () { return document.activeElement.id; }), 'cam-help', 'Legend & keys is just above it');
  await page.keyboard.press('Enter');
  await page.locator('#legend').waitFor({ state: 'visible' });
  assert.ok(!(await page.evaluate(function () { return document.getElementById('dw-plans').open; })), 'opening Legend & keys closes Plans');
  await page.waitForFunction(function () { return document.getElementById('cam-plans').getAttribute('aria-expanded') === 'false' && document.getElementById('cam-help').getAttribute('aria-expanded') === 'true'; });
  await page.keyboard.press('Escape');
  await page.locator('#legend').waitFor({ state: 'hidden' });
  await page.waitForFunction(function () { return document.getElementById('cam-help').getAttribute('aria-expanded') === 'false'; });   // Esc closes Legend & keys
  await page.waitForFunction(function () { return document.activeElement && document.activeElement.id === 'cam-menu'; });

  // 8. Sky is a Menu item (3b R10), not a header button: each press cycles the pinned day to night, then automatic, then day,
  //    and the item's label and accessible name say the setting.
  assert.strictEqual(await page.locator('header.crumb #b-sky, header.crumb .sky-g').count(), 0, 'no Sky button in the header');
  var sky = async function () { return page.evaluate(function () { var b = document.getElementById('cam-sky'); return { t: b.textContent, a: b.getAttribute('aria-label'), sky: document.documentElement.getAttribute('data-sky') }; }); };
  var s = await sky();
  assert.deepStrictEqual(s, { t: 'Sky: Day', a: 'Sky: always day. Press for always night', sky: 'day' }, 'the pinned day: ' + JSON.stringify(s));
  await page.click('#cam-menu'); await page.click('#cam-sky');
  assert.ok(await page.locator('#menu-list').isHidden(), 'a choice closes the Menu');
  s = await sky();
  assert.deepStrictEqual(s, { t: 'Sky: Night', a: 'Sky: always night. Press for automatic', sky: 'night' }, 'Sky goes to night: ' + JSON.stringify(s));
  await page.click('#cam-menu'); await page.click('#cam-sky');
  s = await sky();
  assert.ok(s.t === 'Sky: Auto' && /^Sky: automatic, (day|night) now\. Press for always day$/.test(s.a), 'then automatic: ' + JSON.stringify(s));
  await page.click('#cam-menu'); await page.click('#cam-sky');
  s = await sky();
  assert.deepStrictEqual(s, { t: 'Sky: Day', a: 'Sky: always day. Press for always night', sky: 'day' }, 'and back to day: ' + JSON.stringify(s));
}

async function phone(browser) {
  var ctx = await skyCtx(browser, Object.assign({}, pw.devices['Pixel 5'], { reducedMotion: 'reduce' }));
  var o = await open(ctx, '/garden?at=gardenio/gd-planA'), page = o.page, before = page.url();

  // 1. No plate beside plan A; the title bar is the strip on the tab bar, two 22 px rows at most, under the header.
  var f = await full(page), head = await box(page, 'header.crumb'), tabs = await box(page, '#tabbar');
  assert.strictEqual(f.n, 0, 'no plate beside plan A on the phone');
  assert.ok(f.ttl === 'Edge IdP rollout' && f.bar.y >= head.y + head.height - 1 && f.bar.y + f.bar.height <= tabs.y + 1 && f.bar.height <= 2 * 22 + 12, 'the phone bar is a strip on the tab bar: ' + JSON.stringify([f.bar, head, tabs]));

  // 2. A tap on another plot moves the cursor there (cursor-grid spec 7, build step 4), and the bar with it; the page stays
  //    where it is: no load and no bead opened. The address names the settled cursor's Thing 400 ms on (spec 3.5,
  //    garden.js scheduleURL), so it is waited for, not read on the tap: a slow runner passes the 400 ms first
  //    (gd-uy8TpEFT1X). The tap lands where B shows.
  var pt = await page.evaluate(function (sel) {
    var a = document.querySelector(sel), r = a.getBoundingClientRect();
    for (var j = 1; j < 8; j++) for (var i = 1; i < 8; i++) { var x = r.left + r.width * i / 8, y = r.top + r.height * j / 8; if (document.elementFromPoint(x, y) === a) return { x: x, y: y }; }
    return null;
  }, B);
  assert.ok(pt, 'some of plot B shows past plan A\'s label');
  await page.evaluate(function () { window.__sameDoc = 1; });
  await page.touchscreen.tap(pt.x, pt.y);
  await page.waitForSelector(B + '.cur', { state: 'attached' });
  await page.waitForFunction(function () { var e = document.querySelector('#cursor-bar .cb-me'); return e && /^Second plan/.test(e.textContent); });
  await page.waitForFunction(function () { return new URLSearchParams(location.search).get('at') === 'gardenio/gd-planB'; });
  var u = new URL(page.url());
  assert.ok(await page.evaluate(function () { return window.__sameDoc === 1; }) && u.pathname === new URL(before).pathname && beadOf(page) === null, 'a tap that moves the cursor must not navigate: ' + page.url());

  // 3. A tap on the bar opens its Thing in the bead view, a sheet inside the window (spec 7: Start; a second tap on the plot
  //    itself goes into its phases).
  await page.tap('#cursor-bar .cb-me');
  var p = await panel(page, null);
  assert.ok(/^Second plan/.test(await page.locator('#bv-title').textContent()), 'the tapped plot opens');
  assert.ok(beadOf(page) === 'gd-planB' && new URL(page.url()).pathname === '/garden', 'still the garden: ' + page.url());
  assert.ok(inside(p.box, await box(page, '#main')), 'the sheet sits inside the window: ' + JSON.stringify(p.box));

  assert.deepStrictEqual(o.errors.filter(function (e) { return !/404/.test(e) || /left the garden/.test(e); }), [], 'phone console errors');
  await ctx.close();
}

/* The Mailbox and Porch on a phone (gd-e3LWfAZczq): those screens hide the map, and the bead view with it, so a bead opened
   from one shows the farm, and closing it (the X, Back) returns to the screen it came from with its #mail or #porch. A reload
   of ?bead=<id>#mail opens the panel on the farm too, not behind the Mailbox. */
var MAIL_T = '#pane-mail a.t[href="/garden?bead=gd-mailD1"]';
async function mailbox(browser) {
  var ctx = await skyCtx(browser, Object.assign({}, pw.devices['iPhone 12'], { viewport: { width: 390, height: 844 }, reducedMotion: 'reduce' }));
  var o = await open(ctx, '/garden?at=gardenio/gd-planA'), page = o.page;
  var at = function () { return page.evaluate(function () { return { screen: document.body.dataset.screen, hash: location.hash, bead: new URLSearchParams(location.search).get('bead') }; }); };
  async function shown(id) {   // the panel is on screen, over the farm, for id
    // an answer moves the panel on once its POST lands, so wait for the switch rather than read the old bead's panel
    await page.waitForFunction(function (id) { return window.BEAD && BEAD.current() === id && new URLSearchParams(location.search).get('bead') === id; }, id);
    var p = await panel(page, null), s = await at();
    assert.ok(p.box && p.box.width > 0 && p.box.height > 0, 'the bead view has a box: ' + JSON.stringify(p.box));
    assert.deepStrictEqual(s, { screen: 'farm', hash: '', bead: id }, 'the bead opens on the farm');
    assert.strictEqual(await page.getAttribute('#bv-body .bv-id', 'title'), id, 'the panel is for ' + id + ' (its Copy id): ' + p.text);
  }
  async function back(screen) {   // the panel is closed and the screen it was opened from is back, in the address too
    await page.locator('#bead').waitFor({ state: 'hidden' });
    assert.deepStrictEqual(await at(), { screen: screen, hash: '#' + screen, bead: null }, 'closing returns to the ' + screen);
  }

  // 1. A tap on a decision title in the Mailbox opens it on the farm; the X returns to the Mailbox with the title in view.
  await page.tap('#tabbar [data-go="mail"]');
  assert.deepStrictEqual(await at(), { screen: 'mail', hash: '#mail', bead: null }, 'the Mailbox screen');
  await page.tap(MAIL_T);
  await shown('gd-mailD1');
  assert.strictEqual(await page.locator('#bv-title').textContent(), 'Ship the mailbox fix?', 'the decision opens');
  await page.tap('#bv-x');
  await back('mail');
  assert.ok((await box(page, MAIL_T)).height > 0, 'the decision is on screen again');

  // 2. Back closes it the same way.
  await page.tap(MAIL_T);
  await shown('gd-mailD1');
  await page.goBack();
  await back('mail');

  // 3. A reload of ?bead=<id>#mail opens the panel on the farm, not behind the Mailbox; closing it lands on the Mailbox.
  await page.goto(url + '/garden?bead=gd-mailD1#mail');
  await shown('gd-mailD1');
  await page.keyboard.press('Escape');
  await back('mail');

  // 4. A bead the Porch chat mentions (chat.js openBead's garden:open-bead) opens on the farm; Escape returns to the Porch.
  await page.tap('#tabbar [data-go="porch"]');
  var taken = await page.evaluate(function () { return !document.dispatchEvent(new CustomEvent('garden:open-bead', { detail: { id: 'gd-planA' }, cancelable: true })); });
  assert.ok(taken, 'the bead view takes the chat\'s bead');
  await shown('gd-planA');
  await page.keyboard.press('Escape');
  await back('porch');

  // 5. An answered ask leaves the screen and the next one takes its place (gd-LEfzdQl057): answered inside the bead view
  //    (a tap arms the option, a second sends it), the panel moves on to the next decision the Mailbox shows and the
  //    answered row is gone from the Mailbox; answered again with none left, the panel closes back to the Mailbox, now empty.
  //    The buttons are the decision face's own (WB-1, gd-vFUZG6FnTT), which stands for the Mailbox row's copy once the bead lands.
  var mailRows = function () { return page.evaluate(function () { return Array.prototype.map.call(document.querySelectorAll('#d-mail > li'), function (li) { return li.dataset.src.slice(2); }); }); };
  var YES = '#bead .bv-face [data-act="resolve"][data-opt="y"]';
  async function answer(id) {   // the panel shows id, fetched (its Description section is in), and Yes is tapped twice
    await shown(id);
    await page.locator('#bead .bv-text, #bead .bv-empty').first().waitFor();
    await page.tap(YES);
    assert.strictEqual((await page.locator(YES).textContent()).trim(), 'Confirm: Yes', 'the first tap arms the answer for ' + id);
    await page.tap(YES);
  }
  await page.tap('#tabbar [data-go="mail"]');
  assert.deepStrictEqual(await mailRows(), ['gd-mailD1', 'gd-mailD2'], 'two decisions wait, newest first');
  await page.tap(MAIL_T);
  await answer('gd-mailD1');
  await panel(page, 'Second question?');   // the panel moves on once the answer's POST lands, not on the tap (gd-vmK2D7egCY)
  await shown('gd-mailD2');
  await page.locator('#d-mail [data-src="i-gd-mailD1"]').waitFor({ state: 'detached' });
  assert.deepStrictEqual(await mailRows(), ['gd-mailD2'], 'the answered decision left the Mailbox');
  assert.strictEqual(await page.locator('#toast').textContent(), 'Answered gd-mailD1 with y.', 'the toast says what was sent');
  await answer('gd-mailD2');
  await back('mail');
  assert.deepStrictEqual(await mailRows(), [], 'the Mailbox is empty once the last ask is answered');
  assert.ok(await page.locator('#pane-mail').evaluate(function (p) { return p.classList.contains('none'); }), 'the Mailbox says it is empty');

  assert.deepStrictEqual(o.errors, [], 'mailbox console errors');
  await ctx.close();
}

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  try {
    await opening(browser, { viewport: { width: 1707, height: 842 } }, 'desktop');
    await opening(browser, Object.assign({}, pw.devices['iPhone 12'], { viewport: { width: 390, height: 844 } }), 'phone');
    await cameraClear(browser, Object.assign({}, pw.devices['iPhone 12'], { viewport: { width: 390, height: 844 } }), 'phone');
    await cameraClear(browser, { viewport: { width: 820, height: 1180 }, isMobile: true, hasTouch: true }, 'mid');
    await menuFits(browser, Object.assign({}, pw.devices['iPhone 12'], { viewport: { width: 390, height: 664 } }), 'phone-safari');
    await menuFits(browser, Object.assign({}, pw.devices['iPhone 12 landscape'], { viewport: { width: 750, height: 340 } }), 'phone-sideways');
    await menuFits(browser, Object.assign({}, pw.devices['Galaxy S8 landscape'], { viewport: { width: 740, height: 360 } }), 'android-sideways');
    for (var scheme of ['light', 'dark']) {
      await phaseRows(browser, { viewport: { width: 1280, height: 800 }, colorScheme: scheme }, 'desktop-' + scheme);
      await phaseRows(browser, Object.assign({}, pw.devices['iPhone 12'], { viewport: { width: 390, height: 844 }, colorScheme: scheme }), 'phone-' + scheme);
    }
    await desktop(browser); await phone(browser); await mailbox(browser); console.log('card browser checks passed');
  }
  finally { await browser.close(); }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
