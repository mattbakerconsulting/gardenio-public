/* A land window's Scene laid out by the World's pure functions, run by TestSceneLaysOutUnderNode:
   node scene_layout.js <gardenhub web/world.js> <scene.json>
   Lays the scene out read-only, places its gardeners at the Plan and Bead levels and builds the project board, and prints
   what landed where as JSON for the Go test to hold. */
"use strict";
var assert = require("assert"), fs = require("fs");
var W = require(process.argv[2]), doc = JSON.parse(fs.readFileSync(process.argv[3], "utf8"));
var world = W.layout(doc, null, { readonly: true, now: 1 });
assert.strictEqual(world.memoChanged, false, "a read-only layout writes no memo");
assert.strictEqual(world.biomes.length, 1, "one land, one biome");
var b = world.biomes[0], st = W.gardenerState(world, doc.gardeners), bd = W.board(b, st[3].plots, null);
var ids = function (xs) { return xs.map(function (x) { return x.id; }); };
var out = {
  biome: { key: b.key, name: b.name, needs: b.needs },
  plots: b.plots.map(function (p) {
    return { id: p.id, staked: !!p.staked, needs: p.needs, beds: p.beds.map(function (bed) { return { title: bed.title, cells: ids(bed.cells), done: bed.done, total: bed.total, closed: bed.closed }; }) };
  }),
  nursery: ids(b.nursery), orchard: ids(b.orchard),
  gardeners: {}, board: { head: bd.head, rows: bd.rows.map(function (r) { return r.id; }) }
};
doc.gardeners.forEach(function (g) { var s = st[3].spots[g.id]; out.gardeners[g.id] = s.plot ? s.plot : "home"; });
console.log(JSON.stringify(out));
