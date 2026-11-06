package gardenhub

import (
	"bytes"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gardenio/controller/internal/heraldry"
)

// The World's arms picture (heraldry spec §6.4, step 4.1): /garden/arms.png answers the inputs web/heraldry.js puts in its
// query with the renderer's PNG at exactly that device height (and the row's width), by day and by night, the content
// address as its ETag, a 304 for it, a day's private cache; a form, height or theme the renderer cannot draw is a 400.
// The six-token form a bead stored before the run draws the same row as its run.
func TestArmsRoute(t *testing.T) {
	mux := http.NewServeMux()
	(&Handler{}).Register(mux)
	get := func(q, etag string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/garden/arms.png?"+q, nil)
		if etag != "" {
			r.Header.Set("If-None-Match", etag)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	const arms = "🧭🔥📦"
	a, err := heraldry.Parse(arms)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		arms  string
		h     int
		theme string
		th    heraldry.Theme
	}{{arms, 24, "day", heraldry.Day}, {arms, 48, "day", heraldry.Day}, {arms, 72, "night", heraldry.Night}, {"blue 🧭 . 🔥 . 📦", 36, "", heraldry.Day}} {
		q := url.Values{"arms": {c.arms}, "h": {strconv.Itoa(c.h)}}
		if c.theme != "" {
			q.Set("theme", c.theme)
		}
		res := get(q.Encode(), "")
		if res.Code != http.StatusOK {
			t.Fatalf("h=%d theme=%q: %d %s", c.h, c.theme, res.Code, res.Body)
		}
		if ct := res.Header().Get("Content-Type"); ct != "image/png" {
			t.Errorf("h=%d: Content-Type %q", c.h, ct)
		}
		if cc := res.Header().Get("Cache-Control"); cc != "private, max-age=86400" {
			t.Errorf("h=%d: Cache-Control %q", c.h, cc)
		}
		want := `"` + heraldry.Key(a, heraldry.FormatPNG, c.h, c.th) + `"`
		if et := res.Header().Get("ETag"); et != want {
			t.Errorf("h=%d theme=%q: ETag %s, want %s", c.h, c.theme, et, want)
		}
		img, err := png.Decode(bytes.NewReader(res.Body.Bytes()))
		if err != nil {
			t.Fatalf("h=%d: %v", c.h, err)
		}
		if b := img.Bounds(); b.Dx() != heraldry.Width(3, c.h) || b.Dy() != c.h {
			t.Errorf("h=%d: bitmap %dx%d, want %dx%d", c.h, b.Dx(), b.Dy(), heraldry.Width(3, c.h), c.h)
		}
		if res := get(q.Encode(), want); res.Code != http.StatusNotModified || res.Body.Len() != 0 {
			t.Errorf("h=%d: If-None-Match its ETag answered %d with %d bytes, want an empty 304", c.h, res.Code, res.Body.Len())
		}
	}
	for _, q := range []string{
		url.Values{"arms": {arms}}.Encode(),                                 // no height
		url.Values{"arms": {arms}, "w": {"24"}}.Encode(),                    // a width, which the row is not keyed by
		url.Values{"arms": {arms}, "h": {"7"}}.Encode(),                     // under the renderer's range
		url.Values{"arms": {arms}, "h": {"257"}}.Encode(),                   // over it
		url.Values{"arms": {arms}, "h": {"24"}, "theme": {"dusk"}}.Encode(), // no such theme
		url.Values{"arms": {""}, "h": {"24"}}.Encode(),                      // no arms
		url.Values{"arms": {"plaid 🧭 . . . ."}, "h": {"24"}}.Encode(),       // not a stored form
		url.Values{"arms": {"🧭🔥📦🔑🌠🐐"}, "h": {"24"}}.Encode(),                // six emoji
	} {
		if res := get(q, ""); res.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", q, res.Code)
		}
	}
}

