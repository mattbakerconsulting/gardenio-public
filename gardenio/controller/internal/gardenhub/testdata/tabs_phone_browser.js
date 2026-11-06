/* Browser check for the phone's navigation (M1.4, gd-4Y2qOgfmJE; gardenio/docs/lands-mobile.md §2, the home-screen app has no
   Back button), run from TestPhoneTabsUnderChromium against a live /garden on an httptest server with the card world and a live
   Commons, seen by a viewer who may watch. Usage: node tabs_phone_browser.js <base url> <node_modules dir>.

   On a phone (360x780, touch):
     1. the tab bar is Farm, Commons, Porch, Mail, Workbench and Glass, each 44 px tall, one row, every word whole;
     2. the Workbench tab opens the viewer's inbox over the map and the Glass tab the Wall; a tab switch pushes no history;
     3. another tab closes the panel, and the Workbench tab opens the one it last showed again;
     4. the close returns to the screen the panel was opened from (the Mailbox), the focus on the tab;
     5. a deep link to a bead or plan opens its panel with the Workbench tab marked, and to a Glass with the Glass tab
        marked; Back from a deep-linked panel (or its close) lands on the Farm, not off the page;
     6. the Commons tab goes to the workbench's page, the viewer's steward (/garden/c/s; the band's sheet is gone,
        gd-ZeiSZV7j2Y).
   Held sideways (750x340) the six tabs are a rail down the left edge, each 44 px or more. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));
var TABS = ['farm', 'commons', 'porch', 'mail', 'workbench', 'glass'];

/* What the page shows: the tab marked, the panel ({kind}:{id} or ''), the screen, the address and history's length. */
function now(page) {
  return page.evaluate(function () {
    var on = Array.prototype.filter.call(document.querySelectorAll('#tabbar button'), function (b) { return b.getAttribute('aria-current') === 'page'; }).map(function (b) { return b.dataset.go; });
    var p = window.WORKBENCH && WORKBENCH.current && WORKBENCH.current();
    return { on: on.join(','), panel: p && !document.getElementById('bead').hidden ? p.kind + ':' + p.id : '', screen: document.body.dataset.screen,
      url: location.pathname + location.search + location.hash, len: history.length };
  });
}
/* Polls until the page shows want's fields (never slept for, gd-bNZrDyCr1m). */
async function shows(page, want, what) {
  var got;
  for (var end = Date.now() + 15000; Date.now() < end;) {
    got = await now(page);
    if (Object.keys(want).every(function (k) { return got[k] === want[k]; })) return got;
    await new Promise(function (r) { setTimeout(r, 25); });
  }
  assert.fail(what + ': want ' + JSON.stringify(want) + ', got ' + JSON.stringify(got));
}
async function tap(page, go) { await page.tap('#tabbar [data-go=' + go + ']'); }

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  try {
    var errors = [];
    var ctx = await browser.newContext({ viewport: { width: 360, height: 780 }, hasTouch: true, isMobile: true, reducedMotion: 'reduce' });
    var page = await ctx.newPage();
    page.on('pageerror', function (e) { errors.push(String(e)); });
    await page.goto(url + '/garden');
    await shows(page, { on: 'farm', panel: '' }, 'the Farm first');

    // 1. six tabs on one row at 360 px, 44 px tall, no word cut
    var tabs = await page.$$eval('#tabbar button', function (bs) {
      return bs.map(function (b) { var r = b.getBoundingClientRect(), t = b.cloneNode(true); t.querySelectorAll('.pip,.sr').forEach(function (x) { x.remove(); }); return { go: b.dataset.go, top: Math.round(r.top), w: r.width, h: r.height, right: r.right, word: t.textContent.trim(), cut: b.scrollWidth > b.clientWidth + 6 }; });
    });
    assert.deepStrictEqual(tabs.map(function (t) { return t.go; }), TABS, 'the tab bar names every surface');
    assert.deepStrictEqual(tabs.map(function (t) { return t.word; }), ['Farm', 'Commons', 'Porch', 'Mail', 'Bench', 'Glass']);
    tabs.forEach(function (t) {
      assert.ok(t.h >= 44 && t.top === tabs[0].top && t.right <= 360, 'a 44 px tab on the one row, on screen: ' + JSON.stringify(t));
      assert.ok(!t.cut, 'its word fits: ' + JSON.stringify(t));
    });
    assert.strictEqual(await page.getAttribute('#tabbar [data-go=workbench]', 'aria-label'), 'Workbench');
    assert.strictEqual(await page.$eval('#tabbar [data-go=mail]', function (b) { return b.textContent.replace(/\s+/g, ' ').trim(); }).then(function (s) { return s.indexOf('Mailbox'); }), 0, 'the Mail tab is read as Mailbox');
    var len = (await now(page)).len;

    // 2. the Workbench tab: the inbox over the map; the Glass tab: the Wall; neither pushes history
    await tap(page, 'workbench');
    await shows(page, { on: 'workbench', panel: 'queue:inbox', screen: 'farm' }, 'the Workbench tab');
    await tap(page, 'glass');
    var g = await shows(page, { on: 'glass', panel: 'glass:wall' }, 'the Glass tab');
    assert.strictEqual(g.len, len, 'a tab switch pushes no history entry');

    // 3. another tab closes the panel; the Workbench tab opens the one it last showed
    await tap(page, 'porch');
    await shows(page, { on: 'porch', panel: '', screen: 'porch' }, 'the Porch tab closes the panel');
    await tap(page, 'workbench');
    await shows(page, { on: 'workbench', panel: 'queue:inbox' }, 'the Workbench tab opens its last panel again');

    // 4. from the Mailbox: the Glass tab, then the close, back on the Mailbox with the focus on the tab
    await tap(page, 'mail');
    await shows(page, { on: 'mail', panel: '', screen: 'mail' }, 'the Mail tab');
    await tap(page, 'glass');
    await shows(page, { on: 'glass', panel: 'glass:wall' }, 'the Glass tab from the Mailbox');
    await page.tap('#bv-x');
    await shows(page, { on: 'mail', panel: '', screen: 'mail' }, 'the close returns to the Mailbox');
    assert.strictEqual(await page.evaluate(function () { return document.activeElement && document.activeElement.dataset.go; }), 'glass', 'the focus goes back to the tab that opened it');

    // 5. deep links open their pane, and Back from one lands on the Farm
    await page.goto(url + '/garden?plan=gd-planA');
    await shows(page, { on: 'workbench', panel: 'plan:gd-planA' }, 'a plan link opens the plan, the Workbench tab marked');
    await page.goBack();
    await shows(page, { on: 'farm', panel: '', screen: 'farm' }, 'Back from a deep-linked plan is the Farm');
    assert.ok(/\/garden(\?|$)/.test((await now(page)).url) && !/plan=/.test((await now(page)).url), 'on /garden without the panel: ' + (await now(page)).url);
    await page.goto(url + '/garden?bead=gd-planA');
    await shows(page, { on: 'workbench', panel: 'bead:gd-planA' }, 'a bead link opens the bead view');
    await page.tap('#bv-x');
    var f = await shows(page, { on: 'farm', panel: '' }, 'its close is the Farm');
    await page.goto(url + '/garden?glass=gd-GlassKey001');
    await shows(page, { on: 'glass', panel: 'glass:gd-GlassKey001' }, 'an agent\'s Glass link, the Glass tab marked');
    assert.deepStrictEqual(errors, [], 'phone page errors');

    // 6. the Commons tab goes to the workbench's page
    await page.goto(url + '/garden');
    await shows(page, { on: 'farm', panel: '' }, 'the Farm again');
    await tap(page, 'commons');
    await page.waitForURL(function (u) { return u.pathname === '/garden/c/s'; });
    await ctx.close();

    // sideways: the rail down the left edge holds all six
    ctx = await browser.newContext({ viewport: { width: 750, height: 340 }, hasTouch: true, isMobile: true, reducedMotion: 'reduce' });
    page = await ctx.newPage();
    await page.goto(url + '/garden');
    await page.waitForSelector('#tabbar button[data-go=glass]', { state: 'visible' });
    var rail = await page.$$eval('#tabbar button', function (bs) { return bs.map(function (b) { var r = b.getBoundingClientRect(); return { go: b.dataset.go, left: Math.round(r.left), top: r.top, bottom: r.bottom, h: r.height }; }); });
    rail.forEach(function (t, i) { assert.ok(t.h >= 44 && t.left === rail[0].left && t.bottom <= 340 && (!i || t.top > rail[i - 1].top), 'a 44 px tab down the rail: ' + JSON.stringify(t)); });
    await ctx.close();
    console.log('phone tab bar checks passed');
  } finally { await browser.close(); }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
