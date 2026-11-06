/* Browser walk for the camera across a re-layout that widens a farm (homestead-world-design.md 29.11, X-D14; step 15
   gd-gFMObwPAzs), run from TestCameraGrowsUnderChromium against a live /garden on the fixture farm, whose projects stand in
   lattice columns 1 (gardenio) to 4 (music). Usage: node camgrow_browser.js <base url> <node_modules dir>.

   The cursor on coop's farmstead (column 3), framed. A document that grows coop's farm flies to the new framing, which holds the
   whole wider farm. With a pointer down on the board, one that grows it again moves no camera: coop's farm corner, the anchor,
   did not move, and a gesture gets no reframe. After that pan, one that widens kbeads's farm (column 2, west of coop) moves
   coop east in the world, and coop's farm stays at the same screen px. Each document is the page's own with escalations added,
   dispatched as a farmdoc; the server's later documents are held off. Zero page errors throughout. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));

/* The page's farm document with n open escalations added to project key and mail letters past them (the farm counts both among its
   letters), handed on
   as the next farm document; the camera's state once any fly has landed: coop's farm in world tiles and screen px. */
async function grow(page, key, n, mail) {
  return page.evaluate(async function (a) {
    var d = JSON.parse(JSON.stringify(GARDEN.farm())), es = d.escalations = d.escalations || {}, h = d.home = d.home || {};
    es.open = (es.open || []).filter(function (e) { return e.project !== a.key; });
    for (var i = 0; i < a.n; i++) es.open.push({ id: 'gd-cam' + a.key + i, title: 'Merge ' + i, kind: 'escalation', state: 'pending', human: true, since: i + 1, project: a.key, family: 'merge' });
    if (es.by_project) es.by_project[a.key] = a.n;
    h[a.key] = h[a.key] || {}; h[a.key].letters = a.n + a.mail;
    window.__camDoc = d;
    document.dispatchEvent(new CustomEvent('farmdoc', { detail: d }));
  }, { key: key, n: n, mail: mail || 0 });
}
async function settled(page) {
  await page.waitForFunction(function () { return !GARDEN.cam().anim; });
  await page.evaluate(function () { return new Promise(function (r) { requestAnimationFrame(function () { requestAnimationFrame(r); }); }); });
  return page.evaluate(function () {
    var c = GARDEN.cam(), b = c.w.index['project:coop'], h = b.home, T = WORLD.TILE;
    return { cam: { cx: c.cam.cx, cy: c.cam.cy, z: c.cam.z }, home: { x: h.x, y: h.y, w: h.w, h: h.h }, g: b.grid.g, kb: c.w.index['project:kbeads'].grid.g,
      sx: (h.x * T - c.cam.cx) * c.cam.z + c.vp.w / 2, sy: (h.y * T - c.cam.cy) * c.cam.z + c.vp.h / 2, sr: ((h.x + h.w) * T - c.cam.cx) * c.cam.z + c.vp.w / 2, vw: c.vp.w };
  });
}

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); } catch (e) { console.log(String(e).split('\n')[0]); process.exit(3); }
  try {
    var ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } }), page = await ctx.newPage(), errs = [];
    page.on('pageerror', function (e) { errs.push(String(e)); });
    await page.goto(url + '/garden?at=coop/home');
    await page.waitForSelector('#cursor-bar .cb-me', { state: 'attached' });
    await page.evaluate(function () { document.addEventListener('farmdoc', function (e) { if (window.__camDoc && e.detail !== window.__camDoc) e.stopImmediatePropagation(); }, true); });
    var s0 = await settled(page);
    assert.deepStrictEqual([s0.g, s0.kb, s0.home.w], [0, 0, 32], 'the fixture farms are 32 wide');

    // coop's own framed farm grows: the camera flies to the new framing, which holds the wider farm
    await grow(page, 'coop', 45);
    var flying = await page.evaluate(function () { return GARDEN.cam().anim; });
    var s1 = await settled(page);
    assert.deepStrictEqual([s1.g, s1.home.x, s1.home.w], [8, s0.home.x, 40], 'coop grows 8 tiles in place');
    assert.ok(flying && (s1.cam.cx !== s0.cam.cx || s1.cam.z !== s0.cam.z), 'the camera flew to the new framing: ' + JSON.stringify([s0.cam, s1.cam]));
    assert.ok(s1.sx >= 0 && s1.sr <= s1.vw, 'the whole wider farm in view: ' + JSON.stringify([s1.sx, s1.sr, s1.vw]));

    // a pointer down on the board: coop grows again, its corner stays, and no reframe runs under the gesture
    await page.mouse.move(640, 700); await page.mouse.down();
    await grow(page, 'coop', 88, 60);
    await page.waitForTimeout(400);
    var s2 = await settled(page);
    assert.deepStrictEqual([s2.g, s2.home.x], [12, s0.home.x], 'coop grows 12 tiles in place');
    assert.deepStrictEqual(s2.cam, s1.cam, 'with a pointer down only the anchor runs, and it did not move');
    await page.mouse.move(610, 690, { steps: 4 }); await page.mouse.up();   // the gesture ends as a pan: the camera is the viewer's now

    // kbeads, a column west of coop, widens: coop moves east in the world and stays put on the screen; under the viewer's
    // camera no reframe runs, so the anchor alone holds it
    var s3 = await settled(page);
    await grow(page, 'kbeads', 12);
    var s4 = await settled(page);
    assert.deepStrictEqual([s4.kb, s4.home.x - s3.home.x, s4.home.y - s3.home.y], [4, 4, 0], 'kbeads grows 4 tiles and coop moves 4 east');
    assert.ok(Math.abs(s4.sx - s3.sx) < 0.5 && Math.abs(s4.sy - s3.sy) < 0.5 && s4.cam.z === s3.cam.z, 'coop\'s farm at the same screen px: ' + JSON.stringify([s3.sx, s3.sy, s4.sx, s4.sy]));

    assert.deepStrictEqual(errs, [], 'no page errors');
    await ctx.close();
    console.log('camera grow browser checks passed');
  } finally { await browser.close(); }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
