// beads-viewer per-comment permalink anchors.
//
// Pure client-side. See bead ka-0000001537.
//
// Behavior:
//   - On click of `a.comment-permalink`, copy the absolute URL of this
//     comment (current page + `#comment-<id>`) to the clipboard.
//   - Default navigation is preserved so the URL fragment updates and the
//     browser scrolls to the anchored comment.
//   - On success, briefly toggle `.copy-feedback` on the anchor for ~1s
//     so CSS can highlight it. On failure, toggle `.copy-failed`.
(function () {
  'use strict';

  function flash(el, cls) {
    el.classList.add(cls);
    if (el._linkResetTimer) clearTimeout(el._linkResetTimer);
    el._linkResetTimer = setTimeout(function () {
      el.classList.remove(cls);
      el._linkResetTimer = null;
    }, 1000);
  }

  function absoluteURL(id) {
    return window.location.origin + window.location.pathname + '#comment-' + id;
  }

  document.addEventListener('click', function (evt) {
    var a = evt.target.closest && evt.target.closest('a.comment-permalink');
    if (!a) return;
    var id = a.getAttribute('data-comment-id') || '';
    if (!id) return;
    if (!navigator.clipboard || !navigator.clipboard.writeText) {
      flash(a, 'copy-failed');
      return;
    }
    navigator.clipboard.writeText(absoluteURL(id)).then(
      function () { flash(a, 'copy-feedback'); },
      function () { flash(a, 'copy-failed'); }
    );
  });
})();
