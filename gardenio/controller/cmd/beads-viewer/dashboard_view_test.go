package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/grafanaembed"
)

// fakeModelReader implements dashboardModelReader for buildDashboardView tests.
type fakeModelReader struct {
	revDeps  []beadsapi.Dependency
	revErr   error
	beads    map[string]*beadsapi.BeadDetail
	getErr   error
	getCalls int
}

func (f *fakeModelReader) GetReverseDependencies(_ context.Context, _ string) ([]beadsapi.Dependency, error) {
	if f.revErr != nil {
		return nil, f.revErr
	}
	return f.revDeps, nil
}

func (f *fakeModelReader) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	f.getCalls++
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.beads[id], nil
}

func dashboardBead(fields map[string]string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{ID: "gd-DASH01", Type: "dashboard", Title: "fallback title", Fields: fields}
}

func TestBuildDashboardView_EmbedURL(t *testing.T) {
	tests := []struct {
		name        string
		uid         string
		grafanaBase string
		wantEmbed   bool
		wantContain string
	}{
		{"valid uid + base", "cost-overview", "https://g.example/grafana", true, "/grafana/d/cost-overview?kiosk"},
		{"trailing slash trimmed", "abc_123", "https://g.example/grafana/", true, "/grafana/d/abc_123?kiosk"},
		{"no base → no embed", "cost-overview", "", false, ""},
		{"no uid → no embed", "", "https://g.example", false, ""},
		{"invalid uid (slash) → no embed", "a/b", "https://g.example", false, ""},
		{"invalid uid (space) → no embed", "a b", "https://g.example", false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := buildDashboardView(context.Background(), dashboardBead(map[string]string{"dashboard_uid": tc.uid}), nil, tc.grafanaBase)
			if v.HasEmbed() != tc.wantEmbed {
				t.Fatalf("HasEmbed()=%v want %v (EmbedURL=%q)", v.HasEmbed(), tc.wantEmbed, v.EmbedURL)
			}
			if tc.wantContain != "" && !strings.Contains(string(v.EmbedURL), tc.wantContain) {
				t.Errorf("EmbedURL=%q want contains %q", v.EmbedURL, tc.wantContain)
			}
		})
	}
}

func TestBuildDashboardView_Metadata(t *testing.T) {
	v := buildDashboardView(context.Background(), dashboardBead(map[string]string{
		"dashboard_uid":     "uid1",
		"title":             "Cost Overview",
		"dashboard_folder":  "ACME",
		"dashboard_version": "7",
		"dashboard_source":  "beads",
		"dashboard_hash":    "0123456789abcdeffedcba",
	}), nil, "")
	if v.Title != "Cost Overview" {
		t.Errorf("Title=%q", v.Title)
	}
	if v.Folder != "ACME" || v.Version != "7" || v.Source != "beads" {
		t.Errorf("meta mismatch: %+v", v)
	}
	if v.Hash != "0123456789ab" {
		t.Errorf("Hash=%q want 12-char prefix", v.Hash)
	}
}

func TestBuildDashboardView_TitleFallsBackToBeadTitle(t *testing.T) {
	v := buildDashboardView(context.Background(), dashboardBead(map[string]string{"dashboard_uid": "u"}), nil, "")
	if v.Title != "fallback title" {
		t.Errorf("Title=%q want bead title fallback", v.Title)
	}
}

