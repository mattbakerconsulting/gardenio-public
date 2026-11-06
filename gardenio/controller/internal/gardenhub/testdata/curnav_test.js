/* The page's cursor (web/curnav.js; gardenio/docs/superpowers/specs/2026-10-20-lands-cursor-grid-design.md sections 3.2, 3.5,
   3.6 and 4.3), run by TestCurNavUnderNode:  node curnav_test.js <web dir>
   Over WORLD.layout() of the 10x farm (testdata/world_tenx.js), on a project with several plans: the key map; the opening cursor (an at=, the saved path, else depth 0 on the first
   project); moves, descends and ascends keeping a per-grid memory that a descend honours; the saved copy round-tripping;
   cursor jumps and the needs walk; the grid's cells with their 1-based row and column, its name and the words a cell's name
   ends with; the live region's sentence; each strip's zones from the page's model, a ledger patch rebuilding only its home
   cell, and the Home key; the outline's rows in reading order; and
   the pointer's pick. Prints a JSON report. */
"use strict";
var path = require("path"), assert = require("assert");
var W = require(path.join(process.argv[2], "world.js")), C = require(path.join(process.argv[2], "cursor.js")), N = require(path.join(process.argv[2], "curnav.js")), ART = require(path.join(process.argv[2], "art.js"));
var doc = require(path.join(__dirname, "world_tenx.js")).tenx();
var world = W.layout(doc, null, { now: 1792001000000 }), report = {};
/* a project with several plans: plan A and plan B are its first two, KEY its at= segment */
var PB = world.biomes.filter(function (b) { return b.key && b.plots.length >= 3 && b.plots[0].beds.length >= 2; })[0], KEY = encodeURIComponent(PB.key), PID = PB.id;
var PA = PB.plots[0].id, PBB = PB.plots[1].id;
function last(a) { return a[a.length - 1]; }

/* ---------- 1. the keys (spec 3.2) ---------- */
var key = function (k, shift) { return N.action({ key: k, shiftKey: !!shift }); };
assert.deepStrictEqual(key("ArrowLeft"), { act: "move", dir: "left" });
assert.deepStrictEqual(key("ArrowDown", true), { act: "jump", dir: "down" }, "Shift+arrow is the sibling jump");
["Enter", "+", "="].forEach(function (k) { assert.deepStrictEqual(key(k), { act: "in" }, k + " goes in"); });
["Escape", "-"].forEach(function (k) { assert.deepStrictEqual(key(k), { act: "out" }, k + " comes out"); });
["x", "z", "Backspace", "_"].forEach(function (k) { assert.strictEqual(key(k), null, k + " is no longer a cursor key (3b R6)"); });
assert.deepStrictEqual(key(" "), { act: "open" });
assert.deepStrictEqual(key("0"), { act: "top" });
assert.deepStrictEqual(key("Home"), { act: "home", edge: true });
assert.deepStrictEqual(key("End"), { act: "end" });
assert.deepStrictEqual([key("n"), key("N", true)], [{ act: "need", step: 1 }, { act: "need", step: -1 }]);
assert.deepStrictEqual(key("/"), { act: "find" });
assert.strictEqual(key("q"), null, "any other key is not the cursor's");

/* ---------- 2. each strip from the page's model, and the tree (homestead-world-design.md 28.2, 28.6) ---------- */
var model = { decision: [{ slot: 0, item: { id: "gd-dec1", title: "Answer me", state: "open", type: "decision", u: 1792000000, project: PB.key } }], growing: [{ slot: 2, item: { id: "gd-grow1", title: "Growing", state: "in_progress", type: "task", u: 1792000000, project: PB.key } }], agent: [] };
var slots = {}, home = N.homeSpec(world, model, slots), HID = "home:" + PB.key, dec = home[PID].mailbox.items[0], mbx = PB.home.cells.mailbox;
assert.deepStrictEqual([dec.id, dec.kind, dec.slot, dec.project], ["gd-dec1", "letter", 0, PB.key], "a letter in its own project's Mailbox (WORLD.homeFill)");
assert.ok(dec.x > mbx.x && dec.x < mbx.x + mbx.w && dec.y > mbx.y && dec.y < mbx.y + mbx.h, "inside the Mailbox cell");
assert.strictEqual(slots[PB.key].mailbox["gd-dec1"], 0, "its slot is kept for the next patch");
assert.deepStrictEqual(N.homeSpec(null, model, {}), {}, "no layout yet: no strips");
var nav = N.nav();
nav.build(world, home, { now: 1792001000000 });
assert.strictEqual(nav.tree.nodes[HID + "/mailbox"].status, "1 decision waiting");
assert.ok(world.index["project:"], "the 10x farm has a Commons");

