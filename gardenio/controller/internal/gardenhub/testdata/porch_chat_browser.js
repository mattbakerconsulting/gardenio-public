/* Browser walk for the Porch chat (gd-AXfec5afZo; the Commons thread since gd-EwISaXh6Xk), run from TestPorchChatUnderChromium
   against a live /garden on an httptest server with the Commons fixture land (commons_world_test.go) mounted beside it.
   Usage: node porch_chat_browser.js <base url> <node_modules dir>.

   Desktop: a Porch row of a gardener bound to a thread goes to its node in the workbench, /garden/c/n/<agent> (gd-ZeiSZV7j2Y:
   the band that showed the thread inside /garden is gone); a gardener bound to no thread opens its own bead in the garden's
   bead view instead, without leaving /garden. Phone (Pixel 5): the tab bar's Porch, then Open agent chat, goes to the node.
   Nothing links to the retired /agents/chat, and there are zero page errors. A gardener on the farm: a tap on its field
   figure opens its Glass, or its transcript below the operator tier, in place (H-D21, homestead-agents-as-people.md 12.3):
   the thread left the figure's click and stays one tap away on its Porch row, which the walks above pin. The node's own page
   is the workbench's (its walks are workbench_*_browser.js), so here it is a stub: this walk pins where the Porch goes. */
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

var shots = process.env.GARDENHUB_SHOTS || '';   // a directory: save a PNG at each step, for review
async function shot(page, name) { if (shots) await page.screenshot({ path: require('path').join(shots, 'porch-' + name + '.png') }); }
function inGarden(u) { return /^\/garden(\/|\?|$)/.test(new URL(u).pathname + new URL(u).search); }
var NODE = '/garden/c/n/a-1';

async function open(ctx) {
  var page = await ctx.newPage(), errors = [], nav = [], missed = [];
  page.on('pageerror', function (e) { errors.push(String(e)); });
  // A gardener bound to no thread is a 404 from the place lookup by design; the browser logs it, so it is pinned below.
  page.on('response', function (r) { if (r.status() === 404) missed.push(new URL(r.url()).pathname); });
  page.on('console', function (m) { if (m.type() === 'error' && !/EventSource|events|status of 404/.test(m.text())) errors.push(m.text()); });
  page.on('framenavigated', function (f) { if (f === page.mainFrame()) nav.push(f.url()); });
  await page.route('**/garden/c/n/**', function (r) { return r.request().isNavigationRequest() ? r.fulfill({ status: 200, contentType: 'text/html', body: '<!doctype html><title>Workbench</title>' }) : r.continue(); });
  await page.goto(url + '/garden');
  await page.waitForSelector('#d-porch > li[data-src="i-a-1"]', { state: 'attached' });
  return { page: page, errors: errors, nav: nav, missed: missed };
}
async function nodeShows(page) {
  await page.waitForURL(function (u) { return u.pathname === NODE; });
  assert.strictEqual(await page.title(), 'Workbench', 'the workbench\'s address, not a /garden panel');
}
/* every address the walk saw before it went to the node was /garden's */
function stayed(o) { o.nav.filter(function (u) { return new URL(u).pathname !== NODE; }).forEach(function (u) { assert.ok(inGarden(u), 'the URL left /garden: ' + u); }); }

async function desktop(browser) {
  var ctx = await skyCtx(browser, { viewport: { width: 1280, height: 800 }, reducedMotion: 'reduce' });
  var o = await open(ctx), page = o.page;
  assert.strictEqual(await page.locator('a[href*="/agents/chat"]').count(), 0, 'no link to the retired agent chat');
  assert.strictEqual(await page.locator('#chat').count(), 0, 'the Porch has no conversation view of its own');

  // 1. A gardener with no thread opens its own bead in the garden's bead view, in place.
  await page.click('#dbtn-porch');
  await shot(page, 'desktop-porch');
  await page.evaluate(function () { window.__still = true; });
  await page.click('#d-porch > li[data-src="i-a-2"] .m');   // the row's meta line: the whole row is the target
  await page.locator('#bead').waitFor({ state: 'visible' });
  await page.waitForFunction(function () { return /Idle, between tasks\./.test(document.getElementById('bv-body').textContent); });
  assert.strictEqual(new URL(page.url()).searchParams.get('bead'), 'a-2', 'the address carries the bead: ' + page.url());
  assert.ok(await page.evaluate(function () { return window.__still === true; }), 'nothing reloaded the page');
  await shot(page, 'desktop-no-thread');
  await page.keyboard.press('Escape');

  // 2. A Porch row of a gardener with a thread goes to its node in the workbench.
  await page.click('#d-porch > li[data-src="i-a-1"] .t');
  await nodeShows(page);
  stayed(o);
  assert.deepStrictEqual(o.missed, ['/garden/c/bead/a-2'], 'the only miss is the gardener with no thread');
  assert.deepStrictEqual(o.errors, [], 'desktop page errors');
  await ctx.close();
}

async function phone(browser) {
  var ctx = await skyCtx(browser, Object.assign({}, pw.devices['Pixel 5'], { reducedMotion: 'reduce' }));
  var o = await open(ctx), page = o.page;
  await page.tap('#tabbar [data-go=porch]');
  await page.locator('#chat-open').waitFor({ state: 'visible' });
  // Open agent chat opens the first gardener on the Porch; make that the one with a thread.
  var first = await page.evaluate(function () { return document.querySelector('#d-porch > li').dataset.src; });
  if (first === 'i-a-1') await page.tap('#chat-open');
  else await page.tap('#d-porch > li[data-src="i-a-1"] .t');
  await nodeShows(page);
  stayed(o);
  assert.deepStrictEqual(o.missed, [], 'phone 404s');
  assert.deepStrictEqual(o.errors, [], 'phone page errors');
  await ctx.close();
}

/* A gardener on the farm: the phone's walk. The cursor on its field figure, a tap acts on it (cursor-grid spec 7). */
async function tapSprite(page, id) {
  await page.waitForFunction(function (id) { return window.GARDEN.fly(id); }, id);   // its home, the cursor on the gardener's field stand
  var sel = '#ghots .hot[href$="' + id + '"], #hots .hot[href="/garden?bead=' + id + '"]';
  await page.waitForSelector(sel, { state: 'visible' });
  await page.tap(sel);
}
async function sprite(browser) {
  var ctx = await browser.newContext(Object.assign({}, pw.devices['Pixel 5'], { reducedMotion: 'reduce' }));
  var o = await open(ctx), page = o.page;
  await tapSprite(page, 'a-2');
  await page.waitForFunction(function () { var q = new URLSearchParams(location.search); return q.get('glass') === 'a-2' || q.get('transcript') === 'a-2'; });
  await shot(page, 'sprite-glass');
  stayed(o);
  assert.deepStrictEqual(o.errors, [], 'sprite page errors');
  await ctx.close();
}

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  try { await desktop(browser); await phone(browser); await sprite(browser); console.log('porch chat browser walk passed'); }
  finally { await browser.close(); }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
