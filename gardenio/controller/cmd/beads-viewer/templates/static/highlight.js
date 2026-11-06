// highlight.js — flash-highlight a mention anchor inside the description
// block when the URL carries ?highlight=<bead-id>[&rel=<verb>]. Triggered
// by the constellation edge click handler (see constellation.js) so that
// clicking a typed mention edge in the graph lands on the source bead and
// scrolls the inline ka-XXX into view with a brief yellow flash.
//
// Behavior:
//  1. Parse `highlight` (target bead id) and optional `rel` (verb).
//  2. Find the first matching <a class="mention" data-bead-id="<id>">
//     inside #description-block. If `rel` is supplied, prefer the typed
//     match (data-relationship-type="<rel>"); otherwise fall back to any
//     match.
//  3. Add `.mention-highlight` for ~3 seconds, then remove. Scroll the
//     anchor into view (block: center), smoothly unless the reader asked
//     for prefers-reduced-motion (an explicit `behavior: 'smooth'` ignores
//     the CSS scroll-behavior net, so the choice has to be made here).
//  4. If no match (mention may live in notes / a comment), scroll the
//     description heading into view and console.warn for debugging.
//
// Out of scope (tracked as follow-ups in the E2 spec):
//  - Highlighting mentions inside comments (loaded via htmx fragments).
//  - Highlighting mentions inside notes.
(function () {
  'use strict';
  var HIGHLIGHT_MS = 3000;

  function findAnchor(scope, targetID, rel) {
    if (!scope || !targetID) {
      return null;
    }
    if (rel) {
      var typed = scope.querySelector(
        'a.mention[data-bead-id="' + cssEscape(targetID) + '"][data-relationship-type="' + cssEscape(rel) + '"]'
      );
      if (typed) {
        return typed;
      }
    }
    return scope.querySelector('a.mention[data-bead-id="' + cssEscape(targetID) + '"]');
  }

  function cssEscape(s) {
    if (typeof window.CSS !== 'undefined' && typeof window.CSS.escape === 'function') {
      return window.CSS.escape(s);
    }
    // Fallback: only allow [A-Za-z0-9_-] from bead IDs / verbs; everything
    // else gets stripped. Bead IDs and verbs are constrained to that set
    // server-side (see kbeads/internal/references/typed.go) so this is
    // safe.
    return String(s).replace(/[^A-Za-z0-9_-]/g, '');
  }

  // scrollBehavior picks the scrollIntoView behavior from the reader's
  // motion preference, as plan_focus_step.js does.
  function scrollBehavior() {
    var reduce = window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
    return reduce ? 'auto' : 'smooth';
  }

  function flash(anchor) {
    anchor.classList.add('mention-highlight');
    window.setTimeout(function () {
      anchor.classList.remove('mention-highlight');
    }, HIGHLIGHT_MS);
    try {
      anchor.scrollIntoView({ block: 'center', behavior: scrollBehavior() });
    } catch (_) {
      anchor.scrollIntoView();
    }
  }

  function run() {
    var params;
    try {
      params = new URLSearchParams(window.location.search);
    } catch (_) {
      return;
    }
    var targetID = params.get('highlight');
    if (!targetID) {
      return;
    }
    var rel = params.get('rel') || '';
    var scope = document.getElementById('description-block');
    if (!scope) {
      return;
    }
    var anchor = findAnchor(scope, targetID, rel);
    if (anchor) {
      flash(anchor);
      return;
    }
    // No inline anchor — mention may live in notes or a comment, both of
    // which are out of scope for E2. Scroll the description heading into
    // view as a graceful fallback and warn for debugging.
    if (window.console && typeof window.console.warn === 'function') {
      window.console.warn(
        'highlight.js: no inline mention anchor for ' + targetID +
        (rel ? ' (rel=' + rel + ')' : '') +
        ' inside #description-block — may live in notes or a comment.'
      );
    }
    try {
      scope.scrollIntoView({ block: 'start', behavior: scrollBehavior() });
    } catch (_) {
      scope.scrollIntoView();
    }
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', run);
  } else {
    run();
  }
})();
