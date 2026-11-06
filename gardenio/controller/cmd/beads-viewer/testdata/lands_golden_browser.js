/* Golden screenshots of the Lands front of house (step 7.5, gd-WFIUrR8dZA), run from TestLandsGoldens against the
   viewer's /lands/ pages on an httptest server (and, since step 7.6, the GitHub App setup page; since 7.7, the plot
   application form and the applications page; since the Window's PW.3, a plan's /w/ page and its 404). Usage:
     node lands_golden_browser.js <base url> <node_modules dir> <out dir> <fonts dir> [<full-page dir>]
   Writes <out dir>/<surface>-<device>-<theme>.png (the first viewport) and prints one line per file; the Go test compares
   them with testdata/golden/*.png through internal/golden. With a fifth argument it also writes full-page shots there, for
   grading (commons-ux-acceptance); those are never goldens. LANDS_SWEEP=1 with a full-page dir also shoots the phone sweep
   (Lands mobile M0.1, gd-0B2drDTgFw) there: iPhone 12 and Galaxy S8, portrait and landscape, light and dark, as
   <surface>-<cell>-<theme>.png and -full.png, and writes sweep.json, the sideways scrolls and small targets found, instead
   of failing on them. Modelled on composer_golden_browser.js: the same pinned fonts,
   reduced motion, hidden caret, device scale 1; how to read a failure: gardenio/tests/gardenhub-browser/GOLDENS.md.

   Light and dark are the visitor's system setting (prefers-color-scheme): a stranger has no stored skin, and the pages
   wear the Homestead (step 4.1), picking dark-homestead and the meadow's night scene from their system's dark mode or
   the clock (skin_clock.go), pinned here to noon UTC.

   Each surface is shot at a 390x844 phone, the phone turned sideways (phoneland: the iPhone 12 landscape's 750x340, M4.1
   gd-T3JPWCD9xM) and a 1280x800 desktop. Before any shot, per page and size, the capture fails rather than a pixel budget when:
   - the page threw;
   - the document is wider than the viewport (a sideways scroll on a phone);
   - on either phone, the Commons UX rubric's C3 probes (probes.js) find a control under 44x44 CSS px (a link in running text
     aside, WCAG 2.5.8), a box that scrolls sideways and is not built to, or a field under 16 px, which iOS zooms into on
     focus (M2.4, gd-KiRStJd56I). */
'use strict';
var base = process.argv[2], mods = process.argv[3], out = process.argv[4], fonts = process.argv[5], full = process.argv[6];
var fs = require('fs'), path = require('path'), assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));
var probes = require(path.join(__dirname, '..', '..', '..', '..', '..', '.claude', 'skills', 'commons-ux-acceptance', 'scripts', 'probes.js'));

