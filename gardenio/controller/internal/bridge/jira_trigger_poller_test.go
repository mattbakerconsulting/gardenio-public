package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// triggerPollerDaemon implements both JiraBeadClient (for the poller) and
// TriggerBeadClient (for the trigger evaluator).
type triggerPollerDaemon struct {
	mu     sync.Mutex
	beads  map[string]*beadsapi.BeadDetail
	nextID int

	createCalls int
	updateCalls int
	spawnCalls  int
	closeCalls  int
}

func newTriggerPollerDaemon() *triggerPollerDaemon {
	return &triggerPollerDaemon{
		beads: make(map[string]*beadsapi.BeadDetail),
	}
}

func (m *triggerPollerDaemon) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createCalls++
	m.nextID++
	id := fmt.Sprintf("bd-tp-%d", m.nextID)
	var fields map[string]string
	if len(req.Fields) > 0 {
		_ = json.Unmarshal(req.Fields, &fields)
	}
	m.beads[id] = &beadsapi.BeadDetail{
		ID:     id,
		Title:  req.Title,
		Type:   req.Type,
		Status: "open",
		Labels: req.Labels,
		Fields: fields,
	}
	return id, nil
}

func (m *triggerPollerDaemon) ListTaskBeads(_ context.Context) ([]*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []*beadsapi.BeadDetail
	for _, b := range m.beads {
		if b.Type == "task" {
			result = append(result, b)
		}
	}
	return result, nil
}

func (m *triggerPollerDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Auto-inject a past last_checked so triggers aren't caught by the
	// runtime-bootstrap guard in these tests.
	for _, b := range m.beads {
		ensurePastLastChecked(b)
	}
	var result []*beadsapi.BeadDetail
	for _, b := range m.beads {
		match := true
		if len(q.Types) > 0 {
			typeMatch := false
			for _, t := range q.Types {
				if b.Type == t {
					typeMatch = true
					break
				}
			}
			if !typeMatch {
				match = false
			}
		}
		if len(q.Kinds) > 0 {
			kindMatch := false
			for _, k := range q.Kinds {
				if b.Kind == k {
					kindMatch = true
					break
				}
			}
			if !kindMatch {
				match = false
			}
		}
		if len(q.Labels) > 0 {
			for _, ql := range q.Labels {
				found := false
				for _, bl := range b.Labels {
					if bl == ql {
						found = true
						break
					}
				}
				if !found {
					match = false
					break
				}
			}
		}
		if len(q.Statuses) > 0 {
			statusMatch := false
			for _, s := range q.Statuses {
				if b.Status == s {
					statusMatch = true
					break
				}
			}
			if !statusMatch {
				match = false
			}
		}
		if match {
			result = append(result, b)
		}
	}
	return &beadsapi.ListBeadsResult{Beads: result, Total: len(result)}, nil
}

func (m *triggerPollerDaemon) AddDependency(_ context.Context, _, _, _, _ string) error { return nil }

func (m *triggerPollerDaemon) AddComment(_ context.Context, _, _, _ string) error { return nil }

func (m *triggerPollerDaemon) UpdateBeadFields(_ context.Context, beadID string, fields map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updateCalls++
	if b, ok := m.beads[beadID]; ok {
		if b.Fields == nil {
			b.Fields = make(map[string]string)
		}
		for k, v := range fields {
			b.Fields[k] = v
		}
	}
	return nil
}

func (m *triggerPollerDaemon) AddLabel(_ context.Context, _, _ string) error {
	return nil
}

func (m *triggerPollerDaemon) UpdateBeadFieldsTyped(_ context.Context, beadID string, fields map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updateCalls++
	if b, ok := m.beads[beadID]; ok {
		if b.Fields == nil {
			b.Fields = make(map[string]string)
		}
		for k, v := range fields {
			b.Fields[k] = fmt.Sprintf("%v", v)
		}
	}
	return nil
}

func (m *triggerPollerDaemon) CloseBead(_ context.Context, beadID string, _ map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closeCalls++
	if b, ok := m.beads[beadID]; ok {
		b.Status = "closed"
	}
	return nil
}

