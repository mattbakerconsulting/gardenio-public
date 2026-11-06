/* The escalation sprites (web/art.js, plan gd-PJB3D7glwF step 3 gd-UDx80F47JN), run by TestArtUnderNode:
     node art_test.js <web dir>
   One 16x16 map per family, keyed by ESCKIND.FAMILIES, each distinct; an unknown family drawn as other's chest; the state drawn
   on top as a shape, never colour alone: pending a raised flag, approved a gear whose two frames differ (frame 0 is the still
   one), rejected and failed a crack through the body; a farm task with family and wait (FarmTask) drawn as that sprite on its
   mound, a closed one as the pile, the bitmap key naming family and state; drawEscalation at a whole number of px per sprite
   px. Prints a JSON report whose families the Go test compares with the Go family table. */
"use strict";
var path = require("path"), assert = require("assert");
var ART = require(path.join(process.argv[2], "art.js")), ESCKIND = require(path.join(process.argv[2], "esckind.js"));
var FAMS = ESCKIND.FAMILIES, STATES = ART.ESC_STATES;

function count(rs, test) { var n = 0; rs.forEach(function (r) { for (var i = 0; i < r.length; i++) if (test(r.charAt(i), i)) n++; }); return n; }
function corner(rs) { return rs.slice(0, 5).map(function (r) { return r.slice(9); }).join("|"); }   // the dressing's corner
function sq(rs) { return rs.length === 16 && rs.every(function (r) { return r.length === 16; }); }

/* ---------- 1. a sprite per family, each its own ---------- */
assert.deepStrictEqual(STATES, ["pending", "approved", "rejected", "failed"]);
var bare = {};
FAMS.forEach(function (f) {
  var m = ART.escRows(f, "", 0);
  assert.ok(sq(m), f + ": 16x16");
  assert.ok(count(m, function (c) { return c === "k"; }) > 0 && count(m, function (c) { return c !== "." && c !== "k"; }) >= 8, f + ": outlined, a body");
  assert.strictEqual(corner(m).replace(/[.|]/g, "").replace(/k/g, "").length, 0, f + ": keeps the dressing's corner (rows 0-4) clear");
  bare[f] = m.join("\n");
});
FAMS.forEach(function (a, i) { FAMS.slice(i + 1).forEach(function (b) { assert.notStrictEqual(bare[a], bare[b], a + " and " + b + " look the same"); }); });
assert.strictEqual(ART.escRows("teleport", "", 0).join("\n"), bare.other, "an unknown family is other's chest");
assert.strictEqual(ART.escRows(undefined, "pending", 0).join("\n"), ART.escRows("other", "pending", 0).join("\n"));

/* ---------- 2. the state, on top, as a shape ---------- */
var seen = {};
FAMS.forEach(function (f) {
  var b = bare[f].split("\n");
  STATES.forEach(function (st) {
    [0, 1].forEach(function (fr) {
      var m = ART.escRows(f, st, fr), key = f + "|" + st + "|" + fr;
      assert.ok(sq(m), key + ": 16x16");
      assert.notStrictEqual(m.join("\n"), bare[f], key + ": dressed");
      seen[key] = m.join("\n");
    });
    var m0 = ART.escRows(f, st, 0), m1 = ART.escRows(f, st, 1);
    if (st === "pending") {
      assert.ok(count(m0, function (c, i) { return (c === "e" || c === "E") && i >= 11; }) >= 6, f + ": a red flag raised at the top right");
      assert.ok(m0.slice(0, 7).every(function (r) { return r.charAt(11) === "k"; }), f + ": on its pole");
    }
    if (st === "approved") {
      assert.notStrictEqual(m0.join("\n"), m1.join("\n"), f + ": the gear turns by frame");
      assert.ok(count(m0, function (c, i) { return c === "R" && i >= 9; }) >= 4 && m0[3].charAt(12) === "k", f + ": a gear with its hub");
    } else assert.strictEqual(m0.join("\n"), m1.join("\n"), f + " " + st + ": still");
    if (st === "rejected" || st === "failed") {
      assert.strictEqual(corner(m0), corner(b), f + " " + st + ": no flag, no gear");
      var split = 0; m0.forEach(function (r, j) { for (var i = 0; i < 16; i++) if (r.charAt(i) !== b[j].charAt(i)) split++; });
      assert.ok(split >= 2, f + " " + st + ": the crack splits the body (" + split + " px)");
      assert.strictEqual(count(m0, function (c) { return c !== "." && c !== "k"; }) < count(b, function (c) { return c !== "." && c !== "k"; }), true, f + ": the crack takes body pixels");
    }
  });
  assert.strictEqual(seen[f + "|rejected|0"], seen[f + "|failed|0"], f + ": rejected and failed are the one crack (the words tell them apart)");
  assert.notStrictEqual(seen[f + "|pending|0"], seen[f + "|approved|0"]);
});

