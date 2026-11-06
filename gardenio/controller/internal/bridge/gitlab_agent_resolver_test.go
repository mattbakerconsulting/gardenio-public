package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// mockResolverDaemon implements AgentResolverClient for testing.
type mockResolverDaemon struct {
	mu           sync.Mutex
	agentBeads   map[string]*beadsapi.BeadDetail // agent name → bead
	spawnedBeads []*beadsapi.BeadDetail
	updatedBeads map[string]map[string]string    // bead ID → fields
	mrBeadByTask map[string]*beadsapi.BeadDetail // task bead ID → MR bead
	spawnErr     error
}

func newMockResolverDaemon() *mockResolverDaemon {
	return &mockResolverDaemon{
		agentBeads:   make(map[string]*beadsapi.BeadDetail),
		updatedBeads: make(map[string]map[string]string),
		mrBeadByTask: make(map[string]*beadsapi.BeadDetail),
	}
}

// MRBeadForTask returns a configured MR bead for the given task, or nil to
// exercise the legacy task-bead mr_* field fallback path.
func (m *mockResolverDaemon) MRBeadForTask(_ context.Context, taskBeadID string) (*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mrBeadByTask[taskBeadID], nil
}

func (m *mockResolverDaemon) FindAgentBead(_ context.Context, agentName string) (*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.agentBeads[agentName]; ok {
		return b, nil
	}
	return nil, &beadsapi.APIError{StatusCode: 404, Message: "not found"}
}

func (m *mockResolverDaemon) SpawnAgent(_ context.Context, agentName, project, taskID, role, customPrompt string, extraFields ...map[string]string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.spawnErr != nil {
		return "", m.spawnErr
	}
	id := "bd-spawned-" + agentName
	fields := map[string]string{
		"agent":   agentName,
		"project": project,
		"mode":    "crew",
		"role":    role,
		"prompt":  customPrompt,
	}
	if taskID != "" {
		fields["task_id"] = taskID
	}
	for _, extra := range extraFields {
		for k, v := range extra {
			fields[k] = v
		}
	}
	bead := &beadsapi.BeadDetail{
		ID:     id,
		Title:  agentName,
		Type:   "agent",
		Fields: fields,
	}
	m.spawnedBeads = append(m.spawnedBeads, bead)
	return id, nil
}

func (m *mockResolverDaemon) ListActiveAgentBeadDetails(_ context.Context) ([]*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []*beadsapi.BeadDetail
	for _, b := range m.agentBeads {
		result = append(result, b)
	}
	return result, nil
}

func (m *mockResolverDaemon) UpdateBeadFields(_ context.Context, beadID string, fields map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updatedBeads[beadID] = fields
	return nil
}

func (m *mockResolverDaemon) getSpawned() []*beadsapi.BeadDetail {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*beadsapi.BeadDetail{}, m.spawnedBeads...)
}

func (m *mockResolverDaemon) getUpdated(beadID string) map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.updatedBeads[beadID]
}

// newTestAgentResolver creates an AgentResolver with a mock daemon and a GitLab
// test server that returns MR details.
func newTestAgentResolver(daemon *mockResolverDaemon, gitlabServer *httptest.Server) *AgentResolver {
	var gitlab *GitLabClient
	if gitlabServer != nil {
		gitlab = newTestGitLabClient(gitlabServer.URL)
	}
	return NewAgentResolver(AgentResolverConfig{
		Daemon: daemon,
		GitLab: gitlab,
		Nudge:  subscriberNudge(daemon.FindAgentBead),
		Logger: slog.Default(),
	})
}

