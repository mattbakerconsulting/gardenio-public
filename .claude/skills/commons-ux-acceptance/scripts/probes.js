/* The DOM probes of the Commons UX rubric (gardenio/docs/commons-ux-rubric.md §2), run in the page after a surface settles:
     var probes = require('./probes.js');
     var r = await page.evaluate(probes.run, { root: '#bead', coarse: true });
   r.hits maps each probe to { gates, n, items: [{ sel, text, value }] } (items capped at MAX, n is the full count), and
   r.notRun lists probes that could not run and why. A hit is a CANDIDATE fail for the gates it names: the screenshot
   confirms or clears it. run() is one self-contained function because page.evaluate serialises it; it closes over nothing.

   root scopes the element probes to the surface under test (the open card, the bead panel), so a probe of the bead view is
   not flooded by the farm behind it; the page-level probes (the document scrolling sideways, one h1, the stylesheet scans)
   always look at the whole document. coarse says the cell has a touch pointer, where the 44 px target rule applies. */
'use strict';

function run(opts) {
  var MAX = 25;
  var GATES = {
    sideways: ['C2', 'C3'], silentClip: ['C1'], ellipsisNoWay: ['C1'], smallTarget: ['C3', 'C8'], zoomField: ['C3'], tinyText: ['C3', 'C12'],
    lowContrast: ['C5'], untokenedColour: ['C5'], namelessControl: ['C8', 'C12'], imageWithoutText: ['C12'],
    duplicateH1: ['C11', 'C12'], bareId: ['C12'], hoverOnly: ['C3', 'C8'], offScreenTip: ['C1', 'C3'],
  };
  var hits = {}, notRun = {};
  // a bead id: two letters, a dash, and a suffix that carries a capital or a digit (gd-AbCdEfGhIj, ka-0000001234, gd-A1y),
  // standing alone: not a segment of a hyphenated name (gardenio-crew-very-lo-2)
  var BARE_ID = /(?<![\w-])[a-z]{2}-(?=[a-z]*[A-Z0-9])[A-Za-z0-9]{1,16}(?![\w-])/;
  Object.keys(GATES).forEach(function (k) { hits[k] = { gates: GATES[k], n: 0, items: [] }; });
  function hit(k, el, value, text) {
    var h = hits[k];
    h.n++;
    if (h.items.length < MAX) h.items.push({ sel: el ? sel(el) : '', text: (text != null ? text : el ? ownText(el) : '').slice(0, 80), value: value });
  }
  function sel(el) {
    var parts = [];
    for (var e = el; e && e.nodeType === 1 && parts.length < 4; e = e.parentElement) {
      var s = e.tagName.toLowerCase();
      if (e.id) { parts.unshift(s + '#' + e.id); break; }
      if (typeof e.className === 'string' && e.className.trim()) s += '.' + e.className.trim().split(/\s+/).slice(0, 2).join('.');
      parts.unshift(s);
    }
    return parts.join(' > ');
  }
  function ownText(el) {
    var t = '';
    for (var n = el.firstChild; n; n = n.nextSibling) if (n.nodeType === 3) t += n.nodeValue;
    return t.replace(/\s+/g, ' ').trim();
  }
  function visible(el) {
    if (!el.checkVisibility({ opacityProperty: true, visibilityProperty: true })) return false;
    var r = el.getBoundingClientRect();
    return r.width > 0 && r.height > 0;
  }
  function px(v) { return parseFloat(v) || 0; }

  // Colour maths: parse the computed rgb()/rgba(), composite over what is behind, WCAG relative luminance.
  function rgba(s) {
    var m = /rgba?\(([^)]+)\)/.exec(s || '');
    if (!m) return null;
    var p = m[1].split(/[\s,\/]+/).filter(Boolean).map(parseFloat);
    return [p[0], p[1], p[2], p.length > 3 ? p[3] : 1];
  }
  function over(top, under) {
    var a = top[3];
    return [top[0] * a + under[0] * (1 - a), top[1] * a + under[1] * (1 - a), top[2] * a + under[2] * (1 - a), 1];
  }
  function lum(c) {
    var f = function (v) { v /= 255; return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4); };
    return 0.2126 * f(c[0]) + 0.7152 * f(c[1]) + 0.0722 * f(c[2]);
  }
  function ratio(a, b) { var x = lum(a), y = lum(b); return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05); }
  /* The background a text element sits on: its own and its ancestors' background colours, composited from the root down.
     null when an ancestor paints an image or gradient, or the box sits over the canvas, where only a pixel sample answers. */
  function backdrop(el) {
    var layers = [];
    for (var e = el; e && e.nodeType === 1; e = e.parentElement) {
      var cs = getComputedStyle(e);
      if (cs.backgroundImage && cs.backgroundImage !== 'none') return null;
      var c = rgba(cs.backgroundColor);
      if (c && c[3] > 0) { layers.push(c); if (c[3] >= 1) break; }
      if (e.parentElement === null) layers.push([255, 255, 255, 1]);
    }
    if (!layers.length) return null;
    var bg = layers[layers.length - 1];
    for (var i = layers.length - 2; i >= 0; i--) bg = over(layers[i], bg);
    return bg;
  }

  var root = opts.root ? document.querySelector(opts.root) : document.body;
  if (!root) return { error: 'root ' + opts.root + ' not in the page', hits: hits, notRun: notRun };
  var all = [root].concat(Array.prototype.slice.call(root.querySelectorAll('*')));
  /* Screen-reader-only text (garden.css .sr: absolute, 1x1, clipped) is meant to be unseen: not a clip, not tiny text. */
  function srOnly(el) {
    var cs = getComputedStyle(el);
    return cs.position === 'absolute' && el.clientWidth <= 1 && el.clientHeight <= 1 && (cs.clip !== 'auto' || cs.clipPath !== 'none');
  }
  function underSrOnly(el) { for (var e = el.parentElement; e; e = e.parentElement) if (srOnly(e)) return true; return false; }
  var shown = all.filter(function (el) { return !/^(SCRIPT|STYLE|TEMPLATE|NOSCRIPT|BR)$/.test(el.tagName) && visible(el) && !srOnly(el); });

  /* The layout viewport, the width the page's CSS lays out in: not innerWidth, which Chromium with isMobile widens to the
     content, so a 484 px page on a 390 px phone read 484 > 484 (gd-t11FduPB9K). */
  var vw = document.documentElement.clientWidth, vh = document.documentElement.clientHeight;
  // Page scrolls sideways (C2, C3): the whole document, whatever the root.
  if (document.documentElement.scrollWidth > vw) hit('sideways', document.documentElement, document.documentElement.scrollWidth + ' > ' + vw, '');
  /* A box built to scroll sideways inside itself (rubric C2: code blocks and tables scroll in their own box): a form field, a
     pre or code block, a table's wrapper, the Glass terminal (xterm), a picture strip or chip row (.pic-strip, .cm-quick, the
     docked bin's .bin-rows, a pane's tab and scope rows .qv-tabs / .qv-scope), a scroll-snap carousel, and anything marked data-scroll-x. Inside a pre or a terminal, all of it.
     A box inside a visually hidden one (the closed outline, #outline: clipped to 1x1 like .sr) is squashed, not scrolling. */
  var ZOOM_FIELD = 'textarea,select,input:not([type=checkbox],[type=radio],[type=range],[type=color],[type=file],[type=button],[type=submit],[type=reset],[type=image])';
  var SCROLL_X_OK = 'input,textarea,select,pre,code,:has(> table),.gl-term,.xterm,.xterm-viewport,.pic-strip,.cm-quick,.bin-rows,.qv-tabs,.qv-scope,[data-scroll-x]';

  shown.forEach(function (el) {
    var cs = getComputedStyle(el), text = ownText(el);
    // A pane that scrolls sideways inside itself (C2, C3; gd-JPFtrLnNxK): an overflow-x auto|scroll box whose content is wider
    // than it, a long URL pushing the Workbench pane body sideways while the page itself stays put.
    // A carousel is exempt only by its snap axis, the first keyword: an unanchored /x/ matched 'y proximity' (gd-16gcyG8KTa).
    if (/auto|scroll/.test(cs.overflowX) && el.scrollWidth > el.clientWidth + 1 && el !== document.body && el !== document.documentElement &&
        !el.matches(SCROLL_X_OK) && !el.closest('pre,.gl-term,.xterm') && !/^(x|inline|both)\b/.test(cs.scrollSnapType) && !underSrOnly(el)) {
      hit('sideways', el, el.scrollWidth + ' > ' + el.clientWidth + ' (overflow-x: ' + cs.overflowX + ')');
    }
    if (text) {
      // Silent clip (C1): content wider or taller than a box that hides overflow, with no ellipsis and no line clamp.
      var hides = /hidden|clip/.test(cs.overflowX + ' ' + cs.overflowY);
      var clamp = cs.webkitLineClamp && cs.webkitLineClamp !== 'none';
      var wide = el.scrollWidth > el.clientWidth + 1, tall = el.scrollHeight > el.clientHeight + 1;
      var ellipsis = cs.textOverflow === 'ellipsis';
      if (hides && ((wide && !ellipsis) || (tall && !clamp && !ellipsis))) hit('silentClip', el, el.scrollWidth + 'x' + el.scrollHeight + ' in ' + el.clientWidth + 'x' + el.clientHeight);
      // Ellipsis with no way to the rest (C1): an actually-cut box with no full text nearby and no way to open the thing. A
      // title is a way only under a mouse: a touch pointer never shows a tooltip (gd-wSXXwG3VHN). A box marked data-unclamp
      // opens its clamp on a tap (the Workbench head, gd-2UXcdhdN6c).
      if ((ellipsis && wide) || (clamp && tall)) {
        var way = false;
        for (var e = el, up = 0; e && up < 4; e = e.parentElement, up++) {
          if ((e.title && !opts.coarse) || e.hasAttribute('data-unclamp') || e.getAttribute('aria-label') || e.getAttribute('aria-description') || /^(A|BUTTON|SUMMARY)$/.test(e.tagName) || e.getAttribute('role') === 'button') { way = true; break; }
        }
        if (!way) hit('ellipsisNoWay', el, ellipsis ? 'text-overflow: ellipsis' : '-webkit-line-clamp: ' + cs.webkitLineClamp);
      }
      // Tiny text (C3, C12): any text under 11 px; a run long enough to be body text under 13 px.
      var fs = px(cs.fontSize);
      if (fs < 11 || (fs < 13 && text.length > 40)) hit('tinyText', el, fs + 'px' + (fs >= 11 ? ' (body text)' : ''));
      // Low contrast (C5): 4.5:1, or 3:1 for large text (>= 24 px, or >= 18.66 px bold).
      var fg = rgba(cs.color), bg = backdrop(el);
      // opacity on the element or an ancestor fades the text toward its backdrop (approximated: the box behind fades too)
      for (var o = el; fg && o && o.nodeType === 1; o = o.parentElement) fg[3] *= parseFloat(getComputedStyle(o).opacity);
      if (fg && bg) {
        var r = ratio(over(fg, bg), bg), large = fs >= 24 || (fs >= 18.66 && px(cs.fontWeight) >= 700);
        if (r < (large ? 3 : 4.5)) hit('lowContrast', el, r.toFixed(2) + ':1 (' + cs.color + ' on rgb(' + bg.slice(0, 3).map(Math.round).join(',') + '), need ' + (large ? 3 : 4.5) + ')');
      } else if (fg) {
        notRun.lowContrast = (notRun.lowContrast || 0) + 1;   // over an image or the canvas: sample the PNG instead
      }
    }
    // Bare id (C12): a bead id in a text run that is not a link, a chip or code. Any length of suffix, so a short fixture id
    // (gd-A1y) counts as a fleet one does; it must carry a capital or a digit, so a hyphenated word (in-flight) does not.
    var bare = text && BARE_ID.exec(text);
    if (bare && !el.closest('a,code,pre,kbd,[class*=chip]')) hit('bareId', el, bare[0]);

    var control = el.matches('button,a[href],[role=button],summary,input:not([type=hidden]),select,textarea');
    if (control) {
      // Nameless control (C8, C12): no accessible name from any of the usual sources.
      var name = (el.getAttribute('aria-label') || '').trim() || (el.getAttribute('title') || '').trim() || (el.innerText || '').trim() ||
        (el.getAttribute('aria-labelledby') || '').split(/\s+/).map(function (id) { var t = document.getElementById(id); return t ? t.textContent.trim() : ''; }).join('') ||
        (el.labels && el.labels.length ? el.labels[0].textContent.trim() : '') || (el.getAttribute('placeholder') || '').trim() ||
        Array.prototype.map.call(el.querySelectorAll('img[alt]'), function (i) { return i.alt; }).join('').trim();
      if (!name) hit('namelessControl', el, 'no accessible name', el.outerHTML.slice(0, 80));
      // Small target (C3, C8): under a coarse pointer, smaller than 44x44 counting a declared ::after hit-slop. A link inside
      // running text is exempt (WCAG 2.5.8): its parent carries more text than it does. A field inside its label is tapped
      // through the label, so the label's box counts (the People lightbox's role radios, gd-T3JPWCD9xM).
      if (opts.coarse) {
        var b = el.getBoundingClientRect(), w = b.width, h = b.height, af = getComputedStyle(el, '::after');
        if (af.content && af.content !== 'none' && af.position === 'absolute') {
          w += Math.max(0, -px(af.left)) + Math.max(0, -px(af.right));
          h += Math.max(0, -px(af.top)) + Math.max(0, -px(af.bottom));
        }
        var lab = el.matches('input,select,textarea') && el.closest('label');
        if (lab) { var lb = lab.getBoundingClientRect(); w = Math.max(w, lb.width); h = Math.max(h, lb.height); }
        var inline = el.tagName === 'A' && cs.display === 'inline' && el.parentElement && el.parentElement.textContent.trim().length > el.textContent.trim().length + 3;
        if ((w < 44 || h < 44) && !inline) hit('smallTarget', el, Math.round(w) + 'x' + Math.round(h), (el.innerText || el.getAttribute('aria-label') || '').trim());
        // Zoom field (C3; lands-mobile.md §2, gd-xWWqgcmzXM): iOS zooms the page into a focused field whose text is under 16 px.
        if (el.matches(ZOOM_FIELD) && px(cs.fontSize) < 16) hit('zoomField', el, cs.fontSize, el.getAttribute('aria-label') || el.name || '');
      }
    }
    // Image without text (C12): an img with no alt, a canvas or svg the reader can see that names nothing.
    if (el.tagName === 'IMG' && !el.hasAttribute('alt')) hit('imageWithoutText', el, 'img without alt', el.getAttribute('src') || '');
    if ((el.tagName === 'CANVAS' || el.tagName === 'svg') && !el.closest('[aria-hidden=true]') && !el.getAttribute('aria-label') && !el.getAttribute('aria-labelledby') && el.getAttribute('role') !== 'presentation') {
      hit('imageWithoutText', el, el.tagName.toLowerCase() + ' without aria-label', '');
    }
  });

  // Tip off the screen (C1, C3; gd-O3nwaqbNUG): a shown tooltip whose box leaves the viewport. Page-level, whatever the root:
  // a tip floats over the surface it belongs to.
  Array.prototype.forEach.call(document.querySelectorAll('[role=tooltip]'), function (el) {
    if (!visible(el)) return;
    var b = el.getBoundingClientRect(), out = [];
    if (b.left < 0) out.push('left by ' + Math.round(-b.left) + ' px');
    if (b.top < 0) out.push('top by ' + Math.round(-b.top) + ' px');
    if (b.right > vw) out.push('right by ' + Math.round(b.right - vw) + ' px');
    if (b.bottom > vh) out.push('bottom by ' + Math.round(b.bottom - vh) + ' px');
    if (out.length) hit('offScreenTip', el, Math.round(b.width) + 'x' + Math.round(b.height) + ' past the ' + out.join(', '), (el.innerText || '').trim());
  });

  // Duplicate heading (C11, C12): one h1 per page.
  var h1 = document.querySelectorAll('h1');
  if (h1.length > 1) Array.prototype.forEach.call(h1, function (h) { hit('duplicateH1', h, h1.length + ' h1 on the page'); });

  /* The stylesheet scans, once per page: a :hover rule with no :focus-visible / :focus / :focus-within twin (hover-only, C3
     and C8), and a colour literal outside the :root token blocks (untokened colour, C5). Cross-origin sheets cannot be read. */
  var rules = [];
  function walk(list, media) {
    Array.prototype.forEach.call(list, function (r) {
      if (r.cssRules && !r.selectorText) walk(r.cssRules, media || (r.conditionText || r.media && r.media.mediaText || ''));
      else if (r.selectorText) rules.push({ sel: r.selectorText, css: r.style.cssText, media: media || '' });
    });
  }
  Array.prototype.forEach.call(document.styleSheets, function (s) {
    try { walk(s.cssRules, ''); } catch (e) { notRun.stylesheets = (notRun.stylesheets || []).concat(s.href || 'inline'); }
  });
  var sels = {};
  rules.forEach(function (r) { r.sel.split(',').forEach(function (s) { sels[s.trim()] = true; }); });
  rules.forEach(function (r) {
    r.sel.split(',').forEach(function (s) {
      s = s.trim();
      if (s.indexOf(':hover') < 0) return;
      var twins = [':focus-visible', ':focus', ':focus-within'].map(function (f) { return s.replace(/:hover/g, f); });
      if (!twins.some(function (t) { return sels[t]; })) hit('hoverOnly', null, r.media ? '@media ' + r.media : '', s);
    });
    if (/(^|,)\s*:root\b/.test(r.sel)) return;
    // a custom property defines a token wherever it is declared; only a literal in an ordinary property is untokened
    var lit = /#[0-9a-fA-F]{3,8}\b|\b(rgb|hsl)a?\(/.exec(r.css.split(';').filter(function (d) { return !/^\s*--/.test(d); }).join(';'));
    if (lit) hit('untokenedColour', null, lit[0], r.sel.slice(0, 80));
  });

  return { root: opts.root || 'body', coarse: !!opts.coarse, viewport: vw + 'x' + vh, hits: hits, notRun: notRun };
}

module.exports = { run: run };
