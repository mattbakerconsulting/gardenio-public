package gardenhub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// The bead view's writes (Lands workbench-glass spec WB-2..WB-4, WB-12; step 5.4, gd-PXHQecYeDU).

// GET /garden/bead/{id}?comments=N sends more of the newest comments (Show older), never past beadCommentsAll; the
// document says when its description is a clipped copy (not to be edited) and whether the viewer may spawn on it.
func TestBeadViewOlderCommentsClipAndSpawn(t *testing.T) {
	f := beadFixture()
	var cs []beadsapi.Comment
	for i := 0; i < beadCommentsAll+20; i++ {
		cs = append(cs, beadsapi.Comment{Author: "a", Text: fmt.Sprintf("c%d", i)})
	}
	f.comments["gd-task"] = cs
	f.beads["gd-task"].Description = strings.Repeat("x", beadDescMax+1)
	var asked []string
	mux := http.NewServeMux()
	(&Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v", Beads: f, MaySpawn: func(_ *http.Request, p string) bool {
		asked = append(asked, p)
		return p == "gardenio"
	}}).Register(mux)

	get := func(q string) map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/garden/bead/gd-task"+q, nil))
		var doc map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("%s: %v\n%s", q, err, rec.Body.String())
		}
		return doc
	}
	for q, want := range map[string]int{"": beadCommentsMax, "?comments=80": 80, "?comments=5": beadCommentsMax, "?comments=99999": beadCommentsAll, "?comments=x": beadCommentsMax} {
		doc := get(q)
		got := doc["comments"].([]any)
		last := got[len(got)-1].(map[string]any)["text"]
		if len(got) != want || last != fmt.Sprintf("c%d", beadCommentsAll+19) {
			t.Errorf("%q: %d comments ending %v, want the newest %d", q, len(got), last, want)
		}
	}
	doc := get("")
	if doc["description_clipped"] != true || doc["spawn"] != true {
		t.Errorf("description_clipped %v, spawn %v; want both true", doc["description_clipped"], doc["spawn"])
	}
	if asked[len(asked)-1] != "gardenio" {
		t.Errorf("MaySpawn asked about %q, want the bead's project", asked[len(asked)-1])
	}

	f.beads["gd-task"].Description = "short"
	f.beads["gd-task"].Labels = []string{"project:kbeads"}
	doc = get("")
	if _, ok := doc["description_clipped"]; ok {
		t.Error("a whole description is not clipped")
	}
	if _, ok := doc["spawn"]; ok {
		t.Error("a land the viewer may not spawn in offers no spawn")
	}
	if _, doc := getBead(t, beadMux(f), "gd-task"); doc["spawn"] != nil {
		t.Error("no MaySpawn: no spawn")
	}
}

// The page says data-edit only to a viewer CanEdit lets write, and inlines beadwrite.js before the bead view that calls it.
func TestPageOffersBeadWrites(t *testing.T) {
	if i, j := strings.Index(pageHTML, `{{asset "beadwrite.js"}}`), strings.Index(pageHTML, `{{asset "workbench.js"}}`); i < 0 || j < i {
		t.Fatalf("beadwrite.js must be inlined before workbench.js: %d, %d", i, j)
	}
	for _, can := range []bool{false, true} {
		h := &Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v", CanEdit: func(*http.Request) bool { return can }}
		mux := http.NewServeMux()
		h.Register(mux)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/garden", nil))
		body := inlined(t, rec.Body.String())
		if got := strings.Contains(body, `data-edit="1"`); got != can {
			t.Errorf("CanEdit %v: data-edit present %v", can, got)
		}
		if !strings.Contains(body, "var BEADWRITE = ") {
			t.Error("the page does not carry beadwrite.js")
		}
	}
}

