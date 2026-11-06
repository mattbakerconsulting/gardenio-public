package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/gardenhub"
	"gardenio/controller/internal/oidcauth"
)

// The plan cockpit's reader, its JSON writes and the R7 retirement (Lands workbench-glass spec WB-7, WB-8, §2.6; step
// 5.9, gd-nTPjJ2yllf).

// The reader answers the cockpit /plans/{id} draws: the plan's lifecycle and label, its steps and progress, its
// decisions with their prompts, its narrative's summary; a bead that is not an epic has no cockpit.
func TestGardenPlansReadsTheCockpit(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Fields = map[string]string{"plan_state": "building"}
	g := gardenPlans{NewServer(f, slog.Default(), "", "", "", nil)}
	d, err := g.Plan(context.Background(), "gd-plan1")
	if err != nil {
		t.Fatal(err)
	}
	if d.ID != "gd-plan1" || d.Title != "Plan: ship the /plans page" || !d.Plan || d.State != "building" || d.Project != "gardenio" || d.Status != "in_progress" {
		t.Errorf("head %+v", d)
	}
	if len(d.Steps) != 1 || d.Steps[0].ID != "gd-step1" || d.Steps[0].Title != "Wire the route" || d.Total != 1 || d.Closed != 0 {
		t.Errorf("steps %+v, total %d closed %d", d.Steps, d.Total, d.Closed)
	}
	if len(d.Decisions) != 1 || d.Decisions[0].ID != "gd-dec1" || d.Decisions[0].Resolved || !strings.Contains(d.Decisions[0].Prompt, "Label the epic") {
		t.Errorf("decisions %+v", d.Decisions)
	}
	if d.Decisions[0].Mine || d.Decisions[0].AskedOf != "" {
		t.Errorf("a decision with no operator is asked of no one by name: %+v", d.Decisions[0])
	}
	if d.Narrative.Summary != "Research the viewer, build the cockpit." {
		t.Errorf("summary %q", d.Narrative.Summary)
	}
	if d.Crew == nil || d.Crew.Members == nil {
		t.Errorf("crew %+v: no crew is an empty roster", d.Crew)
	}

	if _, err := g.Plan(context.Background(), "gd-step1"); !errors.Is(err, gardenhub.ErrNotPlan) {
		t.Errorf("a task: %v, want ErrNotPlan", err)
	}
	if _, err := g.Plan(context.Background(), "gd-nothere99"); err == nil {
		t.Error("a bead the daemon does not have: want its error")
	}
}

// A decision says whom it is asked of (gd-e8EAWMUiNw): the viewer's own is "mine", another operator's carries their name,
// and a raw id is never shown as a name.
func TestGardenPlansSaysWhomADecisionAsks(t *testing.T) {
	for _, c := range []struct {
		name, op, display, viewer string
		mine                      bool
		askedOf                   string
	}{
		{"the viewer's own", "oidc:me@example.com", "Me", "me@example.com", true, ""},
		{"another operator's", "oidc:flo@example.com", "Flo", "me@example.com", false, "Flo"},
		{"no name known", "oidc:flo@example.com", "", "", false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := seedPlansFake()
			f.beads["gd-dec1"].OperatedBy, f.beads["gd-dec1"].OperatedByDisplay = c.op, c.display
			g := gardenPlans{NewServer(f, slog.Default(), "", "", "", nil)}
			d, err := g.Plan(oidcauth.ContextWithEmail(context.Background(), c.viewer), "gd-plan1")
			if err != nil {
				t.Fatal(err)
			}
			if len(d.Decisions) != 1 || d.Decisions[0].Mine != c.mine || d.Decisions[0].AskedOf != c.askedOf {
				t.Errorf("decisions %+v, want mine %v asked_of %q", d.Decisions, c.mine, c.askedOf)
			}
		})
	}
}

