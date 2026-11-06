/* Titles, not ids (gd-9yOATV2qCs, Lands step 4b.1): a bead id the garden shows a person reads as the bead's title (or a
   short title where space is tight), and is a link that zooms the map to the bead and opens it in the bead view, the
   path a cursor Enter or a sprite click takes. The id stays the link's tooltip, for operators.
   One helper for every renderer. A title comes from the farm document (every plan, phase, task, seed, meadow bead and
   gardener it carries), from what the page already shows (window.GARDEN.known), from a title another document taught it
   (TITLES.learn: a bead view's relations, a Commons message's chip, a link dress() finds labelled with a title) or, for
   an id none of those know, from GET /garden/names, asked once per id in a batch after a dress (gd-QTukIA8Nvv). The
   short title is preferred wherever a document carries one. An id the garden cannot name stays the id, still a link.
   Links are /garden?bead=<id> like every other bead link on the page, so web/workbench.js's click handler opens them (and
   flies there); a new tab or a shared link opens the same bead. Text goes in through textContent only. */
var TITLES = (function () {
  /* ---------- pure logic (also run under node by the Go tests) ---------- */
  /* A bead id: two lowercase letters, a dash and ten letters or digits with at least one capital or digit, so
     "re-introduced" stays a word (web/workbench.js runs() uses the same rule). */
  var ID = /\b([a-z]{2}-(?=[A-Za-z0-9]{0,9}[A-Z0-9])[A-Za-z0-9]{10})\b/g;
  var URL_RE = /https?:\/\/[^\s<>"'`]+/g;
  var SHORT = 36;   // a short title, in characters: a chip, a toast, a label over the map
  function isId(s) { ID.lastIndex = 0; var m = ID.exec(String(s || '')); return !!m && m.index === 0 && m[0].length === String(s).length; }
  /* The title cut to n characters (graphemes as Array.from sees them) at a word where it can, with an ellipsis. A cut
     never leaves less than half of n. */
  function short(title, n) {
    var s = String(title == null ? '' : title).replace(/\s+/g, ' ').trim(), cs = Array.from(s);
    n = n || SHORT;
    if (cs.length <= n) return s;
    var cut = cs.slice(0, n - 1).join(''), sp = cut.lastIndexOf(' ');
    if (cs[n - 1] !== ' ' && sp >= Math.floor(n / 2)) cut = cut.slice(0, sp);   // the cut already ends a word
    return cut.replace(/[\s.,;:!?·—-]+$/, '') + '…';
  }
  /* What a person reads for id: its title (short when n is given), or the id when nothing knows it. */
  function label(id, title, n) { return title ? (n ? short(title, n) : String(title)) : String(id); }
  /* The tooltip: the whole title and the id, or the id alone. */
  function tip(id, title) { return title && title !== id ? title + ' (' + id + ')' : String(id); }
  /* Text cut into runs: plain text and bead ids. An id inside a URL stays part of the text (the URL is not ours to cut), and
     so does one glued to a '/', a path's last segment (agent/some-branch/gd-AbCdEfGhIj: web/workbench.js runs()'s rule). */
  function split(text) {
    text = String(text == null ? '' : text);
    var out = [], at = 0, urls = [], m;
    URL_RE.lastIndex = 0;
    while ((m = URL_RE.exec(text))) urls.push([m.index, m.index + m[0].length]);
    ID.lastIndex = 0;
    while ((m = ID.exec(text))) {
      var i = m.index, inURL = urls.some(function (u) { return i >= u[0] && i < u[1]; });
      if (inURL || text.charAt(i - 1) === '/' || text.charAt(i + m[1].length) === '/') continue;
      if (i > at) out.push({ k: 'text', s: text.slice(at, i) });
      out.push({ k: 'id', s: m[1] });
      at = i + m[1].length;
    }
    if (at < text.length) out.push({ k: 'text', s: text.slice(at) });
    return out;
  }
  function hasId(text) { return split(text).some(function (r) { return r.k === 'id'; }); }
  /* Every {id, title} the farm document carries, into idx (id -> name): plans, their phases and tasks, waiting decisions,
     seeds, the meadow and the harvested plans; a gardener by its agent name. The name is the short title when the record
     has one (gd-QTukIA8Nvv), else the title. Walked by shape, so a new list the document grows is read too. */
  function index(doc, idx) {
    idx = idx || {};
    (function walk(x, depth) {
      if (!x || typeof x !== 'object' || depth > 8) return;
      if (Array.isArray(x)) { for (var i = 0; i < x.length; i++) walk(x[i], depth + 1); return; }
      var id = x.id, st = typeof x.short_title === 'string' ? x.short_title.trim() : '';
      var t = st || (typeof x.title === 'string' && x.title ? x.title : typeof x.agent === 'string' && x.agent ? x.agent : '');
      if (typeof id === 'string' && t && t !== id && isId(id)) idx[id] = t;
      for (var k in x) if (Object.prototype.hasOwnProperty.call(x, k) && x[k] && typeof x[k] === 'object') walk(x[k], depth + 1);
    })(doc, 0);
    return idx;
  }

  /* What a person reads for a farm record (a plan, phase, task or closed step, a seed): its short title when it has one, else
     its title, else its id (gd-MpWy5X34hh). The title stays the tooltip's. */
  function name(x) {
    if (!x) return '';
    var st = typeof x.short_title === 'string' ? x.short_title.trim() : '';
    return st || (typeof x.title === 'string' && x.title ? x.title : String(x.id || ''));
  }
  /* The Plan › Phase › Task chain for id on the map, read from the layout index (WORLD's world.index) without a search: a
     task's cell names its plot (the plan) and its phase id, and the plan carries its phases. Each part is {kind, id, name,
     title}: name for the label (name()), title for the tooltip. A plot is [plan]; a task with no phase (the Other bed, a
     staked plot's steps) is [plan, task]; anything else is []. */
  function chain(index, id) {
    var it = index && index[id], out = [];
    if (!it) return out;
    var part = function (kind, x) { return { kind: kind, id: x.id, name: name(x), title: x.title || String(x.id) }; };
    if (it.kind === 'plot') return it.plan ? [part('plan', it.plan)] : out;
    if (it.kind !== 'task' || !it.task) return out;
    var plot = index[it.plot], plan = plot && plot.plan;
    if (plan) {
      out.push(part('plan', plan));
      var ph = it.phase ? (plan.phases || []).filter(function (p) { return p.id === it.phase; })[0] : null;
      if (ph) out.push(part('phase', ph));
    }
    out.push(part('task', it.task));
    return out;
  }

  var api = { ID: ID, SHORT: SHORT, isId: isId, short: short, label: label, tip: tip, split: split, hasId: hasId, index: index, name: name, chain: chain,
    title: function () { return ''; }, unseen: function () { return null; }, learn: function () {}, link: null, text: function (id) { return String(id); }, dress: function () {} };
  if (typeof document === 'undefined') return api;

  /* ---------- DOM ---------- */
  var base = (document.body && document.body.dataset.base) || '', known = {};
  function href(id) { return base + '/garden?bead=' + encodeURIComponent(id); }
  /* The title for id, or ''. The farm document's first, then what the page shows (the ledger, the map). */
  function title(id) {
    if (!id) return '';
    if (known[id]) return known[id];
    var g = window.GARDEN, k = g && g.known ? g.known(id) : null;
    return k && k.title && k.title !== id ? k.title : '';
  }
  function learn(id, t) { if (id && t && t !== id && isId(id)) known[id] = String(t); }
  /* What a person reads for id where the text cannot be a link (a button, a label drawn over the map): its title. */
  function text(id, n) { return label(id, title(id), n); }
  /* A link to id: its title (short with o.short, a number of characters or true for SHORT), the id in its tooltip, and
     data-bead for a copy or a probe. o.cls replaces the class (default bl); o.text is the title when the caller knows it. */
  function link(id, o) {
    o = o || {};
    var t = o.text && o.text !== id ? o.text : title(id), n = o.short === true ? SHORT : o.short || 0;
    if (o.text) learn(id, o.text);
    var a = document.createElement('a');
    a.className = o.cls || 'bl';
    a.href = href(id);
    a.textContent = label(id, t, n);
    a.title = tip(id, t);
    a.dataset.bead = id;
    return a;
  }
  /* Where an id is text, not a link: inside a link already, a button or a form control, or an element that lays itself
     out over the map (data-titles="text"). There it is swapped for its title in place. */
  var SKIP = { SCRIPT: 1, STYLE: 1, TEXTAREA: 1, INPUT: 1, SELECT: 1, OPTION: 1, CODE: 1, PRE: 1, KBD: 1, SAMP: 1 };
  function plainIn(el, root) {
    for (var e = el; e && e !== root.parentNode; e = e.parentNode) {
      if (e.nodeType !== 1) continue;
      if (e.tagName === 'A' || e.tagName === 'BUTTON' || e.tagName === 'SUMMARY' || e.getAttribute('role') === 'button' || e.dataset.titles === 'text') return true;
    }
    return false;
  }
  function skipped(el, root) {
    for (var e = el; e && e !== root.parentNode; e = e.parentNode) {
      if (e.nodeType !== 1) continue;
      if (SKIP[e.tagName] || e.isContentEditable || e.dataset.titles === 'off') return true;
    }
    return false;
  }
  /* Every bare id under root becomes its title: a link where a link can go, the title as text where one cannot. A link
     whose whole text is the id of the bead it opens reads as that bead's title, with the id in its tooltip. Idempotent,
     so a renderer calls it on what it just drew, as often as it draws. */
  function dress(root) {
    if (!root || !root.ownerDocument && root !== document) return;
    var links = root.querySelectorAll ? root.querySelectorAll('a[href]') : [];
    Array.prototype.forEach.call(links, function (a) {
      var id = a.dataset.bead || (typeof BEAD !== 'undefined' && BEAD.linkId ? BEAD.linkId(base, a.href, location.origin) : '');
      var t = a.textContent.trim();
      if (!id) return;
      if (t !== id) { if (a.classList.contains('cm-bead') && a.dataset.titles !== 'off') learn(id, t); return; }   // a chip the Commons titled (names.go) teaches the title
      var tt = title(id);
      if (!a.dataset.bead) a.dataset.bead = id;
      if (!tt) { if (!a.title) a.title = id; want(a, id); return; }
      a.textContent = tt;
      a.title = tip(id, tt);
    });
    var w = document.createTreeWalker(root, 4 /* NodeFilter.SHOW_TEXT */, null), nodes = [], n;
    while ((n = w.nextNode())) if (hasId(n.nodeValue)) nodes.push(n);
    nodes.forEach(function (tn) {
      var p = tn.parentNode;
      if (!p || skipped(p, root)) return;
      var plain = plainIn(p, root), frag = document.createDocumentFragment();
      split(tn.nodeValue).forEach(function (r) {
        if (r.k === 'text') frag.appendChild(document.createTextNode(r.s));
        else if (plain) {
          frag.appendChild(document.createTextNode(text(r.s)));
          var a = p.closest ? p.closest('a,button') : null;   // "Open <title>": the id stays in its tooltip
          if (a && !a.title && title(r.s)) a.title = r.s;
        }
        else frag.appendChild(link(r.s, { cls: 'bl tl' }));
      });
      p.replaceChild(frag, tn);
    });
  }

  /* The farm document (web/plans.js) carries the titles of everything on the map; a link drawn before it came, still
     reading as its id, takes its title now. */
  document.addEventListener('farmdoc', function (e) { index(e.detail, known); retitle(); });
  /* Every link on the page still reading as its id takes its title, now that something knows it. */
  function retitle() {
    Array.prototype.forEach.call(document.querySelectorAll('a[data-bead]'), function (a) { fix(a, a.dataset.bead); });
  }
  function fix(a, id) {
    var t = a.textContent === id ? title(id) : '';
    if (t) { a.textContent = t; a.title = tip(id, t); }
  }

  /* The names nothing on the page knows (gd-QTukIA8Nvv): the links dress() left reading as ids are asked of
     GET /garden/names in one batch after the dress (up to NAMES_MAX ids a call), once per id; what comes back is learned
     and every link that still reads as the id takes it, the batch's own links first (they may not be in the document
     yet). An id the garden does not name stays the id and is not asked again; a failed call is forgotten, so a later
     dress asks once more. */
  var NAMES_MAX = 40, asked = {}, pending = [], timer = null;
  function want(a, id) {
    if (asked[id] || !isId(id) || typeof fetch !== 'function') return;
    pending.push([a, id]);
    if (!timer) timer = setTimeout(flush, 0);
  }
  function flush() {
    timer = null;
    var batch = pending, ids = [], seen = {};
    pending = [];
    batch.forEach(function (p) {
      var id = p[1];
      if (title(id)) { fix(p[0], id); return; }   // learned since it was queued (the workbench's tree, a later fragment)
      if (!seen[id] && !asked[id]) { seen[id] = true; ids.push(id); }
    });
    while (ids.length) ask(ids.splice(0, NAMES_MAX), batch);
  }
  function ask(ids, batch) {
    ids.forEach(function (id) { asked[id] = true; });
    fetch(base + '/garden/names?' + ids.map(function (id) { return 'id=' + encodeURIComponent(id); }).join('&'),
      { credentials: 'same-origin', cache: 'no-store', headers: { Accept: 'application/json' } })
      .then(function (r) { return r.ok ? r.json() : Promise.reject(new Error(String(r.status))); })
      .then(function (d) {
        var names = d && d.names || {};
        ids.forEach(function (id) { if (typeof names[id] === 'string') learn(id, names[id]); });
        batch.forEach(function (p) { fix(p[0], p[1]); });
        retitle();
      }, function () { ids.forEach(function (id) { delete asked[id]; }); });
  }

  /* A link to an id with no known title gets a muted chip after it (gd-im6yFl7Nk1); null when the title is known. */
  function unseen(a, id, t) {
    if (!a || !id || (t && t !== id) || title(id)) return null;
    var s = document.createElement('span'), u = ' (not visible to you)';
    a.title = id + u;
    s.className = 'bv-unseen';
    s.textContent = u;
    if (a.parentNode) a.parentNode.insertBefore(s, a.nextSibling);
    return s;
  }

  api.title = title;
  api.unseen = unseen;
  api.learn = learn;
  api.text = text;
  api.link = link;
  api.href = href;
  api.dress = dress;
  api.retitle = retitle;
  return api;
})();
if (typeof module !== 'undefined') module.exports = TITLES;
