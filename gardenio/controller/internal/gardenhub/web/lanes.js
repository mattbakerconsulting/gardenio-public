/* LANES: who uses a lane (homestead-world-design.md 29.3, X-D11): the ways the release truck, the ride truck and a walking
   gardener take along a home biome's four lanes, out of the bay or the farmstead's gate and in at a plot (gd-tl9bVIBKTZ).
   Pure; runs under node (transitions_test.go, truck_test.go). Units: tiles inside, world px for a figure's way. */
var LANES = (function () {
  "use strict";
  var W = typeof WORLD !== "undefined" ? WORLD : require("./world.js");
  var TILE = W.TILE, LANE = W.LANE, SPIRAL = W.SPIRAL;
  /* A way is a list of points in tiles, x a column's middle and y where the feet or wheels are, so a point on a lane's middle
     line is on that lane's tiles; a net point also names its lane (on). */
  /* laneLines(b) -> the middle line of each of biome b's four lanes, tiles: {west, east (x), north, south (y)}; null without them. */
  function laneLines(b) {
    var out = {}, n = 0;
    (b && b.lanes || []).forEach(function (l) { n++; out[l.name] = l.name === "west" || l.name === "east" ? l.x + l.w / 2 : l.y + l.h / 2; });
    return n === 4 ? out : null;
  }
  function wayLen(pts) { var d = 0; for (var i = 1; i < pts.length; i++) d += Math.abs(pts[i].x - pts[i - 1].x) + Math.abs(pts[i].y - pts[i - 1].y); return d; }
  function upright(on) { return on === "west" || on === "east"; }
  /* laneWay(L, a, z) -> the way from net point a to net point z on lines L (laneLines), both included: along one lane, through
     the crossroads where two lanes cross, or between two parallel lanes over whichever crossing lane is shorter. */
  function laneWay(L, a, z) {
    if (a.on === z.on) return [a, z];
    var cross = function (p, q) { return upright(p) ? { x: L[p], y: L[q] } : { x: L[q], y: L[p] }; };
    if (upright(a.on) !== upright(z.on)) return [a, cross(a.on, z.on), z];
    var ways = (upright(a.on) ? ["north", "south"] : ["west", "east"]).map(function (o) { return [a, cross(a.on, o), cross(z.on, o), z]; });
    return wayLen(ways[0]) <= wayLen(ways[1]) ? ways[0] : ways[1];
  }
  /* plotDoor(L, p, s) -> {at, legs}: where a way into plot p to its spot s (tiles) leaves the lanes. A plot in the three middle
     rows has a north or south lane along it: the way turns in off that lane at s's column. One further out goes along the
     west or east lane on its side to the plot gap on the farm's side of it, along that gap to s's column and in. */
  function plotDoor(L, p, s) {
    var q = SPIRAL[p.slot], on, gy;
    if (Math.abs(q.r) <= 1) { on = q.r < 0 ? "north" : q.r > 0 ? "south" : Math.abs(s.y - L.north) <= Math.abs(s.y - L.south) ? "north" : "south"; return { at: { x: s.x, y: L[on], on: on }, legs: [s] }; }
    on = q.c < 0 ? "west" : q.c > 0 ? "east" : Math.abs(s.x - L.west) <= Math.abs(s.x - L.east) ? "west" : "east";
    gy = q.r > 0 ? p.y - LANE / 2 : p.y + p.h + LANE / 2;
    return { at: { x: L[on], y: gy, on: on }, legs: [{ x: s.x, y: gy }, s] };
  }
  /* The shortest of the ways out, each a list of points ending on a net point, on to plot p's spot s; null without lanes. */
  function viaLanes(b, outs, p, s) {
    var L = laneLines(b), best = null;
    if (!L) return null;
    var d = plotDoor(L, p, s);
    outs(L).forEach(function (o) { var w = o.slice(0, -1).concat(laneWay(L, o[o.length - 1], d.at), d.legs); if (!best || wayLen(w) < wayLen(best)) best = w; });
    return best;
  }
  /* figure (a 16 x 24 figure's top left, world px) <-> foot (its feet's point, tiles); a way's repeated points dropped */
  function foot(f) { return { x: (f.x + 8) / TILE, y: (f.y + 24) / TILE }; }
  function figure(pts) {
    var out = [];
    pts.forEach(function (p) { var q = { x: Math.round(p.x * TILE - 8), y: Math.round(p.y * TILE - 24) }, l = out[out.length - 1]; if (!l || l.x !== q.x || l.y !== q.y) out.push(q); });
    return out;
  }
  /* The biome and plot a way of biome id to plot id runs in: both laid out, the plot that biome's; else null (the way is straight). */
  function laneFarm(world, biomeId, plotId) {
    var ix = world && world.index || {}, b = ix[biomeId], p = ix[plotId];
    return b && b.kind === "biome" && b.home && p && p.kind === "plot" && "project:" + p.project === b.id ? { b: b, p: p } : null;
  }
  /* walkLanes(world, biomeId, from, plotId, to) -> the walk from a gardener's home tile `from` in biome biomeId (homeTile's) to its
     spot `to` on plot plotId, figure top lefts (world px), or null when the plot is not that biome's: out of the farmstead by the
     gate (the end of an arm: west and east on its row, north at its north arm, south at its south arm) that makes the way shortest, along the lanes and in at the plot (plotDoor). The walk home is
     this way reversed. */
  function walkLanes(world, biomeId, from, plotId, to) {
    var f = laneFarm(world, biomeId, plotId);
    if (!f) return null;
    var h = foot(from), x = f.b.home.cross, XN = x.xn + 0.5, XS = x.xs + 0.5, Y = x.y + 0.5;
    var w = viaLanes(f.b, function (L) {   // from a field stand (28.12): north up to the east-west arm and along it to the north arm, south along its row to the south arm (29.10)
      return [[h, { x: h.x, y: Y }, { x: XN, y: Y }, { x: XN, y: L.north, on: "north" }], [h, { x: XS, y: h.y }, { x: XS, y: L.south, on: "south" }]]
        .concat(["west", "east"].map(function (on) { return [h, { x: h.x, y: Y }, { x: L[on], y: Y, on: on }]; }));
    }, f.p, foot(to));
    return w && figure(w);
  }
  /* bayOut(b) -> the way out of biome b's truck bay (28.10, 29.2): its middle, then due west onto the west lane, whose net point
     ends it; tiles. The release truck and the ride truck both leave by it. */
  function bayOut(b, L) { var y = b.home.bay, my = y.y + y.h / 2; return [{ x: y.x + y.w / 2, y: my }, { x: L.west, y: my, on: "west" }]; }
  /* rideLanes(world, biomeId, door, plotId, to) -> the ride truck's way from the barn's door (barnDoor) to spot `to` on plot
     plotId, as walkLanes's: down into the bay, out of it west onto the west lane, along the lanes and in at the plot; null when
     the plot is not that biome's. The ride home is this way reversed. */
  function rideLanes(world, biomeId, door, plotId, to) {
    var f = laneFarm(world, biomeId, plotId);
    if (!f) return null;
    var d = foot(door), w = viaLanes(f.b, function (L) { var o = bayOut(f.b, L); return [[d, { x: d.x, y: o[0].y }].concat(o.slice(1))]; }, f.p, foot(to));
    return w && figure(w);
  }
  return { laneLines: laneLines, laneWay: laneWay, plotDoor: plotDoor, bayOut: bayOut, walkLanes: walkLanes, rideLanes: rideLanes };
})();
if (typeof module !== "undefined") module.exports = LANES;