func TestResolveAndNudge_AgentAlive_Nudges(t *testing.T) {
	// Set up a coop server that accepts nudges.
	var nudgeReceived string
	coopServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agent/nudge" {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			nudgeReceived = body["message"]
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]bool{"delivered": true})
			return
		}
		http.NotFound(w, r)
	}))
	defer coopServer.Close()

	daemon := newMockResolverDaemon()
	daemon.agentBeads["test-agent"] = &beadsapi.BeadDetail{
		ID:    "bd-agent-1",
		Title: "test-agent",
		Type:  "agent",
		Notes: "coop_url: " + coopServer.URL,
		Fields: map[string]string{
			"agent":   "test-agent",
			"project": "gardenio",
		},
	}
	// project_id / mr_iid are now sourced exclusively from the MR bead
	// (epic ka-0000001613 / task ka-0000008134).
	daemon.mrBeadByTask["gd-task-1"] = &beadsapi.BeadDetail{
		ID:   "mr-bead-1",
		Type: "merge-request",
		Fields: map[string]string{
			"mr_url":            "https://gitlab.com/org/repo/-/merge_requests/42",
			"gitlab_project_id": "99",
			"gitlab_mr_iid":     "42",
		},
	}

	resolver := newTestAgentResolver(daemon, nil)

	taskBead := BeadEvent{
		ID:       "gd-task-1",
		Type:     "task",
		Title:    "Fix auth bug",
		Assignee: "test-agent",
		Labels:   []string{"project:gardenio"},
		Fields: map[string]string{
			"mr_url": "https://gitlab.com/org/repo/-/merge_requests/42",
		},
	}

	err := resolver.ResolveAndNudge(context.Background(), taskBead, "Review comment: fix the bug")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if nudgeReceived != "Review comment: fix the bug" {
		t.Errorf("nudge message = %q, want %q", nudgeReceived, "Review comment: fix the bug")
	}

	// Verify MR binding fields were set on agent bead.
	updated := daemon.getUpdated("bd-agent-1")
	if updated["gitlab_mr_url"] != "https://gitlab.com/org/repo/-/merge_requests/42" {
		t.Errorf("gitlab_mr_url = %q, want MR URL", updated["gitlab_mr_url"])
	}
	if updated["gitlab_project_id"] != "99" {
		t.Errorf("gitlab_project_id = %q, want 99", updated["gitlab_project_id"])
	}
	if updated["gitlab_mr_iid"] != "42" {
		t.Errorf("gitlab_mr_iid = %q, want 42", updated["gitlab_mr_iid"])
	}
}

func TestResolveAndNudge_AgentAlive_CoopURLInFields(t *testing.T) {
	// Agent has coop_url as a field instead of in notes.
	var nudgeReceived bool
	coopServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agent/nudge" {
			nudgeReceived = true
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]bool{"delivered": true})
			return
		}
		http.NotFound(w, r)
	}))
	defer coopServer.Close()

	daemon := newMockResolverDaemon()
	daemon.agentBeads["test-agent"] = &beadsapi.BeadDetail{
		ID:    "bd-agent-1",
		Title: "test-agent",
		Type:  "agent",
		Fields: map[string]string{
			"agent":    "test-agent",
			"coop_url": coopServer.URL,
		},
	}

	resolver := newTestAgentResolver(daemon, nil)

	taskBead := BeadEvent{
		ID:       "gd-task-1",
		Type:     "task",
		Assignee: "test-agent",
		Fields: map[string]string{
			"mr_url": "https://gitlab.com/org/repo/-/merge_requests/42",
		},
	}

	err := resolver.ResolveAndNudge(context.Background(), taskBead, "test message")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !nudgeReceived {
		t.Error("expected nudge to be delivered via coop")
	}
}

