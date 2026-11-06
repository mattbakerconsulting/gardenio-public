// agent_terminal.js — Phase 1+4 PTY pane glue.
//
// Phase 1 wired xterm.js (loaded as xterm.bundle.js) to a coopmux WebSocket so
// the agent-detail page streams live PTY output. On WS drop, falls back to
// fetching the cached /screen snapshot via htmx and reopens the WS with
// exponential backoff.
//
// Phase 4 adds:
//   - Page Visibility API: hold WS open when the tab is hidden; on resume
//     after >30 s force a hard reconnect so a long-suspended socket doesn't
//     keep ferrying stale frames.
//   - aria-live mirror: forward each line written to xterm into a sibling
//     polite live region so screen readers announce scrollback as it lands.
//   - Font-size selector: read `agent_term_fontsize` cookie, apply on init.
//   - Per-transport health dot: keep `#term-health-pty` in sync with the WS
//     state (live | reconnecting | error | offline).
//   - prefers-reduced-motion: disable cursor blink when the OS asks.
//   - Coarse-pointer aware: lower scrollback ceiling on mobile.
//
// Anti-scope: no input, no key handling, no resize-on-input. The terminal
// is a read-only viewer — keystrokes are ignored client-side. (Operator
// writes go through the dedicated keys/nudge/respond htmx surfaces.)
(function () {
  'use strict';

  if (typeof window === 'undefined' || typeof window.Terminal === 'undefined') {
    return; // xterm.bundle.js failed to load — fallback <pre> stays visible.
  }

  var card = document.querySelector('.agent-card[data-bead-id]');
  if (!card) return;

  var beadID = card.getAttribute('data-bead-id');
  var sessionID = (card.getAttribute('data-session-id') || '').trim();
  if (!beadID || !sessionID) return; // No live session to attach to.

  var fallback = document.getElementById('term-fallback');
  var termEl = document.getElementById('term');
  var statusEl = document.getElementById('term-status');
  var ariaLive = document.getElementById('term-aria-live');
  var healthDot = document.getElementById('term-health-pty');
  var catchUpPill = document.getElementById('term-catching-up');
  if (!termEl) return;

  var basePath = (window.BEADS_VIEWER_BASE_PATH || '').replace(/\/+$/, '');
  var coarsePointer = card.getAttribute('data-coarse-pointer') === '1';

  // wsURL derives from window.location: replace the first DNS label with
  // "coopmux" so beads.<host> → coopmux.<host>. Matches the plan-of-record
  // convention; if a deployment uses a different host scheme it must be
  // adjusted here. Read-only fallback (the <pre>) keeps the page useful even
  // when this URL fails to connect.
  function wsURL() {
    var host = window.location.host;
    var labels = host.split('.');
    if (labels.length > 1) {
      labels[0] = 'coopmux';
    } else {
      labels[0] = 'coopmux.' + labels[0];
    }
    var scheme = window.location.protocol === 'https:' ? 'wss' : 'ws';
    return scheme + '://' + labels.join('.') + '/ws/' + encodeURIComponent(sessionID);
  }

  // Honor prefers-reduced-motion: block xterm's cursor blink when the OS
  // requests reduced motion. Picked up at init; live media-query changes
  // are rare enough that a reload is acceptable.
  var reducedMotion = false;
  try {
    reducedMotion = window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  } catch (e) { /* legacy browsers without matchMedia — leave default off */ }

  // Font-size cookie → xterm fontSize. Three steps that map to the existing
  // CSS classes; xterm picks them up via the option, the surrounding
  // container picks them via the class so the fallback <pre> sizes too.
  var fontSize = readCookie('agent_term_fontsize') || 'medium';
  var fontSizePx = ({ small: 11, medium: 13, large: 16 })[fontSize] || 13;
  applyFontSizeClass(fontSize);

  // Mobile: smaller scrollback so a phone doesn't try to retain 5k rows
  // of PTY output it can't usefully scroll through.
  var scrollback = coarsePointer ? 1000 : 5000;

  var term = new window.Terminal({
    convertEol: true,
    cursorBlink: !reducedMotion,
    disableStdin: true, // Phase 1: read-only.
    scrollback: scrollback,
    fontSize: fontSizePx,
    screenReaderMode: true,
    theme: { background: '#0d1117' },
  });
  term.open(termEl);

  // aria-live mirror. xterm.screenReaderMode emits offscreen text into its
  // own ARIA region, but it is buried inside the xterm DOM and doesn't
  // always survive theme overrides. We additionally copy each line as it
  // is written into a sibling polite region — bounded to the last 40 lines
  // so the announcer doesn't replay history on every reflow.
  var ariaBuffer = [];
  var ariaMax = 40;
  if (ariaLive) {
    try {
      term.onLineFeed(function () {
        var buf = term.buffer && term.buffer.active;
        if (!buf) return;
        var y = buf.cursorY + buf.viewportY - 1;
        if (y < 0) return;
        var line = buf.getLine(y);
        if (!line) return;
        var text = line.translateToString(true);
        if (!text) return;
        ariaBuffer.push(text);
        if (ariaBuffer.length > ariaMax) ariaBuffer.shift();
        ariaLive.textContent = ariaBuffer.join('\n');
      });
    } catch (e) { /* xterm API drift — leave the sibling region empty */ }
  }

  function setStatus(msg) {
    if (statusEl) statusEl.textContent = msg;
  }

  function setHealth(state) {
    if (!healthDot) return;
    healthDot.setAttribute('data-state', state);
    healthDot.setAttribute('title', 'PTY ' + state);
    healthDot.setAttribute('aria-label', 'PTY transport ' + state);
  }

  function showLive() {
    if (fallback && !fallback.hidden) fallback.hidden = true;
    if (termEl.hidden) termEl.hidden = false;
  }

  function showCatchUp(visible) {
    if (!catchUpPill) return;
    catchUpPill.hidden = !visible;
  }

  function refreshFallback() {
    if (!fallback) return;
    var url = basePath + '/agents/' + encodeURIComponent(beadID) + '/screen';
    fetch(url, { credentials: 'same-origin' })
      .then(function (r) { return r.ok ? r.text() : ''; })
      .then(function (html) {
        if (!html) return;
        // Server-rendered fragment is `<pre id="term-fallback">…</pre>`.
        // Replace the existing element so the new snapshot is visible.
        var tmp = document.createElement('div');
        tmp.innerHTML = html;
        var fresh = tmp.querySelector('#term-fallback');
        if (fresh && fallback.parentNode) {
          fallback.parentNode.replaceChild(fresh, fallback);
          fallback = fresh;
        }
        fallback.hidden = false;
        if (termEl) termEl.hidden = true;
      })
      .catch(function () { /* benign — keep stale fallback visible */ });
  }

  var ws = null;
  var firstFrame = true;
  var backoff = 500;
  var maxBackoff = 15000;
  var closed = false;
  var hiddenAt = 0;
  var hardReconnectThresholdMs = 30000;

  // probeAuth distinguishes auth-expired (cross-origin 303 from a session gate
  // like AWS Verified Access) from a genuinely flaky upstream. A WS upgrade
  // that dies at the edge surfaces only as a generic onclose — the browser
  // hides the HTTP status — so without this probe the client reconnect-storms
  // against the OAuth redirect every backoff tick for as long as the tab is
  // open. Probing the same-origin status endpoint with redirect:'manual' is
  // the only way to see the real verdict; the edge 303s everything in lockstep
  // during a re-auth window, so the same-origin state mirrors the WS host's.
  // Mirrors agent_events.js. (ka-0000005477)
  function probeAuth() {
    if (typeof window.fetch !== 'function') return Promise.resolve(null);
    var ctrl = (typeof window.AbortController === 'function') ? new AbortController() : null;
    var t = setTimeout(function () { if (ctrl) try { ctrl.abort(); } catch (e) {} }, 5000);
    return window.fetch(basePath + '/agents/events-status', {
      method: 'GET',
      credentials: 'include',
      cache: 'no-store',
      redirect: 'manual',
      headers: { 'Accept': 'application/json' },
      signal: ctrl ? ctrl.signal : undefined
    }).then(function (resp) {
      clearTimeout(t);
      if (resp.type === 'opaqueredirect' || resp.status === 0) return 'auth-expired';
      if (resp.status === 401 || resp.status === 403) return 'auth-expired';
      if (resp.status >= 200 && resp.status < 300) return 'ok';
      return null;
    }).catch(function () {
      clearTimeout(t);
      return null;
    });
  }

  function connect() {
    if (closed) return;
    setStatus('connecting…');
    setHealth('connecting');
    showCatchUp(true);
    try {
      ws = new WebSocket(wsURL());
    } catch (e) {
      setStatus('connect failed');
      setHealth('error');
      scheduleReconnect();
      return;
    }

    ws.binaryType = 'arraybuffer';

    ws.onopen = function () {
      backoff = 500;
      setStatus('live');
      setHealth('live');
    };

    ws.onmessage = function (ev) {
      if (firstFrame) {
        firstFrame = false;
        showLive();
        showCatchUp(false);
      }
      if (typeof ev.data === 'string') {
        term.write(ev.data);
      } else if (ev.data instanceof ArrayBuffer) {
        term.write(new Uint8Array(ev.data));
      }
    };

    ws.onerror = function () {
      setStatus('error');
      setHealth('error');
    };

    ws.onclose = function () {
      ws = null;
      if (closed) return;
      setStatus('disconnected');
      setHealth('offline');
      refreshFallback();
      probeAuth().then(function (verdict) {
        if (closed) return;
        if (verdict === 'auth-expired') {
          // Stop the reconnect storm: an expired edge session 303s the WS
          // upgrade to the OAuth flow, which can never succeed from JS. Park
          // the client and surface the state; the shared policy defers the
          // actual re-auth to the user's next real action (ka-0000004168),
          // with the fallback <pre> snapshot keeping the page useful.
          closed = true;
          setStatus('session expired — reload to re-authenticate');
          setHealth('auth-expired');
          if (window.bvReauthOnEdgeExpiry) { window.bvReauthOnEdgeExpiry(); }
          return;
        }
        scheduleReconnect();
      });
    };
  }

  function scheduleReconnect() {
    if (closed) return;
    var delay = backoff;
    backoff = Math.min(backoff * 2, maxBackoff);
    setTimeout(connect, delay);
  }

  // Page Visibility: don't kill an open WS when the tab hides — the agent
  // could keep streaming and we want to catch up cheaply on resume. But
  // if the tab was hidden for more than 30 s, do a hard reconnect so we
  // get a fresh stream rather than whatever stale state the proxy may be
  // holding.
  function onVisibilityChange() {
    if (document.visibilityState === 'hidden') {
      hiddenAt = Date.now();
      return;
    }
    if (document.visibilityState === 'visible' && hiddenAt > 0) {
      var awayMs = Date.now() - hiddenAt;
      hiddenAt = 0;
      if (awayMs > hardReconnectThresholdMs && ws) {
        try { ws.close(); } catch (e) { /* ignore */ }
      }
    }
  }
  document.addEventListener('visibilitychange', onVisibilityChange);

  // Font-size selector: small/medium/large radio. Persists to a long-lived
  // cookie so the choice survives reloads.
  var fontSel = document.getElementById('term-fontsize');
  if (fontSel) {
    fontSel.value = fontSize;
    fontSel.addEventListener('change', function () {
      var v = fontSel.value;
      if (!({ small: true, medium: true, large: true })[v]) return;
      writeCookie('agent_term_fontsize', v);
      applyFontSizeClass(v);
      var px = ({ small: 11, medium: 13, large: 16 })[v];
      try { term.options.fontSize = px; } catch (e) { /* xterm API drift */ }
    });
  }

  // Events SSE health dot: agent_events.js owns the EventSource lifecycle
  // but doesn't know about the dot. Mirror the EventSource readyState into
  // `#term-health-events` whenever we hear from the lane (custom event
  // dispatched by agent_events.js). Falls back to "unknown" when the helper
  // isn't loaded.
  document.addEventListener('agent-events:health', function (ev) {
    var dot = document.getElementById('term-health-events');
    if (!dot) return;
    var state = (ev && ev.detail && ev.detail.state) || 'unknown';
    dot.setAttribute('data-state', state);
    dot.setAttribute('title', 'events ' + state);
    dot.setAttribute('aria-label', 'Bead events transport ' + state);
  });

  window.addEventListener('beforeunload', function () {
    closed = true;
    document.removeEventListener('visibilitychange', onVisibilityChange);
    if (ws) try { ws.close(); } catch (e) { /* ignore */ }
  });

  function readCookie(name) {
    var pairs = (document.cookie || '').split(';');
    for (var i = 0; i < pairs.length; i++) {
      var p = pairs[i].trim();
      var eq = p.indexOf('=');
      if (eq < 0) continue;
      if (p.slice(0, eq) === name) return decodeURIComponent(p.slice(eq + 1));
    }
    return '';
  }
  function writeCookie(name, value) {
    var oneYear = 365 * 24 * 60 * 60;
    var enc = encodeURIComponent(value);
    document.cookie = name + '=' + enc + '; path=/; max-age=' + oneYear + '; SameSite=Lax';
  }
  function applyFontSizeClass(size) {
    if (!card) return;
    card.classList.remove('agent-font-small', 'agent-font-medium', 'agent-font-large');
    card.classList.add('agent-font-' + size);
  }

  // Coarse-pointer feature detect — if the OS reports `(pointer: coarse)`
  // but the request didn't carry the override, append `?coarse=1` so the
  // next navigation lets the server send mobile defaults too.
  try {
    if (window.matchMedia('(pointer: coarse)').matches && !coarsePointer) {
      var mobileMarker = document.getElementById('term-coarse-marker');
      if (mobileMarker) mobileMarker.hidden = false;
    }
  } catch (e) { /* legacy browsers */ }

  connect();
})();
