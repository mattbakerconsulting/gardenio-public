package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"log/slog"

	"gardenio/controller/internal/beadsapi"
)

func TestDecisions_HandleCreated(t *testing.T) {
	d := &Decisions{
		notifier:  &mockNotifier{},
		logger:    slog.Default(),
		escalated: make(map[string]time.Time),
	}

	// Non-decision bead should be ignored.
	nonDecision := marshalSSEBeadPayload(BeadEvent{
		ID:   "abc",
		Type: "agent",
	})
	d.handleCreated(context.Background(), nonDecision)

	mn := d.notifier.(*mockNotifier)
	if len(mn.getCreated()) != 0 {
		t.Fatal("non-decision bead should not trigger notification")
	}

	// Decision bead should trigger notification.
	decision := marshalSSEBeadPayload(BeadEvent{
		ID:       "dec-1",
		Type:     "decision",
		Title:    "Pick a color",
		Assignee: "crew-town-crew-hq",
		Fields: map[string]string{
			"question": "What color?",
			"options":  `["red","blue"]`,
		},
	})
	d.handleCreated(context.Background(), decision)

	created := mn.getCreated()
	if len(created) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(created))
	}
	if created[0].ID != "dec-1" {
		t.Errorf("expected bead ID dec-1, got %s", created[0].ID)
	}
	if created[0].Assignee != "crew-town-crew-hq" {
		t.Errorf("expected assignee crew-town-crew-hq, got %s", created[0].Assignee)
	}
}

func TestDecisions_HandleClosed_NudgesCoop(t *testing.T) {
	// Set up a fake coop server that records nudge calls.
	var nudgeReceived sync.Mutex
	var nudgeMessage string
	coopServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agent/nudge" {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			nudgeReceived.Lock()
			nudgeMessage = body["message"]
			nudgeReceived.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer coopServer.Close()

	// Set up a mock daemon that returns the agent bead with coop_url in Notes.
	daemon := newMockDaemon()
	daemon.beads["crew-town-crew-hq"] = &beadsapi.BeadDetail{
		ID:    "crew-town-crew-hq",
		Notes: "coop_url: " + coopServer.URL,
	}
	notif := &mockNotifier{}

	d := &Decisions{
		daemon:    daemon,
		notifier:  notif,
		logger:    slog.Default(),
		escalated: make(map[string]time.Time),
	}

	closedEvent := marshalSSEBeadPayload(BeadEvent{
		ID:       "dec-1",
		Type:     "decision",
		Assignee: "crew-town-crew-hq",
		Fields: map[string]string{
			"prompt":    "What color?",
			"chosen":    "blue",
			"rationale": "it's calming",
		},
	})
	d.handleClosed(context.Background(), closedEvent)

	// Verify nudge was sent to coop.
	time.Sleep(50 * time.Millisecond) // give async processing a moment
	nudgeReceived.Lock()
	msg := nudgeMessage
	nudgeReceived.Unlock()

	if msg == "" {
		t.Fatal("expected coop nudge, got none")
	}
	want := "Decision dec-1 resolved: What color?\nChosen: blue\nRationale: it's calming"
	if msg != want {
		t.Errorf("unexpected nudge message:\n got: %s\nwant: %s", msg, want)
	}

	// Verify notifier.UpdateDecision was called.
	updated := notif.getUpdated()
	if len(updated) != 1 {
		t.Fatalf("expected 1 update call, got %d", len(updated))
	}
	if updated[0].BeadID != "dec-1" || updated[0].Chosen != "blue" || updated[0].Rationale != "it's calming" {
		t.Errorf("unexpected update call: %+v", updated[0])
	}
}

// TestDecisions_HandleClosed_RationaleFromFetch verifies that when the SSE close
// event omits rationale (common in practice), it is fetched from the full bead
// and included in the nudge message sent to the LLM.
func TestDecisions_HandleClosed_RationaleFromFetch(t *testing.T) {
	var nudgeReceived sync.Mutex
	var nudgeMessage string
	coopServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agent/nudge" {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			nudgeReceived.Lock()
			nudgeMessage = body["message"]
			nudgeReceived.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer coopServer.Close()

	daemon := newMockDaemon()
	daemon.beads["agent-x"] = &beadsapi.BeadDetail{
		ID:    "agent-x",
		Notes: "coop_url: " + coopServer.URL,
	}
	// Full bead has chosen, rationale, and prompt; SSE event has none.
	daemon.beads["dec-rat"] = &beadsapi.BeadDetail{
		ID: "dec-rat",
		Fields: map[string]string{
			"prompt":    "Should we proceed?",
			"chosen":    "proceed",
			"rationale": "looks good to me",
		},
	}

	d := &Decisions{
		daemon:    daemon,
		notifier:  &mockNotifier{},
		logger:    slog.Default(),
		escalated: make(map[string]time.Time),
	}

	// SSE event carries neither chosen nor rationale — all must come from the fetch.
	closedEvent := marshalSSEBeadPayload(BeadEvent{
		ID:       "dec-rat",
		Type:     "decision",
		Assignee: "agent-x",
		Fields:   map[string]string{},
	})
	d.handleClosed(context.Background(), closedEvent)

	time.Sleep(50 * time.Millisecond)
	nudgeReceived.Lock()
	msg := nudgeMessage
	nudgeReceived.Unlock()

	want := "Decision dec-rat resolved: Should we proceed?\nChosen: proceed\nRationale: looks good to me"
	if msg != want {
		t.Errorf("nudge message = %q, want %q", msg, want)
	}
}

func TestDecisions_HandleClosed_NoAssignee(t *testing.T) {
	d := &Decisions{
		notifier:  &mockNotifier{},
		logger:    slog.Default(),
		escalated: make(map[string]time.Time),
	}

	// Decision closed without assignee — should log warning but not panic.
	closedEvent := marshalSSEBeadPayload(BeadEvent{
		ID:   "dec-2",
		Type: "decision",
		Fields: map[string]string{
			"chosen": "yes",
		},
	})
	d.handleClosed(context.Background(), closedEvent)
	// No panic = pass.
}

func TestDecisions_HandleClosed_Expiry(t *testing.T) {
	notif := &mockNotifier{}
	d := NewDecisions(DecisionsConfig{
		Notifier: notif,
		Logger:   slog.Default(),
	})

	// Decision closed with chosen=_expired should dismiss the Slack message.
	expiredEvent := marshalSSEBeadPayload(BeadEvent{
		ID:   "dec-3",
		Type: "decision",
		Fields: map[string]string{
			"chosen":    "_expired",
			"rationale": "Decision expired after 30m with no response",
		},
	})
	d.handleClosed(context.Background(), expiredEvent)

	dismissed := notif.getDismissed()
	if len(dismissed) != 1 {
		t.Fatalf("expected 1 dismiss call, got %d", len(dismissed))
	}
	if dismissed[0] != "dec-3" {
		t.Errorf("expected dismissed bead dec-3, got %s", dismissed[0])
	}

	// UpdateDecision should NOT be called for expired decisions.
	if len(notif.getUpdated()) != 0 {
		t.Error("UpdateDecision should not be called for expired decisions")
	}
}

