/* Golden screenshots of an empty composer (gd-5e6xgIOH6B), run from TestComposerGoldens against the viewer's /threads/{id}
   on an httptest server fed one Slack-bound wish. Usage:
     node composer_golden_browser.js <page url> <node_modules dir> <out dir> <fonts dir> <empty prompt>
   Writes <out dir>/composer-<device>-<theme>.png and prints one line per file; the Go test compares them with
   testdata/golden/*.png through internal/golden. Modelled on internal/gardenhub/testdata/golden_browser.js (same pinned
   fonts, reduced motion, hidden caret, device scale 1); how to read a failure: gardenio/tests/gardenhub-browser/GOLDENS.md.

   What is shot is the composer row only (the empty textarea showing the house voice, and Send): the label above it names
   the viewer and the hint below it carries a U+2318 the pinned font subsets do not have. Light is the baseline skin, dark
   the dark-terminal skin, homestead the Homestead day skin and dark-homestead its night (plan gd-JaMxRKsc8z, step 4.3),
   each chosen the way the viewer does it (localStorage beads-viewer:skin before first paint). The clock is pinned to noon
   UTC on 2026-10-20, so the Homestead season (data-season, from the month) is always autumn.

   Two checks run before any shot, and fail the capture rather than a pixel budget:
   - the textarea's placeholder IS the house voice (gardenhub.EmptyPrompt, passed in), so the golden pins the constant;
   - in every skin the switcher offers, the placeholder colour clears 4.5:1 against what the textarea is painted on (its
     background composited over its ancestors'), the WCAG text minimum. */
'use strict';
var url = process.argv[2], mods = process.argv[3], out = process.argv[4], fonts = process.argv[5], prompt = process.argv[6];
var fs = require('fs'), path = require('path'), assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));

var DEVICES = {
  phone: Object.assign({}, pw.devices['iPhone 12'], { viewport: { width: 390, height: 844 }, deviceScaleFactor: 1 }),
  desktop: { viewport: { width: 1280, height: 800 }, deviceScaleFactor: 1 },
};
var THEMES = { light: 'baseline', dark: 'dark-terminal', homestead: 'homestead', 'dark-homestead': 'dark-homestead' };
var SKINS = ['homestead', 'dark-homestead', 'baseline', 'minimal-refined', 'dense-admin', 'docs-notion', 'dark-terminal', 'tufte'];
var BOX = '#thread-composer-text', ROW = '#thread-composer .thread2-composer-row';

/* The pinned fonts. The viewer's skins name system stacks (and generic families no @font-face can shadow), so the pinned
   faces are forced onto every element: the shot then shows the composer's layout and colours, not the machine's fonts. */
function fontStyle() {
  var face = function (family, file, weight, style) {
    var b64 = fs.readFileSync(path.join(fonts, file)).toString('base64');
    return '@font-face{font-family:"' + family + '";src:url(data:font/ttf;base64,' + b64 + ') format("truetype");font-weight:' + weight + ';font-style:' + style + ';font-display:block}';
  };
  return '<style id="golden-fonts">' +
    face('Golden Sans', 'NotoSans-Regular.ttf', 400, 'normal') + face('Golden Sans', 'NotoSans-Bold.ttf', 700, 'normal') +
    face('Golden Sans', 'NotoSans-Italic.ttf', 400, 'italic') +
    face('Golden Mono', 'NotoSansMono-Regular.ttf', 400, 'normal') + face('Golden Mono', 'NotoSansMono-Bold.ttf', 700, 'normal') +
    '*{font-family:"Golden Sans","Golden Mono",sans-serif !important}' +
    'code,kbd,pre,samp{font-family:"Golden Mono","Golden Sans",monospace !important}' +
    '</style>';
}

