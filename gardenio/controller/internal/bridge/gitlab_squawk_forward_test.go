package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestGitLabSquawkForwarder_IgnoresNonMessageBeads(t *testing.T) {
	f := &GitLabSquawkForwarder{
		daemon: newMockDaemon(),
		gitlab: NewGitLabClient(GitLabClientConfig{Logger: slog.Default()}),
		logger: slog.Default(),
		seen:   make(map[string]bool),
	}

	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-task1",
		Type:   "task",
		Labels: []string{"bug"},
	})
	f.handleClosed(context.Background(), data)

	if f.alreadySeen("bd-task1") {
		t.Error("expected task bead to be ignored, not marked as seen")
	}
}

func TestGitLabSquawkForwarder_IgnoresMessageWithoutSquawkLabel(t *testing.T) {
	f := &GitLabSquawkForwarder{
		daemon: newMockDaemon(),
		gitlab: NewGitLabClient(GitLabClientConfig{Logger: slog.Default()}),
		logger: slog.Default(),
		seen:   make(map[string]bool),
	}

	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-msg1",
		Type:   "message",
		Labels: []string{"from:test-agent"},
	})
	f.handleClosed(context.Background(), data)

	if f.alreadySeen("bd-msg1") {
		t.Error("expected non-squawk message bead to be ignored")
	}
}

func TestGitLabSquawkForwarder_SkipsAgentWithoutMRBinding(t *testing.T) {
	mock := newMockDaemon()
	// Agent exists but has no gitlab_mr_url metadata.
	mock.mu.Lock()
	bd := beadsapiBeadDetail("test-agent", "agent", nil)
	mock.beads["test-agent"] = &bd
	mock.mu.Unlock()

	f := &GitLabSquawkForwarder{
		daemon: mock,
		gitlab: NewGitLabClient(GitLabClientConfig{Logger: slog.Default()}),
		logger: slog.Default(),
		seen:   make(map[string]bool),
	}

	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-squawk1",
		Type:   "message",
		Labels: []string{"squawk", "to-mr", "from:test-agent"},
		Fields: map[string]string{
			"source_agent": "test-agent",
			"text":         "Build complete",
		},
	})
	f.handleClosed(context.Background(), data)

	// Should be marked as seen (was processed) but no MR post attempted.
	if !f.alreadySeen("bd-squawk1") {
		t.Error("expected bead to be marked as seen after processing")
	}
}

func TestGitLabSquawkForwarder_SkipsUnknownAgent(t *testing.T) {
	mock := newMockDaemon()
	// No agent seeded — FindAgentBead will return error.

	f := &GitLabSquawkForwarder{
		daemon: mock,
		gitlab: NewGitLabClient(GitLabClientConfig{Logger: slog.Default()}),
		logger: slog.Default(),
		seen:   make(map[string]bool),
	}

	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-squawk2",
		Type:   "message",
		Labels: []string{"squawk", "to-mr", "from:unknown-agent"},
		Fields: map[string]string{
			"source_agent": "unknown-agent",
			"text":         "Hello",
		},
	})
	f.handleClosed(context.Background(), data)

	// Agent not found → silently skipped, bead still marked seen.
	if !f.alreadySeen("bd-squawk2") {
		t.Error("expected bead to be marked as seen")
	}
}

func TestGitLabSquawkForwarder_Dedup(t *testing.T) {
	f := &GitLabSquawkForwarder{
		daemon: newMockDaemon(),
		gitlab: NewGitLabClient(GitLabClientConfig{Logger: slog.Default()}),
		logger: slog.Default(),
		seen:   make(map[string]bool),
	}

	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-squawk-dup",
		Type:   "message",
		Labels: []string{"squawk", "to-mr", "from:agent-1"},
		Fields: map[string]string{
			"source_agent": "agent-1",
			"text":         "First call",
		},
	})

	f.handleClosed(context.Background(), data)
	if !f.alreadySeen("bd-squawk-dup") {
		t.Error("expected bead to be seen after first call")
	}

	// Second call should be deduped — no panic, no double processing.
	f.handleClosed(context.Background(), data)
}

