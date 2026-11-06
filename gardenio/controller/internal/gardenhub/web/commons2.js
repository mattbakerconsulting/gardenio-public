/* The Commons v2 workbench (plan gd-lRaCxsiLvC, steps 4-6) at /garden/c/n/{id}, its own page (web/node.html): the tree rail
   of any bead and its children, the selected node's document (the bead view's own, web/workbench.js BEAD.into: head, face,
   conversation and its composer, gd-0GnBi7SRfK), and the Jobs, Plan and Open rails. It shows internal/commons's fragments (node.go, render.go), revalidated
   by ETag; the stream (stream.go via web/live.js) patches rows, jobs and messages. Keys: j/k, Enter, u, Esc, /, Cmd-K,
   Shift+F10. Step 6 (gd-Txos7COlj0) keeps it instant: a send paints at once and the stream or the answer settles it, the
   fragments, trees and threads seen stay in memory (served stale and revalidated after a node event), a row is fetched on
   hover or focus before it is opened, drafts are kept per node, and unread marks come from the stream. Under 900 px (step 7,
   gd-DhGFLKK0tj) it is a push stack of two screens on the same fragments: the root's tree one level at a time, and a node
   with a Thread / Jobs / Plan / Open strip; a tap pushes, and Back (the bar's, the phone's) is always one level up. Steward
   view S2 (gd-LisW87zrnf): the land's steward is a root too, at /garden/c/s[/{land}] (commons/stewardnode.go): a fixed
   Steward row tops every tree (g s), every row's menu asks it about that row, and its document is its conversation in
   #general, its notes with Holds edited in place, and the Tending and Asks for you rails. On a phone (S3, gd-qV1nCdpbVA)
   the Steward row tops the tree screen and pushes the steward screen, its node with a Thread / Jobs / Tending / Asks strip
   and the plans it tends under Tending; it has no tree screen, so Back goes where it came from (the farm, or a root). */
