package gardenhub

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// The opening phrase of a Send-a-gardener mention, and only it, names the bead a gardener was sent to.
func TestTendedBead(t *testing.T) {
	cases := map[string]string{
		"@gardenio tend gd-A1b: Figure out what's up with this bead.": "gd-A1b",
		"  @gardenio   tend   ka-0000001089 : do it":                  "ka-0000001089",
		"@gardenio tend gd-A1b":                                       "gd-A1b", // nothing after the id: still sent
		"@gardenio tend gd-A1b\nFigure it out":                        "",       // a newline is not the colon
		"@gardenio adopt gd-A1b into a plan.":                         "",       // Adopt's mention
		"@gardenio tend the garden: gd-A1b":                           "",       // words, not an id
		"@gardenio tend gd-A1b, then gd-A2e":                          "",       // a comma is no colon
		"please @gardenio tend gd-A1b: now":                           "",       // not at the head
		"@gardenio tend gd-x: too short":                              "",
		"":                                                            "",
	}
	for text, want := range cases {
		if got := tendedIn(text); got != want {
			t.Errorf("tendedIn(%q) = %q, want %q", text, got, want)
		}
	}
	long := "@gardenio tend gd-A1b: " + strings.Repeat("x", 5000)
	if tendedIn(long) != "gd-A1b" {
		t.Error("a long prompt after the phrase is still a send")
	}
	if TendedBead(nil) != "" || TendedBead(&beadsapi.BeadDetail{}) != "" {
		t.Error("no bead, no root text: nothing")
	}
	if got := TendedBead(&beadsapi.BeadDetail{Fields: map[string]string{"slack_root_text": "@gardenio tend gd-A1b: go"}}); got != "gd-A1b" {
		t.Errorf("TendedBead reads slack_root_text: %q", got)
	}
}