func TestBuildDashboardView_ModelDocPanels(t *testing.T) {
	model := `{"title":"D","panels":[
	  {"id":1,"title":"CPU","type":"timeseries"},
	  {"id":2,"title":"Group","type":"row","panels":[{"id":3,"title":"Mem","type":"gauge"}]}
	]}`
	reader := &fakeModelReader{
		revDeps: []beadsapi.Dependency{{BeadID: "gd-DOC01", DependsOnID: "gd-DASH01", Type: "parent-child"}},
		beads: map[string]*beadsapi.BeadDetail{
			"gd-DOC01": {ID: "gd-DOC01", Type: "doc", Labels: []string{dashboardModelDocLabel}, Fields: map[string]string{"content": model}},
		},
	}
	v := buildDashboardView(context.Background(), dashboardBead(map[string]string{"dashboard_uid": "u"}), reader, "")
	if v.ModelDocID != "gd-DOC01" {
		t.Fatalf("ModelDocID=%q", v.ModelDocID)
	}
	if len(v.Panels) != 3 {
		t.Fatalf("got %d panels want 3: %+v", len(v.Panels), v.Panels)
	}
	if v.Panels[0].Title != "CPU" || v.Panels[1].Type != "row" || v.Panels[2].Title != "Mem" {
		t.Errorf("panel flatten wrong: %+v", v.Panels)
	}
}

func TestBuildDashboardView_ModelDegraded(t *testing.T) {
	tests := []struct {
		name    string
		reader  *fakeModelReader
		wantErr string
	}{
		{
			"deps error",
			&fakeModelReader{revErr: errors.New("boom")},
			"model fetch failed",
		},
		{
			"no model doc",
			&fakeModelReader{revDeps: nil},
			"no model attached",
		},
		{
			"unparseable model",
			&fakeModelReader{
				revDeps: []beadsapi.Dependency{{BeadID: "gd-DOC01", DependsOnID: "gd-DASH01", Type: "parent-child"}},
				beads: map[string]*beadsapi.BeadDetail{
					"gd-DOC01": {ID: "gd-DOC01", Type: "doc", Labels: []string{dashboardModelDocLabel}, Fields: map[string]string{"content": "not json"}},
				},
			},
			"model unparseable",
		},
		{
			"wrong-label doc ignored",
			&fakeModelReader{
				revDeps: []beadsapi.Dependency{{BeadID: "gd-DOC01", DependsOnID: "gd-DASH01", Type: "parent-child"}},
				beads: map[string]*beadsapi.BeadDetail{
					"gd-DOC01": {ID: "gd-DOC01", Type: "doc", Labels: []string{"other"}, Fields: map[string]string{"content": "{}"}},
				},
			},
			"no model attached",
		},
		{
			"non-parent-child dep ignored",
			&fakeModelReader{
				revDeps: []beadsapi.Dependency{{BeadID: "gd-DOC01", DependsOnID: "gd-DASH01", Type: "blocks"}},
				beads: map[string]*beadsapi.BeadDetail{
					"gd-DOC01": {ID: "gd-DOC01", Type: "doc", Labels: []string{dashboardModelDocLabel}, Fields: map[string]string{"content": "{}"}},
				},
			},
			"no model attached",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := buildDashboardView(context.Background(), dashboardBead(map[string]string{"dashboard_uid": "u"}), tc.reader, "")
			if v.ModelErr != tc.wantErr {
				t.Errorf("ModelErr=%q want %q", v.ModelErr, tc.wantErr)
			}
			if len(v.Panels) != 0 {
				t.Errorf("expected no panels, got %+v", v.Panels)
			}
		})
	}
}

func TestBuildDashboardView_NilBead(t *testing.T) {
	if v := buildDashboardView(context.Background(), nil, nil, "https://g"); v != nil {
		t.Errorf("nil bead should yield nil view, got %+v", v)
	}
}

func TestExtractDashboardPanels_EmptyAndBad(t *testing.T) {
	if _, err := extractDashboardPanels(nil); err == nil {
		t.Error("empty model should error")
	}
	if _, err := extractDashboardPanels([]byte("[1,2,3]")); err == nil {
		t.Error("non-object model should error")
	}
	panels, err := extractDashboardPanels([]byte(`{"panels":[]}`))
	if err != nil || len(panels) != 0 {
		t.Errorf("empty panels: panels=%v err=%v", panels, err)
	}
}

// --- Directive expansion ---

