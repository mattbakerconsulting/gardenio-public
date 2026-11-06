package grafanaembed

import (
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const testBase = "https://grafana.clonk.tv"

func TestURLs_PanelAndDashboardShape(t *testing.T) {
	o := Opts{PanelID: "3", Vars: []Var{{Name: "agent", Val: "gardenio-foo-123"}}}
	src, open := URLs(testBase, "uid", o)
	if want := testBase + "/d-solo/uid?panelId=3&kiosk&var-agent=gardenio-foo-123"; src != want {
		t.Errorf("panel src = %q, want %q", src, want)
	}
	if want := testBase + "/d/uid?viewPanel=3&var-agent=gardenio-foo-123"; open != want {
		t.Errorf("panel open = %q, want %q", open, want)
	}

	src, open = URLs(testBase, "uid", Opts{})
	if src != testBase+"/d/uid?kiosk" || open != testBase+"/d/uid" {
		t.Errorf("dashboard urls = %q, %q", src, open)
	}
	src, open = URLs(testBase, "uid", Opts{Theme: "dark"})
	if src != testBase+"/d/uid?kiosk&theme=dark" || open != testBase+"/d/uid?theme=dark" {
		t.Errorf("dashboard urls with theme = %q, %q", src, open)
	}
}

func TestURLs_ThemeAndTimeBeforeVarsAndEscaped(t *testing.T) {
	o := Opts{
		PanelID: "2", Theme: "dark", From: "now/d", To: "now", Timezone: "utc",
		Vars: []Var{{Name: "project", Val: "$__all"}},
	}
	src, open := URLs(testBase, "uid", o)
	tail := "&theme=dark&from=now%2Fd&to=now&timezone=utc&var-project=%24__all"
	if want := testBase + "/d-solo/uid?panelId=2&kiosk" + tail; src != want {
		t.Errorf("src = %q, want %q", src, want)
	}
	if want := testBase + "/d/uid?viewPanel=2" + tail; open != want {
		t.Errorf("open = %q, want %q", open, want)
	}
}

func TestURLs_EmptyBase(t *testing.T) {
	if src, open := URLs("", "uid", Opts{PanelID: "1"}); src != "" || open != "" {
		t.Errorf("empty base urls = %q, %q; want both empty", src, open)
	}
}

func TestHTML_UnconfiguredCard(t *testing.T) {
	out := HTML("", `x"><script>`, Opts{PanelID: "1"})
	if strings.Contains(out, "<iframe") || strings.Contains(out, "<script>") {
		t.Errorf("unconfigured card must carry no iframe and escape the uid: %s", out)
	}
	if !strings.Contains(out, "grafana-embed--unconfigured") || !strings.Contains(out, "Grafana not configured") {
		t.Errorf("unconfigured card missing its note: %s", out)
	}
}

func TestHTML_HeightAndSandbox(t *testing.T) {
	for _, tc := range []struct {
		opts Opts
		want string
	}{
		{Opts{}, `height="420"`},
		{Opts{PanelID: "1"}, `height="260"`},
		{Opts{PanelID: "1", Height: "360"}, `height="360"`},
		{Opts{PanelID: "1", Height: "99999"}, `height="1200"`},
		{Opts{PanelID: "1", Height: "-5"}, `height="260"`},
		{Opts{PanelID: "1", Height: "tall"}, `height="260"`},
	} {
		out := HTML(testBase, "uid", tc.opts)
		if !strings.Contains(out, tc.want) {
			t.Errorf("%+v: want %s in %s", tc.opts, tc.want, out)
		}
		if !strings.Contains(out, `sandbox="allow-scripts allow-same-origin allow-popups"`) {
			t.Errorf("%+v: iframe lost its sandbox: %s", tc.opts, out)
		}
	}
	if out := HTML(testBase, "uid", Opts{PanelID: "1", Theme: "dark"}); !strings.Contains(out, `kiosk&amp;theme=dark`) {
		t.Errorf("src must be HTML-escaped in the attribute: %s", out)
	}
}

func TestParseTheme(t *testing.T) {
	for in, want := range map[string]Theme{"": ThemeDark, "dark": ThemeDark, "light": ThemeLight} {
		if got, err := ParseTheme(in); err != nil || got != want {
			t.Errorf("ParseTheme(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"Dark", "auto", "dark&kiosk", " light"} {
		if _, err := ParseTheme(in); !errors.Is(err, ErrInvalid) {
			t.Errorf("ParseTheme(%q) err = %v, want ErrInvalid", in, err)
		}
	}
}

func TestParseRange(t *testing.T) {
	for in, want := range map[string]Range{
		"":      {"7d", "now-7d", "now"},
		"today": {"today", "now/d", "now"},
		"7d":    {"7d", "now-7d", "now"},
		"30d":   {"30d", "now-30d", "now"},
	} {
		if got, err := ParseRange(in); err != nil || got != want {
			t.Errorf("ParseRange(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, in := range []string{"24h", "now-24h", "90d", "7D", "7d&from=now-1y"} {
		if _, err := ParseRange(in); !errors.Is(err, ErrInvalid) {
			t.Errorf("ParseRange(%q) err = %v, want ErrInvalid", in, err)
		}
	}
}

func TestValidProject(t *testing.T) {
	for _, ok := range []string{"gardenio", "a", "0", "my-land-2", strings.Repeat("a", 63)} {
		if !ValidProject(ok) {
			t.Errorf("ValidProject(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{
		"", "-gardenio", "gardenio-", "Gardenio", "garden_io", "garden.io", "a b",
		"$__all", "gardenio&var-x=1", "gardenio%26", strings.Repeat("a", 64),
	} {
		if ValidProject(bad) {
			t.Errorf("ValidProject(%q) = true, want false", bad)
		}
	}
}

// testSets exercises every set shape apart from the shipped registry, so these
// tests do not move when a registry panel does.
var testSets = []Set{
	{Name: "fleet", UID: "dash", ProjectVar: true, Panels: []Panel{{ID: 1, Title: "One"}, {ID: 5, Title: "Five", Note: "n"}}},
	{Name: "project", UID: "dash", PerProject: true, ProjectVar: true, Panels: []Panel{{ID: 6, Title: "Six"}, {ID: 9, Title: "Nine", LandOnly: true}}},
	{Name: "plain", UID: "plain-dash", Panels: []Panel{{ID: 2, Title: "Two"}}},
}

func TestBuild_Rejections(t *testing.T) {
	for _, q := range []Query{
		{Set: "nope"},
		{Set: ""},
		{Set: "fleet", Range: "24h"},
		{Set: "fleet", Theme: "auto"},
		{Set: "fleet", Project: "gardenio"},      // fleet set takes no project
		{Set: "fleet", Land: true},               // nor a land
		{Set: "plain", Project: "gardenio"},      // nor does a set without the var
		{Set: "project"},                         // project set needs one
		{Set: "project", Project: "$__all"},      // only the server sends $__all
		{Set: "project", Project: "Gardenio"},    // not a DNS label
		{Set: "project", Project: "a&var-b=c"},   // no query smuggling
		{Set: "project", Project: "../../admin"}, // no path either
	} {
		if res, err := build(testSets, testBase, q); !errors.Is(err, ErrInvalid) {
			t.Errorf("build(%+v) = %+v, %v; want ErrInvalid", q, res, err)
		}
	}
}

func TestBuild_FleetSetSendsAllProjects(t *testing.T) {
	res, err := build(testSets, testBase+"/", Query{Set: "fleet"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Range != "7d" || res.Theme != "dark" || res.Project != "" {
		t.Errorf("defaults = %q/%q/%q", res.Range, res.Theme, res.Project)
	}
	tail := "&theme=dark&from=now-7d&to=now&timezone=utc&var-project=%24__all"
	if want := testBase + "/d/dash?" + strings.TrimPrefix(tail, "&"); res.OpenDashboardURL != want {
		t.Errorf("open_dashboard_url = %q, want %q", res.OpenDashboardURL, want)
	}
	if len(res.Panels) != 2 {
		t.Fatalf("panels = %d, want 2", len(res.Panels))
	}
	p := res.Panels[1]
	if p.ID != 5 || p.Title != "Five" || p.Note != "n" {
		t.Errorf("card words = %+v", p)
	}
	if want := testBase + "/d-solo/dash?panelId=5&kiosk" + tail; p.Src != want {
		t.Errorf("src = %q, want %q", p.Src, want)
	}
	if want := testBase + "/d/dash?viewPanel=5" + tail; p.OpenURL != want {
		t.Errorf("open_url = %q, want %q", p.OpenURL, want)
	}
	if want := HTML(testBase, "dash", Opts{PanelID: "5", Height: "360", Theme: "dark", From: "now-7d", To: "now", Timezone: "utc",
		Vars: []Var{{Name: "project", Val: "$__all"}}}); p.EmbedHTML != want {
		t.Errorf("embed_html is not the shared builder's markup:\n got %s\nwant %s", p.EmbedHTML, want)
	}
}

func TestBuild_ProjectSetAndToday(t *testing.T) {
	res, err := build(testSets, testBase, Query{Set: "project", Project: "my-land", Range: "today", Theme: "light"})
	if err != nil {
		t.Fatal(err)
	}
	want := testBase + "/d-solo/dash?panelId=6&kiosk&theme=light&from=now%2Fd&to=now&timezone=utc&var-project=my-land"
	if res.Panels[0].Src != want {
		t.Errorf("src = %q, want %q", res.Panels[0].Src, want)
	}
	if res.Project != "my-land" || res.Range != "today" || res.Theme != "light" {
		t.Errorf("echoed inputs = %+v", res)
	}
	if len(res.Panels) != 1 {
		t.Errorf("an operator project got a land-only panel: %+v", res.Panels)
	}
}

func TestBuild_TenantLandAddsLandOnlyPanels(t *testing.T) {
	res, err := build(testSets, testBase, Query{Set: "project", Project: "my-land", Land: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Panels) != 2 || res.Panels[1].ID != 9 {
		t.Fatalf("panels = %+v, want 6 then the land-only 9", res.Panels)
	}
	if !strings.Contains(res.Panels[1].Src, "panelId=9&") || !strings.HasSuffix(res.Panels[1].Src, "&var-project=my-land") {
		t.Errorf("land panel src = %q", res.Panels[1].Src)
	}
}

func TestBuild_NoProjectVar(t *testing.T) {
	res, err := build(testSets, testBase, Query{Set: "plain", Range: "30d"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Panels[0].Src, "var-") {
		t.Errorf("a dashboard without a project variable got one: %s", res.Panels[0].Src)
	}
}

func TestBuild_Unconfigured(t *testing.T) {
	res, err := build(testSets, "", Query{Set: "fleet"})
	if err != nil {
		t.Fatalf("an empty base is a degraded 200, not an error: %v", err)
	}
	if res.OpenDashboardURL != "" {
		t.Errorf("open_dashboard_url = %q, want empty", res.OpenDashboardURL)
	}
	for _, p := range res.Panels {
		if p.Src != "" || p.OpenURL != "" {
			t.Errorf("card %d has urls with no base: %q %q", p.ID, p.Src, p.OpenURL)
		}
		if p.Title == "" || p.EmbedHTML != Unconfigured("dash") {
			t.Errorf("card %d lost its words or its degraded card: %+v", p.ID, p)
		}
	}
	// The route still validates with Grafana unset: a bad input is a 400 everywhere.
	if _, err := build(testSets, "", Query{Set: "fleet", Range: "1y"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("unconfigured build accepted a bad range: %v", err)
	}
}

func TestBuild_JSONShape(t *testing.T) {
	res, err := Build(testBase, Query{Set: "costs-fleet"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"set", "project", "range", "theme", "open_dashboard_url", "panels"} {
		if _, ok := m[k]; !ok {
			t.Errorf("result JSON missing %q: %s", k, b)
		}
	}
	card := m["panels"].([]any)[0].(map[string]any)
	for _, k := range []string{"id", "title", "scope", "note", "src", "open_url", "embed_html"} {
		if _, ok := card[k]; !ok {
			t.Errorf("card JSON missing %q: %v", k, card)
		}
	}
}

// uidRe is the viewer's dashboardUIDRe: every registry uid must pass it, so a
// registry entry could also be embedded by the viewer's :::grafana directive.
var uidRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func TestRegistry_WellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range sets {
		if seen[s.Name] {
			t.Errorf("duplicate set %q", s.Name)
		}
		seen[s.Name] = true
		if !uidRe.MatchString(s.UID) {
			t.Errorf("set %s uid %q fails the uid charset", s.Name, s.UID)
		}
		if s.PerProject && !s.ProjectVar {
			t.Errorf("set %s is per-project but its dashboard has no project variable", s.Name)
		}
		if len(s.Panels) == 0 {
			t.Errorf("set %s has no panels", s.Name)
		}
		ids := map[int]bool{}
		for _, p := range s.Panels {
			if p.ID <= 0 || ids[p.ID] {
				t.Errorf("set %s panel id %d is not a unique positive id", s.Name, p.ID)
			}
			ids[p.ID] = true
			if p.Title == "" || p.Scope == "" {
				t.Errorf("set %s panel %d lacks a title or scope line", s.Name, p.ID)
			}
		}
	}
}

// The shipped sets embed gardenio-garden-costs by the ids
// grafana_garden_costs_dashboard_test.go pins (garden-grafana-panels.md, "After
// 1.4: gardenio-garden-costs"). Changing a row here means changing that test,
// the dashboard JSON and the design note's table in the same PR.
func TestRegistry_GardenCosts(t *testing.T) {
	for _, tc := range []struct {
		set        string
		perProject bool
		ids        []int
		landOnly   []int
	}{
		{set: "costs-fleet", ids: []int{1, 2, 3, 4, 5}},
		{set: "costs-project", perProject: true, ids: []int{1, 10, 6, 7, 8, 9, 12}, landOnly: []int{9, 12}},
	} {
		s, ok := LookupSet(tc.set)
		if !ok {
			t.Errorf("%s missing", tc.set)
			continue
		}
		if s.UID != "gardenio-garden-costs" || s.PerProject != tc.perProject || !s.ProjectVar {
			t.Errorf("%s = %+v", tc.set, s)
		}
		var ids, landOnly []int
		for _, p := range s.Panels {
			ids = append(ids, p.ID)
			if p.LandOnly {
				landOnly = append(landOnly, p.ID)
			}
		}
		if !slices.Equal(ids, tc.ids) || !slices.Equal(landOnly, tc.landOnly) {
			t.Errorf("%s panel ids = %v (land-only %v), want %v (land-only %v)", tc.set, ids, landOnly, tc.ids, tc.landOnly)
		}
	}
}

// The fleet set sends var-project=$__all, and an operator project gets no
// land-only panel.
func TestRegistry_GardenCostsVars(t *testing.T) {
	fleet, err := Build(testBase, Query{Set: "costs-fleet"})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range fleet.Panels {
		if !strings.HasSuffix(p.Src, "&var-project=%24__all") {
			t.Errorf("costs-fleet panel %d src = %q", p.ID, p.Src)
		}
	}
	op, err := Build(testBase, Query{Set: "costs-project", Project: "gardenio"})
	if err != nil {
		t.Fatal(err)
	}
	land, err := Build(testBase, Query{Set: "costs-project", Project: "my-land", Land: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(op.Panels) != 5 || len(land.Panels) != 7 {
		t.Errorf("costs-project panels: operator %d (want 5), tenant land %d (want 7)", len(op.Panels), len(land.Panels))
	}
}

// The per-agent sets embed gardenio-garden-agents by the ids
// grafana_garden_agents_dashboard_test.go pins (step 4.2): one agent's four
// panels on its face, and a gardener's runs with Spend by run first.
func TestRegistry_GardenAgents(t *testing.T) {
	for set, want := range map[string][]int{"agent": {1, 2, 3, 4}, "gardener": {5, 1, 2, 3, 4}} {
		s, ok := LookupSet(set)
		if !ok {
			t.Errorf("%s missing", set)
			continue
		}
		if s.UID != "gardenio-garden-agents" || !s.PerAgent || s.PerProject || s.ProjectVar {
			t.Errorf("%s = %+v", set, s)
		}
		var ids []int
		for _, p := range s.Panels {
			ids = append(ids, p.ID)
		}
		if !slices.Equal(ids, want) {
			t.Errorf("%s panel ids = %v, want %v", set, ids, want)
		}
	}
}

// One agent is its name in var-agent; a gardener's runs are joined by '|' (the
// board's agent=~"$agent" matches any of them, anchored); no var-project.
func TestBuild_AgentSetsSendVarAgent(t *testing.T) {
	one, err := Build(testBase, Query{Set: "agent", Agents: []string{"gardenio-42-agent-and-rfu"}, Range: "today"})
	if err != nil {
		t.Fatal(err)
	}
	if len(one.Panels) != 4 || !slices.Equal(one.Agents, []string{"gardenio-42-agent-and-rfu"}) {
		t.Fatalf("agent set = %+v", one)
	}
	for _, p := range one.Panels {
		if !strings.HasSuffix(p.Src, "&var-agent=gardenio-42-agent-and-rfu") || strings.Contains(p.Src, "var-project") {
			t.Errorf("agent panel %d src = %q", p.ID, p.Src)
		}
	}
	if !strings.HasSuffix(one.OpenDashboardURL, "&var-agent=gardenio-42-agent-and-rfu") {
		t.Errorf("agent open_dashboard_url = %q", one.OpenDashboardURL)
	}
	runs, err := Build(testBase, Query{Set: "gardener", Agents: []string{"guard-run-12", "guard-run-11"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs.Panels) != 5 || runs.Panels[0].ID != 5 {
		t.Fatalf("gardener set = %+v", runs)
	}
	for _, p := range runs.Panels {
		if !strings.HasSuffix(p.Src, "&var-agent=guard-run-12%7Cguard-run-11") {
			t.Errorf("gardener panel %d src = %q", p.ID, p.Src)
		}
	}
}

// health-land embeds gardenio-garden-health by the ids
// grafana_garden_health_dashboard_test.go pins (garden-grafana-panels.md, "After
// 4.1: gardenio-garden-health"): the fleet-wide Lands SLO cards on every
// project, the land's own cards on a tenant land only.
func TestRegistry_GardenHealth(t *testing.T) {
	s, ok := LookupSet("health-land")
	if !ok {
		t.Fatal("health-land missing")
	}
	if s.UID != "gardenio-garden-health" || !s.PerProject || !s.ProjectVar {
		t.Errorf("health-land = %+v", s)
	}
	var ids, landOnly []int
	for _, p := range s.Panels {
		ids = append(ids, p.ID)
		if p.LandOnly {
			landOnly = append(landOnly, p.ID)
		}
	}
	wantIDs, wantLand := []int{1, 2, 3, 4, 5, 7, 9, 8, 10, 12, 13}, []int{7, 9, 8, 10, 12, 13}
	if !slices.Equal(ids, wantIDs) || !slices.Equal(landOnly, wantLand) {
		t.Errorf("health-land panel ids = %v (land-only %v), want %v (land-only %v)", ids, landOnly, wantIDs, wantLand)
	}
	op, err := Build(testBase, Query{Set: "health-land", Project: "gardenio"})
	if err != nil {
		t.Fatal(err)
	}
	land, err := Build(testBase, Query{Set: "health-land", Project: "my-land", Land: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(op.Panels) != 5 || len(land.Panels) != 11 {
		t.Errorf("health-land panels: operator %d (want 5), tenant land %d (want 11)", len(op.Panels), len(land.Panels))
	}
	for _, p := range land.Panels {
		if !strings.Contains(p.Src, "/d-solo/gardenio-garden-health?") || !strings.HasSuffix(p.Src, "&var-project=my-land") {
			t.Errorf("health-land panel %d src = %q", p.ID, p.Src)
		}
	}
	if _, err := Build(testBase, Query{Set: "health-land"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("health-land with no project = %v, want ErrInvalid", err)
	}
}
