package beadsapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListGates_ParsesResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/v1/agents/gd-agent1/gates" {
			t.Errorf("expected path /v1/agents/gd-agent1/gates, got %s", r.URL.Path)
		}
		gates := []GateRow{
			{AgentBeadID: "gd-agent1", GateID: "decision", Status: "pending"},
			{AgentBeadID: "gd-agent1", GateID: "commit-push", Status: "satisfied"},
		}
		_ = json.NewEncoder(w).Encode(gates)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	gates, err := c.ListGates(context.Background(), "gd-agent1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gates) != 2 {
		t.Fatalf("expected 2 gates, got %d", len(gates))
	}
	if gates[0].GateID != "decision" || gates[0].Status != "pending" {
		t.Errorf("unexpected first gate: %+v", gates[0])
	}
	if gates[1].GateID != "commit-push" || gates[1].Status != "satisfied" {
		t.Errorf("unexpected second gate: %+v", gates[1])
	}
}

func TestListGates_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal error"}`))
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	_, err := c.ListGates(context.Background(), "gd-bad")
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestSatisfyGate_SendsCorrectRequest(t *testing.T) {
	var gotMethod, gotPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	err := c.SatisfyGate(context.Background(), "gd-agent1", "decision")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("expected POST, got %s", gotMethod)
	}
	if gotPath != "/v1/agents/gd-agent1/gates/decision/satisfy" {
		t.Errorf("expected path /v1/agents/gd-agent1/gates/decision/satisfy, got %s", gotPath)
	}
}

func TestClearGate_SendsCorrectRequest(t *testing.T) {
	var gotMethod, gotPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	err := c.ClearGate(context.Background(), "gd-agent1", "commit-push")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("expected DELETE, got %s", gotMethod)
	}
	if gotPath != "/v1/agents/gd-agent1/gates/commit-push" {
		t.Errorf("expected path /v1/agents/gd-agent1/gates/commit-push, got %s", gotPath)
	}
}