// The elements beadwrite.js hides with .hidden whose class sets display: the class rule beats the UA [hidden] rule, so each
// needs its own [hidden]{display:none} or the write bar stays up (and clickable, dropping the draft) under its open form and
// the draft stays up under its preview (gd-lQx0IC0HNx).
func TestBeadWriteHiddenBeatsDisplay(t *testing.T) {
	rule, css := regexp.MustCompile(`([^{}@;]+)\{([^{}]*)\}`), regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(pageCSS, "")
	for _, c := range []struct{ cls, hide string }{{".bv-acts", "bar.hidden = true"}, {".bw-text", "ta.hidden = true"}} {
		cls, shows, hides := c.cls, false, false
		for _, m := range rule.FindAllStringSubmatch(css, -1) {
			for _, sel := range strings.Split(m[1], ",") {
				switch strings.TrimSpace(sel) {
				case cls:
					shows = shows || strings.Contains(m[2], "display:") && !strings.Contains(m[2], "display:none")
				case cls + "[hidden]":
					hides = hides || strings.Contains(m[2], "display:none")
				}
			}
		}
		if shows && !hides {
			t.Errorf("%s sets display but has no %s[hidden]{display:none}", cls, cls)
		}
		if !strings.Contains(pageBeadWriteJS, c.hide) {
			t.Errorf("beadwrite.js no longer hides %s with .hidden; update this test", cls)
		}
	}
}

