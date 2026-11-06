package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// hubNow pins the render clock so the golden assertions are time-of-day
// independent.
func hubNow() time.Time { return time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC) }

// getHub issues GET /p/<project>?view=overview against a full route set and
// returns the recorder. The ?view=overview opt-in is the keystone seam: the
// list remains the default (ka-0000001620 flips it later).
func getHub(t *testing.T, f *fakeDaemon, project string) *httptest.ResponseRecorder {
	t.Helper()
	mux := mkServerWithNow(t, f, hubNow)
	req := httptest.NewRequest(http.MethodGet, "/p/"+project+"?view=overview", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// TestProjectHub_RoutesAndRenders verifies the ?view=overview dispatch reaches
// the hub (not the legacy list) and the page carries the header, the full tab
// bar, and the eager Overview sections.
func TestProjectHub_RoutesAndRenders(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{
		"gardenio": {Name: "gardenio", BeadID: "gd-proj1", Status: "open", GitURL: "https://example.com/repo.git"},
	}
	f.beads["gd-proj1"] = &beadsapi.BeadDetail{
		ID: "gd-proj1", Type: "project", Status: "open",
		Fields: map[string]string{"git_url": "https://example.com/repo.git"},
	}
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-1", Type: "task", Status: "open", Priority: 2, CreatedAt: hubNow(), UpdatedAt: hubNow()},
		},
		Total: 1,
	}

	w := getHub(t, f, "gardenio")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// Header + config-bead deep link.
	for _, want := range []string{
		`class="project-hub-header"`,
		"config: gd-proj1",
		`href="/gd-proj1"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("hub body missing %q", want)
		}
	}
	// Full tab bar in canonical order.
	for _, tab := range []string{"Overview", "Work", "Agents", "Gardeners", "Costs", "Sequencer→"} {
		if !strings.Contains(body, ">"+tab+"<") {
			t.Errorf("tab bar missing %q tab", tab)
		}
	}
	if i, j := strings.Index(body, ">Overview<"), strings.Index(body, ">Work<"); i < 0 || j < 0 || i >= j {
		t.Errorf("Overview must render before Work (i=%d j=%d)", i, j)
	}
	// Eager Overview sections.
	for _, want := range []string{
		`class="section-tabs"`,
		`<h2>Health`,
		`<h2>Work`,
		`view full list →`,
		`<section class="hub-section hub-config"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("hub Overview missing %q", want)
		}
	}
	// It must be the hub, NOT the legacy list (no bulk bar / "Open beads" list).
	if strings.Contains(body, `id="project-bulk-bar"`) {
		t.Error("hub rendered the legacy list bulk bar; ?view=overview should route to the hub")
	}
}

// TestProjectHub_LazySectionShells verifies the eager hub render mounts all six
// dependent-object sections as HTMX reveal-on-scroll shells (task ka-0000003487,
// design doc §3.2): each carries an intersect-once trigger, an outerHTML swap, a
// /b/project/<name>/<endpoint> hx-get, a stable slug id, and a loading
// placeholder. The fragment bodies themselves are exercised in
// project_hub_sections_test.go; this pins the wiring in project_hub.html.
func TestProjectHub_LazySectionShells(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{
		"gardenio": {Name: "gardenio", BeadID: "gd-proj1", Status: "open"},
	}
	f.beads["gd-proj1"] = &beadsapi.BeadDetail{
		ID: "gd-proj1", Type: "project", Status: "open",
	}
	f.listResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{}, Total: 0}

	w := getHub(t, f, "gardenio")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// Every lazy fragment endpoint must be wired to a reveal-on-scroll shell.
	for _, sec := range []struct{ slug, endpoint, loading string }{
		{"project-hub-agents", "agents", "Loading agents"},
		{"project-hub-gardeners", "gardeners", "Loading gardeners"},
		{"project-hub-accounts", "accounts", "Loading accounts"},
		{"project-hub-costs", "costs", "Loading costs"},
		{"project-hub-activity", "activity", "Loading recent activity"},
		{"project-hub-decisions", "decisions", "Loading decisions"},
	} {
		if !strings.Contains(body, `hx-get="/b/project/gardenio/`+sec.endpoint+`"`) {
			t.Errorf("missing lazy hx-get for %q section", sec.endpoint)
		}
		if !strings.Contains(body, `id="`+sec.slug+`-shell"`) {
			t.Errorf("missing shell id for %q section", sec.slug)
		}
		if !strings.Contains(body, sec.loading) {
			t.Errorf("missing loading placeholder %q", sec.loading)
		}
	}
	// Reveal-on-scroll contract: intersect-once trigger + outerHTML swap.
	if !strings.Contains(body, `hx-trigger="intersect once"`) {
		t.Error("lazy shells must use hx-trigger=\"intersect once\" (not revealed; ka-0000003717)")
	}
	if !strings.Contains(body, `hx-swap="outerHTML"`) {
		t.Error("lazy shells must swap outerHTML so the rendered <section> replaces the shell")
	}
	// The shells live AFTER the eager Overview, not inside it.
	if i, j := strings.Index(body, "project-hub-overview"), strings.Index(body, "project-hub-lazy-sections"); i < 0 || j < 0 || j < i {
		t.Errorf("lazy sections must render after the eager Overview (overview=%d lazy=%d)", i, j)
	}
}