func (m *triggerPollerDaemon) UpdateBead(_ context.Context, beadID string, req beadsapi.UpdateBeadRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.beads[beadID]; ok {
		if req.Status != nil {
			b.Status = *req.Status
		}
		if req.Assignee != nil {
			b.Assignee = *req.Assignee
		}
	}
	return nil
}

func (m *triggerPollerDaemon) SpawnAgent(_ context.Context, _, _, _, _, _ string, _ ...map[string]string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.spawnCalls++
	m.nextID++
	return fmt.Sprintf("agent-tp-%d", m.nextID), nil
}

func (m *triggerPollerDaemon) ListProjectBeads(_ context.Context) (map[string]beadsapi.ProjectInfo, error) {
	return map[string]beadsapi.ProjectInfo{}, nil
}

func (m *triggerPollerDaemon) MRBeadForTask(_ context.Context, _ string) (*beadsapi.BeadDetail, error) {
	return nil, nil
}

func (m *triggerPollerDaemon) GetBead(_ context.Context, beadID string) (*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.beads[beadID]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("bead %s not found", beadID)
}

// --- Poller ↔ Trigger Integration Tests ---

func TestSetTriggerEvaluator_SetsField(t *testing.T) {
	daemon := newTriggerPollerDaemon()
	p := NewJiraPoller(nil, daemon, JiraPollerConfig{
		Projects: []string{"PE"},
		Logger:   discardLogger(),
	})

	if p.triggers != nil {
		t.Fatal("expected triggers to be nil initially")
	}

	eval := NewJiraTriggerEvaluator(JiraTriggerConfig{
		Logger: discardLogger(),
	})
	p.SetTriggerEvaluator(eval)

	if p.triggers != eval {
		t.Error("expected SetTriggerEvaluator to set the triggers field")
	}
}

func TestSetTriggerEvaluator_OverwritesPrevious(t *testing.T) {
	daemon := newTriggerPollerDaemon()
	p := NewJiraPoller(nil, daemon, JiraPollerConfig{
		Projects: []string{"PE"},
		Logger:   discardLogger(),
	})

	eval1 := NewJiraTriggerEvaluator(JiraTriggerConfig{Logger: discardLogger()})
	eval2 := NewJiraTriggerEvaluator(JiraTriggerConfig{Logger: discardLogger()})

	p.SetTriggerEvaluator(eval1)
	p.SetTriggerEvaluator(eval2)

	if p.triggers != eval2 {
		t.Error("expected second evaluator to overwrite first")
	}
}

func TestPollerPollNilTriggers_DoesNotPanic(t *testing.T) {
	// Verify the nil guard in poll(): if p.triggers != nil { ... }
	daemon := newTriggerPollerDaemon()

	// Create a JIRA server that returns empty results.
	jiraServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issues": []any{}, "total": 0, "maxResults": 50, "startAt": 0,
		})
	}))
	defer jiraServer.Close()

	p := NewJiraPoller(newTestJiraClient(jiraServer.URL), daemon, JiraPollerConfig{
		Projects: []string{"PE"},
		Logger:   discardLogger(),
	})

	// triggers is nil by default — poll should not panic.
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("poll() panicked with nil triggers: %v", r)
			}
		}()
		p.poll(context.Background())
	}()
}

