/* Browser walk for the teensy music player (gd-z8nt2TpS6o), run from TestTunesUnderChromium against a live /garden on an
   httptest server. Usage: node tunes_browser.js <base url> <node_modules dir>; GARDENHUB_SHOTS=<dir> also writes PNGs.

   HTMLMediaElement and fetch are stubbed before the page's scripts run, so nothing leaves the test: the hub's playlist.json
   answers with FIXTURE (made-up songs), play() fires the play event and resolves, pause() fires pause, currentTime seeks
   and ticks timeupdate, and duration is 100 s. Desktop: the HUD shows the note button alone, no panel and no request yet;
   a press fetches the playlist once, turns the button into Pause and opens the panel with a title from the playlist, a
   link to the song on the hub, the scrubber at 0:00 of 1:40; a tick moves the scrubber; a drag seeks; Next changes the song
   and keeps playing without a second fetch; a press pauses and the panel folds; the saved state brings the same song back
   on a reload. Phone (Pixel 5): the button has a 44 px target and the panel is at most 160 px wide. Open, the panel is one row no taller
   than the button or the plaque (gd-sdlUIuiXy7), on both. Opening it moves no HUD control: the plaque, the note, Mine/Everyone,
   the sky, Plans, the Porch row and the camera keep their boxes from idle, and the panel covers none of them (on a
   phone it lies over the plaque's right end, the one thing it may cover) (gd-BhcI6pKAmV). A playlist that fails to load leaves a
   disabled button and no panel. Zero page errors. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));
var shots = process.env.GARDENHUB_SHOTS || '';
async function shot(page, name) { if (shots) await page.screenshot({ path: require('path').join(shots, 'tunes-' + name + '.png') }); }

var FIXTURE = { tracks: [
  { id: 't-alpha', kind: 'song', title: 'Alpha Test Tone', src: '/audio/alpha.mp3', href: '/w/t-alpha/', thumb: '/thumbs/t-alpha.svg', duration_s: 61 },
  { id: 't-beta', kind: 'song', title: 'Beta Test Tone', src: '/audio/beta.mp3', href: '/w/t-beta/', thumb: '/thumbs/t-beta.svg', duration_s: 75 },
  { id: 't-gamma', kind: 'song', title: 'Gamma Test Tone', src: '/audio/gamma.mp3', href: '/w/t-gamma/', thumb: '/thumbs/t-gamma.svg', duration_s: 88 },
] };

/* fetch: the hub's playlist.json answers with the fixture (or a 503 when fail), counted in window.__lists; anything else
   goes to the real fetch. */
function stubFetch(o) {
  var real = window.fetch;
  window.__lists = 0;
  window.fetch = function (u, init) {
    if (/^https:\/\/play\.clonk\.tv\/playlist\.json$/.test(String(u))) {
      window.__lists++;
      return Promise.resolve(o.fail ? new Response('down', { status: 503 }) : new Response(JSON.stringify(o.list), { status: 200, headers: { 'Content-Type': 'application/json' } }));
    }
    return real.call(this, u, init);
  };
}

function stubAudio() {
  var proto = HTMLMediaElement.prototype;
  var state = new WeakMap();
  function st(el) { var s = state.get(el); if (!s) { s = { paused: true, t: 0, src: '' }; state.set(el, s); } return s; }
  Object.defineProperty(proto, 'paused', { get: function () { return st(this).paused; }, configurable: true });
  Object.defineProperty(proto, 'ended', { get: function () { return false; }, configurable: true });
  Object.defineProperty(proto, 'duration', { get: function () { return st(this).src ? 100 : NaN; }, configurable: true });
  Object.defineProperty(proto, 'currentTime', {
    get: function () { return st(this).t; },
    set: function (v) { st(this).t = v; this.dispatchEvent(new Event('timeupdate')); },
    configurable: true,
  });
  Object.defineProperty(proto, 'src', {
    get: function () { return st(this).src; },
    set: function (v) { var s = st(this); s.src = String(v); s.t = 0; this.dispatchEvent(new Event('durationchange')); },
    configurable: true,
  });
  proto.load = function () {};
  proto.play = function () { var s = st(this); if (s.paused) { s.paused = false; this.dispatchEvent(new Event('play')); this.dispatchEvent(new Event('playing')); } return Promise.resolve(); };
  proto.pause = function () { var s = st(this); if (!s.paused) { s.paused = true; this.dispatchEvent(new Event('pause')); } };
  window.__tick = function (dt) { var a = window.TUNES.player.audio; a.currentTime = a.currentTime + dt; };
}

