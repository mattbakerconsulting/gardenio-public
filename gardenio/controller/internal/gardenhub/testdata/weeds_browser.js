/* Browser check for the weeds and the fog (gd-ytTNc3Fu9M), run from TestWeedsUnderChromium against /garden on an httptest
   server with weedsWorld: a Commons plan with no label and no phases whose step is wilted, a loose bead with no label, a draft
   untouched for ten days, a PR still open after its bead closed, and a project (attic) idle for twenty days.
   Usage: node weeds_browser.js <base url> <node_modules dir> <now, unix ms>. Date.now() is fixed at <now>, the farm's clock
   (weedsNow), so every weed's age is the same on every run. GARDENHUB_SHOTS=<dir> saves a PNG at each step, for review.

   Desktop: at the Project level the weedy plot wears a thistle on its corner (purple pixels on the canvas), its link names its
   weeds and the title bar's status line counts them (gd-zDdiA3HBh5); the attic region is under fog at the World level (lighter than a live region)
   and its link says it is dormant; the gardenio link tallies its weeds; the long-frosted seed says why in its link. The info card
   that carried each weed's "why" sentence is gone (cursor-grid spec N-D14): the plan's bead view carries it (build step 6), its
   Weeds first, each why linking the bead it names inside the garden, then its operator and phases.
   Phone, light and dark: the title bar counts the plot's weeds and stays on the screen, above the tab bar; the bead view shows
   every weed line unscrolled (gd-TDLWiDH7Yj). No request leaves /garden; no console errors. */
'use strict';
var url = process.argv[2], mods = process.argv[3], now = +process.argv[4], assert = require('assert'), path = require('path');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));
/* Every context pins the sky (web/sky.js, gd-SwRqFcjWc6) to its emulated colour scheme: light is day and dark is night
   whatever the clock says, so a run at night (CI is UTC) sees the page a run by day does. */
function skyCtx(browser, opts) {
  opts = opts || {};
  return browser.newContext(opts).then(function (ctx) {
    return ctx.addInitScript(function (s) { try { localStorage.setItem('garden.sky', s); } catch (e) { /* no storage: the emulated scheme still decides by day */ } }, opts.colorScheme === 'dark' ? 'night' : 'day').then(function () { return ctx; });
  });
}
var shots = process.env.GARDENHUB_SHOTS || '';
/* the accessible name ends with the cell's place in the cursor's grid (cursor-grid spec 3.6); after a weeds sentence the place is
   a sentence of its own, never ".," (gd-jhE8MpXvJm) */
var WHERE = ' Row \\d+ of \\d+(, column \\d+ of \\d+)?\\.$';
var PLOT = '#hots a.w-plot[href="/garden?bead=gd-weedP"]';

/* /garden?at=<at>: the cursor on that Thing (cursor-grid spec 3.5); wait is its grid's link to wait for; enter goes into it. */
async function open(ctx, at, wait, enter) {
  var page = await ctx.newPage(), errors = [];
  await page.clock.setFixedTime(now);
  page.on('console', function (m) { if (m.type() === 'error') errors.push(m.text()); });
  page.on('pageerror', function (e) { errors.push(String(e)); });
  page.on('request', function (r) { var u = new URL(r.url()); if (u.origin === new URL(url).origin && u.pathname !== '/garden' && u.pathname.indexOf('/garden/') !== 0) errors.push('left the garden: ' + u.pathname + u.search); });
  await page.goto(url + '/garden?at=' + at);
  await page.waitForSelector(wait, { state: 'attached' });
  if (enter) { await page.keyboard.press('Enter'); await page.waitForSelector(enter, { state: 'attached' }); }
  // a timer task, then two frames: the Enter's signs land in a setTimeout(0) task of their own (gd-fsuxCGGbZr)
  await page.evaluate(function () { return new Promise(function (r) { setTimeout(function () { requestAnimationFrame(function () { requestAnimationFrame(r); }); }, 0); }); });
  return { page: page, errors: errors };
}
async function shot(page, name) { if (shots) await page.screenshot({ path: path.join(shots, 'weeds-' + name + '.png') }); }
/* The title bar (gd-zDdiA3HBh5): the cursor Thing's title and status line as "title · status", and the bar's box. */
async function full(page) {
  var l = page.locator('#cursor-bar');
  await page.locator('#cursor-bar .cb-me').waitFor({ state: 'visible' });
  var t = await page.locator('#cursor-bar .cb-me').textContent(), st = await page.locator('#cursor-bar .cb-sw').textContent();
  return { text: (t + (st ? ' · ' + st : '')).replace(/\s+/g, ' '), box: await l.boundingBox() };
}
async function label(page, sel) { return page.locator(sel).first().getAttribute('aria-label'); }
/* Canvas pixels in a board rect (CSS px): the mean colour, and how many are purple (red and blue well above green: the
   thistle's head, whatever smoothing a zoom below native size applies; grass, soil and crops are green or brown). */
