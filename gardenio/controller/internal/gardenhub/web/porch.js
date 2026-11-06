/* The Porch (gardenio/docs/homestead-agents-as-people.md sections 2 and 6, gd-ikrXlqYTL5): gardeners[] grouped by the plot
   each stands on, those at home under "In the field". A row: sprite, name (a link to its bead; web/chat.js opens its Commons
   thread on a plain click), role, state in words, spend, since when, the step it tends, where it stands ("In the field of
   <project> and on <plot>", 28.12), "Go to plot". web/garden.js owns the camera: plotOf (where the map drew each one, so
   Porch and map agree), whereOf (its place words) and go (the flight). With no gardeners[] the page
   mirrors the Ledger's agents here, as each project's barn does (section 4.4, 28.4). */
var PORCH = (function () {
  var W = typeof WORLD !== 'undefined' ? WORLD : require('./world.js');
  var R = typeof REASON !== 'undefined' ? REASON : require('./reason.js');
  var BARN = 'In the field', NEEDS = 'Needs you';   // BARN: those at home, each standing in its project's field (28.12)

  /* ---------- pure logic (also run under node by the Go tests) ---------- */
  /* groups(gardeners, plotOf, opt) -> {groups, n, work, home, land, all, cost}. plotOf(g) is {id, title} on a plot, else null;
     beside a band item or a goat (gd-6FCoWelnx2) it also carries place (the band's name) and name (the bead's title).
     opt.me: the viewer's operated_by alias; opt.only keeps only theirs ("Mine"); opt.land(g): off a plot but at its land's
     homestead (a steward, Lands 9.8). Rows: the viewer's first, then need (WORLD.byNeed). One down or waiting on a human
     (REASON.needsYou, gd-J3l6W4X6DJ) is in "Needs you", always first; then groups holding the viewer's, the neediest row, the
     title; "In the field" (those at home) last. n counts rows, work at work, home at the barn, land at a homestead, all before the scope
     (WORLD.barnWords, 5a); every gardener count reads this (gd-rC46vJomwy). cost: summed WORLD.costWords, {usd, atLeast}, or null. */
  function groups(gardeners, plotOf, opt) {
    opt = opt || {};
    var me = opt.me || '', by = {}, out = [], n = 0, work = 0, home = 0, land = 0, cents = 0, priced = 0, atLeast = false, list = gardeners || [];
    var mine = function (g) { return !!me && g.operator === me; };
    list.forEach(function (g) {
      if (opt.only && !mine(g)) return;
      var p = plotOf(g), need = R.needsYou(g), key = need ? '!needs' : p ? p.id : '', grp = by[key];
      if (!grp) { grp = by[key] = need ? { key: 'needs', plan: '', need: true, title: NEEDS, rows: [], mine: false, lead: null } : { key: key || 'barn', plan: key, title: p ? p.title || p.id : BARN, rows: [], mine: false, lead: null }; out.push(grp); }
      grp.rows.push(g); n++; if (!p) { if (opt.land && opt.land(g)) land++; else home++; }
      if (!grp.lead || W.byNeed(g, grp.lead) < 0) grp.lead = g;   // the neediest, which orders the plots
      if (mine(g)) grp.mine = true;
      if (W.gardenerWord(g) === 'At work') work++;
      var cw = W.costWords(g); if (cw) { cents += Math.round(cw.usd * 100); priced++; atLeast = atLeast || cw.atLeast; }
    });
    out.forEach(function (grp) { grp.rows.sort(function (a, b) { return (mine(b) ? 1 : 0) - (mine(a) ? 1 : 0) || W.byNeed(a, b); }); });
    out.sort(function (a, b) {
      return (b.need ? 1 : 0) - (a.need ? 1 : 0) || (a.plan ? 0 : 1) - (b.plan ? 0 : 1) || (b.mine ? 1 : 0) - (a.mine ? 1 : 0) || W.byNeed(a.lead, b.lead) ||
        (a.title < b.title ? -1 : a.title > b.title ? 1 : 0);
    });
    return { groups: out, n: n, work: work, home: home, land: land, all: list.length, cost: priced ? { usd: cents / 100, atLeast: atLeast } : null };
  }
  /* The step a row names: what it tends on its plot, or is assigned at the barn; else where the garden sent it (web/tend.js);
     else the escalation it runs (WORLD.runningWords; w its FarmWait, for the family); for a thread agent at home, "In the
     Commons:" and its thread (WORLD.homeWord, whose "At the barn" means a plain gardener at home). */
  function taskLine(g, p, w) {
    if (g.tends && !g.task) return 'Sent to: ' + (g.tends_title || g.tends) + (p ? '' : ' (not on a plot)');
    if (p && p.place) return 'Tending: ' + (g.task_title || p.name) + ' in ' + p.place;   // beside its wildflower, pot, tree, headstone or goat
    if (g.escalation && !g.task) return W.runningWords(g, w);
    if (!p && W.homeWord(g) !== 'At the barn') return W.homeWord(g);   // a thread agent at home: the thread it answers (5b)
    if (p) return g.task_title ? 'Tending: ' + g.task_title : 'Tending ' + p.title;
    return g.task ? 'Assigned: ' + (g.task_title || g.task) + ' (not on a plot)' : '';
  }
  /* "5 gardeners · 3 at work · spent at least $12.40". */
  function summary(m) {
    return m.n + (m.n === 1 ? ' gardener' : ' gardeners') + ' · ' + m.work + ' at work' +
      (m.cost ? ' · spent ' + (m.cost.atLeast ? 'at least ' : '') + '$' + m.cost.usd.toFixed(2) : '');
  }

  var api = { BARN: BARN, NEEDS: NEEDS, groups: groups, taskLine: taskLine, summary: summary, sync: function () { return false; }, show: function () { return false; } };
  if (typeof document === 'undefined') return api;

  /* ---------- the pane ---------- */
  var ul = document.getElementById('d-porch'), pane = document.getElementById('pane-porch');
  if (!ul || !pane) return api;
  var em = document.getElementById('porch-empty'), sub = document.getElementById('porch-sub');
  var cur = '', go = null, emBase = em ? em.textContent : '';

  function el(tag, cls, text) { var e = document.createElement(tag); if (cls) e.className = cls; if (text !== undefined) e.textContent = text; return e; }
  function header(grp) {
    var li = el('li', 'pgrp' + (grp.need ? ' needs' : '')), h = li.appendChild(el('h3', '', grp.title + ' '));   // then its count, after a dot (garden.css)
    h.appendChild(el('span', 'n', String(grp.rows.length)));
    li.dataset.k = 'grp:' + grp.key; li.dataset.u = grp.title + '|' + grp.rows.length;
    return li;
  }
  /* One gardener's row; data-src i-<agent id>, as a Ledger row, so web/chat.js opens its thread. Down reads red; its reason
     (section 2.3) under it. */
  function row(g, p, o) {
    var pose = R.pose(g), why = R.of(g);
    var li = el('li', 'item gd' + (o.me && g.operator === o.me ? ' mine' : '') + (pose === 'ask' ? ' blocked' : ''));
    li.dataset.k = 'g:' + g.id; li.dataset.src = 'i-' + g.id; li.dataset.gid = g.id; li.dataset.op = g.operator || '';
    var c = li.appendChild(el('canvas', 'gd-pic')); c.width = 24; c.height = 28; c.setAttribute('aria-hidden', 'true');
    if (typeof ART !== 'undefined') ART.drawGardener(c.getContext('2d'), W.gardenerLook(g), 4, 2, 0, !!o.night);
    var t = li.appendChild(el('a', 't', g.agent || g.id)); t.href = o.base + g.href; li._t = t;
    var m = li.appendChild(el('span', 'm')), word = W.gardenerWord(g);
    m.appendChild(el('span', 'chip', String(g.role || 'agent').split(',').join(', ')));   // gardener,plan-gardener reads as two roles
    m.appendChild(el('span', 'st st-' + (pose === 'down' ? 'failed' : g.state || 'idle'), word));
    var cw = W.costWords(g);
    if (cw) { var ch = m.appendChild(el('span', 'chip gd-cost', cw.chip)); ch.setAttribute('role', 'img'); ch.setAttribute('aria-label', cw.label); ch.title = g.cost.text || cw.label; }
    var tm = m.appendChild(el('time', '', o.ago ? o.ago(g.since || 0) + ' ago' : '')); tm.dataset.t = String(g.since || 0);
    if (why && why.text) li.appendChild(el('span', 'gd-why rsn-' + why.kind, why.text));
    var tl = taskLine(g, p, o.escOf ? o.escOf(g) : null);
    if (tl) {   // its task's arms before the words (5.9)
      var ts = li.appendChild(el('span', 'gd-task', tl)), ap = g.task_arms && typeof ARMS !== 'undefined' ? ARMS.pic(o.base, g.task_arms, 16, 'ar-line ar-gd', '', 1) : null;
      if (ap) ts.insertBefore(ap, ts.firstChild);
    }
    var wh = o.whereOf ? o.whereOf(g) : '';
    if (wh) li.appendChild(el('span', 'gd-where', wh));
    var b = li.appendChild(el('button', 'btn small ghost gd-go', p ? p.place ? 'Go there' : 'Go to plot' : 'Go to the field')); b.type = 'button'; b.dataset.gid = g.id; li._go = b;
    b.setAttribute('aria-label', (p ? 'Go to ' + (p.place ? p.place + ': ' + p.name : p.title) : 'Go to the field') + ': ' + (g.agent || g.id));
    return li;
  }
  /* The rows a keyboard walks, in order. */
  function rows() { var a = []; for (var i = 0; i < ul.children.length; i++) if (ul.children[i]._t) a.push(ul.children[i]); return a; }
  /* One tab stop for the list (a WAI-ARIA roving tab stop): the current row's link and its button. */
  function rove() {
    var a = rows(), at = -1;
    a.forEach(function (li, i) { if (li.dataset.gid === cur) at = i; });
    if (at < 0) at = 0;
    a.forEach(function (li, i) { li._t.tabIndex = i === at ? 0 : -1; li._go.tabIndex = i === at ? 0 : -1; li.classList.toggle('here', li.dataset.gid === cur); });
  }

  /* sync(gardeners, o) -> true once it drew the Porch from gardeners[]. o: {me, only, base, night, ago, plotOf, whereOf, go, escOf}. Rows are
     reconciled, reused while unchanged, so focus survives a refresh. */
  function sync(gardeners, o) {
    go = o.go || null;
    var m = groups(gardeners, o.plotOf, o), have = {}, want = [], stale = [], i;
    /* Rows with no data-k are the Ledger's mirror (web/garden.js): all go, not only the last (gd-aVUvUN8gQy). */
    for (i = 0; i < ul.children.length; i++) { if (ul.children[i].dataset.k) have[ul.children[i].dataset.k] = ul.children[i]; else stale.push(ul.children[i]); }
    var keep = function (k, u, make) { var old = have[k]; if (old && old.dataset.u === u) { delete have[k]; want.push(old); return; } var n = make(); n.dataset.u = u; want.push(n); };
    m.groups.forEach(function (grp) {
      keep('grp:' + grp.key, grp.title + '|' + grp.rows.length, function () { return header(grp); });
      grp.rows.forEach(function (g) {
        var p = o.plotOf(g);
        var w = o.escOf ? o.escOf(g) : null;
        keep('g:' + g.id, JSON.stringify([g, p, o.me === g.operator, !!o.night, w && w.family, o.whereOf ? o.whereOf(g) : '']), function () { return row(g, p, o); });
      });
    });
    Object.keys(have).forEach(function (k) { stale.push(have[k]); });
    stale.forEach(function (n) { if (n.parentNode === ul) ul.removeChild(n); });
    want.forEach(function (n, j) { if (ul.children[j] !== n) ul.insertBefore(n, ul.children[j] || null); });
    if (typeof TITLES !== 'undefined') TITLES.dress(ul);   // a reason's or a task's bead id reads as its title (web/titles.js)
    rove();
    pane.classList.toggle('none', m.n === 0);
    if (em) { var msg = m.n === 0 && m.all > 0 ? 'None of yours. ' + m.all + ' more under Everyone.' : emBase; if (em.textContent !== msg) em.textContent = msg; }
    var s = m.all ? summary(m) : '';
    if (sub) sub.textContent = s ? s + '. ' : '';
    ['tpip-porch', 'dpip-porch'].forEach(function (id) { var p = document.getElementById(id); if (p) { p.textContent = m.n > 0 ? String(m.n) : ''; p.title = s; } });
    return true;
  }
  /* show(id) -> the Porch at a gardener's row (its sprite clicked): marked, scrolled to, focused. False when "Mine" left it out. */
  function show(id) {
    var li = null; rows().forEach(function (r) { if (r.dataset.gid === id) li = r; });
    if (!li) return false;
    cur = id; rove();
    if (li.scrollIntoView) li.scrollIntoView({ block: 'nearest' });
    try { li._t.focus({ preventScroll: true }); } catch (e) { li._t.focus(); }
    return true;
  }

  ul.addEventListener('click', function (e) {
    var b = e.target && e.target.closest ? e.target.closest('.gd-go') : null;
    if (!b || !go) return;
    e.preventDefault(); cur = b.dataset.gid; rove(); go(cur);
  });
  ul.addEventListener('focusin', function (e) {
    var li = e.target && e.target.closest ? e.target.closest('li.gd') : null;
    if (li && li.dataset.gid !== cur) { cur = li.dataset.gid; rove(); }
  });
  /* Arrows (and Home/End) move between the rows' links and wrap; Tab reaches the row's Go button. */
  ul.addEventListener('keydown', function (e) {
    var a = rows(), i = -1, to = -1;
    a.forEach(function (li, k) { if (li._t === document.activeElement || li._go === document.activeElement) i = k; });
    if (i < 0) return;
    if (e.key === 'ArrowDown' || e.key === 'ArrowRight') to = (i + 1) % a.length;
    else if (e.key === 'ArrowUp' || e.key === 'ArrowLeft') to = (i + a.length - 1) % a.length;
    else if (e.key === 'Home') to = 0; else if (e.key === 'End') to = a.length - 1;
    if (to < 0) return;
    e.preventDefault(); cur = a[to].dataset.gid; rove(); a[to]._t.focus();
  });

  api.sync = function (gardeners, o) { return sync(gardeners, o); };
  api.show = show;
  return api;
})();
if (typeof module !== 'undefined') module.exports = PORCH;