// TestProjectHub_EmptyProject verifies a label-only project with no beads still
// renders the hub: header shows "label-only project", config section shows the
// no-config message, and the page does not 500.
func TestProjectHub_EmptyProject(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{} // no config bead
	f.listResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{}, Total: 0}

	w := getHub(t, f, "orphan")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	for _, want := range []string{
		"label-only project",
		"No <code>type:project</code> config bead",
		`class="section-tabs"`, // tab bar still renders
	} {
		if !strings.Contains(body, want) {
			t.Errorf("empty-project hub missing %q", want)
		}
	}
	// No config-bead deep link when there is no config bead.
	if strings.Contains(body, "config: gd-") {
		t.Error("empty project should not render a config-bead id chip")
	}
	// Admin-gated Config tab still absent (no roles = fail-closed), and
	// a label-only project has no config-bead edit hint.
	if strings.Contains(body, "edit via CLI") {
		t.Error("label-only project should not show the CLI edit hint (no config bead)")
	}
}

// TestProjectHub_FullyPopulated verifies the config summary renders the labeled
// ProjectInfo fields, secrets are shown by NAME only (never values), the
// validity badge reflects a clean config, and the work digest carries counts.
func TestProjectHub_FullyPopulated(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{
		"gardenio": {
			Name:             "gardenio",
			BeadID:           "gd-proj1",
			Status:           "open",
			Prefix:           "kd",
			GitURL:           "https://example.com/repo.git",
			DefaultBranch:    "main",
			Image:            "registry/agent:latest",
			Account:          "acct-1",
			Model:            "claude-opus-4-8",
			CPURequest:       "500m",
			CPULimit:         "2000m",
			MemoryRequest:    "512Mi",
			MemoryLimit:      "2Gi",
			StorageClass:     "gp3",
			ServiceAccount:   "gardenio-agents-sa",
			DockerEnabled:    true,
			PostgresEnabled:  false,
			SchedulesEnabled: true,
			TriggersEnabled:  true,
			SlackChannels:    []string{"#gardenio"},
			Secrets: []beadsapi.SecretEntry{
				{Env: "MY_TOKEN", Secret: "gardenio-tokens", Key: "my-token"},
			},
		},
	}
	f.beads["gd-proj1"] = &beadsapi.BeadDetail{
		ID: "gd-proj1", Type: "project", Status: "open",
		Fields: map[string]string{
			"git_url": "https://example.com/repo.git",
			"secrets": `[{"env":"MY_TOKEN","secret":"gardenio-tokens","key":"my-token"}]`,
		},
	}
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-1", Type: "task", Status: "open", Priority: 0, CreatedAt: hubNow(), UpdatedAt: hubNow()},
			{ID: "gd-2", Type: "bug", Status: "in_progress", Priority: 2, CreatedAt: hubNow(), UpdatedAt: hubNow()},
		},
		Total: 2,
	}

	w := getHub(t, f, "gardenio")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// Labeled config fields.
	for _, want := range []string{
		"https://example.com/repo.git",
		"registry/agent:latest",
		"acct-1",
		"claude-opus-4-8",
		"500m", "2000m", "512Mi", "2Gi",
		"gardenio-agents-sa",
		"#gardenio",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("config summary missing %q", want)
		}
	}
	// Flags render on/off chips.
	if !strings.Contains(body, "hub-flag-on") || !strings.Contains(body, "hub-flag-off") {
		t.Error("config flags should render both on and off states")
	}
	// SECURITY: secret env name + reference shown; never a value. There is no
	// value in the model, but assert the reference shape renders and that a
	// hypothetical value string is absent.
	if !strings.Contains(body, "MY_TOKEN") {
		t.Error("secret env var name should render")
	}
	if !strings.Contains(body, "gardenio-tokens/my-token") {
		t.Error("secret should render as Secret/key reference")
	}
	// A clean config → green validity badge.
	if !strings.Contains(body, "validity-green") {
		t.Errorf("fully-populated clean config should show a green validity badge; body=%s", body)
	}
	// Work digest carries chip counts from the summary.
	if !strings.Contains(body, "chip-count") {
		t.Error("work digest should render chip counts when the summary loaded")
	}
}