func TestGitLabSquawkForwarder_AcceptsSayLabel(t *testing.T) {
	mock := newMockDaemon()
	// No agent bead seeded, so FindAgentBead will fail — that's fine,
	// we just verify the "say" label passes the filter.

	f := &GitLabSquawkForwarder{
		daemon: mock,
		gitlab: NewGitLabClient(GitLabClientConfig{Logger: slog.Default()}),
		logger: slog.Default(),
		seen:   make(map[string]bool),
	}

	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-say1",
		Type:   "message",
		Labels: []string{"say", "to-mr", "from:test-agent"},
		Fields: map[string]string{
			"source_agent": "test-agent",
			"text":         "Legacy say message",
		},
	})
	f.handleClosed(context.Background(), data)

	if !f.alreadySeen("bd-say1") {
		t.Error("expected legacy 'say' label to be accepted")
	}
}

func TestGitLabSquawkForwarder_ResolveMR_FromURL(t *testing.T) {
	f := &GitLabSquawkForwarder{
		logger: slog.Default(),
	}

	agentBead := &beadsapi.BeadDetail{
		Fields: map[string]string{},
	}
	mrURL := "https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/42"

	projectPath, iid, err := f.resolveMR(agentBead, mrURL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if projectPath != "Acme/Platform/monorepo" {
		t.Errorf("projectPath = %q, want %q", projectPath, "Acme/Platform/monorepo")
	}
	if iid != 42 {
		t.Errorf("iid = %d, want 42", iid)
	}
}

func TestGitLabSquawkForwarder_ResolveMR_FromBeadFieldIID(t *testing.T) {
	f := &GitLabSquawkForwarder{
		logger: slog.Default(),
	}

	agentBead := &beadsapi.BeadDetail{
		Fields: map[string]string{
			"gitlab_mr_iid": "99",
		},
	}
	mrURL := "https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/42"

	// Should prefer the bead field IID over the URL IID.
	projectPath, iid, err := f.resolveMR(agentBead, mrURL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if projectPath != "Acme/Platform/monorepo" {
		t.Errorf("projectPath = %q, want %q", projectPath, "Acme/Platform/monorepo")
	}
	if iid != 99 {
		t.Errorf("iid = %d, want 99 (from bead field)", iid)
	}
}

func TestGitLabSquawkForwarder_ResolveMR_InvalidURL(t *testing.T) {
	f := &GitLabSquawkForwarder{
		logger: slog.Default(),
	}

	agentBead := &beadsapi.BeadDetail{
		Fields: map[string]string{},
	}
	_, _, err := f.resolveMR(agentBead, "not-a-valid-url")
	if err == nil {
		t.Error("expected error for invalid MR URL")
	}
}

// TestGitLabSquawkForwarder_SkipsWithoutToMRLabel is the core opt-in guard for
// ka-0000005748: a squawk from an MR-bound agent that is NOT explicitly marked
// with the "to-mr" label must NOT be mirrored to the MR. It is deliberately not
// even marked seen — the bead is not the forwarder's to consume (the Slack
// Squawk watcher still handles it). This is the fix for the flood: 184 of 332
// gardenio MR comments were unconditionally-mirrored progress squawks.
func TestGitLabSquawkForwarder_SkipsWithoutToMRLabel(t *testing.T) {
	mock := newMockDaemon()
	// Seed an agent that IS bound to an MR — the only thing missing is the
	// opt-in label, so under the old unconditional behavior this WOULD post.
	mock.mu.Lock()
	bd := beadsapiBeadDetail("mr-agent", "agent", map[string]string{
		"gitlab_mr_url": "https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/42",
	})
	mock.beads["mr-agent"] = &bd
	mock.mu.Unlock()

	// Fail loudly if any GitLab call is attempted.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected GitLab call %s %s — unlabelled squawk must not be mirrored", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	f := &GitLabSquawkForwarder{
		daemon: mock,
		gitlab: NewGitLabClient(GitLabClientConfig{BaseURL: srv.URL, Token: "t", Logger: slog.Default()}),
		logger: slog.Default(),
		seen:   make(map[string]bool),
	}

	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-noopt",
		Type:   "message",
		Labels: []string{"squawk", "from:mr-agent"}, // no "to-mr"
		Fields: map[string]string{
			"source_agent": "mr-agent",
			"text":         "re-verified merge-ready (respawn #7)",
		},
	})
	f.handleClosed(context.Background(), data)

	// Not consumed: left for the Slack watcher, not marked seen.
	if f.alreadySeen("bd-noopt") {
		t.Error("expected unlabelled squawk to be skipped BEFORE the seen-mark (not consumed by the MR forwarder)")
	}
}

