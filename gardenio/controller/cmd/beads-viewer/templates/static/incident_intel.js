// incident_intel.js — Phase 1 shared catalog primitives.
//
// Plan-of-record: docs/research/incident-intelligence/synthesis.md §Phase 1.
// Bead: ka-0000007090. Exposes a single global `IncidentIntel` namespace so
// downstream widgets can compose against these primitives without each one
// re-implementing keyboard handling, time formatting, or modal plumbing.
//
// The existing keys.js script handles global navigation (j/k/g/x). This bus
// is widget-scoped: handlers register with a scope id, and only handlers
// inside the currently-focused widget fire. Letters reserved by keys.js
// (j, k, /, ?, c, o, g, G) are blocked at registration time so widget
// authors get a clear error rather than a silent miss.
(function () {
  'use strict';

  var RESERVED_KEYS = {
    'j': 'keys.js (move down)',
    'k': 'keys.js (move up)',
    '/': 'keys.js (focus search)',
    '?': 'keys.js (toggle help)',
    'g': 'keys.js (jump prefix)',
    'G': 'keys.js (jump bottom)',
    'o': 'keys.js (open focused row)'
  };

  // ---------- Letter-key bus ----------
  var keyHandlers = []; // [{ scope, key, handler, label }]

  function isEditable(el) {
    if (!el) return false;
    var tag = (el.tagName || '').toLowerCase();
    if (tag === 'input' || tag === 'textarea' || tag === 'select') return true;
    return !!(el.isContentEditable);
  }

  function findScopeFromTarget(target) {
    var n = target;
    while (n && n.nodeType === 1) {
      if (n.dataset && n.dataset.iiScope) return n.dataset.iiScope;
      n = n.parentNode;
    }
    return null;
  }

  function registerKey(scope, key, handler, label) {
    if (!scope) throw new Error('IncidentIntel.registerKey: scope is required');
    if (!key || key.length !== 1) throw new Error('IncidentIntel.registerKey: key must be a single character');
    if (RESERVED_KEYS[key]) {
      throw new Error('IncidentIntel.registerKey: key "' + key + '" is reserved by ' + RESERVED_KEYS[key]);
    }
    if (typeof handler !== 'function') throw new Error('IncidentIntel.registerKey: handler must be a function');
    keyHandlers.push({ scope: scope, key: key, handler: handler, label: label || '' });
  }

  function unregisterScope(scope) {
    keyHandlers = keyHandlers.filter(function (h) { return h.scope !== scope; });
  }

  function dispatchKey(ev) {
    if (ev.metaKey || ev.ctrlKey || ev.altKey) return;
    if (isEditable(ev.target)) return;
    if (ev.key.length !== 1) return;
    var activeScope = findScopeFromTarget(document.activeElement);
    if (!activeScope) return;
    for (var i = 0; i < keyHandlers.length; i++) {
      var h = keyHandlers[i];
      if (h.scope === activeScope && h.key === ev.key) {
        ev.preventDefault();
        h.handler(ev);
        return;
      }
    }
  }

  // ---------- Relative-time component ----------
  // <time class="ii-relative-time" datetime="2026-05-06T20:00:00Z">5m ago</time>
  // Tick once a minute; format follows fmtTimeAgo from active.go on the server.
  function formatAgo(t) {
    var now = Date.now();
    var ms = now - t.getTime();
    if (ms < 0) return 'in the future';
    var s = Math.floor(ms / 1000);
    if (s < 60) return 'just now';
    var m = Math.floor(s / 60);
    if (m < 60) return m + 'm ago';
    var h = Math.floor(m / 60);
    if (h < 24) return h + 'h ago';
    var d = Math.floor(h / 24);
    if (d < 30) return d + 'd ago';
    var mo = Math.floor(d / 30);
    if (mo < 12) return mo + 'mo ago';
    var y = Math.floor(d / 365);
    return y + 'y ago';
  }

  function refreshRelativeTimes(root) {
    var nodes = (root || document).querySelectorAll('time.ii-relative-time[datetime]');
    var now = Date.now();
    for (var i = 0; i < nodes.length; i++) {
      var n = nodes[i];
      var dt = n.getAttribute('datetime');
      var t = new Date(dt);
      if (isNaN(t.getTime())) continue;
      n.textContent = formatAgo(t);
      // Stale flag — if more than 1 hour old, mark for styling.
      var stale = (now - t.getTime()) > (60 * 60 * 1000);
      n.dataset.stale = stale ? 'true' : 'false';
    }
  }

  // ---------- Confirm modal ----------
  // Returns a Promise<boolean> — resolves true if the user confirms.
  function confirmModal(opts) {
    opts = opts || {};
    return new Promise(function (resolve) {
      var backdrop = document.createElement('div');
      backdrop.className = 'ii-modal-backdrop';
      var modal = document.createElement('div');
      modal.className = 'ii-modal';
      modal.setAttribute('role', 'dialog');
      modal.setAttribute('aria-modal', 'true');
      var title = document.createElement('h3');
      title.className = 'ii-modal-title';
      title.textContent = opts.title || 'Confirm';
      var body = document.createElement('p');
      body.className = 'ii-modal-body';
      body.textContent = opts.body || 'Are you sure?';
      var actions = document.createElement('div');
      actions.className = 'ii-modal-actions';
      var cancelBtn = document.createElement('button');
      cancelBtn.type = 'button';
      cancelBtn.className = 'ii-modal-cancel';
      cancelBtn.textContent = opts.cancelLabel || 'Cancel';
      var confirmBtn = document.createElement('button');
      confirmBtn.type = 'button';
      confirmBtn.className = 'ii-modal-confirm' + (opts.danger ? ' ii-modal-confirm-danger' : '');
      confirmBtn.textContent = opts.confirmLabel || 'Confirm';
      actions.appendChild(cancelBtn);
      actions.appendChild(confirmBtn);
      modal.appendChild(title);
      modal.appendChild(body);
      modal.appendChild(actions);
      backdrop.appendChild(modal);

      function close(result) {
        backdrop.removeEventListener('keydown', onKey, true);
        if (backdrop.parentNode) backdrop.parentNode.removeChild(backdrop);
        resolve(result);
      }
      function onKey(ev) {
        if (ev.key === 'Escape') { ev.preventDefault(); close(false); }
        if (ev.key === 'Enter') { ev.preventDefault(); close(true); }
      }
      backdrop.addEventListener('click', function (ev) {
        if (ev.target === backdrop) close(false);
      });
      cancelBtn.addEventListener('click', function () { close(false); });
      confirmBtn.addEventListener('click', function () { close(true); });

      document.body.appendChild(backdrop);
      backdrop.tabIndex = -1;
      backdrop.addEventListener('keydown', onKey, true);
      confirmBtn.focus();
    });
  }

  // ---------- Drawer ----------
  // openDrawer({ title, html }) => returns the drawer element; close via closeDrawer.
  var activeDrawer = null;
  function openDrawer(opts) {
    closeDrawer(); // single drawer at a time
    opts = opts || {};
    var backdrop = document.createElement('div');
    backdrop.className = 'ii-drawer-backdrop';
    var drawer = document.createElement('aside');
    drawer.className = 'ii-drawer';
    drawer.setAttribute('role', 'dialog');
    drawer.setAttribute('aria-modal', 'true');
    var header = document.createElement('header');
    header.className = 'ii-drawer-header';
    var title = document.createElement('h3');
    title.className = 'ii-drawer-title';
    title.textContent = opts.title || '';
    var close = document.createElement('button');
    close.className = 'ii-drawer-close';
    close.type = 'button';
    close.setAttribute('aria-label', 'Close drawer');
    close.textContent = '×';
    header.appendChild(title);
    header.appendChild(close);
    var body = document.createElement('div');
    body.className = 'ii-drawer-body';
    if (opts.html) body.innerHTML = opts.html;
    else if (opts.text) body.textContent = opts.text;
    drawer.appendChild(header);
    drawer.appendChild(body);
    // Nest the drawer inside the backdrop so DOM queries like
    // `.ii-drawer-backdrop .ii-drawer` resolve (ka-0000005611). Both
    // elements are position:fixed so nesting has no layout effect.
    backdrop.appendChild(drawer);
    document.body.appendChild(backdrop);
    // Force a layout flush so the transition runs.
    drawer.getBoundingClientRect();
    drawer.dataset.open = 'true';
    function onKey(ev) { if (ev.key === 'Escape') closeDrawer(); }
    // Only the backdrop itself (not the drawer it now contains) closes.
    backdrop.addEventListener('click', function (ev) {
      if (ev.target === backdrop) closeDrawer();
    });
    close.addEventListener('click', closeDrawer);
    document.addEventListener('keydown', onKey);
    activeDrawer = { backdrop: backdrop, drawer: drawer, onKey: onKey };
    return drawer;
  }
  function closeDrawer() {
    if (!activeDrawer) return;
    document.removeEventListener('keydown', activeDrawer.onKey);
    if (activeDrawer.backdrop.parentNode) activeDrawer.backdrop.parentNode.removeChild(activeDrawer.backdrop);
    if (activeDrawer.drawer.parentNode) activeDrawer.drawer.parentNode.removeChild(activeDrawer.drawer);
    activeDrawer = null;
  }

  // ---------- Filter-chip strip ----------
  // Wires click-to-remove and clear-all on any element with [data-ii-chip-strip].
  // Emits a CustomEvent('ii:chip-removed', detail: { value }) on removal,
  // and 'ii:chip-cleared' on the clear button.
  function bindChipStrips(root) {
    var strips = (root || document).querySelectorAll('[data-ii-chip-strip]');
    for (var i = 0; i < strips.length; i++) {
      var strip = strips[i];
      if (strip.dataset.iiBound === 'true') continue;
      strip.dataset.iiBound = 'true';
      strip.addEventListener('click', function (ev) {
        var rm = ev.target.closest('.ii-chip-remove');
        if (rm) {
          var chip = rm.closest('.ii-chip');
          if (!chip) return;
          var val = chip.dataset.value || chip.textContent.trim();
          chip.parentNode.removeChild(chip);
          this.dispatchEvent(new CustomEvent('ii:chip-removed', { bubbles: true, detail: { value: val } }));
          return;
        }
        var clr = ev.target.closest('.ii-chip-clear');
        if (clr) {
          var chips = this.querySelectorAll('.ii-chip');
          for (var j = 0; j < chips.length; j++) chips[j].parentNode.removeChild(chips[j]);
          this.dispatchEvent(new CustomEvent('ii:chip-cleared', { bubbles: true }));
        }
      });
    }
  }

  // ---------- Source-kind icon helper ----------
  // Returns an HTML snippet for a source-kind chip given a slug + optional label.
  var SOURCE_KINDS = {
    'mezmo': 'Mezmo',
    'victoria-metrics': 'VictoriaMetrics',
    'victoria-traces': 'VictoriaTraces',
    'sentry': 'Sentry',
    'jira': 'JIRA',
    'gitlab': 'GitLab',
    'code': 'Code',
    'slack': 'Slack'
  };
  function sourceChip(kind, label) {
    if (!SOURCE_KINDS[kind]) kind = 'code';
    var name = label || SOURCE_KINDS[kind] || kind;
    var span = document.createElement('span');
    span.className = 'ii-source';
    span.dataset.sourceKind = kind;
    var icon = document.createElement('span');
    icon.className = 'ii-source-icon';
    icon.setAttribute('aria-hidden', 'true');
    var text = document.createTextNode(name);
    span.appendChild(icon);
    span.appendChild(text);
    return span;
  }

  // ---------- Bootstrap ----------
  function init() {
    document.addEventListener('keydown', dispatchKey);
    refreshRelativeTimes(document);
    setInterval(function () { refreshRelativeTimes(document); }, 60 * 1000);
    bindChipStrips(document);
    bindDrawerKeys(document);
    // Also rebind after htmx swaps so newly-injected fragments work.
    document.body.addEventListener('htmx:afterSwap', function (ev) {
      bindChipStrips(ev.target);
      refreshRelativeTimes(ev.target);
      bindDrawerKeys(ev.target);
    });
  }

  // bindDrawerKeys: any element with data-ii-drawer-key opens a drawer whose
  // body is the matching <template data-ii-drawer-key="..."> element. Used by
  // Phase 5 RCA evidence cards so drill-down content stays out of the live
  // DOM until requested. Idempotent — re-binding the same node is a no-op.
  function bindDrawerKeys(root) {
    if (!root || !root.querySelectorAll) return;
    var btns = root.querySelectorAll('[data-ii-drawer-key]:not(template):not([data-ii-drawer-bound])');
    btns.forEach(function (btn) {
      btn.dataset.iiDrawerBound = '1';
      btn.addEventListener('click', function () {
        var key = btn.getAttribute('data-ii-drawer-key');
        if (!key) return;
        var tpl = document.querySelector('template[data-ii-drawer-key="' + key + '"]');
        var html = tpl ? tpl.innerHTML : '';
        openDrawer({
          title: btn.getAttribute('data-ii-drawer-title') || 'Detail',
          html: html
        });
      });
    });
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }

  window.IncidentIntel = {
    registerKey: registerKey,
    unregisterScope: unregisterScope,
    refreshRelativeTimes: refreshRelativeTimes,
    formatAgo: formatAgo,
    confirmModal: confirmModal,
    openDrawer: openDrawer,
    closeDrawer: closeDrawer,
    bindChipStrips: bindChipStrips,
    bindDrawerKeys: bindDrawerKeys,
    sourceChip: sourceChip,
    SOURCE_KINDS: SOURCE_KINDS,
    RESERVED_KEYS: RESERVED_KEYS
  };
})();
