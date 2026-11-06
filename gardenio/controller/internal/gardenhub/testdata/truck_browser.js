/* Browser walk for the release truck on the lanes (homestead-world-design.md 29.3, X-D11; plan gd-rLs7EoTa7a step 6,
   gd-tl9bVIBKTZ), run from TestTruckUnderChromium against a live /garden on the fixture farm. Usage: node truck_browser.js
   <base url> <node_modules dir>.

   gardenio's release goes live (releases_fixture.json's shipping, then live, fed with the truck's clock pinned), so its truck
   drives out. At moments through the drive, the camera on where the truck should be at the Plan level, the page draws it
   there: west out of the bay onto the west lane, north up it, then east along the north lane across the east lane's
   crossroads and off the region's east edge, its middle on a lane tile the whole way; away, it is drawn nowhere; on the way
   back it keeps to the lanes and parks in its bay facing east. Zero page errors throughout. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert'), path = require('path');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));
var REL = require(path.join(__dirname, 'releases_fixture.json'));

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); } catch (e) { console.log(String(e).split('\n')[0]); process.exit(3); }
  try {
    var page = await browser.newPage({ viewport: { width: 1280, height: 800 } }), errors = [];
    page.on('pageerror', function (e) { errors.push(String(e)); });
    await page.goto(url + '/garden?at=yard');
    await page.waitForSelector('#hots .w-home', { state: 'attached' });
    var r = await page.evaluate(async function (rel) {
      var t0 = 1792000000000;
      TRUCK.motion(true); TRUCK.pin(t0 - 1000); TRUCK.feed(rel.shipping); TRUCK.pin(t0); TRUCK.feed(rel.live);   // loading, then live: it drives out at t0
      var doc = await (await fetch('/garden/farm', { headers: { Accept: 'application/json' } })).json();
      window.__truck = TRUCK.place(TRUCK.norm(rel.live), WORLD.layout(doc, null, { readonly: true }))[0];
      var tr = window.__truck, b = WORLD.layout(doc, null, { readonly: true }).index['project:' + tr.key];
      return { t0: t0, x: tr.x, y: tr.y, lanes: b.lanes, bay: tr.bay, east: b.x + b.w, T: WORLD.TILE, TW: TRUCK.TW, TH: TRUCK.TH, D: TRUCK.DRIVE, A: TRUCK.AWAY, B: TRUCK.BACK };
    }, REL);
    var lane = function (n) { return r.lanes.filter(function (l) { return l.name === n; })[0]; };
    var inR = function (q, p) { return p.x >= q.x && p.x < q.x + q.w && p.y >= q.y && p.y < q.y + q.h; };
    var pull = { x: lane('west').x, y: r.bay.y, w: r.bay.x + r.bay.w - lane('west').x, h: r.bay.h };
    /* where a truck's box is: lane, bay (between the bay and the west lane), off (past the region's east edge), else grass */
    function where(q) {
      var m = { x: (q.x + r.TW / 2) / r.T, y: (q.y + r.TH / 2) / r.T };
      return m.x >= r.east ? 'off' : r.lanes.some(function (l) { return inR(l, m); }) ? 'lane' : inR(pull, m) ? 'bay' : 'grass';
    }
    /* the pose TRUCK says it has at t, and the truck the page drew at t with the camera on that pose at the Plan level (null: none) */
    async function at(t) {
      var want = await page.evaluate(function (a) { return TRUCK.pose(window.__truck, { at: a.t0, cargo: 4 }, a.t, false); }, { t0: r.t0, t: t });
      await page.evaluate(function (a) { TRUCK.pin(a.t); window.GARDEN.look({ cx: a.cx, cy: a.cy, z: 0.82 }); }, { t: t, cx: want.x + r.TW / 2, cy: want.y + r.TH / 2 });
      await page.waitForTimeout(120);
      return { want: want, got: await page.evaluate(function () { return TRUCK.list()[0] || null; }) };
    }
    // out: nineteen moments through the drive, each drawn where it drives and on the lanes
    var out = [], s, k;
    for (k = 1; k < 20; k++) {
      s = await at(r.t0 + r.D * k / 20);
      if (where(s.want) === 'off') continue;   // past the region's edge: the box may be beyond the view's pad
      assert.ok(s.got, 'the truck is drawn ' + k + '/20 through its drive: ' + JSON.stringify(s.want));
      assert.deepStrictEqual([s.got.phase, s.got.x, s.got.y, s.got.dir], ['out', s.want.x, s.want.y, s.want.dir], 'drawn where it drives at ' + k + '/20');
      assert.notStrictEqual(where(s.got), 'grass', 'on the lanes at ' + k + '/20: ' + JSON.stringify(s.got));
      out.push(s.got);
    }
    assert.ok(out[0].x < r.x && out[0].dir === -1 && out[0].y === r.y, 'west out of the bay first: ' + JSON.stringify(out[0]));
    var wx = (lane('west').x + 1.5) * r.T - r.TW / 2;
    assert.ok(out.some(function (q) { return q.x === wx && q.y < r.y; }), 'up the middle of the west lane: ' + JSON.stringify(out));
    var east = out.filter(function (q) { return q.dir === 1; }), ny = (lane('north').y + 1.5) * r.T - r.TH / 2, ex = (lane('east').x + 1.5) * r.T - r.TW / 2;
    assert.ok(east.length >= 3, 'then east: ' + JSON.stringify(out));
    assert.ok(east.every(function (q, i) { return q.y === ny && (!i || q.x > east[i - 1].x); }), 'east along the middle of the north lane: ' + JSON.stringify(east));
    assert.ok(east[0].x < ex && east[east.length - 1].x > ex, 'across the east lane at the north-east crossroads: ' + JSON.stringify([east[0].x, ex, east[east.length - 1].x]));
    s = await at(r.t0 + r.D - 1);
    assert.strictEqual(where(s.want), 'off', 'off the region by the end of its drive: ' + JSON.stringify(s.want));
    // away: off the map; then home along the lanes, parked in its bay facing east
    s = await at(r.t0 + r.D + r.A / 2);
    assert.deepStrictEqual([s.want.phase, s.got], ['away', null], 'away, it is drawn nowhere');
    var back = 0;
    for (k = 1; k < 10; k++) {
      s = await at(r.t0 + r.D + r.A + r.B * k / 10);
      if (where(s.want) === 'off') continue;
      assert.ok(s.got && s.got.phase === 'back' && where(s.got) !== 'grass', 'home on the lanes at ' + k + '/10: ' + JSON.stringify(s));
      back++;
    }
    assert.ok(back >= 5, 'seen on its way home: ' + back);
    s = await at(r.t0 + r.D + r.A + r.B + 1);
    assert.deepStrictEqual([s.got.phase, s.got.x, s.got.y, s.got.dir], ['park', r.x, r.y, 1], 'parked in its bay, facing east');
    assert.deepStrictEqual(errors, [], 'no page errors');
    console.log('truck browser: ' + out.length + ' moments out on the lanes, east across x=' + ex + ', ' + back + ' home, parked at ' + r.x + ',' + r.y);
  } finally {
    await browser.close();
  }
})().catch(function (e) { console.error(e); process.exit(1); });
