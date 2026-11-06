// context_menu.js — right-click context menu on bead rows.
//
// Adds a small contextmenu handler on `.bead-row` elements that opens a
// floating menu with a "Spawn agent on bead(s)" action. The menu is
// selection-aware:
//
//   - If the right-clicked row is part of the current multi-select
//     (.project-row-select:checked, count >= 2 incl. this row): the
//     action targets the selection.
//   - Else: the action targets just the right-clicked row, without
//     mutating selection state.
//
// Closes on click-away, Esc, scroll, or another contextmenu.
//
// Action wiring: opens the existing /quick-spawn modal via htmx. For the
// multi-bead case, hidden `task_ids[]` inputs are injected into the
// modal form after the swap completes — the quick-spawn POST handler
// merges these with the visible single `task_id` input via the
// `task_ids[]` form key (see quick_spawn.go: collectTaskIDs).
//
// Keyboard fallback: a "Spawn agent on selection" button on
// #project-bulk-bar (data-bulk-spawn) drives the same action so users
// who can't right-click still have access. project_bulk.js shows the
// bar whenever >=1 row is selected.
//
// IIFE; no framework. Re-attaches automatically because the contextmenu
// listener is delegated from `document`, so htmx swaps don't unbind it.

(function () {
  'use strict';

  var BASE = (window.BEADS_VIEWER_BASE_PATH || '').replace(/\/+$/, '');
  var menuEl = null;

  function bar() {
    return document.getElementById('project-bulk-bar');
  }

  function projectFromPage() {
    var b = bar();
    if (b && b.getAttribute('data-project')) {
      return b.getAttribute('data-project');
    }
    // Fallback: parse /p/<project> from the URL.
    var m = window.location.pathname.match(/\/p\/([^\/?#]+)/);
    return m ? decodeURIComponent(m[1]) : '';
  }

  function checkedIDs() {
    var out = [];
    document
      .querySelectorAll('.project-row-select:checked')
      .forEach(function (cb) {
        if (cb.value) out.push(cb.value);
      });
    return out;
  }

  // Decide what bead IDs the action should target given the right-clicked
  // row (or null when invoked from the bulk-bar button).
  function targetIDsFor(row) {
    var sel = checkedIDs();
    if (!row) {
      return sel; // bulk-bar entry point — always selection.
    }
    var rowID = row.getAttribute('data-bead-id') || '';
    if (!rowID) return [];
    // If row is part of the current selection AND selection has >=2: act on
    // selection. Else: just the row.
    if (sel.length >= 2 && sel.indexOf(rowID) !== -1) {
      return sel;
    }
    return [rowID];
  }

  // ---- menu rendering ----------------------------------------------------

  function closeMenu() {
    if (menuEl && menuEl.parentNode) {
      menuEl.parentNode.removeChild(menuEl);
    }
    menuEl = null;
  }

  function clamp(n, lo, hi) {
    return Math.max(lo, Math.min(hi, n));
  }

  function buildMenu(ids) {
    var div = document.createElement('div');
    div.className = 'beads-context-menu';
    div.setAttribute('role', 'menu');

    var label =
      ids.length > 1
        ? 'Spawn agent on ' + ids.length + ' beads'
        : 'Spawn agent on ' + (ids[0] || 'bead');

    var spawnBtn = document.createElement('button');
    spawnBtn.type = 'button';
    spawnBtn.className = 'beads-context-menu-item';
    spawnBtn.setAttribute('role', 'menuitem');
    spawnBtn.textContent = label;
    spawnBtn.addEventListener('click', function (e) {
      e.preventDefault();
      e.stopPropagation();
      closeMenu();
      openSpawnModal(ids);
    });
    div.appendChild(spawnBtn);

    var cancelBtn = document.createElement('button');
    cancelBtn.type = 'button';
    cancelBtn.className = 'beads-context-menu-item beads-context-menu-cancel';
    cancelBtn.setAttribute('role', 'menuitem');
    cancelBtn.textContent = 'Cancel';
    cancelBtn.addEventListener('click', function (e) {
      e.preventDefault();
      e.stopPropagation();
      closeMenu();
    });
    div.appendChild(cancelBtn);

    return div;
  }

  function openMenu(x, y, ids) {
    closeMenu();
    if (!ids.length) return;
    var div = buildMenu(ids);
    // Render off-screen first to measure, then position clamped to viewport.
    div.style.position = 'fixed';
    div.style.left = '-9999px';
    div.style.top = '-9999px';
    document.body.appendChild(div);
    menuEl = div;

    var w = div.offsetWidth || 200;
    var h = div.offsetHeight || 80;
    var vw = window.innerWidth;
    var vh = window.innerHeight;
    var px = clamp(x, 0, Math.max(0, vw - w));
    var py = clamp(y, 0, Math.max(0, vh - h));
    div.style.left = px + 'px';
    div.style.top = py + 'px';

    // Focus first item for keyboard users.
    var first = div.querySelector('.beads-context-menu-item');
    if (first) first.focus();
  }

  // ---- spawn modal wiring ------------------------------------------------

  function openSpawnModal(ids) {
    if (!window.htmx || typeof window.htmx.ajax !== 'function') {
      return;
    }
    if (!ids.length) return;
    var project = projectFromPage();
    var first = ids[0];
    var url = BASE + '/quick-spawn?task=' + encodeURIComponent(first);
    if (project) url += '&project=' + encodeURIComponent(project);

    // For multi-bead, install a one-shot afterSwap listener to inject the
    // remaining IDs as hidden `task_ids[]` inputs into the modal form.
    if (ids.length > 1) {
      installMultiBeadInjector(ids);
    }

    window.htmx.ajax('GET', url, '#quick-spawn-host');
  }

  function installMultiBeadInjector(ids) {
    var host = document.getElementById('quick-spawn-host');
    if (!host) return;
    function onSwap(evt) {
      if (evt.target !== host) return;
      host.removeEventListener('htmx:afterSwap', onSwap);
      var form = host.querySelector('form.quick-spawn-form');
      if (!form) return;
      ids.forEach(function (id) {
        var input = document.createElement('input');
        input.type = 'hidden';
        input.name = 'task_ids[]';
        input.value = id;
        form.appendChild(input);
      });
      // Surface a small note so the operator knows the spawn targets the
      // full selection rather than just the visible task_id field.
      var note = document.createElement('p');
      note.className = 'quick-spawn-multi-note';
      note.textContent =
        'Spawning against selection of ' + ids.length + ' beads.';
      form.insertBefore(note, form.firstChild);
    }
    host.addEventListener('htmx:afterSwap', onSwap);
  }

  // ---- event wiring ------------------------------------------------------

  document.addEventListener('contextmenu', function (e) {
    var row = e.target && e.target.closest && e.target.closest('.bead-row');
    if (!row) {
      // Right-click outside any bead row closes any open menu.
      if (menuEl) closeMenu();
      return;
    }
    var ids = targetIDsFor(row);
    if (!ids.length) return;
    e.preventDefault();
    openMenu(e.clientX, e.clientY, ids);
  });

  document.addEventListener('click', function (e) {
    // Click anywhere (other than inside the menu itself) closes it.
    if (menuEl && !menuEl.contains(e.target)) {
      closeMenu();
    }
    // Bulk-bar fallback button.
    var btn = e.target && e.target.closest && e.target.closest('[data-bulk-spawn]');
    if (btn) {
      e.preventDefault();
      var ids = targetIDsFor(null);
      if (ids.length) openSpawnModal(ids);
    }
  });

  document.addEventListener('keydown', function (e) {
    if (e.key === 'Escape' && menuEl) {
      e.preventDefault();
      closeMenu();
    }
  });

  // Scroll closes the menu — its absolute coords go stale fast.
  window.addEventListener('scroll', closeMenu, true);
  window.addEventListener('resize', closeMenu);
})();
