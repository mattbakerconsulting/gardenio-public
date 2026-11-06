package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// rollupEngine wires a TriggerEngine with the uncovered-open-lane roll-up sweep
// enabled, a recording notifier, a mutable clock (for the once-per-day throttle),
// and a parentOf map on the daemon so the child-count Parent query is honored.
// Mirrors reaperEngine / drainEngine.
func rollupEngine(t *testing.T, parentOf map[string]string, beads ...*beadsapi.BeadDetail) (*TriggerEngine, *mockTriggerDaemon, *recordingNotifier, *time.Time) {
	t.Helper()
	daemon := &mockTriggerDaemon{beads: beads, parentOf: parentOf}
	rec := &recordingNotifier{}
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	clk := &now
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon:                         daemon,
		Notifier:                       rec.notify,
		Logger:                         slog.Default(),
		UncoveredOpenLaneRollupEnabled: true,
		UncoveredOpenLaneRollupNotify:  true,
		Clock:                          func() time.Time { return *clk },
	})
	return engine, daemon, rec, clk
}

// closedEnabledLane builds a closed+enabled non-manual trigger anchored to an
// epic via the gardener-epic:<id> label — the base shape of a roll-up candidate.
func closedEnabledLane(id, title, epicID string, extraFields map[string]string, labels ...string) *beadsapi.BeadDetail {
	fields := map[string]string{
		"enabled": "true",
		"project": "gardenio",
	}
	for k, v := range extraFields {
		fields[k] = v
	}
	allLabels := append([]string{"gardener-epic:" + epicID}, labels...)
	return &beadsapi.BeadDetail{
		ID:     id,
		Type:   "trigger",
		Status: "closed",
		Title:  title,
		Labels: allLabels,
		Fields: fields,
	}
}

// rollupEpic builds an epic bead of the given status.
func rollupEpic(id, title, status string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{ID: id, Type: "epic", Status: status, Title: title}
}

// rollupChild builds an open child bead parented (via parentOf in the fixture) to
// an epic, so the Parent child-count query sees it.
func rollupChild(id, status string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:        id,
		Type:      "task",
		Status:    status,
		Title:     "child " + id,
		UpdatedAt: time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC),
	}
}

// createdDoc returns the single roll-up doc CreateBead request, or fails.
func createdDoc(t *testing.T, d *mockTriggerDaemon) beadsapi.CreateBeadRequest {
	t.Helper()
	var found []beadsapi.CreateBeadRequest
	for _, r := range d.createdBeads {
		if r.Type == "doc" {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly 1 doc created, got %d", len(found))
	}
	return found[0]
}

// docContent extracts fields.content from a create request.
func docContent(t *testing.T, req beadsapi.CreateBeadRequest) string {
	t.Helper()
	var f struct {
		Content string `json:"content"`
		Format  string `json:"format"`
	}
	if err := json.Unmarshal(req.Fields, &f); err != nil {
		t.Fatalf("unmarshal doc fields: %v", err)
	}
	if f.Format != "md" {
		t.Errorf("doc format = %q, want md", f.Format)
	}
	return f.Content
}

func TestUncoveredOpenLaneRollup_Disabled(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon:                         daemon,
		Logger:                         slog.Default(),
		UncoveredOpenLaneRollupEnabled: false,
		Clock:                          func() time.Time { return time.Now() },
	})
	engine.SweepUncoveredOpenLaneRollup(context.Background())
	if len(daemon.createdBeads) != 0 || len(daemon.updatedIDs) != 0 {
		t.Fatalf("disabled sweep must be a no-op; created=%d updated=%d", len(daemon.createdBeads), len(daemon.updatedIDs))
	}
}

