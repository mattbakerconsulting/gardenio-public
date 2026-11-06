package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

// ── runSquawk ────────────────────────────────────────────────────────────

func TestRunSquawk_EmptyText(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runSquawk(cmd, []string{""}); err == nil {
		t.Error("expected empty text error")
	}
}

func TestRunSquawk_Success(t *testing.T) {
	// Clear lineage env vars so this test exercises only the create+close path.
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_TRIGGER_BEAD_ID", "")
	t.Setenv("SLACK_THREAD_CHANNEL", "")
	t.Setenv("SLACK_THREAD_TS", "")
	t.Setenv("SLACK_USER_ID", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	disableSquawkAutoComment(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads":
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-SQ1"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/close"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			// Lineage fallback may probe for a slack-thread or user bead;
			// return empty so it converges on the no-op path.
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	oldActor := actor
	actor = "me"
	t.Cleanup(func() { actor = oldActor })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runSquawk(cmd, []string{"hello world"}); err != nil {
		t.Errorf("err: %v", err)
	}
}

// disableSquawkAutoComment opts the current test out of the post-squawk
// auto-comment on the actor's claimed bead. Tests that exercise only the
// create+close+lineage paths use this so their mock daemons don't need to
// model the GET /v1/beads listing call.
func disableSquawkAutoComment(t *testing.T) {
	t.Helper()
	old := squawkNoComment
	squawkNoComment = true
	t.Cleanup(func() { squawkNoComment = old })
}

func TestRunSquawk_CreateError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runSquawk(cmd, []string{"msg"}); err == nil {
		t.Error("expected create error")
	}
}

func TestRunSquawk_CloseError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/beads" {
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-SQ1"})
			return
		}
		// close fails
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runSquawk(cmd, []string{"msg"}); err == nil {
		t.Error("expected close error")
	}
}

func TestRunSquawk_TruncatesLongTitle(t *testing.T) {
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_TRIGGER_BEAD_ID", "")
	disableSquawkAutoComment(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			title, _ := body["title"].(string)
			if len(title) != 500 {
				t.Errorf("expected title length 500, got %d", len(title))
			}
			if !strings.HasSuffix(title, "...") {
				t.Errorf("expected truncated suffix: %q", title[len(title)-5:])
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-SQ1"})
		case strings.HasSuffix(r.URL.Path, "/close"):
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	long := strings.Repeat("x", 600)
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runSquawk(cmd, []string{long}); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestRunSquawk_WritesTriggeredByEdges(t *testing.T) {
	t.Setenv("BOAT_AGENT_BEAD_ID", "gd-AGENT")
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_TRIGGER_BEAD_ID", "gd-TRIG")
	// Clear any inherited Slack thread coords so runSquawk's thread_id lookup
	// (ka-0000003279, gated on isThreadSpawnedAgent) does not fire against this
	// dependency-only mock; this test only exercises the triggered-by edges.
	t.Setenv("SLACK_THREAD_CHANNEL", "")
	t.Setenv("SLACK_THREAD_TS", "")
	disableSquawkAutoComment(t)

	var deps []map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads":
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-SQ1"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/dependencies"):
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			// Path looks like /v1/beads/gd-SQ1/dependencies — record the depends_on
			// + type for assertion.
			deps = append(deps, map[string]string{
				"depends_on_id": body["depends_on_id"],
				"type":          body["type"],
			})
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/close"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads/gd-AGENT":
			// The agent card lookup (agentCardPlace) is best-effort: no agent
			// bead leaves the squawk unstamped.
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runSquawk(cmd, []string{"linked squawk"}); err != nil {
		t.Errorf("err: %v", err)
	}

	if len(deps) != 2 {
		t.Fatalf("expected 2 triggered-by deps, got %d: %+v", len(deps), deps)
	}
	if deps[0]["depends_on_id"] != "gd-AGENT" || deps[0]["type"] != "triggered-by" {
		t.Errorf("expected first dep agent triggered-by, got %+v", deps[0])
	}
	if deps[1]["depends_on_id"] != "gd-TRIG" || deps[1]["type"] != "triggered-by" {
		t.Errorf("expected second dep trigger triggered-by, got %+v", deps[1])
	}
}

func TestRunSquawk_SkipsDuplicateTriggerEdge(t *testing.T) {
	// When trigger bead == agent bead, only one edge should be written.
	t.Setenv("BOAT_AGENT_BEAD_ID", "gd-SAME")
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_TRIGGER_BEAD_ID", "gd-SAME")
	disableSquawkAutoComment(t)

	var depCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads":
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-SQ1"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/dependencies"):
			depCalls++
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/close"):
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runSquawk(cmd, []string{"once"}); err != nil {
		t.Errorf("err: %v", err)
	}
	if depCalls != 1 {
		t.Errorf("expected exactly 1 dep call, got %d", depCalls)
	}
}

func TestRunSquawk_LineageBestEffort(t *testing.T) {
	// Dep API returns 500 — squawk should still succeed (lineage is best-effort).
	t.Setenv("BOAT_AGENT_BEAD_ID", "gd-AGENT")
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_TRIGGER_BEAD_ID", "")
	disableSquawkAutoComment(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads":
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-SQ1"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/dependencies"):
			w.WriteHeader(http.StatusInternalServerError)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/close"):
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runSquawk(cmd, []string{"resilient"}); err != nil {
		t.Errorf("squawk should not fail when lineage write fails: %v", err)
	}
}

// ── closeAgentBeadOnExit ─────────────────────────────────────────────────

func TestCloseAgentBeadOnExit_NoAgentID(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "")
	oldActor := actor
	actor = "unknown"
	t.Cleanup(func() { actor = oldActor })
	// No daemon needed — actor="unknown" short-circuits.
	closeAgentBeadOnExit("test")
}

