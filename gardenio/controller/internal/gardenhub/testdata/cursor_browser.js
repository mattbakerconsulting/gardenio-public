/* The cursor in a real browser (gardenio/docs/superpowers/specs/2026-10-20-lands-cursor-grid-design.md section 10.4, build step
   3), run from TestCursorUnderChromium against a live /garden on an httptest server with the goldens' world.
   Usage: node cursor_browser.js <base url> <node_modules dir>.

   Desktop 1280x800, reduced motion: ?at= names the cursor's Thing and z= is ignored; #hots is a role=grid of rows and gridcells
   holding the current grid's links, with exactly one in the tab order (the cursor's, ringed by .cur) and each named with its row
   and column; Tab into the map lands on the cursor and Tab again leaves it; the arrows move focus and the cursor; Enter goes
   into a plan's phases and a phase's steps, announcing once, and Escape comes back out by the same way; Shift and an arrow
   jumps to the next plan at the same depth; the address names the cursor without z=, and a reload restores it; Home is the
   project's home cell and a second Home the Commons's (homestead-world-design.md 28.6), 0 is depth 0; an old ?at=yard link
   lands on the Commons's home with a toast, its address rewritten to ~/home (28.7). Zero console errors.
   The signs (spec 6, build step 5): no plate for the cursor's Thing at any depth (gd-zDdiA3HBh5): the title bar names it, its
   lineage as links, its whole title and its status line, and follows the cursor; child labels stay within their budget; the
   flicker probe: a MutationObserver on #lbls records zero mutations while the mouse sweeps 50 points across the board; a click on
   the bar's title opens the Thing in the bead view. GARDENHUB_SHOTS=<dir> saves a screenshot at each depth. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));
var A = '#hots a.w-plot[href="/garden?bead=gd-planA"]';

/* What the cursor is: the grid's name, its one tab stop, where focus is, and the address. */
function state(page) {
  return page.evaluate(function () {
    var hots = document.getElementById('hots'), stops = hots.querySelectorAll('.hot[tabindex="0"]'), cur = hots.querySelectorAll('.hot.cur'), a = document.activeElement;
    var q = new URLSearchParams(location.search);
    return { grid: hots.getAttribute('aria-label'), role: hots.getAttribute('role'), stops: stops.length, cur: cur.length, name: cur[0] ? cur[0].getAttribute('aria-label') : '',
      kind: cur[0] ? cur[0].className : '', focused: !!(a && cur[0] && a === cur[0]), inGrid: hots.contains(a), at: q.get('at'), z: q.get('z'),
      say: document.getElementById('cursay').textContent, level: WORLD.LEVEL_NAMES[window.GARDEN.cam().level] + ' · ' + Math.round(window.GARDEN.cam().z * 100) / 100 };
  });
}
/* The page has laid the farm out and opened its cursor. Until the farm document lands the grid is empty, so a .cur on its own
   is not that; the title bar names the cursor's Thing only once the cursor is open (gd-bNZrDyCr1m). */
function opened(page) { return page.waitForSelector('#cursor-bar .cb-me', { state: 'attached' }); }
/* A key, then a timer task in the page: a cursor move refreshes its signs in a setTimeout(0) task (gd-fsuxCGGbZr), and timers of
   equal delay run in the order they were set, so the state read after sees the move's signs. */
async function press(page, key) { await page.keyboard.press(key); await page.waitForTimeout(60); await page.evaluate(function () { return new Promise(function (r) { setTimeout(r, 0); }); }); return state(page); }
/* The signs on screen: the plates in #lbls other than child labels and the World's header plates (none: the cursor's Thing has
   no label), the cursor's cell box, the child labels and the window. */
