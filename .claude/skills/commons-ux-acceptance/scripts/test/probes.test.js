/* Self-test for probes.js: each §2 probe of the Commons UX rubric fires on a page built to trip it, and none fires on a page
   built to pass. Real Chromium (the probes read layout and computed style), no server: page.setContent.
     node --test .claude/skills/commons-ux-acceptance/scripts/test/probes.test.js
   Needs playwright-core (gardenio/tests/gardenhub-browser/node_modules, or GARDENHUB_PW_MODULES) and its Chromium
   (`make gardenhub-browser-deps`); without them it skips, unless GARDENHUB_BROWSER_REQUIRED=1, as in CI. */
'use strict';
var test = require('node:test'), assert = require('assert'), path = require('path');
var probes = require('../probes.js');

var mods = process.env.GARDENHUB_PW_MODULES || path.join(__dirname, '..', '..', '..', '..', '..', 'gardenio', 'tests', 'gardenhub-browser', 'node_modules');
var pw = null, why = '';
try { pw = require(require.resolve('playwright-core', { paths: [mods] })); } catch (e) { why = 'playwright-core not under ' + mods; }

var BAD = '<!doctype html><html><head><style>' +
  ':root{--ink:#222}' +
  'body{margin:0;font:16px sans-serif;color:var(--ink);background:#fff}' +
  '.clip{width:60px;overflow:hidden;white-space:nowrap}' +
  '.ell{width:60px;overflow:hidden;white-space:nowrap;text-overflow:ellipsis}' +
  '.wide{width:2000px}' +
  '.tiny{font-size:9px}' +
  '.pale{color:#ccc}' +
  '.go:hover{color:var(--ink)}' +
  '.pane{width:200px;overflow:auto}' +
  '</style></head><body><main id="root">' +
  '<h1>One</h1><h1>Two</h1>' +
  '<div class="pane"><p>https://example.com/a/very/long/unbroken/address/that/pushes/the/pane/body/sideways/inside/itself</p></div>' +
  '<div class="clip">A title much too long for its box</div>' +
  '<div class="ell">Another title much too long for its box</div>' +
  '<div class="wide">wide</div>' +
  '<p class="tiny">tiny words</p>' +
  '<p class="pale">pale words on white</p>' +
  '<button style="width:30px;height:30px"></button>' +
  '<label><input type="checkbox" style="width:18px;height:18px"></label>' +
  '<textarea aria-label="Note" style="width:200px;height:60px;font-size:14px"></textarea>' +
  '<a class="go" href="#x">go</a>' +
  '<img src="data:image/gif;base64,R0lGODlhAQABAAAAACw=" width="10" height="10">' +
  '<p>see gd-AbCdEfGhIj for more</p>' +
  '<p>Assigned: gd-A1y (not on a plot)</p>' +
  '<div role="tooltip" style="position:absolute;left:-50px;top:200px;width:120px">A tip off the left edge</div>' +
  '</main></body></html>';

var GOOD = '<!doctype html><html><head><style>' +
  ':root{--ink:#222;--bg:#fff}' +
  'body{margin:0;font:16px sans-serif;color:var(--ink);background:var(--bg)}' +
  '.ell{width:60px;overflow:hidden;white-space:nowrap;text-overflow:ellipsis}' +
  'a,button{display:inline-block;min-width:44px;min-height:44px}' +
  '.go:hover,.go:focus-visible{color:var(--ink)}' +
  '.sr{position:absolute;width:1px;height:1px;margin:-1px;padding:0;overflow:hidden;clip:rect(0 0 0 0);white-space:nowrap;border:0;font-size:12px}' +
  '.pane{width:200px;overflow:auto;overflow-wrap:anywhere}.strip{display:flex;width:200px;overflow-x:auto}.strip>span{flex:none;width:120px}' +
  '.snap{scroll-snap-type:x mandatory}td{white-space:nowrap}pre{overflow:auto}' +
  '</style></head><body><main id="root">' +
  '<div class="pane"><p>https://example.com/a/very/long/unbroken/address/that/wraps/inside/its/pane</p>' +
  '<pre>a code line far too long for a two hundred pixel pane, scrolling inside its own box</pre>' +
  '<div style="overflow-x:auto"><table><tr><td>a table cell far too wide for its wrapper</td><td>another</td></tr></table></div></div>' +
  '<div class="strip snap"><span>One</span><span>Two</span><span>Three</span><span>Four</span></div>' +
  '<div class="sr"><div style="overflow:auto"><p style="white-space:nowrap">https://example.com/closed/outline</p></div></div>' +
  '<div class="strip" data-scroll-x><span>One</span><span>Two</span><span>Three</span><span>Four</span></div>' +
  '<h1>One</h1>' +
  '<span class="sr">A name for a screen reader that runs well past forty characters</span>' +
  '<a class="ell" href="#t">A title much too long for its box</a>' +
  '<p>Plain readable words, an in-flight re-run by gardenio-crew-very-lo-2.</p>' +
  '<button>Send</button>' +
  '<textarea aria-label="Note" style="width:200px;height:60px;font-size:16px"></textarea>' +
  '<label style="display:flex;align-items:center;min-height:44px"><input type="radio" name="role" style="width:18px;height:18px"> Owner</label>' +
  '<a class="go" href="#x">Open</a>' +
  '<img alt="a dot" src="data:image/gif;base64,R0lGODlhAQABAAAAACw=" width="10" height="10">' +
  '<p>see <a href="/garden?bead=gd-AbCdEfGhIj">gd-AbCdEfGhIj the task</a></p>' +
  '<div role="tooltip" style="position:absolute;left:16px;top:200px;width:120px">A tip on the screen</div>' +
  '</main></body></html>';

