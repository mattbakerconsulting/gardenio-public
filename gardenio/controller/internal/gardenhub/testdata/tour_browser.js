/* Browser check for the chore tour (gd-S2Cn2dTsP0), run from TestTourUnderChromium against /garden on an httptest server with
   weedsWorld, whose POST /b/{id}/{verb} stands in for the viewer's write endpoints (403 for gd-orph, 200 otherwise).
   Usage: node tour_browser.js <base url> <node_modules dir> <now, unix ms>. Date.now() is fixed at <now>, the farm's clock
   (weedsNow), so the stops' ages, and so their order, are the same on every run. GARDENHUB_SHOTS=<dir> saves a PNG at each step.

   Desktop: Tend opens the card on the first of five stops, the heaviest (the plan whose decision has waited longest) with focus
   on its title. The tour is a cursor walk (lands-cursor-grid-design.md section 8): at every stop the card sits on the board
   clear of the camera group and the header, the cursor (its .cur ring) is on the stop and framed in the map above
   the card, and no beacon sits on the card. Answer opens the ask in the bead view. A ping
   posts an addressed comment and, refused, says why on the card; Close and Defer post only on a second tap; Adopt claims; each
   success moves to the next stop and says what was done. The last stop ends on a summary with the cursor still on it; Done
   closes the card and hands focus back to Tend with the cursor left on the last stop, and Escape does the same from the
   keyboard. Phone, light and dark: the card is a sheet
   under the header, left of the camera strip, every button is 44 px tall, and the stop is framed between the sheet and the tab bar. No request leaves
   /garden but the tour's own posts; no console errors. */
'use strict';
var url = process.argv[2], mods = process.argv[3], now = +process.argv[4], assert = require('assert'), path = require('path');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));
var shots = process.env.GARDENHUB_SHOTS || '';
var STOPS = ['gd-planA', 'gd-orph', 'gd-A1b', 'gd-weedP', 'gd-weedS'];   // the five heaviest on the weeds' farm; the first is the oldest ask
var CURS = ['gd-planA', 'gardenio', 'gd-A1b', 'gd-weedP', 'gd-weedS'];    // where the cursor stands at each: the orphaned PR's closed bead is off the map, so its project

