/* web/transcript.js: a transcript, the Workbench's transcript kind (bird-captions spec 11.4-11.5, gd-kl2CL3X9jQ, gd-X5dM2VRgDd).
   ?transcript=<id> is one gardener's last calls newest first (ACTIVITY.callRow) from GET /garden/farm/activity/<id>, the whole
   200-call ring in one page; ?transcript=farm|project:<p>|plot:<plan> is every gardener there the viewer's cut keeps, merged by
   time, from GET /garden/farm/activity?scope=, 100 rows a page, each run of rows headed by its gardener's name and arms. Older
   pages come by the answer's next cursor, a `call` event's skipped rows by its prev (never a unix second). Kept live by the farm
   stream's `call` events (web/plans.js: `farmcall`) while the stream holds it open. follow() lends the same rows and cadence
   to the Glass's transcript section (web/glass.js). Text via textContent. */
var TRANSCRIPT = (function () {
  'use strict';
  var A = typeof ACTIVITY !== 'undefined' ? ACTIVITY : require('./activity.js');
  /* ---------- pure logic (also run under node by the Go tests) ---------- */
  var MAX = 200, HOLD = 300;   // rows held: a gardener's, the ring's (transcriptRows); a scope's, three of its pages
  /* A scope, not a gardener: the farm, a project or a plot (transcript.go parseTranscriptScope). */
  function scoped(id) { return id === 'farm' || /^(project|plot):\S/.test(id); }
  function url(id, cur) {
    var c = cur ? 'cursor=' + encodeURIComponent(cur) : '';
    return scoped(id) ? '/garden/farm/activity?scope=' + encodeURIComponent(id) + (c ? '&' + c : '') : '/garden/farm/activity/' + encodeURIComponent(id) + (c ? '?' + c : '');
  }
  /* What tells two rows apart (a scope's rows carry their gardener's id). */
  function key(r) { return [r.id || '', r.at, r.class, r.tool, r.dur, r.ok, r.cap || ''].join('|'); }
  /* a and b as one list newest first by start, less repeats, at most max (MAX): within one second a's rows go first, so a is
     the newer side (live rows, a catch-up page; the page held, before an older page). */
  function merge(a, b, max) {
    var have = {}, out = [];
    a.concat(b).forEach(function (r) { var k = key(r); if (!have[k]) { have[k] = 1; out.push(r); } });
    return out.sort(function (x, y) { return (y.at || 0) - (x.at || 0); }).slice(0, max || MAX);
  }
  /* {class, n} per class, commonest first. */
  function classes(rows) {
    var n = {};
    rows.forEach(function (r) { if (r.class) n[r.class] = (n[r.class] || 0) + 1; });
    return Object.keys(n).map(function (c) { return { class: c, n: n[c] }; }).sort(function (a, b) { return b.n - a.n || (a.class < b.class ? -1 : 1); });
  }
  function kept(rows, cls) { return cls ? rows.filter(function (r) { return r.class === cls; }) : rows; }
  /* Where a scope is, in words: "on the farm", "in gardenio", "on this plot"; '' for a gardener. */
  function where(id) { return id === 'farm' ? 'on the farm' : /^project:/.test(id) ? 'in ' + id.slice(8) : /^plot:/.test(id) ? 'on this plot' : ''; }
  /* The line over the rows; w: where() of a scope, since: the oldest row's clock. */
  function summary(shown, all, cls, w, since) {
    if (!w) {
      if (!all) return 'No calls yet: a row is written when a call finishes.';
      return (cls ? shown + ' of its last ' + all + ' calls were ' + cls : 'Its last ' + (all === 1 ? 'call' : all + ' calls')) + ', newest first.';
    }
    if (!all) return 'No calls ' + w + ' yet: a row is written when a call finishes.';
    return (cls ? shown + ' of the last ' + all + ' calls ' + w + ' were ' + cls : 'The last ' + (all === 1 ? 'call' : all + ' calls') + ' ' + w) + (since ? ', since ' + since : '') + ', newest first.';
  }
  /* The gardeners and plans of the farm document as {id: {name, plan, arms, armsName, title, project}} lookups. */
  function farmWho(doc) {
    var plans = {}, who = {};
    ((doc && doc.plans) || []).forEach(function (p) { plans[p.id] = p; });
    ((doc && doc.gardeners) || []).forEach(function (g) {
      var p = g.plot && plans[g.plot.plan];
      who[g.id] = { name: g.agent || g.id, plan: p ? p.id : '', arms: (p && p.heraldry) || '', armsName: (p && p.heraldry_name) || '' };
    });
    return { who: who, plans: plans };
  }

  var api = { MAX: MAX, HOLD: HOLD, scoped: scoped, url: url, key: key, merge: merge, classes: classes, kept: kept, where: where, summary: summary, farmWho: farmWho };
  if (typeof document === 'undefined' || typeof WORKBENCH === 'undefined') return api;

  /* ---------- DOM ---------- */
  var W = WORKBENCH, B = document.body, base = (B && B.dataset.base) || '';
  var titleEl = document.getElementById('bv-title'), kindEl = document.getElementById('bv-kind'), bodyEl = document.getElementById('bv-body');
  if (!bodyEl || typeof fetch === 'undefined') return api;
  /* S: the panel shown; doc: its page landed; pend: live rows before it did; next/prev: its cursors; cut: live rows pushed
     paged ones out */
  var S = null, sid = '', subbed = '', birds = {};

  function el(t, cls, text) { var e = document.createElement(t); if (cls) e.className = cls; if (text !== undefined) e.textContent = text; return e; }
  function nameOf(id) { var k = window.GARDEN && GARDEN.known ? GARDEN.known(id) : null; return (k && k.title) || id; }
  function farm() { return farmWho(window.GARDEN && GARDEN.farm ? GARDEN.farm() : null); }
  function title(id) { return id === 'farm' ? 'The whole farm' : /^project:/.test(id) ? id.slice(8) : /^plot:/.test(id) ? nameOf(id.slice(5)) : nameOf(id); }
  function cap(id) { return scoped(id) ? HOLD : MAX; }
  function getJSON(path) {
    return fetch(base + path, { credentials: 'same-origin', redirect: 'manual', cache: 'no-store', headers: { Accept: 'application/json' } })
      .then(function (r) { if (!r.ok) throw new Error(r.status); return r.json(); });
  }
  /* Hold the stream's transcript on id, or let it go (''). */
  function sub(id) {
    var want = id ? sid + '|' + id : '';
    if (!sid || subbed === want) return;
    subbed = want;
    var undo = function () { if (want && subbed === want) subbed = ''; };   // tried again on the next open or stream
    fetch(base + '/garden/farm/events/view', { method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ sid: sid, transcript: id }) })
      .then(function (r) { if (!r.ok) undo(); }, undo);
  }
  /* The class's bird at 2 px a pixel, night palette, drawn once per species. */
  function bird(cls) {
    var sp = typeof BIRDS !== 'undefined' ? BIRDS.speciesOf(cls) : '', s = sp && BIRDS.SP[sp];
    var c = el('canvas', 'tx-g'); c.width = 32; c.height = 20; c.setAttribute('aria-hidden', 'true');
    if (!s || !c.getContext) return c;
    if (!birds[sp]) {
      var b = birds[sp] = el('canvas'), x = b.getContext('2d'), n = 2, x0 = (32 - s.w * n) / 2, y0 = (20 - s.h * n) / 2, rim = BIRDS.rimOf(s, true);
      b.width = 32; b.height = 20;
      if (rim) { x.fillStyle = rim; s.runs[1].forEach(function (r) { x.fillRect(x0 + r.x * n - 1, y0 + r.y * n - 1, r.w * n + 2, n + 2); }); }
      s.runs[1].forEach(function (r) { x.fillStyle = s.night[r.i]; x.fillRect(x0 + r.x * n, y0 + r.y * n, r.w * n, n); });
    }
    c.getContext('2d').drawImage(birds[sp], 0, 0);
    return c;
  }
  /* A scope row's head: its gardener's plot's arms (one picture per sky, garden.css .ar-pic) and its name, which opens its own
     transcript. */
  function head(id, w) {
    var h = el('p', 'tx-who'), g = w[id] || { name: nameOf(id) };
    if (g.arms && typeof ARMS !== 'undefined') {
      var box = h.appendChild(el('span', 'ar-pic'));
      [false, true].forEach(function (night) { var i = box.appendChild(el('img', night ? 'ar-night' : 'ar-day')); i.height = 14; i.decoding = 'async'; i.alt = g.armsName; i.src = ARMS.svgSrc(base, g.arms, night); });
    }
    var a = h.appendChild(el('a', 'bl', g.name)); a.href = W.href('transcript', id); a.setAttribute('aria-label', 'Read the transcript of ' + g.name);
    return h;
  }
  function row(r) {
    var v = A.callRow(r), li = el('li', 'tx-row' + (v.failed ? ' failed' : ''));
    var t = li.appendChild(el('time', 'tx-t', v.time)); t.dateTime = new Date((r.at || 0) * 1000).toISOString();
    li.appendChild(bird(v.class));
    var w = li.appendChild(el('span', 'tx-w', v.words));
    w.appendChild(el('span', 'tx-c', ' ' + (v.class || 'call')));   // the class in words, never the bird alone
    var d = li.appendChild(el('span', 'tx-d', v.failed ? 'failed' + (v.dur ? ' · ' + v.dur : '') : v.open ? 'unfinished' : v.dur));
    if (!d.textContent) d.textContent = '—';
    return li;
  }
  function paintNow() {
    if (!S || !S.now) return;
    var d = A.of(S.id), t = Date.now() / 1000;
    S.now.hidden = !(d && d.open);
    if (S.now.hidden) return;
    S.now.textContent = 'Now: ' + (S.cap && S.cap.at === d.at ? S.cap.text : A.toolWords(d, t)) + ' · started ' + Math.max(0, Math.round(t - d.at)) + ' s ago';
  }
  function paintRows() {
    if (!S || !S.list) return;
    var shown = kept(S.rows, S.cls), last = S.rows[S.rows.length - 1], w = S.scope ? farm().who : null, was = '';
    S.sum.textContent = summary(shown.length, S.rows.length, S.cls, where(S.id), last && A.clock(last.at));
    S.list.textContent = '';
    shown.forEach(function (r) {
      var li = S.list.appendChild(row(r));
      if (w && r.id !== was) li.insertBefore(head(r.id, w), li.firstChild);   // a run of one gardener's rows is headed once
      was = r.id;
    });
    if (typeof TITLES !== 'undefined') TITLES.dress(S.list);   // a bead id in a caption reads as its title, and opens it
    var cs = classes(S.rows), pick = S.pick.lastChild;
    S.pick.hidden = cs.length < 2;
    pick.textContent = '';
    [{ class: '', n: S.rows.length }].concat(cs).forEach(function (c) {
      var o = pick.appendChild(el('option', '', (c.class || 'every class') + ' (' + c.n + ')')); o.value = c.class; o.selected = S.cls === c.class;
    });
    S.older.hidden = !S.next;
  }
  /* The rows older than those held, by the next cursor. */
  function older() {
    var my = S, b = S.older;
    b.disabled = true; b.textContent = 'Loading…';
    getJSON(url(my.id, my.next)).then(function (d) {
      if (S !== my) return;
      S.rows = merge(S.rows, d.rows || [], cap(S.id)); S.next = d.next || '';
      b.disabled = false; b.textContent = 'Show older calls'; paintRows();
    }, function () { if (S === my) { b.disabled = false; b.textContent = 'Could not load them: try again'; } });
  }
  /* The rows newer than the newest page held, by its prev cursor: what a `call` event's skipped counted, up to n pages. */
  function catchUp(n) {
    var my = S;
    if (!my.prev || my.busy) return;
    my.busy = true;
    getJSON(url(my.id, my.prev)).then(function (d) {
      my.busy = false;
      if (S !== my) return;
      S.rows = merge(d.rows || [], S.rows, cap(S.id)); S.prev = d.prev || S.prev;
      paintRows();
      if (d.more && n > 1) catchUp(n - 1);
    }, function () { my.busy = false; });
  }
  function link(acts, text, kind, id) { var a = acts.appendChild(el('a', 'btn small ghost', text)); a.href = W.href(kind, id); return a; }
  function paint(id, d, text, isErr, retry) {
    if (!S || S.id !== id) S = { id: id, scope: scoped(id), rows: [], cls: '', doc: false, pend: [], cap: null, next: '', prev: '', cut: false };
    var f = farm(), g = f.who[id], plot = /^plot:/.test(id) && f.plans[id.slice(5)];
    kindEl.textContent = 'transcript';
    titleEl.textContent = title(id);
    bodyEl.textContent = '';
    var acts = isErr && !S.scope ? el('div') : bodyEl.appendChild(el('div', 'bv-acts tx-acts'));   // no way on to a gardener the farm does not have
    if (!S.scope) {
      if (B.dataset.glass === '1') link(acts, 'Watch live', 'glass', id).setAttribute('aria-label', 'Watch ' + nameOf(id) + ' live in the Glass');
      link(acts, 'Open the agent bead', 'bead', id);
      if (g && g.plan) link(acts, 'Every call on its plot', 'transcript', 'plot:' + g.plan);
    } else {
      if (plot) { link(acts, 'Open the plan', 'bead', plot.id); if (plot.project) link(acts, 'Every call in ' + plot.project, 'transcript', 'project:' + plot.project); }
      if (id !== 'farm') link(acts, 'Every call on the farm', 'transcript', 'farm');
    }
    var n = bodyEl.appendChild(el('p', 'bv-note' + (isErr ? ' err' : ''), text || '')); n.setAttribute('role', 'status');
    if (isErr && retry) { var b = n.appendChild(el('button', 'btn small bv-retry', 'Try again')); b.type = 'button'; b.onclick = function () { W.reload('transcript', id); }; }
    S.now = S.scope ? null : bodyEl.appendChild(el('p', 'tx-now'));
    if (S.now) { S.now.setAttribute('aria-live', 'polite'); paintNow(); }
    if (!d) { S.list = null; return; }
    S.rows = merge(S.pend, d.rows || [], cap(id)); S.pend = []; S.doc = true; S.next = d.next || ''; S.prev = d.prev || ''; S.cut = false;
    if (S.cls && !S.rows.some(function (r) { return r.class === S.cls; })) S.cls = '';
    S.pick = bodyEl.appendChild(el('label', 'tx-pick', 'Show calls of '));
    var pick = S.pick.appendChild(el('select', 'bw-in'));
    pick.onchange = function () { S.cls = pick.value; paintRows(); };
    S.sum = bodyEl.appendChild(el('p', 'qv-line tx-sum'));
    S.list = bodyEl.appendChild(el('ol', 'tx-list')); S.list.setAttribute('aria-label', 'Calls ' + (S.scope ? where(id) : 'of ' + nameOf(id)) + ', newest first');
    S.older = bodyEl.appendChild(el('button', 'btn small ghost tx-more', 'Show older calls')); S.older.type = 'button'; S.older.onclick = older;
    paintRows();
    S.foot = bodyEl.appendChild(el('p', 'gc-foot'));
    foot();
  }
  function foot() {
    S.foot.textContent = 'New calls arrive as they finish. The garden keeps a gardener\'s last ' + MAX + ' calls, in memory only.' +
      (S.cut ? ' This panel holds the newest ' + HOLD + ': reopen it to page further back.' : '');
  }
  function shown() { var c = W.current(); return c && c.kind === 'transcript' ? c.id : ''; }

  /* follow(id, on): another panel's hold on one gardener's calls (web/glass.js's transcript section, gd-GPu096omMw): its rows
     newest first, at most MAX, fetched now, again on a farmhello, a call event that skipped rows or follow(id) once more, and
     moved on by the stream's call events, as this panel's are. on(rows, status) after each change: status is the failed
     fetch's HTTP status (0: no answer), else undefined. follow('') lets go. */
  var F = null;
  function follow(id, on) {
    if (!id) { if (F) { F = null; if (!shown()) sub(''); } return; }
    if (!F || F.id !== id) F = { id: id, rows: [], busy: false, again: false };
    F.on = on;
    sub(id); fetchF();
  }
  function fetchF() {
    var my = F;
    if (!my) return;
    if (my.busy) { my.again = true; return; }
    my.busy = true;
    var done = function () { my.busy = false; if (my.again && F === my) { my.again = false; fetchF(); } };
    getJSON(url(my.id)).then(function (d) {
      if (F === my) { my.rows = merge(my.rows, (d && d.rows) || [], MAX); my.on(my.rows); }
      done();
    }, function (e) {
      if (F === my) my.on(my.rows, Number(e && e.message) || 0);
      done();
    });
  }
  function followCall(c) {
    var id = F.id, mine = (c.rows || []).filter(function (r) { return r.id === id; }).map(function (r) { var o = {}; for (var k in r) if (k !== 'id') o[k] = r[k]; return o; });
    if (c.skipped) { fetchF(); return; }   // more came than one event carries: the page holds them
    if (mine.length) { F.rows = merge(mine, F.rows, MAX); F.on(F.rows); }
  }

  document.addEventListener('farmhello', function (e) {
    sid = (e.detail && e.detail.sid) || ''; subbed = '';
    if (F) { sub(F.id); fetchF(); }   // what was written while no stream held it
    var id = shown(); if (!id) return;
    sub(id); W.refresh('transcript', id);   // what was written while no stream held it
  });
  document.addEventListener('farmcall', function (e) {
    var c = e.detail || {}, id = shown();
    if (F) followCall(c);
    if (!id || !S || S.id !== id) return;
    var mine = S.scope ? (c.rows || []) : (c.rows || []).filter(function (r) { return r.id === id; }).map(function (r) { var o = {}; for (var k in r) if (k !== 'id') o[k] = r[k]; return o; });
    if (c.skipped && !S.scope) { W.refresh('transcript', id); return; }   // more came than one event carries: the page holds them
    if (mine.length) {
      if (!S.doc) { S.pend = merge(mine, S.pend, cap(id)); return; }
      var all = merge(mine, S.rows, Infinity);
      if (all.length > cap(id) && S.next) { S.next = ''; S.cut = true; foot(); }   // the oldest go, and the cursor past them with them
      S.rows = all.slice(0, cap(id));
      paintRows(); paintNow();
    }
    if (c.skipped && S.doc) catchUp(3);   // the rows the event left out, newer than the page held
  });
  document.addEventListener('farmactivity', function (e) {
    var ev = e.detail;
    if (!S || S.scope || !ev || ev.id !== S.id || ev.id !== shown()) return;
    if (ev.dur === 0 && ev.cap) S.cap = { at: ev.at, text: ev.cap };
    paintNow();
  });
  setInterval(function () { if (S && shown() === S.id && !document.hidden) paintNow(); }, 5000);

  W.register('transcript', {
    noun: 'transcript',
    url: url,
    paint: paint,
    failure: function (status, id) {
      if (scoped(id)) return status === 400 ? { text: 'The farm has no ' + id.replace(':', ' ') + ' to read.', retry: false } : null;
      return status === 404 ? { text: nameOf(id) + ' is not on the farm, or its transcript is not yours to read.', retry: false } : null;
    },
    shown: function (id) { sub(id); },
    closed: function () { S = null; sub(''); }
  });
  api.follow = follow;
  api.row = row;   // one call as this panel lists it, for the Glass's section
  return api;
})();
if (typeof module !== 'undefined') module.exports = TRANSCRIPT;
