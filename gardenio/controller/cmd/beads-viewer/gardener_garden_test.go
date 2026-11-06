package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/gardenhub"
	"gardenio/controller/internal/oidcauth"
)

// The gardener and settings panels' readers, their writes' JSON answers and the R9 and R10 retirements (Lands
// workbench-glass spec WB-11, WB-13, §2.6; step 5.12, gd-au7ywvWAG8).

// The gardener panel reads what /gardener/{id} shows: the header (name, goal, project, counters), the life-thread newest
// first with the live run marked, the beads its runs touched by lane (the runs themselves left out), and the config form's
// values; a bead that is no gardener, or none at all, is not there.
func TestGardenGardenerReadsTheGardener(t *testing.T) {
	f := seedGardenerPageFake(t)
	f.beads["gd-cap"].Fields["chain"] = `{"to":{"phase":"review","terminal_goal":"every MR merged"}}`
	f.beads["gd-task"] = &beadsapi.BeadDetail{ID: "gd-task", Type: "task", Title: "Not a gardener", Status: "open"}
	srv := NewServer(f, testLogger(), "", "", "", nil)
	srv.SetNow(gardenerPageNow)

	d, err := gardenGardeners{srv}.Gardener(context.Background(), "gd-cap")
	if err != nil {
		t.Fatal(err)
	}
	if d.ID != "gd-cap" || d.Name != "delivery" || d.Goal != "drain the ready queue" || d.Project != "gardenio" || !d.Enabled || d.Closed {
		t.Fatalf("header %+v", d)
	}
	if d.RunsSpawned != 3 || d.RunsCompleted != 2 || d.MaxRuns != 10 || d.RunBudget != 100 || len(d.Partial) != 0 {
		t.Errorf("counters %+v partial %v", d, d.Partial)
	}
	if len(d.Runs) != 3 || d.Runs[0].ID != "gd-r3" || !d.Runs[0].Live || d.Runs[0].Outcome != "" || d.Runs[2].ID != "gd-r1" || d.Runs[2].Outcome != "ok" {
		t.Errorf("runs %+v: newest first, the live one with no outcome yet", d.Runs)
	}
	if d.LiveCount != 1 || d.RunCount != 3 {
		t.Errorf("run count %d live %d", d.RunCount, d.LiveCount)
	}
	if len(d.Beads) != 4 || d.Beads[0].Lane != "in_progress" || d.Beads[3].Lane != "done" {
		t.Errorf("lanes %+v", d.Beads)
	}
	for _, l := range d.Beads {
		for _, b := range l.Beads {
			if strings.HasPrefix(b.ID, "gd-r") {
				t.Errorf("lane %s carries run %s: the runs are the Runs tab", l.Lane, b.ID)
			}
		}
	}
	if d.Config == nil || d.Config.TerminalGoal != "drain the ready queue" || d.Config.MaxRuns != "10" {
		t.Errorf("config %+v", d.Config)
	}
	if len(d.Successors) != 1 || !strings.Contains(d.Successors[0], "phase=review") {
		t.Errorf("successors %v", d.Successors)
	}

	var ae *beadsapi.APIError
	if _, err := (gardenGardeners{srv}).Gardener(context.Background(), "gd-task"); !errors.As(err, &ae) || ae.StatusCode != http.StatusNotFound {
		t.Errorf("a task: %v, want a 404", err)
	}
	if _, err := (gardenGardeners{srv}).Gardener(context.Background(), "gd-nothere"); err == nil {
		t.Error("no bead: want an error")
	}
}

