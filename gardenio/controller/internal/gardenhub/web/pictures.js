/* web/pictures.js: the pictures of /garden (Lands workbench-glass spec WB-18, step 5.15, gd-uEEeNMlk1a). A picture is a
   link round a thumbnail (a Commons attachment, a grade's .bv-pics, a markdown or bundle a.bv-md-pic); wire(root) finds
   them once each.
   The preview: one shared div.pic-preview (role=tooltip, aria-describedby while shown), after DELAY ms of a fine-pointer
   hover or at once on focus-visible, and never under a coarse pointer, where a tap opens the viewer (gd-GYSjwuhmed); the
   thumbnail's own src at up to min(70vw, 70vh), beside it inside the viewport, none when no bigger. Fixed and
   pointer-events: none; leave, blur, press, Escape or scroll take it down; none for a held, unloaded or failed picture.
   Its alt is empty (the link's is read once).
   The viewer is the Workbench kind pic, /garden?pic=<path>, local, fetching nothing: a click or Enter opens it on the set
   around the picture (setFrom()); a modified or middle click still opens the file. Previous, Next, "N of M", Open whole and
   a filmstrip; arrows, Home, End and a swipe cycle, wrapping, with a polite live line. Cycling uses replaceState, so a
   reload shows that picture alone; Back closes. Text goes in through textContent.
   size(), place(), path(), nameOf(), wrap(), keyTo(), counter(), swipeTo() and setOf() are pure (pictures_test.go).
   Units: CSS px. */
