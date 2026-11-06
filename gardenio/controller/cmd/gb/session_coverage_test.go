package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// ── runSessionList ────────────────────────────────────────────────────────

func TestRunSessionList_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/beads") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)
	t.Setenv("BOAT_AGENT", "agent-1")

	oldAll, oldLimit, oldAgent := sessionAll, sessionLimit, sessionAgent
	sessionAll, sessionLimit, sessionAgent = false, 10, ""
	t.Cleanup(func() { sessionAll, sessionLimit, sessionAgent = oldAll, oldLimit, oldAgent })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runSessionList(cmd, nil); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestRunSessionList_Populated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{
			"beads": []map[string]any{
				{
					"id":     "gd-S1",
					"title":  "session:agent-1:now",
					"type":   "session",
					"status": "open",
					"fields": json.RawMessage(`{"agent":"agent-1","status":"active","started_at":"` +
						time.Now().Add(-5*time.Minute).UTC().Format(time.RFC3339) + `"}`),
				},
				{
					"id":     "gd-S2",
					"title":  "session:agent-2:earlier",
					"type":   "session",
					"status": "closed",
					"fields": json.RawMessage(`{"agent":"agent-2","status":"completed","started_at":"2026-01-01T00:00:00Z","ended_at":"2026-01-01T00:05:00Z","exit_code":"0","total_cost_usd":"1.25"}`),
				},
			},
			"total": 2,
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	oldAll, oldLimit, oldAgent := sessionAll, sessionLimit, sessionAgent
	sessionAll, sessionLimit, sessionAgent = true, 20, ""
	t.Cleanup(func() { sessionAll, sessionLimit, sessionAgent = oldAll, oldLimit, oldAgent })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runSessionList(cmd, nil); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestRunSessionList_JSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	oldJSON, oldAll := jsonOutput, sessionAll
	jsonOutput, sessionAll = true, true
	t.Cleanup(func() { jsonOutput, sessionAll = oldJSON, oldAll })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runSessionList(cmd, nil); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestRunSessionList_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	oldAll := sessionAll
	sessionAll = true
	t.Cleanup(func() { sessionAll = oldAll })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runSessionList(cmd, nil); err == nil {
		t.Error("expected error")
	}
}

// ── runSessionShow ────────────────────────────────────────────────────────

func TestRunSessionShow_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/v1/beads/") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "gd-S1",
			"title":  "session:agent:now",
			"type":   "session",
			"status": "open",
			"fields": json.RawMessage(`{"agent":"agent","status":"active","started_at":"2026-01-01T00:00:00Z","ended_at":"2026-01-01T00:05:00Z","input_tokens":"100","output_tokens":"50","total_cost_usd":"0.01"}`),
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runSessionShow(cmd, []string{"gd-S1"}); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestRunSessionShow_JSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "gd-S1",
			"title":  "session",
			"type":   "session",
			"fields": json.RawMessage(`{"agent":"a"}`),
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	oldJSON := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = oldJSON })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runSessionShow(cmd, []string{"gd-S1"}); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestRunSessionShow_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runSessionShow(cmd, []string{"gd-NOPE"}); err == nil {
		t.Error("expected error")
	}
}

// ── runSessionRegister ────────────────────────────────────────────────────

func TestRunSessionRegister_MissingAgent(t *testing.T) {
	t.Setenv("BOAT_AGENT", "")
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runSessionRegister(cmd, nil); err == nil {
		t.Error("expected error for missing BOAT_AGENT")
	}
}

func TestRunSessionRegister_OK(t *testing.T) {
	var createHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// CreateBead POST + maybe AddDependency POST.
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v1/beads") {
			createHits++
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "gd-NEW"})
			return
		}
		// AddDependency or other: 200 OK
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("BOAT_AGENT", "agent-x")
	t.Setenv("KD_AGENT_ID", "gd-A1")
	t.Setenv("BOAT_ROLE", "crew")

	oldLog := sessionLog
	sessionLog = ""
	t.Cleanup(func() { sessionLog = oldLog })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runSessionRegister(cmd, nil); err != nil {
		t.Errorf("err: %v", err)
	}
	if createHits == 0 {
		t.Error("expected at least one CreateBead call")
	}
}

// ── runSessionReconcile ──────────────────────────────────────────────────

func TestRunSessionReconcile_InvalidMaxAge(t *testing.T) {
	oldMaxAge := reconcileMaxAge
	reconcileMaxAge = "not-a-duration"
	t.Cleanup(func() { reconcileMaxAge = oldMaxAge })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runSessionReconcile(cmd, nil); err == nil {
		t.Error("expected error for invalid --max-age")
	}
}

