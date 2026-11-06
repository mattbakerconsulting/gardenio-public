package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
)

// lineageMockServer mocks the kbeads daemon for writeSquawkLineage tests.
// It handles the squawk-create + close path, the auto-comment list (returns
// empty so no comment is attempted), AddDependency (POST /v1/beads/{id}/dependencies),
// and ListBeadsFiltered for slack-thread / user-bead lookups.
type lineageMockServer struct {
	mu sync.Mutex

	// Captured dep targets — (beadID, dependsOnID) pairs.
	deps []dependency

	// Captured create-bead requests (raw JSON bodies of POST /v1/beads), in
	// order, so tests can assert the fields stamped on a created message bead
	// (e.g. the thread_id wish-pointer, ka-0000003279).
	createBodies []map[string]any

	// Listing responses keyed by query.
	threadBead map[string]any   // returned when labels query matches a slack-thread label
	userBeads  []map[string]any // returned for type=user/kind=config queries
	// channelBeads maps a Slack channel id to the type=channel bead bound to
	// it, for the field=slack_channel_id lookup (channel_stamp.go).
	channelBeads map[string]map[string]any
	// refuseStamped makes a create carrying fields.channel_id fail with 400.
	refuseStamped bool
	// agentBeads are served by GET /v1/beads/{id}; any other id is 404.
	agentBeads map[string]map[string]any
	// cardBeads are returned for the type=message card lookup
	// (agentCardPlace), and cardQueries records each one's field filters.
	cardBeads   []map[string]any
	cardQueries [][]string
}

// lastCreateLabels returns the labels of the most recent POST /v1/beads body.
func (s *lineageMockServer) lastCreateLabels() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.createBodies) == 0 {
		return nil
	}
	raw, _ := s.createBodies[len(s.createBodies)-1]["labels"].([]any)
	out := make([]string, 0, len(raw))
	for _, l := range raw {
		out = append(out, fmt.Sprintf("%v", l))
	}
	return out
}

// lastCreateFields returns the fields map of the most recent POST /v1/beads
// body as map[string]string, or nil when none was captured. The daemon client
// sends fields as a nested JSON object under "fields".
func (s *lineageMockServer) lastCreateFields() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.createBodies) == 0 {
		return nil
	}
	body := s.createBodies[len(s.createBodies)-1]
	raw, ok := body["fields"].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		out[k] = fmt.Sprintf("%v", v)
	}
	return out
}

type dependency struct {
	beadID    string
	dependsOn string
}

func (s *lineageMockServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads":
			body := map[string]any{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.mu.Lock()
			s.createBodies = append(s.createBodies, body)
			refuse := s.refuseStamped
			s.mu.Unlock()
			if fields, _ := body["fields"].(map[string]any); refuse && fields["channel_id"] != nil {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "channel_id is not a channel bead"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-SQ1"})

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/close"):
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			s.mu.Lock()
			defer s.mu.Unlock()

			q, _ := url.ParseQuery(r.URL.RawQuery)
			labels := q.Get("labels")
			types := q.Get("type")
			assignee := q.Get("assignee")

			// slack-thread lookup
			if strings.HasPrefix(labels, "slack-thread:") {
				if s.threadBead == nil {
					_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"beads": []any{s.threadBead}, "total": 1,
				})
				return
			}

			// channel-bead lookup by slack_channel_id
			if types == "channel" {
				slackChannel, _ := strings.CutPrefix(q.Get("field"), "slack_channel_id=")
				beads := []any{}
				if ch, ok := s.channelBeads[slackChannel]; ok {
					beads = append(beads, ch)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"beads": beads, "total": len(beads)})
				return
			}

			// agent card lookup (kind=card, subject_id=<agent bead>)
			if types == "message" {
				s.cardQueries = append(s.cardQueries, q["field"])
				_ = json.NewEncoder(w).Encode(map[string]any{"beads": s.cardBeads, "total": len(s.cardBeads)})
				return
			}

			// user-bead lookup
			if types == "user" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"beads": s.userBeads, "total": len(s.userBeads),
				})
				return
			}

			// auto-comment claimed-bead list (kind=issue, status=in_progress, assignee=...)
			if assignee != "" {
				_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
				return
			}

			_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/dependencies"):
			// Path: /v1/beads/{id}/dependencies
			parts := strings.Split(r.URL.Path, "/")
			var beadID string
			if len(parts) >= 4 {
				beadID = parts[3]
			}
			body := map[string]string{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.mu.Lock()
			s.deps = append(s.deps, dependency{beadID: beadID, dependsOn: body["depends_on_id"]})
			s.mu.Unlock()
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodGet && strings.Count(r.URL.Path, "/") == 3 && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			s.mu.Lock()
			b, ok := s.agentBeads[strings.TrimPrefix(r.URL.Path, "/v1/beads/")]
			s.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
				return
			}
			_ = json.NewEncoder(w).Encode(b)

		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func clearLineageEnv(t *testing.T) {
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_TRIGGER_BEAD_ID", "")
	t.Setenv("SLACK_THREAD_CHANNEL", "")
	t.Setenv("SLACK_THREAD_TS", "")
	t.Setenv("SLACK_USER_ID", "")
	// Point HOME at a tmp dir so git config user.email returns nothing
	// (avoids picking up a real value from the test runner).
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
}

