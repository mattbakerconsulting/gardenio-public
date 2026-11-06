package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// seedPhaseLabelPlanFake seeds one phased plan whose phase sub-epics carry the
// conventional "Phase N — …" titles, so the rendered grid exercises the
// splitPhaseLabel tag/name split (the shared seedPhasedPlanFake titles its
// phases "Research"/"Implementation", which peel no prefix). Minimal: one phase
// with one step is enough to emit a labeled grid row.
func seedPhaseLabelPlanFake() *fakeDaemon {
	f := newFake()
	tm := func(min int) time.Time { return time.Date(2026, 7, 1, 10, min, 0, 0, time.UTC) }

	plan := &beadsapi.BeadDetail{
		ID: "gd-plan1", Title: "Plan: ship phases", Kind: "issue", Type: "epic",
		Status: "in_progress", Labels: []string{"project:gardenio", "plan"},
		Fields:    map[string]string{"phase_order": `["gd-phase1","gd-phase2"]`},
		UpdatedAt: tm(0),
	}
	phase1 := &beadsapi.BeadDetail{ID: "gd-phase1", Title: "Phase 1 — Settle the unknown", Kind: "issue", Type: "epic", Status: "in_progress"}
	phase2 := &beadsapi.BeadDetail{ID: "gd-phase2", Title: "Phase 2 — Make it honest", Kind: "issue", Type: "epic", Status: "open"}
	s1 := &beadsapi.BeadDetail{ID: "gd-s1", Title: "First step", Kind: "issue", Type: "task", Status: "closed", CreatedAt: tm(1)}
	s2 := &beadsapi.BeadDetail{ID: "gd-s2", Title: "Second step", Kind: "issue", Type: "task", Status: "open", CreatedAt: tm(2)}

	for _, b := range []*beadsapi.BeadDetail{plan, phase1, phase2, s1, s2} {
		f.beads[b.ID] = b
	}
	f.revDeps = map[string][]beadsapi.Dependency{
		"gd-plan1": {
			{BeadID: "gd-phase1", DependsOnID: "gd-plan1", Type: "parent-child"},
			{BeadID: "gd-phase2", DependsOnID: "gd-plan1", Type: "parent-child"},
		},
		"gd-phase1": {{BeadID: "gd-s1", DependsOnID: "gd-phase1", Type: "parent-child"}},
		"gd-phase2": {{BeadID: "gd-s2", DependsOnID: "gd-phase2", Type: "parent-child"}},
	}
	f.hotEpicsResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{plan}, Total: 1}
	f.epicSubtreeCount = map[string]int{"gd-plan1": 2, "gd-plan1|closed": 1}
	return f
}

// plan_grid_phase_label_test.go — ka-0000005352 iter-88. The mini task-grid's
// phase label ("Phase 1 — Settle the e2e-reference unknown") conventionally
// carries a boilerplate "Phase N — " lead. Measured on the live prod board: 92
// of 155 rendered labels carried it, and it burned ~60px of the label's ~112px
// visible width (54%) in the 239px lane while 132/155 labels already clipped to
// an ellipsis — so the ordinal+word crowded the DISTINGUISHING phase name out of
// view. splitPhaseLabel peels the ordinal into a compact fixed-width tag ("P1")
// so the reclaimed width goes to the name; the untouched full Title still rides
// the row tooltip, so zero information is lost. These tests pin the split
// behaviour, its wiring through buildPlanGrid, and the rendered markup.

// TestSplitPhaseLabel covers the prefix grammar splitPhaseLabel recognises and,
// critically, the cases it must LEAVE VERBATIM so no row ever loses its name or
// grows a spurious tag.
func TestSplitPhaseLabel(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantTag  string
		wantName string
	}{
		// em-dash, en-dash, and plain hyphen separators all peel.
		{"em dash", "Phase 1 — Settle the e2e-reference unknown", "P1", "Settle the e2e-reference unknown"},
		{"en dash", "Phase 2 – Make the number honest", "P2", "Make the number honest"},
		{"hyphen", "Phase 3 - Build coverage.yml", "P3", "Build coverage.yml"},
		{"phase zero", "Phase 0 — Foundations", "P0", "Foundations"},
		{"multi digit", "Phase 12 — Late phase", "P12", "Late phase"},
		{"no space before dash", "Phase 4— Compact", "P4", "Compact"},
		// A name that itself contains a dash keeps every later dash in the name.
		{"dash in name", "Phase 5 — Fully-supported local runs — no cloud", "P5", "Fully-supported local runs — no cloud"},

		// LEFT VERBATIM (empty tag, name == input):
		{"other trailer", "Other", "", "Other"},
		{"empty", "", "", ""},
		{"hand titled", "Research", "", "Research"},
		{"phase word only no ordinal", "Phase — Research", "", "Phase — Research"},
		{"no separator", "Phase 1 Settle", "", "Phase 1 Settle"},
		// A prefix with an EMPTY remainder must degrade to the verbatim title so a
		// bare "P3" tag never renders beside a blank name.
		{"empty remainder", "Phase 3 — ", "", "Phase 3 — "},
		{"empty remainder no trailing space", "Phase 3 —", "", "Phase 3 —"},
		// "Phase" mid-title must not match — the prefix is anchored to the start.
		{"phase mid title", "Prep for Phase 2 — details", "", "Prep for Phase 2 — details"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotTag, gotName := splitPhaseLabel(c.in)
			if gotTag != c.wantTag || gotName != c.wantName {
				t.Errorf("splitPhaseLabel(%q) = (%q, %q), want (%q, %q)", c.in, gotTag, gotName, c.wantTag, c.wantName)
			}
		})
	}
}

