/* Browser check for meadow adoption (gd-6HQyB0jgmO), run from TestAdoptUnderChromium against a live /garden on an httptest
   server with the fixture farm, signed in as an operator who may adopt. Usage: node adopt_browser.js <base url> <node_modules
   dir>; ADOPT_SHOTS=<dir> also writes the panel's PNGs.

   The bead view of a loose meadow bead (gd-M2) offers "Adopt into a plan"; a plan, a bead under a non-plan epic (gd-M3)
   and a phase step do not. One press asks a gardener (gd-BXbYNWyOcF): with the Commons answering (a routed 201), the panel
   says so, links the thread to follow and withdraws the buttons; the mention posted names the bead. Without a Commons (the
   bare harness: 404) the picker opens by itself, under a note saying why. The picker lists the bead's project's open plans
   and their open phases (not a closed phase, not another project's plan), and is driven by keys alone: Tab to the button,
   Enter, arrow to a phase, Tab to Adopt, Enter. The panel then says where the bead went and lists the phase under "Part of",
   and the buttons are gone. A deep link opens on its bead's grid even when the farm document beats the bead view's bundle.
   Desktop 1280x800. */
'use strict';
var url = process.argv[2], mods = process.argv[3], shots = process.env.ADOPT_SHOTS || '', assert = require('assert'), path = require('path');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));

