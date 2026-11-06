/* The release truck's golden shots (gd-yOOVahJMCh) and its release panel (gd-rSz81Me1Pe), split out of golden_browser.js by
   concern: require('./golden_truck_browser.js')(h) with the harness's boxClip, look and paneClip gives truckShot and
   releaseShot, SHOTS row builders that golden_browser.js and the skill's shoot.js both use. */
'use strict';
var path = require('path');
var RELEASES = require(path.join(__dirname, 'releases_fixture.json'));

module.exports = function (h) {
  var boxClip = h.boxClip, look = h.look, paneClip = h.paneClip;
  /* truckShot (gd-yOOVahJMCh): the release truck in gardenio's bay under its Barn (28.10), the farm document's releases blob (plan A's shipped tag in
     its map) fed from testdata/releases_fixture.json[moment] (TRUCK.feed), until fixtureWorld() carries releases (step 5,
     gd-90IZtpGyR4). The camera on the truck's bay at the Plan level (0.82x), the shot clipped round it;
     tip: at the Bead level (1.12x, then 1x), its tip open as a hover (a desktop) or focus (a phone) opens it. */
  function truckShot(name, moment, tip) {
    return { name: name, path: '/garden?at=~/home', clip: tip ? boxClip(['#stip', '#ghots a.w-truck'], 40) : truckClip, show: async function (page, as) {
      await page.waitForSelector('#hots .w-home', { state: 'attached' });   // the farm is in; the cursor on the Commons's home cell keeps its label off the truck
      var at = await page.evaluate(async function (rel) {
        TRUCK.feed(rel);   // pinned: the live stream's documents carry no releases yet
        var doc = await (await fetch('/garden/farm', { headers: { Accept: 'application/json' } })).json();
        var tr = TRUCK.place(TRUCK.norm(rel), WORLD.layout(doc, null, { readonly: true }))[0];
        return { cx: tr.x + tr.w / 2, cy: tr.y + tr.h / 2 };
      }, RELEASES[moment]);
      if (tip) { await look(page, { cx: at.cx, cy: at.cy, z: 1.12 }); await look(page, { z: 1 }); }
      else await look(page, { cx: at.cx, cy: at.cy, z: 0.82 });
      await page.waitForFunction(function () { return TRUCK.list().length === 1; });
      if (!tip) return;
      await page.waitForSelector('#ghots a.w-truck', { state: 'attached' });
      if (as === 'phone') await page.evaluate(function () { document.querySelector('#ghots a.w-truck').focus(); });
      else await page.evaluate(function () { document.querySelector('#ghots a.w-truck').dispatchEvent(new PointerEvent('pointerenter', { pointerType: 'mouse' })); });
      await page.waitForFunction(function () { var t = document.getElementById('stip'); return t && !t.hidden && /Release 2026\.299\.2/.test(t.textContent); });
    } };
  }
  /* The middle of the viewport, where truckShot puts the truck: the truck (72 x 48 world px, gd-Ir8jxl4c57), its bay, the SHIPPED
     plate over it and the barn above it. */
  async function truckClip(page) { var vs = page.viewportSize(); return { x: Math.round(vs.width / 2 - 170), y: Math.round(vs.height / 2 - 90), width: 340, height: 180 }; }
  /* releaseShot (gd-rSz81Me1Pe, graded and promoted in gd-HHMReWCHUi): the truck at its bay (truckShot's 'shipping' moment),
     focused, Enter: the release panel over the map, the pane clipped. The viewer's /api/releases/{id} is not on the fixture
     server, so the shot answers it: 12 numbered PRs of the truck's 14 by plan and step, plan A finished; stress (shoot.js only)
     72 PRs over 9 plans with F1, F2, F4 and F6 titles. */
  function releaseShot(name, stress) {
    var R = 'https://github.com/mattbakerconsulting/gardenio/pull/', rows = [];
    if (!stress) {
      [['gd-planA', 'Plan A: the release record', 'closed', 6], ['gd-planP', 'Plan P: the truck', '', 4], ['gd-planB', 'Plan B: the Release page', '', 2]].forEach(function (p, j) {
        for (var i = 0; i < p[3]; i++) rows.push({ pr: 1800 + j * 10 + i, title: 'feat(gardenio/gardenhub): change ' + (i + 1) + ' of ' + p[1].split(':')[0], url: R + (1800 + j * 10 + i), plan: p[0], plan_title: p[1], plan_status: p[2], step: p[0] + '-s' + (i % 2), step_title: (i % 2 + 1) + '. Step ' + (i % 2 + 1) + ' of ' + p[1].split(':')[0] });
      });
    } else {
      for (var i = 0; i < 72; i++) {
        var pl = 'gd-StressPlan' + (i % 9), long = ('fix(gardenio/gardenhub): the release panel 🧑‍🌾 مريم keeps a long title readable when it is far too long for one line ').repeat(3).slice(0, 200);
        rows.push({ pr: 4000 + i, title: i % 7 === 3 ? 'see https://github.com/mattbakerconsulting/gardenio/pull/' + (4000 + i) + '/files?diff=split&w=1&file-filters%5B%5D=.go' : long, url: R + (4000 + i), plan: i % 9 === 8 ? '' : pl, plan_title: i % 9 > 5 ? '' : 'Plan ' + (i % 9) + ': ' + long, step: pl + '-s' + (i % 3), step_title: i % 3 ? 'Step ' + (i % 3) + ' 🚚 ' + long : '' });
      }
    }
    var rel = { id: 'gd-RelFix0002', tag: '2026.299.2', status: 'built', sha: 'abcdef0123456789abcdef0123456789abcdef01', cut_at: '2026-10-25T14:33:20Z', project: 'gardenio', previous: 'gd-RelFix0001', previous_tag: '2026.299.1',
      contents: rows, deployments: [{ target: 'prod', since: '2026-10-22T16:33:20Z', until: '2026-10-25T16:00:00Z' }] };
    var truck = truckShot(name, 'shipping', false);
    return { name: name, path: truck.path, root: '#bead', clip: paneClip('#bead'), show: async function (page, as) {
      await page.route('**/api/releases/**', function (route) { return route.fulfill({ json: rel }); });
      await truck.show(page, as);
      await page.waitForSelector('#ghots a.w-truck', { state: 'attached' });
      await page.focus('#ghots a.w-truck');
      await page.keyboard.press('Enter');
      await page.waitForSelector('#bead:not([hidden]) .rl-plan');
    } };
  }

  return { truckShot: truckShot, releaseShot: releaseShot };
};
