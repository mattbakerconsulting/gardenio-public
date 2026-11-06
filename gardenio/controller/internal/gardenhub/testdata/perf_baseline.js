/* The Lands performance baseline (plan gd-MEGB3dEaHu, step P0.1 gd-j8jXq1TC1m): loads /garden and each Lands surface cold
   and warm in Chromium on a throttled phone and on a desktop, and measures what the plan's budgets are written against.
   Usage:
     node perf_baseline.js <base url> <node_modules dir> <out dir>
   Run from TestLandsPerfBaseline against the goldens' fixture farm (`make lands-perf-baseline OUT=<dir>`), or by hand
   against a deployed /garden with a session cookie (LANDS_PERF_COOKIE, below). Writes <out dir>/results.json (every run)
   and <out dir>/summary.md (medians, one table per profile) and prints the summary.

   Profiles:
     phone    390x844, touch, mobile UA, CPU throttled 4x, network 9 Mbps down / 1.5 Mbps up / 170 ms RTT (DevTools-style
              per-request latency, not a real TCP model)
     desktop  1280x800, no throttling
   Per surface and profile, LANDS_PERF_RUNS (default 3) runs of:
     cold  a fresh browser context (empty HTTP cache, no storage) loads the surface's URL
     warm  the same context loads it again (HTTP cache, localStorage and cookies from the cold load)
   and once per profile an idle pass: every surface open at once, LANDS_PERF_IDLE seconds (default 300) left alone, then the
   JS heap read, before and after a forced GC, and the bytes the page fetched while idle (the pages share the CPU then, so
   long tasks are read in the settled window of each cold load instead). One
   unmeasured load opens each profile, so the first surface does not pay the server's first snapshot build.
   On the phone, when the World is measured, a pan pass (B7, gd-K3KmHhfnhd): LANDS_PERF_PAN runs (default LANDS_PERF_RUNS)
   of world_pan_profile.js's profile(), its measurer reused as is: a bare full-screen canvas at the same throttle (the
   calibration: the machine's floor), then the World on the 10x farm, a 10 s pan and a zoom. One row per run, kind pan:
     longTasks    in the pan: count, total and longest (ms); B7 is the longest
     panMs        the pan's length; frames: animation frames run in it
     calibration  the bare canvas: frames, ms, long (count), total and max (ms); the gate grades B7 against it
     zoom         the zoom's ms, frames and long tasks, for reading (B7 is graded on the pan, as P2.1's gate is)

   Metrics, per load:
     bytes      raw: response bodies decoded; wire: what crossed the network (headers + body, as served: gzip where the
                server gzips); gz: the bodies if every one were gzipped (what edge gzip, P1.1, would get to). Long-lived
                streams (text/event-stream) are counted apart, as streams, not in the page weight.
     requests   every request up to TTI but streams; streams: EventSource and other never-ending responses
     ttfb       the document's response headers, from CDP (navigation timing's responseStart misses the emulated latency)
     fcp, lcp   paint timing / largest-contentful-paint, read before the first interaction
     ready      the surface's own element on screen (the row's ready() resolved), from navigation start
     tti        Lighthouse-style: from FCP, the end of the last long task before the first 5 s window with no long task
                and at most two requests in flight (resource timing); cut at 30 s (null: never quiet). Streams still open
                have no resource timing entry, so they never hold it back.
     inp        the first interaction's (the row's act()) longest Event Timing duration; Chromium reports only entries
                over 16 ms, so 0 means "under 16 ms"
     longTasks  count, total and longest (ms) from navigation start to TTI
     settled    cold loads only: the long tasks in the 10 s after the first interaction, nothing else touched: what the
                page costs the main thread while someone just looks at it
     bench      throttled cold loads only: bench() just before the load and just after it, the node's speed while it
                loaded (a CI pod's swings within one run, 2-3x in minutes, never show in the run's opening bench)
     latency    cold loads of a row with a latency spec (a workbench surface's, shoot.js), when LANDS_PERF_LATENCY is on:
                the Commons v2 interaction budgets (rubric section 7, B12-B15; plan gd-lRaCxsiLvC step 1 gd-J3LZYVcuBK),
                each driven for real on the surface once its first interaction is read (latencyPass, below):
                  keyPaint  KEYS typed into the composer, one key every KEY_GAP ms: from each keydown to just after the
                            frame that paints its input event (median, max, each); rows: the timeline rows (.cm-messages
                            > li) any of those keys mutated, which must be none
                  echo      a message sent from that composer (Enter on a desktop, Send tapped on a phone): from the input
                            (a tap's release, latencyKit) to just after the frame that first shows it in the timeline
                  open      a thread, a plan and an epic opened from the surface, then closed and opened again: from the
                            click or tap to just after the frame that shows the node's content; cold (first open) and warm
                            (the reopen, which may come from a cache), per root; a root the surface is already on has
                            no entry (commonsthread opens the plan and the epic, commonsplan the thread and the epic)
                  stream    a message said into the surface's channel (or thread) by the farm, on its stream: from the
                            moment the server puts it on the stream to just after the frame that shows it (same host, so
                            one wall clock); null where the farm has no test route for it or no live stream
                A step that did not show within LATENCY_WAIT ms is null, and its error is in latency.errors. "Just after
                the frame" is a message posted from the frame's requestAnimationFrame callback, which runs once that frame
                is painted. The fixture's routes (POST /test/perf/say, /test/perf/forget: perfWorld in
                perf_baseline_test.go) say the stream's message and forget every message the pass wrote.
   Environment:
     LANDS_PERF_SURFACES  comma list of surface names (default: all but the opt-in ones, which are run only when named)
     LANDS_PERF_PROFILES  comma list, phone,desktop (default both)
     LANDS_PERF_RUNS      runs per surface, profile and cold/warm (default 3)
     LANDS_PERF_IDLE      idle seconds before the heap read (default 300; 0 skips the idle pass)
     LANDS_PERF_PAN       runs of the pan pass (default LANDS_PERF_RUNS; 0 skips it)
     LANDS_PERF_SETTLE    seconds of the settled window after a cold load's first interaction (default 10; 0 skips it:
                          no budget reads it, so the CI gate, P3.1, saves the time)
     LANDS_PERF_TRACE     a directory: after its measured runs, each surface is loaded once more, cold, under a Playwright
                          trace (screenshots and DOM snapshots), written to <dir>/<profile>-<surface>.zip. That load is not
                          measured, so the trace's own cost never reaches results.json. Open one at trace.playwright.dev.
     LANDS_PERF_TRACE_PROFILES  comma list of the profiles traced (default phone)
     LANDS_PERF_LATENCY   1 runs the latency pass on the cold loads of the rows that have one (default off; lands-perf.yml
                          turns it on, so the CI gate grades B12-B15, Commons v2 step 9 gd-4s2Rw9hgBd). It needs the
                          fixture's test routes for the stream and the clean-up, which TestLandsPerfBaseline serves then.
     LANDS_PERF_TARGET    a label for the summary: fixture (TestLandsPerfBaseline sets it) or production
     LANDS_PERF_COOKIE    "name=value; name2=value2", set on <base url>'s host for every context: a signed-in session for a
                          deployed /garden. The fixture farm needs none.
     LANDS_PERF_PATHS     JSON {"surface": "/garden?..."} overriding a surface's path, for a deployed farm whose beads are
                          not the fixture's (gd-planA, gd-GlassLive01, ...)
   Run with `require`, it measures nothing until use() is called: the commons-ux-acceptance skill's shoot.js (P3.2
   gd-qeaDwFSDqq) measures the surface it shoots with the same load(), idle() and bench(), so a UX grade's perf rows are
   the numbers this script and the CI gate read. */
'use strict';
var base, out, pw, PROFILES;
var fs = require('fs'), path = require('path'), zlib = require('zlib'), assert = require('assert');
var worldPan = require(path.join(__dirname, 'world_pan_profile.js'));

