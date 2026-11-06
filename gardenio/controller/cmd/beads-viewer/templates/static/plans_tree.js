// plans_tree.js — the client half of the /plans zoomable forest (?view=tree;
// plans_tree.go, plan_tree.html, thread ka-0000005850).
//
// The server already renders every tree into ONE normalized viewBox, so a zoom
// level is nothing but a class on the .plans-forest container. This file makes
// that flip instant: intercept the zoom chip click, swap the class, push the
// ?zoom= into history. No fetch, no re-layout, no re-render.
//
// PROGRESSIVE ENHANCEMENT is the contract. Every zoom chip is a real <a> with a
// real ?zoom= URL that the server honors (normalizePlanZoom), so with JS off,
// blocked, or broken, the control still works — it just costs a page load. This
// file only ever REMOVES that page load; it never becomes required.
//
// Keyboard: +/- step the zoom in/out, matching the muscle memory of every other
// zoomable surface. Bound on the document only while a forest is on the page,
// and skipped whenever focus is in a text input so it can never eat typing.
(function () {
  'use strict';

  var LEVELS = ['canopy', 'grove', 'tree'];

  function forest() {
    return document.querySelector('.plans-forest');
  }

  // apply swaps the container class and syncs the chips' active state. It is the
  // single place the DOM changes, so the class, the chip highlight and the URL
  // can never drift apart.
  function apply(level, push) {
    var el = forest();
    if (!el || LEVELS.indexOf(level) === -1) return;
    LEVELS.forEach(function (l) { el.classList.remove('plans-forest-' + l); });
    el.classList.add('plans-forest-' + level);
    el.setAttribute('data-forest-zoom-level', level);

    document.querySelectorAll('[data-forest-zoom]').forEach(function (chip) {
      var active = chip.getAttribute('data-forest-zoom').toLowerCase() === level;
      chip.classList.toggle('chip-active', active);
      if (active) chip.setAttribute('aria-current', 'true');
      else chip.removeAttribute('aria-current');
    });

    if (push && window.history && window.history.replaceState) {
      var url = new URL(window.location.href);
      // Grove is the server's default, so its URL omits the key — keep the
      // client's URLs byte-identical to the ones the server would have minted.
      if (level === 'grove') url.searchParams.delete('zoom');
      else url.searchParams.set('zoom', level);
      window.history.replaceState({}, '', url.toString());
    }
  }

  function current() {
    var el = forest();
    if (!el) return 'grove';
    var lvl = el.getAttribute('data-forest-zoom-level');
    return LEVELS.indexOf(lvl) === -1 ? 'grove' : lvl;
  }

  function step(delta) {
    var i = LEVELS.indexOf(current()) + delta;
    if (i < 0) i = 0;
    if (i > LEVELS.length - 1) i = LEVELS.length - 1;
    apply(LEVELS[i], true);
  }

  function isTyping(target) {
    if (!target) return false;
    var tag = (target.tagName || '').toLowerCase();
    return tag === 'input' || tag === 'textarea' || tag === 'select' || target.isContentEditable;
  }

  function init() {
    if (!forest()) return;

    document.querySelectorAll('[data-forest-zoom]').forEach(function (chip) {
      chip.addEventListener('click', function (ev) {
        // Let a modified click (new tab / new window) behave natively.
        if (ev.metaKey || ev.ctrlKey || ev.shiftKey || ev.altKey || ev.button !== 0) return;
        var level = chip.getAttribute('data-forest-zoom').toLowerCase();
        if (LEVELS.indexOf(level) === -1) return;
        ev.preventDefault();
        apply(level, true);
      });
    });

    document.addEventListener('keydown', function (ev) {
      if (isTyping(ev.target) || ev.metaKey || ev.ctrlKey || ev.altKey) return;
      if (ev.key === '+' || ev.key === '=') { ev.preventDefault(); step(1); }
      else if (ev.key === '-' || ev.key === '_') { ev.preventDefault(); step(-1); }
    });
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
