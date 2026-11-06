/* The land golden rows (Lands Track B step 8.8, gd-vJK94LpTOm), out of golden_browser.js to keep that file under quench's
   1500-line cap.

   Where a tenant lands: /garden?land=meadow, the camera on the land's region and the plaque turned into the land header, the
   land's name and its purse in words. The card
   world's farm has no tenant land, so the farm document is rewritten on its way to the page into a tenant's cut of it
   (farm_cut.go): music's region, its plans, seeds and steward, renamed meadow (the Commons fixture's tenant land, which has a
   #general) and nothing else, with a purse on its cost row. land_test.go's land header tests guard the words; these guard the
   picture.
   - land: the whole page, the purse low (81.5 % used: the warn word), and Steward, the link to the land's steward view
     (music's steward is the land's, gd-LisW87zrnf).
   - landwall: the plaque only, the purse at the wall (the alert fill).
   - landcap: the plaque only, the purse low and the steward resting at its daily run cap (gd-OvKMtyWtrp); land's steward
     runs under its cap, so land is the cap's negative. */

'use strict';

/* tenantDoc(j, purse, land, steward) -> j cut to music's things, renamed land (default meadow), with purse on its cost row
   and steward's fields over its steward's. */
function tenantDoc(j, purse, land, steward) {
  land = land || 'meadow';
  var mine = function (x) { return x && x.project === 'music'; };
  var keep = function (xs) { return (xs || []).filter(mine).map(function (x) { x.project = land; return x; }); };
  j.plans = keep(j.plans); j.seeds = keep(j.seeds); j.gardeners = keep(j.gardeners);
  ['meadow', 'closed_plans'].forEach(function (k) { if (j[k]) { j[k].shown = keep(j[k].shown); j[k].total = j[k].shown.length; } });
  if (j.wishes) { j.wishes.open = keep(j.wishes.open); j.wishes.closed = keep(j.wishes.closed); j.wishes.open_total = j.wishes.open.length; j.wishes.closed_total = j.wishes.closed.length; }
  j.releases = {};
  var row = keep(j.cost && j.cost.projects)[0] || { project: land, has: true };
  row.purse = Object.assign({ land: land }, purse);
  if (steward) row.steward = Object.assign({}, row.steward, steward);
  j.cost = Object.assign({}, j.cost, { projects: [row] });
  return j;
}

var LOW = { state: 'ok', blocked: false, granted_credits: 100, spent_credits: 79.5, in_flight_credits: 2, remaining_credits: 18.5, margin_credits: 5, used_pct: 81.5, running_agents: 1 };
var CAPPED = { max_runs_per_day: 24, runs_today: 24, capped: true, resumes_at: 1792800000 };   // 00:00 UTC
var WALL = { state: 'wall', blocked: true, granted_credits: 100, spent_credits: 96.8, in_flight_credits: 0, remaining_credits: 3.2, margin_credits: 5, used_pct: 96.8, running_agents: 0 };

/* show(purse, land, steward) puts the land (default meadow) on screen: the farm document rewritten, the page opened
   again with nothing kept from its first load, the camera on its region, the plaque named and its purse said (and a capped
   steward's run cap). */
function show(purse, land, steward) {
  land = land || 'meadow';
  return async function (page) {
    await page.route('**/garden/farm', async function (r) {
      var res = await r.fetch();
      return r.fulfill({ response: res, json: tenantDoc(await res.json(), purse, land, steward) });
    });
    // The row's goto opened the page before this route, on the card world's whole farm, where the land is not on the map, and
    // how far that first load gets before it is left depends on the runner (gd-Gbu89DjQSt). By then it may have written its
    // address (at=gardenio) and the world's placement, cells and cursor memos, all of which a reload keeps: a slow runner
    // shot a "Nothing on the map is called gardenio" toast, or the land placed round the whole farm's memo. So the page opens
    // again at the row's own address, with those memos (garden.js's MEMO_KEY, CELLS_STORE and CURNAV.STORE) dropped before
    // its scripts run: what it lays out comes from the tenant's document alone.
    await page.addInitScript(function () {
      try { localStorage.removeItem('garden.world.v1'); localStorage.removeItem('garden.cursor.v1'); sessionStorage.removeItem('garden.cells.v1'); } catch (e) { /* no storage, no memo */ }
    });
    await page.goto(new URL(page.url()).origin + '/garden?land=' + encodeURIComponent(land));
    await page.waitForSelector('#hots .w-biome.cur', { state: 'attached' });
    await page.waitForSelector('#cursor-bar .cb-me');
    await page.waitForFunction(function (a) {
      var h = document.getElementById('land-hd');
      return h && !h.hidden && document.querySelector('.crumb h1').textContent === a.land && !h.querySelector('.lh-purse').hidden && !h.querySelector('.lh-cap:not([hidden])') === !a.capped;
    }, { land: land, capped: !!(steward && steward.capped) });
    await page.evaluate(function () { var a = document.activeElement; if (a && a.blur) a.blur(); });   // no focus ring in a golden
  };
}

function rows(paneClip) {
  return [
    { name: 'land', path: '/garden?land=meadow', show: show(LOW) },
    { name: 'landwall', path: '/garden?land=meadow', clip: paneClip('.crumb .plaque'), show: show(WALL) },
    { name: 'landcap', path: '/garden?land=meadow', clip: paneClip('.crumb .plaque'), show: show(LOW, null, CAPPED) },
  ];
}

module.exports = { rows: rows, tenantDoc: tenantDoc, show: show, CAPPED: CAPPED };
