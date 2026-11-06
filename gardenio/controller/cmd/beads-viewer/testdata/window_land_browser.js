/* A land window's page in a real Chromium (LW.4 gd-OJOsIH52NE), run from TestWindowLandUnderChromium against the viewer's
   routes on an httptest server, the window reading internal/planwindow's mixed land fixture. Usage: node
   window_land_browser.js <page url> <node_modules dir>; WINDOW_LAND_SHOTS=<dir> saves a screenshot per state and
   WINDOW_LAND_PROBES=<probes.js> runs the commons-ux probes on each (<state>.probes.json beside it).

   At a phone's size (touch) and a desktop's, by day and by night: land.js shows the canvas and draws the World on it in many
   colours; a tap on a plot opens a plan's card, a plan's name in the list opens that plan's card with focus in it, and Escape
   closes it again; on the desktop a drag and the wheel move the picture; nothing is fetched outside /w/ and /lands/static/
   and nothing throws. With JS off the canvas stays hidden and the list is the page. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert'), path = require('path'), fs = require('fs');
var shots = process.env.WINDOW_LAND_SHOTS || '', probesJs = process.env.WINDOW_LAND_PROBES || '';
var pw = require(require.resolve('playwright-core', { paths: [mods] }));
var probes = probesJs ? require(path.resolve(probesJs)) : null;
var TITLES = ['Grow the orchard', 'Sketch a pond', 'Build the shed', 'withheld: contains an internal reference', 'Huge plan', 'Clear the brambles'];
var CELLS = [
  { name: 'phone-light', ctx: { viewport: { width: 390, height: 844 }, deviceScaleFactor: 2, hasTouch: true, isMobile: true, colorScheme: 'light' }, night: false },
  { name: 'phone-dark', ctx: { viewport: { width: 390, height: 844 }, deviceScaleFactor: 2, hasTouch: true, isMobile: true, colorScheme: 'dark' }, night: true },
  { name: 'desktop-light', ctx: { viewport: { width: 1440, height: 900 }, colorScheme: 'light' }, night: false },
  { name: 'desktop-dark', ctx: { viewport: { width: 1440, height: 900 }, colorScheme: 'dark' }, night: true },
];

async function keep(page, name, coarse) {
  if (!shots) return;
  await page.screenshot({ path: path.join(shots, name + '.png') });
  if (probes) fs.writeFileSync(path.join(shots, name + '.probes.json'), JSON.stringify(await page.evaluate(probes.run, { root: '#w-land', coarse: coarse }), null, 2));
}
// What the canvas holds: its distinct colours and a hash of its pixels.
function canvas(page) {
  return page.evaluate(function () {
    var cv = document.getElementById('w-cv'), d = cv.getContext('2d').getImageData(0, 0, cv.width, cv.height).data, seen = {}, h = 0;
    for (var k = 0; k < d.length; k += 4) { var c = d[k] << 16 | d[k + 1] << 8 | d[k + 2]; seen[c] = 1; h = (h * 31 + c) | 0; }
    return { colours: Object.keys(seen).length, hash: h };
  });
}
function cardTitle(page) {
  return page.evaluate(function () { var c = document.getElementById('w-card'), h = c.querySelector('h2'); return c.hidden ? '' : h ? h.textContent : '?'; });
}
async function settle(page) { await page.waitForTimeout(400); }

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); } catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  try {
    for (var cell of CELLS) {
      var ctx = await browser.newContext(cell.ctx), page = await ctx.newPage(), errs = [], asked = [], touch = !!cell.ctx.hasTouch;
      page.on('pageerror', function (e) { errs.push(String(e)); });
      page.on('request', function (r) { asked.push(new URL(r.url()).pathname); });
      // the skin clock follows the local hour: noon by day, ten at night
      await page.clock.setFixedTime(new Date(cell.night ? '2026-10-30T22:00:00' : '2026-10-30T12:00:00'));
      await page.goto(url);
      await page.waitForSelector('#w-world:not([hidden])', { timeout: 15000 });
      var skin = await page.evaluate(function () { return document.documentElement.getAttribute('data-skin'); });
      assert.strictEqual(skin, cell.night ? 'dark-homestead' : 'homestead', cell.name + ': skin');
      var c0 = { colours: 0 };
      for (var i = 0; i < 40 && c0.colours <= 12; i++) { await settle(page); c0 = await canvas(page); }
      assert.ok(c0.colours > 12, cell.name + ': only ' + c0.colours + ' colours on the canvas');
      await settle(page);
      await keep(page, cell.name, touch);

      if (!touch) {
        // the keyboard reaches the map, and its focus ring shows
        for (var tabs = 0; tabs < 6 && !(await page.evaluate(function () { return document.activeElement && document.activeElement.id === 'w-cv'; })); tabs++) await page.keyboard.press('Tab');
        assert.ok(await page.evaluate(function () { return document.activeElement.id === 'w-cv'; }), cell.name + ': Tab never reached the map');
        await keep(page, cell.name + '-focus', touch);
        var f0 = (await canvas(page)).hash;
        await page.keyboard.press('ArrowRight');
        await page.keyboard.press('+');
        await settle(page);
        assert.notStrictEqual((await canvas(page)).hash, f0, cell.name + ': the keys did not move the map');
      }

      // a tap on the map: try a grid until one lands on a plot, a tree or a pot
      var box = await page.locator('#w-cv').boundingBox(), opened = '';
      for (var gy = 1; gy < 12 && !opened; gy++) for (var gx = 1; gx < 8 && !opened; gx++) {
        var x = box.x + box.width * gx / 8, y = box.y + box.height * gy / 12;
        if (touch) await page.touchscreen.tap(x, y); else await page.mouse.click(x, y);
        opened = await cardTitle(page);
      }
      assert.ok(TITLES.indexOf(opened) >= 0, cell.name + ': a tap on the map opened ' + JSON.stringify(opened));
      await settle(page);
      await keep(page, cell.name + '-tap', touch);
      await page.keyboard.press('Escape');
      await page.locator('#w-cv').focus();
      await page.keyboard.press('Escape');
      assert.strictEqual(await cardTitle(page), '', cell.name + ': Escape left the card open');

      // the list: a plan's name opens its card, with focus in it
      await page.locator('#w-plans li[data-plan="plan-1"] button.w-pick').click();
      assert.strictEqual(await cardTitle(page), 'Grow the orchard', cell.name + ': the list opened the wrong card');
      var inCard = await page.evaluate(function () { return document.getElementById('w-card').contains(document.activeElement); });
      assert.ok(inCard, cell.name + ': focus did not move into the card');
      var words = await page.locator('#w-card').innerText();
      ['Building', '2 of 5 steps done', 'Phases', 'Dig', 'Plant', 'Waiting on the operator', 'Crew', 'crew-a', 'Working now'].forEach(function (w) {
        assert.ok(words.indexOf(w) >= 0, cell.name + ': the card lacks ' + JSON.stringify(w) + ':\n' + words);
      });
      assert.strictEqual(await page.locator('#w-card a, #w-card form, #w-card input').count(), 0, cell.name + ': the card has a link or a form');
      await settle(page);
      await page.evaluate(function () { window.scrollTo(0, 0); });
      await keep(page, cell.name + '-card', touch);
      await page.keyboard.press('Escape');
      assert.strictEqual(await cardTitle(page), '', cell.name + ': Escape did not close the card');
      var back = await page.evaluate(function () { return document.activeElement && document.activeElement.className; });
      assert.ok(/w-pick/.test(back), cell.name + ': focus did not go back to the list (' + back + ')');

      if (!touch) {
        var a = await canvas(page);
        await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
        await page.mouse.down();
        await page.mouse.move(box.x + box.width / 2 + 120, box.y + box.height / 2 + 60, { steps: 6 });
        await page.mouse.up();
        await settle(page);
        var b = await canvas(page);
        assert.notStrictEqual(a.hash, b.hash, cell.name + ': a drag did not move the map');
        await page.mouse.wheel(0, -400);
        await settle(page);
        assert.notStrictEqual((await canvas(page)).hash, b.hash, cell.name + ': the wheel did not zoom');
        assert.strictEqual(await cardTitle(page), '', cell.name + ': a drag opened a card');
      }

      var stray = asked.filter(function (p) { return !/^\/w\//.test(p) && !/^\/lands\/static\//.test(p); });
      assert.deepStrictEqual(stray, [], cell.name + ': fetched outside /w/ and the skin');
      assert.deepStrictEqual(errs, [], cell.name + ': the page threw');
      console.log(cell.name + ': ' + c0.colours + ' colours; tap opened ' + JSON.stringify(opened) + '; ' + asked.length + ' requests');
      await ctx.close();
    }

    // JS off: the canvas stays hidden and the list is the page
    var off = await browser.newContext({ viewport: { width: 390, height: 844 }, deviceScaleFactor: 2, hasTouch: true, isMobile: true, javaScriptEnabled: false });
    var p = await off.newPage();
    await p.goto(url);
    assert.strictEqual(await p.locator('#w-world').isVisible(), false, 'nojs: the canvas shows');
    assert.strictEqual(await p.locator('#w-plans li[data-plan]').count(), TITLES.length, 'nojs: the list');
    assert.strictEqual(await p.locator('button').count(), 0, 'nojs: a button that cannot work');
    await keep(p, 'phone-nojs', true);
    if (shots) await p.screenshot({ path: path.join(shots, 'phone-nojs-full.png'), fullPage: true });
    await off.close();
    console.log('nojs: the list alone');
  } finally { await browser.close(); }
})().catch(function (e) { console.error(e); process.exit(1); });