function signs(page) {
  return page.evaluate(function () {
    var r = function (e) { var b = e.getBoundingClientRect(); return { x: b.left, y: b.top, w: b.width, h: b.height }; };
    var cur = document.querySelector('#hots .hot.cur'), bd = document.getElementById('board').getBoundingClientRect();
    return { plates: Array.prototype.map.call(document.querySelectorAll('#lbls .sign:not(.child):not(.head)'), function (e) { return e.className + ': ' + e.textContent; }),
      cur: cur ? r(cur) : null, children: document.querySelectorAll('#lbls .sign.child').length, board: { x: bd.left, y: bd.top, w: bd.width, h: bd.height } };
  });
}
function overlaps(a, b) { return a.x < b.x + b.w - 1 && b.x < a.x + a.w - 1 && a.y < b.y + b.h - 1 && b.y < a.y + a.h - 1; }
function within(a, b) { return a.x >= b.x - 1 && a.y >= b.y - 1 && a.x + a.w <= b.x + b.w + 1 && a.y + a.h <= b.y + b.h + 1; }
/* The steady title bar (gd-yozHl8UqIs): where it is, what it reads and its links' hot ids. */
function bar(page) {
  return page.evaluate(function () {
    var b = document.getElementById('cursor-bar'), r = b.getBoundingClientRect();
    return { hidden: b.hidden, x: r.left, y: r.top, text: b.textContent, links: Array.prototype.map.call(b.querySelectorAll('a[data-hot]'), function (a) { return a.dataset.hot; }),
      ttl: (b.querySelector('.cb-me') || {}).textContent || '', st: (b.querySelector('.cb-sw') || {}).textContent || '',
      icons: Array.prototype.map.call(b.querySelectorAll('.cb-st .ic'), function (p) { var c = p.querySelector('canvas'), cb = c ? c.getBoundingClientRect() : null; return { k: p.className.replace(/^ic /, ''), w: p.textContent.trim(), px: cb ? [cb.width, cb.height] : null, hidden: c ? c.getAttribute('aria-hidden') : null }; }) };
  });
}
/* No plate for the cursor's Thing (gd-zDdiA3HBh5): the title bar names it whole (its accessible name starts with the bar's
   title) with a status line; child labels within the 35 a project's plots may have. */
