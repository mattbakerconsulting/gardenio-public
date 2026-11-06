/* The Workbench (workbench-glass spec 2.5, gd-lZbcsMQhRh): one panel inside the map window that shows one thing at a
   time, of a kind; the bead view is its first kind. Its address is /garden?<kind>=<id> (KINDS); one panel is open at a
   time, and a reload, a new tab, a Slack link and a Commons card all land on it. A kind is shown once a renderer registers
   for it (WORKBENCH.register), or web/lazy.js says the bundle that will is coming (the Glass), whose open waits for it. On a
   desktop the panel stands right of the world; on a phone it is a sheet from the header to the tab bar (garden.css .bv).

   The host owns what every kind shares: the address, history and Back, the header and close button, Escape, a phone's
   edge swipe, the fetch of the kind's JSON and its error lines, and the plain click on an in-garden link. On a phone a
   panel opened from the Commons sheet returns to the sheet as it was; one opened from the
   Mailbox or the Porch returns to that screen. A renderer paints the header and body from what the page already knows,
   then the kind's document. Text goes in through textContent only.

   The bead renderer (BEAD, below): what the page already knows paints
   at once (window.GARDEN.known); the rest (description, fields, parents and children, comments) comes from
   GET /garden/bead/<id> (bead.go). A cross-reference opens another bead here, never the viewer.

   Live (WB-14; step 5.6, gd-uRytFwHkLT): an open panel holds one stream, the kind's live(id) (live.go), which says the
   thing (or a part: part(), merge()) moved; the host refetches and repaints in place, keeping scroll, folds and focus.
   While the renderer holds the viewer's work (hold()) the repaint waits and stale(id) says so. A stream back after a drop
   refetches; a refused one, or no EventSource, falls back to a fetch a minute (Commons D8). A hidden tab lets it go.

   Degraded (WB-15): a failed load says what failed in words (failure(), never the browser's own "Failed to fetch") and
   paints what the page already knows; one that may pass on a second try offers Try again. A failed refresh keeps the
   document shown and says it could not refresh (failed(id, text)). */
