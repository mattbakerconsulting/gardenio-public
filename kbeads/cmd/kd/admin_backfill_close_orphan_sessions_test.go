package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

func sessionBead(id string, createdAt time.Time, fields map[string]any) *model.Bead {
	raw, _ := json.Marshal(fields)
	return &model.Bead{
		ID:        id,
		Type:      "session",
		Kind:      model.KindData,
		Status:    model.StatusOpen,
		Fields:    raw,
		CreatedAt: createdAt,
	}
}

func TestAdminBackfillCloseOrphanSessions_ClosesByEndedAt(t *testing.T) {
	mc := newMockClient()
	now := time.Now()

	// ended_at set → orphan, must close.
	mc.ListBeadsResult = []*model.Bead{
		sessionBead("gd-sess-ended", now, map[string]any{
			"status":   "active",
			"ended_at": "2026-05-13T12:00:00Z",
		}),
		sessionBead("gd-sess-active", now, map[string]any{
			"status": "active",
		}),
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()
	defer func() {
		_ = adminBackfillCloseOrphanSessionsCmd.Flags().Set("dry-run", "false")
		_ = adminBackfillCloseOrphanSessionsCmd.Flags().Set("min-age", "24h")
	}()

	captureStdout(t, func() {
		if err := adminBackfillCloseOrphanSessionsCmd.RunE(adminBackfillCloseOrphanSessionsCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if got := len(mc.CloseBeadCalls); got != 1 {
		t.Fatalf("expected 1 CloseBead call, got %d", got)
	}
	if mc.CloseBeadCalls[0].ID != "gd-sess-ended" {
		t.Errorf("expected to close gd-sess-ended, got %s", mc.CloseBeadCalls[0].ID)
	}
}

func TestAdminBackfillCloseOrphanSessions_ClosesByCompletedStatus(t *testing.T) {
	mc := newMockClient()
	now := time.Now()

	mc.ListBeadsResult = []*model.Bead{
		sessionBead("gd-sess-completed", now, map[string]any{"status": "completed"}),
		sessionBead("gd-sess-crashed", now, map[string]any{"status": "crashed"}),
		sessionBead("gd-sess-retired", now, map[string]any{"status": "retired"}),
		sessionBead("gd-sess-active", now, map[string]any{"status": "active"}),
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()
	defer func() { _ = adminBackfillCloseOrphanSessionsCmd.Flags().Set("dry-run", "false") }()

	captureStdout(t, func() {
		if err := adminBackfillCloseOrphanSessionsCmd.RunE(adminBackfillCloseOrphanSessionsCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	closedIDs := make([]string, 0, len(mc.CloseBeadCalls))
	for _, c := range mc.CloseBeadCalls {
		closedIDs = append(closedIDs, c.ID)
	}
	sort.Strings(closedIDs)

	want := []string{"gd-sess-completed", "gd-sess-crashed", "gd-sess-retired"}
	if len(closedIDs) != len(want) {
		t.Fatalf("expected to close %v, got %v", want, closedIDs)
	}
	for i, id := range want {
		if closedIDs[i] != id {
			t.Errorf("closed[%d] = %s, want %s", i, closedIDs[i], id)
		}
	}
}

func TestAdminBackfillCloseOrphanSessions_ClosesOldActiveByMinAge(t *testing.T) {
	mc := newMockClient()
	now := time.Now()

	mc.ListBeadsResult = []*model.Bead{
		// Old active session — agent crashed, will never reconcile.
		sessionBead("gd-sess-stale", now.Add(-48*time.Hour), map[string]any{"status": "active"}),
		// Recent active session — leave alone.
		sessionBead("gd-sess-fresh", now.Add(-1*time.Hour), map[string]any{"status": "active"}),
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()
	defer func() { _ = adminBackfillCloseOrphanSessionsCmd.Flags().Set("min-age", "24h") }()
	_ = adminBackfillCloseOrphanSessionsCmd.Flags().Set("min-age", "24h")

	captureStdout(t, func() {
		if err := adminBackfillCloseOrphanSessionsCmd.RunE(adminBackfillCloseOrphanSessionsCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if got := len(mc.CloseBeadCalls); got != 1 {
		t.Fatalf("expected 1 CloseBead call, got %d", got)
	}
	if mc.CloseBeadCalls[0].ID != "gd-sess-stale" {
		t.Errorf("expected to close gd-sess-stale, got %s", mc.CloseBeadCalls[0].ID)
	}
}

func TestAdminBackfillCloseOrphanSessions_DryRun(t *testing.T) {
	mc := newMockClient()
	now := time.Now()

	mc.ListBeadsResult = []*model.Bead{
		sessionBead("gd-sess-completed", now, map[string]any{"status": "completed"}),
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()
	defer func() { _ = adminBackfillCloseOrphanSessionsCmd.Flags().Set("dry-run", "false") }()
	_ = adminBackfillCloseOrphanSessionsCmd.Flags().Set("dry-run", "true")

	captureStdout(t, func() {
		if err := adminBackfillCloseOrphanSessionsCmd.RunE(adminBackfillCloseOrphanSessionsCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if got := len(mc.CloseBeadCalls); got != 0 {
		t.Errorf("dry-run made %d CloseBead calls, want 0", got)
	}
}

func TestSessionOrphaned(t *testing.T) {
	now := time.Now()
	cutoff := now.Add(-24 * time.Hour)
	noAgents := sessionAgents{beads: map[string]*model.Bead{}, missing: map[string]bool{}}

	tests := []struct {
		name string
		b    *model.Bead
		want bool
	}{
		{
			name: "ended_at set",
			b:    sessionBead("a", now, map[string]any{"status": "active", "ended_at": "2026-01-01T00:00:00Z"}),
			want: true,
		},
		{
			name: "status completed",
			b:    sessionBead("b", now, map[string]any{"status": "completed"}),
			want: true,
		},
		{
			name: "status crashed",
			b:    sessionBead("c", now, map[string]any{"status": "crashed"}),
			want: true,
		},
		{
			name: "old active",
			b:    sessionBead("d", now.Add(-48*time.Hour), map[string]any{"status": "active"}),
			want: true,
		},
		{
			name: "recent active",
			b:    sessionBead("e", now.Add(-1*time.Hour), map[string]any{"status": "active"}),
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reason, _ := sessionOrphanReason(tc.b, cutoff, noAgents, now, 10*time.Minute)
			if got := reason != ""; got != tc.want {
				t.Errorf("sessionOrphanReason = %q, want selected=%v", reason, tc.want)
			}
		})
	}
}

func agentBead(id string, status model.Status, closedAt *time.Time) *model.Bead {
	return &model.Bead{ID: id, Type: "agent", Status: status, ClosedAt: closedAt}
}

// gd-c6muILNedN: an active session is judged by its agent bead. A live agent
// keeps its session open however old it is; an agent closed past the grace,
// or gone, gets its session closed and stamped ended/retired.
func TestAdminBackfillCloseOrphanSessions_ByAgentBead(t *testing.T) {
	mc := newMockClient()
	now := time.Now()
	longAgo := now.Add(-72 * time.Hour).UTC().Truncate(time.Second)
	justNow := now.Add(-time.Minute)

	mc.Beads["gd-agent-live"] = agentBead("gd-agent-live", model.StatusOpen, nil)
	mc.Beads["gd-agent-closed"] = agentBead("gd-agent-closed", model.StatusClosed, &longAgo)
	mc.Beads["gd-agent-closing"] = agentBead("gd-agent-closing", model.StatusClosed, &justNow)

	mc.ListBeadsResult = []*model.Bead{
		sessionBead("gd-sess-live-old", now.Add(-96*time.Hour), map[string]any{"status": "active", "agent_bead_id": "gd-agent-live"}),
		sessionBead("gd-sess-agent-closed", now.Add(-1*time.Hour), map[string]any{"status": "active", "agent_bead_id": "gd-agent-closed"}),
		sessionBead("gd-sess-agent-gone", now.Add(-1*time.Hour), map[string]any{"status": "active", "agent_bead_id": "gd-agent-gone"}),
		sessionBead("gd-sess-agent-closing", now.Add(-1*time.Hour), map[string]any{"status": "active", "agent_bead_id": "gd-agent-closing"}),
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	captureStdout(t, func() {
		if err := adminBackfillCloseOrphanSessionsCmd.RunE(adminBackfillCloseOrphanSessionsCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	closedIDs := make([]string, 0, len(mc.CloseBeadCalls))
	for _, c := range mc.CloseBeadCalls {
		closedIDs = append(closedIDs, c.ID)
	}
	sort.Strings(closedIDs)
	want := []string{"gd-sess-agent-closed", "gd-sess-agent-gone"}
	if len(closedIDs) != len(want) || closedIDs[0] != want[0] || closedIDs[1] != want[1] {
		t.Fatalf("closed %v, want %v", closedIDs, want)
	}

	stamps := map[string]map[string]string{}
	for _, u := range mc.UpdateBeadCalls {
		var f map[string]string
		if err := json.Unmarshal(u.Req.Fields, &f); err != nil {
			t.Fatalf("stamp for %s: %v", u.ID, err)
		}
		stamps[u.ID] = f
	}
	if got := stamps["gd-sess-agent-closed"]; got["status"] != "retired" || got["ended_at"] != longAgo.Format(time.RFC3339) {
		t.Errorf("agent-closed stamp = %v, want status=retired ended_at=%s (the agent's closed_at)", got, longAgo.Format(time.RFC3339))
	}
	if got := stamps["gd-sess-agent-gone"]; got["status"] != "retired" || got["ended_at"] == "" {
		t.Errorf("agent-gone stamp = %v, want status=retired and an ended_at", got)
	}
}

// A session gb already recorded keeps its exit status and ended_at.
func TestSessionCloseStamp_KeepsRecordedExit(t *testing.T) {
	b := sessionBead("s", time.Now(), map[string]any{"status": "completed", "ended_at": "2026-10-01T00:00:00Z"})
	if got := sessionCloseStamp(b, time.Now()); got != nil {
		t.Errorf("stamp = %s, want nil", got)
	}
}

// A token that may not close the sessions (403 on each) must stop early, and
// --limit bounds attempts rather than successes.
func TestAdminBackfillCloseOrphanSessions_StopsWhenEveryCloseFails(t *testing.T) {
	mc := newMockClient()
	now := time.Now()
	for i := 0; i < 20; i++ {
		mc.ListBeadsResult = append(mc.ListBeadsResult,
			sessionBead(fmt.Sprintf("gd-sess-%d", i), now, map[string]any{"status": "completed"}))
	}
	mc.CloseErr = errors.New("HTTP 403: ownership denied")

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	var err error
	captureStdout(t, func() {
		err = adminBackfillCloseOrphanSessionsCmd.RunE(adminBackfillCloseOrphanSessionsCmd, nil)
	})
	if err == nil {
		t.Fatal("RunE succeeded with every close refused, want error")
	}
	if got := len(mc.CloseBeadCalls); got != maxLeadingCloseFailures {
		t.Errorf("attempted %d closes, want to stop after %d", got, maxLeadingCloseFailures)
	}
}
