package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// mockEscalationBeadClient extends the mock to support the full BeadClient
// methods needed by the Escalations watcher.
type mockEscalationBeadClient struct {
	mu                 sync.Mutex
	beads              map[string]*beadsapi.BeadDetail
	spawnedAgents      []string
	spawnedAgentFields []map[string]string
	updatedFields      map[string]map[string]string
	closedBeads        map[string]map[string]string
	dependencies       []mockDep
	comments           []mockComment
	labelCalls         []mockLabelCall
	updateBeadRequests map[string]beadsapi.UpdateBeadRequest
	listQueries        []beadsapi.ListBeadsQuery
	spawnErr           error
	spawnedAgentOps    []string // the X-Operated-By each SpawnAgent carried (beadsapi.WithOperator), "" for none
	resultCalls        map[string]beadsapi.CaptureEscalationResultRequest
	resultErr          error
	created            []beadsapi.CreateBeadRequest
}

type mockDep struct {
	beadID, dependsOnID, depType string
}

type mockLabelCall struct {
	beadID, label string
}

type mockComment struct {
	beadID, author, text string
}

func newMockEscalationBeadClient() *mockEscalationBeadClient {
	return &mockEscalationBeadClient{
		beads:         make(map[string]*beadsapi.BeadDetail),
		updatedFields: make(map[string]map[string]string),
		closedBeads:   make(map[string]map[string]string),
	}
}

func (m *mockEscalationBeadClient) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.beads[id]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("not found: %s", id)
}

func (m *mockEscalationBeadClient) FindAgentBead(_ context.Context, name string) (*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, b := range m.beads {
		if b.Fields != nil && b.Fields["agent"] == name {
			return b, nil
		}
	}
	return nil, fmt.Errorf("agent not found: %s", name)
}

func (m *mockEscalationBeadClient) RevokeRunner(context.Context, string) (int, int, error) {
	return 0, 0, nil
}

func (m *mockEscalationBeadClient) FindAgentBeadAnyStatus(_ context.Context, name string) (*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, b := range m.beads {
		if b.Fields != nil && b.Fields["agent"] == name {
			return b, nil
		}
	}
	return nil, fmt.Errorf("agent not found: %s", name)
}

func (m *mockEscalationBeadClient) CloseBead(_ context.Context, id string, fields map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closedBeads[id] = fields
	return nil
}

// CaptureEscalationResult models POST /v1/escalations/{id}/result — the
// dedicated result verb recordResult must route through instead of the generic
// CloseBead (ka-0000006558). It records into resultCalls (status + result) so a
// test can assert a failure result actually terminated the escalation.
func (m *mockEscalationBeadClient) CaptureEscalationResult(_ context.Context, id string, req beadsapi.CaptureEscalationResultRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.resultCalls == nil {
		m.resultCalls = make(map[string]beadsapi.CaptureEscalationResultRequest)
	}
	m.resultCalls[id] = req
	if m.resultErr != nil {
		return m.resultErr
	}
	return nil
}

func (m *mockEscalationBeadClient) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.created = append(m.created, req)
	return "gd-new1", nil
}

func (m *mockEscalationBeadClient) SpawnAgent(ctx context.Context, name, project, taskID, role, prompt string, extra ...map[string]string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.spawnErr != nil {
		return "", m.spawnErr
	}
	m.spawnedAgents = append(m.spawnedAgents, name)
	m.spawnedAgentOps = append(m.spawnedAgentOps, beadsapi.OperatorFromContext(ctx))
	fields := map[string]string{"project": project, "task_id": taskID, "role": role, "prompt": prompt}
	for _, e := range extra {
		for k, v := range e {
			fields[k] = v
		}
	}
	m.spawnedAgentFields = append(m.spawnedAgentFields, fields)
	return "gd-agent1", nil
}

func (m *mockEscalationBeadClient) ListDecisionBeads(_ context.Context) ([]*beadsapi.BeadDetail, error) {
	return nil, nil
}

func (m *mockEscalationBeadClient) ListAgentBeads(_ context.Context) ([]beadsapi.AgentBead, error) {
	return nil, nil
}

func (m *mockEscalationBeadClient) ListAccountBeads(_ context.Context) (map[string]beadsapi.AccountInfo, error) {
	return nil, nil
}

func (m *mockEscalationBeadClient) ListProjectBeads(_ context.Context) (map[string]beadsapi.ProjectInfo, error) {
	return nil, nil
}

func (m *mockEscalationBeadClient) ListAssignedTask(_ context.Context, _ string) (*beadsapi.BeadDetail, error) {
	return nil, nil
}