var env = process.env;
var RUNS = +(env.LANDS_PERF_RUNS || 3), PAN = env.LANDS_PERF_PAN === undefined || env.LANDS_PERF_PAN === '' ? null : +env.LANDS_PERF_PAN, IDLE = +(env.LANDS_PERF_IDLE === undefined ? 300 : env.LANDS_PERF_IDLE);
var TARGET = env.LANDS_PERF_TARGET || 'production', PATHS = JSON.parse(env.LANDS_PERF_PATHS || '{}');
var SETTLE = 1000 * +(env.LANDS_PERF_SETTLE === undefined ? 10 : env.LANDS_PERF_SETTLE);
var LATENCY = env.LANDS_PERF_LATENCY === '1', KEYS = 'gardens', KEY_GAP = 150, LATENCY_WAIT = 10000;
var TRACE = env.LANDS_PERF_TRACE || '', TRACE_PROFILES = (env.LANDS_PERF_TRACE_PROFILES || 'phone').split(',').map(function (s) { return s.trim(); });
var QUIET = 5000, TTI_CAP = 30000;

function profilesOf(pw) {
  return {
    phone: {
      ctx: Object.assign({}, pw.devices['Pixel 5'], { viewport: { width: 390, height: 844 } }),
      cpu: 4, net: { offline: false, latency: 170, downloadThroughput: 9e6 / 8, uploadThroughput: 1.5e6 / 8 },
    },
    desktop: { ctx: { viewport: { width: 1280, height: 800 }, deviceScaleFactor: 1 }, cpu: 1, net: null },
  };
}

/* Point the script at a farm: o.base (the URL), o.pw (playwright-core); o.runs, o.idle (s), o.settle (s) and o.latency
   override the environment's LANDS_PERF_RUNS, LANDS_PERF_IDLE, LANDS_PERF_SETTLE and LANDS_PERF_LATENCY. */
function use(o) {
  base = o.base; pw = o.pw; PROFILES = profilesOf(pw);
  if (o.runs !== undefined) RUNS = o.runs;
  if (o.idle !== undefined) IDLE = o.idle;
  if (o.settle !== undefined) SETTLE = 1000 * o.settle;
  if (o.latency !== undefined) LATENCY = !!o.latency;
}

function blur(page) { return page.evaluate(function () { var a = document.activeElement; if (a && a.blur) a.blur(); }); }
function tapOrClick(page, profile, sel) { return profile === 'phone' ? page.tap(sel) : page.click(sel); }

/* The latency pass's roots (B14): a thread, a plan and an epic opened in the Commons v2 workbench (plan gd-lRaCxsiLvC step 4,
   web/commons2.js): jumped to by id (Ctrl-K, the id, Enter) from the root a UX grade's surface opened, and Back to it. The
   band's own roots, opened from its channel, went with the band (gd-ZeiSZV7j2Y). */
var WB_ROOTS = ['thread gd-wishPorch', 'plan gd-wbPlan', 'epic gd-wbStress'].map(function (r) {
  var id = r.split(' ')[1];
  // shown: its tree drawn with it selected, and its own document (not the last root's) on screen
  return { name: r.split(' ')[0], jump: id, shown: new Function('var s = COMMONS2.state(), id = ' + JSON.stringify(id) + ', r = document.querySelector("#wb2 .wb2-row[aria-selected=true]");' +
    ' return s.root === id && s.doc === id && !!r && r.dataset.id === id && window.__lat.seen(r) && !document.querySelector("#wb2 .wb2-doc[aria-busy]");') };
});
/* A workbench root's latency spec (a surface's latency; shoot.js's workbench rows use it too): the selected node's
   conversation (its document is the bead view's, gd-0GnBi7SRfK: list, input, send and text name its timeline, composer
   and a line's text there), #general's gd-wishPorch, and WB_ROOTS jumped to. push: on a phone the composer is on the node
   screen, pushed by a tap on the root's row in its tree (step 7, gd-DhGFLKK0tj). ready: the conversation's composer is back
   and the stream is on its channel. */
var WB_LATENCY = { section: '#wb2 .wb2-bead .bv-sec[data-sec=conversation]', list: '.bv-cv-list', input: '.bv-cv-say textarea', send: '.bv-cv-say button', text: '.bv-cv-text',
  channel: 'gd-chGeneral', thread: 'gd-wishPorch', open: true, roots: WB_ROOTS,
  push: '#wb2 .wb2-tree:not(.wb2-tree--kept) .wb2-row[aria-level="1"] > .wb2-line .wb2-t',
  ready: function () { return !!document.querySelector('#wb2 .wb2-bead .bv-cv-say textarea') && /channel=/.test(COMMONS2.state().stream); } };

/* One row per surface the plan budgets (P0.1): path opens it; ready(page, profile) resolves once it is on screen; act(page,
   profile) is the first interaction INP is read from, a thing a person does first on that surface. The paths name the
   fixture farm's beads (cardWorld, goldenPorch); LANDS_PERF_PATHS points them at a deployed farm's. */
var SURFACES = [
  // the World: /garden at its opening, the cursor on the first project; the first move of the cursor
  { name: 'world', label: 'World', path: '/garden', ready: async function (page) {
    await page.waitForFunction(function () { return document.querySelector('#cursor-bar .cb-me') && document.querySelector('#hots .hot.cur'); });
  }, act: async function (page, profile) {
    if (profile === 'phone') return page.tap('#cam-in');   // a phone's first press: In, a zoom step
    await page.focus('#hots .hot.cur'); await page.keyboard.press('ArrowRight');
  } },
  // Workbench, beads: plan A's bead view; scrolling its body a page (a wheel on the desktop, a swipe's worth on a phone)
  { name: 'beads', label: 'Workbench bead view', path: '/garden?bead=gd-planA', ready: paneReady(function () { return !!document.querySelector('#bv-body .bv-prog, #bv-body h1, #bv-body'); }), act: async function (page) {
    await page.click('#bv-body', { position: { x: 20, y: 20 } });
  } },
  // Workbench, plans: plan A's cockpit; opening its first step
  { name: 'plans', label: 'Workbench plan cockpit', path: '/garden?plan=gd-planA', ready: paneReady(function () { return document.querySelectorAll('#bv-body .pv-step').length > 0; }), act: async function (page, profile) {
    await tapOrClick(page, profile, '#bv-body .pv-step >> nth=0');
  } },
  // Workbench, inbox and decisions: the Queues panel on each tab; switching to the other tab
  { name: 'inbox', label: 'Workbench inbox', path: '/garden?queue=inbox', ready: paneReady(function () { return !!document.querySelector('#bv-body .qv-tabs'); }), act: async function (page, profile) {
    await tapOrClick(page, profile, '#bv-body .qv-tabs [role=tab] >> nth=1');
  } },
  { name: 'decisions', label: 'Workbench decisions', path: '/garden?queue=decisions', ready: paneReady(function () { return !!document.querySelector('#bv-body .qv-list .qv-row'); }), act: async function (page, profile) {
    await tapOrClick(page, profile, '#bv-body .qv-tabs [role=tab] >> nth=0');
  } },
  // the Glass: a live session in xterm; the Wall: every live session as tiles. Their first interaction is a tap on the pane.
  { name: 'glass', label: 'Glass (watch)', path: '/garden?glass=gd-GlassLive01', ready: paneReady(function () { var r = document.querySelector('#bv-body .gl-x .xterm-rows'); return !!r && r.textContent.trim().length > 0; }), act: async function (page) {
    await page.click('#bv-body .gl-note');
  } },
  { name: 'wall', label: 'Glass (Wall)', path: '/garden?glass=wall', ready: paneReady(function () { return document.querySelectorAll('#bv-body .wl-grid .wl-cell .wl-pre').length > 0; }), act: async function (page) {
    await page.click('#bv-body .wl-note');
  } },
  // the Commons v2 workbench (web/commons2.js, plan gd-lRaCxsiLvC): #general's gd-wishPorch as a thread root and plan
  // gd-wbPlan as a plan root, each on its tree screen; a j in the tree. Their latency pass is WB_LATENCY's (B12-B15).
  { name: 'commonsthread', label: 'Workbench thread root', path: '/garden/c/n/gd-wishPorch', ready: wbReady, act: wbAct, latency: WB_LATENCY },
  { name: 'commonsplan', label: 'Workbench plan root', path: '/garden/c/n/gd-wbPlan', ready: wbReady, act: wbAct, latency: WB_LATENCY },
];

