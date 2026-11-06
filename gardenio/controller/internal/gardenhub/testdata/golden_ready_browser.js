/* What a golden shot waits for in the page before it is taken: the lazy bundles run, Plan P's arms painted, the Commons'
   image attachments loaded. Each is a page function for page.waitForFunction, so it runs in the page and reads its globals.
   Split out of golden_browser.js by concern, which requires it. Runs nothing of its own. */
'use strict';

/* The Workbench and the Commons load after the World (web/lazy.js, P1.3, gd-p02ZelpMxk): name, or with none every bundle,
   has run. A page with no LAZY loads everything at once. */
function lazyLoaded(name) { return !window.LAZY || LAZY.loaded(name || undefined); }
/* Plan P's arms (cardWorld, goldenPlanArms) have arrived wherever the World hangs them, at the Plan and Bead depths (heraldry
   step 4.3): the picture is fetched on first draw and painted on the repaint its arrival asks for, which settle's frames
   cover. HERALDRY.picture hands back the loaded bitmap for the page's own key (slot, ratio, sky) and starts nothing new
   once the page has asked. Elsewhere there is nothing to wait for. */
var ARMS_P = 'blue \u{1F9ED} . \u{1F525} . \u{1F4E6}';
function armsLoaded(arms) {
  if (!window.GARDEN || typeof HERALDRY === 'undefined' || typeof SKY === 'undefined' || !GARDEN.cam || GARDEN.cam().level < 2) return true;
  return !!HERALDRY.picture(arms, HERALDRY.SLOT, Math.min(window.devicePixelRatio || 1, 3), SKY.night());
}
/* Every Commons image attachment on screen has loaded: its <img> is loading="lazy" decoding="async" (commons/render.go), so
   commonsbead-desktop-dark was shot with #general's dusk picture still blank (PR #1668, ready run 41266220529). One out of
   view, clipped off by its timeline's scroll or held on data saver is not waited for: a lazy one never loads there. */
function attachmentsLoaded() {
  return Array.prototype.every.call(document.querySelectorAll('.cm-img:not(.cm-img--held) img[src]'), function (i) {
    var r = i.getBoundingClientRect(), s = i.closest('.cm-messages'), b = s ? s.getBoundingClientRect() : { top: 0, bottom: innerHeight };
    return !r.width || r.bottom <= Math.max(0, b.top) || r.top >= Math.min(innerHeight, b.bottom) ? true : i.complete;
  });
}

module.exports = { lazyLoaded: lazyLoaded, ARMS_P: ARMS_P, armsLoaded: armsLoaded, attachmentsLoaded: attachmentsLoaded };
