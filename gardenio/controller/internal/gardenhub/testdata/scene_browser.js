/* The World drawn in a page with none of the signed-in page (LW.3 gd-VEbAvsTNDe), run from TestSceneDrawsAloneUnderChromium
   against a test page on an httptest server: a canvas, the page-scene.js bundle as /garden serves it and a land window's scene
   (internal/planwindow's mixed fixture) inline, and a fetch that only records. Usage: node scene_browser.js <base url>
   <node_modules dir>; GARDENHUB_SHOTS=<dir> saves a screenshot per level.

   At a phone's size, light and dark, it lays the scene out with SCENE.lay and draws it with SCENE.scene(cv).draw at each level,
   framed on the region (World, Project), its first plot (Plan) or that plot's first step (Bead), until draw asks for no more
   frames. Each frame has many colours, the region's own ground short of Bead and the wild grass around it at World and Project; nothing was fetched, no me, PLANS, WORKBENCH,
   BEAD, CHAT or GARDEN exists, and nothing threw. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert'), path = require('path');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); } catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  try {
    for (var night of [false, true]) {
      var page = await browser.newPage({ viewport: { width: 390, height: 844 }, deviceScaleFactor: 2 }), errs = [];
      page.on('pageerror', function (e) { errs.push(String(e)); });
      await page.goto(url + '/');
      var r = await page.evaluate(function (night) {
        var out = { gone: ['me', 'PLANS', 'WORKBENCH', 'BEAD', 'CHAT', 'GARDEN', 'VIEWCUT'].filter(function (k) { return typeof window[k] !== 'undefined'; }), levels: [] };
        var doc = JSON.parse(document.getElementById('doc').textContent), st = SCENE.lay(doc), cv = document.getElementById('cv'), sc = SCENE.scene(cv);
        var vp = { w: 390, h: 844, dpr: 2 }, b = st.world.biomes[0], f = WORLD.tiles(b.foot), p = WORLD.tiles(b.plots[0]), c = WORLD.tiles(b.plots[0].beds[0].cells[0]);
        var px = function (c) { return c.r << 16 | c.g << 8 | c.b; }, hex = function (s) { return parseInt(s.slice(1), 16); };
        var grass = hex(ART.palette(night).g), ground = hex(ART.biomePalette(night, b.palette).g);
        for (var level = 0; level <= 3; level++) {
          var lr = WORLD.levelRange(level), z = Math.sqrt(lr[0] * Math.min(lr[1], 4)), at = [f, f, p, c][level], i = 0;
          var o = { cam: { cx: at.x + at.w / 2, cy: at.y + at.h / 2, z: z }, vp: vp, night: night, world: st.world, gs: st.gs, sst: st.sst, edgeIx: st.edgeIx, weedIx: st.weedIx, nb: st.nb };
          while (sc.draw(o) && ++i < 40) { /* a region's chunks bake over a few frames */ }
          var d = cv.getContext('2d').getImageData(0, 0, cv.width, cv.height).data, seen = {};
          for (var k = 0; k < d.length; k += 4) seen[px({ r: d[k], g: d[k + 1], b: d[k + 2] })] = 1;
          out.levels.push({ level: level, lod: WORLD.lodFor(z), frames: i + 1, colours: Object.keys(seen).length, grass: !!seen[grass], ground: !!seen[ground] });
        }
        out.fetched = window.FETCHED;
        return out;
      }, night);
      console.log((night ? 'night ' : 'day ') + JSON.stringify(r));
      assert.deepStrictEqual(r.gone, [], 'the test page has signed-in globals');
      assert.deepStrictEqual(r.fetched, [], 'drawing the scene fetched');
      r.levels.forEach(function (l) {
        assert.strictEqual(l.lod, l.level, 'the camera for level ' + l.level + ' is at level ' + l.lod);
        assert.ok(l.frames < 40, 'level ' + l.level + ' never settled');
        assert.ok(l.colours > 12, 'level ' + l.level + ': only ' + l.colours + ' colours on the canvas');
        if (l.level <= 2) assert.ok(l.ground, 'level ' + l.level + ": the region's ground is not on the canvas");
        if (l.level <= 1) assert.ok(l.grass, 'level ' + l.level + ': no wild grass around the region');
      });
      assert.deepStrictEqual(errs, [], 'the page threw');
      if (process.env.GARDENHUB_SHOTS) await page.screenshot({ path: path.join(process.env.GARDENHUB_SHOTS, 'scene-' + (night ? 'dark' : 'light') + '.png') });
      await page.close();
    }
  } finally { await browser.close(); }
})().catch(function (e) { console.error(e); process.exit(1); });
