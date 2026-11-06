package gardenhub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// adoptFake is the bead view's fake with a writable dependency set: an AddDependency is recorded and becomes a row the
// next read sees, so a repeat adoption finds its own edge.
type adoptFake struct {
	*fakeBeads
	writes  []beadsapi.Dependency
	by      []string // each write's created_by
	ops     []string // the operator each write's context carried
	failAdd error
}

func (f *adoptFake) AddDependency(ctx context.Context, beadID, dependsOnID, depType, createdBy string) error {
	if f.failAdd != nil {
		return f.failAdd
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	d := beadsapi.Dependency{BeadID: beadID, DependsOnID: dependsOnID, Type: depType}
	f.writes = append(f.writes, d)
	f.by = append(f.by, createdBy)
	f.deps = append(f.deps, d)
	f.ops = append(f.ops, beadsapi.OperatorFromContext(ctx))
	return nil
}

func adoptFixture() *adoptFake {
	gd := []string{"project:gardenio"}
	return &adoptFake{fakeBeads: &fakeBeads{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-plan":   {ID: "gd-plan", Title: "Homestead", Type: "epic", Status: "open", Labels: []string{"project:gardenio", "plan"}, Fields: map[string]string{"phase_order": `["gd-ph1","gd-ph2"]`}},
			"gd-ph1":    {ID: "gd-ph1", Title: "1. World", Type: "epic", Status: "open", Labels: gd},
			"gd-ph2":    {ID: "gd-ph2", Title: "2. Done", Type: "epic", Status: "closed", Labels: gd},
			"gd-stray":  {ID: "gd-stray", Title: "Not a phase", Type: "epic", Status: "open", Labels: gd},
			"gd-done":   {ID: "gd-done", Title: "Harvested", Type: "epic", Status: "closed", Labels: []string{"project:gardenio", "plan"}},
			"cl-plan":   {ID: "cl-plan", Title: "Claimlayer plan", Type: "epic", Status: "open", Labels: []string{"project:claimlayer", "plan"}},
			"gd-loose":  {ID: "gd-loose", Title: "A loose task", Type: "task", Status: "open", Labels: gd},
			"gd-bug":    {ID: "gd-bug", Title: "A loose bug", Type: "bug", Status: "open", Labels: gd},
			"gd-nolab":  {ID: "gd-nolab", Title: "No project label", Type: "task", Status: "open"},
			"gd-bare":   {ID: "gd-bare", Title: "A plan with no project", Type: "epic", Status: "open", Labels: []string{"plan"}},
			"gd-thread": {ID: "gd-thread", Title: "Slack thread: ...", Type: "task", Status: "open", Labels: []string{"project:gardenio", "thread"}},
			"gd-chat":   {ID: "gd-chat", Title: "Raised in a thread", Type: "feature", Status: "open", Labels: gd},
			"gd-held":   {ID: "gd-held", Title: "Already a step", Type: "task", Status: "open", Labels: gd},
			"gd-hidden": {ID: "gd-hidden", Title: "Parent we cannot see", Type: "task", Status: "open", Labels: gd},
			"gd-shut":   {ID: "gd-shut", Title: "Closed task", Type: "task", Status: "closed", Labels: gd},
			"gd-ask":    {ID: "gd-ask", Title: "A decision", Type: "decision", Status: "open", Labels: gd},
		},
		deps: []beadsapi.Dependency{
			{BeadID: "gd-ph1", DependsOnID: "gd-plan", Type: "parent-child"},
			{BeadID: "gd-chat", DependsOnID: "gd-thread", Type: "parent-child"},
			{BeadID: "gd-chat", DependsOnID: "gd-loose", Type: "blocks"},
			{BeadID: "gd-held", DependsOnID: "gd-stray", Type: "parent-child"},
			{BeadID: "gd-hidden", DependsOnID: "gd-gone", Type: "parent-child"},
		},
	}}
}

func adoptMux(h *Handler) *http.ServeMux {
	mux := http.NewServeMux()
	h.Register(mux)
	mux.HandleFunc("POST /garden/bead/{id}/adopt", h.Adopt) // beads-viewer registers it behind its operator gate
	return mux
}

func adoptHandler(f *adoptFake) *Handler {
	h := &Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v", Viewer: func(*http.Request) string { return "oidc:op@example.com" }}
	if f != nil {
		h.Beads, h.Links = f, f
	}
	return h
}

