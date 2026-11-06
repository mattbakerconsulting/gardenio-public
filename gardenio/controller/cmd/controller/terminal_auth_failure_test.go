package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func testTerminalAuthLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// fakeDiagnosticWriter is an in-memory beadDiagnosticWriter for tests. It
// records every UpdateBeadFieldsTyped call and can be seeded with beads for
// GetBead / FindAgentBead resolution.
type fakeDiagnosticWriter struct {
	mu sync.Mutex

	byID    map[string]*beadsapi.BeadDetail
	byAgent map[string]*beadsapi.BeadDetail
	findErr error

	updates []fakeUpdate
}

type fakeUpdate struct {
	beadID string
	fields map[string]any
}

func newFakeDiagnosticWriter() *fakeDiagnosticWriter {
	return &fakeDiagnosticWriter{
		byID:    make(map[string]*beadsapi.BeadDetail),
		byAgent: make(map[string]*beadsapi.BeadDetail),
	}
}

func (f *fakeDiagnosticWriter) seed(bead *beadsapi.BeadDetail, agentName string) {
	f.byID[bead.ID] = bead
	if agentName != "" {
		f.byAgent[agentName] = bead
	}
}

func (f *fakeDiagnosticWriter) FindAgentBead(_ context.Context, agentName string) (*beadsapi.BeadDetail, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	if b, ok := f.byAgent[agentName]; ok {
		return b, nil
	}
	return nil, beadsapi.ErrAgentNotFound
}

func (f *fakeDiagnosticWriter) GetBead(_ context.Context, beadID string) (*beadsapi.BeadDetail, error) {
	if b, ok := f.byID[beadID]; ok {
		return b, nil
	}
	return nil, beadsapi.ErrAgentNotFound
}

func (f *fakeDiagnosticWriter) UpdateBeadFieldsTyped(_ context.Context, beadID string, fields map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Copy so later mutation of the caller's map can't affect what we recorded.
	cp := make(map[string]any, len(fields))
	for k, v := range fields {
		cp[k] = v
	}
	f.updates = append(f.updates, fakeUpdate{beadID: beadID, fields: cp})
	return nil
}

func (f *fakeDiagnosticWriter) lastUpdate() (fakeUpdate, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.updates) == 0 {
		return fakeUpdate{}, false
	}
	return f.updates[len(f.updates)-1], true
}

func (f *fakeDiagnosticWriter) updateCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.updates)
}

func newTestTerminalAuthListener(daemon beadDiagnosticWriter) *terminalAuthFailureListener {
	return &terminalAuthFailureListener{
		daemon:   daemon,
		logger:   testTerminalAuthLogger(),
		debounce: 30 * time.Second,
		lastSeen: make(map[string]time.Time),
	}
}

func TestTerminalAuthFailureListener_NilWhenNoNats(t *testing.T) {
	l := newTerminalAuthFailureListener(terminalAuthFailureConfig{
		NatsURL: "",
		Daemon:  newFakeDiagnosticWriter(),
		Logger:  testTerminalAuthLogger(),
	})
	if l != nil {
		t.Fatal("expected nil listener when NATS URL is empty")
	}
}

func TestTerminalAuthFailureListener_NilWhenNoDaemon(t *testing.T) {
	l := newTerminalAuthFailureListener(terminalAuthFailureConfig{
		NatsURL: "nats://localhost:4222",
		Daemon:  nil,
		Logger:  testTerminalAuthLogger(),
	})
	if l != nil {
		t.Fatal("expected nil listener when daemon is nil")
	}
}

func TestTerminalAuthFailureEvent_Unmarshal(t *testing.T) {
	raw := `{
		"event": "terminal_auth_failure",
		"error_category": "unauthorized",
		"session_id": "sess-123",
		"detail": "API Error: 401 key not allowed to access model",
		"agent": "worker-1",
		"agent_bead_id": "gd-ABC",
		"project": "gardenio",
		"account": "claude-max-main"
	}`

	var evt terminalAuthFailureEvent
	if err := json.Unmarshal([]byte(raw), &evt); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if evt.ErrorCategory != "unauthorized" {
		t.Errorf("error_category = %q, want %q", evt.ErrorCategory, "unauthorized")
	}
	if evt.AgentBeadID != "gd-ABC" {
		t.Errorf("agent_bead_id = %q, want %q", evt.AgentBeadID, "gd-ABC")
	}
	if !strings.Contains(evt.Detail, "key not allowed to access model") {
		t.Errorf("detail = %q, want it to contain the 401 string", evt.Detail)
	}
}

