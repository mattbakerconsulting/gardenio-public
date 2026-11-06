/* Browser check for the Escalations cell (plan gd-PJB3D7glwF step 6 gd-4FIF5pzC6O, homestead-world-design.md 28.11 and 28.13),
   run from TestEscalationYardUnderChromium against a live /garden on an httptest server with the card world plus one open
   escalation of each family in gardenio (pending, approved, rejected and failed among them) and 90 merges in a project zoo,
   signed in as an owner who may approve. Usage: node escyard_browser.js <base url> <node_modules dir> [shots dir].

   Each escalation is its own cell, in ESCKIND's order (in hand first, then rejected and failed), and its link's accessible name
   is ESCKIND.name's: the family's noun, the title, the state word, since when, and the plan or project it came from. Hover
   and focus show its sign tip in the same words; a pending one adds the Mailbox's Approve, Reject and Comment box, an
   approved one none. Enter, Space and a click open its bead view. Under reduced motion an approved escalation's gear does
   not turn. Past its widest cell's 88 slots (29.10, 29.11), zoo's cell shows 87 and a "+3" crate that opens the Mail pane on them
   all. The Home sign says "10 escalations", and the World level's board counts them. Approve, pressed twice in the tip, posts to the viewer's
   /b/{id}/escalation/approve; the harness closes the bead and the next farm document takes the cell away. */
'use strict';
var url = process.argv[2], mods = process.argv[3], shots = process.argv[4] || '', assert = require('assert'), path = require('path');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));
var TIP = '#stip', CELL = function (id) { return '#hots a.w-escalation[href$="bead=' + id + '"]'; };
var FAMILY = { 'gd-Emerge': 'Merge escalation', 'gd-Edeploy': 'Deploy escalation', 'gd-Econfig': 'Config edit escalation',
  'gd-Eprompt': 'Prompt install escalation', 'gd-Ebeads': 'Beads admin escalation', 'gd-Eaccess': 'Access escalation',
  'gd-Einfra': 'Infra escalation', 'gd-Eread': 'Read-only query escalation', 'gd-Eother': 'Escalation', 'gd-planB1e': 'Escalation' };
var STATE = { 'gd-Emerge': 'waiting on the merge desk', 'gd-Edeploy': 'approved, running', 'gd-Econfig': 'waiting on', 'gd-Eprompt': 'rejected, still open',
  'gd-Ebeads': 'failed, still open', 'gd-Eaccess': 'waiting on', 'gd-Einfra': 'approved, running', 'gd-Eread': 'waiting on', 'gd-Eother': 'rejected, still open',
  'gd-planB1e': 'approved, running' };
var LAST = ['gd-Ebeads', 'gd-Eother', 'gd-Eprompt'];   // rejected and failed: the cell's third group