func postAdopt(t *testing.T, mux *http.ServeMux, id string, form url.Values) (int, adoptResult) {
	t.Helper()
	req := httptest.NewRequest("POST", "/garden/bead/"+id+"/adopt", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var res adoptResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("adopt %s answered %d with no JSON: %v\n%s", id, rec.Code, err, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("adopt %s Content-Type = %q", id, ct)
	}
	return rec.Code, res
}

func form(kv ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v
}

// Adopting a loose bead writes one parent-child edge to the plan, as the operator, and says so; asking again finds the
// edge and writes nothing (gd-6HQyB0jgmO).
func TestAdoptIntoPlanIsOneEdgeAndIdempotent(t *testing.T) {
	f := adoptFixture()
	mux := adoptMux(adoptHandler(f))

	code, res := postAdopt(t, mux, "gd-loose", form("plan", "gd-plan"))
	if code != 200 || !res.OK || res.Already || res.Plan != "gd-plan" || res.ID != "gd-loose" {
		t.Fatalf("adopt into plan: %d %+v", code, res)
	}
	want := beadsapi.Dependency{BeadID: "gd-loose", DependsOnID: "gd-plan", Type: "parent-child"}
	if len(f.writes) != 1 || f.writes[0] != want {
		t.Fatalf("writes = %+v, want exactly %+v", f.writes, want)
	}
	if f.by[0] != "oidc:op@example.com" {
		t.Errorf("the edge's created_by is %q, want the operator", f.by[0])
	}
	if f.ops[0] != "oidc:op@example.com" {
		t.Errorf("the write's context carried operator %q; the daemon audits X-Operated-By", f.ops[0])
	}

	code, res = postAdopt(t, mux, "gd-loose", form("plan", "gd-plan"))
	if code != 200 || !res.OK || !res.Already {
		t.Fatalf("adopting again: %d %+v, want 200 already", code, res)
	}
	if len(f.writes) != 1 {
		t.Fatalf("adopting again wrote again: %+v", f.writes)
	}
}

// A phase of the plan is a target too; the edge goes to the phase, and a repeat is idempotent the same way.
func TestAdoptIntoPhase(t *testing.T) {
	f := adoptFixture()
	mux := adoptMux(adoptHandler(f))
	code, res := postAdopt(t, mux, "gd-bug", form("plan", "gd-plan", "phase", "gd-ph1"))
	if code != 200 || !res.OK || res.Phase != "gd-ph1" {
		t.Fatalf("adopt into phase: %d %+v", code, res)
	}
	if len(f.writes) != 1 || f.writes[0].DependsOnID != "gd-ph1" || f.writes[0].Type != "parent-child" {
		t.Fatalf("writes = %+v, want one parent-child edge to gd-ph1", f.writes)
	}
	if code, res = postAdopt(t, mux, "gd-bug", form("plan", "gd-plan", "phase", "gd-ph1")); code != 200 || !res.Already || len(f.writes) != 1 {
		t.Fatalf("again: %d %+v writes %d", code, res, len(f.writes))
	}
}

// A Slack thread bead is a parent-child parent that leaves a bead in the meadow (it is provenance, not a container), so
// it does not stop adoption.
func TestAdoptKeepsAThreadParent(t *testing.T) {
	f := adoptFixture()
	code, res := postAdopt(t, adoptMux(adoptHandler(f)), "gd-chat", form("plan", "gd-plan"))
	if code != 200 || !res.OK || len(f.writes) != 1 {
		t.Fatalf("a bead under a thread bead: %d %+v writes %+v", code, res, f.writes)
	}
}

// Every refusal answers before any write: the target is not an open plan or one of its open phases, the bead is not open
// loose work, it sits in another project, or it already has a container parent (or one this viewer cannot read).
func TestAdoptRefusals(t *testing.T) {
	for _, c := range []struct {
		name, id string
		form     url.Values
		code     int
		says     string
		parent   string
	}{
		{"target is an epic but not a plan", "gd-loose", form("plan", "gd-stray"), 400, "is not a plan", ""},
		{"target is a task", "gd-loose", form("plan", "gd-thread"), 400, "is not a plan", ""},
		{"target is closed", "gd-loose", form("plan", "gd-done"), 409, "is closed", ""},
		{"target is missing", "gd-loose", form("plan", "gd-nope"), 404, "There is no plan gd-nope", ""},
		{"phase not in phase_order", "gd-loose", form("plan", "gd-plan", "phase", "gd-stray"), 400, "is not a phase of gd-plan", ""},
		{"phase is closed", "gd-loose", form("plan", "gd-plan", "phase", "gd-ph2"), 409, "phase gd-ph2 is closed", ""},
		{"no plan named", "gd-loose", form("phase", "gd-ph1"), 400, "which plan", ""},
		{"bead is missing", "gd-nope", form("plan", "gd-plan"), 404, "There is no bead gd-nope", ""},
		{"bead is a decision", "gd-ask", form("plan", "gd-plan"), 409, "is a decision", ""},
		{"bead is an epic", "gd-ph1", form("plan", "gd-plan"), 409, "is an epic", ""},
		{"bead is closed", "gd-shut", form("plan", "gd-plan"), 409, "gd-shut is closed", ""},
		{"another project", "gd-loose", form("plan", "cl-plan"), 409, "belongs to gardenio and the plan to claimlayer", ""},
		{"bead has no project", "gd-nolab", form("plan", "gd-plan"), 409, "gd-nolab has no project", ""},
		{"plan has no project", "gd-loose", form("plan", "gd-bare"), 409, "plan gd-bare has no project", ""},
		{"already under an epic", "gd-held", form("plan", "gd-plan"), 409, "already part of gd-stray", "gd-stray"},
		{"parent not readable", "gd-hidden", form("plan", "gd-plan"), 409, "already part of gd-gone", "gd-gone"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := adoptFixture()
			code, res := postAdopt(t, adoptMux(adoptHandler(f)), c.id, c.form)
			if code != c.code || res.OK || !strings.Contains(res.Error, c.says) || res.Parent != c.parent {
				t.Errorf("%d %+v, want %d saying %q (parent %q)", code, res, c.code, c.says, c.parent)
			}
			if len(f.writes) != 0 {
				t.Errorf("a refused adoption wrote %+v", f.writes)
			}
		})
	}
}