// TestBuildPlanGrid_PhaseLabelSplit proves buildPlanGrid populates PhaseTag /
// PhaseName off each phase's Title while keeping Title untouched (the tooltip
// source), and that the trailing "Other" row carries no tag — so the render can
// stay a thin split of precomputed fields.
func TestBuildPlanGrid_PhaseLabelSplit(t *testing.T) {
	plan := &beadsapi.BeadDetail{ID: "gd-plan", Type: "epic", Fields: map[string]string{
		"phase_order": `["gd-p1","gd-p2"]`,
	}}
	direct := map[string]*beadsapi.BeadDetail{
		"gd-p1":    {ID: "gd-p1", Type: "epic", Status: "in_progress", Title: "Phase 1 — Settle the unknown"},
		"gd-p2":    {ID: "gd-p2", Type: "epic", Status: "open", Title: "Phase 2 — Make it honest"},
		"gd-loose": {ID: "gd-loose", Type: "task", Status: "open", Title: "Loose"},
	}
	grand := map[string]*beadsapi.BeadDetail{
		"gd-a": {ID: "gd-a", Type: "task", Status: "closed", Title: "A"},
		"gd-b": {ID: "gd-b", Type: "task", Status: "open", Title: "B"},
	}
	bySub := map[string][]string{"gd-p1": {"gd-a"}, "gd-p2": {"gd-b"}}
	directIDs := []string{"gd-p1", "gd-p2", "gd-loose"}
	grid := buildPlanGrid(plan, []string{"gd-p1", "gd-p2"}, directIDs, bySub, direct, grand)

	if len(grid) != 3 {
		t.Fatalf("got %d rows, want 3 (2 phases + Other)", len(grid))
	}
	// Phase rows keep the FULL Title for the tooltip, and split it for the face.
	checkRow := func(i int, wantTitle, wantTag, wantName string) {
		r := grid[i]
		if r.Title != wantTitle {
			t.Errorf("row %d Title = %q, want %q (tooltip must keep full title)", i, r.Title, wantTitle)
		}
		if r.PhaseTag != wantTag || r.PhaseName != wantName {
			t.Errorf("row %d split = (%q, %q), want (%q, %q)", i, r.PhaseTag, r.PhaseName, wantTag, wantName)
		}
	}
	checkRow(0, "Phase 1 — Settle the unknown", "P1", "Settle the unknown")
	checkRow(1, "Phase 2 — Make it honest", "P2", "Make it honest")
	// The "Other" trailer carries no tag and mirrors its title into PhaseName.
	checkRow(2, "Other", "", "Other")
}

// TestPlansCard_PhaseTagRendered drives the real /plans render on a plan whose
// phase sub-epics carry the conventional "Phase N — …" titles and asserts the
// grid emits the compact .plan-taskgrid-phase-tag beside the truncating
// .plan-taskgrid-phase-label, with the FULL "Phase N — …" title still on the
// row's title tooltip — the two-derivations agreement (advice ka-0000005924)
// between what shows on the face and what the hover reveals.
func TestPlansCard_PhaseTagRendered(t *testing.T) {
	f := seedPhaseLabelPlanFake()
	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans", "matt@acme.example"))
	if w.Code != 200 {
		t.Fatalf("GET /plans: expected 200, got %d", w.Code)
	}
	html := w.Body.String()

	if !strings.Contains(html, `class="plan-taskgrid-phase-tag">P1<`) {
		t.Fatalf("expected the compact P1 tag in the rendered grid; got none.\nHTML:\n%s", html)
	}
	if !strings.Contains(html, `class="plan-taskgrid-phase-label">Settle the unknown<`) {
		t.Errorf("expected the phase NAME (prefix peeled) as the label text")
	}
	// The wrapper still carries the FULL "Phase N — " title as its tooltip, so the
	// ordinal+word peeled off the face is one hover away.
	if !strings.Contains(html, `class="plan-taskgrid-phase" title="Phase 1 &mdash; Settle the unknown"`) &&
		!strings.Contains(html, `class="plan-taskgrid-phase" title="Phase 1 — Settle the unknown"`) {
		t.Errorf("expected the phase wrapper to keep the full \"Phase 1 — …\" title tooltip; the split must not drop it.\nHTML:\n%s", html)
	}
}
