package bridge

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// mockJiraResolverDaemon implements JiraAgentResolverClient for testing.
type mockJiraResolverDaemon struct {
	mu            sync.Mutex
	agentBeads    map[string]*beadsapi.BeadDetail // by agent name
	activeAgents  []*beadsapi.BeadDetail
	spawned       []spawnCall
	spawnID       string
	findAgentErr  error
	listAgentsErr error
	triggerBeads  []*beadsapi.BeadDetail          // returned by ListBeadsFiltered for trigger lookups
	taskBeads     map[string]*beadsapi.BeadDetail // by bead ID, returned by GetBead
	closedBeads   []string                        // bead IDs passed to CloseBead
	closeBeadErr  error
	updatedFields []updateFieldsCall // bead-field updates (GAP 2 live-agent comment_id refresh)
	updateErr     error
	// spawnErrs is a per-call error sequence for SpawnAgent: entry i (nil ⇒
	// success) is returned on the i-th call, and any entry past the slice ⇒
	// success. Used to model a transient-then-success daemon blip on the
	// wake-path respawn retry (ka-0000002721). Each ATTEMPT is recorded in
	// spawnAttempts; only a successful call appends to spawned.
	spawnErrs     []error
	spawnAttempts int
}

// updateFieldsCall records one UpdateBeadFields invocation for assertions.
type updateFieldsCall struct {
	BeadID string
	Fields map[string]string
}

func (m *mockJiraResolverDaemon) UpdateBeadFields(_ context.Context, beadID string, fields map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.updateErr != nil {
		return m.updateErr
	}
	m.updatedFields = append(m.updatedFields, updateFieldsCall{BeadID: beadID, Fields: fields})
	return nil
}

func (m *mockJiraResolverDaemon) getUpdatedFields() []updateFieldsCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]updateFieldsCall, len(m.updatedFields))
	copy(out, m.updatedFields)
	return out
}

func (m *mockJiraResolverDaemon) GetBead(_ context.Context, beadID string) (*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.taskBeads[beadID]; ok {
		return b, nil
	}
	return nil, nil
}

func (m *mockJiraResolverDaemon) CloseBead(_ context.Context, beadID string, _ map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closeBeadErr != nil {
		return m.closeBeadErr
	}
	m.closedBeads = append(m.closedBeads, beadID)
	return nil
}

func (m *mockJiraResolverDaemon) getClosed() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.closedBeads))
	copy(out, m.closedBeads)
	return out
}

// findAnyAgent is FindAgentBead that also finds an agent of activeAgents, as
// the real daemon would: the mail stand-in (subscriberNudge) looks agents up
// by name.
func (m *mockJiraResolverDaemon) findAnyAgent(ctx context.Context, agentName string) (*beadsapi.BeadDetail, error) {
	if b, err := m.FindAgentBead(ctx, agentName); err == nil {
		return b, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, b := range m.activeAgents {
		if b.Fields["agent"] == agentName || b.Title == agentName {
			return b, nil
		}
	}
	return nil, fmt.Errorf("agent %s not found", agentName)
}

func (m *mockJiraResolverDaemon) FindAgentBead(_ context.Context, agentName string) (*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.findAgentErr != nil {
		return nil, m.findAgentErr
	}
	bead, ok := m.agentBeads[agentName]
	if !ok {
		return nil, fmt.Errorf("agent %s not found", agentName)
	}
	return bead, nil
}

func (m *mockJiraResolverDaemon) ListActiveAgentBeadDetails(_ context.Context) ([]*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listAgentsErr != nil {
		return nil, m.listAgentsErr
	}
	return m.activeAgents, nil
}

func (m *mockJiraResolverDaemon) SpawnAgent(_ context.Context, agentName, project, taskID, role, prompt string, extra ...map[string]string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	attempt := m.spawnAttempts
	m.spawnAttempts++
	if attempt < len(m.spawnErrs) {
		if err := m.spawnErrs[attempt]; err != nil {
			return "", err
		}
	}
	var extraFields map[string]string
	if len(extra) > 0 {
		extraFields = extra[0]
	}
	m.spawned = append(m.spawned, spawnCall{
		AgentName: agentName,
		Project:   project,
		TaskID:    taskID,
		Role:      role,
		Prompt:    prompt,
		Extra:     extraFields,
	})
	return m.spawnID, nil
}

func (m *mockJiraResolverDaemon) getSpawnAttempts() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.spawnAttempts
}

func (m *mockJiraResolverDaemon) ListBeadsFiltered(_ context.Context, _ beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return &beadsapi.ListBeadsResult{Beads: m.triggerBeads}, nil
}

func (m *mockJiraResolverDaemon) getSpawned() []spawnCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]spawnCall, len(m.spawned))
	copy(out, m.spawned)
	return out
}

func jiraResolverTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestJiraAgentResolver_NudgeAliveAgent(t *testing.T) {
	// Start a mock coop server that accepts nudge.
	var nudgeReceived bool
	coopServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nudgeReceived = true
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"delivered":true}`)
	}))
	defer coopServer.Close()

	daemon := &mockJiraResolverDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{
			"jira-pe-1234-abcd": {
				ID:     "agent-bead-1",
				Title:  "jira-pe-1234-abcd",
				Fields: map[string]string{"agent": "jira-pe-1234-abcd", "coop_url": coopServer.URL},
			},
		},
		spawnID: "new-agent-bead",
	}

	resolver := NewJiraAgentResolver(JiraAgentResolverConfig{
		Daemon: daemon,
		Nudge:  subscriberNudge(daemon.findAnyAgent),
		Logger: jiraResolverTestLogger(),
	})

	err := resolver.ResolveAndNudge(context.Background(),
		"jira-pe-1234-abcd", "PE-1234", "gardenio", "task-1", "fix the bug")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !nudgeReceived {
		t.Error("expected nudge to be delivered to alive agent")
	}
	if len(daemon.getSpawned()) != 0 {
		t.Error("expected no spawn when agent is alive")
	}
}

// TestJiraAgentResolver_FollowUpRefreshesLiveAgentCommentID pins GAP 2 of
// ka-0000007091: nudging a LIVE named agent on a follow-up must refresh that
// agent bead's jira_comment_id to the waking comment, so the agent's next
// `gb jira reply` anchors its focusedCommentId quote-link to the comment that
// woke it rather than the stale original mention.
func TestJiraAgentResolver_FollowUpRefreshesLiveAgentCommentID(t *testing.T) {
	coopServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"delivered":true}`)
	}))
	defer coopServer.Close()

	daemon := &mockJiraResolverDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{
			"jira-pe-1234-abcd": {
				ID:    "agent-bead-1",
				Title: "jira-pe-1234-abcd",
				Fields: map[string]string{
					"agent":           "jira-pe-1234-abcd",
					"coop_url":        coopServer.URL,
					"jira_comment_id": "old-mention-comment",
				},
			},
		},
	}

	resolver := NewJiraAgentResolver(JiraAgentResolverConfig{
		Daemon: daemon,
		Nudge:  subscriberNudge(daemon.findAnyAgent),
		Logger: jiraResolverTestLogger(),
	})

	err := resolver.ResolveAndNudgeFollowUp(context.Background(),
		"jira-pe-1234-abcd", "PE-1234", "gardenio", "task-1", "waking-comment-42", "please respond", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	updates := daemon.getUpdatedFields()
	if len(updates) != 1 {
		t.Fatalf("expected 1 field update refreshing jira_comment_id, got %d", len(updates))
	}
	if updates[0].BeadID != "agent-bead-1" {
		t.Errorf("refreshed the wrong bead: %q", updates[0].BeadID)
	}
	if got := updates[0].Fields["jira_comment_id"]; got != "waking-comment-42" {
		t.Errorf("jira_comment_id = %q, want waking-comment-42", got)
	}
}

// TestJiraAgentResolver_FollowUpRefreshesOtherActiveAgent covers the second
// live-agent branch (GAP 2): when the named agent is dead but another active
// agent is bound to the ticket, the follow-up nudges THAT agent and refreshes
// ITS jira_comment_id anchor.
func TestJiraAgentResolver_FollowUpRefreshesOtherActiveAgent(t *testing.T) {
	coopServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"delivered":true}`)
	}))
	defer coopServer.Close()

	daemon := &mockJiraResolverDaemon{
		// The named agent bead is absent (FindAgentBead errors) → dead path.
		agentBeads: map[string]*beadsapi.BeadDetail{},
		// But another active agent is bound to PE-1234.
		activeAgents: []*beadsapi.BeadDetail{
			{
				ID:    "other-agent-bead",
				Title: "jira-pe-1234-other",
				Fields: map[string]string{
					"agent":    "jira-pe-1234-other",
					"jira_key": "PE-1234",
					"coop_url": coopServer.URL,
				},
			},
		},
	}

	resolver := NewJiraAgentResolver(JiraAgentResolverConfig{
		Daemon: daemon,
		Nudge:  subscriberNudge(daemon.findAnyAgent),
		Logger: jiraResolverTestLogger(),
	})

	err := resolver.ResolveAndNudgeFollowUp(context.Background(),
		"jira-pe-1234-abcd", "PE-1234", "gardenio", "task-1", "waking-comment-99", "please respond", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(daemon.getSpawned()) != 0 {
		t.Errorf("expected no spawn when another active agent is found, got %d", len(daemon.getSpawned()))
	}
	updates := daemon.getUpdatedFields()
	if len(updates) != 1 {
		t.Fatalf("expected 1 field update, got %d", len(updates))
	}
	if updates[0].BeadID != "other-agent-bead" {
		t.Errorf("refreshed the wrong bead: %q", updates[0].BeadID)
	}
	if got := updates[0].Fields["jira_comment_id"]; got != "waking-comment-99" {
		t.Errorf("jira_comment_id = %q, want waking-comment-99", got)
	}
}

// TestJiraAgentResolver_FollowUpNoCommentIDNoRefresh guards that an empty
// commentID (a plain nudge that carried no originating comment id) does NOT
// clobber an existing anchor with an empty value — refresh is skipped.
func TestJiraAgentResolver_FollowUpNoCommentIDNoRefresh(t *testing.T) {
	coopServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"delivered":true}`)
	}))
	defer coopServer.Close()

	daemon := &mockJiraResolverDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{
			"jira-pe-1234-abcd": {
				ID:     "agent-bead-1",
				Title:  "jira-pe-1234-abcd",
				Fields: map[string]string{"agent": "jira-pe-1234-abcd", "coop_url": coopServer.URL},
			},
		},
	}

	resolver := NewJiraAgentResolver(JiraAgentResolverConfig{
		Daemon: daemon,
		Nudge:  subscriberNudge(daemon.findAnyAgent),
		Logger: jiraResolverTestLogger(),
	})

	// Empty commentID (the plain ResolveAndNudge entry point).
	if err := resolver.ResolveAndNudge(context.Background(),
		"jira-pe-1234-abcd", "PE-1234", "gardenio", "task-1", "ping"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := len(daemon.getUpdatedFields()); n != 0 {
		t.Errorf("expected no field update when commentID is empty, got %d", n)
	}
}

