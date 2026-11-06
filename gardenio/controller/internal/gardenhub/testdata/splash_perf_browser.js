/* The splash's ?perf=1 frame readout in a real browser (gd-8nCJKznQzj), run from TestSplashPerfUnderChromium against
   /garden/splash on an httptest server. Usage: node splash_perf_browser.js <base url> <node_modules dir> [shots dir].

   On the golden harness's phone (an iPhone 12, 390x844, touch), in light and then the same page switched to dark (the scene
   does not follow the scheme, so one page, and one software-GL first frame, serves both), pinned to noon with the wind
   blowing and ?perf=1: once it draws, #splash-perf is an <output> with aria-live off and a label, fixed in the top left
   corner inside the 390 px screen, its text 11 px or more and selectable, above the time controls without touching them
   (Play is still what a tap on it finds). Its first line is the console's frame line ("splash: frame <mean> ms (p95 <p95>)
   ... low tier, budget 33 ms"), its second the time running and the step-downs ("running 4 s, the last 10 s; no
   step-downs"); with the wind stopped, the time running moves on within a bounded wait, as the readout is written again
   every 2 s. The same page without ?perf=1 has no readout and keeps no frames for one, as soon as it boots, with no frame
   waited for. Zero console errors. With a third argument (or SPLASH_PERF_SHOTS) it saves a PNG of each scheme there, for a
   reviewer's eye. */
