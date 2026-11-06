// beads-viewer tab-title badge for new comments while page is backgrounded.
//
// Pure client-side, no server change. See bead ka-0000001438.
//
// Behavior:
//   - When the comments-section live-poll swaps in markup with one or more
//     `.comment-new` rows AND the tab is not visible
//     (`document.visibilityState !== 'visible'`), prefix `document.title`
//     with `(N) ` where N is the count of `.comment-new` rows in the DOM.
//   - On `visibilitychange` back to visible, strip the `(N) ` prefix.
//   - Server-rendered title is not modified at the source — the next full
//     navigation re-renders the original title.
(function () {
  'use strict';

  // Strict prefix shape we own: "(<digits>) " at the very start.
  var BADGE_RE = /^\(\d+\)\s/;

  function stripBadge(title) {
    return title.replace(BADGE_RE, '');
  }

  function applyBadge(n) {
    var base = stripBadge(document.title);
    document.title = '(' + n + ') ' + base;
  }

  function clearBadge() {
    var stripped = stripBadge(document.title);
    if (stripped !== document.title) document.title = stripped;
  }

  function countNew() {
    return document.querySelectorAll('.comment-new').length;
  }

  function isHidden() {
    // Defensive — older browsers without the Page Visibility API are treated
    // as "visible" so we never badge a foreground tab.
    return typeof document.visibilityState === 'string' &&
      document.visibilityState !== 'visible';
  }

  // After every htmx swap (the comments partial is the one that matters),
  // re-evaluate. countNew() spans the whole document, so a swap to an
  // unrelated partial still produces the correct N as long as the comments
  // section's `.comment-new` rows are still in the DOM.
  document.addEventListener('htmx:afterSwap', function () {
    if (!isHidden()) return;
    var n = countNew();
    if (n > 0) applyBadge(n);
  });

  // Tab focus → drop the badge. New-class CSS animations have their own
  // 1.5s timeout in layout.html so they fade naturally; we only own the
  // title prefix here.
  document.addEventListener('visibilitychange', function () {
    if (document.visibilityState === 'visible') clearBadge();
  });
})();
