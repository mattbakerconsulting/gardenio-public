/* Browser check that every text panel on the /garden farm scrolls under a finger on a phone (gd-O24RP2gSmc), run from
   TestTextPanelsTouchScrollUnderChromium against a live /garden on an httptest server with the fixture farm, a dozen agents
   on the Porch and a dozen decisions in the Mailbox. Usage: node touch_scroll_browser.js <base url> <node_modules dir>.

   The wheel checks (drawer_scroll_browser.js) do not cover a phone: a touch scroll is a gesture the page can refuse with
   touch-action, pointer capture or a preventDefault on the move, none of which the wheel sees. Each panel here is opened
   the way a phone user opens it, given more content than fits (a padded tail, so the check does not depend on how big the
   fixture farm is), and scrolled with a finger (a CDP Input.dispatchTouchEvent start, moves and end; synthesizeScrollGesture's touch source does nothing headless); the
   scroller must move. Phone: iPhone 12 (390x844), touch on.
   The World is the other case (gd-cihiPae1J9): the map takes its own gestures (touch-action:none on the board), so a finger
   dragged over it and a two-finger spread on it move the map, never the page: no document scroll, no visual viewport pan or
   zoom. And a decision option whose button shows a short word says its whole label when a tap arms it (a phone has no title
   tooltip). */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));
/* Every context pins the sky (web/sky.js, gd-SwRqFcjWc6) to its emulated colour scheme: light is day and dark is night
   whatever the clock says, so a run at night (CI is UTC) sees the page a run by day does. */
function skyCtx(browser, opts) {
  opts = opts || {};
  return browser.newContext(opts).then(function (ctx) {
    return ctx.addInitScript(function (s) { try { localStorage.setItem('garden.sky', s); } catch (e) { /* no storage: the emulated scheme still decides by day */ } }, opts.colorScheme === 'dark' ? 'night' : 'day').then(function () { return ctx; });
  });
}

/* The finger's lift starts a fling; wait until it is over (scrollTop still across two reads) before going on. A tap while
   Chromium still flings only stops the fling and never clicks (its tap suppression), so a header tapped too soon after the
   scroll did not close its drawer on a loaded runner (gd-2N3EdfCfja). */
async function settled(page, scroller) {
  var last = -1;
  for (var i = 0; i < 50; i++) {
    await page.waitForTimeout(i ? 100 : 400);
    var top = await page.evaluate(function (s) { return document.querySelector(s).scrollTop; }, scroller);
    if (top === last) return;
    last = top;
  }
}

// Where the page itself is: its scroll and the visual viewport's offset and zoom. A gesture the map takes moves none of them.
function pageAt(page) {
  return page.evaluate(function () { var vv = window.visualViewport || { offsetLeft: 0, offsetTop: 0, scale: 1 }; return { sx: window.scrollX, sy: window.scrollY, vx: Math.round(vv.offsetLeft), vy: Math.round(vv.offsetTop), scale: Math.round(vv.scale * 100) / 100 }; });
}
var STILL = { sx: 0, sy: 0, vx: 0, vy: 0, scale: 1 };
// Fingers on the board's middle: one dragged up by 200 px, or two spread from 40 to 220 px apart (n 2).
async function onBoard(page, cdp, n) {
  var r = await page.locator('#board').boundingBox(), x = Math.round(r.x + r.width / 2), y = Math.round(r.y + r.height / 2);
  var pts = function (i) { return n === 2 ? [{ id: 1, x: x - 20 - 7.5 * i, y: y }, { id: 2, x: x + 20 + 7.5 * i, y: y }] : [{ id: 1, x: x, y: y + 100 - 8 * i }]; };
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: pts(0) });
  for (var i = 1; i <= 24; i++) { await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: pts(i) }); await page.waitForTimeout(16); }
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
  await page.waitForTimeout(500);
}

