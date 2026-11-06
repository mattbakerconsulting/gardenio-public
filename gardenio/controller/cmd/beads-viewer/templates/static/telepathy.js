// telepathy.js — unified cross-agent thinking feed (epic ka-0000007154,
// task ka-0000002159).
//
// Subscribes to /telepathy/events (SSE) and prepends each rendered HTML row
// (produced by renderThinkingRow in telepathy.go) into #telepathy-feed. The
// server emits complete HTML per event; this shim arranges insertion, the
// EventSource lifecycle (backoff + auth probe, mirroring agent_events.js), an
// auto-scroll toggle, a client-side agent-name filter, and a live count.
(function () {
  'use strict';

  if (typeof window === 'undefined' || typeof window.EventSource === 'undefined') {
    return; // EventSource missing — leave empty-state visible.
  }

  var page = document.querySelector('.telepathy-page');
  if (!page) return;
  var feed = document.getElementById('telepathy-feed');
  if (!feed) return;

  var url = page.getAttribute('data-events-url');
  if (!url) return;
  var statusUrl = page.getAttribute('data-status-url') || '';

  var statusEl = page.querySelector('.telepathy-status');
  var statusLabel = page.querySelector('.telepathy-status-label');
  var autoScrollBox = page.querySelector('.telepathy-autoscroll');
  var filterInput = page.querySelector('.telepathy-filter');
  var countEl = page.querySelector('.telepathy-count');
  var pauseBtn = page.querySelector('.telepathy-pause');

  var emptyState = feed.querySelector('.telepathy-empty-state');
  var maxRows = 200; // bounded so a long-lived stream doesn't grow the DOM forever.
  var total = 0;
  var filterText = '';

  // Pause freezes the visible feed so an operator can read or point during a
  // demo. Incoming events queue in pendingRows (newest-first, bounded to maxRows)
  // and flush oldest-first on resume so insertion order matches live mode.
  var paused = false;
  var pendingRows = [];

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
    var base = total ? total + (total === 1 ? ' thought' : ' thoughts') : '';
    if (paused && pendingRows.length) {
      base += (base ? ' · ' : '') + pendingRows.length + ' buffered';
    }
    countEl.textContent = base;
  }

  function matchesFilter(row) {
    if (!filterText) return true;
    var agent = (row.getAttribute('data-agent') || '').toLowerCase();
    return agent.indexOf(filterText) !== -1;
  }

  function applyFilter() {
    var rows = feed.querySelectorAll('.telepathy-event');
    for (var i = 0; i < rows.length; i++) {
      rows[i].style.display = matchesFilter(rows[i]) ? '' : 'none';
    }
  }

  function atTop() {
    return feed.scrollTop <= 4;
  }

  function appendRow(htmlStr) {
    if (!htmlStr) return;
    if (emptyState && emptyState.parentNode === feed) {
      feed.removeChild(emptyState);
      emptyState = null;
    }
    var tmp = document.createElement('div');
    tmp.innerHTML = htmlStr;
    var row = tmp.firstElementChild;
    if (!row) return;

    var autoScroll = autoScrollBox ? autoScrollBox.checked : true;
    var wasAtTop = atTop();

    if (!matchesFilter(row)) row.style.display = 'none';
    feed.insertBefore(row, feed.firstChild);
    total++;
    updateCount();

    while (feed.children.length > maxRows) {
      feed.removeChild(feed.lastChild);
    }
    // Newest rows are inserted at the top; keep the viewport pinned there when
    // auto-scroll is on (or the user was already at the top).
    if (autoScroll || wasAtTop) {
      feed.scrollTop = 0;
    }
  }

  function onEvent(ev) {
    if (paused) {
      // Buffer newest-first and cap at maxRows so a long pause can't grow
      // memory unbounded; oldest overflow is dropped.
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
    if (paused) {
      pauseBtn.textContent = pendingRows.length
        ? '▶ Resume (' + pendingRows.length + ')'
        : '▶ Resume';
    } else {
      pauseBtn.textContent = '⏸ Pause';
    }
  }

  function setPaused(next) {
    paused = next;
    if (pauseBtn) pauseBtn.setAttribute('aria-pressed', paused ? 'true' : 'false');
    if (!paused && pendingRows.length) {
      // Flush oldest-first so the visible order matches live insertion.
      for (var i = pendingRows.length - 1; i >= 0; i--) {
        appendRow(pendingRows[i]);
      }
      pendingRows.length = 0;
    }
    updatePauseLabel();
    updateCount();
  }

  // probeAuth distinguishes auth-expired (cross-origin 303 from a session gate
  // like AWS Verified Access) from generic flaky-upstream errors. Mirrors
  // agent_events.js — EventSource cannot follow cross-origin redirects, so a
  // one-shot fetch with redirect:'manual' is the only way to inspect it.
  function probeAuth() {
    if (!statusUrl || typeof window.fetch !== 'function') return Promise.resolve(null);
    var ctrl = (typeof window.AbortController === 'function') ? new AbortController() : null;
    var t = setTimeout(function () { if (ctrl) try { ctrl.abort(); } catch (e) {} }, 5000);
    return window.fetch(statusUrl, {
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

  var es = null;
  var backoff = 1000;
  var maxBackoff = 30000;
  var closed = false;

  function openEventSource() {
    if (closed) return;
    try {
      es = new EventSource(url, { withCredentials: true });
    } catch (e) {
      setStatus('error');
      scheduleReconnect();
      return;
    }
    es.addEventListener('thinking', onEvent);
    es.onopen = function () {
      backoff = 1000;
      setStatus('live');
    };
    es.onerror = function () {
      if (es) { try { es.close(); } catch (e) { /* ignore */ } }
      es = null;
      setStatus('offline');
      probeAuth().then(function (verdict) {
        if (verdict === 'auth-expired') {
          closed = true;
          setStatus('auth-expired');
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

  if (filterInput) {
    filterInput.addEventListener('input', function () {
      filterText = filterInput.value.trim().toLowerCase();
      applyFilter();
    });
  }

  if (pauseBtn) {
    pauseBtn.addEventListener('click', function () {
      setPaused(!paused);
    });
  }

  window.addEventListener('beforeunload', function () {
    closed = true;
    if (es) { try { es.close(); } catch (e) { /* ignore */ } }
  });

  updateCount();
  connect();
})();
