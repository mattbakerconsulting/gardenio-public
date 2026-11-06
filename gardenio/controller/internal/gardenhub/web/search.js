/* web/search.js: the search panel (Lands workbench-glass spec WB-5, step 5.7, gd-Oqh2suV8ap), the Workbench's q kind,
   /garden?q=<text>. It reads GET /garden/search?q=<text> (search.go: the viewer's own search, an exact id first, then the
   beads closest in meaning, or word matches when semantic search is off or fails, which the panel says), and lists the
   hits; each hit's title opens its bead panel here, and Back returns to the list.

   Its ways in: Cmd-K or Ctrl-K anywhere in the garden, even from a text field, and '/' on the map, both open the '/' jump
   (garden.js), whose last row is "Search every bead for ..." (the first when the map has nothing for it); and the field at
   the top of the panel, which searches again in place (the address follows, one entry for the panel). With the panel
   open, Cmd-K goes back to its field. Text goes in through textContent. */
var SEARCH = (function () {
  /* ---------- pure logic (also run under node by the Go tests) ---------- */
  /* The query's JSON under base. */
  function url(q) { return '/garden/search?q=' + encodeURIComponent(q); }
  function span(sec) {
    if (sec < 3600) return Math.max(1, Math.round(sec / 60)) + 'm';
    if (sec < 86400) return Math.round(sec / 3600) + 'h';
    return Math.round(sec / 86400) + 'd';
  }
  var STATUS = { open: 'open', in_progress: 'in progress', blocked: 'blocked', deferred: 'deferred', closed: 'closed' };
  /* The words under a hit's title: the id typed, what it is, its state, land, priority, when it last changed. Every state
     is a word (never colour alone). */
  function line(h, now) {
    var out = [];
    if (h.exact) out.push('the id you typed');
    var t = h.plan ? 'plan' : h.type || '';
    if (t) out.push(t);
    if (h.status) out.push(STATUS[h.status] || h.status.replace(/_/g, ' '));
    if (h.project) out.push(h.project);
    if (typeof h.priority === 'number' && h.type !== 'agent') out.push('P' + h.priority);
    if (h.updated > 0) out.push('changed ' + span(Math.max(60, now - h.updated)) + ' ago');
    return out;
  }
  /* The line over the hits: how they were found, or what could not be read (the reader's note says why there are only
     word matches). */
  function mode(d) {
    if (!d) return '';
    if (d.note) return d.note;
    if (!(d.hits || []).length) return '';
    return d.mode === 'semantic' ? 'Closest in meaning first.' : d.mode === 'lexical' ? 'Word matches.' : '';
  }
  function empty(q) { return 'No bead matches “' + q + '”.'; }
  /* Cmd-K or Ctrl-K, with no other modifier. */
  function isFind(e) { return !!e && (e.metaKey || e.ctrlKey) && !e.altKey && !e.shiftKey && String(e.key || '').toLowerCase() === 'k'; }
  var api = { url: url, line: line, mode: mode, empty: empty, isFind: isFind };
  if (typeof document === 'undefined' || typeof WORKBENCH === 'undefined') return api;

  /* ---------- DOM ---------- */
  var W = WORKBENCH, B = document.body, base = B.dataset.base || '';
  var titleEl = document.getElementById('bv-title'), kindEl = document.getElementById('bv-kind'), bodyEl = document.getElementById('bv-body');
  if (!bodyEl) return api;
  var formEl = null, inp = null, ownOpen = false;

  function el(t, cls, text) { var e = document.createElement(t); if (cls) e.className = cls; if (text !== undefined) e.textContent = text; return e; }
  function href(id) { return base + '/garden?bead=' + encodeURIComponent(id); }
  function cur() { var c = W.current(); return c && c.kind === 'q' ? c.id : ''; }

  /* The field stays across paints of the panel (the same element, kept when the rest of the body is replaced), so a search
     from it keeps the focus and the caret where they were. */
  function form() {
    if (formEl) return formEl;
    formEl = el('form', 'sv-form');
    formEl.setAttribute('role', 'search');
    inp = el('input');
    inp.type = 'search'; inp.name = 'q'; inp.maxLength = 200; inp.autocomplete = 'off'; inp.spellcheck = false;
    inp.setAttribute('aria-label', 'Search every bead'); inp.setAttribute('enterkeyhint', 'search');
    var go = el('button', 'btn small', 'Search'); go.type = 'submit';
    formEl.appendChild(inp); formEl.appendChild(go);
    formEl.addEventListener('submit', function (e) {
      e.preventDefault();
      var v = inp.value.replace(/\s+/g, ' ').trim(), q = cur();
      if (!v) return;
      if (v === q) { W.reload('q', q); return; }
      ownOpen = true;
      W.open('q', v, { replace: true });
    });
    /* typing never moves the map, and Escape in the field closes the panel as it does from anywhere in it */
    inp.addEventListener('keydown', function (e) { if (e.key !== 'Escape') e.stopPropagation(); });
    return formEl;
  }
  function hit(h, now) {
    var li = el('li', 'qv-row sv-row');
    li.dataset.id = h.id;
    var a = el('a', 'bl qv-t', h.short_title || h.title || h.id);
    a.href = href(h.id); a.title = (h.title || h.id) + ' (' + h.id + ')';
    if (h.title && typeof TITLES !== 'undefined') TITLES.learn(h.id, h.title);
    li.appendChild(a);
    li.appendChild(el('p', 'qv-line', line(h, now).join(' · ')));
    if (h.text) li.appendChild(el('p', 'bv-text qv-text', h.text));
    return li;
  }
  function paint(q, d, note, isErr, retry) {
    kindEl.textContent = 'search';
    titleEl.textContent = q;
    var f = form();
    if (f.parentNode === bodyEl) { while (f.nextSibling) bodyEl.removeChild(f.nextSibling); while (f.previousSibling) bodyEl.removeChild(f.previousSibling); }
    else { bodyEl.textContent = ''; bodyEl.appendChild(f); }
    if (document.activeElement !== inp) inp.value = q;
    var n = el('p', 'bv-note' + (isErr ? ' err' : ''), note || '');
    n.setAttribute('role', 'status');
    if (isErr && retry) {
      var b = el('button', 'btn small bv-retry', 'Try again'); b.type = 'button';
      b.onclick = function () { b.disabled = true; b.textContent = 'Loading…'; W.reload('q', q); };
      n.appendChild(document.createTextNode(' ')); n.appendChild(b);
    }
    bodyEl.appendChild(n);
    if (!d) return;
    var hits = d.hits || [], m = mode(d);
    if (m) bodyEl.appendChild(el('p', 'bv-note', m));
    if (!hits.length) { bodyEl.appendChild(el('p', 'bv-empty', empty(q))); return; }
    var now = Math.floor(Date.now() / 1000), list = el('ul', 'qv-list');
    list.setAttribute('aria-label', 'Beads matching ' + q);
    hits.forEach(function (h) { list.appendChild(hit(h, now)); });
    bodyEl.appendChild(list);
  }

  W.register('q', {
    noun: 'search',
    url: url,
    paint: paint,
    /* the query is no bead to load (gd-dv5wiNnKuh) */
    failure: function (status, q) { var f = W.failure(status, 'search', q); f.text = f.text.replace(/^Could not load this search/, 'Could not search for \u201c' + q + '\u201d'); return f; },
    /* a search from the field keeps the focus there; any other way in focuses the title, as every panel does */
    focus: function () { var mine = ownOpen; ownOpen = false; return mine ? inp : null; },
    closed: function () { formEl = null; inp = null; ownOpen = false; }
  });

  /* Cmd-K or Ctrl-K: the panel's field when it is open, else the '/' jump (its field again when it is up), from anywhere
     in the garden, a text field too: heard on the way down, before a field's own keys stop it. */
  document.addEventListener('keydown', function (e) {
    if (!isFind(e) || e.defaultPrevented) return;
    e.preventDefault();
    var jump = document.getElementById('jump'), jq = document.getElementById('jump-q');
    if (cur() && inp && document.body.contains(inp)) { inp.focus(); inp.select(); }
    else if (jump && !jump.hidden && jq) jq.focus();
    else if (window.GARDEN && GARDEN.find) GARDEN.find('');
  }, true);
  return api;
})();
if (typeof module !== 'undefined') module.exports = SEARCH;
