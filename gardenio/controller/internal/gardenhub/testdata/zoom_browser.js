/* Diagnostic (gd-AOBOyh4iu4): does zooming reload /garden or rebuild it from scratch? ZOOM_MODE=desktop emulates a browser
   zoom (devicePixelRatio and CSS viewport change); ZOOM_MODE=phone emulates an iPhone-sized touch screen and pinches the map in
   and out with two fingers. Usage: node zoom_browser.js <base url> <node_modules dir>. Exits 1 on a reload. */
'use strict';
var url = process.argv[2], mods = process.argv[3], mode = process.env.ZOOM_MODE || 'desktop';
var pw = require(require.resolve('playwright-core', { paths: [mods] }));
function snap(page) {
  return page.evaluate(function () {
    var c = window.GARDEN && window.GARDEN.cam ? window.GARDEN.cam() : null, cur = document.querySelector('#hots .hot.cur'), cv = document.getElementById('cv');
    var mem = performance.memory ? Math.round(performance.memory.usedJSHeapSize / 1048576) : -1;
    return { marker: window.__zoomMarker || 0, dpr: window.devicePixelRatio, w: innerWidth, h: innerHeight, cv: cv.width + 'x' + cv.height, cam: c && { z: Math.round(c.z * 1000) / 1000, level: c.level }, cur: cur ? cur.getAttribute('aria-label') : null,
      href: location.href.replace(/^https?:\/\/[^/]+/, ''), labels: document.querySelectorAll('#lbls .sign').length, hots: document.querySelectorAll('#hots .hot').length, dom: document.getElementsByTagName('*').length, heapMB: mem, active: document.activeElement && (document.activeElement.className || document.activeElement.tagName) };
  });
}
async function pinch(cdp, page, cx, cy, spread, steps) {   // spread > 0: fingers part (zoom in); < 0: they close (zoom out)
  var d0 = spread > 0 ? 30 : 160, d1 = d0 + spread, p = function (d) { return [{ x: cx - d / 2, y: cy, id: 0 }, { x: cx + d / 2, y: cy, id: 1 }]; };
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [p(d0)[0]] });
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: p(d0) });
  for (var i = 1; i <= steps; i++) { await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: p(d0 + (d1 - d0) * i / steps) }); await page.waitForTimeout(16); }
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [p(d1)[0]] });
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
}
(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  try {
    var phone = mode === 'phone';
    var ctx = await browser.newContext(phone ? { viewport: { width: 393, height: 852 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true, reducedMotion: 'reduce' } : { viewport: { width: 1280, height: 800 }, reducedMotion: 'reduce' });
    await ctx.addInitScript(function () { try { localStorage.setItem('garden.sky', 'day'); } catch (e) { } });
    var page = await ctx.newPage(), events = [], reqs = [];
    page.on('console', function (m) { if (m.type() === 'error' || m.type() === 'warning') events.push('console.' + m.type() + ': ' + m.text()); });
    page.on('pageerror', function (e) { events.push('pageerror: ' + String(e)); });
    page.on('framenavigated', function (f) { if (f === page.mainFrame()) events.push('NAVIGATED: ' + f.url()); });
    page.on('load', function () { events.push('LOAD'); });
    page.on('request', function (r) { if (!/\/garden\/events/.test(r.url())) reqs.push(r.method() + ' ' + r.url().replace(url, '')); });
    await page.goto(url + '/garden?at=gardenio/gd-planA');
    await page.waitForSelector('#hots .hot.cur', { state: 'attached' });
    await page.waitForTimeout(800);
    await page.evaluate(function () { window.__zoomMarker = 1; });
    console.log('mode ' + mode + ' before: ' + JSON.stringify(await snap(page)));
    events.length = 0; reqs.length = 0;
    var cdp = await ctx.newCDPSession(page), bad = false, label;
    async function report(label) {
      var s = await snap(page);
      console.log(label + ': ' + JSON.stringify(s));
      if (!s.marker) { bad = true; console.log('  !! page state lost (reload)'); }
      if (events.length) { console.log('  events: ' + JSON.stringify(events)); events.length = 0; }
      if (reqs.length) { console.log('  requests: ' + JSON.stringify(reqs)); reqs.length = 0; }
    }
    if (!phone) {
      for (var z of [1.1, 1.25, 1.5, 1.75, 2, 1.75, 1.5, 1.25, 1.1, 1]) {
        await cdp.send('Emulation.setDeviceMetricsOverride', { width: Math.round(1280 / z), height: Math.round(800 / z), deviceScaleFactor: z, mobile: false });
        await page.waitForTimeout(400); await report('zoom ' + z);
      }
    } else {
      var b = await page.evaluate(function () { var r = document.getElementById('board').getBoundingClientRect(); return { x: r.left, y: r.top, w: r.width, h: r.height }; });
      var cx = b.x + b.w / 2, cy = b.y + b.h / 2;
      for (var k = 0; k < 3; k++) {
        await pinch(cdp, page, cx, cy, 130, 12); await page.waitForTimeout(500); await report('pinch in #' + k);
        await pinch(cdp, page, cx, cy, -130, 12); await page.waitForTimeout(500); await report('pinch out #' + k);
      }
      // a finger landing on a hot link, then the pinch
      var a = await page.evaluate(function () { var e = document.querySelector('#hots a.hot'); if (!e) return null; var r = e.getBoundingClientRect(); return { x: r.left + r.width / 2, y: r.top + r.height / 2, href: e.getAttribute('href') }; });
      if (a) { await pinch(cdp, page, a.x, a.y, 130, 12); await page.waitForTimeout(600); await report('pinch in on link ' + a.href); await pinch(cdp, page, a.x, a.y, -130, 12); await page.waitForTimeout(600); await report('pinch out on link'); }
      // a single tap on a leaf link (what a tap after a pinch would be)
      if (a) { await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x: a.x, y: a.y, id: 0 }] }); await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] }); await page.waitForTimeout(600); await report('tap on link'); }
    }
    await browser.close();
    process.exit(bad ? 1 : 0);
  } catch (e) { console.error(e && e.stack || e); try { await browser.close(); } catch (_) { } process.exit(1); }
})();