func TestResolveAndNudge_AgentDead_SpawnsNew(t *testing.T) {
	daemon := newMockResolverDaemon()
	// No agent bead → agent is dead.

	// GitLab server returns MR details with source branch.
	gitlabServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := GitLabMR{
			IID:          42,
			State:        "opened",
			ProjectID:    99,
			SourceBranch: "fix/auth-bug",
			TargetBranch: "main",
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer gitlabServer.Close()

	resolver := newTestAgentResolver(daemon, gitlabServer)

	taskBead := BeadEvent{
		ID:       "gd-task-1",
		Type:     "task",
		Title:    "Fix auth bug",
		Assignee: "test-agent",
		Labels:   []string{"project:gardenio"},
		Fields: map[string]string{
			"mr_url": "https://gitlab.com/org/repo/-/merge_requests/42",
		},
	}

	err := resolver.ResolveAndNudge(context.Background(), taskBead, "Review comment")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify agent was spawned.
	spawned := daemon.getSpawned()
	if len(spawned) != 1 {
		t.Fatalf("expected 1 spawned agent, got %d", len(spawned))
	}

	agent := spawned[0]
	if agent.Fields["agent"] != "test-agent" {
		t.Errorf("agent name = %q, want test-agent", agent.Fields["agent"])
	}
	if agent.Fields["project"] != "gardenio" {
		t.Errorf("project = %q, want gardenio", agent.Fields["project"])
	}
	if agent.Fields["task_id"] != "gd-task-1" {
		t.Errorf("task_id = %q, want gd-task-1", agent.Fields["task_id"])
	}
	if agent.Fields["spawn_source"] != "gitlab-mr-review" {
		t.Errorf("spawn_source = %q, want gitlab-mr-review", agent.Fields["spawn_source"])
	}
	if agent.Fields["gitlab_mr_url"] != "https://gitlab.com/org/repo/-/merge_requests/42" {
		t.Errorf("gitlab_mr_url = %q, want MR URL", agent.Fields["gitlab_mr_url"])
	}
	if agent.Fields["gitlab_mr_source_branch"] != "fix/auth-bug" {
		t.Errorf("gitlab_mr_source_branch = %q, want fix/auth-bug", agent.Fields["gitlab_mr_source_branch"])
	}
	if agent.Fields["gitlab_project_id"] != "99" {
		t.Errorf("gitlab_project_id = %q, want 99", agent.Fields["gitlab_project_id"])
	}
	if agent.Fields["gitlab_mr_iid"] != "42" {
		t.Errorf("gitlab_mr_iid = %q, want 42", agent.Fields["gitlab_mr_iid"])
	}
}

func TestResolveAndNudge_AgentExistsButStopping_SpawnsNew(t *testing.T) {
	daemon := newMockResolverDaemon()
	// Agent bead still active but its agent asked to stop (not running).
	daemon.agentBeads["test-agent"] = &beadsapi.BeadDetail{
		ID:    "bd-agent-old",
		Title: "test-agent",
		Type:  "agent",
		Fields: map[string]string{
			"agent":          "test-agent",
			"project":        "gardenio",
			"stop_requested": "true",
		},
	}

	resolver := newTestAgentResolver(daemon, nil)

	taskBead := BeadEvent{
		ID:       "gd-task-1",
		Type:     "task",
		Title:    "Fix it",
		Assignee: "test-agent",
		Labels:   []string{"project:gardenio"},
		Fields: map[string]string{
			"mr_url": "https://gitlab.com/org/repo/-/merge_requests/5",
		},
	}

	err := resolver.ResolveAndNudge(context.Background(), taskBead, "Review comment")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spawned := daemon.getSpawned()
	if len(spawned) != 1 {
		t.Fatalf("expected 1 spawned agent, got %d", len(spawned))
	}
	if spawned[0].Fields["agent"] != "test-agent" {
		t.Errorf("spawned agent name = %q, want test-agent", spawned[0].Fields["agent"])
	}
}

func TestResolveAndNudge_NoAssignee_ReturnsError(t *testing.T) {
	daemon := newMockResolverDaemon()
	resolver := newTestAgentResolver(daemon, nil)

	taskBead := BeadEvent{
		ID:       "gd-task-1",
		Type:     "task",
		Assignee: "", // no assignee
		Fields:   map[string]string{"mr_url": "https://gitlab.com/org/repo/-/merge_requests/1"},
	}

	err := resolver.ResolveAndNudge(context.Background(), taskBead, "msg")
	if err == nil {
		t.Fatal("expected error for empty assignee")
	}
}

func TestResolveAndNudge_MRMerged_SkipsSpawn(t *testing.T) {
	daemon := newMockResolverDaemon()
	// merged-state lives on the MR bead now (epic ka-0000001613).
	daemon.mrBeadByTask["gd-task-1"] = &beadsapi.BeadDetail{
		ID:   "mr-bead-1",
		Type: "merge-request",
		Fields: map[string]string{
			"mr_url":     "https://gitlab.com/org/repo/-/merge_requests/42",
			"mr_outcome": "merged",
		},
	}
	resolver := newTestAgentResolver(daemon, nil)

	taskBead := BeadEvent{
		ID:       "gd-task-1",
		Type:     "task",
		Title:    "Fix auth bug",
		Assignee: "test-agent",
		Labels:   []string{"project:gardenio"},
		Fields: map[string]string{
			"mr_url": "https://gitlab.com/org/repo/-/merge_requests/42",
		},
	}

	err := resolver.ResolveAndNudge(context.Background(), taskBead, "Review comment")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spawned := daemon.getSpawned()
	if len(spawned) != 0 {
		t.Errorf("expected no spawned agents for merged MR, got %d", len(spawned))
	}
}

func TestResolveAndNudge_TaskClosed_SkipsSpawn(t *testing.T) {
	daemon := newMockResolverDaemon()
	resolver := newTestAgentResolver(daemon, nil)

	taskBead := BeadEvent{
		ID:       "gd-task-1",
		Type:     "task",
		Title:    "Fix auth bug",
		Status:   "closed",
		Assignee: "test-agent",
		Labels:   []string{"project:gardenio"},
		Fields: map[string]string{
			"mr_url": "https://gitlab.com/org/repo/-/merge_requests/42",
		},
	}

	err := resolver.ResolveAndNudge(context.Background(), taskBead, "Review comment")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spawned := daemon.getSpawned()
	if len(spawned) != 0 {
		t.Errorf("expected no spawned agents for closed task, got %d", len(spawned))
	}
}

func TestResolveAndNudge_RecentSpawn_SkipsDouble(t *testing.T) {
	daemon := newMockResolverDaemon()

	gitlabServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := GitLabMR{IID: 42, State: "opened", ProjectID: 99, SourceBranch: "fix/auth"}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer gitlabServer.Close()

	resolver := newTestAgentResolver(daemon, gitlabServer)

	taskBead := BeadEvent{
		ID:       "gd-task-1",
		Type:     "task",
		Title:    "Fix auth bug",
		Assignee: "test-agent",
		Labels:   []string{"project:gardenio"},
		Fields: map[string]string{
			"mr_url": "https://gitlab.com/org/repo/-/merge_requests/42",
		},
	}

	// First call should spawn.
	if err := resolver.ResolveAndNudge(context.Background(), taskBead, "Review comment"); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if len(daemon.getSpawned()) != 1 {
		t.Fatalf("expected 1 spawn on first call, got %d", len(daemon.getSpawned()))
	}

	// Second call should be deduped.
	if err := resolver.ResolveAndNudge(context.Background(), taskBead, "Another comment"); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if len(daemon.getSpawned()) != 1 {
		t.Errorf("expected still 1 spawn after dedup, got %d", len(daemon.getSpawned()))
	}
}

// TestBuildMRFields_PopulatesFromMRBead verifies that mr metadata
// (project_id, iid) flows from the linked MR bead — task-bead mr_* writes
// were dropped under epic ka-0000001613 / task ka-0000008134. The task
// bead's `mr_url` field still acts as the trigger seed.
func TestBuildMRFields_PopulatesFromMRBead(t *testing.T) {
	d := newMockResolverDaemon()
	d.mrBeadByTask["gd-task-1"] = &beadsapi.BeadDetail{
		ID:   "mr-bead-1",
		Type: "merge-request",
		Fields: map[string]string{
			"mr_url":            "https://gitlab.com/org/repo/-/merge_requests/42",
			"gitlab_project_id": "99",
			"gitlab_mr_iid":     "42",
		},
	}
	resolver := newTestAgentResolver(d, nil)

	taskBead := BeadEvent{
		ID: "gd-task-1",
		Fields: map[string]string{
			"mr_url": "https://gitlab.com/org/repo/-/merge_requests/42",
		},
	}

	fields := resolver.buildMRFields(taskBead)
	if fields["gitlab_mr_url"] != "https://gitlab.com/org/repo/-/merge_requests/42" {
		t.Errorf("gitlab_mr_url = %q", fields["gitlab_mr_url"])
	}
	if fields["gitlab_project_id"] != "99" {
		t.Errorf("gitlab_project_id = %q", fields["gitlab_project_id"])
	}
	if fields["gitlab_mr_iid"] != "42" {
		t.Errorf("gitlab_mr_iid = %q", fields["gitlab_mr_iid"])
	}
}

func TestBuildMRFields_FetchesSourceBranch(t *testing.T) {
	gitlabServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := GitLabMR{
			IID:          42,
			ProjectID:    99,
			SourceBranch: "feature/new-thing",
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer gitlabServer.Close()

	resolver := newTestAgentResolver(newMockResolverDaemon(), gitlabServer)

	taskBead := BeadEvent{
		Fields: map[string]string{
			"mr_url": "https://gitlab.com/org/repo/-/merge_requests/42",
		},
	}

	fields := resolver.buildMRFields(taskBead)
	if fields["gitlab_mr_source_branch"] != "feature/new-thing" {
		t.Errorf("gitlab_mr_source_branch = %q, want feature/new-thing", fields["gitlab_mr_source_branch"])
	}
	// Should backfill project_id and iid from the GitLab response.
	if fields["gitlab_project_id"] != "99" {
		t.Errorf("gitlab_project_id = %q, want 99", fields["gitlab_project_id"])
	}
	if fields["gitlab_mr_iid"] != "42" {
		t.Errorf("gitlab_mr_iid = %q, want 42", fields["gitlab_mr_iid"])
	}
}

// TestBuildMRFields_PrefersMRBead asserts that values from the linked MR bead
// override task-bead mr_* fields (epic ka-0000001613).
func TestBuildMRFields_PrefersMRBead(t *testing.T) {
	d := newMockResolverDaemon()
	d.mrBeadByTask["task-1"] = &beadsapi.BeadDetail{
		ID:   "mr-bead-1",
		Type: "merge-request",
		Fields: map[string]string{
			"mr_url":            "https://gitlab.com/org/repo/-/merge_requests/77",
			"gitlab_project_id": "77",
			"mr_iid":            "77",
			"source_branch":     "from-mr-bead",
		},
	}
	resolver := newTestAgentResolver(d, nil)

	taskBead := BeadEvent{
		ID: "task-1",
		Fields: map[string]string{
			"mr_url":                  "https://gitlab.com/org/repo/-/merge_requests/42",
			"gitlab_project_id":       "99",
			"gitlab_mr_iid":           "42",
			"gitlab_mr_source_branch": "from-task",
		},
	}

	fields := resolver.buildMRFields(taskBead)
	if got := fields["gitlab_mr_url"]; got != "https://gitlab.com/org/repo/-/merge_requests/77" {
		t.Errorf("gitlab_mr_url = %q, want MR-bead value", got)
	}
	if fields["gitlab_project_id"] != "77" {
		t.Errorf("gitlab_project_id = %q, want 77", fields["gitlab_project_id"])
	}
	if fields["gitlab_mr_iid"] != "77" {
		t.Errorf("gitlab_mr_iid = %q, want 77", fields["gitlab_mr_iid"])
	}
	if fields["gitlab_mr_source_branch"] != "from-mr-bead" {
		t.Errorf("gitlab_mr_source_branch = %q, want from-mr-bead", fields["gitlab_mr_source_branch"])
	}
}

func TestBuildMRFields_NoMRURL_ReturnsEmpty(t *testing.T) {
	resolver := newTestAgentResolver(newMockResolverDaemon(), nil)

	taskBead := BeadEvent{
		Fields: map[string]string{}, // no mr_url
	}

	fields := resolver.buildMRFields(taskBead)
	if len(fields) != 0 {
		t.Errorf("expected empty fields, got %v", fields)
	}
}

func TestHandleReviewNudge_WithResolver(t *testing.T) {
	// Test that handleReviewNudge uses the resolver when configured.
	var nudgeReceived string
	coopServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agent/nudge" {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			nudgeReceived = body["message"]
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]bool{"delivered": true})
			return
		}
		http.NotFound(w, r)
	}))
	defer coopServer.Close()

	resolverDaemon := newMockResolverDaemon()
	resolverDaemon.agentBeads["my-agent"] = &beadsapi.BeadDetail{
		ID:    "bd-agent-1",
		Title: "my-agent",
		Type:  "agent",
		Notes: "coop_url: " + coopServer.URL,
		Fields: map[string]string{
			"agent": "my-agent",
		},
	}

	resolver := newTestAgentResolver(resolverDaemon, nil)

	// Build a mock GitLabBeadClient that returns the task bead.
	syncDaemon := &mockGitLabBeadClient{
		beads: []*beadsapi.BeadDetail{},
	}

	sync := NewGitLabSync(GitLabSyncConfig{
		Daemon:   syncDaemon,
		Logger:   slog.Default(),
		Resolver: resolver,
	})

	// Simulate a bead update event with mr_has_review_comments changed.
	// Changes are at the wrapper level in SSE format, not inside the bead.
	data, _ := json.Marshal(map[string]any{
		"bead": map[string]any{
			"id":       "gd-task-1",
			"type":     "task",
			"title":    "Fix bug",
			"assignee": "my-agent",
			"fields": map[string]string{
				"mr_has_review_comments": "true",
				"mr_url":                 "https://gitlab.com/org/repo/-/merge_requests/42",
			},
		},
		"changes": map[string]any{
			"mr_has_review_comments": "true",
		},
	})
	sync.handleReviewNudge(context.Background(), data)

	if nudgeReceived == "" {
		t.Error("expected nudge to be delivered via resolver")
	}
	if nudgeReceived != "MR has new review comments — address them: https://gitlab.com/org/repo/-/merge_requests/42" {
		t.Errorf("unexpected nudge message: %q", nudgeReceived)
	}
}

