/* What makes a golden shot repeatable, page by page: the pinned fonts, the frozen clock, no tap highlight, the sky pinned to
   the colour scheme, and the probe of the text a shot draws. Split out of golden_browser.js by concern (gd-iXUhuhF1wO), which
   requires it and exports these on to the commons-ux-acceptance skill's shoot.js. Runs nothing of its own. */
'use strict';
var fs = require('fs'), path = require('path'), assert = require('assert');

/* The pinned fonts as one <style>, spliced into the /garden document before </head> so first paint already uses them.
   Display text (the mono stack) falls back to the sans and the sans to the mono for the few symbols only one carries. */
function fontStyle(fontsDir) {
  var face = function (family, file, weight, style) {
    var b64 = fs.readFileSync(path.join(fontsDir, file)).toString('base64');
    return '@font-face{font-family:"' + family + '";src:url(data:font/ttf;base64,' + b64 + ') format("truetype");font-weight:' + weight + ';font-style:' + style + ';font-display:block}';
  };
  return '<style id="golden-fonts">' +
    face('Golden Sans', 'NotoSans-Regular.ttf', 400, 'normal') + face('Golden Sans', 'NotoSans-Bold.ttf', 700, 'normal') +
    face('Golden Sans', 'NotoSans-Italic.ttf', 400, 'italic') +
    face('Golden Mono', 'NotoSansMono-Regular.ttf', 400, 'normal') + face('Golden Mono', 'NotoSansMono-Bold.ttf', 700, 'normal') +
    ':root{--font-display:"Golden Mono","Golden Sans",monospace;--font-body:"Golden Sans","Golden Mono",sans-serif}' +
    '</style>';
}

/* pin makes page shot-ready before its first request: frozen Date.now(), and the pinned fonts spliced into every /garden
   document served from base with NO_TAP_HIGHLIGHT. */
async function pin(page, base, at, style) {
  await page.clock.setFixedTime(at);
  var origin = new URL(base).origin, dress = style + NO_TAP_HIGHLIGHT;
  // /garden itself, a Commons channel or thread URL a browser navigates to (the Commons serves it the same page), and the splash
  await page.route(function (u) { return u.origin === origin && (u.pathname === '/garden' || u.pathname === '/garden/splash' || u.pathname.indexOf('/garden/c/') === 0); }, async function (route) {
    if (!route.request().isNavigationRequest()) return route.continue();   // the pane's own fragment fetches
    // A redirect (a band address to its land's steward, gd-3NuXXEFSgk) goes back to the browser as one, so it lands at the
    // new address; route.fetch would follow it and the page would be served at the old one.
    var res = await route.fetch({ maxRedirects: 0 });
    if (res.status() >= 300 && res.status() < 400) return route.fulfill({ response: res });
    var body = await res.text();
    assert.ok(body.indexOf('</head>') > 0, 'the /garden document has a </head> to put the fonts before');
    await route.fulfill({ response: res, body: body.replace('</head>', dress + '</head>') });
  });
  // A redirect's next hop never reaches the route, so the page it lands on (one of the same documents) is dressed as it
  // loads; one the route served is already dressed and left alone.
  await page.addInitScript(function (a) {
    var p = location.pathname;
    if (location.origin !== a.origin || !(p === '/garden' || p === '/garden/splash' || p.indexOf('/garden/c/') === 0)) return;
    document.addEventListener('DOMContentLoaded', function () {
      if (!document.getElementById('golden-no-tap-highlight')) document.head.insertAdjacentHTML('beforeend', a.dress);
    });
  }, { origin: origin, dress: dress });
}

/* A phone tap (page.tap) lays Chromium's tap highlight, translucent blue, over the tapped control and fades it out on the
   compositor, which animations: 'disabled' does not reach: the Porch tab was shot olive, mid-fade, on a slow runner
   (gd-hs5SdlumPi). Turned off for every element, so no shot catches one. */
var NO_TAP_HIGHLIGHT = '<style id="golden-no-tap-highlight">*{-webkit-tap-highlight-color:transparent!important}</style>';

