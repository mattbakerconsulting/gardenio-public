package bridge

// Regression guards for the dark-Jira write-token fence, client layer
// (task ka-0000005242). A JiraClient constructed read-only (dark agent pod)
// must fail EVERY write method closed with ErrJiraReadOnly WITHOUT issuing a
// request, while read methods stay usable. This is belt-and-suspenders over the
// fenced JIRA_API_TOKEN (pod layer) and the denied Atlassian egress (network
// layer): a dark agent's write fails loudly and locally, never silently
// attempted against real upstream Jira.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// newTrackingJiraClient returns a read-only JiraClient pointed at a test server
// that records whether it was ever hit. A read-only write must never reach it.
func newTrackingJiraClient(t *testing.T, readOnly bool) (*JiraClient, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(srv.Close)
	c := NewJiraClient(JiraClientConfig{
		BaseURL:  srv.URL,
		Email:    "dark@acme.example",
		APIToken: "unused-in-readonly",
		ReadOnly: readOnly,
	})
	return c, &hits
}

func TestJiraClient_ReadOnlyBlocksAllWrites(t *testing.T) {
	c, hits := newTrackingJiraClient(t, true)
	if !c.ReadOnly() {
		t.Fatal("client should report ReadOnly()")
	}
	ctx := context.Background()

	writes := map[string]func() error{
		"TransitionIssue":  func() error { return c.TransitionIssue(ctx, "FSM-1", "Done") },
		"AddComment":       func() error { return c.AddComment(ctx, "FSM-1", "hi") },
		"AddCommentVis":    func() error { return c.AddCommentWithVisibility(ctx, "FSM-1", "hi", CommentInternal) },
		"AddCommentSrc":    func() error { return c.AddCommentWithVisibilityAndSourceLink(ctx, "FSM-1", "hi", CommentInternal, "9") },
		"AddMarkdownComnt": func() error { return c.AddMarkdownCommentWithVisibilityAndSourceLink(ctx, "FSM-1", "**hi**", CommentInternal, "9") },
		"AddRemoteLink":    func() error { return c.AddRemoteLink(ctx, "FSM-1", "https://x", "t") },
		"AddLabels":        func() error { return c.AddLabels(ctx, "FSM-1", []string{"l"}) },
		"SetAffectsVers":   func() error { return c.SetAffectsVersions(ctx, "FSM-1", []string{"1.0"}) },
		"AssignIssue":      func() error { return c.AssignIssue(ctx, "FSM-1", "acct-1") },
	}
	for name, fn := range writes {
		t.Run(name, func(t *testing.T) {
			err := fn()
			if err == nil {
				t.Fatalf("%s returned nil error in read-only mode; writes must fail closed", name)
			}
			if !errors.Is(err, ErrJiraReadOnly) {
				t.Fatalf("%s error = %v, want ErrJiraReadOnly", name, err)
			}
		})
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("read-only writes issued %d HTTP request(s); they must never touch the network", got)
	}
}

func TestJiraClient_ReadOnlyAddAttachmentBlocked(t *testing.T) {
	c, hits := newTrackingJiraClient(t, true)
	_, err := c.AddAttachment(context.Background(), "FSM-1", "f.png", []byte("data"))
	if err == nil || !errors.Is(err, ErrJiraReadOnly) {
		t.Fatalf("AddAttachment error = %v, want ErrJiraReadOnly", err)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("read-only AddAttachment issued %d HTTP request(s)", got)
	}
}

// TestJiraClient_WritableModeUnaffected proves the guard is opt-in: a normal
// (writable) client still reaches the server for a write.
func TestJiraClient_WritableModeUnaffected(t *testing.T) {
	c, hits := newTrackingJiraClient(t, false)
	if c.ReadOnly() {
		t.Fatal("default client must not be read-only")
	}
	if err := c.AddComment(context.Background(), "FSM-1", "hi"); err != nil {
		t.Fatalf("writable AddComment failed: %v", err)
	}
	if got := hits.Load(); got == 0 {
		t.Fatal("writable AddComment never reached the server")
	}
}