// ListBeadsFiltered honors Limit/Offset so callers that page to exhaustion are
// exercised against a real page boundary rather than a mock that hands back the
// whole set on every request (ka-0000005723). Results are sorted by ID for a
// stable page sequence — map iteration order would otherwise let a bead repeat
// on one page and vanish from another.
func (m *mockEscalationBeadClient) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listQueries = append(m.listQueries, q)
	var results []*beadsapi.BeadDetail
	for _, b := range m.beads {
		for _, t := range q.Types {
			if b.Type == t {
				results = append(results, b)
			}
		}
	}
	sort.Slice(results, func(i, j int) bool { return results[i].ID < results[j].ID })

	total := len(results)
	if q.Offset > 0 {
		if q.Offset >= len(results) {
			results = nil
		} else {
			results = results[q.Offset:]
		}
	}
	if q.Limit > 0 && len(results) > q.Limit {
		results = results[:q.Limit]
	}
	return &beadsapi.ListBeadsResult{Beads: results, Total: total}, nil
}

func (m *mockEscalationBeadClient) AddDependency(_ context.Context, beadID, dependsOnID, depType, createdBy string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dependencies = append(m.dependencies, mockDep{beadID, dependsOnID, depType})
	return nil
}

// AddLabel records each call (beadID, label) so tests that assert on label
// side effects (e.g. the SLA nudge sweep's "escalated"/"overdue" labels) can
// inspect labelCalls; older tests that never assert on labels are unaffected.
func (m *mockEscalationBeadClient) AddLabel(_ context.Context, beadID, label string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.labelCalls = append(m.labelCalls, mockLabelCall{beadID, label})
	return nil
}

func (m *mockEscalationBeadClient) GetMentionedBy(_ context.Context, _ string, _ int) ([]beadsapi.BeadReferenceSummary, error) {
	return nil, nil
}

func (m *mockEscalationBeadClient) GetReverseDependencies(_ context.Context, _ string) ([]beadsapi.Dependency, error) {
	return nil, nil
}

func (m *mockEscalationBeadClient) ResolveTicket(_ context.Context, _ string) (*beadsapi.BeadDetail, error) {
	return nil, nil
}

func (m *mockEscalationBeadClient) UpdateBead(_ context.Context, id string, req beadsapi.UpdateBeadRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.updateBeadRequests == nil {
		m.updateBeadRequests = make(map[string]beadsapi.UpdateBeadRequest)
	}
	m.updateBeadRequests[id] = req
	if len(req.Fields) > 0 {
		stringified := make(map[string]string, len(req.Fields))
		for k, v := range req.Fields {
			if v == nil {
				continue
			}
			stringified[k] = fmt.Sprintf("%v", v)
		}
		m.updatedFields[id] = stringified
	}
	return nil
}

func (m *mockEscalationBeadClient) UpdateBeadFields(_ context.Context, id string, fields map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updatedFields[id] = fields
	return nil
}

func (m *mockEscalationBeadClient) UpdateBeadFieldsTyped(_ context.Context, id string, fields map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	stringified := make(map[string]string, len(fields))
	for k, v := range fields {
		stringified[k] = fmt.Sprintf("%v", v)
	}
	m.updatedFields[id] = stringified
	return nil
}

func (m *mockEscalationBeadClient) AddComment(_ context.Context, beadID, author, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.comments = append(m.comments, mockComment{beadID, author, text})
	return nil
}

func (m *mockEscalationBeadClient) GetComments(_ context.Context, _ string) ([]beadsapi.Comment, error) {
	return nil, nil
}

func (m *mockEscalationBeadClient) ListSlackOperatorAliases(_ context.Context) (*beadsapi.ListSlackOperatorAliasesResponse, error) {
	return &beadsapi.ListSlackOperatorAliasesResponse{}, nil
}

func (m *mockEscalationBeadClient) ResyncOperatorIdentity(_ context.Context, _ int64) (*beadsapi.UpdateOperatorIdentityResponse, error) {
	return &beadsapi.UpdateOperatorIdentityResponse{Updated: true}, nil
}

func (m *mockEscalationBeadClient) ResyncUserBeadAliases(_ context.Context, _ string) (*beadsapi.LinkOperatorAliasesResponse, error) {
	return &beadsapi.LinkOperatorAliasesResponse{}, nil
}

func TestEscalations_HandleCreated_TracksRequestingAgent(t *testing.T) {
	client := newMockEscalationBeadClient()
	esc := NewEscalations(EscalationsConfig{
		Daemon: client,
		Logger: slog.Default(),
	})

	event := BeadEvent{
		ID:   "gd-esc1",
		Type: "escalation",
		Fields: map[string]string{
			"operation_type":   "aws_iam",
			"requesting_agent": "gd-agent-abc",
		},
	}
	data, _ := json.Marshal(map[string]any{"bead": event})

	esc.handleCreated(context.Background(), data)

	esc.pendingMu.Lock()
	agent := esc.pending["gd-esc1"]
	esc.pendingMu.Unlock()

	if agent != "gd-agent-abc" {
		t.Errorf("pending agent=%q, want gd-agent-abc", agent)
	}
}

