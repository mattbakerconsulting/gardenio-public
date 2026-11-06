/* Browser check for an escalation's executor beside its icon (plan gd-PJB3D7glwF step 7 gd-B5aDmNhmjz, WORLD.escStands), run
   from TestEscalationExecutorUnderChromium against a live /garden with escyard's world and one executor of gd-Eaccess that has no
   task. Usage: node escexec_browser.js <base url> <node_modules dir> [shots dir].

   Its link (a.w-gesc) is beside the icon's cell: on its row, right of it, clear of every other icon. Hover shows the
   steward's tip (#stip) with its name and "Running access escalation: Escalation k8s_rbac"; the link is its bead. */
'use strict';
var url = process.argv[2], mods = process.argv[3], shots = process.argv[4] || '', assert = require('assert'), path = require('path');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));
var CELL = '#hots a.w-escalation[href$="bead=gd-Eaccess"]', FIG = '#ghots a.w-gesc';

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  try {
    var errors = [], ctx = await browser.newContext({ viewport: { width: 1280, height: 800 }, reducedMotion: 'reduce', colorScheme: 'light' });
    await ctx.addInitScript(function () { try { localStorage.setItem('garden.sky', 'day'); } catch (e) { /* the scheme decides */ } });
    var page = await ctx.newPage();
    page.on('pageerror', function (e) { errors.push(String(e)); });
    await page.goto(url + '/garden?at=gardenio/esc%3Agd-Eaccess');
    await page.waitForSelector(CELL + '.cur', { state: 'attached', timeout: 15000 });
    await page.waitForSelector(FIG, { state: 'attached', timeout: 15000 });
    assert.strictEqual(await page.locator(FIG).count(), 1, 'one Escalations stand: the executor of gd-Eaccess');
    var box = function (sel) { return page.locator(sel).first().evaluate(function (a) { var r = a.getBoundingClientRect(); return { x: r.left, y: r.top, w: r.width, h: r.height }; }); };
    var cell = await box(CELL), fig = await box(FIG);
    assert.ok(fig.x >= cell.x + cell.w - 1, 'right of its icon: ' + JSON.stringify([cell, fig]));
    assert.ok(Math.abs((fig.y + fig.h) - (cell.y + cell.h)) < 2, 'feet on the icon\'s foot: ' + JSON.stringify([cell, fig]));
    var others = await page.$$eval('#hots a.w-escalation', function (as) { return as.map(function (a) { var r = a.getBoundingClientRect(); return { x: r.left, y: r.top, w: r.width, h: r.height }; }); });
    var foot = { x: fig.x, y: fig.y + fig.h / 3, w: fig.w, h: fig.h * 2 / 3 };   // its body: the head may rise into the row above
    assert.ok(others.every(function (o) { return !(o.x < foot.x + foot.w - 0.5 && foot.x < o.x + o.w - 0.5 && o.y < foot.y + foot.h - 0.5 && foot.y < o.y + o.h - 0.5); }), 'it covers no icon');
    assert.ok(/bead=gd-gEscExec$/.test(await page.locator(FIG).getAttribute('href')), 'its link is its bead');
    await page.mouse.move(1270, 790);
    if (shots) await page.screenshot({ path: path.join(shots, 'escexec-beside.png'), animations: 'disabled', caret: 'hide' });
    await page.hover(FIG, { force: true });
    await page.waitForFunction(function () { var t = document.getElementById('stip'); return t && !t.hidden; }, null, { timeout: 5000 });
    var tip = await page.locator('#stip').innerText();
    assert.ok(tip.indexOf('escalation-gd-eaccess') === 0, 'its name heads the tip: ' + tip);
    assert.ok(tip.indexOf('Running access escalation: Escalation k8s_rbac') > 0, 'what it runs: ' + tip);
    if (shots) await page.screenshot({ path: path.join(shots, 'escexec-hover.png'), animations: 'disabled', caret: 'hide' });
    assert.deepStrictEqual(errors, [], 'no page errors');
    console.log('ok');
  } finally { await browser.close(); }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
