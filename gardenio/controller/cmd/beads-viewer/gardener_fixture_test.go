package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TestIsFixtureGardenerBead exercises each of the three independent exclusion
// signals (label prefix, title marker, e2e-spec created_by) and — critically —
// asserts that a real gardener carrying none of them is NOT flagged (bug
// ka-0000008054, acceptance criterion 3: no real gardener accidentally excluded).
func TestIsFixtureGardenerBead(t *testing.T) {
	cases := []struct {
		name string
		bead *beadsapi.BeadDetail
		want bool
	}{
		{"nil", nil, false},
		{
			"the real fixture (all three signals)",
			&beadsapi.BeadDetail{
				ID:        "ka-0000004925",
				Title:     "[E2E fixture] gardener bead",
				Labels:    []string{"fixture:e2e:gardener", "project:gardenio", "role:gardener", "trigger:schedule"},
				CreatedBy: "gardenio-browser-e2e-spec-7vx",
			},
			true,
		},
		{
			"fixture: label alone",
			&beadsapi.BeadDetail{ID: "gd-x", Title: "Innocuous title", Labels: []string{"fixture:foo", "role:gardener"}, CreatedBy: "thread-123"},
			true,
		},
		{
			"title marker alone (case/space insensitive)",
			&beadsapi.BeadDetail{ID: "gd-x", Title: "  [e2e Fixture] something", Labels: []string{"role:gardener"}, CreatedBy: "thread-123"},
			true,
		},
		{
			"e2e-spec created_by alone",
			&beadsapi.BeadDetail{ID: "gd-x", Title: "Gardener: real-sounding", Labels: []string{"role:gardener"}, CreatedBy: "gardenio-browser-e2e-spec-abc"},
			true,
		},
		{
			"a real gardener — none of the signals",
			&beadsapi.BeadDetail{
				ID:        "gd-real",
				Title:     "Gardener: drive operated_by initials epic",
				Labels:    []string{"trigger:schedule", "role:gardener", "project:gardenio"},
				CreatedBy: "thread-1779908146-389129",
			},
			false,
		},
		{
			"real gardener whose title merely mentions e2e (not the bracketed marker)",
			&beadsapi.BeadDetail{
				ID:        "gd-real2",
				Title:     "Gardener: RWX e2e manual-on-MR cost-saving research",
				Labels:    []string{"role:gardener"},
				CreatedBy: "thread-1",
			},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isFixtureGardenerBead(tc.bead); got != tc.want {
				t.Errorf("isFixtureGardenerBead(%q) = %v; want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestExcludeFixtureGardener_EnvOptIn asserts the env-aware wrapper: a fixture
// gardener is excluded by default (production), but the
// BEADS_VIEWER_RENDER_FIXTURE_GARDENERS opt-in (set only by the behavioral-e2e
// harness, bug gd-R8wc4mOHL3) flips it to render. A real gardener is never
// excluded regardless of the flag.
func TestExcludeFixtureGardener_EnvOptIn(t *testing.T) {
	fixture := mkFixtureGardenerBead()
	real := &beadsapi.BeadDetail{
		ID:        "gd-real",
		Title:     "Gardener: drive operated_by initials epic",
		Labels:    []string{"trigger:schedule", "role:gardener", "project:gardenio"},
		CreatedBy: "thread-1779908146-389129",
	}

	// Default (flag unset): fixture excluded, real gardener rendered.
	t.Setenv(renderFixtureGardenersEnv, "")
	if !excludeFixtureGardener(fixture) {
		t.Errorf("fixture gardener must be excluded when render opt-in is off")
	}
	if excludeFixtureGardener(real) {
		t.Errorf("real gardener must never be excluded")
	}

	// Opt-in (flag set): fixture rendered, real gardener still rendered.
	t.Setenv(renderFixtureGardenersEnv, "1")
	if excludeFixtureGardener(fixture) {
		t.Errorf("fixture gardener must render when render opt-in is on")
	}
	if excludeFixtureGardener(real) {
		t.Errorf("real gardener must never be excluded (opt-in on)")
	}
}

// mkFixtureGardenerBead builds a gardener bead in the shape the E2E seeder uses:
// a type=trigger + role:gardener bead whose GardenerSpec.Name ("E2E demo-drive
// gardener") diverges from its true bead title ("[E2E fixture] gardener bead"),
// carrying the fixture:e2e:gardener label and the e2e-spec created_by.
func mkFixtureGardenerBead() *beadsapi.BeadDetail {
	spec := map[string]any{
		"name":           "E2E demo-drive gardener",
		"terminal_goal":  "drive the demo epic to done",
		"runs_spawned":   3,
		"runs_completed": 3,
	}
	raw, _ := json.Marshal(spec)
	return &beadsapi.BeadDetail{
		ID:        "ka-0000004925",
		Title:     "[E2E fixture] gardener bead",
		Type:      "trigger",
		Status:    "open",
		Labels:    []string{"fixture:e2e:gardener", "project:gardenio", "role:gardener", "trigger:schedule"},
		Fields:    map[string]string{"role": "gardener", "gardener": string(raw)},
		CreatedBy: "gardenio-browser-e2e-spec-7vx",
		UpdatedAt: time.Date(2026, 5, 1, 11, 0, 0, 0, time.UTC),
	}
}

// TestHandleGardeners_ExcludesFixtureGardener seeds the live fixture alongside a
// real gardener and asserts the index renders the real one but NOT the fixture
// (bug ka-0000008054, criteria 1 + 3).
func TestHandleGardeners_ExcludesFixtureGardener(t *testing.T) {
	f := seedGardenersIndexFake(t)
	// Append the fixture to the trigger/schedule response so it flows through
	// the same collect() projection a live deployment hits.
	f.triggerScheduleResp.Beads = append(f.triggerScheduleResp.Beads, mkFixtureGardenerBead())
	f.triggerScheduleResp.Total = len(f.triggerScheduleResp.Beads)

	mux := mkServerWithNow(t, f, gardenersNow)
	req := httptest.NewRequest("GET", "/gardeners", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// The two real gardeners still render.
	if !strings.Contains(body, "delivery") || !strings.Contains(body, "watchdog") {
		t.Errorf("real gardeners missing from index")
	}
	// The fixture must NOT render as a GARDENER ROW: its synthetic name and its
	// /gardener/{id} life-thread link must be absent from the live roster.
	for _, banned := range []string{"E2E demo-drive gardener", `href="/gardener/ka-0000004925"`} {
		if strings.Contains(body, banned) {
			t.Errorf("index leaked fixture as a gardener row: %q", banned)
		}
	}
	// The All-chip / total count reflects only the two real gardeners.
	if !strings.Contains(body, `href="/gardener/gd-cap1"`) {
		t.Errorf("expected real gardener link present")
	}
	// But the fixture IS accounted for in the "N hidden — why" disclosure
	// (bug ka-0000007605): it links to the plain bead page and is labelled an
	// E2E/test fixture, so the All-chip-vs-population delta is never silent.
	if !strings.Contains(body, "hidden from this roster") {
		t.Errorf("missing 'N hidden' affordance for the excluded fixture")
	}
	if !strings.Contains(body, `href="/ka-0000004925"`) {
		t.Errorf("hidden disclosure should link the fixture's plain bead page; body=%s", body)
	}
	if !strings.Contains(body, "E2E / test fixture") {
		t.Errorf("hidden disclosure should classify the fixture; body=%s", body)
	}
}

// TestHandleGardenerDetail_FixtureReturns404 asserts a deep link to the fixture
// gardener 404s rather than rendering its synthetic runs as real
// (bug ka-0000008054, criterion 1 — detail surface).
func TestHandleGardenerDetail_FixtureReturns404(t *testing.T) {
	f := newFake()
	fix := mkFixtureGardenerBead()
	f.beads[fix.ID] = fix

	mux := mkServerWithNow(t, f, gardenersNow)
	req := httptest.NewRequest("GET", "/gardener/"+fix.ID, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for fixture detail, got %d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "E2E demo-drive gardener") {
		t.Errorf("fixture detail leaked synthetic gardener name")
	}
}

// TestHandleGardenerDetail_FixtureRendersUnderRenderOptIn is the behavioral-e2e
// path (bug gd-R8wc4mOHL3): with BEADS_VIEWER_RENDER_FIXTURE_GARDENERS=1 set (the
// playwright.config.js webServer env), a deep link to the seeded fixture gardener
// must render 200 — surfacing its synthetic spec name — instead of 404, so
// gardener_detail.spec.js sees the page it asserts against.
func TestHandleGardenerDetail_FixtureRendersUnderRenderOptIn(t *testing.T) {
	t.Setenv(renderFixtureGardenersEnv, "1")
	f := newFake()
	fix := mkFixtureGardenerBead()
	f.beads[fix.ID] = fix

	mux := mkServerWithNow(t, f, gardenersNow)
	req := httptest.NewRequest("GET", "/gardener/"+fix.ID, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for fixture detail under render opt-in, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "E2E demo-drive gardener") {
		t.Errorf("fixture detail should surface the synthetic gardener name under render opt-in")
	}
}

// TestHandleGardenerDetail_RealGardenerStillRenders is the negative control: a
// real gardener bead with none of the fixture signals must still render 200, so
// the exclusion never over-reaches (bug ka-0000008054, criterion 3).
func TestHandleGardenerDetail_RealGardenerStillRenders(t *testing.T) {
	f := newFake()
	spec := map[string]any{"name": "delivery", "terminal_goal": "drain the ready queue"}
	raw, _ := json.Marshal(spec)
	real := &beadsapi.BeadDetail{
		ID:        "gd-real",
		Title:     "Gardener: drive operated_by initials epic",
		Type:      "trigger",
		Status:    "open",
		Labels:    []string{"trigger:schedule", "role:gardener", "project:gardenio"},
		Fields:    map[string]string{"role": "gardener", "gardener": string(raw)},
		CreatedBy: "thread-1779908146-389129",
		UpdatedAt: gardenersNow(),
	}
	f.beads[real.ID] = real

	mux := mkServerWithNow(t, f, gardenersNow)
	req := httptest.NewRequest("GET", "/gardener/"+real.ID, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for real gardener, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "delivery") {
		t.Errorf("real gardener name missing")
	}
	// Name ("delivery") diverges from bead title — the reconciliation subtitle
	// must surface the bead title (criterion 2).
	if !strings.Contains(body, "Gardener: drive operated_by initials epic") {
		t.Errorf("expected divergent bead-title subtitle to render")
	}
}

// TestGardenerRowView_NameDivergesFromTitle covers the name/title reconciliation
// predicate the index + detail templates gate on (bug ka-0000008054, criterion 2).
func TestGardenerRowView_NameDivergesFromTitle(t *testing.T) {
	cases := []struct {
		name, title string
		want        bool
	}{
		{"E2E demo-drive gardener", "[E2E fixture] gardener bead", true},
		{"delivery", "delivery", false},
		{"", "some title", false},
		{"some name", "", false},
		{"  delivery  ", "delivery", false}, // whitespace-insensitive
	}
	for _, tc := range cases {
		r := gardenerRowView{Name: tc.name, Title: tc.title}
		if got := r.NameDivergesFromTitle(); got != tc.want {
			t.Errorf("gardenerRowView{Name:%q,Title:%q}.NameDivergesFromTitle()=%v; want %v", tc.name, tc.title, got, tc.want)
		}
		v := gardenerView{Name: tc.name, Title: tc.title}
		if got := v.NameDivergesFromTitle(); got != tc.want {
			t.Errorf("gardenerView{Name:%q,Title:%q}.NameDivergesFromTitle()=%v; want %v", tc.name, tc.title, got, tc.want)
		}
	}
}
