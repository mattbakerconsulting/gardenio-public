package gardenhub

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// The plan cockpit (Lands workbench-glass spec WB-7 and WB-8; step 5.9, gd-nTPjJ2yllf).

type fakePlans struct {
	asked []string
	doc   func(id string) *PlanDoc
	pour  func(id string) *PourDoc
	err   error
}

func (f *fakePlans) Plan(_ context.Context, id string) (*PlanDoc, error) {
	f.asked = append(f.asked, id)
	if f.err != nil {
		return nil, f.err
	}
	if d := f.doc(id); d != nil {
		return d, nil
	}
	return nil, ErrNotPlan
}

func (f *fakePlans) Pour(_ context.Context, id string) (*PourDoc, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.pour == nil {
		return &PourDoc{}, nil
	}
	return f.pour(id), nil
}

func yes(*http.Request) bool { return true }

// GET /garden/plan/{id} asks the reader for the plan, answers a bead with no cockpit or one the viewer cannot see as a 404
// and a daemon failure as a 502, says which writes the panel may offer and caps what it sends.
func TestPlanViewReadsThePlan(t *testing.T) {
	steps := func(n int) []PlanStep {
		out := make([]PlanStep, n)
		for i := range out {
			out[i] = PlanStep{ID: "gd-s" + strings.Repeat("x", i+1), Title: "step"}
		}
		return out
	}
	f := &fakePlans{doc: func(id string) *PlanDoc {
		if id != "gd-plan" {
			return nil
		}
		return &PlanDoc{ID: id, Title: "The plan", Plan: true, State: "building",
			Phases:    []PlanPhase{{ID: "gd-ph1", Title: "One", Steps: steps(planStepsMax - 1)}, {ID: "gd-ph2", Title: "Empty"}},
			Steps:     steps(3),
			Decisions: []PlanDecision{{ID: "gd-d", Title: "Which?", Prompt: strings.Repeat("why ", 400)}},
			Narrative: PlanNarrative{Summary: "short", Docs: []PlanNarrativeDoc{{ID: "gd-doc", Content: strings.Repeat("x", planDocMax+5)}}},
		}
	}}
	h := &Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v", Plans: f}

	code, doc := getQueue(t, h, "/garden/plan/gd-plan")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	if doc["id"] != "gd-plan" || doc["plan"] != true || doc["state"] != "building" {
		t.Errorf("doc %v", doc)
	}
	if doc["edit"] != nil || doc["approve"] != nil {
		t.Errorf("no CanEdit or CanApproveEscalation offers no writes: edit %v approve %v", doc["edit"], doc["approve"])
	}
	phases := doc["phases"].([]any)
	one, empty := phases[0].(map[string]any), phases[1].(map[string]any)
	if n := len(one["steps"].([]any)); n != planStepsMax-1 {
		t.Errorf("phase one: %d steps", n)
	}
	if s, ok := empty["steps"].([]any); !ok || len(s) != 0 {
		t.Errorf("an empty phase sends [] steps, got %v", empty["steps"])
	}
	if n := len(doc["steps"].([]any)); n != 1 || doc["more"] != true {
		t.Errorf("the steps are capped at %d across the phases: %d unphased, more %v", planStepsMax, n, doc["more"])
	}
	if p := doc["decisions"].([]any)[0].(map[string]any)["prompt"].(string); len([]rune(p)) > planTextMax {
		t.Errorf("prompt not clipped: %d runes", len([]rune(p)))
	}
	nd := doc["narrative"].(map[string]any)["docs"].([]any)[0].(map[string]any)
	if nd["clipped"] != true || len([]rune(nd["content"].(string))) > planDocMax {
		t.Errorf("a long doc is cut and says so: clipped %v", nd["clipped"])
	}

	h.CanEdit, h.CanApproveEscalation = yes, yes
	if _, doc = getQueue(t, h, "/garden/plan/gd-plan"); doc["edit"] != true || doc["approve"] != true {
		t.Errorf("edit %v approve %v", doc["edit"], doc["approve"])
	}

	for _, c := range []struct {
		h    *Handler
		path string
		want int
	}{
		{h, "/garden/plan/gd-task", 404},
		{&Handler{Plans: &fakePlans{err: &beadsapi.APIError{StatusCode: 403}}}, "/garden/plan/gd-plan", 404},
		{&Handler{Plans: &fakePlans{err: &beadsapi.APIError{StatusCode: 404}}}, "/garden/plan/gd-plan", 404},
		{&Handler{Plans: &fakePlans{err: errors.New("boom")}}, "/garden/plan/gd-plan", 502},
		{&Handler{}, "/garden/plan/gd-plan", 503},
		{h, "/garden/plan/" + strings.Repeat("x", 201), 400},
	} {
		if code, _ := getQueue(t, c.h, c.path); code != c.want {
			t.Errorf("%s: %d, want %d", c.path, code, c.want)
		}
	}
}