async function open(ctx, at, level) {
  var page = await ctx.newPage(), o = { page: page, errors: [], posts: [] };
  await page.clock.setFixedTime(now);
  page.on('console', function (m) { if (m.type() === 'error' && !/403 \(Forbidden\)/.test(m.text())) o.errors.push(m.text()); });
  page.on('pageerror', function (e) { o.errors.push(String(e)); });
  page.on('request', function (r) {
    var u = new URL(r.url());
    if (u.origin !== new URL(url).origin) return;
    if (r.method() === 'POST' && /^\/b\/[^/]+\/(comments|claim|defer|close)$/.test(u.pathname)) { o.posts.push([u.pathname, new URLSearchParams(r.postData() || '').get('text') || '']); return; }
    if (u.pathname !== '/garden' && u.pathname.indexOf('/garden/') !== 0) o.errors.push('left the garden: ' + r.method() + ' ' + u.pathname);
  });
  await page.goto(url + '/garden?at=' + at + '&z=' + level);
  await page.waitForSelector('#hots .hot', { state: 'attached' });
  return o;
}
async function shot(page, name) { if (shots) await page.screenshot({ path: path.join(shots, 'tour-' + name + '.png') }); }
function overlap(a, b) { return a.x < b.x + b.width && b.x < a.x + a.width && a.y < b.y + b.height && b.y < a.y + a.height; }
/* The card, the stop it shows (from its Open link) and the geometry the checks need, once the camera has settled. */
async function card(page) {
  await page.locator('#tour').waitFor({ state: 'visible' });
  await page.waitForFunction(function () { var r = document.querySelector('#hots .hot.cur'); return r && r.getBoundingClientRect().width > 0; });
  // An action's post is answered asynchronously: read the card once it has (the next stop, or the refusal), not mid-send.
  await page.waitForFunction(function () { var n = document.querySelector('#tour .tour-note'); return !n || n.textContent !== 'Sending…'; });
  return page.evaluate(function () {
    var t = document.getElementById('tour'), rect = function (e) { if (!e) return null; var b = e.getBoundingClientRect(); return b.width && b.height ? { x: b.left, y: b.top, width: b.width, height: b.height } : null; };
    var open = t.querySelector('.tour-open'), bead = open ? new URL(open.href).searchParams.get('bead') : '', cur = document.querySelector('#hots .hot.cur');
    return {
      n: t.querySelector('#tour-n').textContent, title: t.querySelector('#tour-h').textContent, bead: bead, note: (t.querySelector('.tour-note') || {}).textContent || '',
      acts: Array.prototype.map.call(t.querySelectorAll('.tour-acts button'), function (b) { return b.textContent; }),
      first: (t.querySelector('.tour-acts .first') || {}).textContent || '', focus: document.activeElement && document.activeElement.id,
      cur: cur.href ? new URL(cur.href).searchParams.get('bead') : cur.getAttribute('aria-label').split(':')[0], card: rect(t), ring: rect(cur), board: rect(document.getElementById('board')), hdr: rect(document.querySelector('header.crumb')),
      tabs: rect(document.getElementById('tabbar')), cam: rect(document.querySelector('.cam')),
      bcns: Array.prototype.map.call(document.querySelectorAll('#bcns .bcn'), rect).filter(Boolean),
      buttons: Array.prototype.map.call(t.querySelectorAll('button,a'), function (b) { return Math.round(b.getBoundingClientRect().height); })
    };
  });
}
/* Every stop: the card on the board and off the controls, the cursor's ring in the free map beside it, no arrow on the card. */
function placed(c, phone) {
  var k = c.card, r = c.ring, b = c.board;
  assert.strictEqual(c.cur, CURS[STOPS.indexOf(c.bead)], 'the cursor is on the stop: ' + c.bead);
  assert.ok(k.x >= b.x - 1 && k.x + k.width <= b.x + b.width + 1 && k.y + k.height <= b.y + b.height + 1, 'the card is on the board: ' + JSON.stringify([k, b]));
  assert.ok(k.y >= c.hdr.y + c.hdr.height - 1, 'the card is under the header');
  if (c.cam) assert.ok(!overlap(k, c.cam), 'the card is off the controls: ' + JSON.stringify([k, c.cam]));
  c.bcns.forEach(function (x) { assert.ok(!overlap(k, x), 'no beacon on the card: ' + JSON.stringify([k, x])); });
  var cy = r.y + r.height / 2, cx = r.x + r.width / 2;
  if (phone) {
    assert.ok(cy > k.y + k.height && cy < c.tabs.y, 'a phone frames the stop between the sheet and the tab bar: ' + JSON.stringify([r, k, c.tabs]));
    assert.ok(k.x <= b.x + 1 && k.width >= b.width * 0.7, 'a phone card is a sheet from the left edge to the camera strip: ' + JSON.stringify([k, b]));
    c.buttons.forEach(function (h) { assert.ok(h >= 44, 'a 44 px touch target: ' + JSON.stringify(c.buttons)); });
  } else {
    assert.ok(r.y + r.height <= k.y + 1 && r.y >= c.hdr.y + c.hdr.height - 1 && r.x >= b.x - 1 && r.x + r.width <= b.x + b.width + 1, 'a desktop frames the stop above the card: ' + JSON.stringify([r, k]));
  }
}
function curBead(page) { return page.evaluate(function () { var n = document.querySelector('#hots .hot.cur'); return n && n.href ? new URL(n.href).searchParams.get('bead') : ''; }); }
async function click(page, text) { await page.locator('#tour .tour-acts button', { hasText: text }).click(); }
function done(o, what) { assert.deepStrictEqual(o.errors, [], what + ': console errors or a request outside /garden'); }

