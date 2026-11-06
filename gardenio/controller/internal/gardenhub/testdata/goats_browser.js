/* Browser walk for the goats' ghost and solid, hover and click (gd-2cPnm71cxT, step 2.3 of plan gd-EGpbDpqsec), run from
   TestGoatsUnderChromium against a live /garden on the fixture farm (wishes gd-wish1, tended, with kin on plan A; gd-wish2,
   untended, with none, in its project's pen). Usage: node goats_browser.js <base url> <node_modules dir>.

   Desktop: at the Plan level round plan A, with the goats' clock pinned, a mouse over gd-wish1's goat shows the tip naming the
   wish, who asked and that it is tended, and off it the tip goes; a click on it asks the Commons for its thread
   (GET /garden/c/bead/gd-wish1, CHAT.open) and, with none there, opens the wish's bead view. The fixture's untended wish is a
   ghost and the tended one is not. Phone (Pixel 5): the first tap on the goat names it (the tip stays), the second asks for
   its thread. A project's pen (gd-kVKuWqIK1v, homestead-world-design.md 28.2): framed on it, the untended wish with no kin on
   the map stands in the pen, a ghost, and gd-wish1's open gate says it is out.
   A mouse over that gate names the wish, out grazing,
   and its card's "Zoom to it on the map" (gd-xlRpUpTjPZ) takes the cursor into
   plan A, where the goat grazes: the page's ?at= becomes gardenio/gd-planA, the Plan level (or closer) holds and the goat is
   drawn. Zero page errors throughout. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));

async function open(ctx) {
  var page = await ctx.newPage(), errors = [];
  page.on('pageerror', function (e) { errors.push(String(e)); });
  await page.route('**/garden/c/bead/gd-wish1', function (r) { return r.fulfill({ status: 404, body: 'no thread' }); });   // no Commons here: CHAT.open falls back to the bead view
  await page.goto(url + '/garden?at=gardenio/gd-planA');
  await page.waitForSelector('#cursor-bar .cb-me', { state: 'attached' });
  await page.evaluate(function () { return new Promise(function (r) { setTimeout(r, 0); }); });   // after the cursor's signs and framing, a setTimeout(0) task of their own (gd-fsuxCGGbZr)
  var lv = await page.evaluate(function () { return window.GARDEN.cam().level; });
  if (lv !== 2) { await page.evaluate(function () { window.GARDEN.look({ z: 0.82 }); }); }
  await page.waitForFunction(function () { return window.GARDEN.cam().level === 2; });
  return { page: page, errors: errors };
}
/* gd-wish1's goat, pinned at a moment it stands on bare map (the canvas is what the point hits, not a sign or a link): its
   centre in page px. The board is centred on plan A, then on the goat: its ring's west, north and south sides are the
   plot's verge tiles, under its link, so it is on bare map only on the east side, in the gap, which the cursor's framing
   (and a phone's width) can leave off the board (gd-RgOXTnVbPC). */
async function goat(page) {
  var at = await page.evaluate(async function () {
    var G = window.GOATS, frame = function () { return new Promise(function (r) { requestAnimationFrame(function () { requestAnimationFrame(r); }); }); };
    var bd = document.getElementById('board').getBoundingClientRect();
    var T = window.WORLD.TILE, p = window.GARDEN.cam().w.index['gd-planA'], mine = function () { return G.list().filter(function (x) { return x.id === 'gd-wish1' && !x.out; })[0]; };   // its goat, not its open gate in the pen
    for (var k = 0; k < 40; k++) {
      G.pin(1792000000000 + k * G.PER + 1000);   // early in a spell: grazing, not walking
      window.GARDEN.look({ cx: (p.x + p.w / 2) * T, cy: (p.y + p.h / 2) * T });
      await frame();
      var g = mine();
      if (!g) continue;
      window.GARDEN.look({ cx: g.x, cy: g.y });
      await frame();
      if (!(g = mine())) continue;
      var x = bd.left + g.css.x + g.css.w / 2, y = bd.top + g.css.y + g.css.h / 2, e = document.elementFromPoint(x, y);
      if (e && e.id === 'cv') return { x: x, y: y, ghosts: G.list().map(function (q) { return q.id + ':' + q.ghost; }) };
    }
    return null;
  });
  assert.ok(at, "gd-wish1's goat on bare map at the Plan level round plan A");
  return at;
}
/* A point on bare map well clear of the goat at g (page px): off it, with no link under the mouse (a staked plot's link wears a
   tip of its own). */
