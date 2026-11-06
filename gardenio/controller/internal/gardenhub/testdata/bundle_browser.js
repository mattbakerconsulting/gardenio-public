/* Browser check for the bundle panel (Lands workbench-glass spec WB-9, step 5.10), run from TestBundleUnderChromium against
   /garden on cardWorld, whose bundle reader is fixtureBundles (bundle_test.go). Usage:
   node bundle_browser.js <base url> <node_modules dir>.

   Desktop: the bundle's bead face says "Read as one document", which opens the panel here: the address says bundle=, the
   header names the bundle, the line under it its documents, the bead it was filed for, the pull request and the bundle
   bead; the documents come in the reader's order, each by its format (markdown's heading dropped when it repeats the
   headline, a diff's lines marked in words, a csv as a table with its header, the screenshot loaded through the image
   proxy, a link to a new tab, text as text), and the contents rail lists them all; a rail button brings its document to
   the top and focuses it, and the address keeps no #fragment. The "for" link opens its bead here and Back returns to the
   bundle. The rail marks the document being read (aria-current), the clicked one after a click, the last at the
   body's end and the first back at its top. An html page runs no script, neither in its frame nor on this page. A daemon failure on the documents says so
   with Try again, and an unknown bundle says it is not there. Phone: no rail, the documents one after another, and a
   tap opens a clamped head title. No console errors. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));

function search(page) { return page.evaluate(function () { return location.search + location.hash; }); }
function docs(page) { return page.evaluate(function () { return Array.prototype.map.call(document.querySelectorAll('#bv-body .bd-doc'), function (a) { return a.dataset.id + ':' + a.dataset.format; }); }); }
async function shown(page, title) {
  await page.waitForFunction(function (title) {
    var p = document.getElementById('bead');
    return !p.hidden && p.dataset.kind === 'bundle' && document.getElementById('bv-title').textContent === title && !/Loading/.test(document.getElementById('bv-body').textContent);
  }, title);
}
function watch(page) {
  var errors = [];
  page.on('console', function (m) { if (m.type() === 'error') errors.push(m.text()); });
  page.on('pageerror', function (e) { errors.push(String(e)); });
  return errors;
}
var ORDER = ['gd-FaceBndDoc2:md', 'gd-FaceBndDoc1:md', 'gd-FaceBndDoc4:diff', 'gd-FaceBndDoc5:csv', 'gd-FaceBndDoc6:image', 'gd-FaceBndDoc7:link', 'gd-FaceBndDoc3:text'];

async function desktop(browser) {
  var ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } }), page = await ctx.newPage(), errors = watch(page);
  await page.goto(url + '/garden?bead=gd-FaceBundle1');
  await page.waitForFunction(function () { return window.WORKBENCH && WORKBENCH.has('bundle') && !!document.querySelector('#bv-body .bv-face .bv-acts a'); });
  var open = page.locator('#bv-body .bv-face .bv-acts a', { hasText: 'Read as one document' });
  await open.click();
  await shown(page, 'Workbench narrative');
  assert.ok(/(^|[?&])bundle=gd-FaceBundle1($|&)/.test(await search(page)), 'the address names the panel: ' + await search(page));
  assert.strictEqual(await page.locator('#bead .bv-kind').textContent(), 'bundle');
  var meta = await page.locator('#bv-body .bd-meta').textContent();
  assert.ok(/^7 documents · plan narrative/.test(meta), 'the line under the title: ' + meta);
  assert.ok(/for 5\. Workbench/.test(meta) && /from !57 · merged/.test(meta), 'its bead and its pull request: ' + meta);
  assert.strictEqual(await page.locator('#bv-body .bd-meta a.bv-url').getAttribute('target'), '_blank', 'the pull request opens a new tab');
  assert.deepStrictEqual(await docs(page), ORDER, "the reader's order, each by its format");

  var first = page.locator('#bv-body .bd-doc').first();
  assert.strictEqual(await first.locator('.bd-h').textContent(), 'The face block');
  assert.strictEqual(await first.locator('.bv-md h4, .bv-md h5, .bv-md h6').count(), 0, 'a heading that repeats the headline is dropped');
  assert.strictEqual(await first.locator('.bv-md li').count(), 2, 'its list');
  assert.ok(/type beadFace struct/.test(await first.locator('.bv-md pre code').textContent()), 'its code block');
  assert.strictEqual(await page.locator('#bv-body .bd-doc').nth(1).locator('a.bv-ref').count(), 1, 'a bead id in a document is a link here');

  var diff = page.locator('#bv-body .bd-doc[data-format=diff]');
  assert.strictEqual(await diff.locator('.bd-sub').nth(1).textContent(), '1 file · 2 added · 1 removed');
  assert.strictEqual(await diff.locator('.bd-add').count(), 2);
  assert.strictEqual(await diff.locator('.bd-del').first().getAttribute('aria-label'), 'removed: // faces', 'a removed line says so in words');
  assert.strictEqual(await diff.locator('.bd-sub').first().textContent(), 'diff · go · its bead', 'what it is, its language and its bead');

  var csv = page.locator('#bv-body .bd-doc[data-format=csv]');
  assert.deepStrictEqual(await csv.locator('th').allTextContents(), ['Face', 'Phone', 'Desktop']);
  assert.strictEqual(await csv.locator('tbody tr').count(), 3);
  assert.strictEqual(await csv.locator('tbody tr').nth(2).locator('td').first().textContent(), 'report, markdown', 'a quoted field keeps its comma');

  var img = page.locator('#bv-body .bd-doc[data-format=image] img');
  await page.waitForFunction(function () { var i = document.querySelector('#bv-body .bd-doc[data-format=image] img'); return i && i.complete && i.naturalWidth > 0; });
  assert.strictEqual(await img.getAttribute('alt'), 'The bead panel on a phone');
  assert.ok(/\/image\/coop\/sessions\/images\/bundle-face-320x200\.png$/.test(await img.getAttribute('src')), 'through the image proxy');
  var link = page.locator('#bv-body .bd-doc[data-format=link] a.bd-link');
  assert.strictEqual(await link.getAttribute('href'), 'https://github.com/acme/garden/pull/57');
  assert.strictEqual(await link.getAttribute('target'), '_blank');
  assert.strictEqual(await page.locator('#bv-body .bd-doc[data-format=text] pre').textContent(), 'Inline edit, comments, lifecycle.');
  // every block of code is in the page's own mono, never the browser's monospace (which differs by machine, and moved the goldens)
  var fonts = await page.evaluate(function () {
    var want = getComputedStyle(document.documentElement).getPropertyValue('--font-display').trim().replace(/\s*,\s*/g, ',');
    return Array.prototype.filter.call(document.querySelectorAll('#bv-body .bd-doc pre, #bv-body .bd-doc pre code'), function (e) {
      return getComputedStyle(e).fontFamily.replace(/\s*,\s*/g, ',') !== want;
    }).map(function (e) { return e.tagName + ' ' + getComputedStyle(e).fontFamily; });
  });
  assert.deepStrictEqual(fonts, [], 'a code block in a font other than --font-display');

  // the contents rail: every headline, and a button brings its document up and focuses it
  var rail = page.locator('#bv-body .bd-toc');
  assert.ok(await rail.isVisible(), 'a desktop shows the contents');
  assert.strictEqual(await rail.locator('button').count(), 7);
  await rail.locator('button', { hasText: 'Faces graded' }).click();
  await page.waitForFunction(function () { var a = document.activeElement; return a && a.dataset && a.dataset.id === 'gd-FaceBndDoc5'; });
  var top = await page.evaluate(function () { var b = document.getElementById('bv-body'), d = document.querySelector('.bd-doc[data-id=gd-FaceBndDoc5]'); return Math.round(d.getBoundingClientRect().top - b.getBoundingClientRect().top); });
  assert.ok(top >= 0 && top <= 24, 'the document is brought to the top: ' + top);
  assert.ok(!/#/.test(await search(page)), 'no #fragment in the address');
  // the rail follows the reader: the document read is its one current entry, in the rail's view (gd-oLUkvMS97l)
  function current() {
    return page.evaluate(function () {
      var c = document.querySelectorAll('#bv-body .bd-toc [aria-current]'), n = document.querySelector('#bv-body .bd-toc').getBoundingClientRect();
      var r = c.length === 1 && c[0].getBoundingClientRect();
      return c.length + ' ' + (c[0] ? c[0].textContent : '') + (r && (r.top < n.top - 1 || r.bottom > n.bottom + 1) ? ' (out of view)' : '');
    });
  }
  assert.strictEqual(await current(), '1 Faces graded', 'a clicked entry is current');
  await page.evaluate(function () { var b = document.getElementById('bv-body'); b.scrollTop = b.scrollHeight; });
  await page.waitForFunction(function (t) { var c = document.querySelector('#bv-body .bd-toc [aria-current]'); return c && c.textContent === t; }, await rail.locator('button').last().textContent());
  await page.evaluate(function () { document.getElementById('bv-body').scrollTop = 0; });
  await page.waitForFunction(function (t) { var c = document.querySelector('#bv-body .bd-toc [aria-current]'); return c && c.textContent === t; }, await rail.locator('button').first().textContent());
  assert.ok(!/out of view/.test(await current()), 'the current entry is in the rail\'s view: ' + await current());

  // the bead it was filed for opens here, and Back returns to the bundle
  await page.locator('#bv-body .bd-meta a', { hasText: '5. Workbench' }).click();
  await page.waitForFunction(function () { return document.getElementById('bead').dataset.kind === 'bead' && /(^|[?&])bead=gd-FaceEpic01/.test(location.search); });
  await page.goBack();
  await shown(page, 'Workbench narrative');

  // an html page runs no script
  await page.goto(url + '/garden?bundle=gd-BundleHtml1');
  await shown(page, 'A web page');
  var frame = page.locator('#bv-body iframe.bd-frame');
  assert.strictEqual(await frame.getAttribute('sandbox'), 'allow-popups allow-popups-to-escape-sandbox', 'no scripts, no same origin');
  await page.waitForFunction(function () { var f = document.querySelector('#bv-body iframe.bd-frame'); return f && f.contentWindow && f.contentWindow.length === 0; });
  var inner = await page.frameLocator('#bv-body iframe.bd-frame').locator('#p').textContent();
  assert.strictEqual(inner, 'framed', 'the frame ran no script');
  assert.strictEqual(await page.evaluate(function () { return document.body.dataset.pwned || ''; }), '', 'nor reached this page');

  // what could not be read says so
  await page.goto(url + '/garden?bundle=gd-BundleDown1');
  await shown(page, 'A bundle the daemon half answered');
  assert.ok(/Could not read its documents: the beads daemon did not answer\./.test(await page.locator('#bv-body').textContent()));
  assert.strictEqual(await page.locator('#bv-body .bv-retry').count(), 1, 'with Try again');
  await page.goto(url + '/garden?bundle=gd-nothere00');
  await page.waitForFunction(function () { return /There is no bundle gd-nothere00, or you cannot see it\./.test(document.getElementById('bv-body').textContent); });
  assert.strictEqual(await page.locator('#bv-body .bv-retry').count(), 0, 'nothing to try again');

  // the unknown bundle's 404, and Chromium saying the frame's script was blocked (the sandbox at work), are the only errors
  assert.deepStrictEqual(errors.filter(function (e) { return !/404/.test(e) && !/^Blocked script execution in 'about:srcdoc'/.test(e); }), [], 'no console errors');
  await ctx.close();
}

