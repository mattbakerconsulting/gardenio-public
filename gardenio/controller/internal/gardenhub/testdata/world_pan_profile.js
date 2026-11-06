/* The World's render profile on the 10x farm (Lands performance pass, step P2.1 gd-PoCdbE1SJX): /garden in Chromium on the
   phone profile, its /garden/farm document swapped for testdata/world_tenx.js's (50 projects, 360 plans, 60 gardeners), then
   idle, a 10 s pan and a zoom, each read for long tasks, frames and where the main thread's time went.
   Usage:
     node world_pan_profile.js <base url> <node_modules dir> <out dir>
   Run from TestWorldPanProfile (`make world-pan-profile OUT=<dir>`), which serves the goldens' fixture farm. Writes
   <out dir>/report.json, prints a line per phase and the report's gist on its last line.

   Profile: Pixel 5 (390x844, touch, mobile UA, 2.75x), CPU throttled 4x, 9 Mbps / 170 ms (perf_baseline.js's phone).
   Phases, after the map is laid out and 3 s quiet:
     idle   WORLD_PAN_IDLE ms (5000) of nothing: no frame should be asked for and no long task run
     pan    10 s of one finger dragging the world about: a swipe every PAN_STEP ms, so the camera is always moving; the
            gate is here
     zoom   descends to a phase and out again by the camera column's In and Out (a CDP touch at each one's centre; the camera
            eases 240 ms each), then a pinch and a spread
   Per phase: long tasks (n, total, max, each [start ms into the phase, ms]), frames (requestAnimationFrame callbacks run)
   and errors; with a CPU profile, the top self time by function.
   The gate (WORLD_PAN_GATE_MS, default 100): no long task longer than it during the pan. The script exits 1 when it is
   missed, so TestWorldPanProfile fails.
   Environment, to dig in (the profiler and the trace cost the page time, so a gate run leaves both off):
     WORLD_PAN_CPU         CPU throttle (4); 1 reads the machine's own speed
     WORLD_PAN_DPR         device pixel ratio (the Pixel 5's 2.75)
     WORLD_PAN_CPUPROFILE  1: a CPU profile per phase, <phase>.cpuprofile (WORLD_PAN_SAMPLE_US, default 1000 us)
     WORLD_PAN_TRACE       1: a DevTools trace per phase, <phase>.trace.json (open it in the Performance panel)
     WORLD_PAN_ARGS        extra Chromium flags, space separated (e.g. --disable-gpu)
   Read the numbers against the machine (report.calibration, a bare full-screen canvas at the same throttle, is its floor): a shared agent pod (1.5 CPUs on a busy node) is several times slower than a phone
   at the same throttle, and a 2D canvas without a GPU is rasterized on the main thread at Commit (gardenio/docs/
   lands-perf-world-render.md).
   Run with `require`, it measures nothing until profile() is called: perf_baseline.js's pan pass (B7 in the CI gate,
   gd-K3KmHhfnhd) runs the same calibration, pan and zoom through it, one call per run. */
'use strict';
var fs = require('fs'), path = require('path'), assert = require('assert');
var gen = require(path.join(__dirname, 'world_tenx.js'));

var env = process.env;
var IDLE = +(env.WORLD_PAN_IDLE || 5000), PAN = 10000, PAN_STEP = 300, GATE = +(env.WORLD_PAN_GATE_MS || 100);
var CPU = +(env.WORLD_PAN_CPU || 4), PROFILE = env.WORLD_PAN_CPUPROFILE === '1', TRACE = env.WORLD_PAN_TRACE === '1';

/* The 10x farm the frame budget walks (world_frame_test.js): closed rows with harvests, 60 gardeners on plots. Built on
   first use, so a script that requires this one pays for it only when it pans. */
var DOC;
function doc() { return DOC || (DOC = JSON.stringify(gen.withGardeners(gen.withClosed(gen.tenx()), 60))); }
/* The stream: a hello and a heartbeat, then a long retry, so the page holds the 10x document and nothing else. */
var SSE = 'retry: 600000\nevent: hb\ndata: {}\n\n';

/* In the page before its own scripts: long tasks, and a count of animation-frame callbacks run, so an idle page that keeps
   asking for frames shows. */