/* contrast runs in the page: the placeholder colour against the textarea's backdrop, alpha-composited up the tree. */
function contrast(sel) {
  function rgba(s) {
    var m = /rgba?\(([^)]+)\)/.exec(s);
    if (!m) throw new Error('unparsed colour ' + s);
    var p = m[1].split(/[\s,/]+/).filter(Boolean).map(parseFloat);
    return { r: p[0], g: p[1], b: p[2], a: p.length > 3 ? p[3] : 1 };
  }
  function over(top, under) {
    var a = top.a + under.a * (1 - top.a);
    if (a === 0) return { r: 0, g: 0, b: 0, a: 0 };
    var mix = function (c) { return (top[c] * top.a + under[c] * under.a * (1 - top.a)) / a; };
    return { r: mix('r'), g: mix('g'), b: mix('b'), a: a };
  }
  function lum(c) {
    var f = function (v) { v /= 255; return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4); };
    return 0.2126 * f(c.r) + 0.7152 * f(c.g) + 0.0722 * f(c.b);
  }
  var el = document.querySelector(sel), layers = [];
  for (var n = el; n; n = n.parentElement) layers.push(rgba(getComputedStyle(n).backgroundColor));
  var bg = { r: 255, g: 255, b: 255, a: 1 };   // the canvas under an html with no background
  for (var i = layers.length - 1; i >= 0; i--) bg = over(layers[i], bg);
  var ph = getComputedStyle(el, '::placeholder');
  var fg = rgba(ph.color);
  fg = over({ r: fg.r, g: fg.g, b: fg.b, a: fg.a * parseFloat(ph.opacity || '1') }, bg);
  var hi = Math.max(lum(fg), lum(bg)), lo = Math.min(lum(fg), lum(bg));
  return { ratio: (hi + 0.05) / (lo + 0.05), fg: ph.color, bg: getComputedStyle(el).backgroundColor };
}

async function open(browser, device, skin, style) {
  var ctx = await browser.newContext(Object.assign({}, DEVICES[device], { colorScheme: skin.indexOf('dark') === 0 ? 'dark' : 'light', reducedMotion: 'reduce', locale: 'en-US', timezoneId: 'UTC' }));
  await ctx.clock.setFixedTime(new Date('2026-10-20T12:00:00Z'));   // the season is the month's: pinned, so the Homestead shots hold
  await ctx.addInitScript(function (s) { try { localStorage.setItem('beads-viewer:skin', s); } catch (e) { /* the capture asserts the skin below */ } }, skin);
  var page = await ctx.newPage(), errors = [];
  page.on('pageerror', function (e) { errors.push(String(e)); });
  var doc = new URL(url);
  await page.route(function (u) { return u.origin === doc.origin && u.pathname === doc.pathname; }, async function (route) {
    var res = await route.fetch(), body = await res.text();
    assert.ok(body.indexOf('</head>') > 0, 'the thread page has a </head> to put the fonts before');
    await route.fulfill({ response: res, body: body.replace('</head>', style + '</head>') });
  });
  await page.goto(url);
  await page.waitForSelector(ROW, { state: 'visible' });
  assert.strictEqual(await page.evaluate(function () { return document.documentElement.getAttribute('data-skin'); }), skin, 'skin applied');
  return { ctx: ctx, page: page, errors: errors };
}

(async function () {
  assert.ok(url && mods && out && fonts && prompt, 'usage: node composer_golden_browser.js <page url> <node_modules> <out dir> <fonts dir> <empty prompt>');
  var browser;
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  try {
    var style = fontStyle();
    for (var skin of SKINS) {
      var s = await open(browser, 'desktop', skin, style);
      assert.strictEqual(await s.page.getAttribute(BOX, 'placeholder'), prompt, 'the empty composer shows the house voice');
      var c = await s.page.evaluate(contrast, BOX);
      console.log('contrast ' + skin + ' ' + c.ratio.toFixed(2) + ':1 (placeholder ' + c.fg + ' on ' + c.bg + ')');
      assert.ok(c.ratio >= 4.5, skin + ': placeholder contrast ' + c.ratio.toFixed(2) + ':1 is under 4.5:1');
      await s.ctx.close();
    }
    for (var device of Object.keys(DEVICES)) {
      for (var theme of Object.keys(THEMES)) {
        var p = await open(browser, device, THEMES[theme], style);
        await p.page.evaluate(function () { return Promise.all([document.fonts.load('400 15px "Golden Sans"'), document.fonts.load('700 12px "Golden Sans"'), document.fonts.ready]); });
        assert.strictEqual(await p.page.evaluate(function () { return document.fonts.check('400 15px "Golden Sans"'); }), true, 'pinned fonts loaded');
        await p.page.evaluate(function () { return new Promise(function (r) { requestAnimationFrame(function () { requestAnimationFrame(r); }); }); });
        var file = path.join(out, 'composer-' + device + '-' + theme + '.png');
        await p.page.locator(ROW).screenshot({ path: file, animations: 'disabled', caret: 'hide' });
        assert.deepStrictEqual(p.errors, [], 'composer-' + device + '-' + theme + ' page errors');
        console.log('shot ' + path.basename(file));
        await p.ctx.close();
      }
    }
    console.log('composer shots taken');
  }
  finally { await browser.close(); }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
