/* Browser check for the plan cockpit (Lands workbench-glass spec WB-7 and WB-8, step 5.9), run from TestPlanUnderChromium
   against /garden on cardWorld, whose cockpit reader is fixturePlans (plan_test.go). Usage:
   node plan_browser.js <base url> <node_modules dir>.

   Desktop: plan A's bead view offers Open the cockpit, which opens the panel on ?plan=gd-planA (kind plan, its title):
   where the plan stands and its progress in words; what it waits on, the human asks first; its steps by phase in order,
   each with its words; the decision with the Mailbox's answer box; the crew; the narrative's markdown built as elements.
   A step's title opens its bead here and Back returns to the cockpit; Escape closes and drops plan from the address. The
   cardWorld viewer may not write, so no lever or form is offered. Phone: /garden?plan=gd-planA opens the sheet over the
   farm. No console errors. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));

function search(page) { return page.evaluate(function () { return location.search; }); }
function text(page, sel) { return page.locator(sel).first().textContent(); }
async function shown(page) {
  await page.waitForFunction(function () {
    var p = document.getElementById('bead');
    return !p.hidden && p.dataset.kind === 'plan' && !!document.querySelector('#bv-body .pv-steps');
  });
}
function watch(page) {
  var errors = [];
  page.on('console', function (m) { if (m.type() === 'error') errors.push(m.text()); });
  page.on('pageerror', function (e) { errors.push(String(e)); });
  return errors;
}

async function desktop(browser) {
  var ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } }), page = await ctx.newPage(), errors = watch(page);
  await page.goto(url + '/garden?bead=gd-planA');
  await page.waitForFunction(function () { return window.WORKBENCH && window.WORKBENCH.has('plan') && document.getElementById('bead').dataset.kind === 'bead'; });

  // the plan's face offers the cockpit
  var open = page.locator('#bv-body a.btn', { hasText: 'Open the cockpit' });
  await open.waitFor({ state: 'visible' });
  await open.click();
  await shown(page);
  assert.ok(/(^|[?&])plan=gd-planA($|&)/.test(await search(page)), 'the address names the cockpit: ' + await search(page));
  assert.ok(!/(^|[?&])bead=/.test(await search(page)), 'one panel at a time: ' + await search(page));
  assert.strictEqual(await text(page, '#bead .bv-kind'), 'plan');
  assert.strictEqual(await text(page, '#bv-title'), 'Edge IdP rollout');
  assert.strictEqual(await text(page, '#bv-body .pv-state'), 'Building.');
  assert.ok(/^2 of 9 steps closed \(22%\) · gardenio · changed 20m ago$/.test(await text(page, '#bv-body .pv-meta .qv-line')), 'progress in words');

  // what it waits on: the decision, the escalation, then the step that waits on another
  var waits = await page.evaluate(function () { return Array.prototype.map.call(document.querySelectorAll('#bv-body .pv-waits li'), function (li) { return li.className + ':' + li.querySelector('.wt').textContent; }); });
  assert.deepStrictEqual(waits, ['ask:decision asked of you', 'ask:gitlab mr merge for the owner to approve', 'wtg:Flip production to the new IdP waits on']);

  // the steps by phase, in order
  var groups = await page.evaluate(function () { return Array.prototype.map.call(document.querySelectorAll('#bv-body .pv-group'), function (g) { var t = g.querySelector('.pv-gt'); return (t ? t.textContent : '') + ':' + g.querySelectorAll('.pv-step').length; }); });
  assert.deepStrictEqual(groups, ['Foundations:4', 'Rollout:4', 'Cutover:0', 'Other steps:1']);
  assert.strictEqual(await page.locator('#bv-body .pv-step[data-id="gd-A1a"] .qv-line').textContent(), 'in progress · mo · MR opened · 20m ago');
  assert.strictEqual(await page.locator('#bv-body .pv-step[data-id="gd-A2a"] .qv-line').textContent(), 'open · waits on 2 beads · 5h ago');
  assert.strictEqual(await page.locator('#bv-body .pv-step[data-id="gd-A1a"] .pv-n').textContent(), '#1');

  // the decision carries the Mailbox's answer box; no writes for a viewer who may not
  assert.ok(await page.locator('#bv-body .qv-row[data-id="gd-A2f"] .act[data-kind="decision"]').isVisible(), 'the answer box');
  assert.strictEqual(await page.locator('#bv-body .pv-levers, #bv-body .pv-form, #bv-body .pv-pour, #bv-body [data-pv]').count(), 0, 'no levers or forms without edit');

  // the crew and the narrative
  assert.ok(/3 agents: 2 working, 1 done\./.test(await page.locator('#bv-body').textContent()), 'the crew in words');
  assert.strictEqual(await page.locator('#bv-body .pv-prose .bv-md strong').first().textContent(), 'the why', 'markdown built as elements');
  assert.strictEqual(await page.locator('#bv-body .pv-prose .bv-md li').count(), 2, 'the design doc\'s list');

  // a step opens its bead here; Back returns to the cockpit
  await page.click('#bv-body .pv-step[data-id="gd-A1b"] a.qv-t');
  await page.waitForFunction(function () { return /bead=gd-A1b/.test(location.search) && document.getElementById('bead').dataset.kind === 'bead'; });
  await page.goBack();
  await shown(page);
  assert.ok(/(^|[?&])plan=gd-planA($|&)/.test(await search(page)), 'Back is the cockpit: ' + await search(page));

  // Escape closes, and plan leaves the address
  await page.focus('#bv-title');
  await page.keyboard.press('Escape');
  await page.waitForFunction(function () { return document.getElementById('bead').hidden; });
  assert.ok(!/(^|[?&])plan=/.test(await search(page)), 'closing drops plan: ' + await search(page));

  // an id with no cockpit says so, in words
  await page.goto(url + '/garden?plan=gd-A1a');
  await page.waitForFunction(function () { return document.getElementById('bead').dataset.kind === 'plan' && /There is no plan gd-A1a/.test(document.getElementById('bv-body').textContent); });

  assert.deepStrictEqual(errors.filter(function (e) { return !/404/.test(e); }), [], 'no console errors but the 404 asked for');
  await ctx.close();
}

async function phone(browser) {
  var ctx = await browser.newContext({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true }), page = await ctx.newPage(), errors = watch(page);
  await page.goto(url + '/garden?plan=gd-planA');
  await shown(page);
  assert.ok(await page.locator('#bead').isVisible(), 'the sheet is on screen over the farm');
  assert.strictEqual(await text(page, '#bv-title'), 'Edge IdP rollout');
  await page.tap('#bv-x');
  await page.waitForFunction(function () { return document.getElementById('bead').hidden; });
  assert.deepStrictEqual(errors, [], 'no console errors');
  await ctx.close();
}

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  try { await desktop(browser); await phone(browser); console.log('plan browser checks passed'); }
  finally { await browser.close(); }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
