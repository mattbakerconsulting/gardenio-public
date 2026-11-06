/* web/glasskey.js: the Glass, attach (Lands workbench-glass spec GL-9, GL-10, GL-11; step 6.6, gd-mWooPMTNwA). Watch
   (web/glass.js) is look only, nudge and respond (web/glassact.js) are not the keyboard; this module is the keyboard, and
   only once the viewer takes it:
   - Attach: POST /garden/glass/<id>/control action=claim (glass_attach.go). The relay tells every watcher of the session
     who holds the keyboard (the stream's control event, glass:control here), so each sees "Attached as <them>" above the
     terminal. A keyboard someone else holds is taken only with "Take the keyboard from <them>" (take=1); they are told.
   - The terminal is the way in (operator, decision gd-vRWdWdROSa; gd-FpaO6Pb9bR): a click or tap on it, or Enter on it
     from the keyboard, attaches, and its textarea takes focus inside that gesture, so a phone opens its keyboard. There is
     no input field and no key buttons: attached, the terminal wears the accent frame and a blinking cursor, and what is
     typed shows the way any terminal shows it, echoed by the PTY.
   - Attached, the terminal takes typing: what xterm hands over (onData) is split into typed text (input) and named keys
     (Esc, Tab, Enter, arrows, Ctrl chords; keys), in order, through POST /garden/glass/<id>/send. Escape sequences coop
     cannot name are dropped, never sent as text. Once the app turns mouse tracking on, xterm's SGR reports of a click or
     wheel turn go as mouse (coop re-encodes them; motion is dropped) and a click no longer moves the cursor. Under it, one row pinned to the foot of the pane while the terminal is in view (on a
     phone it scrolls sideways rather than wrap, gd-dDsLGuIAGI): on a touch screen the keys a phone's keyboard lacks (Esc, Tab,
     Ctrl, held for the next letter typed, the arrows and Paste; TOUCH, gd-2yJFWJnO1I), then Fit width (resize), Compact
     (GL-10), Let go.
   - A click or tap in the prompt moves the cursor there: Up/Down to the row, then Left/Right along it, counted in
     characters (moveTo). Only inside the block of rows around the caret that no blank row or box rule bounds (the prompt
     box Claude Code draws), so a click on the output above moves nothing and Up never reaches the prompt history. The
     caret is the single inverse-video cell an Ink app like Claude Code draws, else the terminal's own cursor.
   - Esc, Ctrl-C, Ctrl-D and Ctrl-Z, and Compact, ask first, typed straight into the terminal too (Esc because Claude Code
     reads it as "interrupt"): the server answers "confirm" and sends nothing until Send is pressed (confirmed=1); Cancel,
     or Esc in the question, is audited (cancel=1). The question takes the key row's place, Cancel focused, and typing
     goes back to the terminal once it is answered. A refusal says why in the server's words.
   - Letting go: Esc Esc (two within ESC_GAP; a lone Esc is sent once the gap passes), a minute away (the tab hidden, the
     window or the panel left; BLUR_MS), closing the Glass or the page, or the claim lapsing or being taken. The claim is
     renewed every RENEW_MS while held, and by every send.
   glass.js tells this module about its Glass (glass:open {id, doc, keys}, glass:term {id, term}, glass:control and
   glass:close); this module tells it when the viewer holds the keyboard (glass:attached {id, on}). Text goes in through
   textContent only. */