func TestJiraAgentResolver_SpawnDeadAgent(t *testing.T) {
	// Agent bead still active but its agent asked to stop (dead).
	daemon := &mockJiraResolverDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{
			"jira-pe-1234-abcd": {
				ID:     "agent-bead-1",
				Title:  "jira-pe-1234-abcd",
				Fields: map[string]string{"agent": "jira-pe-1234-abcd", "stop_requested": "true"},
			},
		},
		spawnID: "new-agent-bead",
		triggerBeads: []*beadsapi.BeadDetail{
			{Fields: map[string]string{
				"action": "spawn_agent", "project": "gardenio",
				"role": "gardenio-crew", "enabled": "true",
			}},
		},
	}

	resolver := NewJiraAgentResolver(JiraAgentResolverConfig{
		Daemon: daemon,
		Nudge:  subscriberNudge(daemon.findAnyAgent),
		Logger: jiraResolverTestLogger(),
	})

	err := resolver.ResolveAndNudge(context.Background(),
		"jira-pe-1234-abcd", "PE-1234", "gardenio", "task-1", "fix the bug")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	spawned := daemon.getSpawned()
	if len(spawned) != 1 {
		t.Fatalf("expected 1 spawn for dead agent, got %d", len(spawned))
	}
	if spawned[0].AgentName != "jira-pe-1234-abcd" {
		t.Errorf("expected same agent name, got %q", spawned[0].AgentName)
	}
	if spawned[0].Extra["jira_key"] != "PE-1234" {
		t.Errorf("expected jira_key=PE-1234, got %q", spawned[0].Extra["jira_key"])
	}
}

func TestJiraAgentResolver_SpawnUsesRoleFromMatchingTrigger(t *testing.T) {
	daemon := &mockJiraResolverDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{},
		spawnID:    "new-agent-bead",
		triggerBeads: []*beadsapi.BeadDetail{
			{Fields: map[string]string{
				"action":  "spawn_agent",
				"project": "monorepo",
				"role":    "monorepo-gardener",
				"enabled": "true",
			}},
		},
	}

	resolver := NewJiraAgentResolver(JiraAgentResolverConfig{
		Daemon: daemon,
		Nudge:  subscriberNudge(daemon.findAnyAgent),
		Logger: jiraResolverTestLogger(),
	})

	err := resolver.ResolveAndNudge(context.Background(),
		"jira-pe-9999-zzzz", "PE-9999", "monorepo", "task-9", "do the thing")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	spawned := daemon.getSpawned()
	if len(spawned) != 1 {
		t.Fatalf("expected 1 spawn, got %d", len(spawned))
	}
	if spawned[0].Role != "monorepo-gardener" {
		t.Errorf("role = %q, want monorepo-gardener (derived from trigger bead)", spawned[0].Role)
	}
}

func TestJiraAgentResolver_SpawnNotFound(t *testing.T) {
	// Agent bead not found at all.
	daemon := &mockJiraResolverDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{},
		spawnID:    "new-agent-bead",
		triggerBeads: []*beadsapi.BeadDetail{
			{Fields: map[string]string{
				"action": "spawn_agent", "project": "gardenio",
				"role": "gardenio-crew", "enabled": "true",
			}},
		},
	}

	resolver := NewJiraAgentResolver(JiraAgentResolverConfig{
		Daemon: daemon,
		Nudge:  subscriberNudge(daemon.findAnyAgent),
		Logger: jiraResolverTestLogger(),
	})

	err := resolver.ResolveAndNudge(context.Background(),
		"jira-pe-5678-wxyz", "PE-5678", "gardenio", "task-2", "do the thing")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(daemon.getSpawned()) != 1 {
		t.Fatalf("expected 1 spawn for not-found agent, got %d", len(daemon.getSpawned()))
	}
}

func TestJiraAgentResolver_SpawnCooldown(t *testing.T) {
	daemon := &mockJiraResolverDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{},
		spawnID:    "agent-1",
		triggerBeads: []*beadsapi.BeadDetail{
			{Fields: map[string]string{
				"action": "spawn_agent", "project": "gardenio",
				"role": "gardenio-crew", "enabled": "true",
			}},
		},
	}

	resolver := NewJiraAgentResolver(JiraAgentResolverConfig{
		Daemon: daemon,
		Nudge:  subscriberNudge(daemon.findAnyAgent),
		Logger: jiraResolverTestLogger(),
	})

	// First call spawns.
	err := resolver.ResolveAndNudge(context.Background(),
		"jira-pe-1234-abcd", "PE-1234", "gardenio", "task-1", "first mention")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(daemon.getSpawned()) != 1 {
		t.Fatalf("expected 1 spawn, got %d", len(daemon.getSpawned()))
	}

	// Second call within cooldown should be suppressed.
	err = resolver.ResolveAndNudge(context.Background(),
		"jira-pe-1234-abcd", "PE-1234", "gardenio", "task-1", "second mention")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(daemon.getSpawned()) != 1 {
		t.Fatalf("expected still 1 spawn (cooldown active), got %d", len(daemon.getSpawned()))
	}
}

