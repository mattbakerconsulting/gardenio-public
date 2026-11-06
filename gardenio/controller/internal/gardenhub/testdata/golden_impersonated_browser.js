/* The Become golden rows (plan gd-0SUIdKXWV3 step 4), out of golden_browser.js to keep that file under quench's 1500-line cap.

   A Become session: the fixture rig reads X-UX-Impersonator as the verified token's impersonator claim (card_browser_test.go
   uxImpersonator), so the page draws "You are <tenant>, impersonated by <admin>" and Stop in the banner on every surface.
   - impersonated: the banner above the opening farm. */

'use strict';

/* impersonate sends the header from here on and opens at (the row's path again: a reload would open what the page has since
   put in the address), then waits for the banner's Stop. */
async function impersonate(page, at) {
  await page.setExtraHTTPHeaders({ 'X-UX-Impersonator': 'matt' });
  await page.goto(new URL(at, page.url()).href);
  await page.waitForSelector('#imp .imp-stop', { state: 'visible' });
}

function rows() {
  return [
    { name: 'impersonated', path: '/garden', show: async function (page) {
      await impersonate(page, '/garden');
      await page.waitForFunction(function () { return document.querySelector('#cursor-bar .cb-me') && document.querySelector('#hots .hot.cur'); });
    } },
  ];
}

module.exports = { rows: rows };
