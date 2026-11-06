/* web/wall.js: the Glass's Wall (Lands workbench-glass spec GL-5, 3.3; step 6.7, gd-Pjha29mbgN), /garden?glass=wall (or
   ?glass= with no agent, workbench.js EMPTY): every live session in the viewer's lands as text-grid tiles, attention order,
   look only. GET /garden/glass/wall (glass_wall.go) names the agents; its SSE feed (internal/glass Wall, one /ws/mux upstream
   filtered per viewer) moves them: agents (who), tiles (a snapshot), screens (coopmux's ~1 s batches), state (GL-4),
   online/offline, ended (coopmux unreachable, said in words). Updates apply once a frame. A tile is a <pre> of spans in
   coopmux's SGR colours (coop's ansi.ts), textContent only, never a canvas (PD 6 #4), ordered prompt, error, rate limited,
   working, idle, rest (SessionSidebar.tsx); a click watches it (?glass=<agent>) and Back returns here. Tiles fill the pane
   (gd-2f1ImvHLDU): CSS columns at least 420 px, one height per row (MIN_ROWS to TILE_ROWS), scaled down no further than
   MIN_SCALE (10 px glyphs), then cropped right, the last lines that fit.
   A gardener figure (a.hot.w-gardener) shows the tile at small size on hover, focus or a finger's tap (GARDEN.cam's
   hots): its chip, its last six lines from /garden/glass/wall?agent=<id>, and Watch (spec 3.3, GL-12), clear of the
   figure and the signs. The Menu's Glass item opens the Wall. Only for a viewer who may watch (body data-glass); the routes
   check again. Nothing here writes to a session. */