func TestEscalations_HandleUpdated_SkipsWithoutApproval(t *testing.T) {
	client := newMockEscalationBeadClient()
	esc := NewEscalations(EscalationsConfig{
		Daemon: client,
		Logger: slog.Default(),
	})

	event := BeadEvent{
		ID:   "gd-esc3",
		Type: "escalation",
		Fields: map[string]string{
			"operation_type": "aws_iam",
			// No approved_by field.
		},
	}
	data, _ := json.Marshal(map[string]any{"bead": event})

	esc.handleUpdated(context.Background(), data)

	client.mu.Lock()
	defer client.mu.Unlock()

	if len(client.spawnedAgents) != 0 {
		t.Errorf("should not have spawned any agents without approval")
	}
}

// TestHandleUpdated_ApprovalDoesNotSpawn (P7 Task 12, spec D10): the daemon's
// approve handler claims the one executor; the bridge executes nothing. For
// every legacy execution_target an approval spawns no agent, nudges no one,
// writes no field, and updates the Slack card exactly once, however many
// times the approval is redelivered. gd-mQrOElVxhd ran twice (server replay
// and bridge spawn) before this.
func TestHandleUpdated_ApprovalDoesNotSpawn(t *testing.T) {
	for _, target := range []string{"local", "service_account", ""} {
		t.Run("target="+target, func(t *testing.T) {
			client := newMockEscalationBeadClient()
			notifier := &mockEscalationNotifier{}
			esc := NewEscalations(EscalationsConfig{Daemon: client, EscalationNotifier: notifier, Logger: slog.Default()})
			var nudged []string
			esc.nudge = func(_ context.Context, agent, _ string) error {
				nudged = append(nudged, agent)
				return nil
			}
			event := BeadEvent{
				ID: "gd-esc-ns", Type: "escalation", Labels: []string{"project:gardenio"},
				Fields: map[string]string{
					"operation_type":     "k8s_rbac",
					"operation_spec":     `{"action":"create_role_binding"}`,
					"execution_script":   "kubectl apply -f rb.yaml",
					"approved_by":        "oidc:owner@example.com",
					"execution_target":   target,
					"execution_dispatch": target,
					"requesting_agent":   "gd-agent-xyz",
				},
			}
			esc.handleUpdated(context.Background(), approvalEvent(t, event))
			esc.handleUpdated(context.Background(), approvalEvent(t, event))

			client.mu.Lock()
			defer client.mu.Unlock()
			if len(client.spawnedAgents) != 0 {
				t.Errorf("approval spawned %v, want none", client.spawnedAgents)
			}
			if len(client.updatedFields) != 0 {
				t.Errorf("approval wrote fields %v, want none", client.updatedFields)
			}
			if len(nudged) != 0 {
				t.Errorf("approval nudged %v, want none", nudged)
			}
			notifier.mu.Lock()
			defer notifier.mu.Unlock()
			if len(notifier.approvedCalls) != 1 {
				t.Errorf("Slack approval updates = %v, want one", notifier.approvedCalls)
			}
		})
	}
}

// TestEscalations_HandleUpdated_ApprovalTransitionNotifies covers each shape of
// the approval event: the approve endpoint on an open escalation (approved_by +
// status), on one already in_progress (approved_by only), and an older daemon
// that published the status flip alone. Each updates the Slack card.
func TestEscalations_HandleUpdated_ApprovalTransitionNotifies(t *testing.T) {
	cases := map[string]map[string]any{
		"approved_by and status": {"approved_by": "gd-user1", "status": "in_progress"},
		"approved_by only":       {"approved_by": "gd-user1"},
		"status only":            {"status": "in_progress"},
	}
	for name, changes := range cases {
		t.Run(name, func(t *testing.T) {
			client := newMockEscalationBeadClient()
			notifier := &mockEscalationNotifier{}
			esc := NewEscalations(EscalationsConfig{Daemon: client, EscalationNotifier: notifier, Logger: slog.Default()})
			event := BeadEvent{
				ID:     "gd-esct",
				Type:   "escalation",
				Labels: []string{"project:gardenio"},
				Fields: map[string]string{
					"operation_type":   "aws_iam",
					"approved_by":      "gd-user1",
					"execution_target": "local",
				},
			}

			esc.handleUpdated(context.Background(), updateEvent(t, event, changes))

			notifier.mu.Lock()
			defer notifier.mu.Unlock()
			if len(notifier.approvedCalls) != 1 {
				t.Fatalf("Slack approval updates = %v, want one", notifier.approvedCalls)
			}
		})
	}
}

func approvalEvent(t *testing.T, bead BeadEvent) []byte {
	t.Helper()
	return updateEvent(t, bead, map[string]any{"approved_by": bead.Fields["approved_by"], "status": "in_progress"})
}