function observe() {
  var p = window.__pan = { long: [], raf: 0 };
  try {
    new PerformanceObserver(function (l) { l.getEntries().forEach(function (e) { p.long.push([e.startTime, e.duration]); }); }).observe({ type: 'longtask', buffered: true });
  } catch (e) { p.noLong = String(e); }
  var raf = window.requestAnimationFrame.bind(window);
  window.requestAnimationFrame = function (f) { return raf(function (t) { p.raf++; f(t); }); };
}

function read(page) { return page.evaluate(function () { return { now: performance.now(), long: window.__pan.long.slice(), raf: window.__pan.raf }; }); }

/* CPU profile self time by function (file and line as served: a script's bundle, assets.go), heaviest first. */
function topSelf(prof, n, base) {
  var self = {}, byId = {}, dt = prof.timeDeltas || [], total = 0;
  prof.nodes.forEach(function (nd) { byId[nd.id] = nd; });
  (prof.samples || []).forEach(function (id, i) {
    var nd = byId[id], cf = nd.callFrame, d = (dt[i] || 0) / 1000;
    total += d;
    var k = cf.functionName ? cf.functionName + ' ' + (cf.url ? cf.url.replace(base, '').replace(/^.*\//, '') : '') + ':' + (cf.lineNumber + 1) : '(' + (cf.functionName || nd.callFrame.url || 'program') + ')';
    if (!cf.functionName && !cf.url) k = '(' + (cf.functionName || 'native') + ')';
    self[k] = (self[k] || 0) + d;
  });
  return { total: Math.round(total), top: Object.keys(self).map(function (k) { return [k, Math.round(self[k] * 10) / 10]; }).sort(function (a, b) { return b[1] - a[1]; }).slice(0, n) };
}

/* One phase on st's page (st: page, cdp, errors, out, base); a CPU profile and a trace only with an out dir to write them to. */
async function phase(st, name, fn) {
  var page = st.page, cdp = st.cdp, prof0 = PROFILE && st.out, trace = TRACE && st.out;
  var r0 = await read(page), errs0 = st.errors.length;
  if (prof0) await cdp.send('Profiler.start');
  if (trace) await page.context().browser().startTracing(page, { path: path.join(st.out, name + '.trace.json'), categories: ['devtools.timeline', 'disabled-by-default-devtools.timeline', 'v8.execute', 'blink.user_timing'] });
  await fn();
  if (trace) await page.context().browser().stopTracing();
  var prof = prof0 ? (await cdp.send('Profiler.stop')).profile : { nodes: [] };
  if (prof0) fs.writeFileSync(path.join(st.out, name + '.cpuprofile'), JSON.stringify(prof));
  var r1 = await read(page), lt = r1.long.slice(r0.long.length);
  var rep = {
    ms: Math.round(r1.now - r0.now), raf: r1.raf - r0.raf,
    longTasks: { n: lt.length, total: Math.round(lt.reduce(function (a, x) { return a + x[1]; }, 0)), max: Math.round(lt.reduce(function (a, x) { return Math.max(a, x[1]); }, 0)), list: lt.map(function (x) { return [Math.round(x[0] - r0.now), Math.round(x[1])]; }) },
    cpu: topSelf(prof, 15, st.base), errors: st.errors.slice(errs0),
  };
  console.log(name + ': ' + rep.ms + ' ms, ' + rep.raf + ' frames, long tasks ' + rep.longTasks.n + ' / ' + rep.longTasks.total + ' ms / max ' + rep.longTasks.max + ' ms; script self ' + rep.cpu.total + ' ms');
  rep.cpu.top.slice(0, 8).forEach(function (t) { console.log('    ' + t[1] + ' ms  ' + t[0]); });
  return rep;
}

/* A one-finger swipe or a two-finger pinch through CDP touch events, at about 60 Hz over ms. */
async function touch(cdp, tracks, ms) {
  var steps = Math.max(2, Math.round(ms / 16));
  function at(k) { return tracks.map(function (t, i) { return { x: t[0] + (t[2] - t[0]) * k, y: t[1] + (t[3] - t[1]) * k, id: i }; }); }
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: at(0) });
  for (var s = 1; s <= steps; s++) {
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: at(s / steps) });
    await new Promise(function (ok) { setTimeout(ok, ms / steps); });
  }
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
}

/* The machine's floor: a page that only repaints a full-screen canvas (50 small fills a frame) for 2 s at the same profile and
   throttle. Its long tasks are the raster and commit cost of any full-screen canvas here, before the World draws anything:
   frames, ms, and long tasks (count, total ms, longest). */
