/* web/credits.js: a tenant land's credit charts in the Costs panel (plan gd-gh5J0vVbJP step 3.3; garden-grafana-panels.md,
   "The tenant panel set (3.3)"), from GET /garden/charts/costs-land: spent tiles, remaining of granted, burn to the wall,
   credits per agent. Inline SVG by createElementNS, text by textContent, the --box tokens; credits only. */
var CREDITS = (function () {
  var NS = 'http://www.w3.org/2000/svg', W = 320, H = 150, PAD = 6;
  /* Credits in words: two decimals under 10, one under 1000, else whole; a figure not known is a dash, never 0. */
  function num(v) { return typeof v === 'number' && isFinite(v) ? v.toFixed(v < 10 ? 2 : v < 1000 ? 1 : 0) : '—'; }
  function when(t) { return new Date(t * 1000).toISOString().slice(0, 16).replace('T', ' ') + ' UTC'; }
  function span(s) {
    var h = s / 3600;
    return h < 1 ? 'within the hour' : h < 48 ? 'in about ' + Math.round(h) + ' hours' : 'in about ' + Math.round(h / 24) + ' days';
  }
  /* The three tiles, [label, value] pairs. */
  function tiles(d) {
    var s = (d && d.spent) || {};
    return [['Today', num(s.today)], ['7 days', num(s.week)], ['30 days', num(s.month)]];
  }
  /* The meter: the remaining's and the wall's share of granted (0-100), and its words. */
  function meter(p) {
    p = p || {};
    if (p.state === 'unknown') return { pct: null, wall: null, words: 'The purse can’t be read right now, so new agents wait.' };
    var g = p.granted_credits, r = p.remaining_credits, ok = g > 0 && typeof r === 'number';
    function pc(v) { return ok ? Math.max(0, Math.min(100, v / g * 100)) : null; }
    var words = ok ? num(r) + ' of ' + num(g) + ' credits left; the wall is at ' + num(p.margin_credits) + '.' : 'No credits granted yet.';
    if (p.state === 'wall') words += ' At the wall: new agents wait until more credits are granted.';
    return { pct: pc(r), wall: pc(p.margin_credits), words: words };
  }
  /* The burn line in the W × H box: the points, the dashed run-out, the wall's y, its words. x runs a quarter past now. */
  function burn(d) {
    var b = (d && d.burn) || {}, pts = b.points || [], p = (d && d.purse) || {};
    var x0 = b.start, x1 = b.end + (b.end - b.start) / 4, top = Math.max(p.granted_credits || 0, b.wall || 0);
    pts.forEach(function (q) { top = Math.max(top, q[1]); });
    top = top > 0 ? top * 1.05 : 1;
    function X(t) { return PAD + (W - 2 * PAD) * (t - x0) / ((x1 - x0) || 1); }
    function Y(v) { return PAD + (H - 2 * PAD) * (1 - Math.max(0, v) / top); }
    var line = pts.map(function (q) { return [X(q[0]), Y(q[1])]; }), proj = null, from = typeof p.remaining_credits === 'number' ? p.remaining_credits : pts.length ? pts[pts.length - 1][1] : null;
    var rate = b.rate_per_hour, words;
    if (from !== null && rate > 0) {
      var tEnd = b.run_out && b.run_out < x1 ? b.run_out : x1, vEnd = Math.max(b.wall, from - rate * (tEnd - b.end) / 3600);
      proj = [[X(b.end), Y(from)], [X(tEnd), Y(vEnd)]];
    }
    if (!(rate > 0)) words = 'No credits are burning now, so there is no run-out to show.';
    else if (!b.run_out) words = 'At ' + num(rate) + ' credits an hour, no run-out is in sight.';
    else if (b.run_out <= b.end) words = 'At ' + num(rate) + ' credits an hour, the land is at its wall.';
    else words = 'At ' + num(rate) + ' credits an hour, the land reaches its wall ' + span(b.run_out - b.end) + ' (' + when(b.run_out) + ').';
    return { line: line, proj: proj, wallY: Y(b.wall || 0), nowX: X(b.end), top: top / 1.05, words: words };
  }
  /* The per-agent rows, [name, width % of the first, value], and the rest's line ('' with none). */
  function agents(d) {
    var a = (d && d.agents) || [], top = a.length ? a[0].credits : 0;
    var rows = a.map(function (x) { return [x.agent, top > 0 ? Math.max(1, x.credits / top * 100) : 0, num(x.credits)]; });
    var r = d && d.rest;
    return { rows: rows, rest: r && r.count ? 'and ' + r.count + ' more agent' + (r.count === 1 ? '' : 's') + ': ' + num(r.credits) : '' };
  }
  /* The history's own words when it does not draw ('' when it does). */
  function historyWord(d) {
    var h = d && d.history;
    return h === 'none' ? 'No credit history yet.' : h === 'unavailable' ? 'Credit history is unavailable right now.' : '';
  }

  /* paint(host, d): the cards into host, emptied first. */
  function paint(host, d) {
    var doc = host.ownerDocument || document;
    function el(t, cls, text) { var e = doc.createElement(t); if (cls) e.className = cls; if (text !== undefined) e.textContent = text; return e; }
    function sv(t, a) { var e = doc.createElementNS(NS, t); Object.keys(a).forEach(function (k) { e.setAttribute(k, a[k]); }); return e; }
    function card(id, title) {
      var s = el('section', 'gp-card cr-card'), h = el('h3', 'gp-t', title);
      h.id = 'cr-' + id; s.setAttribute('aria-labelledby', h.id); s.appendChild(h); host.appendChild(s);
      return s;
    }
    host.textContent = '';
    var hw = historyWord(d), c;
    if (!hw) {
      c = card('spent', 'Credits spent');
      var dl = el('dl', 'cr-tiles');
      tiles(d).forEach(function (t) { var g = el('div', 'cr-tile'); g.appendChild(el('dt', '', t[0])); g.appendChild(el('dd', '', t[1])); dl.appendChild(g); });
      c.appendChild(dl);
    }
    var m = meter(d && d.purse);
    c = card('left', 'Remaining of granted');
    if (m.pct !== null) {
      var bar = sv('svg', { viewBox: '0 0 100 8', preserveAspectRatio: 'none', 'class': 'cr-meter', role: 'img', 'aria-label': m.words });
      bar.appendChild(sv('rect', { x: 0, y: 0, width: 100, height: 8, 'class': 'cr-track' }));
      bar.appendChild(sv('rect', { x: 0, y: 0, width: m.pct, height: 8, 'class': 'cr-fill' + (d.purse.state === 'wall' ? ' at-wall' : '') }));
      bar.appendChild(sv('rect', { x: Math.max(0, m.wall - 0.4), y: 0, width: 0.8, height: 8, 'class': 'cr-wall' }));
      c.appendChild(bar);
    }
    c.appendChild(el('p', 'gp-note', m.words));
    if (hw) { host.appendChild(el('p', 'bv-note cr-none', hw)); return; }
    var b = burn(d);
    c = card('burn', 'Burn to the wall');
    var svg = sv('svg', { viewBox: '0 0 ' + W + ' ' + H, 'class': 'cr-chart', role: 'img', tabindex: 0, 'aria-label': 'Credits left over the range. ' + b.words });
    svg.appendChild(sv('line', { x1: PAD, x2: W - PAD, y1: b.wallY, y2: b.wallY, 'class': 'cr-wall-rule' }));
    svg.appendChild(sv('line', { x1: b.nowX, x2: b.nowX, y1: PAD, y2: H - PAD, 'class': 'cr-now' }));
    [[PAD + 2, PAD + 9, 'start', num(b.top)], [b.nowX - 3, PAD + 9, 'end', 'now'], [W - PAD - 2, b.wallY - 3, 'end', 'wall']].forEach(function (t) {
      var x = sv('text', { x: t[0], y: t[1], 'text-anchor': t[2], 'class': 'cr-lbl' }); x.textContent = t[3]; svg.appendChild(x);
    });
    if (b.line.length) svg.appendChild(sv('polyline', { points: b.line.map(function (q) { return q[0].toFixed(1) + ',' + q[1].toFixed(1); }).join(' '), 'class': 'cr-line' }));
    if (b.proj) svg.appendChild(sv('line', { x1: b.proj[0][0], y1: b.proj[0][1], x2: b.proj[1][0], y2: b.proj[1][1], 'class': 'cr-proj' }));
    var dot = sv('circle', { r: 3, cx: -9, cy: -9, 'class': 'cr-dot' });
    svg.appendChild(dot);
    c.appendChild(svg);
    var read = el('p', 'gp-note cr-read', ''); read.setAttribute('role', 'status'); c.appendChild(read);
    c.appendChild(el('p', 'gp-note', b.words));
    /* hover or tap names the nearest point; the arrows step */
    var pts = (d.burn && d.burn.points) || [], at = -1;
    function show(i) {
      if (!pts.length) return;
      at = Math.max(0, Math.min(pts.length - 1, i));
      dot.setAttribute('cx', b.line[at][0]); dot.setAttribute('cy', b.line[at][1]);
      read.textContent = when(pts[at][0]) + ': ' + num(pts[at][1]) + ' credits left';
    }
    function near(e) {
      var r = svg.getBoundingClientRect(), x = (e.clientX - r.left) / (r.width || 1) * W, best = 0;
      b.line.forEach(function (q, i) { if (Math.abs(q[0] - x) < Math.abs(b.line[best][0] - x)) best = i; });
      show(best);
    }
    svg.addEventListener('pointermove', near); svg.addEventListener('pointerdown', near);
    svg.addEventListener('keydown', function (e) {
      var s = e.key === 'ArrowRight' ? 1 : e.key === 'ArrowLeft' ? -1 : 0;
      if (s) { e.preventDefault(); show(at < 0 ? pts.length - 1 : at + s); }
    });
    var a = agents(d);
    c = card('agents', 'Credits per agent');
    c.appendChild(el('p', 'gp-scope', 'The last two days or so'));
    if (!a.rows.length) c.appendChild(el('p', 'gp-note', 'No agent has spent credits lately.'));
    else {
      var ol = el('ol', 'cr-agents');
      a.rows.forEach(function (r) {
        var li = el('li'), track = el('span', 'cr-bar'), fill = el('span', 'cr-bar-fill');
        fill.style.width = r[1].toFixed(1) + '%'; track.appendChild(fill);
        var nm = el('span', 'cr-name', r[0]); nm.title = r[0];
        li.appendChild(nm); li.appendChild(track); li.appendChild(el('span', 'cr-val', r[2]));
        ol.appendChild(li);
      });
      c.appendChild(ol);
      if (a.rest) c.appendChild(el('p', 'gp-note', a.rest));
    }
  }

  return { num: num, when: when, span: span, tiles: tiles, meter: meter, burn: burn, agents: agents, historyWord: historyWord, paint: paint };
})();
if (typeof module !== 'undefined') module.exports = CREDITS;
