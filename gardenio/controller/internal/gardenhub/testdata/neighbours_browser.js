/* Browser check for operator fences and Mine (gd-MFcv2hSfue), run from TestNeighboursUnderChromium against a live /garden on
   an httptest server with the card fixture farm seen by oidc:matt@example.com, with four operators and one owner-only plan.
   Usage: node neighbours_browser.js <base url> <node_modules dir> <now, unix ms>. GARDENHUB_SHOTS=<dir> saves the screenshots.
   Date.now() is fixed at <now> (the farm's clock, goldenNow) while timers still run, so the fixture never ages into fog.

   Desktop (1440x900) and phone (390x844), light and dark: Mine is on by default for a signed-in viewer; matt's plot stays lit
   while flo's is dimmed; Everyone lifts the dimming; the choice survives a reload (localStorage garden.scope). Each plot's
   fence carries its operator's colour; the legend lists every operator, the viewer's first and marked "(you)", each with its
   colour and pattern in words; the plot link's accessible name says the operator by name. Zero console
   errors. */
'use strict';
var url = process.argv[2], mods = process.argv[3], now = +process.argv[4], assert = require('assert'), path = require('path');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));
var PLOT = '#hots a.w-plot', MINE = PLOT + '[href="/garden?bead=gd-planA"]', FLO = PLOT + '[href="/garden?bead=gd-kbeadsa"]';

/* Every context runs at the farm's clock (now) and pins the sky (web/sky.js, gd-SwRqFcjWc6) to its emulated colour scheme:
   light is day and dark is night whatever the clock says, so a run at night (CI is UTC) sees the page a run by day does. */
function skyCtx(browser, opts) {
  opts = opts || {};
  return browser.newContext(opts).then(async function (ctx) {
    await ctx.clock.setFixedTime(now);
    return ctx.addInitScript(function (s) { try { localStorage.setItem('garden.sky', s); } catch (e) { /* no storage: the emulated scheme still decides by day */ } }, opts.colorScheme === 'dark' ? 'night' : 'day').then(function () { return ctx; });
  });
}

async function shot(page, name) { if (process.env.GARDENHUB_SHOTS) await page.screenshot({ path: path.join(process.env.GARDENHUB_SHOTS, 'neighbours-' + name + '.png') }); }
async function frames(page) { await page.evaluate(function () { return new Promise(function (r) { requestAnimationFrame(function () { requestAnimationFrame(r); }); }); }); }
async function open(ctx) {
  var page = await ctx.newPage(), errors = [];
  page.on('console', function (m) { if (m.type() === 'error') errors.push(m.text()); });
  page.on('pageerror', function (e) { errors.push(String(e)); });
  await page.goto(url + '/garden');
  await page.waitForFunction(function () { return window.GARDEN && window.GARDEN.known('gd-kbeadsa').onMap; });
  await to(page, FLO);
  return { page: page, errors: errors };
}
/* The cursor onto a plot (a cursor jump, cursor-grid spec 4.3): its project's plots are the grid's links, framed on their used
   bounds, at the Project level or (a small project) the Plan level, where fences and the Mine dimming are drawn. The world
   can be laid out (known().onMap) before the cursor opens, and until it does GARDEN.fly is false and moves nothing, so the
   jump is retried until it lands (gd-RiSAxqkdHY). */