func TestJiraAgentResolver_SpawnFailsWithoutTrigger(t *testing.T) {
	// No trigger bead configured → spawn must fail, no hardcoded fallback.
	daemon := &mockJiraResolverDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{},
		spawnID:    "new-agent-bead",
	}

	resolver := NewJiraAgentResolver(JiraAgentResolverConfig{
		Daemon: daemon,
		Nudge:  subscriberNudge(daemon.findAnyAgent),
		Logger: jiraResolverTestLogger(),
	})

	err := resolver.ResolveAndNudge(context.Background(),
		"jira-pe-1234-abcd", "PE-1234", "gardenio", "task-1", "prompt")

	if err == nil {
		t.Fatal("expected error when no trigger bead configured")
	}
	if len(daemon.getSpawned()) != 0 {
		t.Errorf("expected no spawn without trigger, got %d", len(daemon.getSpawned()))
	}
}

func TestJiraAgentResolver_EmptyAgentName(t *testing.T) {
	resolver := NewJiraAgentResolver(JiraAgentResolverConfig{
		Logger: jiraResolverTestLogger(),
	})

	err := resolver.ResolveAndNudge(context.Background(),
		"", "PE-1234", "gardenio", "task-1", "prompt")

	if err == nil {
		t.Fatal("expected error for empty agent name")
	}
}

func TestJiraAgentResolver_FindActiveAgentForJiraKey(t *testing.T) {
	daemon := &mockJiraResolverDaemon{
		activeAgents: []*beadsapi.BeadDetail{
			{
				ID:    "agent-bead-1",
				Title: "jira-pe-1234-abcd",
				Fields: map[string]string{
					"agent":    "jira-pe-1234-abcd",
					"jira_key": "PE-1234",
					"coop_url": "http://localhost:9999",
				},
			},
			{
				ID:    "agent-bead-2",
				Title: "jira-pe-5678-wxyz",
				Fields: map[string]string{
					"agent":       "jira-pe-5678-wxyz",
					"jira_key":    "PE-5678",
					"agent_state": "done", // terminal → dead
				},
			},
		},
	}

	resolver := NewJiraAgentResolver(JiraAgentResolverConfig{
		Daemon: daemon,
		Logger: jiraResolverTestLogger(),
	})

	// Should find the alive agent for PE-1234.
	name, _, found := resolver.FindActiveAgentForJiraKey(context.Background(), "PE-1234")
	if !found {
		t.Fatal("expected to find active agent for PE-1234")
	}
	if name != "jira-pe-1234-abcd" {
		t.Errorf("agent name = %q", name)
	}

	// PE-5678 has a bead but it is terminal → not found.
	_, _, found = resolver.FindActiveAgentForJiraKey(context.Background(), "PE-5678")
	if found {
		t.Error("expected not to find agent for PE-5678 (terminal)")
	}

	// PE-9999 has no bead at all.
	_, _, found = resolver.FindActiveAgentForJiraKey(context.Background(), "PE-9999")
	if found {
		t.Error("expected not to find agent for PE-9999")
	}
}

func TestJiraAgentResolver_NudgeAliveAgent_CoopURLInNotes(t *testing.T) {
	// Test that coop_url from notes is also checked (like gitlab resolver).
	var nudgeReceived bool
	coopServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nudgeReceived = true
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"delivered":true}`)
	}))
	defer coopServer.Close()

	// Notes uses line-based "key: value" format (parsed by beadsapi.ParseNotes).
	notes := "coop_url: " + coopServer.URL

	daemon := &mockJiraResolverDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{
			"jira-pe-1234-abcd": {
				ID:     "agent-bead-1",
				Title:  "jira-pe-1234-abcd",
				Fields: map[string]string{"agent": "jira-pe-1234-abcd"},
				Notes:  notes,
			},
		},
		spawnID: "new-agent-bead",
	}

	resolver := NewJiraAgentResolver(JiraAgentResolverConfig{
		Daemon: daemon,
		Nudge:  subscriberNudge(daemon.findAnyAgent),
		Logger: jiraResolverTestLogger(),
	})

	err := resolver.ResolveAndNudge(context.Background(),
		"jira-pe-1234-abcd", "PE-1234", "gardenio", "task-1", "fix it")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !nudgeReceived {
		t.Error("expected nudge via coop_url from notes")
	}
	if len(daemon.getSpawned()) != 0 {
		t.Error("expected no spawn when agent is alive via notes")
	}
}

func TestJiraAgentResolver_NudgeActiveAgentForSameKey(t *testing.T) {
	// Agent named differently but already bound to the same JIRA key.
	var nudgeReceived bool
	coopServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nudgeReceived = true
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"delivered":true}`)
	}))
	defer coopServer.Close()

	daemon := &mockJiraResolverDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{},
		activeAgents: []*beadsapi.BeadDetail{
			{
				ID:    "agent-bead-other",
				Title: "gardenio-worker-1",
				Fields: map[string]string{
					"agent":    "gardenio-worker-1",
					"jira_key": "PE-1234",
					"coop_url": coopServer.URL,
				},
			},
		},
		spawnID: "new-agent",
	}

	resolver := NewJiraAgentResolver(JiraAgentResolverConfig{
		Daemon: daemon,
		Nudge:  subscriberNudge(daemon.findAnyAgent),
		Logger: jiraResolverTestLogger(),
	})

	// Request agent "new-name" but PE-1234 already has an active agent.
	err := resolver.ResolveAndNudge(context.Background(),
		"new-name", "PE-1234", "gardenio", "task-1", "do it")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !nudgeReceived {
		t.Error("expected nudge to existing active agent for same JIRA key")
	}
	if len(daemon.getSpawned()) != 0 {
		t.Error("expected no spawn when active agent exists for JIRA key")
	}
}