async function probe(html, coarse) {
  var browser = await pw.chromium.launch();
  try {
    var ctx = await browser.newContext({ viewport: { width: 390, height: 844 }, hasTouch: coarse, isMobile: coarse });
    var page = await ctx.newPage();
    await page.setContent(html);
    return await page.evaluate(probes.run, { root: '#root', coarse: coarse });
  } finally { await browser.close(); }
}

function fired(r) { return Object.keys(r.hits).filter(function (k) { return r.hits[k].n > 0; }).sort(); }

var skip = false;
if (!pw) skip = why;

test('every probe fires on a page built to trip it', { skip: skip }, async function (t) {
  var r;
  try { r = await probe(BAD, true); }
  catch (e) {
    if (process.env.GARDENHUB_BROWSER_REQUIRED === '1') throw e;
    return t.skip('no Chromium: ' + String(e.message).split('\n')[0]);
  }
  assert.deepStrictEqual(fired(r), ['bareId', 'duplicateH1', 'ellipsisNoWay', 'hoverOnly', 'imageWithoutText', 'lowContrast',
    'namelessControl', 'offScreenTip', 'sideways', 'silentClip', 'smallTarget', 'tinyText', 'untokenedColour', 'zoomField']);
  assert.deepStrictEqual(r.hits.bareId.items.map(function (i) { return i.value; }), ['gd-AbCdEfGhIj', 'gd-A1y'], 'a short fixture id is a bare id too');
  assert.ok(/past the left by 50 px/.test(r.hits.offScreenTip.items[0].value), 'the overrun is measured: ' + r.hits.offScreenTip.items[0].value);
  assert.strictEqual(r.hits.duplicateH1.n, 2);
  assert.ok(r.hits.sideways.items.some(function (i) { return /^div\.pane/.test(i.sel.split(' > ').pop()) && /overflow-x: auto/.test(i.value); }),
    'a pane scrolling sideways inside itself is caught: ' + JSON.stringify(r.hits.sideways.items));
  assert.ok(/^1\.[0-9]+:1/.test(r.hits.lowContrast.items[0].value), 'contrast ratio reported: ' + r.hits.lowContrast.items[0].value);
  assert.ok(r.hits.smallTarget.items.some(function (i) { return i.value === '30x30'; }), 'the 30 px button is measured');
  assert.ok(r.hits.smallTarget.items.some(function (i) { return /label > input$/.test(i.sel); }), 'a field in a label hardly bigger than itself is small: ' + JSON.stringify(r.hits.smallTarget.items));
  assert.deepStrictEqual(r.hits.zoomField.items.map(function (i) { return i.text + ' ' + i.value; }), ['Note 14px'], 'the 14 px field zooms on focus, the checkbox does not');
  assert.deepStrictEqual(r.hits.lowContrast.gates, ['C5']);
});

test('no probe fires on a page built to pass', { skip: skip }, async function (t) {
  var r;
  try { r = await probe(GOOD, true); }
  catch (e) {
    if (process.env.GARDENHUB_BROWSER_REQUIRED === '1') throw e;
    return t.skip('no Chromium: ' + String(e.message).split('\n')[0]);
  }
  assert.deepStrictEqual(fired(r), [], JSON.stringify(r.hits, null, 1));
});

