package main

// crew_takeover_test.go — the acceptance test for bug gd-bcdJr7cT0L.
//
// It replays the 2026-09-21 incident exactly: a FRESH gardener instance takes
// over an epic whose predecessor instances left two crews alive. Before the fix
// `gb crew` filtered on spawned_by == <my identity>, the new instance matched
// neither predecessor, printed "No active crew", called the two in_progress
// tasks orphaned and dispatched duplicates.
//
// The test asserts the two things the bead's acceptance criterion names:
//   1. the fresh instance LISTS both predecessor crews, and
//   2. both of their tasks come back OWNED — so there is no free slot to
//      dispatch into.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// takeoverFixture is the live-fleet snapshot from the incident.
const (
	takeoverEpic = "gd-6fmhiieilr" // the taints epic the gardener is pinned to

	// The fresh gardener instance: a NEW identity, spawned by the trigger.
	takeoverNewGardenerBead  = "gd-CAPNEW"
	takeoverNewGardenerAgent = "gardenio-gardener-for-gd-6fmhiieilr-2tkj"

	// Predecessor crews — alive, and spawned by identities that no longer exist.
	takeoverCrewATask  = "gd-KPFzrQIvnc"
	takeoverCrewBTask  = "gd-lbEqC5ngPM"
	takeoverCrewASpawn = "gardenio-gardener-for-gd-6fmhiieilr-927i"
	takeoverCrewBSpawn = "gardenio-plan-gardener-e835"
)

// newTakeoverDaemon serves the incident's bead graph: the epic's subtree, the
// active agent beads, and the individual bead reads gb crew issues.
func newTakeoverDaemon(t *testing.T, now time.Time) *httptest.Server {
	t.Helper()
	stamp := func(d time.Duration) string { return now.Add(-d).UTC().Format(time.RFC3339) }

	beads := map[string]map[string]any{
		takeoverEpic:            {"id": takeoverEpic, "title": "Taints epic", "type": "epic", "status": "in_progress"},
		takeoverCrewATask:       {"id": takeoverCrewATask, "title": "Taint propagation", "type": "task", "status": "in_progress"},
		takeoverCrewBTask:       {"id": takeoverCrewBTask, "title": "kbeads data race", "type": "task", "status": "in_progress"},
		takeoverNewGardenerBead: {"id": takeoverNewGardenerBead, "title": takeoverNewGardenerAgent, "type": "agent", "status": "open"},
	}
	// The fresh gardener's own agent bead: role=gardener, pinned to the epic via
	// task_id — this is what makes the tree scope auto-derivable.
	beads[takeoverNewGardenerBead]["fields"] = map[string]string{
		"agent":      takeoverNewGardenerAgent,
		"project":    "gardenio",
		"role":       "gardener",
		"mode":       "crew",
		"task_id":    takeoverEpic,
		"spawned_by": "trigger:gd-TRIG",
		"created_at": stamp(2 * time.Minute),
	}

	agentList := []map[string]any{
		{
			"id": takeoverNewGardenerBead, "title": takeoverNewGardenerAgent, "type": "agent", "status": "open",
			"fields": beads[takeoverNewGardenerBead]["fields"],
		},
		{
			"id": "gd-rEeecH6nCL", "title": "taint-propagation-on-9py", "type": "agent", "status": "open",
			"fields": map[string]string{
				"agent": "taint-propagation-on-9py", "project": "gardenio", "role": "crew", "mode": "crew",
				"task_id": takeoverCrewATask, "spawned_by": takeoverCrewASpawn,
				"agent_state": "working", "created_at": stamp(28 * time.Minute),
				"last_progress_at": stamp(2 * time.Minute),
			},
		},
		{
			"id": "gd-5yTudj6Z0W", "title": "kbeads-data-race-x7q", "type": "agent", "status": "open",
			"fields": map[string]string{
				"agent": "kbeads-data-race-x7q", "project": "gardenio", "role": "crew", "mode": "crew",
				"task_id": takeoverCrewBTask, "spawned_by": takeoverCrewBSpawn,
				"agent_state": "working", "created_at": stamp(24 * time.Minute),
				"last_progress_at": stamp(3 * time.Minute),
			},
		},
	}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if id, ok := strings.CutPrefix(r.URL.Path, "/v1/beads/"); ok && id != "" {
			b, found := beads[id]
			if !found {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "not found"})
				return
			}
			_ = json.NewEncoder(w).Encode(b)
			return
		}
		q := r.URL.Query()
		if parent := q.Get("parent"); parent != "" {
			if parent != takeoverEpic {
				_ = json.NewEncoder(w).Encode(map[string]any{"beads": []map[string]any{}, "total": 0})
				return
			}
			children := []map[string]any{beads[takeoverCrewATask], beads[takeoverCrewBTask]}
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": children, "total": len(children)})
			return
		}
		if q.Get("assignee") != "" {
			// resolveAgentByActor fallback — unused here (KD_AGENT_ID is set).
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": []map[string]any{}, "total": 0})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": agentList, "total": len(agentList)})
	}))
}