// mockGitLabBeadClient implements GitLabBeadClient for sync tests.
type mockGitLabBeadClient struct {
	beads []*beadsapi.BeadDetail
}

func (m *mockGitLabBeadClient) ListTaskBeads(_ context.Context) ([]*beadsapi.BeadDetail, error) {
	return m.beads, nil
}

func (m *mockGitLabBeadClient) ListTaskBeadsAll(_ context.Context) ([]*beadsapi.BeadDetail, error) {
	return m.beads, nil
}

// ListTaskBeadsForMR mirrors the daemon's server-side fields.mr_url equality
// predicate so tests exercise the same narrowing the real client performs.
func (m *mockGitLabBeadClient) ListTaskBeadsForMR(_ context.Context, mrURL string) ([]*beadsapi.BeadDetail, error) {
	if mrURL == "" {
		return nil, nil
	}
	var result []*beadsapi.BeadDetail
	for _, b := range m.beads {
		if b.Fields["mr_url"] == mrURL {
			result = append(result, b)
		}
	}
	return result, nil
}

func (m *mockGitLabBeadClient) UpdateBeadFields(_ context.Context, _ string, _ map[string]string) error {
	return nil
}

func (m *mockGitLabBeadClient) ListActiveAgentBeadDetails(_ context.Context) ([]*beadsapi.BeadDetail, error) {
	return nil, nil
}

