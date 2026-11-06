/* The costsland golden row, out of golden_browser.js to keep that file under quench's 1500-line cap (gd-kSG5BqJMRo).

   A tenant land's credits (plan gd-gh5J0vVbJP step 3.3): the Costs panel on land:acme drawn by credits.js from landCredits,
   what GET /garden/charts/costs-land answers acme's tenant (the card world's farm has no tenant land, and land_credits_test.go
   guards the route): the spent tiles, the meter with the wall marked, the burn line with its dashed run-out, ten agents ranked
   and the rest. golden_browser.js puts row(paneClip, clock) in SHOTS; clock() is its frozen now, in ms. */

'use strict';

/* acme's credits as costs-land answers its tenant, over range, ending at now: 100 granted, 38.4 left, the wall at 5, a week's
   remaining falling hourly with a weekend's lull, half a credit an hour, twelve agents. */
function landCredits(range, now) {
  var end = Math.floor(now / 1000), start = end - 7 * 86400, pts = [], v = 60;
  for (var t = start, i = 0; t <= end; t += 3600, i++) { pts.push([t, Math.round(v * 100) / 100]); v -= (i % 168) > 96 && (i % 168) < 144 ? 0.03 : 0.17; }
  var agents = ['gardenio-crew-edge', 'gardenio-crew-login', 'acme-steward', 'acme-thread-docs', 'acme-crew-billing', 'acme-crew-search',
    'acme-crew-onboard', 'acme-review', 'acme-crew-export', 'acme-crew-a11y', 'acme-crew-ci', 'acme-crew-misc'];
  return { land: 'acme', range: range || '7d', history: 'ok',
    purse: { state: 'ok', granted_credits: 100, spent_credits: 59.6, in_flight_credits: 2, remaining_credits: 38.4, margin_credits: 5, used_pct: 61.6, read_at: new Date(now).toISOString() },
    spent: { today: 1.62, week: 21.4, month: 61.6 },
    burn: { start: start, end: end, points: pts, rate_per_hour: 0.5, wall: 5, run_out: end + 67 * 3600 },
    agents: agents.slice(0, 10).map(function (a, i) { return { agent: a, credits: Math.round(9.6 / (i + 1) * 100) / 100 }; }),
    rest: { count: 2, credits: 0.31 } };
}

function row(paneClip, clock) {
  return { name: 'costsland', path: '/garden?costs=land:acme', clip: paneClip('#bead'), show: async function (page) {
    await page.route('**/garden/charts/costs-land?*', function (r) { return r.fulfill({ json: landCredits(new URL(r.request().url()).searchParams.get('range'), clock()) }); });
    await page.reload();
    await page.locator('#bead').waitFor({ state: 'visible' });
    await page.waitForFunction(function () { return document.getElementById('bead').dataset.kind === 'costs' && document.querySelectorAll('#bv-body .cr-card').length === 4; });
  } };
}

module.exports = { row: row, landCredits: landCredits };