'use strict';
var url = process.argv[2], mods = process.argv[3], shots = process.argv[4] || process.env.SPLASH_PERF_SHOTS, assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));
var NOON = '2026-10-26T12:00:00Z';
var LINE = /^splash: frame [0-9.]+ ms \(p95 [0-9.]+\), draw [0-9.]+ ms \(p95 [0-9.]+\) over \d+ frames, \d+ blades, (low|mid|high) tier, budget (\d+) ms$/;
var RUNNING = /^running (?:(\d+) m )?(\d+) s, the last 10 s; (no step-downs|stepped [a-z]+ -> [a-z]+ at \d+ s(, [a-z]+ -> [a-z]+ at \d+ s)*)$/;

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  var errors = [], report = [];
  // open loads path and waits for the splash to boot into WebGL and, when frame says, to draw its first frame. It polls on a
  // timer, not on animation frames, which a software GL hands out seconds apart
  async function open(ctx, path, frame) {
    var page = await ctx.newPage();
    page.on('console', function (m) { if (m.type() === 'error') errors.push(m.text()); });
    page.on('pageerror', function (e) { errors.push(String(e.message || e)); });
    await page.goto(url + path);
    // SwiftShader compiles the scene's shaders slowly
    await page.waitForFunction(function (frame) { var gl = document.body.dataset.gl; return gl === 'off' || gl === 'on' && (!frame || +document.getElementById('splash').dataset.frames >= 1); }, frame, { timeout: 240000, polling: 100 });
    assert.strictEqual(await page.evaluate(function () { return document.body.dataset.gl + ' ' + (document.body.dataset.why || ''); }), 'on ', 'the splash draws in WebGL');
    return page;
  }
  // readout reads #splash-perf: its words, its place, its style, whether it can be selected, and what a tap on Play finds;
  // with calm, it then stops the wind in the same task, as each evaluate waits out a software GL's frame of seconds
  function readout(page, calm) {
    return page.evaluate(function (calm) {
      var el = document.getElementById('splash-perf');
      if (!el) return null;
      var r = el.getBoundingClientRect(), row = document.querySelector('#splash-time .row').getBoundingClientRect(), cs = getComputedStyle(el);
      var play = document.getElementById('splash-play').getBoundingClientRect(), hit = document.elementFromPoint(play.left + play.width / 2, play.top + play.height / 2);
      var sel = window.getSelection();
      sel.selectAllChildren(el);
      var picked = sel.toString();
      sel.removeAllRanges();
      if (calm) window.SPLASH.view.animate(false, false);
      return { tag: el.tagName, live: el.getAttribute('aria-live'), label: el.getAttribute('aria-label'), text: el.textContent, picked: picked,
        rect: [r.left, r.top, r.right, r.bottom], row: row.top, width: innerWidth, position: cs.position, size: parseFloat(cs.fontSize), select: cs.userSelect,
        color: cs.color, background: cs.backgroundColor, play: hit && hit.id };
    }, !!calm);
  }
  // check holds a readout to its place, its words and its style under scheme, and gives its two lines
  function check(a, scheme) {
    assert.ok(a, scheme + ': ?perf=1 shows the readout');
    var lines = a.text.split('\n');
    assert.ok(lines.length === 2 && LINE.test(lines[0]) && RUNNING.test(lines[1]), scheme + ': the console\'s frame line, then the time running and the step-downs: ' + JSON.stringify(a.text));
    var m = LINE.exec(lines[0]);
    assert.ok(m[1] === 'low' && m[2] === '33', scheme + ': a phone\'s tier and its 33 ms budget: ' + lines[0]);
    assert.ok(a.tag === 'OUTPUT' && a.live === 'off' && a.label, scheme + ': an output, labelled, not announced: ' + JSON.stringify([a.tag, a.live, a.label]));
    assert.ok(a.position === 'fixed' && a.rect[0] >= 16 && a.rect[1] >= 0 && a.rect[2] <= a.width - 16 && a.rect[3] < a.row, scheme + ': fixed in the top corner, inside the screen, above the time controls: ' + JSON.stringify([a.rect, a.row, a.width]));
    assert.ok(a.size >= 11 && a.select === 'text' && a.picked.replace(/\s+/g, ' ').trim() === a.text.replace(/\s+/g, ' ').trim(), scheme + ': legible and selectable: ' + JSON.stringify([a.size, a.select, a.picked]));
    assert.strictEqual(a.play, 'splash-play', scheme + ': a tap on Play still finds Play');
    return lines;
  }
  // secs is the time running a readout's second line says, in seconds
  function secs(l) { var m = RUNNING.exec(l); assert.ok(m, 'the time running: ' + l); return (+m[1] || 0) * 60 + +m[2]; }
  try {
    var phone = Object.assign({}, pw.devices['iPhone 12'], { viewport: { width: 390, height: 844 }, timezoneId: 'UTC' });
    var ctx = await browser.newContext(Object.assign({}, phone, { colorScheme: 'light' }));
    var page = await open(ctx, '/garden/splash?t=' + NOON + '&wind=1&perf=1', true);
    // read with the wind blowing, then stopped (under ?t= nothing else draws), as a software GL's frames of seconds hold the
    // main thread and the 2 s timer with it
    var a = await readout(page, true), lines = check(a, 'light');
    // written again every 2 s: a few seconds on, the time running has moved, polled up to a bounded deadline rather than read
    // once after a fixed wait
    var r0 = secs(lines[1]), r1 = r0, b;
    for (var until = Date.now() + 90000; ; await page.waitForTimeout(500)) {
      b = await readout(page);
      r1 = secs(b.text.split('\n')[1]);
      if (r1 >= r0 + 3 || Date.now() > until) break;
    }
    assert.ok(r1 >= r0 + 3, 'light: the readout is written again as the page runs: ' + r0 + ' s to ' + r1 + ' s');
    assert.ok(b.text.split('\n').length === 2 && LINE.test(b.text.split('\n')[0]), 'light: written again, still the frame line: ' + JSON.stringify(b.text));
    report.push('light: ' + b.text.replace('\n', ' | '));
    // the shots, still with the wind stopped, so a software GL's frames of seconds do not starve them
    if (shots) await page.screenshot({ path: require('path').join(shots, 'splash-perf-light.png'), timeout: 120000 });
    await page.emulateMedia({ colorScheme: 'dark' });
    assert.ok(await page.evaluate(function () { return matchMedia('(prefers-color-scheme: dark)').matches; }), 'dark: the page is in the dark scheme');
    var d = await readout(page);
    check(d, 'dark');
    report.push('dark: ' + d.text.replace('\n', ' | '));
    if (shots) await page.screenshot({ path: require('path').join(shots, 'splash-perf-dark.png'), timeout: 120000 });
    assert.deepStrictEqual([d.color, d.background], [a.color, a.background], 'the readout keeps its colours in the dark, over a scene that does not follow the scheme');
    await ctx.close();

    ctx = await browser.newContext(phone);
    page = await open(ctx, '/garden/splash?t=' + NOON + '&wind=1', false);
    var none = await page.evaluate(function () { return { el: !!document.getElementById('splash-perf'), roll: window.SPLASH.view.perf.roll }; });
    assert.deepStrictEqual(none, { el: false, roll: null }, 'without ?perf=1 there is no readout and no frames kept for one');
    report.push('without ?perf=1: no readout');
    await ctx.close();

    assert.deepStrictEqual(errors, [], 'no console errors');
    console.log(report.join('\n'));
  } finally {
    await browser.close();
  }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