// TestJiraAgentResolver_InheritsHumanOperatorFromTaskBead verifies the
// respawn path stamps the human operator carried on the mention task bead onto
// the spawned agent (human-operator invariant, task ka-0000002839). Before the
// fix this path omitted operated_by entirely, booting the agent unattributed.
func TestJiraAgentResolver_InheritsHumanOperatorFromTaskBead(t *testing.T) {
	daemon := &mockJiraResolverDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{},
		spawnID:    "new-agent-bead",
		triggerBeads: []*beadsapi.BeadDetail{
			{Fields: map[string]string{
				"action": "spawn_agent", "project": "gardenio",
				"role": "crew", "enabled": "true",
			}},
		},
		taskBeads: map[string]*beadsapi.BeadDetail{
			"task-1": {ID: "task-1", Fields: map[string]string{
				"operated_by": "slack:U0OPERATOR1",
			}},
		},
	}

	resolver := NewJiraAgentResolver(JiraAgentResolverConfig{
		Daemon: daemon,
		Nudge:  subscriberNudge(daemon.findAnyAgent),
		Logger: jiraResolverTestLogger(),
	})

	if err := resolver.ResolveAndNudge(context.Background(),
		"jira-pe-1234-abcd", "PE-1234", "gardenio", "task-1", "fix the bug"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	spawned := daemon.getSpawned()
	if len(spawned) != 1 {
		t.Fatalf("expected 1 spawn, got %d", len(spawned))
	}
	if got := spawned[0].Extra["operated_by"]; got != "slack:U0OPERATOR1" {
		t.Errorf("operated_by = %q; want slack:U0OPERATOR1 inherited from task bead", got)
	}
}

// TestJiraAgentResolver_EnforceRefusesNonHuman verifies that in enforce mode a
// respawn whose task bead carries no resolvable human operator is refused
// before any agent is spawned.
func TestJiraAgentResolver_EnforceRefusesNonHuman(t *testing.T) {
	t.Setenv("GARDENIO_REQUIRE_HUMAN_OPERATOR", "enforce")

	daemon := &mockJiraResolverDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{},
		spawnID:    "new-agent-bead",
		triggerBeads: []*beadsapi.BeadDetail{
			{Fields: map[string]string{
				"action": "spawn_agent", "project": "gardenio",
				"role": "crew", "enabled": "true",
			}},
		},
		taskBeads: map[string]*beadsapi.BeadDetail{
			"task-1": {ID: "task-1", Fields: map[string]string{
				"operated_by": "fleet:jira-bridge", // non-human sentinel
			}},
		},
	}

	resolver := NewJiraAgentResolver(JiraAgentResolverConfig{
		Daemon: daemon,
		Nudge:  subscriberNudge(daemon.findAnyAgent),
		Logger: jiraResolverTestLogger(),
	})

	err := resolver.ResolveAndNudge(context.Background(),
		"jira-pe-1234-abcd", "PE-1234", "gardenio", "task-1", "fix the bug")
	if err == nil {
		t.Fatalf("expected refusal in enforce mode, got nil")
	}
	if len(daemon.getSpawned()) != 0 {
		t.Errorf("expected no spawn on enforce refusal, got %d", len(daemon.getSpawned()))
	}
}

// TestJiraAgentResolver_DefaultOperatorFallback pins the origin-bug fix
// (ka-0000002128): when the mention task bead carries no walkable human
// operator (only a non-human jira:/fleet: sentinel) AND a designated default
// operator is configured, the respawn is attributed to that human so the
// agent boots with BEADS_OPERATED_BY and can file escalations/squawks.
func TestJiraAgentResolver_DefaultOperatorFallback(t *testing.T) {
	daemon := &mockJiraResolverDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{},
		spawnID:    "new-agent-bead",
		triggerBeads: []*beadsapi.BeadDetail{
			{Fields: map[string]string{
				"action": "spawn_agent", "project": "gardenio",
				"role": "crew", "enabled": "true",
			}},
		},
		taskBeads: map[string]*beadsapi.BeadDetail{
			"task-1": {ID: "task-1", Fields: map[string]string{
				"operated_by": "fleet:jira-bridge", // non-human sentinel
			}},
		},
	}

	resolver := NewJiraAgentResolver(JiraAgentResolverConfig{
		Daemon:          daemon,
		Nudge:           subscriberNudge(daemon.findAnyAgent),
		Logger:          jiraResolverTestLogger(),
		DefaultOperator: "oidc:matthew.baker@acme.example",
	})

	if err := resolver.ResolveAndNudge(context.Background(),
		"jira-pe-1234-abcd", "PE-1234", "gardenio", "task-1", "fix the bug"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	spawned := daemon.getSpawned()
	if len(spawned) != 1 {
		t.Fatalf("expected 1 spawn, got %d", len(spawned))
	}
	if got, want := spawned[0].Extra["operated_by"], "oidc:matthew.baker@acme.example"; got != want {
		t.Errorf("operated_by = %q; want %q (designated default operator)", got, want)
	}
}

