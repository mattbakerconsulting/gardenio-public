package gardenhub

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Tool calls as birds (gd-pLFocqmYyf, step 4b of plan gd-gAKb8Bf0Nm). web/birds.js is pure under node: the species by doing
// class (none for a thought or a wait); 10 x 6 and 14 x 8 sprites of four colours (gd-cFZPuAjqJu: body, wing, belly or beak,
// eye) and three frames, a flap up, level, down, level, and the hawk a fourth, its glide, which it soars in between flaps and
// lands in (gd-BuWCnuLYQv), drawn in device px at a whole number of them per sprite px, 1.5 CSS px at Plan rounded (gd-7dRp5a6r15)
// and 1.25x the zoom but never under 2 at Bead (gd-Wucpbrm3Z6), ringed so it reads on grass and dirt by day and on dark ground
// at night; a bird floats up off its gardener's head, straight up and then on up and away, left and right by turns, each gone in
// 1.5-2 s, a flock's later birds a body behind (gd-CykqEHeEpX; before it, it flew the shortest way off the beds). On the page's
// side, fed events and drawn: nothing for 2 s after the stream opens; a fold of five flies as a flock of three; at most 24 live;
// nothing below the Plan level, off the view or for a gardener still on its way; reduced motion is one still bird for 1 s; a
// subagent's hawk circles over the head until it stops and then lands; a project's sign says "busy" at World and Project for 30 s.
// The caption stays on the gardener's head (gd-CykqEHeEpX; captions spec 9.1 and 9.2): the wire's cap else the tool word
// (gd-VLVayQzQIB, 4k: the card's word map, short: "go test", "git push", "shell", "subagent"), one plate per gardener, the newest
// call's, perched over the head from the call for LINGER ms, bird or no bird, solid at once, fading out over 300 ms, one drawImage each,
// kept 8 px inside the screen; its ink and halo read on every ground by day and night. The words go on top (gd-VHkZ3pCEqK, 4n):
// draw() paints the birds under the figures and places the plates, words() draws them after the figures, once a draw. A plate
// keeps off every face, "?" bubble, the sign's tip, the cursor's ring, a still bird and a hawk, taking over the head, else its
// right, else its left, or dropped; beside a still bird (reduced motion) its right, left, then over it. A gardener at home flies
// its birds from its figure in its home strip's barn (WORLD.homeFill), else its Yard slot while the Yard still draws.
// Skipped when node is not installed.
func TestBirdsUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `
var on = {}; global.document = { addEventListener: function (k, f) { (on[k] = on[k] || []).push(f); } };
function fire(k, d) { (on[k] || []).forEach(function (f) { f({ detail: d }); }); }
var W = require(process.argv[1]), B = require(process.argv[2]), A = require(process.argv[3]), AC = require(process.argv[4]), assert = require("assert"), T = W.TILE;
// species by class
assert.deepStrictEqual(["read", "edit", "test", "build", "shell", "git", "beads", "deploy", "web", "delegate", "say", "think", "waiting"].map(B.speciesOf),
  ["sparrow", "finch", "crow", "crow", "crow", "twig", "pigeon", "gull", "swift", "hawk", "robin", "", ""]);
// sprites: 10 x 6 (the sparrow, the finch, the pigeon) or 14 x 8, three frames (the hawk four), rows of the letters a to f; each frame has its
// body and an eye and is the same both ways (a bird flies every way unmirrored); by day and by night a colour per letter, each
// run painted the colour at its letter's place
assert.deepStrictEqual(Object.keys(B.SP).map(function (k) { return k + " " + B.SP[k].w + "x" + B.SP[k].h; }),
  ["sparrow 10x6", "finch 10x6", "pigeon 10x6", "robin 10x6", "crow 14x8", "twig 14x8", "gull 14x8", "swift 14x8", "hawk 14x8"]);
Object.keys(B.SP).forEach(function (k) {
  var s = B.SP[k];
  assert.strictEqual(s.f.length, k === "hawk" ? 4 : 3, k);
  s.f.forEach(function (fr, f) {
    assert.strictEqual(fr.length, s.h, k);
    fr.forEach(function (r) { assert.ok(r.length === s.w && /^[a-f.]+$/.test(r) && r === r.split("").reverse().join(""), k + ": " + r); });
    assert.ok(/a/.test(fr.join("")) && /d/.test(fr.join("")), k + " frame " + f + ": a body and an eye");
  });
  assert.ok(s.day.length === s.night.length, k);
  s.runs.forEach(function (rs) { rs.forEach(function (r) { assert.ok(r.i === B.INKS.indexOf(r.c) && r.i < s.day.length, k + ": " + r.c); }); });
  // up, level, down: the wing tips at the top, at the shoulder, at the bottom, so every frame is its own
  var tips = s.f.slice(0, 3).map(function (fr) { for (var y = 0; y < fr.length; y++) if (fr[y][0] !== ".") return y; return -1; });
  assert.ok(tips[0] < tips[1] && tips[1] < tips[2], k + ": wing tips " + tips);
  s.f.forEach(function (fr, f) { s.f.forEach(function (gr, g) { assert.ok(f === g || fr.join() !== gr.join(), k + " frames " + f + " and " + g); }); });
});
// the hawk's glide: wings flat as at level, its tail spread wider than any flap's
var tail = function (fr) { return Math.max.apply(null, fr[fr.length - 1].split(".").map(function (r) { return r.length; })); };
assert.deepStrictEqual(B.SP.hawk.f[3].slice(3, 5), B.SP.hawk.f[1].slice(3, 5), "the glide's wings as the level's");
assert.ok(B.SP.hawk.f.slice(0, 3).every(function (fr) { return tail(B.SP.hawk.f[3]) > tail(fr); }), "its tail spread");
// at night each shows on the grass: a ringed bird's first colour 3:1 against the rim and the rim against the grass, an unringed
// one's 3:1 against the grass itself
function lum(h) { var c = [1, 3, 5].map(function (i) { var v = parseInt(h.substr(i, 2), 16) / 255; return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4); }); return 0.2126 * c[0] + 0.7152 * c[1] + 0.0722 * c[2]; }
function ratio(x, y) { var a = lum(x), b = lum(y); return (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05); }
var grass = A.palette(true).g;
assert.ok(ratio(B.RIM, grass) >= 3, "the rim on the night grass");
Object.keys(B.SP).forEach(function (k) { var s = B.SP[k], r = ratio(s.night[0], s.rim ? B.RIM : grass); assert.ok(r >= 3, k + " at night: " + r.toFixed(2) + ":1"); });
assert.deepStrictEqual(Object.keys(B.SP).filter(function (k) { return !B.SP[k].rim; }), ["finch", "gull"]);
// by day every bird is ringed (a dark one in a pale ring, a bright one in a dark ring), its first colour 3:1 off the ring, and the
// bird or its ring 3:1 off every grass, soil and sand tone it flies over (gd-Wucpbrm3Z6: a dark "v" was lost on grass and dirt)
var day = A.palette(false);
Object.keys(B.SP).forEach(function (k) {
  var s = B.SP[k], rim = B.rimOf(s, false);
  assert.strictEqual(rim, s.rim ? B.DAY_RIM : B.INK, k);
  assert.ok(ratio(s.day[0], rim) >= 3, k + " off its day ring: " + ratio(s.day[0], rim).toFixed(2) + ":1");
  "ghdstuPN".split("").forEach(function (g) { var r = Math.max(ratio(s.day[0], day[g]), ratio(rim, day[g])); assert.ok(r >= 3, k + " by day on " + g + ": " + r.toFixed(2) + ":1"); });
});
assert.deepStrictEqual([B.rimOf(B.SP.crow, true), B.rimOf(B.SP.finch, true)], [B.RIM, ""], "at night only the dark birds ringed");
// device px per sprite px, whole: 1.5 CSS px at the Plan level, rounded, so 3 at DPR 2 (a crow 21 CSS px wide) and 2 at DPR 1
// (a crow 28 px wide); at the Bead level 1.25x the zoom, never under 2, so a Bead bird is never smaller than a Plan one and
// grows as the camera comes in: bigger at every zoom from DPR 2, and at DPR 1 from 2x (below it both are 2, the Plan bird's
// 1.5 rounded up to the Bead floor)
assert.deepStrictEqual([B.pxOf(0.49, 1), B.pxOf(0.49, 2), B.pxOf(0.49, 3), B.pxOf(0.91, 1)], [2, 3, 5, 2], "Plan");
assert.deepStrictEqual([1, 1.5, 2, 3, 4].map(function (z) { return B.pxOf(z, 1, 3); }), [2, 2, 3, 4, 5], "Bead at DPR 1");
assert.deepStrictEqual([B.pxOf(1, 2, 3), B.pxOf(2, 2, 3), B.pxOf(3, 2, 3)], [4, 5, 8], "Bead at DPR 2");
[1, 2, 3].forEach(function (d) {
  Object.keys(B.SP).forEach(function (k) { assert.ok(B.SP[k].w * B.pxOf(0.49, d, 2) / d >= 15, k + " at Plan, DPR " + d); });
  [1, 1.5, 2, 3, 4].forEach(function (z) {
    var bead = B.pxOf(z, d, 3), plan = B.pxOf(0.91, d, 2);
    assert.ok(d > 1 || z >= 2 ? bead > plan : bead === plan, "Bead " + z + "x over Plan at DPR " + d + ": " + bead + " to " + plan);
  });
});
// the tool word a bird carries (4k): the card's own map, short; its class when the wire has none
assert.deepStrictEqual([["read", "Read"], ["edit", "Edit"], ["test", "go_test"], ["build", "cargo_build"], ["git", "git_push"], ["git", "git_other"], ["beads", "kd_show"],
  ["beads", "gb_other"], ["deploy", "helm_upgrade"], ["shell", "other"], ["web", "WebFetch"], ["read", "mcp__slack__read"], ["shell", ""], ["delegate", "subagent"], ["delegate", "Agent"]].map(function (c) { return B.tagOf(c[0], c[1]); }),
  ["Read", "Edit", "go test", "cargo build", "git push", "git", "kd show", "gb", "helm upgrade", "shell", "WebFetch", "mcp slack read", "shell", "subagent", "subagent"]);
assert.deepStrictEqual([AC.toolWords({ class: "shell", tool: "other" }, 0), AC.toolWords({ class: "test", tool: "go_test" }, 0), AC.toolWords({ class: "delegate", tool: "subagent" }, 0)],
  ["a shell command", "go test", "a subagent"], "the card keeps its long words from the same map");
// one label per gardener, the newest call's, newest first
assert.deepStrictEqual(B.pick([{ id: "a", t0: 1 }, { id: "b", t0: 5 }, { id: "a", t0: 3 }, { id: "b", t0: 2 }, { id: "c", t0: 4 }]).map(function (l) { return l.id + l.t0; }), ["b5", "c4", "a3"]);
// placed in order: one over a label placed before it, or over the sign's tip, is dropped, not stacked; one is kept 8 px inside the
// screen, and dropped once its bird is off it
function lab(id, x, y) { return { id: id, x: x, y: y, w: 40, h: 14, cx: x + 20, cy: y - 8 }; }
var scr = { w: 390, h: 844, e: 8 };
assert.deepStrictEqual(B.place([lab("a", 100, 100), lab("b", 120, 106), lab("c", 141, 100), lab("d", 300, 300)], scr, [{ x: 290, y: 290, w: 20, h: 20 }]).map(function (l) { return l.id; }), ["a", "c"]);
assert.deepStrictEqual(B.place([lab("a", -15, 400), lab("b", 360, 838), lab("c", 400, 400)], scr).map(function (l) { return [l.id, l.x, l.y]; }), [["a", 8, 400], ["b", 342, 822]]);
assert.deepStrictEqual(B.place([lab("a", -15, 400), lab("c", 1000, -900)], null).map(function (l) { return [l.id, l.x, l.y]; }), [["a", -15, 400], ["c", 1000, -900]], "no screen: anywhere");
// a label whose spot is taken tries its next one (4n): the first clear of what was placed and of keep, else dropped
function alt(l, spots) { l.alt = spots; return l; }
assert.deepStrictEqual(B.place([lab("a", 100, 100), alt(lab("b", 120, 106), [{ x: 120, y: 130 }]), alt(lab("c", 300, 300), [{ x: 250, y: 300 }, { x: 300, y: 330 }]), alt(lab("d", 100, 120), [{ x: 100, y: 90 }])], scr, [{ x: 290, y: 290, w: 20, h: 20 }, { x: 240, y: 290, w: 20, h: 20 }])
  .map(function (l) { return [l.id, l.x, l.y, "alt" in l]; }), [["a", 100, 100, false], ["b", 120, 130, false], ["c", 300, 330, false]], "b and c on their next clear spot, d nowhere clear and dropped");
// the sides a plate tries: over the head, else over it off to the right or the left, else its right, else its left; beside a
// still bird its right, left, then over it
assert.deepStrictEqual([B.ON_HEAD, B.BY_BIRD], [["o", "or", "ol", "r", "l"], ["r", "l", "o"]]);
// its ink 4.5:1 off its halo, and one of them 3:1 off every ground, by day and at night; on the plate (the halo at 92% over
// the ground, gd-TmYAAOBQEH) the ink is still 4.5:1 off it on every ground, so a caption reads over a crowded plot
function mix(f, b, a) { return "#" + [1, 3, 5].map(function (i) { var v = Math.round(a * parseInt(f.substr(i, 2), 16) + (1 - a) * parseInt(b.substr(i, 2), 16)); return ("0" + v.toString(16)).slice(-2); }).join(""); }
[false, true].forEach(function (night) {
  var c = night ? B.TAG.night : B.TAG.day, pal = A.palette(night);
  assert.ok(ratio(c[0], c[1]) >= 4.5, "label ink on its halo, night " + night + ": " + ratio(c[0], c[1]).toFixed(2) + ":1");
  "ghdstuPN".split("").forEach(function (g) { var r = Math.max(ratio(c[0], pal[g]), ratio(c[1], pal[g])); assert.ok(r >= 3, "label on " + g + ", night " + night + ": " + r.toFixed(2) + ":1"); });
  "ghdstuPN".split("").forEach(function (g) { var r = ratio(c[0], mix(c[1], pal[g], B.PLATE.fill)); assert.ok(r >= 4.5, "plate ink on " + g + ", night " + night + ": " + r.toFixed(2) + ":1"); });
});
// a plate's text (captions spec 9.1): the wire's cap, its whitespace collapsed, else the tool word; wrapped at words to two
// lines, the second ending in "…" when it still overflows, a word wider than the plate broken inside it, never inside a code point
assert.deepStrictEqual([B.plateOf({ doing: "shell", tool: "other", cap: "  Show\n the  plan " }), B.plateOf({ doing: "shell", tool: "other" }), B.plateOf({ doing: "shell", tool: "other", cap: " " }),
  B.plateOf({ doing: "delegate", tool: "Agent", cap: "Explore gardenhub transcript endpoints" }), B.plateOf({ doing: "delegate", tool: "Agent" })],
  ["Show the plan", "shell", "shell", "Explore gardenhub transcript endpoints", "subagent"]);
var mono = function (s) { return Array.from(s).length * 6; };   // 31 characters in 190 px
assert.deepStrictEqual(B.wrap("Show plan ready frontier", 190, mono), ["Show plan ready frontier"]);
assert.deepStrictEqual(B.wrap("Trial-merge main into #1282 in a detached worktree", 190, mono), ["Trial-merge main into #1282 in", "a detached worktree"]);
assert.deepStrictEqual(B.wrap("Rebase the caption plate branch onto main and rerun every bird golden under node", 190, mono), ["Rebase the caption plate branch", "onto main and rerun every bird…"]);
assert.deepStrictEqual(B.wrap("Rebase the caption plate branch onto main and rerun everywhere-at-once", 190, mono), ["Rebase the caption plate branch", "onto main and rerun…"], "cut back to a word");
assert.deepStrictEqual(B.wrap("abcdefghijabcdefghijabcdefghijabcdefghij", 60, mono), ["abcdefghij", "abcdefghi…"], "a long word broken");
assert.deepStrictEqual(B.wrap("🐦🐦🐦🐦", 18, mono), ["🐦🐦🐦", "🐦"], "whole code points");
assert.strictEqual(B.TAG_CAP, 256, "the bake cache holds 256 plates: captions vary far more than tool words");
// a bird floats up off its gardener's head (gd-CykqEHeEpX): straight up RUN, then on up and away, left or right by turns; never
// below the head once up; gone in 1.5-2 s; from a field spot the same way; a swift leaves the screen, a longer run in the same time
var p = { x: 100, y: 40, w: 32, h: 32 };
function spot(col, row, cell) { return { plot: "P", col: col, row: row, x: col * T, y: T * row - 8, shown: true, cell: cell || "", fence: !cell }; }
function samples(r) { var out = []; for (var i = 0; i <= 60; i++) out.push(B.at({ pts: r.pts, t0: 0, dur: r.dur }, i / 60 * r.dur * 0.999)); return out; }
[p.x + 1, p.x + 5, p.x + 16, p.x + 30].forEach(function (col, i) {
  var s = spot(col, p.y + 31), r = B.route(s, "sparrow", i), h = { x: s.x + 8, y: s.y + 4 };
  assert.deepStrictEqual(r.pts[0], h, "off its head");
  assert.deepStrictEqual(r.pts[1], { x: h.x, y: h.y - B.RUN }, "straight up");
  assert.ok(r.pts[2].y < r.pts[1].y && Math.abs(r.pts[2].x - h.x) === B.RUN / 2, "on up and away: " + JSON.stringify(r.pts));
  assert.ok(r.dur >= 1500 && r.dur <= 2000, "gone in 1.5-2 s: " + r.dur);
  samples(r).forEach(function (pt) { if (pt && !pt.done) assert.ok(pt.y <= h.y, "below the head at " + JSON.stringify(pt)); });
  assert.notStrictEqual(r.pts[2].x > h.x, B.route(s, "sparrow", i + 1).pts[2].x > h.x, "left and right by turns");
});
var cs = spot(p.x + 9, p.y + 4, "gd-cell"), rc = B.route(cs, "finch", 0);
assert.deepStrictEqual(rc.pts[1], { x: cs.x + 8, y: cs.y + 4 - B.RUN }, "from a field spot the same way up");
var sw = B.route(spot(p.x + 5, p.y + 31), "swift", 0);
assert.deepStrictEqual([sw.pts[1].y, sw.dur], [sw.pts[0].y - B.SWIFT_RUN, 2000], "a swift: a longer run in the same time");
// at: not off yet, flapping, gone
var b = { pts: [{ x: 0, y: 0 }, { x: 0, y: 48 }], t0: 100, dur: 1500 };
assert.strictEqual(B.at(b, 50), null);
assert.deepStrictEqual([0, 1, 2, 3, 4, 5, 8].map(function (k) { return B.at(b, 100 + k * B.FLAP + 10).f; }), [0, 1, 2, 1, 0, 1, 0], "a flap: up, level, down, level");
assert.ok(B.at(b, 1600).done);
// admit: three in flight per gardener, 24 in all
var live = [{ id: "a" }, { id: "a" }, { id: "b" }];
assert.deepStrictEqual([B.admit(live, "a", 5), B.admit(live, "b", 5), B.admit(live, "c", 1), B.admit(live, "c", 0)], [1, 2, 1, 0]);
var full = []; for (var i = 0; i < 24; i++) full.push({ id: "x" + i });
assert.strictEqual(B.admit(full, "c", 1), 0);
var barn = []; for (i = 0; i < 8; i++) barn.push({ id: "b" + i, barn: "project:" });
assert.deepStrictEqual([B.admit(barn, "c", 3, "project:"), B.admit(barn, "c", 3, "project:x"), B.admit(barn, "c", 3, "")], [0, 3, 3], "a barn's cap of 8 is its own");

/* ---------- the page's side, fed events and drawn ---------- */
var ctx = { fills: 0, fillRect: function () { this.fills++; }, drawImage: function () {} };
var plots = { P: p }, spots = {}, ids = [];
for (i = 0; i < 30; i++) { ids.push("g" + i); spots["g" + i] = spot(p.x + 1 + (i % 28), p.y + 31); }
spots.off = spot(p.x + 1, p.y + 31); spots.off.shown = false;
var gs = { 2: { spots: spots }, 3: { spots: spots } }, view = { x: p.x * T - 64, y: p.y * T - 64, w: 40 * T, h: 40 * T };
function o(x) { var r = { level: 2, night: false, view: view, gs: gs, world: { index: plots }, reduce: false, moving: function () { return false; } }; for (var k in x) r[k] = x[k]; return r; }
function call(id, doing, n, tool, cap) { var e = { id: id, doing: doing, tool: tool || "Read", at: 1, dur: 0 }; if (n != null) e.n = n; if (cap) e.cap = cap; B.hear(e); }
var now = 1792000000000; B.pin(now);
fire("farmdoc", { gardeners: [{ id: "g0", project: "Gardenio" }, { id: "g1", project: "music" }] });
fire("farmlive", { kind: "open" });
B.pin(now += 1000); call("g0", "read"); B.draw(ctx, o());
assert.strictEqual(B.list().length, 0, "nothing for 2 s after the stream opens");
assert.strictEqual(B.busySig(), "gardenio", "but the project is busy");
B.pin(now += 1500); call("g0", "read"); assert.ok(B.draw(ctx, o()), "a bird flies: ask for the next frame");
assert.deepStrictEqual(B.list().map(function (x) { return x.sp; }), ["sparrow"]);
assert.ok(ctx.fills > 0, "drawn");
// on the page: in device px, three per sprite px at 0.49x and DPR 2, over a pale rim one CSS px wide at night, the world transform
// put back after
var m = { a: 0.98, b: 0, c: 0, d: 0.98, e: 40, f: 20 }, rects = [], sets = [], imgs = [], ops = [];
var sctx = { fillStyle: "", globalAlpha: 1, getTransform: function () { return m; }, setTransform: function () { sets.push([].slice.call(arguments)); },
  fillRect: function (x, y, w, h) { rects.push({ x: x, y: y, w: w, h: h, c: this.fillStyle }); ops.push("bird"); }, drawImage: function (im, x, y) { imgs.push({ x: x, y: y, a: this.globalAlpha }); ops.push("word"); } };
function span(rs, k, d) { return Math.max.apply(null, rs.map(function (r) { return r[k] + r[d]; })) - Math.min.apply(null, rs.map(function (r) { return r[k]; })); }
B.draw(sctx, o({ z: 0.49, night: true }));
var body = rects.filter(function (r) { return r.c !== B.RIM; }), rim = rects.filter(function (r) { return r.c === B.RIM; });
assert.deepStrictEqual([span(body, "x", "w"), span(body, "y", "h")], [30, 18], "a sparrow 30 x 18 device px");
assert.deepStrictEqual([span(rim, "x", "w"), span(rim, "y", "h")], [34, 22], "ringed two device px round");
assert.ok(rects.indexOf(rim[0]) < rects.indexOf(body[0]), "the rim under the bird");
assert.ok(body.every(function (r) { return r.w % 3 === 0 && r.h === 3; }), "whole sprite px");
assert.deepStrictEqual(sets, [[1, 0, 0, 1, 0, 0], [0.98, 0, 0, 0.98, 40, 20]], "drawn in device px, the world transform put back");
rects = []; B.draw(sctx, o({ z: 0.49 }));
assert.ok(rects.length > 0 && rects.every(function (r) { return r.c !== B.RIM; }) && rects[0].c === B.DAY_RIM, "by day the pale day rim");
// at the Bead level, zoomed in to 2x at DPR 2: five device px per sprite px, a sparrow 50 x 30 in a two-px rim
m = { a: 4, b: 0, c: 0, d: 4, e: 40, f: 20 }; rects = [];
B.draw(sctx, o({ level: 3, z: 2, night: true }));
body = rects.filter(function (r) { return r.c !== B.RIM; }); rim = rects.filter(function (r) { return r.c === B.RIM; });
assert.deepStrictEqual([span(body, "x", "w"), span(body, "y", "h"), span(rim, "x", "w")], [50, 30, 54], "a Bead sparrow 50 x 30 device px");
m = { a: 0.98, b: 0, c: 0, d: 0.98, e: 40, f: 20 };
call("g0", "edit", 5); B.draw(ctx, o());
assert.deepStrictEqual(B.list().map(function (x) { return x.sp; }), ["sparrow", "finch", "finch"], "a fold of five: the flock tops up to three");
assert.ok(B.list()[2].t0 > B.list()[1].t0, "the flock's later bird a beat behind");
call("g0", "think"); call("g0", "read", 0); B.draw(ctx, o());
assert.strictEqual(B.list().length, 3, "a thought and a finish lift nothing");
B.pin(now += 2200); B.draw(ctx, o());
assert.strictEqual(B.list().length, 0, "gone in 2 s");
B.pin(now += B.LINGER); B.draw(ctx, o());
assert.strictEqual(B.draw(ctx, o()), false, "and once its plate has lingered the render stops asking for frames");
// a flock drawn bigger than its world size flies in a line: each later bird a beat behind, at least its own drawn height back
call("g0", "edit", 3); rects = []; B.draw(sctx, o({ z: 0.49 }));
var fl = B.list(), hgt = B.SP.finch.h * B.pxOf(0.49, 2) / m.a;   // world px
assert.ok(fl.length === 3 && fl[1].t0 - fl[0].t0 >= 90 && fl[2].t0 - fl[1].t0 >= 90, "a flock of three, a beat apart");
var r0 = B.route(spots.g0, "finch", 0), len0 = 0;
for (i = 1; i < r0.pts.length; i++) len0 += Math.abs(r0.pts[i].x - r0.pts[i - 1].x) + Math.abs(r0.pts[i].y - r0.pts[i - 1].y);
assert.ok((fl[1].t0 - fl[0].t0) / fl[0].dur * len0 >= hgt - 1, "a drawn height behind: " + (fl[1].t0 - fl[0].t0) + " ms");
assert.ok(fl[2].t0 + fl[2].dur <= fl[0].t0 + 2000, "its last bird gone 2 s after the lift: " + (fl[2].t0 + fl[2].dur - fl[0].t0) + " ms");
B.pin(now += 2200); B.draw(ctx, o());
assert.strictEqual(B.list().length, 0, "the flock gone in 2 s too");
B.pin(now += B.LINGER); B.draw(ctx, o());
// the caption on the head (gd-CykqEHeEpX), in screen px (three device px per sprite px here, DPR 2). draw() paints the birds
// before the figures and places their gardeners' plates but draws none; words() draws them after the figures and their bubbles
// (4n), in device px with the caller's transform put back, once a draw, so a render that drew no birds draws no stale words
var vp = { w: 3000, h: 3000 }, hx = function (id) { return m.a * (spots[id].x + 8) + m.e; }, hy = function (id) { return m.a * (spots[id].y + 4) + m.f; };
call("g0", "read", null, "Read"); call("g10", "test", null, "go_test"); B.draw(ctx, o()); B.pin(now += B.TAG_FADE); imgs = []; ops = [];
B.draw(sctx, o({ z: 0.49, vp: vp })); ops.push("figures"); sets = []; B.words(sctx);
assert.deepStrictEqual([ops.indexOf("bird") === 0, ops.slice(ops.indexOf("figures"))], [true, ["figures", "word", "word"]], "birds under the figures, one drawImage a word over them");
assert.deepStrictEqual(sets, [[1, 0, 0, 1, 0, 0], [0.98, 0, 0, 0.98, 40, 20]], "the words in device px, the transform put back");
imgs = []; B.words(sctx); assert.strictEqual(imgs.length, 0, "once a draw");
var tg = B.tags(), tag = function (id) { return tg.filter(function (l) { return l.id === id; })[0]; };
assert.deepStrictEqual(tg.map(function (l) { return l.id + ":" + l.word; }).sort(), ["g0:Read", "g10:go test"]);
tg.forEach(function (l) { assert.ok(l.h >= B.TAG_PX * 2 && l.h <= B.TAG_PX * 2 * 2, l.word + ": " + l.h + " device px tall, at DPR 2"); });
// no word on a face: each gardener's, from its hat's brim down, as wide as its padded map (device px)
function face(id) { var s = spots[id]; return { x: m.a * (s.x - 3) + m.e, y: m.a * (s.y + 4) + m.f, w: m.a * 22, h: m.a * 11 }; }
function over(a, b) { return a.x < b.x + b.w && b.x < a.x + a.w && a.y < b.y + b.h && b.y < a.y + a.h; }
function offFaces(list, what) { list.forEach(function (l) { ids.forEach(function (id) { assert.ok(!over(l, face(id)), what + ": " + l.word + " on " + id + "'s face"); }); }); }
offFaces(tg, "lifting");
// each perches over its gardener's head at once: centred on it, wholly above its hat's brim, while the bird floats up under it
["g0", "g10"].forEach(function (id) { assert.ok(Math.abs(tag(id).x + tag(id).w / 2 - hx(id)) <= 1 && tag(id).y + tag(id).h <= hy(id), id + "'s plate over its head: " + JSON.stringify(tag(id))); });
// the newest call's word only: a flock carries one, and a gardener's earlier bird gives its word up
call("g0", "git", null, "git_push"); call("g10", "edit", 5, "Edit"); B.draw(sctx, o({ z: 0.49, vp: vp }));
assert.deepStrictEqual(B.tags().map(function (l) { return l.id + ":" + l.word; }).sort(), ["g0:git push", "g10:Edit"]);
assert.deepStrictEqual([B.list().filter(function (x) { return x.id === "g0"; }).length, B.list().filter(function (x) { return x.id === "g10"; }).length], [2, 3]);
// three gardeners side by side: no two plates overlap, and none is on a face; g0's, a tile from g1's newer one, which has no
// clear side either, is dropped (a plate stands over a head, so two heads a tile apart carry one between them)
B.pin(now += 16); call("g1", "read", null, "Grep"); B.draw(sctx, o({ z: 0.49, vp: vp }));
tg = B.tags();
assert.deepStrictEqual(tg.map(function (l) { return l.id + ":" + l.word; }).sort(), ["g10:Edit", "g1:Grep"]);
tg.forEach(function (a, i) { tg.slice(i + 1).forEach(function (b) { assert.ok(!over(a, b), a.word + " over " + b.word); }); });
offFaces(tg, "side by side");
// off the sign's tip (in CSS px) too: on another side, or dropped
var g1 = tag("g1"), tip = { x: g1.x / 2, y: g1.y / 2, w: 4, h: 4 };
B.draw(sctx, o({ z: 0.49, vp: vp, keep: [tip] }));
assert.ok(B.tags().every(function (l) { return !over(l, { x: tip.x * 2, y: tip.y * 2, w: 8, h: 8 }); }), "kept off the sign's tip");
// the plate stays on the head while its bird floats up, solid, and once the birds are gone, until LINGER ms after the call
var lead = B.list().filter(function (x) { return x.id === "g1"; })[0];
B.pin(lead.t0 + lead.dur - 150); B.draw(sctx, o({ z: 0.49, vp: vp }));
g1 = B.tags().filter(function (l) { return l.id === "g1"; })[0];
assert.ok(Math.abs(g1.x + g1.w / 2 - hx("g1")) <= 1 && g1.y + g1.h <= hy("g1") && g1.a === 1, "on its head, solid, as its bird goes: " + JSON.stringify(g1));
B.pin(now += 3500); B.draw(ctx, o());
assert.strictEqual(B.list().length, 0, "the birds gone");
assert.deepStrictEqual(B.tags().map(function (l) { return l.id + ":" + l.word; }).sort(), ["g0:git push", "g10:Edit", "g1:Grep"], "their plates still up (in world px, at DPR 1, g0's fits on its left)");
B.pin(now += B.LINGER); B.draw(ctx, o());
assert.deepStrictEqual([B.list().length, B.tags().length, B.perched().length], [0, 0, 0], "and gone LINGER ms after the calls");
// lingering (gd-2mYRxayl9b, spec 9.2): the plate perches over its gardener's head from the call for LINGER ms, solid at once
// (a pinned clock still shows it) and fading out over its last 300 ms; held still between, it asks for no frames once its bird is gone
var lg = function (id) { return B.tags().filter(function (l) { return l.id === id; })[0]; }, mine = function (id) { return B.list().filter(function (x) { return x.id === id; }); };
B.pin(now += 16); var t5 = now; call("g5", "read", null, "Read", "Read the linger section of the captions spec"); B.draw(sctx, o({ z: 0.49, vp: vp }));
var l5 = lg("g5"), b5 = mine("g5")[0], gone = b5.t0 + b5.dur;
assert.deepStrictEqual([mine("g5").length, l5.word, l5.a], [1, "Read the linger section of the captions spec", 1], "its bird up, its plate on the head, solid at once");
B.pin(t5 + 30); assert.ok(B.draw(sctx, o({ z: 0.49, vp: vp })), "its bird up: frames wanted");
l5 = lg("g5"); assert.strictEqual(l5.a, 1, "solid: " + l5.a);
B.pin(gone + 30); assert.strictEqual(B.draw(sctx, o({ z: 0.49, vp: vp })), false, "the bird gone, the plate held still: no frames");
l5 = lg("g5");
assert.ok(mine("g5").length === 0 && l5.a === 1 && Math.abs(l5.x + l5.w / 2 - hx("g5")) <= 1 && l5.y + l5.h <= hy("g5"), "solid, over its head: " + JSON.stringify(l5));
offFaces([l5], "perched");
B.pin(t5 + B.LINGER - 150); assert.ok(B.draw(sctx, o({ z: 0.49, vp: vp })), "fading out: frames wanted");
assert.ok(lg("g5").a > 0.4 && lg("g5").a < 0.6, "half faded 150 ms before it goes: " + lg("g5").a);
B.pin(t5 + B.LINGER); assert.strictEqual(B.draw(sctx, o({ z: 0.49, vp: vp })), false);
assert.deepStrictEqual([B.tags().length, B.perched().length], [0, 0], "gone LINGER ms after the call");
// placed each frame clear of a figure walking under it (ANIM.faces): it takes its other side, never the walker's face
var AN = require(require("path").join(require("path").dirname(process.argv[2]), "transitions.js")), faces0 = AN.faces;
B.pin(now = t5 + B.LINGER + 16); call("g5", "read", null, "Read", "Read the linger section of the captions spec"); B.draw(sctx, o({ z: 0.49, vp: vp }));
b5 = mine("g5")[0]; gone = b5.t0 + b5.dur;
B.pin(gone + 1000); B.draw(sctx, o({ z: 0.49, vp: vp })); l5 = lg("g5");
var wk = { id: "walker", x: Math.round((l5.x + l5.w - 2 - m.e) / m.a) - 21, y: Math.round((l5.y + l5.h / 2 - m.f) / m.a) - 9 };   // its face (HEAD) over the plate's right end (in view; the plate is centred on the head, so its left end is off it)
var wface = { x: m.a * (wk.x - 3) + m.e, y: m.a * (wk.y + 4) + m.f, w: m.a * 22, h: m.a * 11 };
assert.ok(over(l5, wface), "the walker's face is where the plate stood");
AN.faces = function () { return [wk]; }; B.draw(sctx, o({ z: 0.49, vp: vp }));
assert.ok(B.tags().every(function (l) { return !over(l, wface); }), "kept off the walker's face: " + JSON.stringify(B.tags()));
AN.faces = faces0; B.draw(sctx, o({ z: 0.49, vp: vp }));
assert.deepStrictEqual([lg("g5").x, lg("g5").y], [l5.x, l5.y], "back by its gardener once the walker has passed");
// one plate per gardener: calls faster than the linger show only the newest, flying or perched
var caps5 = ["First call", "Second call", "Third call", "Fourth call", "Fifth call"];
caps5.forEach(function (c, i) {
  B.pin(now = gone + 1000 + i * 2100); call("g5", ["read", "edit", "git", "test", "read"][i], null, "Read", c); B.draw(sctx, o({ z: 0.49, vp: vp }));
  for (var f = 0; f < 2100; f += 100) {
    B.pin(now + f); B.draw(sctx, o({ z: 0.49, vp: vp }));
    var mineT = B.tags().filter(function (l) { return l.id === "g5"; });
    assert.ok(mineT.length <= 1 && (!mineT.length || mineT[0].word === c), "call " + i + " +" + f + " ms: " + JSON.stringify(mineT.map(function (l) { return l.word; })));
    assert.ok(B.perched().every(function (q) { return q.id !== "g5" || q.word === c; }), "perched: the newest only");
  }
  assert.ok(lg("g5"), "call " + i + " still up before the next");
});
// a late caption (a farmdoing for the call's at, spec 7.2) rewrites the plate still up; one for another call does not
B.pin(now += 2100); B.hear({ id: "g5", doing: "shell", tool: "other", at: 7, dur: 0 }); B.draw(sctx, o({ z: 0.49, vp: vp }));
assert.strictEqual(lg("g5").word, "shell", "no caption yet: its tool word");
fire("farmdoing", { id: "g5", at: 6, cap: "Not this call" }); fire("farmdoing", { id: "g5", at: 7, cap: "" }); B.draw(sctx, o({ z: 0.49, vp: vp }));
assert.strictEqual(lg("g5").word, "shell");
fire("farmdoing", { id: "g5", at: 7, cap: "List  the open\nbeads" }); B.draw(sctx, o({ z: 0.49, vp: vp }));
assert.strictEqual(lg("g5").word, "List the open beads", "its bird up: rewritten");
B.pin(now += 2100); B.draw(sctx, o({ z: 0.49, vp: vp }));
fire("farmdoing", { id: "g5", at: 7, cap: "List the open beads in gardenio" }); B.draw(sctx, o({ z: 0.49, vp: vp }));
assert.strictEqual(lg("g5").word, "List the open beads in gardenio", "its bird gone: rewritten");
// none below the Plan level, nor after it
B.draw(ctx, o({ level: 1 })); B.draw(sctx, o({ z: 0.49, vp: vp }));
assert.deepStrictEqual([B.tags().length, B.perched().length], [0, 0], "World and Project: nothing lingers");
// at most MAX plates on the farm, the oldest perched going first; a quiet farm clears every one LINGER ms after its bird
ids.slice(0, 24).forEach(function (id) { B.pin(now += 10); call(id, "shell", null, "other", "Call of " + id); B.draw(ctx, o()); });
B.pin(now += 2100); B.draw(ctx, o());
assert.deepStrictEqual([B.list().length, B.perched().length], [0, 24], "24 perched");
ids.slice(24).forEach(function (id) { B.pin(now += 10); call(id, "shell", null, "other", "Call of " + id); B.draw(ctx, o()); });
assert.deepStrictEqual(B.perched().map(function (q) { return q.id; }), ids.slice(6).sort(), "six more up: the six oldest dropped");
// with the farm's birds full a gardener's next call has no bird: its plate says that call all the same
ids.slice(0, 6).forEach(function (id) { call(id, "edit", 3, "Edit", "Flock of " + id); }); B.draw(ctx, o());
assert.deepStrictEqual([B.list().length, B.perched().map(function (q) { return q.id; })], [B.MAX, ids.slice(0, 6).concat(ids.slice(12)).sort()], "the birds full, the 24 newest calls' plates up");
call("g23", "read", null, "Read", "No bird for this one"); B.draw(ctx, o());
assert.deepStrictEqual([B.list().length, B.perched().filter(function (q) { return q.id === "g23"; })[0].word], [B.MAX, "No bird for this one"]);
B.pin(now + B.LINGER - 100); B.draw(ctx, o());
assert.ok(B.perched().length > 0, "still lingering just before");
B.pin(now + B.LINGER + B.TAG_FADE); assert.strictEqual(B.draw(ctx, o()), false, "a quiet farm asks for no frames");
assert.deepStrictEqual([B.list().length, B.tags().length, B.perched().length], [0, 0, 0], "and has no plate up");
// the caption on its plate (gd-TmYAAOBQEH): one plate for the flock; at most 200 CSS px wide on a phone
// (400 device px at DPR 2) and 240 otherwise, two lines, the second cut to a word and ending in "…"
var cap1 = "Trial-merge main into #1282 in a detached worktree, then push the branch for review", lh = Math.ceil(B.TAG_PX * 2 * B.PLATE.lh);
[[{ w: 390, h: 844 }, 200, ["Trial-merge main into #1282 in", "a detached worktree, then push…"]], [{ w: 1280, h: 800 }, 240, ["Trial-merge main into #1282 in a", "detached worktree, then push the…"]]].forEach(function (c) {
  m = { a: 0.98, b: 0, c: 0, d: 0.98, e: -1400, f: -600 };   // g0 on the phone's screen
  B.pin(now += 16); call("g0", "shell", 3, "other", cap1); B.draw(sctx, o({ z: 0.49, vp: c[0] }));
  var pl = B.tags();
  assert.deepStrictEqual(pl.map(function (l) { return l.id + ":" + l.word; }), ["g0:" + cap1], "one plate for the flock: " + JSON.stringify(pl));
  assert.strictEqual(B.list().filter(function (x) { return x.id === "g0"; }).length, 3, "a flock of three");
  assert.deepStrictEqual(pl[0].lines, c[2], c[1] + " px");
  assert.ok(pl[0].w <= c[1] * 2 && pl[0].h === 2 * lh + 2 * (B.PLATE.py * 2 + 2), "a plate " + pl[0].w + " x " + pl[0].h + " device px, cap " + c[1] + " CSS px");
  offFaces(pl, "a plate at " + c[1]);
  B.pin(now += 2200 + B.LINGER); B.draw(ctx, o());
});
m = { a: 0.98, b: 0, c: 0, d: 0.98, e: 40, f: 20 };
// a hawk carries its subagent's description
B.hear({ id: "g3", doing: "delegate", tool: "Agent", at: 1, dur: 0, cap: "Explore gardenhub transcript endpoints" }); B.pin(now += 16); B.draw(sctx, o({ z: 0.49, vp: vp }));
assert.deepStrictEqual(B.tags().map(function (l) { return l.id + ":" + l.word; }), ["g3:Explore gardenhub transcript endpoints"], "the hawk's plate");
B.hear({ id: "g3", doing: "delegate", tool: "subagent", at: 1, dur: 4000 }); B.pin(now += 600); B.draw(ctx, o());
assert.deepStrictEqual(B.hawks(), [], "landed");
// a crowded plot, by day and at night: every gardener's call captioned, the plates placed never overlap one another or a face;
// a head whose caption has no clear side takes its tool word's plate there instead, and one with neither none
[false, true].forEach(function (night) {
  B.pin(now += 2200 + B.LINGER); B.draw(ctx, o());
  ids.forEach(function (id, i) { call(id, ["read", "edit", "test", "git"][i % 4], null, "Read", "Step " + i + ": read the plan, claim its next task and check the team news"); });
  rects = []; B.draw(sctx, o({ z: 0.49, vp: vp, night: night })); var pl = B.tags();
  var capd = pl.filter(function (l) { return /^Step /.test(l.word); }), words = pl.filter(function (l) { return !/^Step /.test(l.word); });
  assert.ok(capd.length >= 1 && capd.length + words.length >= 2, capd.length + " captions and " + words.length + " tool words over the crowded plot (a row of 30 figures a tile apart), night " + night);
  pl.forEach(function (a, i) { pl.slice(i + 1).forEach(function (b) { assert.ok(!over(a, b), a.word + " over " + b.word); }); });
  capd.forEach(function (l) { assert.ok(l.lines.length === 2 && l.w <= 480, l.word); });
  words.forEach(function (l) { assert.ok(["Read", "Edit", "go test", "git push"].indexOf(l.word) >= 0 && l.lines.length === 1, l.word); });
  offFaces(pl, "crowded, night " + night);
});
B.pin(now += 2200 + B.LINGER); B.draw(ctx, o());
// reduced motion: the plate stands beside the still bird, then over the head once it is gone, with no fade
var t4 = now; call("g4", "read", null, "Read", "Read web/birds.js"); B.draw(sctx, o({ z: 0.49, vp: vp, reduce: true }));
assert.deepStrictEqual([B.list().map(function (x) { return x.id + ":" + x.still; }), B.tags().map(function (l) { return l.word + ":" + l.a; })], [["g4:true"], ["Read web/birds.js:1"]], "a still bird and its plate");
offFaces(B.tags(), "still");
assert.ok(B.tags()[0].x >= B.tags()[0].cx, "beside the still bird, on its right");
B.pin(now += B.STILL + 1); assert.strictEqual(B.draw(sctx, o({ z: 0.49, vp: vp, reduce: true })), false, "held: no frames");
assert.deepStrictEqual([B.list().length, B.tags().map(function (l) { return l.word + ":" + l.a; })], [0, ["Read web/birds.js:1"]], "its bird gone, the plate stays");
assert.ok(Math.abs(B.tags()[0].x + B.tags()[0].w / 2 - hx("g4")) <= 1 && B.tags()[0].y + B.tags()[0].h <= hy("g4"), "over its head: " + JSON.stringify(B.tags()[0]));
B.pin(t4 + B.LINGER - 2); B.draw(sctx, o({ z: 0.49, vp: vp, reduce: true }));
assert.strictEqual(B.tags()[0].a, 1, "no fade under reduced motion");
B.pin(now += B.LINGER); B.draw(ctx, o({ reduce: true }));
assert.deepStrictEqual([B.list().length, B.tags().length], [0, 0], "gone LINGER ms after its bird");
// caps: at most 24 live
ids.forEach(function (id) { call(id, "shell"); }); B.draw(ctx, o());
assert.strictEqual(B.list().length, B.MAX, "at most 24 birds");
B.pin(now += 2200 + B.LINGER); B.draw(ctx, o());
// nothing below the Plan level, off the view, for a gardener not shown or on its way
call("g0", "read"); B.draw(ctx, o({ level: 1 })); B.draw(ctx, o());
assert.strictEqual(B.list().length, 0, "World and Project: no birds, and none waiting");
call("g0", "read"); call("off", "read"); B.draw(ctx, o({ view: { x: 0, y: 0, w: 10, h: 10 } }));
call("g1", "read"); call("off", "read"); B.draw(ctx, o({ moving: function (id) { return id === "g1"; } }));
assert.strictEqual(B.list().length, 0, "off the view, not shown, on its way: no birds");
// reduced motion: one still bird over the figure for 1 s
call("g0", "git", 3, "git_push"); B.draw(ctx, o({ reduce: true })); call("g0", "git", null, "git_push"); B.draw(ctx, o({ reduce: true }));
assert.deepStrictEqual(B.list(), [{ id: "g0", sp: "twig", t0: now, dur: B.STILL, still: true, barn: "" }]);
// perched on its head: centred on it and wholly above the padded 22 x 26 map (ART.GD_MAPS, the 16 x 24 figure at 3, 1 in it), so
// no pose's prop and no figure a tile to either side covers it
var px = []; B.draw({ drawImage: function () {}, fillRect: function (x, y, w, h) { for (var i = 0; i < w; i++) for (var j = 0; j < h; j++) px.push([x + i, y + j]); } }, o({ reduce: true }));
var s0 = spots.g0, xs = px.map(function (q) { return q[0]; }), ys = px.map(function (q) { return q[1]; });
Object.keys(A.GD_MAPS).forEach(function (pose) { assert.ok(A.GD_MAPS[pose].length <= 26, pose); });
assert.ok(Math.max.apply(null, ys) < s0.y - 1, "over the figure's map");
assert.ok(Math.abs((Math.min.apply(null, xs) + Math.max.apply(null, xs) + 1) / 2 - (s0.x + 8)) <= 0.5, "centred on its head");
// on the page too, at its own size (a DOMMatrix is single precision, so its scale over the zoom is a hair under the DPR)
m = { a: Math.fround(0.49), b: 0, c: 0, d: Math.fround(0.49), e: 0, f: 0 }; rects = []; B.draw(sctx, o({ z: 0.49, reduce: true }));
assert.strictEqual(span(rects.filter(function (r) { return r.c !== B.DAY_RIM; }), "x", "w"), B.SP.twig.w * 2, "two device px per sprite px at DPR 1 (1.5 rounded)");
assert.strictEqual(span(rects, "x", "w"), B.SP.twig.w * 2 + 2, "in its day rim");
var x0 = Math.min.apply(null, rects.map(function (r) { return r.x; }));
assert.strictEqual(Math.max.apply(null, rects.map(function (r) { return r.y + r.h; })), Math.round(m.a * (s0.y - 1)), "standing a px over the map");
assert.ok(Math.abs(x0 + (B.SP.twig.w * 2 + 2) / 2 - m.a * (s0.x + 8)) <= 0.5, "centred on its head");
// its word beside it, level with it, for its 1 s, not faded
var st = B.tags(), bx1 = Math.max.apply(null, rects.map(function (r) { return r.x + r.w; })), by0 = Math.min.apply(null, rects.map(function (r) { return r.y; })), by1 = Math.max.apply(null, rects.map(function (r) { return r.y + r.h; }));
assert.deepStrictEqual(st.map(function (l) { return l.word + ":" + l.a; }), ["git push:1"], "the still bird's word");
assert.ok(st[0].x >= bx1 && st[0].x <= bx1 + 3, "beside it");
assert.ok(Math.abs(st[0].y + st[0].h / 2 - (by0 + by1) / 2) <= 1, "level with it");
// at the Bead level (one device px a world px here, a sparrow 22 px wide in its rim), where words ran under bubbles and into faces
m = { a: 1, b: 0, c: 0, d: 1, e: 0, f: 0 };
var bead = function (x) { var r = o({ level: 3, z: 1, reduce: true }); for (var k in x) r[k] = x[k]; return r; };
var left = function (l) { return l.x + l.w <= l.cx - 11; }, right = function (l) { return l.x >= l.cx + 11; };
B.pin(now += 1001); B.draw(ctx, o({ reduce: true })); call("g0", "read", null, "Read");
B.draw(sctx, bead()); st = B.tags();
assert.ok(st.length === 1 && right(st[0]), "beside it, on its right: " + JSON.stringify(st));
// its word keeps off its gardener's "?" bubble (it was drawn over "Read"): to its left
var asks = { lookOf: function (id) { return { pose: id === "g0" ? "ask" : "work" }; } }; for (k in gs) asks[k] = gs[k];
B.draw(sctx, bead({ gs: asks })); st = B.tags();
assert.ok(st.length === 1 && left(st[0]) && st[0].y + st[0].h === st[0].cy + 7, "left of its bird, clear of its bubble, its foot level with the bird's (a plate taller than it): " + JSON.stringify(st));
// and off the cursor's ring, the DOM outline over the canvas: a word across the ring's band takes the other side
var ring = { left: (s0.x + 24) + "px", top: (s0.y - 40) + "px", width: "200px", height: "200px" };
document.querySelector = function (q) { return q === "#hots .hot.cur" ? { style: ring } : null; };
B.draw(sctx, bead()); st = B.tags();
assert.ok(st.length === 1 && left(st[0]), "left of its bird, clear of the ring: " + JSON.stringify(st));
ring.left = (s0.x - 100) + "px"; B.draw(sctx, bead());
assert.ok(right(B.tags()[0]), "inside the ring, clear of its band: on its right again");
delete document.querySelector;
// a face on either side of it, level with its word (a gardener a row up, two tiles off, as "WebFetch" ran into one): over it;
// and one over it too: dropped, never drawn over a face
var gf = { 3: { spots: { g0: spots.g0, up1: spot(p.x + 3, p.y + 30), up2: spot(p.x - 1, p.y + 30) } } };
B.draw(sctx, bead({ gs: gf })); st = B.tags();
assert.ok(st.length === 1 && Math.abs(st[0].x + st[0].w / 2 - st[0].cx) <= 1 && st[0].y + st[0].h <= st[0].cy - 7, "over its bird, centred: " + JSON.stringify(st));
gf[3].spots.up3 = spot(p.x + 1, p.y + 29); B.draw(sctx, bead({ gs: gf }));
assert.deepStrictEqual(B.tags(), [], "no side clear of a face: no word");
gf[3].spots.up2 = spot(p.x - 6, p.y + 30); B.draw(sctx, bead({ gs: gf }));
assert.ok(B.tags().length === 1 && left(B.tags()[0]), "its left clear: there");
// a still bird under the figure a row up (on its face, so hidden by it): no word, which would seem to name that gardener
B.draw(sctx, bead({ gs: { 3: { spots: { g0: spots.g0, above: spot(p.x + 1, p.y + 30) } } } }));
assert.deepStrictEqual(B.tags(), [], "a hidden bird says nothing");
B.pin(now += 1001 + B.LINGER); B.draw(ctx, o({ reduce: true }));
assert.strictEqual(B.list().length, 0);
// a subagent: the hawk circles until it stops, then lands on the gardener
B.hear({ id: "g2", doing: "delegate", tool: "Agent", at: 1, dur: 0 }); B.hear({ id: "g2", doing: "delegate", tool: "subagent", at: 1, dur: 0 });
assert.deepStrictEqual(B.hawks(), ["g2"]);
assert.strictEqual(B.list().length, 0, "the hawk is not a bird of the call");
var s2 = spots.g2, e2 = { x: s2.x + 8, y: s2.y + 4 - B.HAWK_UP };
for (var k = 0; k < 12; k++) { var hp = B.hawkAt({ t0: now, land: 0 }, s2, now + k * 200); assert.ok(Math.abs(hp.x - e2.x) <= 14 && Math.abs(hp.y - e2.y) <= 4, "the hawk circles over its head"); }
// it soars: each lap a glide, then one flap, a SOAR beat a frame; it lands in its glide
assert.deepStrictEqual(B.GLIDE.map(function (f, i) { return B.hawkAt({ t0: now, land: 0 }, s2, now + i * B.SOAR + 10).f; }), [3, 3, 0, 1, 2, 1]);
assert.strictEqual(B.GLIDE.length * B.SOAR, 2400, "a flap a lap");
assert.strictEqual(B.hawkAt({ t0: now, land: now + 1000 }, s2, now + 1200).f, 3, "landing in its glide");
var hl = B.hawkAt({ t0: now, land: now + 1000 }, s2, now + 1499); assert.ok(Math.abs(hl.x - (s2.x + 8)) <= 1 && Math.abs(hl.y - (s2.y + 4)) <= 1, "onto the head");
B.pin(now += 5000); assert.ok(B.draw(ctx, o()), "a circling hawk asks for frames");
assert.deepStrictEqual(B.tags().map(function (l) { return l.id + ":" + l.word; }), ["g2:subagent"], "its head says subagent while the hawk circles");
B.pin(now += B.LINGER + 1000); B.draw(ctx, o());
assert.deepStrictEqual(B.perched().map(function (q) { return q.id; }), ["g2"], "and keeps saying it past LINGER while it circles");
B.hear({ id: "g2", doing: "delegate", tool: "subagent", at: 1, dur: 4000 });
B.pin(now += 250); B.draw(ctx, o()); assert.deepStrictEqual(B.hawks(), ["g2"], "landing");
B.pin(now += 300); B.draw(ctx, o()); assert.deepStrictEqual(B.hawks(), [], "landed");
// busy: the project's full sign at World and Project, for 30 s
var tree = { nodes: { "project:gardenio": { kind: "biome", key: "gardenio" }, "project:music": { kind: "biome", key: "music" } } };
B.pin(now = now + 31000); call("g0", "read");
function signed(id, lv) { return B.sign(tree.nodes[id], "3 plans", lv); }
assert.deepStrictEqual([signed("project:gardenio", 0), signed("project:gardenio", 1), signed("project:gardenio", 2), signed("project:music", 1)],
  ["3 plans · busy", "3 plans · busy", "3 plans", "3 plans"]);
assert.strictEqual(B.stale(), false);
B.pin(now += 30001); assert.strictEqual(B.stale(), true, "gone quiet: the signs are re-read");
assert.strictEqual(signed("project:gardenio", 1), "3 plans");
// at its field stand (5b, gd-1kFVOmUjiK; homestead-world-design.md 28.4, 28.12): a figure at home (WORLD.homeTile, the one
// WORLD.homeFill drew in its project's Growing field) flies its birds up off its head as from a plot, kept inside the field's
// columns, never off over the Barn or the Pen
function hplan(id, project) { return { id: id, title: id, project: project, created: 1, activity: 1, counts: {}, phases: [], unphased: { id: "", counts: {}, tasks: [] } }; }
var hw = W.layout({ plans: [hplan("pa", "alpha"), hplan("pb", "beta")], seeds: [], closed_plans: { total: 0, shown: [] }, meadow: { total: 0, shown: [] } }, null, { now: 1, readonly: true }), hg = [];
function hgard(id, project) { hg.push({ id: id, agent: id, role: "crew", project: project, state: "idle", bucket: "idle", since: hg.length, href: "" }); }
for (i = 0; i < 7; i++) hgard("h" + i, "alpha");   // as many as alpha's field draws (28.12), its last slot free of the "+N"
for (i = 0; i < 2; i++) hgard("k" + i, "beta");
hgard("nohome", "alpha");
var hst = W.gardenerState(hw, hg);
W.homeFill(hw, { agent: W.gardenerField(hst, null, "", 0, function (it) { return it.id !== W.FIELD + "nohome"; }) }, {});   // out of the Mine scope: not drawn
var ca = hw.index["project:alpha"].home.cells.growing, Y = B.fieldSky(hw, "project:alpha");
assert.deepStrictEqual(Y, { barn: "project:alpha", at: ca.y * T + T / 2, x0: ca.x * T, x1: (ca.x + ca.w - 1) * T }, "the top row of alpha's Growing field, inside its columns");
assert.strictEqual(B.fieldSky(hw, "project:nope"), null);
for (i = 0; i < 7; i++) {
  var ys = W.homeTile(hw, hst.byId["h" + i]);
  assert.ok(ys.drawn && ys.biome === "project:alpha" && ys.x >= ca.x * T && ys.x + 16 <= (ca.x + ca.w) * T, "h" + i + " is drawn at its stand in alpha's field");
  [0, 1].forEach(function (seed) {
    var r = B.route(ys, "sparrow", seed, Y);
    assert.deepStrictEqual([r.pts[0].x, r.pts[0].y], [ys.x + 8, ys.y + 4], "off its head");
    assert.deepStrictEqual([r.pts[1].x, r.pts[1].y], [ys.x + 8, ys.y + 4 - B.RUN], "straight up off it");
    assert.strictEqual(r.pts[2].y, ys.y + 4 - 1.5 * B.RUN, "on up and away");
    assert.ok(r.pts[2].x >= Y.x0 && r.pts[2].x <= Y.x1, "h" + i + " seed " + seed + ": inside the cell's columns, " + r.pts[2].x);
    samples(r).forEach(function (q) { if (q && !q.done) assert.ok(q.x >= ca.x * T && q.x <= (ca.x + ca.w) * T, "never off the field, over the Barn or the Pen"); });
  });
}
var h3 = W.homeTile(hw, hst.byId.h3), hk = B.hawkAt({ t0: 0, land: 0 }, h3, 600); assert.ok(Math.abs(hk.y - (h3.y + 4 - B.HAWK_UP)) <= 4, "a hawk circles over its head at its stand");
// on the page: birds for the figures drawn in a field, none for one at home with no figure (past the field's "+N", or out of the
// Mine scope); at most BARN_MAX over each field, another field and the plots keep theirs
var hs = {};
Object.keys(hst[3].spots).forEach(function (id) { hs[id] = hst[3].spots[id]; });
for (i = 0; i < 6; i++) hs["g" + i] = spots["g" + i];
var gh = { 2: { spots: hs }, 3: { spots: hs }, byId: hst.byId, look: hst.look }, hwo = { index: Object.assign({}, plots, hw.index) };
B.pin(now += 5000); B.draw(ctx, o({ gs: gh, world: hwo, view: null }));
call("nohome", "read"); B.draw(ctx, o({ gs: gh, world: hwo, view: null }));
assert.strictEqual(B.list().length, 0, "at home with no figure drawn: no bird");
call("h0", "read"); B.draw(ctx, o({ gs: gh, world: hwo, view: null }));
assert.deepStrictEqual(B.list().map(function (x) { return x.id + ":" + x.barn; }), ["h0:project:alpha"], "a field figure's bird, over its own field");
B.pin(now += 3000); B.draw(ctx, o({ gs: gh, world: hwo, view: null }));
for (i = 0; i < 7; i++) call("h" + i, "edit", 3);
for (i = 0; i < 2; i++) call("k" + i, "edit", 3);
for (i = 0; i < 6; i++) call("g" + i, "edit", 3);
B.draw(ctx, o({ gs: gh, world: hwo, view: null }));
function overBarn(y) { return B.list().filter(function (x) { return x.barn === y; }).length; }
assert.strictEqual(overBarn("project:alpha"), B.BARN_MAX, "at most " + B.BARN_MAX + " over alpha's field");
assert.strictEqual(overBarn("project:beta"), 6, "beta's field is not held back by alpha's");
assert.strictEqual(overBarn(""), B.MAX - B.BARN_MAX - 6, "the plots take the rest, to MAX");
B.pin(now += 3000); B.draw(ctx, o({ gs: gh, world: hwo, view: null }));
assert.strictEqual(B.list().length, 0);
`
	var args []string
	for _, f := range []string{"world.js", "birds.js", "art.js", "activity.js"} {
		p, err := filepath.Abs(filepath.Join("web", f))
		if err != nil {
			t.Fatal(err)
		}
		args = append(args, p)
	}
	out, err := exec.Command(node, append([]string{"-e", script}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("node birds test failed: %v\n%s", err, out)
	}
}

