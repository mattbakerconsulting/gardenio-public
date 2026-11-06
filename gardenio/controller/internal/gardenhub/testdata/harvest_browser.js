/* Browser check for the bead view's Harvest section (harvest spec 6.2, step 4b.6.3), run from TestHarvestUnderChromium
   against /garden on an httptest server with cardWorld, whose bead reader holds gd-HarvestSt1 (harvestStep in
   bead_harvest_test.go): a closed step with a merged PR, two UX grades, a report, a follow-up and two crews.
   Usage: node harvest_browser.js <base url> <node_modules dir>. GARDENHUB_SHOTS=<dir> saves a PNG per cell, for review.

   Desktop and phone, light and dark: on the closed step Harvest is the panel's first section, above Description; its
   headline is the base word and the badge words in the spec's order; the newest grade's four pictures load through the
   image proxy, each a link with a name; the grades list newest first with who; lead time and cost are in words. On an open
   step there is no Harvest section. No console errors, and the only requests off /garden are the image proxy's. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert'), path = require('path');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));
var shots = process.env.GARDENHUB_SHOTS || '';
function skyCtx(browser, opts) {
  return browser.newContext(opts).then(function (ctx) {
    return ctx.addInitScript(function (s) { try { localStorage.setItem('garden.sky', s); } catch (e) { /* no storage: the emulated scheme decides */ } }, opts.colorScheme === 'dark' ? 'night' : 'day').then(function () { return ctx; });
  });
}
async function view(ctx, id) {
  var page = await ctx.newPage(), errors = [];
  page.on('console', function (m) { if (m.type() === 'error') errors.push(m.text()); });
  page.on('pageerror', function (e) { errors.push(String(e)); });
  page.on('request', function (r) {
    var u = new URL(r.url());
    if (u.origin === new URL(url).origin && u.pathname !== '/garden' && u.pathname.indexOf('/garden/') !== 0 && u.pathname.indexOf('/image/') !== 0) errors.push('left the garden: ' + u.pathname);
  });
  await page.goto(url + '/garden?bead=' + id + '&at=gardenio');
  await page.locator('#bead').waitFor({ state: 'visible' });
  await page.waitForFunction(function () { var b = document.getElementById('bv-body'); return b.textContent.trim() && !/Loading/.test(b.textContent) && b.querySelector('.bv-sec'); });
  return { page: page, errors: errors };
}

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); } catch (e) { console.log(String(e).split('\n')[0]); process.exit(3); }
  var cells = [
    { name: 'desktop-light', opts: { viewport: { width: 1440, height: 900 }, colorScheme: 'light' } },
    { name: 'desktop-dark', opts: { viewport: { width: 1440, height: 900 }, colorScheme: 'dark' } },
    { name: 'phone-light', opts: { viewport: { width: 390, height: 844 }, deviceScaleFactor: 2, isMobile: true, hasTouch: true, colorScheme: 'light' } },
    { name: 'phone-dark', opts: { viewport: { width: 390, height: 844 }, deviceScaleFactor: 2, isMobile: true, hasTouch: true, colorScheme: 'dark' } },
  ];
  try {
    for (var c of cells) {
      var ctx = await skyCtx(browser, c.opts), v = await view(ctx, 'gd-HarvestSt1'), page = v.page;
      await page.waitForFunction(function () { return Array.prototype.every.call(document.querySelectorAll('#bv-body .bv-pics img'), function (i) { return i.complete; }); });
      var got = await page.evaluate(function () {
        var b = document.getElementById('bv-body'), secs = b.querySelectorAll('.bv-sec'), hv = b.querySelector('.bv-harvest');
        var dts = Array.prototype.map.call(hv.querySelectorAll('dt'), function (d) { return d.textContent; });
        var dd = function (k) { var i = dts.indexOf(k); return i < 0 ? '' : hv.querySelectorAll('dd')[i].textContent; };
        return {
          first: secs[0].querySelector('h3').textContent, second: secs[1].querySelector('h3').textContent,
          head: Array.prototype.map.call(hv.querySelectorAll('.bv-hv-head > *'), function (e) { return e.textContent; }),
          pics: Array.prototype.map.call(hv.querySelectorAll('.bv-pics a'), function (a) {
            var i = a.querySelector('img'), r = a.getBoundingClientRect();
            return { src: i && i.getAttribute('src'), ok: !!i && i.naturalWidth > 0, alt: i && i.alt, h: r.height, w: r.width, href: a.href };
          }),
          grades: Array.prototype.map.call(hv.querySelectorAll('dd .bv-rels li'), function (l) { return l.textContent; }),
          dts: dts, checks: dd('Checks'), time: dd('Time and cost'), wrap: dd('Wrap-up'), fu: dd('Follow-ups'),
          over: b.scrollWidth > b.clientWidth + 1,
        };
      });
      assert.strictEqual(got.first, 'Harvest', c.name + ': Harvest is the first section');
      assert.strictEqual(got.second, 'Description', c.name + ': Description comes after it');
      assert.deepStrictEqual(got.head, ['harvested', 'merged #825', 'graded 5/5 gates', '4 pictures', '3 tests', 'checks green', 'merged after 2 rounds'], c.name + ': the headline');
      assert.strictEqual(got.pics.length, 4, c.name + ': the newest grade\'s pictures');
      got.pics.forEach(function (p, i) {
        assert.ok(p.ok && /^\/image\/coop\/sessions\/images\/b\d\.png$/.test(p.src), c.name + ': picture ' + i + ' loads through the proxy: ' + JSON.stringify(p));
        assert.strictEqual(p.alt, 'cursorphase, bead view · b' + (i + 1) + '.png', c.name + ': named by its grade\'s subject and its file, no counter');
        assert.ok(p.h >= 44 && p.w >= 44, c.name + ': a picture is a 44px target: ' + p.w + 'x' + p.h);
        assert.strictEqual(new URL(p.href).pathname, p.src, c.name + ': a picture links to its proxied upload, never a bucket host: ' + p.href);
      });
      assert.strictEqual(got.grades.length, 2);
      assert.ok(/^graded 5\/5: cursorphase, bead view.*by crew-b/.test(got.grades[0]) && /^graded 4\/5: cursorphase.*by crew-a/.test(got.grades[1]), c.name + ': grades newest first: ' + got.grades);
      assert.deepStrictEqual(got.dts, ['Grades', 'Checks', 'Time and cost', 'Produce', 'Follow-ups', 'Wrap-up']);
      assert.strictEqual(got.checks, 'no goldens touched');
      assert.strictEqual(got.time, 'closed in 2d · $0.29 across 2 crews');
      assert.ok(/still open/.test(got.fu) && /^Stamp lands on every plot/.test(got.wrap), c.name + ': follow-ups and wrap-up: ' + got.fu + ' / ' + got.wrap);
      assert.ok(!got.over, c.name + ': nothing scrolls sideways');
      if (shots) await page.screenshot({ path: path.join(shots, 'harvest-' + c.name + '.png') });
      // an open step has no harvest
      var o = await view(ctx, 'gd-StressF1F2');
      assert.strictEqual(await o.page.locator('#bv-body .bv-harvest').count(), 0, c.name + ': no Harvest on an open step');
      assert.deepStrictEqual(v.errors.concat(o.errors), [], c.name + ': no errors');
      await ctx.close();
    }
    console.log('harvest: section first on a closed step, absent on an open one, desktop and phone, light and dark');
  } finally {
    await browser.close();
  }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
