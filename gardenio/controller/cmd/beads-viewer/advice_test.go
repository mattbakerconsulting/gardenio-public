package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func TestHandleAdvice_Empty(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{Beads: nil, Total: 0}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/advice", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "No advice in this scope.") {
		t.Errorf("expected empty-state copy, body=%s", body)
	}
	if len(f.listAllQ) == 0 {
		t.Fatal("expected ListBeadsFiltered to be called")
	}
	q := f.listAllQ[0]
	if got := q.Types; len(got) != 1 || got[0] != "advice" {
		t.Errorf("types = %v, want [advice]", got)
	}
	if len(q.Statuses) != 0 {
		t.Errorf("statuses = %v, want none (closed advice still appears)", q.Statuses)
	}
	if q.Sort != "created_at" {
		t.Errorf("sort = %q, want created_at", q.Sort)
	}
	if q.Limit != adviceListLimit {
		t.Errorf("limit = %d, want %d", q.Limit, adviceListLimit)
	}
}

func adviceFixtures() *beadsapi.ListBeadsResult {
	now := time.Now()
	return &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:          "gd-adv-global",
				Title:       "Global advice for everyone",
				Type:        "advice",
				Status:      "open",
				Priority:    1,
				Labels:      []string{"global"},
				Description: "Always do the right thing.",
				CreatedAt:   now.Add(-2 * time.Hour),
				UpdatedAt:   now.Add(-30 * time.Minute),
			},
			{
				ID:          "gd-adv-project",
				Title:       "Project-targeted gardenio advice",
				Type:        "advice",
				Status:      "open",
				Priority:    2,
				Labels:      []string{"project:gardenio"},
				Description: "Specific to the gardenio project.",
				CreatedAt:   now.Add(-3 * time.Hour),
				UpdatedAt:   now.Add(-1 * time.Hour),
				Fields: map[string]string{
					"hook_trigger": "session-end",
				},
			},
			{
				ID:        "gd-adv-role",
				Title:     "Role-targeted gardener advice",
				Type:      "advice",
				Status:    "open",
				Priority:  2,
				Labels:    []string{"role:gardener"},
				CreatedAt: now.Add(-4 * time.Hour),
				UpdatedAt: now.Add(-2 * time.Hour),
			},
			{
				ID:        "gd-adv-closed",
				Title:     "Archived advice still listed",
				Type:      "advice",
				Status:    "closed",
				Priority:  3,
				Labels:    []string{"global"},
				CreatedAt: now.Add(-7 * 24 * time.Hour),
				UpdatedAt: now.Add(-5 * 24 * time.Hour),
			},
			{
				ID:        "gd-adv-advanced",
				Title:     "Advanced gN: targeting",
				Type:      "advice",
				Status:    "open",
				Priority:  2,
				Labels:    []string{"g0:role:gardener", "g0:project:gardenio"},
				CreatedAt: now.Add(-5 * time.Hour),
				UpdatedAt: now.Add(-3 * time.Hour),
			},
		},
		Total: 5,
	}
}

func TestHandleAdvice_NonEmpty_RendersRows(t *testing.T) {
	f := newFake()
	f.listResp = adviceFixtures()
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/advice", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	for _, want := range []string{
		"gd-adv-global", "Global advice for everyone",
		"gd-adv-project", "Project-targeted gardenio advice",
		"gd-adv-role", "Role-targeted gardener advice",
		"gd-adv-advanced", "Advanced gN: targeting",
		"gd-adv-closed", "Archived advice still listed",
		`href="/gd-adv-global"`,
		"archived",          // pill on closed advice
		"⚡ session-end",     // hook trigger bolt
		"scope-chip-global", // chip on All view
		"scope-chip-project",
		"scope-chip-role",
		"scope-chip-advanced",
		"All Advice",       // current scope title
		"project:gardenio", // sidebar chip label rendered as scope chip
		"role:gardener",
		"Advanced targeting", // sidebar entry shown when advanced advice exists
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/advice missing %q\nbody=%s", want, body)
		}
	}
	if strings.Contains(body, "No advice in this scope.") {
		t.Errorf("unexpected empty-state when advice rows present")
	}
}

