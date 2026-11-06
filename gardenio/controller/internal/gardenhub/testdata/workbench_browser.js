/* The Commons v2 workbench (plan gd-lRaCxsiLvC step 4, gd-sXB0CyaqTA), run from TestWorkbenchUnderChromium against the
   card world's Commons with the workbench roots in it (workbench_world_test.go). Usage: node workbench_browser.js <base url>
   <node_modules dir>.

   On a desktop, opened at /garden/c/n/gd-wbPlan:
   - it is the workbench's own page (node.html): none of the farm's scripts, the tree with the focus and the root selected;
   - j and k move the selection, and the document, the Plan rail (the step marked) and the Jobs rail (the crew's jobs)
     follow it;
   - Enter opens the row here (a new address), u goes up with the old root selected, Back returns;
   - Ctrl-K jumps to a bead id, / finds in the tree (only the matches and their paths shown), Enter selects the first;
   - Shift+F10 opens the row menu, the arrows move in it, Escape closes it back to the tree, Open here opens the row;
   - the document is the bead view's (gd-0GnBi7SRfK): its Conversation's composer sends on Enter, and the reply shows once;
   - a node event on the selected row moves its state in place;
   - the head's reply line says the holder's state, and its node events move it; an agent's head is its live card (step 5);
   - Escape goes to the farm (/garden), and Back brings the workbench back on the row it had selected.
   Zero page errors. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); } catch (e) { console.error(String(e.message || e).split('\n')[0]); process.exit(3); }
  var page = await browser.newPage({ viewport: { width: 1280, height: 800 } }), errs = [];
  page.on('pageerror', function (e) { errs.push(String(e)); });
  var sel = function () { return page.evaluate(function () { var r = document.querySelector('#wb2 .wb2-row[aria-selected=true]'); return r && r.dataset.id; }); };
  var shown = async function (id, root) {
    await page.waitForFunction(function (a) {
      var r = document.querySelector('#wb2 .wb2-row[aria-selected=true]'), d = document.querySelector('#wb2 .wb2-doc');
      return r && r.dataset.id === a.id && (!a.root || COMMONS2.state().root === a.root) && d && !d.hasAttribute('aria-busy') && d.querySelector('.wb2-head');
    }, { id: id, root: root || '' }, { timeout: 10000 });
  };
  var path = function () { return page.evaluate(function () { return location.pathname; }); };

  await page.goto(url + '/garden/c/n/gd-wbPlan');
  await shown('gd-wbPlan', 'gd-wbPlan');
  assert.ok(await page.evaluate(function () { return typeof GARDEN === 'undefined' && !document.getElementById('board'); }), 'no farm under the workbench');
  assert.strictEqual(await path(), '/garden/c/n/gd-wbPlan', 'the address is the node');
  assert.ok(await page.evaluate(function () { return document.activeElement.classList.contains('wb2-tree'); }), 'the tree has the focus');

  // j and k, and the rails following
  for (var i = 0; i < 4; i++) await page.keyboard.press('j');
  await shown('gd-wbS3');
  await page.waitForSelector('#wb2 [data-rail=jobs] .cm-job', { timeout: 10000 });
  assert.strictEqual(await page.locator('#wb2 [data-rail=jobs] .cm-job').count(), 3, 'the crew\'s three jobs');
  assert.match(await page.locator('#wb2 [data-rail=jobs] .wb2-where').textContent(), /Held by gardenio-crew-S3/);
  assert.match(await page.locator('#wb2 [data-rail=plan] [aria-current=step]').textContent(), /The node model/);
  assert.match(await page.locator('#wb2 [data-rail=plan] .wb2-where').textContent(), /Step 3 of 4 in Phase 1/);
  assert.match(await page.locator('#wb2 [data-rail=open] .wb2-rail-h').textContent(), /2 open, 0 done/);
  await page.keyboard.press('k');
  await shown('gd-wbS2');
  await page.keyboard.press('j');
  await shown('gd-wbS3');

  // Enter opens here, u goes up keeping the old root selected, Back returns
  await page.keyboard.press('Enter');
  await shown('gd-wbS3', 'gd-wbS3');
  assert.strictEqual(await path(), '/garden/c/n/gd-wbS3');
  assert.match(await page.locator('#wb2 .wb2-uplink').textContent(), /Up: .*Phase 1/);
  await page.keyboard.press('u');
  await shown('gd-wbS3', 'gd-wbPhase1');
  assert.strictEqual(await path(), '/garden/c/n/gd-wbPhase1');
  await page.goBack();
  await shown('gd-wbS3', 'gd-wbS3');

  // Ctrl-K jumps to an id; / finds in the tree
  await page.keyboard.press('Control+K');
  assert.ok(await page.evaluate(function () { return document.activeElement.classList.contains('wb2-find'); }), 'Ctrl-K focuses the jump field');
  await page.keyboard.type('gd-wbPlan');
  await page.keyboard.press('Enter');
  await shown('gd-wbPlan', 'gd-wbPlan');
  await page.keyboard.press('/');
  await page.keyboard.type('phone');
  var left = await page.evaluate(function () { return [].filter.call(document.querySelectorAll('#wb2 .wb2-tree:not(.wb2-tree--kept) .wb2-row[data-id]'), function (r) { return r.offsetParent; }).map(function (r) { return r.dataset.id; }); });
  assert.deepStrictEqual(left, ['gd-wbPlan', 'gd-wbPhase2', 'gd-wbS6'], 'find keeps the matches and their paths');
  await page.keyboard.press('Enter');
  await shown('gd-wbS6');
  await page.keyboard.press('/');
  await page.keyboard.press('Escape');   // clears the find, back to the tree
  assert.strictEqual(await page.locator('#wb2 .wb2-row[data-id][hidden]').count(), 0, 'Escape in the find shows every row again');

  // the row menu
  await page.keyboard.press('Shift+F10');
  await page.waitForSelector('#wb2 .wb2-menu:not([hidden]) [role=menuitem]');
  assert.deepStrictEqual(await page.locator('#wb2 .wb2-menu [role=menuitem]').allTextContents(), ['Open here', 'Up', 'Show in farm', 'Copy id', 'Ask the steward about this']);
  await page.keyboard.press('ArrowDown');
  assert.strictEqual(await page.evaluate(function () { return document.activeElement.textContent; }), 'Up');
  await page.keyboard.press('Escape');
  assert.ok(await page.evaluate(function () { return document.querySelector('#wb2 .wb2-menu').hidden && document.activeElement.classList.contains('wb2-tree'); }), 'Escape closes the menu to the tree');
  await page.keyboard.press('Shift+F10');
  await page.keyboard.press('Enter');
  await shown('gd-wbS6', 'gd-wbS6');

  // the composer: on the plan's building step, whose thread is #general's gd-wishPorch
  await page.goto(url + '/garden/c/n/gd-wbPlan');
  await shown('gd-wbPlan', 'gd-wbPlan');
  await page.locator('#wb2 .wb2-row[data-id=gd-wbS4] > .wb2-line .wb2-t').click();
  await shown('gd-wbS4');
  var input = '#wb2 .wb2-bead .bv-cv-say textarea';
  await page.waitForSelector(input);
  var said = 'From the workbench ' + Date.now();
  await page.locator(input).fill(said);
  await page.locator(input).press('Enter');
  await page.waitForFunction(function (t) { return [].some.call(document.querySelectorAll('#wb2 .bv-cv-list > li'), function (li) { return li.textContent.indexOf(t) >= 0 && !li.querySelector('.bv-cv-st'); }); }, said, { timeout: 10000 });
  await page.waitForTimeout(500);   // the stream's copy of it, which must settle rather than repeat it
  assert.strictEqual(await page.evaluate(function (t) { return [].filter.call(document.querySelectorAll('#wb2 .bv-cv-list > li'), function (li) { return li.textContent.indexOf(t) >= 0; }).length; }, said), 1, 'the reply shows once');
  assert.strictEqual(await page.locator(input).inputValue(), '', 'Send empties the composer');

  // a node event moves the selected row in place
  await page.waitForFunction(function () { return /node=gd-wbS4/.test(COMMONS2.state().stream); });
  var moved = false;
  for (var n = 0; n < 5 && !moved; n++) {
    await page.request.post(url + '/test/wb/node?id=gd-wbS4&status=in_progress');
    moved = await page.waitForFunction(function () { var r = document.querySelector('#wb2 .wb2-row[data-id=gd-wbS4]'); return r && r.dataset.state === 'in_progress' && r.querySelector('.wb2-dot').dataset.state === 'in_progress'; }, null, { timeout: 2000 }).then(function () { return true; }, function () { return false; });
  }
  assert.ok(moved, 'the node event patched the row');
  assert.strictEqual(await sel(), 'gd-wbS4', 'the patch kept the selection');

  // beads read as short titles, never as ids (gd-QTukIA8Nvv): a bare id nothing the page loaded titled takes its short
  // title from GET /garden/names (the fixture's own ids, gd-wbS3, are too short to be bead ids to web/titles.js)
  assert.strictEqual(await page.evaluate(function () { return TITLES.title('gd-wbNamedOne'); }), '', 'unknown before the dress');
  await page.evaluate(function () {
    var a = document.createElement('a'), p = document.createElement('p');
    a.href = document.body.dataset.base + '/garden?bead=gd-wbNamedOne'; a.dataset.bead = 'gd-wbNamedOne'; a.textContent = 'gd-wbNamedOne'; a.id = 'bare-id';
    p.appendChild(a); document.querySelector('#wb2 .wb2-doc').appendChild(p);
    TITLES.dress(p);
  });
  await page.waitForFunction(function () { var a = document.getElementById('bare-id'); return a.textContent === 'named by short title' && a.title === 'named by short title (gd-wbNamedOne)'; }, null, { timeout: 5000 });

  // Escape to the farm, Back to the workbench
  await page.locator('#wb2 .wb2-tree').focus();
  await page.keyboard.press('Escape');
  await page.waitForURL(/\/garden$/);
  await page.waitForSelector('#board');
  await page.goBack();
  await shown('gd-wbS4', 'gd-wbPlan');

  // step 5 (gd-Z05Hye7yP4): the reply line says the holder's state, and moves with its node events; an agent's head is its live card
  await page.locator('#wb2 .wb2-row[data-id=gd-wbS3] > .wb2-line .wb2-t').click();
  await shown('gd-wbS3');
  var hold = function (want) { return page.waitForFunction(function (w) { var h = document.querySelector('#wb2 .wb2-head > .wb2-hold'); return h && !h.hidden && h.textContent === w; }, want, { timeout: 2000 }).then(function () { return true; }, function () { return false; }); };
  assert.ok(await hold('gardenio-crew-S3 · working'), 'the reply line names the holder at work');
  await page.waitForFunction(function () { return /node=gd-wbCrew3/.test(COMMONS2.state().stream); });
  var idle = false;
  for (var k = 0; k < 5 && !idle; k++) {
    await page.request.post(url + '/test/wb/node?id=gd-wbCrew3&status=open&state=idle');
    idle = await hold('gardenio-crew-S3 · idle, waiting for you');
  }
  assert.ok(idle, 'the holder going idle moves the reply line');
  await page.goto(url + '/garden/c/n/gd-wbCrew3');
  await shown('gd-wbCrew3', 'gd-wbCrew3');
  assert.match(await page.locator('#wb2 .wb2-head .cm-node-live').textContent(), /^\s*Idle\b.*Watch live\s+Nudge$/, 'the agent\'s head carries its live card');

  assert.deepStrictEqual(errs, [], 'page errors');
  await browser.close();
  console.log('workbench: keys, menu, find, jump, up, back, farm, composer, a node event, the reply line and the live head ok');
})().catch(function (e) { console.error(e); process.exit(1); });
