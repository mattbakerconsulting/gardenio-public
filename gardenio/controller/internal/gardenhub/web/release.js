/* web/release.js: the Workbench's release kind, /garden?release=<id> (gd-rSz81Me1Pe), opened from the release truck. From
   the viewer's /api/releases/<id>: its changelog by plan, then step, as the Release page groups it. Text via textContent. */
var RELEASE = (function () {
  'use strict';
  function url(id) { return '/api/releases/' + encodeURIComponent(id); }
  function arr(v) { return Array.isArray(v) ? v : []; }
  function plural(n, one, many) { return n + ' ' + (n === 1 ? one : many); }
  /* group(rows) -> [{id, title, status, n, steps: [{id, title, rows}]}]: finished plans first, then most PRs, unplanned last. */
  function group(rows) {
    var by = {}, out = [];
    arr(rows).forEach(function (r) {
      var pk = r.plan || '', p = by[pk] || (by[pk] = out[out.push({ id: pk, title: '', status: '', n: 0, steps: [], at: {} }) - 1]), sk = r.step || r.task || '';
      var s = p.at[sk] || (p.at[sk] = p.steps[p.steps.push({ id: sk, title: '', rows: [] }) - 1]);
      p.title = p.title || r.plan_title || ''; p.status = p.status || r.plan_status || ''; p.n++;
      s.title = s.title || r.step_title || ''; s.rows.push(r);
    });
    return out.sort(function (a, b) {
      return (!a.id - !b.id) || ((b.status === 'closed') - (a.status === 'closed')) || (b.n - a.n) || (!a.title - !b.title) || String(a.title || a.id).localeCompare(b.title || b.id);
    });
  }
  /* summary(d, count) -> "86 PRs from 5 plans"; count, the truck's, also counts the PRs with no number (nit b). */
  function summary(d, count) {
    var rows = arr(d && d.contents), np = group(rows).filter(function (g) { return g.id; }).length, gap = Math.max(0, (count || 0) - rows.length);
    return plural(rows.length + gap, 'PR', 'PRs') + (np ? ' from ' + plural(np, 'plan', 'plans') : '') + (gap ? ' (' + gap + ' with no PR number, not listed)' : '');
  }
  function stamp(s) { var d = new Date(s || ''); return isNaN(d.getTime()) ? '' : d.toISOString().slice(0, 16).replace('T', ' ') + 'Z'; }
  var STATUS = { cut: 'Cut, not built or deployed yet', built: 'Built, not deployed yet', live: 'Live', superseded: 'Superseded' };
  function facts(d) {
    var on = arr(d.deployments).filter(function (x) { return x && x.target && !x.until; });
    return [STATUS[d.status] || STATUS.cut, d.cut_at ? 'cut ' + stamp(d.cut_at) : '',
      on.length ? 'runs on ' + on.map(function (x) { return x.target + (stamp(x.since) ? ' since ' + stamp(x.since) : ''); }).join(', ') : 'No deployments yet'].filter(Boolean);
  }
  var api = { url: url, group: group, summary: summary, facts: facts, stamp: stamp };
  if (typeof document === 'undefined' || typeof WORKBENCH === 'undefined') return api;

  /* ---------- DOM ---------- */
  var W = WORKBENCH, base = (document.body && document.body.dataset.base) || '';
  var titleEl = document.getElementById('bv-title'), kindEl = document.getElementById('bv-kind'), bodyEl = document.getElementById('bv-body');
  if (!bodyEl) return api;
  function el(t, cls, text) { var e = document.createElement(t); if (cls) e.className = cls; if (text !== undefined) e.textContent = text; return e; }
  function link(href, text, cls) { var a = el('a', cls || 'bl', text); a.href = base + href; return a; }
  /* A plan or step link by its title; with none known, "Untitled <noun>" and the id in its tooltip, never a bare id. */
  function beadLink(id, title, noun) {
    var a = typeof TITLES !== 'undefined' ? TITLES.link(id, { cls: 'bl', text: title && title !== id ? title : '' }) : link('/garden?bead=' + encodeURIComponent(id), title || id);
    if (a.textContent === id) { a.textContent = 'Untitled ' + noun; a.title = id; }
    return a;
  }
  function outLink(u, text) {
    var to = typeof BEAD !== 'undefined' ? BEAD.safeHref(u || '') : '';
    if (!to || !/^https?:/i.test(to)) return el('span', '', text);
    var a = el('a', 'bl bv-url', text); a.href = to; a.title = u; a.target = '_blank'; a.rel = 'noopener noreferrer';
    return a;
  }
  function relLink(id, tag, word) { return link('/garden?release=' + encodeURIComponent(id), word + ' ' + (tag || id)); }
  function known(id) { return typeof TRUCK !== 'undefined' && TRUCK.release ? TRUCK.release(id) : null; }

  function paint(id, d, note, isErr, retry) {
    var k = known(id);
    kindEl.textContent = 'release';
    titleEl.textContent = 'Release ' + ((d && d.tag) || (k && k.tag) || id);
    bodyEl.textContent = '';
    if (note) {
      var n = bodyEl.appendChild(el('p', 'bv-note' + (isErr ? ' err' : ''), note));
      n.setAttribute('role', 'status');
      if (isErr && retry) { var b = n.appendChild(el('button', 'btn small bv-retry', 'Try again')); b.type = 'button'; b.onclick = function () { b.disabled = true; W.reload('release', id); }; }
    }
    if (!d) return;
    var meta = bodyEl.appendChild(el('div', 'bv-meta'));
    facts(d).forEach(function (w) { meta.appendChild(el('span', '', w)); });
    if (d.previous) meta.appendChild(relLink(d.previous, d.previous_tag, 'previous'));
    if (d.next) meta.appendChild(relLink(d.next, d.next_tag, 'next'));
    meta.appendChild(link('/garden?bead=' + encodeURIComponent(d.id), 'the release bead'));
    var rows = arr(d.contents), h = bodyEl.appendChild(el('h3', 'bd-h rl-h', 'Changelog'));
    h.appendChild(el('span', 'bd-sub', ' ' + summary(d, k && k.id === d.id ? k.count : 0)));
    if (!rows.length) { bodyEl.appendChild(el('p', 'bv-empty', 'No pull requests are recorded for this release.')); return; }
    group(rows).forEach(function (g) {
      var sec = bodyEl.appendChild(el('section', 'rl-plan')), ph = sec.appendChild(el('h4', 'rl-ph'));
      if (g.id) ph.appendChild(beadLink(g.id, g.title, 'plan')); else ph.textContent = 'Not in a plan';
      ph.appendChild(el('span', 'bd-sub', ' ' + plural(g.n, 'PR', 'PRs') + (g.status === 'closed' ? ' · finished the plan' : '')));
      g.steps.forEach(function (s) {
        if (s.id && g.id) { var sh = sec.appendChild(el('p', 'rl-step')); sh.appendChild(beadLink(s.id, s.title, 'step')); }
        var ul = sec.appendChild(el('ul', 'rl-prs'));
        s.rows.forEach(function (r) {
          var li = ul.appendChild(el('li'));
          li.appendChild(outLink(r.url, '#' + r.pr));
          li.appendChild(document.createTextNode(' ' + (r.title || '')));
        });
      });
    });
  }

  W.register('release', { noun: 'release', url: url, paint: paint });
  return api;
})();
if (typeof module !== 'undefined') module.exports = RELEASE;
