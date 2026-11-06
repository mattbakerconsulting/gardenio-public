// plans_board.js — the /plans card-peek drawer driver (Matt's ask, thread
// ka-0000005903: "when you click on a card the plan pops out from the side
// instead of being a whole new page; this will make it way easier to switch
// between plans").
//
// Contract with plans.html:
//   - #plan-peek-drawer / #plan-peek-backdrop are the static shell (hidden).
//   - #plan-peek-body is the mount the GET /plans/{id}/peek fragment lands in.
//   - Every plan card is an .plan-card <li> carrying data-plan-id.
//
// Behaviour:
//   - A plain left-click anywhere on a card opens the drawer for that plan.
//     Clicks on interactive descendants (buttons, inputs, selects, forms, and
//     links OTHER than the card's own title/id links) keep their native
//     behaviour — the dispatch button, gardener toggle, bulk checkbox, project
//     chip, and step deep-link all still work.
//   - The card title + id links open the drawer on a plain click but keep
//     their href for middle-click / ctrl-click / cmd-click "open the full
//     cockpit in a new tab".
//   - The open plan is mirrored in ?peek=<id> via history.pushState, so
//     back/forward and deep links restore the drawer; Esc, the × button, and
//     a backdrop click close it.
//   - Fragments are fetched with htmx.ajax when htmx is present (so the
//     mounted markup's hx-* step verbs are processed), falling back to fetch +
//     innerHTML otherwise.
(function () {
  'use strict';

  var drawer = document.getElementById('plan-peek-drawer');
  var backdrop = document.getElementById('plan-peek-backdrop');
  var body = document.getElementById('plan-peek-body');
  if (!drawer || !backdrop || !body) return;

  var basePath = drawer.getAttribute('data-base-path') || '';

  function peekURL(id) {
    return basePath + '/plans/' + encodeURIComponent(id) + '/peek';
  }

  function loadFragment(id) {
    body.innerHTML = '<p class="meta plan-peek-loading">Loading plan…</p>';
    if (window.htmx && typeof window.htmx.ajax === 'function') {
      window.htmx.ajax('GET', peekURL(id), { target: '#plan-peek-body', swap: 'innerHTML' });
      return;
    }
    fetch(peekURL(id), { credentials: 'same-origin' })
      .then(function (res) {
        if (!res.ok) throw new Error('peek fetch failed: ' + res.status);
        return res.text();
      })
      .then(function (html) { body.innerHTML = html; })
      .catch(function () {
        body.innerHTML = '<p class="plan-peek-error">Failed to load the plan preview — ' +
          '<a href="' + basePath + '/plans/' + encodeURIComponent(id) + '">open the full page</a>.</p>';
      });
  }

  function openPeek(id, pushHistory) {
    if (!id) return;
    drawer.hidden = false;
    backdrop.hidden = false;
    drawer.setAttribute('data-plan-id', id);
    loadFragment(id);
    if (pushHistory) {
      var url = new URL(window.location.href);
      url.searchParams.set('peek', id);
      history.pushState({ planPeek: id }, '', url);
    }
    var close = drawer.querySelector('.plan-peek-close');
    if (close) close.focus();
  }

  function closePeek(pushHistory) {
    if (drawer.hidden) return;
    drawer.hidden = true;
    backdrop.hidden = true;
    drawer.removeAttribute('data-plan-id');
    body.innerHTML = '';
    if (pushHistory) {
      var url = new URL(window.location.href);
      if (url.searchParams.has('peek')) {
        url.searchParams.delete('peek');
        history.pushState({ planPeek: null }, '', url);
      }
    }
  }

  // A click target that must keep its native behaviour: any interactive
  // element that is NOT the card's own open-the-plan links (title + id).
  function isNativeInteractive(el, card) {
    var node = el;
    while (node && node !== card) {
      var tag = node.tagName;
      if (tag === 'BUTTON' || tag === 'INPUT' || tag === 'SELECT' ||
          tag === 'TEXTAREA' || tag === 'LABEL' || tag === 'FORM') {
        return true;
      }
      if (tag === 'A') {
        // The title link and the id link open the peek; every other anchor
        // (project chip, step deep-link, escalation badge, terminal link)
        // navigates as before.
        var opensPeek = node.classList.contains('plan-card-title') ||
          (node.parentElement && node.parentElement.classList.contains('plan-card-id'));
        return !opensPeek;
      }
      node = node.parentElement;
    }
    return false;
  }

  document.addEventListener('click', function (evt) {
    if (evt.defaultPrevented) return;
    // Modified clicks (new tab / window) always pass through to native links.
    if (evt.button !== 0 || evt.metaKey || evt.ctrlKey || evt.shiftKey || evt.altKey) return;
    var card = evt.target.closest && evt.target.closest('.plan-card');
    if (!card || !card.dataset.planId) return;
    if (isNativeInteractive(evt.target, card)) return;
    evt.preventDefault();
    openPeek(card.dataset.planId, true);
  });

  backdrop.addEventListener('click', function () { closePeek(true); });
  drawer.addEventListener('click', function (evt) {
    if (evt.target.closest && evt.target.closest('.plan-peek-close')) closePeek(true);
  });
  document.addEventListener('keydown', function (evt) {
    if (evt.key === 'Escape' && !drawer.hidden) closePeek(true);
  });

  window.addEventListener('popstate', function () {
    var id = new URL(window.location.href).searchParams.get('peek');
    if (id) openPeek(id, false);
    else closePeek(false);
  });

  // Deep link: ?peek=<id> on load opens the drawer immediately.
  var initial = new URL(window.location.href).searchParams.get('peek');
  if (initial) openPeek(initial, false);
})();
