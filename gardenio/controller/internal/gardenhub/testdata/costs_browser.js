/* Browser check for the Costs panel's frames (plan gd-gh5J0vVbJP step 2.4, gd-5HipEi6w2o; 2.1's web/costs.js), run from
   TestCostsUnderChromium against /garden on cardWorld, its GET /garden/charts/{set} pointed at a stub Grafana under
   <base>/grafana on the same server (each solo panel a page with one control, as Grafana's own). Usage:
   node costs_browser.js <base url> <node_modules dir>.

   Desktop, motion allowed: /garden?costs=fleet opens the panel; a card not yet in view has no src and its frame box
   shimmers (gp-shimmer), one in view gets its src from the stub and loses the skeleton on load; every frame is titled
   "<card title> Grafana panel". With every card brought into view and loaded, Tab from the checked chip reaches each
   card's Open link and then its frame (into Grafana's control and out again), in order, then the dashboard and sign-in
   links, and the next Tab leaves the panel. Desktop, prefers-reduced-motion: reduce: a box still waiting does not
   animate. Phone: the cards fit the sheet's width. No request leaves the test server, no console errors. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));

function watch(page) {
  var errors = [], away = [];
  page.on('console', function (m) { if (m.type() === 'error') errors.push(m.text()); });
  page.on('pageerror', function (e) { errors.push(String(e)); });
  page.on('request', function (r) { if (!/^(data|about|blob):/.test(r.url()) && r.url().indexOf(url) !== 0) away.push(r.url()); });
  return { errors: errors, away: away };
}
async function open(page) {
  await page.goto(url + '/garden?costs=fleet');
  await page.waitForFunction(function () {
    var p = document.getElementById('bead');
    return p && !p.hidden && p.dataset.kind === 'costs' && document.querySelectorAll('#bv-body .gp-card').length > 1;
  });
}
/* The last card's box, which the panel has not reached yet: no src, no load, its skeleton showing. */
function waiting(page) {
  return page.evaluate(function () {
    var cards = document.querySelectorAll('#bv-body .gp-card'), last = cards[cards.length - 1], box = last.querySelector('.gp-box'), f = last.querySelector('iframe');
    return { src: f.getAttribute('src'), loaded: box.classList.contains('loaded'), anim: getComputedStyle(box).animationName, word: last.querySelector('.gp-word').textContent };
  });
}
/* What has the keyboard focus, named as a person would: the chip's range, a link's label, a frame's title. */
function focused(page) {
  return page.evaluate(function () {
    var e = document.activeElement;
    if (!e || !document.getElementById('bv-body').contains(e)) return 'outside the panel';
    if (e.tagName === 'IFRAME') return 'frame ' + e.getAttribute('title');
    if (e.tagName === 'BUTTON') return e.dataset.range ? 'chip ' + e.dataset.range : e.textContent;
    return e.getAttribute('aria-label') || e.textContent;
  });
}

