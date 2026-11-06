// incident_intel_telemetry.js — Phase 9 click-through telemetry.
//
// Listens for clicks on any element carrying a `data-ii-telemetry` attribute.
// POSTs a small JSON payload to /dev/incident-intel/telemetry; failures are
// swallowed so a flaky network never blocks navigation. We use sendBeacon
// when available so the request survives the navigation that the click
// itself triggers.
(function () {
  'use strict';

  var ENDPOINT = '/dev/incident-intel/telemetry';

  function basePath() {
    // BasePath, when configured, is exposed via document.documentElement.dataset.basePath.
    // Falls back to '' (default mount) so the dev server keeps working unchanged.
    var bp = document.documentElement.dataset.basePath || '';
    return bp.replace(/\/$/, '');
  }

  function sendEvent(payload) {
    var url = basePath() + ENDPOINT;
    var body = JSON.stringify(payload);
    try {
      if (navigator && typeof navigator.sendBeacon === 'function') {
        var blob = new Blob([body], { type: 'application/json' });
        navigator.sendBeacon(url, blob);
        return;
      }
    } catch (_) { /* fall through */ }
    try {
      fetch(url, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: body,
        keepalive: true,
        credentials: 'same-origin',
      }).catch(function () { /* swallow */ });
    } catch (_) { /* swallow */ }
  }

  function handleClick(ev) {
    // Walk up from the click target to find the nearest element with
    // data-ii-telemetry — buttons frequently wrap an inner <span> or <code>.
    var el = ev.target;
    while (el && el !== document) {
      if (el.dataset && el.dataset.iiTelemetry) {
        sendEvent({
          event: el.dataset.iiTelemetry,
          target: el.dataset.iiTelemetryTarget || '',
          from: window.location.pathname || '',
          path: el.tagName ? el.tagName.toLowerCase() : '',
        });
        return;
      }
      el = el.parentNode;
    }
  }

  // capture=true so we still see clicks even if a child handler stops
  // propagation. Passive — telemetry never blocks navigation.
  document.addEventListener('click', handleClick, { capture: true, passive: true });

  // Expose a tiny test hook so unit tests can trigger sendEvent directly.
  window.IncidentIntel = window.IncidentIntel || {};
  window.IncidentIntel.telemetry = { send: sendEvent };
})();