func updateEvent(t *testing.T, bead BeadEvent, changes map[string]any) []byte {
	t.Helper()
	w := map[string]any{"bead": bead}
	if changes != nil {
		w["changes"] = changes
	}
	data, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestEscalations_HandleUpdated_LaterUpdateOfApprovedDoesNotSpawn is the
// gd-TMYvBxNY6h regression: a short_title groomer pass over approved
// escalations woke an executor for each, one on an owner-held merge. A fresh
// Escalations (a restarted bridge, empty triggered map) sees cosmetic and field
// updates of an approved escalation and must spawn nothing for any of them.
func TestEscalations_HandleUpdated_LaterUpdateOfApprovedDoesNotSpawn(t *testing.T) {
	approved := BeadEvent{
		ID:     "gd-seCXUzmKFv",
		Type:   "escalation",
		Status: "in_progress",
		Labels: []string{"project:gardenio"},
		Fields: map[string]string{
			"operation_type":   "gitlab_mr_merge",
			"approved_by":      "oidc:owner@example.com",
			"execution_target": "local",
			"requesting_agent": "gd-agent-xyz",
		},
	}
	cases := map[string]map[string]any{
		"short_title": {"short_title": "merge and roll PR 504"},
		"labels":      {"labels": []string{"project:gardenio", "hold"}},
		"fields":      {"fields": map[string]any{"approved_by": "oidc:owner@example.com", "note": "x"}, "note": "x"},
		"priority":    {"priority": 1},
		"no changes":  nil,
	}
	for name, changes := range cases {
		t.Run(name, func(t *testing.T) {
			client := newMockEscalationBeadClient()
			notifier := &mockEscalationNotifier{}
			esc := NewEscalations(EscalationsConfig{
				Daemon:             client,
				EscalationNotifier: notifier,
				Logger:             slog.Default(),
			})

			esc.handleUpdated(context.Background(), updateEvent(t, approved, changes))

			client.mu.Lock()
			defer client.mu.Unlock()
			if len(client.spawnedAgents) != 0 {
				t.Errorf("a %s update of an approved escalation spawned %v, want none", name, client.spawnedAgents)
			}
			if f := client.updatedFields[approved.ID]; f != nil && f["executor"] != "" {
				t.Errorf("executor stamped on a %s update: %q", name, f["executor"])
			}
		})
	}
}

func TestApprovalTransition(t *testing.T) {
	cases := []struct {
		name    string
		changes map[string]any
		want    bool
	}{
		{"nil", nil, false},
		{"short_title", map[string]any{"short_title": "x"}, false},
		{"status closed", map[string]any{"status": "closed"}, false},
		{"status open", map[string]any{"status": "open"}, false},
		{"empty approved_by", map[string]any{"approved_by": ""}, false},
		{"approved_by removed", map[string]any{"approved_by": nil}, false},
		{"approved_by", map[string]any{"approved_by": "oidc:a@b"}, true},
		{"status in_progress", map[string]any{"status": "in_progress"}, true},
	}
	for _, tc := range cases {
		if got := approvalTransition(&BeadEvent{Changes: tc.changes}); got != tc.want {
			t.Errorf("%s: approvalTransition=%v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestEscalations_HandleClosed_NudgesRequestingAgent(t *testing.T) {
	client := newMockEscalationBeadClient()
	// Add the requesting agent bead.
	client.beads["gd-agent-req"] = &beadsapi.BeadDetail{
		ID:     "gd-agent-req",
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{"agent": "test-agent"},
	}

	esc := NewEscalations(EscalationsConfig{
		Daemon: client,
		Logger: slog.Default(),
	})

	event := BeadEvent{
		ID:   "gd-esc5",
		Type: "escalation",
		Fields: map[string]string{
			"operation_type":   "aws_iam",
			"result_status":    "success",
			"result":           `{"output":"IAM role created"}`,
			"requesting_agent": "gd-agent-req",
		},
	}
	data, _ := json.Marshal(map[string]any{"bead": event})

	esc.handleClosed(context.Background(), data)

	var nudge *beadsapi.CreateBeadRequest
	for i := range client.created {
		if client.created[i].Type == "mail" && client.created[i].Assignee == "test-agent" {
			nudge = &client.created[i]
		}
	}
	if nudge == nil {
		t.Fatalf("requesting agent was not sent nudge mail; creates=%+v", client.created)
	}
	if !strings.Contains(nudge.Description, "completed") {
		t.Errorf("nudge message=%q, should contain 'completed'", nudge.Description)
	}
}

// TestEscalations_HandleClosed_RespawnsDeadRequestingAgent is the regression
// test for gd-t0gm8q62Ac: a resolved (success) escalation whose requesting
// agent has already gone terminal must not just log a failed nudge and drop
// the verdict — it must dispatch a follow-through crew onto the requesting
// task so the mechanical follow-through (poll CI, merge, close task) still
// happens.
func TestEscalations_HandleClosed_RespawnsDeadRequestingAgent(t *testing.T) {
	client := newMockEscalationBeadClient()
	// The requesting agent bead is TERMINAL (agent_state=done) — NudgeAgent's
	// deadAgentStates check fails it before ever reaching a coop_url, so the
	// nudge always errors here regardless of Notes.
	client.beads["gd-agent-req"] = &beadsapi.BeadDetail{
		ID:     "gd-agent-req",
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{
			"agent":       "test-agent",
			"agent_state": "done",
			"project":     "gardenio",
			"task_id":     "gd-task-1",
			"role":        "crew",
		},
	}

	esc := NewEscalations(EscalationsConfig{
		Daemon: client,
		Logger: slog.Default(),
	})

	event := BeadEvent{
		ID:   "gd-esc-dead",
		Type: "escalation",
		Fields: map[string]string{
			"operation_type":   "github_pr_merge",
			"result_status":    "success",
			"result":           `{"output":"merge on green"}`,
			"requesting_agent": "gd-agent-req",
			"requesting_task":  "gd-task-1",
		},
	}
	data, _ := json.Marshal(map[string]any{"bead": event})

	esc.handleClosed(context.Background(), data)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.spawnedAgents) != 1 {
		t.Fatalf("spawned %d agents, want 1 follow-through crew", len(client.spawnedAgents))
	}
	fields := client.spawnedAgentFields[0]
	if fields["task_id"] != "gd-task-1" {
		t.Errorf("task_id = %q, want gd-task-1", fields["task_id"])
	}
	if fields["escalation_bead_id"] != "gd-esc-dead" {
		t.Errorf("escalation_bead_id = %q, want gd-esc-dead", fields["escalation_bead_id"])
	}
	if fields["spawn_source"] != "escalation-resume" {
		t.Errorf("spawn_source = %q, want escalation-resume", fields["spawn_source"])
	}
	// The bridge resolves the requester by the bead id in requesting_agent and
	// names that same id as spawned_by; the daemon's continuation gate matches
	// the escalation's server-stamped requesting_agent against it.
	if fields["spawned_by"] != "gd-agent-req" {
		t.Errorf("spawned_by = %q, want the requesting agent's bead id gd-agent-req", fields["spawned_by"])
	}
	if fields[beadsapi.FieldWakeReason] != beadsapi.WakeReasonEscalation {
		t.Errorf("wake_reason = %q, want %q", fields[beadsapi.FieldWakeReason], beadsapi.WakeReasonEscalation)
	}
	if !strings.Contains(fields["prompt"], "gd-esc-dead") {
		t.Errorf("prompt does not reference the resolved escalation: %q", fields["prompt"])
	}
}

// TestEscalations_HandleClosed_NoRespawnOnFailureResult confirms the
// coordinator's interim rule: a failure result has no follow-through to
// resume (the requester, if alive, would just re-escalate), so a dead
// requesting agent must NOT trigger a respawn for a failed escalation.
func TestEscalations_HandleClosed_NoRespawnOnFailureResult(t *testing.T) {
	client := newMockEscalationBeadClient()
	client.beads["gd-agent-req"] = &beadsapi.BeadDetail{
		ID:   "gd-agent-req",
		Type: "agent",
		Fields: map[string]string{
			"agent":       "test-agent",
			"agent_state": "done",
			"project":     "gardenio",
			"task_id":     "gd-task-1",
		},
	}

	esc := NewEscalations(EscalationsConfig{
		Daemon: client,
		Logger: slog.Default(),
	})

	event := BeadEvent{
		ID:   "gd-esc-fail",
		Type: "escalation",
		Fields: map[string]string{
			"operation_type":   "github_pr_merge",
			"result_status":    "failure",
			"requesting_agent": "gd-agent-req",
			"requesting_task":  "gd-task-1",
		},
	}
	data, _ := json.Marshal(map[string]any{"bead": event})

	esc.handleClosed(context.Background(), data)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.spawnedAgents) != 0 {
		t.Errorf("spawned %d agents for a failure result, want 0", len(client.spawnedAgents))
	}
}

// TestEscalations_HandleClosed_NoRespawnForE2EFixture mirrors
// TestDecisions_HandleClosed_NoRespawnForE2EFixture: a resolved escalation
// created by an e2e test fixture must never mint a real pod (ka-0000000809).
func TestEscalations_HandleClosed_NoRespawnForE2EFixture(t *testing.T) {
	client := newMockEscalationBeadClient()
	client.beads["gd-agent-req"] = &beadsapi.BeadDetail{
		ID:   "gd-agent-req",
		Type: "agent",
		Fields: map[string]string{
			"agent":       "test-agent",
			"agent_state": "done",
			"project":     "gardenio",
			"task_id":     "gd-task-1",
		},
	}

	esc := NewEscalations(EscalationsConfig{
		Daemon: client,
		Logger: slog.Default(),
	})

	event := BeadEvent{
		ID:        "gd-esc-e2e",
		Type:      "escalation",
		CreatedBy: "e2e-fixture-actor",
		Labels:    []string{"e2e:true"},
		Fields: map[string]string{
			"operation_type":   "github_pr_merge",
			"result_status":    "success",
			"requesting_agent": "gd-agent-req",
			"requesting_task":  "gd-task-1",
		},
	}
	data, _ := json.Marshal(map[string]any{"bead": event})

	esc.handleClosed(context.Background(), data)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.spawnedAgents) != 0 {
		t.Errorf("spawned %d agents for an e2e fixture escalation, want 0", len(client.spawnedAgents))
	}
}

// TestEscalations_HandleClosed_RespawnDedup confirms a burst of redelivered
// SSE close events for the same escalation mints at most one follow-through
// crew (mirrors TestDecisions_HandleClosed_RespawnDedup).
func TestEscalations_HandleClosed_RespawnDedup(t *testing.T) {
	client := newMockEscalationBeadClient()
	client.beads["gd-agent-req"] = &beadsapi.BeadDetail{
		ID:   "gd-agent-req",
		Type: "agent",
		Fields: map[string]string{
			"agent":       "test-agent",
			"agent_state": "done",
			"project":     "gardenio",
			"task_id":     "gd-task-1",
		},
	}

	esc := NewEscalations(EscalationsConfig{
		Daemon: client,
		Logger: slog.Default(),
	})

	event := BeadEvent{
		ID:   "gd-esc-dup",
		Type: "escalation",
		Fields: map[string]string{
			"operation_type":   "github_pr_merge",
			"result_status":    "success",
			"requesting_agent": "gd-agent-req",
			"requesting_task":  "gd-task-1",
		},
	}
	data, _ := json.Marshal(map[string]any{"bead": event})

	esc.handleClosed(context.Background(), data)
	esc.handleClosed(context.Background(), data)
	esc.handleClosed(context.Background(), data)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.spawnedAgents) != 1 {
		t.Errorf("spawned %d agents across 3 redelivered close events, want 1 (dedup)", len(client.spawnedAgents))
	}
}

