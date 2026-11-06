package gardenhub

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// A bead in a field keeps its cell once placed (gd-xhpccozIcC, plan gd-HuLo1VPwsC step 4). world.js planBeds takes the
// plan's placement memo from the last layout (opts.cells, kept per tab in sessionStorage by garden.js) and gives it back
// pruned to the document's plans: reordering the same steps, a status or title change, a new step (even one older than the
// planted ones, which the created-then-id plant order alone would move ahead), and a close all leave every planted bead
// where it was; a new bead takes the lowest free cell; a bead planted before and listed no longer holds its cell as a dim
// marker (bed.gone) rather than closing the gap, the stubble count giving it up; a full bed reclaims its lowest markers;
// a cold layout (no memo) plants exactly as before. The memo survives a round trip through JSON, ignores garbage, is read
// but not written on a readonly layout, and reports cellsChanged only when it changed.
func TestBedCellsKeepTheirPlaceUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `
var W = require(process.argv[1]), assert = require("assert");
function doc(plans) { return { plans: plans, seeds: [], closed_plans: { total: 0, shown: [] }, meadow: { total: 0, shown: [] }, meta: { truncated: false } }; }
function step(id, created, state) { return { id: id, title: "Step " + id, type: "task", state: state || "open", created: created, updated: created }; }
function plan(tasks, closed, more) {
  var c = { open: tasks.length + (more || 0), closed: closed.length };
  return { id: "pl", title: "Plan", project: "beds", created: 1, counts: c, unphased: null, phases: [{ id: "ph", title: "Phase", status: "open", counts: c, more: more || 0, tasks: tasks, closed: closed }] };
}
function cellsOf(w) { var out = {}; w.index.pl.beds[0].cells.forEach(function (c) { out[c.id] = c.i; }); return out; }
function bed(w) { return w.index.pl.beds[0]; }
var A = step("a", 10), B = step("b", 20), C = step("c", 30), D = step("d", 40), E = step("e", 50, "closed");
// 1. cold: plant order, cells 0..n-1, the memo returned for the plan's bed, cellsChanged
var w0 = W.layout(doc([plan([C, A, D], [B, E])]), null, { now: 1 });
assert.deepStrictEqual(cellsOf(w0), { a: 0, b: 1, c: 2, d: 3, e: 4 }, "a cold layout plants by created, then id");
assert.deepStrictEqual(w0.cells, { pl: { ph: { a: 0, b: 1, c: 2, d: 3, e: 4 } } }, "the memo is {plan: {bed: {bead: cell}}}");
assert.strictEqual(w0.cellsChanged, true);
assert.deepStrictEqual([bed(w0).gone, bed(w0).stubble, bed(w0).stubbleAt, bed(w0).used], [[], 0, 5, { c0: 0, r0: 0, cols: 3, rows: 3 }]);
// 2. the same steps reordered in the document, states and titles changed: every cell stays, the memo is unchanged
var mem = JSON.parse(JSON.stringify(w0.cells));
var shuffled = plan([D, C, A], [E, B]); shuffled.phases[0].tasks[0].state = "in_progress"; shuffled.phases[0].tasks[1].title = "Renamed"; shuffled.phases[0].tasks[2].updated = 999;
var w1 = W.layout(doc([shuffled]), null, { now: 2, cells: mem });
assert.deepStrictEqual(cellsOf(w1), cellsOf(w0), "reordering the same steps leaves every cell where it was");
assert.strictEqual(w1.cellsChanged, false, "nothing to write when nothing moved");
assert.deepStrictEqual(bed(w1).cells.map(function (c) { return c.id; }), ["a", "b", "c", "d", "e"], "cells come in cell order");
// 3. a new step older than the planted ones: plant order alone would put it first; with the memo it takes the next free cell
var old = step("0", 5);
var w2 = W.layout(doc([plan([C, A, D, old], [B, E])]), null, { now: 3, cells: w1.cells });
assert.deepStrictEqual(cellsOf(w2), { a: 0, b: 1, c: 2, d: 3, e: 4, "0": 5 }, "no planted bead moved; the new one took the next free cell");
assert.strictEqual(w2.cellsChanged, true);
assert.deepStrictEqual(cellsOf(W.layout(doc([plan([C, A, D, old], [B, E])]), null, { now: 3 })), { "0": 0, a: 1, b: 2, c: 3, d: 4, e: 5 }, "without the memo the older step moves everything along: the memo is what holds them");
// 4. a step closes and keeps its cell; a closed step past the listed ones leaves a dim marker, not a gap; the next new step
//    takes the cell after the marker; the marker counts against the stubble (it is one of the closed count)
var closedC = step("c", 30, "closed"), p3 = plan([A, D, old], [B, E, closedC]); p3.phases[0].closed = [B, E]; p3.phases[0].counts.closed = 3; p3.counts.closed = 3;   // c closed and not listed
var w3 = W.layout(doc([p3]), null, { now: 4, cells: w2.cells });
assert.deepStrictEqual(cellsOf(w3), { a: 0, b: 1, d: 3, e: 4, "0": 5 }, "the gap at cell 2 is not closed");
assert.deepStrictEqual([bed(w3).gone, bed(w3).stubble, bed(w3).stubbleAt], [[2], 0, 6], "the gone bead is a dim marker at its cell; the closed count past the listed steps is the marker");
assert.deepStrictEqual(w3.cells.pl.ph, { a: 0, b: 1, c: 2, d: 3, e: 4, "0": 5 }, "the memo keeps the gone bead's cell");
var F = step("f", 60), p4 = JSON.parse(JSON.stringify(p3)); p4.phases[0].tasks.push(F);
var w4 = W.layout(doc([p4]), null, { now: 5, cells: w3.cells });
assert.deepStrictEqual(cellsOf(w4), { a: 0, b: 1, d: 3, e: 4, "0": 5, f: 6 }, "a new step takes the next free cell past the marker, not the gap");
var p5 = JSON.parse(JSON.stringify(p4)); p5.phases[0].closed.push(closedC);
assert.deepStrictEqual(cellsOf(W.layout(doc([p5]), null, { now: 6, cells: w4.cells })), { a: 0, b: 1, c: 2, d: 3, e: 4, "0": 5, f: 6 }, "listed again, the bead is back in its own cell");
// 5. the crate follows the plants into the lowest free cell, after the markers, and the used blocks cover everything placed
var p6 = JSON.parse(JSON.stringify(p4)); p6.phases[0].more = 2; p6.phases[0].counts.open += 2;
var w6 = W.layout(doc([p6]), null, { now: 7, cells: w4.cells });
assert.deepStrictEqual([bed(w6).crate.i, bed(w6).crate.n, bed(w6).used, bed(w6).stubbleAt], [7, 2, { c0: 0, r0: 0, cols: 3, rows: 3 }, 8]);
// 6. a full bed: 81 cells held by 60 tasks, a crate and 20 closed, then the 20 listed closed steps are replaced by 20 others:
//    the old ones become markers and must give their cells up to the newcomers, lowest first; nothing listed moves
var ts = [], cl = [], cl2 = [], i;
for (i = 0; i < 60; i++) ts.push(step("t" + i, 100 + i));
for (i = 0; i < 20; i++) { cl.push(step("c" + i, 200 + i, "closed")); cl2.push(step("n" + i, 300 + i, "closed")); }
var wf = W.layout(doc([plan(ts, cl, 7)]), null, { now: 8 });
assert.strictEqual(bed(wf).cells.length, 80); assert.strictEqual(bed(wf).crate.i, 80);
var wf2 = W.layout(doc([plan(ts, cl2, 7)]), null, { now: 9, cells: wf.cells });
var cf = cellsOf(wf), cf2 = cellsOf(wf2);
ts.forEach(function (tk) { assert.strictEqual(cf2[tk.id], cf[tk.id], "a task keeps its cell in a full bed"); });
assert.deepStrictEqual(cl2.map(function (c) { return cf2[c.id]; }), [80].concat(cl.slice(0, 19).map(function (c) { return cf[c.id]; })), "the first newcomer took the crate's free cell, the rest the reclaimed markers, lowest first");
assert.deepStrictEqual([bed(wf2).gone, bed(wf2).crate.i, bed(wf2).cells.length], [[], 79, 80], "the crate took the last marker; none is left");
// 7. the memo round-trips through JSON (sessionStorage), ignores garbage, is pruned to the document's plans, and is read but
//    not written on a readonly layout
var json = JSON.stringify(w4.cells);
assert.deepStrictEqual(cellsOf(W.layout(doc([p4]), null, { now: 10, cells: JSON.parse(json) })), cellsOf(w4), "a refresh keeps the cells");
var junk = { pl: { ph: { a: 3, b: 3, c: "x", d: 99, e: -1, f: 1.5 }, zz: "bad" }, other: 7 };
assert.deepStrictEqual(cellsOf(W.layout(doc([p4]), null, { now: 11, cells: junk })), { "0": 0, a: 3, b: 1, d: 2, e: 4, f: 5 }, "a duplicate, a non-integer and an out-of-range cell are dropped; the rest hold and the others plant around them");
assert.deepStrictEqual(Object.keys(W.layout(doc([p4]), null, { now: 11, cells: junk }).cells), ["pl"], "plans gone from the document leave the memo");
var ro = W.layout(doc([plan([C, A, D, old], [B, E])]), null, { now: 12, cells: mem, readonly: true });
assert.deepStrictEqual(cellsOf(ro), { a: 0, b: 1, c: 2, d: 3, e: 4, "0": 5 }, "readonly still places by the memo");
assert.deepStrictEqual([ro.cellsChanged, ro.cells], [false, mem], "but writes nothing back");
assert.strictEqual(W.layout(doc([p4]), null, { now: 13, cells: "garbage" }).cellsChanged, true, "a non-object memo is an empty one");
// 8. a phase whose steps are all closed but whose status is still open is closed and does not take current; the next one does
var pd = plan([], [B, E]); pd.phases[0].status = "open"; pd.phases[0].counts = { closed: 2 };
pd.phases.push({ id: "ph2", title: "Next", status: "open", counts: { open: 1 }, more: 0, tasks: [A], closed: [] });
var wd = W.layout(doc([pd]), null, { now: 14 }), bd = wd.index.pl.beds;
assert.deepStrictEqual(bd.map(function (b) { return [b.closed, b.current]; }), [[true, false], [false, true]], "a done-but-open phase stays closed and the next phase is current");
`
	world, err := filepath.Abs(filepath.Join("web", "world.js"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "-e", script, world).CombinedOutput()
	if err != nil {
		t.Fatalf("node bed cells test failed: %v\n%s", err, out)
	}
}
