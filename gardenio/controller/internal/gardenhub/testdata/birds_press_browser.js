/* Browser walk for a bird's label as a way to the Glass (gd-jt6OwmV4Jk, bird-captions spec 10), run from
   TestBirdsPressUnderChromium against a live /garden on the fixture farm (cardWorld: the Glass is on, so body data-glass).
   Usage: node birds_press_browser.js <base url> <node_modules dir> [png dir].

   At the Plan level round plan A, with BIRDS's clock pinned past its quiet window, a gardener on plan A is sent a call with a
   caption; its bird is held mid-flight (the clock does not move) with its plate up. Desktop (1280x800): a click on the plate is
   no press (the address keeps no glass=); a mouse held on it lights its ring at 250 ms (BIRDS.pressed) and at 500 ms opens the
   gardener's Glass (?glass=<id>, said in #cursay); with that closed (the mouse let go over it), a right-click on it does at once.
   Phone (Pixel 5, touch): a
   finger held on the plate opens the Glass, and a finger that pans 30 px first does not. Zero page errors throughout. With a
   png dir, each layout's plate is shot as drawn and with its ring lit (<layout>-plate.png, <layout>-plate-lit.png).
   One page load per layout: the Glass opens in the Workbench and closes in place, and the cases that move the camera (the
   click goes into plan A, the pan pans) run last, so the 25 m browser suite pays for no reloads. */
'use strict';
var url = process.argv[2], mods = process.argv[3], out = process.argv[4] || '', assert = require('assert'), path = require('path');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));

async function open(ctx) {
  var page = await ctx.newPage(), errors = [];
  page.on('pageerror', function (e) { errors.push(String(e)); });
  await framed(page);
  return { page: page, errors: errors };
}
/* /garden framed on plan A at the Plan level, the stream open, and the Workbench loaded (it comes after the World, web/lazy.js,
   gd-p02ZelpMxk): the press opens the Glass in it, and a press made while the bundle is still running lights its ring late. */
async function framed(page) {
  await page.goto(url + '/garden?at=gardenio/gd-planA');
  await page.waitForSelector('#cursor-bar .cb-me', { state: 'attached' });
  await page.evaluate(function () { return new Promise(function (r) { setTimeout(r, 0); }); });   // after the cursor's signs and framing, a setTimeout(0) task of their own (gd-fsuxCGGbZr)
  await page.waitForFunction(function () { return !window.LAZY || LAZY.loaded('workbench'); });
  var lv = await page.evaluate(function () { return window.GARDEN.cam().level; });
  /* at the Plan level on plan A's middle: the Project level frames the region, whose middle is no longer plan A's plot once the
     farmstead holds slot 0 (gd-8cz6uje2WZ) */
  if (lv !== 2) await page.evaluate(async function () {
    var doc = await (await fetch('/garden/farm', { headers: { Accept: 'application/json' } })).json(), memo = null, T = WORLD.TILE;
    try { memo = JSON.parse(localStorage.getItem('garden.world.v1')); } catch (e) { /* no memo: a fresh layout */ }
    var a = WORLD.layout(doc, memo, { readonly: true }).index['gd-planA'];
    window.GARDEN.look({ cx: (a.x + a.w / 2) * T, cy: (a.y + a.h / 2) * T, z: 0.82 });
  });
  await page.waitForFunction(function () { return window.GARDEN.cam().level === 2 && window.BIRDS.opened() > 0; });
}
/* A call for a gardener on plan A with its plate up on its head: {id, x, y (the plate's middle), box (its rect), page px}. */
async function plate(page) {
  var p = await page.evaluate(async function () {
    var doc = await (await fetch('/garden/farm', { headers: { Accept: 'application/json' } })).json(), B = window.BIRDS;
    var ids = (doc.gardeners || []).filter(function (g) { return g.plot && g.plot.plan === 'gd-planA'; }).map(function (g) { return g.id; });
    var frame = function () { return new Promise(function (r) { requestAnimationFrame(function () { requestAnimationFrame(r); }); }); };
    B.motion(true); B.pin(B.opened() + B.QUIET + 1000);
    /* one gardener at a time until a plate is up: the plate stands on the head (gd-CykqEHeEpX), and one under the plan's label
       between two neighbours has no clear spot */
    for (var i = 0; i < ids.length && !B.tags().length; i++) {
      document.dispatchEvent(new CustomEvent('farmactivity', { detail: { id: ids[i], doing: 'read', tool: 'Read', at: 1, dur: 0, cap: 'Read web/birds.js' } }));
      for (var k = 0; k < 20 && !B.tags().length; k++) await frame();
    }
    /* and still: a plate is placed each frame, and a sign refresh after the call can still move it,
       so the press is aimed where it rests, three frames alike with its alpha up (a press flake on CI, gd-y1XrMhKH0D; lit says why) */
    var key = function () { var t = B.tags()[0]; return t ? [t.gid, t.x, t.y, t.w, t.h, t.a > 0].join() : ''; };
    for (var n = 0, last = key(), same = 0; n < 30 && same < 3; n++) { await frame(); var now = key(); same = now === last ? same + 1 : 0; last = now; }
    var t = B.tags()[0], s = B.scale(), bd = document.getElementById('board').getBoundingClientRect();
    if (!t) return { ids: ids };
    return { id: t.gid, ids: ids, x: bd.left + (t.x + t.w / 2) / s, y: bd.top + (t.y + t.h / 2) / s, box: { x: bd.left + t.x / s, y: bd.top + t.y / s, w: t.w / s, h: t.h / s } };
  });
  assert.ok(p.id, 'a plate up for a gardener on plan A: ' + JSON.stringify(p));
  return p;
}
/* The Glass the press opened, closed in place (the address drops glass=); returns p's plate where it is drawn now. */
async function unglass(page, p) {
  var q = await page.evaluate(function (id) {
    window.WORKBENCH.close();
    var t = window.BIRDS.tags().filter(function (t) { return t.gid === id; })[0], s = window.BIRDS.scale(), bd = document.getElementById('board').getBoundingClientRect();
    return t && { id: t.gid, ids: [], x: bd.left + (t.x + t.w / 2) / s, y: bd.top + (t.y + t.h / 2) / s, box: { x: bd.left + t.x / s, y: bd.top + t.y / s, w: t.w / s, h: t.h / s } };
  }, p.id);
  assert.strictEqual(await glassOf(page), null, 'the Glass closed');
  assert.ok(q, 'the plate still up once the Glass closed');
  return q;
}
/* arm(page, p): before a press on p, the page itself notes when the press went down, when p's ring lit (BIRDS.pressed) and when
   it went out, every 4 ms; with a png dir it reads the lit plate off the canvas the frame after (a page screenshot can take longer
   than the 250 ms left before the Glass opens over it). Noted in the page, not waited for from node: on a busy CI runner the ring lit, but past a
   450 ms wait from node or between its rAF polls (gd-eP5cSjcJvd). */