// The core regression: a terminal auth failure MUST land error_category +
// error_detail on the agent bead and bump total_errors — the exact data that
// was missing during the 2026-07-25 opus-5 outage (ka-0000003962).
func TestTerminalAuthFailure_PersistsDiagnosticsByBeadID(t *testing.T) {
	daemon := newFakeDiagnosticWriter()
	daemon.seed(&beadsapi.BeadDetail{
		ID:     "gd-AGENT1",
		Fields: map[string]string{"total_errors": "2"},
	}, "worker-1")
	l := newTestTerminalAuthListener(daemon)

	l.persist(context.Background(), terminalAuthFailureEvent{
		Event:         "terminal_auth_failure",
		ErrorCategory: "unauthorized",
		Detail:        "API Error: 401 key not allowed to access model",
		Agent:         "worker-1",
		AgentBeadID:   "gd-AGENT1",
	}, "coop.events.auth.terminal_failure")

	upd, ok := daemon.lastUpdate()
	if !ok {
		t.Fatal("expected an UpdateBeadFieldsTyped call, got none")
	}
	if upd.beadID != "gd-AGENT1" {
		t.Errorf("bead id = %q, want gd-AGENT1", upd.beadID)
	}
	if upd.fields["error_category"] != "unauthorized" {
		t.Errorf("error_category = %v, want unauthorized", upd.fields["error_category"])
	}
	if got := upd.fields["error_detail"]; got != "API Error: 401 key not allowed to access model" {
		t.Errorf("error_detail = %v, want the 401 string", got)
	}
	// total_errors was 2, must be bumped to 3.
	if upd.fields["total_errors"] != 3 {
		t.Errorf("total_errors = %v, want 3", upd.fields["total_errors"])
	}
}

// When the event omits agent_bead_id, the listener resolves the bead by agent
// name (legacy pods that never set BOAT_AGENT_BEAD_ID).
func TestTerminalAuthFailure_ResolvesByAgentName(t *testing.T) {
	daemon := newFakeDiagnosticWriter()
	daemon.seed(&beadsapi.BeadDetail{
		ID:     "gd-BYNAME",
		Fields: map[string]string{},
	}, "worker-9")
	l := newTestTerminalAuthListener(daemon)

	l.persist(context.Background(), terminalAuthFailureEvent{
		ErrorCategory: "out_of_credits",
		Detail:        "insufficient credits",
		Agent:         "worker-9",
	}, "coop.events.auth.terminal_failure")

	upd, ok := daemon.lastUpdate()
	if !ok {
		t.Fatal("expected an update via by-name resolution, got none")
	}
	if upd.beadID != "gd-BYNAME" {
		t.Errorf("bead id = %q, want gd-BYNAME", upd.beadID)
	}
	// No prior total_errors seeded → seeded from zero, bumped to 1.
	if upd.fields["total_errors"] != 1 {
		t.Errorf("total_errors = %v, want 1", upd.fields["total_errors"])
	}
}

// A second identical event within the debounce window must not write twice.
func TestTerminalAuthFailure_DebouncesRepeatEvents(t *testing.T) {
	daemon := newFakeDiagnosticWriter()
	daemon.seed(&beadsapi.BeadDetail{ID: "gd-DEB", Fields: map[string]string{}}, "w")
	l := newTestTerminalAuthListener(daemon)

	evt := terminalAuthFailureEvent{
		ErrorCategory: "unauthorized",
		Detail:        "401 key not allowed to access model",
		Agent:         "w",
		AgentBeadID:   "gd-DEB",
	}
	l.persist(context.Background(), evt, "s")
	l.persist(context.Background(), evt, "s")

	if n := daemon.updateCount(); n != 1 {
		t.Fatalf("expected exactly 1 update within debounce window, got %d", n)
	}
}

// An event with no category and no detail carries no signal and must be
// ignored — no bead write.
func TestTerminalAuthFailure_IgnoresEmptyEvent(t *testing.T) {
	daemon := newFakeDiagnosticWriter()
	l := newTestTerminalAuthListener(daemon)
	l.persist(context.Background(), terminalAuthFailureEvent{Agent: "w", AgentBeadID: "gd-X"}, "s")
	if n := daemon.updateCount(); n != 0 {
		t.Fatalf("expected no update for an empty event, got %d", n)
	}
}

// A pathologically long detail is truncated before it is persisted so it can't
// bloat the bead.
func TestTruncateDetail(t *testing.T) {
	short := "401 key not allowed to access model"
	if got := truncateDetail(short); got != short {
		t.Errorf("short detail should pass through unchanged, got %q", got)
	}
	long := strings.Repeat("x", terminalAuthDetailMaxLen+500)
	got := truncateDetail(long)
	if len(got) <= terminalAuthDetailMaxLen {
		t.Errorf("truncated detail length = %d, want > cap (has suffix marker)", len(got))
	}
	if !strings.HasSuffix(got, "…(truncated)") {
		t.Errorf("truncated detail should carry the truncation marker, got tail %q", got[len(got)-20:])
	}
	if !strings.HasPrefix(got, strings.Repeat("x", terminalAuthDetailMaxLen)) {
		t.Error("truncated detail should keep the leading cap worth of characters")
	}
}