// SameSite=Lax lets a same-site page (any *.clonk.tv) post on the operator's session, so the route takes a write only
// from its own origin: a browser's Sec-Fetch-Site other than same-origin, or an Origin naming another host, is refused
// before anything is read or written. The bead view's own fetch, and a request with neither header, go through.
func TestAdoptRefusesAnotherOrigin(t *testing.T) {
	for _, c := range []struct {
		name    string
		headers map[string]string
		code    int
	}{
		{"same-site page", map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "https://evil.clonk.tv"}, 403},
		{"cross-site page", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, 403},
		{"typed or bookmarked", map[string]string{"Sec-Fetch-Site": "none"}, 403},
		{"other Origin, no fetch metadata", map[string]string{"Origin": "https://evil.clonk.tv"}, 403},
		{"opaque Origin", map[string]string{"Origin": "null"}, 403},
		{"forwarded host is not the Origin", map[string]string{"Origin": "https://evil.clonk.tv", "X-Forwarded-Host": "gardenio.clonk.tv"}, 403},
		{"the page itself", map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://example.com"}, 200},
		{"the page through the ingress", map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "https://gardenio.clonk.tv", "X-Forwarded-Host": "gardenio.clonk.tv"}, 200},
		{"not a browser", nil, 200},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := adoptFixture()
			req := httptest.NewRequest("POST", "/garden/bead/gd-loose/adopt", strings.NewReader(form("plan", "gd-plan").Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			for k, v := range c.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			adoptMux(adoptHandler(f)).ServeHTTP(rec, req)
			var res adoptResult
			_ = json.Unmarshal(rec.Body.Bytes(), &res)
			if rec.Code != c.code {
				t.Fatalf("%d %+v, want %d", rec.Code, res, c.code)
			}
			if c.code == 403 && (len(f.writes) != 0 || res.OK || res.Error == "") {
				t.Errorf("a refused origin wrote %+v or said nothing (%+v)", f.writes, res)
			}
		})
	}
}

// No viewer, no daemon client, or a daemon that refuses or fails the write: each is a JSON answer the bead view can say.
func TestAdoptEnvironmentFailures(t *testing.T) {
	h := adoptHandler(nil)
	if code, res := postAdopt(t, adoptMux(h), "gd-loose", form("plan", "gd-plan")); code != 503 || res.OK {
		t.Errorf("no daemon client: %d %+v, want 503", code, res)
	}

	f := adoptFixture()
	h = adoptHandler(f)
	h.Viewer = func(*http.Request) string { return "" }
	if code, res := postAdopt(t, adoptMux(h), "gd-loose", form("plan", "gd-plan")); code != 401 || res.OK || len(f.writes) != 0 {
		t.Errorf("anonymous: %d %+v, want 401 and no write", code, res)
	}

	f = adoptFixture()
	f.failAdd = &beadsapi.APIError{StatusCode: http.StatusForbidden, Message: "no"}
	if code, res := postAdopt(t, adoptMux(adoptHandler(f)), "gd-loose", form("plan", "gd-plan")); code != 403 || res.OK || !strings.Contains(res.Error, "did not let you") {
		t.Errorf("daemon 403: %d %+v, want 403", code, res)
	}
	f.failAdd = errors.New("connection refused")
	if code, res := postAdopt(t, adoptMux(adoptHandler(f)), "gd-loose", form("plan", "gd-plan")); code != 502 || res.OK {
		t.Errorf("daemon down: %d %+v, want 502", code, res)
	}
}