test('a snap carousel is exempt only when it snaps on x', { skip: skip }, async function (t) {
  // gd-16gcyG8KTa: 'y proximity' has an x in it; a vertical snap container that scrolls sideways is still a pane pushed sideways.
  // Chromium computes 'y proximity' as 'y' (the default strictness dropped), so the page reports each box's authored value
  // verbatim, as an engine that keeps the full form would, and the probe's regex sees 'proximity'.
  var types = ['y proximity', 'y mandatory', 'x mandatory', 'inline proximity', 'both'];
  var verbatim = '<script>var gcs = getComputedStyle; getComputedStyle = function (el) { var cs = gcs.apply(window, arguments);' +
    ' return el.style.scrollSnapType ? new Proxy(cs, { get: function (o, k) { return k === "scrollSnapType" ? el.getAttribute("style").split(":")[1] :' +
    ' typeof o[k] === "function" ? o[k].bind(o) : o[k]; } }) : cs; };</script>';
  var html = '<!doctype html><html><head><style>.strip{display:flex;width:200px;overflow-x:auto}.strip>span{flex:none;width:120px}</style>' + verbatim + '</head>' +
    '<body><main id="root">' + types.map(function (s) {
      return '<div class="strip" id="s' + s.replace(/ /g, '-') + '" style="scroll-snap-type:' + s + '"><span>One</span><span>Two</span><span>Three</span></div>';
    }).join('') + '</main></body></html>';
  var r;
  try { r = await probe(html, true); }
  catch (e) {
    if (process.env.GARDENHUB_BROWSER_REQUIRED === '1') throw e;
    return t.skip('no Chromium: ' + String(e.message).split('\n')[0]);
  }
  var hit = r.hits.sideways.items.map(function (i) { return i.sel.split(' > ').pop(); }).sort();
  assert.deepStrictEqual(hit, ['div#sy-mandatory', 'div#sy-proximity'], JSON.stringify(r.hits.sideways.items));
});

test('a tooltip is a way to cut text only under a mouse', { skip: skip }, async function (t) {
  var html = '<!doctype html><html><head><style>.ell{width:60px;overflow:hidden;white-space:nowrap;text-overflow:ellipsis}</style></head>' +
    '<body><main id="root"><div class="ell" title="A title much too long for its box">A title much too long for its box</div></main></body></html>';
  var fine, touch;
  try { fine = await probe(html, false); touch = await probe(html, true); }
  catch (e) {
    if (process.env.GARDENHUB_BROWSER_REQUIRED === '1') throw e;
    return t.skip('no Chromium: ' + String(e.message).split('\n')[0]);
  }
  assert.strictEqual(fine.hits.ellipsisNoWay.n, 0, 'a mouse hovers for the title');
  assert.strictEqual(touch.hits.ellipsisNoWay.n, 1, 'a touch pointer never sees it');
});

test('a clamp a tap opens is a way to its rest under touch too', { skip: skip }, async function (t) {
  var html = '<!doctype html><html><head><style>.cl{width:60px;display:-webkit-box;-webkit-box-orient:vertical;-webkit-line-clamp:2;overflow:hidden}</style></head>' +
    '<body><main id="root"><h2 class="cl" data-unclamp>A title much too long for its box, it runs on to many lines</h2></main></body></html>';
  var touch;
  try { touch = await probe(html, true); }
  catch (e) {
    if (process.env.GARDENHUB_BROWSER_REQUIRED === '1') throw e;
    return t.skip('no Chromium: ' + String(e.message).split('\n')[0]);
  }
  assert.strictEqual(touch.hits.ellipsisNoWay.n, 0, JSON.stringify(touch.hits.ellipsisNoWay.items));
});

test('small targets count only under a coarse pointer', { skip: skip }, async function (t) {
  var r;
  try { r = await probe(BAD, false); }
  catch (e) {
    if (process.env.GARDENHUB_BROWSER_REQUIRED === '1') throw e;
    return t.skip('no Chromium: ' + String(e.message).split('\n')[0]);
  }
  assert.strictEqual(r.hits.smallTarget.n, 0);
  assert.strictEqual(r.hits.zoomField.n, 0, 'a mouse page zooms into no field');
});

test('a page wider than the phone scrolls sideways though Chromium widens innerWidth to it', { skip: skip }, async function (t) {
  // gd-t11FduPB9K: with isMobile and a device-width meta viewport, a 484 px page at 390 px reads innerWidth 485, so a probe
  // against innerWidth saw no overflow; the layout viewport (documentElement.clientWidth) stays 390.
  var html = '<!doctype html><html><head><meta name="viewport" content="width=device-width,initial-scale=1"></head>' +
    '<body style="margin:0"><main id="root"><div style="width:484px">wide</div>' +
    '<div role="tooltip" style="position:absolute;left:300px;top:100px;width:150px">A tip past the right edge</div></main></body></html>';
  var r;
  try { r = await probe(html, true); }
  catch (e) {
    if (process.env.GARDENHUB_BROWSER_REQUIRED === '1') throw e;
    return t.skip('no Chromium: ' + String(e.message).split('\n')[0]);
  }
  assert.ok(r.hits.sideways.items.some(function (i) { return i.sel === 'html' && /^48[45] > 390$/.test(i.value); }), JSON.stringify(r.hits.sideways.items));
  assert.ok(r.hits.offScreenTip.items.some(function (i) { return /right by 60 px/.test(i.value); }), JSON.stringify(r.hits.offScreenTip.items));
  assert.strictEqual(r.viewport, '390x844');
});
