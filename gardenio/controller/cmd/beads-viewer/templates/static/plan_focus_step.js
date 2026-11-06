// plan_focus_step.js — highlight + scroll to a deep-linked plan step
// (Matt's ask, ka-0000000150: "when you click the in-progress item from the
// index page it should send you to the plan but highlight the active task you
// clicked on for ~10 seconds and scroll to it").
//
// The /plans index card's "now / next" chip links to
// /plans/{id}#step-{stepID}. On arrival this shim reads that hash, scrolls the
// matching li#step-{stepID} into view, and paints a highlight class on it for
// FOCUS_MS. The browser's native fragment jump would land the row at the very
// top edge with no visual emphasis and no re-scroll if the timeline re-renders;
// this gives the deliberate "here's the task you clicked" cue instead.
//
// Survives live re-renders: the plan cockpit's SSE stream (plan_events.js)
// swaps the WHOLE #plan-steps section outerHTML whenever a child bead changes
// (a step closing, an assignee flip), which would drop a highlight painted on
// the old DOM node. While the focus window is still open we re-apply the class
// on every htmx swap so the emphasis rides through a mid-window re-render, and
// clear it when the window elapses. No dependency on plan_events.js — a plain
// deep-link with SSE unavailable still highlights on first paint.
(function () {
  'use strict';

  if (typeof document === 'undefined' || typeof document.getElementById !== 'function') {
    return;
  }

  var FOCUS_MS = 10000; // ~10s per Matt's ask
  var FOCUS_CLASS = 'plan-tl-step-focus';

  // The step id currently being focused (kd-… bead id), and the wall-clock
  // deadline after which the highlight is dropped. focusUntil==0 means no
  // active focus window.
  var focusStepID = '';
  var focusUntil = 0;
  var clearTimer = null;

  // stepIDFromHash returns the bead id embedded in a #step-<id> location hash,
  // or "" when the hash is absent / a different anchor (e.g. #decisions).
  function stepIDFromHash() {
    var h = (window.location && window.location.hash) || '';
    if (h.indexOf('#step-') !== 0) return '';
    return h.slice('#step-'.length);
  }

  // applyFocus paints the highlight on the row for the currently-focused step,
  // if it is present in the DOM. Idempotent: re-running it (e.g. after an SSE
  // swap replaced the node) re-attaches the class to the fresh node. Returns
  // the row element when found so the initial call can also scroll it.
  function applyFocus() {
    if (!focusStepID) return null;
    var row = document.getElementById('step-' + focusStepID);
    if (!row) return null;
    if (!row.classList.contains(FOCUS_CLASS)) {
      row.classList.add(FOCUS_CLASS);
    }
    return row;
  }

  // clearFocus ends the focus window and strips the highlight from whichever
  // node currently carries it (the row may have been swapped since it was set).
  function clearFocus() {
    focusUntil = 0;
    if (clearTimer) { clearTimeout(clearTimer); clearTimer = null; }
    var id = focusStepID;
    focusStepID = '';
    if (!id) return;
    var row = document.getElementById('step-' + id);
    if (row) row.classList.remove(FOCUS_CLASS);
  }

  // beginFocus opens a focus window for stepID: highlights + scrolls the row
  // and schedules the clear FOCUS_MS later. Safe to call when the row is not
  // yet in the DOM — the highlight is (re)applied on the next htmx swap.
  function beginFocus(stepID) {
    if (!stepID) return;
    // Reset any prior window (a second click on another card before the first
    // elapsed) so only the newly-clicked step stays highlighted.
    clearFocus();
    focusStepID = stepID;
    focusUntil = now() + FOCUS_MS;

    var row = applyFocus();
    if (row && typeof row.scrollIntoView === 'function') {
      var reduce = window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
      try {
        row.scrollIntoView({ behavior: reduce ? 'auto' : 'smooth', block: 'center' });
      } catch (e) {
        // Older browsers: scrollIntoView with no options still centers roughly.
        try { row.scrollIntoView(); } catch (e2) { /* ignore */ }
      }
    }

    clearTimer = setTimeout(clearFocus, FOCUS_MS);
  }

  function now() {
    return (typeof Date.now === 'function') ? Date.now() : new Date().getTime();
  }

  // Re-apply the highlight after any htmx swap while the window is still open —
  // the SSE #plan-steps re-render (plan_events.js swapById) replaces the row's
  // DOM node, dropping the class we painted. Registered once; cheap no-op when
  // no focus is active.
  document.addEventListener('htmx:afterSwap', function () {
    if (!focusStepID || focusUntil === 0) return;
    if (now() >= focusUntil) { clearFocus(); return; }
    applyFocus();
  });

  // The SSE step swap in plan_events.js is a vanilla replaceChild, not an htmx
  // request, so it does NOT fire htmx:afterSwap. plan_events.js applies a flash
  // class on the swapped #plan-steps; observe that node's replacement via a
  // MutationObserver on its parent so the highlight rides SSE re-renders too.
  function observeStepsSwaps() {
    if (typeof MutationObserver !== 'function') return;
    var steps = document.getElementById('plan-steps');
    if (!steps || !steps.parentNode) return;
    var obs = new MutationObserver(function () {
      if (!focusStepID || focusUntil === 0) return;
      if (now() >= focusUntil) { clearFocus(); return; }
      applyFocus();
    });
    obs.observe(steps.parentNode, { childList: true, subtree: true });
  }

  // Kick off on load from the arriving hash, and on any in-page hashchange
  // (e.g. the operator clicks a second index card in another tab that shares
  // this document — rare, but hashchange is the honest trigger).
  function start() {
    observeStepsSwaps();
    var id = stepIDFromHash();
    if (id) beginFocus(id);
  }

  window.addEventListener('hashchange', function () {
    var id = stepIDFromHash();
    if (id) beginFocus(id);
  });

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', start);
  } else {
    start();
  }
})();
