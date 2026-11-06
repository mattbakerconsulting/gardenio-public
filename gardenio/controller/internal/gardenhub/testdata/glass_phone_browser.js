/* Browser check that the Glass is a terminal you can use on a phone (M2.3, gd-2yJFWJnO1I; gardenio/docs/lands-mobile.md §3),
   run from TestGlassPhoneUnderChromium against a live /garden on an httptest server with the card world, whose
   gd-GlassKey001 offers attach. Usage: node glass_phone_browser.js <base url> <node_modules dir>.

   On an iPhone 12 (390x844, touch), once a tap on the terminal attaches:
     1. the key row has Esc, Tab, Ctrl, the four arrows and Paste, each a 44 px target, on one row;
     2. a tap on a key sends it, and the terminal keeps the focus (so the phone keeps its keyboard up);
     3. Ctrl, then a letter typed, sends that Ctrl chord (Ctrl-C asks first), and Ctrl lets go after it;
     4. Paste types the clipboard, its line break as Enter;
     5. a two-finger spread on the terminal sizes its glyphs up, not the page, and the size toggle puts them back.
   On a desktop (no touch) those keys are not shown.
   Each pinch is a CDP Input.dispatchTouchEvent sequence (Playwright has no pinch). */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));
var GLASS = '/garden?glass=gd-GlassKey001';

