/* Browser check for the witness (web/witness.js, gd-TNC80dPgv3), run from TestWitnessUnderChromium against a live /garden
   on an httptest server with the fixture farm. Usage: node witness_browser.js <base url> <node_modules dir>.
   Phone 393x852 at DPR 3. Off by default: /garden says nothing. /garden?witness=1 switches it on for the browser: the first
   load says so; a viewport resize is said in the toast; a reload says it is load #2, reloaded, closed first (pagehide seen),
   with the last event and the camera; ?witness=0 switches it off again and a load says nothing. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));

function toast(page) { return page.evaluate(function () { var t = document.getElementById('toast'); return t && !t.hidden ? t.textContent : ''; }); }

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  try {
    var ctx = await browser.newContext({ viewport: { width: 393, height: 852 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true, reducedMotion: 'reduce' });
    await ctx.addInitScript(function () { try { localStorage.setItem('garden.sky', 'day'); } catch (e) { /* no storage */ } });
    var page = await ctx.newPage(), errs = [];
    page.on('pageerror', function (e) { errs.push(String(e)); });

    // off by default
    await page.goto(url + '/garden?at=gardenio/gd-planA');
    await page.waitForSelector('#hots .hot.cur', { state: 'attached' });
    await page.waitForTimeout(300);
    assert.ok(!/witness/.test(await toast(page)), 'the witness speaks while off: ' + await toast(page));

    // on: the first load says so
    await page.goto(url + '/garden?witness=1&at=gardenio/gd-planA');
    await page.waitForSelector('#hots .hot.cur', { state: 'attached' });
    await page.waitForTimeout(300);
    var t1 = await toast(page);
    assert.ok(/witness on: this is the first load/.test(t1), 'the first load on: ' + t1);
    console.log('on: ' + t1);

    // an event: the viewport resizes
    await page.setViewportSize({ width: 393, height: 870 });
    await page.waitForTimeout(1300);
    var t2 = await toast(page);
    assert.ok(/window 393x870 @L\d z[\d.]+/.test(t2), 'a resize is said with the camera: ' + t2);
    console.log('event: ' + t2);

    // a reload: load #2, reloaded, closed first, with the last event
    await page.reload();
    await page.waitForSelector('#hots .hot.cur', { state: 'attached' });
    await page.waitForTimeout(300);
    var t3 = await toast(page);
    assert.ok(/witness: load #2, reloaded, the page was closed first \(pagehide seen\); last seen \d+s ago at level \d z [\d.]+; last events: .*window 393x870 @L\d z[\d.]+ \/ hidden/.test(t3), 'the reload verdict: ' + t3);
    console.log('reload: ' + t3);

    // off again
    await page.goto(url + '/garden?witness=0&at=gardenio/gd-planA');
    await page.waitForSelector('#hots .hot.cur', { state: 'attached' });
    await page.waitForTimeout(300);
    assert.ok(!/witness/.test(await toast(page)), 'the witness speaks after ?witness=0: ' + await toast(page));
    await page.goto(url + '/garden?at=gardenio/gd-planA');
    await page.waitForSelector('#hots .hot.cur', { state: 'attached' });
    await page.waitForTimeout(300);
    assert.ok(!/witness/.test(await toast(page)), 'the witness stays on after ?witness=0: ' + await toast(page));
    assert.deepStrictEqual(errs, [], 'page errors');
    await browser.close();
  } catch (e) { console.error(e && e.stack || e); try { await browser.close(); } catch (_) { /* already gone */ } process.exit(1); }
})();