// TestJiraAgentResolver_DefaultOperatorSatisfiesEnforce verifies the
// designated default operator clears the enforce-mode gate even when the task
// bead carries no human operator — the fallback is a real human, so the
// respawn proceeds rather than being refused.
func TestJiraAgentResolver_DefaultOperatorSatisfiesEnforce(t *testing.T) {
	t.Setenv("GARDENIO_REQUIRE_HUMAN_OPERATOR", "enforce")

	daemon := &mockJiraResolverDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{},
		spawnID:    "new-agent-bead",
		triggerBeads: []*beadsapi.BeadDetail{
			{Fields: map[string]string{
				"action": "spawn_agent", "project": "gardenio",
				"role": "crew", "enabled": "true",
			}},
		},
		taskBeads: map[string]*beadsapi.BeadDetail{
			"task-1": {ID: "task-1", Fields: map[string]string{}},
		},
	}

	resolver := NewJiraAgentResolver(JiraAgentResolverConfig{
		Daemon:          daemon,
		Nudge:           subscriberNudge(daemon.findAnyAgent),
		Logger:          jiraResolverTestLogger(),
		DefaultOperator: "oidc:matthew.baker@acme.example",
	})

	if err := resolver.ResolveAndNudge(context.Background(),
		"jira-pe-1234-abcd", "PE-1234", "gardenio", "task-1", "fix the bug"); err != nil {
		t.Fatalf("expected spawn to proceed with default operator, got error: %v", err)
	}
	spawned := daemon.getSpawned()
	if len(spawned) != 1 {
		t.Fatalf("expected 1 spawn, got %d", len(spawned))
	}
	if got, want := spawned[0].Extra["operated_by"], "oidc:matthew.baker@acme.example"; got != want {
		t.Errorf("operated_by = %q; want %q", got, want)
	}
}

// TestJiraAgentResolver_NonHumanDefaultOperatorIgnored verifies the
// construction guard drops a misconfigured non-human default operator so it
// cannot reintroduce the daemon-rejected shape (ka-0000002128). With the
// default dropped and no human on the task bead, enforce mode refuses.
func TestJiraAgentResolver_NonHumanDefaultOperatorIgnored(t *testing.T) {
	t.Setenv("GARDENIO_REQUIRE_HUMAN_OPERATOR", "enforce")

	daemon := &mockJiraResolverDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{},
		spawnID:    "new-agent-bead",
		triggerBeads: []*beadsapi.BeadDetail{
			{Fields: map[string]string{
				"action": "spawn_agent", "project": "gardenio",
				"role": "crew", "enabled": "true",
			}},
		},
		taskBeads: map[string]*beadsapi.BeadDetail{
			"task-1": {ID: "task-1", Fields: map[string]string{}},
		},
	}

	resolver := NewJiraAgentResolver(JiraAgentResolverConfig{
		Daemon:          daemon,
		Nudge:           subscriberNudge(daemon.findAnyAgent),
		Logger:          jiraResolverTestLogger(),
		DefaultOperator: "fleet:jira-bridge", // non-human → ignored at construction
	})

	err := resolver.ResolveAndNudge(context.Background(),
		"jira-pe-1234-abcd", "PE-1234", "gardenio", "task-1", "fix the bug")
	if err == nil {
		t.Fatalf("expected refusal in enforce mode (non-human default ignored), got nil")
	}
	if len(daemon.getSpawned()) != 0 {
		t.Errorf("expected no spawn, got %d", len(daemon.getSpawned()))
	}
}

// TestJiraAgentResolver_TerminalStateAgent_ClosesAndRespawns is the core F3
// scenario (plan ka-0000007401): the assigned agent STOPPED (gb stop / crash)
// — its bead lingers in an active status with agent_state=done and a stale
// coop_url. A follow-up must NOT nudge the dead pod; it must close the stale
// bead (so SpawnAgent's name-dedup can't return it) and re-spawn under the
// SAME name so the entrypoint resumes the prior session.
func TestJiraAgentResolver_TerminalStateAgent_ClosesAndRespawns(t *testing.T) {
	daemon := &mockJiraResolverDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{
			"jira-pe-1234-abcd": {
				ID:    "agent-bead-stale",
				Title: "jira-pe-1234-abcd",
				Notes: "coop_url: http://10.0.0.9:8080", // stale — pod is gone
				Fields: map[string]string{
					"agent":       "jira-pe-1234-abcd",
					"agent_state": "done",
				},
			},
		},
		spawnID:      "new-agent-bead",
		triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
	}

	resolver := NewJiraAgentResolver(JiraAgentResolverConfig{
		Daemon: daemon,
		Nudge:  subscriberNudge(daemon.findAnyAgent),
		Logger: jiraResolverTestLogger(),
	})

	err := resolver.ResolveAndNudgeFollowUp(context.Background(),
		"jira-pe-1234-abcd", "PE-1234", "gardenio", "task-1", "70001", "operator follow-up", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The stale terminal bead must be closed before the same-name respawn.
	closed := daemon.getClosed()
	if len(closed) != 1 || closed[0] != "agent-bead-stale" {
		t.Errorf("closed beads = %v; want [agent-bead-stale]", closed)
	}

	spawned := daemon.getSpawned()
	if len(spawned) != 1 {
		t.Fatalf("expected 1 respawn for terminal-state agent, got %d", len(spawned))
	}
	if spawned[0].AgentName != "jira-pe-1234-abcd" {
		t.Errorf("respawn name = %q; want same name for session resume", spawned[0].AgentName)
	}
	if got := spawned[0].Extra["jira_comment_id"]; got != "70001" {
		t.Errorf("jira_comment_id = %q; want 70001 stamped on respawned agent", got)
	}
}