/* The workbench open: its root's row selected and its document and rails loaded, focus off the page. */
async function wbReady(page) {
  await page.waitForSelector('#wb2 .wb2-row[aria-selected=true]', { state: 'visible' });
  await page.waitForFunction(function () {
    var d = document.querySelector('#wb2 .wb2-doc'), r = document.querySelector('#wb2 .wb2-rails');
    return !!d && !d.hasAttribute('aria-busy') && !!d.querySelector('.wb2-head') && !/Loading/.test(d.textContent) && !(r && /Loading/.test(r.textContent));
  });
  await blur(page);
}
async function wbAct(page) { await page.focus('#wb2 .wb2-tree'); await page.keyboard.press('j'); }
function paneReady(fn) {
  return async function (page) {
    await page.locator('#bead').waitFor({ state: 'visible' });
    await page.waitForFunction(fn);
  };
}

/* In the page, once per load: window.__lat, the latency pass's stamps. arm() makes the next keydown, pointerdown, touchstart
   or mousedown the interaction's start (t0, its event's timeStamp). For a press (not a key), the first pointerup, touchend
   or mouseup after it (t1) is the start instead when it came before the paint (start()): a tap or click acts on release,
   and the time a finger rests on the glass is not the page's (Event Timing, and so INP, never counts it either).
   Playwright's tap sends touchStart and touchEnd as two CDP calls, 35-80 ms apart on a loaded pod, and timing from the
   touchstart put that gap into every phone send (gardenio/docs/perf/commons-v2-step6-2026-11-03.md). until(src, ms)
   resolves with the time just after the frame in which the predicate (a function's source) first holds, checked on every
   DOM change and every frame, or null after ms; painted(cb) calls cb with that time for the frame now being drawn (a
   message posted from requestAnimationFrame runs once the frame is painted). While keying, every input event's time to
   paint from its keydown goes into keys; rows(list) starts counting the timeline rows (list > li, the surface's list,
   .cm-messages by default) whose subtree changes, and returns a stop() that answers how many. */
function latencyKit() {
  if (window.__lat) return;
  var L = window.__lat = { t0: null, t1: null, armed: false, pressed: false, keying: false, lastKey: 0, keys: [] };
  L.painted = function (cb) { requestAnimationFrame(function () { var mc = new MessageChannel(); mc.port1.onmessage = function () { cb(performance.now()); }; mc.port2.postMessage(0); }); };
  L.seen = function (el) { var r = el.getBoundingClientRect(); return r.width > 0 && r.height > 0 && (!el.checkVisibility || el.checkVisibility({ checkOpacity: true, checkVisibilityCSS: true })); };
  ['keydown', 'pointerdown', 'touchstart', 'mousedown'].forEach(function (t) {
    window.addEventListener(t, function (e) { if (L.armed) { L.armed = false; L.pressed = t !== 'keydown'; L.t0 = e.timeStamp; } }, true);
  });
  ['pointerup', 'touchend', 'mouseup'].forEach(function (t) {
    window.addEventListener(t, function (e) { if (L.pressed && L.t1 === null) L.t1 = e.timeStamp; }, true);
  });
  window.addEventListener('keydown', function (e) { L.lastKey = e.timeStamp; }, true);
  window.addEventListener('input', function () { if (!L.keying) return; var t = L.lastKey; L.painted(function (p) { L.keys.push(p - t); }); }, true);
  L.arm = function () { L.armed = true; L.pressed = false; L.t0 = null; L.t1 = null; };
  L.start = function (p) { return L.t1 !== null && L.t1 <= p ? L.t1 : L.t0; };
  L.until = function (src, ms) {
    var pred = (0, eval)('(' + src + ')');
    return new Promise(function (ok) {
      var done = false, mo, end = performance.now() + ms;
      function finish(v) { if (done) return; done = true; mo.disconnect(); ok(v); }
      function check() { if (done) return; var hit = false; try { hit = pred(); } catch (e) { hit = false; } if (hit) { done = true; mo.disconnect(); L.painted(ok); } }
      mo = new MutationObserver(check);
      mo.observe(document.documentElement, { subtree: true, childList: true, attributes: true, characterData: true });
      (function frame() { if (done) return; if (performance.now() > end) { finish(null); return; } check(); requestAnimationFrame(frame); })();
    });
  };
  L.rows = function (list) {
    list = list || '.cm-messages';
    var touched = new Set(), mo = new MutationObserver(function (ms) {
      ms.forEach(function (m) {
        var n = m.target.nodeType === 1 ? m.target : m.target.parentElement, li = n && n.closest ? n.closest(list + ' > li') : null;
        if (li) touched.add(li);
        if (n && n.matches && n.matches(list)) [].forEach.call(m.addedNodes, function (a) { touched.add(a); }), [].forEach.call(m.removedNodes, function (a) { touched.add(a); });
      });
    });
    mo.observe(document.documentElement, { subtree: true, childList: true, attributes: true, characterData: true });
    return function () { mo.takeRecords(); mo.disconnect(); return touched.size; };
  };
}

/* One timed step: arm the start (unless the start is the server's, stream), start waiting for shown, do it, and answer the
   ms from the start to the paint, or null (the error into errs) when it never showed. */
async function timed(page, shown, doIt, errs, what) {
  await page.evaluate(function (a) { var L = window.__lat; L.arm(); L.wait = L.until(a.src, a.ms); }, { src: String(shown), ms: LATENCY_WAIT });
  try { await doIt(); } catch (e) { errs.push(what + ': ' + String(e.message || e).split('\n')[0]); return null; }
  var v = await page.evaluate(function () { var L = window.__lat; return L.wait.then(function (p) { return p === null || L.t0 === null ? null : p - L.start(p); }); });
  if (v === null) errs.push(what + ': not shown within ' + LATENCY_WAIT + ' ms');
  else if (v < 0) { errs.push(what + ': shown before the input'); v = null; }
  return v;
}

/* The latency pass (the header's latency) on a loaded Commons surface s; profile picks how a send and an open are done.
   Every message it writes carries mark, and the farm forgets them after (POST /test/perf/forget), so the next load reads
   the farm it opened on. */