// birds.js loads after the modules it reads (world.js, activity.js, transitions.js) and before garden.js, which draws the birds
// before the gardeners, their words after them, and asks the signs for "busy".
func TestBirdsJSOnThePage(t *testing.T) {
	at := func(s string) int { return strings.Index(pageHTML, `{{asset "`+s+`"}}`) }
	b := at("birds.js")
	for _, s := range []string{"world.js", "activity.js", "transitions.js"} {
		if a := at(s); a < 0 || b < 0 || a > b {
			t.Errorf("%s at %d, birds.js at %d: birds.js must load after it", s, a, b)
		}
	}
	if g := at("garden.js"); b > g {
		t.Errorf("birds.js at %d, garden.js at %d: birds.js must load first", b, g)
	}
	d, g := strings.Index(pageSceneJS, "BIRDS.draw("), strings.Index(pageSceneJS, "ART.drawPlotGardeners(")
	if d < 0 || g < 0 || d > g {
		t.Error("scene.js does not draw the birds before the gardeners")
	}
	// their words after the figures, their bubbles and the transitions, in draw() after drawWorldLayer (4n, gd-VHkZ3pCEqK)
	w, l, a := strings.Index(pageSceneJS, "BIRDS.words(ctx)"), strings.Index(pageSceneJS, "drawWorldLayer(vis);"), strings.Index(pageSceneJS, "ANIM.draw(ctx,")
	if w < 0 || l < 0 || a < 0 || w < l || w < a {
		t.Error("scene.js does not draw the birds' words after the figures and the transitions")
	}
	if !strings.Contains(pageAppJS, "BIRDS.sign(") || !strings.Contains(pageAppJS, "BIRDS.onWake(") {
		t.Error("garden.js does not ask the signs for busy or wake for a bird")
	}
}
