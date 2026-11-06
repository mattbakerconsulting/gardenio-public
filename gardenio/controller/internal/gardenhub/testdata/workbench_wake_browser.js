/* The workbench's agent head's Wake / Restart (gd-7DyHdnx8fC), run from TestWorkbenchWakeUnderChromium against the card
   world's Commons, with the farm's down gardener gd-gDownB in it (workbench_world_test.go). Usage: node
   workbench_wake_browser.js <base url> <node_modules dir>.

   As a viewer who may restart gardeners (X-UX-Waker: 1):
   - a working agent's node (gd-wbCrew3) has no reason line and loads no page-wake.js (web/wake.js);
   - the down gardener's head says its reason and offers Restart…; pressing it asks once, naming the agent and its task,
     with the focus on the Restart that confirms; Cancel goes back to Restart…, focused;
   - a refusal is said in place, and Restart… is offered again, focused;
   - then the restart is posted to /garden/gardener/gd-gDownB/wake and said in place, with no second button.
   As one who may not: the reason is read, nothing is offered and nothing names page-wake.js.
   Zero page errors. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); } catch (e) { console.error(String(e.message || e).split('\n')[0]); process.exit(3); }
  var ctx = await browser.newContext({ viewport: { width: 1280, height: 800 }, extraHTTPHeaders: { 'X-UX-Waker': '1' } });
  var page = await ctx.newPage(), errs = [], scripts = [], posts = [];
  page.on('pageerror', function (e) { errs.push(String(e)); });
  page.on('request', function (r) { if (r.resourceType() === 'script') scripts.push(r.url()); });
  var answers = [{ status: 409, body: { ok: false, error: 'g-down-b is stopping.' } }, { status: 200, body: { ok: true, agent: 'g-down-b' } }];
  await page.route('**/garden/gardener/*/wake', function (route) {
    var r = route.request(), a = answers[Math.min(posts.length, answers.length - 1)];
    posts.push({ method: r.method(), path: new URL(r.url()).pathname });
    route.fulfill({ status: a.status, contentType: 'application/json', body: JSON.stringify(a.body) });
  });

  await page.goto(url + '/garden/c/n/gd-wbCrew3');
  await page.waitForSelector('#wb2 .wb2-head .cm-node-live', { timeout: 10000 });
  var wakeJs = await page.evaluate(function () { return document.body.dataset.wakeJs || ''; });
  assert.ok(wakeJs && await page.evaluate(function () { return document.body.dataset.wake === '1'; }), 'a viewer who may restart is told so');
  assert.strictEqual(await page.locator('#wb2 .cm-node-reason, #wb2 .wb2-wake').count(), 0, 'a working agent has no reason and no button');
  assert.ok(!scripts.some(function (s) { return s.indexOf(wakeJs) >= 0; }) && await page.evaluate(function () { return typeof WAKE === 'undefined'; }), 'no wake code on a cold load');

  await page.goto(url + '/garden/c/n/gd-gDownB');
  var restart = page.locator('#wb2 .wb2-head .wb2-wake button.gwake');
  await restart.waitFor({ timeout: 10000 });
  assert.strictEqual((await page.locator('#wb2 .wb2-head .cm-node-reason').textContent()).trim(), 'Failed', 'the farm\'s reason, in the head');
  assert.strictEqual(await restart.textContent(), 'Restart…');
  assert.ok(await page.evaluate(function () { var r = document.querySelector('#wb2 .cm-node-reason'); return r.nextElementSibling.matches('.wb2-wake'); }), 'the button is under the reason');

  await restart.click();
  var yes = page.locator('#wb2 .wb2-wake button.gwake-yes');
  await yes.waitFor({ timeout: 5000 });
  assert.strictEqual(await page.locator('#wb2 .wb2-wake [role=status]').first().textContent(), 'Restart g-down-b? It will resume its task Step one of plan B.');
  assert.strictEqual(await yes.textContent(), 'Restart');
  assert.ok(await page.evaluate(function () { return document.activeElement.matches('.wb2-wake button.gwake-yes'); }), 'the focus is on the Restart that confirms');
  await page.keyboard.press('Tab');
  assert.ok(await page.evaluate(function () { return document.activeElement.matches('.wb2-wake button.gwake-no'); }), 'Cancel is the next stop');
  await page.keyboard.press('Enter');
  await restart.waitFor({ timeout: 5000 });
  assert.ok(await page.evaluate(function () { return document.activeElement.matches('.wb2-wake button.gwake'); }), 'Cancel puts the focus back on Restart…');
  assert.strictEqual(posts.length, 0, 'asking posts nothing');

  await restart.click(); await yes.click();
  await page.waitForFunction(function () { var n = document.querySelector('#wb2 .wb2-wake .err'); return n && /stopping/.test(n.textContent); }, null, { timeout: 5000 });
  assert.strictEqual(await page.locator('#wb2 .wb2-wake .err').textContent(), 'g-down-b is stopping.', 'the refusal, in place');
  assert.ok(await page.evaluate(function () { return document.activeElement.matches('.wb2-wake button.gwake'); }), 'refused: Restart… again, focused');

  await restart.click(); await yes.click();
  await page.waitForFunction(function () { var w = document.querySelector('#wb2 .wb2-wake'); return w && /^Restarted\./.test(w.textContent); }, null, { timeout: 5000 });
  assert.strictEqual(await page.locator('#wb2 .wb2-wake').textContent(), 'Restarted. It shows as starting on the next update.');
  assert.strictEqual(await page.locator('#wb2 .wb2-wake button').count(), 0, 'restarted: no second button');
  assert.deepStrictEqual(posts, [{ method: 'POST', path: '/garden/gardener/gd-gDownB/wake' }, { method: 'POST', path: '/garden/gardener/gd-gDownB/wake' }]);
  assert.strictEqual(await page.evaluate(function () { return location.pathname; }), '/garden/c/n/gd-gDownB', 'all in place');

  var other = await (await browser.newContext({ viewport: { width: 1280, height: 800 } })).newPage();
  other.on('pageerror', function (e) { errs.push(String(e)); });
  await other.goto(url + '/garden/c/n/gd-gDownB');
  await other.waitForSelector('#wb2 .wb2-head .cm-node-reason', { timeout: 10000 });
  assert.ok(await other.evaluate(function () { return !document.body.dataset.wake && !document.body.dataset.wakeJs; }), 'a viewer who may not is not told of it');
  assert.strictEqual(await other.locator('#wb2 .wb2-wake').count(), 0, 'and is offered nothing');

  assert.deepStrictEqual(errs, [], 'page errors');
  await browser.close();
  console.log('workbench wake: reason in the head, lazy, asked once, refusal and restart in place, admin only');
})().catch(function (e) { console.error(e); process.exit(1); });