func TestCloseAgentBeadOnExit_Success(t *testing.T) {
	var closed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// closeAgentBeadOnExit first GETs the bead (to decide whether to
		// synthesize a fallback wrap-up — ka-0000000101), then POSTs /close.
		if strings.HasSuffix(r.URL.Path, "/close") {
			closed = true
			w.WriteHeader(http.StatusOK)
			return
		}
		// Bead lookup: return a bead with no wrapup so the fallback path runs.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "gd-A1",
			"title":  "test agent",
			"fields": map[string]string{},
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("KD_AGENT_ID", "gd-A1")
	closeAgentBeadOnExit("test")
	if !closed {
		t.Error("expected the agent bead to be closed")
	}
}

func TestCloseAgentBeadOnExit_CloseFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("KD_AGENT_ID", "gd-A1")
	// Error is logged to stderr but function returns normally.
	closeAgentBeadOnExit("test")
}

// ── resolveTaskFromDeps (with daemon) ────────────────────────────────────

func TestResolveTaskFromDeps_NoAssignedDep(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"dependencies": []map[string]any{
				{"bead_id": "gd-A1", "depends_on_id": "gd-B1", "type": "blocks"},
			},
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("BOAT_AGENT_BEAD_ID", "gd-A1")
	if got := resolveTaskFromDeps(); got != "" {
		t.Errorf("expected empty: %q", got)
	}
}

func TestResolveTaskFromDeps_Assigned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"dependencies": []map[string]any{
				{"bead_id": "gd-A1", "depends_on_id": "gd-T1", "type": "assigned"},
			},
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("BOAT_AGENT_BEAD_ID", "gd-A1")
	if got := resolveTaskFromDeps(); got != "gd-T1" {
		t.Errorf("got %q", got)
	}
}

func TestResolveTaskFromDeps_DaemonError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("BOAT_AGENT_BEAD_ID", "gd-A1")
	if got := resolveTaskFromDeps(); got != "" {
		t.Errorf("expected empty on error: %q", got)
	}
}

// ── runCrew ──────────────────────────────────────────────────────────────