func (m *mockGitLabBeadClient) AddComment(_ context.Context, _, _, _ string) error {
	return nil
}

func (m *mockGitLabBeadClient) UpsertMergeRequestBead(_ context.Context, _ beadsapi.MergeRequestBeadFields, _ bool) (string, error) {
	return "", nil
}

func (m *mockGitLabBeadClient) AddDependency(_ context.Context, _, _, _, _ string) error {
	return nil
}

func (m *mockGitLabBeadClient) MRBeadForTask(_ context.Context, _ string) (*beadsapi.BeadDetail, error) {
	return nil, nil
}

func TestFindActiveAgentForMR_FindsAliveAgent(t *testing.T) {
	daemon := newMockResolverDaemon()
	daemon.agentBeads["mr-agent"] = &beadsapi.BeadDetail{
		ID:    "bd-agent-mr",
		Title: "mr-agent",
		Type:  "agent",
		Notes: "coop_url: http://localhost:9999",
		Fields: map[string]string{
			"agent":         "mr-agent",
			"gitlab_mr_url": "https://gitlab.com/org/repo/-/merge_requests/42",
		},
	}

	resolver := newTestAgentResolver(daemon, nil)

	name, found := resolver.FindActiveAgentForMR(context.Background(), "https://gitlab.com/org/repo/-/merge_requests/42")
	if !found {
		t.Fatal("expected to find active agent for MR")
	}
	if name != "mr-agent" {
		t.Errorf("agent name = %q, want mr-agent", name)
	}
}

