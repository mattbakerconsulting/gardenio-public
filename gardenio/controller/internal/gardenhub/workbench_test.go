package gardenhub

import (
	"strings"
	"testing"
)

// workbench.js's host runs under node (Lands workbench-glass spec 2.5, W-D5; step 5.1, gd-lZbcsMQhRh): withPanel keeps one
// panel parameter in the address and every other one as written, panelOf reads the panel the address names among the kinds
// asked for, a link opens a panel only when it is this garden's own /garden?<kind>=, and Back over the phone's Commons sheet
// counts the panel's entries above the sheet's stack and goes back over exactly the ones it pushed.
func TestWorkbenchPanelLogicUnderNode(t *testing.T) {
	script := `
var V = require(process.argv[1]), W = V.host, assert = require("assert");
assert.deepStrictEqual(W.KINDS, ["bead", "plan", "project", "queue", "q", "bundle", "gardener", "glass", "costs", "creds", "settings", "pic", "transcript", "release"], "the spec's kinds, the Costs panel, the credential pool, the viewer's settings, the picture viewer, a gardener's transcript and a release's changelog");
// withPanel: one panel at a time, everything else as written
assert.strictEqual(W.withPanel("?at=gardenio%2Fgd-1&z=2", "plan", "gd-p"), "?at=gardenio%2Fgd-1&z=2&plan=gd-p", "at and z kept as written");
assert.strictEqual(W.withPanel("?bead=gd-b&at=yard", "plan", "gd-p"), "?at=yard&plan=gd-p", "another kind's parameter goes");
assert.strictEqual(W.withPanel("?plan=a&q=fix+it&glass=g&at=yard", "bead", "gd-b"), "?at=yard&bead=gd-b", "every kind's parameter goes");
assert.strictEqual(W.withPanel("?plan=a&at=yard", "", ""), "?at=yard", "closed: none left");
assert.strictEqual(W.withPanel("?plan=a&at=yard", "plan", ""), "?at=yard", "an empty id is a close");
assert.strictEqual(W.withPanel("?thread=t1&channel=c", "q", "a b/c"), "?thread=t1&channel=c&q=a%20b%2Fc", "a Commons thread's parameters stay");
assert.strictEqual(W.withPanel("?pla=x&plans=y", "plan", "p"), "?pla=x&plans=y&plan=p", "only the kinds' own keys");
assert.strictEqual(W.withPanel("", "project", "gardenio"), "?project=gardenio");
assert.strictEqual(V.withBead("?plan=a&at=yard", "gd-b"), "?at=yard&bead=gd-b", "withBead is withPanel's bead");
// panelOf: the first panel parameter among the kinds asked for decides
assert.deepStrictEqual(W.panelOf("?at=yard&plan=gd-p&z=2"), { kind: "plan", id: "gd-p" });
assert.deepStrictEqual(W.panelOf("?plan=gd-p&bead=gd-b"), { kind: "plan", id: "gd-p" }, "query order");
assert.deepStrictEqual(W.panelOf("?plan=gd-p&bead=gd-b", ["bead"]), { kind: "bead", id: "gd-b" }, "a kind no renderer shows is passed over");
assert.deepStrictEqual(W.panelOf("?q=fix+the+gate"), { kind: "q", id: "fix the gate" }, "decoded");
assert.deepStrictEqual(W.panelOf("?bead=%20gd-b%20"), { kind: "bead", id: "gd-b" }, "trimmed");
assert.strictEqual(W.panelOf("?bead=&plan=gd-p"), null, "an empty first one names none");
assert.strictEqual(W.panelOf("?at=yard"), null);
assert.strictEqual(W.panelOf(""), null);
assert.strictEqual(W.panelOf("?plan=gd-p", []), null);
// tierKind: below the operator tier an agent's Glass is its transcript, wherever it is asked for; the Wall stays the Wall's
assert.strictEqual(W.tierKind("glass", "gd-a", false), "transcript");
assert.strictEqual(W.tierKind("glass", "gd-a", true), "glass", "an operator watches");
assert.strictEqual(W.tierKind("glass", "wall", false), "glass", "the Wall is not one agent's");
assert.strictEqual(W.tierKind("bead", "gd-a", false), "bead");
// linkPanel: this garden's own address only
var O = "https://gardenio.example";
assert.deepStrictEqual(W.linkPanel("", O + "/garden?plan=gd-p", O), { kind: "plan", id: "gd-p" });
assert.deepStrictEqual(W.linkPanel("/v", "/v/garden?at=yard&project=gardenio", O), { kind: "project", id: "gardenio" }, "relative, other params");
assert.strictEqual(W.linkPanel("", O + "/garden?plan=gd-p", O, ["bead"]), null, "not a kind shown here: the link leaves the page as before");
assert.strictEqual(W.linkPanel("", "https://evil.example/garden?plan=gd-p", O), null, "another origin");
assert.strictEqual(W.linkPanel("/v", O + "/garden?plan=gd-p", O), null, "outside the base path");
assert.strictEqual(W.linkPanel("", O + "/garden/plan/gd-p", O), null, "a JSON route is not a link");
assert.strictEqual(W.linkPanel("", "::", "not a url"), null);
`
	runNodeLogic(t, "workbench.js", script)
}

// The page inlines the host before anything that opens a panel, the bead view registers with it, and the rest of the page
// still reaches the bead view as BEAD, so every ?bead= caller is unchanged.
func TestWorkbenchHostsTheBeadView(t *testing.T) {
	body := pageHTML
	if !strings.Contains(body, `{{asset "workbench.js"}}`) {
		t.Fatal("garden.html does not inline web/workbench.js")
	}
	h, b := strings.Index(pageWorkbenchJS, "var WORKBENCH = "), strings.Index(pageWorkbenchJS, "var BEAD = ")
	if h < 0 || b < h {
		t.Errorf("the host must come before the bead view: WORKBENCH at %d, BEAD at %d", h, b)
	}
	if !strings.Contains(pageWorkbenchJS, "W.register('bead', {") {
		t.Error("the bead view does not register as the bead kind")
	}
	for _, s := range []string{"api.open = function (id, how) { W.open('bead', id, how); }", "api.current = function () { return cur(); }"} {
		if !strings.Contains(pageWorkbenchJS, s) {
			t.Errorf("BEAD lost %q", s)
		}
	}
}
