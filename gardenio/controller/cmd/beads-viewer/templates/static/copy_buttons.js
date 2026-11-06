// beads-viewer copy-to-clipboard buttons on bead_show.
//
// Pure client-side. See bead ka-0000003083.
//
// Behavior:
//   - On click of `.copy-btn[data-copy="id"]` copy `data-value` (the bead ID)
//     to the clipboard.
//   - On click of `.copy-btn[data-copy="permalink"]` copy
//     `${window.location.origin}${window.location.pathname}` (the bead's
//     absolute URL with no query/hash) to the clipboard.
//   - On success, briefly swap the button label to "Copied!" for ~1s and
//     toggle `.copy-feedback` so the CSS can highlight it.
//   - On failure (no clipboard API, denied permission, etc.), swap to
//     "Copy failed" for ~1s. We do not try to install a hidden-input
//     fallback — modern browsers all expose `navigator.clipboard` over
//     HTTPS, which is the only context this viewer is served from.
(function () {
  'use strict';

  function permalink() {
    return window.location.origin + window.location.pathname;
  }

  function valueFor(btn) {
    var kind = btn.getAttribute('data-copy');
    if (kind === 'permalink') return permalink();
    return btn.getAttribute('data-value') || '';
  }

  function flash(btn, label) {
    var original = btn.dataset.originalLabel || btn.textContent;
    btn.dataset.originalLabel = original;
    btn.textContent = label;
    btn.classList.add('copy-feedback');
    if (btn._copyResetTimer) clearTimeout(btn._copyResetTimer);
    btn._copyResetTimer = setTimeout(function () {
      btn.textContent = original;
      btn.classList.remove('copy-feedback');
      btn._copyResetTimer = null;
    }, 1000);
  }

  function copy(btn) {
    var value = valueFor(btn);
    if (!value) return;
    if (!navigator.clipboard || !navigator.clipboard.writeText) {
      flash(btn, 'Copy failed');
      return;
    }
    navigator.clipboard.writeText(value).then(
      function () { flash(btn, 'Copied!'); },
      function () { flash(btn, 'Copy failed'); }
    );
  }

  // Event delegation so this works without re-binding after htmx swaps.
  document.addEventListener('click', function (evt) {
    var btn = evt.target.closest && evt.target.closest('.copy-btn');
    if (!btn) return;
    evt.preventDefault();
    copy(btn);
  });
})();
