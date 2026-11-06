/* The splash's quality tiers in a real browser (plan gd-EJGA3zDGCk step 9), run from TestSplashQualityUnderChromium against
   /garden/splash on an httptest server. Usage: node splash_quality_browser.js <base url> <node_modules dir>.

   The phone: the golden harness's phone (an iPhone 12, 390x844, touch) on the page pinned to noon with the wind blowing
   (?t=&wind=1), no ?q=. It starts at the low tier against the phone's 33 ms budget: 8,000 blades, one 1024 px cascade, the
   pond's own water, the mid ground's trees as impostors, 1 device px a CSS px. After its first frame the probe's log() opens
   a 10 s run and closes it, and the frame times are read from the line it logs to the console ("splash: frame <mean> ms
   (p95 <p95>) ... low tier, budget 33 ms"). Where the GL is hardware the mean must be within the budget; where it is a
   software rasteriser (SwiftShader, llvmpipe: what CI and an agent pod have) a low-tier frame takes seconds, so the run is
   reported and the budget not asserted, with the reason. The viewer then asking for reduced motion (emulateMedia), the
   wind stops at once and no animation frame is drawn for 5 s. Hidden (visibilitychange) the page stops drawing and visible
   again draws on; on pagehide its WebGL context is lost and the canvas marked gone.

   The laptop: 1280x800 is the budget's laptop, but no GL draws the high tier there in a CI runner's time, so on hardware GL
   it runs at a reduced 320x200 under ?q=high: held at high (two cascades at 2048 and 1024 px, the pond a mirror), its 10 s
   run logged the same way and held to the 16 ms budget. On software GL it is skipped, with the reason (SwiftShader takes
   over 10 s a frame of the high tier at that size, so a run would time nothing). Zero console errors. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));
var NOON = '2026-10-26T12:00:00Z', RUN = 10000;
var LINE = /^splash: frame ([0-9.]+) ms \(p95 ([0-9.]+)\), draw ([0-9.]+) ms \(p95 ([0-9.]+)\) over (\d+) frames, (\d+) blades, (low|mid|high) tier, budget (\d+) ms$/;

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  var errors = [], report = [];
  async function open(ctx, path) {
    var page = await ctx.newPage(), lines = [];
    page.on('console', function (m) { if (m.type() === 'error') errors.push(m.text()); if (m.type() === 'info' && /^splash: /.test(m.text())) lines.push(m.text()); });
    page.on('pageerror', function (e) { errors.push(String(e.message || e)); });
    await page.goto(url + path);
    // SwiftShader compiles the scene's shaders slowly
    await page.waitForFunction(function () { return document.body.dataset.gl === 'off' || document.body.dataset.gl === 'on' && +document.getElementById('splash').dataset.frames >= 1; }, null, { timeout: 240000 });
    assert.strictEqual(await page.evaluate(function () { return document.body.dataset.gl + ' ' + (document.body.dataset.why || ''); }), 'on ', 'the splash draws in WebGL');
    return { page: page, lines: lines };
  }
  // software says whether the page's GL is a software rasteriser, and which
  function software(page) {
    return page.evaluate(function () {
      var gl = window.SPLASH.view.renderer.getContext(), ext = gl.getExtension('WEBGL_debug_renderer_info');
      var r = String(ext ? gl.getParameter(ext.UNMASKED_RENDERER_WEBGL) : gl.getParameter(gl.RENDERER));
      return /swiftshader|llvmpipe|software|softpipe/i.test(r) ? r : '';
    });
  }
  // run opens a RUN ms run with the probe's log(), closes it the same way, and returns the line the second logged
  async function run(o) {
    await o.page.evaluate(function () { window.__splash.log(); });
    var n = o.lines.length;
    await o.page.waitForTimeout(RUN);
    var s = await o.page.evaluate(function () { return window.__splash.log(); });
    var line = o.lines.slice(n).filter(function (l) { return LINE.test(l); }).pop();
    assert.ok(line, 'the run\'s frame times are logged to the console: ' + o.lines.slice(n).join(' | '));
    var m = LINE.exec(line);
    assert.ok(+m[1] === s.frame && +m[5] === s.n, 'the logged line is the run\'s summary: ' + line + ' ' + JSON.stringify(s));
    return { line: line, frame: +m[1], p95: +m[2], n: +m[5], blades: +m[6], tier: m[7], budget: +m[8] };
  }
  // budget asserts a run within its budget on hardware GL, and reports it either way
  function budget(name, r, soft) {
    var what = name + ': ' + r.tier + ' tier, frame ' + r.frame + ' ms (p95 ' + r.p95 + ') over ' + r.n + ' frames in ' + RUN / 1000 + ' s, budget ' + r.budget + ' ms';
    if (soft) { report.push(what + '; budget NOT asserted: software GL (' + soft + ') draws a frame in seconds'); return; }
    assert.ok(r.n >= RUN / 1000 && r.frame <= r.budget, what + ': over budget');
    report.push(what + ': within budget');
  }
  try {
    var ctx = await browser.newContext(Object.assign({}, pw.devices['iPhone 12'], { viewport: { width: 390, height: 844 }, timezoneId: 'UTC' }));
    var ph = await open(ctx, '/garden/splash?t=' + NOON + '&wind=1');
    var q = await ph.page.evaluate(function () { return window.__splash.quality(); });
    assert.ok(q.tier === 'low' && q.blades === 8000 && q.cascades === 1 && q.maps.join() === '0,1024' && q.pond === 'water' && q.impostors && q.ratio === 1, 'a phone starts at the low tier: ' + JSON.stringify(q));
    var soft = await software(ph.page), r = await run(ph);
    assert.ok(r.tier === 'low' && r.budget === 33 && r.blades === 8000, 'the phone\'s run is logged at low against 33 ms: ' + r.line);
    budget('phone 390x844', r, soft);
    // reduced motion asked for now: the wind stops, and no animation frame is drawn
    await ph.page.emulateMedia({ reducedMotion: 'reduce' });
    var c0 = await ph.page.evaluate(function () { return window.__splash.quality().frames; });
    await ph.page.waitForTimeout(5000);
    var c1 = await ph.page.evaluate(function () { return window.__splash.quality().frames; });
    // the page reads the change at its next style update, after that frame's tick has asked for the one after it, so the
    // frame under way and the one it asked for may both land: 11 to 13 on a runner at 4-5 s a SwiftShader frame (gd-owQJjry58h)
    assert.ok(c1 - c0 <= 2, 'under reduced motion the animation frames stop (a frame under way, and the one it asked for, may finish): ' + c0 + ' to ' + c1);
    // hidden it stops drawing; visible again it draws on
    var hid = await ph.page.evaluate(function () {
      Object.defineProperty(document, 'visibilityState', { configurable: true, get: function () { return 'hidden'; } });
      document.dispatchEvent(new Event('visibilitychange'));
      return window.__splash.quality();
    });
    var f0 = hid.frames;
    await ph.page.waitForTimeout(3000);
    var still = await ph.page.evaluate(function () { return window.__splash.quality(); });
    assert.ok(hid.paused && still.paused && still.frames - f0 <= 1, 'hidden, the page stops drawing (a frame under way may finish): ' + f0 + ' to ' + still.frames);
    var back = await ph.page.evaluate(function () {
      Object.defineProperty(document, 'visibilityState', { configurable: true, get: function () { return 'visible'; } });
      document.dispatchEvent(new Event('visibilitychange'));
      return window.__splash.quality();
    });
    assert.ok(!back.paused, 'visible again it draws on');
    // pagehide: the scene and the context let go
    var gone = await ph.page.evaluate(function () {
      var gl = window.SPLASH.view.renderer.getContext();
      window.dispatchEvent(new PageTransitionEvent('pagehide', { persisted: false }));
      return { lost: gl.isContextLost(), gone: document.getElementById('splash').dataset.gone };
    });
    assert.ok(gone.lost && gone.gone === '1', 'on pagehide the WebGL context is lost and the canvas gone: ' + JSON.stringify(gone));
    await ctx.close();

    if (soft) report.push('laptop high tier at a reduced 320x200: SKIPPED, software GL (' + soft + ') takes over 10 s a frame of it');
    else {
      ctx = await browser.newContext({ viewport: { width: 320, height: 200 }, timezoneId: 'UTC' });
      var lap = await open(ctx, '/garden/splash?t=' + NOON + '&wind=1&q=high');
      q = await lap.page.evaluate(function () { return window.__splash.quality(); });
      assert.ok(q.tier === 'high' && q.judging === null && q.blades === 60000 && q.cascades === 2 && q.maps.join() === '2048,1024' && q.pond === 'mirror', '?q=high holds the high tier: ' + JSON.stringify(q));
      r = await run(lap);
      assert.ok(r.tier === 'high', 'logged at high: ' + r.line);
      // a 320x200 screen is a phone's by its size, so the page judges it by 33 ms; the laptop's budget is 16
      budget('laptop at a reduced 320x200', Object.assign({}, r, { budget: 16 }), '');
      await ctx.close();
    }

    assert.deepStrictEqual(errors, [], 'no console errors (a shader that fails to compile logs one)');
    console.log(report.join('\n'));
  } finally {
    await browser.close();
  }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