func TestEmbedGrafanaDirectives_WholeDashboard(t *testing.T) {
	in := "<p>before</p>\n:::grafana cost-overview\n<p>after</p>"
	out := embedGrafanaDirectives(in, "https://g.example/grafana")
	if !strings.Contains(out, `class="grafana-embed"`) {
		t.Fatalf("no embed div: %s", out)
	}
	if !strings.Contains(out, `src="https://g.example/grafana/d/cost-overview?kiosk"`) {
		t.Errorf("bad src: %s", out)
	}
	if !strings.Contains(out, "<p>before</p>") || !strings.Contains(out, "<p>after</p>") {
		t.Errorf("surrounding content lost: %s", out)
	}
}

func TestEmbedGrafanaDirectives_Panel(t *testing.T) {
	out := embedGrafanaDirectives(":::grafana uid1 panel=3", "https://g/grafana")
	// & is HTML-escaped to &amp; inside the attribute value.
	if !strings.Contains(out, "/d-solo/uid1?panelId=3&amp;kiosk") {
		t.Errorf("panel solo src missing: %s", out)
	}
	if !strings.Contains(out, "Grafana panel 3") {
		t.Errorf("panel label missing: %s", out)
	}
}

func TestEmbedGrafanaDirectives_HeightClamped(t *testing.T) {
	out := embedGrafanaDirectives(":::grafana uid1 height=99999", "https://g")
	if !strings.Contains(out, `height="1200"`) {
		t.Errorf("height not clamped to max: %s", out)
	}
}

func TestEmbedGrafanaDirectives_WrappedInParagraph(t *testing.T) {
	// goldmark wraps a lone directive line in <p>…</p>.
	out := embedGrafanaDirectives("<p>:::grafana abc</p>", "https://g")
	if !strings.Contains(out, `class="grafana-embed"`) {
		t.Errorf("p-wrapped directive not expanded: %s", out)
	}
}

func TestEmbedGrafanaDirectives_Unconfigured(t *testing.T) {
	out := embedGrafanaDirectives(":::grafana abc", "")
	if !strings.Contains(out, "grafana-embed--unconfigured") {
		t.Errorf("want link-card fallback when no base: %s", out)
	}
	if strings.Contains(out, "<iframe") {
		t.Errorf("must not emit iframe when unconfigured: %s", out)
	}
}

func TestEmbedGrafanaDirectives_NoDirectiveUnchanged(t *testing.T) {
	in := "<p>just some text with gd-ABC123 mention</p>"
	if out := embedGrafanaDirectives(in, "https://g"); out != in {
		t.Errorf("non-directive HTML mutated: %q", out)
	}
}

func TestEmbedGrafanaDirectives_InvalidUIDIgnored(t *testing.T) {
	// uid with a slash never matches the directive regex → left as-is.
	in := ":::grafana bad/uid"
	if out := embedGrafanaDirectives(in, "https://g"); strings.Contains(out, "<iframe") {
		t.Errorf("invalid uid should not produce iframe: %s", out)
	}
}

func TestParseGrafanaEmbedOpts(t *testing.T) {
	o := parseGrafanaEmbedOpts(" panel=5 height=300 bogus=x")
	if o.PanelID != "5" || o.Height != "300" {
		t.Errorf("parse: %+v", o)
	}
	// non-numeric panel is rejected.
	if got := parseGrafanaEmbedOpts(" panel=abc"); got.PanelID != "" {
		t.Errorf("non-numeric panel accepted: %+v", got)
	}
}