// The page tells the bead view whether to offer adoption, and the ETag tells the two pages apart; adopt.js is inlined
// before workbench.js, which mounts it.
func TestAdoptPageFlag(t *testing.T) {
	serve := func(can bool) *httptest.ResponseRecorder {
		h := adoptHandler(adoptFixture())
		h.CanAdopt = func(*http.Request) bool { return can }
		rec := httptest.NewRecorder()
		adoptMux(h).ServeHTTP(rec, httptest.NewRequest("GET", "/garden", nil))
		return rec
	}
	yes, no := serve(true), serve(false)
	if !strings.Contains(yes.Body.String(), ` data-adopt="1"`) {
		t.Error("an operator's page lacks data-adopt")
	}
	if strings.Contains(no.Body.String(), ` data-adopt="1"`) {
		t.Error("a page for a viewer who may not adopt carries data-adopt")
	}
	if yes.Header().Get("ETag") == no.Header().Get("ETag") {
		t.Error("the page with and without adoption share an ETag")
	}
	body := inlined(t, yes.Body.String())
	a, b := strings.Index(body, "var ADOPT = "), strings.Index(body, "var BEAD = ")
	if a < 0 || b < 0 || a > b {
		t.Errorf("adopt.js must be inlined before workbench.js: ADOPT at %d, BEAD at %d", a, b)
	}
	if !strings.Contains(pageWorkbenchJS, "ADOPT.mount(") {
		t.Error("workbench.js does not mount the adoption picker")
	}
}

