/* Browser check for the welcome tour (Lands Track B step 9.14b), run from TestWelcomeTourUnderChromium against /garden on the
   card farm cut to the gardenio land, whose page is due the tour until POST /garden/welcome records it (POST
   /test/welcome/reset makes it due again). Usage: node welcome_browser.js <base url> <node_modules dir>. GARDENHUB_SHOTS=<dir>
   saves a PNG at each stop.

   Desktop: the first /garden opens the card on its own, "Welcome · stop 1 of 3", focus on its title, the cursor on the land.
   Next walks how to ask and the steward's hello; the last stop's button is "Open the steward", which closes the card,
   records the tour once and goes to the land's steward in the workbench, /garden/c/s/<land> (its page is the workbench's,
   so here it is a stub). A reload runs no tour. Phone: the card on the Farm, every button 44 px tall; Escape dismisses it,
   and that records it too. No console errors. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert'), path = require('path');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));
var shots = process.env.GARDENHUB_SHOTS || '';
var TITLES = ['Welcome to the gatehouse', 'How to ask', 'Your steward said hello'];

async function open(ctx, where) {
  var page = await ctx.newPage(), o = { page: page, errors: [], seen: 0 };
  page.on('console', function (m) { if (m.type() === 'error') o.errors.push(m.text()); });
  page.on('pageerror', function (e) { o.errors.push(String(e)); });
  page.on('request', function (r) { if (r.method() === 'POST' && new URL(r.url()).pathname === '/garden/welcome') o.seen++; });
  await page.route('**/garden/c/s/**', function (r) { return r.request().isNavigationRequest() ? r.fulfill({ status: 200, contentType: 'text/html', body: '<!doctype html><title>Workbench</title>' }) : r.continue(); });
  await page.goto(url + where);
  await page.waitForSelector('#hots .hot', { state: 'attached' });
  return o;
}
async function shot(page, name) { if (shots) await page.screenshot({ path: path.join(shots, 'welcome-' + name + '.png') }); }
/* A card that never shows says why: whether the server sent a page due the tour (body data-welcome) and the
   console errors so far, which done() would only report after it (gd-vnypm27vat). */
async function card(page, errors) {
  try { await page.locator('#tour').waitFor({ state: 'visible' }); }
  catch (e) {
    var due = await page.evaluate(function () { return document.body.dataset.welcome || ''; });
    throw new Error('the welcome card never showed (due "' + due + '", console errors ' + JSON.stringify(errors || []) + '): ' + e.message);
  }
  return page.evaluate(function () {
    var t = document.getElementById('tour'), b = t.getBoundingClientRect(), cur = document.querySelector('#hots .hot.cur');
    return { n: t.querySelector('#tour-n').textContent, title: t.querySelector('#tour-h').textContent, focus: document.activeElement && document.activeElement.id,
      next: t.querySelector('.tour-next').textContent, acts: t.querySelectorAll('.tour-acts button').length, meta: !!t.querySelector('.tour-meta'),
      cur: cur ? cur.getAttribute('aria-label') : '', w: b.width, h: b.height,
      buttons: Array.prototype.map.call(t.querySelectorAll('button'), function (x) { return Math.round(x.getBoundingClientRect().height); }) };
  });
}
async function settle(page) { await page.waitForTimeout(1200); }   // the first farm document and the cursor have come and gone
function done(o, what) { assert.deepStrictEqual(o.errors, [], what + ': console errors'); }

async function desktop(browser) {
  var ctx = await browser.newContext({ viewport: { width: 1280, height: 800 }, reducedMotion: 'reduce' });
  var o = await open(ctx, '/garden'), page = o.page;
  for (var i = 0; i < TITLES.length; i++) {
    var c = await card(page, o.errors);
    assert.strictEqual(c.n, 'Welcome · stop ' + (i + 1) + ' of 3');
    assert.strictEqual(c.title, TITLES[i]);
    assert.strictEqual(c.acts, 0, 'a welcome stop offers nothing to do'); assert.ok(!c.meta, 'and names no holder');
    assert.ok(c.w > 200 && c.h > 60, 'the card is on screen: ' + JSON.stringify(c));
    assert.ok(/gardenio/i.test(c.cur), 'the cursor is on the land: ' + c.cur);
    if (i === 0) assert.strictEqual(c.focus, 'tour-h', 'focus on the first stop\'s title');
    assert.strictEqual(c.next, i < TITLES.length - 1 ? 'Next' : 'Open the steward');
    await shot(page, 'desk-' + (i + 1));
    assert.strictEqual(o.seen, 0, 'nothing is recorded mid-tour');
    await page.locator('#tour .tour-next').click();
  }
  await page.waitForURL(function (u) { return u.pathname === '/garden/c/s/gardenio'; });
  assert.strictEqual(await page.title(), 'Workbench', 'the last stop goes to the land\'s steward');
  assert.strictEqual(o.seen, 1, 'the tour is recorded once');
  done(o, 'desktop');

  // A revisit (any device: the flag is the person's) runs no tour.
  var again = await open(ctx, '/garden');
  await settle(again.page);
  assert.ok(await again.page.locator('#tour').isHidden(), 'a revisit runs no tour');
  assert.strictEqual(again.seen, 0);
  done(again, 'revisit');
  await ctx.close();
}

async function phone(browser) {
  var r = await fetch(url + '/test/welcome/reset', { method: 'POST' });
  assert.strictEqual(r.status, 204);
  var ctx = await browser.newContext(Object.assign({}, pw.devices['iPhone 12'], { viewport: { width: 390, height: 844 }, reducedMotion: 'reduce' }));
  var o = await open(ctx, '/garden'), page = o.page;
  var c = await card(page, o.errors);
  assert.strictEqual(c.title, TITLES[0]);
  assert.ok(c.buttons.every(function (h) { return h >= 44; }), 'every button is 44 px tall: ' + c.buttons);
  await shot(page, 'phone-1');
  await page.keyboard.press('Escape');
  assert.ok(await page.locator('#tour').isHidden(), 'Escape dismisses the tour');
  await page.waitForTimeout(300);
  assert.strictEqual(o.seen, 1, 'a dismissal is recorded too');
  done(o, 'phone');
  await ctx.close();
}

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  try { await desktop(browser); await phone(browser); console.log('welcome browser checks passed'); }
  finally { await browser.close(); }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