// beadwrite.js's pure half runs under node: what an edit sends, the verbs a bead offers, the links and their removes, the
// older-comments step, the answer's words, the spawn form and the preview's links.
func TestBeadWriteLogicUnderNode(t *testing.T) {
	script := `
var W = require(process.argv[1]), assert = require("assert");
var d = { id: "gd-a", title: "Fix it", status: "open", priority: 2, assignee: "", owner: "o@x", labels: ["project:gardenio", "ux"], description: "Line\n\nTwo" };

/* an edit sends only what changed, in the form's order, to the field's own route */
assert.deepStrictEqual(W.changes(d, { title: " Fix it ", status: "open", priority: "2", assignee: "", owner: "o@x", labels: "ux,  project:gardenio", description: "Line\n\nTwo\n" }), [], "trimmed values and reordered labels are no change");
var c = W.changes(d, { title: "Fix it now", priority: "0", labels: "ux", description: "New\n\ntext " });
assert.deepStrictEqual(c.map(function (x) { return x.path + "=" + x.value; }), ["title=Fix it now", "priority=0", "labels=ux", "description=New\n\ntext "]);
assert.deepStrictEqual(W.changes(Object.assign({}, d, { description_clipped: true }), { description: "cut" }), [], "a clipped description is never sent");
assert.deepStrictEqual(W.changes(d, {}), []);
assert.strictEqual(W.value(d, "labels"), "project:gardenio, ux");
assert.strictEqual(W.value({ priority: 0 }, "priority"), "0");
assert.strictEqual(W.value({}, "priority"), "");

/* the lifecycle verbs, as the bead page's toolbar has them */
assert.deepStrictEqual(W.verbs(d, "oidc:me@x"), ["close", "claim"]);
assert.deepStrictEqual(W.verbs(Object.assign({}, d, { assignee: "Me@X" }), "oidc:me@x"), ["close", "release"], "a claim writes the bare email");
assert.deepStrictEqual(W.verbs(Object.assign({}, d, { assignee: "oidc:me@x" }), "oidc:me@x"), ["close", "release"]);
assert.deepStrictEqual(W.verbs(Object.assign({}, d, { status: "closed" }), "oidc:me@x"), ["reopen"]);
assert.deepStrictEqual(W.verbs(Object.assign({}, d, { type: "escalation" }), "oidc:me@x"), ["claim"], "an escalation is settled by Approve and Reject");
assert.deepStrictEqual(W.verbs(Object.assign({}, d, { type: "agent" }), "oidc:me@x"), []);
assert.strictEqual(W.mine("", "oidc:me@x"), false);
assert.strictEqual(W.mine("me@x", ""), false);

/* links: a pasted id, where each kind posts, and the remove a waits-on or holds-up row offers */
assert.ok(W.isId(" gd-AbC123xyz0 ") && W.isId("ka-0000001089"));
assert.ok(!W.isId("gd-") && !W.isId("GD-abc123") && !W.isId("hello world") && !W.isId(""));
assert.deepStrictEqual([W.linkOf("waits").path, W.linkOf("waits").form("gd-b")], ["deps", { other: "gd-b", dir: "waits" }]);
assert.deepStrictEqual([W.linkOf("holds").path, W.linkOf("holds").form("gd-b")], ["deps", { other: "gd-b", dir: "holds" }]);
assert.deepStrictEqual([W.linkOf("parent").path, W.linkOf("parent").form("gd-b")], ["parent", { parent_id: "gd-b" }]);
assert.strictEqual(W.linkOf("nope"), null);
assert.deepStrictEqual(W.unlink("blocked_by", "gd-b"), { path: "deps/remove", form: { other: "gd-b", dir: "waits" }, word: "Stop waiting on" });
assert.deepStrictEqual(W.unlink("blocks", "gd-b").form, { other: "gd-b", dir: "holds" });
assert.strictEqual(W.unlink("parents", "gd-b"), null, "a parent changes through Part of, not Remove");
assert.strictEqual(W.unlink("children", "gd-b"), null);

/* Show older: forty more at a time, up to the server's cap, none when all are shown */
assert.strictEqual(W.older(40, 41), 41);
assert.strictEqual(W.older(40, 500), 80);
assert.strictEqual(W.older(380, 500), 400);
assert.strictEqual(W.older(400, 500), 0, "past the cap the note says how many are not shown");
assert.strictEqual(W.older(3, 3), 0);

/* an answer's words (action_json.go) */
assert.strictEqual(W.failure(200, { ok: true }), "");
assert.strictEqual(W.failure(204, null), "");
assert.strictEqual(W.failure(400, { ok: false, error: "Title  cannot\nbe empty.", status: 400 }), "Title cannot be empty.");
assert.strictEqual(W.failure(403, { ok: false, status: 403 }), "You may not do that here.");
assert.strictEqual(W.failure(418, null), "That did not go through (418).");

/* the spawn form: the bead's land and the bead as the task; an empty role or model keeps the defaults */
assert.deepStrictEqual(W.spawnForm({ id: "gd-a", project: "gardenio" }, { role: "", model: "", prompt: " go " }), { project: "gardenio", task_id: "gd-a", prompt: "go" });
assert.deepStrictEqual(W.spawnForm({ id: "gd-a", project: "gardenio" }, { role: "crew", model: "opus", prompt: "go" }), { project: "gardenio", task_id: "gd-a", prompt: "go", role: "crew", model: "opus" });
assert.strictEqual(W.spawnable({ id: "gd-a", project: "gardenio", spawn: true, type: "task" }), true);
assert.strictEqual(W.spawnable({ id: "gd-a", project: "gardenio", type: "task" }), false, "only where the server says the viewer may spawn");
assert.strictEqual(W.spawnable({ id: "gd-a", spawn: true, type: "task" }), false, "no land, no spawn");
assert.strictEqual(W.spawnable({ id: "gd-a", project: "gardenio", spawn: true, type: "agent" }), false);

/* a preview's link to the viewer's bead page opens the bead in the garden; anything else stays as it is */
assert.strictEqual(W.repoint("/v", "/v/gd-AbC123xyz0", "https://h"), "/v/garden?bead=gd-AbC123xyz0");
assert.strictEqual(W.repoint("", "https://h/gd-AbC123xyz0", "https://h"), "/garden?bead=gd-AbC123xyz0");
assert.strictEqual(W.repoint("/v", "https://elsewhere/v/gd-AbC123xyz0", "https://h"), "");
assert.strictEqual(W.repoint("/v", "/v/plans/gd-AbC123xyz0", "https://h"), "");
assert.strictEqual(W.repoint("/v", "/v/gd-AbC123xyz0?x=1#c", "https://h"), "/v/garden?bead=gd-AbC123xyz0");
`
	runNodeLogic(t, "beadwrite.js", script)
}
