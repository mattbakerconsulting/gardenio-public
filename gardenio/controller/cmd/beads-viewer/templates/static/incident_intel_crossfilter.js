// Phase 8 — cross-filter v2 client.
// Plan-of-record: docs/research/incident-intelligence/synthesis.md §Phase 8.
// Bead: ka-0000007379.
//
// Exposes window.IncidentIntel.crossFilter:
//   .read()           returns {key: [v1, v2]}
//   .write(state)     writes state to URL fragment + emits change event
//   .add(k, v)        convenience: append a value
//   .remove(k, v)     convenience: drop a value
//   .clear()          clears all
//   .subscribe(fn)    fn is called with the new state when it changes
//   .serialize()      wire-format string ("k1:v1,k2:v2")
//   .applyTo(host)    hides non-matching descendants of `host` whose
//                       `data-ii-crossfilter-keys` lists fields the filter
//                       constrains.
//
// URL fragment: `#ii-f=<encoded>`. Multiple values for one key are comma-
// separated pairs of the same key, matching the Go-side parser:
//   `service:study-web,service:sponsor-api,severity:p1`.

(function () {
  if (typeof window === 'undefined') return;
  var II = window.IncidentIntel = window.IncidentIntel || {};

  var FRAGMENT_KEY = 'ii-f';

  function readFragment() {
    var frag = (window.location.hash || '').replace(/^#/, '');
    if (!frag) return '';
    var parts = frag.split('&');
    for (var i = 0; i < parts.length; i++) {
      var kv = parts[i].split('=');
      if (kv[0] === FRAGMENT_KEY) {
        try { return decodeURIComponent(kv[1] || ''); } catch (e) { return ''; }
      }
    }
    return '';
  }

  function writeFragment(serialized) {
    var frag = (window.location.hash || '').replace(/^#/, '');
    var parts = frag.split('&').filter(function (p) {
      return p && p.split('=')[0] !== FRAGMENT_KEY;
    });
    if (serialized) {
      parts.push(FRAGMENT_KEY + '=' + encodeURIComponent(serialized));
    }
    var next = parts.length ? '#' + parts.join('&') : '';
    if (next !== window.location.hash) {
      // Use replaceState so the back button isn't polluted with chip toggles.
      try {
        window.history.replaceState(null, '', window.location.pathname + window.location.search + next);
      } catch (e) {
        window.location.hash = next;
      }
    }
  }

  function parse(serialized) {
    var out = {};
    if (!serialized) return out;
    var pairs = String(serialized).split(',');
    for (var i = 0; i < pairs.length; i++) {
      var pair = pairs[i].trim();
      if (!pair) continue;
      var idx = pair.indexOf(':');
      if (idx < 1) continue;
      var k = pair.slice(0, idx).trim();
      var v = pair.slice(idx + 1).trim();
      if (!k || !v) continue;
      if (!out[k]) out[k] = [];
      if (out[k].indexOf(v) < 0) out[k].push(v);
    }
    return out;
  }

  function serialize(state) {
    if (!state) return '';
    var keys = Object.keys(state).sort();
    var parts = [];
    for (var i = 0; i < keys.length; i++) {
      var k = keys[i];
      var vs = state[k] || [];
      vs.slice().sort().forEach(function (v) {
        parts.push(k + ':' + v);
      });
    }
    return parts.join(',');
  }

  var state = parse(readFragment());
  var subscribers = [];

  function notify() {
    var s = serialize(state);
    writeFragment(s);
    document.dispatchEvent(new CustomEvent('ii:crossfilter-changed', {
      detail: { state: state, serialized: s }
    }));
    for (var i = 0; i < subscribers.length; i++) {
      try { subscribers[i](state, s); } catch (e) { /* keep going */ }
    }
  }

  function read() { return state; }

  function write(next) {
    state = next || {};
    notify();
  }

  function add(k, v) {
    if (!k || !v) return;
    if (!state[k]) state[k] = [];
    if (state[k].indexOf(v) < 0) {
      state[k].push(v);
      notify();
    }
  }

  function remove(k, v) {
    if (!state[k]) return;
    var i = state[k].indexOf(v);
    if (i < 0) return;
    state[k].splice(i, 1);
    if (state[k].length === 0) delete state[k];
    notify();
  }

  function clear() {
    if (!Object.keys(state).length) return;
    state = {};
    notify();
  }

  function subscribe(fn) {
    if (typeof fn !== 'function') return function () {};
    subscribers.push(fn);
    return function () {
      var i = subscribers.indexOf(fn);
      if (i >= 0) subscribers.splice(i, 1);
    };
  }

  // applyTo finds descendants of host with data-ii-crossfilter-keys (a
  // comma-separated list of keys this element cares about) and sets the
  // `hidden` attribute when the filter excludes the element. Element values
  // come from `data-ii-crossfilter-<key>` attributes, e.g.
  //   <li data-ii-crossfilter-keys="service" data-ii-crossfilter-service="study-web">
  function applyTo(host) {
    if (!host) host = document;
    var nodes = host.querySelectorAll('[data-ii-crossfilter-keys]');
    var totalByGroup = {};
    var visibleByGroup = {};
    for (var i = 0; i < nodes.length; i++) {
      var el = nodes[i];
      var keys = (el.getAttribute('data-ii-crossfilter-keys') || '').split(',').map(function (k) { return k.trim(); }).filter(Boolean);
      var visible = true;
      for (var j = 0; j < keys.length; j++) {
        var key = keys[j];
        if (!state[key] || !state[key].length) continue;
        var attr = el.getAttribute('data-ii-crossfilter-' + key) || '';
        var values = attr.split(',').map(function (s) { return s.trim(); }).filter(Boolean);
        var match = false;
        for (var v = 0; v < values.length; v++) {
          if (state[key].indexOf(values[v]) >= 0) { match = true; break; }
        }
        if (!match) { visible = false; break; }
      }
      if (visible) el.removeAttribute('hidden');
      else el.setAttribute('hidden', 'hidden');
      // Update widget-level totals so badges can show "filtering N of M".
      var group = el.closest('[data-ii-crossfilter-group]');
      if (group) {
        var gid = group.getAttribute('data-ii-crossfilter-group');
        totalByGroup[gid] = (totalByGroup[gid] || 0) + 1;
        if (visible) visibleByGroup[gid] = (visibleByGroup[gid] || 0) + 1;
      }
    }
    // Update each badge.
    var badges = (host.querySelectorAll ? host : document).querySelectorAll('[data-ii-crossfilter-badge-for]');
    for (var b = 0; b < badges.length; b++) {
      var badge = badges[b];
      var gid2 = badge.getAttribute('data-ii-crossfilter-badge-for');
      var total = totalByGroup[gid2];
      var vis = visibleByGroup[gid2] || 0;
      if (total === undefined) continue;
      if (Object.keys(state).length === 0) {
        badge.setAttribute('data-ii-crossfilter-active', 'false');
        badge.textContent = badge.getAttribute('data-ii-crossfilter-default') || badge.textContent;
      } else {
        badge.setAttribute('data-ii-crossfilter-active', 'true');
        badge.textContent = 'filtering ' + vis + ' of ' + total;
      }
    }
  }

  function applyAll() { applyTo(document); }

  II.crossFilter = {
    read: read,
    write: write,
    add: add,
    remove: remove,
    clear: clear,
    subscribe: subscribe,
    serialize: function () { return serialize(state); },
    applyTo: applyTo
  };

  // Re-apply on every change, on initial load, and after htmx swaps so newly-
  // injected fragments inherit the filter state without explicit wiring.
  document.addEventListener('ii:crossfilter-changed', applyAll);
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', applyAll);
  } else {
    applyAll();
  }
  document.addEventListener('htmx:afterSwap', applyAll);

  // Filter-chip strips: Phase 1 already emits ii:chip-removed / ii:chip-cleared.
  // Phase 8 adds two-way binding: the chip strip becomes a writer for the
  // filter, and chip values use `key:value` to round-trip with this module.
  document.addEventListener('ii:chip-removed', function (ev) {
    var raw = ev.detail && ev.detail.value;
    if (!raw) return;
    var idx = raw.indexOf(':');
    if (idx < 1) return;
    remove(raw.slice(0, idx).trim(), raw.slice(idx + 1).trim());
  });
  document.addEventListener('ii:chip-cleared', function () {
    clear();
  });
})();