async function shoot(page, name) { if (shots) await page.screenshot({ path: path.join(shots, 'escyard-' + name + '.png'), animations: 'disabled', caret: 'hide' }); }
function cells(page) {
  return page.$$eval('#hots a.w-escalation', function (as) {
    return as.map(function (a) { var m = /Row (\d+) of \d+, column (\d+) of/.exec(a.getAttribute('aria-label') || ''); return { id: a.getAttribute('href').replace(/^.*bead=/, ''), name: a.getAttribute('aria-label'), r: m ? +m[1] : 0, c: m ? +m[2] : 0 }; })
      .sort(function (a, b) { return a.r - b.r || a.c - b.c; });
  });
}
/* The cursor's escalation (its bead id) after a key's move has settled, and whether its link holds focus. */
async function curOf(page) {
  await page.waitForTimeout(60);
  return page.evaluate(function () { var c = document.querySelector('#hots .hot.cur'); return { id: c && (c.getAttribute('href') || '').replace(/^.*bead=/, ''), focused: !!c && document.activeElement === c }; });
}
function beadOpen(page, id) { return page.waitForFunction(function (id) { var b = document.getElementById('bead'); return b && !b.hidden && typeof BEAD !== 'undefined' && BEAD.current() === id; }, id, { timeout: 10000 }); }
async function closeBead(page) { await page.keyboard.press('Escape'); await page.waitForFunction(function () { return document.getElementById('bead').hidden; }); }
function tipUp(page) { return page.waitForFunction(function () { var t = document.getElementById('stip'); return t && !t.hidden; }); }

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  try {
    var errors = [], posts = [], init = function () { try { localStorage.setItem('garden.sky', 'day'); } catch (e) { /* the scheme decides */ } };
    var desk = await browser.newContext({ viewport: { width: 1280, height: 800 }, reducedMotion: 'reduce', colorScheme: 'light' });
    await desk.addInitScript(init);
    var page = await desk.newPage();
    page.on('pageerror', function (e) { errors.push(String(e)); });
    page.on('request', function (r) { if (r.method() === 'POST' && /\/escalation\//.test(r.url())) posts.push({ url: r.url(), body: r.postData() || '' }); });
    await page.goto(url + '/garden?at=gardenio/esc%3Agd-Eaccess');
    await page.waitForSelector(CELL('gd-Eaccess') + '.cur', { state: 'attached', timeout: 15000 });
    await page.mouse.move(1270, 790);
    await shoot(page, 'cell');

    // One cell per escalation, in ESCKIND's order, each named in ESCKIND's words.
    var cs = await cells(page);
    assert.deepStrictEqual(cs.map(function (c) { return c.id; }).sort(), Object.keys(FAMILY).sort(), 'every open escalation of gardenio is a cell: ' + JSON.stringify(cs));
    assert.deepStrictEqual(cs.slice(-3).map(function (c) { return c.id; }).sort(), LAST, 'rejected and failed come last, in reading order');
    cs.forEach(function (c) {
      var lead = FAMILY[c.id] + ': ' + (c.id === 'gd-planB1e' ? 'Title gd-planB1e' : 'Escalation ');
      assert.ok(c.name.indexOf(lead) === 0, c.id + ': the noun, then the title: ' + c.name);
      assert.ok(c.name.indexOf(', ' + STATE[c.id]) > 0, c.id + ': its state word: ' + c.name);
      assert.ok(/, since \d+ (min|h|d), from /.test(c.name), c.id + ': since when, and where from: ' + c.name);
    });
    assert.ok(/from plan Second plan with a title long enough/.test(cs.filter(function (c) { return c.id === 'gd-planB1e'; })[0].name), 'a plan\'s escalation names its plan');
    assert.ok(/from project gardenio\./.test(cs.filter(function (c) { return c.id === 'gd-Emerge'; })[0].name), 'a loose one its project');

    // Step 8 (gd-AmIpCiVTvY): the keyboard roves the cells. The grid is named for its cells' states and has one tab stop;
    // Home and End reach its first and last cell, an arrow the next, Escape comes out to the zone.
    var hots = await page.evaluate(function () { var h = document.getElementById('hots'); return { role: h.getAttribute('role'), name: h.getAttribute('aria-label'), stops: h.querySelectorAll('.hot[tabindex="0"]').length }; });
    assert.deepStrictEqual([hots.role, hots.stops], ['grid', 1], 'a grid with one roving tab stop: ' + JSON.stringify(hots));
    var parts = /^Escalations, (.+)$/.exec(hots.name);
    assert.ok(parts && /\d+ waiting on (you|others)\b/.test(hots.name) && /\d+ running\b/.test(hots.name), "named for what waits and what runs: " + hots.name);
    assert.strictEqual(parts[1].split(', ').reduce(function (n, p) { return n + parseInt(p, 10); }, 0), cs.length, 'the name counts every cell: ' + hots.name);
    await page.focus(CELL(cs[1].id));   // past the first: gd-Eaccess, waiting on you, is it in gardenio's one-column cell (29.10)
    await page.keyboard.press('Home');
    assert.deepStrictEqual(await curOf(page), { id: cs[0].id, focused: true }, 'Home: the first cell, focused');
    await page.keyboard.press('End');
    assert.deepStrictEqual(await curOf(page), { id: cs[cs.length - 1].id, focused: true }, 'End: the last');
    await page.keyboard.press('ArrowUp');
    var lc = cs[cs.length - 1], up = cs.filter(function (c) { return c.r === lc.r - 1 && c.c === lc.c; })[0];
    assert.deepStrictEqual(await curOf(page), { id: up.id, focused: true }, 'Up: the cell above it');
    await page.keyboard.press('Escape');
    await page.waitForFunction(function () { var c = document.querySelector('#hots .hot.cur'); return c && /^Escalations: /.test(c.getAttribute('aria-label') || ''); });
    await page.goto(url + '/garden?at=gardenio/esc%3Agd-Eaccess');
    await page.waitForSelector(CELL('gd-Eaccess') + '.cur', { state: 'attached' });
    await page.mouse.move(1270, 790);

    // Hover: the sign tip, and on a pending escalation the Mailbox's answer box.
    await page.hover(CELL('gd-Eaccess'), { force: true });
    await page.waitForFunction(function () { var t = document.getElementById('stip'); return t && !t.hidden && t.querySelector('.act'); });
    await shoot(page, 'tip-pending');
    assert.strictEqual((await page.textContent(TIP + ' > b')).trim(), 'Escalation k8s_rbac');
    assert.ok(/^Access escalation, waiting on .+, since \d+ (min|h|d), from project gardenio\.$/.test((await page.textContent(TIP + ' .esc-st')).trim()), 'the tip says family, state, since and where from');
    assert.strictEqual(await page.getAttribute(TIP, 'role'), 'group', 'a card to work in');
    assert.deepStrictEqual(await page.$$eval(TIP + ' .acts button', function (bs) { return bs.map(function (b) { return b.textContent.trim(); }); }), ['Approve', 'Reject', 'Comment…']);
    assert.strictEqual(await page.getAttribute(TIP + ' a.pot-open', 'href'), '/garden?bead=gd-Eaccess');
    await page.mouse.move(1270, 790);
    await page.waitForFunction(function () { return document.getElementById('stip').hidden; });   // in a one-column cell the tip lay over the next cells
    await page.hover(CELL('gd-Edeploy'), { force: true });
    await page.waitForFunction(function () { var t = document.getElementById('stip'), b = t && t.querySelector('b'); return t && !t.hidden && b && b.textContent === 'Escalation helm_deploy'; });
    assert.strictEqual(await page.getAttribute(TIP, 'role'), 'tooltip', 'an approved escalation\'s tip is words only');
    assert.strictEqual(await page.locator(TIP + ' .act').count(), 0, 'no answer box past pending');
    await page.mouse.move(1270, 790);
    await page.waitForFunction(function () { return document.getElementById('stip').hidden; });

    // Focus shows it too; Enter, Space and a click open the bead view.
    await page.focus(CELL('gd-Eread'));
    await tipUp(page);
    assert.strictEqual((await page.textContent(TIP + ' > b')).trim(), 'Escalation cluster_read', 'focus shows the tip');
    // no plate of the cursor's stands beside the tip (gd-6zpr543nNt, gd-zDdiA3HBh5): the title bar says what it would have
    assert.strictEqual(await page.evaluate(function () { return document.querySelectorAll('#lbls .sign:not(.child):not(.head)').length; }), 0, 'no plate beside the cursor\'s cell');
    await page.evaluate(function () { document.activeElement.blur(); });
    await page.waitForFunction(function () { return document.getElementById('stip').hidden; });
    await page.focus(CELL('gd-Eread'));
    await tipUp(page);
    await page.keyboard.press('Enter');
    await beadOpen(page, 'gd-Eread');
    await closeBead(page);
    await page.focus(CELL('gd-Einfra'));
    await page.keyboard.press(' ');
    await beadOpen(page, 'gd-Einfra');
    await closeBead(page);
    await page.click(CELL('gd-Einfra'), { force: true });   // the cursor's cell: a click opens it
    await beadOpen(page, 'gd-Einfra');
    await closeBead(page);

    // Reduced motion: an approved escalation's gear stands still.
    await page.mouse.move(1270, 790);
    var box = await page.$eval(CELL('gd-Edeploy'), function (a) { var r = a.getBoundingClientRect(); return { x: r.left, y: r.top, width: r.width, height: r.height }; });
    var a1 = await page.screenshot({ clip: box }); await page.waitForTimeout(900); var a2 = await page.screenshot({ clip: box });
    assert.ok(a1.equals(a2), 'no gear turns under reduced motion');

    // Answer in place: the first press arms, the second posts; the next farm document takes the cell away.
    await page.hover(CELL('gd-Eaccess'), { force: true });
    await page.waitForFunction(function () { var t = document.getElementById('stip'); return t && !t.hidden && t.querySelector('.act'); });
    await page.locator(TIP + ' .acts button', { hasText: 'Approve' }).click();
    assert.strictEqual(posts.length, 0, 'one press does not answer');
    await page.locator(TIP + ' .acts button.armed').click();
    await page.waitForFunction(function () { var t = document.getElementById('stip'), n = t.querySelector('p.note[role=status]'); return t.hidden || (n && /^Answered: Approve\./.test(n.textContent)); });
    await shoot(page, 'answered');
    assert.strictEqual(posts.length, 1, 'one POST');
    assert.ok(/\/b\/gd-Eaccess\/escalation\/approve$/.test(posts[0].url), 'the Mailbox\'s endpoint: ' + posts[0].url);
    await page.waitForFunction(async function () {
      var d = await (await fetch('/garden/farm', { headers: { Accept: 'application/json' } })).json();
      return !((d.escalations || {}).open || []).some(function (w) { return w.id === 'gd-Eaccess'; });
    }, null, { polling: 250, timeout: 15000 });
    await page.waitForFunction(function (sel) { var t = document.getElementById('stip'); return !document.querySelector(sel) && (t.hidden || !/k8s_rbac/.test(t.textContent)); }, CELL('gd-Eaccess'), { timeout: 15000 });   // its tip closes; the focused cursor cell's may show
    await page.goto(url + '/garden?at=gardenio/esc%3Agd-Eread');
    await page.waitForSelector(CELL('gd-Eread') + '.cur', { state: 'attached' });
    assert.deepStrictEqual((await cells(page)).map(function (c) { return c.id; }).sort(), Object.keys(FAMILY).filter(function (id) { return id !== 'gd-Eaccess'; }).sort(), 'the answered escalation has left the cell');

    // The Home sign counts them, and the project's words at the World level.
    await page.goto(url + '/garden?at=gardenio/home');
    await page.waitForSelector('#hots a.w-home.cur,#hots .cur', { state: 'attached' });
    var home = await page.$eval('#hots .cur', function (a) { return a.getAttribute('aria-label'); });
    assert.ok(/\b9 escalations\b/.test(home), 'the Home sign: ' + home);
    await page.goto(url + '/garden?at=gardenio');
    await page.waitForFunction(function () { var c = document.querySelector('#hots .cur'); return c && /escalations/.test(c.getAttribute('aria-description') || ''); }, null, { timeout: 10000 });
    var board = await page.$eval('#hots .cur', function (a) { return a.getAttribute('aria-description'); });
    assert.ok(/ · 9 escalations\./.test(board), 'the project\'s words count them: ' + board);

    // Past the cell's slots at its widest: 87 cells and a "+3" crate, which opens the Mail pane on them all.
    await page.goto(url + '/garden?at=zoo/esc%3Agd-Ezoo29');
    await page.waitForSelector(CELL('gd-Ezoo29') + '.cur', { state: 'attached' });
    await page.mouse.move(1270, 790);
    await shoot(page, 'crate');
    assert.strictEqual((await cells(page)).length, 87, 'the widest cell\'s slots less the crate');
    var crate = '#hots a.w-crate';
    assert.ok(/^\+3 more escalations/.test(await page.getAttribute(crate, 'aria-label')), 'the crate counts the rest: ' + await page.getAttribute(crate, 'aria-label'));
    assert.strictEqual(await page.getAttribute(crate, 'href'), '/garden?queue=escalations');
    await page.focus(crate);
    await page.keyboard.press(' ');
    await page.waitForFunction(function () { var p = document.getElementById('pane-mail'); return p && p.dataset.only === 'escalation'; }, null, { timeout: 10000 });

    // The phone (gd-AmIpCiVTvY, gd-wAN7P4Armw): the camera column's In goes into the home cell, a tap on the zone in its
    // courtyard (29.6) moves the cursor there and In goes into it, Out comes back; the cursor escalation's tip is the bar's
    // text on the tab bar, and no docked row stands between the bar and the tab bar.
    var phone = await browser.newContext({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true, reducedMotion: 'reduce', colorScheme: 'light' });
    await phone.addInitScript(init);
    var pp = await phone.newPage();
    pp.on('pageerror', function (e) { errors.push(String(e)); });
    await pp.goto(url + '/garden?at=gardenio/home');
    await pp.waitForSelector('#hots .hot.cur', { state: 'attached', timeout: 15000 });
    var cam = function (k) { return pp.locator('#cam-' + k); }, curName = function () { return pp.$eval('#hots .hot.cur', function (c) { return c.getAttribute('aria-label') || ''; }); };
    await cam('in').tap(); await pp.waitForTimeout(300);
    await pp.locator('#hots .hot[aria-label^="Escalations: "]').first().tap(); await pp.waitForTimeout(80);
    assert.ok(/^Escalations: /.test(await curName()), 'a tap reaches the Escalations in the courtyard: ' + await curName());
    await cam('in').tap(); await pp.waitForTimeout(300);
    var on = await pp.$eval('#hots .hot.cur', function (c) { return (c.getAttribute('href') || '').replace(/^.*bead=/, ''); });
    assert.ok(FAMILY[on], 'In: an escalation: ' + on);
    var tipLine = await pp.$eval('#cursor-bar .cb-st.cb-tip', function (e) { return e.textContent; });
    assert.ok(new RegExp('^' + FAMILY[on] + ', .*since \\d+ (min|h|d), from (plan|project) ').test(tipLine), 'the bar on the tab bar reads its tip (its "· " is CSS): ' + tipLine);
    var geo = await pp.evaluate(function () { var b = document.getElementById('cursor-bar').getBoundingClientRect(), p = document.getElementById('tabbar').getBoundingClientRect(); return { gap: p.top - b.bottom, right: b.right, vw: innerWidth, sw: document.documentElement.scrollWidth }; });
    assert.ok(Math.abs(geo.gap) <= 1, 'the bar sits on the tab bar, nothing docked between: ' + JSON.stringify(geo));
    assert.ok(geo.sw <= geo.vw, 'no sideways scroll: ' + JSON.stringify(geo));
    await cam('out').tap(); await pp.waitForTimeout(300);
    assert.ok(/^Escalations: /.test(await curName()), 'Out: back to the zone: ' + await curName());
    await phone.close();

    assert.deepStrictEqual(errors, [], 'no page errors');
    console.log('ok: the keyboard roves the Escalations (Home, End, arrows, Escape) under its states\' name, a phone\'s tap and In reach them with the tip on the tab bar; one cell per escalation in ESCKIND\'s order and words, its tip and in-place answer, Enter/Space/click open it, no gear under reduced motion, the +3 crate, the Home and World counts, and an approve leaves the cell');
  } finally {
    await browser.close();
  }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
