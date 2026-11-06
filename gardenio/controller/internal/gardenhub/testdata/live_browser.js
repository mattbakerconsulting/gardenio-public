/* Browser check for live panels (Lands workbench-glass spec WB-14, WB-15; step 5.6, gd-uRytFwHkLT), run from
   TestLivePanelsUnderChromium against /garden on an httptest server with liveWorld (live_test.go), whose POST /test/comment
   adds a comment the way the daemon does and POST /test/daemon takes the bead reader down and back.
   Usage: node live_browser.js <base url> <node_modules dir>. GARDENHUB_SHOTS=<dir> saves a PNG per state, for review.

   Desktop and phone, light and dark:
   - live: a comment added while the panel is open shows in it by itself, the panel's scroll where it was;
   - held: with a draft typed into the composer, a new comment does not repaint; the panel says the bead changed, the draft
     stays, and Show the latest shows it;
   - a failed refresh keeps the document and says it could not refresh, in words;
   - degraded: a load the daemon fails paints what the map knows (its title, not its id) with the line in words and Try
     again, which loads the bead once the daemon is back (unless its stream's own retry got there first).
   No console errors but the 502s asked for, and nothing leaves /garden but the test's own hooks. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert'), path = require('path');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));
var shots = process.env.GARDENHUB_SHOTS || '';
function skyCtx(browser, opts) {
  return browser.newContext(opts).then(function (ctx) {
    return ctx.addInitScript(function (s) { try { localStorage.setItem('garden.sky', s); } catch (e) { /* no storage: the emulated scheme decides */ } }, opts.colorScheme === 'dark' ? 'night' : 'day').then(function () { return ctx; });
  });
}
async function view(ctx, id, errors) {
  var page = await ctx.newPage();
  page.on('console', function (m) { if (m.type() === 'error' && !/status of 502/.test(m.text())) errors.push(m.text()); });
  page.on('pageerror', function (e) { errors.push(String(e)); });
  page.on('request', function (r) {
    var u = new URL(r.url());
    if (u.origin === new URL(url).origin && u.pathname !== '/garden' && u.pathname.indexOf('/garden/') !== 0 && u.pathname.indexOf('/test/') !== 0) errors.push('left the garden: ' + u.pathname);
  });
  await page.goto(url + '/garden?bead=' + id + '&at=gardenio');
  await page.locator('#bead').waitFor({ state: 'visible' });
  return page;
}
function hook(page, p) { return page.evaluate(function (p) { return fetch(p, { method: 'POST' }).then(function (r) { return r.status; }); }, p); }
function comments(page) { return page.evaluate(function () { return Array.prototype.map.call(document.querySelectorAll('#bv-body .bv-comments li'), function (l) { return l.textContent; }); }); }
async function shot(page, cell, name) { if (shots) await page.screenshot({ path: path.join(shots, 'live-' + name + '-' + cell + '.png') }); }

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
      var errors = [], ctx = await skyCtx(browser, c.opts), page = await view(ctx, 'gd-planA', errors);
      await page.waitForFunction(function () { return /The first comment\./.test(document.getElementById('bv-body').textContent); });
      // the stream is open before anything moves (the panel's own stream, live.go)
      await page.waitForFunction(function () { return fetch('/garden/state').then(function (r) { return r.json(); }).then(function (s) { return s.live_panels >= 1; }); }, null, { polling: 100 });

      // live: a new comment shows by itself, the scroll kept
      await page.evaluate(function () { document.getElementById('bv-body').scrollTop = 300; });
      var n0 = (await comments(page)).length;
      assert.strictEqual(await hook(page, '/test/comment?id=gd-planA'), 204);
      await page.waitForFunction(function (n) { return document.querySelectorAll('#bv-body .bv-comments li').length > n; }, n0, { timeout: 8000 });
      var top = await page.evaluate(function () { return document.getElementById('bv-body').scrollTop; });
      assert.ok(Math.abs(top - 300) <= 2, c.name + ': the refresh keeps the scroll (at ' + top + ', was 300)');
      assert.ok(/Live comment/.test((await comments(page))[0]), c.name + ': the new comment is first (newest first)');
      await shot(page, c.name, 'refreshed');

      // held: a draft in the composer holds the repaint
      var ta = page.locator('#bv-body .bw-comment textarea');
      await ta.fill('half a thought');
      var n1 = (await comments(page)).length;
      await hook(page, '/test/comment?id=gd-planA');
      await page.locator('#bv-body .bv-livenote').waitFor({ state: 'visible', timeout: 8000 });
      var held = await page.evaluate(function () {
        var n = document.querySelector('#bv-body .bv-livenote');
        return { text: n.textContent, role: n.getAttribute('role'), btn: n.querySelector('button') && n.querySelector('button').textContent };
      });
      assert.ok(/This bead changed while you were writing here\./.test(held.text), c.name + ': held line: ' + held.text);
      assert.strictEqual(held.role, 'status');
      assert.strictEqual(held.btn, 'Show the latest');
      assert.strictEqual(await ta.inputValue(), 'half a thought', c.name + ': the draft stays');
      assert.strictEqual((await comments(page)).length, n1, c.name + ': no repaint while held');
      await page.locator('#bv-body .bv-livenote').scrollIntoViewIfNeeded();
      await shot(page, c.name, 'held');
      await page.locator('#bv-body .bv-livenote button').click();
      await page.waitForFunction(function (n) { return document.querySelectorAll('#bv-body .bv-comments li').length > n && !document.querySelector('#bv-body .bv-livenote'); }, n1, { timeout: 8000 });

      // a failed refresh keeps the document and says so
      assert.strictEqual(await hook(page, '/test/daemon?down=1'), 204);
      var n2 = (await comments(page)).length;
      await hook(page, '/test/comment?id=gd-planA');
      await page.locator('#bv-body .bv-livenote.err').waitFor({ state: 'visible', timeout: 8000 });
      var failed = await page.evaluate(function () { var n = document.querySelector('#bv-body .bv-livenote'); return { text: n.textContent, btn: n.querySelector('button') && n.querySelector('button').textContent }; });
      assert.ok(/^Could not refresh this bead: the beads daemon did not answer \(502\)\. It shows the bead as it was a moment ago\./.test(failed.text), c.name + ': failed refresh line: ' + failed.text);
      assert.strictEqual(failed.btn, 'Try again');
      assert.strictEqual((await comments(page)).length, n2, c.name + ': the document shown stays');
      await page.locator('#bv-body .bv-livenote').scrollIntoViewIfNeeded();
      await shot(page, c.name, 'refresh-failed');
      await page.close();

      // degraded: what the map knows, in words, with Try again
      page = await view(ctx, 'gd-A1b', errors);
      await page.locator('#bv-body .bv-note.err').waitFor({ state: 'visible', timeout: 8000 });
      var deg = await page.evaluate(function () {
        var n = document.querySelector('#bv-body .bv-note.err'), b = document.getElementById('bv-body');
        return { title: document.getElementById('bv-title').textContent, text: n.textContent, btn: n.querySelector('button') && n.querySelector('button').textContent, over: b.scrollWidth > b.clientWidth + 1, meta: !!b.querySelector('.bv-meta') };
      });
      assert.strictEqual(deg.title, 'Staging soak test for exchange', c.name + ': the title the map knows, not the id');
      assert.ok(/^Could not load this bead: the beads daemon did not answer \(502\)\. This is what the map knows about it\./.test(deg.text), c.name + ': degraded line: ' + deg.text);
      assert.ok(!/Failed to fetch|Close and reopen/.test(deg.text), c.name + ': no browser wording, no "close and reopen"');
      assert.strictEqual(deg.btn, 'Try again');
      assert.ok(deg.meta, c.name + ': never a blank panel');
      assert.ok(!deg.over, c.name + ': no sideways scroll');
      await shot(page, c.name, 'degraded');
      assert.strictEqual(await hook(page, '/test/daemon?down=0'), 204);
      // the bead's stream retries a 502 by itself (web/live.js, from below a second), so the panel may load before the press
      await page.locator('#bv-body .bv-note.err button').click({ timeout: 4000 }).catch(function (e) { return page.evaluate(function () { return !document.querySelector('#bv-body .bv-note.err'); }).then(function (gone) { if (!gone) throw e; }); });
      await page.waitForFunction(function () { return Array.prototype.some.call(document.querySelectorAll('#bv-body .bv-sec h3'), function (h) { return h.textContent === 'Description'; }) && !document.querySelector('#bv-body .bv-note.err'); }, null, { timeout: 8000 });
      assert.deepStrictEqual(errors, [], c.name + ': console errors');
      await ctx.close();
      console.log('ok ' + c.name);
    }
  } finally {
    await browser.close();
  }
})().catch(function (e) { console.error(e && e.stack || String(e)); process.exit(1); });