/* The text a shot draws that may be outside the pinned fonts, for golden.Capture: the characters past printable ASCII, and
   the first family of text whose stack starts with no pinned face, in the visible text nodes, ::before/::after content and
   form fields inside area (the clip, or the viewport) and not scrolled out of view. golden.Pinned.Unpinned judges which
   of them a system font draws, which is what makes a shot differ between an agent pod and the CI runner (gd-VJWqCUihlQ).
   Reads layout and style only, so the shot after it is the same. */
function drawnText(area) {
  area = area || { x: 0, y: 0, width: innerWidth, height: innerHeight };
  var glyphs = {}, families = {}, range = document.createRange(), boxes = new Map();
  var cut = function (a, r) {
    var x = Math.max(a.x, r.left), y = Math.max(a.y, r.top);
    return { x: x, y: y, width: Math.min(a.x + a.width, r.right) - x, height: Math.min(a.y + a.height, r.bottom) - y };
  };
  var box = function (el) {   // area cut by every ancestor that clips its overflow: where el's text can show
    if (!el || el === document.body) return area;
    if (!boxes.has(el)) {
      var b = box(el.parentElement), st = getComputedStyle(el);
      boxes.set(el, st.overflowX !== 'visible' || st.overflowY !== 'visible' ? cut(b, el.getBoundingClientRect()) : b);
    }
    return boxes.get(el);
  };
  var shows = function (b, r) { return r.width > 0 && r.height > 0 && b.width > 0 && b.height > 0 && r.right > b.x && r.left < b.x + b.width && r.bottom > b.y && r.top < b.y + b.height; };
  var note = function (el, text, pseudo) {
    if (!/\S/.test(text)) return;
    for (var ch of text) if (ch > '~') glyphs[ch] = true;
    var first = getComputedStyle(el, pseudo).fontFamily.split(',')[0].trim().replace(/^["']|["']$/g, '');
    if (first !== 'Golden Sans' && first !== 'Golden Mono') families[first] = true;
  };
  var walk = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT), n;
  while ((n = walk.nextNode())) {
    var el = n.parentElement;
    if (!el || !/\S/.test(n.data) || !el.checkVisibility({ visibilityProperty: true })) continue;
    range.selectNodeContents(n);
    var b = box(el);
    if (Array.prototype.some.call(range.getClientRects(), function (r) { return shows(b, r); })) note(el, n.data);
  }
  document.querySelectorAll('body *').forEach(function (el) {
    if (!el.checkVisibility({ visibilityProperty: true }) || !shows(box(el.parentElement), el.getBoundingClientRect())) return;
    ['::before', '::after'].forEach(function (p) {
      (getComputedStyle(el, p).content.match(/"(?:[^"\\]|\\.)*"/g) || []).forEach(function (q) { note(el, q.slice(1, -1).replace(/\\(.)/g, '$1'), p); });
    });
    if (el.matches('textarea') || (el.matches('input') && /^(text|search|email|url|tel|number)$/.test(el.type))) note(el, el.value || el.placeholder || '');
    if (el.matches('select') && el.selectedOptions.length) note(el, el.selectedOptions[0].text);
  });
  return { glyphs: Object.keys(glyphs).join(''), families: Object.keys(families) };
}

/* Every context pins the sky (web/sky.js, gd-SwRqFcjWc6) to its emulated colour scheme: light is day and dark is night
   whatever the clock says, so a run at night (CI is UTC) sees the page a run by day does. */
function skyCtx(browser, opts) {
  opts = opts || {};
  return browser.newContext(opts).then(function (ctx) {
    return ctx.addInitScript(function (s) { try { localStorage.setItem('garden.sky', s); } catch (e) { /* no storage: the emulated scheme still decides by day */ } }, opts.colorScheme === 'dark' ? 'night' : 'day').then(function () { return ctx; });
  });
}

module.exports = { fontStyle: fontStyle, pin: pin, drawnText: drawnText, skyCtx: skyCtx };
