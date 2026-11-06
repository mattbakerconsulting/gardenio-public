// agents_stream.js — SSE client for the /agents index AND the gardener page.
//
// Subscribes to the URL in [data-agents-stream] and swaps rendered rows keyed
// by data-bead-id / id="agent-row-<id>". Two scopes share this client:
//
//   /agents index   — /agents/events?project=<filter>, streams <tr> table rows.
//                      New rows prepend to the first active <tbody>; closed rows
//                      move from the active table to the recent table.
//   /gardener/{id}    — /agents/events?schedule_id=<capID>, streams <li> life-
//                      thread cards. New iterations prepend to the top of
//                      <ul class="gardener-thread"> (newest-first); closed cards
//                      swap in place to their terminal state (no recent list).
//
// For an existing row in either scope, the new HTML replaces the node in place.
// parseRow() detects the row shape (<tr> vs <li>) from the markup, so the same
// swap logic drives both pages.
//
// Gated on the `data-agents-stream` attribute so other pages that happen to
// reuse the same script bundle are no-ops.
(function () {
  'use strict';

  if (typeof window === 'undefined' || typeof window.EventSource === 'undefined') {
    return;
  }

  var anchor = document.querySelector('[data-agents-stream]');
  if (!anchor) return;
  var url = anchor.getAttribute('data-agents-stream');
  if (!url) return;
  // Same-origin status probe so we can detect an expired AVA session WITHOUT
  // ever opening the EventSource (which Firefox would otherwise flag as
  // "Blocked" once AVA issues its cross-origin 303). The status URL is the
  // events URL with /events swapped for /events-status; ?project=... is
  // dropped because the probe is auth-only, not project-scoped.
  var statusUrl = (function () {
    var s = url.replace(/\/agents\/events(\?|$)/, '/agents/events-status$1');
    return s.split('?')[0];
  })();

  var backoff = 1000;
  var maxBackoff = 30000;
  var closed = false;
  var es = null;

  function emitHealth(state) {
    try {
      document.dispatchEvent(new CustomEvent('agents-events:health', { detail: { state: state } }));
    } catch (e) { /* ignore — older browsers without CustomEvent */ }
    if (anchor) {
      anchor.setAttribute('data-agents-stream-state', state);
    }
  }

  // probeAuth issues a one-shot fetch with redirect:'manual' so we can tell
  // an auth-expired condition (cross-origin 303 from a session-gate proxy
  // like AWS Verified Access kicking us back to its OAuth flow) apart from a
  // flaky upstream. Firefox blocks EventSource from following cross-origin
  // redirects, so the EventSource just keeps erroring with no signal — a
  // plain fetch is the only way to inspect the actual response.
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
      // redirect:'manual' surfaces as type='opaqueredirect' when the server
      // returned a 3xx. Older WebKit reports status===0 for the same case.
      if (resp.type === 'opaqueredirect' || resp.status === 0) return 'auth-expired';
      if (resp.status === 401 || resp.status === 403) return 'auth-expired';
      if (resp.status >= 200 && resp.status < 300) return 'ok';
      return null;
    }).catch(function () {
      clearTimeout(t);
      return null;
    });
  }

  function showAuthExpiredBanner() {
    if (document.querySelector('.agents-events-auth-banner')) return;
    var banner = document.createElement('div');
    banner.className = 'agents-events-auth-banner';
    banner.setAttribute('role', 'alert');
    var msg = document.createElement('span');
    msg.textContent = 'Live updates paused — your session may have expired. ';
    var btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'agents-events-auth-banner-reload';
    btn.textContent = 'Reload to re-authenticate';
    btn.addEventListener('click', function () { window.location.reload(); });
    banner.appendChild(msg);
    banner.appendChild(btn);
    var parent = anchor && anchor.parentNode;
    if (parent) {
      parent.insertBefore(banner, anchor);
    } else {
      document.body.insertBefore(banner, document.body.firstChild);
    }
  }

  function parseRow(html) {
    if (!html) return null;
    var trimmed = html.replace(/^\s+/, '');
    // The /agents index streams <tr> table rows; the gardener life-thread
    // streams <li> cards. A <tr> must be parsed inside a real <table> or the
    // browser strips it; an <li> must be parsed inside a <ul> for the same
    // reason. Pick the container by the first tag so both row shapes survive.
    var container;
    if (/^<li[\s>]/i.test(trimmed)) {
      container = document.createElement('ul');
    } else {
      var tbl = document.createElement('table');
      container = document.createElement('tbody');
      tbl.appendChild(container);
    }
    container.innerHTML = trimmed;
    return container.firstElementChild || null;
  }

  function findExisting(beadId) {
    if (!beadId) return null;
    return document.getElementById('agent-row-' + beadId) ||
      document.querySelector('tr[data-bead-id="' + cssEscape(beadId) + '"]') ||
      document.querySelector('li[data-bead-id="' + cssEscape(beadId) + '"]');
  }

  function cssEscape(s) {
    if (window.CSS && typeof window.CSS.escape === 'function') {
      return window.CSS.escape(s);
    }
    return String(s).replace(/[^a-zA-Z0-9_-]/g, '\\$&');
  }

  function firstActiveTbody(project) {
    // Prefer the project-section tbody. We can't introspect the section
    // heading reliably, so fall back to the first non-recent agents-table.
    var tables = document.querySelectorAll('table.agents-table:not(.agents-table-recent) tbody');
    if (!tables.length) return null;
    return tables[0];
  }

  function recentTbody() {
    var t = document.querySelector('table.agents-table.agents-table-recent tbody');
    return t || null;
  }

  // gardenerThread is the <ul class="gardener-thread"> the gardener page renders
  // its iteration cards into. Present only on /gardener/{id}; null elsewhere.
  // When present, the stream is gardener-scoped and emits <li> cards, so live
  // rows prepend here (newest-first) instead of into an agents <tbody>, and
  // closed rows stay in place (no separate "recent" list on the gardener page).
  function gardenerThread() {
    return document.querySelector('ul.gardener-thread');
  }

  // riseIn gives a newly streamed row the htmx arrival: .htmx-added until it
  // settles (htmx's own 20ms settle delay). The Homestead skins step a row
  // that wears it up into place (homestead.css, "Living signals"); every other
  // skin leaves the class unstyled. Rows swapped in place do not rise.
  function riseIn(el) {
    el.classList.add('htmx-added');
    getComputedStyle(el).transform; // record the offset even if no frame lands in the 20ms
    setTimeout(function () { el.classList.remove('htmx-added'); }, 20);
  }

  function onCreated(ev) {
    var row = parseRow(ev.data);
    if (!row) return;
    var existing = findExisting(row.getAttribute('data-bead-id'));
    if (existing) {
      existing.replaceWith(row);
      return;
    }
    // Gardener-scoped <li> card for a not-yet-rendered iteration: prepend it to
    // the top of the life-thread (newest-first), demoting the prior live card
    // to historical purely by position — its own IsLive styling is unchanged
    // until its next event arrives.
    var thread = gardenerThread();
    if (thread) {
      thread.insertBefore(row, thread.firstChild);
      riseIn(row);
      return;
    }
    var target = firstActiveTbody(row.getAttribute('data-project'));
    if (target) {
      target.insertBefore(row, target.firstChild);
      riseIn(row);
    }
  }

  function onUpdated(ev) {
    var row = parseRow(ev.data);
    if (!row) return;
    var existing = findExisting(row.getAttribute('data-bead-id'));
    if (existing) {
      existing.replaceWith(row);
    }
  }

  function onClosed(ev) {
    var row = parseRow(ev.data);
    if (!row) return;
    var beadId = row.getAttribute('data-bead-id');
    var existing = findExisting(beadId);
    // Gardener page: the card swaps in place to its terminal (historical) state
    // — there is no separate "recent" list, and removing it would drop the run
    // out of the life-thread. The server-rendered closed card already carries
    // bead-row-terminated, so a straight replace transitions it correctly.
    if (gardenerThread()) {
      if (existing) {
        existing.replaceWith(row);
      }
      return;
    }
    // Move the node from the active table to the recent table. The streamed
    // closed row is the 6-column recent <tr> shape (renderAgentsIndexRow emits
    // agents_recent_row for closed events), so it fits the recent table's
    // header. In list (non-compact) density there is no recent table, so
    // removing-without-reinserting would make the closed agent silently vanish
    // until the next page load (ka-0000007652). When there is nowhere to move
    // the terminal row, mark the existing active node terminated IN PLACE so it
    // stops reading as live — never leave a closed agent rendering as active.
    // The next eager render reconciles it into the recent section.
    var target = recentTbody();
    if (!target) {
      if (existing) {
        existing.classList.add('bead-row-terminated');
      }
      return;
    }
    if (existing && existing.parentNode) {
      existing.parentNode.removeChild(existing);
    }
    row.classList.add('bead-row-terminated');
    target.insertBefore(row, target.firstChild);
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
    es.addEventListener('created', onCreated);
    es.addEventListener('updated', onUpdated);
    es.addEventListener('closed', onClosed);
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
          showAuthExpiredBanner();
          // Notify the shared edge-reauth policy. A stream expiring is passive
          // (the user is watching, not clicking), so the handler now DEFERS the
          // re-auth rather than force-reloading mid-read — the banner above is
          // the manual fallback and the user's next real action self-heals.
          // (ka-0000005477, ka-0000004168)
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
    // onerror path will then surface the banner if it really is auth.
    probeAuth().then(function (verdict) {
      if (closed) return;
      if (verdict === 'auth-expired') {
        closed = true;
        emitHealth('auth-expired');
        showAuthExpiredBanner();
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
