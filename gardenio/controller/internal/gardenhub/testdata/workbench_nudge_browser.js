/* The workbench's live head's Nudge (gd-1iJEc1nYqm), run from TestWorkbenchNudgeUnderChromium against the card world's
   Commons with the workbench roots in it (workbench_world_test.go). Usage: node workbench_nudge_browser.js <base url>
   <node_modules dir>.

   Opened at an agent's node, /garden/c/n/gd-wbCrew3, as an operator:
   - the page loads no page-nudge.js (web/glassact.js) until Nudge is clicked;
   - Nudge opens the nudge box in place, right under the head's live line, with the focus in its field, at the same address;
   - a second Nudge keeps the one box;
   - Send the nudge posts type=nudge and the words to the agent's /send, and says the garden's answer under it.
   Zero page errors. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); } catch (e) { console.error(String(e.message || e).split('\n')[0]); process.exit(3); }
  var page = await browser.newPage({ viewport: { width: 1280, height: 800 } }), errs = [], scripts = [], sent = [];
  page.on('pageerror', function (e) { errs.push(String(e)); });
  page.on('request', function (r) { if (r.resourceType() === 'script') scripts.push(r.url()); });
  await page.route('**/garden/glass/*/send', function (route) {
    var r = route.request();
    sent.push({ path: new URL(r.url()).pathname, body: r.postData() });
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ ok: true, result: 'sent', note: 'Sent to gardenio-crew-S3.' }) });
  });

  await page.goto(url + '/garden/c/n/gd-wbCrew3');
  await page.waitForSelector('#wb2 .wb2-head .cm-node-live .cm-nudge', { timeout: 10000 });
  var nudgeJs = await page.evaluate(function () { return document.body.dataset.nudgeJs || ''; });
  assert.ok(nudgeJs, 'an operator\'s page names page-nudge.js');
  assert.ok(!scripts.some(function (s) { return s.indexOf(nudgeJs) >= 0; }) && await page.evaluate(function () { return typeof GLASSACT === 'undefined'; }), 'no nudge box code on a cold load');

  var nudge = page.locator('#wb2 .wb2-head .cm-node-live .cm-nudge');
  await nudge.click();
  await page.waitForSelector('#wb2 .wb2-head .cm-node-live + form.gl-nudge textarea', { timeout: 10000 });
  assert.strictEqual(await page.evaluate(function () { return location.pathname; }), '/garden/c/n/gd-wbCrew3', 'the nudge opens in place');
  assert.ok(await page.evaluate(function () { return document.activeElement.matches('form.gl-nudge textarea'); }), 'the focus is in the nudge field');
  assert.match(await page.locator('#wb2 form.gl-nudge').getAttribute('aria-label'), /^Nudge gardenio-crew-S3$/);
  assert.strictEqual(await nudge.getAttribute('aria-expanded'), 'true');
  await nudge.click();
  assert.strictEqual(await page.locator('#wb2 form.gl-nudge').count(), 1, 'a second Nudge keeps the one box');

  await page.locator('#wb2 form.gl-nudge textarea').fill('Look at the failing test first');
  await page.locator('#wb2 form.gl-nudge button[type=submit]').click();
  await page.waitForFunction(function () { var n = document.querySelector('#wb2 form.gl-nudge .gl-sent'); return n && /Sent to/.test(n.textContent); }, null, { timeout: 5000 });
  assert.strictEqual(sent.length, 1, 'one post');
  assert.strictEqual(sent[0].path, '/garden/glass/gd-wbCrew3/send');
  var form = new URLSearchParams(sent[0].body);
  assert.strictEqual(form.get('type'), 'nudge');
  assert.strictEqual(form.get('message'), 'Look at the failing test first');
  assert.strictEqual(await page.locator('#wb2 form.gl-nudge textarea').inputValue(), '', 'a sent nudge clears its field');

  assert.deepStrictEqual(errs, [], 'page errors');
  await browser.close();
  console.log('workbench nudge: lazy, in place, one box, sent');
})().catch(function (e) { console.error(e); process.exit(1); });