// GET /garden/plan/{id}/pour lists the formulas only for a viewer who may write here (the pour is operator-gated).
func TestPlanPourAsksAnOperator(t *testing.T) {
	f := &fakePlans{doc: func(string) *PlanDoc { return nil }, pour: func(id string) *PourDoc {
		return &PourDoc{Project: "gardenio", Formulas: []PourFormula{{ID: "gd-f1", Title: "Release", Steps: 4, Vars: []PourVar{{Name: "version", Required: true}}}}}
	}}
	h := &Handler{Plans: f}
	if code, _ := getQueue(t, h, "/garden/plan/gd-plan/pour"); code != http.StatusForbidden {
		t.Errorf("no CanEdit: %d, want 403", code)
	}
	h.CanEdit = yes
	code, doc := getQueue(t, h, "/garden/plan/gd-plan/pour")
	if code != 200 || doc["plan"] != "gd-plan" || len(doc["formulas"].([]any)) != 1 {
		t.Fatalf("%d %v", code, doc)
	}
	f.pour = func(string) *PourDoc { return &PourDoc{} }
	if _, doc = getQueue(t, h, "/garden/plan/gd-plan/pour"); doc["formulas"] == nil {
		t.Error("no formulas is [], not null")
	}
	h.Plans = &fakePlans{err: ErrNotPlan}
	if code, _ := getQueue(t, h, "/garden/plan/gd-task/pour"); code != 404 {
		t.Errorf("not a plan: %d", code)
	}
}

// subscribeAll is one panel under many ids: any of them moves it, closing it twice or after closeAll is safe, and Stats
// counts it once.
func TestMovesSubscribeAll(t *testing.T) {
	var m moves
	ch, stop := m.subscribeAll([]string{"gd-a", "gd-b", "gd-a", ""})
	_, stop2 := m.subscribe("gd-b")
	if n := m.count(); n != 2 {
		t.Errorf("count %d, want 2 panels", n)
	}
	m.touch("gd-b")
	select {
	case <-ch:
	default:
		t.Error("a move of gd-b must signal the cockpit")
	}
	m.touch("gd-a", "gd-b")
	m.touch("gd-a")
	<-ch
	select {
	case <-ch:
		t.Error("one pending signal says it all")
	default:
	}
	stop()
	stop()
	if n := m.count(); n != 1 {
		t.Errorf("count %d after stop, want 1", n)
	}
	_, stop3 := m.subscribeAll([]string{"gd-x", "gd-y"})
	m.closeAll()
	stop3()
	stop2()
	_, stop4 := m.subscribeAll(nil)
	stop4()
	stop4()
}