var GLASSKEY = (function () {
  /* ---------- pure logic (also run under node by the Go tests) ---------- */
  function sendURL(id) { return '/garden/glass/' + encodeURIComponent(id) + '/send'; }
  function controlURL(id) { return '/garden/glass/' + encodeURIComponent(id) + '/control'; }
  var ESC_GAP = 400, BLUR_MS = 60000, RENEW_MS = 30000, TEXT_GAP = 30;
  /* Escape sequences xterm sends for keys coop can name (coop encode_key), normal and application cursor modes. */
  var SEQ = { '\x1b[A': 'Up', '\x1b[B': 'Down', '\x1b[C': 'Right', '\x1b[D': 'Left', '\x1bOA': 'Up', '\x1bOB': 'Down', '\x1bOC': 'Right', '\x1bOD': 'Left',
    '\x1b[H': 'Home', '\x1b[F': 'End', '\x1bOH': 'Home', '\x1bOF': 'End', '\x1b[1~': 'Home', '\x1b[4~': 'End',
    '\x1b[2~': 'Insert', '\x1b[3~': 'Delete', '\x1b[5~': 'PageUp', '\x1b[6~': 'PageDown',
    '\x1bOP': 'F1', '\x1bOQ': 'F2', '\x1bOR': 'F3', '\x1bOS': 'F4', '\x1b[15~': 'F5', '\x1b[17~': 'F6', '\x1b[18~': 'F7', '\x1b[19~': 'F8',
    '\x1b[20~': 'F9', '\x1b[21~': 'F10', '\x1b[23~': 'F11', '\x1b[24~': 'F12' };
  /* The escape sequence at s[i] (ESC already there): CSI (ESC [ params final) or SS3 (ESC O char); '' when none. */
  function escSeq(s, i) {
    var c = s.charAt(i + 1);
    if (c === 'O') return s.length > i + 2 ? s.slice(i, i + 3) : '';
    if (c !== '[') return '';
    for (var j = i + 2; j < s.length; j++) {
      var k = s.charCodeAt(j);
      if (k >= 0x40 && k <= 0x7e) return s.slice(i, j + 1);
      if (k < 0x20 || k > 0x3f) return '';
    }
    return '';
  }
  /* What xterm hands over (onData: typing, a key, a paste) as the frames to send, in order and merged:
     [{type: 'input', text}] for text, [{type: 'keys', keys: [...]}] for named keys, [{type: 'mouse', report}] for a click. Control characters are keys
     (Enter, Tab, Backspace, Esc, Ctrl-<letter>), never text; sequences coop cannot name (Alt chords, Shift arrows,
     bracketed-paste marks) are dropped. */
  function frames(data) {
    var out = [], s = String(data == null ? '' : data);
    function text(t) { var l = out[out.length - 1]; if (l && l.type === 'input') l.text += t; else out.push({ type: 'input', text: t }); }
    function key(k) { var l = out[out.length - 1]; if (l && l.type === 'keys') l.keys.push(k); else out.push({ type: 'keys', keys: [k] }); }
    for (var i = 0; i < s.length; i++) {
      var c = s.charCodeAt(i);
      if (c === 0x1b) {
        var q = escSeq(s, i);
        if (q) {
          if (SEQ[q]) key(SEQ[q]);
          else if (/^\x1b\[<\d+;\d+;\d+[Mm]$/.test(q) && !(q.slice(3).split(';')[0] & 32)) out.push({ type: 'mouse', report: q });
          i += q.length - 1 + (q === '\x1b[M' ? 3 : 0);   // an X10 mouse report: its three bytes are not text
          continue;
        }
        if (i + 1 < s.length && s.charCodeAt(i + 1) >= 0x20) { i++; continue; }   // an Alt chord: coop has no name for it
        key('Esc');
      } else if (c === 0x0d) key('Enter');
      else if (c === 0x09) key('Tab');
      else if (c === 0x7f) key('Backspace');
      else if (c >= 1 && c <= 26) key('Ctrl-' + String.fromCharCode(64 + c));
      else if (c < 0x20 || (c >= 0x80 && c < 0xa0)) continue;   // NUL, Ctrl-\ and the like, C1: nothing coop types
      else text(s.charAt(i));
    }
    return out;
  }
  /* What the banner says about the keyboard, {who, mine, text}: control is the stream's control event (internal/glass
     Control) or the document's, you the viewer as a claim names them. */
  function controlLine(c, you) {
    var who = (c && c.holder) || '', mine = !!who && !!you && who === you;
    if (!who) return { who: '', mine: false, text: '' };
    return { who: who, mine: mine, text: mine ? 'Attached as you (' + you + '). What you type goes to the agent; click in its prompt to move the cursor. Esc Esc lets go.'
      : 'Attached as ' + who + ': they have the keyboard. You are watching.' };
  }
  /* Why the keyboard left this viewer, from the control event that took it ('' when it did not). */
  function lostLine(c, you) {
    if (!c || !you || c.holder === you) return '';
    if (c.holder) return c.holder + ' took the keyboard. You are watching again.';
    if (c.reason === 'expired' && c.from === you) return 'Let go: the claim lapsed. You are watching again.';
    return '';
  }
  /* The touch row's keys, [label, what it does (a coop key name, ctrl or paste), accessible name]. */
  var TOUCH = [['Esc', 'Esc'], ['Tab', 'Tab'], ['Ctrl', 'ctrl', 'Ctrl, for the next letter typed'], ['↑', 'Up', 'Up arrow'], ['↓', 'Down', 'Down arrow'],
    ['←', 'Left', 'Left arrow'], ['→', 'Right', 'Right arrow'], ['Paste', 'paste', 'Paste the clipboard']];
  /* What Ctrl held makes of the next thing typed: a letter as its control character, '' for anything else. */
  function ctrlOf(data) { return /^[a-z]$/i.test(data || '') ? String.fromCharCode(data.toUpperCase().charCodeAt(0) - 64) : ''; }
  /* The columns that fit a box w px wide in cells cw px wide, 2 to 1000; 0 when it cannot say. */
  function fitCols(w, cw) { return w > 0 && cw > 0 ? Math.max(2, Math.min(1000, Math.floor(w / cw))) : 0; }

  /* Click to move the cursor. ROWS_MAX bounds how far a click may move it up or down. */
  var ROWS_MAX = 40;
  function repeat(k, n) { var out = []; for (var i = 0; i < n; i++) out.push(k); return out; }
  /* A row that bounds the block a click may move the cursor in: blank, or a box's rule or corner (Claude Code draws its
     prompt between two ─ rules; a box with ╭ ╰ corners has │ sides, and a side is inside). */
  function bound(line) { var t = String(line == null ? '' : line).trim(); return !t || /^[─━═╭╮╰╯┌┐└┘]/.test(t); }
  /* The Up/Down that move the caret from row cy to a click on row ay, lineAt(y) the text of row y (null past the screen):
     [] on the caret's row; null when a bound row lies between them or under the click, or it is too far. */
  function rowKeys(lineAt, cy, ay) {
    if (ay === cy) return [];
    var d = ay > cy ? 1 : -1, n = Math.abs(ay - cy);
    if (n > ROWS_MAX || bound(lineAt(cy))) return null;
    for (var y = cy + d; ; y += d) {
      var l = lineAt(y);
      if (l == null || bound(l)) return null;
      if (y === ay) break;
    }
    return repeat(d > 0 ? 'Down' : 'Up', n);
  }
  /* The Left/Right that move the caret from cell cx to a click on cell ax of its row; cells are the row's characters by
     cell (' ' blank, null the second half of a wide character). Counted in characters, and never past the row's text: not
     into a prompt mark ('> ', '❯ ') or a box side before it, nor the blanks or the side after it. */
  function colKeys(cells, cx, ax) {
    var end = cells.length, start = 0;
    while (end > 0 && (cells[end - 1] === ' ' || cells[end - 1] === '│')) end--;
    while (start < end && cells[start] === ' ') start++;
    if (cells[start] === '│') { start++; while (start < end && cells[start] === ' ') start++; }
    if (/^[>❯›]$/.test(cells[start] || '') && cells[start + 1] === ' ') start += 2;
    start = Math.min(start, cx);
    ax = Math.max(start, Math.min(ax, Math.max(end, cx)));
    while (ax > 0 && cells[ax] === null) ax--;   // onto the first half of a wide character
    var n = 0;
    for (var i = Math.min(cx, ax); i < Math.max(cx, ax); i++) if (cells[i] !== null) n++;
    return repeat(ax > cx ? 'Right' : 'Left', n);
  }
  /* Where an Ink app's drawn caret sits in a row, by cell: the one cell of a run of exactly one inverse cell; -1 when none. */
  function soleInverse(inv) {
    for (var i = 0; i < inv.length; i++) if (inv[i] && !inv[i - 1] && !inv[i + 1]) return i;
    return -1;
  }
  var api = { sendURL: sendURL, controlURL: controlURL, frames: frames, controlLine: controlLine, lostLine: lostLine, fitCols: fitCols, TOUCH: TOUCH, ctrlOf: ctrlOf,
    bound: bound, rowKeys: rowKeys, colKeys: colKeys, soleInverse: soleInverse, ESC_GAP: ESC_GAP, BLUR_MS: BLUR_MS, RENEW_MS: RENEW_MS };
  if (typeof document === 'undefined') return api;

  /* ---------- DOM ---------- */
  var B = document.body, base = B.dataset.base || '';
  if (B.dataset.glass !== '1') return api;
  /* The Glass attached from: {id, doc, slot, box, term, banner, bar, note, ask, held, holder, q, buf, bufT, escT, awayT, renewT, sub, down (where a press began), move (the click being moved to)} */
  var cur = null;

  function el(t, cls, text) { var e = document.createElement(t); if (cls) e.className = cls; if (text !== undefined) e.textContent = text; return e; }
  function btn(parent, cls, text, label) { var b = parent.appendChild(el('button', 'btn small ' + cls, text)); b.type = 'button'; if (label) b.setAttribute('aria-label', label); return b; }
  function say(c, text, err) { c.note.textContent = text || ''; c.note.className = 'bv-note gl-sent' + (text ? (err ? ' err' : ' ok') : ''); }
  function tell(name, detail) { try { document.dispatchEvent(new CustomEvent(name, { detail: detail })); } catch (e) { /* no CustomEvent */ } }

  /* POST fields to url; resolves to {status, body} (body the JSON answer, or the text of a plain one). */
  function post(url, fields, keepalive) {
    var body = new URLSearchParams();
    fields.forEach(function (kv) { body.append(kv[0], kv[1]); });
    return fetch(base + url, { method: 'POST', credentials: 'same-origin', cache: 'no-store', keepalive: !!keepalive,
      headers: { Accept: 'application/json', 'Content-Type': 'application/x-www-form-urlencoded' }, body: body })
      .then(function (r) {
        var json = (r.headers.get('Content-Type') || '').indexOf('application/json') === 0;
        return (json ? r.json() : r.text()).then(function (b) { return { status: r.status, body: b }; }, function () { return { status: r.status, body: null }; });
      }, function () { return { status: 0, body: null }; });
  }
  function noteOf(res, fallback) {
    var b = res.body;
    if (b && typeof b === 'object' && b.note) return String(b.note);
    if (typeof b === 'string' && b.trim()) return b.trim();
    return res.status ? fallback + ' (' + res.status + ').' : 'The garden could not be reached. Try again.';
  }

  /* ---------- the key row and the banner under the terminal ---------- */
  /* The terminal may be clicked into: attach is offered, live, and nobody else holds the keyboard. */
  function attachable(c) { return !!(c.doc.can_attach && c.doc.live && !c.held && !(c.holder && c.holder !== c.doc.you)); }
  function paint(c) {
    var line = controlLine({ holder: c.held ? c.doc.you : c.holder }, c.doc.you);
    c.banner.textContent = line.text;
    c.banner.hidden = !line.text;
    c.banner.className = 'gl-attached' + (line.mine ? ' mine' : '');
    c.bar.textContent = '';
    c.bar.removeAttribute('role');
    c.dock.classList.toggle('held', c.held);
    if (c.box) {
      c.box.classList.toggle('can-type', attachable(c));
      c.box.classList.toggle('typing', c.held);
    }
    if (!c.doc.can_attach || !c.doc.live) return;
    if (!c.held) {
      if (c.holder && c.holder !== c.doc.you) {
        var a = btn(c.bar, 'ghost gl-take', 'Take the keyboard from ' + c.holder, 'Take the keyboard of ' + (c.doc.agent || c.id) + ' from ' + c.holder);
        a.onclick = function () { claim(c, true); };
      } else c.bar.appendChild(el('span', 'gl-keys-hint', 'Click or tap the terminal to type into it (or Enter on it): what you type goes to the agent, and other watchers see it is you.'));
      return;
    }
    c.bar.setAttribute('role', 'toolbar');
    c.bar.setAttribute('aria-label', 'Attached to ' + (c.doc.agent || c.id));
    TOUCH.forEach(function (k) {   // shown on a touch screen only (garden.css .gl-k)
      var b = btn(c.bar, 'ghost gl-k', k[0], k[2]);
      b.addEventListener('mousedown', function (e) { e.preventDefault(); });   // the terminal keeps the focus, and a phone its keyboard
      if (k[1] === 'ctrl') b.setAttribute('aria-pressed', String(!!c.ctrl));
      b.onclick = function () { touchKey(c, k[1]); refocus(c); };
    });
    var fit = btn(c.bar, 'ghost gl-fitw', 'Fit width', 'Fit width to my screen');
    fit.onclick = function () { resize(c); };
    var compact = btn(c.bar, 'ghost danger gl-compact', 'Compact…', 'Compact the agent’s context, asks first');
    compact.onclick = function () { send(c, [['type', 'compact']]); };
    var off = btn(c.bar, 'gl-letgo', 'Let go');
    off.onclick = function () { release(c, 'Let go: you are watching again.'); };
  }
  function refocus(c) { if (c.held && c.term) try { c.term.focus(); } catch (e) { /* gone */ } }

  /* The terminal takes typing only while the keyboard is held. */
  function arm(c, on) {
    var t = c.term;
    if (t) {
      try { t.options.disableStdin = !on; t.options.cursorBlink = on; } catch (e) { /* an xterm without options */ }
      if (t.textarea) { t.textarea.readOnly = !on; t.textarea.tabIndex = on ? 0 : -1; }
      if (on && !c.sub && t.onData) c.sub = t.onData(function (d) { typed(c, d); });
      if (!on && c.sub) { try { c.sub.dispose(); } catch (e) { /* gone */ } c.sub = null; }
    }
  }
  function setHeld(c, on, line, err) {
    if (c.held !== on) {
      c.held = on;
      if (on) c.holder = c.doc.you;
      else { clearTimeout(c.escT); clearTimeout(c.awayT); clearTimeout(c.bufT); c.buf = ''; c.ctrl = false; c.escT = c.awayT = c.bufT = null; c.ask.textContent = ''; c.ask.hidden = true; c.dock.classList.remove('asking'); }
      clearInterval(c.renewT);
      c.renewT = on ? setInterval(function () { renew(c); }, RENEW_MS) : null;
      arm(c, on);
      tell('glass:attached', { id: c.id, on: on });
    }
    paint(c);
    if (line !== undefined) say(c, line, err);
    if (on) refocus(c);
    else if (c.box && c.box.contains(document.activeElement) && document.activeElement !== c.box) c.box.focus();   // out of xterm, onto the terminal
  }

  /* ---------- the claim ---------- */
  function claim(c, take) {
    say(c, take ? 'Taking the keyboard…' : 'Attaching…');
    post(controlURL(c.id), take ? [['action', 'claim'], ['take', '1']] : [['action', 'claim']]).then(function (res) {
      if (cur !== c) return;
      var b = res.body && typeof res.body === 'object' ? res.body : null;
      if (b && b.ok) { setHeld(c, true, b.result === 'taken' ? b.note : ''); return; }   // the banner says the rest
      if (b && b.control) c.holder = b.control.holder || '';
      arm(c, false);   // take() opened the textarea for the gesture
      setHeld(c, false, noteOf(res, 'The keyboard could not be taken'), true);
    });
  }
  function renew(c) {
    post(controlURL(c.id), [['action', 'renew']]).then(function (res) {
      if (cur !== c || !c.held || res.status !== 409) return;
      var b = res.body && typeof res.body === 'object' ? res.body : null;
      c.holder = (b && b.control && b.control.holder) || '';
      setHeld(c, false, noteOf(res, 'The keyboard was let go'), true);
    });
  }
  /* Let go: told to the server (kept alive past an unload), the panel back to watching. */
  function release(c, line) {
    if (!c.held) return;
    flush(c);
    c.holder = '';
    setHeld(c, false, line);
    post(controlURL(c.id), [['action', 'release']], true);
  }

  /* ---------- typing ---------- */
  /* Sends run one after another, in the order typed. */
  function send(c, fields) {
    c.q = c.q.then(function () {
      if (cur !== c) return;
      return post(sendURL(c.id), fields).then(function (res) { answered(c, fields, res); });
    });
  }
  function answered(c, fields, res) {
    if (cur !== c) return;
    var b = res.body && typeof res.body === 'object' ? res.body : null;
    if (res.status >= 200 && res.status < 300 && (!b || b.ok !== false)) {
      if (b && b.result === 'canceled') say(c, 'Not sent.');
      return;
    }
    if (b && b.result === 'confirm') { confirm(c, fields, b.note); return; }
    if (b && b.result === 'not_attached') { c.holder = ''; setHeld(c, false, noteOf(res, 'Not sent'), true); return; }
    say(c, noteOf(res, 'Not sent: the garden did not answer'), true);
  }
  /* Ask before a send the server holds (Esc, Ctrl-C and the like, Compact): Send goes with confirmed=1, Cancel with cancel=1.
     Cancel comes first and has the focus; Send is apart from it, in the danger style. Esc in the question is Cancel (and
     stays in it: the bead view does not close), so a second Esc after a lone one sends nothing. */
  function confirm(c, fields, note) {
    c.ask.textContent = '';
    c.ask.hidden = false;
    c.dock.classList.add('asking');
    c.ask.appendChild(el('p', 'gl-ask-q', note || 'Send it?'));
    var row = c.ask.appendChild(el('div', 'gl-ask-row'));
    var compact = fields[0][1] === 'compact';
    var no = btn(row, 'ghost gl-ask-no', 'Cancel');
    var yes = btn(row, 'danger gl-ask-yes', compact ? 'Compact' : 'Send ' + ((fields[1] && fields[1][1]) || ''));
    function done() { c.ask.textContent = ''; c.ask.hidden = true; c.ask.onkeydown = null; c.dock.classList.remove('asking'); refocus(c); }
    yes.onclick = function () { done(); send(c, fields.concat([['confirmed', '1']])); };
    no.onclick = function () { done(); send(c, fields.concat([['cancel', '1']])); };
    c.ask.onkeydown = function (e) { if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); no.onclick(); } };
    no.focus();
  }
  /* Typed text waits TEXT_GAP ms for more, so a word is one send, not one a letter. */
  function flush(c) {
    clearTimeout(c.bufT); c.bufT = null;
    if (!c.buf) return;
    var t = c.buf; c.buf = '';
    send(c, [['type', 'input'], ['text', t]]);
  }
  function sendFrames(c, data) {
    frames(data).forEach(function (f) {
      if (f.type === 'input') { c.buf += f.text; clearTimeout(c.bufT); c.bufT = setTimeout(function () { flush(c); }, TEXT_GAP); }
      else { flush(c); send(c, f.type === 'mouse' ? [['type', 'mouse'], ['report', f.report]] : [['type', 'keys'], ['keys', f.keys.join(',')]]); }
    });
  }
  /* A key of the touch row: Ctrl toggles for the next letter, Paste types the clipboard, any other goes as its key at once
     (Esc too: Esc Esc letting go is the keyboard's). */
  function touchKey(c, k) {
    if (!c.held) return;
    if (k === 'ctrl') { latch(c, !c.ctrl); return; }
    latch(c, false);
    if (k === 'paste') { paste(c); return; }
    flush(c);
    send(c, [['type', 'keys'], ['keys', k]]);
  }
  function latch(c, on) { c.ctrl = on; var b = c.bar.querySelector('.gl-k[aria-pressed]'); if (b) b.setAttribute('aria-pressed', String(on)); }
  /* The clipboard typed as xterm types a paste: its line breaks as Enter. */
  function paste(c) {
    var cb = navigator.clipboard;
    if (!cb || !cb.readText) { say(c, 'This browser does not let the page read the clipboard.', true); return; }
    cb.readText().then(function (t) { if (cur === c && c.held && t) sendFrames(c, String(t).replace(/\r\n?|\n/g, '\r')); },
      function () { if (cur === c) say(c, 'Not pasted: the browser did not hand over the clipboard.', true); });
  }
  /* What xterm hands over while attached. A lone Esc waits ESC_GAP for a second: Esc Esc lets go, sending neither. */
  function typed(c, data) {
    if (!c.held) return;
    if (c.ctrl) { latch(c, false); data = ctrlOf(data) || data; }
    if (data === '\x1b') {
      if (c.escT) { clearTimeout(c.escT); c.escT = null; release(c, 'Let go (Esc Esc): you are watching again.'); return; }
      c.escT = setTimeout(function () { c.escT = null; sendFrames(c, '\x1b'); }, ESC_GAP);
      return;
    }
    if (c.escT) { clearTimeout(c.escT); c.escT = null; sendFrames(c, '\x1b'); }
    sendFrames(c, data);
  }
  /* Fit the PTY's width to the panel: every watcher's terminal follows (the relay resyncs them at the new size). */
  function resize(c) {
    if (!c.term) { say(c, 'The terminal has not loaded, so there is nothing to fit.', true); return; }
    var cell = el('span', 'gl-cell', 'WWWWWWWWWW'), host = c.box, px = (c.term.options && c.term.options.fontSize) || 13;   // a pinch sets it (glass.js)
    cell.style.cssText = 'position:absolute;visibility:hidden;white-space:pre;font:400 ' + px + 'px/1 ' + (getComputedStyle(document.documentElement).getPropertyValue('--font-display').trim() || 'monospace');
    host.appendChild(cell);
    var cols = fitCols(host.clientWidth - 16, cell.getBoundingClientRect().width / 10);
    cell.remove();
    if (!cols) return;
    send(c, [['type', 'resize'], ['cols', String(cols)], ['rows', String(c.term.rows || 24)]]);
    say(c, 'Fitted to ' + cols + ' columns: everyone watching sees it rewrap.');
  }

  /* ---------- the terminal itself: click or tap into it, click to move the cursor ---------- */
  /* Attach from the terminal. Its textarea takes focus now, inside the click or tap, so a phone opens its keyboard; the
     claim then wires the typing (arm), or a refusal closes the textarea again. */
  function take(c) {
    var t = c.term;
    if (t && t.textarea) { t.textarea.readOnly = false; t.textarea.tabIndex = 0; try { t.focus(); } catch (e) { /* gone */ } }
    claim(c, false);
  }
  /* The screen cell under a click, {x, y} with y a buffer row: the terminal is scaled to fit, so it is measured as drawn. */
  function cellAt(c, e) {
    var t = c.term, s = t && t.element && t.element.querySelector('.xterm-screen');
    if (!s || !t.buffer) return null;
    var r = s.getBoundingClientRect();
    if (!r.width || !r.height) return null;
    var x = Math.floor((e.clientX - r.left) * t.cols / r.width), y = Math.floor((e.clientY - r.top) * t.rows / r.height);
    if (x < 0 || y < 0 || x >= t.cols || y >= t.rows) return null;
    return { x: x, y: t.buffer.active.viewportY + y };
  }
  function lineAt(b, y) { var l = b.getLine(y); return l ? l.translateToString(true) : null; }
  function cellsOf(b, y) {
    var l = b.getLine(y), out = [];
    if (!l) return out;
    for (var x = 0; x < l.length; x++) { var k = l.getCell(x); out.push(!k ? ' ' : k.getWidth() === 0 ? null : (k.getChars() || ' ')); }
    return out;
  }
  /* The caret: the single inverse cell an Ink app draws (it hides the terminal's cursor), the lowest on the screen; else
     the terminal's own cursor. {x, y}, y a buffer row. */
  function caret(t) {
    var b = t.buffer.active;
    for (var y = b.baseY + t.rows - 1; y >= b.baseY; y--) {
      var l = b.getLine(y), inv = [];
      if (!l) continue;
      for (var x = 0; x < l.length; x++) { var k = l.getCell(x); inv.push(!!(k && k.isInverse())); }
      var at = soleInverse(inv);
      if (at >= 0) return { x: at, y: y };
    }
    return { x: b.cursorX, y: b.baseY + b.cursorY };
  }
  /* Named keys in sends of at most 32 (the server's cap). */
  function sendKeys(c, keys) {
    for (var i = 0; i < keys.length; i += 32) send(c, [['type', 'keys'], ['keys', keys.slice(i, i + 32).join(',')]]);
  }
  /* Once the sends so far are answered and the screen has settled (no write for SETTLE ms, at most MOVE_WAIT), run fn. */
  var SETTLE = 80, MOVE_WAIT = 1500;
  function settled(c, fn) {
    var t = c.term, my = c.move;
    c.q.then(function () {
      if (cur !== c || c.move !== my) return;
      var quiet = null, sub = null, give = setTimeout(go, MOVE_WAIT);
      function go() { clearTimeout(quiet); clearTimeout(give); if (sub) { try { sub.dispose(); } catch (e) { /* gone */ } sub = null; } if (cur === c && c.move === my && c.held) fn(); }
      function wrote() { clearTimeout(quiet); quiet = setTimeout(go, SETTLE); }
      var on = t.onWriteParsed || t.onRender;
      if (on) sub = on.call(t, wrote);
      wrote();
    });
  }
  /* Move the caret to the cell clicked: down or up to its row, then along it, each from where the caret is by then (the app
     decides where Up and Down land, so the row is checked again, at most three times). */
  function moveTo(c, at, tries) {
    var t = c.term;
    if (!at || !t || !c.held) return;
    var b = t.buffer.active, k = caret(t), v = rowKeys(function (y) { return lineAt(b, y); }, k.y, at.y);
    if (!v) return;
    flush(c);
    if (v.length) {
      if ((tries || 0) >= 3) return;
      sendKeys(c, v);
      settled(c, function () { moveTo(c, at, (tries || 0) + 1); });
      return;
    }
    sendKeys(c, colKeys(cellsOf(b, k.y), k.x, at.x));
  }
  function clicked(c, e) {
    var d = c.down;
    c.down = null;
    if (cur !== c || (d && Math.abs(e.clientX - d.x) + Math.abs(e.clientY - d.y) > 6)) return;   // a drag: a selection, a pan
    if (!c.held) { if (attachable(c) && c.term) take(c); return; }   // no xterm, nothing to type into
    if (c.term && ((c.term.hasSelection && c.term.hasSelection()) || (c.term.modes && c.term.modes.mouseTrackingMode !== 'none'))) return;   // the app takes the click
    c.move = {};   // a newer click takes over from one still moving
    moveTo(c, cellAt(c, e), 0);
  }
  function wire(c) {
    var box = c.box;
    if (!box) return;
    box.addEventListener('pointerdown', function (e) { c.down = { x: e.clientX, y: e.clientY }; });
    box.addEventListener('click', function (e) { clicked(c, e); });
    box.addEventListener('keydown', function (e) {
      if (e.target === box && e.key === 'Enter' && cur === c && attachable(c)) { e.preventDefault(); take(c); }
    });
  }

  /* ---------- letting go when away ---------- */
  function away(c) { return document.hidden || (document.hasFocus && !document.hasFocus()) || !(c.panel && c.panel.contains(document.activeElement)); }
  function watchAway() {
    var c = cur;
    if (!c || !c.held) return;
    if (!away(c)) { clearTimeout(c.awayT); c.awayT = null; return; }
    if (!c.awayT) c.awayT = setTimeout(function () { c.awayT = null; if (cur === c && away(c)) release(c, 'Let go: you were away for a minute. You are watching again.'); }, BLUR_MS);
  }
  ['visibilitychange', 'focusin', 'focusout'].forEach(function (n) { document.addEventListener(n, function () { setTimeout(watchAway, 0); }); });
  window.addEventListener('blur', watchAway);
  window.addEventListener('focus', watchAway);
  window.addEventListener('pagehide', function () { if (cur && cur.held) release(cur, ''); });

  document.addEventListener('glass:open', function (e) {
    var d = e.detail || {}, doc = d.doc || {};
    if (cur && cur.held) release(cur, '');
    cur = null;
    if (!d.keys || !d.id) return;
    var c = { id: d.id, doc: doc, slot: d.keys, box: d.box, panel: d.keys.parentNode, term: null, held: false, holder: (doc.control && doc.control.holder) || '',
      q: Promise.resolve(), buf: '', bufT: null, ctrl: false, escT: null, awayT: null, renewT: null, sub: null, down: null, move: null };
    d.keys.textContent = '';
    c.dock = d.keys.appendChild(el('div', 'gl-dock'));   // the key row, or the question in its place
    c.bar = c.dock.appendChild(el('div', 'gl-keybar'));
    c.bar.setAttribute('data-scroll-x', '');
    c.ask = c.dock.appendChild(el('div', 'gl-ask'));
    c.ask.setAttribute('role', 'alertdialog'); c.ask.setAttribute('aria-label', 'Confirm');
    c.ask.hidden = true;
    c.banner = d.keys.appendChild(el('p', 'gl-attached'));
    c.banner.setAttribute('role', 'status');
    c.note = d.keys.appendChild(el('p', 'bv-note gl-sent'));
    c.note.setAttribute('role', 'status');
    cur = c;
    wire(c);
    paint(c);
  });
  document.addEventListener('glass:term', function (e) { var d = e.detail || {}; if (cur && d.id === cur.id) { cur.term = d.term; arm(cur, cur.held); } });
  document.addEventListener('glass:control', function (e) {
    var d = e.detail || {}, c = cur;
    if (!c || d.id !== c.id) return;
    var ctl = d.control || {};
    c.holder = ctl.holder || '';
    if (c.held && c.holder !== c.doc.you) setHeld(c, false, lostLine(ctl, c.doc.you) || 'The keyboard was let go. You are watching again.', true);
    else paint(c);
  });
  document.addEventListener('glass:close', function () { if (cur && cur.held) release(cur, ''); cur = null; });
  return api;
})();
if (typeof module !== 'undefined') module.exports = GLASSKEY;
