/* The bead view's Work and Open sections (gd-efoAiLlzRC): the Commons node's tree of the work under a bead (two levels, each
   row with its state, PR, open ask and jobs: internal/commons node.go) and its Open rail, the node fragment the Commons
   workbench draws (GET /garden/c/n/{id}?embed=1, X-Commons: fragment, as web/commons2.js reads it), embedded in the in-garden bead
   panel (web/workbench.js). Every row opens that bead's panel. Once the fragment is in, each row of the panel's own Children,
   Related, Waits on and Holds up lists that the fragment draws is hidden, so a bead is listed once; with the Commons off or
   the read failed, those lists stand as they are. */
var BEADWORK = (function () {
  /* ---------- pure logic (also run under node by the Go tests) ---------- */
  var DROPS = { children: 1, related: 1, blocked_by: 1, blocks: 1 };   // the relation lists a drawn row leaves; Part of stays
  var FRESH = 5000, RETRY = 30000;   // ms: a fragment younger is drawn as it is; a failed read is not tried again sooner
  /* Whether a relation row stays in the panel's list key: Part of always, any other unless the node draws its bead. */
  function keeps(key, id, drawn) { return !DROPS[key] || !drawn[id]; }
  /* The heading of a relation list with n of its all rows left: Children with some drawn above is Other children. */
  function heading(key, was, n, all) {
    var t = String(was).replace(/\s*\(\d+\)$/, '');
    return (n < all && key === 'children' ? 'Other children' : t) + ' (' + n + ')';
  }
  /* Whether m ({f, at}: the fragment last read, f null when the read failed) wants reading again at now. */
  function stale(m, now) { return !m || now - m.at > (m.f ? FRESH : RETRY); }

  var api = { keeps: keeps, heading: heading, stale: stale, reset: function () {}, mount: function () {}, node: function () {} };
  if (typeof document === 'undefined') return api;

  /* ---------- DOM ---------- */
  var memo = new Map(), pend = {}, last = null;   // by bead id: the fragment last read, and the read in flight; the last mount
  api.reset = function () { memo.clear(); pend = {}; last = null; };
  function put(id, f) { memo.delete(id); memo.set(id, { f: f, at: Date.now() }); if (memo.size > 24) memo.delete(memo.keys().next().value); return f; }
  function read(base, id) {
    return pend[id] || (pend[id] = fetch(base + '/garden/c/n/' + encodeURIComponent(id) + '?embed=1', { credentials: 'same-origin', cache: 'no-cache', headers: { 'X-Commons': 'fragment' } }).then(function (r) {
      if (!r.ok || r.status === 204) throw new Error('no node: ' + r.status);   // 204: the Commons holds none for it (embed=1, commons node.go)
      return r.text();
    }).then(function (t) {
      var tpl = document.createElement('template'), f;
      tpl.innerHTML = String(t).trim();   // the Commons' server markup, as commons2.js made() takes it
      f = tpl.content.querySelector('[data-commons="node"]');
      if (!f || f.dataset.id !== id) throw new Error('not the node of ' + id);
      return f;
    }).then(function (f) { delete pend[id]; return put(id, f); }, function () { delete pend[id]; return put(id, null); }));
  }

  /* Fill slot, an empty node the panel placed above its relation lists, with the node of bead id: at once from the fragment
     in memory, and again when a fresher one differs. h: the panel's {base, el, sec, href, dress, body, shown}, shown the ids
     it lists above already (a plan's phases, a staked plot's steps), which the tree leaves out. */
  api.mount = function (slot, id, h) {
    var m = memo.get(id);
    last = { slot: slot, id: id, h: h };
    if (m && m.f) draw(slot, m.f, h);
    if (stale(m, Date.now())) read(h.base, id).then(function (f) { if (f && slot.isConnected && !(m && m.f && m.f.outerHTML === f.outerHTML)) draw(slot, f, h); });
  };
  /* A bead under the mounted one moved (a Commons node event, n: commons stream.go Node, web/beadtalk.js listen): its Work row,
     drawn and in memory, takes its state and whether it is closed, as the workbench's tree rows do (commons2.js patch). A bead
     with no row drawn (new work) has the node read again, once its events pause. */
  api.node = function (n) {
    var at = last, m = at && memo.get(at.id), st = n && (n.agent_state || n.status), hit = false;
    if (!at || !at.slot.isConnected || !st) return;
    [at.slot, m && m.f].forEach(function (root, i) {
      var li = root && root.querySelector('li.cm-tn[data-id="' + CSS.escape(n.id) + '"]'), s = li && li.querySelector(':scope > .cm-tn-meta > .cm-state');
      if (!li) return;
      if (!i) hit = true;
      li.dataset.state = st; li.classList.toggle('cm-tn--closed', n.status === 'closed');
      if (s) { s.dataset.state = st; s.textContent = st.replace(/_/g, ' '); }
    });
    if (hit) return;
    clearTimeout(api.node.t);
    api.node.t = setTimeout(function () { var mm = memo.get(at.id); if (mm) mm.at = 0; if (last === at) api.mount(at.slot, at.id, at.h); }, 400);
  };
  function draw(slot, f, h) {
    var el = h.el, shown = h.shown || {}, drawn = {}, tree = f.querySelector('.cm-tree'), open = f.querySelector('.cm-node-open ul');
    tree = tree ? tree.cloneNode(true) : el('ol');
    open = open ? open.cloneNode(true) : null;
    Array.prototype.forEach.call(tree.querySelectorAll(':scope > li.cm-tn, :scope > li.cm-empty'), function (li) { if (!li.dataset.id || shown[li.dataset.id]) li.remove(); });
    Array.prototype.forEach.call(tree.querySelectorAll('li.cm-tn'), function (li) { drawn[li.dataset.id] = 1; });
    if (open) Array.prototype.forEach.call(open.querySelectorAll('li[data-id]'), function (li) { drawn[li.dataset.id] = 1; });
    slot.textContent = ''; slot.dataset.drawn = '1';
    var rows = tree.querySelectorAll(':scope > li.cm-tn').length, work = null;
    if (rows) { tree.className = 'bv-work-tree'; work = slot.appendChild(h.sec('Work (' + rows + ')', tree, 'work')); }
    if (open) {
      var done = open.querySelectorAll('li.cm-open--done').length, n = open.children.length;
      open.className = 'bv-work-open';
      slot.appendChild(h.sec('Open (' + (done ? (n - done) + ' open, ' + done + ' done' : n) + ')', open, 'open'));
    }
    Array.prototype.forEach.call(slot.querySelectorAll('a'), function (a) {
      var id = a.dataset.node || a.dataset.bead;
      if (id) {   // a row, its expand, its ask or blocker: that bead's panel, here
        a.href = h.href(id); a.removeAttribute('data-node');
        if (a.classList.contains('cm-tn-title')) a.classList.add('bl');
        if (a.classList.contains('cm-tn-blocked') && a.title) a.textContent = 'waits on ' + a.title;
      } else if (a.getAttribute('href')) { a.target = '_blank'; a.rel = 'noopener noreferrer'; }   // a pull request, out of the garden
    });
    h.dress(slot);
    // the panel's own lists: a row the node draws is hidden, a list left empty goes, and Children's place (the wish face's
    // link to it, goSec) passes to Work when it is gone
    var gone = false;
    Array.prototype.forEach.call(h.body.querySelectorAll('.bv-sec[data-rel], .bv-sec[data-sec]'), function (s) {
      var key = s.dataset.rel || s.dataset.sec, lis = s.querySelectorAll(':scope > ul.bv-rels > li[data-id]'), h3 = s.querySelector(':scope > h3');
      if (!DROPS[key] || !lis.length) return;
      s.dataset.rel = key;   // kept once its data-sec goes; any other section is left as drawn (the Commons node draws it too)
      var left = 0;
      Array.prototype.forEach.call(lis, function (li) { li.hidden = !keeps(key, li.dataset.id, drawn); if (!li.hidden) left++; });
      if (!s.dataset.h) s.dataset.h = h3.textContent;
      h3.textContent = heading(key, s.dataset.h, left, lis.length);
      s.hidden = !left;
      s.dataset.sec = left ? key : '';
      if (key === 'children' && !left) gone = true;
    });
    if (work && gone) work.dataset.sec = 'children';
  }
  return api;
})();
if (typeof module !== 'undefined') module.exports = BEADWORK;