/* a ledger patch on the same layout rebuilds only the home cells it touched: the projects' nodes and the other strips are the
   same objects (28.6) */
var keep = nav.tree.nodes[world.biomes[0].id], oh = world.biomes.filter(function (b) { return b.id !== PID; })[0].home.id, other = nav.tree.nodes[oh], model2 = { decision: [], growing: model.growing, agent: [] };
nav.path = C.pathFor(nav.tree, "gd-dec1");
assert.deepStrictEqual(nav.path, [PID, HID, HID + "/mailbox", "gd-dec1"]);
var t0 = nav.tree;
assert.strictEqual(nav.patch(N.homeSpec(world, model2, slots), {}), true, "a patch that changed a strip says so");
assert.ok(nav.tree === t0 && nav.tree.nodes[world.biomes[0].id] === keep && nav.tree.nodes[oh] === other, "the layout's nodes and the untouched strips are kept");
assert.ok(!nav.tree.nodes["gd-dec1"] && !nav.tree.nodes[HID + "/mailbox"], "the answered letter is gone, and its empty Mailbox is no stop (N-D3)");
assert.deepStrictEqual(nav.path.slice(0, 3), [PID, HID, HID + "/growing"], "the cursor on it lands in the nearest zone: " + nav.path.join(" > "));
assert.deepStrictEqual(C.pathFor(nav.tree, "gd-grow1"), [PID, HID, HID + "/growing", "gd-grow1"], "the rest of the strip is there");
assert.strictEqual(nav.patch(N.homeSpec(world, model2, slots), {}), false, "nothing changed: nothing rebuilt");
nav.patch(N.homeSpec(world, model, slots), {});
assert.deepStrictEqual(C.pathFor(nav.tree, "gd-dec1"), [PID, HID, HID + "/mailbox", "gd-dec1"], "and back");

/* the Home key (28.6, H-D11): the cursor's project's home cell from any depth, then the Commons's, which bumps */
nav.open(KEY + "/" + PA, null);
nav.descend([]);
assert.strictEqual(nav.home().bumped, false);
assert.deepStrictEqual(nav.path, [PID, HID], "Home: the home cell of the project the cursor is in");
nav.home();
assert.deepStrictEqual(nav.path, ["project:", "home:"], "a second Home: the Commons's");
assert.strictEqual(nav.home().bumped, true, "on the Commons's: a bump");
nav.open("~", null); nav.home();
assert.deepStrictEqual(nav.path, ["project:", "home:"], "on the Commons: the Commons's");

/* the Escalations (gd-AmIpCiVTvY): the zone's grid is named for what its cells' state words say; Home and End (the keys) go to
   its first and last cell, a second Home on to the home cell; the Menu's Home is the home cell at once; End anywhere else
   bumps; Escape comes out; and the title bar on the phone's tab bar reads the cursor escalation's sign tip */
var NOWS = 1792001000, escDoc = Object.assign({}, doc, { escalations: { open: [
  { id: "gd-eY1", title: "Merge the fix", kind: "escalation", state: "pending", human: true, since: NOWS - 7200, project: PB.key, family: "merge" },
  { id: "gd-eY2", title: "Grant read", kind: "escalation", state: "pending", human: true, since: NOWS - 600, project: PB.key, family: "access" },
  { id: "gd-eY3", title: "Roll the chart", kind: "escalation", state: "approved", human: false, waits_on: "the deployer", since: NOWS - 300, project: PB.key, family: "deploy" },
  { id: "gd-eY4", title: "Apply infra", kind: "escalation", state: "approved", human: false, since: NOWS - 900, project: PB.key, family: "infra" },
  { id: "gd-eY5", title: "Install prompt", kind: "escalation", state: "rejected", human: false, since: NOWS - 60, project: PB.key, family: "prompt" }] } });