async function latencyPass(page, s, profile) {
  var L = s.latency, errs = [], out = { errors: errs }, mark = 'perf-' + Date.now().toString(36) + Math.random().toString(36).slice(2, 6);
  await page.evaluate(latencyKit);
  var list = L.section + ' ' + (L.list || '.cm-messages'), input = page.locator(L.section + ' ' + (L.input || '.cm-input')).first();
  // a phone's workbench (step 7, gd-DhGFLKK0tj): the composer is on the node screen, pushed by a tap on the selected row
  var push = async function () {
    if (!L.push || await input.isVisible().catch(function () { return false; })) return;
    await tapOrClick(page, profile, L.push); await input.waitFor({ state: 'visible', timeout: LATENCY_WAIT });
  };
  try {
    await push();
    // B12: keys into the composer, one at a time, and the rows they touched
    await input.fill('');
    await input.focus();
    await page.evaluate(function (list) { var l = window.__lat; l.keys = []; l.keying = true; l.stopRows = l.rows(list); }, L.list || '');
    for (var k of KEYS) { await page.keyboard.type(k); await page.waitForTimeout(KEY_GAP); }
    var kp = await page.evaluate(function () { var l = window.__lat; l.keying = false; return { each: l.keys.slice(), rows: l.stopRows() }; });
    out.keyPaint = { n: kp.each.length, median: median(kp.each), max: kp.each.length ? Math.max.apply(null, kp.each) : null, each: kp.each.map(Math.round) };
    out.rows = kp.rows;
    if (kp.each.length < KEYS.length) errs.push('keys: ' + kp.each.length + ' of ' + KEYS.length + ' input events painted');
    // B13: send it, to its echo in the timeline
    var text = 'Latency ' + mark;
    await input.fill(text);
    out.echo = await timed(page, 'function () { return [].some.call(document.querySelectorAll(' + JSON.stringify(list + ' > li') + '), function (li) { return li.textContent.indexOf(' + JSON.stringify(mark) + ') >= 0 && window.__lat.seen(li); }); }',
      async function () { if (profile === 'phone') await page.tap(L.section + ' ' + (L.send || '.cm-send')); else { await input.focus(); await page.keyboard.press('Enter'); } }, errs, 'echo');
  } catch (e) { errs.push('composer: ' + String(e.message || e).split('\n')[0]); }
  // B14: each root, opened cold, closed, opened again (warm)
  if (L.open) {
    out.open = {};
    if (L.push && await page.evaluate(function () { var v = document.getElementById('wb2'); return !!v && v.dataset.screen === 'node' && !!v.querySelector('.wb2-strip').offsetParent; })) await page.goBack();   // its jumps start on the tree screen
    for (var root of L.roots || WB_ROOTS) {
      // a root the surface is already on is not opened: it is on screen before the input, so it times nothing
      if (root.jump && await page.evaluate(function (id) { return COMMONS2.state().root === id; }, root.jump)) continue;
      var o = out.open[root.name] = { cold: null, warm: null }, link = root.link;
      try {
        if (root.href) {
          link = '[data-perf-root=' + root.name + ']';
          await page.evaluate(function (a) {
            var p = location.pathname, b = p.slice(0, Math.max(0, p.indexOf('/garden'))), at = document.querySelector(a.list + ' ' + a.text);
            var el = document.createElement('a'); el.href = b + a.href; el.textContent = ' ' + a.name; el.setAttribute('data-perf-root', a.name); at.appendChild(el);
          }, { list: list, text: L.text || '.cm-text', href: root.href, name: root.name });
        }
        for (var kind of ['cold', 'warm']) {
          if (root.jump) {   // the workbench: Ctrl-K, the id, and Enter timed; Back to where it was
            await page.focus('#wb2 .wb2-find'); await page.keyboard.press('Control+K'); await page.keyboard.type(root.jump);
            o[kind] = await timed(page, String(root.shown), function () { return page.keyboard.press('Enter'); }, errs, 'open ' + root.name + ' ' + kind);
            await page.goBack();
          } else {
            o[kind] = await timed(page, String(root.shown), function () { return tapOrClick(page, profile, link); }, errs, 'open ' + root.name + ' ' + kind);
            await tapOrClick(page, profile, root.close);
          }
          await page.waitForTimeout(300);
        }
      } catch (e) { errs.push('open ' + root.name + ': ' + String(e.message || e).split('\n')[0]); }
    }
  }
  // B15: a message the farm says on the stream, from the moment it is put there to its paint
  await push().catch(function (e) { errs.push('stream: ' + String(e.message || e).split('\n')[0]); });
  if (L.ready) await page.waitForFunction(L.ready, null, { timeout: LATENCY_WAIT }).catch(function () { errs.push('stream: the section was not back and listening'); });
  var said = 'Streamed ' + mark;
  await page.evaluate(function (a) { var l = window.__lat; l.wait = l.until(a.src, a.ms).then(function (p) { return p === null ? null : p + performance.timeOrigin; }); },
    { src: 'function () { return [].some.call(document.querySelectorAll(' + JSON.stringify(list + ' > li') + '), function (li) { return li.textContent.indexOf(' + JSON.stringify(said) + ') >= 0 && window.__lat.seen(li); }); }', ms: LATENCY_WAIT });
  var resp = await page.context().request.post(base + '/test/perf/say?' + new URLSearchParams({ ch: L.channel, thread: L.thread || '', text: said, mark: mark })).catch(function (e) { return { e: e }; });
  if (resp.e || !resp.ok()) {
    errs.push('stream: the farm has no POST /test/perf/say (' + (resp.e ? String(resp.e.message || resp.e).split('\n')[0] : resp.status()) + ')');
    out.stream = null;
  } else {
    var at = (await resp.json()).at, painted = await page.evaluate(function () { return window.__lat.wait; });
    out.stream = painted === null ? null : painted - at;
    if (painted === null) errs.push('stream: not shown within ' + LATENCY_WAIT + ' ms');
  }
  await page.context().request.post(base + '/test/perf/forget?' + new URLSearchParams({ mark: mark })).catch(function () {});
  return out;
}

/* In the page before its own scripts: observers for long tasks, LCP and Event Timing, buffered into window.__perf, so a
   long task while the inline scripts parse is seen. */
function observe() {
  var p = window.__perf = { long: [], lcp: 0, lcpEl: '', events: [] };
  try {
    new PerformanceObserver(function (l) { l.getEntries().forEach(function (e) { p.long.push([e.startTime, e.duration]); }); }).observe({ type: 'longtask', buffered: true });
  } catch (e) { p.noLong = String(e); }
  try {
    new PerformanceObserver(function (l) {
      l.getEntries().forEach(function (e) {
        p.lcp = e.startTime;
        var el = e.element;
        p.lcpEl = el ? el.tagName.toLowerCase() + (el.id ? '#' + el.id : '') + (el.className && typeof el.className === 'string' ? '.' + el.className.trim().split(/\s+/).join('.') : '') : (e.url || '');
      });
    }).observe({ type: 'largest-contentful-paint', buffered: true });
  } catch (e) { p.noLcp = String(e); }
  try {
    new PerformanceObserver(function (l) { l.getEntries().forEach(function (e) { if (e.interactionId) p.events.push([e.name, e.startTime, e.duration, e.interactionId]); }); }).observe({ type: 'event', durationThreshold: 16, buffered: true });
  } catch (e) { p.noEvents = String(e); }
}

function readPage() {
  var nav = performance.getEntriesByType('navigation')[0] || {}, fcp = performance.getEntriesByName('first-contentful-paint')[0];
  var res = performance.getEntriesByType('resource').map(function (r) { return [r.startTime, r.responseEnd]; });
  return {
    now: performance.now(), dcl: nav.domContentLoadedEventEnd, load: nav.loadEventEnd,
    fcp: fcp ? fcp.startTime : null, lcp: window.__perf.lcp, lcpEl: window.__perf.lcpEl, long: window.__perf.long.slice(), res: res,
    events: window.__perf.events.slice(),
  };
}

/* TTI over a page read (readPage), as Lighthouse has it: from FCP (or DOMContentLoaded, if later), find the first 5 s window
   with no long task and no more than two requests in flight, trying each long task's end and each request's end as a start;
   TTI is the end of the last long task before that window (or FCP). null while no such window has closed yet. */