async function desktop(browser) {
  var ctx = await browser.newContext({ viewport: { width: 1280, height: 800 }, reducedMotion: 'no-preference' }), page = await ctx.newPage(), w = watch(page);
  await open(page);
  assert.strictEqual(await page.locator('#bv-title').textContent(), 'Costs: Fleet');
  var titles = await page.$$eval('#bv-body .gp-card', function (cs) { return cs.map(function (c) { return [c.querySelector('h3').textContent, c.querySelector('iframe').getAttribute('title')]; }); });
  assert.ok(titles.length >= 3, 'the fleet set\'s cards: ' + titles.length);
  titles.forEach(function (t) { assert.strictEqual(t[1], t[0] + ' Grafana panel', 'each frame is titled by its card'); });

  var w0 = await waiting(page);
  assert.strictEqual(w0.src, null, 'a card out of view has no src yet');
  assert.ok(!w0.loaded);
  assert.strictEqual(w0.word, 'Loading chart…');
  assert.strictEqual(w0.anim, 'gp-shimmer', 'its box shimmers while it waits');
  await page.waitForFunction(function () { var b = document.querySelector('#bv-body .gp-card .gp-box'); return b.classList.contains('loaded'); });
  var first = await page.evaluate(function () {
    var c = document.querySelector('#bv-body .gp-card');
    return { src: c.querySelector('iframe').getAttribute('src'), hidden: c.querySelector('.gp-word').hidden, anim: getComputedStyle(c.querySelector('.gp-box')).animationName };
  });
  assert.ok(first.src.indexOf(url + '/grafana/d-solo/') === 0, 'the first card loads the stub: ' + first.src);
  assert.ok(first.hidden, 'the skeleton label hides on load');
  assert.strictEqual(first.anim, 'none', 'a loaded box stops shimmering');

  // bring every card into view so every frame holds Grafana's page, then walk the panel with Tab
  var n = titles.length;
  for (var i = 0; i < n; i++) await page.locator('#bv-body .gp-card').nth(i).scrollIntoViewIfNeeded();
  await page.waitForFunction(function (k) { return document.querySelectorAll('#bv-body .gp-box.loaded').length === k; }, n);
  for (var j = 0; j < n; j++) await page.frameLocator('#bv-body .gp-card:nth-child(' + (j + 1) + ') iframe').locator('button').waitFor();
  var want = ['chip 7d'];
  titles.forEach(function (t) { want.push('Open ' + t[0] + ' in Grafana', 'frame ' + t[1]); });
  want.push('Open the dashboard in Grafana', 'Open Grafana once to sign in', 'outside the panel');
  await page.focus('#bv-body .gp-chips [aria-checked=true]');
  var got = [await focused(page)];
  while (got.length < want.length) {
    await page.keyboard.press('Tab');
    var now = await focused(page);
    if (now !== got[got.length - 1]) got.push(now);   // the Tab inside a frame, onto Grafana's control, keeps the frame focused here
    else if (now.indexOf('frame ') !== 0) break;     // stuck anywhere else is a trap
  }
  assert.deepStrictEqual(got, want, 'keyboard focus: the chip, each card\'s Open link and frame in turn, the two links, then out');

  assert.deepStrictEqual(w.away, [], 'no request leaves the test server');
  assert.deepStrictEqual(w.errors, [], 'no console errors');
  await ctx.close();
}

async function reduced(browser) {
  var ctx = await browser.newContext({ viewport: { width: 1280, height: 800 }, reducedMotion: 'reduce' }), page = await ctx.newPage(), w = watch(page);
  await open(page);
  var w0 = await waiting(page);
  assert.ok(w0.src === null && !w0.loaded && w0.word === 'Loading chart…', 'the last card still waits: ' + JSON.stringify(w0));
  assert.strictEqual(w0.anim, 'none', 'prefers-reduced-motion: the waiting box does not shimmer');
  assert.deepStrictEqual(w.errors, [], 'no console errors');
  await ctx.close();
}

async function phone(browser) {
  var ctx = await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true }), page = await ctx.newPage(), w = watch(page);
  await open(page);
  var fit = await page.evaluate(function () {
    var b = document.getElementById('bv-body'), c = b.querySelector('.gp-card').getBoundingClientRect(), r = b.getBoundingClientRect();
    return { over: b.scrollWidth - b.clientWidth, left: c.left - r.left, right: r.right - c.right };
  });
  assert.ok(fit.over <= 1, 'nothing runs off the side of the sheet: ' + fit.over);
  assert.ok(fit.left >= 0 && fit.right >= 0, 'a card sits inside the sheet: ' + JSON.stringify(fit));
  assert.deepStrictEqual(w.errors, [], 'no console errors');
  await ctx.close();
}

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); } catch (e) { console.log('no chromium: ' + e.message); process.exit(3); }
  try { await desktop(browser); await reduced(browser); await phone(browser); } finally { await browser.close(); }
  console.log('costs panel frames: ok');
})().catch(function (e) { console.error(e); process.exit(1); });
