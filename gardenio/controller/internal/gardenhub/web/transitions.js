/* ANIM: the gardeners' transitions on the farm (gardenio/docs/homestead-agents-as-people.md section 7, gd-elQWxzYhdm). A
   gardener walks from its field stand to its plot when it starts and back when it goes idle; to a plot of another biome a
   truck leaves its own barn's door, picks it up at its stand and drives it there, and back. It bursts and flies back to its
   stand when it goes down or is interrupted, walks home with a scroll when it finishes, and walks from plot to plot along the
   paths between them when its task moves (World Phase 3, gd-gMMtrgIT1W). The walk and the ride keep to the lanes
   (29.3, X-D11): the walker out of the farmstead's nearest gate, the truck out of the bay onto the west lane, along the lanes
   to the plot gap beside the plot and in at the spot's column; between biomes, or to a band item, they go straight. Its
   stand is in its home strip's Growing field (homestead-world-design.md 28.4, 28.12: WORLD.homeTile for the walks, the
   fly-back and the puff; WORLD.barnDoor for the truck, and for all of them when homeFill drew no stand). Placement (WORLD.gardenerSpots)
   is the truth the moment a patch lands: this layer only draws a moving figure in place of that id's static sprite until it
   lands, and a moving gardener has no link or barn slot until then (rule 7.2.1). plan() and at() are pure and run under node
   (transitions_test.go); the rest is the page's. plans.js says what each farm document is on 'farmlive' events (load, open,
   reset, or a patch with its exits); garden.js calls update() per farm document and draw() per render. Units: world px, ms. */
