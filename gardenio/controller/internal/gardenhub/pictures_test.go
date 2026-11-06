package gardenhub

import (
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// web/pictures.js's pure logic under node (Lands workbench-glass spec WB-18, step 5.15, gd-uEEeNMlk1a): the preview's size
// and where it stands beside its thumbnail, always inside the viewport; the viewer's set (a path on this site only, once
// each, the picture alone when the set does not hold it), its keys and swipes wrapping at the ends, its counter, and the
// address the Workbench host writes for it (?pic=<path>, one panel at a time).
func TestPicturesLogicUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `
var P = require(process.argv[1]), W = require(process.argv[2]).host, assert = require("assert");
function rect(l, t, w, h) { return { left: l, top: t, right: l + w, bottom: t + h, width: w, height: h }; }
function inside(p, vw, vh) { return p.x >= P.EDGE && p.y >= P.EDGE && p.x + p.w <= vw - P.EDGE && p.y + p.h <= vh - P.EDGE; }
// size: fits BOX of the shorter side; a big picture shrinks, a tiny one grows to twice its thumbnail at most, none without a size
assert.deepStrictEqual(P.size(2000, 1000, 320, 160, 1280, 800), { w: 560, h: 280 }, "a big picture: 0.7 of the shorter side");
assert.deepStrictEqual(P.size(400, 300, 96, 72, 1280, 800), { w: 400, h: 300 }, "a mid one at its natural size");
assert.deepStrictEqual(P.size(3, 2, 320, 240, 1280, 800), { w: 560, h: 373 }, "a tiny one at twice its thumbnail, within the box");
assert.deepStrictEqual(P.size(100, 100, 40, 40, 1280, 800), { w: 100, h: 100 }, "never past natural when that beats twice the thumbnail");
assert.strictEqual(P.size(0, 0, 10, 10, 800, 600), null);
// place: right when it fits, else left, else shrunk to the roomier side, else above or below, else over the middle, and
// never smaller than the thumbnail it previews: a place that would shrink it so falls through, to null when none is bigger
var vw = 1280, vh = 800, p;
p = P.place(rect(100, 100, 320, 240), 572, 385, vw, vh);
assert.deepStrictEqual([p.side, p.x, p.w, p.h], ["right", 428, 572, 385]); assert.ok(inside(p, vw, vh), JSON.stringify(p));
assert.strictEqual(p.y, Math.round(100 + 120 - 385 / 2), "centred on the thumbnail");
p = P.place(rect(900, 100, 320, 240), 572, 385, vw, vh);
assert.deepStrictEqual([p.side, p.x], ["left", 900 - 8 - 572]); assert.ok(inside(p, vw, vh));
p = P.place(rect(400, 300, 160, 120), 1000, 600, vw, vh);
assert.strictEqual(p.side, "right", "short of room both sides: the roomier"); assert.ok(p.w < 1000 && p.w >= P.MIN_SIDE && inside(p, vw, vh), JSON.stringify(p));
p = P.place(rect(400, 300, 480, 200), 572, 385, vw, vh);
assert.strictEqual(p.side, "over", "the roomier side, and above or below, would shrink it under the thumbnail: over the middle"); assert.ok(p.w === 572 && inside(p, vw, vh), JSON.stringify(p));
p = P.place(rect(8, 600, 374, 180), 400, 300, 390, 844);
assert.strictEqual(p.side, "above", "a phone-wide thumbnail low down: above it"); assert.ok(inside(p, 390, 844) && p.y + p.h <= 600 - P.GAP, JSON.stringify(p));
p = P.place(rect(100, 100, 200, 640), 370, 1200, 390, 844);
assert.strictEqual(p.side, "over", "no room anywhere: over the middle"); assert.ok(inside(p, 390, 844) && p.h > 640, JSON.stringify(p));
p = P.place(rect(10, 790, 50, 8), 300, 900, vw, vh);
assert.ok(inside(p, vw, vh), "a tall preview by a thumbnail at the bottom stays inside: " + JSON.stringify(p));
// gd-9BZII9m6Md: a 480x270 Commons attachment near full column width, raised by a keyboard focus. Mid 820x1180: the side
// has 302 px, under the 494 px thumbnail, so it stands below it at its full size. Phone 390x844: the picture is already as
// big as the screen allows (the preview could only be 273 px), so there is none.
var s = P.size(480, 270, 494, 283, 820, 1180);
p = P.place(rect(8, 409, 494, 283), s.w + P.FRAME, s.h + P.FRAME, 820, 1180);
assert.deepStrictEqual([p.side, p.w, p.h], ["below", 586, 335], "mid: below the thumbnail, not shrunk beside it: " + JSON.stringify(p)); assert.ok(inside(p, 820, 1180));
s = P.size(480, 270, 371, 212, 390, 844);
assert.strictEqual(P.place(rect(10, 278, 371, 212), s.w + P.FRAME, s.h + P.FRAME, 390, 844), null, "phone: no preview smaller than its thumbnail");
// and so for any thumbnail anywhere: no preview, or one at least the thumbnail's size and bigger on a side
[[390, 844], [820, 1180], [1280, 800], [1440, 900]].forEach(function (v) {
  for (var tw = 40; tw <= v[0] - 16; tw += 37) for (var l = 8; l + tw <= v[0] - 8; l += 61) for (var t = 8; t < v[1] - 40; t += 97) {
    var th = Math.round(tw * 9 / 16), z = P.size(480, 270, tw, th, v[0], v[1]), q = P.place(rect(l, t, tw, th), z.w + P.FRAME, z.h + P.FRAME, v[0], v[1]);
    assert.ok(q === null || (q.w >= tw && q.h >= th && (q.w > tw || q.h > th) && inside(q, v[0], v[1])), JSON.stringify([v, l, t, tw, th, q]));
  }
});
// paths and names
assert.strictEqual(P.path("/garden/c/f/gardenio/gd-ch/abc/plot.png"), "/garden/c/f/gardenio/gd-ch/abc/plot.png");
["//evil.example/x.png", "https://garden.example/image/a.png", "javascript:alert(1)", "/a\\\\b", "/a b", "", null, "image/a.png"].forEach(function (x) { assert.strictEqual(P.path(x), "", "not a path here: " + x); });
assert.strictEqual(P.nameOf("/garden/c/f/l/c/h/my%20plot.png?x=1"), "my plot.png");
assert.strictEqual(P.nameOf("/image/"), "picture");
// the set: in order, once each, without what is no path; src alone when the set lacks it; null for a foreign src
var items = [{ src: "/image/a.png", name: "A", open: "https://garden.example/image/a.png" }, { src: "/image/b.png", name: "" }, { src: "https://x.example/c.png" }, { src: "/image/a.png", name: "again" }, { src: "/image/d.png", name: "D" }];
s = P.setOf(items, "/image/b.png");
assert.deepStrictEqual(s, { i: 1, items: [{ src: "/image/a.png", name: "A", open: "https://garden.example/image/a.png" }, { src: "/image/b.png", name: "b.png", open: "/image/b.png" }, { src: "/image/d.png", name: "D", open: "/image/d.png" }] });
assert.deepStrictEqual(P.setOf(items, "/image/zz.png"), { i: 0, items: [{ src: "/image/zz.png", name: "zz.png", open: "/image/zz.png" }] }, "a reload: the picture alone");
assert.deepStrictEqual(P.setOf(null, "/garden/c/f/l/c/h/x.png").items.length, 1);
assert.strictEqual(P.setOf(items, "https://x.example/c.png"), null, "never a picture from elsewhere");
// cycling: keys and swipes wrap; a set of one has none
assert.deepStrictEqual(["ArrowRight", "ArrowLeft", "Home", "End", "Enter"].map(function (k) { return P.keyTo(k, 0, 4); }), [1, 3, 0, 3, -1]);
assert.strictEqual(P.keyTo("ArrowRight", 3, 4), 0, "wraps at the end");
assert.strictEqual(P.keyTo("ArrowRight", 0, 1), -1, "a set of one does not cycle");
assert.deepStrictEqual([P.wrap(-1, 4), P.wrap(4, 4), P.wrap(2, 4), P.wrap(3, 0)], [3, 0, 2, 0]);
assert.deepStrictEqual([P.swipeTo(-80, 10), P.swipeTo(80, -10), P.swipeTo(-30, 0), P.swipeTo(-60, 70)], [1, -1, 0, 0]);
assert.strictEqual(P.counter(2, 7), "3 of 7");
// the address: ?pic=<path>, one panel at a time, read back by the host
assert.strictEqual(W.withPanel("?bead=gd-1&at=yard", "pic", "/image/a b.png"), "?at=yard&pic=%2Fimage%2Fa%20b.png");
assert.deepStrictEqual(W.panelOf("?at=yard&pic=%2Fimage%2Fa%20b.png"), { kind: "pic", id: "/image/a b.png" });
`
	js, err := filepath.Abs(filepath.Join("web", "pictures.js"))
	if err != nil {
		t.Fatal(err)
	}
	wb, err := filepath.Abs(filepath.Join("web", "workbench.js"))
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, "-e", script, js, wb).CombinedOutput(); err != nil {
		t.Fatalf("node pictures logic test failed: %v\n%s", err, out)
	}
}

// A grade picture's preview and the picture viewer in a real Chromium (WB-18): hover and focus raise the preview inside the
// viewport, a click opens ?pic= on the picture's set, the keys, buttons, filmstrip and a swipe cycle it, Back returns and a
// reload shows the picture alone. What is asserted lives in testdata/pictures_browser.js.
func TestPicturesUnderChromium(t *testing.T) {
	underChromium(t, "pictures_browser.js", func() *httptest.Server { return cardWorld(t, time.Now) })
}