/* ---------- 3. the plot: a FarmTask with its family and wait ---------- */
var task = { id: "gd-E1", type: "escalation", state: "open", priority: 2, family: "merge", op: "github_pr_merge", wait: "pending" };
var b = ART.taskBead(task, 0);
assert.deepStrictEqual([b.t, b.s, b.fam, b.w], ["escalation", "open", "merge", "pending"]);
var plot = ART.beadRows(b, 0);
assert.ok(sq(plot));
assert.strictEqual(plot[15], ART.beadRows({ t: "task", s: "open" }, 0)[15], "on the mound like any plot cell");
assert.ok(count(plot, function (c, i) { return (c === "e" || c === "E") && i >= 11; }) >= 6, "the flagged crate");
var crate = ART.escRows("merge", "pending", 0);
assert.ok(crate.every(function (r, j) { for (var i = 0; i < 16; i++) { var c = r.charAt(i); if (c !== "." && c !== "k" && plot[j].charAt(i) !== c) return false; } return true; }), "the merge sprite over the mound");
assert.notStrictEqual(ART.beadKey(b, 0), ART.beadKey(ART.taskBead({ type: "escalation", state: "open", family: "deploy", wait: "pending" }, 0), 0), "the key names the family");
assert.notStrictEqual(ART.beadKey(b, 0), ART.beadKey(ART.taskBead({ type: "escalation", state: "open", family: "merge", wait: "failed" }, 0), 0), "and the state");
var legacy = ART.taskBead({ type: "escalation", state: "open" }, 0);
assert.deepStrictEqual([legacy.fam, legacy.w], ["other", ""], "a document without family is other's chest, undressed");
var closed = ART.beadRows(ART.taskBead({ type: "escalation", state: "closed", family: "merge" }, 0), 0);
assert.strictEqual(closed.join("\n"), ART.beadRows({ t: "decision", s: "closed" }, 0).join("\n"), "closed is the pile");
assert.ok(ART.beadRows({ t: "decision", s: "open" }, 0).join("").indexOf("j") >= 0, "the decision's post unchanged");

/* ---------- 4. placed alone: whole device px per sprite px ---------- */
function rec() { var r = { fills: [] }; r.fillRect = function (x, y, w, h) { r.fills.push([x, y, w, h]); }; return r; }
var pal = ART.palette(false), npal = ART.palette(true);
[1, 2.4, 3, 0.6].forEach(function (n) {
  var c = rec(), u = Math.max(1, Math.round(n));
  ART.drawEscalation(c, pal, "access", "approved", 10, 20, n, 1);
  assert.ok(c.fills.length > 0 && c.fills.every(function (q) { return (q[0] - 10) % u === 0 && (q[1] - 20) % u === 0 && q[2] % u === 0 && q[3] === u; }), n + ": " + u + " px a sprite px");
});
var nc = rec(), seenCols = {}; nc.fillRect = function () { seenCols[this.fillStyle] = 1; };
ART.drawEscalation(nc, npal, "merge", "pending", 0, 0, 1, 0);
assert.ok(seenCols[npal.M] && !seenCols[pal.b], "at night: the night palette, the rim's M");

console.log(JSON.stringify({ families: FAMS, sprites: Object.keys(seen).length }));
