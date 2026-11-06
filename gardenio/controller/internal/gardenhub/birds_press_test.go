package gardenhub

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// From a label to the Glass (gd-jt6OwmV4Jk, bird-captions spec 10), under node: the hit test over BIRDS.tags() (a plate and 4 px
// round it, a bird's sprite box, never a faded plate or a perched plate's gone bird); the press's state machine on a fake clock
// (the ring lit at 250 ms, the Glass at 500 ms; a short press, a move of 10 px, a second pointer, a camera move or a cancel is
// no press; a right-click or a touch's own contextmenu opens at once and only once, and a touch's after its press was dropped
// is eaten unfired, as Android sends one after a pan); the w key's gardener (a gardener's own, else the newest call's of those
// on the cursor's plot or step); and the page's side wired to a fake board: the Glass's address, said in #cursay, the click
// after a press eaten, nothing over the camera, Jump, tip, beacons or a panel, the toast for a viewer who may not watch,
// nothing below the Plan level, the ring drawn while held. Skipped when node is not installed.
func TestBirdsPressUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `
var on = {}, els = { cursay: { textContent: "" } }, body = { dataset: { glass: "1", base: "/b" } };
global.document = { addEventListener: function (k, f) { (on[k] = on[k] || []).push(f); }, body: body, getElementById: function (id) { return els[id] || null; }, querySelector: function () { return null; } };
global.location = { href: "" };
function fire(k, d) { (on[k] || []).forEach(function (f) { f({ detail: d }); }); }
var W = require(process.argv[1]), B = require(process.argv[2]), N = require(process.argv[3]), assert = require("assert"), T = W.TILE;

// the w key is the cursor's "watch" act; Shift+W is not
var key = function (k, shift) { return N.action({ key: k, shiftKey: !!shift }); };
assert.deepStrictEqual([key("w"), key("W"), key("W", true)], [{ act: "watch" }, { act: "watch" }, null]);
assert.deepStrictEqual([B.PRESS_MS, B.PRESS_LIT, B.PRESS_PX, B.HIT_PAD, B.TAP_PAD], [500, 250, 10, 4, 13], "a long press and its slack, a finger's slack");

// hitAt: a plate and HIT_PAD round it, the first in the list (the newest) wins, a faded plate is no target (a bird is none:
// the plate stays on the head and the bird floats off, gd-CykqEHeEpX)
var list = [{ gid: "a", a: 1, x: 10, y: 10, w: 50, h: 20 }, { gid: "b", a: 1, x: 40, y: 20, w: 50, h: 20 }, { gid: "c", a: 0, x: 200, y: 0, w: 50, h: 20 }];
assert.deepStrictEqual([[12, 12], [7, 15], [5, 15], [63, 33], [65, 15], [2, 2], [8, 9], [50, 25], [80, 35], [210, 5]].map(function (q) { return B.hitAt(list, q[0], q[1], 4); }),
  ["a", "a", "", "a", "", "", "a", "a", "b", ""]);

// presser on a fake clock
var now = 0, timers = [], fired = [], lits = [], cam = "c0", target = "g1";
function tick(ms) { var end = now + ms, t; for (;;) { t = timers.filter(function (x) { return !x.done && x.at <= end; }).sort(function (a, b) { return a.at - b.at; })[0]; if (!t) break; now = t.at; t.done = true; t.f(); } now = end; }
var P = B.presser({ hit: function () { return target; }, cam: function () { return cam; }, lit: function (id) { lits.push(id); }, fire: function (id, how) { fired.push(id + ":" + how); },
  set: function (f, ms) { var t = { f: f, at: now + ms }; timers.push(t); return t; }, clear: function (t) { if (t) t.done = true; } });
function reset() { fired = []; lits = []; cam = "c0"; target = "g1"; }
P.down(1, 100, 100, true); tick(249); assert.deepStrictEqual(lits, [], "not lit before 250 ms");
tick(1); assert.deepStrictEqual(lits, ["g1"], "its ring lit at 250 ms");
tick(249); assert.deepStrictEqual(fired, [], "not yet");
tick(1); assert.deepStrictEqual([fired, lits], [["g1:press"], ["g1", ""]], "at 500 ms the Glass, the ring put out");
assert.strictEqual(P.up(1), true, "the press's lift is not a tap");
reset(); P.down(1, 100, 100, true); tick(200); assert.strictEqual(P.up(1), false, "a short press keeps its meaning (the cursor)"); tick(1000); assert.deepStrictEqual([fired, lits], [[], []]);
reset(); P.down(1, 100, 100, true); P.move(1, 106, 103); tick(500); assert.deepStrictEqual(fired, ["g1:press"], "a wobble under 10 px is still a press"); P.up(1);
reset(); P.down(1, 100, 100, true); P.move(1, 106, 104); tick(500); assert.deepStrictEqual(fired, [], "10 px (Manhattan) is a pan, not a press"); P.up(1);
reset(); P.down(1, 100, 100, true); P.move(2, 300, 300); tick(500); assert.deepStrictEqual(fired, ["g1:press"], "another pointer's move is not this one's"); P.up(1);
reset(); P.down(1, 100, 100, true); tick(100); P.down(2, 140, 100, false); tick(1000); assert.deepStrictEqual(fired, [], "a second finger (a pinch) drops it"); P.up(2); P.up(1);
reset(); P.down(1, 100, 100, true); tick(300); cam = "c1"; tick(300); assert.deepStrictEqual([fired, lits], [[], ["g1", ""]], "the camera moved under it: no press, the ring out"); P.up(1);
reset(); P.down(1, 100, 100, true); cam = "c1"; tick(600); assert.deepStrictEqual([fired, lits], [[], []], "nor lit"); P.up(1);
reset(); P.down(1, 100, 100, true); tick(100); P.cancel(1); tick(1000); assert.deepStrictEqual(fired, [], "a pointercancel drops it");
reset(); target = ""; P.down(1, 100, 100, true); tick(1000); assert.deepStrictEqual([fired, lits], [[], []], "off a label or bird nothing arms"); P.up(1);
reset(); assert.strictEqual(P.context(5, 5), true); assert.deepStrictEqual(fired, ["g1:menu"], "a right-click opens it at once");
reset(); target = ""; assert.strictEqual(P.context(5, 5), false, "off a target the browser keeps its menu"); assert.deepStrictEqual(fired, []);
reset(); P.down(1, 100, 100, true); tick(500); assert.strictEqual(P.context(100, 100), true, "a touch's own contextmenu after the press is eaten"); assert.deepStrictEqual(fired, ["g1:press"], "once"); assert.strictEqual(P.up(1), true);
reset(); P.down(1, 100, 100, true); tick(300); assert.strictEqual(P.context(100, 100), true); tick(500); assert.deepStrictEqual(fired, ["g1:menu"], "a contextmenu before 500 ms opens it, once"); P.up(1);
reset(); P.down(7, 100, 100, true); P.down(8, 100, 100, true); tick(500); assert.deepStrictEqual(fired, ["g1:press"], "a new touch sequence forgets a ghost pointer"); P.up(8);
// Android ends a touch's long press in its own contextmenu even when the press was dropped: eaten, never fired
reset(); tick(1000); P.down(1, 100, 100, true, true); P.move(1, 112, 100); tick(500); assert.deepStrictEqual([P.context(112, 100, true), fired], [true, []], "a drift past 10 px, then the touch's menu");
reset(); P.down(1, 100, 100, true, true); tick(300); cam = "c1"; assert.deepStrictEqual([P.context(100, 100, false), fired, lits], [true, [], ["g1", ""]], "the camera moved: a touch is down, so its menu is eaten (no pointerType)");
P.up(1); assert.deepStrictEqual([P.context(100, 100, false), fired], [true, []], "just lifted, still eaten");
tick(500); assert.deepStrictEqual([P.context(100, 100, false), fired], [true, ["g1:menu"]], "PRESS_MS later a right-click is one again");
reset(); P.down(1, 100, 100, true, true); P.down(2, 140, 100, false, true); assert.deepStrictEqual([P.context(100, 100, true), fired], [true, []], "a pinch's menu"); P.up(2); P.up(1); tick(500);
reset(); target = ""; assert.deepStrictEqual([P.context(5, 5, true), fired], [true, []], "a touch's menu off a press is eaten too");

// watchOf: a gardener's own; else of those on the cursor's plot or step the newest call's
var st = { spots: { a: { plot: "P", cell: "T1", shown: true }, b: { plot: "P", cell: "T1", shown: true }, c: { plot: "P", cell: "", shown: true } }, plots: { P: { ids: ["a", "b", "c"] } } }, ats = { a: 10, b: 20, c: 5 };
var atOf = function (id) { return ats[id] || 0; };
assert.deepStrictEqual([B.watchOf(st, { id: "P", kind: "plot" }, atOf), B.watchOf(st, { id: "T1", kind: "task" }, atOf), B.watchOf(st, { id: "c", kind: "gardener" }, atOf), B.watchOf(st, { id: "X", kind: "task" }, atOf), B.watchOf(st, null, atOf)],
  ["b", "b", "c", "", ""]);
ats.b = 0; assert.strictEqual(B.watchOf(st, { id: "T1", kind: "task" }, atOf), "a", "the newest call's");
assert.deepStrictEqual([B.aria({ kind: "gardener" }), B.aria({ kind: "plot" })], [" Press W to watch it in the Glass.", ""]);

// the page's side: a still bird and its plate drawn at 0.49x, DPR 2, then pressed through a fake board
global.setTimeout = function (f, ms) { var t = { f: f, at: now + ms }; timers.push(t); return t; };
global.clearTimeout = function (t) { if (t) t.done = true; };
var p = { x: 100, y: 40, w: 32, h: 32 }, spots = { g0: { plot: "P", col: 101, row: 71, x: 101 * T, y: T * 71 - 8, shown: true, cell: "" } };
var gs = { 2: { spots: spots, plots: { P: { ids: ["g0"] } } }, 3: { spots: spots } }, view = { x: p.x * T - 64, y: p.y * T - 64, w: 40 * T, h: 40 * T }, vp = { w: 3000, h: 3000 };
var m = { a: 0.98, b: 0, c: 0, d: 0.98, e: 40, f: 20 }, strokes = [];
var sctx = { fillStyle: "", globalAlpha: 1, getTransform: function () { return m; }, setTransform: function () {}, fillRect: function () {}, drawImage: function () {},
  beginPath: function () {}, moveTo: function () {}, arcTo: function () {}, closePath: function () {}, stroke: function () { strokes.push(this.strokeStyle); } };
var o = { level: 2, z: 0.49, night: false, view: view, vp: vp, gs: gs, world: { index: { P: p } }, reduce: true, moving: function () { return false; } };
var t0 = 1792000000000; B.pin(t0);
fire("farmdoc", { gardeners: [{ id: "g0", agent: "g-zero", project: "Gardenio" }] });
fire("farmlive", { kind: "open" });
B.pin(t0 += 3000); B.hear({ id: "g0", doing: "read", tool: "Read", at: 1, dur: 0, cap: "Read web/birds.js" }); B.draw(sctx, o);
var tg = B.tags()[0];
assert.ok(tg && tg.gid === "g0" && tg.w > 0, "a plate says its gardener: " + JSON.stringify(tg));
assert.strictEqual(B.scale(), 2, "CSS px to the frame (device px at DPR 2)");
var level = 2, toasts = [], bon = {}, don = {}, board = { addEventListener: function (k, f) { (bon[k] = bon[k] || []).push(f); }, ownerDocument: { addEventListener: function (k, f) { (don[k] = don[k] || []).push(f); } } };
// ev dispatches on the board, then its document (a bubble); off(k) only on the document, as off the board (on a panel over it)
function ev(k, x, y, more, off) { var e = { type: k, pointerId: 1, pointerType: k === "contextmenu" ? "mouse" : "touch", button: 0, isPrimary: true, x: x, y: y, prevented: 0, stopped: 0, preventDefault: function () { this.prevented++; }, stopPropagation: function () { this.stopped++; } }; for (var j in more || {}) e[j] = more[j]; (off ? [] : bon[k] || []).concat(don[k] || []).forEach(function (f) { f(e); }); return e; }
var watch = B.wire(board, { level: function () { return level; }, cam: function () { return "fixed"; }, local: function (e) { return [e.x, e.y]; }, node: function () { return { id: "P", kind: "plot" }; }, st: function () { return gs[level]; }, toast: function (s) { toasts.push(s); } });
var cx = (tg.x + tg.w / 2) / 2, cy = (tg.y + tg.h / 2) / 2;   // the plate's middle, CSS px
ev("pointerdown", cx, cy); tick(250);
assert.strictEqual(B.pressed(), "g0", "held 250 ms: its ring lit");
B.draw(sctx, o); B.words(sctx); assert.deepStrictEqual(strokes, [B.INK], "the ring drawn in the ink at full strength");
tick(250);
assert.deepStrictEqual([location.href, els.cursay.textContent, B.pressed()], ["/b/garden?glass=g0", "Watching g-zero in the Glass", ""], "a long press opens its gardener's Glass");
ev("pointerup", cx, cy); var c = ev("click", cx, cy);
assert.deepStrictEqual([c.prevented, c.stopped], [1, 1], "its click is eaten: no cursor move, no thread");
tick(1); c = ev("click", cx, cy); assert.deepStrictEqual([c.prevented, c.stopped], [0, 0], "the next click is a click");
location.href = ""; ev("pointerdown", cx, cy); tick(200); ev("pointerup", cx, cy); tick(1000); assert.strictEqual(location.href, "", "a short press is the cursor's");
// a finger's press 10 px off the plate (gd-cihiPae1J9): an 18 px plate is a 44 px target under a finger, not under a mouse
var by = tg.y / 2 - 10;
assert.ok(tg.h / 2 + 2 * B.TAP_PAD >= 44, "a point 10 px over the plate, the plate " + tg.h / 2 + " px tall");
location.href = ""; ev("pointerdown", cx, by, { pointerType: "mouse" }); tick(600); ev("pointerup", cx, by, { pointerType: "mouse" }); tick(1); assert.strictEqual(location.href, "", "a mouse 10 px off misses it");
location.href = ""; ev("pointerdown", cx, by); tick(500); assert.strictEqual(location.href, "/b/garden?glass=g0", "a finger 10 px off still opens it"); ev("pointerup", cx, by); tick(1000); location.href = "";
c = ev("contextmenu", cx, cy);
assert.deepStrictEqual([c.prevented, location.href], [1, "/b/garden?glass=g0"], "a right-click on the plate opens it at once");
location.href = ""; c = ev("contextmenu", 1, 1); assert.deepStrictEqual([c.prevented, location.href], [0, ""], "off one the browser's menu");
ev("pointerdown", cx, cy, { pointerType: "mouse", button: 2 }); tick(1000); ev("pointerup", cx, cy); assert.strictEqual(location.href, "", "a right button held is not a long press");
// a mouse held until the Glass opens is let go over it, off the board: the press still ends, and the next right-click fires
location.href = ""; ev("pointerdown", cx, cy, { pointerType: "mouse" }); tick(500); ev("pointerup", cx, cy, { pointerType: "mouse" }, true); tick(1);
location.href = ""; c = ev("contextmenu", cx, cy); assert.deepStrictEqual([c.prevented, location.href], [1, "/b/garden?glass=g0"], "a right-click after a press let go on the Glass");
// over the camera, the Jump field, the tip, the beacons or a panel the board's own: no press, the browser's menu
var hud = function (id) { return { target: { closest: function (sel) { return sel.split(",").indexOf(id) >= 0 ? {} : null; } } }; };
[".cam", "#jump", "#stip", "#bcns", "#bead"].forEach(function (id) {
  location.href = ""; c = ev("contextmenu", cx, cy, hud(id)); assert.deepStrictEqual([c.prevented, location.href], [0, ""], "a right-click in " + id);
  ev("pointerdown", cx, cy, hud(id)); tick(600); ev("pointerup", cx, cy, hud(id)); assert.deepStrictEqual([location.href, B.pressed()], ["", ""], "a long press in " + id); tick(500);
});
// a touch press dropped by a pan, then Android's own long-press menu: eaten, the Glass not opened
location.href = ""; ev("pointerdown", cx, cy); ev("pointermove", cx + 15, cy); tick(500); c = ev("contextmenu", cx + 15, cy, { pointerType: "touch" });
assert.deepStrictEqual([c.prevented, location.href], [1, ""], "a pan's menu is eaten, not fired"); ev("pointerup", cx + 15, cy); tick(500);
level = 1; c = ev("contextmenu", cx, cy); assert.deepStrictEqual([c.prevented, location.href], [0, ""], "no birds below the Plan level, so no target"); level = 2;
body.dataset.glass = ""; ev("contextmenu", cx, cy); assert.deepStrictEqual([toasts, location.href], [["The Glass is not open to you here"], ""], "a viewer who may not watch is told so");
assert.strictEqual(B.aria({ kind: "gardener" }), "", "nor offered W"); body.dataset.glass = "1";
// the plate over the head once its still bird is gone: still a target
B.pin(t0 += B.STILL + 1); B.draw(sctx, o); tg = B.tags()[0];
assert.ok(tg && tg.gid === "g0", "still up, with no bird");
location.href = ""; ev("contextmenu", (tg.x + tg.w / 2) / 2, (tg.y + tg.h / 2) / 2); assert.strictEqual(location.href, "/b/garden?glass=g0", "a lingering plate opens it too");
// the w key: a focused gardener link's, else the cursor's plot's newest call
location.href = ""; watch({ _h: { kind: "gardener", g: { id: "gX" } } }); assert.strictEqual(location.href, "/b/garden?glass=gX");
location.href = ""; watch(null); assert.deepStrictEqual([location.href, els.cursay.textContent], ["/b/garden?glass=g0", "Watching g-zero in the Glass"]);
location.href = ""; level = 3; gs[3] = { spots: {} }; watch(null); assert.deepStrictEqual([location.href, els.cursay.textContent], ["", "No gardener at the cursor to watch"]);
`
	var args []string
	for _, f := range []string{"world.js", "birds.js", "curnav.js"} {
		p, err := filepath.Abs(filepath.Join("web", f))
		if err != nil {
			t.Fatal(err)
		}
		args = append(args, p)
	}
	out, err := exec.Command(node, append([]string{"-e", script}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("node birds press test failed: %v\n%s", err, out)
	}
	if s := strings.TrimSpace(string(out)); s != "" {
		t.Logf("%s", s)
	}
}

// The same gesture in Chromium on the fixture farm (cardWorld, the Glass on): on a desktop a click on a plate is no press, a
// mouse held on it lights its ring and then opens its gardener's Glass, and a right-click opens it at once; on a phone a finger
// held on it opens the Glass and one that pans first does not. What is asserted lives in testdata/birds_press_browser.js.
// BIRDS_PRESS_PNG=<dir> keeps each layout's plate as drawn and with its ring lit, for the commons-ux grade.
func TestBirdsPressUnderChromium(t *testing.T) {
	var args []string
	if d := os.Getenv("BIRDS_PRESS_PNG"); d != "" {
		args = append(args, d)
	}
	underChromium(t, "birds_press_browser.js", func() *httptest.Server { return cardWorld(t, time.Now) }, args...)
}
