package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/gardenhub"
	"gardenio/controller/internal/oidcauth"
)

// The project panel's reader, the config editor's JSON answer and the R5 and R8 retirements (Lands workbench-glass spec
// WB-10, §2.6; step 5.11, gd-fT15vJniWQ).

// projectPanelFake answers each list the reader asks by what it asks for (its route), each call with fresh beads, so the
// sections, read side by side, never share one.
type projectPanelFake struct {
	*fakeDaemon
	lists map[string][]beadsapi.BeadDetail
	errs  map[string]error
}

func projectPanelRoute(q beadsapi.ListBeadsQuery) string {
	switch {
	case slices.Contains(q.Types, "agent"):
		return "agents"
	case slices.Contains(q.Types, "trigger"), slices.Contains(q.Types, "schedule"):
		return "gardeners"
	case slices.Contains(q.Types, "decision"):
		return "decisions"
	case slices.Contains(q.Types, "project"):
		return "projects"
	case slices.Contains(q.Types, "account"):
		return "accounts"
	case len(q.Kinds) > 0:
		return "summary"
	case len(q.Types) == 0 && slices.Equal(q.Statuses, []string{"in_progress"}):
		return "growing"
	case len(q.Types) == 0 && slices.Equal(q.Statuses, projectRecentStatuses):
		return "recent"
	}
	return "other"
}

func (f *projectPanelFake) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	r := projectPanelRoute(q)
	if err := f.errs[r]; err != nil {
		return nil, err
	}
	var out []*beadsapi.BeadDetail
	for _, b := range f.lists[r] {
		cp := b
		cp.Fields = map[string]string{}
		for k, v := range b.Fields {
			cp.Fields[k] = v
		}
		out = append(out, &cp)
	}
	return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
}

func newProjectPanelFake() *projectPanelFake {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{"gardenio": {Name: "gardenio", BeadID: "gd-proj-gardenio", SchedulesEnabled: false, TriggersEnabled: true}}
	f.beads["gd-proj-gardenio"] = &beadsapi.BeadDetail{ID: "gd-proj-gardenio", Type: "project", Title: "Project: gardenio", Status: "open", Fields: map[string]string{
		"git_url": "https://github.com/acme/gardenio", "image": "agent:1", "memory_limit": "lots", "docker": "true", "secrets": `[{"env":"TOKEN","secret":"gh","key":"token"}]`}}
	f.accountsByProjectResp = []beadsapi.AccountInfo{{Name: "anthropic-main", Provider: "anthropic", Model: "opus", Status: "active"}}
	f.escalationsResp = []beadsapi.EscalationDetail{
		{Issue: &beadsapi.BeadDetail{ID: "gd-esc-mine", Type: "escalation", Title: "Merge it", Status: "open", Labels: []string{"project:gardenio"}, Fields: map[string]string{"operation_type": "gitlab_mr_merge"}}},
		{Issue: &beadsapi.BeadDetail{ID: "gd-esc-other", Type: "escalation", Title: "Not ours", Status: "open", Labels: []string{"project:other"}}},
	}
	p := "project:gardenio"
	return &projectPanelFake{fakeDaemon: f, errs: map[string]error{}, lists: map[string][]beadsapi.BeadDetail{
		"agents": {{ID: "gd-agent-1", Type: "agent", Title: "crew-1", Status: "in_progress", Labels: []string{p},
			Fields: map[string]string{"agent": "crew-1", "role": "crew", "agent_state": "working", "task_id": "gd-task-1", "task_title": "Do it"}}},
		"gardeners": {{ID: "gd-trig-1", Type: "trigger", Title: "guard the plan", Status: "open", Labels: []string{"role:gardener", p}, Fields: map[string]string{"role": "gardener"}}},
		"decisions": {
			{ID: "gd-dec-mine", Type: "decision", Title: "Which first?", Status: "open", Labels: []string{p}, Fields: map[string]string{"prompt": "Pick", "options": `[{"id":"a","label":"A"}]`}},
			{ID: "gd-dec-other", Type: "decision", Title: "Elsewhere", Status: "open", Labels: []string{"project:other"}},
		},
		"projects": {{ID: "gd-proj-gardenio", Type: "project", Title: "Project: gardenio", Status: "open", Fields: map[string]string{}}},
		"accounts": {{ID: "gd-acct-1", Type: "account", Title: "anthropic-main", Status: "open", Fields: map[string]string{"provider": "anthropic", "daily_cost_usd": "4.2", "weekly_cost_usd": "30", "total_cost_usd": "400"}}},
		"growing":  {{ID: "gd-task-1", Type: "task", Title: "Do it", Status: "in_progress", Labels: []string{p}}},
		"recent": {
			{ID: "gd-task-1", Type: "task", Title: "Do it", Status: "in_progress", Labels: []string{p}},
			{ID: "gd-task-0", Type: "task", Title: "Done it", Status: "closed", Labels: []string{p}},
		},
	}}
}

