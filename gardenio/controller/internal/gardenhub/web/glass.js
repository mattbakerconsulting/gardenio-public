/* web/glass.js: the Glass, watch (workbench-glass spec GL-1, gd-9adIfFKLls), the Workbench's glass kind,
   /garden?glass=<agent bead id>: one agent's terminal, live, look only. GET /garden/glass/<id> (glass.go) paints the
   header (GL-3) and the cached screen at once; a live agent then loads xterm.js (web/vendor/xterm.bundle.js, hashed,
   the first time a Glass opens) and the relay's SSE stream, /garden/glass/<id>/stream (internal/glass): replay (reset, then
   the recent bytes), pty (live bytes), screen (a resync past the relay's ring: reset, then the lines), state and usage (the
   chip, GL-4, Prompt and Error pulsing; cost and context), exit and ended (gone, refused, unavailable: the last screen
   stays, with why in words). Watch never writes to the PTY (G-D3): no input, keys or resize. The terminal keeps the PTY's
   geometry, scaled down to the panel (GL-2) unless that would shrink its glyphs under 10 px (a phone, upright): then it starts
   at Actual size, unscaled, to pan sideways. A pinch on the terminal sets its glyphs' size, at Actual size (pinchFont,
   gd-2yJFWJnO1I); the size toggle beside the chip picks either at FONT. Nothing rewraps. The terminal comes first: its transcript sits under it (the gardener's last calls, web/transcript.js
   follow(), gd-GPu096omMw), folded on a phone, and the header's rows under that (gd-bR8ZcOmzM5).
   Opened from the agent bead's face, a gardener's Commons thread header and agent card, a figure's card (web/wall.js) and
   a decision or permission card's See the screen (at the prompt; glass:to tells web/glassact.js). Close returns to the
   opener. Only for a viewer who may watch (body data-glass: operator tier), whose page loads page-glass.js unasked
   (web/lazy.js); the routes check again. Text goes in through textContent only.
   Nudge and respond are web/glassact.js's, in the slot under the terminal: glass:open {id, doc, slot},
   glass:state {id, state} and glass:close tell it; it answers glass:prompt {id, on, prompt}, coop's reading, which outranks
   the bead's agent_state (GL-4): the chip says Prompt on either coop reading, stream or not. Attach is web/glasskey.js's, under the terminal (glass:open's keys): told glass:term {id, term} and glass:control {id,
   control}, it answers glass:attached {id, on}, and the panel stops saying "look only". This module types nothing. */