async function calibrate(ctx, cpu) {
  var page = await ctx.newPage(), cdp = await ctx.newCDPSession(page);
  if (cpu > 1) await cdp.send('Emulation.setCPUThrottlingRate', { rate: cpu });
  await page.setContent('<canvas id=c style="position:fixed;inset:0;width:100%;height:100%"></canvas>');
  var r = await page.evaluate(function () {
    return new Promise(function (ok) {
      var c = document.getElementById('c'), d = Math.min(devicePixelRatio || 1, 2), x, t0 = performance.now(), n = 0, lt = [];
      c.width = Math.round(innerWidth * d); c.height = Math.round(innerHeight * d); x = c.getContext('2d');
      new PerformanceObserver(function (l) { l.getEntries().forEach(function (e) { lt.push(Math.round(e.duration)); }); }).observe({ type: 'longtask' });
      (function f() {
        n++;
        for (var i = 0; i < 50; i++) { x.fillStyle = i % 2 ? '#3a5' : '#5a3'; x.fillRect((i * 37 + n * 5) % c.width, (i * 91) % c.height, 24, 24); }
        if (performance.now() - t0 < 2000) requestAnimationFrame(f);
        else setTimeout(function () { ok({ frames: n, ms: Math.round(performance.now() - t0), long: lt.length, total: lt.reduce(function (a, d) { return a + d; }, 0), max: Math.max.apply(null, lt.concat(0)) }); }, 100);
      })();
    });
  });
  await page.close();
  console.log('calibration: a bare full-screen canvas, ' + r.frames + ' frames in 2 s, long tasks ' + r.long + ' / ' + r.total + ' ms, max ' + r.max + ' ms');
  return r;
}

/* The calibration, then idle (o.idle ms, default WORLD_PAN_IDLE; 0 skips it), the 10 s pan and the zoom on a fresh context of
   browser at o.base, its /garden/farm answered with the 10x document. o.pw: playwright-core (its Pixel 5); o.cpu: the
   throttle (default WORLD_PAN_CPU); o.cookies: set on the context first (a deployed /garden's session); o.out: where the
   CPU profiles and traces go (none without it). Returns the report: calibration, ready_ms, boot, idle (null when skipped),
   pan, zoom, wall_ms. */