// TestEscalations_HandleClosed_SparseApprovedBy_RendersResultNotRejected is a
// regression test for gd-vGDRQErPzD: the close SSE event is sparse (carries
// result_status=success but not approved_by, which is written at approval time).
// handleClosed must backfill approved_by from the full bead so the close routes
// to NotifyEscalationResult (green summary), NOT DismissEscalation ("Rejected").
func TestEscalations_HandleClosed_SparseApprovedBy_RendersResultNotRejected(t *testing.T) {
	client := newMockEscalationBeadClient()
	// The full bead carries approved_by (stamped at approval time) — the sparse
	// close event below does not.
	client.beads["gd-esc-sparse"] = &beadsapi.BeadDetail{
		ID:     "gd-esc-sparse",
		Type:   "escalation",
		Status: "closed",
		Fields: map[string]string{
			"result_status":     "success",
			"result":            `{"output":"terraform applied"}`,
			"requesting_agent":  "gd-agent-req",
			"requesting_task":   "gd-task-1",
			"operation_type":    "terraform_apply",
			"approved_by":       "gd-user1",
			"approved_by_email": "op@acme.example",
		},
	}

	notifier := &mockEscalationNotifier{}
	esc := NewEscalations(EscalationsConfig{
		Daemon:             client,
		EscalationNotifier: notifier,
		Logger:             slog.Default(),
	})

	// Sparse close event: result_status present but approved_by absent — this is
	// exactly the shape the escalation reaper's close-time write produces.
	event := BeadEvent{
		ID:   "gd-esc-sparse",
		Type: "escalation",
		Fields: map[string]string{
			"result_status": "success",
		},
	}
	data, _ := json.Marshal(map[string]any{"bead": event})

	esc.handleClosed(context.Background(), data)

	notifier.mu.Lock()
	defer notifier.mu.Unlock()
	if len(notifier.dismissCalls) != 0 {
		t.Errorf("approved+executed escalation was mislabeled Rejected: DismissEscalation called for %v", notifier.dismissCalls)
	}
	if len(notifier.resultCalls) != 1 || notifier.resultCalls[0] != "gd-esc-sparse" {
		t.Errorf("expected NotifyEscalationResult for gd-esc-sparse, got resultCalls=%v", notifier.resultCalls)
	}
}

