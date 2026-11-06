package gardenhub

import (
	"strings"
	"testing"
)

// The plot rollup at the project depth (cursor-grid spec gardenio/docs/superpowers/specs/2026-10-20-lands-cursor-grid-design.md
// section 6.6, owner decision gd-zBRZ3rUfWS) is pure, so its contract runs under node (testdata/rollup_test.js):
// WORLD.plotRollup's frame rules (current beats closed beats blocked-and-nothing-moving beats review), fill, marks, the Other
// bed, the ninth-position crate, the empty plan's bar and the gardeners; and ART.drawPlotRollup / drawRollupMarks at 30, 48,
// 66, 99 and 132 px (the 36 px floor, the beds under the stamp from 90 px, the marks riding the fence).
func TestPlotRollup(t *testing.T) {
	r := nodeScript(t, "rollup_test.js")
	t.Logf("fixture plots %v", r["fixturePlots"])
}

// The page draws the stamp in place of the flat plate at the project depth, and a plot's "!N" is the stamp's need mark, not a
// second glyph stacked on it (gd-dmYYfaQYuA).
func TestPlotRollupWired(t *testing.T) {
	for _, want := range []string{"ART.drawRollupPlate(ctx,pal,", "ART.drawRollupStamp(x,ART.palette(night),", "ART.drawRollupMarks(ctx,pal,", "WORLD.plotRollup(p,", "if(level<=1&&s.plot)return;"} {
		if !strings.Contains(pageAppJS+pageSceneJS, want) {
			t.Errorf("garden.js and scene.js do not contain %q", want)
		}
	}
}

// The World level's project board (4b.2, owner decision gd-H6ZPz5V262) is pure: WORLD.board orders a project's plans needs-you
// first, then moving (a live gardener on the plot), then by progress and title; counts the plans past the plot cap in "+N more"
// and the summary; leaves zero parts out of it; lists every plan with a gardener (on, gd-vi77cq1oYY); and WORLD.boardWords and
// WORLD.onWords say the same things for the link and the live region.
func TestProjectBoard(t *testing.T) {
	script := `
var W = require(process.argv[1]), assert = require("assert");
function plot(id, title, c, needs) { return { id: id, plan: { id: id, title: title, counts: c }, needs: needs || 0 }; }
var b = { id: "project:gardenio", name: "gardenio", overflow: { n: 3 }, plots: [
  plot("a", "Alpha", { closed: 9, open: 1 }),
  plot("b", "Bravo", { closed: 1, open: 3 }),
  plot("c", "Charlie", { closed: 2, in_progress: 2 }, 2),
  plot("d", "Delta", {}),
  plot("e", "Echo", { closed: 1, open: 1 }),
] };
var gp = { b: { counts: { all: 2 } }, e: { counts: { all: 1 } } };
var bd = W.board(b, gp, 4);
assert.deepStrictEqual(bd.rows.map(function (r) { return r.id; }), ["c", "e", "b", "a"], "needs you, then moving by progress, then the rest");
assert.deepStrictEqual(bd.rows[0], { id: "c", title: "Charlie", needs: 2, pct: 50, live: 0 });
assert.strictEqual(bd.rows[1].live, 1); assert.strictEqual(bd.rows[2].pct, 25);
assert.strictEqual(bd.plans, 8, "the plans past the plot cap count"); assert.strictEqual(bd.more, 4, "Delta and the three past the cap");
assert.strictEqual(bd.head, "8 plans · 2 moving · 1 needs you");
assert.strictEqual(W.board({ name: "x", plots: [plot("q", "Q", {})] }, null).head, "1 plan", "zero parts are left out");
assert.strictEqual(W.board(b, gp, 0).rows.length, 0); assert.strictEqual(W.board(b, gp, 99).more, 3);
assert.strictEqual(W.board({ name: "x", plots: [plot("q", "Q", {}, 1), plot("r", "R", {}, 1)] }, null).head, "2 plans · 2 need you");
assert.strictEqual(W.boardWords(W.board(b, gp, 2)), "gardenio: 8 plans · 2 moving · 1 needs you. Charlie: 2 things need you, 50% done. Echo: 50% done, 1 gardener. 6 more plans: go in to see them. Gardeners on Bravo (2).");
var on = [{ id: "b", title: "Bravo", live: 2, down: 0, ask: 0 }, { id: "e", title: "Echo", live: 1, down: 0, ask: 0 }];
assert.deepStrictEqual(W.board(b, gp, 0).on, on, "a header lists every plan with a gardener (gd-vi77cq1oYY)");
assert.strictEqual(W.onWords(on), " Gardeners on Bravo (2), Echo (1).");
assert.strictEqual(W.onWords([]), "", "no gardeners: no words");
var gq = { a: { counts: { all: 1, ask: 1 } }, d: { counts: { all: 3, down: 1, ask: 1 } }, e: { counts: { all: 1 } } };
assert.strictEqual(W.onWords(W.board(b, gq, 0).on), " Gardeners on Delta (3, 1 down, 1 waiting on you), Alpha (1, 1 waiting on you), Echo (1).", "down first, then asking, then in slot order");
`
	runNodeLogic(t, "world.js", script)
}

// The page heads the other projects at the World level and gives the project links the board's words (the labels are
// aria-hidden); the cursor's project gets no board over the map (gd-OOt8Mx2Mps).
func TestProjectBoardWired(t *testing.T) {
	for _, want := range []string{"said=WORLD.boardWords(WORLD.board(n.ref,gp,SAY_ROWS));want[n.id]=said;", "WORLD.board(k.ref,gp,0)", "describeAll(want);", "worldPlates(list,fr,keep,sv,hud);", "WORLD.visible(world,v,Math.max(level,1))"} {
		if !strings.Contains(pageAppJS+pageSceneJS, want) {
			t.Errorf("garden.js and scene.js do not contain %q", want)
		}
	}
}
