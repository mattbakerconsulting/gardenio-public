/* Browser check that a cut text opens in place and never navigates (gd-Fmq3fgqFTm), run from
   TestCutsOpenInPlaceUnderChromium against the card world.
   Usage: node cuts_browser.js <base url> <node_modules dir> <wish id> <steward path>. GARDENHUB_SHOTS=<dir> saves a PNG of
   each view shut and opened.

   On the stress steward's document a #general line past the transcript's cut, in the panel a squawk's face past the bead
   view's fold, and on the wish's node that squawk's Conversation line: each opens whole and shuts again from its button
   (aria-expanded, by mouse and by key), with the address and every scroll as they were; no "Read it all" is a link; each
   view has one Expand all that opens every cut (and Markdown fold) in it and is then Collapse all. No console errors. */
'use strict';
var url = process.argv[2], mods = process.argv[3], wish = process.argv[4], sw = process.argv[5], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] })), shots = process.env.GARDENHUB_SHOTS || '';

/* the page's address, every scroll above sel's button, and its cut's state */
function look(page, sel) {
  return page.evaluate(function (sel) {
    var go = document.querySelector(sel), cut = go.closest('.cm-cut'), s = [window.scrollY];
    var head = cut.querySelector(':scope > .cm-cut-head'), whole = cut.querySelector(':scope > .cm-cut-whole');
    for (var e = go.parentElement; e; e = e.parentElement) s.push(e.scrollTop);
    return { href: location.href, scroll: s, expanded: go.getAttribute('aria-expanded'), word: go.textContent, tag: go.tagName,
      head: head.getClientRects().length ? head.textContent : '', whole: whole && whole.getClientRects().length ? whole.textContent : '' };
  }, sel);
}

/* the address once it has held for half a second: the farm writes its camera into it (?at=) as it lands, by itself */
async function settled(page) {
  for (var was = ''; was !== page.url();) { was = page.url(); await page.waitForTimeout(500); }
}

/* sel's cut opened and shut by mouse and by key, in place; the whole it opens */
async function opens(page, sel, what) {
  await settled(page);
  await page.evaluate(function (sel) { document.querySelector(sel).scrollIntoView({ block: 'center' }); }, sel);
  var shut = await look(page, sel);
  assert.strictEqual(shut.tag, 'BUTTON', what + ': its control is a button, not a link');
  assert.strictEqual(shut.expanded, 'false', what + ': it starts shut');
  assert.ok(shut.head && !shut.whole, what + ': its cut part shows');
  await page.click(sel);
  var open = await look(page, sel);
  assert.strictEqual(open.href, shut.href, what + ': the address stays');
  assert.deepStrictEqual(open.scroll, shut.scroll, what + ': the scroll stays');
  assert.deepStrictEqual([open.expanded, open.word, open.head], ['true', 'Show less', ''], what + ': it opens');
  assert.ok(open.whole.length > shut.head.length && open.whole.indexOf(shut.head.replace(/…$/, '')) === 0 && !/…$/.test(open.whole), what + ': whole: ' + open.whole.slice(-60));
  await page.click(sel);
  var again = await look(page, sel);
  assert.deepStrictEqual([again.expanded, again.word, again.head, again.whole, again.href], ['false', shut.word, shut.head, '', shut.href], what + ': it shuts again');
  await page.focus(sel);
  await page.keyboard.press('Enter');
  assert.strictEqual((await look(page, sel)).expanded, 'true', what + ': Enter opens it');
  await page.keyboard.press('Space');
  assert.strictEqual((await look(page, sel)).expanded, 'false', what + ': Space shuts it');
  return open.whole;
}

