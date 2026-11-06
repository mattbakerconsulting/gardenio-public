/* The cost golden rows, out of golden_browser.js to keep that file under quench's 1500-line cap.

   The header purse (gd-H9LerJ0v3g): $ today and $ remaining in the header's controls, beside Mine and Everyone, for each
   state of an admin's fleet figures (ok, low, over, no account budget, the rollup down) and a scoped viewer's one row per
   project (one low, one with no budget, one stale). And plan A's cockpit with its spend gone stale (gd-rtE07tg6Nh).
   golden_browser.js puts rows(boxClip, paneClip, clock) in SHOTS; clock() is its frozen now, in ms. */

'use strict';

/* The header purse painted from cost, a fixture farm cost blob (web/costs.js paintPurse), once the farm document has painted
   it: later documents are held off, so the shot keeps the fixture's state. Clipped to the header's controls. */
function purseShot(boxClip, name, cost) {
  return { name: name, path: '/garden', clip: boxClip(['.crumb .ctl'], 6), show: async function (page) {
    await page.waitForSelector('#purse:not([hidden])');
    await page.evaluate(function (c) {
      document.addEventListener('farmdoc', function (e) { e.stopImmediatePropagation(); }, true);
      COSTS.paintPurse(c);
    }, cost);
  } };
}

/* Plan A's cockpit at its crew header, the spend line stale: the farm document handed on once more with plan A's cost as
   the farm sends it when its crew's rollup stops being re-stamped (TestCostPatchUnderChromium drives that patch for real);
   later documents are held off, so the shot keeps it. */
function cockpitStale(paneClip) {
  return { name: 'cockpitstale', path: '/garden?plan=gd-planA', clip: paneClip('#bead'), show: async function (page) {
    await page.waitForSelector('#bv-body .pv-crew .pv-spend');
    await page.evaluate(function () {
      var d = JSON.parse(JSON.stringify(GARDEN.farm()));
      d.plans.forEach(function (p) { if (p.id === 'gd-planA') p.cost.stale = true; });
      document.addEventListener('farmdoc', function (e) { if (e.detail !== d) e.stopImmediatePropagation(); }, true);
      document.dispatchEvent(new CustomEvent('farmdoc', { detail: d }));
      document.querySelector('#bv-body .pv-crew').scrollIntoView({ block: 'start' });
    });
    await page.waitForSelector('#bv-body .pv-spend.stale');
  } };
}

function rows(boxClip, paneClip, clock) {
  var stale = Math.floor(clock() / 1000) - 1380;
  return [
    purseShot(boxClip, 'purseok', { scope: 'fleet', has: true, today_usd: 13.5, has_budget: true, budget_usd: 300, remaining_usd: 186.5, level: 'ok', projects: [] }),
    purseShot(boxClip, 'purselow', { scope: 'fleet', has: true, today_usd: 41.2, has_budget: true, budget_usd: 300, remaining_usd: 38.75, level: 'warn', projects: [] }),
    purseShot(boxClip, 'purseover', { scope: 'fleet', has: true, today_usd: 72.04, has_budget: true, budget_usd: 300, remaining_usd: 0, level: 'over', projects: [] }),
    purseShot(boxClip, 'pursenobudget', { scope: 'fleet', has: true, today_usd: 13.5, projects: [] }),
    purseShot(boxClip, 'pursestale', { scope: 'fleet', as_of: stale, has_budget: true, budget_usd: 300, remaining_usd: 186.5, level: 'ok', projects: [] }),
    purseShot(boxClip, 'pursescoped', { scope: 'project', has: false, projects: [
      { project: 'gardenio', has: true, has_budget: true, budget_usd: 50, today_usd: 6.4, remaining_usd: 9.6, level: 'warn' },
      { project: 'kbeads', has: true, has_budget: false, today_usd: 1.25 },
      { project: 'coop', has: false, has_budget: true, budget_usd: 20, as_of: stale }] }),
    cockpitStale(paneClip),
  ];
}

module.exports = { rows: rows };