// TestJiraAgentResolver_TerminalStateAgent_CloseFailureStillSpawns pins the
// best-effort posture: a CloseBead failure is logged, not fatal — the respawn
// still proceeds (matching respawnThreadAgent).
func TestJiraAgentResolver_TerminalStateAgent_CloseFailureStillSpawns(t *testing.T) {
	daemon := &mockJiraResolverDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{
			"jira-pe-1234-abcd": {
				ID:    "agent-bead-stale",
				Title: "jira-pe-1234-abcd",
				Fields: map[string]string{
					"agent":       "jira-pe-1234-abcd",
					"agent_state": "failed",
				},
			},
		},
		spawnID:      "new-agent-bead",
		triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
		closeBeadErr: fmt.Errorf("daemon unavailable"),
	}

	resolver := NewJiraAgentResolver(JiraAgentResolverConfig{
		Daemon: daemon,
		Nudge:  subscriberNudge(daemon.findAnyAgent),
		Logger: jiraResolverTestLogger(),
	})

	err := resolver.ResolveAndNudgeFollowUp(context.Background(),
		"jira-pe-1234-abcd", "PE-1234", "gardenio", "task-1", "70002", "follow-up", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(daemon.getSpawned()) != 1 {
		t.Fatalf("expected respawn despite close failure, got %d spawns", len(daemon.getSpawned()))
	}
}

