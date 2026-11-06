// bundle_toc.js — flash-highlight a bundle doc card when the URL hash
// targets it (e.g. /bundle/<id>#doc-ka-XXX). Mirrors the typed-mention
// scroll/highlight pattern in highlight.js so the same yellow-flash
// idiom applies to TOC navigation in the bundle viewer.
//
// Behavior:
//  1. On DOMContentLoaded, parse `location.hash` for `#doc-<bead-id>`,
//     find the matching <article id="doc-<bead-id>">, scroll it into
//     view and flash `.bundle-doc-target` for ~3 seconds.
//  2. On `hashchange` (TOC link click), repeat the highlight so each
//     click visibly lands on the target card. The browser handles the
//     native scroll via the `id` attribute; this script only paints the
//     flash class.
//
// Tracked under T6 of epic ka-0000004611 (bead ka-0000007526).
(function () {
  'use strict';
  var HIGHLIGHT_MS = 3000;
  var HASH_PREFIX = '#doc-';

  function cssEscape(s) {
    if (typeof window.CSS !== 'undefined' && typeof window.CSS.escape === 'function') {
      return window.CSS.escape(s);
    }
    return String(s).replace(/[^A-Za-z0-9_-]/g, '');
  }

  function targetFromHash(hash) {
    if (!hash || hash.indexOf(HASH_PREFIX) !== 0) {
      return null;
    }
    var beadID = hash.slice(HASH_PREFIX.length);
    if (!beadID) {
      return null;
    }
    return document.getElementById('doc-' + beadID) ||
      document.querySelector('article.bundle-doc[data-doc-id="' + cssEscape(beadID) + '"]');
  }

  function flash(article, scroll) {
    article.classList.add('bundle-doc-target');
    window.setTimeout(function () {
      article.classList.remove('bundle-doc-target');
    }, HIGHLIGHT_MS);
    if (scroll) {
      try {
        article.scrollIntoView({ block: 'start', behavior: 'smooth' });
      } catch (_) {
        article.scrollIntoView();
      }
    }
  }

  function run() {
    var article = targetFromHash(window.location.hash);
    if (article) {
      flash(article, true);
    }
  }

  function onHashChange() {
    var article = targetFromHash(window.location.hash);
    if (article) {
      // Browser already handled the scroll via the anchor's href;
      // skip our own scroll so we don't override smooth-scroll mid-flight.
      flash(article, false);
    }
  }

  window.addEventListener('hashchange', onHashChange);

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', run);
  } else {
    run();
  }
})();