/* The /send posts so far, as their form fields. */
function sends(page) {
  var out = [];
  page.on('request', function (r) {
    if (r.method() === 'POST' && /\/garden\/glass\/[^/]+\/send$/.test(r.url())) out.push(Object.fromEntries(new URLSearchParams(r.postData() || '')));
  });
  return out;
}
/* Waits for a /send whose fields include want, polled (never slept for, gd-bNZrDyCr1m). */
async function sent(list, want, what) {
  for (var end = Date.now() + 20000; Date.now() < end;) {
    if (list.some(function (f) { return Object.keys(want).every(function (k) { return f[k] === want[k]; }); })) return;
    await new Promise(function (r) { setTimeout(r, 25); });
  }
  assert.fail(what + ': no send with ' + JSON.stringify(want) + ' in ' + JSON.stringify(list));
}
async function attach(page, tap) {
  await page.goto(url + GLASS);
  await page.waitForFunction(function () {
    var rows = document.querySelector('#bv-body .gl-x .xterm-rows');
    return !!rows && rows.textContent.indexOf('token exchange for agent pods') >= 0 && !!document.querySelector('#bv-body .gl-term.can-type');
  });
  var term = page.locator('#bv-body .gl-term .gl-x');
  if (tap) await term.tap(); else await term.click();
  await page.waitForFunction(function () { return !!document.querySelector('#bv-body .gl-attached.mine') && !!document.querySelector('#bv-body .gl-letgo'); });
}
function focused(page) { return page.evaluate(function () { var a = document.activeElement; return !!a && a.classList.contains('xterm-helper-textarea'); }); }
function glyphs(page) { return page.evaluate(function () { return parseFloat(getComputedStyle(document.querySelector('#bv-body .gl-x .xterm-rows')).fontSize); }); }

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  try {
    var errors = [];
    var ctx = await browser.newContext(Object.assign({}, pw.devices['iPhone 12'], { viewport: { width: 390, height: 844 }, reducedMotion: 'reduce' }));
    await ctx.grantPermissions(['clipboard-read', 'clipboard-write'], { origin: url });
    var page = await ctx.newPage();
    page.on('pageerror', function (e) { errors.push(String(e)); });
    var posts = sends(page);
    await attach(page, true);

    // 1. the keys a phone's keyboard lacks, 44 px each, on the one row
    var keys = await page.evaluate(function () {
      return Array.prototype.map.call(document.querySelectorAll('#bv-body .gl-keybar .gl-k'), function (b) {
        var r = b.getBoundingClientRect();
        return { t: b.textContent, w: r.width, h: r.height, top: Math.round(r.top) };
      });
    });
    assert.deepStrictEqual(keys.map(function (k) { return k.t; }), ['Esc', 'Tab', 'Ctrl', '↑', '↓', '←', '→', 'Paste']);
    keys.forEach(function (k) { assert.ok(k.w >= 44 && k.h >= 44, k.t + ' is a 44 px target: ' + JSON.stringify(k)); });
    assert.strictEqual(new Set(keys.map(function (k) { return k.top; })).size, 1, 'one row, never wrapped: ' + JSON.stringify(keys));

    // 2. a key goes at once and the terminal keeps the focus
    await page.locator('#bv-body .gl-k', { hasText: '↑' }).tap();
    await sent(posts, { type: 'keys', keys: 'Up' }, 'the up arrow');
    await page.locator('#bv-body .gl-k', { hasText: 'Tab' }).tap();
    await sent(posts, { type: 'keys', keys: 'Tab' }, 'Tab');
    assert.ok(await focused(page), 'after a key the terminal keeps the focus, so the phone keeps its keyboard');

    // 3. Ctrl, then c: Ctrl-C, which asks first; Ctrl lets go after one letter
    var ctrl = page.locator('#bv-body .gl-k', { hasText: 'Ctrl' });
    await ctrl.tap();
    assert.strictEqual(await ctrl.getAttribute('aria-pressed'), 'true', 'Ctrl is held');
    await page.keyboard.type('c');
    await sent(posts, { type: 'keys', keys: 'Ctrl-C' }, 'Ctrl then c');
    await page.waitForFunction(function () { var a = document.querySelector('#bv-body .gl-ask'); return !!a && !a.hidden && a.textContent.indexOf('Ctrl-C') >= 0; });
    await page.locator('#bv-body .gl-ask-no').tap();
    assert.strictEqual(await ctrl.getAttribute('aria-pressed'), 'false', 'Ctrl lets go after the letter');
    assert.ok(!posts.some(function (f) { return f.type === 'input' && /c/.test(f.text); }), 'the c was not typed as text: ' + JSON.stringify(posts));

    // 4. Paste types the clipboard, a line break as Enter
    await page.evaluate(function () { return navigator.clipboard.writeText('git status\n'); });
    await page.locator('#bv-body .gl-k', { hasText: 'Paste' }).tap();
    await sent(posts, { type: 'input', text: 'git status' }, 'Paste');
    await sent(posts, { type: 'keys', keys: 'Enter' }, 'the pasted line break');

    // 5. a spread on the terminal doubles its glyphs, the page unzoomed; the size toggle puts them back
    var f0 = await glyphs(page);
    var box = await page.locator('#bv-body .gl-term').boundingBox();
    var cdp = await ctx.newCDPSession(page), m = { x: Math.round(box.x + box.width / 2), y: Math.round(box.y + Math.min(box.height, 300) / 2) };
    function at(dx, k) { return { x: m.x + Math.round(dx * k), y: m.y }; }
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [Object.assign({ id: 1 }, at(-40, 1)), Object.assign({ id: 2 }, at(40, 1))] });
    for (var i = 1; i <= 12; i++) {
      var k = 1 + i / 12;
      await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [Object.assign({ id: 1 }, at(-40, k)), Object.assign({ id: 2 }, at(40, k))] });
    }
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
    await page.waitForFunction(function (f) {
      var r = document.querySelector('#bv-body .gl-x .xterm-rows');
      return !!r && parseFloat(getComputedStyle(r).fontSize) >= f * 1.8;
    }, f0);
    var after = await page.evaluate(function () {
      return { scale: window.visualViewport ? window.visualViewport.scale : 1, actual: document.querySelector('#bv-body .gl-term').classList.contains('actual'),
        toggle: document.querySelector('#bv-body .gl-size').textContent };
    });
    assert.deepStrictEqual(after, { scale: 1, actual: true, toggle: 'Fit to the panel' }, 'the terminal grew, at Actual size, and the page did not zoom');
    await page.locator('#bv-body .gl-size').tap();
    await page.waitForFunction(function () { return parseFloat(getComputedStyle(document.querySelector('#bv-body .gl-x .xterm-rows')).fontSize) === 13; });
    console.log('phone: ' + keys.length + ' touch keys; a spread took the glyphs from ' + f0 + ' px up, the toggle back to 13');
    await ctx.close();

    // a desktop has a keyboard: no touch keys
    var desk = await browser.newContext({ viewport: { width: 1280, height: 900 } });
    var dp = await desk.newPage();
    dp.on('pageerror', function (e) { errors.push(String(e)); });
    await attach(dp, false);
    var shown = await dp.evaluate(function () {
      return Array.prototype.filter.call(document.querySelectorAll('#bv-body .gl-keybar .gl-k'), function (b) { return b.getClientRects().length > 0; }).length;
    });
    assert.strictEqual(shown, 0, 'a desktop shows no touch keys');
    await desk.close();

    assert.deepStrictEqual(errors, [], 'page errors');
    console.log('glass phone checks passed');
  } finally { await browser.close(); }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
