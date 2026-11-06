/* Browser check for the Mailbox's way to the map (gd-kNV6Eos4ON), run from TestMailMapUnderChromium against a live /garden on
   an httptest server with the goldens' world. Usage: node mailmap_browser.js <base url> <node_modules dir> [shots dir].

   An owner asked about a plan from Slack could not find it on /garden: nothing they already use led to its plot. The
   Mailbox's decision gd-A2f is plan A's (its waiting list carries it), and gd-planRask is the staked releases plan's own card, so each row says "Part of <plan A>" and has an
   "Open on the map" link to ?at=gardenio/gd-planA. With the cursor on plan P, desktop (1440x900, reduced motion): a click puts the cursor on plan A's
   plot, focus on it and the address naming it, the same place the ?at= link itself lands; a modified click is left to the
   browser. Phone (390x844, touch): the Mailbox screen gives way to the farm, the cursor on the plot. A row no plan carries
   has no such line. With a shots dir, each arrival is shot there for the UX grade. */
'use strict';
var url = process.argv[2], mods = process.argv[3], shots = process.argv[4] || '', assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));
function skyCtx(browser, opts) {
  opts = opts || {};
  return browser.newContext(opts).then(function (ctx) {
    return ctx.addInitScript(function (s) { try { localStorage.setItem('garden.sky', s); } catch (e) { /* no storage: the emulated scheme still decides by day */ } }, opts.colorScheme === 'dark' ? 'night' : 'day').then(function () { return ctx; });
  });
}
var START = '/garden?at=gardenio/gd-planP', ROW = '#d-mail > li[data-src="i-gd-A2f"]', LINK = ROW + ' .onplan a.to-map';
var RROW = '#d-mail > li[data-src="i-gd-planRask"]', RLINK = RROW + ' .onplan a.to-map';
async function shoot(page, name) { if (shots) await page.screenshot({ path: require('path').join(shots, name + '.png') }); }
/* Where the cursor is: the address it wrote (?at=), the focused element's name, the screen. */
function where(page) {
  return page.evaluate(function () {
    var a = document.activeElement;
    return { at: new URLSearchParams(location.search).get('at'), screen: document.body.dataset.screen, focus: a ? a.getAttribute('aria-label') || a.textContent : '' };
  });
}
/* The plot is picked out on arrival: the cursor's ring (#hots .hot.cur) is the plot's own link, drawn with a box on the board. */
async function pickedOut(page, id) {
  var c = await page.evaluate(function () {
    var e = document.querySelector('#hots .hot.cur'), b = e && e.getBoundingClientRect(), o = e && getComputedStyle(e);
    return e ? { href: e.getAttribute('href'), w: b.width, h: b.height, outline: o.outlineStyle + ' ' + o.boxShadow } : null;
  });
  assert.ok(c, 'a cursor is drawn');
  assert.strictEqual(c.href, '/garden?bead=' + id, 'the cursor is on the plot');
  assert.ok(c.w > 40 && c.h > 40, 'the cursor ring has a box: ' + JSON.stringify(c));
}
async function open(ctx, path) {
  var page = await ctx.newPage(), errors = [];
  page.on('console', function (m) { if (m.type() === 'error') errors.push(m.text()); });
  page.on('pageerror', function (e) { errors.push(String(e)); });
  await page.goto(url + path);
  await page.waitForSelector('#hots a.w-plot[href="/garden?bead=gd-planP"]', { state: 'attached' });   // the cursor on plan P, away from plan A
  return { page: page, errors: errors };
}
async function row(page, ROW, LINK, at) {
  try { await page.waitForSelector(LINK, { state: 'visible', timeout: 5000 }); } catch (e) {
    throw new Error('no plan line on ' + ROW + ': ' + JSON.stringify(await page.evaluate(function () {
      return Array.prototype.map.call(document.querySelectorAll('#d-mail > li'), function (li) { return [li.dataset.src, li.className, li.innerHTML.slice(0, 200)]; });
    })));
  }
  var r = await page.evaluate(function (s) {
    var li = document.querySelector(s), p = li.querySelector('.onplan'), a = p.querySelector('a.to-map');
    return { title: a.getAttribute('aria-label').replace(/^Open (.+) on the map$/, '$1'), short: p.querySelector('b').textContent, text: p.textContent, href: a.getAttribute('href'), label: a.getAttribute('aria-label'), before: p.nextElementSibling && p.nextElementSibling.className,
      others: Array.prototype.filter.call(document.querySelectorAll('#d-mail > li'), function (o) { return o !== li; }).map(function (o) { return [o.dataset.src, !!o.querySelector('.onplan')]; }) };
  }, ROW);
  assert.ok(/^Part of .+Open on the map$/.test(r.text), 'the row names its plan: ' + r.text);
  assert.strictEqual(r.href, '/garden?at=' + at, 'the link is the plan plot deep link');
  assert.ok(/^Open .+ on the map$/.test(r.label), r.label);
  assert.strictEqual(r.before, 'act', 'the line sits above the answer box');
  assert.ok(r.short.length <= r.title.length, 'the line names the plan by its short title, the link in full: ' + r.short);
  return r;
}

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); } catch (e) { console.error(String(e)); process.exit(3); }
  try {
    // desktop: the Mailbox pane, then a click on "Open on the map"
    var dctx = await skyCtx(browser, { viewport: { width: 1440, height: 900 }, reducedMotion: 'reduce' });
    var d = await open(dctx, START), page = d.page;
    await page.click('#dbtn-mail');
    var r = await row(page, ROW, LINK, 'gardenio/gd-planA');
    var rr = await row(page, RROW, RLINK, 'gardenio/gd-planR');
    assert.strictEqual(rr.short, 'release record, truck and release page', 'the staked plan by its short title');   // a staked plan's own card: its plot, before it is approved
    await page.waitForFunction(function () { return document.querySelectorAll('#d-mail > li').length > 1; });
    assert.ok(r.others.some(function (o) { return o[0] === 'i-d-1' && !o[1]; }), 'd-1, which no plan carries, has no plan line: ' + JSON.stringify(r.others));
    await shoot(page, 'mailmap-desktop-row');
    await page.click(LINK);
    await page.waitForFunction(function () { return new URLSearchParams(location.search).get('at') === 'gardenio/gd-planA'; });
    var w = await where(page);
    assert.strictEqual(w.screen, 'farm');
    assert.strictEqual(w.focus.indexOf(r.title + ':'), 0, 'focus is on the plan plot, as the row named it: ' + w.focus);
    await pickedOut(page, 'gd-planA');
    console.log('desktop: landed at ' + w.at + ', focus on ' + w.focus);
    await shoot(page, 'mailmap-desktop-landed');
    await page.click(RLINK);
    await page.waitForFunction(function () { return new URLSearchParams(location.search).get('at') === 'gardenio/gd-planR'; });
    var ws = await where(page);
    assert.ok(/^Releases of a project.*: Staked out, /.test(ws.focus), 'focus is on the staked plot: ' + ws.focus);
    await pickedOut(page, 'gd-planR');
    console.log('desktop: the staked plan landed at ' + ws.at + ', focus on ' + ws.focus);
    await shoot(page, 'mailmap-desktop-staked');
    // the ?at= link itself lands in the same place
    var direct = await open(dctx, START);
    await direct.page.goto(url + '/garden?at=gardenio/gd-planA');
    await direct.page.waitForSelector('#hots a.w-plot[href="/garden?bead=gd-planA"]', { state: 'attached' });
    await pickedOut(direct.page, 'gd-planA');   // the ?at= link itself picks out the plot (the Slack link, gd-kNV6Eos4ON c)
    await shoot(direct.page, 'mailmap-desktop-direct');
    assert.deepStrictEqual(d.errors.concat(direct.errors), [], 'no console errors');

    // phone: the Mailbox screen, a tap, the farm
    var pctx = await skyCtx(browser, Object.assign({}, pw.devices['iPhone 12'], { viewport: { width: 390, height: 844 }, reducedMotion: 'reduce', colorScheme: 'dark' }));
    var p = await open(pctx, START);
    await p.page.tap('#tabbar [data-go="mail"]');
    var pr = await row(p.page, ROW, LINK, 'gardenio/gd-planA');
    await p.page.locator(LINK).scrollIntoViewIfNeeded();
    await shoot(p.page, 'mailmap-phone-row');
    await p.page.tap(LINK);
    await p.page.waitForFunction(function () { return new URLSearchParams(location.search).get('at') === 'gardenio/gd-planA'; });
    var pw2 = await where(p.page);
    assert.strictEqual(pw2.screen, 'farm', 'the Mailbox screen gives way to the farm');
    assert.strictEqual(pw2.focus.indexOf(pr.title + ':'), 0, 'focus is on the plan plot: ' + pw2.focus);
    await pickedOut(p.page, 'gd-planA');
    console.log('phone: landed at ' + pw2.at + ', focus on ' + pw2.focus);
    await shoot(p.page, 'mailmap-phone-landed');
    assert.deepStrictEqual(p.errors, [], 'no console errors');
    console.log('mailmap: ok');
  } finally { await browser.close(); }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
