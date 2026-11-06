/* The phone's on-screen keyboard (M1.3, gd-xWWqgcmzXM) in the bead view (gd-KomHHeYfpT), run from TestKeyboardUnderChromium
   against the card world. Usage: node keyboard_browser.js <base url> <node_modules dir>.

   A keyboard resizes only the visual viewport, on iOS and on Chrome 108 and later (docs/lands-mobile.md §2). Chromium's
   emulation has no keyboard, so the walk gives window.visualViewport the height and offset a keyboard leaves (KB, and an
   iOS-style pan of the layout viewport) and fires its resize event, as the browser does. On a 390x844 touch phone a
   decision's Other answer, the comment box, the edit form and the Glass's nudge box stay above the keyboard with their
   submit, every field is 16 px or more (so iOS does not zoom into it), and there are zero page errors. The Commons'
   composer and lightbox were the band's (gone, gd-ZeiSZV7j2Y); the workbench's own phone walk is
   workbench_phone_browser.js. Real iOS is the M4.3 walk's. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));
var KB = 420;

async function open(ctx, at) {
  var page = await ctx.newPage(), errors = [];
  page.on('pageerror', function (e) { errors.push(String(e)); });
  page.on('console', function (m) { if (m.type() === 'error' && !/EventSource|events/.test(m.text())) errors.push(m.text()); });
  await page.goto(url + at);
  return { page: page, errors: errors };
}
function settle(page) { return page.evaluate(function () { return new Promise(function (r) { requestAnimationFrame(function () { requestAnimationFrame(r); }); }); }); }
/* The keyboard up ({ h: the visual viewport's height, top: its offset in the layout viewport }) or down (null). */
async function keyboard(page, kb) {
  await page.evaluate(function (kb) {
    var vv = window.visualViewport;
    delete vv.height; delete vv.offsetTop;
    if (kb) {
      Object.defineProperty(vv, 'height', { configurable: true, get: function () { return kb.h; } });
      Object.defineProperty(vv, 'offsetTop', { configurable: true, get: function () { return kb.top; } });
    }
    vv.dispatchEvent(new Event('resize'));
  }, kb || null);
  await settle(page);
}
/* sel's box, and whether it lies inside the visual viewport */
function inView(page, sel) {
  return page.evaluate(function (sel) {
    var b = document.querySelector(sel).getBoundingClientRect(), vv = window.visualViewport;
    return { top: Math.round(b.top), bottom: Math.round(b.bottom), vv: [vv.offsetTop, vv.offsetTop + vv.height], ok: b.height > 0 && b.top >= vv.offsetTop - 1 && b.bottom <= vv.offsetTop + vv.height + 1 };
  }, sel);
}
async function shows(page, sel, why) { var v = await inView(page, sel); assert.ok(v.ok, why + ': ' + sel + ' ' + JSON.stringify(v)); }
/* every text field on the page whose type is under 16 px, hidden ones too (the probe sees only what shows) */
function zoomers(page) {
  return page.evaluate(function () {
    var q = 'textarea,select,input:not([type=hidden],[type=checkbox],[type=radio],[type=range],[type=color],[type=file],[type=button],[type=submit],[type=reset],[type=image])';
    return Array.prototype.filter.call(document.querySelectorAll(q), function (el) { return parseFloat(getComputedStyle(el).fontSize) < 16; })
      .map(function (el) { return (el.id || el.className || el.name || el.tagName) + ' ' + getComputedStyle(el).fontSize; });
  });
}
/* The bead view's fields (gd-KomHHeYfpT): sel scrolled to the panel's foot and tapped, as a reader does, then the keyboard up
   and panned: the field and go, its submit (when given), inside the visual viewport; with the keyboard down the panel is
   whole again. */
async function above(page, sel, go, why) {
  await page.$eval(go || sel, function (b) { b.scrollIntoView({ block: 'end' }); });
  await page.tap(sel);
  assert.ok(await page.$eval(sel, function (f) { return document.activeElement === f; }), why + ': the tap took the focus');
  await keyboard(page, { h: KB, top: 0 });
  await shows(page, sel, why + ', keyboard up');
  if (go) await shows(page, go, why + ', keyboard up');
  await keyboard(page, { h: KB, top: 150 });
  await shows(page, sel, why + ', keyboard up, panned');
  if (go) await shows(page, go, why + ', keyboard up, panned');
  await keyboard(page, null);
  var b = await page.$eval('#bead', function (p) { return Math.round(p.getBoundingClientRect().bottom); });
  assert.ok(b > 700, why + ': the panel has its height back: its foot at ' + b);
}

/* The bead view on the card world (cardWorld with goldenPorch, as a writer): the open decision d-1's Other answer, the
   comment composer and the edit form, then the Glass's nudge box. */
async function bead(browser) {
  var ctx = await browser.newContext(Object.assign({}, pw.devices['Pixel 5'], { viewport: { width: 390, height: 844 }, reducedMotion: 'reduce' }));
  await ctx.setExtraHTTPHeaders({ 'X-UX-Writer': '1' });
  var o = await open(ctx, '/garden?bead=d-1'), page = o.page;
  await page.locator('#bv-body .act[data-kind=decision] button[data-act=other]').waitFor({ state: 'visible' });
  await page.locator('#bv-body .bw-comment textarea').waitFor({ state: 'visible' });
  await page.waitForFunction(function () { return LAZY.loaded('workbench'); });
  assert.deepStrictEqual(await zoomers(page), [], 'every field in the bead view is 16 px under a finger');
  await page.$eval('#bv-body .act[data-kind=decision] button[data-act=other]', function (b) { b.scrollIntoView({ block: 'end' }); });
  await page.tap('#bv-body .act[data-kind=decision] button[data-act=other]');
  await above(page, '#bv-body .act[data-kind=decision] .free textarea', '#bv-body .act[data-kind=decision] .free button[data-act=resolve]', 'the decision\'s Other');
  await above(page, '#bv-body .bw-comment textarea', '#bv-body .bw-comment button[type=submit]', 'the comment box');
  await page.locator('#bv-body .bw-bar button', { hasText: 'Edit' }).tap();
  await page.locator('#bv-body form.bw-edit #bw-labels').waitFor({ state: 'visible' });
  // the edit form is taller than the panel above a keyboard: the field in focus shows, Save under the description a scroll away
  await above(page, '#bv-body form.bw-edit #bw-labels', null, 'the edit form\'s labels');
  await above(page, '#bv-body form.bw-edit .bw-text', null, 'the edit form\'s description');
  assert.deepStrictEqual(o.errors, [], 'bead view page errors');
  await page.close();

  o = await open(ctx, '/garden?glass=gd-GlassAsk001'); page = o.page;
  await page.locator('#bv-body .gl-nudge textarea').waitFor({ state: 'visible' });
  await page.waitForFunction(function () { return LAZY.loaded('workbench'); });
  assert.deepStrictEqual(await zoomers(page), [], 'every field in the Glass is 16 px under a finger');
  await above(page, '#bv-body .gl-nudge textarea', '#bv-body .gl-nudge button[type=submit]', 'the Glass\'s nudge');
  assert.deepStrictEqual(o.errors, [], 'Glass page errors');
  await ctx.close();
}

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('no Chromium: ' + e.message); process.exit(3); }
  try { await bead(browser); console.log('keyboard walk, bead view: ok'); }
  finally { await browser.close(); }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