// TestEscalations_HandleClosed_NeverApproved_RendersRejected confirms the
// genuine-rejection path still routes to DismissEscalation: an escalation closed
// without ever being approved (no approved_by anywhere) is a real rejection.
func TestEscalations_HandleClosed_NeverApproved_RendersRejected(t *testing.T) {
	client := newMockEscalationBeadClient()
	client.beads["gd-esc-rejected"] = &beadsapi.BeadDetail{
		ID:     "gd-esc-rejected",
		Type:   "escalation",
		Status: "closed",
		Fields: map[string]string{
			"result_status":    "rejected",
			"requesting_agent": "gd-agent-req",
			"operation_type":   "aws_iam",
			// No approved_by anywhere — genuinely rejected.
		},
	}

	notifier := &mockEscalationNotifier{}
	esc := NewEscalations(EscalationsConfig{
		Daemon:             client,
		EscalationNotifier: notifier,
		Logger:             slog.Default(),
	})

	event := BeadEvent{
		ID:   "gd-esc-rejected",
		Type: "escalation",
		Fields: map[string]string{
			"result_status": "rejected",
		},
	}
	data, _ := json.Marshal(map[string]any{"bead": event})

	esc.handleClosed(context.Background(), data)

	notifier.mu.Lock()
	defer notifier.mu.Unlock()
	if len(notifier.dismissCalls) != 1 || notifier.dismissCalls[0] != "gd-esc-rejected" {
		t.Errorf("expected DismissEscalation for genuinely-rejected escalation, got dismissCalls=%v", notifier.dismissCalls)
	}
	if len(notifier.resultCalls) != 0 {
		t.Errorf("rejected escalation should not post a result summary, got resultCalls=%v", notifier.resultCalls)
	}
}

