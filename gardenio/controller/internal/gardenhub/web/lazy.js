/* LAZY: the World first, the rest once it is up (P1.3, gd-p02ZelpMxk). First paint runs page-scene.js and page-farm.js;
   this loads the bundles garden.html only names (body data-*-js): page-workbench.js (Porch chat, bead view, Workbench),
   a second after the first farm document has drawn (on a panel's deep link, and the panel its document), when idle, or at
   once for a panel link or a bead opened; page-glass.js, page-land.js and page-panels.js after page-workbench.js, when what
   they draw is asked for (EXTRA: unasked). The Commons is the workbench's own page (web/node.html), not a bundle here.
   BEAD and CHAT are stubs until workbench.js and chat.js replace them; a click the rest would take (a phone's tap on a
   gardener too) is held and replayed once it has run. */
var LAZY = (function () {
  'use strict';
  /* WORKBENCH.KINDS (lazy_test.go keeps the two the same) */
  var KINDS = ['bead', 'plan', 'project', 'queue', 'q', 'bundle', 'gardener', 'glass', 'costs', 'creds', 'settings', 'pic', 'transcript', 'release'];
  function dec(s) { try { return decodeURIComponent(String(s).replace(/\+/g, ' ')); } catch (e) { return String(s); } }
  function kindsIn(search) {
    return String(search || '').replace(/^\?/, '').split('&').map(function (p) {
      var eq = p.indexOf('=');
      return dec(eq < 0 ? p : p.slice(0, eq));
    }).filter(function (k) { return KINDS.indexOf(k) >= 0; });
  }
  /* the kinds page-panels.js draws (garden.html's lazy preload names the same) */
  var PANELS = ['gardener', 'bundle', 'settings', 'release'];
  function panelIn(search) { return kindsIn(search).length > 0; }
  /* the bundle that draws a panel: page-wall.js the Wall (?glass= with no agent, or wall; glass: the viewer may watch),
     page-glass.js one agent's Glass and the credential pool, page-panels.js its kinds */
  function bundleOf(kind, glass, id) {
    if (kind === 'creds') return 'glass';
    if (PANELS.indexOf(kind) >= 0) return 'panels';
    return kind === 'glass' && glass ? (!id || id === 'wall' ? 'wall' : 'glass') : '';
  }
  function landIn(search) { return /(^|[?&])land=[^&]/.test(String(search || '')); }
  /* what an address needs at once: 'workbench' (a panel parameter) or '' */
  function needOf(search) { return panelIn(search) ? 'workbench' : ''; }
  /* a link the Workbench opens in place: this page's /garden with a panel parameter */
  function panelLink(base, href, origin) {
    var u;
    try { u = new URL(href, origin + '/'); } catch (e) { return false; }
    return u.origin === origin && u.pathname === base + '/garden' && panelIn(u.search);
  }

  var api = { KINDS: KINDS, PANELS: PANELS, panelIn: panelIn, needOf: needOf, panelLink: panelLink, bundleOf: bundleOf, landIn: landIn, later: function () { return ''; }, need: function () { return Promise.reject(new Error('no page')); }, loaded: function () { return false; }, opened: '' };
  if (typeof document === 'undefined') return api;
  api.opened = location.search;   // the query the page opened with: the cursor writes its own ?at= before the Workbench runs

  var B = document.body, base = B.dataset.base || '';
  var SRC = {};
  ['workbench', 'wall', 'glass', 'land', 'panels'].forEach(function (n) { SRC[n] = B.dataset[n + 'Js'] || ''; });
  var ORDER = ['workbench'], got = {}, done = {}, auto = {}, glass = B.dataset.glass === '1';
  var EXTRA = { wall: false, glass: false, panels: false, land: landIn(location.search) || B.dataset.cut === '1' };   // a land's header, a tenant's credits

  /* a bundle's <script>, once; a failed load is forgotten so the next ask retries */
  function script(name) {
    if (got[name]) return got[name];
    got[name] = new Promise(function (ok, no) {
      var s = document.createElement('script');
      s.src = SRC[name];
      s.onload = function () { done[name] = true; ok(); if (name === 'workbench') extras(); };
      s.onerror = function () { got[name] = null; s.remove(); no(new Error('The page’s ' + name + ' did not load.')); };
      document.body.appendChild(s);
    });
    return got[name];
  }
  /* after the Workbench: this viewer's extras and the address's panel's */
  function extras() {
    var want = Object.keys(EXTRA).filter(function (n) { return EXTRA[n]; });
    var asked = String(location.search).replace(/^\?/, '').split('&').map(function (p) {
      var eq = p.indexOf('=');
      return bundleOf(dec(eq < 0 ? p : p.slice(0, eq)), glass, eq < 0 ? '' : dec(p.slice(eq + 1)).trim());
    });
    want.concat(asked).forEach(function (n) { if (SRC[n]) { auto[n] = true; need(n).catch(function () {}); } });
  }
  /* need(name): it and every bundle before it in ORDER, run in that order */
  function need(name) {
    var at = ORDER.indexOf(name);
    if (name in EXTRA && SRC[name]) return need('workbench').then(function () { return script(name); });
    if (at < 0) return Promise.reject(new Error('no bundle ' + name));
    return ORDER.slice(0, at + 1).reduce(function (p, n) { return p.then(function () { return script(n); }); }, Promise.resolve());
  }
  /* loaded(): every bundle the page loads unasked */
  function loaded(name) { return name ? !!done[name] : ORDER.concat(Object.keys(auto)).every(function (n) { return done[n]; }); }
  api.need = need;
  api.loaded = loaded;
  api.later = function (kind, id) { var b = bundleOf(kind, glass, id); return b && SRC[b] ? b : ''; };

  var first = needOf(location.search) || (landIn(location.search) ? 'land' : '');
  function rest() { need('workbench').catch(function () {}); }
  function idle() { (window.requestIdleCallback || function (f) { return setTimeout(f, 1); })(rest, { timeout: 2000 }); }
  /* the rest once every event in names has fired, a second on (what they draw settles: the perf script's World Ready, a
     pane's LCP), when idle; at 6 s whatever has not come (no farm document, a panel that never loads) */
  function after(names) {
    var left = names.length, later = setTimeout(go, 6000);
    function go() { if (later === null) return; clearTimeout(later); later = null; setTimeout(idle, 1000); }
    names.forEach(function (n) {
      document.addEventListener(n, function once() { document.removeEventListener(n, once); if (--left === 0) go(); });
    });
  }
  /* a panel's deep link: the rest waits for the panel and the World to draw, as it waits for the World alone, since it
     costs a phone's pane about a second of LCP run any sooner (gd-J9y24ir4MV) */
  if (first === 'workbench') { need(first).catch(function () {}); after(['farmdoc', 'workbench:landed']); }
  else if (first) need(first).then(rest, rest);
  else after(['farmdoc']);

  function beadHref(id) { return base + '/garden?bead=' + encodeURIComponent(id); }
  var bead = window.BEAD = {
    open: function (id, how) {
      need('workbench').then(function () { if (window.BEAD !== bead) window.BEAD.open(id, how); else location.href = beadHref(id); },
        function () { location.href = beadHref(id); });
    },
    close: function () {},
    current: function () { return ''; }
  };
  var chat = window.CHAT = {
    open: function (id, o) {
      need('workbench').then(function () { if (window.CHAT !== chat) window.CHAT.open(id, o); else if (o && o.none) o.none(); },
        function () { if (o && o.none) o.none(); else location.href = beadHref(id); });
    }
  };

  /* garden:open-bead (the Porch chat, the tour), which workbench.js takes once it runs */
  document.addEventListener('garden:open-bead', function (e) {
    var id = e.detail && e.detail.id;
    if (!id || loaded('workbench')) return;
    e.preventDefault();
    window.BEAD.open(id);
  });
  /* a plain click on an in-garden panel link (workbench.js follow()) */
  document.addEventListener('click', function (e) {
    if (loaded('workbench') || e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    var a = e.target && e.target.closest ? e.target.closest('a[href]') : null;
    if (!a || a.target === '_blank' || !panelLink(base, a.href, location.origin)) return;
    e.preventDefault();
    need('workbench').then(function () { if (!WORKBENCH.follow(a)) location.href = a.href; }, function () { location.href = a.href; });
  });
  /* a Porch row (captured, as chat.js's) and the buttons the rest wires: clicked again once its bundle has run */
  var LATER = [['#d-porch > li', 'workbench', true], ['#chat-open', 'workbench'], ['#cam-settings', 'panels'], ['#cam-wall', 'wall']];
  document.addEventListener('click', function (e) {
    if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || !e.target || !e.target.closest) return;
    for (var i = 0; i < LATER.length; i++) {
      var hit = e.target.closest(LATER[i][0]), name = LATER[i][1];
      if (!hit || loaded(name)) continue;
      if (LATER[i][2]) { var other = e.target.closest('a,button'); if (other && !other.classList.contains('t')) return; }   // a row's own buttons are theirs
      e.preventDefault(); e.stopPropagation();
      need(name).then(function () { hit.click(); }, function () {});
      return;
    }
  }, true);
  /* a finger's tap on a gardener drawn with no link (web/wall.js pins its card): the gardener under it is taken now, before
     garden.js moves the camera, and its card pinned once the Glass has run; a later tap with no gardener drops it */
  var board = document.getElementById('board'), tapped = null;
  if (board && B.dataset.glass === '1') board.addEventListener('click', function (e) {
    if (loaded('wall') || !window.GARDEN || !window.WORLD || !matchMedia('(pointer: coarse)').matches || e.target.closest('a.hot.w-gardener')) return;
    if (e.target !== board && !e.target.closest('#cv,#hots,#ghots')) { tapped = null; return; }   // a control over the map, as web/wall.js reads it
    var G = GARDEN.cam(), r = board.getBoundingClientRect(), w = WORLD.toWorld(G.cam, G.vp, e.clientX - r.left, e.clientY - r.top);
    var h = tapped = G.ghots.filter(function (x) { return x.kind === 'gardener' && WORLD.hit(x, { x: w.x, y: w.y, w: 0, h: 0 }); })[0] || null;
    if (h) need('wall').then(function () { if (tapped === h && window.WALL && WALL.pin) WALL.pin(h); }, function () {});
  }, true);
  /* Cmd/Ctrl-K with no Workbench yet: the '/' jump, as web/search.js does then */
  document.addEventListener('keydown', function (e) {
    if (loaded('workbench') || e.defaultPrevented || !(e.metaKey || e.ctrlKey) || e.altKey || e.shiftKey || String(e.key || '').toLowerCase() !== 'k') return;
    e.preventDefault();
    var jump = document.getElementById('jump'), jq = document.getElementById('jump-q');
    if (jump && !jump.hidden && jq) jq.focus();
    else if (window.GARDEN && GARDEN.find) GARDEN.find('');
  }, true);
  /* the Glass on the World (body data-glass): the Menu's Wall (LATER: its tap) and a figure's card on hover or focus; its
     event again once it has run. */
  function again(n, ev) { need(n).then(ev, function () {}); }
  var ok = function (n) { return glass && !loaded(n || 'glass'); }, wall = document.getElementById('cam-wall');
  if (wall && glass) wall.hidden = false;
  if (board) ['pointerover', 'focusin'].forEach(function (n) {
    board.addEventListener(n, function (e) {
      var f = ok('wall') && e.target.closest && e.target.closest('a.hot.w-gardener');
      if (f && (n === 'focusin' || e.pointerType === 'mouse')) again('wall', function () {
        if (n === 'focusin' ? document.activeElement === f : f.matches(':hover')) f.dispatchEvent(new (n === 'focusin' ? FocusEvent : PointerEvent)(n, { bubbles: true, pointerType: 'mouse' }));
      });
    });
  });
  window.addEventListener('popstate', function () {
    var n = needOf(location.search);
    if (n && !loaded(n)) need(n).catch(function () {});
  });
  return api;
})();
if (typeof module !== 'undefined') module.exports = LAZY;
