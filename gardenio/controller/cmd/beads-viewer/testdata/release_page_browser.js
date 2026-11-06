/* The Release pages in a real Chromium (plan gd-UZKZ9H25UD step 8), run from TestReleasePagesUnderChromium against the
   viewer's routes on an httptest server, with step 6's JSON routes answered by a fixture of the agreed shape. Usage:
   node release_page_browser.js <server url> <node_modules dir>; RELEASE_PAGE_SHOTS=<dir> saves a screenshot per cell.

   At a phone's size (390 px) and a desktop's, in the day skin and the night one: /releases lists gardenio's releases newest
   first with their status words (an older built release a newer one overtook reads "skipped"), counts and where they run;
   /release/gd-rel3 shows the header, the Contents grouped by plan with the finished plan first and Unplanned last, the
   Deployments timeline with prod running now, and Previous; /release/gd-rel1 says "No deployments yet." and "This
   release carries no PRs."; nothing scrolls sideways; Tab reaches the PR links; and nothing throws. */
'use strict';
var base = process.argv[2], mods = process.argv[3], assert = require('assert'), path = require('path');
var shots = process.env.RELEASE_PAGE_SHOTS || '';
var pw = require(require.resolve('playwright-core', { paths: [mods] }));
var CELLS = [
  { name: 'phone-day', skin: 'homestead', ctx: { viewport: { width: 390, height: 844 }, deviceScaleFactor: 2, hasTouch: true, isMobile: true } },
  { name: 'phone-night', skin: 'dark-homestead', ctx: { viewport: { width: 390, height: 844 }, deviceScaleFactor: 2, hasTouch: true, isMobile: true } },
  { name: 'desktop-day', skin: 'homestead', ctx: { viewport: { width: 1440, height: 900 } } },
  { name: 'desktop-night', skin: 'dark-homestead', ctx: { viewport: { width: 1440, height: 900 } } },
];

async function noSideScroll(page, label) {
  var over = await page.evaluate(function () { return document.documentElement.scrollWidth - window.innerWidth; });
  assert.ok(over <= 1, label + ': page scrolls sideways by ' + over + 'px');
}

