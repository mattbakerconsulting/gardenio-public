// Command palette (Cmd-K / Ctrl-K) for beads-viewer.
//
// Bead: ka-0000005771
//
// Behavior:
//   - Cmd-K (macOS) / Ctrl-K toggles the overlay from any page.
//   - The overlay is built lazily on first open: a backdrop + a centered
//     <article> with an <input> and a <ul> for results. htmx is wired via
//     `htmx.process` so `hx-trigger="keyup changed delay:150ms"` debounces
//     suggestion fetches against /cmdk/suggest.
//   - Esc, click on backdrop, or a successful navigation closes the overlay
//     and restores focus to the previously-focused element.
//   - ArrowUp / ArrowDown move the active row; Enter follows the active
//     row's link. Tab is trapped inside the input (single tab stop) so
//     the user can't accidentally focus the underlying page.
//   - On `htmx:afterSwap` of the result fragment the first row is auto-
//     selected so Enter works without arrow-keying first.
(function () {
  'use strict';

  var BASE = (window.BEADS_VIEWER_BASE_PATH || '').replace(/\/+$/, '');

  var root = null;        // .cmdk-sheet wrapper
  var input = null;
  var resultsUL = null;
  var lastFocus = null;

  function isOpen() { return root && !root.hidden; }

  function build() {
    if (root) return root;
    root = document.createElement('div');
    root.className = 'cmdk-sheet';
    root.hidden = true;
    root.setAttribute('role', 'dialog');
    root.setAttribute('aria-modal', 'true');
    root.setAttribute('aria-label', 'Command palette');
    root.innerHTML =
      '<div class="cmdk-backdrop" data-cmdk-close></div>' +
      '<article class="cmdk-card">' +
      '  <input type="search" class="cmdk-input" autocomplete="off" spellcheck="false"' +
      '         placeholder="Jump to bead by id, slug, or title…"' +
      '         name="q"' +
      '         hx-get="' + BASE + '/cmdk/suggest"' +
      '         hx-trigger="keyup changed delay:150ms, focus"' +
      '         hx-target=".cmdk-results"' +
      '         hx-swap="innerHTML">' +
      '  <ul class="cmdk-results" role="listbox">' +
      '    <li class="cmdk-empty" role="presentation">Type to search beads…</li>' +
      '  </ul>' +
      '</article>';
    document.body.appendChild(root);
    input = root.querySelector('.cmdk-input');
    resultsUL = root.querySelector('.cmdk-results');

    // Wire htmx onto the freshly-built input.
    if (window.htmx && typeof window.htmx.process === 'function') {
      window.htmx.process(root);
    }

    root.addEventListener('click', function (e) {
      if (e.target && e.target.hasAttribute && e.target.hasAttribute('data-cmdk-close')) {
        close();
      }
      // Click on a result row: htmx is irrelevant — the <a> handles navigation.
      var li = e.target.closest && e.target.closest('.cmdk-item');
      if (li) close();
    });

    // After the suggestion fragment swaps, auto-select the first row so
    // Enter follows it without an arrow-key first.
    root.addEventListener('htmx:afterSwap', function (e) {
      if (e && e.target === resultsUL) selectFirst();
    });

    // Mouse hover updates the active row (matches typical palette UX).
    resultsUL.addEventListener('mousemove', function (e) {
      var li = e.target.closest && e.target.closest('.cmdk-item');
      if (li) setActive(li);
    });
    return root;
  }

  function open() {
    build();
    lastFocus = document.activeElement;
    root.hidden = false;
    input.value = '';
    resultsUL.innerHTML = '<li class="cmdk-empty" role="presentation">Type to search beads…</li>';
    // Focus next tick so any in-flight click that opened the palette
    // doesn't immediately steal focus back.
    setTimeout(function () { input.focus(); }, 0);
  }

  function close() {
    if (!root || root.hidden) return;
    root.hidden = true;
    if (lastFocus && typeof lastFocus.focus === 'function') {
      try { lastFocus.focus(); } catch (_) { /* ignore */ }
    }
    lastFocus = null;
  }

  function items() {
    if (!resultsUL) return [];
    return Array.prototype.slice.call(resultsUL.querySelectorAll('.cmdk-item'));
  }

  function activeItem() {
    if (!resultsUL) return null;
    return resultsUL.querySelector('.cmdk-item.cmdk-active');
  }

  function setActive(li) {
    var prev = activeItem();
    if (prev) prev.classList.remove('cmdk-active');
    if (li) {
      li.classList.add('cmdk-active');
      // Keep the active row visible inside the scrollable results pane.
      if (typeof li.scrollIntoView === 'function') {
        li.scrollIntoView({ block: 'nearest' });
      }
    }
  }

  function selectFirst() {
    var all = items();
    if (all.length) setActive(all[0]);
  }

  function move(delta) {
    var all = items();
    if (!all.length) return;
    var cur = activeItem();
    var idx = cur ? all.indexOf(cur) : -1;
    var next = idx < 0 ? 0 : (idx + delta + all.length) % all.length;
    setActive(all[next]);
  }

  function activate() {
    var li = activeItem() || items()[0];
    if (!li) return;
    var a = li.querySelector('a');
    if (a && a.href) {
      // Let the browser handle navigation (preserves history, modifier keys).
      window.location.href = a.href;
    }
  }

  document.addEventListener('keydown', function (e) {
    // Toggle: Cmd-K / Ctrl-K from anywhere.
    if ((e.metaKey || e.ctrlKey) && !e.altKey && !e.shiftKey && (e.key === 'k' || e.key === 'K')) {
      e.preventDefault();
      if (isOpen()) close(); else open();
      return;
    }
    if (!isOpen()) return;
    if (e.key === 'Escape') {
      e.preventDefault();
      close();
      return;
    }
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      move(1);
      return;
    }
    if (e.key === 'ArrowUp') {
      e.preventDefault();
      move(-1);
      return;
    }
    if (e.key === 'Enter') {
      e.preventDefault();
      activate();
      return;
    }
    if (e.key === 'Tab') {
      // Single tab stop: keep focus inside the palette.
      e.preventDefault();
      if (input) input.focus();
      return;
    }
  });
})();