var ANIM = (function () {
  "use strict";
  var W = typeof WORLD !== "undefined" ? WORLD : require("./world.js");
  var LN = typeof LANES !== "undefined" ? LANES : require("./lanes.js");
  /* A leisurely pace (rule 4, gd-OpU3ePStLF): a viewer should be able to see who is leaving the barn and where they go, so
     the truck and the walk take seconds, not a blink. */
  var SPEED = 100;                                  // world px per second, truck and walk (rule 4)
  var RIDE = [2500, 7000], ARC = [2500, 4500];      // a ride or walk, and the flight back, clamped to these (ms)
  var WALK = [1000, 7000];                          // a walk to another plot or column: a step along the fence is short (ms)
  var BURST = 1000, PUFF = 600, STAND = 900;        // the particles, the puff at the barn, the scarecrow dropping (ms)
  var STEP = 320;                                   // one walking frame (ms)
  var CATCH_UP = 2000, EXIT_AGE = 10, MAX_PATCH = 8, MAX_RUN = 12;   // rules 2 and 3 (EXIT_AGE in seconds)
  var LABEL_MS = 10000, FINISHED_S = 600, TLDR_MAX = 60;             // the arrival label, and the barn card's "Just finished"
  var pinned = null, force = false, openAt = -Infinity, pending = null, running = [], labels = [], finished = [], landFn = null, lblsEl = null;

  /* The progress clock (rule 5): Date.now() unless a test pinned it. */
  function now() { return pinned === null ? Date.now() : pinned; }
  function clamp(v, r) { return Math.max(r[0], Math.min(r[1], v)); }
  function clip(s, n) { var r = Array.from(String(s || "").trim()); return r.length <= n ? r.join("") : r.slice(0, n - 1).join("").replace(/\s+$/, "") + "…"; }
  function dist(a, b) { return Math.sqrt((a.x - b.x) * (a.x - b.x) + (a.y - b.y) * (a.y - b.y)); }
  function lerp(a, b, k) { return { x: a.x + (b.x - a.x) * k, y: a.y + (b.y - a.y) * k }; }

  /* Gardener id's barn door and home tile in WORLD.gardenerState st (world px, the 16 x 24 figure's top left), or null. */
  function door(st, id) { var d = st && W.barnDoor(st.world, W.homeFor(st.byId[id])); return d && { x: d.x, y: d.y }; }
  function home(st, id) { var h = st && st.byId[id] && W.homeTile(st.world, st.byId[id]); return h && { x: h.x, y: h.y }; }
  /* stand(st, id, b) -> {at, near}: gardener id's field stand in st when homeFill drew one (at, its figure's top left, else
     null), and whether spot b (a where() result) is in the stand's biome, so the way there is a walk, not a ride (28.12). */
  function stand(st, id, b) {
    var h = st && st.byId[id] && W.homeTile(st.world, st.byId[id]), ix = (st && st.world && st.world.index) || {}, it = ix[b.plot];
    if (!h || !h.drawn) return { at: null, near: false };
    return { at: { x: h.x, y: h.y }, near: !!it && "project:" + W.projectOf(ix, it) === h.biome };
  }
  /* The lane way (homestead-world-design.md 29.3) between gardener id's barn and its spot b on a plot (a where() result) in
     WORLD.gardenerState st: the ride truck's out of the bay (LANES.rideLanes) or the walk's out of the farmstead's gate
     (LANES.walkLanes), from start (door or home); null when that plot is not its home biome's, so the way is straight. */
  function lanes(st, id, b, ride, start) {
    var g = st && st.byId[id];
    return g && start ? (ride ? LN.rideLanes : LN.walkLanes)(st.world, W.homeFor(g), start, b.plot, b) : null;
  }

  /* Where a gardener is in WORLD.gardenerState st at level lv: k is none (not a gardener), home, plot (drawn on it) or more
     (on it, behind the "+N" badge), with the figure's top left and facing for a plot, its plot's lane (the paths around it)
     and whether it is down. */
  function where(st, lv, id) {
    var s = st && st[lv] && st[lv].spots[id];
    if (!s || s.land) return { k: "none" };   // a steward at its homestead (WORLD.stewardState) neither rides nor walks
    if (s.home) return { k: "home" };
    var down = st.look[id] && st.look[id].pose === "down", pl = st[lv].plots[s.plot];
    return s.shown ? { k: "plot", plot: s.plot, x: s.x, y: s.y, face: s.face, down: down, lane: pl && pl.lane } : { k: "more", plot: s.plot, down: down };
  }
  /* route(a, b) -> [points]: the way a walker goes from spot a to spot b (where() results), in world px. On its own plot it
     steps straight across the field. To another plot of the same project it goes down into the path under its plot, along it, and,
     when the other plot is in another row, up or down the path beside its plot (on the side the other plot is), then along
     the path under the other plot and up into its field: the walker keeps to the paths between the plots. Between two
     projects the paths do not line up, so it goes straight. */
  function route(a, b) {
    var la = a.lane, lb = b.lane;
    if (a.plot === b.plot || !la || !lb || la.project !== lb.project) return [{ x: a.x, y: a.y }, { x: b.x, y: b.y }];
    var pts = [{ x: a.x, y: a.y }, { x: a.x, y: la.y }];
    if (la.y !== lb.y) { var gx = lb.r > la.r ? la.r : lb.r < la.r ? la.l : la.r; pts.push({ x: gx, y: la.y }, { x: gx, y: lb.y }); }
    pts.push({ x: b.x, y: lb.y }, { x: b.x, y: b.y });
    return pts;
  }
  function length(pts) { var d = 0; for (var i = 1; i < pts.length; i++) d += dist(pts[i - 1], pts[i]); return d; }
  /* The way a truck faces a fraction k along a route: the sign of its run across x there, else of the last before it, else
     of the first after it. */
  function heading(pts, k) {
    var left = k * length(pts), dx = 0, i;
    for (i = 1; i < pts.length; i++) {
      if (pts[i].x !== pts[i - 1].x) dx = pts[i].x - pts[i - 1].x;
      var d = dist(pts[i - 1], pts[i]);
      if (left <= d && d > 0) break;
      left -= d;
    }
    for (; !dx && i < pts.length; i++) dx = pts[i].x - pts[i - 1].x;
    return dx;
  }
  /* The point a fraction k of the way along a route, by distance. */
  function along(pts, k) {
    var left = k * length(pts);
    for (var i = 1; i < pts.length; i++) {
      var d = dist(pts[i - 1], pts[i]);
      if (left <= d && d > 0) return lerp(pts[i - 1], pts[i], left / d);
      left -= d;
    }
    return { x: pts[pts.length - 1].x, y: pts[pts.length - 1].y };
  }

  /* plan(prev, next, exits, o) -> {list, changed}: the transitions one live patch starts (the table in section 7.1), before
     rule 3's limits. prev and next are WORLD.gardenerState before and after it; exits the patch's; o = {level, view (the
     padded viewport, world px), t (now, ms), nowS (now, unix s)}. A transition is
     {id, kind, look, from, to, t0, dur, moving (the static sprite waits for it), stuck, tldr, agent, pts (a walk to another
     plot: its route), box (its path's bounds)}. changed lists every id whose spot or pose changed, so a transition already running for it is replaced or cut.
     Nothing starts below the Plan level or off the padded viewport; a patch with more than MAX_PATCH is a cut (rule 2), and
     none starts for a gardener with no barn laid out. */
  function plan(prev, next, exits, o) {
    var out = [], changed = [], lv = o.level, seen = {}, ex = {};
    (exits || []).forEach(function (x) { if (x && x.id && !(x.at < o.nowS - EXIT_AGE)) ex[x.id] = x; });
    var ids = Object.keys((prev && prev[3] && prev[3].spots) || {}).concat(Object.keys((next && next[3] && next[3].spots) || {}));
    var add = function (tr) { tr.t0 = o.t; out.push(tr); };
    var move = function (id, kind, look, from, to, extra) {
      if (!from || !to) return;
      var tr = { id: id, kind: kind, look: look, from: from, to: to, dur: clamp(dist(from, to) / SPEED * 1000, RIDE), moving: kind === "ride" || kind === "rideback" || kind === "walkto" || kind === "walkout" };
      for (var k in extra) tr[k] = extra[k];
      add(tr);
    };
    /* a ride or walk between home and a plot spot, along the lanes when they reach it (pts out from home; back: reversed) */
    var trip = function (id, kind, look, from, to, pts, back, extra) {
      var e = extra || {};
      if (pts) { e.pts = back ? pts.slice().reverse() : pts; e.dur = clamp(length(e.pts) / SPEED * 1000, RIDE); }
      move(id, kind, look, from, to, e);
    };
    var burst = function (id, look, at, stuck, b0) {
      if (!b0) return;
      var arc = clamp(dist(at, b0) / SPEED * 1000, ARC);
      add({ id: id, kind: "burst", look: look, from: { x: at.x, y: at.y, face: at.face }, to: b0, arc: arc, dur: BURST + arc + PUFF, stuck: stuck, moving: false, h: arcHeight(at, b0) });
    };
    ids.forEach(function (id) {
      if (seen[id]) return; seen[id] = 1;
      var a = where(prev, lv, id), b = where(next, lv, id), la = prev && prev.look[id], lb = next && next.look[id];
      if (a.k !== b.k || a.plot !== b.plot || a.x !== b.x || a.y !== b.y || a.face !== b.face || a.down !== b.down || (la && lb && la.pose !== lb.pose)) changed.push(id);
      var sf;
      if (b.k === "plot" && (a.k === "none" || a.k === "home")) {   // it went to work: it walks out of its stand, or a truck picks it up there
        sf = stand(next, id, b);
        if (sf.near) trip(id, "walkout", lb, sf.at, b, lanes(next, id, b, false, sf.at), false);
        else trip(id, "ride", lb, door(next, id), b, sf.at && door(next, id) ? [door(next, id), sf.at, { x: b.x, y: b.y }] : lanes(next, id, b, true, door(next, id)), false);
      }
      else if (a.k === "plot" && b.k === "plot" && !a.down && !b.down && (a.plot !== b.plot || a.x !== b.x || a.y !== b.y)) {
        var pts = route(a, b);   // its task moved: it walks there along the paths (World Phase 3); the sprite at b waits for it
        move(id, "walkto", lb, pts[0], pts[pts.length - 1], { pts: pts, dur: clamp(length(pts) / SPEED * 1000, WALK) });
      }
      else if (a.k === "plot" && b.k === "plot" && a.plot === b.plot && a.down !== b.down) {
        if (b.down) burst(id, lb, b, true, home(next, id)); else add({ id: id, kind: "stand", look: lb, from: b, to: b, dur: STAND, moving: false });
      } else if (a.k === "plot" && b.k === "home") {   // it went idle: it walks back to its stand, or the truck drops it there and goes on to the barn
        sf = stand(next, id, a);
        if (sf.near) trip(id, "walk", la, a, sf.at, lanes(prev, id, a, false, sf.at), true);
        else trip(id, "rideback", la, a, door(next, id), sf.at && door(next, id) ? [door(next, id), sf.at, { x: a.x, y: a.y }] : lanes(prev, id, a, true, door(next, id)), true);
      }
      else if (a.k === "plot" && b.k === "none" && ex[id]) {
        if (ex[id].kind === "finished") trip(id, "walk", la, a, home(prev, id), lanes(prev, id, a, false, home(prev, id)), true, { tldr: ex[id].tldr || "", agent: ex[id].agent || "" });
        else burst(id, la, a, false, home(prev, id));
      } else if (a.k === "home" && b.k === "none" && ex[id]) {
        var s = W.homeTile(prev.world, prev.byId[id]);   // a puff only where its figure stood
        if (s && s.drawn) add({ id: id, kind: "puff", look: la, from: { x: s.x, y: s.y }, to: { x: s.x, y: s.y }, dur: PUFF * 2, moving: false });
      }
    });
    if (lv < 2 || out.length > MAX_PATCH) return { list: [], changed: changed };
    return { list: out.filter(function (tr) { tr.box = pathBox(tr); return !o.view || hit(tr.box, o.view); }), changed: changed };
  }
  function arcHeight(a, b) { return Math.max(32, Math.min(160, dist(a, b) * .3)); }
  /* The bounds of a transition's path in world px, the figure included (and the flight's arc above it). */
  function pathBox(tr) {
    var pts = tr.pts || [tr.from, tr.to], xs = pts.map(function (p) { return p.x; }), ys = pts.map(function (p) { return p.y; });
    var x0 = Math.min.apply(null, xs) - 8, y0 = Math.min.apply(null, ys) - (tr.h || 0) - 12;
    return { x: x0, y: y0, w: Math.max.apply(null, xs) + 24 - x0, h: Math.max.apply(null, ys) + 32 - y0 };
  }
  function hit(a, b) { return a.x < b.x + b.w && b.x < a.x + a.w && a.y < b.y + b.h && b.y < a.y + a.h; }

  /* at(tr, t) -> {done, hide, ops}: what one transition draws at time t (ms). hide is true while it stands in for the id's
     static sprite. ops, in draw order: {s: truck, x, y, dir} (the truck's 24 x 16 box), {s: walk, x, y, f}, {s: fly, x, y},
     {s: pose, pose, x, y, face} (the figure's top left and the side it faces), {s: part, x, y, c: H | S} (a particle's centre), {s: puff, x, y, k}. */
  function at(tr, t) {
    var e = Math.max(0, t - tr.t0), k = Math.min(1, e / tr.dur), ops = [], p;
    if (e >= tr.dur) return { done: true, hide: false, ops: ops };
    if (tr.kind === "ride" || tr.kind === "rideback") {
      p = tr.pts ? along(tr.pts, k) : lerp(tr.from, tr.to, k);
      ops.push({ s: "truck", x: Math.round(p.x) - 4, y: Math.round(p.y) + 10, dir: (tr.pts ? heading(tr.pts, k) : tr.to.x - tr.from.x) < 0 ? "l" : "r" });
      return { done: false, hide: tr.kind === "ride", ops: ops };
    }
    if (tr.kind === "walk" || tr.kind === "walkto" || tr.kind === "walkout") {
      p = tr.pts ? along(tr.pts, k) : lerp(tr.from, tr.to, k);
      ops.push({ s: "walk", x: Math.round(p.x), y: Math.round(p.y), f: Math.floor(e / STEP) % 2 });
      return { done: false, hide: tr.kind !== "walk", ops: ops };
    }
    if (tr.kind === "stand") { var up = e < tr.dur / 2; ops.push({ s: "pose", pose: up ? "down" : tr.look.pose, x: tr.to.x, y: tr.to.y + (up ? 6 : 3), face: tr.to.face }); return { done: false, hide: true, ops: ops }; }
    if (tr.kind === "puff") { ops.push({ s: "puff", x: tr.from.x + 8, y: tr.from.y + 16, k: k }); return { done: false, hide: false, ops: ops }; }
    // burst: the particles (and a scarecrow rising in its place), then the flight to the barn, then a puff there
    var c = { x: tr.from.x + 8, y: tr.from.y + 12 };
    if (e < BURST) {
      var q = e / BURST;
      if (tr.stuck) ops.push({ s: "pose", pose: "down", x: tr.from.x, y: tr.from.y + Math.round(12 * (1 - q)), face: tr.from.face });
      for (var i = 0; i < 8; i++) {
        var ang = i * Math.PI / 4 + .4, r = 4 + 26 * q;
        ops.push({ s: "part", x: c.x + Math.cos(ang) * r, y: c.y + Math.sin(ang) * r * .8 - 10 * q + 24 * q * q, c: i % 2 ? "S" : "H" });
      }
      return { done: false, hide: tr.stuck, ops: ops };
    }
    if (e < BURST + tr.arc) {
      var a = (e - BURST) / tr.arc; p = lerp(tr.from, tr.to, a);
      ops.push({ s: "fly", x: Math.round(p.x), y: Math.round(p.y - tr.h * 4 * a * (1 - a)) });
      return { done: false, hide: false, ops: ops };
    }
    ops.push({ s: "puff", x: tr.to.x + 8, y: tr.to.y + 12, k: (e - BURST - tr.arc) / PUFF });
    return { done: false, hide: false, ops: ops };
  }

  /* ---------- the page's side ---------- */
  if (typeof document !== "undefined" && document.addEventListener) {
    document.addEventListener("farmlive", function (e) {
      var d = e.detail || {};
      if (d.kind === "open") openAt = now(); else pending = { kind: d.kind, exits: d.exits || [] };
    });
  }
  function stop(tr) { running = running.filter(function (r) { return r !== tr; }); }
  function land() { if (landFn) landFn(); }
  /* One farm document landed: prev and next are WORLD.gardenerState before and after it. o = {level, view, reduce, cut}.
     Only a live patch animates (rule 2): not the first document, a reset, the catch-up after a (re)connect, or reduced
     motion, which cut every running transition to its end state. cut (world px rects, the old feet of the biomes the document
     slid or moved, homestead-world-design.md 29.11) cuts the running transitions that meet one, planned on the old rects. */
  function update(prev, next, o) {
    var p = pending, t = now(), nowS = t / 1000; pending = null;
    if (o.cut && o.cut.length) running = running.filter(function (tr) { return !o.cut.some(function (r) { return hit(tr.box, r); }); });
    if (p) (p.exits || []).forEach(function (x) { if (x.kind === "finished" && x.at >= nowS - FINISHED_S) finished.unshift({ agent: x.agent || x.id, tldr: x.tldr || "", at: x.at }); });
    finished = finished.filter(function (f) { return f.at >= nowS - FINISHED_S; }).slice(0, 3);
    if (!p || p.kind !== "patch" || t - openAt < CATCH_UP || (o.reduce && !force) || !prev || !next) { running = []; return; }
    var r = plan(prev, next, p.exits, { level: o.level, view: o.view, t: t, nowS: nowS }), cut = {};
    r.changed.forEach(function (id) { cut[id] = 1; });
    running = running.filter(function (tr) { return !cut[tr.id]; }).concat(r.list).slice(0, MAX_RUN);
  }
  /* Draws every running transition in world px (the caller has set the world transform) and returns true while any runs, so
     the page asks for the next frame (rule 5). o = {level, night, sp, cam, vp}. Below the Plan level every one is cut. A
     finished walk shows its tldr over its home tile for LABEL_MS. */
  function draw(ctx, o) {
    var t = now(), landed = false;
    if (o.level < 2 && running.length) { landed = running.some(function (tr) { return tr.moving; }); running = []; }
    running.slice().forEach(function (tr) {
      var f = at(tr, t);
      if (f.done) { stop(tr); if (tr.moving) landed = true; if (tr.kind === "walk" && tr.tldr) label(tr, t); return; }
      f.ops.forEach(function (op) {
        if (op.s === "truck") ART.drawTruck(ctx, tr.look, op.x, op.y, op.dir, o.night, o.sp);
        else if (op.s === "walk") ART.drawWalker(ctx, tr.look, op.x, op.y, op.f, o.night, o.sp);
        else if (op.s === "fly") ART.drawFlyer(ctx, tr.look, op.x, op.y, o.night, o.sp);
        else if (op.s === "pose") ART.drawGardener(ctx, { pose: op.pose, group: tr.look.group, shirt: tr.look.shirt, hat: tr.look.hat }, op.x, op.y, 0, o.night, o.sp, op.face);
        else if (op.s === "part") ART.drawParticle(ctx, tr.look, op.x, op.y, op.c, o.night);
        else ART.drawPuff(ctx, op.x, op.y, op.k, o.night);
      });
    });
    placeLabels(o, t);
    if (landed) land();
    return running.length > 0;
  }
  /* The arrival label: a DOM label over the walk's end (its home tile) with the tldr cut at TLDR_MAX characters, gone after LABEL_MS. */
  function label(tr, t) {
    lblsEl = lblsEl || document.getElementById("lbls");
    if (!lblsEl) return;
    var e = document.createElement("span");
    e.className = "lbl w-tldr"; e.textContent = clip(tr.tldr, TLDR_MAX);
    lblsEl.appendChild(e);
    var l = { e: e, x: tr.to.x + 8, y: tr.to.y - 4, until: t + LABEL_MS };
    labels.push(l);
    setTimeout(function () { e.remove(); labels = labels.filter(function (x) { return x !== l; }); }, LABEL_MS);
  }
  /* Each label centred over its anchor, kept inside the board (a phone's view can have the barn near its edge). */
  function placeLabels(o, t) {
    labels.forEach(function (l) {
      var x = (l.x - o.cam.cx) * o.cam.z + o.vp.w / 2, y = (l.y - o.cam.cy) * o.cam.z + o.vp.h / 2, show = o.level >= 2 && t < l.until;
      l.e.style.display = show ? "" : "none";
      if (!show) return;
      var hw = (l.e.offsetWidth || 0) / 2;
      if (hw && o.vp.w > 2 * hw + 8) x = Math.max(hw + 4, Math.min(o.vp.w - hw - 4, x));
      l.e.style.transform = "translate(" + Math.round(x) + "px," + Math.round(y) + "px) translate(-50%,-100%)";
    });
  }

  return {
    SPEED: SPEED, RIDE: RIDE, WALK: WALK, ARC: ARC, BURST: BURST, PUFF: PUFF, STAND: STAND, STEP: STEP, LABEL_MS: LABEL_MS, CATCH_UP: CATCH_UP, EXIT_AGE: EXIT_AGE, MAX_PATCH: MAX_PATCH, MAX_RUN: MAX_RUN,
    now: now, plan: plan, at: at, where: where, route: route, along: along, heading: heading, door: door, home: home, update: update, draw: draw,
    /* is a transition standing in for id's sprite on its plot right now / is id still on its way (no link, no barn slot)? */
    /* the figures the running transitions draw now, each its top left {id, x, y} (world px): a bird's plate keeps off their faces */
    faces: function () { var t = now(), out = []; running.forEach(function (tr) { at(tr, t).ops.forEach(function (op) { if (op.s === "walk" || op.s === "pose" || op.s === "fly") out.push({ id: tr.id, x: op.x, y: op.y }); }); }); return out; },
    hides: function (id) { var t = now(); return running.some(function (tr) { return tr.id === id && at(tr, t).hide; }); },
    moving: function (id) { return running.some(function (tr) { return tr.id === id && tr.moving; }); },
    still: function (it) { return !running.some(function (tr) { return tr.id === it.id && tr.moving; }); },
    /* what the barns and their cards depend on: who is still moving, and who just finished */
    sig: function () { return running.filter(function (tr) { return tr.moving; }).map(function (tr) { return tr.id; }).sort().join(",") + "|" + finished.map(function (f) { return f.at + f.agent; }).join(","); },
    finished: function () { var f = finished[0]; return f ? " Just finished: " + f.agent + (f.tldr ? ": " + f.tldr : "") + "." : ""; },
    onLand: function (fn) { landFn = fn; },
    /* test hooks (the goldens and the frame budget): pin the clock, animate under reduced motion, read what runs */
    pin: function (ms) { pinned = ms === undefined ? null : ms; },
    motion: function (on) { force = !!on; },
    list: function () { return running.map(function (tr) { return { id: tr.id, kind: tr.kind, t0: tr.t0, dur: tr.dur, arc: tr.arc || 0, burst: BURST, from: tr.from, to: tr.to, pts: tr.pts || null }; }); },
    opened: function () { return openAt; }
  };
})();
if (typeof module !== "undefined") module.exports = ANIM;
