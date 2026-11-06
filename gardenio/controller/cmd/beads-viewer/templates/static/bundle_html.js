// beads-viewer bundle html-doc enhancer.
//
// Beads: ka-0000004212 (auto-resize), ka-0000006557 (in-frame hover previews).
// Plan-of-record: plans/daily-bundle-productionization.md
//
// A bundle "html" doc renders inside an <iframe class="bundle-html"
// sandbox="allow-same-origin" srcdoc="…">. The frame is script-free (no
// allow-scripts — that is the XSS defense), so everything here runs in the
// PARENT and reaches into the frame via its same-origin contentDocument.
//
// Two jobs:
//   1. Auto-resize: grow each iframe to its document's scrollHeight so the
//      bundle page scrolls as one document — no inner scrollbar.
//   2. Hover previews: bind bead-link hover INSIDE each frame and drive the
//      shared popover (preview_popover.js, window.__beadPreview), translating
//      the link rect from frame coordinates into the parent viewport.
//
// Same-origin access can throw (cross-origin or not-yet-loaded). Every frame
// reach is wrapped; a failure degrades to the fixed min-height + no preview,
// never a console error storm.
(function () {
  'use strict';

  var RESIZE_PAD = 4; // px slack so the last line never clips into a scrollbar

  function frameDoc(iframe) {
    try {
      return iframe.contentDocument || (iframe.contentWindow && iframe.contentWindow.document) || null;
    } catch (_) {
      return null; // cross-origin or detached — give up on this frame.
    }
  }

  // resize sets the iframe height to its content height. Idempotent; safe to
  // call repeatedly (load, ResizeObserver, image loads, window resize).
  function resize(iframe) {
    var doc = frameDoc(iframe);
    if (!doc || !doc.body) return;
    var h = 0;
    try {
      var de = doc.documentElement;
      h = Math.max(
        doc.body.scrollHeight, doc.body.offsetHeight,
        de ? de.scrollHeight : 0, de ? de.offsetHeight : 0
      );
    } catch (_) { return; }
    if (h > 0) iframe.style.height = (h + RESIZE_PAD) + 'px';
  }

  // bindPreviews wires bead-link hover inside one frame to the shared popover.
  // A bead link in the frame is any <a> whose resolved pathname is `/ka-XXX`
  // (the newspaper authors relative /kd- hrefs). We hand preview_popover.js a
  // synthetic anchor with an ABSOLUTE href and a parent-translated rect.
  function bindPreviews(iframe) {
    var api = window.__beadPreview;
    if (!api) return; // popover script not loaded — previews simply absent.
    var doc = frameDoc(iframe);
    if (!doc || doc.__bundlePreviewBound) return;
    doc.__bundlePreviewBound = true;

    // Translate a frame-local rect into the parent viewport by adding the
    // iframe's own viewport offset. Recomputed live so scroll/resize reposition
    // correctly.
    function synthFor(a) {
      var abs = a.href; // resolved against the frame's base = the viewer origin
      return {
        href: abs,
        closest: function () { return null; },
        getBoundingClientRect: function () {
          var fr = iframe.getBoundingClientRect();
          var lr = a.getBoundingClientRect();
          return {
            left: fr.left + lr.left, top: fr.top + lr.top,
            right: fr.left + lr.right, bottom: fr.top + lr.bottom,
            width: lr.width, height: lr.height,
          };
        },
      };
    }

    doc.addEventListener('mouseover', function (e) {
      var a = e.target.closest && e.target.closest('a');
      if (!a) return;
      var synth = synthFor(a);
      if (!api.isBeadLink(synth)) return;
      api.scheduleShow(synth);
    });
    doc.addEventListener('mouseout', function (e) {
      var a = e.target.closest && e.target.closest('a');
      if (!a) return;
      var synth = synthFor(a);
      if (!api.isBeadLink(synth)) return;
      api.cancelShow();
      if (api.isOpen()) api.scheduleHide(); else api.hide();
    });
  }

  function enhance(iframe) {
    resize(iframe);
    bindPreviews(iframe);
    // Re-measure after late layout (web fonts, images decoding inside srcdoc).
    var doc = frameDoc(iframe);
    if (doc && typeof ResizeObserver === 'function' && doc.body) {
      try {
        var ro = new ResizeObserver(function () { resize(iframe); });
        ro.observe(doc.body);
      } catch (_) { /* ignore */ }
    }
  }

  function init() {
    var frames = document.querySelectorAll('iframe.bundle-html');
    Array.prototype.forEach.call(frames, function (iframe) {
      // srcdoc frames usually parse before this runs, but guard both paths.
      if (frameDoc(iframe) && frameDoc(iframe).readyState === 'complete') {
        enhance(iframe);
      } else {
        iframe.addEventListener('load', function () { enhance(iframe); }, { once: true });
        // Fallback in case 'load' already fired for a srcdoc frame.
        setTimeout(function () { enhance(iframe); }, 0);
      }
    });
  }

  // Reposition-safe: the popover repositions itself on window scroll/resize via
  // its own listeners (it re-reads the synthetic anchor's live rect), so we
  // only need to re-measure heights on window resize.
  window.addEventListener('resize', function () {
    var frames = document.querySelectorAll('iframe.bundle-html');
    Array.prototype.forEach.call(frames, resize);
  });

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
