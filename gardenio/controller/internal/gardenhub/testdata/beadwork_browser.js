/* Browser check for the bead panel's Work and Open sections (gd-efoAiLlzRC), run from TestBeadWorkUnderChromium against
   /garden on an httptest server with cardWorld, whose Commons daemon there holds every bead of the bead view, so each has a
   node (GET /garden/c/n/{id}). GET /test/beadwork/kids names each bead's children, {id: [child ids]}.
   Usage: node beadwork_browser.js <base url> <node_modules dir>. GARDENHUB_SHOTS=<dir> saves PNGs of two Work sections.

   Desktop: for every bead with children, its panel draws the node's tree as its Work section, and each child is listed
   once: one visible row among the Work tree and the panel's own relation lists, or (a plan's phases, a staked plot's steps,
   a bundle's documents) none there and a link elsewhere in the panel. Every row of the tree opens that bead's panel, never
   the Commons node, and the panel has no Open in workbench button. A tree row clicked opens its bead here. No console
   errors, and no node read refused. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] })), shots = process.env.GARDENHUB_SHOTS || '';

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); } catch (e) { console.log(String(e).split('\n')[0]); process.exit(3); }
  try {
    var page = await browser.newPage({ viewport: { width: 1440, height: 900 } }), errors = [];
    page.on('console', function (m) { if (m.type() === 'error' && !/^Failed to load resource/.test(m.text())) errors.push(m.text()); });   // a refused read is the response line below
    page.on('pageerror', function (e) { errors.push(String(e)); });
    page.on('response', function (r) { var p = new URL(r.url()).pathname; if (r.status() >= 400 && p.indexOf('/garden/c/n/') === 0) errors.push(r.status() + ' ' + p); });   // a node the panel asks for
    var kids = await (await page.request.get(url + '/test/beadwork/kids')).json(), ids = Object.keys(kids).sort();
    assert.ok(ids.length >= 20, 'the fixture farm has beads with children: ' + ids.length);
    var leaf = [].concat.apply([], ids.map(function (i) { return kids[i]; })).filter(function (c) { return !kids[c]; })[0];   // opened first, so each bead below is read afresh
    await page.goto(url + '/garden?bead=' + encodeURIComponent(leaf));
    await page.locator('#bead').waitFor({ state: 'visible' });
    var seen = 0, trees = 0;
    for (var id of ids) {
      // the panel opens it as a link in the garden does, and reads its node; the work slot drawn before is marked, so the
      // wait below takes the new one
      var node = page.waitForResponse(function (r) { return new URL(r.url()).pathname === '/garden/c/n/' + id; }, { timeout: 10000 });
      await page.evaluate(function (id) {
        var w = document.querySelector('#bv-body .bv-work');
        if (w) w.dataset.old = '1';
        document.dispatchEvent(new CustomEvent('garden:open-bead', { detail: { id: id }, cancelable: true }));
      }, id);
      assert.strictEqual((await node).status(), 200, id + ': its node');
      await page.waitForFunction(function () { var w = document.querySelector('#bv-body .bv-work:not([data-old])'); return w && w.dataset.drawn; }, null, { timeout: 10000 }).catch(function () {
        throw new Error(id + ': its node was never drawn: ' + JSON.stringify(kids[id]));
      });
      var got = await page.evaluate(function (a) {
        var b = document.getElementById('bv-body'), vis = function (e) { return !!e.offsetParent; };
        return {
          title: document.getElementById('bv-title').textContent,
          kids: a.kids.map(function (c) {
            var rows = [].filter.call(b.querySelectorAll('.bv-rels li[data-id="' + c + '"], .bv-work-tree li.cm-tn[data-id="' + c + '"]'), vis);
            var links = [].filter.call(b.querySelectorAll('a[href$="bead=' + encodeURIComponent(c) + '"]'), vis);
            return { id: c, rows: rows.length, links: links.length, where: rows.map(function (r) { var x = r.closest('section'); return (x && (x.className + '/' + x.dataset.sec + '/' + x.dataset.rel)) + ':' + r.className; }) };
          }),
          nodeLinks: [].filter.call(b.querySelectorAll('.bv-work a[href*="/garden/c/n/"], .bv-work a[data-node]'), function () { return true; }).length,
          titles: [].map.call(b.querySelectorAll('.bv-work-tree .cm-tn-title'), function (x) { return x.getAttribute('href'); }),
          wb: /Open in workbench/.test(b.textContent),
        };
      }, { kids: kids[id] });
      got.kids.forEach(function (k) {
        assert.ok(k.rows === 1 || (k.rows === 0 && k.links > 0), id + ' (' + got.title + '): child ' + k.id + ' is listed ' + k.rows + ' times, linked ' + k.links + ' ' + JSON.stringify(k.where));
      });
      assert.strictEqual(got.nodeLinks, 0, id + ': a Work row opens the Commons node');
      assert.ok(got.titles.every(function (h) { return /\/garden\?bead=/.test(h); }), id + ': every Work row opens its bead here: ' + got.titles);
      if (got.titles.length) trees++;
      assert.ok(!got.wb, id + ': the panel still offers Open in workbench');
      seen++;
    }
    assert.ok(trees > ids.length / 2, 'most beads draw their tree (a plan lists its phases, a bundle its documents, above): ' + trees + ' of ' + ids.length);
    // a tree row clicked opens its bead in the panel
    var first = ids.filter(function (i) { return i === 'gd-planA'; })[0] || ids[0];
    await page.goto(url + '/garden?bead=' + encodeURIComponent(first));
    var row = page.locator('#bv-body .bv-work-tree .cm-tn-title').first();
    await row.waitFor({ state: 'visible' });
    var to = await row.evaluate(function (a) { return a.closest('li').dataset.id; });
    await row.click();
    await page.waitForFunction(function (to) { return new URLSearchParams(location.search).get('bead') === to && !document.querySelector('#bv-body .bv-work-tree li[data-id="' + to + '"]:not(li li)'); }, to, { timeout: 10000 });
    assert.strictEqual(new URL(page.url()).pathname, '/garden', 'a Work row stays in the garden');
    if (shots) {   // for review: the Work section of plan A's first phase and of the wish, desktop and phone
      for (var c of [{ n: 'desktop', o: { viewport: { width: 1440, height: 900 } } }, { n: 'phone', o: { viewport: { width: 390, height: 844 }, deviceScaleFactor: 2, isMobile: true, hasTouch: true } }]) {
        var sp = await browser.newPage(c.o);
        for (var b of ['gd-A1', 'gd-wishSlack']) {
          await sp.goto(url + '/garden?bead=' + b);
          await sp.waitForFunction(function () { return !!document.querySelector('#bv-body .bv-work[data-drawn]'); });
          await sp.evaluate(function () { var s = document.querySelector('#bv-body .bv-work'), bd = document.getElementById('bv-body'); bd.scrollTop = s.offsetTop - bd.offsetTop - 4; });
          await sp.locator('#bead').screenshot({ path: require('path').join(shots, 'beadwork-' + b + '-' + c.n + '.png') });
        }
        await sp.close();
      }
    }
    assert.deepStrictEqual(errors, [], 'no console errors');
    console.log('beadwork: ' + seen + ' beads, each child once, ' + trees + ' with a Work tree');
  } finally {
    await browser.close();
  }
})().catch(function (e) { console.error(e); process.exit(1); });
