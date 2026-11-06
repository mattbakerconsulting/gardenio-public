/* The workbench's optimistic send at its edges (plan gd-lRaCxsiLvC step 6 follow-up, gd-JpYV1FWtnZ), run from
   TestWorkbenchSendUnderChromium against workbenchRig (workbench_world_test.go). Usage: node workbench_send_browser.js <base url>
   <node_modules dir>.

   On a desktop, on gd-wbS4 (its thread is #general's gd-wishPorch), in its document's Conversation composer (the bead
   view's, web/beadtalk.js, gd-0GnBi7SRfK):
   - a send held past 3 s offers Retry; Retry while the first attempt is still on its way, then both answered, is one message
     bead and one row;
   - a send refused after its node was left says so (Retry) when the node is open again, having written nothing, and its
     Retry settles it;
   - a send held after its node was left says so (Retry) when the node is open again, and settles when the answer comes.
   Zero page errors. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); } catch (e) { console.error(String(e.message || e).split('\n')[0]); process.exit(3); }
  var page = await browser.newPage({ viewport: { width: 1280, height: 800 } }), errs = [], ended = 0;
  page.on('pageerror', function (e) { errs.push(String(e)); });
  var reply = /\/garden\/c\/t\/gd-wishPorch\/reply$/;
  var done = function (r) { if (r.method() === 'POST' && reply.test(new URL(r.url()).pathname)) ended++; };
  page.on('requestfinished', done); page.on('requestfailed', done);
  var shown = async function (id) {
    await page.waitForFunction(function (id) {
      var r = document.querySelector('#wb2 .wb2-row[aria-selected=true]'), d = document.querySelector('#wb2 .wb2-doc');
      return r && r.dataset.id === id && d && !d.hasAttribute('aria-busy') && d.querySelector('.wb2-head');
    }, id, { timeout: 10000 });
  };
  var open = async function (id) { await page.click('#wb2 .wb2-row[data-id=' + id + '] > .wb2-line .wb2-t'); await shown(id); };
  var input = '#wb2 .wb2-bead .bv-cv-say textarea', retry = '#wb2 .bv-cv-list .bv-cv-retry';
  // each row with t: its pending line ('' once settled), and # when it is the server's
  var rows = function (t) { return page.evaluate(function (t) { return [].filter.call(document.querySelectorAll('#wb2 .bv-cv-list > li'), function (li) { return li.textContent.indexOf(t) >= 0; }).map(function (li) { var st = li.querySelector('.bv-cv-st'); return (st ? st.textContent : '') + (li.dataset.id ? ' #' : ''); }); }, t); };
  var sent = async function (t) { return +(await (await page.request.get(url + '/test/wb/sent?' + new URLSearchParams({ text: t }))).text()); };
  var settled = async function () { await page.waitForFunction(function () { return !document.querySelector('#wb2 .bv-cv-list .bv-cv-st'); }, null, { timeout: 10000 }); await page.waitForTimeout(500); };
  // the next send's POST, held until h.go(); refused then when h.refuse
  var hold = null;
  await page.route(function (u) { return reply.test(u.pathname); }, async function (route) {
    var h = route.request().method() === 'POST' && hold;
    if (!h) return route.continue();
    hold = null;
    await h.wait;
    return h.refuse ? route.abort() : route.continue();
  });
  var held = function (refuse) { var go; var h = { wait: new Promise(function (ok) { go = ok; }), refuse: !!refuse }; h.go = go; return (hold = h); };
  var send = async function (t) { await page.locator(input).fill(t); await page.locator(input).press('Enter'); };

  await page.goto(url + '/garden/c/n/gd-wbPlan');
  await shown('gd-wbPlan');
  await open('gd-wbS4');
  await page.waitForSelector(input);

  // Retry while the first attempt is still on its way: one message
  var said = 'Retried ' + Date.now(), h = held(), at = ended;
  await send(said);
  await page.waitForSelector(retry, { timeout: 6000 });
  await page.click(retry);
  for (var end = Date.now() + 10000; ended < at + 1 && Date.now() < end;) await page.waitForTimeout(25);
  assert.strictEqual(ended, at + 1, 'the Retry was answered');
  h.go();
  for (end = Date.now() + 10000; ended < at + 2 && Date.now() < end;) await page.waitForTimeout(25);
  assert.strictEqual(ended, at + 2, 'the first attempt was answered');
  await settled();
  assert.strictEqual(await sent(said), 1, 'one message bead for a send and its Retry');
  var got = await rows(said);
  assert.deepStrictEqual(got, [' #'], 'one row, the server\'s');

  // refused after its node was left: open again, it says so with Retry, nothing written; Retry settles it
  said = 'Refused away ' + Date.now(); h = held(true);
  await send(said);
  assert.deepStrictEqual(await rows(said), ['Sending…'], 'the send is on screen, pending');
  await open('gd-wbPhase1');   // a node with no thread
  at = ended; h.go();
  for (end = Date.now() + 10000; ended < at + 1 && Date.now() < end;) await page.waitForTimeout(25);
  await page.waitForTimeout(100);
  await open('gd-wbS4');
  await page.waitForSelector(input);
  got = await rows(said);
  assert.ok(got.length === 1 && /^Not sent: /.test(got[0]), 'it says it was not sent: ' + got);
  assert.strictEqual(await sent(said), 0, 'nothing was written');
  await page.click(retry);
  await settled();
  assert.deepStrictEqual(await rows(said), [' #'], 'Retry settles it into the server\'s row');
  assert.strictEqual(await sent(said), 1, 'one message bead');

  // held after its node was left: open again, it offers Retry; the answer then settles it
  said = 'Held away ' + Date.now(); h = held();
  await send(said);
  await open('gd-wbPhase1');
  await page.waitForTimeout(3300);
  await open('gd-wbS4');
  assert.deepStrictEqual(await rows(said), ['Not sent yet.'], 'it says it is not sent yet');
  assert.strictEqual(await page.locator(retry).count(), 1, 'with Retry');
  h.go();
  await settled();
  assert.deepStrictEqual(await rows(said), [' #'], 'it settles into the server\'s row');
  assert.strictEqual(await sent(said), 1, 'one message bead');

  assert.deepStrictEqual(errs, [], 'page errors');
  await browser.close();
  console.log('workbench send: Retry while slow, refused and held after leaving the node ok');
})().catch(function (e) { console.error(e); process.exit(1); });