// TestProjectHub_DegradedDaemon verifies best-effort degradation through the
// full render path: a daemon error on the summary query hides the health panel
// but the hub still renders config + the tab bar with a 200 (never a 500).
func TestProjectHub_DegradedDaemon(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{
		"gardenio": {Name: "gardenio", BeadID: "gd-proj1", Status: "open", GitURL: "https://example.com/repo.git"},
	}
	f.beads["gd-proj1"] = &beadsapi.BeadDetail{
		ID: "gd-proj1", Type: "project", Status: "open",
		Fields: map[string]string{"git_url": "https://example.com/repo.git"},
	}
	// Summary query errors → health panel hidden, chips fall back to label-only.
	f.listErr = &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}

	w := getHub(t, f, "gardenio")
	if w.Code != http.StatusOK {
		t.Fatalf("degraded daemon must still 200, got %d; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// Config still renders.
	if !strings.Contains(body, "config: gd-proj1") {
		t.Error("config panel should still render when the summary query errored")
	}
	// Health panel degrades to its unavailable message.
	if !strings.Contains(body, "Health is unavailable right now.") {
		t.Errorf("health panel should show its unavailable message on summary error; body=%s", body)
	}
	// Tab bar still present.
	if !strings.Contains(body, `class="section-tabs"`) {
		t.Error("tab bar should render even on a degraded daemon")
	}
}

// TestProjectHub_DefaultViewIsHub pins the flipped default (ka-0000001620): a
// bare /p/<project> (no ?view, no list params) now serves the rich hub Overview,
// while ?view=list still serves the legacy bead list — so the Work tab is
// preserved without breaking the default route.
func TestProjectHub_DefaultViewIsHub(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{"gardenio": {Name: "gardenio", BeadID: "gd-proj1"}}
	f.listResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{}, Total: 0}
	mux := mkServerWithNow(t, f, hubNow)

	// Bare /p/<project> → hub Overview (tab bar present, no list bulk bar).
	req := httptest.NewRequest(http.MethodGet, "/p/gardenio", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `class="section-tabs"`) {
		t.Error("bare /p/<project> should render the hub (tab bar), now that the default is flipped")
	}
	if strings.Contains(body, `id="project-bulk-bar"`) {
		t.Error("bare /p/<project> should not render the legacy list bulk bar (default is the hub)")
	}

	// ?view=list → the legacy list (bulk bar present).
	req = httptest.NewRequest(http.MethodGet, "/p/gardenio?view=list", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("?view=list status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `id="project-bulk-bar"`) {
		t.Error("?view=list should still render the legacy list (bulk bar)")
	}
}

// TestProjectHub_FilterParamsStillRouteToList pins the compat seam
// (ka-0000001620): pre-existing filter deep links that carry a list-shaping
// param (type/status/sort/label/q) but no ?view= still land on the legacy list
// after the default flipped to the hub — so index cards, the agents band, and
// type-chip URLs are not orphaned.
func TestProjectHub_FilterParamsStillRouteToList(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{"gardenio": {Name: "gardenio", BeadID: "gd-proj1"}}
	f.listResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{}, Total: 0}
	mux := mkServerWithNow(t, f, hubNow)

	for _, q := range []string{"?status=open", "?type=task", "?sort=-updated_at", "?label=foo", "?q=bar"} {
		req := httptest.NewRequest(http.MethodGet, "/p/gardenio"+q, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", q, w.Code)
		}
		if !strings.Contains(w.Body.String(), `id="project-bulk-bar"`) {
			t.Errorf("/p/gardenio%s should route to the legacy list (bulk bar) via the compat seam", q)
		}
	}

	// An explicit ?view=overview wins even when a stray filter param is present.
	req := httptest.NewRequest(http.MethodGet, "/p/gardenio?view=overview&type=task", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if strings.Contains(w.Body.String(), `id="project-bulk-bar"`) {
		t.Error("?view=overview should render the hub even with a filter param present")
	}
}