// Pour lists the plan's project's formulas, each with its step count and the variables its pour asks for.
func TestGardenPlansPourListsFormulas(t *testing.T) {
	plan := planEpicFixture("gd-planX", "gardenio")
	f := planPourFake(plan, formulaBeadFixture("gd-f1", "Security review", "gardenio", map[string]string{
		"steps": stepsJSON(3),
		"vars":  `[{"name":"component","description":"Component name","required":true},{"name":"env","default":"staging","enum":["staging","prod"]}]`,
	}))
	d, err := gardenPlans{NewServer(f, slog.Default(), "", "", "", nil)}.Pour(context.Background(), "gd-planX")
	if err != nil {
		t.Fatal(err)
	}
	if d.Project != "gardenio" || len(d.Formulas) != 1 {
		t.Fatalf("%+v", d)
	}
	x := d.Formulas[0]
	if x.ID != "gd-f1" || x.Title != "Security review" || x.Steps != 3 || len(x.Vars) != 2 {
		t.Fatalf("formula %+v", x)
	}
	if v := x.Vars[0]; v.Name != "component" || !v.Required || v.Note != "Component name" {
		t.Errorf("var %+v", v)
	}
	if v := x.Vars[1]; v.Default != "staging" || len(v.Enum) != 2 {
		t.Errorf("var %+v", v)
	}
	if f.listLastQ == nil || len(f.listLastQ.Labels) != 1 || f.listLastQ.Labels[0] != "project:gardenio" {
		t.Errorf("list query %+v: the plan's project only", f.listLastQ)
	}
}

func jsonPost(target, body string) *http.Request {
	req := operatorPost(target, body)
	req.Header.Set("Accept", "application/json")
	return req
}

func actionJSON(t *testing.T, w *httptest.ResponseRecorder) actionResponse {
	t.Helper()
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type %q, body %s", ct, w.Body.String())
	}
	var res actionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("%v: %s", err, w.Body.String())
	}
	return res
}

// The cockpit's writes answer JSON to a caller that asks for it (the garden's cockpit): ok with the bead it made or
// moved, or the service's message and status; a viewer who may not write gets the gate's refusal as JSON too; and an
// htmx caller still gets its fragment.
func TestPlanWritesAnswerJSON(t *testing.T) {
	f := seedPlansFake()
	f.createIDForReq = func(beadsapi.CreateBeadRequest) string { return "gd-newstep01" }
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, jsonPost("/plans/gd-plan1/steps", "title=Add+tests"))
	if res := actionJSON(t, w); w.Code != 200 || !res.OK || res.Bead != "gd-newstep01" {
		t.Errorf("add step: %d %+v, want ok with the new step", w.Code, res)
	}

	w = httptest.NewRecorder()
	mux.ServeHTTP(w, jsonPost("/plans/gd-plan1/steps", "title=++"))
	if res := actionJSON(t, w); res.OK || res.Status < 400 || res.Error == "" || w.Code != res.Status {
		t.Errorf("an empty title: %d %+v, want the service's refusal", w.Code, res)
	}

	w = httptest.NewRecorder()
	mux.ServeHTTP(w, jsonPost("/plans/gd-plan1/steps/gd-step1/close", ""))
	if res := actionJSON(t, w); w.Code != 200 || !res.OK || res.Bead != "gd-step1" {
		t.Errorf("close step: %d %+v", w.Code, res)
	}

	w = httptest.NewRecorder()
	mux.ServeHTTP(w, jsonPost("/plans/gd-plan1/narrative", "narrative=New+summary"))
	if res := actionJSON(t, w); w.Code != 200 || !res.OK || res.Bead != "gd-plan1" {
		t.Errorf("narrative: %d %+v", w.Code, res)
	}

	viewer := httptest.NewRequest(http.MethodPost, "/plans/gd-plan1/steps", strings.NewReader("title=x"))
	viewer.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	viewer.Header.Set("Accept", "application/json")
	viewer.Header.Set("X-Auth-Request-Email", "v@acme.example")
	viewer = viewer.WithContext(oidcauth.ContextWithRoles(viewer.Context(), []string{oidcauth.RoleViewer}))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, viewer)
	if res := actionJSON(t, w); res.OK || w.Code != http.StatusForbidden {
		t.Errorf("a viewer: %d %+v, want 403 as JSON", w.Code, res)
	}

	w = httptest.NewRecorder()
	mux.ServeHTTP(w, operatorHxPost("/plans/gd-plan1/steps", "title=Another"))
	if ct := w.Header().Get("Content-Type"); w.Code != 200 || !strings.HasPrefix(ct, "text/html") {
		t.Errorf("htmx: %d %q, want the fragment", w.Code, ct)
	}
}