func TestRunCrew_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := &cobra.Command{}
	cmd.Flags().Bool("all", false, "")
	cmd.Flags().String("project", "", "")
	cmd.SetContext(context.Background())

	if err := runCrew(cmd, nil); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestRunCrew_EmptyShowAll(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := &cobra.Command{}
	cmd.Flags().Bool("all", true, "")
	cmd.Flags().String("project", "", "")
	_ = cmd.Flags().Set("all", "true")
	cmd.SetContext(context.Background())

	if err := runCrew(cmd, nil); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestRunCrew_ListError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := &cobra.Command{}
	cmd.Flags().Bool("all", false, "")
	cmd.Flags().String("project", "", "")
	cmd.SetContext(context.Background())

	if err := runCrew(cmd, nil); err == nil {
		t.Error("expected list error")
	}
}

func TestRunCrew_Populated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/beads/gd-T") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "gd-T1", "title": "task 1", "type": "task",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"beads": []map[string]any{
				{
					"id": "gd-A1", "title": "agent a", "type": "agent",
					"status": "open",
					"fields": json.RawMessage(`{"agent":"bot1","project":"p","role":"crew","spawned_by":"me","task_id":"gd-T1","created_at":"2020-01-01T00:00:00Z"}`),
				},
			},
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	oldActor := actor
	actor = "me"
	t.Cleanup(func() { actor = oldActor })

	cmd := &cobra.Command{}
	cmd.Flags().Bool("all", false, "")
	cmd.Flags().String("project", "", "")
	cmd.SetContext(context.Background())

	if err := runCrew(cmd, nil); err != nil {
		t.Errorf("err: %v", err)
	}
}

// ── resolveBaseBranch / storeWorkspaceInfo ───────────────────────────────

func TestResolveBaseBranch_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	if got := resolveBaseBranch(context.Background(), "gd-B1"); got != "origin/main" {
		t.Errorf("got %q", got)
	}
}

func TestResolveBaseBranch_NoDeps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"dependencies": []any{}})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	if got := resolveBaseBranch(context.Background(), "gd-B1"); got != "origin/main" {
		t.Errorf("got %q", got)
	}
}

func TestResolveBaseBranch_ParentHasWorkspace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/dependencies") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"dependencies": []map[string]any{
					{"bead_id": "gd-B1", "depends_on_id": "gd-P1", "type": "parent"},
				},
			})
			return
		}
		// GetBead(gd-P1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "gd-P1", "title": "parent", "type": "task",
			"fields": json.RawMessage(`{"workspace":"{\"branch\":\"feat/parent\",\"base_branch\":\"main\"}"}`),
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	if got := resolveBaseBranch(context.Background(), "gd-B1"); got != "feat/parent" {
		t.Errorf("got %q", got)
	}
}

func TestStoreWorkspaceInfo_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// UpdateBeadFields makes a GET then PATCH; just return OK on all.
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "gd-B1", "type": "task",
				"fields": json.RawMessage(`{}`),
			})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	if err := storeWorkspaceInfo(context.Background(), "gd-B1", WorkspaceInfo{}); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestStoreWorkspaceInfo_WithBranch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "gd-B1", "type": "task",
				"fields": json.RawMessage(`{}`),
			})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	info := WorkspaceInfo{Branch: "fix/foo", WorktreePath: "/tmp/x", BaseBranch: "main"}
	if err := storeWorkspaceInfo(context.Background(), "gd-B1", info); err != nil {
		t.Errorf("err: %v", err)
	}
}

// ── satisfyGateOnReady ───────────────────────────────────────────────────

func TestSatisfyGateOnReady_NoAgent(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "")
	oldActor := actor
	actor = "unknown"
	t.Cleanup(func() { actor = oldActor })
	// No daemon access expected — resolveAgentIDWithFallback fails early.
	satisfyGateOnReady(context.Background())
}

func TestSatisfyGateOnReady_OpenDecisionExists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"beads": []map[string]any{
				{
					"id": "gd-D1", "title": "dec", "type": "decision",
					"status": "open",
					"fields": json.RawMessage(`{"requesting_agent_bead_id":"gd-A1"}`),
				},
			},
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("KD_AGENT_ID", "gd-A1")
	// Should return early without calling SatisfyGate.
	satisfyGateOnReady(context.Background())
}

// ── registerSessionBead ──────────────────────────────────────────────────

