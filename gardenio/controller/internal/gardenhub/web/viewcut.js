/* VIEWCUT: the farm stream sends this page only what it can see (gd-fBGGNdgrLc, step 4g of plan gd-gAKb8Bf0Nm;
   gardenio/docs/homestead-farm-read-model.md, "Activity"). /garden/farm/events opens with a `hello` event naming the stream
   (web/plans.js hands it over as `farmhello`); this file POSTs the page's view for it to /garden/farm/events/view, and again
   whenever the view changes, at most once every WAIT ms while the camera moves. Below the Plan level the view is the level alone:
   the stream then sends no calls, only which projects are busy (a `busy` event every few seconds, for BIRDS.sign). At the Plan and
   Bead levels it is the gardeners standing on a plot in view or within MARGIN of it, so a call heard while the page catches up with
   a pan still has its bird, and the gardeners at home at a Barn cell in that grown view (5b, 28.4: a barn is one plot, at its
   middle; their birds fly over it, web/birds.js); the stream sends their calls only, and each gardener that comes into view its doing (a `doing` event,
   for web/activity.js). Until the first POST lands the stream sends every call, as before. want() and key() are pure and run
   under node (activity_page_test.go); garden.js calls see() per render. Units: world px, ms. */
var VIEWCUT = (function () {
  'use strict';
  var W = typeof WORLD !== 'undefined' ? WORLD : require('./world.js');
  var PLAN = 2, CAP = 256, MARGIN = 0.25, WAIT = 300;   // the Plan level; ids a view names (maxViewIDs); the margin, of the view's size each side; ms between two POSTs

  /* want(o) -> {level, ids}: o = {level, view (world px {x, y, w, h}), gs (WORLD.gardenerState)}. Below the Plan level, no ids.
     From it, the gardeners shown on a plot at this level whose spot lies inside the view grown by MARGIN each side: the CAP
     nearest its middle, sorted by id. Those at home count from their Growing field's middle when it meets that view (28.12). */
  function want(o) {
    if (!o || !(o.level >= PLAN) || !o.gs || !o.view) return { level: (o && o.level) || 0, ids: [] };
    var v = o.view, mx = v.w * MARGIN, my = v.h * MARGIN, cx = v.x + v.w / 2, cy = v.y + v.h / 2, spots = (o.gs[o.level] || {}).spots || {}, near = [];
    Object.keys(spots).forEach(function (id) {
      var s = spots[id];
      if (!s || !s.shown || s.x < v.x - mx || s.x > v.x + v.w + mx || s.y < v.y - my || s.y > v.y + v.h + my) return;
      near.push({ id: id, d: Math.abs(s.x - cx) + Math.abs(s.y - cy) });
    });
    var fields = (o.gs[o.level] || {}).fields || {}, ix = o.gs.world && o.gs.world.index || {};
    Object.keys(fields).forEach(function (bid) {
      var c = ix[bid] && ix[bid].home && ix[bid].home.cells.growing, y = c && W.tiles(c);
      if (!y || !(y.x < v.x + v.w + mx && y.x + y.w > v.x - mx && y.y < v.y + v.h + my && y.y + y.h > v.y - my)) return;
      var yd = Math.abs(y.x + y.w / 2 - cx) + Math.abs(y.y + y.h / 2 - cy);
      fields[bid].forEach(function (id) { if (!(spots[id] && spots[id].plot)) near.push({ id: id, d: yd }); });
    });
    near.sort(function (a, b) { return a.d - b.d || (a.id < b.id ? -1 : 1); });
    return { level: o.level, ids: near.slice(0, CAP).map(function (n) { return n.id; }).sort() };
  }
  /* The view as the server tells views apart: the busy flags alone, or the ids named. */
  function key(w) { return w.level < PLAN ? 'busy' : 'ids:' + w.ids.join(','); }

  var api = { PLAN: PLAN, CAP: CAP, MARGIN: MARGIN, WAIT: WAIT, want: want, key: key, see: function () {} };
  if (typeof document === 'undefined' || !document.addEventListener || typeof fetch === 'undefined') return api;

  /* ---------- the page's side ---------- */
  var base = (document.body && document.body.dataset && document.body.dataset.base) || '', sid = '', sent = '', last = null, seen = null, timer = null;
  function post() {
    timer = null;
    if (!sid || !last || key(last) === sent) return;
    var k = key(last), at = sid;
    sent = k;
    fetch(base + '/garden/farm/events/view', { method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ sid: sid, level: last.level, ids: last.ids }) })
      .then(function (r) { if (!r.ok && sid === at && sent === k) sent = ''; }, function () { if (sid === at && sent === k) sent = ''; });   // tried again on the next change, or the next stream
  }
  function soon(now) { if (now) { if (timer) clearTimeout(timer); post(); } else if (!timer) timer = setTimeout(post, WAIT); }
  /* o as for want(), each render. Nothing is worked out again while the level, the view and the gardeners stand still. */
  api.see = function (o) {
    var v = o && o.view;
    if (!v || (seen && seen.level === o.level && seen.gs === o.gs && seen.x === v.x && seen.y === v.y && seen.w === v.w && seen.h === v.h)) return;
    seen = { level: o.level, gs: o.gs, x: v.x, y: v.y, w: v.w, h: v.h };
    last = want(o);
    if (key(last) !== sent) soon(false);
  };
  /* A new stream starts from every call: say the view at once. */
  document.addEventListener('farmhello', function (e) { sid = (e.detail && e.detail.sid) || ''; sent = ''; if (last) soon(true); });
  return api;
})();
if (typeof module !== 'undefined') module.exports = VIEWCUT;