func TestPollerPoll_CallsEvaluateWhenTriggersSet(t *testing.T) {
	daemon := newTriggerPollerDaemon()

	// Add a trigger bead that the evaluator will find.
	daemon.beads["trig-1"] = &beadsapi.BeadDetail{
		ID: "trig-1", Title: "Test Trigger", Type: "trigger", Status: "open",
		Labels: []string{"trigger:jira"},
		Fields: map[string]string{
			"jql": "project = TESTPOLL", "action": "notify",
			"enabled": "true", "project": "test",
		},
	}

	// JIRA server that returns one issue for both poller JQL and trigger JQL.
	var jiraSearchCount atomic.Int32
	jiraServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jiraSearchCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		// Return an issue that matches the trigger JQL.
		issues := []JiraIssue{{Key: "TESTPOLL-1", Fields: JiraIssueFields{Summary: "Test issue"}}}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issues": issues, "total": 1, "maxResults": 50, "startAt": 0,
		})
	}))
	defer jiraServer.Close()

	jiraClient := newTestJiraClient(jiraServer.URL)

	var notifyCount atomic.Int32
	eval := NewJiraTriggerEvaluator(JiraTriggerConfig{
		Jira:   jiraClient,
		Daemon: daemon,
		Notifier: func(_ context.Context, _, _ string, _ string) {
			notifyCount.Add(1)
		},
		Logger: discardLogger(),
	})

	p := NewJiraPoller(jiraClient, daemon, JiraPollerConfig{
		Projects: []string{"PE"},
		Logger:   discardLogger(),
	})
	p.SetTriggerEvaluator(eval)

	// Run poll — this calls triggers.Evaluate(ctx) at the end.
	p.poll(context.Background())

	// The evaluator should have found trig-1, searched JIRA, and fired notify.
	if notifyCount.Load() != 1 {
		t.Errorf("expected 1 notify call from trigger, got %d", notifyCount.Load())
	}

	// JIRA should have been called at least twice: once by poll, once by evaluator.
	if jiraSearchCount.Load() < 2 {
		t.Errorf("expected ≥2 JIRA search calls (poll + trigger), got %d", jiraSearchCount.Load())
	}
}

func TestPollerPoll_CallsEvaluateOnEachCycle(t *testing.T) {
	// Track how many times ListBeadsFiltered is called with type=trigger
	// (i.e., how many times loadTriggers runs inside Evaluate).
	var triggerLoadCalls atomic.Int32
	daemon := &countingTriggerPollerDaemon{
		triggerPollerDaemon: newTriggerPollerDaemon(),
		onListFiltered: func(q beadsapi.ListBeadsQuery) {
			for _, tp := range q.Types {
				if tp == "trigger" {
					triggerLoadCalls.Add(1)
				}
			}
		},
	}

	daemon.beads["trig-1"] = &beadsapi.BeadDetail{
		ID: "trig-1", Title: "Repeating", Type: "trigger", Status: "open",
		Labels: []string{"trigger:jira"},
		Fields: map[string]string{
			"jql": "project = PE", "action": "notify",
			"enabled": "true",
		},
	}

	jiraServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issues": []any{}, "total": 0, "maxResults": 50, "startAt": 0,
		})
	}))
	defer jiraServer.Close()

	jiraClient := newTestJiraClient(jiraServer.URL)

	eval := NewJiraTriggerEvaluator(JiraTriggerConfig{
		Jira:   jiraClient,
		Daemon: daemon,
		Logger: discardLogger(),
	})

	p := NewJiraPoller(jiraClient, daemon, JiraPollerConfig{
		Projects: []string{"PE"},
		Logger:   discardLogger(),
	})
	p.SetTriggerEvaluator(eval)

	// Run poll three times.
	p.poll(context.Background())
	p.poll(context.Background())
	p.poll(context.Background())

	// loadTriggers (type=trigger query) should have been called on each poll.
	if got := triggerLoadCalls.Load(); got != 3 {
		t.Errorf("expected 3 trigger load calls (one per poll), got %d", got)
	}
}

// countingTriggerPollerDaemon wraps triggerPollerDaemon and intercepts ListBeadsFiltered.
type countingTriggerPollerDaemon struct {
	*triggerPollerDaemon
	onListFiltered func(q beadsapi.ListBeadsQuery)
}

func (c *countingTriggerPollerDaemon) ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if c.onListFiltered != nil {
		c.onListFiltered(q)
	}
	return c.triggerPollerDaemon.ListBeadsFiltered(ctx, q)
}