func TestUncoveredOpenLaneRollup_SurfacesGenuinelyUncovered(t *testing.T) {
	parentOf := map[string]string{"gd-child1": "gd-epicA", "gd-child2": "gd-epicA"}
	engine, daemon, rec, _ := rollupEngine(t, parentOf,
		closedEnabledLane("gd-lane1", "Users-CAMP lane", "gd-epicA", nil),
		rollupEpic("gd-epicA", "Users CAMP epic", "open"),
		rollupChild("gd-child1", "open"),
		rollupChild("gd-child2", "in_progress"),
	)
	engine.SweepUncoveredOpenLaneRollup(context.Background())

	req := createdDoc(t, daemon)
	if req.Title != uncoveredOpenLaneRollupTitle {
		t.Errorf("doc title = %q, want %q", req.Title, uncoveredOpenLaneRollupTitle)
	}
	hasMarker, hasProject := false, false
	for _, l := range req.Labels {
		if l == uncoveredOpenLaneRollupMarkerLabel {
			hasMarker = true
		}
		if l == "project:gardenio" {
			hasProject = true
		}
	}
	if !hasMarker || !hasProject {
		t.Errorf("doc labels = %v, want marker + project:gardenio", req.Labels)
	}
	body := docContent(t, req)
	if !strings.Contains(body, "gd-lane1") || !strings.Contains(body, "gd-epicA") {
		t.Errorf("report body missing the uncovered lane / epic:\n%s", body)
	}
	if !strings.Contains(body, "| 2 |") {
		t.Errorf("report body should show 2 open children:\n%s", body)
	}
	// System operator must be stamped so the daemon does not fail-closed.
	if len(daemon.createSystemOperators) == 0 || daemon.createSystemOperators[0] != uncoveredOpenLaneRollupSystemOperator {
		t.Errorf("system operator = %v, want %q", daemon.createSystemOperators, uncoveredOpenLaneRollupSystemOperator)
	}
	if len(rec.calls) != 1 {
		t.Errorf("expected 1 notify for a non-empty report, got %d", len(rec.calls))
	}
}

func TestUncoveredOpenLaneRollup_ExcludesCoveredAndRetired(t *testing.T) {
	parentOf := map[string]string{
		"gd-cOpen": "gd-epicCovered",
		"gd-oOpen": "gd-epicOpenClosedAnchor", // unused epic status matters more
		"gd-dOpen": "gd-epicDrained",
	}
	engine, daemon, _, _ := rollupEngine(t, parentOf,
		// Covered: has a live open+enabled successor gardener on the same epic.
		closedEnabledLane("gd-covered", "covered lane", "gd-epicCovered", nil),
		&beadsapi.BeadDetail{ID: "gd-successor", Type: "trigger", Status: "open", Title: "successor",
			Labels: []string{"gardener-epic:gd-epicCovered"}, Fields: map[string]string{"enabled": "true"}},
		rollupEpic("gd-epicCovered", "covered epic", "open"),
		rollupChild("gd-cOpen", "open"),

		// Closed anchor epic → fix 1 territory, excluded.
		closedEnabledLane("gd-closedanchor", "closed-anchor lane", "gd-p5i2epJtiS", nil),
		rollupEpic("gd-p5i2epJtiS", "closed epic", "closed"),

		// Non-epic anchor (the ka-0000006579 false positive) → excluded.
		closedEnabledLane("gd-nonepic", "non-epic anchor lane", "gd-notanepic", nil),
		&beadsapi.BeadDetail{ID: "gd-notanepic", Type: "trigger", Status: "open", Title: "not an epic",
			Fields: map[string]string{"enabled": "true"}},

		// Drained open epic (0 open children) → excluded.
		closedEnabledLane("gd-drained", "drained lane", "gd-epicDrained", nil),
		rollupEpic("gd-epicDrained", "drained epic", "open"),
		rollupChild("gd-dClosed", "closed"), // closed child does not count

		// Engine-retired (terminate marker) → drain fix 3 territory, excluded.
		closedEnabledLane("gd-retired", "retired lane", "gd-epicRetired",
			map[string]string{"gardener_terminated_kind": "bead_query"}),
		rollupEpic("gd-epicRetired", "retired-anchor epic", "open"),

		// Unanchored (no gardener-epic label) → drain fix 2 territory, excluded.
		&beadsapi.BeadDetail{ID: "gd-0ghfjfsdvo", Type: "trigger", Status: "closed", Title: "unanchored",
			Fields: map[string]string{"enabled": "true"}},

		// Manual + E2E-fixture → excluded defensively.
		closedEnabledLane("gd-manual", "manual lane", "gd-MoWkMnETlf",
			map[string]string{"trigger_mode": "manual"}),
		rollupEpic("gd-MoWkMnETlf", "manual epic", "open"),
	)
	// Give the drained epic a closed child under it.
	daemon.parentOf["gd-dClosed"] = "gd-epicDrained"

	engine.SweepUncoveredOpenLaneRollup(context.Background())

	req := createdDoc(t, daemon)
	body := docContent(t, req)
	if !strings.Contains(body, "0 uncovered lanes this cycle") {
		t.Errorf("expected an empty (0-lane) report, got:\n%s", body)
	}
	for _, id := range []string{"gd-covered", "gd-closedanchor", "gd-nonepic", "gd-drained", "gd-retired", "gd-0ghfjfsdvo", "gd-manual"} {
		if strings.Contains(body, id) {
			t.Errorf("report must NOT list excluded lane %s:\n%s", id, body)
		}
	}
}