// GET /garden/plan/{id}/events reads the plan as the viewer (refused: 404, 502, 503), then says "moved" with the plan's id
// when any bead the cockpit draws moves, a step as much as the plan, and nothing for a bead it does not draw.
func TestPlanEventsStream(t *testing.T) {
	src := newFake()
	hub := New(src, "/v", nil)
	hub.Start()
	defer hub.Close()
	for start := time.Now(); !hub.Stats().Upstream && time.Since(start) < 5*time.Second; {
		time.Sleep(5 * time.Millisecond)
	}
	plans := &fakePlans{doc: func(id string) *PlanDoc {
		if id != "gd-plan" {
			return nil
		}
		return &PlanDoc{ID: id, Phases: []PlanPhase{{ID: "gd-ph", Steps: []PlanStep{{ID: "gd-step"}}}}, Decisions: []PlanDecision{{ID: "gd-dec"}}}
	}}
	for _, c := range []struct {
		h    *Handler
		id   string
		want int
	}{
		{&Handler{Hub: hub, Plans: plans}, "gd-task", 404},
		{&Handler{Hub: hub, Plans: &fakePlans{err: errors.New("boom")}}, "gd-plan", 502},
		{&Handler{Hub: hub}, "gd-plan", 503},
		{&Handler{Plans: plans}, "gd-plan", 503},
	} {
		m := http.NewServeMux()
		c.h.Register(m)
		rec := httptest.NewRecorder()
		m.ServeHTTP(rec, httptest.NewRequest("GET", "/garden/plan/"+c.id+"/events", nil))
		if rec.Code != c.want {
			t.Errorf("%s: %d, want %d", c.id, rec.Code, c.want)
		}
	}
	if n := hub.moves.count(); n != 0 {
		t.Errorf("a refused stream must not stay subscribed: %d panels", n)
	}

	mux := http.NewServeMux()
	(&Handler{Hub: hub, BasePath: "/v", Plans: plans}).Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/garden/plan/gd-plan/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for hub.moves.count() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := hub.moves.count(); n != 2 {
		t.Fatalf("the stream holds the plan and its beads: %d subscriptions", n)
	}
	next := func(what string) string {
		timeout := time.After(5 * time.Second)
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatal("stream ended")
				}
				if l == "event: moved" {
					return <-lines
				}
			case <-timeout:
				t.Fatalf("no moved event within 5s for %s", what)
			}
		}
	}
	src.events <- ev("beads.comment.added", `{"comment":{"bead_id":"gd-other"}}`)
	src.events <- ev("beads.bead.updated", `{"bead":{"id":"gd-step"}}`)
	if got := next("a step"); got != `data: {"id":"gd-plan"}` {
		t.Errorf("moved data = %q, want the plan's id", got)
	}
	src.events <- ev("beads.bead.closed", `{"bead":{"id":"gd-dec"}}`)
	next("a decision")
	src.events <- ev("beads.dependency.added", `{"dependency":{"bead_id":"gd-new","depends_on_id":"gd-plan","type":"parent-child"}}`)
	next("a step added to the plan")
	cancel()
	deadline = time.Now().Add(5 * time.Second)
	for hub.moves.count() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := hub.moves.count(); n != 0 {
		t.Errorf("a closed stream must unsubscribe: %d panels", n)
	}
}

// The page inlines plan.js after workbench.js, and plan.js registers the plan kind.
func TestPageCarriesPlan(t *testing.T) {
	if i, j := strings.Index(pageHTML, `{{asset "workbench.js"}}`), strings.Index(pageHTML, `{{asset "plan.js"}}`); j < 0 || j < i {
		t.Fatalf("plan.js must be inlined after workbench.js: %d, %d", i, j)
	}
	mux := http.NewServeMux()
	(&Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v"}).Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/garden", nil))
	for _, want := range []string{"var PLAN = ", "W.register('plan',", "BEAD.mdInto(view, BEAD.markdown(text))"} {
		if !strings.Contains(inlined(t, rec.Body.String()), want) {
			t.Errorf("the page lacks %q", want)
		}
	}
}