func TestHandleAdvice_ProjectScope_FiltersToProject(t *testing.T) {
	f := newFake()
	f.listResp = adviceFixtures()
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/advice?scope=project:gardenio", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	if !strings.Contains(body, "gd-adv-project") {
		t.Errorf("project scope should include project advice, body=%s", body)
	}
	for _, want := range []string{"gd-adv-global", "gd-adv-role", "gd-adv-advanced"} {
		if strings.Contains(body, want) {
			t.Errorf("project scope should exclude %q", want)
		}
	}
	if !strings.Contains(body, "Project: gardenio") {
		t.Errorf("expected scope title 'Project: gardenio', body=%s", body)
	}
}

func TestHandleAdvice_AdvancedScope_FiltersToAdvanced(t *testing.T) {
	f := newFake()
	f.listResp = adviceFixtures()
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/advice?scope=advanced", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "gd-adv-advanced") {
		t.Errorf("advanced scope should include gN: advice, body=%s", body)
	}
	for _, want := range []string{"gd-adv-project", "gd-adv-role", "gd-adv-global"} {
		if strings.Contains(body, want) {
			t.Errorf("advanced scope should exclude %q", want)
		}
	}
}

func TestHandleAdvice_SearchFiltersBody(t *testing.T) {
	f := newFake()
	f.listResp = adviceFixtures()
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/advice?q=gardenio", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// "gardenio" is in the project label and the project advice description.
	if !strings.Contains(body, "gd-adv-project") {
		t.Errorf("search should match project advice, body=%s", body)
	}
	// Role advice has no "gardenio" anywhere.
	if strings.Contains(body, "gd-adv-role") {
		t.Errorf("search should not match role advice")
	}
}

func TestHandleAdvice_DaemonError_RendersBanner(t *testing.T) {
	f := newFake()
	f.listErr = &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/advice", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Failed to load advice.") {
		t.Errorf("expected error banner, body=%s", w.Body.String())
	}
}

func TestClassifyAdvice_Cases(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		want   scopeKind
	}{
		{"no labels → global", nil, scopeGlobal},
		{"global only", []string{"global"}, scopeGlobal},
		{"project only", []string{"project:gardenio"}, scopeProject},
		{"role only", []string{"role:gardener"}, scopeRole},
		{"agent only", []string{"agent:bot-1"}, scopeAgent},
		{"global + project → project (pre-filter wins)", []string{"global", "project:gardenio"}, scopeProject},
		{"global + role → global (role has no pre-filter)", []string{"global", "role:gardener"}, scopeGlobal},
		{"gN: prefix → advanced", []string{"g0:project:gardenio", "g0:role:gardener"}, scopeAdvanced},
		{"two projects → project (pick first available bucket)", []string{"project:a", "project:b"}, scopeProject},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := classifyAdvice(tc.labels)
			if c.kind != tc.want {
				t.Errorf("kind = %v, want %v (labels=%v)", c.kind, tc.want, tc.labels)
			}
		})
	}
}

func TestParseScopeParam(t *testing.T) {
	cases := []struct {
		in    string
		kind  scopeKind
		value string
	}{
		{"", scopeAll, ""},
		{"all", scopeAll, ""},
		{"global", scopeGlobal, ""},
		{"advanced", scopeAdvanced, ""},
		{"project:gardenio", scopeProject, "gardenio"},
		{"role:gardener", scopeRole, "gardener"},
		{"agent:bot-1", scopeAgent, "bot-1"},
		{"junk", scopeAll, ""},
		{"rig:gardenio", scopeAll, ""}, // retired alias falls through to scopeAll
		{"project:", scopeAll, ""},
	}
	for _, tc := range cases {
		got := parseScopeParam(tc.in)
		if got.kind != tc.kind || got.value != tc.value {
			t.Errorf("parseScopeParam(%q) = (%v, %q), want (%v, %q)", tc.in, got.kind, got.value, tc.kind, tc.value)
		}
	}
}