async function desktop(browser) {
  var ctx = await browser.newContext({ viewport: { width: 1280, height: 800 }, reducedMotion: 'reduce' });
  var o = await open(ctx, 'gardenio', 1), page = o.page, seen = [];
  await page.click('#cam-menu');   // Tend is in the camera's Menu (3b R8)
  await page.click('#cam-tend');
  assert.strictEqual(await page.getAttribute('#cam-tend', 'aria-expanded'), 'true');
  assert.ok(await page.locator('#menu-list').isHidden(), 'a choice closes the Menu');

  // 1. The oldest ask: answered in the bead view, inside the garden.
  var c = await card(page);
  assert.strictEqual(c.n, 'Tend the garden · stop 1 of 5'); assert.strictEqual(c.focus, 'tour-h', 'focus on the stop\'s title');
  assert.strictEqual(c.bead, 'gd-planA'); assert.deepStrictEqual(c.acts, ['Answer']);
  placed(c, false); seen.push(c.bead);
  await shot(page, 'ask-desktop-light');
  await click(page, 'Answer');
  await page.locator('#bead').waitFor({ state: 'visible' });
  await page.click('#bv-x');
  await page.locator('#tour .tour-next').click();

  // 2. The orphaned PR: a ping to its bead's gardener, refused here, says why and stays.
  c = await card(page);
  assert.strictEqual(c.bead, 'gd-orph'); assert.deepStrictEqual(c.acts, ['Ping rae']); placed(c, false); seen.push(c.bead);
  await shot(page, 'orphan-desktop-light');
  await click(page, 'Ping rae');
  await page.waitForFunction(function () { var n = document.querySelector('#tour .tour-note.err'); return n && n.textContent; });
  c = await card(page);
  assert.strictEqual(c.note, 'You may not comment on gd-orph.'); assert.strictEqual(c.bead, 'gd-orph', 'a refusal stays on the stop');
  assert.ok(/^@rae the chore tour in \/garden stopped at gd-orph\. Orphaned PR: PR #77 in o\/r is still open/.test(o.posts[0][1]), JSON.stringify(o.posts));
  await page.locator('#tour .tour-next').click();

  // 3. A wilted step: Close posts only on the second tap.
  c = await card(page);
  assert.strictEqual(c.bead, 'gd-A1b'); assert.deepStrictEqual(c.acts, ['Ping mo', 'Adopt', 'Defer', 'Close']); assert.strictEqual(c.first, 'Ping mo', 'the suggestion comes first');
  placed(c, false); seen.push(c.bead);
  await shot(page, 'wilt-desktop-light');
  await click(page, 'Close');
  assert.strictEqual(await page.locator('#tour .tour-acts .armed').textContent(), 'Confirm: Close');
  assert.strictEqual(o.posts.length, 1, 'the first tap only arms Close');
  await click(page, 'Confirm: Close');

  // 4. The Commons plan, two weeds on one stop: adopted.
  c = await card(page);
  assert.strictEqual(c.note, 'Closed gd-A1b.', 'the next card says what was done');
  assert.strictEqual(c.bead, 'gd-weedP'); placed(c, false); seen.push(c.bead);
  assert.strictEqual(await page.locator('#tour .tour-why').count(), 2, 'one stop names both weeds');
  await click(page, 'Adopt');

  // 5. The long-frosted draft: deferred on the second tap, then the summary.
  c = await card(page);
  assert.strictEqual(c.note, 'Adopted gd-weedP: it is yours, in progress.');
  assert.strictEqual(c.bead, 'gd-weedS'); assert.strictEqual(c.n, 'Tend the garden · stop 5 of 5'); placed(c, false); seen.push(c.bead);
  await shot(page, 'seed-desktop-light');
  await click(page, 'Defer'); await click(page, 'Confirm: Defer');
  await page.waitForFunction(function () { return document.getElementById('tour-n').textContent === 'Tend the garden · done'; });
  assert.strictEqual(await page.locator('#tour-h').textContent(), 'You tended 3 things.');
  assert.strictEqual(await page.locator('#tour .tour-note').textContent(), 'Deferred gd-weedS.');
  assert.strictEqual((await card(page)).cur, 'gd-weedS', 'the summary leaves the cursor on the last stop');
  await shot(page, 'done-desktop-light');
  assert.deepStrictEqual(seen, STOPS, 'the five heaviest, in order');
  assert.deepStrictEqual(o.posts.map(function (p) { return p[0]; }), ['/b/gd-orph/comments', '/b/gd-A1b/close', '/b/gd-weedP/claim', '/b/gd-weedS/defer']);

  // Done hands focus back to the Menu (Tend is in it, closed); from the keyboard, Enter opens the Menu, Down walks to Tend,
  // Enter opens the tour and Escape ends it the same way.
  await page.locator('#tour .tour-next', { hasText: 'Done' }).click();
  assert.ok(await page.locator('#tour').isHidden()); assert.strictEqual(await page.getAttribute('#cam-tend', 'aria-expanded'), 'false');
  assert.strictEqual(await curBead(page), 'gd-weedS', 'Done leaves the cursor on the last stop');
  await page.waitForFunction(function () { return new URL(location.href).searchParams.get('at') === 'gardenio/gd-weedS'; });   // and the address names it
  assert.strictEqual(await page.evaluate(function () { return document.activeElement.id; }), 'cam-menu');
  await page.keyboard.press('Enter');
  assert.strictEqual(await page.getAttribute('#cam-menu', 'aria-expanded'), 'true');
  for (var k = 0; k < 5; k++) await page.keyboard.press('ArrowDown');   // Home, All, Plans, Outline, Tend
  assert.strictEqual(await page.evaluate(function () { return document.activeElement.id; }), 'cam-tend');
  await page.keyboard.press('Enter');
  c = await card(page);
  assert.strictEqual(c.focus, 'tour-h'); assert.strictEqual(c.bead, 'gd-planA');
  await page.keyboard.press('Escape');
  assert.ok(await page.locator('#tour').isHidden());
  assert.strictEqual(await page.evaluate(function () { return document.activeElement.id; }), 'cam-menu');
  assert.strictEqual(await curBead(page), 'gd-planA', 'Escape leaves the cursor on the stop the tour was at');
  done(o, 'desktop'); await ctx.close();
}

async function phone(browser, scheme) {
  var ctx = await browser.newContext(Object.assign({}, pw.devices['iPhone 12'], { viewport: { width: 390, height: 844 }, reducedMotion: 'reduce', colorScheme: scheme }));
  var o = await open(ctx, '~/gd-weedP', 1), page = o.page;
  await page.tap('#cam-menu');
  await page.tap('#cam-tend');
  for (var i = 0; i < STOPS.length; i++) {
    var c = await card(page);
    assert.strictEqual(c.bead, STOPS[i]); placed(c, true);
    if (i === 0 || i === 2) await shot(page, (i ? 'wilt' : 'ask') + '-phone-' + scheme);
    await page.locator('#tour .tour-next').tap();
  }
  assert.strictEqual(await page.locator('#tour-h').textContent(), 'Nothing tended this time.');
  await page.locator('#tour .tour-x').tap();
  assert.ok(await page.locator('#tour').isHidden());
  assert.deepStrictEqual(o.posts, [], 'walking past every stop posts nothing');
  done(o, 'phone ' + scheme); await ctx.close();
}

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  try { await desktop(browser); await phone(browser, 'light'); await phone(browser, 'dark'); console.log('tour browser checks passed'); }
  finally { await browser.close(); }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