async function pixels(page, r) {
  return page.evaluate(function (r) {
    var cv = document.getElementById('cv'), b = cv.getBoundingClientRect(), d = cv.width / b.width;
    var x = Math.max(0, Math.round((r.x - b.left) * d)), y = Math.max(0, Math.round((r.y - b.top) * d)), w = Math.max(1, Math.round(r.width * d)), h = Math.max(1, Math.round(r.height * d));
    var px = cv.getContext('2d').getImageData(x, y, w, h).data, n = 0, sum = [0, 0, 0];
    for (var i = 0; i < px.length; i += 4) {
      sum[0] += px[i]; sum[1] += px[i + 1]; sum[2] += px[i + 2];
      if (px[i + 2] > px[i + 1] + 40 && px[i] > px[i + 1] + 15) n++;
    }
    var k = px.length / 4;
    return { n: n, mean: [sum[0] / k, sum[1] / k, sum[2] / k] };
  }, r);
}
/* The bead view of the weedy plan, once the farm document has filled its sections: each section's heading and its rows' text. */
async function beadView(ctx) {
  var o = await open(ctx, '~/gd-weedP&bead=gd-weedP', PLOT), page = o.page;
  await page.locator('#bead .bv-weeds').waitFor({ state: 'visible' });
  await page.locator('#bead .bv-phases').waitFor({ state: 'visible' });
  o.secs = await page.$$eval('#bv-body .bv-live > .bv-sec', function (ss) {
    return ss.map(function (s) { return { h: s.querySelector('h3').textContent, rows: Array.prototype.map.call(s.querySelectorAll('li, p'), function (r) { return r.textContent.replace(/\s+/g, ' '); }) }; });
  });
  return o;
}
function checkSections(secs) {
  // nobody stands on the plot (the farm document's gardeners[] has no one there), so there is no Gardeners here
  assert.deepStrictEqual(secs.map(function (s) { return s.h; }), ['Weeds', 'Operator', 'Phases (1)'], 'the plan\'s sections, Weeds first');
  var w = secs[0].rows;
  assert.strictEqual(w.length, 3, 'every weed: ' + JSON.stringify(w));
  assert.ok(w.some(function (r) { return /^Wilted: Sort the seed tins, in progress, but no change for 20 hours .*Gardener: flo\.Sort the seed tins$/.test(r); }), 'the step\'s weed names it and links it: ' + JSON.stringify(w));
  assert.ok(w.some(function (r) { return /^Plan without phases: 1 open step sits in one Other row\./.test(r); }) && w.some(function (r) { return /^No project label: it grows in the Commons\./.test(r); }), JSON.stringify(w));
  assert.ok(/^matt, \w+ \w+ fence$/.test(secs[1].rows[0]), 'its operator and fence in words: ' + secs[1].rows[0]);
  assert.deepStrictEqual(secs[2].rows, ['·Other0/1 steps'], 'the Other bed');
}
function done(o, what) { assert.deepStrictEqual(o.errors, [], what + ': console errors or a request outside /garden'); }

