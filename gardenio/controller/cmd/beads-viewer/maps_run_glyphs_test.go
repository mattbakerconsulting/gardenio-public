package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// glyphIterBead builds an iteration agent bead owned by gardenerID (via trigger_id),
// at the given created_at, with the given status + wrapup. Mirrors the shape the
// detail page's gardenerIterations consumes.
func glyphIterBead(id, gardenerID string, created time.Time, status, wrapup string) *beadsapi.BeadDetail {
	fields := map[string]string{fieldTriggerID: gardenerID}
	if wrapup != "" {
		fields["wrapup"] = wrapup
	}
	return &beadsapi.BeadDetail{
		ID:        id,
		Title:     id,
		Type:      "agent",
		Status:    status,
		Fields:    fields,
		CreatedAt: created,
	}
}

// TestEnrichGardenerRunGlyphs_FallsBackToIterations: a gardener with an EMPTY
// ProgressState.Runs (the subtree-mode case — bug ka-0000001069) gets its
// recent-run glyph strip backfilled from its real iteration beads, ordered
// oldest→newest with the wrap-up outcome carried per glyph.
func TestEnrichGardenerRunGlyphs_FallsBackToIterations(t *testing.T) {
	f := newFake()
	base := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	f.iterationGlyphBeads = map[string][]*beadsapi.BeadDetail{
		"gd-sub": {
			// Out-of-order on purpose; deriveGardenerRunGlyphs re-sorts ascending.
			glyphIterBead("gd-i3", "gd-sub", base.Add(2*time.Hour), "open", ""),                             // live → flat
			glyphIterBead("gd-i1", "gd-sub", base, "closed", ""),                                            // clean close → ok
			glyphIterBead("gd-i2", "gd-sub", base.Add(time.Hour), "closed", `{"blockers":"stuck on auth"}`), // blocker → fail
		},
	}
	srv := NewServer(f, slog.Default(), "", "", "", nil)

	rows := []gardenerRowView{{ID: "gd-sub"}} // empty Runs
	srv.enrichGardenerRunGlyphs(context.Background(), rows)

	if len(rows[0].Runs) != 3 {
		t.Fatalf("expected 3 derived run glyphs, got %d", len(rows[0].Runs))
	}
	// Oldest→newest: i1 (ok), i2 (fail), i3 (flat).
	want := []string{"ok", "fail", "flat"}
	for i, g := range rows[0].Runs {
		if g.Outcome != want[i] {
			t.Errorf("glyph[%d] outcome = %q, want %q", i, g.Outcome, want[i])
		}
		if g.RunNumber != i+1 {
			t.Errorf("glyph[%d] RunNumber = %d, want %d", i, g.RunNumber, i+1)
		}
	}
}

// TestEnrichGardenerRunGlyphs_SkipsRowsWithRuns: a row that already carries a
// ProgressState-derived strip is left untouched (no fetch, no overwrite), so the
// richer scored sparkline wins over the iteration fallback.
func TestEnrichGardenerRunGlyphs_SkipsRowsWithRuns(t *testing.T) {
	f := newFake()
	f.iterationGlyphBeads = map[string][]*beadsapi.BeadDetail{
		"gd-has": {glyphIterBead("gd-x", "gd-has", time.Now(), "closed", "")},
	}
	srv := NewServer(f, slog.Default(), "", "", "", nil)

	existing := []gardenerRunGlyph{{RunNumber: 1, Outcome: "ok", Height: 50}}
	rows := []gardenerRowView{{ID: "gd-has", Runs: existing}}
	srv.enrichGardenerRunGlyphs(context.Background(), rows)

	if len(rows[0].Runs) != 1 || rows[0].Runs[0].Height != 50 {
		t.Errorf("row with existing Runs should be untouched, got %+v", rows[0].Runs)
	}
}

// TestEnrichGardenerRunGlyphs_SkipsExpiredShells: never-ran spawn shells
// (agent_state=expired) are not runs (bug ka-0000006805) and must not be drawn
// as glyphs.
func TestEnrichGardenerRunGlyphs_SkipsExpiredShells(t *testing.T) {
	f := newFake()
	base := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	shell := glyphIterBead("gd-shell", "gd-sub", base.Add(time.Hour), "closed", "")
	shell.Fields[fieldAgentState] = agentStateExpired
	f.iterationGlyphBeads = map[string][]*beadsapi.BeadDetail{
		"gd-sub": {
			glyphIterBead("gd-real", "gd-sub", base, "closed", ""),
			shell,
		},
	}
	srv := NewServer(f, slog.Default(), "", "", "", nil)

	rows := []gardenerRowView{{ID: "gd-sub"}}
	srv.enrichGardenerRunGlyphs(context.Background(), rows)

	if len(rows[0].Runs) != 1 {
		t.Fatalf("expected 1 glyph (expired shell skipped), got %d", len(rows[0].Runs))
	}
}

// TestEnrichGardenerRunGlyphs_NoIterationsLeavesEmpty: a gardener with no findable
// iterations keeps its empty strip (the card degrades to counters), never panics.
func TestEnrichGardenerRunGlyphs_NoIterationsLeavesEmpty(t *testing.T) {
	f := newFake()
	srv := NewServer(f, slog.Default(), "", "", "", nil)
	rows := []gardenerRowView{{ID: "gd-none"}}
	srv.enrichGardenerRunGlyphs(context.Background(), rows)
	if len(rows[0].Runs) != 0 {
		t.Errorf("expected empty strip for a gardener with no iterations, got %d", len(rows[0].Runs))
	}
}

// TestHandleMaps_RunGlyphFallbackEndToEnd: a subtree-mode gardener whose
// ProgressState is empty but which has real closed iteration beads renders a
// non-empty recent-run strip + sparkline on /maps (acceptance criterion for
// bug ka-0000001069).
func TestHandleMaps_RunGlyphFallbackEndToEnd(t *testing.T) {
	f := newFake()
	// Gardener with assign_bead set and NO progress_state.runs — the real-world
	// subtree-mode shape that rendered an empty strip before the fix.
	spec := map[string]any{
		"name":          "subtree-gardener",
		"terminal_goal": "drain the epic",
		"assign_bead":   "gd-epic",
	}
	raw, _ := json.Marshal(spec)
	f.triggerScheduleResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{{
			ID:        "gd-sub",
			Title:     "subtree-gardener",
			Type:      "trigger",
			Status:    "open",
			Labels:    []string{"trigger:schedule", "role:gardener", "project:gardenio"},
			Fields:    map[string]string{"role": "gardener", "gardener": string(raw)},
			UpdatedAt: time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
		}},
		Total: 1,
	}
	base := time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC)
	f.iterationGlyphBeads = map[string][]*beadsapi.BeadDetail{
		"gd-sub": {
			glyphIterBead("gd-i1", "gd-sub", base, "closed", ""),
			glyphIterBead("gd-i2", "gd-sub", base.Add(time.Hour), "closed", ""),
			glyphIterBead("gd-i3", "gd-sub", base.Add(2*time.Hour), "closed", ""),
		},
	}
	mux := mkServerWithNow(t, f, mapsNow)

	req := httptest.NewRequest("GET", "/maps", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, "gardener-spark-bar") {
		t.Errorf("expected a non-empty recent-run strip (gardener-spark-bar) from the iteration fallback")
	}
	// 3 runs ≥ mapsSparkMinRuns, so the sparkline draws too.
	if !strings.Contains(body, "maps-runs-spark") {
		t.Errorf("expected the verdict sparkline (maps-runs-spark) from the iteration fallback")
	}
}