func TestRunSessionReconcile_None(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	oldMaxAge, oldAgent, oldDry := reconcileMaxAge, sessionAgent, reconcileDryRun
	reconcileMaxAge = "1h"
	sessionAgent = ""
	reconcileDryRun = false
	t.Cleanup(func() {
		reconcileMaxAge, sessionAgent, reconcileDryRun = oldMaxAge, oldAgent, oldDry
	})

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runSessionReconcile(cmd, nil); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestRunSessionReconcile_DryRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// ListOrphanedSessions queries /v1/beads?type=session&status=...
		body := map[string]any{
			"beads": []map[string]any{
				{
					"id":     "gd-S1",
					"title":  "session",
					"type":   "session",
					"status": "open",
					"fields": json.RawMessage(`{"agent":"a","status":"active","started_at":"2020-01-01T00:00:00Z"}`),
				},
			},
			"total": 1,
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	oldMaxAge, oldAgent, oldDry := reconcileMaxAge, sessionAgent, reconcileDryRun
	reconcileMaxAge = "1h"
	sessionAgent = ""
	reconcileDryRun = true
	t.Cleanup(func() {
		reconcileMaxAge, sessionAgent, reconcileDryRun = oldMaxAge, oldAgent, oldDry
	})

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runSessionReconcile(cmd, nil); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestRunSessionReconcile_DryRunJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{
			"beads": []map[string]any{
				{
					"id":     "gd-S1",
					"title":  "session",
					"type":   "session",
					"status": "open",
					"fields": json.RawMessage(`{"agent":"a","status":"active","started_at":"2020-01-01T00:00:00Z"}`),
				},
			},
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	oldMaxAge, oldAgent, oldDry, oldJSON := reconcileMaxAge, sessionAgent, reconcileDryRun, jsonOutput
	reconcileMaxAge = "1h"
	sessionAgent = ""
	reconcileDryRun = true
	jsonOutput = true
	t.Cleanup(func() {
		reconcileMaxAge, sessionAgent, reconcileDryRun, jsonOutput = oldMaxAge, oldAgent, oldDry, oldJSON
	})

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runSessionReconcile(cmd, nil); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestRunSessionReconcile_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	oldMaxAge, oldAgent, oldDry := reconcileMaxAge, sessionAgent, reconcileDryRun
	reconcileMaxAge = "1h"
	sessionAgent = ""
	reconcileDryRun = false
	t.Cleanup(func() {
		reconcileMaxAge, sessionAgent, reconcileDryRun = oldMaxAge, oldAgent, oldDry
	})

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runSessionReconcile(cmd, nil); err == nil {
		t.Error("expected error")
	}
}

// ── updateAccountUsage / reconcileOrphanedSessions edge cases ─────────────

func TestUpdateAccountUsage_NoDaemon(t *testing.T) {
	old := daemon
	daemon = nil
	t.Cleanup(func() { daemon = old })
	updateAccountUsage(context.Background(), "proj", nil) // no-op
}

func TestReconcileOrphanedSessions_NoDaemon(t *testing.T) {
	old := daemon
	daemon = nil
	t.Cleanup(func() { daemon = old })
	reconcileOrphanedSessions(context.Background(), "agent-x") // no-op
	reconcileOrphanedSessions(context.Background(), "")        // no-op
}

// ── news.go runNews ───────────────────────────────────────────────────────

func newNewsCmd() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Flags().Bool("all", false, "")
	cmd.Flags().String("window", "2h", "")
	cmd.Flags().IntP("limit", "n", 50, "")
	cmd.Flags().String("project", "", "")
	cmd.Flags().Bool("all-projects", false, "")
	cmd.SetContext(context.Background())
	return cmd
}

func TestRunNews_InvalidWindow(t *testing.T) {
	cmd := newNewsCmd()
	cmd.Flags().Set("window", "not-a-duration")
	if err := runNews(cmd, nil); err == nil {
		t.Error("expected error")
	}
}

func TestRunNews_NoActivity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := newNewsCmd()
	cmd.Flags().Set("all-projects", "true")
	if err := runNews(cmd, nil); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestRunNews_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := newNewsCmd()
	cmd.Flags().Set("all-projects", "true")
	if err := runNews(cmd, nil); err == nil {
		t.Error("expected error")
	}
}

func TestRunNews_WithContent(t *testing.T) {
	recent := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339Nano)
	// gb news now fans its section fetches out CONCURRENTLY (ka-0000006233), so
	// the mock daemon must route by REQUEST CONTENT, not call order — a shared
	// call counter both races under -race and no longer maps to a fixed
	// sequence. Each section query carries distinct type/status/kind params.
	beadEnvelope := func(w http.ResponseWriter, beads ...map[string]any) {
		if beads == nil {
			beads = []map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": beads})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		typ, status := q.Get("type"), q.Get("status")
		switch {
		case typ == "" && status == "in_progress":
			beadEnvelope(w, map[string]any{"id": "gd-I1", "title": "wip", "type": "task", "kind": "issue",
				"status": "in_progress", "assignee": "other-agent", "labels": []string{"project:x"},
				"updated_at": recent, "fields": json.RawMessage(`{}`)})
		case typ == "" && status == "closed":
			beadEnvelope(w, map[string]any{"id": "gd-C1", "title": "done", "type": "task", "kind": "issue",
				"status": "closed", "labels": []string{"project:x"},
				"updated_at": recent, "fields": json.RawMessage(`{}`)})
		case typ == "decision" && status == "closed":
			beadEnvelope(w, map[string]any{"id": "gd-D1", "title": "d", "type": "decision", "status": "closed",
				"labels": []string{"project:x"}, "updated_at": recent,
				"fields": json.RawMessage(`{"chosen":"a"}`)})
		case typ == "report" && status == "closed":
			beadEnvelope(w, map[string]any{"id": "gd-R1", "title": "rep", "type": "report", "status": "closed",
				"labels": []string{"project:x"}, "updated_at": recent,
				"fields": json.RawMessage(`{}`)})
		case typ == "agent":
			beadEnvelope(w, map[string]any{"id": "gd-A1", "title": "other-agent", "type": "agent", "status": "open",
				"fields": json.RawMessage(`{}`)})
		default:
			// Open decisions/escalations, relevance sub-queries, and any other
			// best-effort fetch: an empty envelope keeps every axis valid.
			beadEnvelope(w)
		}
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	oldActor := actor
	actor = "me"
	t.Cleanup(func() { actor = oldActor })

	cmd := newNewsCmd()
	cmd.Flags().Set("all-projects", "true")
	cmd.Flags().Set("all", "true")
	if err := runNews(cmd, nil); err != nil {
		t.Errorf("err: %v", err)
	}
}
