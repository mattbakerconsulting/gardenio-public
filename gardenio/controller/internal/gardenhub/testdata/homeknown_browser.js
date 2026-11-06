/* Browser check that a strip's letters are on the map (homestead-world-design.md 28.2, gd-2vAMq1HHI9), run from
   TestStripKnownUnderChromium against a live /garden on an httptest server with the goldens' world plus 60 loose asks in
   gardenio, more than its Mailbox cell holds. Usage: node homeknown_browser.js <base url> <node_modules dir>.

   The bead view's "Show on the map" and a bead link's fly read GARDEN.known(id).onMap. An ask the strip places is on the
   map, and GARDEN.fly puts the cursor on it; one past the Mailbox's capacity (its "+N") is not, and GARDEN.fly finds nothing. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); } catch (e) { console.error(String(e)); process.exit(3); }
  try {
    var ctx = await browser.newContext({ viewport: { width: 1280, height: 800 }, reducedMotion: 'reduce' });
    await ctx.addInitScript(function () { try { localStorage.setItem('garden.scope', 'all'); } catch (e) { /* no storage: Everyone is the default without a viewer */ } });
    var page = await ctx.newPage(), errors = [];
    page.on('pageerror', function (e) { errors.push(String(e)); });
    await page.goto(url + '/garden?at=gardenio');
    await page.waitForFunction(function () { return window.GARDEN && window.GARDEN.known('gd-loose00').onMap; }, null, { timeout: 15000 });
    var r = await page.evaluate(function () {
      var ids = Array.prototype.map.call(document.querySelectorAll('#l-decision > li[id^="i-gd-loose"]'), function (li) { return li.id.slice(2); }), on = [], off = [], flew = [], stuck = [];
      ids.forEach(function (id) { (window.GARDEN.known(id).onMap ? on : off).push(id); });
      on.forEach(function (id) { if (!window.GARDEN.fly(id)) stuck.push(id); });
      off.forEach(function (id) { if (window.GARDEN.fly(id)) flew.push(id); });
      return { ids: ids.length, on: on.length, off: off.length, stuck: stuck, flew: flew };
    });
    assert.strictEqual(r.ids, 60, 'the Ledger lists the 60 loose asks: ' + JSON.stringify(r));
    assert.ok(r.on > 0 && r.off > 0, 'the Mailbox places some and counts the rest in its +N: ' + JSON.stringify(r));
    assert.deepStrictEqual(r.stuck, [], 'every placed letter is a cursor stop');
    assert.deepStrictEqual(r.flew, [], 'no letter past the Mailbox\'s capacity is');
    assert.deepStrictEqual(errors, [], 'no page errors');
    console.log('homeknown: ' + r.on + ' on the map, ' + r.off + ' past the Mailbox: ok');
  } finally { await browser.close(); }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