var PICTURES = (function () {
  'use strict';
  /* ---------- pure logic (also run under node by the Go tests) ---------- */
  var LINKS = '.cm-img > a, .bv-pics a, a.bv-md-pic';
  var SCOPES = '.bv-pics, .cm-msg, .bv-md, .bd-docs, .bv-sec';   // a set: the nearest of these round the picture clicked
  var DELAY = 250, BOX = 0.7, GAP = 8, EDGE = 8, FRAME = 12, MIN_SIDE = 160, SWIPE = 48;   // FRAME: the preview's padding and border, both sides
  function clamp(v, lo, hi) { return Math.max(lo, Math.min(hi, v)); }
  /* p when it is a path on this site (root-relative: never another origin, a scheme or a backslash), else ''. */
  function path(p) { p = String(p || ''); return /^\/(?!\/)[^\\\s]*$/.test(p) ? p : ''; }
  /* A picture's name from its path: the last segment, decoded. */
  function nameOf(p) {
    var s = String(p || '').split(/[?#]/)[0].split('/').pop() || '';
    try { s = decodeURIComponent(s); } catch (e) { /* kept as written */ }
    return s || 'picture';
  }
  /* The preview picture's size for a natural nw x nh shown as a tw x th thumbnail in a vw x vh viewport: as big as fits
     BOX of the viewport's shorter side, never past its natural size unless that is under twice the thumbnail's. null for
     a picture with no size. */
  function size(nw, nh, tw, th, vw, vh) {
    if (!(nw > 0 && nh > 0)) return null;
    var box = Math.min(vw, vh) * BOX, s = Math.min(box / nw, box / nh, Math.max(1, 2 * tw / nw, 2 * th / nh));
    return { w: Math.max(1, Math.round(nw * s)), h: Math.max(1, Math.round(nh * s)) };
  }
  /* Where a w x h preview stands for a thumbnail at rect r ({left, top, right, bottom, width, height}): {x, y, w, h, side}.
     Beside it, to the right when it fits, else the left; short of room on both, shrunk to the roomier side when that has
     MIN_SIDE, else above or below, and over the middle of the viewport when even that is short. Always EDGE inside. A
     place that would leave it smaller than the thumbnail on either side, or no bigger on both, is no preview (gd-9BZII9m6Md):
     the next one is tried, and null when none is bigger (a phone-wide picture: it is already as big as the screen allows,
     and Enter opens the viewer). */
  function place(r, w, h, vw, vh) {
    var right = vw - r.right - GAP - EDGE, left = r.left - GAP - EDGE, below = vh - r.bottom - GAP - EDGE, above = r.top - GAP - EDGE;
    var side = w <= right ? 'right' : w <= left ? 'left' : right >= left ? 'right' : 'left', room = Math.max(right, left), k, x, y;
    function bigger(k) { return w * k >= r.width && h * k >= r.height && (w * k > r.width || h * k > r.height); }
    if (w <= room || room >= MIN_SIDE) {
      k = Math.min(1, (side === 'right' ? right : left) / w, (vh - 2 * EDGE) / h);
      x = side === 'right' ? r.right + GAP : r.left - GAP - w * k;
      y = clamp(r.top + r.height / 2 - h * k / 2, EDGE, vh - EDGE - h * k);
      if (bigger(k)) return at(x, y, w * k, h * k, side);
    }
    room = Math.max(below, above);
    if (room >= MIN_SIDE) {
      side = below >= above ? 'below' : 'above';
      k = Math.min(1, room / h, (vw - 2 * EDGE) / w);
      y = side === 'below' ? r.bottom + GAP : r.top - GAP - h * k;
      x = clamp(r.left + r.width / 2 - w * k / 2, EDGE, vw - EDGE - w * k);
      if (bigger(k)) return at(x, y, w * k, h * k, side);
    }
    k = Math.min(1, (vh - 2 * EDGE) / h, (vw - 2 * EDGE) / w);
    return bigger(k) ? at((vw - w * k) / 2, (vh - h * k) / 2, w * k, h * k, 'over') : null;
  }
  function at(x, y, w, h, side) { return { x: Math.round(x), y: Math.round(y), w: Math.floor(w), h: Math.floor(h), side: side }; }
  function wrap(i, n) { return n > 0 ? ((i % n) + n) % n : 0; }
  /* The picture a key goes to from i of n, or -1 for a key that is not the viewer's (or a set of one). */
  function keyTo(key, i, n) {
    if (n < 2) return -1;
    if (key === 'ArrowLeft') return wrap(i - 1, n);
    if (key === 'ArrowRight') return wrap(i + 1, n);
    if (key === 'Home') return 0;
    if (key === 'End') return n - 1;
    return -1;
  }
  function counter(i, n) { return (i + 1) + ' of ' + n; }
  /* A touch that moved dx, dy: 1 (a swipe left, the next), -1 (right, the previous) or 0 (too short, or more up than across). */
  function swipeTo(dx, dy) { return Math.abs(dx) < SWIPE || Math.abs(dy) >= Math.abs(dx) ? 0 : dx < 0 ? 1 : -1; }
  /* The set the viewer shows for src, {items, i}: items ({src, name, open}) with a path for src, once each, in order, and
     src's place among them; src alone when items does not hold it (a reload, a set the page no longer has); null when src
     is no path on this site. */
  function setOf(items, src) {
    var p = path(src), seen = {}, list = [];
    if (!p) return null;
    (items || []).forEach(function (it) {
      var s = path(it && it.src);
      if (!s || seen[s]) return;
      seen[s] = true;
      list.push({ src: s, name: it.name || nameOf(s), open: it.open || s });
    });
    var i = list.map(function (it) { return it.src; }).indexOf(p);
    return i < 0 ? { items: [{ src: p, name: nameOf(p), open: p }], i: 0 } : { items: list, i: i };
  }
  var api = { LINKS: LINKS, SCOPES: SCOPES, DELAY: DELAY, BOX: BOX, GAP: GAP, EDGE: EDGE, FRAME: FRAME, MIN_SIDE: MIN_SIDE, SWIPE: SWIPE,
    path: path, nameOf: nameOf, size: size, place: place, wrap: wrap, keyTo: keyTo, counter: counter, swipeTo: swipeTo, setOf: setOf, wire: function () {} };
  if (typeof document === 'undefined') return api;

  /* ---------- DOM: the preview ---------- */
  var hoverMq = window.matchMedia ? window.matchMedia('(hover: hover) and (pointer: fine)') : null;
  var wired = new WeakSet(), tip = null, shownFor = null, pending = null, timer = 0, last = null;   // last: the set the last click read
  function el(t, cls, text) { var e = document.createElement(t); if (cls) e.className = cls; if (text !== undefined) e.textContent = text; return e; }
  /* The thumbnail a link previews: one with a src on this site, loaded, not held by data saver; else null. */
  function shown(a) {
    var img = a.querySelector('img');
    return img && !a.closest('.cm-img--held') && path(img.getAttribute('src')) && img.complete && img.naturalWidth > 0 ? img : null;
  }
  function show(a) {
    var img = a.isConnected && shown(a), r = img && a.getBoundingClientRect();
    if (!img) return;
    var vw = document.documentElement.clientWidth || window.innerWidth, vh = window.innerHeight;
    var s = size(img.naturalWidth, img.naturalHeight, r.width, r.height, vw, vh), p = s && place(r, s.w + FRAME, s.h + FRAME, vw, vh);
    if (!p) return;
    if (!tip) {
      tip = el('div', 'pic-preview'); tip.id = 'pic-preview'; tip.setAttribute('role', 'tooltip'); tip.hidden = true;
      var t = tip.appendChild(el('img')); t.alt = ''; t.decoding = 'async'; t.referrerPolicy = 'no-referrer';
      document.body.appendChild(tip);
    }
    if (shownFor && shownFor !== a) shownFor.removeAttribute('aria-describedby');
    var pi = tip.firstChild;
    pi.setAttribute('src', img.getAttribute('src'));
    pi.style.width = (p.w - FRAME) + 'px'; pi.style.height = (p.h - FRAME) + 'px';
    tip.style.left = p.x + 'px'; tip.style.top = p.y + 'px'; tip.dataset.side = p.side;
    tip.hidden = false; shownFor = a;
    a.setAttribute('aria-describedby', 'pic-preview');
  }
  function hide() {
    clearTimeout(timer); timer = 0; pending = null;
    if (!tip || tip.hidden) return;
    tip.hidden = true;
    if (shownFor) shownFor.removeAttribute('aria-describedby');
    shownFor = null;
  }
  function focusVisible(a) { try { return a.matches(':focus-visible'); } catch (e) { return false; } }
  /* The set round the picture a was clicked on: every picture in the nearest scope, in order, but a held one. */
  function setFrom(a) {
    var scope = a.closest(SCOPES) || a.parentNode, items = [], img = a.querySelector('img');
    Array.prototype.forEach.call(scope.querySelectorAll(LINKS), function (x) {
      var i = x.querySelector('img');
      if (!i || x.closest('.cm-img--held')) return;
      items.push({ src: i.getAttribute('src'), name: i.getAttribute('alt') || '', open: /^https?:\/\//i.test(x.href) ? x.href : '' });
    });
    return img ? setOf(items, img.getAttribute('src')) : null;
  }
  function opens(e, a) {
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    if (a.closest('.cm-img--held') || typeof WORKBENCH === 'undefined' || !WORKBENCH.has('pic')) return;   // a held one's tap shows it
    var s = setFrom(a);
    if (!s) return;
    e.preventDefault(); hide();
    last = s;
    WORKBENCH.open('pic', s.items[s.i].src, { opener: a });
  }
  function wire(root) {
    if (!root || !root.querySelectorAll) return;
    if (shownFor && !shownFor.isConnected) hide();   // a repaint took its picture away
    Array.prototype.forEach.call(root.querySelectorAll(LINKS), function (a) {
      if (wired.has(a)) return;
      wired.add(a);
      a.addEventListener('pointerenter', function (e) {
        if (e.pointerType === 'touch' || !(hoverMq && hoverMq.matches)) return;
        clearTimeout(timer); pending = a;
        timer = setTimeout(function () { timer = 0; pending = null; show(a); }, DELAY);
      });
      a.addEventListener('pointerleave', function () { if (shownFor === a || pending === a) hide(); });
      a.addEventListener('pointerdown', hide);
      a.addEventListener('focus', function () { if (hoverMq && hoverMq.matches && focusVisible(a)) show(a); });
      a.addEventListener('blur', function () { if (shownFor === a || pending === a) hide(); });
      a.addEventListener('click', function (e) { opens(e, a); });
    });
  }
  api.wire = wire;
  api.preview = function () { return shownFor; };   // the link whose preview shows, or null (tests)
  document.addEventListener('keydown', function (e) {   // Escape takes the preview down first, and only that
    if (e.key === 'Escape' && tip && !tip.hidden && !e.defaultPrevented) { e.preventDefault(); hide(); }
  }, true);
  window.addEventListener('scroll', hide, { capture: true, passive: true });
  window.addEventListener('resize', hide);

  /* ---------- DOM: the viewer (the Workbench kind pic) ---------- */
  var W = typeof WORKBENCH !== 'undefined' ? WORKBENCH : null, panel = document.getElementById('bead');
  var kindEl = document.getElementById('bv-kind'), titleEl = document.getElementById('bv-title'), bodyEl = document.getElementById('bv-body');
  if (!W || !panel || !bodyEl) return api;
  var v = null;   // the viewer shown: {items, i, img, n, open, prev, next, strip, say}
  function button(text, label, cls, fn) {
    var b = el('button', 'btn small ' + cls, text);
    b.type = 'button'; b.setAttribute('aria-label', label); b.onclick = fn;
    return b;
  }
  function preload(it) { var i = new Image(); i.decoding = 'async'; i.referrerPolicy = 'no-referrer'; i.src = it.src; }
  /* Show picture i of the set; say it when the viewer moved there (not when it opened). */
  function go(i, say) {
    var n = v.items.length, it = v.items[i];
    v.i = i;
    v.img.src = it.src; v.img.alt = it.name;
    titleEl.textContent = it.name;
    v.n.textContent = counter(i, n);
    v.open.href = it.open || it.src;
    Array.prototype.forEach.call(v.strip.children, function (li, j) { if (j === i) li.firstChild.setAttribute('aria-current', 'true'); else li.firstChild.removeAttribute('aria-current'); });
    if (say) v.say.textContent = counter(i, n) + ', ' + it.name;
    if (n > 1) { preload(v.items[wrap(i + 1, n)]); preload(v.items[wrap(i - 1, n)]); }
    if (window.history && history.replaceState && say) {
      try { history.replaceState(history.state, '', location.pathname + W.withPanel(location.search, 'pic', it.src) + location.hash); } catch (e) { /* a sandboxed frame */ }
    }
  }
  function paint(id, s) {
    kindEl.textContent = 'picture';
    bodyEl.textContent = '';
    v = null;
    if (!s) {
      titleEl.textContent = 'Picture';
      bodyEl.appendChild(el('p', 'bv-note err', 'There is no picture at this address: the viewer shows only pictures the garden serves.'));
      return;
    }
    var n = s.items.length, stage = el('div', 'pic-stage'), img = el('img'), bar = el('div', 'pic-bar'), strip = el('ol', 'pic-strip');
    img.decoding = 'async'; img.referrerPolicy = 'no-referrer';
    stage.appendChild(img);
    v = { items: s.items, i: s.i, img: img, strip: strip, n: el('span', 'pic-n'), say: el('p', 'pic-say'), open: el('a', 'btn small ghost pic-open', 'Open') };
    v.prev = button('‹ Previous', 'Previous picture', 'pic-prev', function () { go(wrap(v.i - 1, n), true); });
    v.next = button('Next ›', 'Next picture', 'pic-next', function () { go(wrap(v.i + 1, n), true); });
    v.prev.disabled = v.next.disabled = n < 2;
    v.open.appendChild(el('span', 'pic-whole', ' whole'));   // Open whole; a phone's bar drops the word (no arrow glyph: the goldens' fonts lack one)
    v.open.target = '_blank'; v.open.rel = 'noopener noreferrer'; v.open.setAttribute('aria-label', 'Open the whole picture in a new tab');
    v.say.setAttribute('aria-live', 'polite');
    [v.prev, v.n, v.open, v.next].forEach(function (x) { bar.appendChild(x); });
    strip.setAttribute('aria-label', 'Pictures in this set');
    strip.hidden = n < 2;
    s.items.forEach(function (it, j) {
      var li = el('li'), b = button('', 'Picture ' + counter(j, n) + ': ' + it.name, 'pic-thumb', function () { go(j, true); }), t = el('img');
      t.src = it.src; t.alt = ''; t.loading = 'lazy'; t.decoding = 'async'; t.referrerPolicy = 'no-referrer';
      b.appendChild(t); li.appendChild(b); strip.appendChild(li);
    });
    var sw = null;   // a swipe across the picture cycles it (not from the left edge, which is Back over the Commons)
    stage.addEventListener('touchstart', function (e) { var t = e.touches[0]; sw = e.touches.length === 1 && t.clientX >= 28 ? { x: t.clientX, y: t.clientY } : null; }, { passive: true });
    stage.addEventListener('touchend', function (e) {
      if (!sw || !v) return;
      var t = e.changedTouches[0], d = swipeTo(t.clientX - sw.x, t.clientY - sw.y);
      sw = null;
      if (d && v.items.length > 1) go(wrap(v.i + d, v.items.length), true);
    }, { passive: true });
    [stage, bar, strip, v.say].forEach(function (x) { bodyEl.appendChild(x); });
    go(s.i, false);
  }
  panel.addEventListener('keydown', function (e) {
    var c = W.current();
    if (!v || !c || c.kind !== 'pic' || e.defaultPrevented || e.altKey || e.ctrlKey || e.metaKey || /^(INPUT|TEXTAREA|SELECT)$/.test(e.target.tagName)) return;
    var k = keyTo(e.key, v.i, v.items.length);
    if (k < 0) return;
    e.preventDefault();
    go(k, true);
  });
  W.register('pic', {
    noun: 'picture',
    local: function (id) { return setOf(last ? last.items : [], id); },   // no fetch: the set the click read, else the picture alone
    paint: paint,
    focus: function () { return v && !v.next.disabled ? v.next : null; },
    closed: function () { v = null; }
  });
  return api;
})();
if (typeof module !== 'undefined') module.exports = PICTURES;