function tti(r) {
  var from = Math.max(r.fcp || 0, r.dcl || 0), long = r.long.slice().sort(function (a, b) { return a[0] - b[0]; });
  var ends = long.map(function (t) { return t[0] + t[1]; }).concat(r.res.map(function (q) { return q[1]; }));
  var starts = [from].concat(ends.filter(function (e) { return e > from; })).sort(function (a, b) { return a - b; });
  for (var s of starts) {
    if (s + QUIET > r.now) return null;
    var busy = long.some(function (t) { return t[0] + t[1] > s && t[0] < s + QUIET; });
    if (busy) continue;
    var points = [s].concat(r.res.map(function (q) { return q[0]; }).filter(function (t) { return t > s && t < s + QUIET; }));
    var crowded = points.some(function (t) { return r.res.filter(function (q) { return q[0] <= t && (q[1] === 0 || q[1] > t); }).length > 2; });
    if (!crowded) return long.reduce(function (a, t) { return t[0] + t[1] <= s ? Math.max(a, t[0] + t[1]) : a; }, from);
  }
  return null;
}

/* The network as CDP saw it: every request's type, MIME, wire bytes, decoded bytes, and whether it is a stream. mark()
   starts a window and since() returns what crossed the wire in it: every request sent after the mark, and for one open at
   the mark (an EventSource opened by the load, a fetch still in flight) a copy carrying only the bytes since (carried:
   true), so a stream's idle frames count and its load-time bytes do not. */
function netTap(cdp) {
  var reqs = new Map(), t0 = 0, doc = {}, atMark = new Map();
  cdp.on('Network.requestWillBeSent', function (e) {
    if (/^(data|blob):/.test(e.request.url)) return;   // no network
    if (!reqs.has(e.requestId)) reqs.set(e.requestId, { url: e.request.url, type: e.type, wire: 0, raw: 0, done: false, at: e.timestamp, cached: false });
    if (e.type === 'Document' && !doc.sent) doc.sent = e.timestamp;
  });
  cdp.on('Network.responseReceived', function (e) {
    var q = reqs.get(e.requestId); if (!q) return;
    if (e.type === 'Document' && !doc.got) doc.got = e.timestamp;
    q.mime = e.response.mimeType; q.status = e.response.status; q.enc = (e.response.headers['content-encoding'] || e.response.headers['Content-Encoding'] || '');
    q.cached = !!(e.response.fromDiskCache || e.response.fromMemoryCache || e.response.fromServiceWorker);
    q.stream = e.type === 'EventSource' || /event-stream/.test(e.response.mimeType || '');
  });
  cdp.on('Network.requestServedFromCache', function (e) { var q = reqs.get(e.requestId); if (q) q.cached = true; });
  cdp.on('Network.dataReceived', function (e) { var q = reqs.get(e.requestId); if (q) { q.raw += e.dataLength; q.wire += e.encodedDataLength; } });
  cdp.on('Network.loadingFinished', function (e) { var q = reqs.get(e.requestId); if (q) { q.wire = Math.max(q.wire, e.encodedDataLength); q.done = true; } });
  cdp.on('Network.loadingFailed', function (e) { var q = reqs.get(e.requestId); if (q) { q.failed = e.errorText; q.done = true; } });
  return { reqs: reqs, ttfb: function () { return doc.sent && doc.got ? (doc.got - doc.sent) * 1000 : null; }, mark: function () { t0 = reqs.size; atMark = new Map(); reqs.forEach(function (q, id) { atMark.set(id, q.wire); }); },
    since: function () {
      var all = Array.from(reqs.values()), carried = [];
      atMark.forEach(function (w, id) { var q = reqs.get(id); if (q.wire > w) carried.push(Object.assign({}, q, { wire: q.wire - w, carried: true })); });
      return carried.concat(all.slice(t0));
    } };
}

async function gzSize(cdp, id, q) {
  if (q.cached || q.stream || q.failed || !q.done || q.raw === 0 || q.status === 304) return q.cached ? 0 : q.wire;
  try {
    var b = await cdp.send('Network.getResponseBody', { requestId: id });
    return zlib.gzipSync(Buffer.from(b.body, b.base64Encoded ? 'base64' : 'utf8'), { level: 6 }).length;
  } catch (e) { return q.wire; }   // evicted or not text: count what crossed
}

async function weigh(cdp, tap) {
  var w = { raw: 0, wire: 0, gz: 0, requests: 0, cached: 0, streams: 0, streamBytes: 0, byType: {} };
  for (var [id, q] of tap.reqs) {
    if (!q.done && !q.failed) q.stream = true;   // still open once the page is quiet: an EventSource or a held fetch
    if (q.stream) { w.streams++; w.streamBytes += q.wire; continue; }
    w.requests++;
    if (q.cached) { w.cached++; continue; }
    var gz = await gzSize(cdp, id, q), k = q.type || 'Other';
    w.raw += q.raw; w.wire += q.wire; w.gz += gz;
    var t = w.byType[k] || (w.byType[k] = { n: 0, raw: 0, wire: 0, gz: 0 });
    t.n++; t.raw += q.raw; t.wire += q.wire; t.gz += gz;
  }
  w.top = Array.from(tap.reqs.values()).filter(function (q) { return !q.stream && !q.cached; })
    .sort(function (a, b) { return b.raw - a.raw; }).slice(0, 6)
    .map(function (q) { return { url: q.url.replace(base, ''), type: q.type, raw: q.raw, wire: q.wire, enc: q.enc || '' }; });
  return w;
}

/* LANDS_PERF_COOKIE as Playwright cookies on base's host. */
function cookies() {
  if (!env.LANDS_PERF_COOKIE) return [];
  var host = new URL(base).hostname;
  return env.LANDS_PERF_COOKIE.split(';').map(function (c) {
    var i = c.indexOf('='); return { name: c.slice(0, i).trim(), value: c.slice(i + 1).trim(), domain: host, path: '/', secure: base.indexOf('https:') === 0 };
  }).filter(function (c) { return c.name; });
}

/* s, when given, may carry ctx (more context options: a fixture header, say) and init (a script before the page's own). */
async function newContext(browser, profile, s) {
  var ctx = await browser.newContext(Object.assign({}, PROFILES[profile].ctx, s && s.ctx, { locale: 'en-US', timezoneId: 'UTC' }));
  if (env.LANDS_PERF_COOKIE) await ctx.addCookies(cookies());
  await ctx.addInitScript(observe);
  if (s && s.init) await ctx.addInitScript(s.init);
  return ctx;
}

async function throttled(page, profile) {
  var cdp = await page.context().newCDPSession(page), p = PROFILES[profile];
  await cdp.send('Network.enable');
  await cdp.send('Performance.enable');
  if (p.net) await cdp.send('Network.emulateNetworkConditions', p.net);
  if (p.cpu > 1) await cdp.send('Emulation.setCPUThrottlingRate', { rate: p.cpu });
  return cdp;
}

/* One load: navigate, wait for the surface, read paint timing before touching it, wait for TTI's quiet window, weigh the
   requests, then the first interaction and its INP. */