var COMMONS2 = (function () {
  'use strict';
  /* ---------- pure logic (also run under node by the Go tests) ---------- */
  var ID = /^[a-z]{2,3}-[A-Za-z0-9-]{3,}$/;
  var BEAD_WAIT = 120;   // ms a root's tree waits past its fragment for the selected row's document read (reroot)
  /* The bead a path names under base, /garden/c/n/{id}, else ''. */
  function route(path, base) {
    var p = String(path || '');
    if (base && p.indexOf(base) === 0) p = p.slice(base.length);
    var m = /^\/garden\/c\/n\/([^/]+)\/?$/.exec(p);
    if (!m) return '';
    try { return decodeURIComponent(m[1]); } catch (e) { return m[1]; }
  }
  function href(base, id) { return base + '/garden/c/n/' + encodeURIComponent(id); }
  /* The land a steward's address names under base: /garden/c/s/{land} its land, /garden/c/s '' (the viewer's), else null. */
  function stewardRoute(path, base) {
    var p = String(path || '');
    if (base && p.indexOf(base) === 0) p = p.slice(base.length);
    var m = /^\/garden\/c\/s(?:\/([^/]+))?\/?$/.exec(p);
    if (!m) return null;
    try { return decodeURIComponent(m[1] || ''); } catch (e) { return m[1]; }
  }
  function stewardHref(base, land) { return base + '/garden/c/s' + (land ? '/' + encodeURIComponent(land) : ''); }
  /* The draft "Ask the steward about this" opens: @steward, and the row by its title and id. */
  function askDraft(title, id) { return '@steward about ' + (title && title !== id ? title + ' (' + id + ')' : id) + ': '; }
  /* What the steward's composer sends: the words, @steward first unless they name it already (the home garden's steward
     wakes on @steward only, gd-jNaTmCjity). */
  function stewardText(t) { t = String(t || '').trim(); return !t || /(^|[^\w@])@steward\b/i.test(t) ? t : '@steward ' + t; }
  /* The Holds line that would open a section of its own, which the save refuses (land_notes.go), or ''. */
  function badHold(t) { return String(t || '').split('\n').filter(function (l) { return /^## /.test(l.replace(/[ \t\r]+$/, '')); })[0] || ''; }
  /* rows: [{id, title, up}] in tree order (up: the index of its parent row, -1 for the root). The indices of the rows q
     matches (title or id, any case) and of every row above one, so a match keeps its path; all when q is blank. */
  function found(rows, q) {
    q = String(q || '').trim().toLowerCase();
    var keep = {};
    rows.forEach(function (r, i) {
      if (q && !hit(r, q)) return;
      for (var j = i; j >= 0 && !keep[j]; j = rows[j].up) keep[j] = true;
    });
    return rows.map(function (r, i) { return i; }).filter(function (i) { return keep[i]; });
  }
  function hit(r, q) { return (r.title + ' ' + r.id).toLowerCase().indexOf(String(q).trim().toLowerCase()) >= 0; }
  /* The Plan rail of row i: its parent's children with i among them; the root's children for the root. */
  function siblings(rows, i) {
    var up = i <= 0 ? 0 : rows[i].up;
    return rows.map(function (r, j) { return j; }).filter(function (j) { return j > 0 && rows[j].up === up; });
  }
  /* The agent that holds row i: itself when it is one, else its first child agent at work, else its first child agent. */
  function holder(rows, i) {
    if (!rows[i]) return '';
    if (rows[i].type === 'agent') return rows[i].id;
    var kids = rows.filter(function (r) { return r.up === i && r.type === 'agent'; });
    var busy = kids.filter(function (r) { return !/^(done|closed|dead|failed)$/.test(r.state); });
    return (busy[0] || kids[0] || { id: '' }).id;
  }
  /* The agents at work anywhere under row i (a wish's crews hang off the tasks it gave rise to), by row index. */
  function busyUnder(rows, i) {
    var under = function (j) { for (j = rows[j].up; j >= 0; j = rows[j] ? rows[j].up : -1) if (j === i) return true; return false; };
    return rows.map(function (r, j) { return j; }).filter(function (j) { return j > i && rows[j].type === 'agent' && !/^(done|closed|dead|failed)$/.test(rows[j].state) && under(j); });
  }
  /* The next row from i, by step, among the visible indices vis, held at the ends. */
  function step(vis, i, by) {
    var at = vis.indexOf(i);
    if (at < 0) return vis.length ? vis[0] : -1;
    return vis[Math.max(0, Math.min(vis.length - 1, at + by))];
  }
  /* Whether row i opens one level down on a phone rather than as a node: work under it (more than its agents, who hold it
     and show in its Jobs), or rows below it the fragment did not send. */
  function down(rows, i) { return i > 0 && !!rows[i] && (!!rows[i].more || rows.some(function (r) { return r.up === i && r.type !== 'agent'; })); }
  /* A server time in the reader's zone: {date: "Oct 14", hm: "18:03"}, or null (web/commons.js clock). */
  var MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
  function clock(stamp) {
    var d = /^\d{4}-\d{2}-\d{2}T/.test(String(stamp || '')) ? new Date(stamp) : null, p = function (n) { return (n < 10 ? '0' : '') + n; };
    return d && !isNaN(d.getTime()) ? { date: MONTHS[d.getMonth()] + ' ' + d.getDate(), hm: p(d.getHours()) + ':' + p(d.getMinutes()) } : null;
  }
  /* the gardener a .cm-node-reason (commons node.go) names, as web/wake.js reads one */
  function wakeOf(d, words) { return { id: d.id || '', agent: d.agent || '', task_title: d.task || '', reason: d.reason ? { kind: d.reason, text: words || '' } : null }; }
  var api = { route: route, href: href, found: found, hit: hit, clock: clock, siblings: siblings, holder: holder, step: step, down: down, ID: ID,
    stewardRoute: stewardRoute, stewardHref: stewardHref, askDraft: askDraft, stewardText: stewardText, badHold: badHold, wakeOf: wakeOf };
  if (typeof document === 'undefined') return api;

  /* ---------- the view ---------- */
  var B = document.body, base = B.dataset.base || '';
  /* root: the bead the tree is opened on; rows: its tree, flat; sel: the selected row's index; frags: each node's
     fragment by id, the least recently used first (stale: to revalidate on its next use; pend: in flight); trees: each
     root's drawn tree; threads: each thread's section by id; doc: the node the document shows, drawn from docF; thread:
     its thread ({land, channel, id, el}); drafts and unread: by node id; es: the stream, at: its last event id; mine: the
     ids of the messages sent from this page; seen: of those the stream brought. sw: each land's steward trigger, swOf the
     land of each; about: the bead a steward draft asks about, by node; talk: the steward's #general on screen; held: each
     land's Holds as last saved here; wake: each agent's Wake / Restart step (web/wake.js), by node; at: the thread an address asked for (?thread=);
     beads: each bead's document (GET /garden/bead/{id}) by id, bpend: its read in flight, bstale: to read again on its next
     use (a stream event came), bjs: the bead view's bundle loading. */
  var S = { root: '', rows: [], sel: -1, frags: new Map(), stale: {}, pend: {}, trees: new Map(), threads: new Map(), doc: '', docF: null,
    thread: null, drafts: {}, unread: {}, hold: '', es: null, spot: '', at: '', mine: {}, seen: {}, mode: 'find', gen: 0,
    sw: {}, swOf: {}, about: {}, talk: null, held: {}, wake: {}, here: new URLSearchParams(location.search).get('thread') || '', beads: new Map(), bpend: {}, bstale: {}, bjs: null };
  /* put v under k in an LRU map of at most n; out gets the one put out */
  function lru(m, k, v, n, out) {
    m.delete(k); m.set(k, v);
    if (m.size > n) { var k0 = m.keys().next().value; if (out) out(m.get(k0)); m.delete(k0); }
    return v;
  }
  function el(tag, cls, text) { var e = document.createElement(tag); if (cls) e.className = cls; if (text !== undefined) e.textContent = text; return e; }
  var view = el('div', 'wb2'), bar = el('header', 'wb2-bar'), tree = newTree(), docEl = el('article', 'wb2-doc');
  var rails = el('aside', 'wb2-rails'), menu = el('ul', 'wb2-menu'), live = el('p', 'sr'), find = el('input', 'wb2-find');
  view.id = 'wb2'; view.setAttribute('role', 'region'); view.setAttribute('aria-label', 'Workbench');
  live.setAttribute('role', 'status');
  var tabs = el('nav', 'wb2-tabs'), farmBtn = el('button', 'wb2-tab', 'Farm'), here = el('span', 'wb2-tab', 'Workbench');
  farmBtn.type = 'button'; farmBtn.setAttribute('aria-keyshortcuts', 'Escape'); farmBtn.title = 'Back to the farm (Esc)';
  here.setAttribute('aria-current', 'page'); tabs.setAttribute('aria-label', 'Screens');
  tabs.append(farmBtn, here);
  var crumbs = el('nav', 'wb2-crumbs'); crumbs.setAttribute('aria-label', 'Where this is');
  find.type = 'search'; find.autocomplete = 'off'; find.spellcheck = false;
  find.setAttribute('aria-label', 'Find in the tree, or jump to a bead id'); find.setAttribute('aria-keyshortcuts', '/ Control+K Meta+K');
  find.placeholder = 'Find (/) or jump to an id (Ctrl-K)';
  var back = el('button', 'wb2-back'), backT = back.appendChild(el('span')), title = el('h1', 'wb2-title'), strip = el('nav', 'wb2-strip');
  back.type = 'button'; strip.setAttribute('role', 'tablist'); strip.setAttribute('aria-label', 'The node');
  bar.append(back, title, tabs, crumbs, find);
  var treeBox = el('nav', 'wb2-col wb2-treecol'); treeBox.setAttribute('aria-label', 'Tree');
  /* the fixed Steward row over every tree: the root's land's steward, one click (or g s) away */
  var swRow = el('a', 'wb2-swrow');
  swRow.append(dot(''), el('span', 'wb2-swname', 'Steward'), el('span', 'wb2-swnote', 'Ask it, or see what it tends'));
  swRow.href = stewardHref(base, ''); swRow.setAttribute('aria-keyshortcuts', 'g s'); swRow.title = 'The land’s steward (g s)';
  treeBox.append(swRow, tree);
  /* a root's tree; each keeps its own row ids, as the trees kept beside it are in the page too */
  function newTree() {
    var t = el('ul', 'wb2-tree');
    t.setAttribute('role', 'tree'); t.tabIndex = 0; t.setAttribute('aria-label', 'The tree: j and k move, Enter opens here, u goes up');
    t.dataset.n = S.tn = (S.tn || 0) + 1;
    return t;
  }
  docEl.className = 'wb2-col wb2-doc'; docEl.setAttribute('aria-label', 'The node');
  rails.className = 'wb2-col wb2-rails'; rails.setAttribute('aria-label', 'Jobs, plan and what is open');
  var railJobs = rail('Jobs', 'jobs'), railPlan = rail('Plan', 'plan'), railOpen = rail('Open', 'open');
  menu.setAttribute('role', 'menu'); menu.hidden = true;
  var cols = el('div', 'wb2-cols'); cols.append(treeBox, docEl, rails);
  [['Thread', 'thread', docEl], ['Jobs', 'jobs', railJobs], ['Plan', 'plan', railPlan], ['Open', 'open', railOpen]].forEach(function (t) {
    var b = strip.appendChild(el('button', 'wb2-stab', t[0]));
    b.type = 'button'; b.setAttribute('role', 'tab'); b.dataset.tab = t[1]; b.setAttribute('aria-controls', t[2].id = 'wb2-' + t[1]);
  });
  view.append(bar, strip, cols, menu, live);
  view.dataset.screen = 'tree';
  function rail(name, key) {
    var s = el('section', 'wb2-rail'), h = el('h3', 'wb2-rail-h', name);
    s.dataset.rail = key; s.append(h, el('div', 'wb2-rail-b'));
    rails.appendChild(s);
    return s;
  }
  function railBody(r, title) { if (title) r.firstChild.textContent = title; var b = r.lastChild; b.textContent = ''; return b; }
  function say(t) { live.textContent = t; }

  /* server markup made ready to insert, its bead ids read as titles (web/titles.js) and its times in the reader's zone */
  function made(t) {
    var tpl = document.createElement('template'); tpl.innerHTML = String(t).trim();
    if (typeof TITLES !== 'undefined') TITLES.dress(tpl.content);
    Array.prototype.forEach.call(tpl.content.querySelectorAll('time[datetime] .cm-date'), function (d) {
      var c = clock(d.parentNode.getAttribute('datetime'));
      if (!c) return;
      d.textContent = c.date;
      while (d.nextSibling) d.parentNode.removeChild(d.nextSibling);
      d.parentNode.appendChild(document.createTextNode(' ' + c.hm));
    });
    return tpl.content;
  }
  var WHY = { 401: 'Your session ended. Reload the page to sign in again.', 403: 'You may not see that.', 404: 'That bead is not here, or not in a land you can see.' };
  function get(url, json) {
    return fetch(url, { credentials: 'same-origin', cache: 'no-cache', headers: json ? { Accept: 'application/json' } : { 'X-Commons': 'fragment' } }).then(function (r) {
      return r.text().then(function (t) {
        if (!r.ok) { var e = new Error(WHY[r.status] || (r.status < 500 && t.trim()) || 'The garden did not answer (' + r.status + ').'); e.status = r.status; throw e; }
        return json ? JSON.parse(t) : made(t).firstElementChild;
      });
    }, function () { throw new Error('The garden could not be reached.'); });
  }
  /* a node's fragment: the one in memory (revalidated after, when a node event made it stale), else fetched; fresh: fetched */
  function node(id, fresh) {
    var f = S.frags.get(id);
    if (!f || fresh) return fetchNode(id);
    lru(S.frags, id, f, 48);
    if (S.stale[id]) revalidate(id, f);
    return Promise.resolve(f);
  }
  function fetchNode(id) {
    return S.pend[id] || (S.pend[id] = get(href(base, id)).then(function (f) {
      delete S.pend[id]; delete S.stale[id];
      if (f.dataset.kind === 'steward') { S.swOf[id] = f.dataset.land || ''; S.sw[S.swOf[id]] = id; }
      return lru(S.frags, id, f, 48);
    }, function (e) { delete S.pend[id]; throw e; }));
  }
  /* A stale fragment fetched again: unchanged, the one in memory stays (and the views drawn from it); changed, the tree or
     the document showing it is drawn again, the document only while nobody is writing in it. */
  function revalidate(id, was) {
    fetchNode(id).then(function (f) {
      if (f.outerHTML === was.outerHTML) { lru(S.frags, id, was, 48); return; }
      if (S.talk && id === S.doc && docEl.contains(document.activeElement) && swPatch(f)) { S.docF = f; return; }   // typing to the steward: only its conversation moves
      if (id === S.root) rooted(f, S.rows[S.sel] && S.rows[S.sel].id, true);
      else if (id === S.doc && !docEl.contains(document.activeElement)) show(S.sel);
    }, function () { /* the stale one stays */ });
  }
  /* id's fragment and its document on the wire now, before the page is touched for its open, which then waits on the same
     requests (B14, gd-50NuE93HTf): the document no longer comes in the fragment, so it is not a second round trip after it */
  function fetchFirst(id) {
    if (!S.frags.has(id)) fetchNode(id).then(null, function () { /* the open says why */ });
    if (!S.beads.has(id) && S.swOf[id] === undefined) beadRead(id).then(null, function () { /* the open says why */ });
  }
  /* the row's fragment fetched before it is opened (hover, focus, the next row by key), once */
  function warm(id) {
    if (!id || S.frags.has(id) || S.pend[id] || warm.id === id) return;
    clearTimeout(warm.t); warm.id = id;
    warm.t = setTimeout(function () {
      warm.id = '';
      if (!S.frags.has(id)) fetchNode(id).then(null, function () { /* opened later, it says why */ });
      if (!S.beads.has(id) && S.swOf[id] === undefined) beadRead(id).then(null, function () { /* opened later, it says why */ });   // its document too
    }, 60);
  }

  /* ---------- the tree ---------- */
  /* The root's rows, flat in tree order: the root, then its children to the depth the fragment sends. */
  function rowsOf(f) {
    var head = f.querySelector('.cm-node-head'), th = head.querySelector('.cm-node-thread');
    var rows = [{ id: f.dataset.id, title: text(head.querySelector('.cm-node-title')), type: f.dataset.kind, state: f.dataset.state, up: -1, meta: head.querySelector('.cm-tn-meta'), lvl: 1, thread: th ? th.dataset.thread : '' }];
    (function walk(ol, up, lvl) {
      Array.prototype.forEach.call(ol ? ol.children : [], function (li) {
        if (!li.classList.contains('cm-tn')) {
          if (li.classList.contains('cm-tn-more')) rows.push({ more: text(li), up: up, lvl: lvl, id: '', title: '' });
          return;
        }
        var t = li.querySelector(':scope > .cm-tn-title'), i = rows.length;
        rows.push({ id: li.dataset.id, title: text(t), type: li.dataset.type, state: li.dataset.state, up: up, lvl: lvl, closed: li.classList.contains('cm-tn--closed'), meta: li.querySelector(':scope > .cm-tn-meta'), more: text(li.querySelector(':scope > .cm-tn-more')), thread: li.dataset.thread || '', flag: li.dataset.flag || '' });
        walk(li.querySelector(':scope > ol'), i, lvl + 1);
      });
    })(f.querySelector('.cm-tree'), 0, 2);
    if (typeof TITLES !== 'undefined') rows.forEach(function (r) { if (r.id && r.title) TITLES.learn(r.id, r.title); });   // the tree's names, for every id the page shows as text (gd-QTukIA8Nvv)
    return rows;
  }
  function dot(st) { var d = el('span', 'wb2-dot'); d.dataset.state = st || ''; d.setAttribute('aria-hidden', 'true'); return d; }
  function text(e) { return e ? e.textContent.trim() : ''; }
  /* a row's meta as the tree shows it: no tab stops (the tree is one), and a blocker by its title rather than its id */
  function meta(m) {
    m = m.cloneNode(true);
    Array.prototype.forEach.call(m.querySelectorAll('a'), function (x) { x.tabIndex = -1; if (x.classList.contains('cm-tn-blocked') && x.title) x.textContent = 'waits on ' + x.title; });
    return m;
  }
  function rowId(i) { return 'wb2-r' + tree.dataset.n + '-' + i; }
  /* The tree on screen kept for its root, in the page with content-visibility:hidden so its layout is kept too, and a new
     one put in its place; a kept tree put out of memory leaves the page. */
  function stash() {
    if (S.treeF && S.rows[0]) {
      var had = tree.contains(document.activeElement), was = S.trees.get(S.rows[0].id);
      if (was && was.ul !== tree) was.ul.remove();
      lru(S.trees, S.rows[0].id, { f: S.treeF, rows: S.rows, ul: tree, crumbs: [].slice.call(crumbs.childNodes) }, 8, function (o) { o.ul.remove(); });
      showTree(tree, false);
      tree = treeBox.appendChild(newTree());
      if (had) tree.focus({ preventScroll: true });
    }
    S.treeF = null;
  }
  /* a tree shown, or kept: inert, hidden, and with no row selected, so the page has one selection */
  function showTree(t, on) {
    t.classList.toggle('wb2-tree--kept', !on); t.inert = !on;
    if (on) { t.removeAttribute('aria-hidden'); return; }
    t.setAttribute('aria-hidden', 'true');
    Array.prototype.forEach.call(t.querySelectorAll('[aria-selected=true]'), function (e) { e.setAttribute('aria-selected', 'false'); });
  }
  function dropTree(k) { var t = S.trees.get(k); if (t && t.ul !== tree) t.ul.remove(); S.trees.delete(k); }
  /* On a phone the rows past a screenful (75 px each, garden.css) are drawn a frame after the tree, so its open lays out and
     paints only the rows it shows (B14, gd-50NuE93HTf); a row selected before then lets them all in at once. */
  var later = [];
  function letIn() { later.forEach(function (li) { li.classList.remove('wb2-row--later'); }); later = []; }
  /* The tree of root fragment f: the one kept from f, shown again where it is, else drawn now. */
  function drawTree(f) {
    if (S.rows[0] && S.rows[0].id === f.dataset.id) S.treeF = null;   // the same root drawn again, in place
    else stash();
    S.treeF = f;
    var v = S.trees.get(f.dataset.id);
    if (v && v.f === f) {
      var had = document.activeElement === tree;
      tree.remove(); tree = v.ul; showTree(tree, true); S.trees.delete(f.dataset.id);
      if (had) tree.focus({ preventScroll: true });
      S.rows = v.rows; crumbs.replaceChildren.apply(crumbs, v.crumbs);
      S.rows.forEach(function (r, i) { if (r.id) unread(i, S.unread[r.id]); });
      if (find.value || tree.querySelector('.wb2-row[hidden]')) filter(find.value);
      return;
    }
    var rows = S.rows = rowsOf(f), up = f.querySelector('.cm-node-up');
    tree.textContent = ''; crumbs.textContent = '';
    if (up) {
      var u = el('li', 'wb2-up'), b = el('a', 'wb2-uplink');
      b.href = href(base, up.dataset.node); b.dataset.open = up.dataset.node;
      b.append(el('span', 'wb2-glyph'), document.createTextNode(' Up: '), el('span', 'wb2-uptitle', text(up)));
      b.setAttribute('aria-keyshortcuts', 'u'); b.tabIndex = -1;
      u.setAttribute('role', 'none'); u.appendChild(b); tree.appendChild(u);
      var c = el('a', 'wb2-crumb', text(up)); c.href = b.href; c.dataset.open = up.dataset.node;
      crumbs.append(c, el('span', 'wb2-sep', '›'));
    }
    crumbs.appendChild(el('span', 'wb2-crumb wb2-crumb--here', rows[0].title)).setAttribute('aria-current', 'location');
    // the rows built off the page and put in at once, so a long tree costs the page one insert (B14, gd-50NuE93HTf)
    var box = document.createDocumentFragment(), groups = [box], pre = rowId(''), under = {};   // groups[n]: where the rows of level n + 1 go
    var shown = phone() ? Math.ceil(innerHeight / 75) + 1 : rows.length;
    letIn();
    rows.forEach(function (r) { if (r.type !== 'agent') under[r.up] = true; });   // down(), for every row in one pass
    rows.forEach(function (r, i) {
      var li = el('li', 'wb2-row' + (r.closed ? ' wb2-row--closed' : '') + (r.id ? '' : ' wb2-row--more'));
      li.id = pre + i; li.dataset.i = i;
      if (i > shown) { li.classList.add('wb2-row--later'); later.push(li); }
      groups[r.lvl - 1].appendChild(li);
      if (!r.id) { li.setAttribute('role', 'none'); li.appendChild(el('span', 'wb2-moretxt', r.more)); return; }
      li.setAttribute('role', 'treeitem'); li.setAttribute('aria-level', r.lvl); li.setAttribute('aria-selected', 'false');
      li.dataset.id = r.id; li.dataset.type = r.type || ''; li.dataset.state = r.state || '';
      if (i > 0 && (r.more || under[i])) li.dataset.down = '1';
      var line = el('div', 'wb2-line'), a = el('a', 'wb2-t', r.title);
      a.href = href(base, r.id); a.tabIndex = -1;
      line.append(dot(r.state), a);
      var m = r.meta ? meta(r.meta) : el('span', 'cm-tn-meta');
      if (r.more) m.appendChild(el('span', 'wb2-below', r.more + ' below'));
      if (r.flag) m.appendChild(el('span', 'wb2-flag', r.flag)).dataset.flag = r.flag;   // a steward's plan stalled, a step parked
      line.appendChild(m);
      var mb = el('button', 'wb2-more', '···'); mb.type = 'button'; mb.tabIndex = -1;
      mb.setAttribute('aria-haspopup', 'menu'); mb.setAttribute('aria-label', 'Actions for ' + r.title);
      line.appendChild(mb); li.appendChild(line);
      var g = el('ul', 'wb2-group'); g.setAttribute('role', 'group'); li.appendChild(g);
      groups[r.lvl] = g;
    });
    Array.prototype.forEach.call(box.querySelectorAll('ul[role=group]'), function (g) { if (!g.children.length) g.remove(); else g.parentNode.setAttribute('aria-expanded', 'true'); });
    tree.appendChild(box);
    if (later.length) requestAnimationFrame(function () { setTimeout(letIn, 0); });   // after the frame that shows the tree
    rows.forEach(function (r, i) { if (S.unread[r.id]) unread(i, true); });
    find.value && filter(find.value);
  }
  function rowEl(i) { return document.getElementById(rowId(i)); }
  /* row i marked (on) or not as having messages since it was last open */
  function unread(i, on) {
    var e = rowEl(i), m = e && e.querySelector('.wb2-line > .cm-tn-meta'), n = m && m.querySelector('.wb2-new');
    if (!m || !on === !n) return;
    if (on) m.appendChild(el('span', 'wb2-new', 'new')); else n.remove();
  }
  function visible() { return S.rows.map(function (r, i) { return i; }).filter(function (i) { var e = rowEl(i); return S.rows[i].id && e && !e.hidden; }); }
  function select(i, quiet) {
    var r = S.rows[i];
    if (!r || !r.id) return;
    var was = rowEl(S.sel);
    if (was) was.setAttribute('aria-selected', 'false');
    S.sel = i;
    var e = rowEl(i);
    if (e.classList.contains('wb2-row--later')) letIn();
    e.setAttribute('aria-selected', 'true'); tree.setAttribute('aria-activedescendant', e.id);
    // scrolled in the next frame, whose layout it then shares, rather than laid out once more now
    if (i > 0) requestAnimationFrame(function () { if (e.isConnected) e.firstChild.scrollIntoView({ block: 'nearest' }); });
    if (history.replaceState) try { history.replaceState(Object.assign({}, history.state, { wb2: S.root, sel: r.id }), '', location.href); } catch (x) { /* sandboxed */ }
    show(i); head();
    if (!quiet) say(r.title + (r.state ? ', ' + r.state.replace(/_/g, ' ') : ''));
  }

  /* ---------- the document and the rails ---------- */
  function show(i) {
    var r = S.rows[i], gen = ++S.gen, bd = r.type === 'steward' || (S.beads.has(r.id) && !S.bstale[r.id]) ? null : beadRead(r.id);   // on the wire beside the fragment (B14)
    drawPlan(i);
    if (S.unread[r.id]) { delete S.unread[r.id]; unread(i, false); }
    keep();
    if (S.doc !== r.id && !S.frags.has(r.id)) { docEl.textContent = ''; docEl.appendChild(el('p', 'wb2-note', 'Loading ' + r.title + '…')); docEl.setAttribute('aria-busy', 'true'); }
    node(r.id).then(function (f) {
      if (gen !== S.gen) return;
      if (S.doc === r.id && S.docF === f && docEl.querySelector('.wb2-head')) { connect(); return; }   // on screen already (Open here)
      S.doc = r.id; S.docF = f;
      var sw = f.dataset.kind === 'steward', run = sw && f.querySelector('.cm-sw-watch');
      if (sw) drawDoc(f); else beadDoc(f, gen, bd);
      drawOpen(f);
      var h = f.dataset.kind === 'agent' ? r.id : sw ? run ? run.dataset.node : '' : holder(f === S.treeF ? S.rows : rowsOf(f), 0) || holder(S.rows, i);   // its own fragment has its agents
      drawJobs(h, gen, f, h ? [] : busyUnder(S.rows, i));
      transcript(f, gen);
    }, function (err) {
      if (gen !== S.gen) return;
      S.doc = ''; S.docF = null;
      docEl.removeAttribute('aria-busy'); docEl.textContent = '';
      var p = el('p', 'wb2-err', 'Could not open ' + r.title + '. ' + err.message); p.setAttribute('role', 'alert');
      var again = el('button', 'btn small', 'Try again'); again.type = 'button';
      again.addEventListener('click', function () { show(i); });
      docEl.append(p, again);
    });
  }
  /* the draft in the composer on screen, kept for the node it was written on */
  function keep() {
    var ta = docEl.querySelector('.cm-compose .cm-input');
    if (!ta || !S.doc) return;
    if (ta.value.trim()) S.drafts[S.doc] = ta.value; else delete S.drafts[S.doc];
  }
  /* the steward's document: its head, its notes and its conversation (talk) */
  function drawDoc(f) {
    docEl.textContent = ''; docEl.removeAttribute('aria-busy');
    var head = f.querySelector('.cm-node-head').cloneNode(true), up = head.querySelector('.cm-node-up');
    if (up) up.remove();
    head.className = 'wb2-head';
    var hook = head.querySelector('.cm-sw-compose'), said = hook && head.querySelector(':scope > .cm-tn-meta');
    if (hook) hook.remove();   // the steward's composer is drawn under its conversation (talk)
    if (said) said.remove();   // its state is its line, cm-sw-now
    head.insertBefore(el('p', 'wb2-kind', f.dataset.kind), head.firstChild);
    docEl.appendChild(head);
    var face = f.querySelector('.cm-node-face');
    if (face) { face = face.cloneNode(true); face.className = 'wb2-face'; docEl.appendChild(phone() ? noteFold(face) : face); holds(face, f.dataset.land || ''); }
    var tr = el('section', 'wb2-transcript'); tr.setAttribute('aria-label', 'Transcript');
    tr.appendChild(el('p', 'wb2-note', 'Loading the thread…'));
    docEl.appendChild(tr);
  }
  /* Any other node's document: the bead view's own (web/workbench.js BEAD.into, gd-0GnBi7SRfK), GET /garden/bead/{id} drawn
     as the panel draws it, so /garden/c/n/{id} and /garden?bead={id} read alike; the one in memory at once, then the read bd
     (none: the one in memory is current) when it differs. Its head adds what only the node has: a live agent's line (Watch live, Nudge) and its reason (Wake). */
  function beadDoc(f, gen, bd) {
    var id = f.dataset.id, head = el('header', 'wb2-head'), body = el('div', 'wb2-bead'), had = S.beads.get(id);
    head.append(el('p', 'wb2-kind', f.dataset.kind === 'bead' ? (S.rows[S.sel] || {}).type || 'bead' : f.dataset.kind), el('h2', 'cm-node-title', text(f.querySelector('.cm-node-title'))));
    ['live', 'reason'].forEach(function (k) { var p = f.querySelector('.cm-node-head > .cm-node-' + k); if (p) head.appendChild(p.cloneNode(true)); });
    docEl.textContent = ''; docEl.setAttribute('aria-busy', 'true');
    docEl.append(head, body);
    body.appendChild(el('p', 'wb2-note', 'Loading…'));
    wake(head, id); holdLine();
    var go = function (d) { if (gen === S.gen && body.isConnected) paintBead(id, d); };
    if (!had && !bd) bd = beadRead(id);
    beadJS().then(function () {
      if (had) go(had);
      return bd && bd.then(function (d) { if (!had || (JSON.stringify(d) !== JSON.stringify(had) && !BEAD.holding(body))) go(d); });
    }).then(null, function (err) {
      if (gen !== S.gen || !body.isConnected || had) return;
      docEl.removeAttribute('aria-busy'); body.textContent = '';
      var p = body.appendChild(el('p', 'wb2-err', 'Could not read it. ' + err.message)), again = body.appendChild(el('button', 'btn small', 'Try again'));
      p.setAttribute('role', 'alert'); again.type = 'button';
      again.addEventListener('click', function () { show(S.sel); });
    });
  }
  /* bead id's document d into the document on screen, its scroll kept */
  function paintBead(id, d) {
    var head = docEl.querySelector('.wb2-head'), body = docEl.querySelector('.wb2-bead'), top = docEl.scrollTop;
    if (S.doc !== id || !head || !body) return;
    BEAD.into({ box: docEl, title: head.querySelector('.cm-node-title'), kind: head.querySelector('.wb2-kind'), body: body }, id, d);
    docEl.scrollTop = top; docEl.removeAttribute('aria-busy');
  }
  /* bead id's document read (once at a time), kept in memory */
  function beadRead(id) {
    var u = typeof BEAD !== 'undefined' && BEAD.url ? BEAD.url(id) : base + '/garden/bead/' + encodeURIComponent(id);
    return S.bpend[id] || (S.bpend[id] = get(u, true).then(function (d) { delete S.bpend[id]; delete S.bstale[id]; return lru(S.beads, id, d, 24); }, function (e) { delete S.bpend[id]; throw e; }));
  }
  /* The bead view's bundle (body data-bead-js: web/workbench.js and what its document draws with), loaded once. The page has
     no Workbench panel: its Try again, Show older and a sent line read the document again here instead (reload: drawn even
     while the viewer writes in it). */
  function beadJS() {
    return S.bjs || (S.bjs = (typeof BEAD !== 'undefined' && BEAD.into ? Promise.resolve() : new Promise(function (ok, no) {
      var s = document.createElement('script');
      s.src = B.dataset.beadJs; s.onload = ok;
      s.onerror = function () { S.bjs = null; s.remove(); no(new Error('The bead view could not be loaded.')); };
      B.appendChild(s);
    })).then(function () {
      var again = function (force) { return function (k, id) { if (k === 'bead' && id === S.doc) beadRead(id).then(function (d) { if (S.doc === id && (force || !BEAD.holding(docEl.querySelector('.wb2-bead')))) paintBead(id, d); }, function () { /* it stays as shown */ }); }; };
      WORKBENCH.reload = again(true); WORKBENCH.refresh = again(false);
    }));
  }
  function drawOpen(f) {
    var lis = f.querySelectorAll('.cm-node-open li'), done = f.querySelectorAll('.cm-node-open li.cm-open--done').length;
    var sw = f.dataset.kind === 'steward', name = sw ? 'Asks for you' : 'Open', sec = f.querySelector('.cm-node-open');
    if (sw) done = +(sec && sec.dataset.done) || 0;   // the steward's answered decisions are a count, not rows
    var b = railBody(railOpen, lis.length || (sw && done) ? name + ' · ' + (sw ? lis.length : lis.length - done) + ' open, ' + done + ' done' : name);
    if (!lis.length) { b.appendChild(el('p', 'wb2-note', sw ? 'Nothing asks for you.' : 'Nothing open under it.')); return; }
    var ul = el('ul', 'wb2-list');
    Array.prototype.forEach.call(lis, function (li) { ul.appendChild(li.cloneNode(true)); });
    Array.prototype.forEach.call(ul.querySelectorAll('.cm-tick svg'), function (g) { g.setAttribute('aria-hidden', 'true'); });   // the tick's span says done
    b.appendChild(ul);
  }
  function drawPlan(i) {
    var r = S.rows[i], sib = siblings(S.rows, i), b, kind = (S.rows[0] || {}).type;
    var name = i === 0 ? r.title : (S.rows[r.up] || {}).title || '';
    var plan = kind === 'plan' || kind === 'phase';
    if (kind === 'steward' && i === 0) { tending(r); return; }
    b = railBody(railPlan, plan ? 'Plan' : kind === 'thread' ? 'Thread' : kind === 'steward' ? 'Plans it tends' : 'Siblings');
    if (!sib.length) { b.appendChild(el('p', 'wb2-note', 'Nothing beside it.')); return; }
    var at = sib.indexOf(i), ol = el('ol', 'wb2-steps');
    b.appendChild(el('p', 'wb2-where', at >= 0 ? (plan ? 'Step ' : '') + (at + 1) + ' of ' + sib.length + ' in ' + name : sib.length + ' under ' + (i === 0 ? 'it' : name)));   // the root's title is the document's head
    sib.forEach(function (j) {
      var s = S.rows[j], li = el('li', 'wb2-step' + (s.closed ? ' wb2-row--closed' : '')), a = el('a', 'wb2-steplink', s.title);
      li.dataset.state = s.state || ''; a.href = href(base, s.id); a.dataset.sel = j;
      li.append(dot(s.state), a);
      if (j === i) { li.setAttribute('aria-current', 'step'); li.classList.add('wb2-step--here'); }
      ol.appendChild(li);
    });
    b.appendChild(ol);
  }
  /* The steward's Tending: its stalled plans and the steps parked at the purse wall, from its fragment. */
  function tending(r) {
    var b = railBody(railPlan, 'Tending'), f = S.frags.get(r.id) || S.treeF, ol = f && f.querySelector('.cm-sw-tending ol');
    var plans = S.rows.filter(function (p) { return p.type === 'epic' && p.up === 0; });   // not its Wishes
    if (plans.length) {   // its tree's plans, for a phone's steward screen, which has no tree beside it (garden.css shows them there only)
      var box = b.appendChild(el('div', 'wb2-swplans')), pl = el('ol', 'wb2-steps');
      box.append(el('p', 'wb2-where', 'Plans it tends'), pl);
      plans.forEach(function (p) {
        var li = pl.appendChild(el('li', 'wb2-step')), a = el('a', 'wb2-steplink', p.title);
        li.dataset.state = p.state || ''; a.href = href(base, p.id); a.dataset.open = p.id;
        li.append(dot(p.state), a);
        if (p.flag) li.appendChild(el('span', 'wb2-flag', p.flag)).dataset.flag = p.flag;
      });
    }
    if (!ol) { b.appendChild(el('p', 'wb2-note', 'Nothing stalled or parked.')); return; }
    if (plans.length) b.appendChild(el('p', 'wb2-where wb2-swplans', 'Stalled or parked'));
    ol = b.appendChild(ol.cloneNode(true)); ol.className = 'wb2-steps wb2-tending';
  }
  /* Jobs: the holder's, from its fragment; the stream adds to them (job events for S.hold). With no holder, the agents at work
     under it (busy, row indices), so the rail never says nobody works on what its tree shows at work. */
  function drawJobs(h, gen, f, busy) {
    S.hold = h; S.hst = null;
    var b = railBody(railJobs, 'Jobs');
    if (!h) {
      var p = b.appendChild(el('p', 'wb2-note', f.dataset.kind === 'steward' ? 'No run of the steward is at work now.' : 'No agent holds it' + (busy.length ? '; at work under it: ' : '.')));
      busy.forEach(function (j, n) { var a = p.appendChild(el('a', '', S.rows[j].title)); a.href = href(base, S.rows[j].id); a.dataset.sel = j; p.appendChild(document.createTextNode(n < busy.length - 1 ? ', ' : '.')); });
      connect(); return;
    }
    var who = b.appendChild(el('p', 'wb2-where', 'Held by ')).appendChild(el('a', '', (typeof TITLES !== 'undefined' && TITLES.title(h)) || h));
    who.href = href(base, h); who.dataset.node = h;
    (f.dataset.id === h ? Promise.resolve(f) : node(h)).then(function (af) {
      if (gen !== S.gen) return;
      who.textContent = S.hname = text(af.querySelector('.cm-node-title')) || h;
      S.hst = S.hst || af.dataset.state; holdLine();
      var ol = af.querySelector('.cm-jobs ol');
      b.appendChild(ol ? ol.cloneNode(true) : el('p', 'wb2-note', 'No jobs seen yet.')).classList.add('wb2-jobs');
    }, function () { if (gen === S.gen) b.appendChild(el('p', 'wb2-note', 'Its jobs could not be read.')); });
    connect();
  }
  function jobLi(j) {
    var run = j.ok === undefined || j.ok === null, li = el('li', 'cm-job' + (run ? ' cm-job--running' : j.ok === false ? ' cm-job--failed' : ''));
    li.dataset.job = j.id; li.dataset.tool = j.tool;
    li.append(el('code', 'cm-job-cmd', j.command || j.tool), document.createTextNode(' '),
      el('span', 'cm-job-meta', (j.exit !== undefined ? 'exit ' + j.exit + ' · ' : '') + (run ? 'running' : dur(j.duration_ms || 0))));
    if ((j.head || []).length) li.appendChild(el('pre', 'cm-job-out', j.head.join('\n')));
    return li;
  }
  function dur(ms) { return ms < 1000 ? ms + ' ms' : ms < 60000 ? (ms / 1000).toFixed(1) + ' s' : Math.floor(ms / 60000) + 'm ' + Math.floor(ms / 1000) % 60 + 's'; }

  /* The live head's Nudge: page-nudge.js (web/glassact.js), loaded by the first one, opens its nudge box under the head's
     line; a page that cannot load it goes where the link goes, the farm's Glass. */
  var nudgeJs = null;
  function nudge(a) {
    var at = a.closest('.cm-node-live'), open = function () { var b = at.nextElementSibling; return b && b.matches('.gl-nudge') && b; };
    if (open()) { open().querySelector('textarea').focus(); return; }
    nudgeJs = nudgeJs || new Promise(function (ok, no) {
      var s = document.createElement('script');
      s.src = B.dataset.nudgeJs; s.onload = ok;
      s.onerror = function () { nudgeJs = null; s.remove(); no(); };
      B.appendChild(s);
    });
    nudgeJs.then(function () {
      if (!at.isConnected || open()) return;
      GLASSACT.mount(at, a.dataset.agent, a.dataset.name);
      a.setAttribute('aria-expanded', 'true');
    }, function () { location.href = a.href; });
  }

  /* Wake / Restart (gd-7DyHdnx8fC): web/wake.js's lines under an agent head's .cm-node-reason when body data-wake-js
     (the viewer may restart gardeners), wake.js loaded on the first such head; Yes posts /garden/gardener/{id}/wake. */
  var wakeJs = null;
  function wake(head, id) {
    var p = head.querySelector('.cm-node-reason'), was = S.wake[id];
    if (!p && was && was.step !== 'busy') delete S.wake[id];   // at work again
    if (!p || !B.dataset.wakeJs) return;
    wakeJs = wakeJs || new Promise(function (ok, no) {
      var s = document.createElement('script');
      s.src = B.dataset.wakeJs; s.onload = ok;
      s.onerror = function () { wakeJs = null; s.remove(); no(); };
      B.appendChild(s);
    });
    wakeJs.then(function () {
      var g = wakeOf(p.dataset, p.textContent), s = S.wake[g.id];
      if (s && s.step !== 'busy' && !WAKE.verb(g)) delete S.wake[g.id];   // no longer asleep, down or stale
      wakePaint(p);
    }, function () { /* no buttons; gb wake still does it */ });
  }
  /* the lines of p's gardener in the box after p; focus: the class of the button to focus */
  function wakePaint(p, focus) {
    if (!p.isConnected) return;
    var g = wakeOf(p.dataset, p.textContent), ls = WAKE.lines(g, S.wake[g.id]), box = p.nextElementSibling;
    if (!box || !box.matches('.wb2-wake')) box = p.parentNode.insertBefore(el('div', 'wb2-wake'), p.nextSibling);
    box.textContent = ''; box.hidden = !ls.length;
    ls.forEach(function (ln) {
      if (!Array.isArray(ln[1])) { box.appendChild(el('p', 'cm-note ' + ln[0], ln[1])).setAttribute('role', 'status'); return; }
      var row = box.appendChild(el('p', 'wb2-wakeacts'));
      ln[1].forEach(function (k) { var b = row.appendChild(el('button', k.cls, k.text)); b.type = 'button'; b.dataset.id = k.id; });
    });
    var f = focus && box.querySelector('button.' + focus);
    if (f) f.focus();
  }
  function wakePress(b) {
    var box = b.closest('.wb2-wake'), p = box.previousElementSibling, g = wakeOf(p.dataset, p.textContent), s = S.wake[g.id];
    if (b.classList.contains('gwake')) { S.wake[g.id] = { step: 'ask' }; wakePaint(p, 'gwake-yes'); return; }
    if (b.classList.contains('gwake-no')) { delete S.wake[g.id]; wakePaint(p, 'gwake'); return; }
    if (!b.classList.contains('gwake-yes') || !s || s.step !== 'ask') return;
    S.wake[g.id] = { step: 'busy' }; wakePaint(p);
    fetch(base + '/garden/gardener/' + encodeURIComponent(g.id) + '/wake', { method: 'POST', credentials: 'same-origin', redirect: 'manual', headers: { Accept: 'application/json' } })
      .then(function (r) {
        if (r.type === 'opaqueredirect') return WAKE.outcome(401, null, g);
        return r.json().then(function (j) { return WAKE.outcome(r.status, j, g); }, function () { return WAKE.outcome(r.status, null, g); });
      }, function () { return { ok: false, text: 'The garden did not answer. Try again.' }; })
      .then(function (o) {
        S.wake[g.id] = o;
        var at = docEl.querySelector('.wb2-head .cm-node-reason');   // the head may have been redrawn
        if (at && at.dataset.id === g.id) wakePaint(at, o.ok ? '' : 'gwake');
      });
  }

  /* The node's thread: the steward's is its conversation (talk); any other's is where it lives (GET /garden/c/bead/{id},
     place.go, kept by thread), so the stream carries its channel and a message there reaches the document's conversation
     (put). */
  function transcript(f, gen) {
    var th = f.querySelector('.cm-node-thread'), c = th && S.threads.get(th.dataset.thread);
    S.thread = null; S.talk = null;
    if (f.dataset.kind === 'steward') { talk(f, docEl.querySelector('.wb2-transcript')); return; }
    if (!th) { connect(); return; }
    if (c) { S.thread = lru(S.threads, c.id, c, 8); connect(); return; }
    get(th.getAttribute('href'), true).then(function (p) {
      if (gen === S.gen) S.thread = lru(S.threads, p.thread, { land: p.land, channel: p.channel, id: p.thread }, 8);
    }, function () { /* not in a channel the viewer can see: no channel on the stream */ }).then(function () { if (gen === S.gen) connect(); });
  }
  function finger() { return !!window.matchMedia && matchMedia('(pointer: coarse)').matches; }
  /* The steward's conversation: the #general threads that address it (its fragment's), the one the address asked for
     (?thread=) marked, and a composer that posts a new thread to #general with @steward, about the row it was asked about. */
  function talk(f, slot) {
    var conv = f.querySelector('.cm-sw-talk'), hook = f.querySelector('.cm-sw-compose');
    slot.textContent = '';
    if (conv) {   // its threads are the transcript's rows (.cm-messages > li: contained, so a key lays out none, B12)
      conv = slot.appendChild(conv.cloneNode(true)); conv.classList.add('wb2-swtalk');
      var rows = conv.querySelector(':scope > ol');
      if (rows) rows.classList.add('cm-messages');
    }
    if (hook) {
      S.talk = { land: f.dataset.land, channel: hook.dataset.channel };
      var form = slot.appendChild(el('form', 'cm-compose wb2-swform')), to = form.appendChild(el('p', 'cm-to', 'To the steward in #general: everyone in the land reads it'));
      var ta = form.appendChild(el('textarea', 'cm-input')), ab = form.appendChild(el('input')), btn = form.appendChild(el('button', 'btn cm-send'));
      form.method = 'post'; form.setAttribute('action', hook.dataset.say); form.dataset.commons = 'compose'; form.dataset.steward = '1';
      to.setAttribute('aria-hidden', 'true');
      ta.name = 'text'; ta.rows = 1; ta.maxLength = 10000; ta.placeholder = 'Ask the steward…'; ta.setAttribute('aria-label', 'Ask the steward, in #general');
      ta.enterKeyHint = finger() ? 'enter' : 'send'; ta.value = S.drafts[S.doc] || '';
      ab.type = 'hidden'; ab.name = 'about'; ab.value = S.about[S.doc] || '';
      btn.type = 'submit'; btn.appendChild(el('span', 'cm-word', 'Send'));
    } else slot.appendChild(el('p', 'wb2-note', 'The steward has no #general to talk in here.'));
    var at = S.here && slot.querySelector('.cm-sw-thread[data-thread="' + CSS.escape(S.here) + '"]');
    if (at) { at.classList.add('wb2-swhere'); S.here = ''; requestAnimationFrame(function () { if (at.isConnected) at.scrollIntoView({ block: 'nearest' }); }); }
    else if (phone()) requestAnimationFrame(function () { var c = slot.closest('.wb2-cols'); if (c) c.scrollTop = c.scrollHeight; });   // a phone's Thread opens on its newest, as a chat does (gd-1jjjx7CMU3)
    if (S.ask === S.doc) { S.ask = ''; swFocus(); }
    cutAll(); holdLine(); connect();
  }
  /* the steward's document's one Expand all, at the end of its runs line, while its notes or conversation hold a cut text
     or a fold (web/cuts.js) */
  function cutAll() { var h = docEl.querySelector(':scope > .wb2-head'); if (h) CUTS.all(docEl, h.querySelector(':scope > .cm-sw-today') || h); }
  /* The steward's conversation drawn again from its fragment f, in place, the rest of the document (its composer and the
     focus in it) left as it is: a thread sent from here and not yet in f, or still sending, stays at the end. */
  function swPatch(f) {
    var now = docEl.querySelector('.wb2-swtalk'), conv = f.querySelector('.cm-sw-talk');
    if (!now || !conv) return false;
    var next = conv.cloneNode(true), ol = next.querySelector(':scope > ol');
    next.classList.add('wb2-swtalk');
    if (!ol) { var none = next.querySelector(':scope > .cm-empty'); if (none) none.remove(); ol = next.appendChild(el('ol')); }
    ol.classList.add('cm-messages');
    Array.prototype.forEach.call(now.querySelectorAll(':scope > ol > .cm-sw-thread'), function (li) {
      var mine = li.classList.contains('wb2-pending') || li.classList.contains('wb2-failed') || li.querySelector('.wb2-sent');
      if (mine && !(li.dataset.thread && next.querySelector('.cm-sw-thread[data-thread="' + CSS.escape(li.dataset.thread) + '"]'))) ol.appendChild(li);
    });
    if (!ol.children.length) { ol.remove(); next.appendChild(el('p', 'cm-empty', 'Nobody has asked the steward anything in #general yet.')); }
    now.replaceWith(next); cutAll();
    return true;
  }
  /* the steward's composer focused, its draft's caret at the end */
  function swFocus() {
    var ta = docEl.querySelector('.wb2-swform .cm-input');
    if (!ta) return;
    if (S.drafts[S.doc] && (!ta.value.trim() || /^@steward about /.test(ta.value))) ta.value = S.drafts[S.doc];   // an ask replaces only an empty draft or an ask
    var ab = ta.form.elements.about; if (ab) ab.value = S.about[S.doc] || '';
    ta.focus({ preventScroll: true }); ta.setSelectionRange(ta.value.length, ta.value.length);
  }
  /* The steward's Holds, edited in place on its face (in place of the land header's notes dialog, land_notes.go): an
     owner or member of the land (GET /garden/notes/{land}: edit, holds_ok) gets Edit; the save posts the Holds it opened as
     "was", and a 409 says the Holds now with the draft kept. The node is kept 30 s on the server (stewardCacheFor), so a
     Holds saved here is shown as saved until then. The face's data-holds says it once settled: edit, read or none. */
  /* A phone's steward notes behind one line of counts, "Holds 2 · Promises 1", so its conversation is on the first screen
     (rubric C11: counts, not lists, at the head; gd-1jjjx7CMU3). */
  function noteFold(face) {
    var d = el('details', 'bv-fold wb2-facefold'), sum = [];
    [].forEach.call(face.querySelectorAll('h1,h2,h3,h4,h5,h6'), function (h) {
      if (h.closest('.cm-cut-whole')) return;   // a cut face's whole says them again
      var n = 0;
      for (var x = h.nextElementSibling; x && !/^H[1-6]$/.test(x.tagName); x = x.nextElementSibling) n += x.querySelectorAll('li').length;
      sum.push(text(h).replace(/\s*\(.*\)$/, '') + (n ? ' ' + n : ''));   // "Answers (newest first)": the name
    });
    d.append(el('summary', '', sum.join(' · ') || 'Notes'), face);
    return d;
  }
  function holds(face, land) {
    var h = [].filter.call(face.querySelectorAll('h1,h2,h3,h4,h5,h6'), function (x) { return /^Holds(\s|$)/.test(text(x)) && !x.closest('.cm-cut-whole'); })[0];
    if (!h) { face.dataset.holds = 'none'; return; }
    var kept = S.held[land];
    if (kept && Date.now() - kept.at < 35000) heldText(h, kept.text);
    var url = base + '/garden/notes/' + encodeURIComponent(land);
    get(url, true).then(function (d) {
      face.dataset.holds = 'read';
      if (!h.isConnected || !d || !d.edit || !d.holds_ok || h.querySelector('.wb2-holdsedit')) return;
      face.dataset.holds = 'edit';
      var b = el('button', 'btn small wb2-holdsedit', 'Edit'); b.type = 'button'; b.setAttribute('aria-label', 'Edit Holds');
      h.append(' ', b);
      b.addEventListener('click', function () { editHolds(h, kept && Date.now() - kept.at < 35000 ? kept.text : d.holds || '', url, land); });
    }, function () { face.dataset.holds = 'read'; /* the notes are read alone; Holds stay as they are */ });
  }
  function holdBody(h) { var out = []; for (var n = h.nextElementSibling; n && !/^H[1-6]$/.test(n.tagName); n = n.nextElementSibling) out.push(n); return out; }
  function heldText(h, t) {
    holdBody(h).forEach(function (n) { if (!n.matches('form.wb2-holds')) n.remove(); });
    h.after(el('div', 'wb2-holdstext' + (t.trim() ? '' : ' wb2-note'), t.trim() || 'Nothing yet.'));
  }
  function editHolds(h, was, url, land) {
    var open = h.nextElementSibling;
    if (open && open.matches('form.wb2-holds')) { open.querySelector('textarea').focus(); return; }
    var body = holdBody(h), f = el('form', 'wb2-holds'), ta = f.appendChild(el('textarea', 'cm-input'));
    var hint = f.appendChild(el('p', 'wb2-note', 'One hold to a line. The steward sees a change on its next run.')), st = f.appendChild(el('p', 'wb2-holdsst'));
    var acts = f.appendChild(el('div', 'wb2-holdsacts')), save = acts.appendChild(el('button', 'btn small', 'Save Holds')), cancel = acts.appendChild(el('button', 'btn small', 'Cancel'));
    ta.rows = 4; ta.value = was; ta.spellcheck = true; ta.placeholder = '- never merge a PR that touches billing';
    ta.setAttribute('aria-label', 'Holds'); hint.id = 'wb2-holdshint'; ta.setAttribute('aria-describedby', hint.id);
    st.setAttribute('role', 'status'); save.type = 'submit'; cancel.type = 'button';
    body.forEach(function (n) { n.hidden = true; });
    h.after(f); ta.focus();
    var edit = h.querySelector('.wb2-holdsedit'), done = function () { f.remove(); body.forEach(function (n) { n.hidden = false; }); if (edit) edit.focus(); };
    var why = function (t) { st.textContent = t; st.setAttribute('role', 'alert'); };
    cancel.addEventListener('click', done);
    f.addEventListener('keydown', function (e) { if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); done(); } });
    f.addEventListener('submit', function (e) {
      e.preventDefault(); e.stopPropagation();
      var bad = badHold(ta.value);
      if (bad) { why('A line in Holds can’t start with “## ”: that starts a section of its own (' + bad.trim() + ').'); return; }
      save.disabled = true; st.removeAttribute('role'); st.textContent = 'Saving…';
      fetch(url + '/holds', { method: 'POST', credentials: 'same-origin', cache: 'no-store', headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
        body: JSON.stringify({ holds: ta.value, was: was }) }).then(function (r) {
        return r.text().then(function (t) { var j = null; try { j = JSON.parse(t); } catch (x) { /* a plain refusal */ } return { r: r, j: j, t: t }; });
      }).then(function (x) {
        save.disabled = false;
        if (x.r.ok) {
          S.held[land] = { text: x.j && typeof x.j.holds === 'string' ? x.j.holds : ta.value, at: Date.now() };
          f.remove(); heldText(h, S.held[land].text); if (edit) edit.focus();
          say('Holds saved. The steward sees them on its next run.');
          return;
        }
        if (x.r.status === 409 && x.j) { was = x.j.holds || ''; why((x.j.error || 'Holds changed since you opened them.') + ' Your draft is kept; Save again to replace them.'); return; }
        why((x.j && x.j.error) || x.t.trim() || 'That did not go through (' + x.r.status + ').');
      }, function () { save.disabled = false; why('The garden could not be reached. Your draft is kept: try again.'); });
    });
  }
  /* Send, optimistic: the message is in the transcript at once, pending, and settles when the stream or the answer brings
     its row; with neither in 3 s, or a refusal, it says so and offers Retry. Every attempt carries the send's nonce, so a
     Retry of one that was only slow writes nothing twice (commons/sendonce.go). */
  function send(form) {
    var sw = !!form.dataset.steward, ta = form.querySelector('.cm-input'), ol = sw ? swList() : form.closest('.cm-thread').querySelector('.cm-messages');
    if (!ta || !ta.value.trim() || !ol) return;
    var words = sw ? stewardText(ta.value) : ta.value.trim(), li = el('li', (sw ? 'cm-sw-thread' : 'cm-msg') + ' wb2-pending'), m = li.appendChild(el('p', 'cm-meta'));
    m.append(el('span', 'cm-author', 'You'), document.createTextNode(' '), el('span', 'wb2-sending', 'Sending…'));
    li.appendChild(el('p', 'cm-text', words));
    li.wb2 = { text: words, form: form, action: form.getAttribute('action'), node: S.doc, n: 0, nonce: nonce(), sw: sw, about: sw ? form.elements.about.value : '' };
    ta.value = ''; delete S.drafts[S.doc]; ta.wb2Typed = 0;
    if (sw) { form.elements.about.value = ''; delete S.about[S.doc]; }
    var none = ol.querySelector(':scope > .cm-empty');
    if (none) none.remove();
    ol.appendChild(li);
    // scrolled to in the frame that paints it, and posted after that frame, so the request costs the echo nothing
    requestAnimationFrame(function () { if (li.isConnected) li.scrollIntoView({ block: 'nearest' }); setTimeout(function () { post(li); }, 0); });
  }
  /* the steward conversation's list of threads, made when it has none yet */
  function swList() {
    var sec = docEl.querySelector('.wb2-swtalk');
    if (!sec) return null;
    var ol = sec.querySelector(':scope > ol'), none = sec.querySelector(':scope > .cm-empty');
    if (none) none.remove();
    return ol || sec.appendChild(el('ol', 'cm-messages'));
  }
  function nonce() {
    try { return crypto.randomUUID(); } catch (e) { return Date.now().toString(36) + '-' + Math.random().toString(36).slice(2, 12); }
  }
  /* one attempt at li's send: any attempt's answer settles it, and only the last one's refusal says so. li may be in a
     thread off screen (its node left), where it is kept as it should show when that node is open again. */
  function post(li) {
    var w = li.wb2, n = ++w.n, st = li.querySelector('.wb2-sending'), again = li.querySelector('.wb2-retry');
    if (!w.body) { w.body = new FormData(w.form); w.body.set('text', w.text); w.body.set('nonce', w.nonce); if (w.sw) w.body.set('about', w.about); }   // built after the echo's frame, kept for Retry
    li.classList.remove('wb2-failed'); st.textContent = 'Sending…'; st.removeAttribute('role');
    if (again) again.remove();
    clearTimeout(w.t); w.t = setTimeout(function () { if (li.parentNode) mark(li, 'Not sent yet.'); }, 3000);
    fetch(w.action, { method: 'POST', credentials: 'same-origin', body: w.body, headers: { 'X-Commons': 'fragment' } }).then(function (r) {
      return r.text().then(function (t) { if (!r.ok) throw new Error(t.trim() || 'That did not go through (' + r.status + ').'); return t; });
    }, function () { throw new Error('The garden could not be reached.'); }).then(function (t) { settle(li, made(t).firstElementChild); }, function (e) { if (n === w.n) failed(li, e.message); });
  }
  /* the pending row li replaced by its row (once: the stream's copy, or another attempt's answer, may have come first) */
  function settle(li, row) {
    var ol = li.parentNode, id = row && row.dataset.id;
    clearTimeout(li.wb2.t);
    if (id) S.mine[id] = true;
    if (!ol) return;
    if (li.wb2.sw) { swSent(li, id); return; }
    if (!id || ol.querySelector('[id="m-' + CSS.escape(id) + '"]')) li.remove(); else li.replaceWith(row);
  }
  /* a steward thread sent: it stays as written, now the thread of its id, and the steward's node is read again on its next use */
  function swSent(li, id) {
    var st = li.querySelector('.wb2-sending'), again = li.querySelector('.wb2-retry');
    li.classList.remove('wb2-pending', 'wb2-failed');
    if (id) li.dataset.thread = id;
    if (st) { st.className = 'wb2-sent'; st.removeAttribute('role'); st.textContent = 'sent to the steward'; }
    if (again) again.remove();
    S.stale[li.wb2.node] = true;
  }
  /* li's send refused: Retry is offered; off screen, its text goes back to its node as the draft instead, when that is free */
  function failed(li, why) {
    var w = li.wb2, ta = S.doc === w.node && docEl.querySelector('.cm-compose .cm-input');
    clearTimeout(w.t);
    if (!li.parentNode) return;
    if (!li.isConnected && w.node && (ta ? !ta.value.trim() : !S.drafts[w.node])) {
      li.remove();
      if (ta) ta.value = w.text; else S.drafts[w.node] = w.text;
      return;
    }
    mark(li, 'Not sent. ' + why);
  }
  function mark(li, what) {
    var st = li.querySelector('.wb2-sending');
    li.classList.add('wb2-failed'); st.textContent = what; st.setAttribute('role', 'alert');
    if (!li.querySelector('.wb2-retry')) { var b = el('button', 'btn small wb2-retry', 'Retry'); b.type = 'button'; li.querySelector('.cm-meta').appendChild(b); }
  }
  /* The reply line: who holds the node and its state (idle, working, at a prompt), kept by node events; the steward's composer
     says it, any other node's head (its composer is the bead view's). */
  var SAY = { idle: 'idle, waiting for you', prompt: 'at a prompt', stuck: 'no progress lately' };
  function holdLine() {
    var to = docEl.querySelector('.cm-compose .cm-to') || docEl.querySelector('.wb2-head');
    if (!to) return;
    var p = to.querySelector('.wb2-hold') || to.appendChild(el('span', 'wb2-hold')), st = S.hst || '';
    p.hidden = !S.hold || !st;
    p.dataset.state = st; p.textContent = p.title = (S.hname || S.hold) + ' · ' + (SAY[st] || st.replace(/_/g, ' '));   // the line cuts a long name; title keeps it
  }
  /* A message row of the thread on screen (commons render.go "message", new or changed) into the document's conversation and
     the document in memory, read from the row (web/beadtalk.js hear): no fetch of its own (B15). A row whose line it cannot
     draw from the row has the document read again. */
  function put(li) {
    var ol = docEl.querySelector('.wb2-bead .bv-cv-list'), l = BEADTALK.row(li);
    if (!l || !ol || !S.thread || (l.thread !== S.thread.id && l.id !== S.thread.id)) return;
    if (!BEADTALK.hear(S.doc, ol, l, S.thread.land)) reread(S.doc);
    keepLine(S.doc, l);
  }
  /* line l (put) written into bead id's document in memory, so an open from memory draws it as the stream has it */
  function keepLine(id, l) {
    var k = (S.beads.get(id) || {}).kin;
    if (!k) return;
    (k.conversation || []).forEach(function (c) { if (c.id === l.id && c.via !== 'card') { if (l.gone) c.deleted = true; else if (l.text != null) c.text = l.text; } });
    if (l.card) (k.agents || []).forEach(function (a) { if (a.id === l.card.agent) { a.state = l.card.state; a.live = l.card.live; } });
  }
  /* bead id's document read again and drawn, once its events pause, while it is on screen (WORKBENCH.refresh, from the bead
     view's bundle: beadJS) */
  function reread(id) {
    clearTimeout(reread.t);
    reread.t = setTimeout(function () { if (S.doc === id) beadJS().then(function () { if (S.doc === id) WORKBENCH.refresh('bead', id); }, function () { /* it stays as shown */ }); }, 400);
  }

  /* ---------- the stream ---------- */
  function connect() {
    if (typeof LIVE === 'undefined' || !window.EventSource || !S.root) return;
    var q = '?root=' + encodeURIComponent(S.root);
    var sel = S.rows[S.sel];
    [sel && sel.id, S.hold].forEach(function (id) { if (id && id !== S.root) q += '&node=' + encodeURIComponent(id); });
    var ch = S.thread || S.talk;
    if (ch) q += '&land=' + encodeURIComponent(ch.land) + '&channel=' + encodeURIComponent(ch.channel);
    if (S.es && S.spot === q) return;
    if (S.es) S.es.close();
    S.spot = q;
    // from the last moment the stream before it was current, so what came between the two is not lost (events.go replay);
    // the replay reaches a little further back, so a message already heard is not news again
    var replay = !!S.at, es = S.es = LIVE.source(base + '/garden/c/events' + q + (S.at ? '&since=' + encodeURIComponent(S.at) : ''));
    function on(t, fn) { es.addEventListener(t, function (e) { if (S.es !== es) return; if (e.lastEventId) S.at = e.lastEventId; var d; try { d = JSON.parse(e.data); } catch (x) { return; } fn(d); }); }
    on('ping', function () { /* only its id */ });
    on('replayed', function () { replay = false; });
    on('node', patch);
    on('job', function (d) {
      var b = railJobs.lastChild, ol = b.querySelector('ol.wb2-jobs'), j = d.job;
      if (!j || d.id !== S.hold) return;
      if (!ol) { var none = b.querySelector('.wb2-jobs'); if (none) none.remove(); ol = b.appendChild(el('ol', 'wb2-jobs')); }
      Array.prototype.forEach.call(ol.querySelectorAll('.cm-empty'), function (x) { x.remove(); });
      var was = ol.querySelector('[data-job="' + CSS.escape(j.id) + '"]'), li = jobLi(j);
      if (was) was.replaceWith(li); else ol.insertBefore(li, ol.firstChild);
      S.stale[S.hold] = true;
    });
    ['message', 'card'].forEach(function (t) {
      on(t, function (d) {
        var th = d.thread || d.id;
        if ((!S.thread || th !== S.thread.id) && !S.mine[d.id] && !(replay && S.seen[d.id])) heard(th);
        S.seen[d.id] = true;
        S.beads.forEach(function (x, k) { S.bstale[k] = true; });   // any document may carry it: read again on its next open
        if (S.talk && !S.mine[d.id] && !(replay && S.seen[d.id])) talked();
        if (d.html) put(made(d.html).firstElementChild);   // its whole row: d.append comes only beside it (commons stream.go version)
      });
    });
    on('reset', function () { S.threads.clear(); S.beads.forEach(function (d, k) { S.bstale[k] = true; }); Array.from(S.trees.keys()).forEach(dropTree); S.frags.forEach(function (f, k) { S.stale[k] = true; }); reroot(S.root, true); });
  }
  /* a message in the steward's #general: its node, which may now show it, is read again (unchanged, nothing is drawn) */
  function talked() {
    clearTimeout(talked.t);
    talked.t = setTimeout(function () { var f = S.talk && S.frags.get(S.doc); if (f) revalidate(S.doc, f); }, 600);
  }
  /* a message in thread th, which is not on screen: its section in memory is dropped, and the rows it is the thread of are
     marked unread */
  function heard(th) {
    S.threads.delete(th);
    S.rows.forEach(function (r, i) { if (r.id && r.thread === th && r.id !== S.doc) { S.unread[r.id] = true; unread(i, true); } });
  }
  /* a node event: the rows that show it take its state and title; one the tree lacks refetches the root */
  var again = 0;
  function patch(d) {
    var n = d.node;
    if (!n) return;
    var st = n.agent_state || n.status, hit = false, hs = docEl.querySelector('.wb2-head .cm-state');
    if (n.id === S.hold) { S.hst = st; holdLine(); }
    if (n.id === S.doc && hs) { hs.dataset.state = st; hs.textContent = st.replace(/_/g, ' '); }
    if (n.id === S.doc && S.beads.has(n.id)) reread(n.id);   // its document reads it again
    S.rows.forEach(function (r, i) {
      if (r.id !== n.id) return;
      hit = true; r.state = st; r.closed = n.status === 'closed';
      var e = rowEl(i), s = e && e.querySelector('.wb2-line .cm-state');
      if (!e) return;
      e.dataset.state = e.querySelector('.wb2-dot').dataset.state = st; e.classList.toggle('wb2-row--closed', r.closed);
      if (s) { s.dataset.state = st; s.textContent = st.replace(/_/g, ' '); }
    });
    S.frags.forEach(function (f, k) { S.stale[k] = true; });   // any of them may show it: served as they are, then revalidated
    S.beads.forEach(function (d, k) { S.bstale[k] = true; });
    S.trees.forEach(function (t, k) { if (t.rows.some(function (r) { return r.id === n.id; })) dropTree(k); });
    if (hit) { if (S.sel >= 0) drawPlan(S.sel); return; }
    clearTimeout(again); again = setTimeout(function () { reroot(S.root, true); }, 400);
  }

  /* ---------- open, up, find, jump, the menu ---------- */
  /* Open id as the root: the tree, then the row that was selected (keep: an id), else the root. */
  function reroot(id, fresh, keep) {
    var gen = ++S.gen;
    if ((!fresh || id !== S.root) && !S.frags.has(id)) { stash(); tree.textContent = ''; tree.appendChild(el('li', 'wb2-note', 'Loading…')).setAttribute('role', 'none'); }
    var was = S.rows[S.sel] && S.rows[S.sel].id, bp = S.bpend[keep || (fresh && was) || id];
    S.root = id;
    return node(id, fresh).then(function (f) {
      // the selected row's document, when its read left with the fragment (fetchFirst), drawn in the tree's task: one frame,
      // not a second after it (B14); a read slower than BEAD_WAIT ms more is drawn when it lands
      return bp ? Promise.race([bp, new Promise(function (ok) { setTimeout(ok, BEAD_WAIT); })]).then(function () { return f; }, function () { return f; }) : f;
    }).then(function (f) {
      if (gen === S.gen) rooted(f, keep || (fresh ? was : ''), fresh);
    }, function (err) {
      if (gen !== S.gen) return;
      stash(); S.rows = []; S.doc = ''; S.docF = null;
      tree.textContent = ''; docEl.textContent = '';
      var p = el('p', 'wb2-err', 'Could not open ' + id + '. ' + err.message); p.setAttribute('role', 'alert');
      docEl.appendChild(p);
      document.title = 'Workbench';
    });
  }
  /* root fragment f drawn, with the row want selected (quiet: not said), else the root */
  function rooted(f, want, quiet) {
    drawTree(f);
    document.title = S.rows[0].title + ' · Workbench';
    swRow.href = stewardHref(base, f.dataset.land || ''); swRow.dataset.land = f.dataset.land || '';
    if (f.dataset.kind === 'steward') swRow.setAttribute('aria-current', 'page'); else swRow.removeAttribute('aria-current');
    var sf = S.frags.get(S.sw[f.dataset.land || '']);
    swRow.firstChild.dataset.state = sf ? sf.dataset.state || '' : '';   // the steward's state once it has been read
    var at = 0;
    S.rows.forEach(function (r, i) { if (want && r.id === want) at = i; });
    S.sel = -1;
    select(at, quiet);
  }

  /* ---------- the phone's push stack ---------- */
  var narrow = window.matchMedia && matchMedia('(max-width: 899px)');
  function phone() { return !!narrow && narrow.matches; }
  /* The screen shown under 900 px, the tree or the node (on tab), and the bar saying where Back goes and what this is. */
  function screen(s, tab) {
    if (s === 'node') S.tab = tab || S.tab || 'thread';
    if (view.dataset.screen !== s || view.dataset.tab !== S.tab) cols.scrollTop = 0;
    view.dataset.screen = s; view.dataset.tab = S.tab || 'thread';
    Array.prototype.forEach.call(strip.children, function (b) { var on = b.dataset.tab === view.dataset.tab; b.setAttribute('aria-selected', on); b.tabIndex = on ? 0 : -1; });
    head();
  }
  function head() {
    var r = S.rows[S.sel] || {}, u = tree.querySelector('.wb2-uplink'), node = view.dataset.screen === 'node';
    var k = (S.rows[0] || {}).type || '', st = history.state || {}, sw = k === 'steward';
    title.textContent = node ? sw ? 'Steward' : r.title || '' : k.charAt(0).toUpperCase() + k.slice(1);   // the tree's first row is the root's title
    backT.textContent = sw ? st.from ? st.fromT || 'Back' : 'Farm' : node ? (S.rows[0] || {}).title || '' : u ? text(u.querySelector('.wb2-uptitle')) : fromSw(st) ? 'Steward' : 'Farm';
    back.setAttribute('aria-label', 'Back to ' + backT.textContent);
    strip.children[2].textContent = sw ? 'Tending' : 'Plan'; strip.children[3].textContent = sw ? 'Asks' : 'Open';
  }
  /* whether history entry st is a root opened from the steward (its Tending), which Back returns to */
  function fromSw(st) { return !!st.from && S.swOf[st.from] !== undefined; }
  /* the screen a phone opens root id on: a steward's is its node, its tree being the Tending tab's plans */
  function screenOf(id, s) { return phone() && S.swOf[id] !== undefined ? 'node' : s; }
  /* row i tapped on a phone: one with rows under it opens as the root, one level down; any other, and the root, is pushed */
  function tapped(i) {
    var r = S.rows[i];
    if (!r || !r.id) return;   // Enter on the tree with no row selected
    if (down(S.rows, i)) { go(r.id); return; }
    select(i);
    if (history.pushState) try { history.pushState({ wb2: S.root, sel: r.id, screen: 'node' }, '', location.href); } catch (e) { /* sandboxed */ }
    screen('node', 'thread');
  }
  /* Back, one level up: the node to its tree, the tree to the root above it, the top to the farm. The way the stack came
     is walked back in history; a level the page opened on is put in its place. */
  function goBack() {
    var st = history.state || {}, u = tree.querySelector('.wb2-uplink');
    if ((S.rows[0] || {}).type === 'steward') { if (st.from) history.back(); else farm(); return; }
    if (view.dataset.screen === 'node') { if (st.screen === 'node') history.back(); else screen('tree'); return; }
    if (!u) { if (fromSw(st)) history.back(); else farm(); }
    else if (st.from && st.from === u.dataset.open) history.back();
    else go(u.dataset.open, S.root, true);
  }
  /* the view kept to the visual viewport while a phone's keyboard is up, so the composer sits on the keyboard */
  function viewport() {
    var vv = window.visualViewport, kb = phone() && vv && vv.height < window.innerHeight - 120;
    view.style.top = kb ? vv.offsetTop + 'px' : ''; view.style.height = kb ? vv.height + 'px' : ''; view.style.bottom = kb ? 'auto' : '';
    var f = document.activeElement;   // the field written in stays on screen above the keyboard, its form with it when it fits
    if (kb && docEl.contains(f) && f.matches('textarea,input')) (f.form && f.form.offsetHeight < vv.height ? f.form : f).scrollIntoView({ block: 'nearest' });
  }
  if (window.visualViewport) { visualViewport.addEventListener('resize', viewport); visualViewport.addEventListener('scroll', viewport); }
  /* id opened as the root at a new address (swap: in place of this one), with the row keep selected */
  function go(id, keep, swap) {
    if (!id) return;
    fetchFirst(id);
    if (history.pushState) try { history[swap ? 'replaceState' : 'pushState']({ wb2: id, sel: keep || '', from: swap ? '' : S.root, fromT: swap ? '' : (S.rows[0] || {}).title || '' }, '', addr(id)); } catch (e) { /* sandboxed */ }
    if (screenOf(id, 'tree') === 'node') screen('node', 'thread'); else screen('tree');
    reroot(id, false, keep);
    tree.focus({ preventScroll: true });
  }
  /* a node's address: a steward's is its land's, /garden/c/s/{land} */
  function addr(id) { return S.swOf[id] !== undefined ? stewardHref(base, S.swOf[id]) : href(base, id); }
  /* The land's steward trigger ('' the viewer's land): known, else read from its address, its fragment kept as its node's. */
  function steward(land) {
    if (S.sw[land]) return Promise.resolve(S.sw[land]);
    var k = 's:' + land;
    return S.pend[k] || (S.pend[k] = get(stewardHref(base, land)).then(function (f) {
      var id = f.dataset.id;
      delete S.pend[k];
      S.sw[land] = S.sw[f.dataset.land] = id; S.swOf[id] = f.dataset.land;
      lru(S.frags, id, f, 48);
      if (swRow.dataset.land === land) swRow.firstChild.dataset.state = f.dataset.state || '';
      return id;
    }, function (e) { delete S.pend[k]; throw e; }));
  }
  /* The steward opened as the root; r, a row it is asked about: the composer gets a draft naming it, sent with about=r.id. */
  function toSteward(land, r) {
    return steward(land || '').then(function (id) {
      if (r) { S.drafts[id] = askDraft(r.title, r.id); S.about[id] = r.id; S.ask = id; }
      if (id === S.root && S.doc === id) { S.sel = -1; select(0, true); swFocus(); return; }
      go(id);
    }, function (e) { say('The steward could not be opened. ' + e.message); });
  }
  function rootLand() { var f = S.treeF || S.frags.get(S.root); return f ? f.dataset.land || '' : ''; }
  function up() { var u = tree.querySelector('.wb2-uplink'); if (u) go(u.dataset.open, S.root); else say('This is the top: it hangs from nothing.'); }
  /* the farm, on the bead id when there is one (its bead view, whose Show on the map flies there) */
  function farm(id) { location.assign(base + '/garden' + (id ? '?bead=' + encodeURIComponent(id) : '')); }
  function filter(q) {
    var keep = found(S.rows.map(function (r) { return { id: r.id, title: r.title || r.more || '', up: r.up }; }), q);
    S.rows.forEach(function (r, i) { var e = rowEl(i); if (e) e.hidden = keep.indexOf(i) < 0; });
    return keep;
  }
  function seek(mode) { S.mode = mode; find.placeholder = mode === 'jump' ? 'Jump to a row or a bead id' : 'Find in the tree'; find.focus(); find.select(); }
  function sought() {
    var q = find.value.trim(), first = visible().filter(function (i) { return !q || hit(S.rows[i], q); })[0];
    if (S.mode === 'jump' && ID.test(q) && !S.rows.some(function (r) { return r.id === q; })) { fetchFirst(q); find.value = ''; filter(''); go(q); return; }
    if (first === undefined) { say('Nothing in the tree matches ' + q + '.'); return; }
    if (S.mode === 'jump') { var id = S.rows[first].id; find.value = ''; filter(''); go(id); return; }
    select(first); tree.focus();
  }
  function openMenu(i, at) {
    var r = S.rows[i];
    if (!r || !r.id) return;
    select(i, true);
    menu.textContent = '';
    [['Open here', function () { go(r.id); }], ['Up', up], ['Show in farm', function () { farm(r.id); }], ['Copy id', function () {
      (navigator.clipboard ? navigator.clipboard.writeText(r.id) : Promise.reject()).then(function () { say('Copied ' + r.id); }, function () { say('Could not copy; the id is ' + r.id); });
      tree.focus();
    }], ['Ask the steward about this', function () { toSteward(rootLand(), r); }]].forEach(function (m) {
      var li = el('li'), b = el('button', 'wb2-mi', m[0]);
      li.setAttribute('role', 'none'); b.type = 'button'; b.setAttribute('role', 'menuitem'); b.tabIndex = -1;
      b.addEventListener('click', function () { closeMenu(); m[1](); });
      li.appendChild(b); menu.appendChild(li);
    });
    var box = (at || rowEl(i).firstChild).getBoundingClientRect(), vb = view.getBoundingClientRect();
    menu.style.left = Math.max(8, Math.min(box.right - vb.left - 180, vb.width - 196)) + 'px';
    menu.style.top = (box.bottom - vb.top + 4) + 'px';
    menu.setAttribute('aria-label', 'Actions for ' + r.title);
    menu.hidden = false; menu.querySelector('button').focus();
  }
  function closeMenu() { if (menu.hidden) return; menu.hidden = true; tree.focus(); }
  function typing(e) { var t = e.target; return t && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName)); }

  view.addEventListener('keydown', function (e) {
    var k = e.key, mod = e.metaKey || e.ctrlKey;
    if (mod && (k === 'k' || k === 'K')) { e.preventDefault(); e.stopPropagation(); seek('jump'); return; }
    if (!menu.hidden && menu.contains(e.target)) {
      var items = [].slice.call(menu.querySelectorAll('button')), at = items.indexOf(document.activeElement);
      if (k === 'ArrowDown' || k === 'j') items[(at + 1) % items.length].focus();
      else if (k === 'ArrowUp' || k === 'k') items[(at + items.length - 1) % items.length].focus();
      else if (k === 'Escape' || k === 'Tab') closeMenu();
      else return;
      e.preventDefault(); e.stopPropagation(); return;
    }
    if (e.target === find) {
      if (k === 'Enter') sought();
      else if (k === 'Escape') { find.value = ''; filter(''); tree.focus(); }
      else return;
      e.preventDefault(); e.stopPropagation(); return;
    }
    if (typing(e) || mod || e.altKey) {
      if (k === 'Enter' && !e.shiftKey && !mod && e.target.classList.contains('cm-input') && e.target.form && e.target.form.matches('form[data-commons=compose]') && !finger()) { e.preventDefault(); send(e.target.form); }
      else if (k === 'Escape' && typing(e)) { e.preventDefault(); tree.focus(); }
      return;
    }
    var vis = visible(), onTree = e.target === tree, g = S.g && Date.now() - S.g < 1500;
    S.g = k === 'g' ? Date.now() : 0;
    if (k === 'g') { e.preventDefault(); return; }   // g s: the steward
    if (g && k === 's') toSteward(rootLand());
    else if (k === 'Escape') farm();
    else if (k === '/') seek('find');
    else if (k === 'u') up();
    else if (onTree && (k === 'j' || k === 'ArrowDown')) { select(step(vis, S.sel, 1)); ahead(vis, 1); }
    else if (onTree && (k === 'k' || k === 'ArrowUp')) { select(step(vis, S.sel, -1)); ahead(vis, -1); }
    else if (onTree && k === 'Home') select(vis[0]);
    else if (onTree && k === 'End') select(vis[vis.length - 1]);
    else if (onTree && k === 'Enter') { if (phone()) tapped(S.sel); else if (S.sel > 0) go(S.rows[S.sel].id); }
    else if (onTree && (k === 'ContextMenu' || (k === 'F10' && e.shiftKey))) openMenu(S.sel);
    else return;
    e.preventDefault(); e.stopPropagation();
  });
  /* the row after the selection, the way the keys are going, fetched before it is reached */
  function ahead(vis, by) { var r = S.rows[step(vis, S.sel, by)]; if (r) warm(r.id); }
  /* the node a row or link under t opens, for warm */
  function target(t) {
    var row = t.closest && t.closest('.wb2-row[data-id]'), a = t.closest && t.closest('a[href]');
    if (row && !(a && !row.contains(a))) return row.dataset.id;
    if (!a) return '';
    if (a.dataset.sel) return (S.rows[+a.dataset.sel] || {}).id;
    return a.dataset.open || a.dataset.node || (a.origin === location.origin && route(a.pathname, base)) || beadLink(a);
  }
  /* the bead a link of the bead view's opens (base/garden?bead={id}), which opens here as a node, else '' */
  function beadLink(a) { return !a.target && a.origin === location.origin && a.pathname === base + '/garden' ? new URLSearchParams(a.search).get('bead') || '' : ''; }
  view.addEventListener('mouseover', function (e) { warm(target(e.target)); });
  view.addEventListener('focusin', function (e) { if (e.target !== tree) warm(target(e.target)); });
  ['mouseenter', 'focus'].forEach(function (t) { swRow.addEventListener(t, function () { steward(swRow.dataset.land || '').then(null, function () { /* opened, it says why */ }); }); });
  find.addEventListener('input', function () { if (S.mode === 'find') filter(find.value); });
  view.addEventListener('click', function (e) {
    var t = e.target, a = t.closest && t.closest('a');
    if (!menu.hidden && !menu.contains(t)) closeMenu();
    if (t === farmBtn) { farm(); return; }
    if (back.contains(t)) { goBack(); return; }
    if (t.closest('.wb2-stab')) { screen('node', t.closest('.wb2-stab').dataset.tab); return; }
    if (t.closest('.wb2-retry')) { post(t.closest('.wb2-pending')); return; }
    var sb = t.closest('.cm-send');   // Send sends from the click, not after the form's submission steps
    if (sb && sb.form && sb.form.matches('form[data-commons=compose]')) { e.preventDefault(); send(sb.form); return; }
    if (a === swRow && !(e.button || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey)) { e.preventDefault(); toSteward(swRow.dataset.land || ''); return; }
    var mb = t.closest('.wb2-more');
    if (mb) { openMenu(+mb.closest('.wb2-row').dataset.i, mb); return; }
    if (e.button || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    if (a && a.dataset.open) { e.preventDefault(); go(a.dataset.open, a.classList.contains('wb2-uplink') || a.classList.contains('wb2-crumb') ? S.root : ''); return; }
    if (a && a.dataset.sel) { e.preventDefault(); select(+a.dataset.sel); if (phone()) screen('node', 'thread'); return; }
    if (a && B.dataset.nudgeJs && a.matches('.cm-node-live .cm-nudge')) { e.preventDefault(); nudge(a); return; }
    var wb = t.closest('.wb2-wake button');
    if (wb) { wakePress(wb); return; }
    var nid = a && !tree.contains(a) && (a.dataset.node || (!a.target && a.origin === location.origin && route(a.pathname, base)) || beadLink(a));   // a node's or a bead's link opens here
    if (nid) { e.preventDefault(); var i = S.rows.map(function (r) { return r.id; }).indexOf(nid); if (i >= 0) select(i); else go(nid); return; }
    var row = t.closest('.wb2-row[data-id]');
    if (!row || !tree.contains(row)) return;
    if (a && !a.classList.contains('wb2-t') && !a.dataset.node) return;   // a PR's link opens the PR
    if (a) e.preventDefault();
    if (phone()) { tapped(+row.dataset.i); return; }
    select(+row.dataset.i); tree.focus();
  });
  // not on a phone, where a tap opens already: a quick tap on the screen the last tap opened, near where it was, reads as a
  // double tap and would open its row as the root (gd-DhGFLKK0tj)
  treeBox.addEventListener('dblclick', function (e) { var row = e.target.closest('.wb2-row[data-id]'); if (row && +row.dataset.i > 0 && !phone()) go(row.dataset.id); });
  // pressing Send leaves the focus (and a phone's keyboard) in the composer
  view.addEventListener('mousedown', function (e) { if (e.target.closest('.cm-send')) e.preventDefault(); });
  view.addEventListener('submit', function (e) { if (e.target.matches('form[data-commons=compose]')) { e.preventDefault(); send(e.target); } });
  window.addEventListener('popstate', function (e) {
    var id = route(location.pathname, base), land = stewardRoute(location.pathname, base), st = e.state || {};
    if (id) popTo(id, st);
    else if (land !== null) steward(land).then(function (sid) { popTo(sid, st); }, function (err) { say(err.message); });
  });
  /* history's step to root id, with its row and screen (st, the entry's state) */
  function popTo(id, st) {
    var s = screenOf(id, st.screen === 'node' ? 'node' : 'tree');
    S.pop = st;   // a later step through history wins over one still loading
    if (id !== S.root || !S.rows.length || S.rows[0].id !== id) { screen(s); reroot(id, false, st.sel).then(function () { if (S.pop === st) screen(s); }); return; }
    var i = S.rows.map(function (r) { return r.id; }).indexOf(st.sel);
    if (i >= 0 && i !== S.sel) select(i, true);
    screen(s);
  }

  /* open(id): the workbench on id, the page's address, with the row sel selected (on screen s, on a phone); a phone that
     lands here from outside gets the level above put under it, so Back walks up the stack (phone design 3.6) */
  api.open = function (id, sel, s) {
    if (!view.isConnected) B.appendChild(view);
    var landed = !(history.state && history.state.wb2);
    screen(screenOf(id, s || 'tree'));
    return reroot(id, false, sel).then(function () {
      var u = tree.querySelector('.wb2-uplink');
      if (landed && u && phone() && history.pushState) try {
        history.replaceState({ wb2: u.dataset.open, sel: id }, '', href(base, u.dataset.open)); history.pushState({ wb2: id, sel: S.rows[S.sel].id, from: u.dataset.open }, '', href(base, id));
      } catch (e) { /* sandboxed */ }
      tree.focus({ preventScroll: true });
    });
  };
  api.state = function () { return { root: S.root, doc: S.doc, sel: S.rows[S.sel] && S.rows[S.sel].id, hold: S.hold, thread: S.thread && S.thread.id, rows: S.rows.length, stream: S.spot, screen: view.dataset.screen, tab: view.dataset.tab, steward: S.swOf[S.root] !== undefined }; };
  /* steward(land): the workbench on the land's steward, a steward's address */
  api.steward = function (land) {
    if (!view.isConnected) B.appendChild(view);
    return steward(land).then(function (id) { return api.open(id, st0.sel, st0.screen); }, function (err) {
      var p = el('p', 'wb2-err', 'Could not open the steward. ' + err.message); p.setAttribute('role', 'alert');
      docEl.textContent = ''; docEl.appendChild(p);
    });
  };
  var first = route(location.pathname, base), swFirst = stewardRoute(location.pathname, base), st0 = history.state || {};
  if (first && B.dataset.beadJs) beadJS().then(null, function () { /* the document says why */ });   // on the wire beside the root's fragment
  if (first) api.open(first, st0.sel, st0.screen);
  else if (swFirst !== null) api.steward(swFirst);
  return api;
})();
if (typeof module !== 'undefined') module.exports = COMMONS2;
