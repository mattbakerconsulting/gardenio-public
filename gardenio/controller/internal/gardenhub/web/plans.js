/* /garden plans slot (Homestead Phase 2): each plan a crop tile that opens its bead view, plus the seeds, harvested plans,
   archive piles and the meadow. Data is the /garden/farm document and its SSE patches (docs/homestead-farm-read-model.md),
   viewer-neutral, so "Mine" is applied here. Text goes in through textContent only. */
var PLANS = (function () {
  var W = typeof WORLD !== 'undefined' ? WORLD : require('./world.js'), tile = W.tile, open = W.openOf, needs = W.needsOf;   // the crop, shared with the World (web/scene.js)
  var STATE_LABEL = { in_progress: 'growing', review: 'in review', blocked: 'blocked', open: 'planted', deferred: 'dormant' };

  /* ---------- pure logic (also run under node by the Go tests) ---------- */
  function emptyDoc() {
    return { v: 0, plans: [], seeds: [], gardeners: [], closed_plans: { total: 0, shown: [] }, meadow: { total: 0, with_mr: 0, shown: [] },
      wishes: { open: [], open_total: 0, closed: [], closed_total: 0 }, home: {}, escalations: { open: [], total: 0 }, releases: {}, piles: { closed_decisions: 0, closed_escalations: 0, closed_work: 0, closed_plans: 0 }, meta: { schema: 1, truncated: false, row_cap: 0, rows: 0 }, cost: null };   // cost: absent with no spend source; its rows carry the stewards (WORLD.stewardState)
  }
  function byNewest(a, b) { return ((b.activity || b.updated || 0) - (a.activity || a.updated || 0)) || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0); }
  function upsertSet(list, set) {
    if (!set) return list;
    var rm = {}, up = {}, out = [];
    (set.rm || []).forEach(function (id) { rm[id] = 1; });
    (set.up || []).forEach(function (e) { up[e.id] = e; });
    list.forEach(function (e) { if (!rm[e.id] && !up[e.id]) out.push(e); });
    Object.keys(up).forEach(function (id) { out.push(up[id]); });
    return out.sort(byNewest);
  }
  /* A patch sends a held phase as {id, same: 1} and an unchanged unphased row as {same: 1} (farm_delta.go planDelta);
     fillPlan puts them back, false when the page does not hold one (a gap). */
  function fillPlan(held, p) {
    var byID = {}, ok = true;
    ((held || {}).phases || []).forEach(function (ph) { byID[ph.id] = ph; });
    if (p.phases) p.phases = p.phases.map(function (ph) { if (!ph || !ph.same) return ph; if (!byID[ph.id]) ok = false; return byID[ph.id]; });
    if (p.unphased && p.unphased.same) { if (!held || !held.unphased) ok = false; p.unphased = held && held.unphased; }
    return ok;
  }
  /* A patch either takes the held document from p.prev to p.v, or the caller reconnects (returns null on a gap). */
  function applyPatch(doc, p) {
    if (!doc || p.prev !== doc.v) return null;
    if (p.plans && p.plans.up) {
      var held = {};
      doc.plans.forEach(function (e) { held[e.id] = e; });
      if (!p.plans.up.every(function (e) { return fillPlan(held[e.id], e); })) return null;
    }
    var d = {
      v: p.v, plans: upsertSet(doc.plans, p.plans), seeds: upsertSet(doc.seeds, p.seeds), gardeners: upsertSet(doc.gardeners || [], p.gardeners),
      closed_plans: p.closed_plans || doc.closed_plans, meadow: p.meadow || doc.meadow, wishes: p.wishes || doc.wishes, home: p.home || doc.home, escalations: p.escalations || doc.escalations, releases: p.releases || doc.releases, piles: p.piles || doc.piles, meta: p.meta || doc.meta,
      cost: p.cost || doc.cost
    };
    return d;
  }
  function normalize(doc) {
    var d = emptyDoc();
    Object.keys(d).forEach(function (k) { if (doc && doc[k] !== undefined && doc[k] !== null) d[k] = doc[k]; });
    d.plans = (d.plans || []).slice().sort(byNewest);
    d.seeds = (d.seeds || []).slice().sort(byNewest);
    return d;
  }
  function plural(n, one, many) { return n + ' ' + (n === 1 ? one : many); }
  /* ---------- what waits on whom (gd-TEl9BfDL2z) ----------
     From needs_human and waiting[]: only an unanswered decision or an escalation to approve waits on a human ("needs you");
     an approved one waits on whoever executes it, by name, or on the operator when none is left (gd-g7jfH27efD). An older
     document counts every open ask as a human one. */
  var WAIT_STATE = { answered: 'answered', approved: 'approved', rejected: 'rejected, still open', failed: 'failed, still open' };
  function whom(w) { return w.waits_on || (w.kind === 'decision' ? 'the agent that asked' : 'its executor'); }
  function names(list) { return list.length <= 2 ? list.join(', ') : list.slice(0, 2).join(', ') + ' +' + (list.length - 2); }
  /* When the operator is expected to act (gd-g7jfH27efD): " · due 2026-10-19 21:30Z", or "" without a due time. */
  function due(w) { return w.due ? ' · due ' + new Date(w.due * 1000).toISOString().slice(0, 16).replace('T', ' ') + 'Z' : ''; }
  /* A human ask still to answer or approve; a human wait that names its party ("the operator") is said like the others. */
  function asking(w) { return w.human && !w.waits_on; }
  /* One open item: "decision waiting on you", "escalation to approve", "escalation approved · waiting on <agent>",
     "escalation approved · waiting on the operator · due <time>". */
  function waitText(w) {
    if (asking(w)) return w.kind === 'decision' ? 'decision waiting on you' : 'escalation to approve';
    return w.kind + ' ' + (WAIT_STATE[w.state] || w.state) + ' · waiting on ' + whom(w) + due(w);
  }
  /* A plan's open items as phrases, {text, human}: human asks first, the rest by kind and whom, then a count left out. */
  function waitPhrases(p) {
    var ws = p.waiting || [], n = needs(p), dec = 0, esc = 0, held = 0, groups = [], by = {}, out = [];
    ws.forEach(function (w) {
      if (asking(w)) { if (w.kind === 'decision') dec++; else esc++; return; }
      if (w.human) held++;
      var k = w.kind + '/' + w.state + '/' + !!w.human, g = by[k];
      if (!g) { g = by[k] = { w: w, n: 0, on: [] }; groups.push(g); }
      g.n++; if (g.on.indexOf(whom(w)) < 0) g.on.push(whom(w));
    });
    if (dec) out.push({ text: plural(dec, 'decision', 'decisions') + ' waiting on you', human: true });
    if (esc) out.push({ text: plural(esc, 'escalation', 'escalations') + ' to approve', human: true });
    if (n > dec + esc + held) out.push({ text: (dec + esc ? '+' : '') + plural(n - dec - esc - held, 'decision', 'decisions') + ' waiting on you', human: true });
    groups.sort(function (a, b) { return !!b.w.human - !!a.w.human; });   // human waits first, as in waiting[]
    groups.forEach(function (g) {
      out.push({ text: (g.n === 1 ? g.w.kind : g.n + ' ' + g.w.kind + 's') + ' ' + (WAIT_STATE[g.w.state] || g.w.state) + ' · waiting on ' + names(g.on) + (g.n === 1 ? due(g.w) : ''), human: !!g.w.human });
    });
    var more = (p.open_decisions || 0) + (p.open_escalations || 0) - Math.max(ws.length, n);
    if (p.waiting && more > 0) out.push({ text: '+' + more + ' more open', human: false });
    return out;
  }
  function waitLine(p) { return waitPhrases(p).map(function (x) { return x.text; }).join('; '); }
  /* The short form for a sign with little room: the need count when a human is wanted, else whom the plan waits on. */
  function waitShort(p) {
    var n = needs(p), on = [];
    if (n) return plural(n, 'needs you', 'need you');
    (p.waiting || []).forEach(function (w) { if (!w.human && on.indexOf(whom(w)) < 0) on.push(whom(w)); });
    return on.length === 1 ? 'waiting on ' + on[0] : on.length ? 'waiting on ' + on.length + ' agents' : '';
  }
  /* The head-of-slot tally: how many plans, and how many are in each status (the same crop the tile shows). */
  var STATUS = [['post', 'waiting on a decision', 'ask'], ['blocked', 'blocked', 'blk'], ['fruit', 'in review', 'rev'], ['sprout', 'growing', 'wip'], ['wilt', 'idle 12h+', 'idl']];
  function summary(plans, now) {
    var s = { total: plans.length, by: { post: 0, blocked: 0, fruit: 0, sprout: 0, wilt: 0 } };
    plans.forEach(function (p) { s.by[tile(p, now).crop]++; });
    return s;
  }

  /* ---------- the forest: /plans ?view=tree's glyph (cmd/beads-viewer/plans_tree.go) on the farm doc ----------
     Same shape and constants: a trunk, a limb per phase, a leaf per step along the limb's outer 65%. The doc carries
     per-phase counts by state, so leaves are drawn per count in LEAF_ORDER (closed at the trunk): /plans' silhouette. */
  var TREE = { W: 100, H: 120, GROUND: 116, APEX: 20, X: 50, MAX_BRANCHES: 9, MAX_LEAVES: 12, LABEL: 18 };
  var LEAF_ORDER = ['closed', 'review', 'in_progress', 'blocked', 'open', 'deferred'];
  function leafRadius(n) { return n <= 4 ? 3.4 : n <= 7 ? 2.9 : n <= 10 ? 2.4 : 2.0; }
  function shortLabel(s) {
    var r = Array.from(String(s || '').trim());
    return r.length <= TREE.LABEL ? r.join('') : r.slice(0, TREE.LABEL).join('').replace(/\s+$/, '') + '…';
  }
  function counts(c) {
    c = c || {};
    return { in_progress: c.in_progress || 0, review: c.review || 0, blocked: c.blocked || 0, open: c.open || 0, deferred: c.deferred || 0, closed: c.closed || 0 };
  }
  /* One phase's leaf states in LEAF_ORDER. Past MAX_LEAVES finished work drops first, so live work never disappears;
     the shortfall is `over`, so the glyph says it was clipped. */
  var CLIP_ORDER = ['closed', 'deferred', 'open', 'review', 'in_progress', 'blocked'];
  function phaseLeaves(c) {
    var n = counts(c), all = [], total = 0;
    LEAF_ORDER.forEach(function (k) { total += n[k]; });
    var over = Math.max(0, total - TREE.MAX_LEAVES), drop = over;
    CLIP_ORDER.forEach(function (k) { var d = Math.min(n[k], drop); n[k] -= d; drop -= d; });
    LEAF_ORDER.forEach(function (k) { for (var i = 0; i < n[k]; i++) all.push(k); });
    return { leaves: all, over: over };
  }
  /* The stand a plan grows in: waiting on a human, being worked on, or just planted. */
  function stand(p, now) {
    var t = tile(p, now);
    if (t.needs > 0) return 'needs';
    if (t.counts.in_progress > 0 || t.counts.review > 0) return 'growing';
    return 'planted';
  }
  var STANDS = [{ key: 'needs', label: 'Needs you' }, { key: 'growing', label: 'Growing' }, { key: 'planted', label: 'Planted' }];
  function tree(p) {
    var groups = (p.phases || []).map(function (ph) { return { title: ph.title || ph.id || '', counts: ph.counts }; });
    var u = p.unphased;
    if (u && u.counts && open(counts(u.counts)) + counts(u.counts).closed > 0) groups.push({ title: '', counts: u.counts, other: true });
    var out = { branches: [], overflow: 0, bare: false, crew: (p.counts && p.counts.in_progress > 0) ? p.counts.in_progress : 0 };
    if (groups.length > TREE.MAX_BRANCHES) { out.overflow = groups.length - TREE.MAX_BRANCHES; groups = groups.slice(0, TREE.MAX_BRANCHES); }
    if (!groups.length) { out.bare = true; return out; }
    var n = groups.length, step = (TREE.GROUND - TREE.APEX) / (n + 1);
    groups.forEach(function (g, i) {
      var side = i % 2 === 0 ? -1 : 1, y = TREE.GROUND - (i + 1) * step;
      var taper = n > 1 ? 1 - 0.55 * i / (n - 1) : 1, len = 14 + 30 * taper, dx = side * len, dy = -0.34 * len;
      var pl = phaseLeaves(g.counts), b = {
        title: g.title, label: shortLabel(g.title), side: side, x1: TREE.X, y1: y, x2: TREE.X + dx, y2: y + dy,
        lx: TREE.X + dx / 2, ly: y + dy / 2, leaves: [], over: pl.over, empty: !pl.leaves.length, done: counts(g.counts).closed
      };
      var norm = Math.hypot(dx, dy) || 1, ux = dx / norm, uy = dy / norm, px = -uy, py = ux, r = leafRadius(pl.leaves.length);
      pl.leaves.forEach(function (state, j) {
        var m = pl.leaves.length, frac = m > 1 ? 0.35 + 0.65 * j / (m - 1) : 1, off = (j % 2 === 1 ? -1 : 1) * r * 0.95;
        b.leaves.push({ state: state, cx: TREE.X + ux * frac * norm + px * off, cy: y + uy * frac * norm + py * off, r: r });
      });
      out.branches.push(b);
    });
    return out;
  }
  function mine(operator, me) { return !me || operator === me; }
  function safeURL(u) { return /^https?:\/\//i.test(u || '') ? u : ''; }

  /* A plan's spend (gd-BL2JQbsfyy): the rollup on its epic (farm_plan_cost.go), the /plans crew card's figure to the cent
     (plans_crew_rollup_agree_test.go). No rollup yet is a dash, never $0; a document with no figure says nothing of spend. */
  function hasCost(c) { return !!(c && c.has && typeof c.usd === 'number'); }
  function spendText(c) { return hasCost(c) ? '$' + c.usd.toFixed(2) + ', ' + plural(c.crew || 0, 'gardener', 'gardeners') + (c.stale ? ' · stale' : '') : '—'; }
  function planLists(d) { return d ? [].concat(d.plans || [], d.seeds || [], (d.closed_plans || {}).shown || []) : []; }
  function spendShown(d) { return planLists(d).some(function (x) { return hasCost(x.cost); }); }
  function costOf(d, id) { var x = planLists(d).filter(function (x) { return x.id === id; })[0]; return x ? x.cost || { has: false, usd: null } : null; }
  /* Most spent first, no figure last, newest among equals. */
  function spendOf(p) { return hasCost(p.cost) ? p.cost.usd : -1; }
  function sortPlans(ps, by) { return by !== 'spend' ? ps : ps.slice().sort(function (a, b) { return (spendOf(b) - spendOf(a)) || byNewest(a, b); }); }

  var api = { emptyDoc: emptyDoc, normalize: normalize, applyPatch: applyPatch, tile: tile, summary: summary, STATUS: STATUS, tree: tree, stand: stand, STANDS: STANDS, phaseLeaves: phaseLeaves, TREE: TREE, mine: mine, safeURL: safeURL, STATE_LABEL: STATE_LABEL,
    needs: needs, waitText: waitText, waitPhrases: waitPhrases, waitLine: waitLine, waitShort: waitShort,
    spendText: spendText, spendShown: spendShown, costOf: costOf, sortPlans: sortPlans };
  if (typeof document === 'undefined') return api;

  /* ---------- DOM ---------- */
  var B = document.body, slot = document.getElementById('plans-slot');
  if (!slot || typeof fetch === 'undefined' || typeof EventSource === 'undefined') return api;
  var base = B.dataset.base || '', me = B.dataset.me || '';
  var doc = null, es = null, queued = false;
  var lastMsg = Date.now(), loadErr = '', failures = 0, wasStale = false, retryTimer = 0;
  var STALE_MS = 20000, WATCHDOG_MS = 45000;   // the server heartbeats every 15 s
  var openPhases = {}, shownAll = {};
  var mode = pref('plans.mode', ['crops', 'forest'], 'crops'), zoom = pref('plans.zoom', ['canopy', 'grove', 'tree'], 'grove'), order = pref('plans.sort', ['newest', 'spend'], 'newest');
  function pref(key, ok, dflt) { try { var v = localStorage.getItem(key); return ok.indexOf(v) >= 0 ? v : dflt; } catch (e) { return dflt; } }
  function setPref(key, v) { try { localStorage.setItem(key, v); } catch (e) { /* a per-viewer convenience only */ } }

  function el(t, cls, text) { var e = document.createElement(t); if (cls) e.className = cls; if (text !== undefined) e.textContent = text; return e; }
  /* A bead reference opens the bead view (web/workbench.js catches the click; ?bead= reopens it). */
  /* A bead link reads as its title (titles, not ids: web/titles.js), the id in its tooltip. */
  function link(id, text, cls) {
    var a = el('a', cls || 'bl', text || (typeof TITLES !== 'undefined' ? TITLES.text(id) : id));
    a.href = base + '/garden?bead=' + encodeURIComponent(id); a.title = id; a.dataset.bead = id;
    return a;
  }
  function dress(root) { if (typeof TITLES !== 'undefined') TITLES.dress(root); }
  function ago(t) { var d = Math.max(0, Math.floor(Date.now() / 1000) - t); if (d < 60) return d + 's'; if (d < 3600) return Math.floor(d / 60) + 'm'; if (d < 86400) return Math.floor(d / 3600) + 'h'; return Math.floor(d / 86400) + 'd'; }
  function scopeMine() { return B.classList.contains('scope-mine'); }
  function visiblePlans() { return doc.plans.filter(function (p) { return !scopeMine() || mine(p.operator, me); }); }
  function visibleSeeds() { return doc.seeds.filter(function (s) { return !scopeMine() || mine(s.operator, me); }); }

  function sprite(kind) {
    var c = document.createElement('canvas'); c.width = 24; c.height = 28; c.className = 'crop-art'; c.setAttribute('aria-hidden', 'true');
    try { ART.drawLegend(c.getContext('2d'), kind, typeof SKY !== 'undefined' && SKY.night()); } catch (e) { /* the tile reads fine without its picture */ }
    return c;
  }
  function chip(cls, text, title) { var s = el('span', 'pc ' + cls, text); if (title) s.title = title; return s; }
  function spendLine(p, cls) { return spendShown(doc) ? el('span', cls + (hasCost(p.cost) && p.cost.stale ? ' stale' : ''), spendText(p.cost)) : null; }
  function spendLabel(p) { return hasCost(p.cost) ? ', spent ' + spendText(p.cost) : ''; }

  function renderTile(p) {
    var t = tile(p, Date.now() / 1000), b = el('button', 'crop' + (t.needs ? ' ripe' : '') + (t.crop === 'wilt' ? ' wilted' : ''));
    b.type = 'button'; b.dataset.id = p.id;
    b.setAttribute('aria-label', p.title + ': ' + t.pct + '% done, ' + plural(t.open, 'step', 'steps') + ' left' + spendLabel(p) + '. Open the plan.');
    b.appendChild(sprite(t.crop));
    var body = el('span', 'crop-body');
    body.appendChild(el('span', 'crop-title', p.title));
    var meta = el('span', 'crop-meta');
    if (p.project) meta.appendChild(el('span', 'pc', p.project));
    if (p.state && p.state !== 'building') meta.appendChild(el('span', 'pc', p.state));
    meta.appendChild(el('span', 'crop-age', ago(p.activity || p.updated) + ' ago'));
    body.appendChild(meta);
    var sp = spendLine(p, 'crop-spend'); if (sp) body.appendChild(sp);
    var bar = el('span', 'bar'); bar.setAttribute('role', 'img'); bar.setAttribute('aria-label', t.done + ' of ' + t.total + ' steps closed');
    var fill = el('span', 'bar-fill'); fill.style.width = t.pct + '%'; bar.appendChild(fill); body.appendChild(bar);
    var chips = el('span', 'crop-chips');
    if (t.counts.in_progress) chips.appendChild(chip('wip', t.counts.in_progress + ' growing'));
    if (t.counts.review) chips.appendChild(chip('rev', t.counts.review + ' in review'));
    if (t.counts.blocked) chips.appendChild(chip('blk', t.counts.blocked + ' blocked'));
    if (t.counts.open) chips.appendChild(chip('opn', t.counts.open + ' planted'));
    waitPhrases(p).forEach(function (x) { chips.appendChild(chip(x.human ? 'ask' : 'wtg', x.text)); });
    chips.appendChild(chip('dn', t.done + '/' + t.total + ' done'));
    body.appendChild(chips); b.appendChild(body);
    b.onclick = function () { openPlan(p.id); };
    return b;
  }

  /* A plan opens in its bead view, the one Space on its plot opens (3b R12): its phases each link to their own. */
  function openPlan(id) { if (typeof BEAD !== 'undefined') BEAD.open(id); }

  function fold(key, summary, build) {
    var d = el('details', 'fold'); d.open = !!openPhases['fold/' + key];
    d.addEventListener('toggle', function () { openPhases['fold/' + key] = d.open; });
    d.appendChild(el('summary', '', summary)); build(d); return d;
  }
  function rowList(items, limit, key, line) {
    var ul = el('ul', 'rows'), cap = shownAll[key] ? items.length : limit;
    items.slice(0, cap).forEach(function (x) { ul.appendChild(line(x)); });
    var box = el('div'); box.appendChild(ul);
    if (items.length > cap) {
      var more = el('button', 'btn small', 'Show ' + (items.length - cap) + ' more'); more.type = 'button';
      more.onclick = function () { shownAll[key] = true; renderPlans(); }; box.appendChild(more);
    }
    return box;
  }


  var SVGNS = 'http://www.w3.org/2000/svg';
  function sv(t, attrs, text) {
    var e = document.createElementNS(SVGNS, t);
    Object.keys(attrs || {}).forEach(function (k) { e.setAttribute(k, attrs[k]); });
    if (text !== undefined) e.textContent = text;
    return e;
  }
  /* One plan as an SVG tree; <title>s and the aria-label carry the states, so the picture is never the only carrier. */
  function renderTree(p) {
    var t = tile(p, Date.now() / 1000), g = tree(p), b = el('button', 'tree' + (t.needs ? ' ripe' : '') + (t.crop === 'wilt' ? ' wilted' : ''));
    b.type = 'button'; b.title = p.title; b.dataset.id = p.id;
    b.setAttribute('aria-label', p.title + ': ' + t.pct + '% done, ' + plural(t.open, 'step', 'steps') + ' left across ' + plural((p.phases || []).length, 'phase', 'phases') +
      (waitLine(p) ? ', ' + waitLine(p) : '') + spendLabel(p) + '. Open the plan.');
    var svg = sv('svg', { viewBox: '0 0 ' + TREE.W + ' ' + TREE.H, 'class': 'tree-svg', 'aria-hidden': 'true', focusable: 'false' });
    svg.appendChild(sv('line', { x1: 8, y1: TREE.GROUND + 2, x2: 92, y2: TREE.GROUND + 2, 'class': 'tr-ground' }));
    svg.appendChild(sv('line', { x1: TREE.X, y1: TREE.GROUND, x2: TREE.X, y2: TREE.APEX, 'class': 'tr-trunk' }));
    g.branches.forEach(function (br) {
      var grp = sv('g', { 'class': 'tr-branch' + (br.empty ? ' bare' : '') });
      grp.appendChild(sv('title', {}, (br.title || 'Other steps') + (br.done ? ' (' + br.done + ' closed)' : '')));
      grp.appendChild(sv('line', { x1: br.x1, y1: br.y1, x2: br.x2, y2: br.y2, 'class': 'tr-limb' }));
      br.leaves.forEach(function (l) {
        var c = sv('circle', { cx: l.cx.toFixed(1), cy: l.cy.toFixed(1), r: l.r, 'class': 'lf lf-' + l.state });
        c.appendChild(sv('title', {}, l.state === 'closed' ? 'closed' : STATE_LABEL[l.state] || l.state));
        grp.appendChild(c);
      });
      if (br.over) grp.appendChild(sv('text', { x: br.x2 + br.side * 3, y: br.y2 + 1.5, 'class': 'tr-over', 'text-anchor': br.side < 0 ? 'end' : 'start' }, '+' + br.over));
      if (br.label) grp.appendChild(sv('text', { x: br.lx, y: br.ly - 3, 'class': 'tr-label', 'text-anchor': 'middle' }, br.label));
      svg.appendChild(grp);
    });
    if (g.overflow) svg.appendChild(sv('text', { x: TREE.X, y: TREE.APEX - 6, 'class': 'tr-over', 'text-anchor': 'middle' }, '+' + g.overflow + ' phases'));
    if (t.needs) svg.appendChild(sv('circle', { cx: TREE.X, cy: TREE.APEX - 8, r: 6, 'class': 'tr-halo' }));
    if (g.crew) svg.appendChild(sv('circle', { cx: TREE.X, cy: TREE.GROUND - 1, r: 3.2, 'class': 'tr-crew' }));
    b.appendChild(svg);
    var cap = el('span', 'tree-cap');
    cap.appendChild(el('span', 'tree-title', p.title));
    cap.appendChild(el('span', 'tree-sub', t.done + '/' + t.total + ' done' + (t.needs ? ' · ' + waitShort(p) : '')));
    var sp = spendLine(p, 'tree-spend'); if (sp) cap.appendChild(sp);
    b.appendChild(cap);
    b.onclick = function () { openPlan(p.id); };
    return b;
  }
  function renderForest(plans) {
    var box = el('div', 'forest zoom-' + zoom), now = Date.now() / 1000;
    var leg = el('p', 'forest-key');
    [['closed', 'closed'], ['review', 'in review'], ['in_progress', 'growing'], ['blocked', 'blocked'], ['open', 'planted'], ['deferred', 'dormant']].forEach(function (k) {
      var i = el('span', 'fk'); i.appendChild(el('i', 'lf-key lf-' + k[0])); i.appendChild(document.createTextNode(k[1])); leg.appendChild(i);
    });
    leg.appendChild(el('span', 'fk-note', 'A limb is a phase, a leaf is a step. Gold halo: a decision is waiting. Green dot: work in flight.'));
    box.appendChild(leg);
    STANDS.forEach(function (st) {
      var ps = plans.filter(function (p) { return stand(p, now) === st.key; });
      if (!ps.length) return;
      var sec = el('section', 'stand stand-' + st.key), h = el('h3', 'stand-h', st.label);
      h.appendChild(el('span', 'n', String(ps.length))); sec.appendChild(h);
      var row = el('div', 'trees'); ps.forEach(function (p) { row.appendChild(renderTree(p)); }); sec.appendChild(row); box.appendChild(sec);
    });
    return box;
  }
  function segButton(label, on, fn) {
    var b = el('button', 'btn small', label); b.type = 'button'; b.setAttribute('aria-pressed', String(on)); b.onclick = fn; return b;
  }

  /* No document yet: say so (loading, or why it failed and when it retries), never a blank slot. */
  function renderProblem() {
    queued = false;
    if (doc) return;
    slot.textContent = '';
    var head = el('div', 'plans-head'); head.appendChild(el('h2', '', 'Plans')); slot.appendChild(head);
    if (loadErr) {
      var p = el('p', 'warn', 'The plans did not load (' + loadErr + '). Trying again shortly. The rest of the garden still works.');
      p.setAttribute('role', 'alert'); slot.appendChild(p);
      var b = el('button', 'btn small', 'Try again now'); b.type = 'button'; b.onclick = function () { clearTimeout(retryTimer); load(); }; slot.appendChild(b);
    } else {
      var l = el('p', 'more', 'Loading the plans…'); l.setAttribute('role', 'status'); slot.appendChild(l);
    }
    slot.hidden = false;
  }
  function isStale() { return !!doc && (!es || es.readyState !== 1) && Date.now() - lastMsg > STALE_MS; }
  function renderPlans() {
    queued = false;
    if (!doc) { renderProblem(); return; }
    var ae = document.activeElement, focusID = ae && (ae.classList.contains('crop') || ae.classList.contains('tree')) ? ae.dataset.id : '';
    slot.textContent = '';
    var plans = sortPlans(visiblePlans(), spendShown(doc) ? order : 'newest'), seeds = visibleSeeds();
    var head = el('div', 'plans-head');
    head.appendChild(el('h2', '', 'Plans'));
    head.appendChild(el('span', 'n', plural(plans.length, 'plan', 'plans')));
    var all = doc.plans.length;
    head.appendChild(el('span', 'hint', scopeMine() ? 'yours · ' + all + ' in all, switch to Everyone to see them' : 'everyone\'s · being built'));
    var views = el('div', 'seg plans-views'); views.setAttribute('role', 'group'); views.setAttribute('aria-label', 'Plans view');
    [['crops', 'Crops'], ['forest', 'Forest']].forEach(function (m) {
      views.appendChild(segButton(m[1], mode === m[0], function () { mode = m[0]; setPref('plans.mode', mode); renderPlans(); }));
    });
    head.appendChild(views);
    if (mode === 'forest') {
      var zs = el('div', 'seg plans-zoom'); zs.setAttribute('role', 'group'); zs.setAttribute('aria-label', 'Forest zoom');
      [['canopy', 'Canopy'], ['grove', 'Grove'], ['tree', 'Tree']].forEach(function (z) {
        zs.appendChild(segButton(z[1], zoom === z[0], function () { zoom = z[0]; setPref('plans.zoom', zoom); renderPlans(); }));
      });
      head.appendChild(zs);
    }
    if (spendShown(doc)) {
      var so = el('div', 'seg plans-sort'); so.setAttribute('role', 'group'); so.setAttribute('aria-label', 'Sort plans by');
      [['newest', 'Newest'], ['spend', 'Spend']].forEach(function (o) {
        so.appendChild(segButton(o[1], order === o[0], function () { order = o[0]; setPref('plans.sort', order); renderPlans(); }));
      });
      head.appendChild(so);
    }
    slot.appendChild(head);
    wasStale = isStale();
    if (wasStale) {
      var n = el('p', 'warn', 'Live updates paused. These plans are as of ' + ago(Math.floor(lastMsg / 1000)) + ' ago; reconnecting.');
      n.setAttribute('role', 'status'); slot.appendChild(n);
    }
    if (plans.length) {
      var sm = summary(plans, Date.now() / 1000), tally = el('div', 'tally');
      STATUS.forEach(function (st) { if (sm.by[st[0]]) tally.appendChild(chip(st[2], sm.by[st[0]] + ' ' + st[1])); });
      slot.appendChild(tally);
    }
    if (doc.meta && doc.meta.truncated) slot.appendChild(el('p', 'warn', 'The farm read hit its row cap (' + doc.meta.row_cap + '): the newest rows are shown and some counts may be low.'));
    if (!plans.length) {
      slot.appendChild(el('p', 'more', scopeMine() && doc.plans.length ? 'None of the ' + doc.plans.length + ' plans being built is yours. Switch to Everyone to see them.' : 'No plans are being built right now.'));
    } else {
      if (mode === 'forest') slot.appendChild(renderForest(plans));
      else {
        var grid = el('div', 'crops');
        plans.forEach(function (p) { grid.appendChild(renderTile(p)); });
        slot.appendChild(grid);
      }
    }
    var folds = el('div', 'folds');
    if (seeds.length) {
      var fr = seeds.filter(function (s) { return s.state === 'frosted'; }).length;
      folds.appendChild(fold('seeds', 'Seeds: ' + fr + ' frosted (drafting) · ' + (seeds.length - fr) + ' ready to approve', function (d) {
        d.appendChild(rowList(seeds, 30, 'seeds', function (s) {
          var li = el('li', 'row seed ' + s.state);
          li.appendChild(el('span', 'sb', s.state));
          li.appendChild(link(s.id, s.title, 'bl'));
          li.appendChild(el('span', 'sm', plural(s.phases, 'phase', 'phases') + ' · ' + plural(s.tasks, 'step', 'steps') + (s.project ? ' · ' + s.project : '')));
          return li;
        }));
      }));
    }
    var cp = doc.closed_plans || { total: 0, shown: [] };
    if (cp.total) {
      folds.appendChild(fold('closed', 'Harvested plans: ' + cp.total, function (d) {
        d.appendChild(rowList(cp.shown || [], 12, 'closed', function (x) {
          var li = el('li', 'row'); li.appendChild(el('span', 'sb', x.state || 'complete'));
          li.appendChild(link(x.id, x.title, 'bl')); li.appendChild(el('span', 'sm', ago(x.updated) + ' ago' + (x.project ? ' · ' + x.project : ''))); return li;
        }));
        if (cp.total > (cp.shown || []).length) d.appendChild(el('p', 'more', 'The newest ' + cp.shown.length + ' of ' + cp.total + ' are listed.'));
      }));
    }
    var pl = doc.piles || {};
    if (pl.closed_escalations || pl.closed_decisions || pl.closed_work) {
      slot.appendChild(el('p', 'piles', 'Archive piles (counted, never listed): ' + (pl.closed_escalations || 0) + ' escalations · ' + (pl.closed_decisions || 0) + ' decisions · ' + (pl.closed_work || 0) + ' closed work beads'));
    }
    var md = doc.meadow || { total: 0, shown: [], with_mr: 0 };
    if (md.total) {
      folds.appendChild(fold('meadow', 'Meadow: ' + md.total + ' loose beads' + (md.with_mr ? ' · ' + md.with_mr + ' with a PR' : ''), function (d) {
        d.appendChild(rowList(md.shown || [], 15, 'meadow', function (k) {
          var li = el('li', 'row'); li.appendChild(el('span', 'sb', STATE_LABEL[k.state] || k.state));
          li.appendChild(link(k.id, k.title, 'bl'));
          var m = el('span', 'sm', (k.assignee ? k.assignee + ' · ' : '') + ago(k.updated) + ' ago');
          li.appendChild(m);
          if (k.mr && safeURL(k.mr.url)) { var a = el('a', 'bl', k.mr.iid || 'MR'); a.href = safeURL(k.mr.url); a.target = '_blank'; a.rel = 'noopener noreferrer'; li.appendChild(a); }
          return li;
        }));
        if (md.total > (md.shown || []).length) d.appendChild(el('p', 'more', 'The newest ' + md.shown.length + ' of ' + md.total + ' are listed.'));
      }));
    }
    if (folds.childNodes.length) slot.appendChild(folds);
    dress(slot);
    slot.hidden = false;
    if (focusID) { var f = slot.querySelector('[data-id="' + focusID + '"].crop, [data-id="' + focusID + '"].tree'); if (f) f.focus({ preventScroll: true }); }
  }
  /* The map (garden.js) lays the world out from this document, handed over by event. */
  function announce() { try { document.dispatchEvent(new CustomEvent('farmdoc', { detail: doc })); } catch (e) { /* no CustomEvent: the map stays empty */ } }
  /* What the next document is, for web/transitions.js: load, reset or a patch with its exits, and when the stream opens
     (its replay does not animate). Sent before announce(). */
  function tell(kind, exits) { try { document.dispatchEvent(new CustomEvent('farmlive', { detail: { kind: kind, exits: exits || [] } })); } catch (e) { /* no transitions */ } }
  function schedule() { if (queued) return; queued = true; (window.requestAnimationFrame || setTimeout)(renderPlans); }

  function connect() {
    if (es) es.close();
    es = LIVE.source(base + '/garden/farm/events?since=' + (doc ? doc.v : 0));   // web/live.js
    es.onopen = function () { lastMsg = Date.now(); tell('open'); if (wasStale) schedule(); };
    es.onerror = function () { if (isStale() !== wasStale) schedule(); };
    es.addEventListener('hb', function () { lastMsg = Date.now(); if (wasStale) schedule(); });
    /* What a gardener is doing (web/activity.js): ephemeral, outside the patch chain, handed over and never stored. */
    es.addEventListener('activity', function (e) { lastMsg = Date.now(); try { document.dispatchEvent(new CustomEvent('farmactivity', { detail: JSON.parse(e.data) })); } catch (x) { /* no CustomEvent: no activity */ } });
    /* The stream's id, for the page's view (web/viewcut.js), and what it sends in place of every call: busy projects
       (web/birds.js), a gardener's doing (web/activity.js), a held transcript's rows (web/transcript.js). */
    ['hello', 'busy', 'doing', 'call'].forEach(function (n) { es.addEventListener(n, function (e) { lastMsg = Date.now(); try { document.dispatchEvent(new CustomEvent('farm' + n, { detail: JSON.parse(e.data) })); } catch (x) { /* no CustomEvent: the full stream */ } }); });
    es.addEventListener('reset', function (e) { lastMsg = Date.now(); doc = normalize(JSON.parse(e.data)); tell('reset'); announce(); schedule(); });
    es.addEventListener('patch', function (e) {
      lastMsg = Date.now();
      var p = JSON.parse(e.data), next = applyPatch(doc, p);
      if (!next) { connect(); return; }   // gap: reconnect with the version we hold; the server answers with a reset
      doc = next; tell('patch', p.exits); announce(); schedule();
    });
  }
  /* The fetch starts now, but the first document waits for the page's scripts: the parser may yield between bundles, and a
     'farmdoc' listener after this one (web/workbench.js's deep-link flight) would miss it (gd-rM1LY2052v). */
  function parsed(f) { if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', f); else f(); }
  function load() {
    fetch(base + '/garden/farm', { credentials: 'same-origin', headers: { Accept: 'application/json' } })
      .then(function (r) { if (!r.ok) throw new Error('farm ' + r.status); return r.json(); })
      .then(function (j) { var d = normalize(j); parsed(function () { doc = d; loadErr = ''; failures = 0; lastMsg = Date.now(); tell('load'); announce(); schedule(); connect(); }); })
      .catch(function (e) {   // the scene and ledger keep working without the plans
        loadErr = e && e.message ? e.message : 'network error'; failures++;
        schedule();
        retryTimer = setTimeout(load, Math.min(30000, 3000 * failures));
      });
  }
  /* The document last handed over, for a file that runs after it was (web/lazy.js loads the bead view and the Commons late). */
  api.doc = function () { return doc; };
  new MutationObserver(function () { if (doc) schedule(); }).observe(B, { attributes: true, attributeFilter: ['class'] });
  document.addEventListener('visibilitychange', function () {
    if (document.hidden) { if (es) { es.close(); es = null; } } else if (doc) { connect(); }
  });
  if (typeof SKY !== 'undefined') SKY.on(function () { schedule(); });   // the sky flipped (web/sky.js): the crop sprites are painted per scheme
  setInterval(function () { if (doc && !document.hidden) schedule(); }, 60000);   // ages and wilt
  /* A stream dead without an error event: no message (heartbeats count) for 45 s means reconnect. */
  setInterval(function () {
    if (!doc || document.hidden) return;
    if (es && Date.now() - lastMsg > WATCHDOG_MS) connect();
    if (isStale() !== wasStale) schedule();
  }, 5000);
  load();
  return api;
})();
if (typeof module !== 'undefined') module.exports = PLANS;