// The reader sends the project's sections from the hub's own builders: its agents, gardeners (and the config's paused
// schedules), accounts with their spend, what grows and what moved, its own decisions and escalations (another project's
// left out), and its config with the hub's findings; an operator land has no purse. A section the daemon did not answer is
// named in Partial and the rest still come; a name that is no project and holds nothing is not found.
func TestGardenProjectReadsTheProject(t *testing.T) {
	f := newProjectPanelFake()
	srv := newHubServer(f.fakeDaemon)
	srv.daemon = f

	d, err := gardenProjects{srv}.Project(context.Background(), "gardenio")
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "gardenio" || len(d.Partial) != 0 || d.Purse != nil {
		t.Fatalf("doc %+v", d)
	}
	if len(d.Agents) != 1 || d.Agents[0].Name != "crew-1" || d.Agents[0].State != "working" || d.Agents[0].TaskID != "gd-task-1" {
		t.Errorf("agents %+v", d.Agents)
	}
	if len(d.Gardeners) != 1 || d.Gardeners[0].ID != "gd-trig-1" || !d.SchedulesPaused || d.TriggersPaused {
		t.Errorf("gardeners %+v, paused %v/%v", d.Gardeners, d.SchedulesPaused, d.TriggersPaused)
	}
	if len(d.Accounts) != 1 || d.Accounts[0].Name != "anthropic-main" || d.Spend == nil {
		t.Errorf("accounts %+v spend %+v", d.Accounts, d.Spend)
	}
	if len(d.Growing) != 1 || len(d.Recent) != 2 || d.Recent[1].Status != "closed" {
		t.Errorf("growing %+v recent %+v", d.Growing, d.Recent)
	}
	if len(d.Decisions) != 1 || d.Decisions[0].ID != "gd-dec-mine" || len(d.Decisions[0].Options) != 1 {
		t.Errorf("decisions %+v: the project's own, with its options", d.Decisions)
	}
	if len(d.Escalations) != 1 || d.Escalations[0].ID != "gd-esc-mine" {
		t.Errorf("escalations %+v: the project's own", d.Escalations)
	}
	c := d.Config
	if c == nil || c.BeadID != "gd-proj-gardenio" || c.GitURL != "https://github.com/acme/gardenio" || !c.Docker || len(c.Secrets) != 1 || c.Secrets[0].Env != "TOKEN" {
		t.Fatalf("config %+v", c)
	}
	found := false
	for _, x := range c.Findings {
		found = found || (x.Field == "memory_limit" && x.Severity == "error")
	}
	if !found {
		t.Errorf("findings %+v: a bad quantity is the hub's error", c.Findings)
	}
	if !d.CanEdit {
		t.Error("a roleless session may edit the config, as the editor allows it")
	}

	// a viewer with roles but not admin may not edit
	ctx := oidcauth.ContextWithRoles(context.Background(), []string{oidcauth.RoleViewer})
	if d, _ = (gardenProjects{srv}).Project(ctx, "gardenio"); d.CanEdit {
		t.Error("a viewer may not edit the config")
	}

	// one section down: it is named, the rest come
	f.errs["agents"] = errors.New("down")
	if d, err = (gardenProjects{srv}).Project(context.Background(), "gardenio"); err != nil || strings.Join(d.Partial, ",") != "agents" || len(d.Recent) != 2 {
		t.Errorf("agents down: err %v partial %v recent %d", err, d.Partial, len(d.Recent))
	}
	delete(f.errs, "agents")

	// a name that is no project and holds nothing is not there; the project list down is an error
	f.lists = map[string][]beadsapi.BeadDetail{}
	f.escalationsResp = nil
	var ae *beadsapi.APIError
	if _, err := (gardenProjects{srv}).Project(context.Background(), "nothere"); !errors.As(err, &ae) || ae.StatusCode != http.StatusNotFound {
		t.Errorf("nothere: %v, want a 404", err)
	}
	f.projectsErr = errors.New("down")
	if _, err := (gardenProjects{srv}).Project(context.Background(), "gardenio"); err == nil {
		t.Error("the project list down: want an error")
	}
}

