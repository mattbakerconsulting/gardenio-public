/* The band's old Commons addresses in a browser (gd-3NuXXEFSgk), run from TestCommonsOldAddressUnderChromium against the
   goldens' world. Usage: node commons_old_address_browser.js <base url> <node_modules dir> <pinned clock, unix ms>.

   A channel's address, /garden/c/{land}/{channel} (by name or by id), and the channel list, /garden/c, are links in Slack
   and in beads; the Commons answers them with a redirect to the land's steward (gardenio/docs/adr-commons-band-replaced.md).
   Each is opened twice, once plain and once under the goldens' pin (golden_pin_browser.js), which fulfills a navigation
   under /garden/c/ itself: either way the browser ends at the steward's address and the workbench draws the steward there,
   not node.html at the old address with an empty tree: the Steward row, and gardenio's steward itself (meadow has none in
   this world). Zero page errors. */
'use strict';
var path = require('path'), assert = require('assert');
var url = process.argv[2], mods = process.argv[3], now = Number(process.argv[4]);
var pw = require(require.resolve('playwright-core', { paths: [mods] }));
var { pin } = require(path.join(__dirname, 'golden_pin_browser.js'));

var OLD = [
  ['/garden/c/gardenio/general', '/garden/c/s/gardenio', true],
  ['/garden/c/gardenio/gd-chGeneral', '/garden/c/s/gardenio', true],
  ['/garden/c/meadow/general', '/garden/c/s/meadow', false],
  ['/garden/c', '/garden/c/s', true],
];

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); } catch (e) { console.error(String(e.message || e).split('\n')[0]); process.exit(3); }
  var errs = [];
  try {
    for (var pinned of [false, true]) {
      for (var [from, to, steward] of OLD) {
        var what = from + (pinned ? ' (pinned)' : '');
        var ctx = await browser.newContext({ viewport: { width: 360, height: 780 }, isMobile: true, hasTouch: true });
        var page = await ctx.newPage();
        page.on('pageerror', function (e) { errs.push(what + ': ' + e); });
        if (pinned) await pin(page, url, now, '');
        await page.goto(url + from);
        assert.strictEqual(new URL(page.url()).pathname, to, what + ' ends at the steward\'s address');
        await page.waitForSelector('#wb2 .wb2-swrow', { state: 'attached', timeout: 10000 });
        if (steward) {
          await page.waitForFunction(function () { return COMMONS2.state().steward; }, null, { timeout: 10000 })
            .catch(async function () { assert.fail(what + ': the workbench draws no steward: ' + JSON.stringify(await page.evaluate(function () { return COMMONS2.state(); }))); });
        }
        if (pinned) assert.ok(await page.locator('#golden-no-tap-highlight').count(), what + ': the pin still dresses the steward\'s page');
        await ctx.close();
      }
    }
    assert.deepStrictEqual(errs, [], 'page errors');
    console.log('ok: ' + OLD.length + ' old addresses land on the steward, plain and pinned');
  } finally {
    await browser.close();
  }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