var WALL = (function () {
  /* ---------- pure logic (also run under node by the Go tests) ---------- */
  var ID = 'wall', THUMB_ROWS = 6, TILE_ROWS = 24, MIN_ROWS = 8, MIN_SCALE = 10 / 12, FIT_PAD = 8, CARD_COLS = 64;   // .wl-pre is 12 px, .wl-fit's padding 4 px
  function url(one) { return '/garden/glass/wall' + (one ? '?agent=' + encodeURIComponent(one) : ''); }
  /* web/world.js's, which every page loads */
  var stateView = (typeof WORLD !== 'undefined' ? WORLD : require('./world.js')).stateView;
  /* The PTY's geometry from a screen ({cols, rows}), 80 x 24 when there is none or it is not one. */
  function geometry(s) {
    var c = s && Number(s.cols), r = s && Number(s.rows);
    return c >= 2 && c <= 1000 && r >= 1 && r <= 500 ? { cols: c, rows: r } : { cols: 80, rows: 24 };
  }
  /* How far a terminal natW px wide is scaled to fit a box boxW px wide: down only, never up (GL-2). */
  function fitScale(boxW, natW) { return boxW > 0 && natW > boxW ? boxW / natW : 1; }
  /* The attention order (GL-5) of a state chip's key (stateView): the states that want a human first. */
  var RANK = { prompt: 0, error: 1, rate_limited: 2, working: 3, idle: 4, starting: 5, restarting: 5, parked: 5 };
  function rank(key) { return Object.prototype.hasOwnProperty.call(RANK, key) ? RANK[key] : 6; }
  /* tiles ({id, name, key}) in attention order, then by name, then by id: a stable order for tiles in the same state. */
  function order(tiles) {
    return tiles.slice().sort(function (a, b) {
      return rank(a.key) - rank(b.key) || String(a.name || '').localeCompare(String(b.name || '')) || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0);
    });
  }
  /* What the Wall's status line says of tiles ({key}): how many are live and how many want a human. */
  function summary(tiles, notLive) {
    var n = tiles.length, ask = tiles.filter(function (t) { return t.key === 'prompt'; }).length, bad = tiles.filter(function (t) { return t.key === 'error' || t.key === 'rate_limited'; }).length;
    var s = n ? n + (n === 1 ? ' live session' : ' live sessions') : 'No live sessions in your lands right now';
    var parts = [];
    if (ask) parts.push(ask + ' at a prompt');
    if (bad) parts.push(bad + ' in error or rate limited');
    if (parts.length) s += ': ' + parts.join(', ');
    if (notLive) s += (n ? '; ' : '. ') + notLive + (notLive === 1 ? ' more agent has no live session.' : ' more agents have no live session.');
    else s += '.';
    return s;
  }

  /* coop's ANSI colours (web/src/lib/ansi.ts): the 16 named, the 6x6x6 cube and the greys; red, blue, magenta and bright
     black lifted to 4.5:1 on --wl-bg (gd-pHyKC0Uef1). The Glass's xterm takes the 16 as its theme. */
  var COLORS = ['#000000', '#e05252', '#0dbc79', '#e5e510', '#3b82d6', '#c84cc8', '#11a8cd', '#e5e5e5',
    '#808080', '#f14c4c', '#23d18b', '#f5f543', '#3b8eea', '#d670d6', '#29b8db', '#ffffff'];
  function color256(n) {
    n = Number(n) || 0;
    if (n < 16) return COLORS[n];
    if (n < 232) { var i = n - 16; return 'rgb(' + Math.floor(i / 36) * 51 + ',' + (Math.floor(i / 6) % 6) * 51 + ',' + (i % 6) * 51 + ')'; }
    var v = (n - 232) * 10 + 8;
    return 'rgb(' + v + ',' + v + ',' + v + ')';
  }
  function fresh() { return { bold: false, faint: false, italic: false, under: false, strike: false, inv: false, fg: null, bg: null }; }
  function colorAt(p, i) {   // 38;5;n or 38;2;r;g;b at p[i]
    if (p[i + 1] === 5 && i + 2 < p.length) return { c: { n: p[i + 2] }, used: 3 };
    if (p[i + 1] === 2 && i + 4 < p.length) return { c: { rgb: [p[i + 2], p[i + 3], p[i + 4]] }, used: 5 };
    return { c: null, used: 1 };
  }
  function sgr(st, p) {
    if (!p.length) { st = fresh(); return st; }
    for (var i = 0; i < p.length; i++) {
      var x = p[i], r;
      if (x === 0) st = fresh();
      else if (x === 1) st.bold = true; else if (x === 2) st.faint = true; else if (x === 3) st.italic = true;
      else if (x === 4) st.under = true; else if (x === 7) st.inv = true; else if (x === 9) st.strike = true;
      else if (x === 22) { st.bold = false; st.faint = false; } else if (x === 23) st.italic = false; else if (x === 24) st.under = false;
      else if (x === 27) st.inv = false; else if (x === 29) st.strike = false;
      else if (x >= 30 && x <= 37) st.fg = { n: x - 30 }; else if (x === 39) st.fg = null;
      else if (x >= 40 && x <= 47) st.bg = { n: x - 40 }; else if (x === 49) st.bg = null;
      else if (x >= 90 && x <= 97) st.fg = { n: x - 90 + 8 }; else if (x >= 100 && x <= 107) st.bg = { n: x - 100 + 8 };
      else if (x === 38 || x === 48) { r = colorAt(p, i); if (x === 38) st.fg = r.c; else st.bg = r.c; i += r.used - 1; }
    }
    return st;
  }
  function cssColor(c, bold) {
    if (c.rgb) return 'rgb(' + c.rgb.join(',') + ')';
    return bold && c.n < 8 ? COLORS[c.n + 8] : color256(c.n);
  }
  /* A span's style as element.style properties, or null for the terminal's own colours: bold brightens a named colour, inverse
     swaps the two (the terminal's own when a side has none). */
  function style(st) {
    var o = {}, any = false, fg = st.fg, bg = st.bg;
    if (st.inv) { var t = fg; fg = bg; bg = t; }
    if (fg) { o.color = cssColor(fg, st.bold); any = true; } else if (st.inv) { o.color = 'var(--wl-bg)'; any = true; }
    if (bg) { o.backgroundColor = cssColor(bg, false); any = true; } else if (st.inv) { o.backgroundColor = 'var(--wl-fg)'; any = true; }
    if (st.bold) { o.fontWeight = '700'; any = true; }
    if (st.faint) { o.opacity = '0.55'; any = true; }
    if (st.italic) { o.fontStyle = 'italic'; any = true; }
    var deco = (st.under ? ['underline'] : []).concat(st.strike ? ['line-through'] : []);
    if (deco.length) { o.textDecoration = deco.join(' '); any = true; }
    return any ? o : null;
  }
  var CSI = /\x1b\[([0-9;]*)([A-Za-z])/g, OSC = /\x1b\].*?(?:\x1b\\|\x07)/g, ESC = /\x1b[ -\/]*[0-~]/g;   // ESC, its intermediates and its final (ESC ( B, ESC 7, ...)
  /* One screen line as [text, style] spans: SGR colours applied, every other escape dropped. */
  function spans(line) {
    var s = String(line == null ? '' : line).replace(OSC, ''), out = [], st = fresh(), last = 0, m;
    CSI.lastIndex = 0;
    while ((m = CSI.exec(s)) !== null) {
      if (m.index > last) out.push([s.slice(last, m.index).replace(ESC, ''), style(st)]);
      last = CSI.lastIndex;
      if (m[2] === 'm') st = sgr(st, m[1] ? m[1].split(';').map(Number) : []);
    }
    if (last < s.length) out.push([s.slice(last).replace(ESC, ''), style(st)]);
    return out.filter(function (p) { return p[0]; });
  }
  /* A line's text alone. */
  function plain(line) { return spans(line).map(function (p) { return p[0]; }).join(''); }
  /* The last n lines that say something: trailing blank lines (an idle prompt's empty screen below it) are not a thumbnail.
     dense drops the blank lines between them too: a figure's card has six rows, and each of them says something. */
  function tail(lines, n, dense) {
    lines = lines || [];
    if (dense) lines = lines.filter(function (l) { return plain(l).trim(); });
    var end = lines.length;
    while (end > 0 && !plain(lines[end - 1]).trim()) end--;
    return lines.slice(Math.max(0, end - n), end);
  }
  var api = { ID: ID, THUMB_ROWS: THUMB_ROWS, MIN_SCALE: MIN_SCALE, url: url, rank: rank, order: order, summary: summary, COLORS: COLORS, color256: color256, spans: spans, plain: plain, tail: tail, geometry: geometry, fitScale: fitScale };
  if (typeof document === 'undefined' || typeof WORKBENCH === 'undefined') return api;

  /* ---------- DOM ---------- */
  var W = WORKBENCH, B = document.body, base = B.dataset.base || '';
  var titleEl = document.getElementById('bv-title'), kindEl = document.getElementById('bv-kind'), bodyEl = document.getElementById('bv-body');
  if (!bodyEl || B.dataset.glass !== '1') return api;
  function el(t, cls, text) { var e = document.createElement(t); if (cls) e.className = cls; if (text !== undefined) e.textContent = text; return e; }
  function parse(e) { try { return JSON.parse(e.data); } catch (x) { return null; } }
  var raf = window.requestAnimationFrame ? function (f) { return window.requestAnimationFrame(f); } : function (f) { return setTimeout(f, 16); };

  /* A screen ({lines, cols}) into pre, at most rows lines of it (dense: tail's), its width the PTY's own. */
  function paintScreen(pre, screen, rows, dense) {
    var lines = tail((screen && screen.lines) || [], rows, dense);
    pre.textContent = '';
    lines.forEach(function (ln) {
      var row = pre.appendChild(el('span', 'wl-ln'));
      spans(ln).forEach(function (p) {
        var s = row.appendChild(el('span', '', p[0]));
        if (p[1]) for (var k in p[1]) s.style[k] = p[1][k];
      });
      row.appendChild(document.createTextNode('\n'));
    });
    var g = geometry(screen);
    pre.style.width = g.cols + 'ch';
  }
  function chipInto(c, state) {
    var v = stateView(state);
    c.textContent = '';
    c.appendChild(el('span', 'gl-dot')).setAttribute('aria-hidden', 'true');
    c.appendChild(el('span', '', v.word));
    c.className = 'gl-state ' + v.tone + (v.pulse ? ' pulse' : '');
    c.dataset.state = v.key;
    return v;
  }
  /* Scale a figure's card's screen down to its box's width (GL-2: down only, never rewrapped). With cols (the PTY's) and fewer
     wanted, only the first want columns are fitted and the rest cropped: a figure's card is small, and lines start at the left. */
  function fitTile(box, fitEl, cols, want) {
    var nat = fitEl.offsetWidth, s = fitScale(box.clientWidth, cols > want ? nat * want / cols : nat);
    fitEl.style.transform = s < 1 ? 'scale(' + s + ')' : '';
    box.style.height = Math.ceil(fitEl.offsetHeight * s) + 'px';
  }

  /* ---------- the Wall panel ---------- */
  var S = null;   // the Wall shown: {doc, agents: id -> agent, tiles: id -> {live, state, screen, el, ...}, es, dirty, order, frame}
  function say(text, err) { if (S) { S.noteEl.textContent = text; S.noteEl.className = 'bv-note wl-note' + (err ? ' err' : ''); } }
  function tileEl(a) {
    var li = el('li', 'wl-cell'), link = li.appendChild(el('a', 'wl-tile'));
    link.href = base + '/garden' + W.withPanel('', 'glass', a.id);
    var head = link.appendChild(el('span', 'wl-head'));
    var name = head.appendChild(el('span', 'wl-name', a.agent || a.id));
    var chip = head.appendChild(el('span', 'gl-state'));
    var sub = link.appendChild(el('span', 'wl-sub'));
    var box = link.appendChild(el('span', 'wl-scr')), fitEl = box.appendChild(el('span', 'wl-fit')), pre = fitEl.appendChild(el('pre', 'wl-pre'));
    pre.setAttribute('aria-hidden', 'true');   // the link says who and how it is; the screen is for the eye
    return { li: li, link: link, name: name, chip: chip, sub: sub, box: box, fit: fitEl, pre: pre };
  }
  function subOf(a) { return a.task_title ? 'Tending: ' + a.task_title : [a.role, a.project ? 'on ' + a.project : ''].filter(Boolean).join(' '); }
  function tileOf(id) {
    var t = S.tiles[id];
    if (!t) t = S.tiles[id] = { live: false, state: null, screen: null, shown: null };
    return t;
  }
  function touch(id) { if (S) { S.dirty[id] = 1; schedule(); } }
  function schedule() { if (S && !S.frame) { var my = S; S.frame = raf(function () { if (S === my) { S.frame = 0; flush(); } }); } }
  /* Apply what moved since the last frame: each dirty tile's chip, words and screen, then the order. */
  function flush() {
    if (!S) return;
    var ids = Object.keys(S.dirty), list = [];
    S.dirty = {};
    ids.forEach(function (id) {
      var a = S.agents[id], t = S.tiles[id];
      if (!a || !t || !t.live) { if (t && t.dom) { t.dom.li.remove(); t.dom = null; } return; }
      if (!t.dom) t.dom = tileEl(a);
      var v = chipInto(t.dom.chip, t.state || a.state);
      t.key = v.key;
      t.dom.name.textContent = a.agent || a.id;
      t.dom.sub.textContent = subOf(a);
      t.dom.link.setAttribute('aria-label', 'Watch ' + (a.agent || a.id) + ', ' + v.word + (subOf(a) ? ', ' + subOf(a) : ''));
      t.dom.li.classList.toggle('want', v.pulse || v.key === 'rate_limited');
    });
    Object.keys(S.agents).forEach(function (id) {
      var t = S.tiles[id];
      if (t && t.live && t.dom) list.push({ id: id, name: S.agents[id].agent, key: t.key, t: t });
    });
    var sorted = order(list), key = sorted.map(function (x) { return x.id; }).join(' ');
    if (key !== S.order) {
      var a = document.activeElement, had = a && S.grid.contains(a);
      sorted.forEach(function (x) { S.grid.appendChild(x.t.dom.li); });
      if (had && document.activeElement !== a && a.focus) { try { a.focus({ preventScroll: true }); } catch (e) { a.focus(); } }
      S.order = key;
    }
    layout(sorted);
    S.empty.hidden = sorted.length > 0;
    S.sumEl.textContent = summary(sorted, Object.keys(S.agents).length - sorted.length);
  }
  /* Every tile's screen one height, the pane's below the grid shared by its rows of tiles (a screen of MIN_ROWS to TILE_ROWS
     lines), and each screen scaled to its tile's width but never below MIN_SCALE, its right side cropped past that; it
     shows the last lines that fit, at the foot of its box. Run each frame something moved, and on a resize. */
  function layout(sorted) {
    if (!sorted.length) return;
    var g = S.grid, gs = getComputedStyle(g), d = sorted[0].t.dom, ls = getComputedStyle(d.link);
    var n = Math.ceil(sorted.length / Math.max(1, gs.gridTemplateColumns.split(' ').length)), line = parseFloat(getComputedStyle(d.pre).lineHeight) || 15;
    var top = g.getBoundingClientRect().top - bodyEl.getBoundingClientRect().top - bodyEl.clientTop + bodyEl.scrollTop;
    var chrome = d.box.getBoundingClientRect().top - d.link.getBoundingClientRect().top + parseFloat(ls.paddingBottom) + parseFloat(ls.borderBottomWidth);
    var each = (bodyEl.clientHeight - top - parseFloat(getComputedStyle(bodyEl).paddingBottom) - (n - 1) * (parseFloat(gs.rowGap) || 0)) / n - chrome;
    var edge = d.box.offsetHeight - d.box.clientHeight, h = Math.floor(Math.max(MIN_ROWS * line * MIN_SCALE, Math.min(TILE_ROWS * line, each - edge - FIT_PAD)) + FIT_PAD + edge);
    sorted.forEach(function (x) {
      var t = x.t, dom = t.dom;
      if (t.screen !== t.shown) { paintScreen(dom.pre, t.screen, t.rows || TILE_ROWS); t.shown = t.screen; t.painted = t.rows || TILE_ROWS; }
      var s = Math.max(MIN_SCALE, fitScale(dom.box.clientWidth, dom.fit.offsetWidth));
      t.rows = Math.max(1, Math.min(TILE_ROWS, Math.floor((h - edge - FIT_PAD * s) / (line * s))));
      if (t.painted !== t.rows) { paintScreen(dom.pre, t.screen, t.rows); t.painted = t.rows; }
      dom.fit.style.transform = s < 1 ? 'scale(' + s + ')' : '';
      dom.box.style.height = h + 'px';
    });
  }
  function setAgents(list) {
    var next = {};
    (list || []).forEach(function (a) { if (a && a.id) next[a.id] = a; });
    Object.keys(S.agents).forEach(function (id) { if (!next[id]) { touch(id); } });
    S.agents = next;
    Object.keys(next).forEach(touch);
  }
  function setTile(m, snap) {
    if (!m || !m.agent) return;
    var t = tileOf(m.agent);
    t.live = true;
    if (m.state) t.state = m.state;
    if (m.screen) t.screen = m.screen;
    if (snap) snap[m.agent] = 1;
    touch(m.agent);
  }

  function stream() {
    if (!S || S.es || S.ended || !S.doc.live || !S.doc.stream || typeof EventSource === 'undefined') return;
    var my = S, es = LIVE.source(S.doc.stream);   // below the viewer root already (glass_wall.go)
    S.es = es;
    function mine() { return S === my && my.es === es; }
    es.addEventListener('agents', function (e) { var m = parse(e); if (mine() && m) setAgents(m.agents); });
    es.addEventListener('tiles', function (e) {
      var m = parse(e); if (!mine() || !m) return;
      var seen = {};
      (m.tiles || []).forEach(function (x) { setTile(x, seen); });
      Object.keys(my.tiles).forEach(function (id) { if (!seen[id] && my.tiles[id].live) { my.tiles[id].live = false; touch(id); } });
      say('Live. Look only: nothing here reaches a terminal. Pick a tile to watch it.');
    });
    es.addEventListener('screens', function (e) { var m = parse(e); if (mine() && m) (m.screens || []).forEach(function (x) { setTile(x); }); });
    es.addEventListener('state', function (e) { var m = parse(e); if (mine() && m && m.agent) { tileOf(m.agent).state = m.state; touch(m.agent); } });
    es.addEventListener('online', function (e) { var m = parse(e); if (mine() && m && m.agent) { tileOf(m.agent).live = true; touch(m.agent); } });
    es.addEventListener('offline', function (e) { var m = parse(e); if (mine() && m && m.agent) { tileOf(m.agent).live = false; touch(m.agent); } });
    es.addEventListener('ended', function (e) {
      var m = parse(e); if (!mine()) return;
      es.close(); my.es = null; my.ended = true;
      say(m && m.reason === 'refused' ? 'Coopmux refused the Wall, so its tiles have stopped. These are the last screens the garden had.' :
        'The live Wall is not available here: the garden cannot reach coopmux for it. These are the last screens it had.', true);
    });
    es.onopen = function () { if (mine()) say('Connected. Waiting for the sessions…'); };
    es.onerror = function () {
      if (!mine()) return;
      if (es.readyState === 2) { my.es = null; my.ended = true; say('The live Wall is unavailable right now: the garden did not open its feed.', true); }
      else say('Reconnecting to the live Wall…');
    };
  }
  function unstream() { if (S && S.es) { S.es.close(); S.es = null; } }
  function teardown() { unstream(); S = null; }
  document.addEventListener('visibilitychange', function () {
    if (!S) return;
    if (document.hidden) unstream();   // coming back opens a fresh snapshot
    else stream();
  });
  window.addEventListener('resize', function () {
    if (!S) return;
    schedule();   // flush lays the tiles out again
  });

  function paint(id, d, note, isErr, retry) {
    teardown();
    kindEl.textContent = 'glass';
    titleEl.textContent = 'The Wall';
    bodyEl.textContent = '';
    if (!d) {
      var n = bodyEl.appendChild(el('p', 'bv-note' + (isErr ? ' err' : ''), note || ''));
      n.setAttribute('role', 'status');
      if (retry) { var b = n.appendChild(el('button', 'btn small bv-retry', 'Try again')); b.type = 'button'; b.onclick = function () { W.reload('glass', ID); }; }
      return;
    }
    var box = bodyEl.appendChild(el('div', 'wl'));
    var meta = box.appendChild(el('div', 'bv-meta wl-meta'));
    var sumEl = meta.appendChild(el('span', 'wl-sum'));
    sumEl.setAttribute('role', 'status');
    meta.appendChild(el('span', 'gl-only', 'Look only'));
    var noteEl = box.appendChild(el('p', 'bv-note wl-note'));
    var empty = box.appendChild(el('p', 'bv-note wl-empty', 'No live sessions in your lands right now. A gardener appears here as soon as its session starts.'));
    var grid = box.appendChild(el('ul', 'wl-grid'));
    grid.setAttribute('aria-label', 'Live sessions, those that need you first');
    S = { doc: d, agents: {}, tiles: {}, es: null, ended: false, dirty: {}, order: '', frame: 0, grid: grid, sumEl: sumEl, noteEl: noteEl, empty: empty };
    setAgents(d.agents);
    if (d.partial) box.insertBefore(el('p', 'bv-note', 'Showing the first ' + (d.agents || []).length + ' agents.'), grid);
    flush();
    if (!d.live) { say('The live Wall is not available here yet: the garden has no coopmux feed for it.', true); return; }
    say('Connecting to the live sessions…');
    stream();
  }
  W.register('glass', { noun: 'wall', url: function () { return url(); }, paint: paint,
    hold: function () { return !!(S && S.doc); },   // a refetch would drop the tiles: the feed keeps them live
    stale: function () {}, closed: teardown }, ID);

  /* The Menu's Glass item opens the Wall (garden.html #cam-wall): the land header's "Glass" (spec 3.3). */
  var item = document.getElementById('cam-wall');
  if (item) { item.hidden = false; item.addEventListener('click', function () { W.open('glass', ID); }); }

  /* ---------- a gardener figure's card: its tile at small size (spec 3.3) ---------- */
  var board = document.getElementById('board'), card = null, C = null, hideTimer = 0, HIDE_MS = 250;
  function agentOf(node) { var h = node && node._h; return h && h.kind === 'gardener' ? (h.g && h.g.id) || h.id || '' : ''; }
  function figure(t) { return t && t.closest ? t.closest('a.hot.w-gardener') : null; }
  function cardEl() {
    if (card) return card;
    card = board.appendChild(el('div', 'wl-card px'));
    card.setAttribute('role', 'group');
    card.onpointerenter = function () { clearTimeout(hideTimer); };   // the mouse crosses onto its Watch
    card.onpointerleave = function (e) { if (e.pointerType === 'mouse' && C && !C.pinned) hideSoon(); };
    card.onclick = function (e) { if (e.target.closest('a')) closeCard(); };
    card.hidden = true;
    return card;
  }
  /* Below its figure (above it with no room below), on the map under the header and over the tab bar (GARDEN.map: a phone sideways
     has a short one, gd-LWjSUInmGI), and clear of the figure, the signs on the map and the camera buttons (gd-NzbcgfRNr4: under
     it, a plot's sign hid its first line): SIGNFIT.fit moves it to the nearest clear spot. */
  function placeCard(node) {
    if (!card || card.hidden) return;
    var b = board.getBoundingClientRect(), r = node.getBoundingClientRect(), w = card.offsetWidth, h = card.offsetHeight, v = GARDEN.map(), t = Math.max(8, v.top);
    var box = function (q) { return { x: q.left - b.left, y: q.top - b.top, w: q.width, h: q.height }; }, th = box(r), keep = [th].concat(SIGNFIT.keep());
    Array.prototype.forEach.call(document.querySelectorAll('#lbls .sign'), function (s) { var q = s.getBoundingClientRect(); if (q.width && q.height) keep.push(box(q)); });
    var f = SIGNFIT.fit({ x: th.x + th.w / 2 - w / 2, y: th.y + th.h + 8, w: w, h: h }, th, 'below', { x: 8, y: t, w: b.width - 16, h: b.height - t - Math.max(8, v.bottom || 0) }, 8, keep);
    card.style.left = f.x + 'px'; card.style.top = f.y + 'px';
  }
  function paintCard() {
    if (!C) return;
    var c = cardEl(), name = C.agent ? C.agent.agent || C.id : C.name;
    c.textContent = '';
    var head = c.appendChild(el('div', 'wl-head'));
    head.appendChild(el('b', 'wl-name', name));
    chipInto(head.appendChild(el('span', 'gl-state')), C.state || (C.agent && C.agent.state) || WORLD.glassState(C.g));
    if (C.screen) {
      var fitBox = c.appendChild(el('div', 'wl-scr small')), fitEl = fitBox.appendChild(el('div', 'wl-fit')), pre = fitEl.appendChild(el('pre', 'wl-pre'));
      c.hidden = false;   // measured shown
      paintScreen(pre, C.screen, THUMB_ROWS, true);
      fitTile(fitBox, fitEl, geometry(C.screen).cols, CARD_COLS);
    } else c.appendChild(el('div', 'wl-none', C.word || 'Connecting to its session…'));
    c.setAttribute('aria-label', name);
    c.appendChild(el('a', 'btn small gwatch', 'Watch')).href = base + '/garden' + W.withPanel('', 'glass', C.id);   // GL-12
    c.hidden = false;
    placeCard(C.node);
  }
  function showCard(node, one) {
    var id = one || agentOf(node);
    clearTimeout(hideTimer);
    if (!id) return;
    if (C && C.id === id) { C.node = node; paintCard(); return; }
    closeCard();
    C = { id: id, node: node, g: node._h.g, name: node._h.g.agent || id, agent: null, state: null, screen: null, word: '', es: null };
    if (typeof EventSource !== 'undefined') {
      var my = C, es = LIVE.source(base + url(id));   // web/live.js: a hover reconnects nothing else
      C.es = es;
      var mine = function () { return C === my; };
      es.addEventListener('agents', function (e) { var m = parse(e); if (!mine() || !m) return; my.agent = (m.agents || [])[0] || null; if (!my.agent) my.word = 'No live session.'; paintCard(); });
      var take = function (x) { if (!x || x.agent !== my.id) return; if (x.state) my.state = x.state; if (x.screen) my.screen = x.screen; };
      es.addEventListener('tiles', function (e) { var m = parse(e); if (!mine() || !m) return; (m.tiles || []).forEach(take); if (!(m.tiles || []).length && my.agent) my.word = 'Its session is not live.'; paintCard(); });
      es.addEventListener('screens', function (e) { var m = parse(e); if (mine() && m) { (m.screens || []).forEach(take); paintCard(); } });
      es.addEventListener('state', function (e) { var m = parse(e); if (mine() && m) { take(m); paintCard(); } });
      es.addEventListener('offline', function () { if (mine()) { my.screen = null; my.word = 'Its session ended.'; paintCard(); } });
      es.addEventListener('ended', function () { if (mine()) { es.close(); my.word = 'Its live screen is not available here.'; paintCard(); } });
      es.onerror = function () { if (mine() && es.readyState === 2) { my.word = 'Its live screen is not available here.'; paintCard(); } };
    }
    paintCard();
  }
  function closeCard() {
    clearTimeout(hideTimer);
    if (C && C.es) C.es.close();
    C = null;
    if (card) card.hidden = true;
  }
  function hideSoon() { clearTimeout(hideTimer); hideTimer = setTimeout(closeCard, HIDE_MS); }   // a figure to its neighbour keeps one feed
  if (board) {
    board.addEventListener('pointerover', function (e) { var f = figure(e.target); if (f && e.pointerType === 'mouse') showCard(f); });
    board.addEventListener('pointerout', function (e) { var f = figure(e.target); if (f && e.pointerType === 'mouse' && !(e.relatedTarget && f.contains(e.relatedTarget))) hideSoon(); });
    board.addEventListener('focusin', function (e) { var f = figure(e.target); if (f) showCard(f); else if (C) hideSoon(); });
    board.addEventListener('focusout', function (e) { if (figure(e.target) && !figure(e.relatedTarget)) hideSoon(); });
    board.addEventListener('click', function (e) { if (figure(e.target)) closeCard(); });   // it opens its thread (garden.js)
    board.addEventListener('click', function (e) {   // a finger's tap on a gardener drawn with no link (under 44 px): its card, pinned
      if (!matchMedia('(pointer: coarse)').matches || e.target.closest('a.hot.w-gardener,.wl-card')) return;
      if (e.target !== board && !e.target.closest('#cv,#hots,#ghots')) { closeCard(); return; }   // a control over the map (In, a beacon, the title bar) is no tap on what is drawn under it (gd-a628FpJRO5)
      var G = GARDEN.cam(), r = board.getBoundingClientRect(), w = WORLD.toWorld(G.cam, G.vp, e.clientX - r.left, e.clientY - r.top);
      var h = G.ghots.filter(function (x) { return x.kind === 'gardener' && WORLD.hit(x, { x: w.x, y: w.y, w: 0, h: 0 }); })[0];
      if (h) api.pin(h); else closeCard();
    }, true);
    document.addEventListener('visibilitychange', function () { if (document.hidden) closeCard(); });
    /* a figure focused or under the mouse before this file ran (it loads after the World, web/lazy.js) gets its card now */
    var had = figure(document.activeElement) || board.querySelector('a.hot.w-gardener:hover');
    if (had) showCard(had);
  }
  /* h (a GARDEN.cam hot): its card, pinned, as for a finger's tap on it; web/lazy.js replays one made before this file ran */
  api.pin = function (h) {
    if (!board) return;
    showCard({ _h: h, getBoundingClientRect: function () {   // where the figure is drawn now (api.place follows the camera)
      var b = board.getBoundingClientRect(), c = GARDEN.cam(), p = WORLD.toScreen(c.cam, c.vp, h.x, h.y);
      return { left: b.left + p.x, top: b.top + p.y, width: h.w * c.cam.z, height: h.h * c.cam.z };
    } }, h.id);
    if (C) C.pinned = true;
  };
  api.place = function () { if (C) placeCard(C.node); };   // the camera moved (garden.js)
  return api;
})();
if (typeof module !== 'undefined') module.exports = WALL;
