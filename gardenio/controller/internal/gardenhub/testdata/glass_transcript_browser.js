/* Browser check that the Glass carries its gardener's transcript under the terminal (gd-GPu096omMw), run from
   TestGlassTranscriptUnderChromium against a live /garden on an httptest server with the card world. Usage:
   node glass_transcript_browser.js <base url> <node_modules dir>.

     1. On a desktop, gd-GlassLive01's Glass shows the terminal, then a section named Transcript with the rows of the fixture
        activity document (testdata/glass_activity.json) newest first under a heading that counts them and says when the last
        was, then the header's rows; Open the transcript alone links to the transcript kind; Go to its plot is hidden, since
        that agent is on no plot, and Enter on the panel says it is not on a plot and leaves the Glass open (H-D21).
     2. A gardener on a plot (gd-gAskB, plot B) reads its calls from the farm's own ring, and Go to its plot links to the
        plot's ?at=, its figure as the cursor's target; a click on it closes the Glass and puts focus on its plot figure at
        the map's full zoom.
     3. On a phone the section starts folded, so the terminal keeps the screen; opened, it stays open after a reload.
     4. A viewer below the operator tier never gets the Glass: ?glass=<id> lands on ?transcript=<id>. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));
var fixture = require('./glass_activity.json');

function rows(page) {
  return page.evaluate(function () {
    return Array.prototype.map.call(document.querySelectorAll('#bv-body .gl-tx .tx-row'), function (li) { return { words: li.querySelector('.tx-w').textContent, failed: li.classList.contains('failed') }; });
  });
}
async function glass(page, id, n) {
  await page.goto(url + '/garden?glass=' + id);
  await page.waitForFunction(function (n) { return document.querySelectorAll('#bv-body .gl-tx .tx-row').length === n; }, n);
}

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  var errors = [];
  function watch(page) { page.on('pageerror', function (e) { errors.push(String(e)); }); return page; }
  try {
    var opts = { locale: 'en-US', timezoneId: 'UTC', reducedMotion: 'reduce' };
    // 1. the terminal, the transcript, the header's rows, in that order
    var ctx = await browser.newContext(Object.assign({ viewport: { width: 1280, height: 800 } }, opts));
    var page = watch(await ctx.newPage());
    await glass(page, 'gd-GlassLive01', fixture.rows.length);
    var got = await page.evaluate(function () {
      var b = document.getElementById('bv-body'), term = b.querySelector('.gl-term'), sec = b.querySelector('section.gl-tx'), dl = b.querySelector('.gl-rows');
      var after = function (a, z) { return !!(a && z && a.compareDocumentPosition(z) & Node.DOCUMENT_POSITION_FOLLOWING); };
      var plot = b.querySelector('.gl-plot'), alone = Array.prototype.find.call(b.querySelectorAll('.bv-acts a'), function (a) { return a.textContent === 'Open the transcript alone'; });
      return { order: after(term, sec) && after(sec, dl), label: sec.getAttribute('aria-label'), sum: sec.querySelector('.gl-tx-sum').textContent,
        open: sec.querySelector('.gl-tx-fold').getAttribute('aria-expanded'), shown: !!sec.querySelector('.gl-tx-list').offsetParent,
        alone: alone ? alone.getAttribute('href') : '', plotHidden: !!plot && plot.hidden && !plot.offsetParent, old: b.textContent.indexOf('Read its transcript') >= 0 };
    });
    assert.deepStrictEqual(got, { order: true, label: 'Transcript', sum: 'Its last 8 calls, the last at 19:34:12', open: 'true', shown: true,
      alone: '/garden?transcript=gd-GlassLive01', plotHidden: true, old: false }, 'the Glass on a desktop');
    var rs = await rows(page);
    assert.deepStrictEqual(rs.map(function (r) { return r.words.split(' ')[0]; }), ['Run', 'Edit', 'Read', 'Run', 'Show', 'Check', 'Explore', 'Starting'], 'the fixture document, newest first');
    assert.strictEqual(rs.filter(function (r) { return r.failed; }).length, 1, 'its failed call is marked');
    await page.focus('#bv-title');
    await page.keyboard.press('Enter');
    var rest = await page.evaluate(function () { var p = document.getElementById('bead'); return { open: !p.hidden, kind: p.dataset.kind, say: document.getElementById('cursay').textContent }; });
    assert.ok(rest.open && rest.kind === 'glass' && / is not on a plot$/.test(rest.say), 'Enter with no plot to go to: ' + JSON.stringify(rest));

    // 2. a gardener on a plot: the farm's ring, and the way to its plot
    await glass(page, 'gd-gAskB', 12);
    var plot = await page.evaluate(function () { var a = document.querySelector('#bv-body .gl-plot'); return { hidden: a.hidden, href: a.getAttribute('href'), cursor: a.dataset.cursor }; });
    assert.ok(!plot.hidden && /^\/garden\?at=[^&]+\/gd-planB$/.test(plot.href) && plot.cursor === 'gd-gAskB', 'Go to its plot: ' + JSON.stringify(plot));
    await page.click('#bv-body .gl-plot');
    await page.waitForFunction(function () { var a = document.activeElement; return document.getElementById('bead').hidden && !!a && a.matches('#ghots a.w-gardener[href$="gd-gAskB"]'); });
    assert.strictEqual(await page.evaluate(function () { return window.GARDEN.cam().z; }), 4, 'its plot figure at the map\'s full zoom');
    await ctx.close();

    // 3. a phone: folded at first, and as the viewer left it after
    ctx = await browser.newContext(Object.assign({}, pw.devices['iPhone 12'], { viewport: { width: 390, height: 844 } }, opts));
    page = watch(await ctx.newPage());
    await glass(page, 'gd-GlassLive01', fixture.rows.length);
    var fold = page.locator('#bv-body .gl-tx-fold');
    assert.strictEqual(await fold.getAttribute('aria-expanded'), 'false', 'folded on a phone');
    assert.ok(!(await page.locator('#bv-body .gl-tx-list').isVisible()), 'its rows hidden');
    assert.ok(/Its last 8 calls/.test(await fold.textContent()), 'the heading still counts them');
    await fold.tap();
    assert.strictEqual(await fold.getAttribute('aria-expanded'), 'true');
    assert.ok(await page.locator('#bv-body .gl-tx-list').isVisible(), 'opened, its rows show');
    await glass(page, 'gd-GlassLive01', fixture.rows.length);
    assert.strictEqual(await page.locator('#bv-body .gl-tx-fold').getAttribute('aria-expanded'), 'true', 'kept open after a reload');
    await ctx.close();

    // 4. below the operator tier: the transcript kind, never the Glass
    ctx = await browser.newContext(Object.assign({ viewport: { width: 1280, height: 800 }, extraHTTPHeaders: { 'X-Test-Roles': 'gardenio-viewer' } }, opts));
    page = watch(await ctx.newPage());
    await page.goto(url + '/garden?glass=gd-gAskB');
    await page.waitForFunction(function () { var p = document.getElementById('bead'); return !!p && !p.hidden && p.dataset.kind === 'transcript' && document.querySelectorAll('#bv-body .tx-list .tx-row').length === 12; });
    var tier = await page.evaluate(function () {
      var q = new URLSearchParams(location.search);   // the cursor may add its own at=
      return { glass: document.body.dataset.glass || '', transcript: q.get('transcript'), asked: q.has('glass'), watch: !!document.querySelector('#bv-body a[href*="glass="]') };
    });
    assert.deepStrictEqual(tier, { glass: '', transcript: 'gd-gAskB', asked: false, watch: false }, 'the tier-gated path lands on the transcript kind');
    await ctx.close();

    assert.deepStrictEqual(errors, [], 'no page errors');
    console.log('glass transcript: ' + rs.length + ' fixture rows between the terminal and the header; folded on a phone; a viewer lands on ' + tier.search);
  } catch (e) {
    console.error(e && e.stack || e);
    process.exitCode = 1;
  } finally {
    await browser.close();
  }
})();
