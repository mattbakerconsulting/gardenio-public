package gardenhub

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// The Snap button's pure half (web/snap.js) under node: the context a snapshot carries and its file's name. Skipped when
// node is not installed.
func TestSnapLogic(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `
var S = require(process.argv[1]), assert = require("assert");
// The Snap button's context (gd-GbF2JS04h5): what the view frames, one entry per thing, nearest first, capped; the view link.
var items = [{ kind: "sign", id: "gardenio", title: "gardenio" }, { kind: "biome", id: "gardenio", title: "gardenio" }, { kind: "biome", id: "~", title: "The wild" },
  { kind: "plot", id: "gd-p1", title: "Plan one" }, { kind: "plotsign", id: "gd-p1", title: "Plan one" }, { kind: "mail", id: "" }, { kind: "barn" },
  { kind: "task", id: "gd-t1", title: "Dig" }, { kind: "seed", id: "gd-s1", title: "Seed" }, { kind: "tree", id: "gd-p0", title: "Old plan" }, { kind: "bead", id: "gd-y1", title: "Loose bead" }];
for (var i = 0; i < 40; i++) items.push({ kind: "task", id: "gd-x" + i, title: "x" });
var c = S.snapContext({ path: "/v/garden", search: "?bead=gd-b1&at=old&z=0", at: "gardenio/gd-p1", level: 2, who: "mine", night: true, items: items, vp: { w: 1280.4, h: 800 }, now: new Date(Date.UTC(2026, 9, 19, 23, 40, 5, 678)) });
assert.deepStrictEqual(Object.keys(c), ["kind", "url", "at", "z", "who", "night", "framed", "vp", "taken_at"]);
assert.strictEqual(c.url, "/v/garden?bead=gd-b1&at=gardenio/gd-p1&z=2");
assert.strictEqual(c.kind, "farm"); assert.strictEqual(c.z, 2); assert.strictEqual(c.who, "mine"); assert.strictEqual(c.night, true);
assert.deepStrictEqual(c.vp, [1280, 800]); assert.strictEqual(c.taken_at, "2026-10-19T23:40:05Z");
assert.strictEqual(c.framed.length, 24, "capped at 24");
assert.deepStrictEqual(c.framed.slice(0, 7), [{ kind: "project", id: "gardenio", title: "gardenio" }, { kind: "biome", id: "~", title: "The wild" },
  { kind: "plan", id: "gd-p1", title: "Plan one" }, { kind: "bead", id: "gd-t1", title: "Dig" }, { kind: "bead", id: "gd-s1", title: "Seed" },
  { kind: "plan", id: "gd-p0", title: "Old plan" }, { kind: "bead", id: "gd-y1", title: "Loose bead" }]);
var w = S.snapContext({ path: "/garden", search: "", at: "", level: null, who: "all", night: false, items: [], vp: { w: 390, h: 700 }, now: new Date(0) });
assert.strictEqual(w.url, "/garden"); assert.strictEqual(w.z, undefined, "over the wild there is no level"); assert.deepStrictEqual(w.framed, []);
// a field stand (28.12) frames its gardener's bead, in its line's words, where the barn's figures were; a strip's bead is itself
var model = { items: { "field:gd-rae": { id: "field:gd-rae", kind: "stand", g: { id: "gd-rae" }, title: "rae", line: "rae — working: Dig" }, "gd-y1": { id: "gd-y1", title: "Loose bead" } } };
assert.deepStrictEqual([S.snapItem({ kind: "bead", id: "field:gd-rae" }, null, model), S.snapItem({ kind: "bead", id: "gd-y1" }, null, model)],
  [{ kind: "bead", id: "gd-rae", title: "rae — working: Dig" }, { kind: "bead", id: "gd-y1", title: "Loose bead" }]);
assert.strictEqual(S.snapName("gardenio", "gardenio/gd-p1", new Date(2026, 9, 19, 9, 5)), "farm-gardenio-gardenio-gd-p1-20261019-0905.png");
assert.strictEqual(S.snapName("my land", "", new Date(2026, 0, 2, 23, 59)), "farm-my-land-map-20260102-2359.png");
assert.strictEqual(S.snapName("gardenio", "~/gd-m%2F1", new Date(2026, 0, 2, 3, 4)), "farm-gardenio-gd-m-1-20260102-0304.png");
`
	js, err := filepath.Abs(filepath.Join("web", "snap.js"))
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, "-e", script, js).CombinedOutput(); err != nil {
		t.Fatalf("node snap logic test failed: %v\n%s", err, out)
	}
}