// /plans/{id} and /plans (R7) say "Open it in the garden" to a viewer who reaches /garden (default on), and with the
// redirect flag on answer 302 to the cockpit and to the garden; an htmx swap and the cockpit's partials keep their
// answers, and the flags are R7's own.
func TestGardenPlansRetirement(t *testing.T) {
	newSrv := func(t *testing.T, env map[string]string) http.Handler {
		srv := NewServer(seedPlansFake(), slog.Default(), "/v", "", "", nil)
		if env != nil {
			srv.tunables = newWatcherWithEnv(t, env)
		}
		srv.SetGardenHandler(&gardenhub.Handler{Hub: gardenhub.New(nil, "/v", nil), BasePath: "/v"})
		return edgeAuthMux(srv)
	}
	get := func(h http.Handler, path string, hx bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Auth-Request-Email", "alice@example.com")
		if hx {
			req.Header.Set("HX-Request", "true")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	banner := func(w *httptest.ResponseRecorder) bool {
		return strings.Contains(w.Body.String(), `id="garden-banner"`)
	}

	on := newSrv(t, nil)
	for path, want := range map[string]string{
		"/plans/gd-plan1":         `href="/v/garden?plan=gd-plan1"`,
		"/plans?project=gardenio": `href="/v/garden"`,
	} {
		if w := get(on, path, false); w.Code != http.StatusOK || !banner(w) || !strings.Contains(w.Body.String(), want) {
			t.Errorf("%s: %d, banner with %s missing", path, w.Code, want)
		}
	}
	if w := get(newSrv(t, map[string]string{gardenPlansBannerEnv: "false"}), "/plans/gd-plan1", false); banner(w) {
		t.Error("plans banner flag off: no banner")
	}
	if w := get(newSrv(t, map[string]string{gardenBeadRedirectEnv: "true"}), "/plans/gd-plan1", false); w.Code != http.StatusOK {
		t.Errorf("only the bead redirect on: %d, want the cockpit page", w.Code)
	}

	redirect := newSrv(t, map[string]string{gardenPlansRedirectEnv: "true"})
	for path, want := range map[string]string{
		"/plans/gd-plan1":                    "/v/garden?plan=gd-plan1",
		"/plans/gd-plan1?at=gardenio&bead=1": "/v/garden?at=gardenio&plan=gd-plan1",
		"/plans":                             "/v/garden",
		"/plans?project=gardenio&view=tree":  "/v/garden",
	} {
		if w := get(redirect, path, false); w.Code != http.StatusFound || w.Header().Get("Location") != want {
			t.Errorf("%s: %d to %q, want 302 to %q", path, w.Code, w.Header().Get("Location"), want)
		}
	}
	if w := get(redirect, "/plans/gd-plan1", true); w.Code != http.StatusOK {
		t.Errorf("an htmx swap with the redirect on: %d, want the page", w.Code)
	}
	if w := get(redirect, "/plans/gd-plan1/narrative", false); w.Code != http.StatusOK {
		t.Errorf("a cockpit partial with the redirect on: %d, want the fragment", w.Code)
	}
}