/* the view scope's one Expand all (in host) opens every cut in it, then shuts them all as Collapse all */
async function expandsAll(page, scope, host, what) {
  var at = function () {
    return page.evaluate(function (a) {
      var s = document.querySelector(a[0]), b = s.querySelectorAll('.cm-cutall'), gos = [].map.call(s.querySelectorAll('.cm-cut-go'), function (g) { return g.getAttribute('aria-expanded'); });
      [].forEach.call(s.querySelectorAll('details.md-more'), function (d) { gos.push(d.open ? 'true' : 'false'); });   // a Markdown fold counts as a cut
      return { n: b.length, inHost: !!s.querySelector(a[1] + ' > .cm-cutall'), word: b[0] && b[0].textContent, expanded: b[0] && b[0].getAttribute('aria-expanded'), gos: gos, href: location.href };
    }, [scope, host]);
  };
  var was = await at();
  assert.deepStrictEqual([was.n, was.inHost, was.word, was.expanded], [1, true, 'Expand all', 'false'], what + ': one Expand all, in its head');
  assert.ok(was.gos.length > 0 && was.gos.every(function (g) { return g === 'false'; }), what + ': its cuts start shut: ' + was.gos);
  await page.click(scope + ' .cm-cutall');
  var all = await at();
  assert.ok(all.gos.every(function (g) { return g === 'true'; }), what + ': Expand all opens every cut: ' + all.gos);
  assert.deepStrictEqual([all.word, all.expanded, all.href], ['Collapse all', 'true', was.href], what + ': it is Collapse all, the address kept');
  if (shots) await page.screenshot({ path: shots + '/cuts-' + what.replace(/\W+/g, '-') + '-open.png' });
  await page.click(scope + ' .cm-cutall');
  var none = await at();
  assert.ok(none.gos.every(function (g) { return g === 'false'; }), what + ': Collapse all shuts every cut: ' + none.gos);
  assert.strictEqual(none.word, 'Expand all', what + ': and is Expand all again');
  return was.gos.length;
}

/* no "Read it all" on the page is a link away */
async function noLinkAway(page, what) {
  var away = await page.evaluate(function () { return [].filter.call(document.querySelectorAll('a'), function (a) { return /read it all|show all/i.test(a.textContent); }).map(function (a) { return a.href; }); });
  assert.deepStrictEqual(away, [], what + ': a link away to read the rest');
}

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); } catch (e) { console.log(String(e).split('\n')[0]); process.exit(3); }
  try {
    var page = await browser.newPage({ viewport: { width: 1440, height: 900 } }), errors = [];
    // the fixture keeps no notes doc for the stress land, so its Holds' read (GET /garden/notes/<land>) is a 404 there
    page.on('console', function (m) { if (m.type() === 'error' && !/\/garden\/notes\//.test((m.location() || {}).url || '')) errors.push(m.text()); });
    page.on('pageerror', function (e) { errors.push(String(e)); });

    // the stress steward's #general line past its 240-rune cut
    await page.goto(url + sw);
    var line = '#wb2 .wb2-doc .wb2-swtalk .cm-cut-go';
    await page.waitForSelector(line, { state: 'visible', timeout: 15000 });
    if (shots) await page.screenshot({ path: shots + '/cuts-steward-shut.png' });
    await noLinkAway(page, 'steward');
    await opens(page, line, 'steward line');
    var n = await expandsAll(page, '#wb2 .wb2-doc', '#wb2 .wb2-doc > .wb2-head > .cm-sw-today', 'steward');

    // a squawk past the bead view's fold, in the wish's thread
    var long = 'long squawk ' + Date.now() + ': ' + new Array(80).join('the crew wrote more ');
    var sq = await page.evaluate(function (a) {
      return fetch('/test/squawk?thread=' + encodeURIComponent(a.id) + '&text=' + encodeURIComponent(a.words), { method: 'POST' }).then(function (r) { if (!r.ok) throw new Error('squawk: ' + r.status); return r.text(); });
    }, { id: wish, words: long });

    // its face in the panel
    await page.goto(url + '/garden?bead=' + encodeURIComponent(sq));
    var face = '#bv-body .bv-face .cm-cut-go';
    await page.waitForSelector(face, { state: 'visible', timeout: 15000 });
    if (shots) await page.screenshot({ path: shots + '/cuts-panel-shut.png' });
    await noLinkAway(page, 'panel');
    assert.ok((await opens(page, face, 'squawk face')).indexOf(long.trim()) >= 0, 'the face opens to the whole squawk');
    await expandsAll(page, '#bv-body', '#bv-body > .bv-meta', 'panel');

    // its line in the wish's Conversation, on the wish's node
    await page.goto(url + '/garden/c/n/' + encodeURIComponent(wish));
    var conv = '#wb2 .wb2-bead .bv-sec[data-sec=conversation] .cm-cut-go';
    await page.waitForSelector(conv, { state: 'visible', timeout: 15000 });
    await noLinkAway(page, 'node');
    assert.ok((await opens(page, conv, 'conversation line')).indexOf(long.trim()) >= 0, 'the line opens to the whole squawk');
    await expandsAll(page, '#wb2 .wb2-bead', '#wb2 .wb2-bead > .bv-meta', 'node');

    assert.deepStrictEqual(errors, [], 'console errors');
    console.log('cuts: the steward line (' + n + ' cuts on its document), ' + sq + "'s face and its Conversation line open in place");
  } finally {
    await browser.close();
  }
})().catch(function (e) { console.error(e); process.exit(1); });
