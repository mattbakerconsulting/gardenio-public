/* The connection lost and back on a phone (M3.2 of the Lands mobile pass, gd-Xo3Stftjv6; rubric C10), run from
   TestSleepWakeUnderChromium against /garden on sleepWorld (sleepwake_browser_test.go), whose /test/sleep/down and /up take
   every connection down and back. Usage: node sleepwake_browser.js <base url> <node_modules dir>. GARDENHUB_SHOTS=<dir>
   saves a PNG of each offline line.

   On an iPhone-sized page (web/live.js, the page's one connection):
   1. the connection down: past the grace the page says, in words, that it is reconnecting and since when, the farm's live
      dot says so too; once the connection is back the line goes and the dot is live again;
   2. the browser offline: the line says offline at once; online again, the page reconnects at once (no backoff's wait).
   No console errors but the 502s and lost connections asked for. The Commons stream's replay and unread counts were the
   band's (gone, gd-ZeiSZV7j2Y); the workbench's stream is its own page's. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert'), path = require('path');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));
var shots = process.env.GARDENHUB_SHOTS || '';

function post(page, q) { return page.evaluate(function (q) { return fetch('/test/sleep/' + q, { method: 'POST' }).then(function (r) { return r.status; }); }, q); }
/* the farm's live dot connected (web/garden.js: the hub's stream open) */
function liveOn(page, ms) { return page.waitForSelector('#live.on', { state: 'attached', timeout: ms || 15000 }); }
function bar(page) { return page.evaluate(function () { var b = document.getElementById('netbar'); return b.hidden ? null : b.textContent; }); }
async function shot(page, name) { if (shots) await page.screenshot({ path: path.join(shots, 'sleepwake-' + name + '.png') }); }

async function walk(browser) {
  var dev = Object.assign({}, pw.devices['iPhone 12'], { viewport: { width: 390, height: 844 }, timezoneId: 'UTC' });
  var ctx = await browser.newContext(Object.assign({}, dev, { reducedMotion: 'reduce' }));
  var page = await ctx.newPage(), errors = [];
  page.on('pageerror', function (e) { errors.push(String(e)); });
  page.on('console', function (m) { if (m.type() === 'error' && !/502|ERR_INTERNET_DISCONNECTED|ERR_CONNECTION|ERR_INCOMPLETE_CHUNKED_ENCODING|EventSource|Failed to fetch/.test(m.text())) errors.push(m.text()); });
  await page.goto(url + '/garden');
  await liveOn(page);
  // the bundles the page loads unasked, in before the outage: a script answered 502 is a refused script, not a lost connection
  await page.waitForFunction(function () { return !window.LAZY || LAZY.loaded(); }, null, { timeout: 15000 });
  assert.strictEqual(await bar(page), null, 'connected: no offline line');

  // 1. the connection down, the page visible
  await post(page, 'down');
  await page.waitForFunction(function () { return !document.getElementById('netbar').hidden; }, null, { timeout: 10000 });
  assert.match(await bar(page), /^Reconnecting · as of \d\d:\d\d$/, 'down past the grace, said in words');
  assert.ok(await page.locator('#live.off').count() === 1, "the farm's live dot says so too");
  await shot(page, 'reconnecting');
  await post(page, 'up');
  await page.waitForFunction(function () { return document.getElementById('netbar').hidden; }, null, { timeout: 40000 });
  await liveOn(page, 40000);

  // 2. the browser offline
  await ctx.setOffline(true);
  await page.waitForFunction(function () { return !document.getElementById('netbar').hidden; }, null, { timeout: 5000 });
  assert.match(await bar(page), /^Offline · as of \d\d:\d\d$/, 'offline, said at once');
  await shot(page, 'offline');
  await page.waitForTimeout(3000);   // long enough for a backoff to have grown
  var t0 = Date.now();
  await ctx.setOffline(false);
  await page.waitForFunction(function () { return document.getElementById('netbar').hidden; }, null, { timeout: 15000 });
  assert.ok(Date.now() - t0 < 8000, 'online again, the page reconnects without a backoff\'s wait: ' + (Date.now() - t0) + ' ms');
  assert.deepStrictEqual(errors, [], 'page errors');
  await ctx.close();
}

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); } catch (e) { console.error(String(e)); process.exit(3); }
  try { await walk(browser); console.log('sleep and wake: ok'); } finally { await browser.close(); }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
