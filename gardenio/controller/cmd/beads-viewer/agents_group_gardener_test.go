package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TestCollapseLineages_CollapsesChainToLiveHead verifies that a succession chain
// (three incarnations sharing a lineage_root_id, linked by prior_agent_id)
// collapses to its newest incarnation as the head with the older two folded into
// Successors newest-first.
func TestCollapseLineages_CollapsesChainToLiveHead(t *testing.T) {
	base := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	rows := []agentRowView{
		// The root (oldest) — no prior pointer.
		{ID: "gd-r1", Name: "cap-run-1", LineageRootID: "gd-r1", PriorAgentID: "", CreatedAt: base},
		// Middle incarnation — points back at the root.
		{ID: "gd-r2", Name: "cap-run-2", LineageRootID: "gd-r1", PriorAgentID: "gd-r1", CreatedAt: base.Add(time.Hour)},
		// Newest incarnation (live head) — points back at the middle; nothing
		// points at it.
		{ID: "gd-r3", Name: "cap-run-3", LineageRootID: "gd-r1", PriorAgentID: "gd-r2", CreatedAt: base.Add(2 * time.Hour)},
	}

	groups := collapseLineages(rows)
	if len(groups) != 1 {
		t.Fatalf("expected 1 lineage group, got %d", len(groups))
	}
	g := groups[0]
	if g.Head.ID != "gd-r3" {
		t.Errorf("head = %q, want gd-r3 (newest, unpointed-at)", g.Head.ID)
	}
	if !g.HasSuccessors() || g.SuccessorCount() != 2 {
		t.Fatalf("want 2 successors, got %d", g.SuccessorCount())
	}
	// Successors newest-first: gd-r2 then gd-r1.
	if g.Successors[0].ID != "gd-r2" || g.Successors[1].ID != "gd-r1" {
		t.Errorf("successors = [%s %s], want [gd-r2 gd-r1]", g.Successors[0].ID, g.Successors[1].ID)
	}
}

// TestCollapseLineages_FirstOfLineHasNoSuccessors verifies a lone agent (its own
// root, no prior) is a one-run chain with no expandable history.
func TestCollapseLineages_FirstOfLineHasNoSuccessors(t *testing.T) {
	rows := []agentRowView{
		{ID: "gd-solo", Name: "solo", LineageRootID: "gd-solo", CreatedAt: time.Now()},
	}
	groups := collapseLineages(rows)
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	if groups[0].HasSuccessors() {
		t.Errorf("first-of-line agent should have no successors, got %d", groups[0].SuccessorCount())
	}
}

// TestBuildAgentGardenerGroups_BucketsRealAndSyntheticSeparately verifies the
// builder buckets rows under their real gardener (with a rich header pulled from
// the gardener bead) and routes synthetic-owned rows into the fleet-direct lane,
// which sorts last.
func TestBuildAgentGardenerGroups_BucketsRealAndSyntheticSeparately(t *testing.T) {
	base := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	f := newFake()
	// A real gardener (trigger) bead so the header resolves.
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-cap": {
			ID:     "gd-cap",
			Title:  "delivery-drive gardener",
			Type:   "trigger",
			Status: "open",
			Fields: map[string]string{
				"enabled":       "true",
				"terminal_goal": "drain the delivery epic",
			},
		},
	}
	srv := NewServer(f, testLogger(), "", "", "", nil)
	srv.SetNow(func() time.Time { return base.Add(3 * time.Hour) })

	rows := []agentRowView{
		// Owned by the real gardener.
		{ID: "gd-a1", Name: "crew-1", GardenerID: "gd-cap", LineageRootID: "gd-a1", CreatedAt: base.Add(2 * time.Hour)},
		// Gardener-less (synthetic owner) — fleet-direct lane.
		{ID: "gd-a2", Name: "oneoff", GardenerID: "synthetic-gardener:unknown", LineageRootID: "gd-a2", CreatedAt: base.Add(time.Hour)},
	}

	buckets := srv.buildAgentGardenerGroups(context.Background(), rows)
	if len(buckets) != 2 {
		t.Fatalf("expected 2 buckets, got %d", len(buckets))
	}
	// Real gardener bucket leads (its head is newer); fleet-direct is last.
	if buckets[0].Gardener == nil {
		t.Fatalf("first bucket should be the real gardener, got nil Gardener (label=%q)", buckets[0].Label)
	}
	if buckets[0].GardenerID != "gd-cap" {
		t.Errorf("first bucket GardenerID = %q, want gd-cap", buckets[0].GardenerID)
	}
	if buckets[0].Gardener.Name == "" {
		t.Errorf("real gardener header should carry a name")
	}
	last := buckets[len(buckets)-1]
	if !last.FleetDirect {
		t.Errorf("last bucket should be the fleet-direct lane, got FleetDirect=%v label=%q", last.FleetDirect, last.Label)
	}
	if last.Gardener != nil {
		t.Errorf("fleet-direct lane should carry no gardener header")
	}
	if last.AgentCount != 1 {
		t.Errorf("fleet-direct count = %d, want 1", last.AgentCount)
	}
}