// tend.js's pure half runs under node: the same grammar as tend.go (TEND.tends), which beads are offered the button, the
// mention it says, where, what the viewer is told, and the gardeners a bead has.
func TestTendLogicUnderNode(t *testing.T) {
	script := `
var T = require(process.argv[1]), assert = require("assert");
/* the grammar, as tend.go reads it (TestTendedBead has the same cases) */
assert.strictEqual(T.tends("@gardenio tend gd-A1b: Figure out what's up with this bead."), "gd-A1b");
assert.strictEqual(T.tends("  @gardenio   tend   ka-0000001089 : do it"), "ka-0000001089");
assert.strictEqual(T.tends("@gardenio tend gd-A1b"), "gd-A1b");
assert.strictEqual(T.tends("@gardenio tend gd-A1b\nFigure it out"), "");
assert.strictEqual(T.tends("@gardenio adopt gd-A1b into a plan."), "");
assert.strictEqual(T.tends("@gardenio tend the garden: gd-A1b"), "");
assert.strictEqual(T.tends("@gardenio tend gd-A1b, then gd-A2e"), "");
assert.strictEqual(T.tends("please @gardenio tend gd-A1b: now"), "");
assert.strictEqual(T.tends(""), "");
assert.strictEqual(T.tends(null), "");

/* which beads get the button: any bead with an id that is not a gardener itself */
assert.strictEqual(T.offered({ id: "gd-ax", type: "task", status: "open" }), true);
assert.strictEqual(T.offered({ id: "gd-ax", type: "decision", status: "open" }), true, "a decision too: figure out what is up with it");
assert.strictEqual(T.offered({ id: "gd-ax", type: "epic", status: "closed" }), true, "a closed bead can still be looked into");
assert.strictEqual(T.offered({ id: "gd-ax", type: "agent", status: "open" }), false, "a gardener is asked in its thread, not sent another");
assert.strictEqual(T.offered({ type: "task" }), false);
assert.strictEqual(T.offered(null), false);

/* the mention: the fixed phrase the farm reads, the prompt as typed, then the bead in words and what to say back */
assert.strictEqual(T.DEFAULT, "Figure out what's up with this bead and make progress on it, or say what unblocks it.");
var m = T.ask({ id: "gd-ax", project: "gardenio", type: "bug", title: 'Fix the "thing"  now' }, "");
assert.strictEqual(T.tends(m), "gd-ax", "the mention it says is one the farm reads");
assert.ok(m.indexOf("@gardenio tend gd-ax: " + T.DEFAULT) === 0, "an empty prompt is the default one: " + m);
assert.ok(m.indexOf("It is a bug in gardenio (\"Fix the 'thing' now\").") > 0, "the bead in words: " + m);
assert.ok(m.indexOf("kd show gd-ax") > 0, "how to read it");
assert.ok(/Reply here either way\.$/.test(m), "and to say back: " + m);
var m2 = T.ask({ id: "gd-ay" }, "  Why is this  still open?  ");
assert.ok(m2.indexOf("@gardenio tend gd-ay: Why is this  still open?") === 0, "the prompt as typed, trimmed: " + m2);
assert.ok(m2.indexOf("It is a bead.") > 0, "no type, no project, no title: still a sentence: " + m2);
var m3 = T.ask({ id: "gd-az", type: "epic", title: "Plan" }, "@gardenio tend gd-az: already phrased");
assert.strictEqual(T.tends(m3), "gd-az"); assert.ok(m3.indexOf("@gardenio tend gd-az: already phrased") === 0, "a prompt that already opens with the phrase is not doubled: " + m3);
assert.strictEqual(T.askPath("/v", { id: "gd-ax", project: "a b" }), "/v/garden/c/a%20b/general/say");
assert.strictEqual(T.askPath("/v", { id: "gd-ax" }), "", "no project: no land to ask in");
assert.strictEqual(T.threadPath("", { project: "gardenio" }, "gd-m 1"), "/garden/c/n/gd-m%201", "its node in the workbench");
assert.deepStrictEqual(T.asked(201, "Starting a gardener.", "gd-ax", "gd-m1"), { ok: true, text: "Sent a gardener to gd-ax. Starting a gardener." });
assert.deepStrictEqual(T.asked(200, "", "gd-ax", "gd-m1"), { ok: true, text: "Sent a gardener to gd-ax." });
assert.deepStrictEqual(T.asked(200, "", "gd-ax", ""), { ok: false, text: "This land has no Commons, so no gardener can be sent." }, "a 200 with no message is another page answering");
assert.deepStrictEqual(T.asked(403, "", "gd-ax", ""), { ok: false, text: "You may not post in this land, so no gardener can be sent." });
assert.deepStrictEqual(T.asked(429, "", "gd-ax", ""), { ok: false, text: "The Commons is busy. Try again in a moment." });
assert.deepStrictEqual(T.asked(500, "", "gd-ax", ""), { ok: false, text: "The send did not go through (500)." });

/* the gardeners a bead has: sent to it, or on it as their task; the viewer's own first, then the neediest */
var gs = [
  { id: "a", agent: "g-a", task: "gd-ax", state: "working", bucket: "alive", operator: "oidc:you", since: 10 },
  { id: "b", agent: "g-b", tends: "gd-ax", state: "working", bucket: "alive", operator: "oidc:me", since: 20 },
  { id: "c", agent: "g-c", tends: "gd-ax", state: "failed", bucket: "stuck", operator: "oidc:you", since: 30 },
  { id: "d", agent: "g-d", tends: "gd-other", state: "working", bucket: "alive", operator: "oidc:me", since: 40 },
  { id: "e", agent: "g-e", task: "gd-other", tends: "gd-ax", state: "idle", bucket: "idle", operator: "", since: 50 },
  { id: "f", agent: "escalation-gd-ax", escalation: "gd-ax", state: "working", bucket: "alive", operator: "", since: 60 }
];
assert.deepStrictEqual(T.here(gs, "gd-ax", "oidc:me").map(function (g) { return g.id; }), ["b", "c", "a", "f", "e"], "mine first, then down before at work before resting; the escalation's executor is here too");
assert.deepStrictEqual(T.here(gs, "gd-ax", "").map(function (g) { return g.id; }), ["c", "a", "b", "f", "e"], "no viewer: down, then at work (who has waited longest first), then resting");
assert.deepStrictEqual(T.here(gs, "gd-other", "").map(function (g) { return g.id; }), ["d", "e"], "an executor is on its escalation only");
assert.deepStrictEqual(T.here(null, "gd-ax", "oidc:me"), []);
assert.deepStrictEqual(T.here(gs, "", "oidc:me"), []);
/* what a gardener said when it left: finished exits for this bead, newest first, none older than a day */
var ex = [{ id: "b", agent: "g-b", kind: "finished", tends: "gd-ax", tldr: "Closed it.", at: 1000 }, { id: "c", agent: "g-c", kind: "interrupted", tends: "gd-ax", at: 1001 }, { id: "d", agent: "g-d", kind: "parked", tends: "gd-ax", tldr: "Needs a key.", at: 1002 }, { id: "z", agent: "g-z", kind: "finished", tends: "gd-ay", tldr: "no", at: 1003 }, { id: "f", agent: "escalation-gd-ax", kind: "finished", escalation: "gd-ax", tldr: "Merged it.", at: 999 }];
assert.deepStrictEqual(T.left([], ex, "gd-ax", 1100).map(function (x) { return x.agent + ":" + x.kind + ":" + (x.tldr || ""); }), ["g-d:parked:Needs a key.", "g-c:interrupted:", "g-b:finished:Closed it.", "escalation-gd-ax:finished:Merged it."], "what was sent to it, and the executor that ran it");
assert.deepStrictEqual(T.left(ex.slice(0, 1), ex.slice(1), "gd-ax", 1100).length, 4, "kept across patches, once each");
assert.deepStrictEqual(T.left([], ex, "gd-ax", 1003 + 86400 + 1), [], "a day on, gone");
assert.deepStrictEqual([T.leftWords({ kind: "finished" }), T.leftWords({ kind: "parked" }), T.leftWords({ kind: "interrupted" })], ["finished", "parked", "left without a word"]);
`
	runNodeLogic(t, "tend.js", script)
}