var escWorld = W.layout(escDoc, null, { now: NOWS * 1000 }), en = N.nav(), EZ = [PID, HID, HID + "/escalation"];
en.build(escWorld, N.homeSpec(escWorld, { decision: [], growing: [], agent: [] }, {}), { now: NOWS * 1000 });
assert.ok(en.to("gd-eY3").found, "the cursor reaches an escalation");
assert.deepStrictEqual(en.path.slice(0, 3), EZ, "in its project's Escalations");
assert.strictEqual(en.cells().label, "Escalations, 2 waiting on you, 2 running, 1 rejected or failed", "the zone's name counts its cells");
var escIds = en.cells().cells.map(function (c) { return c.node.id; }), escFirst = escIds[0], escLast = last(escIds);
assert.strictEqual(en.home(true).bumped, false); assert.strictEqual(last(en.path), escFirst, "Home: the first escalation");
assert.strictEqual(en.end().bumped, false); assert.strictEqual(last(en.path), escLast, "End: the last");
assert.strictEqual(en.end().bumped, true, "End on the last: a bump");
en.home(true); en.home(true);
assert.deepStrictEqual(en.path, [PID, HID], "Home from the first: the home cell");
assert.strictEqual(en.end().bumped, true, "End outside the Escalations: a bump");
en.to("gd-eY3"); en.home();
assert.deepStrictEqual(en.path, [PID, HID], "the Menu's Home: the home cell at once");
en.to("gd-eY3"); en.ascend();
assert.deepStrictEqual(en.path, EZ, "Escape: out to the Escalations");
en.to("gd-eY3");
var eb = en.bar(escWorld.index, {});
assert.deepStrictEqual(eb.parts.map(function (p) { return p.name; }).slice(-2), ["Escalations", "Roll the chart"]);
assert.strictEqual(eb.state, "Deploy escalation, approved, running, waits on the deployer, since 5 min, from project " + PB.key, "the bar reads the sign tip: " + eb.state);
report.escalations = { label: en.cells().label, bar: eb.state };

/* ---------- 3. the opening cursor (spec 3.5) ---------- */
var firstProject = nav.tree.root.kids.filter(function (id) { return nav.tree.nodes[id].kind === "biome" && nav.tree.nodes[id].k >= 1; })[0];
assert.strictEqual(nav.open("", null), false);
assert.deepStrictEqual(nav.path, [firstProject], "no link and nothing saved: depth 0 on the first project after the Commons (28.5)");
assert.strictEqual(nav.open(KEY + "/" + PA, null), true, "an at= is found");
assert.deepStrictEqual(nav.path, C.pathFor(nav.tree, PA), "the at= names the cursor's Thing");
assert.strictEqual(nav.path.length, 2, "a plan is depth 1");
assert.strictEqual(nav.open(KEY + "/gd-gone", null), true, "a gone id falls back to its project, found");
assert.deepStrictEqual(nav.path, [PID]);
assert.strictEqual(nav.open("nowhere-at-all/gd-x", null), false, "an unknown project is not found");
var mem = { bad: 7 }; mem[PID] = PBB;
var saved = JSON.stringify({ path: [PID, PA, "stale-id-in-between", PBB], memory: mem });
nav.open("", saved);
assert.deepStrictEqual(nav.path, C.pathFor(nav.tree, PBB), "the saved path resolves by its last id");
assert.strictEqual(nav.memory[PID], PBB, "the saved memory comes back");
assert.ok(!("bad" in nav.memory), "only string memories are read back");
nav.open("", "{not json");
assert.deepStrictEqual(nav.path, [firstProject], "a broken save is ignored");

