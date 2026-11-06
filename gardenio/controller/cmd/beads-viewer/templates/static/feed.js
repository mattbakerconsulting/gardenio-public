// feed.js — prototype activity-feed front page (task ka-0000003020, epic
// ka-0000003263). Behind /feed; does NOT touch the / landing page.
//
// Subscribes to /feed/events (SSE) and prepends each pre-rendered HTML card
// (produced by renderFeedCard in feed.go) into the Concept-A stream. Concept B
// (lanes) is a pure client-side re-bucketing of the SAME cards — toggling the
// view never re-subscribes. Mirrors the EventSource lifecycle (backoff + auth
// probe) of telepathy.js / agent_events.js.
//
// Controls: view toggle (stream|lanes), per-type mute chips (kill firehose
// noise), pause/buffer, auto-scroll, and a project text filter.
(function () {
  'use strict';

  if (typeof window === 'undefined' || typeof window.EventSource === 'undefined') {
    return; // EventSource missing — leave first-paint backfill visible.
  }

  var page = document.querySelector('.feed-page');
  if (!page) return;
  var stream = document.getElementById('feed-stream');
  if (!stream) return;

  var url = page.getAttribute('data-events-url');
  if (!url) return;
  var statusUrl = page.getAttribute('data-status-url') || '';
  var backfillUrl = page.getAttribute('data-backfill-url') || '';

  var statusEl = page.querySelector('.feed-status');
  var statusLabel = page.querySelector('.feed-status-label');
  var autoScrollBox = page.querySelector('.feed-autoscroll');
  var projectSelect = page.querySelector('.feed-project');
  var userSelect = page.querySelector('.feed-user');
  var countEl = page.querySelector('.feed-count');
  var pauseBtn = page.querySelector('.feed-pause');
  var viewBtns = page.querySelectorAll('.feed-viewtoggle button');
  var scopeBtns = page.querySelectorAll('.feed-scopetoggle button');
  var typeBtns = page.querySelectorAll('.feed-typefilters button');
  var filteredEmpty = page.querySelector('.feed-filtered-empty');
  var widenBtn = page.querySelector('.feed-widen');
  var lanes = {};
  page.querySelectorAll('.feed-lane-body').forEach(function (el) {
    lanes[el.getAttribute('data-lane')] = el;
  });

  var maxRows = 200; // bound the DOM so a long-lived stream can't grow forever.
  var total = 0;
  var mutedTypes = {}; // type name -> true when muted

  // ---- Dedup + reconnect tracking (Phase 2, ka-0000000643). ----
  // The warm-paint ring (hub backfill), the durable first-paint seed, and the
  // reconnect ?since= replay can all surface the same event, so dedup by a
  // stable key. lastTs tracks the newest event time rendered so reconnect can
  // gap-fill exactly what was missed.
  var seenKeys = {};        // "<bead-id>|<kind>|<ts>" -> true
  var seenOrder = [];       // FIFO of keys so the dedup set stays bounded
  var seenCap = 1000;
  var lastTs = '';          // max data-ts seen (RFC3339 string; lexs-sort == time-sort)

  function cardKey(card) {
    return (card.getAttribute('data-bead-id') || '') + '|' +
      (card.getAttribute('data-kind') || '') + '|' +
      (card.getAttribute('data-ts') || '');
  }

  // markSeen records a card key; returns false if it was already seen (a dup).
  function markSeen(key) {
    if (seenKeys[key]) return false;
    seenKeys[key] = true;
    seenOrder.push(key);
    if (seenOrder.length > seenCap) {
      var old = seenOrder.shift();
      delete seenKeys[old];
    }
    return true;
  }

  // noteTs advances lastTs if the card carries a newer timestamp.
  function noteTs(card) {
    var ts = card.getAttribute('data-ts') || '';
    if (ts && ts > lastTs) lastTs = ts;
  }

  // ---- Default per-viewer scoping (ka-0000003403). ----
  // scope: "mine" (only events the viewer operated) | "everyone".
  // project: exact project name | "" (all). Both seed from server-rendered
  // attributes so the first paint already reflects the viewer's defaults.
  var hasViewer = page.getAttribute('data-has-viewer') === '1';
  var viewerEmail = (page.getAttribute('data-viewer-email') || '').toLowerCase();
  var scope = page.getAttribute('data-scope') || (hasViewer ? 'mine' : 'everyone');
  if (scope === 'mine' && !hasViewer) scope = 'everyone';
  var projectFilter = (page.getAttribute('data-project') || '');
  // userFilter: a specific operator key (lower-cased email or raw operator id)
  // selected from the dropdown, "" for all users. A specific user is a more
  // precise operator filter than the binary mine/everyone scope, so when it is
  // set it overrides the scope's operator check (see cardVisible).
  var userFilter = (page.getAttribute('data-user') || '').toLowerCase();

  var paused = false;
  var pendingRows = []; // buffered HTML strings, newest-first, while paused

  var STATUS_LABELS = {
    connecting: 'connecting…',
    live: 'live',
    offline: 'reconnecting…',
    error: 'error',
    'auth-expired': 'session expired — reload',
  };

  function setStatus(state) {
    if (statusEl) statusEl.setAttribute('data-state', state);
    if (statusLabel) statusLabel.textContent = STATUS_LABELS[state] || state;
  }

  function updateCount() {
    if (!countEl) return;
    var base = total ? total + (total === 1 ? ' event' : ' events') : '';
    if (paused && pendingRows.length) {
      base += (base ? ' · ' : '') + pendingRows.length + ' buffered';
    }
    countEl.textContent = base;
  }

  // cardMine reports whether a card's operator identity matches the viewer.
  // Prefers the resolved operator email (strongest signal); falls back to the
  // raw operator id carrying the viewer email (e.g. "oidc:<email>").
  function cardMine(card) {
    if (!viewerEmail) return false;
    var opEmail = (card.getAttribute('data-operator-email') || '').toLowerCase();
    if (opEmail && opEmail === viewerEmail) return true;
    var op = (card.getAttribute('data-operator') || '').toLowerCase();
    return op.indexOf(viewerEmail) !== -1;
  }

  // cardUser reports whether a card's operator identity matches a specific
  // dropdown-selected user key (lower-cased email or raw operator id). Mirrors
  // cardMine but against an arbitrary chosen operator rather than the viewer.
  function cardUser(card, key) {
    if (!key) return true;
    var opEmail = (card.getAttribute('data-operator-email') || '').toLowerCase();
    if (opEmail && opEmail === key) return true;
    var op = (card.getAttribute('data-operator') || '').toLowerCase();
    return op !== '' && op === key;
  }

  function cardVisible(card) {
    var type = card.getAttribute('data-kind') || '';
    if (mutedTypes[type]) return false;
    // A specific user pick is the most precise operator filter; when set it
    // supersedes the binary mine/everyone scope. Otherwise "mine" applies.
    if (userFilter) {
      if (!cardUser(card, userFilter)) return false;
    } else if (scope === 'mine' && !cardMine(card)) {
      return false;
    }
    if (projectFilter) {
      var proj = card.getAttribute('data-project') || '';
      if (proj !== projectFilter) return false;
    }
    return true;
  }

  // applyVisibility re-evaluates every card against the current scope + project
  // + mute filters. Cheap enough at maxRows*4 (stream + 3 lanes) on each control
  // change; we never re-evaluate per incoming event beyond the new card.
  function applyVisibility() {
    var cards = page.querySelectorAll('.feed-card');
    var anyVisibleInStream = false;
    for (var i = 0; i < cards.length; i++) {
      var vis = cardVisible(cards[i]);
      cards[i].style.display = vis ? '' : 'none';
      if (vis && cards[i].parentNode === stream) anyVisibleInStream = true;
    }
    updateFilteredEmpty(cards.length > 0 && !anyVisibleInStream);
  }

  // updateFilteredEmpty toggles the "nothing matches your filters" hint. Only
  // shown when there are cards but the active filters hide them all — so an
  // empty "Mine" view explains itself instead of looking broken.
  function updateFilteredEmpty(show) {
    if (!filteredEmpty) return;
    filteredEmpty.hidden = !show;
  }

  function trim(container) {
    while (container.children.length > maxRows) {
      container.removeChild(container.lastChild);
    }
  }

  // riseIn gives a streamed card the htmx arrival: .htmx-added until it
  // settles (htmx's own 20ms settle delay). The Homestead skins step a card
  // that wears it up into place (homestead.css, "Living signals"); every other
  // skin leaves the class unstyled. First-paint cards never pass through here.
  function riseIn(el) {
    el.classList.add('htmx-added');
    getComputedStyle(el).transform; // record the offset even if no frame lands in the 20ms
    setTimeout(function () { el.classList.remove('htmx-added'); }, 20);
  }

  function appendRow(htmlStr) {
    if (!htmlStr) return;
    var emptyState = stream.querySelector('.feed-empty-state');
    if (emptyState) stream.removeChild(emptyState);

    var tmp = document.createElement('div');
    tmp.innerHTML = htmlStr;
    var card = tmp.firstElementChild;
    if (!card) return;

    // Dedup: the ring backfill, the durable seed, and reconnect replay can all
    // re-deliver the same event. Skip a card we've already rendered.
    if (!markSeen(cardKey(card))) return;
    noteTs(card);

    var autoScroll = autoScrollBox ? autoScrollBox.checked : true;
    var vis = cardVisible(card);
    if (!vis) card.style.display = 'none';

    // Concept A: prepend into the stream.
    stream.insertBefore(card, stream.firstChild);
    riseIn(card);
    trim(stream);

    // Concept B: a clone goes into the matching lane so toggling view is
    // instant and never re-subscribes. Clone (not move) so both layouts hold a
    // copy and either can be shown without re-fetching.
    var lane = lanes[card.getAttribute('data-lane')];
    if (lane) {
      var clone = card.cloneNode(true);
      if (!vis) clone.style.display = 'none';
      lane.insertBefore(clone, lane.firstChild);
      riseIn(clone);
      trim(lane);
    }

    total++;
    updateCount();
    if (vis) updateFilteredEmpty(false);
    if (autoScroll) {
      stream.scrollTop = 0;
      if (lane) lane.scrollTop = 0;
    }
  }

  function onEvent(ev) {
    if (paused) {
      pendingRows.unshift(ev.data);
      if (pendingRows.length > maxRows) pendingRows.length = maxRows;
      updatePauseLabel();
      updateCount();
      return;
    }
    appendRow(ev.data);
  }

  function updatePauseLabel() {
    if (!pauseBtn) return;
    pauseBtn.textContent = paused
      ? (pendingRows.length ? '▶ Resume (' + pendingRows.length + ')' : '▶ Resume')
      : '⏸ Pause';
  }

  function setPaused(next) {
    paused = next;
    if (pauseBtn) pauseBtn.setAttribute('aria-pressed', paused ? 'true' : 'false');
    if (!paused && pendingRows.length) {
      for (var i = pendingRows.length - 1; i >= 0; i--) appendRow(pendingRows[i]);
      pendingRows.length = 0;
    }
    updatePauseLabel();
    updateCount();
  }

  function setView(view) {
    page.setAttribute('data-view', view);
    for (var i = 0; i < viewBtns.length; i++) {
      var btn = viewBtns[i];
      btn.setAttribute('aria-pressed', btn.getAttribute('data-view') === view ? 'true' : 'false');
    }
    // Reflect in the URL so a reload / share keeps the chosen view, without a
    // navigation (the SSE connection stays open).
    if (window.history && window.history.replaceState) {
      var u = new URL(window.location.href);
      if (view === 'stream') u.searchParams.delete('view');
      else u.searchParams.set('view', view);
      window.history.replaceState({}, '', u.toString());
    }
  }

  // ---- The auth-probe + reconnect lifecycle (mirrors telepathy.js). ----
  function probeAuth() {
    if (!statusUrl || typeof window.fetch !== 'function') return Promise.resolve(null);
    var ctrl = (typeof window.AbortController === 'function') ? new AbortController() : null;
    var t = setTimeout(function () { if (ctrl) try { ctrl.abort(); } catch (e) {} }, 5000);
    return window.fetch(statusUrl, {
      method: 'GET', credentials: 'include', cache: 'no-store',
      redirect: 'manual', headers: { 'Accept': 'application/json' },
      signal: ctrl ? ctrl.signal : undefined
    }).then(function (resp) {
      clearTimeout(t);
      if (resp.type === 'opaqueredirect' || resp.status === 0) return 'auth-expired';
      if (resp.status === 401 || resp.status === 403) return 'auth-expired';
      if (resp.status >= 200 && resp.status < 300) return 'ok';
      return null;
    }).catch(function () { clearTimeout(t); return null; });
  }

  var es = null;
  var backoff = 1000;
  var maxBackoff = 30000;
  var closed = false;
  var hasConnectedOnce = false;
  // Every taxonomy kind is its own SSE event name (see feed.go). Phase 3
  // (ka-0000006994) added the multi-family kinds: mr-opened, pipeline, squawk,
  // mail — each multiplexed off the same lifecycle stream, each its own SSE
  // event name. The agent family (gate / spawned / stopped) is no longer
  // surfaced (ka-0000003549), so its event names are gone.
  var EVENT_NAMES = [
    'decision', 'closed', 'mr-merged', 'mr-opened', 'claimed', 'blocked',
    'created', 'updated', 'pipeline', 'squawk', 'mail'
  ];

  // requestBackfill fetches /feed/backfill?since=<lastTs> and renders each
  // replayed card through the dedup'd append path, healing the gap a
  // disconnect opened (Phase 2, ka-0000000643). The endpoint streams SSE-framed
  // cards under the "backfill" event name; we parse the frames out of the text
  // body (a one-shot fetch, not a live EventSource). Best-effort: any error is
  // swallowed — the live stream resumes regardless.
  function requestBackfill() {
    if (!backfillUrl || typeof window.fetch !== 'function') return;
    var u = backfillUrl;
    if (lastTs) u += (u.indexOf('?') === -1 ? '?' : '&') + 'since=' + encodeURIComponent(lastTs);
    window.fetch(u, { credentials: 'include', cache: 'no-store', headers: { 'Accept': 'text/event-stream' } })
      .then(function (resp) { return resp.ok ? resp.text() : ''; })
      .then(function (body) {
        if (!body) return;
        // Parse "data: <card>" lines out of the SSE frames, oldest-first (the
        // server emits them in chronological order), and append each.
        var lines = body.split('\n');
        for (var i = 0; i < lines.length; i++) {
          var line = lines[i];
          if (line.indexOf('data:') === 0) {
            var html = line.slice(5).replace(/^ /, '');
            if (html) appendRow(html);
          }
        }
      })
      .catch(function () { /* swallow — live stream covers us */ });
  }

  function openEventSource() {
    if (closed) return;
    try {
      es = new EventSource(url, { withCredentials: true });
    } catch (e) {
      setStatus('error');
      scheduleReconnect();
      return;
    }
    EVENT_NAMES.forEach(function (name) { es.addEventListener(name, onEvent); });
    es.onopen = function () {
      backoff = 1000;
      setStatus('live');
      // On reconnect, gap-fill anything missed while we were offline. Skipped on
      // the very first open (the durable first-paint seed already covers it).
      // Keyed off durable event time (?since=lastTs), not Last-Event-ID — those
      // ids reset on viewer/daemon restart and can't be replayed durably.
      if (hasConnectedOnce) requestBackfill();
      hasConnectedOnce = true;
    };
    es.onerror = function () {
      if (es) { try { es.close(); } catch (e) {} }
      es = null;
      setStatus('offline');
      probeAuth().then(function (verdict) {
        if (verdict === 'auth-expired') {
          closed = true;
          setStatus('auth-expired');
          // Route into the shared edge-reauth policy (layout.html). A stream
          // expiring is passive (the user is watching, not clicking), so the
          // handler defers the re-auth to the user's next real action — the
          // status pill above is the manual fallback. (ka-0000005477,
          // ka-0000004168)
          if (window.bvReauthOnEdgeExpiry) { window.bvReauthOnEdgeExpiry(); }
          return;
        }
        scheduleReconnect();
      });
    };
  }

  function connect() {
    if (closed) return;
    setStatus('connecting');
    probeAuth().then(function (verdict) {
      if (closed) return;
      if (verdict === 'auth-expired') {
        closed = true;
        setStatus('auth-expired');
        if (window.bvReauthOnEdgeExpiry) { window.bvReauthOnEdgeExpiry(); }
        return;
      }
      openEventSource();
    });
  }

  function scheduleReconnect() {
    if (closed) return;
    var delay = backoff;
    backoff = Math.min(backoff * 2, maxBackoff);
    setTimeout(connect, delay);
  }

  // ---- Wire controls. ----
  for (var i = 0; i < viewBtns.length; i++) {
    (function (btn) {
      btn.addEventListener('click', function () { setView(btn.getAttribute('data-view')); });
    })(viewBtns[i]);
  }

  // Seed mutedTypes from the server-rendered chip state so noise-by-default
  // families (created, updated, squawk, spawned, stopped — rendered
  // aria-pressed="false") start muted without a flash of unwanted cards
  // (noise-floor concern, synthesis §6.3; ka-0000005238: normal events / trigger
  // updates / session creation are hidden by default, one click to re-admit).
  for (var m = 0; m < typeBtns.length; m++) {
    var t = typeBtns[m].getAttribute('data-type');
    if (t && typeBtns[m].getAttribute('aria-pressed') === 'false') mutedTypes[t] = true;
  }

  for (var j = 0; j < typeBtns.length; j++) {
    (function (btn) {
      btn.addEventListener('click', function () {
        var type = btn.getAttribute('data-type');
        var nowMuted = btn.getAttribute('aria-pressed') === 'true'; // pressed=shown -> click mutes
        mutedTypes[type] = nowMuted;
        btn.setAttribute('aria-pressed', nowMuted ? 'false' : 'true');
        applyVisibility();
      });
    })(typeBtns[j]);
  }

  // syncURL reflects the active scope + project in the querystring so a reload
  // or share keeps the same view, without navigating (SSE stays open).
  function syncURL() {
    if (!(window.history && window.history.replaceState)) return;
    var u = new URL(window.location.href);
    if (scope === 'everyone' || !hasViewer) u.searchParams.delete('scope');
    else u.searchParams.set('scope', scope);
    // project="" means "all"; encode it explicitly so it overrides the stored
    // default on reload, but drop it when it equals the viewer's default.
    var def = page.getAttribute('data-default-project') || '';
    if (projectFilter === def) u.searchParams.delete('project');
    else u.searchParams.set('project', projectFilter);
    // user="" means "all users" — drop the param; otherwise pin the selection.
    if (userFilter) u.searchParams.set('user', userFilter);
    else u.searchParams.delete('user');
    window.history.replaceState({}, '', u.toString());
  }

  function setScope(next) {
    if (!hasViewer) next = 'everyone';
    scope = next;
    for (var i = 0; i < scopeBtns.length; i++) {
      var btn = scopeBtns[i];
      btn.setAttribute('aria-pressed', btn.getAttribute('data-scope') === scope ? 'true' : 'false');
    }
    applyVisibility();
    syncURL();
  }

  function setProject(next) {
    projectFilter = next || '';
    if (projectSelect && projectSelect.value !== projectFilter) projectSelect.value = projectFilter;
    applyVisibility();
    syncURL();
  }

  function setUser(next) {
    userFilter = (next || '').toLowerCase();
    if (userSelect && userSelect.value !== userFilter) userSelect.value = userFilter;
    applyVisibility();
    syncURL();
  }

  for (var s = 0; s < scopeBtns.length; s++) {
    (function (btn) {
      btn.addEventListener('click', function () { setScope(btn.getAttribute('data-scope')); });
    })(scopeBtns[s]);
  }

  if (projectSelect) {
    projectSelect.addEventListener('change', function () { setProject(projectSelect.value); });
  }

  if (userSelect) {
    userSelect.addEventListener('change', function () { setUser(userSelect.value); });
  }

  if (widenBtn) {
    widenBtn.addEventListener('click', function () {
      setScope('everyone');
      setProject('');
      setUser('');
    });
  }

  if (pauseBtn) {
    pauseBtn.addEventListener('click', function () { setPaused(!paused); });
  }

  window.addEventListener('beforeunload', function () {
    closed = true;
    if (es) { try { es.close(); } catch (e) {} }
  });

  // First paint already rendered seed cards into BOTH the stream and the lanes
  // (the lanes are SSR'd from the same Backfill set in feed.html, bucketed by
  // status Lane — ka-0000001008, so ?view=lanes is populated before this script
  // runs). We therefore only register the stream cards' keys + max timestamp so
  // the live stream / reconnect replay dedup against them (Phase 2,
  // ka-0000000643); we do NOT re-clone them into the lanes (that would double
  // every seed card).
  (function registerBackfill() {
    var existing = stream.querySelectorAll('.feed-card');
    total = existing.length;
    for (var k = 0; k < existing.length; k++) {
      markSeen(cardKey(existing[k]));
      noteTs(existing[k]);
    }
  })();

  // Apply the viewer's default scope + project to the first-paint backfill so
  // the initial view is already relevant (ka-0000003403) rather than the
  // whole-fleet firehose. Reflects the resolved defaults back into the URL.
  applyVisibility();
  syncURL();

  updateCount();
  connect();
})();
