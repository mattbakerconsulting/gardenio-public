// molecule_events.js — live molecule execution progress (formula-legibility
// plan ka-0000007376, step S3 ka-0000007739).
//
// Subscribes to /molecule/{id}/events (SSE). Like the plan cockpit
// (plan_events.js), the server re-renders the WHOLE #molecule-exec fragment
// on each relevant subtree change and emits it under the `molecule-updated`
// event; this shim swaps the section's outerHTML in place so the DAG status
// tints, the progress bar, and the "you are here" frontier reflect reality
// the moment a step bead changes — no reload. The lifecycle (probe → connect
// → backoff → close) mirrors plan_events.js so auth-expiry handling stays
// consistent across the viewer.
(function () {
  'use strict';

  if (typeof window === 'undefined' || typeof window.EventSource === 'undefined') {
    return; // EventSource missing — the server-rendered view stays static.
  }

  var anchor = document.querySelector('[data-molecule-events]');
  if (!anchor) return;
  var url = anchor.getAttribute('data-molecule-events');
  if (!url) return;

  // Same-origin auth probe before opening EventSource, so an expired AVA
  // session never trips the browser's "Blocked" indicator on the SSE request.
  // Reuse the fleet-wide /agents/events-status endpoint — it carries no
  // per-stream state, only the auth verdict (same reuse as plan_events.js).
  var statusUrl = (function () {
    var basePath = url.split('?')[0].replace(/\/molecule\/.*$/, '');
    return basePath + '/agents/events-status';
  })();

  var es = null;
  var backoff = 1000;
  var maxBackoff = 30000;
  var closed = false;

  function onMoleculeEvent(ev) {
    if (!ev || !ev.data) return;
    var current = document.getElementById('molecule-exec');
    if (!current) return;
    var tmp = document.createElement('div');
    tmp.innerHTML = ev.data;
    var next = tmp.firstElementChild;
    if (!next || next.id !== 'molecule-exec') return;
    current.parentNode.replaceChild(next, current);
    next.classList.remove('molecule-exec-updated');
    // force reflow so the flash restarts even on consecutive swaps
    void next.offsetWidth;
    next.classList.add('molecule-exec-updated');
  }

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
      scheduleReconnect();
      return;
    }
    es.addEventListener('molecule-updated', onMoleculeEvent);
    es.onopen = function () {
      backoff = 1000;
    };
    es.onerror = function () {
      if (es) { try { es.close(); } catch (e) { /* ignore */ } }
      es = null;
      probeAuth().then(function (verdict) {
        if (verdict === 'auth-expired') {
          closed = true;
          if (window.bvReauthOnEdgeExpiry) { window.bvReauthOnEdgeExpiry(); }
          return;
        }
        scheduleReconnect();
      });
    };
  }

  function connect() {
    if (closed) return;
    probeAuth().then(function (verdict) {
      if (closed) return;
      if (verdict === 'auth-expired') {
        closed = true;
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