func newCrewCmd(t *testing.T, tree string) (*cobra.Command, *strings.Builder) {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.Flags().Bool("all", false, "")
	cmd.Flags().String("project", "gardenio", "")
	cmd.Flags().String("tree", tree, "")
	cmd.SetContext(context.Background())
	var buf strings.Builder
	cmd.SetOut(&buf)
	return cmd, &buf
}

// TestCrewTakeoverSeesPredecessorCrews is the bead's acceptance criterion: a
// fresh gardener instance on an epic with live predecessor crews LISTS them.
func TestCrewTakeoverSeesPredecessorCrews(t *testing.T) {
	srv := newTakeoverDaemon(t, time.Now())
	defer srv.Close()
	swapDaemon(t, srv.URL)

	// The fresh instance's identity: nothing in the fleet was spawned by it.
	t.Setenv("KD_AGENT_ID", takeoverNewGardenerBead)
	oldActor := actor
	actor = takeoverNewGardenerAgent
	t.Cleanup(func() { actor = oldActor })

	cmd, buf := newCrewCmd(t, "")
	if err := runCrew(cmd, nil); err != nil {
		t.Fatalf("runCrew: %v", err)
	}
	got := buf.String()

	for _, want := range []string{"taint-propagation-on-9py", "kbeads-data-race-x7q"} {
		if !strings.Contains(got, want) {
			t.Errorf("fresh gardener did not list live predecessor crew %q — this is gd-bcdJr7cT0L.\nOutput:\n%s", want, got)
		}
	}
	if strings.Contains(got, "No active crew") {
		t.Errorf("fresh gardener reported an empty crew while two crews were alive.\nOutput:\n%s", got)
	}
	// It must not count itself as its own crew. (The header names the actor, so
	// check the listing rows, not the whole output.)
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "  working") && strings.Contains(line, takeoverNewGardenerAgent) {
			t.Errorf("gardener listed itself as its own crew.\nOutput:\n%s", got)
		}
	}
	if !strings.Contains(got, "Your crew (2 agents") {
		t.Errorf("expected exactly the two predecessor crews.\nOutput:\n%s", got)
	}
}