func TestDecisions_HandleClosed_Dismissed(t *testing.T) {
	notif := &mockNotifier{}
	d := NewDecisions(DecisionsConfig{
		Notifier: notif,
		Logger:   slog.Default(),
	})

	// Decision closed with chosen=dismissed should dismiss the Slack message.
	dismissedEvent := marshalSSEBeadPayload(BeadEvent{
		ID:   "dec-4",
		Type: "decision",
		Fields: map[string]string{
			"chosen": "dismissed",
		},
	})
	d.handleClosed(context.Background(), dismissedEvent)

	dismissed := notif.getDismissed()
	if len(dismissed) != 1 {
		t.Fatalf("expected 1 dismiss call, got %d", len(dismissed))
	}
}

func TestDecisions_HandleUpdated_Escalation(t *testing.T) {
	notif := &mockNotifier{}
	d := NewDecisions(DecisionsConfig{
		Notifier: notif,
		Logger:   slog.Default(),
	})

	// Non-decision bead should be ignored.
	nonDecision := marshalSSEBeadPayload(BeadEvent{
		ID:     "agent-1",
		Type:   "agent",
		Labels: []string{"escalated"},
	})
	d.handleUpdated(context.Background(), nonDecision)
	if len(notif.getEscalated()) != 0 {
		t.Fatal("non-decision bead should not trigger escalation")
	}

	// Decision without escalation marker should be ignored.
	normalUpdate := marshalSSEBeadPayload(BeadEvent{
		ID:   "dec-5",
		Type: "decision",
		Fields: map[string]string{
			"question": "Deploy to prod?",
		},
	})
	d.handleUpdated(context.Background(), normalUpdate)
	if len(notif.getEscalated()) != 0 {
		t.Fatal("decision without escalation should not trigger notification")
	}

	// Decision with "escalated" label should trigger notification.
	escalatedEvent := marshalSSEBeadPayload(BeadEvent{
		ID:       "dec-6",
		Type:     "decision",
		Labels:   []string{"escalated"},
		Assignee: "gardenio/crew/test-bot",
		Fields: map[string]string{
			"question": "Deploy to prod?",
		},
	})
	d.handleUpdated(context.Background(), escalatedEvent)

	escalated := notif.getEscalated()
	if len(escalated) != 1 {
		t.Fatalf("expected 1 escalation, got %d", len(escalated))
	}
	if escalated[0].ID != "dec-6" {
		t.Errorf("expected bead ID dec-6, got %s", escalated[0].ID)
	}
}

func TestDecisions_HandleUpdated_EscalationByLabel(t *testing.T) {
	notif := &mockNotifier{}
	d := NewDecisions(DecisionsConfig{
		Notifier: notif,
		Logger:   slog.Default(),
	})

	// Decision with "escalated" label should trigger escalation.
	criticalEvent := marshalSSEBeadPayload(BeadEvent{
		ID:     "dec-7",
		Type:   "decision",
		Labels: []string{"escalated", "urgent"},
		Fields: map[string]string{
			"question": "System down — deploy hotfix?",
		},
	})
	d.handleUpdated(context.Background(), criticalEvent)

	escalated := notif.getEscalated()
	if len(escalated) != 1 {
		t.Fatalf("expected 1 escalation, got %d", len(escalated))
	}
}

func TestDecisions_HandleUpdated_EscalationDedup(t *testing.T) {
	notif := &mockNotifier{}
	d := NewDecisions(DecisionsConfig{
		Notifier: notif,
		Logger:   slog.Default(),
	})

	escalatedEvent := marshalSSEBeadPayload(BeadEvent{
		ID:     "dec-8",
		Type:   "decision",
		Labels: []string{"escalated"},
		Fields: map[string]string{
			"question": "Approve?",
		},
	})

	// First call: should notify.
	d.handleUpdated(context.Background(), escalatedEvent)
	// Second call: should be deduplicated.
	d.handleUpdated(context.Background(), escalatedEvent)

	escalated := notif.getEscalated()
	if len(escalated) != 1 {
		t.Fatalf("expected exactly 1 escalation (dedup), got %d", len(escalated))
	}
}

