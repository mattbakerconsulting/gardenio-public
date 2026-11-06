package beadsapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEmitHook_SendsCorrectRequest(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody EmitHookRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		resp := EmitHookResponse{
			Block:    false,
			Warnings: []string{"gate: decision pending"},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	resp, err := c.EmitHook(context.Background(), EmitHookRequest{
		AgentBeadID: "gd-agent1",
		HookType:    "Stop",
		ToolName:    "Bash",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("expected POST, got %s", gotMethod)
	}
	if gotPath != "/v1/hooks/emit" {
		t.Errorf("expected path /v1/hooks/emit, got %s", gotPath)
	}
	if gotBody.AgentBeadID != "gd-agent1" {
		t.Errorf("expected agent_bead_id=gd-agent1, got %s", gotBody.AgentBeadID)
	}
	if gotBody.HookType != "Stop" {
		t.Errorf("expected hook_type=Stop, got %s", gotBody.HookType)
	}

	if resp.Block {
		t.Error("expected block=false")
	}
	if len(resp.Warnings) != 1 || resp.Warnings[0] != "gate: decision pending" {
		t.Errorf("unexpected warnings: %v", resp.Warnings)
	}
}

func TestEmitHook_BlockingResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := EmitHookResponse{
			Block:  true,
			Reason: "decision gate not satisfied",
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	resp, err := c.EmitHook(context.Background(), EmitHookRequest{
		AgentBeadID: "gd-agent2",
		HookType:    "PreToolUse",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Block {
		t.Error("expected block=true")
	}
	if resp.Reason != "decision gate not satisfied" {
		t.Errorf("expected reason, got %s", resp.Reason)
	}
}

func TestEmitHook_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"gate evaluation failed"}`))
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	_, err := c.EmitHook(context.Background(), EmitHookRequest{
		AgentBeadID: "gd-agent3",
		HookType:    "Stop",
	})
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}