// TestBuildAgentGardenerGroups_UnresolvableGardenerDegradesToLabel verifies that a
// real (non-synthetic) gardener id whose bead cannot be fetched still forms a
// bucket, labeled by the id, rather than being dropped.
func TestBuildAgentGardenerGroups_UnresolvableGardenerDegradesToLabel(t *testing.T) {
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{} // gardener bead absent
	srv := NewServer(f, testLogger(), "", "", "", nil)
	srv.SetNow(time.Now)

	rows := []agentRowView{
		{ID: "gd-a1", Name: "crew-1", GardenerID: "gd-missing", LineageRootID: "gd-a1", CreatedAt: time.Now()},
	}
	buckets := srv.buildAgentGardenerGroups(context.Background(), rows)
	if len(buckets) != 1 {
		t.Fatalf("expected 1 bucket, got %d", len(buckets))
	}
	if buckets[0].Gardener != nil {
		t.Errorf("unresolvable gardener should have nil header")
	}
	if buckets[0].Label != "gd-missing" {
		t.Errorf("label = %q, want the gardener id gd-missing", buckets[0].Label)
	}
}

// TestHandleAgents_GroupGardener_RendersBucketsAndLineage drives the full handler
// with ?group=gardener and asserts the rendered page groups by gardener, shows the
// gardener header, collapses the chain to its head, and offers the expandable
// successor history.
func TestHandleAgents_GroupGardener_RendersBucketsAndLineage(t *testing.T) {
	now := time.Now()
	f := newFake()
	// The gardener bead (resolved for the bucket header).
	capBead := &beadsapi.BeadDetail{
		ID:     "gd-cap",
		Title:  "delivery gardener",
		Type:   "trigger",
		Status: "open",
		Fields: map[string]string{"enabled": "true", "terminal_goal": "drain delivery"},
	}
	f.beads = map[string]*beadsapi.BeadDetail{"gd-cap": capBead}
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			// Live head of a two-run chain owned by gd-cap.
			{
				ID:        "gd-head",
				Title:     "crew-head",
				Type:      "agent",
				Status:    "in_progress",
				Labels:    []string{"project:gardenio"},
				UpdatedAt: now.Add(-2 * time.Minute),
				CreatedAt: now.Add(-10 * time.Minute),
				Fields: map[string]string{
					"agent":           "crew-head",
					"role":            "crew",
					"agent_state":     "working",
					"gardener_id":     "gd-cap",
					"lineage_root_id": "gd-old",
					"prior_agent_id":  "gd-old",
					"project":         "gardenio",
				},
			},
			// The older incarnation (successor history) — same root, no prior.
			{
				ID:        "gd-old",
				Title:     "crew-old",
				Type:      "agent",
				Status:    "in_progress",
				Labels:    []string{"project:gardenio"},
				UpdatedAt: now.Add(-40 * time.Minute),
				CreatedAt: now.Add(-50 * time.Minute),
				Fields: map[string]string{
					"agent":           "crew-old",
					"role":            "crew",
					"agent_state":     "working",
					"gardener_id":     "gd-cap",
					"lineage_root_id": "gd-old",
					"project":         "gardenio",
				},
			},
			// A gardener-less agent → fleet-direct lane.
			{
				ID:        "gd-solo",
				Title:     "crew-solo",
				Type:      "agent",
				Status:    "in_progress",
				Labels:    []string{"project:gardenio"},
				UpdatedAt: now.Add(-5 * time.Minute),
				CreatedAt: now.Add(-6 * time.Minute),
				Fields: map[string]string{
					"agent":       "crew-solo",
					"role":        "crew",
					"agent_state": "working",
					"project":     "gardenio",
					// no gardener_id / schedule_id → synthetic-gardener:unknown
				},
			},
		},
		Total: 3,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/agents?group=gardener", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	for _, want := range []string{
		// Gardener bucket header + link to its life-thread.
		"delivery gardener",
		`href="/gardener/gd-cap"`,
		// The live head renders.
		"crew-head",
		// Expandable successor history.
		"<details",
		"earlier incarnation",
		// The folded older incarnation is inside.
		"crew-old",
		// Fleet-direct lane for the gardener-less agent.
		"Fleet-direct",
		"crew-solo",
		// The group toggle marks gardener active.
		"group",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("group=gardener page missing %q\nbody=%s", want, body)
		}
	}
}

// TestHandleAgents_DefaultGroupIsProject verifies the default (no ?group) view
// still groups by project and does NOT render gardener buckets — the new mode is
// strictly opt-in.
func TestHandleAgents_DefaultGroupIsProject(t *testing.T) {
	now := time.Now()
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-a1",
				Title:     "crew-1",
				Type:      "agent",
				Status:    "in_progress",
				Labels:    []string{"project:gardenio"},
				UpdatedAt: now.Add(-2 * time.Minute),
				Fields:    map[string]string{"agent": "crew-1", "role": "crew", "project": "gardenio"},
			},
		},
		Total: 1,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/agents", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()

	if !strings.Contains(body, "<h2>gardenio") {
		t.Errorf("default view should group by project (missing <h2>gardenio)\nbody=%s", body)
	}
	if strings.Contains(body, "Fleet-direct") {
		t.Errorf("default view should not render the gardener fleet-direct lane")
	}
}
