/* Browser check that the map-window drawers scroll inside themselves (gd-8q27r5vXYv), run from TestDrawersScrollUnderChromium
   against a live /garden on an httptest server with the fixture farm, a dozen agents on the Porch and a dozen decisions in
   the Mailbox. Usage: node drawer_scroll_browser.js <base url> <node_modules dir>.

   A <details> wraps its content in the ::details-content slot, which is not a flex item with min-height:0 unless garden.css
   makes it one; without that the open drawer is bounded but its content is not, so the list spills out of the window and
   the wheel does nothing. Desktop (1707x842): the Mailbox and Porch panes and the Plans body each end inside their drawer,
   are shorter than their content, and the wheel over them scrolls the last item into view. Phone (390x844): the dock is the
   Porch / Mailbox screen between the header and the tab bar, and it scrolls inside itself the same way. */
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

// scroller: the element that must scroll; drawer: the open <details> it lives in; last: the last item of its list
async function scrolls(page, name, scroller, drawer, last, wheel) {
  var m = function () {
    return page.evaluate(function (s) {
      var e = document.querySelector(s[0]), d = document.querySelector(s[1]), l = document.querySelectorAll(s[2]);
      var r = e.getBoundingClientRect(), dr = d.getBoundingClientRect(), lr = l[l.length - 1].getBoundingClientRect();
      return { ch: e.clientHeight, sh: e.scrollHeight, top: e.scrollTop, bottom: r.bottom, dbottom: dr.bottom, vh: innerHeight,
        rtop: r.top, lastTop: lr.top, lastBottom: lr.bottom, n: l.length };
    }, [scroller, drawer, last]);
  };
  var a = await m();
  assert.ok(a.n > 5, name + ': the fixture lists more than five items: ' + a.n);
  assert.ok(a.bottom <= a.dbottom + 1 && a.dbottom <= a.vh, name + ': the scroller ends inside its drawer, inside the window: ' + JSON.stringify(a));
  assert.ok(a.ch < a.sh, name + ': the scroller is shorter than its content: ' + JSON.stringify(a));
  assert.ok(a.lastTop >= a.bottom, name + ': the last item starts out of view: ' + JSON.stringify(a));
  var r = await page.locator(scroller).boundingBox();
  await page.mouse.move(r.x + r.width / 2, r.y + Math.min(r.height / 2, 120));
  for (var i = 0; i < 20; i++) await page.mouse.wheel(0, wheel || 400);
  await page.waitForFunction(function (s) { var e = document.querySelector(s); return e.scrollTop + e.clientHeight >= e.scrollHeight - 1; }, scroller, { timeout: 5000 });
  var b = await m();
  assert.ok(b.top > a.top, name + ': the wheel scrolls it: ' + JSON.stringify([a, b]));
  assert.ok(b.lastBottom <= b.bottom + 1 && b.lastTop >= b.rtop - 1, name + ': the last item scrolls into view: ' + JSON.stringify(b));
  console.log(name + ': ' + b.n + ' items, ' + a.ch + ' of ' + a.sh + ' px shown, wheel to ' + b.top);
}

async function open(ctx) {
  var page = await ctx.newPage(), errors = [];
  page.on('console', function (m) { if (m.type() === 'error') errors.push(m.text()); });
  page.on('pageerror', function (e) { errors.push(String(e)); });
  await page.goto(url + '/garden');
  await page.waitForFunction(function () { return document.querySelectorAll('#d-mail > li').length > 5 && document.querySelectorAll('#d-porch > li').length > 5; });
  return { page: page, errors: errors };
}

async function desktop(browser) {
  var ctx = await skyCtx(browser, { viewport: { width: 1707, height: 842 }, reducedMotion: 'reduce' });
  var o = await open(ctx), page = o.page;

  await page.click('#dbtn-mail');
  await scrolls(page, 'desktop Mailbox', '#pane-mail', '#dw-dock', '#d-mail > li');
  await page.click('#dbtn-porch');
  await scrolls(page, 'desktop Porch', '#pane-porch', '#dw-dock', '#d-porch > li');
  await page.click('#dbtn-porch');

  await page.click('#cam-menu'); await page.click('#cam-plans');   // Plans is in the camera's Menu (3b R9)
  await page.locator('#plans-slot').waitFor({ state: 'visible' });
  await scrolls(page, 'desktop Plans', '#dw-plans > .dbody', '#dw-plans', '#plans-slot .crop, #plans-slot .tree');

  assert.deepStrictEqual(o.errors.filter(function (e) { return !/404/.test(e); }), [], 'desktop console errors');
  await ctx.close();
}

async function phone(browser) {
  var ctx = await skyCtx(browser, Object.assign({}, pw.devices['iPhone 12'], { viewport: { width: 390, height: 844 }, reducedMotion: 'reduce' }));
  var o = await open(ctx), page = o.page;
  await page.tap('#tabbar [data-go=mail]');
  await page.locator('#pane-mail').waitFor({ state: 'visible' });
  var bar = await page.locator('#tabbar').boundingBox(), dw = await page.locator('#dw-dock').boundingBox();
  assert.ok(dw.y + dw.height <= bar.y + 1, 'phone: the dock sheet ends above the tab bar: ' + JSON.stringify([dw, bar]));
  assert.strictEqual(await page.evaluate(function () { return getComputedStyle(document.getElementById('pane-mail')).overflowY; }), 'visible', 'phone: the pane does not scroll, the screen does');
  await scrolls(page, 'phone Mailbox', '#dock', '#dw-dock', '#d-mail > li');
  assert.deepStrictEqual(o.errors.filter(function (e) { return !/404/.test(e); }), [], 'phone console errors');
  await ctx.close();
}

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  try { await desktop(browser); await phone(browser); console.log('drawer scroll browser checks passed'); }
  finally { await browser.close(); }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