func TestMockDaemon_ListDecisionBeads(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["dec-10"] = &beadsapi.BeadDetail{
		ID:   "dec-10",
		Type: "decision",
		Fields: map[string]string{
			"question": "Deploy?",
		},
	}
	daemon.beads["agent-1"] = &beadsapi.BeadDetail{
		ID:   "agent-1",
		Type: "agent",
	}
	daemon.beads["dec-11"] = &beadsapi.BeadDetail{
		ID:       "dec-11",
		Type:     "decision",
		Assignee: "test-bot",
		Labels:   []string{"escalated"},
		Fields: map[string]string{
			"question": "Rollback?",
		},
	}

	decisions, err := daemon.ListDecisionBeads(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 2 {
		t.Fatalf("expected 2 decisions, got %d", len(decisions))
	}

	// Verify only decision beads returned.
	ids := map[string]bool{}
	for _, d := range decisions {
		ids[d.ID] = true
		if d.Type != "decision" {
			t.Errorf("expected type=decision, got %s", d.Type)
		}
	}
	if !ids["dec-10"] || !ids["dec-11"] {
		t.Errorf("expected dec-10 and dec-11, got %v", ids)
	}
}

func TestMockDaemon_ListAgentBeads(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["agent-a"] = &beadsapi.BeadDetail{
		ID:   "agent-a",
		Type: "agent",
		Fields: map[string]string{
			"role":    "crew",
			"project": "gardenio",
		},
	}
	daemon.beads["dec-1"] = &beadsapi.BeadDetail{
		ID:   "dec-1",
		Type: "decision",
	}

	agents, err := daemon.ListAgentBeads(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	if agents[0].ID != "agent-a" {
		t.Errorf("expected agent-a, got %s", agents[0].ID)
	}
	if agents[0].Project != "gardenio" {
		t.Errorf("expected project=gardenio, got %s", agents[0].Project)
	}
}

func TestDecisionQuestion(t *testing.T) {
	// "prompt" field is preferred over "question".
	if got := decisionQuestion(map[string]string{"prompt": "Deploy?"}); got != "Deploy?" {
		t.Errorf("expected prompt value, got %q", got)
	}
	// Fallback to legacy "question" field.
	if got := decisionQuestion(map[string]string{"question": "Rollback?"}); got != "Rollback?" {
		t.Errorf("expected question value, got %q", got)
	}
	// "prompt" takes precedence when both are set.
	if got := decisionQuestion(map[string]string{"prompt": "A", "question": "B"}); got != "A" {
		t.Errorf("expected prompt to take precedence, got %q", got)
	}
	// Empty fields returns empty string.
	if got := decisionQuestion(map[string]string{}); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestDecisions_HandleReportClosed_PostsReport(t *testing.T) {
	daemon := newMockDaemon()
	notif := &mockNotifier{}
	d := NewDecisions(DecisionsConfig{
		Daemon:   daemon,
		Notifier: notif,
		Logger:   slog.Default(),
	})

	closedEvent := marshalSSEBeadPayload(BeadEvent{
		ID:   "report-1",
		Type: "report",
		Fields: map[string]string{
			"decision_id": "dec-42",
			"report_type": "plan",
			"content":     "Here is the implementation plan.",
		},
	})
	d.handleReportClosed(context.Background(), closedEvent)

	reports := notif.getReports()
	if len(reports) != 1 {
		t.Fatalf("expected 1 PostReport call, got %d", len(reports))
	}
	if reports[0].DecisionID != "dec-42" {
		t.Errorf("expected decision_id=dec-42, got %s", reports[0].DecisionID)
	}
	if reports[0].ReportType != "plan" {
		t.Errorf("expected report_type=plan, got %s", reports[0].ReportType)
	}
	if reports[0].Content != "Here is the implementation plan." {
		t.Errorf("expected content mismatch, got %q", reports[0].Content)
	}
}

func TestDecisions_HandleReportClosed_FetchesMissingFields(t *testing.T) {
	daemon := newMockDaemon()
	// Full bead in daemon has the content; SSE event does not.
	daemon.beads["report-2"] = &beadsapi.BeadDetail{
		ID:   "report-2",
		Type: "report",
		Fields: map[string]string{
			"decision_id": "dec-99",
			"report_type": "checklist",
			"content":     "Fetched from daemon.",
		},
	}
	notif := &mockNotifier{}
	d := NewDecisions(DecisionsConfig{
		Daemon:   daemon,
		Notifier: notif,
		Logger:   slog.Default(),
	})

	// SSE event has no content or decision_id — triggers GetBead fallback.
	closedEvent := marshalSSEBeadPayload(BeadEvent{
		ID:     "report-2",
		Type:   "report",
		Fields: map[string]string{},
	})
	d.handleReportClosed(context.Background(), closedEvent)

	reports := notif.getReports()
	if len(reports) != 1 {
		t.Fatalf("expected 1 PostReport call, got %d", len(reports))
	}
	if reports[0].DecisionID != "dec-99" {
		t.Errorf("expected decision_id=dec-99, got %s", reports[0].DecisionID)
	}
	if reports[0].Content != "Fetched from daemon." {
		t.Errorf("expected fetched content, got %q", reports[0].Content)
	}
}

func TestDecisions_HandleReportClosed_IgnoresNonReport(t *testing.T) {
	notif := &mockNotifier{}
	d := NewDecisions(DecisionsConfig{
		Notifier: notif,
		Logger:   slog.Default(),
	})

	// Decision bead close should be ignored by handleReportClosed.
	closedEvent := marshalSSEBeadPayload(BeadEvent{
		ID:   "dec-1",
		Type: "decision",
		Fields: map[string]string{
			"chosen": "yes",
		},
	})
	d.handleReportClosed(context.Background(), closedEvent)

	if len(notif.getReports()) != 0 {
		t.Fatal("non-report bead should not trigger PostReport")
	}
}

func TestDecisions_HandleReportClosed_NoDecisionID(t *testing.T) {
	notif := &mockNotifier{}
	d := NewDecisions(DecisionsConfig{
		Notifier: notif,
		Logger:   slog.Default(),
	})

	// Report with no decision_id should be skipped.
	closedEvent := marshalSSEBeadPayload(BeadEvent{
		ID:   "report-3",
		Type: "report",
		Fields: map[string]string{
			"content": "orphan report",
		},
	})
	d.handleReportClosed(context.Background(), closedEvent)

	if len(notif.getReports()) != 0 {
		t.Fatal("report without decision_id should not trigger PostReport")
	}
}

func TestDecisions_HandleCreated_ContextFieldPassedThrough(t *testing.T) {
	notif := &mockNotifier{}
	d := &Decisions{
		notifier:  notif,
		logger:    slog.Default(),
		escalated: make(map[string]time.Time),
	}

	// Decision bead with context field should pass it through to the notifier.
	decision := marshalSSEBeadPayload(BeadEvent{
		ID:       "dec-ctx-1",
		Type:     "decision",
		Title:    "Choose deployment strategy",
		Assignee: "crew-test",
		Fields: map[string]string{
			"prompt":  "Which deployment strategy?",
			"options": `["blue-green","canary"]`,
			"context": "We need to deploy the auth service update. Current system handles 10k RPM.",
		},
	})
	d.handleCreated(context.Background(), decision)

	created := notif.getCreated()
	if len(created) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(created))
	}
	if created[0].Fields["context"] != "We need to deploy the auth service update. Current system handles 10k RPM." {
		t.Errorf("expected context field to be passed through, got %q", created[0].Fields["context"])
	}
}

func TestDecisions_HandleCreated_SkipsE2EDecisions(t *testing.T) {
	notif := &mockNotifier{}
	d := &Decisions{
		notifier:  notif,
		logger:    slog.Default(),
		escalated: make(map[string]time.Time),
	}

	// E2E decision with e2e:true label should be silently skipped.
	e2eDecision := marshalSSEBeadPayload(BeadEvent{
		ID:     "dec-e2e-1",
		Type:   "decision",
		Title:  "E2E dashboard decision test-123",
		Labels: []string{"e2e:true"},
		Fields: map[string]string{
			"prompt": "Approve?",
		},
	})
	d.handleCreated(context.Background(), e2eDecision)

	if len(notif.getCreated()) != 0 {
		t.Fatal("e2e decision should not trigger Slack notification")
	}

	// Normal decision without the label should still be notified.
	normalDecision := marshalSSEBeadPayload(BeadEvent{
		ID:   "dec-normal-1",
		Type: "decision",
		Fields: map[string]string{
			"prompt": "Deploy?",
		},
	})
	d.handleCreated(context.Background(), normalDecision)

	if len(notif.getCreated()) != 1 {
		t.Fatalf("expected 1 notification for normal decision, got %d", len(notif.getCreated()))
	}
}

func TestDecisions_NotifySpawner_SendsMailToGardener(t *testing.T) {
	daemon := newMockDaemon()

	// Set up the crew agent bead with spawned_by pointing to a gardener.
	daemon.beads["agent-crew-1"] = &beadsapi.BeadDetail{
		ID:   "agent-crew-1",
		Type: "agent",
		Fields: map[string]string{
			"agent":      "crew-worker",
			"spawned_by": "my-gardener",
			"role":       "crew",
			"project":    "monorepo",
		},
	}
	// Set up the gardener agent bead (active).
	daemon.beads["my-gardener"] = &beadsapi.BeadDetail{
		ID:   "bd-gardener-1",
		Type: "agent",
		Fields: map[string]string{
			"agent":       "my-gardener",
			"agent_state": "working",
			"role":        "gardener",
		},
	}

	d := NewDecisions(DecisionsConfig{
		Daemon:   daemon,
		Notifier: &mockNotifier{},
		Logger:   slog.Default(),
	})

	// Simulate a decision created by the crew agent.
	decisionEvent := marshalSSEBeadPayload(BeadEvent{
		ID:       "dec-spawn-1",
		Type:     "decision",
		Assignee: "crew-worker",
		Fields: map[string]string{
			"prompt":                   "Should we proceed with the fix?",
			"options":                  `[{"id":"yes","short":"Yes"},{"id":"no","short":"No"}]`,
			"requested_by":             "crew-worker",
			"requesting_agent_bead_id": "agent-crew-1",
		},
	})
	d.handleCreated(context.Background(), decisionEvent)

	// Verify a mail bead was created for the gardener.
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	var mailFound *beadsapi.BeadDetail
	for _, b := range daemon.mailBeadsLocked() {
		if b.Type == "mail" && b.Assignee == "my-gardener" {
			mailFound = b
			break
		}
	}
	if mailFound == nil {
		t.Fatal("expected mail bead for gardener, got none")
	}
	if mailFound.Fields["decision_bead_id"] != "dec-spawn-1" {
		t.Errorf("expected decision_bead_id=dec-spawn-1, got %s", mailFound.Fields["decision_bead_id"])
	}
	if mailFound.Fields["source_agent"] != "crew-worker" {
		t.Errorf("expected source_agent=crew-worker, got %s", mailFound.Fields["source_agent"])
	}
	// Check delivery:interrupt label is present.
	hasInterrupt := false
	for _, l := range mailFound.Labels {
		if l == "delivery:interrupt" {
			hasInterrupt = true
			break
		}
	}
	if !hasInterrupt {
		t.Error("expected delivery:interrupt label on mail bead")
	}
}

func TestDecisions_NotifySpawner_SkipsWhenNoSpawnedBy(t *testing.T) {
	daemon := newMockDaemon()

	// Crew agent without spawned_by field.
	daemon.beads["agent-crew-2"] = &beadsapi.BeadDetail{
		ID:   "agent-crew-2",
		Type: "agent",
		Fields: map[string]string{
			"agent":   "crew-solo",
			"role":    "crew",
			"project": "monorepo",
		},
	}

	d := NewDecisions(DecisionsConfig{
		Daemon:   daemon,
		Notifier: &mockNotifier{},
		Logger:   slog.Default(),
	})

	decisionEvent := marshalSSEBeadPayload(BeadEvent{
		ID:       "dec-spawn-2",
		Type:     "decision",
		Assignee: "crew-solo",
		Fields: map[string]string{
			"prompt":                   "Deploy?",
			"requested_by":             "crew-solo",
			"requesting_agent_bead_id": "agent-crew-2",
		},
	})
	d.handleCreated(context.Background(), decisionEvent)

	// No mail bead should be created.
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	for _, b := range daemon.mailBeadsLocked() {
		if b.Type == "mail" {
			t.Fatal("no mail bead should be created when spawned_by is empty")
		}
	}
}

func TestDecisions_NotifySpawner_SkipsWhenSpawnerDone(t *testing.T) {
	daemon := newMockDaemon()

	daemon.beads["agent-crew-3"] = &beadsapi.BeadDetail{
		ID:   "agent-crew-3",
		Type: "agent",
		Fields: map[string]string{
			"agent":      "crew-late",
			"spawned_by": "dead-gardener",
			"role":       "crew",
		},
	}
	// Gardener exists but is done.
	daemon.beads["dead-gardener"] = &beadsapi.BeadDetail{
		ID:   "bd-gardener-dead",
		Type: "agent",
		Fields: map[string]string{
			"agent":       "dead-gardener",
			"agent_state": "done",
			"role":        "gardener",
		},
	}

	d := NewDecisions(DecisionsConfig{
		Daemon:   daemon,
		Notifier: &mockNotifier{},
		Logger:   slog.Default(),
	})

	decisionEvent := marshalSSEBeadPayload(BeadEvent{
		ID:       "dec-spawn-3",
		Type:     "decision",
		Assignee: "crew-late",
		Fields: map[string]string{
			"prompt":                   "Too late?",
			"requested_by":             "crew-late",
			"requesting_agent_bead_id": "agent-crew-3",
		},
	})
	d.handleCreated(context.Background(), decisionEvent)

	// No mail bead should be created since gardener is done.
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	for _, b := range daemon.mailBeadsLocked() {
		if b.Type == "mail" {
			t.Fatal("no mail bead should be created when spawner is done")
		}
	}
}

func TestDecisions_HandleClosed_RespawnsDeadAgent(t *testing.T) {
	// Set up a daemon where the agent bead exists but has no coop_url (dead).
	daemon := newMockDaemon()
	agentBead := &beadsapi.BeadDetail{
		ID:   "bd-agent-1",
		Type: "agent",
		Fields: map[string]string{
			"agent":       "my-crew-agent",
			"agent_state": "done",
			"project":     "gardenio",
			"task_id":     "gd-TASK123",
			"role":        "crew",
		},
	}
	daemon.beads["my-crew-agent"] = agentBead // for FindAgentBead
	daemon.beads["bd-agent-1"] = agentBead    // for GetBead by ID
	notif := &mockNotifier{}

	d := NewDecisions(DecisionsConfig{
		Daemon:   daemon,
		Notifier: notif,
		Logger:   slog.Default(),
	})

	closedEvent := marshalSSEBeadPayload(BeadEvent{
		ID:       "dec-respawn-1",
		Type:     "decision",
		Assignee: "my-crew-agent",
		Labels:   []string{"project:gardenio"},
		Fields: map[string]string{
			"prompt":                   "Should we deploy?",
			"chosen":                   "yes-deploy",
			"rationale":                "ready to go",
			"requesting_agent_bead_id": "bd-agent-1",
		},
	})
	d.handleClosed(context.Background(), closedEvent)

	// Verify a new agent bead was spawned.
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	var spawnedAgent *beadsapi.BeadDetail
	for _, b := range daemon.beads {
		if b.Type == "agent" && b.Fields["spawn_source"] == "decision-resume" {
			spawnedAgent = b
			break
		}
	}
	if spawnedAgent == nil {
		t.Fatal("expected a new agent bead to be spawned for decision resume")
	}
	if spawnedAgent.Fields["decision_bead_id"] != "dec-respawn-1" {
		t.Errorf("expected decision_bead_id=dec-respawn-1, got %s", spawnedAgent.Fields["decision_bead_id"])
	}
	if spawnedAgent.Fields["project"] != "gardenio" {
		t.Errorf("expected project=gardenio, got %s", spawnedAgent.Fields["project"])
	}
	if spawnedAgent.Fields["task_id"] != "gd-TASK123" {
		t.Errorf("expected task_id=gd-TASK123, got %s", spawnedAgent.Fields["task_id"])
	}
	if spawnedAgent.Fields["prompt"] == "" {
		t.Error("expected prompt to contain decision context")
	}
}

func TestDecisions_HandleClosed_RespawnPreservesPlanID(t *testing.T) {
	// A plan-gardener agent is bound to its plan epic by fields.plan_id (the
	// reverse-lookup key the beads-viewer's planAssistant() resolver keys on).
	// A decision-resume respawn must carry plan_id forward, or the respawned
	// gardener comes back bound only by task_id — a second same-named gardener
	// bead with plan_id=null that fragments plan resolution (ka-0000000352).
	daemon := newMockDaemon()
	agentBead := &beadsapi.BeadDetail{
		ID:   "bd-plan-gardener-1",
		Type: "agent",
		Fields: map[string]string{
			"agent":       "gardenio-plan-gardener-gd-plan99",
			"agent_state": "done",
			"project":     "gardenio",
			"task_id":     "gd-plan99",
			"plan_id":     "gd-plan99",
			"role":        "plan-gardener",
		},
	}
	daemon.beads["gardenio-plan-gardener-gd-plan99"] = agentBead
	daemon.beads["bd-plan-gardener-1"] = agentBead

	d := NewDecisions(DecisionsConfig{
		Daemon:   daemon,
		Notifier: &mockNotifier{},
		Logger:   slog.Default(),
	})

	closedEvent := marshalSSEBeadPayload(BeadEvent{
		ID:       "dec-plan-gate-1",
		Type:     "decision",
		Assignee: "gardenio-plan-gardener-gd-plan99",
		Labels:   []string{"project:gardenio"},
		Fields: map[string]string{
			"prompt":                   "Approve & build?",
			"chosen":                   "homepage",
			"rationale":                "approved",
			"requesting_agent_bead_id": "bd-plan-gardener-1",
		},
	})
	d.handleClosed(context.Background(), closedEvent)

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	var spawnedAgent *beadsapi.BeadDetail
	for _, b := range daemon.beads {
		if b.Type == "agent" && b.Fields["spawn_source"] == "decision-resume" {
			spawnedAgent = b
			break
		}
	}
	if spawnedAgent == nil {
		t.Fatal("expected a new agent bead to be spawned for decision resume")
	}
	if spawnedAgent.Fields["plan_id"] != "gd-plan99" {
		t.Errorf("expected plan_id=gd-plan99 carried forward, got %q", spawnedAgent.Fields["plan_id"])
	}
}

// TestDecisions_HandleClosed_NoRespawnForHumanOrUnknownRequester is the
// regression guard for gd-Iii8U3m2pt: a decision the owner filed with kd from a
// laptop carries created_by=<email> and no agent requester. Resolving it minted
// agent bead 'crew/<email>' (spawn_source=decision-resume). A human principal,
// or a name with no agent bead behind it, must never be respawned as a crew.
func TestDecisions_HandleClosed_NoRespawnForHumanOrUnknownRequester(t *testing.T) {
	cases := []struct {
		name      string
		assignee  string
		createdBy string
	}{
		{name: "email filer", createdBy: "owner@example.com"},
		{name: "oidc filer", createdBy: "oidc:owner@example.com"},
		{name: "email assignee", assignee: "owner@example.com", createdBy: "laptop-claude"},
		{name: "slack operator", createdBy: "slack:U0123ABCD"},
		{name: "unknown actor with no agent bead", createdBy: "laptop-operator"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			daemon := newMockDaemon()
			d := NewDecisions(DecisionsConfig{
				Daemon:   daemon,
				Notifier: &mockNotifier{},
				Logger:   slog.Default(),
			})
			d.handleClosed(context.Background(), marshalSSEBeadPayload(BeadEvent{
				ID:        "dec-human-1",
				Type:      "decision",
				Assignee:  tc.assignee,
				CreatedBy: tc.createdBy,
				Labels:    []string{"project:gardenio"},
				Fields: map[string]string{
					"prompt": "Which way?",
					"chosen": "a",
				},
			}))
			if n := countDecisionResumes(daemon); n != 0 {
				t.Fatalf("decision by %q/%q must not respawn anyone, got %d spawn(s)", tc.assignee, tc.createdBy, n)
			}
		})
	}
}