// plan.js's pure half runs under node: where the plan stands, its levers by lifecycle, the step groups and their words,
// each step's and phase's writes, what the plan waits on, the crew, and the pour's body and checks.
func TestPlanLogicUnderNode(t *testing.T) {
	script := `
var P = require(process.argv[1]), assert = require("assert");
assert.strictEqual(P.url("gd-a b"), "/garden/plan/gd-a%20b");
assert.strictEqual(P.path("gd-p", "/steps"), "/plans/gd-p/steps");

assert.strictEqual(P.stateLine({ plan: false }), "An epic, not yet a plan.");
assert.strictEqual(P.stateLine({ plan: true, state: "ready" }), "Ready: waiting on the owner to approve the build.");
assert.strictEqual(P.stateLine({ plan: true, state: "", status: "open" }), "Not in the plan lifecycle.");
assert.strictEqual(P.stateLine({ plan: true, state: "", status: "closed" }), "Closed.");
var now = 100000;
assert.strictEqual(P.progress({ total: 12, closed: 7, pct: 58, project: "gardenio", last_change: now - 3 * 3600 }, now), "7 of 12 steps closed (58%) · gardenio · changed 3h ago");
assert.strictEqual(P.progress({ total: 0, closed: 0, pct: 0 }, now), "No steps yet");

function acts(d) { return P.levers(d).map(function (v) { return v.act; }); }
assert.deepStrictEqual(acts({ id: "gd-p", plan: true, state: "building" }), [], "no edit, no levers");
assert.deepStrictEqual(acts({ id: "gd-p", edit: true, plan: false }), ["mark"]);
assert.deepStrictEqual(acts({ id: "gd-p", edit: true, plan: true, state: "draft" }), ["ready", "abandon"]);
assert.deepStrictEqual(acts({ id: "gd-p", edit: true, plan: true, state: "ready" }), ["build", "draft", "abandon"]);
assert.deepStrictEqual(acts({ id: "gd-p", edit: true, plan: true, state: "building" }), ["complete", "abandon"]);
assert.deepStrictEqual(acts({ id: "gd-p", edit: true, plan: true, state: "" }), ["complete", "abandon"], "complete is legal from unset");
assert.deepStrictEqual(acts({ id: "gd-p", edit: true, plan: true, state: "complete" }), ["reopen"]);
assert.deepStrictEqual(acts({ id: "gd-p", edit: true, plan: true, state: "", status: "closed" }), ["reopen"]);
var ready = P.levers({ id: "gd-p", edit: true, plan: true, state: "draft" })[0];
assert.strictEqual(ready.path, "/plans/gd-p/state"); assert.deepStrictEqual(ready.params, { state: "ready" });
assert.ok(P.levers({ id: "gd-p", edit: true, plan: true, state: "building" }).every(function (v) { return v.arm; }), "complete and abandon arm first");
var ab = P.levers({ id: "gd-p", edit: true, plan: true, state: "building", title: "A plan whose title runs on well past forty characters" }).pop();
assert.ok(ab.danger && ab.arm === "Tap again to abandon “A plan whose title runs on well past fo…”", "Abandon is danger and names the plan, cut at 40: " + ab.arm);

var d = { id: "gd-p", edit: true, plan: true, state: "building", phases: [{ id: "gd-ph1", title: "One", steps: [{ id: "gd-s1" }] }, { id: "gd-ph2", steps: [] }], steps: [{ id: "gd-s9" }] };
assert.deepStrictEqual(P.groups(d).map(function (g) { return g.title + ":" + g.steps.length; }), ["One:1", "gd-ph2:0", "Other steps:1"]);
assert.deepStrictEqual(P.groups({ steps: [] }).map(function (g) { return g.title; }), ["Steps"], "no phases: one group, even empty");
assert.deepStrictEqual(P.groups({ phases: [{ id: "gd-ph" }] }).map(function (g) { return g.title; }), ["gd-ph"], "phases only: no Other steps");

assert.deepStrictEqual(P.stepLine({ status: "in_progress", assignee: "mo", mr: { state: "opened" }, blocked_by: [{ id: "gd-b" }], updated: now - 600 }, now),
  ["in progress", "mo", "MR opened", "waits on 1 bead", "10m ago"]);
assert.deepStrictEqual(P.stepLine({ status: "open", blocked_by: [{ id: "gd-e", escalation: true }] }, now), ["open", "waits on an escalation"]);
assert.deepStrictEqual(P.stepLine({ status: "closed", closed: true, blocked_by: [{ id: "gd-b" }, { id: "gd-c" }] }, now), ["closed"], "a closed step waits on nothing");

function sv(s, first, last, dd) { return P.stepVerbs(dd || d, s, first, last).map(function (v) { return v.act; }); }
assert.deepStrictEqual(sv({ id: "gd-s1" }, true, true), ["dispatch", "rename", "close"], "Close, the danger verb, goes last");
assert.deepStrictEqual(sv({ id: "gd-s1" }, false, false), ["dispatch", "rename", "up", "down", "close"]);
assert.deepStrictEqual(sv({ id: "gd-s1", closed: true }, false, false), ["reopen"]);
assert.deepStrictEqual(sv({ id: "gd-s1" }, true, false, { id: "gd-p", edit: true, plan: true, state: "complete" }), ["rename", "down", "close"], "no dispatch on a finished plan");
assert.deepStrictEqual(sv({ id: "gd-s1" }, true, true, { id: "gd-p", plan: true }), [], "no edit, no writes");
assert.strictEqual(P.stepVerbs(d, { id: "gd-s 1" }, false, true)[2].path, "/plans/gd-p/steps/gd-s%201/reorder?dir=up");
var sc = P.stepVerbs(d, { id: "gd-s1", short_title: "Ship the thing" }, true, true).pop();
assert.ok(sc.danger && sc.arm === "Tap again to close “Ship the thing”", "the step Close is danger and its confirm names the step: " + sc.arm);
assert.strictEqual(P.stepVerbs(d, { id: "gd-s1" }, true, true).pop().arm, "Tap again to close gd-s1", "no title: the id");
assert.deepStrictEqual(P.phaseVerbs(d, d.phases[0], true, false).map(function (v) { return v.path; }), ["/plans/gd-p/phases/gd-ph1/reorder?dir=down"]);
assert.deepStrictEqual(P.phaseVerbs(d, d.phases[0], true, true), []);

var w = P.waits({ decisions: [{ id: "gd-d1", title: "Which?", since: 5 }, { id: "gd-d2", resolved: true }], escalations: [{ id: "gd-e1", operation: "gitlab_mr_merge" }],
  steps: [{ id: "gd-s1", title: "Ship", blocked_by: [{ id: "gd-b1", title: "Fix" }, { id: "gd-e1", escalation: true }] }, { id: "gd-s2", closed: true, blocked_by: [{ id: "gd-b2" }] }] });
assert.deepStrictEqual(w.map(function (x) { return x.id + "|" + x.text + "|" + x.human; }),
  ["gd-d1|decision waiting on an answer|true", "gd-e1|gitlab mr merge waiting on approval|true", "gd-b1|Ship waits on|false"]);
/* steps that wait on one bead are one row, grouped by the blocker: the first step named, the rest counted (gd-tPd6ljOAsH) */
w = P.waits({ steps: [{ id: "gd-s1", title: "Step 1", blocked_by: [{ id: "gd-b1" }, { id: "gd-b1" }] }, { id: "gd-s2", title: "Step 2", blocked_by: [{ id: "gd-b1" }, { id: "gd-b2" }] },
  { id: "gd-s3", title: "Step 3", blocked_by: [{ id: "gd-b1" }] }, { id: "gd-s4", closed: true, blocked_by: [{ id: "gd-b2" }] }, { id: "gd-s5", title: "Step 5", blocked_by: [{ id: "gd-b2" }] }] });
assert.deepStrictEqual(w.map(function (x) { return x.id + "|" + x.text; }), ["gd-b1|Step 1 and 2 more steps wait on", "gd-b2|Step 2 and 1 more step wait on"]);
/* whom an ask waits on, when the plan says: the viewer, an operator by name, the escalation's approver */
w = P.waits({ decisions: [{ id: "gd-d1", mine: true }, { id: "gd-d3", asked_of: "Flo" }], escalations: [{ id: "gd-e1", operation: "gitlab_mr_merge", audience: "desk" }, { id: "gd-e2", audience: "owner" }] });
assert.deepStrictEqual(w.map(function (x) { return x.text; }), ["decision asked of you", "decision asked of Flo", "gitlab mr merge for the merge desk to approve", "escalation for the owner to approve"]);
assert.strictEqual(P.askedOf({}), "");
assert.strictEqual(P.approver({ audience: "operator" }), "an operator");

assert.deepStrictEqual(P.crewLine({ bucket: "working", role: "crew", step: { id: "gd-s1", title: "Ship" }, step_n: 3, since: now - 120 }, now), ["working", "crew", "on #3 Ship", "2m ago"]);
assert.deepStrictEqual(P.crewLine({ state: "spawning" }, now), ["spawning"]);
assert.strictEqual(P.crewSum({ total: 3, working: 2, failed: 1 }), "3 agents: 2 working, 1 failed.");
assert.strictEqual(P.crewSum({ total: 1, done: 1 }), "1 agent: 1 done.");
assert.strictEqual(P.crewSum(null), "No crew yet.");

assert.deepStrictEqual(P.pourBody("gd-f", { version: " 1.2 ", empty: "  " }, " Release 1.2 ", true), { formula_id: "gd-f", vars: { version: "1.2" }, dry_run: true, phase_title: "Release 1.2" });
assert.deepStrictEqual(P.pourBody("gd-f", {}, "", false), { formula_id: "gd-f", vars: {}, dry_run: false });
var f = { vars: [{ name: "version", required: true }, { name: "env", required: true, default: "staging" }, { name: "note" }] };
assert.deepStrictEqual(P.missing(f, {}), ["version"]);
assert.deepStrictEqual(P.missing(f, { version: "1" }), []);
assert.deepStrictEqual(P.previewLines([{ title: "A", children: [{ id: "gd-x", type: "epic" }] }, { title: "B", type: "task" }]),
  [{ depth: 0, text: "A" }, { depth: 1, text: "gd-x (epic)" }, { depth: 0, text: "B" }]);
`
	runNodeLogic(t, "plan.js", script)
}