func TestUncoveredOpenLaneRollup_UpdatesInPlace(t *testing.T) {
	parentOf := map[string]string{"gd-child1": "gd-epicA"}
	existingDoc := &beadsapi.BeadDetail{
		ID:     "gd-existingrollup",
		Type:   "doc",
		Status: "open",
		Title:  uncoveredOpenLaneRollupTitle,
		Labels: []string{"project:gardenio", uncoveredOpenLaneRollupMarkerLabel},
		Fields: map[string]string{"content": "stale", "format": "md"},
	}
	engine, daemon, _, _ := rollupEngine(t, parentOf,
		existingDoc,
		closedEnabledLane("gd-lane1", "lane", "gd-epicA", nil),
		rollupEpic("gd-epicA", "epic", "open"),
		rollupChild("gd-child1", "open"),
	)
	engine.SweepUncoveredOpenLaneRollup(context.Background())

	for _, r := range daemon.createdBeads {
		if r.Type == "doc" {
			t.Fatalf("existing living doc must be updated, not re-created")
		}
	}
	if len(daemon.updatedIDs) != 1 || daemon.updatedIDs[0] != "gd-existingrollup" {
		t.Fatalf("expected in-place update of gd-existingrollup, got updatedIDs=%v", daemon.updatedIDs)
	}
	if body := daemon.updatedFields[0]["content"]; !strings.Contains(body, "gd-lane1") {
		t.Errorf("updated content should list the uncovered lane, got:\n%s", body)
	}
}

func TestUncoveredOpenLaneRollup_Throttle(t *testing.T) {
	parentOf := map[string]string{"gd-child1": "gd-epicA"}
	engine, daemon, _, clk := rollupEngine(t, parentOf,
		closedEnabledLane("gd-lane1", "lane", "gd-epicA", nil),
		rollupEpic("gd-epicA", "epic", "open"),
		rollupChild("gd-child1", "open"),
	)
	engine.SweepUncoveredOpenLaneRollup(context.Background())
	firstCreates := len(daemon.createdBeads)
	if firstCreates == 0 {
		t.Fatal("first run should have created the report")
	}
	// Second run 1h later is inside the 24h throttle → no-op.
	*clk = clk.Add(time.Hour)
	engine.SweepUncoveredOpenLaneRollup(context.Background())
	if len(daemon.createdBeads) != firstCreates {
		t.Fatalf("second run within throttle must be a no-op; creates %d -> %d", firstCreates, len(daemon.createdBeads))
	}
	// A day later it is due again (the doc now exists, so it updates in place).
	*clk = clk.Add(25 * time.Hour)
	engine.SweepUncoveredOpenLaneRollup(context.Background())
	// No existing doc in m.beads (CreateBead does not add it), so it creates again;
	// either way the sweep ran — assert it produced a write this time.
	if len(daemon.createdBeads) == firstCreates && len(daemon.updatedIDs) == 0 {
		t.Fatal("run after throttle window should have refreshed the report")
	}
}
