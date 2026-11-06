// beads-viewer keyboard shortcuts.
//
// Pure client-side, ephemeral, no Go changes. See bead ka-0000001999.
//
// Shortcuts:
//   j / k         - move focus down / up the visible bead list
//   Enter / o     - open the focused bead
//   gg            - jump to top of the bead list
//   G             - jump to bottom of the bead list
//   /             - focus the search input (people search once /people ships — M2 §4)
//   Esc           - close inline-edit popover / blur input / close help overlay
//   c             - on bead-detail, focus the new-comment textarea
//   g h           - home (/)
//   g i           - in-progress / active surface (/active)
//   g + a/d/s/v/c - jump to /agents /decisions /triggers /advice /costs
//   ?             - toggle the help overlay
(function () {
  'use strict';

  var BASE = (window.BEADS_VIEWER_BASE_PATH || '').replace(/\/+$/, '');

  var ROW_SELECTOR = '.bead-row, .advice-row, .sched-row, .search-results tbody tr';
  var FOCUS_ATTR = 'data-kbd-focus';
  var FOCUS_CLASS = 'kbd-focus';

  function isEditable(el) {
    if (!el) return false;
    var tag = (el.tagName || '').toLowerCase();
    if (tag === 'input' || tag === 'textarea' || tag === 'select') return true;
    if (el.isContentEditable) return true;
    return false;
  }

  function visibleRows() {
    var rows = document.querySelectorAll(ROW_SELECTOR);
    var out = [];
    for (var i = 0; i < rows.length; i++) {
      var r = rows[i];
      // skip rows with no nav link (header rows on tables, etc.)
      if (!r.querySelector('a[href]')) continue;
      if (r.offsetParent === null) continue; // hidden
      out.push(r);
    }
    return out;
  }

  function clearFocus() {
    var prev = document.querySelectorAll('[' + FOCUS_ATTR + ']');
    for (var i = 0; i < prev.length; i++) {
      prev[i].removeAttribute(FOCUS_ATTR);
      prev[i].classList.remove(FOCUS_CLASS);
    }
  }

  function setFocusedRow(row) {
    clearFocus();
    if (!row) return;
    row.setAttribute(FOCUS_ATTR, '');
    row.classList.add(FOCUS_CLASS);
    if (typeof row.scrollIntoView === 'function') {
      row.scrollIntoView({ block: 'nearest' });
    }
  }

  function focusedIndex(rows) {
    for (var i = 0; i < rows.length; i++) {
      if (rows[i].hasAttribute(FOCUS_ATTR)) return i;
    }
    return -1;
  }

  function moveFocus(delta) {
    var rows = visibleRows();
    if (rows.length === 0) return;
    var idx = focusedIndex(rows);
    if (idx === -1) {
      idx = delta > 0 ? 0 : rows.length - 1;
    } else {
      idx = (idx + delta + rows.length) % rows.length;
    }
    setFocusedRow(rows[idx]);
  }

  function openFocused() {
    var rows = visibleRows();
    var idx = focusedIndex(rows);
    if (idx === -1) return;
    var link = rows[idx].querySelector('a[href]');
    if (link) link.click();
  }

  function gotoTop() {
    var rows = visibleRows();
    if (rows.length === 0) return;
    setFocusedRow(rows[0]);
  }

  function gotoBottom() {
    var rows = visibleRows();
    if (rows.length === 0) return;
    setFocusedRow(rows[rows.length - 1]);
  }

  // closeInlineEdit looks at the active element. If it sits inside an inline
  // edit popover (title/description/labels/owner block, or a status/priority/
  // assignee form), it triggers the matching Cancel button to restore the
  // read-only render. Returns true if it handled the keypress.
  var EDIT_BLOCK_SELECTOR = [
    '.title-edit-block',
    '.description-edit-block',
    '.labels-edit-block',
    '.owner-edit-block',
  ].join(',');
  var EDIT_FORM_SELECTOR = [
    '.bead-status-edit',
    '.bead-priority-edit',
    '.bead-assignee-edit',
  ].join(',');

  function closeInlineEdit(ae) {
    if (!ae) return false;
    var block = ae.closest && ae.closest(EDIT_BLOCK_SELECTOR);
    if (block) {
      var cancel = block.querySelector(
        '.bead-action-cancel-title, ' +
        '.bead-action-cancel-description, ' +
        '.bead-action-cancel-labels, ' +
        '.bead-action-cancel-owner'
      );
      if (cancel) {
        cancel.click();
        return true;
      }
    }
    var form = ae.closest && ae.closest(EDIT_FORM_SELECTOR);
    if (form) {
      // status/priority/assignee forms have no cancel button — blur the
      // active control so the user can dismiss without committing a change.
      if (typeof ae.blur === 'function') ae.blur();
      return true;
    }
    return false;
  }

  function focusSearch() {
    var input = document.querySelector('nav .nav-search input[name="q"]');
    if (!input) return;
    input.focus();
    if (typeof input.select === 'function') input.select();
  }

  function focusComment() {
    var ta = document.getElementById('comment-text');
    if (!ta) return;
    ta.focus();
    // Place caret at end if there is existing draft text.
    if (typeof ta.value === 'string') {
      var n = ta.value.length;
      try { ta.setSelectionRange(n, n); } catch (e) { /* some browsers reject on hidden */ }
    }
  }

  // ---- Help overlay ----
  var helpDialog = null;

  function ensureHelpDialog() {
    if (helpDialog) return helpDialog;
    helpDialog = document.createElement('dialog');
    helpDialog.className = 'kbd-help';
    helpDialog.innerHTML = [
      '<form method="dialog" class="kbd-help-form">',
      '<h2>Keyboard shortcuts</h2>',
      '<table class="kbd-help-table"><tbody>',
      '<tr><td><kbd>j</kbd> / <kbd>k</kbd></td><td>Move focus down / up the bead list</td></tr>',
      '<tr><td><kbd>Enter</kbd> / <kbd>o</kbd></td><td>Open the focused bead</td></tr>',
      '<tr><td><kbd>g</kbd> <kbd>g</kbd></td><td>Jump to top of list</td></tr>',
      '<tr><td><kbd>G</kbd></td><td>Jump to bottom of list</td></tr>',
      '<tr><td><kbd>/</kbd></td><td>Focus the search input</td></tr>',
      '<tr><td><kbd>Esc</kbd></td><td>Close inline edit / blur input / close this overlay</td></tr>',
      '<tr><td><kbd>c</kbd></td><td>Focus the new-comment textarea (bead detail)</td></tr>',
      '<tr><td><kbd>g</kbd> <kbd>h</kbd></td><td>Home</td></tr>',
      '<tr><td><kbd>g</kbd> <kbd>i</kbd></td><td>Active</td></tr>',
      '<tr><td><kbd>g</kbd> <kbd>a</kbd></td><td>Agents</td></tr>',
      '<tr><td><kbd>g</kbd> <kbd>d</kbd></td><td>Decisions</td></tr>',
      '<tr><td><kbd>g</kbd> <kbd>v</kbd></td><td>Advice</td></tr>',
      '<tr><td><kbd>g</kbd> <kbd>s</kbd></td><td>Triggers</td></tr>',
      '<tr><td><kbd>g</kbd> <kbd>c</kbd></td><td>Costs</td></tr>',
      '<tr><td><kbd>?</kbd></td><td>Toggle this help</td></tr>',
      '</tbody></table>',
      '<p class="kbd-help-actions"><button type="submit" autofocus>Close</button></p>',
      '</form>',
    ].join('');
    document.body.appendChild(helpDialog);
    return helpDialog;
  }

  function toggleHelp() {
    var d = ensureHelpDialog();
    if (d.open) {
      d.close();
    } else if (typeof d.showModal === 'function') {
      d.showModal();
    } else {
      d.setAttribute('open', '');
    }
  }

  // ---- "g" prefix ----
  var gPending = false;
  var gTimer = null;
  var GO_TARGETS = {
    a: '/agents',
    d: '/decisions',
    s: '/triggers',
    v: '/advice',
    c: '/costs',
    i: '/active',
    h: '/',
  };
  // GO_LOCAL handles two-key prefixes that act on the current page (e.g. `gg`
  // jumps to the top of the visible list) instead of navigating elsewhere.
  var GO_LOCAL = {
    g: gotoTop,
  };

  function clearGPrefix() {
    gPending = false;
    if (gTimer) {
      clearTimeout(gTimer);
      gTimer = null;
    }
  }

  function startGPrefix() {
    gPending = true;
    if (gTimer) clearTimeout(gTimer);
    gTimer = setTimeout(clearGPrefix, 1000);
  }

  function navigateTo(path) {
    window.location.href = BASE + path;
  }

  // ---- Main key handler ----
  document.addEventListener('keydown', function (e) {
    // Gate OFF on the /up surface. /up installs its own document-level keymap
    // (up.js) over the SAME keys this handler binds (?, c, j, g, /, Esc). Both
    // scripts load in the shared layout, so without this bail BOTH document
    // keydown listeners fire on one press — e.g. `?` opened two independent
    // <dialog>s (this one's .kbd-help AND up.js's .up-help). up.js self-gates
    // OFF on every non-/up page (its `[data-surface="up"]` presence check); this
    // is the symmetric mirror so up.js is the sole keyboard owner on /up
    // (ka-0000003390). Scoped to keydown: the nav "?" button and htmx-swap focus
    // upkeep are unaffected on other pages.
    if (document.querySelector('[data-surface="up"]')) return;

    // Always allow Esc to clear input focus or close help.
    if (e.key === 'Escape') {
      var d = helpDialog;
      if (d && d.open) {
        d.close();
        e.preventDefault();
        return;
      }
      var ae = document.activeElement;
      // Inline-edit popover (title/description/labels/owner/status/priority/
      // assignee) takes precedence over the generic blur path.
      if (closeInlineEdit(ae)) {
        e.preventDefault();
        clearGPrefix();
        return;
      }
      if (isEditable(ae)) {
        // If a search input has content, clear it on first Esc; blur on the
        // next Esc. Matches the "/" → type → Esc → Esc unwind that power
        // users expect from GitHub/Linear-style keyboarding.
        var isSearch = ae && ae.tagName && ae.tagName.toLowerCase() === 'input' &&
          (ae.type === 'search' || ae.name === 'q');
        if (isSearch && typeof ae.value === 'string' && ae.value.length > 0) {
          ae.value = '';
        } else {
          ae.blur();
        }
        e.preventDefault();
      }
      clearGPrefix();
      return;
    }

    if (isEditable(document.activeElement)) return;
    if (e.metaKey || e.ctrlKey || e.altKey) return;

    // Two-key "g" prefix.
    if (gPending) {
      var path = GO_TARGETS[e.key];
      var local = GO_LOCAL[e.key];
      clearGPrefix();
      if (local) {
        e.preventDefault();
        local();
        return;
      }
      if (path) {
        e.preventDefault();
        navigateTo(path);
      }
      return;
    }

    switch (e.key) {
      case 'j':
        e.preventDefault();
        moveFocus(1);
        return;
      case 'k':
        e.preventDefault();
        moveFocus(-1);
        return;
      case 'o':
      case 'Enter':
        if (focusedIndex(visibleRows()) !== -1) {
          e.preventDefault();
          openFocused();
        }
        return;
      case 'G':
        e.preventDefault();
        gotoBottom();
        return;
      case '/':
        e.preventDefault();
        focusSearch();
        return;
      case 'c':
        if (document.getElementById('comment-text')) {
          e.preventDefault();
          focusComment();
        }
        return;
      case 'g':
        e.preventDefault();
        startGPrefix();
        return;
      case '?':
        e.preventDefault();
        toggleHelp();
        return;
    }
  });

  // Keep focus highlight valid across htmx swaps that replace the row list.
  document.addEventListener('htmx:afterSwap', function () {
    var rows = visibleRows();
    if (focusedIndex(rows) === -1) clearFocus();
  });

  // Wire the nav "?" help button if present.
  document.addEventListener('DOMContentLoaded', function () {
    var btn = document.querySelector('.kbd-help-button');
    if (btn) {
      btn.addEventListener('click', function (ev) {
        ev.preventDefault();
        toggleHelp();
      });
    }
  });
})();