// adopt.js's pure half runs under node: which bead documents get the picker, the choices a farm document gives a bead's
// project, the form, and what the viewer is told.
func TestAdoptLogicUnderNode(t *testing.T) {
	script := `
var A = require(process.argv[1]), assert = require("assert");
assert.strictEqual(A.adoptable({ id: "a", type: "task", status: "open" }), true, "no parents");
assert.strictEqual(A.adoptable({ id: "a", type: "chore", status: "in_progress", parents: [{ id: "t", type: "task", title: "Slack thread" }] }), true, "a thread parent");
assert.strictEqual(A.adoptable({ id: "a", type: "task", status: "open", parents: [{ id: "p", type: "epic", title: "Plan" }] }), false, "under an epic");
assert.strictEqual(A.adoptable({ id: "a", type: "task", status: "open", parents: [{ id: "p" }] }), false, "a parent we cannot see");
assert.strictEqual(A.adoptable({ id: "a", type: "decision", status: "open" }), false, "not loose work");
assert.strictEqual(A.adoptable({ id: "a", type: "bug", status: "closed" }), false, "closed");
assert.strictEqual(A.adoptable(null), false);
assert.strictEqual(A.adoptable({ type: "task" }), false, "no id");

var doc = {
  plans: [
    { id: "p2", title: "Zinnia", project: "gardenio", status: "open", phases: [{ id: "z1", title: "1. Sow", status: "open" }, { id: "z2", title: "2. Done", status: "closed" }] },
    { id: "p1", title: "aster", project: "gardenio", status: "open", phases: [] },
    { id: "p3", title: "Other", project: "claimlayer", status: "open", phases: [{ id: "o1", title: "x", status: "open" }] },
    { id: "p4", title: "Commons plan", status: "open" },
    { id: "p5", title: "Shut", project: "gardenio", status: "closed" }
  ],
  seeds: [{ id: "s1", title: "Draft", project: "gardenio", status: "open" }, { id: "s2", title: "Elsewhere", project: "claimlayer", status: "open" }]
};
function lines(cs) { return cs.map(function (c) { return c.group + "|" + c.label + "|" + c.plan + "|" + c.phase; }); }
assert.deepStrictEqual(lines(A.choices(doc, "gardenio")), [
  "aster|No phase|p1|",
  "Commons plan|No phase|p4|",
  "Zinnia|No phase|p2|",
  "Zinnia|1. Sow|p2|z1",
  "Draft plans|Draft (draft)|s1|"
], "own project and project-less plans by title, open phases in order, then drafts");
assert.deepStrictEqual(A.choices(doc, "").length, 8, "a bead with no project may go to any open plan or phase");
assert.deepStrictEqual(A.choices(null, "gardenio"), []);
var cs = A.choices(doc, "gardenio");
assert.deepStrictEqual(cs.map(A.where), ["aster", "Commons plan", "Zinnia", "Zinnia \u203a 1. Sow", "Draft (draft)"], "a choice named in full");
assert.deepStrictEqual(A.params({ plan: "p2", phase: "z1" }), { plan: "p2", phase: "z1" });
assert.deepStrictEqual(A.params({ plan: "p1", phase: "" }), { plan: "p1" });
assert.deepStrictEqual(A.outcome(200, { ok: true }, "Zinnia"), { ok: true, text: "Adopted into Zinnia." });
assert.deepStrictEqual(A.outcome(200, { ok: true, already: true }, "Zinnia"), { ok: true, text: "Already in Zinnia." });
assert.deepStrictEqual(A.outcome(409, { ok: false, error: "gd-x is already part of gd-y." }, "Zinnia"), { ok: false, text: "gd-x is already part of gd-y." });
assert.deepStrictEqual(A.outcome(403, null, "Zinnia"), { ok: false, text: "You are not allowed to adopt beads." }, "the operator gate answers plain text");
assert.deepStrictEqual(A.outcome(500, null, "Zinnia"), { ok: false, text: "That did not go through (500)." });

/* handing the bead to a gardener (gd-BXbYNWyOcF) */
var m = A.ask({ id: "gd-x", project: "gardenio", title: 'Fix the "thing"  now' });
assert.ok(/^@gardenio adopt gd-x into a plan\. It is loose work in gardenio \("Fix the 'thing' now"\)\. /.test(m), "a mention of @gardenio, naming the bead, its land and its title: " + m);
assert.ok(m.indexOf("kd dep add gd-x <plan or phase id> --type parent-child") > 0, "it says the one edge to write");
assert.ok(/If no plan fits, say so and leave it in the meadow\.$/.test(m), "and what to do when nothing fits");
assert.ok(A.ask({ id: "gd-y" }).indexOf('It is loose work. Read it') > 0, "no project, no title: still a whole sentence");
assert.strictEqual(A.askPath("/v", { id: "gd-x", project: "a b" }), "/v/garden/c/a%20b/general/say", "said in the land's general channel");
assert.strictEqual(A.askPath("/v", { id: "gd-x" }), "", "no project: no land to ask in");
assert.strictEqual(A.threadPath("", { project: "gardenio" }, "gd-m 1"), "/garden/c/n/gd-m%201", "its node in the workbench");
assert.deepStrictEqual(A.asked(201, "Starting a gardener.", "gd-x", "gd-m1"), { ok: true, text: "Asked a gardener to adopt gd-x into a plan. Starting a gardener.", pick: false });
assert.deepStrictEqual(A.asked(201, "", "gd-x", "gd-m1"), { ok: true, text: "Asked a gardener to adopt gd-x into a plan.", pick: false });
assert.deepStrictEqual(A.asked(404, "", "gd-x", ""), { ok: false, text: "This land has no Commons, so no gardener can be asked.", pick: true }, "no Commons: the picker opens instead");
assert.deepStrictEqual(A.asked(200, "", "gd-x", ""), { ok: false, text: "This land has no Commons, so no gardener can be asked.", pick: true }, "a 200 with no message is another page answering, not a gardener");
assert.deepStrictEqual(A.asked(403, "", "gd-x", "").pick, true, "may not post: the picker");
assert.deepStrictEqual(A.asked(429, "", "gd-x", ""), { ok: false, text: "The Commons is busy. Try again in a moment.", pick: false }, "busy: try again, no picker");
assert.deepStrictEqual(A.asked(500, "", "gd-x", ""), { ok: false, text: "The ask did not go through (500).", pick: false });
`
	runNodeLogic(t, "adopt.js", script)
}

// In Chromium on the fixture farm, an operator adopts a loose meadow bead into a phase with keys alone, and only a loose bead
// is offered it. What is asserted lives in testdata/adopt_browser.js; ADOPT_SHOTS=<dir> keeps its PNGs.
func TestAdoptUnderChromium(t *testing.T) {
	underChromium(t, "adopt_browser.js", func() *httptest.Server {
		return cardWorldAs(t, time.Now, "oidc:matt@example.com", nil)
	})
}
