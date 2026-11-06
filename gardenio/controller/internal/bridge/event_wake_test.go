package bridge

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// fakeEventWakeClient records the UpdateBead PATCH + AddComment writes the
// event-wake primitive issues, so tests assert exactly what it wrote.
type fakeEventWakeClient struct {
	updates     map[string]beadsapi.UpdateBeadRequest
	comments    map[string]string
	updateErr   error
	commentErr  error
	updateCalls int
}

func newFakeEventWakeClient() *fakeEventWakeClient {
	return &fakeEventWakeClient{
		updates:  map[string]beadsapi.UpdateBeadRequest{},
		comments: map[string]string{},
	}
}

func (f *fakeEventWakeClient) UpdateBead(_ context.Context, beadID string, req beadsapi.UpdateBeadRequest) error {
	f.updateCalls++
	if f.updateErr != nil {
		return f.updateErr
	}
	f.updates[beadID] = req
	return nil
}

func (f *fakeEventWakeClient) AddComment(_ context.Context, beadID, _, text string) error {
	if f.commentErr != nil {
		return f.commentErr
	}
	f.comments[beadID] = text
	return nil
}

func eventWakeTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func dormantBead(id string, extra map[string]string) *beadsapi.BeadDetail {
	fields := map[string]string{
		"agent":                     "persist-agent",
		"project":                   "gardenio",
		beadsapi.FieldSuccessor:     beadsapi.SuccessorSame,
		beadsapi.FieldAgentState:    beadsapi.AgentStateDormant,
		beadsapi.FieldNextRespawnAt: "2099-01-01T00:00:00Z",
		beadsapi.FieldRespawnCount:  "2",
	}
	for k, v := range extra {
		fields[k] = v
	}
	return &beadsapi.BeadDetail{ID: id, Type: "agent", Status: "closed", Fields: fields}
}

func TestEventWake_RevivesDormantIdentity(t *testing.T) {
	f := newFakeEventWakeClient()
	bead := dormantBead("bd-dormant", nil)

	woke, err := eventWakeDormantAgent(context.Background(), f, eventWakeTestLogger(), bead, beadsapi.WakeReasonDecision, "slack:U123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !woke {
		t.Fatal("expected woke=true for a dormant parked identity")
	}

	req, ok := f.updates["bd-dormant"]
	if !ok {
		t.Fatal("expected an UpdateBead PATCH")
	}
	if req.Status == nil || *req.Status != "open" {
		t.Errorf("status: got %v, want open", req.Status)
	}
	// wake_reason stamped.
	if got := req.Fields[beadsapi.FieldWakeReason]; got != beadsapi.WakeReasonDecision {
		t.Errorf("wake_reason: got %v, want %q", got, beadsapi.WakeReasonDecision)
	}
	// next_respawn_at + agent_state cleared (explicit nil = delete).
	if v, present := req.Fields[beadsapi.FieldNextRespawnAt]; !present || v != nil {
		t.Errorf("next_respawn_at: got %v (present=%v), want explicit nil", v, present)
	}
	if v, present := req.Fields[beadsapi.FieldAgentState]; !present || v != nil {
		t.Errorf("agent_state: got %v (present=%v), want explicit nil", v, present)
	}
	// respawn_count MUST NOT be touched — events reset TTL, not budget.
	if _, present := req.Fields[beadsapi.FieldRespawnCount]; present {
		t.Errorf("respawn_count must not be in the patch (events reset TTL, not budget); got %v", req.Fields[beadsapi.FieldRespawnCount])
	}
	if f.comments["bd-dormant"] == "" {
		t.Error("expected an audit comment on the revived bead")
	}
}

func TestEventWake_IgnoresNonDormant(t *testing.T) {
	logger := eventWakeTestLogger()
	cases := []struct {
		name string
		bead *beadsapi.BeadDetail
	}{
		{"nil", nil},
		{"open-agent", &beadsapi.BeadDetail{ID: "b1", Type: "agent", Status: "in_progress", Fields: map[string]string{beadsapi.FieldSuccessor: beadsapi.SuccessorSame}}},
		{"non-agent", &beadsapi.BeadDetail{ID: "b2", Type: "task", Status: "closed", Fields: map[string]string{beadsapi.FieldSuccessor: beadsapi.SuccessorSame}}},
		{"closed-no-wake-intent", &beadsapi.BeadDetail{ID: "b3", Type: "agent", Status: "closed", Fields: map[string]string{beadsapi.FieldSuccessor: "none"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeEventWakeClient()
			woke, err := eventWakeDormantAgent(context.Background(), f, logger, tc.bead, beadsapi.WakeReasonMention, "gardenio")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if woke {
				t.Error("expected woke=false for a non-dormant bead")
			}
			if f.updateCalls != 0 {
				t.Errorf("expected no UpdateBead call, got %d", f.updateCalls)
			}
		})
	}
}

func TestEventWake_RefusesGoalUnachievable(t *testing.T) {
	f := newFakeEventWakeClient()
	bead := dormantBead("bd-unachievable", map[string]string{beadsapi.FieldGoalUnachievable: "true"})

	woke, err := eventWakeDormantAgent(context.Background(), f, eventWakeTestLogger(), bead, beadsapi.WakeReasonAssignment, "gardenio")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if woke {
		t.Error("expected woke=false for a goal-unachievable identity")
	}
	if f.updateCalls != 0 {
		t.Errorf("expected no revive for a goal-unachievable identity, got %d UpdateBead calls", f.updateCalls)
	}
}

func TestEventWake_PropagatesUpdateError(t *testing.T) {
	f := newFakeEventWakeClient()
	f.updateErr = errors.New("boom")
	bead := dormantBead("bd-err", nil)

	woke, err := eventWakeDormantAgent(context.Background(), f, eventWakeTestLogger(), bead, beadsapi.WakeReasonDecision, "gardenio")
	if err == nil {
		t.Fatal("expected an error when UpdateBead fails")
	}
	if woke {
		t.Error("expected woke=false on UpdateBead failure")
	}
}

func TestEventWake_CommentFailureStillWakes(t *testing.T) {
	f := newFakeEventWakeClient()
	f.commentErr = errors.New("comment down")
	bead := dormantBead("bd-nocomment", nil)

	woke, err := eventWakeDormantAgent(context.Background(), f, eventWakeTestLogger(), bead, beadsapi.WakeReasonDecision, "gardenio")
	if err != nil {
		t.Fatalf("a comment failure must not fail the wake: %v", err)
	}
	if !woke {
		t.Fatal("expected woke=true even when the audit comment fails")
	}
	if _, ok := f.updates["bd-nocomment"]; !ok {
		t.Error("expected the reopen PATCH to have landed")
	}
}