async function profile(browser, o) {
  var cpu = o.cpu === undefined ? CPU : o.cpu, idle = o.idle === undefined ? IDLE : o.idle, pw = o.pw;
  var report = {}, st = { out: o.out || '', base: o.base, errors: [] };
  var ctx = await browser.newContext(Object.assign({}, pw.devices['Pixel 5'], { viewport: { width: 390, height: 844 }, deviceScaleFactor: +(env.WORLD_PAN_DPR || pw.devices['Pixel 5'].deviceScaleFactor), locale: 'en-US', timezoneId: 'UTC' }));
  try {
    if (o.cookies && o.cookies.length) await ctx.addCookies(o.cookies);
    report.calibration = await calibrate(ctx, cpu);
    await ctx.addInitScript(observe);
    await ctx.route(/\/garden\/farm(\?.*)?$/, function (r) { r.fulfill({ status: 200, contentType: 'application/json', body: doc() }); });
    await ctx.route(/\/garden\/farm\/events(\?.*)?$/, function (r) { r.fulfill({ status: 200, contentType: 'text/event-stream', body: SSE }); });
    var page = st.page = await ctx.newPage();
    page.on('pageerror', function (e) { st.errors.push(String(e)); });
    var cdp = st.cdp = await ctx.newCDPSession(page);
    await cdp.send('Profiler.enable');
    await cdp.send('Profiler.setSamplingInterval', { interval: +(env.WORLD_PAN_SAMPLE_US || 1000) });
    await cdp.send('Network.enable');
    await cdp.send('Network.emulateNetworkConditions', { offline: false, latency: 170, downloadThroughput: 9e6 / 8, uploadThroughput: 1.5e6 / 8 });
    if (cpu > 1) await cdp.send('Emulation.setCPUThrottlingRate', { rate: cpu });
    var t0 = Date.now();
    await page.goto(o.base + '/garden', { waitUntil: 'commit', timeout: 60000 });
    // the 10x layout is in when the world's biome signs are: more than the fixture farm's handful
    await page.waitForFunction(function () { return document.querySelector('#hots .hot.cur') && document.querySelectorAll('#hots .hot').length > 20; }, null, { timeout: 120000 });
    report.ready_ms = Date.now() - t0;
    var boot = await read(page);
    report.boot = { long: { n: boot.long.length, max: Math.round(boot.long.reduce(function (a, x) { return Math.max(a, x[1]); }, 0)) } };
    await page.waitForTimeout(3000);   // let the first chunk bakes and the signs settle
    // In and Out are tapped by CDP touch at their centres, measured once: page.tap() would run Playwright's selector
    // engine over the page's ~7k nodes in the page's own main thread on every step, and that time would read as the World's
    var at = await page.evaluate(function () {
      var o = {};
      ['in', 'out'].forEach(function (k) { var r = document.getElementById('cam-' + k).getBoundingClientRect(); o[k] = [r.left + r.width / 2, r.top + r.height / 2]; });
      return o;
    });
    var cam = async function (k) {
      await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x: at[k][0], y: at[k][1] }] });
      await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
    };

    report.idle = idle > 0 ? await phase(st, 'idle', function () { return page.waitForTimeout(idle); }) : null;

    // the pan: drag the world left along its top rows, up, back right, up again, PAN_STEP apart, for PAN ms
    report.pan = await phase(st, 'pan', async function () {
      var SWIPE = { left: [300, 500, 120, 500], right: [100, 500, 280, 500], up: [200, 560, 200, 380] };
      var seq = ['left', 'left', 'left', 'up', 'right', 'right', 'right', 'up'], i = 0, end = Date.now() + PAN;
      while (Date.now() < end) {
        var k = seq[i++ % seq.length], t = Date.now();
        await touch(cdp, [SWIPE[k]], 120);
        var left = PAN_STEP - (Date.now() - t);
        if (left > 0) await page.waitForTimeout(left);
      }
      await page.waitForTimeout(400);
    });

    // the zoom: in to the plan and phase and out again by In and Out, then a pinch out and a spread released half-way
    report.zoom = await phase(st, 'zoom', async function () {
      for (var k of ['in', 'in', 'in', 'out', 'out', 'out', 'in', 'in', 'out', 'out']) { await cam(k); await page.waitForTimeout(500); }
      await touch(cdp, [[150, 450, 190, 420], [250, 450, 210, 480]], 300);   // a pinch: out (bumps at the world)
      await page.waitForTimeout(500);
      await touch(cdp, [[180, 430, 120, 380], [220, 470, 280, 520]], 300);   // a spread: in
      await page.waitForTimeout(800);
    });
    report.wall_ms = Date.now() - t0;
  } finally {
    await ctx.close();
  }
  return report;
}

module.exports = { profile: profile, GATE: GATE };

if (require.main === module) (async function main() {
  var base = process.argv[2], mods = process.argv[3], out = process.argv[4];
  assert.ok(base && mods && out, 'usage: node world_pan_profile.js <base url> <node_modules> <out dir>');
  var pw = require(require.resolve('playwright-core', { paths: [mods] }));
  fs.mkdirSync(out, { recursive: true });
  var browser = await pw.chromium.launch({ args: (env.WORLD_PAN_ARGS || '').split(' ').filter(Boolean) });
  var report = { chromium: browser.version(), at: new Date().toISOString(), cpu: CPU, gate: GATE, doc_bytes: doc().length };
  try {
    Object.assign(report, await profile(browser, { base: base, pw: pw, out: out }));
  } finally {
    await browser.close();
  }
  var worst = report.pan.longTasks.max;
  report.pass = worst <= GATE;
  fs.writeFileSync(path.join(out, 'report.json'), JSON.stringify(report, null, 1));
  console.log('gate: longest task in the 10 s pan ' + worst + ' ms against ' + GATE + ' ms: ' + (report.pass ? 'pass' : 'MISSED'));
  console.log(JSON.stringify({ pass: report.pass, pan_max: worst, pan_long: report.pan.longTasks.n, idle_raf: report.idle && report.idle.raf, idle_long: report.idle && report.idle.longTasks.n, zoom_max: report.zoom.longTasks.max, ready_ms: report.ready_ms, calibration_max: report.calibration.max }));
  process.exit(report.pass ? 0 : 1);
})().catch(function (e) { console.error(e); process.exit(2); });