func TestRegisterSessionBead_NoDaemon(t *testing.T) {
	oldD := daemon
	daemon = nil
	t.Cleanup(func() { daemon = oldD })
	got := registerSessionBead(context.Background(), k8sConfig{agent: "a"}, "")
	if got != "" {
		t.Errorf("got %q", got)
	}
}

// writeResumeLog creates a temp file with size >= minResumeLogBytes so it
// passes localResumeLogLoadable. Returns the path; cleaned up by t.TempDir.
func writeResumeLog(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/resume.log"
	body := bytes.Repeat([]byte("x"), minResumeLogBytes+16)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("writing resume log: %v", err)
	}
	return path
}

// stubS3Resume swaps s3ResumeSessionExistsFn for the duration of the test
// so we don't hit real S3.
func stubS3Resume(t *testing.T, exists bool) {
	t.Helper()
	prev := s3ResumeSessionExistsFn
	s3ResumeSessionExistsFn = func(context.Context, string) bool { return exists }
	t.Cleanup(func() { s3ResumeSessionExistsFn = prev })
}

func TestRegisterSessionBead_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/beads" {
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-S1"})
			return
		}
		w.WriteHeader(http.StatusOK) // dependency add ignored
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("KD_AGENT_ID", "gd-A1")
	got := registerSessionBead(context.Background(), k8sConfig{
		agent: "bot1", project: "p", role: "crew",
	}, writeResumeLog(t))
	if got != "gd-S1" {
		t.Errorf("got %q", got)
	}
}

