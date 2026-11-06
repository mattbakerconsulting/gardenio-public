// deck_swipe.js — swipe gestures for the mobile card-deck (/deck).
//
// Plan: ka-0000000492. Horizontal skip/back is Step 5 (ka-0000005925); the
// upward "open the cockpit" flick is Step 5b (ka-0000005224) — the "swipe up to
// action" acceptance criterion the design mock spelled `flick-up: open full
// cockpit`. Narrative: bundle ka-0000004994 §2.
//
// THE ONE NON-NEGOTIABLE RULE — gestures NAVIGATE, they never COMMIT.
//   A swipe can only ever do three things, all non-destructive: SKIP the current
//   card (send it to the back of the deck), go BACK to the previous one, or
//   FLICK UP to open that plan's full cockpit — a *read* surface where the
//   fleet-affecting actions live behind their own explicit buttons. A mis-swipe
//   must never resolve a decision, dispatch a step, or run Approve & build. That
//   safety is STRUCTURAL here, not a matter of care: this module has no knowledge
//   of and no path to the resolve, dispatch, or build verbs. All it can do on a
//   completed swipe is (a) dispatch a cancelable `deck:navigate` CustomEvent for
//   skip/back and, if the deck ships them, (b) `.click()` the deck's own
//   [data-deck-skip] / [data-deck-back] / [data-deck-up] controls — the very
//   same links a keyboard or JS-off user uses. The up control is the active
//   card's own "Open full cockpit" <a href> (a plain GET), so an up-flick can
//   only ever *navigate to* the actions, never perform one. Commit actions (the
//   answer buttons) are plain taps and are explicitly excluded from starting a
//   drag, so tapping "Merge" can never be read as a swipe. (Contrast the retired
//   iOS app, which bound swipe-left to a destructive close —
//   beads-viewer-ios-parity-phase2.md:53,58. We deliberately do NOT repeat that
//   on a surface where the actions are fleet-affecting.)
//
// DOM CONTRACT (all Step-4-owned; this module binds to data-* attributes only):
//   [data-deck]        — the deck root. Its presence is the gate; absent ⇒ no-op.
//   [data-deck-card]   — the active (frontmost) card. The element that follows
//                        the pointer. Background/stacked cards must NOT carry it.
//   [data-deck-skip]   — OPTIONAL nav control inside the deck. A left-swipe
//                        clicks it (card flies off left). Absent ⇒ left-swipe
//                        just rubber-bands back.
//   [data-deck-back]   — OPTIONAL nav control. A right-swipe clicks it (card
//                        flies off right). Absent ⇒ right-swipe rubber-bands.
//   [data-deck-up]     — OPTIONAL nav control, scoped to the ACTIVE card (each
//                        card links to its OWN plan cockpit, so this is looked
//                        up inside [data-deck-card] first, then the deck). A
//                        scroll-safe upward FLICK clicks it. Absent ⇒ an
//                        up-flick does nothing (and never fires the deck:navigate
//                        event, so a deck whose handler maps unknown directions
//                        to skip can't misread it). Wired onto the lean card's
//                        "Open full cockpit" link in partials/deck_card.html.
//   [data-deck-no-swipe] — OPTIONAL. Any descendant so marked (plus every native
//                        interactive element) never starts a drag.
//
//   Instead of (or in addition to) the control buttons, the deck may listen for
//   the cancelable `deck:navigate` CustomEvent dispatched on the deck root with
//   detail { direction: 'skip' | 'back' }. preventDefault() on it suppresses the
//   fallback control .click(). Either wiring is non-destructive by construction.
//
// PROGRESSIVE ENHANCEMENT: the deck is a working page without this file — the
// Skip/Back buttons and one-tap answers all function on their own. Swipe is a
// pure enhancement layer that reuses those same controls.
//
// touch-action: the active card gets `touch-action: pan-y` (injected stylesheet)
// so the browser keeps native vertical scrolling while we own the horizontal
// axis — we never fight the page's own scroll, and only take over once a gesture
// is unambiguously horizontal.
//
// THE UP-FLICK IS SCROLL-SAFE BY CONSTRUCTION. Because vertical is the browser's
// axis, we NEVER preventDefault a vertical gesture and NEVER capture the pointer
// on it — the page scrolls exactly as it would without this file. We merely
// *measure* the vertical flick and, on a clean pointerup, act only if the
// browser did NOT consume it as a scroll. Two independent guards make that safe:
// a real scroll fires `pointercancel` (the browser took the gesture), which can
// never navigate; and even if a browser still delivers pointerup after scrolling,
// we require the scroll offset to be UNCHANGED across the gesture. So the up-flick
// resolves to "open cockpit" only when there was nothing to scroll — i.e. on the
// above-the-fold deck card it is designed for — and otherwise silently yields to
// scrolling. It errs toward scroll, never toward an accidental navigation.
//
// prefers-reduced-motion: the drag itself is direct 1:1 manipulation (allowed),
// but the fly-off / snap-back TRANSITIONS are suppressed when the user asks for
// reduced motion — the card just snaps to its end state with no animation.
//
// Delegation is on `document` (mirroring plans_board.js / preview_popover.js) so
// htmx swaps and card re-renders need no rebinding.
(function () {
  'use strict';

  // --- gate: bail instantly on any page with no deck. Re-checked on htmx loads
  //     below, so a deck that arrives via a later swap still gets wired. ---
  var styleInjected = false;
  function deckPresent() { return !!document.querySelector('[data-deck]'); }

  // Inject `touch-action: pan-y` for deck cards exactly once, and only once a
  // deck actually exists. The rule matches nothing elsewhere, but we still gate
  // it so non-deck pages stay untouched.
  function ensureStyle() {
    if (styleInjected || !deckPresent()) return;
    var st = document.createElement('style');
    st.setAttribute('data-deck-swipe', '');
    // pan-y: browser keeps vertical scroll; horizontal is ours to claim.
    st.textContent = '[data-deck-card]{touch-action:pan-y;}';
    document.head.appendChild(st);
    styleInjected = true;
  }

  var reduceMotion = window.matchMedia
    ? window.matchMedia('(prefers-reduced-motion: reduce)')
    : { matches: false };

  var SLOP = 8;          // px of travel before we lock an axis
  var MIN_COMMIT = 64;   // absolute px floor to count as a committed swipe
  var COMMIT_FRAC = 0.22;// or this fraction of card width, whichever is larger
  var FLICK_VPX = 0.5;   // px/ms — a fast flick commits below the distance bar
  var MAX_ROT = 6;       // deg of tilt at full-width drag (skipped if reduced)
  var MIN_UP = 56;       // px of upward travel to consider an up-flick at all
  var FLICK_UP_VPX = 0.6;// px/ms upward velocity floor — set a touch higher than
                         // the side flick: an up-flick competes with scroll, so
                         // we demand a clearly deliberate flick, never a drift.

  // Per-gesture state. Only one pointer is ever tracked.
  var pointerId = null;  // active pointer, or null when idle
  var card = null;       // the [data-deck-card] being dragged
  var deck = null;       // its [data-deck] root
  var startX = 0, startY = 0;
  var lastX = 0, lastY = 0, lastT = 0, vX = 0, vY = 0;
  var startScrollY = 0;  // scroll offset at gesture start — the up-flick acts
                         // only if this is unchanged at pointerup (no scroll).
  var axis = '';         // '' until locked, then 'x' (we own it) or 'y' (the
                         //   browser scrolls; we only MEASURE for an up-flick)
  // A control-less deck can still commit swipes if it listens for deck:navigate;
  // it opts in by setting data-deck-nav-events on the deck root.
  var hasNavListener = false;

  // A pointerdown target that must NOT start a drag: any native-interactive
  // control (so answer buttons, links, inputs stay tappable) or anything the
  // deck opted out with [data-deck-no-swipe]. Modeled on plans_board.js
  // isNativeInteractive.
  function blocksDrag(el, root) {
    var node = el;
    while (node && node !== root) {
      if (node.hasAttribute && node.hasAttribute('data-deck-no-swipe')) return true;
      var tag = node.tagName;
      if (tag === 'BUTTON' || tag === 'INPUT' || tag === 'SELECT' ||
          tag === 'TEXTAREA' || tag === 'LABEL' || tag === 'FORM' ||
          tag === 'A' || tag === 'DETAILS' || tag === 'SUMMARY') {
        return true;
      }
      node = node.parentElement;
    }
    return false;
  }

  function isVisible(el) {
    return !!el && el.offsetParent !== null &&
      el.getAttribute('aria-hidden') !== 'true' && !el.hasAttribute('inert');
  }

  function commitThreshold() {
    var w = (card && card.offsetWidth) || window.innerWidth || 320;
    return Math.max(MIN_COMMIT, w * COMMIT_FRAC);
  }

  // Paint the card at the current horizontal offset. Direct manipulation, so it
  // tracks the finger even under reduced-motion (no transition is set here).
  function paint(dx) {
    if (!card) return;
    var w = (card.offsetWidth) || window.innerWidth || 320;
    var rot = reduceMotion.matches ? 0 : (dx / w) * MAX_ROT;
    var frac = Math.min(1, Math.abs(dx) / (w * 0.6));
    card.style.transform = 'translateX(' + dx + 'px) rotate(' + rot.toFixed(2) + 'deg)';
    card.style.opacity = String(1 - 0.4 * frac);
  }

  function clearTransform(withTransition) {
    if (!card) return;
    card.style.transition = (withTransition && !reduceMotion.matches)
      ? 'transform 180ms ease, opacity 180ms ease' : '';
    card.style.transform = '';
    card.style.opacity = '';
  }

  // Fire a horizontal navigation (skip/back). Together with openCockpit() below
  // (the up-flick), this is the module's ENTIRE outbound surface: dispatch a
  // cancelable event, then (unless suppressed) click the matching nav control.
  // Neither path can commit an answer — every control it clicks is a Skip/Back
  // link or the card's own cockpit link, all plain GET navigations.
  function navigate(direction) {
    if (!deck) return;
    var ev;
    try {
      ev = new CustomEvent('deck:navigate', {
        bubbles: true, cancelable: true, detail: { direction: direction }
      });
    } catch (_) { ev = null; }
    var suppressed = ev ? !deck.dispatchEvent(ev) : false;
    if (suppressed) return; // the deck took over via preventDefault()
    var sel = direction === 'skip' ? '[data-deck-skip]' : '[data-deck-back]';
    var control = deck.querySelector(sel);
    if (control && typeof control.click === 'function') control.click();
  }

  // Current vertical scroll offset, cross-browser.
  function currentScrollY() {
    return window.pageYOffset ||
      (document.documentElement && document.documentElement.scrollTop) ||
      (document.body && document.body.scrollTop) || 0;
  }

  // The active card's up-navigation control: its OWN "Open full cockpit" link.
  // Scoped to [data-deck-card] first (each card links to its own plan), falling
  // back to a deck-level control. Returns null when the deck ships no up target.
  function upControl() {
    return (card && card.querySelector('[data-deck-up]')) ||
      (deck && deck.querySelector('[data-deck-up]')) || null;
  }

  // The up-flick's ONLY outbound action: click the active card's [data-deck-up]
  // control. That control is a real <a href> to the plan cockpit — a GET to a
  // read surface — so this can only ever NAVIGATE, never commit (the same
  // structural guarantee skip/back have). Deliberately does NOT dispatch the
  // deck:navigate event: that event's detail.direction is consumed by deck
  // shells whose handler maps every non-'back' direction to 'skip', so emitting
  // 'up' there would flip the card instead of opening the cockpit. A shell that
  // wants to intercept the up-flick (e.g. an in-page cockpit drawer) binds a
  // click handler on [data-deck-up], exactly as it already does for
  // [data-deck-skip] / [data-deck-back].
  function openCockpit() {
    var control = upControl();
    if (control && typeof control.click === 'function') control.click();
  }

  // Decide, at the end of a vertical gesture, whether it was a deliberate
  // up-flick that should open the cockpit — or just scrolling (in which case we
  // do nothing and the page keeps its native scroll). Called ONLY from a clean
  // pointerup (a scroll fires pointercancel, handled separately).
  function maybeUpFlick(dy) {
    if (dy > -MIN_UP) return;                 // not far enough up
    if (vY > -FLICK_UP_VPX) return;           // not a fast enough UPWARD flick
    if (currentScrollY() !== startScrollY) return; // the page scrolled ⇒ leave it
    if (!upControl()) return;                 // no cockpit target ⇒ inert
    openCockpit();
  }

  function endGesture(commit, dx) {
    var w = (card && card.offsetWidth) || window.innerWidth || 320;
    var direction = dx < 0 ? 'skip' : 'back';
    var control = deck && deck.querySelector(
      direction === 'skip' ? '[data-deck-skip]' : '[data-deck-back]');
    // Only commit if the matching control (or an event listener) can act; with
    // no way to navigate that way, snap back rather than swallow the gesture.
    var canGo = !!control || hasNavListener;
    if (commit && canGo) {
      if (reduceMotion.matches) {
        clearTransform(false);
        navigate(direction);
      } else {
        // Fly the card off in the swipe direction, then hand off. htmx will
        // swap in the next card; if nothing swaps, the transform is reset.
        card.style.transition = 'transform 160ms ease-out, opacity 160ms ease-out';
        card.style.transform = 'translateX(' + (dx < 0 ? -1.15 : 1.15) * w +
          'px) rotate(' + (dx < 0 ? -MAX_ROT : MAX_ROT) + 'deg)';
        card.style.opacity = '0';
        var flown = card;
        navigate(direction);
        // Reset the (possibly reused) node shortly after, in case no swap landed.
        window.setTimeout(function () {
          if (flown && flown.isConnected) {
            flown.style.transition = '';
            flown.style.transform = '';
            flown.style.opacity = '';
          }
        }, 180);
      }
    } else {
      clearTransform(true);
    }
  }

  function resetState() {
    pointerId = null; card = null; deck = null; axis = '';
    startX = startY = lastX = lastY = lastT = vX = vY = startScrollY = 0;
  }

  document.addEventListener('pointerdown', function (evt) {
    if (pointerId !== null) return;                 // one gesture at a time
    if (evt.button !== undefined && evt.button !== 0) return; // primary only
    var c = evt.target.closest && evt.target.closest('[data-deck-card]');
    if (!c || !isVisible(c)) return;
    var d = c.closest('[data-deck]');
    if (!d) return;
    if (blocksDrag(evt.target, c)) return;          // taps on controls pass through
    ensureStyle();
    hasNavListener = d.hasAttribute('data-deck-nav-events');
    pointerId = evt.pointerId;
    card = c; deck = d;
    startX = lastX = evt.clientX; startY = lastY = evt.clientY;
    lastT = evt.timeStamp; vX = vY = 0; axis = '';
    startScrollY = currentScrollY();                // baseline for the up-flick's
                                                    // "did the page scroll?" guard
    card.style.transition = '';                     // follow finger with no lag
  }, { passive: true });

  document.addEventListener('pointermove', function (evt) {
    if (evt.pointerId !== pointerId || !card) return;
    var dx = evt.clientX - startX;
    var dy = evt.clientY - startY;

    if (axis === '') {
      if (Math.abs(dx) < SLOP && Math.abs(dy) < SLOP) return; // still in slop
      // Lock to whichever axis dominates. Horizontal ⇒ we own it (capture the
      // pointer, paint the drag). Vertical ⇒ the BROWSER owns it for native
      // scroll — we neither capture nor preventDefault; we only keep measuring
      // so pointerup can tell a deliberate up-flick from a scroll.
      axis = Math.abs(dx) > Math.abs(dy) ? 'x' : 'y';
      if (axis === 'x') {
        try { card.setPointerCapture(evt.pointerId); } catch (_) {}
      }
    }

    if (axis === 'x') {
      if (evt.cancelable) evt.preventDefault(); // claim the horizontal gesture
      var dt = evt.timeStamp - lastT;
      if (dt > 0) vX = (evt.clientX - lastX) / dt;
      lastX = evt.clientX; lastT = evt.timeStamp;
      paint(dx);
    } else if (axis === 'y') {
      // Vertical: never preventDefault (that would fight the page's own scroll),
      // never paint. Just track the running velocity for the up-flick decision.
      var dyt = evt.timeStamp - lastT;
      if (dyt > 0) vY = (evt.clientY - lastY) / dyt;
      lastY = evt.clientY; lastT = evt.timeStamp;
    }
  }, { passive: false });

  function onPointerEnd(evt) {
    if (evt.pointerId !== pointerId) return;
    var wasX = axis === 'x';
    var wasY = axis === 'y';
    var dx = evt.clientX - startX;
    var dy = evt.clientY - startY;
    var cancelled = evt.type === 'pointercancel';
    if (card) { try { card.releasePointerCapture(evt.pointerId); } catch (_) {} }
    if (wasX && !cancelled) {
      var committed = Math.abs(dx) >= commitThreshold() ||
        (Math.abs(vX) >= FLICK_VPX && Math.abs(dx) >= SLOP * 2);
      endGesture(committed, dx);
    } else if (wasX) {
      clearTransform(true); // pointercancel mid-drag ⇒ snap back
    } else if (wasY && !cancelled) {
      // Clean end of a vertical gesture. pointercancel means the browser scrolled
      // (took the gesture) — we never navigate then; only a scroll-free pointerup
      // can be an up-flick.
      maybeUpFlick(dy);
    }
    resetState();
  }
  document.addEventListener('pointerup', onPointerEnd, { passive: true });
  document.addEventListener('pointercancel', onPointerEnd, { passive: true });

  // A deck can arrive via an htmx swap after first paint; wire touch-action then.
  document.addEventListener('htmx:load', ensureStyle);
  document.addEventListener('htmx:afterSwap', ensureStyle);

  // Initial pass.
  if (document.readyState !== 'loading') ensureStyle();
  else document.addEventListener('DOMContentLoaded', ensureStyle);
})();