/* The HUD's controls, each by its box: what a song starting must not move. */
var HUD = ['.plaque', '#tunes .tn-btn', '#b-mine', '#b-all', '#dw-plans > summary', '.dbtns', '.cam'];
function hud(page) {
  return page.evaluate(function (sels) {
    var out = {};
    sels.forEach(function (sel) {
      var r = document.querySelector(sel).getBoundingClientRect();
      out[sel] = [r.x, r.y, r.width, r.height].map(Math.round);
    });
    return out;
  }, HUD);
}
/* stays(page, idle, plaqueOK) asserts the open panel moved no HUD control from its idle box and covers none of them; plaqueOK
   lets it lie over the plaque (a phone). */
async function stays(page, idle, plaqueOK) {
  var now = await hud(page), pb = await page.locator('#tunes .tn-panel').boundingBox();
  assert.deepStrictEqual(now, idle, 'opening the panel moves no HUD control');
  HUD.forEach(function (sel) {
    var b = now[sel];
    if (!b[2] || (plaqueOK && sel === '.plaque')) return;
    var apart = pb.x >= b[0] + b[2] || b[0] >= pb.x + pb.width || pb.y >= b[1] + b[3] || b[1] >= pb.y + pb.height;
    assert.ok(apart, 'the panel covers no HUD control: ' + sel + ' ' + JSON.stringify([b, pb]));
  });
}

async function open(browser, opts, fail) {
  var ctx = await browser.newContext(Object.assign({ reducedMotion: 'reduce' }, opts));
  await ctx.addInitScript(function () { try { localStorage.setItem('garden.sky', 'day'); } catch (e) { /* the scheme decides */ } });
  await ctx.addInitScript(stubFetch, { list: FIXTURE, fail: Boolean(fail) });
  await ctx.addInitScript(stubAudio);
  var page = await ctx.newPage(), errors = [];
  page.on('pageerror', function (e) { errors.push(String(e)); });
  page.on('console', function (m) { if (m.type() === 'error' && !/EventSource|events|status of 404/.test(m.text())) errors.push(m.text()); });
  await page.goto(url + '/garden');
  await page.waitForSelector('#tunes .tn-btn', { state: 'visible' });
  /* the stream open: the plaque's live words ("connecting", then "live · synced just now") set its width, so the note after it
     moves 59 px when they change; the idle boxes are read once they have (CI run 41181207543 read them before) */
  await page.waitForSelector('#live.on');
  return { ctx: ctx, page: page, errors: errors };
}