// POST /p/{project}/config asked for JSON answers the editor's outcome: ok with the config bead once saved, a field the
// route refuses under "fields" with a 422, and the form for everyone else as before.
func TestProjectConfigSaveAnswersJSON(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{"gardenio": {Name: "gardenio", BeadID: "gd-proj-gardenio"}}
	f.beads["gd-proj-gardenio"] = &beadsapi.BeadDetail{ID: "gd-proj-gardenio", Type: "project", Title: "Project: gardenio", Status: "open", Fields: map[string]string{}}
	srv := NewServer(f, testLogger(), "", "", "", nil)
	h := edgeAuthMux(srv)
	post := func(form url.Values, accept string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/p/gardenio/config", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Auth-Request-Email", "alice@example.com")
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	var res projectConfigAction
	w := post(url.Values{"cpu_request": {"lots"}}, "application/json")
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || w.Code != http.StatusUnprocessableEntity || res.OK || !strings.Contains(res.Fields["cpu_request"], "not a valid Kubernetes quantity") {
		t.Errorf("a bad quantity: %d %+v (%v)", w.Code, res, err)
	}
	res = projectConfigAction{}
	w = post(url.Values{"cpu_request": {"500m"}, "docker": {"on"}}, "application/json")
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || w.Code != http.StatusOK || !res.OK || res.Bead != "gd-proj-gardenio" {
		t.Errorf("a good save: %d %+v (%v): %s", w.Code, res, err, w.Body.String())
	}
	if w := post(url.Values{"cpu_request": {"lots"}}, ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "not a valid Kubernetes quantity") {
		t.Errorf("the form keeps its fragment: %d", w.Code)
	}
}

// /active and /feed (R5) and the project hub (R8) say "Open it in the garden" to a viewer who reaches /garden (default on),
// and with their redirect flags on answer 302: /active and /feed with a project to its panel's activity tab, without one to
// the garden, their own filters dropped; the hub to the project panel. The hub's Work list stays a page, and each pair of
// flags is its own.
func TestGardenProjectRetirement(t *testing.T) {
	newSrv := func(t *testing.T, env map[string]string) http.Handler {
		f := newFake()
		f.projectsResp = map[string]beadsapi.ProjectInfo{"gardenio": {Name: "gardenio"}}
		f.listResp = &beadsapi.ListBeadsResult{}
		srv := NewServer(f, testLogger(), "/v", "", "", nil)
		if env != nil {
			srv.tunables = newWatcherWithEnv(t, env)
		}
		srv.SetGardenHandler(&gardenhub.Handler{Hub: gardenhub.New(nil, "/v", nil), BasePath: "/v"})
		return edgeAuthMux(srv)
	}
	get := func(h http.Handler, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Auth-Request-Email", "alice@example.com")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	banner := func(w *httptest.ResponseRecorder, href string) bool {
		return strings.Contains(w.Body.String(), `id="garden-banner"`) && strings.Contains(w.Body.String(), `href="`+href+`"`)
	}

	on := newSrv(t, nil)
	for path, href := range map[string]string{
		"/active?project=gardenio": "/v/garden?tab=activity&amp;project=gardenio",
		"/active":                  "/v/garden",
		"/p/gardenio":              "/v/garden?project=gardenio",
	} {
		if w := get(on, path); w.Code != http.StatusOK || !banner(w, href) {
			t.Errorf("%s: %d, banner to %s: %v", path, w.Code, href, banner(w, href))
		}
	}
	if w := get(on, "/p/gardenio?view=list"); strings.Contains(w.Body.String(), `id="garden-banner"`) {
		t.Error("the Work list is not retiring: no banner")
	}
	if w := get(newSrv(t, map[string]string{gardenActivityBannerEnv: "false"}), "/active?project=gardenio"); strings.Contains(w.Body.String(), `id="garden-banner"`) {
		t.Error("the activity banner's flag turns it off")
	}
	if w := get(newSrv(t, map[string]string{gardenActivityBannerEnv: "false"}), "/p/gardenio"); !strings.Contains(w.Body.String(), `id="garden-banner"`) {
		t.Error("the activity banner's flag does not turn off the project one")
	}

	redirect := newSrv(t, map[string]string{gardenActivityRedirectEnv: "true", gardenProjectRedirectEnv: "true"})
	for path, want := range map[string]string{
		"/active?project=gardenio&operated_by=me": "/v/garden?tab=activity&project=gardenio",
		"/feed?project=gardenio&scope=mine":       "/v/garden?tab=activity&project=gardenio",
		"/feed":                                   "/v/garden",
		"/p/gardenio":                             "/v/garden?project=gardenio",
		"/p/gardenio?view=overview&at=gardenio":   "/v/garden?at=gardenio&project=gardenio",
	} {
		if w := get(redirect, path); w.Code != http.StatusFound || w.Header().Get("Location") != want {
			t.Errorf("%s: %d to %q, want 302 to %q", path, w.Code, w.Header().Get("Location"), want)
		}
	}
	for _, path := range []string{"/p/gardenio?view=list", "/p/gardenio?status=open"} {
		if w := get(redirect, path); w.Code != http.StatusOK {
			t.Errorf("%s: %d, want the Work list page", path, w.Code)
		}
	}
	if w := get(newSrv(t, map[string]string{gardenActivityRedirectEnv: "true"}), "/p/gardenio"); w.Code != http.StatusOK {
		t.Errorf("the activity redirect flag does not move the hub: %d", w.Code)
	}
}
