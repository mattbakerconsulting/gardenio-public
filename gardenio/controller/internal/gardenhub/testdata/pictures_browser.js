/* Browser check for a picture's hover preview and the picture viewer (Lands workbench-glass spec WB-18, step 5.15,
   gd-uEEeNMlk1a), run from TestPicturesUnderChromium against /garden on an httptest server with cardWorld, whose bead
   reader holds gd-HarvestSt1 (bead_harvest_test.go): a closed step whose newest grade has four pictures (the .bv-pics grid).
   Usage: node pictures_browser.js <base url> <node_modules dir>. GARDENHUB_SHOTS=<dir> saves a PNG per state, for review.
   (The Commons attachment's preview is checked in commons_browser.js, where the walk uploads one.)

   Desktop, dark: a mouse resting on a grade picture raises the preview with the thumbnail's own src, inside the viewport,
   pointer-transparent, the link described by it, the layout unmoved; leaving takes it down; a keyboard focus raises it at
   once and Escape takes it down while the bead view stays open. A click opens the viewer (?pic=) on that picture of four:
   its counter, its name, a filmstrip, the focus on Next; the keys cycle it, wrapping, and a polite line says where it is;
   the address follows; Back returns to the bead view. A reload of the address shows the picture alone, and a foreign path
   shows none. Phone: a focus raises no preview, a tap opens the viewer with no preview, and a swipe moves it on. Phone landscape 750x340: the picture,
   the bar and the filmstrip all fit the body with no scroll (gd-GJK74AghVE). No console errors. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert'), path = require('path');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));
var shots = process.env.GARDENHUB_SHOTS || '';
var PICS = '#bv-body .bv-pics a', BEAD = 'gd-HarvestSt1';
async function ctxOf(browser, opts) {
  var ctx = await browser.newContext(opts);
  await ctx.addInitScript(function (s) { try { localStorage.setItem('garden.sky', s); } catch (e) { /* no storage */ } }, opts.colorScheme === 'dark' ? 'night' : 'day');
  return ctx;
}
async function pageOf(ctx, q) {
  var page = await ctx.newPage(), errors = [];
  page.on('console', function (m) { if (m.type() === 'error') errors.push(m.text()); });
  page.on('pageerror', function (e) { errors.push(String(e)); });
  await page.goto(url + '/garden' + q);
  await page.locator('#bead').waitFor({ state: 'visible' });
  return { page: page, errors: errors };
}
async function bead(ctx) {
  var v = await pageOf(ctx, '?bead=' + BEAD + '&at=gardenio');
  await v.page.waitForFunction(function (sel) {
    var a = document.querySelectorAll(sel);
    return a.length === 4 && Array.prototype.every.call(a, function (x) { var i = x.querySelector('img'); return i && i.complete && i.naturalWidth > 0; });
  }, PICS);
  return v;
}
function tip(page) {
  return page.evaluate(function () {
    var t = document.getElementById('pic-preview');
    if (!t || t.hidden) return null;
    var r = t.getBoundingClientRect(), cs = getComputedStyle(t);
    return { src: t.querySelector('img').getAttribute('src'), alt: t.querySelector('img').alt, x: r.left, y: r.top, w: r.width, h: r.height, pe: cs.pointerEvents, pos: cs.position, role: t.getAttribute('role'),
      vw: document.documentElement.clientWidth, vh: window.innerHeight, by: (document.querySelector('[aria-describedby=pic-preview]') || {}).className };
  });
}
function viewer(page) {
  return page.evaluate(function () {
    var b = document.getElementById('bead'), q = function (s) { return b.querySelector(s); };
    return { kind: b.dataset.kind, title: document.getElementById('bv-title').textContent, n: (q('.pic-n') || {}).textContent, say: (q('.pic-say') || {}).textContent,
      src: (q('.pic-stage img') || {}).getAttribute ? q('.pic-stage img').getAttribute('src') : null, open: (q('.pic-open') || {}).href, strip: q('.pic-strip') ? q('.pic-strip').children.length : 0,
      stripHidden: q('.pic-strip') ? q('.pic-strip').hidden : null, cur: Array.prototype.map.call(b.querySelectorAll('.pic-thumb'), function (x) { return x.getAttribute('aria-current') === 'true'; }).indexOf(true),
      prevOff: q('.pic-prev') ? q('.pic-prev').disabled : null, nextOff: q('.pic-next') ? q('.pic-next').disabled : null, focus: document.activeElement && document.activeElement.className,
      pic: new URLSearchParams(location.search).get('pic'), bead: new URLSearchParams(location.search).get('bead'), w: b.getBoundingClientRect().width, img: q('.pic-stage img') ? [q('.pic-stage img').getBoundingClientRect().width, q('.pic-stage img').getBoundingClientRect().height, q('.pic-stage img').naturalWidth, q('.pic-stage img').naturalHeight] : null, note: (q('.bv-note') || {}).textContent };
  });
}

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); } catch (e) { console.log(String(e).split('\n')[0]); process.exit(3); }
  try {
    // ---- desktop, dark (C7) ----
    var ctx = await ctxOf(browser, { viewport: { width: 1280, height: 800 }, colorScheme: 'dark' }), v = await bead(ctx), page = v.page;
    var srcs = await page.locator(PICS + ' img').evaluateAll(function (l) { return l.map(function (i) { return i.getAttribute('src'); }); });
    var before = await page.locator(PICS).first().boundingBox();
    await page.hover(PICS + ' >> nth=0');
    await page.waitForFunction(function () { var t = document.getElementById('pic-preview'); return t && !t.hidden; });
    var t = await tip(page);
    assert.strictEqual(t.src, srcs[0], 'the preview shows the thumbnail\'s own src');
    assert.strictEqual(t.alt, '', 'the preview adds no second name');
    assert.strictEqual(t.role, 'tooltip');
    assert.ok(t.x >= 0 && t.y >= 0 && t.x + t.w <= t.vw && t.y + t.h <= t.vh, 'the preview lies inside the viewport: ' + JSON.stringify(t));
    assert.ok(t.w > before.width * 1.5, 'the preview is bigger than the thumbnail: ' + t.w + ' vs ' + before.width);
    assert.deepStrictEqual([t.pe, t.pos], ['none', 'fixed'], 'pointer-transparent and out of the flow');
    assert.strictEqual(await page.locator(PICS).first().getAttribute('aria-describedby'), 'pic-preview', 'the link is described by its preview');
    assert.deepStrictEqual(await page.locator(PICS).first().boundingBox(), before, 'the layout did not move');
    if (shots) await page.screenshot({ path: path.join(shots, 'pictures-preview-desktop-dark.png') });
    await page.mouse.move(640, 790);
    await page.waitForFunction(function () { return document.getElementById('pic-preview').hidden; });
    assert.strictEqual(await page.locator(PICS).first().getAttribute('aria-describedby'), null, 'leaving drops the description');
    // the keyboard: a focus-visible raises it at once; Escape takes it down and leaves the bead view open
    await page.locator(PICS).nth(1).focus();
    await page.keyboard.press('Shift+Tab'); await page.keyboard.press('Tab');
    t = await tip(page);
    assert.ok(t && t.src === srcs[1], 'a keyboard focus raises the preview: ' + JSON.stringify(t));
    await page.keyboard.press('Escape');
    assert.strictEqual(await tip(page), null, 'Escape takes the preview down');
    assert.ok(await page.locator('#bead').isVisible(), '... and only the preview');

    // the viewer: a click on the second picture
    await page.click(PICS + ' >> nth=1');
    await page.waitForSelector('#bead[data-kind=pic] .pic-stage img');
    var s = await viewer(page);
    assert.strictEqual(await tip(page), null, 'the click takes the preview down');
    assert.deepStrictEqual([s.n, s.src, s.pic, s.bead, s.strip, s.cur], ['2 of 4', srcs[1], srcs[1], null, 4, 1], 'the viewer opens on picture 2 of 4: ' + JSON.stringify(s));
    assert.strictEqual(s.title, 'cursorphase, bead view · b2.png', 'named by its grade\'s subject and its file; the counter says where it stands');
    assert.deepStrictEqual(s.img.slice(0, 2), s.img.slice(2), 'a small picture shows at its natural size, not stretched to a blur: ' + JSON.stringify(s.img));
    assert.strictEqual(new URL(s.open).pathname, srcs[1], 'Open whole is the link the thumbnail opened, through the proxy: ' + s.open);
    assert.ok(/pic-next/.test(s.focus), 'the focus is on Next: ' + s.focus);
    assert.ok(s.w > 1200, 'the viewer takes the map window\'s width on a desktop: ' + s.w);
    assert.strictEqual(s.say, '', 'nothing said on opening');
    var unnamed = await page.evaluate(function () {
      var focusable = Array.prototype.slice.call(document.querySelectorAll('#bead a[href], #bead button, #bead [tabindex]:not([tabindex="-1"]), #pic-preview, #pic-preview *'));
      return focusable.filter(function (e) { return e.closest('#pic-preview') ? e.tabIndex >= 0 : !(e.getAttribute('aria-label') || e.textContent.trim()); }).map(function (e) { return e.outerHTML.slice(0, 80); });
    });
    assert.deepStrictEqual(unnamed, [], 'every control in the viewer is named, and the preview takes no focus');
    if (shots) await page.screenshot({ path: path.join(shots, 'pictures-viewer-desktop-dark.png') });
    await page.keyboard.press('ArrowRight');
    s = await viewer(page);
    assert.deepStrictEqual([s.n, s.src, s.pic, s.cur, s.say], ['3 of 4', srcs[2], srcs[2], 2, '3 of 4, cursorphase, bead view · b3.png'], 'ArrowRight: the next, said, the address with it');
    await page.keyboard.press('End'); assert.strictEqual((await viewer(page)).n, '4 of 4');
    await page.keyboard.press('ArrowRight'); assert.strictEqual((await viewer(page)).n, '1 of 4', 'wraps at the end');
    await page.keyboard.press('ArrowLeft'); assert.strictEqual((await viewer(page)).n, '4 of 4', 'and at the start');
    await page.keyboard.press('Home'); assert.strictEqual((await viewer(page)).n, '1 of 4');
    await page.click('#bead .pic-prev'); assert.strictEqual((await viewer(page)).n, '4 of 4', 'Previous wraps too');
    await page.click('#bead .pic-thumb >> nth=2'); s = await viewer(page);
    assert.deepStrictEqual([s.n, s.cur], ['3 of 4', 2], 'a filmstrip picture goes there');
    assert.strictEqual(await page.locator('#bead .pic-thumb').nth(0).getAttribute('aria-label'), 'Picture 1 of 4: cursorphase, bead view · b1.png', 'each filmstrip button is named');
    // Back returns to the bead view it was opened from
    await page.goBack();
    await page.waitForSelector('#bead[data-kind=bead]');
    assert.strictEqual(new URL(page.url()).searchParams.get('bead'), BEAD, 'Back: the bead view again');
    // a reload of a viewer address: the picture alone; a foreign one: none
    var r = await pageOf(ctx, '?pic=' + encodeURIComponent(srcs[3]));
    await r.page.waitForSelector('#bead[data-kind=pic] .pic-stage img');
    s = await viewer(r.page);
    assert.deepStrictEqual([s.n, s.src, s.prevOff, s.nextOff, s.stripHidden], ['1 of 1', srcs[3], true, true, true], 'a reload shows the picture alone: ' + JSON.stringify(s));
    assert.strictEqual(s.title, 'b4.png', 'named by its file');
    var f = await pageOf(ctx, '?pic=' + encodeURIComponent('//evil.example/x.png'));
    s = await viewer(f.page);
    assert.ok(/no picture at this address/.test(s.note) && s.src === null, 'a foreign path shows nothing: ' + JSON.stringify(s));
    await r.page.keyboard.press('Escape');
    assert.ok(await r.page.locator('#bead').isHidden(), 'Escape closes the viewer');
    // a full-size screenshot (1280x800, bigger than the stage): shown whole, shrunk to fit it, its shape kept
    await f.page.close(); await r.page.close(); await page.close();
    var big = await pageOf(ctx, '?pic=' + encodeURIComponent(srcs[3].replace(/b4\.png$/, 'shot-1280x800.png')));
    await big.page.waitForFunction(function () { var i = document.querySelector('#bead[data-kind=pic] .pic-stage img'); return i && i.complete && i.naturalWidth > 0; });
    var fit = await big.page.evaluate(function () {
      var st = document.querySelector('#bead .pic-stage'), i = st.querySelector('img'), b = i.getBoundingClientRect();
      return { w: b.width, h: b.height, sw: st.clientWidth, sh: st.clientHeight };
    });
    assert.ok(fit.w < fit.sw + 1 && fit.h < fit.sh + 1 && (Math.abs(fit.w - fit.sw) < 1 || Math.abs(fit.h - fit.sh) < 1), 'a big picture fits the stage: ' + JSON.stringify(fit));
    assert.ok(Math.abs(fit.w / fit.h - 1.6) < 0.02, 'and keeps its shape: ' + JSON.stringify(fit));
    if (shots) await big.page.screenshot({ path: path.join(shots, 'pictures-viewer-1280x800-desktop-dark.png') });
    assert.deepStrictEqual(v.errors.concat(r.errors, f.errors, big.errors), [], 'desktop: no errors');
    await ctx.close();

    // ---- phone: a tap opens the viewer, no preview; a swipe moves it on ----
    var pctx = await ctxOf(browser, { viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true, colorScheme: 'dark' }), pv = await bead(pctx), pp = pv.page;
    await pp.locator(PICS).nth(1).focus();
    await pp.keyboard.press('Shift+Tab'); await pp.keyboard.press('Tab');
    assert.strictEqual(await tip(pp), null, 'no preview on a focus under a coarse pointer, over the harvest\'s chips (gd-GYSjwuhmed)');
    await pp.tap(PICS + ' >> nth=0');
    await pp.waitForSelector('#bead[data-kind=pic] .pic-stage img');
    assert.strictEqual(await tip(pp), null, 'no preview on touch');
    assert.strictEqual((await viewer(pp)).n, '1 of 4');
    await pp.evaluate(function () {
      var st = document.querySelector('#bead .pic-stage'), r = st.getBoundingClientRect(), y = r.top + r.height / 2;
      var touch = function (x) { return new Touch({ identifier: 1, target: st, clientX: x, clientY: y }); };
      st.dispatchEvent(new TouchEvent('touchstart', { touches: [touch(300)], changedTouches: [touch(300)], bubbles: true }));
      st.dispatchEvent(new TouchEvent('touchend', { touches: [], changedTouches: [touch(160)], bubbles: true }));
    });
    assert.strictEqual((await viewer(pp)).n, '2 of 4', 'a swipe left: the next');
    var ov = await pp.evaluate(function () { return document.documentElement.scrollWidth > window.innerWidth + 1; });
    assert.ok(!ov, 'phone: nothing scrolls sideways');
    if (shots) await pp.screenshot({ path: path.join(shots, 'pictures-viewer-phone-dark.png') });
    assert.deepStrictEqual(pv.errors, [], 'phone: no errors');
    await pctx.close();

    // ---- phone landscape 750x340 (gd-GJK74AghVE): the picture, the bar and the filmstrip are all in the body, no scroll ----
    var lctx = await ctxOf(browser, { viewport: { width: 750, height: 340 }, isMobile: true, hasTouch: true, colorScheme: 'dark' }), lv = await bead(lctx), lp = lv.page;
    await lp.tap(PICS + ' >> nth=1');
    await lp.waitForFunction(function () { var i = document.querySelector('#bead[data-kind=pic] .pic-stage img'); return i && i.complete && i.naturalWidth > 0; });
    var fit = await lp.evaluate(function () {
      var b = document.getElementById('bv-body'), r = b.getBoundingClientRect(), box = function (s) { return document.querySelector('#bead ' + s).getBoundingClientRect(); };
      return { scroll: b.scrollHeight - b.clientHeight, bottom: r.bottom, parts: ['.pic-stage', '.pic-bar', '.pic-strip'].map(function (s) { return [s, box(s).bottom]; }), stage: box('.pic-stage').height, img: box('.pic-stage img').height, natural: document.querySelector('#bead .pic-stage img').naturalHeight };
    });
    assert.ok(fit.scroll <= 1, 'landscape: the viewer body does not scroll, ' + JSON.stringify(fit));
    fit.parts.forEach(function (x) { assert.ok(x[1] <= fit.bottom + 1, 'landscape: ' + x[0] + ' is on screen, ' + JSON.stringify(fit)); });
    assert.ok(fit.stage >= 150 && fit.img === Math.min(fit.natural, fit.stage - 4), 'landscape: the picture has room and shows whole, ' + JSON.stringify(fit));
    if (shots) await lp.screenshot({ path: path.join(shots, 'pictures-viewer-phone-landscape-dark.png') });
    assert.deepStrictEqual(lv.errors, [], 'landscape: no errors');
    await lctx.close();
    console.log('pictures: preview on hover and focus, viewer cycles by key, button, filmstrip and swipe, Back, reload');
  } finally {
    await browser.close();
  }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