func TestWriteSquawkLineage_HumanFallback_SlackThread(t *testing.T) {
	clearLineageEnv(t)
	t.Setenv("SLACK_THREAD_CHANNEL", "C123")
	t.Setenv("SLACK_THREAD_TS", "1779836613.718519")

	mock := &lineageMockServer{
		threadBead: map[string]any{
			"id":     "gd-THREAD1",
			"title":  "Slack thread",
			"type":   "task",
			"kind":   "issue",
			"status": "open",
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
	if err := runSquawk(cmd, []string{"hello"}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	if len(mock.deps) != 1 {
		t.Fatalf("expected 1 triggered-by dep, got %d (%+v)", len(mock.deps), mock.deps)
	}
	if mock.deps[0].beadID != "gd-SQ1" || mock.deps[0].dependsOn != "gd-THREAD1" {
		t.Errorf("expected gd-SQ1 -> gd-THREAD1, got %+v", mock.deps[0])
	}
}

func TestWriteSquawkLineage_HumanFallback_UserBeadBySlackID(t *testing.T) {
	clearLineageEnv(t)
	// No slack thread env vars set — should fall through to user bead.
	t.Setenv("SLACK_USER_ID", "U0OPERATOR1")

	mock := &lineageMockServer{
		userBeads: []map[string]any{
			{
				"id":     "gd-MATT",
				"title":  "Matt Baker",
				"type":   "user",
				"kind":   "config",
				"status": "open",
				"fields": map[string]string{
					"slack_id": "U0OPERATOR1",
					"email":    "matt@example.com",
				},
			},
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
	if err := runSquawk(cmd, []string{"hello"}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	if len(mock.deps) != 1 {
		t.Fatalf("expected 1 triggered-by dep, got %d (%+v)", len(mock.deps), mock.deps)
	}
	if mock.deps[0].dependsOn != "gd-MATT" {
		t.Errorf("expected dep on gd-MATT, got %+v", mock.deps[0])
	}
}

func TestWriteSquawkLineage_HumanFallback_SlackThreadPreferredOverUser(t *testing.T) {
	clearLineageEnv(t)
	t.Setenv("SLACK_THREAD_CHANNEL", "C123")
	t.Setenv("SLACK_THREAD_TS", "1779836613.718519")
	t.Setenv("SLACK_USER_ID", "U0OPERATOR1")

	mock := &lineageMockServer{
		threadBead: map[string]any{"id": "gd-THREAD1"},
		userBeads: []map[string]any{
			{"id": "gd-MATT", "type": "user", "fields": map[string]string{"slack_id": "U0OPERATOR1"}},
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
	if err := runSquawk(cmd, []string{"hello"}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	if len(mock.deps) != 1 || mock.deps[0].dependsOn != "gd-THREAD1" {
		t.Errorf("expected single dep on slack-thread bead, got %+v", mock.deps)
	}
}

func TestWriteSquawkLineage_HumanFallback_NoResolveStaysNoop(t *testing.T) {
	clearLineageEnv(t)
	// No identifiers at all.

	mock := &lineageMockServer{}
	srv := httptest.NewServer(mock.handler(t))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	oldActor := actor
	actor = "me"
	t.Cleanup(func() { actor = oldActor })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runSquawk(cmd, []string{"hello"}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	if len(mock.deps) != 0 {
		t.Errorf("expected no deps when nothing resolves, got %+v", mock.deps)
	}
}

func TestWriteSquawkLineage_HumanFallback_SkippedWhenAgentContextSet(t *testing.T) {
	clearLineageEnv(t)
	// Agent context present — fallback must NOT fire even if slack-thread env is set.
	t.Setenv("BOAT_AGENT_BEAD_ID", "gd-AGENT1")
	t.Setenv("SLACK_THREAD_CHANNEL", "C123")
	t.Setenv("SLACK_THREAD_TS", "1779836613.718519")

	mock := &lineageMockServer{
		threadBead: map[string]any{"id": "gd-THREAD1"},
	}
	srv := httptest.NewServer(mock.handler(t))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	oldActor := actor
	actor = "me"
	t.Cleanup(func() { actor = oldActor })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runSquawk(cmd, []string{"hello"}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	if len(mock.deps) != 1 || mock.deps[0].dependsOn != "gd-AGENT1" {
		t.Errorf("expected single dep on agent bead, got %+v", mock.deps)
	}
}

func TestWriteSquawkLineage_HumanFallback_UnknownThreadAndUserStaysNoop(t *testing.T) {
	clearLineageEnv(t)
	t.Setenv("SLACK_THREAD_CHANNEL", "C123")
	t.Setenv("SLACK_THREAD_TS", "1779836613.718519")
	t.Setenv("SLACK_USER_ID", "U-UNKNOWN")

	// threadBead nil → list returns empty; userBeads has no matching slack_id.
	mock := &lineageMockServer{
		userBeads: []map[string]any{
			{"id": "gd-OTHER", "type": "user", "fields": map[string]string{"slack_id": "U-OTHER"}},
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
	if err := runSquawk(cmd, []string{"hello"}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	if len(mock.deps) != 0 {
		t.Errorf("expected no deps when nothing resolves, got %+v", mock.deps)
	}
}

// TestRunSquawk_StampsThreadID asserts that a squawk emitted inside a Slack
// thread carries the thread_id wish-pointer on its message bead, so the
// beads-viewer /threads conversation lane renders it as a member. Membership
// is UNION(fields.thread_id == headID, parent-child closure); the triggered-by
// lineage dep is NOT a membership edge, so without thread_id the squawk is
// invisible in the thread view (ka-0000003279).
func TestRunSquawk_StampsThreadID(t *testing.T) {
	clearLineageEnv(t)
	t.Setenv("SLACK_THREAD_CHANNEL", "C123")
	t.Setenv("SLACK_THREAD_TS", "1779836613.718519")

	mock := &lineageMockServer{
		threadBead: map[string]any{
			"id":     "gd-THREAD1",
			"title":  "Slack thread",
			"type":   "task",
			"kind":   "issue",
			"status": "open",
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
	if err := runSquawk(cmd, []string{"progress update"}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	fields := mock.lastCreateFields()
	if fields == nil {
		t.Fatal("expected a created message bead with fields")
	}
	if got := fields["thread_id"]; got != "gd-THREAD1" {
		t.Errorf("squawk message bead thread_id = %q, want gd-THREAD1 (fields=%v)", got, fields)
	}
}

// TestRunSquawk_NoThreadContext_NoThreadID asserts that a squawk with no
// resolvable thread context (no env coords, no thread bead) does NOT stamp a
// thread_id — non-thread squawks stay unchanged (ka-0000003279).
func TestRunSquawk_NoThreadContext_NoThreadID(t *testing.T) {
	clearLineageEnv(t)

	mock := &lineageMockServer{} // no threadBead, no userBeads
	srv := httptest.NewServer(mock.handler(t))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	oldActor := actor
	actor = "me"
	t.Cleanup(func() { actor = oldActor })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runSquawk(cmd, []string{"standalone squawk"}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	fields := mock.lastCreateFields()
	if fields == nil {
		t.Fatal("expected a created message bead with fields")
	}
	if _, ok := fields["thread_id"]; ok {
		t.Errorf("non-thread squawk must not carry thread_id; got %v", fields)
	}
}

// TestRecordSlackReplyBead_StampsThreadID asserts that gb slack reply binds its
// recorded message bead to the thread head via thread_id, so an agent's
// outbound reply shows in the /threads conversation lane (ka-0000003279).
func TestRecordSlackReplyBead_StampsThreadID(t *testing.T) {
	clearLineageEnv(t)
	t.Setenv("SLACK_THREAD_CHANNEL", "C123")
	t.Setenv("SLACK_THREAD_TS", "1779836613.718519")

	mock := &lineageMockServer{
		threadBead: map[string]any{
			"id":     "gd-THREAD1",
			"title":  "Slack thread",
			"type":   "task",
			"kind":   "issue",
			"status": "open",
		},
	}
	srv := httptest.NewServer(mock.handler(t))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	oldActor := actor
	actor = "agent-x"
	t.Cleanup(func() { actor = oldActor })

	recordSlackReplyBead(context.Background(), "on it", "C123", "1779836613.718519")

	fields := mock.lastCreateFields()
	if fields == nil {
		t.Fatal("expected a created message bead with fields")
	}
	if got := fields["thread_id"]; got != "gd-THREAD1" {
		t.Errorf("slack-reply message bead thread_id = %q, want gd-THREAD1 (fields=%v)", got, fields)
	}
	if got := fields["text"]; got != "on it" {
		t.Errorf("slack-reply message text = %q, want %q", got, "on it")
	}
}