func TestPollerRun_CallsTriggerCatchUpOnStartup(t *testing.T) {
	daemon := newTriggerPollerDaemon()

	// Add trigger bead and an existing task bead tagged by this trigger.
	daemon.beads["trig-1"] = &beadsapi.BeadDetail{
		ID: "trig-1", Title: "Startup Trigger", Type: "trigger", Status: "open",
		Labels: []string{"trigger:jira"},
		Fields: map[string]string{
			"jql": "project = PE", "action": "notify",
			"enabled": "true",
		},
	}
	daemon.beads["existing-1"] = &beadsapi.BeadDetail{
		ID: "existing-1", Title: "[PE-50] Old issue", Type: "task", Kind: "issue",
		Status: "closed",
		Labels: []string{"trigger:trig-1", "jira:PE-50"},
		Fields: map[string]string{"jira_key": "PE-50"},
	}

	// JIRA server returns PE-50 (already seen) — should NOT trigger.
	jiraServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		issues := []JiraIssue{{Key: "PE-50", Fields: JiraIssueFields{Summary: "Old issue"}}}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issues": issues, "total": 1, "maxResults": 50, "startAt": 0,
		})
	}))
	defer jiraServer.Close()

	jiraClient := newTestJiraClient(jiraServer.URL)

	var notifyCount atomic.Int32
	eval := NewJiraTriggerEvaluator(JiraTriggerConfig{
		Jira:   jiraClient,
		Daemon: daemon,
		Notifier: func(_ context.Context, _, _ string, _ string) {
			notifyCount.Add(1)
		},
		Logger: discardLogger(),
	})

	p := NewJiraPoller(jiraClient, daemon, JiraPollerConfig{
		Projects:     []string{"PE"},
		PollInterval: 10 * time.Millisecond,
		Logger:       discardLogger(),
	})
	p.SetTriggerEvaluator(eval)

	// Run with a short-lived context. Run does:
	// 1. p.CatchUp(ctx)
	// 2. p.triggers.CatchUp(ctx) — this should populate seen set with PE-50
	// 3. p.fullScan(ctx) — creates poller beads
	// 4. poll loop
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = p.Run(ctx)

	// Verify CatchUp populated the seen set: PE-50 should NOT have triggered a notification.
	if notifyCount.Load() != 0 {
		t.Errorf("expected 0 notifications (PE-50 was in CatchUp seen set), got %d", notifyCount.Load())
	}

	// Verify the seen set was populated.
	eval.mu.Lock()
	seen := eval.seen["trig-1"]
	eval.mu.Unlock()
	if !seen["PE-50"] {
		t.Error("expected PE-50 in seen set after CatchUp via Run")
	}
}

func TestPollerRunNilTriggers_CatchUpSkipped(t *testing.T) {
	daemon := newTriggerPollerDaemon()

	jiraServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issues": []any{}, "total": 0, "maxResults": 50, "startAt": 0,
		})
	}))
	defer jiraServer.Close()

	p := NewJiraPoller(newTestJiraClient(jiraServer.URL), daemon, JiraPollerConfig{
		Projects:     []string{"PE"},
		PollInterval: 10 * time.Millisecond,
		Logger:       discardLogger(),
	})
	// triggers is nil — Run should not panic at the CatchUp guard.

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Run() panicked with nil triggers: %v", r)
			}
		}()
		_ = p.Run(ctx)
	}()
}

func TestPollerPoll_EvaluatorErrorDoesNotCrashPoller(t *testing.T) {
	daemon := newTriggerPollerDaemon()

	// Trigger evaluator with a daemon that errors on loadTriggers.
	// The evaluator uses its own daemon (TriggerBeadClient), separate from
	// the poller daemon. We give it a failing one.
	errDaemon := &failingTriggerDaemon{}
	eval := NewJiraTriggerEvaluator(JiraTriggerConfig{
		Daemon: errDaemon,
		Logger: discardLogger(),
	})

	jiraServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issues": []any{}, "total": 0, "maxResults": 50, "startAt": 0,
		})
	}))
	defer jiraServer.Close()

	p := NewJiraPoller(newTestJiraClient(jiraServer.URL), daemon, JiraPollerConfig{
		Projects: []string{"PE"},
		Logger:   discardLogger(),
	})
	p.SetTriggerEvaluator(eval)

	// poll should not panic even if the evaluator's daemon fails.
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("poll() panicked when evaluator errored: %v", r)
			}
		}()
		p.poll(context.Background())
	}()
}