async function load(page, cdp, s, profile, settle) {
  var tap = netTap(cdp), errors = [], r = { surface: s.name, profile: profile };
  page.on('pageerror', function (e) { errors.push(String(e)); });
  var url = base + (PATHS[s.name] || s.path), t0 = Date.now();
  var resp = await page.goto(url, { waitUntil: 'commit', timeout: 60000 });
  r.status = resp ? resp.status() : 0;
  await s.ready(page, profile);
  r.ready = await page.evaluate(function () { return performance.now(); });
  var pr;
  for (;;) {
    pr = await page.evaluate(readPage);
    var t = tti(pr);
    if (t !== null) { r.tti = t; break; }
    if (pr.now > TTI_CAP) { r.tti = null; break; }
    await page.waitForTimeout(500);
  }
  Object.assign(r, { ttfb: tap.ttfb(), fcp: pr.fcp, lcp: pr.lcp || null, lcpEl: pr.lcpEl, dcl: pr.dcl, load: pr.load });
  var upTo = r.tti === null ? pr.now : r.tti, lt = pr.long.filter(function (x) { return x[0] < upTo; });
  r.longList = pr.long.map(function (x) { return [Math.round(x[0]), Math.round(x[1])]; });
  r.longTasks = { n: lt.length, total: lt.reduce(function (a, x) { return a + x[1]; }, 0), max: lt.reduce(function (a, x) { return Math.max(a, x[1]); }, 0) };
  r.bytes = await weigh(cdp, tap);
  var before = (await page.evaluate(readPage)).events.length;
  try {
    await s.act(page, profile);
    await page.evaluate(function () { return new Promise(function (ok) { requestAnimationFrame(function () { requestAnimationFrame(function () { setTimeout(ok, 300); }); }); }); });
    var ev = (await page.evaluate(readPage)).events.slice(before);
    r.inp = ev.reduce(function (a, e) { return Math.max(a, e[2]); }, 0);
    r.inpEvents = ev.map(function (e) { return e[0] + ' ' + Math.round(e[2]); });
  } catch (e) { r.inp = null; r.actError = String(e.message || e).split('\n')[0]; }
  if (settle && LATENCY && s.latency) {
    try { r.latency = await latencyPass(page, s, profile); }
    catch (e) { r.latency = { errors: ['latency pass: ' + String(e.message || e).split('\n')[0]] }; }
  }
  if (settle && SETTLE > 0) {
    var n0 = (await page.evaluate(readPage)).long.length;
    await page.waitForTimeout(SETTLE);
    var sl = (await page.evaluate(readPage)).long.slice(n0);
    r.settled = { ms: SETTLE, n: sl.length, total: sl.reduce(function (a, x) { return a + x[1]; }, 0), max: sl.reduce(function (a, x) { return Math.max(a, x[1]); }, 0) };
  }
  r.wall = Date.now() - t0;
  r.errors = errors;
  return r;
}

async function measure(browser, s, profile, run) {
  var ctx = await newContext(browser, profile, s), rows = [];
  try {
    for (var kind of ['cold', 'warm']) {
      var b0 = kind === 'cold' && PROFILES[profile].cpu > 1 ? await bench(browser) : null;
      var page = await ctx.newPage(), cdp = await throttled(page, profile);
      try {
        var r = await load(page, cdp, s, profile, kind === 'cold');
        r.kind = kind; r.run = run;
        if (b0 !== null) r.bench = [b0, await bench(browser)];
        rows.push(r);
        console.log([profile, s.name, kind, 'run ' + run, 'ttfb ' + ms(r.ttfb), 'fcp ' + ms(r.fcp), 'lcp ' + ms(r.lcp), 'ready ' + ms(r.ready), 'tti ' + ms(r.tti),
          'inp ' + ms(r.inp), 'settled ' + (r.settled ? r.settled.n + '/' + ms(r.settled.total) : '-'), 'long ' + r.longTasks.n + '/' + ms(r.longTasks.total), 'req ' + r.bytes.requests + (r.bytes.cached ? ' (' + r.bytes.cached + ' cached)' : '') + ' streams ' + r.bytes.streams,
          'raw ' + kb(r.bytes.raw), 'wire ' + kb(r.bytes.wire), 'gz ' + kb(r.bytes.gz)].join('  ') + (r.bench ? '  bench ' + r.bench.join('/') : '') + (r.actError ? '  act: ' + r.actError : '') + (r.errors.length ? '  errors: ' + r.errors.length : '') +
          (r.latency ? '  latency ' + latencyLine(r.latency) : ''));
      } catch (e) {
        console.log(profile + ' ' + s.name + ' ' + kind + ' run ' + run + ' FAILED: ' + String(e.message || e).split('\n')[0]);
        rows.push({ surface: s.name, profile: profile, kind: kind, run: run, error: String(e.message || e).split('\n')[0] });
      } finally { await page.close(); }
    }
  } finally { await ctx.close(); }
  return rows;
}

/* One run of the pan pass (the header's pan rows): world_pan_profile.js's calibration, pan and zoom on the phone's throttle,
   with no idle phase (B7 does not read it). */
async function pan(browser, run) {
  var t0 = Date.now(), r = { surface: 'world', profile: 'phone', kind: 'pan', run: run };
  try {
    var rep = await worldPan.profile(browser, { base: base, pw: pw, cpu: PROFILES.phone.cpu, idle: 0, cookies: cookies() });
    var lt = function (l) { return { n: l.n, total: l.total, max: l.max }; };
    Object.assign(r, { ready: rep.ready_ms, panMs: rep.pan.ms, frames: rep.pan.raf, longTasks: lt(rep.pan.longTasks), longList: rep.pan.longTasks.list,
      calibration: rep.calibration, zoom: { ms: rep.zoom.ms, frames: rep.zoom.raf, longTasks: lt(rep.zoom.longTasks) }, errors: rep.pan.errors.concat(rep.zoom.errors) });
    var c = r.calibration;
    console.log(['phone', 'world', 'pan', 'run ' + run, 'calibration ' + c.frames + ' frames, long ' + c.long + '/' + ms(c.total) + '/' + ms(c.max),
      'pan ' + ms(r.panMs) + ' ms, ' + r.frames + ' frames, long ' + r.longTasks.n + '/' + ms(r.longTasks.total) + '/' + ms(r.longTasks.max),
      'zoom long ' + r.zoom.longTasks.n + '/' + ms(r.zoom.longTasks.total) + '/' + ms(r.zoom.longTasks.max)].join('  ') + (r.errors.length ? '  errors: ' + r.errors.length : ''));
  } catch (e) {
    r.error = String(e.message || e).split('\n')[0];
    console.log('phone world pan run ' + run + ' FAILED: ' + r.error);
  }
  r.wall = Date.now() - t0;
  return r;
}

/* One more cold load of a surface under a Playwright trace (LANDS_PERF_TRACE), for a person to open when a number moves;
   its numbers are thrown away, since snapshotting the DOM at every step costs the page time. */
async function trace(browser, s, profile) {
  var ctx = await newContext(browser, profile), file = path.join(TRACE, profile + '-' + s.name + '.zip');
  await ctx.tracing.start({ name: profile + '-' + s.name, title: profile + ' ' + s.label + ' (cold)', screenshots: true, snapshots: true });
  try {
    var page = await ctx.newPage(), cdp = await throttled(page, profile);
    await load(page, cdp, s, profile, false);
  } catch (e) {
    console.log(profile + ' ' + s.name + ' trace load FAILED: ' + String(e.message || e).split('\n')[0]);
  } finally {
    await ctx.tracing.stop({ path: file });
    await ctx.close();
  }
  console.log(profile + ' ' + s.name + ' trace: ' + file);
}

/* The idle pass: every surface open in its own context at once, left alone IDLE seconds, then the heap as it stands and
   after a forced GC, and what the page fetched while idle (polls, stream frames): the requests it sent from the moment the
   last surface was open to the moment its row is read, idleMs (IDLE s and the reads before it), and the frames its open
   streams received in that window. The tap starts before the page loads, so it knows the streams the load opened. */