// TestEscalations_HandleClosed_SuccessNeverApproved_RendersResultNotRejected
// is the exact gd-R4ZVtmripO repro (PR #183 merge, bug gd-VULjTsGZsA): the
// merge desk closed the escalation directly with result_status=success, with
// no approved_by stamped anywhere (never routed through the approval flow).
// handleClosed must still post the success summary, not the "Rejected"
// banner.
func TestEscalations_HandleClosed_SuccessNeverApproved_RendersResultNotRejected(t *testing.T) {
	client := newMockEscalationBeadClient()
	client.beads["gd-esc-mergedesk"] = &beadsapi.BeadDetail{
		ID:     "gd-esc-mergedesk",
		Type:   "escalation",
		Status: "closed",
		Fields: map[string]string{
			"result_status":    "success",
			"result":           `{"output":"PR #183 merged"}`,
			"requesting_agent": "gd-agent-req",
			"operation_type":   "github_pr_merge",
			// No approved_by anywhere — closed directly by the merge desk.
		},
	}

	notifier := &mockEscalationNotifier{}
	esc := NewEscalations(EscalationsConfig{
		Daemon:             client,
		EscalationNotifier: notifier,
		Logger:             slog.Default(),
	})

	event := BeadEvent{
		ID:   "gd-esc-mergedesk",
		Type: "escalation",
		Fields: map[string]string{
			"result_status": "success",
		},
	}
	data, _ := json.Marshal(map[string]any{"bead": event})

	esc.handleClosed(context.Background(), data)

	notifier.mu.Lock()
	defer notifier.mu.Unlock()
	if len(notifier.dismissCalls) != 0 {
		t.Errorf("successful never-approved escalation was mislabeled Rejected: DismissEscalation called for %v", notifier.dismissCalls)
	}
	if len(notifier.resultCalls) != 1 || notifier.resultCalls[0] != "gd-esc-mergedesk" {
		t.Errorf("expected NotifyEscalationResult for gd-esc-mergedesk, got resultCalls=%v", notifier.resultCalls)
	}
}