// heraldry.js names the picture the route draws, at the height the renderer would choose for the slot and ratio
// (heraldry.DeviceHeight); picture() fetches each arms, height and theme once and hands
// back the bitmap only once it has loaded; ART.drawArms blits it 1:1 on whole device px under the world's transform and puts
// the transform back. Skipped when node is not installed.
func TestHeraldryJSUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	var heights []string
	for _, d := range []float64{0, 1, 1.25, 1.5, 2, 2.625, 3} {
		heights = append(heights, strconv.Itoa(heraldry.DeviceHeight(24, d)))
	}
	script := `
var H = require(process.argv[1]), ART = require(process.argv[2]), assert = require("assert");
// the height the renderer would pick (heraldry.DeviceHeight)
assert.deepStrictEqual([0, 1, 1.25, 1.5, 2, 2.625, 3].map(function (d) { return H.deviceHeight(24, d); }), [` + strings.Join(heights, ", ") + `]);
assert.strictEqual(H.deviceHeight(4, 1), ` + strconv.Itoa(heraldry.MinHeight) + `); assert.strictEqual(H.deviceHeight(400, 3), ` + strconv.Itoa(heraldry.MaxHeight) + `);   // inside the renderer's range
assert.strictEqual(H.SLOT, 24);
assert.strictEqual(H.src("/bv", "🧭🔥📦", 48, true), "/bv/garden/arms.png?arms=%F0%9F%A7%AD%F0%9F%94%A5%F0%9F%93%A6&h=48&theme=night");
assert.strictEqual(H.src("", "sand . . 🛡️ . .", 24, false), "/garden/arms.png?arms=sand%20.%20.%20%F0%9F%9B%A1%EF%B8%8F%20.%20.&h=24&theme=day");
// no DOM: nothing to fetch, nothing drawn
assert.strictEqual(H.picture("blue 🧭 . . . .", 24, 2, false), null);
// a DOM: one Image per arms, height and theme; null until it loads, then the same element; a repaint asked once per arrival
var made = [];
global.Image = function () { made.push(this); this.naturalWidth = 0; this.naturalHeight = 0; };
var repaints = 0;
H.init("/bv", function () { repaints++; });
assert.strictEqual(H.picture("", 24, 2, false), null);
assert.strictEqual(made.length, 0);
assert.strictEqual(H.picture("blue 🧭 . . . .", 24, 2, false), null);
assert.strictEqual(H.picture("blue 🧭 . . . .", 24, 2, false), null);
assert.strictEqual(made.length, 1);
assert.strictEqual(made[0].src, "/bv/garden/arms.png?arms=blue%20%F0%9F%A7%AD%20.%20.%20.%20.&h=48&theme=day");
made[0].naturalWidth = 156; made[0].naturalHeight = 48; made[0].onload();
assert.strictEqual(repaints, 1);
assert.strictEqual(H.picture("blue 🧭 . . . .", 24, 2, false), made[0]);
assert.strictEqual(H.picture("blue 🧭 . . . .", 24, 2, true), null);    // night is its own picture
assert.strictEqual(H.picture("blue 🧭 . . . .", 24, 3, false), null);   // and so is another density
assert.strictEqual(made.length, 3);
assert.strictEqual(made[2].src.slice(-15), "&h=72&theme=day");
made[1].onerror();                                                      // refused: never drawn, never asked again
assert.strictEqual(H.picture("blue 🧭 . . . .", 24, 2, true), null);
assert.strictEqual(made.length, 3);
assert.strictEqual(repaints, 1);
// drawArms: whole device px under the world's transform, 1:1, the transform put back
var calls = [], m = { a: 2.5, b: 0, c: 0, d: 2.5, e: 100.3, f: -40.6 }, cur = m;
var ctx = { getTransform: function () { return cur; }, setTransform: function (a, b, c, d, e, f) { cur = { a: a, b: b, c: c, d: d, e: e, f: f }; calls.push(["t", a, e, f]); },
  drawImage: function (img, x, y, w, h) { calls.push(["i", cur.a, x, y, w, h]); } };
assert.deepStrictEqual(ART.drawArms(ctx, made[0], 160.1, 32), { x: 501, y: 39, w: 156, h: 48 });
assert.deepStrictEqual(calls, [["t", 1, 0, 0], ["i", 1, 501, 39, 156, 48], ["t", 2.5, 100.3, -40.6]]);
assert.deepStrictEqual(cur, m);
// no transform to read (a node canvas mock): world px are device px
var plain = []; assert.deepStrictEqual(ART.drawArms({ drawImage: function (i, x, y, w, h) { plain.push([x, y, w, h]); } }, made[0], 3.4, 5.6), { x: 3, y: 6, w: 156, h: 48 });
assert.deepStrictEqual(plain, [[3, 6, 156, 48]]);
// nothing yet: nothing drawn
assert.strictEqual(ART.drawArms(ctx, null, 0, 0), null);
assert.strictEqual(ART.drawArms(ctx, { naturalWidth: 0 }, 0, 0), null);
// the plot gate (4.3): left on the first bed's column, the foot one screen px above the bed at any zoom and ratio
[[0.45, 1], [0.45, 2], [1, 1], [1, 1.5], [2, 2], [4, 3]].forEach(function (c) {
  var z = c[0], d = c[1], g = H.gate(32, 64, z, d), cssH = H.deviceHeight(24, d) / d;
  assert.strictEqual(g.x, 32);
  assert.ok(Math.abs((64 - g.y) * z - (cssH + 1)) < 1e-9, "foot at z " + z + " dpr " + d);
  assert.ok(g.y >= 0, "inside the 2-tile gap over a plot at world y 32 at z " + z);   // the bed sits 2 tiles in, the next plot 2 tiles up
});
// drawPlot: a named plan's picture at its gate, with the bed offset; nothing for a plan without arms or with arms the farm
// could not name
var drawn = [], draw = function (c, img, x, y) { drawn.push([img, x, y]); return { x: x, y: y }; }, plot = { x: 10, y: 20, plan: { heraldry: "blue 🧭 . . . .", heraldry_name: "arms: compass" } };
H.drawPlot(null, draw, plot, 16, 2, 1, 2, false);
assert.deepStrictEqual(drawn, [[made[0], 192, H.gate(192, 352, 1, 2).y]]);
H.drawPlot(null, draw, { x: 0, y: 0, plan: {} }, 16, 2, 1, 2, false);
H.drawPlot(null, draw, { x: 0, y: 0, plan: { heraldry: "blue 🌱 . . . ." } }, 16, 2, 1, 2, false);
assert.strictEqual(drawn.length, 1);
assert.strictEqual(made.length, 3);   // and fetched nothing new
// hotAria: the plot's and its sign's say the plan's arms, a phase bed's (its stake's) the phase's; tasks and the rest none
assert.strictEqual(H.hotAria({ kind: "plot", plot: plot }), " Arms: compass.");
assert.strictEqual(H.hotAria({ kind: "plotsign", plot: plot }), " Arms: compass.");
assert.strictEqual(H.hotAria({ kind: "bed", node: { ref: { arms: "arms: fire, compass, package" } } }), " Arms: fire, compass, package.");
assert.strictEqual(H.hotAria({ kind: "bed", node: { ref: { arms: "" } } }), "");
assert.strictEqual(H.hotAria({ kind: "task", task: { heraldry: "red . . 🔑 . ." } }), "");
assert.strictEqual(H.hotAria({ kind: "biome" }), "");
// the words a hot adds for arms
assert.strictEqual(H.aria("arms: fire, compass, package"), " Arms: fire, compass, package.");
assert.strictEqual(H.aria(""), "");
assert.strictEqual(H.aria(undefined), "");
console.log("ok");
`
	her, err := filepath.Abs(filepath.Join("web", "heraldry.js"))
	if err != nil {
		t.Fatal(err)
	}
	art, err := filepath.Abs(filepath.Join("web", "art.js"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "-e", script, her, art).CombinedOutput()
	if err != nil {
		t.Fatalf("heraldry.js under node: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "ok") {
		t.Fatalf("heraldry.js under node printed %q", out)
	}
}

// The gardener's badge (5.9) draws in world px under the world's transform: at the Bead depth on a dpr-2 screen (z 2) the
// picture is centred 8 world px right of the figure's x, its foot 2 world px over the head, whatever z and dpr scale it by
// (the barn's badge once drew under the identity transform with device px and sat on the head, gd-QboM9sPYCg).
func TestHeraldryBadgeUnderNode(t *testing.T) {
	script := `
var path = require("path"), H = require(process.argv[1]), ART = require(path.join(path.dirname(process.argv[1]), "art.js")), assert = require("assert");
var made = [];
global.Image = function () { made.push(this); this.naturalWidth = 0; this.naturalHeight = 0; };
H.init("/bv", function () {});
var cur = { a: 1, b: 0, c: 0, d: 1, e: 0, f: 0 }, m;
var ctx = { getTransform: function () { return cur; }, setTransform: function (a, b, c, d, e, f) { cur = { a: a, b: b, c: c, d: d, e: e, f: f }; }, drawImage: function () {} };
assert.strictEqual(H.badge(ctx, ART.drawArms, "red 🔑 . . . .", 50, 30, 2, 2, false, false), null);   // asks for the picture
var bimg = made[made.length - 1]; assert.strictEqual(bimg.src.slice(-15), "&h=32&theme=day");
bimg.naturalWidth = 104; bimg.naturalHeight = 32; bimg.onload();
cur = m = { a: 4, b: 0, c: 0, d: 4, e: -120, f: 60 };
var bb = H.badge(ctx, ART.drawArms, "red 🔑 . . . .", 50, 30, 2, 2, false, false);
assert.deepStrictEqual(bb, { x: Math.round(4 * 58 - 120 - 52), y: 4 * 28 + 60 - 32, w: 104, h: 32 });
assert.deepStrictEqual(cur, m);
// gd-M0IjVGTzSP: a figure in the path column (x 0, row 2) under its bed's sign (tile 0,0) and beside a bed (from x 16, y 16):
// over the head the badge would cover the sign's foot and the bed's corner, so it drops 3 under the sign and goes 6 left of
// the bed, its foot on the hat's rim. With nothing to shun it keeps its place, and each badge drawn joins keep.
cur = { a: 1, b: 0, c: 0, d: 1, e: 0, f: 0 };
var sz = function (c, img, x, y) { return { x: x, y: y, w: img.naturalWidth, h: img.naturalHeight }; };
assert.strictEqual(H.badge(ctx, sz, "🔑🔥📦", 0, 24, 1, 1, false, false), null);
var simg = made[made.length - 1]; simg.naturalWidth = 23; simg.naturalHeight = 8; simg.onload();
var keep = [];
H.shun(ctx, keep, 14, 14, 148, 148, 1); H.shun(ctx, keep, 1, 0, 14, 16, 2); H.shun(ctx, keep, 1, 26, 14, 22, 1);
assert.deepStrictEqual(H.badge(ctx, sz, "🔑🔥📦", 0, 24, 1, 1, false, false, keep), { x: -9.5, y: 16, w: 23, h: 8 });
assert.deepStrictEqual(keep[3], { x: -9.5, y: 16, w: 23, h: 8, c: 2 });
assert.deepStrictEqual(H.badge(ctx, sz, "🔑🔥📦", 200, 24, 1, 1, false, false, []), { x: 196.5, y: 14, w: 23, h: 8 });
`
	runNodeLogic(t, "heraldry.js", script)
}

// gateRect is the screen box drawPlot's picture covers, which the beacon arrows slide off (gd-rA31ZIfHdv): the gate's foot
// through the camera, the picture's own CSS size at any zoom, and none while there is no picture or for unnamed arms.
func TestHeraldryGateRectUnderNode(t *testing.T) {
	script := `
var H = require(process.argv[1]), assert = require("assert");
var made = [];
global.Image = function () { made.push(this); this.naturalWidth = 0; this.naturalHeight = 0; };
H.init("/bv", function () {});
var plot = { x: 10, y: 20, plan: { heraldry: "blue 🧭 . . . .", heraldry_name: "arms: compass" } }, vp = { w: 400, h: 300, dpr: 2 };
assert.strictEqual(H.gateRect(plot, 16, 2, { cx: 192, cy: 352, z: 1 }, vp, false), null);   // asks for the picture
made[0].naturalWidth = 156; made[0].naturalHeight = 48; made[0].onload();
assert.deepStrictEqual(H.gateRect(plot, 16, 2, { cx: 192, cy: 352, z: 1 }, vp, false), { x: 200, y: 125, w: 78, h: 24 });
assert.deepStrictEqual(H.gateRect(plot, 16, 2, { cx: 192, cy: 352, z: 0.5 }, vp, false), { x: 200, y: 125, w: 78, h: 24 });
assert.strictEqual(H.gateRect(plot, 16, 2, { cx: 192, cy: 352, z: 1 }, vp, true), null);   // night's has not loaded
assert.strictEqual(H.gateRect({ x: 0, y: 0, plan: { heraldry: "blue 🌱 . . . ." } }, 16, 2, { cx: 0, cy: 0, z: 1 }, vp, false), null);
assert.strictEqual(made.length, 2);
`
	runNodeLogic(t, "heraldry.js", script)
}

// The page loads heraldry.js after art.js and before garden.js, which wires its repaint and base path once.
func TestHeraldryJSWired(t *testing.T) {
	art, her, app := strings.Index(pageHTML, `{{asset "art.js"}}`), strings.Index(pageHTML, `{{asset "heraldry.js"}}`), strings.Index(pageHTML, `{{asset "garden.js"}}`)
	if her < 0 || her < art || app < her {
		t.Errorf("garden.html script order: art.js %d, heraldry.js %d, garden.js %d; want art.js, heraldry.js, garden.js", art, her, app)
	}
	// a picture arriving repaints the map and lays the beacons out again, which slide off the cursor plot's arms (gd-rA31ZIfHdv)
	if !strings.Contains(pageAppJS, "HERALDRY.init(base,function(){queueRender();syncBeacons();})") {
		t.Error("garden.js does not wire HERALDRY.init(base,function(){queueRender();syncBeacons();})")
	}
	// step 4.3: the plot pass hangs the picture at the gate from the Plan depth in, and the plot's and the stake's hots say the
	// arms in words; no stake, task cell or stamp draws a picture (Q16, R6). Below the Plan level only the plot the cursor is
	// inside keeps its arms (CURSOR.gatePlot), drawn after the plot pass, so a short map that frames a plan at Project art
	// still shows the arms its label stands clear of (gd-rA31ZIfHdv)
	for _, want := range []string{"function drawGate(p,gates){var gb=HERALDRY.drawPlot(ctx,ART.drawArms,p,T,WORLD.BED.off[0],cam.z,vp.dpr,night);if(gb)gates.push(gb);}", "if(level>=2)drawGate(p,gates);", "if(level<2&&op.gate)vis.plots.forEach(function(p){if(p.id===op.gate)drawGate(p,gates);});", "gate:gatePlotId()", "h.tipTitle+': '+h.tipText,HERALDRY.hotAria(h)+"} {
		if !strings.Contains(pageAppJS+pageSceneJS, want) {
			t.Errorf("garden.js and scene.js do not carry %q", want)
		}
	}
	// step 5.9: two more pictures, each a gardener's badge (HERALDRY.badge) at the Plan depth and in, over the figures on
	// the plots (scene.js, through ART.drawPlotGardeners) and over those in each strip's Growing field (scene.js drawStrip, 28.12); still no stake,
	// task cell or stamp
	if n := strings.Count(pageAppJS+pageSceneJS, "ART.drawArms"); n != 3 {
		t.Errorf("garden.js and scene.js draw arms in %d places, want 3 (the plot gate and the gardeners' badges on plots and in the field)", n)
	}
	for _, want := range []string{"HERALDRY.badge(ctx,ART.drawArms,a.canonical,x,y,cam.z,vp.dpr,night,lift,gates)", "var gr=level>=2&&l.fill?l.fill.growing:null;", "HERALDRY.badge(ctx,ART.drawArms,a.canonical,(h.x+h.w-1)*T,h.y*T+4,cam.z,vp.dpr,night,false);"} {
		if !strings.Contains(pageAppJS+pageSceneJS, want) {
			t.Errorf("garden.js and scene.js do not carry %q", want)
		}
	}
	mux := http.NewServeMux()
	(&Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v"}).Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/garden", nil))
	if !strings.Contains(inlined(t, rec.Body.String()), "var HERALDRY = (function () {") {
		t.Error("the served page does not carry heraldry.js")
	}
}

// armsName is the farm's plain-words name for a plan's or a phase's arms (step 4.3, spec §8): the renderer's PlainName, and ""
// for none and for any form the renderer refuses, so the canvas neither names nor fetches what it cannot draw.
func TestArmsName(t *testing.T) {
	for in, want := range map[string]string{
		"🧭🔥📦":            "arms: compass, fire, package",
		"blue 🧭 . 🔥 . 📦": "arms: compass, fire, package", // the six-token form a bead stored before the run
		"":               "",
		"blue 🌱 . . . .": "", // not in the library
		"blue . . . . .": "", // holds no emoji
		"blue":           "", // not a stored form
	} {
		if got := armsName(in); got != want {
			t.Errorf("armsName(%q) = %q, want %q", in, got, want)
		}
	}
}