var DEVICES = {
  phone: Object.assign({}, pw.devices['iPhone 12'], { viewport: { width: 390, height: 844 }, deviceScaleFactor: 1 }),
  phoneland: Object.assign({}, pw.devices['iPhone 12 landscape'], { deviceScaleFactor: 1 }),
  desktop: { viewport: { width: 1280, height: 800 }, deviceScaleFactor: 1 },
};
var THEMES = { light: 'light', dark: 'dark' };
// the phone sweep: the descriptors' own viewports (Safari's bars drawn on the iPhone), Chromium for both engines
var SWEEP = {};
[['ios390', 'iPhone 12'], ['and360', 'Galaxy S8']].forEach(function (d) {
  [['', ''], ['land', ' landscape']].forEach(function (o) {
    var dev = Object.assign({}, pw.devices[d[1] + o[1]], { deviceScaleFactor: 1 });
    delete dev.defaultBrowserType;
    SWEEP[d[0] + o[0]] = dev;
  });
});
var C3 = [];   // every C3 probe hit of the run, failed together once every shot is taken
var SURFACES = [
  { name: 'lands', path: '/lands/', ready: 'main h1' },
  { name: 'landsterms', path: '/lands/terms', ready: 'article.lands-doc h1' },
  { name: 'landsaccept', path: '/lands/accept', ready: 'form button[type=submit]' },
  // The GitHub App setup page (step 7.6, gd-WsBvUZTzft): a passed check, and a refused one (installed on more than one
  // repository). TestLandsGoldens answers installation 1 with a pass and 2 with that refusal.
  { name: 'landsgithub', path: '/lands/github/setup?installation_id=1&code=c&setup_action=install&state=golden', ready: 'dl.lands-facts' },
  { name: 'landsgithubrefused', path: '/lands/github/setup?installation_id=2&code=c&setup_action=install&state=golden', ready: '.lands-err', status: 403 },
  // The plot application (step 7.7, gd-LJoqthyf2w): the form for an applicant who accepted and connected a repository,
  // and the applications page for one with an application the operator asked about and an older one they denied.
  { name: 'landsapply', path: '/lands/apply', ready: 'form.lands-form button[type=submit]' },
  { name: 'landsapplications', path: '/lands/applications', ready: 'ul.lands-apps' },
  // The form sent back with a refusal about one field (M2.4, gd-KiRStJd56I): the shot is of that field in focus, its error
  // between its hint and the field, where it stays in view above a phone's keyboard.
  { name: 'landsapplyerror', path: '/lands/apply', post: 'land_name=tomato-patch&purpose=Keep+the+docs+tidy.&credits=lots', ready: '#credits-error', focus: '#credits', status: 400 },
  // The sign-in picker (step 4.1): standalone like these pages, in the same skin.
  { name: 'login', path: '/login', ready: '.picker a' },
  // The Window (plan gd-pz87vf3xsB, PW.3): a plan through a window link, read with no session (TestLandsGoldens opens
  // the token below on planwindow's phased fixture), and the 404 every other token gets.
  { name: 'window', path: '/w/gw_Zm9yZ2V0LW1lLW5vdC1fXw', ready: 'main#w-main footer.w-foot' },
  { name: 'windowmissing', path: '/w/gw_b3RoZXItd2luZG93LW9rIQ', ready: 'main h1', status: 404 },
];

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
    'code,kbd,pre,samp,mark{font-family:"Golden Mono","Golden Sans",monospace !important}' +
    '</style>';
}

