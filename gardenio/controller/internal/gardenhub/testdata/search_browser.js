/* Browser check for the search panel (Lands workbench-glass spec WB-5, step 5.7), run from TestSearchUnderChromium against
   /garden on cardWorld, whose search reader is fixtureSearch (search_test.go). Usage:
   node search_browser.js <base url> <node_modules dir>.

   Desktop: Ctrl-K opens the '/' jump with its field focused; typing "edge" lists the map's matches and, last, "Search
   every bead for “edge”", which opens the panel: the address says q=edge, the hits come closest in meaning first, each
   saying what it is in words. The panel's field searches again in place: the address follows with no new entry, the
   focus stays in the field, an empty answer says so and the word-match note is shown. Ctrl-K with the panel open goes
   back to its field. A hit's title opens its bead here and Back returns to the hits; Escape closes the panel and drops
   q from the address. '/' on the map opens the jump too, and a query the map has nothing for puts the search row first.
   Phone: the Go button's jump leads to the panel over the farm. No console errors. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));

function hits(page) { return page.evaluate(function () { return Array.prototype.map.call(document.querySelectorAll('#bv-body .sv-row'), function (li) { return li.dataset.id; }); }); }
function search(page) { return page.evaluate(function () { return location.search; }); }
function active(page) { return page.evaluate(function () { var a = document.activeElement; return a ? (a.id || a.tagName.toLowerCase() + (a.type ? '[' + a.type + ']' : '')) : ''; }); }
async function shown(page, q) {
  await page.waitForFunction(function (q) {
    var p = document.getElementById('bead');
    return !p.hidden && p.dataset.kind === 'q' && document.getElementById('bv-title').textContent === q &&
      !/Loading/.test(document.querySelector('#bv-body [role=status]').textContent) && (document.querySelector('#bv-body .qv-list') || document.querySelector('#bv-body .bv-empty'));
  }, q);
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
  await page.waitForFunction(function () { return window.WORKBENCH && window.WORKBENCH.has('q') && document.querySelector('#hots .hot.cur'); });

  // Ctrl-K opens the jump; typing lists the map's matches, then the search row
  await page.keyboard.press('Control+k');
  await page.locator('#jump').waitFor({ state: 'visible' });
  assert.strictEqual(await active(page), 'jump-q', 'Ctrl-K focuses the jump\'s field');
  await page.keyboard.type('edge');
  var last = page.locator('#jump-list li').last();
  assert.strictEqual(await last.locator('.jt').textContent(), 'Search every bead for “edge”');
  assert.strictEqual(await last.locator('.jk').textContent(), 'search');
  await page.keyboard.press('ArrowUp');   // from the first row, round to the last
  assert.strictEqual(await last.getAttribute('aria-selected'), 'true');
  var before = await page.evaluate(function () { return history.length; });
  await page.keyboard.press('Enter');
  await shown(page, 'edge');
  assert.ok(await page.locator('#jump').isHidden(), 'the jump closes');
  assert.ok(/(^|[?&])q=edge($|&)/.test(await search(page)), 'the address names the panel: ' + await search(page));
  assert.deepStrictEqual(await hits(page), ['gd-planA', 'gd-FaceWork01', 'gd-FaceDecide1'], 'the reader\'s order');
  assert.strictEqual(await page.locator('#bead .bv-kind').textContent(), 'search');
  assert.ok(/Closest in meaning first\./.test(await page.locator('#bv-body').textContent()), 'how the hits were found');
  assert.strictEqual(await page.locator('#bv-body .sv-row').first().locator('.qv-line').textContent(), 'plan · in progress · gardenio · P1 · changed 3h ago');
  assert.strictEqual(await page.locator('#bv-body .sv-row[data-id="gd-FaceWork01"] a.qv-t').textContent(), 'Typed faces', 'the short title');
  assert.strictEqual(await page.locator('#bv-body .sv-form input').inputValue(), 'edge');

  // the field searches again in place, one history entry, the focus kept
  await page.fill('#bv-body .sv-form input', 'nothing here');
  await page.focus('#bv-body .sv-form input');
  await page.keyboard.press('Enter');
  await shown(page, 'nothing here');
  assert.ok(/q=nothing%20here/.test(await search(page)), 'the address follows: ' + await search(page));
  assert.ok(/No bead matches “nothing here”\./.test(await page.locator('#bv-body').textContent()), 'an empty answer says so');
  assert.strictEqual(await active(page), 'input[search]', 'the focus stays in the field');
  await page.fill('#bv-body .sv-form input', 'words');
  await page.keyboard.press('Enter');
  await shown(page, 'words');
  assert.deepStrictEqual(await hits(page), ['gd-FaceMsg0001']);
  assert.ok(/word matches only/.test(await page.locator('#bv-body').textContent()), 'the fallback is said');
  assert.strictEqual(await page.evaluate(function () { return history.length; }), before + 1, 'searching again adds no entry');

  // Ctrl-K with the panel open is its field
  await page.focus('#bv-title');
  await page.keyboard.press('Control+k');
  assert.strictEqual(await active(page), 'input[search]', 'Ctrl-K goes back to the field');
  assert.ok(await page.locator('#jump').isHidden(), 'and does not open the jump');

  // a hit opens its bead here; Back returns to the hits
  await page.fill('#bv-body .sv-form input', 'edge');
  await page.keyboard.press('Enter');
  await shown(page, 'edge');
  await page.click('#bv-body .sv-row[data-id="gd-FaceWork01"] a.qv-t');
  await page.waitForFunction(function () { return /bead=gd-FaceWork01/.test(location.search) && document.getElementById('bead').dataset.kind === 'bead'; });
  assert.ok(!/(^|[?&])q=/.test(await search(page)), 'the bead\'s address carries no query: ' + await search(page));
  await page.goBack();
  await shown(page, 'edge');
  assert.deepStrictEqual(await hits(page), ['gd-planA', 'gd-FaceWork01', 'gd-FaceDecide1'], 'Back is the hits as they were');

  // Escape closes, and the query leaves the address
  await page.focus('#bv-title');
  await page.keyboard.press('Escape');
  await page.waitForFunction(function () { return document.getElementById('bead').hidden; });
  assert.ok(!/(^|[?&])q=/.test(await search(page)), 'closing drops q: ' + await search(page));

  // '/' on the map opens the jump; a query the map has nothing for puts the search row first, and an id finds its bead
  await page.focus('#board');
  await page.keyboard.press('/');
  await page.locator('#jump').waitFor({ state: 'visible' });
  await page.keyboard.type('zzqx');
  assert.strictEqual(await page.locator('#jump-list li.sel .jk').textContent(), 'search', 'nothing on the map: the search row leads');
  await page.fill('#jump-q', 'gd-FaceWork01');
  await page.waitForFunction(function () { return /gd-FaceWork01/.test(document.querySelector('#jump-list li:last-child .jt').textContent); });
  await page.click('#jump-list li:last-child');
  await shown(page, 'gd-FaceWork01');
  assert.strictEqual(await page.locator('#bv-body .sv-row .qv-line').textContent(), 'the id you typed · task · in progress · gardenio · P2');
  await page.click('#bv-x');
  await page.waitForFunction(function () { return document.getElementById('bead').hidden; });

  assert.deepStrictEqual(errors, [], 'no console errors');
  await ctx.close();
}

async function phone(browser) {
  var ctx = await browser.newContext({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true }), page = await ctx.newPage(), errors = watch(page);
  await page.goto(url + '/garden');
  await page.waitForFunction(function () { return window.WORKBENCH && window.WORKBENCH.has('q') && document.querySelector('#hots .hot.cur'); });
  await page.tap('#cam-go');
  await page.locator('#jump').waitFor({ state: 'visible' });
  await page.fill('#jump-q', 'edge');
  await page.tap('#jump-list li:last-child');
  await shown(page, 'edge');
  assert.ok(await page.locator('#bead').isVisible(), 'the panel is on screen over the farm');
  assert.deepStrictEqual(await hits(page), ['gd-planA', 'gd-FaceWork01', 'gd-FaceDecide1']);
  await page.tap('#bv-x');
  await page.waitForFunction(function () { return document.getElementById('bead').hidden; });
  assert.deepStrictEqual(errors, [], 'no console errors');
  await ctx.close();
}

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  try { await desktop(browser); await phone(browser); console.log('search browser checks passed'); }
  finally { await browser.close(); }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