function arm(page, p) {
  return page.evaluate(function (a) {
    var B = window.BIRDS, w = window.__press = { down: 0, lit: 0, off: 0, png: '' }, b = a.box;
    document.addEventListener('pointerdown', function d() { w.down = performance.now(); document.removeEventListener('pointerdown', d, true); }, true);
    var iv = setInterval(function () {
      if (w.lit) { if (B.pressed() !== a.id) { w.off = performance.now(); clearInterval(iv); } return; }
      if (B.pressed() !== a.id) return;
      w.lit = performance.now();
      if (a.shoot) requestAnimationFrame(function () { requestAnimationFrame(function () {
        var cv = document.getElementById('cv'), bd = document.getElementById('board').getBoundingClientRect(), k = cv.width / bd.width, m = 24;
        var c = document.createElement('canvas'), x = (b.x - bd.left - m) * k, y = (b.y - bd.top - m) * k;
        c.width = Math.round((b.w + 2 * m) * k); c.height = Math.round((b.h + 2 * m) * k);
        c.getContext('2d').drawImage(cv, x, y, c.width, c.height, 0, 0, c.width, c.height);
        w.png = c.toDataURL('image/png').split(',')[1];
      }); });
    }, 4);
  }, { id: p.id, box: p.box, shoot: !!out });
}
/* lit(page, p, name): the armed press on p lit its ring no sooner than PRESS_LIT after it went down (and so before its Glass
   opened, which un-lights it); else the failure names what the page had under the pointer then (the plate's box, its alpha, the
   camera, the element hit), so a miss says why. With a png dir, <name>.png is the plate as lit. */