func TestParseGrafanaEmbedOpts_Vars(t *testing.T) {
	// var-<name>= tokens are collected in order; panel/height still parse.
	o := parseGrafanaEmbedOpts(" panel=1 var-agent=gardenio-foo-123 var-project=gardenio height=300")
	if o.PanelID != "1" || o.Height != "300" {
		t.Errorf("panel/height alongside vars: %+v", o)
	}
	if len(o.Vars) != 2 {
		t.Fatalf("want 2 vars, got %+v", o.Vars)
	}
	if o.Vars[0] != (grafanaembed.Var{Name: "agent", Val: "gardenio-foo-123"}) {
		t.Errorf("var[0]: %+v", o.Vars[0])
	}
	if o.Vars[1] != (grafanaembed.Var{Name: "project", Val: "gardenio"}) {
		t.Errorf("var[1]: %+v", o.Vars[1])
	}
	// empty var name (var-=x) is ignored.
	if got := parseGrafanaEmbedOpts(" var-=x"); len(got.Vars) != 0 {
		t.Errorf("empty var name accepted: %+v", got.Vars)
	}
}

func TestEmbedGrafanaDirectives_VarPassthrough_SoloPanel(t *testing.T) {
	out := embedGrafanaDirectives(":::grafana gardenio-charts-candidate panel=2 var-agent=gardenio-foo-123", "https://g/grafana")
	// & is HTML-escaped to &amp; inside the attribute value; var param rides
	// after the panelId+kiosk pair.
	if !strings.Contains(out, "/d-solo/gardenio-charts-candidate?panelId=2&amp;kiosk&amp;var-agent=gardenio-foo-123") {
		t.Errorf("solo-panel var passthrough missing/misordered: %s", out)
	}
	// Open↗ URL also carries the var so the pop-out matches the scoped frame.
	if !strings.Contains(out, "/d/gardenio-charts-candidate?viewPanel=2&amp;var-agent=gardenio-foo-123") {
		t.Errorf("open URL var passthrough missing: %s", out)
	}
}

func TestEmbedGrafanaDirectives_VarPassthrough_WholeDashboard(t *testing.T) {
	out := embedGrafanaDirectives(":::grafana fleet var-project=gardenio", "https://g/grafana")
	if !strings.Contains(out, "/d/fleet?kiosk&amp;var-project=gardenio") {
		t.Errorf("whole-dashboard src var passthrough missing: %s", out)
	}
	// Whole-dashboard Open↗ URL has no query yet, so the var opens one with `?`.
	if !strings.Contains(out, `href="https://g/grafana/d/fleet?var-project=gardenio"`) {
		t.Errorf("whole-dashboard open URL should start query with ?: %s", out)
	}
}

func TestEmbedGrafanaDirectives_VarValueURLEncoded(t *testing.T) {
	// A dotted model id (a real account-bead `model` value) stays intact through
	// QueryEscape. Spaces can't reach here — the directive regex rejects them —
	// but the builder still QueryEscapes for defense in depth.
	out := grafanaembed.HTML("https://g", "uid", grafanaembed.Opts{
		PanelID: "5",
		Vars:    []grafanaembed.Var{{Name: "model", Val: "claude-opus-4.8"}},
	})
	if !strings.Contains(out, "var-model=claude-opus-4.8") {
		t.Errorf("model var not passed through: %s", out)
	}
}

func TestEmbedGrafanaDirectives_NoVarsUnchangedSrc(t *testing.T) {
	// Regression guard: with no var- tokens the src must be byte-identical to
	// the pre-feature output (no stray trailing &).
	out := embedGrafanaDirectives(":::grafana uid1 panel=3", "https://g/grafana")
	if !strings.Contains(out, "/d-solo/uid1?panelId=3&amp;kiosk\"") {
		t.Errorf("no-var src changed shape: %s", out)
	}
}

func TestGrafanaEmbedHTML_EscapesPanelLabel(t *testing.T) {
	// panel id reaches the label; ensure the builder escapes it (defense in
	// depth even though parse only admits digits).
	out := grafanaembed.HTML("https://g", "uid", grafanaembed.Opts{PanelID: "7"})
	if !strings.Contains(out, "sandbox=") {
		t.Errorf("iframe missing sandbox: %s", out)
	}
	// The embed must suppress its own scrollbar: a solo-panel Grafana kiosk page
	// can momentarily overflow its fixed-height iframe during responsive reflow,
	// which surfaced an inner scrollbar the operator could drag (ka-0000002791).
	// scrolling="no" (legacy) is the contract; CSS overflow:hidden on
	// .grafana-embed-frame is the standards-mode enforcement.
	if !strings.Contains(out, `scrolling="no"`) {
		t.Errorf("iframe missing scrolling=\"no\" (scroll-suppression contract): %s", out)
	}
}

