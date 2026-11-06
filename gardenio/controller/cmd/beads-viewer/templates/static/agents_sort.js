// agents_sort.js — click-to-sort for the /agents compact table's Context column
// (ka-0000006399). The operator asked to front context-window occupancy in an
// obvious way; alongside the always-visible gauge, the Context column header is
// clickable to rank agents by how full their context window is.
//
// Scope: each project group renders its own <table class="agents-table">, so a
// click sorts only the tbody of the table whose header was clicked. Sorting is
// purely client-side (reorders existing rows); it does not refetch. The SSE
// live-update path (agents_stream.js) inserts/moves rows independently — a
// re-sort is a one-shot manual action, not a sticky mode, so the two never race.
(function () {
  'use strict';

  function sortKeyFor(th) {
    return th.getAttribute('data-sort-key') || '';
  }

  // ctxPct reads the numeric sort value off a row for the ctx column. Rows with
  // no reported context carry data-ctx-pct="0" and sort to the bottom on desc.
  function ctxPct(row) {
    var cell = row.querySelector('td.agents-table-ctx');
    if (!cell) return -1;
    var v = parseInt(cell.getAttribute('data-ctx-pct'), 10);
    return isNaN(v) ? -1 : v;
  }

  function sortTable(table, th) {
    var tbody = table.tBodies[0];
    if (!tbody) return;
    // Toggle direction: first click on a header sorts descending (fullest
    // first — the operator wants to spot near-full contexts), then flips.
    var current = th.getAttribute('aria-sort');
    var desc = current !== 'descending';
    th.setAttribute('aria-sort', desc ? 'descending' : 'ascending');

    // Clear the sort indicator on any sibling headers so only the active
    // column shows an arrow.
    var heads = th.parentNode ? th.parentNode.querySelectorAll('th[data-sort-key]') : [];
    for (var i = 0; i < heads.length; i++) {
      if (heads[i] !== th) heads[i].setAttribute('aria-sort', 'none');
    }

    var rows = Array.prototype.slice.call(tbody.rows);
    rows.sort(function (a, b) {
      var av = ctxPct(a);
      var bv = ctxPct(b);
      if (av === bv) return 0;
      return desc ? bv - av : av - bv;
    });
    // Re-append in sorted order; appendChild moves existing nodes.
    for (var j = 0; j < rows.length; j++) {
      tbody.appendChild(rows[j]);
    }
  }

  function onActivate(th) {
    if (sortKeyFor(th) !== 'ctx') return;
    var table = th.closest ? th.closest('table.agents-table') : null;
    if (table) sortTable(table, th);
  }

  // The sort control is a real <button> inside the columnheader <th>, so it is
  // natively focusable and its Enter/Space activation dispatches a click — this
  // one listener covers both mouse and keyboard. We keep aria-sort on the <th>
  // (a native columnheader, which supports it — a role="button" th would not,
  // and axe flags that as a critical aria-allowed-attr violation).
  document.addEventListener('click', function (ev) {
    var th = ev.target && ev.target.closest ? ev.target.closest('th.agents-th-sort') : null;
    if (th) onActivate(th);
  });
})();