async function lit(page, p, name) {
  try { await page.waitForFunction(function () { return window.__press.lit > 0; }, null, { polling: 50, timeout: 3000 }); } catch (e) {
    var now = await page.evaluate(function (q) {
      var B = window.BIRDS, s = B.scale(), bd = document.getElementById('board').getBoundingClientRect(), c = window.GARDEN.cam(), hit = document.elementFromPoint(q.x, q.y);
      var t = B.tags().filter(function (t) { return t.gid === q.id; })[0];
      return { pressed: B.pressed(), press: window.__press, level: c.level, cam: [c.cam.cx, c.cam.cy, c.z], plate: t && { x: bd.left + t.x / s, y: bd.top + t.y / s, w: t.w / s, h: t.h / s, a: t.a }, under: hit && (hit.id || hit.className) };
    }, p);
    throw new Error('a press on ' + p.id + '\'s plate lit no ring: ' + JSON.stringify({ at: [p.x, p.y], box: p.box, now: now }) + '\n' + e.message);
  }
  var w = await page.evaluate(function () { return { down: window.__press.down, lit: window.__press.lit, at: window.BIRDS.PRESS_LIT }; });
  assert.ok(w.down > 0 && w.lit - w.down >= w.at - 20, 'the ring lit at PRESS_LIT, not at once: ' + JSON.stringify(w));
  if (!out) return;
  await page.waitForFunction(function () { return !!window.__press.png; }, null, { polling: 50, timeout: 3000 });
  require('fs').writeFileSync(path.join(out, name + '.png'), Buffer.from(await page.evaluate(function () { return window.__press.png; }), 'base64'));
}
/* glassed(page, p): the press on p opened its Glass (?glass=<id>) within PRESS_MS plus CI headroom (a busy runner slips the
   press timer, gd-c85rWBzO2F); else the failure names the address, BIRDS.pressed(), the level, the camera, #cursay and the
   armed press's times (off: when its ring went out, at the press firing or at a drop), so a miss says whether it fired. */
async function glassed(page, p) {
  var ms = await page.evaluate(function () { return window.BIRDS.PRESS_MS; }) + 4000;
  try { await page.waitForFunction(function (id) { return new URL(location.href).searchParams.get('glass') === id; }, p.id, { timeout: ms }); } catch (e) {
    var now = await page.evaluate(function () {
      var c = window.GARDEN.cam(), say = document.getElementById('cursay');
      return { url: location.href, pressed: window.BIRDS.pressed(), level: c.level, cam: [c.cam.cx, c.cam.cy, c.z], said: say && say.textContent, press: window.__press && { down: window.__press.down, lit: window.__press.lit, off: window.__press.off }, t: performance.now() };
    });
    throw new Error('a press on ' + p.id + '\'s plate opened no Glass in ' + ms + ' ms: ' + JSON.stringify(now) + '\n' + e.message);
  }
}
function glassOf(page) { return page.evaluate(function () { return new URL(location.href).searchParams.get('glass'); }); }
async function shot(page, p, name) {
  if (!out) return;
  var m = 24, b = p.box;
  await page.screenshot({ path: path.join(out, name + '.png'), clip: { x: Math.max(0, b.x - m), y: Math.max(0, b.y - m), width: b.w + 2 * m, height: b.h + 2 * m } });
}

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); } catch (e) { console.log(String(e).split('\n')[0]); process.exit(3); }
  try {
    // desktop: held, the plate's ring lights and then the Glass opens; a right-click opens it at once; a click is no press
    var ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } }), o = await open(ctx), page = o.page, p = await plate(page);
    await shot(page, p, 'desktop-plate');
    await arm(page, p); await page.mouse.move(p.x, p.y); await page.mouse.down();
    await lit(page, p, 'desktop-plate-lit');
    await glassed(page, p);
    await page.mouse.up();
    var said = await page.evaluate(function () { return document.getElementById('cursay').textContent; });
    assert.ok(/^Watching .+ in the Glass$/.test(said), 'said in #cursay: ' + said);
    p = await unglass(page, p);
    await page.mouse.click(p.x, p.y, { button: 'right' });
    await glassed(page, p);
    p = await unglass(page, p);
    await page.mouse.click(p.x, p.y);   // last: the click goes into plan A, the camera with it
    await page.waitForTimeout(700);
    assert.strictEqual(await glassOf(page), null, 'a click on the plate is the cursor\'s, not the Glass');
    assert.deepStrictEqual(o.errors, [], 'desktop: no page errors');
    await ctx.close();

    // phone: a finger held on the plate opens the Glass; one that pans first does not
    ctx = await browser.newContext(pw.devices['Pixel 5']); o = await open(ctx); page = o.page; p = await plate(page);
    await shot(page, p, 'phone-plate');
    var cdp = await ctx.newCDPSession(page), touch = function (type, x, y) { return cdp.send('Input.dispatchTouchEvent', { type: type, touchPoints: type === 'touchEnd' ? [] : [{ x: x, y: y }] }); };
    await arm(page, p); await touch('touchStart', p.x, p.y);
    await lit(page, p, 'phone-plate-lit');
    await glassed(page, p);
    await touch('touchEnd');
    p = await unglass(page, p);
    await touch('touchStart', p.x, p.y);   // last: the pan moves the camera
    await touch('touchMove', p.x + 15, p.y + 15);
    await page.waitForTimeout(700);
    await touch('touchEnd');
    await page.waitForTimeout(200);
    assert.strictEqual(await glassOf(page), null, 'a finger that moved 30 px is a pan, not a press');
    assert.deepStrictEqual(o.errors, [], 'phone: no page errors');
    await ctx.close();
    console.log('birds press: desktop click/hold/right-click and phone hold/pan checked');
  } finally {
    await browser.close();
  }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
