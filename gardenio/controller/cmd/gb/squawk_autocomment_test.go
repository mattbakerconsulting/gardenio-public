package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
)

// squawkMockServer is a stand-in for the beads daemon that records the
// AddComment call (POST /v1/beads/{id}/comments) and lets the caller control
// the GET /v1/beads listing response.
type squawkMockServer struct {
	mu sync.Mutex

	// Beads to return from GET /v1/beads (the listing for claimed work).
	listBeads []map[string]any
	// Status code to return on the listing call (0 = 200 with listBeads).
	listStatus int
	// Status code for the AddComment call (0 = 200).
	commentStatus int

	// Captured state.
	listCalls       int
	commentBeadID   string
	commentAuthor   string
	commentText     string
	commentCalls    int
	listQueryParams string
	createLabels    []string // labels on the POST /v1/beads create call
}

func (s *squawkMockServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads":
			body, _ := io.ReadAll(r.Body)
			var payload struct {
				Labels []string `json:"labels"`
			}
			_ = json.Unmarshal(body, &payload)
			s.mu.Lock()
			s.createLabels = payload.Labels
			s.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-SQ1"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/close"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			// Tests assert on the claimed-bead list call only; the lineage
			// fallback paths (slack-thread label lookup, user bead lookup)
			// hit the same endpoint but with different query params — return
			// empty results and don't bump listCalls for those so existing
			// assertions remain stable.
			if strings.Contains(r.URL.RawQuery, "labels=slack-thread") ||
				strings.Contains(r.URL.RawQuery, "type=user") {
				_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
				return
			}
			s.mu.Lock()
			s.listCalls++
			s.listQueryParams = r.URL.RawQuery
			status := s.listStatus
			beads := s.listBeads
			s.mu.Unlock()
			if status != 0 {
				w.WriteHeader(status)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"beads": beads,
				"total": len(beads),
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/dependencies"):
			// Lineage fallback may attach triggered-by deps; accept silently.
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			body, _ := io.ReadAll(r.Body)
			var payload map[string]string
			_ = json.Unmarshal(body, &payload)
			// Path: /v1/beads/<id>/comments
			parts := strings.Split(r.URL.Path, "/")
			beadID := ""
			if len(parts) >= 4 {
				beadID = parts[3]
			}
			s.mu.Lock()
			s.commentBeadID = beadID
			s.commentAuthor = payload["author"]
			s.commentText = payload["text"]
			s.commentCalls++
			status := s.commentStatus
			s.mu.Unlock()
			if status != 0 {
				w.WriteHeader(status)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func TestRunSquawk_AutoCommentOnClaimedBead(t *testing.T) {
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_TRIGGER_BEAD_ID", "")
	t.Setenv("SLACK_USER_ID", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("SLACK_THREAD_CHANNEL", "C123")
	t.Setenv("SLACK_THREAD_TS", "1779836613.718519")

	mock := &squawkMockServer{
		listBeads: []map[string]any{
			{"id": "gd-WORK1", "title": "claimed task", "kind": "issue", "status": "in_progress", "assignee": "me"},
		},
	}
	srv := httptest.NewServer(mock.handler(t))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	oldActor := actor
	actor = "me"
	t.Cleanup(func() { actor = oldActor })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runSquawk(cmd, []string{"build started"}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	if mock.commentCalls != 1 {
		t.Fatalf("expected 1 comment call, got %d", mock.commentCalls)
	}
	if mock.commentBeadID != "gd-WORK1" {
		t.Errorf("expected comment on gd-WORK1, got %q", mock.commentBeadID)
	}
	if mock.commentAuthor != "me" {
		t.Errorf("expected author=me, got %q", mock.commentAuthor)
	}
	if !strings.HasPrefix(mock.commentText, "build started") {
		t.Errorf("expected comment to start with original squawk, got %q", mock.commentText)
	}
	if !strings.Contains(mock.commentText, "squawk gd-SQ1 from me") {
		t.Errorf("expected footer with squawk id + agent, got %q", mock.commentText)
	}
	if !strings.Contains(mock.commentText, "slack-thread channel=C123 ts=1779836613.718519") {
		t.Errorf("expected slack-thread footer, got %q", mock.commentText)
	}

	// List call should target in_progress + kind=issue + assignee=me.
	q := mock.listQueryParams
	if !strings.Contains(q, "assignee=me") {
		t.Errorf("expected list query to filter assignee=me, got %q", q)
	}
	if !strings.Contains(q, "status=in_progress") {
		t.Errorf("expected list query to filter status=in_progress, got %q", q)
	}
	if !strings.Contains(q, "kind=issue") {
		t.Errorf("expected list query to filter kind=issue, got %q", q)
	}
}

func TestRunSquawk_AutoCommentNoSlackEnv(t *testing.T) {
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_TRIGGER_BEAD_ID", "")
	t.Setenv("SLACK_USER_ID", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("SLACK_THREAD_CHANNEL", "")
	t.Setenv("SLACK_THREAD_TS", "")

	mock := &squawkMockServer{
		listBeads: []map[string]any{
			{"id": "gd-WORK2", "title": "claimed", "kind": "issue", "status": "in_progress", "assignee": "me"},
		},
	}
	srv := httptest.NewServer(mock.handler(t))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	oldActor := actor
	actor = "me"
	t.Cleanup(func() { actor = oldActor })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runSquawk(cmd, []string{"progress note"}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	if mock.commentCalls != 1 {
		t.Fatalf("expected 1 comment call, got %d", mock.commentCalls)
	}
	if strings.Contains(mock.commentText, "slack-thread") {
		t.Errorf("expected no slack-thread footer when env vars unset, got %q", mock.commentText)
	}
	if !strings.Contains(mock.commentText, "squawk gd-SQ1 from me") {
		t.Errorf("expected basic footer, got %q", mock.commentText)
	}
}

func TestRunSquawk_AutoCommentNoClaimedBead(t *testing.T) {
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_TRIGGER_BEAD_ID", "")
	t.Setenv("SLACK_USER_ID", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	mock := &squawkMockServer{listBeads: nil} // empty list
	srv := httptest.NewServer(mock.handler(t))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	oldActor := actor
	actor = "me"
	t.Cleanup(func() { actor = oldActor })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runSquawk(cmd, []string{"hi"}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	if mock.listCalls != 1 {
		t.Errorf("expected exactly 1 list call, got %d", mock.listCalls)
	}
	if mock.commentCalls != 0 {
		t.Errorf("expected no comment when no claimed bead, got %d calls", mock.commentCalls)
	}
}

func TestRunSquawk_AutoCommentListErrorBestEffort(t *testing.T) {
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_TRIGGER_BEAD_ID", "")
	t.Setenv("SLACK_USER_ID", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	mock := &squawkMockServer{listStatus: http.StatusInternalServerError}
	srv := httptest.NewServer(mock.handler(t))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	oldActor := actor
	actor = "me"
	t.Cleanup(func() { actor = oldActor })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	// Squawk must succeed even when listing fails.
	if err := runSquawk(cmd, []string{"resilient"}); err != nil {
		t.Errorf("squawk should not fail on list error: %v", err)
	}
	if mock.commentCalls != 0 {
		t.Errorf("expected no comment call on list error, got %d", mock.commentCalls)
	}
}

func TestRunSquawk_AutoCommentAddCommentErrorBestEffort(t *testing.T) {
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_TRIGGER_BEAD_ID", "")
	t.Setenv("SLACK_USER_ID", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	mock := &squawkMockServer{
		listBeads: []map[string]any{
			{"id": "gd-WORK3", "title": "x", "kind": "issue", "status": "in_progress", "assignee": "me"},
		},
		commentStatus: http.StatusInternalServerError,
	}
	srv := httptest.NewServer(mock.handler(t))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	oldActor := actor
	actor = "me"
	t.Cleanup(func() { actor = oldActor })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runSquawk(cmd, []string{"resilient2"}); err != nil {
		t.Errorf("squawk should not fail on comment error: %v", err)
	}
	if mock.commentCalls != 1 {
		t.Errorf("expected 1 comment attempt, got %d", mock.commentCalls)
	}
}

func TestRunSquawk_NoCommentFlag(t *testing.T) {
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_TRIGGER_BEAD_ID", "")
	t.Setenv("SLACK_USER_ID", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	mock := &squawkMockServer{
		listBeads: []map[string]any{
			{"id": "gd-WORK4", "title": "claimed", "kind": "issue", "status": "in_progress", "assignee": "me"},
		},
	}
	srv := httptest.NewServer(mock.handler(t))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	oldActor := actor
	actor = "me"
	t.Cleanup(func() { actor = oldActor })

	old := squawkNoComment
	squawkNoComment = true
	t.Cleanup(func() { squawkNoComment = old })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runSquawk(cmd, []string{"no comment please"}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if mock.listCalls != 0 {
		t.Errorf("expected no list call with --no-comment, got %d", mock.listCalls)
	}
	if mock.commentCalls != 0 {
		t.Errorf("expected no comment call with --no-comment, got %d", mock.commentCalls)
	}
}

func TestRunSquawk_AutoCommentSkipsUnknownActor(t *testing.T) {
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_TRIGGER_BEAD_ID", "")
	t.Setenv("SLACK_USER_ID", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	mock := &squawkMockServer{}
	srv := httptest.NewServer(mock.handler(t))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	oldActor := actor
	actor = "unknown"
	t.Cleanup(func() { actor = oldActor })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runSquawk(cmd, []string{"who am I"}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if mock.listCalls != 0 {
		t.Errorf("expected no list call for unknown actor, got %d", mock.listCalls)
	}
}

// TestRunSquawk_ToMRLabel verifies the opt-in mirror flag (ka-0000005748):
// `gb squawk --to-mr` stamps the "to-mr" label on the message bead (which the
// GitLab squawk forwarder gates on), while a plain squawk does not.
func TestRunSquawk_ToMRLabel(t *testing.T) {
	setup := func(t *testing.T) *squawkMockServer {
		t.Setenv("BOAT_AGENT_BEAD_ID", "")
		t.Setenv("KD_AGENT_ID", "")
		t.Setenv("BOAT_TRIGGER_BEAD_ID", "")
		t.Setenv("SLACK_USER_ID", "")
		t.Setenv("HOME", t.TempDir())
		t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
		t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
		t.Setenv("SLACK_THREAD_CHANNEL", "")
		t.Setenv("SLACK_THREAD_TS", "")

		mock := &squawkMockServer{}
		srv := httptest.NewServer(mock.handler(t))
		t.Cleanup(srv.Close)
		swapDaemon(t, srv.URL)

		oldActor := actor
		actor = "me"
		t.Cleanup(func() { actor = oldActor })

		// squawkToMR is a package global set by the flag; reset after each run.
		oldToMR := squawkToMR
		t.Cleanup(func() { squawkToMR = oldToMR })
		return mock
	}

	t.Run("with --to-mr", func(t *testing.T) {
		mock := setup(t)
		squawkToMR = true
		cmd := &cobra.Command{}
		cmd.SetContext(context.Background())
		if err := runSquawk(cmd, []string{"reply to reviewer"}); err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !slices.Contains(mock.createLabels, "to-mr") {
			t.Errorf("expected 'to-mr' label on squawk bead, got %v", mock.createLabels)
		}
		if !slices.Contains(mock.createLabels, "squawk") {
			t.Errorf("expected 'squawk' label preserved, got %v", mock.createLabels)
		}
	})

	t.Run("without --to-mr", func(t *testing.T) {
		mock := setup(t)
		squawkToMR = false
		cmd := &cobra.Command{}
		cmd.SetContext(context.Background())
		if err := runSquawk(cmd, []string{"routine progress"}); err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if slices.Contains(mock.createLabels, "to-mr") {
			t.Errorf("expected NO 'to-mr' label on a plain squawk, got %v", mock.createLabels)
		}
	})
}
