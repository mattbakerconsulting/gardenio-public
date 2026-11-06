/* Browser check that a cost move over /garden/farm/events repaints every figure in place (gd-rtE07tg6Nh), run from
   TestCostPatchUnderChromium against /garden on cardWorld, whose POST /test/farm/cost?step=up|down moves the spend.
   Usage: node costs_patch_browser.js <base url> <node_modules dir>.

   Two pages, neither reloaded: /garden with the Plans drawer open and /garden?plan=gd-planA's cockpit. Before: the header
   purse's Fleet row says $0.00 today, plan A's crop and its cockpit say "$12.35, 3 gardeners", and g-ask-b's Porch row
   has no chip (nothing counted yet). Step up (the patch takes cost from the farm's cost blob, its plans and its
   gardeners): the purse says $12.50 today on both pages; plan A's crop and cockpit "$19.25, 2 gardeners"; g-ask-b's row
   the chip "$4.10+" read "spent $4.10, at least". Step down (the rollup errors, the clock passes planspend.StaleAfter):
   the purse row is stale, a dash for today and "as of" the last good read, never $0; plan A's crop and cockpit say
   "· stale" and carry the stale class; the chip keeps its figure. No console errors. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));

function watch(page, errors) {
  page.on('console', function (m) { if (m.type() === 'error') errors.push(m.text()); });
  page.on('pageerror', function (e) { errors.push(String(e)); });
}
/* What each page shows of the spend, read off the DOM (the Porch's rows are kept while its drawer is shut). */
function seen(page) {
  return page.evaluate(function () {
    function q(s) { return document.querySelector(s); }
    var row = q('#purse .pu-row'), crop = q('#plans-slot .crop[data-id="gd-planA"] .crop-spend'), pv = q('#bv-body .pv-spend'), chip = q('#d-porch li[data-gid="gd-gAskB"] .gd-cost');
    return {
      loaded: window.__loaded, purse: row ? row.textContent : null, purseStale: !!(row && row.classList.contains('stale')),
      today: row ? row.querySelector('.pu-v').textContent : null, asOf: row && row.querySelector('.pu-s') ? row.querySelector('.pu-s').textContent : '',
      crop: crop ? crop.textContent : null, cropStale: !!(crop && crop.classList.contains('stale')),
      cockpit: pv ? pv.textContent : null, cockpitStale: !!(pv && pv.classList.contains('stale')),
      chip: chip ? chip.textContent : null, chipSay: chip ? chip.getAttribute('aria-label') : null
    };
  });
}
async function until(page, what, test) {
  try { await page.waitForFunction(test, null, { timeout: 15000 }); }
  catch (e) { throw new Error(what + ': ' + JSON.stringify(await seen(page))); }
}
async function step(page, name) {
  var r = await page.request.post(url + '/test/farm/cost?step=' + name);
  assert.strictEqual(r.status(), 204, 'the farm published the ' + name + ' patch: ' + await r.text());
}

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  try {
    var ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } }), errors = [];
    var map = await ctx.newPage(), plan = await ctx.newPage();
    watch(map, errors); watch(plan, errors);
    await map.goto(url + '/garden');
    await map.waitForSelector('#cursor-bar .cb-me');
    await map.click('#cam-menu'); await map.click('#cam-plans');
    await map.waitForSelector('#plans-slot .crop[data-id="gd-planA"] .crop-spend');
    await plan.goto(url + '/garden?plan=gd-planA');
    await plan.waitForSelector('#bv-body .pv-spend');
    for (var p of [map, plan]) {
      await p.waitForSelector('#purse:not([hidden]) .pu-row');
      await p.evaluate(function () { window.__loaded = 1; });   // gone if the page reloads
    }
    var a = await seen(map), b = await seen(plan);
    assert.strictEqual(a.today, '$0.00', 'before: the rollup answered with no spend: ' + a.purse);
    assert.strictEqual(a.crop, '$12.35, 3 gardeners');
    assert.strictEqual(b.cockpit, 'Spend: $12.35, 3 gardeners');
    assert.strictEqual(a.chip, null, 'before: g-ask-b has nothing counted, so no chip');

    await step(map, 'up');
    await until(map, 'the map after up', function () {
      var r = document.querySelector('#purse .pu-v'), c = document.querySelector('#plans-slot .crop[data-id="gd-planA"] .crop-spend');
      return r && r.textContent === '$12.50' && c && c.textContent === '$19.25, 2 gardeners' && !!document.querySelector('#d-porch li[data-gid="gd-gAskB"] .gd-cost');
    });
    await until(plan, 'the cockpit after up', function () {
      var r = document.querySelector('#purse .pu-v'), s = document.querySelector('#bv-body .pv-spend');
      return r && r.textContent === '$12.50' && s && s.textContent === 'Spend: $19.25, 2 gardeners';
    });
    a = await seen(map); b = await seen(plan);
    assert.deepStrictEqual([a.chip, a.chipSay], ['$4.10+', 'spent $4.10, at least'], 'the chip, a lower bound while g-ask-b runs');
    assert.ok(!a.purseStale && !a.cropStale && !b.cockpitStale, 'nothing stale after up: ' + JSON.stringify([a, b]));
    assert.ok(/^Fleet\$12\.50 today/.test(a.purse), 'the purse row: ' + a.purse);

    await step(map, 'down');
    await until(map, 'the map after down', function () {
      var r = document.querySelector('#purse .pu-row'), c = document.querySelector('#plans-slot .crop[data-id="gd-planA"] .crop-spend');
      return r && r.classList.contains('stale') && c && c.classList.contains('stale');
    });
    await until(plan, 'the cockpit after down', function () {
      var r = document.querySelector('#purse .pu-row'), s = document.querySelector('#bv-body .pv-spend');
      return r && r.classList.contains('stale') && s && s.classList.contains('stale');
    });
    a = await seen(map); b = await seen(plan);
    for (var v of [a, b]) {
      assert.strictEqual(v.today, '—', 'the rollup down: a dash for today, never $0: ' + v.purse);
      assert.ok(/^ as of \d\d:\d\d UTC$/.test(v.asOf), 'the stale line names the last good read: ' + JSON.stringify(v.asOf));
    }
    assert.strictEqual(a.crop, '$19.25, 2 gardeners · stale');
    assert.strictEqual(b.cockpit, 'Spend: $19.25, 2 gardeners · stale');
    assert.strictEqual(a.chip, '$4.10+', 'the chip keeps its figure');
    assert.deepStrictEqual([a.loaded, b.loaded], [1, 1], 'no page reloaded');
    assert.deepStrictEqual(errors, [], 'no console errors');
    await ctx.close();
    console.log('cost patch browser checks passed');
  } finally { await browser.close(); }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
