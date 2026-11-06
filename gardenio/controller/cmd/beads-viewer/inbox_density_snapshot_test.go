package main

import (
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TestInbox_DensitySnapshot dumps a /inbox HTML render with dense fixture rows
// (long actor names, long project names, agent rows with state+pod+closed chips,
// epic rows with status, decision rows). Run with GB_INBOX_SNAPSHOT_OUT=/tmp/foo.html
// to capture for before/after screenshot comparison; otherwise the test is a
// no-op smoke test that just exercises the render path.
func TestInbox_DensitySnapshot(t *testing.T) {
	out := os.Getenv("GB_INBOX_SNAPSHOT_OUT")
	if out == "" {
		t.Skip("set GB_INBOX_SNAPSHOT_OUT to capture an inbox HTML snapshot")
	}

	humanID := int64(100)
	now := time.Now()

	mk := func(id, title string, typ, status string, fields map[string]string, labels []string, ago time.Duration) *beadsapi.BeadDetail {
		b := &beadsapi.BeadDetail{
			ID:           id,
			Title:        title,
			Type:         typ,
			Status:       status,
			OperatedByID: &humanID,
			UpdatedAt:    now.Add(-ago),
			CreatedAt:    now.Add(-ago - time.Hour),
			Fields:       fields,
			Labels:       labels,
		}
		if typ == "task" {
			b.Category = "intake"
		}
		return b
	}

	beads := map[string]*beadsapi.BeadDetail{
		"gd-intake01": mk("gd-intake01",
			"plain intake row — short title", "task", "open",
			nil, []string{"project:gardenio"}, 5*time.Minute),
		"gd-intake02": mk("gd-intake02",
			"intake with a fairly long title that may push chips around on narrow viewports indeed",
			"task", "open", nil, []string{"project:monorepo-very-long-project-name"}, 12*time.Minute),
		"gd-decis01": mk("gd-decis01",
			"decision: pick auth strategy for new service",
			"decision", "open", nil, []string{"project:gardenio"}, 30*time.Minute),
		"gd-epic01": mk("gd-epic01",
			"Beads-Viewer Inbox: density & efficiency umbrella epic with a long title",
			"epic", "open", nil, []string{"project:gardenio"}, 1*time.Hour),
		"gd-agent01": mk("gd-agent01",
			"gardenio-beadsviewer-inbox-collapse-r19 working on chip overflow", "agent", "in_progress",
			map[string]string{"agent_state": "alive", "pod_phase": "Running", "agent": "gardenio-beadsviewer-inbox-collapse-r19"},
			[]string{"project:gardenio", "role:crew"}, 3*time.Minute),
		"gd-agent02": mk("gd-agent02",
			"sched-gardenio-gardener-delivery-est4 driving READY beads through code → MR → deploy → close",
			"agent", "in_progress",
			map[string]string{"agent_state": "stuck", "pod_phase": "Running", "agent": "sched-gardenio-gardener-delivery-est4"},
			[]string{"project:gardenio", "role:gardener"}, 15*time.Minute),
		"gd-agent03": mk("gd-agent03",
			"closed agent row",
			"agent", "closed",
			map[string]string{"agent_state": "closed", "agent": "old-agent-name-here"},
			[]string{"project:monorepo"}, 2*time.Hour),
	}

	daemon := &inboxFilteringDaemon{fakeDaemon: &fakeDaemon{
		beads: beads,
		deps:  map[string][]beadsapi.Dependency{},
	}}
	lookup := &inboxFakeLookup{rows: map[string]struct {
		id    int64
		kind  string
		found bool
	}{
		"alice@example.com": {id: humanID, kind: "human", found: true},
	}}

	mux := mkInboxMux(t, daemon, lookup)
	req := inboxRequest("GET", "/inbox", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}

	if err := os.WriteFile(out, w.Body.Bytes(), 0o644); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	t.Logf("wrote inbox snapshot to %s (%d bytes)", out, w.Body.Len())
}

// TestInbox_QuietSnapshot dumps a /inbox HTML render where only Decisions
// are populated — exercise the quiet-inbox path so we can capture
// before/after screenshots for the "+N empty:" collapse (ka-0000001480).
// Run with GB_INBOX_SNAPSHOT_OUT=/tmp/quiet.html.
func TestInbox_QuietSnapshot(t *testing.T) {
	out := os.Getenv("GB_INBOX_SNAPSHOT_OUT")
	if out == "" {
		t.Skip("set GB_INBOX_SNAPSHOT_OUT to capture a quiet inbox snapshot")
	}

	humanID := int64(100)
	now := time.Now()

	beads := map[string]*beadsapi.BeadDetail{
		"gd-decis01": {
			ID: "gd-decis01", Title: "decision: pick auth strategy for new service",
			Type: "decision", Status: "open",
			OperatedByID: &humanID, UpdatedAt: now.Add(-30 * time.Minute),
			CreatedAt: now.Add(-1 * time.Hour),
			Labels:    []string{"project:gardenio"},
		},
	}

	daemon := &inboxFilteringDaemon{fakeDaemon: &fakeDaemon{
		beads: beads,
		deps:  map[string][]beadsapi.Dependency{},
	}}
	lookup := &inboxFakeLookup{rows: map[string]struct {
		id    int64
		kind  string
		found bool
	}{
		"alice@example.com": {id: humanID, kind: "human", found: true},
	}}

	mux := mkInboxMux(t, daemon, lookup)
	req := inboxRequest("GET", "/inbox", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}

	if err := os.WriteFile(out, w.Body.Bytes(), 0o644); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	t.Logf("wrote quiet inbox snapshot to %s (%d bytes)", out, w.Body.Len())
}
