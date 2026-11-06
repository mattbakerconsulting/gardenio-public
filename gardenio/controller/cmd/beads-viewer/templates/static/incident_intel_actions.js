// incident_intel_actions.js — Phase 3 shared action dispatcher.
//
// Plan-of-record: docs/research/incident-intelligence/synthesis.md §Phase 3.
// Bead: ka-0000005188. One handler binds every actionable button across every
// Incident Intelligence widget. Widget templates render
//
//   <button data-ii-action='{"kind":"external-link","target":"https://..."}'>
//     Open
//   </button>
//
// and this script handles confirm/auth/telemetry uniformly. Adding a new
// action kind means editing this file once, not editing every widget.
//
// D6 (IDE deep-link): the `ide-deeplink` kind reads the user's preferred IDE
// from localStorage (`ii:ide-pref` ∈ {"cursor","vscode"}, default "cursor")
// and constructs the URL with that scheme. The settings UI lives in the
// gallery — see incident_intel_gallery.html.
(function () {
  'use strict';

  var STORAGE_IDE_PREF = 'ii:ide-pref';
  var STORAGE_IDE_ROOT = 'ii:ide-root';

  function readIDEPref() {
    try {
      var v = localStorage.getItem(STORAGE_IDE_PREF);
      if (v === 'cursor' || v === 'vscode') return v;
    } catch (e) { /* localStorage may be denied */ }
    return 'cursor';
  }

  function readIDERoot() {
    try {
      return localStorage.getItem(STORAGE_IDE_ROOT) || '';
    } catch (e) { return ''; }
  }

  function writeIDEPref(v) {
    if (v !== 'cursor' && v !== 'vscode') return;
    try { localStorage.setItem(STORAGE_IDE_PREF, v); } catch (e) { /* ignore */ }
  }

  function writeIDERoot(v) {
    try { localStorage.setItem(STORAGE_IDE_ROOT, v || ''); } catch (e) { /* ignore */ }
  }

  // Telemetry — emits a CustomEvent on document for any listener (e.g. a
  // future analytics shim) to pick up. Two events:
  //   - ii:action-dispatched (descriptor: <full descriptor>)
  //   - ii:action-error      (descriptor, error)
  function emit(name, detail) {
    document.dispatchEvent(new CustomEvent(name, { bubbles: false, detail: detail || {} }));
  }

  function parseDescriptor(el) {
    var raw = el.getAttribute('data-ii-action');
    if (!raw) return null;
    try {
      return JSON.parse(raw);
    } catch (e) {
      console.warn('incident-intel: bad data-ii-action JSON on', el, e);
      return null;
    }
  }

  function buildIDEUrl(target) {
    var pref = readIDEPref();
    var root = readIDERoot();
    var path = target || '';
    // If the descriptor target already includes :line, keep it; the IDE
    // schemes both accept "<scheme>://file/<absolute-path>:<line>" form.
    var prefix = pref === 'vscode' ? 'vscode://file/' : 'cursor://file/';
    if (root && path && path.charAt(0) !== '/') {
      // Best-effort join — root ends with / or not, path is repo-relative.
      if (root.charAt(root.length - 1) !== '/') root = root + '/';
      path = root + path;
    }
    return prefix + path;
  }

  function executeExternalLink(d) {
    if (!d.target) throw new Error('external-link: missing target');
    window.open(d.target, '_blank', 'noopener,noreferrer');
  }

  function executeIDEDeeplink(d) {
    if (!d.target) throw new Error('ide-deeplink: missing target');
    var url = buildIDEUrl(d.target);
    // navigation by setting location triggers the OS handler for cursor:// /
    // vscode://; window.open works too but some browsers block it without a
    // user gesture chain.
    window.location.href = url;
  }

  function executeClipboard(d) {
    var text = d.target || '';
    if (navigator.clipboard && navigator.clipboard.writeText) {
      return navigator.clipboard.writeText(text);
    }
    // Fallback for older browsers — synthesize a textarea and execCommand.
    var ta = document.createElement('textarea');
    ta.value = text;
    ta.setAttribute('readonly', '');
    ta.style.position = 'absolute';
    ta.style.left = '-9999px';
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand('copy'); } finally { document.body.removeChild(ta); }
    return Promise.resolve();
  }

  function executeModalTrigger(d) {
    // target = element id of a template OR raw HTML payload via params.html
    var html = (d.params && d.params.html) || '';
    var title = (d.params && d.params.title) || d.label || 'Details';
    if (!html && d.target) {
      var src = document.getElementById(d.target);
      if (src) html = src.innerHTML;
    }
    return window.IncidentIntel.confirmModal({
      title: title,
      body: html, // textContent — caller can swap to a richer modal later
      confirmLabel: 'Close',
      cancelLabel: ''
    });
  }

  function executeDrawerTrigger(d) {
    var html = (d.params && d.params.html) || '';
    var text = (d.params && d.params.text) || '';
    var title = (d.params && d.params.title) || d.label || 'Details';
    if (!html && !text && d.target) {
      var src = document.getElementById(d.target);
      if (src) html = src.innerHTML;
    }
    window.IncidentIntel.openDrawer({ title: title, html: html, text: text });
  }

  function executeHtmxPost(d, originEl) {
    if (!d.id) throw new Error('htmx-post: missing id');
    var basePath = (window.IncidentIntel && window.IncidentIntel.basePath) || '';
    var url = basePath + '/actions/' + encodeURIComponent(d.id);
    var body = JSON.stringify(d.params || {});
    return fetch(url, {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: body
    }).then(function (resp) {
      if (!resp.ok) {
        return resp.text().then(function (txt) {
          throw new Error('action ' + d.id + ' failed: ' + resp.status + ' ' + txt.slice(0, 200));
        });
      }
      return resp.text().then(function (html) {
        if (d.swap_into) {
          var tgt = document.querySelector(d.swap_into);
          if (tgt) tgt.innerHTML = html;
        }
        return html;
      });
    });
  }

  function dispatch(d, originEl) {
    var promise = Promise.resolve(true);
    if (d.confirm) {
      promise = window.IncidentIntel.confirmModal({
        title: d.confirm.title,
        body: d.confirm.body,
        confirmLabel: d.confirm.confirm,
        danger: !!d.confirm.danger
      });
    }
    return promise.then(function (ok) {
      if (!ok) { emit('ii:action-cancelled', { descriptor: d }); return null; }
      emit('ii:action-dispatched', { descriptor: d });
      switch (d.kind) {
        case 'external-link':  executeExternalLink(d); break;
        case 'ide-deeplink':   executeIDEDeeplink(d); break;
        case 'clipboard':      return executeClipboard(d);
        case 'modal-trigger':  return executeModalTrigger(d);
        case 'drawer-trigger': executeDrawerTrigger(d); break;
        case 'htmx-post':      return executeHtmxPost(d, originEl);
        default:
          throw new Error('unknown action kind: ' + d.kind);
      }
      return null;
    }).catch(function (err) {
      emit('ii:action-error', { descriptor: d, error: String(err) });
      console.warn('incident-intel action failed', d, err);
    });
  }

  function onClick(ev) {
    var el = ev.target.closest('[data-ii-action]');
    if (!el) return;
    var d = parseDescriptor(el);
    if (!d) return;
    ev.preventDefault();
    dispatch(d, el);
  }

  // Bind any [data-ii-action][data-ii-hotkey] elements via the letter-key bus.
  // Hotkey takes effect when the surrounding [data-ii-scope] is focused.
  function bindHotkeys(root) {
    var nodes = (root || document).querySelectorAll('[data-ii-action]');
    for (var i = 0; i < nodes.length; i++) {
      var el = nodes[i];
      if (el.dataset.iiHotkeyBound === 'true') continue;
      var d = parseDescriptor(el);
      if (!d || !d.hotkey) continue;
      var scopeEl = el.closest('[data-ii-scope]');
      if (!scopeEl) continue;
      var scope = scopeEl.dataset.iiScope;
      try {
        (function (descriptor, origin) {
          window.IncidentIntel.registerKey(scope, descriptor.hotkey, function () {
            dispatch(descriptor, origin);
          }, descriptor.label || descriptor.id || descriptor.kind);
        })(d, el);
        el.dataset.iiHotkeyBound = 'true';
      } catch (e) {
        console.warn('incident-intel: hotkey registration failed', e);
      }
    }
  }

  function init() {
    document.addEventListener('click', onClick);
    bindHotkeys(document);
    // Rebind after htmx swaps so freshly-injected fragments work.
    document.body.addEventListener('htmx:afterSwap', function (ev) {
      bindHotkeys(ev.target);
    });
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }

  // Extend the IncidentIntel namespace published by Phase 1.
  if (!window.IncidentIntel) window.IncidentIntel = {};
  window.IncidentIntel.dispatchAction = dispatch;
  window.IncidentIntel.parseActionDescriptor = parseDescriptor;
  window.IncidentIntel.idePref = {
    read: readIDEPref,
    write: writeIDEPref,
    readRoot: readIDERoot,
    writeRoot: writeIDERoot,
    STORAGE_PREF: STORAGE_IDE_PREF,
    STORAGE_ROOT: STORAGE_IDE_ROOT
  };
})();
