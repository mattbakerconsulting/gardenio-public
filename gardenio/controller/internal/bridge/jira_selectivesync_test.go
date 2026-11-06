package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// This file holds the integration tests for the selective-sync controls
// (ka-0000001679): the single coordinated FSM toggle honored by all four
// outbound paths, and the bead→ticket scoping guard. The pure-policy unit tests
// live in jira_project_gate_test.go and jira_scope_test.go.

// countingJiraServer returns an httptest server that records every outbound
// Jira write (comment / transition / remotelink / assignee) and a getter for
// the count. Any path under /rest/api/3/issue/ that mutates is counted.
func countingJiraServer(t *testing.T) (*httptest.Server, func() int) {
	t.Helper()
	var (
		mu    sync.Mutex
		calls int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			// Transition lookups are reads; return an empty-but-valid set so a
			// transition attempt would still need a following POST (counted).
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"transitions":[{"id":"31","name":"In Progress","to":{"name":"In Progress"}},{"id":"41","name":"Review","to":{"name":"Review"}}]}`)
		default: // POST/PUT = a mutation
			calls++
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":"1"}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() int { mu.Lock(); defer mu.Unlock(); return calls }
}

// --- Feature 1: single coordinated FSM toggle across all four paths ---

// TestSelectiveSync_FSMToggle_SyncPath proves the sync-back path writes to FSM
// ONLY when the coordinated toggle is on. Off ⇒ 0 writes (legacy behavior);
// on ⇒ writes flow.
func TestSelectiveSync_FSMToggle_SyncPath(t *testing.T) {
	makeEvent := func() []byte {
		return marshalSSEBeadPayload(BeadEvent{
			ID: "bd-fsm-sync", Type: "task", Title: "[FSM-100] Customer request",
			Status:   "in_progress",
			Assignee: "agent-triage-1",
			Labels:   []string{"source:jira", "jira:FSM-100"},
			Fields:   map[string]string{"jira_key": "FSM-100"},
		})
	}

	t.Run("toggle off blocks FSM", func(t *testing.T) {
		srv, count := countingJiraServer(t)
		s := NewJiraSync(JiraSyncConfig{
			Jira:        newTestJiraClient(srv.URL),
			Logger:      slog.Default(),
			ProjectGate: DefaultProjectSyncGate(false),
		})
		s.handleUpdated(context.Background(), makeEvent())
		if n := count(); n != 0 {
			t.Errorf("FSM off: expected 0 Jira writes, got %d", n)
		}
	})

	t.Run("toggle on permits FSM", func(t *testing.T) {
		srv, count := countingJiraServer(t)
		s := NewJiraSync(JiraSyncConfig{
			Jira:                  newTestJiraClient(srv.URL),
			Logger:                slog.Default(),
			ProjectGate:           DefaultProjectSyncGate(true),
			EnableClaimTransition: true,
		})
		s.handleUpdated(context.Background(), makeEvent())
		if n := count(); n == 0 {
			t.Error("FSM on: expected Jira writes to flow, got 0")
		}
	})
}

// TestSelectiveSync_FSMToggle_MentionPath proves the mention path spawns an FSM
// agent ONLY when the toggle is on.
func TestSelectiveSync_FSMToggle_MentionPath(t *testing.T) {
	build := func(fsmOn bool) (*JiraMentionHandler, *mockJiraMentionDaemon) {
		daemon := &mockJiraMentionDaemon{
			createID:     "task-1",
			spawnID:      "agent-1",
			triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
		}
		h := NewJiraMentionHandler(JiraMentionHandlerConfig{
			Users:       allowJiraMentionUsers{},
			Daemon:      daemon,
			Project:     "gardenio",
			Logger:      jiraMentionTestLogger(),
			ProjectGate: DefaultProjectSyncGate(fsmOn),
		})
		return h, daemon
	}

	t.Run("toggle off blocks FSM mention", func(t *testing.T) {
		h, daemon := build(false)
		event := makeJiraMentionEvent("FSM-100", "Customer ticket", "@gardenio fix this", "User")
		h.HandleMention(context.Background(), event)
		if len(daemon.created) != 0 || len(daemon.spawned) != 0 {
			t.Errorf("FSM off: expected no bead/agent, got created=%d spawned=%d",
				len(daemon.created), len(daemon.spawned))
		}
	})

	t.Run("toggle on permits FSM mention", func(t *testing.T) {
		h, daemon := build(true)
		event := makeJiraMentionEvent("FSM-100", "Customer ticket", "@gardenio fix this", "User")
		h.HandleMention(context.Background(), event)
		if len(daemon.created) != 1 || len(daemon.spawned) != 1 {
			t.Errorf("FSM on: expected 1 bead + 1 spawn, got created=%d spawned=%d",
				len(daemon.created), len(daemon.spawned))
		}
	})
}

// TestSelectiveSync_FSMToggle_SquawkPath proves the squawk forwarder posts to
// an FSM ticket ONLY when the toggle is on.
func TestSelectiveSync_FSMToggle_SquawkPath(t *testing.T) {
	build := func(fsmOn bool) (*JiraSquawkForwarder, *mockJiraSquawkJira) {
		daemon := &mockJiraSquawkDaemon{
			agentBeads: map[string]*beadsapi.BeadDetail{
				"test-agent": {
					ID:     "bd-test-agent",
					Title:  "test-agent",
					Fields: map[string]string{"jira_key": "FSM-100"},
				},
			},
		}
		jira := &mockJiraSquawkJira{}
		f := NewJiraSquawkForwarder(JiraSquawkForwarderConfig{
			Daemon:      daemon,
			Jira:        jira,
			Logger:      jiraSquawkTestLogger(),
			ProjectGate: DefaultProjectSyncGate(fsmOn),
		})
		return f, jira
	}
	event := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-squawk-fsm",
		Type:   "message",
		Labels: []string{"squawk", "from:test-agent"},
		Fields: map[string]string{"source_agent": "test-agent", "text": "status note"},
	})

	t.Run("toggle off blocks FSM squawk", func(t *testing.T) {
		f, jira := build(false)
		f.handleClosed(context.Background(), event)
		if n := len(jira.getComments()); n != 0 {
			t.Errorf("FSM off: expected 0 comments, got %d", n)
		}
	})

	t.Run("toggle on permits FSM squawk", func(t *testing.T) {
		f, jira := build(true)
		f.handleClosed(context.Background(), event)
		if n := len(jira.getComments()); n != 1 {
			t.Errorf("FSM on: expected 1 comment, got %d", n)
		}
	})
}

// TestSelectiveSync_FSMToggle_PollerPath proves the poller back-off comment
// reaches FSM ONLY when the toggle is on, and that bead-close + tracked-delete
// still run regardless (the toggle suppresses only the outbound comment).
func TestSelectiveSync_FSMToggle_PollerPath(t *testing.T) {
	run := func(fsmOn bool) int {
		p, _, count := pollerBackoffServer(t, "FSM-100")
		p.cfg.ProjectGate = DefaultProjectSyncGate(fsmOn)
		p.checkAssigneeBatch(context.Background(), []string{"FSM-100"})
		return count()
	}

	if n := run(false); n != 0 {
		t.Errorf("FSM off: expected 0 back-off comments, got %d", n)
	}
	if n := run(true); n != 1 {
		t.Errorf("FSM on: expected 1 back-off comment, got %d", n)
	}
}

// TestSelectiveSync_OneGateAllPaths proves the single shared gate is what each
// path consults: constructing every path with the SAME on-gate value flips FSM
// write-back on everywhere at once (the atomicity property — ka-0000001679).
func TestSelectiveSync_OneGateAllPaths(t *testing.T) {
	gate := DefaultProjectSyncGate(true)

	// sync
	srv, count := countingJiraServer(t)
	s := NewJiraSync(JiraSyncConfig{Jira: newTestJiraClient(srv.URL), Logger: slog.Default(), ProjectGate: gate})
	s.handleClosed(context.Background(), marshalSSEBeadPayload(BeadEvent{
		ID: "bd-x", Type: "task", Title: "[FSM-7] x", Status: "closed",
		Labels: []string{"source:jira", "jira:FSM-7"}, Fields: map[string]string{"jira_key": "FSM-7"},
	}))
	if count() == 0 {
		t.Error("shared on-gate: sync path should write FSM")
	}

	// squawk
	sd := &mockJiraSquawkDaemon{agentBeads: map[string]*beadsapi.BeadDetail{
		"a": {ID: "bd-a", Title: "a", Fields: map[string]string{"jira_key": "FSM-7"}},
	}}
	sj := &mockJiraSquawkJira{}
	f := NewJiraSquawkForwarder(JiraSquawkForwarderConfig{Daemon: sd, Jira: sj, Logger: jiraSquawkTestLogger(), ProjectGate: gate})
	f.handleClosed(context.Background(), marshalSSEBeadPayload(BeadEvent{
		ID: "bd-sq", Type: "message", Labels: []string{"squawk", "from:a"},
		Fields: map[string]string{"source_agent": "a", "text": "hi"},
	}))
	if len(sj.getComments()) == 0 {
		t.Error("shared on-gate: squawk path should write FSM")
	}
}

// --- Feature 2: bead→ticket scoping guard ---

// TestSelectiveSync_ScopeGuard_SyncInScope proves the positive control for the
// sync-back path: a bead bound to PE-500 whose write targets PE-500 is in scope
// and the closing comment is posted. In the sync path the write target is
// derived from the bead's own binding, so the scope guard is a
// belt-and-suspenders invariant here — the independent-source mismatch case
// (real teeth) is exercised in the poller test below.
func TestSelectiveSync_ScopeGuard_SyncInScope(t *testing.T) {
	srv, count := countingJiraServer(t)
	s := NewJiraSync(JiraSyncConfig{Jira: newTestJiraClient(srv.URL), Logger: slog.Default(), ProjectGate: DefaultProjectSyncGate(false)})
	s.handleClosed(context.Background(), marshalSSEBeadPayload(BeadEvent{
		ID: "bd-scope-ok", Type: "task", Title: "[PE-500] ok", Status: "closed",
		Labels: []string{"source:jira", "jira:PE-500"}, Fields: map[string]string{"jira_key": "PE-500"},
	}))
	if count() == 0 {
		t.Error("in-scope PE close should post a closing comment")
	}
}

// TestSelectiveSync_ScopeGuard_SyncInScopeMethod exercises JiraSync.inScope
// directly so the refusal path (target ≠ the bead's bound ticket) is covered
// even though the sync handlers can only ever target the binding itself.
func TestSelectiveSync_ScopeGuard_SyncInScopeMethod(t *testing.T) {
	s := NewJiraSync(JiraSyncConfig{Jira: newTestJiraClient("https://example.com"), Logger: slog.Default()})
	bound := &BeadEvent{ID: "bd-1", Fields: map[string]string{"jira_key": "PE-100"}, Labels: []string{"jira:PE-100"}}

	if !s.inScope(bound, "PE-100") {
		t.Error("write targeting the bead's own ticket must be in scope")
	}
	if s.inScope(bound, "PE-999") {
		t.Error("write targeting a DIFFERENT ticket must be refused")
	}
	if s.inScope(bound, "FSM-1") {
		t.Error("write targeting an arbitrary ticket must be refused")
	}
	unbound := &BeadEvent{ID: "bd-2", Fields: map[string]string{}}
	if s.inScope(unbound, "PE-100") {
		t.Error("an unbound bead must not be able to write to any ticket")
	}
	if s.inScope(nil, "PE-100") {
		t.Error("a nil bead must fail closed")
	}
}

// TestSelectiveSync_ScopeGuard_PollerMismatch proves the genuinely-independent
// scope case: the poller back-off comment targets the POLLED issue key, but the
// tracked bead it found is bound to a DIFFERENT ticket. The guard must refuse
// the comment (so the bridge can't comment on a ticket the bead doesn't own),
// while still completing the close + tracked-delete bookkeeping.
func TestSelectiveSync_ScopeGuard_PollerMismatch(t *testing.T) {
	// pollerBackoffServer wires a bead bound to the SAME key as the polled
	// issue. We drift the bead's AUTHORITATIVE binding (its jira_key field) to a
	// DIFFERENT ticket while keeping the jira:PE-100 label so the poller's
	// label-scoped lookup still finds it — simulating a crafted/drifted binding.
	// boundJiraKeyFromFields prefers the field, so bound=PE-999 ≠ target PE-100.
	p, daemon, count := pollerBackoffServer(t, "PE-100")
	p.cfg.ProjectGate = DefaultProjectSyncGate(false) // PE allowed; isolate the scope check

	daemon.mu.Lock()
	b := daemon.beads["bd-backoff-PE-100"]
	b.Labels = []string{"source:jira", "jira:PE-100"}  // keep label so lookup finds it
	b.Fields = map[string]string{"jira_key": "PE-999"} // but bind the FIELD elsewhere
	daemon.mu.Unlock()

	p.checkAssigneeBatch(context.Background(), []string{"PE-100"})

	if n := count(); n != 0 {
		t.Errorf("scope mismatch: expected 0 back-off comments (target PE-100 != bound PE-999), got %d", n)
	}
	// Bookkeeping still runs: bead closed, issue dropped from tracked.
	daemon.mu.Lock()
	closed := daemon.beads["bd-backoff-PE-100"].Status == "closed"
	daemon.mu.Unlock()
	if !closed {
		t.Error("scope mismatch must still close the bead (only the comment is suppressed)")
	}
	p.mu.Lock()
	_, stillTracked := p.tracked["PE-100"]
	p.mu.Unlock()
	if stillTracked {
		t.Error("scope mismatch must still drop the issue from tracked")
	}
}

// TestSelectiveSync_ScopeGuard_PollerMatch proves the positive control: when
// the tracked bead's binding matches the polled issue key, the back-off comment
// is posted (the guard does not over-block).
func TestSelectiveSync_ScopeGuard_PollerMatch(t *testing.T) {
	p, _, count := pollerBackoffServer(t, "PE-100")
	p.cfg.ProjectGate = DefaultProjectSyncGate(false)
	p.checkAssigneeBatch(context.Background(), []string{"PE-100"})
	if n := count(); n != 1 {
		t.Errorf("in-scope PE: expected 1 back-off comment, got %d", n)
	}
}