func TestRegisterSessionBead_S3Resume(t *testing.T) {
	var captured beadsapi.SessionFields
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/beads" && r.Method == http.MethodPost {
			var req struct {
				Fields json.RawMessage `json:"fields"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			_ = json.Unmarshal(req.Fields, &captured)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-S2"})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("COOP_S3_RESUME_SESSION", "sess-abc-123")
	stubS3Resume(t, true) // S3 archive exists for this session
	got := registerSessionBead(context.Background(), k8sConfig{agent: "a"}, "")
	if got != "gd-S2" {
		t.Errorf("got %q", got)
	}
	if !captured.Resumed {
		t.Errorf("expected Resumed=true for S3 resume, got false")
	}
	if captured.SessionLog != "s3:sess-abc-123" {
		t.Errorf("got SessionLog %q, want s3:sess-abc-123", captured.SessionLog)
	}
}

// TestRegisterSessionBead_S3EnvSetButObjectMissing covers ka-0000005950:
// the env var carries an intent (last_session_id from the agent bead) but
// the S3 archive does not actually exist. resumed must be false.
func TestRegisterSessionBead_S3EnvSetButObjectMissing(t *testing.T) {
	var captured beadsapi.SessionFields
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/beads" && r.Method == http.MethodPost {
			var req struct {
				Fields json.RawMessage `json:"fields"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			_ = json.Unmarshal(req.Fields, &captured)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-S2b"})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("COOP_S3_RESUME_SESSION", "sess-stale-from-old-bead")
	stubS3Resume(t, false) // no objects under this prefix
	got := registerSessionBead(context.Background(), k8sConfig{agent: "a"}, "")
	if got != "gd-S2b" {
		t.Errorf("got %q", got)
	}
	if captured.Resumed {
		t.Errorf("expected Resumed=false when S3 archive does not exist, got true")
	}
	if captured.SessionLog != "" {
		t.Errorf("expected empty SessionLog when nothing was resumed, got %q", captured.SessionLog)
	}
}

// TestRegisterSessionBead_NoPriorState is the canonical ka-0000005950
// repro: no local transcript, no S3 env var — resumed must be false.
func TestRegisterSessionBead_NoPriorState(t *testing.T) {
	var captured beadsapi.SessionFields
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/beads" && r.Method == http.MethodPost {
			var req struct {
				Fields json.RawMessage `json:"fields"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			_ = json.Unmarshal(req.Fields, &captured)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-S2c"})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("COOP_S3_RESUME_SESSION", "")
	// Clear the production-wire env vars too so the new agent-bead-keyed
	// S3 branch (ka-0000002486) does not fire from ambient pod-shell state
	// when developers run the suite locally.
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("COOP_S3_BUCKET", "")
	got := registerSessionBead(context.Background(), k8sConfig{agent: "a"}, "")
	if got != "gd-S2c" {
		t.Errorf("got %q", got)
	}
	if captured.Resumed {
		t.Errorf("expected Resumed=false on fresh start, got true")
	}
}

// TestRegisterSessionBead_LocalLogMissingNotResumed ensures a non-empty
// resumeLog argument that points to a missing file does not yield
// resumed=true. Defends the truth invariant against caller bugs.
func TestRegisterSessionBead_LocalLogMissingNotResumed(t *testing.T) {
	var captured beadsapi.SessionFields
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/beads" && r.Method == http.MethodPost {
			var req struct {
				Fields json.RawMessage `json:"fields"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			_ = json.Unmarshal(req.Fields, &captured)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-S2d"})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("COOP_S3_RESUME_SESSION", "") // isolate from ambient pod env
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("COOP_S3_BUCKET", "")
	got := registerSessionBead(context.Background(), k8sConfig{agent: "a"}, "/tmp/does-not-exist-"+t.Name())
	if got != "gd-S2d" {
		t.Errorf("got %q", got)
	}
	if captured.Resumed {
		t.Errorf("expected Resumed=false when resume log path is missing, got true")
	}
}

// TestRegisterSessionBead_S3AgentKeyResume covers ka-0000002486: the
// production wire is COOP_S3_SESSION_KEY=<agent-bead-id> (set on the coop
// subprocess by init_agent_loop.go, not on gb itself). gb falls back to
// agentS3SessionKey() — i.e. KD_AGENT_ID / BOAT_AGENT_BEAD_ID — and
// probes S3 under that prefix.
func TestRegisterSessionBead_S3AgentKeyResume(t *testing.T) {
	var (
		captured beadsapi.SessionFields
		probed   string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/beads" && r.Method == http.MethodPost {
			var req struct {
				Fields json.RawMessage `json:"fields"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			_ = json.Unmarshal(req.Fields, &captured)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-S4"})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("COOP_S3_RESUME_SESSION", "") // production never sets this
	t.Setenv("KD_AGENT_ID", "gd-Agent42")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("COOP_S3_BUCKET", "some-bucket")

	prev := s3ResumeSessionExistsFn
	s3ResumeSessionExistsFn = func(_ context.Context, key string) bool {
		probed = key
		return true // archive exists for this agent's bead-keyed prefix
	}
	t.Cleanup(func() { s3ResumeSessionExistsFn = prev })

	got := registerSessionBead(context.Background(), k8sConfig{agent: "a"}, "")
	if got != "gd-S4" {
		t.Errorf("got %q", got)
	}
	if probed != "gd-Agent42" {
		t.Errorf("probed S3 key = %q, want gd-Agent42", probed)
	}
	if !captured.Resumed {
		t.Errorf("expected Resumed=true when agent-keyed S3 archive exists, got false")
	}
	if captured.SessionLog != "s3:gd-Agent42" {
		t.Errorf("got SessionLog %q, want s3:gd-Agent42", captured.SessionLog)
	}
}

// TestRegisterSessionBead_S3AgentKeyMissingArchive ensures that the
// agent-keyed branch obeys ground-truth: COOP_S3_BUCKET + KD_AGENT_ID are
// set but no archive exists yet (fresh agent's first pod). resumed must
// remain false rather than claiming intent.
func TestRegisterSessionBead_S3AgentKeyMissingArchive(t *testing.T) {
	var captured beadsapi.SessionFields
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/beads" && r.Method == http.MethodPost {
			var req struct {
				Fields json.RawMessage `json:"fields"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			_ = json.Unmarshal(req.Fields, &captured)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-S5"})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("COOP_S3_RESUME_SESSION", "")
	t.Setenv("KD_AGENT_ID", "gd-H9NY4rpmxb")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("COOP_S3_BUCKET", "some-bucket")
	stubS3Resume(t, false) // no objects under prefix yet

	got := registerSessionBead(context.Background(), k8sConfig{agent: "a"}, "")
	if got != "gd-S5" {
		t.Errorf("got %q", got)
	}
	if captured.Resumed {
		t.Errorf("expected Resumed=false for fresh agent with no S3 archive, got true")
	}
	if captured.SessionLog != "" {
		t.Errorf("got SessionLog %q, want empty", captured.SessionLog)
	}
}

// TestRegisterSessionBead_S3AgentKeyNoBucket ensures the new branch does
// not fire when COOP_S3_BUCKET is unset (local-dev / unconfigured envs).
// Without this guard, dev-machine runs would hit real S3 with stale creds.
func TestRegisterSessionBead_S3AgentKeyNoBucket(t *testing.T) {
	var probeCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/beads" && r.Method == http.MethodPost {
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-S6"})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("COOP_S3_RESUME_SESSION", "")
	t.Setenv("KD_AGENT_ID", "gd-Agent99")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("COOP_S3_BUCKET", "")

	prev := s3ResumeSessionExistsFn
	s3ResumeSessionExistsFn = func(context.Context, string) bool {
		probeCalls++
		return true
	}
	t.Cleanup(func() { s3ResumeSessionExistsFn = prev })

	_ = registerSessionBead(context.Background(), k8sConfig{agent: "a"}, "")
	if probeCalls != 0 {
		t.Errorf("expected S3 probe to be skipped when COOP_S3_BUCKET is empty, got %d calls", probeCalls)
	}
}

func TestRegisterSessionBead_LocalResumeWinsOverS3Env(t *testing.T) {
	var captured beadsapi.SessionFields
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/beads" && r.Method == http.MethodPost {
			var req struct {
				Fields json.RawMessage `json:"fields"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			_ = json.Unmarshal(req.Fields, &captured)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-S3"})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("COOP_S3_RESUME_SESSION", "sess-should-be-ignored")
	stubS3Resume(t, true) // even if S3 had data, local should win
	localPath := writeResumeLog(t)
	got := registerSessionBead(context.Background(), k8sConfig{agent: "a"}, localPath)
	if got != "gd-S3" {
		t.Errorf("got %q", got)
	}
	if !captured.Resumed {
		t.Errorf("expected Resumed=true")
	}
	if captured.SessionLog != localPath {
		t.Errorf("got SessionLog %q, want %q (local resume wins)", captured.SessionLog, localPath)
	}
}

func TestRegisterSessionBead_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	got := registerSessionBead(context.Background(), k8sConfig{agent: "a"}, "")
	if got != "" {
		t.Errorf("got %q", got)
	}
}

// ── closeSessionBead no-ops ──────────────────────────────────────────────

func TestCloseSessionBead_NilDaemon(t *testing.T) {
	oldD := daemon
	daemon = nil
	t.Cleanup(func() { daemon = oldD })
	closeSessionBead(context.Background(), "", 0, "", false)
	closeSessionBead(context.Background(), "gd-S1", 0, "", false)
}

func TestCloseSessionBead_EmptyID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	closeSessionBead(context.Background(), "", 0, "", false)
}

// ── checkWrapUpGate: blocked path ────────────────────────────────────────

func TestCheckWrapUpGate_Blocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// GetBead(gd-A1): stop_requested=true, no wrapup
		if r.URL.Path == "/v1/beads/gd-A1" && r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "gd-A1", "title": "t", "type": "agent",
				"fields": json.RawMessage(`{"stop_requested":"true"}`),
			})
			return
		}
		// wrapup-config bead with hard enforce
		if r.URL.Path == "/v1/beads" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"beads": []map[string]any{
					{
						"id": "gd-W1", "title": WrapUpConfigCategory,
						"type": "config", "kind": "config", "status": "open",
						"labels":      []string{"global"},
						"description": `{"enabled":true,"enforce":"hard","required":["accomplishments"]}`,
					},
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	blocked, reason := checkWrapUpGate(context.Background(), "gd-A1")
	if !blocked {
		t.Error("expected blocked")
	}
	if !strings.Contains(reason, "accomplishments") {
		t.Errorf("reason: %q", reason)
	}
}

// ── runPrime / runPrimeTo variants ───────────────────────────────────────

func TestRunPrime_Prewarmed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("BOAT_AGENT_STATE", "prewarmed")
	t.Setenv("KD_AGENT_ID", "gd-A1")
	// Unassigned: sentinel absent (point it at a guaranteed-empty temp dir so a
	// stray real /tmp/standby_assigned can't flake this).
	origSentinel := standbyAssignedSentinelPath
	standbyAssignedSentinelPath = filepath.Join(t.TempDir(), "standby_assigned")
	defer func() { standbyAssignedSentinelPath = origSentinel }()
	var out, errBuf bytes.Buffer
	if err := runPrimeTo(&out, &errBuf); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(out.String(), "Prewarmed Agent") {
		t.Errorf("out: %q", out.String())
	}
}

// TestRunPrime_PrewarmedAssigned verifies that once a prewarmed pod has been
// assigned work (standbyAssignedSentinelPath present), gb prime no longer emits
// the lightweight standby card — it falls through to full workflow context so
// the assigned agent knows how to do and wrap up its work (ka-0000004623).
func TestRunPrime_PrewarmedAssigned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("BOAT_AGENT_STATE", "prewarmed")
	t.Setenv("KD_AGENT_ID", "gd-A1")
	origSentinel := standbyAssignedSentinelPath
	standbyAssignedSentinelPath = filepath.Join(t.TempDir(), "standby_assigned")
	defer func() { standbyAssignedSentinelPath = origSentinel }()
	if err := os.WriteFile(standbyAssignedSentinelPath, []byte("1"), 0644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	var out, errBuf bytes.Buffer
	if err := runPrimeTo(&out, &errBuf); err != nil {
		t.Fatalf("err: %v", err)
	}
	if strings.Contains(out.String(), "Prewarmed Agent") {
		t.Errorf("assigned prewarmed pod should not get the standby card, got: %q", out.String())
	}
}

func TestRunPrime_SystemPromptFlag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	old := primeSystemPrompt
	primeSystemPrompt = true
	t.Cleanup(func() { primeSystemPrompt = old })

	t.Setenv("BOAT_AGENT_STATE", "")
	t.Setenv("KD_AGENT_ID", "")
	var out, errBuf bytes.Buffer
	if err := runPrimeTo(&out, &errBuf); err != nil {
		t.Fatalf("err: %v", err)
	}
	// buildSystemPromptBody renders identity/workflow/advice/wrap-up
	if out.Len() == 0 {
		t.Error("expected output")
	}
}

func TestRunPrime_SizeOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	old := primeSizeOnly
	primeSizeOnly = true
	t.Cleanup(func() { primeSizeOnly = old })

	t.Setenv("BOAT_AGENT_STATE", "")
	var out, errBuf bytes.Buffer
	if err := runPrimeTo(&out, &errBuf); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(out.String(), "total_bytes=") {
		t.Errorf("expected size report: %q", out.String())
	}
}

func TestRunPrime_Normal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("BOAT_AGENT_STATE", "")
	t.Setenv("KD_AGENT_ID", "")
	var out, errBuf bytes.Buffer
	if err := runPrimeTo(&out, &errBuf); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(errBuf.String(), "total_bytes=") {
		t.Errorf("expected size report on stderr: %q", errBuf.String())
	}
}

func TestRunPrime_CommandDelegates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runPrime(cmd, nil); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestSatisfyGateOnReady_Satisfied(t *testing.T) {
	var satisfied bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/satisfy"):
			satisfied = true
			w.WriteHeader(http.StatusOK)
		case strings.Contains(r.URL.Path, "/v1/beads/gd-A1") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "gd-A1", "type": "agent",
				"fields": json.RawMessage(`{}`),
			})
		case r.URL.Path == "/v1/beads" || r.URL.Path == "/v1/beads/list":
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("KD_AGENT_ID", "gd-A1")
	satisfyGateOnReady(context.Background())
	if !satisfied {
		t.Error("expected SatisfyGate call")
	}
}
