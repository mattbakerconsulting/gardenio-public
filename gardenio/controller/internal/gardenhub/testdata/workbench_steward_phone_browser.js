/* The steward on a phone (steward view S3, gd-qV1nCdpbVA), run from TestWorkbenchStewardPhoneUnderChromium against the
   workbench rig with gardenio's steward in it (workbench_world_test.go wbStewards). Usage: node
   workbench_steward_phone_browser.js <base url> <node_modules dir>.

   At 390 x 844 with touch:
   - a root's tree screen opens with the Steward row first, above the root's own row;
   - a tap on it pushes the steward screen at /garden/c/s/gardenio: its node, under a Thread / Jobs / Tending / Asks strip,
     the bar saying Steward and Back to the root it came from; Thread is its conversation with the composer, Jobs its run's,
     Tending the plans it tends (the stalled one flagged) above the stalled and the parked, Asks what waits on you;
   - a plan tapped in Tending opens as the root, whose Back is the steward again; the steward's Back is the root it came
     from, in the bar and in history;
   - the steward's address opened from outside (the farm card's "Talk to the steward") is the same screen, its Back the farm;
   - a send from its composer is a new #general thread to @steward, shown once and settled.
   Every wait is on a settled state (COMMONS2.state() and the DOM), never on a timer. Zero page errors. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); } catch (e) { console.error(String(e.message || e).split('\n')[0]); process.exit(3); }
  var ctx = await browser.newContext(Object.assign({}, pw.devices['iPhone 12'], { viewport: { width: 390, height: 844 } }));
  var page = await ctx.newPage(), errs = [];
  page.on('pageerror', function (e) { errs.push(String(e)); });
  // the root, screen and address wanted, the root's tree drawn and its selected row's document settled (and a steward's
  // conversation drawn)
  var at = async function (want) {
    await page.waitForFunction(function (w) {
      var s = COMMONS2.state(), d = document.querySelector('#wb2 .wb2-doc'), top = document.querySelector('#wb2 .wb2-tree:not(.wb2-tree--kept) .wb2-row[aria-level="1"]');
      return Object.keys(w).every(function (k) { return k === 'path' ? location.pathname === w.path : s[k] === w[k]; }) && d && !d.hasAttribute('aria-busy') &&
        !!top && top.dataset.id === s.root && s.doc === s.sel && (!s.steward || !!d.querySelector('.wb2-swtalk'));
    }, want, { timeout: 10000 }).catch(async function (e) {
      var got = await page.evaluate(function () { return { state: COMMONS2.state(), path: location.pathname, hist: history.state }; }).catch(function (x) { return String(x); });
      throw new Error('at(' + JSON.stringify(want) + ') never held: ' + JSON.stringify(got) + '\n' + e.message);
    });
  };
  var shown = function (sel) { return page.evaluate(function (q) { return [].filter.call(document.querySelectorAll(q), function (e) { return !!e.offsetParent; }).map(function (e) { return e.dataset.id || e.dataset.thread || e.dataset.rail || e.dataset.open || e.textContent.trim(); }); }, sel); };
  var bar = function () { return page.evaluate(function () { return [document.querySelector('#wb2 .wb2-back').textContent, document.querySelector('#wb2 .wb2-title').textContent]; }); };
  var tab = async function (k) { await page.locator('#wb2 .wb2-stab[data-tab=' + k + ']').tap(); await page.waitForFunction(function (t) { return COMMONS2.state().tab === t; }, k); };
  var STEWARD = { root: 'gd-wbSteward', sel: 'gd-wbSteward', screen: 'node', path: '/garden/c/s/gardenio' };

  // the Steward row tops the tree screen
  await page.goto(url + '/garden/c/n/gd-wbPlan');
  await at({ root: 'gd-wbPlan', screen: 'tree' });
  var first = await page.evaluate(function () {
    var col = document.querySelector('#wb2 .wb2-treecol'), sw = col.querySelector('.wb2-swrow'), row = col.querySelector('.wb2-tree:not(.wb2-tree--kept) .wb2-row[data-id]');
    var a = sw.getBoundingClientRect(), b = row.getBoundingClientRect();
    return { name: sw.querySelector('.wb2-swname').textContent, above: a.bottom <= b.top + 1, h: Math.round(a.height), w: Math.round(a.width), href: sw.getAttribute('href') };
  });
  assert.deepStrictEqual([first.name, first.above, first.href], ['Steward', true, '/garden/c/s/gardenio'], 'the Steward row is first: ' + JSON.stringify(first));
  assert.ok(first.h >= 44 && first.w >= 300, 'a whole-width row, a 44 px tap (' + first.w + ' x ' + first.h + ')');

  // a tap pushes the steward screen
  await page.locator('#wb2 .wb2-swrow').tap();
  await at(Object.assign({ tab: 'thread' }, STEWARD));
  assert.deepStrictEqual(await bar(), ['Commons v2: a workbench for any bead and its children', 'Steward'], 'Back names the root it came from');
  assert.deepStrictEqual(await shown('#wb2 .wb2-stab'), ['Thread', 'Jobs', 'Tending', 'Asks']);
  assert.deepStrictEqual(await shown('#wb2 .wb2-treecol, #wb2 .wb2-rail'), [], 'the steward screen shows its document only');
  assert.deepStrictEqual(await shown('#wb2 .wb2-swtalk .cm-sw-thread'), ['gd-wishSlack', 'gd-msgSw1', 'gd-msgSw2'], 'its conversation, the land\'s wish first');
  await page.waitForSelector('#wb2 .wb2-swform .cm-input', { state: 'visible' });

  // its tabs
  await tab('jobs');
  await page.waitForSelector('#wb2 [data-rail=jobs] .cm-job', { state: 'visible' });
  assert.deepStrictEqual(await shown('#wb2 .wb2-rail, #wb2 .wb2-doc'), ['jobs']);
  assert.match(await page.locator('#wb2 [data-rail=jobs]').textContent(), /kd show gd-wbS3/, 'its run\'s jobs');
  await tab('plan');
  assert.deepStrictEqual(await shown('#wb2 .wb2-rail'), ['plan']);
  assert.deepStrictEqual(await shown('#wb2 .wb2-swplans .wb2-steplink'), ['gd-wbPlan', 'gd-wbEpic'], 'the plans it tends, not its Wishes');
  assert.strictEqual(await page.locator('#wb2 .wb2-swplans li', { has: page.locator('[data-open=gd-wbEpic]') }).locator('.wb2-flag').textContent(), 'stalled');
  assert.deepStrictEqual(await page.locator('#wb2 [data-rail=plan] .wb2-tending > li').evaluateAll(function (ls) { return ls.map(function (l) { return l.dataset.id + ':' + l.dataset.flag; }); }),
    ['gd-wbEpic:stalled', 'gd-wbS4:parked'], 'then what is stalled or parked');
  await tab('open');
  assert.deepStrictEqual(await shown('#wb2 .wb2-rail'), ['open']);
  assert.match(await page.locator('#wb2 [data-rail=open]').textContent(), /report on Commons v2 step 3/, 'what asks for you');

  // a plan it tends opens as the root, and Back is the steward
  await tab('plan');
  await page.locator('#wb2 .wb2-swplans [data-open=gd-wbPlan]').tap();
  await at({ root: 'gd-wbPlan', screen: 'tree', path: '/garden/c/n/gd-wbPlan' });
  assert.deepStrictEqual(await bar(), ['Steward', 'Plan']);
  await page.locator('#wb2 .wb2-back').tap();
  await at(STEWARD);
  // the steward's Back is the root it came from, in the bar and in history
  await page.locator('#wb2 .wb2-back').tap();
  await at({ root: 'gd-wbPlan', screen: 'tree', path: '/garden/c/n/gd-wbPlan' });
  assert.deepStrictEqual(await bar(), ['Farm', 'Plan']);
  await page.goForward();
  await at(STEWARD);
  await page.goBack();
  await at({ root: 'gd-wbPlan', screen: 'tree', path: '/garden/c/n/gd-wbPlan' });

  // from outside, as the farm card's "Talk to the steward" opens it: the same screen, its Back the farm
  await page.goto(url + '/garden/c/s/gardenio');
  await at(Object.assign({ tab: 'thread' }, STEWARD));
  assert.deepStrictEqual(await bar(), ['Farm', 'Steward']);
  assert.deepStrictEqual(await shown('#wb2 .wb2-stab'), ['Thread', 'Jobs', 'Tending', 'Asks']);

  // a send: a new #general thread to @steward, shown once and settled
  var said = 'From the phone ' + Date.now();
  await page.locator('#wb2 .wb2-swform .cm-input').fill(said);
  await page.locator('#wb2 .wb2-swform .cm-send').tap();
  await page.waitForFunction(function (t) { return [].some.call(document.querySelectorAll('#wb2 .wb2-swtalk .cm-sw-thread'), function (li) { return li.textContent.indexOf(t) >= 0 && !!li.querySelector('.wb2-sent'); }); }, said, { timeout: 10000 });
  var msg = await (await page.request.get(url + '/test/wb/msg?text=' + encodeURIComponent('@steward ' + said))).json();
  assert.deepStrictEqual([msg.channel_id, msg.thread_id || ''], ['gd-chGeneral', ''], 'a new #general thread: ' + JSON.stringify(msg));
  assert.strictEqual(await page.evaluate(function (t) { return [].filter.call(document.querySelectorAll('#wb2 .wb2-swtalk .cm-sw-thread'), function (li) { return li.textContent.indexOf(t) >= 0; }).length; }, said), 1, 'the send shows once');

  await page.locator('#wb2 .wb2-back').tap();
  await page.waitForURL(/\/garden$/);

  assert.deepStrictEqual(errs, [], 'page errors');
  await browser.close();
  console.log('workbench steward phone: the Steward row first, the steward screen and its tabs, Tending\'s plans, Back up the stack, the farm\'s entry and a send ok');
})().catch(function (e) { console.error(e); process.exit(1); });