// An agent requester found by name (no requesting_agent_bead_id, as with a
// decision an agent filed through kd) still respawns: the human/unknown skip
// must not swallow real agents (gd-Iii8U3m2pt). The requester has gb-stopped,
// so its bead is CLOSED and the real daemon's FindAgentBead cannot see it;
// enforceActiveStatusScoping makes the mock match, so only the any-status
// lookup resolves it.
func TestDecisions_HandleClosed_RespawnsAgentRequesterFoundByName(t *testing.T) {
	daemon := newMockDaemon()
	daemon.enforceActiveStatusScoping = true
	daemon.beads["gardenio-some-crew-abc"] = &beadsapi.BeadDetail{
		ID:     "bd-agent-byname",
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{
			"agent":       "gardenio-some-crew-abc",
			"agent_state": "done",
			"project":     "gardenio",
			"task_id":     "gd-TASK9",
			"role":        "crew",
		},
	}
	d := NewDecisions(DecisionsConfig{
		Daemon:   daemon,
		Notifier: &mockNotifier{},
		Logger:   slog.Default(),
	})
	d.handleClosed(context.Background(), marshalSSEBeadPayload(BeadEvent{
		ID:        "dec-byname-1",
		Type:      "decision",
		CreatedBy: "gardenio-some-crew-abc",
		Labels:    []string{"project:gardenio"},
		Fields: map[string]string{
			"prompt": "Ship it?",
			"chosen": "yes",
		},
	}))
	if n := countDecisionResumes(daemon); n != 1 {
		t.Fatalf("an agent requester must still be respawned once, got %d spawn(s)", n)
	}
}

