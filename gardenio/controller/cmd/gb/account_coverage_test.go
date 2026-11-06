package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

// ── runAccountStatus ──────────────────────────────────────────────────────

func TestRunAccountStatus_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runAccountStatus(cmd, nil); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestRunAccountStatus_Populated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"beads": []map[string]any{
				{
					"id": "gd-A1", "title": "acct-1", "type": "account",
					"status": "open", "labels": []string{"project:gardenio"},
					"fields": json.RawMessage(`{"provider":"anthropic","model":"opus","fallback":"acct-2"}`),
				},
				{
					"id": "gd-A2", "title": "acct-global", "type": "account",
					"status": "open", "labels": []string{},
					"fields": json.RawMessage(`{"provider":"openai"}`),
				},
			},
			"total": 2,
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runAccountStatus(cmd, nil); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestRunAccountStatus_JSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"beads": []map[string]any{
				{
					"id": "gd-A1", "title": "acct-1", "type": "account",
					"status": "open", "labels": []string{"project:p"},
					"fields": json.RawMessage(`{"provider":"anthropic"}`),
				},
			},
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	oldJSON := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = oldJSON })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runAccountStatus(cmd, nil); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestRunAccountStatus_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runAccountStatus(cmd, nil); err == nil {
		t.Error("expected error")
	}
}

// ── runAccountRotate ──────────────────────────────────────────────────────

func TestRunAccountRotate_EmptyPool(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runAccountRotate(cmd, []string{"missing"}); err == nil {
		t.Error("expected error for empty pool")
	}
}

func TestRunAccountRotate_NoFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"beads": []map[string]any{
				{
					"id": "gd-A1", "title": "acct-only", "type": "account",
					"status": "open", "labels": []string{"project:p"},
					"fields": json.RawMessage(`{"provider":"a"}`),
				},
			},
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runAccountRotate(cmd, []string{"p"}); err == nil {
		t.Error("expected error for no fallback")
	}
}

func TestRunAccountRotate_FallbackMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"beads": []map[string]any{
				{
					"id": "gd-A1", "title": "acct-1", "type": "account",
					"status": "open", "labels": []string{"project:p"},
					"fields": json.RawMessage(`{"provider":"a","fallback":"does-not-exist"}`),
				},
			},
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runAccountRotate(cmd, []string{"p"}); err == nil {
		t.Error("expected error for missing fallback bead")
	}
}

func TestRunAccountRotate_OK(t *testing.T) {
	var patchCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			// Read-modify-write: return current bead
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "gd-A1", "title": "acct-1", "type": "account", "status": "open",
				"labels": []string{"project:p"},
				"fields": json.RawMessage(`{"provider":"a","fallback":"acct-2","rotation_count":"0"}`),
			})
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			atomic.AddInt32(&patchCount, 1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			// list beads
			_ = json.NewEncoder(w).Encode(map[string]any{
				"beads": []map[string]any{
					{
						"id": "gd-A1", "title": "acct-1", "type": "account",
						"status": "open", "labels": []string{"project:p"},
						"fields": json.RawMessage(`{"provider":"a","fallback":"acct-2"}`),
					},
					{
						"id": "gd-A2", "title": "acct-2", "type": "account",
						"status": "open", "labels": []string{"project:p"},
						"fields": json.RawMessage(`{"provider":"a"}`),
					},
				},
			})
		}
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runAccountRotate(cmd, []string{"p"}); err != nil {
		t.Errorf("err: %v", err)
	}
}

// ── runAccountReset ───────────────────────────────────────────────────────

func TestRunAccountReset_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runAccountReset(cmd, []string{"missing"}); err == nil {
		t.Error("expected error for missing account")
	}
}

func TestRunAccountReset_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runAccountReset(cmd, []string{"x"}); err == nil {
		t.Error("expected error")
	}
}

func TestRunAccountReset_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/v1/beads/gd-A1"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"beads": []map[string]any{
					{
						"id": "gd-A1", "title": "acct-1", "type": "account",
						"status": "open", "labels": []string{},
						"fields": json.RawMessage(`{"provider":"a"}`),
					},
				},
			})
		}
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	// Clear LiteLLM env so snapshotLiteLLMSpend is a no-op.
	t.Setenv("LITELLM_URL", "")
	t.Setenv("LITELLM_MASTER_KEY", "")

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runAccountReset(cmd, []string{"acct-1"}); err != nil {
		t.Errorf("err: %v", err)
	}
}

// ── findBeadByTitle ───────────────────────────────────────────────────────

func TestFindBeadByTitle(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		{ID: "a", Title: "foo"},
		{ID: "b", Title: "bar"},
	}
	if got := findBeadByTitle(beads, "bar"); got == nil || got.ID != "b" {
		t.Errorf("expected b, got %+v", got)
	}
	if got := findBeadByTitle(beads, "none"); got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

// ── snapshotLiteLLMSpend ──────────────────────────────────────────────────

func TestSnapshotLiteLLMSpend_Unconfigured(t *testing.T) {
	t.Setenv("LITELLM_URL", "")
	t.Setenv("LITELLM_MASTER_KEY", "")
	bead := &beadsapi.BeadDetail{Fields: map[string]string{"litellm_key": "sk-1"}}
	if got := snapshotLiteLLMSpend(context.Background(), bead); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestSnapshotLiteLLMSpend_NoVirtualKey(t *testing.T) {
	t.Setenv("LITELLM_URL", "http://x")
	t.Setenv("LITELLM_MASTER_KEY", "k")
	bead := &beadsapi.BeadDetail{Fields: map[string]string{}}
	if got := snapshotLiteLLMSpend(context.Background(), bead); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestSnapshotLiteLLMSpend_QueryFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv("LITELLM_URL", srv.URL)
	t.Setenv("LITELLM_MASTER_KEY", "k")
	bead := &beadsapi.BeadDetail{Fields: map[string]string{"litellm_key": "sk-1"}}
	if got := snapshotLiteLLMSpend(context.Background(), bead); got != "" {
		t.Errorf("expected empty on query fail, got %q", got)
	}
}
