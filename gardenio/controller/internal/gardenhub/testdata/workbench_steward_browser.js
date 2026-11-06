/* The steward view on the desktop (steward view S2, gd-LisW87zrnf), run from TestWorkbenchStewardUnderChromium against the
   card world's Commons with gardenio's steward in it (workbench_world_test.go wbStewards). Usage: node
   workbench_steward_browser.js <base url> <node_modules dir>.

   On a desktop:
   - every root's tree has the fixed Steward row, to the root's land's steward; g s opens it as the root, at
     /garden/c/s/gardenio, the row marked current;
   - its head says what it is doing and why, its runs today and Watch live; its tree is the plans it tends, a plan's
     gardener under it, the stalled one flagged, then the land's Wishes under #general (gd-ak4P2qXMss), the fixture Slack
     wish with its last line and its live crew; its rails are Tending (the stalled plan, the parked step), Asks for you
     (its open decision and the promise past due, one answered) and its run's jobs; its document is the #general threads
     that address it and the wish's, an agent's words labelled;
   - Holds is edited in place (GET and POST /garden/notes/gardenio fulfilled here): Edit, the box with the Holds it opened,
     Save posts them with "was", and the face shows the saved Holds;
   - a row's menu "Ask the steward about this" opens the steward with a draft naming the row, its about field set; Enter
     sends it as a new #general thread (@steward first, fields.about the row), shown at once and settled;
   - /garden/c/s opens the home garden's steward, and ?thread= marks that thread.
   Zero page errors. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); } catch (e) { console.error(String(e.message || e).split('\n')[0]); process.exit(3); }
  var page = await browser.newPage({ viewport: { width: 1280, height: 800 } }), errs = [];
  page.on('pageerror', function (e) { errs.push(String(e)); });
  var shown = async function (id, root) {
    await page.waitForFunction(function (a) {
      var r = document.querySelector('#wb2 .wb2-row[aria-selected=true]'), d = document.querySelector('#wb2 .wb2-doc');
      return r && r.dataset.id === a.id && (!a.root || COMMONS2.state().root === a.root) && d && !d.hasAttribute('aria-busy') && d.querySelector('.wb2-head');
    }, { id: id, root: root || '' }, { timeout: 10000 });
  };
  var talked = function () { return page.waitForSelector('#wb2 .wb2-swtalk', { timeout: 10000 }); };
  var path = function () { return page.evaluate(function () { return location.pathname + location.search; }); };
  var saved = null;
  await page.route('**/garden/notes/gardenio', function (r) {
    return r.fulfill({ json: { id: 'gd-wbNotes', land: 'gardenio', content: '## Holds\n- no chart merges before Friday\'s release\n', holds: '- no chart merges before Friday\'s release', holds_ok: true, edit: true } });
  });
  await page.route('**/garden/notes/gardenio/holds', function (r) {
    saved = JSON.parse(r.request().postData());
    return r.fulfill({ json: { id: 'gd-wbNotes', land: 'gardenio', holds: saved.holds, holds_ok: true, edit: true, saved: true } });
  });

  // the Steward row on a plan's tree, and g s
  await page.goto(url + '/garden/c/n/gd-wbPlan');
  await shown('gd-wbPlan', 'gd-wbPlan');
  assert.strictEqual(await page.locator('#wb2 .wb2-treecol > .wb2-swrow').getAttribute('href'), '/garden/c/s/gardenio', 'the row is the root land\'s steward');
  await page.keyboard.press('g');
  await page.keyboard.press('s');
  await shown('gd-wbSteward', 'gd-wbSteward');
  await talked();
  assert.strictEqual(await path(), '/garden/c/s/gardenio', 'the steward\'s address is its land\'s');
  assert.strictEqual(await page.locator('#wb2 .wb2-swrow').getAttribute('aria-current'), 'page');
  assert.ok(await page.evaluate(function () { return COMMONS2.state().steward; }));

  // head, tree, rails, conversation
  assert.match(await page.locator('#wb2 .wb2-head .cm-sw-now').textContent(), /^Working\s+Tending Commons v2/);
  assert.match(await page.locator('#wb2 .wb2-head .cm-sw-today').textContent(), /3 runs today of 12 · \$1\.84 today · Watch live/);
  assert.deepStrictEqual(await page.locator('#wb2 .wb2-tree:not(.wb2-tree--kept) .wb2-row[data-id]').evaluateAll(function (rs) { return rs.map(function (r) { return r.dataset.id; }); }),
    ['gd-wbSteward', 'gd-wbPlan', 'gd-wbGard', 'gd-wbEpic', 'gd-chGeneral', 'gd-wishSlack'], 'the plans it tends, a plan\'s gardener under it, then its Wishes');
  var wish = page.locator('#wb2 .wb2-tree:not(.wb2-tree--kept) .wb2-row[data-id=gd-wishSlack]');
  assert.strictEqual(await wish.getAttribute('aria-level'), '3', 'a wish is under Wishes');
  assert.match(await wish.locator('.cm-tn-wish').textContent(), /^5 replies · gardenio-one-bead-view-k2p .*PR open with the before shots of the three views; waiting on CI$/, 'its last line, its crew\'s, said once');
  assert.strictEqual(await wish.getAttribute('data-state'), 'working');
  assert.match(await page.locator('#wb2 [data-rail=plan] .wb2-rail-h').textContent(), /^Tending$/);
  assert.deepStrictEqual(await page.locator('#wb2 [data-rail=plan] .wb2-tending > li').evaluateAll(function (ls) { return ls.map(function (l) { return l.dataset.id + ':' + l.dataset.flag; }); }),
    ['gd-wbEpic:stalled', 'gd-wbS4:parked']);
  assert.match(await page.locator('#wb2 [data-rail=open] .wb2-rail-h').textContent(), /^Asks for you · 2 open, 1 done$/);
  assert.match(await page.locator('#wb2 [data-rail=open]').textContent(), /report on Commons v2 step 3/, 'the promise past due asks');
  await page.waitForSelector('#wb2 [data-rail=jobs] .cm-job', { timeout: 10000 });
  assert.match(await page.locator('#wb2 [data-rail=jobs]').textContent(), /Held by gardenio-steward-k3x9[\s\S]*kd show gd-wbS3/);
  assert.deepStrictEqual(await page.locator('#wb2 .wb2-swtalk .cm-sw-thread').evaluateAll(function (ts) { return ts.map(function (t) { return t.dataset.thread; }); }), ['gd-wishSlack', 'gd-msgSw1', 'gd-msgSw2'], 'newest last');
  assert.strictEqual(await page.locator('#wb2 .wb2-swtalk .cm-sw-line--steward').count(), 2);
  assert.match(await page.locator('#wb2 .wb2-swtalk [data-thread=gd-msgSw1] .cm-sw-line--agent').textContent(), /agent/);
  assert.match(await page.locator('#wb2 .wb2-swform .cm-to').textContent(), /To the steward in #general/);

  // the stream: listening on #general, a reply in a thread that asks it shows in place
  await page.waitForFunction(function () { return /channel=gd-chGeneral/.test(COMMONS2.state().stream); }, null, { timeout: 5000 });
  await page.request.post(url + '/test/wb/say?thread=gd-msgSw1&text=' + encodeURIComponent('streamed in, steward'));
  await page.waitForFunction(function () { return /streamed in, steward/.test(document.querySelector('#wb2 .wb2-swtalk').textContent); }, null, { timeout: 5000 });
  // and while someone types to it: the conversation moves, the draft and the focus stay
  await page.locator('#wb2 .wb2-swform .cm-input').fill('half a thought');
  await page.locator('#wb2 .wb2-swform .cm-input').focus();
  await page.request.post(url + '/test/wb/say?thread=gd-msgSw2&text=' + encodeURIComponent('a second reply'));
  await page.waitForFunction(function () { return /a second reply/.test(document.querySelector('#wb2 .wb2-swtalk').textContent); }, null, { timeout: 5000 });
  assert.ok(await page.evaluate(function () { var t = document.activeElement; return t && t.matches('.wb2-swform .cm-input') && t.value === 'half a thought'; }), 'the draft keeps its focus and words');
  await page.locator('#wb2 .wb2-swform .cm-input').fill('');
  await page.evaluate(function () { document.activeElement.blur(); });

  // a jump away (Ctrl-K) and Back: the steward again, listening
  await page.focus('#wb2 .wb2-find'); await page.keyboard.press('Control+K'); await page.keyboard.type('gd-wbEpic'); await page.keyboard.press('Enter');
  await shown('gd-wbEpic', 'gd-wbEpic');
  await page.goBack();
  await shown('gd-wbSteward', 'gd-wbSteward');
  await page.waitForFunction(function () { return !!document.querySelector('#wb2 .wb2-swform .cm-input') && /channel=/.test(COMMONS2.state().stream); }, null, { timeout: 5000 })
    .catch(async function (e) { throw new Error('not back and listening: ' + JSON.stringify(await page.evaluate(function () { return [COMMONS2.state(), location.pathname, !!document.querySelector('#wb2 .wb2-swform')]; }))); });

  // Holds in place
  var edit = page.locator('#wb2 .wb2-face .wb2-holdsedit');
  await edit.waitFor({ timeout: 10000 });
  await edit.click();
  var box = page.locator('#wb2 .wb2-face form.wb2-holds textarea');
  assert.strictEqual(await box.inputValue(), '- no chart merges before Friday\'s release', 'the box holds the Holds it opened');
  assert.ok(await box.evaluate(function (t) { return document.activeElement === t; }), 'the box has the focus');
  await box.fill('- no chart merges before Friday\'s release\n- ask before a schema change');
  await page.keyboard.press('Enter');   // a new line in Holds, never a send
  assert.match(await box.inputValue(), /schema change\n$/);
  await page.locator('#wb2 .wb2-face form.wb2-holds button[type=submit]').click();
  await page.waitForSelector('#wb2 .wb2-face .wb2-holdstext', { timeout: 5000 });
  assert.deepStrictEqual([saved.was, /ask before a schema change/.test(saved.holds)], ['- no chart merges before Friday\'s release', true], 'the save posts the Holds and the ones it opened');
  assert.match(await page.locator('#wb2 .wb2-face .wb2-holdstext').textContent(), /ask before a schema change/);
  assert.ok(await page.evaluate(function () { return document.activeElement.classList.contains('wb2-holdsedit'); }), 'the focus goes back to Edit');

  // Ask the steward about a row
  await page.goBack();
  await shown('gd-wbPlan', 'gd-wbPlan');
  await page.locator('#wb2 .wb2-row[data-id=gd-wbS3] > .wb2-line .wb2-t').click();
  await shown('gd-wbS3');
  await page.keyboard.press('Shift+F10');
  await page.locator('#wb2 .wb2-menu [role=menuitem]', { hasText: 'Ask the steward about this' }).click();
  await shown('gd-wbSteward', 'gd-wbSteward');
  await talked();
  await page.waitForFunction(function () { var t = document.activeElement; return t && t.matches('.wb2-swform .cm-input'); }, null, { timeout: 5000 });
  var ta = page.locator('#wb2 .wb2-swform .cm-input');
  assert.strictEqual(await ta.inputValue(), '@steward about The node model: any bead and its children (gd-wbS3): ');
  assert.strictEqual(await page.locator('#wb2 .wb2-swform input[name=about]').inputValue(), 'gd-wbS3');
  await page.keyboard.type('is it stuck?');
  await page.keyboard.press('Enter');
  await page.waitForSelector('#wb2 .wb2-swtalk .cm-sw-thread.wb2-pending, #wb2 .wb2-swtalk .cm-sw-thread .wb2-sent', { timeout: 2000 });
  await page.waitForSelector('#wb2 .wb2-swtalk .cm-sw-thread .wb2-sent', { timeout: 5000 });
  var words = '@steward about The node model: any bead and its children (gd-wbS3): is it stuck?';
  var msg = await (await page.request.get(url + '/test/wb/msg?text=' + encodeURIComponent(words))).json();
  assert.deepStrictEqual([msg.channel_id, msg.about, msg.thread_id || ''], ['gd-chGeneral', 'gd-wbS3', ''], 'a new #general thread about the row: ' + JSON.stringify(msg));
  assert.strictEqual(await page.locator('#wb2 .wb2-swform input[name=about]').inputValue(), '', 'the about goes with that send only');
  assert.strictEqual(await ta.inputValue(), '');

  // the home garden's address, and ?thread=
  await page.goto(url + '/garden/c/s');
  await shown('gd-wbSteward', 'gd-wbSteward');
  await page.goto(url + '/garden/c/s/gardenio?thread=gd-msgSw2');
  await shown('gd-wbSteward', 'gd-wbSteward');
  await talked();
  assert.strictEqual(await page.locator('#wb2 .wb2-swtalk .wb2-swhere').getAttribute('data-thread'), 'gd-msgSw2', 'the thread the address names is marked');

  assert.deepStrictEqual(errs, [], 'page errors');
  await browser.close();
  console.log('workbench steward: the row, g s, head, tree, rails, conversation, Holds in place, the ask, the address and ?thread= ok');
})().catch(function (e) { console.error(e); process.exit(1); });