// TestJiraAgentResolver_NoCommentID_OmitsField verifies the plain
// ResolveAndNudge path (mention resolver, no comment context) does not stamp
// an empty jira_comment_id.
func TestJiraAgentResolver_NoCommentID_OmitsField(t *testing.T) {
	daemon := &mockJiraResolverDaemon{
		agentBeads:   map[string]*beadsapi.BeadDetail{},
		spawnID:      "new-agent-bead",
		triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
	}

	resolver := NewJiraAgentResolver(JiraAgentResolverConfig{
		Daemon: daemon,
		Nudge:  subscriberNudge(daemon.findAnyAgent),
		Logger: jiraResolverTestLogger(),
	})

	if err := resolver.ResolveAndNudge(context.Background(),
		"jira-pe-1234-abcd", "PE-1234", "gardenio", "task-1", "prompt"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	spawned := daemon.getSpawned()
	if len(spawned) != 1 {
		t.Fatalf("expected 1 spawn, got %d", len(spawned))
	}
	if _, present := spawned[0].Extra["jira_comment_id"]; present {
		t.Error("jira_comment_id should be omitted when no comment id is supplied")
	}
}

// TestJiraAgentResolver_SkipsTerminalAgentForSameKey verifies the
// same-jira-key reuse scan does not nudge a terminal-state agent whose stale
// coop_url lingers on the bead — that nudge would silently vanish.
func TestJiraAgentResolver_SkipsTerminalAgentForSameKey(t *testing.T) {
	daemon := &mockJiraResolverDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{},
		activeAgents: []*beadsapi.BeadDetail{
			{
				ID:    "agent-bead-dead",
				Title: "gardenio-worker-1",
				Fields: map[string]string{
					"agent":       "gardenio-worker-1",
					"jira_key":    "PE-1234",
					"coop_url":    "http://10.0.0.9:8080", // stale
					"agent_state": "done",
				},
			},
		},
		spawnID:      "new-agent-bead",
		triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
	}

	resolver := NewJiraAgentResolver(JiraAgentResolverConfig{
		Daemon: daemon,
		Nudge:  subscriberNudge(daemon.findAnyAgent),
		Logger: jiraResolverTestLogger(),
	})

	err := resolver.ResolveAndNudgeFollowUp(context.Background(),
		"jira-new-name", "PE-1234", "gardenio", "task-1", "70003", "follow-up", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The terminal agent must NOT absorb the follow-up as a nudge; a fresh
	// spawn must happen instead.
	if len(daemon.getSpawned()) != 1 {
		t.Fatalf("expected 1 spawn (terminal same-key agent skipped), got %d", len(daemon.getSpawned()))
	}
}

// TestJiraAgentResolver_FreshRespawn_StampsCleanStart pins the --fresh
// continuity levers (ka-0000008188, Slack parity) on the RESPAWN path: a
// follow-up carrying --fresh must respawn the stopped agent with a CLEAN
// conversation, which means (1) session_resume=false stamped on the spawned
// agent bead (podspec BOAT_SESSION_RESUME=0 → findResumeSession "" + shouldThaw
// veto) and (2) the worked task bead's resume_s3_key breadcrumb cleared so
// resolveCanonicalS3Key does not adopt the ORIGINAL transcript prefix at boot.
func TestJiraAgentResolver_FreshRespawn_StampsCleanStart(t *testing.T) {
	daemon := &mockJiraResolverDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{
			"jira-pe-1234-abcd": {
				ID:    "agent-bead-stale",
				Title: "jira-pe-1234-abcd",
				Fields: map[string]string{
					"agent":       "jira-pe-1234-abcd",
					"agent_state": "done", // stopped → respawn
				},
			},
		},
		// The worked task bead carries a canonical resume breadcrumb (as a prior
		// involuntary death would have stamped). --fresh must clear it.
		taskBeads: map[string]*beadsapi.BeadDetail{
			"task-1": {
				ID:     "task-1",
				Fields: map[string]string{"resume_s3_key": "agent-bead-original"},
			},
		},
		spawnID:      "new-agent-bead",
		triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
	}

	resolver := NewJiraAgentResolver(JiraAgentResolverConfig{
		Daemon: daemon,
		Nudge:  subscriberNudge(daemon.findAnyAgent),
		Logger: jiraResolverTestLogger(),
	})

	err := resolver.ResolveAndNudgeFollowUp(context.Background(),
		"jira-pe-1234-abcd", "PE-1234", "gardenio", "task-1", "70010", "please continue --fresh", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spawned := daemon.getSpawned()
	if len(spawned) != 1 {
		t.Fatalf("expected 1 respawn, got %d", len(spawned))
	}
	// Lever 1: session_resume=false on the respawned agent bead.
	if got := spawned[0].Extra["session_resume"]; got != "false" {
		t.Errorf("session_resume = %q on --fresh respawn; want \"false\"", got)
	}
	// The comment anchor still threads through so gb jira reply targets the
	// waking comment even on a fresh conversation.
	if got := spawned[0].Extra["jira_comment_id"]; got != "70010" {
		t.Errorf("jira_comment_id = %q; want 70010 stamped on --fresh respawn", got)
	}
	// Lever 2: the task bead's resume_s3_key breadcrumb is cleared.
	var clearedBreadcrumb bool
	for _, u := range daemon.getUpdatedFields() {
		if u.BeadID == "task-1" {
			if v, ok := u.Fields["resume_s3_key"]; ok && v == "" {
				clearedBreadcrumb = true
			}
		}
	}
	if !clearedBreadcrumb {
		t.Errorf("--fresh respawn must clear resume_s3_key on the task bead; updates=%+v",
			daemon.getUpdatedFields())
	}
}

// TestJiraAgentResolver_NonFreshRespawn_NoCleanStart is the control for
// TestJiraAgentResolver_FreshRespawn_StampsCleanStart: an ordinary (no --fresh)
// respawn must NOT stamp session_resume and must NOT clear the resume_s3_key
// breadcrumb, so session continuity is preserved exactly as before.
func TestJiraAgentResolver_NonFreshRespawn_NoCleanStart(t *testing.T) {
	daemon := &mockJiraResolverDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{
			"jira-pe-1234-abcd": {
				ID:    "agent-bead-stale",
				Title: "jira-pe-1234-abcd",
				Fields: map[string]string{
					"agent":       "jira-pe-1234-abcd",
					"agent_state": "done",
				},
			},
		},
		taskBeads: map[string]*beadsapi.BeadDetail{
			"task-1": {
				ID:     "task-1",
				Fields: map[string]string{"resume_s3_key": "agent-bead-original"},
			},
		},
		spawnID:      "new-agent-bead",
		triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
	}

	resolver := NewJiraAgentResolver(JiraAgentResolverConfig{
		Daemon: daemon,
		Nudge:  subscriberNudge(daemon.findAnyAgent),
		Logger: jiraResolverTestLogger(),
	})

	err := resolver.ResolveAndNudgeFollowUp(context.Background(),
		"jira-pe-1234-abcd", "PE-1234", "gardenio", "task-1", "70011", "please continue", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spawned := daemon.getSpawned()
	if len(spawned) != 1 {
		t.Fatalf("expected 1 respawn, got %d", len(spawned))
	}
	if _, present := spawned[0].Extra["session_resume"]; present {
		t.Errorf("session_resume must be absent on a non-fresh respawn; extra=%+v", spawned[0].Extra)
	}
	for _, u := range daemon.getUpdatedFields() {
		if u.BeadID == "task-1" {
			if _, ok := u.Fields["resume_s3_key"]; ok {
				t.Errorf("non-fresh respawn must not touch resume_s3_key; update=%+v", u.Fields)
			}
		}
	}
}

// TestJiraAgentResolver_FreshLiveAgent_InertNudge pins that --fresh is INERT on
// the live-agent path (ka-0000008188): a running pod holds its conversation in
// memory and cannot be told to forget it, so the follow-up is delivered as a
// normal nudge with NO spawn and NO session_resume stamp. (The webhook posts a
// separate operator note telling them to kill + re-mention for a clean slate.)
func TestJiraAgentResolver_FreshLiveAgent_InertNudge(t *testing.T) {
	var nudgeReceived bool
	coopServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nudgeReceived = true
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"delivered":true}`)
	}))
	defer coopServer.Close()

	daemon := &mockJiraResolverDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{
			"jira-pe-1234-abcd": {
				ID:    "agent-bead-live",
				Title: "jira-pe-1234-abcd",
				Fields: map[string]string{
					"agent":    "jira-pe-1234-abcd",
					"coop_url": coopServer.URL, // alive
				},
			},
		},
	}

	resolver := NewJiraAgentResolver(JiraAgentResolverConfig{
		Daemon: daemon,
		Nudge:  subscriberNudge(daemon.findAnyAgent),
		Logger: jiraResolverTestLogger(),
	})

	err := resolver.ResolveAndNudgeFollowUp(context.Background(),
		"jira-pe-1234-abcd", "PE-1234", "gardenio", "task-1", "70012", "keep going --fresh", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !nudgeReceived {
		t.Error("expected the live agent to be nudged even with --fresh")
	}
	if len(daemon.getSpawned()) != 0 {
		t.Errorf("--fresh must NOT spawn when the agent is alive; got %d spawns", len(daemon.getSpawned()))
	}
}
