/* Why a gardener is not simply working, drawn (gd-J3l6W4X6DJ, plan step 2b; gardenio/docs/homestead-agents-as-people.md
   sections 2.3 and 5.1). The farm document's gardeners[] carry reason {kind, text, href}, built server-side (farm_reason.go)
   so the card, the Porch and Lands say the same words. This module turns it into the pose a gardener wears (a scarecrow when
   down, a speech bubble when parked, zzz asleep, wilting when stale, a sprout when starting), the line its card leads with,
   the Porch's "Needs you", and the bubble's way to its letter in the Mailbox. A steward's (Lands 9.8, farm_steward.go) adds
   in_flight and promise, a line of its memory, which change no pose. A kind this client does not know is no reason, and the state alone picks the pose as in Phase 1. A reason never changes where a gardener stands. */
var REASON = (function () {
  /* ---------- pure logic (also run under node by the Go tests) ---------- */
  var POSE = { down: 'down', parked: 'ask', sleeping: 'sleep', stale: 'wilt', starting: 'start', in_flight: 'stand', promise: 'stand' };   // the last two: a steward's memory line (Lands 9.8)
  /* The speech bubble over a gardener's head, from its 16 x 24 figure's top left, in world px: where ART.drawGardener draws
     it, and the rect of its button (kind gask). */
  var BUBBLE = { dx: 9, dy: -12, w: 14, h: 13 };
  function of(g) { var r = g && g.reason; return r && POSE[r.kind] ? r : null; }
  /* The pose: the reason's, else the state's (bucket stuck: down; yielding: ask; working: work; the rest stand). */
  function pose(g) {
    var r = of(g); if (r) return POSE[r.kind];
    return g.bucket === 'stuck' ? 'down' : g.state === 'yielding' ? 'ask' : g.state === 'working' ? 'work' : 'stand';
  }
  /* Needs you: down, or waiting on a human. The Porch's top group; the need order's first two ranks. */
  function needsYou(g) { var p = pose(g); return p === 'down' || p === 'ask'; }
  function dec(s) { try { return decodeURIComponent(String(s).replace(/\+/g, ' ')); } catch (e) { return String(s); } }
  /* The letter a parked gardener waits on: the handle's bead id from its reason's href (/garden?bead=<id>), else "". */
  function letter(g) {
    var r = of(g), m = r && r.kind === 'parked' && r.href ? /[?&]bead=([^&#]+)/.exec(r.href) : null;
    return m ? dec(m[1]) : '';
  }
  /* The line a gardener's card leads with, or null: [cls, text]; for a parked gardener a link to its letter (web/workbench.js opens
     the handle's bead view on a modified click, which carries the Mailbox's answer row). */
  function line(g, base) {
    var r = of(g), id = letter(g), cls = r ? 'rsn rsn-' + r.kind : '';
    if (!r || !r.text) return null;
    return id ? [cls, [{ text: r.text, href: (base || '') + r.href, cls: 'gletter', id: id }]] : [cls, r.text];
  }

  var api = { POSE: POSE, BUBBLE: BUBBLE, of: of, pose: pose, needsYou: needsYou, letter: letter, line: line, open: function () { return false; } };
  if (typeof document === 'undefined') return api;

  /* ---------- the way to the letter ---------- */
  /* open(id) -> true when the Mailbox shows the handle's row: the dock's Mailbox pane (a phone's Mailbox screen) with the row
     marked, scrolled to and focused, so its answer row is right there. When the row is not in the Mailbox (the Mine scope
     leaves it out), the handle's bead view, which carries the same answer row. */
  function open(id) {
    if (!id) return false;
    var sel = typeof CSS !== 'undefined' && CSS.escape ? CSS.escape('i-' + id) : 'i-' + id;
    var ul = document.getElementById('d-mail'), li = ul && ul.querySelector('li[data-src="' + sel + '"]'), G = window.GARDEN;
    if (!li) { if (typeof BEAD !== 'undefined' && BEAD.open) BEAD.open(id); return false; }
    if (G && G.toScreen) G.toScreen('mail');
    Array.prototype.forEach.call(ul.querySelectorAll('li.here'), function (o) { if (o !== li) o.classList.remove('here'); });
    li.classList.add('here');
    if (li.scrollIntoView) li.scrollIntoView({ block: 'nearest' });
    var t = li.querySelector('a.t') || li;
    try { t.focus({ preventScroll: true }); } catch (e) { t.focus(); }
    return true;
  }
  api.open = open;
  return api;
})();
if (typeof module !== 'undefined') module.exports = REASON;