async function idle(browser, surfaces, profile) {
  var open = [];
  for (var s of surfaces) {
    var ctx = await newContext(browser, profile, s), page = await ctx.newPage(), cdp = await throttled(page, profile), tap = netTap(cdp);
    page.setDefaultTimeout(120000);   // the pages opened before it are still running on the same throttled CPUs
    try {
      await page.goto(base + (PATHS[s.name] || s.path), { waitUntil: 'commit', timeout: 120000 });
      await s.ready(page, profile);
      await page.waitForTimeout(2000);
      var m0 = await metrics(cdp);
      open.push({ s: s, ctx: ctx, cdp: cdp, tap: tap, m0: m0 });
    } catch (e) {
      console.log(profile + ' ' + s.name + ' idle FAILED to open: ' + String(e.message || e).split('\n')[0]);
      await ctx.close();
    }
  }
  console.log(profile + ': ' + open.length + ' surfaces open, idling ' + IDLE + ' s');
  // the idle window starts now, for every surface at once: opening the ones after it took 40-140 s on a CI pod, and the
  // requests a page sent meanwhile used to count against IDLE s, so B8 read the pod's speed (gd-CLQT35hQnk)
  var t0 = Date.now();
  open.forEach(function (o) { o.tap.mark(); });
  await new Promise(function (ok) { setTimeout(ok, IDLE * 1000); });
  var rows = [];
  for (var o of open) {
    var m1 = await metrics(o.cdp), got = o.tap.since(), idleMs = Date.now() - t0;
    await o.cdp.send('HeapProfiler.collectGarbage');
    var m2 = await metrics(o.cdp);
    var row = { surface: o.s.name, profile: profile, kind: 'idle', idleS: IDLE, idleMs: idleMs, heapLoaded: m0Heap(o.m0), heapIdle: m1.JSHeapUsedSize, heapIdleGC: m2.JSHeapUsedSize,
      nodes: m1.Nodes, listeners: m1.JSEventListeners, idleRequests: idleRequests(got), idleBytes: idleBytes(got) };
    console.log([profile, o.s.name, 'idle ' + Math.round(idleMs / 1000) + 's', 'heap loaded ' + mb(row.heapLoaded), 'idle ' + mb(row.heapIdle), 'after GC ' + mb(row.heapIdleGC),
      'nodes ' + row.nodes, 'idle req ' + row.idleRequests, 'idle bytes ' + kb(row.idleBytes)].join('  '));
    rows.push(row);
    await o.ctx.close();
  }
  return rows;
}
/* A tap window's requests sent (not streams, nor ones carried over from before it) and its wire bytes, streams included. */
function idleRequests(got) { return got.filter(function (q) { return !q.stream && !q.carried; }).length; }
function idleBytes(got) { return got.reduce(function (a, q) { return a + q.wire; }, 0); }
function m0Heap(m) { return m.JSHeapUsedSize; }
async function metrics(cdp) {
  var m = {}; (await cdp.send('Performance.getMetrics')).metrics.forEach(function (x) { m[x.name] = x.value; }); return m;
}

/* The machine's speed, unthrottled, in a blank page: a fixed workload's median ms over five tries. The phone profile's
   numbers scale with it (4x of a busy node's CPU is slower than 4x of an idle one's), so two runs compare only near the
   same bench; it is taken before and after the run and written to the summary. */
async function bench(browser) {
  var ctx = await browser.newContext(), page = await ctx.newPage();
  try {
    return await page.evaluate(function () {
      var tries = [];
      for (var k = 0; k < 5; k++) {
        var t = performance.now(), o = [];
        for (var i = 0; i < 20000; i++) o.push({ id: 'gd-' + i.toString(36), n: i * 1.5, tags: ['a' + (i % 7), 'b' + (i % 11)] });
        var j = JSON.parse(JSON.stringify(o)), h = 0;
        for (var x = 0; x < 2e6; x++) h = (h * 31 + x) | 0;
        j.sort(function (a, b) { return (b.n % 97) - (a.n % 97); });
        tries.push(performance.now() - t + (h & 0));
      }
      tries.sort(function (a, b) { return a - b; });
      return Math.round(tries[2]);
    });
  } finally { await ctx.close(); }
}

/* A row's latency pass on one line: key-to-paint median/max and rows, echo, each root's cold/warm open, stream. */
function latencyLine(l) {
  var o = l.open ? ' open ' + WB_ROOTS.map(function (x) { var v = l.open[x.name] || {}; return x.name + ' ' + ms(v.cold) + '/' + ms(v.warm); }).join(', ') : '';
  return 'keys ' + (l.keyPaint ? ms(l.keyPaint.median) + '/' + ms(l.keyPaint.max) : '-') + ' rows ' + (l.rows === undefined ? '-' : l.rows) + ' echo ' + ms(l.echo) + o +
    ' stream ' + ms(l.stream) + (l.errors && l.errors.length ? ' (' + l.errors.join('; ') + ')' : '');
}

function ms(v) { return v === null || v === undefined ? '-' : Math.round(v) + ''; }
function kb(v) { return v === null || v === undefined ? '-' : (v / 1024).toFixed(v < 10240 ? 1 : 0); }
function mb(v) { return v === null || v === undefined ? '-' : (v / 1048576).toFixed(1); }
function median(xs) {
  xs = xs.filter(function (x) { return x !== null && x !== undefined && !isNaN(x); }).sort(function (a, b) { return a - b; });
  if (!xs.length) return null;
  var m = xs.length >> 1; return xs.length % 2 ? xs[m] : (xs[m - 1] + xs[m]) / 2;
}