// A bead still listed as active but whose agent asked to stop is not alive:
// liveness is agent_state / stop_requested now that nothing dials coop_url.
func TestFindActiveAgentForMR_StoppingAgent(t *testing.T) {
	daemon := newMockResolverDaemon()
	daemon.agentBeads["dead-agent"] = &beadsapi.BeadDetail{
		ID:    "bd-agent-dead",
		Title: "dead-agent",
		Type:  "agent",
		Fields: map[string]string{
			"agent":          "dead-agent",
			"gitlab_mr_url":  "https://gitlab.com/org/repo/-/merge_requests/42",
			"stop_requested": "true",
		},
	}

	resolver := newTestAgentResolver(daemon, nil)

	_, found := resolver.FindActiveAgentForMR(context.Background(), "https://gitlab.com/org/repo/-/merge_requests/42")
	if found {
		t.Error("expected not to find a stopping agent")
	}
}

func TestFindActiveAgentForMR_DifferentMR(t *testing.T) {
	daemon := newMockResolverDaemon()
	daemon.agentBeads["other-agent"] = &beadsapi.BeadDetail{
		ID:    "bd-agent-other",
		Title: "other-agent",
		Type:  "agent",
		Notes: "coop_url: http://localhost:9999",
		Fields: map[string]string{
			"agent":         "other-agent",
			"gitlab_mr_url": "https://gitlab.com/org/repo/-/merge_requests/99",
		},
	}

	resolver := newTestAgentResolver(daemon, nil)

	_, found := resolver.FindActiveAgentForMR(context.Background(), "https://gitlab.com/org/repo/-/merge_requests/42")
	if found {
		t.Error("expected not to find agent for different MR")
	}
}