async function to(page, sel) {
  await page.waitForFunction(function (id) { return window.GARDEN.fly(id); }, sel.match(/bead=([^"]+)/)[1]);
  await page.waitForSelector(sel, { state: 'attached' });
  await page.waitForFunction(function () { var l = window.GARDEN && window.GARDEN.cam().level; return l === 1 || l === 2; });
  await frames(page);
}
/* The board pixels under a plot link: mean luminance of its middle and the colours along its left edge band. */
async function sample(page, sel) {
  await to(page, sel);
  var b = await page.locator(sel).boundingBox();
  assert.ok(b, sel + ' has no box');
  return page.evaluate(function (b) {
    var cv = document.getElementById('cv'), r = cv.getBoundingClientRect(), k = cv.width / r.width, x = cv.getContext('2d');
    var px = function (sx, sy, w, h) { return x.getImageData(Math.round((sx - r.left) * k), Math.round((sy - r.top) * k), Math.max(1, Math.round(w * k)), Math.max(1, Math.round(h * k))).data; };
    var mid = px(b.x + b.width / 4, b.y + b.height / 3, b.width / 2, b.height / 3), sum = 0, n = 0;
    for (var i = 0; i < mid.length; i += 4) { sum += 0.2126 * mid[i] + 0.7152 * mid[i + 1] + 0.0722 * mid[i + 2]; n++; }
    var edge = px(b.x + 1, b.y, 3, b.height), cols = [];
    for (var j = 0; j < edge.length; j += 4) cols.push([edge[j], edge[j + 1], edge[j + 2]]);
    return { lum: sum / n, edge: cols };
  }, b);
}
function hasColour(cols, hex) {
  var t = [parseInt(hex.substr(1, 2), 16), parseInt(hex.substr(3, 2), 16), parseInt(hex.substr(5, 2), 16)];
  return cols.some(function (c) { return Math.abs(c[0] - t[0]) + Math.abs(c[1] - t[1]) + Math.abs(c[2] - t[2]) < 40; });
}
async function styles(page) {
  return page.evaluate(async function () {
    var doc = await (await fetch('/garden/farm', { headers: { Accept: 'application/json' } })).json();
    return NEIGHBOURS.assign(doc);
  });
}

async function walk(browser, name, opts) {
  var ctx = await skyCtx(browser, Object.assign({ reducedMotion: 'reduce' }, opts)), o = await open(ctx), page = o.page;
  var st = await styles(page), matt = st['oidc:matt@example.com'], flo = st['oidc:flo@example.com'];
  assert.deepStrictEqual(Object.keys(st).sort(), ['ivy', 'oidc:flo@example.com', 'oidc:matt@example.com', 'oidc:mo@example.com', 'oidc:sam@example.com'], name + ': five operators');

  // Mine is on by default for a signed-in viewer: the viewer's plot is lit, flo's dimmed
  assert.strictEqual(await page.getAttribute('#b-mine', 'aria-pressed'), 'true', name + ': Mine is on');
  var mineOn = await sample(page, MINE), floOn = await sample(page, FLO);
  await shot(page, name + '-mine');
  await page.click('#b-all');
  await frames(page);
  var mineAll = await sample(page, MINE), floAll = await sample(page, FLO);
  await shot(page, name + '-everyone');
  assert.ok(floOn.lum < floAll.lum * 0.7, name + ': flo\'s plot is dimmed under Mine: ' + floOn.lum.toFixed(1) + ' vs ' + floAll.lum.toFixed(1));
  assert.ok(Math.abs(mineOn.lum - mineAll.lum) < 3, name + ': the viewer\'s plot is lit either way: ' + mineOn.lum.toFixed(1) + ' vs ' + mineAll.lum.toFixed(1));
  assert.ok(hasColour(mineAll.edge, matt.colour), name + ': matt\'s fence is ' + matt.colourName);
  assert.ok(hasColour(floAll.edge, flo.colour), name + ': flo\'s fence is ' + flo.colourName);
  assert.ok(!hasColour(floAll.edge, matt.colour) || matt.colour === flo.colour, name + ': flo\'s fence is not matt\'s colour');

  // the choice persists
  await page.reload();
  await page.waitForFunction(function () { return window.GARDEN && window.GARDEN.known('gd-kbeadsa').onMap; });
  await to(page, FLO);
  assert.strictEqual(await page.getAttribute('#b-all', 'aria-pressed'), 'true', name + ': Everyone survives a reload');
  await page.click('#b-mine');
  await frames(page);

  // identity in words: the plot link's accessible name (the info card that also said it is gone, cursor-grid spec N-D14)
  assert.ok((await page.getAttribute(FLO, 'aria-label')).indexOf('Operator: flo, ' + flo.colourName + ' ' + flo.pattern + ' fence') >= 0, name + ': the link names the operator');
  await to(page, MINE);
  assert.ok((await page.getAttribute(MINE, 'aria-label')).indexOf('Operator: matt (you), ') >= 0, name + ': the viewer\'s link says (you)');

  // the legend lists every operator, the viewer first
  await page.click('#cam-menu'); await page.click('#cam-help');   // Legend & keys is in the camera's Menu (3b R9)
  await page.locator('#legend-ops').waitFor({ state: 'visible' });
  var rows = await page.$$eval('#legend-ops li', function (l) { return l.map(function (e) { return e.textContent; }); });
  assert.strictEqual(rows.length, 5, name + ': five legend rows: ' + rows);
  assert.strictEqual(rows[0], 'matt (you), ' + matt.colourName + ' ' + matt.pattern + ' fence', name + ': yours first');
  assert.ok(rows.every(function (r) { return / fence$/.test(r); }), name + ': each row says colour and pattern: ' + rows);
  assert.strictEqual(await page.$$eval('#legend-ops canvas.fence', function (l) { return l.length; }), 5, name + ': a swatch per row');
  await page.locator('#legend-ops').scrollIntoViewIfNeeded();
  await shot(page, name + '-legend');

  // inside the plan: the band sits on the fence rail of the plot's sprite tiles
  await page.goto(url + '/garden?at=kbeads/gd-kbeadsa');
  await page.waitForSelector(FLO, { state: 'attached' });
  await page.keyboard.press('Enter');   // into the plan: its phases framed, at the Plan level or closer
  await page.waitForFunction(function () { return window.GARDEN && window.GARDEN.cam().level >= 2; });
  await frames(page);
  await shot(page, name + '-bead');

  assert.deepStrictEqual(o.errors.filter(function (e) { return !/404/.test(e); }), [], name + ': console errors');
  console.log(name + ': ok (' + rows.join(' | ') + ')');
  await ctx.close();
}

(async function () {
  var browser;
  assert.ok(url && mods && now > 0, 'usage: node neighbours_browser.js <base url> <node_modules> <now ms>');
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  try {
    await walk(browser, 'desktop-light', { viewport: { width: 1440, height: 900 } });
    await walk(browser, 'desktop-dark', { viewport: { width: 1440, height: 900 }, colorScheme: 'dark' });
    await walk(browser, 'phone-light', Object.assign({}, pw.devices['iPhone 12'], { viewport: { width: 390, height: 844 } }));
    await walk(browser, 'phone-dark', Object.assign({}, pw.devices['iPhone 12'], { viewport: { width: 390, height: 844 }, colorScheme: 'dark' }));
    console.log('neighbours browser checks passed');
  } finally { await browser.close(); }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