// fixturePlans is the card world's cockpit reader (card_browser_test.go): plan A as the farm fixture draws it (farm_test.go
// fixtureWorld), its three phases and their steps, the step under no phase, the decision its Rollout phase asks, an
// escalation that holds up the cutover, its crew and its narrative. Any other id has no cockpit. Times are against now.
func fixturePlans(now time.Time) PlanReader {
	ago := func(d time.Duration) int64 { return now.Add(-d).Unix() }
	return &fakePlans{doc: func(id string) *PlanDoc {
		if id != "gd-planA" {
			return nil
		}
		return &PlanDoc{
			ID: "gd-planA", Title: "Edge IdP rollout", Project: "gardenio", Status: "in_progress", State: "building", Plan: true,
			Total: 9, Closed: 2, Pct: 22, LastChange: ago(20 * time.Minute),
			Phases: []PlanPhase{
				{ID: "gd-A1", Number: 1, Title: "Foundations", Status: "in_progress", Total: 4, Done: 2, Steps: []PlanStep{
					{ID: "gd-A1a", Number: 1, Title: "Token exchange for agent pods", Type: "feature", Status: "in_progress", Priority: 2, Assignee: "mo",
						MR: &PlanMR{State: "opened", URL: "https://example.com/mr/412"}, Updated: ago(20 * time.Minute)},
					{ID: "gd-A1b", Number: 2, Title: "Staging soak test for exchange", Type: "task", Status: "in_progress", Priority: 2, Assignee: "mo", Updated: ago(2 * time.Hour)},
					{ID: "gd-A1x", Number: 3, Title: "Pick the IdP", Type: "task", Status: "closed", Closed: true, Priority: 2, Updated: ago(30 * time.Hour)},
					{ID: "gd-A1y", Number: 4, Title: "Staging IdP behind the edge", Type: "task", Status: "closed", Closed: true, Priority: 2,
						MR: &PlanMR{State: "merged", URL: "https://example.com/mr/405"}, Updated: ago(26 * time.Hour)},
				}},
				{ID: "gd-A2", Number: 2, Title: "Rollout", Status: "open", Total: 4, Steps: []PlanStep{
					{ID: "gd-A2a", Number: 5, Title: "Flip production to the new IdP", Type: "task", Status: "open", Priority: 0,
						BlockedBy: []PlanBlocker{{ID: "gd-A1a", Title: "Token exchange for agent pods"}, {ID: "gd-A2esc", Title: "Merge MR !412", Escalation: true, Operation: "gitlab_mr_merge"}}, Updated: ago(5 * time.Hour)},
					{ID: "gd-A2b", Number: 6, Title: "Announce the cutover window", Type: "task", Status: "open", Priority: 2, Updated: ago(6 * time.Hour)},
					{ID: "gd-A2c", Number: 7, Title: "Refresh fails after clock skew", Type: "bug", Status: "open", Priority: 0, Updated: ago(4 * time.Hour)},
					{ID: "gd-A2f", Number: 8, Title: "Where does the chat live?", Type: "decision", Status: "open", Priority: 2, Updated: ago(time.Hour)},
				}},
				{ID: "gd-A3", Number: 3, Title: "Cutover", Status: "closed", Closed: true, Steps: []PlanStep{}},
			},
			Steps: []PlanStep{{ID: "gd-A9", Number: 9, Title: "Rotate the staging signing key", Type: "chore", Status: "open", Priority: 2, Updated: ago(8 * time.Hour)}},
			Decisions: []PlanDecision{{ID: "gd-A2f", Title: "Where does the chat live?", Prompt: "The Porch or the Commons? The Commons has threads; the Porch is closer to the map.",
				Status: "open", RequestedBy: "g-ask", Since: ago(time.Hour), Mine: true, Options: []QueueOption{{ID: "commons", Text: "The Commons"}, {ID: "porch", Text: "The Porch"}}}},
			Escalations: []PlanEscalation{{ID: "gd-A2esc", Title: "Merge MR !412", Operation: "gitlab_mr_merge", Audience: "owner", Since: ago(3 * time.Hour)}},
			Crew: &PlanCrew{Total: 3, Working: 2, Done: 1, Members: []PlanCrewMember{
				{ID: "gd-gWork1", Name: "g-work-1", Role: "crew", State: "working", Bucket: "working", Live: true, Step: &QueueRef{ID: "gd-A1a", Title: "Token exchange for agent pods"}, StepN: 1, Since: ago(2 * time.Minute)},
				{ID: "gd-gWork2", Name: "g-work-2", Role: "crew", State: "working", Bucket: "working", Live: true, Step: &QueueRef{ID: "gd-A1b", Title: "Staging soak test for exchange"}, StepN: 2, Since: ago(9 * time.Minute)},
				{ID: "gd-gBye", Name: "g-bye", Role: "crew", State: "done", Bucket: "done", Step: &QueueRef{ID: "gd-A1y", Title: "Staging IdP behind the edge"}, StepN: 4, Outcome: "merged", Since: ago(26 * time.Hour)},
			}},
			Narrative: PlanNarrative{Summary: "Roll the edge IdP out.\n\nSecond line, with **the why**: agents get short-lived tokens.", Bundle: "gd-Abun", BundleTitle: "Edge IdP plan",
				Docs: []PlanNarrativeDoc{{ID: "gd-Adoc", Headline: "Design", Format: "markdown", Content: "## Shape\n\n- Exchange at the edge\n- Refresh in the pod"}}},
		}
	}, pour: func(string) *PourDoc {
		return &PourDoc{Project: "gardenio", Formulas: []PourFormula{{ID: "gd-fRel", Title: "Release train", Steps: 4, Vars: []PourVar{{Name: "version", Required: true, Note: "calver"}}}}}
	}}
}

// The plan cockpit in a real Chromium: plan A's face offers Open the cockpit, which opens the panel on ?plan=; it shows
// where the plan stands, what it waits on, its steps by phase with their words, the decision with the Mailbox's answer
// box, the crew and the narrative's markdown; a step opens its bead here and Back returns; Escape closes. What is asserted
// lives in testdata/plan_browser.js.
func TestPlanUnderChromium(t *testing.T) {
	underChromium(t, "plan_browser.js", func() *httptest.Server { return cardWorld(t, time.Now) })
}
