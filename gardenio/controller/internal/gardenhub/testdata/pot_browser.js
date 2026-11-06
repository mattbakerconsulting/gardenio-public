/* Browser check for a Nursery pot's card (9.11b, gd-m38yKRX0hE), run from TestPotUnderChromium against a live /garden on an
   httptest server with the fixture farm, signed in as the owner the ready seed gd-seed2's "run it?" decision asks. Usage:
   node pot_browser.js <base url> <node_modules dir>; POT_SHOTS=<dir> also writes PNGs.

   On a phone the plan's bead view opens with the pot's card first (In the Nursery: its state and the decision's answer box). A desktop's hover on the pot opens its tip: the plan's title, "Waiting on your answer", the question and the
   decision's options, the card clear of the pot. Tab from the pot goes into the card and Escape comes back. Run the plan
   answers on its second press through the Mailbox's own POST (/b/gd-seed2ask/decision/resolve, option_id=approve), the card
   says so (or has closed already), and once the plan is building (the harness's resolve stands in for the steward) the pot has left the Nursery:
   the tip closes, the farm document lists the plan as a plot, and the plot is planted in the very slot it was staked in. The
   ready seed is staked out (homestead-world-design.md section 3), so its card rides the staked plot, not a pot.
   Reading the plan from the plot (26.14): the tip lists its two steps under the card, each a link to its bead view; the bead
   view's Staked out section is followed by Steps, each step's body cut to 280 runes until a click opens it, then the plan's
   narrative, and the steps are not listed again as children; a tablet's first tap on the plot opens the tip, its second goes
   in (the cursor leaves the plot for its bed). */
'use strict';
var url = process.argv[2], mods = process.argv[3], shots = process.env.POT_SHOTS || '', assert = require('assert'), path = require('path');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));
var POT = '#hots a.w-staked.cur', TIP = '#stip';   // the ready seed is staked out on a plot, and wears the pot's card there

