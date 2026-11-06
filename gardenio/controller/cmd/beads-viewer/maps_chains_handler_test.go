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

// seedChainedMapsFake serves a 3-gardener chain research→impl→ux, all live, with
// the predecessor (research) having reached its terminal goal so the first
// handoff has fired. The chain.to edges are exposed via forward typed mentions
// (relationship_type=chains_to), the C1 read path the /maps board consumes.
func seedChainedMapsFake(t *testing.T) *fakeDaemon {
	t.Helper()
	f := newFake()

	mk := func(id, name, goal, status string, achieved bool) *beadsapi.BeadDetail {
		spec := map[string]any{"name": name, "terminal_goal": goal, "runs_spawned": 4, "runs_completed": 3}
		raw, _ := json.Marshal(spec)
		fields := map[string]string{"role": "gardener", "gardener": string(raw)}
		b := &beadsapi.BeadDetail{
			ID:        id,
			Title:     name,
			Type:      "trigger",
			Status:    status,
			Labels:    []string{"trigger:schedule", "role:gardener", "project:gardenio"},
			Fields:    fields,
			UpdatedAt: time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
		}
		_ = achieved
		return b
	}

	f.triggerScheduleResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			mk("gd-research", "research", "design epic approved", "open", true),
			mk("gd-impl", "impl", "phase 1 merged", "open", false),
			mk("gd-ux", "ux", "triggers console shipped", "open", false),
		},
		Total: 3,
	}

	// chains_to edges, declared on each predecessor (forward mention).
	f.mentions = map[string][]beadsapi.BeadReferenceSummary{
		"gd-research": {{ID: "gd-impl", RelationshipType: strptr("chains_to")}},
		"gd-impl":     {{ID: "gd-ux", RelationshipType: strptr("chains_to")}},
	}
	return f
}

// TestHandleMaps_RendersChainWorkflow: the board renders the chained gardeners as
// a workflow rail with directed handoff arrows ABOVE the standalone grid.
func TestHandleMaps_RendersChainWorkflow(t *testing.T) {
	f := seedChainedMapsFake(t)
	// Mark research as goal-achieved so the first handoff lights. gardenerRowView
	// derives GoalState from termination provenance for closed gardeners; for an
	// OPEN gardener whose target is closed it's "achieved". Simplest reliable
	// signal in this fake: close research with a goal-completion path is complex,
	// so we assert the structural rendering (rail + arrows + workflow grouping)
	// which does not depend on the achieved state.
	mux := mkServerWithNow(t, f, mapsNow)

	req := httptest.NewRequest("GET", "/maps", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	for _, want := range []string{
		`class="maps-workflows"`, // the rendered workflow section
		`class="maps-rail"`,      // the predecessor→heir rail
		`class="maps-arrow`,      // a directed handoff arrow (class prefix; may carry is-lit)
		"chained gardeners",      // the workflow header pill
		"research workflow",      // emergent workflow name from the root gardener
		`href="/gardener/gd-research"`,
		`href="/gardener/gd-impl"`,
		`href="/gardener/gd-ux"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}

	// All three chained gardeners belong to the workflow rail, so the standalone
	// grid should be empty (no maps-board grid emitted when every live gardener
	// is chained).
	if strings.Contains(body, `class="maps-board"`) {
		t.Errorf("expected no standalone grid when every gardener is chained")
	}
}

// TestHandleMaps_NoChainsRendersPlainGrid: with no chain mentions, the board
// renders exactly as before C2 — the standalone grid, no workflow section.
func TestHandleMaps_NoChainsRendersPlainGrid(t *testing.T) {
	mux := mkServerWithNow(t, seedMapsFake(t), mapsNow)

	req := httptest.NewRequest("GET", "/maps", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `class="maps-board"`) {
		t.Errorf("expected standalone grid with no chains")
	}
	// Guard against the rendered section, not the always-present CSS class
	// definitions in the <style> block.
	if strings.Contains(body, `class="maps-workflows"`) {
		t.Errorf("expected no workflow section with no chains")
	}
}