// The page says whether the viewer may send a gardener (body data-tend), the ETag tells the two pages apart, tend.js is
// inlined before workbench.js mounts it, and workbench.js mounts the send and the live list.
func TestTendPageFlag(t *testing.T) {
	serve := func(can bool) *httptest.ResponseRecorder {
		h := &Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v", CanTend: func(*http.Request) bool { return can }}
		mux := http.NewServeMux()
		h.Register(mux)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/garden", nil))
		return rec
	}
	yes, no := serve(true), serve(false)
	if !strings.Contains(yes.Body.String(), ` data-tend="1"`) {
		t.Error("an operator's page lacks data-tend")
	}
	if strings.Contains(no.Body.String(), ` data-tend="1"`) {
		t.Error("a page for a viewer who may not send carries data-tend")
	}
	if yes.Header().Get("ETag") == no.Header().Get("ETag") {
		t.Error("the page with and without Send a gardener share an ETag")
	}
	body := inlined(t, yes.Body.String())
	if i, j := strings.Index(body, "var TEND = "), strings.Index(body, "var BEAD = "); i < 0 || j < 0 || i > j {
		t.Error("tend.js is not inlined before workbench.js")
	}
	if !strings.Contains(pageWorkbenchJS, "TEND.mount(bodyEl, d)") || !strings.Contains(pageWorkbenchJS, "TEND.live(actsEl, k.id") {
		t.Error("workbench.js does not mount the send and its live list")
	}
	if !strings.Contains(pagePorchJS, "g.tends") {
		t.Error("porch.js does not say where a sent gardener went")
	}
}