// TestGitLabSquawkForwarder_PostsWithToMRLabel is the positive path: a squawk
// explicitly marked "to-mr" from an MR-bound agent IS posted as an MR note.
func TestGitLabSquawkForwarder_PostsWithToMRLabel(t *testing.T) {
	mock := newMockDaemon()
	mock.mu.Lock()
	bd := beadsapiBeadDetail("mr-agent", "agent", map[string]string{
		"gitlab_mr_url": "https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/42",
		"gitlab_mr_iid": "42",
	})
	mock.beads["mr-agent"] = &bd
	mock.mu.Unlock()

	var gotPath, gotBody string
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		gotPath = r.URL.Path
		var payload map[string]string
		_ = json.NewDecoder(r.Body).Decode(&payload)
		gotBody = payload["body"]
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(GitLabNote{ID: 1})
	}))
	t.Cleanup(srv.Close)

	f := &GitLabSquawkForwarder{
		daemon: mock,
		gitlab: NewGitLabClient(GitLabClientConfig{BaseURL: srv.URL, Token: "t", Logger: slog.Default()}),
		logger: slog.Default(),
		seen:   make(map[string]bool),
	}

	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-opt",
		Type:   "message",
		Labels: []string{"squawk", "to-mr", "from:mr-agent"},
		Fields: map[string]string{
			"source_agent": "mr-agent",
			"text":         "Fixed the nil-pointer you flagged; pushed.",
		},
	})
	f.handleClosed(context.Background(), data)

	if posts != 1 {
		t.Fatalf("expected exactly 1 MR note POST, got %d", posts)
	}
	// net/http decodes the %2F-escaped project path before it reaches the
	// handler, so assert on the decoded form.
	wantPath := "/api/v4/projects/Acme/Platform/monorepo/merge_requests/42/notes"
	if gotPath != wantPath {
		t.Errorf("POST path = %q, want %q", gotPath, wantPath)
	}
	if !strings.Contains(gotBody, "Fixed the nil-pointer") || !strings.Contains(gotBody, "mr-agent") {
		t.Errorf("MR note body missing expected content: %q", gotBody)
	}
}

// TestGitLabSquawkForwarder_TruncatesLongBody verifies the mrSquawkMaxLen cap:
// an oversized squawk is truncated before it reaches the MR note.
func TestGitLabSquawkForwarder_TruncatesLongBody(t *testing.T) {
	mock := newMockDaemon()
	mock.mu.Lock()
	bd := beadsapiBeadDetail("mr-agent", "agent", map[string]string{
		"gitlab_mr_url": "https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/42",
		"gitlab_mr_iid": "42",
	})
	mock.beads["mr-agent"] = &bd
	mock.mu.Unlock()

	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]string
		_ = json.NewDecoder(r.Body).Decode(&payload)
		gotBody = payload["body"]
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(GitLabNote{ID: 1})
	}))
	t.Cleanup(srv.Close)

	f := &GitLabSquawkForwarder{
		daemon: mock,
		gitlab: NewGitLabClient(GitLabClientConfig{BaseURL: srv.URL, Token: "t", Logger: slog.Default()}),
		logger: slog.Default(),
		seen:   make(map[string]bool),
	}

	long := strings.Repeat("x", mrSquawkMaxLen+500)
	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-long",
		Type:   "message",
		Labels: []string{"squawk", "to-mr", "from:mr-agent"},
		Fields: map[string]string{
			"source_agent": "mr-agent",
			"text":         long,
		},
	})
	f.handleClosed(context.Background(), data)

	// The prefix framing ("**gardenio agent** `mr-agent`:\n\n") plus the capped
	// text; the text portion must not exceed mrSquawkMaxLen.
	if strings.Count(gotBody, "x") > mrSquawkMaxLen {
		t.Errorf("expected text truncated to <= %d chars, got %d 'x'", mrSquawkMaxLen, strings.Count(gotBody, "x"))
	}
	if !strings.HasSuffix(gotBody, "...") {
		t.Errorf("expected truncated body to end with ellipsis, got suffix %q", gotBody[len(gotBody)-5:])
	}
}

// beadsapiBeadDetail is a test helper to create a BeadDetail with optional fields.
func beadsapiBeadDetail(name, beadType string, fields map[string]string) beadsapi.BeadDetail {
	return beadsapi.BeadDetail{
		ID:     "bd-" + name,
		Title:  name,
		Type:   beadType,
		Fields: fields,
	}
}