func TestResolveAndNudge_MRScopedDedup(t *testing.T) {
	// When two different task beads reference the same MR, only one agent should be spawned.
	daemon := newMockResolverDaemon()

	resolver := newTestAgentResolver(daemon, nil)

	task1 := BeadEvent{
		ID:       "gd-task-1",
		Type:     "task",
		Title:    "First task",
		Assignee: "agent-a",
		Labels:   []string{"project:gardenio"},
		Fields: map[string]string{
			"mr_url": "https://gitlab.com/org/repo/-/merge_requests/42",
		},
	}

	task2 := BeadEvent{
		ID:       "gd-task-2",
		Type:     "task",
		Title:    "Second task",
		Assignee: "agent-b",
		Labels:   []string{"project:gardenio"},
		Fields: map[string]string{
			"mr_url": "https://gitlab.com/org/repo/-/merge_requests/42",
		},
	}

	// First call should spawn.
	if err := resolver.ResolveAndNudge(context.Background(), task1, "Comment 1"); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if len(daemon.getSpawned()) != 1 {
		t.Fatalf("expected 1 spawn, got %d", len(daemon.getSpawned()))
	}

	// Second call with different task but same MR should be deduped by MR URL.
	if err := resolver.ResolveAndNudge(context.Background(), task2, "Comment 2"); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if len(daemon.getSpawned()) != 1 {
		t.Errorf("expected still 1 spawn after MR dedup, got %d", len(daemon.getSpawned()))
	}
}

