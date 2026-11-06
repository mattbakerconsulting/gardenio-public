// agent_events.js — Phase 2 bead-events activity lane.
//
// Subscribes to /agents/{id}/events (SSE) and prepends each rendered HTML row
// into <aside id="agent-events">. The server already produces complete HTML
// per event (see renderAgentEventRow in agent_events.go); this shim only
// arranges insertion + the EventSource lifecycle. No htmx-sse extension is
// needed.
//
// Anti-scope (Phase 2): no client-side filtering, no row dedup, no infinite
// scroll. Server-side filtering is sufficient and keeps the page simple.
(function () {
  'use strict';

  if (typeof window === 'undefined' || typeof window.EventSource === 'undefined') {
    return; // EventSource missing — leave empty-state visible.
  }

  var lane = document.getElementById('agent-events');
  if (!lane) return;
  var url = lane.getAttribute('data-events-url');
  if (!url) return;
  // Same-origin status probe so we can detect an expired AVA session WITHOUT
  // opening the EventSource (Firefox flags the SSE request as "Blocked" when
  // AVA issues a cross-origin 303). The per-agent /agents/{id}/events stream
  // reuses the fleet-wide /agents/events-status endpoint as a session probe —
  // it carries no per-agent state, only the auth verdict.
  var statusUrl = (function () {
    var basePath = url.split('?')[0].replace(/\/agents\/.*$/, '');
    return basePath + '/agents/events-status';
  })();

  var emptyState = lane.querySelector('.agent-events-empty');
  var maxRows = 200; // bounded so a long-lived stream doesn't grow the DOM forever.

  var es = null;
  var backoff = 1000;
  var maxBackoff = 30000;
  var closed = false;

  function appendRow(html) {
    if (!html) return;
    if (emptyState && emptyState.parentNode === lane) {
      lane.removeChild(emptyState);
      emptyState = null;
    }
    var tmp = document.createElement('div');
    tmp.innerHTML = html;
    var row = tmp.firstElementChild;
    if (!row) return;
    lane.insertBefore(row, lane.firstChild);
    while (lane.children.length > maxRows) {
      lane.removeChild(lane.lastChild);
    }
  }

  function onEvent(ev) {
    appendRow(ev.data);
  }

  function emitHealth(state) {
    try {
      document.dispatchEvent(new CustomEvent('agent-events:health', { detail: { state: state } }));
    } catch (e) { /* ignore — older browsers without CustomEvent */ }
  }

  // probeAuth distinguishes auth-expired (cross-origin 303 from a session
  // gate like AWS Verified Access) from generic flaky-upstream errors.
  // EventSource is forbidden from following cross-origin redirects, so it
  // keeps erroring with no signal — a one-shot fetch with redirect:'manual'
  // is the only way to inspect the response. See ka-0000007070.
  function probeAuth() {
    if (typeof window.fetch !== 'function') return Promise.resolve(null);
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

  function openEventSource() {
    if (closed) return;
    try {
      es = new EventSource(url, { withCredentials: true });
    } catch (e) {
      emitHealth('error');
      scheduleReconnect();
      return;
    }
    es.addEventListener('created', onEvent);
    es.addEventListener('updated', onEvent);
    es.addEventListener('closed', onEvent);
    es.onopen = function () {
      backoff = 1000;
      emitHealth('live');
    };
    es.onerror = function () {
      // EventSource will retry on its own for transient errors. We close
      // explicitly so we control the backoff and avoid runaway reconnects
      // on auth failure (proxied 401 closes the connection).
      if (es) { try { es.close(); } catch (e) { /* ignore */ } }
      es = null;
      emitHealth('offline');
      probeAuth().then(function (verdict) {
        if (verdict === 'auth-expired') {
          closed = true;
          emitHealth('auth-expired');
          if (window.bvReauthOnEdgeExpiry) { window.bvReauthOnEdgeExpiry(); }
          return;
        }
        scheduleReconnect();
      });
    };
  }

  function connect() {
    if (closed) return;
    emitHealth('connecting');
    // Probe the same-origin status endpoint BEFORE opening EventSource so an
    // auth-expired session never triggers the browser's "Blocked" indicator
    // on the SSE request. probeAuth returning null (network blip, fetch
    // unavailable) falls back to opening EventSource directly — the existing
    // onerror path will then surface the auth-expired state if applicable.
    probeAuth().then(function (verdict) {
      if (closed) return;
      if (verdict === 'auth-expired') {
        closed = true;
        emitHealth('auth-expired');
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

  window.addEventListener('beforeunload', function () {
    closed = true;
    if (es) { try { es.close(); } catch (e) { /* ignore */ } }
  });

  connect();
})();