async function bare(page, g) {
  var at = await page.evaluate(function (g) {
    var bd = document.getElementById('board').getBoundingClientRect();
    for (var y = bd.top + 8; y < bd.bottom; y += 16) for (var x = bd.left + 8; x < bd.right; x += 16) {
      var e = document.elementFromPoint(x, y);
      if (e && e.id === 'cv' && Math.hypot(x - g.x, y - g.y) > 120) return { x: x, y: y };
    }
    return null;
  }, g);
  assert.ok(at, 'a point on bare map');
  return at;
}
async function tipText(page) { return page.evaluate(function () { var t = document.getElementById('stip'); return t && !t.hidden ? t.textContent : ''; }); }

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); } catch (e) { console.log(String(e).split('\n')[0]); process.exit(3); }
  try {
    // desktop: hover names it, off it the tip goes, a click asks for its thread
    var ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } }), o = await open(ctx), page = o.page;
    var g = await goat(page);
    assert.ok(g.ghosts.indexOf('gd-wish1:false') >= 0, 'the tended wish is solid: ' + g.ghosts);
    g.ghosts.forEach(function (s) { if (s.indexOf('gd-wish2:') === 0) assert.strictEqual(s, 'gd-wish2:true', 'the untended wish is a ghost'); });
    await page.mouse.move(g.x, g.y);
    await page.waitForFunction(function () { var t = document.getElementById('stip'); return t && !t.hidden; });
    var text = await tipText(page);
    ['exchange behind the edge', 'Asked by matt', 'Tended', 'Thread in the Commons'].forEach(function (w) { assert.ok(text.indexOf(w) >= 0, 'the tip says ' + w + ': ' + text); });
    var off = await bare(page, g);
    await page.mouse.move(off.x, off.y);
    await page.waitForFunction(function () { return document.getElementById('stip').hidden; });
    await page.mouse.move(g.x, g.y);
    await page.waitForFunction(function () { return !document.getElementById('stip').hidden; });
    var asked = page.waitForRequest(function (r) { return new URL(r.url()).pathname === '/garden/c/bead/gd-wish1'; }, { timeout: 5000 });
    await page.mouse.click(g.x, g.y);
    await asked;
    await page.waitForFunction(function () { return new URL(location.href).searchParams.get('bead') === 'gd-wish1'; });
    assert.deepStrictEqual(o.errors, [], 'no page errors on the desktop');
    await ctx.close();

    // phone: the first tap names it and the tip stays; the second asks for its thread
    var dev = pw.devices['Pixel 5'];
    ctx = await browser.newContext(dev); o = await open(ctx); page = o.page;
    g = await goat(page);
    var n = 0;
    page.on('request', function (r) { if (new URL(r.url()).pathname === '/garden/c/bead/gd-wish1') n++; });
    await page.touchscreen.tap(g.x, g.y);
    await page.waitForFunction(function () { var t = document.getElementById('stip'); return t && !t.hidden; });
    assert.ok((await tipText(page)).indexOf('exchange behind the edge') >= 0, 'a tap names the wish');
    await page.waitForTimeout(400);
    assert.strictEqual(n, 0, 'the first tap only names it');
    assert.ok(!(await page.evaluate(function () { return document.getElementById('stip').hidden; })), 'the tip stays');
    asked = page.waitForRequest(function (r) { return new URL(r.url()).pathname === '/garden/c/bead/gd-wish1'; }, { timeout: 5000 });
    await page.touchscreen.tap(g.x, g.y);
    await asked;
    assert.deepStrictEqual(o.errors, [], 'no page errors on the phone');
    await ctx.close();

    // a project's pen (gd-kVKuWqIK1v; homestead-world-design.md 28.2): framed on the Pen cell of the strip holding gd-wish1, the
    // kinless wish's goat stands in it, a ghost, and gd-wish1, out grazing by plan A, has its open gate there
    ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } });
    page = await ctx.newPage(); var errs = [];
    page.on('pageerror', function (e) { errs.push(String(e)); });
    await page.goto(url + '/garden?at=gardenio/gd-planA');   // the cursor inside the project, so its pen is open ground, not under the project's cell
    await page.waitForFunction(function () { return window.GARDEN && window.GARDEN.cam(); });
    var penAt = await page.evaluate(async function () {
      var doc = await (await fetch('/garden/farm')).json(), bd = document.getElementById('board').getBoundingClientRect();
      var w = WORLD.layout(doc, null, { readonly: true }), pen = w.biomes.map(function (b) { return b.pen; }).filter(function (p) { return p.items.some(function (it) { return it.id === 'wish:gd-wish1'; }); })[0];
      var frame = function () { return new Promise(function (r) { requestAnimationFrame(function () { requestAnimationFrame(r); }); }); };
      window.GARDEN.look({ z: 1.5, on: [pen.id] }); await frame();
      GOATS.pin(1792000000000 + 1000); window.GARDEN.look({}); await frame();
      var gate = GOATS.list().filter(function (q) { return q.id === 'gd-wish1' && q.out; })[0];
      if (gate) { window.GARDEN.look({ cx: gate.x, cy: gate.y }); await frame(); gate = GOATS.list().filter(function (q) { return q.id === 'gd-wish1' && q.out; })[0]; }   // the gate under the board's middle, clear of the HUD's buttons
      var gp = gate && document.elementFromPoint(bd.left + gate.css.x + gate.css.w / 2, bd.top + gate.css.y + gate.css.h / 2);
      return { level: window.GARDEN.cam().level, pen: pen.items.map(function (it) { return it.id + ':' + it.out; }),
        goats: GOATS.list().map(function (q) { return q.id + ':' + q.plot + ':' + q.ghost + (q.out ? ':out' : ''); }),
        gate: gate ? { x: bd.left + gate.css.x + gate.css.w / 2, y: bd.top + gate.css.y + gate.css.h / 2, under: gp ? gp.id || gp.tagName + '.' + gp.className : '' } : null };
    });
    assert.ok(penAt.level >= 2, 'goats are drawn from the Plan level in: ' + penAt.level);
    assert.deepStrictEqual(penAt.pen, ['wish:gd-wish1:true', 'wish:gd-wish2:false'], "every open wish in its project's pen, oldest first (gd-wDdun2AVwq); gd-wish1 out");
    assert.ok(penAt.goats.indexOf('gd-wish2:pen:true') >= 0, 'the kinless wish in the pen, a ghost: ' + penAt.goats);
    assert.ok(penAt.goats.indexOf('gd-wish1:pen:false:out') >= 0, "gd-wish1's open gate in the pen: " + penAt.goats);
    // the gate's card is the way to the goat: a hover names the wish, out grazing; "Zoom to it on the map" is a cursor jump to plan A
    assert.ok(penAt.gate && /^cv$|w-home/.test(penAt.gate.under), "gd-wish1's gate on open ground or its home cell, which spans the pen (under: " + (penAt.gate && penAt.gate.under) + ')');
    await page.mouse.move(penAt.gate.x, penAt.gate.y);
    await page.waitForFunction(function () { var t = document.getElementById('stip'); return t && !t.hidden; });
    var gt = await tipText(page);
    ['exchange behind the edge', 'out grazing by its plan', 'Thread in the Commons', 'Zoom to it on the map'].forEach(function (w) { assert.ok(gt.indexOf(w) >= 0, 'the gate names the wish, out, with the jump: ' + gt); });
    assert.strictEqual(await page.getAttribute('#stip a.ggoto', 'href'), '/garden?at=gardenio/gd-planA', "the link's own href, for a load without script");
    await page.click('#stip a.ggoto');
    await page.waitForFunction(function () { return /^gardenio\/gd-(planA|A1)/.test(new URL(location.href).searchParams.get('at') || ''); });   // inside plan A: the plot, or its first phase
    assert.strictEqual(await page.evaluate(function () { return window.GARDEN.here(); }), 'gardenio', 'the cursor is on plan A in gardenio');
    await page.waitForFunction(function () { var c = window.GARDEN.cam(); return c.level >= 2 && !c.anim; }, null, { timeout: 5000 });   // the fly has landed inside plan A, at the Plan level or closer, where the goat is drawn: look({}) below cuts a fly short where it is (gd-RgOXTnVbPC)
    var goatThere = await page.evaluate(async function () {
      var frame = function () { return new Promise(function (r) { requestAnimationFrame(function () { requestAnimationFrame(r); }); }); };
      window.GARDEN.look({}); await frame();
      return GOATS.list().filter(function (q) { return q.id === 'gd-wish1' && !q.out; }).length;
    });
    assert.strictEqual(goatThere, 1, "gd-wish1's goat is drawn where the jump landed");
    assert.ok(await page.evaluate(function () { return document.getElementById('stip').hidden; }), 'the card went with the jump');
    assert.deepStrictEqual(errs, [], 'no page errors at the pen');
    await ctx.close();
    console.log('goats browser checks passed');
  } finally { await browser.close(); }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