var WORKBENCH = (function () {
  /* ---------- pure logic (also run under node by the Go tests) ---------- */
  /* The panel kinds, in the spec's order (W-D5), the Costs panel (plan gd-gh5J0vVbJP step 2.1: ?costs=fleet|<project>),
     the credential pool (GL-15, step 6.9: ?creds=pool), then the viewer's own settings (WB-13, step 5.12: ?settings=me)
     the picture viewer (WB-18, step 5.15: ?pic=<path>, web/pictures.js), a gardener's transcript (?transcript=<id>, web/transcript.js) and a release's changelog (?release=<id>, web/release.js). */
  var KINDS = ['bead', 'plan', 'project', 'queue', 'q', 'bundle', 'gardener', 'glass', 'costs', 'creds', 'settings', 'pic', 'transcript', 'release'];
  function dec(s) { try { return decodeURIComponent(String(s).replace(/\+/g, ' ')); } catch (e) { return String(s); } }
  function keyOf(p) { var eq = p.indexOf('='); return dec(eq < 0 ? p : p.slice(0, eq)); }
  /* The decoded value of key in a query string, or null. */
  function param(search, key) {
    var parts = String(search || '').replace(/^\?/, '').split('&');
    for (var i = 0; i < parts.length; i++) {
      if (!parts[i]) continue;
      var eq = parts[i].indexOf('=');
      if (keyOf(parts[i]) === key) return eq < 0 ? '' : dec(parts[i].slice(eq + 1));
    }
    return null;
  }
  /* The query with the panel set to kind=id: every panel parameter dropped (one panel at a time), then kind=id added at the
     end, none when id is empty. Every other parameter (at=, z=, ...) is kept as written. */
  function withPanel(search, kind, id) {
    var keep = String(search || '').replace(/^\?/, '').split('&').filter(function (p) { return p && KINDS.indexOf(keyOf(p)) < 0; });
    if (kind && id) keep.push(encodeURIComponent(kind) + '=' + encodeURIComponent(id));
    return keep.length ? '?' + keep.join('&') : '';
  }
  /* A kind whose empty id still names a panel: ?glass= with no agent is the Wall (Lands GL-5), its own id 'wall'. */
  var EMPTY = { glass: 'wall' };
  /* The panel a query names, {kind, id}, or null: the first parameter of one of kinds (default every kind) decides, and an
     empty id names none (EMPTY's own panel for its kind). */
  function panelOf(search, kinds) {
    kinds = kinds || KINDS;
    var parts = String(search || '').replace(/^\?/, '').split('&');
    for (var i = 0; i < parts.length; i++) {
      if (!parts[i]) continue;
      var k = keyOf(parts[i]);
      if (kinds.indexOf(k) < 0) continue;
      var eq = parts[i].indexOf('='), id = (eq < 0 ? '' : dec(parts[i].slice(eq + 1)).trim()) || EMPTY[k] || '';
      return id ? { kind: k, id: id } : null;
    }
    return null;
  }
  /* The kind a viewer gets for kind=id: below the operator tier (glass false) there is no Glass, so one agent's Glass is its
     transcript instead, wherever it was asked for (a figure, a link, an address); the Wall stays the Wall (gd-GPu096omMw). */
  function tierKind(kind, id, glass) { return kind === 'glass' && !glass && id && id !== EMPTY.glass ? 'transcript' : kind; }
  /* The panel an in-garden link opens (base/garden?<kind>=<id> on this origin), or null. */
  function linkPanel(base, href, origin, kinds) {
    var u;
    try { u = new URL(href, origin + '/'); } catch (e) { return null; }
    if (u.origin !== origin || u.pathname !== base + '/garden') return null;
    return panelOf(u.search, kinds);
  }

  /* The line a failed load says (WB-15), {text, retry}: what failed, in words; retry when a second try may pass (no answer,
     a daemon error), not for a session that ended or a thing that is not there. status 0 is no answer at all. By noun,
     not id (gd-dv5wiNnKuh). */
  function failure(status, noun, id) {
    var cl = 'Could not load ' + (/s$/.test(noun) ? 'the ' : 'this ') + noun;
    if (status === 401 || status === 'redirect') return { text: 'Your session ended. Reload the page to sign in again.', retry: false };
    if (status === 404) return { text: 'There is no ' + noun + ' ' + id + ', or you cannot see it.', retry: false };
    if (!status) return { text: cl + ': the garden did not answer.', retry: true };
    if (status === 502 || status === 503 || status === 504) return { text: cl + ': the beads daemon did not answer (' + status + ').', retry: true };
    return { text: cl + ' (' + status + ').', retry: true };
  }
  /* "Could not load <what>" said as "Could not refresh this <noun>", or as words. */
  function refreshFailure(text, noun, words) { return String(text).replace(/^Could not load [^:]*?(?=: | \(|\.$)/, words || 'Could not refresh this ' + noun); }
  /* Where a repaint puts the focus back: the focused control's tag, its label (aria-label, else its text) and its href,
     matched against the repainted panel's controls; '' when the focus is not on one. */
  function placeKey(tag, label, href) { return tag ? String(tag).toLowerCase() + '|' + String(label || '').trim() + '|' + (href || '') : ''; }

  var R = {}, RI = {};   // kind -> renderer; kind=id -> the renderer of that one panel of its kind (the Wall, glass=wall)
  function rOf(kind, id) { return RI[kind + '=' + id] || (EMPTY[kind] === id ? null : R[kind]); }   // EMPTY's id is its own renderer's only
  function later(kind, id) { return typeof LAZY !== 'undefined' && LAZY.later ? LAZY.later(kind, id) : ''; }
  function shows(kind) { return !!R[kind] || Object.keys(RI).some(function (k) { return k.indexOf(kind + '=') === 0; }) || !!later(kind); }
  function kinds() { return KINDS.filter(shows); }
  var api = { KINDS: KINDS, param: param, withPanel: withPanel, panelOf: panelOf, tierKind: tierKind, linkPanel: linkPanel, failure: failure, refreshFailure: refreshFailure, placeKey: placeKey,
    register: function (kind, r, id) { if (id) RI[kind + '=' + id] = r; else R[kind] = r; }, has: function (kind) { return shows(kind); }, open: function () {}, close: function () {}, current: function () { return null; },
    from: function () { return ''; }, reload: function () {}, refresh: function () {} };
  if (typeof document === 'undefined') return api;

  /* ---------- DOM ---------- */
  var B = document.body, panel = document.getElementById('bead');
  if (!panel || typeof fetch === 'undefined') return api;
  var base = B.dataset.base || '', titleEl = document.getElementById('bv-title'), bodyEl = document.getElementById('bv-body');
  var xBtn = document.getElementById('bv-x');
  var cur = null, seq = 0, opener = null, from = '', opened = false;   // cur: the panel shown, {kind, id}; opened: one has been
  var glassOn = B.dataset.glass === '1';   // the viewer may watch (operator tier): else a Glass asked for is the transcript
  /* The kinds an address or a link may name: those shown, and the Glass for a viewer who gets its transcript instead. */
  function named() { var k = kinds(); return glassOn || k.indexOf('glass') >= 0 ? k : k.concat('glass'); }
  /* The panel the address names for this viewer; moved: it named the Glass, so the address is rewritten to what opens. */
  function addressed() { var p = panelOf(location.search, named()), k = p && tierKind(p.kind, p.id, glassOn); return p && { kind: k, id: p.id, moved: k !== p.kind }; }
  var loading = false, rerun = false, hasDoc = false;   // a fetch of the shown panel in flight; a move came meanwhile; it shows its document

  function G() { return window.GARDEN || null; }
  function focus(e) { if (!e || !e.focus) return; try { e.focus({ preventScroll: true }); } catch (x) { e.focus(); } }
  function closeLabel(kind, id) { return 'Close the ' + (rOf(kind, id).noun || kind) + ' view'; }
  function is(kind, id) { return !!cur && cur.kind === kind && cur.id === id; }

  function setURL(kind, id, push, clear) {
    if (!window.history || !history.replaceState) return;
    var url = location.pathname + withPanel(location.search, kind, id) + location.hash;
    if (url === location.pathname + location.search + location.hash) return;
    var st = history.state;
    try { if (push && history.pushState) history.pushState(null, '', url); else history.replaceState(clear ? null : st, '', url); } catch (e) { /* a sandboxed frame: the panel still works */ }
  }
  /* A phone's deep link to the page's first panel lands with the Farm beneath it (phone design 3.6, M1.4 gd-4Y2qOgfmJE): its
     entry is marked wbDeep over the address without it, so Back or the close goes to the map; a reload keeps the mark. */
  function beneath() {
    if (opened || !window.history || !history.pushState || history.state || location.pathname !== base + '/garden' || window.innerWidth >= 900) return;
    try { var here = location.pathname + location.search + location.hash; history.replaceState(null, '', location.pathname + withPanel(location.search, '', '') + location.hash); history.pushState({ wbDeep: 1 }, '', here); } catch (e) { /* sandboxed */ }
  }

  /* Open the panel on kind=id. how.fromURL: the address already says so (a reload, Back); how.replace: do not add a history
     entry; how.opener: the element close hands focus back to (the farm's cursor link when the view follows the cursor,
     garden.js viewFollow), else the element focused when the panel first opened. */
  function open(kind, id, how) {
    how = how || {};
    id = String(id || '').trim();
    var k = tierKind(kind, id, glassOn);
    if (k !== kind) { kind = k; how = { opener: how.opener, replace: !!how.fromURL || how.replace }; }   // an address that said glass= now says transcript=
    var r = rOf(kind, id);
    if (id && !r && later(kind, id)) { LAZY.need(later(kind, id)).then(function () { if (rOf(kind, id) && !is(kind, id)) open(kind, id, how); }, function () {}); return; }
    if (!id || !r) return;
    var g = G(), again = is(kind, id); opened = true;
    if (!cur) opener = document.activeElement;
    if (how.opener) opener = how.opener;
    if (!how.fromURL) setURL(kind, id, !again && !how.replace);   // before toFarm, so the entry it leaves keeps its #mail
    /* A phone's Mailbox or Porch screen hides the map the panel sits in: show the farm, and remember the screen to go back
       to on close (a cross-reference opened from the farm keeps the first one). */
    var was = g ? g.toFarm() || '' : '';
    if (was || !cur) from = was;
    xBtn.setAttribute('aria-label', closeLabel(kind, id));
    var prev = cur && rOf(cur.kind, cur.id);
    if (prev && prev !== r && prev.closed) prev.closed();   // one panel at a time: the last kind lets go
    cur = { kind: kind, id: id };
    panel.dataset.kind = kind;
    panel.hidden = false;
    rerun = false; hasDoc = false;
    if (r.local) r.paint(id, r.local(id), ''); else r.paint(id, null, 'Loading…');   // a local kind paints what the page has, no fetch
    if (r.shown) r.shown(id);
    focus((r.focus && r.focus(id)) || titleEl);
    load(kind, id);
    if (r.local) drawn(kind, id);
    live(); if (g && g.tabs) g.tabs();   // a phone's tab bar marks the Workbench or the Glass
  }
  /* workbench:landed: the panel opened on kind=id has drawn its document, or why it has none (web/lazy.js holds the Commons
     for it on a deep link, gd-J9y24ir4MV) */
  function drawn(kind, id) {
    try { document.dispatchEvent(new CustomEvent('workbench:landed', { detail: { kind: kind, id: id } })); } catch (e) { /* no CustomEvent */ }
  }
  /* Fetch the shown panel's document and paint it. quiet: a refresh of what is shown (a move, Show the latest), which keeps
     the panel's place and, on a failure, the document it shows (the renderer's failed() says so). */
  function load(kind, id, quiet, ev) {
    var my = ++seq, r = rOf(kind, id), noun = r.noun || kind, part = ev && hasDoc && r.part ? r.part(id, ev) : '';
    if (r.local) return;
    loading = true;
    var fail = function (f) { var e = new Error(f.text); e.retry = f.retry; throw e; };
    var why = function (status) { return (r.failure && r.failure(status, id)) || failure(status, noun, id); };
    fetch(base + (part || r.url(id)), { credentials: 'same-origin', redirect: 'manual', cache: 'no-cache', headers: { Accept: 'application/json' } })   // unchanged: a 304
      .then(function (res) {
        if (res.ok) return res.json();
        fail(why(res.type === 'opaqueredirect' ? 'redirect' : res.status));
      }, function () { fail(why(0)); })
      .then(function (d) {
        if (my !== seq) return; if (part && !(d = r.merge(id, d))) { load(kind, id, true); return; }
        if (r.done) r.done(id, d);
        if (quiet) keepPlace(function () { r.paint(id, d, ''); }); else r.paint(id, d, '');
        if (typeof PICTURES !== 'undefined') PICTURES.wire(bodyEl);   // its pictures preview on a hover and open the viewer (web/pictures.js)
        hasDoc = true; failed = '';
        if (r.landed) r.landed(id, d);
        if (!quiet) drawn(kind, id);
        settled();
      }, function (err) {
        if (my !== seq) return;
        var text = err.message || 'Could not load this ' + noun + '.';
        failed = err.retry ? 'retry' : 'final';
        if (quiet && hasDoc && r.failed) r.failed(id, text, !!err.retry);   // keep the document shown, and say so
        else {
          if (r.done) r.done(id, null);
          hasDoc = false;
          if (quiet) keepPlace(function () { r.paint(id, null, text, true, !!err.retry); }); else r.paint(id, null, text, true, !!err.retry);
        }
        if (!quiet) drawn(kind, id);
        settled();
      });
  }
  /* The shown panel's fetch finished: one that a move overtook goes once more. */
  function settled() {
    loading = false;
    if (rerun) { rerun = false; if (cur) moved(cur.kind, cur.id); }
  }
  /* Repaint in place: the body's scroll, its open folds (by their summary) and the focused control come back. */
  function placeOf(e) { return placeKey(e.tagName, e.getAttribute('aria-label') || e.textContent, e.getAttribute('href')); }
  function keepPlace(paint) {
    var top = bodyEl.scrollTop, a = document.activeElement, had = !!a && a !== bodyEl && bodyEl.contains(a), key = had ? placeOf(a) : '';
    var opens = Array.prototype.map.call(bodyEl.querySelectorAll('details[open] > summary'), function (x) { return x.textContent; });
    paint();
    Array.prototype.forEach.call(bodyEl.querySelectorAll('details > summary'), function (x) { if (opens.indexOf(x.textContent) >= 0) x.parentNode.open = true; });
    bodyEl.scrollTop = top;
    if (!had) return;
    var back = Array.prototype.filter.call(bodyEl.querySelectorAll(a.tagName), function (x) { return placeOf(x) === key; })[0];
    focus(back || titleEl);
  }

  /* ---------- live (WB-14) ---------- */
  var es = null, esKey = '', esDropped = false, floor = null, failed = '';   // failed: the last fetch failed, 'retry' or 'final'
  var FLOOR_MS = 60000;   // the poll when there is no stream
  function unlive() {
    if (es) { es.close(); es = null; }
    esKey = ''; esDropped = false;
    if (floor) { clearInterval(floor); floor = null; }
  }
  /* The shown panel's stream, opened once per panel (live() again is a no-op); none for a kind without one, or a hidden tab. */
  function live() {
    var r = cur && rOf(cur.kind, cur.id), url = r && r.live ? r.live(cur.id) : '', key = url ? cur.kind + '=' + cur.id : '';
    if (!key || document.hidden) { unlive(); return; }
    if (esKey === key) return;
    unlive();
    var kind = cur.kind, id = cur.id;
    esKey = key;
    if (typeof EventSource === 'undefined') { poll(kind, id); return; }
    es = LIVE.source(base + url);   // web/live.js
    es.addEventListener('moved', function (e) { moved(kind, id, e.data); });
    es.onopen = function () {
      // back after a drop: what it missed is unknown; a document that landed before the stream opened may predate it
      if (esDropped || !loading) moved(kind, id);
      esDropped = false;
    };
    es.onerror = function () {
      esDropped = true;
      if (es && es.readyState === 2) { es = null; poll(kind, id); }   // refused (gone, not allowed, no daemon): the floor
    };
  }
  function poll(kind, id) {
    if (floor) clearInterval(floor);
    floor = setInterval(function () { if (!document.hidden && failed !== 'final') moved(kind, id); }, FLOOR_MS);   // not one that is not there
  }
  /* kind=id moved: fetch it again, once at a time, unless the renderer holds the viewer's work (then it says so). */
  function moved(kind, id, ev) {
    if (!is(kind, id)) return;
    var r = rOf(kind, id);
    if (r.hold && r.hold()) { if (r.stale) r.stale(id); return; }
    if (loading) { rerun = true; return; }
    load(kind, id, true, ev);
  }
  document.addEventListener('visibilitychange', function () {
    if (!cur) return;
    if (document.hidden) { unlive(); return; }
    live();
    moved(cur.kind, cur.id);   // what moved while hidden is unknown
  });
  function close(how) {
    if (!cur) return;
    var was = cur, st = window.history && history.state, g = G(), back = from;
    cur = null; seq++; from = ''; loading = false; rerun = false; hasDoc = false; failed = '';
    unlive();
    panel.hidden = true; bodyEl.textContent = '';
    if (rOf(was.kind, was.id).closed) rOf(was.kind, was.id).closed();
    if (!(how && how.fromURL)) { if (st && st.wbDeep && !how) history.back(); else setURL('', '', false, !!how); }   // a deep link's panel: Back to the Farm beneath it; how.tab: another tab took the screen
    if (g && back) g.toScreen(back);   // the Mailbox or Porch the panel was opened from, its #mail or #porch back in the address
    var f = opener; opener = null; if (g && g.tabs) g.tabs();   // the tab bar marks the screen again
    if (f && f !== document.body && document.body.contains(f)) focus(f);
    else focus(document.getElementById('board'));
  }

  /* A plain click on an in-garden panel link opens it here. A modified click (new tab or window) keeps the browser's own
     handling, which lands on /garden?<kind>=<id> and opens the same panel there. A drag (garden.js suppresses its click) or
     a first tap that only moved the cursor (garden.js prevents it) is not an open. The renderer may follow the open (the
     bead view zooms the map to the bead). */
  document.addEventListener('click', function (e) {
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    var a = e.target && e.target.closest ? e.target.closest('a[href]') : null;
    if (!a || a.target === '_blank') return;
    follow(a, function () { e.preventDefault(); });
  });
  /* follow(a): open the panel link a names, as its click does; false when it names none. web/lazy.js calls it for a click
     taken before this file ran. */
  function follow(a, taken) {
    var p = linkPanel(base, a.href, location.origin, named());
    if (!p) return false;
    p.kind = tierKind(p.kind, p.id, glassOn);
    if (taken) taken();
    open(p.kind, p.id);
    var r = rOf(p.kind, p.id);
    if (r && r.followed) r.followed(p.id, a);
    return true;
  }
  panel.addEventListener('keydown', function (e) {
    if (e.key === 'Escape' && !e.defaultPrevented) { e.preventDefault(); e.stopPropagation(); close(); }
  });
  xBtn.onclick = function () { close(); };
  window.addEventListener('popstate', function () {
    var p = addressed();
    if (p && !is(p.kind, p.id)) open(p.kind, p.id, p.moved ? { replace: true } : { fromURL: true });
    else if (!p && cur) close({ fromURL: true });
  });

  /* A kind's renderer: {noun (the word in its error and close lines), url(id) (its JSON under base), paint(id, d, note,
     isErr) (the header's kind and title, and the body: d is null while loading and on a failed load), and optionally
     local(id) (a kind that fetches nothing: what paint gets as d), shown(id) (just opened), focus(id) (the element to focus once it is open, else its title), done(id, d) (the load finished, d null on a failure), landed(id, d) (painted with its
     document), closed() (it let go of the panel), deep(id) (opened from the page's own address), followed(id, link)
     (a click on link opened it), and for a live panel (WB-14) live(id) (its stream under
     base, which says "moved"), hold() (a refresh now would lose the viewer's work), stale(id) (it moved while held: say so)
     and failed(id, text, retry) (a refresh failed: say so over the document shown); paint's fifth argument, retry, says a
     failed load may pass on a second try; failure(status, id) (the kind's own words for a failed load, {text, retry}, or
     null for the host's)}. With id, r renders only kind=id (the Wall is glass=wall) and the kind's own renderer every other
     id. Registering opens the page's own address when it names
     this kind (this id) and nothing is open yet, whatever order the renderers load in. */
  api.register = function (kind, r, id) {
    if (KINDS.indexOf(kind) < 0) return;
    if (id) RI[kind + '=' + id] = r; else R[kind] = r;
    var p = cur ? null : addressed();
    if (p && p.kind === kind && rOf(kind, p.id) === r) { if (r.deep) r.deep(p.id); beneath(); open(kind, p.id, p.moved ? { replace: true } : { fromURL: true }); }
  };
  api.open = function (kind, id, how) { open(kind, id, how); };
  api.follow = function (a) { return follow(a); };
  api.close = function (how) { close(how); };   // how.tab: a phone's tab bar closes it for another screen (web/garden.js goPane)
  api.current = function () { return cur ? { kind: cur.kind, id: cur.id } : null; };   // the panel shown, null when closed
  api.from = function () { return from; };   // the screen the panel was opened over: 'mail', 'porch' or ''
  api.reload = function (kind, id) { if (is(kind, id)) load(kind, id); };   // fetch the shown panel's document again
  api.refresh = function (kind, id, d) { if (is(kind, id) && !loading) { if (d && hasDoc) keepPlace(function () { rOf(kind, id).paint(id, d, ''); }); else load(kind, id, true); } };   // ... in place (Show the latest, Try again), or paint d
  api.moved = function (kind, id) { moved(kind, id); };   // ... when a stream not its own says it moved (web/queue.js)
  api.href = function (kind, id) { return base + '/garden' + withPanel('', kind, id); };
  return api;
})();

/* The bead view: the Workbench's bead kind (?bead=<id>), and the BEAD the rest of the page has always called (open, close,
   current, linkId). */
var BEAD = (function () {
  /* ---------- pure logic (also run under node by the Go tests) ---------- */
  var W = WORKBENCH, param = W.param;
  /* The query with bead= set to id (dropped when id is empty): withPanel's, so any other panel's parameter goes too. Every
     other parameter (at=, z=, ...) is kept as written. */
  function withBead(search, id) { return W.withPanel(search, 'bead', id); }
  /* The bead an in-garden link opens (base/garden?bead=<id> on this origin), or ''. */
  function linkId(base, href, origin) { var p = W.linkPanel(base, href, origin, ['bead']); return p ? p.id : ''; }
  /* The relation lists of a bead document, in reading order, non-empty only. shown ({id: true}) is what the view lists above
     already, a plan's phases or a bundle's documents: those children are not listed twice, and what is left reads as Other
     children (gd-cNbP4dtM8E). */
  var RELS = [['parents', 'Part of'], ['children', 'Children'], ['blocked_by', 'Waits on'], ['blocks', 'Holds up'], ['related', 'Related']];
  function sections(d, shown) {
    shown = shown || {};
    return RELS.map(function (r) {
      var all = (d || {})[r[0]] || [], list = r[0] === 'children' ? all.filter(function (b) { return !shown[b.id]; }) : all;
      return { key: r[0], title: list.length < all.length ? 'Other children' : r[1], list: list };
    }).filter(function (s) { return s.list.length > 0; });
  }
  /* The bead ids a view lists before its relations: the plan's phases (renderPlot), a staked plan's steps (renderActs) and the
     face's items (a bundle's documents). */
  function shownIds(plan, fv, seed) {
    var ids = {};
    ((seed || {}).steps || []).forEach(function (st) { ids[st.id] = true; });
    if (plan) phases(plan).forEach(function (p) { if (p.id) ids[p.id] = true; });
    ((fv || {}).items || []).forEach(function (it) { (it.parts || []).forEach(function (pt) { if (pt && pt.bead) ids[pt.bead] = true; }); });
    return ids;
  }
  /* u when it is an http(s) URL, else '': the only links out a bead's data may make. */
  function safeURL(u) { return /^https?:\/\//i.test(u || '') ? u : ''; }
  function mrURL(d) {
    var f = ((d || {}).fields || []).filter(function (x) { return x.k === 'mr_url' || x.k === 'pr_url'; })[0];
    return f ? safeURL(f.v) : '';
  }
  /* The bead's pull request as a button that says what it does ("Open PR #57", a new tab, its arrow garden.css .ext), or null when there is none or
     the face shows it already (a work face's Pull request row, a merge request's Link): never the same link twice. */
  function prButton(d) {
    var u = mrURL(d), f = (d || {}).face, m = f && (f.kind === 'work' && !d.harvest ? (f.work || {}).mr : f.kind === 'mr' ? f.mr : null);
    if (!u || (m && safeURL(m.url))) return null;
    var n = /\/(?:pull|pulls|merge_requests)\/(\d+)(?:[/?#]|$)/.exec(u), what = n ? 'PR #' + n[1] : 'the pull request';
    return { url: u, t: 'Open ' + what, aria: 'Open ' + (n ? 'pull request #' + n[1] : 'the pull request') + ' in a new tab' };
  }
  /* Free text (description, notes, comments) cut into runs: plain text, an http(s) URL, or a bead id. A URL ends at
     whitespace or a quote, and trailing sentence punctuation (and a close bracket it did not open) stays text. A bead id is
     two lowercase letters, a dash and ten letters or digits with at least one capital or digit, so "re-introduced" stays
     a word, and one glued to a '/' is a path's (gd-vsCHW7UqmF). A URL wins over an id inside it. */
  var LINKS = /(https?:\/\/[^\s<>"'`]+)|\b([a-z]{2}-(?=[A-Za-z0-9]{0,9}[A-Z0-9])[A-Za-z0-9]{10})\b/g;
  var OPENS = { ')': '(', ']': '[', '}': '{' };
  function trimURL(u) {
    for (;;) {
      var c = u.charAt(u.length - 1), o = OPENS[c];
      if (/[.,;:!?*]/.test(c) || (o && u.split(o).length <= u.split(c).length - 1)) u = u.slice(0, -1);
      else return u;
    }
  }
  function runs(text) {
    text = String(text == null ? '' : text);
    var out = [], at = 0, m;
    LINKS.lastIndex = 0;
    while ((m = LINKS.exec(text))) {
      var s = m[0], kind = m[1] ? 'url' : 'bead';
      if (kind === 'url') { s = trimURL(s); if (s.length <= 'https://'.length) continue; LINKS.lastIndex = m.index + s.length; }
      else if (text.charAt(m.index - 1) === '/' || text.charAt(m.index + s.length) === '/') continue;
      if (m.index > at) out.push({ k: 'text', s: text.slice(at, m.index) });
      out.push(kind === 'url' ? { k: 'url', s: s, label: urlLabel(s) } : { k: 'bead', s: s });
      at = m.index + s.length;
    }
    if (at < text.length) out.push({ k: 'text', s: text.slice(at) });
    return out;
  }
  /* A URL's short label: its host (www. dropped) and the last two path segments, each capped, the rest elided. The full
     URL stays in the link's title. */
  function cap(s, n) { s = Array.from(s); return s.length > n ? s.slice(0, n - 1).join('') + '…' : s.join(''); }
  function urlLabel(u) {
    var x;
    try { x = new URL(u); } catch (e) { return cap(u, 48); }
    var segs = x.pathname.split('/').filter(Boolean).map(function (p) { try { return cap(decodeURIComponent(p), 24); } catch (e) { return cap(p, 24); } });
    var host = x.hostname.replace(/^www\./, '');
    return host + (segs.length > 2 ? '/…/' + segs.slice(-2).join('/') : segs.length ? '/' + segs.join('/') : '');
  }
  var STATE = { in_progress: 'growing', review: 'in review', blocked: 'blocked', open: 'planted', deferred: 'dormant', closed: 'closed' };
  function stateWord(st) { return STATE[st] || st || ''; }
  /* A plan's phase list for its bead view (cursor-grid spec 5.3, 6.4): every phase in phase_order, then Other when it holds
     anything, numbered as the plot numbers its beds, each {n, id, title, other, done, total, closed, current, crated}. The
     current phase is the first one not closed, the plot's lit stake (WORLD.planBeds' rule). Past nine beds the plot shows
     eight and a crate "+N phases", whose A opens this view: crated marks the ones only listed here. */
  var BEDS = 9;
  function countsTotal(c) { return (c.in_progress || 0) + (c.review || 0) + (c.blocked || 0) + (c.open || 0) + (c.deferred || 0) + (c.closed || 0); }
  function phases(plan) {
    var rows = [], u = (plan || {}).unphased, current = false;
    ((plan || {}).phases || []).forEach(function (ph) { rows.push({ ph: ph, other: false }); });
    if (u && ((u.tasks || []).length || countsTotal(u.counts || {}))) rows.push({ ph: u, other: true });
    return rows.map(function (r, k) {
      var ph = r.ph, c = ph.counts || {}, total = countsTotal(c), done = c.closed || 0, live = (ph.tasks || []).length + (ph.more || 0);
      var closed = !r.other && (ph.status === 'closed' || (total > 0 && done >= total && !live));
      var o = { n: k + 1, id: r.other ? '' : ph.id || '', title: ph.title || (r.other ? 'Other' : ph.id || 'Phase ' + (k + 1)), other: r.other, done: done, total: total, closed: closed, current: false, crated: rows.length > BEDS && k >= BEDS - 1 };
      if (!r.other && !closed && !current) { o.current = true; current = true; }
      return o;
    });
  }
  /* What a phase row says after its title: its steps, then closed or current in words (state is never colour alone). */
  function phaseNote(p) {
    var bits = [p.done + '/' + p.total + ' steps'];
    if (p.closed) bits.push('closed'); else if (p.current) bits.push('current');
    if (p.crated) bits.push('in the crate');
    return bits.join(' · ');
  }

  /* A closed step's harvest (harvest spec 4.2, 6; bead.go's harvest block): the base in a word, then what it proved, each
     badge as its word in the spec's order (crate, rosette, photo, vial, lantern, seal), every one in words because state is
     never colour alone. A count the writers never recorded is absent; a recorded zero lights nothing. */
  var BASES = { sheaf: 'harvested', trampled: 'abandoned' };
  function baseWord(b) { return BASES[b] || 'closed'; }
  function plural(n, one, many) { return n + ' ' + (n === 1 ? one : many); }
  /* The grade the farm and the label show: the newest kind=ux evidence (grades come newest first). */
  function newestGrade(h) { return ((h || {}).grades || []).filter(function (g) { return (!g.kind || g.kind === 'ux') && g.of > 0; })[0] || null; }
  function badges(h) {
    h = h || {};
    var out = [], g = newestGrade(h), mr = h.mr, pics = (g ? (g.urls || []).length : 0) + (h.goldens || 0);
    if (mr && mr.state === 'merged') out.push({ k: 'crate', w: 'merged' + (mr.number ? ' ' + mr.number : '') });
    if (g) out.push({ k: 'rosette', w: 'graded ' + g.score + '/' + g.of + ' gates' });
    if (pics > 0) out.push({ k: 'photo', w: plural(pics, 'picture', 'pictures') });
    if (h.tests > 0) out.push({ k: 'vial', w: plural(h.tests, 'test', 'tests') });
    if (h.ci === 'success') out.push({ k: 'lantern', w: 'checks green' });
    if (h.rounds >= 1) out.push({ k: 'seal', w: h.rounds === 1 ? 'merged first time' : 'merged after ' + h.rounds + ' rounds' });
    return out;
  }
  /* The merge-time signals no lit badge already says, as words: checks that did not go green, a recorded zero tests, and
     the goldens touched (a recorded zero said). An unrecorded one is left out. */
  function signals(h) {
    h = h || {};
    var out = [];
    if (h.ci && h.ci !== 'success') out.push(/^fail/i.test(h.ci) ? 'checks failed' : 'checks ' + h.ci);
    if (h.tests === 0) out.push('no tests touched');
    if (h.goldens !== undefined) out.push(h.goldens ? plural(h.goldens, 'golden', 'goldens') + ' touched' : 'no goldens touched');
    return out;
  }
  function span(sec) {
    if (sec < 3600) return Math.max(1, Math.round(sec / 60)) + 'm';
    if (sec < 86400) return Math.round(sec / 3600) + 'h';
    return Math.round(sec / 86400) + 'd';
  }
  /* Lead time, then cost: "closed in 2d · $0.29 across 2 crews". An unread cost says so; it is never $0. */
  function metrics(h) {
    h = h || {};
    var out = [];
    if (h.lead > 0) out.push('closed in ' + span(h.lead));
    if ((h.partial || []).indexOf('crews') >= 0) out.push('cost not read');
    else if (h.crews > 0) out.push('$' + (h.cost_usd || 0).toFixed(2) + ' across ' + plural(h.crews, 'crew', 'crews'));
    return out;
  }
  /* A grade's picture through the viewer's image proxy (gb image upload prints <viewer>/image/<key>): the /image/ path
     under base, or '' for a link the proxy does not serve. The uploader's raw private-bucket S3 URL is the same key, so it
     goes through the proxy too (gd-tNvYsFiDb8). Only a key under <prefix>/images/<scope>/<file>. */
  var S3_HOST = /^[a-z0-9.-]+\.s3(?:\.[a-z0-9-]+)?\.amazonaws\.com$/;
  function picSrc(base, u) {
    var x;
    try { x = new URL(u); } catch (e) { return ''; }
    if (x.protocol !== 'https:' && x.protocol !== 'http:') return '';
    var i = x.pathname.indexOf('/image/');
    if (i < 0 && S3_HOST.test(x.hostname) && x.pathname.indexOf('/images/') > 0) return (base || '') + '/image' + x.pathname;
    return i < 0 || x.pathname.length <= i + 7 ? '' : (base || '') + x.pathname.slice(i);
  }

  /* A report's markdown (format=markdown), read into blocks the panel builds node by node (task 3.7, gd-GJ3xhSQsdh): never
     HTML, so raw HTML stays text. The CommonMark an agent writes, headings to autolinks. A block is {t: 'h', n, c}, {t: 'p', c},
     {t: 'code', s}, {t: 'quote', c: blocks}, {t: 'ul' | 'ol', start, items: [blocks]}, {t: 'table', align, head, rows},
     {t: 'hr'}; inline c is a list of {k: 'text', s}, {k: 'b' | 'i' | 'del', c}, {k: 'code', s}, {k: 'a', href, c},
     {k: 'img', src, alt, title}, {k: 'br'}. */
  var MD_FENCE = /^ {0,3}(`{3,}|~{3,})/, MD_H = /^ {0,3}(#{1,6})(?:[ \t]+(.*?))?(?:[ \t]+#+)?[ \t]*$/, MD_HR = /^ {0,3}([-*_])(?:[ \t]*\1){2,}[ \t]*$/;
  var MD_SETEXT = /^ {0,3}(=+|-+)[ \t]*$/, MD_QUOTE = /^ {0,3}>/, MD_LI = /^( *)([-*+]|\d{1,9}[.)])([ \t]+|$)(.*)$/, MD_DELIM = /^ *\|? *:?-+:? *(\| *:?-+:? *)*\|? *$/;
  function blank(l) { return /^[ \t]*$/.test(l); }
  function cells(l) {
    var s = l.trim().replace(/^\|/, '').replace(/(^|[^\\])\|$/, '$1'), out = [], cur = '', code = false;
    for (var i = 0; i < s.length; i++) {
      var c = s.charAt(i);
      if (c === '\\' && s.charAt(i + 1) === '|') { cur += '|'; i++; continue; }
      if (c === '`') code = !code;
      if (c === '|' && !code) { out.push(cur.trim()); cur = ''; continue; }
      cur += c;
    }
    out.push(cur.trim());
    return out;
  }
  function tableAt(lines, i) { return i + 1 < lines.length && lines[i].indexOf('|') >= 0 && MD_DELIM.test(lines[i + 1]) && lines[i + 1].indexOf('-') >= 0 && cells(lines[i]).length === cells(lines[i + 1]).length; }
  /* Does line i start a block that ends a paragraph? (A setext underline is the paragraph's own.) */
  function interrupts(lines, i) {
    var l = lines[i], m = MD_LI.exec(l);
    return MD_FENCE.test(l) || MD_H.test(l) || MD_HR.test(l) || MD_QUOTE.test(l) || tableAt(lines, i) || !!(m && m[1].length < 4 && m[4].trim() && (!/\d/.test(m[2]) || /^1\D/.test(m[2])));
  }
  function mdBlocks(lines) {
    var out = [], i = 0, m;
    while (i < lines.length) {
      var l = lines[i];
      if (blank(l)) { i++; continue; }
      if ((m = MD_FENCE.exec(l))) {
        var fence = m[1], body = [], ind = l.length - l.replace(/^ +/, '').length;
        for (i++; i < lines.length && !new RegExp('^ {0,3}' + fence.charAt(0) + '{' + fence.length + ',}[ \\t]*$').test(lines[i]); i++) body.push(lines[i].replace(new RegExp('^ {0,' + ind + '}'), ''));
        i++;
        out.push({ t: 'code', s: body.join('\n') });
        continue;
      }
      if ((m = MD_H.exec(l))) { out.push({ t: 'h', n: m[1].length, c: mdInline(m[2] || '') }); i++; continue; }
      if (MD_HR.test(l)) { out.push({ t: 'hr' }); i++; continue; }
      if (MD_QUOTE.test(l)) {
        var q = [];
        for (; i < lines.length && !blank(lines[i]) && (MD_QUOTE.test(lines[i]) || !interrupts(lines, i)); i++) q.push(lines[i].replace(/^ {0,3}> ?/, ''));
        out.push({ t: 'quote', c: mdBlocks(q) });
        continue;
      }
      if ((m = MD_LI.exec(l)) && m[1].length < 4) { i = mdList(lines, i, out); continue; }
      if (tableAt(lines, i)) {
        var align = cells(lines[i + 1]).map(function (c) { return /^:-+:$/.test(c) ? 'center' : /-:$/.test(c) ? 'right' : /^:/.test(c) ? 'left' : ''; });
        var head = cells(lines[i]).map(mdInline), rows = [];
        for (i += 2; i < lines.length && !blank(lines[i]) && lines[i].indexOf('|') >= 0; i++) {
          var r = cells(lines[i]).slice(0, head.length);
          while (r.length < head.length) r.push('');
          rows.push(r.map(mdInline));
        }
        out.push({ t: 'table', align: align, head: head, rows: rows });
        continue;
      }
      var para = [l.replace(/^ +/, '')];
      for (i++; i < lines.length && !blank(lines[i]) && !interrupts(lines, i) && !MD_SETEXT.test(lines[i]); i++) para.push(lines[i].replace(/^ +/, ''));
      if (i < lines.length && MD_SETEXT.test(lines[i]) && !blank(lines[i])) { out.push({ t: 'h', n: lines[i].trim().charAt(0) === '=' ? 1 : 2, c: mdInline(para.join('\n')) }); i++; continue; }
      out.push({ t: 'p', c: mdInline(para.join('\n')) });
    }
    return out;
  }
  /* A list from line i into out; returns the line after it. An item holds the lines indented past its marker (or two
     spaces past the marker's own indent, how a hand-nested list is often typed), and a lazy line that carries its
     paragraph on. A marker of the other kind starts another list. */
  function mdList(lines, i, out) {
    var m = MD_LI.exec(lines[i]), ind = m[1].length, ordered = /\d/.test(m[2]), list = { t: ordered ? 'ol' : 'ul', start: ordered ? parseInt(m[2], 10) : 1, items: [] };
    while (i < lines.length) {
      m = MD_LI.exec(lines[i]);
      if (!m || m[1].length !== ind || /\d/.test(m[2]) !== ordered) break;
      var col = Math.min(m[1].length + m[2].length + Math.max(1, Math.min(4, m[3].length)), ind + 2), body = [m[4]], gap = false;
      for (i++; i < lines.length; i++) {
        var l = lines[i], lead = l.length - l.replace(/^ +/, '').length;
        if (blank(l)) { gap = true; body.push(''); continue; }
        if (lead >= col) { body.push(l.slice(col)); gap = false; continue; }
        if (!gap && !interrupts(lines, i) && !MD_LI.test(l)) { body.push(l.trim()); continue; }
        break;
      }
      while (body.length && blank(body[body.length - 1])) body.pop();
      list.items.push(mdBlocks(body));
      if (gap && i < lines.length && !MD_LI.test(lines[i])) break;
    }
    out.push(list);
    return i;
  }
  var MD_PUNCT = /[!-\/:-@\[-`{-~]/, mdHard = false;   // mdHard: markdown(text, true) reads every newline in a paragraph as a break
  /* The inline runs of a paragraph's text. */
  function mdInline(s) {
    var out = [], text = '', i = 0, m;
    function flush() { if (text) { out.push({ k: 'text', s: text }); text = ''; } }
    function push(n) { flush(); out.push(n); }
    while (i < s.length) {
      var c = s.charAt(i), rest = s.slice(i);
      if (c === '\\' && i + 1 < s.length && MD_PUNCT.test(s.charAt(i + 1))) { text += s.charAt(i + 1); i += 2; continue; }
      if (c === '\\' && s.charAt(i + 1) === '\n') { push({ k: 'br' }); i += 2; continue; }
      if (c === '\n') { if (mdHard || / {2,}$/.test(text)) { text = text.replace(/ +$/, ''); push({ k: 'br' }); } else { text = text.replace(/ +$/, '') + ' '; } i++; while (s.charAt(i) === ' ') i++; continue; }
      if (c === '`') {
        var run = /^`+/.exec(rest)[0], end = s.indexOf(run, i + run.length);
        while (end >= 0 && s.charAt(end + run.length) === '`') end = s.indexOf(run, end + run.length + 1);
        if (end < 0) { text += run; i += run.length; continue; }
        var code = s.slice(i + run.length, end).replace(/\n/g, ' ');
        if (/^ .*[^ ].* $/.test(code)) code = code.slice(1, -1);
        push({ k: 'code', s: code }); i = end + run.length; continue;
      }
      if ((c === '!' && s.charAt(i + 1) === '[') || c === '[') {
        var at = c === '!' ? i + 1 : i, lk = mdLink(s, at);
        if (lk) {
          push(c === '!' ? { k: 'img', src: lk.href, alt: mdPlain(mdInline(lk.label)), title: lk.title } : { k: 'a', href: lk.href, c: mdInline(lk.label).filter(function (n) { return n.k !== 'a'; }) });
          i = lk.end; continue;
        }
      }
      if (c === '<' && (m = /^<((?:https?:\/\/|mailto:)[^\s<>]+)>/.exec(rest))) { push({ k: 'a', href: m[1], c: [{ k: 'text', s: m[1].replace(/^mailto:/, '') }] }); i += m[0].length; continue; }
      if ((c === '*' || c === '_' || c === '~') && (m = mdEmph(s, i))) { push({ k: m.k, c: mdInline(m.inner) }); i = m.end; continue; }
      text += c; i++;
    }
    flush();
    return out;
  }
  /* A link or image's [label](href "title") at s[at] === '[': {label, href, title, end}, or null when it is not one. */
  function mdLink(s, at) {
    var depth = 0, j = at;
    for (; j < s.length; j++) {
      var c = s.charAt(j);
      if (c === '\\') { j++; continue; }
      if (c === '[') depth++;
      else if (c === ']' && --depth === 0) break;
    }
    if (j >= s.length || s.charAt(j + 1) !== '(') return null;
    var m = /^\(\s*(<[^<>\n]*>|[^\s()]*(?:\([^\s()]*\)[^\s()]*)*)(?:\s+("[^"]*"|'[^']*'|\([^)]*\)))?\s*\)/.exec(s.slice(j + 1));
    if (!m) return null;
    return { label: s.slice(at + 1, j), href: m[1].replace(/^<|>$/g, ''), title: m[2] ? m[2].slice(1, -1) : '', end: j + 1 + m[0].length };
  }
  /* Emphasis at s[i]: ** or __ strong, * or _ emphasis, ~~ strikethrough; closed by the same run, never by one that follows a
     space, and an underscore never inside a word (snake_case stays text). */
  function mdEmph(s, i) {
    var mm = /^(\*\*|__|~~|\*|_)/.exec(s.slice(i)), run = mm ? mm[0] : '', ch = run.charAt(0), word = /[A-Za-z0-9]/;
    if (!run) return null;
    if (ch === '_' && word.test(s.charAt(i - 1))) return null;
    if (!s.charAt(i + run.length) || /\s/.test(s.charAt(i + run.length))) return null;
    for (var j = i + run.length + 1; j <= s.length - run.length; j++) {
      if (s.charAt(j - 1) === '\\') continue;
      if (s.slice(j, j + run.length) !== run || /\s/.test(s.charAt(j - 1))) continue;
      if (run.length === 1 && s.charAt(j + 1) === ch) { j++; continue; }   // part of a closing ** inside: keep looking
      if (ch === '_' && word.test(s.charAt(j + run.length))) continue;
      return { k: ch === '~' ? 'del' : run.length === 2 ? 'b' : 'i', inner: s.slice(i + run.length, j), end: j + run.length };
    }
    return null;
  }
  function mdPlain(c) { return (c || []).map(function (n) { return n.k === 'text' || n.k === 'code' ? n.s : n.k === 'br' ? ' ' : n.k === 'img' ? n.alt : mdPlain(n.c); }).join(''); }
  function markdown(text, hard) { mdHard = !!hard; try { return mdBlocks(String(text == null ? '' : text).replace(/\r\n?/g, '\n').replace(/\t/g, '    ').split('\n')); } finally { mdHard = false; } }
  /* An href a report's link may carry: the web and mail, and a path on this site; anything else (javascript:, data:) is not
     a link. */
  function safeHref(u) { return /^(https?:\/\/|mailto:)/i.test(u) || /^\/(?!\/)/.test(u) || /^#/.test(u) ? u : ''; }
  /* The proxied src of a report picture: a /image/ URL, absolute or on this site (root-relative), through picSrc. */
  function mdPicSrc(base, u) { return picSrc(base, /^\/(?!\/)/.test(u) ? 'https://site.invalid' + u : u); }

  /* Typed faces (Lands workbench-glass spec WB-1, step 5.3): the document's face block (bead_face.go), keyed by kind, read
     into one view per kind: the section's heading, a state line, a lead paragraph, rows of a label and its parts, a list,
     long texts, links out and, for an open decision or escalation, the answer box web/answer.js drives. A part is a
     string, {bead: id, t: title} (opens here), {url: u, t: label} (a new tab), {go: href, t: label} (another panel here),
     {b: text} or {sm: text}. A kind with no renderer, and a bead with no face, keep the generic fields table below.
     ctx: {id, title, now (unix seconds), base, has(kind) (a Workbench kind is shown, so a link can open its panel), mine (the ask
     is in the viewer's Mailbox, so it waits on them), mapped (the plan is on the map, its phases listed above), costs (the
     viewer may read the Grafana charts, body data-costs)}. */
  function words(s) { return String(s == null ? '' : s).replace(/[_]+/g, ' ').trim(); }
  function ago2(now, t) { return t > 0 ? span(Math.max(60, now - t)) + ' ago' : ''; }
  /* A bead the face names, as its title; one the daemon did not return (gone, or not visible to this viewer) says so and
     still links, its id in the tooltip. */
  function ref(r) { return r && r.id ? { bead: r.id, t: r.title || 'a bead not visible to you' } : null; }
  /* A thread, as a link to its head's panel here, named by its short title (gd-zn6iUnPd9Q). */
  function threadRef(r) { return r && r.id ? { bead: r.id, t: r.short_title || r.title || 'a thread not visible to you' } : null; }
  /* A link out, {url, t}, only to an http(s) URL; any other scheme (javascript:, data:) is the label as plain text, so bead
     data never puts a script link in the /garden origin (gd-fubODCnFyd). */
  function linkOut(u, t) { return safeURL(u) ? { url: u, t: t } : t; }
  function joined(list) { var out = []; list.filter(Boolean).forEach(function (p, i) { if (i) out.push(' · '); out.push(p); }); return out; }
  /* Who asked and for what: the agent (a link when it is a bead), then "for" its task and "in" its plan. */
  function askerParts(a) {
    if (!a) return null;
    var out = [], who = ref(a.agent) || a.agent_name;
    if (who) out.push(who);
    if (a.task) { if (out.length) out.push(' for '); out.push(ref(a.task)); }
    if (a.plan) { out.push(out.length ? ' in ' : 'plan '); out.push(ref(a.plan)); }
    return out.length ? out : null;
  }
  /* A link to a Workbench kind's panel, or null while that kind has no renderer: a face never sends the viewer to a beads
     viewer page (links stay inside /garden, gd-qie6tPtdpi), so the cockpit, a bundle's document, a gardener's page and the
     Glass appear here as their steps land. */
  function panelLink(ctx, t, kind, id, aria) { return ctx.has(kind) ? { t: t, go: ctx.base + '/garden' + W.withPanel('', kind, id), aria: aria } : null; }
  function some(list) { list = list.filter(Boolean); return list.length ? list : undefined; }
  function due(now, t) { var d = t - now; return d >= 0 ? 'in ' + span(Math.max(60, d)) : span(Math.max(60, -d)) + ' overdue'; }
  var CHECKS = { success: 'checks passed', passed: 'checks passed', failed: 'checks failed', failure: 'checks failed', running: 'checks running', pending: 'checks waiting', canceled: 'checks canceled' };
  function checks(p) { return p ? CHECKS[String(p).toLowerCase()] || 'checks ' + words(p) : ''; }
  function mrParts(m) {
    var n = m.number ? (/^[!#]/.test(m.number) ? m.number : '#' + m.number) : '';
    return joined([m.url ? linkOut(m.url, m.title || n || urlLabel(m.url)) : n, m.state ? words(m.state) : '', checks(m.pipeline)]);
  }
  /* A live park (bead_face.go gardenerPark) in words: parked on a merge, or on the operator. */
  function parkWords(lead, kind) { return lead + (kind === 'mr_merged' ? ' until a pull request merges.' : ': every open step waits on the operator.'); }
  var PLAN = { draft: ['Draft: still being researched.', ''], ready: ['Ready: waiting on the owner to approve the build.', 'ask'], building: ['Building.', ''], complete: ['Complete.', 'ok'], abandoned: ['Abandoned.', 'dim'] };
  var FACES = {
    work: function (f) {
      var rows = [['Who', [f.assignee || 'nobody has it yet']]];
      if (f.owner && f.owner !== f.assignee) rows.push(['Owner', [f.owner]]);
      if (f.mr) rows.push(['Pull request', mrParts(f.mr)]);
      if (f.due) rows.push(['Due', [due(this.now, f.due)]]);
      if (f.deferred && f.deferred > this.now) rows.push(['Dormant until', ['wakes ' + due(this.now, f.deferred)]]);
      if (f.closed_by) rows.push(['Closed by', [f.closed_by]]);
      return { h: 'Work', rows: rows };
    },
    epic: function (f) {
      var c = f.counts || {}, done = c.closed || 0, bits = [];
      ['in_progress', 'review', 'blocked', 'open', 'deferred'].forEach(function (k) { if (c[k]) bits.push(c[k] + ' ' + stateWord(k)); });
      var state = f.total ? done + ' of ' + plural(f.total, 'child', 'children') + ' closed' : 'No children yet.';
      return { h: 'Progress', state: state, tone: f.total && done >= f.total ? 'ok' : '', rows: bits.length ? [['Still open', [bits.join(' · ')]]] : [], note: f.more ? 'Counted over the children listed below.' : '' };
    },
    plan: function (f) {
      var st = PLAN[f.state] || [f.state ? words(f.state) + '.' : 'Not in the plan lifecycle.', ''], rows = [];
      if (!this.mapped) rows.push(['Phases', [String(f.phases || 0)]]);   // a plan on the map lists its phases above
      if (f.steps && !this.mapped) rows.push(['Steps', [String(f.steps)]]);
      /* the plan's gardener loop (bead_face.go planGardener) and its live park, said as the gardener face says it */
      if (f.gardener) rows.push(['Gardener', joined([ref(f.gardener), f.gardener_state && f.gardener_state !== 'running' ? f.gardener_state : ''])]);
      if (f.dormant) rows.push(['Waits on', [ref(f.dormant)]]);
      return { h: 'Plan', state: f.dormant ? st[0] + ' ' + parkWords('Its gardener is parked', f.dormant_kind) : st[0], tone: f.dormant ? 'ask' : st[1], rows: rows,
        links: some([panelLink(this, 'Open the cockpit', 'plan', this.id), this.mapped ? panelLink(this, 'Every call on its plot', 'transcript', 'plot:' + this.id, 'Read what its gardeners did, call by call') : null]) };   // its plot's transcript (web/transcript.js)
    },
    decision: function (f) {
      var now = this.now, opt = {}, state, tone = '';
      (f.options || []).forEach(function (o) { opt[o.id] = o; });
      var pick = f.chosen && opt[f.chosen], pickText = pick ? pick.short || pick.label || pick.id : f.chosen;
      var by = joined([f.by ? 'by ' + f.by : '', ago2(now, f.at)]).join('');
      if (f.state === 'open') { state = this.mine ? 'Waiting on you to answer.' : 'Waiting on the operator to answer.'; tone = 'ask'; }
      else if (f.state === 'dismissed') state = 'Dismissed' + (by ? ' ' + by : '') + '.';
      else if (f.state === 'expired') state = 'Expired unanswered.';
      else { state = 'Answered' + (pickText ? ': ' + pickText : '') + (by ? ' (' + by + ')' : '') + '.'; tone = 'ok'; }
      var items = (f.options || []).map(function (o) {
        var chosen = !!f.chosen && o.id === f.chosen, head = o.short || o.label || o.id, parts = [{ b: head }];
        if (o.short && o.label && o.label !== o.short) parts.push({ sm: o.label, tip: o.label });   // clamped to two lines under a mouse (whole in its tooltip), whole on touch
        if (o.artifact) parts.push({ sm: '→ ' + words(o.artifact) });
        if (chosen) parts.push({ sm: 'chosen' });
        return { cls: chosen ? 'chosen' : '', parts: parts };
      });
      var rows = [], asked = askerParts(f.asked_by);
      if (asked) rows.push(['Asked by', asked]); else if (f.requested) rows.push(['Asked by', [f.requested]]);
      var v = { h: 'Decision', state: state, tone: tone, lead: f.prompt || '', body: f.context && markdown(f.context, true), rows: rows, items: items, itemsH: 'Options', texts: [] };
      if (!f.context && f.open) v.noBody = 'No context was given with this question.';
      if ((f.evidence || []).length) v.evidence = f.evidence;   // the screenshots on the beads it is about, under the context (gd-Pi7C83262N)
      if (f.more_options) v.note = '+' + plural(f.more_options, 'more option', 'more options') + ' not shown.';
      if (f.answer) v.texts.push([f.state === 'dismissed' ? 'Why' : 'Answer', f.answer]);
      if (f.open) v.acts = { kind: 'decision', id: this.id, options: (f.options || []).filter(function (o) { return o.id; }).map(function (o) { return { id: o.id, text: o.short || o.label || o.id, label: o.label || '' }; }) };
      return v;
    },
    escalation: function (f) {
      var now = this.now, by = [f.by ? 'by ' + f.by : '', ago2(now, f.at)].filter(Boolean).join(' '), by_ = by ? ' ' + by : '';
      var who = this.mine ? 'you' : { desk: 'the merge desk', operator: 'the operators' }[f.audience] || 'the owner';
      var STATES = { pending: 'Waiting on ' + who + ' to approve.', approved: 'Approved' + by_ + '; running.', done: 'Done: approved' + by_ + '.', failed: 'Failed: approved' + by_ + ', then the run failed.', rejected: 'Rejected' + by_ + '.' };
      var tone = { pending: 'ask', done: 'ok', failed: 'bad', rejected: 'bad' }[f.state] || '';
      var rows = [];
      if (f.op) rows.push(['Operation', [words(f.op)]]);
      if (f.sensitivity) rows.push(['Sensitivity', [words(f.sensitivity)]]);
      if (f.audience) rows.push(['For', [{ owner: 'the owner', desk: 'the merge desk', operator: 'the operators' }[f.audience] || words(f.audience)]]);
      var asked = askerParts(f.asked_by);
      if (asked) rows.push(['Asked by', asked]);
      if ((f.targets || []).length) rows.push(['Acts on', joined(f.targets.map(function (u) { return linkOut(u, urlLabel(u)); }))]);
      if (f.method) rows.push(['Approved via', [words(f.method)]]);
      if (f.executor) rows.push(['Run by', [f.executor]]);
      var v = { h: 'Escalation', state: STATES[f.state] || words(f.state), tone: tone, lead: f.ask || '', rows: rows, texts: [] };
      if (f.reason) v.texts.push(['Why rejected', f.reason]);
      if (f.result) v.texts.push(['Result', f.result]);
      if (f.open) { v.acts = { kind: 'escalation', id: this.id, title: this.title || '' }; if (f.held) v.acts.held = f.held; }
      return v;
    },
    agent: function (f, d) {
      var rows = [], cost = [];
      if (f.task) rows.push(['Tending', [typeof ARMS !== 'undefined' ? ARMS.lineArms(d) : null, ref(f.task)].filter(Boolean)]);   // its task's line arms (gd-ASLjQPkHJz)
      if (f.thread && !(d.kin && d.kin.head)) rows.push(['In thread', [threadRef(f.thread)]]);   // the thread a thread agent answers (5b), unless Part of wish names it
      if (f.project || f.role) rows.push(['Works', [joined([f.project ? 'on ' + f.project : '', f.role ? 'as ' + f.role : '']).join('')]]);
      if (f.mode || f.harness) rows.push(['Runs', [joined([f.mode, f.harness]).join('')]]);
      if (f.cost_usd > 0) cost.push('$' + f.cost_usd.toFixed(2));
      if (f.turns > 0) cost.push(plural(f.turns, 'turn', 'turns'));
      if (cost.length) rows.push([f.live ? 'Spent so far' : 'Spent', [cost.join(' · ')]]);
      if (f.spawned_by) rows.push(['Spawned by', [f.spawned_by]]);
      var v = { h: 'Gardener', state: f.live ? words(f.state) || 'on the farm' : (f.state ? words(f.state) + ' · ' : '') + 'left the farm', tone: f.live ? '' : 'dim', lead: f.tldr || '', rows: rows };   // the wrap-up leads: the outcome first
      v.links = some([f.live ? panelLink(this, 'Watch live', 'glass', this.id, 'Watch ' + f.name + ' live') : null, f.live ? panelLink(this, 'Read its transcript', 'transcript', this.id, 'Read what ' + f.name + ' did, call by call') : null,   // the Glass takes the agent bead (glass.go), as its transcript does (transcript.go)
        this.costs && f.name ? panelLink(this, 'See its charts', 'costs', 'agent:' + f.name, 'See the charts of ' + f.name) : null]);   // its bill, context and turns (web/costs.js, step 4.2)
      return v;
    },
    message: function (f, d) {
      var VIA = { squawk: 'a squawk', slack: 'Slack', message: 'a message' }, rows = [];
      if (f.from) rows.push(['From', [f.from]]);
      rows.push(['Sent', [joined([VIA[f.via] || words(f.via), ago2(this.now, f.at)]).join('')]]);
      if (f.thread && !(d && d.kin && d.kin.head)) rows.push(['In thread', [threadRef(f.thread)]]);   // unless Part of wish names it
      return { h: 'Message', lead: f.text || '', rows: rows };   // the message itself leads, who sent it under it (rubric C11)
    },
    bundle: function (f) {
      var docs = f.docs || [];
      return { h: 'Documents (' + docs.length + ')', state: docs.length ? '' : 'No documents yet.', items: docs.map(function (x, i) { return { parts: [{ sm: String(i + 1) }, { bead: x.id, t: x.title }].concat(x.format ? [{ sm: x.format }] : []) }; }),
        note: f.more ? '+' + plural(f.more, 'more document', 'more documents') + ' not listed.' : '', links: some([panelLink(this, 'Read as one document', 'bundle', this.id)]) };
    },
    /* A markdown report (the format gb decision report files, and one with no format) is the document itself: its blocks,
       whole, never folded (the bead view is the detail a card links to, rubric C11). Any other format stays text. */
    report: function (f) {
      var rows = [], md = !f.format || /^(markdown|md)$/i.test(f.format);
      if (f.type) rows.push(['Kind', [words(f.type)]]);
      if (f.format && !md) rows.push(['Format', [f.format]]);
      var v = { h: 'Report', rows: rows, state: f.content ? '' : 'This report has no content.', texts: [] };
      if (f.content && md) v.doc = markdown(f.content);
      else if (f.content) v.texts.push(['Content', f.content]);
      if (v.doc && v.doc.length && v.doc[0].t === 'h' && v.doc[0].n === 1 && mdPlain(v.doc[0].c).trim() === String(this.title || '').trim()) v.doc.shift();   // the header says the title already
      return v;
    },
    gardener: function (f) {
      var today = new Date(this.now * 1000).toISOString().slice(0, 10), rows = [], runs = [];
      /* dormant is the live park (bead_face.go gardenerPark), never the stamp: a loop parked on the operator, or on a merge. */
      var parked = f.dormant ? parkWords('Parked', f.dormant_kind) : '';
      var state = f.retired ? 'Retired.' : !f.enabled ? 'Paused.' : parked || 'Running' + (f.standing ? ', standing' : '') + '.';
      if (f.dormant) rows.push(['Waits on', [ref(f.dormant)]]);
      if (f.cron) rows.push(['Schedule', [f.cron + (f.timezone ? ' (' + f.timezone + ')' : '')]]);
      if (f.runs_spawned) runs.push(f.runs_spawned + ' spawned');
      if (f.runs_completed) runs.push(f.runs_completed + ' finished');
      if (f.max_runs) runs.push('cap ' + f.max_runs);
      if (runs.length) rows.push(['Runs', [runs.join(' · ')]]);
      if (f.max_runs_per_day) rows.push(['Today', [(f.runs_day === today ? f.runs_today || 0 : 0) + ' of ' + f.max_runs_per_day + ' runs']]);
      if (f.failures) rows.push(['Failures', [plural(f.failures, 'failed run', 'failed runs') + ' in a row']]);
      if (f.last_run) rows.push(['Last run', [ref(f.last_run)]]);
      return { h: f.steward ? 'Steward' : 'Gardener loop', state: state, tone: f.retired || !f.enabled ? 'dim' : f.dormant || f.failures ? 'ask' : '', rows: rows, texts: f.goal ? [['Goal', f.goal]] : [],
        links: some([panelLink(this, 'Open the gardener page', 'gardener', this.id)]) };
    },
    mr: function (f) {
      var rows = [];
      if (f.url) rows.push(['Link', [linkOut(f.url, f.title || urlLabel(f.url))]]);
      if (f.source || f.target) rows.push(['Branches', [(f.source || '?') + ' → ' + (f.target || '?')]]);
      if (f.author) rows.push(['Author', [f.author]]);
      if (f.approvals) rows.push(['Approvals', [f.approvals]]);
      if (f.threads && f.threads !== '0') rows.push(['Open threads', [f.threads]]);
      if (f.commit) rows.push(['Last commit', [f.commit]]);
      var st = (f.state || '').toLowerCase(), live = st !== 'merged' && st !== 'closed', waits = [], ap = /^(\d+)\/(\d+)$/.exec(f.approvals || '');
      if (live && ap && +ap[1] < +ap[2]) waits.push(plural(+ap[2] - +ap[1], 'more approval', 'more approvals'));
      if (live && +f.threads > 0) waits.push(plural(+f.threads, 'open thread', 'open threads'));
      var line = joined([words(f.state) || 'state unknown', checks(f.pipeline)]).join('') + (waits.length ? '; waiting on ' + waits.join(' and ') : '') + '.';
      return { h: 'Merge request', state: line.charAt(0).toUpperCase() + line.slice(1), tone: st === 'merged' ? 'ok' : st === 'closed' ? 'dim' : /fail/i.test(f.pipeline || '') ? 'bad' : waits.length ? 'ask' : '', rows: rows };
    }
  };
  /* A long text folded (Commons UX rubric C11: about 600 characters before a fold): {head, rest} with head cut at a word
     boundary near n and marked with an ellipsis, rest '' when it fits. Cut by code point, never inside one. */
  var FOLD = 600, FOLD_LIST = 8;
  function fold(text, n) {
    var cs = Array.from(String(text || '')), at;
    n = n || FOLD;
    if (cs.length <= n) return { head: cs.join(''), rest: '' };
    var head = cs.slice(0, Math.round(n * 0.8)).join(''), sp = head.search(/\s\S*$/);
    if (sp > n * 0.5) head = head.slice(0, sp);
    at = head.length;
    return { head: head.replace(/[\s,;:.-]+$/, '') + '…', rest: cs.join('').slice(at), total: cs.length };
  }
  /* The view of a document's face, or null (no face, or a kind with no renderer: the generic fields table stands). */
  function faceView(d, ctx) {
    var f = (d || {}).face, r = f && FACES[f.kind];
    return r && f[f.kind] ? r.call(ctx, f[f.kind], d) : null;
  }
  /* The kind word in the panel's header: what the face calls the bead. */
  function kindWord(d, type) {
    var f = (d || {}).face;
    if (f && f.kind === 'gardener') return f.gardener && f.gardener.steward ? 'steward' : 'gardener loop';
    if (f && f.kind === 'plan') return 'plan';
    if (f && (f.kind === 'mr' || f.kind === 'wish')) return f.kind === 'mr' ? 'merge request' : 'wish';
    return type === 'agent' ? 'gardener' : type || 'bead';
  }

  /* What a failed load paints under its line (WB-15): what the map knows, said as that, or that the map does not show it. */
  function degraded(k) {
    k = k || {};
    return k.title || k.status || k.project ? 'This is what the map knows about it.' : 'The map does not show it either.';
  }
  /* A refresh now would lose the viewer's work (WB-14): a form opened in the panel (an edit, a close reason, a spawn, a
     gardener's brief, an adoption), an answer armed or a write in flight, or text typed into a box. The comment composer,
     the conversation's composer and the add-a-link form are always there, so they hold only once typed into. */
  function holding(root) {
    if (!root || !root.querySelector) return false;
    if (root.querySelector('form:not(.bw-comment):not(.bw-links):not(.bv-cv-say), .armed, [data-armed], [disabled]:not(.bv-retry)')) return true;
    return Array.prototype.some.call(root.querySelectorAll('textarea, input'), function (x) { return !!x.value && x.value !== x.defaultValue; });
  }

  var api = { degraded: degraded, holding: holding, param: param, withBead: withBead, linkId: linkId, sections: sections, shownIds: shownIds, mrURL: mrURL, prButton: prButton, safeURL: safeURL, stateWord: stateWord, runs: runs, urlLabel: urlLabel, phases: phases, phaseNote: phaseNote, baseWord: baseWord, badges: badges, signals: signals, metrics: metrics, picSrc: picSrc, faceView: faceView, foldList: foldList, kindWord: kindWord, fold: fold, markdown: markdown, mdPlain: mdPlain, safeHref: safeHref, mdPicSrc: mdPicSrc, FACES: FACES, host: W, open: function () {}, close: function () {}, current: function () { return ''; } };
  if (typeof document === 'undefined') return api;

  /* ---------- DOM ---------- */
  var B = document.body, panel = document.getElementById('bead');
  if (typeof fetch === 'undefined') return api;
  // a page with no panel (the Commons node's, web/node.html) draws a document only where into() is asked to
  var base = B.dataset.base || '', titleEl = document.getElementById('bv-title'), kindEl = document.getElementById('bv-kind'), bodyEl = document.getElementById('bv-body');
  if (!panel) panel = titleEl = kindEl = bodyEl = el('div');
  var last = null, lastAt = 0, failNote = null, actsEl = null, writeEl = null, flyOnDoc = false, more = {};   // more: bead id -> how many comments to ask for (Show older)
  var drawnPlan = false;   // whether the farm knew the shown bead's plan when its body was drawn (its face and sections hang on it)
  var BW = typeof BEADWRITE !== 'undefined' ? BEADWRITE : null;   // the writes (web/beadwrite.js), when the page carries them
  var BT = typeof BEADTALK !== 'undefined' && BEADTALK.use ? BEADTALK : null;   // the Comments and Conversation sections (web/beadtalk.js)
  var BK = typeof BEADWORK !== 'undefined' && BEADWORK.mount ? BEADWORK : null;   // the Work and Open sections, the Commons node's (web/beadwork.js)
  if (BT) BT.use({ el: el, sec: sec, button: button, href: href, ago: ago, plural: plural, markdown: markdown, mdInto: mdInto, fold: fold, rich: rich, retryButton: retryButton, ask: function (id, n) { more[id] = n; }, B: B, W: W, BW: BW, base: base, live: !!document.getElementById('bead') });

  function G() { return window.GARDEN || null; }
  function known(id) { var g = G(); return g ? g.known(id) : { id: id }; }
  /* The bead the panel shows, '' when it is closed or shows another kind. */
  function cur() { var c = W.current(); return c && c.kind === 'bead' ? c.id : ''; }
  function el(t, cls, text) { var e = document.createElement(t); if (cls) e.className = cls; if (text !== undefined) e.textContent = text; return e; }
  function ago(t) { var d = Math.max(0, Math.floor(Date.now() / 1000) - t); if (d < 60) return d + 's'; if (d < 3600) return Math.floor(d / 60) + 'm'; if (d < 86400) return Math.floor(d / 3600) + 'h'; return Math.floor(d / 86400) + 'd'; }
  function href(id) { return base + '/garden?bead=' + encodeURIComponent(id); }
  /* A long list shows eight and a count (rubric C11): from ten rows, the ninth on (bar a chosen option) hide behind Show all <n>,
     which lists them in place and focuses the first. The list, or the list and its button; a Workbench panel folds its lists too. */
  function foldList(ul) {
    var lis = [].slice.call(ul.children), rest = lis.length > FOLD_LIST + 1 ? lis.slice(FOLD_LIST).filter(function (li) { return li.className != 'chosen'; }) : [];
    if (!rest.length) return ul;
    var f = document.createDocumentFragment(), all = el('button', 'btn small ghost bv-more', 'Show all ' + lis.length);
    all.type = 'button'; all.onclick = function () { rest.forEach(function (li) { li.hidden = false; }); all.remove(); var a = rest[0].querySelector('a,button'); if (a) a.focus(); };
    rest.forEach(function (li) { li.hidden = true; });
    f.appendChild(ul); f.appendChild(all);
    return f;
  }
  function button(text, fn) { var b = el('button', 'btn small', text); b.type = 'button'; b.onclick = fn; return b; }
  /* A paragraph of free text with its URLs and bead ids as links, built node by node (never innerHTML). A bead id opens here
     (the click handler takes /garden?bead=) and carries the title the page or this document knows; a URL opens a new tab
     under its short label. */
  function rich(cls, text, titles) { return runsInto(el('p', cls), text, titles); }
  function runsInto(p, text, titles) {
    runs(text).forEach(function (r) {
      if (r.k === 'text') { p.appendChild(document.createTextNode(r.s)); return; }
      var a;
      if (r.k === 'bead') {   // its title, the id in its tooltip (titles, not ids: web/titles.js)
        var t = titles[r.s] || known(r.s).title;
        if (t === r.s) t = '';
        if (typeof TITLES !== 'undefined') a = TITLES.link(r.s, { cls: 'bl bv-ref', text: t });
        else { a = el('a', 'bl bv-ref', t || r.s); a.href = href(r.s); a.title = t ? t + ' (' + r.s + ')' : r.s; }
        p.appendChild(a);
        // one the document looked up and the daemon did not return (unseen: '' in titles) says so, as a relation row does
        if (!t && titles[r.s] === '' && !(a.title && a.title !== r.s)) { a.title = r.s + ' (not visible to you)'; p.appendChild(el('span', 'bv-unseen', ' (not visible to you)')); }
        return;
      } else {
        a = el('a', 'bl bv-url', r.label); a.href = r.s; a.title = r.s;
        if (!linkId(base, r.s, location.origin)) { a.target = '_blank'; a.rel = 'noopener noreferrer'; }
      }
      p.appendChild(a);
    });
    return p;
  }
  /* The bead's id, for an operator: a copy button that names it in its tooltip, never the id as text (gd-9yOATV2qCs). */
  function idButton(id) {
    var b = button('Copy id', function () {
      var done = function () { b.textContent = 'Copied'; setTimeout(function () { b.textContent = 'Copy id'; }, 1500); };
      try { navigator.clipboard.writeText(id).then(done, function () { b.textContent = id; }); } catch (e) { b.textContent = id; }
    });
    b.className = 'btn small bv-id'; b.title = id; b.setAttribute('aria-label', 'Copy the id ' + id);
    return b;
  }
  /* The titles a document carries for the bead ids its text names (bead_mentions.go): id -> title, and '' for an id it
     looked up that the daemon did not return (unseen), which runsInto says is not visible. */
  function docTitles(d) {
    var t = {};
    Object.keys((d || {}).mentions || {}).forEach(function (id) { t[id] = d.mentions[id]; if (typeof TITLES !== 'undefined') TITLES.learn(id, d.mentions[id]); });
    ((d || {}).unseen || []).forEach(function (id) { if (!t[id]) t[id] = ''; });
    return t;
  }
  /* Every bead id the panel shows reads as its title, a link (web/titles.js). */
  function dress(root) { if (typeof TITLES !== 'undefined') TITLES.dress(root); }
  /* A plan document's step bodies by id: the bead route sends a ready or asked plan's children with their descriptions. */
  function stepBodies(d) { var b = {}; ((d || {}).children || []).forEach(function (c) { if (c.body) b[c.id] = c.body; }); return b; }
  function childIDs(d) { if (!d || !d.children) return null; var h = {}; d.children.forEach(function (c) { h[c.id] = 1; }); return h; }
  function sec(title, node, key) { var s = el('section', 'bv-sec'); if (key) s.dataset.sec = key; s.appendChild(el('h3', '', title)); s.appendChild(node); return s; }
  /* Scroll the panel to its section key (sec) and focus its heading, for a face's part that links to it ({sec}, web/wishface.js). */
  function goSec(key) { var h = bodyEl.querySelector('[data-sec="' + key + '"] > h3'); if (h) { h.tabIndex = -1; h.focus({ preventScroll: true }); h.scrollIntoView({ block: 'start' }); } }

  /* Lines from WEEDS.lines or EDGES.lines ([cls, text, href, title]) as a list: the sentence, then the bead it names as a
     link into this view. */
  function lineList(cls, lines) {
    var ul = el('ul', 'bv-rels ' + cls);
    lines.forEach(function (ln) {
      var li = el('li', ln[0]);
      li.appendChild(el('span', '', ln[1]));
      if (ln[2] && ln[3]) { var a = el('a', 'bl bv-ref', ln[3]); a.href = ln[2]; li.appendChild(a); }
      ul.appendChild(li);
    });
    return ul;
  }
  /* What the plot's info card carried, before it retired (cursor-grid spec N-D14, 6.4): who stands on the plot, its operator,
     its edges to other projects and every phase in full. */
  function renderPlot(k) {
    var who = k.who || [];
    if (who.length) {
      var gl = el('ul', 'bv-rels bv-who');
      who.forEach(function (g) {
        var li = el('li');
        if (g.id) { var a = el('a', 'bl', g.name); a.href = href(g.id); li.appendChild(a); } else li.appendChild(el('span', '', g.name));
        var bits = [];   // no arms picture: two of them put a phone's cold load over B1 (decision gd-C5WGUm5SLu); the Porch row, badge and tip keep them
        if (g.word) bits.push(g.word);
        if (g.task) bits.push('tending ' + g.task);
        if (bits.length) li.appendChild(el('span', 'sm', bits.join(' · ')));
        gl.appendChild(li);
      });
      actsEl.appendChild(sec('Gardeners here (' + who.length + ')', gl));
    }
    if (k.operator) actsEl.appendChild(sec('Operator', el('p', 'bv-text', k.operator)));
    if ((k.edges || []).length) actsEl.appendChild(sec('Across projects', lineList('bv-edges', k.edges)));
    var ps = phases(k.plan);
    if (ps.length) {
      var ol = el('ol', 'bv-rels bv-phases');
      ps.forEach(function (p) {
        var li = el('li', p.current ? 'cur' : p.closed ? 'done' : '');
        var body = el('span', 'bv-pb');   // the title and its note wrap beside the number, never under it
        li.appendChild(el('span', 'bv-n', p.other ? '·' : String(p.n)));
        if (p.id) { var a = el('a', 'bl', p.title); a.href = href(p.id); body.appendChild(a); } else body.appendChild(el('span', 'bv-pt', p.title));
        body.appendChild(el('span', 'sm', phaseNote(p)));
        li.appendChild(body);
        ol.appendChild(li);
      });
      actsEl.appendChild(sec('Phases (' + ps.length + ')', ol));
    }
  }

  /* The buttons that act on the bead where the farm can: fly to it, its pull request, and (for a
     decision or escalation in the Mailbox) a copy of the Mailbox's answer row, which garden.js's act() drives as it is. Above
     them, why each weed here is one (first, so a phone shows them unscrolled, gd-TDLWiDH7Yj); below, for a plan, what its
     card carried. */
  function renderActs(k, d) {
    if (!actsEl) return;
    actsEl.textContent = '';
    if (k.seed && typeof STIP !== 'undefined') actsEl.appendChild(sec(k.staked ? 'Staked out' : 'In the Nursery', STIP.potCard(k.seed, false)));   // a pot: its state and its plan decision's card, first (9.11b)
    if (k.staked && typeof STIP !== 'undefined') actsEl.appendChild(sec('Steps (' + (k.seed.steps || []).length + ')', STIP.potSteps(k.seed, stepBodies(d), href, childIDs(d), !!(d && d.description))));   // a staked plot: every step with its body, then the narrative
    if ((k.weeds || []).length) actsEl.appendChild(sec('Weeds', lineList('bv-weeds', k.weeds)));
    var row = el('div', 'bv-acts');
    if (k.onMap) row.appendChild(button('Show on the map', function () {
      var g = G();
      if (g) g.fly(k.id);
    }));
    var pr = prButton(d);
    if (pr) { var a = el('a', 'btn small ext', pr.t); a.href = pr.url; a.target = '_blank'; a.rel = 'noopener noreferrer'; a.setAttribute('aria-label', pr.aria); a.title = pr.aria; row.appendChild(a); }
    if (row.children.length) actsEl.appendChild(row);
    if (writeEl) actsEl.appendChild(writeEl);   // the write bar under them, above a plan's sections (a phone reaches it unscrolled); the same node each paint, so an open form keeps its draft
    if (k.plan && typeof PLANS !== 'undefined') {
      var t = PLANS.tile(k.plan, Date.now() / 1000);
      actsEl.appendChild(el('p', 'bv-prog', t.done + ' of ' + t.total + ' steps closed' + (t.total ? ' (' + t.pct + '%)' : '') + (t.counts.in_progress ? ' · ' + t.counts.in_progress + ' growing' : '') + (t.counts.blocked ? ' · ' + t.counts.blocked + ' blocked' : '') + (PLANS.waitLine(k.plan) ? ' · ' + PLANS.waitLine(k.plan) : '')));
      var ws = k.plan.waiting || [];
      if (ws.length) {   // each open decision and escalation, whom it waits on and since when; a link opens it here
        var ul = el('ul', 'pm-waits');
        ws.forEach(function (w) {
          var li = el('li', w.human ? 'ask' : 'wtg'), a = el('a', 'bl', w.title || w.id);
          a.href = base + '/garden?bead=' + encodeURIComponent(w.id); a.title = w.title ? w.title + ' (' + w.id + ')' : w.id;
          li.appendChild(el('span', 'wt', PLANS.waitText(w))); li.appendChild(a); li.appendChild(el('span', 'sm', ago(w.since) + ' ago'));
          ul.appendChild(li);
        });
        actsEl.appendChild(sec('Open decisions and escalations', ul));
      }
      renderPlot(k);
    } else if ((k.edges || []).length) actsEl.appendChild(sec('Across projects', lineList('bv-edges', k.edges)));
    var fv = faceView(d, faceCtx(k.id));   // a face with its own answer box (an open decision or escalation) stands for the Mailbox row's copy
    if (k.act && !(fv && fv.acts)) actsEl.appendChild(sec('Waiting on you', k.act.cloneNode(true)));
    if (!k.plan && typeof TEND !== 'undefined') TEND.live(actsEl, k.id, { ago: ago });   // the gardeners sent to it, live (web/tend.js); a plan's are listed above
    dress(actsEl);
  }

  /* The Harvest section (harvest spec 6.2), first on a closed step: the headline (base word and badges), the newest grade's
     pictures, every grade with who and when, the merge-time signals, lead time and cost, produce and follow-ups, and the
     crew's wrap-up. */
  function renderHarvest(h, titles) {
    var s = el('section', 'bv-sec bv-harvest');
    s.appendChild(el('h3', '', 'Harvest'));
    var head = el('p', 'bv-hv-head');
    head.appendChild(el('span', 'bv-hv-base ' + (h.base || 'bare'), baseWord(h.base)));
    badges(h).forEach(function (b) { var c = head.appendChild(el('span', 'pc bv-hv-b ' + b.k, b.w)); if (typeof ART !== 'undefined') c.insertBefore(ART.badgeIcon(b.k), c.firstChild); });   // icon and word (spec 6.2)
    s.appendChild(head);

    var g = newestGrade(h), pics = g ? (g.urls || []).filter(function (u) { return picSrc(base, u); }) : [];
    if (pics.length) {
      var grid = el('div', 'bv-pics');
      pics.forEach(function (u, i) {
        var a = el('a'), img = el('img'), name = (g.subject || 'grade') + ' · ' + (typeof PICTURES !== 'undefined' ? PICTURES.nameOf(u) : 'picture ' + (i + 1));   // its subject and file, the viewer's title; its place is the viewer's own "N of M" (gd-ch5rTDKzvE)
        a.href = picSrc(base, u); a.target = '_blank'; a.rel = 'noopener noreferrer'; a.title = name;   // the proxied picture, never the raw bucket link
        img.src = picSrc(base, u); img.alt = name; img.loading = 'lazy'; img.decoding = 'async'; img.referrerPolicy = 'no-referrer';
        img.onerror = function () { a.textContent = 'picture ' + (i + 1); a.className = 'gone'; };   // a viewer the proxy refuses keeps the link
        a.appendChild(img); grid.appendChild(a);
      });
      s.appendChild(grid);
    }

    var dl = el('dl', 'bv-fields bv-hv'), row = function (k, v) { dl.appendChild(el('dt', '', k)); var dd = el('dd'); dd.appendChild(v); dl.appendChild(dd); };
    if ((h.grades || []).length) {
      var ul = el('ul', 'bv-rels');
      h.grades.forEach(function (x) {
        var li = el('li'), a = el('a', 'bl', (x.of ? 'graded ' + x.score + '/' + x.of : 'graded') + (x.subject ? ': ' + x.subject : ''));
        a.href = href(x.id); a.title = x.id; li.appendChild(a);
        var bits = [];
        if (x.percent) bits.push(x.percent + '%');
        if (x.by) bits.push('by ' + x.by);
        if (x.at) bits.push(ago(x.at) + ' ago');
        if ((x.urls || []).length) bits.push(plural(x.urls.length, 'picture', 'pictures'));
        li.appendChild(el('span', 'sm', bits.join(' · ')));
        ul.appendChild(li);
      });
      row('Grades', ul);
    }
    var sg = signals(h);
    if (h.mr && h.mr.state !== 'merged') sg.unshift(h.mr.state === 'closed' ? 'MR closed unmerged' : 'MR still open');
    if (sg.length) row('Checks', el('span', '', sg.join(' · ')));
    var mt = metrics(h);
    if (mt.length) row('Time and cost', el('span', '', mt.join(' · ')));
    var links = function (list, more) {
      var p = el('span', 'bv-hv-l');
      list.forEach(function (r) {
        var name = r.short_title || r.title, a = el('a', 'bl', name || r.id); a.href = href(r.id); a.title = r.title ? r.title + ' (' + r.id + ')' : r.id;
        if (name && typeof TITLES !== 'undefined') TITLES.learn(r.id, name);
        p.appendChild(a);
      });
      if (more) p.appendChild(el('span', 'sm', more));
      return p;
    };
    if ((h.produce || []).length) row('Produce', links(h.produce, ''));
    var fu = h.follow_ups || [], open = fu.filter(function (r) { return r.status !== 'closed'; }).length;
    if (fu.length) row('Follow-ups', links(fu, open ? open + ' still open' : 'all closed'));
    if (h.tldr) row('Wrap-up', rich('bv-text', h.tldr + (h.tldr_by ? ' (' + h.tldr_by + ')' : ''), titles));
    if (dl.children.length) s.appendChild(dl);
    return s;
  }

  /* A face's parts as nodes: text, a bead link (opens here), a link out, a link to a page, a small aside. */
  function partsInto(node, parts) {
    (parts || []).forEach(function (p) {
      if (!p) return;
      if (typeof p === 'string') { node.appendChild(document.createTextNode(p)); return; }
      if (p.nodeType) { node.appendChild(p); return; }
      if (p.sm !== undefined) { var sm = node.appendChild(el('span', 'sm', p.sm)); if (p.tip) sm.title = p.tip; return; }
      if (p.b !== undefined) { node.appendChild(el('b', '', p.b)); return; }
      if (p.sec) { var j = node.appendChild(el('a', 'bl', p.t)); j.href = '#'; j.dataset.sec = p.sec; j.addEventListener('click', function (e) { e.preventDefault(); goSec(p.sec); }); return; }
      if (!p.bead && p.url && !safeURL(p.url)) { node.appendChild(document.createTextNode(p.t || '')); return; }   // never a javascript: href
      var a = el('a', 'bl', p.t || p.bead || p.url || p.go);
      if (p.bead) { a.href = href(p.bead); a.title = p.t && p.t !== p.bead ? p.t + ' (' + p.bead + ')' : p.bead; if (p.t && p.t !== p.bead && typeof TITLES !== 'undefined') TITLES.learn(p.bead, p.t); }
      else if (p.url) { a.href = p.url; a.title = p.url; a.target = '_blank'; a.rel = 'noopener noreferrer'; }
      else a.href = p.go;
      node.appendChild(a);
    });
    return node;
  }
  /* The answer box web/answer.js drives and builds (ANSWER.box): it is on the page from its first paint, for a pot's card
     (web/stip.js) before this file has run (web/lazy.js). */
  function answerBox(a) { return ANSWER.box(a); }
  /* The face's section (WB-1): its heading, state line, lead, rows, list, texts, links and answer box. A state is said in
     words; its tone only colours them. */
  /* A long text folded behind "Show all N characters", which opens the whole of it in place and shuts it again (web/cuts.js). */
  function folded(cls, text, titles) {
    var f = fold(text);
    if (!f.rest) return rich(cls, text, titles);
    return CUTS.make(rich(cls, f.head, titles), function () { var p = rich(cls, text, titles); dress(p); return p; }, 'Show all ' + f.total.toLocaleString('en-US') + ' characters');
  }
  /* A report's markdown blocks (markdown() above) built into the panel, element by element. A picture the viewer's image
     proxy serves shows at the panel's width and opens whole in a new tab; one alone in its paragraph is a figure captioned
     with its title or alt text. A picture from anywhere else is not loaded from the page: it stays a link, as on the bead page. */
  function mdInto(parent, blocks, titles) {
    blocks.forEach(function (b) {
      var e, figs;
      if (b.t === 'h') e = inlineInto(el('h' + Math.min(6, b.n + 3), 'bv-md-h bv-md-h' + Math.min(b.n, 3)), b.c, titles);
      else if (b.t === 'p') {
        figs = b.c.filter(function (n) { return !(n.k === 'text' && !n.s.trim()) && n.k !== 'br'; });
        if (figs.length && figs.every(function (n) { return n.k === 'img' && mdPicSrc(base, n.src); })) { figs.forEach(function (n) { parent.appendChild(figure(n)); }); return; }
        e = inlineInto(el('p'), b.c, titles);
      } else if (b.t === 'code') { e = el('pre', 'bv-md-pre'); e.tabIndex = 0; e.appendChild(el('code', '', b.s)); }
      else if (b.t === 'quote') e = mdInto(el('blockquote'), b.c, titles);
      else if (b.t === 'hr') e = el('hr');
      else if (b.t === 'ul' || b.t === 'ol') {
        e = el(b.t);
        if (b.t === 'ol' && b.start !== 1) e.start = b.start;
        b.items.forEach(function (it) { e.appendChild(mdInto(el('li'), it, titles)); });
      } else if (b.t === 'table') {
        var t = el('table'), tr = el('tr'), body = el('tbody');
        b.head.forEach(function (c, k) { var th = inlineInto(el('th'), c, titles); th.scope = 'col'; if (b.align[k]) th.style.textAlign = b.align[k]; tr.appendChild(th); });
        t.appendChild(el('thead')).appendChild(tr);
        b.rows.forEach(function (r) { var row = el('tr'); r.forEach(function (c, k) { var td = inlineInto(el('td'), c, titles); if (b.align[k]) td.style.textAlign = b.align[k]; row.appendChild(td); }); body.appendChild(row); });
        t.appendChild(body);
        e = el('div', 'bv-md-table'); e.tabIndex = 0; e.setAttribute('role', 'region'); e.setAttribute('aria-label', 'Table'); e.appendChild(t);   // a wide table scrolls by itself
      }
      if (e) parent.appendChild(e);
    });
    return parent;
  }
  function inlineInto(parent, nodes, titles) {
    nodes.forEach(function (n) {
      var a;
      if (n.k === 'text') runsInto(parent, n.s, titles);
      else if (n.k === 'br') parent.appendChild(el('br'));
      else if (n.k === 'code') parent.appendChild(el('code', '', n.s));
      else if (n.k === 'b' || n.k === 'i' || n.k === 'del') parent.appendChild(inlineInto(el({ b: 'strong', i: 'em', del: 'del' }[n.k]), n.c, titles));
      else if (n.k === 'img') parent.appendChild(mdPicSrc(base, n.src) ? picture(n, 'bv-md-pic') : outLink(n.src, n.alt ? 'picture: ' + n.alt : n.src));
      else if (n.k === 'a') {
        var to = safeHref(n.href);
        if (!to) { inlineInto(parent, n.c, titles); return; }
        a = inlineInto(el('a', 'bl bv-url'), n.c, titles); a.href = to; a.title = n.href;
        if (!linkId(base, to, location.origin) && !/^#/.test(to)) { a.target = '_blank'; a.rel = 'noopener noreferrer'; }
        parent.appendChild(a);
      }
    });
    return parent;
  }
  function outLink(u, label) {
    var to = safeHref(u), a;
    if (!to) return document.createTextNode(label);
    a = el('a', 'bl bv-url', label); a.href = to; a.title = u; a.target = '_blank'; a.rel = 'noopener noreferrer';
    return a;
  }
  function picture(n, cls) {
    var a = el('a', cls), img = el('img'), src = mdPicSrc(base, n.src);
    a.href = src; a.target = '_blank'; a.rel = 'noopener noreferrer'; a.title = 'Open the picture whole';
    img.src = src; img.alt = n.alt || 'picture'; img.loading = 'lazy'; img.decoding = 'async'; img.referrerPolicy = 'no-referrer';
    img.onerror = function () { a.textContent = 'Picture did not load' + (n.alt ? ': ' + n.alt : ''); a.className = cls + ' gone'; };   // a viewer the proxy refuses keeps the link
    a.appendChild(img);
    return a;
  }
  function figure(n) {
    var f = el('figure', 'bv-md-fig');
    f.appendChild(picture(n, 'bv-md-pic'));
    if (n.title || n.alt) f.appendChild(el('figcaption', '', n.title || n.alt));
    return f;
  }
  function renderFace(v, kind, titles) {
    var s = el('section', 'bv-sec bv-face'), dl;
    s.dataset.face = kind;
    s.appendChild(el('h3', v.said ? 'sr' : '', v.h));   // said: the panel's kind word above says it already (one fact once)
    if (v.state) s.appendChild(el('p', 'bv-face-state' + (v.tone ? ' ' + v.tone : ''), v.state));
    if (v.lead) s.appendChild(folded('bv-text bv-face-lead', v.lead, titles));
    if (v.sum) s.appendChild(partsInto(el('p', 'bv-face-sum'), v.sum));   // one line of counts under the lead (a wish's)
    if (v.body || v.noBody) s.appendChild(v.body ? mdInto(el('div', 'bv-md bv-face-body'), v.body, titles) : el('p', 'bv-empty', v.noBody));
    var ev = v.evidence && typeof ANSWER !== 'undefined' && ANSWER.evidence(v.evidence, function (u) { return picSrc(base, u); }, href);   // web/answer.js
    if (ev) s.appendChild(ev);
    if (v.acts) s.appendChild(answerBox(v.acts));   // the ask, then its action, then the detail (rubric C11)
    if ((v.rows || []).length) {
      dl = el('dl', 'bv-fields bv-face-rows');
      v.rows.forEach(function (r) { dl.appendChild(el('dt', '', r[0])); dl.appendChild(partsInto(el('dd'), r[1])); });
      s.appendChild(dl);
    }
    if ((v.items || []).length) {
      if (v.itemsH) s.appendChild(el('p', 'bv-face-k', v.itemsH));
      var ul = el('ul', 'bv-face-list');
      v.items.forEach(function (it) { ul.appendChild(partsInto(el('li', it.cls || ''), it.parts)); });
      s.appendChild(foldList(ul));
    }
    if (v.doc) s.appendChild(mdInto(el('div', 'bv-md'), v.doc, titles));
    (v.texts || []).forEach(function (t) { s.appendChild(el('p', 'bv-face-k', t[0])); s.appendChild(folded('bv-text', t[1], titles)); });
    if (v.note) s.appendChild(el('p', 'bv-note', v.note));
    if ((v.links || []).length) {
      var row = el('div', 'bv-acts');
      v.links.forEach(function (l) { var a = el('a', 'btn small', l.t); a.href = l.go; if (l.aria) { a.setAttribute('aria-label', l.aria); a.title = l.aria; } row.appendChild(a); });
      s.appendChild(row);
    }
    return s;
  }
  function faceCtx(id, title) { return { id: id, title: title, now: Math.floor(Date.now() / 1000), base: base, has: W.has, mine: !!known(id).act, mapped: !!known(id).plan, costs: document.body.dataset.costs === '1' }; }

  /* One line atop the body for what live refreshing has to say (WB-14, WB-15): it moved while held, or a refresh failed. A
     second one replaces the first. */
  function liveNote(text, act, fn) {
    var n = bodyEl.querySelector('.bv-livenote');
    if (!n) { n = el('div', 'bv-note bv-livenote'); n.setAttribute('role', 'status'); var meta = bodyEl.querySelector('.bv-meta'); bodyEl.insertBefore(n, meta ? meta.nextSibling : bodyEl.firstChild); }
    n.textContent = '';
    n.appendChild(el('span', '', text + ' '));
    if (act) { var b = button(act, fn); b.className = 'btn small'; n.appendChild(b); }
    return n;
  }
  function retryButton(id) { var b = button('Try again', function () { b.disabled = true; b.textContent = 'Loading…'; W.refresh('bead', id); }); b.className = 'btn small bv-retry'; return b; }

  function render(k, d, note, isErr, retry) {
    var x = d || {}, type = x.type || k.type || '', status = x.status || k.status || '';
    drawnPlan = !!(k.plan || k.seed);   // a seed too: its state pill reads the farm's word, not the bead's status
    panel.classList.toggle('doc', type === 'report');   // a report is a document: it reads wide on a desktop (garden.css .bv.doc)
    kindEl.textContent = kindWord(x, type);
    titleEl.textContent = x.title || k.title || k.id;
    var la = typeof ARMS !== 'undefined' ? ARMS.lineArms(x, k) : null;
    if (la) titleEl.insertBefore(la, titleEl.firstChild);
    bodyEl.textContent = '';
    var meta = el('div', 'bv-meta');
    meta.appendChild(idButton(k.id));
    if (status) meta.appendChild(el('span', 'pc', k.seed ? (k.staked ? 'staked out' : 'seed') : stateWord(status)));   // a plan not yet approved is no planted bead
    if (x.priority !== undefined) meta.appendChild(el('span', 'pc', 'P' + x.priority));
    if (x.project || k.project) meta.appendChild(el('span', 'pc', x.project || k.project));
    if (x.assignee && !(x.face && x.face.kind === 'wish' && x.assignee === x.created_by)) meta.appendChild(el('span', '', x.assignee));   // a wish's minting bridge holds nothing: its face says who asked
    var up = x.updated || k.updated;
    if (up) meta.appendChild(el('span', '', 'updated ' + ago(up) + ' ago'));
    bodyEl.appendChild(meta);
    var wh = x.kin && x.kin.head;   // a bead of a thread, not its head: the wish it is part of, up to the head's panel (gd-UXDp9GUeMB)
    if (wh) { var wp = bodyEl.appendChild(el('p', 'bv-wish', 'Part of wish ')), wa = wp.appendChild(el('a', 'bl', wh.short_title || wh.title || wh.id)); wa.href = href(wh.id); wa.title = (wh.title || wh.id) + ' (' + wh.id + ')'; }
    if (note) {   // a failed load says so over what the map knows (WB-15), never a blank panel
      var n = el('p', 'bv-note' + (isErr ? ' err' : ''), note + (isErr && !d ? ' ' + degraded(k) : ''));
      n.setAttribute('role', 'status'); bodyEl.appendChild(n); dress(n);
      if (isErr && retry) n.appendChild(retryButton(k.id));
    }
    if ((x.partial || []).length) {
      var pn = el('p', 'bv-note', 'The beads daemon did not send the ' + x.partial.join(' or ') + '. ');
      pn.appendChild(retryButton(k.id)); bodyEl.appendChild(pn);
    }
    actsEl = el('div', 'bv-live');
    bodyEl.appendChild(actsEl);
    writeEl = null;
    if (d && BW) {   // Edit, close/reopen/claim/release, Put an agent on this (web/beadwrite.js, WB-2, WB-4, WB-12); renderActs places it
      writeEl = el('div', 'bv-write');
      BW.mount(writeEl, d);
      if (!writeEl.firstChild) writeEl = null;
    }
    renderActs(k, x);
    if (!d) return;
    if (typeof TEND !== 'undefined') TEND.mount(bodyEl, d);   // "Send a gardener" on every bead (web/tend.js)
    if (typeof ADOPT !== 'undefined') ADOPT.mount(bodyEl, d, function () { W.reload('bead', d.id); });   // a loose bead: "Adopt into a plan" (web/adopt.js)

    var titles = docTitles(x);   // the titles this document carries, for the bead ids its text names (mentions: bead_mentions.go)
    RELS.forEach(function (r) { (x[r[0]] || []).forEach(function (b) { if (b.title) titles[b.id] = b.title; }); });
    var arms = typeof ARMS !== 'undefined' ? ARMS.section(x) : null;   // an epic's arms first: the picture and its name, then Change arms (web/arms.js)
    if (arms) bodyEl.appendChild(arms);
    if (x.harvest) bodyEl.appendChild(renderHarvest(x.harvest, titles));   // first on a closed step (harvest spec 6.2): the step's face
    var fv = x.harvest && x.face && x.face.kind === 'work' ? null : faceView(x, faceCtx(k.id, x.title || k.title));
    if (fv) bodyEl.appendChild(renderFace(fv, x.face.kind, titles));
    var desc = !x.description_is_context && x.description;   // a report's document, a decision's context: its face's body
    if (desc || !(fv && (fv.doc || x.face.kind === 'decision' || x.face.kind === 'wish'))) bodyEl.appendChild(sec('Description', desc ? rich('bv-text', desc, titles) : el('p', 'bv-empty', 'No description.')));
    if (x.notes) bodyEl.appendChild(sec('Notes', rich('bv-text', x.notes, titles)));
    var conv = BT && BT.talk(x);   // a bead in a thread: its conversation and its comments as one timeline, in place of the Comments section
    if (conv) bodyEl.appendChild(BT.render(x, conv, titles));
    var shown = shownIds(k.plan && typeof PLANS !== 'undefined' ? k.plan : null, fv, k.staked ? k.seed : null), work = BK && bodyEl.appendChild(el('div', 'bv-work'));
    sections(x, shown).forEach(function (s) {
      var ul = el('ul', 'bv-rels');
      s.list.forEach(function (r) {
        var name = r.short_title || r.title, li = el('li'), a = el('a', 'bl', name || r.id);   // the short title where it has one (gd-QTukIA8Nvv)
        a.href = href(r.id); li.appendChild(a); li.dataset.id = r.id;
        var bits = [];
        a.title = r.title ? r.title + ' (' + r.id + ')' : r.id;   // the title and the id in the tooltip, not the line
        if (name && typeof TITLES !== 'undefined') TITLES.learn(r.id, name);
        if (r.status) bits.push(stateWord(r.status));
        if (r.type && r.type !== 'task') bits.push(r.type);
        if (r.rel) bits.push(r.rel);
        if (r.project && r.project !== x.project) bits.push('in ' + r.project);   // a relation into another project (gd-CNhEh30Ykf)
        if (!r.title) bits.push('not visible to you');
        li.appendChild(el('span', 'sm', bits.join(' · ')));
        if (BW) BW.rel(li, s.key, r, x);   // Remove on a waits-on or holds-up row (WB-4)
        ul.appendChild(li);
      });
      bodyEl.appendChild(sec(s.title + ' (' + s.list.length + ')', ul, s.key));
    });
    if (x.rel_more) bodyEl.appendChild(el('p', 'bv-note', '+' + plural(x.rel_more, 'more linked bead is', 'more linked beads are') + ' not listed.'));
    var lk = BW && BW.links(x);   // Add a link: waits on, holds up, part of (WB-4)
    if (lk) bodyEl.appendChild(lk);
    if (work) BK.mount(work, x.id, { base: base, el: el, sec: sec, href: href, dress: dress, body: bodyEl, shown: shown });   // the lists above keep only what its tree does not draw

    if (!conv && BT) bodyEl.appendChild(BT.comments(k, x, titles));

    if ((x.fields || []).length || (x.labels || []).length || x.created_by) {
      var det = el('details', 'bv-fold'), dl = el('dl', 'bv-fields');
      det.appendChild(el('summary', '', 'Fields and labels'));
      var row = function (k2, v) { dl.appendChild(el('dt', '', k2)); dl.appendChild(el('dd', '', v)); };
      if (x.created_by) row('created by', x.created_by + (x.created ? ' · ' + ago(x.created) + ' ago' : ''));
      if (x.operated_by) row('operated by', x.operated_by);
      if (x.owner) row('owner', x.owner);
      if (x.closed) row('closed', x.closed);
      if ((x.labels || []).length) row('labels', x.labels.join(', '));
      (x.fields || []).forEach(function (f) { row(f.k, f.v); });
      det.appendChild(dl);
      if (x.fields_more) det.appendChild(el('p', 'bv-note', '+' + plural(x.fields_more, 'more field', 'more fields') + ' not shown.'));
      bodyEl.appendChild(det);
    }
    CUTS.all(bodyEl, meta);   // one Expand all for its cut texts, by its meta line (web/cuts.js, gd-Fmq3fgqFTm)
    dress(bodyEl);   // what is left: a weed's or an edge's bead, a field naming one, an adoption note
  }

  /* The bead the viewer has open is the farm's selection (web/chat.js names it in a gardener's thread as the context chip):
     garden:bead {id, title, project, type} when one opens and again once its document lands, {id: ''} when the panel closes. */
  function tell(id, k, d) {
    var x = d || {};
    k = k || {};
    try { document.dispatchEvent(new CustomEvent('garden:bead', { detail: { id: id, title: x.title || k.title || '', project: x.project || k.project || '', type: x.type || k.type || '' } })); }
    catch (e) { /* no CustomEvent: no chip */ }
  }
  /* ... and a click on a bead link zooms the map to it, as a cursor Enter or a sprite click does (titles, not ids:
     gd-9yOATV2qCs). A link on the map has moved the cursor already, and over the phone's Commons sheet the map is hidden
     ("Show on the map" flies). Two frames on, so a phone's Mailbox or Porch screen the open left has given the board its
     size. */
  function flyLater(id) {
    var raf = window.requestAnimationFrame || function (f) { return setTimeout(f, 16); };
    raf(function () { raf(function () { var g = G(); if (g && cur() === id && known(id).onMap) g.fly(id); }); });
  }
  function flyDeep(docIn) { if (flyOnDoc && docIn && typeof PLANS !== 'undefined' && PLANS.doc && PLANS.doc()) { flyOnDoc = false; flyLater(cur()); } }   // a deep link's flight once the farm's and the bead's documents are both in (docIn): flown sooner, its frames held the panel's LCP (gd-N5BigisFtS)
  W.register('bead', {
    noun: 'bead', url: function (id) { return '/garden/bead/' + encodeURIComponent(id) + (more[id] ? '?comments=' + more[id] : ''); },
    paint: function (id, d, note, isErr, retry) { failNote = isErr && !d ? { note: note, retry: retry } : null; render(known(id), d, note, isErr, retry); },
    shown: function (id) { tell(id, known(id), null); },
    done: function (id, d) { last = d; if (d) lastAt = Math.floor(Date.now() / 1000); flyDeep(true); },   // flyLater's two frames: after this paint
    live: function (id) { return '/garden/bead/' + encodeURIComponent(id) + '/events'; },   // live.go: "moved" when it moves (WB-14)
    part: function (id, ev) { return /"only":"comments"/.test(ev) && last && !last.partial ? '/garden/bead/' + encodeURIComponent(id) + '?after=' + last.comments_total : ''; },   // only comments added: fetch those (bead.go)
    merge: function (id, p) { var d = last && p.comments_total === last.comments_total + p.comments.length ? LIVE.patch(last, p) : null; if (d) d.comments = d.comments.slice(-Math.max(40, more[id] || 0)); return d; },   // null: fetch it whole
    hold: function () { return holding(bodyEl); },
    stale: function () { liveNote('This bead changed while you were writing here.', 'Show the latest', function () { W.refresh('bead', cur()); }); },
    failed: function (id, text, retry) {   // the document stays: say it could not refresh, and since when it is shown
      var n = liveNote(W.refreshFailure(text, 'bead') + ' It shows the bead as it was ' + (Date.now() / 1000 - lastAt >= 60 ? ago(lastAt) + ' ago' : 'a moment ago') + '.');
      n.classList.add('err');
      Array.prototype.forEach.call(bodyEl.querySelectorAll('.bv-retry'), function (b) { b.disabled = false; b.textContent = 'Try again'; });
      if (retry) n.appendChild(retryButton(id));
    },
    landed: function (id, d) { tell(id, known(id), d); },
    closed: function () { last = null; failNote = null; actsEl = null; writeEl = null; more = {}; if (BT) BT.reset(); if (BK) BK.reset(); tell('', null, null); },
    deep: function () {   // fly once the farm and the bead have landed (flyDeep), unless the link said where to look (as opened: LAZY.opened)
      flyOnDoc = param(typeof LAZY !== 'undefined' ? LAZY.opened : location.search, 'at') === null;
    },
    followed: function (id, a) { if (!a.closest('#board')) flyLater(id); }
  });

  /* The Porch chat (web/chat.js) asks for a bead it mentions with this event; taking it (preventDefault) keeps the page. */
  document.addEventListener('garden:open-bead', function (e) {
    var id = e.detail && e.detail.id;
    if (!id) return;
    e.preventDefault();
    W.open('bead', id);
  });
  /* The farm document decides what is on the map: refresh the panel's buttons, and fly to a deep-linked bead once it lands
     (unless the link also said where to look, ?at=). */
  document.addEventListener('farmdoc', function () {
    if (!cur()) return;
    // not over an answer in flight: an armed button, one sending, or a half-typed answer in the Mailbox row's copy
    if (actsEl && actsEl.querySelector('.act .armed, .act [disabled]')) return;
    var ta = actsEl && actsEl.querySelector('.act textarea');
    if (ta && ta.value) return;
    var k = known(cur());
    if (!last && failNote) render(k, null, failNote.note, true, failNote.retry);   // a failed load paints what the map knows, and the map just landed (WB-15)
    /* the document beat the farm to a plan: its face counted the phases and its sections listed them, which the map's phases
       list now shows, so the body is drawn again (once, as the plan lands on the map), unless the viewer is writing in it */
    else if (last && !!(k.plan || k.seed) !== drawnPlan && !holding(bodyEl)) redraw(k);
    else renderActs(k, last || {});
    /* two frames on, so the map's ResizeObserver has measured the board (a Commons band opened by the same deep link) and the
       flight frames the bead in the board as it is */
    flyDeep(!!last || !!failNote);
  });

  /* A deep-linked bead opens as this renderer registers, before the kinds after it (web/plan.js ...): a face drawn then
     lacks the links to those kinds (gd-CBMT713TEt). Once this bundle has run every kind has registered: draw it again. */
  var again = function () { if (cur() && last && !holding(bodyEl)) redraw(known(cur())); };
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', again); else setTimeout(again, 0);
  /* The shown document drawn again in place, its pictures wired anew (as the Workbench's load does after a paint). */
  function redraw(k) {
    render(k, last);
    if (typeof PICTURES !== 'undefined') PICTURES.wire(bodyEl);
  }

  /* A phone's keyboard (the visual viewport shorter than the window by more than 120 px): the bead view ends at its top edge,
     and a field in focus there is scrolled into view, with its form when the form fits. A desktop keeps its own bottom. */
  function keyboard() {
    var vv = window.visualViewport, bv = document.getElementById('bead'), sc = bodyEl;
    if (!vv || !bv || !sc) return;
    var kb = vv.height < window.innerHeight - 120 && window.innerWidth < 900;
    bv.style.bottom = kb ? Math.max(0, window.innerHeight - vv.offsetTop - vv.height) + 'px' : '';
    var f = document.activeElement, c = kb && bv.kbH !== vv.height && bv.contains(f) && f.matches('textarea,input,select') && (f.closest('.free,form') || f);
    if (c) (c.offsetHeight < sc.clientHeight ? c : f).scrollIntoView({ block: 'nearest' });
    bv.kbH = kb ? vv.height : 0;
  }
  if (window.visualViewport) { window.visualViewport.addEventListener('resize', keyboard); window.visualViewport.addEventListener('scroll', keyboard); }
  api.open = function (id, how) { W.open('bead', id, how); };   // how.replace: in place of the bead shown, with no history entry (garden.js answered())
  api.close = function () { W.close(); };   // whatever the panel shows
  /* Bead id's document d drawn as the panel draws it, into h's box (its doc class), title, kind and body: on a page with no
     panel, the Commons node's document (web/commons2.js, gd-0GnBi7SRfK), which reads d from url(id). */
  api.into = function (h, id, d) { panel = h.box; titleEl = h.title; kindEl = h.kind; bodyEl = h.body; last = d; render(known(id), d); };
  api.url = function (id) { return base + '/garden/bead/' + encodeURIComponent(id) + (more[id] ? '?comments=' + more[id] : ''); };
  api.current = function () { return cur(); };   // the bead the panel shows, '' when closed
  api.answerBox = answerBox;   // the Queues panel's rows carry the same box (web/queue.js)
  api.mdInto = function (parent, blocks, titles) { return mdInto(parent, blocks, titles || {}); };   // markdown() blocks built into parent (the bundle panel, web/bundle.js), titles as render()'s
  api.titles = docTitles;   // a document's titles map, for that mdInto
  return api;
})();
if (typeof module !== 'undefined') module.exports = BEAD;