// TestCrewTakeoverMarksPredecessorTasksOwned is the other half of acceptance:
// having listed them, the gardener must see both tasks as OWNED, so there is no
// free slot and it dispatches nothing new.
func TestCrewTakeoverMarksPredecessorTasksOwned(t *testing.T) {
	now := time.Now()
	srv := newTakeoverDaemon(t, now)
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("KD_AGENT_ID", takeoverNewGardenerBead)
	oldActor := actor
	actor = takeoverNewGardenerAgent
	t.Cleanup(func() { actor = oldActor })

	cmd, buf := newCrewCmd(t, "")
	if err := runCrew(cmd, nil); err != nil {
		t.Fatalf("runCrew: %v", err)
	}
	if !strings.Contains(buf.String(), "[OWNED]") {
		t.Errorf("live predecessor crews were not marked OWNED.\nOutput:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "Do NOT reclaim") {
		t.Errorf("missing the never-reclaim banner.\nOutput:\n%s", buf.String())
	}

	// And the same verdict as data, which is what a gardener gates dispatch on.
	for task, progress := range map[string]time.Time{
		takeoverCrewATask: now.Add(-2 * time.Minute),
		takeoverCrewBTask: now.Add(-3 * time.Minute),
	} {
		if !mustOwn(t, "working", progress, now) {
			t.Errorf("task %s is in_progress with a live working agent but is not OWNED — it would be reclaimed", task)
		}
	}
}

func mustOwn(t *testing.T, state string, progress, now time.Time) bool {
	t.Helper()
	_, owned := crewOwnership(state, progress.UTC().Format(time.RFC3339), "", now)
	return owned
}

// TestCrewExplicitTreeFlag pins the operator-facing escape hatch: --tree <epic>
// gives the same answer without any agent identity at all.
func TestCrewExplicitTreeFlag(t *testing.T) {
	srv := newTakeoverDaemon(t, time.Now())
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("KD_AGENT_ID", "")
	oldActor := actor
	actor = "some-operator"
	t.Cleanup(func() { actor = oldActor })

	cmd, buf := newCrewCmd(t, takeoverEpic)
	if err := runCrew(cmd, nil); err != nil {
		t.Fatalf("runCrew: %v", err)
	}
	for _, want := range []string{"taint-propagation-on-9py", "kbeads-data-race-x7q"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("--tree %s did not list %q.\nOutput:\n%s", takeoverEpic, want, buf.String())
		}
	}
}

// TestCrewNoTreeScopeStillLineageScoped guards the unchanged path: a caller
// with no agent identity and no --tree keeps the historical lineage behaviour,
// and is told why the list may be short.
func TestCrewNoTreeScopeStillLineageScoped(t *testing.T) {
	srv := newTakeoverDaemon(t, time.Now())
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("KD_AGENT_ID", "")
	oldActor := actor
	actor = "nobody-spawned-anything"
	t.Cleanup(func() { actor = oldActor })

	cmd, buf := newCrewCmd(t, "")
	if err := runCrew(cmd, nil); err != nil {
		t.Fatalf("runCrew: %v", err)
	}
	if !strings.Contains(buf.String(), "No active crew spawned by") {
		t.Errorf("expected the lineage-scoped empty message.\nOutput:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "--tree") {
		t.Errorf("empty lineage listing must point at --tree.\nOutput:\n%s", buf.String())
	}
}

func TestCrewOwnership(t *testing.T) {
	now := time.Date(2026, 9, 21, 4, 3, 0, 0, time.UTC)
	rfc := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }

	tests := []struct {
		name       string
		state      string
		lastProg   string
		createdAt  string
		wantOwned  bool
		wantNoBeat bool
	}{
		{name: "working, fresh progress", state: "working", lastProg: rfc(2 * time.Minute), wantOwned: true},
		{name: "working, at the window edge", state: "working", lastProg: rfc(15 * time.Minute), wantOwned: true},
		{name: "working, progress just past the window", state: "working", lastProg: rfc(16 * time.Minute)},
		{name: "done is never owned", state: "done", lastProg: rfc(time.Minute)},
		{name: "failed is never owned", state: "failed", lastProg: rfc(time.Minute)},
		{name: "case-insensitive terminal state", state: "Done", lastProg: rfc(time.Minute)},
		{name: "booting agent falls back to created_at", state: "spawning", createdAt: rfc(time.Minute), wantOwned: true},
		{name: "old agent with no progress stamp", state: "working", createdAt: rfc(3 * time.Hour)},
		{name: "clock skew ahead is still fresh", state: "working", lastProg: now.Add(5 * time.Minute).Format(time.RFC3339), wantOwned: true},
		{name: "no timestamps at all", state: "working", wantNoBeat: true},
		{name: "unparseable timestamp", state: "working", lastProg: "not-a-time", wantNoBeat: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hb, owned := crewOwnership(tt.state, tt.lastProg, tt.createdAt, now)
			if owned != tt.wantOwned {
				t.Errorf("crewOwnership(%q, %q, %q) owned = %v, want %v", tt.state, tt.lastProg, tt.createdAt, owned, tt.wantOwned)
			}
			if tt.wantNoBeat && !hb.IsZero() {
				t.Errorf("expected no heartbeat, got %v", hb)
			}
		})
	}
}