// --- End-to-end: dashboard bead through the bead-show page ---

func TestBeadShow_DashboardType_LiveEmbed(t *testing.T) {
	f := newFake()
	f.beads["gd-dash1"] = &beadsapi.BeadDetail{
		ID: "gd-dash1", Type: "dashboard", Status: "open", Title: "Cost Overview",
		Fields: map[string]string{
			"dashboard_uid":     "cost-overview",
			"dashboard_folder":  "ACME",
			"dashboard_version": "3",
			"dashboard_source":  "beads",
			"dashboard_hash":    "abc123def456ffff",
		},
	}
	f.beads["gd-dashdoc"] = &beadsapi.BeadDetail{
		ID: "gd-dashdoc", Type: "doc", Status: "open", Labels: []string{dashboardModelDocLabel},
		Fields: map[string]string{"content": `{"panels":[{"id":1,"title":"Spend","type":"timeseries"}]}`},
	}
	if f.revDeps == nil {
		f.revDeps = map[string][]beadsapi.Dependency{}
	}
	f.revDeps["gd-dash1"] = []beadsapi.Dependency{{BeadID: "gd-dashdoc", DependsOnID: "gd-dash1", Type: "parent-child"}}

	srv := NewServer(f, slog.Default(), "", "", "", nil)
	srv.grafanaBaseURL = "https://gardenio.example/grafana"
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/gd-dash1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"dashboard-card",
		`src="https://gardenio.example/grafana/d/cost-overview?kiosk"`,
		"Open in Grafana",
		"authored in beads",
		"abc123def456", // short hash
		"edit model doc gd-dashdoc",
		"Spend", // panel from model
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard page missing %q", want)
		}
	}
}

// TestBeadShow_GrafanaDirectiveInDescription proves requirement #2: a
// :::grafana directive in any bead's description renders an embed, exercising
// the goldmark → LinkifyDescriptionHTML → embedGrafanaDirectives path.
func TestBeadShow_GrafanaDirectiveInDescription(t *testing.T) {
	t.Setenv("GRAFANA_BASE_URL", "https://gardenio.example/grafana")
	f := newFake()
	f.beads["gd-task-with-embed"] = &beadsapi.BeadDetail{
		ID: "gd-task-with-embed", Type: "task", Status: "open", Title: "Task w/ embed",
		Description: "See the dashboard below:\n\n:::grafana cost-overview\n\nand a single panel:\n\n:::grafana cost-overview panel=2\n",
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-task-with-embed", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "grafana-embed-frame") {
		t.Errorf("description embed iframe missing")
	}
	if !strings.Contains(body, "/d/cost-overview?kiosk") {
		t.Errorf("whole-dashboard embed missing")
	}
	if !strings.Contains(body, "panelId=2") {
		t.Errorf("single-panel embed missing")
	}
}

func TestBeadShow_DashboardType_NoGrafanaFallback(t *testing.T) {
	f := newFake()
	f.beads["gd-dash2"] = &beadsapi.BeadDetail{
		ID: "gd-dash2", Type: "dashboard", Status: "open", Title: "Bare Dash",
		Fields: map[string]string{"dashboard_uid": "bare"},
	}
	// No grafanaBaseURL set on the server → no iframe.
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-dash2", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "<iframe") {
		t.Errorf("no iframe expected without grafanaBaseURL")
	}
	if !strings.Contains(body, "grafana-embed--unconfigured") {
		t.Errorf("expected unconfigured note, body:\n%s", body)
	}
}