func TestTruncateResult(t *testing.T) {
	short := "hello"
	if got := truncateResult(short, 100); got != short {
		t.Errorf("truncateResult(%q, 100)=%q, want %q", short, got, short)
	}

	long := strings.Repeat("x", 200)
	got := truncateResult(long, 50)
	if len(got) != 50 {
		t.Errorf("truncateResult len=%d, want 50", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Error("truncated result should end with ...")
	}
}

func TestSummarizeCredentials(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"valid array", `[{"type":"aws_role","identifier":"arn:aws:iam::role/admin"},{"type":"k8s_context","identifier":"prod"}]`, "`aws_role`, `k8s_context`"},
		{"empty array", `[]`, ""},
		{"invalid JSON", `not-json`, ""},
		{"type only", `[{"type":"ssh_key"}]`, "`ssh_key`"},
		{"identifier fallback", `[{"identifier":"my-token"}]`, "`my-token`"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := summarizeCredentials(tt.input)
			if got != tt.want {
				t.Errorf("summarizeCredentials() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSensitivityEmoji(t *testing.T) {
	tests := []struct {
		level string
		want  string
	}{
		{"public", ":large_green_circle:"},
		{"internal", ":large_yellow_circle:"},
		{"sensitive", ":large_orange_circle:"},
		{"secret", ":red_circle:"},
		{"", ""},
		{"unknown", ""},
	}
	for _, tt := range tests {
		t.Run(tt.level, func(t *testing.T) {
			got := sensitivityEmoji(tt.level)
			if got != tt.want {
				t.Errorf("sensitivityEmoji(%q) = %q, want %q", tt.level, got, tt.want)
			}
		})
	}
}

// approvedEvent is the approval of an escalation (approvalEvent carries the
// approved_by change that approvalTransition gates on), with fields layered
// over a realistic merge.
func approvedEvent(t *testing.T, id string, fields map[string]string) []byte {
	t.Helper()
	f := map[string]string{
		"operation_type":   "github_pr_merge",
		"operation_spec":   `{"repo":"mattbakerconsulting/gardenio","pr":700}`,
		"approved_by":      "machine:gardenio-laptop-operator",
		"execution_target": "local",
		"requesting_agent": "gd-agent-xyz",
	}
	for k, v := range fields {
		f[k] = v
	}
	return approvalEvent(t, BeadEvent{ID: id, Type: "escalation", Labels: []string{"project:gardenio"}, Fields: f})
}

// TestEscalations_ExecuteByCoordinatorSpawnsNoExecutor: the coordinator that
// holds execute_by does the work itself (it now holds the escalation), so the
// bridge must not spawn an "escalation-<id>" executor (nor does it for any
// other approval: TestHandleUpdated_ApprovalDoesNotSpawn).
func TestEscalations_ExecuteByCoordinatorSpawnsNoExecutor(t *testing.T) {
	for _, lane := range []string{"coordinator:merge-desk", "coordinator:laptop", "server"} {
		client := newMockEscalationBeadClient()
		esc := NewEscalations(EscalationsConfig{Daemon: client, Logger: slog.Default()})
		esc.handleUpdated(context.Background(), approvedEvent(t, "gd-x1", map[string]string{"execute_by": lane}))
		client.mu.Lock()
		if len(client.spawnedAgents) != 0 || client.updatedFields["gd-x1"]["executor"] != "" {
			t.Errorf("execute_by=%s spawned %v / executor %q", lane, client.spawnedAgents, client.updatedFields["gd-x1"]["executor"])
		}
		client.mu.Unlock()
	}
}

// execute_by=requester: the requesting agent is told to proceed; no executor.
func TestEscalations_ExecuteByRequesterNudgesRequester(t *testing.T) {
	client := newMockEscalationBeadClient()
	client.beads["gd-agent-xyz"] = &beadsapi.BeadDetail{ID: "gd-agent-xyz", Type: "agent", Fields: map[string]string{"agent": "crew-asker"}}
	esc := NewEscalations(EscalationsConfig{Daemon: client, Logger: slog.Default()})
	var nudged []string
	esc.nudge = func(_ context.Context, agent, msg string) error {
		nudged = append(nudged, agent+": "+msg)
		return nil
	}
	esc.handleUpdated(context.Background(), approvedEvent(t, "gd-x2", map[string]string{"execute_by": "requester"}))

	if len(nudged) != 1 || !strings.HasPrefix(nudged[0], "crew-asker: ") || !strings.Contains(nudged[0], "gb escalation close gd-x2") {
		t.Fatalf("nudges = %v", nudged)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	for _, n := range client.spawnedAgents {
		if strings.HasPrefix(n, "escalation-") {
			t.Errorf("requester-executed escalation spawned executor %q", n)
		}
	}
}

// An in-hand, unapproved escalation's pingback request does not post to
// Slack: hand-up (which makes it the owner's) is its way to a human.
func TestEscalations_PingbackSkipsInHand(t *testing.T) {
	client := newMockEscalationBeadClient()
	notifier := &mockEscalationNotifier{} // escalation_pingback_loop_test.go
	esc := NewEscalations(EscalationsConfig{Daemon: client, Logger: slog.Default(), EscalationNotifier: notifier})
	for _, c := range []struct {
		id, held, audience string
		posts              int
	}{{"gd-p1", "coordinator:laptop", "operator", 0}, {"gd-p2", "operator", "owner", 1}} {
		data, _ := json.Marshal(map[string]any{"bead": BeadEvent{ID: c.id, Type: "escalation", Status: "open", Fields: map[string]string{
			"held_by": c.held, "audience": c.audience, "pingback_requested_at": "2026-10-20T05:00:00Z",
		}}})
		notifier.mu.Lock()
		before := len(notifier.pingbacks)
		notifier.mu.Unlock()
		esc.handleUpdated(context.Background(), data)
		notifier.mu.Lock()
		got := len(notifier.pingbacks) - before
		notifier.mu.Unlock()
		if got != c.posts {
			t.Errorf("held_by=%s: %d pingback posts, want %d", c.held, got, c.posts)
		}
	}
}