async function phone(browser) {
  var ctx = await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true }), page = await ctx.newPage(), errors = watch(page);
  /* The head title clamps to two lines and a tap opens it, as a phone shows no tooltip; a new title starts clamped
     (gd-2UXcdhdN6c). The long title is the bundle's own: the panel's live stream opens after the page's load and its open
     repaints the panel, which would put back the bundle's title over one written here and close it (gd-fWCbRpfcat). */
  var LONG = 'A long human title that keeps going '.repeat(6).trim();
  await page.route('**/garden/bundle/gd-FaceBundle1', async function (r) { var res = await r.fetch(), d = await res.json(); d.title = LONG; await r.fulfill({ response: res, json: d }); });
  await page.goto(url + '/garden?bundle=gd-FaceBundle1');
  await shown(page, LONG);
  assert.ok(await page.locator('#bv-body .bd-toc').isHidden(), 'a phone has no contents rail');
  assert.deepStrictEqual(await docs(page), ORDER);
  var wide = await page.evaluate(function () { var b = document.getElementById('bv-body'); return b.scrollWidth - b.clientWidth; });
  assert.ok(wide <= 1, 'nothing runs off the side of the panel: ' + wide);
  var cut = function () { return page.evaluate(function () { var h = document.getElementById('bv-title'); return h.scrollHeight > h.clientHeight + 1; }); };
  assert.ok(await cut(), 'a long title is clamped');
  /* a tap's click follows its touch events, so a slow runner may measure before it lands: wait for the toggle */
  var opened = function (on) { return page.waitForFunction(function (on) { return document.getElementById('bv-title').classList.contains('open') === on; }, on); };
  await page.tap('#bv-title');
  await opened(true);
  assert.ok(!(await cut()), 'a tap opens the whole title');
  await page.tap('#bv-title');
  await opened(false);
  assert.ok(await cut(), 'a second tap clamps it again');
  await page.tap('#bv-title');
  await page.evaluate(function (t) { document.getElementById('bv-title').textContent = t + ' again'; }, LONG);
  await page.waitForFunction(function () { return !document.getElementById('bv-title').classList.contains('open'); });
  assert.ok(await cut(), 'a new title starts clamped');
  assert.deepStrictEqual(errors, [], 'no console errors');
  await ctx.close();
}

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); } catch (e) { console.log('no chromium: ' + e.message); process.exit(3); }
  try { await desktop(browser); await phone(browser); } finally { await browser.close(); }
  console.log('bundle panel: ok');
})().catch(function (e) { console.error(e); process.exit(1); });