async function desktop(browser) {
  var o = await open(browser, { viewport: { width: 1280, height: 800 } }), page = o.page;
  var root = page.locator('#tunes'), btn = root.locator('.tn-btn'), panel = root.locator('.tn-panel');
  assert.strictEqual(await root.evaluate(function (e) { return e.parentElement.className; }), 'crumb', 'the player sits in the HUD beside the plaque');
  var plaque = await page.locator('.plaque').boundingBox(), bb = await btn.boundingBox();
  assert.ok(bb.x >= plaque.x + plaque.width && bb.y < plaque.y + plaque.height, 'the note sits on the plaque\'s row, after it');
  assert.strictEqual(await btn.getAttribute('aria-label'), 'Play music');
  assert.strictEqual(await btn.getAttribute('aria-pressed'), 'false');
  assert.strictEqual(await panel.isVisible(), false, 'idle: no panel');
  assert.strictEqual(await page.evaluate(function () { return window.TUNES.player.audio.src; }), '', 'no song is fetched before a press');
  assert.strictEqual(await page.evaluate(function () { return window.__lists; }), 0, 'nor the playlist: it waits for a press');
  assert.strictEqual(await page.evaluate(function () { return typeof window.TUNES_PLAYLIST; }), 'undefined', 'the page inlines no playlist');
  var idle = await hud(page);
  await shot(page, 'idle');

  await btn.click();
  await panel.waitFor({ state: 'visible' });
  assert.ok(await root.evaluate(function (e) { return e.classList.contains('on'); }));
  assert.strictEqual(await btn.getAttribute('aria-label'), 'Pause music');
  assert.strictEqual(await btn.getAttribute('aria-pressed'), 'true');
  assert.strictEqual(await page.evaluate(function () { return window.__lists; }), 1, 'the press fetched the playlist');
  var first = await page.evaluate(function () { var p = window.TUNES.player; return { i: p.current(), src: p.audio.src, title: p.track().title }; });
  assert.ok(first.i >= 0 && /^https:\/\/play\.clonk\.tv\/audio\/[a-z]+\.mp3$/.test(first.src), 'the song streams from the hub: ' + first.src);
  var pbd = await panel.boundingBox(), bbOn = await btn.boundingBox();
  assert.ok(pbd.height <= bbOn.height && pbd.height <= plaque.height, 'the open panel is one row, no taller than the button or the plaque: ' + JSON.stringify([pbd, bbOn, plaque]));
  await stays(page, idle, false);
  assert.ok(pbd.y >= plaque.y + plaque.height && pbd.x + pbd.width <= plaque.x + plaque.width + 0.5, 'on a desktop the panel hangs under the plaque: ' + JSON.stringify([pbd, plaque]));
  assert.strictEqual((await panel.locator('.tn-title').textContent()).trim(), first.title, 'the panel names the song');
  assert.ok(/^https:\/\/play\.clonk\.tv\/w\//.test(await panel.locator('.tn-title').getAttribute('href')), 'the title links to the song on the hub');
  assert.strictEqual((await panel.locator('.tn-cur').textContent()).trim(), '0:00');
  assert.strictEqual((await panel.locator('.tn-dur').textContent()).trim(), '1:40');
  assert.strictEqual(await panel.locator('.tn-range').getAttribute('aria-label'), 'Seek');
  await shot(page, 'playing');

  await page.evaluate(function () { window.__tick(25); });
  assert.strictEqual((await panel.locator('.tn-cur').textContent()).trim(), '0:25');
  assert.strictEqual(await panel.locator('.tn-range').inputValue(), '250', 'the scrubber follows the song');
  assert.strictEqual(await panel.locator('.tn-range').getAttribute('aria-valuetext'), '0:25 of 1:40');

  await panel.locator('.tn-range').evaluate(function (r) { r.value = '500'; r.dispatchEvent(new Event('input', { bubbles: true })); r.dispatchEvent(new Event('change', { bubbles: true })); });
  assert.strictEqual(await page.evaluate(function () { return window.TUNES.player.audio.currentTime; }), 50, 'the scrubber seeks');
  assert.strictEqual((await panel.locator('.tn-cur').textContent()).trim(), '0:50');

  await panel.locator('.tn-next').click();
  var second = await page.evaluate(function () { var p = window.TUNES.player; return { i: p.current(), id: p.track().id, paused: p.audio.paused, title: p.track().title }; });
  assert.notStrictEqual(second.i, first.i, 'Next moves to another song');
  assert.strictEqual(second.paused, false, 'and keeps playing');
  assert.strictEqual((await panel.locator('.tn-title').textContent()).trim(), second.title);
  assert.strictEqual((await panel.locator('.tn-cur').textContent()).trim(), '0:00');
  assert.strictEqual(await page.evaluate(function () { return window.__lists; }), 1, 'the playlist is fetched once a page');

  await btn.click();
  await panel.waitFor({ state: 'hidden' });
  assert.strictEqual(await btn.getAttribute('aria-label'), 'Play music (' + second.title + ')', 'paused: the button says what a press resumes');
  assert.strictEqual(await btn.getAttribute('aria-pressed'), 'false');
  assert.ok(!(await root.evaluate(function (e) { return e.classList.contains('on'); })));
  await shot(page, 'paused');

  var saved = JSON.parse(await page.evaluate(function () { return sessionStorage.getItem('tunes.player'); }));
  assert.strictEqual(saved.id, second.id, 'the song is remembered');
  assert.strictEqual(saved.playing, false);
  await page.reload();
  await page.waitForSelector('#tunes .tn-btn', { state: 'visible' });
  await page.waitForFunction(function () { return window.TUNES.player.current() >= 0; });
  assert.strictEqual(await page.evaluate(function () { return window.TUNES.player.track().id; }), second.id, 'a reload brings the same song back');
  assert.strictEqual(await panel.isVisible(), false, 'paused before the reload: still folded');

  assert.deepStrictEqual(o.errors, [], 'no page errors');
  await o.ctx.close();
}

/* The hub is down: a press finds no playlist, and the button goes quietly disabled, no panel, no page error. */
async function down(browser) {
  var o = await open(browser, { viewport: { width: 1280, height: 800 } }, true), page = o.page;
  var btn = page.locator('#tunes .tn-btn');
  await btn.click();
  await page.waitForFunction(function () { return document.querySelector('#tunes .tn-btn').disabled; });
  assert.strictEqual(await btn.getAttribute('aria-label'), 'Music unavailable');
  assert.strictEqual(await page.locator('#tunes .tn-panel').isVisible(), false, 'no panel without songs');
  assert.strictEqual(await page.evaluate(function () { return window.TUNES.player.audio.src; }), '', 'and no song');
  assert.strictEqual(await page.evaluate(function () { return window.__lists; }), 1);
  await shot(page, 'down');
  assert.deepStrictEqual(o.errors, [], 'no page errors');
  await o.ctx.close();
}

async function phone(browser) {
  var o = await open(browser, Object.assign({}, pw.devices['Pixel 5'])), page = o.page;
  var btn = page.locator('#tunes .tn-btn'), panel = page.locator('#tunes .tn-panel');
  var box = await btn.boundingBox(), plaque = await page.locator('.plaque').boundingBox();
  assert.ok(box.width >= 44 && box.height >= 44, 'a 44 px target on a phone: ' + JSON.stringify(box));
  assert.ok(box.y < plaque.y + plaque.height, 'on a phone the note shares the title row, not a row of its own: ' + JSON.stringify([box, plaque]));
  var idle = await hud(page);
  await shot(page, 'phone-idle');
  await btn.click();
  await panel.waitFor({ state: 'visible' });
  var pb = await panel.boundingBox();
  await stays(page, idle, true);
  assert.ok(pb.width <= 160, 'the panel stays small on a phone: ' + pb.width);
  assert.ok(pb.height <= 44 && pb.height <= plaque.height, 'the open panel is one row on a phone too, no taller than the button or the plaque: ' + JSON.stringify([pb, plaque]));
  var nb = await panel.locator('.tn-next').boundingBox();
  assert.ok(nb.y >= pb.y && nb.y + nb.height <= pb.y + pb.height + 0.5, 'Next sits inside the panel: ' + JSON.stringify([nb, pb]));
  var vp = page.viewportSize();
  assert.ok(pb.x >= 0 && pb.x + pb.width <= vp.width, 'the panel stays on screen');
  await shot(page, 'phone-playing');
  assert.deepStrictEqual(o.errors, [], 'no page errors');
  await o.ctx.close();
}

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); } catch (e) { console.error(String(e)); process.exit(3); }
  try {
    await desktop(browser);
    await phone(browser);
    await down(browser);
    console.log('tunes: desktop idle/playing/seek/next/pause/reload, phone target and hub-down ok');
  } finally { await browser.close(); }
})().catch(function (e) { console.error(e); process.exit(1); });