function summarise(rows, meta) {
  var lines = ['# Lands performance baseline', '',
    'Target: ' + meta.target + ' (' + meta.base + '), Chromium ' + meta.chromium + ', ' + meta.at + '. Medians of ' + RUNS + ' run(s). ' +
    'CPU bench (unthrottled, lower is faster) ' + meta.bench.join(' ms before, ') + ' ms after: compare runs only at a similar bench.', '',
    'Phone: 390x844, 4x CPU slowdown, 9 Mbps down / 1.5 Mbps up / 170 ms RTT. Desktop: 1280x800, unthrottled. ' +
    'Times in ms from navigation start; sizes in KB. raw = decoded bodies, wire = as served, gz = every body gzipped. ' +
    'INP 0 = under 16 ms (Chromium reports no Event Timing entry below that). ' +
    'TTI ">30000" = no 5 s quiet window within 30 s. ' + (SETTLE > 0 ? 'Settled = long tasks in the ' + SETTLE / 1000 + ' s after the first interaction (cold loads).' : 'Settled: not measured (LANDS_PERF_SETTLE=0).'), ''];
  Object.keys(PROFILES).forEach(function (profile) {
    var any = rows.some(function (r) { return r.profile === profile; });
    if (!any) return;
    lines.push('## ' + profile, '');
    lines.push('| Surface | Load | Raw KB | Wire KB | gz KB | Req | TTFB | FCP | LCP | Ready | TTI | INP | Long tasks to TTI (n / total / max) | Settled ' + SETTLE / 1000 + ' s (n / total ms) |');
    lines.push('|---|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|---|---|');
    SURFACES.forEach(function (s) {
      ['cold', 'warm'].forEach(function (kind) {
        var rs = rows.filter(function (r) { return r.profile === profile && r.surface === s.name && r.kind === kind && !r.error; });
        if (!rs.length) return;
        var m = function (f) { return median(rs.map(f)); }, never = rs.filter(function (r) { return r.tti === null; }).length;
        lines.push('| ' + [s.label, kind, kb(m(function (r) { return r.bytes.raw; })), kb(m(function (r) { return r.bytes.wire; })), kb(m(function (r) { return r.bytes.gz; })),
          ms(m(function (r) { return r.bytes.requests; })), ms(m(function (r) { return r.ttfb; })), ms(m(function (r) { return r.fcp; })), ms(m(function (r) { return r.lcp; })),
          ms(m(function (r) { return r.ready; })), never === rs.length ? '>' + TTI_CAP : ms(m(function (r) { return r.tti; })) + (never ? ' (' + never + ' >' + TTI_CAP / 1000 + 's)' : ''),
          ms(m(function (r) { return r.inp; })), ms(m(function (r) { return r.longTasks.n; })) + ' / ' + ms(m(function (r) { return r.longTasks.total; })) + ' / ' + ms(m(function (r) { return r.longTasks.max; })),
          kind === 'cold' ? ms(m(function (r) { return r.settled && r.settled.n; })) + ' / ' + ms(m(function (r) { return r.settled && r.settled.total; })) : ''].join(' | ') + ' |');
      });
    });
    var latRows = rows.filter(function (r) { return r.profile === profile && r.kind === 'cold' && r.latency; });
    if (latRows.length) {
      lines.push('', 'Latency pass (B12–B15, cold loads): ms from the input (the server\'s push for the stream) to just after the frame that shows it. Open is cold / warm.', '');
      lines.push('| Surface | Key to paint (median / max) | Rows touched | Send to echo | ' + WB_ROOTS.map(function (x) { return 'Open ' + x.name; }).join(' | ') + ' | Stream to paint |');
      lines.push('|---|--:|--:|--:|' + WB_ROOTS.map(function () { return '--:'; }).join('|') + '|--:|');
      SURFACES.forEach(function (s) {
        var rs = latRows.filter(function (r) { return r.surface === s.name; }).map(function (r) { return r.latency; });
        if (!rs.length) return;
        var m = function (f) { return median(rs.map(function (l) { try { return f(l); } catch (e) { return null; } })); };
        lines.push('| ' + [s.label, ms(m(function (l) { return l.keyPaint.median; })) + ' / ' + ms(m(function (l) { return l.keyPaint.max; })), ms(m(function (l) { return l.rows; })), ms(m(function (l) { return l.echo; }))]
          .concat(WB_ROOTS.map(function (x) { return rs.some(function (l) { return l.open; }) ? ms(m(function (l) { return l.open[x.name].cold; })) + ' / ' + ms(m(function (l) { return l.open[x.name].warm; })) : '–'; }))
          .concat([ms(m(function (l) { return l.stream; }))]).join(' | ') + ' |');
      });
    }
    var panRows = rows.filter(function (r) { return r.profile === profile && r.kind === 'pan' && !r.error; });
    if (panRows.length) {
      lines.push('', 'Pan pass (B7): the World on the 10x farm, a 10 s pan and a zoom, after a bare full-screen canvas at the same throttle (calibration). Long tasks are n / total / max ms; the load is the pan\'s time per frame over the bare canvas\'s.', '');
      lines.push('| Run | Calibration frames | Calibration long tasks | Pan frames | Pan long tasks | Load | Zoom long tasks |');
      lines.push('|--:|--:|---|--:|---|--:|---|');
      panRows.forEach(function (r) {
        var c = r.calibration, l = r.longTasks, z = r.zoom.longTasks, load = c.frames > 0 && r.frames > 0 ? (r.panMs / r.frames) / (c.ms / c.frames) : null;
        lines.push('| ' + [r.run, c.frames, c.long + ' / ' + ms(c.total) + ' / ' + ms(c.max), r.frames, l.n + ' / ' + ms(l.total) + ' / ' + ms(l.max),
          load === null ? '-' : load.toFixed(2) + 'x', z.n + ' / ' + ms(z.total) + ' / ' + ms(z.max)].join(' | ') + ' |');
      });
    }
    var idleRows = rows.filter(function (r) { return r.profile === profile && r.kind === 'idle'; });
    if (idleRows.length) {
      lines.push('', 'Idle ' + idleRows[0].idleS + ' s, every surface open at once:', '');
      lines.push('| Surface | Heap loaded MB | Heap idle MB | Heap after GC MB | DOM nodes | Idle requests | Idle KB |');
      lines.push('|---|--:|--:|--:|--:|--:|--:|');
      idleRows.forEach(function (r) {
        var s = SURFACES.find(function (x) { return x.name === r.surface; });
        lines.push('| ' + [s.label, mb(r.heapLoaded), mb(r.heapIdle), mb(r.heapIdleGC), r.nodes, r.idleRequests, kb(r.idleBytes)].join(' | ') + ' |');
      });
    }
    lines.push('');
  });
  var failed = rows.filter(function (r) { return r.error || r.actError || (r.errors && r.errors.length) || (r.latency && r.latency.errors.length); });
  if (failed.length) {
    lines.push('Problems:', '');
    failed.forEach(function (r) { lines.push('- ' + r.profile + ' ' + r.surface + ' ' + r.kind + ' run ' + r.run + ': ' + (r.error || r.actError || (r.errors && r.errors.length ? r.errors.join('; ') : 'latency: ' + r.latency.errors.join('; ')))); });
    lines.push('');
  }
  return lines.join('\n');
}

module.exports = { SURFACES: SURFACES, WB_ROOTS: WB_ROOTS, WB_LATENCY: WB_LATENCY, use: use, measure: measure, idle: idle, pan: pan, bench: bench, median: median, netTap: netTap, idleRequests: idleRequests, idleBytes: idleBytes };

if (require.main === module) (async function () {
  base = process.argv[2]; out = process.argv[4];
  assert.ok(base && process.argv[3] && out, 'usage: node perf_baseline.js <base url> <node_modules> <out dir>');
  use({ base: base, pw: require(require.resolve('playwright-core', { paths: [process.argv[3]] })) });
  var only = (env.LANDS_PERF_SURFACES || '').split(',').map(function (s) { return s.trim(); }).filter(Boolean);
  var profiles = (env.LANDS_PERF_PROFILES || 'phone,desktop').split(',').map(function (s) { return s.trim(); }).filter(Boolean);
  var surfaces = SURFACES.filter(function (s) { return only.length ? only.indexOf(s.name) >= 0 : !s.optIn; });
  profiles.forEach(function (p) { assert.ok(PROFILES[p], 'unknown profile ' + p); });
  assert.ok(surfaces.length, 'no surface matches LANDS_PERF_SURFACES=' + env.LANDS_PERF_SURFACES);
  fs.mkdirSync(out, { recursive: true });
  if (TRACE) fs.mkdirSync(TRACE, { recursive: true });
  var browser = await pw.chromium.launch();
  var pans = PAN === null ? RUNS : PAN;
  var meta = { target: TARGET, base: TARGET === 'fixture' ? 'httptest' : base, chromium: browser.version(), at: new Date().toISOString(), runs: RUNS, idle: IDLE, settle: SETTLE / 1000, pan: pans, bench: [await bench(browser)] };
  console.log('perf baseline: ' + surfaces.length + ' surfaces x ' + profiles.join(',') + ' x ' + RUNS + ' runs, Chromium ' + meta.chromium);
  var rows = [];
  try {
    for (var profile of profiles) {
      // one unmeasured load first, so the first surface measured does not pay the server's first snapshot build
      var wctx = await newContext(browser, profile), wp = await wctx.newPage();
      try { await wp.goto(base + (PATHS[surfaces[0].name] || surfaces[0].path), { timeout: 60000 }); await surfaces[0].ready(wp, profile); }
      catch (e) { console.log('warm-up: ' + String(e.message || e).split('\n')[0]); }
      finally { await wctx.close(); }
      for (var s of surfaces) {
        for (var run = 1; run <= RUNS; run++) rows = rows.concat(await measure(browser, s, profile, run));
        if (TRACE && TRACE_PROFILES.indexOf(profile) >= 0) await trace(browser, s, profile);
      }
      if (profile === 'phone' && surfaces.some(function (s) { return s.name === 'world'; })) {
        for (var pr = 1; pr <= pans; pr++) rows.push(await pan(browser, pr));
      }
      if (IDLE > 0) rows = rows.concat(await idle(browser, surfaces, profile));
      fs.writeFileSync(path.join(out, 'results.json'), JSON.stringify({ meta: meta, rows: rows }, null, 1));   // a long run keeps what it has
    }
    meta.bench.push(await bench(browser));
    fs.writeFileSync(path.join(out, 'results.json'), JSON.stringify({ meta: meta, rows: rows }, null, 1));   // with the closing bench
  } finally { await browser.close(); }
  var md = summarise(rows, meta);
  fs.writeFileSync(path.join(out, 'summary.md'), md + '\n');
  console.log('\n' + md);
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