async function setSkin(page, skin) {
  // SKIN_CLOCK picks day or night by the clock; pin the cell's skin.
  await page.evaluate(function (s) { document.documentElement.setAttribute('data-skin', s); }, skin);
}

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); } catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  try {
    for (var cell of CELLS) {
      var ctx = await browser.newContext(cell.ctx), page = await ctx.newPage(), errs = [];
      page.on('pageerror', function (e) { errs.push(String(e)); });

      // /releases
      await page.goto(base + '/releases');
      await setSkin(page, cell.skin);
      await page.waitForSelector('section[data-project="gardenio"] .rel-list:not([hidden]) .rel-row', { timeout: 10000 });
      var rows = await page.$$eval('section[data-project="gardenio"] .rel-row', function (els) {
        return els.map(function (li) {
          return {
            tag: li.querySelector('.rel-tag').textContent, href: li.querySelector('.rel-tag').getAttribute('href'),
            status: li.querySelector('.release-status').textContent, counts: li.querySelector('.rel-counts').textContent,
            runs: li.querySelector('.rel-runs').textContent.replace(/\s+/g, ' ').trim(),
          };
        });
      });
      assert.deepStrictEqual(rows.map(function (r) { return r.tag; }), ['2026.304.2', '2026.304.1', '2026.304.0'], cell.name + ': list order');
      assert.deepStrictEqual(rows.map(function (r) { return r.status; }), ['live', 'superseded', 'skipped'], cell.name + ': status words');
      assert.strictEqual(rows[0].href, '/release/gd-rel3');
      assert.strictEqual(rows[0].counts, '4 PRs \u00b7 2 plans', cell.name + ': counts');
      assert.ok(/^prod since \d{4}-\d\d-\d\d \d\d:\d\dZ$/.test(rows[0].runs), cell.name + ': runs ' + JSON.stringify(rows[0].runs));
      assert.strictEqual(rows[2].runs, 'not deployed yet', cell.name + ': an undeployed release');
      var liveColour = await page.$eval('.release-status-live', function (el) { return getComputedStyle(el).color; });
      await noSideScroll(page, cell.name + ' /releases');
      if (shots) await page.screenshot({ path: path.join(shots, 'releases-' + cell.name + '.png'), fullPage: true });

      // /release/gd-rel3
      await page.goto(base + '/release/gd-rel3');
      await setSkin(page, cell.skin);
      await page.waitForSelector('#rel-page .rel-plan', { timeout: 10000 });
      var view = await page.evaluate(function () {
        var q = function (s) { var el = document.querySelector(s); return el ? el.textContent.replace(/\s+/g, ' ').trim() : null; };
        return {
          status: q('[data-slot="status"]'),
          sha: document.querySelector('[data-slot="sha"] a').getAttribute('href'),
          cutBy: q('[data-slot="cut_by"]'),
          plans: Array.prototype.map.call(document.querySelectorAll('.rel-plan > h3 > [data-slot="plan"]'), function (a) { return a.textContent; }),
          finished: q('.rel-plan [data-slot="finished"]:not([hidden])'),
          steps: Array.prototype.map.call(document.querySelectorAll('.rel-plan:first-of-type .rel-step h4 a'), function (a) { return a.textContent; }),
          firstPR: q('.rel-pr .rel-pr-num'),
          prHref: document.querySelector('.rel-pr .rel-pr-num').getAttribute('href'),
          deploys: Array.prototype.map.call(document.querySelectorAll('.rel-deploy'), function (li) { return li.textContent.replace(/\s+/g, ' ').trim(); }),
          prev: document.querySelector('[data-slot="previous"]').hidden ? null : document.querySelector('[data-slot="previous"]').getAttribute('href'),
          next: document.querySelector('[data-slot="next"]').hidden,
          state: document.querySelector('#rel-page > [data-slot="state"]').hidden,
        };
      });
      assert.strictEqual(view.status, 'live', cell.name + ': status');
      assert.strictEqual(view.sha, 'https://github.com/acme/gardenio/commit/9cf6aca202d993f56ae0a59a4b7bf4f444916536', cell.name + ': sha link');
      assert.strictEqual(view.cutBy, 'release-captain');
      assert.deepStrictEqual(view.plans, ['Releases of a project', 'Ownership grant table', 'Unplanned'], cell.name + ': plan order');
      assert.strictEqual(view.finished, 'finished in this release', cell.name + ': finished plan');
      assert.deepStrictEqual(view.steps, ['Machine client'], cell.name + ': steps of the first plan');
      assert.strictEqual(view.firstPR, '#1639');
      assert.strictEqual(view.prHref, 'https://github.com/acme/gardenio/pull/1639');
      assert.strictEqual(view.deploys.length, 1);
      assert.ok(/^prod k3s since .* running now revision 414 · by gardenio-release-deployer · script$/.test(view.deploys[0]), cell.name + ': deploy ' + JSON.stringify(view.deploys[0]));
      assert.strictEqual(view.prev, '/release/gd-rel2', cell.name + ': previous');
      assert.strictEqual(view.next, true, cell.name + ': the newest release has no next');
      assert.strictEqual(view.state, true, cell.name + ': loading line left up');
      await noSideScroll(page, cell.name + ' /release');
      if (shots) await page.screenshot({ path: path.join(shots, 'release-' + cell.name + '.png'), fullPage: true });

      // Keyboard: Tab from the top reaches the first PR link.
      if (!cell.ctx.isMobile) {
        await page.focus('body');
        var reached = false;
        for (var i = 0; i < 120 && !reached; i++) {
          await page.keyboard.press('Tab');
          reached = await page.evaluate(function () { var a = document.activeElement; return !!(a && a.classList.contains('rel-pr-num')); });
        }
        assert.ok(reached, cell.name + ': Tab never reached a PR link');
        var ring = await page.evaluate(function () { return getComputedStyle(document.activeElement).outlineStyle; });
        assert.notStrictEqual(ring, 'none', cell.name + ': no focus ring on the PR link');
      }

      // /release/gd-rel2: superseded on prod, next and previous by tag.
      await page.goto(base + '/release/gd-rel2');
      await page.waitForSelector('.rel-deploy', { timeout: 10000 });
      var two = await page.evaluate(function () {
        return {
          deploy: document.querySelector('.rel-deploy [data-slot="state"]').textContent,
          next: document.querySelector('[data-slot="next"]').getAttribute('href'),
          prev: document.querySelector('[data-slot="previous"]').getAttribute('href'),
        };
      });
      assert.ok(/^superseded \d{4}-\d\d-\d\d \d\d:\d\dZ$/.test(two.deploy), cell.name + ': superseded-at ' + JSON.stringify(two.deploy));
      assert.strictEqual(two.next, '/release/2026.304.2?project=gardenio');
      assert.strictEqual(two.prev, '/release/2026.303.2?project=gardenio');

      // /release/gd-rel1: nothing deployed, nothing carried.
      await page.goto(base + '/release/gd-rel1');
      await page.waitForSelector('[data-slot="no_deployments"]:not([hidden])', { timeout: 10000 });
      var empty = await page.evaluate(function () {
        return [document.querySelector('[data-slot="no_deployments"]').textContent, document.querySelector('[data-slot="contents_summary"]').textContent,
          document.querySelector('[data-slot="runs_on"]').textContent];
      });
      assert.deepStrictEqual(empty, ['No deployments yet.', 'This release carries no PRs.', 'not deployed yet'], cell.name + ': empty release');

      assert.deepStrictEqual(errs, [], cell.name + ': page errors');
      console.log(cell.name + ': ' + rows.length + ' releases listed; plans ' + view.plans.join(' / ') + '; live colour ' + liveColour);
      await ctx.close();
    }
  } finally {
    await browser.close();
  }
})().catch(function (e) { console.error(String(e.stack || e)); process.exit(1); });