// failingTriggerDaemon implements TriggerBeadClient but always returns errors.
type failingTriggerDaemon struct{}

func (d *failingTriggerDaemon) ListBeadsFiltered(_ context.Context, _ beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	return nil, fmt.Errorf("simulated daemon failure")
}
func (d *failingTriggerDaemon) CreateBead(_ context.Context, _ beadsapi.CreateBeadRequest) (string, error) {
	return "", fmt.Errorf("simulated daemon failure")
}
func (d *failingTriggerDaemon) UpdateBeadFields(_ context.Context, _ string, _ map[string]string) error {
	return fmt.Errorf("simulated daemon failure")
}
func (d *failingTriggerDaemon) UpdateBeadFieldsTyped(_ context.Context, _ string, _ map[string]any) error {
	return fmt.Errorf("simulated daemon failure")
}
func (d *failingTriggerDaemon) SpawnAgent(_ context.Context, _, _, _, _, _ string, _ ...map[string]string) (string, error) {
	return "", fmt.Errorf("simulated daemon failure")
}
func (d *failingTriggerDaemon) ListProjectBeads(_ context.Context) (map[string]beadsapi.ProjectInfo, error) {
	return nil, fmt.Errorf("simulated daemon failure")
}
func (d *failingTriggerDaemon) GetBead(_ context.Context, _ string) (*beadsapi.BeadDetail, error) {
	return nil, fmt.Errorf("simulated daemon failure")
}
func (d *failingTriggerDaemon) AddDependency(_ context.Context, _, _, _, _ string) error {
	return fmt.Errorf("simulated daemon failure")
}
func (d *failingTriggerDaemon) AddComment(_ context.Context, _, _, _ string) error {
	return fmt.Errorf("simulated daemon failure")
}

func (d *failingTriggerDaemon) AddLabel(_ context.Context, _, _ string) error {
	return nil
}
func (d *failingTriggerDaemon) CloseBead(_ context.Context, _ string, _ map[string]string) error {
	return fmt.Errorf("simulated daemon failure")
}
func (d *failingTriggerDaemon) UpdateBead(_ context.Context, _ string, _ beadsapi.UpdateBeadRequest) error {
	return fmt.Errorf("simulated daemon failure")
}
func (d *failingTriggerDaemon) MRBeadForTask(_ context.Context, _ string) (*beadsapi.BeadDetail, error) {
	return nil, fmt.Errorf("simulated daemon failure")
}

// pouring-stall dependency-graph stubs (ka-0000000957) — this mock does not
// exercise the sweep.
func (m *triggerPollerDaemon) GetReverseDependencies(_ context.Context, _ string) ([]beadsapi.Dependency, error) {
	return nil, nil
}
func (m *triggerPollerDaemon) BatchGetDependencies(_ context.Context, _ []string, _ string) (map[string][]beadsapi.Dependency, error) {
	return nil, nil
}
func (m *triggerPollerDaemon) BatchGetBeads(_ context.Context, _ []string) (map[string]*beadsapi.BeadDetail, error) {
	return nil, nil
}

// pouring-stall dependency-graph stubs (ka-0000000957) — this mock does not
// exercise the sweep.
func (d *failingTriggerDaemon) GetReverseDependencies(_ context.Context, _ string) ([]beadsapi.Dependency, error) {
	return nil, nil
}
func (d *failingTriggerDaemon) BatchGetDependencies(_ context.Context, _ []string, _ string) (map[string][]beadsapi.Dependency, error) {
	return nil, nil
}
func (d *failingTriggerDaemon) BatchGetBeads(_ context.Context, _ []string) (map[string]*beadsapi.BeadDetail, error) {
	return nil, nil
}