/* ---------- 4. moving, memory, save (spec 3.2, N-D4) ---------- */
nav = N.nav(); nav.build(world, home, {}); nav.open(KEY + "/" + PA, null);
var d = nav.descend([]);
assert.ok(!d.bumped && d.path.length === 3 && d.path[1] === PA, "Enter on a plan goes into its phases");
var bed = last(nav.path);
var m = nav.move("right"), moved = !m.bumped;
var left = last(nav.path);
nav.ascend();
assert.deepStrictEqual(nav.path, C.pathFor(nav.tree, PA), "Esc comes out to the plan");
assert.strictEqual(nav.memory[PA], left, "the plan remembers the phase the cursor left");
nav.descend([]);
assert.strictEqual(last(nav.path), left, "and a descend lands there again");
report.memory = { first_bed: bed, moved: moved, left: left };
var back = N.nav(); back.build(world, home, {}); back.open("", nav.save());
assert.deepStrictEqual(back.path, nav.path, "the saved copy restores the path");
assert.strictEqual(back.memory[PA], left, "and the memory");
nav.top();
assert.deepStrictEqual(nav.path, [PID], "0 is depth 0 on the cursor's project");
assert.strictEqual(nav.top().bumped, true, "at depth 0, 0 bumps");
assert.strictEqual(nav.ascend().bumped, true, "and so does Esc");
var mv = nav.move("left"), col = nav.col;
assert.ok(mv.bumped || nav.path.length === 1, "a move stays at its depth");
assert.ok(col === undefined || typeof col === "number");
/* the memory is capped, oldest first */
var big = N.nav(); big.build(world, home, {});
for (var i = 0; i < N.MEMORY_MAX + 20; i++) big.memory["m" + i] = "x";
big.open(KEY + "/" + PA, null);
assert.ok(Object.keys(big.memory).length <= N.MEMORY_MAX, "the memory holds at most " + N.MEMORY_MAX + " grids");
assert.ok(!("m0" in big.memory) && big.memory[PID] === PA, "the oldest go, the newest stay");

/* ---------- 5. cursor jumps and the needs walk (section 8) ---------- */
var j = nav.to(PBB);
assert.ok(j.found && !j.bumped && last(nav.path) === PBB, "a cursor jump to a plan");
j = nav.to(PBB);
assert.ok(j.found && j.bumped, "a jump to where the cursor is: found, and nothing moves");
j = nav.to("gd-nowhere-at-all");
assert.ok(!j.found && last(nav.path) === PBB, "a jump to nothing leaves the cursor");
var needs = [PA, HID + "/mailbox", PBB];
nav.to("~/home");
assert.strictEqual(last(nav.need(needs, 1).path), PA, "n from elsewhere: the first that needs you");
assert.strictEqual(last(nav.need(needs, 1).path), HID + "/mailbox", "then the next");
assert.strictEqual(last(nav.need(needs, -1).path), PA, "N walks back");
assert.strictEqual(last(nav.need(needs, -1).path), PBB, "and wraps");
assert.strictEqual(nav.need([], 1).bumped, true, "nothing needs you: a bump");

/* ---------- 6. the grid's cells and words (spec 3.6) ---------- */
nav.open(KEY + "/" + PA, null);
var all = nav.cells(), g = nav.tree.nodes[PID];
assert.strictEqual(all.grid, g, "the current grid is the cursor's container");
assert.deepStrictEqual(all.cells.map(function (c) { return c.node.id; }), g.kids, "its occupied cells, in reading order");
all.cells.forEach(function (c) {
  assert.ok(c.row >= 1 && c.row <= all.rows, "row in 1.." + all.rows + ": " + c.row);
  assert.ok(c.node.span ? c.col === 0 : c.col >= 1 && c.col <= all.cols, "column in 1.." + all.cols + " (0 for a spanning band): " + c.col);
});
assert.ok(all.cells.some(function (c) { return c.row === 1; }) && all.cells.some(function (c) { return c.col <= 1; }), "indices count from the used bounds (a band spans them all)");
assert.strictEqual(all.label, PB.name + ": " + all.cells.length + " plan" + (all.cells.length === 1 ? "" : "s") + ", " + all.rows + " by " + all.cols, "the grid's name");
var cell = all.cells.filter(function (c) { return c.node.id === PA; })[0];
assert.strictEqual(nav.where(cell, all), ", row " + cell.row + " of " + all.rows + ", column " + cell.col + " of " + all.cols);
assert.strictEqual(nav.where({ row: 2, col: 0 }, { rows: 3, cols: 7 }), ", row 2 of 3", "a spanning cell has no column");
/* the words around a name read as sentences, never ".," (gd-jhE8MpXvJm) */
assert.strictEqual(N.sentences("Ground rules: 0/1 steps", " Arms on sand: compass centre."), "Ground rules: 0/1 steps. Arms on sand: compass centre.");
assert.strictEqual(N.sentences("Ground rules: 0/1 steps", ""), "Ground rules: 0/1 steps", "nothing follows: no full stop added");
assert.strictEqual(N.sentences("Done.", " Arms on sand: compass centre."), "Done. Arms on sand: compass centre.", "a name that ends a sentence keeps its one stop");
assert.strictEqual(N.sayAt("Ground rules: 0/1 steps", ", row 1 of 3"), "Ground rules: 0/1 steps, row 1 of 3");
assert.strictEqual(N.sayAt("Arms on sand: compass centre.", ", row 1 of 3, column 2 of 4"), "Arms on sand: compass centre. Row 1 of 3, column 2 of 4.");
assert.strictEqual(nav.say("move"), "", "a plain move announces nothing");
nav.descend([]);
var on = nav.node();
assert.strictEqual(nav.say("in"), "Inside " + nav.tree.nodes[PA].title + ": " + nav.cells().rows + " by " + nav.cells().cols + " phases, on phase " + on.ordinal + ", " + on.title + ".", "a descend announces once: " + nav.say("in"));
nav.top();
assert.ok(/^The farm: \d+ by \d+ places, on /.test(nav.say("top")), nav.say("top"));
report.grid = { label: all.label, say_in: "Inside …" };

