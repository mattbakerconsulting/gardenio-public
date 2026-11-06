// beads-viewer per-bead unread-comment indicator on listing pages.
//
// Pure client-side. See bead ka-0000002605.
//
// Storage:
//   localStorage["bv:lastSeen:v1"] -> JSON object { "<beadId>": "<RFC3339Nano>", ... }
//
// On a bead-detail page (`#comments-section[data-bead-id]` is present), update
// `lastSeen[beadId]` to max of: existing entry, the section's
// `data-comments-latest`, and the bead's `data-bead-updated-at`. This collapses
// "I have seen the latest comment" and "I have seen this bead at this version"
// into a single high-water mark; either is enough to clear the dot for that
// bead on listings.
//
// On listings, every row carries `data-bead-id` and `data-last-activity-ts`.
// If `last-activity-ts > lastSeen[beadId]`, decorate the row with an unread
// dot. The decoration is idempotent and re-applied on htmx swaps so an
// auto-poll (`every 30s`) re-renders correctly.
//
// Cross-tab: a `storage` event in any other tab triggers a full re-decorate
// in this tab.
(function () {
  'use strict';

  var KEY = 'bv:lastSeen:v1';
  var DOT_CLASS = 'bead-row-unread-dot';
  var ROW_CLASS = 'bead-row-unread';

  function readMap() {
    try {
      var raw = localStorage.getItem(KEY);
      if (!raw) return {};
      var v = JSON.parse(raw);
      return (v && typeof v === 'object') ? v : {};
    } catch (e) {
      return {};
    }
  }

  function writeMap(m) {
    try {
      localStorage.setItem(KEY, JSON.stringify(m));
    } catch (e) {
      /* quota / private mode — silently drop; next visit recovers */
    }
  }

  // Returns true when `a` is later than `b`. Empty/invalid strings sort
  // before any real timestamp. RFC3339Nano values are lexicographically
  // ordered when normalized to UTC ("Z"), which is what server.go emits.
  function isLater(a, b) {
    if (!a) return false;
    if (!b) return true;
    return a > b;
  }

  function maxStamp(a, b) {
    return isLater(a, b) ? a : b;
  }

  // Bead-show writer: read the highest activity stamp visible on this page
  // and update lastSeen[beadId] if it advances.
  function recordSeenOnBeadShow() {
    var section = document.getElementById('comments-section');
    if (!section) return;
    var beadId = section.getAttribute('data-bead-id');
    if (!beadId) return;
    var commentsLatest = section.getAttribute('data-comments-latest') || '';
    var beadUpdated = section.getAttribute('data-bead-updated-at') || '';
    var newest = maxStamp(commentsLatest, beadUpdated);
    if (!newest) return;
    var m = readMap();
    if (isLater(newest, m[beadId])) {
      m[beadId] = newest;
      writeMap(m);
    }
  }

  // Listing-row decorator: walk every `[data-bead-id][data-last-activity-ts]`
  // and toggle the unread dot based on the current map. Listing rows that lack
  // a stamp (e.g. the bead has never been updated) are ignored — no dot.
  function decorateListingRows() {
    var rows = document.querySelectorAll('[data-bead-id][data-last-activity-ts]');
    if (!rows.length) return;
    var m = readMap();
    for (var i = 0; i < rows.length; i++) {
      var row = rows[i];
      // Skip the bead-show comments-section, which carries the same
      // attributes but is the writer side, not a listing row.
      if (row.id === 'comments-section') continue;
      var beadId = row.getAttribute('data-bead-id');
      var ts = row.getAttribute('data-last-activity-ts');
      var unread = !!ts && isLater(ts, m[beadId]);
      // <tr> cannot contain inline content directly — HTML5 foster-parents
      // it outside the table. Anchor the dot inside the first <td> instead.
      var isTableRow = row.tagName === 'TR';
      var host = isTableRow ? row.querySelector(':scope > td') : row;
      if (!host) continue;
      var existing = host.querySelector(':scope > .' + DOT_CLASS);
      if (unread) {
        row.classList.add(ROW_CLASS);
        if (!existing) {
          var dot = document.createElement('span');
          dot.className = DOT_CLASS;
          dot.setAttribute('role', 'img');
          dot.setAttribute('aria-label', 'unread updates');
          dot.title = 'New activity since you last viewed';
          host.insertBefore(dot, host.firstChild);
        }
      } else {
        row.classList.remove(ROW_CLASS);
        if (existing) existing.remove();
      }
    }
  }

  function runAll() {
    recordSeenOnBeadShow();
    decorateListingRows();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', runAll);
  } else {
    runAll();
  }

  // htmx swaps a partial in (e.g. /active row close, /decisions resolve, the
  // comments live-poll). Re-decorate after every swap so newly-injected rows
  // pick up dots and the bead-show writer runs again if the comments section
  // was the swap target.
  document.addEventListener('htmx:afterSwap', runAll);

  // Cross-tab: another tab marked a bead as seen → re-decorate here.
  window.addEventListener('storage', function (ev) {
    if (ev.key === KEY) decorateListingRows();
  });
})();