async function oneSign(page, what) {
  var g = await signs(page), s = await state(page), b = await bar(page);
  assert.deepStrictEqual(g.plates, [], what + ': no plate beside the cursor\'s Thing: ' + JSON.stringify(g));
  assert.ok(!b.hidden && b.ttl.length >= 2 && s.name.indexOf(b.ttl.slice(0, Math.min(b.ttl.length, 12))) === 0, what + ': the title bar names the cursor\'s Thing: ' + JSON.stringify([b.ttl, s.name]));
  assert.ok(b.st.length > 0, what + ': the title bar carries its status line: ' + JSON.stringify(b));
  assert.ok(g.children <= 35, what + ': child labels within budget: ' + g.children);
  if (process.env.GARDENHUB_SHOTS) await page.screenshot({ path: require('path').join(process.env.GARDENHUB_SHOTS, 'cursor-' + what.replace(/\W+/g, '-') + '.png') });
  return b;
}

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  try {
    var ctx = await browser.newContext({ viewport: { width: 1280, height: 800 }, reducedMotion: 'reduce' });
    await ctx.addInitScript(function () { try { localStorage.setItem('garden.sky', 'day'); } catch (e) { /* the scheme decides */ } });
    var page = await ctx.newPage(), errors = [];
    page.on('console', function (m) { if (m.type() === 'error') errors.push(m.text()); });
    page.on('pageerror', function (e) { errors.push(String(e)); });

    // 1. ?at= puts the cursor on plan A, depth 1; z= is ignored (spec 3.5); the grid and its one tab stop (spec 3.6).
    await page.goto(url + '/garden?at=gardenio/gd-planA&z=3');
    await page.waitForSelector(A + '.cur', { state: 'attached' });
    var s = await state(page);
    assert.strictEqual(s.role, 'grid');
    assert.ok(/^gardenio: \d+ plans?, \d+ by \d+$/.test(s.grid), 'the grid is named for the project: ' + s.grid);
    assert.ok(!/^Bead/.test(s.level), 'z=3 does not drive the camera: ' + s.level);
    assert.deepStrictEqual([s.stops, s.cur], [1, 1], 'exactly one tab stop, the cursor');
    assert.ok(/^Edge IdP rollout: .*(, row|\. Row) \d+ of \d+, column \d+ of \d+\.?$/.test(s.name), 'the cursor\'s name ends with its place: ' + s.name);
    assert.ok(!/\.,/.test(s.name), 'the cursor\'s name reads as sentences, never ".," (gd-jhE8MpXvJm): ' + s.name);
    var shape = await page.evaluate(function () {
      var rows = Array.prototype.slice.call(document.querySelectorAll('#hots > [role=row]'));
      return { rows: rows.length, cells: rows.reduce(function (n, r) { return n + r.querySelectorAll(':scope > [role=gridcell]').length; }, 0), hots: document.querySelectorAll('#hots .hot').length,
        indexed: rows.every(function (r) { return +r.getAttribute('aria-rowindex') >= 1 && Array.prototype.every.call(r.children, function (c) { return +c.getAttribute('aria-colindex') >= 1; }); }) };
    });
    assert.ok(shape.rows >= 1 && shape.cells === shape.hots && shape.indexed, 'rows of gridcells, each holding one link, with their indices: ' + JSON.stringify(shape));
    var f = await oneSign(page, 'plan A');
    assert.ok(f.ttl === 'Edge IdP rollout' && /^1 needs you · phase \d+ of \d+/.test(f.st), 'the bar reads plan A whole, the need words first: ' + JSON.stringify(f));

    // 1b. The flicker probe (spec 10.4): the mouse sweeps 50 points across the board and #lbls does not change at all.
    await page.evaluate(function () {
      window.__lblMut = 0;
      new MutationObserver(function (l) { window.__lblMut += l.length; }).observe(document.getElementById('lbls'), { subtree: true, childList: true, attributes: true, characterData: true });
    });
    var bd = (await signs(page)).board;
    for (var i = 0; i < 50; i++) await page.mouse.move(bd.x + 8 + (bd.w - 16) * ((i * 37) % 50) / 49, bd.y + 8 + (bd.h - 16) * ((i * 13) % 50) / 49);
    await page.waitForTimeout(150);
    assert.strictEqual(await page.evaluate(function () { return window.__lblMut; }), 0, 'the mouse sweep changed no sign');

    // 1c. On the Meadow no plate stands over gd-M3 (gd-UAKclcq5hu: gd-M2's label covered it, so a click on gd-M3 opened gd-M2).
    await page.goto(url + '/garden?at=gardenio/gd-M2');
    await page.waitForSelector('#hots a.hot.cur[href="/garden?bead=gd-M2"]', { state: 'attached' });
    await oneSign(page, 'meadow flower');
    await page.goto(url + '/garden?at=gardenio/gd-planA');
    await page.waitForSelector(A + '.cur', { state: 'attached' });

    // 2. Tab into the map lands on the cursor; Tab again leaves it.
    await page.focus('#main');   // the map is the first tab stop in it (3b R9 took the drawer buttons off the map)
    s = await press(page, 'Tab');
    assert.ok(s.focused, 'Tab into the map lands on the cursor: ' + JSON.stringify(s));
    s = await press(page, 'Tab');
    assert.ok(!s.inGrid, 'Tab again leaves the grid');
    await page.focus(A);

    // 3. The arrows move the cursor and focus with it, skipping empty slots; at an edge the cursor stays.
    var start = (await state(page)).name, seen = {};
    for (var k of ['ArrowRight', 'ArrowDown', 'ArrowLeft', 'ArrowUp']) { s = await press(page, k); assert.ok(s.focused && s.stops === 1, k + ': focus is the cursor: ' + JSON.stringify(s)); seen[s.name] = 1; }
    assert.ok(Object.keys(seen).length >= 2 || Object.keys(seen)[0] === start, 'the arrows walked the plots: ' + JSON.stringify(Object.keys(seen)));
    await oneSign(page, 'after the arrows');
    await page.focus(A);
    s = await state(page);
    assert.ok(/^Edge IdP rollout/.test(s.name), 'focus on a link moves the cursor to it: ' + s.name);

    // 3b. The steady title bar (gd-yozHl8UqIs) reads the plan on plan A; it keeps its place at every depth (4, 5).
    var b1 = await bar(page);
    assert.ok(!b1.hidden && b1.links.length === 1 && b1.links[0] === 'gd-planA' && /^Edge IdP rollout/.test(b1.text), 'the title bar reads plan A: ' + JSON.stringify(b1));

    // 4. Enter goes into plan A's phases, and Enter again into a phase's steps; each descend is announced once.
    s = await press(page, 'Enter');
    assert.ok(/^Edge IdP rollout: \d+ phases?, \d+ by \d+$/.test(s.grid) && /w-bed/.test(s.kind) && s.focused, 'into the plan: ' + JSON.stringify(s));
    assert.ok(/^Inside Edge IdP rollout: \d+ by \d+ phases, on phase \d+, /.test(s.say), 'the descend is announced: ' + s.say);
    var phaseGrid = s.grid;
    f = await oneSign(page, 'a phase');
    assert.ok(f.links.length === 2 && f.links[0] === 'gd-planA', 'the plan is the phase\'s lineage in the bar: ' + JSON.stringify(f));
    s = await press(page, 'Enter');
    assert.ok(/ \d+ steps?, \d+ by \d+$/.test(s.grid) && /w-task/.test(s.kind) && s.focused, 'into a phase: ' + JSON.stringify(s));
    await oneSign(page, 'a step');
    var b3 = await bar(page);
    assert.ok(b3.links.length === 3 && b3.links[0] === 'gd-planA' && b3.links[1] === 'gd-A1' && /^Edge IdP rollout›Foundations›/.test(b3.text), 'on a step the bar reads Plan › Phase › Task: ' + JSON.stringify(b3));
    assert.ok(b3.x === b1.x && b3.y === b1.y, 'the bar never moves: ' + JSON.stringify([b1, b3]));

    // 5. Escape comes back out by the same way.
    s = await press(page, 'Escape');
    assert.strictEqual(s.grid, phaseGrid, 'Escape: back to the phases');
    s = await press(page, 'Escape');
    assert.ok(/^gardenio: /.test(s.grid) && /^Edge IdP rollout/.test(s.name) && s.focused, 'and to plan A: ' + JSON.stringify(s));
    var b5 = await bar(page);
    assert.ok(b5.x === b1.x && b5.y === b1.y && b5.links.length === 1, 'the bar is where it was, reading plan A again: ' + JSON.stringify(b5));

    // 5b. A click on the bar's title opens the cursor's Thing in the bead view (spec 7: Start).
    await page.click('#cursor-bar .cb-me');
    await page.waitForFunction(function () { var b = document.getElementById('bead'); return b && !b.hidden && /Edge IdP rollout/.test(document.getElementById('bv-title').textContent); });
    await page.keyboard.press('Escape');
    await page.waitForFunction(function () { return document.getElementById('bead').hidden; });
    await page.focus(A);

    // 5c. With the bead view open (Space), the arrows still move the cursor and the view follows it in place; Escape closes
    //     the view and the cursor stays where it got to, with focus back on its link (gd-IUV4yofidl).
    await page.keyboard.press('Space');
    await page.waitForFunction(function () { var b = document.getElementById('bead'); return b && !b.hidden && /Edge IdP rollout/.test(document.getElementById('bv-title').textContent); });
    var moved = null, entries = await page.evaluate(function () { return history.length; });
    for (var dir of ['ArrowRight', 'ArrowDown', 'ArrowLeft', 'ArrowUp']) { s = await press(page, dir); if (!/^Edge IdP rollout/.test(s.name)) { moved = dir; break; } }
    assert.ok(moved, 'an arrow moves the cursor while the bead view is open: ' + JSON.stringify(s));
    var follow = await page.evaluate(function () {
      var c = document.querySelector('#hots .hot.cur'), b = document.getElementById('bead');
      return { open: !b.hidden, cell: c ? new URL(c.href).searchParams.get('bead') : '', view: new URLSearchParams(location.search).get('bead'), title: document.getElementById('bv-title').textContent, inView: b.contains(document.activeElement), entries: history.length };
    });
    assert.ok(follow.open && follow.cell && follow.cell === follow.view && follow.inView, 'the bead view follows the cursor onto ' + s.name + ': ' + JSON.stringify(follow));
    assert.ok(s.name.indexOf(follow.title) === 0, 'and names it: ' + JSON.stringify([s.name, follow.title]));
    assert.strictEqual(follow.entries, entries, 'following adds no history entry');
    var movedName = s.name;
    s = await press(page, 'Escape');
    var closed = await page.evaluate(function () { return { open: !document.getElementById('bead').hidden, bead: new URLSearchParams(location.search).get('bead') }; });
    assert.ok(!closed.open && !closed.bead, 'Escape closes the bead view: ' + JSON.stringify(closed));
    assert.ok(s.name === movedName && s.focused, 'the cursor stays where it got to, focus on its link: ' + JSON.stringify([movedName, s]));
    await page.focus(A);

    // 6. Shift and an arrow, inside plan A's phases, jumps to another plan's phases at the same depth (spec 3.3).
    await press(page, 'Enter');
    var jumped = null;
    for (var dir of ['ArrowRight', 'ArrowLeft', 'ArrowDown', 'ArrowUp']) {
      s = await press(page, 'Shift+' + dir);
      if (s.grid !== phaseGrid && / phases?, /.test(s.grid)) { jumped = dir; break; }
    }
    assert.ok(jumped, 'a sibling jump reaches another plan\'s phases: ' + JSON.stringify(s));
    console.log('desktop: Shift+' + jumped + ' jumped to ' + s.grid);

    // 7. The address names the cursor's Thing, without z=; a reload restores the cursor. The address follows 400 ms after the
    //    last move, so wait for it to name the Thing the saved path (written on every move) ends on: an at= that is merely not
    //    plan A's can be the one step 5c's move left, and the reload then opens there (gd-QGz1CE6hyO).
    await page.waitForFunction(function () {
      var p = JSON.parse(localStorage.getItem('garden.cursor.v1') || '{}').path || [], at = new URLSearchParams(location.search).get('at') || '';
      return p.length > 0 && at.slice(at.indexOf('/') + 1) === p[p.length - 1];   // any project's: step 6 can jump out of gardenio
    });
    s = await state(page);
    assert.strictEqual(s.z, null, 'z= is no longer written');
    var before = s;
    await page.reload();
    await opened(page);
    s = await state(page);
    assert.deepStrictEqual([s.grid, s.name], [before.grid, before.name], 'the reload restores the cursor');
    await page.goto(url + '/garden');
    await opened(page);
    s = await state(page);
    assert.deepStrictEqual([s.grid, s.name], [before.grid, before.name], 'a bare /garden opens on the saved cursor (garden.cursor.v1)');

    // 8. Home is the cursor's project's home cell, from any depth; a second Home the Commons's (28.6, H-D11); 0 is depth 0.
    //    From plan A's phases: step 6's jump can leave gardenio (plan A is east of the farmstead, gd-8cz6uje2WZ).
    await page.goto(url + '/garden?at=gardenio/gd-A1');
    await opened(page);
    s = await press(page, 'Home');
    assert.ok(/^Home: /.test(s.name) && /^gardenio: /.test(s.grid), 'Home: gardenio\'s home cell: ' + JSON.stringify(s));
    await oneSign(page, 'a home cell');
    s = await press(page, 'Enter');
    assert.ok(/^Home: [1-5] places?, [12] by [1-4]$/.test(s.grid), 'Enter: its zones, laid out as the courtyard (29.6): ' + s.grid);
    //    The Escalations and the Mailbox share the north-east quarter, the Mailbox outside (29.2): Up and Right reach the
    //    Mailbox, Left from it the Escalations, and its approved escalation (plan B's) is a cell in them, its link the bead.
    for (var i = 0; i < 6 && !/^Mailbox/.test(s.name); i++) s = await press(page, i % 2 ? 'ArrowRight' : 'ArrowUp');
    assert.ok(/^Mailbox/.test(s.name), 'the Mailbox is the north-east zone: ' + JSON.stringify(s));
    s = await press(page, 'ArrowLeft');
    assert.ok(/^Escalations: /.test(s.name) && /1 escalation/.test(s.name), 'Left from the Mailbox: the Escalations: ' + JSON.stringify(s));
    s = await press(page, 'Enter');
    assert.ok(/^Escalations, 1 running$/.test(s.grid) && /w-escalation/.test(s.kind), 'Enter: its escalation, the grid named for its state (gd-AmIpCiVTvY): ' + JSON.stringify(s));
    assert.strictEqual(await page.evaluate(function () { var c = document.querySelector('#hots .hot.cur'); return c && c.getAttribute('href'); }), '/garden?bead=gd-planB1e', 'its link is the bead');
    s = await press(page, 'Home');
    assert.ok(/^Home: /.test(s.name) && /^gardenio: /.test(s.grid), 'Home from a zone: its home cell again: ' + JSON.stringify(s));
    s = await press(page, 'Home');
    assert.ok(/^Home: /.test(s.name) && !/^gardenio: /.test(s.grid), 'a second Home: the Commons\'s: ' + JSON.stringify(s));
    s = await press(page, '0');
    assert.ok(/^The farm: /.test(s.grid), '0: depth 0: ' + JSON.stringify(s));

    // 8b. A legacy ?at=yard link (28.7, H-D12) lands on the Commons's home cell with a toast naming where it went, and the
    //     address is rewritten at once to the Commons's home (~/home): no link copied from the page says yard.
    await page.goto(url + '/garden?at=yard');
    await opened(page);
    s = await state(page);
    assert.ok(/^Home: /.test(s.name) && !/^gardenio: /.test(s.grid), '?at=yard: the Commons\'s home cell: ' + JSON.stringify(s));
    assert.strictEqual(s.at, '~/home', 'the address says the Commons\'s home, not yard');
    var moved = await page.evaluate(function () { var t = document.getElementById('toast'); return t && !t.hidden ? t.textContent : ''; });
    assert.ok(/^The Yard is gone: every project has its own home now\. Showing the .+ home\.$/.test(moved), 'the toast says where it went: ' + moved);

    // 9. A closed step is a cursor stop in its bed with its own title (gd-0Fj6E2vPql): ?at= opens on it, its label says it is
    //    closed, the arrows walk between it and the open steps, and Space opens its bead view.
    await page.goto(url + '/garden?at=gardenio/gd-A1x');
    await page.waitForSelector('#hots a.hot.cur[href="/garden?bead=gd-A1x"]', { state: 'attached' });
    s = await state(page);
    assert.ok(/w-task/.test(s.kind) && /^Title gd-A1x/.test(s.name) && / steps?, /.test(s.grid), 'the cursor opens on the closed step: ' + JSON.stringify(s));
    //    Its link says the harvest cell's base and every badge in words (harvest spec 4.3: state is never colour alone).
    var al = await page.getAttribute('#hots a.hot.cur[href="/garden?bead=gd-A1x"]', 'aria-label');
    assert.ok(/harvested · graded 4\/5 gates · 1 test · checks green · merged after 2 rounds/.test(al), 'the harvest cell says its badges: ' + al);
    f = await oneSign(page, 'a closed step');
    //    The title bar's status line (harvest spec 5): the lead words, then every badge as a 12 px icon and its word in priority
    //    order, none dropped (gd-zDdiA3HBh5).
    assert.ok(/^harvested · \d+[smhd] ago$/.test(f.st), 'the closed step\'s status says it was harvested, and when: ' + JSON.stringify(f));
    assert.deepStrictEqual(f.icons.map(function (p) { return [p.k, p.w]; }), [['rosette', 'graded 4/5 gates'], ['vial', '1 test'], ['lantern', 'checks green'], ['seal', 'merged after 2 rounds']], 'every badge in priority order: ' + JSON.stringify(f.icons));
    assert.ok(f.icons.every(function (p) { return p.px && p.px[0] === 12 && p.px[1] === 12 && p.hidden === 'true'; }), 'each icon is a 12 px canvas, hidden from the reader (its word is its name): ' + JSON.stringify(f.icons));
    var names = {};
    for (var dir of ['ArrowLeft', 'ArrowUp', 'ArrowRight', 'ArrowDown']) { s = await press(page, dir); names[s.name] = 1; }
    assert.ok(Object.keys(names).length >= 2 && / steps?, /.test(s.grid), 'the arrows walk the bed from the closed step: ' + JSON.stringify(Object.keys(names)));
    await page.goto(url + '/garden?at=gardenio/gd-A1x');
    await page.waitForSelector('#hots a.hot.cur[href="/garden?bead=gd-A1x"]', { state: 'attached' });
    await page.keyboard.press('Space');
    await page.waitForFunction(function () { var b = document.getElementById('bead'); return b && !b.hidden && /Title gd-A1x/.test(document.getElementById('bv-title').textContent); });
    await page.keyboard.press('Escape');
    await page.waitForFunction(function () { return document.getElementById('bead').hidden; });

    // The dock beside the map takes no key from it (gd-DT9pEP0YaG): on a desktop the Porch or the Mailbox open (#porch, #mail,
    // which a reload keeps) leaves the screen flag off the farm, and the map's keys still move the cursor, from the page and
    // from the map itself; a key pressed in the dock stays the dock's. Each pane is a fresh load: from the porch round's
    // address, a goto that changes only the hash is a same-document navigation, which keeps focus on the porch control the
    // round ended on until Chromium's focus fixup notices its pane went hidden, so the arrow "from the page" raced it.
    for (var pane of ['porch', 'mail']) {
      await page.goto('about:blank');
      await page.goto(url + '/garden?at=gardenio/gd-A1#' + pane);
      await page.waitForFunction(function () { var c = document.querySelector('#hots .hot.cur'); return !!c && /^Foundations/.test(c.getAttribute('aria-label') || ''); });
      var dock = await page.evaluate(function () { var d = document.getElementById('dw-dock'), l = document.querySelector('.left'); return { screen: document.body.dataset.screen, open: !!d && !d.hidden, map: !!l && getComputedStyle(l).display !== 'none', page: document.activeElement === document.body }; });
      assert.deepStrictEqual(dock, { screen: pane, open: true, map: true, page: true }, 'the ' + pane + ' dock is open beside the map, focus on the page: ' + JSON.stringify(dock));
      s = await press(page, 'ArrowRight');
      assert.ok(/^Rollout/.test(s.name), 'with the ' + pane + ' open, an arrow from the page moves the cursor: ' + JSON.stringify(s));
      await page.focus('#board');
      s = await press(page, 'ArrowLeft');
      assert.ok(/^Foundations/.test(s.name), 'with the ' + pane + ' open, an arrow from the map moves the cursor: ' + JSON.stringify(s));
      s = await press(page, 'Enter');
      assert.ok(/ steps?, /.test(s.grid), 'with the ' + pane + ' open, Enter goes into the phase: ' + JSON.stringify(s));
      s = await press(page, 'Escape');
      assert.ok(/ phases?, /.test(s.grid) && /^Foundations/.test(s.name), 'with the ' + pane + ' open, Escape comes back out: ' + JSON.stringify(s));
      assert.strictEqual(await page.evaluate(function () { return document.body.dataset.screen; }), pane, 'the keys leave the dock as it was');
      var inDock = await page.evaluate(function () { var el = document.querySelector('#pane-' + document.body.dataset.screen + ' button, #pane-' + document.body.dataset.screen + ' a'); if (el) el.focus(); return !!el && document.activeElement === el; });
      assert.ok(inDock, 'the ' + pane + ' pane has a control that takes focus');
      s = await press(page, 'ArrowRight');
      assert.ok(/^Foundations/.test(s.name), 'a key pressed in the ' + pane + ' dock stays the dock\'s: ' + JSON.stringify(s));
    }

    assert.deepStrictEqual(errors.filter(function (e) { return !/404/.test(e); }), [], 'console errors');
    console.log('cursor browser checks passed');
    await ctx.close();
  } finally { await browser.close(); }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