// The settings panel reads the viewer's user bead (spawn preferences, identity), the projects to choose from and
// quick-spawn's roles; POST /settings with scope=garden writes only the fields it carries, every other preference kept as
// stored and the homepage layout not written, answers JSON when asked, and a refusal is JSON with its message.
func TestGardenSettingsReadsAndSavesItsOwnFields(t *testing.T) {
	f := settingsFakeWith(Preferences{DefaultSpawnProject: "gardenio", DefaultSpawnRole: "crew", VibeMode: true, Skin: "auto"}, Identity{DisplayName: "Op", Team: "core"})
	f.beads["gd-user-op"].Fields["profile_prefs"] = `{"hidden_sections":["open_mrs"]}`
	f.projectsResp = map[string]beadsapi.ProjectInfo{"music": {Name: "music"}, "gardenio": {Name: "gardenio"}}
	srv := NewServer(f, testLogger(), "", "", "", nil)

	d, err := gardenSettings{srv}.Settings(oidcauth.ContextWithEmail(context.Background(), authorEmail))
	if err != nil {
		t.Fatal(err)
	}
	if !d.HasBead || d.BeadID != "gd-user-op" || d.Spawn.Project != "gardenio" || d.Spawn.Role != "crew" || d.Identity.DisplayName != "Op" || d.Identity.Team != "core" {
		t.Errorf("settings %+v", d)
	}
	if strings.Join(d.Projects, ",") != "gardenio,music" || strings.Join(d.Roles, ",") != strings.Join(defaultQuickSpawnRoles, ",") {
		t.Errorf("choices %v %v", d.Projects, d.Roles)
	}
	if d, _ := (gardenSettings{srv}).Settings(context.Background()); d == nil || d.Email != "" || d.HasBead {
		t.Errorf("no session: %+v", d)
	}

	mux := mkServer(t, f)
	post := func(form url.Values) *httptest.ResponseRecorder {
		req := authedPost("/settings", form)
		req.Header.Set("Accept", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w
	}
	w := post(url.Values{"scope": {"garden"}, "default_spawn_role": {" gardener "}, "pronouns": {"they/them"}})
	var res actionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || w.Code != http.StatusOK || !res.OK {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	got := f.updateFieldsLastVal
	p, _ := got["preferences"].(Preferences)
	if p.DefaultSpawnRole != "gardener" || p.DefaultSpawnProject != "gardenio" || !bool(p.VibeMode) || p.Skin != "auto" {
		t.Errorf("preferences %+v: the role written, the project, the full viewer and the skin kept", p)
	}
	if got["pronouns"] != "they/them" || got["display_name"] != "Op" || got["team"] != "core" {
		t.Errorf("identity %v: pronouns written, the rest kept", got)
	}
	if _, ok := got["profile_prefs"]; ok {
		t.Error("the garden's save does not write the homepage layout")
	}

	calls := f.updateFieldsCalls
	w = post(url.Values{"scope": {"garden"}, "slack_channel_id": {"not-a-channel"}})
	res = actionResponse{}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.OK || w.Code != http.StatusBadRequest || !strings.Contains(res.Error, "Slack channel id") || f.updateFieldsCalls != calls {
		t.Errorf("a bad channel: %d %s", w.Code, w.Body.String())
	}
}

// POST /create-gardener asked for JSON answers ok with the new gardener's bead or the form's refusal with its message;
// for everyone else it keeps its fragment.
func TestCreateGardenerAnswersJSON(t *testing.T) {
	mux := mkServer(t, newFake())
	post := func(form url.Values, accept string) *httptest.ResponseRecorder {
		req := authedPost("/create-gardener", form)
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w
	}
	w := post(url.Values{"project": {"gardenio"}, "prompt": {"drive it"}}, "application/json")
	var res actionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.OK || w.Code != http.StatusBadRequest || res.Error != "Epic id is required." {
		t.Errorf("no epic: %d %s", w.Code, w.Body.String())
	}
	if w := post(url.Values{"project": {"gardenio"}, "prompt": {"drive it"}}, ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Epic id is required.") {
		t.Errorf("the sheet keeps its fragment: %d %s", w.Code, w.Body.String())
	}
}

// The gardener page, the /gardeners index (R9) and /settings (R10) say "Open it in the garden" to a viewer who reaches
// /garden (default on); with their redirect flags on they answer 302: a gardener to its panel, the index to the garden,
// the create sheet to its project's Gardeners tab, the quick-spawn sheet to its task's bead panel, /settings to the
// settings panel. An htmx request and the page's fragments stay; each pair of flags is its own.
func TestGardenGardenerRetirement(t *testing.T) {
	newSrv := func(t *testing.T, env map[string]string) http.Handler {
		f := seedGardenerPageFake(t)
		srv := NewServer(f, testLogger(), "/v", "", "", nil)
		srv.SetNow(gardenerPageNow)
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
	banner := func(w *httptest.ResponseRecorder, href string) bool {
		return strings.Contains(w.Body.String(), `id="garden-banner"`) && strings.Contains(w.Body.String(), `href="`+href+`"`)
	}

	on := newSrv(t, nil)
	for path, href := range map[string]string{
		"/gardener/gd-cap": "/v/garden?gardener=gd-cap",
		"/gardeners":       "/v/garden",
		"/settings":        "/v/garden?settings=me",
	} {
		if w := get(on, path, false); w.Code != http.StatusOK || !banner(w, href) {
			t.Errorf("%s: %d, banner to %s: %v", path, w.Code, href, banner(w, href))
		}
	}
	if w := get(newSrv(t, map[string]string{gardenGardenerBannerEnv: "false"}), "/gardener/gd-cap", false); strings.Contains(w.Body.String(), `id="garden-banner"`) {
		t.Error("the gardener banner's flag turns it off")
	}
	if w := get(newSrv(t, map[string]string{gardenGardenerBannerEnv: "false"}), "/settings", false); !strings.Contains(w.Body.String(), `id="garden-banner"`) {
		t.Error("the gardener banner's flag does not turn off the settings one")
	}

	redirect := newSrv(t, map[string]string{gardenGardenerRedirectEnv: "true", gardenSettingsRedirectEnv: "true"})
	for path, want := range map[string]string{
		"/gardener/gd-cap?at=gardenio":   "/v/garden?at=gardenio&gardener=gd-cap",
		"/gardeners?filter=active":       "/v/garden",
		"/create-gardener?project=music": "/v/garden?tab=gardeners&project=music",
		"/create-gardener":               "/v/garden",
		"/quick-spawn?task=gd-t1":        "/v/garden?bead=gd-t1",
		"/quick-spawn?project=music":     "/v/garden?project=music",
		"/settings":                      "/v/garden?settings=me",
	} {
		if w := get(redirect, path, false); w.Code != http.StatusFound || w.Header().Get("Location") != want {
			t.Errorf("%s: %d to %q, want 302 to %q", path, w.Code, w.Header().Get("Location"), want)
		}
	}
	for _, path := range []string{"/gardener/gd-cap/runs", "/quick-spawn-inline?task=gd-t1&project=gardenio", "/settings/view"} {
		if w := get(redirect, path, false); w.Code == http.StatusFound {
			t.Errorf("%s: a fragment of the page is not retired, got a 302", path)
		}
	}
	if w := get(redirect, "/gardener/gd-cap", true); w.Code != http.StatusOK {
		t.Errorf("an htmx request keeps the page: %d", w.Code)
	}
	if w := get(newSrv(t, map[string]string{gardenSettingsRedirectEnv: "true"}), "/gardener/gd-cap", false); w.Code != http.StatusOK {
		t.Errorf("the settings redirect flag does not move the gardener page: %d", w.Code)
	}
}
