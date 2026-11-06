package gardenhub

import (
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// neighbours.js (gd-MFcv2hSfue) keeps whose-plan logic pure so it runs without a browser: the operator -> style mapping is
// deterministic, distinct operators get distinct colours (up to 8) and patterns (up to 4), the fence geometry stays inside the
// plot, and "Mine" lights the viewer's own plots and regions. Skipped when node is not installed.
func TestNeighboursLogicUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `
var N = require(process.argv[1]), assert = require("assert");
function plan(id, op, owner) { return { id: id, operator: op, owner: owner }; }
// whose, nameOf, mine
assert.strictEqual(N.whose(plan("a", "OIDC:Matt@Example.com")), "oidc:matt@example.com", "lowercased alias");
assert.strictEqual(N.whose(plan("a", "", "Flo")), "flo", "the owner when no operator");
assert.strictEqual(N.whose(plan("a")), "");
assert.strictEqual(N.whose(null), "");
assert.strictEqual(N.nameOf("oidc:matthewbaker1110@gmail.com"), "matthewbaker1110", "no scheme, the email's local part");
assert.strictEqual(N.nameOf("flo"), "flo");
assert.strictEqual(N.nameOf(""), "nobody");
assert.ok(N.mine(plan("a", "oidc:me@example.com"), "oidc:me@example.com"));
assert.ok(N.mine(plan("a", "OIDC:Me@Example.com"), "oidc:me@example.com"), "case does not matter");
assert.ok(!N.mine(plan("a", "oidc:me@example.com"), ""), "nobody signed in owns nothing");
assert.ok(!N.mine(plan("a", ""), ""), "an unowned plan is not the anonymous viewer's");
assert.ok(!N.mine(plan("a", "oidc:flo@example.com"), "oidc:me@example.com"));
// assign: deterministic, order-free, one style per alias, seeds count too
var ops = ["oidc:a@x", "oidc:b@x", "oidc:c@x", "oidc:d@x"];
var doc = { plans: ops.map(function (o, i) { return plan("p" + i, o); }).concat([plan("dup", "OIDC:A@X"), plan("none", "")]), seeds: [plan("s1", "oidc:seedy@x")] };
var s1 = N.assign(doc), s2 = N.assign({ plans: doc.plans.slice().reverse(), seeds: doc.seeds });
assert.deepStrictEqual(s1, s2, "shuffled plans, same styles");
assert.deepStrictEqual(Object.keys(s1).sort(), ops.concat(["oidc:seedy@x"]).sort(), "one style per operator, seeds included, no empty alias");
var cs = {}, ps = {};
Object.keys(s1).forEach(function (k) { cs[s1[k].colour] = 1; ps[s1[k].pattern] = 1; assert.strictEqual(s1[k].name, N.nameOf(k)); });
assert.strictEqual(Object.keys(cs).length, 5, "five operators, five colours");
assert.strictEqual(Object.keys(ps).length, 4, "every pattern used before one repeats");
// a lone operator keeps its hashed colour and pattern whoever else is on the farm later... as long as nobody took them
var lone = N.assign({ plans: [plan("x", "oidc:a@x")] })["oidc:a@x"], h = N.h32("oidc:a@x");
assert.strictEqual(lone.colour, N.COLOURS[h % 8].c);
assert.strictEqual(lone.pattern, N.PATTERNS[(h >>> 8) % 4]);
// past 8 operators colours repeat, never the colour-and-pattern pair
var many = []; for (var i = 0; i < 20; i++) many.push(plan("m" + i, "oidc:op" + i + "@x"));
var sm = N.assign({ plans: many }), pairs = {};
Object.keys(sm).forEach(function (k) { var p = sm[k].colour + sm[k].pattern; assert.ok(!pairs[p], "pair repeats: " + p); pairs[p] = 1; });
assert.deepStrictEqual(N.assign(null), {});
// styleOf, describe, legendRows
assert.strictEqual(N.styleOf(s1, plan("q", "OIDC:B@X")), s1["oidc:b@x"]);
assert.strictEqual(N.styleOf(s1, plan("q", "")), null);
assert.strictEqual(N.styleOf(s1, plan("q", "oidc:stranger@x")), null);
var sb = s1["oidc:b@x"];
assert.strictEqual(N.describe(sb, "oidc:b@x"), "b (you), " + sb.colourName + " " + sb.pattern + " fence", "identity in words: name, colour and pattern");
assert.strictEqual(N.describe(sb, ""), "b, " + sb.colourName + " " + sb.pattern + " fence");
assert.strictEqual(N.describe(null, ""), "No operator, plain fence");
assert.deepStrictEqual(N.legendRows(s1, "oidc:c@x").map(function (r) { return r.name; }), ["c", "a", "b", "d", "seedy"], "yours first, then by name");
// geometry: every painted run sits inside the plot, on its edge band, and the double pattern is two lines a side
var r = { x: 16, y: 32, w: 256, h: 192 };
N.PATTERNS.forEach(function (pat) {
  [[0.2, 16], [0.7, 16], [2, 16]].forEach(function (zt) {
    var b = N.bandFor(zt[0], zt[1]), rs = N.ringRects(r, b.t, pat, b.u);
    assert.ok(rs.length >= 4, pat + " paints every side");
    rs.forEach(function (a) {
      assert.ok(a[2] > 0 && a[3] > 0, pat + " empty run");
      assert.ok(a[0] >= r.x - 1e-9 && a[1] >= r.y - 1e-9 && a[0] + a[2] <= r.x + r.w + 1e-9 && a[1] + a[3] <= r.y + r.h + 1e-9, pat + " outside the plot: " + a);
      var onEdge = a[0] < r.x + b.t + 1e-9 || a[1] < r.y + b.t + 1e-9 || a[0] + a[2] > r.x + r.w - b.t - 1e-9 || a[1] + a[3] > r.y + r.h - b.t - 1e-9;
      assert.ok(onEdge, pat + " run off the fence band: " + a);
    });
  });
});
assert.strictEqual(N.ringRects(r, 4, "solid", 8).length, 4);
assert.strictEqual(N.ringRects(r, 6, "double", 8).length, 8);
assert.ok(N.ringRects(r, 4, "dotted", 8).length > N.ringRects(r, 4, "dashed", 8).length, "dots are shorter than dashes");
// the band is at least 4 screen px (so a 30 px plot shows it) and a quarter tile
assert.strictEqual(N.bandFor(0.2, 16).t, 20);
assert.strictEqual(N.bandFor(2, 16).t, 4);
// drawing: an unowned plot paints nothing; an owned one a dark band then its colour, fillRect only
function Ctx() { this.calls = []; this.fillStyle = ""; }
Ctx.prototype.fillRect = function (x, y, w, h) { this.calls.push([this.fillStyle, x, y, w, h]); };
var c = new Ctx(); N.fence(c, r, null, 1, 16); assert.strictEqual(c.calls.length, 0);
c = new Ctx(); N.fence(c, r, sb, 1, 16);
assert.ok(c.calls.length > 4 && c.calls[0][0] !== sb.colour && c.calls[c.calls.length - 1][0] === sb.colour);
c = new Ctx(); N.dim(c, r); assert.deepStrictEqual(c.calls.length, 1);
// a region is the viewer's when it holds one of their plots or seeds
var me = "oidc:me@x";
assert.ok(N.biomeMine({ plots: [{ plan: plan("a", "oidc:flo@x") }, { plan: plan("b", me) }], nursery: [] }, me));
assert.ok(N.biomeMine({ plots: [], nursery: [{ seed: plan("s", me) }, { plan: plan("t", me) }] }, me));
assert.ok(!N.biomeMine({ plots: [{ plan: plan("a", "oidc:flo@x") }], nursery: [] }, me));
assert.ok(!N.biomeMine({}, me));
`
	src, err := filepath.Abs(filepath.Join("web", "neighbours.js"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "-e", script, src).CombinedOutput()
	if err != nil {
		t.Fatalf("node neighbours logic test failed: %v\n%s", err, out)
	}
}

// The page wires neighbours.js before garden.js, and garden.js hooks it in at the farm document (styles and legend), the
// plot draw (fence, then the rollup stamp at the project depth over it, then the Mine dimming) and the plot card (the operator
// in words).
func TestNeighboursWiredIntoGarden(t *testing.T) {
	_, rec := servedPage(t)
	body := inlined(t, rec.Body.String())
	i, j := strings.Index(body, "var NEIGHBOURS ="), strings.Index(body, "var B=document.body")
	if i < 0 || j < 0 || i > j {
		t.Fatalf("neighbours.js must load before garden.js (at %d, garden.js at %d)", i, j)
	}
	for _, want := range []string{
		"farmDoc=doc;nbStyles=NB.assign(doc);", "function drawDoc(prev){\n  NB.legend(nbStyles,me);", // the legend once the veil lifts (gd-N5BigisFtS)
		"NB.fence(ctx,pr,NB.styleOf(nbStyles,p.plan),cam.z,T);if(level>=2)drawGate(p,gates);if(ro)drawRollup(p,ro);if(mine&&!NB.mine(p.plan,mine))NB.dim(ctx,pr);", // the arms hang on the fence and dim with the plot (heraldry 4.3)
		"if(level===0&&mine&&!NB.biomeMine(b,mine))NB.dim(ctx,WORLD.tiles(b.foot));",
		"mine:lit()&&me,", // Mine dims the plots, in scene.js
		"' Operator: '+NB.describe(NB.styleOf(nbStyles,pl),me)",
		"function lit(){return scope==='mine'&&!!me;}",
	} {
		if !strings.Contains(pageAppJS+pageSceneJS, want) {
			t.Errorf("garden.js and scene.js missing %s", want)
		}
	}
}

// Operator fences and Mine in a real browser (gd-MFcv2hSfue): the card fixture farm with four operators (matt, the viewer,
// on plan A, plans B-D and two music plans; flo on kbeads; mo on coop; sam on three music plans) and one plan that names only
// an owner (ivy). What is asserted lives in testdata/neighbours_browser.js; GARDENHUB_SHOTS=<dir> saves its screenshots.
// The farm and the browser both run at goldenNow (gd-Klc6pDIOcb): on the wall clock the fixture's beads age, and from
// baseUnix + 14 days the gardenio project is dormant and its fog washes matt's fence out of its colour.
func TestNeighboursUnderChromium(t *testing.T) {
	underChromium(t, "neighbours_browser.js", func() *httptest.Server {
		return cardWorldAs(t, func() time.Time { return goldenNow }, "oidc:matt@example.com", func(src *fakeFarmSrc) {
			for _, b := range src.beads {
				switch {
				case strings.HasPrefix(b.ID, "gd-kbeads"):
					b.OperatedBy = "oidc:flo@example.com"
				case strings.HasPrefix(b.ID, "gd-coop"):
					b.OperatedBy = "oidc:mo@example.com"
				case b.ID == "gd-musica" || b.ID == "gd-musicc" || b.ID == "gd-musice":
					b.OperatedBy = "oidc:sam@example.com"
				case b.ID == "gd-planP":
					b.OperatedBy, b.Owner = "", "ivy"
				}
			}
		})
	}, strconv.FormatInt(goldenNow.UnixMilli(), 10))
}