// A drawer's header closes it: tap it and wait for [open] to go, tapping once more if the first tap was swallowed.
async function closeDrawer(page, id) {
  var shut = function (d) { return !document.getElementById(d).open; };
  await page.tap('#' + id + ' > summary');
  try { await page.waitForFunction(shut, id, { timeout: 3000 }); }
  catch (e) { await page.tap('#' + id + ' > summary'); await page.waitForFunction(shut, id); }
}

// A finger drags the scroller up by dy px, starting at its centre. The scroller must have moved afterwards.
async function fingerScrolls(page, cdp, name, scroller) {
  await page.evaluate(function (s) {
    var e = document.querySelector(s);
    if (!e) throw new Error('no scroller ' + s);
    e.scrollTop = 0;
    var pad = document.createElement('div'); pad.className = 'probe-pad'; pad.style.height = '3000px'; e.appendChild(pad);
  }, scroller);
  var r = await page.locator(scroller).boundingBox();
  assert.ok(r && r.height > 60, name + ': the scroller is on screen: ' + JSON.stringify(r));
  var before = await page.evaluate(function (s) { var e = document.querySelector(s); return { ch: e.clientHeight, sh: e.scrollHeight, top: e.scrollTop, ta: getComputedStyle(e).touchAction }; }, scroller);
  assert.ok(before.ch < before.sh, name + ': the scroller is shorter than its content: ' + JSON.stringify(before));
  var x = Math.round(r.x + r.width / 2), y0 = Math.round(r.y + r.height * 0.75), y1 = Math.round(r.y + r.height * 0.25);
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x: x, y: y0, id: 1 }] });
  for (var y = y0 - 8; y >= y1; y -= 8) await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ x: x, y: y, id: 1 }] });
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
  await settled(page, scroller);
  var after = await page.evaluate(function (s) { var e = document.querySelector(s); return { top: e.scrollTop, ta: getComputedStyle(e).touchAction }; }, scroller);
  assert.ok(after.top > 0, name + ': a finger scrolls it (touch-action ' + before.ta + '): ' + JSON.stringify([before, after]));
  await page.evaluate(function (s) { var p = document.querySelector(s + ' .probe-pad'); if (p) p.remove(); }, scroller);
  console.log(name + ': finger scrolled to ' + after.top + ' (touch-action ' + before.ta + ')');
}

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  try {
    var ctx = await skyCtx(browser, Object.assign({}, pw.devices['iPhone 12'], { viewport: { width: 390, height: 844 }, reducedMotion: 'reduce' }));
    var page = await ctx.newPage(), errors = [], failed = [];
    page.on('console', function (m) { if (m.type() === 'error') errors.push(m.text()); });
    page.on('pageerror', function (e) { errors.push(String(e)); });
    await page.goto(url + '/garden');
    await page.waitForFunction(function () { return document.querySelectorAll('#d-mail > li').length > 5 && document.querySelectorAll('#d-porch > li').length > 5 && document.querySelectorAll('#hots .hot').length > 0; });   // the cursor's grid: at depth 0 its projects' buttons
    var cdp = await ctx.newCDPSession(page);

    // The World: a drag and a pinch on the map are the map's (its cursor, its depth), never the page's.
    try {
      assert.strictEqual(await page.evaluate(function () { return getComputedStyle(document.getElementById('board')).touchAction; }), 'none', 'the board takes its own gestures');
      var grid0 = await page.evaluate(function () { return document.getElementById('hots').getAttribute('aria-label'); });
      await onBoard(page, cdp, 1);
      assert.deepStrictEqual(await pageAt(page), STILL, 'a finger dragged over the map moves no page');
      await onBoard(page, cdp, 2);
      assert.deepStrictEqual(await pageAt(page), STILL, 'a pinch on the map zooms no page');
      var grid1 = await page.evaluate(function () { return document.getElementById('hots').getAttribute('aria-label'); });
      assert.notStrictEqual(grid1, grid0, 'the pinch went into the map instead');
      console.log('phone World: a drag and a pinch moved the map (' + grid0 + ' -> ' + grid1 + '), not the page');
    } catch (e) { failed.push(String(e.message || e)); console.log('FAIL ' + (e.message || e)); }

    // Outline: the text model of the map, opened from the camera's Menu.
    await page.tap('#cam-menu');
    await page.tap('#cam-outline');
    await page.waitForFunction(function () { return document.getElementById('outline').classList.contains('open'); });
    try { await fingerScrolls(page, cdp, 'phone Outline', '#outline .otree'); } catch (e) { failed.push(String(e.message || e)); console.log('FAIL ' + (e.message || e)); }
    await page.tap('#outline-x');

    // Plans sheet.
    await page.tap('#cam-menu'); await page.tap('#cam-plans');   // Plans is in the camera's Menu (3b R9); its header closes it
    await page.locator('#plans-slot').waitFor({ state: 'visible' });
    try { await fingerScrolls(page, cdp, 'phone Plans', '#dw-plans > .dbody'); } catch (e) { failed.push(String(e.message || e)); console.log('FAIL ' + (e.message || e)); }
    await closeDrawer(page, 'dw-plans');

    // Legend & keys sheet.
    await page.tap('#cam-menu'); await page.tap('#cam-help');
    await page.locator('#legend').waitFor({ state: 'visible' });
    try { await fingerScrolls(page, cdp, 'phone Legend', '#dw-help > .dbody'); } catch (e) { failed.push(String(e.message || e)); console.log('FAIL ' + (e.message || e)); }
    await closeDrawer(page, 'dw-help');

    // Jump dialog list.
    await page.tap('#cam-go');
    await page.locator('#jump').waitFor({ state: 'visible' });
    await page.fill('#jump-q', 'p');
    await page.waitForFunction(function () { return document.querySelectorAll('#jump-list li').length > 0; });
    try { await fingerScrolls(page, cdp, 'phone Jump list', '#jump-list'); } catch (e) { failed.push(String(e.message || e)); console.log('FAIL ' + (e.message || e)); }
    await page.tap('#jump-close');

    // Bead view, opened from a plot on the map.
    await page.goto(url + '/garden?bead=gd-planA');
    await page.locator('#bead').waitFor({ state: 'visible' });
    await page.waitForFunction(function () { return document.getElementById('bv-body').textContent.trim().length > 0; });
    cdp = await ctx.newCDPSession(page);
    try { await fingerScrolls(page, cdp, 'phone Bead view', '#bv-body'); } catch (e) { failed.push(String(e.message || e)); console.log('FAIL ' + (e.message || e)); }

    // Mailbox screen (the dock), from the tab bar.
    await page.tap('#tabbar [data-go=mail]');
    await page.locator('#pane-mail').waitFor({ state: 'visible' });
    try { await fingerScrolls(page, cdp, 'phone Mailbox', '#dock'); } catch (e) { failed.push(String(e.message || e)); console.log('FAIL ' + (e.message || e)); }
    // Its first decision's Wait: the button says the short word, its first tap the whole label.
    var WAIT = '#d-mail > li [data-act=resolve][data-opt=b]';
    try {
      assert.strictEqual((await page.locator(WAIT).first().textContent()).trim(), 'Wait');
      await page.locator(WAIT).first().tap();
      assert.strictEqual((await page.locator(WAIT).first().textContent()).trim(), 'Confirm: Wait for the walk', 'a tap arms the option in its whole words');
      console.log('phone Mailbox: a tap on Wait reads Confirm: Wait for the walk');
    } catch (e) { failed.push(String(e.message || e)); console.log('FAIL ' + (e.message || e)); }

    assert.deepStrictEqual(failed, [], 'panels a finger cannot scroll');
    assert.deepStrictEqual(errors.filter(function (e) { return !/404/.test(e); }), [], 'phone console errors');
    await ctx.close();
    console.log('touch scroll browser checks passed');
  } finally { await browser.close(); }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