async function openBead(page, id) {
  await page.goto(url + '/garden?bead=' + id);
  try { await page.waitForFunction(function () { return document.querySelectorAll('#hots a').length > 0; }); }   // the farm document is in, the cursor flown to the bead
  catch (e) { throw new Error(id + ': no link in #hots (' + await page.evaluate(function () { return document.getElementById('hots').getAttribute('aria-label'); }) + '): ' + e.message); }
  await page.locator('#bead').waitFor({ state: 'visible' });
  await page.waitForFunction(function () { var b = document.getElementById('bv-body'); return b.textContent.trim() && !/Loading/.test(b.textContent); });
}
async function shoot(page, name) {
  if (!shots) return;
  var r = await page.locator('#bead').boundingBox();
  await page.screenshot({ path: path.join(shots, name + '.png'), clip: r, animations: 'disabled', caret: 'hide' });
}
var BTN = '#bead .ad .ad-ask', OWN = '#bead .ad .ad-own', SAY = '**/garden/c/gardenio/general/say';

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  try {
    var ctx = await browser.newContext({ viewport: { width: 1280, height: 800 }, reducedMotion: 'reduce', colorScheme: 'light' });
    await ctx.addInitScript(function () { try { localStorage.setItem('garden.sky', 'day'); } catch (e) { /* the scheme decides */ } });
    var page = await ctx.newPage(), errors = [];
    page.on('pageerror', function (e) { errors.push(String(e)); });

    // The farm document lands before the bundle that flies to a deep-linked bead has run (gd-rM1LY2052v), and that lazy
    // bundle runs only once the opened cursor has written its own ?at= into the address, which the bundle once took for the
    // link's (gd-p02ZelpMxk): the cursor still opens on the bead's grid, not on the World's buttons.
    var held = '';
    await page.route(/page-workbench/, async function (route) {
      var r = await route.fetch(), t = Date.now();
      while (!/[?&]at=/.test(page.url()) && Date.now() - t < 5000) await new Promise(function (ok) { setTimeout(ok, 50); });
      held = page.url();
      await route.fulfill({ response: r });
    });
    await openBead(page, 'gd-M3');
    assert.ok(/[?&]at=/.test(held), 'the Workbench ran after the opened cursor wrote its ?at=: ' + held);
    await page.unroute(/page-workbench/);

    // Not offered: a plan, a bead under a non-plan epic, a step already in a phase.
    for (var id of ['gd-planA', 'gd-M3', 'gd-A2a']) {
      await openBead(page, id);
      assert.strictEqual(await page.locator(BTN).count(), 0, id + ' must not offer adoption');
    }

    // A gardener is asked: the Commons answers the mention with the message's fragment and the bridge's note.
    var posted = '';
    await page.route(SAY, function (route) {
      posted = route.request().postData() || '';
      route.fulfill({ status: 201, headers: { 'X-Commons-Note': encodeURIComponent('Starting a gardener in #general.') }, contentType: 'text/html; charset=utf-8', body: '<li class="cm-msg" id="m-gd-msg1" data-id="gd-msg1" data-channel="general"></li>' });
    });
    await openBead(page, 'gd-M2');
    await shoot(page, 'adopt-1-offered');
    var btn = page.locator(BTN);
    assert.strictEqual(await btn.count(), 1, 'the loose bead offers adoption');
    assert.strictEqual((await btn.textContent()).trim(), 'Adopt into a plan');
    assert.strictEqual((await page.locator(OWN).textContent()).trim(), 'Pick the plan yourself…', 'the picker is the second choice');
    await btn.focus();
    await page.keyboard.press('Enter');
    await page.waitForFunction(function () { var n = document.querySelector('#bead .ad [role=status]'); return n && /^Asked a gardener/.test(n.textContent); });
    assert.ok(/^text=%40gardenio\+adopt\+gd-M2\+into\+a\+plan\./.test(posted), 'the mention names the bead and asks for a plan: ' + posted.slice(0, 80));
    assert.ok(/kd\+dep\+add\+gd-M2/.test(posted), 'and says which edge to write');
    var askedText = await page.evaluate(function () { return document.querySelector('#bead .ad [role=status]').textContent; });
    assert.strictEqual(askedText, 'Asked a gardener to adopt gd-M2 into a plan. Starting a gardener in #general. Follow it in the Commons.', 'the panel says so, with the bridge\'s word');
    assert.ok(/\/garden\/c\/n\/gd-msg1$/.test(await page.locator('#bead .ad .ad-follow').getAttribute('href')), 'and links the node of the thread the mention roots');
    assert.strictEqual(await page.locator(BTN).count() + await page.locator(OWN).count(), 0, 'one press is one gardener: the buttons withdraw');
    await shoot(page, 'adopt-1b-asked');
    await page.unroute(SAY);

    // No Commons in the land (a 404 from the say route; the harness's own fake Commons would answer 201): the picker opens by
    // itself under a note saying why.
    await page.route(SAY, function (route) { route.fulfill({ status: 404, contentType: 'text/plain', body: '404 page not found' }); });
    await openBead(page, 'gd-M2');
    btn = page.locator(BTN);
    await btn.focus();
    await page.keyboard.press('Enter');
    var sel = page.locator('#ad-sel');
    await sel.waitFor({ state: 'visible' });
    assert.ok(await page.evaluate(function () { return document.activeElement && document.activeElement.id === 'ad-sel'; }), 'the picker takes focus');
    assert.strictEqual(await page.evaluate(function () { return document.querySelector('#bead .ad [role=status]').textContent; }), 'This land has no Commons, so no gardener can be asked.', 'and says why it opened');
    var opts = await page.evaluate(function () {
      return Array.from(document.querySelectorAll('#ad-sel optgroup')).map(function (g) { return g.label + ': ' + Array.from(g.children).map(function (o) { return o.textContent; }).join(' | '); });
    });
    var edge = opts.filter(function (l) { return l.indexOf('Edge IdP rollout: ') === 0; })[0] || '';
    assert.strictEqual(edge, 'Edge IdP rollout: No phase | Foundations | Rollout', 'plan A and its open phases, not the closed Cutover: ' + JSON.stringify(opts));
    assert.ok(!opts.some(function (l) { return /The (kbeads|coop|music) plan/.test(l); }), 'no other project\'s plans: ' + JSON.stringify(opts));
    assert.ok(opts.some(function (l) { return /^Draft plans: .*Local agents \(draft\)/.test(l); }), 'draft plans are offered: ' + JSON.stringify(opts));
    await sel.selectOption({ label: 'Rollout' });
    assert.strictEqual(await page.locator('#bead .ad-into').textContent(), 'Into Edge IdP rollout \u203a Rollout.', 'the closed list still names the plan');
    await shoot(page, 'adopt-2-picker');

    await page.keyboard.press('Tab');
    assert.strictEqual(await page.evaluate(function () { return document.activeElement.textContent; }), 'Adopt', 'Tab from the list reaches Adopt');
    await page.keyboard.press('Enter');
    await page.waitForFunction(function () { var n = document.querySelector('#bead .ad [role=status]'); return n && /^Adopted|err/.test(n.textContent + n.className); });
    var said = await page.evaluate(function () { return document.querySelector('#bead .ad [role=status]').textContent; });
    assert.strictEqual(said, 'Adopted into Edge IdP rollout \u203a Rollout.', 'the panel says where the bead went');
    await page.waitForFunction(function () { return Array.from(document.querySelectorAll('#bead .bv-sec h3')).some(function (h) { return /^Part of/.test(h.textContent); }); });
    assert.ok(await page.evaluate(function () { return !!document.querySelector('#bead .bv-rels a[href$="bead=gd-A2"]'); }), 'Part of lists the phase');
    assert.strictEqual(await page.locator(BTN).count() + await page.locator(OWN).count(), 0, 'an adopted bead is no longer offered adoption');
    await shoot(page, 'adopt-3-done');

    assert.deepStrictEqual(errors, [], 'page errors');
    console.log('adopt: offered on gd-M2 only; a gardener asked (mention posted, thread linked); without a Commons the picker ' + JSON.stringify(opts) + '; adopted into gd-A2 by keys');
  } finally {
    await browser.close();
  }
})().catch(function (e) { console.error(e); process.exit(1); });
