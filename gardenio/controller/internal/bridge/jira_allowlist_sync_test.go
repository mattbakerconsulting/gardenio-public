package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// jiraRecordingServer captures the path, method, and decoded JSON body of every
// request, so sync tests can assert which governed writes actually hit Jira.
type jiraRecordingServer struct {
	mu       sync.Mutex
	requests []recordedJiraRequest
}

type recordedJiraRequest struct {
	Method string
	Path   string
	Body   map[string]any
}

func newJiraRecordingServer() (*httptest.Server, *jiraRecordingServer) {
	rec := &jiraRecordingServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		rec.mu.Lock()
		rec.requests = append(rec.requests, recordedJiraRequest{Method: r.Method, Path: r.URL.Path, Body: body})
		rec.mu.Unlock()

		// Provide a plausible transitions list so TransitionIssue's GET resolves.
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/transitions") {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"transitions":[{"id":"31","name":"In Progress","to":{"name":"In Progress"}},{"id":"41","name":"Review","to":{"name":"Review"}}]}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":"1"}`)
	}))
	return srv, rec
}

func (rec *jiraRecordingServer) writesTo(substr string) []recordedJiraRequest {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var out []recordedJiraRequest
	for _, r := range rec.requests {
		if r.Method == http.MethodGet {
			continue // transitions lookups are reads, not writes
		}
		if strings.Contains(r.Path, substr) {
			out = append(out, r)
		}
	}
	return out
}

// roleResolverStub maps an agent name to a role, satisfying JiraSyncRoleResolver.
type roleResolverStub struct {
	roles map[string]string // agentName → role
}

func (s *roleResolverStub) FindAgentBead(_ context.Context, agentName string) (*beadsapi.BeadDetail, error) {
	role, ok := s.roles[agentName]
	if !ok {
		return nil, fmt.Errorf("agent %s not found", agentName)
	}
	return &beadsapi.BeadDetail{ID: "ag-" + agentName, Fields: map[string]string{"role": role}}, nil
}

// TestAddCommentWithVisibility_WireFormat verifies the JSM sd.public.comment
// property is set correctly for internal vs public comments (criterion 3).
func TestAddCommentWithVisibility_WireFormat(t *testing.T) {
	tests := []struct {
		name         string
		vis          CommentVisibility
		wantInternal bool
	}{
		{"internal note", CommentInternal, true},
		{"public reply", CommentPublic, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, rec := newJiraRecordingServer()
			defer srv.Close()
			c := newTestJiraClient(srv.URL)

			if err := c.AddCommentWithVisibility(context.Background(), "PE-1", "hello", tt.vis); err != nil {
				t.Fatalf("AddCommentWithVisibility: %v", err)
			}

			writes := rec.writesTo("/comment")
			if len(writes) != 1 {
				t.Fatalf("expected 1 comment write, got %d", len(writes))
			}
			props, ok := writes[0].Body["properties"].([]any)
			if !ok || len(props) == 0 {
				t.Fatalf("expected sd.public.comment property, got body: %v", writes[0].Body)
			}
			prop := props[0].(map[string]any)
			if prop["key"] != "sd.public.comment" {
				t.Errorf("property key = %v, want sd.public.comment", prop["key"])
			}
			val := prop["value"].(map[string]any)
			if got := val["internal"].(bool); got != tt.wantInternal {
				t.Errorf("internal = %v, want %v", got, tt.wantInternal)
			}
		})
	}
}

// TestJiraSync_GateDropsDisallowedTransition exercises the full outbound sync
// path: a restricted-role agent's claim event should post an internal note
// (comment allowed) but the transition + assignment must be DROPPED and logged
// (criterion 2 + 4). Enforce mode.
func TestJiraSync_GateDropsDisallowedTransition(t *testing.T) {
	srv, rec := newJiraRecordingServer()
	defer srv.Close()

	var logBuf bytes.Buffer
	gate := NewAllowlistGate(FieldAllowlist{
		"restricted": internalNoteOnlyPolicy(),
	}, AllowlistModeEnforce, allowlistTestLogger(&logBuf))

	s := NewJiraSync(JiraSyncConfig{
		Jira:         newTestJiraClient(srv.URL),
		Logger:       slog.New(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelError})),
		BotAccountID: "bot-acct-123",
		Gate:         gate,
		Roles:        &roleResolverStub{roles: map[string]string{"agent-x": "restricted"}},
	})

	event := marshalSSEBeadPayload(BeadEvent{
		ID: "bd-1", Type: "task", Title: "[PE-7001] thing", Status: "in_progress", Assignee: "agent-x",
		Labels: []string{"source:jira", "jira:PE-7001"},
		Fields: map[string]string{"jira_key": "PE-7001"},
	})
	s.handleUpdated(context.Background(), event)

	// Comment is allowed for restricted (internal note) → exactly one comment.
	if got := len(rec.writesTo("/comment")); got != 1 {
		t.Errorf("expected 1 internal-note comment write, got %d", got)
	}
	// Transition POST must be dropped.
	if got := rec.writesTo("/transitions"); len(got) != 0 {
		t.Errorf("expected transition to be DROPPED, got %d writes", len(got))
	}
	// Assignment PUT must be dropped.
	if got := rec.writesTo("/assignee"); len(got) != 0 {
		t.Errorf("expected assignment to be DROPPED, got %d writes", len(got))
	}
	// Rejections logged with role+field (criterion 2).
	logged := logBuf.String()
	if !strings.Contains(logged, "role=restricted") {
		t.Errorf("expected rejection log with role=restricted, got: %s", logged)
	}
	if !strings.Contains(logged, "field=transition") || !strings.Contains(logged, "field=assignee") {
		t.Errorf("expected rejection logs for transition+assignee, got: %s", logged)
	}
}

// TestJiraSync_GateUnknownRoleFailsClosed verifies that when the acting agent's
// role cannot be resolved (unknown agent, no Fields[role]), enforce mode drops
// EVERY governed write — including the comment (criterion 4, fail closed).
func TestJiraSync_GateUnknownRoleFailsClosed(t *testing.T) {
	srv, rec := newJiraRecordingServer()
	defer srv.Close()

	gate := NewAllowlistGate(DefaultFieldAllowlist(), AllowlistModeEnforce,
		allowlistTestLogger(&bytes.Buffer{}))

	s := NewJiraSync(JiraSyncConfig{
		Jira:   newTestJiraClient(srv.URL),
		Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelError})),
		Gate:   gate,
		// Resolver returns not-found for this agent → role "" → unknown → deny.
		Roles: &roleResolverStub{roles: map[string]string{}},
	})

	event := marshalSSEBeadPayload(BeadEvent{
		ID: "bd-2", Type: "task", Title: "[PE-7002] thing", Status: "closed", Assignee: "ghost",
		Labels: []string{"source:jira", "jira:PE-7002"},
		Fields: map[string]string{"jira_key": "PE-7002"},
	})
	s.handleClosed(context.Background(), event)

	if got := len(rec.writesTo("/comment")); got != 0 {
		t.Errorf("unknown role must fail closed: expected 0 comment writes, got %d", got)
	}
}

// TestJiraSync_NilGateIsUngated confirms back-compat: a JiraSync built without a
// gate performs writes exactly as before (no regression for existing wiring).
func TestJiraSync_NilGateIsUngated(t *testing.T) {
	srv, rec := newJiraRecordingServer()
	defer srv.Close()

	s := NewJiraSync(JiraSyncConfig{
		Jira:   newTestJiraClient(srv.URL),
		Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelError})),
		// No Gate, no Roles.
	})

	event := marshalSSEBeadPayload(BeadEvent{
		ID: "bd-3", Type: "task", Title: "[PE-7003] thing", Status: "closed", Assignee: "agent-z",
		Labels: []string{"source:jira", "jira:PE-7003"},
		Fields: map[string]string{"jira_key": "PE-7003"},
	})
	s.handleClosed(context.Background(), event)

	if got := len(rec.writesTo("/comment")); got != 1 {
		t.Errorf("ungated sync should post the closing comment, got %d writes", got)
	}
}

// TestJiraSync_RoleFromBeadFields verifies the acting role is read from the
// bead's own Fields["role"] without needing a resolver, and that a broad role
// is allowed to transition.
func TestJiraSync_RoleFromBeadFields(t *testing.T) {
	srv, rec := newJiraRecordingServer()
	defer srv.Close()

	gate := NewAllowlistGate(FieldAllowlist{"crew": allFieldsPolicy()},
		AllowlistModeEnforce, allowlistTestLogger(&bytes.Buffer{}))

	s := NewJiraSync(JiraSyncConfig{
		Jira:   newTestJiraClient(srv.URL),
		Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelError})),
		Gate:   gate,
		// No Roles resolver — role must come from the bead fields.
	})

	event := marshalSSEBeadPayload(BeadEvent{
		ID: "bd-4", Type: "task", Title: "[PE-7004] thing", Status: "in_progress", Assignee: "agent-q",
		Labels: []string{"source:jira", "jira:PE-7004"},
		Fields: map[string]string{"jira_key": "PE-7004", "role": "crew"},
	})
	s.handleUpdated(context.Background(), event)

	if got := len(rec.writesTo("/comment")); got != 1 {
		t.Errorf("expected claim comment, got %d", got)
	}
	if got := len(rec.writesTo("/transitions")); got != 1 {
		t.Errorf("crew should be allowed to transition, got %d POSTs", got)
	}
}