async function arrive(page) {
  await page.goto(url + '/garden?at=gd-seed2');
  await page.waitForSelector(POT, { state: 'attached' });
}
async function shoot(page, name) { if (shots) await page.screenshot({ path: path.join(shots, name + '.png'), animations: 'disabled', caret: 'hide' }); }
/* A point on sel's link that a tap reaches (no label or bed over it), as pointer_browser.js finds one. */
function bare(page, sel) {
  return page.evaluate(function (sel) {
    var a = document.querySelector(sel), r = a.getBoundingClientRect();
    for (var i = 1; i < 10; i++) for (var j = 1; j < 10; j++) { var x = r.left + r.width * i / 10, y = r.top + r.height * j / 10, h = document.elementFromPoint(x, y); if (h && h.closest('.hot') === a) return { x: x, y: y }; }
    return null;
  }, sel);
}
function texts(page, sel) { return page.$$eval(sel, function (ns) { return ns.map(function (n) { return n.textContent.trim(); }); }); }

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  try {
    var errors = [], init = function () { try { localStorage.setItem('garden.sky', 'day'); } catch (e) { /* the scheme decides */ } };

    // A phone: the plan's bead view, its Staked out section first with the same answer box, then its steps.
    var phone = await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true, reducedMotion: 'reduce' });
    await phone.addInitScript(init);
    var pp = await phone.newPage();
    pp.on('pageerror', function (e) { errors.push(String(e)); });
    await pp.goto(url + '/garden?at=gd-seed2&bead=gd-seed2');   // a tap on a staked plot goes in, as on any plot; its plan's bead view carries the card
    await pp.waitForFunction(function () { var b = document.getElementById('bead'); return b && !b.hidden && b.querySelector('.pot-card .act'); });
    await shoot(pp, 'pot-phone-bead');
    assert.deepStrictEqual((await texts(pp, '#bead .bv-live > .bv-sec > h3')).slice(0, 2), ['Staked out', 'Steps (2)'], 'the card leads the bead view, its steps next');
    assert.strictEqual((await pp.textContent('#bead .pot-card .pst')).trim(), 'Waiting on your answer');
    assert.deepStrictEqual(await texts(pp, '#bead .pot-card .acts button'), ['Run the plan', 'Change it first', 'Drop it', 'Other…']);
    await pp.waitForSelector('#bead .pot-steps .pot-body[role=button]');   // the bodies ride the bead document
    assert.deepStrictEqual(await texts(pp, '#bead .pot-steps .bl'), ['Package the gardener', 'Reach the cluster through the edge']);
    assert.deepStrictEqual(await pp.$$eval('#bead .pot-steps .bl', function (as) { return as.map(function (a) { return a.getAttribute('href'); }); }), ['/garden?bead=gd-seed2s1', '/garden?bead=gd-seed2s2']);
    var bodies = await texts(pp, '#bead .pot-steps .pot-body');
    assert.strictEqual(bodies[0], 'A brew formula.', 'a short body whole');
    assert.ok(/…$/.test(bodies[1]) && Array.from(bodies[1]).length <= 281 && Array.from(bodies[1]).length > 270, 'a long body cut at 280 runes (its trailing space dropped): ' + bodies[1]);
    // the narrative is the plan's description: its Description section says it, and the Steps section does not again
    assert.ok((await texts(pp, '#bead .bv-sec .bv-text')).some(function (t) { return t.trim() === 'Run the gardener next to the owner, on their laptop.'; }), 'then the narrative');
    assert.strictEqual(await pp.locator('#bead .pot-sum').count(), 0, 'said once');
    assert.ok(!(await texts(pp, '#bead .bv-sec h3')).some(function (t) { return /^Children/.test(t); }), 'the steps are not listed again as children');
    await shoot(pp, 'pot-phone-steps');
    await pp.tap('#bead .pot-body[aria-expanded=false]');
    assert.ok(/The end\.$/.test((await texts(pp, '#bead .pot-steps .pot-body'))[1]), 'a tap opens the whole body');
    assert.strictEqual(await pp.getAttribute('#bead .pot-body[role=button]', 'aria-expanded'), 'true');
    await phone.close();

    // A tablet: the first tap on the cursor's staked plot opens its tip, pinned; the second closes it and goes in.
    var tab = await browser.newContext({ viewport: { width: 820, height: 1180 }, isMobile: true, hasTouch: true, reducedMotion: 'reduce' });
    await tab.addInitScript(init);
    var tp = await tab.newPage();
    tp.on('pageerror', function (e) { errors.push(String(e)); });
    await arrive(tp);
    var at = await bare(tp, POT);   // a bare spot of the plot: a tap there opens the plan, as on any plot
    assert.ok(at, 'some of the plot is bare to a finger');
    await tp.touchscreen.tap(at.x, at.y);
    await tp.waitForFunction(function () { var t = document.getElementById('stip'); return t && !t.hidden && t.querySelector('.pot-steps'); });
    assert.ok(await tp.evaluate(function () { return document.getElementById('bead').hidden && !!document.querySelector('#hots a.w-staked.cur'); }), 'the first tap only opens the tip');
    await shoot(tp, 'pot-tablet-tip');
    await tp.touchscreen.tap(at.x, at.y);
    await tp.waitForFunction(function () { return document.getElementById('stip').hidden && !document.querySelector('#hots a.w-staked.cur'); });
    await tab.close();
    await phone.close();

    // A desktop: hover opens the tip.
    var desk = await browser.newContext({ viewport: { width: 1280, height: 800 }, reducedMotion: 'reduce', colorScheme: 'light' });
    await desk.addInitScript(init);
    var page = await desk.newPage(), posts = [];
    page.on('pageerror', function (e) { errors.push(String(e)); });
    page.on('request', function (r) { if (r.method() === 'POST' && /\/decision\/resolve$/.test(r.url())) posts.push({ url: r.url(), body: r.postData() || '' }); });
    await arrive(page);
    await page.hover(POT, { force: true });
    await page.waitForFunction(function () { var t = document.getElementById('stip'); return t && !t.hidden && t.querySelector('.pot-card .act'); });
    await shoot(page, 'pot-desktop-tip');
    assert.strictEqual((await page.textContent(TIP + ' .pot-card > b')).trim(), 'Local agents');
    assert.strictEqual((await page.textContent(TIP + ' .pst')).trim(), 'Waiting on your answer');
    assert.strictEqual((await page.textContent(TIP + ' .act .ask')).trim(), 'Run it?', 'the question without the plan\'s id and title');
    assert.deepStrictEqual(await texts(page, TIP + ' .acts button'), ['Run the plan', 'Change it first', 'Drop it', 'Other…']);
    assert.strictEqual(await page.getAttribute(TIP, 'role'), 'group');
    assert.deepStrictEqual(await texts(page, TIP + ' .pot-steps li'), ['1Package the gardener', '2Reach the cluster through the edge'], 'the steps under the card, numbered as the stakes');
    assert.strictEqual(await page.getAttribute(TIP + ' .pot-steps li:nth-child(2) a', 'href'), '/garden?bead=gd-seed2s2', 'a step opens its bead view');
    var clear = await page.evaluate(function (sel) {
      var a = document.querySelector('#stip').getBoundingClientRect(), b = document.querySelector(sel).getBoundingClientRect();
      return a.right <= b.left || b.right <= a.left || a.bottom <= b.top || b.bottom <= a.top;
    }, POT);
    assert.ok(clear, 'the card never covers its pot');

    // Keys: Tab from the pot goes into the card, Escape comes back to the pot.
    await page.focus(POT);
    await page.keyboard.press('Tab');
    assert.strictEqual(await page.evaluate(function () { return document.activeElement.textContent.trim(); }), 'Run the plan', 'Tab goes into the card');
    await page.keyboard.press('Escape');
    assert.ok(await page.evaluate(function () { return document.activeElement.classList.contains('w-staked') && document.getElementById('stip').hidden; }), 'Escape closes the card, back on the pot');

    // Answer: the first press arms, the second posts approve through the Mailbox's endpoint.
    var PLOT = '#hots a.w-plot[href$="bead=gd-seed2"]', where = function () { return page.$eval(PLOT, function (a) { return [a.style.left, a.style.top, a.style.width]; }); };
    var staked = await where();
    await page.mouse.move(640, 790);   // off the pot, so the next hover enters it again
    await page.hover(POT, { force: true });
    await page.waitForFunction(function () { var t = document.getElementById('stip'); return t && !t.hidden; });
    var run = page.locator(TIP + ' .acts button', { hasText: 'Run the plan' });
    await run.click();
    assert.strictEqual(posts.length, 0, 'one press does not answer');
    await page.locator(TIP + ' .acts button.armed').click();
    await page.mouse.move(640, 790);   // away again: the armed button said the option's whole label and grew, so a mouse left on it can rest on the next pot
    // the card says it was answered, or has already closed because the plan left the Nursery (the stub's farm is quick)
    await page.waitForFunction(function () { var t = document.getElementById('stip'), n = t.querySelector('.pot-card .note'); return t.hidden || (n && /^Answered/.test(n.textContent)); });
    await shoot(page, 'pot-desktop-answered');
    assert.strictEqual(posts.length, 1, 'one POST');
    assert.ok(/\/b\/gd-seed2ask\/decision\/resolve$/.test(posts[0].url), 'the Mailbox\'s endpoint: ' + posts[0].url);
    assert.strictEqual(posts[0].body, 'option_id=approve');

    // The plan is building: the pot leaves the Nursery and the card closes.
    await page.waitForFunction(async function () {
      var d = await (await fetch('/garden/farm', { headers: { Accept: 'application/json' } })).json();
      return !(d.seeds || []).some(function (s) { return s.id === 'gd-seed2'; }) && (d.plans || []).some(function (p) { return p.id === 'gd-seed2'; });
    }, null, { polling: 250, timeout: 15000 });
    await page.waitForFunction(function () { return document.getElementById('stip').hidden; }, null, { timeout: 15000 });
    await page.waitForFunction(function (sel) { var a = document.querySelector(sel); return a && !a.classList.contains('w-staked'); }, PLOT, { timeout: 15000 });
    assert.deepStrictEqual(await where(), staked, 'approving plants the plan in its staked slot: the plot does not move');
    assert.deepStrictEqual(errors, [], 'no page errors');
    console.log('ok: staked plot card and steps on phone, tablet and desktop, approve posted, the plan planted in place');
  } finally {
    await browser.close();
  }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
