/* The Commons v2 workbench kept instant (plan gd-lRaCxsiLvC step 6, gd-Txos7COlj0), run from TestWorkbenchFastUnderChromium
   against workbenchRig (workbench_world_test.go). Usage: node workbench_fast_browser.js <base url> <node_modules dir>.

   On a desktop, opened at /garden/c/n/gd-wbPlan:
   - hovering a row fetches its node and its document before it is clicked, and the click fetches neither again;
   - the draft in its document's composer (the bead view's, gd-0GnBi7SRfK) stays with its node across a switch and back;
   - a node seen before opens again with no fetch of the node, its document or its thread;
   - a send paints at once, pending, before the server answers, and settles into one row when it does; pressing Send leaves
     the focus in the composer;
   - a refused send offers Retry, which sends it again and settles;
   - a message in a thread not on screen marks the rows of that thread new, and opening one clears it;
   - a root opened again shows the tree kept for it, in place, with the focus on it.
   Zero page errors. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); } catch (e) { console.error(String(e.message || e).split('\n')[0]); process.exit(3); }
  var page = await browser.newPage({ viewport: { width: 1280, height: 800 } }), errs = [], reqs = [];
  page.on('pageerror', function (e) { errs.push(String(e)); });
  page.on('request', function (r) { reqs.push(r.method() + ' ' + new URL(r.url()).pathname); });
  var count = function (re) { return reqs.filter(function (x) { return re.test(x); }).length; };
  var until = async function (fn, what) {
    for (var end = Date.now() + 10000; Date.now() < end; await page.waitForTimeout(25)) if (fn()) return;
    assert.fail('timed out: ' + what);
  };
  var shown = async function (id, root) {
    await page.waitForFunction(function (a) {
      var r = document.querySelector('#wb2 .wb2-row[aria-selected=true]'), d = document.querySelector('#wb2 .wb2-doc');
      return r && r.dataset.id === a.id && (!a.root || COMMONS2.state().root === a.root) && d && !d.hasAttribute('aria-busy') && d.querySelector('.wb2-head');
    }, { id: id, root: root || '' }, { timeout: 10000 });
  };
  var row = function (id) { return '#wb2 .wb2-row[data-id=' + id + '] > .wb2-line .wb2-t'; };
  var input = '#wb2 .wb2-bead .bv-cv-say textarea', pending = '#wb2 .bv-cv-list > li .bv-cv-st';
  var rows = function (t) { return page.evaluate(function (t) { return [].filter.call(document.querySelectorAll('#wb2 .bv-cv-list > li'), function (li) { return li.textContent.indexOf(t) >= 0; }).map(function (li) { return li.className + (li.querySelector('.bv-cv-st') ? ' pending' : '') + (li.dataset.id ? ' #' : ''); }); }, t); };
  // a send's POST, held until hold.go() (or aborted), for the sends below
  var hold = null;
  await page.route(function (u) { return /\/garden\/c\/t\/gd-wishPorch\/reply$/.test(u.pathname); }, async function (route) {
    var h = route.request().method() === 'POST' && hold;
    if (!h) return route.continue();
    hold = null;
    if (h.abort) return route.abort();
    await h.wait;
    return route.continue();
  });
  var held = function () { var go; var h = { wait: new Promise(function (ok) { go = ok; }) }; h.go = go; return (hold = h); };

  await page.goto(url + '/garden/c/n/gd-wbPlan');
  await shown('gd-wbPlan', 'gd-wbPlan');

  // prefetch on hover
  await page.hover(row('gd-wbS4'));
  await until(function () { return count(/^GET \/garden\/c\/n\/gd-wbS4$/) === 1 && count(/^GET \/garden\/bead\/gd-wbS4$/) === 1; }, 'hover fetches the row and its document');
  await page.click(row('gd-wbS4'));
  await shown('gd-wbS4');
  assert.strictEqual(count(/^GET \/garden\/c\/n\/gd-wbS4$/), 1, 'the click fetched no node more');
  assert.strictEqual(count(/^GET \/garden\/bead\/gd-wbS4$/), 1, 'the click fetched no document more');
  await page.waitForSelector(input);

  // the draft
  await page.locator(input).pressSequentially('draft for S4');
  await page.click(row('gd-wbS3'));
  await shown('gd-wbS3');
  await page.waitForSelector(input);
  assert.strictEqual(await page.locator(input).inputValue(), '', 'another node has its own (empty) draft');
  await page.click(row('gd-wbPhase1'));   // a node with no thread, seen once
  await shown('gd-wbPhase1');
  var before = reqs.length;
  await page.click(row('gd-wbS4'));
  await shown('gd-wbS4');
  assert.strictEqual(await page.locator(input).inputValue(), 'draft for S4', 'the draft came back with its node');
  // warm: away and back again, nothing fetched for either node or the thread
  await page.click(row('gd-wbPhase1'));
  await shown('gd-wbPhase1');
  await page.click(row('gd-wbS4'));
  await shown('gd-wbS4');
  await page.waitForSelector(input);
  assert.deepStrictEqual(reqs.slice(before).filter(function (x) { return /\/garden\/(c\/)?(n|bead)\/|\/t\/gd-wishPorch$/.test(x); }), [], 'warm switches fetch nothing');

  // an optimistic send: on screen, pending, before the server answers; one row once it does
  var said = 'Optimistic ' + Date.now(), h = held();
  await page.locator(input).fill(said);
  await page.locator(input).press('Enter');
  assert.deepStrictEqual(await rows(said), ['bv-cv bv-cv--person pending'], 'the send is on screen while the server holds it');
  assert.match(await page.locator(pending).textContent(), /Sending/);
  assert.strictEqual(await page.locator(input).inputValue(), '', 'Send empties the composer at once');
  h.go();
  await page.waitForFunction(function (p) { return !document.querySelector(p); }, pending, { timeout: 10000 });
  await page.waitForTimeout(500);   // the stream's copy, which must settle rather than repeat it
  assert.strictEqual((await rows(said)).length, 1, 'it settles into one row');
  assert.match((await rows(said))[0], / #$/, 'the settled row is the server\'s');

  // Send, pressed: the focus stays in the composer
  said = 'Pressed ' + Date.now();
  await page.locator(input).fill(said);
  await page.click('#wb2 .wb2-bead .bv-cv-say button');
  assert.ok(await page.evaluate(function () { return document.activeElement.matches('.bv-cv-say textarea'); }), 'Send leaves the focus in the composer');
  await page.waitForFunction(function (p) { return !document.querySelector(p); }, pending, { timeout: 10000 });
  assert.strictEqual((await rows(said)).length, 1, 'the pressed send settles into one row');

  // refused: Retry sends it again
  said = 'Refused ' + Date.now(); hold = { abort: true };
  await page.locator(input).fill(said);
  await page.locator(input).press('Enter');
  await page.waitForSelector('#wb2 .bv-cv-list .bv-cv-retry', { timeout: 6000 });
  assert.match(await page.locator(pending).textContent(), /^Not sent: /);
  await page.click('#wb2 .bv-cv-list .bv-cv-retry');
  await page.waitForFunction(function (p) { return !document.querySelector(p); }, pending, { timeout: 10000 });
  await page.waitForTimeout(500);
  assert.strictEqual((await rows(said)).length, 1, 'Retry settles into one row');

  // unread from the stream: on a node with no thread, a message in gd-wishPorch marks its rows new; opening one clears it
  await page.click(row('gd-wbPhase1'));
  await shown('gd-wbPhase1');
  await page.waitForFunction(function () { return !/channel=/.test(COMMONS2.state().stream); });
  var heard = 'Heard ' + Date.now(), marked = false;
  for (var n = 0; n < 5 && !marked; n++) {
    await page.request.post(url + '/test/wb/say?' + new URLSearchParams({ thread: 'gd-wishPorch', text: heard }));
    marked = await page.waitForSelector('#wb2 .wb2-row[data-id=gd-wbS4] > .wb2-line .wb2-new', { timeout: 2000 }).then(function () { return true; }, function () { return false; });
  }
  assert.ok(marked, 'a message in its thread marks the row new');
  assert.strictEqual(await page.locator('#wb2 .wb2-row[data-id=gd-wbPhase1] > .wb2-line .wb2-new').count(), 0, 'a row of another thread is not marked');
  await page.click(row('gd-wbS4'));
  await shown('gd-wbS4');
  assert.strictEqual(await page.locator('#wb2 .wb2-row[data-id=gd-wbS4] > .wb2-line .wb2-new').count(), 0, 'opening it clears the mark');
  await page.waitForFunction(function (t) { return [].some.call(document.querySelectorAll('#wb2 .bv-cv-list > li'), function (li) { return li.textContent.indexOf(t) >= 0; }); }, heard, { timeout: 10000 });

  // Open here and Back: the plan's tree is the one kept, shown again in place, and one tree is shown at a time
  await page.evaluate(function () { document.querySelector('#wb2 .wb2-tree:not(.wb2-tree--kept)').dataset.seen = 'plan'; });
  await page.locator('#wb2 .wb2-tree:not(.wb2-tree--kept)').focus();
  await page.keyboard.press('Enter');
  await shown('gd-wbS4', 'gd-wbS4');
  await page.goBack();
  await shown('gd-wbS4', 'gd-wbPlan');
  assert.deepStrictEqual(await page.evaluate(function () { return [].map.call(document.querySelectorAll('#wb2 .wb2-tree:not(.wb2-tree--kept)'), function (t) { return t.dataset.seen || ''; }); }), ['plan'], 'the kept tree came back');
  assert.ok(await page.evaluate(function () { return document.activeElement.classList.contains('wb2-tree') && !document.activeElement.inert; }), 'the focus is on the tree shown');
  assert.deepStrictEqual(await page.evaluate(function () { return [].map.call(document.querySelectorAll('#wb2 [role=treeitem][aria-selected=true]'), function (r) { return r.dataset.id; }); }), ['gd-wbS4'], 'one row is selected in the page, in the tree shown');

  assert.deepStrictEqual(errs, [], 'page errors');
  await browser.close();
  console.log('workbench fast: prefetch, drafts, warm switches, optimistic send, Retry and unread ok');
})().catch(function (e) { console.error(e); process.exit(1); });