var GLASS = (function () {
  /* ---------- pure logic (also run under node by the Go tests) ---------- */
  function url(id) { return '/garden/glass/' + encodeURIComponent(id); }
  /* web/world.js's, which every page loads */
  var WD = typeof WORLD !== 'undefined' ? WORLD : require('./world.js'), stateView = WD.stateView, waitLine = WD.waitLine;
  /* A token count in words: "48k tokens". */
  function tokens(n) { n = Number(n) || 0; return n >= 1000 ? Math.round(n / 1000) + 'k tokens' : n + (n === 1 ? ' token' : ' tokens'); }
  /* What a usage event says, {cost, context_tokens}: coop's usage:update carries the running totals under cumulative, its
     usage snapshot only the token counters (no cost, no context level). '' and 0 for what it does not say. */
  function usageOf(u) {
    var c = (u && u.cumulative) || u || {};
    return { cost: Number(c.total_cost_usd) > 0 ? '$' + Number(c.total_cost_usd).toFixed(2) : '', context_tokens: Number(c.context_tokens) > 0 ? Number(c.context_tokens) : 0 };
  }
  /* The header's rows (GL-3) from the document, [label, text, bead]: the task it tends (a bead link), where and as what it
     works, its pod, account, cost so far and context size. Rows with nothing to say are left out. */
  function headRows(d) {
    d = d || {};
    var rows = [];
    if (d.task) rows.push(['Tending', d.task_title || d.task, d.task]);
    var works = [d.project ? 'on ' + d.project : '', d.role ? 'as ' + d.role : ''].filter(Boolean).join(' ');
    if (works) rows.push(['Works', works]);
    if (d.pod) rows.push(['Pod', d.pod]);
    if (d.account) rows.push(['Account', d.account]);
    if (d.cost) rows.push(['Spent so far', d.cost]);
    if (d.context_tokens > 0) rows.push(['Context', tokens(d.context_tokens)]);
    return rows;
  }
  /* web/wall.js's geometry, fitScale and 16 colours, kept the same by TestGlassLogicUnderNode: page-glass.js does not wait
     on page-wall.js, which a Glass watch never draws (B1, gd-gjqJXrcXsJ). */
  function geometry(s) {
    var c = s && Number(s.cols), r = s && Number(s.rows);
    return c >= 2 && c <= 1000 && r >= 1 && r <= 500 ? { cols: c, rows: r } : { cols: 80, rows: 24 };
  }
  function fitScale(boxW, natW) { return boxW > 0 && natW > boxW ? boxW / natW : 1; }
  var COLORS = ['#000000', '#e05252', '#0dbc79', '#e5e510', '#3b82d6', '#c84cc8', '#11a8cd', '#e5e5e5',
    '#808080', '#f14c4c', '#23d18b', '#f5f543', '#3b8eea', '#d670d6', '#29b8db', '#ffffff'];
  /* The least scale that keeps .gl-pre's and xterm's 13 px glyphs legible: 10 px, the Wall's floor too (MIN_SCALE). */
  var READABLE = 10 / 13;
  /* Whether a terminal natW px wide shows at Actual size in a box boxW px wide: as the viewer chose (true, false), else
     (null) only when fitting it would scale it under READABLE. */
  function actualSize(boxW, natW, chosen) { return chosen != null ? !!chosen : fitScale(boxW, natW) < READABLE; }
  /* The glyphs' size in px after a pinch that spread the fingers by ratio over a terminal of px glyphs shown at scale shown:
     what the fingers left on the screen, whole, FONT_MIN to FONT_MAX. */
  var FONT = 13, FONT_MIN = 6, FONT_MAX = 32;
  function pinchFont(px, shown, ratio) { var n = Math.round(px * (shown || 1) * ratio); return isFinite(n) ? Math.max(FONT_MIN, Math.min(FONT_MAX, n)) : FONT; }
  /* A screen as the bytes that paint it on a reset terminal: its lines, then the cursor where coop says it is. */
  function screenText(s) {
    var out = ((s && s.lines) || []).join('\r\n'), c = s && s.cursor;
    if (c && c.row >= 0 && c.col >= 0) out += '\x1b[' + (Math.floor(c.row) + 1) + ';' + (Math.floor(c.col) + 1) + 'H';
    return out;
  }
  /* base64 to bytes, '' for none or a bad one. */
  function bytes(b64) {
    var s;
    try { s = atob(String(b64 || '')); } catch (e) { return new Uint8Array(0); }
    var out = new Uint8Array(s.length);
    for (var i = 0; i < s.length; i++) out[i] = s.charCodeAt(i);
    return out;
  }
  /* Why the live terminal stopped, from the relay's ended reason (internal/glass terminal()). */
  function endedLine(reason) {
    if (reason === 'gone') return 'The session is over: coopmux no longer has it (the agent left the farm, or its pod was replaced). This is its last screen.';
    if (reason === 'refused') return 'Coopmux refused the relay, so the live terminal is unavailable. This is the last screen the garden had.';
    if (reason === 'unavailable') return 'The live terminal is not available here yet: the garden cannot reach coopmux for it. This is the last screen the garden had.';
    return 'The live terminal stopped. This is its last screen.';
  }
  /* The line for an agent with no live session: what is shown instead. */
  function notLive(d) {
    return d && d.screen ? 'Not live: coopmux has no session for this agent now. This is the last screen it had.' : 'Not live: coopmux has no session for this agent now, and no screen to show.';
  }
  function exitLine(e) {
    var code = e && e.code != null ? e.code : null, sig = e && e.signal != null ? e.signal : null;
    return 'The agent process exited' + (code != null ? ' with code ' + code : sig != null ? ' on signal ' + sig : '') + '.';
  }
  /* The transcript section under the terminal (gd-GPu096omMw): its heading's words for rows newest first, the calls held
     and when the last was. */
  var AC = typeof ACTIVITY !== 'undefined' ? ACTIVITY : require('./activity.js');
  function txHead(rows) {
    var n = (rows && rows.length) || 0;
    return n ? (n === 1 ? 'Its last call' : 'Its last ' + n + ' calls') + ', the last at ' + AC.clock(rows[0].at || 0) : 'No calls yet';
  }
  /* Whether the section starts folded in a panel w px wide: as the viewer last left it (saved '1' open, '0' folded), else
     folded under FOLD_W, so a phone's terminal keeps the screen. */
  var FOLD_W = 600;
  function folded(w, saved) { return saved === '0' ? true : saved === '1' ? false : w > 0 && w < FOLD_W; }
  var api = { url: url, waitLine: waitLine, txHead: txHead, folded: folded, FOLD_W: FOLD_W, stateView: stateView, tokens: tokens, usageOf: usageOf, headRows: headRows, geometry: geometry, fitScale: fitScale, COLORS: COLORS, READABLE: READABLE, actualSize: actualSize, FONT: FONT, pinchFont: pinchFont,
    screenText: screenText, bytes: bytes, endedLine: endedLine, notLive: notLive, exitLine: exitLine };
  if (typeof document === 'undefined' || typeof WORKBENCH === 'undefined') return api;

  /* ---------- DOM ---------- */
  var W = WORKBENCH, B = document.body, base = B.dataset.base || '';
  var titleEl = document.getElementById('bv-title'), kindEl = document.getElementById('bv-kind'), bodyEl = document.getElementById('bv-body');
  if (!bodyEl || B.dataset.glass !== '1') return api;
  var XTERM = { js: B.dataset.xtermJs || '', css: B.dataset.xtermCss || '' };   // web/vendor's xterm.js, hashed (assets.go)
  /* The terminal's font: the page's display stack (garden.css --font-display, a monospace one). */
  function font() {
    var f = '';
    try { f = getComputedStyle(document.documentElement).getPropertyValue('--font-display').trim(); } catch (e) { /* no styles yet */ }
    return f || 'ui-monospace,Menlo,Consolas,monospace';
  }
  /* Resolves once that font is loaded, so xterm measures its cells in it, not in a fallback. */
  function fontReady() {
    if (!document.fonts || !document.fonts.load) return Promise.resolve();
    return document.fonts.load('400 13px ' + font()).then(function () {}, function () {});
  }
  var S = null;   // the Glass shown: {id, doc, box, fit, pre, term, es, ended, actual (the viewer's choice, null for none), font, scale (as fit() shows it), sizeEl, attached, relay, noteEl, chipEl, detailEl, rowsEl, onlyEl, usage}

  function el(t, cls, text) { var e = document.createElement(t); if (cls) e.className = cls; if (text !== undefined) e.textContent = text; return e; }

  /* xterm.js, loaded once, the first time a live Glass opens (GL-1); a promise of window.Terminal. */
  var xtermP = null;
  function xterm() {
    if (window.Terminal) return Promise.resolve(window.Terminal);
    if (xtermP) return xtermP;
    xtermP = new Promise(function (ok, no) {
      var css = el('link'); css.rel = 'stylesheet'; css.href = XTERM.css; document.head.appendChild(css);
      var js = el('script'); js.src = XTERM.js; js.async = true;
      js.onload = function () { if (window.Terminal) ok(window.Terminal); else no(new Error('no Terminal')); };
      js.onerror = function () { no(new Error('xterm did not load')); };
      document.head.appendChild(js);
    });
    xtermP.catch(function () { xtermP = null; });   // the next Glass tries again
    return xtermP;
  }

  /* Tell web/glassact.js (nudge and respond) and web/glasskey.js (attach) about the Glass. */
  function tell(name, detail) { try { document.dispatchEvent(new CustomEvent(name, { detail: detail })); } catch (e) { /* no CustomEvent */ } }
  /* The line for a live terminal: look only, or the keyboard this viewer holds (glass:attached). */
  function liveLine() { return S && S.attached ? 'Live. You have the keyboard: what you type goes to the agent.' : 'Live. Look only: you cannot type into it here.'; }
  function say(text, err) { if (S && S.noteEl) { S.noteEl.textContent = text; S.noteEl.className = 'bv-note gl-note' + (err ? ' err' : ''); } }
  function chip(s) {
    if (!S) return;
    var v = stateView(s, !!S.doc.can_respond);
    S.chipEl.textContent = '';
    var dot = S.chipEl.appendChild(el('span', 'gl-dot'));
    dot.setAttribute('aria-hidden', 'true');
    S.chipEl.appendChild(el('span', '', v.word));
    S.chipEl.className = 'gl-state ' + v.tone + (v.pulse ? ' pulse' : '');
    S.chipEl.dataset.state = v.key;
    S.detailEl.textContent = v.detail;
    S.detailEl.hidden = !v.detail;
  }
  function rows() {
    if (!S) return;
    var d = {}, k;
    for (k in S.doc) d[k] = S.doc[k];
    d.cost = S.usage.cost || d.cost;   // the stream's running totals supersede the bead's
    d.context_tokens = S.usage.context_tokens || d.context_tokens;
    S.rowsEl.textContent = '';
    headRows(d).forEach(function (r) {
      S.rowsEl.appendChild(el('dt', '', r[0]));
      var dd = S.rowsEl.appendChild(el('dd'));
      if (r[2]) { var a = dd.appendChild(el('a', '', r[1])); a.href = base + '/garden' + W.withPanel('', 'bead', r[2]); }
      else dd.textContent = r[1];
    });
  }

  /* Scale the terminal to the box's width (GL-2), or show it at its own size to pan (actualSize); the toggle says which. */
  function fit() {
    if (!S || !S.fit) return;
    var f = S.fit, box = S.box, a = actualSize(box.clientWidth, f.offsetWidth, S.actual), s = a ? 1 : fitScale(box.clientWidth, f.offsetWidth);
    S.scale = s;
    f.style.transform = s < 1 ? 'scale(' + s + ')' : '';
    box.style.height = a ? '' : Math.ceil(f.offsetHeight * s) + 'px';
    box.classList.toggle('actual', a);
    S.sizeEl.textContent = a ? 'Fit to the panel' : 'Actual size';
    S.sizeEl.setAttribute('aria-pressed', String(a));
  }
  function paintPre(screen) {
    if (!S) return;
    if (!S.pre) S.pre = S.fit.appendChild(el('pre', 'gl-pre'));
    var g = geometry(screen), lines = (screen && screen.lines) || [];
    S.pre.textContent = lines.join('\n');
    S.pre.style.fontSize = S.font + 'px';
    S.pre.style.width = g.cols + 'ch';   // the PTY's own width, whatever the lines hold
    S.pre.style.minHeight = (g.rows * 1.2) + 'em';
    fit();
  }
  /* The Wall's 16 ANSI colours (COLORS) on the always-dark terminal; xterm lifts any colour
     under 4.5:1 and garden.css its dim text (gd-pHyKC0Uef1). */
  function theme() {
    var t = { background: '#0b1220', foreground: '#e6e1cf', cursor: '#f0b43a', selectionBackground: '#4c609b' }, c = COLORS;
    'black red green yellow blue magenta cyan white'.split(' ').forEach(function (k, i) { t[k] = c[i]; t['bright' + k[0].toUpperCase() + k.slice(1)] = c[i + 8]; });
    return t;
  }
  /* The terminal: xterm at the PTY's geometry, stdin off, no handler that could write anything back. The cached screen is
     its first paint; the replay that follows resets it. */
  function startTerm(T) {
    if (!S || S.term) return;
    var g = geometry(S.doc.screen);
    var term = new T({ cols: g.cols, rows: g.rows, disableStdin: true, cursorBlink: false, cursorStyle: 'block', scrollback: 1000,
      fontSize: S.font, lineHeight: 1, fontFamily: font(), convertEol: false, allowTransparency: false,
      minimumContrastRatio: 4.5, theme: theme() });
    var host = el('div', 'gl-x');
    if (S.pre) { S.pre.remove(); S.pre = null; }
    S.fit.appendChild(host);
    term.open(host);
    if (term.textarea) { term.textarea.readOnly = true; term.textarea.tabIndex = -1; }   // look only: nothing to type into
    S.term = term;
    tell('glass:term', { id: S.id, term: term });   // web/glasskey.js types into it once the viewer attaches
    if (S.doc.screen) term.write(screenText(S.doc.screen));
    term.write('', fit);
  }
  /* Reset term and write data, in order: xterm queues writes but runs reset() and resize() at once, so a reset that way could
     land before bytes still queued (a cached screen, live pty) and the new bytes paint over the old. RIS (ESC c) resets
     in-band, and a resize to the PTY's geometry g (this page's xterm only) runs from the queue. modes is the relay's: the
     bytes that set the app's alternate screen and mouse tracking, written after the reset, since the ring that follows
     rarely holds them (gd-bftIGCMNVc); without them the wheel scrolls here instead of reaching the app. */
  var RIS = '\x1bc';
  function repaint(term, data, g, modes) {
    if (g) term.write('', function () { if (term.cols !== g.cols || term.rows !== g.rows) term.resize(g.cols, g.rows); });
    term.write(RIS + (modes || ''));
    term.write(data, fit);
  }

  /* The terminal's glyphs at px: xterm measures its cells again at the PTY's own geometry, so nothing rewraps. */
  function setSize(px) {
    if (!S) return;
    S.font = px;
    if (S.term) { try { S.term.options.fontSize = px; } catch (e) { /* an xterm without options */ } }
    if (S.pre) S.pre.style.fontSize = px + 'px';
    requestAnimationFrame(fit);
  }
  /* Two fingers on the terminal size its glyphs, not the page: scaled as they spread, set when one lifts. */
  function pinchable(box) {
    var p = null;
    function gap(t) { return Math.hypot(t[0].clientX - t[1].clientX, t[0].clientY - t[1].clientY); }
    box.addEventListener('touchstart', function (e) { if (S && e.touches.length === 2) p = { d: gap(e.touches) || 1, s: S.scale || 1, r: 0 }; }, { passive: true });
    box.addEventListener('touchmove', function (e) {
      if (!p || !S || e.touches.length !== 2) return;
      if (e.cancelable) e.preventDefault();
      p.r = gap(e.touches) / p.d;
      S.fit.style.transform = 'scale(' + p.s * p.r + ')';
    }, { passive: false });
    function end(e) {
      if (!p || e.touches.length >= 2) return;
      var q = p; p = null;
      if (!S) return;
      if (!q.r) { fit(); return; }
      S.actual = true;
      setSize(pinchFont(S.font, q.s, q.r));
    }
    box.addEventListener('touchend', end);
    box.addEventListener('touchcancel', end);
    box.addEventListener('gesturestart', function (e) { e.preventDefault(); });   // Safari's own pinch
  }

  function parse(e) { try { return JSON.parse(e.data); } catch (x) { return null; } }
  /* The relay's stream, once the terminal is up (or could not load: then screens only). */
  function stream() {
    if (!S || S.es || S.ended || !S.doc.live || !S.doc.stream || typeof EventSource === 'undefined') return;
    var my = S, es = LIVE.source(S.doc.stream), got = false;   // web/live.js
    S.es = es;
    function mine() { return S === my && my.es === es; }
    es.addEventListener('replay', function (e) {
      var m = parse(e); if (!mine() || !m) return;
      if (my.term) repaint(my.term, bytes(m.data), null, m.modes);
      got = true; say(my.term ? liveLine() : 'Live, as screens: the terminal did not load, so this shows each resync.');
    });
    es.addEventListener('pty', function (e) {
      var m = parse(e); if (!mine() || !m || !my.term) return;
      my.term.write(bytes(m.data));
    });
    es.addEventListener('screen', function (e) {
      var m = parse(e); if (!mine() || !m) return;
      if (my.term) repaint(my.term, screenText(m), geometry(m), m.modes); else paintPre(m);
      if (!got) { got = true; say(liveLine()); }
    });
    es.addEventListener('state', function (e) { var m = parse(e); if (mine() && m) { my.relay = m; chip(m); tell('glass:state', { id: my.id, state: m }); txFollow(); plotLink(); } });   // a new state: what it did to get there
    es.addEventListener('usage', function (e) { var m = parse(e); if (mine() && m) { my.usage = usageOf(m); rows(); } });
    es.addEventListener('control', function (e) { var m = parse(e); if (mine() && m) tell('glass:control', { id: my.id, control: m }); });
    es.addEventListener('exit', function (e) { var m = parse(e); if (mine()) say(exitLine(m)); });
    es.addEventListener('ended', function (e) {
      var m = parse(e); if (!mine()) return;
      es.close(); my.es = null; my.ended = true;
      say(endedLine(m && m.reason), true);
    });
    es.onopen = function () { if (mine() && !got) say('Connected. Waiting for the terminal…'); };
    es.onerror = function () {
      if (!mine()) return;
      if (es.readyState === 2) { my.es = null; my.ended = true; say('The live terminal is unavailable right now: the garden did not open its stream. This is the last screen it had.', true); }
      else say('Reconnecting to the live terminal…');
    };
  }
  function unstream() { if (S && S.es) { S.es.close(); S.es = null; } }
  document.addEventListener('visibilitychange', function () {
    if (!S || !S.doc) return;
    if (document.hidden) unstream();   // the relay keeps the upstream a while; coming back is a fresh replay
    else stream();
  });
  window.addEventListener('resize', function () { fit(); });

  function teardown() {
    if (!S) return;
    tell('glass:close', { id: S.id });
    unstream();
    if (S.term) { try { S.term.dispose(); } catch (e) { /* already gone */ } }
    S = null;
  }
  function view(id, d) {
    var box = el('div', 'gl');
    var meta = box.appendChild(el('div', 'bv-meta gl-meta'));
    var chipEl = meta.appendChild(el('span', 'gl-state'));
    chipEl.setAttribute('role', 'status');
    var onlyEl = meta.appendChild(el('span', 'gl-only', 'Look only'));
    var size = meta.appendChild(el('button', 'btn small ghost gl-size', 'Actual size')); size.type = 'button';
    size.setAttribute('aria-pressed', 'false');
    size.onclick = function () {
      if (!S) return;
      S.actual = !S.box.classList.contains('actual');
      setSize(FONT);
    };
    var detailEl = box.appendChild(el('p', 'bv-note gl-detail')); detailEl.hidden = true;
    var noteEl = box.appendChild(el('p', 'bv-note gl-note'));
    noteEl.setAttribute('role', 'status');
    var term = box.appendChild(el('div', 'gl-term'));
    term.setAttribute('role', 'region'); term.setAttribute('aria-label', 'Terminal of ' + (d.agent || id) + ', look only');
    term.tabIndex = 0;   // scrolls with the keys at Actual size
    var fitEl = term.appendChild(el('div', 'gl-fit'));
    pinchable(term);
    var keys = box.appendChild(el('div', 'gl-keys'));   // attach (web/glasskey.js) fills it, its key row pinned under the terminal
    var write = box.appendChild(el('div', 'gl-write'));   // nudge and respond (web/glassact.js) fill it
    var tx = txSection(d.agent || id); box.appendChild(tx.el);   // what it did, under what it shows
    var rowsEl = box.appendChild(el('dl', 'bv-fields gl-rows'));   // the header (GL-3), under the terminal it describes
    var acts = box.appendChild(el('div', 'bv-acts'));
    var bead = acts.appendChild(el('a', 'btn small ghost', 'Open the agent bead')); bead.href = base + '/garden' + W.withPanel('', 'bead', id);
    acts.appendChild(el('a', 'btn small ghost', 'Open the transcript alone')).href = base + '/garden' + W.withPanel('', 'transcript', id);   // its calls, captioned (web/transcript.js)
    // its plot on the map, the cursor on its figure there (data-cursor; goPlot); none for a gardener on no plot
    var plot = acts.appendChild(el('a', 'btn small ghost gl-plot', 'Go to its plot')); plot.dataset.cursor = id; plot.hidden = true;
    plot.onclick = function (e) { if (e.button || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return; e.preventDefault(); goPlot(); };
    // relay: coop's latest word on the agent's state (a stream state, or a prompt web/glassact.js read), null until it says
    S = { id: id, doc: d, box: term, fit: fitEl, write: write, keys: keys, pre: null, term: null, es: null, ended: false, actual: null, font: FONT, scale: 1, sizeEl: size, attached: false,
      relay: null, noteEl: noteEl, chipEl: chipEl, detailEl: detailEl, rowsEl: rowsEl, onlyEl: onlyEl, usage: { cost: '', context_tokens: 0 }, tx: tx, plotEl: plot };
    return box;
  }
  /* The transcript section (gd-GPu096omMw): a heading that folds it, then the calls as web/transcript.js lists them. */
  var FOLD_KEY = 'gardenio.glass.transcript';
  function saved() { try { return localStorage.getItem(FOLD_KEY); } catch (e) { return null; } }
  function txSection(name) {
    var sec = el('section', 'gl-tx'); sec.setAttribute('aria-label', 'Transcript');
    var h = sec.appendChild(el('h3', 'gl-tx-h'));
    var fold = h.appendChild(el('button', 'gl-tx-fold')); fold.type = 'button';
    fold.appendChild(el('span', 'gl-tx-k', 'Transcript'));
    var sum = fold.appendChild(el('span', 'gl-tx-sum', 'Loading its calls…'));
    var note = sec.appendChild(el('p', 'bv-note gl-tx-note')); note.setAttribute('role', 'status'); note.hidden = true;
    var list = sec.appendChild(el('ol', 'tx-list gl-tx-list')); list.setAttribute('aria-label', 'Calls of ' + name + ', newest first');
    list.id = 'gl-tx-list'; fold.setAttribute('aria-controls', list.id);
    function set(shut) { sec.classList.toggle('shut', shut); fold.setAttribute('aria-expanded', String(!shut)); }
    set(folded(bodyEl.clientWidth || window.innerWidth, saved()));
    fold.onclick = function () {
      var shut = !sec.classList.contains('shut');
      set(shut);
      try { localStorage.setItem(FOLD_KEY, shut ? '0' : '1'); } catch (e) { /* no storage: it folds for this panel only */ }
    };
    return { el: sec, sum: sum, note: note, list: list };
  }
  /* The rows web/transcript.js holds for the Glass's gardener, or why it has none (status: the fetch's). */
  function txRows(rows, status) {
    if (!S || !S.tx || typeof TRANSCRIPT === 'undefined' || !TRANSCRIPT.row) return;
    var t = S.tx;
    t.sum.textContent = status != null && !rows.length ? 'Not loaded' : txHead(rows);
    t.note.hidden = status == null;
    t.note.textContent = status === 404 ? 'The farm has no calls of this agent: it is not a gardener on the farm, or its transcript is not yours to read.'
      : status != null ? 'Could not load its calls' + (status ? ' (' + status + ')' : ': the garden did not answer') + '. They load again on its next state change.' : '';
    t.list.textContent = '';
    rows.forEach(function (r) { t.list.appendChild(TRANSCRIPT.row(r)); });
    if (typeof TITLES !== 'undefined') TITLES.dress(t.list);   // a bead id in a caption reads as its title, and opens it
  }
  function txFollow() { if (S && typeof TRANSCRIPT !== 'undefined' && TRANSCRIPT.follow) TRANSCRIPT.follow(S.id, txRows); }
  function unfollow() { if (typeof TRANSCRIPT !== 'undefined' && TRANSCRIPT.follow) TRANSCRIPT.follow(''); }
  /* Go to its plot: the plot's ?at= the map knows for this gardener (web/world.js gardenerKnown), hidden while it has none. */
  function plotLink() {
    if (!S || !S.plotEl) return;
    var k = window.GARDEN && GARDEN.known ? GARDEN.known(S.id) : null, at = k && k.plotAt;
    S.plotEl.hidden = !at;
    if (at) S.plotEl.href = base + '/garden?at=' + at;
  }
  /* Go to its plot, the Glass's second stop (H-D21): the panel closes and the map flies to the gardener's plot figure at full
     zoom (GARDEN.plot). A resting gardener has no plot: the terminal bumps and #cursay says so. */
  function goPlot() {
    if (!S) return;
    var id = S.id, name = S.doc.agent || id, box = S.box, say = document.getElementById('cursay');
    if (window.GARDEN && GARDEN.plot && GARDEN.plot(id)) return;
    if (say) say.textContent = name + ' is not on a plot';
    box.classList.add('bump'); setTimeout(function () { box.classList.remove('bump'); }, 140);
  }
  /* Enter on the panel (its title, which the open focuses) or the terminal's fit box goes to its plot; never in a text box, the
     nudge box or an attached terminal, whose keys web/glasskey.js and web/glassact.js hold. The map's keys never take it
     (garden.js reads the open bead view's Enter as the cursor's), and the Workbench's Escape still only closes. */
  var panelEl = document.getElementById('bead');
  if (panelEl) panelEl.addEventListener('keydown', function (e) {
    if (e.key !== 'Enter' || e.ctrlKey || e.metaKey || e.altKey || e.shiftKey || panelEl.dataset.kind !== 'glass') return;
    var t = e.target;
    if (t !== panelEl && t !== titleEl && !(S && (t === S.box || t === S.fit))) return;
    e.stopPropagation();
    if (e.defaultPrevented || !S || !S.doc) return;   // web/glasskey.js took it to attach, or the panel is still loading
    e.preventDefault(); goPlot();
  });
  function paint(id, d, note, isErr, retry) {
    teardown();
    kindEl.textContent = 'glass';
    titleEl.textContent = (d && d.agent) || id;
    bodyEl.textContent = '';
    if (!d) {
      var n = bodyEl.appendChild(el('p', 'bv-note' + (isErr ? ' err' : ''), note || ''));
      n.setAttribute('role', 'status');
      if (retry) { var b = n.appendChild(el('button', 'btn small bv-retry', 'Try again')); b.type = 'button'; b.onclick = function () { W.reload('glass', id); }; }
      return;
    }
    bodyEl.appendChild(view(id, d));
    chip(d.state);
    rows();
    plotLink();
    txFollow();
    tell('glass:open', { id: id, doc: d, slot: S.write, keys: S.keys, box: S.box });
    if (d.screen) paintPre(d.screen);
    if (!d.live) { say(notLive(d)); return; }
    say('Connecting to the live terminal…');
    var my = S;
    xterm().then(function (T) { return fontReady().then(function () { if (S === my) { startTerm(T); stream(); } }); }, function () {
      if (S !== my) return;
      say('The terminal did not load, so this shows screens only.', true);
      stream();
    });
  }

  /* web/glasskey.js holds the keyboard (on) or let go of it: the panel says which. */
  document.addEventListener('glass:attached', function (e) {
    var d = e.detail || {};
    if (!S || d.id !== S.id || S.attached === !!d.on) return;
    S.attached = !!d.on;
    S.onlyEl.textContent = S.attached ? 'Attached' : 'Look only';
    S.onlyEl.classList.toggle('on', S.attached);
    S.box.setAttribute('aria-label', 'Terminal of ' + (S.doc.agent || S.id) + (S.attached ? ', attached: what you type goes to the agent' : ', look only'));
    if (S.es && !S.ended) say(liveLine());
  });

  /* web/glassact.js read the prompt off coop: on, the agent waits at one (prompt is {type, tool}); off, coop has none. That
     is coop's state as much as a stream state is, and it outranks the bead's agent_state (GL-4): with no stream (no relay
     key, 6.3b) or before its first state, the chip still says Prompt. A prompt the stream reported waits for the stream to
     say it went. */
  document.addEventListener('glass:prompt', function (e) {
    var d = e.detail || {};
    if (!S || d.id !== S.id) return;
    var at = !!S.relay && String(S.relay.state || '').toLowerCase() === 'prompt';
    if (d.on && !at) S.relay = { state: 'prompt', attention: true, prompt: d.prompt || null, read: true };
    else if (!d.on && at && S.relay.read) S.relay = null;
    else return;
    chip(S.relay || S.doc.state);
  });

  W.register('glass', { noun: 'agent', url: url, paint: paint,
    hold: function () { return !!(S && S.doc); },   // a refetch would reset the terminal: it keeps itself live
    stale: function () {}, closed: function () { teardown(); unfollow(); },
    /* Opened from a decision or permission card's See the screen (data-glass-to=prompt): web/glassact.js scrolls to the
       prompt once it is read (spec 3.3). */
    followed: function (id, a) { if (a && a.dataset && a.dataset.glassTo) tell('glass:to', { id: id, to: a.dataset.glassTo }); } });

  return api;
})();
if (typeof module !== 'undefined') module.exports = GLASS;
