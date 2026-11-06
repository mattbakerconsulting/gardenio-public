/* The Commons v2 workbench on a phone (plan gd-lRaCxsiLvC step 7, gd-DhGFLKK0tj), run from TestWorkbenchPhoneUnderChromium
   against the workbench rig (workbench_world_test.go). Usage: node workbench_phone_browser.js <base url> <node_modules dir>.

   At 390 x 844 with touch, opened at /garden/c/n/gd-wbPlan:
   - the tree screen shows the root and its children only, one level, and the bar says Back to the farm and what this is;
   - a tap on a row with work under it opens that row as the root, one level down; a tap on any other row pushes the node
     screen on its Thread tab, under a Thread / Jobs / Plan / Open strip, the bead view's document with its composer on
     screen (gd-0GnBi7SRfK);
   - the tabs show the document or one rail at a time, and a step in the Plan tab is shown in place, on its Thread tab;
   - the bar's Back is one level up each time (node, its tree, the root above, the farm), and so is the browser's Back;
   - a node address opened from outside gets the level above put under it in history;
   - with the keyboard up (the visual viewport shorter) the view follows it and the composer is above the keyboard;
   - a send from the phone's composer shows once.
   Zero page errors. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));

/* a visual viewport the test moves, as a phone's keyboard would: window.__vv(height) */
function fakeViewport() {
  var on = [], vv = { height: window.innerHeight, width: window.innerWidth, offsetTop: 0, offsetLeft: 0, scale: 1, addEventListener: function (t, f) { on.push(f); }, removeEventListener: function () {} };
  Object.defineProperty(window, 'visualViewport', { value: vv, configurable: true });
  window.__vv = function (h) { vv.height = h; on.forEach(function (f) { f(); }); };
}

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); } catch (e) { console.error(String(e.message || e).split('\n')[0]); process.exit(3); }
  var ctx = await browser.newContext(Object.assign({}, pw.devices['iPhone 12'], { viewport: { width: 390, height: 844 } }));
  await ctx.addInitScript(fakeViewport);
  var page = await ctx.newPage(), errs = [];
  page.on('pageerror', function (e) { errs.push(String(e)); });
  var state = function () { return page.evaluate(function () { var s = COMMONS2.state(); s.path = location.pathname; return s; }); };
  var at = async function (want) {
    await page.waitForFunction(function (w) {
      var s = COMMONS2.state(), d = document.querySelector('#wb2 .wb2-doc'), top = document.querySelector('#wb2 .wb2-tree:not(.wb2-tree--kept) .wb2-row[aria-level="1"]');
      // S.root is set when a root is asked for, before its tree comes: wait for its tree and the selected row's document too
      return Object.keys(w).every(function (k) { return k === 'path' ? location.pathname === w.path : s[k] === w[k]; }) && d && !d.hasAttribute('aria-busy') &&
        !!top && top.dataset.id === s.root && s.doc === s.sel;
    }, want, { timeout: 10000 }).catch(async function (e) {
      // what held it, for a timeout on a CI runner: the state, the document's busy flag and the tree's top row
      var got = await page.evaluate(function () {
        var s = COMMONS2.state(), d = document.querySelector('#wb2 .wb2-doc'), top = document.querySelector('#wb2 .wb2-tree:not(.wb2-tree--kept) .wb2-row[aria-level="1"]');
        return { state: s, path: location.pathname, hist: history.state, busy: !!d && d.hasAttribute('aria-busy'), doc: d && d.textContent.trim().slice(0, 120), top: top && top.dataset.id };
      }).catch(function (x) { return String(x); });
      throw new Error('at(' + JSON.stringify(want) + ') never held: ' + JSON.stringify(got) + '\n' + e.message);
    });
  };
  var shown = function (sel) { return page.evaluate(function (q) { return [].filter.call(document.querySelectorAll(q), function (e) { return !!e.offsetParent; }).map(function (e) { return e.dataset.id || e.dataset.rail || e.textContent.trim(); }); }, sel); };
  var bar = function () { return page.evaluate(function () { return [document.querySelector('#wb2 .wb2-back').textContent, document.querySelector('#wb2 .wb2-title').textContent]; }); };
  var row = function (id) { return page.locator('#wb2 .wb2-tree:not(.wb2-tree--kept) .wb2-row[data-id=' + id + '] > .wb2-line .wb2-t'); };

  // the tree screen, one level
  await page.goto(url + '/garden/c/n/gd-wbPlan');
  await at({ root: 'gd-wbPlan', screen: 'tree' });
  assert.deepStrictEqual(await shown('#wb2 .wb2-row[data-id]'), ['gd-wbPlan', 'gd-wbPhase1', 'gd-wbPhase2', 'gd-wbS7'], 'the root and its children, one level');
  assert.deepStrictEqual(await shown('#wb2 .wb2-doc, #wb2 .wb2-rail, #wb2 .wb2-strip'), [], 'no document, rails or tabs on the tree screen');
  assert.deepStrictEqual(await bar(), ['Farm', 'Plan']);

  // a row with work under it opens one level down; a step is pushed, on its Thread tab
  await row('gd-wbPhase1').tap();
  await at({ root: 'gd-wbPhase1', screen: 'tree', path: '/garden/c/n/gd-wbPhase1' });
  assert.deepStrictEqual(await bar(), ['Commons v2: a workbench for any bead and its children', 'Phase']);
  await row('gd-wbS3').tap();
  await at({ root: 'gd-wbPhase1', sel: 'gd-wbS3', screen: 'node', tab: 'thread', path: '/garden/c/n/gd-wbPhase1' });
  assert.deepStrictEqual(await bar(), ['Phase 1: the stream and the model', 'The node model: any bead and its children']);
  assert.deepStrictEqual(await shown('#wb2 .wb2-stab'), ['Thread', 'Jobs', 'Plan', 'Open']);
  assert.deepStrictEqual(await shown('#wb2 .wb2-treecol, #wb2 .wb2-rail'), [], 'the node screen shows its document only');
  await page.waitForSelector('#wb2 .wb2-bead .bv-cv-say textarea', { state: 'visible' });

  // the tabs
  await page.locator('#wb2 .wb2-stab[data-tab=jobs]').tap();
  await page.waitForSelector('#wb2 [data-rail=jobs] .cm-job', { state: 'visible' });
  assert.deepStrictEqual(await shown('#wb2 .wb2-rail'), ['jobs']);
  assert.strictEqual(await page.locator('#wb2 [data-rail=jobs] .cm-job').count(), 3, 'the crew\'s three jobs');
  assert.deepStrictEqual(await shown('#wb2 .wb2-doc'), []);
  await page.locator('#wb2 .wb2-stab[data-tab=plan]').tap();
  assert.deepStrictEqual(await shown('#wb2 .wb2-rail'), ['plan']);
  assert.strictEqual(await page.locator('#wb2 .wb2-stab[aria-selected=true]').textContent(), 'Plan');
  await page.locator('#wb2 [data-rail=plan] .wb2-steplink', { hasText: 'One stream' }).tap();
  await at({ sel: 'gd-wbS2', screen: 'node', tab: 'thread' });
  assert.deepStrictEqual(await bar(), ['Phase 1: the stream and the model', 'One stream that carries content'], 'a step beside it is shown in place');

  // the bar's Back: the node to its tree, the tree to the root above, the top to the farm; the browser's Back returns
  await page.locator('#wb2 .wb2-back').tap();
  await at({ root: 'gd-wbPhase1', screen: 'tree', path: '/garden/c/n/gd-wbPhase1' });
  await page.locator('#wb2 .wb2-back').tap();
  await at({ root: 'gd-wbPlan', screen: 'tree', path: '/garden/c/n/gd-wbPlan' });
  await page.locator('#wb2 .wb2-back').tap();
  await page.waitForURL(/\/garden$/);
  await page.goBack();
  await at({ root: 'gd-wbPlan', screen: 'tree' });

  // the browser's Back walks the same stack
  var p2 = await row('gd-wbPhase2').boundingBox();
  p2 = { x: p2.x + p2.width / 2, y: p2.y + p2.height / 2 };
  await row('gd-wbPhase2').tap();
  await at({ root: 'gd-wbPhase2', screen: 'tree' });
  // tapped quickly near where gd-wbPhase2 was: Chromium counts it a double tap, which must not open the row as the root
  var bb = await row('gd-wbS6').boundingBox();
  await row('gd-wbS6').tap({ position: { x: Math.min(bb.width - 1, Math.max(1, p2.x - bb.x)), y: Math.min(bb.height - 1, Math.max(1, p2.y - bb.y)) } });
  await at({ sel: 'gd-wbS6', screen: 'node' });
  await page.goBack();
  await at({ root: 'gd-wbPhase2', screen: 'tree', path: '/garden/c/n/gd-wbPhase2' });
  await page.goBack();
  await at({ root: 'gd-wbPlan', screen: 'tree', path: '/garden/c/n/gd-wbPlan' });
  await page.goForward();
  await page.goForward();
  await at({ root: 'gd-wbPhase2', sel: 'gd-wbS6', screen: 'node' });

  // a node address from outside: the level above is under it
  await page.goto(url + '/garden/c/n/gd-wbS3');
  await at({ root: 'gd-wbS3', screen: 'tree', path: '/garden/c/n/gd-wbS3' });
  assert.deepStrictEqual(await bar(), ['Phase 1: the stream and the model', 'Bead']);
  await page.locator('#wb2 .wb2-back').tap();
  await at({ root: 'gd-wbPhase1', screen: 'tree', path: '/garden/c/n/gd-wbPhase1' });
  await page.goForward();
  await at({ root: 'gd-wbS3', screen: 'tree' });

  // the keyboard up: the view follows the visual viewport and the composer sits on the keyboard; a send shows once
  await page.goto(url + '/garden/c/n/gd-wbPlan');
  await at({ root: 'gd-wbPlan', screen: 'tree' });
  await row('gd-wbS7').tap();
  await at({ sel: 'gd-wbS7', screen: 'node' });
  var input = '#wb2 .wb2-bead .bv-cv-say textarea';
  await page.locator(input).tap();
  await page.evaluate(function () { window.__vv(420); });
  var kb = await page.evaluate(function () { var v = document.getElementById('wb2').getBoundingClientRect(), c = document.querySelector('#wb2 .wb2-bead .bv-cv-say').getBoundingClientRect(); return [Math.round(v.height), Math.round(c.top), Math.round(c.bottom)]; });
  assert.strictEqual(kb[0], 420, 'the view is the visual viewport\'s height');
  assert.ok(kb[1] >= 0 && kb[2] <= 420, 'the composer is above the keyboard (' + kb[1] + '-' + kb[2] + ')');
  var said = 'From the phone ' + Date.now();
  await page.locator(input).fill(said);
  await page.locator('#wb2 .wb2-bead .bv-cv-say button').tap();
  await page.waitForFunction(function (t) { return [].some.call(document.querySelectorAll('#wb2 .bv-cv-list > li'), function (li) { return li.textContent.indexOf(t) >= 0 && !li.querySelector('.bv-cv-st'); }); }, said, { timeout: 10000 });
  await page.waitForTimeout(500);   // the stream's copy of it, which must settle rather than repeat it
  assert.strictEqual(await page.evaluate(function (t) { return [].filter.call(document.querySelectorAll('#wb2 .bv-cv-list > li'), function (li) { return li.textContent.indexOf(t) >= 0; }).length; }, said), 1, 'the send shows once');
  await page.evaluate(function () { window.__vv(innerHeight); });
  assert.strictEqual(await page.evaluate(function () { return document.getElementById('wb2').style.height; }), '', 'the keyboard down, the view is the screen again');

  assert.deepStrictEqual(errs, [], 'page errors');
  assert.ok((await state()).root, 'still the workbench');
  await browser.close();
  console.log('workbench phone: one level, push, tabs, Back up the stack, a deep link\'s level above, the keyboard and a send ok');
})().catch(function (e) { console.error(e); process.exit(1); });