// The laptop operator files with `kd --actor laptop-claude` under the owner's
// login: assignee=laptop-claude, created_by=<email>. Earlier wrong respawns left
// a closed laptop-claude agent bead, so the any-status lookup finds one; the
// human creator must still stop the respawn (gd-xQ9Vi6jBmS). Same setup as the
// found-by-name test above, which does respawn, except for the creator.
func TestDecisions_HandleClosed_NoRespawnForHumanCreatorUnderActorName(t *testing.T) {
	daemon := newMockDaemon()
	daemon.enforceActiveStatusScoping = true
	daemon.beads["laptop-claude"] = &beadsapi.BeadDetail{
		ID:     "bd-agent-laptop",
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{
			"agent":       "laptop-claude",
			"agent_state": "done",
			"project":     "gardenio",
			"role":        "crew",
		},
	}
	d := NewDecisions(DecisionsConfig{
		Daemon:   daemon,
		Notifier: &mockNotifier{},
		Logger:   slog.Default(),
	})
	d.handleClosed(context.Background(), marshalSSEBeadPayload(BeadEvent{
		ID:        "dec-actor-1",
		Type:      "decision",
		Assignee:  "laptop-claude",
		CreatedBy: "owner@example.com",
		Labels:    []string{"project:gardenio"},
		Fields: map[string]string{
			"prompt": "Keep it?",
			"chosen": "keep",
		},
	}))
	if n := countDecisionResumes(daemon); n != 0 {
		t.Fatalf("a decision a human created under an actor name must not respawn the actor, got %d spawn(s)", n)
	}
}

// A failed any-status lookup is not proof the requester is not an agent: the
// respawn goes ahead without agent context rather than strand a parked task.
func TestDecisions_HandleClosed_RespawnsWhenAgentLookupFails(t *testing.T) {
	daemon := newMockDaemon()
	daemon.enforceActiveStatusScoping = true
	daemon.findAgentAnyErr = errors.New("daemon unavailable")
	d := NewDecisions(DecisionsConfig{
		Daemon:   daemon,
		Notifier: &mockNotifier{},
		Logger:   slog.Default(),
	})
	d.handleClosed(context.Background(), marshalSSEBeadPayload(BeadEvent{
		ID:        "dec-lookupfail-1",
		Type:      "decision",
		CreatedBy: "gardenio-some-crew-abc",
		Labels:    []string{"project:gardenio"},
		Fields: map[string]string{
			"prompt": "Ship it?",
			"chosen": "yes",
		},
	}))
	if n := countDecisionResumes(daemon); n != 1 {
		t.Fatalf("a lookup failure must fall back to respawning, got %d spawn(s)", n)
	}
}

