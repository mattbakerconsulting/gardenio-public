/* The land header (Lands 8.8, gd-vJK94LpTOm; track-b spec 3.6): on /garden?land=<land> the plaque names the land and,
   under the counts, shows its purse in words and level (watcher Level 50/80/100); the link opens ?costs=land:<land>.
   No purse (operator land) leaves the row empty; a land the farm does not show leaves the plaque as it was. Later items
   join the row: the steward's run cap (gd-OvKMtyWtrp), and Steward, the land's steward view in the workbench
   (/garden/c/s/<land>, its notes and Holds there; steward view S2 gd-LisW87zrnf, in place of the notes dialog). */
var LANDHD = (function () {
  'use strict';
  /* ---------- pure logic (also run under node by the Go tests) ---------- */
  function landOf(search) {
    var out = '';
    String(search || '').replace(/^\?/, '').split('&').forEach(function (kv) {
      var e = kv.indexOf('='), k = e < 0 ? kv : kv.slice(0, e);
      if (k !== 'land' || out) return;
      try { out = decodeURIComponent((e < 0 ? '' : kv.slice(e + 1)).replace(/\+/g, ' ')).trim(); } catch (x) { out = ''; }
    });
    return out;
  }
  /* known: the farm shows the land (a cost row, plan, seed or gardener of it). */
  function known(doc, land) {
    if (!doc || !land) return false;
    var of = function (xs) { return (xs || []).some(function (x) { return x && x.project === land; }); };
    return of(doc.cost && doc.cost.projects) || of(doc.plans) || of(doc.seeds) || of(doc.gardeners);
  }
  function row(doc, land) {
    return ((doc && doc.cost && doc.cost.projects) || []).filter(function (p) { return p && p.project === land; })[0] || null;
  }
  function num(n) {
    if (typeof n !== 'number' || !isFinite(n)) return '—';
    var a = Math.abs(n);
    return a >= 1000 || n === Math.round(n) ? String(Math.round(n)) : a >= 10 ? n.toFixed(1) : n.toFixed(2);
  }
  /* purse(p) -> {left, level, tone, title}, or null (no purse). tone: '', 'warn' or 'alert'. */
  function purse(p) {
    if (!p || p.state === 'unmetered') return null;
    var g = p.granted_credits, r = p.remaining_credits, u = p.used_pct;
    if (p.state === 'unknown') return { left: 'Credits unknown', level: 'new agents wait', tone: 'alert', title: 'The purse can’t be read right now, so new agents wait.' };
    var left = typeof g === 'number' && g > 0 ? num(r) + ' of ' + num(g) + ' credits left' : 'No credits granted yet';
    if (p.state === 'wall') return { left: left, level: 'at the wall', tone: 'alert', title: 'At the wall: new agents wait until more credits are granted.' };
    if (typeof u === 'number' && u >= 80) return { left: left, level: 'low', tone: 'warn', title: Math.round(u) + '% used.' };
    if (typeof u === 'number' && u >= 50) return { left: left, level: 'half used', tone: '', title: Math.round(u) + '% used.' };
    return { left: left, level: 'ok', tone: '', title: typeof u === 'number' ? Math.round(u) + '% used.' : '' };
  }
  /* cap(s) -> the steward's run cap in words while hit, else null */
  function cap(s) {
    var r = s && s.resumes_at;
    return s && s.capped ? { runs: s.runs_today + ' of ' + s.max_runs_per_day + ' runs today', back: 'back at ' + (r > 0 ? new Date(r * 1e3).toISOString().slice(11, 16) : '00:00') + ' UTC' } : null;
  }
  /* stewardHref(base, doc, land) -> the land's steward view while the farm names its steward, else '' */
  function stewardHref(base, doc, land) { return (row(doc, land) || {}).steward ? (base || '') + '/garden/c/s/' + encodeURIComponent(land) : ''; }
  var api = { landOf: landOf, known: known, row: row, num: num, purse: purse, cap: cap, stewardHref: stewardHref };
  if (typeof document === 'undefined') return api;

  /* ---------- the page ---------- */
  var B = document.body, land = landOf(location.search), plaque = document.querySelector('.crumb .plaque');
  var h1 = plaque && plaque.querySelector('h1'), box = document.getElementById('land-hd'), title = document.title;
  if (!land || !h1 || !box) return api;
  var pEl = box.querySelector('.lh-purse'), on = false;
  function paint(doc) {
    var show = known(doc, land);
    if (show !== on) {
      on = show;
      h1.textContent = show ? land : 'The Garden';
      document.title = show ? land + ' — ' + title : title;
      plaque.classList.toggle('is-land', show);
      box.hidden = !show;
    }
    if (!show || !pEl) return;
    var w = purse((row(doc, land) || {}).purse);
    pEl.hidden = !w;
    if (!w) return;
    pEl.href = (B.dataset.base || '') + '/garden?costs=land:' + encodeURIComponent(land);
    pEl.title = w.title;
    pEl.dataset.tone = w.tone;
    pEl.querySelector('.lh-left').textContent = w.left;
    pEl.querySelector('.lh-level').textContent = w.level;
  }
  /* the steward's run cap (gd-OvKMtyWtrp) */
  var cEl = box.querySelector('.lh-cap');
  function paintCap(doc) {
    var st = (row(doc, land) || {}).steward, c = cap(st);
    cEl.hidden = !c;
    if (!c) return;
    cEl.href = (B.dataset.base || '') + st.href;
    cEl.querySelector('.lh-runs').textContent = c.runs;
    cEl.querySelector('.lh-back').textContent = c.back;
  }
  /* Steward: the land's steward view */
  var sEl = box.querySelector('.lh-steward');
  function paintSteward(doc) {
    var h = stewardHref(B.dataset.base, doc, land);
    sEl.hidden = !h;
    if (h) sEl.href = h;
  }
  var g = window.GARDEN, f = g && g.farm ? g.farm() : null;
  paint(f); paintCap(f); paintSteward(f);
  document.addEventListener('farmdoc', function (e) { paint(e.detail); paintCap(e.detail); paintSteward(e.detail); });
  return api;
})();
if (typeof module !== 'undefined') module.exports = LANDHD;
