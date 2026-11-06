// beads-viewer hover preview popover for bead-ID links.
//
// Plan-of-record: docs/design/beads-viewer/popover-preview.md
// Bead: ka-0000005464; regression sweep + coordination: ka-0000006720
//
// Behavior:
//   - On hover (or focus) of an <a> whose pathname under
//     BEADS_VIEWER_BASE_PATH is exactly `ka-XXX`, fetch
//     `{base}/b/{id}/preview` and render the returned HTML fragment in a
//     singleton floating <aside id="bead-preview-popover" role="tooltip">.
//   - 150 ms hover-intent before show; 150 ms hide debounce.
//   - Per-page-load Map cache; single in-flight request per id.
//   - Mouse on popover keeps it open; Escape or click hides it.
//   - Viewport-aware: flips above/below and clamps within the window.
//   - Listens for `bead-popover-open` CustomEvent on document so other
//     popovers (priority/assignee inline edits, future popovers) can
//     declare "I am opening — preview, get out of the way" and the
//     preview will hide. Preview itself does NOT dispatch this event,
//     since it is the lowest-priority popover.
//
// Event delegation on document mirrors comment_links.js / copy_buttons.js
// so htmx swaps and SSE refreshes don't need re-binding.
(function () {
  'use strict';

  var BASE = (window.BEADS_VIEWER_BASE_PATH || '').replace(/\/+$/, '');
  var HOVER_MS = 150;
  var HIDE_MS = 150;
  var MARGIN = 8;       // viewport edge clamp
  var GAP = 6;          // gap between link and popover

  var cache = new Map();        // id → Promise<string|null>
  var inflight = new Map();     // id → AbortController
  var showT = null, hideT = null;
  var popover = null;
  var currentLink = null;
  var currentId = null;

  // hrefOf returns the plain href string for both HTML <a> and SVG <a>.
  // SVGAElement exposes .href as an SVGAnimatedString (not a string), which
  // stringifies to "[object SVGAnimatedString]" and breaks new URL(...).
  function hrefOf(a) {
    if (!a) return '';
    if (a.href && typeof a.href === 'object' && 'baseVal' in a.href) return a.href.baseVal;
    if (typeof a.href === 'string') return a.href;
    return a.getAttribute ? (a.getAttribute('href') || '') : '';
  }

  // linkKindOf classifies a link as a bead link (path is `kd-…`), an agent
  // link (path is `a/{name}`), or neither. Agent links resolve through
  // /a/{name}/preview, which mirrors the bead_preview fragment contract.
  function linkKindOf(a) {
    var raw = hrefOf(a);
    if (!raw) return null;
    var url;
    try { url = new URL(raw, location.origin); } catch (_) { return null; }
    if (url.origin !== location.origin) return null;
    var path = url.pathname;
    if (BASE && path.indexOf(BASE) === 0) path = path.slice(BASE.length);
    path = path.replace(/^\/+/, '').replace(/\/+$/, '');
    if (/^(gd|ka)-[A-Za-z0-9]+$/.test(path)) {
      // An anchor link (`/ka-X#c1-1`) carries the section slug in the hash.
      // We pass it to the preview so the popover shows the anchored section
      // (heading + following body) rather than the whole-bead excerpt
      // (anchored hover preview — ka-0000003033).
      var anchor = (url.hash || '').replace(/^#/, '');
      return { kind: 'bead', id: path, anchor: anchor };
    }
    var m = /^a\/([^/]+)$/.exec(path);
    if (m) return { kind: 'agent', name: decodeURIComponent(m[1]) };
    return null;
  }

  function isBeadLink(a) { return linkKindOf(a) !== null; }

  // previewURLFor returns the htmx fragment URL for the link's classified
  // kind. Bead links → /b/{id}/preview; agent links → /a/{name}/preview.
  function previewURLFor(info) {
    if (info.kind === 'bead') {
      var u = BASE + '/b/' + encodeURIComponent(info.id) + '/preview';
      if (info.anchor) u += '?anchor=' + encodeURIComponent(info.anchor);
      return u;
    }
    if (info.kind === 'agent') {
      return BASE + '/a/' + encodeURIComponent(info.name) + '/preview';
    }
    return null;
  }

  // cacheKeyFor namespaces the per-page-load cache so a bead and an agent
  // that happen to share a string never collide. The anchor is part of the
  // key so ka-X and ka-X#c1-1 cache distinct section previews.
  function cacheKeyFor(info) {
    if (info.kind === 'bead') return 'b:' + info.id + (info.anchor ? '#' + info.anchor : '');
    if (info.kind === 'agent') return 'a:' + info.name;
    return null;
  }

  function gapFor(anchor) {
    return anchor && anchor.closest && anchor.closest('figure.constellation') ? 10 : GAP;
  }

  function ensurePopover() {
    if (popover) return popover;
    popover = document.createElement('aside');
    popover.id = 'bead-preview-popover';
    popover.setAttribute('role', 'tooltip');
    popover.hidden = true;
    popover.addEventListener('mouseenter', cancelHide);
    popover.addEventListener('mouseleave', scheduleHide);
    document.body.appendChild(popover);
    return popover;
  }

  function fetchPreview(info) {
    var key = cacheKeyFor(info);
    if (cache.has(key)) return cache.get(key);
    var ac = new AbortController();
    inflight.set(key, ac);
    var p = fetch(previewURLFor(info), {
      signal: ac.signal,
      credentials: 'same-origin',
      headers: { 'Accept': 'text/html' },
    }).then(function (r) {
      if (r.status === 404) return r.text();      // notfound HTML fragment
      if (!r.ok) return null;
      return r.text();
    }).catch(function (e) {
      if (e && e.name === 'AbortError') return null;
      return null;
    }).then(function (html) {
      inflight.delete(key);
      return html;
    });
    cache.set(key, p);
    return p;
  }

  // position(el, anchor) places `el` adjacent to `anchor`, flipping above
  // when there's no room below, then clamps horizontally within the
  // viewport.
  function position(el, anchor) {
    el.style.position = 'fixed';
    el.style.left = '0px';
    el.style.top = '0px';
    el.style.maxWidth = '380px';
    // Force layout to measure intrinsic size.
    el.style.visibility = 'hidden';
    el.hidden = false;
    var rect = anchor.getBoundingClientRect();
    var pw = el.offsetWidth;
    var ph = el.offsetHeight;
    var vw = window.innerWidth;
    var vh = window.innerHeight;

    var gap = gapFor(anchor);
    var top;
    var spaceBelow = vh - rect.bottom;
    var spaceAbove = rect.top;
    if (spaceBelow >= ph + gap + MARGIN || spaceBelow >= spaceAbove) {
      top = rect.bottom + gap;
    } else {
      top = rect.top - ph - gap;
    }
    if (top < MARGIN) top = MARGIN;
    if (top + ph > vh - MARGIN) top = Math.max(MARGIN, vh - ph - MARGIN);

    var left = rect.left;
    if (left + pw > vw - MARGIN) left = vw - pw - MARGIN;
    if (left < MARGIN) left = MARGIN;

    el.style.left = left + 'px';
    el.style.top = top + 'px';
    el.style.visibility = '';
  }

  function show(link) {
    var info = linkKindOf(link);
    if (!info) return;
    var key = cacheKeyFor(info);
    currentLink = link;
    currentId = key;
    fetchPreview(info).then(function (html) {
      // User moved on, or fetch failed (network error / abort).
      if (currentLink !== link || currentId !== key || !html) return;
      var el = ensurePopover();
      el.innerHTML = html;
      position(el, link);
      el.hidden = false;
    });
  }

  function hide() {
    cancelShow();
    cancelHide();
    currentLink = null;
    currentId = null;
    if (popover) popover.hidden = true;
  }

  function scheduleShow(link) {
    cancelShow();
    cancelHide();
    showT = setTimeout(function () { showT = null; show(link); }, HOVER_MS);
  }
  function cancelShow() { if (showT) { clearTimeout(showT); showT = null; } }
  function scheduleHide() {
    cancelHide();
    hideT = setTimeout(function () { hideT = null; hide(); }, HIDE_MS);
  }
  function cancelHide() { if (hideT) { clearTimeout(hideT); hideT = null; } }

  document.addEventListener('mouseover', function (e) {
    var a = e.target.closest && e.target.closest('a');
    if (!isBeadLink(a)) return;
    scheduleShow(a);
  });
  document.addEventListener('mouseout', function (e) {
    var a = e.target.closest && e.target.closest('a');
    if (!isBeadLink(a)) return;
    cancelShow();
    if (popover && !popover.hidden) scheduleHide();
    else if (currentLink === a) hide();
  });
  document.addEventListener('focusin', function (e) {
    var a = e.target.closest && e.target.closest('a');
    if (!isBeadLink(a)) return;
    cancelShow(); cancelHide();
    show(a);
  });
  document.addEventListener('focusout', function (e) {
    var a = e.target.closest && e.target.closest('a');
    if (!isBeadLink(a)) return;
    if (currentLink === a) hide();
  });
  document.addEventListener('keydown', function (e) {
    if (e.key === 'Escape') hide();
  });
  // Coordination: any other popover (priority edit, assignee edit,
  // future overlays) dispatches `bead-popover-open` on document when it
  // takes the screen. Preview yields immediately so two popovers never
  // overlap. The dispatcher passes itself in detail.source so future
  // logic can ignore self-dispatched events if needed.
  document.addEventListener('bead-popover-open', function (e) {
    if (e && e.detail && e.detail.source === 'preview') return;
    hide();
  });
  document.addEventListener('click', function (e) {
    var a = e.target.closest && e.target.closest('a');
    if (isBeadLink(a)) hide();
  });
  // Reposition on scroll/resize while open so the popover doesn't drift.
  window.addEventListener('scroll', function () {
    if (popover && !popover.hidden && currentLink) position(popover, currentLink);
  }, true);
  window.addEventListener('resize', function () {
    if (popover && !popover.hidden && currentLink) position(popover, currentLink);
  });

  // Cross-frame control surface for bundle_html.js. A bundle html doc renders
  // inside a same-origin (script-free) iframe, so its bead links cannot be
  // reached by this script's document-level delegation. bundle_html.js binds
  // hover inside each frame and drives the popover here via a SYNTHETIC anchor:
  // an object exposing href, getBoundingClientRect (already translated into the
  // PARENT viewport), and closest (returns null — frame links are never inside
  // a .constellation figure). show()/position()/scheduleShow() use only those
  // three members, so a synthetic anchor behaves identically to a real one.
  // This keeps all fetch/cache/positioning logic in one place.
  window.__beadPreview = {
    scheduleShow: function (synthAnchor) { if (isBeadLink(synthAnchor)) scheduleShow(synthAnchor); },
    cancelShow: cancelShow,
    scheduleHide: scheduleHide,
    cancelHide: cancelHide,
    isOpen: function () { return !!(popover && !popover.hidden); },
    isBeadLink: isBeadLink,
    hide: hide,
  };
})();