async function desktop(browser) {
  var ctx = await skyCtx(browser, { viewport: { width: 1280, height: 800 }, reducedMotion: 'reduce' });

  // 1. The cursor on the weedy Commons plot: it wears a thistle and names its weeds; the title bar counts them.
  var o = await open(ctx, '~/gd-weedP', PLOT), page = o.page;
  assert.match(await label(page, PLOT), new RegExp('Weeds: 1 wilted bead, 1 plan without phases, 1 unlabelled bead\\.' + WHERE));
  var b = await page.locator(PLOT).boundingBox(), corner = { x: b.x + b.width - 14, y: b.y + b.height - 14, width: 20, height: 20 };
  await shot(page, 'project-desktop-light');
  var purple = await pixels(page, corner);
  assert.ok(purple.n >= 6, 'a thistle on the weedy plot\'s corner (' + purple.n + ' purple px)');
  var c = await full(page);
  assert.ok(/ · 3 weeds$/.test(c.text), 'the title bar counts the plot\'s weeds: ' + c.text);
  done(o, 'project'); await page.close();

  // 2. Depth 0 on the attic: the dormant attic is under fog, lighter than a live region; its link says so.
  o = await open(ctx, 'attic', '#hots .w-biome'); page = o.page;
  var biome = async function (name) { var l = page.locator('#hots .w-biome[aria-label^="' + name + ':"]'); return l.boundingBox(); };
  var ab = await biome('attic'), gb = await biome('gardenio');
  assert.ok(ab && gb, 'both regions are in view at depth 0');
  assert.match(await page.locator('#hots .w-biome[aria-label^="attic:"]').getAttribute('aria-label'), new RegExp('Dormant for 20 days\\.' + WHERE));
  var mid = function (r) { return { x: r.x + r.width * 0.3, y: r.y + r.height * 0.3, width: r.width * 0.4, height: r.height * 0.4 }; };
  var fogged = await pixels(page, mid(ab)), live = await pixels(page, mid(gb));
  // depth 0 frames the lattice at the Project level on a desktop, where a live region's plots are crop plates: fog is the pale one
  assert.ok(Math.min.apply(null, fogged.mean) > 140 && fogged.mean[2] > live.mean[2] + 40, 'fog lightens the dormant region: ' + JSON.stringify([fogged.mean, live.mean]));
  await shot(page, 'world-desktop-light');
  c = await full(page);
  assert.ok(/^attic · .*\b1 weed$/.test(c.text), 'the title bar counts the attic\'s weed: ' + c.text);
  done(o, 'attic sign'); await page.close();

  // 3. The gardenio project's link tallies its weeds; the title bar leads with the need words.
  o = await open(ctx, 'gardenio', '#hots .w-biome'); page = o.page;
  var sign = '#hots .w-biome[aria-label^="gardenio:"]';
  assert.match(await label(page, sign), /Weeds: 1 orphaned PR, .*1 long-frosted draft/);
  c = await full(page);
  assert.ok(/^gardenio.*needs you/.test(c.text), 'the title bar leads with the need words: ' + c.text);
  done(o, 'gardenio sign'); await page.close();

  // 4. Inside the weedy plan: thistles on the weedy cells; the long-frosted seed pot in its band says why.
  o = await open(ctx, '~/gd-weedP', PLOT, '#hots .w-bed'); page = o.page;
  await shot(page, 'plan-desktop-light');
  done(o, 'plan'); await page.close();
  var POT = '#hots a.w-seed[href="/garden?bead=gd-weedS"]';
  o = await open(ctx, 'gardenio/gd-weedS', POT); page = o.page;
  assert.match(await label(page, POT), new RegExp('Weeds: 1 long-frosted draft\\.' + WHERE));
  await shot(page, 'seed-desktop-light');
  done(o, 'seed'); await page.close();

  // 5. The plan's bead view carries what its card did: why each weed is one (the step's links into the garden), then its
  // operator and phases.
  o = await beadView(ctx); page = o.page;
  checkSections(o.secs);
  var ref = page.locator('#bead .bv-weeds a.bv-ref');
  assert.strictEqual(await ref.count(), 1, 'only the step\'s weed names another bead');
  assert.strictEqual(await ref.getAttribute('href'), '/garden?bead=gd-weedP1');
  await shot(page, 'bead-desktop-light');
  await ref.click();
  await page.waitForFunction(function () { return document.getElementById('bv-title').textContent === 'Sort the seed tins'; });
  assert.ok(/[?&]bead=gd-weedP1/.test(page.url()), 'the step opens in the bead view: ' + page.url());
  await page.locator('#bead .bv-weeds').waitFor({ state: 'visible' });
  assert.ok(/^Wilted: in progress/.test(await page.locator('#bead .bv-weeds li').first().textContent()), 'a step lists its own weed');
  done(o, 'bead view'); await page.close();
  await ctx.close();
}

async function phone(browser, scheme) {
  var ctx = await skyCtx(browser, Object.assign({}, pw.devices['iPhone 12'], { viewport: { width: 390, height: 844 }, reducedMotion: 'reduce', colorScheme: scheme }));
  var o = await open(ctx, '~/gd-weedP', PLOT), page = o.page;
  await shot(page, 'project-phone-' + scheme);
  var c = await full(page), tabs = await page.locator('#tabbar').boundingBox();
  assert.ok(/ · 3 weeds$/.test(c.text), 'the title bar counts the plot\'s weeds: ' + c.text);
  assert.ok(c.box.x >= 0 && c.box.x + c.box.width <= 390 && c.box.y >= 0 && c.box.y + c.box.height <= tabs.y + 1, 'the bar stays on the phone screen, above the tab bar: ' + JSON.stringify([c.box, tabs]));
  done(o, 'phone ' + scheme); await page.close();
  // the bead view on a phone: every weed line is on the screen before any scroll (gd-TDLWiDH7Yj)
  o = await beadView(ctx); page = o.page;
  checkSections(o.secs);
  var body = await page.locator('#bv-body').boundingBox(), last = await page.locator('#bead .bv-weeds li').last().boundingBox();
  assert.ok(last.y + last.height <= Math.min(body.y + body.height, tabs.y), 'the last weed line is in view unscrolled: ' + JSON.stringify([last, body, tabs]));
  await shot(page, 'bead-phone-' + scheme);
  done(o, 'phone bead view ' + scheme); await ctx.close();
}

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  try { await desktop(browser); await phone(browser, 'light'); await phone(browser, 'dark'); console.log('weeds browser checks passed'); }
  finally { await browser.close(); }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