async function shoot(browser, surface, device, theme, style, sweep) {
  var ctx = await browser.newContext(Object.assign({}, sweep ? SWEEP[device] : DEVICES[device], { colorScheme: THEMES[theme], reducedMotion: 'reduce', locale: 'en-US', timezoneId: 'UTC' }));
  await ctx.clock.setFixedTime(new Date('2026-10-20T12:00:00Z'));   // noon: the clock says day, so only the system setting makes night
  var page = await ctx.newPage(), errors = [];
  page.on('pageerror', function (e) { errors.push(String(e)); });
  var url = base + surface.path, doc = new URL(url);
  await page.route(function (u) { return u.origin === doc.origin && u.pathname === doc.pathname; }, async function (route) {
    var res = await route.fetch(surface.post ? { method: 'POST', postData: surface.post, headers: Object.assign({}, route.request().headers(), { 'content-type': 'application/x-www-form-urlencoded' }) } : undefined), body = await res.text();
    assert.ok(body.indexOf('</head>') > 0, surface.name + ' has a </head> to put the fonts before');
    await route.fulfill({ response: res, body: body.replace('</head>', style + '</head>') });
  });
  var res = await page.goto(url);
  assert.strictEqual(res.status(), surface.status || 200, surface.name + ' status');
  await page.waitForSelector(surface.ready, { state: 'visible' });
  await page.evaluate(function () { return Promise.all([document.fonts.load('400 16px "Golden Sans"'), document.fonts.load('700 16px "Golden Sans"'), document.fonts.ready]); });
  assert.strictEqual(await page.evaluate(function () { return document.fonts.check('400 16px "Golden Sans"'); }), true, 'pinned fonts loaded');
  if (surface.focus) await page.focus(surface.focus);
  var skin = await page.evaluate(function () { return document.documentElement.getAttribute('data-skin'); });
  assert.strictEqual(skin, theme === 'dark' ? 'dark-homestead' : 'homestead', surface.name + ' follows the system colour scheme');
  var width = await page.evaluate(function () { return { doc: document.documentElement.scrollWidth, win: window.innerWidth }; });
  var name = surface.name + '-' + device + '-' + theme + '.png';
  if (sweep) {
    var found = await page.evaluate(function () {
      return Array.from(document.querySelectorAll('.lands-btn, button, a, input, select, textarea, summary')).filter(function (el) { return !el.closest('[hidden]') && el.getClientRects().length; }).map(function (el) {
        var r = el.getBoundingClientRect();
        return { tag: el.tagName.toLowerCase(), text: (el.textContent || el.value || el.name || '').trim().slice(0, 40), w: Math.round(r.width), h: Math.round(r.height), inline: el.tagName === 'A' && getComputedStyle(el).display === 'inline' };
      }).filter(function (b) { return b.w < 44 || b.h < 44; });
    });
    await page.evaluate(function () { return new Promise(function (r) { requestAnimationFrame(function () { requestAnimationFrame(r); }); }); });
    await page.screenshot({ path: path.join(full, name), animations: 'disabled', caret: 'hide' });
    await page.screenshot({ path: path.join(full, name.replace('.png', '-full.png')), fullPage: true, animations: 'disabled', caret: 'hide' });
    sweep.push({ shot: name, sideways: width.doc > width.win ? width : null, small: found, errors: errors });
    console.log('swept ' + name);
    return ctx.close();
  }
  assert.ok(width.doc <= width.win, surface.name + '-' + device + ': the page is ' + width.doc + ' px wide in a ' + width.win + ' px viewport');
  if (DEVICES[device].hasTouch) {
    // a control inside a [hidden] box (the music player's panel, closed until a song plays) is not visible, so not probed
    var c3 = await page.evaluate(probes.run, { root: 'body', coarse: true });
    c3.hits.smallTarget.items.concat(c3.hits.sideways.items, c3.hits.zoomField.items).forEach(function (h) { C3.push(name + ': ' + h.sel + ' "' + h.text + '" ' + h.value); });
  }
  await page.evaluate(function () { return new Promise(function (r) { requestAnimationFrame(function () { requestAnimationFrame(r); }); }); });
  await page.screenshot({ path: path.join(out, name), animations: 'disabled', caret: 'hide' });
  if (full) await page.screenshot({ path: path.join(full, name.replace('.png', '-full.png')), fullPage: true, animations: 'disabled', caret: 'hide' });
  assert.deepStrictEqual(errors, [], name + ' page errors');
  console.log('shot ' + name);
  await ctx.close();
}

(async function () {
  assert.ok(base && mods && out && fonts, 'usage: node lands_golden_browser.js <base url> <node_modules> <out dir> <fonts dir> [<full-page dir>]');
  var browser;
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  try {
    var style = fontStyle();
    for (var surface of SURFACES) {
      for (var device of Object.keys(DEVICES)) {
        for (var theme of Object.keys(THEMES)) await shoot(browser, surface, device, theme, style);
      }
    }
    assert.deepStrictEqual(C3, [], 'the rubric C3 probes on the phones');
    console.log('lands shots taken');
    if (process.env.LANDS_SWEEP === '1' && full) {
      var sweep = [];
      for (var s of SURFACES) for (var d of Object.keys(SWEEP)) for (var t of Object.keys(THEMES)) await shoot(browser, s, d, t, style, sweep);
      fs.writeFileSync(path.join(full, 'sweep.json'), JSON.stringify(sweep, null, 2));
      console.log('lands phone sweep in ' + full);
    }
  }
  finally { await browser.close(); }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