// TestHandleAdvice_DensitySignals verifies the info-density fixes (gd-l14dOVcEXS):
// scope chips render in every view, author appears in the row meta, and the
// relative timestamp carries an absolute-time title for hover precision.
func TestHandleAdvice_DensitySignals(t *testing.T) {
	now := time.Now()
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-adv-author",
				Title:     "Advice with author",
				Type:      "advice",
				Status:    "open",
				Priority:  1,
				Labels:    []string{"project:gardenio"},
				CreatedBy: "alice",
				CreatedAt: now.Add(-2 * time.Hour),
				UpdatedAt: now.Add(-30 * time.Minute),
			},
		},
		Total: 1,
	}
	mux := mkServer(t, f)

	// Project scope used to drop the chip; it must now render so scope is
	// always legible regardless of which sidebar bucket is active.
	req := httptest.NewRequest("GET", "/advice?scope=project:gardenio", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"scope-chip-project",               // chip renders on project scope view
		"project:gardenio",                 // chip label
		`class="advice-row-author"`,        // author shown
		`>alice<`,                          // author value
		`class="advice-row-ago" title="20`, // absolute RFC timestamp on hover
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/advice?scope=project:gardenio missing %q\nbody=%s", want, body)
		}
	}
}

// TestHandleAdvice_StatsBanner verifies the header stats strip (ka-0000003569):
// total open + per-scope breakdown is rendered so the page does not start with
// a wall of sparse rows and 50% gutters. Counts derive from the full advice
// population (scope-wide), not the filtered visible rows.
func TestHandleAdvice_StatsBanner(t *testing.T) {
	f := newFake()
	f.listResp = adviceFixtures()
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/advice", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`class="advice-stats"`,
		`Advice catalog totals`,       // aria-label
		`class="advice-stats-num">4<`, // 4 open total (global, project, role, advanced)
		`class="advice-stats-label">open<`,
		`class="advice-stats-num">1<`, // 1 global open
		`class="advice-stats-label">global<`,
		`class="advice-stats-label">projects<`,
		`class="advice-stats-label">roles<`,
		`class="advice-stats-label">advanced<`, // fixture has one advanced row
		`class="advice-stats-label">archived<`, // fixture has one closed row
		`class="advice-stats-label">shown<`,
		`advice-list-grid`, // wide-viewport 2-column grid class
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/advice missing %q\nbody=%s", want, body)
		}
	}
}

// TestHandleAdvice_StatsBanner_EmptyHidesArchived verifies the archived
// counter is suppressed when there are no closed advice beads, so the strip
// stays compact instead of showing "0 archived".
func TestHandleAdvice_StatsBanner_EmptyHidesArchived(t *testing.T) {
	now := time.Now()
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID: "gd-only-global", Title: "Only global", Type: "advice",
				Status: "open", Labels: []string{"global"},
				CreatedAt: now.Add(-1 * time.Hour), UpdatedAt: now.Add(-30 * time.Minute),
			},
		},
		Total: 1,
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/advice", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	if strings.Contains(body, `>archived<`) {
		t.Errorf("expected no archived counter when closed=0\nbody=%s", body)
	}
	if strings.Contains(body, `>advanced<`) {
		t.Errorf("expected no advanced counter when advanced=0\nbody=%s", body)
	}
}

// TestBuildScopeChips_AllDimensions covers the multi-chip output: combined
// project + role advice surfaces both chips so the row exposes the full
// targeting set rather than collapsing to a single primary scope.
func TestBuildScopeChips_AllDimensions(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		want   []scopeChipView
	}{
		{
			name:   "global only",
			labels: []string{"global"},
			want:   []scopeChipView{{Kind: "global", Label: "global"}},
		},
		{
			name:   "project + role",
			labels: []string{"project:gardenio", "role:gardener"},
			want: []scopeChipView{
				{Kind: "project", Label: "project:gardenio"},
				{Kind: "role", Label: "role:gardener"},
			},
		},
		{
			name:   "advanced collapses to single chip",
			labels: []string{"g0:project:gardenio", "g0:role:gardener"},
			want:   []scopeChipView{{Kind: "advanced", Label: "advanced"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := classifyAdvice(tc.labels)
			got := buildScopeChips(c)
			if len(got) != len(tc.want) {
				t.Fatalf("chips = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("chip[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}