/* ---------- 7. the outline's rows, from the tree (spec 3.6) ---------- */
var rows = N.outline(nav.tree);
assert.deepStrictEqual(rows.map(function (r) { return r.id; }), nav.tree.root.kids, "the roots in the lattice's reading order, cell 0 first");
assert.strictEqual(rows[0].kind, "biome");
(function walk(list) { list.forEach(function (r) { var n = nav.tree.nodes[r.id]; assert.strictEqual(r.hot, r.id); assert.strictEqual(r.sub, n.status || ""); assert.deepStrictEqual(r.children.map(function (k) { return k.id; }), n.kids); walk(r.children); }); })(rows);
assert.deepStrictEqual(N.outline(null), []);
// extra rows (28.7): a home cell's Gardeners group after its zones; one whose node is not in the tree is a root row
var hid = Object.keys(nav.tree.nodes).filter(function (id) { return nav.tree.nodes[id].kind === "home"; })[0], gx = { kind: "gardeners", id: "gardeners:x", hot: "gardeners:x", title: "Gardeners", children: [] };
var xr = N.outline(nav.tree, (function () { var o = { "home:nowhere": gx }; o[hid] = gx; return o; })()), hp = nav.tree.nodes[hid].parent;
var hr = xr.filter(function (r) { return r.id === hp; })[0].children.filter(function (r) { return r.id === hid; })[0];
assert.deepStrictEqual(hr.children.map(function (k) { return k.id; }), nav.tree.nodes[hid].kids.concat(["gardeners:x"]), "the group after the home cell's zones");
assert.deepStrictEqual(xr.slice(rows.length).map(function (r) { return r.id; }), ["gardeners:x"], "a group with no home cell in the tree: a root row");

/* ---------- 8. what a pointer, a tap and a zoom ask of the cursor (spec 4.3, 7) ---------- */
var gb = nav.tree.nodes[PID].rect, T = W.TILE;
var mid = function (id) { var r = nav.tree.nodes[id].rect; return [(r.x + r.w / 2) * T, (r.y + r.h / 2) * T]; };
nav.open(KEY + "/" + PA, null);
var pb = mid(PBB);
assert.strictEqual(nav.pick(pb[0], pb[1]), PBB, "pick: the plot under a point, at the cursor's depth");
assert.ok(nav.inGrid(PBB) && !nav.inGrid(PID), "inGrid: a plot is a cell of the cursor's grid, its project is not");
assert.strictEqual(nav.pick((gb.x + gb.w / 2) * T, (gb.y - 1000) * T), null, "pick: nothing on bare ground off every region");
var ownBed = nav.tree.nodes[PA].kids[0], ob = mid(ownBed);
assert.strictEqual(nav.pick(ob[0], ob[1]), PA, "pick goes no deeper than the cursor: a bed of the cursor's plot is the plot");
var other = world.biomes.filter(function (b) { return b.id !== PID && b.plots.length; })[0], op = mid(other.plots[0].id);
assert.strictEqual(nav.pick(op[0], op[1]), other.plots[0].id, "pick: a plot in another project, outside the current grid");

report.ok = true;
process.stdout.write(JSON.stringify(report) + "\n");
