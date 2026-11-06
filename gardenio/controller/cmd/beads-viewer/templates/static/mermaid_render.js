// beads-viewer mermaid diagram driver — plan step 2e (ka-0000005977).
//
// Server-side, a ```mermaid fence renders (mermaid.go) as:
//   <pre class="mermaid-diagram" data-mermaid="1"><code class="language-mermaid">ESCAPED SOURCE</code></pre>
// which is a readable code block with zero JS (the no-JS / lib-load-failure /
// parse-error degradation). This driver promotes each such block into inline
// SVG by lazy-loading the vendored mermaid library ONLY when a diagram is
// present on the page.
//
// VENDORED LIB: /assets/static/mermaid.min.js — mermaid v11.16.0 UMD
//   (sha256 74d7c46dabca328c2294733910a8aa1ed0c37451776e8d5295da38a2b758fb9b).
//   Sets globalThis.mermaid. ~975 KB gzipped, so it is NEVER script-tagged —
//   it is fetched on demand here, keeping diagram-free pages free of the cost.
//   Fetched at build time by cmd/beads-viewer/fetch-vendored.sh, which holds
//   the version and sha256 pins; not committed (gd-wNQzR9ptZI).
//   Size/version guards: mermaid_asset_test.go.
//
// SECURITY (see mermaid.go for the full two-layer argument):
//   - The diagram source reaches this driver only as an already-HTML-escaped
//     <code> text node; we read it via code.textContent (the browser-decoded
//     literal) and hand the STRING to mermaid — raw author markup is never a
//     live DOM element at any instant.
//   - mermaid runs securityLevel:'strict' + htmlLabels:false, so its output SVG
//     is DOMPurify-sanitised, HTML-in-labels is off, and click-directive JS is
//     refused. A diagram-embedded %%{init:{securityLevel:'loose'}}%% CANNOT
//     downgrade strict mode (securityLevel is in mermaid's immutable secure[]
//     deny-list; TestMermaidVersionPin guards the vendored bundle).
//   - We assign ONLY mermaid.render()'s returned SVG string via innerHTML, and
//     insert every error message via textContent — never author content as HTML,
//     no eval / new Function.
(function () {
  'use strict';

  var SELECTOR = 'pre.mermaid-diagram[data-mermaid="1"]:not([data-mermaid-done])';
  var counter = 0;
  var loadPromise = null; // memoized lazy load of the mermaid library
  var initialized = false;

  function basePath() {
    return (window.BEADS_VIEWER_BASE_PATH || '');
  }

  // prefersDark matches the viewer's active skin so diagrams read against the
  // same background as the surrounding UI. The viewer sets html[data-skin]; we
  // also honor the OS preference as a fallback.
  function prefersDark() {
    try {
      var skin = document.documentElement.getAttribute('data-skin');
      if (skin === 'dark') return true;
      if (skin === 'baseline' || skin === 'light') return false;
      return !!(window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches);
    } catch (_) {
      return false;
    }
  }

  // loadMermaid injects the vendored library exactly once and resolves when
  // globalThis.mermaid is available. Memoized so concurrent htmx swaps that each
  // call scan() do not double-inject the ~975KB script.
  function loadMermaid() {
    if (loadPromise) return loadPromise;
    if (window.mermaid) {
      loadPromise = Promise.resolve(window.mermaid);
      return loadPromise;
    }
    loadPromise = new Promise(function (resolve, reject) {
      var s = document.createElement('script');
      s.src = basePath() + '/assets/static/mermaid.min.js';
      s.async = true;
      s.onload = function () {
        if (window.mermaid) resolve(window.mermaid);
        else reject(new Error('mermaid global missing after load'));
      };
      s.onerror = function () { reject(new Error('failed to load mermaid.min.js')); };
      document.head.appendChild(s);
    });
    return loadPromise;
  }

  function initOnce(mermaid) {
    if (initialized) return;
    initialized = true;
    mermaid.initialize({
      startOnLoad: false,          // WE drive rendering; mermaid never auto-scans.
      securityLevel: 'strict',     // DOMPurify-sanitised SVG, no click-JS, no HTML labels.
      htmlLabels: false,
      flowchart: { htmlLabels: false },
      theme: prefersDark() ? 'dark' : 'default',
      fontFamily: 'inherit',
    });
  }

  // renderError degrades one diagram to its readable source plus a visible error
  // note, without crashing the page or leaving a spinner (there is no spinner
  // state — the <pre> source is visible until a render succeeds and replaces it).
  // Every dynamic string is inserted via textContent; author/library-supplied
  // text is NEVER assigned as HTML.
  function renderError(node, id, message) {
    node.setAttribute('data-mermaid-done', '1');
    node.classList.add('mermaid-error-block');
    // Remove any transient measurement node mermaid may leave in <body> on a
    // parse failure so failed diagrams don't accumulate orphans across swaps.
    ['', 'd'].forEach(function (p) {
      var stray = document.getElementById(p + id);
      if (stray && stray.parentNode) stray.parentNode.removeChild(stray);
    });
    if (node.querySelector('.mermaid-error')) return; // already annotated
    var note = document.createElement('div');
    note.className = 'mermaid-error';
    note.setAttribute('role', 'alert');
    note.textContent = 'Diagram failed to render: ' + (message || 'unknown error');
    node.appendChild(note);
  }

  function renderOne(mermaid, node) {
    var code = node.querySelector('code');
    if (!code) { node.setAttribute('data-mermaid-done', '1'); return; }
    var src = code.textContent; // browser-decoded literal mermaid source
    var id = 'mmd-' + (counter++);
    // Guard idempotency: mark before the async render so a re-entrant scan()
    // (htmx swap) never double-processes this node.
    node.setAttribute('data-mermaid-done', '1');
    var parsed;
    try {
      // parse() throws synchronously on bad syntax; catch here so a broken
      // diagram degrades cleanly rather than rejecting below.
      parsed = mermaid.parse(src);
    } catch (err) {
      renderError(node, id, err && err.message ? err.message : String(err));
      return;
    }
    Promise.resolve(parsed)
      .then(function () { return mermaid.render(id, src); })
      .then(function (out) {
        // out.svg is the DOMPurify-sanitised SVG string; assigning it is the
        // ONLY innerHTML write and it never carries un-sanitised author markup.
        node.innerHTML = out.svg;
        node.classList.add('mermaid-rendered');
        if (typeof out.bindFunctions === 'function') {
          // No-op under securityLevel:'strict' (no interactive callbacks), but
          // call it so future non-strict features would attach correctly.
          try { out.bindFunctions(node); } catch (_) { /* ignore */ }
        }
      })
      .catch(function (err) {
        renderError(node, id, err && err.message ? err.message : String(err));
      });
  }

  // scan finds un-rendered diagrams and, only if any exist, lazy-loads the lib
  // and renders each. Safe to call repeatedly (idempotent via data-mermaid-done).
  function scan() {
    var nodes = document.querySelectorAll(SELECTOR);
    if (nodes.length === 0) return; // LAZY GATE: no diagram → fetch nothing.
    loadMermaid().then(function (mermaid) {
      initOnce(mermaid);
      Array.prototype.forEach.call(nodes, function (node) {
        renderOne(mermaid, node);
      });
    }).catch(function (err) {
      // Library unavailable (404 / offline): degrade every pending diagram to
      // its readable source with an error note. The <pre> source stays visible.
      Array.prototype.forEach.call(nodes, function (node) {
        renderError(node, 'mmd-' + (counter++), (err && err.message) || 'diagram engine unavailable');
      });
    });
  }

  // Expose for any caller that swaps markdown in dynamically.
  window.__mermaidScan = scan;

  // htmx swaps can inject descriptions/comments containing diagrams after load.
  document.addEventListener('htmx:afterSwap', scan);

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', scan);
  } else {
    scan();
  }
})();
