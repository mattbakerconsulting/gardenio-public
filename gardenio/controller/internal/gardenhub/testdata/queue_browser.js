/* Browser check for the Queues panel (Lands workbench-glass spec WB-6, step 5.5), run from TestQueuesUnderChromium against
   /garden on cardWorld, whose Queues reader is fixtureQueues (queue_test.go) and whose POST /b/{id}/decision/resolve
   answers 200. Usage: node queue_browser.js <base url> <node_modules dir>.

   Desktop: the Mailbox's "All queues" link names the Decisions tab (the Mailbox holds a decision) and opens the panel in
   place: Decisions, the longest-waiting row first; a lists change that concerns it refetches it, one that does not, nothing, and one while an answer is armed only says so. The Escalations tab shows the owner's one and the In hand line, no
   Approve for an account that may not approve; Everyone's puts all=1 in the address; the arrow keys walk the tabs and keep
   it. Answering a decision from its row (two taps) takes the row away and says so. A row's title opens its bead here
   (all=1 leaves the address with the queue) and Back returns to the queue; closing drops the queue from the address.
   Phone: the tab bar's Mailbox, then the link, opens the panel over the farm, and closing it returns to the Mailbox.
   No console errors. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));

function rows(page) { return page.evaluate(function () { return Array.prototype.map.call(document.querySelectorAll('#bv-body .qv-row'), function (li) { return li.dataset.id; }); }); }
function title(page) { return page.evaluate(function () { return document.getElementById('bv-title').textContent; }); }
function search(page) { return page.evaluate(function () { return location.search; }); }
async function tab(page, name) {
  await page.waitForFunction(function (n) { var t = document.getElementById('bv-title'); return t.textContent === n && document.querySelector('#bv-body .qv-list'); }, name);
}
function watch(page) {
  var errors = [];
  page.on('console', function (m) { if (m.type() === 'error') errors.push(m.text()); });
  page.on('pageerror', function (e) { errors.push(String(e)); });
  return errors;
}

async function desktop(browser) {
  var ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } }), page = await ctx.newPage(), errors = watch(page);
  await page.goto(url + '/garden');
  await page.waitForFunction(function () { return document.querySelector('#d-mail li') && window.WORKBENCH && WORKBENCH.has('queue'); });
  await page.click('#dbtn-mail');
  var link = page.locator('#mail-queues');
  await link.waitFor({ state: 'visible' });
  assert.ok(/\/garden\?queue=decisions$/.test(await link.getAttribute('href')), 'the Mailbox names the Decisions tab: ' + await link.getAttribute('href'));
  await link.click();
  await tab(page, 'Decisions');
  assert.ok(/queue=decisions/.test(await search(page)), 'the address names the panel');
  assert.deepStrictEqual(await rows(page), ['gd-FaceDecide1', 'gd-QueueDec001'], 'the longest-waiting first');
  assert.strictEqual(await page.locator('#bv-body .qv-row').first().locator('.qv-line b').textContent(), 'asked of you', 'why it needs the viewer, in words');
  assert.strictEqual(await page.locator('#bv-body [role=tab][aria-selected=true]').textContent(), 'Decisions');

  // live (gd-6I6gmi6851): a change to the page's lists that concerns the tab refetches it in place, one that does not, nothing
  var asked = 0;
  page.on('request', function (r) { if (/\/garden\/queue\/decisions/.test(r.url())) asked++; });
  function lists(d) { return page.evaluate(function (d) { document.dispatchEvent(new CustomEvent('garden:lists', { detail: d })); }, d); }
  await lists({ up: [{ id: 'gd-NotListed', sec: 'work' }], rm: ['gd-NotListed2'] });
  await page.waitForTimeout(300);
  assert.strictEqual(asked, 0, 'a change that does not concern the tab fetches nothing');
  await Promise.all([page.waitForResponse(/\/garden\/queue\/decisions/), lists({ rm: ['gd-QueueDec001'] })]);
  await tab(page, 'Decisions');
  assert.strictEqual(asked, 1, 'one refetch, for the change that concerns the tab');
  assert.deepStrictEqual(await rows(page), ['gd-FaceDecide1', 'gd-QueueDec001'], 'repainted from the fixture');

  await page.click('#bv-body [role=tab]:has-text("Escalations")');
  await tab(page, 'Escalations');
  assert.deepStrictEqual(await rows(page), ['gd-QueueEsc001']);
  assert.ok(/queue=escalations/.test(await search(page)), 'the tab is in the address');
  assert.strictEqual(await page.locator('#bv-body .qv-row .act').count(), 0, 'no Approve for an account that may not approve');
  assert.strictEqual(await page.locator('#bv-body .qv-row.needs').count(), 0, 'an escalation this account cannot approve is not styled as needing it');
  assert.strictEqual(await page.locator('#bv-body .qv-row .qv-who').textContent(), 'The owner approves or rejects this; your account cannot.', 'it says who can');
  assert.ok(/1 in hand by the desk/.test(await page.locator('#bv-body').textContent()), 'the In hand line');

  await page.click('#bv-body .qv-scope button:has-text("Everyone")');
  await page.waitForFunction(function () { return /all=1/.test(location.search) && document.querySelector('#bv-body .qv-scope [aria-pressed=true]').textContent !== 'Mine'; });
  await page.focus('#bv-body [role=tab][aria-selected=true]');
  await page.keyboard.press('ArrowLeft');
  await tab(page, 'Decisions');
  assert.ok(/all=1/.test(await search(page)), 'a tab keeps Everyone\'s');

  // answer the second decision from its row: the first tap arms, the second sends
  var ship = page.locator('#bv-body .qv-row[data-id="gd-QueueDec001"] [data-act=resolve][data-opt=ship]');
  await ship.click();
  assert.ok(/Confirm|again/.test(await ship.textContent()), 'the first tap arms');
  var was = asked;
  await lists({ rm: ['gd-FaceDecide1'] });   // armed: the refresh waits and says so
  await page.waitForFunction(function () { return /changed while you were writing/.test(document.querySelector('#bv-body .bv-note').textContent); });
  assert.strictEqual(asked, was, 'no refetch while an answer is armed');
  await ship.click();
  await page.waitForFunction(function () { return !document.querySelector('#bv-body .qv-row[data-id="gd-QueueDec001"]'); });
  assert.ok(/Answered gd-QueueDec001/.test(await page.locator('#toast').textContent()), 'the toast says so');
  assert.deepStrictEqual(await rows(page), ['gd-FaceDecide1']);

  // a row's title opens its bead here; Back returns to the queue
  await page.click('#bv-body .qv-row[data-id="gd-FaceDecide1"] a.qv-t');
  await page.waitForFunction(function () { return /bead=gd-FaceDecide1/.test(location.search) && document.getElementById('bead').dataset.kind === 'bead'; });
  assert.ok(!/all=1|queue=/.test(await search(page)), 'the bead\'s address carries no queue: ' + await search(page));
  await page.goBack();
  await tab(page, 'Decisions');
  assert.ok(/queue=decisions/.test(await search(page)) && /all=1/.test(await search(page)), 'Back is the queue as it was: ' + await search(page));

  await page.click('#bv-x');
  await page.waitForFunction(function () { return document.getElementById('bead').hidden; });
  assert.ok(!/queue=|all=1/.test(await search(page)), 'closing drops the queue: ' + await search(page));
  assert.deepStrictEqual(errors, [], 'no console errors');
  await ctx.close();
}

async function phone(browser) {
  var ctx = await browser.newContext({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true }), page = await ctx.newPage(), errors = watch(page);
  await page.goto(url + '/garden');
  await page.waitForFunction(function () { return document.querySelector('#d-mail li') && window.WORKBENCH && WORKBENCH.has('queue'); });
  await page.tap('#tabbar [data-go=mail]');
  await page.locator('#mail-queues').waitFor({ state: 'visible' });
  await page.tap('#mail-queues');
  await tab(page, 'Decisions');
  assert.ok(await page.locator('#bead').isVisible(), 'the panel is on screen over the farm');
  await page.tap('#bv-x');
  await page.waitForFunction(function () { return document.getElementById('bead').hidden && document.body.dataset.screen === 'mail'; });
  assert.deepStrictEqual(errors, [], 'no console errors');
  await ctx.close();
}

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  try { await desktop(browser); await phone(browser); console.log('queues browser checks passed'); }
  finally { await browser.close(); }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