func TestIsHumanFiledDecision(t *testing.T) {
	cases := map[string]bool{
		"oidc:owner@example.com": true,
		"owner@example.com":      true,
		" oidc:owner ":           true,
		"slack:U0123ABCD":        true,
		"gardenio-some-crew-abc": false,
		"crew/my-agent":          false,
		"machine:gardenio-bv":    false,
		"":                       false,
	}
	for name, want := range cases {
		if got := isHumanFiledDecision(name); got != want {
			t.Errorf("isHumanFiledDecision(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestDecisions_HandleClosed_NoRespawnForExpired(t *testing.T) {
	// Agent is dead, decision is expired — should NOT respawn.
	daemon := newMockDaemon()
	daemon.beads["dead-agent"] = &beadsapi.BeadDetail{
		ID:   "bd-dead-1",
		Type: "agent",
		Fields: map[string]string{
			"agent":       "dead-agent",
			"agent_state": "done",
			"project":     "gardenio",
		},
	}

	d := NewDecisions(DecisionsConfig{
		Daemon:   daemon,
		Notifier: &mockNotifier{},
		Logger:   slog.Default(),
	})

	expiredEvent := marshalSSEBeadPayload(BeadEvent{
		ID:       "dec-expired-1",
		Type:     "decision",
		Assignee: "dead-agent",
		Fields: map[string]string{
			"chosen":    "_expired",
			"rationale": "timed out",
		},
	})
	d.handleClosed(context.Background(), expiredEvent)

	// No new agent should be spawned.
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	for _, b := range daemon.beads {
		if b.Type == "agent" && b.Fields["spawn_source"] == "decision-resume" {
			t.Fatal("should not respawn agent for expired decision")
		}
	}
}

// TestDecisions_HandleClosed_NoRespawnForE2EFixture is the regression guard for
// ka-0000000809: closing an E2E test-fixture decision (as
// test-auto-close-cascade.sh scenario 4 does) must NEVER spawn a real crew pod.
// The fixture's created_by is a throwaway e2e actor, so the nudge always fails
// and the un-guarded respawn path always fired — one fixture close = one leaked
// CrashLooping pod holding cluster capacity hostage.
func TestDecisions_HandleClosed_NoRespawnForE2EFixture(t *testing.T) {
	// Each case is a distinct way the real fixture is recognizable as e2e. Any
	// ONE must be enough to suppress the respawn (the guards are redundant on
	// purpose), so every case is exercised in isolation.
	cases := []struct {
		name      string
		createdBy string
		labels    []string
	}{
		{
			// The secondary module fix labels every fixture fixture:e2e:*.
			name:   "fixture-label",
			labels: []string{"fixture:e2e:auto-close-cascade", "project:e2e-test"},
		},
		{
			// The unlabelled fixture as it exists TODAY: no fixture: label, only
			// the project:e2e-test coincidence and the e2e-* KD_ACTOR created_by.
			name:      "project-and-actor-only",
			createdBy: "e2e-test-autoclose-12345",
			labels:    []string{"project:e2e-test"},
		},
		{
			// project:e2e-test alone (hard refusal — no real agent belongs there).
			name:   "project-only",
			labels: []string{"project:e2e-test"},
		},
		{
			// The e2e:true label decision watchers use.
			name:   "e2e-true-label",
			labels: []string{"e2e:true", "project:gardenio"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			daemon := newMockDaemon()
			agentBead := &beadsapi.BeadDetail{
				ID:   "bd-e2e-actor",
				Type: "agent",
				Fields: map[string]string{
					"agent":       "e2e-fixture-agent",
					"agent_state": "done",
					"project":     "e2e-test",
				},
			}
			daemon.beads["e2e-fixture-agent"] = agentBead
			daemon.beads["bd-e2e-actor"] = agentBead

			d := NewDecisions(DecisionsConfig{
				Daemon:   daemon,
				Notifier: &mockNotifier{},
				Logger:   slog.Default(),
			})

			closedEvent := marshalSSEBeadPayload(BeadEvent{
				ID:        "dec-e2e-fixture-1",
				Type:      "decision",
				Assignee:  "e2e-fixture-agent",
				CreatedBy: tc.createdBy,
				Labels:    tc.labels,
				Fields: map[string]string{
					"prompt": "e2e auto-close cascade fixture — not a real decision",
					"chosen": "a",
				},
			})
			d.handleClosed(context.Background(), closedEvent)

			daemon.mu.Lock()
			defer daemon.mu.Unlock()
			for _, b := range daemon.beads {
				if b.Type == "agent" && b.Fields["spawn_source"] == "decision-resume" {
					t.Fatalf("must NOT respawn a real pod for an e2e-fixture decision (%s)", tc.name)
				}
			}
		})
	}
}

// TestDecisions_HandleClosed_NoRespawnForE2EProject_LabelsStripped guards the
// belt-and-suspenders path (ka-0000000809): an SSE close event can arrive with
// its labels dropped, so the label-based isE2EDecisionRespawn check would miss
// it — but the agent bead still resolves project=e2e-test, and the last gate
// before SpawnAgent must refuse that project regardless.
func TestDecisions_HandleClosed_NoRespawnForE2EProject_LabelsStripped(t *testing.T) {
	daemon := newMockDaemon()
	agentBead := &beadsapi.BeadDetail{
		ID:   "bd-e2e-proj",
		Type: "agent",
		Fields: map[string]string{
			"agent":       "e2e-proj-agent",
			"agent_state": "done",
			"project":     "e2e-test", // resolves to the throwaway project
			"task_id":     "gd-E2ETASK",
		},
	}
	daemon.beads["e2e-proj-agent"] = agentBead
	daemon.beads["bd-e2e-proj"] = agentBead

	d := NewDecisions(DecisionsConfig{
		Daemon:   daemon,
		Notifier: &mockNotifier{},
		Logger:   slog.Default(),
	})

	// No labels, no e2e-* created_by: only the resolved agent-bead project
	// betrays this as an e2e fixture, exercising the spawn-site gate.
	closedEvent := marshalSSEBeadPayload(BeadEvent{
		ID:       "dec-e2e-proj-1",
		Type:     "decision",
		Assignee: "e2e-proj-agent",
		Fields: map[string]string{
			"prompt":                   "fixture?",
			"chosen":                   "a",
			"requesting_agent_bead_id": "bd-e2e-proj",
		},
	})
	d.handleClosed(context.Background(), closedEvent)

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	for _, b := range daemon.beads {
		if b.Type == "agent" && b.Fields["spawn_source"] == "decision-resume" {
			t.Fatal("must NOT respawn a real pod into project=e2e-test even with labels stripped")
		}
	}
}

// TestIsE2EDecisionRespawn unit-tests the guard predicate directly, including
// the negative case: a real cross-project decision must still respawn.
func TestIsE2EDecisionRespawn(t *testing.T) {
	cases := []struct {
		name      string
		createdBy string
		labels    []string
		want      bool
	}{
		{"fixture-label", "", []string{"fixture:e2e:auto-close-cascade"}, true},
		{"project-e2e-test", "", []string{"project:e2e-test"}, true},
		{"e2e-true-label", "", []string{"e2e:true", "project:gardenio"}, true},
		{"e2e-actor-prefix", "e2e-test-autoclose-1", []string{"project:gardenio"}, true},
		{"real-gardenio-decision", "gardenio-crew-abc", []string{"project:gardenio"}, false},
		{"no-labels-no-actor", "", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isE2EDecisionRespawn(BeadEvent{CreatedBy: tc.createdBy, Labels: tc.labels})
			if got != tc.want {
				t.Errorf("isE2EDecisionRespawn(createdBy=%q, labels=%v) = %v, want %v",
					tc.createdBy, tc.labels, got, tc.want)
			}
		})
	}
}

func TestDecisions_HandleClosed_RespawnDedup(t *testing.T) {
	// Verify that respawn dedup prevents double-spawning for the same decision.
	daemon := newMockDaemon()
	daemon.beads["dup-agent"] = &beadsapi.BeadDetail{
		ID:   "bd-dup-1",
		Type: "agent",
		Fields: map[string]string{
			"agent":       "dup-agent",
			"agent_state": "done",
			"project":     "gardenio",
		},
	}

	d := NewDecisions(DecisionsConfig{
		Daemon:   daemon,
		Notifier: &mockNotifier{},
		Logger:   slog.Default(),
	})

	closedEvent := marshalSSEBeadPayload(BeadEvent{
		ID:       "dec-dup-1",
		Type:     "decision",
		Assignee: "dup-agent",
		Fields: map[string]string{
			"prompt": "Deploy?",
			"chosen": "yes",
		},
	})

	// First close: should spawn.
	d.handleClosed(context.Background(), closedEvent)
	// Second close: should be deduped.
	d.handleClosed(context.Background(), closedEvent)

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	spawnCount := 0
	for _, b := range daemon.beads {
		if b.Type == "agent" && b.Fields["spawn_source"] == "decision-resume" {
			spawnCount++
		}
	}
	if spawnCount != 1 {
		t.Fatalf("expected exactly 1 spawned agent (dedup), got %d", spawnCount)
	}
}

func TestDecisions_HandleClosed_RespawnPreservesSlackThread(t *testing.T) {
	// Verify that Slack thread binding is propagated to the respawned agent.
	daemon := newMockDaemon()
	threadBead := &beadsapi.BeadDetail{
		ID:   "bd-thread-1",
		Type: "agent",
		Fields: map[string]string{
			"agent":                "thread-agent",
			"agent_state":          "done",
			"project":              "gardenio",
			"slack_thread_channel": "C12345",
			"slack_thread_ts":      "1234567890.123456",
			"slack_user_id":        "U12345",
		},
	}
	daemon.beads["thread-agent"] = threadBead // for FindAgentBead
	daemon.beads["bd-thread-1"] = threadBead  // for GetBead by ID

	d := NewDecisions(DecisionsConfig{
		Daemon:   daemon,
		Notifier: &mockNotifier{},
		Logger:   slog.Default(),
	})

	closedEvent := marshalSSEBeadPayload(BeadEvent{
		ID:       "dec-thread-1",
		Type:     "decision",
		Assignee: "thread-agent",
		Fields: map[string]string{
			"prompt":                   "Proceed?",
			"chosen":                   "yes",
			"requesting_agent_bead_id": "bd-thread-1",
		},
	})
	d.handleClosed(context.Background(), closedEvent)

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	for _, b := range daemon.beads {
		if b.Type == "agent" && b.Fields["spawn_source"] == "decision-resume" {
			if b.Fields["slack_thread_channel"] != "C12345" {
				t.Errorf("expected slack_thread_channel=C12345, got %s", b.Fields["slack_thread_channel"])
			}
			if b.Fields["slack_thread_ts"] != "1234567890.123456" {
				t.Errorf("expected slack_thread_ts preserved, got %s", b.Fields["slack_thread_ts"])
			}
			if b.Fields["slack_user_id"] != "U12345" {
				t.Errorf("expected slack_user_id=U12345, got %s", b.Fields["slack_user_id"])
			}
			return
		}
	}
	t.Fatal("expected a spawned agent with decision-resume source")
}

// TestDecisions_HandleClosed_RespawnPreservesOperatedBy verifies that a respawn
// inherits the prior agent bead's human operated_by — both mirrored into the
// spawned bead's field AND stamped onto ctx (X-Operated-By) — rather than
// dead-ending on the fleet:slack-bridge sentinel (ka-0000007919).
func TestDecisions_HandleClosed_RespawnPreservesOperatedBy(t *testing.T) {
	daemon := newMockDaemon()
	priorBead := &beadsapi.BeadDetail{
		ID:   "bd-op-1",
		Type: "agent",
		Fields: map[string]string{
			"agent":       "op-agent",
			"agent_state": "done",
			"project":     "gardenio",
			"operated_by": "oidc:matthew.baker@acme.example",
		},
	}
	daemon.beads["op-agent"] = priorBead
	daemon.beads["bd-op-1"] = priorBead

	d := NewDecisions(DecisionsConfig{
		Daemon:   daemon,
		Notifier: &mockNotifier{},
		Logger:   slog.Default(),
	})

	closedEvent := marshalSSEBeadPayload(BeadEvent{
		ID:       "dec-op-1",
		Type:     "decision",
		Assignee: "op-agent",
		Fields: map[string]string{
			"prompt":                   "Proceed?",
			"chosen":                   "yes",
			"requesting_agent_bead_id": "bd-op-1",
		},
	})
	d.handleClosed(context.Background(), closedEvent)

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if got := beadsapi.OperatorFromContext(daemon.lastSpawnAgentCtx); got != "oidc:matthew.baker@acme.example" {
		t.Errorf("expected ctx operator oidc:matthew.baker@acme.example, got %q", got)
	}
	for _, b := range daemon.beads {
		if b.Type == "agent" && b.Fields["spawn_source"] == "decision-resume" {
			if b.Fields["operated_by"] != "oidc:matthew.baker@acme.example" {
				t.Errorf("expected operated_by=oidc:matthew.baker@acme.example, got %s", b.Fields["operated_by"])
			}
			if b.Fields["spawned_by"] != "bd-op-1" {
				t.Errorf("expected spawned_by=bd-op-1 (lineage edge), got %s", b.Fields["spawned_by"])
			}
			return
		}
	}
	t.Fatal("expected a spawned agent with decision-resume source")
}

// TestDecisions_HandleClosed_RespawnDerivesOperatedByFromSlackUser verifies the
// fallback: when the prior agent bead has no human operated_by but does carry a
// slack_user_id, the respawn re-derives a daemon-accepted slack:U… operator
// (ka-0000007919).
func TestDecisions_HandleClosed_RespawnDerivesOperatedByFromSlackUser(t *testing.T) {
	daemon := newMockDaemon()
	priorBead := &beadsapi.BeadDetail{
		ID:   "bd-op-2",
		Type: "agent",
		Fields: map[string]string{
			"agent":         "slack-agent",
			"agent_state":   "done",
			"project":       "gardenio",
			"slack_user_id": "U0OPERATOR1",
			// operated_by intentionally absent (or a non-human sentinel).
			"operated_by": "fleet:slack-bridge",
		},
	}
	daemon.beads["slack-agent"] = priorBead
	daemon.beads["bd-op-2"] = priorBead

	d := NewDecisions(DecisionsConfig{
		Daemon:   daemon,
		Notifier: &mockNotifier{},
		Logger:   slog.Default(),
	})

	closedEvent := marshalSSEBeadPayload(BeadEvent{
		ID:       "dec-op-2",
		Type:     "decision",
		Assignee: "slack-agent",
		Fields: map[string]string{
			"prompt":                   "Proceed?",
			"chosen":                   "yes",
			"requesting_agent_bead_id": "bd-op-2",
		},
	})
	d.handleClosed(context.Background(), closedEvent)

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if got := beadsapi.OperatorFromContext(daemon.lastSpawnAgentCtx); got != "slack:U0OPERATOR1" {
		t.Errorf("expected ctx operator slack:U0OPERATOR1, got %q", got)
	}
	for _, b := range daemon.beads {
		if b.Type == "agent" && b.Fields["spawn_source"] == "decision-resume" {
			if b.Fields["operated_by"] != "slack:U0OPERATOR1" {
				t.Errorf("expected operated_by=slack:U0OPERATOR1, got %s", b.Fields["operated_by"])
			}
			return
		}
	}
	t.Fatal("expected a spawned agent with decision-resume source")
}

// TestHumanOperatorFromAgentBead unit-tests the resolution helper in isolation:
// human operated_by wins, else slack_user_id is promoted, else "".
func TestHumanOperatorFromAgentBead(t *testing.T) {
	cases := []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{"nil", nil, ""},
		{"human operated_by wins", map[string]string{"operated_by": "oidc:a@b.co", "slack_user_id": "U123"}, "oidc:a@b.co"},
		{"slack operated_by wins", map[string]string{"operated_by": "slack:U999"}, "slack:U999"},
		{"derive from slack_user_id", map[string]string{"operated_by": "fleet:slack-bridge", "slack_user_id": "U123"}, "slack:U123"},
		{"no operated_by, slack_user_id", map[string]string{"slack_user_id": "U123"}, "slack:U123"},
		{"non-human everything", map[string]string{"operated_by": "fleet:slack-bridge"}, ""},
		{"empty", map[string]string{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var bead *beadsapi.BeadDetail
			if c.fields != nil {
				bead = &beadsapi.BeadDetail{Fields: c.fields}
			}
			if got := humanOperatorFromAgentBead(bead); got != c.want {
				t.Errorf("humanOperatorFromAgentBead = %q; want %q", got, c.want)
			}
		})
	}
}

func TestDecisions_HandleClosed_NoRespawnWhenNudgeSucceeds(t *testing.T) {
	// When the agent is alive and nudge succeeds, no respawn should happen.
	coopServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer coopServer.Close()

	daemon := newMockDaemon()
	daemon.beads["alive-agent"] = &beadsapi.BeadDetail{
		ID:    "alive-agent",
		Notes: "coop_url: " + coopServer.URL,
		Fields: map[string]string{
			"agent":       "alive-agent",
			"agent_state": "working",
		},
	}

	d := NewDecisions(DecisionsConfig{
		Daemon:   daemon,
		Notifier: &mockNotifier{},
		Logger:   slog.Default(),
	})

	closedEvent := marshalSSEBeadPayload(BeadEvent{
		ID:       "dec-alive-1",
		Type:     "decision",
		Assignee: "alive-agent",
		Fields: map[string]string{
			"prompt": "Deploy?",
			"chosen": "yes",
		},
	})
	d.handleClosed(context.Background(), closedEvent)

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	for _, b := range daemon.beads {
		if b.Type == "agent" && b.Fields["spawn_source"] == "decision-resume" {
			t.Fatal("should not respawn agent when nudge succeeds")
		}
	}
}

func TestIsShutdownDecision(t *testing.T) {
	tests := []struct {
		chosen string
		want   bool
	}{
		{"shut down", true},
		{"Shut down the agent", true},
		{"Work is complete, shut down the agent", true},
		{"Stand down, work complete", true},
		{"shutdown", true},
		{"Shutdown the agent", true},
		{"SHUT DOWN", true},
		{"yes-deploy", false},
		{"proceed", false},
		{"dismissed", false},
		{"_expired", false},
		{"", false},
		{"Create a plan", false},
	}
	for _, tt := range tests {
		if got := isShutdownDecision(tt.chosen); got != tt.want {
			t.Errorf("isShutdownDecision(%q) = %v, want %v", tt.chosen, got, tt.want)
		}
	}
}

func TestDecisions_HandleClosed_NoRespawnForShutdown(t *testing.T) {
	// Agent is dead, decision chosen is "shut down" — should NOT respawn.
	daemon := newMockDaemon()
	daemon.beads["shutdown-agent"] = &beadsapi.BeadDetail{
		ID:   "bd-shutdown-1",
		Type: "agent",
		Fields: map[string]string{
			"agent":       "shutdown-agent",
			"agent_state": "done",
			"project":     "gardenio",
		},
	}

	d := NewDecisions(DecisionsConfig{
		Daemon:   daemon,
		Notifier: &mockNotifier{},
		Logger:   slog.Default(),
	})

	shutdownEvent := marshalSSEBeadPayload(BeadEvent{
		ID:       "dec-shutdown-1",
		Type:     "decision",
		Assignee: "shutdown-agent",
		Fields: map[string]string{
			"prompt": "Session complete. What next?",
			"chosen": "Work is complete, shut down the agent",
		},
	})
	d.handleClosed(context.Background(), shutdownEvent)

	// No new agent should be spawned.
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	for _, b := range daemon.beads {
		if b.Type == "agent" && b.Fields["spawn_source"] == "decision-resume" {
			t.Fatal("should not respawn agent for shutdown decision")
		}
	}
}

func TestDecisions_HandleClosed_NoRespawnForStandDown(t *testing.T) {
	// Agent is dead, decision chosen is "stand down" — should NOT respawn.
	daemon := newMockDaemon()
	daemon.beads["standdown-agent"] = &beadsapi.BeadDetail{
		ID:   "bd-standdown-1",
		Type: "agent",
		Fields: map[string]string{
			"agent":       "standdown-agent",
			"agent_state": "done",
			"project":     "gardenio",
		},
	}

	d := NewDecisions(DecisionsConfig{
		Daemon:   daemon,
		Notifier: &mockNotifier{},
		Logger:   slog.Default(),
	})

	standdownEvent := marshalSSEBeadPayload(BeadEvent{
		ID:       "dec-standdown-1",
		Type:     "decision",
		Assignee: "standdown-agent",
		Fields: map[string]string{
			"prompt": "All done. Options:",
			"chosen": "Stand down, work complete",
		},
	})
	d.handleClosed(context.Background(), standdownEvent)

	// No new agent should be spawned.
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	for _, b := range daemon.beads {
		if b.Type == "agent" && b.Fields["spawn_source"] == "decision-resume" {
			t.Fatal("should not respawn agent for stand-down decision")
		}
	}
}

func TestMockDaemon_ListDecisionBeads_Empty(t *testing.T) {
	daemon := newMockDaemon()

	decisions, err := daemon.ListDecisionBeads(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 0 {
		t.Fatalf("expected 0 decisions, got %d", len(decisions))
	}
}