func TestResolveAndNudge_ActiveAgentForMR_NudgesInstead(t *testing.T) {
	// When an active agent already exists for the MR, nudge it instead of spawning.
	var nudgeReceived string
	coopServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agent/nudge" {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			nudgeReceived = body["message"]
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]bool{"delivered": true})
			return
		}
		http.NotFound(w, r)
	}))
	defer coopServer.Close()

	daemon := newMockResolverDaemon()
	// Agent "other-agent" is alive and bound to MR 42.
	daemon.agentBeads["other-agent"] = &beadsapi.BeadDetail{
		ID:    "bd-other",
		Title: "other-agent",
		Type:  "agent",
		Notes: "coop_url: " + coopServer.URL,
		Fields: map[string]string{
			"agent":         "other-agent",
			"gitlab_mr_url": "https://gitlab.com/org/repo/-/merge_requests/42",
		},
	}

	resolver := newTestAgentResolver(daemon, nil)

	// Task assigned to "dead-agent" (no agent bead) for the same MR.
	taskBead := BeadEvent{
		ID:       "gd-task-new",
		Type:     "task",
		Title:    "New comment",
		Assignee: "dead-agent",
		Labels:   []string{"project:gardenio"},
		Fields: map[string]string{
			"mr_url": "https://gitlab.com/org/repo/-/merge_requests/42",
		},
	}

	err := resolver.ResolveAndNudge(context.Background(), taskBead, "New review comment")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should nudge existing agent, not spawn.
	if nudgeReceived != "New review comment" {
		t.Errorf("expected nudge to existing agent, got %q", nudgeReceived)
	}
	if len(daemon.getSpawned()) != 0 {
		t.Errorf("expected no spawns, got %d", len(daemon.getSpawned()))
	}
}

// pouring-stall dependency-graph stubs (ka-0000000957) — this mock does not
// exercise the sweep.
func (m *mockResolverDaemon) GetReverseDependencies(_ context.Context, _ string) ([]beadsapi.Dependency, error) {
	return nil, nil
}
func (m *mockResolverDaemon) BatchGetDependencies(_ context.Context, _ []string, _ string) (map[string][]beadsapi.Dependency, error) {
	return nil, nil
}
func (m *mockResolverDaemon) BatchGetBeads(_ context.Context, _ []string) (map[string]*beadsapi.BeadDetail, error) {
	return nil, nil
}
func (m *mockGitLabBeadClient) GetReverseDependencies(_ context.Context, _ string) ([]beadsapi.Dependency, error) {
	return nil, nil
}
func (m *mockGitLabBeadClient) BatchGetDependencies(_ context.Context, _ []string, _ string) (map[string][]beadsapi.Dependency, error) {
	return nil, nil
}
func (m *mockGitLabBeadClient) BatchGetBeads(_ context.Context, _ []string) (map[string]*beadsapi.BeadDetail, error) {
	return nil, nil
}
